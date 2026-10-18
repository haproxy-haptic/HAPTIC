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

package deployer

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/coalesce"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercycle"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
)

const (
	// maxDeployFailureRetries bounds the fast self-reschedule of a retryable
	// deploy failure. Beyond this many consecutive fast retries for the SAME
	// occurrence, the scheduler stops the hot retry and hands off to the
	// 60s DriftPreventionMonitor backstop — so a permanently-wedged deploy (e.g. a
	// config HAProxy keeps rejecting) can't spin. A NEW render resets the budget.
	maxDeployFailureRetries = 5

	// maxFailureRetryBackoff caps the exponential fast-retry backoff. It matches
	// config.DefaultDriftPreventionInterval (60s): once a single retry would wait
	// as long as the drift backstop anyway, there's nothing to gain over letting
	// drift drive it.
	maxFailureRetryBackoff = 60 * time.Second
)

// handleTemplateRendered arms deployment for a completed render.
//
// Cold terms wait for their first accepted render (ADR-0033). Subsequent renders
// use the asynchronous gate unless a refusal has pinned it (ADR-0022).
func (s *DeploymentScheduler) handleTemplateRendered(ctx context.Context, event *events.TemplateRenderedEvent) {
	occurrence, err := templateEventOccurrence(event)
	if err != nil {
		s.logger.Error("Ignoring a render without exact deployment identity", "error", err)
		return
	}
	identity, err := inspectOccurrence(occurrence)
	if err != nil {
		s.logger.Error("Ignoring a render without exact deployment identity", "error", err)
		return
	}
	s.cancelRetainedRecovery()
	s.retainedRenderError = ""
	s.mu.Lock()
	s.lastRenderedOccurrence = occurrence
	pinned := s.gatePinned || s.awaitingFirstAcceptedRenderLocked()
	s.mu.Unlock()

	if pinned {
		s.logger.Warn("Render gate is holding renders; waiting for its verdict before deploying",
			"plan", identity.planID,
			"correlation_id", event.CorrelationID())
		return
	}

	s.dispatchRender(ctx, event.CorrelationID(), event.Coalescible(), deployReason(event.TriggerReason))
}

// handleRenderGateCompleted moves the deployment side of the gate's latch: a
// refusal holds every later render until a verdict passes, and that pass
// dispatches the render being held — but only when the verdict names the newest
// render, so a verdict for a superseded plan never dispatches an unchecked one.
func (s *DeploymentScheduler) handleRenderGateCompleted(ctx context.Context, event *events.RenderGateCompletedEvent) {
	occurrence, err := gateEventOccurrence(event)
	if err != nil {
		s.logger.Error("Ignoring a render gate verdict without exact deployment identity", "error", err)
		return
	}
	identity, err := inspectOccurrence(occurrence)
	if err != nil {
		s.logger.Error("Ignoring a render gate verdict without exact deployment identity", "error", err)
		return
	}
	// A verdict for a plan the fleet has moved past scopes the gate's revert;
	// it says nothing about the render this scheduler is converging on, so it
	// must not move the latch, the deployable render or a queued deployment.
	if !event.Newest {
		s.logger.Debug("Ignoring a render gate verdict for a superseded plan",
			"plan", identity.planID, "ok", event.OK)
		return
	}

	s.mu.Lock()
	wasPinned := s.gatePinned || s.awaitingFirstAcceptedRenderLocked()
	// Newest is the gate's snapshot; a newer render can reach us before its verdict.
	namesHeldRender := samePlan(s.lastRenderedOccurrence, occurrence)
	alreadyDispatched := samePlan(s.lastValidatedOccurrence, occurrence)
	if !namesHeldRender && !alreadyDispatched {
		s.mu.Unlock()
		s.logger.Debug("Ignoring a render gate verdict superseded in scheduler", "plan", identity.planID)
		return
	}
	if !event.OK {
		s.mu.Unlock()
		s.holdAfterRefusal(event, occurrence, identity.planID)
		s.renderFailedForRetention(ctx, event.Message)
		return
	}
	if s.retainedRenderError != "" && !alreadyDispatched {
		s.mu.Unlock()
		return
	}
	if alreadyDispatched {
		s.acceptRenderLocked()
	}
	if !wasPinned || namesHeldRender {
		s.gatePinned = false
	}
	s.mu.Unlock()

	if !wasPinned || !namesHeldRender || alreadyDispatched {
		return
	}

	s.logger.Info("Render gate passed the held render, deploying it", "plan", identity.planID)
	s.dispatchRender(ctx, event.CorrelationID(), false, "rendergate_release")

	// The released render is now what the fleet runs, so it is what a later
	// refusal must roll back to. Without this the rollback would reach past it
	// to the render accepted before the incident, dropping everything HAProxy
	// validated in between.
	s.mu.Lock()
	s.acceptRenderLocked()
	s.mu.Unlock()
}

