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
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	v1 "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/metrics"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercycle"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/timeouts"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderoutput"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/configpublisher"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

const retainedDeploymentReason = "retained_configuration"

var errRetainedPodConfigured = errors.New("retained configuration leaves an existing pod untouched")

// RetainedLoader reads complete, acknowledged snapshots without publishing.
type RetainedLoader interface {
	LoadRetained(context.Context, string, string, types.UID) ([]configpublisher.RetainedConfig, error)
}

// RetainedIntentStore fences writers and records output identity before apply.
type RetainedIntentStore interface {
	ClaimDeploymentAuthority(context.Context, *configpublisher.DeploymentAuthority) error
	RecordDeploymentIntent(context.Context, *configpublisher.DeploymentAuthority, string, string) error
	CheckDeploymentIntent(context.Context, *configpublisher.DeploymentAuthority, string, string) error
	CheckDeploymentAuthority(context.Context, *configpublisher.DeploymentAuthority) error
}

// RetainedStore provides snapshots and the durable deployment barrier.
type RetainedStore interface {
	RetainedLoader
	RetainedIntentStore
}

// RetainedChecker checks a complete snapshot with the current HAProxy binary.
type RetainedChecker interface {
	CheckOutput(context.Context, *renderoutput.Snapshot, string) error
}

type retainedRecovery struct {
	loader    RetainedLoader
	checker   RetainedChecker
	template  *v1.HAProxyTemplateConfig
	deployer  *Component
	metrics   *metrics.Metrics
	store     RetainedIntentStore
	authority atomic.Pointer[configpublisher.DeploymentAuthority]
	leaseName string
}

type retainedResult struct {
	epoch      uint64
	podSet     string
	occurrence *rendercycle.Occurrence
	checksum   string
	err        error
}

// EnableRetainedRecovery wires the leader's read, validation and bootstrap path.
func (s *DeployStack) EnableRetainedRecovery(store RetainedStore, checker RetainedChecker, template *v1.HAProxyTemplateConfig, domainMetrics *metrics.Metrics, leaseName string) {
	recovery := &retainedRecovery{loader: store, store: store, checker: checker, template: template.DeepCopy(), deployer: s.Deployer, metrics: domainMetrics, leaseName: leaseName}
	s.Scheduler.retained, s.Deployer.retained = recovery, recovery
}

func (c *Component) claimRetainedAuthority(ctx context.Context) error {
	if c.retained == nil {
		return nil
	}
	r := c.retained
	authority := configpublisher.DeploymentAuthority{Namespace: r.template.Namespace, Name: r.template.Name, UID: r.template.UID, Claim: rand.Text(), Standalone: c.fence == nil, LeaseName: r.leaseName, Identity: c.identity()}
	if err := wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		attemptCtx, cancel := context.WithTimeout(ctx, timeouts.KubernetesAPITimeout)
		defer cancel()
		if err := c.claimAuthority(attemptCtx, &authority); err != nil {
			c.Logger().Warn("Deployment authority is unavailable; HAProxy updates are paused", "error", err)
			return false, nil
		}
		return true, nil
	}); err != nil {
		return err
	}
	r.authority.Store(&authority)
	return nil
}

// claimAuthority lifts the epoch past an intent that outranks it the way an
// outranking pod does: Reclaim refuses unless the counter regressed.
func (c *Component) claimAuthority(ctx context.Context, authority *configpublisher.DeploymentAuthority) error {
	authority.Epoch = c.leaderEpoch()
	err := c.retained.store.ClaimDeploymentAuthority(ctx, authority)
	var ahead *configpublisher.IntentEpochAheadError
	if c.fence == nil || !errors.As(err, &ahead) {
		return err
	}
	if _, err := c.fence.Reclaim(ctx, ahead.Epoch); err != nil {
		return fmt.Errorf("%w: %w", ahead, err)
	}
	c.Logger().Warn("The leader epoch had regressed below the deployment intent, reclaimed it",
		"intent_epoch", ahead.Epoch, "epoch", c.leaderEpoch())
	authority.Epoch = c.leaderEpoch()
	return c.retained.store.ClaimDeploymentAuthority(ctx, authority)
}

