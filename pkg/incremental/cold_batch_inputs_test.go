package incremental

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func startColdBatchInputReads(batch ColdExactBatch, keys []InputKey) func() error {
	var readers sync.WaitGroup
	errs := make([]error, len(keys))
	for index, key := range keys {
		readers.Go(func() {
			query := batch.Query(index)
			input, err := query.ExactInputOwned(key)
			if err == nil {
				_, err = query.Complete(string(input.Value))
			}
			errs[index] = err
		})
	}

	return func() error {
		readers.Wait()
		for _, err := range errs {
			if err != nil {
				return err
			}
		}
		return nil
	}
}

func TestColdExactBatchResolvesSharedInputOnceAndOwnsIt(t *testing.T) {
	inputKey := NewInputKey("shared")
	const queryCount = 128
	queries := make([]QueryKey, queryCount)
	for index := range queries {
		queries[index] = NewQueryKey(fmt.Sprintf("query-%03d", index))
	}
	graph := exactValueTestGraph(t, queries...)
	source := []byte("immutable")
	var resolverCalls atomic.Int64
	session, err := graph.BeginColdResetWithConcurrentResolver(func(_ context.Context, key InputKey) (Input, error) {
		resolverCalls.Add(1)
		return Input{Key: key, Revision: NewRevision("revision"), Found: true, Value: source}, nil
	})
	if err != nil {
		t.Fatalf("BeginColdResetWithResolver() error = %v", err)
	}
	results, err := session.EvaluateAllColdExactBatch(t.Context(), func(
		_ context.Context,
		batch ColdExactBatch,
	) error {
		wait := startColdBatchInputReads(batch, slices.Repeat([]InputKey{inputKey}, queryCount))
		return wait()
	}, queries...)
	if err != nil {
		t.Fatalf("EvaluateAllColdExactBatch() error = %v", err)
	}
	if calls := resolverCalls.Load(); calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", calls)
	}
	source[0] = 'X'
	for index, result := range results {
		value, valueErr := result.Value.Bytes()
		if valueErr != nil || string(value) != "immutable" {
			t.Fatalf("result %d = %q, %v; want immutable", index, value, valueErr)
		}
	}
	var verified []InputRevision
	if err := session.Commit(t.Context(), func(_ context.Context, inputs []InputRevision) (bool, error) {
		verified = append([]InputRevision(nil), inputs...)
		return true, nil
	}); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	want := InputRevision{Key: inputKey, Revision: NewRevision("revision"), Found: true}
	if len(verified) != 1 || verified[0] != want {
		t.Fatalf("verified inputs = %#v, want %#v", verified, want)
	}
}

func completeColdExactBatchFromDistinctInputs(
	batch ColdExactBatch,
	inputs []InputKey,
	allResolversEntered <-chan struct{},
	releaseResolvers chan struct{},
	entered *atomic.Int64,
) error {
	wait := startColdBatchInputReads(batch, inputs)
	select {
	case <-allResolversEntered:
	case <-time.After(5 * time.Second):
		close(releaseResolvers)
		_ = wait()
		return fmt.Errorf("only %d of %d distinct resolvers ran concurrently", entered.Load(), len(inputs))
	}
	close(releaseResolvers)
	return wait()
}

func TestColdExactBatchResolvesDistinctInputsConcurrently(t *testing.T) {
	const queryCount = 32
	queries := make([]QueryKey, queryCount)
	inputs := make([]InputKey, queryCount)
	for index := range queries {
		queries[index] = NewQueryKey(fmt.Sprintf("query-%03d", index))
		inputs[index] = NewInputKey(fmt.Sprintf("input-%03d", index))
	}
	graph := exactValueTestGraph(t, queries...)
	allResolversEntered := make(chan struct{})
	releaseResolvers := make(chan struct{})
	var entered atomic.Int64
	session, err := graph.BeginColdResetWithConcurrentResolver(func(_ context.Context, key InputKey) (Input, error) {
		if entered.Add(1) == queryCount {
			close(allResolversEntered)
		}
		<-releaseResolvers
		return exactInput(key, "revision", key.Opaque()), nil
	})
	if err != nil {
		t.Fatalf("BeginColdResetWithResolver() error = %v", err)
	}
	results, err := session.EvaluateAllColdExactBatch(t.Context(), func(
		_ context.Context,
		batch ColdExactBatch,
	) error {
		return completeColdExactBatchFromDistinctInputs(
			batch, inputs, allResolversEntered, releaseResolvers, &entered,
		)
	}, queries...)
	if err != nil {
		t.Fatalf("EvaluateAllColdExactBatch() error = %v", err)
	}
	for index, result := range results {
		value, valueErr := result.Value.Bytes()
		if valueErr != nil || string(value) != inputs[index].Opaque() {
			t.Fatalf("result %d = %q, %v; want %q", index, value, valueErr, inputs[index].Opaque())
		}
	}
	mustCommit(t, session)
}

