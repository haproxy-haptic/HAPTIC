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
	"reflect"
	"slices"

	iradix "github.com/hashicorp/go-immutable-radix/v2"

	"github.com/haproxy-haptic/HAPTIC/pkg/incremental"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
)

type exactCycleResourceScope struct {
	authority  *exactCycleResourceAuthority
	alias      string
	source     stores.RevisionSource
	sequence   uint64
	root       stores.ReadSnapshot
	proofs     *iradix.Tree[struct{}]
	proofsRoot *iradix.Node[struct{}]
	proofsLen  int
	auth       exactCycleResourceScopeAuthentication
}

type exactCycleResourceAuthority struct {
	seal *exactCycleResourceAuthority
}

type exactCycleResourceScopeAuthentication struct {
	authority  *exactCycleResourceAuthority
	alias      string
	source     stores.RevisionSource
	sequence   uint64
	root       stores.ReadSnapshot
	proofsRoot *iradix.Node[struct{}]
	proofsLen  int
}

type exactCycleResourceObservationsAuthentication struct {
	authority *exactCycleResourceAuthority
	root      *iradix.Node[exactCycleResourceScope]
	count     int
}

type exactCycleResourceObservations struct {
	authority  *exactCycleResourceAuthority
	scopes     *iradix.Tree[exactCycleResourceScope]
	scopesRoot *iradix.Node[exactCycleResourceScope]
	scopesLen  int
	auth       exactCycleResourceObservationsAuthentication
	seal       *exactCycleResourceObservations
}

type exactCycleResourceScopeBuild struct {
	root   stores.ReadSnapshot
	proofs *iradix.Txn[struct{}]
	count  int
}

type exactCycleStoreRoot struct {
	authority *exactCycleResourceAuthority
	alias     string
	source    stores.RevisionSource
	sequence  uint64
	auth      exactCycleStoreRootAuthentication
}

type exactCycleStoreRootAuthentication struct {
	authority *exactCycleResourceAuthority
	alias     string
	source    stores.RevisionSource
	sequence  uint64
}

type exactCycleStoreRootsAuthentication struct {
	authority *exactCycleResourceAuthority
	root      *iradix.Node[exactCycleStoreRoot]
	count     int
}

type exactCycleStoreRoots struct {
	authority *exactCycleResourceAuthority
	roots     *iradix.Tree[exactCycleStoreRoot]
	rootsRoot *iradix.Node[exactCycleStoreRoot]
	rootsLen  int
	auth      exactCycleStoreRootsAuthentication
	seal      *exactCycleStoreRoots
}

func newExactCycleResourceAuthority() *exactCycleResourceAuthority {
	authority := &exactCycleResourceAuthority{}
	authority.seal = authority
	return authority
}

func newEmptyExactCycleResourceObservations() *exactCycleResourceObservations {
	authority := newExactCycleResourceAuthority()
	scopes := iradix.New[exactCycleResourceScope]()
	result := &exactCycleResourceObservations{
		authority: authority, scopes: scopes, scopesRoot: scopes.Root(), scopesLen: scopes.Len(),
	}
	result.auth = exactCycleResourceObservationsAuthentication{
		authority: result.authority, root: result.scopesRoot, count: result.scopesLen,
	}
	result.seal = result
	return result
}

func (r *incrementalRenderSession) captureExactCycleStoreRoots() (*exactCycleStoreRoots, error) {
	if r == nil || !r.cachePublicationEnabled {
		return nil, errExactCycleUnavailable
	}
	authority := newExactCycleResourceAuthority()
	txn := iradix.New[exactCycleStoreRoot]().Txn()
	for alias, snapshot := range r.renderSnapshots {
		if alias == "" || snapshot == nil || snapshot.RevisionSource() == 0 {
			return nil, errExactCycleUnavailable
		}
		root := exactCycleStoreRoot{
			authority: authority, alias: alias, source: snapshot.RevisionSource(), sequence: snapshot.Sequence(),
		}
		root.auth = exactCycleStoreRootAuthentication{
			authority: root.authority, alias: root.alias, source: root.source, sequence: root.sequence,
		}
		if _, replaced := txn.Insert([]byte(alias), root); replaced {
			return nil, errors.New("exact cycle store root is duplicated")
		}
	}
	roots := txn.Commit()
	result := &exactCycleStoreRoots{
		authority: authority, roots: roots, rootsRoot: roots.Root(), rootsLen: roots.Len(),
	}
	result.auth = exactCycleStoreRootsAuthentication{
		authority: result.authority, root: result.rootsRoot, count: result.rootsLen,
	}
	result.seal = result
	return result, nil
}

