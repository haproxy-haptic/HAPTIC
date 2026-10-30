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
	"context"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/haproxy-haptic/HAPTIC/tests/e2e/httpclient"
	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
)

func vectorChildPID(ctx context.Context, pod string) (string, error) {
	out, err := execInHAProxyPod(ctx, pod, "vector", "/bin/sh", "-c", `
for _proc in /proc/[0-9]*; do
  _comm=
  IFS= read -r _comm < "$_proc/comm" || true
  if [ "$_comm" = vector ]; then
    printf '%s\n' "${_proc##*/}"
    exit 0
  fi
done
exit 1
`)
	return strings.TrimSpace(out), err
}

// TestVectorSidecar covers the parts of the vector wiring that only a live
// cluster can establish. The chart's validationTests prove the config renders;
// they cannot prove that HAProxy's datagrams arrive, that the pushed config
// actually reached the sidecar, or that HAProxy's exporter applies the chart's
// query defaults on the pod IP while honouring a scraper's own query.
//
// Deliberately NOT t.Parallel(): the restart assess kills the Vector child of a
// shared HAProxy pod. Export briefly stops while the supervisor recovers it, so a
// concurrent test could misdiagnose that intentional fault as its own failure.
func TestVectorSidecar(t *testing.T) {
	var pod string
	var pods []string

	feature := features.New("Vector sidecar: log transport, vector metrics, direct HAProxy exporter").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			pods = listHAProxyPods(t)
			if len(pods) == 0 {
				t.Fatal("no HAProxy pods found")
			}
			pod = pods[0]
			// Enabled by default; skip rather than fail if this run disabled it, so
			// the suite stays honest in both configurations.
			if resolveAccessLogContainer(ctx, t) != "vector" {
				t.Skip("vector sidecar not deployed in this configuration")
			}
			return ctx
		}).
		Assess("the rendered config activates Vector's metrics exporter", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			return assessVectorMetricsActivation(ctx, t, pods)
		}).
		Assess("vector's endpoint carries its own series and not HAProxy's", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			return assessVectorOwnSeries(ctx, t, pods)
		}).
		Assess("HAProxy's exporter answers on the pod IP with the chart's query defaults, and honours a scraper's own", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			return assessHAProxyExporterQueries(ctx, t, pod)
		}).
		Assess("the watchdog restarts a wedged Vector child without affecting HAProxy", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			return assessVectorWatchdogRecovery(ctx, t, cfg, pod)
		}).
		Assess("an access-log record reaches the sidecar over the Unix socket", assessVectorSocketLog).
		Feature()

	testEnv.Test(t, feature)
}

func assessVectorMetricsActivation(ctx context.Context, t *testing.T, pods []string) context.Context {
	t.Helper()
	for _, candidate := range pods {
		var body string
		err := testutil.WaitForCondition(ctx, testutil.FastWaitConfig(), func(c context.Context) (bool, error) {
			out, err := apiProxyGet(c, candidate, VectorMetricsPort, "metrics")
			if err != nil {
				return false, err
			}
			body = out
			return strings.Contains(body, "vector_"), nil
		})
		if err != nil {
			configState, _ := execInHAProxyPod(ctx, candidate, "haproxy", "sh", "-c",
				"ls -l /etc/haproxy/general/vector.yaml 2>&1 || true")
			t.Fatalf("Vector's metrics exporter never activated on pod %s: %v (got %d bytes)\nconfig file state: %s",
				candidate, err, len(body), configState)
		}
	}
	return ctx
}

