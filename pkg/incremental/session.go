package incremental

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync/atomic"

	"github.com/haproxy-haptic/HAPTIC/pkg/incremental/internal/orderedset"
)

const (
	sessionPublicationOpen uint32 = iota
	sessionPublicationActive
	sessionPublicationAborted
	sessionPublicationClosed
)

// Session isolates speculative input and query state until commit.
type Session struct {
	graph *Graph
	// base is the committed generation this session reads. It is retained,
	// not re-read from the graph, so a session that never commits (an
	// admission render) keeps a consistent view while other sessions commit;
	// a commit still compares baseGeneration against the graph's current one.
	base               *graphGeneration
	baseGeneration     uint64
	targetGeneration   uint64
	cold               bool
	replacement        bool
	resolver           InputResolver
	resolverConcurrent bool
	started            bool
	verifying          bool
	committing         bool
	failure            error
	publicationState   atomic.Uint32

	baseInputs    map[InputKey]inputEntry
	inputChanges  map[InputKey]inputEntry
	inputVersions map[inputVersionKey]inputEntry
	baseNodes     map[QueryKey]nodeEntry
	nodeChanges   map[QueryKey]nodeEntry

	observations            map[InputKey]InputRevision
	active                  map[QueryKey]int
	stack                   []QueryKey
	queried                 map[QueryKey]struct{}
	queriedBatches          [][]QueryKey
	activeColdExactBatch    *coldExactBatchState
	stagedReverse           map[dependencyKey]orderedset.Root
	replacementReverse      map[dependencyKey]orderedset.Root
	replacementReverseReady bool
	counterDeltas           map[QueryKey]NodeCounters
	bulkCounterDeltas       []queryCounterDelta
	removedQueries          map[QueryKey]struct{}
	retiredInputs           []InputKey
	preparedGraphCommit     atomic.Pointer[preparedGraphCommit]
}

type queryCounterDelta struct {
	key   QueryKey
	delta NodeCounters
}

type inputVersionKey struct {
	key      InputKey
	revision Revision
}

// BaseGeneration returns the generation on which this transaction is based.
func (s *Session) BaseGeneration() uint64 {
	return s.baseGeneration
}

// ExactInput returns the transaction's current immutable value for key.
func (s *Session) ExactInput(key InputKey) (Input, bool, error) {
	if err := s.ready(); err != nil {
		return Input{}, false, err
	}
	entry, exists, err := s.currentInput(key)
	if err != nil || !exists {
		return Input{}, exists, err
	}
	return Input{
		Key:      key,
		Revision: entry.revision,
		Found:    entry.found,
		Value:    cloneBytes(entry.value),
	}, true, nil
}

// MatchesExactInput compares a caller-owned snapshot without copying the
// graph's immutable bytes.
func (s *Session) MatchesExactInput(expected Input) (bool, error) {
	if err := s.ready(); err != nil {
		return false, err
	}
	if !validInputKey(expected.Key) || !validRevision(expected.Revision) ||
		(!expected.Found && len(expected.Value) != 0) {
		return false, fmt.Errorf("incremental input comparison has an invalid snapshot")
	}
	entry, exists, err := s.borrowInput(expected.Key)
	if err != nil || !exists {
		return false, err
	}
	return entry.revision == expected.Revision && sameInputValue(entry, expected), nil
}

// ValidateBaseExactValue verifies identity with the query root captured by this transaction.
func (s *Session) ValidateBaseExactValue(key QueryKey, root ExactValueRoot) error {
	if err := s.ready(); err != nil {
		return err
	}
	if err := root.validateOwned(s.graph.valueAuthority, key); err != nil {
		return err
	}
	entry, exists, err := s.baseNode(key)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("incremental query has no transaction-base exact value")
	}
	if entry.value.value != root.value {
		return fmt.Errorf("incremental exact value is not the transaction-base query root")
	}
	return nil
}

// ValidateCurrentExactValue verifies identity with the query root currently staged by this transaction.
func (s *Session) ValidateCurrentExactValue(key QueryKey, root ExactValueRoot) error {
	if err := s.ready(); err != nil {
		return err
	}
	if err := root.validateOwned(s.graph.valueAuthority, key); err != nil {
		return err
	}
	entry, exists, err := s.currentNode(key)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("incremental query has no transaction-current exact value")
	}
	if entry.value.value != root.value {
		return fmt.Errorf("incremental exact value is not the transaction-current query root")
	}
	return nil
}

// HasInputDependents reports whether the transaction retains a direct dependent of key.
func (s *Session) HasInputDependents(key InputKey) (bool, error) {
	if err := s.ready(); err != nil {
		return false, err
	}
	return s.hasCurrentDependents(inputDep(key))
}

