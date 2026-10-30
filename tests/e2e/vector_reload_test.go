// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/e2e-framework/pkg/envconf"

	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
)

const vectorReloadMarkerIP = "127.0.0.2"

func assertVectorHistogramReload(ctx context.Context, t *testing.T, cfg *envconf.Config, pod, namespace, host string) {
	t.Helper()
	for range 8 {
		status, err := selectedHAProxyStatus(ctx, pod, host, "/vector-reload-seed")
		require.NoError(t, err)
		require.Equal(t, "200", status)
	}
	before := waitForVectorHistograms(ctx, t, pod, namespace, nil, 9)
	for series, buckets := range before {
		require.EqualValues(t, 9, buckets[len(buckets)-1], "unexpected seed request count: %s", series)
	}
	client, err := cfg.NewClient()
	require.NoError(t, err)
	dyn, err := dynamic.NewForConfig(client.RESTConfig())
	require.NoError(t, err)
	configs := dyn.Resource(configGVR).Namespace(ControllerNamespace)
	original, err := configs.Get(ctx, HAProxyConfigName, metav1.GetOptions{})
	require.NoError(t, err)
	previous, existed, err := unstructured.NestedMap(original.Object, "spec", "templatingSettings", "extraContext", "vector", "logMetrics")
	require.NoError(t, err)
	modified := maps.Clone(previous)
	if modified == nil {
		modified = make(map[string]any)
	}
	_, collision := modified["hapticReloadProbe"]
	require.False(t, collision)
	modified["hapticReloadProbe"] = map[string]any{
		"enabled": true, "field": "client_ip", "kind": "enum", "metric": "reload_probe_total", "values": []any{vectorReloadMarkerIP},
	}
	traffic := vectorReloadTraffic{pod: pod, namespace: namespace, host: host, buckets: before}
	var patched *unstructured.Unstructured
	t.Cleanup(func() {
		if patched == nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), testutil.SlowWaitConfig().Timeout)
		defer cancel()
		operation := "remove"
		if existed {
			operation = "add"
		}
		state := captureVectorReloadState(cleanupCtx, t, pod)
		restored, err := patchVectorLogMetrics(cleanupCtx, configs, patched, operation, previous)
		require.NoError(t, err)
		waitForVectorReload(cleanupCtx, t, configs, restored, &traffic, state, false)
	})
	state := captureVectorReloadState(ctx, t, pod)
	patched, err = patchVectorLogMetrics(ctx, configs, original, "add", modified)
	require.NoError(t, err)
	waitForVectorReload(ctx, t, configs, patched, &traffic, state, true)
	traffic.sendMarkers(ctx, t, 21)
	waitForVectorReloadProbe(ctx, t, pod, 22)
	t.Logf("Vector retained all histogram buckets across topology reload and %d controlled requests", traffic.sent)
}

type vectorReloadTraffic struct {
	pod, namespace, host string
	buckets              map[string][]uint64
	sent                 uint64
}

func (v *vectorReloadTraffic) send(ctx context.Context, t *testing.T, count uint64) {
	t.Helper()
	v.sendRequests(ctx, t, count, false)
}

func (v *vectorReloadTraffic) sendMarkers(ctx context.Context, t *testing.T, count uint64) {
	t.Helper()
	v.sendRequests(ctx, t, count, true)
}

func (v *vectorReloadTraffic) sendRequests(ctx context.Context, t *testing.T, count uint64, marker bool) {
	t.Helper()
	for range count {
		status, err := v.requestStatus(ctx, marker)
		require.NoError(t, err)
		require.Equal(t, "200", status)
	}
	after := waitForVectorHistograms(ctx, t, v.pod, v.namespace, v.buckets, count)
	for series, buckets := range after {
		var previousDelta uint64
		for index, value := range buckets {
			require.GreaterOrEqual(t, value, v.buckets[series][index], "bucket reset: %s[%d]", series, index)
			delta := value - v.buckets[series][index]
			require.GreaterOrEqual(t, delta, previousDelta, "non-monotonic bucket increments: %s[%d]", series, index)
			previousDelta = delta
		}
		require.Equal(t, count, previousDelta, "request count changed across reload: %s", series)
	}
	v.sent += count
	v.buckets = after
}

func (v *vectorReloadTraffic) requestStatus(ctx context.Context, marker bool) (string, error) {
	if !marker {
		return selectedHAProxyStatus(ctx, v.pod, v.host, "/vector-reload-observation")
	}
	out, err := execInHAProxyPod(ctx, v.pod, "haproxy", "curl",
		"-sS", "--connect-timeout", "1", "--max-time", "2",
		"--interface", vectorReloadMarkerIP, "-o", "/dev/null", "-w", "%{http_code}",
		"-H", "Host: "+v.host, "http://127.0.0.1/vector-reload-marker")
	return strings.TrimSpace(out), err
}

type vectorReloadState struct {
	since   time.Time
	reloads int
	pid     string
}

func captureVectorReloadState(ctx context.Context, t *testing.T, pod string) vectorReloadState {
	t.Helper()
	since := time.Now().Add(-5 * time.Second)
	logs, err := readHAProxyAccessLog(ctx, t, since, pod)
	require.NoError(t, err)
	pid, err := vectorChildPID(ctx, pod)
	require.NoError(t, err)
	return vectorReloadState{since: since, reloads: strings.Count(logs, "Vector has reloaded"), pid: pid}
}

