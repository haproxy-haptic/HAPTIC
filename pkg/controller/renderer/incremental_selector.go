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
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/names"
	"github.com/haproxy-haptic/HAPTIC/pkg/incremental"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

type incrementalSelectorIdentity struct {
	group string
	cell  string
	key   string
	count bool
}

func incrementalSelectorIdentityInputKey(identity incrementalSelectorIdentity) incremental.InputKey {
	if identity.count {
		return incrementalSelectorCountInputKey(identity.group, identity.cell)
	}
	if identity.key == "" {
		return incrementalSelectorValuesInputKey(identity.group, identity.cell)
	}
	return incrementalSelectorInputKey(identity.group, identity.cell, identity.key)
}

func compareBool(left, right bool) int {
	if left == right {
		return 0
	}
	if left {
		return 1
	}
	return -1
}

type incrementalPublicationSelector struct {
	ctx       context.Context
	reader    incremental.Reader
	session   *incrementalRenderSession
	component *incrementalComponent
	lease     *incrementalBatchReaderLease
}

type incrementalPreflightPublicationSelector struct {
	selector *incrementalPublicationSelector
}

func (s *incrementalPublicationSelector) Select(group, cell, key string) (value any, found bool, err error) {
	if s == nil || s.session == nil || s.component == nil || s.reader == nil {
		return nil, false, errors.New("incremental publication selector is unavailable")
	}
	release, err := beginIncrementalCapability(s.lease, "shared.Select")
	if err != nil {
		return nil, false, err
	}
	defer release()
	return s.selectValue(group, cell, key)
}

func (s *incrementalPublicationSelector) selectValue(group, cell, key string) (value any, found bool, err error) {
	if err := validateIncrementalSelectorRequest(s.session.state, s.component, group, cell, key); err != nil {
		return nil, false, err
	}
	scope, _ := templating.IncrementalScope(s.ctx)
	if !s.session.coldGraphProducerAuthorized(s.ctx, group) {
		if err := s.session.requireProducerGroupCall(group, scope); err != nil {
			return nil, false, err
		}
	}
	value, certificate, found, err := s.session.publicationInput(
		s.reader,
		incrementalSelectorInputKey(group, cell, key),
	)
	if err != nil {
		return nil, false, fmt.Errorf(
			"decoding incremental publication %q/%q/%q: %w", group, cell, key, err,
		)
	}
	if !found {
		return nil, false, nil
	}
	if err := templating.RegisterIncrementalImmutableCertificate(s.ctx, certificate); err != nil {
		return nil, false, err
	}
	return value, true, nil
}

func (s *incrementalPublicationSelector) SelectValues(group, cell string) ([]any, error) {
	if s == nil || s.session == nil || s.component == nil || s.reader == nil {
		return nil, errors.New("incremental publication selector is unavailable")
	}
	release, err := beginIncrementalCapability(s.lease, "shared.SelectValues")
	if err != nil {
		return nil, err
	}
	defer release()
	return s.selectValues(group, cell)
}

func (s *incrementalPublicationSelector) selectValues(group, cell string) ([]any, error) {
	if err := validateIncrementalSelectorCellRequest(s.session.state, s.component, group, cell); err != nil {
		return nil, err
	}
	scope, _ := templating.IncrementalScope(s.ctx)
	if !s.session.coldGraphProducerAuthorized(s.ctx, group) {
		if err := s.session.requireProducerGroupCall(group, scope); err != nil {
			return nil, err
		}
	}
	value, certificate, found, err := s.session.publicationInput(
		s.reader,
		incrementalSelectorValuesInputKey(group, cell),
	)
	if err != nil {
		return nil, fmt.Errorf("decoding incremental publication values %q/%q: %w", group, cell, err)
	}
	if !found {
		return []any{}, nil
	}
	values, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("incremental publication values %q/%q must be an array, got %T", group, cell, value)
	}
	if err := templating.RegisterIncrementalImmutableCertificate(s.ctx, certificate); err != nil {
		return nil, err
	}
	return values, nil
}

func (s *incrementalPublicationSelector) Count(group, cell string) (int, error) {
	if s == nil || s.session == nil || s.component == nil || s.reader == nil {
		return 0, errors.New("incremental publication selector is unavailable")
	}
	release, err := beginIncrementalCapability(s.lease, "shared.Count")
	if err != nil {
		return 0, err
	}
	defer release()
	return s.count(group, cell)
}

