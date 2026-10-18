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

package resultauthority

import (
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/haproxy-haptic/HAPTIC/pkg/incremental"
)

type arenaSlotState uint8

const (
	arenaSlotEmpty arenaSlotState = iota
	arenaSlotPending
	arenaSlotBound
	arenaSlotTaken
)

// Arena owns a fixed set of independently authenticated result slots.
type Arena[V any, M comparable] struct {
	lifetime   sync.RWMutex
	seal       *Arena[V, M]
	generation uint64
	slots      []arenaSlot[V, M]
	revoked    bool
}

type arenaSlot[V any, M comparable] struct {
	mu          sync.Mutex
	ref         Ref[V, M]
	owner       *Arena[V, M]
	index       int
	generation  uint64
	key         incremental.QueryKey
	encoded     string
	root        incremental.ExactValueRoot
	value       V
	metadata    M
	state       arenaSlotState
	hasMetadata bool
}

// Ref is the authenticated authority stored inside one arena slot.
type Ref[V any, M comparable] struct {
	arena      *Arena[V, M]
	seal       *Ref[V, M]
	slot       int
	generation uint64
	key        incremental.QueryKey
	encoded    string
}

// InitializeRequest transfers one value into an exact arena slot.
type InitializeRequest[V any, M comparable] struct {
	Index    int
	Key      incremental.QueryKey
	Encoded  string
	Value    V
	Metadata *M
}

// BindRequest authenticates one pending or already-bound arena slot.
type BindRequest[V any, M comparable] struct {
	Ref       *Ref[V, M]
	Key       incremental.QueryKey
	Encoded   string
	OwnerRoot incremental.ExactValueRoot
	Root      incremental.ExactValueRoot
}

// TakeRequest identifies one arena slot for an atomic ownership transfer.
type TakeRequest[V any, M comparable] struct {
	Ref       *Ref[V, M]
	Key       incremental.QueryKey
	Encoded   string
	OwnerRoot incremental.ExactValueRoot
	Root      incremental.ExactValueRoot
}

// NewArena allocates exactly capacity result slots for generation.
func NewArena[V any, M comparable](capacity int, generation uint64) (*Arena[V, M], error) {
	if capacity <= 0 {
		return nil, errors.New("result arena capacity must be positive")
	}
	if generation == 0 {
		return nil, errors.New("result arena generation must be non-zero")
	}
	arena := &Arena[V, M]{generation: generation, slots: make([]arenaSlot[V, M], capacity)}
	arena.seal = arena
	for index := range arena.slots {
		slot := &arena.slots[index]
		slot.owner = arena
		slot.index = index
		slot.generation = generation
	}
	return arena, nil
}

// InitializeOwned transfers value ownership into one exact empty slot.
func (a *Arena[V, M]) InitializeOwned(
	index int,
	key incremental.QueryKey,
	encoded string,
	value V,
	metadata *M,
) (*Ref[V, M], error) {
	if a == nil {
		return nil, errors.New("result arena is unavailable")
	}
	a.lifetime.RLock()
	defer a.lifetime.RUnlock()
	if a.seal != a || a.revoked || a.generation == 0 || a.slots == nil {
		return nil, errors.New("result arena has invalid provenance")
	}
	if index < 0 || index >= len(a.slots) {
		return nil, fmt.Errorf("result arena slot %d is out of range", index)
	}
	if key.Opaque() == "" {
		return nil, errors.New("result arena slot key is empty")
	}
	slot := &a.slots[index]
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if slot.owner != a || slot.index != index || slot.generation != a.generation ||
		slot.state != arenaSlotEmpty {
		return nil, fmt.Errorf("result arena slot %d is unavailable", index)
	}
	return initializeArenaSlot(slot, a, key, encoded, value, metadata), nil
}

