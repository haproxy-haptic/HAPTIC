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
	"reflect"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/incremental"
	k8sstore "github.com/haproxy-haptic/HAPTIC/pkg/k8s/store"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/typegen"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

type incrementalResourceMaterializationAuthority struct {
	seal    atomic.Pointer[incrementalResourceMaterializationAuthority]
	revoked atomic.Bool
}

type incrementalResourceMaterializationArena struct {
	seal      *incrementalResourceMaterializationArena
	authority *incrementalResourceMaterializationAuthority
	entries   incrementalDecodedCache[incremental.InputKey, *incrementalResourceMaterialization]
	direct    incrementalDecodedCache[
		incrementalDirectBoundResourceSpecKey,
		*incrementalDirectBoundResourceSpec,
	]
}

const incrementalDirectBoundResourceInlineKeyCount = 4

type incrementalDirectBoundResourceSpecKey struct {
	resourceType string
	operation    rendercontext.DirectBoundResourceOperation
	elementType  reflect.Type
	returnType   reflect.Type
	keys         [incrementalDirectBoundResourceInlineKeyCount]string
	overflow     string
	keyCount     int
}

type incrementalDirectBoundResourceSpec struct {
	seal      *incrementalDirectBoundResourceSpec
	proof     *incrementalDirectBoundResourceSpecProof
	authority *incrementalResourceMaterializationAuthority
	key       incrementalDirectBoundResourceSpecKey
	inputKey  incremental.InputKey
	spec      resourceInputSpec
}

type incrementalDirectBoundResourceSpecProof struct {
	seal      *incrementalDirectBoundResourceSpecProof
	entry     *incrementalDirectBoundResourceSpec
	authority *incrementalResourceMaterializationAuthority
	key       incrementalDirectBoundResourceSpecKey
	inputKey  incremental.InputKey
}

type incrementalResourceMaterialization struct {
	seal         *incrementalResourceMaterialization
	authority    *incrementalResourceMaterializationAuthority
	proof        *incrementalResourceMaterializationProof
	key          incremental.InputKey
	resourceType string
	scope        resourceInputScope
	revision     incremental.Revision
	found        bool
	encoded      string
	encodedHash  uint64
	itemCount    int
	storeValue   *k8sstore.ImmutableSnapshotProjection
	raw          incrementalResourceMaterializationRawState
	projected    incrementalResourceMaterializationProjectedState
	source       stores.RevisionSource
	sequence     uint64
	projection   atomic.Pointer[incrementalDirectResourceProjection]
	directResult atomic.Pointer[incrementalResourceMaterializationDirectResult]
}

type incrementalResourceMaterializationDirectResult struct {
	seal        *incrementalResourceMaterializationDirectResult
	proof       *incrementalResourceMaterializationDirectResultProof
	owner       *incrementalResourceMaterialization
	elementType reflect.Type
	returnType  reflect.Type
	value       reflect.Value
	certificate *templating.IncrementalImmutableCertificate
}

type incrementalResourceMaterializationDirectResultProof struct {
	seal        *incrementalResourceMaterializationDirectResultProof
	result      *incrementalResourceMaterializationDirectResult
	owner       *incrementalResourceMaterialization
	elementType reflect.Type
	returnType  reflect.Type
	value       incrementalResourceMaterializationValueIdentity
	certificate *templating.IncrementalImmutableCertificate
}

type incrementalResourceMaterializationValueIdentity struct {
	typeOf  reflect.Type
	kind    reflect.Kind
	pointer uintptr
	isNil   bool
}

type incrementalResourceMaterializationRawState struct {
	seal  *incrementalResourceMaterializationRawState
	owner *incrementalResourceMaterialization
	mu    sync.Mutex
	value atomic.Pointer[incrementalResourceMaterializationRawItems]
}

type incrementalResourceMaterializationRawItems struct {
	seal        *incrementalResourceMaterializationRawItems
	owner       *incrementalResourceMaterialization
	items       []any
	certificate atomic.Pointer[templating.IncrementalImmutableCertificate]
}

type incrementalResourceMaterializationProjectedState struct {
	seal   *incrementalResourceMaterializationProjectedState
	owner  *incrementalResourceMaterialization
	mu     sync.Mutex
	values map[reflect.Type]*incrementalResourceMaterializationProjectedItems
}

