package incremental

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sync"
)

const coldExactBatchInputShardCount = 256

type coldExactBatchInputShard struct {
	gate   sync.Mutex
	inputs map[InputKey]*coldExactBatchInputResolution
}

type coldExactBatchInputResolution struct {
	ready chan struct{}
	entry inputEntry
	err   error
}

func (q ColdExactBatchQuery) Input(key InputKey) (value []byte, found bool, err error) {
	input, err := q.ExactInputOwned(key)
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
	return s.session.resolveInputEntry(ctx, key)
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
