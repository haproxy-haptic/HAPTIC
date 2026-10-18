// Copyright 2026 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package templating

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStatusPatchProjectionPlanReturnsSameRootOnlyForExactGroup(t *testing.T) {
	projection := mustPlanProjection(t, "route", "uid", "rv", "rendered", "a")
	plan := NewStatusPatchProjectionPlan()
	first, err := plan.ReplaceGroup("routes", projection)
	require.NoError(t, err)
	second, err := first.ReplaceGroup("routes", projection)
	require.NoError(t, err)
	assert.Same(t, first, second)

	recurring := mustPlanProjection(t, "route", "uid", "rv", "rendered", "a")
	third, err := second.ReplaceGroup("routes", recurring)
	require.NoError(t, err)
	assert.NotSame(t, second, third)

	removed, err := third.ReplaceGroup("routes", nil)
	require.NoError(t, err)
	assert.NotSame(t, third, removed)
	removedAgain, err := removed.ReplaceGroup("routes", nil)
	require.NoError(t, err)
	assert.Same(t, removed, removedAgain)
}

func TestStatusPatchProjectionPlanRejectsConflictsAtomically(t *testing.T) {
	firstProjection := mustPlanProjection(t, "route", "uid", "rv", "rendered", "first")
	plan, err := NewStatusPatchProjectionPlan().ReplaceGroup("first", firstProjection)
	require.NoError(t, err)
	conflict := mustPlanProjection(t, "route", "uid", "rv", "rendered", "second")

	_, err = plan.ReplaceGroup("second", conflict)
	require.ErrorContains(t, err, "conflicting groups \"first\" and \"second\"")

	replay, err := plan.PrepareReplay()
	require.NoError(t, err)
	collector := NewStatusPatchCollector()
	require.NoError(t, collector.ReplayProjectionPlan(replay))
	patches, err := collector.Patches()
	require.NoError(t, err)
	require.Len(t, patches, 1)
	assert.Equal(t, "first", patches[0].Variants["rendered"]["owner"])
}

func TestStatusPatchProjectionPlanOrdersEntriesWithinOneConflictGroup(t *testing.T) {
	first := mustPlanProjection(t, "route", "uid", "rv", "rendered", "first")
	last := mustPlanProjection(t, "route", "uid", "rv", "rendered", "last")
	plan, err := NewStatusPatchProjectionPlan().ReplaceEntry("routes", "001", first)
	require.NoError(t, err)
	plan, err = plan.ReplaceEntry("routes", "002", last)
	require.NoError(t, err)

	patches, err := snapshotProjectionPlan(t, plan, nil).Patches()
	require.NoError(t, err)
	require.Len(t, patches, 1)
	assert.Equal(t, "last", patches[0].Variants["rendered"]["owner"])

	withoutLast, err := plan.ReplaceEntry("routes", "002", nil)
	require.NoError(t, err)
	patches, err = snapshotProjectionPlan(t, withoutLast, nil).Patches()
	require.NoError(t, err)
	require.Len(t, patches, 1)
	assert.Equal(t, "first", patches[0].Variants["rendered"]["owner"])
}

func TestStatusPatchProjectionPlanEntryStillRejectsAnotherConflictGroup(t *testing.T) {
	first := mustPlanProjection(t, "route", "uid", "rv", "rendered", "first")
	conflict := mustPlanProjection(t, "route", "uid", "rv", "rendered", "conflict")
	plan, err := NewStatusPatchProjectionPlan().ReplaceEntry("routes-a", "001", first)
	require.NoError(t, err)

	_, err = plan.ReplaceEntry("routes-b", "001", conflict)
	require.ErrorContains(t, err, `conflicting groups "routes-a" and "routes-b"`)

	patches, patchErr := snapshotProjectionPlan(t, plan, nil).Patches()
	require.NoError(t, patchErr)
	require.Len(t, patches, 1)
	assert.Equal(t, "first", patches[0].Variants["rendered"]["owner"])
}

