package incremental

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestColdExactBatchKeepsPerQueryDependenciesAndRoots(t *testing.T) {
	leftInput := NewInputKey("left-input")
	rightInput := NewInputKey("right-input")
	leftQuery := NewQueryKey("left-query")
	rightQuery := NewQueryKey("right-query")
	graph := exactValueTestGraph(t, leftQuery, rightQuery)
	session := mustColdSession(t, graph,
		exactInput(leftInput, "left-1", "left"),
		exactInput(rightInput, "right-1", "right"),
	)
	inputByQuery := map[QueryKey]InputKey{leftQuery: leftInput, rightQuery: rightInput}
	created := make([]ExactValueRoot, 2)
	results, err := session.EvaluateAllColdExactBatch(t.Context(), func(
		_ context.Context,
		batch ColdExactBatch,
	) error {
		return completeColdExactBatchFromInputs(batch, inputByQuery, leftQuery, rightQuery, created)
	}, rightQuery, leftQuery)
	if err != nil {
		t.Fatalf("EvaluateAllColdExactBatch() error = %v", err)
	}
	for index, result := range results {
		if result.Value != created[index] {
			t.Fatalf("result %d root was replaced", index)
		}
		if err := graph.ValidateExactValue(result.Key, result.Value); err != nil {
			t.Fatalf("result %d root authority: %v", index, err)
		}
	}
	mustCommit(t, session)

	warm := mustBegin(t, graph)
	mustApply(t, warm, exactInput(leftInput, "left-2", "changed"))
	dirty, err := warm.DirtyQueries()
	if err != nil {
		t.Fatalf("DirtyQueries() error = %v", err)
	}
	if len(dirty) != 1 || dirty[0] != leftQuery {
		t.Fatalf("dirty queries = %#v, want only %#v", dirty, leftQuery)
	}
	warm.Abort()
}

func completeColdExactBatchFromInputs(
	batch ColdExactBatch,
	inputByQuery map[QueryKey]InputKey,
	leftQuery, rightQuery QueryKey,
	created []ExactValueRoot,
) error {
	if batch.Len() != 2 || batch.Query(0).Key() != leftQuery || batch.Query(1).Key() != rightQuery {
		return fmt.Errorf("cold batch order is not deterministic")
	}
	for index := range batch.Len() {
		query := batch.Query(index)
		value, found, readErr := query.Input(inputByQuery[query.Key()])
		if readErr != nil || !found {
			return fmt.Errorf("reading %q: found=%v: %w", query.Key().Opaque(), found, readErr)
		}
		created[index], readErr = query.Complete(string(value))
		if readErr != nil {
			return readErr
		}
	}
	return nil
}

func TestColdExactBatchCompleteWaveKeepsIndependentExactRoots(t *testing.T) {
	left := NewQueryKey("left")
	right := NewQueryKey("right")
	graph := exactValueTestGraph(t, left, right)
	session := mustColdSession(t, graph)
	var created []ExactResult

	results, err := session.EvaluateAllColdExactBatch(t.Context(), func(
		_ context.Context,
		batch ColdExactBatch,
	) error {
		var completeErr error
		created, completeErr = batch.CompleteWave(
			ColdExactBatchValue{Index: 0, Key: left, Value: "same"},
			ColdExactBatchValue{Index: 1, Key: right, Value: "same"},
		)
		return completeErr
	}, right, left)
	requireColdExactBatchResults(t, results, err, left, right)
	if len(created) != 2 || created[0].Key != left || created[1].Key != right {
		t.Fatalf("CompleteWave() results = %#v", created)
	}
	for index := range results {
		same, sameErr := results[index].Value.SameRoot(created[index].Value)
		if sameErr != nil || !same {
			t.Fatalf("result %d root = same %v, error %v", index, same, sameErr)
		}
		if err := graph.ValidateExactValue(results[index].Key, results[index].Value); err != nil {
			t.Fatalf("result %d root authority: %v", index, err)
		}
	}
	same, err := created[0].Value.SameRoot(created[1].Value)
	if err != nil || same {
		t.Fatalf("cross-query SameRoot() = %v, %v", same, err)
	}
}

func TestColdExactBatchCompleteWaveRejectsFinalKeyAtomically(t *testing.T) {
	left := NewQueryKey("left")
	right := NewQueryKey("right")
	graph := exactValueTestGraph(t, left, right)
	session := mustColdSession(t, graph)

	results, err := session.EvaluateAllColdExactBatch(t.Context(), func(
		_ context.Context,
		batch ColdExactBatch,
	) error {
		values := []ColdExactBatchValue{
			{Index: 0, Key: left, Value: "left"},
			{Index: 1, Key: NewQueryKey("foreign"), Value: "right"},
		}
		if _, completeErr := batch.CompleteWave(values...); completeErr == nil ||
			!strings.Contains(completeErr.Error(), "invalid authority") {
			return fmt.Errorf("poisoned CompleteWave() error = %v", completeErr)
		}
		for index := range batch.Len() {
			if batch.state.completions[index].Load() != coldExactBatchValueUnset ||
				batch.state.slots[index] != (coldExactBatchValueSlot{}) {
				return fmt.Errorf("failed completion wave mutated slot %d", index)
			}
		}
		values[1].Key = right
		_, completeErr := batch.CompleteWave(values...)
		return completeErr
	}, left, right)
	requireColdExactBatchResults(t, results, err, left, right)
}

