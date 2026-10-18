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
	"cmp"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/haproxy-haptic/HAPTIC/pkg/incremental"
)

type incrementalCarrierPlan struct {
	stages         []incrementalCarrierStage
	groupStages    []incrementalCarrierGroupStage
	logicalQueries int
}

type incrementalCarrierGroupStage struct {
	wave   int
	groups []string
}

type incrementalCarrierStage struct {
	wave           int
	groups         []string
	carriers       []incrementalCarrier
	logicalQueries int
}

type incrementalCarrier struct {
	source    string
	namespace string
	name      string
	lanes     []incrementalCarrierLane
}

type incrementalCarrierLane struct {
	batchIndex int
	queryKey   incremental.QueryKey
	component  *incrementalComponent
}

type incrementalCarrierQuery struct {
	batchIndex    int
	queryKey      incremental.QueryKey
	componentName string
	source        string
	namespace     string
	name          string
}

type incrementalCarrierTopology struct {
	components    map[string]*incrementalComponent
	edges         map[string][]string
	groupWaves    map[string]int
	groupRanks    map[string]int
	componentRank map[string]int
}

type incrementalCarrierIdentity struct {
	source    string
	namespace string
	name      string
}

func (r *incrementalRenderSession) planColdComponentCarriers(
	batch incremental.ColdExactBatch,
) (*incrementalCarrierPlan, error) {
	if r == nil || r.state == nil || batch.Len() == 0 {
		return nil, errors.New("incremental cold carrier batch is incomplete")
	}
	queries := make([]incrementalCarrierQuery, batch.Len())
	for index := 0; index < batch.Len(); index++ {
		query, err := r.resolveIncrementalCarrierQuery(index, batch.Query(index).Key())
		if err != nil {
			return nil, err
		}
		queries[index] = query
	}
	return planIncrementalComponentCarriers(r.state, queries)
}

func (r *incrementalRenderSession) planColdComponentCarrierKeys(
	keys []incremental.QueryKey,
) (*incrementalCarrierPlan, error) {
	if r == nil || r.state == nil || len(keys) == 0 {
		return nil, errors.New("incremental cold carrier keys are incomplete")
	}
	ordered := slices.Clone(keys)
	slices.SortFunc(ordered, func(left, right incremental.QueryKey) int {
		return strings.Compare(left.Opaque(), right.Opaque())
	})
	queries := make([]incrementalCarrierQuery, len(ordered))
	for index, key := range ordered {
		if key.Opaque() == "" {
			return nil, errors.New("incremental cold carrier query key is empty")
		}
		if index > 0 && ordered[index-1] == key {
			return nil, fmt.Errorf("incremental cold carrier query %s is duplicated", key)
		}
		query, err := r.resolveIncrementalCarrierQuery(index, key)
		if err != nil {
			return nil, err
		}
		queries[index] = query
	}
	return planIncrementalComponentCarriers(r.state, queries)
}

func (r *incrementalRenderSession) resolveIncrementalCarrierQuery(
	batchIndex int,
	queryKey incremental.QueryKey,
) (incrementalCarrierQuery, error) {
	component, source, namespace, name, ok := r.resolveComponentQuery(queryKey)
	if !ok {
		return incrementalCarrierQuery{}, fmt.Errorf(
			"incremental cold carrier received non-component query %s",
			queryKey,
		)
	}
	return incrementalCarrierQuery{
		batchIndex:    batchIndex,
		queryKey:      queryKey,
		componentName: component.name,
		source:        source,
		namespace:     namespace,
		name:          name,
	}, nil
}

