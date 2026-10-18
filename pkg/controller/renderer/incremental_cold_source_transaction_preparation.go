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
	"errors"
	"fmt"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/incremental"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

type incrementalColdSourceTransactionPreparedChild struct {
	description incrementalColdSourceTransactionChild
	reader      incremental.ColdExactBatchQuery
	frame       incrementalColdSourceFrameView
}

type incrementalColdSourceTransactionSharedKey struct {
	source     string
	namespace  string
	name       string
	projection incrementalColdSourceTransactionProjection
}

type incrementalColdSourceTransactionSharedSource struct {
	key                   incrementalColdSourceTransactionSharedKey
	generation            *incrementalColdSourceFrameGeneration
	itemSlot              *incrementalColdSourceInputSlot
	subjectSlot           *incrementalColdSourceInputSlot
	itemInput             *incrementalColdCertifiedSourceInput
	subjectInput          *incrementalColdCertifiedSourceInput
	item                  map[string]any
	itemCertificate       *templating.IncrementalImmutableCertificate
	ownerInput            incremental.Input
	projectionKey         incremental.QueryKey
	projectionObservation incremental.ExactQueryObservation
}

func incrementalColdSourceTransactionSharedKeyFor(
	group *incrementalColdSourceTransactionGroup,
) (incrementalColdSourceTransactionSharedKey, error) {
	if group == nil || group.key.source == "" || group.key.name == "" || group.key.projection == 0 {
		return incrementalColdSourceTransactionSharedKey{}, errors.New(
			"incremental cold source transaction shared source is incomplete",
		)
	}
	return incrementalColdSourceTransactionSharedKey{
		source: group.key.source, namespace: group.key.namespace, name: group.key.name,
		projection: group.key.projection,
	}, nil
}

func (c *incrementalColdCarrierWaveCoordinator) prepareSourceTransactionWorker(
	waveIndex int,
	waveCtx context.Context,
	sourceFrames *incrementalColdSourceFrameGeneration,
	planned *incrementalColdCarrierPlannedWorkerWave,
	arena *incrementalColdResultArena,
) (*incrementalSourceTransactionRender, *incrementalColdCarrierLifecycle, error) {
	if c == nil || waveCtx == nil || sourceFrames == nil || planned == nil || arena == nil {
		return nil, nil, errors.New("incremental cold source transaction worker preparation is incomplete")
	}
	groups, descriptions, err := c.session.coldSourceTransactionGroups(planned)
	if err != nil {
		return nil, nil, err
	}
	if len(descriptions) == 0 {
		if len(groups) != 0 {
			return nil, nil, errors.New("incremental cold source transaction empty worker has groups")
		}
		return nil, nil, nil
	}
	children := make([]*preparedIncrementalComponent, len(descriptions))
	batchIndexes := make([]int, len(descriptions))
	arenaSlots := make([]int, len(descriptions))
	sharedSources := make(map[incrementalColdSourceTransactionSharedKey]*incrementalColdSourceTransactionSharedSource)
	for groupIndex := range groups {
		if err := c.prepareSourceTransactionGroup(
			waveIndex,
			waveCtx,
			sourceFrames,
			&groups[groupIndex],
			descriptions,
			children,
			sharedSources,
		); err != nil {
			return nil, nil, err
		}
	}
	for childIndex := range descriptions {
		description := &descriptions[childIndex]
		if children[childIndex] == nil {
			return nil, nil, fmt.Errorf("incremental cold source transaction omitted child %d", childIndex)
		}
		batchIndex := description.item.batchIndex
		slot, exists := arena.slotForBatchIndex(batchIndex)
		if !exists || arena.keys[slot] != description.item.queryKey {
			return nil, nil, errors.New("incremental cold source transaction result slot changed")
		}
		batchIndexes[childIndex] = batchIndex
		arenaSlots[childIndex] = slot
	}
	render, err := c.session.prepareSourceTransactionRender(
		waveCtx, children, batchIndexes, arenaSlots, groups,
	)
	if err != nil {
		return nil, nil, err
	}
	lifecycle := &incrementalColdCarrierLifecycle{
		segments: []incrementalColdCarrierSegment{{
			start: 0, end: len(children), execution: render.execution,
		}},
		total: len(children), active: -1,
	}
	lifecycle.seal = lifecycle
	return render, lifecycle, nil
}