func TestColdExactBatchCompleteWaveRejectsPartialFinalSlotAtomically(t *testing.T) {
	left := NewQueryKey("left")
	right := NewQueryKey("right")
	graph := exactValueTestGraph(t, left, right)
	session := mustColdSession(t, graph)

	results, err := session.EvaluateAllColdExactBatch(t.Context(), func(
		_ context.Context,
		batch ColdExactBatch,
	) error {
		poison := exactValueExecution{key: right}
		batch.state.slots[1].execution = poison
		values := []ColdExactBatchValue{
			{Index: 0, Key: left, Value: "left"},
			{Index: 1, Key: right, Value: "right"},
		}
		if _, completeErr := batch.CompleteWave(values...); completeErr == nil ||
			!strings.Contains(completeErr.Error(), "invalid provenance") {
			return fmt.Errorf("partial-slot CompleteWave() error = %v", completeErr)
		}
		if batch.state.completions[0].Load() != coldExactBatchValueUnset ||
			batch.state.slots[0] != (coldExactBatchValueSlot{}) ||
			batch.state.completions[1].Load() != coldExactBatchValueUnset ||
			batch.state.slots[1].execution != poison {
			return fmt.Errorf("failed completion wave mutated a slot")
		}
		batch.state.slots[1] = coldExactBatchValueSlot{}
		_, completeErr := batch.CompleteWave(values...)
		return completeErr
	}, left, right)
	requireColdExactBatchResults(t, results, err, left, right)
}

func TestColdExactBatchCompleteWaveRejectsCorruptedAuthorityAtomically(t *testing.T) {
	left := NewQueryKey("left")
	right := NewQueryKey("right")
	graph := exactValueTestGraph(t, left, right)
	session := mustColdSession(t, graph)

	results, err := session.EvaluateAllColdExactBatch(t.Context(), func(
		_ context.Context,
		batch ColdExactBatch,
	) error {
		values := []ColdExactBatchValue{
			{Index: 0, Key: left, Value: "left"},
			{Index: 1, Key: right, Value: "right"},
		}
		authority := &batch.state.observationAuthority
		authority.seal = nil
		if _, completeErr := batch.CompleteWave(values...); completeErr == nil ||
			!strings.Contains(completeErr.Error(), "invalid authority") {
			return fmt.Errorf("corrupted-authority CompleteWave() error = %v", completeErr)
		}
		for index := range batch.Len() {
			if batch.state.completions[index].Load() != coldExactBatchValueUnset ||
				batch.state.slots[index] != (coldExactBatchValueSlot{}) {
				return fmt.Errorf("failed completion wave mutated slot %d", index)
			}
		}
		authority.seal = authority
		_, completeErr := batch.CompleteWave(values...)
		return completeErr
	}, left, right)
	requireColdExactBatchResults(t, results, err, left, right)
}

func TestColdExactBatchConcurrentCompleteWavePublishesOneWholeWave(t *testing.T) {
	left := NewQueryKey("left")
	right := NewQueryKey("right")
	graph := exactValueTestGraph(t, left, right)
	session := mustColdSession(t, graph)

	results, err := session.EvaluateAllColdExactBatch(t.Context(), func(
		_ context.Context,
		batch ColdExactBatch,
	) error {
		return completeConcurrentColdExactBatchWaves(batch, [][]ColdExactBatchValue{
			{
				{Index: 0, Key: left, Value: "first-left"},
				{Index: 1, Key: right, Value: "first-right"},
			},
			{
				{Index: 0, Key: left, Value: "second-left"},
				{Index: 1, Key: right, Value: "second-right"},
			},
		})
	}, left, right)
	requireColdExactBatchResults(t, results, err, left, right)
	leftValue := coldExactRootString(t, results[0].Value)
	rightValue := coldExactRootString(t, results[1].Value)
	if leftValue != "first-left" && leftValue != "second-left" {
		t.Fatalf("left value = %q", leftValue)
	}
	if (leftValue == "first-left") != (rightValue == "first-right") {
		t.Fatalf("mixed completion wave values = %q, %q", leftValue, rightValue)
	}
}

func exerciseColdExactBatchHandleAuthority(batch ColdExactBatch, retained ColdExactBatchQuery) error {
	forged := retained
	forged.index = 1
	if _, completeErr := forged.Complete("forged"); completeErr == nil ||
		!strings.Contains(completeErr.Error(), "invalid authority") {
		return fmt.Errorf("forged Complete() error = %v", completeErr)
	}
	if _, completeErr := batch.Query(-1).Complete("invalid"); completeErr == nil {
		return fmt.Errorf("out-of-range query completed")
	}
	leftRoot, completeErr := retained.Complete("left")
	if completeErr != nil {
		return completeErr
	}
	if _, completeErr := retained.Complete("duplicate"); completeErr == nil ||
		!strings.Contains(completeErr.Error(), "already has a value") {
		return fmt.Errorf("duplicate Complete() error = %v", completeErr)
	}
	rightRoot, completeErr := batch.Query(1).Complete("right")
	if completeErr != nil {
		return completeErr
	}
	if same, sameErr := leftRoot.SameRoot(rightRoot); sameErr != nil || same {
		return fmt.Errorf("cross-query SameRoot() = %v, %v", same, sameErr)
	}
	return nil
}