func TestColdExactBatchObservesImmutableInputsConcurrently(t *testing.T) {
	const queryCount = 64
	queries := make([]QueryKey, queryCount)
	inputs := make([]Input, queryCount)
	for index := range queries {
		queries[index] = NewQueryKey(fmt.Sprintf("query-%03d", index))
		inputs[index] = exactInput(
			NewInputKey(fmt.Sprintf("input-%03d", index)),
			"revision",
			fmt.Sprintf("value-%03d", index),
		)
	}
	session := mustColdSession(t, exactValueTestGraph(t, queries...), inputs...)
	results, err := session.EvaluateAllColdExactBatch(t.Context(), func(
		_ context.Context,
		batch ColdExactBatch,
	) error {
		errs := make([]error, batch.Len())
		var workers sync.WaitGroup
		workers.Add(batch.Len())
		for index := range batch.Len() {
			query := batch.Query(index)
			expected := inputs[index]
			go func() {
				defer workers.Done()
				err := query.ObserveExactImmutableInput(ImmutableInput{
					Key: expected.Key, Revision: expected.Revision, Found: expected.Found,
					Value: string(expected.Value),
				})
				if err != nil {
					errs[index] = err
					return
				}
				_, errs[index] = query.Complete(string(expected.Value))
			}()
		}
		workers.Wait()
		return errors.Join(errs...)
	}, queries...)
	if err != nil {
		t.Fatalf("EvaluateAllColdExactBatch() error = %v", err)
	}
	for index, result := range results {
		value, valueErr := result.Value.String()
		if valueErr != nil || value != string(inputs[index].Value) {
			t.Fatalf("result %d = %q, %v; want %q", index, value, valueErr, inputs[index].Value)
		}
	}
	mustCommit(t, session)
}

func TestColdExactBatchImmutableObservationRejectsSameRevisionPoison(t *testing.T) {
	inputKey := NewInputKey("input")
	queryKey := NewQueryKey("query")
	session := mustColdSession(
		t,
		exactValueTestGraph(t, queryKey),
		exactInput(inputKey, "revision", "value"),
	)
	_, err := session.EvaluateAllColdExactBatch(t.Context(), func(
		_ context.Context,
		batch ColdExactBatch,
	) error {
		return batch.Query(0).ObserveExactImmutableInput(ImmutableInput{
			Key: inputKey, Revision: NewRevision("revision"), Found: true, Value: "poison",
		})
	}, queryKey)
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("EvaluateAllColdExactBatch() error = %v, want %v", err, ErrRevisionConflict)
	}
	if session.graph.Generation() != 0 {
		t.Fatal("failed immutable input observation changed the graph")
	}
}

