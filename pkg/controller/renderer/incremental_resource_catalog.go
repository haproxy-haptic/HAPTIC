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
	"errors"
	"fmt"
	"hash/maphash"
	"strings"
	"sync"

	iradix "github.com/hashicorp/go-immutable-radix/v2"

	"github.com/haproxy-haptic/HAPTIC/pkg/incremental"
)

const incrementalResourceCatalogShardCount = 64

type incrementalResourceCatalogMutationState uint8

const (
	incrementalResourceCatalogPresent incrementalResourceCatalogMutationState = iota + 1
	incrementalResourceCatalogDeleted
)

type incrementalResourceCatalogSnapshotProof struct {
	owner *incrementalResourceCatalogSnapshot
	tree  *iradix.Tree[struct{}]
	seal  *incrementalResourceCatalogSnapshotProof
}

type incrementalResourceCatalogSnapshot struct {
	seal  *incrementalResourceCatalogSnapshot
	tree  *iradix.Tree[struct{}]
	proof *incrementalResourceCatalogSnapshotProof
}

type incrementalResourceCatalogMutation struct {
	owner      *incrementalResourceCatalog
	generation uint64
	key        incremental.InputKey
	state      incrementalResourceCatalogMutationState
}

type incrementalResourceCatalogShard struct {
	mu      sync.RWMutex
	changes map[incremental.InputKey]incrementalResourceCatalogMutation
}

type incrementalResourceCatalogShards struct {
	seal   *incrementalResourceCatalogShards
	values [incrementalResourceCatalogShardCount]incrementalResourceCatalogShard
}

type incrementalResourceCatalogProof struct {
	owner   *incrementalResourceCatalog
	session *incrementalRenderSession
	shards  *incrementalResourceCatalogShards
	seed    maphash.Seed
	seal    *incrementalResourceCatalogProof
}

type incrementalResourceCatalog struct {
	seal       *incrementalResourceCatalog
	session    *incrementalRenderSession
	shards     *incrementalResourceCatalogShards
	seed       maphash.Seed
	proof      *incrementalResourceCatalogProof
	base       *incrementalResourceCatalogSnapshot
	generation uint64
}

func newIncrementalResourceCatalogSnapshot(
	tree *iradix.Tree[struct{}],
) *incrementalResourceCatalogSnapshot {
	if tree == nil {
		tree = iradix.New[struct{}]()
	}
	snapshot := &incrementalResourceCatalogSnapshot{tree: tree}
	snapshot.seal = snapshot
	proof := &incrementalResourceCatalogSnapshotProof{owner: snapshot, tree: tree}
	proof.seal = proof
	snapshot.proof = proof
	return snapshot
}

func (s *incrementalResourceCatalogSnapshot) valid() bool {
	return s != nil && s.seal == s && s.tree != nil && s.proof != nil &&
		s.proof.seal == s.proof && s.proof.owner == s && s.proof.tree == s.tree
}

func (s *incrementalResourceCatalogSnapshot) Len() int {
	if !s.valid() {
		return 0
	}
	return s.tree.Len()
}

func (s *incrementalResourceCatalogSnapshot) Root() *iradix.Node[struct{}] {
	if !s.valid() {
		return nil
	}
	return s.tree.Root()
}

func newIncrementalResourceCatalog(
	session *incrementalRenderSession,
	base *incrementalResourceCatalogSnapshot,
) *incrementalResourceCatalog {
	if !base.valid() {
		base = newIncrementalResourceCatalogSnapshot(nil)
	}
	shards := &incrementalResourceCatalogShards{}
	shards.seal = shards
	for index := range incrementalResourceCatalogShardCount {
		shards.values[index].changes = map[incremental.InputKey]incrementalResourceCatalogMutation{}
	}
	catalog := &incrementalResourceCatalog{
		session: session, shards: shards, seed: maphash.MakeSeed(), base: base, generation: 1,
	}
	catalog.seal = catalog
	proof := &incrementalResourceCatalogProof{
		owner: catalog, session: session, shards: shards, seed: catalog.seed,
	}
	proof.seal = proof
	catalog.proof = proof
	return catalog
}

func (c *incrementalResourceCatalog) validFor(session *incrementalRenderSession) bool {
	return c != nil && c.seal == c && c.session == session && c.shards != nil &&
		c.shards.seal == c.shards && c.seed != (maphash.Seed{}) && c.proof != nil &&
		c.proof.seal == c.proof && c.proof.owner == c && c.proof.session == session &&
		c.proof.shards == c.shards && c.proof.seed == c.seed
}

func (c *incrementalResourceCatalog) shard(
	key incremental.InputKey,
) *incrementalResourceCatalogShard {
	index := maphash.Comparable(c.seed, key) & (incrementalResourceCatalogShardCount - 1)
	return &c.shards.values[index]
}

func (c *incrementalResourceCatalog) lockAll() {
	for index := range incrementalResourceCatalogShardCount {
		c.shards.values[index].mu.Lock()
	}
}

