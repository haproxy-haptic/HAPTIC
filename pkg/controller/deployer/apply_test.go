// Copyright 2026 Philipp Hossner
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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/agenttest"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	agentclient "github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/client"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
)

// renderFor builds a render whose one dynamic backend points at address, plus
// one map file. The plan describes exactly the bytes the deployment carries, so
// the digests the manifest declares are the digests of the parts it sends.
func renderFor(id, address, mapContent string) (*renderplan.Plan, string, *dataplane.AuxiliaryFiles) {
	_ = id
	profileText := "defaults\n  mode http\n"
	backendText := "backend be_app\n  server srv1 " + address + ":8080\n"
	config := profileText + backendText
	backend := renderplan.Backend{
		Name:           "be_app",
		Profile:        "http",
		Mode:           "http",
		Shape:          renderplan.ShapeDynamic,
		Servers:        []renderplan.Server{{Name: "srv1", Address: address, Port: 8080}},
		BodyDigest:     renderplan.DigestString("body"),
		CommentsDigest: renderplan.DigestString("comments"),
		TextDigest:     renderplan.DigestString(backendText),
		Body:           []string{"body"},
		Comments:       []string{"comments"},
		ContentKnown:   true,
	}
	backend.RecordDigest = testBackendRecordDigest(&backend)
	plan := &renderplan.Plan{
		SchemaVersion: renderplan.SchemaVersion,
		Sections: []renderplan.Section{
			{Kind: renderplan.SectionKindProfile, Name: "http", TextDigest: renderplan.DigestString(profileText), Length: len(profileText), Text: profileText, TextKnown: true},
			{Kind: renderplan.SectionKindBackend, Name: "be_app", TextDigest: backend.TextDigest, Length: len(backendText), Text: backendText, TextKnown: true},
		},
		Backends: map[string]renderplan.Backend{"be_app": backend},
		Profiles: map[string]renderplan.Profile{"http": {Name: "http", BodyDigest: renderplan.DigestString("  mode http\n")}},
		Maps: map[string]renderplan.Map{"maps/host.map": {
			Path:    "maps/host.map",
			Entries: renderplan.ParseMapEntries(mapContent),
		}},
		Files: []renderplan.File{
			{
				Path: "haproxy.cfg", Kind: renderplan.FileKindConfig, ReloadOnChange: true,
				Digest: renderplan.DigestString(config), Size: int64(len(config)), Content: config, ContentKnown: true,
			},
			{
				Path: "maps/host.map", Kind: renderplan.FileKindMap,
				Digest: renderplan.DigestString(mapContent), Size: int64(len(mapContent)), Content: mapContent, ContentKnown: true,
			},
		},
	}
	aux := &dataplane.AuxiliaryFiles{
		MapFiles: []auxiliaryfiles.MapFile{{Path: "maps/host.map", Content: mapContent}},
	}
	plan.ComputeID()
	return plan, config, aux
}

func testBackendRecordDigest(backend *renderplan.Backend) string {
	record := *backend
	record.RecordDigest = ""
	record.TextDigest = ""
	encoded, err := json.Marshal(&record)
	if err != nil {
		panic(err)
	}
	return renderplan.Digest(encoded)
}

const mapEntry = "example.com be_app\n"

// renderWithServers moves 1,001 server addresses between renders.
func renderWithServers(id string, addressOffset int) (*renderplan.Plan, string, *dataplane.AuxiliaryFiles) {
	_ = id
	const servers = api.MaxOpsPerApply + 1
	profileText := "defaults\n  mode http\n"
	backendText := "backend be_app\n"
	list := make([]renderplan.Server, 0, servers)
	for i := range servers {
		name := fmt.Sprintf("srv%d", i)
		address := fmt.Sprintf("10.%d.%d.%d", addressOffset, i/250, i%250)
		list = append(list, renderplan.Server{Name: name, Address: address, Port: 8080})
		backendText += "  server " + name + " " + address + ":8080\n"
	}
	config := profileText + backendText
	backend := renderplan.Backend{
		Name: "be_app", Profile: "http", Mode: "http", Shape: renderplan.ShapeDynamic,
		Servers:      list,
		BodyDigest:   renderplan.DigestString("body"),
		RecordDigest: "",
		TextDigest:   renderplan.DigestString(backendText),
		Body:         []string{"body"},
		ContentKnown: true,
	}
	backend.CommentsDigest = renderplan.DigestString("")
	backend.RecordDigest = testBackendRecordDigest(&backend)
	plan := &renderplan.Plan{
		SchemaVersion: renderplan.SchemaVersion,
		Sections: []renderplan.Section{
			{Kind: renderplan.SectionKindProfile, Name: "http", TextDigest: renderplan.DigestString(profileText), Length: len(profileText), Text: profileText, TextKnown: true},
			{Kind: renderplan.SectionKindBackend, Name: "be_app", TextDigest: backend.TextDigest, Length: len(backendText), Text: backendText, TextKnown: true},
		},
		Backends: map[string]renderplan.Backend{"be_app": backend},
		Profiles: map[string]renderplan.Profile{"http": {Name: "http", BodyDigest: renderplan.DigestString("  mode http\n")}},
		Files: []renderplan.File{{
			Path: "haproxy.cfg", Kind: renderplan.FileKindConfig, ReloadOnChange: true,
			Digest: renderplan.DigestString(config), Size: int64(len(config)), Content: config, ContentKnown: true,
		}},
	}
	plan.ComputeID()
	return plan, config, &dataplane.AuxiliaryFiles{}
}

// renderWithBackends builds a render carrying one dynamic backend per name, so
// the diff between two of them composes a backend removal.
func renderWithBackends(id string, names ...string) (*renderplan.Plan, string, *dataplane.AuxiliaryFiles) {
	_ = id
	profileText := "defaults\n  mode http\n"
	config := profileText
	plan := &renderplan.Plan{
		SchemaVersion: renderplan.SchemaVersion,
		Sections: []renderplan.Section{{
			Kind: renderplan.SectionKindProfile, Name: "http", TextDigest: renderplan.DigestString(profileText),
			Length: len(profileText), Text: profileText, TextKnown: true,
		}},
		Backends: map[string]renderplan.Backend{},
		Profiles: map[string]renderplan.Profile{"http": {Name: "http", BodyDigest: renderplan.DigestString("  mode http\n")}},
	}
	for _, name := range names {
		sectionText := "backend " + name + "\n  server srv1 10.0.0.1:8080\n"
		config += sectionText
		backend := renderplan.Backend{
			Name: name, Profile: "http", Mode: "http", Shape: renderplan.ShapeDynamic,
			Servers:      []renderplan.Server{{Name: "srv1", Address: "10.0.0.1", Port: 8080}},
			BodyDigest:   renderplan.DigestString("body-" + name),
			TextDigest:   renderplan.DigestString(sectionText),
			Body:         []string{"body-" + name},
			ContentKnown: true,
		}
		backend.CommentsDigest = renderplan.DigestString("")
		backend.RecordDigest = testBackendRecordDigest(&backend)
		plan.Backends[name] = backend
		plan.Sections = append(plan.Sections, renderplan.Section{
			Kind: renderplan.SectionKindBackend, Name: name, TextDigest: backend.TextDigest,
			Length: len(sectionText), Text: sectionText, TextKnown: true,
		})
	}
	plan.Files = []renderplan.File{{
		Path: "haproxy.cfg", Kind: renderplan.FileKindConfig, ReloadOnChange: true,
		Digest: renderplan.DigestString(config), Size: int64(len(config)), Content: config, ContentKnown: true,
	}}
	plan.ComputeID()
	return plan, config, &dataplane.AuxiliaryFiles{}
}

