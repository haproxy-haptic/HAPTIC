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
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/helpers"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

type incrementalBindingPlan struct {
	bindings          []incrementalBinding
	byComponent       map[string][]incrementalBinding
	bySource          map[string][]incrementalComponent
	projectionSources map[string]struct{}
	props             map[string][]byte
	owners            map[string]incrementalComponent
}

type incrementalBindingCache struct {
	inputs *templating.IncrementalBindingInputSnapshot
	plan   *incrementalBindingPlan
}

func (s *incrementalRenderState) planBindings(
	ctx context.Context,
	baseContext map[string]any,
) (*incrementalBindingPlan, error) {
	plan, _, _, err := s.prepareBindingPlan(ctx, baseContext)
	return plan, err
}

func (s *incrementalRenderState) prepareBindingPlan(
	ctx context.Context,
	baseContext map[string]any,
) (*incrementalBindingPlan, *incrementalBindingCache, bool, error) {
	if len(s.bindingEntryPoints) == 0 {
		return s.staticBindingPlan, nil, true, nil
	}
	snapshotPlanner, authenticated := s.planner.(templating.IncrementalBindingSnapshotPlanner)
	if !authenticated {
		plan := cloneIncrementalBindingPlan(s.staticBindingPlan)
		plan, err := s.planDynamicBindings(ctx, baseContext, plan)
		return plan, nil, false, err
	}
	if s.snapshot != nil && s.snapshot.bindingCache != nil && snapshotPlanner.MatchIncrementalBindingInputs(
		s.bindingEntryPoints,
		baseContext,
		s.snapshot.bindingCache.inputs,
	) {
		cache := s.snapshot.bindingCache
		return cache.plan, cache, true, nil
	}
	inputs, err := snapshotPlanner.SnapshotIncrementalBindingInputs(s.bindingEntryPoints, baseContext)
	if err != nil {
		return nil, nil, false, err
	}
	plan := cloneIncrementalBindingPlan(s.staticBindingPlan)
	for _, name := range s.dynamicComponents {
		component := s.components[name]
		bindings, err := s.planComponentBindingsSnapshot(ctx, &component, snapshotPlanner, inputs)
		if err != nil {
			return nil, nil, false, err
		}
		if err := plan.addComponentBindings(&component, bindings); err != nil {
			return nil, nil, false, err
		}
	}
	plan.sort()
	cache := &incrementalBindingCache{inputs: inputs, plan: cloneIncrementalBindingPlan(plan)}
	return plan, cache, true, nil
}

func (s *incrementalRenderState) planDynamicBindings(
	ctx context.Context,
	baseContext map[string]any,
	plan *incrementalBindingPlan,
) (*incrementalBindingPlan, error) {
	for _, name := range s.dynamicComponents {
		component := s.components[name]
		bindings, err := s.planComponentBindings(ctx, &component, baseContext)
		if err != nil {
			return nil, err
		}
		if err := plan.addComponentBindings(&component, bindings); err != nil {
			return nil, err
		}
	}
	plan.sort()
	return plan, nil
}

func (s *incrementalRenderState) planComponentBindingsSnapshot(
	ctx context.Context,
	component *incrementalComponent,
	planner templating.IncrementalBindingSnapshotPlanner,
	inputs *templating.IncrementalBindingInputSnapshot,
) ([]incrementalBinding, error) {
	entryPoint := helpers.IncrementalBindingsEntryPointName(component.name)
	encoded, err := planner.RenderIncrementalBindingsSnapshot(ctx, entryPoint, inputs)
	if err != nil {
		return nil, fmt.Errorf("planning incremental component %q bindings: %w", component.name,
			remapIncrementalTemplateError(component.name, entryPoint, err))
	}
	bindings, err := decodeIncrementalBindings(component.name, encoded, s.config.WatchedResources)
	if err != nil {
		return nil, fmt.Errorf("planning incremental component %q bindings: %w", component.name, err)
	}
	if err := s.checkProjectionScopes(component, bindings); err != nil {
		return nil, fmt.Errorf("planning incremental component %q bindings: %w", component.name, err)
	}
	return bindings, nil
}

// checkProjectionScopes applies the watched-namespace scope to resource
// projections, which read the store by key without a template lookup.
func (s *incrementalRenderState) checkProjectionScopes(component *incrementalComponent, bindings []incrementalBinding) error {
	if s.scopeFor == nil || !component.resourceProjection {
		return nil
	}
	for _, binding := range bindings {
		projection, err := decodeIncrementalResourceProjection(binding.props)
		if err != nil {
			return err
		}
		if err := s.scopeFor(binding.source).Check(projection.Keys); err != nil {
			return err
		}
	}
	return nil
}

