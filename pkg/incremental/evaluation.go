package incremental

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
)

// Evaluate returns a cloned value for one query.
func (s *Session) Evaluate(ctx context.Context, key QueryKey) ([]byte, error) {
	if err := s.readyMutationContext(ctx); err != nil {
		return nil, err
	}
	s.started = true
	entry, err := s.evaluateNode(ctx, key)
	if err != nil {
		return nil, s.fail(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, s.fail(err)
	}
	if err := s.observe(entry.inputs); err != nil {
		return nil, s.fail(err)
	}
	value, err := entry.value.Bytes()
	if err != nil {
		return nil, s.fail(err)
	}
	return value, nil
}

// EvaluateAll evaluates unique query keys in deterministic order.
func (s *Session) EvaluateAll(ctx context.Context, keys ...QueryKey) ([]Result, error) {
	if err := s.readyMutationContext(ctx); err != nil {
		return nil, err
	}
	ordered := append([]QueryKey(nil), keys...)
	sortQueryKeys(ordered)
	for index, key := range ordered {
		if !validQueryKey(key) {
			return nil, s.fail(fmt.Errorf("incremental query key is empty"))
		}
		if index > 0 && ordered[index-1] == key {
			return nil, s.fail(fmt.Errorf("incremental query batch contains a duplicate key"))
		}
	}

	s.started = true
	results := make([]Result, 0, len(ordered))
	for _, key := range ordered {
		entry, err := s.evaluateNode(ctx, key)
		if err != nil {
			return nil, s.fail(err)
		}
		if err := ctx.Err(); err != nil {
			return nil, s.fail(err)
		}
		if err := s.observe(entry.inputs); err != nil {
			return nil, s.fail(err)
		}
		value, err := entry.value.Bytes()
		if err != nil {
			return nil, s.fail(err)
		}
		results = append(results, Result{Key: key, Value: value})
	}
	return results, nil
}

// EvaluateAllBatch evaluates cache misses through one batch call while keeping
// one dependency frame, value, and cache node per query. A batched query may
// read inputs and already-evaluable queries, but it cannot depend on another
// member of the same batch.
func (s *Session) EvaluateAllBatch(
	ctx context.Context,
	batch BatchQueryFunc,
	keys ...QueryKey,
) ([]Result, error) {
	if err := s.readyMutationContext(ctx); err != nil {
		return nil, err
	}
	if batch == nil {
		return nil, s.fail(fmt.Errorf("incremental query batch has no implementation"))
	}
	exact, err := s.evaluateAllBatch(ctx, func(ctx context.Context, queries []BatchQuery) ([]exactBatchExecution, error) {
		values, err := callQueryBatch(ctx, batch, queries)
		if err != nil {
			return nil, err
		}
		result := make([]exactBatchExecution, len(values))
		for index := range values {
			result[index].err = values[index].Err
			if values[index].Err == nil && index < len(queries) {
				result[index].value, result[index].err = queries[index].NewExactValue(string(values[index].Value))
			}
		}
		return result, nil
	}, keys...)
	if err != nil {
		return nil, err
	}
	results := make([]Result, len(exact))
	for index := range exact {
		value, err := exact[index].Value.Bytes()
		if err != nil {
			return nil, s.fail(err)
		}
		results[index] = Result{Key: exact[index].Key, Value: value}
	}
	return results, nil
}

// EvaluateAllExactBatch evaluates cache misses and returns authenticated immutable roots.
func (s *Session) EvaluateAllExactBatch(
	ctx context.Context,
	batch ExactBatchQueryFunc,
	keys ...QueryKey,
) ([]ExactResult, error) {
	if err := s.readyMutationContext(ctx); err != nil {
		return nil, err
	}
	if batch == nil {
		return nil, s.fail(fmt.Errorf("incremental exact query batch has no implementation"))
	}
	return s.evaluateAllBatch(ctx, func(ctx context.Context, queries []BatchQuery) ([]exactBatchExecution, error) {
		values, err := callExactQueryBatch(ctx, batch, queries)
		if err != nil {
			return nil, err
		}
		result := make([]exactBatchExecution, len(values))
		for index := range values {
			result[index] = exactBatchExecution{value: values[index].Value, err: values[index].Err}
		}
		return result, nil
	}, keys...)
}

type exactBatchExecution struct {
	value ExactValueRoot
	err   error
}

type exactBatchExecutor func(context.Context, []BatchQuery) ([]exactBatchExecution, error)

type pendingBatchQuery struct {
	index    int
	key      QueryKey
	previous *nodeEntry
	frame    *dependencyFrame
	reader   *batchQueryReader
}

func (s *Session) evaluateAllBatch(
	ctx context.Context,
	batch exactBatchExecutor,
	keys ...QueryKey,
) ([]ExactResult, error) {
	if err := s.readyMutationContext(ctx); err != nil {
		return nil, err
	}
	ordered, err := validateQueryBatch(keys)
	if err != nil {
		return nil, s.fail(err)
	}

	s.started = true
	results, pending, err := s.prepareExactBatchQueries(ctx, ordered)
	if err != nil {
		return nil, s.fail(err)
	}
	if len(pending) == 0 {
		return results, nil
	}

	requests, deactivate := s.activateBatchQueries(ctx, pending)
	defer deactivate()
	values, err := batch(ctx, requests)
	revokeBatchQueries(pending)
	if err != nil {
		return nil, s.fail(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, s.fail(err)
	}
	if len(values) != len(pending) {
		return nil, s.fail(fmt.Errorf(
			"incremental query batch returned %d values for %d queries",
			len(values),
			len(pending),
		))
	}
	if err := s.stageExactBatchResults(results, pending, values); err != nil {
		return nil, s.fail(err)
	}
	return results, nil
}

func (s *Session) prepareExactBatchQueries(
	ctx context.Context,
	ordered []QueryKey,
) ([]ExactResult, []pendingBatchQuery, error) {
	results := make([]ExactResult, len(ordered))
	pending := make([]pendingBatchQuery, 0, len(ordered))
	for index, key := range ordered {
		entry, previous, execute, prepareErr := s.prepareBatchQuery(ctx, key)
		if prepareErr != nil {
			return nil, nil, prepareErr
		}
		if !execute {
			if err := s.observe(entry.inputs); err != nil {
				return nil, nil, err
			}
			results[index] = ExactResult{Key: key, Value: entry.value}
			continue
		}
		pending = append(pending, pendingBatchQuery{
			index:    index,
			key:      key,
			previous: previous,
			frame:    newDependencyFrame(),
		})
	}
	return results, pending, nil
}

func (s *Session) activateBatchQueries(
	ctx context.Context,
	pending []pendingBatchQuery,
) (requests []BatchQuery, deactivate func()) {
	batchKeys := make(map[QueryKey]struct{}, len(pending))
	baseStackLength := len(s.stack)
	for _, query := range pending {
		batchKeys[query.key] = struct{}{}
		s.active[query.key] = len(s.stack)
		s.stack = append(s.stack, query.key)
	}
	deactivate = func() {
		for _, query := range pending {
			delete(s.active, query.key)
		}
		s.stack = s.stack[:baseStackLength]
	}

	requests = make([]BatchQuery, len(pending))
	sessionGate := &sync.Mutex{}
	for index := range pending {
		execution := newExactValueExecution(s.graph.valueAuthority, pending[index].key)
		reader := &batchQueryReader{
			queryReader: &queryReader{session: s, frame: pending[index].frame, ctx: ctx},
			key:         pending[index].key,
			batchKeys:   batchKeys,
			sessionGate: sessionGate,
			execution:   execution,
		}
		pending[index].reader = reader
		requests[index] = BatchQuery{
			Key:    pending[index].key,
			Reader: reader,
			root: func(value string) (ExactValueRoot, error) {
				return reader.newExactValue(value)
			},
		}
	}
	return requests, deactivate
}

func revokeBatchQueries(pending []pendingBatchQuery) {
	for index := range pending {
		pending[index].reader.revoke()
	}
}

func (s *Session) stageExactBatchResults(
	results []ExactResult,
	pending []pendingBatchQuery,
	values []exactBatchExecution,
) error {
	for index := range pending {
		query := &pending[index]
		if readerErr := query.reader.currentError(); readerErr != nil {
			return readerErr
		}
		if values[index].err != nil {
			return &queryError{key: query.key, err: values[index].err}
		}
		entry, stageErr := s.stageExecutedExactQuery(
			query.key,
			query.frame,
			values[index].value,
			query.previous,
			query.reader.execution,
		)
		if stageErr != nil {
			return stageErr
		}
		if err := s.observe(entry.inputs); err != nil {
			return err
		}
		results[query.index] = ExactResult{Key: query.key, Value: entry.value}
	}
	return nil
}

func validateQueryBatch(keys []QueryKey) ([]QueryKey, error) {
	ordered := append([]QueryKey(nil), keys...)
	sortQueryKeys(ordered)
	for index, key := range ordered {
		if !validQueryKey(key) {
			return nil, fmt.Errorf("incremental query key is empty")
		}
		if index > 0 && ordered[index-1] == key {
			return nil, fmt.Errorf("incremental query batch contains a duplicate key")
		}
	}
	return ordered, nil
}

func (s *Session) prepareBatchQuery(
	ctx context.Context,
	key QueryKey,
) (entry nodeEntry, previous *nodeEntry, execute bool, err error) {
	if err := s.readyContext(ctx); err != nil {
		return nodeEntry{}, nil, false, err
	}
	run, defined, err := s.graph.definition(key)
	if err != nil {
		return nodeEntry{}, nil, false, err
	}
	if !defined {
		return nodeEntry{}, nil, false, fmt.Errorf("incremental query is not defined")
	}
	if run == nil {
		return nodeEntry{}, nil, false, fmt.Errorf("incremental query has no implementation")
	}
	s.queried[key] = struct{}{}
	if index, active := s.active[key]; active {
		path := append([]QueryKey(nil), s.stack[index:]...)
		path = append(path, key)
		return nodeEntry{}, nil, false, &CycleError{Path: path}
	}

	current, exists, err := s.currentNode(key)
	if err != nil {
		return nodeEntry{}, nil, false, err
	}
	if exists && !current.dirty {
		s.addCounters(key, NodeCounters{CacheHits: 1})
		return current, nil, false, nil
	}
	if exists {
		changed, observations, err := s.verifyDependencies(ctx, current.deps)
		if err != nil {
			return nodeEntry{}, nil, false, err
		}
		if !changed {
			current.dirty = false
			current.inputs = observations
			if err := s.stageNodeChange(key, &current); err != nil {
				return nodeEntry{}, nil, false, err
			}
			s.addCounters(key, NodeCounters{CacheHits: 1, Backdates: 1})
			return cloneNodeEntry(&current), nil, false, nil
		}
		previous = &current
	}
	return nodeEntry{}, previous, true, nil
}

type batchQueryReader struct {
	*queryReader
	key         QueryKey
	batchKeys   map[QueryKey]struct{}
	sessionGate *sync.Mutex
	execution   *exactValueExecution
	localGate   sync.Mutex
	invocations sync.RWMutex
	revoked     bool
}

func (r *batchQueryReader) Input(key InputKey) (value []byte, found bool, err error) {
	if err := r.begin(); err != nil {
		return nil, false, err
	}
	defer r.invocations.RUnlock()
	input, err := r.exactInputOwned(key)
	if err != nil {
		return nil, false, err
	}
	return cloneBytes(input.Value), input.Found, nil
}

func (r *batchQueryReader) ExactInput(key InputKey) (Input, error) {
	if err := r.begin(); err != nil {
		return Input{}, err
	}
	defer r.invocations.RUnlock()
	input, err := r.exactInputOwned(key)
	input.Value = cloneBytes(input.Value)
	return input, err
}

func (r *batchQueryReader) ExactInputOwned(key InputKey) (Input, error) {
	if err := r.begin(); err != nil {
		return Input{}, err
	}
	defer r.invocations.RUnlock()
	return r.exactInputOwned(key)
}

func (r *batchQueryReader) ObserveExactInput(expected InputRevision) error {
	if err := r.begin(); err != nil {
		return err
	}
	defer r.invocations.RUnlock()
	if !validInputKey(expected.Key) || !validRevision(expected.Revision) {
		return r.fail(fmt.Errorf("incremental input observation has an invalid identity"))
	}
	entry, err := r.loadInput(expected.Key)
	if err != nil {
		return err
	}
	if entry.revision != expected.Revision || entry.found != expected.Found {
		return r.fail(ErrRevisionConflict)
	}
	return r.recordInput(expected.Key, entry)
}

func (r *batchQueryReader) ObserveExactInputValue(expected Input) error {
	if err := r.begin(); err != nil {
		return err
	}
	defer r.invocations.RUnlock()
	if err := validateInputBatch([]Input{expected}); err != nil {
		return r.fail(err)
	}
	entry, err := r.loadInput(expected.Key)
	if err != nil {
		return err
	}
	if entry.revision != expected.Revision || !sameInputValue(entry, expected) {
		return r.fail(ErrRevisionConflict)
	}
	return r.recordInput(expected.Key, entry)
}

func (*batchQueryReader) exactInputValueObserver() {}

func (r *batchQueryReader) ObserveExactImmutableInput(expected ImmutableInput) error {
	if err := r.begin(); err != nil {
		return err
	}
	defer r.invocations.RUnlock()
	if err := validateImmutableInput(expected); err != nil {
		return r.fail(err)
	}
	entry, err := r.loadInput(expected.Key)
	if err != nil {
		return err
	}
	if entry.revision != expected.Revision || !sameImmutableInputValue(entry, expected) {
		return r.fail(ErrRevisionConflict)
	}
	return r.recordInput(expected.Key, entry)
}

func (*batchQueryReader) exactImmutableInputObserver() {}

func (r *batchQueryReader) Query(ctx context.Context, key QueryKey) ([]byte, error) {
	if err := r.begin(); err != nil {
		return nil, err
	}
	defer r.invocations.RUnlock()
	if _, batched := r.batchKeys[key]; batched {
		return nil, r.fail(fmt.Errorf("incremental query batch member cannot depend on another batch member"))
	}
	r.sessionGate.Lock()
	if err := r.currentError(); err != nil {
		r.sessionGate.Unlock()
		return nil, err
	}
	entry, err := r.session.evaluateNode(ctx, key)
	r.sessionGate.Unlock()
	if err != nil {
		return nil, r.fail(err)
	}
	if err := r.recordQuery(key, &entry); err != nil {
		return nil, err
	}
	value, err := entry.value.Bytes()
	if err != nil {
		return nil, r.fail(err)
	}
	return value, nil
}

func (r *batchQueryReader) newExactValue(value string) (ExactValueRoot, error) {
	if err := r.begin(); err != nil {
		return ExactValueRoot{}, err
	}
	defer r.invocations.RUnlock()
	if err := r.currentError(); err != nil {
		return ExactValueRoot{}, err
	}
	if !r.execution.valid(r.session.graph.valueAuthority, r.key) {
		return ExactValueRoot{}, errors.New("incremental batch query has invalid exact-value authority")
	}
	return newExactValueRootForExecution(
		r.session.graph.valueAuthority,
		r.key,
		value,
		r.execution,
	), nil
}

func (r *batchQueryReader) exactInputOwned(key InputKey) (Input, error) {
	entry, err := r.loadInput(key)
	if err != nil {
		return Input{}, err
	}
	detached := cloneInputEntry(entry)
	if err := r.recordInput(key, entry); err != nil {
		return Input{}, err
	}
	return Input{
		Key:      key,
		Revision: detached.revision,
		Found:    detached.found,
		Value:    detached.value,
	}, nil
}

func (r *batchQueryReader) loadInput(key InputKey) (inputEntry, error) {
	r.sessionGate.Lock()
	defer r.sessionGate.Unlock()
	if err := r.currentError(); err != nil {
		return inputEntry{}, err
	}
	entry, exists, err := r.session.borrowInput(key)
	if err == nil && !exists {
		entry, err = r.session.resolveInputBorrowed(r.ctx, key)
	}
	if err != nil {
		return inputEntry{}, r.fail(err)
	}
	return entry, nil
}

func (r *batchQueryReader) recordInput(key InputKey, entry inputEntry) error {
	r.localGate.Lock()
	defer r.localGate.Unlock()
	if r.err != nil {
		return r.err
	}
	if err := r.frame.addInput(key, entry); err != nil {
		r.err = err
	}
	return r.err
}

func (r *batchQueryReader) recordQuery(key QueryKey, entry *nodeEntry) error {
	r.localGate.Lock()
	defer r.localGate.Unlock()
	if r.err != nil {
		return r.err
	}
	if err := r.frame.addQuery(key, entry); err != nil {
		r.err = err
	}
	return r.err
}

func (r *batchQueryReader) currentError() error {
	r.localGate.Lock()
	defer r.localGate.Unlock()
	return r.err
}

func (r *batchQueryReader) fail(err error) error {
	r.localGate.Lock()
	defer r.localGate.Unlock()
	if r.err == nil {
		r.err = err
	}
	return r.err
}

func (r *batchQueryReader) begin() error {
	r.invocations.RLock()
	if r.revoked {
		r.invocations.RUnlock()
		return fmt.Errorf("incremental batch query reader is no longer active")
	}
	return nil
}

func (r *batchQueryReader) revoke() {
	r.invocations.Lock()
	r.revoked = true
	r.invocations.Unlock()
}

func callQueryBatch(
	ctx context.Context,
	batch BatchQueryFunc,
	requests []BatchQuery,
) (values []BatchValue, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = &panicError{value: recovered}
			values = nil
		}
	}()
	return batch(ctx, requests)
}

