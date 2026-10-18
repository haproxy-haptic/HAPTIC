package incremental

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"sync/atomic"
)

const coldExactBatchInputShardCount = 256

const (
	coldExactBatchValueUnset uint32 = iota
	coldExactBatchValueBinding
	coldExactBatchValueReady
)

// ColdExactBatchFunc computes every query in one cold columnar execution.
type ColdExactBatchFunc func(context.Context, ColdExactBatch) error

// ColdExactBatch exposes sorted query handles without allocating one reader per query.
type ColdExactBatch struct {
	state *coldExactBatchState
}

// ColdExactBatchValue binds one batch index to its exact query value.
type ColdExactBatchValue struct {
	Index int
	Key   QueryKey
	Value string
}

// CompleteWave initializes no value unless every member passes preflight.
func (b ColdExactBatch) CompleteWave(values ...ColdExactBatchValue) ([]ExactResult, error) {
	state := b.state
	if state == nil || state.seal != state {
		return nil, fmt.Errorf("incremental cold batch has invalid authority")
	}
	return state.completeWave(values)
}

// SealWave authenticates and freezes one completed dependency wave atomically.
func (b ColdExactBatch) SealWave(results ...ExactResult) error {
	state := b.state
	if state == nil || state.seal != state {
		return fmt.Errorf("incremental cold batch has invalid authority")
	}
	return state.sealWave(results)
}

// Len returns the number of queries in the batch.
func (b ColdExactBatch) Len() int {
	if b.state == nil || b.state.seal != b.state {
		return 0
	}
	return b.state.count
}

// Query returns the independently tracked query at index.
func (b ColdExactBatch) Query(index int) ColdExactBatchQuery {
	if b.state == nil || b.state.seal != b.state || index < 0 || index >= b.state.count {
		return ColdExactBatchQuery{}
	}
	b.state.lifetime.RLock()
	defer b.state.lifetime.RUnlock()
	if b.state.revoked {
		return ColdExactBatchQuery{state: b.state, index: index}
	}
	return ColdExactBatchQuery{state: b.state, index: index, key: b.state.keys[index]}
}

// ColdExactBatchQuery is a query-bound dependency reader and value authority.
type ColdExactBatchQuery struct {
	state *coldExactBatchState
	index int
	key   QueryKey
}

// Key returns the opaque query identity bound to this handle.
func (q ColdExactBatchQuery) Key() QueryKey {
	return q.key
}

// Complete records the query's only value and returns its authenticated root.
func (q ColdExactBatchQuery) Complete(value string) (ExactValueRoot, error) {
	state, err := q.begin()
	if err != nil {
		return ExactValueRoot{}, err
	}
	defer state.lifetime.RUnlock()
	if err := state.queryError(q.index); err != nil {
		return ExactValueRoot{}, err
	}
	if !state.completions[q.index].CompareAndSwap(
		coldExactBatchValueUnset,
		coldExactBatchValueBinding,
	) {
		return ExactValueRoot{}, fmt.Errorf("incremental cold batch query already has a value")
	}
	slot := &state.slots[q.index]
	slot.execution = exactValueExecution{authority: state.session.graph.valueAuthority, key: q.key}
	slot.execution.seal = &slot.execution
	root := initializeExactValueRoot(
		&slot.value,
		state.session.graph.valueAuthority,
		q.key,
		value,
		&slot.execution,
	)
	state.completions[q.index].Store(coldExactBatchValueReady)
	return root, nil
}

func (q ColdExactBatchQuery) Input(key InputKey) (value []byte, found bool, err error) {
	state, err := q.begin()
	if err != nil {
		return nil, false, err
	}
	defer state.lifetime.RUnlock()
	input, err := state.exactInputOwned(q.index, key)
	if err != nil {
		return nil, false, err
	}
	return input.Value, input.Found, nil
}

func (q ColdExactBatchQuery) ExactInput(key InputKey) (Input, error) {
	return q.ExactInputOwned(key)
}

func (q ColdExactBatchQuery) ExactInputOwned(key InputKey) (Input, error) {
	state, err := q.begin()
	if err != nil {
		return Input{}, err
	}
	defer state.lifetime.RUnlock()
	return state.exactInputOwned(q.index, key)
}

func (q ColdExactBatchQuery) ObserveExactInput(expected InputRevision) error {
	state, err := q.begin()
	if err != nil {
		return err
	}
	defer state.lifetime.RUnlock()
	if !validInputKey(expected.Key) || !validRevision(expected.Revision) {
		return state.failQuery(q.index, fmt.Errorf("incremental input observation has an invalid identity"))
	}
	entry, err := state.loadInput(q.index, expected.Key)
	if err != nil {
		return err
	}
	if entry.revision != expected.Revision || entry.found != expected.Found {
		return state.failQuery(q.index, ErrRevisionConflict)
	}
	return state.recordInput(q.index, expected.Key, entry)
}

func (ColdExactBatchQuery) exactInputObserver() {}

