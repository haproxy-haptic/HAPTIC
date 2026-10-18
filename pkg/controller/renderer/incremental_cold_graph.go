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
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/haproxy-haptic/HAPTIC/pkg/incremental"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

type incrementalColdGraphContextKey struct{}

type incrementalColdGraphAuthority struct {
	seal    *incrementalColdGraphAuthority
	session *incrementalRenderSession
	ready   map[string]*incrementalGroupIndex
}

const incrementalColdCarrierBulkGroupItems = incrementalColdVectorItemsPerShard

func newIncrementalColdGraphAuthority(
	session *incrementalRenderSession,
	completed map[string]*incrementalGroupIndex,
) (*incrementalColdGraphAuthority, error) {
	if session == nil || !session.cold {
		return nil, errors.New("incremental cold graph authority has no cold session")
	}
	ready := make(map[string]*incrementalGroupIndex, len(completed))
	for group, index := range completed {
		if group == "" || index == nil || session.groupIndexes[group] != index || !session.groupReady[group] {
			return nil, fmt.Errorf("incremental cold graph group %q is not ready", group)
		}
		if err := index.validateAuthentication(); err != nil {
			return nil, fmt.Errorf("incremental cold graph group %q: %w", group, err)
		}
		ready[group] = index
	}
	authority := &incrementalColdGraphAuthority{session: session, ready: ready}
	authority.seal = authority
	return authority, nil
}

func (a *incrementalColdGraphAuthority) authorizes(
	session *incrementalRenderSession,
	group string,
) bool {
	if a == nil || a.seal != a || a.session != session || session == nil || !session.cold {
		return false
	}
	index, ready := a.ready[group]
	return ready && index != nil && session.groupReady[group] && session.groupIndexes[group] == index
}

func (r *incrementalRenderSession) coldGraphProducerAuthorized(
	ctx context.Context,
	group string,
) bool {
	if ctx == nil {
		return false
	}
	authority, _ := ctx.Value(incrementalColdGraphContextKey{}).(*incrementalColdGraphAuthority)
	return authority.authorizes(r, group)
}

func (r *incrementalRenderSession) prepareColdComponentGraph(ctx context.Context) error {
	if r == nil || r.state == nil || r.graphSession == nil || !r.cold {
		return nil
	}
	groups, keysByGroup, allKeys, err := r.planColdComponentGraphQueries()
	if err != nil {
		return err
	}
	if len(allKeys) == 0 {
		return nil
	}

	renderers, err := r.selectColdComponentGraphRenderers(allKeys)
	if err != nil {
		return err
	}
	if renderers.vectorDisabled {
		r.coldVectorDisabled = true
		return nil
	}
	plan, err := r.planColdComponentCarrierKeys(allKeys)
	if err != nil {
		return fmt.Errorf("planning incremental cold graph: %w", err)
	}
	if plan == nil || plan.logicalQueries != len(allKeys) || len(plan.groupStages) == 0 {
		return errors.New("incremental cold graph planner returned an invalid plan")
	}
	groupOrder, err := r.orderColdComponentGraphGroups(plan, groups)
	if err != nil {
		return err
	}
	if renderers.carrierEligible {
		return r.evaluateColdComponentCarrierPlan(
			ctx, renderers, plan, groupOrder, allKeys, keysByGroup,
		)
	}
	return r.runColdComponentVectorStages(ctx, renderers.vector, groupOrder, keysByGroup)
}

func (r *incrementalRenderSession) planColdComponentGraphQueries() (
	groups []string,
	keysByGroup map[string][]incremental.QueryKey,
	allKeys []incremental.QueryKey,
	err error,
) {
	demandDriven := r.state.resourceProjectionDemandDrivenClosure()
	groups = make([]string, 0, len(r.state.groups))
	for group := range r.state.groups {
		if demandDriven[group] {
			continue
		}
		groups = append(groups, group)
	}
	slices.Sort(groups)
	keysByGroup = make(map[string][]incremental.QueryKey, len(groups))
	allKeys = make([]incremental.QueryKey, 0)
	for _, group := range groups {
		keys, keysErr := r.queriesForGroup(group)
		if keysErr != nil {
			return nil, nil, nil, fmt.Errorf("planning incremental cold group %q: %w", group, keysErr)
		}
		for _, key := range keys {
			pending, pendingErr := r.coldComponentGraphQueryPending(group, key)
			if pendingErr != nil {
				return nil, nil, nil, pendingErr
			}
			if !pending {
				continue
			}
			keysByGroup[group] = append(keysByGroup[group], key)
			allKeys = append(allKeys, key)
		}
	}
	return groups, keysByGroup, allKeys, nil
}

