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

package renderer

import (
	"errors"
	"fmt"
	"slices"

	"golang.org/x/sync/errgroup"

	"github.com/haproxy-haptic/HAPTIC/pkg/incremental"
)

type incrementalColdCarrierGraphSchedule struct {
	seal         *incrementalColdCarrierGraphSchedule
	keys         []incremental.QueryKey
	groupOrder   []string
	keysByGroup  map[string][]incremental.QueryKey
	queryIndexes map[incremental.QueryKey]int
}

func newIncrementalColdCarrierGraphSchedule(
	plan *incrementalCarrierPlan,
	groupOrder []string,
	keys []incremental.QueryKey,
	keysByGroup map[string][]incremental.QueryKey,
) (*incrementalColdCarrierGraphSchedule, error) {
	if plan == nil || plan.logicalQueries != len(keys) || len(keys) == 0 || len(groupOrder) == 0 {
		return nil, errors.New("incremental cold carrier graph schedule is incomplete")
	}
	ordered := slices.Clone(keys)
	slices.SortFunc(ordered, compareIncrementalQueryKey)
	queryIndexes := make(map[incremental.QueryKey]int, len(ordered))
	for index, key := range ordered {
		if key.Opaque() == "" || index > 0 && ordered[index-1] == key {
			return nil, errors.New("incremental cold carrier graph query order is invalid")
		}
		queryIndexes[key] = index
	}
	ownedByGroup := make(map[string][]incremental.QueryKey, len(keysByGroup))
	seen := make(map[incremental.QueryKey]struct{}, len(ordered))
	for _, group := range groupOrder {
		if group == "" {
			return nil, errors.New("incremental cold carrier graph contains an empty group")
		}
		groupKeys := slices.Clone(keysByGroup[group])
		slices.SortFunc(groupKeys, compareIncrementalQueryKey)
		for _, key := range groupKeys {
			if _, expected := queryIndexes[key]; !expected {
				return nil, fmt.Errorf("incremental cold carrier graph group %q has an unknown query", group)
			}
			if _, duplicate := seen[key]; duplicate {
				return nil, fmt.Errorf("incremental cold carrier graph query %s belongs to multiple groups", key)
			}
			seen[key] = struct{}{}
		}
		ownedByGroup[group] = groupKeys
	}
	if len(seen) != len(ordered) {
		return nil, errors.New("incremental cold carrier graph omitted a query")
	}
	schedule := &incrementalColdCarrierGraphSchedule{
		keys:         ordered,
		groupOrder:   slices.Clone(groupOrder),
		keysByGroup:  ownedByGroup,
		queryIndexes: queryIndexes,
	}
	schedule.seal = schedule
	return schedule, nil
}

func compareIncrementalQueryKey(left, right incremental.QueryKey) int {
	if left.Opaque() < right.Opaque() {
		return -1
	}
	if left.Opaque() > right.Opaque() {
		return 1
	}
	return 0
}

func (s *incrementalColdCarrierGraphSchedule) executable() bool {
	return s != nil && s.seal == s && len(s.keys) > 0 && len(s.queryIndexes) == len(s.keys)
}

func validateIncrementalColdCarrierGraphStageOrder(
	schedule *incrementalColdCarrierGraphSchedule,
	stage *incrementalColdCarrierStageResult,
) error {
	if schedule == nil || schedule.seal != schedule || stage == nil || len(stage.indexes) == 0 ||
		len(stage.indexes) != len(stage.results) {
		return errors.New("incremental cold carrier graph stage order is incomplete")
	}
	previous := -1
	for resultIndex, batchIndex := range stage.indexes {
		result := stage.results[resultIndex]
		expectedIndex, exists := schedule.queryIndexes[result.Key]
		if !exists || batchIndex < 0 || batchIndex >= len(schedule.keys) || batchIndex <= previous ||
			expectedIndex != batchIndex || schedule.keys[batchIndex] != result.Key {
			return errors.New("incremental cold carrier graph stage results are not in global batch order")
		}
		previous = batchIndex
	}
	return nil
}

func selectIncrementalColdCarrierGraphStage(
	plan *incrementalCarrierPlan,
	groups []string,
) (*incrementalCarrierStage, bool, error) {
	if plan == nil || len(groups) == 0 {
		return nil, false, errors.New("incremental cold carrier graph stage selection is incomplete")
	}
	expected := make(map[string]struct{}, len(groups))
	for _, group := range groups {
		if group == "" {
			return nil, false, errors.New("incremental cold carrier graph stage has an empty group")
		}
		if _, duplicate := expected[group]; duplicate {
			return nil, false, fmt.Errorf("incremental cold carrier graph stage repeats group %q", group)
		}
		expected[group] = struct{}{}
	}
	selected := &incrementalCarrierStage{wave: -1, groups: slices.Clone(groups)}
	seenIndexes := make(map[int]struct{})
	seenQueries := make(map[incremental.QueryKey]struct{})
	for stageIndex := range plan.stages {
		planned := &plan.stages[stageIndex]
		for carrierIndex := range planned.carriers {
			carrier := &planned.carriers[carrierIndex]
			lanes, laneErr := selectIncrementalColdCarrierLanes(carrier, expected, seenIndexes, seenQueries)
			if laneErr != nil {
				return nil, false, laneErr
			}
			if len(lanes) == 0 {
				continue
			}
			selected.carriers = append(selected.carriers, incrementalCarrier{
				source: carrier.source, namespace: carrier.namespace, name: carrier.name, lanes: lanes,
			})
			selected.logicalQueries += len(lanes)
		}
	}
	if selected.logicalQueries == 0 {
		return nil, false, nil
	}
	return selected, true, nil
}