type incrementalResourceMaterializationProjectedItems struct {
	seal        *incrementalResourceMaterializationProjectedItems
	owner       *incrementalResourceMaterialization
	elementType reflect.Type
	values      []reflect.Value
}

type incrementalResourceMaterializationProof struct {
	seal         *incrementalResourceMaterializationProof
	authority    *incrementalResourceMaterializationAuthority
	key          incremental.InputKey
	resourceType string
	scope        resourceInputScope
	revision     incremental.Revision
	found        bool
	encoded      string
	encodedHash  uint64
	itemCount    int
	storeValue   *k8sstore.ImmutableSnapshotProjection
	raw          *incrementalResourceMaterializationRawState
	projected    *incrementalResourceMaterializationProjectedState
	source       stores.RevisionSource
	sequence     uint64
}

func newIncrementalResourceMaterializationArena() *incrementalResourceMaterializationArena {
	authority := &incrementalResourceMaterializationAuthority{}
	authority.seal.Store(authority)
	arena := &incrementalResourceMaterializationArena{authority: authority}
	arena.seal = arena
	return arena
}

func (a *incrementalResourceMaterializationArena) valid() bool {
	return a != nil && a.seal == a && a.authority != nil &&
		!a.authority.revoked.Load() && a.authority.seal.Load() == a.authority
}

func (a *incrementalResourceMaterializationArena) revoke() {
	if a == nil || a.authority == nil || !a.authority.revoked.CompareAndSwap(false, true) {
		return
	}
	a.authority.seal.CompareAndSwap(a.authority, nil)
	a.entries.reset()
	a.direct.reset()
}

func (a *incrementalResourceMaterializationArena) directBoundResourceSpec(
	declaration rendercontext.DirectBoundResourceMaterialization,
	keys []string,
) (*resourceInputSpec, error) {
	if !a.valid() {
		return nil, errors.New("incremental resource materialization arena has invalid provenance")
	}
	if err := declaration.Authenticate(); err != nil {
		return nil, err
	}
	key := newIncrementalDirectBoundResourceSpecKey(declaration, keys)
	hash := key.hash()
	entry, err := a.direct.loadOrCompute(
		key,
		hash,
		func() (*incrementalDirectBoundResourceSpec, error) {
			spec, specErr := directBoundResourceInputSpec(declaration, keys)
			if specErr != nil {
				return nil, specErr
			}
			candidate := &incrementalDirectBoundResourceSpec{
				authority: a.authority,
				key:       key,
				inputKey:  resourceInputKey(&spec),
				spec:      spec,
			}
			candidate.seal = candidate
			candidate.proof = &incrementalDirectBoundResourceSpecProof{
				entry: candidate, authority: a.authority, key: key, inputKey: candidate.inputKey,
			}
			candidate.proof.seal = candidate.proof
			if authenticateErr := candidate.authenticate(a, &key); authenticateErr != nil {
				return nil, authenticateErr
			}
			return candidate, nil
		},
	)
	if err != nil {
		return nil, err
	}
	if err := entry.authenticate(a, &key); err != nil {
		return nil, err
	}
	return &entry.spec, nil
}

func newIncrementalDirectBoundResourceSpecKey(
	declaration rendercontext.DirectBoundResourceMaterialization,
	keys []string,
) incrementalDirectBoundResourceSpecKey {
	key := incrementalDirectBoundResourceSpecKey{
		resourceType: declaration.ResourceType,
		operation:    declaration.Operation,
		elementType:  declaration.ElementType,
		returnType:   declaration.ReturnType,
		keyCount:     len(keys),
	}
	if len(keys) <= len(key.keys) {
		copy(key.keys[:], keys)
		return key
	}
	key.overflow = incremental.EncodeTuple("direct-resource-keys", keys...)
	return key
}

func (k *incrementalDirectBoundResourceSpecKey) hash() uint64 {
	hash := incrementalDecodedCacheStringHash(k.resourceType)
	hash ^= uint64(k.operation)
	hash *= incrementalDecodedCacheHashPrime
	keyCount := k.keyCount
	if keyCount < 0 {
		keyCount = 0
	}
	hash ^= uint64(keyCount)
	hash *= incrementalDecodedCacheHashPrime
	if k.overflow != "" {
		return incrementalDecodedCacheStringHash(k.overflow) ^ hash
	}
	for index := range k.keyCount {
		value := k.keys[index]
		for byteIndex := 0; byteIndex < len(value); byteIndex++ {
			hash ^= uint64(value[byteIndex])
			hash *= incrementalDecodedCacheHashPrime
		}
		hash ^= uint64(len(value))
		hash *= incrementalDecodedCacheHashPrime
	}
	return hash
}

