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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/deployplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
)

// TestInPlaceOnlyWhileAReloadIsPending: without a pending reload the ops carry
// everything, so nothing is duplicated into the in-place set.
func TestInPlaceOnlyWhileAReloadIsPending(t *testing.T) {
	prev := basePlan(withBackend(dynBackend("be-a", srv("SRV_1", "10.0.0.1", 8080))))
	next := basePlan(withBackend(dynBackend("be-a", srv("SRV_1", "10.0.0.2", 8080))))

	got := deployplan.Diff(next, on34(prev))

	assert.Equal(t, deployplan.VerdictRuntime, got.Verdict)
	assert.Empty(t, got.InPlace)
}

// TestInPlaceRidesAReloadTheirPodMayPace: a render that reloads carries the
// in-place subset too, because the pod paces the reload when its window is
// closed and the worker keeps serving until it fires — an endpoint change in
// the same render must not wait for that.
func TestInPlaceRidesAReloadThePodMayPace(t *testing.T) {
	prev := basePlan(withBackend(dynBackend("be-a", srv("SRV_1", "10.0.0.1", 8080))))
	next := basePlan(withBackend(dynBackend("be-a", srv("SRV_1", "10.0.0.2", 8080))))
	next.Sections = append(next.Sections, renderplan.Section{Kind: renderplan.SectionKindCore, Name: "core#9", TextDigest: "new"})
	base := on34(prev)
	base.WorkerOps = prev

	got := deployplan.Diff(next, base)

	require.Equal(t, deployplan.VerdictReload, got.Verdict)
	assert.Empty(t, got.Ops, "a reload carries no runtime ops")
	require.Equal(t, []string{api.OpServerSetAddr}, kinds(got.InPlace))
	assert.Equal(t, "10.0.0.2", got.InPlace[0].Address)
	require.NotNil(t, got.WorkerPlan)
}

func TestInPlaceIsComputedAgainstTheWorkerPlan(t *testing.T) {
	applied := basePlan(withBackend(dynBackend("be-a", srv("SRV_1", "10.0.0.1", 8080))))
	worker := basePlan(withBackend(dynBackend("be-a", weighted(10))))
	next := basePlan(withBackend(dynBackend("be-a", weighted(30))))

	base := on34(applied)
	base.WorkerOps = worker
	base.ReloadPending = true

	got := deployplan.Diff(next, base)

	require.Len(t, got.InPlace, 1)
	assert.Equal(t, api.OpServerSetWeight, got.InPlace[0].Kind)
	assert.Equal(t, 30, *got.InPlace[0].Weight)
}

func TestInPlaceSetsMaxconnOnTheWorker(t *testing.T) {
	applied := basePlan(withBackend(dynBackend("be-a", srvWith("SRV_1", "10.0.0.1", maxconn("1500")))))
	next := basePlan(withBackend(dynBackend("be-a", srvWith("SRV_1", "10.0.0.1", maxconn("750")))))
	base := on34(applied)
	base.WorkerOps = applied
	base.ReloadPending = true

	got := deployplan.Diff(next, base)

	require.Equal(t, []string{api.OpServerSetMaxconn}, kinds(got.InPlace))
	assert.Equal(t, 750, *got.InPlace[0].Maxconn)
	require.NotNil(t, got.WorkerPlan)
	assert.Equal(t, []renderplan.KeywordArg{maxconn("750")}, got.WorkerPlan.Backends["be-a"].Servers[0].Extra)
	assert.Equal(t, []renderplan.KeywordArg{maxconn("1500")}, applied.Backends["be-a"].Servers[0].Extra, "the worker plan is a copy")
}

