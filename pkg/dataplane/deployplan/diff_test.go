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

package deployplan_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/deployplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
)

// TestDiffOpOrder covers rule 8's sequence: everything the new state needs
// exists and is published before anything the old state used goes away.
func TestDiffOpOrder(t *testing.T) {
	prev := basePlan(
		withBackend(dynBackend("be-old", srv("SRV_1", "10.0.0.1", 8080))),
		withBackend(dynBackend("be-keep", srv("SRV_1", "10.0.0.3", 8080))),
		withMap(renderplan.Map{Path: routeMap, Entries: []renderplan.Entry{
			entry("keep.example.com", "be-keep"), entry("old.example.com", "be-old"),
		}}),
		withFile(&renderplan.File{Path: certPath, Kind: renderplan.FileKindCert, Digest: "before"}),
	)
	moved := dynBackend("be-keep", srv("SRV_1", "10.0.0.4", 8080))
	next := basePlan(
		withBackend(dynBackend("be-new", srv("SRV_1", "10.0.0.2", 8080))),
		withBackend(moved),
		withMap(renderplan.Map{Path: routeMap, Entries: []renderplan.Entry{
			entry("keep.example.com", "be-keep"), entry("new.example.com", "be-new"),
		}}),
		withFile(&renderplan.File{Path: certPath, Kind: renderplan.FileKindCert, Digest: "after"}),
	)
	base := withMapsLoaded(on34(prev), routeMap)
	base.Inventory.Certs = []string{certPath}

	got := deployplan.Diff(next, base)

	require.Equal(t, deployplan.VerdictRuntime, got.Verdict, got.Reasons)
	assert.Equal(t, []string{
		api.OpBackendAdd, api.OpServerAdd, api.OpServerEnable,
		api.OpServerSetAddr,
		api.OpBackendPublish,
		api.OpMapAdd,
		api.OpCertSet,
		api.OpMapDel,
		api.OpBackendUnpublish,
		api.OpServerDisable, api.OpServerWaitRemovable, api.OpServerDel,
		api.OpBackendWaitRemovable, api.OpBackendDel,
	}, kinds(got.Ops))
}

// TestDiffIsAllOrNothing covers rule 8: one item that needs a reload takes the
// whole apply with it, because the file set already carries everything.
func TestDiffIsAllOrNothing(t *testing.T) {
	prev := basePlan(
		withBackend(dynBackend("be-a", srv("SRV_1", "10.0.0.1", 8080))),
		withBackend(structuralBackend("be-b", srv("SRV_1", "10.0.0.5", 8080))),
	)
	changed := structuralBackend("be-b", srv("SRV_1", "10.0.0.5", 8080))
	changed.BodyDigest = renderplan.DigestString("stick-table changed")
	next := basePlan(
		withBackend(dynBackend("be-a", srv("SRV_1", "10.0.0.9", 8080))),
		withBackend(changed),
	)

	got := deployplan.Diff(next, on34(prev))

	assert.Equal(t, deployplan.VerdictReload, got.Verdict)
	assert.Equal(t, api.ModeReload, got.Mode)
	assert.Empty(t, got.Ops)
	assert.Zero(t, got.Chunks)
	assert.Len(t, got.Files, len(next.Files))
}

func TestDiffNoopRenderIsFileOnly(t *testing.T) {
	prev := basePlan(withBackend(dynBackend("be-a", srv("SRV_1", "10.0.0.1", 8080))))

	got := deployplan.Diff(basePlan(withBackend(dynBackend("be-a", srv("SRV_1", "10.0.0.1", 8080)))), on34(prev))

	assert.Equal(t, deployplan.VerdictFileOnly, got.Verdict)
	assert.Equal(t, api.ModeAuto, got.Mode)
	assert.Empty(t, got.Ops)
	assert.Empty(t, got.Reasons)
}

func TestDiffWithoutARenderReloads(t *testing.T) {
	got := deployplan.Diff(nil, on34(basePlan()))

	assert.Equal(t, deployplan.VerdictReload, got.Verdict)
	assert.Equal(t, []string{"no render"}, got.Reasons)
}