func (c *incrementalResourceCatalog) unlockAll() {
	for index := incrementalResourceCatalogShardCount - 1; index >= 0; index-- {
		c.shards.values[index].mu.Unlock()
	}
}

func (c *incrementalResourceCatalog) validStateLocked() bool {
	if !c.base.valid() || c.generation == 0 {
		return false
	}
	for index := range incrementalResourceCatalogShardCount {
		if c.shards.values[index].changes == nil {
			return false
		}
	}
	return true
}

func (c *incrementalResourceCatalog) reset(base *incrementalResourceCatalogSnapshot) bool {
	if !base.valid() {
		base = newIncrementalResourceCatalogSnapshot(nil)
	}
	c.lockAll()
	if c.generation == ^uint64(0) {
		c.unlockAll()
		return false
	}
	c.generation++
	c.base = base
	for index := range incrementalResourceCatalogShardCount {
		c.shards.values[index].changes = map[incremental.InputKey]incrementalResourceCatalogMutation{}
	}
	c.unlockAll()
	return true
}

func (m incrementalResourceCatalogMutation) validFor(
	catalog *incrementalResourceCatalog,
	key incremental.InputKey,
) bool {
	return m.owner == catalog && m.generation == catalog.generation && m.key == key &&
		(m.state == incrementalResourceCatalogPresent || m.state == incrementalResourceCatalogDeleted)
}

func (c *incrementalResourceCatalog) presenceLocked(
	shard *incrementalResourceCatalogShard,
	key incremental.InputKey,
) (bool, error) {
	if !c.base.valid() || c.generation == 0 || shard.changes == nil {
		return false, errors.New("incremental resource catalog has invalid ownership")
	}
	if mutation, changed := shard.changes[key]; changed {
		if !mutation.validFor(c, key) {
			return false, fmt.Errorf("incremental resource catalog entry %s has invalid provenance", key)
		}
		return mutation.state == incrementalResourceCatalogPresent, nil
	}
	_, exists := c.base.tree.Get([]byte(key.Opaque()))
	return exists, nil
}

func (c *incrementalResourceCatalog) setLocked(
	shard *incrementalResourceCatalogShard,
	key incremental.InputKey,
	state incrementalResourceCatalogMutationState,
) {
	shard.changes[key] = incrementalResourceCatalogMutation{
		owner: c, generation: c.generation, key: key, state: state,
	}
}

func (r *incrementalRenderSession) authenticatedCatalog() (*incrementalResourceCatalog, error) {
	if r == nil || !r.catalog.validFor(r) {
		return nil, errors.New("incremental resource catalog has invalid ownership")
	}
	return r.catalog, nil
}

func (r *incrementalRenderSession) catalogGet(
	key incremental.InputKey,
) (resourceInputSpec, bool, error) {
	spec, valid := canonicalResourceCatalogKey(key)
	if !valid {
		return resourceInputSpec{}, false,
			fmt.Errorf("incremental resource catalog key %s has invalid provenance", key)
	}
	catalog, err := r.authenticatedCatalog()
	if err != nil {
		return resourceInputSpec{}, false, err
	}
	shard := catalog.shard(key)
	shard.mu.RLock()
	exists, err := catalog.presenceLocked(shard, key)
	shard.mu.RUnlock()
	if err != nil {
		return resourceInputSpec{}, false, err
	}
	return spec, exists, nil
}

func (r *incrementalRenderSession) catalogLoadOrStore(
	key incremental.InputKey,
	candidate *resourceInputSpec,
) error {
	if resourceInputKey(candidate) != key {
		return fmt.Errorf("incremental resource catalog candidate %s has invalid provenance", key)
	}
	catalog, err := r.authenticatedCatalog()
	if err != nil {
		return err
	}
	shard := catalog.shard(key)
	shard.mu.RLock()
	present, err := catalog.presenceLocked(shard, key)
	shard.mu.RUnlock()
	if err != nil {
		return err
	}
	if present {
		return nil
	}
	shard.mu.Lock()
	present, err = catalog.presenceLocked(shard, key)
	if err == nil && !present {
		catalog.setLocked(shard, key, incrementalResourceCatalogPresent)
	}
	shard.mu.Unlock()
	return err
}

func (r *incrementalRenderSession) catalogInsert(
	key incremental.InputKey,
	spec *resourceInputSpec,
) error {
	if resourceInputKey(spec) != key {
		return fmt.Errorf("incremental resource catalog candidate %s has invalid provenance", key)
	}
	catalog, err := r.authenticatedCatalog()
	if err != nil {
		return err
	}
	shard := catalog.shard(key)
	shard.mu.Lock()
	if _, err = catalog.presenceLocked(shard, key); err == nil {
		catalog.setLocked(shard, key, incrementalResourceCatalogPresent)
	}
	shard.mu.Unlock()
	return err
}