func TestColdExactBatchHandleAuthorityAndRevocation(t *testing.T) {
	left := NewQueryKey("left")
	right := NewQueryKey("right")
	graph := exactValueTestGraph(t, left, right)
	session := mustColdSession(t, graph)
	var retainedBatch ColdExactBatch
	var retained ColdExactBatchQuery
	results, err := session.EvaluateAllColdExactBatch(t.Context(), func(
		_ context.Context,
		batch ColdExactBatch,
	) error {
		retainedBatch = batch
		retained = batch.Query(0)
		return exerciseColdExactBatchHandleAuthority(batch, retained)
	}, right, left)
	if err != nil {
		t.Fatalf("EvaluateAllColdExactBatch() error = %v", err)
	}
	if err := graph.ValidateExactValue(left, results[0].Value); err != nil {
		t.Fatalf("left root authority: %v", err)
	}
	if err := graph.ValidateExactValue(right, results[1].Value); err != nil {
		t.Fatalf("right root authority: %v", err)
	}
	if _, err := retained.Complete("late"); err == nil || !strings.Contains(err.Error(), "no longer active") {
		t.Fatalf("late Complete() error = %v", err)
	}
	if _, _, err := retained.Input(NewInputKey("late")); err == nil ||
		!strings.Contains(err.Error(), "no longer active") {
		t.Fatalf("late Input() error = %v", err)
	}
	lateInput := NewInputKey("late")
	lateRevision := NewRevision("late")
	lateCalls := []struct {
		name string
		call func() error
	}{
		{name: "ExactInput", call: func() error {
			_, callErr := retained.ExactInput(lateInput)
			return callErr
		}},
		{name: "ExactInputOwned", call: func() error {
			_, callErr := retained.ExactInputOwned(lateInput)
			return callErr
		}},
		{name: "ObserveExactInput", call: func() error {
			return retained.ObserveExactInput(InputRevision{
				Key: lateInput, Revision: lateRevision, Found: true,
			})
		}},
		{name: "ObserveExactInputValue", call: func() error {
			return retained.ObserveExactInputValue(Input{
				Key: lateInput, Revision: lateRevision, Found: true, Value: []byte("late"),
			})
		}},
		{name: "Query", call: func() error {
			_, callErr := retained.Query(t.Context(), right)
			return callErr
		}},
		{name: "retained batch factory", call: func() error {
			_, callErr := retainedBatch.Query(1).Complete("late")
			return callErr
		}},
	}
	for _, lateCall := range lateCalls {
		if callErr := lateCall.call(); callErr == nil ||
			!strings.Contains(callErr.Error(), "no longer active") {
			t.Errorf("late %s error = %v", lateCall.name, callErr)
		}
	}
}

func TestColdExactBatchRejectsConcurrentDuplicateCompletion(t *testing.T) {
	queryKey := NewQueryKey("query")
	graph := exactValueTestGraph(t, queryKey)
	session := mustColdSession(t, graph)
	results, err := session.EvaluateAllColdExactBatch(t.Context(), func(
		_ context.Context,
		batch ColdExactBatch,
	) error {
		query := batch.Query(0)
		errs := make([]error, 2)
		var workers sync.WaitGroup
		workers.Add(len(errs))
		for index := range errs {
			go func() {
				defer workers.Done()
				_, errs[index] = query.Complete(fmt.Sprintf("value-%d", index))
			}()
		}
		workers.Wait()
		successes := 0
		for _, completeErr := range errs {
			if completeErr == nil {
				successes++
				continue
			}
			if !strings.Contains(completeErr.Error(), "already has a value") {
				return completeErr
			}
		}
		if successes != 1 {
			return fmt.Errorf("successful completions = %d, want 1", successes)
		}
		return nil
	}, queryKey)
	if err != nil {
		t.Fatalf("EvaluateAllColdExactBatch() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %#v", results)
	}
}

func TestColdExactBatchRejectsNestedBatchExecution(t *testing.T) {
	outer := NewQueryKey("outer")
	inner := NewQueryKey("inner")
	graph := exactValueTestGraph(t, outer, inner)
	session := mustColdSession(t, graph)
	_, err := session.EvaluateAllColdExactBatch(t.Context(), func(
		ctx context.Context,
		batch ColdExactBatch,
	) error {
		if _, completeErr := batch.Query(0).Complete("outer"); completeErr != nil {
			return completeErr
		}
		_, nestedErr := session.EvaluateAllColdExactBatch(ctx, func(
			_ context.Context,
			nested ColdExactBatch,
		) error {
			_, completeErr := nested.Query(0).Complete("inner")
			return completeErr
		}, inner)
		return nestedErr
	}, outer)
	if err == nil || !strings.Contains(err.Error(), "already active") {
		t.Fatalf("nested EvaluateAllColdExactBatch() error = %v", err)
	}
	if graph.Generation() != 0 {
		t.Fatal("nested batch changed the graph")
	}
}

