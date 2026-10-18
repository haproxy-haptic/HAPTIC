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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/names"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/incremental"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

const incrementalBackendPlanFirst = "first"

const incrementalBackendPlanMarker = "@haptic:incremental-backend:"

type incrementalBackendPlanCall struct {
	Policy   string                             `json:"policy,omitempty"`
	Identity string                             `json:"identity,omitempty"`
	Profile  *rendercontext.PreparedPlanProfile `json:"profile,omitempty"`
	Backend  *rendercontext.PreparedPlanBackend `json:"backend,omitempty"`
	Owners   []uint32                           `json:"owners,omitempty"`
	WhenAny  *incrementalBackendPlanCondition   `json:"whenAny,omitempty"`
}

type incrementalBackendPlanCondition struct {
	Cell string   `json:"cell"`
	Keys []string `json:"keys"`
}

type incrementalBackendPlanPart struct {
	Text        string  `json:"text,omitempty"`
	BackendCall *uint32 `json:"backendCall,omitempty"`
}

type incrementalBackendPlanRecorder struct {
	mu     sync.Mutex
	lease  *incrementalBatchReaderLease
	calls  []incrementalBackendPlanCall
	tokens map[string]uint32
}

type incrementalBackendPlanInstance struct {
	group string
	incrementalInstanceResult
}

type incrementalBackendPlanLocation struct {
	component string
	source    string
	namespace string
	name      string
	call      int
}

type incrementalBackendPlanSelection struct {
	winningCalls map[incrementalBackendPlanLocation]struct{}
	profileCalls map[incrementalBackendPlanLocation]struct{}
}

type incrementalBackendPlanPublicationOwners map[string]map[string]incrementalGroupInstanceID

type incrementalBackendPlanProfileCandidate struct {
	location incrementalBackendPlanLocation
	profile  *rendercontext.PreparedPlanProfile
}

type incrementalBackendPlanProfileRequirement struct {
	instance *incrementalBackendPlanInstance
}

type incrementalBackendPlanSelector struct {
	winnerIdentities   map[string]struct{}
	winningCalls       map[incrementalBackendPlanLocation]struct{}
	requiredProfiles   map[string]incrementalBackendPlanProfileRequirement
	profiles           map[string]incrementalBackendPlanProfileCandidate
	standaloneProfiles map[string]struct{}
}

type incrementalPreparedPlanRegistrar interface {
	RegisterPreparedProfile(rendercontext.PreparedPlanProfile) (string, error)
	RegisterPreparedBackend(*rendercontext.PreparedPlanBackend) (string, error)
}

var _ templating.IncrementalBackendPlanRegistrar = (*incrementalBackendPlanRecorder)(nil)

func newIncrementalBackendPlanRecorder() *incrementalBackendPlanRecorder {
	return &incrementalBackendPlanRecorder{
		tokens: make(map[string]uint32),
	}
}

func (r *incrementalBackendPlanRecorder) Profile(record map[string]any) (string, error) {
	release, err := beginIncrementalCapability(r.lease, "planRegistry.Profile")
	if err != nil {
		return "", err
	}
	defer release()
	prepared, err := rendercontext.PreparePlanProfile(record)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	prepared = prepared.Clone()
	r.calls = append(r.calls, incrementalBackendPlanCall{Profile: &prepared})
	return prepared.Name, nil
}

func (r *incrementalBackendPlanRecorder) Backend(record map[string]any, text string) (string, error) {
	release, err := beginIncrementalCapability(r.lease, "planRegistry.Backend")
	if err != nil {
		return "", err
	}
	defer release()
	return r.backend(record, text, nil)
}

func (r *incrementalBackendPlanRecorder) BackendWhenAny(
	record map[string]any,
	text, cell string,
	keys []string,
) (string, error) {
	release, err := beginIncrementalCapability(r.lease, "planRegistry.BackendWhenAny")
	if err != nil {
		return "", err
	}
	defer release()
	condition, err := prepareIncrementalBackendPlanCondition(cell, keys)
	if err != nil {
		return "", err
	}
	return r.backend(record, text, condition)
}