// TestInPlaceExcludesWhatOutlivesTheReload: deletes and backend lifecycle ops
// are not safe to run against a worker that is about to be replaced.
func TestInPlaceExcludesDeletesAndBackendOps(t *testing.T) {
	worker := basePlan(
		withBackend(dynBackend("be-a", srv("SRV_1", "10.0.0.1", 8080), srv("SRV_2", "10.0.0.2", 8080))),
		withBackend(dynBackend("be-old", srv("SRV_1", "10.0.0.3", 8080))),
	)
	next := basePlan(
		withBackend(dynBackend("be-a", srv("SRV_1", "10.0.0.1", 8080))),
		withBackend(dynBackend("be-new", srv("SRV_1", "10.0.0.4", 8080))),
	)
	base := on34(worker)
	base.WorkerOps = worker
	base.ReloadPending = true

	got := deployplan.Diff(next, base)

	assert.Equal(t, []string{api.OpServerDisable}, kinds(got.InPlace))
	assert.Equal(t, "SRV_2", got.InPlace[0].Server)
}

func TestInPlaceMapAndCertOps(t *testing.T) {
	worker := basePlan(
		withMap(renderplan.Map{Path: routeMap, Entries: []renderplan.Entry{
			entry("a.example.com", "be-a"), entry("gone.example.com", "be-gone"), entry("multi", "one"),
		}}),
		withFile(&renderplan.File{Path: certPath, Kind: renderplan.FileKindCert, Digest: "before"}),
	)
	next := basePlan(
		withMap(renderplan.Map{Path: routeMap, Entries: []renderplan.Entry{
			entry("a.example.com", "be-z"), entry("multi", "one"), entry("multi", "two"),
		}}),
		withFile(&renderplan.File{Path: certPath, Kind: renderplan.FileKindCert, Digest: "after"}),
	)
	base := withMapsLoaded(on34(worker), routeMap)
	base.WorkerOps = worker
	base.Inventory.Certs = []string{certPath}
	base.ReloadPending = true

	got := deployplan.Diff(next, base)

	require.Equal(t, []string{api.OpCertSet}, kinds(got.InPlace))
	require.NotNil(t, got.WorkerPlan)
	after := got.WorkerPlan.Maps[routeMap]
	assert.Equal(t, worker.Maps[routeMap].Entries, after.Entries)
	assert.Equal(t, "after", fileDigest(got.WorkerPlan, certPath))
	assert.NotEqual(t, worker.ID, got.WorkerPlan.ID)
	assert.NotEqual(t, next.ID, got.WorkerPlan.ID)

	base.WorkerOps = got.WorkerPlan
	again := deployplan.Diff(next, base)
	assert.Empty(t, again.InPlace)
	assert.Nil(t, again.WorkerPlan)

	// Without the deferred multiset replacement, the complete map delta can run.
	later := basePlan(
		withMap(renderplan.Map{Path: routeMap, Entries: []renderplan.Entry{entry("a.example.com", "be-z")}}),
		withFile(&renderplan.File{Path: certPath, Kind: renderplan.FileKindCert, Digest: "after"}),
	)
	third := deployplan.Diff(later, base)
	require.Equal(t, []string{api.OpMapSet, api.OpMapDel, api.OpMapDel}, kinds(third.InPlace))
	assert.Equal(t, "a.example.com", third.InPlace[0].Key)
	assert.Equal(t, "gone.example.com", third.InPlace[1].Key)
	assert.Equal(t, "multi", third.InPlace[2].Key)
	assert.Equal(t, later.Maps[routeMap].Entries, third.WorkerPlan.Maps[routeMap].Entries)
}