func TestColdExactBatchRejectsForeignRootState(t *testing.T) {
	queryKey := NewQueryKey("query")
	foreignGraph := exactValueTestGraph(t, queryKey)
	foreignSession := mustBegin(t, foreignGraph)
	foreignResults, err := foreignSession.EvaluateAllExactBatch(
		t.Context(),
		exactStringBatch(map[QueryKey]string{queryKey: "foreign"}),
		queryKey,
	)
	if err != nil {
		t.Fatalf("foreign EvaluateAllExactBatch() error = %v", err)
	}
	foreignSession.Abort()

	graph := exactValueTestGraph(t, queryKey)
	session := mustColdSession(t, graph)
	_, err = session.EvaluateAllColdExactBatch(t.Context(), func(
		_ context.Context,
		batch ColdExactBatch,
	) error {
		state := batch.Query(0).state
		state.slots[0] = coldExactBatchValueSlot{value: *foreignResults[0].Value.value}
		state.slots[0].value.seal = &state.slots[0].value
		state.slots[0].value.storage.owner = &state.slots[0].value
		state.completions[0].Store(coldExactBatchValueReady)
		return nil
	}, queryKey)
	if err == nil || !strings.Contains(err.Error(), "another query") {
		t.Fatalf("EvaluateAllColdExactBatch() error = %v", err)
	}
	if graph.Generation() != 0 {
		t.Fatal("foreign root state changed the graph")
	}
}

func TestColdExactBatchRejectsMissingContiguousValueStorage(t *testing.T) {
	queryKey := NewQueryKey("query")
	graph := exactValueTestGraph(t, queryKey)
	session := mustColdSession(t, graph)
	_, err := session.EvaluateAllColdExactBatch(t.Context(), func(
		_ context.Context,
		batch ColdExactBatch,
	) error {
		query := batch.Query(0)
		query.state.slots = query.state.slots[:0]
		_, completeErr := query.Complete("value")
		return completeErr
	}, queryKey)
	if err == nil || !strings.Contains(err.Error(), "invalid authority") {
		t.Fatalf("EvaluateAllColdExactBatch() error = %v", err)
	}
	if graph.Generation() != 0 {
		t.Fatal("missing value storage changed the graph")
	}
}

func TestColdExactBatchRejectsMemberDependenciesAtAnyDepth(t *testing.T) {
	left := NewQueryKey("left")
	right := NewQueryKey("right")
	middle := NewQueryKey("middle")
	tests := []struct {
		name string
		read func(context.Context, ColdExactBatch) error
	}{
		{
			name: "direct",
			read: func(ctx context.Context, batch ColdExactBatch) error {
				_, err := batch.Query(0).Query(ctx, right)
				return err
			},
		},
		{
			name: "nested",
			read: func(ctx context.Context, batch ColdExactBatch) error {
				_, err := batch.Query(0).Query(ctx, middle)
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			graph := mustGraph(t,
				Definition{Key: left, Run: constantQuery("left")},
				Definition{Key: right, Run: constantQuery("right")},
				Definition{Key: middle, Run: func(ctx context.Context, reader Reader) ([]byte, error) {
					return reader.Query(ctx, right)
				}},
			)
			session := mustColdSession(t, graph)
			_, err := session.EvaluateAllColdExactBatch(t.Context(), func(
				ctx context.Context,
				batch ColdExactBatch,
			) error {
				return test.read(ctx, batch)
			}, left, right)
			if err == nil || !strings.Contains(err.Error(), "cannot depend on another batch member") {
				t.Fatalf("EvaluateAllColdExactBatch() error = %v", err)
			}
			if graph.Generation() != 0 {
				t.Fatal("rejected member dependency changed the graph")
			}
		})
	}
}

func recordColdExactBatchDependenciesConcurrently(
	batch ColdExactBatch,
	inputKey InputKey,
	expected InputRevision,
) error {
	query := batch.Query(0)
	errs := make([]error, 64)
	var workers sync.WaitGroup
	workers.Add(len(errs))
	for index := range errs {
		go func() {
			defer workers.Done()
			if index%2 == 0 {
				errs[index] = query.ObserveExactInput(expected)
				return
			}
			input, readErr := query.ExactInputOwned(inputKey)
			if readErr == nil {
				input.Value[0] = 'X'
			}
			errs[index] = readErr
		}()
	}
	workers.Wait()
	for _, readErr := range errs {
		if readErr != nil {
			return readErr
		}
	}
	_, completeErr := query.Complete("done")
	return completeErr
}

func TestColdExactBatchConcurrentDependencyRecording(t *testing.T) {
	inputKey := NewInputKey("input")
	queryKey := NewQueryKey("query")
	expected := InputRevision{Key: inputKey, Revision: NewRevision("revision"), Found: true}
	graph := exactValueTestGraph(t, queryKey)
	session := mustColdSession(t, graph, exactInput(inputKey, "revision", "immutable"))
	_, err := session.EvaluateAllColdExactBatch(t.Context(), func(
		_ context.Context,
		batch ColdExactBatch,
	) error {
		return recordColdExactBatchDependenciesConcurrently(batch, inputKey, expected)
	}, queryKey)
	if err != nil {
		t.Fatalf("EvaluateAllColdExactBatch() error = %v", err)
	}
	var verified []InputRevision
	err = session.Commit(t.Context(), func(_ context.Context, inputs []InputRevision) (bool, error) {
		verified = append([]InputRevision(nil), inputs...)
		return true, nil
	})
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if len(verified) != 1 || verified[0] != expected {
		t.Fatalf("verified inputs = %#v, want %#v", verified, expected)
	}
}