// deployTo runs one whole deployment against the fake agents and returns the
// completion the deployer published.
func deployTo(t *testing.T, component *Component, bus *deployerBus, plan *renderplan.Plan,
	config string, aux *dataplane.AuxiliaryFiles, reason string, endpoints ...dataplane.Endpoint,
) *events.DeploymentCompletedEvent {
	t.Helper()
	occurrence := mustOccurrenceFor(plan, config, aux, nil)
	event, err := events.NewDeploymentScheduledEventWithCycle(
		occurrence, endpoints, "rt-cfg-1", "haptic", reason, true,
	)
	require.NoError(t, err)
	component.deployToEndpoints(context.Background(), func() {}, event, "deployment-"+plan.ID)
	return testutil.WaitForEvent[*events.DeploymentCompletedEvent](t, bus.Events, testutil.LongTimeout)
}

func agentEndpoint(agent *agenttest.Agent, podName string) dataplane.Endpoint {
	return dataplane.Endpoint{
		URL:          agent.URL(),
		Username:     agent.Username(),
		Password:     agent.Password(),
		PodName:      podName,
		PodNamespace: "haptic",
		PodUID:       podName + "-uid",
	}
}

// A pod that reports no applied plan cannot be diffed against: it gets the
// complete file set and a reload, with the plan blob so its next baseline is
// readable even after this controller is gone.
func TestApply_FirstApplyIsFullStateAndReload(t *testing.T) {
	agent := agenttest.New(t)
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)
	plan, config, aux := renderFor("plan-1", "10.0.0.1", mapEntry)

	completed := deployTo(t, component, bus, plan, config, aux, "config_validation",
		agentEndpoint(agent, "haproxy-0"))

	require.Equal(t, 1, completed.Succeeded)
	require.Equal(t, 0, completed.Failed)
	applies := agent.Applies()
	require.Len(t, applies, 1)
	assert.Equal(t, api.ModeReload, applies[0].Manifest.Mode, "an unknown baseline can only be reloaded onto")
	assert.Empty(t, applies[0].Manifest.Ops, "ops composed against nothing would be composed against a guess")
	assert.Len(t, applies[0].Parts, 2, "every file travels when the agent holds none of them")
	assert.NotEmpty(t, applies[0].Plan, "the pod must be able to report a baseline this controller can decode")
	assert.Equal(t, api.ResultReload, applies[0].Result.Mode)
}

// The second apply against a known baseline composes runtime ops: HAProxy
// changes the server's address without a reload, and only the changed file
// travels.
func TestApply_SecondApplyRunsAtRuntime(t *testing.T) {
	agent := agenttest.New(t)
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)
	endpoint := agentEndpoint(agent, "haproxy-0")

	plan1, config1, aux1 := renderFor("plan-1", "10.0.0.1", mapEntry)
	deployTo(t, component, bus, plan1, config1, aux1, "config_validation", endpoint)

	plan2, config2, aux2 := renderFor("plan-2", "10.0.0.2", mapEntry)
	completed := deployTo(t, component, bus, plan2, config2, aux2, "config_validation", endpoint)

	require.Equal(t, 1, completed.Succeeded)
	applies := agent.Applies()
	require.Len(t, applies, 2)
	second := applies[1]
	assert.Equal(t, api.ModeAuto, second.Manifest.Mode)
	require.Len(t, second.Manifest.Ops, 1)
	assert.Equal(t, api.OpServerSetAddr, second.Manifest.Ops[0].Kind)
	assert.Equal(t, "10.0.0.2", second.Manifest.Ops[0].Address)
	assert.Equal(t, api.ResultRuntime, second.Result.Mode)
	assert.Contains(t, second.Parts, "haproxy.cfg", "haproxy.cfg always travels whole")
	assert.NotContains(t, second.Parts, "maps/host.map", "an unchanged file the agent holds must not travel")
	assert.Empty(t, second.Plan, "a runtime apply carries no blob; the keeper delivers it afterwards")
	assert.Equal(t, plan1.ID, second.Manifest.ExpectedPrevPlanID)
	awaitStoredPlan(t, agent, plan2.ID)
}

// awaitStoredPlan waits for the keeper to hand the pod the blob of planID:
// the pod hands back only the blob of the plan it applied, and a leader with
// a cold cache reads its baseline from it.
func awaitStoredPlan(t *testing.T, agent *agenttest.Agent, planID string) {
	t.Helper()
	require.Eventually(t, func() bool {
		state := agent.State()
		return state.AppliedPlanID == planID && len(state.AppliedPlan) > 0
	}, testutil.EventTimeout, 5*time.Millisecond, "the keeper did not deliver the blob of %s", planID)
}

// A map file whose content changed but whose entries the diff cannot express as
// runtime ops is written without a reload: file_only.
func TestApply_MapContentChangeIsFileOnly(t *testing.T) {
	agent := agenttest.New(t)
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)
	endpoint := agentEndpoint(agent, "haproxy-0")

	plan1, config1, aux1 := renderFor("plan-1", "10.0.0.1", mapEntry)
	deployTo(t, component, bus, plan1, config1, aux1, "config_validation", endpoint)

	// Same render, different map bytes: the plan's entries are unchanged, so
	// nothing is composed and only the file moves.
	plan2, config2, aux2 := renderFor("plan-2", "10.0.0.1", mapEntry+"# a comment\n")
	completed := deployTo(t, component, bus, plan2, config2, aux2, "config_validation", endpoint)

	require.Equal(t, 1, completed.Succeeded)
	applies := agent.Applies()
	require.Len(t, applies, 2)
	assert.Equal(t, api.ResultFileOnly, applies[1].Result.Mode)
	assert.Contains(t, applies[1].Parts, "maps/host.map")
	assert.Equal(t, applies[0].Result.WorkerOpsPlanID, applies[1].Manifest.ExpectedWorkerOpsPlanID)
	assert.Equal(t, applies[0].Result.WorkerOpsPlanProof, applies[1].Manifest.ExpectedWorkerOpsPlanProof)
}

func TestApply_AgentWithoutWorkerFenceReceivesFullReload(t *testing.T) {
	agent := agenttest.New(t, agenttest.WithoutWorkerFence())
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)
	endpoint := agentEndpoint(agent, "haproxy-0")
	plan1, config1, aux1 := renderFor("plan-1", "10.0.0.1", mapEntry)
	deployTo(t, component, bus, plan1, config1, aux1, "config_validation", endpoint)
	plan2, config2, aux2 := renderFor("plan-2", "10.0.0.2", mapEntry)
	completed := deployTo(t, component, bus, plan2, config2, aux2, "config_validation", endpoint)
	require.Equal(t, 1, completed.Succeeded)
	applies := agent.Applies()
	require.Len(t, applies, 2)
	require.Equal(t, api.ModeReload, applies[1].Manifest.Mode)
	require.Empty(t, applies[1].Manifest.Ops)
	require.Empty(t, applies[1].Manifest.InPlaceOps)
	require.Len(t, applies[1].Parts, 2)
	require.Equal(t, api.ResultReload, applies[1].Result.Mode)
}

// Re-applying the same render changes nothing on the pod.
func TestApply_UnchangedRenderIsANoop(t *testing.T) {
	agent := agenttest.New(t)
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)
	endpoint := agentEndpoint(agent, "haproxy-0")

	plan, config, aux := renderFor("plan-1", "10.0.0.1", mapEntry)
	deployTo(t, component, bus, plan, config, aux, "config_validation", endpoint)
	completed := deployTo(t, component, bus, plan, config, aux, "config_validation", endpoint)

	require.Equal(t, 1, completed.Succeeded)
	applies := agent.Applies()
	require.Len(t, applies, 2)
	assert.Equal(t, api.ResultNoop, applies[1].Result.Mode)
}