func callExactQueryBatch(
	ctx context.Context,
	batch ExactBatchQueryFunc,
	requests []BatchQuery,
) (values []ExactBatchValue, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = &panicError{value: recovered}
			values = nil
		}
	}()
	return batch(ctx, requests)
}

func (s *Session) evaluateNode(ctx context.Context, key QueryKey) (nodeEntry, error) {
	if err := s.readyContext(ctx); err != nil {
		return nodeEntry{}, err
	}
	if batch := s.activeColdExactBatch; batch != nil {
		if entry, member, err := s.evaluateBatchMember(batch, key); member || err != nil {
			return entry, err
		}
	}
	run, defined, err := s.graph.definition(key)
	if err != nil {
		return nodeEntry{}, err
	}
	if !validQueryKey(key) || !defined {
		return nodeEntry{}, fmt.Errorf("incremental query is not defined")
	}
	if run == nil {
		return nodeEntry{}, fmt.Errorf("incremental query has no implementation")
	}
	s.queried[key] = struct{}{}
	if index, active := s.active[key]; active {
		path := append([]QueryKey(nil), s.stack[index:]...)
		path = append(path, key)
		return nodeEntry{}, &CycleError{Path: path}
	}

	s.active[key] = len(s.stack)
	s.stack = append(s.stack, key)
	defer func() {
		delete(s.active, key)
		s.stack = s.stack[:len(s.stack)-1]
	}()

	previous, exists, err := s.currentNode(key)
	if err != nil {
		return nodeEntry{}, err
	}
	if exists && !previous.dirty {
		s.addCounters(key, NodeCounters{CacheHits: 1})
		return previous, nil
	}
	if exists {
		entry, backdated, err := s.backdateCleanNode(ctx, key, &previous)
		if err != nil {
			return nodeEntry{}, err
		}
		if backdated {
			return entry, nil
		}
	}

	if !exists {
		return s.executeQuery(ctx, key, run, nil)
	}
	return s.executeQuery(ctx, key, run, &previous)
}