func TestColdExactBatchCanceledNestedQueryDoesNotWaitForAnotherQuery(t *testing.T) {
	left := NewQueryKey("left")
	right := NewQueryKey("right")
	slow := NewQueryKey("slow")
	fast := NewQueryKey("fast")
	entered := make(chan struct{})
	release := make(chan struct{})
	graph := mustGraph(t,
		Definition{Key: left, Run: constantQuery("left")},
		Definition{Key: right, Run: constantQuery("right")},
		Definition{Key: slow, Run: func(ctx context.Context, _ Reader) ([]byte, error) {
			close(entered)
			select {
			case <-release:
				return []byte("slow"), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}},
		Definition{Key: fast, Run: constantQuery("fast")},
	)
	session := mustColdSession(t, graph)
	_, err := session.EvaluateAllColdExactBatch(t.Context(), func(
		ctx context.Context,
		batch ColdExactBatch,
	) error {
		slowErr := make(chan error, 1)
		go func() {
			_, queryErr := batch.Query(0).Query(ctx, slow)
			slowErr <- queryErr
		}()
		<-entered

		canceled, cancel := context.WithCancel(ctx)
		cancel()
		_, queryErr := batch.Query(1).Query(canceled, fast)
		if !errors.Is(queryErr, context.Canceled) {
			close(release)
			<-slowErr
			return fmt.Errorf("canceled nested Query() error = %v", queryErr)
		}
		close(release)
		if queryErr := <-slowErr; queryErr != nil {
			return queryErr
		}
		_, completeErr := batch.Query(0).Complete("left")
		if completeErr != nil {
			return completeErr
		}
		_, completeErr = batch.Query(1).Complete("right")
		return completeErr
	}, left, right)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("EvaluateAllColdExactBatch() error = %v, want %v", err, context.Canceled)
	}
	if graph.Generation() != 0 {
		t.Fatal("canceled nested query changed the graph")
	}
}

func requireColdExactBatchStateReleased(t *testing.T, state *coldExactBatchState) {
	t.Helper()
	if state == nil || !state.revoked || state.session != nil || state.ctx != nil ||
		state.keys != nil || state.frames != nil || state.slots != nil || state.entries != nil ||
		state.completions != nil || state.knownInputs != nil || state.resolverToken != nil {
		t.Fatalf("retained handle kept cold execution state: %#v", state)
	}
	for index := range state.inputShards {
		if state.inputShards[index].inputs != nil {
			t.Fatalf("retained handle kept input shard %d", index)
		}
	}
}

func TestColdExactBatchRetainedHandleReleasesExecutionState(t *testing.T) {
	inputKey := NewInputKey("input")
	queryKey := NewQueryKey("query")
	graph := exactValueTestGraph(t, queryKey)
	session, err := graph.BeginColdResetWithConcurrentResolver(func(_ context.Context, key InputKey) (Input, error) {
		return exactInput(key, "revision", strings.Repeat("input", 1024)), nil
	})
	if err != nil {
		t.Fatalf("BeginColdResetWithConcurrentResolver() error = %v", err)
	}
	var retainedBatch ColdExactBatch
	var retainedQuery ColdExactBatchQuery
	results, err := session.EvaluateAllColdExactBatch(t.Context(), func(
		_ context.Context,
		batch ColdExactBatch,
	) error {
		retainedBatch = batch
		retainedQuery = batch.Query(0)
		if _, readErr := retainedQuery.ExactInputOwned(inputKey); readErr != nil {
			return readErr
		}
		_, completeErr := retainedQuery.Complete(strings.Repeat("result", 1024))
		return completeErr
	}, queryKey)
	if err != nil {
		t.Fatalf("EvaluateAllColdExactBatch() error = %v", err)
	}
	requireColdExactBatchStateReleased(t, retainedQuery.state)
	if retainedBatch.Len() != 1 || retainedQuery.Key() != queryKey {
		t.Fatalf("retained handle identity changed: len=%d key=%#v", retainedBatch.Len(), retainedQuery.Key())
	}
	if _, err := retainedQuery.Complete("late"); err == nil || !strings.Contains(err.Error(), "no longer active") {
		t.Fatalf("late Complete() error = %v", err)
	}
	if _, err := retainedBatch.Query(0).Complete("late"); err == nil ||
		!strings.Contains(err.Error(), "no longer active") {
		t.Fatalf("late batch query Complete() error = %v", err)
	}
	value, valueErr := results[0].Value.String()
	if valueErr != nil || value != strings.Repeat("result", 1024) {
		t.Fatalf("retained exact result = %q, %v", value, valueErr)
	}
	mustCommit(t, session)
}

func TestColdExactBatchDrainsAcceptedReadsBeforeRevocation(t *testing.T) {
	inputKey := NewInputKey("input")
	queryKey := NewQueryKey("query")
	graph := exactValueTestGraph(t, queryKey)
	resolverStarted := make(chan struct{})
	releaseResolver := make(chan struct{})
	session, err := graph.BeginColdResetWithResolver(func(_ context.Context, key InputKey) (Input, error) {
		close(resolverStarted)
		<-releaseResolver
		return exactInput(key, "revision", "value"), nil
	})
	if err != nil {
		t.Fatalf("BeginColdResetWithResolver() error = %v", err)
	}
	type evaluation struct {
		results []ExactResult
		err     error
	}
	evaluated := make(chan evaluation, 1)
	readFinished := make(chan error, 1)
	var retained ColdExactBatchQuery
	go func() {
		results, evaluateErr := session.EvaluateAllColdExactBatch(t.Context(), func(
			_ context.Context,
			batch ColdExactBatch,
		) error {
			retained = batch.Query(0)
			if _, completeErr := retained.Complete("done"); completeErr != nil {
				return completeErr
			}
			go func() {
				_, readErr := retained.ExactInputOwned(inputKey)
				readFinished <- readErr
			}()
			<-resolverStarted
			return nil
		}, queryKey)
		evaluated <- evaluation{results: results, err: evaluateErr}
	}()
	<-resolverStarted
	select {
	case result := <-evaluated:
		t.Fatalf("EvaluateAllColdExactBatch() returned before the accepted read drained: %#v", result)
	default:
	}
	close(releaseResolver)
	if err := <-readFinished; err != nil {
		t.Fatalf("accepted read error = %v", err)
	}
	result := <-evaluated
	if result.err != nil || len(result.results) != 1 {
		t.Fatalf("EvaluateAllColdExactBatch() = %#v, %v", result.results, result.err)
	}
	if _, err := retained.Complete("late"); err == nil || !strings.Contains(err.Error(), "no longer active") {
		t.Fatalf("late Complete() error = %v", err)
	}
	mustCommit(t, session)
}