// InitializeOwnedMany transfers value ownership only after every slot passes preflight.
func (a *Arena[V, M]) InitializeOwnedMany(
	requests []InitializeRequest[V, M],
) ([]*Ref[V, M], error) {
	if a == nil {
		return nil, errors.New("result arena is unavailable")
	}
	a.lifetime.RLock()
	defer a.lifetime.RUnlock()
	if !validArenaProvenance(a) {
		return nil, errors.New("result arena has invalid provenance")
	}
	order, err := a.initializationOrder(requests)
	if err != nil {
		return nil, err
	}
	lockArenaSlots(a, order)
	defer unlockArenaSlots(a, order)
	if err := a.preflightInitialization(requests); err != nil {
		return nil, err
	}
	return a.initializeArenaSlots(requests), nil
}

func validArenaProvenance[V any, M comparable](arena *Arena[V, M]) bool {
	return arena != nil && arena.seal == arena && !arena.revoked &&
		arena.generation != 0 && arena.slots != nil
}

func (a *Arena[V, M]) initializationOrder(
	requests []InitializeRequest[V, M],
) ([]int, error) {
	if len(requests) == 0 {
		return nil, errors.New("result arena initialization range is empty")
	}
	order := make([]int, len(requests))
	for requestIndex := range requests {
		request := &requests[requestIndex]
		if request.Index < 0 || request.Index >= len(a.slots) {
			return nil, fmt.Errorf("result arena slot %d is out of range", request.Index)
		}
		if request.Key.Opaque() == "" {
			return nil, errors.New("result arena slot key is empty")
		}
		order[requestIndex] = request.Index
	}
	slices.Sort(order)
	if arenaSlotsRepeat(order) {
		return nil, errors.New("result arena initialization range repeats a slot")
	}
	return order, nil
}

func (a *Arena[V, M]) preflightInitialization(
	requests []InitializeRequest[V, M],
) error {
	for requestIndex := range requests {
		request := &requests[requestIndex]
		slot := &a.slots[request.Index]
		if slot.owner != a || slot.index != request.Index || slot.generation != a.generation ||
			slot.state != arenaSlotEmpty {
			return fmt.Errorf("result arena slot %d is unavailable", request.Index)
		}
	}
	return nil
}

func (a *Arena[V, M]) initializeArenaSlots(
	requests []InitializeRequest[V, M],
) []*Ref[V, M] {
	refs := make([]*Ref[V, M], len(requests))
	for requestIndex := range requests {
		request := &requests[requestIndex]
		refs[requestIndex] = initializeArenaSlot(
			&a.slots[request.Index],
			a,
			request.Key,
			request.Encoded,
			request.Value,
			request.Metadata,
		)
	}
	return refs
}

func arenaSlotsRepeat(order []int) bool {
	for index := 1; index < len(order); index++ {
		if order[index] == order[index-1] {
			return true
		}
	}
	return false
}

func lockArenaSlots[V any, M comparable](arena *Arena[V, M], order []int) {
	for _, slotIndex := range order {
		arena.slots[slotIndex].mu.Lock()
	}
}

func unlockArenaSlots[V any, M comparable](arena *Arena[V, M], order []int) {
	for index := len(order) - 1; index >= 0; index-- {
		arena.slots[order[index]].mu.Unlock()
	}
}

func initializeArenaSlot[V any, M comparable](
	slot *arenaSlot[V, M],
	arena *Arena[V, M],
	key incremental.QueryKey,
	encoded string,
	value V,
	metadata *M,
) *Ref[V, M] {
	ref := &slot.ref
	*ref = Ref[V, M]{
		arena:      arena,
		slot:       slot.index,
		generation: arena.generation,
		key:        key,
		encoded:    encoded,
	}
	ref.seal = ref
	slot.key = key
	slot.encoded = encoded
	slot.root = incremental.ExactValueRoot{}
	slot.value = value
	var metadataZero M
	slot.metadata = metadataZero
	slot.hasMetadata = false
	if metadata != nil {
		slot.metadata = *metadata
		slot.hasMetadata = true
	}
	slot.state = arenaSlotPending
	return ref
}

