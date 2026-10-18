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

package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewMetrics(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)

	assert.NotNil(t, metrics)
	assert.NotNil(t, metrics.ReconciliationDuration)
	assert.NotNil(t, metrics.ReconciliationTotal)
	assert.NotNil(t, metrics.ReconciliationErrors)
	assert.NotNil(t, metrics.DeploymentDuration)
	assert.NotNil(t, metrics.DeploymentTotal)
	assert.NotNil(t, metrics.DeploymentErrors)
	assert.NotNil(t, metrics.ValidationTotal)
	assert.NotNil(t, metrics.ValidationErrors)
	assert.NotNil(t, metrics.ResourceCount)
	assert.NotNil(t, metrics.EventSubscribers)
	assert.NotNil(t, metrics.EventsPublished)
}

func TestMetrics_RecordReconciliation(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)

	// Record successful reconciliation
	metrics.RecordReconciliation(1.5, true)

	// Verify total counter incremented
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.ReconciliationTotal))

	// Verify error counter not incremented
	assert.Equal(t, 0.0, testutil.ToFloat64(metrics.ReconciliationErrors))

	// Verify histogram was created (can't easily check count)
	assert.NotNil(t, metrics.ReconciliationDuration)

	// Record failed reconciliation
	metrics.RecordReconciliation(0, false)

	// Verify total counter incremented
	assert.Equal(t, 2.0, testutil.ToFloat64(metrics.ReconciliationTotal))

	// Verify error counter incremented
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.ReconciliationErrors))
}

func TestMetrics_RecordDeployment(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)

	// Record successful deployment
	metrics.RecordDeployment(2.5, true)

	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.DeploymentTotal))
	assert.Equal(t, 0.0, testutil.ToFloat64(metrics.DeploymentErrors))
	assert.NotNil(t, metrics.DeploymentDuration)

	// Record failed deployment
	metrics.RecordDeployment(0, false)

	assert.Equal(t, 2.0, testutil.ToFloat64(metrics.DeploymentTotal))
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.DeploymentErrors))
}

func TestMetrics_RecordDeploymentReloads(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)

	metrics.RecordDeploymentReloads(2)
	assert.Equal(t, 2.0, testutil.ToFloat64(metrics.HAProxyReloadsTotal))

	// A reload-free deployment perturbs nothing.
	metrics.RecordDeploymentReloads(0)
	assert.Equal(t, 2.0, testutil.ToFloat64(metrics.HAProxyReloadsTotal))
}

func TestMetrics_RecordAgentApply(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)

	metrics.RecordAgentApply("haproxy-0", "runtime")
	metrics.RecordAgentApply("haproxy-0", "runtime")
	metrics.RecordAgentApply("haproxy-0", "reload")
	metrics.RecordAgentApply("haproxy-1", "runtime")

	assert.Equal(t, 2.0, testutil.ToFloat64(metrics.AgentApplyTotal.WithLabelValues("haproxy-0", "runtime")))
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.AgentApplyTotal.WithLabelValues("haproxy-0", "reload")))
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.AgentApplyTotal.WithLabelValues("haproxy-1", "runtime")))
}

func TestMetrics_RecordRender(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)

	metrics.RecordRender("cold")
	metrics.RecordRender("warm")
	metrics.RecordRender("warm")
	metrics.RecordRender("replay")

	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.RenderTotal.WithLabelValues("cold")))
	assert.Equal(t, 2.0, testutil.ToFloat64(metrics.RenderTotal.WithLabelValues("warm")))
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.RenderTotal.WithLabelValues("replay")))
}

func TestMetrics_RecordRenderFailure(t *testing.T) {
	metrics := NewMetrics(prometheus.NewRegistry())

	metrics.RecordRenderFailure(false)
	metrics.RecordRenderFailure(false)
	metrics.RecordRenderFailure(true)
	metrics.RecordRender("warm")

	assert.Equal(t, 2.0, testutil.ToFloat64(metrics.RenderTotal.WithLabelValues("cold")), "failed renders count as renders")
	assert.Equal(t, 2.0, testutil.ToFloat64(metrics.RenderErrors.WithLabelValues("cold")))
	assert.Equal(t, 2.0, testutil.ToFloat64(metrics.RenderTotal.WithLabelValues("warm")))
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.RenderErrors.WithLabelValues("warm")))
}