func sealColdExactBatchProducerWave(
	batch ColdExactBatch,
	session *Session,
	graph *Graph,
	inputKey InputKey,
	producer QueryKey,
) (ColdExactBatchQuery, error) {
	producerQuery := batch.Query(0)
	value, found, err := producerQuery.Input(inputKey)
	if err != nil || !found {
		return ColdExactBatchQuery{}, fmt.Errorf("reading producer input: found=%v: %w", found, err)
	}
	producerRoot, err := producerQuery.Complete(string(value))
	if err != nil {
		return ColdExactBatchQuery{}, err
	}
	if err := batch.SealWave(ExactResult{Key: producer, Value: producerRoot}); err != nil {
		return ColdExactBatchQuery{}, err
	}
	if err := session.ValidateCurrentExactValue(producer, producerRoot); err != nil {
		return ColdExactBatchQuery{}, fmt.Errorf("validating sealed producer: %w", err)
	}
	if graph.Generation() != 0 {
		return ColdExactBatchQuery{}, fmt.Errorf("sealed wave changed graph generation")
	}
	if _, found := graph.Value(producer); found {
		return ColdExactBatchQuery{}, fmt.Errorf("sealed wave escaped its speculative session")
	}
	if _, err := producerQuery.Complete("late"); err == nil || !strings.Contains(err.Error(), "sealed") {
		return ColdExactBatchQuery{}, fmt.Errorf("sealed producer remained mutable: %v", err)
	}
	return producerQuery, nil
}

func TestColdExactBatchSealsDependencyWavesInsideOneTransaction(t *testing.T) {
	inputKey := NewInputKey("producer-input")
	producer := NewQueryKey("00-producer")
	consumer := NewQueryKey("10-consumer")
	graph := exactValueTestGraph(t, producer, consumer)
	session := mustColdSession(t, graph, exactInput(inputKey, "r1", "value"))
	var retained ColdExactBatchQuery

	results, err := session.EvaluateAllColdExactBatch(t.Context(), func(
		ctx context.Context,
		batch ColdExactBatch,
	) error {
		producerQuery, err := sealColdExactBatchProducerWave(batch, session, graph, inputKey, producer)
		if err != nil {
			return err
		}
		retained = producerQuery

		consumerQuery := batch.Query(1)
		producerValue, err := consumerQuery.Query(ctx, producer)
		if err != nil {
			return err
		}
		consumerRoot, err := consumerQuery.Complete(string(producerValue) + "-consumer")
		if err != nil {
			return err
		}
		return batch.SealWave(ExactResult{Key: consumer, Value: consumerRoot})
	}, consumer, producer)
	if err != nil {
		t.Fatalf("EvaluateAllColdExactBatch() error = %v", err)
	}
	if len(results) != 2 || coldExactRootString(t, results[0].Value) != "value" ||
		coldExactRootString(t, results[1].Value) != "value-consumer" {
		t.Fatalf("results = %#v", results)
	}
	if _, _, err := retained.Input(inputKey); err == nil || !strings.Contains(err.Error(), "no longer active") {
		t.Fatalf("retained sealed Input() error = %v", err)
	}
	mustCommit(t, session)

	warm := mustBegin(t, graph)
	mustApply(t, warm, exactInput(inputKey, "r2", "changed"))
	dirty, err := warm.DirtyQueries()
	if err != nil {
		t.Fatalf("DirtyQueries() error = %v", err)
	}
	if len(dirty) != 2 || dirty[0] != producer || dirty[1] != consumer {
		t.Fatalf("dirty queries = %#v, want producer and consumer", dirty)
	}
	warm.Abort()
}

func TestColdExactBatchSealWaveRejectsSubstitutedRootAtomically(t *testing.T) {
	left := NewQueryKey("left")
	right := NewQueryKey("right")
	graph := exactValueTestGraph(t, left, right)
	session := mustColdSession(t, graph)

	_, err := session.EvaluateAllColdExactBatch(t.Context(), func(
		_ context.Context,
		batch ColdExactBatch,
	) error {
		leftRoot, err := batch.Query(0).Complete("left")
		if err != nil {
			return err
		}
		rightRoot, err := batch.Query(1).Complete("right")
		if err != nil {
			return err
		}
		err = batch.SealWave(
			ExactResult{Key: left, Value: leftRoot},
			ExactResult{Key: right, Value: leftRoot},
		)
		if err == nil || !strings.Contains(err.Error(), "belongs to another query") {
			return fmt.Errorf("SealWave() error = %v", err)
		}
		if len(session.nodeChanges) != 0 {
			return fmt.Errorf("failed wave staged %d nodes", len(session.nodeChanges))
		}
		if err := batch.SealWave(ExactResult{Key: right, Value: rightRoot}); err == nil {
			return fmt.Errorf("poisoned batch accepted another wave")
		}
		return nil
	}, left, right)
	if err == nil || !strings.Contains(err.Error(), "belongs to another query") {
		t.Fatalf("EvaluateAllColdExactBatch() error = %v", err)
	}
	if graph.Generation() != 0 {
		t.Fatal("failed wave changed graph generation")
	}
	if _, found := graph.Value(left); found {
		t.Fatal("failed wave published its valid prefix")
	}
}