// TestDiffChunking covers the op cap: an apply is split, never dropped, until
// even the split would exceed what one sync may send.
func TestDiffChunking(t *testing.T) {
	tests := []struct {
		name    string
		servers int
		verdict deployplan.Verdict
		chunks  int
	}{
		{name: "one execution batch", servers: 400, verdict: deployplan.VerdictRuntime, chunks: 1},
		{name: "two execution batches", servers: 600, verdict: deployplan.VerdictRuntime, chunks: 2},
		{name: "beyond the chunk budget", servers: 4500, verdict: deployplan.VerdictReload},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prev := basePlan(withBackend(dynBackend("be-a", srv("SRV_0", "10.0.0.1", 8080))))
			next := basePlan(withBackend(dynBackend("be-a", weightedServers(tt.servers)...)))

			got := deployplan.Diff(next, on34(prev))

			require.Equal(t, tt.verdict, got.Verdict, got.Reasons)
			assert.Equal(t, tt.chunks, got.Chunks)
			if tt.verdict == deployplan.VerdictReload {
				reasonsContain(t, got.Reasons, "op cap")
				return
			}
			chunked := got.Chunk()
			require.Len(t, chunked, tt.chunks)
			total := 0
			for _, chunk := range chunked {
				assert.LessOrEqual(t, len(chunk), api.MaxOpsPerApply)
				total += len(chunk)
			}
			assert.Equal(t, len(got.Ops), total)
		})
	}
}

// The first apply carries the in-place batch as well and the agent's client
// bounds the two lists together, so a chunk composed as if it were alone is
// refused before it is sent — the pod does not even receive the files.
func TestDecisionChunkBudgetsTheInPlaceBatch(t *testing.T) {
	tests := []struct {
		name    string
		ops     int
		inPlace int
		want    []int
	}{
		{name: "no batch to make room for", ops: 1500, want: []int{1000, 500}},
		{name: "the batch takes its share of the first apply", ops: 1500, inPlace: 400, want: []int{600, 900}},
		{name: "a full batch leaves the first apply to itself", ops: 5, inPlace: api.MaxOpsPerApply, want: []int{0, 5}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision := deployplan.Decision{
				Ops:     make([]api.Op, tt.ops),
				InPlace: make([]api.Op, tt.inPlace),
			}

			chunks := decision.Chunk()

			sizes := make([]int, 0, len(chunks))
			for i, chunk := range chunks {
				sizes = append(sizes, len(chunk))
				carried := len(chunk)
				if i == 0 {
					carried += tt.inPlace
				}
				assert.LessOrEqual(t, carried, api.MaxOpsPerApply)
			}
			assert.Equal(t, tt.want, sizes)
		})
	}
}

// TestComposedOpsCoversEveryKindTheRulesEmit is the drift gate behind
// client.ComposableOps: an agent is measured against this list, so a kind the
// rules emit but the list omits would be sent to a pod that never claimed it.
func TestComposedOpsCoversEveryKindTheRulesEmit(t *testing.T) {
	composed := deployplan.ComposedOps()
	require.Equal(t, slices.Compact(slices.Sorted(slices.Values(composed))), slices.Sorted(slices.Values(composed)),
		"the list must name every kind once")

	for _, decision := range everyRuleDecision(t) {
		for _, op := range append(slices.Clone(decision.Ops), decision.InPlace...) {
			assert.Contains(t, composed, op.Kind)
		}
	}
}