func (r *exactCycleStoreRoots) matches(session *incrementalRenderSession) (bool, error) {
	if err := r.validate(); err != nil {
		return false, err
	}
	if session == nil || !session.cachePublicationEnabled || len(session.renderSnapshots) != r.rootsLen {
		return false, nil
	}
	matched := true
	var matchErr error
	r.roots.Root().Walk(func(key []byte, root exactCycleStoreRoot) bool {
		alias := string(key)
		if root.authority != r.authority || root.alias != alias || root.alias == "" || root.source == 0 ||
			root.authority != root.auth.authority || root.alias != root.auth.alias ||
			root.source != root.auth.source || root.sequence != root.auth.sequence {
			matchErr = errors.New("exact cycle store root has invalid provenance")
			return true
		}
		current := session.renderSnapshots[alias]
		if current == nil || current.RevisionSource() != root.source || current.Sequence() != root.sequence {
			matched = false
			return true
		}
		return false
	})
	return matched && matchErr == nil, matchErr
}

func (r *exactCycleStoreRoots) validate() error {
	if r == nil || r.seal != r || r.authority == nil || r.authority.seal != r.authority ||
		r.roots == nil || r.rootsRoot != r.roots.Root() || r.rootsLen != r.roots.Len() ||
		r.authority != r.auth.authority || r.rootsRoot != r.auth.root || r.rootsLen != r.auth.count {
		return errors.New("exact cycle store roots have invalid provenance")
	}
	return nil
}

func (o *exactCycleResourceObservations) rebase(
	session *incrementalRenderSession,
) (*exactCycleResourceObservations, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	if session == nil || !session.cachePublicationEnabled {
		return nil, errExactCycleUnavailable
	}
	authority := newExactCycleResourceAuthority()
	txn := iradix.New[exactCycleResourceScope]().Txn()
	var rebaseErr error
	o.scopes.Root().Walk(func(key []byte, previous exactCycleResourceScope) bool {
		alias := string(key)
		if previous.authority != o.authority {
			rebaseErr = errors.New("exact cycle resource scope has a foreign authority")
			return true
		}
		if err := previous.validate(alias); err != nil {
			rebaseErr = err
			return true
		}
		root := session.renderSnapshots[alias]
		if root == nil || root.RevisionSource() != previous.source ||
			!reflect.TypeOf(root).Comparable() {
			rebaseErr = errors.New("exact cycle resource scope cannot rebase its immutable root")
			return true
		}
		scope := exactCycleResourceScope{
			authority:  authority,
			alias:      alias,
			source:     root.RevisionSource(),
			sequence:   root.Sequence(),
			root:       root,
			proofs:     previous.proofs,
			proofsRoot: previous.proofsRoot,
			proofsLen:  previous.proofsLen,
		}
		scope.auth = exactCycleResourceScopeAuthentication{
			authority:  scope.authority,
			alias:      scope.alias,
			source:     scope.source,
			sequence:   scope.sequence,
			root:       scope.root,
			proofsRoot: scope.proofsRoot,
			proofsLen:  scope.proofsLen,
		}
		txn.Insert(key, scope)
		return false
	})
	if rebaseErr != nil {
		return nil, rebaseErr
	}
	scopes := txn.Commit()
	rebased := &exactCycleResourceObservations{
		authority:  authority,
		scopes:     scopes,
		scopesRoot: scopes.Root(),
		scopesLen:  scopes.Len(),
	}
	rebased.auth = exactCycleResourceObservationsAuthentication{
		authority: rebased.authority,
		root:      rebased.scopesRoot,
		count:     rebased.scopesLen,
	}
	rebased.seal = rebased
	if err := rebased.validate(); err != nil {
		return nil, err
	}
	return rebased, nil
}