// The drift pass asks each pod to re-hash its tree, and carries the plan blob
// so a pod whose stored copy went missing gets it back.
func TestApply_DriftPassVerifiesTheTree(t *testing.T) {
	agent := agenttest.New(t)
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)
	endpoint := agentEndpoint(agent, "haproxy-0")

	plan, config, aux := renderFor("plan-1", "10.0.0.1", mapEntry)
	initial := deployTo(t, component, bus, plan, config, aux, "config_validation", endpoint)
	occurrence, err := initial.RenderOccurrence()
	require.NoError(t, err)
	component.SetValidatedOccurrence(occurrence)

	completed := deployTo(t, component, bus, plan, config, aux, events.TriggerReasonDriftPrevention, endpoint)

	require.Equal(t, 1, completed.Succeeded)
	applies := agent.Applies()
	require.Len(t, applies, 2)
	assert.Equal(t, api.ResultNoop, applies[1].Result.Mode)
	assert.Equal(t, plan.ID, applies[1].Manifest.ValidatedPlanID,
		"the drift apply is what promotes the rollback baseline, so it must name the validated plan")
	assert.NotEmpty(t, applies[1].Plan, "the drift apply refreshes the pod's stored plan")
	assert.Equal(t, plan.ID, agent.State().LKGPlanID)
}

// A pod whose applied plan moved on under this controller's feet re-reads the
// state and diffs again rather than forcing a reload.
func TestApply_PrevMismatchRediffsFromTheAgentState(t *testing.T) {
	agent := agenttest.New(t)
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)
	endpoint := agentEndpoint(agent, "haproxy-0")

	plan1, config1, aux1 := renderFor("plan-1", "10.0.0.1", mapEntry)
	deployTo(t, component, bus, plan1, config1, aux1, "config_validation", endpoint)

	// A foreign writer advances the pod's baseline between this controller's
	// state read and its apply.
	agent.ConflictOnce("prev_mismatch")

	plan2, config2, aux2 := renderFor("plan-2", "10.0.0.2", mapEntry)
	completed := deployTo(t, component, bus, plan2, config2, aux2, "config_validation", endpoint)

	applies := agent.Applies()
	require.Len(t, applies, 3, "the conflicting apply, then the re-diffed one")
	assert.Equal(t, "prev_mismatch", applies[1].Conflict.Reason)
	assert.Nil(t, applies[1].Result, "a conflicted apply writes nothing")
	assert.Equal(t, api.ModeAuto, applies[2].Manifest.Mode,
		"a moved baseline is re-diffed, not reloaded onto")
	require.NotNil(t, applies[2].Result)
	assert.Equal(t, 1, completed.Succeeded)
	assert.NotEmpty(t, applies[2].Plan, "after a conflict the pod's stored plan is not known to be this controller's")
}

// The worker moved on between the state read and the apply (its pacer fired):
// the in-place batch is composed against a worker the pod no longer has, the
// agent refuses without writing, and the deployer re-diffs against the worker
// as it is now — the pod's applied baseline is intact, so nothing degrades to
// a full-state reload.
func TestApply_WorkerOpsMismatchRediffsAgainstTheWorker(t *testing.T) {
	agent := agenttest.New(t)
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)
	endpoint := agentEndpoint(agent, "haproxy-0")

	plan1, config1, aux1 := renderFor("plan-1", "10.0.0.1", mapEntry)
	deployTo(t, component, bus, plan1, config1, aux1, "config_validation", endpoint)
	agent.SetReloadPending(true)
	agent.ConflictOnce("worker_ops_mismatch")

	plan2, config2, aux2 := renderFor("plan-2", "10.0.0.2", mapEntry)
	completed := deployTo(t, component, bus, plan2, config2, aux2, "config_validation", endpoint)

	applies := agent.Applies()
	require.Len(t, applies, 3, "the conflicting apply, then the re-diffed one")
	assert.Equal(t, "worker_ops_mismatch", applies[1].Conflict.Reason)
	assert.Nil(t, applies[1].Result, "a conflicted apply writes nothing")
	third := applies[2]
	assert.Equal(t, api.ModeAuto, third.Manifest.Mode, "the applied baseline is intact: no full-state reload")
	assert.Equal(t, []string{api.OpServerSetAddr}, opKinds(third.Manifest.InPlaceOps), "re-diffed in-place batch")
	require.NotNil(t, third.Result)
	assert.True(t, third.Result.OK)
	assert.Equal(t, 0, completed.Failed)
	assert.Equal(t, 1, completed.PendingReloads)
}

// A baseline the agent dropped (a refused apply, a restart) cannot be diffed
// against: the retry carries the complete file set and a reload.
func TestApply_UnknownBaselineFallsBackToFullState(t *testing.T) {
	agent := agenttest.New(t)
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)
	endpoint := agentEndpoint(agent, "haproxy-0")

	plan1, config1, aux1 := renderFor("plan-1", "10.0.0.1", mapEntry)
	deployTo(t, component, bus, plan1, config1, aux1, "config_validation", endpoint)
	agent.ConflictOnce("unknown_baseline")

	plan2, config2, aux2 := renderFor("plan-2", "10.0.0.2", mapEntry)
	completed := deployTo(t, component, bus, plan2, config2, aux2, "config_validation", endpoint)

	applies := agent.Applies()
	require.Len(t, applies, 3)
	assert.Equal(t, "unknown_baseline", applies[1].Conflict.Reason)
	assert.Equal(t, api.ModeReload, applies[2].Manifest.Mode)
	assert.Empty(t, applies[2].Manifest.Ops)
	assert.Len(t, applies[2].Parts, 2, "a full state carries every file")
	assert.Equal(t, 1, completed.Succeeded)
}

// A newer leader epoch owns the fleet: this controller gives leadership up
// rather than racing its successor. Only releasing the Lease re-arms it — a
// replica that just stopped dispatching keeps renewing the Lease it holds, and
// nothing would ever start it leading again.
func TestApply_StaleEpochStandsDown(t *testing.T) {
	agent := agenttest.New(t)
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)
	fence := &fixedFence{epoch: 1, reclaimErr: errors.New("the lease is held at a newer epoch")}
	component.fence = fence
	endpoint := agentEndpoint(agent, "haproxy-0")

	plan1, config1, aux1 := renderFor("plan-1", "10.0.0.1", mapEntry)
	deployTo(t, component, bus, plan1, config1, aux1, "config_validation", endpoint)

	// A newer leader has spoken to this pod at a higher epoch.
	agent.ConflictOnce("stale_epoch")

	plan2, config2, aux2 := renderFor("plan-2", "10.0.0.2", mapEntry)
	completed := deployTo(t, component, bus, plan2, config2, aux2, "config_validation", endpoint)

	assert.Equal(t, 1, completed.Failed)
	assert.Equal(t, 0, completed.Succeeded)
	assert.Equal(t, []string{"stale_leader_epoch"}, fence.standDowns(),
		"standing down must release the lease, not only announce that leadership was lost")

	applies := agent.Applies()
	require.Len(t, applies, 2, "a stood-down controller does not try again")
	assert.Equal(t, "stale_epoch", applies[1].Conflict.Reason)
	assert.Nil(t, applies[1].Result, "a stood-down controller must write nothing")
}

// Without leader election there is no Lease to hand back, so the event is all
// the leader-only components have to stop on.
func TestApply_StaleEpochWithoutAFenceReportsLostLeadership(t *testing.T) {
	agent := agenttest.New(t)
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)
	endpoint := agentEndpoint(agent, "haproxy-0")

	plan1, config1, aux1 := renderFor("plan-1", "10.0.0.1", mapEntry)
	deployTo(t, component, bus, plan1, config1, aux1, "config_validation", endpoint)

	agent.ConflictOnce("stale_epoch")
	lostCh := bus.SubscribeTypes("stand-down", 4, events.EventTypeLostLeadership)

	plan2, config2, aux2 := renderFor("plan-2", "10.0.0.2", mapEntry)
	completed := deployTo(t, component, bus, plan2, config2, aux2, "config_validation", endpoint)

	assert.Equal(t, 1, completed.Failed)
	lost := testutil.WaitForEvent[*events.LostLeadershipEvent](t, lostCh, testutil.LongTimeout)
	assert.Equal(t, "stale_leader_epoch", lost.Reason)
	assert.Equal(t, standaloneIdentity, lost.Identity)
}