func TestMetrics_RecordApplyRejectedAndSkew(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)

	metrics.RecordApplyRejected("haproxy-0")
	metrics.RecordAgentVersionSkew()
	metrics.RecordAgentVersionSkew()

	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.ApplyRejectedTotal.WithLabelValues("haproxy-0")))
	assert.Equal(t, 0.0, testutil.ToFloat64(metrics.ApplyRejectedTotal.WithLabelValues("haproxy-1")))
	assert.Equal(t, 2.0, testutil.ToFloat64(metrics.AgentVersionSkewTotal))
}

func TestMetrics_RecordRuntimeOps(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)

	metrics.RecordRuntimeBackendOp("backend_add")
	metrics.RecordRuntimeServerOp("server_add")
	metrics.RecordRuntimeServerOp("server_add")

	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.RuntimeBackendOpsTotal.WithLabelValues("backend_add")))
	assert.Equal(t, 2.0, testutil.ToFloat64(metrics.RuntimeServerOpsTotal.WithLabelValues("server_add")))
}

func TestMetrics_RecordRuntimeBackendFallback(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)

	metrics.RecordRuntimeBackendFallback("name_collision")
	metrics.RecordRuntimeBackendFallback("name_collision")
	metrics.RecordRuntimeBackendFallback("op_rejected")

	assert.Equal(t, 2.0, testutil.ToFloat64(metrics.RuntimeBackendFallbackTotal.WithLabelValues("name_collision")))
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.RuntimeBackendFallbackTotal.WithLabelValues("op_rejected")))
}

func TestMetrics_RecordValidation(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)

	// Record successful validation
	metrics.RecordValidation(true)

	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.ValidationTotal))
	assert.Equal(t, 0.0, testutil.ToFloat64(metrics.ValidationErrors))

	// Record failed validation
	metrics.RecordValidation(false)

	assert.Equal(t, 2.0, testutil.ToFloat64(metrics.ValidationTotal))
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.ValidationErrors))
}

func TestMetrics_SetResourceCount(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)

	// Set counts for different resource types
	metrics.SetResourceCount("ingresses", 10)
	metrics.SetResourceCount("services", 25)

	// Verify values
	ingresses, err := metrics.ResourceCount.GetMetricWithLabelValues("ingresses")
	require.NoError(t, err)
	assert.Equal(t, 10.0, testutil.ToFloat64(ingresses))

	services, err := metrics.ResourceCount.GetMetricWithLabelValues("services")
	require.NoError(t, err)
	assert.Equal(t, 25.0, testutil.ToFloat64(services))

	// Update counts
	metrics.SetResourceCount("ingresses", 15)
	ingresses, err = metrics.ResourceCount.GetMetricWithLabelValues("ingresses")
	require.NoError(t, err)
	assert.Equal(t, 15.0, testutil.ToFloat64(ingresses))
}

func TestMetrics_SetEventSubscribers(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)

	metrics.SetEventSubscribers(5)
	assert.Equal(t, 5.0, testutil.ToFloat64(metrics.EventSubscribers))

	metrics.SetEventSubscribers(10)
	assert.Equal(t, 10.0, testutil.ToFloat64(metrics.EventSubscribers))
}

func TestMetrics_RecordEvent(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)

	metrics.RecordEvent()
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.EventsPublished))

	metrics.RecordEvent()
	metrics.RecordEvent()
	assert.Equal(t, 3.0, testutil.ToFloat64(metrics.EventsPublished))
}

func TestMetrics_InstanceBased(t *testing.T) {
	// Test that metrics are instance-based (not global)
	// This validates the design for application reinitialization

	// Instance 1
	registry1 := prometheus.NewRegistry()
	metrics1 := NewMetrics(registry1)
	metrics1.RecordReconciliation(1.0, true)

	// Instance 2
	registry2 := prometheus.NewRegistry()
	metrics2 := NewMetrics(registry2)
	metrics2.RecordReconciliation(2.0, true)

	// Verify instances are independent
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics1.ReconciliationTotal))
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics2.ReconciliationTotal))

	// Verify each instance only has its own metrics
	gatheredMetrics1, err := registry1.Gather()
	require.NoError(t, err)

	gatheredMetrics2, err := registry2.Gather()
	require.NoError(t, err)

	// Both should have the same metric names but different values
	assert.Len(t, gatheredMetrics1, len(gatheredMetrics2))
}