func (q ColdExactBatchQuery) ObserveExactInputValue(expected Input) error {
	state, err := q.begin()
	if err != nil {
		return err
	}
	defer state.lifetime.RUnlock()
	if err := validateInputBatch([]Input{expected}); err != nil {
		return state.failQuery(q.index, err)
	}
	entry, err := state.loadInput(q.index, expected.Key)
	if err != nil {
		return err
	}
	if entry.revision != expected.Revision || !sameInputValue(entry, expected) {
		return state.failQuery(q.index, ErrRevisionConflict)
	}
	return state.recordInput(q.index, expected.Key, entry)
}

func (ColdExactBatchQuery) exactInputValueObserver() {}

func (q ColdExactBatchQuery) ObserveExactImmutableInput(expected ImmutableInput) error {
	state, err := q.begin()
	if err != nil {
		return err
	}
	defer state.lifetime.RUnlock()
	if err := validateImmutableInput(expected); err != nil {
		return state.failQuery(q.index, err)
	}
	entry, err := state.loadInput(q.index, expected.Key)
	if err != nil {
		return err
	}
	if entry.revision != expected.Revision || !sameImmutableInputValue(entry, expected) {
		return state.failQuery(q.index, ErrRevisionConflict)
	}
	return state.recordInput(q.index, expected.Key, entry)
}

func (ColdExactBatchQuery) exactImmutableInputObserver() {}

func (q ColdExactBatchQuery) Query(ctx context.Context, key QueryKey) ([]byte, error) {
	value, _, err := q.query(ctx, key, false)
	return value, err
}

// QueryWithExactObservation reads one dependency value and returns an opaque
// observation that sibling queries can reuse without reading the value again.
func (q ColdExactBatchQuery) QueryWithExactObservation(
	ctx context.Context,
	key QueryKey,
) ([]byte, ExactQueryObservation, error) {
	return q.query(ctx, key, true)
}

func (q ColdExactBatchQuery) query(
	ctx context.Context,
	key QueryKey,
	withObservation bool,
) ([]byte, ExactQueryObservation, error) {
	state, err := q.begin()
	if err != nil {
		return nil, ExactQueryObservation{}, err
	}
	defer state.lifetime.RUnlock()
	var entry nodeEntry
	var observed *nodeEntry
	if index, member := state.index(key); member {
		observed, err = observedSealedBatchMember(ctx, state, q.index, key, index)
		if err != nil {
			return nil, ExactQueryObservation{}, err
		}
	} else {
		if err := state.queryError(q.index); err != nil {
			return nil, ExactQueryObservation{}, err
		}
		select {
		case <-state.queryToken:
		case <-ctx.Done():
			return nil, ExactQueryObservation{}, state.failQuery(q.index, ctx.Err())
		}
		defer func() { state.queryToken <- struct{}{} }()
		if err := state.queryError(q.index); err != nil {
			return nil, ExactQueryObservation{}, err
		}
		if err := ctx.Err(); err != nil {
			return nil, ExactQueryObservation{}, state.failQuery(q.index, err)
		}
		entry, err = state.session.evaluateNode(ctx, key)
		if err != nil {
			return nil, ExactQueryObservation{}, state.failQuery(q.index, err)
		}
		observed = &entry
	}
	return q.finishQuery(state, key, observed, withObservation)
}

func observedSealedBatchMember(
	ctx context.Context,
	state *coldExactBatchState,
	queryIndex int,
	key QueryKey,
	index int,
) (*nodeEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, state.failQuery(queryIndex, err)
	}
	if !state.sealed[index].Load() {
		return nil, state.failQuery(
			queryIndex,
			fmt.Errorf("incremental query cannot depend on another batch member before it is sealed"),
		)
	}
	observed := &state.entries[index]
	if err := observed.value.validateOwned(state.session.graph.valueAuthority, key); err != nil {
		return nil, state.failQuery(queryIndex, err)
	}
	return observed, nil
}

func (q ColdExactBatchQuery) finishQuery(
	state *coldExactBatchState,
	key QueryKey,
	observed *nodeEntry,
	withObservation bool,
) ([]byte, ExactQueryObservation, error) {
	var observation ExactQueryObservation
	if withObservation {
		var err error
		observation, err = newExactQueryObservation(state, key, observed)
		if err != nil {
			return nil, ExactQueryObservation{}, state.failQuery(q.index, err)
		}
	}
	if err := state.recordQuery(q.index, key, observed); err != nil {
		return nil, ExactQueryObservation{}, err
	}
	value, err := observed.value.Bytes()
	if err != nil {
		return nil, ExactQueryObservation{}, state.failQuery(q.index, err)
	}
	return value, observation, nil
}