func (s *Session) evaluateBatchMember(
	batch *coldExactBatchState,
	key QueryKey,
) (nodeEntry, bool, error) {
	index, member := batch.index(key)
	if !member {
		return nodeEntry{}, false, nil
	}
	if !batch.sealed[index].Load() {
		return nodeEntry{}, true, fmt.Errorf("incremental query cannot depend on another batch member before it is sealed")
	}
	entry := &batch.entries[index]
	if err := entry.value.validateOwned(s.graph.valueAuthority, key); err != nil {
		return nodeEntry{}, true, err
	}
	return cloneNodeEntry(entry), true, nil
}

func (s *Session) backdateCleanNode(
	ctx context.Context,
	key QueryKey,
	previous *nodeEntry,
) (nodeEntry, bool, error) {
	changed, observations, err := s.verifyDependencies(ctx, previous.deps)
	if err != nil {
		return nodeEntry{}, false, err
	}
	if changed {
		return nodeEntry{}, false, nil
	}
	previous.dirty = false
	previous.inputs = observations
	if err := s.stageNodeChange(key, previous); err != nil {
		return nodeEntry{}, false, err
	}
	s.addCounters(key, NodeCounters{CacheHits: 1, Backdates: 1})
	return cloneNodeEntry(previous), true, nil
}