func selectIncrementalColdCarrierLanes(
	carrier *incrementalCarrier,
	expected map[string]struct{},
	seenIndexes map[int]struct{},
	seenQueries map[incremental.QueryKey]struct{},
) ([]incrementalCarrierLane, error) {
	lanes := make([]incrementalCarrierLane, 0, len(carrier.lanes))
	for laneIndex := range carrier.lanes {
		lane := carrier.lanes[laneIndex]
		if lane.component == nil {
			return nil, errors.New("incremental cold carrier graph plan has an empty component")
		}
		if _, include := expected[lane.component.group]; !include {
			continue
		}
		if _, duplicate := seenIndexes[lane.batchIndex]; duplicate {
			return nil, fmt.Errorf("incremental cold carrier graph repeats batch item %d", lane.batchIndex)
		}
		if _, duplicate := seenQueries[lane.queryKey]; duplicate {
			return nil, fmt.Errorf("incremental cold carrier graph repeats query %s", lane.queryKey)
		}
		seenIndexes[lane.batchIndex] = struct{}{}
		seenQueries[lane.queryKey] = struct{}{}
		lanes = append(lanes, lane)
	}
	return lanes, nil
}

func incrementalColdCarrierStageSize(stage *incrementalCarrierStage) int {
	if stage == nil {
		return 0
	}
	return stage.logicalQueries
}

func (r *incrementalRenderSession) applyColdGraphStageResults(
	stageNumber int,
	stageGroups []string,
	results []incremental.ExactResult,
	completed map[string]*incrementalGroupIndex,
	pending map[string]struct{},
) error {
	resultsByGroup, err := r.groupColdGraphStageResults(stageNumber, stageGroups, results)
	if err != nil {
		return err
	}
	prepared := make([]*incrementalPreparedColdGroupAdditions, len(stageGroups))
	applicable := make([]bool, len(stageGroups))
	prepareErrors := make([]error, len(stageGroups))
	var prepareGroup errgroup.Group
	for groupIndex, group := range stageGroups {
		groupResults := resultsByGroup[group]
		if len(groupResults) == 0 {
			continue
		}
		prepareGroup.Go(func() error {
			prepared[groupIndex], applicable[groupIndex], prepareErrors[groupIndex] =
				r.prepareColdGroupAdditions(group, groupResults)
			return nil
		})
	}
	_ = prepareGroup.Wait()
	for groupIndex, group := range stageGroups {
		if prepareErrors[groupIndex] != nil {
			return fmt.Errorf("assembling incremental cold group %q: %w", group, prepareErrors[groupIndex])
		}
		groupResults := resultsByGroup[group]
		if len(groupResults) > 0 {
			if err := r.installColdGraphGroupResults(
				group, prepared[groupIndex], applicable[groupIndex], groupResults,
			); err != nil {
				return fmt.Errorf("assembling incremental cold group %q: %w", group, err)
			}
		}
		if err := r.completeColdGraphGroup(group, completed, pending); err != nil {
			return err
		}
	}
	return nil
}

func (r *incrementalRenderSession) groupColdGraphStageResults(
	stageNumber int,
	stageGroups []string,
	results []incremental.ExactResult,
) (map[string][]incremental.ExactResult, error) {
	expectedGroups := make(map[string]struct{}, len(stageGroups))
	for _, group := range stageGroups {
		expectedGroups[group] = struct{}{}
	}
	resultsByGroup := make(map[string][]incremental.ExactResult, len(stageGroups))
	for index := range results {
		component, ok := r.resolveQueryComponent(results[index].Key)
		if !ok {
			return nil, fmt.Errorf(
				"incremental cold graph stage %d returned invalid query %s",
				stageNumber,
				results[index].Key,
			)
		}
		if _, expected := expectedGroups[component.group]; !expected {
			return nil, fmt.Errorf(
				"incremental cold graph stage %d returned query %s from group %q",
				stageNumber,
				results[index].Key,
				component.group,
			)
		}
		resultsByGroup[component.group] = append(resultsByGroup[component.group], results[index])
	}
	return resultsByGroup, nil
}

func (r *incrementalRenderSession) installColdGraphGroupResults(
	group string,
	prepared *incrementalPreparedColdGroupAdditions,
	applicable bool,
	groupResults []incremental.ExactResult,
) error {
	batched := false
	var err error
	if applicable {
		batched, err = r.installPreparedColdGroupAdditions(prepared)
	}
	if err != nil {
		return err
	}
	if batched {
		return nil
	}
	for index := range groupResults {
		if err := r.applyEvaluatedResult(group, &groupResults[index]); err != nil {
			return err
		}
	}
	return nil
}

func (r *incrementalRenderSession) completeColdGraphGroup(
	group string,
	completed map[string]*incrementalGroupIndex,
	pending map[string]struct{},
) error {
	if err := r.applyIncrementalSelectorChanges(group); err != nil {
		return fmt.Errorf("publishing incremental cold group %q: %w", group, err)
	}
	if err := r.refreshGroup(group); err != nil {
		return fmt.Errorf("refreshing incremental cold group %q: %w", group, err)
	}
	index := r.groupIndexes[group]
	if index == nil {
		return fmt.Errorf("incremental cold group %q has no completed index", group)
	}
	completed[group] = index
	delete(pending, group)
	return nil
}
