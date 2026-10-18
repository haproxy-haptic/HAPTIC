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
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"slices"

	iradix "github.com/hashicorp/go-immutable-radix/v2"
	"golang.org/x/sync/errgroup"

	"github.com/haproxy-haptic/HAPTIC/pkg/incremental"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

func incrementalColdVectorBindings() [10]string {
	return [10]string{
		incrementalControllerContextName,
		incrementalHTTPContextName,
		incrementalItemContextName,
		incrementalPlanRegistryContextName,
		incrementalPropsContextName,
		incrementalRenderModeContextName,
		incrementalRenderSubjectContextName,
		incrementalResourcesContextName,
		incrementalSharedContextName,
		incrementalSourceContextName,
	}
}

type incrementalColdVectorGroup struct {
	entryPoint string
	indexes    []int
	prepared   []*preparedIncrementalComponent
	vector     *preparedIncrementalVectorRender
}

type incrementalColdVectorResult struct {
	finalized []*finalizedIncrementalComponent
	roots     []incremental.ExactValueRoot
}

const incrementalColdVectorItemsPerShard = 100

func (r *incrementalRenderSession) selectFreshColdStart() (bool, error) {
	if r == nil || r.state == nil || r.state.graph == nil || r.base == nil {
		return false, errors.New("incremental cold-start state is unavailable")
	}
	// Pending bytes must execute every consumer and never reuse accepted output.
	if r.pendingHTTPOverlay {
		r.coldReason = "pending-http-overlay"
		return true, nil
	}
	if r.cacheBaseUnavailable {
		r.coldReason = "cache-build-still-pending"
		return true, nil
	}
	if r.state.graph.Generation() != 0 {
		return false, nil
	}
	r.coldReason = "first-render"
	if err := r.validateFreshColdStartSnapshot(); err != nil {
		return false, fmt.Errorf("incremental zero-generation snapshot is not empty: %w", err)
	}
	return true, nil
}

func incrementalSnapshotHasCachedValues(snapshot *incrementalStateSnapshot) bool {
	return len(snapshot.cursors) != 0 || snapshot.httpCursor.token.Valid() ||
		snapshot.bindings.Len() != 0 || snapshot.members.Len() != 0 ||
		snapshot.retired.Len() != 0 || snapshot.results.Len() != 0 ||
		snapshot.derived.Len() != 0 || snapshot.httpEffects.Len() != 0 ||
		snapshot.catalog.Len() != 0 ||
		len(snapshot.groupReady) != 0 || snapshot.bindingCache != nil
}

func (r *incrementalRenderSession) validateFreshColdStartSnapshot() error {
	snapshot := r.base
	if !snapshot.catalog.valid() {
		return errors.New("incremental resource catalog snapshot has invalid provenance")
	}
	if incrementalSnapshotHasCachedValues(snapshot) {
		return errors.New("incremental snapshot has cached values")
	}
	if err := snapshot.activeGroups.validateAuthentication(); err != nil {
		return err
	}
	if snapshot.activeGroups.instances.Len() != 0 {
		return errors.New("incremental snapshot has active groups")
	}
	if len(snapshot.groupIndexes) != len(r.state.groups) {
		return errors.New("incremental snapshot has an invalid group set")
	}
	for group := range r.state.groups {
		index := snapshot.groupIndexes[group]
		empty, err := index.authenticatedStructurallyEmpty()
		if err != nil {
			return fmt.Errorf("incremental group %q: %w", group, err)
		}
		if !empty {
			return fmt.Errorf("incremental group %q has cached values", group)
		}
	}
	if err := validateEmptyIncrementalPreparedPlan(snapshot.preparedPlan, snapshot.results.Root()); err != nil {
		return err
	}
	if err := validateIncrementalStatusPatchPlanAuthentication(snapshot); err != nil {
		return err
	}
	replay, err := snapshot.statusPlan.PrepareReplay()
	if err != nil {
		return err
	}
	if !replay.Empty() {
		return errors.New("incremental snapshot has status patches")
	}
	return nil
}