// A pod outranks the controller because the epoch counter regressed — a Lease
// deleted and recreated loses the annotation — and no rival exists. Giving
// leadership up would freeze the fleet at the low epoch forever, so the epoch is
// lifted past the fleet's instead and the deployment fails into the retry.
func TestApply_StaleEpochFromARegressedCounterReclaimsTheEpoch(t *testing.T) {
	agent := agenttest.New(t)
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)
	fence := &fixedFence{epoch: 1}
	component.fence = fence
	endpoint := agentEndpoint(agent, "haproxy-0")

	plan1, config1, aux1 := renderFor("plan-1", "10.0.0.1", mapEntry)
	deployTo(t, component, bus, plan1, config1, aux1, "config_validation", endpoint)

	agent.SetAppliedEpoch(12)
	plan2, config2, aux2 := renderFor("plan-2", "10.0.0.2", mapEntry)
	completed := deployTo(t, component, bus, plan2, config2, aux2, "config_validation", endpoint)

	assert.Equal(t, 1, completed.Failed)
	assert.Empty(t, fence.standDowns(), "no rival owns the fleet, so leadership must not be given up")
	assert.Equal(t, []uint64{12}, fence.reclaims(), "the epoch must be lifted past the one the pod holds")

	// The retry the scheduler drives now carries the reclaimed epoch.
	plan3, config3, aux3 := renderFor("plan-3", "10.0.0.3", mapEntry)
	completed = deployTo(t, component, bus, plan3, config3, aux3, "config_validation", endpoint)
	assert.Equal(t, 1, completed.Succeeded)
	applies := agent.Applies()
	assert.Equal(t, uint64(13), applies[len(applies)-1].Manifest.Token.LeaderEpoch)
}

// The agent answers a manifest whose parts it does not hold with the list of
// paths; the deployer resends exactly one apply carrying them.
func TestApply_MissingPartsAreResent(t *testing.T) {
	agent := agenttest.New(t)
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)
	endpoint := agentEndpoint(agent, "haproxy-0")

	plan1, config1, aux1 := renderFor("plan-1", "10.0.0.1", mapEntry)
	deployTo(t, component, bus, plan1, config1, aux1, "config_validation", endpoint)

	// The pod reported holding the map file, but its tree does not.
	agent.MissingOnce("maps/host.map")

	plan2, config2, aux2 := renderFor("plan-2", "10.0.0.2", mapEntry)
	completed := deployTo(t, component, bus, plan2, config2, aux2, "config_validation", endpoint)

	applies := agent.Applies()
	require.Len(t, applies, 3)
	assert.Equal(t, []string{"maps/host.map"}, applies[1].Missing)
	assert.Contains(t, applies[2].Parts, "maps/host.map")
	assert.Equal(t, 1, completed.Succeeded)
}

// A refused apply is reported with HAProxy's own words, counted, and it drops
// the pod's baseline so the next apply is the complete state plus a reload.
func TestApply_NackInvalidatesTheBaseline(t *testing.T) {
	agent := agenttest.New(t)
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)
	endpoint := agentEndpoint(agent, "haproxy-0")

	plan1, config1, aux1 := renderFor("plan-1", "10.0.0.1", mapEntry)
	deployTo(t, component, bus, plan1, config1, aux1, "config_validation", endpoint)

	agent.RejectOp(api.OpServerSetAddr)
	failedCh := bus.SubscribeTypes("nack-watch", 4, events.EventTypeInstanceDeploymentFailed)
	plan2, config2, aux2 := renderFor("plan-2", "10.0.0.2", mapEntry)
	completed := deployTo(t, component, bus, plan2, config2, aux2, "config_validation", endpoint)

	require.Equal(t, 1, completed.Failed)
	assert.Equal(t, 1.0, promtestutil.ToFloat64(
		component.metrics.ApplyRejectedTotal.WithLabelValues("haproxy-0")))
	assert.True(t, component.baselineInvalid(&endpoint),
		"a refused apply may have left the pod somewhere no plan describes")

	failed := testutil.WaitForEvent[*events.InstanceDeploymentFailedEvent](t, failedCh, testutil.LongTimeout)
	assert.Contains(t, failed.Error, "rejected by HAProxy")

	// The next deployment must not compose ops against the dropped baseline.
	agent.AcceptOp(api.OpServerSetAddr)
	plan3, config3, aux3 := renderFor("plan-3", "10.0.0.3", mapEntry)
	deployTo(t, component, bus, plan3, config3, aux3, "config_validation", endpoint)

	applies := agent.Applies()
	last := applies[len(applies)-1]
	assert.Equal(t, api.ModeReload, last.Manifest.Mode)
	assert.Empty(t, last.Manifest.Ops)
	assert.False(t, component.baselineInvalid(&endpoint), "an accepted apply clears the invalidation")
}

func composedOpsWithout(kind string) []string {
	return slices.DeleteFunc(agentclient.ComposableOps(), func(op string) bool { return op == kind })
}

// An agent that lacks an op kind the plans do not use keeps the runtime path —
// a rolling upgrade's old pods must not reload on every endpoint change — and
// the skew is logged once, at INFO.
func TestApply_AgentLackingAnUnusedOpKindStaysOnTheRuntimePath(t *testing.T) {
	agent := agenttest.New(t, agenttest.WithAgentOps(composedOpsWithout(api.OpServerSetMaxconn)...))
	bus := newTestBus(t)
	component, logs := reloadLoggingDeployer(bus)
	endpoint := agentEndpoint(agent, "haproxy-0")

	for i, address := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"} {
		plan, config, aux := renderFor(fmt.Sprintf("plan-%d", i), address, mapEntry)
		deployTo(t, component, bus, plan, config, aux, "config_validation", endpoint)
	}

	applies := agent.Applies()
	require.Len(t, applies, 3)
	for _, later := range applies[1:] {
		assert.Equal(t, api.ModeAuto, later.Manifest.Mode)
		assert.Equal(t, []string{api.OpServerSetAddr}, opKinds(later.Manifest.Ops))
		assert.Equal(t, api.ResultRuntime, later.Result.Mode)
	}
	assert.Len(t, reloadLines(t, logs), 1, "only the first apply, which has no baseline, reloads")
	assert.Empty(t, logs.Records("Agent speaks a different contract; sending the complete state and a reload"))
	assert.Equal(t, []slog.Level{slog.LevelInfo},
		logs.Levels("Agent lacks op kinds this controller composes; changes that need them reload the pod"))
	assert.Equal(t, 3.0, promtestutil.ToFloat64(component.metrics.AgentVersionSkewTotal))

	plan, config, aux := renderFor("plan-3", "10.0.0.4", mapEntry)
	deployTo(t, component, bus, plan, config, aux, "config_validation", agentEndpoint(agenttest.New(t), "haproxy-1"))
	assert.NotContains(t, component.podSkew, podKey(&endpoint), "a pod that left the fleet is forgotten")
}