func planIncrementalComponentCarriers(
	state *incrementalRenderState,
	queries []incrementalCarrierQuery,
) (*incrementalCarrierPlan, error) {
	topology, err := buildIncrementalCarrierTopology(state)
	if err != nil {
		return nil, err
	}
	plan := &incrementalCarrierPlan{groupStages: incrementalCarrierGroupStages(state, topology)}
	if len(queries) == 0 {
		return plan, nil
	}
	represented, err := validateIncrementalCarrierQueries(topology, queries)
	if err != nil {
		return nil, err
	}
	groupWaves := projectIncrementalCarrierWaves(topology, represented)
	plan.stages = buildIncrementalCarrierStages(topology, queries, groupWaves)
	plan.logicalQueries = len(queries)
	return plan, nil
}

func validateIncrementalCarrierQueries(
	topology *incrementalCarrierTopology,
	queries []incrementalCarrierQuery,
) (map[string]struct{}, error) {
	represented := make(map[string]struct{})
	indexes := make(map[int]struct{}, len(queries))
	queryKeys := make(map[incremental.QueryKey]struct{}, len(queries))
	for _, query := range queries {
		if query.batchIndex < 0 || query.batchIndex >= len(queries) {
			return nil, fmt.Errorf("incremental carrier query has invalid batch index %d", query.batchIndex)
		}
		if _, duplicate := indexes[query.batchIndex]; duplicate {
			return nil, fmt.Errorf("incremental carrier batch index %d is duplicated", query.batchIndex)
		}
		indexes[query.batchIndex] = struct{}{}
		if _, duplicate := queryKeys[query.queryKey]; duplicate {
			return nil, fmt.Errorf(
				"incremental carrier query %s is duplicated",
				query.queryKey,
			)
		}
		queryKeys[query.queryKey] = struct{}{}

		canonical := topology.components[query.componentName]
		if canonical == nil {
			return nil, fmt.Errorf(
				"incremental carrier query %s has an unknown component",
				query.queryKey,
			)
		}
		if query.source == "" || query.name == "" ||
			!componentQueryKeyMatches(query.queryKey, canonical, query.source, query.namespace, query.name) {
			return nil, fmt.Errorf(
				"incremental carrier query %s has invalid resource identity",
				query.queryKey,
			)
		}
		if canonical.source != "" && canonical.source != query.source {
			return nil, fmt.Errorf(
				"incremental carrier query %s uses source %q, want %q",
				query.queryKey,
				query.source,
				canonical.source,
			)
		}
		represented[canonical.group] = struct{}{}
	}
	return represented, nil
}

func buildIncrementalCarrierStages(
	topology *incrementalCarrierTopology,
	queries []incrementalCarrierQuery,
	groupWaves map[string]int,
) []incrementalCarrierStage {
	stageCarriers := make(map[int]map[incrementalCarrierIdentity][]incrementalCarrierLane)
	stageGroups := make(map[int]map[string]struct{})
	for _, query := range queries {
		component := topology.components[query.componentName]
		wave := groupWaves[component.group]
		identity := incrementalCarrierIdentity{
			source:    query.source,
			namespace: query.namespace,
			name:      query.name,
		}
		if stageCarriers[wave] == nil {
			stageCarriers[wave] = make(map[incrementalCarrierIdentity][]incrementalCarrierLane)
			stageGroups[wave] = make(map[string]struct{})
		}
		stageCarriers[wave][identity] = append(stageCarriers[wave][identity], incrementalCarrierLane{
			batchIndex: query.batchIndex,
			queryKey:   query.queryKey,
			component:  component,
		})
		stageGroups[wave][component.group] = struct{}{}
	}

	waves := make([]int, 0, len(stageCarriers))
	for wave := range stageCarriers {
		waves = append(waves, wave)
	}
	slices.Sort(waves)
	stages := make([]incrementalCarrierStage, 0, len(waves))
	for _, wave := range waves {
		stages = append(stages, buildIncrementalCarrierStage(
			topology,
			wave,
			stageGroups[wave],
			stageCarriers[wave],
		))
	}
	return stages
}

