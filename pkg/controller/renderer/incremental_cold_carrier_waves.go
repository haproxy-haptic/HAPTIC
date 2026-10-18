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

package renderer

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/haproxy-haptic/HAPTIC/pkg/incremental"
)

type incrementalColdCarrierWaves struct {
	seal           *incrementalColdCarrierWaves
	waves          []incrementalColdCarrierPlannedWave
	workers        []incrementalColdCarrierPlannedWorker
	logicalQueries int
}

type incrementalColdCarrierPlannedWave struct {
	groups         []string
	lanes          []incrementalColdCarrierPlannedLane
	logicalQueries int
	activeWorkers  int
}

type incrementalColdCarrierPlannedWorker struct {
	waves []incrementalColdCarrierPlannedWorkerWave
}

type incrementalColdCarrierPlannedWorkerWave struct {
	lanes []incrementalColdCarrierPlannedLane
}

type incrementalColdCarrierPlannedLane struct {
	entryPoint string
	component  *incrementalComponent
	items      []incrementalColdCarrierPlannedItem
}

type incrementalColdCarrierPlannedItem struct {
	batchIndex int
	queryKey   incremental.QueryKey
	source     string
	namespace  string
	name       string
}

type incrementalColdCarrierSourceKey struct {
	source    string
	namespace string
	name      string
}

func (r *incrementalRenderSession) planIncrementalColdCarrierWaves(
	plan *incrementalCarrierPlan,
	graph *incrementalColdCarrierGraphSchedule,
	maxWorkers int,
) (*incrementalColdCarrierWaves, error) {
	if r == nil || r.state == nil || plan == nil || graph == nil || graph.seal != graph || maxWorkers <= 0 ||
		plan.logicalQueries != len(graph.keys) || len(graph.keys) == 0 {
		return nil, errors.New("incremental cold carrier wave plan is incomplete")
	}
	waves, err := r.planColdCarrierWaveSequence(plan, graph, maxWorkers)
	if err != nil {
		return nil, err
	}
	workerCount := 0
	for waveIndex := range waves {
		workerCount = max(workerCount, waves[waveIndex].activeWorkers)
	}
	if workerCount == 0 {
		return nil, errors.New("incremental cold carrier wave plan has no worker")
	}
	workers, err := assignIncrementalColdCarrierWaveWorkers(waves, workerCount)
	if err != nil {
		return nil, err
	}
	result := &incrementalColdCarrierWaves{
		waves:          waves,
		workers:        workers,
		logicalQueries: plan.logicalQueries,
	}
	result.seal = result
	if err := result.validate(graph); err != nil {
		return nil, err
	}
	return result, nil
}

func (r *incrementalRenderSession) planColdCarrierWaveSequence(
	plan *incrementalCarrierPlan,
	graph *incrementalColdCarrierGraphSchedule,
	maxWorkers int,
) ([]incrementalColdCarrierPlannedWave, error) {
	pending := make(map[string]struct{}, len(graph.groupOrder))
	for _, group := range graph.groupOrder {
		pending[group] = struct{}{}
	}
	completed := make(map[string]*incrementalGroupIndex, len(graph.groupOrder))
	waves := make([]incrementalColdCarrierPlannedWave, 0, len(plan.groupStages))
	for len(pending) > 0 {
		readySmall, readyBulk := r.coldGraphReadyPartitions(
			graph.groupOrder,
			pending,
			completed,
			graph.keysByGroup,
		)
		groups := readySmall
		if len(groups) == 0 {
			groups = readyBulk
		}
		if len(groups) == 0 {
			return nil, errors.New("incremental cold carrier wave plan has no dependency-ready group")
		}
		wave, err := planIncrementalColdCarrierWave(plan, graph, groups, len(waves), maxWorkers)
		if err != nil {
			return nil, err
		}
		waves = append(waves, wave)
		for _, group := range groups {
			completed[group] = nil
			delete(pending, group)
		}
	}
	return waves, nil
}