func (r *incrementalRenderSession) coldComponentGraphQueryPending(
	group string,
	key incremental.QueryKey,
) (bool, error) {
	component, source, namespace, name, ok := r.resolveComponentQuery(key)
	if !ok || component.group != group {
		return false, fmt.Errorf("incremental cold group %q has invalid query %s", group, key)
	}
	resultCacheKey := resultKey(&component, source, namespace, name)
	root, cached := r.results.Get(resultCacheKey)
	if !cached {
		return true, nil
	}
	if err := r.verifyGroupIndexResult(
		&component, source, namespace, name, root, true, resultCacheKey,
	); err != nil {
		return false, err
	}
	return false, nil
}

type coldComponentGraphRenderers struct {
	carrier         templating.IncrementalComponentVectorCarrierWavesRenderer
	carrierEligible bool
	vector          templating.IncrementalComponentVectorRenderer
	vectorDisabled  bool
}

func (r *incrementalRenderSession) selectColdComponentGraphRenderers(
	allKeys []incremental.QueryKey,
) (*coldComponentGraphRenderers, error) {
	carrier, carrierAvailable := r.state.engine.(templating.IncrementalComponentVectorCarrierWavesRenderer)
	selected := &coldComponentGraphRenderers{carrier: carrier}
	if carrierAvailable {
		eligible, err := r.preflightColdComponentCarrier(carrier, allKeys)
		if err != nil {
			return nil, err
		}
		selected.carrierEligible = eligible
	}
	if !selected.carrierEligible {
		vector, vectorEligible, err := r.preflightColdComponentVector(allKeys)
		if err != nil {
			return nil, err
		}
		if !vectorEligible {
			selected.vectorDisabled = true
			return selected, nil
		}
		selected.vector = vector
	}
	return selected, nil
}

func (r *incrementalRenderSession) orderColdComponentGraphGroups(
	plan *incrementalCarrierPlan,
	groups []string,
) ([]string, error) {
	expectedGroups := make(map[string]struct{}, len(groups))
	for _, group := range groups {
		expectedGroups[group] = struct{}{}
	}
	seenGroups := make(map[string]struct{}, len(groups))
	groupOrder := make([]string, 0, len(groups))
	for _, stage := range plan.groupStages {
		for _, group := range stage.groups {
			if _, duplicate := seenGroups[group]; duplicate {
				return nil, fmt.Errorf("incremental cold graph repeats group %q", group)
			}
			if _, exists := r.state.groups[group]; !exists {
				return nil, fmt.Errorf("incremental cold graph contains unknown group %q", group)
			}
			if _, expected := expectedGroups[group]; !expected {
				return nil, fmt.Errorf("incremental cold graph contains demand-driven group %q", group)
			}
			seenGroups[group] = struct{}{}
			groupOrder = append(groupOrder, group)
		}
	}
	if len(seenGroups) != len(expectedGroups) {
		return nil, errors.New("incremental cold graph omitted proactive groups")
	}
	return groupOrder, nil
}

func (r *incrementalRenderSession) evaluateColdComponentCarrierPlan(
	ctx context.Context,
	renderers *coldComponentGraphRenderers,
	plan *incrementalCarrierPlan,
	groupOrder []string,
	allKeys []incremental.QueryKey,
	keysByGroup map[string][]incremental.QueryKey,
) error {
	schedule, scheduleErr := newIncrementalColdCarrierGraphSchedule(
		plan,
		groupOrder,
		allKeys,
		keysByGroup,
	)
	if scheduleErr != nil {
		return fmt.Errorf("planning incremental cold carrier graph: %w", scheduleErr)
	}
	return r.evaluateColdComponentCarrierGraphWaves(ctx, renderers.carrier, schedule)
}