func buildIncrementalCarrierStage(
	topology *incrementalCarrierTopology,
	wave int,
	groupSet map[string]struct{},
	lanesByIdentity map[incrementalCarrierIdentity][]incrementalCarrierLane,
) incrementalCarrierStage {
	stage := incrementalCarrierStage{wave: wave}
	for group := range groupSet {
		stage.groups = append(stage.groups, group)
	}
	slices.SortFunc(stage.groups, func(left, right string) int {
		return cmp.Or(
			cmp.Compare(topology.groupRanks[left], topology.groupRanks[right]),
			strings.Compare(left, right),
		)
	})

	identities := make([]incrementalCarrierIdentity, 0, len(lanesByIdentity))
	for identity := range lanesByIdentity {
		identities = append(identities, identity)
	}
	slices.SortFunc(identities, compareIncrementalCarrierIdentity)
	stage.carriers = make([]incrementalCarrier, 0, len(identities))
	for _, identity := range identities {
		lanes := lanesByIdentity[identity]
		slices.SortFunc(lanes, func(left, right incrementalCarrierLane) int {
			return cmp.Or(
				cmp.Compare(topology.groupRanks[left.component.group], topology.groupRanks[right.component.group]),
				cmp.Compare(topology.componentRank[left.component.name], topology.componentRank[right.component.name]),
				strings.Compare(left.queryKey.Opaque(), right.queryKey.Opaque()),
				cmp.Compare(left.batchIndex, right.batchIndex),
			)
		})
		stage.carriers = append(stage.carriers, incrementalCarrier{
			source:    identity.source,
			namespace: identity.namespace,
			name:      identity.name,
			lanes:     lanes,
		})
		stage.logicalQueries += len(lanes)
	}
	return stage
}

func incrementalCarrierGroupStages(
	state *incrementalRenderState,
	topology *incrementalCarrierTopology,
) []incrementalCarrierGroupStage {
	demandDriven := state.resourceProjectionDemandDrivenClosure()
	groupsByWave := make(map[int][]string)
	for group, wave := range topology.groupWaves {
		if demandDriven[group] {
			continue
		}
		groupsByWave[wave] = append(groupsByWave[wave], group)
	}
	waves := make([]int, 0, len(groupsByWave))
	for wave := range groupsByWave {
		waves = append(waves, wave)
	}
	slices.Sort(waves)
	stages := make([]incrementalCarrierGroupStage, 0, len(waves))
	for _, wave := range waves {
		groups := groupsByWave[wave]
		slices.SortFunc(groups, func(left, right string) int {
			return cmp.Or(
				cmp.Compare(topology.groupRanks[left], topology.groupRanks[right]),
				strings.Compare(left, right),
			)
		})
		stages = append(stages, incrementalCarrierGroupStage{wave: wave, groups: groups})
	}
	return stages
}

func buildIncrementalCarrierTopology(state *incrementalRenderState) (*incrementalCarrierTopology, error) {
	topology, groupNames, groupComponents, err := validateIncrementalCarrierTopologyState(state)
	if err != nil {
		return nil, err
	}
	for _, group := range groupNames {
		requirements, err := incrementalCarrierGroupRequirements(groupComponents[group])
		if err != nil {
			return nil, err
		}
		if err := populateIncrementalCarrierGroupEdges(
			state,
			topology,
			groupComponents,
			group,
			requirements,
		); err != nil {
			return nil, err
		}
	}
	if err := populateIncrementalCarrierWaves(topology, groupNames); err != nil {
		return nil, err
	}
	populateIncrementalCarrierRanks(topology, groupNames, groupComponents)
	return topology, nil
}

