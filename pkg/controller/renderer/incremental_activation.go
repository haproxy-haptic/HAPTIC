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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/haproxy-haptic/HAPTIC/pkg/incremental"
)

func (r *incrementalRenderSession) executeActivationPredicate(
	ctx context.Context,
	reader incremental.Reader,
	source, namespace, name string,
) ([]byte, error) {
	bound, err := r.boundActivationComponents(reader, source)
	if err != nil {
		return nil, err
	}
	if len(bound) == 0 {
		return encodeActivationSignature(nil)
	}

	itemBytes, found, err := reader.Input(resourceInputKey(&resourceInputSpec{
		resourceType: source,
		scope:        resourceInputIdentity,
		namespace:    namespace,
		name:         name,
	}))
	if err != nil || !found {
		return nil, err
	}
	item, err := decodeIncrementalComponentObject("activation", "source", itemBytes)
	if err != nil {
		return nil, err
	}
	item, _, err = r.projectActivationItem(ctx, reader, source, item, itemBytes)
	if err != nil {
		return nil, fmt.Errorf("projecting incremental activation item for %q: %w", source, err)
	}

	active, err := activeActivationComponents(bound, item)
	if err != nil {
		return nil, err
	}
	return encodeActivationSignature(active)
}

func (r *incrementalRenderSession) boundActivationComponents(
	reader incremental.Reader,
	source string,
) ([]*incrementalComponent, error) {
	bound := make([]*incrementalComponent, 0, len(r.state.activations[source]))
	for index := range r.state.activations[source] {
		component := &r.state.activations[source][index]
		props, found, err := reader.Input(bindingInputKey(component.name, source))
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		expected, exists := r.bindingPlan.props[string(bindingKey(component.name, source))]
		if !exists || !bytes.Equal(props, expected) {
			return nil, fmt.Errorf("incremental activation binding %q for %q does not match its plan",
				component.name, source)
		}
		bound = append(bound, component)
	}
	return bound, nil
}

func activeActivationComponents(
	bound []*incrementalComponent,
	item map[string]any,
) ([]string, error) {
	active := make([]string, 0, len(bound))
	for _, component := range bound {
		for _, path := range component.activationPaths {
			exists, pathErr := path.Exists(item)
			if pathErr != nil {
				return nil, fmt.Errorf("evaluating incremental component %q activation path: %w",
					component.name, pathErr)
			}
			if exists {
				active = append(active, component.name)
				break
			}
		}
	}
	return active, nil
}

func encodeActivationSignature(active []string) ([]byte, error) {
	if active == nil {
		active = []string{}
	}
	return json.Marshal(active)
}

