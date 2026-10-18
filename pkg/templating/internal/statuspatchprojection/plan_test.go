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

package statuspatchprojection_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	projection "github.com/haproxy-haptic/HAPTIC/pkg/templating/internal/statuspatchprojection"
)

func TestPlanReplacesOnlyTheSelectedExactGroup(t *testing.T) {
	planOwner := &struct{ generation int }{generation: 1}
	plan, err := projection.NewPlan(planOwner)
	require.NoError(t, err)

	firstOwner, first := newProjection(t, "first", projectionInputFor("route-a", "uid-a", "rv-a", "rendered"))
	secondOwner, second := newProjection(t, "second", projectionInputFor("route-b", "uid-b", "rv-b", "rendered"))
	planOwner2 := &struct{ generation int }{generation: 2}
	plan, err = plan.Replace(planOwner, planOwner2, "group-a", first, firstOwner)
	require.NoError(t, err)
	planOwner3 := &struct{ generation int }{generation: 3}
	plan, err = plan.Replace(planOwner2, planOwner3, "group-b", second, secondOwner)
	require.NoError(t, err)

	exact, err := plan.ExactGroup(planOwner3, "group-a", first, firstOwner)
	require.NoError(t, err)
	assert.True(t, exact)
	exact, err = plan.ExactGroup(planOwner3, "group-a", second, secondOwner)
	require.NoError(t, err)
	assert.False(t, exact)

	replacementOwner, replacement := newProjection(
		t, "replacement", projectionInputFor("route-a", "uid-a", "rv-a", "rendered"),
	)
	planOwner4 := &struct{ generation int }{generation: 4}
	replaced, err := plan.Replace(planOwner3, planOwner4, "group-a", replacement, replacementOwner)
	require.NoError(t, err)

	groups, err := replaced.Groups(planOwner4)
	require.NoError(t, err)
	require.Len(t, groups, 2)
	assert.Equal(t, "group-a", groups[0].Name)
	assert.Same(t, replacement, groups[0].Root)
	assert.Equal(t, replacementOwner, groups[0].Owner)
	assert.Equal(t, "group-b", groups[1].Name)
	assert.Same(t, second, groups[1].Root)
	assert.Equal(t, secondOwner, groups[1].Owner)

	previousGroups, err := plan.Groups(planOwner3)
	require.NoError(t, err)
	assert.Same(t, first, previousGroups[0].Root)
}

func TestPlanReplacementRejectsCrossGroupPhaseConflictAtomically(t *testing.T) {
	owner := &struct{ generation int }{generation: 1}
	plan, err := projection.NewPlan(owner)
	require.NoError(t, err)
	firstOwner, first := newProjection(t, "first", projectionInputFor("route", "uid", "rv", "rendered"))
	nextOwner := &struct{ generation int }{generation: 2}
	plan, err = plan.Replace(owner, nextOwner, "first", first, firstOwner)
	require.NoError(t, err)

	conflictOwner, conflict := newProjection(
		t, "conflict", projectionInputFor("route", "uid", "rv", "rendered"),
	)
	failedOwner := &struct{ generation int }{generation: 3}
	_, err = plan.Replace(nextOwner, failedOwner, "second", conflict, conflictOwner)
	require.ErrorContains(t, err, "conflicting groups \"first\" and \"second\"")

	exact, err := plan.ExactGroup(nextOwner, "first", first, firstOwner)
	require.NoError(t, err)
	assert.True(t, exact)
	groups, err := plan.Groups(nextOwner)
	require.NoError(t, err)
	assert.Len(t, groups, 1)
}