// ObserveExactQuery records an authenticated query dependency and its
// transitive inputs without reading or copying the query value.
func (q ColdExactBatchQuery) ObserveExactQuery(observation ExactQueryObservation) error {
	state, err := q.begin()
	if err != nil {
		return err
	}
	defer state.lifetime.RUnlock()
	if err := observation.validateLocked(state); err != nil {
		return state.failQuery(q.index, err)
	}
	key := observation.key
	var observed *nodeEntry
	if index, member := state.index(key); member {
		if !state.sealed[index].Load() {
			return state.failQuery(q.index, fmt.Errorf(
				"incremental exact query observation references an unsealed batch member",
			))
		}
		observed = &state.entries[index]
	} else {
		if err := state.queryError(q.index); err != nil {
			return err
		}
		select {
		case <-state.queryToken:
		case <-state.ctx.Done():
			return state.failQuery(q.index, state.ctx.Err())
		}
		entry, found, loadErr := state.session.currentNode(key)
		state.queryToken <- struct{}{}
		if loadErr != nil {
			return state.failQuery(q.index, loadErr)
		}
		if !found || entry.dirty {
			return state.failQuery(q.index, ErrRevisionConflict)
		}
		observed = &entry
	}
	if err := observed.value.validateOwned(state.session.graph.valueAuthority, key); err != nil {
		return state.failQuery(q.index, err)
	}
	if !observation.matches(observed) {
		return state.failQuery(q.index, ErrRevisionConflict)
	}
	return state.recordQuery(q.index, key, observed)
}

func (ColdExactBatchQuery) exactQueryObserver() {}

func (q ColdExactBatchQuery) begin() (*coldExactBatchState, error) {
	state := q.state
	if state == nil || state.seal != state || q.index < 0 || q.index >= state.count {
		return nil, fmt.Errorf("incremental cold batch query has invalid authority")
	}
	state.lifetime.RLock()
	if state.revoked {
		state.lifetime.RUnlock()
		return nil, fmt.Errorf("incremental cold batch query is no longer active")
	}
	if q.key != state.keys[q.index] {
		state.lifetime.RUnlock()
		return nil, fmt.Errorf("incremental cold batch query has invalid authority")
	}
	if len(state.slots) != state.count {
		state.lifetime.RUnlock()
		return nil, fmt.Errorf("incremental cold batch query has invalid authority")
	}
	if state.sealed[q.index].Load() {
		state.lifetime.RUnlock()
		return nil, fmt.Errorf("incremental cold batch query is sealed")
	}
	return state, nil
}

type coldExactBatchState struct {
	seal                 *coldExactBatchState
	session              *Session
	ctx                  context.Context
	observationAuthority exactQueryObservationAuthority
	count                int
	keys                 []QueryKey
	frames               []coldExactBatchFrame
	slots                []coldExactBatchValueSlot
	entries              []nodeEntry
	completions          []atomic.Uint32
	sealed               []atomic.Bool
	knownInputs          map[InputKey]inputEntry
	inputShards          [coldExactBatchInputShardCount]coldExactBatchInputShard

	lifetime           sync.RWMutex
	revoked            bool
	sealAttempted      bool
	sealedCount        int
	failure            error
	observationFailure atomic.Pointer[coldExactBatchFailure]
	queryToken         chan struct{}
	resolverToken      chan struct{}
}

type coldExactBatchValueSlot struct {
	execution exactValueExecution
	value     exactValue
}

type coldExactBatchFrame struct {
	gate    sync.Mutex
	frame   coldDependencyFrame
	failure atomic.Pointer[coldExactBatchFailure]
}

const coldDependencyFrameInlineCount = 4

type coldDependencyFrame struct {
	dependencySmall [coldDependencyFrameInlineCount]dependency
	inputSmall      [coldDependencyFrameInlineCount]InputRevision
	dependencyCount int
	inputCount      int
	dependencies    []dependency
	inputs          []InputRevision
	dependencyMap   map[dependencyKey]int
	inputMap        map[InputKey]int
}

type coldExactBatchFailure struct {
	err error
}

type coldExactBatchInputShard struct {
	gate   sync.Mutex
	inputs map[InputKey]*coldExactBatchInputResolution
}

type coldExactBatchInputResolution struct {
	ready chan struct{}
	entry inputEntry
	err   error
}

func newColdExactBatchState(
	session *Session,
	ctx context.Context,
	keys []QueryKey,
) *coldExactBatchState {
	state := &coldExactBatchState{
		session:     session,
		ctx:         ctx,
		count:       len(keys),
		keys:        keys,
		frames:      make([]coldExactBatchFrame, len(keys)),
		slots:       make([]coldExactBatchValueSlot, len(keys)),
		entries:     make([]nodeEntry, len(keys)),
		completions: make([]atomic.Uint32, len(keys)),
		sealed:      make([]atomic.Bool, len(keys)),
		knownInputs: session.inputChanges,
		queryToken:  make(chan struct{}, 1),
	}
	state.queryToken <- struct{}{}
	if !session.resolverConcurrent {
		state.resolverToken = make(chan struct{}, 1)
		state.resolverToken <- struct{}{}
	}
	state.seal = state
	initializeExactQueryObservationAuthority(&state.observationAuthority, state, session)
	return state
}

func (s *coldExactBatchState) index(key QueryKey) (int, bool) {
	return slices.BinarySearchFunc(s.keys, key, func(left, right QueryKey) int {
		return cmp.Compare(left.id.String(), right.id.String())
	})
}

