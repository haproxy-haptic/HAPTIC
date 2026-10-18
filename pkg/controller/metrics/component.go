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
	"log/slog"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/buffers"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/component"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/timeouts"
	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

// ComponentName is the unique identifier for the metrics component.
const ComponentName = "metrics"

// Component is an event-driven metrics collector.
//
// Subscribes to controller events and updates metrics via the Metrics struct.
// This is an event adapter that bridges domain events to Prometheus metrics.
//
// IMPORTANT: Instance-based, created fresh per application iteration.
// When the iteration ends (context cancelled), the component stops and
// the metrics it was updating become eligible for garbage collection.
type Component struct {
	metrics        *Metrics
	eventBus       *busevents.EventBus
	eventChan      <-chan busevents.Event // Subscribed in constructor for proper startup synchronization
	resourceCounts map[string]int         // Tracks current resource counts

	// Leader election tracking
	becameLeaderAt time.Time // When this replica became leader (zero if not leader)

	// Queue wait tracking: correlationID → when ReconciliationTriggeredEvent was received
	triggeredAt map[string]time.Time
	lastEvents  *templating.RenderedEventSnapshot

	validated configValidatedTracker
}

// configValidatedTracker derives the Validated state from the events the
// StatusUpdater writes the condition from.
type configValidatedTracker struct {
	// newestVersion is the version of the newest configuration parsed.
	newestVersion string
	// verdict is the configuration's own verdict; zero before the first one.
	verdict configValidatedState
	// refused is set while HAProxy refuses the leader's renders.
	refused bool
	// countedCandidate is the newest candidate generation counted valid, so a
	// leadership replay of the same verdict does not count again.
	countedCandidate uint64
}

// New creates a new metrics component that listens to events.
//
// Parameters:
//   - metrics: The Metrics instance to update (created with metrics.NewMetrics)
//   - eventBus: The EventBus to subscribe to for events
//
// Usage:
//
//	registry := prometheus.NewRegistry()
//	metrics := metrics.NewMetrics(registry)
//	component := metrics.New(metrics, eventBus)
//	go component.Start(ctx)
//	eventBus.Start()
func New(metrics *Metrics, eventBus *busevents.EventBus) *Component {
	// Subscribe to EventBus during construction (before EventBus.Start())
	// This ensures proper startup synchronization without timing-based sleeps
	// Use typed subscription to only receive events we handle (reduces buffer pressure)
	// Critical, not lossy, despite being an observability consumer: the resource
	// gauges accumulate deltas against a once-per-iteration baseline, so a
	// silently dropped event corrupts them until the next restart.
	eventChan := eventBus.SubscribeTypes(ComponentName, buffers.Observability,
		events.EventTypeReconciliationCompleted,
		events.EventTypeReconciliationFailed,
		events.EventTypeReconciliationTriggered,
		events.EventTypeReconciliationStarted,
		events.EventTypeDeploymentCompleted,
		events.EventTypeDeploymentSkipped,
		events.EventTypeInstanceDeploymentFailed,
		events.EventTypeRenderGateCompleted,
		events.EventTypeValidationFailed,
		events.EventTypeIndexSynchronized,
		events.EventTypeResourceIndexUpdated,
		events.EventTypeBecameLeader,
		events.EventTypeLostLeadership,
		events.EventTypeHAProxyPodRejected,
		events.EventTypeConfigParsed,
		events.EventTypeConfigValidated,
		events.EventTypeConfigInvalid,
		events.EventTypeConfigValidationInconclusive,
		events.EventTypeHTTPContentRevoked,
		events.EventTypeHTTPContentRejected,
	)

	return &Component{
		metrics:        metrics,
		eventBus:       eventBus,
		eventChan:      eventChan,
		resourceCounts: make(map[string]int),
		triggeredAt:    make(map[string]time.Time),
	}
}

