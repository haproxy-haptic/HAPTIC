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
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/types"
)

// startTestComponent runs a Component over a fresh registry and event bus. The
// bus starts after New() so the constructor's subscription is already in place.
func startTestComponent(t *testing.T) (*Metrics, *busevents.EventBus) {
	t.Helper()
	metrics := NewMetrics(prometheus.NewRegistry())
	eventBus := busevents.NewEventBus(100)
	component := New(metrics, eventBus)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	eventBus.Start()
	go component.Start(ctx)
	return metrics, eventBus
}

func TestNew(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)
	eventBus := busevents.NewEventBus(100)

	component := New(metrics, eventBus)
	assert.NotNil(t, component)
}

func TestComponent_ConfigInvalidEvent_IncrementsRejectedCounter(t *testing.T) {
	metrics, eventBus := startTestComponent(t)

	// A config rejected by the validationtests validator.
	eventBus.Publish(events.NewConfigInvalidEvent("v1", nil, map[string][]string{
		"validationtests": {`validationTest "x" failed: ...`},
	}))
	time.Sleep(100 * time.Millisecond)

	got := testutil.ToFloat64(metrics.ConfigRejectedTotal.WithLabelValues("validationtests"))
	assert.Equal(t, 1.0, got, "validationtests rejection should increment the counter")

	// An empty error map still counts once, under "coordinator".
	eventBus.Publish(events.NewConfigInvalidEvent("v2", nil, nil))
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.ConfigRejectedTotal.WithLabelValues("coordinator")))
}

func TestComponent_ReconciliationEvents(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)
	eventBus := busevents.NewEventBus(100)

	component := New(metrics, eventBus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Bus first; the component subscribed in New(), so no events are missed.
	eventBus.Start()
	go component.Start(ctx)

	eventBus.Publish(events.NewReconciliationCompletedEvent(1500, "", nil, nil))

	// Give component time to process
	time.Sleep(100 * time.Millisecond)

	// Verify metrics updated
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.ReconciliationTotal))
	assert.Equal(t, 0.0, testutil.ToFloat64(metrics.ReconciliationErrors))

	eventBus.Publish(events.NewReconciliationFailedEventWithStatusSnapshot("template error", "render", 2500, nil))

	time.Sleep(100 * time.Millisecond)

	// Verify error counter incremented
	assert.Equal(t, 2.0, testutil.ToFloat64(metrics.ReconciliationTotal))
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.ReconciliationErrors))
	assert.InDelta(t, 4.0, histogramSum(t, registry, "haptic_reconciliation_duration_seconds"), 1e-9,
		"a failed cycle adds its duration")
}

func histogramSum(t *testing.T, registry *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := registry.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() == name {
			return family.GetMetric()[0].GetHistogram().GetSampleSum()
		}
	}
	t.Fatalf("metric %s is not registered", name)
	return 0
}

func TestComponent_DeploymentEvents(t *testing.T) {
	metrics, eventBus := startTestComponent(t)

	eventBus.Publish(events.NewDeploymentCompletedEvent(&events.DeploymentResult{
		Total:              2,
		Succeeded:          2,
		DurationMs:         2500,
		ReloadsTriggered:   1,
		TotalAPIOperations: 9,
	}))

	time.Sleep(100 * time.Millisecond)

	// Verify metrics updated
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.DeploymentTotal))
	assert.Equal(t, 0.0, testutil.ToFloat64(metrics.DeploymentErrors))
	// The reload counter is sourced from the event payload.
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.HAProxyReloadsTotal))

	// Publish deployment with partial failure
	eventBus.Publish(events.NewDeploymentCompletedEvent(&events.DeploymentResult{
		Total:      2,
		Succeeded:  1,
		Failed:     1,
		DurationMs: 3000,
	}))

	time.Sleep(100 * time.Millisecond)

	// Should still count as success if at least one succeeded
	assert.Equal(t, 2.0, testutil.ToFloat64(metrics.DeploymentTotal))
	assert.Equal(t, 0.0, testutil.ToFloat64(metrics.DeploymentErrors))

	eventBus.Publish(events.NewInstanceDeploymentFailedEvent(
		"http://instance:5555",
		"connection refused",
		false, // not retryable
	))

	time.Sleep(100 * time.Millisecond)

	// Verify error counter incremented
	assert.Equal(t, 3.0, testutil.ToFloat64(metrics.DeploymentTotal))
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.DeploymentErrors))
}

