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

package configchange

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/component"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/generated/clientset/versioned"
	clientsetscheme "github.com/haproxy-haptic/HAPTIC/pkg/generated/clientset/versioned/scheme"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
)

const (
	// StatusUpdaterComponentName is the unique identifier for this component.
	StatusUpdaterComponentName = "status-updater"

	// StatusUpdaterEventBufferSize is the size of the event subscription buffer.
	StatusUpdaterEventBufferSize = busevents.StandardSubscriberBuffer

	// Validation status values written to HAProxyTemplateConfig.Status.ValidationStatus.
	statusValid   = "Valid"
	statusInvalid = "Invalid"
	statusUnknown = "Unknown"

	// eventSourceComponent identifies this controller as the source of emitted
	// Kubernetes Events, so they appear under `kubectl describe haproxytemplateconfig`.
	eventSourceComponent = "haptic-controller"
	// Event reasons (CamelCase per Kubernetes convention).
	eventReasonValidationFailed       = "ValidationFailed"
	eventReasonValidated              = "Validated"
	eventReasonValidationInconclusive = "ValidationInconclusive"
	// Render-gate Event reasons: HAProxy's verdict on a render that was
	// already dispatched, as opposed to a config the load gate refused.
	eventReasonRenderRefused   = "RenderRefusedByHAProxy"
	eventReasonRenderAccepted  = "RenderAcceptedByHAProxy"
	eventReasonContentRevoked  = "HTTPContentRevoked"
	eventReasonContentRejected = "HTTPContentRejected"

	// conditionValidated is the status condition type reporting whether the
	// controller accepted (validated) the observed config generation. Its
	// ObservedGeneration field, together with status.observedGeneration, is the
	// Kubernetes-native way to answer "has the controller processed generation N
	// of my config?" without coupling the spec to a controller version.
	conditionValidated = v1alpha1.ConditionTypeValidated
	// Condition reasons (CamelCase per Kubernetes convention).
	reasonValidationSucceeded     = v1alpha1.ReasonValidationSucceeded
	reasonConfigInvalid           = v1alpha1.ReasonConfigInvalid
	reasonValidationInconclusive  = v1alpha1.ReasonValidationInconclusive
	reasonHAProxyValidationFailed = v1alpha1.ReasonHAProxyValidationFailed
	// A starting pod retries rejected configuration and stays NotReady.
	reasonLoadGateFailed = v1alpha1.ReasonLoadGateFailed
)

// StatusUpdater updates HAProxyTemplateConfig status based on validation results.
//
// This component subscribes to ConfigValidatedEvent, ConfigInvalidEvent, and
// ValidationFailedEvent, updating the HAProxyTemplateConfig CRD status to reflect
// validation state. Users can then see validation errors via `kubectl describe haproxytemplateconfig`.
//
// Architecture:
// This is an event adapter that bridges validation events to Kubernetes API updates.
// It uses the generated typed client for HAProxyTemplateConfig status updates.
//
// The component handles two types of validation:
// - Config validation (Stage 1): Template syntax, JSONPath expressions, etc.
// - HAProxy validation (Stage 4): Rendered config syntax check with haproxy -c.
type StatusUpdater struct {
	*component.Base
	*component.ReadySignal

	// Followers recover missed verdicts through replay on takeover.
	leading atomic.Bool

	crdClient versioned.Interface

	// kubeClient, broadcaster and recorder emit Kubernetes Events on the
	// HAProxyTemplateConfig so failures surface in `kubectl describe` /
	// `kubectl get events`, not just in CRD status and controller logs. The
	// broadcaster's lifecycle is owned by Start/Stop.
	kubeClient  kubernetes.Interface
	broadcaster record.EventBroadcaster
	recorder    record.EventRecorder

	// ctx is the event-loop context captured by Start. Handlers run only on the
	// loop goroutine and use it for Kubernetes API calls so status writes abort
	// on shutdown.
	ctx context.Context

	// Cached config references for HAProxy validation events
	// (ValidationFailedEvent doesn't include CRD references): every source of
	// the merged set, at the generation last observed, so the verdict lands on
	// all of them.
	mu         sync.RWMutex
	configRefs []events.ConfigSourceRef
	// lastEmittedStatus is the validation status of the most recent emitted
	// Event, so a Normal "Validated" Event fires only on recovery
	// (Invalid -> Valid), not on every routine successful validation.
	lastEmittedStatus string
	// lastGateState is what the render gate's last emitted Event said, so its
	// Events mark changes rather than repeating once per render. Nil until the
	// gate has answered at all.
	lastGateState *gateEventState
	// haproxyRefused is set while the Validated condition says a render
	// failed HAProxy's check, so the next passing verdict can clear it: the
	// condition is otherwise only rewritten on a configuration load, and a
	// single failed check would leave it False for the rest of the term.
	haproxyRefused bool
	// Verdict sequences are local to this iteration.
	verdict      uint64
	verdictValid bool

	// asserted is the status this leader last wrote (or found already
	// written) per source, keyed by namespace/name. The status guard
	// re-asserts it over stale writers (#270).
	asserted map[string]v1alpha1.HAProxyTemplateConfigStatus
	// writeMu serializes status writes with the guard, so the guard never
	// restores a verdict the event loop is replacing.
	writeMu                sync.Mutex
	statusGuardWake        chan struct{}
	statusGuardMinInterval time.Duration
	statusGuardMaxInterval time.Duration
}

