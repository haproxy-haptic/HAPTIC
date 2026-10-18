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
	"testing"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/metrics"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	v1 "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/agenttest"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderartifact"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderoutput"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/configpublisher"
)

type retainedTestLoader struct {
	configs []configpublisher.RetainedConfig
	err     error
}

func (l retainedTestLoader) LoadRetained(context.Context, string, string, types.UID) ([]configpublisher.RetainedConfig, error) {
	return l.configs, l.err
}

type retainedTestChecker struct {
	calls int
	err   error
}

func (c *retainedTestChecker) CheckOutput(context.Context, *renderoutput.Snapshot, string) error {
	c.calls++
	return c.err
}

func retainedTestConfig(t *testing.T, address string) (configpublisher.RetainedConfig, *renderplan.Plan) {
	t.Helper()
	plan, config, aux := renderFor("retained", address, mapEntry)
	plan.ComputeID()
	artifactAuthority := renderartifact.NewAuthority()
	artifacts, err := dataplane.BuildAuxiliaryFileSnapshot(artifactAuthority, nil, aux)
	require.NoError(t, err)
	authority, err := renderoutput.NewAuthority(renderplan.NewAuthority(), artifactAuthority)
	require.NoError(t, err)
	output, err := renderoutput.NewSnapshot(authority, config, plan, artifacts, nil)
	require.NoError(t, err)
	checksum, err := output.ContentChecksum()
	require.NoError(t, err)
	return configpublisher.RetainedConfig{Output: output, Authority: authority, Reference: v1.RetainedConfigReference{Checksum: checksum}}, plan
}

func measuredRetainedState(plan *renderplan.Plan) *api.State {
	state := &api.State{AppliedPlanID: plan.ID, AppliedPlanProof: "applied", RunningPlanID: plan.ID, RunningPlanProof: "running", HAProxy: api.HAProxyInfo{WorkerPID: 1, WorkerStartTimeUnixMicros: 1}, Files: map[string]api.FileAt{}}
	for _, file := range plan.Files {
		state.Files[file.Path] = api.FileAt{Digest: file.Digest, Size: file.Size}
	}
	return state
}

func TestRetainedSelectionUsesNewestConfirmedRunningCheckpoint(t *testing.T) {
	newer, newPlan := retainedTestConfig(t, "10.0.0.2")
	older, oldPlan := retainedTestConfig(t, "10.0.0.1")
	configs := []configpublisher.RetainedConfig{newer, older}
	for _, tc := range []struct {
		name        string
		states      []*api.State
		unreachable int
		want        string
	}{
		{"agreement", []*api.State{measuredRetainedState(newPlan)}, 0, newer.Reference.Checksum},
		{"partial rollout", []*api.State{measuredRetainedState(oldPlan), measuredRetainedState(newPlan)}, 0, ""},
		{"only old survived", []*api.State{measuredRetainedState(oldPlan)}, 0, ""},
		{"one matching and one unreachable", []*api.State{measuredRetainedState(newPlan)}, 1, ""},
		{"entire fleet replaced", nil, 0, newer.Reference.Checksum},
		{"unreachable fleet", nil, 1, ""},
		{"unknown state", []*api.State{{AppliedPlanID: "unknown"}}, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectRetained(configs, tc.states, tc.unreachable)
			if tc.want == "" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.Reference.Checksum)
		})
	}
}

func TestRetainedAgreementRejectsUnprovenAgentState(t *testing.T) {
	_, plan := retainedTestConfig(t, "10.0.0.1")
	for _, tc := range []struct {
		name   string
		mutate func(*api.State)
	}{
		{"content mismatch", func(s *api.State) { s.Files["haproxy.cfg"] = api.FileAt{Digest: "forged", Size: 1} }},
		{"missing file", func(s *api.State) { delete(s.Files, "haproxy.cfg") }},
		{"unloaded plan", func(s *api.State) { s.RunningPlanID = "other" }},
		{"pending reload", func(s *api.State) { s.ReloadPendingAt = "pending" }},
		{"unknown worker", func(s *api.State) { s.HAProxy.WorkerPID = 0 }},
		{"invariant failure", func(s *api.State) { s.InvariantViolation = "bad state" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := measuredRetainedState(plan)
			tc.mutate(state)
			assert.False(t, stateConfirmsRetained(state, plan))
		})
	}
}