func (c *incrementalColdCarrierWaveCoordinator) prepareSourceTransactionGroup(
	waveIndex int,
	waveCtx context.Context,
	sourceFrames *incrementalColdSourceFrameGeneration,
	group *incrementalColdSourceTransactionGroup,
	descriptions []incrementalColdSourceTransactionChild,
	children []*preparedIncrementalComponent,
	sharedSources map[incrementalColdSourceTransactionSharedKey]*incrementalColdSourceTransactionSharedSource,
) error {
	if group == nil || len(group.children) == 0 || len(descriptions) != len(children) || sharedSources == nil {
		return errors.New("incremental cold source transaction group is incomplete")
	}
	prepared, err := c.prepareSourceTransactionChildFrames(
		waveIndex, sourceFrames, group, descriptions, children,
	)
	if err != nil {
		return err
	}
	props, propsCertificate, err := authenticateSourceTransactionProps(waveCtx, group, prepared)
	if err != nil {
		return err
	}
	shared, err := c.prepareSourceTransactionSharedSource(waveCtx, group, prepared, sharedSources)
	if err != nil {
		return err
	}
	return c.bindSourceTransactionChildren(
		group, prepared, children, shared, props, propsCertificate,
	)
}

func (c *incrementalColdCarrierWaveCoordinator) prepareSourceTransactionChildFrames(
	waveIndex int,
	sourceFrames *incrementalColdSourceFrameGeneration,
	group *incrementalColdSourceTransactionGroup,
	descriptions []incrementalColdSourceTransactionChild,
	children []*preparedIncrementalComponent,
) ([]incrementalColdSourceTransactionPreparedChild, error) {
	prepared := make([]incrementalColdSourceTransactionPreparedChild, len(group.children))
	for offset, childIndex := range group.children {
		if cause := context.Cause(c.ctx); cause != nil {
			return nil, cause
		}
		if childIndex < 0 || childIndex >= len(descriptions) || children[childIndex] != nil {
			return nil, fmt.Errorf(
				"%w: child %d is invalid", errIncrementalColdSourceTransactionInvariant, childIndex,
			)
		}
		description := descriptions[childIndex]
		query := c.batch.Query(description.item.batchIndex)
		if query.Key() != description.item.queryKey {
			return nil, fmt.Errorf("incremental cold source transaction wave %d query order changed", waveIndex)
		}
		refs, err := sourceFrames.refsFor(
			description.item.batchIndex,
			description.item.queryKey,
			description.component,
			description.item.source,
			description.item.namespace,
			description.item.name,
		)
		if err != nil {
			return nil, attributeToWatchedObject(description.item.queryKey, err)
		}
		frame, err := refs.authenticateDetached(
			description.item.queryKey,
			description.component,
			description.item.source,
			description.item.namespace,
			description.item.name,
		)
		if err != nil {
			return nil, attributeToWatchedObject(description.item.queryKey, err)
		}
		prepared[offset] = incrementalColdSourceTransactionPreparedChild{
			description: description,
			reader:      query,
			frame:       frame,
		}
	}
	return prepared, nil
}

func authenticateSourceTransactionProps(
	waveCtx context.Context,
	group *incrementalColdSourceTransactionGroup,
	prepared []incrementalColdSourceTransactionPreparedChild,
) (props map[string]any, propsCertificate *templating.IncrementalImmutableCertificate, err error) {
	for offset := range prepared {
		child := &prepared[offset]
		expected := bindingInput(incrementalBinding{
			component: child.description.component.name,
			source:    child.description.item.source,
			props:     []byte(group.key.props),
		})
		if child.frame.binding == nil || child.frame.binding.key != expected.Key {
			return nil, nil, fmt.Errorf(
				"%w: child binding changed", errIncrementalColdSourceTransactionInvariant,
			)
		}
		binding, loadErr := child.frame.binding.load(waveCtx, child.reader, child.frame.generation)
		if loadErr != nil {
			return nil, nil, loadErr
		}
		if binding.key != expected.Key || binding.revision != expected.Revision || !binding.found ||
			binding.encoded != group.key.props || binding.value == nil || binding.certificate == nil ||
			!binding.certificate.Guards(binding.value) {
			return nil, nil, fmt.Errorf(
				"%w: child binding changed", errIncrementalColdSourceTransactionInvariant,
			)
		}
		if offset == 0 {
			props = binding.value
			propsCertificate = binding.certificate
			continue
		}
		if binding.certificate != propsCertificate || !sameMapIdentity(binding.value, props) {
			return nil, nil, fmt.Errorf(
				"%w: child bindings do not share one authenticated value",
				errIncrementalColdSourceTransactionInvariant,
			)
		}
	}
	return props, propsCertificate, nil
}