func (s *coldExactBatchState) completeWave(values []ColdExactBatchValue) ([]ExactResult, error) {
	s.lifetime.Lock()
	defer s.lifetime.Unlock()
	if s.revoked || s.session == nil {
		return nil, fmt.Errorf("incremental cold batch is no longer active")
	}
	if !s.validCompletionAuthority() || !s.validCompletionStorage() {
		return nil, fmt.Errorf("incremental cold batch has invalid authority")
	}
	if s.failure != nil {
		return nil, s.failure
	}
	if failure := s.observationFailure.Load(); failure != nil {
		return nil, failure.err
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("incremental cold batch completion wave is empty")
	}
	if err := s.preflightCompletionValues(values); err != nil {
		return nil, err
	}
	return s.initializeCompletionValues(values), nil
}

func (s *coldExactBatchState) validCompletionAuthority() bool {
	if s == nil || s.session == nil || s.session.graph == nil {
		return false
	}
	authority := &s.observationAuthority
	return s.seal == s && s.session.activeColdExactBatch == s &&
		authority.seal == authority && authority.state == s && authority.session == s.session &&
		authority.graph == s.session.graph &&
		authority.baseGeneration == s.session.baseGeneration &&
		authority.targetGeneration == s.session.targetGeneration &&
		s.session.graph.valueAuthority != nil &&
		s.session.graph.valueAuthority.seal == s.session.graph.valueAuthority
}

func (s *coldExactBatchState) validCompletionStorage() bool {
	return s.count > 0 && len(s.keys) == s.count && len(s.frames) == s.count &&
		len(s.slots) == s.count && len(s.entries) == s.count &&
		len(s.completions) == s.count && len(s.sealed) == s.count
}

func (s *coldExactBatchState) preflightCompletionValues(values []ColdExactBatchValue) error {
	previous := -1
	for valueIndex := range values {
		value := &values[valueIndex]
		if err := s.preflightCompletionValue(value, previous); err != nil {
			return err
		}
		previous = value.Index
	}
	return nil
}

func (s *coldExactBatchState) preflightCompletionValue(
	value *ColdExactBatchValue,
	previous int,
) error {
	if value.Index < 0 || value.Index >= s.count || value.Index <= previous {
		return fmt.Errorf("incremental cold batch completion wave is not in batch order")
	}
	if !validQueryKey(value.Key) || !validQueryKey(s.keys[value.Index]) ||
		value.Key != s.keys[value.Index] {
		return fmt.Errorf("incremental cold batch completion wave has invalid authority")
	}
	if s.sealed[value.Index].Load() {
		return fmt.Errorf("incremental cold batch query is sealed")
	}
	if err := s.queryError(value.Index); err != nil {
		return err
	}
	if s.completions[value.Index].Load() != coldExactBatchValueUnset {
		return fmt.Errorf("incremental cold batch query already has a value")
	}
	if s.slots[value.Index] != (coldExactBatchValueSlot{}) {
		return fmt.Errorf("incremental cold batch value slot has invalid provenance")
	}
	return nil
}

func (s *coldExactBatchState) initializeCompletionValues(
	values []ColdExactBatchValue,
) []ExactResult {
	results := make([]ExactResult, len(values))
	for valueIndex := range values {
		s.completions[values[valueIndex].Index].Store(coldExactBatchValueBinding)
	}
	for valueIndex := range values {
		results[valueIndex] = s.initializeCompletionValue(&values[valueIndex])
	}
	for valueIndex := range values {
		s.completions[values[valueIndex].Index].Store(coldExactBatchValueReady)
	}
	return results
}

func (s *coldExactBatchState) initializeCompletionValue(value *ColdExactBatchValue) ExactResult {
	slot := &s.slots[value.Index]
	slot.execution = exactValueExecution{
		authority: s.session.graph.valueAuthority,
		key:       value.Key,
	}
	slot.execution.seal = &slot.execution
	root := initializeExactValueRoot(
		&slot.value,
		s.session.graph.valueAuthority,
		value.Key,
		value.Value,
		&slot.execution,
	)
	return ExactResult{Key: value.Key, Value: root}
}

func (s *coldExactBatchState) sealWave(results []ExactResult) error {
	s.lifetime.Lock()
	defer s.lifetime.Unlock()
	s.sealAttempted = true
	if s.revoked || s.session == nil {
		return fmt.Errorf("incremental cold batch is no longer active")
	}
	if s.failure != nil {
		return s.failure
	}
	if len(results) == 0 {
		return s.failSeal(fmt.Errorf("incremental cold batch wave is empty"))
	}
	indexes := make([]int, len(results))
	prepared := make([]nodeEntry, len(results))
	previous := -1
	for resultIndex := range results {
		result := results[resultIndex]
		index, exists := s.index(result.Key)
		if !exists {
			return s.failSeal(fmt.Errorf("incremental cold batch wave contains a foreign query"))
		}
		if index <= previous {
			return s.failSeal(fmt.Errorf("incremental cold batch wave is not in batch order"))
		}
		if s.sealed[index].Load() {
			return s.failSeal(fmt.Errorf("incremental cold batch query is already sealed"))
		}
		entry, err := s.prepareEntry(index, &result.Value)
		if err != nil {
			return s.failSeal(err)
		}
		indexes[resultIndex] = index
		prepared[resultIndex] = entry
		previous = index
	}
	if err := detachNodeDependencyStorage(prepared); err != nil {
		return s.failSeal(err)
	}
	s.releasePreparedFrames(indexes)
	if err := s.installWave(indexes, prepared); err != nil {
		return s.failSeal(err)
	}
	return nil
}