// holdAfterRefusal closes the latch on a failed verdict for the newest render.
//
// Only HAProxy's own refusal moves the deployable render back: a check that
// could not run is not evidence about the config, and rolling back on it would
// undo a live, working render because a temp directory was unwritable — the
// same rule revert.go applies to the pods. Either way the gate holds: nothing
// has judged the render, so nothing new may be dispatched.
func (s *DeploymentScheduler) holdAfterRefusal(
	event *events.RenderGateCompletedEvent,
	occurrence *rendercycle.Occurrence,
	planID string,
) {
	s.mu.Lock()
	matched := samePlan(s.lastRenderedOccurrence, occurrence) ||
		samePlan(s.lastValidatedOccurrence, occurrence)
	if !matched {
		s.mu.Unlock()
		return
	}
	s.gatePinned = true
	restored := ""
	if event.Refused {
		restored = s.rollBackToAcceptedRenderLocked(occurrence)
	}
	s.mu.Unlock()

	dropped := s.dropPendingRender(occurrence)

	s.logger.Warn("Render gate refused a render; holding further renders",
		"plan", planID,
		"pinned", event.Pinned,
		"refused_by_haproxy", event.Refused,
		"rolled_back_to", restored,
		"dropped_pending", dropped,
		"error", event.Message)
}

// dropPendingPlan retires a deployment of the refused plan that is queued
// behind an in-flight one, reporting whether it dropped anything.
//
// The latch alone does not cover this: a render dispatched before the verdict
// may still be sitting in the pending slot when the refusal lands, and the
// deploy loop would publish it as soon as the current deployment finishes —
// after the scoped revert has already run and found no pod carrying it. Scoped
// to the named plan, because a refusal for one render must not cancel another.
func (s *DeploymentScheduler) dropPendingRender(
	occurrence *rendercycle.Occurrence,
) bool {
	if occurrence == nil {
		return false
	}
	s.schedulerMutex.Lock()
	defer s.schedulerMutex.Unlock()
	if s.state.pending == nil || !samePlan(s.state.pending.occurrence, occurrence) {
		return false
	}
	s.workRevision++
	s.state.pending = nil
	return true
}

// acceptRenderLocked snapshots the render the gate just passed. Caller holds mu.
//
// The three paths that re-send a render to pods rather than dispatch a new one
// — pod discovery, the validation fallback and the retry timers — all read the
// last validated occurrence. That holds whatever was dispatched last, which under
// the optimistic gate includes a render HAProxy has not judged yet, so this
// snapshot is what a refusal rolls them back to.
func (s *DeploymentScheduler) acceptRenderLocked() {
	s.acceptedRender = &acceptedRender{
		occurrence:    s.lastValidatedOccurrence,
		correlationID: s.lastCorrelationID,
	}
}

// rollBackToAcceptedRenderLocked undoes the optimistic promotion of a render
// the gate then refused, so every path that re-sends "the config the fleet
// runs" sends the last one HAProxy accepted — the same set the pods were just
// reverted to — instead of fighting that revert with the refused render.
// Caller holds mu. Returns the plan it rolled back to, for the log.
//
// A refusal naming a plan the scheduler has already superseded leaves the
// deployable render alone: the newer one is simply unjudged, and the gate
// checks it next. With nothing accepted yet — the term's first render was
// refused — there is nothing to roll back to, and the refused render stays the
// only config this controller has; HAProxy refuses it per pod at apply, which
// is visible as a NACK rather than a silent stall.
func (s *DeploymentScheduler) rollBackToAcceptedRenderLocked(
	refused *rendercycle.Occurrence,
) string {
	if s.acceptedRender == nil || !samePlan(s.lastValidatedOccurrence, refused) {
		return ""
	}
	if samePlan(s.acceptedRender.occurrence, refused) {
		return "" // the gate contradicting itself; keep what a pass established
	}
	s.lastValidatedOccurrence = s.acceptedRender.occurrence
	s.lastCorrelationID = s.acceptedRender.correlationID
	s.lastCoalescible = false
	identity, err := inspectOccurrence(s.acceptedRender.occurrence)
	if err != nil {
		return ""
	}
	return identity.planID
}

