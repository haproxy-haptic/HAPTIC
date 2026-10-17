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

package reconciler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/component"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/metrics"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/pipeline"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercycle"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderoutput"
	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

// PipelineExecutor defines the interface for executing the render-validate pipeline.
// This allows mocking in tests.
type PipelineExecutor interface {
	Execute(ctx context.Context, provider stores.StoreProvider, mode rendercontext.RenderMode, extraOpts ...rendercontext.Option) (*pipeline.PipelineResult, error)
}

// CurrentFilesAuthority binds exact render outputs to a leader term.
type CurrentFilesAuthority interface {
	BeginTerm() uint64
	EndTerm(generation uint64)
	Snapshot(generation uint64) (map[string]string, error)
	AcceptOutput(generation uint64, output *renderoutput.Snapshot) error
	ConfirmOutput(generation uint64, output *renderoutput.Snapshot) error
	RollbackOutput(generation uint64, output *renderoutput.Snapshot) error
}

type exactCurrentFilesAuthority interface {
	ExactSource(generation uint64) (rendercontext.CurrentAuxFilesSource, error)
}

const (
	// CoordinatorComponentName is the unique identifier for the ReconciliationCoordinator.
	CoordinatorComponentName = "reconciliation-coordinator"

	// CoordinatorEventBufferSize bounds transport to the independently drained mailbox.
	CoordinatorEventBufferSize = busevents.ResourceChurnSubscriberBuffer
)

// Coordinator orchestrates reconciliation by calling the Pipeline directly.
//
// Render and validate run synchronously inside Pipeline.Execute() (ADR-0001 —
// no event hop), and the Coordinator publishes the appropriate events for
// downstream consumers based on the result.
//
// Flow:
//  1. ReconciliationTriggeredEvent received
//  2. Publish ReconciliationStartedEvent
//  3. Call Pipeline.Execute() (renders and validates)
//  4. If success: Publish TemplateRenderedEvent
//  5. If failure: Publish ReconciliationFailedEvent
//
// The DeploymentScheduler still operates event-driven, receiving
// TemplateRenderedEvent and the render gate's RenderGateCompletedEvent to
// schedule deployments.
type Coordinator struct {
	*component.ReadySignal

	eventBus      *busevents.EventBus
	eventChan     <-chan busevents.Event
	pipeline      PipelineExecutor
	storeProvider stores.StoreProvider
	currentFiles  CurrentFilesAuthority
	acceptance    HTTPContentAcceptance
	ledger        *acceptanceLedger
	graphWarm     func() bool
	metrics       *metrics.Metrics
	logger        *slog.Logger

	// lastStatusPatchSnapshot caches the most recent successful render's status
	// patches. StatusApplier (via events) applies failure variants (renderFailed,
	// deployFailed) against it when a subsequent pipeline execution fails.
	lastStatusPatchSnapshot *templating.StatusPatchSnapshot

	// criticalFailureReported is set once this term reported a failure on
	// unavailable critical content; held renders then stay quiet.
	criticalFailureReported bool
}

// CoordinatorConfig contains configuration for creating a Coordinator.
type CoordinatorConfig struct {
	// EventBus is the event bus for subscribing to events and publishing results.
	EventBus *busevents.EventBus

	// Pipeline is the render-validate pipeline to execute.
	// Must implement PipelineExecutor interface.
	Pipeline PipelineExecutor

	// StoreProvider provides access to resource stores.
	StoreProvider stores.StoreProvider

	// CurrentFiles owns the last accepted auxiliary output for each leader term.
	CurrentFiles CurrentFilesAuthority

	// HTTPAcceptance is the HTTP store's reversible content acceptance; nil
	// without an HTTP store.
	HTTPAcceptance HTTPContentAcceptance

	// GraphWarm reports whether the render service holds a committed graph;
	// it labels failed renders. Nil counts them as warm.
	GraphWarm func() bool

	// Metrics receives one render count per reconcile; optional.
	Metrics *metrics.Metrics

	// Logger is the structured logger.
	Logger *slog.Logger
}