func validateEmptyIncrementalPreparedPlan(
	plan *incrementalPreparedPlan,
	resultRoot *iradix.Node[incremental.ExactValueRoot],
) error {
	if plan == nil {
		return errors.New("incremental prepared plan is unavailable")
	}
	if err := plan.validateAuthentication(resultRoot); err != nil {
		return err
	}
	if plan.instances.Len() != 0 || plan.calls.Len() != 0 || plan.backendCandidates.Len() != 0 ||
		plan.profileCandidates.Len() != 0 || plan.profileVariants.Len() != 0 ||
		plan.standaloneProfiles.Len() != 0 || plan.conditions.Len() != 0 ||
		plan.requirements.Len() != 0 || plan.missingProfiles.Len() != 0 ||
		plan.conflictingProfiles.Len() != 0 || plan.outputs.Len() != 0 {
		return errors.New("incremental prepared plan has cached values")
	}
	return nil
}

func (r *incrementalRenderSession) preflightColdVectorQuery(
	key incremental.QueryKey,
	entryPoints map[string]struct{},
) (projection, cached bool, err error) {
	component, source, namespace, name, ok := r.resolveComponentQuery(key)
	if !ok {
		return false, false, fmt.Errorf(
			"incremental component evaluation received non-component query %s",
			key,
		)
	}
	if component.resourceProjection {
		return true, false, nil
	}
	resultCacheKey := resultKey(&component, source, namespace, name)
	root, found := r.results.Get(resultCacheKey)
	if found {
		if err := r.verifyGroupIndexResult(
			&component, source, namespace, name, root, true, resultCacheKey,
		); err != nil {
			return false, false, err
		}
	}
	if _, pending := r.freshResults[key]; pending {
		return false, false, fmt.Errorf(
			"incremental component query %s already has a pending result",
			key,
		)
	}
	if _, pending := r.httpExecuted[key]; pending {
		return false, false, fmt.Errorf(
			"incremental component query %s already has pending HTTP effects",
			key,
		)
	}
	entryPoints[component.entryPoint] = struct{}{}
	return false, found, nil
}

func (r *incrementalRenderSession) preflightColdComponentVector(
	keys []incremental.QueryKey,
) (templating.IncrementalComponentVectorRenderer, bool, error) {
	renderer, available := r.state.engine.(templating.IncrementalComponentVectorRenderer)
	if !available {
		return nil, false, nil
	}
	entryPoints := make(map[string]struct{})
	allFresh := true
	for _, key := range keys {
		projection, cached, err := r.preflightColdVectorQuery(key, entryPoints)
		if err != nil {
			return nil, false, err
		}
		if projection {
			return renderer, false, nil
		}
		if cached {
			allFresh = false
		}
	}
	ordered := make([]string, 0, len(entryPoints))
	for entryPoint := range entryPoints {
		ordered = append(ordered, entryPoint)
	}
	slices.Sort(ordered)
	allEligible := true
	expectedBindings := incrementalColdVectorBindings()
	for _, entryPoint := range ordered {
		eligibility, eligible := renderer.IncrementalComponentVectorEligibility(entryPoint)
		if !eligible {
			allEligible = false
			continue
		}
		if !slices.Equal(eligibility.BindingNames, expectedBindings[:]) {
			return nil, false, fmt.Errorf(
				"incremental component vector %q declared bindings %v, want %v",
				entryPoint,
				eligibility.BindingNames,
				expectedBindings,
			)
		}
	}
	return renderer, allEligible && allFresh, nil
}