func planIncrementalColdCarrierWave(
	plan *incrementalCarrierPlan,
	graph *incrementalColdCarrierGraphSchedule,
	groups []string,
	waveIndex, maxWorkers int,
) (incrementalColdCarrierPlannedWave, error) {
	wave := incrementalColdCarrierPlannedWave{groups: slices.Clone(groups)}
	for _, group := range groups {
		wave.logicalQueries += len(graph.keysByGroup[group])
	}
	stage, stageSelected, err := selectIncrementalColdCarrierGraphStage(plan, groups)
	if err != nil {
		return wave, fmt.Errorf("selecting incremental cold carrier wave %d: %w", waveIndex, err)
	}
	if wave.logicalQueries == 0 {
		if stageSelected {
			return wave, fmt.Errorf("incremental cold carrier wave %d unexpectedly selected queries", waveIndex)
		}
		return wave, nil
	}
	if !stageSelected || stage.logicalQueries != wave.logicalQueries {
		return wave, fmt.Errorf(
			"incremental cold carrier wave %d selected %d queries, want %d",
			waveIndex,
			incrementalColdCarrierStageSize(stage),
			wave.logicalQueries,
		)
	}
	wave.lanes, err = planIncrementalColdCarrierWaveLanes(stage)
	if err != nil {
		return wave, fmt.Errorf("planning incremental cold carrier wave %d lanes: %w", waveIndex, err)
	}
	wave.activeWorkers = incrementalColdCarrierWaveWorkerCount(wave.lanes, maxWorkers)
	if wave.activeWorkers == 0 {
		return wave, fmt.Errorf("incremental cold carrier wave %d has no active worker", waveIndex)
	}
	return wave, nil
}

func assignIncrementalColdCarrierWaveWorkers(
	waves []incrementalColdCarrierPlannedWave,
	workerCount int,
) ([]incrementalColdCarrierPlannedWorker, error) {
	workers := make([]incrementalColdCarrierPlannedWorker, workerCount)
	for workerIndex := range workers {
		workers[workerIndex].waves = make([]incrementalColdCarrierPlannedWorkerWave, len(waves))
	}
	for waveIndex := range waves {
		wave := &waves[waveIndex]
		sourceWorkers, err := incrementalColdCarrierWaveSourceWorkers(wave)
		if err != nil {
			return nil, fmt.Errorf("assigning incremental cold carrier wave %d: %w", waveIndex, err)
		}
		for laneIndex := range wave.lanes {
			if err := assignIncrementalColdCarrierWaveLane(
				workers, waveIndex, &wave.lanes[laneIndex], wave.activeWorkers, sourceWorkers,
			); err != nil {
				return nil, err
			}
		}
	}
	return workers, nil
}

func assignIncrementalColdCarrierWaveLane(
	workers []incrementalColdCarrierPlannedWorker,
	waveIndex int,
	lane *incrementalColdCarrierPlannedLane,
	activeWorkers int,
	sourceWorkers map[incrementalColdCarrierSourceKey]int,
) error {
	workerItems := make([][]incrementalColdCarrierPlannedItem, activeWorkers)
	for itemIndex := range lane.items {
		item := lane.items[itemIndex]
		workerIndex, exists := sourceWorkers[incrementalColdCarrierSourceKeyFor(item)]
		if !exists || workerIndex < 0 || workerIndex >= activeWorkers {
			return fmt.Errorf("incremental cold carrier wave %d has an unassigned source", waveIndex)
		}
		workerItems[workerIndex] = append(workerItems[workerIndex], item)
	}
	for workerIndex := range workerItems {
		if len(workerItems[workerIndex]) == 0 {
			continue
		}
		workers[workerIndex].waves[waveIndex].lanes = append(
			workers[workerIndex].waves[waveIndex].lanes,
			incrementalColdCarrierPlannedLane{
				entryPoint: lane.entryPoint,
				component:  lane.component,
				items:      workerItems[workerIndex],
			},
		)
	}
	return nil
}