func TestRetainedRecoveryValidatesBeforeScheduling(t *testing.T) {
	bus := newTestBus(t)
	agent := agenttest.New(t)
	config, _ := retainedTestConfig(t, "10.0.0.1")
	checker := &retainedTestChecker{err: errors.New("unsupported directive with current HAProxy")}
	r := &retainedRecovery{loader: retainedTestLoader{configs: []configpublisher.RetainedConfig{config}}, checker: checker, template: &v1.HAProxyTemplateConfig{}, deployer: createTestDeployer(bus.EventBus)}
	result := r.recover(t.Context(), []dataplane.Endpoint{agentEndpoint(agent, "new")})
	require.ErrorContains(t, result.err, "current HAProxy rejects")
	assert.Nil(t, result.occurrence)
	assert.Equal(t, 1, checker.calls)
	assert.Empty(t, agent.Applies())
	checker.err = nil
	result = r.recover(t.Context(), []dataplane.Endpoint{agentEndpoint(agent, "new")})
	require.NoError(t, result.err)
	require.NotNil(t, result.occurrence)
	assert.Empty(t, agent.Applies(), "only the scheduler can dispatch the validated occurrence")
}

func TestRetainedDeploymentLeavesRunningPodsUntouched(t *testing.T) {
	bus := newTestBus(t)
	old := agenttest.New(t)
	fresh := agenttest.New(t)
	plan, config, aux := renderFor("retained", "10.0.0.1", mapEntry)
	warm := createTestDeployer(bus.EventBus)
	deployTo(t, warm, bus, plan, config, aux, "config_validation", agentEndpoint(old, "old"))
	oldApplies := len(old.Applies())
	oldState := old.State()
	cold := createTestDeployer(bus.EventBus)
	result := deployTo(t, cold, bus, plan, config, aux, retainedDeploymentReason, agentEndpoint(old, "old"), agentEndpoint(fresh, "new"))
	assert.Zero(t, result.Failed)
	assert.Equal(t, oldApplies, len(old.Applies()))
	assert.Equal(t, oldState.AppliedPlanID, old.State().AppliedPlanID)
	assert.Equal(t, oldState.AppliedToken, old.State().AppliedToken)
	require.Len(t, fresh.Applies(), 1)
	assert.Equal(t, plan.ID, fresh.State().RunningPlanID)
	deployTo(t, cold, bus, plan, config, aux, retainedDeploymentReason, agentEndpoint(old, "old"), agentEndpoint(fresh, "new"))
	assert.Equal(t, oldApplies, len(old.Applies()))
	assert.Len(t, fresh.Applies(), 1, "a recovery retry must leave the newly configured pod untouched")
}

func TestRetainedBootstrapGuardIsRecheckedBeforeApply(t *testing.T) {
	bus := newTestBus(t)
	agent := agenttest.New(t)
	plan, config, aux := renderFor("retained", "10.0.0.1", mapEntry)
	c := createTestDeployer(bus.EventBus)
	deployTo(t, c, bus, plan, config, aux, "config_validation", agentEndpoint(agent, "pod"))
	state := agent.State()
	outcome, err := c.applyOnce(t.Context(), &podApply{req: &deployRequest{bootstrapOnly: true}, state: &state})
	require.ErrorIs(t, err, errRetainedPodConfigured)
	assert.Nil(t, outcome)
	assert.Len(t, agent.Applies(), 1)
}

type retainedTestIntent struct {
	planID       string
	checksum     string
	recordErr    error
	authorityErr error
	afterCheck   func()
}

