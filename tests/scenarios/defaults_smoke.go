// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/haproxy-haptic/HAPTIC/tests/httpclient"
	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
	"github.com/haproxy-haptic/HAPTIC/tests/tunnel"
)

var applyFailure = regexp.MustCompile(`level=ERROR.*(Failed to apply rendered resource|Failed to resolve GVR for rendered resource)`)

func (s *Session) defaultsSmoke(ctx context.Context) (result error) {
	if err := poll(ctx, 30*time.Second, time.Second, "controller-rendered Service", func(ctx context.Context) (testutil.PollResult, error) {
		_, err := s.Kube(ctx, nil, "get", "service", s.Release+"-haproxy")
		if err != nil {
			return testutil.PollPending, err
		}
		return testutil.PollSucceeded, nil
	}); err != nil {
		return &PhaseError{Code: 6, Phase: "HAProxy Service", Err: err}
	}
	startup, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	forward, err := tunnel.Start(ctx, ctx, s.Client, "svc/"+s.Release+"-haproxy", []tunnel.Port{{Remote: "80"}, {Remote: "443"}, {Remote: "8404"}}, 15*time.Second)
	if err != nil {
		return &PhaseError{Code: 6, Phase: "port-forward", Err: err}
	}
	defer func() { result = errors.Join(result, forward.Process.Stop()) }()
	client := httpclient.New(&httpclient.Config{Host: loopbackHost, HTTPPort: forward.Locals[2], HTTPSPort: forward.Locals[1]})
	defer client.CloseIdleConnections()
	if err := poll(startup, 15*time.Second, time.Second, "HAProxy stats connection", func(ctx context.Context) (testutil.PollResult, error) {
		_, err := client.GET("localhost", "/healthz").Do(ctx)
		if err != nil {
			return testutil.PollPending, err
		}
		return testutil.PollSucceeded, nil
	}); err != nil {
		return &PhaseError{Code: 6, Phase: "port-forward readiness", Err: err}
	}
	for _, probe := range []struct {
		path string
		code int
	}{{"/healthz", 6}, {"/metrics", 7}} {
		if err := checkDefaultEndpoint(ctx, client, probe.path); err != nil {
			return &PhaseError{Code: probe.code, Phase: probe.path, Err: err}
		}
	}
	if err := s.defaultsApplied(ctx); err != nil {
		return &PhaseError{Code: 11, Phase: "rendered resource application", Err: err}
	}
	s.Infof("HTTPS traffic is covered by route scenarios; chart defaults have no HTTPS frontend")
	return nil
}

func checkDefaultEndpoint(ctx context.Context, client *httpclient.Client, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	response, err := client.GET("localhost", path).Do(ctx)
	if err != nil {
		return err
	}
	if response.Status != 200 {
		return fmt.Errorf("%s returned HTTP %d", path, response.Status)
	}
	return nil
}

func (s *Session) defaultsApplied(ctx context.Context) error {
	logs, err := s.Kube(ctx, nil, "logs", "-l", "app.kubernetes.io/component=controller", "-c", "controller", "--tail=-1")
	if err != nil {
		return err
	}
	if strings.TrimSpace(logs.Stdout) == "" {
		return errors.New("controller logs are empty; resource application cannot be verified")
	}
	if applyFailure.MatchString(logs.Stdout) {
		return errors.New("controller failed to apply a rendered resource; inspect controller logs and chart RBAC")
	}
	return nil
}