func planIncrementalColdCarrierWaveLanes(
	stage *incrementalCarrierStage,
) ([]incrementalColdCarrierPlannedLane, error) {
	if stage == nil || stage.logicalQueries <= 0 {
		return nil, errors.New("incremental cold carrier wave stage is incomplete")
	}
	byEntryPoint := make(map[string]*incrementalColdCarrierPlannedLane)
	seenIndexes := make(map[int]struct{}, stage.logicalQueries)
	seenQueries := make(map[incremental.QueryKey]struct{}, stage.logicalQueries)
	for carrierIndex := range stage.carriers {
		carrier := &stage.carriers[carrierIndex]
		if carrier.source == "" || carrier.name == "" {
			return nil, errors.New("incremental cold carrier wave has an invalid resource identity")
		}
		for laneIndex := range carrier.lanes {
			if err := planIncrementalColdCarrierWaveLane(
				carrier, laneIndex, byEntryPoint, seenIndexes, seenQueries,
			); err != nil {
				return nil, err
			}
		}
	}
	if len(seenIndexes) != stage.logicalQueries || len(seenQueries) != stage.logicalQueries {
		return nil, errors.New("incremental cold carrier wave omitted a query")
	}
	return sortedIncrementalColdCarrierPlannedLanes(byEntryPoint), nil
}

func planIncrementalColdCarrierWaveLane(
	carrier *incrementalCarrier,
	laneIndex int,
	byEntryPoint map[string]*incrementalColdCarrierPlannedLane,
	seenIndexes map[int]struct{},
	seenQueries map[incremental.QueryKey]struct{},
) error {
	lane := &carrier.lanes[laneIndex]
	if lane.component == nil || lane.component.entryPoint == "" ||
		!componentQueryKeyMatches(
			lane.queryKey,
			lane.component,
			carrier.source,
			carrier.namespace,
			carrier.name,
		) {
		return errors.New("incremental cold carrier wave has an invalid query association")
	}
	if _, duplicate := seenIndexes[lane.batchIndex]; duplicate {
		return fmt.Errorf("incremental cold carrier wave repeats batch item %d", lane.batchIndex)
	}
	if _, duplicate := seenQueries[lane.queryKey]; duplicate {
		return fmt.Errorf("incremental cold carrier wave repeats query %s", lane.queryKey)
	}
	seenIndexes[lane.batchIndex] = struct{}{}
	seenQueries[lane.queryKey] = struct{}{}
	planned := byEntryPoint[lane.component.entryPoint]
	if planned == nil {
		planned = &incrementalColdCarrierPlannedLane{
			entryPoint: lane.component.entryPoint,
			component:  lane.component,
		}
		byEntryPoint[lane.component.entryPoint] = planned
	} else if !sameIncrementalCarrierComponent(planned.component, lane.component) {
		return fmt.Errorf(
			"incremental cold carrier wave entry point %q belongs to multiple components",
			lane.component.entryPoint,
		)
	}
	planned.items = append(planned.items, incrementalColdCarrierPlannedItem{
		batchIndex: lane.batchIndex,
		queryKey:   lane.queryKey,
		source:     carrier.source,
		namespace:  carrier.namespace,
		name:       carrier.name,
	})
	return nil
}

func sortedIncrementalColdCarrierPlannedLanes(
	byEntryPoint map[string]*incrementalColdCarrierPlannedLane,
) []incrementalColdCarrierPlannedLane {
	entryPoints := make([]string, 0, len(byEntryPoint))
	for entryPoint := range byEntryPoint {
		entryPoints = append(entryPoints, entryPoint)
	}
	slices.Sort(entryPoints)
	lanes := make([]incrementalColdCarrierPlannedLane, len(entryPoints))
	for index, entryPoint := range entryPoints {
		planned := byEntryPoint[entryPoint]
		slices.SortFunc(planned.items, func(left, right incrementalColdCarrierPlannedItem) int {
			return left.batchIndex - right.batchIndex
		})
		lanes[index] = *planned
	}
	return lanes
}

func incrementalColdCarrierWaveWorkerCount(
	lanes []incrementalColdCarrierPlannedLane,
	maxWorkers int,
) int {
	maxItems := 0
	for laneIndex := range lanes {
		maxItems = max(maxItems, len(lanes[laneIndex].items))
	}
	if maxItems == 0 || maxWorkers <= 0 {
		return 0
	}
	return min(max(maxItems/incrementalColdVectorItemsPerShard, 1), maxWorkers)
}