func (*retainedTestIntent) ClaimDeploymentAuthority(context.Context, *configpublisher.DeploymentAuthority) error {
	return nil
}
func (s *retainedTestIntent) CheckDeploymentAuthority(context.Context, *configpublisher.DeploymentAuthority) error {
	return s.authorityErr
}
func (s *retainedTestIntent) RecordDeploymentIntent(_ context.Context, _ *configpublisher.DeploymentAuthority, planID, checksum string) error {
	if s.recordErr != nil {
		return s.recordErr
	}
	s.planID, s.checksum = planID, checksum
	return nil
}
func (s *retainedTestIntent) CheckDeploymentIntent(_ context.Context, _ *configpublisher.DeploymentAuthority, planID, checksum string) error {
	if s.planID != planID || s.checksum != checksum {
		return errors.New("newer deployment intent supersedes checkpoint")
	}
	if s.afterCheck != nil {
		s.afterCheck()
	}
	return nil
}

func installRetainedTestIntent(c *Component, store RetainedIntentStore) *retainedRecovery {
	recovery := &retainedRecovery{store: store, deployer: c}
	recovery.authority.Store(&configpublisher.DeploymentAuthority{Claim: "current"})
	c.retained = recovery
	return recovery
}

// claimIntentAhead holds an intent claimed at epoch ahead, the way the
// retained-state Lease outlives a recreated leader Lease.
type claimIntentAhead struct {
	retainedTestIntent
	ahead  uint64
	claims []uint64
}

func (s *claimIntentAhead) ClaimDeploymentAuthority(_ context.Context, authority *configpublisher.DeploymentAuthority) error {
	s.claims = append(s.claims, authority.Epoch)
	if authority.Epoch <= s.ahead {
		return &configpublisher.IntentEpochAheadError{Epoch: s.ahead}
	}
	return nil
}

func claimingDeployer(fence *fixedFence, store RetainedIntentStore) *Component {
	c := createTestDeployer(testutil.NewTestBus())
	c.fence = fence
	template := &v1.HAProxyTemplateConfig{}
	template.Name, template.Namespace, template.UID = "haptic", "haptic", "template-uid"
	c.retained = &retainedRecovery{store: store, deployer: c, template: template, leaseName: "haptic"}
	return c
}

// An intent that outranks the term's epoch is lifted past the way an
// outranking pod is, and the claim then lands at the new epoch.
func TestClaimLiftsTheEpochPastAnOutrankingDeploymentIntent(t *testing.T) {
	fence := &fixedFence{epoch: 1}
	store := &claimIntentAhead{ahead: 5}
	c := claimingDeployer(fence, store)

	require.NoError(t, c.claimRetainedAuthority(t.Context()))

	assert.Equal(t, []uint64{5}, fence.reclaims())
	assert.Equal(t, []uint64{1, 6}, store.claims)
	assert.Equal(t, uint64(6), c.retained.currentAuthority().Epoch)
}

// When a newer leader owns the Lease, Reclaim refuses and the term claims nothing.
func TestClaimStaysRefusedWhileANewerLeaderOwnsTheIntent(t *testing.T) {
	fence := &fixedFence{epoch: 1, reclaimErr: errors.New("the lease is held at a newer epoch")}
	store := &claimIntentAhead{ahead: 5}
	c := claimingDeployer(fence, store)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	require.Error(t, c.claimRetainedAuthority(ctx))

	assert.Nil(t, c.retained.currentAuthority())
	assert.Equal(t, []uint64{1}, store.claims)
}

func TestRetainedCheckpointFollowsIntentNotAcknowledgementOrder(t *testing.T) {
	bus := newTestBus(t)
	older, olderPlan := retainedTestConfig(t, "10.0.0.1")
	newer, newerPlan := retainedTestConfig(t, "10.0.0.2")
	store := &retainedTestIntent{planID: newerPlan.ID, checksum: newer.Reference.Checksum}
	recovery := installRetainedTestIntent(createTestDeployer(bus.EventBus), store)
	candidates, err := recovery.currentRetained(t.Context(), []configpublisher.RetainedConfig{older, newer})
	require.NoError(t, err)
	selected, err := selectRetained(candidates, []*api.State{measuredRetainedState(newerPlan)}, 0)
	require.NoError(t, err)
	assert.Equal(t, newer.Reference.Checksum, selected.Reference.Checksum)
	_, err = selectRetained(candidates, []*api.State{measuredRetainedState(olderPlan)}, 0)
	require.Error(t, err)
	_, err = recovery.currentRetained(t.Context(), []configpublisher.RetainedConfig{older})
	require.ErrorContains(t, err, "newer deployment intent")
}