func TestMetrics_MultipleOperations(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)

	// Simulate a reconciliation cycle
	metrics.RecordReconciliation(1.5, true)
	metrics.RecordValidation(true)
	metrics.RecordDeployment(2.0, true)
	metrics.SetResourceCount("ingresses", 5)
	metrics.SetEventSubscribers(3)
	metrics.RecordEvent()

	// Verify all metrics recorded
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.ReconciliationTotal))
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.ValidationTotal))
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.DeploymentTotal))
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.EventsPublished))

	ingresses, _ := metrics.ResourceCount.GetMetricWithLabelValues("ingresses")
	assert.Equal(t, 5.0, testutil.ToFloat64(ingresses))

	assert.Equal(t, 3.0, testutil.ToFloat64(metrics.EventSubscribers))
}

func TestMetrics_AllMetricsRegistered(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)

	// GaugeVec / CounterVec metrics don't appear in registry until used
	// with a label value — seed them so Gather sees them.
	metrics.SetResourceCount("test", 0)
	metrics.SetEventSubscribers(0)
	metrics.RecordHAProxyPodRejected("version_mismatch_older")
	metrics.RecordConfigRejected("validationtests")
	metrics.RecordRender("warm")
	metrics.RecordRenderFailure(true)

	metricFamilies, err := registry.Gather()
	require.NoError(t, err)

	expectedMetrics := []string{
		"haptic_reconciliation_duration_seconds",
		"haptic_reconciliation_total",
		"haptic_reconciliation_errors_total",
		"haptic_deployment_duration_seconds",
		"haptic_deployment_total",
		"haptic_deployment_errors_total",
		"haptic_validation_total",
		"haptic_validation_errors_total",
		"haptic_config_validated",
		"haptic_config_validation_total",
		"haptic_resource_count",
		"haptic_event_subscribers",
		"haptic_events_published_total",
		"haptic_haproxy_pods_rejected_total",
		"haptic_config_rejected_total",
		"haptic_haproxy_fleet_size",
		"haptic_haproxy_fleet_converged",
		"haptic_last_full_sync_timestamp_seconds",
		"haptic_deployment_consecutive_failures",
		"haptic_render_total",
		"haptic_render_errors_total",
	}

	// Collect registered metric names
	registeredMetrics := make(map[string]bool)
	for _, mf := range metricFamilies {
		registeredMetrics[mf.GetName()] = true
	}

	// Verify all expected metrics are registered
	for _, expected := range expectedMetrics {
		assert.True(t, registeredMetrics[expected],
			"metric %s not registered", expected)
	}
}

func TestMetrics_RecordWebhookRequest(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)

	// Record successful webhook request
	metrics.RecordWebhookRequest("v1.ConfigMap", "allowed", 0.5)

	// Verify total counter incremented
	webhookTotal, err := metrics.WebhookRequestsTotal.GetMetricWithLabelValues("v1.ConfigMap", "allowed")
	require.NoError(t, err)
	assert.Equal(t, 1.0, testutil.ToFloat64(webhookTotal))

	metrics.RecordWebhookRequest("v1.ConfigMap", "denied", 0.3)

	webhookDenied, err := metrics.WebhookRequestsTotal.GetMetricWithLabelValues("v1.ConfigMap", "denied")
	require.NoError(t, err)
	assert.Equal(t, 1.0, testutil.ToFloat64(webhookDenied))

	metrics.RecordWebhookRequest("v1.Secret", "error", 0.1)

	webhookError, err := metrics.WebhookRequestsTotal.GetMetricWithLabelValues("v1.Secret", "error")
	require.NoError(t, err)
	assert.Equal(t, 1.0, testutil.ToFloat64(webhookError))
}