func (s *coldExactBatchState) finish() error {
	s.lifetime.Lock()
	defer s.lifetime.Unlock()
	if s.revoked || s.session == nil {
		return fmt.Errorf("incremental cold batch is no longer active")
	}
	s.revoked = true
	if s.failure != nil {
		return s.failure
	}
	if !s.sealAttempted {
		indexes := make([]int, s.count)
		prepared := make([]nodeEntry, s.count)
		for index := range s.count {
			entry, err := s.prepareEntry(index, nil)
			if err != nil {
				return s.failSeal(err)
			}
			indexes[index] = index
			prepared[index] = entry
		}
		if err := detachNodeDependencyStorage(prepared); err != nil {
			return s.failSeal(err)
		}
		s.releasePreparedFrames(indexes)
		if err := s.installWave(indexes, prepared); err != nil {
			return s.failSeal(err)
		}
	}
	if s.sealedCount != s.count {
		return s.failSeal(fmt.Errorf("incremental cold batch did not seal every query"))
	}
	return nil
}

func (s *coldExactBatchState) releasePreparedFrames(indexes []int) {
	for _, index := range indexes {
		s.frames[index].frame = coldDependencyFrame{}
	}
}

func (f *coldDependencyFrame) addInput(key InputKey, entry inputEntry) error {
	dep := dependency{
		key:       inputDep(key),
		changedAt: entry.changedAt,
		revision:  entry.revision,
		found:     entry.found,
	}
	if err := f.addDependency(dep, "incremental query observed one input at multiple revisions"); err != nil {
		return err
	}
	return f.addInputRevision(InputRevision{Key: key, Revision: entry.revision, Found: entry.found})
}

func (f *coldDependencyFrame) addQuery(key QueryKey, entry *nodeEntry) error {
	dep := dependency{key: queryDep(key), changedAt: entry.changedAt}
	if err := f.addDependency(dep, "incremental query observed one dependency at multiple revisions"); err != nil {
		return err
	}
	for _, input := range entry.inputs {
		if err := f.addInputRevision(input); err != nil {
			return err
		}
	}
	return nil
}

func (f *coldDependencyFrame) sortedDependencies() []dependency {
	dependencies := f.dependencyValues()
	sortDependencies(dependencies)
	return dependencies
}

func (f *coldDependencyFrame) sortedInputs() []InputRevision {
	inputs := f.inputValues()
	slices.SortFunc(inputs, func(left, right InputRevision) int {
		return cmp.Compare(left.Key.id.String(), right.Key.id.String())
	})
	return inputs
}

func (f *coldDependencyFrame) addDependency(candidate dependency, conflict string) error {
	if f.dependencyMap != nil {
		if index, exists := f.dependencyMap[candidate.key]; exists {
			if f.dependencies[index] != candidate {
				return errors.New(conflict)
			}
			return nil
		}
		f.dependencyMap[candidate.key] = len(f.dependencies)
		f.dependencies = append(f.dependencies, candidate)
		return nil
	}
	for _, current := range f.dependencyValues() {
		if current.key == candidate.key {
			if current != candidate {
				return errors.New(conflict)
			}
			return nil
		}
	}
	if f.dependencies != nil {
		if len(f.dependencies) < dependencyFrameMapThreshold {
			f.dependencies = append(f.dependencies, candidate)
			return nil
		}
		f.dependencyMap = make(map[dependencyKey]int, len(f.dependencies)+1)
		for index := range f.dependencies {
			f.dependencyMap[f.dependencies[index].key] = index
		}
		f.dependencyMap[candidate.key] = len(f.dependencies)
		f.dependencies = append(f.dependencies, candidate)
		return nil
	}
	if f.dependencyCount < len(f.dependencySmall) {
		f.dependencySmall[f.dependencyCount] = candidate
		f.dependencyCount++
		return nil
	}
	f.dependencies = make([]dependency, f.dependencyCount, dependencyFrameMapThreshold)
	copy(f.dependencies, f.dependencySmall[:f.dependencyCount])
	f.dependencies = append(f.dependencies, candidate)
	return nil
}

func (f *coldDependencyFrame) addInputRevision(candidate InputRevision) error {
	if f.inputMap != nil {
		if index, exists := f.inputMap[candidate.Key]; exists {
			if f.inputs[index] != candidate {
				return errors.New("incremental query observed inconsistent input revisions")
			}
			return nil
		}
		f.inputMap[candidate.Key] = len(f.inputs)
		f.inputs = append(f.inputs, candidate)
		return nil
	}
	for _, current := range f.inputValues() {
		if current.Key == candidate.Key {
			if current != candidate {
				return errors.New("incremental query observed inconsistent input revisions")
			}
			return nil
		}
	}
	if f.inputs != nil {
		if len(f.inputs) < dependencyFrameMapThreshold {
			f.inputs = append(f.inputs, candidate)
			return nil
		}
		f.inputMap = make(map[InputKey]int, len(f.inputs)+1)
		for index := range f.inputs {
			f.inputMap[f.inputs[index].Key] = index
		}
		f.inputMap[candidate.Key] = len(f.inputs)
		f.inputs = append(f.inputs, candidate)
		return nil
	}
	if f.inputCount < len(f.inputSmall) {
		f.inputSmall[f.inputCount] = candidate
		f.inputCount++
		return nil
	}
	f.inputs = make([]InputRevision, f.inputCount, dependencyFrameMapThreshold)
	copy(f.inputs, f.inputSmall[:f.inputCount])
	f.inputs = append(f.inputs, candidate)
	return nil
}

