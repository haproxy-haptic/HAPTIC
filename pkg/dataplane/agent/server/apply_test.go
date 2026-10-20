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

package server_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/haproxytest"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
)

func baseFiles(config string) []file {
	return []file{
		{Path: configPath, Content: config, Reload: true},
		{Path: "maps/host.map", Content: "example.com be-a\n"},
	}
}

// firstApply is the fresh-pod case: the baseline is unknown, so the whole set
// is written and reloaded.
func firstApply(t *testing.T, h *harness) api.ApplyResult {
	t.Helper()
	files := baseFiles("global\n")
	m := buildManifest("plan-1", files)
	m.Mode = api.ModeReload
	result := h.apply(&m, files)
	require.True(t, result.OK, "%+v", result.Error)
	return result
}

func TestFirstApplyWritesTheTreeAndReloads(t *testing.T) {
	h := newHarness(t)
	result := firstApply(t, h)

	assert.Equal(t, api.ResultReload, result.Mode)
	assert.Equal(t, "plan-1", result.AppliedPlanID)
	assert.Equal(t, "plan-1", result.RunningPlanID)
	assert.Equal(t, "plan-1", result.LKGPlanID)
	require.NotNil(t, result.Reload)
	assert.True(t, result.Reload.Performed)
	assert.Equal(t, "global\n", h.read(configPath))
	assert.Equal(t, "example.com be-a\n", h.read("maps/host.map"))

	state := h.state(false)
	assert.Equal(t, uint64(1), state.Generation)
	assert.Equal(t, api.Version, state.APIVersion)
	assert.Contains(t, state.AgentOps, api.OpBackendAdd)
	assert.Len(t, state.Files, 2)
	assert.Zero(t, h.metric("haptic_agent_invariant_violations_total"))
}

func TestManifestWithoutIdentityVersionIsRejected(t *testing.T) {
	h := newHarness(t)
	first := firstApply(t, h)
	files := baseFiles("global\nchanged\n")
	m := buildManifest("plan-2", files)
	m.IdentityVersion = 0
	m.Mode = api.ModeReload
	m.ExpectedPrevPlanID = first.AppliedPlanID
	m.ExpectedPrevToken = first.AppliedToken

	status, raw := h.postRaw(&m, files)
	require.Equal(t, http.StatusBadRequest, status, string(raw))
	assert.Contains(t, string(raw), "identity_version")
	assert.Equal(t, "global\n", h.read(configPath))
	assert.Equal(t, first.AppliedPlanID, h.state(false).AppliedPlanID)
}

func TestRuntimeApplyRunsOpsWithoutReloading(t *testing.T) {
	h := newHarness(t)
	first := firstApply(t, h)
	maxconn := 750

	files := baseFiles("global\n")
	files[1].Content = "example.com be-a\nnew.example.com be-b\n"
	m := buildManifest("plan-2", files)
	m.ExpectedPrevPlanID = first.AppliedPlanID
	m.ExpectedPrevToken = first.AppliedToken
	m.Ops = []api.Op{
		{Kind: api.OpBackendAdd, Backend: "be-b", Profile: "prof", Mode: "http"},
		{Kind: api.OpServerAdd, Backend: "be-b", Server: "srv1", Address: "10.0.0.9", Port: 80},
		{Kind: api.OpServerEnable, Backend: "be-b", Server: "srv1"},
		{Kind: api.OpServerSetMaxconn, Backend: "be-b", Server: "srv1", Maxconn: &maxconn},
		{Kind: api.OpBackendPublish, Backend: "be-b"},
		{Kind: api.OpMapAdd, Path: "maps/host.map", Key: "new.example.com", Value: "be-b"},
	}
	result := h.apply(&m, files)

	require.True(t, result.OK, "%+v", result.Error)
	assert.Equal(t, api.ResultRuntime, result.Mode)
	assert.Nil(t, result.Reload)
	assert.Equal(t, "plan-1", result.RunningPlanID, "a runtime apply does not advance the running plan")
	assert.True(t, h.model.HasBackend("be-b"))
	h.model.With(func(m *haproxytest.Model) {
		assert.Equal(t, 750, m.Backends["be-b"].Servers[0].Maxconn)
	})
	assert.Equal(t, uint64(2), h.state(false).Generation)
	assert.Equal(t, 1.0, h.metric("haptic_agent_apply_total", api.ResultRuntime))
	assert.Zero(t, h.metric("haptic_agent_invariant_violations_total"))
}

func TestFencingRefusesAndNeverWrites(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*api.Manifest)
		reason string
	}{
		{
			name:   "previous plan does not match",
			mutate: func(m *api.Manifest) { m.ExpectedPrevPlanID = "plan-elsewhere" },
			reason: "prev_mismatch",
		},
		{
			name:   "previous token does not match",
			mutate: func(m *api.Manifest) { m.ExpectedPrevToken = api.Token{LeaderEpoch: 1, RenderSeq: 99} },
			reason: "prev_mismatch",
		},
		{
			name: "a former leader is still dispatching",
			mutate: func(m *api.Manifest) {
				m.Token = api.Token{LeaderEpoch: 0, RenderSeq: 1}
			},
			reason: "stale_epoch",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			files := baseFiles("global\n")
			m := buildManifest("plan-1", files)
			m.Mode = api.ModeReload
			m.Token = api.Token{LeaderEpoch: 4, RenderSeq: 1}
			require.True(t, h.apply(&m, files).OK)

			next := baseFiles("global\n  maxconn 100\n")
			nextManifest := buildManifest("plan-2", next)
			nextManifest.ExpectedPrevPlanID = "plan-1"
			nextManifest.ExpectedPrevToken = api.Token{LeaderEpoch: 4, RenderSeq: 1}
			nextManifest.Token = api.Token{LeaderEpoch: 4, RenderSeq: 2}
			tc.mutate(&nextManifest)

			status, raw := h.post(&nextManifest, next)
			require.Equal(t, http.StatusConflict, status, string(raw))
			conflict := api.Conflict{}
			require.NoError(t, json.Unmarshal(raw, &conflict))
			assert.Equal(t, tc.reason, conflict.Reason)
			assert.Equal(t, "plan-1", conflict.AppliedPlanID)
			assert.Equal(t, "global\n", h.read(configPath), "a refused apply must not write")
		})
	}
}