func TestMetrics_RecordWebhookValidation(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)

	metrics.RecordWebhookValidation("v1.ConfigMap", "allowed")

	validation, err := metrics.WebhookValidationTotal.GetMetricWithLabelValues("v1.ConfigMap", "allowed")
	require.NoError(t, err)
	assert.Equal(t, 1.0, testutil.ToFloat64(validation))

	metrics.RecordWebhookValidation("v1.ConfigMap", "denied")

	denied, err := metrics.WebhookValidationTotal.GetMetricWithLabelValues("v1.ConfigMap", "denied")
	require.NoError(t, err)
	assert.Equal(t, 1.0, testutil.ToFloat64(denied))

	// Multiple validations of same type
	metrics.RecordWebhookValidation("v1.ConfigMap", "allowed")
	metrics.RecordWebhookValidation("v1.ConfigMap", "allowed")

	validation, err = metrics.WebhookValidationTotal.GetMetricWithLabelValues("v1.ConfigMap", "allowed")
	require.NoError(t, err)
	assert.Equal(t, 3.0, testutil.ToFloat64(validation))
}

func TestMetrics_SetIsLeader(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)

	// Not leader initially
	metrics.SetIsLeader(false)
	assert.Equal(t, 0.0, testutil.ToFloat64(metrics.LeaderElectionIsLeader))

	// Become leader
	metrics.SetIsLeader(true)
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.LeaderElectionIsLeader))

	// Lose leadership
	metrics.SetIsLeader(false)
	assert.Equal(t, 0.0, testutil.ToFloat64(metrics.LeaderElectionIsLeader))
}

func TestMetrics_RecordLeadershipTransition(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)

	metrics.RecordLeadershipTransition()
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.LeaderElectionTransitionsTotal))

	metrics.RecordLeadershipTransition()
	assert.Equal(t, 2.0, testutil.ToFloat64(metrics.LeaderElectionTransitionsTotal))

	metrics.RecordLeadershipTransition()
	assert.Equal(t, 3.0, testutil.ToFloat64(metrics.LeaderElectionTransitionsTotal))
}

func TestMetrics_AddTimeAsLeader(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)

	metrics.AddTimeAsLeader(100.5)
	assert.Equal(t, 100.5, testutil.ToFloat64(metrics.LeaderElectionTimeAsLeaderSeconds))

	metrics.AddTimeAsLeader(50.25)
	assert.Equal(t, 150.75, testutil.ToFloat64(metrics.LeaderElectionTimeAsLeaderSeconds))
}

func TestMetrics_RecordEventDrop(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)

	// Record a drop from reconciler subscriber
	metrics.RecordEventDrop("reconciler", "ResourceIndexUpdatedEvent")

	// Verify aggregate counters incremented
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.EventsDroppedCritical))

	// Verify per-subscriber counter
	counter, err := metrics.EventsDroppedBySubscriber.GetMetricWithLabelValues("reconciler", "ResourceIndexUpdatedEvent")
	require.NoError(t, err)
	assert.Equal(t, 1.0, testutil.ToFloat64(counter))

	// Record another drop from a different subscriber
	metrics.RecordEventDrop("deployer", "ReconciliationCompletedEvent")

	assert.Equal(t, 2.0, testutil.ToFloat64(metrics.EventsDroppedCritical))

	deployer, err := metrics.EventsDroppedBySubscriber.GetMetricWithLabelValues("deployer", "ReconciliationCompletedEvent")
	require.NoError(t, err)
	assert.Equal(t, 1.0, testutil.ToFloat64(deployer))

	// Record multiple drops from same subscriber
	metrics.RecordEventDrop("reconciler", "ResourceIndexUpdatedEvent")
	metrics.RecordEventDrop("reconciler", "ConfigValidatedEvent")

	reconcilerIndex, err := metrics.EventsDroppedBySubscriber.GetMetricWithLabelValues("reconciler", "ResourceIndexUpdatedEvent")
	require.NoError(t, err)
	assert.Equal(t, 2.0, testutil.ToFloat64(reconcilerIndex))

	reconcilerConfig, err := metrics.EventsDroppedBySubscriber.GetMetricWithLabelValues("reconciler", "ConfigValidatedEvent")
	require.NoError(t, err)
	assert.Equal(t, 1.0, testutil.ToFloat64(reconcilerConfig))
}

func TestMetrics_LastFullSyncInitializedToStartTime(t *testing.T) {
	before := time.Now().Unix()
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)

	// Before any deployment, staleness must read as uptime, not a huge number:
	// the gauge is seeded to construction time.
	sync := testutil.ToFloat64(metrics.LastFullSyncTimestamp)
	assert.GreaterOrEqual(t, sync, float64(before),
		"last_full_sync must be initialized to construction time")
	assert.LessOrEqual(t, sync, float64(time.Now().Unix()+1))
}

