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
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	iradix "github.com/hashicorp/go-immutable-radix/v2"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/incremental"
)

type incrementalPreparedPlanExactRootValidator interface {
	ValidateExactValue(incremental.QueryKey, incremental.ExactValueRoot) error
}

type incrementalPreparedPlanBootstrapInstance struct {
	group   string
	id      incrementalGroupInstanceID
	key     string
	encoded string
}

type incrementalPreparedPlanBootstrapCandidate struct {
	encoded   string
	candidate incrementalPreparedBackendCandidate
}

type incrementalPreparedPlanBootstrapProfile struct {
	candidate incrementalPreparedProfileCandidate
}

type incrementalPreparedPlanBootstrapOutput struct {
	key   string
	parts []incrementalPreparedPlanBootstrapOutputPart
}

type incrementalPreparedPlanBootstrapOutputPart struct {
	text     string
	identity string
	location string
}

type incrementalPreparedPlanBootstrapBuilder struct {
	instances           map[string]string
	calls               map[string]string
	backendCalls        map[string]incrementalPreparedPlanBootstrapCandidate
	backendCandidates   map[string]map[string]string
	backendWinners      map[string]incrementalPreparedPlanBootstrapCandidate
	backendWinnerKeys   map[string]string
	profileCandidates   map[string]map[string]string
	profileValues       map[string]map[string]incrementalPreparedPlanBootstrapProfile
	profileVariants     map[string]map[string]map[string]struct{}
	standaloneProfiles  map[string]map[string]struct{}
	conditions          map[string]map[string]struct{}
	requirements        map[string]map[string]string
	missingProfiles     map[string]string
	conflictingProfiles map[string]string
	outputs             map[string]string
	pendingOutputs      []incrementalPreparedPlanBootstrapOutput
	pendingOutputKeys   map[string]struct{}
}

func newIncrementalPreparedPlanFromIndexes(
	groups []string,
	indexes map[string]*incrementalGroupIndex,
	components map[string]incrementalComponent,
	resultRoot *iradix.Node[incremental.ExactValueRoot],
	validator incrementalPreparedPlanExactRootValidator,
) (*incrementalPreparedPlan, error) {
	groupIndexes, trackedGroups, err := validateIncrementalPreparedPlanBootstrapGroups(
		groups, indexes, components,
	)
	if err != nil {
		return nil, err
	}
	instances, err := collectIncrementalPreparedPlanBootstrapInstances(
		trackedGroups, indexes, components, resultRoot, validator,
	)
	if err != nil {
		return nil, err
	}
	builder := newIncrementalPreparedPlanBootstrapBuilder(len(instances))
	for index := range instances {
		if err := builder.addInstance(&instances[index]); err != nil {
			return nil, err
		}
	}
	return newIncrementalPreparedPlanFromBootstrapBuilder(
		builder, groupIndexes, indexes, resultRoot,
	)
}

func newIncrementalPreparedPlanFromBootstrapBuilder(
	builder *incrementalPreparedPlanBootstrapBuilder,
	groupIndexes *iradix.Tree[*incrementalGroupIndex],
	indexes map[string]*incrementalGroupIndex,
	resultRoot *iradix.Node[incremental.ExactValueRoot],
) (*incrementalPreparedPlan, error) {
	if builder == nil || groupIndexes == nil || resultRoot == nil {
		return nil, errors.New("incremental prepared plan bootstrap is incomplete")
	}
	if err := builder.selectWinnersAndOutputs(indexes); err != nil {
		return nil, err
	}
	selected, err := builder.selectedSnapshot()
	if err != nil {
		return nil, err
	}
	plan := &incrementalPreparedPlan{
		instances:           incrementalPreparedPlanFlatTree(builder.instances),
		calls:               incrementalPreparedPlanFlatTree(builder.calls),
		backendCandidates:   incrementalPreparedPlanNestedTree(builder.backendCandidates),
		profileCandidates:   incrementalPreparedPlanNestedTree(builder.profileCandidates),
		profileVariants:     incrementalPreparedPlanVariantTree(builder.profileVariants),
		standaloneProfiles:  incrementalPreparedPlanNestedTree(builder.standaloneProfiles),
		conditions:          incrementalPreparedPlanNestedTree(builder.conditions),
		requirements:        incrementalPreparedPlanNestedTree(builder.requirements),
		missingProfiles:     incrementalPreparedPlanFlatTree(builder.missingProfiles),
		conflictingProfiles: incrementalPreparedPlanFlatTree(builder.conflictingProfiles),
		outputs:             incrementalPreparedPlanFlatTree(builder.outputs),
		groups:              groupIndexes,
		selected:            selected,
		resultRoot:          resultRoot,
	}
	plan.outputMemo = newIncrementalPreparedPlanOutputMemo(plan.outputs.Root(), plan.selected, nil, nil)
	plan.authenticate()
	if err := plan.validateAuthentication(resultRoot); err != nil {
		return nil, err
	}
	return plan, nil
}