// A plan that composes an op kind the agent lacks reloads that pod instead of
// sending it the op.
func TestApply_AgentLackingAComposedOpKindReloadsThatPlan(t *testing.T) {
	agent := agenttest.New(t, agenttest.WithAgentOps(composedOpsWithout(api.OpServerSetAddr)...))
	bus := newTestBus(t)
	component, logs := reloadLoggingDeployer(bus)
	endpoint := agentEndpoint(agent, "haproxy-0")

	plan1, config1, aux1 := renderFor("plan-1", "10.0.0.1", mapEntry)
	deployTo(t, component, bus, plan1, config1, aux1, "config_validation", endpoint)
	plan2, config2, aux2 := renderFor("plan-2", "10.0.0.2", mapEntry)
	completed := deployTo(t, component, bus, plan2, config2, aux2, "config_validation", endpoint)

	require.Equal(t, 1, completed.Succeeded)
	applies := agent.Applies()
	require.Len(t, applies, 2)
	assert.Equal(t, api.ModeReload, applies[1].Manifest.Mode)
	assert.Empty(t, applies[1].Manifest.Ops)
	assert.Empty(t, applies[1].Manifest.InPlaceOps)
	lines := reloadLines(t, logs)
	require.Len(t, lines, 2)
	assert.Contains(t, lines[1].Reasons, "the agent does not execute "+api.OpServerSetAddr)
}

// An agent on another contract gets the complete state and a reload on every
// apply — never a refusal, which would fence the repair path — with a counter,
// a reason on the pod's status, and one WARN per pod.
func TestApply_IncompatibleAgentContractSendsFullStateAndReload(t *testing.T) {
	tests := []struct {
		name   string
		option agenttest.Option
		reason string
	}{
		{"another API major", agenttest.WithAPIVersion(api.Version + 1), "agent speaks API version"},
		{"no reported op kinds", agenttest.WithAgentOps(), "agent reports no op kinds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := agenttest.New(t, tt.option)
			bus := newTestBus(t)
			component, logs := reloadLoggingDeployer(bus)
			endpoint := agentEndpoint(agent, "haproxy-0")
			appliedCh := bus.SubscribeTypes("skew-watch", 4, events.EventTypeConfigAppliedToPod)

			for i, address := range []string{"10.0.0.1", "10.0.0.2"} {
				plan, config, aux := renderFor(fmt.Sprintf("plan-%d", i), address, mapEntry)
				deployTo(t, component, bus, plan, config, aux, "config_validation", endpoint)
			}

			applies := agent.Applies()
			require.Len(t, applies, 2)
			for _, apply := range applies {
				assert.Equal(t, api.ModeReload, apply.Manifest.Mode)
				assert.Empty(t, apply.Manifest.Ops)
			}
			assert.Equal(t, 2.0, promtestutil.ToFloat64(component.metrics.AgentVersionSkewTotal))
			assert.Equal(t, []slog.Level{slog.LevelWarn},
				logs.Levels("Agent speaks a different contract; sending the complete state and a reload"))
			applied := testutil.WaitForEvent[*events.ConfigAppliedToPodEvent](t, appliedCh, testutil.LongTimeout)
			require.NotNil(t, applied.SyncMetadata)
			require.NotEmpty(t, applied.SyncMetadata.Reasons)
			assert.Contains(t, applied.SyncMetadata.Reasons[0], tt.reason)
		})
	}
}

// The plan the fleet ACKed is what the next render reads its server slots from.
func TestApply_AckedPlanReachesTheRenderer(t *testing.T) {
	agent := agenttest.New(t)
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)
	sink := &recordingPlanSink{}
	component.ackedPlans = sink

	plan, config, aux := renderFor("plan-1", "10.0.0.1", mapEntry)
	deployTo(t, component, bus, plan, config, aux, "config_validation", agentEndpoint(agent, "haproxy-0"))

	require.Len(t, sink.plans, 1)
	assert.True(t, exactPlan(plan, sink.plans[0]))
}

// Every pod gets the render, and the fleet's answer is the completion.
func TestApply_FansOutToEveryPod(t *testing.T) {
	first := agenttest.New(t)
	second := agenttest.New(t)
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)

	plan, config, aux := renderFor("plan-1", "10.0.0.1", mapEntry)
	completed := deployTo(t, component, bus, plan, config, aux, "config_validation",
		agentEndpoint(first, "haproxy-0"), agentEndpoint(second, "haproxy-1"))

	assert.Equal(t, 2, completed.Total)
	assert.Equal(t, 2, completed.Succeeded)
	assert.Len(t, first.Applies(), 1)
	assert.Len(t, second.Applies(), 1)
}

// A pod whose reload is only scheduled has accepted the files but does not
// serve them, so the fleet has not converged on the render.
func TestApply_ScheduledReloadIsNotCounted(t *testing.T) {
	agent := agenttest.New(t)
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)
	endpoint := agentEndpoint(agent, "haproxy-0")

	plan1, config1, aux1 := renderFor("plan-1", "10.0.0.1", mapEntry)
	deployTo(t, component, bus, plan1, config1, aux1, "config_validation", endpoint)
	agent.SetReloadPending(true)

	plan2, config2, aux2 := renderFor("plan-2", "10.0.0.2", mapEntry)
	completed := deployTo(t, component, bus, plan2, config2, aux2, "config_validation", endpoint)

	assert.Equal(t, 0, completed.Succeeded, "a pod waiting for its reload is not converged")
	assert.Equal(t, 0, completed.Failed, "and it is not a failure either — the files are on disk")
	assert.Equal(t, 1, completed.PendingReloads, "the scheduler follows up when the reload fires")
	assert.False(t, completed.PendingReloadUntil.IsZero(), "at the time the agent scheduled it for")
}

// A pod that cannot be reached fails without touching the rest of the fleet.
func TestApply_UnreachablePodFailsAlone(t *testing.T) {
	agent := agenttest.New(t)
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)

	plan, config, aux := renderFor("plan-1", "10.0.0.1", mapEntry)
	unreachable := dataplane.Endpoint{URL: "http://127.0.0.1:1", PodName: "haproxy-1", PodNamespace: "haptic"}
	completed := deployTo(t, component, bus, plan, config, aux, "config_validation",
		agentEndpoint(agent, "haproxy-0"), unreachable)

	assert.Equal(t, 1, completed.Succeeded)
	assert.Equal(t, 1, completed.Failed)
	assert.Len(t, agent.Applies(), 1)
}

// The manifest's fencing token is the leadership epoch plus a per-term apply
// sequence, so two applies from one term are ordered and a demoted leader's
// applies are refused.
func TestApply_ManifestCarriesTheFencingToken(t *testing.T) {
	agent := agenttest.New(t)
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)
	component.fence = &fixedFence{epoch: 4}
	endpoint := agentEndpoint(agent, "haproxy-0")

	plan1, config1, aux1 := renderFor("plan-1", "10.0.0.1", mapEntry)
	deployTo(t, component, bus, plan1, config1, aux1, "config_validation", endpoint)
	plan2, config2, aux2 := renderFor("plan-2", "10.0.0.2", mapEntry)
	deployTo(t, component, bus, plan2, config2, aux2, "config_validation", endpoint)

	applies := agent.Applies()
	require.Len(t, applies, 2)
	assert.Equal(t, api.Token{LeaderEpoch: 4, RenderSeq: 1}, applies[0].Manifest.Token)
	assert.Equal(t, api.Token{LeaderEpoch: 4, RenderSeq: 2}, applies[1].Manifest.Token)
	assert.Equal(t, applies[0].Manifest.Token, applies[1].Manifest.ExpectedPrevToken)
}

func TestApplyLegacyScheduledEventIsIgnored(t *testing.T) {
	agent := agenttest.New(t)
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)

	event := events.NewDeploymentScheduledEvent("config", nil,
		[]dataplane.Endpoint{agentEndpoint(agent, "haproxy-0")},
		"rt-cfg-1", "haptic", "config_validation", "checksum", nil, "", nil, true)
	component.deployToEndpoints(context.Background(), func() {}, event, "deployment-1")

	testutil.AssertNoEvent[*events.DeploymentCompletedEvent](t, bus.Events, testutil.NoEventTimeout)
	assert.Empty(t, agent.Applies())
}