func (r *incrementalRenderSession) runColdComponentVectorStages(
	ctx context.Context,
	vector templating.IncrementalComponentVectorRenderer,
	groupOrder []string,
	keysByGroup map[string][]incremental.QueryKey,
) error {
	completed := make(map[string]*incrementalGroupIndex, len(groupOrder))
	pending := make(map[string]struct{}, len(groupOrder))
	for _, group := range groupOrder {
		pending[group] = struct{}{}
	}
	runCtx := context.WithValue(ctx, incrementalRunContextKey{}, r)
	stageNumber := 0
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		readySmall, readyBulk := r.coldGraphReadyPartitions(groupOrder, pending, completed, keysByGroup)
		stageGroups := readySmall
		if len(stageGroups) == 0 {
			stageGroups = readyBulk
		}
		if len(stageGroups) == 0 {
			return errors.New("incremental cold graph has no dependency-ready group")
		}

		authority, err := newIncrementalColdGraphAuthority(r, completed)
		if err != nil {
			return err
		}
		stageCtx := context.WithValue(runCtx, incrementalColdGraphContextKey{}, authority)
		stageKeys := make([]incremental.QueryKey, 0)
		for _, group := range stageGroups {
			stageKeys = append(stageKeys, keysByGroup[group]...)
		}
		results, err := r.evaluateColdComponentVector(stageCtx, vector, stageKeys)
		if err != nil {
			return fmt.Errorf("evaluating incremental cold graph stage %d: %w", stageNumber, err)
		}
		if err := r.applyColdGraphStageResults(stageNumber, stageGroups, results, completed, pending); err != nil {
			return err
		}
		stageNumber++
	}
	return nil
}

func (r *incrementalRenderSession) coldGraphReadyPartitions(
	groupOrder []string,
	pending map[string]struct{},
	completed map[string]*incrementalGroupIndex,
	keysByGroup map[string][]incremental.QueryKey,
) (readySmall, readyBulk []string) {
	readySmall = make([]string, 0)
	readyBulk = make([]string, 0)
	for _, group := range groupOrder {
		if _, waiting := pending[group]; !waiting || !r.coldGraphDependenciesReady(group, completed) {
			continue
		}
		if len(keysByGroup[group]) >= incrementalColdCarrierBulkGroupItems {
			readyBulk = append(readyBulk, group)
		} else {
			readySmall = append(readySmall, group)
		}
	}
	return readySmall, readyBulk
}

func (r *incrementalRenderSession) coldGraphDependenciesReady(
	group string,
	completed map[string]*incrementalGroupIndex,
) bool {
	for _, dependency := range r.state.dependencies[group] {
		if _, configured := r.state.groups[dependency]; !configured {
			continue
		}
		if _, ready := completed[dependency]; !ready {
			return false
		}
	}
	return true
}

func (r *incrementalRenderSession) preflightColdComponentCarrier(
	renderer templating.IncrementalComponentVectorCarrierWavesRenderer,
	keys []incremental.QueryKey,
) (bool, error) {
	if renderer == nil || len(keys) == 0 {
		return false, nil
	}
	eligibility, available := renderer.IncrementalComponentVectorCarrierEligibility()
	if !available {
		return false, nil
	}
	expectedBindings := incrementalColdVectorBindings()
	if !slices.Equal(eligibility.BindingNames, expectedBindings[:]) {
		return false, fmt.Errorf(
			"incremental component carrier declared bindings %v, want %v",
			eligibility.BindingNames,
			expectedBindings,
		)
	}
	templateNames, err := canonicalColdCarrierTemplateNames(eligibility.TemplateNames)
	if err != nil {
		return false, err
	}
	for _, key := range keys {
		eligible, queryErr := r.coldCarrierQueryEligible(key, templateNames)
		if queryErr != nil || !eligible {
			return false, queryErr
		}
	}
	return true, nil
}

func canonicalColdCarrierTemplateNames(names []string) ([]string, error) {
	templateNames := slices.Clone(names)
	if !slices.IsSorted(templateNames) {
		return nil, errors.New("incremental component carrier template names are not canonical")
	}
	for index, name := range templateNames {
		if name == "" || index > 0 && templateNames[index-1] == name {
			return nil, errors.New("incremental component carrier template names are not canonical")
		}
	}
	return templateNames, nil
}

func (r *incrementalRenderSession) coldCarrierQueryEligible(
	key incremental.QueryKey,
	templateNames []string,
) (bool, error) {
	component, source, namespace, name, ok := r.resolveComponentQuery(key)
	if !ok {
		return false, fmt.Errorf(
			"incremental component carrier received non-component query %s",
			key,
		)
	}
	if component.resourceProjection {
		return false, nil
	}
	if _, cached := r.results.Get(resultKey(&component, source, namespace, name)); cached {
		return false, fmt.Errorf("incremental component carrier query %s is already cached", key)
	}
	if _, pending := r.freshResults[key]; pending {
		return false, fmt.Errorf("incremental component carrier query %s already has a pending result", key)
	}
	if _, pending := r.httpExecuted[key]; pending {
		return false, fmt.Errorf("incremental component carrier query %s has pending HTTP effects", key)
	}
	if _, eligible := slices.BinarySearch(templateNames, component.entryPoint); !eligible {
		return false, nil
	}
	return true, nil
}