func TestPlanAllowsDifferentPhasesWithOneLineage(t *testing.T) {
	owner := &struct{ generation int }{generation: 1}
	plan, err := projection.NewPlan(owner)
	require.NoError(t, err)
	firstOwner, first := newProjection(t, "first", projectionInputFor("route", "uid", "rv", "governance"))
	nextOwner := &struct{ generation int }{generation: 2}
	plan, err = plan.Replace(owner, nextOwner, "first", first, firstOwner)
	require.NoError(t, err)
	secondOwner, second := newProjection(t, "second", projectionInputFor("route", "uid", "rv", "rendered"))
	finalOwner := &struct{ generation int }{generation: 3}
	plan, err = plan.Replace(nextOwner, finalOwner, "second", second, secondOwner)
	require.NoError(t, err)

	targets, err := plan.TargetCount(finalOwner)
	require.NoError(t, err)
	assert.Equal(t, 1, targets)
	require.NoError(t, plan.ValidateLineage(finalOwner, "default", "route", "example.test/v1", "Route", "uid", "rv"))
}

func TestPlanRejectsLineageConflictAcrossPhasesAndRecoversAfterRemoval(t *testing.T) {
	owner := &struct{ generation int }{generation: 1}
	plan, err := projection.NewPlan(owner)
	require.NoError(t, err)
	firstOwner, first := newProjection(t, "first", projectionInputFor("route", "uid-a", "rv-a", "governance"))
	nextOwner := &struct{ generation int }{generation: 2}
	plan, err = plan.Replace(owner, nextOwner, "first", first, firstOwner)
	require.NoError(t, err)
	secondOwner, second := newProjection(t, "second", projectionInputFor("route", "uid-b", "rv-b", "rendered"))

	_, err = plan.Replace(nextOwner, &struct{ generation int }{generation: 3}, "second", second, secondOwner)
	require.ErrorContains(t, err, "conflicting source lineage")

	removedOwner := &struct{ generation int }{generation: 4}
	plan, err = plan.Replace(nextOwner, removedOwner, "first", nil, nil)
	require.NoError(t, err)
	finalOwner := &struct{ generation int }{generation: 5}
	plan, err = plan.Replace(removedOwner, finalOwner, "second", second, secondOwner)
	require.NoError(t, err)
	require.NoError(t, plan.ValidateLineage(
		finalOwner, "default", "route", "example.test/v1", "Route", "uid-b", "rv-b",
	))
}

func TestPlanTupleCannotAliasFields(t *testing.T) {
	owner := &struct{ generation int }{generation: 1}
	plan, err := projection.NewPlan(owner)
	require.NoError(t, err)
	firstOwner, first := newProjection(t, "first", &projection.InputPatch{
		Namespace: "a", Name: "bc", APIVersion: "d", Kind: "e", UID: "uid-a", ResourceVersion: "rv-a",
		Variants: map[string]map[string]any{"f": {"owner": "first"}},
	})
	nextOwner := &struct{ generation int }{generation: 2}
	plan, err = plan.Replace(owner, nextOwner, "first", first, firstOwner)
	require.NoError(t, err)
	secondOwner, second := newProjection(t, "second", &projection.InputPatch{
		Namespace: "ab", Name: "c", APIVersion: "d", Kind: "e", UID: "uid-b", ResourceVersion: "rv-b",
		Variants: map[string]map[string]any{"f": {"owner": "second"}},
	})
	finalOwner := &struct{ generation int }{generation: 3}
	plan, err = plan.Replace(nextOwner, finalOwner, "second", second, secondOwner)
	require.NoError(t, err)

	targets, err := plan.TargetCount(finalOwner)
	require.NoError(t, err)
	assert.Equal(t, 2, targets)
}

func TestPlanRejectsCopiedAndForeignOwnership(t *testing.T) {
	owner := &struct{ generation int }{generation: 1}
	plan, err := projection.NewPlan(owner)
	require.NoError(t, err)
	require.NoError(t, plan.Validate(owner))
	require.ErrorContains(t, plan.Validate(&struct{ generation int }{generation: 1}), "invalid provenance")

	copied := *plan
	require.ErrorContains(t, copied.Validate(owner), "invalid provenance")
}