// NewStatusUpdater creates a new StatusUpdater.
//
// Parameters:
//   - crdClient: typed client for HAProxyTemplateConfig CRD status updates
//   - kubeClient: core client used to emit Kubernetes Events on the CRD
//   - eventBus: EventBus to subscribe to validation events
//   - logger: Structured logger for diagnostics
//
// Returns:
//   - *StatusUpdater ready to start
func NewStatusUpdater(
	crdClient versioned.Interface,
	kubeClient kubernetes.Interface,
	eventBus *busevents.EventBus,
	logger *slog.Logger,
) *StatusUpdater {
	u := &StatusUpdater{
		ReadySignal:            component.NewReadySignal(),
		crdClient:              crdClient,
		kubeClient:             kubeClient,
		asserted:               make(map[string]v1alpha1.HAProxyTemplateConfigStatus),
		statusGuardWake:        make(chan struct{}, 1),
		statusGuardMinInterval: statusGuardMinInterval,
		statusGuardMaxInterval: statusGuardMaxInterval,
	}

	// Subscribe to only the event types we handle during construction (before
	// EventBus.Start()) for proper startup synchronization and reduced buffer
	// pressure. component.Base supplies the subscribe + dispatch loop with
	// panic recovery and a sync.Once-guarded Stop, so this component no longer
	// hand-rolls a stopCh or its own select loop.
	u.Base = component.New(&component.Config{
		EventBus:   eventBus,
		Logger:     logger,
		Name:       StatusUpdaterComponentName,
		BufferSize: StatusUpdaterEventBufferSize,
		Handler:    u,
		EventTypes: []string{
			events.EventTypeConfigValidated,
			events.EventTypeConfigInvalid,
			events.EventTypeConfigValidationInconclusive,
			events.EventTypeConfigVerdictReplayed,
			events.EventTypeValidationFailed,
			events.EventTypeRenderGateCompleted,
			events.EventTypeHTTPContentRevoked,
			events.EventTypeHTTPContentRejected,
		},
		EventFilter: func(busevents.Event) bool { return u.leading.Load() },
	})

	return u
}

