// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/haproxy-haptic/HAPTIC/tests/admission"
	"github.com/haproxy-haptic/HAPTIC/tests/fixtures"
)

const chartOCI = "oci://ghcr.io/haproxy-haptic/haptic/charts/haptic"
const brokenTemplate = "{%- var x = %}"
const handoverBaseline = "0.6.0"
const controllerComponent = "controller"

type upgrade struct {
	session       *Session
	baseline      string
	certManager   string
	chart         string
	values        string
	fixtures      string
	work          string
	brokenChart   string
	baselineImage string
	configUIDs    map[string]types.UID
}

func (s *Session) ChartUpgrade(ctx context.Context, baseline, certManager string) error {
	work, err := os.MkdirTemp(s.Artifacts, "upgrade-")
	if err != nil {
		return err
	}
	u := &upgrade{session: s, baseline: baseline, certManager: certManager, chart: filepath.Join(s.Root, "charts", chartName), fixtures: work, work: work, brokenChart: filepath.Join(work, "broken-chart")}
	if err := fixtures.WriteChartUpgrade(work); err != nil {
		return err
	}
	u.values = filepath.Join(u.fixtures, "values.yaml")
	for _, phase := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"released baseline", u.baselineInstall},
		{helmUpgrade, u.upgradeTarget},
		{"rejected upgrade", u.rejectBrokenRelease},
		{"released rollback", u.rollbackBaseline},
		{"repair broken live configuration", u.repairBrokenDeployment},
	} {
		s.Infof("Upgrade %s: %s", baseline, phase.name)
		if err := u.observeTraffic(ctx, phase.name, phase.run); err != nil {
			return fmt.Errorf("%s: %w", phase.name, err)
		}
	}
	s.Infof("PASS: upgrade from %s preserves traffic, rejects bad releases, and repairs bad live configuration", baseline)
	return nil
}

func (u *upgrade) baselineInstall(ctx context.Context) error {
	s := u.session
	if _, err := s.Kube(ctx, nil, "wait", "--for=condition=Ready", "node", "--all", "--timeout=180s"); err != nil {
		return err
	}
	if err := s.LoadControllerImage(ctx); err != nil {
		return err
	}
	if err := s.applyCertManager(ctx, u.certManager); err != nil {
		return err
	}
	for _, deployment := range []string{certManagerNamespace, certManagerWebhook, certManagerCAInjector} {
		if _, err := s.Kube(ctx, nil, "-n", certManagerNamespace, "rollout", "status", "deployment/"+deployment, "--timeout=5m"); err != nil {
			return err
		}
	}
	if err := s.requireCertManagerHTTP2(ctx); err != nil {
		return err
	}
	values := u.values
	if u.baseline == "0.1.0" {
		values = filepath.Join(u.fixtures, "values-0.1.0.yaml")
	}
	if _, err := s.Helm(ctx, helmInstall, s.Release, chartOCI, "--version", u.baseline, "--create-namespace", "-f", values, "--timeout", "15m"); err != nil {
		return err
	}
	if err := s.WaitControllerReady(ctx, 420*time.Second); err != nil {
		return err
	}
	if err := u.recordBaselineImage(ctx); err != nil {
		return err
	}
	routes, err := os.ReadFile(filepath.Join(u.fixtures, "routes.yaml"))
	if err != nil {
		return err
	}
	if err := admission.Wait(ctx, s.Client, routes, 180*time.Second); err != nil {
		return err
	}
	if _, err := s.ConfigSnapshot(ctx); err != nil {
		return err
	}
	if _, err := s.Kube(ctx, bytes.NewReader(routes), "apply", "-f", "-"); err != nil {
		return err
	}
	if _, err := s.Kube(ctx, nil, "rollout", "status", "deployment/upgrade-backend", "--timeout=180s"); err != nil {
		return err
	}
	if err := u.requireConfigIdentity(ctx); err != nil {
		return err
	}
	return s.UpgradeTraffic(ctx, "baseline")
}

func (u *upgrade) targetArgs(chart string) []string {
	s := u.session
	args := make([]string, 0, 7+len(s.ImageValues()))
	args = append(args, helmUpgrade, s.Release, chart, "-f", u.values)
	return append(args, s.ImageValues()...)
}

func (u *upgrade) upgradeTarget(ctx context.Context) (result error) {
	s := u.session
	if err := s.EnsureHelmDiff(ctx); err != nil {
		return err
	}
	if _, err := s.Kube(ctx, nil, "apply", "--server-side", "--force-conflicts", "-f", filepath.Join(u.chart, "crds")); err != nil {
		return err
	}
	if err := s.LoadSPOAImage(ctx); err != nil {
		return err
	}
	args := append([]string{"diff"}, u.targetArgs(u.chart)...)
	if _, err := s.Helm(ctx, append(args, "--dry-run=server")...); err != nil {
		return err
	}
	if u.baseline == handoverBaseline {
		finish, err := u.observeControllers(ctx)
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, finish()) }()
	}
	if _, err := s.Helm(ctx, append(u.targetArgs(u.chart), "--timeout", "20m")...); err != nil {
		return err
	}
	if err := s.requireReleaseDeployed(ctx); err != nil {
		return err
	}
	if err := s.ControllerRollout(ctx); err != nil {
		return err
	}
	if err := s.WaitValidated(ctx, 180*time.Second); err != nil {
		return err
	}
	if err := s.RequireNoControllerRestarts(ctx); err != nil {
		return err
	}
	if err := s.UpgradeTraffic(ctx, "upgraded"); err != nil {
		return err
	}
	return s.requireNoLegacyConfigWebhook(ctx)
}

func (s *Session) requireNoLegacyConfigWebhook(ctx context.Context) error {
	webhooks, err := readJSON[admissionv1.ValidatingWebhookConfigurationList](ctx, s, "get", "validatingwebhookconfigurations")
	if err != nil {
		return err
	}
	for i := range webhooks.Items {
		for j := range webhooks.Items[i].Webhooks {
			if strings.HasPrefix(webhooks.Items[i].Webhooks[j].Name, "haproxytemplateconfig.") {
				return errors.New("legacy per-object config webhook survived the upgrade")
			}
		}
	}
	return nil
}

func (s *Session) requireReleaseDeployed(ctx context.Context) error {
	result, err := s.Helm(ctx, "status", s.Release, "-o", "json")
	if err != nil {
		return err
	}
	var status struct {
		Info struct {
			Status string `json:"status"`
		} `json:"info"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &status); err != nil {
		return err
	}
	if status.Info.Status != "deployed" {
		return fmt.Errorf("release status is %q, expected deployed", status.Info.Status)
	}
	return nil
}

func (s *Session) EnsureHelmDiff(ctx context.Context) error {
	if s.hasHelmDiff(ctx) {
		return nil
	}
	_, _ = s.Run(ctx, "helm", "plugin", helmInstall, "https://github.com/databus23/helm-diff")
	if s.hasHelmDiff(ctx) {
		return nil
	}
	_, _ = s.Run(ctx, "helm", "plugin", helmInstall, "--verify=false", "https://github.com/databus23/helm-diff")
	if !s.hasHelmDiff(ctx) {
		return errors.New("helm-diff is unavailable; install the plugin to exercise the upgrade diff")
	}
	return nil
}

func (s *Session) hasHelmDiff(ctx context.Context) bool {
	result, err := s.Run(ctx, "helm", "plugin", "list")
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(result.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "diff" {
			return true
		}
	}
	return false
}