func TestComponent_DeploymentCompleted_UpdatesFleetConvergence(t *testing.T) {
	metrics, eventBus := startTestComponent(t)

	// Full-fleet success: size == converged, no consecutive failures.
	eventBus.Publish(events.NewDeploymentCompletedEvent(&events.DeploymentResult{
		Total:      3,
		Succeeded:  3,
		DurationMs: 1000,
	}))
	// SetFleetConvergence writes fleet_size first and converged second, so
	// waiting on fleet_size alone can observe the pair half-written and assert a
	// converged that is still 0. Wait on both, as the other cases here do.
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(metrics.HAProxyFleetSize) == 3.0 &&
			testutil.ToFloat64(metrics.HAProxyFleetConverged) == 3.0
	}, 2*time.Second, 20*time.Millisecond, "fleet_size and converged should reflect the deploy")

	assert.Equal(t, 3.0, testutil.ToFloat64(metrics.HAProxyFleetConverged))
	assert.Equal(t, 0.0, testutil.ToFloat64(metrics.DeploymentConsecutiveFailures))

	// Partial failure: converged < size, consecutive failures climbs to 1.
	eventBus.Publish(events.NewDeploymentCompletedEvent(&events.DeploymentResult{
		Total:      3,
		Succeeded:  2,
		Failed:     1,
		DurationMs: 1000,
	}))
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(metrics.DeploymentConsecutiveFailures) == 1.0
	}, 2*time.Second, 20*time.Millisecond, "a partial failure must bump consecutive failures")

	assert.Equal(t, 2.0, testutil.ToFloat64(metrics.HAProxyFleetConverged))
	assert.Less(t,
		testutil.ToFloat64(metrics.HAProxyFleetConverged),
		testutil.ToFloat64(metrics.HAProxyFleetSize),
		"converged < fleet_size is the diverged signal")
}

// TestComponent_LostLeadership_ResetsFleetConvergence verifies that a replica
// which is no longer the leader stops reporting the fleet-convergence values it
// held while leader. DeploymentCompletedEvent is leader-only, so without the
// reset a former leader would keep exporting stale size/converged/failures and
// an ever-growing staleness. After the reset, converged < fleet_size is 0 < 0
// (false) so followers never false-alert.
func TestComponent_LostLeadership_ResetsFleetConvergence(t *testing.T) {
	metrics, eventBus := startTestComponent(t)

	// Become leader and record a partial-failure deploy → non-zero gauges.
	eventBus.Publish(events.NewBecameLeaderEvent("pod-1"))
	eventBus.Publish(events.NewDeploymentCompletedEvent(&events.DeploymentResult{
		Total:      3,
		Succeeded:  1,
		Failed:     2,
		DurationMs: 1000,
	}))
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(metrics.HAProxyFleetSize) == 3.0 &&
			testutil.ToFloat64(metrics.DeploymentConsecutiveFailures) == 1.0
	}, 2*time.Second, 20*time.Millisecond)

	// Lose leadership → the fleet gauges reset to non-misleading values.
	eventBus.Publish(events.NewLostLeadershipEvent("pod-1", "context cancelled"))
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(metrics.HAProxyFleetSize) == 0.0 &&
			testutil.ToFloat64(metrics.HAProxyFleetConverged) == 0.0 &&
			testutil.ToFloat64(metrics.DeploymentConsecutiveFailures) == 0.0
	}, 2*time.Second, 20*time.Millisecond, "losing leadership must reset fleet-convergence gauges")
}