func (s *incrementalColdCarrierWaves) validate(
	graph *incrementalColdCarrierGraphSchedule,
) error {
	if s == nil || s.seal != s || graph == nil || graph.seal != graph || len(s.waves) == 0 ||
		len(s.workers) == 0 || s.logicalQueries != len(graph.keys) {
		return errors.New("incremental cold carrier wave schedule has invalid authority")
	}
	if err := s.validateWaves(graph); err != nil {
		return err
	}
	return s.validateWorkers()
}

func (s *incrementalColdCarrierWaves) validateWaves(
	graph *incrementalColdCarrierGraphSchedule,
) error {
	groupPositions, queryGroups, err := indexIncrementalColdCarrierGraphQueries(graph)
	if err != nil {
		return err
	}

	seenGroups := make(map[string]struct{}, len(groupPositions))
	seen := make([]bool, len(graph.keys))
	logicalQueries := 0
	for waveIndex := range s.waves {
		waveQueries, err := s.validateWave(
			waveIndex, graph, groupPositions, queryGroups, seenGroups, seen,
		)
		if err != nil {
			return err
		}
		logicalQueries += waveQueries
	}
	if len(seenGroups) != len(groupPositions) || logicalQueries != s.logicalQueries {
		return errors.New("incremental cold carrier wave schedule omitted a query")
	}
	for batchIndex, itemSeen := range seen {
		if !itemSeen {
			return fmt.Errorf("incremental cold carrier wave schedule omitted batch item %d", batchIndex)
		}
	}
	return nil
}

func indexIncrementalColdCarrierGraphQueries(
	graph *incrementalColdCarrierGraphSchedule,
) (groupPositions map[string]int, queryGroups map[incremental.QueryKey]string, err error) {
	groupPositions = make(map[string]int, len(graph.groupOrder))
	queryGroups = make(map[incremental.QueryKey]string, len(graph.keys))
	for groupIndex, group := range graph.groupOrder {
		if group == "" {
			return nil, nil, errors.New("incremental cold carrier wave schedule has an empty group")
		}
		if _, duplicate := groupPositions[group]; duplicate {
			return nil, nil, fmt.Errorf("incremental cold carrier wave schedule repeats group %q", group)
		}
		groupPositions[group] = groupIndex
		keys := graph.keysByGroup[group]
		if !slices.IsSortedFunc(keys, compareIncrementalQueryKey) {
			return nil, nil, fmt.Errorf("incremental cold carrier wave group %q has noncanonical queries", group)
		}
		for _, key := range keys {
			batchIndex, exists := graph.queryIndexes[key]
			if !exists || batchIndex < 0 || batchIndex >= len(graph.keys) || graph.keys[batchIndex] != key {
				return nil, nil, fmt.Errorf("incremental cold carrier wave group %q has an invalid query", group)
			}
			if _, duplicate := queryGroups[key]; duplicate {
				return nil, nil, fmt.Errorf(
					"incremental cold carrier wave query %s belongs to multiple groups", key,
				)
			}
			queryGroups[key] = group
		}
	}
	if len(groupPositions) != len(graph.keysByGroup) || len(queryGroups) != len(graph.keys) ||
		len(graph.queryIndexes) != len(graph.keys) ||
		!slices.IsSortedFunc(graph.keys, compareIncrementalQueryKey) {
		return nil, nil, errors.New("incremental cold carrier wave graph is incomplete")
	}
	return groupPositions, queryGroups, nil
}

func (s *incrementalColdCarrierWaves) validateWave(
	waveIndex int,
	graph *incrementalColdCarrierGraphSchedule,
	groupPositions map[string]int,
	queryGroups map[incremental.QueryKey]string,
	seenGroups map[string]struct{},
	seen []bool,
) (waveQueries int, err error) {
	wave := &s.waves[waveIndex]
	waveGroups, expectedQueries, err := validateIncrementalColdCarrierWaveGroups(
		wave, waveIndex, graph, groupPositions, seenGroups,
	)
	if err != nil {
		return 0, err
	}
	waveQueries, err = validateIncrementalColdCarrierWaveLanes(
		wave, waveIndex, graph, queryGroups, waveGroups, seen,
	)
	if err != nil {
		return 0, err
	}
	if waveQueries != expectedQueries || wave.logicalQueries != expectedQueries ||
		wave.activeWorkers != incrementalColdCarrierWaveWorkerCount(wave.lanes, len(s.workers)) {
		return 0, fmt.Errorf("incremental cold carrier wave %d has an invalid shape", waveIndex)
	}
	return waveQueries, nil
}