func TestStatusPatchProjectionPlanPreservesDirectOverwriteAndSourceSemantics(t *testing.T) {
	projected, err := NewStatusPatchProjection([]StatusPatch{{
		Namespace: "default", Name: "route", APIVersion: "example.test/v1", Kind: "Route",
		UID: "uid", ResourceVersion: "rv",
		Variants: map[string]map[string]any{
			"rendered": {"owner": "projected"},
			"deployed": {"owner": "deployed"},
		},
		SourceTemplate: "projected", SourceLine: 20,
	}})
	require.NoError(t, err)
	plan, err := NewStatusPatchProjectionPlan().ReplaceGroup("routes", projected)
	require.NoError(t, err)
	replay, err := plan.PrepareReplay()
	require.NoError(t, err)

	collector := NewStatusPatchCollector()
	require.NoError(t, collector.RegisterWithLineage(
		"default", "route", "example.test/v1", "Route", "uid", "rv",
		map[string]map[string]any{"rendered": {"owner": "direct"}},
	))
	collector.SetSource("default", "route", "example.test/v1", "Route", "direct", 10)
	require.NoError(t, collector.ReplayProjectionPlan(replay))

	snapshot, err := collector.Snapshot()
	require.NoError(t, err)
	count, err := snapshot.Len()
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	patches, err := snapshot.Patches()
	require.NoError(t, err)
	require.Len(t, patches, 1)
	assert.Equal(t, "projected", patches[0].Variants["rendered"]["owner"])
	assert.Equal(t, "deployed", patches[0].Variants["deployed"]["owner"])
	assert.Equal(t, "direct", patches[0].SourceTemplate)
	assert.Equal(t, 10, patches[0].SourceLine)
}

func TestStatusPatchProjectionPlanRejectsDirectLineageConflictAtomically(t *testing.T) {
	projected := mustPlanProjection(t, "route", "uid-b", "rv-b", "rendered", "projected")
	plan, err := NewStatusPatchProjectionPlan().ReplaceGroup("routes", projected)
	require.NoError(t, err)
	replay, err := plan.PrepareReplay()
	require.NoError(t, err)
	collector := NewStatusPatchCollector()
	require.NoError(t, collector.RegisterWithLineage(
		"default", "route", "example.test/v1", "Route", "uid-a", "rv-a",
		map[string]map[string]any{"rendered": {"owner": "direct"}},
	))

	err = collector.ReplayProjectionPlan(replay)
	require.ErrorContains(t, err, "conflicting source lineage")
	require.NoError(t, collector.Register(
		"default", "another", "example.test/v1", "Route",
		map[string]map[string]any{"rendered": {"owner": "another"}},
	))
	patches, patchErr := collector.Patches()
	require.NoError(t, patchErr)
	assert.Len(t, patches, 2)
}

func TestStatusPatchProjectionPlanSnapshotReusesOnlyExactPlanRoot(t *testing.T) {
	projectionA1 := mustPlanProjection(t, "route", "uid", "rv-a", "rendered", "a")
	planA1, err := NewStatusPatchProjectionPlan().ReplaceGroup("routes", projectionA1)
	require.NoError(t, err)
	first := snapshotProjectionPlan(t, planA1, nil)
	second := snapshotProjectionPlan(t, planA1, first)
	assert.Same(t, first, second)

	projectionB := mustPlanProjection(t, "route", "uid", "rv-b", "rendered", "b")
	planB, err := planA1.ReplaceGroup("routes", projectionB)
	require.NoError(t, err)
	third := snapshotProjectionPlan(t, planB, second)
	assert.NotSame(t, second, third)

	projectionA2 := mustPlanProjection(t, "route", "uid", "rv-a", "rendered", "a")
	planA2, err := planB.ReplaceGroup("routes", projectionA2)
	require.NoError(t, err)
	fourth := snapshotProjectionPlan(t, planA2, third)
	assert.NotSame(t, first, fourth)
	equal, err := first.ExactEqual(fourth)
	require.NoError(t, err)
	assert.True(t, equal)
}

func TestStatusPatchProjectionPlanExactRootSafelyReusesMissingLineage(t *testing.T) {
	projected := mustPlanProjection(t, "route", "", "", "rendered", "stable")
	plan, err := NewStatusPatchProjectionPlan().ReplaceGroup("routes", projected)
	require.NoError(t, err)
	first := snapshotProjectionPlan(t, plan, nil)
	second := snapshotProjectionPlan(t, plan, first)
	assert.Same(t, first, second)
}

func TestStatusPatchProjectionPlanRejectsCopiedAndPoisonedRoots(t *testing.T) {
	projection := mustPlanProjection(t, "route", "uid", "rv", "rendered", "stable")
	plan, err := NewStatusPatchProjectionPlan().ReplaceGroup("routes", projection)
	require.NoError(t, err)
	copied := *plan
	require.ErrorContains(t, copied.ValidateAuthentication(), "invalid provenance")

	replay, err := plan.PrepareReplay()
	require.NoError(t, err)
	collector := NewStatusPatchCollector()
	require.NoError(t, collector.ReplayProjectionPlan(replay))
	plan.integrity.root = nil
	_, err = collector.Snapshot()
	require.ErrorContains(t, err, "invalid provenance")
}