func (r *incrementalRenderSession) evaluateColdComponentVector(
	ctx context.Context,
	renderer templating.IncrementalComponentVectorRenderer,
	keys []incremental.QueryKey,
) ([]incremental.ExactResult, error) {
	// Nothing to evaluate is a legitimate stage, not a malformed one: a
	// dependency-ready group can hold no instances, which is what a watched
	// kind with no objects in the cluster looks like. EvaluateAllColdExactBatch
	// returns an empty set without running the batch callback in that case, so
	// `pending` would stay nil and the checks below would reject it.
	if len(keys) == 0 {
		return nil, nil
	}
	var pending *incrementalColdVectorResult
	results, err := r.graphSession.EvaluateAllColdExactBatch(
		ctx,
		func(runCtx context.Context, batch incremental.ColdExactBatch) error {
			value, executeErr := r.executeColdComponentVector(runCtx, renderer, batch)
			if executeErr != nil {
				return executeErr
			}
			pending = value
			return nil
		},
		keys...,
	)
	if err != nil {
		return nil, err
	}
	// Name which of the three shapes failed. They have different causes — the
	// batch callback never running at all is a different bug from it running
	// and producing the wrong count — and the render only fails this way
	// against a live cluster, where a bare sentence costs a full CI round-trip
	// to narrow.
	if pending == nil {
		return nil, fmt.Errorf(
			"incremental cold component vector produced no result: the batch callback did not run for %d key(s)",
			len(results))
	}
	if len(pending.finalized) != len(results) || len(pending.roots) != len(results) {
		return nil, fmt.Errorf(
			"incremental cold component vector returned an invalid result set: %d finalized and %d root(s) for %d result(s)",
			len(pending.finalized), len(pending.roots), len(results))
	}
	for index := range results {
		if results[index].Key != pending.finalized[index].key {
			return nil, errors.New("incremental cold component vector returned results out of order")
		}
		same, sameErr := results[index].Value.SameRoot(pending.roots[index])
		if sameErr != nil || !same {
			return nil, errors.New("incremental cold component vector returned a different exact root")
		}
	}
	if cause := context.Cause(ctx); cause != nil {
		return nil, cause
	}
	if err := r.installFinalizedComponents(pending.finalized...); err != nil {
		return nil, fmt.Errorf("installing incremental cold component vector results: %w", err)
	}
	return results, nil
}

func (r *incrementalRenderSession) executeColdComponentVector(
	ctx context.Context,
	renderer templating.IncrementalComponentVectorRenderer,
	batch incremental.ColdExactBatch,
) (*incrementalColdVectorResult, error) {
	if renderer == nil || batch.Len() == 0 {
		return nil, errors.New("incremental cold component vector is incomplete")
	}
	originalHTTPExecuted := r.httpExecuted
	stagedHTTPExecuted := make(map[incremental.QueryKey][]incrementalHTTPEffect)
	r.httpExecuted = stagedHTTPExecuted
	defer func() {
		r.httpExecuted = originalHTTPExecuted
	}()

	finalized := make([]*finalizedIncrementalComponent, batch.Len())
	groups, err := r.collectColdVectorGroups(ctx, batch, stagedHTTPExecuted, finalized)
	if err != nil {
		return nil, err
	}
	if len(stagedHTTPExecuted) != 0 {
		return nil, errors.New("incremental cold component vector left unclaimed execution records")
	}
	groups = shardIncrementalColdVectorGroups(groups)

	if err := r.prepareColdVectorGroups(ctx, groups); err != nil {
		return nil, err
	}
	if err := renderColdVectorGroups(ctx, renderer, batch, groups); err != nil {
		return nil, err
	}
	if err := r.finalizeColdVectorGroups(groups, finalized); err != nil {
		return nil, err
	}

	roots, err := r.completeColdVectorRoots(batch, finalized)
	if err != nil {
		return nil, err
	}
	return &incrementalColdVectorResult{finalized: finalized, roots: roots}, nil
}

func (r *incrementalRenderSession) prepareColdVectorGroups(
	ctx context.Context,
	groups []incrementalColdVectorGroup,
) error {
	for index := range groups {
		group := &groups[index]
		vector, err := r.prepareComponentVectorRender(ctx, group.prepared[0].component, group.prepared)
		if err != nil {
			return fmt.Errorf("preparing incremental component vector %q: %w", group.entryPoint, err)
		}
		if err := validatePreparedColdComponentVector(vector, len(group.prepared)); err != nil {
			return fmt.Errorf("preparing incremental component vector %q: %w", group.entryPoint, err)
		}
		group.vector = vector
	}
	return nil
}

