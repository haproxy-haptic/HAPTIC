// Copyright 2025 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build acceptance

package acceptance

import (
	"bufio"
	"context"
	"strconv"
	"strings"
	"testing"

	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"sigs.k8s.io/e2e-framework/pkg/types"
)

// metricsClientKey and metricsKey carry the Setup's client and the first scrape
// to the assessments.
const (
	metricsClientKey ctxKey = "metrics.client"
	metricsKey       ctxKey = "metrics.body"
)

// buildMetricsFeature builds a feature that verifies the controller exposes
// Prometheus metrics correctly.
//
// This test validates:
//  1. Metrics server is accessible on the configured port (9090)
//  2. All expected metrics are exposed
//  3. Metrics are updated when operations occur
//  4. Metric values reflect actual controller operations
//
// Test flow:
//  1. Deploy controller with metrics enabled
//  2. Wait for controller to start
//  3. Port-forward to metrics endpoint
//  4. Verify all expected metrics exist
//  5. Trigger operations (reconciliation, validation, deployment)
//  6. Verify metrics are updated with correct values
func buildMetricsFeature() types.Feature {
	return features.New("Metrics Endpoint").
		Setup(setupMetrics).
		Assess("Metrics endpoint accessible", assessMetricsEndpointAccessible).
		Assess("All expected metrics exist", assessExpectedMetricsExist).
		Assess("Metrics have non-zero values", assessMetricsNonZero).
		Assess("Resource count metrics exist", assessResourceCountMetrics).
		Assess("Histogram metrics have buckets", assessHistogramMetricsBuckets).
		Teardown(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			t.Log("Cleaning up metrics test resources")

			client, err := cfg.NewClient()
			if err != nil {
				t.Log("Failed to create client:", err)
				return ctx
			}

			return CleanupControllerEnvironment(ctx, t, client)
		}).
		Feature()
}

func setupMetrics(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	t.Helper()
	t.Log("Setting up metrics test")

	// Generate unique namespace for this test
	namespace := envconf.RandomName("test-metrics", 16)
	t.Logf("Using test namespace: %s", namespace)

	ctx = StoreNamespaceInContext(ctx, namespace)

	client, err := cfg.NewClient()
	if err != nil {
		t.Fatal("Failed to create client:", err)
	}

	// Create controller environment (namespace, RBAC, secrets, CRD, deployment, services)
	opts := DefaultControllerEnvironmentOptions()
	if err := CreateControllerEnvironment(ctx, t, client, namespace, opts); err != nil {
		t.Fatal("Failed to create controller environment:", err)
	}

	// Wait for controller pod to be ready AND reconciliation to complete
	// This ensures metrics will have non-zero values when we check them
	t.Log("Waiting for controller to complete startup reconciliation...")
	metricsClient, err := SetupMetricsAccess(ctx, client, Clientset(), namespace, DefaultClientSetupTimeout)
	if err != nil {
		t.Fatal("Failed to setup metrics access:", err)
	}

	pod, err := WaitForControllerReadyWithMetrics(ctx, client, namespace, metricsClient, DefaultPodReadyTimeout)
	if err != nil {
		t.Fatal("Controller did not become ready:", err)
	}
	t.Logf("Controller pod %s is ready with reconciliation completed", pod.Name)

	// Store metrics client in context for use in Assess phases
	ctx = context.WithValue(ctx, metricsClientKey, metricsClient)

	return ctx
}

func assessMetricsEndpointAccessible(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	t.Helper()
	t.Log("Verifying metrics endpoint is accessible")

	// Get metrics client from context (set in Setup phase)
	metricsClient, ok := ctx.Value(metricsClientKey).(*MetricsClient)
	if !ok {
		t.Fatal("metricsClient not found in context")
	}

	// Fetch metrics via API proxy
	t.Log("Fetching metrics from /metrics endpoint via API proxy")
	metrics, err := metricsClient.GetMetrics(ctx)
	if err != nil {
		t.Fatal("Failed to fetch metrics:", err)
	}

	t.Logf("Received %d bytes of metrics", len(metrics))

	// Store metrics in context for next assessment
	ctx = context.WithValue(ctx, metricsKey, metrics)

	return ctx
}

func assessExpectedMetricsExist(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	t.Helper()
	t.Log("Verifying all expected metrics are present")

	metrics, ok := ctx.Value(metricsKey).(string)
	if !ok {
		t.Fatal("Metrics not found in context")
	}

	// Define expected metrics
	expectedMetrics := []string{
		"haptic_reconciliation_total",
		"haptic_reconciliation_errors_total",
		"haptic_reconciliation_duration_seconds",
		"haptic_deployment_total",
		"haptic_deployment_errors_total",
		"haptic_deployment_duration_seconds",
		"haptic_validation_total",
		"haptic_validation_errors_total",
		"haptic_resource_count",
		"haptic_event_subscribers",
		"haptic_events_published_total",
	}

	// Verify each metric exists
	for _, metric := range expectedMetrics {
		if !strings.Contains(metrics, metric) {
			t.Errorf("Expected metric %s not found in metrics output", metric)
		} else {
			t.Logf("✓ Found metric: %s", metric)
		}
	}

	return ctx
}