func (r *incrementalBackendPlanRecorder) backend(
	record map[string]any,
	text string,
	condition *incrementalBackendPlanCondition,
) (string, error) {
	prepared, err := rendercontext.PreparePlanBackend(record, text)
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	callCount := uint64(len(r.calls))
	if callCount > math.MaxUint32 {
		return "", errors.New("incremental backend plan has too many operations")
	}
	index := uint32(callCount)
	prepared = prepared.Clone()
	r.calls = append(r.calls, incrementalBackendPlanCall{
		Policy:   incrementalBackendPlanFirst,
		Identity: prepared.Backend.Name,
		Backend:  &prepared,
		WhenAny:  cloneIncrementalBackendPlanCondition(condition),
	})
	token := fmt.Sprintf("# %s%d:%s@\n", incrementalBackendPlanMarker, index, prepared.Backend.Name)
	r.tokens[strings.TrimSpace(token)] = index
	return token, nil
}

func prepareIncrementalBackendPlanCondition(
	cell string,
	keys []string,
) (*incrementalBackendPlanCondition, error) {
	if cell == "" || len(keys) == 0 {
		return nil, errors.New("BackendWhenAny requires a non-empty cell and keys")
	}
	canonical := slices.Clone(keys)
	for _, key := range canonical {
		if key == "" {
			return nil, errors.New("BackendWhenAny keys must be non-empty")
		}
	}
	slices.Sort(canonical)
	canonical = slices.Compact(canonical)
	return &incrementalBackendPlanCondition{Cell: cell, Keys: canonical}, nil
}

func (r *incrementalBackendPlanRecorder) freeze(
	text string,
) ([]incrementalBackendPlanCall, []incrementalBackendPlanPart, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	calls := cloneIncrementalBackendPlanCalls(r.calls)
	for profileIndex := range calls {
		profile := calls[profileIndex].Profile
		if profile == nil {
			continue
		}
		for backendIndex := range calls {
			backend := calls[backendIndex].Backend
			if backend != nil && backend.Backend.Profile == profile.Name {
				calls[profileIndex].Owners = append(calls[profileIndex].Owners, uint32(backendIndex))
			}
		}
	}
	parts, err := r.outputParts(text)
	if err != nil {
		return nil, nil, err
	}
	return calls, parts, nil
}

func (r *incrementalBackendPlanRecorder) outputParts(text string) ([]incrementalBackendPlanPart, error) {
	var parts []incrementalBackendPlanPart
	for _, line := range strings.SplitAfter(text, "\n") {
		if line == "" {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if index, exists := r.tokens[trimmed]; exists {
			call := index
			parts = append(parts, incrementalBackendPlanPart{BackendCall: &call})
			continue
		}
		if strings.Contains(line, incrementalBackendPlanMarker) {
			return nil, fmt.Errorf("incremental backend plan emitted malformed token %q", strings.TrimSpace(line))
		}
		if len(parts) > 0 && parts[len(parts)-1].BackendCall == nil {
			parts[len(parts)-1].Text += line
		} else {
			parts = append(parts, incrementalBackendPlanPart{Text: line})
		}
	}
	return parts, nil
}

func validateIncrementalBackendPlanResult(result *incrementalComponentResult) error {
	hasPayload := len(result.BackendPlan) != 0 || len(result.BackendPlanOutput) != 0
	hasPlan := hasPayload || result.BackendPlanDigest != ""
	if hasPlan && (result.Text != "" || len(result.Unique) != 0) {
		return errors.New("incremental component cannot mix backendPlan with text or shared.Unique contributions")
	}
	if !hasPayload {
		if result.BackendPlanDigest != "" {
			return errors.New("backendPlan digest has no effect payload")
		}
		return nil
	}
	for index := range result.BackendPlan {
		if err := validateIncrementalBackendPlanCall(result, index); err != nil {
			return err
		}
	}
	if err := validateIncrementalBackendPlanOutput(result); err != nil {
		return err
	}
	digest, err := digestIncrementalBackendPlan(result.BackendPlan, result.BackendPlanOutput)
	if err != nil {
		return err
	}
	if result.BackendPlanDigest == "" || result.BackendPlanDigest != digest {
		return errors.New("backendPlan effect has an invalid digest")
	}
	return nil
}

func digestIncrementalBackendPlan(
	calls []incrementalBackendPlanCall,
	output []incrementalBackendPlanPart,
) (string, error) {
	payload := struct {
		Calls  []incrementalBackendPlanCall `json:"calls"`
		Output []incrementalBackendPlanPart `json:"output"`
	}{Calls: calls, Output: output}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encoding backendPlan effect: %w", err)
	}
	return renderplan.Digest(encoded), nil
}