func renderColdVectorGroups(
	ctx context.Context,
	renderer templating.IncrementalComponentVectorRenderer,
	batch incremental.ColdExactBatch,
	groups []incrementalColdVectorGroup,
) error {
	renderGroup, renderCtx := errgroup.WithContext(ctx)
	for index := range groups {
		group := &groups[index]
		renderGroup.Go(func() error {
			err := renderer.RenderIncrementalComponentVector(
				renderCtx,
				group.entryPoint,
				templating.IncrementalComponentVectorInput{
					Count:         len(group.prepared),
					SharedContext: group.vector.fixed,
					Bindings:      group.vector.columns,
					Contexts:      group.vector.contexts,
					Lifecycle:     group.vector.execution,
				},
			)
			if err == nil {
				return nil
			}
			var itemErr *templating.IncrementalComponentBatchError
			if errors.As(err, &itemErr) && itemErr.Index >= 0 && itemErr.Index < len(group.indexes) {
				query := batch.Query(group.indexes[itemErr.Index])
				return attributeToWatchedObject(query.Key(), remapIncrementalTemplateError(
					group.prepared[itemErr.Index].component.name,
					group.entryPoint,
					itemErr.Err,
				))
			}
			return remapIncrementalTemplateError(
				group.prepared[0].component.name,
				group.entryPoint,
				err,
			)
		})
	}
	return renderGroup.Wait()
}

func (r *incrementalRenderSession) finalizeColdVectorGroups(
	groups []incrementalColdVectorGroup,
	finalized []*finalizedIncrementalComponent,
) error {
	for index := range groups {
		group := &groups[index]
		_, groupFinalized, err := r.finalizeComponentVectorRender(group.vector)
		if err != nil {
			return fmt.Errorf("finishing incremental component vector %q: %w", group.entryPoint, err)
		}
		if len(groupFinalized) != len(group.indexes) {
			return fmt.Errorf(
				"incremental component vector %q finalized %d results for %d items",
				group.entryPoint,
				len(groupFinalized),
				len(group.indexes),
			)
		}
		for itemIndex, queryIndex := range group.indexes {
			finalized[queryIndex] = groupFinalized[itemIndex]
		}
	}
	return nil
}

func (r *incrementalRenderSession) collectColdVectorGroups(
	ctx context.Context,
	batch incremental.ColdExactBatch,
	stagedHTTPExecuted map[incremental.QueryKey][]incrementalHTTPEffect,
	finalized []*finalizedIncrementalComponent,
) ([]incrementalColdVectorGroup, error) {
	groups := make([]incrementalColdVectorGroup, 0)
	groupByEntryPoint := make(map[string]int)
	for index := 0; index < batch.Len(); index++ {
		query := batch.Query(index)
		component, source, namespace, name, parsed := r.resolveComponentQuery(query.Key())
		if !parsed {
			return nil, fmt.Errorf(
				"incremental cold component vector received non-component query %s",
				query.Key(),
			)
		}
		prepared, immediate, err := r.prepareComponentInputs(
			ctx,
			query,
			&component,
			source,
			namespace,
			name,
		)
		if err != nil {
			return nil, attributeToWatchedObject(query.Key(), err)
		}
		if prepared == nil {
			effects, executed := stagedHTTPExecuted[query.Key()]
			if !executed {
				return nil, fmt.Errorf(
					"incremental cold component vector query %s has no execution record",
					query.Key(),
				)
			}
			delete(stagedHTTPExecuted, query.Key())
			finalized[index], err = finalizeImmediateColdComponent(query.Key(), immediate, effects)
			if err != nil {
				return nil, attributeToWatchedObject(query.Key(), err)
			}
			continue
		}
		groupIndex, exists := groupByEntryPoint[component.entryPoint]
		if !exists {
			groupIndex = len(groups)
			groupByEntryPoint[component.entryPoint] = groupIndex
			groups = append(groups, incrementalColdVectorGroup{entryPoint: component.entryPoint})
		}
		group := &groups[groupIndex]
		if len(group.prepared) > 0 && group.prepared[0].component.name != component.name {
			return nil, fmt.Errorf(
				"incremental component vector entry point %q belongs to multiple components",
				component.entryPoint,
			)
		}
		group.indexes = append(group.indexes, index)
		group.prepared = append(group.prepared, prepared)
	}
	return groups, nil
}

func (r *incrementalRenderSession) completeColdVectorRoots(
	batch incremental.ColdExactBatch,
	finalized []*finalizedIncrementalComponent,
) ([]incremental.ExactValueRoot, error) {
	roots := make([]incremental.ExactValueRoot, batch.Len())
	for index := 0; index < batch.Len(); index++ {
		query := batch.Query(index)
		item := finalized[index]
		if item == nil || item.key != query.Key() {
			return nil, fmt.Errorf(
				"incremental cold component vector query %s has an invalid finalized result",
				query.Key(),
			)
		}
		root, err := query.Complete(item.encoded)
		if err != nil {
			return nil, fmt.Errorf(
				"completing incremental cold component vector query %s: %w",
				query.Key(),
				err,
			)
		}
		if err := r.authenticateColdFinalizedComponent(item, root); err != nil {
			return nil, fmt.Errorf(
				"authenticating incremental cold component vector query %s: %w",
				query.Key(),
				err,
			)
		}
		roots[index] = root
	}
	return roots, nil
}