// Revoke clears every value still owned by the arena and invalidates all refs.
func (a *Arena[V, M]) Revoke() {
	if a == nil {
		return
	}
	a.lifetime.Lock()
	defer a.lifetime.Unlock()
	if a.seal != a || a.revoked {
		return
	}
	a.revoked = true
	for index := range a.slots {
		slot := &a.slots[index]
		slot.mu.Lock()
		var valueZero V
		var metadataZero M
		slot.value = valueZero
		slot.metadata = metadataZero
		slot.root = incremental.ExactValueRoot{}
		slot.key = incremental.QueryKey{}
		slot.encoded = ""
		slot.hasMetadata = false
		slot.state = arenaSlotEmpty
		slot.mu.Unlock()
	}
}

// Pending authenticates an initialized slot before it owns an exact root.
func (r *Ref[V, M]) Pending(
	key incremental.QueryKey,
	encoded string,
	ownerRoot incremental.ExactValueRoot,
) error {
	arena, slot, err := authenticateArenaRef(r, key, encoded)
	if err != nil {
		return err
	}
	defer releaseArenaSlot(arena, slot)
	return validateArenaOwnerRoot(slot, ownerRoot)
}

// Bind installs the slot's exact root or authenticates an existing binding.
func (r *Ref[V, M]) Bind(
	key incremental.QueryKey,
	encoded string,
	ownerRoot, root incremental.ExactValueRoot,
) error {
	arena, slot, err := authenticateArenaRef(r, key, encoded)
	if err != nil {
		return err
	}
	defer releaseArenaSlot(arena, slot)
	if err := validateArenaOwnerRoot(slot, ownerRoot); err != nil {
		return err
	}
	if slot.state == arenaSlotBound || slot.state == arenaSlotTaken {
		return validateArenaRequestedRoot(slot, key, root)
	}
	rootValue, err := root.String()
	if err != nil || rootValue != slot.encoded {
		return errors.New("fresh incremental component result does not match its authoritative value")
	}
	slot.root = root
	slot.state = arenaSlotBound
	return nil
}

// BindMany binds no slot unless every requested authority and root passes preflight.
func BindMany[V any, M comparable](requests []BindRequest[V, M]) error {
	arena, order, err := lockBindRequestSlots(requests)
	if err != nil {
		return err
	}
	defer func() {
		unlockArenaSlots(arena, order)
		arena.lifetime.RUnlock()
	}()
	for requestIndex := range requests {
		if err := validateBindRequest(arena, &requests[requestIndex]); err != nil {
			return err
		}
	}
	for requestIndex := range requests {
		request := &requests[requestIndex]
		slot := &arena.slots[request.Ref.slot]
		if slot.state == arenaSlotPending {
			slot.root = request.Root
			slot.state = arenaSlotBound
		}
	}
	return nil
}

func lockBindRequestSlots[V any, M comparable](
	requests []BindRequest[V, M],
) (*Arena[V, M], []int, error) {
	if len(requests) == 0 || requests[0].Ref == nil || requests[0].Ref.arena == nil {
		return nil, nil, errors.New(
			"fresh incremental component result range has invalid provenance",
		)
	}
	arena := requests[0].Ref.arena
	arena.lifetime.RLock()
	if !validArenaProvenance(arena) {
		arena.lifetime.RUnlock()
		return nil, nil, errors.New(
			"fresh incremental component result range has invalid provenance",
		)
	}
	order := make([]int, len(requests))
	for requestIndex := range requests {
		ref := requests[requestIndex].Ref
		if ref == nil || ref.arena != arena || ref.slot < 0 || ref.slot >= len(arena.slots) {
			arena.lifetime.RUnlock()
			return nil, nil, errors.New(
				"fresh incremental component result range has invalid provenance",
			)
		}
		order[requestIndex] = ref.slot
	}
	slices.Sort(order)
	if arenaSlotsRepeat(order) {
		arena.lifetime.RUnlock()
		return nil, nil, errors.New("fresh incremental component result range repeats a slot")
	}
	lockArenaSlots(arena, order)
	return arena, order, nil
}

