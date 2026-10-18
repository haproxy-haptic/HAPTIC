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
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	v1 "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercycle"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/deployplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/planblob"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
)

// deployRequest is one deployment's desired state, identical for every pod.
type deployRequest struct {
	occurrence      *rendercycle.Occurrence
	identity        *renderOccurrenceIdentity // the occurrence materialized once, shared by every pod
	plan            *renderplan.Plan
	planIndex       sync.Once
	index           *deployplan.Index // plan's index, built by the first diff
	planID          string
	occurrenceProof string
	checksum        string
	contents        map[string]string // file content by manifest path
	patches         *patchMemo        // splices into what the pods hold, one per base
	blob            *planBlob         // the plan, zstd-compressed, as the agent stores it
	token           api.Token
	// baselineIsPlan memoizes, per pod baseline, whether it already is this
	// plan: the answer is the same for every pod on that baseline.
	baselineIsPlan sync.Map
	// validatedPlanFor answers, per pod, which passed plan its manifest should
	// name — the pod's own applied plan when that one passed.
	validatedPlanFor func(authority string, state *api.State) planReference
	// verify makes each pod re-hash its tree before it reports: the drift pass
	// asks what is on disk, not what the agent last wrote.
	verify           bool
	observeReload    bool
	bootstrapOnly    bool
	preparationError error
	retainedGuard    func(context.Context) error
	diffs            *diffMemo
}

// performDeployment executes a single deployment.
//
// This method is called from HandleEvent for each dispatched
// DeploymentScheduledEvent. "Latest wins" coalescing of pending coalescible
// DeploymentScheduledEvents is provided by the embedded component.Base via
// the CoalescesOn hook (see component.go).
//
// Defensive: drops duplicate events if a deployment is already in progress.
func (c *Component) performDeployment(ctx context.Context, event *events.DeploymentScheduledEvent) {
	c.healthTracker.StartProcessing()
	defer c.healthTracker.EndProcessing()

	correlationID := event.CorrelationID()
	deploymentID := event.EventID()

	// Defensive check: atomically set deploymentInProgress from false to true.
	// This prevents concurrent deployments if the scheduler has bugs.
	if !c.deploymentInProgress.CompareAndSwap(false, true) {
		c.Logger().Error("Dropping duplicate DeploymentScheduledEvent - deployment already in progress",
			"reason", event.Reason,
			"endpoint_count", len(event.Endpoints),
			"correlation_id", correlationID)
		return
	}

	deployCtx, cancel, begun := c.beginDeployment(ctx, deploymentID, correlationID)
	if !begun {
		c.deploymentInProgress.Store(false)
		c.Logger().Info("Skipping deployment: the leadership term is draining",
			"deployment_id", deploymentID, "correlation_id", correlationID)
		return
	}
	defer cancel()

	defer func() {
		c.cancelMu.Lock()
		c.activeDeploymentID = ""
		c.activeCorrelationID = ""
		c.activeCancelFunc = nil
		if c.deploymentDone != nil {
			close(c.deploymentDone)
			c.deploymentDone = nil
		}
		c.cancelMu.Unlock()
	}()

	c.Logger().Debug("Deployment scheduled, starting execution",
		"reason", event.Reason,
		"endpoint_count", len(event.Endpoints),
		"config_bytes", deploymentConfigBytes(event),
		"plan", deploymentPlanID(event),
		"deployment_id", deploymentID,
		"correlation_id", correlationID)

	c.deployToEndpoints(deployCtx, cancel, event, deploymentID)
}