func TestUnknownBaselineIsItsOwnConflictReason(t *testing.T) {
	h := newHarness(t)
	files := baseFiles("global\n")
	m := buildManifest("plan-1", files)
	m.ExpectedPrevPlanID = "plan-0"

	status, raw := h.post(&m, files)
	require.Equal(t, http.StatusConflict, status)
	conflict := api.Conflict{}
	require.NoError(t, json.Unmarshal(raw, &conflict))
	assert.Equal(t, "unknown_baseline", conflict.Reason)
	assert.False(t, h.exists(configPath))
}

func TestMissingPartsAreNamed(t *testing.T) {
	h := newHarness(t)
	files := baseFiles("global\n")
	m := buildManifest("plan-1", files)
	m.Mode = api.ModeReload

	status, raw := h.post(&m, files, "maps/host.map")
	require.Equal(t, http.StatusConflict, status)
	missing := api.Missing{}
	require.NoError(t, json.Unmarshal(raw, &missing))
	assert.Equal(t, []string{"maps/host.map"}, missing.Missing)
	assert.False(t, h.exists(configPath), "no part lands while one is missing")
}

func TestUnchangedFilesNeedNoParts(t *testing.T) {
	h := newHarness(t)
	first := firstApply(t, h)

	files := baseFiles("global\n")
	m := buildManifest("plan-2", files)
	m.ExpectedPrevPlanID = first.AppliedPlanID
	m.ExpectedPrevToken = first.AppliedToken

	status, raw := h.post(&m, files, configPath, "maps/host.map")
	require.Equal(t, http.StatusOK, status, string(raw))
	result := api.ApplyResult{}
	require.NoError(t, json.Unmarshal(raw, &result))
	assert.Equal(t, api.ResultNoop, result.Mode)
	assert.Equal(t, "plan-2", result.AppliedPlanID)
}

func TestAPartThatDoesNotMatchItsDigestIsRefused(t *testing.T) {
	h := newHarness(t)
	files := baseFiles("global\n")
	m := buildManifest("plan-1", files)
	m.Mode = api.ModeReload
	files[0].Content = "global\n  tampered\n"

	status, raw := h.post(&m, files)
	require.Equal(t, http.StatusBadRequest, status, string(raw))
	assert.Contains(t, string(raw), "manifest digest")
	assert.False(t, h.exists(configPath))
}

func TestAbsenceDeletesAnOwnedPath(t *testing.T) {
	h := newHarness(t)
	first := firstApply(t, h)

	files := []file{{Path: configPath, Content: "global\n", Reload: true}}
	m := buildManifest("plan-2", files)
	m.ExpectedPrevPlanID = first.AppliedPlanID
	m.ExpectedPrevToken = first.AppliedToken

	result := h.apply(&m, files)
	require.True(t, result.OK)
	assert.False(t, h.exists("maps/host.map"))
	assert.Equal(t, map[string]string{configPath: "global\n"}, h.tree())
}

func TestUnknownOpFallsBackToAReload(t *testing.T) {
	h := newHarness(t)
	first := firstApply(t, h)

	files := baseFiles("global\n  maxconn 200\n")
	m := buildManifest("plan-2", files)
	m.ExpectedPrevPlanID = first.AppliedPlanID
	m.ExpectedPrevToken = first.AppliedToken
	m.Ops = []api.Op{{Kind: "backend_teleport", Backend: "be-a"}}

	result := h.apply(&m, files)
	require.True(t, result.OK, "%+v", result.Error)
	assert.Equal(t, api.ResultReload, result.Mode)
	assert.Equal(t, "plan-2", result.RunningPlanID)
	assert.Equal(t, 1.0, h.metric("haptic_agent_invariant_violations_total", "ops_executable"))
}

func TestARejectedOpReloadsTheDesiredSet(t *testing.T) {
	h := newHarness(t)
	first := firstApply(t, h)

	files := baseFiles("global\n  maxconn 300\n")
	m := buildManifest("plan-2", files)
	m.ExpectedPrevPlanID = first.AppliedPlanID
	m.ExpectedPrevToken = first.AppliedToken
	m.Ops = []api.Op{{Kind: api.OpServerAdd, Backend: "absent", Server: "srv1", Address: "10.0.0.1"}}

	result := h.apply(&m, files)
	require.True(t, result.OK, "%+v", result.Error)
	assert.Equal(t, api.ResultReload, result.Mode)
	assert.Equal(t, "global\n  maxconn 300\n", h.read(configPath))
	assert.Equal(t, 1.0, h.metric("haptic_agent_op_errors_total", api.OpServerAdd))
}

func TestAFailedReloadRestoresTheLastKnownGoodSet(t *testing.T) {
	h := newHarness(t)
	firstApply(t, h)

	h.model.With(func(m *haproxytest.Model) {
		m.ReloadFails = true
		m.ReloadLog = "[ALERT] config : parsing [haproxy.cfg:1] : unknown keyword 'nonsense'."
	})
	files := baseFiles("nonsense\n")
	m := buildManifest("plan-bad", files)
	m.Mode = api.ModeReload
	m.ExpectedPrevPlanID = "plan-1"

	result := h.apply(&m, files)
	require.False(t, result.OK)
	assert.Equal(t, api.ResultRejected, result.Mode)
	require.NotNil(t, result.Error)
	assert.Contains(t, result.Error.Message, "unknown keyword")
	require.NotNil(t, result.Rollback)
	assert.True(t, result.Rollback.Performed)
	assert.Equal(t, "global\n", h.read(configPath), "the tree is back on the last known good set")
	assert.Empty(t, result.AppliedPlanID, "a NACK invalidates the baseline")
	assert.Equal(t, 1.0, h.metric("haptic_agent_rollbacks_total"))
}