func (s *Session) verifyDependencies(
	ctx context.Context,
	dependencies []dependency,
) (bool, []InputRevision, error) {
	observations := map[InputKey]InputRevision{}
	for _, dep := range dependencies {
		if err := ctx.Err(); err != nil {
			return false, nil, err
		}
		changed, err := s.verifyDependency(ctx, dep, observations)
		if err != nil {
			return false, nil, err
		}
		if changed {
			return true, nil, nil
		}
	}
	return false, sortedObservations(observations), nil
}

func (s *Session) verifyDependency(
	ctx context.Context,
	dep dependency,
	observations map[InputKey]InputRevision,
) (bool, error) {
	if dep.key.kind == inputDependency {
		entry, exists, err := s.currentInput(dep.key.input)
		if err != nil {
			return false, err
		}
		if !exists {
			return false, &missingInputError{key: dep.key.input}
		}
		observations[dep.key.input] = InputRevision{
			Key: dep.key.input, Revision: entry.revision, Found: entry.found,
		}
		return entry.changedAt != dep.changedAt || entry.found != dep.found, nil
	}
	if dep.key.kind != queryDependency {
		return false, fmt.Errorf("incremental query has an invalid dependency")
	}
	entry, err := s.evaluateNode(ctx, dep.key.query)
	if err != nil {
		return false, err
	}
	if err := mergeObservations(observations, entry.inputs); err != nil {
		return false, err
	}
	return entry.changedAt != dep.changedAt, nil
}