// RetiredInputs returns inputs removed by a successful commit.
func (s *Session) RetiredInputs() []InputKey {
	return append([]InputKey(nil), s.retiredInputs...)
}

func nodeDependsOn(entry *nodeEntry, key dependencyKey) bool {
	for _, dependency := range entry.deps {
		if dependency.key == key {
			return true
		}
	}
	return false
}

// ApplyInputs stages an atomic invalidation batch before query evaluation starts.
func (s *Session) ApplyInputs(inputs ...Input) error {
	if err := s.readyMutation(); err != nil {
		return err
	}
	if s.started {
		return s.fail(fmt.Errorf("incremental inputs cannot change after evaluation starts"))
	}
	_, err := s.applyInputsAtomically(inputs)
	return err
}

// ApplyInputsWhileIdle stages inputs between query evaluations and returns newly dirty queries.
func (s *Session) ApplyInputsWhileIdle(inputs ...Input) ([]QueryKey, error) {
	if err := s.readyMutation(); err != nil {
		return nil, err
	}
	if len(s.active) != 0 || len(s.stack) != 0 || s.activeColdExactBatch != nil {
		return nil, s.fail(fmt.Errorf("incremental inputs cannot change during query evaluation"))
	}
	return s.applyInputsAtomically(inputs)
}

func (s *Session) applyInputsAtomically(inputs []Input) ([]QueryKey, error) {
	if err := validateInputBatch(inputs); err != nil {
		return nil, s.fail(err)
	}
	batch, err := s.prepareInputBatch(inputs)
	if err != nil {
		return nil, s.fail(err)
	}
	dirtyEntries, newlyDirty, err := s.planInputInvalidations(batch.changed)
	if err != nil {
		return nil, s.fail(err)
	}
	s.publishInputBatch(batch)
	for _, key := range newlyDirty {
		entry := dirtyEntries[key]
		if err := s.stageNodeChange(key, &entry); err != nil {
			return nil, s.fail(err)
		}
		s.addCounters(key, NodeCounters{Invalidations: 1})
	}
	return newlyDirty, nil
}

type inputBatch struct {
	changes      map[InputKey]inputEntry
	versions     map[inputVersionKey]inputEntry
	observations map[InputKey]InputRevision
	changed      []InputKey
}

func (s *Session) prepareInputBatch(inputs []Input) (*inputBatch, error) {
	batch := &inputBatch{
		changes:      make(map[InputKey]inputEntry, len(inputs)),
		versions:     make(map[inputVersionKey]inputEntry, len(inputs)),
		observations: make(map[InputKey]InputRevision, len(inputs)),
		changed:      make([]InputKey, 0, len(inputs)),
	}
	for _, input := range inputs {
		current, exists, err := s.currentInput(input.Key)
		if err != nil {
			return nil, err
		}
		baseline, baselineExists, err := s.baseInput(input.Key)
		if err != nil {
			return nil, err
		}
		if err := s.validateInputVersion(input, current, exists, baseline, baselineExists); err != nil {
			return nil, err
		}
		if exists && current.revision == input.Revision {
			continue
		}
		entry := inputEntry{
			revision:  input.Revision,
			found:     input.Found,
			value:     cloneBytes(input.Value),
			changedAt: s.inputChangedAt(input, current, exists, baseline, baselineExists),
		}
		batch.changes[input.Key] = entry
		batch.versions[inputVersionKey{key: input.Key, revision: input.Revision}] = entry
		batch.observations[input.Key] = revisionOf(input)
		batch.changed = append(batch.changed, input.Key)
	}
	sortInputKeys(batch.changed)
	return batch, nil
}

func (s *Session) inputChangedAt(
	input Input,
	current inputEntry,
	currentExists bool,
	baseline inputEntry,
	baselineExists bool,
) uint64 {
	if baselineExists && baseline.found == input.Found && bytes.Equal(baseline.value, input.Value) {
		return baseline.changedAt
	}
	if !baselineExists && currentExists && current.found == input.Found && bytes.Equal(current.value, input.Value) {
		return current.changedAt
	}
	return s.targetGeneration
}

func (s *Session) publishInputBatch(batch *inputBatch) {
	for key, entry := range batch.changes {
		s.inputChanges[key] = entry
		s.observations[key] = batch.observations[key]
	}
	for key, entry := range batch.versions {
		s.inputVersions[key] = entry
	}
}