func TestStatusPatchProjectionPlanCollectorRejectsForeignReplaySubstitution(t *testing.T) {
	stable := mustPlanProjection(t, "route", "uid", "rv", "rendered", "stable")
	stablePlan, err := NewStatusPatchProjectionPlan().ReplaceGroup("routes", stable)
	require.NoError(t, err)
	stableReplay, err := stablePlan.PrepareReplay()
	require.NoError(t, err)
	collector := NewStatusPatchCollector()
	require.NoError(t, collector.ReplayProjectionPlan(stableReplay))

	foreign := mustPlanProjection(t, "route", "uid", "rv", "rendered", "foreign")
	foreignPlan, err := NewStatusPatchProjectionPlan().ReplaceGroup("routes", foreign)
	require.NoError(t, err)
	foreignReplay, err := foreignPlan.PrepareReplay()
	require.NoError(t, err)
	collector.projectionPlan = foreignReplay

	_, err = collector.Snapshot()
	require.ErrorContains(t, err, "invalid provenance")
}

func TestStatusPatchProjectionPlanPhaseMaterializationIsDetached(t *testing.T) {
	projected := mustPlanProjection(t, "route", "uid", "rv", "rendered", "stable")
	plan, err := NewStatusPatchProjectionPlan().ReplaceGroup("routes", projected)
	require.NoError(t, err)
	snapshot := snapshotProjectionPlan(t, plan, nil)

	first, err := snapshot.PatchesForPhase("rendered")
	require.NoError(t, err)
	first[0].Variants["rendered"]["owner"] = "poisoned"
	second, err := snapshot.PatchesForPhase("rendered")
	require.NoError(t, err)
	assert.Equal(t, "stable", second[0].Variants["rendered"]["owner"])
}

func TestStatusPatchProjectionPlanBulkMatchesSequentialPermutations(t *testing.T) {
	first := mustPlanProjection(t, "route", "uid", "rv", "rendered", "first")
	last := mustPlanProjection(t, "route", "uid", "rv", "rendered", "last")
	policy := mustPlanProjection(t, "policy", "policy-uid", "policy-rv", "deployed", "policy")
	entries := []StatusPatchProjectionPlanEntry{
		{Group: "routes", Entry: "001", Projection: first},
		{Group: "routes", Entry: "002", Projection: last},
		{Group: "policies", Entry: "001", Projection: policy},
	}
	permutations := [][]int{
		{0, 1, 2},
		{0, 2, 1},
		{1, 0, 2},
		{1, 2, 0},
		{2, 0, 1},
		{2, 1, 0},
	}
	for permutationIndex, permutation := range permutations {
		t.Run(fmt.Sprintf("permutation-%d", permutationIndex), func(t *testing.T) {
			ordered := make([]StatusPatchProjectionPlanEntry, len(entries))
			for index, source := range permutation {
				ordered[index] = entries[source]
			}
			bulk, err := NewStatusPatchProjectionPlanFromEntries(ordered)
			require.NoError(t, err)
			require.NoError(t, bulk.ValidateAuthentication())

			sequential := NewStatusPatchProjectionPlan()
			for index := range ordered {
				entry := &ordered[index]
				sequential, err = sequential.ReplaceEntry(entry.Group, entry.Entry, entry.Projection)
				require.NoError(t, err)
			}
			bulkSnapshot := snapshotProjectionPlan(t, bulk, nil)
			sequentialSnapshot := snapshotProjectionPlan(t, sequential, nil)
			equal, equalErr := bulkSnapshot.ExactEqual(sequentialSnapshot)
			require.NoError(t, equalErr)
			assert.True(t, equal)
			for index := range entries {
				entry := &entries[index]
				exact, exactErr := bulk.root.ExactEntry(
					bulk, entry.Group, entry.Entry, entry.Projection.root, entry.Projection,
				)
				require.NoError(t, exactErr)
				assert.True(t, exact)
			}
		})
	}

	empty, err := NewStatusPatchProjectionPlanFromEntries(nil)
	require.NoError(t, err)
	require.NoError(t, empty.ValidateAuthentication())
	replay, err := empty.PrepareReplay()
	require.NoError(t, err)
	assert.True(t, replay.Empty())
}