func (c *Component) checkRetainedAuthority(ctx context.Context) error {
	if c.retained == nil || c.retained.store == nil {
		return nil
	}
	authority := c.retained.currentAuthority()
	if authority == nil {
		return errors.New("deployment authority is not claimed")
	}
	return c.retained.store.CheckDeploymentAuthority(ctx, authority)
}

func (c *Component) prepareRetainedDeployment(ctx context.Context, event *events.DeploymentScheduledEvent, req *deployRequest) {
	req.observeReload = event.Reason == pendingReloadFollowUpReason
	req.bootstrapOnly = event.Reason == retainedDeploymentReason
	if c.retained == nil {
		return
	}
	r := c.retained
	authority := r.currentAuthority()
	if authority == nil {
		req.preparationError = errors.New("deployment authority is not claimed")
		return
	}
	if !req.bootstrapOnly {
		req.preparationError = r.store.RecordDeploymentIntent(ctx, authority, req.planID, req.checksum)
		return
	}
	req.retainedGuard = func(ctx context.Context) error {
		if err := r.store.CheckDeploymentIntent(ctx, r.currentAuthority(), req.planID, req.checksum); err != nil {
			return err
		}
		states, _, unreachable := r.readFleet(ctx, event.Endpoints)
		if err := retainedFleetAgreement(req.plan, states, unreachable); err != nil {
			return err
		}
		return r.store.CheckDeploymentIntent(ctx, r.currentAuthority(), req.planID, req.checksum)
	}
}

func (s *DeploymentScheduler) beginRetainedTerm() {
	s.resetDeploymentTerm()
	s.retainedResults = make(chan retainedResult, 1)
	s.retainedEpoch++
	s.retainedRecovering = false
	s.retainedRetryAt = time.Time{}
	s.retainedRenderError = ""
	s.retainedNotice = ""
	s.setRetainedActive(false)
}

func (s *DeploymentScheduler) stopRetainedTerm() {
	s.cancelRetainedRecovery()
	s.retainedWorkers.Wait()
	s.setRetainedActive(false)
}

func (s *DeploymentScheduler) cancelRetainedRecovery() {
	s.retainedEpoch++
	if s.retainedCancel != nil {
		s.retainedCancel()
		s.retainedCancel = nil
	}
}

func (s *DeploymentScheduler) renderFailedForRetention(ctx context.Context, message string) {
	s.retainedRenderError = message
	s.maybeRecoverRetained(ctx)
}

func (s *DeploymentScheduler) maybeRecoverRetained(ctx context.Context) {
	if s.retained == nil || s.retainedRecovering || s.retainedRenderError == "" || time.Now().Before(s.retainedRetryAt) {
		return
	}
	s.mu.RLock()
	eligible := s.lastValidatedOccurrence == nil
	endpoints := slices.Clone(s.currentEndpoints)
	s.mu.RUnlock()
	if !eligible || len(endpoints) == 0 {
		return
	}
	recoveryCtx, cancel := context.WithTimeout(ctx, 2*timeouts.KubernetesAPILongTimeout)
	s.retainedCancel = cancel
	s.retainedRecovering = true
	epoch := s.retainedEpoch
	results := s.retainedResults
	s.retainedWorkers.Add(1)
	go func() {
		defer s.retainedWorkers.Done()
		defer cancel()
		result := s.retained.recover(recoveryCtx, endpoints)
		result.epoch, result.podSet = epoch, computePodSetHash(endpoints)
		select {
		case results <- result:
		case <-ctx.Done():
		}
	}()
}

