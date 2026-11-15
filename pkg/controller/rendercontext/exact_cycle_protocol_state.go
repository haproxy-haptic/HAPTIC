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

package rendercontext

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"reflect"
	"slices"

	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

type exactCycleRenderContextProtocolStateAuthentication struct {
	kind      string
	paths     templating.PathResolver
	hasPaths  bool
	authority *PlanTokenAuthority
	nonce     string
	semantic  [sha256.Size]byte
	revision  uint64
	nested    templating.ExactCycleProtocolState
}

type exactCycleRenderContextProtocolState struct {
	kind      string
	paths     templating.PathResolver
	hasPaths  bool
	authority *PlanTokenAuthority
	nonce     string
	semantic  [sha256.Size]byte
	revision  uint64
	nested    templating.ExactCycleProtocolState
	auth      exactCycleRenderContextProtocolStateAuthentication
	seal      *exactCycleRenderContextProtocolState
}

func newExactCycleRenderContextProtocolState(
	kind string,
	paths *templating.PathResolver,
	authority *PlanTokenAuthority,
	nonce string,
	semantic [sha256.Size]byte,
	revision uint64,
	nested templating.ExactCycleProtocolState,
) *exactCycleRenderContextProtocolState {
	state := &exactCycleRenderContextProtocolState{
		kind: kind, authority: authority, nonce: nonce, semantic: semantic, revision: revision, nested: nested,
	}
	if paths != nil {
		state.paths = *paths
		state.hasPaths = true
	}
	state.auth = exactCycleRenderContextProtocolStateAuthentication{
		kind: state.kind, paths: state.paths, hasPaths: state.hasPaths,
		authority: state.authority, nonce: state.nonce, semantic: state.semantic,
		revision: state.revision, nested: state.nested,
	}
	state.seal = state
	return state
}

func (s *exactCycleRenderContextProtocolState) ValidateExactCycleProtocolState() error {
	if s == nil || s.seal != s || s.kind == "" || s.kind != s.auth.kind ||
		s.paths != s.auth.paths || s.hasPaths != s.auth.hasPaths ||
		s.authority != s.auth.authority || s.nonce != s.auth.nonce ||
		s.semantic != s.auth.semantic || s.revision != s.auth.revision ||
		!sameExactCycleRenderContextStateIdentity(s.nested, s.auth.nested) {
		return errors.New("exact cycle render-context protocol state has invalid provenance")
	}
	if s.authority != nil {
		if err := s.authority.validate(); err != nil || s.nonce != s.authority.nonce {
			return errors.New("exact cycle plan registry authority is invalid")
		}
	}
	if s.nested != nil {
		return s.nested.ValidateExactCycleProtocolState()
	}
	return nil
}

func (s *exactCycleRenderContextProtocolState) SameExactCycleProtocolState(
	current templating.ExactCycleProtocolState,
) (bool, error) {
	if err := s.ValidateExactCycleProtocolState(); err != nil {
		return false, err
	}
	other, ok := current.(*exactCycleRenderContextProtocolState)
	if !ok {
		return false, nil
	}
	if err := other.ValidateExactCycleProtocolState(); err != nil {
		return false, err
	}
	if s.kind != other.kind || s.paths != other.paths || s.hasPaths != other.hasPaths ||
		s.authority != other.authority || s.nonce != other.nonce || s.semantic != other.semantic ||
		s.revision != other.revision ||
		(s.nested == nil) != (other.nested == nil) {
		return false, nil
	}
	if s.nested == nil {
		return true, nil
	}
	return s.nested.SameExactCycleProtocolState(other.nested)
}

func sameExactCycleRenderContextStateIdentity(
	left, right templating.ExactCycleProtocolState,
) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	leftValue := reflect.ValueOf(left)
	rightValue := reflect.ValueOf(right)
	return leftValue.IsValid() && rightValue.IsValid() &&
		leftValue.Type() == rightValue.Type() && leftValue.Kind() == reflect.Pointer &&
		leftValue.Pointer() == rightValue.Pointer()
}

// ExactCycleProtocolState authenticates the empty registry and its path semantics.
func (r *FileRegistry) ExactCycleProtocolState() (templating.ExactCycleProtocolState, error) {
	if r == nil {
		return nil, errors.New("file registry is nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.registered == nil || len(r.registered) != 0 || r.pathResolver == nil {
		return nil, errors.New("file registry is not fresh and empty")
	}
	return newExactCycleRenderContextProtocolState(
		"fileRegistry", r.pathResolver, nil, "", [sha256.Size]byte{}, 0, nil,
	), nil
}

// ExactCycleProtocolState authenticates the registry's pre-root declaration state.
func (r *PlanRegistry) ExactCycleProtocolState() (templating.ExactCycleProtocolState, error) {
	if r == nil {
		return nil, errors.New("plan registry is nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.validateTokenAuthority(); err != nil {
		return nil, err
	}
	if r.sections == nil || len(r.sections) != 0 || r.backends == nil || len(r.backends) != 0 ||
		r.mapsMeta == nil || len(r.sensitiveMaps) != 0 || len(r.assembled) != 0 || r.prepared != nil || r.assembly != nil ||
		r.documentAssembly != nil {
		return nil, errors.New("plan registry is not in its initial declaration state")
	}
	return newExactCycleRenderContextProtocolState(
		"planRegistry", r.paths, r.authority, r.nonce, exactCycleMapMetaRoot(r.mapsMeta),
		r.declarationRevision, nil,
	), nil
}

// ExactCycleProtocolState authenticates the empty frozen view and its resolver authority.
func (v *DerivedResourceView) ExactCycleProtocolState() (templating.ExactCycleProtocolState, error) {
	if v == nil {
		return nil, errors.New("derived resource view is nil")
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.entries == nil || len(v.entries) != 0 || v.resourceCounts == nil || len(v.resourceCounts) != 0 ||
		v.origins == nil || len(v.origins) != 0 || !v.frozen {
		return nil, errors.New("derived resource view is not fresh, empty, and frozen")
	}
	var nested templating.ExactCycleProtocolState
	if v.resolver != nil {
		provider, ok := v.resolver.(templating.ExactCycleProtocolStateProvider)
		if !ok {
			return nil, errors.New("derived resource resolver has no exact-cycle protocol")
		}
		var err error
		nested, err = provider.ExactCycleProtocolState()
		if err != nil {
			return nil, err
		}
		if nested == nil {
			return nil, errors.New("derived resource resolver returned no exact-cycle state")
		}
		if err := nested.ValidateExactCycleProtocolState(); err != nil {
			return nil, err
		}
	}
	return newExactCycleRenderContextProtocolState(
		"resourceDeriver", nil, nil, "", [sha256.Size]byte{}, 0, nested,
	), nil
}

func exactCycleMapMetaRoot(values map[string]bool) [sha256.Size]byte {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	hash := sha256.New()
	var size [8]byte
	for _, key := range keys {
		binary.BigEndian.PutUint64(size[:], uint64(len(key)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write([]byte(key))
		if values[key] {
			_, _ = hash.Write([]byte{1})
		} else {
			_, _ = hash.Write([]byte{0})
		}
	}
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result
}