func TestStatusPatchProjectionPlanBulkPreservesPerEntryWarmReplacement(t *testing.T) {
	first := mustPlanProjection(t, "route-a", "uid-a", "rv-a", "rendered", "first")
	second := mustPlanProjection(t, "route-b", "uid-b", "rv-b", "rendered", "second")
	policy := mustPlanProjection(t, "policy", "policy-uid", "policy-rv", "deployed", "policy")
	plan, err := NewStatusPatchProjectionPlanFromEntries([]StatusPatchProjectionPlanEntry{
		{Group: "routes", Entry: "001", Projection: first},
		{Group: "routes", Entry: "002", Projection: second},
		{Group: "policies", Entry: "001", Projection: policy},
	})
	require.NoError(t, err)
	replacement := mustPlanProjection(t, "route-b", "uid-b", "rv-b", "rendered", "replacement")
	updated, err := plan.ReplaceEntry("routes", "002", replacement)
	require.NoError(t, err)

	exact, err := updated.root.ExactEntry(updated, "routes", "001", first.root, first)
	require.NoError(t, err)
	assert.True(t, exact)
	exact, err = updated.root.ExactEntry(updated, "routes", "002", replacement.root, replacement)
	require.NoError(t, err)
	assert.True(t, exact)
	exact, err = updated.root.ExactEntry(updated, "policies", "001", policy.root, policy)
	require.NoError(t, err)
	assert.True(t, exact)
	exact, err = plan.root.ExactEntry(plan, "routes", "002", second.root, second)
	require.NoError(t, err)
	assert.True(t, exact)

	removed, err := updated.ReplaceEntry("routes", "002", nil)
	require.NoError(t, err)
	patches, err := snapshotProjectionPlan(t, removed, nil).Patches()
	require.NoError(t, err)
	require.Len(t, patches, 2)
	assert.Equal(t, "first", patches[1].Variants["rendered"]["owner"])
}

func TestStatusPatchProjectionPlanBulkRejectsLateConflictsAtomically(t *testing.T) {
	first := mustPlanProjection(t, "route", "uid-a", "rv-a", "rendered", "first")
	phaseConflict := mustPlanProjection(t, "route", "uid-a", "rv-a", "rendered", "phase")
	lineageConflict := mustPlanProjection(t, "route", "uid-b", "rv-b", "deployed", "lineage")
	tests := []struct {
		name string
		last StatusPatchProjectionPlanEntry
		want string
	}{
		{
			name: "phase",
			last: StatusPatchProjectionPlanEntry{
				Group: "second", Entry: "002", Projection: phaseConflict,
			},
			want: "conflicting groups",
		},
		{
			name: "lineage",
			last: StatusPatchProjectionPlanEntry{
				Group: "first", Entry: "002", Projection: lineageConflict,
			},
			want: "conflicting source lineage",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, err := NewStatusPatchProjectionPlanFromEntries([]StatusPatchProjectionPlanEntry{
				{Group: "first", Entry: "001", Projection: first},
				test.last,
			})
			require.ErrorContains(t, err, test.want)
			assert.Nil(t, plan)
			require.NoError(t, first.ValidateAuthentication())
			require.NoError(t, test.last.Projection.ValidateAuthentication())
			valid, validErr := NewStatusPatchProjectionPlanFromEntries([]StatusPatchProjectionPlanEntry{
				{Group: "first", Entry: "001", Projection: first},
			})
			require.NoError(t, validErr)
			require.NoError(t, valid.ValidateAuthentication())
		})
	}
}

func TestStatusPatchProjectionPlanBulkRejectsInvalidEntriesAndOwnership(t *testing.T) {
	projection := mustPlanProjection(t, "route", "uid", "rv", "rendered", "stable")
	copied := *projection
	tests := []struct {
		name    string
		entries []StatusPatchProjectionPlanEntry
		want    string
	}{
		{
			name:    "empty group",
			entries: []StatusPatchProjectionPlanEntry{{Entry: "001", Projection: projection}},
			want:    "empty group",
		},
		{
			name:    "nil projection",
			entries: []StatusPatchProjectionPlanEntry{{Group: "routes", Entry: "001"}},
			want:    "is nil",
		},
		{
			name:    "copied projection",
			entries: []StatusPatchProjectionPlanEntry{{Group: "routes", Entry: "001", Projection: &copied}},
			want:    "invalid provenance",
		},
		{
			name: "duplicate",
			entries: []StatusPatchProjectionPlanEntry{
				{Group: "routes", Entry: "001", Projection: projection},
				{Group: "routes", Entry: "001", Projection: projection},
			},
			want: "repeats an entry in group",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, err := NewStatusPatchProjectionPlanFromEntries(test.entries)
			require.ErrorContains(t, err, test.want)
			assert.Nil(t, plan)
			require.NoError(t, projection.ValidateAuthentication())
		})
	}
}