func (r *incrementalRenderSession) captureExactCycleResourceObservations() (
	*exactCycleResourceObservations,
	error,
) {
	if r == nil || !r.cachePublicationEnabled {
		return nil, errExactCycleUnavailable
	}
	authority := newExactCycleResourceAuthority()
	r.mu.Lock()
	builds := make(map[string]*exactCycleResourceScopeBuild)
	for key, observation := range r.rootResourceProofs {
		spec, ok := parseResourceInputKey(key)
		if !ok || observation.Key != key {
			r.mu.Unlock()
			return nil, errors.New("exact cycle resource proof has an invalid key")
		}
		root := r.renderSnapshots[spec.resourceType]
		if root == nil || root.RevisionSource() == 0 {
			r.mu.Unlock()
			return nil, errExactCycleUnavailable
		}
		build := builds[spec.resourceType]
		if build == nil {
			build = &exactCycleResourceScopeBuild{root: root, proofs: iradix.New[struct{}]().Txn()}
			builds[spec.resourceType] = build
		}
		if build.root != root || observation.Revision.Opaque() == "" {
			r.mu.Unlock()
			return nil, errors.New("exact cycle resource proofs span multiple immutable roots")
		}
		if _, replaced := build.proofs.Insert([]byte(key.Opaque()), struct{}{}); replaced {
			r.mu.Unlock()
			return nil, errors.New("exact cycle resource proof is duplicated")
		}
		build.count++
	}
	if err := r.carryReusedRootResourceProofs(builds); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	r.mu.Unlock()

	scopeTxn := iradix.New[exactCycleResourceScope]().Txn()
	for alias, build := range builds {
		proofs := build.proofs.Commit()
		scope := exactCycleResourceScope{
			authority: authority,
			alias:     alias, source: build.root.RevisionSource(), sequence: build.root.Sequence(), root: build.root,
			proofs: proofs, proofsRoot: proofs.Root(), proofsLen: build.count,
		}
		if !reflect.TypeOf(scope.root).Comparable() {
			return nil, errExactCycleUnavailable
		}
		scope.auth = exactCycleResourceScopeAuthentication{
			authority: scope.authority, alias: scope.alias, source: scope.source, sequence: scope.sequence,
			root: scope.root, proofsRoot: scope.proofsRoot, proofsLen: scope.proofsLen,
		}
		scopeTxn.Insert([]byte(alias), scope)
	}
	scopes := scopeTxn.Commit()
	observations := &exactCycleResourceObservations{
		authority: authority, scopes: scopes, scopesRoot: scopes.Root(), scopesLen: scopes.Len(),
	}
	observations.auth = exactCycleResourceObservationsAuthentication{
		authority: observations.authority, root: observations.scopesRoot, count: observations.scopesLen,
	}
	observations.seal = observations
	return observations, nil
}

// carryReusedRootResourceProofs keeps a reused root's reads, which didn't run
// this cycle; reuse required them to match the current roots (#604).
func (r *incrementalRenderSession) carryReusedRootResourceProofs(builds map[string]*exactCycleResourceScopeBuild) error {
	if r.rootReuser.reused() == 0 {
		return nil
	}
	previous := r.rootReuser.candidate.resources
	if err := previous.validate(); err != nil {
		return err
	}
	var carryErr error
	previous.scopes.Root().Walk(func(key []byte, scope exactCycleResourceScope) bool {
		alias := string(key)
		root := r.renderSnapshots[alias]
		if root == nil || root.RevisionSource() != scope.source {
			carryErr = errExactCycleUnavailable
			return true
		}
		build := builds[alias]
		if build == nil {
			build = &exactCycleResourceScopeBuild{root: root, proofs: iradix.New[struct{}]().Txn()}
			builds[alias] = build
		}
		if build.root != root {
			carryErr = errors.New("exact cycle resource proofs span multiple immutable roots")
			return true
		}
		scope.proofs.Root().Walk(func(proof []byte, _ struct{}) bool {
			if _, replaced := build.proofs.Insert(proof, struct{}{}); !replaced {
				build.count++
			}
			return false
		})
		return false
	})
	return carryErr
}

func (o *exactCycleResourceObservations) matches(
	ctx context.Context,
	session *incrementalRenderSession,
) (bool, error) {
	if err := o.validate(); err != nil {
		return false, err
	}
	if session == nil || !session.cachePublicationEnabled {
		return false, nil
	}
	matched := true
	var matchErr error
	o.scopes.Root().Walk(func(key []byte, scope exactCycleResourceScope) bool {
		scopeMatched, err := o.scopeMatches(ctx, session, string(key), &scope)
		if err != nil {
			matchErr = err
			return true
		}
		if !scopeMatched {
			matched = false
			return true
		}
		return false
	})
	return matched && matchErr == nil, matchErr
}

func (o *exactCycleResourceObservations) scopeMatches(
	ctx context.Context,
	session *incrementalRenderSession,
	alias string,
	scope *exactCycleResourceScope,
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if scope.authority != o.authority {
		return false, errors.New("exact cycle resource scope has a foreign authority")
	}
	if err := scope.validate(alias); err != nil {
		return false, err
	}
	current := session.renderSnapshots[alias]
	store := session.baseStores[alias]
	if current == nil || store == nil || current.RevisionSource() != scope.source {
		return false, nil
	}
	if current.Sequence() == scope.sequence {
		return true, nil
	}
	journal, ok := store.(stores.ExactRevisionJournal)
	if !ok || journal.ExactRevisionJournalSource() != scope.source {
		return false, nil
	}
	changes, complete := journalChangesThrough(journal, scope.sequence, current.Sequence())
	if !complete {
		return false, nil
	}
	deltas, exact := newResourceIdentityDeltas(changes)
	if !exact {
		return false, nil
	}
	affected, err := scope.affected(changes)
	if err != nil {
		return false, err
	}
	orderedCollections := stores.HasIdentityOrderedReads(scope.root) &&
		stores.HasIdentityOrderedReads(current)
	if !orderedCollections {
		same, err := scope.sameCollectionProofs(ctx, current)
		if err != nil {
			return false, err
		}
		if !same {
			return false, nil
		}
	}
	return deltas.sameAffectedProofs(ctx, scope.root, current, affected, orderedCollections)
}