// everyRuleDecision runs one diff per rule that composes ops, so the drift gate
// sees every kind the engine can emit.
func everyRuleDecision(t *testing.T) []deployplan.Decision {
	t.Helper()
	disabled := srv("SRV_1", "10.0.0.1", 8080)
	disabled.Disabled = true
	reweighted := srv("SRV_1", "10.0.0.1", 8080)
	reweighted.Weight = ptr(7)
	crtList := renderplan.CRTList{Path: listPath, Entries: []renderplan.CRTListEntry{{Cert: certPath}}}

	lifecycle := on34(basePlan(
		withBackend(dynBackend("be-old", srv("SRV_1", "10.0.0.1", 8080))),
		withBackend(dynBackend("be-keep", disabled)),
		withMap(renderplan.Map{Path: routeMap, Entries: []renderplan.Entry{entry("a", "1"), entry("b", "2")}}),
		withFile(&renderplan.File{Path: certPath, Kind: renderplan.FileKindCert, Digest: "before"}),
		withFile(&renderplan.File{Path: caPath, Kind: renderplan.FileKindCA, Digest: "before"}),
		withCRTList(crtList),
	))
	lifecycle.Inventory = api.Inventory{Maps: []string{routeMap}, CRTLists: []string{listPath}, Certs: loadedCerts}
	next := basePlan(
		withBackend(dynBackend("be-new", srv("SRV_2", "10.0.0.2", 8080))),
		withBackend(dynBackend("be-keep", reweighted)),
		withMap(renderplan.Map{Path: routeMap, Entries: []renderplan.Entry{entry("a", "9"), entry("c", "3")}}),
		withFile(&renderplan.File{Path: certPath, Kind: renderplan.FileKindCert, Digest: "after"}),
		withFile(&renderplan.File{Path: caPath, Kind: renderplan.FileKindCA, Digest: "after"}),
		withCRTList(renderplan.CRTList{Path: listPath, Entries: []renderplan.CRTListEntry{
			{Cert: certPath}, {Cert: "certs/other.pem"},
		}}),
	)

	replaced := on34(basePlan(withMap(renderplan.Map{
		Path: routeMap, Ordered: true, Entries: []renderplan.Entry{entry("a", "1"), entry("b", "2")},
	})))
	replaced.Inventory.Maps = []string{routeMap}
	reordered := basePlan(withMap(renderplan.Map{
		Path: routeMap, Ordered: true, Entries: []renderplan.Entry{entry("b", "2"), entry("a", "1")},
	}))

	pending := on34(basePlan(withBackend(dynBackend("be-keep", disabled))))
	pending.Running, pending.WorkerOps, pending.ReloadPending = pending.Applied, pending.Applied, true

	return []deployplan.Decision{
		deployplan.Diff(next, lifecycle),
		deployplan.Diff(reordered, replaced),
		deployplan.Diff(basePlan(withBackend(dynBackend("be-keep", reweighted))), pending),
	}
}

func TestDiffRefusesOpsTheAgentDoesNotExecute(t *testing.T) {
	prev := basePlan()
	next := basePlan(withBackend(dynBackend("be-new", srv("SRV_1", "10.0.0.1", 8080))))
	base := &deployplan.Baseline{
		Applied: prev,
		Caps:    deployplan.CapsFor("3.4.3", []string{api.OpBackendAdd, api.OpServerAdd, api.OpServerEnable}),
	}

	got := deployplan.Diff(next, base)

	require.Equal(t, deployplan.VerdictReload, got.Verdict)
	reasonsContain(t, got.Reasons, "the agent does not execute backend_publish")
}

func TestDiffCapsTheReasonList(t *testing.T) {
	prev := basePlan(withBackend(dynBackend("be-a", srv("SRV_1", "10.0.0.1", 8080))))
	opts := make([]planOpt, 0, deployplan.MaxReasons+6)
	opts = append(opts, withBackend(dynBackend("be-a", srv("SRV_1", "10.0.0.1", 8080))))
	for i := range deployplan.MaxReasons + 5 {
		opts = append(opts, withCore(fmt.Sprintf("frontend#%d", i), fmt.Sprintf("frontend f%d\n", i)))
	}

	got := deployplan.Diff(basePlan(opts...), on34(prev))

	assert.Equal(t, deployplan.VerdictReload, got.Verdict)
	assert.Len(t, got.Reasons, deployplan.MaxReasons)
}

// TestDiffIsDeterministic guards the property the deployer's memoisation
// depends on: the same inputs always produce the same decision.
func TestDiffIsDeterministic(t *testing.T) {
	prev := basePlan(
		withBackend(dynBackend("be-a", srv("SRV_1", "10.0.0.1", 8080))),
		withBackend(dynBackend("be-b", srv("SRV_1", "10.0.0.2", 8080))),
		withMap(renderplan.Map{Path: routeMap, Entries: []renderplan.Entry{entry("a", "be-a"), entry("b", "be-b")}}),
	)
	next := basePlan(
		withBackend(dynBackend("be-a", srv("SRV_1", "10.0.0.9", 8080))),
		withBackend(dynBackend("be-c", srv("SRV_1", "10.0.0.3", 8080))),
		withMap(renderplan.Map{Path: routeMap, Entries: []renderplan.Entry{entry("a", "be-a"), entry("c", "be-c")}}),
	)

	first := deployplan.Diff(next, withMapsLoaded(on34(prev), routeMap))
	second := deployplan.Diff(next, withMapsLoaded(on34(prev), routeMap))

	require.Equal(t, deployplan.VerdictRuntime, first.Verdict, first.Reasons)
	assert.Equal(t, first, second)
}

