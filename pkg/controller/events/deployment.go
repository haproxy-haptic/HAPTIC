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

package events

import (
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercycle"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderoutput"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

// DeploymentStartedEvent is published when deployment to HAProxy instances begins.
//
// This event propagates the correlation ID from DeploymentScheduledEvent.
type DeploymentStartedEvent struct {
	// EndpointCount is the number of HAProxy instances this deploy targets.
	// Only the count is carried: subscribers (statecache, commentator) never
	// read more than len(), and carrying the full slice forced a defensive
	// deep-copy of every endpoint's address and credentials on each publish.
	EndpointCount int
	timestamped

	// Correlation embeds correlation tracking for event tracing.
	Correlation
}

// NewDeploymentStartedEvent creates a new DeploymentStartedEvent.
//
// Use PropagateCorrelation() to propagate correlation from the triggering event:
//
//	event := events.NewDeploymentStartedEvent(len(endpoints),
//	    events.PropagateCorrelation(scheduledEvent))
func NewDeploymentStartedEvent(endpointCount int, opts ...CorrelationOption) *DeploymentStartedEvent {
	return &DeploymentStartedEvent{
		EndpointCount: endpointCount,
		timestamped:   newTimestamped(),
		Correlation:   newCorrelation(opts...),
	}
}

func (e *DeploymentStartedEvent) EventType() string { return EventTypeDeploymentStarted }

// InstanceDeploymentFailedEvent is published when deployment to a single HAProxy instance fails.
//
// This event propagates the correlation ID from DeploymentStartedEvent.
type InstanceDeploymentFailedEvent struct {
	Endpoint  any
	Error     string
	Retryable bool // Whether this failure is retryable
	timestamped

	// Correlation embeds correlation tracking for event tracing.
	Correlation
}

// NewInstanceDeploymentFailedEvent creates a new InstanceDeploymentFailedEvent.
//
// Use PropagateCorrelation() to propagate correlation from the triggering event:
//
//	event := events.NewInstanceDeploymentFailedEvent(endpoint, err, retryable,
//	    events.PropagateCorrelation(startedEvent))
func NewInstanceDeploymentFailedEvent(endpoint any, err string, retryable bool, opts ...CorrelationOption) *InstanceDeploymentFailedEvent {
	return &InstanceDeploymentFailedEvent{
		Endpoint:    endpoint,
		Error:       err,
		Retryable:   retryable,
		timestamped: newTimestamped(),
		Correlation: newCorrelation(opts...),
	}
}

func (e *InstanceDeploymentFailedEvent) EventType() string { return EventTypeInstanceDeploymentFailed }

// DeploymentCompletedEvent is published when deployment to all HAProxy instances completes.
//
// This event propagates the correlation ID from DeploymentStartedEvent.
type DeploymentCompletedEvent struct {
	renderOccurrenceCarrier

	// CycleSnapshot is the exact render cycle this deployment applied.
	CycleSnapshot *rendercycle.Snapshot

	// OutputSnapshot is the exact render this deployment applied.
	OutputSnapshot *renderoutput.Snapshot

	// DeploymentID identifies the exact DeploymentScheduledEvent that finished.
	DeploymentID string

	Total              int // Total number of instances
	Succeeded          int // Number of successful deployments
	Failed             int // Number of failed deployments
	PendingReloads     int // Pods holding the render behind a paced reload
	PendingReloadUntil time.Time
	DurationMs         int64 // Total deployment duration in milliseconds
	ReloadsTriggered   int   // Count of instances that triggered HAProxy reload
	TotalAPIOperations int   // Sum of API operations across all instances
	// Phases is the slowest accepted pod's split of DurationMs; nil when no
	// pod accepted.
	Phases *DeployPhases
	timestamped

	// OperationBreakdown provides a generic breakdown of operations performed.
	// Keys are formatted as "section_type" (e.g., "backend_create", "server_update", "global_update").
	// Values are the count of operations of that type.
	// Aggregated across all successfully deployed instances.
	OperationBreakdown map[string]int

	// BackendDiffFields summarizes which BackendBase fields caused backend updates.
	// Empty when no backend attribute diffs were detected.
	// Example: "[GUID] (48 backends)" or "[Mode, Balance] (3 backends)"
	BackendDiffFields string

	// StatusPatches are the chart-rendered status patches that correspond to
	// the configuration this deployment carried. The StatusApplier reads them
	// from this event and applies the "deployed" variant — guaranteeing that
	// the status conditions it writes describe the config the data plane is
	// actually serving (no side-channel cache, no LATEST-vs-deployed race).
	//
	// Threaded through unchanged from the DeploymentScheduledEvent that
	// triggered this deployment.
	StatusPatches []templating.StatusPatch

	// StatusPatchSnapshot carries the immutable patch set of this deployment.
	StatusPatchSnapshot *templating.StatusPatchSnapshot

	// ContentChecksum is the checksum of the config + auxiliary files THIS
	// deployment actually pushed to the data plane. Threaded through
	// unchanged from the DeploymentScheduledEvent so the DeploymentScheduler
	// can update its lastDeployedConfigHash from a value that's tied to the
	// completing deployment, not from the latest render (which a parallel
	// reconcile may have overwritten while this deployment was in flight).
	//
	// Without this, the scheduler reads s.lastContentChecksum at completion
	// time and mis-records the in-flight render's checksum as "what was just
	// deployed" — silently making future deployments with the same hash
	// hit the unchanged-skip branch and never reach HAProxy. Symptom in CI:
	// a freshly-added Ingress's redirect/auth directive never appears in
	// the live haproxy.cfg even though the controller did render it.
	//
	// Empty string for the zero-endpoint code path (nothing was deployed).
	ContentChecksum string
	RenderProof     string
	Plan            *renderplan.Plan

	// PodSetHash identifies the endpoint authorities THIS deployment targeted.
	// It is captured from DeploymentScheduledEvent.Endpoints before execution.
	PodSetHash string

	// Correlation embeds correlation tracking for event tracing.
	Correlation
}

// DeployPhases is where the slowest pod's apply spent its time. Pods apply in
// parallel, so this pod's split is what the deployment's duration is made of.
type DeployPhases struct {
	Pod         string
	PrepareMs   int64 // the deployment before any pod was contacted
	SettleMs    int64 // the deployment after the last pod answered
	TotalMs     int64 // the whole apply, state read to result
	StateMs     int64 // reading the agent's state
	DiffMs      int64 // composing the decision
	SendMs      int64 // the apply round trips, upload included
	BlobWaitMs  int64 // waiting for the plan blob's encode before the apply that carries it
	UploadBytes int64 // file content sent, which the agent had not proved
	Agent       api.ApplyTiming
}

// DeploymentResult contains the outcome of a deployment operation.
// Used with NewDeploymentCompletedEvent for cleaner parameter passing.
type DeploymentResult struct {
	renderOccurrenceCarrier

	// CycleSnapshot is the exact render cycle this deployment applied.
	CycleSnapshot *rendercycle.Snapshot

	// OutputSnapshot is the exact render this deployment applied.
	OutputSnapshot *renderoutput.Snapshot

	// DeploymentID is the EventID of the DeploymentScheduledEvent being completed.
	DeploymentID string

	Total              int   // Total number of instances
	Succeeded          int   // Number of successful deployments
	Failed             int   // Number of failed deployments
	DurationMs         int64 // Total deployment duration in milliseconds
	ReloadsTriggered   int   // Count of instances that triggered HAProxy reload
	TotalAPIOperations int   // Sum of API operations across all instances
	// PendingReloads are pods that accepted the render but hold it behind a
	// paced reload; PendingReloadUntil is when the latest of them fires.
	PendingReloads     int
	PendingReloadUntil time.Time

	// OperationBreakdown provides a generic breakdown of operations performed.
	// Keys are formatted as "section_type" (e.g., "backend_create", "server_update", "global_update").
	// Values are the count of operations of that type.
	OperationBreakdown map[string]int

	// BackendDiffFields summarizes which BackendBase fields caused backend updates.
	// Empty when no backend attribute diffs were detected.
	BackendDiffFields string

	// StatusPatches are the chart-rendered status patches for the
	// configuration this deployment carried. Forwarded from the
	// DeploymentScheduledEvent and surfaced on DeploymentCompletedEvent for
	// the StatusApplier to consume.
	StatusPatches []templating.StatusPatch

	// StatusPatchSnapshot carries the immutable patch set of this deployment.
	StatusPatchSnapshot *templating.StatusPatchSnapshot

	// ContentChecksum is the checksum of the config + auxiliary files
	// THIS deployment pushed (forwarded from DeploymentScheduledEvent).
	// See DeploymentCompletedEvent.ContentChecksum for the full rationale.
	// Empty when no deployment occurred (zero-endpoint path).
	ContentChecksum string
	RenderProof     string
	Plan            *renderplan.Plan

	// PodSetHash identifies the endpoint authorities THIS deployment targeted.
	// Empty when no deployment occurred (zero-endpoint path).
	PodSetHash string

	// Phases is the slowest accepted pod's split; nil when no pod accepted.
	Phases *DeployPhases
}

// NewDeploymentResultWithOccurrence creates a production result identity.
func NewDeploymentResultWithOccurrence(occurrence *rendercycle.Occurrence) (*DeploymentResult, error) {
	carrier, identity, err := inspectRenderOccurrence(occurrence)
	if err != nil {
		return nil, fmt.Errorf("deployment result: %w", err)
	}
	result := withDeploymentResultIdentity(&DeploymentResult{renderOccurrenceCarrier: carrier}, identity)
	return &result, nil
}

// NewDeploymentCompletedEvent creates a new DeploymentCompletedEvent.
//
// `result` is taken by pointer because DeploymentResult is large enough
// (≥96 bytes) that gocritic flags pass-by-value as `hugeParam`.
//
// `result.StatusPatches` should be forwarded unchanged from the
// DeploymentScheduledEvent that triggered the deployment so the
// StatusApplier reads the patches that correspond exactly to the
// configuration that just shipped (the chart's "deployed" variant).
//
// Use PropagateCorrelation() to propagate correlation from the triggering event:
//
//	event := events.NewDeploymentCompletedEvent(&events.DeploymentResult{
//	    Total:              len(endpoints),
//	    Succeeded:          successCount,
//	    Failed:             failureCount,
//	    DurationMs:         totalDurationMs,
//	    ReloadsTriggered:   reloads,
//	    TotalAPIOperations: ops,
//	    OperationBreakdown: breakdown,
//	    StatusPatches:      scheduledEvent.StatusPatches, // forward unchanged
//	}, events.PropagateCorrelation(startedEvent))
func NewDeploymentCompletedEvent(result *DeploymentResult, opts ...CorrelationOption) *DeploymentCompletedEvent {
	if result != nil && result.occurrence != nil {
		panic("authenticated deployment result requires NewDeploymentCompletedEventWithCycle")
	}
	return newDeploymentCompletedEvent(result, opts...)
}

// NewDeploymentCompletedEventWithCycle creates a production completion.
func NewDeploymentCompletedEventWithCycle(result *DeploymentResult, opts ...CorrelationOption) (*DeploymentCompletedEvent, error) {
	if result == nil {
		return nil, errors.New("deployment completion result is nil")
	}
	occurrence, err := result.RenderOccurrence()
	if err != nil {
		return nil, fmt.Errorf("deployment completion: %w", err)
	}
	carrier, identity, err := inspectRenderOccurrence(occurrence)
	if err != nil {
		return nil, fmt.Errorf("deployment completion: %w", err)
	}
	owned := *result
	owned.CycleSnapshot = nil
	owned.OutputSnapshot = nil
	owned.StatusPatches = nil
	owned.StatusPatchSnapshot = nil
	owned.ContentChecksum = ""
	owned.RenderProof = ""
	owned.Plan = nil
	event := newDeploymentCompletedEvent(&owned, opts...)
	event.renderOccurrenceCarrier = carrier
	authenticated := withDeploymentCompletedIdentity(event, identity)
	return &authenticated, nil
}

// NewDeploymentCompletedEventWithOutputSnapshot creates a production completion.
func NewDeploymentCompletedEventWithOutputSnapshot(result *DeploymentResult, opts ...CorrelationOption) (*DeploymentCompletedEvent, error) {
	return NewDeploymentCompletedEventWithCycle(result, opts...)
}

func newDeploymentCompletedEvent(result *DeploymentResult, opts ...CorrelationOption) *DeploymentCompletedEvent {
	// Defensive copy of the map
	breakdownCopy := maps.Clone(result.OperationBreakdown)

	return &DeploymentCompletedEvent{
		OutputSnapshot:      nil,
		DeploymentID:        result.DeploymentID,
		Total:               result.Total,
		Succeeded:           result.Succeeded,
		Failed:              result.Failed,
		PendingReloads:      result.PendingReloads,
		PendingReloadUntil:  result.PendingReloadUntil,
		DurationMs:          result.DurationMs,
		ReloadsTriggered:    result.ReloadsTriggered,
		TotalAPIOperations:  result.TotalAPIOperations,
		OperationBreakdown:  breakdownCopy,
		BackendDiffFields:   result.BackendDiffFields,
		StatusPatches:       cloneStatusPatches(result.StatusPatches),
		StatusPatchSnapshot: result.StatusPatchSnapshot,
		ContentChecksum:     result.ContentChecksum,
		RenderProof:         result.RenderProof,
		Plan:                result.Plan.Clone(),
		PodSetHash:          result.PodSetHash,
		Phases:              clonePhases(result.Phases),
		timestamped:         newTimestamped(),
		Correlation:         newCorrelation(opts...),
	}
}

func clonePhases(p *DeployPhases) *DeployPhases {
	if p == nil {
		return nil
	}
	clone := *p
	return &clone
}

func (e *DeploymentCompletedEvent) EventType() string { return EventTypeDeploymentCompleted }

// CloneForSubscriber restores authenticated shadows and isolates legacy payloads.
func (e *DeploymentCompletedEvent) CloneForSubscriber() busevents.Event {
	if e == nil {
		panic("cannot clone nil deployment completed event")
	}
	clone := *e
	clone.OperationBreakdown = cloneOperationBreakdown(e.OperationBreakdown)
	clone.StatusPatches = cloneStatusPatches(e.StatusPatches)
	clone.Plan = e.Plan.Clone()
	clone.Phases = clonePhases(e.Phases)
	if e.occurrence != nil {
		clone = withDeploymentCompletedIdentity(&clone, mustInspectRenderOccurrence(e.renderOccurrenceCarrier))
	}
	return &clone
}

func withDeploymentResultIdentity(source *DeploymentResult, identity *renderOccurrenceIdentity) DeploymentResult {
	result := *source
	result.CycleSnapshot = identity.cycle
	result.OutputSnapshot = identity.output
	result.StatusPatches = nil
	result.StatusPatchSnapshot = identity.statusPatches
	result.ContentChecksum = identity.contentChecksum
	result.RenderProof = identity.proof
	result.Plan = nil
	return result
}

func withDeploymentCompletedIdentity(source *DeploymentCompletedEvent, identity *renderOccurrenceIdentity) DeploymentCompletedEvent {
	event := *source
	event.CycleSnapshot = identity.cycle
	event.OutputSnapshot = identity.output
	event.StatusPatches = nil
	event.StatusPatchSnapshot = identity.statusPatches
	event.ContentChecksum = identity.contentChecksum
	event.RenderProof = identity.proof
	event.Plan = nil
	return event
}

// Coalescible implements busevents.CoalescibleEvent. A completed event is a
// full-state notification: it carries the complete status patch set of the
// config the deploy shipped, so for consumers that declare it in their
// CoalescesOn list (only the status applier) the newest of an uninterrupted
// run supersedes the older ones. Consumers with per-event bookkeeping (the
// deployer clears its in-flight flag per completion) must simply not declare
// this type — coalescing is always per-subscriber opt-in.
func (e *DeploymentCompletedEvent) Coalescible() bool { return true }

// DeploymentSkippedEvent is published when the deployment scheduler determines
// that the data plane is already at the just-rendered configuration and no
// deployment work needs to be performed (typically: rendered config hash and
// pod-set hash both match the last successful deployment).
//
// Semantically this is NOT a deployment — nothing was pushed, no reload was
// triggered, no API operations were issued. It exists as its own event type
// so that downstream consumers can distinguish "the controller is converged"
// from "the controller just completed work."
//
// Currently consumed by:
//   - statusapplier, which treats this equivalently to DeploymentCompletedEvent
//     for the purpose of applying the "deployed" status-patch variant — the
//     data plane is serving the latest config, so Kubernetes status conditions
//     gated on data-plane readiness should reflect the current generation.
//
// Other consumers (metrics, commentator, drift_monitor, scheduler,
// statecache) do not subscribe by design — skipped deployments are a
// steady-state signal and bursting through those consumers would either
// produce log spam (commentator) or misleading counters (metrics). They can
// opt in later if there's a concrete need.
//
// This event propagates the correlation ID from the triggering event
// (typically TemplateRenderedEvent) so the converged path remains
// observable in correlation-based tracing.
type DeploymentSkippedEvent struct {
	renderOccurrenceCarrier

	// CycleSnapshot is the exact render cycle already served by the fleet.
	CycleSnapshot *rendercycle.Snapshot

	// OutputSnapshot is the exact render already served by the fleet.
	OutputSnapshot *renderoutput.Snapshot

	// Total is the number of HAProxy endpoints already serving the rendered
	// configuration. Mirrors DeploymentCompletedEvent.Total so subscribers
	// can apply the same "is there actually a data plane to talk to?" guard.
	Total int

	// Reason is a short tag describing why no deployment ran for this
	// config: SkipReasonConfigUnchanged or SkipReasonReloadObserved.
	Reason string

	// ConfigHash is the content checksum of the rendered HAProxy
	// configuration that matched the last successful deployment. Useful
	// for debugging / correlation across the deployer's logs.
	ConfigHash string

	// PodSetHash is the hash of the endpoint set that matched the last
	// successful deployment. Useful for debugging / correlation.
	PodSetHash string

	// StatusPatches are the chart-rendered status patches for the
	// already-deployed configuration. The StatusApplier reads them from
	// this event to write the "deployed" variant — the data plane is
	// serving this exact config, so conditions gated on data-plane
	// readiness should reflect the current generation.
	StatusPatches []templating.StatusPatch

	// StatusPatchSnapshot carries the immutable patch set already deployed.
	StatusPatchSnapshot *templating.StatusPatchSnapshot
	RenderProof         string
	Plan                *renderplan.Plan

	timestamped

	// Correlation embeds correlation tracking for event tracing.
	Correlation
}

// NewDeploymentSkippedEvent creates a new DeploymentSkippedEvent.
//
// statusPatches is the chart-rendered patch set for the already-deployed
// configuration; the StatusApplier reads it from the event to write the
// "deployed" variant. The outer slice is defensively cloned per the
// immutability contract documented in events/CLAUDE.md.
//
// Use PropagateCorrelation() to propagate correlation from the triggering
// event so the skip remains correlated with the originating reconciliation:
//
//	event := events.NewDeploymentSkippedEvent(
//	    len(endpoints),
//	    "config_unchanged",
//	    configHash,
//	    podSetHash,
//	    statusPatches,
//	    events.PropagateCorrelation(scheduledEvent),
//	)
func NewDeploymentSkippedEvent(total int, reason, configHash, podSetHash string, statusPatches []templating.StatusPatch, opts ...CorrelationOption) *DeploymentSkippedEvent {
	return &DeploymentSkippedEvent{
		Total:         total,
		Reason:        reason,
		ConfigHash:    configHash,
		PodSetHash:    podSetHash,
		StatusPatches: cloneStatusPatches(statusPatches),
		timestamped:   newTimestamped(),
		Correlation:   newCorrelation(opts...),
	}
}

// NewDeploymentSkippedEventWithIdentity carries the exact render the fleet is
// already serving.
func NewDeploymentSkippedEventWithIdentity(
	total int,
	reason string,
	configHash string,
	podSetHash string,
	statusPatches []templating.StatusPatch,
	renderProof string,
	plan *renderplan.Plan,
	opts ...CorrelationOption,
) *DeploymentSkippedEvent {
	event := NewDeploymentSkippedEvent(total, reason, configHash, podSetHash, statusPatches, opts...)
	event.RenderProof = renderProof
	event.Plan = plan.Clone()
	return event
}

// NewDeploymentSkippedEventWithStatusSnapshot carries an immutable patch set.
func NewDeploymentSkippedEventWithStatusSnapshot(
	total int,
	reason string,
	configHash string,
	podSetHash string,
	statusPatchSnapshot *templating.StatusPatchSnapshot,
	renderProof string,
	plan *renderplan.Plan,
	opts ...CorrelationOption,
) *DeploymentSkippedEvent {
	event := NewDeploymentSkippedEvent(total, reason, configHash, podSetHash, nil, opts...)
	event.StatusPatchSnapshot = statusPatchSnapshot
	event.RenderProof = renderProof
	event.Plan = plan.Clone()
	return event
}

// NewDeploymentSkippedEventWithCycle carries one exact render cycle.
func NewDeploymentSkippedEventWithCycle(
	occurrence *rendercycle.Occurrence,
	total int,
	reason string,
	podSetHash string,
	opts ...CorrelationOption,
) (*DeploymentSkippedEvent, error) {
	return newDeploymentSkippedEventWithOccurrence(
		occurrence, total, reason, podSetHash, opts...,
	)
}

// NewDeploymentSkippedEventWithOutputSnapshot carries exact occurrence identity.
func NewDeploymentSkippedEventWithOutputSnapshot(
	occurrence *rendercycle.Occurrence,
	total int,
	reason string,
	podSetHash string,
	opts ...CorrelationOption,
) (*DeploymentSkippedEvent, error) {
	return newDeploymentSkippedEventWithOccurrence(
		occurrence, total, reason, podSetHash, opts...,
	)
}

func newDeploymentSkippedEventWithOccurrence(
	occurrence *rendercycle.Occurrence,
	total int,
	reason string,
	podSetHash string,
	opts ...CorrelationOption,
) (*DeploymentSkippedEvent, error) {
	carrier, identity, err := inspectRenderOccurrence(occurrence)
	if err != nil {
		return nil, fmt.Errorf("deployment skipped event: %w", err)
	}
	event := NewDeploymentSkippedEvent(total, reason, "", podSetHash, nil, opts...)
	event.renderOccurrenceCarrier = carrier
	authenticated := withDeploymentSkippedIdentity(event, identity)
	return &authenticated, nil
}

func (e *DeploymentSkippedEvent) EventType() string { return EventTypeDeploymentSkipped }

// CloneForSubscriber restores authenticated shadows and isolates legacy payloads.
func (e *DeploymentSkippedEvent) CloneForSubscriber() busevents.Event {
	if e == nil {
		panic("cannot clone nil deployment skipped event")
	}
	clone := *e
	clone.StatusPatches = cloneStatusPatches(e.StatusPatches)
	clone.Plan = e.Plan.Clone()
	if e.occurrence != nil {
		clone = withDeploymentSkippedIdentity(&clone, mustInspectRenderOccurrence(e.renderOccurrenceCarrier))
	}
	return &clone
}

func withDeploymentSkippedIdentity(source *DeploymentSkippedEvent, identity *renderOccurrenceIdentity) DeploymentSkippedEvent {
	event := *source
	event.CycleSnapshot = identity.cycle
	event.OutputSnapshot = identity.output
	event.ConfigHash = identity.contentChecksum
	event.StatusPatches = nil
	event.StatusPatchSnapshot = identity.statusPatches
	event.RenderProof = identity.proof
	event.Plan = nil
	return event
}

// Coalescible implements busevents.CoalescibleEvent — same full-state
// latest-wins rationale as DeploymentCompletedEvent.Coalescible.
func (e *DeploymentSkippedEvent) Coalescible() bool { return true }

// DeploymentScheduledEvent is published when the deployment scheduler has decided.
// to execute a deployment. This event contains all necessary data for the deployer
// to execute the deployment without maintaining state.
//
// Published by: DeploymentScheduler.
// Consumed by: Deployer component.
//
// This event propagates the correlation ID from TemplateRenderedEvent.
//
// This event implements CoalescibleEvent. The coalescible flag is propagated from
// TemplateRenderedEvent to enable coalescing throughout the reconciliation pipeline.
type DeploymentScheduledEvent struct {
	renderOccurrenceCarrier

	// CycleSnapshot is the exact render cycle to deploy.
	CycleSnapshot *rendercycle.Snapshot

	// OutputSnapshot is the exact render to deploy.
	OutputSnapshot *renderoutput.Snapshot

	// Config is the rendered HAProxy configuration to deploy.
	Config string

	// AuxiliaryFiles contains all rendered auxiliary files.
	AuxiliaryFiles *dataplane.AuxiliaryFiles

	// Endpoints is the list of HAProxy endpoints to deploy to.
	Endpoints []dataplane.Endpoint

	// RuntimeConfigName is the name of the HAProxyCfg resource.
	// Used for publishing ConfigAppliedToPodEvent after successful deployment.
	RuntimeConfigName string

	// RuntimeConfigNamespace is the namespace of the HAProxyCfg resource.
	// Used for publishing ConfigAppliedToPodEvent after successful deployment.
	RuntimeConfigNamespace string

	// ContentChecksum is the pre-computed content checksum covering config + aux files.
	// Propagated from TemplateRenderedEvent to enable aux file comparison caching
	// in the deployer — when the checksum matches the last-deployed checksum for
	// an endpoint, the expensive aux file comparison is skipped.
	ContentChecksum string

	// Reason describes why this deployment was scheduled.
	// Examples: "config_validation", "pod_discovery", "drift_prevention"
	Reason string

	// Plan is the structure of the render being deployed, propagated from
	// TemplateRenderedEvent. Nil when the render produced none.
	Plan *renderplan.Plan

	// PlanID is the digest identifying Plan; it becomes the pod's applied plan
	// id once the deployment lands.
	PlanID string

	// RenderProof is the controller-local witness for this exact render.
	RenderProof string

	// StatusPatches are the chart-rendered status patches for this
	// configuration. The Deployer forwards them unchanged into
	// DeploymentCompletedEvent so the StatusApplier can apply the
	// "deployed" variant with the patches that correspond exactly to
	// the config this deployment shipped.
	StatusPatches []templating.StatusPatch

	// StatusPatchSnapshot carries the immutable patch set for this configuration.
	StatusPatchSnapshot *templating.StatusPatchSnapshot

	// coalescible indicates if this event can be safely skipped when a newer
	// event of the same type is available. Propagated from TemplateRenderedEvent.
	coalescible bool

	timestamped

	// Correlation embeds correlation tracking for event tracing.
	Correlation
}

// NewDeploymentScheduledEvent creates a new DeploymentScheduledEvent.
// Performs defensive copy of endpoints slice.
//
// The coalescible parameter should be propagated from TemplateRenderedEvent.Coalescible()
// to enable coalescing throughout the reconciliation pipeline.
//
// The contentChecksum is the pre-computed checksum of config + aux files, propagated from
// TemplateRenderedEvent. It enables the deployer to skip expensive aux file comparison
// when the content hasn't changed since the last successful sync to an endpoint.
//
// statusPatches is the chart-rendered patch set for this configuration. The Deployer
// forwards it unchanged into DeploymentCompletedEvent so the StatusApplier can apply
// the "deployed" variant with the patches that correspond exactly to the config this
// deployment shipped. The outer slice is defensively cloned.
//
// Use PropagateCorrelation() to propagate correlation from the triggering event:
//
//	event := events.NewDeploymentScheduledEvent(config, auxFiles, endpoints, name, ns, reason, contentChecksum, plan, planID, statusPatches, coalescible,
//	    events.PropagateCorrelation(validationEvent))
func NewDeploymentScheduledEvent(config string, auxFiles *dataplane.AuxiliaryFiles, endpoints []dataplane.Endpoint, runtimeConfigName, runtimeConfigNamespace, reason, contentChecksum string, plan *renderplan.Plan, planID string, statusPatches []templating.StatusPatch, coalescible bool, opts ...CorrelationOption) *DeploymentScheduledEvent {
	return newDeploymentScheduledEvent(config, auxFiles, endpoints, runtimeConfigName, runtimeConfigNamespace, reason,
		contentChecksum, plan, planID, nextRenderProof(), statusPatches, coalescible, opts...)
}

// NewDeploymentScheduledEventWithStatusSnapshot preserves immutable status payloads.
func NewDeploymentScheduledEventWithStatusSnapshot(config string, auxFiles *dataplane.AuxiliaryFiles, endpoints []dataplane.Endpoint, runtimeConfigName, runtimeConfigNamespace, reason, contentChecksum string, plan *renderplan.Plan, planID, renderProof string, statusPatchSnapshot *templating.StatusPatchSnapshot, coalescible bool, opts ...CorrelationOption) *DeploymentScheduledEvent {
	event := newDeploymentScheduledEvent(config, auxFiles, endpoints, runtimeConfigName, runtimeConfigNamespace, reason,
		contentChecksum, plan, planID, renderProof, nil, coalescible, opts...)
	event.StatusPatchSnapshot = statusPatchSnapshot
	return event
}

// NewDeploymentScheduledEventWithCycle creates a production deployment.
func NewDeploymentScheduledEventWithCycle(
	occurrence *rendercycle.Occurrence,
	endpoints []dataplane.Endpoint,
	runtimeConfigName, runtimeConfigNamespace, reason string,
	coalescible bool,
	opts ...CorrelationOption,
) (*DeploymentScheduledEvent, error) {
	return newDeploymentScheduledEventWithOccurrence(
		occurrence, endpoints, runtimeConfigName, runtimeConfigNamespace, reason,
		coalescible, opts...,
	)
}

// NewDeploymentScheduledEventWithOutputSnapshot creates a production deployment.
func NewDeploymentScheduledEventWithOutputSnapshot(
	occurrence *rendercycle.Occurrence,
	endpoints []dataplane.Endpoint,
	runtimeConfigName, runtimeConfigNamespace, reason string,
	coalescible bool,
	opts ...CorrelationOption,
) (*DeploymentScheduledEvent, error) {
	return newDeploymentScheduledEventWithOccurrence(
		occurrence, endpoints, runtimeConfigName, runtimeConfigNamespace, reason,
		coalescible, opts...,
	)
}

func newDeploymentScheduledEventWithOccurrence(
	occurrence *rendercycle.Occurrence,
	endpoints []dataplane.Endpoint,
	runtimeConfigName, runtimeConfigNamespace, reason string,
	coalescible bool,
	opts ...CorrelationOption,
) (*DeploymentScheduledEvent, error) {
	carrier, identity, err := inspectRenderOccurrence(occurrence)
	if err != nil {
		return nil, fmt.Errorf("deployment scheduled event: %w", err)
	}
	event := newDeploymentScheduledEvent(
		"", nil, endpoints, runtimeConfigName, runtimeConfigNamespace, reason,
		"", nil, "", "", nil, coalescible, opts...,
	)
	event.renderOccurrenceCarrier = carrier
	authenticated := withDeploymentScheduledIdentity(event, identity)
	return &authenticated, nil
}

func newDeploymentScheduledEvent(config string, auxFiles *dataplane.AuxiliaryFiles, endpoints []dataplane.Endpoint, runtimeConfigName, runtimeConfigNamespace, reason, contentChecksum string, plan *renderplan.Plan, planID, renderProof string, statusPatches []templating.StatusPatch, coalescible bool, opts ...CorrelationOption) *DeploymentScheduledEvent {
	return &DeploymentScheduledEvent{
		Config:                 config,
		AuxiliaryFiles:         dataplane.CloneAuxiliaryFiles(auxFiles),
		Endpoints:              copySlice(endpoints),
		RuntimeConfigName:      runtimeConfigName,
		RuntimeConfigNamespace: runtimeConfigNamespace,
		ContentChecksum:        contentChecksum,
		Reason:                 reason,
		Plan:                   plan.Clone(),
		PlanID:                 planID,
		RenderProof:            renderProof,
		StatusPatches:          cloneStatusPatches(statusPatches),
		coalescible:            coalescible,
		timestamped:            newTimestamped(),
		Correlation:            newCorrelation(opts...),
	}
}

func (e *DeploymentScheduledEvent) EventType() string { return EventTypeDeploymentScheduled }

// CloneForSubscriber restores authenticated shadows and isolates legacy payloads.
func (e *DeploymentScheduledEvent) CloneForSubscriber() busevents.Event {
	if e == nil {
		panic("cannot clone nil deployment scheduled event")
	}
	clone := *e
	clone.AuxiliaryFiles = dataplane.CloneAuxiliaryFiles(e.AuxiliaryFiles)
	clone.Endpoints = copySlice(e.Endpoints)
	clone.Plan = e.Plan.Clone()
	clone.StatusPatches = cloneStatusPatches(e.StatusPatches)
	if e.occurrence != nil {
		clone = withDeploymentScheduledIdentity(&clone, mustInspectRenderOccurrence(e.renderOccurrenceCarrier))
	}
	return &clone
}

func withDeploymentScheduledIdentity(source *DeploymentScheduledEvent, identity *renderOccurrenceIdentity) DeploymentScheduledEvent {
	event := *source
	event.CycleSnapshot = identity.cycle
	event.OutputSnapshot = identity.output
	event.Config = identity.config
	event.AuxiliaryFiles = nil
	event.ContentChecksum = identity.contentChecksum
	event.Plan = nil
	event.PlanID = identity.planID
	event.RenderProof = identity.proof
	event.StatusPatches = nil
	event.StatusPatchSnapshot = identity.statusPatches
	return event
}

// Coalescible returns true if this event can be safely skipped when a newer
// event of the same type is available. This implements the CoalescibleEvent interface.
func (e *DeploymentScheduledEvent) Coalescible() bool { return e.coalescible }

// DeploymentCancelRequestEvent is published when the scheduler requests cancellation
// of an in-progress deployment (e.g., due to timeout).
//
// Published by: DeploymentScheduler (on timeout)
// Consumed by: Deployer (to cancel running deployment)
//
// DeploymentID must match the scheduled event being cancelled.
type DeploymentCancelRequestEvent struct {
	// DeploymentID identifies the exact DeploymentScheduledEvent to cancel.
	DeploymentID string

	// Reason describes why the deployment is being cancelled.
	Reason string

	timestamped
	Correlation
}

// DeploymentCancelRequestEvent reasons.
const (
	// DeploymentCancelReasonEndpointAuthorityChanged supersedes the deploy: its
	// pod set is not the fleet any more and the replacement set is deployed next.
	DeploymentCancelReasonEndpointAuthorityChanged = "endpoint_authority_changed"
	DeploymentCancelReasonTimeout                  = "deployment_timeout"
)

// NewDeploymentCancelRequestEvent creates a new DeploymentCancelRequestEvent.
func NewDeploymentCancelRequestEvent(deploymentID, reason string, opts ...CorrelationOption) *DeploymentCancelRequestEvent {
	return &DeploymentCancelRequestEvent{
		DeploymentID: deploymentID,
		Reason:       reason,
		timestamped:  newTimestamped(),
		Correlation:  newCorrelation(opts...),
	}
}

func (e *DeploymentCancelRequestEvent) EventType() string { return EventTypeDeploymentCancelRequest }

// DriftPreventionTriggeredEvent is published when the drift prevention monitor.
// detects that no deployment has occurred within the configured interval and
// triggers a deployment to prevent configuration drift.
//
// Published by: DriftPreventionMonitor.
// Consumed by: DeploymentScheduler (which then schedules a deployment).
type DriftPreventionTriggeredEvent struct {
	// TimeSinceLastDeployment is the duration since the last deployment completed.
	TimeSinceLastDeployment time.Duration

	timestamped
}

// NewDriftPreventionTriggeredEvent creates a new DriftPreventionTriggeredEvent.
func NewDriftPreventionTriggeredEvent(timeSinceLast time.Duration) *DriftPreventionTriggeredEvent {
	return &DriftPreventionTriggeredEvent{
		TimeSinceLastDeployment: timeSinceLast,
		timestamped:             newTimestamped(),
	}
}

func (e *DriftPreventionTriggeredEvent) EventType() string { return EventTypeDriftPreventionTriggered }
