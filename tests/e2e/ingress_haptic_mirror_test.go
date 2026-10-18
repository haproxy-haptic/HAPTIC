// Copyright 2025 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/haproxy-haptic/HAPTIC/tests/e2e/httpclient"
	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
)

// readEchoPodLogs returns the stdout of the echo-server container(s) selected
// by labelSelector in namespace. ealen/echo-server logs one JSON line per
// received request (including the request's originalUrl), so a test can grep
// these logs for a unique marker path to prove a request actually reached the
// backend — the only observable signal for a fire-and-forget mirror, whose
// HTTP response HAProxy discards.
func readEchoPodLogs(ctx context.Context, namespace, labelSelector string) (string, error) {
	cmd := exec.CommandContext(ctx, "kubectl",
		kubeconfigFlag, kubeconfigPath,
		"-n", namespace,
		"logs", "-l", labelSelector, "-c", "server", "--tail=2000",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("kubectl logs -n %s -l %s: %w (stderr: %s)", namespace, labelSelector, err, stderr.String())
	}
	return stdout.String(), nil
}

// waitForEchoPodLogsToContain waits until every marker appears in the logs
// readEchoPodLogs returns.
func waitForEchoPodLogsToContain(ctx context.Context, namespace, labelSelector string, markers ...string) error {
	return testutil.WaitForCondition(ctx, testutil.FastWaitConfig(), func(c context.Context) (bool, error) {
		logs, err := readEchoPodLogs(c, namespace, labelSelector)
		if err != nil {
			return false, err
		}
		for _, marker := range markers {
			if !strings.Contains(logs, marker) {
				return false, fmt.Errorf("marker %q is not in the logs of %s", marker, labelSelector)
			}
		}
		return true, nil
	})
}

// TestHapticMirrorTarget exercises fire-and-forget request mirroring driven by
// the native `haproxy-haptic.org/mirror-target` Ingress annotation, end to end
// through the SPOA hub mirror plugin (v0.6.0+ arg_targets contract). The
// annotation path has offline chart validationTests for the generated config,
// but nothing else verifies that a real request is actually copied to the
// mirror backend at runtime — this closes that gap.
//
// Topology: two echo-server backends in the test namespace. `echo-server` is
// the primary route target; `echo-mirror` is the mirror target and receives
// ONLY mirrored traffic (nothing else routes to it), so its pod logs are a
// clean record of what got mirrored. An Ingress for `mirrorHost` routes to the
// primary and carries `haproxy-haptic.org/mirror-target: http://echo-mirror…`.
//
// The mirror is fire-and-forget (HAProxy discards the mirrored response), and
// ealen/echo-server is stateless, so the only observable is the mirror pod's
// stdout. We verify:
//
//  1. Positive: every request to `mirrorHost`, each at its own marker path, is
//     copied to the mirror backend (each marker appears in echo-mirror's logs).
//  2. Negative (host-gating): a request to a DIFFERENT host is NOT mirrored.
//     Proven deterministically with a barrier — after the non-mirror request we
//     send a later request to `mirrorHost` and wait for ITS marker to land at
//     the mirror; once it has, a non-mirror request that was going to be
//     mirrored would have landed too, so the non-mirror marker's absence is
//     conclusive rather than a race.
//
// The mirror plugin is guaranteed enabled in the default e2e profile
// (gateway library on → spoaHub.plugins.mirror auto-enabled), so no library
// guard is needed; if it were off, the chart would fail the render for any
// Ingress carrying this annotation.
func TestHapticMirrorTarget(t *testing.T) {
	const (
		mirrorHost    = "ingress-haptic-mirror.localdev.me"
		nonMirrorHost = "ingress-haptic-mirror-off.localdev.me"
		mirrorLabel   = "app=echo-mirror"
		// The chart default of spoaHub.plugins.mirror.targetTimeoutMs.
		mirrorTargetBudget = 2 * time.Second
	)

	feature := features.New("Ingress: haproxy-haptic.org/mirror-target copies matching requests to the mirror backend").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			client, err := cfg.NewClient()
			if err != nil {
				t.Fatalf("new client: %v", err)
			}
			ns := NamespaceForTest(ctx, t, client)
			DumpLogsOnFailure(t, ns)

			primary := NewEchoServerBackend(ctx, t, client, ns)
			mirror := NewNamedEchoServerBackend(ctx, t, client, ns, "echo-mirror")

			// The mirror plugin (co-located with HAProxy in the controller
			// namespace) dispatches to this authority; only host:port is used,
			// the live request path/query is re-attached by the plugin.
			mirrorTarget := fmt.Sprintf("http://%s.%s.svc.cluster.local", mirror.Service, ns)

			NewIngress(ctx, t, client, ns, &IngressSpec{
				Name:           "echo-primary",
				Host:           mirrorHost,
				BackendService: primary.Service,
				BackendPort:    primary.Port,
				Annotations: map[string]string{
					"haproxy-haptic.org/mirror-target": mirrorTarget,
				},
			})

			// The plugin sends each copy once from the HAProxy pod and drops it
			// if the target can't answer within its budget (#403).
			waitForHAProxyPodsToReach(ctx, t, client, mirrorTarget+"/hm-ready-"+ns, mirrorTargetBudget)

			// Unique per-test markers so parallel tests / prior runs can't
			// pollute the grep. The namespace name is unique per test.
			posMarkers := []string{"/hm-pos-0-" + ns, "/hm-pos-1-" + ns, "/hm-pos-2-" + ns}
			barrierMarker := "/hm-barrier-" + ns
			negMarker := "/hm-neg-" + ns

			// (1) Positive: every request through the primary is copied. Each
			// returns 200 from the primary regardless of the mirror.
			for _, marker := range posMarkers {
				httpclient.New(t).GET(mirrorHost, marker).ExpectOK(t)
			}
			if err := waitForEchoPodLogsToContain(ctx, ns, mirrorLabel, posMarkers...); err != nil {
				t.Fatalf("requests to %s were not all mirrored to %s: %v", mirrorHost, mirrorTarget, err)
			}

			// (2) Negative: a request to a different host must not be mirrored.
			// nonMirrorHost has no Ingress, so HAProxy answers from its default
			// backend — but the request still traverses the frontend where the
			// mirror set-var lives, gated on host == mirrorHost, so it must not
			// fire. Response status is irrelevant here.
			_, _ = httpclient.New(t).GET(nonMirrorHost, negMarker).Do(ctx)

			// Barrier: a later mirror-host request whose landing proves the
			// mirror pipeline has drained past the non-mirror request above.
			httpclient.New(t).GET(mirrorHost, barrierMarker).ExpectOK(t)
			if err := waitForEchoPodLogsToContain(ctx, ns, mirrorLabel, barrierMarker); err != nil {
				t.Fatalf("barrier request to %s was not mirrored: %v", mirrorHost, err)
			}

			logs, err := readEchoPodLogs(ctx, ns, mirrorLabel)
			if err != nil {
				t.Fatalf("read mirror backend logs: %v", err)
			}
			if strings.Contains(logs, negMarker) {
				t.Fatalf("request to non-mirror host %s WAS mirrored: marker %q found in mirror backend logs — mirror-target is not host-scoped", nonMirrorHost, negMarker)
			}

			return ctx
		}).
		Assess("matching request mirrored, non-matching host not mirrored", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			// Conditions are verified in Setup where the namespace/backends are
			// in scope; this Assess keeps the feature wiring aligned with the
			// rest of the suite (see spoa_hub_reload_test.go for the pattern).
			return ctx
		})

	testEnv.Test(t, feature.Feature())
}
