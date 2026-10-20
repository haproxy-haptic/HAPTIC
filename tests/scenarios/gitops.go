// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"

	"github.com/haproxy-haptic/HAPTIC/tests/admission"
	"github.com/haproxy-haptic/HAPTIC/tests/fixtures"
)

const (
	argoVersion      = "v3.5.3"
	fluxVersion      = "v2.9.5"
	gitopsRepository = "http://chart-repository.gitops-test.svc.cluster.local:8080"
	argoNamespace    = "argocd"
	fluxNamespace    = "flux-system"
	gitopsNamespace  = "gitops-test"
	phaseInstalled   = "installed"
	phaseUpgraded    = "upgraded"
	phaseRejected    = "rejected"
	phaseRecovered   = "recovered"
)

type GitOpsOptions struct{ Provider, Certificates, Image string }

type gitops struct {
	session   *Session
	options   *GitOpsOptions
	versions  map[string]string
	images    map[string]string
	webhookCA string
}

func (s *Session) GitOps(ctx context.Context, options *GitOpsOptions) error {
	g := &gitops{session: s, options: options, versions: map[string]string{}, images: map[string]string{}}
	if err := g.setup(ctx); err != nil {
		return err
	}
	if err := g.exercise(ctx); err != nil {
		return err
	}
	for _, kind := range []string{podsResource, "jobs"} {
		if _, err := s.Kube(ctx, nil, "get", kind, "-o", "json"); err != nil {
			return err
		}
	}
	if err := s.Save("result.json", map[string]any{"passed": true, "provider": options.Provider, "phases": []string{helmInstall, "repeat", helmUpgrade, "rejection", "recovery"}}); err != nil {
		return err
	}
	s.Infof("PASS: %s install, stable sync, upgrade, rejection, and recovery", options.Provider)
	return nil
}

func (g *gitops) setup(ctx context.Context) error {
	s := g.session
	for _, namespace := range []string{s.Namespace, certManagerNamespace, gitopsNamespace, argoNamespace} {
		if err := s.Apply(ctx, resource("v1", "Namespace", "", namespace)); err != nil {
			return err
		}
	}
	if g.options.Certificates == certManagerNamespace {
		if err := g.installManifest(ctx, certManagerNamespace, certManagerManifestURL(CertManagerVersion), "", loopbackCertManagerWebhook); err != nil {
			return err
		}
		for _, name := range []string{certManagerNamespace, certManagerWebhook, certManagerCAInjector} {
			if err := g.rollout(ctx, certManagerNamespace, "deployment/"+name); err != nil {
				return err
			}
		}
		if err := s.requireCertManagerHTTP2(ctx); err != nil {
			return err
		}
	} else if err := g.externalCertificates(ctx); err != nil {
		return err
	}
	if err := g.prepareCharts(ctx); err != nil {
		return err
	}
	if err := s.LoadSPOAImage(ctx); err != nil {
		return err
	}
	secret := resource("v1", "Secret", s.Namespace, gitopsCredentialsSecret)
	secret["stringData"] = map[string]string{"dataplane_username": "admin", "dataplane_password": "gitops-lifecycle-fixture-only"}
	if err := s.Apply(ctx, secret); err != nil {
		return err
	}
	return g.installProvider(ctx)
}

func (g *gitops) rollout(ctx context.Context, namespace, target string) error {
	_, err := g.session.Kube(ctx, nil, "-n", namespace, "rollout", "status", target, "--timeout=7m")
	return err
}

func (g *gitops) exercise(ctx context.Context) error {
	if err := g.sync(ctx, phaseInstalled, false); err != nil {
		return err
	}
	routes, err := fixtures.ChartUpgrade("routes.yaml")
	if err != nil {
		return err
	}
	if err := g.applyRoutes(ctx, routes); err != nil {
		return err
	}
	initial, err := g.verifiedSnapshot(ctx, phaseInstalled)
	if err != nil {
		return err
	}
	if err := g.sync(ctx, phaseInstalled, true); err != nil {
		return err
	}
	repeated, err := g.verifiedSnapshot(ctx, "repeat")
	if err != nil {
		return err
	}
	if !initial.equal(repeated) {
		return errors.New("unchanged sync replaced pods, rotated Secrets, or changed configuration")
	}
	if err := g.sync(ctx, phaseUpgraded, false); err != nil {
		return err
	}
	upgraded, err := g.verifiedSnapshot(ctx, phaseUpgraded)
	if err != nil {
		return err
	}
	if !maps.Equal(initial.Secrets, upgraded.Secrets) {
		return errors.New("chart upgrade rotated credentials or certificates")
	}
	if slices.Equal(initial.Pods, upgraded.Pods) {
		return errors.New("chart upgrade did not replace workload pods")
	}
	if err := g.sync(ctx, phaseRejected, false); err != nil {
		return err
	}
	if err := g.failedPreflight(ctx); err != nil {
		return err
	}
	rejected, err := g.snapshot(ctx, phaseRejected)
	if err != nil {
		return err
	}
	if !upgraded.equal(rejected) {
		return errors.New("rejected candidate changed serving configuration, pods, or Secrets")
	}
	if err := g.verifyTraffic(ctx, phaseRejected); err != nil {
		return err
	}
	if err := g.sync(ctx, phaseRecovered, false); err != nil {
		return err
	}
	recovered, err := g.verifiedSnapshot(ctx, phaseRecovered)
	if err != nil {
		return err
	}
	if !maps.Equal(initial.Secrets, recovered.Secrets) {
		return errors.New("recovery rotated credentials or certificates")
	}
	return nil
}

func (g *gitops) failedPreflight(ctx context.Context) error {
	jobs, err := readJSON[batchv1.JobList](ctx, g.session, "get", "jobs")
	if err != nil {
		return err
	}
	var names []string
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if strings.HasSuffix(job.Name, "-pre-rollout") {
			if job.Status.Failed == 0 {
				return fmt.Errorf("pre-rollout job %s did not fail", job.Name)
			}
			names = append(names, job.Name)
		}
	}
	if len(names) != 1 {
		return fmt.Errorf("expected one failed pre-rollout job, found %d", len(names))
	}
	_, err = g.session.Kube(ctx, nil, "logs", "job/"+names[0])
	return err
}

func (g *gitops) applyRoutes(ctx context.Context, routes []byte) error {
	// A synced Application doesn't mean the admission webhook is serving yet.
	if err := admission.Wait(ctx, g.session.Client, routes, 180*time.Second); err != nil {
		return err
	}
	if _, err := g.session.Kube(ctx, bytes.NewReader(routes), "apply", "-f", "-"); err != nil {
		return err
	}
	return g.rollout(ctx, g.session.Namespace, "deployment/upgrade-backend")
}

func (g *gitops) verifyTraffic(ctx context.Context, phase string) error {
	if err := g.session.ControllerRollout(ctx); err != nil {
		return err
	}
	if err := g.session.WaitValidated(ctx, 180*time.Second); err != nil {
		return err
	}
	if err := g.session.UpgradeTraffic(ctx, phase); err != nil {
		return err
	}
	return g.waitPublication(ctx, phase, 180*time.Second, 2*time.Second)
}

func (g *gitops) verifiedSnapshot(ctx context.Context, phase string) (*gitopsSnapshot, error) {
	if err := g.verifyTraffic(ctx, phase); err != nil {
		return nil, err
	}
	return g.snapshot(ctx, phase)
}