func (f *coldDependencyFrame) dependencyValues() []dependency {
	if f.dependencies != nil {
		return f.dependencies
	}
	return f.dependencySmall[:f.dependencyCount]
}

func (f *coldDependencyFrame) inputValues() []InputRevision {
	if f.inputs != nil {
		return f.inputs
	}
	return f.inputSmall[:f.inputCount]
}

func (s *coldExactBatchState) prepareEntry(index int, expected *ExactValueRoot) (nodeEntry, error) {
	key := s.keys[index]
	if err := s.queryError(index); err != nil {
		return nodeEntry{}, &queryError{key: key, err: err}
	}
	if s.completions[index].Load() != coldExactBatchValueReady {
		return nodeEntry{}, &queryError{
			key: key,
			err: fmt.Errorf("incremental cold batch query did not produce a value"),
		}
	}
	if index < 0 || index >= len(s.slots) {
		return nodeEntry{}, &queryError{key: key, err: fmt.Errorf("incremental cold batch query has no value storage")}
	}
	slot := &s.slots[index]
	root := ExactValueRoot{value: &slot.value}
	if err := root.validateOwned(s.session.graph.valueAuthority, key); err != nil {
		return nodeEntry{}, err
	}
	if err := root.validateExecution(&slot.execution); err != nil {
		return nodeEntry{}, err
	}
	if expected != nil {
		if err := expected.validateOwned(s.session.graph.valueAuthority, key); err != nil {
			return nodeEntry{}, err
		}
		if err := expected.validateExecution(&slot.execution); err != nil {
			return nodeEntry{}, err
		}
		if expected.value != root.value {
			return nodeEntry{}, fmt.Errorf("incremental cold batch wave has a substituted value root")
		}
	}
	frame := &s.frames[index].frame
	entry := nodeEntry{
		value:     root,
		deps:      frame.sortedDependencies(),
		inputs:    frame.sortedInputs(),
		changedAt: s.session.targetGeneration,
	}
	for _, dependency := range entry.deps {
		if dependency.key.kind != queryDependency {
			continue
		}
		dependencyIndex, member := s.index(dependency.key.query)
		if member && !s.sealed[dependencyIndex].Load() {
			return nodeEntry{}, fmt.Errorf("incremental cold batch wave depends on an unsealed member")
		}
	}
	return entry, nil
}

func (s *coldExactBatchState) installWave(indexes []int, entries []nodeEntry) error {
	for index := range entries {
		if err := s.validateWaveInputs(entries[index].inputs); err != nil {
			return err
		}
	}
	for _, index := range indexes {
		if _, exists := s.session.nodeChanges[s.keys[index]]; exists {
			return fmt.Errorf("incremental cold batch query already has staged state")
		}
	}
	for _, entry := range entries {
		for _, input := range entry.inputs {
			s.session.observations[input.Key] = input
		}
	}
	if s.session.nodeChanges == nil {
		s.session.nodeChanges = make(map[QueryKey]nodeEntry, len(s.keys))
	}
	for resultIndex, index := range indexes {
		entry := entries[resultIndex]
		s.entries[index] = entry
		s.session.nodeChanges[s.keys[index]] = entry
		s.sealed[index].Store(true)
		s.sealedCount++
	}
	s.session.invalidateReplacementReverse()
	return nil
}

func (s *coldExactBatchState) validateWaveInputs(inputs []InputRevision) error {
	for _, input := range inputs {
		if current, exists := s.session.observations[input.Key]; exists && current != input {
			return fmt.Errorf("incremental query observed inconsistent input revisions")
		}
		authoritative, exists := s.knownInputs[input.Key]
		if !exists {
			shard := &s.inputShards[coldExactBatchInputShardIndex(input.Key)]
			resolution, resolved := shard.inputs[input.Key]
			if !resolved || resolution == nil || resolution.err != nil {
				return fmt.Errorf("incremental cold batch input observation has no exact value")
			}
			authoritative = resolution.entry
		}
		if authoritative.revision != input.Revision || authoritative.found != input.Found {
			return fmt.Errorf("incremental query observed inconsistent input revisions")
		}
	}
	return nil
}

func (s *coldExactBatchState) failSeal(err error) error {
	if s.failure == nil {
		s.failure = err
	}
	return s.failure
}

func (s *coldExactBatchState) revoke() {
	s.lifetime.Lock()
	s.revoked = true
	s.lifetime.Unlock()
}