// Start captures the loop context for handlers and runs the embedded
// component.Base event loop until the context is cancelled or Stop is called.
// Returns nil on graceful shutdown.
func (u *StatusUpdater) Start(ctx context.Context) error {
	u.ctx = ctx
	// Build the Event recorder and forward recorded Events to the API server for
	// the lifetime of the loop. Done here (not in the constructor) so the
	// broadcaster goroutine only exists while the component is actually running.
	u.broadcaster = record.NewBroadcaster()
	u.recorder = u.broadcaster.NewRecorder(clientsetscheme.Scheme, corev1.EventSource{Component: eventSourceComponent})
	u.broadcaster.StartRecordingToSink(&corev1client.EventSinkImpl{Interface: u.kubeClient.CoreV1().Events("")})
	defer u.broadcaster.Shutdown()
	// Each term starts from the handler's newest verdict replay.
	u.FlushPending()
	u.mu.Lock()
	clear(u.asserted)
	u.configRefs = nil
	u.haproxyRefused = false
	u.verdict = 0
	u.verdictValid = false
	u.mu.Unlock()
	u.leading.Store(true)
	defer u.Rearm()
	defer u.leading.Store(false)
	u.MarkReady()
	guardCtx, stopGuard := context.WithCancel(ctx)
	defer stopGuard()
	go u.runStatusGuard(guardCtx)
	return u.Base.Start(ctx)
}

// HandleEvent implements component.EventHandler, routing each validation event
// to the matching status-update handler using the loop context captured by
// Start.
func (u *StatusUpdater) HandleEvent(event busevents.Event) {
	switch e := event.(type) {
	case *events.ConfigVerdictReplayedEvent:
		u.HandleEvent(e.Verdict)
	case *events.ConfigValidatedEvent:
		u.handleConfigValidated(u.ctx, e)
	case *events.ConfigInvalidEvent:
		u.handleConfigInvalid(u.ctx, e)
	case *events.ConfigValidationInconclusiveEvent:
		u.handleConfigValidationInconclusive(u.ctx, e)
	case *events.ValidationFailedEvent:
		u.handleHAProxyValidationFailed(u.ctx, e)
	case *events.RenderGateCompletedEvent:
		u.handleRenderGateCompleted(e)
		u.restoreValidatedAfterPass(u.ctx, e)
	case *events.HTTPContentRevokedEvent:
		u.handleHTTPContentRevoked(e)
	case *events.HTTPContentRejectedEvent:
		u.handleHTTPContentRejected(e)
	}
}

// restoreValidatedAfterPass sets the Validated condition back to True once
// HAProxy accepts a render again after a failed check.
func (u *StatusUpdater) restoreValidatedAfterPass(ctx context.Context, event *events.RenderGateCompletedEvent) {
	if !event.Newest || !event.OK {
		return
	}
	u.mu.Lock()
	refused := u.haproxyRefused
	u.haproxyRefused = false
	refs := slices.Clone(u.configRefs)
	u.mu.Unlock()
	if !refused {
		return
	}
	u.applyVerdict(ctx, refs,
		func(ref events.ConfigSourceRef, status *v1alpha1.HAProxyTemplateConfigStatus) {
			now := metav1.NewTime(time.Now())
			status.ObservedGeneration = ref.Generation
			status.LastValidated = &now
			status.ValidationStatus = statusValid
			status.ValidationMessage = "HAProxy accepted the rendered configuration again"
			status.ValidationErrors = nil
			setValidatedCondition(status, metav1.ConditionTrue, reasonValidationSucceeded,
				"HAProxy accepted the rendered configuration again", ref.Generation)
		},
		"Updated HAProxyTemplateConfig status to Valid (HAProxy accepted a render again)")
}