func shardIncrementalColdVectorGroups(groups []incrementalColdVectorGroup) []incrementalColdVectorGroup {
	return shardIncrementalColdVectorGroupsWithLimit(groups, max(runtime.GOMAXPROCS(0)*2, 1))
}

func shardIncrementalColdVectorGroupsWithLimit(
	groups []incrementalColdVectorGroup,
	maxShards int,
) []incrementalColdVectorGroup {
	maxShards = max(maxShards, 1)
	sharded := make([]incrementalColdVectorGroup, 0, len(groups))
	for index := range groups {
		group := &groups[index]
		shardCount := len(group.prepared) / incrementalColdVectorItemsPerShard
		shardCount = min(max(shardCount, 1), maxShards)
		shardSize := (len(group.prepared) + shardCount - 1) / shardCount
		for start := 0; start < len(group.prepared); start += shardSize {
			end := min(start+shardSize, len(group.prepared))
			sharded = append(sharded, incrementalColdVectorGroup{
				entryPoint: group.entryPoint,
				indexes:    group.indexes[start:end],
				prepared:   group.prepared[start:end],
			})
		}
	}
	return sharded
}

func validatePreparedColdComponentVector(
	vector *preparedIncrementalVectorRender,
	count int,
) error {
	if vector == nil || vector.execution == nil || count <= 0 || len(vector.contexts) != count {
		return errors.New("incremental component vector has an invalid execution shape")
	}
	if len(vector.fixed) != 0 {
		return errors.New("incremental component vector has unexpected shared bindings")
	}
	expectedBindings := incrementalColdVectorBindings()
	if len(vector.columns) != len(expectedBindings) {
		return errors.New("incremental component vector has an invalid binding count")
	}
	for _, name := range expectedBindings {
		if _, exists := vector.columns[name]; !exists {
			return fmt.Errorf("incremental component vector is missing binding %q", name)
		}
	}
	return nil
}

func finalizeImmediateColdComponent(
	key incremental.QueryKey,
	encoded []byte,
	httpEffects []incrementalHTTPEffect,
) (*finalizedIncrementalComponent, error) {
	var result incrementalComponentResult
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, fmt.Errorf("decoding immediate incremental component result: %w", err)
	}
	immutable, fresh, err := newAuthenticatedFreshComponentResult(key, &result)
	if err != nil {
		return nil, fmt.Errorf("encoding immediate incremental component result: %w", err)
	}
	if !stringBytesEqual(immutable, encoded) {
		return nil, errors.New("immediate incremental component result is not canonical")
	}
	return &finalizedIncrementalComponent{
		key:         key,
		encoded:     immutable,
		fresh:       fresh,
		httpEffects: cloneHTTPEffects(httpEffects),
	}, nil
}

func (r *incrementalRenderSession) authenticateColdFinalizedComponent(
	finalized *finalizedIncrementalComponent,
	root incremental.ExactValueRoot,
) error {
	if r == nil || r.state == nil || r.state.graph == nil || finalized == nil ||
		finalized.fresh == nil || finalized.fresh.encoded != finalized.encoded {
		return errors.New("finalized incremental component has invalid provenance")
	}
	return r.authenticateColdFreshComponent(finalized.key, finalized.encoded, finalized.fresh, root)
}

func (r *incrementalRenderSession) authenticateColdFreshComponent(
	key incremental.QueryKey,
	encoded string,
	fresh *authenticatedFreshComponentResult,
	root incremental.ExactValueRoot,
) error {
	if r == nil || r.state == nil || r.state.graph == nil || fresh == nil || fresh.encoded != encoded {
		return errors.New("fresh incremental component has invalid provenance")
	}
	if err := r.state.graph.ValidateExactValue(key, root); err != nil {
		return err
	}
	return bindAuthenticatedFreshComponentResult(fresh, key, root)
}