func validateBindRequest[V any, M comparable](
	arena *Arena[V, M],
	request *BindRequest[V, M],
) error {
	ref := request.Ref
	slot := &arena.slots[ref.slot]
	if !validLockedArenaRef(arena, slot, ref, request.Key, request.Encoded) {
		return errors.New("fresh incremental component result range has invalid provenance")
	}
	if err := validateArenaOwnerRoot(slot, request.OwnerRoot); err != nil {
		return err
	}
	if slot.state != arenaSlotPending {
		return validateArenaRequestedRoot(slot, request.Key, request.Root)
	}
	rootValue, err := request.Root.String()
	if err != nil || rootValue != slot.encoded {
		return errors.New(
			"fresh incremental component result does not match its authoritative value",
		)
	}
	return nil
}

func validLockedArenaRef[V any, M comparable](
	arena *Arena[V, M],
	slot *arenaSlot[V, M],
	ref *Ref[V, M],
	key incremental.QueryKey,
	encoded string,
) bool {
	return ref.seal == ref && ref.arena == arena && ref.generation == arena.generation &&
		ref.key == key && ref.encoded == encoded && &slot.ref == ref &&
		slot.owner == arena && slot.index == ref.slot && slot.generation == ref.generation &&
		slot.key == ref.key && slot.encoded == ref.encoded && validArenaBindingState(slot.state)
}

func validArenaBindingState(state arenaSlotState) bool {
	return state == arenaSlotPending || state == arenaSlotBound || state == arenaSlotTaken
}

// Validate authenticates the ref, its owner root, and its requested root.
func (r *Ref[V, M]) Validate(
	key incremental.QueryKey,
	encoded string,
	ownerRoot, root incremental.ExactValueRoot,
) error {
	arena, slot, err := validatedArenaSlot(r, key, encoded, ownerRoot, root)
	if err != nil {
		return err
	}
	releaseArenaSlot(arena, slot)
	return nil
}

// Materialize returns a detached clone while the arena retains ownership.
func (r *Ref[V, M]) Materialize(
	key incremental.QueryKey,
	encoded string,
	ownerRoot, root incremental.ExactValueRoot,
	clone func(*V) V,
) (V, error) {
	arena, slot, err := validatedArenaSlot(r, key, encoded, ownerRoot, root)
	if err != nil {
		var zero V
		return zero, err
	}
	defer releaseArenaSlot(arena, slot)
	if slot.state == arenaSlotTaken {
		var zero V
		return zero, errors.New("fresh incremental component result ownership was already transferred")
	}
	if clone == nil {
		var zero V
		return zero, errors.New("fresh incremental component result clone function is nil")
	}
	return clone(&slot.value), nil
}

// Take transfers slot ownership exactly once.
func (r *Ref[V, M]) Take(
	key incremental.QueryKey,
	encoded string,
	ownerRoot, root incremental.ExactValueRoot,
) (V, error) {
	arena, slot, err := validatedArenaSlot(r, key, encoded, ownerRoot, root)
	if err != nil {
		var zero V
		return zero, err
	}
	defer releaseArenaSlot(arena, slot)
	if slot.state == arenaSlotTaken {
		var zero V
		return zero, errors.New("fresh incremental component result ownership was already transferred")
	}
	value := slot.value
	var zero V
	slot.value = zero
	slot.state = arenaSlotTaken
	return value, nil
}