// handleRenderGateCompleted emits a Kubernetes Event for the render gate's
// verdict, so a refusal shows up under `kubectl describe haproxytemplateconfig`
// and in `kubectl get events` — the two places an operator looks before they
// know a metric exists.
//
// Only changes are emitted: the gate answers once per render, and a
// steady-state stream of identical Warnings would bury the one that mattered.
// The escalation from "a render was refused" to "nothing deploys any more" is
// a change, and so is HAProxy naming a different problem, so the comparison
// covers all three of them and not just the pass/fail bit.
//
// Verdicts for superseded plans are skipped: those judge a render the fleet has
// moved past, and an operator reading Events wants the current answer.
func (u *StatusUpdater) handleRenderGateCompleted(event *events.RenderGateCompletedEvent) {
	if !event.Newest {
		return
	}
	u.mu.RLock()
	refs := slices.Clone(u.configRefs)
	u.mu.RUnlock()
	if len(refs) == 0 || u.recorder == nil {
		return
	}

	state := gateEventState{ok: event.OK, pinned: event.Pinned, message: event.Message}
	u.mu.Lock()
	previous := u.lastGateState
	changed := previous == nil || *previous != state
	u.lastGateState = &state
	u.mu.Unlock()
	if !changed {
		return
	}

	for _, ref := range refs {
		object := &v1alpha1.HAProxyTemplateConfig{
			ObjectMeta: metav1.ObjectMeta{Name: ref.Name, Namespace: ref.Namespace},
		}
		if event.OK {
			u.recorder.Event(object, corev1.EventTypeNormal, eventReasonRenderAccepted,
				renderGateRecoveryMessage(previous))
			continue
		}
		u.recorder.Event(object, corev1.EventTypeWarning, eventReasonRenderRefused,
			renderGateEventMessage(event))
	}
}

// handleHTTPContentRevoked emits a Warning Event for content taken back after
// HAProxy refused a render containing it.
func (u *StatusUpdater) handleHTTPContentRevoked(event *events.HTTPContentRevokedEvent) {
	u.mu.RLock()
	refs := slices.Clone(u.configRefs)
	u.mu.RUnlock()
	if u.recorder == nil {
		return
	}
	for _, ref := range refs {
		object := &v1alpha1.HAProxyTemplateConfig{
			ObjectMeta: metav1.ObjectMeta{Name: ref.Name, Namespace: ref.Namespace},
		}
		u.recorder.Event(object, corev1.EventTypeWarning, eventReasonContentRevoked,
			httpContentRevokedMessage(event))
	}
}

func (u *StatusUpdater) handleHTTPContentRejected(event *events.HTTPContentRejectedEvent) {
	if u.recorder == nil {
		return
	}
	u.mu.RLock()
	refs := slices.Clone(u.configRefs)
	u.mu.RUnlock()
	for _, ref := range refs {
		object := &v1alpha1.HAProxyTemplateConfig{
			ObjectMeta: metav1.ObjectMeta{
				Name: ref.Name, Namespace: ref.Namespace, UID: ref.UID, ResourceVersion: ref.ResourceVersion,
			},
		}
		u.recorder.Eventf(object, corev1.EventTypeWarning, eventReasonContentRejected,
			"New content from %s failed configuration validation. It was not applied; any previously accepted content remains active. Fix the fetched content or its template.", event.URL)
	}
}

func httpContentRevokedMessage(event *events.HTTPContentRevokedEvent) string {
	effect := "Templates render without it until it is accepted again."
	switch {
	case event.Restored:
		effect = "Its previously accepted content is served instead."
	case event.Critical:
		effect = "Renders fail until it is accepted again, so nothing new deploys."
	}
	return fmt.Sprintf("HAProxy refused a configuration while new content from %s awaited confirmation, so the content was taken back. "+
		"%s Check the RenderRefusedByHAProxy Event and fix the configuration it names.", event.URL, effect)
}

// gateEventState is what an emitted Event said, so the next verdict can tell
// whether it would say anything new.
type gateEventState struct {
	ok      bool
	pinned  bool
	message string
}

// renderGateRecoveryMessage reports a pass without claiming a recovery that did
// not happen: "again" is only true if something was refused before.
func renderGateRecoveryMessage(previous *gateEventState) string {
	if previous != nil && !previous.ok {
		return "HAProxy accepted the rendered configuration again"
	}
	return "HAProxy accepted the rendered configuration"
}