// A new leader starts without controller-local proof bindings. It sends the
// complete exact file set once instead of trusting an opaque persisted blob.
func TestApply_LeaderChangeForcesOneExactReload(t *testing.T) {
	agent := agenttest.New(t)
	bus := newTestBus(t)
	endpoint := agentEndpoint(agent, "haproxy-0")

	previousLeader := createTestDeployer(bus.EventBus)
	previousLeader.fence = &fixedFence{epoch: 1}
	plan1, config1, aux1 := renderFor("plan-1", "10.0.0.1", mapEntry)
	deployTo(t, previousLeader, bus, plan1, config1, aux1, "config_validation", endpoint)

	// Two deployments in the term, which is the normal case: the pod's stored
	// blob has to follow its applied plan, not the epoch that first wrote it.
	plan2, config2, aux2 := renderFor("plan-2", "10.0.0.2", mapEntry)
	deployTo(t, previousLeader, bus, plan2, config2, aux2, "config_validation", endpoint)

	// A different process, a higher epoch, and no memory of either plan.
	newLeader := createTestDeployer(bus.EventBus)
	newLeader.fence = &fixedFence{epoch: 2}
	plan3, config3, aux3 := renderFor("plan-3", "10.0.0.3", mapEntry)
	completed := deployTo(t, newLeader, bus, plan3, config3, aux3, "config_validation", endpoint)

	require.Equal(t, 1, completed.Succeeded)
	applies := agent.Applies()
	require.Len(t, applies, 3)
	assert.Equal(t, api.ResultReload, applies[2].Result.Mode,
		"a controller restart must not infer exact equality from an opaque persisted blob")
	assert.Equal(t, uint64(2), applies[2].Manifest.Token.LeaderEpoch)
	assert.NotEmpty(t, applies[2].Plan, "the new leader restamps the blob with its own epoch")
	assert.Equal(t, 1, completed.ReloadsTriggered)
}

// A pod already on this render holds the blob that describes it, so the apply
// that changes nothing must not repeat it.
func TestApply_UnchangedRenderDoesNotRepeatThePlanBlob(t *testing.T) {
	agent := agenttest.New(t)
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)
	endpoint := agentEndpoint(agent, "haproxy-0")

	plan, config, aux := renderFor("plan-1", "10.0.0.1", mapEntry)
	deployTo(t, component, bus, plan, config, aux, "config_validation", endpoint)
	deployTo(t, component, bus, plan, config, aux, "config_validation", endpoint)

	applies := agent.Applies()
	require.Len(t, applies, 2)
	assert.NotEmpty(t, applies[0].Plan)
	assert.Empty(t, applies[1].Plan, "the pod already reports the blob for this plan")
	assert.Zero(t, agent.PlanReads(), "a controller that holds the plan reads the state without the blob")
}

// A multi-batch transaction binds one plan blob to its completed role proof.
func TestApply_BatchedApplyCarriesThePlanBlobOnce(t *testing.T) {
	agent := agenttest.New(t)
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)
	endpoint := agentEndpoint(agent, "haproxy-0")

	plan1, config1, aux1 := renderWithServers("plan-1", 10)
	deployTo(t, component, bus, plan1, config1, aux1, "config_validation", endpoint)
	plan2, config2, aux2 := renderWithServers("plan-2", 20)
	deployTo(t, component, bus, plan2, config2, aux2, "config_validation", endpoint)

	applies := agent.Applies()
	require.Len(t, applies, 2)
	assert.Empty(t, applies[1].Plan)
	require.Len(t, applies[1].Manifest.OpBatches, 1)
	awaitStoredPlan(t, agent, plan2.ID)
	puts := agent.PlanPuts()
	require.Len(t, puts, 1, "one upload bound to the completed transaction")
	assert.Equal(t, plan2.ID, puts[0].PlanID)
	assert.Equal(t, applies[1].Result.AppliedPlanProof, puts[0].Proof)
}

// The upload runs on the term's context, not the deployment's: a deployment
// cancels its own context as soon as it completes, which is right after the
// apply that offered the blob.
func TestPlanBlobUploadOutlivesTheDeployment(t *testing.T) {
	agent := agenttest.New(t, agenttest.WithPlanPutDelay(50*time.Millisecond))
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)
	endpoint := agentEndpoint(agent, "haproxy-0")
	term, endTerm := context.WithCancel(context.Background())
	defer endTerm()
	component.keeper.Begin(term)

	plan1, config1, aux1 := renderFor("plan-1", "10.0.0.1", mapEntry)
	deployTo(t, component, bus, plan1, config1, aux1, "config_validation", endpoint)
	plan2, config2, aux2 := renderFor("plan-2", "10.0.0.2", mapEntry)
	occurrence := mustOccurrenceFor(plan2, config2, aux2, nil)
	event, err := events.NewDeploymentScheduledEventWithCycle(
		occurrence, []dataplane.Endpoint{endpoint}, "rt-cfg-1", "haptic", "config_validation", true,
	)
	require.NoError(t, err)
	deployCtx, cancel := context.WithCancel(context.Background())
	component.deployToEndpoints(deployCtx, func() {}, event, "deployment-"+plan2.ID)
	cancel()
	testutil.WaitForEvent[*events.DeploymentCompletedEvent](t, bus.Events, testutil.LongTimeout)

	awaitStoredPlan(t, agent, plan2.ID)
}

// An agent without the plan endpoint gets the blob with every apply that
// moves its applied plan on, as before the keeper.
func TestApply_AgentWithoutPlanEndpointGetsTheBlobWithTheApply(t *testing.T) {
	agent := agenttest.New(t, agenttest.WithoutPlanEndpoint())
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)
	endpoint := agentEndpoint(agent, "haproxy-0")

	plan1, config1, aux1 := renderFor("plan-1", "10.0.0.1", mapEntry)
	deployTo(t, component, bus, plan1, config1, aux1, "config_validation", endpoint)
	plan2, config2, aux2 := renderFor("plan-2", "10.0.0.2", mapEntry)
	deployTo(t, component, bus, plan2, config2, aux2, "config_validation", endpoint)
	require.Eventually(t, func() bool { return !component.keeper.Delivers(&endpoint) },
		testutil.EventTimeout, 5*time.Millisecond, "the 404 must retire the keeper for this pod")
	plan3, config3, aux3 := renderFor("plan-3", "10.0.0.3", mapEntry)
	deployTo(t, component, bus, plan3, config3, aux3, "config_validation", endpoint)

	applies := agent.Applies()
	require.Len(t, applies, 3)
	assert.NotEmpty(t, applies[0].Plan, "the first apply reloads and carries the blob")
	assert.Empty(t, applies[1].Plan, "the keeper tried this one and learnt the agent has no endpoint")
	assert.NotEmpty(t, applies[2].Plan, "from then on the apply carries it")
	assert.Equal(t, plan3.ID, agent.State().AppliedPlanID)
	assert.NotEmpty(t, agent.State().AppliedPlan)
}

// Read-back and plan publication must observe the complete operation set.
func TestApply_LargeOpBatchUsesOneTransaction(t *testing.T) {
	agent := agenttest.New(t)
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)
	endpoint := agentEndpoint(agent, "haproxy-0")

	plan1, config1, aux1 := renderWithServers("plan-1", 10)
	deployTo(t, component, bus, plan1, config1, aux1, "config_validation", endpoint)

	plan2, config2, aux2 := renderWithServers("plan-2", 20)
	completed := deployTo(t, component, bus, plan2, config2, aux2, "config_validation", endpoint)

	require.Equal(t, 1, completed.Succeeded)
	applies := agent.Applies()
	require.Len(t, applies, 2, "all operation batches share one apply")
	assert.Len(t, applies[1].Manifest.Ops, api.MaxOpsPerApply)
	require.Len(t, applies[1].Manifest.OpBatches, 1)
	assert.Len(t, applies[1].Manifest.OpBatches[0], 1)
	assert.Len(t, applies[1].Result.OpResults, api.MaxOpsPerApply+1)
	assert.Equal(t, applies[0].Result.AppliedPlanID, applies[1].Manifest.ExpectedPrevPlanID)
	assert.Equal(t, 0, completed.ReloadsTriggered)
}