// NewCoordinator creates a new ReconciliationCoordinator.
//
// Note: eventChan is NOT subscribed here - subscription happens in Start().
// This is a leader-only component that subscribes when Start() is called
// (after leadership is acquired). All-replica components replay their state
// on BecameLeaderEvent to ensure leader-only components receive current state.
//
// Parameters:
//   - cfg: Configuration for the coordinator
//
// Returns:
//   - A new Coordinator instance ready to be started
func NewCoordinator(cfg *CoordinatorConfig) *Coordinator {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Coordinator{
		ReadySignal:   component.NewReadySignal(),
		eventBus:      cfg.EventBus,
		pipeline:      cfg.Pipeline,
		storeProvider: cfg.StoreProvider,
		currentFiles:  cfg.CurrentFiles,
		acceptance:    cfg.HTTPAcceptance,
		ledger:        newAcceptanceLedger(),
		graphWarm:     cfg.GraphWarm,
		metrics:       cfg.Metrics,
		logger:        logger.With("component", CoordinatorComponentName),
	}
}

// Name returns the unique identifier for this component.
func (c *Coordinator) Name() string {
	return CoordinatorComponentName
}

// Start begins the coordinator's event loop.
//
// This method blocks until the context is cancelled.
//
// NOTE: deliberately NOT converted to embed component.Base. Base subscribes
// at construction, but the coordinator is a leader-only component whose
// input (ReconciliationTriggeredEvent) is published by the all-replica
// Reconciler on EVERY replica — a constructor-time subscription would have
// follower replicas fill the buffer and log critical drops continuously.
// Subscribing here, on leadership, keeps followers unsubscribed entirely.
func (c *Coordinator) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	var generation uint64
	if c.currentFiles != nil {
		generation = c.currentFiles.BeginTerm()
		defer c.currentFiles.EndTerm(generation)
	}
	c.ledger = newAcceptanceLedger()
	c.criticalFailureReported = false
	defer c.ledger.attempts.Wait()
	defer cancel()

	// Subscribe when starting (after leadership acquired).
	// Use SubscribeTypesLeaderOnly() to suppress late subscription warning.
	// All-replica components replay their cached state on BecameLeaderEvent.
	defer c.Rearm()
	c.eventChan = c.eventBus.SubscribeTypesLeaderOnly(
		CoordinatorComponentName,
		CoordinatorEventBufferSize,
		events.EventTypeReconciliationTriggered,
		events.EventTypeRenderGateCompleted,
		events.EventTypeHTTPContentAcceptanceRequested,
	)
	// Unsubscribe on loop exit: without this, every leadership
	// re-acquisition on the same instance would stack another subscription
	// whose orphaned channel fills up and logs critical drops forever.
	defer c.eventBus.UnsubscribeTyped(c.eventChan)
	processed := c.eventBus.SubscribeTypesLeaderOnly(
		CoordinatorComponentName+"-resources", busevents.LowVolumeSubscriberBuffer,
		events.EventTypeResourcesProcessed,
	)
	defer c.eventBus.UnsubscribeTyped(processed)
	mailbox := newCoordinatorMailbox(ctx, c.eventChan)
	defer mailbox.stop()

	// Signal that subscription is complete for SubscriptionReadySignaler interface.
	c.MarkReady()

	c.logger.Debug("Reconciliation coordinator starting")

	for {
		event, ok := mailbox.next(ctx)
		if !ok {
			c.logger.Info("Reconciliation coordinator shutting down", "reason", ctx.Err())
			return nil
		}

		switch e := event.(type) {
		case *events.ReconciliationTriggeredEvent:
			occurrence := c.handleReconciliationTriggered(ctx, e, generation)
			if err := waitForResourceApplication(ctx, processed, occurrence); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		case *events.RenderGateCompletedEvent:
			c.settleCurrentFiles(generation, e)
			c.settleAcceptances(e)
		case *events.HTTPContentAcceptanceRequestedEvent:
			if c.acceptance != nil {
				c.requestAcceptanceAttempt(ctx, generation)
			}
		}
	}
}