func (s *Session) executeQuery(
	ctx context.Context,
	key QueryKey,
	run QueryFunc,
	previous *nodeEntry,
) (nodeEntry, error) {
	frame := newDependencyFrame()
	reader := &queryReader{session: s, frame: frame, ctx: ctx}
	value, err := callQuery(ctx, run, reader)
	if reader.err != nil {
		return nodeEntry{}, reader.err
	}
	if err != nil {
		return nodeEntry{}, err
	}
	if err := ctx.Err(); err != nil {
		return nodeEntry{}, err
	}

	entry, err := s.stageExecutedQuery(key, frame, value, previous)
	if err != nil {
		return nodeEntry{}, err
	}
	return cloneNodeEntry(&entry), nil
}

func (s *Session) stageExecutedQuery(
	key QueryKey,
	frame *dependencyFrame,
	value []byte,
	previous *nodeEntry,
) (nodeEntry, error) {
	return s.stageValidatedQuery(
		key,
		frame,
		newExactValueRoot(s.graph.valueAuthority, key, string(value)),
		previous,
	)
}

func (s *Session) stageExecutedExactQuery(
	key QueryKey,
	frame *dependencyFrame,
	value ExactValueRoot,
	previous *nodeEntry,
	execution *exactValueExecution,
) (nodeEntry, error) {
	if err := value.validateOwned(s.graph.valueAuthority, key); err != nil {
		return nodeEntry{}, err
	}
	if err := value.validateExecution(execution); err != nil {
		return nodeEntry{}, err
	}
	return s.stageValidatedQuery(key, frame, value, previous)
}