func TestTheSameRejectedManifestDoesNoWorkInsideTheCooldown(t *testing.T) {
	h := newHarness(t)
	firstApply(t, h)
	h.model.With(func(m *haproxytest.Model) { m.ReloadFails = true })

	files := baseFiles("nonsense\n")
	m := buildManifest("plan-bad", files)
	m.Mode = api.ModeReload
	m.ExpectedPrevPlanID = "plan-1"
	require.False(t, h.apply(&m, files).OK)
	reloadsAfterFirst := h.metric("haptic_agent_reloads_total", "failed")

	m.ExpectedPrevPlanID = ""
	result := h.apply(&m, files)
	assert.False(t, result.OK)
	assert.Equal(t, reloadsAfterFirst, h.metric("haptic_agent_reloads_total", "failed"),
		"a known-bad manifest must not reach HAProxy again")
	assert.Equal(t, "global\n", h.read(configPath))
}

// A revert lands the last known good set, so that is the plan the pod applied.
// Reporting the reverted-away plan would make the controller's next diff
// assume file contents this pod does not have.
func TestRevertLKGRestoresAndReloads(t *testing.T) {
	h := newHarness(t)
	first := firstApply(t, h)

	files := baseFiles("global\n  maxconn 400\n")
	files = append(files, file{Path: "maps/extra.map", Content: "later.example.com be-c\n"})
	m := buildManifest("plan-2", files)
	m.ExpectedPrevPlanID = first.AppliedPlanID
	m.ExpectedPrevToken = first.AppliedToken
	m.Mode = api.ModeAuto
	second := h.apply(&m, files)
	require.True(t, second.OK, "%+v", second.Error)
	require.Equal(t, "plan-1", second.LKGPlanID, "a file-only apply does not promote")
	require.Equal(t, "global\n  maxconn 400\n", h.read(configPath))

	revert := buildManifest(second.AppliedPlanID, nil)
	revert.Mode = api.ModeRevertLKG
	revert.Token = api.Token{LeaderEpoch: 1, RenderSeq: 3}
	result := h.apply(&revert, nil)

	require.True(t, result.OK, "%+v", result.Error)
	require.NotNil(t, result.Rollback)
	assert.True(t, result.Rollback.Performed)
	assert.True(t, result.Rollback.Reloaded)
	assert.Equal(t, "plan-1", result.AppliedPlanID, "the pod applied the last known good plan")
	assert.Equal(t, "plan-1", result.RunningPlanID)
	assert.Equal(t, "plan-1", result.LKGPlanID)
	assert.Equal(t, revert.Token, result.AppliedToken, "the token advances or the next apply is fenced out")
	assert.Equal(t, "global\n", h.read(configPath))
	assert.False(t, h.exists("maps/extra.map"), "a path the reverted plan created is gone")

	state := h.state(true)
	assert.Len(t, state.Files, 2, "the ownership set is the last known good one")
	assert.Contains(t, state.Files, "maps/host.map")
	assert.Empty(t, state.ReloadPendingAt)
	assert.Zero(t, h.metric("haptic_agent_invariant_violations_total"), h.violations())
}

// The scheduled reload of a plan the revert took off disk must not fire: it
// would record a plan id the pod is not serving as running and last known good.
func TestARevertConsumesTheScheduledReload(t *testing.T) {
	h := newHarness(t, withReloadInterval(time.Minute))
	files := baseFiles("global\n")
	m := buildManifest("plan-1", files)
	m.Mode = api.ModeReload
	first := h.apply(&m, files)
	require.True(t, first.OK, "%+v", first.Error)

	next := baseFiles("global\n  maxconn 410\n")
	second := buildManifest("plan-2", next)
	second.Mode = api.ModeReload
	second.ExpectedPrevPlanID = first.AppliedPlanID
	second.ExpectedPrevToken = first.AppliedToken
	scheduled := h.apply(&second, next)
	require.Equal(t, api.ResultScheduled, scheduled.Mode)
	require.NotEmpty(t, h.state(false).ReloadPendingAt)

	revert := buildManifest(scheduled.AppliedPlanID, nil)
	revert.Mode = api.ModeRevertLKG
	result := h.apply(&revert, nil)

	require.True(t, result.OK, "%+v", result.Error)
	assert.Empty(t, h.state(false).ReloadPendingAt, "the revert's reload consumed the scheduled one")
	assert.Equal(t, "plan-1", h.state(false).RunningPlanID)
	assert.Equal(t, "global\n", h.read(configPath))
}

// A 409 is never a write: the manifest that carries a validated plan id is
// refused before anything can advance the rollback baseline.
func TestAFencedApplyDoesNotPromoteTheLKG(t *testing.T) {
	h := newHarness(t)
	files := baseFiles("global\n")
	m := buildManifest("plan-1", files)
	m.Mode = api.ModeReload
	m.Token = api.Token{LeaderEpoch: 4, RenderSeq: 1}
	first := h.apply(&m, files)
	require.True(t, first.OK)

	changed := []file{files[0], {Path: "maps/host.map", Content: "example.com be-a\nb.example.com be-b\n"}}
	fileOnly := buildManifest("plan-2", changed)
	fileOnly.ExpectedPrevPlanID = first.AppliedPlanID
	fileOnly.ExpectedPrevToken = first.AppliedToken
	fileOnly.Token = api.Token{LeaderEpoch: 4, RenderSeq: 2}
	second := h.apply(&fileOnly, changed)
	require.True(t, second.OK, "%+v", second.Error)
	require.Equal(t, api.ResultFileOnly, second.Mode)
	require.Equal(t, "plan-1", second.LKGPlanID, "only a reload promotes the last known good plan")
	journalled := h.persisted()["journal"]

	// A deposed leader retries with a stale epoch and reports that its own
	// haproxy -c passed on the plan this pod already applied.
	stale := buildManifest("plan-3", changed)
	stale.ExpectedPrevPlanID = second.AppliedPlanID
	stale.ExpectedPrevToken = second.AppliedToken
	stale.Token = api.Token{LeaderEpoch: 3, RenderSeq: 9}
	stale.ValidatedPlanID = "plan-2"
	status, raw := h.post(&stale, changed)

	require.Equal(t, http.StatusConflict, status, string(raw))
	assert.Equal(t, "plan-1", h.state(false).LKGPlanID, "a refused apply must not advance the last known good plan")
	assert.Equal(t, journalled, h.persisted()["journal"], "a refused apply must not clear the journal")
}