func (e *incrementalDirectBoundResourceSpec) authenticate(
	arena *incrementalResourceMaterializationArena,
	key *incrementalDirectBoundResourceSpecKey,
) error {
	if e == nil || e.seal != e || e.proof == nil || e.proof.seal != e.proof ||
		e.proof.entry != e || arena == nil || !arena.valid() ||
		e.authority != arena.authority || e.proof.authority != e.authority ||
		e.key != *key || e.proof.key != e.key || e.inputKey != e.proof.inputKey ||
		e.inputKey.Opaque() == "" || resourceInputKey(&e.spec) != e.inputKey ||
		!e.key.matches(&e.spec) {
		return errors.New("incremental direct resource spec has invalid provenance")
	}
	return nil
}

func (k *incrementalDirectBoundResourceSpecKey) matches(spec *resourceInputSpec) bool {
	if spec == nil || spec.resourceType != k.resourceType ||
		len(spec.keys) != k.keyCount {
		return false
	}
	switch k.operation {
	case rendercontext.DirectBoundResourceList:
		if spec.scope != resourceInputList {
			return false
		}
	case rendercontext.DirectBoundResourceFetch, rendercontext.DirectBoundResourceGetSingle:
		if spec.scope != resourceInputGet {
			return false
		}
	default:
		return false
	}
	if k.keyCount <= len(k.keys) {
		return slices.Equal(k.keys[:k.keyCount], spec.keys)
	}
	return incrementalDirectBoundResourceOverflowMatches(k.overflow, spec.keys)
}

func incrementalDirectBoundResourceOverflowMatches(encoded string, keys []string) bool {
	return opaqueMatches(encoded, "direct-resource-keys", keys...)
}

func (a *incrementalResourceMaterializationArena) ensure(
	ctx context.Context,
	snapshot stores.ReadSnapshot,
	spec *resourceInputSpec,
) (*incrementalResourceMaterialization, bool, error) {
	if !a.valid() {
		return nil, false, errors.New("incremental resource materialization arena has invalid provenance")
	}
	if snapshot == nil || spec == nil ||
		(spec.scope != resourceInputList && spec.scope != resourceInputGet) {
		return nil, false, nil
	}
	key := resourceInputKey(spec)
	hash := incrementalDecodedCacheStringHash(key.Opaque())
	entry, found, err := a.entries.load(key, hash)
	if err != nil {
		return nil, false, err
	}
	if found {
		if err := entry.authenticate(a, snapshot, spec); err != nil {
			return nil, false, err
		}
		return entry, true, nil
	}
	entry, err = a.entries.loadOrCompute(
		key,
		hash,
		func() (*incrementalResourceMaterialization, error) {
			read, readErr := readResourceSnapshotItemsMaterialization(ctx, snapshot, spec)
			if readErr != nil {
				return nil, readErr
			}
			input := read.input
			encoded := string(input.Value)
			candidate := &incrementalResourceMaterialization{
				authority:    a.authority,
				key:          input.Key,
				resourceType: spec.resourceType,
				scope:        spec.scope,
				revision:     input.Revision,
				found:        input.Found,
				encoded:      encoded,
				encodedHash:  incrementalDecodedCacheStringHash(encoded),
				itemCount:    read.itemCount,
				storeValue:   read.storeValue,
				source:       snapshot.RevisionSource(),
				sequence:     snapshot.Sequence(),
			}
			candidate.seal = candidate
			candidate.raw.seal = &candidate.raw
			candidate.raw.owner = candidate
			candidate.projected.seal = &candidate.projected
			candidate.projected.owner = candidate
			if read.items != nil {
				raw := &incrementalResourceMaterializationRawItems{
					owner: candidate,
					items: read.items,
				}
				raw.seal = raw
				candidate.raw.value.Store(raw)
			}
			candidate.proof = newIncrementalResourceMaterializationProof(candidate)
			if err := candidate.authenticate(a, snapshot, spec); err != nil {
				return nil, err
			}
			return candidate, nil
		},
	)
	if err != nil {
		return nil, false, err
	}
	if err := entry.authenticate(a, snapshot, spec); err != nil {
		return nil, false, err
	}
	return entry, true, nil
}

