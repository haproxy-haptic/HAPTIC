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
	"sync"

	"github.com/haproxy-haptic/HAPTIC/pkg/incremental"
)

var ErrMetadataUnavailable = errors.New("result metadata is unavailable")

type Handle[V any, M comparable] struct {
	state *state[V, M]
	seal  *Handle[V, M]
}

type state[V any, M comparable] struct {
	mu          sync.Mutex
	owner       *Handle[V, M]
	key         incremental.QueryKey
	encoded     string
	root        incremental.ExactValueRoot
	value       V
	metadata    M
	bound       bool
	taken       bool
	hasMetadata bool
}

func New[V any, M comparable](
	key incremental.QueryKey,
	encoded string,
	value V,
	metadata *M,
	clone func(*V) V,
) *Handle[V, M] {
	return newHandle(key, encoded, clone(&value), metadata)
}

func NewOwned[V any, M comparable](
	key incremental.QueryKey,
	encoded string,
	value V,
	metadata *M,
) *Handle[V, M] {
	return newHandle(key, encoded, value, metadata)
}

func newHandle[V any, M comparable](
	key incremental.QueryKey,
	encoded string,
	value V,
	metadata *M,
) *Handle[V, M] {
	handle := &Handle[V, M]{}
	entry := &state[V, M]{owner: handle, key: key, encoded: encoded, value: value}
	if metadata != nil {
		entry.metadata = *metadata
		entry.hasMetadata = true
	}
	handle.state = entry
	handle.seal = handle
	return handle
}

func Pending[V any, M comparable](
	handle *Handle[V, M],
	key incremental.QueryKey,
	encoded string,
	ownerRoot incremental.ExactValueRoot,
) error {
	entry, err := authenticate(handle, key, encoded)
	if err != nil {
		return err
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return validateOwnerRoot(entry, ownerRoot)
}

func Bind[V any, M comparable](
	handle *Handle[V, M],
	key incremental.QueryKey,
	encoded string,
	ownerRoot, root incremental.ExactValueRoot,
) error {
	entry, err := authenticate(handle, key, encoded)
	if err != nil {
		return err
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if err := validateOwnerRoot(entry, ownerRoot); err != nil {
		return err
	}
	if entry.bound {
		return validateRequestedRoot(entry, key, root)
	}
	rootValue, err := root.String()
	if err != nil || rootValue != entry.encoded {
		return errors.New("fresh incremental component result does not match its authoritative value")
	}
	entry.root = root
	entry.bound = true
	return nil
}

func Validate[V any, M comparable](
	handle *Handle[V, M],
	key incremental.QueryKey,
	encoded string,
	ownerRoot, root incremental.ExactValueRoot,
) error {
	entry, err := authenticate(handle, key, encoded)
	if err != nil {
		return err
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if err := validateOwnerRoot(entry, ownerRoot); err != nil {
		return err
	}
	if !entry.bound {
		return errors.New("fresh incremental component result has no authoritative root")
	}
	return validateRequestedRoot(entry, key, root)
}

func Materialize[V any, M comparable](
	handle *Handle[V, M],
	key incremental.QueryKey,
	encoded string,
	ownerRoot, root incremental.ExactValueRoot,
	clone func(*V) V,
) (V, error) {
	entry, err := validatedEntry(handle, key, encoded, ownerRoot, root)
	if err != nil {
		var zero V
		return zero, err
	}
	defer entry.mu.Unlock()
	if entry.taken {
		var zero V
		return zero, errors.New("fresh incremental component result ownership was already transferred")
	}
	return clone(&entry.value), nil
}

func Take[V any, M comparable](
	handle *Handle[V, M],
	key incremental.QueryKey,
	encoded string,
	ownerRoot, root incremental.ExactValueRoot,
) (V, error) {
	entry, err := validatedEntry(handle, key, encoded, ownerRoot, root)
	if err != nil {
		var zero V
		return zero, err
	}
	defer entry.mu.Unlock()
	if entry.taken {
		var zero V
		return zero, errors.New("fresh incremental component result ownership was already transferred")
	}
	value := entry.value
	var zero V
	entry.value = zero
	entry.taken = true
	return value, nil
}

func MetadataMatches[V any, M comparable](
	handle *Handle[V, M],
	key incremental.QueryKey,
	encoded string,
	ownerRoot, root incremental.ExactValueRoot,
	metadata M,
) error {
	entry, err := validatedEntry(handle, key, encoded, ownerRoot, root)
	if err != nil {
		return err
	}
	defer entry.mu.Unlock()
	if !entry.hasMetadata {
		return ErrMetadataUnavailable
	}
	if entry.metadata != metadata {
		return errors.New("fresh incremental component effects have invalid provenance")
	}
	return nil
}

func validatedEntry[V any, M comparable](
	handle *Handle[V, M],
	key incremental.QueryKey,
	encoded string,
	ownerRoot, root incremental.ExactValueRoot,
) (*state[V, M], error) {
	entry, err := authenticate(handle, key, encoded)
	if err != nil {
		return nil, err
	}
	entry.mu.Lock()
	if err := validateOwnerRoot(entry, ownerRoot); err != nil {
		entry.mu.Unlock()
		return nil, err
	}
	if !entry.bound {
		entry.mu.Unlock()
		return nil, errors.New("fresh incremental component result has no authoritative root")
	}
	if err := validateRequestedRoot(entry, key, root); err != nil {
		entry.mu.Unlock()
		return nil, err
	}
	return entry, nil
}

func authenticate[V any, M comparable](
	handle *Handle[V, M],
	key incremental.QueryKey,
	encoded string,
) (*state[V, M], error) {
	if handle == nil || handle.seal != handle || handle.state == nil ||
		handle.state.owner != handle || handle.state.key != key || handle.state.encoded != encoded {
		return nil, errors.New("fresh incremental component result has invalid provenance")
	}
	return handle.state, nil
}

func validateOwnerRoot[V any, M comparable](
	entry *state[V, M],
	ownerRoot incremental.ExactValueRoot,
) error {
	if !entry.bound {
		if ownerRoot != (incremental.ExactValueRoot{}) {
			return errors.New("fresh incremental component result has invalid provenance")
		}
		return nil
	}
	same, err := ownerRoot.SameRoot(entry.root)
	if err != nil || !same {
		return fmt.Errorf(
			"fresh incremental component result %s has a different stored authoritative root",
			entry.key,
		)
	}
	return nil
}

func validateRequestedRoot[V any, M comparable](
	entry *state[V, M],
	key incremental.QueryKey,
	root incremental.ExactValueRoot,
) error {
	same, err := root.SameRoot(entry.root)
	if err != nil || !same {
		return fmt.Errorf(
			"fresh incremental component result %s does not match its authoritative root",
			key,
		)
	}
	return nil
}