func TestRetainedDispatchRechecksFreshness(t *testing.T) {
	for _, cause := range []string{"newer intent before acknowledgement", "running pod advanced", "intent advances during fleet agreement"} {
		t.Run(cause, func(t *testing.T) {
			bus := newTestBus(t)
			old, fresh := agenttest.New(t), agenttest.New(t)
			retained, plan := retainedTestConfig(t, "10.0.0.1")
			_, config, aux := renderFor("retained", "10.0.0.1", mapEntry)
			store := &retainedTestIntent{planID: plan.ID, checksum: retained.Reference.Checksum}
			warm := createTestDeployer(bus.EventBus)
			deployTo(t, warm, bus, plan, config, aux, "config_validation", agentEndpoint(old, "old"))
			cold := createTestDeployer(bus.EventBus)
			recovery := installRetainedTestIntent(cold, store)
			_, err := recovery.currentRetained(t.Context(), []configpublisher.RetainedConfig{retained})
			require.NoError(t, err)
			switch cause {
			case "newer intent before acknowledgement":
				store.planID = "newer-unacknowledged-plan"
			case "intent advances during fleet agreement":
				store.afterCheck = func() { store.planID = "newer-unacknowledged-plan" }
			case "running pod advanced":
				newPlan, newConfig, newAux := renderFor("retained", "10.0.0.2", mapEntry)
				deployTo(t, warm, bus, newPlan, newConfig, newAux, "config_validation", agentEndpoint(old, "old"))
			}
			before := old.State()
			applyCount := len(old.Applies())
			result := deployTo(t, cold, bus, plan, config, aux, retainedDeploymentReason, agentEndpoint(old, "old"), agentEndpoint(fresh, "new"))
			assert.Positive(t, result.Failed)
			assert.Empty(t, fresh.Applies())
			assert.Equal(t, applyCount, len(old.Applies()))
			assert.Equal(t, before.AppliedPlanID, old.State().AppliedPlanID)
			assert.Equal(t, before.AppliedToken, old.State().AppliedToken)
		})
	}
}

func TestDeploymentCannotApplyWithoutDurableIntent(t *testing.T) {
	bus := newTestBus(t)
	agent := agenttest.New(t)
	plan, config, aux := renderFor("retained", "10.0.0.1", mapEntry)
	c := createTestDeployer(bus.EventBus)
	installRetainedTestIntent(c, &retainedTestIntent{recordErr: errors.New("API unavailable")})
	result := deployTo(t, c, bus, plan, config, aux, "config_validation", agentEndpoint(agent, "new"))
	assert.Equal(t, 1, result.Failed)
	assert.Empty(t, agent.Applies())
}

func TestColdTermWaitsForHAProxyBeforeAdvancingDeployment(t *testing.T) {
	scheduler, scheduled, ctx := gateLatchScheduler(t)
	scheduler.retained = &retainedRecovery{}
	scheduler.retainedRecovering = true
	scheduler.handleEvent(ctx, renderEvent("cold-invalid"))
	requireNothingScheduled(t, scheduled)
	require.Nil(t, scheduler.lastValidatedOccurrence)
	scheduler.handleEvent(ctx, gateEvent("cold-invalid", false, true, true, "HAProxy refuses directive"))
	requireNothingScheduled(t, scheduled)
	require.Nil(t, scheduler.lastValidatedOccurrence, "refusal must leave durable recovery eligible")
	require.Equal(t, "HAProxy refuses directive", scheduler.retainedRenderError)
	scheduler.handleEvent(ctx, renderEvent("cold-valid"))
	requireNothingScheduled(t, scheduled)
	scheduler.handleEvent(ctx, gateEvent("cold-valid", true, false, true, ""))
	requireScheduled(t, scheduled, "cold-valid")
	require.NotNil(t, scheduler.acceptedRender)
	scheduler.handleDeploymentCompleted(completionForActiveDeployment(scheduler, &events.DeploymentResult{Total: 1, Succeeded: 1}))
	scheduler.handleEvent(ctx, renderEvent("warm-next"))
	requireScheduled(t, scheduled, "warm-next")
}