func TestColdExactBatchExplicitWavesMustSealEveryMember(t *testing.T) {
	left := NewQueryKey("left")
	right := NewQueryKey("right")
	graph := exactValueTestGraph(t, left, right)
	session := mustColdSession(t, graph)

	_, err := session.EvaluateAllColdExactBatch(t.Context(), func(
		_ context.Context,
		batch ColdExactBatch,
	) error {
		leftRoot, err := batch.Query(0).Complete("left")
		if err != nil {
			return err
		}
		if err := batch.SealWave(ExactResult{Key: left, Value: leftRoot}); err != nil {
			return err
		}
		_, err = batch.Query(1).Complete("right")
		return err
	}, left, right)
	if err == nil || !strings.Contains(err.Error(), "did not seal every query") {
		t.Fatalf("EvaluateAllColdExactBatch() error = %v", err)
	}
	if graph.Generation() != 0 {
		t.Fatal("incomplete wave sequence changed graph generation")
	}
	if _, found := graph.Value(left); found {
		t.Fatal("session-local sealed wave escaped to graph")
	}
}

func TestColdExactBatchFailuresPublishNothing(t *testing.T) {
	left := NewQueryKey("left")
	right := NewQueryKey("right")
	graph := exactValueTestGraph(t, left, right)
	session := mustColdSession(t, graph)
	_, err := session.EvaluateAllColdExactBatch(t.Context(), func(
		_ context.Context,
		batch ColdExactBatch,
	) error {
		_, completeErr := batch.Query(0).Complete("left")
		return completeErr
	}, left, right)
	if err == nil || !strings.Contains(err.Error(), "did not produce a value") {
		t.Fatalf("EvaluateAllColdExactBatch() error = %v", err)
	}
	if err := session.Commit(t.Context(), acceptRevisions); err == nil ||
		!strings.Contains(err.Error(), "did not produce a value") {
		t.Fatalf("Commit() error = %v", err)
	}
	if graph.Generation() != 0 {
		t.Fatal("incomplete batch changed the graph")
	}
}

func TestColdExactBatchPanicAndCancellationAreAtomic(t *testing.T) {
	queryKey := NewQueryKey("query")
	tests := []struct {
		name  string
		batch func(context.CancelFunc) ColdExactBatchFunc
		want  error
	}{
		{
			name: "panic",
			batch: func(context.CancelFunc) ColdExactBatchFunc {
				return func(_ context.Context, batch ColdExactBatch) error {
					if _, err := batch.Query(0).Complete("panic"); err != nil {
						return err
					}
					panic("boom")
				}
			},
		},
		{
			name: "cancellation",
			batch: func(cancel context.CancelFunc) ColdExactBatchFunc {
				return func(_ context.Context, batch ColdExactBatch) error {
					if _, err := batch.Query(0).Complete("cancelled"); err != nil {
						return err
					}
					cancel()
					return nil
				}
			},
			want: context.Canceled,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runColdExactBatchAtomicFailure(t, queryKey, test.batch, test.want)
		})
	}
}

func runColdExactBatchAtomicFailure(
	t *testing.T,
	queryKey QueryKey,
	makeBatch func(context.CancelFunc) ColdExactBatchFunc,
	want error,
) {
	t.Helper()
	graph := exactValueTestGraph(t, queryKey)
	session := mustColdSession(t, graph)
	ctx, cancel := context.WithCancel(t.Context())
	_, err := session.EvaluateAllColdExactBatch(ctx, makeBatch(cancel), queryKey)
	if want != nil {
		if !errors.Is(err, want) {
			t.Fatalf("EvaluateAllColdExactBatch() error = %v, want %v", err, want)
		}
	} else if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("EvaluateAllColdExactBatch() error = %v, want panic error", err)
	}
	if graph.Generation() != 0 {
		t.Fatal("failed batch changed the graph")
	}
	if _, found := graph.Value(queryKey); found {
		t.Fatal("failed batch published a value")
	}
}

func TestColdExactBatchCommitConflictKeepsWinner(t *testing.T) {
	queryKey := NewQueryKey("query")
	graph := exactValueTestGraph(t, queryKey)
	winner := mustColdSession(t, graph)
	loser := mustColdSession(t, graph)
	evaluate := func(session *Session, value string) ExactValueRoot {
		t.Helper()
		results, err := session.EvaluateAllColdExactBatch(t.Context(), func(
			_ context.Context,
			batch ColdExactBatch,
		) error {
			_, completeErr := batch.Query(0).Complete(value)
			return completeErr
		}, queryKey)
		if err != nil {
			t.Fatalf("EvaluateAllColdExactBatch() error = %v", err)
		}
		return results[0].Value
	}
	winnerRoot := evaluate(winner, "winner")
	evaluate(loser, "loser")
	mustCommit(t, winner)
	if err := loser.Commit(t.Context(), acceptRevisions); !errors.Is(err, ErrCommitConflict) {
		t.Fatalf("loser Commit() error = %v, want %v", err, ErrCommitConflict)
	}
	committed, found, err := graph.ExactValue(queryKey)
	if err != nil || !found || committed != winnerRoot {
		t.Fatalf("ExactValue() = %v, %v, %v; want winner root", committed, found, err)
	}
}