// settleCurrentFiles moves the term's auxiliary baseline with the render gate's
// verdict: a pass makes the render's files the baseline the next render reads
// back, a refusal restores the ones HAProxy last accepted. Verdicts for
// superseded plans judge a render the baseline has moved past.
func (c *Coordinator) settleCurrentFiles(generation uint64, event *events.RenderGateCompletedEvent) {
	if c.currentFiles == nil || !event.Newest {
		return
	}
	occurrence, err := event.RenderOccurrence()
	if err != nil {
		c.logger.Error("currentFiles could not authenticate render occurrence", "error", err)
		return
	}
	cycle, err := occurrence.Snapshot()
	if err != nil {
		c.logger.Error("currentFiles could not authenticate render occurrence", "error", err)
		return
	}
	outputSnapshot, err := cycle.OutputSnapshot()
	if err != nil {
		c.logger.Error("currentFiles could not read render output", "error", err)
		return
	}
	switch {
	case event.OK:
		err = c.currentFiles.ConfirmOutput(generation, outputSnapshot)
	case event.Refused:
		err = c.currentFiles.RollbackOutput(generation, outputSnapshot)
	}
	if err != nil {
		c.logger.Error("currentFiles could not settle render output", "error", err)
	}
}

// handleReconciliationTriggered orchestrates a reconciliation cycle.
func (c *Coordinator) handleReconciliationTriggered(ctx context.Context, event *events.ReconciliationTriggeredEvent, generation uint64) *rendercycle.Occurrence {
	if context.Cause(ctx) != nil {
		return nil
	}
	startTime := time.Now()
	correlationID := event.CorrelationID()

	c.logger.Debug("Reconciliation triggered",
		"reason", event.Reason,
		"correlation_id", correlationID)

	// Publish reconciliation started event, propagating the correlation ID so
	// downstream components (e.g. metrics) can correlate it with the trigger.
	c.eventBus.Publish(events.NewReconciliationStartedEvent(event.Reason, events.PropagateCorrelation(event)))

	renderOpts, err := c.renderOptions(generation)
	if err != nil {
		c.handlePipelineFailure(ctx, &pipeline.PipelineError{Phase: pipeline.PhaseRender, Cause: err}, event, startTime)
		return nil
	}
	acceptedBefore := c.acceptanceSequence()
	result, err := c.pipeline.Execute(pipeline.WithPendingContentWithheld(ctx), c.storeProvider,
		rendercontext.RenderModeReconcile, renderOpts...)
	if cause := context.Cause(ctx); cause != nil {
		c.logger.Debug("Discarding reconciliation result after authority expired",
			"cause", cause,
			"correlation_id", correlationID)
		return nil
	}
	if !pipeline.RenderHeld(err) {
		c.ledger.deployedFrom.Store(acceptedBefore)
	}
	if pipeline.RenderHeld(err) && c.criticalFailureReported {
		c.logger.Debug("Reconciliation skipped while rendering is paused", "error", err, "correlation_id", correlationID)
		return nil
	}
	c.criticalFailureReported = pipeline.CriticalContentUnavailable(err)
	if err != nil {
		c.recordRender(nil, err)
	}
	if pipeline.WaitsForCriticalContent(err) {
		if failure := c.ledger.criticalContentFailure(); failure != nil {
			c.handlePipelineFailure(ctx, failure, event, startTime)
			return nil
		}
		c.logger.Info("Deploy waits for critical http.Fetch content to be accepted", "error", err)
		return nil
	}
	if err != nil {
		c.handlePipelineFailure(ctx, err, event, startTime)
		return nil
	}
	if result == nil {
		c.handlePipelineFailure(ctx, &pipeline.PipelineError{
			Phase: pipeline.PhaseRender,
			Cause: errors.New("pipeline returned no result"),
		}, event, startTime)
		return nil
	}
	if err := c.acceptCurrentFiles(generation, result); err != nil {
		c.handlePipelineFailure(ctx, &pipeline.PipelineError{
			Phase: pipeline.PhaseRender,
			Cause: err,
		}, event, startTime)
		return nil
	}

	// Pipeline succeeded - publish events for downstream components
	return c.handlePipelineSuccess(ctx, result, event, startTime)
}