func TestRetainedGaugeClearsOnlyAfterCurrentRenderConverges(t *testing.T) {
	for _, tc := range []struct {
		name                                    string
		accepted, failed, pending, renderFailed bool
		want                                    float64
	}{
		{name: "recovered deployment", want: 1},
		{name: "current render", accepted: true},
		{name: "apply failed", accepted: true, failed: true, want: 1},
		{name: "reload pending", accepted: true, pending: true, want: 1},
		{name: "render failed again", accepted: true, renderFailed: true, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bus := newTestBus(t)
			scheduler := newDeploymentScheduler(bus.EventBus, testutil.NewTestLogger(), 0, time.Second)
			initLoopChannels(scheduler)
			t.Cleanup(scheduler.stopFailureRetries)
			gauge := prometheus.NewGauge(prometheus.GaugeOpts{Name: "retained_test"})
			scheduler.retained = &retainedRecovery{metrics: &metrics.Metrics{RetainedConfigActive: gauge}}
			scheduler.setRetainedActive(true)
			if tc.accepted {
				primeValidated(scheduler, "current", "checksum", "current")
			}
			if tc.renderFailed {
				scheduler.retainedRenderError = "failed again"
			}
			result := &events.DeploymentResult{Total: 1, Succeeded: 1}
			if tc.failed {
				result.Failed = 1
				result.Succeeded = 0
			}
			if tc.pending {
				result.PendingReloads = 1
			}
			scheduler.handleDeploymentCompleted(completionForActiveDeployment(scheduler, result))
			require.Equal(t, tc.want, promtest.ToFloat64(gauge))
			scheduler.stopRetainedTerm()
			require.Zero(t, promtest.ToFloat64(gauge))
		})
	}
}

func TestReacquiredLeadershipDiscardsPreviousTermRendersWithoutLossEvent(t *testing.T) {
	bus := newTestBus(t)
	scheduler := newDeploymentScheduler(bus.EventBus, testutil.NewTestLogger(), 0, time.Second)
	scheduler.retained = &retainedRecovery{}
	primeRendered(scheduler, "old", "old", "old")
	primeValidated(scheduler, "old", "old", "old")
	scheduler.acceptRenderLocked()
	scheduler.currentEndpoints = oneEndpoint()
	scheduler.state.pending = depFor(oneEndpoint())
	scheduler.beginRetainedTerm()
	require.Nil(t, scheduler.lastRenderedOccurrence)
	require.Nil(t, scheduler.lastValidatedOccurrence)
	require.Nil(t, scheduler.acceptedRender)
	require.Empty(t, scheduler.currentEndpoints)
	require.Nil(t, scheduler.state.pending)
	require.True(t, scheduler.awaitingFirstAcceptedRenderLocked())
}

func TestLatePassingGateCannotDeployBeforeNewerFailedRender(t *testing.T) {
	scheduler, scheduled, ctx := gateLatchScheduler(t)
	scheduler.retained = &retainedRecovery{}
	scheduler.retainedRecovering = true
	scheduler.handleEvent(ctx, renderEvent("delayed-cold-verdict"))
	scheduler.renderFailedForRetention(ctx, "newer render failed")
	scheduler.handleEvent(ctx, gateEvent("delayed-cold-verdict", true, false, true, ""))
	requireNothingScheduled(t, scheduled)
	require.Nil(t, scheduler.lastValidatedOccurrence)
	require.Equal(t, "newer render failed", scheduler.retainedRenderError)
}