func (s *exactCycleResourceScope) sameCollectionProofs(
	ctx context.Context,
	current stores.ReadSnapshot,
) (bool, error) {
	collections, err := s.collectionProofs()
	if err != nil {
		return false, err
	}
	for index := range collections {
		same, err := sameExactCycleResourceScope(ctx, s.root, current, &collections[index])
		if err != nil {
			return false, err
		}
		if !same {
			return false, nil
		}
	}
	return true, nil
}

func (d *resourceIdentityDeltas) sameAffectedProofs(
	ctx context.Context,
	original stores.ReadSnapshot,
	current stores.ReadSnapshot,
	affected []resourceInputSpec,
	orderedCollections bool,
) (bool, error) {
	for index := range affected {
		proof := &affected[index]
		if !orderedCollections &&
			(proof.scope == resourceInputList || proof.scope == resourceInputGet) {
			continue
		}
		same, err := d.sameScopeSemantics(ctx, original, current, proof)
		if err != nil {
			return false, err
		}
		if !same {
			return false, nil
		}
	}
	return true, nil
}

func (s *exactCycleResourceScope) collectionProofs() ([]resourceInputSpec, error) {
	result := make([]resourceInputSpec, 0, s.proofsLen)
	var proofErr error
	s.proofs.Root().Walk(func(key []byte, _ struct{}) bool {
		inputKey := incremental.NewInputKey(string(key))
		spec, ok := parseResourceInputKey(inputKey)
		if !ok || spec.resourceType != s.alias || resourceInputKey(&spec) != inputKey {
			proofErr = errors.New("exact cycle resource proof has invalid provenance")
			return true
		}
		if spec.scope == resourceInputList || spec.scope == resourceInputGet {
			result = append(result, spec)
		}
		return false
	})
	return result, proofErr
}

func sameExactCycleResourceScope(
	ctx context.Context,
	original stores.ReadSnapshot,
	current stores.ReadSnapshot,
	spec *resourceInputSpec,
) (bool, error) {
	before, err := readResourceSnapshotInput(ctx, original, spec)
	if errors.Is(err, stores.ErrSnapshotChanged) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	after, err := readResourceSnapshotInput(ctx, current, spec)
	if errors.Is(err, stores.ErrSnapshotChanged) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return before.Found == after.Found && slices.Equal(before.Value, after.Value), nil
}

func (o *exactCycleResourceObservations) validate() error {
	if o == nil || o.seal != o || o.authority == nil || o.authority.seal != o.authority ||
		o.scopes == nil || o.scopesRoot != o.scopes.Root() || o.scopesLen != o.scopes.Len() ||
		o.authority != o.auth.authority || o.scopesRoot != o.auth.root || o.scopesLen != o.auth.count {
		return errors.New("exact cycle resource observations have invalid provenance")
	}
	return nil
}

func (s *exactCycleResourceScope) validate(alias string) error {
	if s.authority == nil || s.authority.seal != s.authority || s.alias != alias || s.alias == "" ||
		s.source == 0 || s.root == nil || !reflect.TypeOf(s.root).Comparable() ||
		s.root.RevisionSource() != s.source || s.root.Sequence() != s.sequence || s.proofs == nil ||
		s.proofsRoot != s.proofs.Root() || s.proofsLen != s.proofs.Len() ||
		s.authority != s.auth.authority || s.alias != s.auth.alias || s.source != s.auth.source ||
		s.sequence != s.auth.sequence || s.root != s.auth.root || s.proofsRoot != s.auth.proofsRoot ||
		s.proofsLen != s.auth.proofsLen {
		return errors.New("exact cycle resource scope has invalid provenance")
	}
	return nil
}

func (s *exactCycleResourceScope) affected(
	changes []stores.RevisionChange,
) ([]resourceInputSpec, error) {
	affected := make(map[incremental.InputKey]resourceInputSpec)
	for index := range changes {
		for _, spec := range resourceInputCandidates(s.alias, &changes[index]) {
			key := resourceInputKey(&spec)
			if _, found := s.proofs.Root().Get([]byte(key.Opaque())); found {
				affected[key] = spec
			}
		}
	}
	keys := sortedResourceInputSpecs(affected)
	result := make([]resourceInputSpec, 0, len(keys))
	for _, key := range keys {
		spec, ok := parseResourceInputKey(key)
		if !ok || spec.resourceType != s.alias || resourceInputKey(&spec) != key {
			return nil, errors.New("exact cycle resource proof has invalid provenance")
		}
		result = append(result, spec)
	}
	return result, nil
}