func (s *Session) validateInputVersion(
	input Input,
	current inputEntry,
	currentExists bool,
	baseline inputEntry,
	baselineExists bool,
) error {
	if currentExists && current.revision == input.Revision && !sameInputValue(current, input) {
		return fmt.Errorf("incremental input reused an exact revision for different bytes")
	}
	if baselineExists && baseline.revision == input.Revision && !sameInputValue(baseline, input) {
		return fmt.Errorf("incremental input reused an exact revision for different bytes")
	}
	if previous, exists := s.inputVersions[inputVersionKey{key: input.Key, revision: input.Revision}]; exists && !sameInputValue(previous, input) {
		return fmt.Errorf("incremental input reused an exact revision for different bytes")
	}
	return nil
}

func sameInputValue(entry inputEntry, input Input) bool {
	return entry.found == input.Found && bytes.Equal(entry.value, input.Value)
}

func sameImmutableInputValue(entry inputEntry, input ImmutableInput) bool {
	if entry.found != input.Found || len(entry.value) != len(input.Value) {
		return false
	}
	for index := range entry.value {
		if entry.value[index] != input.Value[index] {
			return false
		}
	}
	return true
}

// RemoveQueries transactionally evicts queries and their cached dependents.
func (s *Session) RemoveQueries(keys ...QueryKey) error {
	if err := s.readyMutation(); err != nil {
		return err
	}
	if s.started {
		return s.fail(fmt.Errorf("incremental queries cannot be removed after evaluation starts"))
	}
	return s.removeQueries(keys)
}

// RemoveQueriesWhileIdle transactionally evicts queries between evaluations.
func (s *Session) RemoveQueriesWhileIdle(keys ...QueryKey) error {
	if err := s.readyMutation(); err != nil {
		return err
	}
	if len(s.active) != 0 || len(s.stack) != 0 || s.activeColdExactBatch != nil {
		return s.fail(fmt.Errorf("incremental queries cannot be removed during query evaluation"))
	}
	return s.removeQueries(keys)
}

func (s *Session) removeQueries(keys []QueryKey) error {
	queue := newQueryKeyQueue()
	for _, key := range keys {
		if !validQueryKey(key) {
			return s.fail(fmt.Errorf("incremental query key is empty"))
		}
		queue.Add(key)
	}
	for {
		key, exists := queue.Pop()
		if !exists {
			break
		}
		if _, removed := s.removedQueries[key]; removed {
			continue
		}
		s.removedQueries[key] = struct{}{}
		if err := s.unstageNodeChange(key); err != nil {
			return s.fail(err)
		}
		dependents, err := s.loadDependents(queryDep(key))
		if err != nil {
			return s.fail(err)
		}
		for _, dependent := range dependents {
			if _, removed := s.removedQueries[dependent]; !removed {
				queue.Add(dependent)
			}
		}
	}
	return nil
}

// DirtyQueries returns the sorted cached queries requiring evaluation.
func (s *Session) DirtyQueries() ([]QueryKey, error) {
	if err := s.readyMutation(); err != nil {
		return nil, err
	}
	if s.started {
		return nil, s.fail(fmt.Errorf("incremental dirty queries must be read before evaluation starts"))
	}
	if s.cold {
		return []QueryKey{}, nil
	}

	dirty := map[QueryKey]struct{}{}
	s.graph.mu.RLock()
	if !s.baseValidLocked() {
		s.graph.mu.RUnlock()
		return nil, s.fail(errInvalidBaseGeneration)
	}
	s.base.dirty.Root().Walk(func(key string, _ struct{}) bool {
		dirty[NewQueryKey(key)] = struct{}{}
		return false
	})
	s.graph.mu.RUnlock()
	for key, entry := range s.nodeChanges {
		if entry.dirty {
			dirty[key] = struct{}{}
		} else {
			delete(dirty, key)
		}
	}
	for key := range s.removedQueries {
		delete(dirty, key)
	}

	result := make([]QueryKey, 0, len(dirty))
	for key := range dirty {
		result = append(result, key)
	}
	sortQueryKeys(result)
	return result, nil
}

func (s *Session) applyColdInputs(inputs []Input) error {
	return s.ApplyInputs(inputs...)
}

func validateInputBatch(inputs []Input) error {
	seen := make(map[InputKey]struct{}, len(inputs))
	for _, input := range inputs {
		if !validInputKey(input.Key) {
			return fmt.Errorf("incremental input key is empty")
		}
		if !validRevision(input.Revision) {
			return fmt.Errorf("incremental input revision is empty")
		}
		if !input.Found && len(input.Value) != 0 {
			return fmt.Errorf("incremental negative input has bytes")
		}
		if _, duplicate := seen[input.Key]; duplicate {
			return fmt.Errorf("incremental input batch contains a duplicate key")
		}
		seen[input.Key] = struct{}{}
	}
	return nil
}