// handleConfigValidated handles ConfigValidatedEvent to cache template config metadata.
//
// This caches the template config name and namespace early in the pipeline, allowing
// runtimeConfigName to be computed deterministically without waiting for ConfigPublishedEvent.
// This fixes the race condition where deployment was scheduled before ConfigPublishedEvent arrived.
func (s *DeploymentScheduler) handleConfigValidated(event *events.ConfigValidatedEvent) {
	tc, ok := event.TemplateConfig.(*v1alpha1.HAProxyTemplateConfig)
	if !ok {
		s.logger.Debug("ConfigValidatedEvent.TemplateConfig is not HAProxyTemplateConfig, skipping")
		return
	}

	s.mu.Lock()
	s.templateConfigName = tc.Name
	s.templateConfigNamespace = tc.Namespace
	s.mu.Unlock()

	s.logger.Debug("Cached template config metadata for runtime config name computation",
		"template_config_name", tc.Name,
		"template_config_namespace", tc.Namespace)
}

type dispatchRenderState struct {
	occurrence *rendercycle.Occurrence
	identity   renderOccurrenceIdentity
	endpoints  []dataplane.Endpoint
}

// dispatchRender promotes the cached render to the deployable one and schedules
// it to the current endpoint set.
//
// Called from the render's own event while the gate is open, and from the gate's
// verdict while it is holding — the two paths differ only in what proved the
// render dispatchable, never in what is dispatched.
func (s *DeploymentScheduler) dispatchRender(ctx context.Context, correlationID string, coalescible bool, reason string) {
	render, ok := s.promoteRendered(correlationID, coalescible)
	if !ok {
		return
	}

	if len(render.endpoints) == 0 {
		s.logger.Debug("No endpoints available yet, config cached for later deployment")
		return
	}

	configHash := render.identity.checksum
	podSetHash := computePodSetHash(render.endpoints)
	canSkip, lastDeployedTime := s.renderAlreadyDeployed(&render, podSetHash, reason)

	if canSkip {
		s.logger.Debug("Skipping deployment - config unchanged since last deploy",
			"config_hash", configHash[:8],
			"pod_set_hash", podSetHash[:8],
			"last_deployed", lastDeployedTime.Format(time.RFC3339))
		s.publishUnchangedRender(
			render.occurrence, len(render.endpoints), podSetHash, correlationID,
		)
		return
	}

	// Schedule deployment to current endpoints (or queue if deployment in progress).
	// scheduleOrQueue classifies the render into a lane (runtime-raw vs structural)
	// against the last-dispatched config; the deploy loop applies it accordingly.
	//
	s.scheduleOrQueueOccurrence(ctx, render.occurrence, render.endpoints, reason, correlationID, coalescible)
}

func (s *DeploymentScheduler) promoteRendered(correlationID string, coalescible bool) (dispatchRenderState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	render := dispatchRenderState{
		occurrence: s.lastRenderedOccurrence,
		endpoints:  s.currentEndpoints,
	}
	identity, err := inspectOccurrence(render.occurrence)
	if err != nil {
		s.logger.Error("Rendered config has no exact deployment identity", "error", err)
		return dispatchRenderState{}, false
	}
	render.identity = identity

	s.lastValidatedOccurrence = render.occurrence
	s.lastCorrelationID = correlationID
	s.lastCoalescible = coalescible
	return render, true
}