// renderOptions binds a render to the auxiliary files the fleet runs in this term.
func (c *Coordinator) renderOptions(generation uint64) ([]rendercontext.Option, error) {
	if c.currentFiles == nil {
		return nil, nil
	}
	if exact, ok := c.currentFiles.(exactCurrentFilesAuthority); ok {
		source, err := exact.ExactSource(generation)
		if err != nil {
			return nil, err
		}
		return []rendercontext.Option{rendercontext.WithCurrentAuxFilesSource(source)}, nil
	}
	currentFiles, err := c.currentFiles.Snapshot(generation)
	if err != nil {
		return nil, err
	}
	return []rendercontext.Option{rendercontext.WithCurrentAuxFiles(currentFiles)}, nil
}

func (c *Coordinator) acceptCurrentFiles(generation uint64, result *pipeline.PipelineResult) error {
	if result == nil || result.CycleSnapshot == nil {
		return errors.New("pipeline returned no authenticated render cycle")
	}
	outputSnapshot, err := result.CycleSnapshot.OutputSnapshot()
	if err != nil {
		return fmt.Errorf("reading render cycle output: %w", err)
	}
	if c.currentFiles == nil {
		return nil
	}
	if err := c.currentFiles.AcceptOutput(generation, outputSnapshot); err != nil {
		return fmt.Errorf("accepting render output: %w", err)
	}
	return nil
}

// handlePipelineSuccess publishes events for successful render+validate.
func (c *Coordinator) handlePipelineSuccess(
	ctx context.Context,
	result *pipeline.PipelineResult,
	triggerEvent *events.ReconciliationTriggeredEvent,
	startTime time.Time,
) *rendercycle.Occurrence {
	if context.Cause(ctx) != nil {
		return nil
	}
	if result.CycleSnapshot == nil {
		c.handlePipelineFailure(ctx, &pipeline.PipelineError{
			Phase: pipeline.PhaseRender,
			Cause: errors.New("pipeline returned no authenticated render cycle"),
		}, triggerEvent, startTime)
		return nil
	}
	coalescible := triggerEvent.Coalescible()

	statusSnapshot, err := result.CycleSnapshot.StatusPatchSnapshot()
	if err != nil {
		c.handlePipelineFailure(ctx, &pipeline.PipelineError{
			Phase: pipeline.PhaseRender,
			Cause: fmt.Errorf("reading render cycle status patches: %w", err),
		}, triggerEvent, startTime)
		return nil
	}
	c.lastStatusPatchSnapshot = statusSnapshot

	templateEvent, err := events.NewTemplateRenderedEventWithCycle(
		result.CycleSnapshot, result.RenderDurationMs, triggerEvent.Reason,
		coalescible, events.PropagateCorrelation(triggerEvent),
	)
	if err != nil {
		c.handlePipelineFailure(ctx, &pipeline.PipelineError{
			Phase: pipeline.PhaseRender,
			Cause: fmt.Errorf("building rendered cycle event: %w", err),
		}, triggerEvent, startTime)
		return nil
	}
	occurrence, err := templateEvent.RenderOccurrence()
	if err != nil {
		c.handlePipelineFailure(ctx, &pipeline.PipelineError{
			Phase: pipeline.PhaseRender,
			Cause: fmt.Errorf("reading rendered occurrence: %w", err),
		}, triggerEvent, startTime)
		return nil
	}

	totalDuration := time.Since(startTime).Milliseconds()
	completed, err := events.NewReconciliationCompletedEventWithCycle(
		totalDuration, occurrence, events.PropagateCorrelation(triggerEvent),
	)
	if err != nil {
		c.handlePipelineFailure(ctx, &pipeline.PipelineError{
			Phase: pipeline.PhaseRender,
			Cause: fmt.Errorf("building reconciliation cycle event: %w", err),
		}, triggerEvent, startTime)
		return nil
	}
	if context.Cause(ctx) != nil {
		return nil
	}
	c.ledger.record(occurrence, acceptanceWindow{
		reached: c.acceptanceSequence(), observations: result.HTTPObservations,
	})
	c.eventBus.Publish(templateEvent)

	// No validation event follows: TemplateRenderedEvent is the deploy trigger
	// now, and HAProxy's verdict arrives asynchronously from the render gate.
	for _, warning := range result.ValidationWarnings {
		c.logger.Warn("Rendered output validator warning",
			"warning", warning, "correlation_id", triggerEvent.CorrelationID())
	}
	c.recordRender(result, nil)
	c.eventBus.Publish(completed)

	c.logger.Debug("Reconciliation completed",
		"correlation_id", triggerEvent.CorrelationID(),
		"render_ms", result.RenderDurationMs,
		"validate_ms", result.ValidateDurationMs,
		"total_ms", totalDuration,
		"cache_state", result.CacheState,
		"cache_build_ms", result.CacheBuildMs)
	return occurrence
}