// deployToEndpoints applies the render to every pod, at most maxConcurrentPods
// at a time, and reports the fleet's answer.
func (c *Component) deployToEndpoints(
	ctx context.Context,
	standDown context.CancelFunc,
	event *events.DeploymentScheduledEvent,
	deploymentID string,
) {
	defer c.deploymentInProgress.Store(false)

	startTime := time.Now()
	correlationID := event.CorrelationID()
	occurrence, err := scheduledEventOccurrence(event)
	if err != nil {
		c.Logger().Error("Scheduled deployment has no exact output", "error", err)
		c.reportUndeployable(event, deploymentID, nil)
		return
	}
	identity, err := materializeOccurrence(occurrence)
	if err != nil {
		c.Logger().Error("Scheduled deployment has no exact output", "error", err)
		c.reportUndeployable(event, deploymentID, nil)
		return
	}
	if len(event.Endpoints) == 0 {
		c.reportUndeployable(event, deploymentID, occurrence)
		return
	}
	plan := identity.plan
	if plan == nil {
		c.reportUndeployable(event, deploymentID, occurrence)
		return
	}

	// contentChecksum covers config plus auxiliary files, so it is the value
	// HAProxyCfg.spec.Checksum is comparable to. Plan ids do not replace it:
	// aux bytes outside the plan still ride it.
	podSetHash := computePodSetHash(event.Endpoints)
	request := c.newDeployRequest(
		occurrence, &identity, event.Reason == events.TriggerReasonDriftPrevention,
	)
	if request == nil {
		c.reportUndeployable(event, deploymentID, occurrence)
		return
	}
	c.prepareRetainedDeployment(ctx, event, request)
	c.recordFleet(event.Endpoints)

	c.EventBus().Publish(events.NewDeploymentStartedEvent(
		len(event.Endpoints),
		events.WithCorrelation(correlationID, deploymentID),
	))

	state := &deploymentState{standDown: standDown, operationBreakdown: map[string]int{}}
	prepared := time.Now()
	var wg sync.WaitGroup
	slots := make(chan struct{}, maxConcurrentPods)
	for i := range event.Endpoints {
		wg.Add(1)
		go func(endpoint *dataplane.Endpoint) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			c.deployToPod(ctx, endpoint, request, event, state)
		}(&event.Endpoints[i])
	}
	wg.Wait()
	settling := time.Now()

	c.plans.Retain(c.fleetPlanRefs(event.Endpoints))
	c.clients.Retain(event.Endpoints)
	c.recordFleetAck(plan, atomic.LoadInt32(&state.ackCount))

	c.Logger().Debug("Deployment completed",
		"total_endpoints", len(event.Endpoints),
		"converged", atomic.LoadInt32(&state.convergedCount),
		"failed", atomic.LoadInt32(&state.failureCount),
		"reloads_triggered", atomic.LoadInt32(&state.reloadsTriggered),
		"duration_ms", time.Since(startTime).Milliseconds(),
		"correlation_id", correlationID)

	state.noteDeploymentPhases(prepared.Sub(startTime), time.Since(settling))
	c.publishCompleted(event, deploymentID, podSetHash, state, time.Since(startTime).Milliseconds(), occurrence)
	c.publishDeployedConfig(event, occurrence, int(atomic.LoadInt32(&state.ackCount)), state.confirmedPod)
	c.observeConvergence(event, podSetHash, state, occurrence)
}

// reportUndeployable completes a deployment that cannot be executed: a render
// without a plan carries no file set, and a fleet without pods has no target.
// Both report through the normal completion so the scheduler is never wedged.
func (c *Component) reportUndeployable(
	event *events.DeploymentScheduledEvent,
	deploymentID string,
	occurrence *rendercycle.Occurrence,
) {
	if occurrence == nil {
		c.Logger().Error("Render carries no authenticated occurrence",
			"endpoint_count", len(event.Endpoints),
			"correlation_id", event.CorrelationID())
		return
	}
	result, err := events.NewDeploymentResultWithOccurrence(occurrence)
	if err != nil {
		c.Logger().Error("Refusing to publish an unauthenticated deployment completion", "error", err)
		return
	}
	result.DeploymentID = deploymentID
	if len(event.Endpoints) > 0 {
		c.Logger().Error("Render carries no plan; the agent needs one to apply it",
			"endpoint_count", len(event.Endpoints),
			"correlation_id", event.CorrelationID())
		result.Total = len(event.Endpoints)
		result.Failed = len(event.Endpoints)
	} else {
		c.Logger().Error("No valid endpoints to deploy to")
	}
	completed, err := events.NewDeploymentCompletedEventWithCycle(result,
		events.WithCorrelation(event.CorrelationID(), deploymentID))
	if err != nil {
		c.Logger().Error("Refusing to publish an unauthenticated deployment completion", "error", err)
		return
	}
	c.EventBus().Publish(completed)
}