func TestRecurringPlanContentsGetFreshExactRoots(t *testing.T) {
	projectionOwner, root := newProjection(t, "projection", projectionInputFor("route", "uid", "rv", "rendered"))
	ownerA1 := &struct{ generation int }{generation: 1}
	planA1, err := projection.NewPlan(ownerA1)
	require.NoError(t, err)
	ownerA2 := &struct{ generation int }{generation: 2}
	planA1, err = planA1.Replace(ownerA1, ownerA2, "group", root, projectionOwner)
	require.NoError(t, err)

	ownerB := &struct{ generation int }{generation: 3}
	planB, err := planA1.Replace(ownerA2, ownerB, "group", nil, nil)
	require.NoError(t, err)
	ownerA3 := &struct{ generation int }{generation: 4}
	planA2, err := planB.Replace(ownerB, ownerA3, "group", root, projectionOwner)
	require.NoError(t, err)

	assert.NotSame(t, planA1, planB)
	assert.NotSame(t, planA1, planA2)
	assert.NotSame(t, planB, planA2)
}

func TestPlanSupportsConcurrentPersistentReadersAndReplacements(t *testing.T) {
	owner := &struct{ generation int }{generation: 1}
	plan, err := projection.NewPlan(owner)
	require.NoError(t, err)
	projectionOwner, root := newProjection(t, "initial", projectionInputFor("route", "uid", "rv", "rendered"))
	populatedOwner := &struct{ generation int }{generation: 2}
	plan, err = plan.Replace(owner, populatedOwner, "group", root, projectionOwner)
	require.NoError(t, err)

	const workers = 32
	errorsByWorker := make(chan error, workers)
	var wait sync.WaitGroup
	for index := range workers {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			if worker%2 == 0 {
				exact, exactErr := plan.ExactGroup(populatedOwner, "group", root, projectionOwner)
				if exactErr == nil && !exact {
					exactErr = fmt.Errorf("exact group missing")
				}
				errorsByWorker <- exactErr
				return
			}
			nextOwner := &struct{ worker int }{worker: worker}
			replaced, replaceErr := plan.Replace(populatedOwner, nextOwner, "group", root, projectionOwner)
			if replaceErr == nil {
				replaceErr = replaced.Validate(nextOwner)
			}
			errorsByWorker <- replaceErr
		}(index)
	}
	wait.Wait()
	close(errorsByWorker)
	for workerErr := range errorsByWorker {
		require.NoError(t, workerErr)
	}

	exact, err := plan.ExactGroup(populatedOwner, "group", root, projectionOwner)
	require.NoError(t, err)
	assert.True(t, exact)
}

func TestNewPlanFromEntriesMatchesSequentialPermutations(t *testing.T) {
	firstOwner, first := newProjection(
		t, "first", projectionInputFor("route", "uid", "rv", "rendered"),
	)
	lastOwner, last := newProjection(
		t, "last", projectionInputFor("route", "uid", "rv", "rendered"),
	)
	policyOwner, policy := newProjection(
		t, "policy", projectionInputFor("policy", "policy-uid", "policy-rv", "deployed"),
	)
	entries := []projection.PlanEntry{
		{Group: "routes", Entry: "001", Root: first, Owner: firstOwner},
		{Group: "routes", Entry: "002", Root: last, Owner: lastOwner},
		{Group: "policies", Entry: "001", Root: policy, Owner: policyOwner},
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
			ordered := make([]projection.PlanEntry, len(entries))
			for index, source := range permutation {
				ordered[index] = entries[source]
			}
			bulkOwner := &struct{ name string }{name: "bulk"}
			bulk, err := projection.NewPlanFromEntries(bulkOwner, ordered)
			require.NoError(t, err)
			require.NoError(t, bulk.Validate(bulkOwner))

			sequentialOwner := any(&struct{ generation int }{})
			sequential, err := projection.NewPlan(sequentialOwner)
			require.NoError(t, err)
			for index := range ordered {
				nextOwner := &struct{ generation int }{generation: index + 1}
				entry := &ordered[index]
				sequential, err = sequential.ReplaceEntry(
					sequentialOwner, nextOwner, entry.Group, entry.Entry, entry.Root, entry.Owner,
				)
				require.NoError(t, err)
				sequentialOwner = nextOwner
			}
			assertPlanGroupsEquivalent(t, sequential, sequentialOwner, bulk, bulkOwner)
			for index := range entries {
				entry := &entries[index]
				exact, exactErr := bulk.ExactEntry(
					bulkOwner, entry.Group, entry.Entry, entry.Root, entry.Owner,
				)
				require.NoError(t, exactErr)
				assert.True(t, exact)
			}
		})
	}
}