func (r *incrementalRenderSession) catalogDelete(key incremental.InputKey) error {
	if _, valid := canonicalResourceCatalogKey(key); !valid {
		return fmt.Errorf("incremental resource catalog key %s has invalid provenance", key)
	}
	catalog, err := r.authenticatedCatalog()
	if err != nil {
		return err
	}
	shard := catalog.shard(key)
	shard.mu.Lock()
	if _, err = catalog.presenceLocked(shard, key); err == nil {
		catalog.setLocked(shard, key, incrementalResourceCatalogDeleted)
	}
	shard.mu.Unlock()
	return err
}

func (r *incrementalRenderSession) catalogHasPrefix(prefix []byte) (bool, error) {
	catalog, err := r.authenticatedCatalog()
	if err != nil {
		return false, err
	}
	catalog.lockAll()
	defer catalog.unlockAll()
	if !catalog.validStateLocked() {
		return false, errors.New("incremental resource catalog has invalid ownership")
	}
	changed, err := catalogChangesHavePrefix(catalog, string(prefix))
	if err != nil || changed {
		return changed, err
	}
	return catalogBaseHasPrefix(catalog, prefix)
}

func catalogChangesHavePrefix(
	catalog *incrementalResourceCatalog,
	prefix string,
) (bool, error) {
	for index := range incrementalResourceCatalogShardCount {
		for key, mutation := range catalog.shards.values[index].changes {
			if !mutation.validFor(catalog, key) {
				return false, fmt.Errorf(
					"incremental resource catalog entry %s has invalid provenance", key,
				)
			}
			if _, valid := canonicalResourceCatalogKey(key); !valid {
				return false, fmt.Errorf(
					"incremental resource catalog entry %s has invalid provenance", key,
				)
			}
			if mutation.state == incrementalResourceCatalogPresent &&
				strings.HasPrefix(key.Opaque(), prefix) {
				return true, nil
			}
		}
	}
	return false, nil
}

func catalogBaseHasPrefix(
	catalog *incrementalResourceCatalog,
	prefix []byte,
) (bool, error) {
	var walkErr error
	found := false
	catalog.base.tree.Root().WalkPrefix(prefix, func(rawKey []byte, _ struct{}) bool {
		key := incremental.NewInputKey(string(rawKey))
		_, valid := canonicalResourceCatalogKey(key)
		if !valid {
			walkErr = fmt.Errorf(
				"incremental resource catalog entry %s has invalid provenance", key,
			)
			return true
		}
		shard := catalog.shard(key)
		if mutation, changed := shard.changes[key]; changed {
			if !mutation.validFor(catalog, key) {
				walkErr = fmt.Errorf(
					"incremental resource catalog entry %s has invalid provenance", key,
				)
				return true
			}
			if mutation.state == incrementalResourceCatalogDeleted {
				return false
			}
		}
		found = true
		return true
	})
	return found, walkErr
}

func (r *incrementalRenderSession) catalogCommit() (*incrementalResourceCatalogSnapshot, error) {
	catalog, err := r.authenticatedCatalog()
	if err != nil {
		return nil, err
	}
	catalog.lockAll()
	defer catalog.unlockAll()
	if !catalog.validStateLocked() {
		return nil, errors.New("incremental resource catalog has invalid ownership")
	}
	txn := catalog.base.tree.Txn()
	for index := range incrementalResourceCatalogShardCount {
		for key, mutation := range catalog.shards.values[index].changes {
			if !mutation.validFor(catalog, key) {
				return nil, fmt.Errorf(
					"incremental resource catalog entry %s has invalid provenance", key,
				)
			}
			if _, valid := canonicalResourceCatalogKey(key); !valid {
				return nil, fmt.Errorf(
					"incremental resource catalog entry %s has invalid provenance", key,
				)
			}
			switch mutation.state {
			case incrementalResourceCatalogPresent:
				txn.Insert([]byte(key.Opaque()), struct{}{})
			case incrementalResourceCatalogDeleted:
				txn.Delete([]byte(key.Opaque()))
			default:
				return nil, fmt.Errorf(
					"incremental resource catalog entry %s has invalid provenance", key,
				)
			}
		}
	}
	return newIncrementalResourceCatalogSnapshot(txn.Commit()), nil
}

func (r *incrementalRenderSession) resetCatalog(base *incrementalResourceCatalogSnapshot) {
	if !base.valid() {
		base = newIncrementalResourceCatalogSnapshot(nil)
	}
	if r.catalog == nil || !r.catalog.validFor(r) {
		r.catalog = newIncrementalResourceCatalog(r, base)
		return
	}
	if !r.catalog.reset(base) {
		r.catalog = newIncrementalResourceCatalog(r, base)
	}
}

func canonicalResourceCatalogKey(key incremental.InputKey) (resourceInputSpec, bool) {
	spec, valid := parseResourceInputKey(key)
	return spec, valid && resourceInputKey(&spec) == key
}