func TestStatusPatchProjectionPlanBulkRejectsRootSubstitution(t *testing.T) {
	projection := mustPlanProjection(t, "route", "uid", "rv", "rendered", "stable")
	plan, err := NewStatusPatchProjectionPlanFromEntries([]StatusPatchProjectionPlanEntry{
		{Group: "routes", Entry: "001", Projection: projection},
	})
	require.NoError(t, err)
	foreign, err := NewStatusPatchProjectionPlanFromEntries([]StatusPatchProjectionPlanEntry{
		{Group: "routes", Entry: "001", Projection: projection},
	})
	require.NoError(t, err)

	poisoned := *plan
	poisoned.seal = &poisoned
	poisoned.root = foreign.root
	require.ErrorContains(t, poisoned.ValidateAuthentication(), "invalid provenance")
	require.NoError(t, plan.ValidateAuthentication())
}

func TestStatusPatchProjectionPlanBulkRetainsExactProjectionOwnership(t *testing.T) {
	projection := mustPlanProjection(t, "route", "uid", "rv", "rendered", "stable")
	plan, err := NewStatusPatchProjectionPlanFromEntries([]StatusPatchProjectionPlanEntry{
		{Group: "routes", Entry: "001", Projection: projection},
	})
	require.NoError(t, err)
	replay, err := plan.PrepareReplay()
	require.NoError(t, err)
	projection.integrity.root = nil

	collector := NewStatusPatchCollector()
	require.NoError(t, collector.ReplayProjectionPlan(replay))
	snapshot, err := collector.Snapshot()
	require.NoError(t, err)
	_, err = snapshot.Patches()
	require.ErrorContains(t, err, "invalid provenance")
}

func BenchmarkStatusPatchProjectionPlanNoChange3000(b *testing.B) {
	plan := benchmarkProjectionPlan(b, 3000)
	previous := snapshotProjectionPlan(b, plan, nil)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		replay, err := plan.PrepareReplay()
		if err != nil {
			b.Fatal(err)
		}
		collector := NewStatusPatchCollector()
		if err := collector.ReplayProjectionPlan(replay); err != nil {
			b.Fatal(err)
		}
		if _, err := collector.Snapshot(previous); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStatusPatchProjectionPlanReplaceOneOf3000Entries(b *testing.B) {
	plan := NewStatusPatchProjectionPlan()
	for index := range 3000 {
		projected := mustPlanProjection(
			b,
			fmt.Sprintf("route-%d", index),
			fmt.Sprintf("uid-%d", index),
			"rv",
			"rendered",
			fmt.Sprintf("owner-%d", index),
		)
		var err error
		plan, err = plan.ReplaceEntry("routes", fmt.Sprintf("%04d", index), projected)
		require.NoError(b, err)
	}
	replacement := mustPlanProjection(b, "route-1500", "uid-1500", "rv", "rendered", "replacement")

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := plan.ReplaceEntry("routes", "1500", replacement); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkProjectionPlan(tb testing.TB, count int) *StatusPatchProjectionPlan {
	tb.Helper()
	plan := NewStatusPatchProjectionPlan()
	for index := range count {
		projected := mustPlanProjection(
			tb,
			fmt.Sprintf("route-%d", index),
			fmt.Sprintf("uid-%d", index),
			"rv",
			"rendered",
			fmt.Sprintf("owner-%d", index),
		)
		var err error
		plan, err = plan.ReplaceGroup(fmt.Sprintf("group-%04d", index), projected)
		require.NoError(tb, err)
	}
	return plan
}

func mustPlanProjection(
	tb testing.TB,
	name, uid, resourceVersion, phase, owner string,
) *StatusPatchProjection {
	tb.Helper()
	projected, err := NewStatusPatchProjection([]StatusPatch{{
		Namespace: "default", Name: name, APIVersion: "example.test/v1", Kind: "Route",
		UID: uid, ResourceVersion: resourceVersion,
		Variants: map[string]map[string]any{phase: {"owner": owner}},
	}})
	require.NoError(tb, err)
	return projected
}

func snapshotProjectionPlan(
	tb testing.TB,
	plan *StatusPatchProjectionPlan,
	previous *StatusPatchSnapshot,
) *StatusPatchSnapshot {
	tb.Helper()
	replay, err := plan.PrepareReplay()
	require.NoError(tb, err)
	collector := NewStatusPatchCollector()
	require.NoError(tb, collector.ReplayProjectionPlan(replay))
	snapshot, err := collector.Snapshot(previous)
	require.NoError(tb, err)
	return snapshot
}