func (s *incrementalPublicationSelector) count(group, cell string) (int, error) {
	if err := validateIncrementalSelectorCellRequest(s.session.state, s.component, group, cell); err != nil {
		return 0, err
	}
	scope, _ := templating.IncrementalScope(s.ctx)
	if !s.session.coldGraphProducerAuthorized(s.ctx, group) {
		if err := s.session.requireProducerGroupCall(group, scope); err != nil {
			return 0, err
		}
	}
	input, err := s.reader.ExactInput(incrementalSelectorCountInputKey(group, cell))
	if err != nil {
		return 0, err
	}
	return decodeIncrementalSelectorCount(input.Value)
}

func (s *incrementalPreflightPublicationSelector) Select(
	group, cell, key string,
) (value any, found bool, err error) {
	return s.selector.selectValue(group, cell, key)
}

func (s *incrementalPreflightPublicationSelector) SelectValues(group, cell string) ([]any, error) {
	return s.selector.selectValues(group, cell)
}

func (s *incrementalPreflightPublicationSelector) Count(group, cell string) (int, error) {
	return s.selector.count(group, cell)
}

type coldIncrementalPublicationSelector struct {
	ctx       context.Context
	renderer  *coldIncrementalRenderer
	component *incrementalComponent
}

func (s *coldIncrementalPublicationSelector) Select(group, cell, key string) (value any, found bool, err error) {
	if s == nil || s.renderer == nil || s.component == nil {
		return nil, false, errors.New("cold incremental publication selector is unavailable")
	}
	if err := validateIncrementalSelectorRequest(s.renderer.state, s.component, group, cell, key); err != nil {
		return nil, false, err
	}
	scope, _ := templating.IncrementalScope(s.ctx)
	if err := s.renderer.requireProducerGroupCall(group, scope); err != nil {
		return nil, false, err
	}
	index := s.renderer.groupIndexes[group]
	input, winner, err := incrementalSelectorInputWithWinner(index, group, cell, key)
	if err != nil {
		return nil, false, err
	}
	if !input.Found {
		return nil, false, nil
	}
	if s.renderer.publicationGeneration != nil {
		if err := s.renderer.publicationGeneration.authenticateAuthority(s.renderer.publicationAuthority); err != nil {
			return nil, false, err
		}
		var certificate *templating.IncrementalImmutableCertificate
		value, certificate, found, err = s.renderer.publicationGeneration.resolveSelector(
			group, input, winner,
		)
		if err != nil {
			return nil, false, err
		}
		if found {
			if err := templating.RegisterIncrementalImmutableCertificate(s.ctx, certificate); err != nil {
				return nil, false, err
			}
			return value, true, nil
		}
	}
	value, err = decodeResourceValue(input.Value)
	if err != nil {
		return nil, false, fmt.Errorf("decoding cold incremental publication %q/%q/%q: %w", group, cell, key, err)
	}
	if err := templating.RegisterIncrementalImmutableInputs(s.ctx, value); err != nil {
		return nil, false, err
	}
	return value, true, nil
}

func (s *coldIncrementalPublicationSelector) SelectValues(group, cell string) ([]any, error) {
	if s == nil || s.renderer == nil || s.component == nil {
		return nil, errors.New("cold incremental publication selector is unavailable")
	}
	if err := validateIncrementalSelectorCellRequest(s.renderer.state, s.component, group, cell); err != nil {
		return nil, err
	}
	scope, _ := templating.IncrementalScope(s.ctx)
	if err := s.renderer.requireProducerGroupCall(group, scope); err != nil {
		return nil, err
	}
	input, winners, err := incrementalSelectorValuesInputWithWinners(
		s.renderer.groupIndexes[group], group, cell,
	)
	if err != nil {
		return nil, err
	}
	if !input.Found {
		return []any{}, nil
	}
	if s.renderer.publicationGeneration != nil {
		if err := s.renderer.publicationGeneration.authenticateAuthority(s.renderer.publicationAuthority); err != nil {
			return nil, err
		}
		values, certificate, resolved, resolveErr := s.renderer.publicationGeneration.resolveSelectorValues(
			s.renderer.groupIndexes[group], group, input, winners,
		)
		if resolveErr != nil {
			return nil, resolveErr
		}
		if resolved {
			if err := templating.RegisterIncrementalImmutableCertificate(s.ctx, certificate); err != nil {
				return nil, err
			}
			return values, nil
		}
	}
	values, err := decodeIncrementalSelectorValues(input.Value)
	if err != nil {
		return nil, fmt.Errorf("decoding cold incremental publication values %q/%q: %w", group, cell, err)
	}
	if err := templating.RegisterIncrementalImmutableInputs(s.ctx, values); err != nil {
		return nil, err
	}
	return values, nil
}