// TakeMany preflights one arena range before transferring any slot.
func TakeMany[V any, M comparable](requests []TakeRequest[V, M]) ([]V, error) {
	if len(requests) == 0 || requests[0].Ref == nil || requests[0].Ref.arena == nil {
		return nil, errors.New("fresh incremental component result range has invalid provenance")
	}
	arena := requests[0].Ref.arena
	arena.lifetime.RLock()
	if arena.seal != arena || arena.revoked || arena.generation == 0 || arena.slots == nil {
		arena.lifetime.RUnlock()
		return nil, errors.New("fresh incremental component result range has invalid provenance")
	}
	order, err := arenaTakeOrder(arena, requests)
	if err != nil {
		arena.lifetime.RUnlock()
		return nil, err
	}
	slices.Sort(order)
	for _, slotIndex := range order {
		arena.slots[slotIndex].mu.Lock()
	}
	unlock := func() {
		for index := len(order) - 1; index >= 0; index-- {
			arena.slots[order[index]].mu.Unlock()
		}
		arena.lifetime.RUnlock()
	}
	if err := validateArenaTakeRequests(arena, requests); err != nil {
		unlock()
		return nil, err
	}
	values := make([]V, len(requests))
	for requestIndex := range requests {
		slot := &arena.slots[requests[requestIndex].Ref.slot]
		values[requestIndex] = slot.value
		var zero V
		slot.value = zero
		slot.state = arenaSlotTaken
	}
	unlock()
	return values, nil
}

func arenaTakeOrder[V any, M comparable](
	arena *Arena[V, M],
	requests []TakeRequest[V, M],
) ([]int, error) {
	order := make([]int, len(requests))
	seen := make(map[int]struct{}, len(requests))
	for requestIndex := range requests {
		request := &requests[requestIndex]
		if request.Ref == nil || request.Ref.arena != arena || request.Ref.slot < 0 ||
			request.Ref.slot >= len(arena.slots) {
			return nil, errors.New("fresh incremental component result range has invalid provenance")
		}
		if _, duplicate := seen[request.Ref.slot]; duplicate {
			return nil, errors.New("fresh incremental component result range repeats a slot")
		}
		seen[request.Ref.slot] = struct{}{}
		order[requestIndex] = request.Ref.slot
	}
	return order, nil
}

func validateArenaTakeRequests[V any, M comparable](
	arena *Arena[V, M],
	requests []TakeRequest[V, M],
) error {
	for requestIndex := range requests {
		request := &requests[requestIndex]
		ref := request.Ref
		slot := &arena.slots[ref.slot]
		if !arenaTakeRequestBound(arena, slot, ref, request) {
			return errors.New("fresh incremental component result range has invalid provenance")
		}
		if err := validateArenaOwnerRoot(slot, request.OwnerRoot); err != nil {
			return err
		}
		if err := validateArenaRequestedRoot(slot, request.Key, request.Root); err != nil {
			return err
		}
	}
	return nil
}

func arenaTakeRequestBound[V any, M comparable](
	arena *Arena[V, M],
	slot *arenaSlot[V, M],
	ref *Ref[V, M],
	request *TakeRequest[V, M],
) bool {
	return ref.seal == ref && ref.arena == arena && ref.generation == arena.generation &&
		ref.key == request.Key && ref.encoded == request.Encoded && &slot.ref == ref &&
		slot.owner == arena && slot.index == ref.slot && slot.generation == ref.generation &&
		slot.key == ref.key && slot.encoded == ref.encoded && slot.state == arenaSlotBound
}

// MetadataMatches authenticates optional metadata attached to the slot.
func (r *Ref[V, M]) MetadataMatches(
	key incremental.QueryKey,
	encoded string,
	ownerRoot, root incremental.ExactValueRoot,
	metadata M,
) error {
	arena, slot, err := validatedArenaSlot(r, key, encoded, ownerRoot, root)
	if err != nil {
		return err
	}
	defer releaseArenaSlot(arena, slot)
	if !slot.hasMetadata {
		return ErrMetadataUnavailable
	}
	if slot.metadata != metadata {
		return errors.New("fresh incremental component effects have invalid provenance")
	}
	return nil
}