func validateIncrementalBackendPlanCall(result *incrementalComponentResult, index int) error {
	call := &result.BackendPlan[index]
	if (call.Profile == nil) == (call.Backend == nil) {
		return fmt.Errorf("backendPlan operation %d must declare exactly one profile or backend", index)
	}
	if call.Profile != nil {
		return validateIncrementalBackendPlanProfileCall(result, index)
	}
	if len(call.Owners) != 0 {
		return fmt.Errorf("backendPlan backend operation %d has profile owners", index)
	}
	if err := validateIncrementalBackendPlanCondition(result, index, call.WhenAny); err != nil {
		return err
	}
	if call.Policy != incrementalBackendPlanFirst {
		return fmt.Errorf("backendPlan backend operation %d has unsupported policy %q", index, call.Policy)
	}
	if err := call.Backend.Validate(); err != nil {
		return fmt.Errorf("backendPlan backend operation %d: %w", index, err)
	}
	if call.Identity != call.Backend.Backend.Name {
		return fmt.Errorf("backendPlan backend operation %d identity does not match its declaration", index)
	}
	return nil
}

func validateIncrementalBackendPlanCondition(
	result *incrementalComponentResult,
	callIndex int,
	condition *incrementalBackendPlanCondition,
) error {
	if condition == nil {
		return nil
	}
	if condition.Cell == "" || len(condition.Keys) == 0 {
		return fmt.Errorf("backendPlan backend operation %d has an invalid condition", callIndex)
	}
	previous := ""
	for keyIndex, key := range condition.Keys {
		if key == "" || keyIndex > 0 && key <= previous {
			return fmt.Errorf("backendPlan backend operation %d condition keys are not canonical", callIndex)
		}
		previous = key
		found := false
		for publicationIndex := range result.Published {
			publication := &result.Published[publicationIndex]
			if publication.Cell == condition.Cell && publication.Key == key {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("backendPlan backend operation %d references unpublished value %q/%q", callIndex, condition.Cell, key)
		}
	}
	return nil
}

func validateIncrementalBackendPlanProfileCall(result *incrementalComponentResult, index int) error {
	call := &result.BackendPlan[index]
	if call.Policy != "" || call.Identity != "" {
		return fmt.Errorf("backendPlan profile operation %d has backend arbitration fields", index)
	}
	if call.WhenAny != nil {
		return fmt.Errorf("backendPlan profile operation %d has a backend condition", index)
	}
	if err := call.Profile.Validate(); err != nil {
		return fmt.Errorf("backendPlan profile operation %d: %w", index, err)
	}
	matchingOwners := 0
	for candidateIndex := range result.BackendPlan {
		backend := result.BackendPlan[candidateIndex].Backend
		if backend != nil && backend.Backend.Profile == call.Profile.Name {
			matchingOwners++
		}
	}
	if len(call.Owners) != matchingOwners {
		return fmt.Errorf("backendPlan profile operation %d owners do not match its backends", index)
	}
	previous := uint32(0)
	for ownerIndex, owner := range call.Owners {
		if int(owner) >= len(result.BackendPlan) || result.BackendPlan[owner].Backend == nil ||
			result.BackendPlan[owner].Backend.Backend.Profile != call.Profile.Name {
			return fmt.Errorf("backendPlan profile operation %d has invalid owner %d", index, owner)
		}
		if ownerIndex > 0 && owner <= previous {
			return fmt.Errorf("backendPlan profile operation %d owners are not canonical", index)
		}
		previous = owner
	}
	return nil
}

func validateIncrementalBackendPlanOutput(result *incrementalComponentResult) error {
	for index := range result.BackendPlanOutput {
		part := &result.BackendPlanOutput[index]
		if (part.Text == "") == (part.BackendCall == nil) {
			return fmt.Errorf("backendPlan output part %d must contain exactly one text or backend reference", index)
		}
		if part.BackendCall == nil {
			continue
		}
		call := int(*part.BackendCall)
		if call >= len(result.BackendPlan) || result.BackendPlan[call].Backend == nil {
			return fmt.Errorf("backendPlan output part %d references invalid backend operation %d", index, call)
		}
	}
	return nil
}

func replayIncrementalBackendPlans(
	instances []incrementalBackendPlanInstance,
	registry incrementalPreparedPlanRegistrar,
) (map[string]map[string]string, error) {
	return replayIncrementalBackendPlansWithPublications(instances, nil, registry)
}

func replayIncrementalBackendPlansWithPublications(
	instances []incrementalBackendPlanInstance,
	publicationOwners incrementalBackendPlanPublicationOwners,
	registry incrementalPreparedPlanRegistrar,
) (map[string]map[string]string, error) {
	ordered := orderedIncrementalBackendPlanInstances(instances)
	selection, err := selectIncrementalBackendPlanWinners(ordered, publicationOwners)
	if err != nil {
		return nil, err
	}
	if err := registerIncrementalBackendPlanProfiles(ordered, registry, selection.profileCalls); err != nil {
		return nil, err
	}
	outputs := make(map[string]map[string]*strings.Builder)
	for _, instance := range ordered {
		if err := replayIncrementalBackendPlanInstance(
			instance, registry, selection.winningCalls, outputs,
		); err != nil {
			return nil, err
		}
	}
	return materializeIncrementalBackendPlanOutputs(outputs), nil
}

func orderedIncrementalBackendPlanInstances(
	instances []incrementalBackendPlanInstance,
) []*incrementalBackendPlanInstance {
	ordered := make([]*incrementalBackendPlanInstance, len(instances))
	for index := range instances {
		ordered[index] = &instances[index]
	}
	slices.SortFunc(ordered, func(left, right *incrementalBackendPlanInstance) int {
		return compareIncrementalInstances(&left.incrementalInstanceResult, &right.incrementalInstanceResult)
	})
	return ordered
}

func selectIncrementalBackendPlanWinners(
	instances []*incrementalBackendPlanInstance,
	publicationOwners incrementalBackendPlanPublicationOwners,
) (incrementalBackendPlanSelection, error) {
	selector := incrementalBackendPlanSelector{
		winnerIdentities:   make(map[string]struct{}),
		winningCalls:       make(map[incrementalBackendPlanLocation]struct{}),
		requiredProfiles:   make(map[string]incrementalBackendPlanProfileRequirement),
		profiles:           make(map[string]incrementalBackendPlanProfileCandidate),
		standaloneProfiles: make(map[string]struct{}),
	}
	for _, instance := range instances {
		if err := validateIncrementalBackendPlanInstance(&instance.result); err != nil {
			return incrementalBackendPlanSelection{}, incrementalInstanceError(&instance.incrementalInstanceResult, err)
		}
		for callIndex := range instance.result.BackendPlan {
			if !incrementalBackendPlanCallReachable(instance, callIndex, publicationOwners) {
				continue
			}
			if err := selector.add(instance, callIndex); err != nil {
				return incrementalBackendPlanSelection{}, err
			}
		}
	}
	return selector.selection()
}

func incrementalBackendPlanCallReachable(
	instance *incrementalBackendPlanInstance,
	callIndex int,
	publicationOwners incrementalBackendPlanPublicationOwners,
) bool {
	call := &instance.result.BackendPlan[callIndex]
	if call.Profile != nil || call.WhenAny == nil {
		return true
	}
	owners := publicationOwners[instance.group]
	id := incrementalGroupInstanceID{
		component: instance.component,
		source:    instance.source,
		namespace: instance.namespace,
		name:      instance.name,
	}
	for _, key := range call.WhenAny.Keys {
		owner, exists := owners[string(incrementalPublicationIdentityKey(call.WhenAny.Cell, key))]
		if exists && owner == id {
			return true
		}
	}
	return false
}

func validateIncrementalBackendPlanInstance(result *incrementalComponentResult) error {
	if result.Text != "" || len(result.Unique) != 0 {
		return errors.New("backendPlan component has nonlogical output")
	}
	return validateIncrementalInstanceResult(result)
}

func (s *incrementalBackendPlanSelector) add(instance *incrementalBackendPlanInstance, callIndex int) error {
	call := &instance.result.BackendPlan[callIndex]
	location := incrementalBackendPlanCallLocation(instance, callIndex)
	if call.Profile != nil {
		return s.addProfile(instance, location, call)
	}
	if _, exists := s.winnerIdentities[call.Identity]; exists {
		return nil
	}
	s.winnerIdentities[call.Identity] = struct{}{}
	s.winningCalls[location] = struct{}{}
	if call.Backend.Backend.Profile != "" {
		s.requiredProfiles[call.Backend.Backend.Profile] = incrementalBackendPlanProfileRequirement{instance: instance}
	}
	return nil
}

func (s *incrementalBackendPlanSelector) addProfile(
	instance *incrementalBackendPlanInstance,
	location incrementalBackendPlanLocation,
	call *incrementalBackendPlanCall,
) error {
	previous, exists := s.profiles[call.Profile.Name]
	if exists && previous.profile.Text != call.Profile.Text {
		return incrementalInstanceError(
			&instance.incrementalInstanceResult,
			fmt.Errorf("backendPlan profile %q declared twice with different text", call.Profile.Name),
		)
	}
	if !exists {
		s.profiles[call.Profile.Name] = incrementalBackendPlanProfileCandidate{
			location: location,
			profile:  call.Profile,
		}
	}
	if len(call.Owners) == 0 {
		s.standaloneProfiles[call.Profile.Name] = struct{}{}
	}
	return nil
}

func (s *incrementalBackendPlanSelector) selection() (incrementalBackendPlanSelection, error) {
	profileCalls := make(map[incrementalBackendPlanLocation]struct{})
	for profile := range s.standaloneProfiles {
		profileCalls[s.profiles[profile].location] = struct{}{}
	}
	requiredNames := make([]string, 0, len(s.requiredProfiles))
	for name := range s.requiredProfiles {
		requiredNames = append(requiredNames, name)
	}
	slices.Sort(requiredNames)
	for _, name := range requiredNames {
		requirement := s.requiredProfiles[name]
		candidate, exists := s.profiles[name]
		if !exists {
			return incrementalBackendPlanSelection{}, incrementalInstanceError(
				&requirement.instance.incrementalInstanceResult,
				fmt.Errorf("backendPlan winning backend references undeclared profile %q", name),
			)
		}
		profileCalls[candidate.location] = struct{}{}
	}
	return incrementalBackendPlanSelection{winningCalls: s.winningCalls, profileCalls: profileCalls}, nil
}

func registerIncrementalBackendPlanProfiles(
	instances []*incrementalBackendPlanInstance,
	registry incrementalPreparedPlanRegistrar,
	profileCalls map[incrementalBackendPlanLocation]struct{},
) error {
	for _, instance := range instances {
		for callIndex := range instance.result.BackendPlan {
			location := incrementalBackendPlanCallLocation(instance, callIndex)
			if _, selected := profileCalls[location]; !selected {
				continue
			}
			profile := instance.result.BackendPlan[callIndex].Profile.Clone()
			if _, err := registry.RegisterPreparedProfile(profile); err != nil {
				return incrementalInstanceError(&instance.incrementalInstanceResult, err)
			}
		}
	}
	return nil
}

func replayIncrementalBackendPlanInstance(
	instance *incrementalBackendPlanInstance,
	registry incrementalPreparedPlanRegistrar,
	winningCalls map[incrementalBackendPlanLocation]struct{},
	outputs map[string]map[string]*strings.Builder,
) error {
	tokens := make(map[uint32]string)
	for callIndex := range instance.result.BackendPlan {
		call := &instance.result.BackendPlan[callIndex]
		location := incrementalBackendPlanCallLocation(instance, callIndex)
		if call.Profile != nil {
			continue
		}
		if _, winner := winningCalls[location]; !winner {
			continue
		}
		backend := call.Backend.Clone()
		token, err := registry.RegisterPreparedBackend(&backend)
		if err != nil {
			return incrementalInstanceError(&instance.incrementalInstanceResult, err)
		}
		tokens[uint32(callIndex)] = token
	}
	byComponent := outputs[instance.group]
	if byComponent == nil {
		byComponent = make(map[string]*strings.Builder)
		outputs[instance.group] = byComponent
	}
	builder := byComponent[instance.component]
	if builder == nil {
		builder = &strings.Builder{}
		byComponent[instance.component] = builder
	}
	for _, part := range instance.result.BackendPlanOutput {
		if part.BackendCall == nil {
			builder.WriteString(part.Text)
			continue
		}
		builder.WriteString(tokens[*part.BackendCall])
	}
	return nil
}

func materializeIncrementalBackendPlanOutputs(
	outputs map[string]map[string]*strings.Builder,
) map[string]map[string]string {
	materialized := make(map[string]map[string]string, len(outputs))
	for group, components := range outputs {
		materialized[group] = make(map[string]string, len(components))
		for component, output := range components {
			materialized[group][component] = output.String()
		}
	}
	return materialized
}

func (s *incrementalRenderState) backendPlanGroups() []string {
	var groups []string
	for group, components := range s.groups {
		for index := range components {
			if components[index].backendPlan {
				groups = append(groups, group)
				break
			}
		}
	}
	slices.Sort(groups)
	return groups
}

func validateIncrementalBackendPlanScope(component *incrementalComponent, scope string) error {
	if component.backendPlan && scope != names.MainTemplateName {
		return fmt.Errorf("incremental component %q backendPlan effect must render in %q", component.name, names.MainTemplateName)
	}
	return nil
}

func (r *incrementalRenderSession) prepareBackendPlans(ctx context.Context) error {
	groups := r.state.backendPlanGroups()
	for _, group := range groups {
		if err := r.evaluateGroup(ctx, group); err != nil {
			return err
		}
	}
	if err := r.finalizePreparedPlanBootstrap(); err != nil {
		return err
	}
	registry, err := incrementalPreparedBackendPlanRegistry(r.baseContext)
	if err != nil {
		return err
	}
	if err := r.preparedPlan.prepareRegistry(groups, r.groupIndexes, r.results.Root(), registry); err != nil {
		if auditErr := r.verifyBackendPlanCache(groups); auditErr != nil {
			return auditErr
		}
		return err
	}
	r.planReady = true
	return nil
}

func (r *incrementalRenderSession) finalizePreparedPlanBootstrap() error {
	if !r.preparedPlanBootstrapPending {
		return nil
	}
	groups := r.state.backendPlanGroups()
	var plan *incrementalPreparedPlan
	var err error
	if r.preparedPlanColdBuilder == nil {
		plan, err = newIncrementalPreparedPlanFromIndexes(
			groups, r.groupIndexes, r.state.components, r.results.Root(), r.state.graph,
		)
	} else {
		plan, _, err = r.preparedPlanColdBuilder.finalize(
			groups, r.groupIndexes, r.state.components, r.results.Root(), r.state.graph,
		)
	}
	if err != nil {
		return err
	}
	r.preparedPlan = plan
	r.preparedPlanColdBuilder = nil
	r.preparedPlanBootstrapPending = false
	return nil
}

func (r *incrementalRenderSession) verifyBackendPlanCache(groups []string) error {
	seen, err := r.verifyBackendPlanResultCache(groups)
	if err != nil {
		return err
	}
	return r.verifyBackendPlanGroupIndexes(groups, seen)
}

func (r *incrementalRenderSession) verifyBackendPlanResultCache(
	groups []string,
) (map[string]struct{}, error) {
	planGroups := make(map[string]struct{}, len(groups))
	for _, group := range groups {
		planGroups[group] = struct{}{}
	}
	seen := make(map[string]struct{})
	var walkErr error
	r.results.Root().Walk(func(key []byte, encoded incremental.ExactValueRoot) bool {
		identity, ok := parseResultKey(key)
		if !ok {
			walkErr = fmt.Errorf("incremental result cache has invalid key %s", incremental.DescribeTuple(string(key)))
			return true
		}
		component, exists := r.state.components[identity.component]
		if !exists {
			if _, planGroup := planGroups[identity.group]; planGroup {
				walkErr = fmt.Errorf("incremental backendPlan result %s has an unknown component", incremental.DescribeTuple(string(key)))
				return true
			}
			return false
		}
		if _, planGroup := planGroups[identity.group]; !planGroup {
			return false
		}
		if component.group != identity.group {
			walkErr = fmt.Errorf("incremental backendPlan result %s has the wrong group", incremental.DescribeTuple(string(key)))
			return true
		}
		if err := r.verifyGroupIndexResult(
			&component, identity.source, identity.namespace, identity.name, encoded, true, key,
		); err != nil {
			walkErr = err
			return true
		}
		seen[string(key)] = struct{}{}
		return false
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return seen, nil
}

func (r *incrementalRenderSession) verifyBackendPlanGroupIndexes(
	groups []string,
	seen map[string]struct{},
) error {
	for _, group := range groups {
		var walkErr error
		index := r.groupIndexes[group]
		if index == nil {
			return fmt.Errorf("incremental group %q has no assembly index", group)
		}
		if err := index.validateAuthentication(); err != nil {
			return err
		}
		index.instances.Root().Walk(func(_ string, instance incrementalIndexedGroupInstance) bool {
			component, exists := r.state.components[instance.id.component]
			if !exists || component.group != group {
				walkErr = fmt.Errorf("incremental group %q assembly index has an invalid component", group)
				return true
			}
			key := resultKey(&component, instance.id.source, instance.id.namespace, instance.id.name)
			if _, exists := seen[string(key)]; !exists {
				walkErr = fmt.Errorf("incremental group %q assembly index does not match its result cache", group)
				return true
			}
			return false
		})
		if walkErr != nil {
			return walkErr
		}
	}
	return nil
}

func incrementalBackendPlanRegistry(
	baseContext map[string]any,
) (incrementalPreparedPlanRegistrar, error) {
	registry, ok := baseContext[incrementalPlanRegistryContextName].(incrementalPreparedPlanRegistrar)
	if !ok || registry == nil {
		return nil, errors.New("incremental backend plan registry is unavailable")
	}
	return registry, nil
}

func incrementalPreparedBackendPlanRegistry(
	baseContext map[string]any,
) (incrementalPreparedPlanRegistry, error) {
	registry, ok := baseContext[incrementalPlanRegistryContextName].(incrementalPreparedPlanRegistry)
	if !ok || registry == nil {
		return nil, errors.New("incremental prepared backend plan registry is unavailable")
	}
	return registry, nil
}

func fillMissingIncrementalBackendPlanOutputs(
	state *incrementalRenderState,
	groups []string,
	outputs map[string]map[string]string,
) {
	for _, group := range groups {
		if outputs[group] == nil {
			outputs[group] = map[string]string{}
		}
		components := state.groups[group]
		for index := range components {
			component := &components[index]
			if !component.backendPlan {
				continue
			}
			if _, exists := outputs[group][component.name]; !exists {
				outputs[group][component.name] = ""
			}
		}
	}
}

func (r *incrementalRenderSession) incrementalOutputTextFragment(
	group string,
	component *incrementalComponent,
) (templating.TextFragment, error) {
	if component.backendPlan && r.planReady {
		registry, err := incrementalPreparedBackendPlanRegistry(r.baseContext)
		if err != nil {
			return nil, err
		}
		output, err := r.preparedPlan.outputFragment(group, component.name, r.results.Root(), registry)
		if err != nil {
			return nil, err
		}
		return output, nil
	}
	index := r.groupIndexes[group]
	if index == nil {
		return nil, fmt.Errorf("incremental group %q has no assembly index", group)
	}
	return index.outputContent(component.name)
}

func (r *coldIncrementalRenderer) prepareBackendPlans(ctx context.Context) error {
	groups := r.state.backendPlanGroups()
	instances, err := r.collectBackendPlanInstances(ctx, groups)
	if err != nil {
		return err
	}
	publicationOwners, err := incrementalBackendPlanPublicationOwnersForGroups(groups, r.groupIndexes)
	if err != nil {
		return err
	}
	registry, err := incrementalBackendPlanRegistry(r.baseContext)
	if err != nil {
		return err
	}
	outputs, err := replayIncrementalBackendPlansWithPublications(instances, publicationOwners, registry)
	if err != nil {
		return err
	}
	fillMissingIncrementalBackendPlanOutputs(r.state, groups, outputs)
	r.applyBackendPlanOutputs(groups, outputs)
	r.backendPlanReady = true
	return nil
}

func incrementalBackendPlanPublicationOwnersForGroups(
	groups []string,
	indexes map[string]*incrementalGroupIndex,
) (incrementalBackendPlanPublicationOwners, error) {
	owners := make(incrementalBackendPlanPublicationOwners, len(groups))
	for _, group := range groups {
		index := indexes[group]
		winners, err := index.allPublishedWinners()
		if err != nil {
			return nil, fmt.Errorf("incremental group %q: %w", group, err)
		}
		byIdentity := make(map[string]incrementalGroupInstanceID, len(winners))
		for winnerIndex := range winners {
			winner := &winners[winnerIndex]
			byIdentity[string(incrementalPublicationIdentityKey(winner.value.Cell, winner.value.Key))] = winner.instance
		}
		owners[group] = byIdentity
	}
	return owners, nil
}

func (r *coldIncrementalRenderer) collectBackendPlanInstances(
	ctx context.Context,
	groups []string,
) ([]incrementalBackendPlanInstance, error) {
	instances := make([]incrementalBackendPlanInstance, 0)
	for _, group := range groups {
		resolved, err := r.resolveBackendPlanGroup(ctx, group)
		if err != nil {
			return nil, err
		}
		instances = r.appendColdBackendPlanInstances(instances, group, resolved)
	}
	return instances, nil
}

func (r *coldIncrementalRenderer) resolveBackendPlanGroup(
	ctx context.Context,
	group string,
) ([]incrementalInstanceResult, error) {
	resolved, err := r.resolveGroupInstances(ctx, group)
	if err != nil {
		return nil, err
	}
	if _, err := r.renderGroup(ctx, group); err != nil {
		return nil, err
	}
	return resolved, nil
}

func (r *coldIncrementalRenderer) appendColdBackendPlanInstances(
	instances []incrementalBackendPlanInstance,
	group string,
	resolved []incrementalInstanceResult,
) []incrementalBackendPlanInstance {
	for index := range resolved {
		instance := &resolved[index]
		component, exists := r.state.components[instance.component]
		if exists && component.backendPlan {
			instances = append(instances, incrementalBackendPlanInstance{
				group:                     group,
				incrementalInstanceResult: *instance,
			})
		}
	}
	return instances
}

func (r *coldIncrementalRenderer) applyBackendPlanOutputs(
	groups []string,
	outputs map[string]map[string]string,
) {
	for _, group := range groups {
		components := r.state.groups[group]
		for index := range components {
			component := &components[index]
			if component.backendPlan {
				r.outputs[group][component.name] = outputs[group][component.name]
			}
		}
	}
}

func incrementalBackendPlanCallLocation(
	instance *incrementalBackendPlanInstance,
	call int,
) incrementalBackendPlanLocation {
	return incrementalBackendPlanLocation{
		component: instance.component,
		source:    instance.source,
		namespace: instance.namespace,
		name:      instance.name,
		call:      call,
	}
}

func cloneIncrementalBackendPlanCalls(source []incrementalBackendPlanCall) []incrementalBackendPlanCall {
	if len(source) == 0 {
		return nil
	}
	cloned := slices.Clone(source)
	for index := range cloned {
		if source[index].Profile != nil {
			profile := source[index].Profile.Clone()
			cloned[index].Profile = &profile
		}
		if source[index].Backend != nil {
			backend := source[index].Backend.Clone()
			cloned[index].Backend = &backend
		}
		cloned[index].Owners = slices.Clone(source[index].Owners)
		cloned[index].WhenAny = cloneIncrementalBackendPlanCondition(source[index].WhenAny)
	}
	return cloned
}

func cloneIncrementalBackendPlanCondition(
	condition *incrementalBackendPlanCondition,
) *incrementalBackendPlanCondition {
	if condition == nil {
		return nil
	}
	return &incrementalBackendPlanCondition{Cell: condition.Cell, Keys: slices.Clone(condition.Keys)}
}

func cloneIncrementalBackendPlanParts(source []incrementalBackendPlanPart) []incrementalBackendPlanPart {
	if len(source) == 0 {
		return nil
	}
	cloned := slices.Clone(source)
	for index := range cloned {
		if source[index].BackendCall != nil {
			call := *source[index].BackendCall
			cloned[index].BackendCall = &call
		}
	}
	return cloned
}