func (s *DeploymentScheduler) handleRetainedResult(ctx context.Context, result retainedResult) {
	s.retainedRecovering = false
	s.retainedRetryAt = time.Now().Add(5 * time.Second)
	if result.epoch != s.retainedEpoch || s.retainedRenderError == "" {
		return
	}
	s.mu.RLock()
	endpoints := slices.Clone(s.currentEndpoints)
	eligible := s.lastValidatedOccurrence == nil && computePodSetHash(endpoints) == result.podSet
	s.mu.RUnlock()
	if !eligible {
		return
	}
	if result.err != nil {
		s.reportRetained("RetainedConfigUnavailable", fmt.Sprintf("Retained configuration cannot start new HAProxy pods: %v. Repair the render inputs or restore the published checkpoint.", result.err))
		return
	}
	if result.occurrence == nil {
		return
	}
	s.retained.deployer.SetValidatedOccurrence(result.occurrence)
	s.setRetainedActive(true)
	s.reportRetained("RetainedConfigActive", fmt.Sprintf("New HAProxy pods use retained configuration %s because rendering failed: %s. Repair the render inputs to resume updates.", result.checksum, s.retainedRenderError))
	s.scheduleOrQueueOccurrence(ctx, result.occurrence, endpoints, retainedDeploymentReason, "retained-configuration", false)
}

func (s *DeploymentScheduler) reportRetained(reason, message string) {
	if s.retained == nil || s.retainedNotice == reason+message {
		return
	}
	s.retainedNotice = reason + message
	s.logger.Warn(message, "reason", reason)
	template := s.retained.template
	s.eventBus.Publish(events.NewRetainedConfigEvent(template.Namespace, template.Name, string(template.UID), reason, message))
}

func (s *DeploymentScheduler) setRetainedActive(active bool) {
	if s.retained == nil || s.retained.metrics == nil {
		return
	}
	value := float64(0)
	if active {
		value = 1
	}
	s.retained.metrics.RetainedConfigActive.Set(value)
}

func (r *retainedRecovery) recover(ctx context.Context, endpoints []dataplane.Endpoint) retainedResult {
	states, bootstrap, unreachable := r.readFleet(ctx, endpoints)
	if bootstrap == 0 {
		return retainedResult{}
	}
	configs, err := r.loader.LoadRetained(ctx, r.template.Namespace, r.template.Name, r.template.UID)
	if err != nil {
		return retainedResult{err: err}
	}
	configs, err = r.currentRetained(ctx, configs)
	if err != nil {
		return retainedResult{err: err}
	}
	config, err := selectRetained(configs, states, unreachable)
	if err != nil {
		return retainedResult{err: err}
	}
	if err := r.checker.CheckOutput(ctx, config.Output, config.Reference.Checksum); err != nil {
		return retainedResult{err: fmt.Errorf("current HAProxy rejects checkpoint %s: %w", config.Reference.Checksum, err)}
	}
	occurrence, err := retainedOccurrence(config)
	return retainedResult{occurrence: occurrence, checksum: config.Reference.Checksum, err: err}
}

func (r *retainedRecovery) currentAuthority() *configpublisher.DeploymentAuthority {
	saved := r.authority.Load()
	if saved == nil {
		return nil
	}
	current := *saved
	current.Epoch = r.deployer.leaderEpoch()
	return &current
}

func (r *retainedRecovery) currentRetained(ctx context.Context, configs []configpublisher.RetainedConfig) ([]configpublisher.RetainedConfig, error) {
	if r.store == nil {
		return configs, nil
	}
	authority := r.currentAuthority()
	if authority == nil {
		return nil, errors.New("deployment authority is not claimed")
	}
	failure := errors.New("no acknowledged checkpoint matches the latest deployment intent")
	for i := range configs {
		planID, err := configs[i].Output.PlanID()
		if err != nil {
			return nil, err
		}
		if err := r.store.CheckDeploymentIntent(ctx, authority, planID, configs[i].Reference.Checksum); err != nil {
			failure = err
			continue
		}
		return configs[i : i+1], nil
	}
	return nil, failure
}