func validateIncrementalCarrierTopologyState(
	state *incrementalRenderState,
) (
	topology *incrementalCarrierTopology,
	groupNames []string,
	groupComponents map[string][]incrementalComponent,
	err error,
) {
	if state == nil || len(state.components) == 0 || len(state.groups) == 0 {
		return nil, nil, nil, errors.New("incremental carrier topology is unavailable")
	}
	for group := range state.dependencies {
		if _, exists := state.groups[group]; !exists {
			return nil, nil, nil,
				fmt.Errorf("incremental carrier topology has dependencies for unknown group %q", group)
		}
	}

	groupNames = make([]string, 0, len(state.groups))
	for group := range state.groups {
		groupNames = append(groupNames, group)
	}
	slices.Sort(groupNames)
	topology = &incrementalCarrierTopology{
		components:    make(map[string]*incrementalComponent, len(state.components)),
		edges:         make(map[string][]string, len(state.groups)),
		groupWaves:    make(map[string]int, len(state.groups)),
		groupRanks:    make(map[string]int, len(state.groups)),
		componentRank: make(map[string]int, len(state.components)),
	}
	memberships := make(map[string]string, len(state.components))
	groupComponents, err = validateIncrementalCarrierGroupMembership(state, groupNames, memberships)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := populateIncrementalCarrierTopologyComponents(state, memberships, topology); err != nil {
		return nil, nil, nil, err
	}
	return topology, groupNames, groupComponents, nil
}

func validateIncrementalCarrierGroupMembership(
	state *incrementalRenderState,
	groupNames []string,
	memberships map[string]string,
) (map[string][]incrementalComponent, error) {
	groupComponents := make(map[string][]incrementalComponent, len(state.groups))
	for _, group := range groupNames {
		if group == "" || len(state.groups[group]) == 0 {
			return nil, fmt.Errorf("incremental carrier topology has an invalid group %q", group)
		}
		components := slices.Clone(state.groups[group])
		slices.SortFunc(components, func(left, right incrementalComponent) int {
			return strings.Compare(left.name, right.name)
		})
		if err := validateIncrementalCarrierGroupComponents(
			state, group, components, memberships,
		); err != nil {
			return nil, err
		}
		groupComponents[group] = components
	}
	return groupComponents, nil
}

func validateIncrementalCarrierGroupComponents(
	state *incrementalRenderState,
	group string,
	components []incrementalComponent,
	memberships map[string]string,
) error {
	for index := range components {
		component := &components[index]
		canonical, exists := state.components[component.name]
		if !exists || component.name == "" || component.entryPoint == "" || component.group != group ||
			!sameIncrementalCarrierComponent(&canonical, component) {
			return fmt.Errorf(
				"incremental carrier group %q has invalid component %q",
				group,
				component.name,
			)
		}
		if previous, duplicate := memberships[component.name]; duplicate {
			return fmt.Errorf(
				"incremental carrier component %q belongs to groups %q and %q",
				component.name,
				previous,
				group,
			)
		}
		memberships[component.name] = group
	}
	return nil
}

func populateIncrementalCarrierTopologyComponents(
	state *incrementalRenderState,
	memberships map[string]string,
	topology *incrementalCarrierTopology,
) error {
	for name := range state.components {
		component := state.components[name]
		if name != component.name || memberships[name] != component.group {
			return fmt.Errorf("incremental carrier component %q has invalid group membership", name)
		}
		cloned := cloneIncrementalCarrierComponent(&component)
		topology.components[name] = &cloned
	}
	return nil
}

func incrementalCarrierGroupRequirements(components []incrementalComponent) (map[string]bool, error) {
	requirements := make(map[string]bool)
	for index := range components {
		component := &components[index]
		seen := make(map[string]string, len(component.consumes)+len(component.optionalConsumes))
		for _, dependency := range component.consumes {
			if err := addIncrementalCarrierDependency(seen, requirements, dependency, true); err != nil {
				return nil, fmt.Errorf("incremental carrier component %q: %w", component.name, err)
			}
		}
		for _, dependency := range component.optionalConsumes {
			if err := addIncrementalCarrierDependency(seen, requirements, dependency, false); err != nil {
				return nil, fmt.Errorf("incremental carrier component %q: %w", component.name, err)
			}
		}
	}
	return requirements, nil
}