func (a *incrementalResourceMaterializationArena) matching(
	input incremental.Input,
) (*incrementalResourceMaterialization, bool, error) {
	if !a.valid() {
		return nil, false, errors.New("incremental resource materialization arena has invalid provenance")
	}
	hash := incrementalDecodedCacheStringHash(input.Key.Opaque())
	entry, found, err := a.entries.load(input.Key, hash)
	if err != nil || !found {
		return nil, false, err
	}
	if err := entry.authenticateIdentity(a); err != nil {
		return nil, false, err
	}
	if entry.revision != input.Revision || entry.found != input.Found ||
		!stringBytesEqual(entry.encoded, input.Value) {
		return nil, false, nil
	}
	return entry, true, nil
}

func (m *incrementalResourceMaterialization) authenticate(
	arena *incrementalResourceMaterializationArena,
	snapshot stores.ReadSnapshot,
	spec *resourceInputSpec,
) error {
	if err := m.authenticateIdentity(arena); err != nil {
		return err
	}
	if snapshot == nil || spec == nil || m.key != resourceInputKey(spec) ||
		m.resourceType != spec.resourceType || m.scope != spec.scope ||
		m.source != snapshot.RevisionSource() || m.sequence != snapshot.Sequence() {
		return errors.New("incremental resource materialization has invalid snapshot provenance")
	}
	revision, err := resourceSnapshotRevision(snapshot, spec)
	if err != nil {
		return err
	}
	if m.revision != storeRevision(snapshot.RevisionSource(), revision) {
		return errors.New("incremental resource materialization has invalid revision provenance")
	}
	return nil
}

func (m *incrementalResourceMaterialization) authenticateIdentity(
	arena *incrementalResourceMaterializationArena,
) error {
	if arena == nil || !arena.valid() || m == nil || m.authority != arena.authority {
		return errors.New("incremental resource materialization has invalid provenance")
	}
	return m.authenticateDetached()
}

func (m *incrementalResourceMaterialization) authenticateDetached() error {
	if m == nil {
		return errors.New("incremental resource materialization has invalid provenance")
	}
	if !m.sealsIntact() || !m.shapeConsistent() || !m.proofMatches() {
		return errors.New("incremental resource materialization has invalid provenance")
	}
	if m.storeValue != nil && m.storeValue.Len() != m.itemCount {
		return errors.New("incremental resource materialization has invalid store projection")
	}
	if raw := m.raw.value.Load(); raw != nil {
		return raw.authenticate(m)
	}
	return nil
}

func (m *incrementalResourceMaterialization) sealsIntact() bool {
	rawState := &m.raw
	projectedState := &m.projected
	proof := m.proof
	return m.seal == m && m.authority != nil && m.authority.seal.Load() == m.authority &&
		proof != nil && proof.seal == proof && proof.authority == m.authority &&
		rawState.seal == rawState && rawState.owner == m &&
		projectedState.seal == projectedState && projectedState.owner == m
}

func (m *incrementalResourceMaterialization) shapeConsistent() bool {
	return m.key.Opaque() != "" && m.revision.Opaque() != "" && m.resourceType != "" &&
		(m.scope == resourceInputList || m.scope == resourceInputGet) &&
		m.itemCount >= 0 && (m.scope != resourceInputList || m.found) &&
		(m.scope != resourceInputGet || m.found == (m.itemCount > 0)) &&
		(m.found || m.encoded == "") && (!m.found || m.encoded != "")
}

func (m *incrementalResourceMaterialization) proofMatches() bool {
	proof := m.proof
	return proof.key == m.key && proof.resourceType == m.resourceType && proof.scope == m.scope &&
		proof.revision == m.revision && proof.found == m.found &&
		proof.encoded == m.encoded && proof.encodedHash == m.encodedHash &&
		proof.itemCount == m.itemCount && proof.storeValue == m.storeValue &&
		proof.raw == &m.raw && proof.projected == &m.projected &&
		proof.source == m.source && proof.sequence == m.sequence
}