func waitForVectorReload(ctx context.Context, t *testing.T, configs dynamic.ResourceInterface, expected *unstructured.Unstructured, traffic *vectorReloadTraffic, baseline vectorReloadState, installed bool) {
	t.Helper()
	require.NoError(t, testutil.WaitForCondition(ctx, testutil.SlowWaitConfig(), func(c context.Context) (bool, error) {
		traffic.send(c, t, 1)
		current, err := configs.Get(c, expected.GetName(), metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if current.GetUID() != expected.GetUID() || current.GetGeneration() != expected.GetGeneration() {
			return false, fmt.Errorf("config identity changed: UID=%s generation=%d", current.GetUID(), current.GetGeneration())
		}
		if reason, valid := validatedAtOwnGeneration(current); !valid {
			return false, fmt.Errorf("config %s: %s", current.GetName(), reason)
		}
		body, err := readFileFromHAProxyPod(c, traffic.pod, "/etc/haproxy/general/vector.yaml")
		if err != nil || strings.Contains(body, "hapticReloadProbe") != installed {
			return false, err
		}
		logs, err := readHAProxyAccessLog(c, t, baseline.since, traffic.pod)
		if err != nil || strings.Count(logs, "Vector has reloaded") <= baseline.reloads {
			return false, err
		}
		if installed {
			traffic.sendMarkers(c, t, 1)
			waitForVectorReloadProbe(c, t, traffic.pod, 1)
		}
		return true, nil
	}))
	traffic.send(ctx, t, 1)
	pid, err := vectorChildPID(ctx, traffic.pod)
	require.NoError(t, err)
	require.Equal(t, baseline.pid, pid, "reload restarted Vector")
}

func waitForVectorReloadProbe(ctx context.Context, t *testing.T, pod string, expected float64) {
	t.Helper()
	require.NoError(t, testutil.WaitForCondition(ctx, testutil.FastWaitConfig(), func(c context.Context) (bool, error) {
		count, err := vectorReloadProbeCount(c, pod)
		return count == expected, err
	}))
}

func vectorReloadProbeCount(ctx context.Context, pod string) (float64, error) {
	body, err := apiProxyGet(ctx, pod, VectorMetricsPort, "metrics")
	if err != nil {
		return 0, err
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	family := families["haptic_reload_probe_total"]
	if family == nil {
		return 0, nil
	}
	for _, sample := range family.Metric {
		if metricLabels(sample)["value"] == model.LabelValue(vectorReloadMarkerIP) {
			return sample.GetCounter().GetValue(), nil
		}
	}
	return 0, nil
}

func patchVectorLogMetrics(ctx context.Context, configs dynamic.ResourceInterface, expected *unstructured.Unstructured, operation string, value any) (*unstructured.Unstructured, error) {
	body, err := json.Marshal([]map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": expected.GetUID()},
		{"op": "test", "path": "/metadata/generation", "value": expected.GetGeneration()},
		{"op": operation, "path": "/spec/templatingSettings/extraContext/vector/logMetrics", "value": value},
	})
	if err != nil {
		return nil, err
	}
	return configs.Patch(ctx, HAProxyConfigName, types.JSONPatchType, body, metav1.PatchOptions{})
}

func waitForVectorHistograms(ctx context.Context, t *testing.T, pod, namespace string, baseline map[string][]uint64, count uint64) map[string][]uint64 {
	t.Helper()
	var result map[string][]uint64
	require.NoError(t, testutil.WaitForCondition(ctx, testutil.FastWaitConfig(), func(c context.Context) (bool, error) {
		current, err := vectorHistogramBuckets(c, pod, namespace)
		if err != nil {
			return false, err
		}
		if len(current) != 6 {
			return false, fmt.Errorf("expected six request histograms, got %d", len(current))
		}
		if baseline != nil && len(current) != len(baseline) {
			return false, fmt.Errorf("histogram series changed: %d -> %d", len(baseline), len(current))
		}
		for name, buckets := range current {
			var previous uint64
			if baseline != nil {
				old, found := baseline[name]
				if !found || len(old) != len(buckets) {
					return false, fmt.Errorf("histogram layout changed: %s", name)
				}
				previous = old[len(old)-1]
			}
			if buckets[len(buckets)-1] < previous+count {
				return false, nil
			}
		}
		result = current
		return true, nil
	}))
	return result
}

func vectorHistogramBuckets(ctx context.Context, pod, namespace string) (map[string][]uint64, error) {
	result := make(map[string][]uint64)
	for _, port := range []int{VectorMetricsPort, 9599} {
		body, err := apiProxyGet(ctx, pod, port, "metrics")
		if err != nil {
			return nil, err
		}
		buckets, err := requestHistogramBuckets(body, namespace)
		if err != nil {
			return nil, err
		}
		maps.Copy(result, buckets)
	}
	return result, nil
}

func requestHistogramBuckets(body, namespace string) (map[string][]uint64, error) {
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	result := make(map[string][]uint64)
	for name, family := range families {
		for _, sample := range family.Metric {
			if sample.Histogram == nil {
				continue
			}
			labels := metricLabels(sample)
			if labels["namespace"] != model.LabelValue(namespace) || labels["ingress"] != "echo-vector-log" || labels["status"] != "200" {
				continue
			}
			var buckets []uint64
			for _, bucket := range sample.Histogram.Bucket {
				buckets = append(buckets, bucket.GetCumulativeCount())
			}
			buckets = append(buckets, sample.Histogram.GetSampleCount())
			result[name+labels.String()] = buckets
		}
	}
	return result, nil
}