func assessVectorOwnSeries(ctx context.Context, t *testing.T, pods []string) context.Context {
	t.Helper()
	// Fetched through the API server proxy, which reaches the pod IP exactly
	// as Prometheus does via the PodMonitor — so this asserts the real scrape
	// path. haproxy_* must NOT be here: re-exporting it was 90% of vector's
	// memory at scale, and Prometheus scrapes HAProxy's exporter directly.
	for _, candidate := range pods {
		body, err := apiProxyGet(ctx, candidate, VectorMetricsPort, "metrics")
		if err != nil {
			t.Fatalf("scraping vector's /metrics on pod %s: %v", candidate, err)
		}
		if !strings.Contains(body, "vector_") {
			t.Errorf("vector's /metrics on pod %s is missing its own vector_ series", candidate)
		}
		if strings.Contains(body, "\nhaproxy_") {
			t.Errorf("vector's /metrics on pod %s still re-exports haproxy_ series", candidate)
		}
	}
	return ctx
}

func assessHAProxyExporterQueries(ctx context.Context, t *testing.T, pod string) context.Context {
	t.Helper()
	// Through the API server proxy, i.e. from outside the pod, exactly as
	// Prometheus reaches it. The chart's set-query default must have applied:
	// haproxy_backend_agg_check_status is excluded by extraContext.prometheusExporter
	// (haproxy_backend_status, which is not, proves the exposition is real).
	body, err := apiProxyGet(ctx, pod, HAProxyStatsPort, "metrics")
	if err != nil {
		t.Fatalf("scraping HAProxy's /metrics on the pod IP: %v", err)
	}
	if !strings.Contains(body, "\nhaproxy_backend_status") {
		t.Fatalf("HAProxy's /metrics on the pod IP carries no haproxy_backend_status series (got %d bytes)", len(body))
	}
	if strings.Contains(body, "haproxy_backend_agg_check_status") {
		t.Errorf("HAProxy's /metrics on the pod IP still exposes haproxy_backend_agg_check_status, so the chart's default query was not applied")
	}
	// A scraper's own query wins wholesale: asking for exactly the excluded
	// family gets it, so an operator can always reach the raw exposition.
	own, err := apiProxyGet(ctx, pod, HAProxyStatsPort, "metrics?metrics=haproxy_backend_agg_check_status")
	if err != nil {
		t.Fatalf("scraping HAProxy's /metrics with a scraper-side query: %v", err)
	}
	if !strings.Contains(own, "haproxy_backend_agg_check_status") {
		t.Errorf("a scraper-side ?metrics= query did not override the chart's default (got %d bytes)", len(own))
	}

	// The probes share this listener and must keep working on the pod IP.
	for _, probe := range []struct{ path, want string }{{"healthz", "OK"}, {"ready", "READY"}} {
		out, err := apiProxyGet(ctx, pod, HAProxyStatsPort, probe.path)
		if err != nil {
			t.Fatalf("/%s must stay reachable on the pod IP for the kubelet's probes: %v", probe.path, err)
		}
		if !strings.Contains(out, probe.want) {
			t.Errorf("/%s returned %q, want it to contain %q", probe.path, strings.TrimSpace(out), probe.want)
		}
	}
	return ctx
}

func assessVectorWatchdogRecovery(ctx context.Context, t *testing.T, cfg *envconf.Config, pod string) context.Context {
	t.Helper()
	availabilityPath := "/vector-child-availability"
	host := prepareVectorRecoveryRoute(ctx, t, cfg, pod, availabilityPath)
	return exerciseVectorChildRecovery(ctx, t, pod, host, availabilityPath)
}

func prepareVectorRecoveryRoute(ctx context.Context, t *testing.T, cfg *envconf.Config, pod, availabilityPath string) string {
	t.Helper()
	client, err := cfg.NewClient()
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ns := NamespaceForTest(ctx, t, client)
	DumpLogsOnFailure(t, ns)
	backend := NewEchoServerBackend(ctx, t, client, ns)
	host := fmt.Sprintf("vector-child-restart-%d.localdev.me", time.Now().UnixNano())
	NewIngress(ctx, t, client, ns, &IngressSpec{
		Name:           "echo-vector-child-restart",
		Host:           host,
		BackendService: backend.Service,
		BackendPort:    backend.Port,
	})

	err = testutil.WaitForCondition(ctx, testutil.FastWaitConfig(), func(c context.Context) (bool, error) {
		status, err := selectedHAProxyStatus(c, pod, host, availabilityPath)
		return err == nil && status == "200", nil
	})
	if err != nil {
		t.Fatalf("selected HAProxy pod %s never served the fault-test route: %v", pod, err)
	}
	return host
}