// The controller reads the applied plan back to recover a baseline its own
// cache lost, so it has to describe the plan the pod reports as applied.
func TestStateReturnsThePlanOfTheAppliedPlanID(t *testing.T) {
	h := newHarness(t)
	files := baseFiles("global\n")
	m := buildManifest("plan-1", files)
	m.Mode = api.ModeReload
	first := h.applyWithPlan(&m, files, []byte("opaque-plan-1"))
	require.True(t, first.OK, "%+v", first.Error)

	state := h.state(false)
	assert.Equal(t, []byte("opaque-plan-1"), state.AppliedPlan)
	assert.Equal(t, "plan-1", state.AppliedPlanID)
	assert.True(t, state.AppliedPlanStored)
	// A controller that holds the plan reads without the blob and still
	// learns that the pod keeps one.
	light := h.stateRead(api.StateRead{})
	assert.Empty(t, light.AppliedPlan)
	assert.True(t, light.AppliedPlanStored)
	assert.True(t, light.HoldsAppliedPlan())

	// An apply that carries no plan advances the applied id, so the stored
	// blob no longer describes it and must not be handed out.
	next := buildManifest("plan-2", files)
	next.ExpectedPrevPlanID = first.AppliedPlanID
	next.ExpectedPrevToken = first.AppliedToken
	require.True(t, h.apply(&next, files).OK)
	assert.Empty(t, h.state(false).AppliedPlan, "a blob for another plan is not a baseline")
	assert.False(t, h.stateRead(api.StateRead{}).AppliedPlanStored)

	// A restart invalidates every in-process observation proof. The blob stays
	// opaque until a full reload binds fresh bytes to a fresh agent proof.
	third := buildManifest("plan-3", files)
	third.ExpectedPrevPlanID = "plan-2"
	third.ExpectedPrevToken = next.Token
	require.True(t, h.applyWithPlan(&third, files, []byte("opaque-plan-3")).OK)
	restarted := h.restart()
	restartedState := restarted.state(false)
	assert.Empty(t, restartedState.AppliedPlan)
	assert.Empty(t, restartedState.AppliedPlanProof)

	fourth := buildManifest("plan-4", files)
	fourth.ExpectedPrevPlanID = "plan-3"
	fourth.ExpectedPrevToken = third.Token
	result := restarted.applyWithPlan(&fourth, files, []byte("opaque-plan-4"))
	require.Equal(t, api.ResultReload, result.Mode)
	require.NotEmpty(t, result.AppliedPlanProof)
	assert.Equal(t, []byte("opaque-plan-4"), restarted.state(false).AppliedPlan)
}

// A store the agent created at runtime is loaded from then on, so the next
// diff must see it; otherwise the controller composes cert_new again and
// HAProxy refuses it because the store is already there.
func TestACreatedCertificateEntersTheInventory(t *testing.T) {
	h := newHarness(t)
	pem := "-----BEGIN CERTIFICATE-----\nx\n-----BEGIN PRIVATE KEY-----\ny\n"
	files := []file{
		{Path: configPath, Content: "global\n", Reload: true},
		{Path: "ssl/new.pem", Content: pem, Kind: api.FileKindCert},
	}
	m := buildManifest("plan-1", files)
	m.Mode = api.ModeReload
	first := h.apply(&m, files)
	require.True(t, first.OK, "%+v", first.Error)
	require.NotContains(t, h.state(false).Inventory.Certs, "ssl/new.pem")

	create := buildManifest("plan-2", files)
	create.ExpectedPrevPlanID = first.AppliedPlanID
	create.ExpectedPrevToken = first.AppliedToken
	create.Ops = []api.Op{{Kind: api.OpCertNew, Path: "ssl/new.pem"}}
	result := h.apply(&create, files)

	require.True(t, result.OK, "%+v", result.Error)
	require.NotNil(t, result.Inventory, "the ACK carries the delta")
	assert.Contains(t, result.Inventory.Certs, "ssl/new.pem")
	assert.Contains(t, h.state(false).Inventory.Certs, "ssl/new.pem")
}

func TestVerifiedStateDuringReloadPreservesConfirmedBaseline(t *testing.T) {
	h := newHarness(t)
	first := firstApply(t, h)

	var once sync.Once
	verified := make(chan api.State, 1)
	h.model.With(func(m *haproxytest.Model) {
		m.Reject = func(command string) (string, bool) {
			if command == "reload" {
				once.Do(func() {
					h.model.With(func(inner *haproxytest.Model) { inner.Pid += 5 })
					go func() { verified <- h.state(true) }()
				})
			}
			return "", false
		}
	})

	files := baseFiles("global\n  maxconn 1300\n")
	m := buildManifest("plan-2", files)
	m.Mode = api.ModeReload
	m.ExpectedPrevPlanID = first.AppliedPlanID
	m.ExpectedPrevToken = first.AppliedToken
	result := h.apply(&m, files)

	require.True(t, result.OK, "%+v", result.Error)
	assert.Equal(t, "plan-2", result.RunningPlanID, "the reload did happen")
	assert.Equal(t, "plan-2", result.AppliedPlanID)
	select {
	case state := <-verified:
		assert.Equal(t, result.HAProxy, state.HAProxy)
	case <-time.After(time.Second):
		t.Fatal("verified state did not resume after reload")
	}
	assert.Equal(t, "plan-2", h.state(true).AppliedPlanID)
}