func decodeActivationSignature(encoded []byte) ([]string, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var active []string
	if err := decoder.Decode(&active); err != nil {
		return nil, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	if active == nil || !slices.IsSorted(active) {
		return nil, fmt.Errorf("incremental activation signature is not canonical")
	}
	for index, component := range active {
		if component == "" || index > 0 && active[index-1] == component {
			return nil, fmt.Errorf("incremental activation signature is not canonical")
		}
	}
	canonical, err := json.Marshal(active)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(canonical, encoded) {
		return nil, fmt.Errorf("incremental activation signature is not canonical")
	}
	return active, nil
}

func (r *incrementalRenderSession) prepareActivationStage(ctx context.Context) error {
	keys := r.pendingActivationQueries()
	if len(keys) == 0 {
		return nil
	}
	previous := make(map[incremental.QueryKey][]string, len(keys))
	for _, key := range keys {
		encoded, found := r.baseActivationValue(key)
		if !found {
			continue
		}
		active, err := decodeActivationSignature(encoded)
		if err != nil {
			return fmt.Errorf("decoding cached incremental activation %s: %w", key, err)
		}
		previous[key] = active
	}

	runCtx := context.WithValue(ctx, incrementalRunContextKey{}, r)
	results, err := r.graphSession.EvaluateAll(runCtx, keys...)
	if err != nil {
		return err
	}
	removed := make([]incremental.QueryKey, 0)
	for index := range results {
		source, namespace, name, ok := parseActivationQueryKey(results[index].Key)
		if !ok {
			return fmt.Errorf("incremental activation stage returned an invalid key %s",
				results[index].Key)
		}
		active, err := decodeActivationSignature(results[index].Value)
		if err != nil {
			return fmt.Errorf("decoding incremental activation for %q %s/%s: %w",
				source, namespace, name, err)
		}
		retired, err := r.applyActivationTransition(
			source, namespace, name, previous[results[index].Key], active,
		)
		if err != nil {
			return err
		}
		removed = append(removed, retired...)
		r.activationValues[results[index].Key] = active
		delete(r.activationQueries, results[index].Key)
		delete(r.dirtyQueries, results[index].Key)
	}
	if len(removed) == 0 {
		return nil
	}
	slices.SortFunc(removed, func(left, right incremental.QueryKey) int {
		return strings.Compare(left.Opaque(), right.Opaque())
	})
	if err := r.graphSession.RemoveQueriesWhileIdle(removed...); err != nil {
		return fmt.Errorf("retiring inactive incremental components: %w", err)
	}
	return nil
}

// baseActivationValue reads a cached activation from the generation the
// session began on. The graph's current generation can be ahead of it once a
// concurrent commit lands, and an activation read from there reports an
// instance as already active that this session never activated.
func (r *incrementalRenderSession) baseActivationValue(key incremental.QueryKey) ([]byte, bool) {
	if r.graphSession != nil {
		return r.graphSession.BaseValue(key)
	}
	return r.state.graph.Value(key)
}

func (r *incrementalRenderSession) baseHasDependents(key incremental.QueryKey) bool {
	if r.graphSession != nil {
		return r.graphSession.BaseHasDependents(key)
	}
	return r.state.graph.HasDependents(key)
}

func (r *incrementalRenderSession) baseHasInputDependents(key incremental.InputKey) bool {
	if r.graphSession != nil {
		return r.graphSession.BaseHasInputDependents(key)
	}
	return r.state.graph.HasInputDependents(key)
}

func (r *incrementalRenderSession) pendingActivationQueries() []incremental.QueryKey {
	set := make(map[incremental.QueryKey]struct{}, len(r.activationQueries))
	for key := range r.activationQueries {
		set[key] = struct{}{}
	}
	for key := range r.dirtyQueries {
		if _, _, _, ok := parseActivationQueryKey(key); ok {
			set[key] = struct{}{}
		}
	}
	keys := make([]incremental.QueryKey, 0, len(set))
	for key := range set {
		if _, removed := r.removed[key]; !removed {
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, func(left, right incremental.QueryKey) int {
		return strings.Compare(left.Opaque(), right.Opaque())
	})
	return keys
}

func (r *incrementalRenderSession) applyActivationTransition(
	source, namespace, name string,
	previous, active []string,
) ([]incremental.QueryKey, error) {
	oldSet := make(map[string]struct{}, len(previous))
	for _, component := range previous {
		oldSet[component] = struct{}{}
	}
	activeSet, err := r.validatedActivationSet(source, active)
	if err != nil {
		return nil, err
	}
	removed, err := r.retireDeactivatedComponents(source, namespace, name, oldSet, activeSet)
	if err != nil {
		return nil, err
	}
	if err := r.admitActivatedComponents(source, namespace, name, oldSet, activeSet); err != nil {
		return nil, err
	}
	return removed, nil
}

func (r *incrementalRenderSession) validatedActivationSet(
	source string,
	active []string,
) (map[string]struct{}, error) {
	activeSet := make(map[string]struct{}, len(active))
	for _, componentName := range active {
		component, exists := r.state.components[componentName]
		if !exists || len(component.activationPaths) == 0 {
			return nil, fmt.Errorf("incremental activation names invalid component %q", componentName)
		}
		if _, bound := r.bindingPlan.props[string(bindingKey(componentName, source))]; !bound {
			return nil, fmt.Errorf("incremental activation names unbound component %q for %q",
				componentName, source)
		}
		activeSet[componentName] = struct{}{}
	}
	return activeSet, nil
}

func (r *incrementalRenderSession) retireDeactivatedComponents(
	source, namespace, name string,
	oldSet, activeSet map[string]struct{},
) ([]incremental.QueryKey, error) {
	removed := make([]incremental.QueryKey, 0)
	for componentName := range oldSet {
		if _, remainsActive := activeSet[componentName]; remainsActive {
			continue
		}
		component, exists := r.state.components[componentName]
		if !exists {
			return nil, fmt.Errorf("cached incremental activation names invalid component %q", componentName)
		}
		if err := r.setActivationInstanceActive(&component, source, namespace, name, false); err != nil {
			return nil, err
		}
		query := componentQueryKey(&component, source, namespace, name)
		delete(r.newQueries, query)
		delete(r.dirtyQueries, query)
		r.removed[query] = struct{}{}
		r.retired.Delete([]byte(query.Opaque()))
		if err := r.deleteResult(&component, source, namespace, name); err != nil {
			return nil, err
		}
		removed = append(removed, query)
	}
	return removed, nil
}

func (r *incrementalRenderSession) admitActivatedComponents(
	source, namespace, name string,
	oldSet, activeSet map[string]struct{},
) error {
	for componentName := range activeSet {
		component := r.state.components[componentName]
		if err := r.setActivationInstanceActive(&component, source, namespace, name, true); err != nil {
			return err
		}
		if _, wasActive := oldSet[componentName]; wasActive {
			continue
		}
		query := componentQueryKey(&component, source, namespace, name)
		delete(r.removed, query)
		r.retired.Delete([]byte(query.Opaque()))
		if _, exists := r.results.Get(resultKey(&component, source, namespace, name)); exists {
			return fmt.Errorf("inactive incremental component %q has a cached result", componentName)
		}
		r.newQueries[query] = struct{}{}
	}
	return nil
}

func (r *incrementalRenderSession) activationActive(
	component *incrementalComponent,
	source, namespace, name string,
) (bool, error) {
	if len(component.activationPaths) == 0 {
		return true, nil
	}
	key := activationQueryKey(source, namespace, name)
	active, exists := r.activationValues[key]
	if !exists {
		encoded, found := r.baseActivationValue(key)
		if !found {
			return false, fmt.Errorf("incremental activation for %q %s/%s is unavailable",
				source, namespace, name)
		}
		var err error
		active, err = decodeActivationSignature(encoded)
		if err != nil {
			return false, fmt.Errorf("decoding incremental activation for %q %s/%s: %w",
				source, namespace, name, err)
		}
	}
	_, found := slices.BinarySearch(active, component.name)
	return found, nil
}

func incrementalComponentActive(component *incrementalComponent, item map[string]any) (bool, error) {
	if len(component.activationPaths) == 0 {
		return true, nil
	}
	for _, path := range component.activationPaths {
		exists, err := path.Exists(item)
		if err != nil {
			return false, err
		}
		if exists {
			return true, nil
		}
	}
	return false, nil
}