func TestInPlaceWorkerPlanIsStableAndCarriesTheServerChanges(t *testing.T) {
	worker := basePlan(withBackend(dynBackend("be-a", weighted(10))))
	next := basePlan(withBackend(dynBackend("be-a", weighted(30), srv("SRV_2", "10.0.0.2", 8080))))
	base := on34(worker)
	base.WorkerOps = worker
	base.ReloadPending = true

	got := deployplan.Diff(next, base)
	require.Equal(t, []string{api.OpServerSetWeight, api.OpServerAdd, api.OpServerEnable}, kinds(got.InPlace))
	servers := got.WorkerPlan.Backends["be-a"].Servers
	require.Len(t, servers, 2)
	assert.Equal(t, 30, *servers[0].Weight)
	assert.Equal(t, "SRV_2", servers[1].Name)
	assert.Equal(t, worker.ID, base.WorkerOps.ID, "the worker plan is derived from a copy")
	assert.Len(t, worker.Backends["be-a"].Servers, 1, "the worker plan is derived from a copy")

	assert.Equal(t, got.WorkerPlan.ID, deployplan.Diff(next, base).WorkerPlan.ID, "the id is a function of the inputs")

	base.WorkerOps = got.WorkerPlan
	assert.Empty(t, deployplan.Diff(next, base).InPlace)
}

func TestInPlaceHonoursTheAgentOpSet(t *testing.T) {
	worker := basePlan(withBackend(dynBackend("be-a", weighted(10))))
	next := basePlan(withBackend(dynBackend("be-a", weighted(30))))
	base := &deployplan.Baseline{
		Applied:       worker,
		WorkerOps:     worker,
		Caps:          deployplan.CapsFor("3.4.3", []string{api.OpServerSetAddr}),
		ReloadPending: true,
	}

	got := deployplan.Diff(next, base)

	assert.Empty(t, got.InPlace)
	assert.Nil(t, got.WorkerPlan)
}

// TestInPlaceNeedsTheWorkerOpsBaseline: the agent fences in-place ops on its
// worker-ops id, and once one batch ran neither Applied nor Running is what
// the worker holds — so without that plan nothing is composed.
func TestInPlaceNeedsTheWorkerOpsBaseline(t *testing.T) {
	applied := basePlan(withBackend(dynBackend("be-a", weighted(10))))
	next := basePlan(withBackend(dynBackend("be-a", weighted(30))))
	base := on34(applied)
	base.Running = applied
	base.ReloadPending = true

	got := deployplan.Diff(next, base)

	assert.Empty(t, got.InPlace)
	assert.Nil(t, got.WorkerPlan)
}

// weighted is the server the in-place cases move, at the weight they want.
func weighted(weight int) renderplan.Server {
	server := srv("SRV_1", "10.0.0.1", 8080)
	server.Weight = ptr(weight)
	return server
}

func TestInPlaceMapSetWaitsForABackendTheReloadAdds(t *testing.T) {
	worker := basePlan(
		withBackend(structuralBackend("be-old", srv("s1", "10.0.0.1", 8080))),
		withMap(renderplan.Map{Path: routeMap, Entries: []renderplan.Entry{
			entry("route.example.com", "route-key"), entry("gone.example.com", "be-old"),
		}}),
		withMap(renderplan.Map{Path: "maps/path.map", Entries: []renderplan.Entry{
			entry("route-key/", "be-old"),
		}}),
	)
	next := basePlan(
		withBackend(structuralBackend("be-new", srv("s1", "10.0.0.1", 8081))),
		withMap(renderplan.Map{Path: routeMap, Entries: []renderplan.Entry{
			entry("route.example.com", "route-key"),
		}}),
		withMap(renderplan.Map{Path: "maps/path.map", Entries: []renderplan.Entry{
			entry("route-key/", "be-new"),
		}}),
	)
	base := withMapsLoaded(on34(worker), routeMap, "maps/path.map")
	base.WorkerOps = worker
	base.ReloadPending = true

	got := deployplan.Diff(next, base)

	assert.Empty(t, got.InPlace)
	assert.Nil(t, got.WorkerPlan)

	// Once the reload created the backend, the same value change runs in
	// place against a worker that has it.
	loadedBackend := next.Backends["be-new"]
	worker = basePlan(withBackend(&loadedBackend), withMap(worker.Maps[routeMap]), withMap(worker.Maps["maps/path.map"]))
	base.WorkerOps = worker
	again := deployplan.Diff(next, base)
	assert.Equal(t, []string{api.OpMapSet, api.OpMapDel}, kinds(again.InPlace))
}