func TestComponent_ValidationEvents(t *testing.T) {
	metrics, eventBus := startTestComponent(t)

	eventBus.Publish(events.NewRenderGateCompletedEvent("plan-1", true, false, true, "", false, 100))

	time.Sleep(100 * time.Millisecond)

	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.ValidationTotal))
	assert.Equal(t, 0.0, testutil.ToFloat64(metrics.ValidationErrors))

	eventBus.Publish(events.NewValidationFailedEvent([]string{"syntax error"}, 50, ""))

	time.Sleep(100 * time.Millisecond)

	assert.Equal(t, 2.0, testutil.ToFloat64(metrics.ValidationTotal))
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.ValidationErrors))
}

func TestComponent_ResourceEvents(t *testing.T) {
	metrics, eventBus := startTestComponent(t)

	// First, publish IndexSynchronizedEvent to initialize counts
	eventBus.Publish(events.NewIndexSynchronizedEvent(map[string]int{
		"ingresses": 10,
		"services":  5,
	}))

	time.Sleep(100 * time.Millisecond)

	// Verify initial resource count gauge
	ingresses, err := metrics.ResourceCount.GetMetricWithLabelValues("ingresses")
	require.NoError(t, err)
	assert.Equal(t, 10.0, testutil.ToFloat64(ingresses))

	services, err := metrics.ResourceCount.GetMetricWithLabelValues("services")
	require.NoError(t, err)
	assert.Equal(t, 5.0, testutil.ToFloat64(services))

	// Publish resource index updated event (after initial sync)
	// This adds 3 ingresses and deletes 1, so total becomes 10 + 3 - 1 = 12
	eventBus.Publish(events.NewResourceIndexUpdatedEvent(
		"ingresses",
		types.ChangeStats{
			Created:       3,
			Modified:      0,
			Deleted:       1,
			IsInitialSync: false,
		},
	))

	time.Sleep(100 * time.Millisecond)

	// Verify updated count
	ingresses, err = metrics.ResourceCount.GetMetricWithLabelValues("ingresses")
	require.NoError(t, err)
	assert.Equal(t, 12.0, testutil.ToFloat64(ingresses))

	// Update again: delete 4 ingresses, so total becomes 12 - 4 = 8
	eventBus.Publish(events.NewResourceIndexUpdatedEvent(
		"ingresses",
		types.ChangeStats{
			Created:       0,
			Modified:      0,
			Deleted:       4,
			IsInitialSync: false,
		},
	))

	time.Sleep(100 * time.Millisecond)

	ingresses, err = metrics.ResourceCount.GetMetricWithLabelValues("ingresses")
	require.NoError(t, err)
	assert.Equal(t, 8.0, testutil.ToFloat64(ingresses))
}

// TestComponent_HAProxyPodRejected verifies that HAProxyPodRejectedEvent
// increments haptic_haproxy_pods_rejected_total with the reason as a
// label. The reason enum is the same string the discovery component
// publishes (version_mismatch_older / _newer / version_check_failed)
// and the same string the commentator surfaces as a WARN-level log
// attribute — see TestPodInsight_RejectedEvent_FormatStability for the
// log-side assertion. A regression that mangled or dropped the label
// would silently break the alert "controller refuses to talk to N
// HAProxy pods" because dashboards filter by exact reason value.
func TestComponent_HAProxyPodRejected(t *testing.T) {
	metrics, eventBus := startTestComponent(t)

	// Two rejections with the same reason → counter at 2 for that label.
	eventBus.Publish(events.NewHAProxyPodRejectedEvent("haproxy-a", "version_mismatch_older"))
	eventBus.Publish(events.NewHAProxyPodRejectedEvent("haproxy-b", "version_mismatch_older"))
	// One rejection with a different reason → independent counter at 1.
	eventBus.Publish(events.NewHAProxyPodRejectedEvent("haproxy-c", "version_check_failed"))

	time.Sleep(100 * time.Millisecond)

	older, err := metrics.HAProxyPodsRejectedTotal.GetMetricWithLabelValues("version_mismatch_older")
	require.NoError(t, err)
	assert.Equal(t, 2.0, testutil.ToFloat64(older),
		"two rejections with reason=version_mismatch_older must aggregate on that label")

	probe, err := metrics.HAProxyPodsRejectedTotal.GetMetricWithLabelValues("version_check_failed")
	require.NoError(t, err)
	assert.Equal(t, 1.0, testutil.ToFloat64(probe),
		"reason labels must stay independent — different reason → separate counter")
}