func (s *coldIncrementalPublicationSelector) Count(group, cell string) (int, error) {
	if s == nil || s.renderer == nil || s.component == nil {
		return 0, errors.New("cold incremental publication selector is unavailable")
	}
	if err := validateIncrementalSelectorCellRequest(s.renderer.state, s.component, group, cell); err != nil {
		return 0, err
	}
	scope, _ := templating.IncrementalScope(s.ctx)
	if err := s.renderer.requireProducerGroupCall(group, scope); err != nil {
		return 0, err
	}
	input, err := incrementalSelectorCountInput(s.renderer.groupIndexes[group], group, cell)
	if err != nil {
		return 0, err
	}
	return decodeIncrementalSelectorCount(input.Value)
}

func validateIncrementalSelectorRequest(
	state *incrementalRenderState,
	component *incrementalComponent,
	group, cell, key string,
) error {
	if key == "" {
		return errors.New("shared.Select requires a non-empty key")
	}
	return validateIncrementalSelectorCellRequest(state, component, group, cell)
}

func validateIncrementalSelectorCellRequest(
	state *incrementalRenderState,
	component *incrementalComponent,
	group, cell string,
) error {
	if state == nil || component == nil {
		return errors.New("incremental publication selector has no render state")
	}
	if group == "" || cell == "" {
		return errors.New("shared publication selector requires a non-empty group and cell")
	}
	declared := slices.Contains(component.consumes, group)
	optional := slices.Contains(component.optionalConsumes, group)
	if !declared && !optional {
		return fmt.Errorf("incremental component %q did not declare publication group %q in consumes or optionalConsumes",
			component.name, group)
	}
	components, exists := state.groups[group]
	if !exists {
		if _, absent := state.config.AbsentIncrementalGroups[group]; optional && absent {
			return nil
		}
		return fmt.Errorf("incremental publication group %q is unavailable", group)
	}
	for index := range components {
		if components[index].publishValue {
			return nil
		}
	}
	return fmt.Errorf("incremental publication group %q does not declare publishValue", group)
}

func (r *incrementalRenderSession) requireProducerGroupCall(group, scope string) error {
	components, exists := r.state.groups[group]
	if !exists {
		if _, absent := r.state.config.AbsentIncrementalGroups[group]; absent {
			return nil
		}
		return fmt.Errorf("incremental publication group %q is unavailable", group)
	}
	if !r.requested[group] {
		return fmt.Errorf("incremental publication group %q must complete its canonical root call before selection", group)
	}
	if scope == "" {
		return fmt.Errorf("incremental publication group %q must complete its canonical root call before selection: selection ran outside a root template", group)
	}
	entry, started := r.scopedCalls[incrementalCallScope{group: group, scope: scope}]
	if started && entry.status.complete(len(components)) {
		return nil
	}
	mainStatus := r.scopedCalls[incrementalCallScope{group: group, scope: names.MainTemplateName}].status
	if !started && mainStatus.complete(len(components)) {
		return nil
	}
	if started {
		calls := incrementalCallsInScope(r.scopedCalls, r.calls, group, scope)
		_, err := validateIncrementalGroupCallsInScope(group, scope, components, calls)
		return fmt.Errorf("incremental publication group %q must complete its canonical root call before selection: %w", group, err)
	}
	return fmt.Errorf(
		"incremental publication group %q must complete its canonical root call before selection: neither the current root nor %q has completed a canonical sequence",
		group,
		names.MainTemplateName,
	)
}

func (r *incrementalRenderSession) requireGroupDependencies(group, scope string) error {
	for _, dependency := range r.state.dependencies[group] {
		if err := r.requireProducerGroupCall(dependency, scope); err != nil {
			return fmt.Errorf("incremental group %q dependency: %w", group, err)
		}
	}
	return nil
}

