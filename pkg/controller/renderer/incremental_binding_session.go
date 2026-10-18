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
	"errors"
	"fmt"
	"slices"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/incremental"
)

func (r *incrementalRenderSession) applyBindingPlan() error {
	previous, err := r.currentBindings()
	if err != nil {
		return err
	}
	r.markSourcesForReload(previous)
	next := make(map[string]incrementalBinding, len(r.bindingPlan.bindings))
	for _, binding := range r.bindingPlan.bindings {
		next[string(bindingKey(binding.component, binding.source))] = binding
	}

	for _, key := range sortedBindingKeys(previous, next) {
		if err := r.applyBindingTransition(key, previous, next); err != nil {
			return err
		}
	}
	if err := r.applyDeriveOwners(previous); err != nil {
		return err
	}
	r.pruneInactiveMembers()
	return nil
}

func sortedBindingKeys(previous, next map[string]incrementalBinding) []string {
	keys := make([]string, 0, len(previous)+len(next))
	seen := make(map[string]struct{}, len(previous)+len(next))
	for key := range previous {
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	for key := range next {
		if _, exists := seen[key]; !exists {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

func (r *incrementalRenderSession) applyBindingTransition(
	key string,
	previous, next map[string]incrementalBinding,
) error {
	oldBinding, hadOld := previous[key]
	newBinding, hasNew := next[key]
	switch {
	case hadOld && !hasNew:
		if err := r.removeBinding(oldBinding); err != nil {
			return err
		}
		r.bindings.Delete([]byte(key))
		r.inputChanges[bindingInputKey(oldBinding.component, oldBinding.source)] = absentBindingInput(oldBinding)
	case !hadOld && hasNew:
		r.bindings.Insert([]byte(key), string(newBinding.props))
		if err := r.addBinding(newBinding); err != nil {
			return err
		}
		r.inputChanges[bindingInputKey(newBinding.component, newBinding.source)] = bindingInput(newBinding)
	case hadOld && hasNew && !bytes.Equal(oldBinding.props, newBinding.props):
		component := r.state.components[newBinding.component]
		if component.resourceProjection {
			if err := r.removeBinding(oldBinding); err != nil {
				return err
			}
			if err := r.addBinding(newBinding); err != nil {
				return err
			}
		}
		r.bindings.Insert([]byte(key), string(newBinding.props))
		r.inputChanges[bindingInputKey(newBinding.component, newBinding.source)] = bindingInput(newBinding)
	}
	return nil
}

func (r *incrementalRenderSession) markSourcesForReload(previous map[string]incrementalBinding) {
	previousSources := make(map[string]struct{}, len(previous))
	for _, binding := range previous {
		previousSources[binding.source] = struct{}{}
	}
	for source := range r.bindingPlan.bySource {
		if _, previouslyActive := previousSources[source]; previouslyActive {
			continue
		}
		if _, tracked := r.cursors[source]; tracked {
			r.reloadSources[source] = struct{}{}
		}
	}
}

func (r *incrementalRenderSession) currentBindings() (map[string]incrementalBinding, error) {
	result := map[string]incrementalBinding{}
	var walkErr error
	r.bindings.Root().Walk(func(key []byte, props string) bool {
		component, source, ok := parseBindingKey(key)
		if !ok {
			walkErr = fmt.Errorf("incremental binding has an invalid key %s", incremental.DescribeTuple(string(key)))
			return true
		}
		if _, exists := r.state.components[component]; !exists {
			walkErr = fmt.Errorf("incremental binding names unknown component %q", component)
			return true
		}
		result[string(key)] = incrementalBinding{
			component: component,
			source:    source,
			props:     []byte(props),
		}
		return false
	})
	return result, walkErr
}

func bindingInput(binding incrementalBinding) incremental.Input {
	return incremental.Input{
		Key:      bindingInputKey(binding.component, binding.source),
		Revision: exactBytesRevision("binding", binding.props),
		Found:    true,
		Value:    slices.Clone(binding.props),
	}
}

func absentBindingInput(binding incrementalBinding) incremental.Input {
	return incremental.Input{
		Key:      bindingInputKey(binding.component, binding.source),
		Revision: exactBytesRevision("binding-absent", bindingKey(binding.component, binding.source)),
		Found:    false,
	}
}

func (r *incrementalRenderSession) pruneInactiveMembers() {
	for source := range r.cursors {
		if _, active := r.bindingPlan.bySource[source]; active {
			continue
		}
		memberKeys := make([][]byte, 0)
		r.members.Root().WalkPrefix(memberPrefix(source), func(key []byte, _ struct{}) bool {
			memberKeys = append(memberKeys, slices.Clone(key))
			return false
		})
		for _, key := range memberKeys {
			r.members.Delete(key)
		}
	}
}

func (r *incrementalRenderSession) pruneUnreferencedResourceCursors() error {
	for source := range r.cursors {
		if r.bindingPlan != nil {
			if _, active := r.bindingPlan.bySource[source]; active {
				continue
			}
		}
		hasPrefix, err := r.catalogHasPrefix(resourceInputPrefix(source))
		if err != nil {
			return err
		}
		if !hasPrefix {
			delete(r.cursors, source)
		}
	}
	return nil
}

func (r *incrementalRenderSession) addBinding(binding incrementalBinding) error {
	component := r.state.components[binding.component]
	if component.resourceProjection {
		namespace, name, err := incrementalBindingProjectionIdentity(&component, binding)
		if err != nil {
			return err
		}
		query := r.registerComponentQuery(&component, binding.source, namespace, name)
		r.retired.Delete([]byte(query.Opaque()))
		if _, hasResult := r.results.Get(resultKey(&component, binding.source, namespace, name)); !hasResult {
			r.newQueries[query] = struct{}{}
		}
		r.groupChanged[component.group] = true
		return nil
	}
	r.members.Root().WalkPrefix(memberPrefix(binding.source), func(key []byte, _ struct{}) bool {
		namespace, name, ok := parseMemberKey(key)
		if !ok {
			return false
		}
		query := r.registerComponentQuery(&component, binding.source, namespace, name)
		r.retired.Delete([]byte(query.Opaque()))
		if len(component.activationPaths) > 0 {
			activation := activationQueryKey(binding.source, namespace, name)
			r.retired.Delete([]byte(activation.Opaque()))
			r.activationQueries[activation] = struct{}{}
		}
		if component.deriveResource {
			r.retired.Delete([]byte(derivedProjectionQueryKey(binding.source, namespace, name).Opaque()))
		}
		if len(component.activationPaths) == 0 {
			_, hasResult := r.results.Get(resultKey(&component, binding.source, namespace, name))
			if !hasResult {
				r.newQueries[query] = struct{}{}
			}
		}
		return false
	})
	return nil
}

func (r *incrementalRenderSession) removeBinding(binding incrementalBinding) error {
	component := r.state.components[binding.component]
	if component.resourceProjection {
		return r.removeProjectionBinding(&component, binding)
	}
	removalErr := r.removeMemberBindings(&component, binding.source)
	if len(component.activationPaths) > 0 {
		if !r.hasActivationBinding(binding.source) {
			r.retireActivationSource(binding.source)
		}
	}
	return removalErr
}

func (r *incrementalRenderSession) removeProjectionBinding(
	component *incrementalComponent,
	binding incrementalBinding,
) error {
	namespace, name, err := incrementalBindingProjectionIdentity(component, binding)
	if err != nil {
		return err
	}
	query := r.registerComponentQuery(component, binding.source, namespace, name)
	r.retired.Insert([]byte(query.Opaque()), struct{}{})
	delete(r.newQueries, query)
	if err := r.deleteResult(component, binding.source, namespace, name); err != nil {
		return err
	}
	r.groupChanged[component.group] = true
	return nil
}

func (r *incrementalRenderSession) removeMemberBindings(
	component *incrementalComponent,
	source string,
) error {
	var removalErr error
	r.members.Root().WalkPrefix(memberPrefix(source), func(key []byte, _ struct{}) bool {
		namespace, name, ok := parseMemberKey(key)
		if !ok {
			return false
		}
		query := r.registerComponentQuery(component, source, namespace, name)
		r.retired.Insert([]byte(query.Opaque()), struct{}{})
		if len(component.activationPaths) > 0 {
			if err := r.setActivationInstanceActive(component, source, namespace, name, false); err != nil {
				removalErr = err
				return true
			}
		}
		if component.deriveResource {
			r.retired.Insert([]byte(derivedProjectionQueryKey(source, namespace, name).Opaque()), struct{}{})
		}
		delete(r.newQueries, query)
		if err := r.deleteResult(component, source, namespace, name); err != nil {
			removalErr = err
			return true
		}
		return false
	})
	return removalErr
}

func incrementalBindingProjectionIdentity(
	component *incrementalComponent,
	binding incrementalBinding,
) (namespace, name string, err error) {
	projection, err := incrementalResourceProjectionForBinding(binding)
	if err != nil {
		return "", "", fmt.Errorf(
			"incremental component %q binding %q: %w",
			component.name,
			binding.source,
			err,
		)
	}
	namespace, name, ok := incrementalResourceProjectionIdentity(projection)
	if !ok {
		return "", "", fmt.Errorf(
			"incremental component %q binding %q has invalid projection identity",
			component.name,
			binding.source,
		)
	}
	return namespace, name, nil
}

func (r *incrementalRenderSession) hasActivationBinding(source string) bool {
	components := r.bindingPlan.bySource[source]
	for index := range components {
		if len(components[index].activationPaths) > 0 {
			return true
		}
	}
	return false
}

func (r *incrementalRenderSession) retireActivationSource(source string) {
	r.members.Root().WalkPrefix(memberPrefix(source), func(key []byte, _ struct{}) bool {
		namespace, name, ok := parseMemberKey(key)
		if !ok {
			return false
		}
		activation := activationQueryKey(source, namespace, name)
		r.retired.Insert([]byte(activation.Opaque()), struct{}{})
		delete(r.activationQueries, activation)
		delete(r.activationValues, activation)
		return false
	})
}

func (r *incrementalRenderSession) applyDeriveOwners(previous map[string]incrementalBinding) error {
	oldOwners := map[string]incrementalComponent{}
	for _, binding := range previous {
		component := r.state.components[binding.component]
		if !component.deriveResource {
			continue
		}
		if owner, exists := oldOwners[binding.source]; exists && owner.name != component.name {
			return errors.New("incremental cache contains conflicting deriveResource owners")
		}
		oldOwners[binding.source] = component
	}
	sources := make([]string, 0, len(oldOwners)+len(r.bindingPlan.owners))
	seen := map[string]struct{}{}
	for source := range oldOwners {
		seen[source] = struct{}{}
		sources = append(sources, source)
	}
	for source := range r.bindingPlan.owners {
		if _, exists := seen[source]; !exists {
			sources = append(sources, source)
		}
	}
	slices.Sort(sources)
	for _, source := range sources {
		oldOwner, hadOld := oldOwners[source]
		newOwner, hasNew := r.bindingPlan.owners[source]
		if hadOld == hasNew && (!hadOld || oldOwner.name == newOwner.name) {
			continue
		}
		r.inputChanges[deriveOwnerInputKey(source)] = deriveOwnerInput(source, &newOwner, hasNew)
	}
	return nil
}

func deriveOwnerInput(source string, component *incrementalComponent, found bool) incremental.Input {
	value := []byte(nil)
	if found {
		value = []byte(component.name)
	}
	return incremental.Input{
		Key:      deriveOwnerInputKey(source),
		Revision: exactBytesRevision("derive-owner", append([]byte(source+"\x00"), value...)),
		Found:    found,
		Value:    value,
	}
}

func (r *incrementalRenderSession) applyRenderSubject() error {
	if r.renderMode != rendercontext.RenderModeAdmission {
		return nil
	}
	subject, _ := r.baseContext["admissionSubject"].(map[string]any)
	namespace, _ := subject["namespace"].(string)
	name, _ := subject["name"].(string)
	if name == "" {
		return nil
	}
	for source := range r.bindingPlan.bySource {
		if !admissionSubjectMatches(subject, source, namespace, name) {
			continue
		}
		input, err := r.renderSubjectInput(source, namespace, name)
		if err != nil {
			return err
		}
		r.inputChanges[input.Key] = input
	}
	return nil
}

func admissionSubjectMatches(subject map[string]any, source, namespace, name string) bool {
	if subject == nil || name == "" {
		return false
	}
	if actualNamespace, _ := subject["namespace"].(string); actualNamespace != namespace {
		return false
	}
	if actualName, _ := subject["name"].(string); actualName != name {
		return false
	}
	if store, _ := subject["store"].(string); store == source {
		return true
	}
	stores, _ := subject["stores"].(map[string]any)
	selected, _ := stores[source].(bool)
	return selected
}

func (r *incrementalRenderSession) renderSubjectInput(source, namespace, name string) (incremental.Input, error) {
	mode := string(rendercontext.RenderModeReconcile)
	if r.renderMode == rendercontext.RenderModeAdmission {
		subject, _ := r.baseContext["admissionSubject"].(map[string]any)
		if admissionSubjectMatches(subject, source, namespace, name) {
			mode = string(rendercontext.RenderModeAdmission)
		}
	}
	value, err := encodeResourceValue(map[string]any{
		"mode":                       mode,
		incrementalSourceContextName: source,
		"namespace":                  namespace,
		"name":                       name,
	})
	if err != nil {
		return incremental.Input{}, err
	}
	return incremental.Input{
		Key:      renderSubjectInputKey(source, namespace, name),
		Revision: exactBytesRevision("render-subject", value),
		Found:    true,
		Value:    value,
	}, nil
}