// A revoked HTTP content acceptance is counted once per revoked URL.
func TestComponent_HTTPContentRevoked(t *testing.T) {
	metrics, eventBus := startTestComponent(t)

	eventBus.Publish(events.NewHTTPContentRevokedEvent("https://pages/a", "c1", false, false))
	eventBus.Publish(events.NewHTTPContentRevokedEvent("https://pages/b", "c2", true, true))

	assert.Eventually(t, func() bool {
		return testutil.ToFloat64(metrics.HTTPContentRevokedTotal) == 2
	}, time.Second, 5*time.Millisecond)
}

// Blob revocations and fetch failures have their own counters.
func TestComponent_BlobEvents(t *testing.T) {
	metrics, eventBus := startTestComponent(t)

	eventBus.Publish(events.NewBlobContentRevokedEvent("https://dbs/a", "d1", false))
	eventBus.Publish(events.NewBlobFetchFailedEvent("https://dbs/a", "refused"))
	eventBus.Publish(events.NewBlobFetchFailedEvent("https://dbs/b", "timeout"))

	assert.Eventually(t, func() bool {
		return testutil.ToFloat64(metrics.BlobContentRevokedTotal) == 1 &&
			testutil.ToFloat64(metrics.BlobFetchErrorsTotal) == 2
	}, time.Second, 5*time.Millisecond)
	assert.Zero(t, testutil.ToFloat64(metrics.HTTPContentRevokedTotal))
}

func TestComponent_AllEventTypes(t *testing.T) {
	metrics, eventBus := startTestComponent(t)

	// Publish various event types
	eventBus.Publish(events.NewReconciliationCompletedEvent(1000, "", nil, nil))
	eventBus.Publish(events.NewDeploymentCompletedEvent(&events.DeploymentResult{
		Total:      2,
		Succeeded:  2,
		Failed:     0,
		DurationMs: 2000,
	}))
	eventBus.Publish(events.NewRenderGateCompletedEvent("plan-1", true, false, true, "", false, 100))
	eventBus.Publish(events.NewIndexSynchronizedEvent(map[string]int{
		"services": 15,
	}))

	time.Sleep(100 * time.Millisecond)

	// Verify all metrics updated
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.ReconciliationTotal))
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.DeploymentTotal))
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.ValidationTotal))

	services, err := metrics.ResourceCount.GetMetricWithLabelValues("services")
	require.NoError(t, err)
	assert.Equal(t, 15.0, testutil.ToFloat64(services))

	// Every event should increment the events published counter
	// We published 4 events
	assert.Equal(t, 4.0, testutil.ToFloat64(metrics.EventsPublished))
}

func TestComponent_GracefulShutdown(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)
	eventBus := busevents.NewEventBus(100)

	component := New(metrics, eventBus)

	ctx, cancel := context.WithCancel(context.Background())

	errChan := make(chan error, 1)
	eventBus.Start()
	go func() {
		errChan <- component.Start(ctx)
	}()

	eventBus.Publish(events.NewReconciliationCompletedEvent(500, "", nil, nil))

	time.Sleep(50 * time.Millisecond)

	cancel()

	// Wait for shutdown
	select {
	case err := <-errChan:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("component did not shut down gracefully")
	}

	// Metrics should still reflect the events processed before shutdown
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.ReconciliationTotal))
}