func validateIncrementalColdCarrierWaveGroups(
	wave *incrementalColdCarrierPlannedWave,
	waveIndex int,
	graph *incrementalColdCarrierGraphSchedule,
	groupPositions map[string]int,
	seenGroups map[string]struct{},
) (waveGroups map[string]struct{}, expectedQueries int, err error) {
	waveGroups = make(map[string]struct{}, len(wave.groups))
	previousGroupPosition := -1
	for _, group := range wave.groups {
		position, exists := groupPositions[group]
		if !exists || position <= previousGroupPosition {
			return nil, 0, fmt.Errorf("incremental cold carrier wave %d has a noncanonical group", waveIndex)
		}
		if _, duplicate := seenGroups[group]; duplicate {
			return nil, 0, fmt.Errorf("incremental cold carrier wave schedule repeats group %q", group)
		}
		seenGroups[group] = struct{}{}
		waveGroups[group] = struct{}{}
		previousGroupPosition = position
		expectedQueries += len(graph.keysByGroup[group])
	}
	return waveGroups, expectedQueries, nil
}

func validateIncrementalColdCarrierWaveLanes(
	wave *incrementalColdCarrierPlannedWave,
	waveIndex int,
	graph *incrementalColdCarrierGraphSchedule,
	queryGroups map[incremental.QueryKey]string,
	waveGroups map[string]struct{},
	seen []bool,
) (waveQueries int, err error) {
	previousEntryPoint := ""
	for laneIndex := range wave.lanes {
		lane := &wave.lanes[laneIndex]
		if lane.entryPoint == "" || lane.component == nil || lane.component.entryPoint != lane.entryPoint ||
			(previousEntryPoint != "" && strings.Compare(previousEntryPoint, lane.entryPoint) >= 0) {
			return 0, fmt.Errorf("incremental cold carrier wave %d has an invalid lane", waveIndex)
		}
		if _, included := waveGroups[lane.component.group]; !included {
			return 0, fmt.Errorf("incremental cold carrier wave %d has a lane from another group", waveIndex)
		}
		previousEntryPoint = lane.entryPoint
		laneQueries, laneErr := validateIncrementalColdCarrierWaveLaneItems(
			lane, waveIndex, graph, queryGroups, seen,
		)
		if laneErr != nil {
			return 0, laneErr
		}
		waveQueries += laneQueries
	}
	return waveQueries, nil
}

func validateIncrementalColdCarrierWaveLaneItems(
	lane *incrementalColdCarrierPlannedLane,
	waveIndex int,
	graph *incrementalColdCarrierGraphSchedule,
	queryGroups map[incremental.QueryKey]string,
	seen []bool,
) (laneQueries int, err error) {
	previousBatchIndex := -1
	for itemIndex := range lane.items {
		item := lane.items[itemIndex]
		if item.batchIndex < 0 || item.batchIndex >= len(graph.keys) ||
			item.batchIndex <= previousBatchIndex || item.queryKey != graph.keys[item.batchIndex] ||
			seen[item.batchIndex] || item.source == "" || item.name == "" ||
			queryGroups[item.queryKey] != lane.component.group ||
			!componentQueryKeyMatches(
				item.queryKey,
				lane.component,
				item.source,
				item.namespace,
				item.name,
			) {
			return 0, fmt.Errorf("incremental cold carrier wave %d has an invalid item", waveIndex)
		}
		seen[item.batchIndex] = true
		previousBatchIndex = item.batchIndex
		laneQueries++
	}
	return laneQueries, nil
}