func populateIncrementalCarrierGroupEdges(
	state *incrementalRenderState,
	topology *incrementalCarrierTopology,
	groupComponents map[string][]incrementalComponent,
	group string,
	requirements map[string]bool,
) error {
	expected := make([]string, 0, len(requirements))
	for dependency := range requirements {
		expected = append(expected, dependency)
	}
	slices.Sort(expected)
	declared, err := canonicalIncrementalCarrierDependencies(state.dependencies[group])
	if err != nil {
		return fmt.Errorf("incremental carrier group %q: %w", group, err)
	}
	if !slices.Equal(expected, declared) {
		return fmt.Errorf(
			"incremental carrier group %q dependency index does not match its components",
			group,
		)
	}
	for _, dependency := range expected {
		producer, exists := groupComponents[dependency]
		if !exists {
			if requirements[dependency] || !incrementalCarrierDependencyAbsent(state, dependency) {
				return fmt.Errorf(
					"incremental carrier group %q has unavailable dependency %q",
					group,
					dependency,
				)
			}
			continue
		}
		if !incrementalCarrierGroupPublishes(producer) {
			return fmt.Errorf(
				"incremental carrier dependency group %q does not publish values",
				dependency,
			)
		}
		topology.edges[group] = append(topology.edges[group], dependency)
	}
	return nil
}

func incrementalCarrierDependencyAbsent(state *incrementalRenderState, dependency string) bool {
	if state.config == nil {
		return false
	}
	_, authenticated := state.config.AbsentIncrementalGroups[dependency]
	return authenticated
}

func incrementalCarrierGroupPublishes(components []incrementalComponent) bool {
	for index := range components {
		if components[index].publishValue {
			return true
		}
	}
	return false
}

func populateIncrementalCarrierRanks(
	topology *incrementalCarrierTopology,
	groupNames []string,
	groupComponents map[string][]incrementalComponent,
) {
	slices.SortFunc(groupNames, func(left, right string) int {
		return cmp.Or(
			cmp.Compare(topology.groupWaves[left], topology.groupWaves[right]),
			strings.Compare(left, right),
		)
	})
	componentRank := 0
	for groupRank, group := range groupNames {
		topology.groupRanks[group] = groupRank
		for index := range groupComponents[group] {
			topology.componentRank[groupComponents[group][index].name] = componentRank
			componentRank++
		}
	}
}

func addIncrementalCarrierDependency(
	seen map[string]string,
	requirements map[string]bool,
	dependency string,
	required bool,
) error {
	field := "optionalConsumes"
	if required {
		field = "consumes"
	}
	if dependency == "" {
		return fmt.Errorf("incremental %s contains an empty group", field)
	}
	if previous, duplicate := seen[dependency]; duplicate {
		return fmt.Errorf("incremental %s contains group %q already declared in %s", field, dependency, previous)
	}
	seen[dependency] = field
	requirements[dependency] = requirements[dependency] || required
	return nil
}

func canonicalIncrementalCarrierDependencies(dependencies []string) ([]string, error) {
	result := slices.Clone(dependencies)
	slices.Sort(result)
	for index, dependency := range result {
		if dependency == "" {
			return nil, errors.New("dependency index contains an empty group")
		}
		if index > 0 && dependency == result[index-1] {
			return nil, fmt.Errorf("dependency index contains group %q more than once", dependency)
		}
	}
	return result, nil
}