func (c *Component) newDeployRequest(
	occurrence *rendercycle.Occurrence,
	identity *renderOccurrenceIdentity,
	verify bool,
) *deployRequest {
	if !sameOccurrence(occurrence, identity.occurrence) {
		return nil
	}
	return &deployRequest{
		occurrence:       occurrence,
		identity:         identity,
		plan:             identity.plan,
		planID:           identity.plan.ID,
		occurrenceProof:  identity.proof,
		checksum:         identity.checksum,
		contents:         contentsByPath(identity.plan),
		patches:          newPatchMemo(),
		blob:             encodePlanBlob(identity, c.Logger()),
		token:            api.Token{LeaderEpoch: c.leaderEpoch(), RenderSeq: c.nextRenderSeq()},
		validatedPlanFor: c.validatedPlanFor,
		verify:           verify,
		diffs:            newDiffMemo(),
	}
}

// planBlob is the plan's blob, encoded on first use: the keeper asks for it
// after the apply, off the deploy's critical path, and a plan the pods have
// moved past is never encoded at all. At 5,000 routes the encode is 35 ms.
type planBlob struct {
	identity *renderOccurrenceIdentity
	logger   *slog.Logger
	once     sync.Once
	blob     []byte
}

func encodePlanBlob(identity *renderOccurrenceIdentity, logger *slog.Logger) *planBlob {
	return &planBlob{identity: identity, logger: logger}
}

// bytes encodes on first call; nil when the encode failed.
func (b *planBlob) bytes() []byte {
	if b == nil {
		return nil
	}
	b.once.Do(func() {
		blob, err := planblob.EncodeSnapshot(b.identity.planSnapshot)
		if err != nil {
			// Without the blob a pod that outlives this controller reports a
			// baseline nobody can decode, which costs it one reload — never
			// correctness, so the deployment goes ahead.
			b.logger.Error("Encoding the plan blob failed; pods will not retain this baseline",
				"plan", b.identity.plan.ID, "error", err)
			return
		}
		b.blob = blob
	})
	return b.blob
}

// isPlan reports whether a pod's baseline already is this plan, once per
// baseline rather than once per pod.
func (r *deployRequest) isPlan(baseline *renderplan.Plan) bool {
	if baseline == nil {
		return false
	}
	if known, ok := r.baselineIsPlan.Load(baseline); ok {
		return known.(bool)
	}
	same := exactPlan(baseline, r.plan)
	r.baselineIsPlan.Store(baseline, same)
	return same
}

func contentsByPath(plan *renderplan.Plan) map[string]string {
	if plan == nil {
		return nil
	}
	contents := make(map[string]string, len(plan.Files))
	for i := range plan.Files {
		file := &plan.Files[i]
		if file.ContentKnown {
			contents[file.Path] = file.Content
		}
	}
	return contents
}

// recordFleetAck hands the renderer the plan the fleet now runs — one pod that
// took it is enough, the rest converge on the same render. Called directly, not
// over the bus: the next render must read it (ADR-0001).
func (c *Component) recordFleetAck(plan *renderplan.Plan, acked int32) {
	if plan == nil || c.ackedPlans == nil || acked == 0 {
		return
	}
	c.ackedPlans.SetAckedPlan(plan)
}

// deploymentState aggregates what the pods answered. Counts are atomic; the
// mutex guards the maps and slices.
type deploymentState struct {
	standDown context.CancelFunc

	ackCount         int32 // pods that accepted the apply
	convergedCount   int32 // pods now running the render
	failureCount     int32
	reloadsTriggered int32
	pendingReloads   int32 // pods holding the render behind a paced reload

	mu                 sync.Mutex
	confirmedPod       *v1.PodDeploymentStatus
	totalOperations    int
	operationBreakdown map[string]int
	stoodDown          bool
	pendingReloadUntil time.Time
	running            map[string]runningRender // pod → exact render its worker runs, from its ACK
	slowest            *events.DeployPhases     // the accepted pod whose apply took longest
}

// noteDeploymentPhases records what the deployment spent around the pods.
func (s *deploymentState) noteDeploymentPhases(prepare, settle time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.slowest != nil {
		s.slowest.PrepareMs = prepare.Milliseconds()
		s.slowest.SettleMs = settle.Milliseconds()
	}
}

// notePhases keeps the split of the slowest accepted pod.
func (s *deploymentState) notePhases(phases *events.DeployPhases, durationMs int64) {
	kept := *phases
	kept.TotalMs = durationMs
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.slowest == nil || durationMs > s.slowest.TotalMs {
		s.slowest = &kept
	}
}

type runningRender struct {
	plan       *renderplan.Plan
	occurrence *rendercycle.Occurrence
}