func (s *Session) stageValidatedQuery(
	key QueryKey,
	frame *dependencyFrame,
	value ExactValueRoot,
	previous *nodeEntry,
) (nodeEntry, error) {
	entry := nodeEntry{
		value:     value,
		deps:      frame.sortedDependencies(),
		inputs:    frame.sortedInputs(),
		changedAt: s.targetGeneration,
	}
	detached := []nodeEntry{entry}
	if err := detachNodeDependencyStorage(detached); err != nil {
		return nodeEntry{}, err
	}
	entry = detached[0]
	delta := NodeCounters{Executions: 1, Changes: 1}
	if previous != nil {
		if sameExactValue(previous.value, entry.value) {
			entry.value = previous.value
			entry.changedAt = previous.changedAt
			delta.Changes = 0
			delta.Backdates = 1
		}
	}
	if err := s.stageNodeChange(key, &entry); err != nil {
		return nodeEntry{}, err
	}
	s.addCounters(key, delta)
	return entry, nil
}

func callQuery(ctx context.Context, run QueryFunc, reader Reader) (value []byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = &panicError{value: recovered}
			value = nil
		}
	}()
	return run(ctx, reader)
}

type queryReader struct {
	session *Session
	frame   *dependencyFrame
	ctx     context.Context
	err     error
}

func (r *queryReader) Input(key InputKey) (value []byte, found bool, err error) {
	input, err := r.ExactInput(key)
	if err != nil {
		return nil, false, err
	}
	return input.Value, input.Found, nil
}

func (r *queryReader) ExactInput(key InputKey) (input Input, err error) {
	input, err = r.ExactInputOwned(key)
	input.Value = cloneBytes(input.Value)
	return input, err
}

func (r *queryReader) ExactInputOwned(key InputKey) (input Input, err error) {
	if r.err != nil {
		return Input{}, r.err
	}
	entry, exists, err := r.session.currentInput(key)
	if err != nil {
		r.err = err
		return Input{}, err
	}
	if !exists {
		entry, err = r.session.resolveInput(r.ctx, key)
		if err != nil {
			r.err = err
			return Input{}, err
		}
	}
	if err := r.frame.addInput(key, entry); err != nil {
		r.err = err
		return Input{}, err
	}
	return Input{
		Key:      key,
		Revision: entry.revision,
		Found:    entry.found,
		Value:    entry.value,
	}, nil
}

func (r *queryReader) ObserveExactInput(expected InputRevision) error {
	if r.err != nil {
		return r.err
	}
	if !validInputKey(expected.Key) || !validRevision(expected.Revision) {
		r.err = fmt.Errorf("incremental input observation has an invalid identity")
		return r.err
	}
	entry, exists, err := r.session.borrowInput(expected.Key)
	if err != nil {
		r.err = err
		return err
	}
	if !exists {
		entry, err = r.session.resolveInput(r.ctx, expected.Key)
		if err != nil {
			r.err = err
			return err
		}
	}
	if entry.revision != expected.Revision || entry.found != expected.Found {
		r.err = ErrRevisionConflict
		return r.err
	}
	if err := r.frame.addInput(expected.Key, entry); err != nil {
		r.err = err
		return err
	}
	return nil
}

func (*queryReader) exactInputObserver() {}