func TestColdExactBatchRequiresColdReset(t *testing.T) {
	query := NewQueryKey("query")
	tests := []struct {
		name    string
		session func(*testing.T, *Graph) *Session
	}{
		{
			name:    "initial non-reset replacement",
			session: mustBegin,
		},
		{
			name: "warm",
			session: func(t *testing.T, graph *Graph) *Session {
				t.Helper()
				initial := mustBegin(t, graph)
				if _, err := initial.EvaluateAllExactBatch(
					t.Context(),
					exactStringBatch(map[QueryKey]string{query: "initial"}),
					query,
				); err != nil {
					t.Fatalf("initial EvaluateAllExactBatch() error = %v", err)
				}
				mustCommit(t, initial)
				return mustBegin(t, graph)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			graph := exactValueTestGraph(t, query)
			session := test.session(t, graph)
			_, err := session.EvaluateAllColdExactBatch(
				t.Context(),
				func(context.Context, ColdExactBatch) error { return nil },
				query,
			)
			if err == nil || !strings.Contains(err.Error(), "requires a cold-reset session") {
				t.Fatalf("EvaluateAllColdExactBatch() error = %v", err)
			}
			if !errors.Is(session.Commit(t.Context(), acceptRevisions), err) {
				t.Fatalf("Commit() did not retain cold-batch failure")
			}
		})
	}
}

func TestColdExactBatchCommitFreezesTransferredStateBeforeVerifier(t *testing.T) {
	inputKey := NewInputKey("input")
	queryKey := NewQueryKey("query")
	graph := exactValueTestGraph(t, queryKey)
	session := mustColdSession(t, graph, exactInput(inputKey, "r1", "original"))
	_, err := session.EvaluateAllColdExactBatch(t.Context(), func(
		_ context.Context,
		batch ColdExactBatch,
	) error {
		value, found, readErr := batch.Query(0).Input(inputKey)
		if readErr != nil || !found {
			return fmt.Errorf("reading input: found=%v: %w", found, readErr)
		}
		_, completeErr := batch.Query(0).Complete(string(value))
		return completeErr
	}, queryKey)
	if err != nil {
		t.Fatalf("EvaluateAllColdExactBatch() error = %v", err)
	}
	var mutationErr error
	err = session.Commit(t.Context(), func(context.Context, []InputRevision) (bool, error) {
		_, mutationErr = session.ApplyInputsWhileIdle(exactInput(inputKey, "r2", "poison"))
		return true, nil
	})
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if !errors.Is(mutationErr, ErrSessionClosed) {
		t.Fatalf("verifier mutation error = %v, want %v", mutationErr, ErrSessionClosed)
	}
	if got := stringValue(t, graph, queryKey); got != "original" {
		t.Fatalf("committed value = %q, want original", got)
	}
}

func mustColdSession(t *testing.T, graph *Graph, inputs ...Input) *Session {
	t.Helper()
	session, err := graph.BeginColdReset(inputs...)
	if err != nil {
		t.Fatalf("BeginColdReset() error = %v", err)
	}
	return session
}

func coldExactRootString(t *testing.T, root ExactValueRoot) string {
	t.Helper()
	value, err := root.String()
	if err != nil {
		t.Fatalf("ExactValueRoot.String() error = %v", err)
	}
	return value
}

func requireColdExactBatchResults(
	t *testing.T,
	results []ExactResult,
	err error,
	keys ...QueryKey,
) {
	t.Helper()
	if err != nil {
		t.Fatalf("EvaluateAllColdExactBatch() error = %v", err)
	}
	if len(results) != len(keys) {
		t.Fatalf("results = %#v, want %d", results, len(keys))
	}
	for index := range keys {
		if results[index].Key != keys[index] {
			t.Fatalf("result %d key = %#v, want %#v", index, results[index].Key, keys[index])
		}
	}
}

func completeConcurrentColdExactBatchWaves(
	batch ColdExactBatch,
	waves [][]ColdExactBatchValue,
) error {
	errs := make([]error, len(waves))
	completed := make([][]ExactResult, len(waves))
	start := make(chan struct{})
	var workers sync.WaitGroup
	for waveIndex := range waves {
		workers.Go(func() {
			<-start
			completed[waveIndex], errs[waveIndex] = batch.CompleteWave(waves[waveIndex]...)
		})
	}
	close(start)
	workers.Wait()
	winner := -1
	for waveIndex := range waves {
		if errs[waveIndex] == nil {
			if winner >= 0 {
				return fmt.Errorf("multiple completion waves succeeded")
			}
			winner = waveIndex
			continue
		}
		if !strings.Contains(errs[waveIndex].Error(), "already has a value") {
			return fmt.Errorf("losing CompleteWave() error = %v", errs[waveIndex])
		}
	}
	if winner < 0 || len(completed[winner]) != len(waves[winner]) {
		return fmt.Errorf("no complete wave won")
	}
	return nil
}