func (r *retainedRecovery) readFleet(ctx context.Context, endpoints []dataplane.Endpoint) (states []*api.State, bootstrap, unreachable int) {
	for i := range endpoints {
		client, err := r.deployer.clients.For(&endpoints[i])
		if err != nil {
			unreachable++
			continue
		}
		readCtx, cancel := context.WithTimeout(ctx, timeouts.KubernetesAPITimeout)
		state, err := client.State(readCtx, api.StateRead{Verify: true})
		cancel()
		if err != nil {
			unreachable++
			continue
		}
		if bootstrapState(state) {
			bootstrap++
		} else {
			states = append(states, state)
		}
	}
	return states, bootstrap, unreachable
}

func bootstrapState(state *api.State) bool {
	return state != nil && state.AppliedPlanID == "" && state.RunningPlanID == "" &&
		state.WorkerOpsPlanID == "" && state.LKGPlanID == "" && !state.HoldsAppliedPlan() &&
		state.AppliedToken == (api.Token{}) && state.InvariantViolation == ""
}

func selectRetained(configs []configpublisher.RetainedConfig, states []*api.State, unreachable int) (*configpublisher.RetainedConfig, error) {
	if len(configs) == 0 {
		return nil, errors.New("no acknowledged retained configuration is available")
	}
	snapshot, err := configs[0].Output.PlanSnapshot()
	if err != nil {
		return nil, err
	}
	plan, err := snapshot.SharedPlan()
	if err != nil {
		return nil, err
	}
	if err := retainedFleetAgreement(plan, states, unreachable); err != nil {
		return nil, err
	}
	return &configs[0], nil
}

func retainedFleetAgreement(plan *renderplan.Plan, states []*api.State, unreachable int) error {
	if unreachable > 0 {
		return errors.New("fleet agreement is unknown while an HAProxy pod is unreachable")
	}
	for _, state := range states {
		if !stateConfirmsRetained(state, plan) {
			return errors.New("an existing HAProxy pod reports different or newer configuration; retained configuration is discarded")
		}
	}
	return nil
}

func stateConfirmsRetained(state *api.State, plan *renderplan.Plan) bool {
	return state != nil && state.HAProxy.HasWorkerIdentity() && state.InvariantViolation == "" &&
		state.ReloadPendingAt == "" && state.AppliedPlanID == plan.ID && state.AppliedPlanProof != "" &&
		((state.RunningPlanID == plan.ID && state.RunningPlanProof != "") || (state.WorkerOpsPlanID == plan.ID && state.WorkerOpsPlanProof != "")) &&
		len(state.Files) == len(plan.Files) && measuredHoldsPlan(state, plan)
}

func retainedOccurrence(config *configpublisher.RetainedConfig) (*rendercycle.Occurrence, error) {
	authority, err := rendercycle.NewAuthority(config.Authority)
	if err != nil {
		return nil, err
	}
	status, err := templating.NewStatusPatchCollector().Snapshot()
	if err != nil {
		return nil, err
	}
	eventSnapshot, err := templating.NewEventCollector().Snapshot()
	if err != nil {
		return nil, err
	}
	resources, err := templating.NewRenderedResourceCollector().Snapshot()
	if err != nil {
		return nil, err
	}
	cycle, err := rendercycle.NewSnapshot(authority, config.Output, status, eventSnapshot, resources, nil)
	if err != nil {
		return nil, err
	}
	return rendercycle.NewOccurrence(cycle)
}

func (attempt *podApply) checkRetainedDispatch(ctx context.Context) error {
	if attempt.req.bootstrapOnly && !bootstrapState(attempt.state) {
		return errRetainedPodConfigured
	}
	if attempt.req.retainedGuard != nil {
		return attempt.req.retainedGuard(ctx)
	}
	return nil
}

// A cold term must not supersede its checkpoint with an output HAProxy refuses.
func (s *DeploymentScheduler) awaitingFirstAcceptedRenderLocked() bool {
	return s.retained != nil && s.acceptedRender == nil
}