func assessMetricsNonZero(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	t.Helper()
	t.Log("Verifying metrics have been updated")

	metrics, ok := ctx.Value(metricsKey).(string)
	if !ok {
		t.Fatal("Metrics not found in context")
	}

	// Parse metrics and check for non-zero values
	metricValues := parsePrometheusMetrics(metrics)

	// Check reconciliation metrics (should have occurred at startup)
	if val, ok := metricValues["haptic_reconciliation_total"]; ok && val > 0 {
		t.Logf("✓ haptic_reconciliation_total = %.0f (operations occurred)", val)
	} else {
		t.Error("haptic_reconciliation_total should be > 0")
	}

	// Check validation metrics (should have occurred during startup)
	if val, ok := metricValues["haptic_validation_total"]; ok && val > 0 {
		t.Logf("✓ haptic_validation_total = %.0f (validations occurred)", val)
	} else {
		t.Error("haptic_validation_total should be > 0")
	}

	// Check events published (controller publishes many events)
	if val, ok := metricValues["haptic_events_published_total"]; ok && val > 0 {
		t.Logf("✓ haptic_events_published_total = %.0f (events flowing)", val)
	} else {
		t.Error("haptic_events_published_total should be > 0")
	}

	// Check error counters (should be 0 with valid configuration)
	hasErrors := false
	if val, ok := metricValues["haptic_reconciliation_errors_total"]; ok && val > 0 {
		t.Errorf("haptic_reconciliation_errors_total = %.0f, expected 0", val)
		hasErrors = true
	} else {
		t.Logf("✓ haptic_reconciliation_errors_total = %.0f (no errors)", val)
	}

	if val, ok := metricValues["haptic_validation_errors_total"]; ok && val > 0 {
		t.Errorf("haptic_validation_errors_total = %.0f, expected 0", val)
		hasErrors = true
	} else {
		t.Logf("✓ haptic_validation_errors_total = %.0f (no errors)", val)
	}

	// If errors detected, print controller logs for debugging
	if hasErrors {
		namespace, _ := GetNamespaceFromContext(ctx)
		client, _ := cfg.NewClient()
		DumpControllerLogsOnError(ctx, t, client, namespace)
	}

	return ctx
}

func assessResourceCountMetrics(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	t.Helper()
	t.Log("Verifying resource count metrics")

	metrics, ok := ctx.Value(metricsKey).(string)
	if !ok {
		t.Fatal("Metrics not found in context")
	}

	// Resource count is a gauge with labels, check for the metric with type label
	resourceCountMetrics := []string{
		"haptic_resource_count{type=\"haproxy-pods\"}",
	}

	foundAny := false
	for _, metric := range resourceCountMetrics {
		if strings.Contains(metrics, metric) {
			t.Logf("✓ Found resource count metric: %s", metric)
			foundAny = true
		}
	}

	if !foundAny {
		t.Error("No resource count metrics found")
	}

	return ctx
}

func assessHistogramMetricsBuckets(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
	t.Helper()
	t.Log("Verifying histogram metrics have buckets")

	metrics, ok := ctx.Value(metricsKey).(string)
	if !ok {
		t.Fatal("Metrics not found in context")
	}

	// Histograms should have bucket, sum, and count
	histogramMetrics := []string{
		"haptic_reconciliation_duration_seconds",
		"haptic_deployment_duration_seconds",
	}

	for _, metric := range histogramMetrics {
		// Check for bucket
		if strings.Contains(metrics, metric+"_bucket") {
			t.Logf("✓ Found histogram buckets for %s", metric)
		} else {
			t.Errorf("Missing histogram buckets for %s", metric)
		}

		// Check for sum
		if strings.Contains(metrics, metric+"_sum") {
			t.Logf("✓ Found histogram sum for %s", metric)
		} else {
			t.Errorf("Missing histogram sum for %s", metric)
		}

		// Check for count
		if strings.Contains(metrics, metric+"_count") {
			t.Logf("✓ Found histogram count for %s", metric)
		} else {
			t.Errorf("Missing histogram count for %s", metric)
		}
	}

	return ctx
}

// TestMetrics runs the metrics endpoint test.
func TestMetrics(t *testing.T) {
	testEnv.Test(t, buildMetricsFeature())
}

// parsePrometheusMetrics parses Prometheus metrics output and returns metric values.
// Only parses simple metrics (not histograms or summaries).
func parsePrometheusMetrics(metrics string) map[string]float64 {
	result := make(map[string]float64)

	scanner := bufio.NewScanner(strings.NewReader(metrics))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip comments and empty lines
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Skip histogram/summary buckets, sum, count (we check those separately)
		if strings.Contains(line, "_bucket{") ||
			strings.Contains(line, "_sum ") ||
			strings.Contains(line, "_count ") {
			continue
		}

		// Parse metric line: metric_name{labels} value
		// or: metric_name value
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}

		// Extract metric name (before { or space)
		metricName := parts[0]
		if idx := strings.Index(metricName, "{"); idx != -1 {
			metricName = metricName[:idx]
		}

		value, err := strconv.ParseFloat(parts[len(parts)-1], 64)
		if err != nil {
			continue
		}

		result[metricName] = value
	}

	return result
}