func (c *incrementalColdCarrierWaveCoordinator) bindSourceTransactionChildren(
	group *incrementalColdSourceTransactionGroup,
	prepared []incrementalColdSourceTransactionPreparedChild,
	children []*preparedIncrementalComponent,
	shared *incrementalColdSourceTransactionSharedSource,
	props map[string]any,
	propsCertificate *templating.IncrementalImmutableCertificate,
) error {
	for offset, childIndex := range group.children {
		child := &prepared[offset]
		description := &child.description
		queryKey := c.session.registerComponentQuery(
			description.component,
			description.item.source,
			description.item.namespace,
			description.item.name,
		)
		if queryKey != description.item.queryKey || child.reader.Key() != queryKey {
			return fmt.Errorf("%w: child query changed", errIncrementalColdSourceTransactionInvariant)
		}
		var itemBytes []byte
		if description.component.deriveResource {
			if group.key.projection != incrementalColdSourceTransactionOwner {
				return fmt.Errorf(
					"%w: derive child is not its source owner", errIncrementalColdSourceTransactionInvariant,
				)
			}
			itemBytes = []byte(shared.itemInput.encoded)
		}
		children[childIndex] = &preparedIncrementalComponent{
			queryKey: queryKey, component: description.component, reader: child.reader,
			source: description.item.source, namespace: description.item.namespace, name: description.item.name,
			itemBytes: itemBytes, item: shared.item, itemCertificate: shared.itemCertificate,
			props: props, propsCertificate: propsCertificate,
			renderSubject: shared.subjectInput.value, subjectCertificate: shared.subjectInput.certificate,
		}
	}
	return nil
}

func (c *incrementalColdCarrierWaveCoordinator) prepareSourceTransactionSharedSource(
	waveCtx context.Context,
	group *incrementalColdSourceTransactionGroup,
	prepared []incrementalColdSourceTransactionPreparedChild,
	sharedSources map[incrementalColdSourceTransactionSharedKey]*incrementalColdSourceTransactionSharedSource,
) (*incrementalColdSourceTransactionSharedSource, error) {
	key, err := incrementalColdSourceTransactionSharedKeyFor(group)
	if err != nil {
		return nil, err
	}
	if shared := sharedSources[key]; shared != nil {
		if err := observeIncrementalColdSourceTransactionSharedSource(prepared, shared, 0); err != nil {
			return nil, err
		}
		return shared, nil
	}
	if len(prepared) == 0 {
		return nil, errors.New("incremental cold source transaction shared source has no children")
	}
	first := &prepared[0]
	itemInput, err := first.frame.item.load(waveCtx, first.reader, first.frame.generation)
	if err != nil {
		return nil, err
	}
	if !itemInput.found {
		return nil, fmt.Errorf(
			"incremental cold source transaction query %s became inactive after its shape was sealed",
			first.description.item.queryKey,
		)
	}
	subjectInput, err := first.frame.renderSubject.load(waveCtx, first.reader, first.frame.generation)
	if err != nil {
		return nil, err
	}
	if !subjectInput.found {
		return nil, fmt.Errorf(
			"incremental component %q render subject disappeared",
			first.description.component.name,
		)
	}
	shared := &incrementalColdSourceTransactionSharedSource{
		key: key, generation: first.frame.generation,
		itemSlot: first.frame.item, subjectSlot: first.frame.renderSubject,
		itemInput: itemInput, subjectInput: subjectInput,
		item: itemInput.value, itemCertificate: itemInput.certificate,
	}
	projected := false
	var projectedBytes []byte
	switch key.projection {
	case incrementalColdSourceTransactionRaw, incrementalColdSourceTransactionOwner:
	case incrementalColdSourceTransactionProjected:
		projected = true
		projectedBytes, err = c.projectSourceTransactionSharedItem(waveCtx, key, first, shared)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf(
			"%w: projection class is invalid",
			errIncrementalColdSourceTransactionInvariant,
		)
	}
	shared.item, shared.itemCertificate, err = c.session.authenticateComponentProjection(
		first.description.component.name,
		shared.item,
		projectedBytes,
		shared.itemCertificate,
		projected,
	)
	if err != nil {
		return nil, err
	}
	if err := observeIncrementalColdSourceTransactionSharedSource(prepared, shared, 1); err != nil {
		return nil, err
	}
	sharedSources[key] = shared
	return shared, nil
}