func newIncrementalBindingPlan() *incrementalBindingPlan {
	return &incrementalBindingPlan{
		byComponent:       map[string][]incrementalBinding{},
		bySource:          map[string][]incrementalComponent{},
		projectionSources: map[string]struct{}{},
		props:             map[string][]byte{},
		owners:            map[string]incrementalComponent{},
	}
}

func cloneIncrementalBindingPlan(plan *incrementalBindingPlan) *incrementalBindingPlan {
	if plan == nil {
		return newIncrementalBindingPlan()
	}
	cloned := newIncrementalBindingPlan()
	cloned.bindings = cloneIncrementalBindings(plan.bindings)
	for component, bindings := range plan.byComponent {
		cloned.byComponent[component] = cloneIncrementalBindings(bindings)
	}
	for source, components := range plan.bySource {
		clonedComponents := make([]incrementalComponent, len(components))
		for index := range components {
			clonedComponents[index] = cloneIncrementalCarrierComponent(&components[index])
		}
		cloned.bySource[source] = clonedComponents
	}
	for source := range plan.projectionSources {
		cloned.projectionSources[source] = struct{}{}
	}
	for key, props := range plan.props {
		cloned.props[key] = slices.Clone(props)
	}
	for source := range plan.owners {
		owner := plan.owners[source]
		cloned.owners[source] = cloneIncrementalCarrierComponent(&owner)
	}
	return cloned
}

// equalIncrementalBindingPlan is reflect.DeepEqual over two binding plans,
// typed: the commit-time tamper check runs it on every render.
func equalIncrementalBindingPlan(left, right *incrementalBindingPlan) bool {
	if left == nil || right == nil {
		return left == right
	}
	return equalIncrementalBindings(left.bindings, right.bindings) &&
		equalMapsFunc(left.byComponent, right.byComponent, equalIncrementalBindings) &&
		equalMapsFunc(left.bySource, right.bySource, equalIncrementalComponents) &&
		equalMapsFunc(left.projectionSources, right.projectionSources, func(struct{}, struct{}) bool { return true }) &&
		equalMapsFunc(left.props, right.props, equalBytes) &&
		equalMapsFunc(left.owners, right.owners, func(left, right incrementalComponent) bool {
			return equalIncrementalComponent(&left, &right)
		})
}

func equalMapsFunc[V any](left, right map[string]V, equal func(V, V) bool) bool {
	if (left == nil) != (right == nil) || len(left) != len(right) {
		return false
	}
	for key, leftValue := range left {
		rightValue, exists := right[key]
		if !exists || !equal(leftValue, rightValue) {
			return false
		}
	}
	return true
}

func equalIncrementalBindings(left, right []incrementalBinding) bool {
	if (left == nil) != (right == nil) || len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].component != right[index].component || left[index].source != right[index].source ||
			!equalBytes(left[index].props, right[index].props) ||
			!equalIncrementalResourceProjection(left[index].projection, right[index].projection) {
			return false
		}
	}
	return true
}

func equalIncrementalResourceProjection(left, right *incrementalResourceProjection) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.Cell == right.Cell && left.Key == right.Key && left.Rank == right.Rank &&
		left.digest == right.digest && left.identity == right.identity &&
		equalStrings(left.Keys, right.Keys)
}

func equalIncrementalComponents(left, right []incrementalComponent) bool {
	if (left == nil) != (right == nil) || len(left) != len(right) {
		return false
	}
	for index := range left {
		if !equalIncrementalComponent(&left[index], &right[index]) {
			return false
		}
	}
	return true
}

func equalIncrementalComponent(left, right *incrementalComponent) bool {
	return left.name == right.name && left.entryPoint == right.entryPoint &&
		left.source == right.source && left.root == right.root && left.group == right.group &&
		equalStrings(left.consumes, right.consumes) &&
		equalStrings(left.optionalConsumes, right.optionalConsumes) &&
		equalExistenceJSONPaths(left.activationPaths, right.activationPaths) &&
		left.resourceProjection == right.resourceProjection &&
		left.deriveResource == right.deriveResource &&
		left.recordEvent == right.recordEvent &&
		left.backendPlan == right.backendPlan &&
		left.publishValue == right.publishValue &&
		left.statusPatch == right.statusPatch
}

func equalExistenceJSONPaths(left, right []templating.ExistenceJSONPath) bool {
	if (left == nil) != (right == nil) || len(left) != len(right) {
		return false
	}
	for index := range left {
		if !left[index].Equal(right[index]) {
			return false
		}
	}
	return true
}

func equalStrings(left, right []string) bool {
	return (left == nil) == (right == nil) && slices.Equal(left, right)
}

