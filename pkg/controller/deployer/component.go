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

// Package deployer applies a validated render to the HAProxy fleet through the
// HAPTIC agent that owns each pod's file tree.
//
// The Deployer is a stateless executor: it receives DeploymentScheduledEvent
// and, per pod, reads the agent's baseline, diffs the render against it
// (pkg/dataplane/deployplan) and sends the resulting fenced applies. All
// scheduling, rate limiting and queueing lives in the DeploymentScheduler.
package deployer

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/component"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/metrics"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercycle"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	agentclient "github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/client"
	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/lifecycle"
)

const (
	// ComponentName is the unique identifier for this component.
	ComponentName = "deployer"

	// EventBufferSize is the size of the event subscription buffer.
	// Low-volume component (~1-2 deployment events per reconciliation cycle).
	EventBufferSize = busevents.StandardSubscriberBuffer

	cancellationSubscriberName = ComponentName + "-cancellation"

	// standaloneIdentity names this controller when leader election is off.
	standaloneIdentity = "standalone"
)

// LeadershipFence is the current leadership term: who this controller is, the
// epoch every apply it sends is fenced by, and the two answers to a pod that
// refuses that epoch. A pod that has seen a higher epoch refuses this
// controller's writes.
type LeadershipFence interface {
	Identity() string
	LeaderEpoch() uint64
	// Reclaim lifts the epoch past one a pod already accepted. It errors when
	// a newer leader — not a regressed counter — is behind the refusal, which
	// is the one case where the pod is right and this controller must stop.
	Reclaim(ctx context.Context, floor uint64) (uint64, error)
	// StandDown gives leadership up so a fresh term claims a fresh epoch.
	StandDown(reason string)
}

// Component implements the deployer component.
//
// Event subscriptions:
//   - DeploymentScheduledEvent: apply the render to the scheduled endpoints
//   - DeploymentCancelRequestEvent: cancel an in-progress deployment through a
//     separate control loop that stays responsive while execution blocks
type Component struct {
	*component.Base
	*component.ReadySignal

	deploymentInProgress atomic.Bool // Defensive: prevents concurrent deployments if the scheduler has bugs
	acceptingEvents      atomic.Bool

	// ctx is the event-loop context captured by Start. Handlers run only on the
	// loop goroutine and use it for agent calls, so applies abort on shutdown.
	ctx context.Context

	clients  *agentClients
	plans    *planCache
	keeper   *planKeeper
	fence    LeadershipFence
	retained *retainedRecovery

	contentProofMu sync.Mutex
	contentProofs  map[string]map[string]contentProof

	// renderSeq orders this leadership term's applies inside its epoch. It
	// restarts per term, which the epoch makes unambiguous.
	renderSeq atomic.Uint64

	healthTracker *lifecycle.HealthTracker

	// metrics records the per-pod apply counters directly rather than over the
	// bus: each has exactly one subscriber that only increments (ADR-0001 — no
	// event hop without a second participant). Nil in tests.
	metrics *metrics.Metrics

	// ackedPlans receives the plan a deployment landed on at least one pod, so
	// the renderer reads the fleet's state instead of its own last render. Nil
	// in tests.
	ackedPlans AckedPlanSink

	// baselineSeeded is set once this term has adopted (or given up on) the
	// plan the fleet was already running. See seedBaseline.
	baselineSeeded atomic.Bool

	// stateMu guards the per-fleet facts the apply path reads: which pods must
	// be re-sent their complete state, which plans are proven good, which plans
	// each pod last reported it holds, and the pods this controller last wrote
	// to (the revert's search space).
	stateMu         sync.Mutex
	invalidBaseline map[string]struct{}
	validatedPlans  *validatedPlanSet
	observedPlans   map[string][]string
	fleet           []dataplane.Endpoint
	// awaiting are the renders the fleet accepted behind a paced reload, in
	// dispatch order, until a later deployment observes the fleet running one.
	awaiting []awaitingRender

	// Deployment cancellation support
	cancelMu            sync.Mutex
	activeDeploymentID  string                  // Event ID of the active DeploymentScheduledEvent
	activeCorrelationID string                  // Trace correlation of the active deployment
	activeCancelFunc    context.CancelCauseFunc // Cancel function for active deployment
	deploymentDone      chan struct{}           // Signals when deployment goroutine completes
	pendingCancellation string                  // Exact deployment cancelled before its handler starts
	pendingCancelCause  error                   // Cause pendingCancellation is cancelled with
	draining            bool                    // Set by Drain: the term starts no further deployment
	cancelEventChan     <-chan busevents.Event  // Out-of-band control subscription
}