func validateImmutableInput(input ImmutableInput) error {
	if !validInputKey(input.Key) {
		return fmt.Errorf("incremental input key is empty")
	}
	if !validRevision(input.Revision) {
		return fmt.Errorf("incremental input revision is empty")
	}
	if !input.Found && input.Value != "" {
		return fmt.Errorf("incremental negative input has bytes")
	}
	return nil
}

func revisionOf(input Input) InputRevision {
	return InputRevision{Key: input.Key, Revision: input.Revision, Found: input.Found}
}

func (s *Session) planInputInvalidations(inputs []InputKey) (
	dirtyEntries map[QueryKey]nodeEntry,
	newlyDirty []QueryKey,
	err error,
) {
	queue, err := s.invalidationQueue(inputs)
	if err != nil {
		return nil, nil, err
	}
	visited := map[QueryKey]struct{}{}
	dirtyEntries = map[QueryKey]nodeEntry{}
	for {
		key, exists := queue.Pop()
		if !exists {
			break
		}
		if _, done := visited[key]; done {
			continue
		}
		visited[key] = struct{}{}
		newlyInvalidated, err := s.invalidateQuery(key, dirtyEntries)
		if err != nil {
			return nil, nil, err
		}
		if newlyInvalidated {
			newlyDirty = append(newlyDirty, key)
		}
		if err := s.enqueueUnvisitedDependents(queue, queryDep(key), visited); err != nil {
			return nil, nil, err
		}
	}
	sortQueryKeys(newlyDirty)
	return dirtyEntries, newlyDirty, nil
}

func (s *Session) invalidationQueue(inputs []InputKey) (*queryKeyQueue, error) {
	queue := newQueryKeyQueue()
	for _, key := range inputs {
		dependents, err := s.loadDependents(inputDep(key))
		if err != nil {
			return nil, err
		}
		for _, dependent := range dependents {
			queue.Add(dependent)
		}
	}
	return queue, nil
}

func (s *Session) invalidateQuery(
	key QueryKey,
	dirtyEntries map[QueryKey]nodeEntry,
) (bool, error) {
	if s.wasQueried(key) {
		return false, fmt.Errorf("incremental input invalidated query %s after it was evaluated", key)
	}
	entry, exists, err := s.currentNode(key)
	if err != nil || !exists || entry.dirty {
		return false, err
	}
	entry.dirty = true
	dirtyEntries[key] = entry
	return true, nil
}

func (s *Session) enqueueUnvisitedDependents(
	queue *queryKeyQueue,
	dependency dependencyKey,
	visited map[QueryKey]struct{},
) error {
	dependents, err := s.loadDependents(dependency)
	if err != nil {
		return err
	}
	for _, dependent := range dependents {
		if _, done := visited[dependent]; !done {
			queue.Add(dependent)
		}
	}
	return nil
}

func (s *Session) currentInput(key InputKey) (inputEntry, bool, error) {
	entry, exists, err := s.borrowInput(key)
	return cloneInputEntry(entry), exists, err
}

func (s *Session) borrowInput(key InputKey) (inputEntry, bool, error) {
	if entry, exists := s.inputChanges[key]; exists {
		return entry, true, nil
	}
	if s.cold {
		return inputEntry{}, false, nil
	}
	if entry, exists := s.baseInputs[key]; exists {
		return entry, true, nil
	}

	s.graph.mu.RLock()
	defer s.graph.mu.RUnlock()
	if !s.baseValidLocked() {
		return inputEntry{}, false, errInvalidBaseGeneration
	}
	committed, exists := s.base.inputs.Root().Get([]byte(key.id.String()))
	if exists {
		entry := openCommittedInputEntry(committed)
		s.baseInputs[key] = entry
		return entry, true, nil
	}
	return inputEntry{}, false, nil
}

// baseValidLocked reports whether the retained base generation still carries
// its provenance. Callers hold graph.mu.
func (s *Session) baseValidLocked() bool {
	return s.base != nil && s.base.valid(s.graph)
}

func (s *Session) baseInput(key InputKey) (inputEntry, bool, error) {
	if s.cold {
		return inputEntry{}, false, nil
	}
	if entry, exists := s.baseInputs[key]; exists {
		return cloneInputEntry(entry), true, nil
	}

	s.graph.mu.RLock()
	defer s.graph.mu.RUnlock()
	if !s.baseValidLocked() {
		return inputEntry{}, false, errInvalidBaseGeneration
	}
	committed, exists := s.base.inputs.Root().Get([]byte(key.id.String()))
	if exists {
		entry := openCommittedInputEntry(committed)
		s.baseInputs[key] = entry
		return cloneInputEntry(entry), true, nil
	}
	return inputEntry{}, false, nil
}