func (r *incrementalResourceMaterializationRawItems) authenticate(
	owner *incrementalResourceMaterialization,
) error {
	if r == nil || owner == nil || r.seal != r || r.owner != owner ||
		len(r.items) != owner.itemCount {
		return errors.New("incremental resource materialization has invalid raw provenance")
	}
	if certificate := r.certificate.Load(); certificate != nil && !certificate.Guards(r.items) {
		return errors.New("incremental resource materialization has invalid immutable provenance")
	}
	return nil
}

func (m *incrementalResourceMaterialization) rawItems() ([]any, error) {
	if err := m.authenticateDetached(); err != nil {
		return nil, err
	}
	state := &m.raw
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := m.authenticateDetached(); err != nil {
		return nil, err
	}
	if raw := state.value.Load(); raw != nil {
		return raw.items, nil
	}
	items := []any{}
	if m.found || m.scope == resourceInputList {
		decoded, err := decodeResourceValue([]byte(m.encoded))
		if err != nil {
			return nil, fmt.Errorf("decoding incremental resource %q input: %w", m.resourceType, err)
		}
		if decoded == nil {
			items = nil
		} else if list, ok := decoded.([]any); ok {
			items = list
		} else {
			return nil, fmt.Errorf(
				"decoding incremental resource %q input: expected a list, got %T",
				m.resourceType,
				decoded,
			)
		}
	}
	if len(items) != m.itemCount {
		return nil, errors.New("incremental resource materialization has invalid raw cardinality")
	}
	raw := &incrementalResourceMaterializationRawItems{owner: m, items: items}
	raw.seal = raw
	state.value.Store(raw)
	if err := m.authenticateDetached(); err != nil {
		return nil, err
	}
	return raw.items, nil
}

func (m *incrementalResourceMaterialization) immutableCertificate() (
	*templating.IncrementalImmutableCertificate,
	error,
) {
	items, err := m.rawItems()
	if err != nil {
		return nil, err
	}
	raw := m.raw.value.Load()
	if err := raw.authenticate(m); err != nil {
		return nil, err
	}
	if certificate := raw.certificate.Load(); certificate != nil {
		return certificate, nil
	}
	state := &m.raw
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := raw.authenticate(m); err != nil {
		return nil, err
	}
	if certificate := raw.certificate.Load(); certificate != nil {
		return certificate, nil
	}
	certificate := templating.CertifyIncrementalImmutableInputs(items)
	if certificate == nil || !certificate.Guards(items) {
		return nil, errors.New("incremental resource materialization has invalid immutable provenance")
	}
	raw.certificate.Store(certificate)
	return certificate, raw.authenticate(m)
}

func (m *incrementalResourceMaterialization) projectItems(
	elementType reflect.Type,
) ([]reflect.Value, error) {
	if err := m.authenticateDetached(); err != nil {
		return nil, err
	}
	state := &m.projected
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := m.authenticateDetached(); err != nil {
		return nil, err
	}
	if cached := state.values[elementType]; cached != nil {
		if err := cached.authenticate(m, elementType); err != nil {
			return nil, err
		}
		return slices.Clone(cached.values), nil
	}
	var (
		values []reflect.Value
		err    error
	)
	if m.storeValue != nil {
		values, err = m.storeValue.ProjectItems(elementType)
	} else {
		var items []any
		items, err = m.rawItems()
		if err == nil {
			values, err = projectOwnedResourceItems(items, elementType)
		}
	}
	if err != nil {
		return nil, err
	}
	candidate := &incrementalResourceMaterializationProjectedItems{
		owner:       m,
		elementType: elementType,
		values:      values,
	}
	candidate.seal = candidate
	if err := candidate.authenticate(m, elementType); err != nil {
		return nil, err
	}
	if state.values == nil {
		state.values = make(map[reflect.Type]*incrementalResourceMaterializationProjectedItems)
	}
	state.values[elementType] = candidate
	return slices.Clone(values), nil
}