func (s *coldExactBatchState) release() {
	s.lifetime.Lock()
	s.session = nil
	s.ctx = nil
	s.keys = nil
	s.frames = nil
	s.slots = nil
	s.entries = nil
	s.completions = nil
	s.sealed = nil
	s.knownInputs = nil
	s.failure = nil
	s.observationFailure.Store(nil)
	s.queryToken = nil
	s.resolverToken = nil
	for index := range s.inputShards {
		s.inputShards[index].inputs = nil
	}
	s.lifetime.Unlock()
}

func (s *coldExactBatchState) exactInputOwned(index int, key InputKey) (Input, error) {
	entry, err := s.loadInput(index, key)
	if err != nil {
		return Input{}, err
	}
	detached := cloneInputEntry(entry)
	if err := s.recordInput(index, key, entry); err != nil {
		return Input{}, err
	}
	return Input{
		Key:      key,
		Revision: detached.revision,
		Found:    detached.found,
		Value:    detached.value,
	}, nil
}

func (s *coldExactBatchState) loadInput(index int, key InputKey) (inputEntry, error) {
	if err := s.queryError(index); err != nil {
		return inputEntry{}, err
	}
	entry, exists := s.knownInputs[key]
	if exists {
		return entry, nil
	}
	entry, err := s.resolveInput(s.ctx, key)
	if err != nil {
		return inputEntry{}, s.failQuery(index, err)
	}
	return entry, nil
}

func (s *coldExactBatchState) resolveInput(ctx context.Context, key InputKey) (inputEntry, error) {
	if err := ctx.Err(); err != nil {
		return inputEntry{}, err
	}
	if entry, exists := s.knownInputs[key]; exists {
		return entry, nil
	}
	shard := &s.inputShards[coldExactBatchInputShardIndex(key)]
	shard.gate.Lock()
	resolution, exists := shard.inputs[key]
	if exists {
		shard.gate.Unlock()
		select {
		case <-resolution.ready:
			return resolution.entry, resolution.err
		case <-ctx.Done():
			return inputEntry{}, ctx.Err()
		}
	}
	if shard.inputs == nil {
		shard.inputs = map[InputKey]*coldExactBatchInputResolution{}
	}
	resolution = &coldExactBatchInputResolution{ready: make(chan struct{})}
	shard.inputs[key] = resolution
	shard.gate.Unlock()

	resolution.entry, resolution.err = s.loadResolvedInput(ctx, key)
	close(resolution.ready)
	return resolution.entry, resolution.err
}

func (s *coldExactBatchState) loadResolvedInput(ctx context.Context, key InputKey) (inputEntry, error) {
	if err := ctx.Err(); err != nil {
		return inputEntry{}, err
	}
	if s.session.resolver == nil {
		return inputEntry{}, &missingInputError{key: key}
	}
	if !s.session.resolverConcurrent {
		select {
		case <-s.resolverToken:
			defer func() { s.resolverToken <- struct{}{} }()
		case <-ctx.Done():
			return inputEntry{}, ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			return inputEntry{}, err
		}
	}
	resolved, err := callInputResolver(ctx, s.session.resolver, key)
	if err != nil {
		return inputEntry{}, err
	}
	if resolved.Key != key {
		return inputEntry{}, fmt.Errorf("incremental input resolver returned a different key")
	}
	if err := validateInputBatch([]Input{resolved}); err != nil {
		return inputEntry{}, err
	}
	return inputEntry{
		revision:  resolved.Revision,
		found:     resolved.Found,
		value:     cloneBytes(resolved.Value),
		changedAt: s.session.targetGeneration,
	}, nil
}

func coldExactBatchInputShardIndex(key InputKey) uint8 {
	value := key.id.String()
	if value == "" {
		return 0
	}
	length := len(value)
	if length > math.MaxUint8 {
		length &= math.MaxUint8
	}
	return uint8(length) ^ value[0] ^ value[len(value)-1]
}

func (s *coldExactBatchState) recordInput(index int, key InputKey, entry inputEntry) error {
	frame := &s.frames[index]
	if err := frame.currentError(); err != nil {
		return err
	}
	frame.gate.Lock()
	defer frame.gate.Unlock()
	if err := frame.currentError(); err != nil {
		return err
	}
	if err := frame.frame.addInput(key, entry); err != nil {
		return frame.fail(err)
	}
	return nil
}

func (s *coldExactBatchState) recordQuery(index int, key QueryKey, entry *nodeEntry) error {
	frame := &s.frames[index]
	if err := frame.currentError(); err != nil {
		return err
	}
	frame.gate.Lock()
	defer frame.gate.Unlock()
	if err := frame.currentError(); err != nil {
		return err
	}
	if err := frame.frame.addQuery(key, entry); err != nil {
		return frame.fail(err)
	}
	return nil
}

func (s *coldExactBatchState) queryError(index int) error {
	return s.frames[index].currentError()
}

func (s *coldExactBatchState) failQuery(index int, err error) error {
	err = s.frames[index].fail(err)
	s.observationFailure.CompareAndSwap(nil, &coldExactBatchFailure{err: err})
	return err
}