func TestFailedApplyDoesNotPublishDeferredDeletes(t *testing.T) {
	for _, kind := range []string{"unknown-op", api.OpServerDisable} {
		t.Run(kind, func(t *testing.T) {
			h := newHarness(t)
			first := firstApply(t, h)
			h.model.With(func(m *haproxytest.Model) { m.ReloadFails = true })
			files := baseFiles("global\n")
			m := buildManifest("plan-2", files)
			m.ExpectedPrevPlanID, m.ExpectedPrevToken = first.AppliedPlanID, first.AppliedToken
			m.Ops = []api.Op{
				{Kind: kind, Backend: "missing", Server: "srv"},
				{Kind: api.OpServerDel, Backend: "missing", Server: "srv"},
				{Kind: api.OpBackendDel, Backend: "missing"},
			}
			result := h.apply(&m, files)
			require.False(t, result.OK)
			state := h.state(false)
			assert.Empty(t, state.PendingDeletes.Servers)
			assert.Empty(t, state.PendingDeletes.Backends)
			assert.False(t, sent(h, "wait "))
		})
	}
}

func TestADeferredDeleteWaitsForTheInlineHalf(t *testing.T) {
	h := newHarness(t)
	first := firstApply(t, h)

	files := baseFiles("global\n")
	added := buildManifest("plan-2", files)
	added.ExpectedPrevPlanID = first.AppliedPlanID
	added.ExpectedPrevToken = first.AppliedToken
	added.Ops = []api.Op{
		{Kind: api.OpBackendAdd, Backend: "be-b", Profile: "prof", Mode: "http"},
		{Kind: api.OpServerAdd, Backend: "be-b", Server: "srv1", Address: "10.0.0.9", Port: 80},
		{Kind: api.OpBackendPublish, Backend: "be-b"},
	}
	second := h.apply(&added, files)
	require.True(t, second.OK, "%+v", second.Error)

	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	var once sync.Once
	h.model.With(func(m *haproxytest.Model) {
		m.Reject = func(command string) (string, bool) {
			if strings.HasPrefix(command, "disable server") {
				once.Do(func() { close(entered); <-release })
			}
			return "", false
		}
	})

	removed := buildManifest("plan-3", files)
	removed.ExpectedPrevPlanID = second.AppliedPlanID
	removed.ExpectedPrevToken = second.AppliedToken
	removed.Ops = []api.Op{
		{Kind: api.OpServerDisable, Backend: "be-b", Server: "srv1"},
		{Kind: api.OpServerWaitRemovable, Backend: "be-b", Server: "srv1", TimeoutMs: 2000},
		{Kind: api.OpServerDel, Backend: "be-b", Server: "srv1"},
	}
	done := make(chan api.ApplyResult, 1)
	go func() { done <- h.apply(&removed, files) }()

	<-entered
	assert.Empty(t, h.state(false).PendingDeletes.Servers, "an uncommitted batch must not be visible to the drainer")
	require.Never(t, func() bool { return sent(h, "wait ") }, 300*time.Millisecond, 20*time.Millisecond,
		"the queue must not wait on a server the apply has not disabled yet")
	unblock()

	require.True(t, (<-done).OK)
	require.Eventually(t, func() bool {
		return len(h.model.ServerNames("be-b")) == 0
	}, 10*time.Second, 20*time.Millisecond)
	assert.Less(t, index(h, "disable server"), index(h, "wait "), "the inline half runs first")
}

// sent reports whether the worker has been asked anything starting with prefix.
func sent(h *harness, prefix string) bool { return index(h, prefix) >= 0 }

func index(h *harness, prefix string) int {
	for i, command := range h.model.Sent() {
		if strings.HasPrefix(command, prefix) {
			return i
		}
	}
	return -1
}

// /v1/state answers while an apply mutates the tree; the response must not
// share the live map with the writer.
func TestStateAndApplyRunConcurrently(t *testing.T) {
	h := newHarness(t)
	first := firstApply(t, h)
	applied, token := first.AppliedPlanID, first.AppliedToken

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					h.state(true)
				}
			}
		}()
	}
	for i := range 12 {
		files := baseFiles(fmt.Sprintf("global\n  maxconn %d\n", 2000+i))
		files = append(files, file{
			Path:    fmt.Sprintf("maps/n%d.map", i),
			Content: fmt.Sprintf("host%d.example.com be-a\n", i),
		})
		m := buildManifest(fmt.Sprintf("plan-%d", i+2), files)
		m.ExpectedPrevPlanID = applied
		m.ExpectedPrevToken = token
		m.Token = api.Token{RenderSeq: uint64(i + 2)}
		result := h.apply(&m, files)
		require.True(t, result.OK, "%+v", result.Error)
		applied, token = result.AppliedPlanID, result.AppliedToken
	}
	close(stop)
	readers.Wait()
}

func TestLKGPromotionClearsTheJournal(t *testing.T) {
	h := newHarness(t)
	first := firstApply(t, h)

	files := baseFiles("global\n  maxconn 500\n")
	m := buildManifest("plan-2", files)
	m.ExpectedPrevPlanID = first.AppliedPlanID
	m.ExpectedPrevToken = first.AppliedToken
	m.Ops = []api.Op{{Kind: api.OpMapAdd, Path: "maps/host.map", Key: "example.com", Value: "be-a"}}
	second := h.apply(&m, files)
	require.True(t, second.OK, "%+v", second.Error)
	require.Equal(t, "plan-1", second.LKGPlanID, "a runtime apply does not promote by itself")

	noop := buildManifest("plan-3", files)
	noop.ExpectedPrevPlanID = second.AppliedPlanID
	noop.ExpectedPrevToken = second.AppliedToken
	noop.ValidatedPlanID = "plan-2"
	promoted := h.apply(&noop, files)

	require.True(t, promoted.OK, "%+v", promoted.Error)
	assert.Equal(t, "plan-2", promoted.LKGPlanID)
	assert.Zero(t, h.metric("haptic_agent_invariant_violations_total"))
}