func completeColdExactBatchWithSerializedResolver(
	batch ColdExactBatch,
	inputs []InputKey,
	entered <-chan struct{},
	release chan struct{},
) error {
	wait := startColdBatchInputReads(batch, inputs)
	<-entered
	select {
	case <-entered:
		close(release)
		_ = wait()
		return errors.New("legacy input resolver ran concurrently")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	return wait()
}

func TestColdExactBatchLegacyResolverRemainsSerialized(t *testing.T) {
	const queryCount = 32
	queries := make([]QueryKey, queryCount)
	inputs := make([]InputKey, queryCount)
	for index := range queries {
		queries[index] = NewQueryKey(fmt.Sprintf("query-%03d", index))
		inputs[index] = NewInputKey(fmt.Sprintf("input-%03d", index))
	}
	graph := exactValueTestGraph(t, queries...)
	entered := make(chan struct{}, queryCount)
	release := make(chan struct{})
	var active atomic.Int64
	var maximum atomic.Int64
	session, err := graph.BeginColdResetWithResolver(func(_ context.Context, key InputKey) (Input, error) {
		current := active.Add(1)
		for {
			observed := maximum.Load()
			if current <= observed || maximum.CompareAndSwap(observed, current) {
				break
			}
		}
		entered <- struct{}{}
		<-release
		active.Add(-1)
		return exactInput(key, "revision", key.Opaque()), nil
	})
	if err != nil {
		t.Fatalf("BeginColdResetWithResolver() error = %v", err)
	}
	_, err = session.EvaluateAllColdExactBatch(t.Context(), func(
		_ context.Context,
		batch ColdExactBatch,
	) error {
		return completeColdExactBatchWithSerializedResolver(batch, inputs, entered, release)
	}, queries...)
	if err != nil {
		t.Fatalf("EvaluateAllColdExactBatch() error = %v", err)
	}
	if maximum.Load() != 1 {
		t.Fatalf("maximum concurrent legacy resolver calls = %d, want 1", maximum.Load())
	}
	mustCommit(t, session)
}

func TestColdExactBatchCancellationSkipsResolver(t *testing.T) {
	inputKey := NewInputKey("input")
	queryKey := NewQueryKey("query")
	graph := exactValueTestGraph(t, queryKey)
	var resolverCalls atomic.Int64
	session, err := graph.BeginColdResetWithConcurrentResolver(func(_ context.Context, key InputKey) (Input, error) {
		resolverCalls.Add(1)
		return exactInput(key, "revision", "value"), nil
	})
	if err != nil {
		t.Fatalf("BeginColdResetWithConcurrentResolver() error = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	_, err = session.EvaluateAllColdExactBatch(ctx, func(_ context.Context, batch ColdExactBatch) error {
		cancel()
		_, _, readErr := batch.Query(0).Input(inputKey)
		return readErr
	}, queryKey)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("EvaluateAllColdExactBatch() error = %v, want %v", err, context.Canceled)
	}
	if calls := resolverCalls.Load(); calls != 0 {
		t.Fatalf("resolver calls = %d, want 0", calls)
	}
	if graph.Generation() != 0 {
		t.Fatal("cancelled input read changed the graph")
	}
}

func TestColdExactBatchInputPublicationIsDeterministicAndAtomic(t *testing.T) {
	low := NewInputKey("a-input")
	high := NewInputKey("z-input")
	for range 100 {
		session := &Session{
			inputChanges:  map[InputKey]inputEntry{},
			inputVersions: map[inputVersionKey]inputEntry{},
			observations:  map[InputKey]InputRevision{},
		}
		state := &coldExactBatchState{}
		for key, failure := range map[InputKey]error{
			high: errors.New("z failure"),
			low:  errors.New("a failure"),
		} {
			shard := &state.inputShards[coldExactBatchInputShardIndex(key)]
			if shard.inputs == nil {
				shard.inputs = map[InputKey]*coldExactBatchInputResolution{}
			}
			shard.inputs[key] = &coldExactBatchInputResolution{err: failure}
		}
		if err := session.publishColdExactBatchInputs(state); err == nil || err.Error() != "a failure" {
			t.Fatalf("publishColdExactBatchInputs() error = %v", err)
		}
		if len(session.inputChanges) != 0 || len(session.inputVersions) != 0 || len(session.observations) != 0 {
			t.Fatalf("failed publication changed session inputs: %#v", session.inputChanges)
		}
	}

	existing := inputEntry{revision: NewRevision("old"), found: true, value: []byte("old")}
	session := &Session{
		inputChanges:  map[InputKey]inputEntry{high: existing},
		inputVersions: map[inputVersionKey]inputEntry{},
		observations:  map[InputKey]InputRevision{},
	}
	state := &coldExactBatchState{}
	for key, entry := range map[InputKey]inputEntry{
		low:  {revision: NewRevision("new-low"), found: true, value: []byte("low")},
		high: {revision: NewRevision("new-high"), found: true, value: []byte("high")},
	} {
		shard := &state.inputShards[coldExactBatchInputShardIndex(key)]
		if shard.inputs == nil {
			shard.inputs = map[InputKey]*coldExactBatchInputResolution{}
		}
		shard.inputs[key] = &coldExactBatchInputResolution{entry: entry}
	}
	if err := session.publishColdExactBatchInputs(state); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("publishColdExactBatchInputs() error = %v, want ErrRevisionConflict", err)
	}
	if _, exists := session.inputChanges[low]; exists {
		t.Fatal("failed publication installed its valid prefix")
	}
	if len(session.inputChanges) != 1 || session.inputChanges[high].revision != existing.revision ||
		len(session.inputVersions) != 0 || len(session.observations) != 0 {
		t.Fatalf("failed publication changed session state: %#v", session.inputChanges)
	}
}