func (m *incrementalResourceMaterialization) directSingleResult(
	elementType reflect.Type,
	returnType reflect.Type,
) (reflect.Value, *templating.IncrementalImmutableCertificate, error) {
	if err := m.authenticateDetached(); err != nil {
		return reflect.Value{}, nil, err
	}
	if m.scope != resourceInputGet || m.itemCount != 1 || elementType == nil ||
		returnType != reflect.PointerTo(elementType) {
		return reflect.Value{}, nil, errors.New("incremental direct resource result has invalid shape")
	}
	if cached := m.directResult.Load(); cached != nil {
		if err := cached.authenticate(m, elementType, returnType); err != nil {
			return reflect.Value{}, nil, err
		}
		return cached.value, cached.certificate, nil
	}
	items, err := m.projectItems(elementType)
	if err != nil {
		return reflect.Value{}, nil, err
	}
	if len(items) != 1 || !items[0].IsValid() || items[0].Type() != returnType || items[0].IsNil() {
		return reflect.Value{}, nil, errors.New("incremental direct resource result has invalid projection")
	}
	value := items[0]
	certificate := templating.CertifyIncrementalImmutableInputs(value.Interface())
	if certificate == nil || !certificate.Guards(value.Interface()) {
		return reflect.Value{}, nil, errors.New("incremental direct resource result has invalid immutable provenance")
	}
	candidate := &incrementalResourceMaterializationDirectResult{
		owner: m, elementType: elementType, returnType: returnType,
		value: value, certificate: certificate,
	}
	candidate.seal = candidate
	candidate.proof = &incrementalResourceMaterializationDirectResultProof{
		result: candidate, owner: m, elementType: elementType, returnType: returnType,
		value: incrementalResourceMaterializationResultIdentity(value), certificate: certificate,
	}
	candidate.proof.seal = candidate.proof
	if !m.directResult.CompareAndSwap(nil, candidate) {
		cached := m.directResult.Load()
		if err := cached.authenticate(m, elementType, returnType); err != nil {
			return reflect.Value{}, nil, err
		}
		return cached.value, cached.certificate, nil
	}
	if err := candidate.authenticate(m, elementType, returnType); err != nil {
		return reflect.Value{}, nil, err
	}
	return candidate.value, candidate.certificate, nil
}

func (r *incrementalResourceMaterializationDirectResult) authenticate(
	owner *incrementalResourceMaterialization,
	elementType reflect.Type,
	returnType reflect.Type,
) error {
	if r == nil || r.seal != r || r.proof == nil || r.proof.seal != r.proof ||
		r.proof.result != r || r.owner != owner || r.proof.owner != owner ||
		r.elementType != elementType || r.proof.elementType != elementType ||
		r.returnType != returnType || r.proof.returnType != returnType ||
		!r.value.IsValid() || r.value.Type() != returnType || r.value.IsNil() ||
		r.proof.value != incrementalResourceMaterializationResultIdentity(r.value) ||
		r.certificate == nil || r.proof.certificate != r.certificate {
		return errors.New("incremental direct resource result has invalid provenance")
	}
	return owner.authenticateDetached()
}

func incrementalResourceMaterializationResultIdentity(
	value reflect.Value,
) incrementalResourceMaterializationValueIdentity {
	identity := incrementalResourceMaterializationValueIdentity{typeOf: value.Type(), kind: value.Kind()}
	if value.Kind() == reflect.Pointer {
		identity.isNil = value.IsNil()
		if !identity.isNil {
			identity.pointer = value.Pointer()
		}
	}
	return identity
}

func (p *incrementalResourceMaterializationProjectedItems) authenticate(
	owner *incrementalResourceMaterialization,
	elementType reflect.Type,
) error {
	if p == nil || owner == nil || p.seal != p || p.owner != owner ||
		p.elementType != elementType || len(p.values) != owner.itemCount {
		return errors.New("incremental resource materialization has invalid typed projection")
	}
	if elementType == nil {
		return nil
	}
	want := reflect.PointerTo(elementType)
	for _, value := range p.values {
		if !value.IsValid() || value.Type() != want || value.IsNil() {
			return errors.New("incremental resource materialization has invalid typed projection")
		}
	}
	return nil
}

func projectOwnedResourceItems(items []any, elementType reflect.Type) ([]reflect.Value, error) {
	values := make([]reflect.Value, len(items))
	for index, item := range items {
		if elementType == nil {
			if item != nil {
				values[index] = reflect.ValueOf(item)
			}
			continue
		}
		object, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("resource item %d has type %T", index, item)
		}
		pointer, err := typegen.WrapImmutableIntoPointer(object, elementType)
		if err != nil {
			return nil, fmt.Errorf("projecting resource item %d: %w", index, err)
		}
		values[index] = pointer
	}
	return values, nil
}