func (r *coldIncrementalRenderer) requireProducerGroupCall(group, scope string) error {
	components, exists := r.state.groups[group]
	if !exists {
		if _, absent := r.state.config.AbsentIncrementalGroups[group]; absent {
			return nil
		}
		return fmt.Errorf("incremental publication group %q is unavailable", group)
	}
	if !r.requested[group] {
		return fmt.Errorf("incremental publication group %q must complete its canonical root call before selection", group)
	}
	if scope == "" {
		return fmt.Errorf("incremental publication group %q must complete its canonical root call before selection: selection ran outside a root template", group)
	}
	entry, started := r.scopedCalls[incrementalCallScope{group: group, scope: scope}]
	if started && entry.status.complete(len(components)) {
		return nil
	}
	mainStatus := r.scopedCalls[incrementalCallScope{group: group, scope: names.MainTemplateName}].status
	if !started && mainStatus.complete(len(components)) {
		return nil
	}
	if started {
		calls := incrementalCallsInScope(r.scopedCalls, r.calls, group, scope)
		_, err := validateIncrementalGroupCallsInScope(group, scope, components, calls)
		return fmt.Errorf("incremental publication group %q must complete its canonical root call before selection: %w", group, err)
	}
	return fmt.Errorf(
		"incremental publication group %q must complete its canonical root call before selection: neither the current root nor %q has completed a canonical sequence",
		group,
		names.MainTemplateName,
	)
}

func (r *coldIncrementalRenderer) requireGroupDependencies(group, scope string) error {
	for _, dependency := range r.state.dependencies[group] {
		if err := r.requireProducerGroupCall(dependency, scope); err != nil {
			return fmt.Errorf("incremental group %q dependency: %w", group, err)
		}
	}
	return nil
}

func incrementalSelectorInputKey(group, cell, key string) incremental.InputKey {
	return incremental.NewInputKey(incremental.EncodeTuple("publication-selector", group, cell, key))
}

func parseIncrementalSelectorInputKey(key incremental.InputKey) (incrementalSelectorIdentity, bool) {
	var parts [3]string
	if !decodeOpaque(key.Opaque(), "publication-selector", parts[:]) {
		return incrementalSelectorIdentity{}, false
	}
	return incrementalSelectorIdentity{group: parts[0], cell: parts[1], key: parts[2]}, true
}

func incrementalSelectorValuesInputKey(group, cell string) incremental.InputKey {
	return incremental.NewInputKey(incremental.EncodeTuple("publication-selector-values", group, cell))
}

func parseIncrementalSelectorValuesInputKey(key incremental.InputKey) (incrementalSelectorIdentity, bool) {
	var parts [2]string
	if !decodeOpaque(key.Opaque(), "publication-selector-values", parts[:]) {
		return incrementalSelectorIdentity{}, false
	}
	return incrementalSelectorIdentity{group: parts[0], cell: parts[1]}, true
}

func incrementalSelectorCountInputKey(group, cell string) incremental.InputKey {
	return incremental.NewInputKey(incremental.EncodeTuple("publication-selector-count", group, cell))
}

func parseIncrementalSelectorCountInputKey(key incremental.InputKey) (incrementalSelectorIdentity, bool) {
	var parts [2]string
	if !decodeOpaque(key.Opaque(), "publication-selector-count", parts[:]) {
		return incrementalSelectorIdentity{}, false
	}
	return incrementalSelectorIdentity{group: parts[0], cell: parts[1], count: true}, true
}

func incrementalSelectorInput(
	index *incrementalGroupIndex,
	group, cell, key string,
) (incremental.Input, error) {
	input, _, err := incrementalSelectorInputWithWinner(index, group, cell, key)
	return input, err
}

func incrementalSelectorInputWithWinner(
	index *incrementalGroupIndex,
	group, cell, key string,
) (incremental.Input, *incrementalPublishedWinner, error) {
	input := incremental.Input{Key: incrementalSelectorInputKey(group, cell, key)}
	if index == nil {
		input.Revision = exactBytesRevision("publication-selector", []byte{0})
		return input, nil, nil
	}
	winner, found, err := index.publishedWinner(cell, key)
	if err != nil {
		return incremental.Input{}, nil, err
	}
	if !found {
		input.Revision = exactBytesRevision("publication-selector", []byte{0})
		return input, nil, nil
	}
	revisionValue := make([]byte, 1, len(winner.value.Value)+1)
	revisionValue[0] = 1
	revisionValue = append(revisionValue, winner.value.Value...)
	input.Revision = exactBytesRevision("publication-selector", revisionValue)
	input.Found = true
	input.Value = slices.Clone(winner.value.Value)
	return input, &winner, nil
}