func TestApply_InPlaceBatchSharesTheFirstChunksBudget(t *testing.T) {
	agent := agenttest.New(t)
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)
	endpoint := agentEndpoint(agent, "haproxy-0")

	plan1, config1, aux1 := renderWithServers("plan-1", 10)
	deployTo(t, component, bus, plan1, config1, aux1, "config_validation", endpoint)

	// A reload is already scheduled, so the diff composes in-place ops for the
	// running worker alongside the runtime ops for the new plan.
	agent.SetReloadPending(true)
	plan2, config2, aux2 := renderWithServers("plan-2", 20)
	completed := deployTo(t, component, bus, plan2, config2, aux2, "config_validation", endpoint)

	assert.Equal(t, 0, completed.Failed, "an apply the client refuses never reaches the pod at all")
	assert.Equal(t, 1, completed.PendingReloads)

	applies := agent.Applies()
	require.Greater(t, len(applies), 1)
	inPlace := 0
	for _, apply := range applies[1:] {
		assert.LessOrEqual(t, len(apply.Manifest.Ops)+len(apply.Manifest.InPlaceOps), api.MaxOpsPerApply,
			"the agent client validates the two lists as one budget")
		inPlace += len(apply.Manifest.InPlaceOps)
	}
	assert.Positive(t, inPlace, "the pending reload is exactly when the in-place batch matters")
}

// One diff is shared across the pods that report the same baseline, so every
// fact it branches on has to be part of what makes them the same. A pod at the
// deferral cap plans a reload where another composes the delete batch; handing
// it that batch makes its agent refuse the ops and fall back to a reload it
// never planned, raising the invariant counter that pages an operator.
func TestApply_DiffIsNotSharedAcrossPodsAtTheDeferralCap(t *testing.T) {
	draining := agenttest.New(t)
	idle := agenttest.New(t)
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)
	drainingEndpoint := agentEndpoint(draining, "haproxy-0")
	idleEndpoint := agentEndpoint(idle, "haproxy-1")

	plan1, config1, aux1 := renderWithBackends("plan-1", "be_app", "be_extra")
	deployTo(t, component, bus, plan1, config1, aux1, "config_validation", drainingEndpoint, idleEndpoint)

	// One pod's sessions never closed, so its deferred deletes sit at the cap.
	pending := make([]string, api.MaxPendingBackendDeletes)
	for i := range pending {
		pending[i] = fmt.Sprintf("be_retiring_%d", i)
	}
	draining.SetPendingDeletes(nil, pending)

	plan2, config2, aux2 := renderWithBackends("plan-2", "be_app")
	deployTo(t, component, bus, plan2, config2, aux2, "config_validation", drainingEndpoint, idleEndpoint)

	idleApply := idle.Applies()[1]
	assert.Equal(t, api.ModeAuto, idleApply.Manifest.Mode)
	assert.NotEmpty(t, idleApply.Manifest.Ops, "a pod with no pending deletes removes the backend at runtime")

	drainingApply := draining.Applies()[1]
	assert.Equal(t, api.ModeReload, drainingApply.Manifest.Mode,
		"a pod at the cap can only take this render through a reload")
	assert.Empty(t, drainingApply.Manifest.Ops,
		"ops its agent would refuse must never be sent: the refusal costs it an unplanned reload")
}

// The plan cache retains what the fleet still refers to and nothing else, so a
// long-lived controller does not accumulate every render it ever made.
func TestApply_PlanCacheRetainsWhatTheFleetRuns(t *testing.T) {
	agent := agenttest.New(t)
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)
	endpoint := agentEndpoint(agent, "haproxy-0")

	for i := 1; i <= 3; i++ {
		plan, config, aux := renderFor(fmt.Sprintf("plan-%d", i), fmt.Sprintf("10.0.0.%d", i), mapEntry)
		deployTo(t, component, bus, plan, config, aux, "config_validation", endpoint)
	}

	state := agent.State()
	authority := podKey(&endpoint)
	assert.NotNil(t, component.plans.Plan(authority, state.RunningPlanID, state.RunningPlanProof),
		"the runtime applies never reloaded, so the worker still runs the first render")
	assert.NotNil(t, component.plans.Plan(authority, state.AppliedPlanID, state.AppliedPlanProof))
	component.plans.mu.Lock()
	assert.Len(t, component.plans.plans, 2, "the unreferenced middle render was retired")
	component.plans.mu.Unlock()
}

// A pod that failed still holds the plans it reported, so they are not the
// fleet's garbage: the cache keeps what every pod answered with, not only what
// the pods that ACKed did.
func TestApply_PlanCacheKeepsTheBaselineOfAFailedPod(t *testing.T) {
	healthy := agenttest.New(t)
	sick := agenttest.New(t)
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)
	healthyEndpoint := agentEndpoint(healthy, "haproxy-0")
	sickEndpoint := agentEndpoint(sick, "haproxy-1")

	for i := 1; i <= 2; i++ {
		plan, config, aux := renderFor(fmt.Sprintf("plan-%d", i), fmt.Sprintf("10.0.0.%d", i), mapEntry)
		deployTo(t, component, bus, plan, config, aux, "config_validation", healthyEndpoint, sickEndpoint)
	}

	// One pod's apply fails outright: nothing about its state changed, so it is
	// still the pod that applies plan-2.
	sick.FailOnce()
	plan3, config3, aux3 := renderFor("plan-3", "10.0.0.3", mapEntry)
	completed := deployTo(t, component, bus, plan3, config3, aux3, "config_validation",
		healthyEndpoint, sickEndpoint)
	require.Equal(t, 1, completed.Failed)

	sickState := sick.State()
	assert.NotNil(t, component.plans.Plan(podKey(&sickEndpoint), sickState.AppliedPlanID, sickState.AppliedPlanProof),
		"the pod whose apply failed still applies the render before it")
	assert.NotNil(t, component.plans.Plan(podKey(&sickEndpoint), sickState.RunningPlanID, sickState.RunningPlanProof),
		"the worker of both pods still runs the first render")
}

// Every pod failing at once is a blip, not a fleet that refers to nothing. A
// cache emptied by it costs the whole fleet a full-state reload on the round
// after, for a change HAProxy could have taken at runtime.
func TestApply_PlanCacheSurvivesARoundEveryPodFails(t *testing.T) {
	first := agenttest.New(t)
	second := agenttest.New(t)
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)
	endpoints := []dataplane.Endpoint{agentEndpoint(first, "haproxy-0"), agentEndpoint(second, "haproxy-1")}

	plan1, config1, aux1 := renderFor("plan-1", "10.0.0.1", mapEntry)
	deployTo(t, component, bus, plan1, config1, aux1, "config_validation", endpoints...)

	first.FailOnce()
	second.FailOnce()
	plan2, config2, aux2 := renderFor("plan-2", "10.0.0.2", mapEntry)
	completed := deployTo(t, component, bus, plan2, config2, aux2, "config_validation", endpoints...)
	require.Equal(t, 2, completed.Failed)

	state := first.State()
	assert.NotNil(t, component.plans.Plan(podKey(&endpoints[0]), state.AppliedPlanID, state.AppliedPlanProof),
		"both pods still hold the first render")
}