// Start begins the metrics event processing loop.
//
// This method blocks until the context is cancelled.
// It also periodically updates the observability drop metric from EventBus stats.
func (c *Component) Start(ctx context.Context) error {
	// Ticker to poll EventBus stats for observability drops (every 5 seconds)
	ticker := time.NewTicker(timeouts.TickerPollInterval)
	defer ticker.Stop()

	for {
		select {
		case event := <-c.eventChan:
			// Recover per-event so a panic in a metric handler can't tear down
			// the goroutine (this component can't embed component.Base — it adds
			// a ticker arm). It keeps no logger, so SafeDispatch uses the default.
			component.SafeDispatch(nil, ComponentName, event, func() {
				c.handleEvent(event)
			})
		case <-ticker.C:
			// Update observability drops from EventBus (not exposed via callback).
			// Parser cache stats are reported via CounterFuncs at scrape time,
			// so they don't need polling here.
			c.metrics.SetObservabilityDrops(c.eventBus.DroppedEventsObservability())
			// Update subscriber count
			c.metrics.SetEventSubscribers(c.eventBus.SubscriberCount())
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Metrics returns the underlying Metrics instance for direct access.
//
// This allows other components (like webhook) to record metrics directly
// without going through the event bus.
func (c *Component) Metrics() *Metrics {
	return c.metrics
}

// handleEvent processes individual events and updates corresponding metrics.
func (c *Component) handleEvent(event busevents.Event) {
	// Record every event for total events metric
	c.metrics.RecordEvent()

	// Handle specific event types
	switch e := event.(type) {
	case *events.ReconciliationTriggeredEvent:
		c.triggeredAt[e.CorrelationID()] = e.Timestamp()
	case *events.ReconciliationStartedEvent:
		c.handleReconciliationStarted(e)
	case *events.ReconciliationCompletedEvent:
		c.handleReconciliationCompleted(e)
	case *events.ReconciliationFailedEvent:
		delete(c.triggeredAt, e.CorrelationID()) // cleanup to prevent map growth
		c.metrics.RecordReconciliation(msToSeconds(e.DurationMs), false)
	case *events.DeploymentCompletedEvent:
		c.metrics.RecordDeployment(msToSeconds(e.DurationMs), e.Succeeded > 0 || e.PendingReloads > 0)
		c.metrics.RecordDeploymentReloads(e.ReloadsTriggered)
		// Leader-only event: update the fleet-convergence + config-staleness gauges.
		c.metrics.SetFleetConvergence(e.Total, e.Succeeded, e.Failed)
	case *events.DeploymentSkippedEvent:
		// The data plane serves the config without a deployment having run:
		// the render was unchanged, or the fleet was observed running it.
		c.metrics.SetFleetConvergence(e.Total, e.Total, 0)
	case *events.InstanceDeploymentFailedEvent:
		c.metrics.RecordDeployment(0, false)
	case *events.RenderGateCompletedEvent:
		c.handleRenderGateCompleted(e)
	case *events.ValidationFailedEvent:
		c.metrics.RecordValidation(false)
		c.setRefusedByHAProxy(true)
	case *events.IndexSynchronizedEvent:
		c.handleIndexSynchronized(e)
	case *events.ResourceIndexUpdatedEvent:
		c.handleResourceIndexUpdated(e)
	case *events.BecameLeaderEvent:
		c.becameLeaderAt = e.Timestamp()
		c.metrics.SetIsLeader(true)
		c.metrics.RecordLeadershipTransition()
	case *events.LostLeadershipEvent:
		c.handleLostLeadership(e)
	case *events.HAProxyPodRejectedEvent:
		c.metrics.RecordHAProxyPodRejected(e.Reason)
	case *events.HTTPContentRejectedEvent:
		c.metrics.RecordHTTPContentRejected()
	case *events.HTTPContentRevokedEvent:
		c.metrics.RecordHTTPContentRevoked()
	default:
		c.handleConfigEvent(event)
	}
}

func (c *Component) handleRenderGateCompleted(event *events.RenderGateCompletedEvent) {
	c.metrics.RecordValidation(event.OK)
	if event.Newest && event.OK {
		c.setRefusedByHAProxy(false)
	}
}

func (c *Component) handleConfigEvent(event busevents.Event) {
	switch e := event.(type) {
	case *events.ConfigParsedEvent:
		c.validated.newestVersion = e.Version
	case *events.ConfigValidatedEvent:
		c.handleConfigValidated(e)
	case *events.ConfigInvalidEvent:
		c.handleConfigInvalid(e)
	case *events.ConfigValidationInconclusiveEvent:
		c.metrics.recordConfigValidation(configValidationInconclusive)
		c.setConfigVerdict(configInconclusive)
	}
}

func (c *Component) handleConfigInvalid(event *events.ConfigInvalidEvent) {
	// A missing validator verdict is still a coordinator rejection.
	if len(event.ValidationErrors) == 0 {
		c.metrics.RecordConfigRejected(configRejectedByCoordinator)
	}
	for validator := range event.ValidationErrors {
		c.metrics.RecordConfigRejected(validator)
	}
	c.metrics.recordConfigValidation(configValidationInvalid)
	c.setConfigVerdict(configInvalid)
}

// handleConfigValidated applies fresh verdicts. An event without a candidate
// generation re-announces the running config (startup, leadership replay); it
// applies only while that config is the newest parsed and nothing judged it.
func (c *Component) handleConfigValidated(event *events.ConfigValidatedEvent) {
	tracker := &c.validated
	if event.ActiveSnapshotRestore {
		return
	}
	if event.CandidateGeneration == 0 {
		if event.Version == tracker.newestVersion && tracker.verdict == (configValidatedState{}) {
			c.setConfigVerdict(configValidationSucceeded)
		}
		return
	}
	if event.CandidateGeneration > tracker.countedCandidate {
		tracker.countedCandidate = event.CandidateGeneration
		c.metrics.recordConfigValidation(configValidationValid)
		c.setConfigVerdict(configValidationSucceeded)
	}
}

// setConfigVerdict also ends a HAProxy refusal, as a configuration load
// rewrites the condition.
func (c *Component) setConfigVerdict(verdict configValidatedState) {
	c.validated.verdict = verdict
	c.validated.refused = false
	c.metrics.setConfigValidated(verdict)
}

// Render failures override only an accepted configuration verdict.
func (c *Component) setRefusedByHAProxy(refused bool) {
	tracker := &c.validated
	if tracker.refused == refused || tracker.verdict != configValidationSucceeded {
		return
	}
	tracker.refused = refused
	if refused {
		c.metrics.setConfigValidated(configRefusedByHAProxy)
		return
	}
	c.metrics.setConfigValidated(tracker.verdict)
}

func (c *Component) handleReconciliationStarted(e *events.ReconciliationStartedEvent) {
	if t, ok := c.triggeredAt[e.CorrelationID()]; ok {
		c.metrics.RecordQueueWait(time.Since(t).Seconds())
		delete(c.triggeredAt, e.CorrelationID())
	}
}

func (c *Component) handleIndexSynchronized(e *events.IndexSynchronizedEvent) {
	for resourceType, count := range e.ResourceCounts {
		c.resourceCounts[resourceType] = count
		c.metrics.SetResourceCount(resourceType, count)
	}
}

func (c *Component) handleResourceIndexUpdated(e *events.ResourceIndexUpdatedEvent) {
	// Skip initial sync events - we'll get the totals from IndexSynchronizedEvent
	if e.ChangeStats.IsInitialSync {
		return
	}

	newCount := c.resourceCounts[e.ResourceTypeName] + e.ChangeStats.Created - e.ChangeStats.Deleted
	c.resourceCounts[e.ResourceTypeName] = newCount
	c.metrics.SetResourceCount(e.ResourceTypeName, newCount)
}

func (c *Component) handleLostLeadership(e *events.LostLeadershipEvent) {
	c.metrics.SetIsLeader(false)
	c.metrics.RecordLeadershipTransition()

	if !c.becameLeaderAt.IsZero() {
		c.metrics.AddTimeAsLeader(e.Timestamp().Sub(c.becameLeaderAt).Seconds())
		c.becameLeaderAt = time.Time{}
	}

	// A follower must not keep reporting the fleet-convergence gauges it set
	// while leader (DeploymentCompletedEvent is leader-only), so reset them:
	// converged < fleet_size becomes 0 < 0 (no false alert) and staleness stops
	// growing.
	c.metrics.ResetFleetConvergence()
	c.metrics.SetRenderWarnings(nil)
	c.lastEvents = nil
	c.setRefusedByHAProxy(false)
}

func (c *Component) handleReconciliationCompleted(event *events.ReconciliationCompletedEvent) {
	c.metrics.RecordReconciliation(msToSeconds(event.DurationMs), true)
	c.metrics.SetRenderProfiles(event.ProfileCount)
	if c.becameLeaderAt.IsZero() {
		return
	}
	if err := c.updateRenderWarnings(event); err != nil {
		slog.Error("Cannot update render warning metrics", "error", err, "correlation_id", event.CorrelationID())
	}
}

func (c *Component) updateRenderWarnings(event *events.ReconciliationCompletedEvent) error {
	cycle, _, err := event.AuthenticatedRenderIdentity()
	if err != nil {
		return err
	}
	snapshot, err := cycle.RenderedEventSnapshot()
	if err != nil {
		return err
	}
	if c.lastEvents != nil {
		same, err := snapshot.SameRoot(c.lastEvents)
		if err != nil || same {
			return err
		}
	}
	rendered, err := snapshot.Events()
	if err != nil {
		return err
	}
	counts := make(map[string]int)
	for _, event := range rendered {
		if event.Type == templating.EventTypeWarning {
			counts[event.Reason]++
		}
	}
	c.metrics.SetRenderWarnings(counts)
	c.lastEvents = snapshot
	return nil
}

// msToSeconds converts a duration in milliseconds to seconds.
func msToSeconds(ms int64) float64 {
	return float64(ms) / 1000.0
}