func incrementalSelectorValuesInput(
	index *incrementalGroupIndex,
	group, cell string,
) (incremental.Input, error) {
	input, _, err := incrementalSelectorValuesInputWithWinners(index, group, cell)
	return input, err
}

func incrementalSelectorValuesInputWithWinners(
	index *incrementalGroupIndex,
	group, cell string,
) (incremental.Input, []incrementalPublishedWinner, error) {
	input := incremental.Input{Key: incrementalSelectorValuesInputKey(group, cell)}
	if index == nil {
		input.Revision = exactBytesRevision("publication-selector-values-absent", nil)
		return input, nil, nil
	}
	winners, err := index.publishedWinners(cell)
	if err != nil {
		return incremental.Input{}, nil, err
	}
	encoded := encodeIncrementalSelectorWinnerValues(winners)
	revisionValue := make([]byte, 1, len(encoded)+1)
	revisionValue[0] = 1
	revisionValue = append(revisionValue, encoded...)
	input.Revision = exactBytesRevision("publication-selector-values", revisionValue)
	input.Found = true
	input.Value = encoded
	return input, winners, nil
}

func encodeIncrementalSelectorWinnerValues(winners []incrementalPublishedWinner) []byte {
	size := 2
	for index := range winners {
		size += len(winners[index].value.Value)
		if index != 0 {
			size++
		}
	}
	encoded := make([]byte, 0, size)
	encoded = append(encoded, '[')
	for index := range winners {
		if index != 0 {
			encoded = append(encoded, ',')
		}
		encoded = append(encoded, winners[index].value.Value...)
	}
	return append(encoded, ']')
}

func incrementalSelectorCountInput(
	index *incrementalGroupIndex,
	group, cell string,
) (incremental.Input, error) {
	input := incremental.Input{Key: incrementalSelectorCountInputKey(group, cell), Found: true}
	count := 0
	if index != nil {
		var err error
		count, err = index.publishedWinnerCount(cell)
		if err != nil {
			return incremental.Input{}, err
		}
	}
	value := make([]byte, 8)
	binary.BigEndian.PutUint64(value, uint64(count))
	input.Revision = exactBytesRevision("publication-selector-count", value)
	input.Value = value
	return input, nil
}

func decodeIncrementalSelectorCount(encoded []byte) (int, error) {
	if len(encoded) != 8 {
		return 0, errors.New("incremental publication count payload is invalid")
	}
	count := binary.BigEndian.Uint64(encoded)
	if count > uint64(^uint(0)>>1) {
		return 0, errors.New("incremental publication count overflows int")
	}
	return int(count), nil
}

func decodeIncrementalSelectorValues(encoded []byte) ([]any, error) {
	decoded, err := decodeResourceValue(encoded)
	if err != nil {
		return nil, err
	}
	values, ok := decoded.([]any)
	if !ok {
		return nil, fmt.Errorf("selector payload must be an array, got %T", decoded)
	}
	return values, nil
}

func incrementalSelectorInputForIdentity(
	index *incrementalGroupIndex,
	identity incrementalSelectorIdentity,
) (incremental.Input, error) {
	if identity.count {
		return incrementalSelectorCountInput(index, identity.group, identity.cell)
	}
	if identity.key == "" {
		return incrementalSelectorValuesInput(index, identity.group, identity.cell)
	}
	return incrementalSelectorInput(index, identity.group, identity.cell, identity.key)
}

func (r *incrementalRenderSession) stageIncrementalSelectorReplacement(
	group string,
	previous, next *incrementalGroupIndex,
	id incrementalGroupInstanceID,
	nextResult *incrementalComponentResult,
) error {
	return stageIncrementalSelectorReplacementInto(
		r.selectorPending, r.sessionHasInputDependents, group, previous, next, id, nextResult,
	)
}

// sessionHasInputDependents answers for the generation this session began on
// plus what it staged. The graph's current generation can be a commit ahead
// of the session's base; judged there, a cell whose last reader that commit
// removed would keep its base value in this session while its index moved on.
func (r *incrementalRenderSession) sessionHasInputDependents(key incremental.InputKey) (bool, error) {
	if r.graphSession != nil {
		return r.graphSession.HasInputDependents(key)
	}
	return r.state.graph.HasInputDependents(key), nil
}