// renderGateEventMessage says what is wrong, what it causes, and what to do.
//
// The effect clause has to match what actually happened: only HAProxy's own
// refusal reverts pods (revert.go leaves them alone when the check could not
// run), and telling an operator their fleet was rolled back when it was not
// sends them looking for a change nobody made.
func renderGateEventMessage(event *events.RenderGateCompletedEvent) string {
	if event.Pinned {
		return refusalCause(event) + ": " + event.Message +
			". No new render reaches the pods until this is fixed."
	}
	if event.Refused {
		return "HAProxy refused the rendered configuration: " + event.Message +
			". The pods that took it without loading it were reverted to their last known " +
			"good set, and further renders are held until a check passes."
	}
	return "The render gate could not run: " + event.Message +
		". No pod was touched — the render they hold is simply unjudged — and further " +
		"renders are held until a check succeeds."
}

func refusalCause(event *events.RenderGateCompletedEvent) string {
	if event.Refused {
		return "HAProxy refused the rendered configuration"
	}
	return "The render gate could not run"
}

// handleConfigValidated updates CRD status to reflect successful validation.
func (u *StatusUpdater) handleConfigValidated(ctx context.Context, event *events.ConfigValidatedEvent) {
	htc, ok := event.TemplateConfig.(*v1alpha1.HAProxyTemplateConfig)
	if !ok {
		u.Logger().Debug("ConfigValidatedEvent does not contain HAProxyTemplateConfig, skipping status update",
			"type", fmt.Sprintf("%T", event.TemplateConfig))
		return
	}
	refs := sourceRefsOrFallback(event.Sources, htc)
	if !u.adoptVerdict(event.VerdictSequence, true, refs) {
		return
	}

	// The verdict is a property of the merged set, so every source gets the
	// same condition; observedGeneration is per object, because it is only
	// meaningful against that object's own metadata.generation (ADR-0016).
	u.applyVerdict(ctx, refs,
		func(ref events.ConfigSourceRef, status *v1alpha1.HAProxyTemplateConfigStatus) {
			now := metav1.NewTime(time.Now())
			status.ObservedGeneration = ref.Generation
			status.LastValidated = &now
			status.ValidationStatus = statusValid
			status.ValidationMessage = "Configuration validated successfully"
			status.ValidationErrors = nil
			setValidatedCondition(status, metav1.ConditionTrue, reasonValidationSucceeded,
				"Configuration validated successfully", ref.Generation)
		},
		"Updated HAProxyTemplateConfig status to Valid",
		"version", event.Version)
}

// handleConfigInvalid updates CRD status to reflect validation failure.
func (u *StatusUpdater) handleConfigInvalid(ctx context.Context, event *events.ConfigInvalidEvent) {
	htc, ok := event.TemplateConfig.(*v1alpha1.HAProxyTemplateConfig)
	if !ok {
		u.Logger().Debug("ConfigInvalidEvent does not contain HAProxyTemplateConfig, skipping status update",
			"type", fmt.Sprintf("%T", event.TemplateConfig))
		return
	}
	refs := sourceRefsOrFallback(event.Sources, htc)
	if !u.adoptVerdict(event.VerdictSequence, false, refs) {
		return
	}

	var allErrors []string
	for _, errors := range event.ValidationErrors {
		allErrors = append(allErrors, errors...)
	}

	u.applyVerdict(ctx, refs,
		func(ref events.ConfigSourceRef, status *v1alpha1.HAProxyTemplateConfigStatus) {
			now := metav1.NewTime(time.Now())
			status.ObservedGeneration = ref.Generation
			status.LastValidated = &now
			status.ValidationStatus = statusInvalid
			status.ValidationMessage = fmt.Sprintf("%d validation error(s)", len(allErrors))
			status.ValidationErrors = allErrors
			setValidatedCondition(status, metav1.ConditionFalse, reasonConfigInvalid,
				validationEventMessage(status), ref.Generation)
		},
		"Updated HAProxyTemplateConfig status to Invalid",
		"version", event.Version,
		"error_count", len(allErrors))
}