func (s *Session) currentNode(key QueryKey) (nodeEntry, bool, error) {
	if _, removed := s.removedQueries[key]; removed {
		return nodeEntry{}, false, nil
	}
	if entry, exists := s.nodeChanges[key]; exists {
		if err := entry.value.validateOwned(s.graph.valueAuthority, key); err != nil {
			return nodeEntry{}, false, err
		}
		return cloneNodeEntry(&entry), true, nil
	}
	return s.baseNode(key)
}

func (s *Session) baseNode(key QueryKey) (nodeEntry, bool, error) {
	if s.cold {
		return nodeEntry{}, false, nil
	}
	if entry, exists := s.baseNodes[key]; exists {
		if err := entry.value.validateOwned(s.graph.valueAuthority, key); err != nil {
			return nodeEntry{}, false, err
		}
		return cloneNodeEntry(&entry), true, nil
	}

	s.graph.mu.RLock()
	defer s.graph.mu.RUnlock()
	if !s.baseValidLocked() {
		return nodeEntry{}, false, errInvalidBaseGeneration
	}
	committed, exists := s.base.nodes.Root().Get([]byte(key.id.String()))
	if exists {
		entry, err := openCommittedNodeEntry(s.graph, key, committed)
		if err != nil {
			return nodeEntry{}, false, err
		}
		s.baseNodes[key] = entry
		return cloneNodeEntry(&entry), true, nil
	}
	return nodeEntry{}, false, nil
}

func (s *Session) loadDependents(key dependencyKey) ([]QueryKey, error) {
	committed, staged, err := s.currentReverseRoots(key)
	if err != nil {
		return nil, err
	}
	committedValues, err := committed.Values(s.graph.reverseAuthority, reverseScope(key))
	if err != nil {
		return nil, fmt.Errorf("loading committed reverse dependencies: %w", err)
	}
	stagedValues, err := staged.Values(s.graph.reverseAuthority, reverseScope(key))
	if err != nil {
		return nil, fmt.Errorf("loading staged reverse dependencies: %w", err)
	}
	committedKeys, err := s.filterCommittedDependents(key, committedValues)
	if err != nil {
		return nil, err
	}
	stagedKeys, err := s.filterStagedDependents(stagedValues)
	if err != nil {
		return nil, err
	}
	return mergeQueryKeys(committedKeys, stagedKeys), nil
}

func (s *Session) hasCurrentDependents(key dependencyKey) (bool, error) {
	committed, staged, err := s.currentReverseRoots(key)
	if err != nil {
		return false, err
	}
	invalid := false
	found := false
	err = committed.Range(s.graph.reverseAuthority, reverseScope(key), func(value string) bool {
		dependent := NewQueryKey(value)
		if !validQueryKey(dependent) {
			invalid = true
			return false
		}
		if _, removed := s.removedQueries[dependent]; removed {
			return true
		}
		if changed, exists := s.nodeChanges[dependent]; exists && !nodeDependsOn(&changed, key) {
			return true
		}
		found = true
		return false
	})
	if err != nil {
		return false, fmt.Errorf("checking committed reverse dependencies: %w", err)
	}
	if invalid {
		return false, fmt.Errorf("incremental reverse dependency has an empty query key")
	}
	if found {
		return true, nil
	}
	err = staged.Range(s.graph.reverseAuthority, reverseScope(key), func(value string) bool {
		dependent := NewQueryKey(value)
		if !validQueryKey(dependent) {
			invalid = true
			return false
		}
		if _, removed := s.removedQueries[dependent]; removed {
			return true
		}
		found = true
		return false
	})
	if err != nil {
		return false, fmt.Errorf("checking staged reverse dependencies: %w", err)
	}
	if invalid {
		return false, fmt.Errorf("incremental reverse dependency has an empty query key")
	}
	return found, nil
}

func (s *Session) currentReverseRoots(
	key dependencyKey,
) (committed, staged orderedset.Root, err error) {
	committed = s.graph.reverseAuthority.Empty()
	if s.replacement {
		staged, err = s.replacementReverseRoot(key)
		if err != nil {
			return orderedset.Root{}, orderedset.Root{}, err
		}
		return committed, staged, nil
	}
	if !s.cold {
		s.graph.mu.RLock()
		if !s.baseValidLocked() {
			s.graph.mu.RUnlock()
			return orderedset.Root{}, orderedset.Root{}, errInvalidBaseGeneration
		}
		committed, err = s.graph.reverseRootOfLocked(s.base, key)
		s.graph.mu.RUnlock()
		if err != nil {
			return orderedset.Root{}, orderedset.Root{}, err
		}
	}
	staged, err = s.stagedReverseRoot(key)
	if err != nil {
		return orderedset.Root{}, orderedset.Root{}, err
	}
	return committed, staged, nil
}