func TestComponent_HighEventVolume(t *testing.T) {
	metrics, eventBus := startTestComponent(t)

	// Publish many events rapidly
	for i := range 100 {
		eventBus.Publish(events.NewReconciliationCompletedEvent(int64(i), "", nil, nil))
		if i%10 == 0 {
			eventBus.Publish(events.NewRenderGateCompletedEvent("plan-1", true, false, true, "", false, 100))
		}
	}

	// Give time to process
	time.Sleep(500 * time.Millisecond)

	// Verify all events processed
	assert.Equal(t, 100.0, testutil.ToFloat64(metrics.ReconciliationTotal))
	assert.Equal(t, 10.0, testutil.ToFloat64(metrics.ValidationTotal))
	// 110 events total (100 reconciliation + 10 validation)
	assert.Equal(t, 110.0, testutil.ToFloat64(metrics.EventsPublished))
}

func TestComponent_Metrics(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)
	eventBus := busevents.NewEventBus(100)

	component := New(metrics, eventBus)

	// Test that Metrics() returns the same instance
	got := component.Metrics()
	assert.Same(t, metrics, got)
}

func TestComponent_LeaderElectionEvents(t *testing.T) {
	metrics, eventBus := startTestComponent(t)

	// Initially not leader
	assert.Equal(t, 0.0, testutil.ToFloat64(metrics.LeaderElectionIsLeader))
	assert.Equal(t, 0.0, testutil.ToFloat64(metrics.LeaderElectionTransitionsTotal))

	eventBus.Publish(events.NewBecameLeaderEvent("pod-1"))

	time.Sleep(100 * time.Millisecond)

	// Verify leader metrics updated
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.LeaderElectionIsLeader))
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.LeaderElectionTransitionsTotal))

	// Wait a bit to accumulate time as leader
	time.Sleep(100 * time.Millisecond)

	eventBus.Publish(events.NewLostLeadershipEvent("pod-1", "context cancelled"))

	time.Sleep(100 * time.Millisecond)

	// Verify leader metrics updated
	assert.Equal(t, 0.0, testutil.ToFloat64(metrics.LeaderElectionIsLeader))
	assert.Equal(t, 2.0, testutil.ToFloat64(metrics.LeaderElectionTransitionsTotal))

	// Verify time as leader was recorded (should be > 0)
	timeAsLeader := testutil.ToFloat64(metrics.LeaderElectionTimeAsLeaderSeconds)
	assert.Greater(t, timeAsLeader, 0.0, "time as leader should be recorded")
}

func TestComponent_LostLeadershipWithoutBeingLeader(t *testing.T) {
	metrics, eventBus := startTestComponent(t)

	// Publish LostLeadershipEvent without ever becoming leader
	// This tests the edge case where becameLeaderAt is zero
	eventBus.Publish(events.NewLostLeadershipEvent("pod-1", "context cancelled"))

	time.Sleep(100 * time.Millisecond)

	// Verify metrics - should still record the transition
	assert.Equal(t, 0.0, testutil.ToFloat64(metrics.LeaderElectionIsLeader))
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.LeaderElectionTransitionsTotal))

	// Time as leader should remain 0 since we never became leader
	assert.Equal(t, 0.0, testutil.ToFloat64(metrics.LeaderElectionTimeAsLeaderSeconds))
}

func TestComponent_TickerUpdatesEventSubscribers(t *testing.T) {
	metrics, _ := startTestComponent(t)

	// The component itself creates at least one typed subscription, so SubscriberCount() > 0.
	// Wait for the ticker to fire (TickerPollInterval) and update the gauge.
	// We check after a generous window that the gauge is non-zero.
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(metrics.EventSubscribers) > 0
	}, 10*time.Second, 100*time.Millisecond, "event subscriber count should be > 0 after ticker fires")
}