func populateIncrementalCarrierWaves(topology *incrementalCarrierTopology, groups []string) error {
	const (
		incrementalCarrierUnvisited = iota
		incrementalCarrierVisiting
		incrementalCarrierVisited
	)
	states := make(map[string]int, len(groups))
	stack := make([]string, 0, len(groups))
	var visit func(string) (int, error)
	visit = func(group string) (int, error) {
		switch states[group] {
		case incrementalCarrierVisited:
			return topology.groupWaves[group], nil
		case incrementalCarrierVisiting:
			start := slices.Index(stack, group)
			cycle := append(slices.Clone(stack[start:]), group)
			return 0, fmt.Errorf("incremental carrier dependency cycle: %s", strings.Join(cycle, " -> "))
		}
		states[group] = incrementalCarrierVisiting
		stack = append(stack, group)
		wave := 0
		for _, dependency := range topology.edges[group] {
			dependencyWave, err := visit(dependency)
			if err != nil {
				return 0, err
			}
			wave = max(wave, dependencyWave+1)
		}
		stack = stack[:len(stack)-1]
		states[group] = incrementalCarrierVisited
		topology.groupWaves[group] = wave
		return wave, nil
	}
	for _, group := range groups {
		if _, err := visit(group); err != nil {
			return err
		}
	}
	return nil
}

func projectIncrementalCarrierWaves(
	topology *incrementalCarrierTopology,
	represented map[string]struct{},
) map[string]int {
	waves := make(map[string]int, len(represented))
	var visit func(string) int
	visit = func(group string) int {
		if wave, exists := waves[group]; exists {
			return wave
		}
		wave := 0
		for _, dependency := range topology.edges[group] {
			if _, included := represented[dependency]; included {
				wave = max(wave, visit(dependency)+1)
			}
		}
		waves[group] = wave
		return wave
	}
	for group := range represented {
		visit(group)
	}
	return waves
}

func compareIncrementalCarrierIdentity(left, right incrementalCarrierIdentity) int {
	return cmp.Or(
		strings.Compare(left.source, right.source),
		strings.Compare(left.namespace, right.namespace),
		strings.Compare(left.name, right.name),
	)
}

func cloneIncrementalCarrierComponent(component *incrementalComponent) incrementalComponent {
	cloned := *component
	cloned.consumes = slices.Clone(component.consumes)
	cloned.optionalConsumes = slices.Clone(component.optionalConsumes)
	cloned.activationPaths = slices.Clone(component.activationPaths)
	return cloned
}

func sameIncrementalCarrierComponent(left, right *incrementalComponent) bool {
	return left.name == right.name &&
		left.entryPoint == right.entryPoint &&
		left.source == right.source &&
		left.group == right.group &&
		slices.Equal(left.consumes, right.consumes) &&
		slices.Equal(left.optionalConsumes, right.optionalConsumes) &&
		reflect.DeepEqual(left.activationPaths, right.activationPaths) &&
		left.resourceProjection == right.resourceProjection &&
		left.deriveResource == right.deriveResource &&
		left.recordEvent == right.recordEvent &&
		left.backendPlan == right.backendPlan &&
		left.publishValue == right.publishValue &&
		left.statusPatch == right.statusPatch
}

func (c incrementalCarrier) bindColdQuery(
	batch incremental.ColdExactBatch,
	laneIndex int,
) (incrementalCarrierLane, incremental.ColdExactBatchQuery, error) {
	if laneIndex < 0 || laneIndex >= len(c.lanes) {
		return incrementalCarrierLane{}, incremental.ColdExactBatchQuery{},
			fmt.Errorf("incremental carrier lane index %d is invalid", laneIndex)
	}
	lane := c.lanes[laneIndex]
	if lane.component == nil || lane.batchIndex < 0 || lane.batchIndex >= batch.Len() ||
		!componentQueryKeyMatches(lane.queryKey, lane.component, c.source, c.namespace, c.name) {
		return incrementalCarrierLane{}, incremental.ColdExactBatchQuery{},
			errors.New("incremental carrier lane has invalid query association")
	}
	query := batch.Query(lane.batchIndex)
	if query.Key() != lane.queryKey {
		return incrementalCarrierLane{}, incremental.ColdExactBatchQuery{},
			errors.New("incremental carrier lane query association changed")
	}
	return lane, query, nil
}