func TestAScheduledReloadCoalescesAndRunsInPlaceOps(t *testing.T) {
	h := newHarness(t, withReloadInterval(time.Minute))
	files := baseFiles("global\n")
	m := buildManifest("plan-1", files)
	m.Mode = api.ModeReload
	first := h.apply(&m, files)
	require.True(t, first.OK)

	next := baseFiles("global\n  maxconn 600\n")
	second := buildManifest("plan-2", next)
	second.Mode = api.ModeReload
	second.ExpectedPrevPlanID = first.AppliedPlanID
	second.ExpectedPrevToken = first.AppliedToken
	scheduled := h.apply(&second, next)
	require.True(t, scheduled.OK, "%+v", scheduled.Error)
	require.Equal(t, api.ResultScheduled, scheduled.Mode)
	require.NotNil(t, scheduled.Reload)
	assert.NotEmpty(t, scheduled.Reload.ScheduledAt)
	assert.NotEmpty(t, h.state(false).ReloadPendingAt)
	// The controller follows up at this instant; a second-granular stamp made
	// it arrive up to a second early and re-apply every 250 ms until the pacer
	// fired.
	due, err := time.Parse(time.RFC3339, scheduled.Reload.ScheduledAt)
	require.NoError(t, err)
	assert.Equal(t, scheduled.Reload.ScheduledAt, due.UTC().Format(time.RFC3339Nano), "scheduled_at keeps sub-second precision")
	assert.Equal(t, scheduled.Reload.ScheduledAt, h.state(false).ReloadPendingAt)

	third := buildManifest("plan-3", next)
	third.ExpectedPrevPlanID = scheduled.AppliedPlanID
	third.ExpectedPrevToken = scheduled.AppliedToken
	third.ExpectedWorkerOpsPlanID = scheduled.WorkerOpsPlanID
	third.WorkerOpsPlanID = "plan-1-after"
	third.InPlaceOps = []api.Op{{Kind: api.OpMapAdd, Path: "maps/host.map", Key: "example.com", Value: "be-c"}}
	coalesced := h.apply(&third, next)

	require.True(t, coalesced.OK, "%+v", coalesced.Error)
	assert.Equal(t, api.ResultScheduled, coalesced.Mode)
	assert.Equal(t, "plan-1-after", coalesced.WorkerOpsPlanID, "the worker is at the derived plan, not the render")
	require.NotNil(t, coalesced.Reload)
	assert.Equal(t, scheduled.Reload.ScheduledAt, coalesced.Reload.ScheduledAt, "a coalesced apply says when the reload fires")
	assert.Equal(t, "global\n  maxconn 600\n", h.read(configPath), "the files land even while a reload waits")
}

// A structural apply that arrives with the window closed but no reload
// pending is paced like any other, and its in-place ops run at once: the
// worker keeps serving until the reload fires and must not serve stale
// endpoints meanwhile.
func TestAPacedReloadRunsTheInPlaceOpsAtOnce(t *testing.T) {
	h := newHarness(t, withReloadInterval(time.Minute))
	first := firstApply(t, h)
	require.Empty(t, h.state(false).ReloadPendingAt)

	next := baseFiles("global\n  maxconn 600\n")
	next[1].Content = "example.com be-a\nnew.example.com be-b\n"
	second := buildManifest("plan-2", next)
	second.Mode = api.ModeReload
	second.ExpectedPrevPlanID = first.AppliedPlanID
	second.ExpectedPrevToken = first.AppliedToken
	second.ExpectedWorkerOpsPlanID = "plan-1"
	second.WorkerOpsPlanID = "plan-1-after"
	second.InPlaceOps = []api.Op{{Kind: api.OpMapAdd, Path: "maps/host.map", Key: "new.example.com", Value: "be-b"}}
	result := h.apply(&second, next)

	require.True(t, result.OK, "%+v", result.Error)
	assert.Equal(t, api.ResultScheduled, result.Mode)
	require.NotNil(t, result.Reload)
	assert.NotEmpty(t, result.Reload.ScheduledAt)
	require.Len(t, result.OpResults, 1, "the in-place op ran while the reload waits")
	assert.True(t, result.OpResults[0].OK)
	assert.Equal(t, "plan-1-after", result.WorkerOpsPlanID)
	assert.Contains(t, h.model.MapEntries("maps/host.map"), haproxytest.MapEntry{Key: "new.example.com", Value: "be-b"})
}

func TestAnInPlaceOpOnAStaleWorkerBaselineIsAConflict(t *testing.T) {
	h := newHarness(t, withReloadInterval(time.Minute))
	files := baseFiles("global\n")
	m := buildManifest("plan-1", files)
	m.Mode = api.ModeReload
	first := h.apply(&m, files)

	next := baseFiles("global\n  maxconn 700\n")
	second := buildManifest("plan-2", next)
	second.Mode = api.ModeReload
	second.ExpectedPrevPlanID = first.AppliedPlanID
	second.ExpectedPrevToken = first.AppliedToken
	scheduled := h.apply(&second, next)
	require.Equal(t, api.ResultScheduled, scheduled.Mode)

	third := buildManifest("plan-3", next)
	third.ExpectedPrevPlanID = scheduled.AppliedPlanID
	third.ExpectedPrevToken = scheduled.AppliedToken
	third.ExpectedWorkerOpsPlanID = "plan-from-another-life"
	third.WorkerOpsPlanID = "plan-from-another-life-after"
	third.InPlaceOps = []api.Op{{Kind: api.OpMapAdd, Path: "maps/host.map", Key: "example.com", Value: "be-c"}}
	conflict := h.applyConflict(&third, next)

	assert.Equal(t, "worker_ops_mismatch", conflict.Reason)
	assert.Equal(t, scheduled.WorkerOpsPlanID, conflict.WorkerOpsPlanID, "the caller re-diffs against this worker")
	assert.Equal(t, scheduled.AppliedPlanID, h.state(false).AppliedPlanID, "nothing was written or invalidated")
}