func (s *Session) stagedReverseRoot(key dependencyKey) (orderedset.Root, error) {
	root, exists := s.stagedReverse[key]
	if !exists {
		root = s.graph.reverseAuthority.Empty()
	}
	if err := root.ValidateOwnership(s.graph.reverseAuthority, reverseScope(key)); err != nil {
		return orderedset.Root{}, fmt.Errorf("incremental staged reverse dependency: %w", err)
	}
	return root, nil
}

func (s *Session) replacementReverseRoot(key dependencyKey) (orderedset.Root, error) {
	roots, err := s.replacementReverseRoots()
	if err != nil {
		return orderedset.Root{}, err
	}
	root, exists := roots[key]
	if !exists {
		root = s.graph.reverseAuthority.Empty()
	}
	if err := root.ValidateOwnership(s.graph.reverseAuthority, reverseScope(key)); err != nil {
		return orderedset.Root{}, fmt.Errorf("incremental replacement reverse dependency: %w", err)
	}
	return root, nil
}

func (s *Session) replacementReverseRoots() (map[dependencyKey]orderedset.Root, error) {
	if !s.replacement {
		return nil, fmt.Errorf("incremental replacement reverse dependencies are unavailable")
	}
	if s.replacementReverseReady {
		return s.replacementReverse, nil
	}
	roots, err := buildReplacementReverseRoots(
		s.graph.reverseAuthority,
		s.nodeChanges,
		s.removedQueries,
	)
	if err != nil {
		return nil, err
	}
	s.replacementReverse = roots
	s.replacementReverseReady = true
	return roots, nil
}

func buildReplacementReverseRoots(
	authority *orderedset.Authority,
	nodes map[QueryKey]nodeEntry,
	removed map[QueryKey]struct{},
) (map[dependencyKey]orderedset.Root, error) {
	keys := sortedNodeEntryKeys(nodes)
	counts := make(map[dependencyKey]int)
	for _, key := range keys {
		if !validQueryKey(key) {
			return nil, fmt.Errorf("incremental reverse dependency has an empty query key")
		}
		if _, isRemoved := removed[key]; isRemoved {
			continue
		}
		for _, dependency := range nodes[key].deps {
			counts[dependency.key]++
		}
	}

	dependents := make(map[dependencyKey][]string, len(counts))
	for dependency, count := range counts {
		dependents[dependency] = make([]string, 0, count)
	}
	for _, key := range keys {
		if _, isRemoved := removed[key]; isRemoved {
			continue
		}
		for _, dependency := range nodes[key].deps {
			values := dependents[dependency.key]
			if len(values) > 0 && values[len(values)-1] == key.id.String() {
				return nil, fmt.Errorf(
					"incremental query %s contains a duplicate dependency",
					key,
				)
			}
			dependents[dependency.key] = append(values, key.id.String())
		}
	}

	roots := make(map[dependencyKey]orderedset.Root, len(dependents))
	for dependency, values := range dependents {
		root, err := orderedset.BuildPackedSorted(authority, reverseScope(dependency), values)
		if err != nil {
			return nil, fmt.Errorf("building incremental replacement reverse dependencies: %w", err)
		}
		roots[dependency] = root
	}
	return roots, nil
}

func (s *Session) invalidateReplacementReverse() {
	if !s.replacement {
		return
	}
	s.replacementReverse = nil
	s.replacementReverseReady = false
}

func (s *Session) filterCommittedDependents(
	dependency dependencyKey,
	values []string,
) ([]QueryKey, error) {
	result := make([]QueryKey, 0, len(values))
	for _, value := range values {
		key := NewQueryKey(value)
		if !validQueryKey(key) {
			return nil, fmt.Errorf("incremental reverse dependency has an empty query key")
		}
		if _, removed := s.removedQueries[key]; removed {
			continue
		}
		if staged, changed := s.nodeChanges[key]; changed && !nodeDependsOn(&staged, dependency) {
			continue
		}
		result = append(result, key)
	}
	return result, nil
}

func (s *Session) filterStagedDependents(values []string) ([]QueryKey, error) {
	result := make([]QueryKey, 0, len(values))
	for _, value := range values {
		key := NewQueryKey(value)
		if !validQueryKey(key) {
			return nil, fmt.Errorf("incremental reverse dependency has an empty query key")
		}
		if _, removed := s.removedQueries[key]; !removed {
			result = append(result, key)
		}
	}
	return result, nil
}