func exerciseVectorChildRecovery(ctx context.Context, t *testing.T, pod, host, availabilityPath string) context.Context {
	t.Helper()
	beforeRestarts, err := podJSONPath(ctx, pod,
		`{.status.initContainerStatuses[?(@.name=="vector")].restartCount}`)
	if err != nil {
		t.Fatalf("reading vector restartCount: %v", err)
	}
	beforePID, err := vectorChildPID(ctx, pod)
	if err != nil {
		t.Fatalf("finding Vector child PID: %v", err)
	}

	stopAvailabilityMonitor, err := startSelectedHAProxyAvailabilityMonitor(ctx, pod, host, availabilityPath)
	if err != nil {
		t.Fatalf("starting selected-pod HAProxy availability monitor: %v", err)
	}
	monitorStopped := false
	defer func() {
		if !monitorStopped {
			_ = stopAvailabilityMonitor()
		}
	}()

	faultStarted := time.Now()
	if _, err := execInHAProxyPod(ctx, pod, "vector", "/bin/sh", "-c",
		`kill -STOP "$1"`, "stop-vector-child", beforePID); err != nil {
		t.Fatalf("stopping Vector child PID %s: %v", beforePID, err)
	}
	childRecovered := false
	defer func() {
		if !childRecovered {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _ = execInHAProxyPod(cleanupCtx, pod, "vector", "/bin/sh", "-c",
				`kill -CONT "$1" 2>/dev/null || true`, "resume-vector-child", beforePID)
		}
	}()

	waitForVectorChildReplacement(ctx, t, pod, beforePID)
	childRecovered = true

	afterRestarts, err := podJSONPath(ctx, pod,
		`{.status.initContainerStatuses[?(@.name=="vector")].restartCount}`)
	if err != nil {
		t.Fatalf("reading vector restartCount after child recovery: %v", err)
	}
	if afterRestarts != beforeRestarts {
		t.Fatalf("Vector container restarted while its supervisor should have recovered only the child: %s -> %s",
			beforeRestarts, afterRestarts)
	}

	assertVectorRecoveryLog(ctx, t, pod, host, faultStarted)

	monitorErr := stopAvailabilityMonitor()
	monitorStopped = true
	if monitorErr != nil {
		t.Fatalf("HAProxy became unavailable while the Vector child restarted: %v", monitorErr)
	}

	return ctx
}

func waitForVectorChildReplacement(ctx context.Context, t *testing.T, pod, beforePID string) {
	t.Helper()
	var recoveredPID, metrics string
	recoveryWait := testutil.WaitConfig{
		InitialInterval: 100 * time.Millisecond,
		MaxInterval:     time.Second,
		Timeout:         time.Minute,
		Multiplier:      1.5,
	}
	err := testutil.WaitForCondition(ctx, recoveryWait, func(c context.Context) (bool, error) {
		pid, err := vectorChildPID(c, pod)
		if err != nil {
			return false, err
		}
		if pid == beforePID {
			return false, nil
		}
		body, err := apiProxyGet(c, pod, VectorMetricsPort, "metrics")
		if err != nil {
			return false, err
		}
		recoveredPID, metrics = pid, body
		return strings.Contains(metrics, "vector_"), nil
	})
	if err != nil {
		socketState, _ := execInHAProxyPod(ctx, pod, "haproxy", "sh", "-c",
			"ls -l /run/vector/ 2>&1 || true")
		t.Fatalf("Vector watchdog did not replace stopped PID %s: %v (new PID=%q, metrics=%d bytes)\nsocket dir: %s",
			beforePID, err, recoveredPID, len(metrics), socketState)
	}
}