// handleConfigValidationInconclusive reports that the observed generation is
// not yet validated: neither accepted nor rejected, retried by the handler.
func (u *StatusUpdater) handleConfigValidationInconclusive(ctx context.Context, event *events.ConfigValidationInconclusiveEvent) {
	htc, ok := event.TemplateConfig.(*v1alpha1.HAProxyTemplateConfig)
	if !ok {
		u.Logger().Debug("ConfigValidationInconclusiveEvent does not contain HAProxyTemplateConfig, skipping status update",
			"type", fmt.Sprintf("%T", event.TemplateConfig))
		return
	}
	refs := sourceRefsOrFallback(event.Sources, htc)
	if !u.adoptVerdict(event.VerdictSequence, false, refs) {
		return
	}

	var reasons []string
	for _, validatorName := range slices.Sorted(maps.Keys(event.Reasons)) {
		reasons = append(reasons, event.Reasons[validatorName]...)
	}
	const message = "Configuration could not be validated yet; the previous configuration keeps running and validation is retried"

	u.applyVerdict(ctx, refs,
		func(ref events.ConfigSourceRef, status *v1alpha1.HAProxyTemplateConfigStatus) {
			now := metav1.NewTime(time.Now())
			status.ObservedGeneration = ref.Generation
			status.LastValidated = &now
			status.ValidationStatus = statusUnknown
			status.ValidationMessage = message
			status.ValidationErrors = reasons
			setValidatedCondition(status, metav1.ConditionUnknown, reasonValidationInconclusive,
				validationEventMessage(status), ref.Generation)
		},
		"Updated HAProxyTemplateConfig status to Unknown (validation inconclusive)",
		"version", event.Version,
		"retry_in", event.RetryIn)
}

// handleHAProxyValidationFailed updates CRD status when HAProxy validation fails.
// This handles the case where config validation passes (template syntax OK) but
// the rendered config fails HAProxy's syntax check (haproxy -c).
func (u *StatusUpdater) handleHAProxyValidationFailed(ctx context.Context, event *events.ValidationFailedEvent) {
	u.mu.Lock()
	refs := slices.Clone(u.configRefs)
	// Render results cannot supersede a newer rejected or inconclusive configuration.
	u.haproxyRefused = u.verdictValid && len(refs) > 0
	refused := u.haproxyRefused
	u.mu.Unlock()

	if !refused {
		u.Logger().Debug("No valid configuration verdict this term, skipping HAProxy validation status update")
		return
	}

	u.applyVerdict(ctx, refs,
		func(ref events.ConfigSourceRef, status *v1alpha1.HAProxyTemplateConfigStatus) {
			now := metav1.NewTime(time.Now())
			status.ObservedGeneration = ref.Generation
			status.LastValidated = &now
			status.ValidationStatus = statusInvalid
			status.ValidationMessage = "HAProxy configuration validation failed"
			status.ValidationErrors = event.Errors
			setValidatedCondition(status, metav1.ConditionFalse, reasonHAProxyValidationFailed,
				"HAProxy configuration validation failed", ref.Generation)
		},
		"Updated HAProxyTemplateConfig status to Invalid (HAProxy validation)",
		"error_count", len(event.Errors))
}

func (u *StatusUpdater) adoptVerdict(sequence uint64, valid bool, refs []events.ConfigSourceRef) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if sequence <= u.verdict {
		u.Logger().Debug("Skipping a configuration verdict older than the one this term wrote",
			"verdict", sequence, "newest", u.verdict)
		return false
	}
	u.verdict = sequence
	u.verdictValid = valid
	u.configRefs = slices.Clone(refs)
	// A passing render must not clear this new verdict.
	u.haproxyRefused = false
	return true
}

// sourceRefsOrFallback returns the event's source set, or the merged object's
// own identity when the event predates source tracking (a cached replay from
// an older leader, a hand-published test event).
func sourceRefsOrFallback(refs []events.ConfigSourceRef, htc *v1alpha1.HAProxyTemplateConfig) []events.ConfigSourceRef {
	if len(refs) > 0 {
		return refs
	}
	return []events.ConfigSourceRef{{
		Namespace:       htc.Namespace,
		Name:            htc.Name,
		Generation:      htc.Generation,
		UID:             htc.UID,
		ResourceVersion: htc.ResourceVersion,
	}}
}