func TestNewPlanFromEntriesPreservesExactWarmReplacement(t *testing.T) {
	firstOwner, first := newProjection(
		t, "first", projectionInputFor("route-a", "uid-a", "rv-a", "rendered"),
	)
	secondOwner, second := newProjection(
		t, "second", projectionInputFor("route-b", "uid-b", "rv-b", "rendered"),
	)
	owner := &struct{ generation int }{generation: 1}
	plan, err := projection.NewPlanFromEntries(owner, []projection.PlanEntry{
		{Group: "routes", Entry: "001", Root: first, Owner: firstOwner},
		{Group: "routes", Entry: "002", Root: second, Owner: secondOwner},
	})
	require.NoError(t, err)
	replacementOwner, replacement := newProjection(
		t, "replacement", projectionInputFor("route-b", "uid-b", "rv-b", "rendered"),
	)
	nextOwner := &struct{ generation int }{generation: 2}
	updated, err := plan.ReplaceEntry(owner, nextOwner, "routes", "002", replacement, replacementOwner)
	require.NoError(t, err)

	exact, err := updated.ExactEntry(nextOwner, "routes", "001", first, firstOwner)
	require.NoError(t, err)
	assert.True(t, exact)
	exact, err = updated.ExactEntry(nextOwner, "routes", "002", replacement, replacementOwner)
	require.NoError(t, err)
	assert.True(t, exact)
	exact, err = plan.ExactEntry(owner, "routes", "002", second, secondOwner)
	require.NoError(t, err)
	assert.True(t, exact)
}

func TestNewPlanFromEntriesRejectsLateConflictsWithoutPoisoningInputs(t *testing.T) {
	firstOwner, first := newProjection(
		t, "first", projectionInputFor("route", "uid-a", "rv-a", "rendered"),
	)
	phaseOwner, phaseConflict := newProjection(
		t, "phase", projectionInputFor("route", "uid-a", "rv-a", "rendered"),
	)
	lineageOwner, lineageConflict := newProjection(
		t, "lineage", projectionInputFor("route", "uid-b", "rv-b", "deployed"),
	)
	tests := []struct {
		name     string
		last     projection.PlanEntry
		want     string
		lastRoot *projection.Root
		owner    any
	}{
		{
			name: "phase", want: "conflicting groups",
			last: projection.PlanEntry{
				Group: "second", Entry: "002", Root: phaseConflict, Owner: phaseOwner,
			},
			lastRoot: phaseConflict, owner: phaseOwner,
		},
		{
			name: "lineage", want: "conflicting source lineage",
			last: projection.PlanEntry{
				Group: "first", Entry: "002", Root: lineageConflict, Owner: lineageOwner,
			},
			lastRoot: lineageConflict, owner: lineageOwner,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			owner := &struct{ name string }{name: test.name}
			plan, err := projection.NewPlanFromEntries(owner, []projection.PlanEntry{
				{Group: "first", Entry: "001", Root: first, Owner: firstOwner},
				test.last,
			})
			require.ErrorContains(t, err, test.want)
			assert.Nil(t, plan)
			require.NoError(t, first.Validate(firstOwner))
			require.NoError(t, test.lastRoot.Validate(test.owner))
			valid, validErr := projection.NewPlanFromEntries(owner, []projection.PlanEntry{
				{Group: "first", Entry: "001", Root: first, Owner: firstOwner},
			})
			require.NoError(t, validErr)
			require.NoError(t, valid.Validate(owner))
		})
	}
}