func newIncrementalResourceMaterializationProof(
	materialization *incrementalResourceMaterialization,
) *incrementalResourceMaterializationProof {
	proof := &incrementalResourceMaterializationProof{
		authority:    materialization.authority,
		key:          materialization.key,
		resourceType: materialization.resourceType,
		scope:        materialization.scope,
		revision:     materialization.revision,
		found:        materialization.found,
		encoded:      materialization.encoded,
		encodedHash:  materialization.encodedHash,
		itemCount:    materialization.itemCount,
		storeValue:   materialization.storeValue,
		raw:          &materialization.raw,
		projected:    &materialization.projected,
		source:       materialization.source,
		sequence:     materialization.sequence,
	}
	proof.seal = proof
	return proof
}

func (m *incrementalResourceMaterialization) input() incremental.Input {
	return incremental.Input{
		Key: m.key, Revision: m.revision, Found: m.found, Value: []byte(m.encoded),
	}
}

func (m *incrementalResourceMaterialization) immutableInput() incremental.ImmutableInput {
	return incremental.ImmutableInput{
		Key: m.key, Revision: m.revision, Found: m.found, Value: m.encoded,
	}
}

type incrementalResourceMaterializationRead struct {
	input      incremental.Input
	itemCount  int
	items      []any
	storeValue *k8sstore.ImmutableSnapshotProjection
}

func readResourceSnapshotItemsMaterialization(
	ctx context.Context,
	snapshot stores.ReadSnapshot,
	spec *resourceInputSpec,
) (*incrementalResourceMaterializationRead, error) {
	projection, projected, err := immutableResourceSnapshotProjection(ctx, snapshot, spec)
	if err != nil {
		return nil, err
	}
	if projected {
		found := spec.scope == resourceInputList || projection.Len() > 0
		var encoded []byte
		if found {
			encoded, err = projection.Encode()
			if err != nil {
				return nil, err
			}
		}
		input, inputErr := encodedResourceSnapshotInput(snapshot, spec, found, encoded)
		if inputErr != nil {
			return nil, inputErr
		}
		return &incrementalResourceMaterializationRead{
			input: input, itemCount: projection.Len(), storeValue: projection,
		}, nil
	}

	var (
		items   []any
		found   bool
		readErr error
	)
	switch spec.scope {
	case resourceInputList:
		items, readErr = readIncrementalSnapshotList(ctx, snapshot)
		found = true
	case resourceInputGet:
		items, readErr = readIncrementalSnapshotGet(ctx, snapshot, spec.keys...)
		found = readErr == nil && len(items) > 0
	default:
		return nil, errors.New("incremental resource materialization has an invalid scope")
	}
	if readErr != nil {
		return nil, readErr
	}
	input, encodeErr := encodeResourceSnapshotInput(snapshot, spec, items, found)
	if encodeErr != nil {
		return nil, encodeErr
	}
	if !found && spec.scope == resourceInputGet {
		return &incrementalResourceMaterializationRead{input: input, items: []any{}}, nil
	}
	_, canonical, decodeErr := decodeResourceMaterialization(input, spec)
	if decodeErr != nil {
		return nil, decodeErr
	}
	return &incrementalResourceMaterializationRead{
		input: input, itemCount: len(canonical), items: canonical,
	}, nil
}

func immutableResourceSnapshotProjection(
	ctx context.Context,
	snapshot stores.ReadSnapshot,
	spec *resourceInputSpec,
) (*k8sstore.ImmutableSnapshotProjection, bool, error) {
	switch spec.scope {
	case resourceInputList:
		return k8sstore.ProjectImmutableSnapshotList(ctx, snapshot)
	case resourceInputGet:
		return k8sstore.ProjectImmutableSnapshotGet(ctx, snapshot, spec.keys...)
	default:
		return nil, false, errors.New("incremental resource materialization has an invalid scope")
	}
}

func decodeResourceMaterialization(
	input incremental.Input,
	spec *resourceInputSpec,
) (incremental.Input, []any, error) {
	decoded, err := decodeResourceValue(input.Value)
	if err != nil {
		return incremental.Input{}, nil, err
	}
	if decoded == nil {
		return input, nil, nil
	}
	canonical, ok := decoded.([]any)
	if !ok {
		return incremental.Input{}, nil, fmt.Errorf(
			"decoding incremental resource %q input: expected a list, got %T",
			spec.resourceType,
			decoded,
		)
	}
	return input, canonical, nil
}
