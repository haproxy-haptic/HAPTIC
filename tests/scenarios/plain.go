// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"
)

func (s *Session) InstallWithoutGatewayAPI(ctx context.Context) error {
	if _, err := s.Kube(ctx, nil, "wait", "--for=condition=Ready", "node", "--all", "--timeout=180s"); err != nil {
		return err
	}
	premise, err := s.Kube(ctx, nil, "get", "crd", "gatewayclasses.gateway.networking.k8s.io", "--ignore-not-found", "-o", fieldName)
	if err != nil {
		return err
	}
	if strings.TrimSpace(premise.Stdout) != "" {
		return errors.New("the Gateway API is installed; this scenario requires its CRDs to be absent")
	}
	if err := s.LoadControllerImage(ctx); err != nil {
		return err
	}
	if err := s.LoadSPOAImage(ctx); err != nil {
		return err
	}
	s.Infof("Installing chart defaults without Gateway API")
	args := []string{helmInstall, s.Release, filepath.Join(s.Root, "charts", chartName), helmCreateNamespace, "--timeout", "10m"}
	if _, err = s.Helm(ctx, append(args, s.ImageValues()...)...); err != nil {
		return err
	}
	if err := s.WaitHAProxyReady(ctx, 420*time.Second); err != nil {
		return err
	}
	pods, err := s.Pods(ctx, "loadbalancer")
	if err != nil {
		return err
	}
	if len(pods) == 0 {
		return errors.New("HAProxy pods disappeared after readiness")
	}
	for i := range pods {
		result, readErr := s.Kube(ctx, nil, "exec", pods[i].Name, "-c", "haproxy", "--", "cat", "/etc/haproxy/haproxy.cfg")
		if readErr != nil {
			return readErr
		}
		if err := renderedConfig(result.Stdout); err != nil {
			return err
		}
	}
	if err := s.WaitValidated(ctx, 180*time.Second); err != nil {
		return err
	}
	if err := s.RequireNoControllerRestarts(ctx); err != nil {
		return err
	}
	s.Infof("PASS: default installation serves a rendered configuration without Gateway API")
	return nil
}

func renderedConfig(config string) error {
	if !strings.Contains(config, "default-path origin") {
		return errors.New("HAProxy still serves its bootstrap configuration: no default-path origin")
	}
	for line := range strings.SplitSeq(config, "\n") {
		if strings.HasPrefix(line, "frontend ") {
			return nil
		}
	}
	return errors.New("HAProxy configuration has no frontend")
}