// New creates a new Deployer component.
//
// syncTimeout bounds one pod's apply; the agent's own reload deadline is a
// chart-templated agent flag, not a client timeout. domainMetrics may be nil in
// tests; the per-pod counters are then not recorded.
func New(eventBus *busevents.EventBus, logger *slog.Logger, syncTimeout time.Duration, domainMetrics *metrics.Metrics) *Component {
	clients := newAgentClients(agentStateTimeout, syncTimeout)
	c := &Component{
		ReadySignal:     component.NewReadySignal(),
		clients:         clients,
		keeper:          newPlanKeeper(clients, logger),
		plans:           newPlanCache(),
		contentProofs:   map[string]map[string]contentProof{},
		invalidBaseline: map[string]struct{}{},
		observedPlans:   map[string][]string{},
		validatedPlans:  newValidatedPlanSet(),
		healthTracker:   lifecycle.NewProcessingTracker(ComponentName, lifecycle.DefaultProcessingTimeout),
		metrics:         domainMetrics,
	}
	// Standby replicas receive discovery events; leadership replay supplies fresh state on activation.
	c.Base = component.New(&component.Config{
		EventBus:    eventBus,
		Logger:      logger,
		Name:        ComponentName,
		BufferSize:  EventBufferSize,
		Handler:     c,
		EventFilter: c.acceptEvent,
		EventTypes: []string{
			events.EventTypeDeploymentScheduled,
			events.EventTypeRenderGateCompleted,
			events.EventTypeHAProxyPodsDiscovered,
		},
	})
	c.cancelEventChan = eventBus.SubscribeTypesFiltered(cancellationSubscriberName, EventBufferSize, c.acceptEvent,
		events.EventTypeDeploymentCancelRequest)
	return c
}

func (c *Component) acceptEvent(_ busevents.Event) bool {
	return c.acceptingEvents.Load()
}

// agentStateTimeout bounds a /v1/state read. The agent answers it from memory
// (or one tree hash on the drift pass), so a slow answer is a sick pod.
const agentStateTimeout = 10 * time.Second

// Start begins the deployer's event loop and blocks until ctx is cancelled.
func (c *Component) Start(ctx context.Context) error {
	defer c.Rearm()
	defer c.acceptingEvents.Store(false)
	if err := c.claimRetainedAuthority(ctx); err != nil {
		return fmt.Errorf("claiming deployment authority: %w", err)
	}
	// Discard events buffered before this leadership term. The construction-
	// time subscription persists across terms, so DeploymentScheduledEvents
	// queued when leadership was lost would otherwise replay a stale deployment
	// into the new term. Must run before MarkReady.
	c.FlushPending()
	c.flushPendingCancellationRequests()
	c.cancelMu.Lock()
	c.pendingCancellation = ""
	c.pendingCancelCause = nil
	c.draining = false
	c.cancelMu.Unlock()

	// A new term dials fresh: the previous leader's pooled connections carry no
	// state worth keeping, and its apply sequence restarts under a new epoch.
	c.clients.Close()
	c.renderSeq.Store(0)
	c.baselineSeeded.Store(false)
	c.clearBaselineInvalidations()
	c.forgetAwaitingConvergence()

	c.ctx = ctx
	c.keeper.Begin(ctx)
	controlCtx, stopControl := context.WithCancel(ctx)
	controlDone := make(chan struct{})
	go c.runCancellationLoop(controlCtx, controlDone)
	c.acceptingEvents.Store(true)
	c.MarkReady()
	err := c.Base.Start(ctx)
	c.acceptingEvents.Store(false)

	stopControl()
	c.cancelActiveDeployment("shutdown")
	<-controlDone
	c.keeper.Wait()
	c.clients.Close()
	return err
}