func validateIncrementalPreparedPlanBootstrapGroups(
	groups []string,
	indexes map[string]*incrementalGroupIndex,
	components map[string]incrementalComponent,
) (*iradix.Tree[*incrementalGroupIndex], map[string]struct{}, error) {
	tracked := make(map[string]struct{}, len(groups))
	groupEntries := make(map[string]*incrementalGroupIndex, len(groups))
	for _, group := range groups {
		if group == "" {
			return nil, nil, errors.New("incremental prepared plan has an empty group")
		}
		if _, duplicate := tracked[group]; duplicate {
			return nil, nil, fmt.Errorf("incremental prepared plan repeats group %q", group)
		}
		index := indexes[group]
		if index == nil {
			return nil, nil, fmt.Errorf("incremental group %q has no assembly index", group)
		}
		if err := index.validateAuthentication(); err != nil {
			return nil, nil, fmt.Errorf("authenticating incremental group %q: %w", group, err)
		}
		tracked[group] = struct{}{}
		groupEntries[group] = index
	}
	for name := range components {
		component := components[name]
		if name == "" || component.name != name || component.group == "" {
			return nil, nil, fmt.Errorf("incremental prepared plan component %q has invalid provenance", name)
		}
		if !component.backendPlan {
			continue
		}
		if _, exists := tracked[component.group]; !exists {
			return nil, nil, fmt.Errorf(
				"incremental backendPlan component %q has untracked group %q", name, component.group,
			)
		}
	}
	return incrementalPreparedPlanFlatTree(groupEntries), tracked, nil
}