// A runtime apply moves the worker to the applied plan, so an in-place batch
// composed against that plan — not against the plan of the last reload — is
// the one the pod accepts once a reload is pending.
func TestARuntimeApplyMovesTheWorkerOpsBaseline(t *testing.T) {
	h := newHarness(t, withReloadInterval(time.Minute))
	first := firstApply(t, h)
	assert.Equal(t, "plan-1", h.state(false).WorkerOpsPlanID)

	routed := baseFiles("global\n")
	routed[1].Content = "example.com be-a\nnew.example.com be-b\n"
	runtime := buildManifest("plan-2", routed)
	runtime.ExpectedPrevPlanID = first.AppliedPlanID
	runtime.ExpectedPrevToken = first.AppliedToken
	runtime.Ops = []api.Op{{Kind: api.OpMapAdd, Path: "maps/host.map", Key: "new.example.com", Value: "be-b"}}
	applied := h.apply(&runtime, routed)
	require.True(t, applied.OK, "%+v", applied.Error)
	require.Equal(t, api.ResultRuntime, applied.Mode)
	assert.Equal(t, "plan-2", applied.WorkerOpsPlanID, "every op ran on the worker, so it holds plan-2")
	assert.Equal(t, "plan-1", applied.RunningPlanID)

	structural := baseFiles("global\n  maxconn 600\n")
	structural[1].Content = routed[1].Content
	third := buildManifest("plan-3", structural)
	third.Mode = api.ModeReload
	third.ExpectedPrevPlanID = applied.AppliedPlanID
	third.ExpectedPrevToken = applied.AppliedToken
	scheduled := h.apply(&third, structural)
	require.Equal(t, api.ResultScheduled, scheduled.Mode)
	assert.Equal(t, "plan-2", scheduled.WorkerOpsPlanID, "a scheduled apply changes no worker state")

	fourth := buildManifest("plan-4", structural)
	fourth.ExpectedPrevPlanID = scheduled.AppliedPlanID
	fourth.ExpectedPrevToken = scheduled.AppliedToken
	fourth.ExpectedWorkerOpsPlanID = "plan-2"
	fourth.WorkerOpsPlanID = "plan-2-after"
	fourth.InPlaceOps = []api.Op{{Kind: api.OpMapDel, Path: "maps/host.map", Key: "new.example.com"}}
	inPlace := h.apply(&fourth, structural)
	require.True(t, inPlace.OK, "%+v", inPlace.Error)
	assert.Nil(t, inPlace.Error)
	assert.Equal(t, "plan-2-after", inPlace.WorkerOpsPlanID)
}

func TestTheScheduledReloadFiresWhenTheWindowPasses(t *testing.T) {
	// The window only has to still be open when the second apply lands; both
	// tests then wait up to 10s for the reload to fire. 300ms lost that race
	// under CI's parallel load and made these the suite's only flakes.
	h := newHarness(t, withReloadInterval(2*time.Second))
	files := baseFiles("global\n")
	m := buildManifest("plan-1", files)
	m.Mode = api.ModeReload
	first := h.apply(&m, files)

	next := baseFiles("global\n  maxconn 800\n")
	second := buildManifest("plan-2", next)
	second.Mode = api.ModeReload
	second.ExpectedPrevPlanID = first.AppliedPlanID
	second.ExpectedPrevToken = first.AppliedToken
	require.Equal(t, api.ResultScheduled, h.apply(&second, next).Mode)

	require.Eventually(t, func() bool {
		return h.state(false).RunningPlanID == "plan-2"
	}, 10*time.Second, 20*time.Millisecond)
	assert.Empty(t, h.state(false).ReloadPendingAt)
}

// The controller polls last_apply for a scheduled reload's verdict and takes
// its applied plan as the next baseline, so a failed one must report the
// invalidated baseline, not the one from before the reload.
func TestAFailedScheduledReloadReportsTheInvalidatedBaseline(t *testing.T) {
	// The window only has to still be open when the second apply lands; both
	// tests then wait up to 10s for the reload to fire. 300ms lost that race
	// under CI's parallel load and made these the suite's only flakes.
	h := newHarness(t, withReloadInterval(2*time.Second))
	files := baseFiles("global\n")
	m := buildManifest("plan-1", files)
	m.Mode = api.ModeReload
	first := h.apply(&m, files)

	h.model.With(func(model *haproxytest.Model) { model.ReloadFails = true })
	next := baseFiles("global\n  broken\n")
	second := buildManifest("plan-2", next)
	second.Mode = api.ModeReload
	second.ExpectedPrevPlanID = first.AppliedPlanID
	second.ExpectedPrevToken = first.AppliedToken
	require.Equal(t, api.ResultScheduled, h.apply(&second, next).Mode)

	var last *api.ApplyResult
	require.Eventually(t, func() bool {
		last = h.state(false).LastApply
		return last != nil && last.PlanID == "plan-2" && last.Mode != api.ResultScheduled
	}, 10*time.Second, 20*time.Millisecond)
	assert.False(t, last.OK)
	assert.Empty(t, last.AppliedPlanID, "the NACK must carry the baseline the next apply has to expect")
	assert.Empty(t, h.state(false).AppliedPlanID)
	assert.Equal(t, "plan-1", last.LKGPlanID)
}

func TestStateVerifyObservesTheTree(t *testing.T) {
	h := newHarness(t)
	first := firstApply(t, h)
	held := h.state(false).Files

	require.NoError(t, writeFile(h, "maps/host.map", "tampered by someone else\n"))
	verified := h.state(true).Files
	assert.NotEqual(t, held["maps/host.map"].Digest, verified["maps/host.map"].Digest)
	assert.Empty(t, verified["maps/host.map"].Proof, "a rewritten file keeps no proof")
	assert.Equal(t, held[configPath], verified[configPath])

	files := baseFiles("global\n")
	m := buildManifest("plan-1", files)
	m.ExpectedPrevPlanID = first.AppliedPlanID
	m.ExpectedPrevToken = first.AppliedToken
	status, raw := h.post(&m, files, configPath, "maps/host.map")
	require.Equal(t, http.StatusConflict, status, string(raw))
	missing := api.Missing{}
	require.NoError(t, json.Unmarshal(raw, &missing))
	assert.Equal(t, []string{"maps/host.map"}, missing.Missing)

	status, raw = h.post(&m, files, configPath)
	require.Equal(t, http.StatusOK, status, string(raw))
	result := api.ApplyResult{}
	require.NoError(t, json.Unmarshal(raw, &result))
	assert.Equal(t, api.ResultFileOnly, result.Mode)
	assert.Equal(t, "example.com be-a\n", h.read("maps/host.map"))
}