func validatedArenaSlot[V any, M comparable](
	ref *Ref[V, M],
	key incremental.QueryKey,
	encoded string,
	ownerRoot, root incremental.ExactValueRoot,
) (*Arena[V, M], *arenaSlot[V, M], error) {
	arena, slot, err := authenticateArenaRef(ref, key, encoded)
	if err != nil {
		return nil, nil, err
	}
	if err := validateArenaOwnerRoot(slot, ownerRoot); err != nil {
		releaseArenaSlot(arena, slot)
		return nil, nil, err
	}
	if slot.state == arenaSlotPending {
		releaseArenaSlot(arena, slot)
		return nil, nil, errors.New("fresh incremental component result has no authoritative root")
	}
	if err := validateArenaRequestedRoot(slot, key, root); err != nil {
		releaseArenaSlot(arena, slot)
		return nil, nil, err
	}
	return arena, slot, nil
}

func authenticateArenaRef[V any, M comparable](
	ref *Ref[V, M],
	key incremental.QueryKey,
	encoded string,
) (*Arena[V, M], *arenaSlot[V, M], error) {
	if ref == nil || ref.arena == nil {
		return nil, nil, errors.New("fresh incremental component result has invalid provenance")
	}
	arena := ref.arena
	arena.lifetime.RLock()
	if !arenaRefBound(arena, ref, key, encoded) {
		arena.lifetime.RUnlock()
		return nil, nil, errors.New("fresh incremental component result has invalid provenance")
	}
	slot := &arena.slots[ref.slot]
	slot.mu.Lock()
	if !arenaSlotBoundToRef(arena, slot, ref) {
		slot.mu.Unlock()
		arena.lifetime.RUnlock()
		return nil, nil, errors.New("fresh incremental component result has invalid provenance")
	}
	return arena, slot, nil
}

func arenaRefBound[V any, M comparable](
	arena *Arena[V, M],
	ref *Ref[V, M],
	key incremental.QueryKey,
	encoded string,
) bool {
	return arena.seal == arena && !arena.revoked && arena.generation != 0 && arena.slots != nil &&
		ref.seal == ref && ref.arena == arena && ref.generation == arena.generation &&
		ref.key == key && ref.encoded == encoded && ref.slot >= 0 && ref.slot < len(arena.slots)
}

func arenaSlotBoundToRef[V any, M comparable](
	arena *Arena[V, M],
	slot *arenaSlot[V, M],
	ref *Ref[V, M],
) bool {
	return &slot.ref == ref && slot.owner == arena && slot.index == ref.slot &&
		slot.generation == ref.generation && slot.key == ref.key && slot.encoded == ref.encoded &&
		slot.state != arenaSlotEmpty
}

func releaseArenaSlot[V any, M comparable](arena *Arena[V, M], slot *arenaSlot[V, M]) {
	slot.mu.Unlock()
	arena.lifetime.RUnlock()
}

func validateArenaOwnerRoot[V any, M comparable](
	slot *arenaSlot[V, M],
	ownerRoot incremental.ExactValueRoot,
) error {
	if slot.state == arenaSlotPending {
		if ownerRoot != (incremental.ExactValueRoot{}) {
			return errors.New("fresh incremental component result has invalid provenance")
		}
		return nil
	}
	same, err := ownerRoot.SameRoot(slot.root)
	if err != nil || !same {
		return fmt.Errorf(
			"fresh incremental component result %s has a different stored authoritative root",
			slot.key,
		)
	}
	return nil
}

func validateArenaRequestedRoot[V any, M comparable](
	slot *arenaSlot[V, M],
	key incremental.QueryKey,
	root incremental.ExactValueRoot,
) error {
	same, err := root.SameRoot(slot.root)
	if err != nil || !same {
		return fmt.Errorf(
			"fresh incremental component result %s does not match its authoritative root",
			key,
		)
	}
	return nil
}