func (r *queryReader) ObserveExactInputValue(expected Input) error {
	if r.err != nil {
		return r.err
	}
	if err := validateInputBatch([]Input{expected}); err != nil {
		r.err = err
		return err
	}
	entry, exists, err := r.session.borrowInput(expected.Key)
	if err != nil {
		r.err = err
		return err
	}
	if !exists {
		entry, err = r.session.resolveInput(r.ctx, expected.Key)
		if err != nil {
			r.err = err
			return err
		}
	}
	if entry.revision != expected.Revision || !sameInputValue(entry, expected) {
		r.err = ErrRevisionConflict
		return r.err
	}
	if err := r.frame.addInput(expected.Key, entry); err != nil {
		r.err = err
		return err
	}
	return nil
}

func (*queryReader) exactInputValueObserver() {}

func (r *queryReader) ObserveExactImmutableInput(expected ImmutableInput) error {
	if r.err != nil {
		return r.err
	}
	if err := validateImmutableInput(expected); err != nil {
		r.err = err
		return err
	}
	entry, exists, err := r.session.borrowInput(expected.Key)
	if err != nil {
		r.err = err
		return err
	}
	if !exists {
		entry, err = r.session.resolveInput(r.ctx, expected.Key)
		if err != nil {
			r.err = err
			return err
		}
	}
	if entry.revision != expected.Revision || !sameImmutableInputValue(entry, expected) {
		r.err = ErrRevisionConflict
		return r.err
	}
	if err := r.frame.addInput(expected.Key, entry); err != nil {
		r.err = err
		return err
	}
	return nil
}

func (*queryReader) exactImmutableInputObserver() {}

func (s *Session) resolveInput(ctx context.Context, key InputKey) (inputEntry, error) {
	entry, err := s.resolveInputBorrowed(ctx, key)
	return cloneInputEntry(entry), err
}

func (s *Session) resolveInputBorrowed(ctx context.Context, key InputKey) (inputEntry, error) {
	if s.activeColdExactBatch != nil {
		return s.activeColdExactBatch.resolveInput(ctx, key)
	}
	if s.resolver == nil {
		return inputEntry{}, &missingInputError{key: key}
	}
	resolved, err := callInputResolver(ctx, s.resolver, key)
	if err != nil {
		return inputEntry{}, err
	}
	if resolved.Key != key {
		return inputEntry{}, fmt.Errorf("incremental input resolver returned a different key")
	}
	if err := validateInputBatch([]Input{resolved}); err != nil {
		return inputEntry{}, err
	}
	entry := inputEntry{
		revision:  resolved.Revision,
		found:     resolved.Found,
		value:     cloneBytes(resolved.Value),
		changedAt: s.targetGeneration,
	}
	s.inputChanges[key] = entry
	s.observations[key] = revisionOf(resolved)
	return entry, nil
}

func callInputResolver(
	ctx context.Context,
	resolver InputResolver,
	key InputKey,
) (input Input, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = &panicError{value: recovered}
			input = Input{}
		}
	}()
	return resolver(ctx, key)
}

func (r *queryReader) Query(ctx context.Context, key QueryKey) ([]byte, error) {
	if r.err != nil {
		return nil, r.err
	}
	entry, err := r.session.evaluateNode(ctx, key)
	if err != nil {
		r.err = err
		return nil, err
	}
	if err := r.frame.addQuery(key, &entry); err != nil {
		r.err = err
		return nil, err
	}
	value, err := entry.value.Bytes()
	if err != nil {
		r.err = err
		return nil, err
	}
	return value, nil
}

type dependencyFrame struct {
	dependencySmall [dependencyFrameMapThreshold]dependency
	inputSmall      [dependencyFrameMapThreshold]InputRevision
	dependencyCount int
	inputCount      int
	dependencies    []dependency
	inputs          []InputRevision
	dependencyMap   map[dependencyKey]int
	inputMap        map[InputKey]int
}

func newDependencyFrame() *dependencyFrame {
	return &dependencyFrame{}
}

func (f *dependencyFrame) addInput(key InputKey, entry inputEntry) error {
	depKey := inputDep(key)
	dep := dependency{
		key:       depKey,
		changedAt: entry.changedAt,
		revision:  entry.revision,
		found:     entry.found,
	}
	if err := f.addDependency(dep, "incremental query observed one input at multiple revisions"); err != nil {
		return err
	}
	return f.addInputRevision(InputRevision{Key: key, Revision: entry.revision, Found: entry.found})
}