// noteRunning records which plan a pod's worker runs after its apply.
func (s *deploymentState) noteRunning(endpoint *dataplane.Endpoint, running runningRender) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running == nil {
		s.running = map[string]runningRender{}
	}
	if !exactPlan(running.plan, running.plan) {
		s.running[podKey(endpoint)] = runningRender{}
		return
	}
	s.running[podKey(endpoint)] = running
}

// fleetRunningRender is the render every pod reported running.
func (s *deploymentState) fleetRunningRender(total int) runningRender {
	s.mu.Lock()
	defer s.mu.Unlock()
	if total == 0 || len(s.running) != total {
		return runningRender{}
	}
	var consensus runningRender
	for _, running := range s.running {
		switch {
		case !exactPlan(running.plan, running.plan):
			return runningRender{}
		case consensus.plan == nil:
			consensus = running
		case consensus.occurrence != nil || running.occurrence != nil:
			if !sameOccurrence(consensus.occurrence, running.occurrence) {
				return runningRender{}
			}
		case !exactPlan(consensus.plan, running.plan):
			return runningRender{}
		}
	}
	return consensus
}

// notePendingReload records a pod that scheduled its reload for later.
func (s *deploymentState) notePendingReload(scheduledAt string) {
	atomic.AddInt32(&s.pendingReloads, 1)
	due, err := time.Parse(time.RFC3339, scheduledAt)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if due.After(s.pendingReloadUntil) {
		s.pendingReloadUntil = due
	}
}

// deployToPod applies the render to one pod and folds its answer into state.
func (c *Component) deployToPod(
	ctx context.Context,
	endpoint *dataplane.Endpoint,
	request *deployRequest,
	event *events.DeploymentScheduledEvent,
	state *deploymentState,
) {
	if ctx.Err() != nil {
		c.abandonEndpoint(ctx, endpoint, state)
		return
	}

	start := time.Now()
	outcome, err := c.applyToPod(ctx, endpoint, request)
	durationMs := time.Since(start).Milliseconds()

	switch {
	case errors.Is(err, errRetainedPodConfigured):
		atomic.AddInt32(&state.convergedCount, 1)
	case errors.Is(err, errStaleEpoch):
		c.standDown(state, endpoint, err)
	case superseded(ctx, err):
		c.abandonEndpoint(ctx, endpoint, state)
	case err != nil:
		c.handleEndpointFailure(endpoint, err, durationMs, event, state, request)
	// An apply that reports an error did not do what it was asked, whether or
	// not the agent called the transaction a success: a refused in-place batch
	// answers OK with an error and no applied plan.
	case !outcome.result.OK || outcome.result.Error != nil:
		c.handleEndpointRejection(endpoint, outcome, event, state, request)
	default:
		c.handleEndpointSuccess(endpoint, outcome, durationMs, event, state, request)
	}
}

// abandonEndpoint counts a pod the cancelled deploy did not bring to the
// render; the cancellation itself is logged where it is requested.
func (c *Component) abandonEndpoint(ctx context.Context, endpoint *dataplane.Endpoint, state *deploymentState) {
	c.Logger().Debug("Endpoint deployment cancelled", "pod", endpoint.PodName, "cause", context.Cause(ctx))
	atomic.AddInt32(&state.failureCount, 1)
}

// standDown stops dispatching: another controller holds a higher leader epoch,
// so this one is not the fleet's writer any more. Losing the epoch race is
// losing leadership, and it is given up for real — a replica that only stopped
// dispatching keeps renewing its Lease, and nothing would ever re-arm it.
func (c *Component) standDown(state *deploymentState, endpoint *dataplane.Endpoint, err error) {
	atomic.AddInt32(&state.failureCount, 1)

	state.mu.Lock()
	first := !state.stoodDown
	state.stoodDown = true
	state.mu.Unlock()
	if !first {
		return
	}

	c.Logger().Error("A newer leader epoch owns the fleet, standing down",
		"pod", endpoint.PodName, "identity", c.identity(), "error", err)
	state.standDown()
	if c.fence == nil {
		// No Lease to release: the leadership this reports is nominal, and the
		// event is all the leader-only components have to stop on.
		c.EventBus().Publish(events.NewLostLeadershipEvent(standaloneIdentity, "stale_leader_epoch"))
		return
	}
	c.fence.StandDown("stale_leader_epoch")
}