func mergeQueryKeys(left, right []QueryKey) []QueryKey {
	merged := make([]QueryKey, 0, len(left)+len(right))
	for len(left) != 0 && len(right) != 0 {
		switch {
		case left[0].id.String() < right[0].id.String():
			merged = append(merged, left[0])
			left = left[1:]
		case left[0].id.String() > right[0].id.String():
			merged = append(merged, right[0])
			right = right[1:]
		default:
			merged = append(merged, left[0])
			left = left[1:]
			right = right[1:]
		}
	}
	merged = append(merged, left...)
	return append(merged, right...)
}

type queryKeyQueue struct {
	values []QueryKey
	queued map[QueryKey]struct{}
}

func newQueryKeyQueue() *queryKeyQueue {
	return &queryKeyQueue{queued: map[QueryKey]struct{}{}}
}

func (q *queryKeyQueue) Add(key QueryKey) {
	if _, exists := q.queued[key]; exists {
		return
	}
	q.queued[key] = struct{}{}
	q.values = append(q.values, key)
	for index := len(q.values) - 1; index > 0; {
		parent := (index - 1) / 2
		if q.values[parent].id.String() <= q.values[index].id.String() {
			break
		}
		q.values[parent], q.values[index] = q.values[index], q.values[parent]
		index = parent
	}
}

func (q *queryKeyQueue) Pop() (QueryKey, bool) {
	if len(q.values) == 0 {
		return QueryKey{}, false
	}
	result := q.values[0]
	delete(q.queued, result)
	last := len(q.values) - 1
	q.values[0] = q.values[last]
	q.values = q.values[:last]
	for index := 0; ; {
		left := index*2 + 1
		if left >= len(q.values) {
			break
		}
		right := left + 1
		smallest := left
		if right < len(q.values) && q.values[right].id.String() < q.values[left].id.String() {
			smallest = right
		}
		if q.values[index].id.String() <= q.values[smallest].id.String() {
			break
		}
		q.values[index], q.values[smallest] = q.values[smallest], q.values[index]
		index = smallest
	}
	return result, true
}

func (s *Session) stageNodeChange(key QueryKey, entry *nodeEntry) error {
	if err := s.unstageNodeChange(key); err != nil {
		return err
	}
	s.nodeChanges[key] = *entry
	if s.replacement {
		s.invalidateReplacementReverse()
		return nil
	}
	for _, dependency := range entry.deps {
		root, err := s.stagedReverseRoot(dependency.key)
		if err != nil {
			return err
		}
		next, changed, err := root.Add(
			s.graph.reverseAuthority,
			reverseScope(dependency.key),
			key.id.String(),
		)
		if err != nil {
			return fmt.Errorf("staging incremental reverse dependency of query %s: %w", key, err)
		}
		if !changed {
			return fmt.Errorf("incremental staged reverse dependency already contains query %s", key)
		}
		s.stagedReverse[dependency.key] = next
	}
	return nil
}

func (s *Session) unstageNodeChange(key QueryKey) error {
	entry, exists := s.nodeChanges[key]
	if !exists {
		return nil
	}
	if s.replacement {
		delete(s.nodeChanges, key)
		s.invalidateReplacementReverse()
		return nil
	}
	for _, dependency := range entry.deps {
		root, err := s.stagedReverseRoot(dependency.key)
		if err != nil {
			return err
		}
		next, changed, err := root.Delete(
			s.graph.reverseAuthority,
			reverseScope(dependency.key),
			key.id.String(),
		)
		if err != nil {
			return fmt.Errorf("unstaging incremental reverse dependency of query %s: %w", key, err)
		}
		if !changed {
			return fmt.Errorf("incremental staged reverse dependency does not contain query %s", key)
		}
		size, err := next.Len(s.graph.reverseAuthority, reverseScope(dependency.key))
		if err != nil {
			return fmt.Errorf("unstaging incremental reverse dependency: %w", err)
		}
		if size == 0 {
			delete(s.stagedReverse, dependency.key)
		} else {
			s.stagedReverse[dependency.key] = next
		}
	}
	delete(s.nodeChanges, key)
	return nil
}

func (s *Session) addCounters(key QueryKey, delta NodeCounters) {
	s.counterDeltas[key] = addCounters(s.counterDeltas[key], delta)
}

func (s *Session) addBulkCounters(keys []QueryKey, delta NodeCounters) {
	s.bulkCounterDeltas = slices.Grow(s.bulkCounterDeltas, len(keys))
	for _, key := range keys {
		s.bulkCounterDeltas = append(s.bulkCounterDeltas, queryCounterDelta{key: key, delta: delta})
	}
}

func (s *Session) wasQueried(key QueryKey) bool {
	if _, exists := s.queried[key]; exists {
		return true
	}
	for _, batch := range s.queriedBatches {
		_, exists := slices.BinarySearchFunc(batch, key, func(left, right QueryKey) int {
			return cmp.Compare(left.id.String(), right.id.String())
		})
		if exists {
			return true
		}
	}
	return false
}