// While a reload is pending, an in-place batch moves the worker to a plan that
// is neither the running one nor the render — the render's new map key waits
// for the reload. The manifest names that derived plan, the pod records it, and
// the next batch is composed against it: nothing already applied is repeated,
// and nothing the worker never got is deleted.
func TestApply_InPlaceBatchNamesTheWorkerAfterIt(t *testing.T) {
	agent := agenttest.New(t)
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)
	endpoint := agentEndpoint(agent, "haproxy-0")

	plan1, config1, aux1 := renderFor("plan-1", "10.0.0.1", mapEntry)
	deployTo(t, component, bus, plan1, config1, aux1, "config_validation", endpoint)
	agent.SetReloadPending(true)

	plan2, config2, aux2 := renderFor("plan-2", "10.0.0.2", mapEntry)
	completed := deployTo(t, component, bus, plan2, config2, aux2, "config_validation", endpoint)
	require.Equal(t, 0, completed.Failed)
	applies := agent.Applies()
	require.Len(t, applies, 2)
	second := applies[1].Manifest
	require.Equal(t, []string{api.OpServerSetAddr}, opKinds(second.InPlaceOps))
	assert.Equal(t, plan1.ID, second.ExpectedWorkerOpsPlanID)
	require.NotEmpty(t, second.WorkerOpsPlanID)
	assert.NotEqual(t, plan2.ID, second.WorkerOpsPlanID, "the worker is not at the render after a partial batch")
	assert.Equal(t, second.WorkerOpsPlanID, agent.State().WorkerOpsPlanID)

	// The same render again: the worker already has the address, so no
	// in-place op is composed and the pod's worker baseline stays put.
	completed = deployTo(t, component, bus, plan2, config2, aux2, "config_validation", endpoint)
	require.Equal(t, 0, completed.Failed)
	applies = agent.Applies()
	require.Len(t, applies, 3)
	assert.Empty(t, applies[2].Manifest.InPlaceOps)
	assert.Equal(t, second.WorkerOpsPlanID, agent.State().WorkerOpsPlanID)

	// A third render composes against the derived plan and is accepted.
	plan3, config3, aux3 := renderFor("plan-3", "10.0.0.3", mapEntry)
	completed = deployTo(t, component, bus, plan3, config3, aux3, "config_validation", endpoint)
	require.Equal(t, 0, completed.Failed)
	applies = agent.Applies()
	require.Len(t, applies, 4)
	third := applies[3].Manifest
	require.Equal(t, []string{api.OpServerSetAddr}, opKinds(third.InPlaceOps))
	assert.Equal(t, second.WorkerOpsPlanID, third.ExpectedWorkerOpsPlanID)
	assert.NotEqual(t, third.WorkerOpsPlanID, second.WorkerOpsPlanID)
	assert.True(t, applies[3].Result.OK, "%+v", applies[3].Result.Error)
}

func opKinds(ops []api.Op) []string {
	kinds := make([]string, 0, len(ops))
	for i := range ops {
		kinds = append(kinds, ops[i].Kind)
	}
	return kinds
}

// Under continuous change a render's paced reload fires between two
// deployments, and the deployment after it is itself dispatched behind the
// next reload — so its own completion never says "the fleet runs it". The
// deployer reads that from the ACKs of the deployment that follows and
// publishes the earlier render's deployed status then.
func TestApply_ObservedReloadPublishesTheEarlierRendersStatus(t *testing.T) {
	agent := agenttest.New(t)
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)
	endpoint := agentEndpoint(agent, "haproxy-0")

	plan1, config1, aux1 := renderFor("plan-1", "10.0.0.1", mapEntry)
	deployTo(t, component, bus, plan1, config1, aux1, "config_validation", endpoint)
	agent.SetReloadPending(true)

	plan2, config2, aux2 := renderFor("plan-2", "10.0.0.2", mapEntry)
	completed := deployTo(t, component, bus, plan2, config2, aux2, "config_validation", endpoint)
	require.Equal(t, 1, completed.PendingReloads)
	secondOccurrence, err := completed.RenderOccurrence()
	require.NoError(t, err)
	testutil.AssertNoEvent[*events.DeploymentSkippedEvent](t, bus.Events, testutil.NoEventTimeout)

	// The reload fires and the next render arrives inside the next window.
	agent.FirePendingReload()
	agent.SetReloadPending(true)
	plan3, config3, aux3 := renderFor("plan-3", "10.0.0.3", mapEntry)
	completed = deployTo(t, component, bus, plan3, config3, aux3, "config_validation", endpoint)
	require.Equal(t, 1, completed.PendingReloads)
	require.Equal(t, 0, completed.Succeeded, "the third render itself is not running yet")

	observed := testutil.WaitForEvent[*events.DeploymentSkippedEvent](t, bus.Events, testutil.LongTimeout)
	assert.Equal(t, events.SkipReasonReloadObserved, observed.Reason)
	observedOccurrence, err := observed.RenderOccurrence()
	require.NoError(t, err)
	assert.True(t, sameOccurrence(secondOccurrence, observedOccurrence))
	assert.Equal(t, 1, observed.Total)

	// Once the fleet converges on the newest render its own completion
	// carries the status; nothing older is re-published.
	agent.FirePendingReload()
	completed = deployTo(t, component, bus, plan3, config3, aux3, "config_validation", endpoint)
	require.Equal(t, 1, completed.Succeeded)
	testutil.AssertNoEvent[*events.DeploymentSkippedEvent](t, bus.Events, testutil.NoEventTimeout)
}

// A runtime apply moves the worker to the applied plan. When a reload is
// scheduled afterwards, the in-place batch is composed against that plan —
// not against the plan of the last reload — so nothing the runtime apply
// already did is repeated or undone.
func TestApply_InPlaceBatchFollowsARuntimeApply(t *testing.T) {
	agent := agenttest.New(t)
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)
	endpoint := agentEndpoint(agent, "haproxy-0")

	plan1, config1, aux1 := renderFor("plan-1", "10.0.0.1", mapEntry)
	deployTo(t, component, bus, plan1, config1, aux1, "config_validation", endpoint)
	plan2, config2, aux2 := renderFor("plan-2", "10.0.0.2", mapEntry)
	completed := deployTo(t, component, bus, plan2, config2, aux2, "config_validation", endpoint)
	require.Equal(t, 1, completed.Succeeded)
	require.Equal(t, plan2.ID, agent.State().WorkerOpsPlanID, "the runtime apply moved the worker")

	agent.SetReloadPending(true)
	plan3, config3, aux3 := renderFor("plan-3", "10.0.0.3", mapEntry)
	completed = deployTo(t, component, bus, plan3, config3, aux3, "config_validation", endpoint)
	require.Equal(t, 0, completed.Failed)
	applies := agent.Applies()
	require.Len(t, applies, 3)
	third := applies[2].Manifest
	require.Equal(t, []string{api.OpServerSetAddr}, opKinds(third.InPlaceOps))
	assert.Equal(t, "10.0.0.3", third.InPlaceOps[0].Address)
	assert.Equal(t, plan2.ID, third.ExpectedWorkerOpsPlanID)
	assert.True(t, applies[2].Result.OK, "%+v", applies[2].Result.Error)
}

func TestApply_LargeBatchReloadsAnAgentWithoutRuntimeBatches(t *testing.T) {
	agent := agenttest.New(t, agenttest.WithoutRuntimeBatches())
	bus := newTestBus(t)
	component := createTestDeployer(bus.EventBus)
	endpoint := agentEndpoint(agent, "haproxy-0")
	plan1, config1, aux1 := renderWithServers("plan-1", 10)
	deployTo(t, component, bus, plan1, config1, aux1, "config_validation", endpoint)
	plan2, config2, aux2 := renderWithServers("plan-2", 20)
	completed := deployTo(t, component, bus, plan2, config2, aux2, "config_validation", endpoint)
	require.Equal(t, 1, completed.Succeeded)
	assert.Equal(t, 1, completed.ReloadsTriggered)
	applies := agent.Applies()
	require.Len(t, applies, 2)
	assert.Equal(t, api.ModeReload, applies[1].Manifest.Mode)
	assert.Empty(t, applies[1].Manifest.RuntimeOps())
}