func TestComponent_QueueWaitRecorded(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewMetrics(registry)
	eventBus := busevents.NewEventBus(100)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	eventBus.Start()
	go New(metrics, eventBus).Start(ctx)

	correlationID := "test-correlation-123"

	// Publish triggered event with a known correlation ID
	triggeredEvent := events.NewReconciliationTriggeredEvent("test", false,
		events.WithCorrelation(correlationID, ""))
	eventBus.Publish(triggeredEvent)

	time.Sleep(20 * time.Millisecond)

	// Publish started event with same correlation ID
	startedEvent := events.NewReconciliationStartedEvent("test",
		events.WithCorrelation(correlationID, correlationID))
	eventBus.Publish(startedEvent)

	time.Sleep(100 * time.Millisecond)

	// Verify histogram has exactly one observation with sum > 0
	gathered, err := registry.Gather()
	require.NoError(t, err)

	var found bool
	for _, mf := range gathered {
		if mf.GetName() == "haptic_reconciliation_queue_wait_seconds" {
			for _, m := range mf.GetMetric() {
				if m.GetHistogram() != nil && m.GetHistogram().GetSampleCount() == 1 {
					assert.Greater(t, m.GetHistogram().GetSampleSum(), 0.0)
					found = true
				}
			}
		}
	}
	assert.True(t, found, "expected one observation in haptic_reconciliation_queue_wait_seconds")
}

func TestComponent_InitialSyncSkipped(t *testing.T) {
	metrics, eventBus := startTestComponent(t)

	// Initialize counts first
	eventBus.Publish(events.NewIndexSynchronizedEvent(map[string]int{
		"ingresses": 10,
	}))

	time.Sleep(100 * time.Millisecond)

	ingresses, err := metrics.ResourceCount.GetMetricWithLabelValues("ingresses")
	require.NoError(t, err)
	assert.Equal(t, 10.0, testutil.ToFloat64(ingresses))

	// Publish resource index updated event with IsInitialSync=true
	// This should be skipped and not modify the count
	eventBus.Publish(events.NewResourceIndexUpdatedEvent(
		"ingresses",
		types.ChangeStats{
			Created:       100, // Large number that should be ignored
			Deleted:       0,
			IsInitialSync: true,
		},
	))

	time.Sleep(100 * time.Millisecond)

	// Count should remain unchanged
	ingresses, err = metrics.ResourceCount.GetMetricWithLabelValues("ingresses")
	require.NoError(t, err)
	assert.Equal(t, 10.0, testutil.ToFloat64(ingresses))
}

// TestMetrics_RuntimeMapDivergence pins the counter that makes the
// reload-free lane's degradation visible. Without it, a runtime map failing
// its read-back — and so costing every endpoint change a HAProxy reload —
// shows up only as a WARN log line, which is how it went unnoticed in
// production.
//
// The deployer records this directly rather than over the bus (ADR-0001: its
// only subscriber was this counter), so the recorder is what needs pinning.
func TestMetrics_RuntimeMapDivergence(t *testing.T) {
	metrics := NewMetrics(prometheus.NewRegistry())

	metrics.RecordRuntimeMapDivergence("pod-names.map")
	metrics.RecordRuntimeMapDivergence("pod-names.map")
	metrics.RecordRuntimeMapDivergence("host.map")

	// Labelled by map: which map degrades is what an operator acts on, and
	// the pod is deliberately not a label (one per HAProxy replica would
	// multiply the series for no added signal).
	assert.Equal(t, 2.0, testutil.ToFloat64(metrics.RuntimeMapDivergence.WithLabelValues("pod-names.map")))
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.RuntimeMapDivergence.WithLabelValues("host.map")))
	assert.Equal(t, 0.0, testutil.ToFloat64(metrics.RuntimeMapDivergence.WithLabelValues("never-seen.map")))
}

func TestComponent_HTTPContentRejected(t *testing.T) {
	metrics, bus := startTestComponent(t)
	bus.Publish(events.NewHTTPContentRejectedEvent("https://example.test/a"))
	bus.Publish(events.NewHTTPContentRejectedEvent("https://example.test/b"))
	assert.Eventually(t, func() bool {
		return testutil.ToFloat64(metrics.HTTPContentRejectedTotal) == 2
	}, time.Second, 5*time.Millisecond)
	assert.Zero(t, testutil.ToFloat64(metrics.HTTPContentRevokedTotal))
}