func (s *incrementalColdCarrierWaves) validateWorkers() error {
	maxActiveWorkers := 0
	for waveIndex := range s.waves {
		maxActiveWorkers = max(maxActiveWorkers, s.waves[waveIndex].activeWorkers)
	}
	if maxActiveWorkers != len(s.workers) {
		return errors.New("incremental cold carrier wave schedule has an invalid worker count")
	}
	for workerIndex := range s.workers {
		if len(s.workers[workerIndex].waves) != len(s.waves) {
			return fmt.Errorf("incremental cold carrier worker %d has an invalid wave count", workerIndex)
		}
	}
	// The source-to-worker assignment is a function of the wave alone, so it is
	// built once per wave rather than once per (worker, wave) pair. At chart
	// scale the inner form rebuilt the same three maps for every worker.
	for waveIndex := range s.waves {
		wave := &s.waves[waveIndex]
		sourceWorkers, err := incrementalColdCarrierWaveSourceWorkers(wave)
		if err != nil {
			return fmt.Errorf("incremental cold carrier wave %d assignment is invalid: %w", waveIndex, err)
		}
		for workerIndex := range s.workers {
			expected, err := incrementalColdCarrierExpectedWorkerWaveFrom(wave, workerIndex, sourceWorkers)
			if err != nil {
				return fmt.Errorf(
					"incremental cold carrier worker %d wave %d assignment is invalid: %w",
					workerIndex,
					waveIndex,
					err,
				)
			}
			if !sameIncrementalColdCarrierPlannedLanes(s.workers[workerIndex].waves[waveIndex].lanes, expected) {
				return fmt.Errorf("incremental cold carrier worker %d wave %d has an invalid assignment", workerIndex, waveIndex)
			}
		}
	}
	return nil
}

func incrementalColdCarrierExpectedWorkerWave(
	wave *incrementalColdCarrierPlannedWave,
	workerIndex int,
) ([]incrementalColdCarrierPlannedLane, error) {
	sourceWorkers, err := incrementalColdCarrierWaveSourceWorkers(wave)
	if err != nil {
		return nil, err
	}
	return incrementalColdCarrierExpectedWorkerWaveFrom(wave, workerIndex, sourceWorkers)
}

// incrementalColdCarrierExpectedWorkerWaveFrom is the same assignment against a
// source-to-worker map the caller already built for this wave.
func incrementalColdCarrierExpectedWorkerWaveFrom(
	wave *incrementalColdCarrierPlannedWave,
	workerIndex int,
	sourceWorkers map[incrementalColdCarrierSourceKey]int,
) ([]incrementalColdCarrierPlannedLane, error) {
	if wave == nil || workerIndex < 0 || wave.activeWorkers < 0 {
		return nil, errors.New("incremental cold carrier worker assignment is invalid")
	}
	if workerIndex >= wave.activeWorkers {
		return nil, nil
	}
	lanes := make([]incrementalColdCarrierPlannedLane, 0, len(wave.lanes))
	for laneIndex := range wave.lanes {
		lane := &wave.lanes[laneIndex]
		items := make([]incrementalColdCarrierPlannedItem, 0, len(lane.items)/wave.activeWorkers+1)
		for itemIndex := range lane.items {
			item := lane.items[itemIndex]
			assigned, exists := sourceWorkers[incrementalColdCarrierSourceKeyFor(item)]
			if !exists {
				return nil, errors.New("incremental cold carrier wave has an unassigned source")
			}
			if assigned == workerIndex {
				items = append(items, item)
			}
		}
		if len(items) == 0 {
			continue
		}
		lanes = append(lanes, incrementalColdCarrierPlannedLane{
			entryPoint: lane.entryPoint,
			component:  lane.component,
			items:      items,
		})
	}
	return lanes, nil
}

func incrementalColdCarrierSourceKeyFor(
	item incrementalColdCarrierPlannedItem,
) incrementalColdCarrierSourceKey {
	return incrementalColdCarrierSourceKey{
		source: item.source, namespace: item.namespace, name: item.name,
	}
}