func (c *incrementalColdCarrierWaveCoordinator) projectSourceTransactionSharedItem(
	waveCtx context.Context,
	key incrementalColdSourceTransactionSharedKey,
	first *incrementalColdSourceTransactionPreparedChild,
	shared *incrementalColdSourceTransactionSharedSource,
) ([]byte, error) {
	owner, exists := c.session.bindingPlan.owners[key.source]
	if !exists || !owner.deriveResource {
		return nil, fmt.Errorf(
			"%w: projected source has no derive owner",
			errIncrementalColdSourceTransactionInvariant,
		)
	}
	shared.ownerInput = deriveOwnerInput(key.source, &owner, true)
	if err := observeIncrementalColdSourceTransactionOwner(first.reader, shared.ownerInput); err != nil {
		return nil, err
	}
	shared.projectionKey = derivedProjectionQueryKey(key.source, key.namespace, key.name)
	observer, ok := any(first.reader).(incremental.ExactQueryObserver)
	if !ok {
		return nil, errors.New("incremental cold source transaction requires exact query observations")
	}
	encoded, observation, queryErr := observer.QueryWithExactObservation(waveCtx, shared.projectionKey)
	if queryErr != nil {
		return nil, queryErr
	}
	if err := observation.ValidateFor(shared.projectionKey); err != nil {
		return nil, fmt.Errorf("authenticating incremental source transaction projection: %w", err)
	}
	shared.projectionObservation = observation
	view := rendercontext.NewDerivedResourceView()
	if len(encoded) != 0 {
		entry, decodeErr := decodeDerivedProjection(encoded)
		if decodeErr != nil {
			return nil, fmt.Errorf("decoding incremental source transaction projection: %w", decodeErr)
		}
		if err := view.Replay(&entry); err != nil {
			return nil, fmt.Errorf("replaying incremental source transaction projection: %w", err)
		}
	}
	projectedItem, projectedBytes, err := projectComponentItem(view, key.source, shared.item)
	if err != nil {
		return nil, fmt.Errorf("projecting incremental source transaction item: %w", err)
	}
	shared.item = projectedItem
	return projectedBytes, nil
}

func observeIncrementalColdSourceTransactionSharedSource(
	prepared []incrementalColdSourceTransactionPreparedChild,
	shared *incrementalColdSourceTransactionSharedSource,
	start int,
) error {
	if !incrementalColdSourceTransactionSharedSourceSealed(shared, len(prepared), start) {
		return errors.New("incremental cold source transaction shared source has invalid provenance")
	}
	for offset := range prepared {
		child := &prepared[offset]
		if child.frame.generation != shared.generation || child.frame.item != shared.itemSlot ||
			child.frame.renderSubject != shared.subjectSlot {
			return fmt.Errorf(
				"%w: source input frame changed across props rows",
				errIncrementalColdSourceTransactionInvariant,
			)
		}
	}
	for offset := start; offset < len(prepared); offset++ {
		if err := observeIncrementalColdSourceTransactionChild(prepared[offset].reader, shared); err != nil {
			return err
		}
	}
	return nil
}

func incrementalColdSourceTransactionSharedSourceSealed(
	shared *incrementalColdSourceTransactionSharedSource,
	preparedCount, start int,
) bool {
	return shared != nil && shared.generation != nil && shared.itemSlot != nil &&
		shared.subjectSlot != nil && shared.itemInput != nil && shared.subjectInput != nil &&
		shared.item != nil && shared.itemCertificate != nil &&
		shared.itemCertificate.Guards(shared.item) && start >= 0 && start <= preparedCount
}

func observeIncrementalColdSourceTransactionChild(
	reader incremental.ColdExactBatchQuery,
	shared *incrementalColdSourceTransactionSharedSource,
) error {
	if err := observeIncrementalColdCertifiedSourceInput(reader, shared.itemInput); err != nil {
		return err
	}
	if err := observeIncrementalColdCertifiedSourceInput(reader, shared.subjectInput); err != nil {
		return err
	}
	if shared.key.projection != incrementalColdSourceTransactionProjected {
		return nil
	}
	if err := observeIncrementalColdSourceTransactionOwner(reader, shared.ownerInput); err != nil {
		return err
	}
	observer, ok := any(reader).(incremental.ExactQueryObserver)
	if !ok {
		return errors.New("incremental cold source transaction requires exact query observations")
	}
	if err := observer.ObserveExactQuery(shared.projectionObservation); err != nil {
		return fmt.Errorf("observing incremental source transaction projection: %w", err)
	}
	return nil
}

func observeIncrementalColdSourceTransactionOwner(
	reader incremental.ColdExactBatchQuery,
	owner incremental.Input,
) error {
	observer, ok := any(reader).(incremental.ExactImmutableInputObserver)
	if !ok {
		return errors.New("incremental cold source transaction requires exact owner observations")
	}
	if err := observer.ObserveExactImmutableInput(incremental.ImmutableInput{
		Key: owner.Key, Revision: owner.Revision, Found: owner.Found, Value: string(owner.Value),
	}); err != nil {
		return fmt.Errorf("observing incremental source transaction derive owner: %w", err)
	}
	return nil
}