func TestNewPlanFromEntriesRejectsInvalidLocationsAndOwnership(t *testing.T) {
	projectionOwner, root := newProjection(
		t, "projection", projectionInputFor("route", "uid", "rv", "rendered"),
	)
	owner := &struct{ name string }{name: "plan"}
	tests := []struct {
		name    string
		owner   any
		entries []projection.PlanEntry
		want    string
	}{
		{name: "nil owner", entries: []projection.PlanEntry{{Group: "group", Root: root, Owner: projectionOwner}}, want: "owner is nil"},
		{name: "empty group", owner: owner, entries: []projection.PlanEntry{{Root: root, Owner: projectionOwner}}, want: "empty group"},
		{name: "nil root", owner: owner, entries: []projection.PlanEntry{{Group: "group"}}, want: "no projection root"},
		{
			name: "foreign owner", owner: owner,
			entries: []projection.PlanEntry{{
				Group: "group", Root: root, Owner: &struct{ name string }{name: "foreign"},
			}},
			want: "invalid provenance",
		},
		{
			name: "duplicate", owner: owner,
			entries: []projection.PlanEntry{
				{Group: "group", Entry: "entry", Root: root, Owner: projectionOwner},
				{Group: "group", Entry: "entry", Root: root, Owner: projectionOwner},
			},
			want: "repeats an entry in group",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, err := projection.NewPlanFromEntries(test.owner, test.entries)
			require.ErrorContains(t, err, test.want)
			assert.Nil(t, plan)
			require.NoError(t, root.Validate(projectionOwner))
		})
	}
}

func assertPlanGroupsEquivalent(
	t *testing.T,
	want *projection.PlanRoot,
	wantOwner any,
	got *projection.PlanRoot,
	gotOwner any,
) {
	t.Helper()
	wantGroups, err := want.Groups(wantOwner)
	require.NoError(t, err)
	gotGroups, err := got.Groups(gotOwner)
	require.NoError(t, err)
	require.Len(t, gotGroups, len(wantGroups))
	for index := range wantGroups {
		assert.Equal(t, wantGroups[index].Name, gotGroups[index].Name)
		assert.Same(t, wantGroups[index].Root, gotGroups[index].Root)
		assert.Equal(t, wantGroups[index].Owner, gotGroups[index].Owner)
	}
	wantTargets, err := want.TargetCount(wantOwner)
	require.NoError(t, err)
	gotTargets, err := got.TargetCount(gotOwner)
	require.NoError(t, err)
	assert.Equal(t, wantTargets, gotTargets)
}

func BenchmarkPlanReplaceOneOf3000Groups(b *testing.B) {
	planOwner := &struct{ generation int }{generation: 0}
	plan, err := projection.NewPlan(planOwner)
	require.NoError(b, err)
	projectionOwners := make([]any, 3000)
	projectionRoots := make([]*projection.Root, 3000)
	for index := range projectionRoots {
		projectionOwners[index], projectionRoots[index] = newProjection(
			b,
			fmt.Sprintf("projection-%d", index),
			projectionInputFor(fmt.Sprintf("route-%d", index), fmt.Sprintf("uid-%d", index), "rv", "rendered"),
		)
		nextOwner := &struct{ generation int }{generation: index + 1}
		plan, err = plan.Replace(
			planOwner, nextOwner, fmt.Sprintf("group-%04d", index), projectionRoots[index], projectionOwners[index],
		)
		require.NoError(b, err)
		planOwner = nextOwner
	}
	replacementOwner, replacement := newProjection(
		b, "replacement", projectionInputFor("route-1500", "uid-1500", "rv", "rendered"),
	)

	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		nextOwner := &struct{ iteration int }{iteration: index}
		_, err := plan.Replace(planOwner, nextOwner, "group-1500", replacement, replacementOwner)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func newProjection(
	tb testing.TB,
	name string,
	input *projection.InputPatch,
) (any, *projection.Root) {
	tb.Helper()
	owner := &struct{ name string }{name: name}
	root, err := projection.New(owner, []projection.InputPatch{*input})
	require.NoError(tb, err)
	return owner, root
}

func projectionInputFor(name, uid, resourceVersion, phase string) *projection.InputPatch {
	return &projection.InputPatch{
		Namespace: "default", Name: name, APIVersion: "example.test/v1", Kind: "Route",
		UID: uid, ResourceVersion: resourceVersion,
		Variants: map[string]map[string]any{phase: {"owner": name}},
	}
}