func stageIncrementalSelectorReplacementInto(
	pending map[incrementalSelectorIdentity]incremental.Input,
	hasInputDependents func(incremental.InputKey) (bool, error),
	group string,
	previous, next *incrementalGroupIndex,
	id incrementalGroupInstanceID,
	nextResult *incrementalComponentResult,
) error {
	identities, err := incrementalChangedPublicationIdentities(previous, id, nextResult)
	if err != nil {
		return err
	}
	for _, identity := range identities {
		identity.group = group
		if _, exists := pending[identity]; exists {
			continue
		}
		dependents, err := hasInputDependents(incrementalSelectorIdentityInputKey(identity))
		if err != nil {
			return err
		}
		if !dependents {
			continue
		}
		oldInput, err := incrementalSelectorInputForIdentity(previous, identity)
		if err != nil {
			return err
		}
		newInput, err := incrementalSelectorInputForIdentity(next, identity)
		if err != nil {
			return err
		}
		if sameIncrementalInput(oldInput, newInput) {
			continue
		}
		pending[identity] = oldInput
	}
	return nil
}

func incrementalChangedPublicationIdentities(
	index *incrementalGroupIndex,
	id incrementalGroupInstanceID,
	next *incrementalComponentResult,
) ([]incrementalSelectorIdentity, error) {
	identities := make(map[incrementalSelectorIdentity]struct{})
	if index != nil {
		previous, exists := index.instances.Root().Get(incrementalGroupInstanceKey(id))
		if exists {
			result, err := decodeIndexedGroupInstanceResult(&previous)
			if err != nil {
				return nil, err
			}
			for publicationIndex := range result.Published {
				publication := &result.Published[publicationIndex]
				identities[incrementalSelectorIdentity{cell: publication.Cell, key: publication.Key}] = struct{}{}
				identities[incrementalSelectorIdentity{cell: publication.Cell}] = struct{}{}
				identities[incrementalSelectorIdentity{cell: publication.Cell, count: true}] = struct{}{}
			}
		}
	}
	if next != nil {
		for publicationIndex := range next.Published {
			publication := &next.Published[publicationIndex]
			identities[incrementalSelectorIdentity{cell: publication.Cell, key: publication.Key}] = struct{}{}
			identities[incrementalSelectorIdentity{cell: publication.Cell}] = struct{}{}
			identities[incrementalSelectorIdentity{cell: publication.Cell, count: true}] = struct{}{}
		}
	}
	result := make([]incrementalSelectorIdentity, 0, len(identities))
	for identity := range identities {
		result = append(result, identity)
	}
	slices.SortFunc(result, func(left, right incrementalSelectorIdentity) int {
		if order := strings.Compare(left.cell, right.cell); order != 0 {
			return order
		}
		if order := strings.Compare(left.key, right.key); order != 0 {
			return order
		}
		return compareBool(left.count, right.count)
	})
	return result, nil
}

func (r *incrementalRenderSession) applyIncrementalSelectorChanges(group string) error {
	identities := make([]incrementalSelectorIdentity, 0)
	for identity := range r.selectorPending {
		if identity.group == group {
			identities = append(identities, identity)
		}
	}
	slices.SortFunc(identities, func(left, right incrementalSelectorIdentity) int {
		if order := strings.Compare(left.cell, right.cell); order != 0 {
			return order
		}
		if order := strings.Compare(left.key, right.key); order != 0 {
			return order
		}
		return compareBool(left.count, right.count)
	})
	inputs := make([]incremental.Input, 0, len(identities))
	for _, identity := range identities {
		current, err := incrementalSelectorInputForIdentity(r.groupIndexes[group], identity)
		if err != nil {
			return err
		}
		if !sameIncrementalInput(r.selectorPending[identity], current) {
			inputs = append(inputs, current)
		}
	}
	if len(inputs) > 0 {
		if r.graphSession == nil {
			return errors.New("incremental selector changes have no graph session")
		}
		dirty, err := r.graphSession.ApplyInputsWhileIdle(inputs...)
		if err != nil {
			return fmt.Errorf("applying incremental publication selectors for group %q: %w", group, err)
		}
		for _, query := range dirty {
			r.dirtyQueries[query] = struct{}{}
		}
	}
	for _, identity := range identities {
		delete(r.selectorPending, identity)
	}
	return nil
}