func (f *dependencyFrame) addQuery(key QueryKey, entry *nodeEntry) error {
	depKey := queryDep(key)
	dep := dependency{key: depKey, changedAt: entry.changedAt}
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

func (f *dependencyFrame) sortedDependencies() []dependency {
	dependencies := f.dependencyValues()
	sortDependencies(dependencies)
	return dependencies
}

func (f *dependencyFrame) sortedInputs() []InputRevision {
	inputs := f.inputValues()
	slices.SortFunc(inputs, func(left, right InputRevision) int {
		return cmp.Compare(left.Key.id.String(), right.Key.id.String())
	})
	return inputs
}

const dependencyFrameMapThreshold = 8

func (f *dependencyFrame) addDependency(candidate dependency, conflict string) error {
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
	if f.dependencyCount < len(f.dependencySmall) {
		f.dependencySmall[f.dependencyCount] = candidate
		f.dependencyCount++
		return nil
	}
	f.dependencies = make([]dependency, f.dependencyCount, f.dependencyCount*2)
	copy(f.dependencies, f.dependencySmall[:f.dependencyCount])
	f.dependencies = append(f.dependencies, candidate)
	f.dependencyMap = make(map[dependencyKey]int, len(f.dependencies))
	for index := range f.dependencies {
		f.dependencyMap[f.dependencies[index].key] = index
	}
	return nil
}

func (f *dependencyFrame) addInputRevision(candidate InputRevision) error {
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
	if f.inputCount < len(f.inputSmall) {
		f.inputSmall[f.inputCount] = candidate
		f.inputCount++
		return nil
	}
	f.inputs = make([]InputRevision, f.inputCount, f.inputCount*2)
	copy(f.inputs, f.inputSmall[:f.inputCount])
	f.inputs = append(f.inputs, candidate)
	f.inputMap = make(map[InputKey]int, len(f.inputs))
	for index := range f.inputs {
		f.inputMap[f.inputs[index].Key] = index
	}
	return nil
}

func (f *dependencyFrame) dependencyValues() []dependency {
	if f.dependencies != nil {
		return f.dependencies
	}
	return f.dependencySmall[:f.dependencyCount]
}

func (f *dependencyFrame) inputValues() []InputRevision {
	if f.inputs != nil {
		return f.inputs
	}
	return f.inputSmall[:f.inputCount]
}

func detachNodeDependencyStorage(entries []nodeEntry) error {
	maxInt := int(^uint(0) >> 1)
	dependencyCount := 0
	inputCount := 0
	for index := range entries {
		if len(entries[index].deps) > maxInt-dependencyCount ||
			len(entries[index].inputs) > maxInt-inputCount {
			return errors.New("incremental dependency storage exceeds addressable memory")
		}
		dependencyCount += len(entries[index].deps)
		inputCount += len(entries[index].inputs)
	}
	var dependencies []dependency
	if dependencyCount > 0 {
		dependencies = make([]dependency, dependencyCount)
	}
	var inputs []InputRevision
	if inputCount > 0 {
		inputs = make([]InputRevision, inputCount)
	}
	dependencyOffset := 0
	inputOffset := 0
	for index := range entries {
		entry := &entries[index]
		dependencyEnd := dependencyOffset + len(entry.deps)
		if dependencyEnd > dependencyOffset {
			if dependencyEnd > len(dependencies) {
				return errors.New("incremental dependency storage capacity was miscomputed")
			}
			copy(dependencies[dependencyOffset:dependencyEnd], entry.deps)
			entry.deps = dependencies[dependencyOffset:dependencyEnd:dependencyEnd]
		} else {
			entry.deps = nil
		}
		dependencyOffset = dependencyEnd

		inputEnd := inputOffset + len(entry.inputs)
		if inputEnd > inputOffset {
			if inputEnd > len(inputs) {
				return errors.New("incremental input storage capacity was miscomputed")
			}
			copy(inputs[inputOffset:inputEnd], entry.inputs)
			entry.inputs = inputs[inputOffset:inputEnd:inputEnd]
		} else {
			entry.inputs = nil
		}
		inputOffset = inputEnd
	}
	return nil
}

func mergeObservations(target map[InputKey]InputRevision, inputs []InputRevision) error {
	for _, input := range inputs {
		if current, exists := target[input.Key]; exists && current != input {
			return fmt.Errorf("incremental query observed inconsistent input revisions")
		}
		target[input.Key] = input
	}
	return nil
}

func sortedObservations(inputs map[InputKey]InputRevision) []InputRevision {
	keys := make([]InputKey, 0, len(inputs))
	for key := range inputs {
		keys = append(keys, key)
	}
	sortInputKeys(keys)
	result := make([]InputRevision, 0, len(keys))
	for _, key := range keys {
		result = append(result, inputs[key])
	}
	return result
}