func (s *Session) observe(inputs []InputRevision) error {
	for _, input := range inputs {
		current, exists := s.observations[input.Key]
		if exists && current != input {
			return fmt.Errorf("incremental query observed inconsistent input revisions")
		}
		s.observations[input.Key] = input
	}
	return nil
}

func (s *Session) ready() error {
	switch s.publicationState.Load() {
	case sessionPublicationOpen, sessionPublicationActive:
	case sessionPublicationAborted, sessionPublicationClosed:
		return ErrSessionClosed
	default:
		return ErrSessionClosed
	}
	if s.failure != nil {
		return s.failure
	}
	return nil
}

func (s *Session) readyMutation() error {
	if s.verifying || s.committing {
		return ErrSessionClosed
	}
	return s.ready()
}

func (s *Session) readyContext(ctx context.Context) error {
	if err := s.ready(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return s.fail(err)
	}
	return nil
}

func (s *Session) readyMutationContext(ctx context.Context) error {
	if err := s.readyMutation(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return s.fail(err)
	}
	return nil
}

func (s *Session) fail(err error) error {
	if s.failure == nil {
		s.failure = err
	}
	return s.failure
}

func (s *Session) beginPublication(ctx context.Context) error {
	if !s.publicationState.CompareAndSwap(sessionPublicationOpen, sessionPublicationActive) {
		return ErrSessionClosed
	}
	if err := s.readyMutationContext(ctx); err != nil {
		s.discard()
		return err
	}
	return nil
}

func (s *Session) publicationActive() error {
	if s.publicationState.Load() != sessionPublicationActive {
		return ErrSessionClosed
	}
	return nil
}

func (s *Session) requestPublicationAbort() {
	for {
		switch state := s.publicationState.Load(); state {
		case sessionPublicationOpen:
			if s.publicationState.CompareAndSwap(state, sessionPublicationClosed) {
				s.discard()
				return
			}
		case sessionPublicationActive:
			if s.publicationState.CompareAndSwap(state, sessionPublicationAborted) {
				return
			}
		case sessionPublicationAborted, sessionPublicationClosed:
			return
		default:
			return
		}
	}
}

func (s *Session) completePublication() bool {
	return s.publicationState.CompareAndSwap(sessionPublicationActive, sessionPublicationClosed)
}

// Abort discards all speculative state.
func (s *Session) Abort() {
	if prepared := s.preparedGraphCommit.Load(); prepared != nil {
		_ = prepared.abort()
		return
	}
	s.requestPublicationAbort()
}

func (s *Session) discard() {
	s.publicationState.Store(sessionPublicationClosed)
	s.verifying = false
	s.committing = false
	s.baseInputs = nil
	s.inputChanges = nil
	s.inputVersions = nil
	s.baseNodes = nil
	s.nodeChanges = nil
	s.observations = nil
	s.queried = nil
	s.queriedBatches = nil
	s.activeColdExactBatch = nil
	s.stagedReverse = nil
	s.replacementReverse = nil
	s.counterDeltas = nil
	s.bulkCounterDeltas = nil
	s.removedQueries = nil
	s.preparedGraphCommit.Store(nil)
}

// BaseHasDependents reports whether a committed query in the generation this
// session began on depends on key.
func (s *Session) BaseHasDependents(key QueryKey) bool {
	s.graph.mu.RLock()
	defer s.graph.mu.RUnlock()
	return s.baseValidLocked() && s.graph.hasDependentsOfLocked(s.base, queryDep(key))
}

// BaseHasInputDependents reports whether a committed query in the generation
// this session began on depends on the input key.
func (s *Session) BaseHasInputDependents(key InputKey) bool {
	s.graph.mu.RLock()
	defer s.graph.mu.RUnlock()
	return s.baseValidLocked() && s.graph.hasDependentsOfLocked(s.base, inputDep(key))
}

// BaseValue returns a query's committed value in the generation this session
// began on, or false when the base has no clean value for it. It is the
// session-consistent counterpart of Graph.Value, which reads the graph's
// current generation and so can be ahead of a session another commit passed.
func (s *Session) BaseValue(key QueryKey) ([]byte, bool) {
	s.graph.mu.RLock()
	defer s.graph.mu.RUnlock()
	if !s.baseValidLocked() {
		return nil, false
	}
	entry, exists := s.base.nodes.Root().Get([]byte(key.id.String()))
	if !exists || entry.dirty {
		return nil, false
	}
	if err := entry.value.validateOwned(s.graph.valueAuthority, key); err != nil {
		return nil, false
	}
	value, err := entry.value.Bytes()
	return value, err == nil
}