// HandleEvent implements component.EventHandler: it routes events to the
// appropriate handler.
func (c *Component) HandleEvent(event busevents.Event) {
	switch e := event.(type) {
	case *events.DeploymentScheduledEvent:
		c.performDeployment(c.ctx, e)
	case *events.RenderGateCompletedEvent:
		c.handleRenderGateCompleted(c.ctx, e)
	case *events.HAProxyPodsDiscoveredEvent:
		c.seedBaseline(c.ctx, e.Endpoints)
	}
}

// CoalescesOn implements component.CoalescingHandler: after each dispatch, the
// embedded component.Base drains the subscription channel and processes only
// the LATEST pending coalescible DeploymentScheduledEvent ("latest wins").
//
// This coalescing is load-bearing: deployment is single-threaded, but the
// validator + scheduler upstream can fire many DeploymentScheduledEvents during
// a single deployment. Without it, the deployer would process every queued
// event in FIFO order — deploying the OLDEST pending config first and falling
// further and further behind under load.
func (c *Component) CoalescesOn() []string {
	return []string{events.EventTypeDeploymentScheduled}
}

// HealthCheck implements the lifecycle.HealthChecker interface.
// Returns an error if the component appears to be stalled (processing for > timeout).
// Returns nil when idle - idle is always healthy for event-driven components.
func (c *Component) HealthCheck() error {
	return c.healthTracker.Check()
}

// SetValidatedOccurrence records one exact render execution the gate passed.
func (c *Component) SetValidatedOccurrence(occurrence *rendercycle.Occurrence) {
	if occurrence == nil || occurrence.ValidateAuthentication() != nil {
		return
	}
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.validatedPlans.addOccurrence(occurrence)
}

// validatedPlanFor is what a pod's manifest carries: the pod's own applied plan
// when that passed — which is the only value that promotes its baseline — and
// otherwise the newest passed plan, which is inert for this pod but keeps the
// field meaningful for a pod that catches up between the state read and the
// apply.
func (c *Component) validatedPlanFor(authority string, state *api.State) planReference {
	if state == nil {
		return planReference{}
	}
	applied := c.plans.Baseline(authority, state)
	occurrence := c.planOccurrence(authority, state.AppliedPlanID, state.AppliedPlanProof)
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.validatedPlans.resolve(
		state.AppliedPlanID, state.AppliedPlanProof, applied, occurrence,
	)
}

// planOccurrence logs a stored occurrence that stopped authenticating instead of
// reading it as an absent binding; none of the callers can return an error.
func (c *Component) planOccurrence(authority, planID, planProof string) *rendercycle.Occurrence {
	occurrence, err := c.plans.Occurrence(authority, planID, planProof)
	if err != nil {
		c.Logger().Error("Dropping a cached render occurrence that no longer authenticates",
			"plan", planID, "error", err)
	}
	return occurrence
}

func (c *Component) leaderEpoch() uint64 {
	if c.fence == nil {
		return 0
	}
	return c.fence.LeaderEpoch()
}

func (c *Component) identity() string {
	if c.fence == nil {
		return standaloneIdentity
	}
	return c.fence.Identity()
}

func (c *Component) nextRenderSeq() uint64 {
	return c.renderSeq.Add(1)
}

// podKey identifies one pod across a container restart: a new container is a
// new tree, and a new address is a new agent.
func podKey(endpoint *dataplane.Endpoint) string {
	return endpoint.PodUID + "\x00" + endpoint.PodRuntimeID + "\x00" + endpoint.URL
}