// setValidatedCondition upserts the standard "Validated" status condition,
// stamping the generation the controller acted on. meta.SetStatusCondition
// manages lastTransitionTime (bumped only when Status flips) and dedups by type.
func setValidatedCondition(
	status *v1alpha1.HAProxyTemplateConfigStatus,
	condStatus metav1.ConditionStatus,
	reason, message string,
	observedGeneration int64,
) {
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               conditionValidated,
		Status:             condStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: observedGeneration,
	})
}

// statusEqualIgnoringTimestamps reports whether two statuses differ in
// anything but LastValidated and condition transition times — the fields that
// change on every write by construction and carry no decision.
func statusEqualIgnoringTimestamps(a, b *v1alpha1.HAProxyTemplateConfigStatus) bool {
	normalize := func(in *v1alpha1.HAProxyTemplateConfigStatus) *v1alpha1.HAProxyTemplateConfigStatus {
		out := in.DeepCopy()
		out.LastValidated = nil
		for i := range out.Conditions {
			out.Conditions[i].LastTransitionTime = metav1.Time{}
		}
		return out
	}
	return apiequality.Semantic.DeepEqual(normalize(a), normalize(b))
}

// ReportConfigLoadFailure reports rejection before StatusUpdater starts.
// Reporting errors never mask the load failure; the pod stays NotReady and retries.
func ReportConfigLoadFailure(
	ctx context.Context,
	crdClient versioned.Interface,
	ref events.ConfigSourceRef,
	failures []string,
	logger *slog.Logger,
) {
	current, err := crdClient.HaproxyTemplateICV1alpha1().
		HAProxyTemplateConfigs(ref.Namespace).
		Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		logger.Warn("Failed to get HAProxyTemplateConfig to report load-gate failure",
			"namespace", ref.Namespace, "name", ref.Name, "error", err)
		return
	}

	if current.Status.ObservedGeneration > ref.Generation {
		logger.Info("Not reporting the load-gate failure: the status already covers a newer generation",
			"namespace", ref.Namespace, "name", ref.Name,
			"generation", ref.Generation, "observed_generation", current.Status.ObservedGeneration)
		return
	}

	now := metav1.NewTime(time.Now())
	current.Status.ObservedGeneration = ref.Generation
	current.Status.LastValidated = &now
	current.Status.ValidationStatus = statusInvalid
	current.Status.ValidationErrors = failures
	current.Status.ValidationMessage = fmt.Sprintf("Rejected at startup load gate: %d validationTest failure(s)", len(failures))
	setValidatedCondition(&current.Status, metav1.ConditionFalse, reasonLoadGateFailed,
		validationEventMessage(&current.Status), ref.Generation)

	if _, err := crdClient.HaproxyTemplateICV1alpha1().
		HAProxyTemplateConfigs(ref.Namespace).
		UpdateStatus(ctx, current, metav1.UpdateOptions{}); err != nil {
		logger.Warn("Failed to write load-gate failure to HAProxyTemplateConfig status",
			"namespace", ref.Namespace, "name", ref.Name, "error", err)
		return
	}

	logger.Info("Reported the startup load-gate rejection on HAProxyTemplateConfig status; this pod stays NotReady and retries until the config passes",
		"namespace", ref.Namespace, "name", ref.Name, "failure_count", len(failures))
}

// A newer persisted generation on any source makes the whole verdict stale.
func (u *StatusUpdater) applyVerdict(
	ctx context.Context,
	refs []events.ConfigSourceRef,
	mutate func(events.ConfigSourceRef, *v1alpha1.HAProxyTemplateConfigStatus),
	successMsg string,
	logFields ...any,
) {
	u.writeMu.Lock()
	defer u.writeMu.Unlock()

	current := make([]*v1alpha1.HAProxyTemplateConfig, len(refs))
	for i, ref := range refs {
		htc, err := u.crdClient.HaproxyTemplateICV1alpha1().
			HAProxyTemplateConfigs(ref.Namespace).
			Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			u.Logger().Warn("Failed to get HAProxyTemplateConfig for status update",
				"namespace", ref.Namespace,
				"name", ref.Name,
				"error", err)
			return
		}
		if htc.Status.ObservedGeneration > ref.Generation {
			u.Logger().Info("Not writing a verdict for an older generation than the status already covers",
				"namespace", ref.Namespace, "name", ref.Name,
				"generation", ref.Generation, "observed_generation", htc.Status.ObservedGeneration)
			return
		}
		current[i] = htc
	}
	for i, htc := range current {
		if htc != nil {
			u.writeStatus(ctx, htc, func(status *v1alpha1.HAProxyTemplateConfigStatus) {
				mutate(refs[i], status)
			}, successMsg, logFields...)
		}
	}
}