func collectIncrementalPreparedPlanBootstrapInstances(
	trackedGroups map[string]struct{},
	indexes map[string]*incrementalGroupIndex,
	components map[string]incrementalComponent,
	resultRoot *iradix.Node[incremental.ExactValueRoot],
	validator incrementalPreparedPlanExactRootValidator,
) ([]incrementalPreparedPlanBootstrapInstance, error) {
	if resultRoot == nil {
		return nil, errors.New("incremental prepared plan result root is unavailable")
	}
	if validator == nil {
		return nil, errors.New("incremental prepared plan result authority is unavailable")
	}
	seen := make(map[string]struct{})
	instances := make([]incrementalPreparedPlanBootstrapInstance, 0)
	var walkErr error
	resultRoot.Walk(func(key []byte, root incremental.ExactValueRoot) bool {
		instance, include, err := incrementalPreparedPlanBootstrapInstanceFor(
			key, root, trackedGroups, indexes, components, validator, seen,
		)
		if err != nil {
			walkErr = err
			return true
		}
		if include {
			instances = append(instances, instance)
		}
		return false
	})
	if walkErr != nil {
		return nil, walkErr
	}
	for group := range trackedGroups {
		if err := validateIncrementalPreparedPlanBootstrapIndex(
			group, indexes[group], components, seen,
		); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(instances, func(left, right incrementalPreparedPlanBootstrapInstance) int {
		return cmp.Compare(left.key, right.key)
	})
	return instances, nil
}

func incrementalPreparedPlanBootstrapInstanceFor(
	key []byte,
	root incremental.ExactValueRoot,
	trackedGroups map[string]struct{},
	indexes map[string]*incrementalGroupIndex,
	components map[string]incrementalComponent,
	validator incrementalPreparedPlanExactRootValidator,
	seen map[string]struct{},
) (instance incrementalPreparedPlanBootstrapInstance, include bool, err error) {
	identity, ok := parseResultKey(key)
	if !ok {
		return instance, false, fmt.Errorf("incremental result cache has invalid key %s", incremental.DescribeTuple(string(key)))
	}
	component, componentExists := components[identity.component]
	if componentExists && component.backendPlan &&
		(component.group != identity.group || !incrementalPreparedPlanTracksGroup(trackedGroups, identity.group)) {
		return instance, false, fmt.Errorf("incremental backendPlan result %s has the wrong group", incremental.DescribeTuple(string(key)))
	}
	if !incrementalPreparedPlanTracksGroup(trackedGroups, identity.group) {
		return instance, false, nil
	}
	if !componentExists || component.name != identity.component || component.group != identity.group {
		return instance, false, fmt.Errorf("incremental backendPlan result %s has an invalid component", incremental.DescribeTuple(string(key)))
	}
	expectedKey := resultKey(&component, identity.source, identity.namespace, identity.name)
	if !bytes.Equal(key, expectedKey) {
		return instance, false, fmt.Errorf("incremental backendPlan result %s has a noncanonical identity", incremental.DescribeTuple(string(key)))
	}
	queryKey := componentQueryKey(&component, identity.source, identity.namespace, identity.name)
	if err := validator.ValidateExactValue(queryKey, root); err != nil {
		return instance, false, fmt.Errorf("authenticating incremental backendPlan result %s: %w", incremental.DescribeTuple(string(key)), err)
	}
	encoded, err := root.String()
	if err != nil {
		return instance, false, fmt.Errorf("reading incremental backendPlan result %s: %w", incremental.DescribeTuple(string(key)), err)
	}
	id := incrementalGroupInstanceID{
		component: identity.component,
		source:    identity.source,
		namespace: identity.namespace,
		name:      identity.name,
	}
	index := indexes[identity.group]
	indexed, exists := index.instances.Root().Get(incrementalGroupInstanceKey(id))
	if !exists || indexed.id != id || indexed.encodedResult != encoded {
		return instance, false, fmt.Errorf(
			"incremental group %q assembly index does not match its result cache", identity.group,
		)
	}
	seenKey := incrementalPreparedPlanBootstrapSeenKey(identity.group, id)
	if _, duplicate := seen[seenKey]; duplicate {
		return instance, false, errors.New("incremental prepared plan repeats a result identity")
	}
	seen[seenKey] = struct{}{}
	if !component.backendPlan {
		return instance, false, nil
	}
	return incrementalPreparedPlanBootstrapInstance{
		group: identity.group, id: id, key: seenKey, encoded: encoded,
	}, true, nil
}

func validateIncrementalPreparedPlanBootstrapIndex(
	group string,
	index *incrementalGroupIndex,
	components map[string]incrementalComponent,
	seen map[string]struct{},
) error {
	var walkErr error
	index.instances.Root().Walk(func(key string, instance incrementalIndexedGroupInstance) bool {
		if !stringBytesEqual(key, incrementalGroupInstanceKey(instance.id)) {
			walkErr = fmt.Errorf("incremental group %q assembly index has a noncanonical instance", group)
			return true
		}
		component, exists := components[instance.id.component]
		if !exists || component.name != instance.id.component || component.group != group {
			walkErr = fmt.Errorf("incremental group %q assembly index has an invalid component", group)
			return true
		}
		if _, exists := seen[incrementalPreparedPlanBootstrapSeenKey(group, instance.id)]; !exists {
			walkErr = fmt.Errorf(
				"incremental group %q assembly index does not match its result cache", group,
			)
			return true
		}
		return false
	})
	return walkErr
}

func incrementalPreparedPlanTracksGroup(groups map[string]struct{}, group string) bool {
	_, exists := groups[group]
	return exists
}

func incrementalPreparedPlanBootstrapSeenKey(group string, id incrementalGroupInstanceID) string {
	return string(incrementalPreparedPlanOutputKey(group, id))
}

func newIncrementalPreparedPlanBootstrapBuilder(capacity int) *incrementalPreparedPlanBootstrapBuilder {
	return &incrementalPreparedPlanBootstrapBuilder{
		instances:           make(map[string]string, capacity),
		calls:               make(map[string]string),
		backendCalls:        make(map[string]incrementalPreparedPlanBootstrapCandidate),
		backendCandidates:   make(map[string]map[string]string),
		backendWinners:      make(map[string]incrementalPreparedPlanBootstrapCandidate),
		backendWinnerKeys:   make(map[string]string),
		profileCandidates:   make(map[string]map[string]string),
		profileValues:       make(map[string]map[string]incrementalPreparedPlanBootstrapProfile),
		profileVariants:     make(map[string]map[string]map[string]struct{}),
		standaloneProfiles:  make(map[string]map[string]struct{}),
		conditions:          make(map[string]map[string]struct{}),
		requirements:        make(map[string]map[string]string),
		missingProfiles:     make(map[string]string),
		conflictingProfiles: make(map[string]string),
		outputs:             make(map[string]string, capacity),
		pendingOutputs:      make([]incrementalPreparedPlanBootstrapOutput, 0, capacity),
		pendingOutputKeys:   make(map[string]struct{}, capacity),
	}
}

func (b *incrementalPreparedPlanBootstrapBuilder) addInstance(
	instance *incrementalPreparedPlanBootstrapInstance,
) error {
	result, err := decodeIncrementalComponentResultString(instance.encoded)
	if err != nil {
		return fmt.Errorf("decoding incremental backendPlan instance: %w", err)
	}
	if err := validateIncrementalBackendPlanInstance(&result); err != nil {
		return incrementalInstanceError(&incrementalInstanceResult{
			component: instance.id.component,
			source:    instance.id.source,
			namespace: instance.id.namespace,
			name:      instance.id.name,
		}, err)
	}
	return b.addValidatedInstance(instance, &result)
}

func (b *incrementalPreparedPlanBootstrapBuilder) addValidatedInstance(
	instance *incrementalPreparedPlanBootstrapInstance,
	result *incrementalComponentResult,
) error {
	if instance == nil || result == nil {
		return errors.New("incremental prepared plan instance is incomplete")
	}
	instanceKey := string(incrementalGroupInstanceKey(instance.id))
	if _, duplicate := b.instances[instanceKey]; duplicate {
		return errors.New("incremental prepared plan repeats an instance")
	}
	b.instances[instanceKey] = instance.encoded
	if err := b.addBackendCalls(instance, result); err != nil {
		return err
	}
	if _, duplicate := b.pendingOutputKeys[instance.key]; duplicate {
		return errors.New("incremental prepared plan repeats an output")
	}
	b.pendingOutputKeys[instance.key] = struct{}{}
	pending, err := pendingIncrementalPreparedPlanOutput(instance, result)
	if err != nil {
		return err
	}
	b.pendingOutputs = append(b.pendingOutputs, pending)
	return nil
}

func (b *incrementalPreparedPlanBootstrapBuilder) addBackendCalls(
	instance *incrementalPreparedPlanBootstrapInstance,
	result *incrementalComponentResult,
) error {
	for callIndex := range result.BackendPlan {
		call := &result.BackendPlan[callIndex]
		location := incrementalGroupLocationKey(instance.id, uint64(callIndex))
		if call.Profile != nil {
			if call.Backend != nil {
				return errors.New("incremental prepared plan call mixes a profile and backend")
			}
			if err := b.addProfile(location, call); err != nil {
				return err
			}
			continue
		}
		if call.Backend == nil {
			return errors.New("incremental prepared plan backend call has no declaration")
		}
		candidate := incrementalPreparedBackendCandidate{
			Group:     instance.group,
			Component: instance.id.component,
			Source:    instance.id.source,
			Namespace: instance.id.namespace,
			Name:      instance.id.name,
			Call:      uint32(callIndex),
			Backend:   call.Backend.Clone(),
			WhenAny:   cloneIncrementalBackendPlanCondition(call.WhenAny),
		}
		encoded, err := json.Marshal(candidate)
		if err != nil {
			return fmt.Errorf("encoding incremental prepared backend call: %w", err)
		}
		encodedCandidate := string(encoded)
		locationKey := string(location)
		if _, duplicate := b.calls[locationKey]; duplicate {
			return errors.New("incremental prepared plan repeats a backend call")
		}
		b.calls[locationKey] = encodedCandidate
		b.backendCalls[locationKey] = incrementalPreparedPlanBootstrapCandidate{
			encoded: encodedCandidate, candidate: candidate,
		}
		for _, key := range conditionKeys(call.WhenAny) {
			conditionKey := string(incrementalPreparedPlanConditionKey(
				instance.group, call.WhenAny.Cell, key,
			))
			incrementalPreparedPlanBootstrapSet(b.conditions, conditionKey, locationKey)
		}
	}
	return nil
}

func pendingIncrementalPreparedPlanOutput(
	instance *incrementalPreparedPlanBootstrapInstance,
	result *incrementalComponentResult,
) (incrementalPreparedPlanBootstrapOutput, error) {
	pending := incrementalPreparedPlanBootstrapOutput{
		key: instance.key,
		parts: make([]incrementalPreparedPlanBootstrapOutputPart, 0,
			len(result.BackendPlanOutput)),
	}
	for partIndex := range result.BackendPlanOutput {
		part := &result.BackendPlanOutput[partIndex]
		if part.BackendCall == nil {
			if part.Text == "" {
				return pending, errors.New("incremental prepared plan output has an empty text part")
			}
			pending.parts = append(pending.parts, incrementalPreparedPlanBootstrapOutputPart{text: part.Text})
			continue
		}
		callIndex := int(*part.BackendCall)
		if callIndex >= len(result.BackendPlan) || result.BackendPlan[callIndex].Backend == nil {
			return pending, errors.New("incremental prepared plan output has an invalid backend reference")
		}
		call := &result.BackendPlan[callIndex]
		pending.parts = append(pending.parts, incrementalPreparedPlanBootstrapOutputPart{
			identity: call.Identity,
			location: string(incrementalGroupLocationKey(instance.id, uint64(*part.BackendCall))),
		})
	}
	return pending, nil
}

func (b *incrementalPreparedPlanBootstrapBuilder) addProfile(
	location []byte,
	call *incrementalBackendPlanCall,
) error {
	candidate := incrementalPreparedProfileCandidate{
		Profile: call.Profile.Clone(), Standalone: len(call.Owners) == 0,
	}
	encoded, err := json.Marshal(candidate)
	if err != nil {
		return fmt.Errorf("encoding incremental prepared profile call: %w", err)
	}
	name := candidate.Profile.Name
	locationKey := string(location)
	encodedCandidate := string(encoded)
	incrementalPreparedPlanBootstrapValue(
		b.profileCandidates, name, locationKey, encodedCandidate,
	)
	values := b.profileValues[name]
	if values == nil {
		values = make(map[string]incrementalPreparedPlanBootstrapProfile)
		b.profileValues[name] = values
	}
	if _, duplicate := values[locationKey]; duplicate {
		return errors.New("incremental prepared plan repeats a profile call")
	}
	values[locationKey] = incrementalPreparedPlanBootstrapProfile{
		candidate: candidate,
	}
	variants := b.profileVariants[name]
	if variants == nil {
		variants = make(map[string]map[string]struct{})
		b.profileVariants[name] = variants
	}
	locations := variants[candidate.Profile.Text]
	if locations == nil {
		locations = make(map[string]struct{})
		variants[candidate.Profile.Text] = locations
	}
	locations[locationKey] = struct{}{}
	if candidate.Standalone {
		incrementalPreparedPlanBootstrapSet(b.standaloneProfiles, name, locationKey)
	}
	return nil
}

func (b *incrementalPreparedPlanBootstrapBuilder) selectWinnersAndOutputs(
	indexes map[string]*incrementalGroupIndex,
) error {
	if err := b.selectBackendWinners(indexes); err != nil {
		return err
	}
	b.recordProfileRequirements()
	b.flagProfileProblems()
	return b.encodePendingOutputs()
}

func (b *incrementalPreparedPlanBootstrapBuilder) selectBackendWinners(
	indexes map[string]*incrementalGroupIndex,
) error {
	for _, locationKey := range incrementalPreparedPlanSortedKeys(b.backendCalls) {
		call := b.backendCalls[locationKey]
		index := indexes[call.candidate.Group]
		if index == nil {
			return fmt.Errorf("incremental group %q has no assembly index", call.candidate.Group)
		}
		reachable, err := incrementalPreparedBackendReachable(&call.candidate, index)
		if err != nil {
			return err
		}
		if !reachable {
			continue
		}
		identity := call.candidate.Backend.Backend.Name
		incrementalPreparedPlanBootstrapValue(
			b.backendCandidates, identity, locationKey, call.encoded,
		)
		winnerKey, winnerExists := b.backendWinnerKeys[identity]
		if !winnerExists || locationKey < winnerKey {
			b.backendWinnerKeys[identity] = locationKey
			b.backendWinners[identity] = call
		}
	}
	return nil
}

func (b *incrementalPreparedPlanBootstrapBuilder) recordProfileRequirements() {
	for identity := range b.backendWinners {
		winner := b.backendWinners[identity]
		profile := winner.candidate.Backend.Backend.Profile
		if profile != "" {
			incrementalPreparedPlanBootstrapValue(
				b.requirements, profile, identity, winner.encoded,
			)
		}
	}
}

func (b *incrementalPreparedPlanBootstrapBuilder) flagProfileProblems() {
	profileNames := make(map[string]struct{}, len(b.profileCandidates)+len(b.requirements))
	for name := range b.profileCandidates {
		profileNames[name] = struct{}{}
	}
	for name := range b.requirements {
		profileNames[name] = struct{}{}
	}
	for name := range profileNames {
		if len(b.profileVariants[name]) > 1 {
			b.conflictingProfiles[name] = name
		}
		needed := len(b.standaloneProfiles[name]) > 0 || len(b.requirements[name]) > 0
		if needed && len(b.profileCandidates[name]) == 0 {
			b.missingProfiles[name] = name
		}
	}
}

func (b *incrementalPreparedPlanBootstrapBuilder) encodePendingOutputs() error {
	for outputIndex := range b.pendingOutputs {
		pending := &b.pendingOutputs[outputIndex]
		parts := make([]incrementalPreparedPlanOutputPart, 0, len(pending.parts))
		for partIndex := range pending.parts {
			part := &pending.parts[partIndex]
			if part.text != "" {
				parts = append(parts, incrementalPreparedPlanOutputPart{Text: part.text})
				continue
			}
			if b.backendWinnerKeys[part.identity] == part.location {
				parts = append(parts, incrementalPreparedPlanOutputPart{Backend: part.identity})
			}
		}
		encoded, err := json.Marshal(parts)
		if err != nil {
			return fmt.Errorf("encoding incremental prepared plan output: %w", err)
		}
		b.outputs[pending.key] = string(encoded)
	}
	return nil
}

func (b *incrementalPreparedPlanBootstrapBuilder) selectedSnapshot() (*rendercontext.PreparedPlanSnapshot, error) {
	profiles := make([]rendercontext.PreparedPlanProfile, 0, len(b.profileCandidates))
	for name, candidates := range b.profileValues {
		if len(b.standaloneProfiles[name]) == 0 && len(b.requirements[name]) == 0 {
			continue
		}
		location, exists := incrementalPreparedPlanBootstrapMinimum(candidates)
		if !exists {
			continue
		}
		profiles = append(profiles, candidates[location].candidate.Profile)
	}
	backends := make([]*rendercontext.PreparedPlanBackend, 0, len(b.backendWinners))
	for identity := range b.backendWinners {
		backend := b.backendWinners[identity].candidate.Backend
		backends = append(backends, &backend)
	}
	selected, err := rendercontext.NewPreparedPlanSnapshotFromDeclarations(profiles, backends)
	if err != nil {
		return nil, fmt.Errorf("building incremental prepared plan snapshot: %w", err)
	}
	return selected, nil
}

func incrementalPreparedPlanBootstrapMinimum[T any](values map[string]T) (string, bool) {
	minimum := ""
	found := false
	for key := range values {
		if !found || key < minimum {
			minimum = key
			found = true
		}
	}
	return minimum, found
}

func incrementalPreparedPlanBootstrapSet(
	entries map[string]map[string]struct{},
	outer, inner string,
) {
	values := entries[outer]
	if values == nil {
		values = make(map[string]struct{})
		entries[outer] = values
	}
	values[inner] = struct{}{}
}

func incrementalPreparedPlanBootstrapValue[T any](
	entries map[string]map[string]T,
	outer, inner string,
	value T,
) {
	values := entries[outer]
	if values == nil {
		values = make(map[string]T)
		entries[outer] = values
	}
	values[inner] = value
}

func incrementalPreparedPlanFlatTree[T any](entries map[string]T) *iradix.Tree[T] {
	keys := incrementalPreparedPlanSortedKeys(entries)
	txn := iradix.New[T]().Txn()
	for _, key := range keys {
		txn.Insert([]byte(key), entries[key])
	}
	return txn.Commit()
}

func incrementalPreparedPlanNestedTree[T any](
	entries map[string]map[string]T,
) *iradix.Tree[*iradix.Tree[T]] {
	outer := iradix.New[*iradix.Tree[T]]().Txn()
	for _, outerKey := range incrementalPreparedPlanSortedKeys(entries) {
		inner := incrementalPreparedPlanFlatTree(entries[outerKey])
		if inner.Len() != 0 {
			outer.Insert([]byte(outerKey), inner)
		}
	}
	return outer.Commit()
}

func incrementalPreparedPlanVariantTree(
	entries map[string]map[string]map[string]struct{},
) *iradix.Tree[*iradix.Tree[*iradix.Tree[struct{}]]] {
	outer := iradix.New[*iradix.Tree[*iradix.Tree[struct{}]]]().Txn()
	for _, profile := range incrementalPreparedPlanSortedKeys(entries) {
		variants := incrementalPreparedPlanNestedTree(entries[profile])
		if variants.Len() != 0 {
			outer.Insert([]byte(profile), variants)
		}
	}
	return outer.Commit()
}

func incrementalPreparedPlanSortedKeys[T any](entries map[string]T) []string {
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