// invalidateBaseline makes this pod's next apply carry the complete file set
// and a reload. A refused apply may have left the pod somewhere the render
// plans cannot describe, so no ops may be composed against it.
func (c *Component) invalidateBaseline(endpoint *dataplane.Endpoint) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.invalidBaseline[podKey(endpoint)] = struct{}{}
}

func (c *Component) clearBaselineInvalidation(endpoint *dataplane.Endpoint) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	delete(c.invalidBaseline, podKey(endpoint))
}

func (c *Component) baselineInvalid(endpoint *dataplane.Endpoint) bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	_, invalid := c.invalidBaseline[podKey(endpoint)]
	return invalid
}

func (c *Component) clearBaselineInvalidations() {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	clear(c.invalidBaseline)
	clear(c.observedPlans)
}

// notePodPlans records the plans one pod reports it holds — its state read
// before the apply, then its ACK after it. Every pod that answered at all
// contributes, not only the ones that ACKed: a pod whose apply failed still
// runs its plans, and evicting them costs it a full-state reload on the retry
// seconds later.
func (c *Component) notePodPlans(endpoint *dataplane.Endpoint, proofs ...string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.observedPlans[podKey(endpoint)] = planProofs(proofs)
}

// fleetPlanRefs is every plan the fleet still refers to, forgetting the pods
// that are gone. A pod this deployment could not read keeps its last answer:
// a blip that failed every pod would otherwise evict the whole cache and
// reload the fleet on the next round.
func (c *Component) fleetPlanRefs(endpoints []dataplane.Endpoint) []planCacheKey {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	live := make(map[string]struct{}, len(endpoints))
	for i := range endpoints {
		live[podKey(&endpoints[i])] = struct{}{}
	}
	refs := make([]planCacheKey, 0, len(c.observedPlans)*3)
	for key, proofs := range c.observedPlans {
		if _, wanted := live[key]; !wanted {
			delete(c.observedPlans, key)
			continue
		}
		for _, proof := range proofs {
			refs = append(refs, planCacheKey{authority: key, proof: proof})
		}
	}
	return refs
}

// planProofs drops empty and repeated proofs.
func planProofs(proofs []string) []string {
	kept := make([]string, 0, len(proofs))
	for _, proof := range proofs {
		if proof != "" && !slices.Contains(kept, proof) {
			kept = append(kept, proof)
		}
	}
	return kept
}

// applyPosture decides how much of the desired state one pod gets, and what to
// tell an operator about it. A contract skew is never a refusal: a
// fleet-correlated refusal would fence the repair path.
func (c *Component) applyPosture(endpoint *dataplane.Endpoint, state *api.State) (full bool, notes []string) {
	majorMismatch, missingOps := agentclient.CheckSkew(state)
	if majorMismatch || len(missingOps) > 0 {
		if c.metrics != nil {
			c.metrics.RecordAgentVersionSkew()
		}
		c.Logger().Warn("Agent speaks a different contract; sending the complete state and a reload",
			"pod", endpoint.PodName,
			"agent_api_version", state.APIVersion,
			"controller_api_version", api.Version,
			"missing_ops", len(missingOps))
		notes = append(notes, skewReason(state, majorMismatch, missingOps))
		full = true
	}
	if c.baselineInvalid(endpoint) {
		notes = append(notes, "the previous apply was rejected, resending the complete state")
		full = true
	}
	if !slices.Contains(state.Features, api.FeatureWorkerFence) {
		notes = append(notes, "the agent cannot fence runtime updates across reloads, sending the complete state")
		full = true
	}
	if state.AppliedPlanID != "" && state.AppliedPlanProof == "" {
		notes = append(notes, "the agent cannot prove its applied plan, resending the complete state")
		full = true
	}
	return full, notes
}

func skewReason(state *api.State, majorMismatch bool, missingOps []string) string {
	if majorMismatch {
		return fmt.Sprintf("agent speaks API version %d, this controller version %d", state.APIVersion, api.Version)
	}
	return fmt.Sprintf("agent does not execute %d of the composed op kinds", len(missingOps))
}