func TestMetrics_SetFleetConvergence_FullSuccess(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)

	// Park last_full_sync in the distant past so a real update is unambiguous.
	metrics.LastFullSyncTimestamp.Set(1000)

	before := time.Now().Unix()
	metrics.SetFleetConvergence(3, 3, 0)

	assert.Equal(t, 3.0, testutil.ToFloat64(metrics.HAProxyFleetSize))
	assert.Equal(t, 3.0, testutil.ToFloat64(metrics.HAProxyFleetConverged))
	assert.Equal(t, 0.0, testutil.ToFloat64(metrics.DeploymentConsecutiveFailures))

	sync := testutil.ToFloat64(metrics.LastFullSyncTimestamp)
	assert.GreaterOrEqual(t, sync, float64(before),
		"a full sync must stamp last_full_sync with ~now")
	assert.LessOrEqual(t, sync, float64(time.Now().Unix()+1))
}

func TestMetrics_SetFleetConvergence_PartialFailure(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)

	// Sentinel value that a partial failure must NOT overwrite.
	metrics.LastFullSyncTimestamp.Set(12345)

	metrics.SetFleetConvergence(2, 1, 1)

	assert.Equal(t, 2.0, testutil.ToFloat64(metrics.HAProxyFleetSize))
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.HAProxyFleetConverged))
	assert.Less(t,
		testutil.ToFloat64(metrics.HAProxyFleetConverged),
		testutil.ToFloat64(metrics.HAProxyFleetSize),
		"converged < fleet_size is the diverged signal operators alert on")
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.DeploymentConsecutiveFailures))
	assert.Equal(t, 12345.0, testutil.ToFloat64(metrics.LastFullSyncTimestamp),
		"a partial failure must not advance last_full_sync")
}

func TestMetrics_SetFleetConvergence_ConsecutiveFailures(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)

	metrics.SetFleetConvergence(2, 1, 1)
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.DeploymentConsecutiveFailures))

	// A second, distinct partial failure keeps climbing.
	metrics.SetFleetConvergence(3, 0, 3)
	assert.Equal(t, 2.0, testutil.ToFloat64(metrics.DeploymentConsecutiveFailures))
}

func TestMetrics_SetFleetConvergence_FailureThenSuccessResets(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)

	// Park last_full_sync in the past so the later full sync is observably fresh.
	metrics.LastFullSyncTimestamp.Set(1000)

	metrics.SetFleetConvergence(2, 1, 1)
	metrics.SetFleetConvergence(2, 1, 1)
	assert.Equal(t, 2.0, testutil.ToFloat64(metrics.DeploymentConsecutiveFailures))

	before := time.Now().Unix()
	metrics.SetFleetConvergence(2, 2, 0)

	assert.Equal(t, 0.0, testutil.ToFloat64(metrics.DeploymentConsecutiveFailures),
		"a full sync clears consecutive failures")
	assert.GreaterOrEqual(t, testutil.ToFloat64(metrics.LastFullSyncTimestamp), float64(before),
		"a full sync advances last_full_sync to ~now")
}

func TestMetrics_ResetFleetConvergence(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)

	// Establish non-zero leader-only state.
	metrics.SetFleetConvergence(5, 3, 2)
	require.Equal(t, 5.0, testutil.ToFloat64(metrics.HAProxyFleetSize))
	require.Equal(t, 1.0, testutil.ToFloat64(metrics.DeploymentConsecutiveFailures))

	before := time.Now().Unix()
	metrics.ResetFleetConvergence()

	assert.Equal(t, 0.0, testutil.ToFloat64(metrics.HAProxyFleetSize))
	assert.Equal(t, 0.0, testutil.ToFloat64(metrics.HAProxyFleetConverged))
	assert.Equal(t, 0.0, testutil.ToFloat64(metrics.DeploymentConsecutiveFailures))
	// last_full_sync reset to ~now so a former leader doesn't accrue growing staleness.
	assert.GreaterOrEqual(t, testutil.ToFloat64(metrics.LastFullSyncTimestamp), float64(before))
}
