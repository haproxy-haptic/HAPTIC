// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/haproxy-haptic/HAPTIC/tests/kindutil"
	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
)

const certManagerNamespace = "cert-manager"

// renovate: datasource=github-releases depName=cert-manager/cert-manager
const CertManagerVersion = "v1.21.2"

type PhaseError struct {
	Code  int
	Phase string
	Err   error
}

func (e *PhaseError) Error() string { return fmt.Sprintf("%s: %v", e.Phase, e.Err) }
func (e *PhaseError) Unwrap() error { return e.Err }

type DefaultsOptions struct {
	Image              string
	CertManagerVersion string
	SPOATag            string
	Timeout            time.Duration
}

func (s *Session) HelmDefaults(ctx context.Context, options *DefaultsOptions) error {
	for _, phase := range []struct {
		code int
		name string
		run  func(context.Context) error
	}{
		{2, certManagerNamespace, func(ctx context.Context) error { return s.installDefaultsCertManager(ctx, options) }},
		{3, "default chart install", func(ctx context.Context) error { return s.installDefaultsChart(ctx, options) }},
		{5, "certificates", s.defaultsCertificates},
		{4, "pod readiness", func(ctx context.Context) error { return s.defaultsPods(ctx, options.Timeout) }},
		{10, "config admission", func(ctx context.Context) error { return s.defaultsAdmission(ctx, options.Timeout) }},
		{8, "HAProxy syntax", s.defaultsSyntax},
		{9, "bootstrap retirement", s.defaultsRetirement},
	} {
		s.Infof("Checking %s", phase.name)
		if err := phase.run(ctx); err != nil {
			return &PhaseError{Code: phase.code, Phase: phase.name, Err: err}
		}
	}
	if err := s.defaultsSmoke(ctx); err != nil {
		return err
	}
	s.Infof("PASS: chart defaults pass admission, certificates, syntax, worker retirement, traffic, metrics, and resource application checks")
	return nil
}

func (s *Session) installDefaultsCertManager(ctx context.Context, options *DefaultsOptions) error {
	if err := s.applyCertManager(ctx, options.CertManagerVersion); err != nil {
		return err
	}
	for _, name := range []string{certManagerNamespace, certManagerCAInjector, certManagerWebhook} {
		if _, err := s.Kube(ctx, nil, "-n", certManagerNamespace, "wait", "--for=condition=Available", "deployment/"+name, "--timeout="+options.Timeout.String()); err != nil {
			return err
		}
	}
	if err := s.requireCertManagerHTTP2(ctx); err != nil {
		return err
	}
	err := poll(ctx, 60*time.Second, 2*time.Second, "cert-manager webhook endpoints", func(ctx context.Context) (testutil.PollResult, error) {
		result, err := s.Kube(ctx, nil, "-n", certManagerNamespace, "get", "endpoints", certManagerWebhook, "-o", "jsonpath={.subsets[*].addresses[*].ip}")
		if err != nil {
			return testutil.PollPending, err
		}
		if strings.TrimSpace(result.Stdout) == "" {
			return testutil.PollPending, errors.New("cert-manager webhook has no endpoints")
		}
		if _, err := s.Kube(ctx, nil, "get", "validatingwebhookconfiguration", certManagerWebhook, "-o", fieldName); err != nil {
			return testutil.PollPending, err
		}
		return testutil.PollSucceeded, nil
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		s.Infof("cert-manager webhook endpoints are not ready: %v; chart installation will verify admission", err)
	}
	return nil
}

func (s *Session) installDefaultsChart(ctx context.Context, options *DefaultsOptions) error {
	if options.Image != "" {
		local, err := s.Run(ctx, "docker", "image", "ls", "--quiet", options.Image)
		if err != nil {
			return err
		}
		if strings.TrimSpace(local.Stdout) != "" {
			if err := s.loadControllerImage(ctx, options.Image); err != nil {
				return err
			}
		} else {
			s.Infof("Image %s is not local; Kubernetes will pull it", options.Image)
		}
	}
	images, err := kindutil.LoadChartImages(s.HAProxyVersion)
	if err != nil {
		return err
	}
	if err := kindutil.ValidateChartImageTag(images.SPOAHub, options.SPOATag); err != nil {
		return err
	}
	if err := s.LoadSPOAImage(ctx); err != nil {
		return err
	}
	args := []string{helmUpgrade, "--install", s.Release, filepath.Join(s.Root, "charts", chartName), "--create-namespace"}
	if options.Image != "" {
		args = append(args, s.ImageValues()...)
	}
	_, err = s.Helm(ctx, args...)
	return err
}

func (s *Session) defaultsPods(ctx context.Context, timeout time.Duration) error {
	for _, component := range []string{"controller", "loadbalancer"} {
		if _, err := s.Kube(ctx, nil, "wait", "--for=condition=Ready", "pod", "-l", "app.kubernetes.io/component="+component, "--timeout="+timeout.String()); err != nil {
			return err
		}
	}
	return nil
}