// Drift prevention verifies before every round. A file that still holds the
// bytes its proof was issued for keeps the proof, so the round sends nothing.
func TestStateVerifyKeepsTheProofsOfAnUntouchedTree(t *testing.T) {
	h := newHarness(t)
	first := firstApply(t, h)
	held := h.state(false).Files

	assert.Equal(t, held, h.state(true).Files)

	files := baseFiles("global\n")
	m := buildManifest("plan-1", files)
	m.ExpectedPrevPlanID = first.AppliedPlanID
	m.ExpectedPrevToken = first.AppliedToken
	status, raw := h.post(&m, files, configPath, "maps/host.map")
	require.Equal(t, http.StatusOK, status, string(raw))
	result := api.ApplyResult{}
	require.NoError(t, json.Unmarshal(raw, &result))
	assert.Equal(t, api.ResultNoop, result.Mode)
}

// The digest is xxhash64, which is cheap to collide: a rewrite behind the agent
// with the same digest and size still loses its proof.
func TestStateVerifyDropsTheProofOfARewriteWithTheSameDigest(t *testing.T) {
	const written, colliding = "f16d1e21e4919d26 be\n", "7127d105c5942881 be\n"
	require.Equal(t, renderplan.DigestString(written), renderplan.DigestString(colliding))
	h := newHarness(t)
	files := []file{{Path: configPath, Content: "global\n", Reload: true}, {Path: "maps/host.map", Content: written}}
	m := buildManifest("plan-1", files)
	m.Mode = api.ModeReload
	first := h.apply(&m, files)
	require.True(t, first.OK, "%+v", first.Error)

	require.NoError(t, writeFile(h, "maps/host.map", colliding))
	observed := h.state(true).Files["maps/host.map"]
	assert.Equal(t, renderplan.DigestString(written), observed.Digest, "the digest cannot tell the two apart")
	assert.Empty(t, observed.Proof)
}

func TestAuthenticationIsRequiredForTheAPIButNotTheProbes(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{api.PathHealthz, api.PathReadyz} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.url+path, http.NoBody)
		require.NoError(t, err)
		response, err := h.client.Do(request)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		assert.Equal(t, http.StatusOK, response.StatusCode, path)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.url+api.PathState, http.NoBody)
	require.NoError(t, err)
	response, err := h.client.Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, http.StatusUnauthorized, response.StatusCode)

	request.SetBasicAuth(testUser, "wrong")
	response, err = h.client.Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
}

// The result says where the apply spent its time: the parts, the write and
// the ops each inside the total, so the controller can report the slowest
// pod's split with every deployment.
func TestApplyReportsWhereItsTimeWent(t *testing.T) {
	h := newHarness(t)
	first := firstApply(t, h)

	files := baseFiles("global\n  maxconn 500\n")
	m := buildManifest("plan-2", files)
	m.ExpectedPrevPlanID = first.AppliedPlanID
	m.ExpectedPrevToken = first.AppliedToken
	m.Ops = []api.Op{{Kind: api.OpMapAdd, Path: "maps/host.map", Key: "example.com", Value: "be-a"}}
	result := h.apply(&m, files)
	require.True(t, result.OK, "%+v", result.Error)

	timing := result.Timing
	assert.GreaterOrEqual(t, timing.StageMs, int64(0))
	assert.GreaterOrEqual(t, timing.WriteMs, int64(0))
	assert.GreaterOrEqual(t, timing.OpsMs, int64(0))
	assert.GreaterOrEqual(t, timing.TotalMs, timing.StageMs+timing.WriteMs+timing.OpsMs,
		"the phases are measured inside the total")
}

// A blob put after the apply is the pod's baseline like one that rode the
// apply: handed back for its plan, dropped with the proofs at a restart,
// refused once the applied plan moved on.
func TestPlanPutAfterTheApplyIsTheBaseline(t *testing.T) {
	h := newHarness(t)
	files := baseFiles("global\n")
	m := buildManifest("plan-1", files)
	m.Mode = api.ModeReload
	first := h.apply(&m, files)
	require.True(t, first.OK, "%+v", first.Error)
	assert.False(t, h.stateRead(api.StateRead{}).AppliedPlanStored, "the apply carried no blob")

	assert.Equal(t, http.StatusConflict, h.putPlan("plan-1", "not-the-proof", []byte("opaque-plan-1")))
	assert.Equal(t, http.StatusOK, h.putPlan("plan-1", first.AppliedPlanProof, []byte("opaque-plan-1")))
	state := h.state(false)
	assert.Equal(t, []byte("opaque-plan-1"), state.AppliedPlan)
	assert.True(t, state.AppliedPlanStored)

	// A restart invalidates every in-process proof, for a put blob as for one
	// that rode the apply: nothing is handed back until a new apply binds it.
	restarted := h.restart()
	assert.Empty(t, restarted.state(false).AppliedPlan)

	next := buildManifest("plan-2", files)
	next.ExpectedPrevPlanID = "plan-1"
	next.ExpectedPrevToken = first.AppliedToken
	second := restarted.apply(&next, files)
	require.True(t, second.OK, "%+v", second.Error)
	assert.Equal(t, http.StatusConflict, restarted.putPlan("plan-1", first.AppliedPlanProof, []byte("opaque-plan-1")),
		"the pod moved on; the old plan's blob is refused")
	assert.Empty(t, restarted.state(false).AppliedPlan)
	assert.Equal(t, http.StatusOK, restarted.putPlan("plan-2", second.AppliedPlanProof, []byte("opaque-plan-2")))
	assert.Equal(t, []byte("opaque-plan-2"), restarted.state(false).AppliedPlan)
}