// recordRender counts a render that ran; a held reconcile rendered nothing.
func (c *Coordinator) recordRender(result *pipeline.PipelineResult, err error) {
	switch {
	case c.metrics == nil || pipeline.RenderHeld(err):
	case err != nil:
		c.metrics.RecordRenderFailure(c.graphWarm == nil || c.graphWarm())
	case result != nil:
		c.metrics.RecordRender(result.CacheState)
	}
}

// handlePipelineFailure publishes phase-specific failure events followed by ReconciliationFailedEvent.
//
// This ensures downstream components (e.g., StateCache) that subscribe to phase-specific
// events like ValidationFailedEvent or TemplateRenderFailedEvent receive proper status updates
// on failure, not just on success.
func (c *Coordinator) handlePipelineFailure(
	ctx context.Context,
	err error,
	triggerEvent *events.ReconciliationTriggeredEvent,
	startTime time.Time,
) {
	if context.Cause(ctx) != nil {
		return
	}
	correlationID := triggerEvent.CorrelationID()
	duration := time.Since(startTime).Milliseconds()

	c.logger.Error("Pipeline execution failed",
		"error", err,
		"correlation_id", correlationID,
		"duration_ms", duration)

	// Extract phase from structured PipelineError
	phase := "render" // Default to render for unexpected errors
	if pipelineErr, ok := errors.AsType[*pipeline.PipelineError](err); ok {
		phase = string(pipelineErr.Phase)
	}

	// Publish phase-specific failure event before the general ReconciliationFailedEvent.
	// This mirrors handlePipelineSuccess which publishes TemplateRenderedEvent
	// before ReconciliationCompletedEvent.
	switch phase {
	case string(pipeline.PhaseValidation):
		if context.Cause(ctx) != nil {
			return
		}
		c.eventBus.Publish(events.NewValidationFailedEvent(
			[]string{err.Error()},
			duration,
			triggerEvent.Reason,
			events.PropagateCorrelation(triggerEvent),
		))
	default:
		if context.Cause(ctx) != nil {
			return
		}
		c.eventBus.Publish(events.NewTemplateRenderFailedEvent(
			"", // No specific template name available from pipeline
			err.Error(),
			"", // No stack trace available from pipeline error
			events.PropagateCorrelation(triggerEvent),
		))
	}

	// Forward the last successful render's patches so StatusApplier can
	// apply the renderFailed / deployFailed variant. May be nil if no
	// successful render has happened yet (early bootstrap failure); the
	// applier skips the apply in that case.
	if context.Cause(ctx) != nil {
		return
	}
	c.eventBus.Publish(events.NewReconciliationFailedEventWithStatusSnapshot(
		err.Error(), phase, duration, c.lastStatusPatchSnapshot,
	))
}