func assertVectorRecoveryLog(ctx context.Context, t *testing.T, pod, host string, faultStarted time.Time) {
	t.Helper()
	marker := fmt.Sprintf("/vector-child-recovered-%d", time.Now().UnixNano())
	status, err := selectedHAProxyStatus(ctx, pod, host, marker)
	if err != nil || status != "200" {
		t.Fatalf("selected HAProxy pod %s did not serve the recovery marker (status=%q): %v", pod, status, err)
	}
	rec := findAccessLogRecordInPod(ctx, t, pod, faultStarted, marker)
	if got := recordString(t, rec, "host"); got != host {
		t.Errorf("host = %q, want %q", got, host)
	}
	if got := recordString(t, rec, "instance_pod"); got != pod {
		t.Errorf("instance_pod = %q, want %q", got, pod)
	}
}

func assessVectorSocketLog(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	t.Helper()
	// End-to-end proof that the datagrams arrive. A UNIX datagram sender gets
	// NO error when nothing is listening, so a wrong socket path would look
	// identical to a working one from HAProxy's side.
	client, err := cfg.NewClient()
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ns := NamespaceForTest(ctx, t, client)
	DumpLogsOnFailure(t, ns)
	backend := NewEchoServerBackend(ctx, t, client, ns)
	host := "vector-socket-log.localdev.me"
	NewIngress(ctx, t, client, ns, &IngressSpec{
		Name:           "echo-vector-log",
		Host:           host,
		BackendService: backend.Service,
		BackendPort:    backend.Port,
	})

	// Unique per run, so the log filter can't match another test's traffic.
	marker := "/vector-log-" + ns
	since := time.Now().Add(-5 * time.Second)
	httpclient.New(t).GET(host, marker).ExpectOK(t)

	// findAccessLogRecord reads the container resolved above, i.e. `vector`.
	rec := findAccessLogRecord(ctx, t, since, marker)
	if got := recordString(t, rec, "host"); got != host {
		t.Errorf("host = %q, want %q", got, host)
	}
	pod := recordString(t, rec, "instance_pod")
	err = testutil.WaitForCondition(ctx, testutil.FastWaitConfig(), func(c context.Context) (bool, error) {
		body, err := apiProxyGet(c, pod, VectorMetricsPort, "metrics")
		if err != nil {
			return false, err
		}
		return requestCounterAliasesMatch(body, ns, "echo-vector-log", recordString(t, rec, "server"))
	})
	if err != nil {
		t.Fatalf("request counter aliases did not expose matching observations on %s: %v", pod, err)
	}
	assertVectorHistogramReload(ctx, t, cfg, pod, ns, host)
	return ctx
}

func requestCounterAliasesMatch(body, namespace, ingress, server string) (bool, error) {
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(body))
	if err != nil {
		return false, err
	}
	var aliases [2]map[string]float64
	for index, name := range []string{"haptic_ingress_controller_requests_total", "haptic_ingress_controller_requests"} {
		family := families[name]
		if family == nil || family.GetType().String() != "COUNTER" {
			return false, fmt.Errorf("%s is not exposed as a counter", name)
		}
		aliases[index] = make(map[string]float64)
		positive := false
		for _, sample := range family.Metric {
			labels := metricLabels(sample)
			if labels["namespace"] == model.LabelValue(namespace) && labels["ingress"] == model.LabelValue(ingress) {
				if selected, enabled := labels["server"]; enabled && selected != model.LabelValue(server) {
					return false, fmt.Errorf("%s server = %q, want access-log server %q", name, selected, server)
				}
				value := sample.GetCounter().GetValue()
				aliases[index][labels.String()] = value
				positive = positive || value > 0
			}
		}
		if !positive {
			return false, fmt.Errorf("%s has no observations for %s/%s", name, namespace, ingress)
		}
	}
	return maps.Equal(aliases[0], aliases[1]), nil
}

func metricLabels(sample *dto.Metric) model.LabelSet {
	labels := make(model.LabelSet)
	for _, label := range sample.Label {
		labels[model.LabelName(label.GetName())] = model.LabelValue(label.GetValue())
	}
	return labels
}