// writeStatus applies mutate to current's Status and writes it back via
// UpdateStatus. On success, successMsg is logged at DEBUG with namespace/name
// plus any extra logFields the caller supplies.
func (u *StatusUpdater) writeStatus(
	ctx context.Context,
	current *v1alpha1.HAProxyTemplateConfig,
	mutate func(*v1alpha1.HAProxyTemplateConfigStatus),
	successMsg string,
	logFields ...any,
) {
	namespace, name := current.Namespace, current.Name
	before := current.Status.DeepCopy()
	mutate(&current.Status)

	// Skip no-op writes. With N sources per merged set, most verdicts change
	// nothing on N-1 of them — writing anyway would churn resourceVersions
	// (and, without the watcher's generation filter, echo into revalidation)
	// for a timestamp refresh nobody reads.
	if statusEqualIgnoringTimestamps(before, &current.Status) {
		u.recordAsserted(namespace, name, &current.Status)
		u.Logger().Debug("Status unchanged; skipping write",
			"namespace", namespace, "name", name)
		return
	}

	if _, err := u.crdClient.HaproxyTemplateICV1alpha1().
		HAProxyTemplateConfigs(current.Namespace).
		UpdateStatus(ctx, current, metav1.UpdateOptions{}); err != nil {
		u.Logger().Warn("Failed to update HAProxyTemplateConfig status",
			"namespace", current.Namespace,
			"name", current.Name,
			"error", err)
		return
	}

	u.recordAsserted(namespace, name, &current.Status)
	u.emitStatusEvent(current)

	u.Logger().Debug(successMsg,
		append(logFields, "namespace", current.Namespace, "name", current.Name)...)
}

// emitStatusEvent records a Kubernetes Event reflecting the persisted validation
// status: a Warning whenever the config is Invalid (the recorder aggregates
// repeats), and a single Normal only on recovery (Invalid -> Valid) so routine
// successful validations don't spam events.
func (u *StatusUpdater) emitStatusEvent(htc *v1alpha1.HAProxyTemplateConfig) {
	if u.recorder == nil {
		return
	}
	u.mu.Lock()
	prev := u.lastEmittedStatus
	u.lastEmittedStatus = htc.Status.ValidationStatus
	u.mu.Unlock()

	switch htc.Status.ValidationStatus {
	case statusInvalid:
		u.recorder.Event(htc, corev1.EventTypeWarning, eventReasonValidationFailed, validationEventMessage(&htc.Status))
	case statusUnknown:
		u.recorder.Event(htc, corev1.EventTypeWarning, eventReasonValidationInconclusive, validationEventMessage(&htc.Status))
	case statusValid:
		if prev == statusInvalid || prev == statusUnknown {
			u.recorder.Event(htc, corev1.EventTypeNormal, eventReasonValidated, "HAProxyTemplateConfig is valid again")
		}
	}
}

// validationEventMessage renders a concise Event message from the status: the
// first validation error (plus a "+N more" hint), or the status message when no
// per-error detail is available.
func validationEventMessage(status *v1alpha1.HAProxyTemplateConfigStatus) string {
	if len(status.ValidationErrors) == 0 {
		return status.ValidationMessage
	}
	msg := status.ValidationErrors[0]
	if len(status.ValidationErrors) > 1 {
		msg = fmt.Sprintf("%s (+%d more)", msg, len(status.ValidationErrors)-1)
	}
	return msg
}