func equalBytes(left, right []byte) bool {
	return (left == nil) == (right == nil) && bytes.Equal(left, right)
}

func cloneIncrementalBindings(bindings []incrementalBinding) []incrementalBinding {
	cloned := make([]incrementalBinding, len(bindings))
	for index, binding := range bindings {
		binding.props = slices.Clone(binding.props)
		binding.projection = cloneIncrementalResourceProjection(binding.projection)
		cloned[index] = binding
	}
	return cloned
}

func sortedComponentNames(components map[string]incrementalComponent) []string {
	names := make([]string, 0, len(components))
	for name := range components {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (s *incrementalRenderState) planComponentBindings(
	ctx context.Context,
	component *incrementalComponent,
	bindingContext map[string]any,
) ([]incrementalBinding, error) {
	if component.source != "" {
		return []incrementalBinding{staticIncrementalBinding(component.name, component.source)}, nil
	}
	if s.planner == nil {
		return nil, errors.New("template engine has no incremental binding planner executor")
	}
	entryPoint := helpers.IncrementalBindingsEntryPointName(component.name)
	encoded, err := s.planner.RenderIncrementalBindings(ctx, entryPoint, bindingContext)
	if err != nil {
		return nil, fmt.Errorf("planning incremental component %q bindings: %w", component.name,
			remapIncrementalTemplateError(component.name, entryPoint, err))
	}
	bindings, err := decodeIncrementalBindings(component.name, encoded, s.config.WatchedResources)
	if err != nil {
		return nil, fmt.Errorf("planning incremental component %q bindings: %w", component.name, err)
	}
	if err := s.checkProjectionScopes(component, bindings); err != nil {
		return nil, fmt.Errorf("planning incremental component %q bindings: %w", component.name, err)
	}
	return bindings, nil
}

func (p *incrementalBindingPlan) addComponentBindings(
	component *incrementalComponent,
	bindings []incrementalBinding,
) error {
	for _, binding := range bindings {
		key := string(bindingKey(binding.component, binding.source))
		if _, duplicate := p.props[key]; duplicate {
			return fmt.Errorf("incremental component %q repeats binding %q", component.name, binding.source)
		}
		binding.props = slices.Clone(binding.props)
		if component.resourceProjection {
			projection, err := decodeIncrementalResourceProjection(binding.props)
			if err != nil {
				return fmt.Errorf(
					"incremental component %q binding %q: %w",
					component.name,
					binding.source,
					err,
				)
			}
			binding.projection = projection
			p.projectionSources[binding.source] = struct{}{}
		}
		p.bindings = append(p.bindings, binding)
		p.byComponent[component.name] = append(p.byComponent[component.name], binding)
		if !component.resourceProjection {
			p.bySource[binding.source] = append(p.bySource[binding.source], *component)
		}
		p.props[key] = slices.Clone(binding.props)
		if !component.deriveResource {
			continue
		}
		if owner, exists := p.owners[binding.source]; exists && owner.name != component.name {
			return fmt.Errorf(
				"watched resource %q has multiple active deriveResource components %q and %q",
				binding.source,
				owner.name,
				component.name,
			)
		}
		p.owners[binding.source] = *component
	}
	return nil
}

func (p *incrementalBindingPlan) sort() {
	slices.SortFunc(p.bindings, compareIncrementalBindings)
	for name := range p.byComponent {
		slices.SortFunc(p.byComponent[name], compareIncrementalBindings)
	}
	for source := range p.bySource {
		slices.SortFunc(p.bySource[source], func(left, right incrementalComponent) int {
			return strings.Compare(left.name, right.name)
		})
	}
}

func compareIncrementalBindings(left, right incrementalBinding) int {
	if compared := strings.Compare(left.component, right.component); compared != 0 {
		return compared
	}
	return strings.Compare(left.source, right.source)
}

func sameIncrementalBindingPlans(left, right *incrementalBindingPlan) bool {
	if left == nil || right == nil || len(left.bindings) != len(right.bindings) {
		return left == right
	}
	for index := range left.bindings {
		leftBinding := &left.bindings[index]
		rightBinding := &right.bindings[index]
		if leftBinding.component != rightBinding.component || leftBinding.source != rightBinding.source ||
			!bytes.Equal(leftBinding.props, rightBinding.props) {
			return false
		}
	}
	return true
}

func (p *incrementalBindingPlan) required(base map[string]struct{}) map[string]struct{} {
	required := make(map[string]struct{}, len(base)+len(p.bySource)+len(p.projectionSources))
	for source := range base {
		required[source] = struct{}{}
	}
	for source := range p.bySource {
		required[source] = struct{}{}
	}
	for source := range p.projectionSources {
		required[source] = struct{}{}
	}
	return required
}