func incrementalColdCarrierWaveSourceWorkers(
	wave *incrementalColdCarrierPlannedWave,
) (map[incrementalColdCarrierSourceKey]int, error) {
	if wave == nil || wave.activeWorkers < 0 || (wave.activeWorkers == 0) != (len(wave.lanes) == 0) {
		return nil, errors.New("incremental cold carrier wave source assignment is incomplete")
	}
	if wave.activeWorkers == 0 {
		return map[incrementalColdCarrierSourceKey]int{}, nil
	}
	sources, err := collectIncrementalColdCarrierWaveSources(wave)
	if err != nil {
		return nil, err
	}
	if len(sources) < wave.activeWorkers {
		return nil, errors.New("incremental cold carrier wave has more workers than sources")
	}
	ordered := make([]incrementalColdCarrierSourceKey, 0, len(sources))
	for source := range sources {
		ordered = append(ordered, source)
	}
	slices.SortFunc(ordered, func(left, right incrementalColdCarrierSourceKey) int {
		if compared := strings.Compare(left.source, right.source); compared != 0 {
			return compared
		}
		if compared := strings.Compare(left.namespace, right.namespace); compared != 0 {
			return compared
		}
		return strings.Compare(left.name, right.name)
	})
	workers := make(map[incrementalColdCarrierSourceKey]int, len(ordered))
	for sourceIndex, source := range ordered {
		workers[source] = sourceIndex * wave.activeWorkers / len(ordered)
	}
	return workers, nil
}

func collectIncrementalColdCarrierWaveSources(
	wave *incrementalColdCarrierPlannedWave,
) (map[incrementalColdCarrierSourceKey]struct{}, error) {
	// Sized up front: a wave carries one item per component execution, so these
	// grow to tens of thousands at chart scale and rehashing all the way up was
	// the largest single allocation site of a cold render.
	items := 0
	for laneIndex := range wave.lanes {
		items += len(wave.lanes[laneIndex].items)
	}
	sources := make(map[incrementalColdCarrierSourceKey]struct{}, items)
	seenIndexes := make(map[int]struct{}, items)
	seenQueries := make(map[incremental.QueryKey]struct{}, items)
	for laneIndex := range wave.lanes {
		lane := &wave.lanes[laneIndex]
		if lane.entryPoint == "" || lane.component == nil || lane.component.entryPoint != lane.entryPoint ||
			len(lane.items) == 0 {
			return nil, errors.New("incremental cold carrier wave has an invalid source lane")
		}
		if err := collectIncrementalColdCarrierLaneSources(
			lane, sources, seenIndexes, seenQueries,
		); err != nil {
			return nil, err
		}
	}
	return sources, nil
}

func collectIncrementalColdCarrierLaneSources(
	lane *incrementalColdCarrierPlannedLane,
	sources map[incrementalColdCarrierSourceKey]struct{},
	seenIndexes map[int]struct{},
	seenQueries map[incremental.QueryKey]struct{},
) error {
	for itemIndex := range lane.items {
		item := lane.items[itemIndex]
		if item.batchIndex < 0 || item.source == "" || item.name == "" ||
			!componentQueryKeyMatches(
				item.queryKey,
				lane.component,
				item.source,
				item.namespace,
				item.name,
			) {
			return errors.New("incremental cold carrier wave has an invalid source item")
		}
		if _, duplicate := seenIndexes[item.batchIndex]; duplicate {
			return fmt.Errorf("incremental cold carrier wave repeats batch item %d", item.batchIndex)
		}
		if _, duplicate := seenQueries[item.queryKey]; duplicate {
			return fmt.Errorf("incremental cold carrier wave repeats query %s", item.queryKey)
		}
		seenIndexes[item.batchIndex] = struct{}{}
		seenQueries[item.queryKey] = struct{}{}
		sources[incrementalColdCarrierSourceKeyFor(item)] = struct{}{}
	}
	return nil
}

func sameIncrementalColdCarrierPlannedLanes(
	left []incrementalColdCarrierPlannedLane,
	right []incrementalColdCarrierPlannedLane,
) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].entryPoint != right[index].entryPoint ||
			left[index].component == nil || right[index].component == nil ||
			!sameIncrementalCarrierComponent(left[index].component, right[index].component) ||
			!slices.Equal(left[index].items, right[index].items) {
			return false
		}
	}
	return true
}