func (s *DeploymentScheduler) renderAlreadyDeployed(
	render *dispatchRenderState,
	podSetHash, reason string,
) (bool, time.Time) {
	if reason == events.TriggerReasonDriftPrevention {
		return false, time.Time{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	sameAsDeployed := sameOccurrenceOutput(render.occurrence, s.lastDeployedOccurrence)
	dispatchedSettled := sameOccurrenceOutput(s.lastDispatchedOccurrence, s.lastDeployedOccurrence)
	canSkip := sameAsDeployed && dispatchedSettled &&
		podSetHash == s.lastDeployedPodSetHash && !s.lastDeployedTime.IsZero()
	return canSkip, s.lastDeployedTime
}

func (s *DeploymentScheduler) publishUnchangedRender(
	occurrence *rendercycle.Occurrence,
	total int,
	podSetHash, correlationID string,
) {
	correlation := events.WithCorrelation(correlationID, correlationID)
	skipped, err := events.NewDeploymentSkippedEventWithCycle(
		occurrence, total, events.SkipReasonConfigUnchanged, podSetHash, correlation,
	)
	if err != nil {
		s.logger.Error("Refusing to publish an unauthenticated deployment skip", "error", err)
		return
	}
	s.eventBus.Publish(skipped)
}

// deployReason names why the deploy runs. The drift pass must stay
// distinguishable all the way to the deployer: it verifies each pod's tree
// instead of trusting the digests the agent last recorded.
func deployReason(triggerReason string) string {
	if triggerReason == events.TriggerReasonDriftPrevention {
		return events.TriggerReasonDriftPrevention
	}
	return "config_validation"
}

// handlePodsDiscovered handles HAProxy pod discovery/changes with coalescing.
//
// This schedules deployment of the last validated configuration to the new set of endpoints.
// This is called when HAProxy pods are added/removed/updated without config changes.
//
// After processing the initial event, it drains the event channel for any additional
// coalescible HAProxyPodsDiscoveredEvents and processes only the latest one. This prevents
// queue buildup during high-frequency pod churn (scaling events, rolling updates).
func (s *DeploymentScheduler) handlePodsDiscovered(ctx context.Context, event *events.HAProxyPodsDiscoveredEvent) {
	s.performPodsDiscovered(ctx, event)

	// After processing completes, drain queued events: consecutive coalescible
	// pods-discovered events collapse to their latest; any other event type
	// flushes the held pods-discovered event first and is then handled in
	// arrival order, so neither side can starve the other.
	coalesce.DrainLatest(
		s.eventChan,
		func(e busevents.Event) { s.handleEvent(ctx, e) },
		func(latest *events.HAProxyPodsDiscoveredEvent, supersededCount int) {
			if supersededCount > 0 {
				s.logger.Debug("Coalesced HAProxy pods discovered events",
					"superseded_count", supersededCount)
			}
			s.performPodsDiscovered(ctx, latest)
		},
	)
}

// performPodsDiscovered executes the actual pod discovery handling logic.
func (s *DeploymentScheduler) performPodsDiscovered(ctx context.Context, event *events.HAProxyPodsDiscoveredEvent) {
	// An endpoint-authority change retires the in-flight deploy: its pods are
	// not the fleet any more, and the replacement set must be deployed to as a
	// whole.
	var cancelledDeploymentID, cancelledCorrelationID string
	podSetHash := computePodSetHash(event.Endpoints)
	s.schedulerMutex.Lock()
	if s.lastPodSetHash != "" && s.lastPodSetHash != podSetHash {
		if s.state.deployInFlight {
			cancelledDeploymentID = s.state.activeDeploymentID
			cancelledCorrelationID = s.state.activeCorrelationID
		}
		s.workRevision++
		s.state.pending = nil
	}
	s.schedulerMutex.Unlock()
	s.publishFleetCapabilities(event.Endpoints)
	if cancelledDeploymentID != "" {
		s.eventBus.Publish(events.NewDeploymentCancelRequestEvent(
			cancelledDeploymentID,
			events.DeploymentCancelReasonEndpointAuthorityChanged,
			events.WithCorrelation(cancelledCorrelationID, cancelledDeploymentID),
		))
	}

	s.mu.Lock()
	s.currentEndpoints = event.Endpoints
	endpointCount := len(event.Endpoints)
	occurrence := s.lastValidatedOccurrence
	correlationID := s.lastCorrelationID
	coalescible := s.lastCoalescible
	s.mu.Unlock()

	s.logger.Debug("HAProxy pods discovered",
		"count", endpointCount)

	if occurrence == nil {
		s.retainedRetryAt = time.Time{}
		s.maybeRecoverRetained(ctx)
		s.logger.Debug("No validated config available yet, skipping deployment")
		return
	}

	if endpointCount == 0 {
		s.logger.Debug("No endpoints available, skipping deployment")
		return
	}

	// Schedule the exact last validated occurrence to the new endpoints.
	s.scheduleOrQueueOccurrence(
		ctx, occurrence, event.Endpoints, "pod_discovery", correlationID, coalescible,
	)
}

// handleValidationFailed handles validation failure events.
//
// When validation fails for any reason, we deploy the cached last known good config
// as a fallback. This ensures HAProxy pods stay in sync with a valid configuration
// even when the latest config is invalid (e.g., due to template syntax errors,
// HTTP fetch failures, or invalid HAProxy configuration).
//
// This is critical for resilience: the controller must NOT accept a broken config
// and must continue using the last known good config until a valid one is provided.
func (s *DeploymentScheduler) handleValidationFailed(ctx context.Context, event *events.ValidationFailedEvent) {
	correlationID := event.CorrelationID()

	s.mu.RLock()
	occurrence := s.lastValidatedOccurrence
	endpoints := s.currentEndpoints
	s.mu.RUnlock()

	s.logger.Warn("Validation failed, deploying cached config as fallback",
		"trigger_reason", event.TriggerReason,
		"errors", event.Errors,
		"correlation_id", correlationID)

	if occurrence == nil {
		s.logger.Error("Validation fallback failed: no cached config available",
			"correlation_id", correlationID)
		return
	}

	if len(endpoints) == 0 {
		s.logger.Debug("Validation fallback skipped: no endpoints available",
			"correlation_id", correlationID)
		return
	}

	// Fallback deployments are not coalescible: they must restore the exact
	// last validated occurrence.
	s.scheduleOrQueueOccurrence(ctx, occurrence, endpoints, "validation_fallback", correlationID, false)
}

// handleDeploymentCompleted handles deployment completion events.
//
// This marks the deployment as complete, updates the deployment end time, and
// caches the deployed config hash for optimization. It does NOT re-schedule —
// it only clears deployInFlight and signals the loop, which picks up any pending
// deployment on its next cycle.
//
// The completion must carry the exact occurrence that owns the active slot;
// mutable latest-render state and public identity shadows are never consulted.
func (s *DeploymentScheduler) handleDeploymentCompleted(event *events.DeploymentCompletedEvent) {
	if event == nil {
		s.logger.Warn("Ignoring nil deployment completion")
		return
	}
	occurrence, err := completedEventOccurrence(event)
	if err != nil {
		s.logger.Warn("Ignoring completion without exact deployment identity", "error", err)
		return
	}
	if _, err := inspectOccurrence(occurrence); err != nil {
		s.logger.Warn("Ignoring completion without exact deployment identity", "error", err)
		return
	}
	retired := s.retireActiveDeployment(event, occurrence)
	if !retired.matched {
		s.logger.Warn("Ignoring completion for a deployment that is not active",
			"completed_deployment_id", event.DeploymentID,
			"active_deployment_id", retired.activeDeploymentID,
			"correlation_id", event.CorrelationID())
		return
	}
	if retired.timedOut {
		s.logger.Info("Timed-out deployment terminated",
			"deployment_id", event.DeploymentID,
			"correlation_id", event.CorrelationID())
		s.signalCompleted()
		return
	}

	s.cacheDeploymentCompletion(event, retired.occurrence)
	s.mu.RLock()
	currentValidated := sameOccurrence(retired.occurrence, s.lastValidatedOccurrence)
	s.mu.RUnlock()
	if currentValidated && s.retainedRenderError == "" && event.Total > 0 && event.Failed == 0 && event.PendingReloads == 0 {
		s.setRetainedActive(false)
	}

	// Fast self-reschedule: a retryable per-pod failure (e.g. a transient DPA
	// transaction-version conflict) is otherwise only re-driven by the 60s drift
	// backstop, so the first retry can wait a full minute — a Gateway can sit
	// Programmed!=True that whole time. Re-drive it promptly through the SINGLE
	// coalescing path with bounded backoff. A fully-successful deploy cancels any
	// armed retry (the config converged). Both branches gate on Total>0 so the
	// zero-endpoint "nothing deployed" completion is a no-op.
	switch {
	case event.Total > 0 && event.Failed > 0:
		s.scheduleFailureRetry(event)
	case event.Total > 0 && event.PendingReloads > 0:
		s.schedulePendingReloadFollowUp(event)
	case event.Total > 0 && event.Failed == 0:
		s.cancelFailureRetry()
	}

	s.signalCompleted()
}

type retiredDeployment struct {
	matched            bool
	timedOut           bool
	activeDeploymentID string
	occurrence         *rendercycle.Occurrence
}

func (s *DeploymentScheduler) retireActiveDeployment(
	event *events.DeploymentCompletedEvent,
	occurrence *rendercycle.Occurrence,
) retiredDeployment {
	s.schedulerMutex.Lock()
	defer s.schedulerMutex.Unlock()

	retired := retiredDeployment{activeDeploymentID: s.state.activeDeploymentID}
	if !s.state.deployInFlight || event.DeploymentID == "" ||
		event.DeploymentID != s.state.activeDeploymentID ||
		!sameOccurrence(s.state.activeOccurrence, occurrence) {
		return retired
	}

	retired.matched = true
	retired.timedOut = s.state.deploymentTimedOut
	retired.occurrence = s.state.activeOccurrence
	s.state.deployInFlight = false
	s.state.deploymentTimedOut = false
	s.state.deploymentStartTime = time.Time{}
	s.state.activeDeploymentID = ""
	s.state.activeCorrelationID = ""
	s.state.activeOccurrence = nil
	return retired
}

func (s *DeploymentScheduler) cacheDeploymentCompletion(
	event *events.DeploymentCompletedEvent,
	occurrence *rendercycle.Occurrence,
) {
	if event.PodSetHash == "" || occurrence == nil || occurrence.ValidateAuthentication() != nil {
		return
	}
	fullyDeployed := event.Failed == 0 && event.PendingReloads == 0
	if fullyDeployed {
		s.clearPendingReloadLogState()
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.lastDispatchedOccurrence = occurrence
	if fullyDeployed {
		s.lastDeployedOccurrence = occurrence
		s.lastDeployedPodSetHash = event.PodSetHash
		s.lastDeployedTime = time.Now()
	}
}

// pendingReloadLogKey is one pending-reload wait state; a repeat logs at Debug.
type pendingReloadLogKey struct {
	checksum    string
	pendingPods int
}

func (s *DeploymentScheduler) clearPendingReloadLogState() {
	s.schedulerMutex.Lock()
	defer s.schedulerMutex.Unlock()
	s.lastPendingReloadLog = pendingReloadLogKey{}
}

// pendingReloadFollowUpMargin is added to the agent's scheduled_at so the
// follow-up finds the reload done, not about to run. Renders that arrive while
// reloads are pending are dispatched at once: the pods coalesce their files
// into the pending reload and run the in-place subset — an endpoint change
// must never wait for a reload window.
const (
	pendingReloadFollowUpMargin = 250 * time.Millisecond
	pendingReloadFollowUpReason = "pending_reload_follow_up"
)

// schedulePendingReloadFollowUp re-drives the last validated render once the
// pods' paced reloads have fired. The agent never cancels a scheduled reload
// and never calls back; the controller polls at the scheduled time (plan
// §0.c). The re-diff is a noop for every pod that has reloaded and
// `scheduled` again for one that has not, so the chain ends by itself. It
// rides the single retry timer outside the failure budget: waiting for a
// reload window is not a failure.
func (s *DeploymentScheduler) schedulePendingReloadFollowUp(event *events.DeploymentCompletedEvent) {
	s.schedulerMutex.Lock()
	defer s.schedulerMutex.Unlock()
	if s.retryStopped {
		return
	}
	wait := pendingReloadFollowUpMargin
	if !event.PendingReloadUntil.IsZero() {
		wait = time.Until(event.PendingReloadUntil) + pendingReloadFollowUpMargin
	}
	if wait < pendingReloadFollowUpMargin {
		wait = pendingReloadFollowUpMargin
	}
	if wait > maxFailureRetryBackoff {
		wait = maxFailureRetryBackoff
	}
	s.stopRetryTimerLocked()
	s.retryGeneration++
	generation := s.retryGeneration
	workRevision := s.workRevision
	s.retryCallbacks.Add(1)
	var doneOnce sync.Once
	done := func() {
		doneOnce.Do(s.retryCallbacks.Done)
	}
	s.retryTimerDone = done
	s.retryTimer = time.AfterFunc(wait, func() {
		defer done()
		s.runRetry(generation, workRevision, pendingReloadFollowUpReason)
	})
	s.followUpArmed = true

	// Renders keep dispatching while a reload is pending, and every completion
	// re-arms this timer — dozens of identical lines per second on a
	// churn-heavy fleet. One Info per distinct wait state, repeats at Debug.
	logKey := pendingReloadLogKey{checksum: completedContentChecksum(event), pendingPods: event.PendingReloads}
	level := slog.LevelInfo
	if logKey == s.lastPendingReloadLog {
		level = slog.LevelDebug
	}
	s.lastPendingReloadLog = logKey
	s.logger.Log(context.Background(), level, "Reloads pending on the fleet; following up when they fire",
		"pending_pods", event.PendingReloads,
		"wait_ms", wait.Milliseconds(),
		"checksum", logKey.checksum)
}

// scheduleFailureRetry arms (or re-arms) the single fast-retry timer after a
// deploy completed with failures. It re-dispatches the last-validated render
// through the existing scheduleOrQueue path after a bounded exponential backoff,
// so a transiently-failed deploy self-heals in seconds instead of waiting up to a
// full DriftPreventionInterval for the 60s backstop.
//
// The ONLY async primitive is one time.AfterFunc timer: its callback
// (rescheduleLastValidated) writes the single state.pending slot, so all timing
// stays in the one runDeployLoop and no second scheduling path (the reload-storm
// regression the handleDeploymentCompleted comment warns about) is created.
//
// A new occurrence earns a fresh budget; the same failing occurrence gets at
// most maxDeployFailureRetries fast retries, after which
// the 60s drift backstop takes over — no hot loop on a permanently-wedged deploy.
func (s *DeploymentScheduler) scheduleFailureRetry(event *events.DeploymentCompletedEvent) {
	s.schedulerMutex.Lock()
	defer s.schedulerMutex.Unlock()
	if s.retryStopped {
		return
	}
	occurrence, err := completedEventOccurrence(event)
	if err != nil {
		s.logger.Warn("Deploy failed without exact retry identity; drift backstop continues")
		return
	}
	identity, err := inspectOccurrence(occurrence)
	if err != nil {
		s.logger.Warn("Deploy failed without exact retry identity; drift backstop continues")
		return
	}

	sameFailedRender := sameOccurrence(occurrence, s.lastFailedRetry)
	if !sameFailedRender {
		// A different render is failing now — start a fresh budget for it.
		s.lastFailedRetry = occurrence
		s.deployFailureRetries = 0
	}

	if s.deployFailureRetries >= maxDeployFailureRetries {
		s.logger.Warn("Deploy fast-retry budget exhausted; 60s drift backstop continues",
			"attempts", s.deployFailureRetries,
			"checksum", identity.checksum)
		return
	}

	s.deployFailureRetries++
	backoff := s.failureRetryBackoff(s.deployFailureRetries)
	s.stopRetryTimerLocked()
	s.retryGeneration++
	generation := s.retryGeneration
	workRevision := s.workRevision
	s.retryCallbacks.Add(1)
	var doneOnce sync.Once
	done := func() {
		doneOnce.Do(s.retryCallbacks.Done)
	}
	s.retryTimerDone = done
	s.retryTimer = time.AfterFunc(backoff, func() {
		defer done()
		s.runFailureRetry(generation, workRevision)
	})

	s.logger.Info("Deploy failed; scheduling fast retry",
		"attempt", s.deployFailureRetries,
		"backoff_ms", backoff.Milliseconds(),
		"checksum", identity.checksum)
}

func (s *DeploymentScheduler) runFailureRetry(generation, workRevision uint64) {
	s.runRetry(generation, workRevision, "deploy_failure_retry")
}

// runRetry re-dispatches the last validated render under a reason, if the
// timer that fired is still the armed one and the term did not move on.
func (s *DeploymentScheduler) runRetry(generation, workRevision uint64, reason string) {
	defer s.settleFollowUp()
	s.schedulerMutex.Lock()
	if generation != s.retryGeneration {
		s.schedulerMutex.Unlock()
		return
	}
	s.retryTimer = nil
	s.retryTimerDone = nil
	if s.retryStopped || workRevision != s.workRevision {
		s.schedulerMutex.Unlock()
		return
	}
	s.schedulerMutex.Unlock()

	s.rescheduleLastValidated(generation, workRevision, reason)
}

func (s *DeploymentScheduler) settleFollowUp() {
	s.schedulerMutex.Lock()
	defer s.schedulerMutex.Unlock()
	if s.retryTimer == nil {
		s.followUpArmed = false
	}
	s.signalDrainProgressLocked()
}

func (s *DeploymentScheduler) stopRetryTimerLocked() {
	s.followUpArmed = false
	s.retryGeneration++
	if s.retryTimer == nil {
		return
	}
	if s.retryTimer.Stop() && s.retryTimerDone != nil {
		s.retryTimerDone()
	}
	s.retryTimer = nil
	s.retryTimerDone = nil
}

func (s *DeploymentScheduler) stopFailureRetries() {
	s.schedulerMutex.Lock()
	s.retryStopped = true
	s.stopRetryTimerLocked()
	if s.state.pending != nil && s.state.pending.retryGeneration != 0 {
		s.state.pending = nil
	}
	s.deployFailureRetries = 0
	s.lastFailedRetry = nil
	s.schedulerMutex.Unlock()

	s.retryCallbacks.Wait()
}

// failureRetryBackoff returns the fast-retry backoff for the given 1-based
// attempt: an exponential base<<(attempt-1) doubling, clamped to
// maxFailureRetryBackoff. The base is minDeploymentInterval (default 2s), so the
// sequence is 2s, 4s, 8s, 16s, 32s. The clamp also catches any shift overflow
// (backoff<=0) defensively.
func (s *DeploymentScheduler) failureRetryBackoff(attempt int) time.Duration {
	base := s.minDeploymentInterval
	if base <= 0 {
		base = 2 * time.Second
	}
	backoff := base << (attempt - 1)
	if backoff <= 0 || backoff > maxFailureRetryBackoff {
		backoff = maxFailureRetryBackoff
	}
	return backoff
}

// rescheduleLastValidated snapshots the last validated render, then installs it
// only if the work and retry revisions captured when its timer was armed remain
// current. It never starts a second deploy path.
func (s *DeploymentScheduler) rescheduleLastValidated(generation, workRevision uint64, reason string) {
	s.schedulerMutex.Lock()
	newerPending := s.state.pending != nil && s.state.pending.retryGeneration == 0
	draining, drained := s.draining, s.drainOccurrence
	s.schedulerMutex.Unlock()
	if newerPending {
		// A newer render is already waiting; dispatching it covers this one.
		return
	}
	// If leadership was lost (or we're shutting down) between the timer arming and
	// firing, s.ctx is cancelled and the deploy loop has exited — don't repopulate
	// state.pending for a term that's already over. handleLostLeadership stops the
	// timer, but Stop() can't cancel a callback already running, so this closes
	// that last window. (s.ctx is nil only in direct-call unit tests that never
	// call Start; there the hasValidConfig guard below no-ops safely.)
	if s.ctx != nil && s.ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	occurrence := s.lastValidatedOccurrence
	correlationID := s.lastCorrelationID
	endpoints := s.currentEndpoints
	s.mu.Unlock()
	if draining {
		occurrence = drained
	}

	if len(endpoints) == 0 || occurrence == nil || occurrence.ValidateAuthentication() != nil {
		s.logger.Debug("Fast retry skipped: no validated config or no endpoints")
		return
	}

	dep := &scheduledDeployment{
		workRevision:    workRevision,
		retryGeneration: generation,
		occurrence:      occurrence,
		endpoints:       endpoints,
		reason:          reason,
		correlationID:   correlationID,
	}
	s.installPending(s.ctx, dep)
}

// cancelFailureRetry stops any armed fast-retry timer and resets the budget.
// Called when a deploy completes with zero failures (the config converged) so a
// stale retry from an earlier failure can't fire against an already-good state.
func (s *DeploymentScheduler) cancelFailureRetry() {
	s.schedulerMutex.Lock()
	defer s.schedulerMutex.Unlock()
	s.stopRetryTimerLocked()
	if s.state.pending != nil && s.state.pending.retryGeneration != 0 {
		s.state.pending = nil
	}
	s.deployFailureRetries = 0
	s.lastFailedRetry = nil
}

// handleConfigPublished handles ConfigPublishedEvent by caching runtime config metadata.
//
// This caches the runtime config name and namespace for use when publishing
// ConfigAppliedToPodEvent after successful deployments.
func (s *DeploymentScheduler) handleConfigPublished(event *events.ConfigPublishedEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.runtimeConfigName = event.RuntimeConfigName
	s.runtimeConfigNamespace = event.RuntimeConfigNamespace

	s.logger.Debug("Cached runtime config metadata for deployment events",
		"runtime_config_name", event.RuntimeConfigName,
		"runtime_config_namespace", event.RuntimeConfigNamespace)
}

// handleLostLeadership handles LostLeadershipEvent by clearing deployment state.
//
// When a replica loses leadership, leader-only components (including this scheduler)
// are stopped via context cancellation. However, we defensively clear state to prevent
// potential deadlocks if there's a race condition during shutdown.
//
// This prevents scenarios where:
//   - deployInFlight is stuck true, blocking future deployments
//   - pending contains stale deployments that shouldn't execute
func (s *DeploymentScheduler) handleLostLeadership(_ *events.LostLeadershipEvent) {
	s.resetDeploymentTerm()
}

func (s *DeploymentScheduler) resetDeploymentTerm() {
	s.schedulerMutex.Lock()
	defer s.schedulerMutex.Unlock()

	if s.state.deployInFlight || s.state.pending != nil {
		s.logger.Info("Lost leadership, clearing deployment state",
			"deploy_in_flight", s.state.deployInFlight,
			"has_pending", s.state.pending != nil)
	}

	// Clear all transient deploy state. The deploy loop itself exits via ctx
	// cancellation on leadership loss; its channels are recreated on next Start.
	s.workRevision++
	s.state.deployInFlight = false
	s.state.deploymentTimedOut = false
	s.state.deploymentStartTime = time.Time{}
	s.state.activeDeploymentID = ""
	s.state.activeCorrelationID = ""
	s.state.activeOccurrence = nil
	s.state.lastScheduled = nil
	s.state.pending = nil

	s.retryStopped = true
	s.stopRetryTimerLocked()
	s.deployFailureRetries = 0
	s.lastFailedRetry = nil

	s.lastPodSetHash = ""

	// No in-memory render proves what the fleet ran during another leader's term.
	s.mu.Lock()
	s.lastRenderedOccurrence = nil
	s.lastValidatedOccurrence = nil
	s.lastCorrelationID = ""
	s.lastCoalescible = false
	s.currentEndpoints = nil
	s.lastDeployedOccurrence = nil
	s.lastDispatchedOccurrence = nil
	s.lastDeployedPodSetHash = ""
	s.lastDeployedTime = time.Time{}
	s.gatePinned = false
	s.acceptedRender = nil
	s.mu.Unlock()
}