func weightedServers(count int) []renderplan.Server {
	servers := make([]renderplan.Server, 0, count)
	for i := range count {
		server := srv(fmt.Sprintf("SRV_%d", i), fmt.Sprintf("10.1.%d.%d", i/250, i%250+1), 8080)
		servers = append(servers, server)
	}
	return servers
}

// BenchmarkDiff measures the fleet's common case: one changed server in a
// 3000-backend render with 25 maps.
func BenchmarkDiff(b *testing.B) {
	prev := benchmarkPlan("10.0.0.1")
	next := benchmarkPlan("10.0.0.2")
	base := on34(prev)
	base.Inventory.Maps = benchmarkMapNames()

	b.ReportAllocs()
	for b.Loop() {
		if got := deployplan.Diff(next, base); got.Verdict != deployplan.VerdictRuntime {
			b.Fatalf("verdict %s: %q", got.Verdict, got.Reasons)
		}
	}
}

// The fleet size the plan budgets a diff for: 3000 routes and their maps.
const (
	benchmarkBackends = 3000
	benchmarkMaps     = 25
)

func benchmarkPlan(changedAddress string) *renderplan.Plan {
	opts := make([]planOpt, 0, benchmarkBackends+benchmarkMaps)
	for i := range benchmarkBackends {
		address := fmt.Sprintf("10.%d.%d.%d", i/60000, i/250%250, i%250+1)
		if i == 0 {
			address = changedAddress
		}
		opts = append(opts, withBackend(dynBackend(fmt.Sprintf("be-%04d", i), srv("SRV_1", address, 8080))))
	}
	for i, name := range benchmarkMapNames() {
		entries := make([]renderplan.Entry, 0, 100)
		for j := range 100 {
			entries = append(entries, entry(fmt.Sprintf("host-%d-%d.example.com", i, j), fmt.Sprintf("be-%04d", j)))
		}
		opts = append(opts, withMap(renderplan.Map{Path: name, Entries: entries}))
	}
	return basePlan(opts...)
}

func benchmarkMapNames() []string {
	names := make([]string, 0, benchmarkMaps)
	for i := range benchmarkMaps {
		names = append(names, fmt.Sprintf("maps/route-%02d.map", i))
	}
	return names
}

// BenchmarkDiffAcrossSnapshots is the fleet's diff as the deployer runs it:
// both plans are the shared views of two snapshots of one lineage, so the
// unchanged backends are the same records, and both come indexed.
func BenchmarkDiffAcrossSnapshots(b *testing.B) {
	authority := renderplan.NewAuthority()
	previous, err := renderplan.NewSnapshot(authority, benchmarkPlan("10.0.0.1"), nil)
	require.NoError(b, err)
	current, err := renderplan.NewSnapshot(authority, benchmarkPlan("10.0.0.2"), previous)
	require.NoError(b, err)
	prev, err := previous.SharedPlan()
	require.NoError(b, err)
	next, err := current.SharedPlan()
	require.NoError(b, err)
	base := on34(prev)
	base.Inventory.Maps = benchmarkMapNames()
	base.AppliedIndex = deployplan.IndexPlan(prev)
	nextIndex := deployplan.IndexPlan(next)

	b.ReportAllocs()
	for b.Loop() {
		if got := deployplan.DiffIndexed(next, nextIndex, base); got.Verdict != deployplan.VerdictRuntime {
			b.Fatalf("verdict %s: %q", got.Verdict, got.Reasons)
		}
	}
}

// The indexes are a view, not an input: a diff answers the same with them
// prebuilt as with the plans alone.
func TestDiffIndexedAnswersAsDiffDoes(t *testing.T) {
	prev := benchmarkPlan("10.0.0.1")
	next := benchmarkPlan("10.0.0.2")
	base := on34(prev)
	base.Inventory.Maps = benchmarkMapNames()
	plain := deployplan.Diff(next, base)

	base.AppliedIndex = deployplan.IndexPlan(prev)
	indexed := deployplan.DiffIndexed(next, deployplan.IndexPlan(next), base)
	require.Equal(t, deployplan.VerdictRuntime, plain.Verdict, plain.Reasons)
	assert.Equal(t, plain, indexed)
}