func (f *coldExactBatchFrame) currentError() error {
	failure := f.failure.Load()
	if failure == nil {
		return nil
	}
	return failure.err
}

func (f *coldExactBatchFrame) fail(err error) error {
	if current := f.currentError(); current != nil {
		return current
	}
	failure := &coldExactBatchFailure{err: err}
	if f.failure.CompareAndSwap(nil, failure) {
		return err
	}
	return f.currentError()
}

// EvaluateAllColdExactBatch evaluates fresh queries in a cold-reset session.
func (s *Session) EvaluateAllColdExactBatch(
	ctx context.Context,
	batch ColdExactBatchFunc,
	keys ...QueryKey,
) ([]ExactResult, error) {
	if err := s.readyMutationContext(ctx); err != nil {
		return nil, err
	}
	if batch == nil {
		return nil, s.fail(fmt.Errorf("incremental cold exact query batch has no implementation"))
	}
	if !s.cold {
		return nil, s.fail(fmt.Errorf("incremental cold exact batch requires a cold-reset session"))
	}
	ordered, err := validateQueryBatch(keys)
	if err != nil {
		return nil, s.fail(err)
	}
	if len(ordered) == 0 {
		return []ExactResult{}, nil
	}
	if err := s.validateFreshColdBatch(ordered); err != nil {
		return nil, s.fail(err)
	}

	s.started = true
	s.queriedBatches = append(s.queriedBatches, ordered)
	state := newColdExactBatchState(s, ctx, ordered)
	defer state.release()
	s.activeColdExactBatch = state
	err = callColdExactBatch(ctx, batch, ColdExactBatch{state: state})
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = s.ready()
	}
	if err == nil {
		err = state.finish()
	} else {
		state.revoke()
	}
	s.activeColdExactBatch = nil
	if err != nil {
		return nil, s.fail(err)
	}
	if err := s.publishColdExactBatchInputs(state); err != nil {
		return nil, s.fail(err)
	}
	s.addBulkCounters(state.keys, NodeCounters{Executions: 1, Changes: 1})

	results := make([]ExactResult, len(ordered))
	for index, key := range ordered {
		results[index] = ExactResult{Key: key, Value: state.entries[index].value}
	}
	return results, nil
}

func (s *Session) validateFreshColdBatch(keys []QueryKey) error {
	if s.activeColdExactBatch != nil {
		return fmt.Errorf("incremental cold exact batch is already active")
	}
	for _, key := range keys {
		run, defined, err := s.graph.definition(key)
		if err != nil {
			return err
		}
		if !defined {
			return fmt.Errorf("incremental query is not defined")
		}
		if run == nil {
			return fmt.Errorf("incremental query has no implementation")
		}
		if s.wasQueried(key) {
			return fmt.Errorf("incremental cold batch query %s was already evaluated", key)
		}
		if _, exists := s.nodeChanges[key]; exists {
			return fmt.Errorf("incremental cold batch query %s already has staged state", key)
		}
	}
	return nil
}

func (s *Session) publishColdExactBatchInputs(state *coldExactBatchState) error {
	var firstKey InputKey
	var firstErr error
	for shardIndex := range state.inputShards {
		for key, resolution := range state.inputShards[shardIndex].inputs {
			err := resolution.err
			if err == nil {
				err = s.validateColdExactBatchInput(key, resolution.entry)
			}
			if err != nil && (firstErr == nil || key.id.String() < firstKey.id.String()) {
				firstKey = key
				firstErr = err
			}
		}
	}
	if firstErr != nil {
		return firstErr
	}
	for shardIndex := range state.inputShards {
		for key, resolution := range state.inputShards[shardIndex].inputs {
			entry := resolution.entry
			if _, exists := s.inputChanges[key]; exists {
				continue
			}
			s.inputChanges[key] = entry
			s.inputVersions[inputVersionKey{key: key, revision: entry.revision}] = entry
			s.observations[key] = InputRevision{Key: key, Revision: entry.revision, Found: entry.found}
		}
	}
	return nil
}

func (s *Session) validateColdExactBatchInput(key InputKey, entry inputEntry) error {
	if current, exists := s.inputChanges[key]; exists {
		if current.revision != entry.revision || current.found != entry.found ||
			!slices.Equal(current.value, entry.value) {
			return ErrRevisionConflict
		}
		return nil
	}
	versionKey := inputVersionKey{key: key, revision: entry.revision}
	if previous, exists := s.inputVersions[versionKey]; exists &&
		(previous.found != entry.found || !slices.Equal(previous.value, entry.value)) {
		return fmt.Errorf("incremental input reused an exact revision for different bytes")
	}
	observation := InputRevision{Key: key, Revision: entry.revision, Found: entry.found}
	if previous, exists := s.observations[key]; exists && previous != observation {
		return fmt.Errorf("incremental query observed inconsistent input revisions")
	}
	return nil
}

func callColdExactBatch(
	ctx context.Context,
	batch ColdExactBatchFunc,
	queries ColdExactBatch,
) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = &panicError{value: recovered}
		}
	}()
	return batch(ctx, queries)
}
