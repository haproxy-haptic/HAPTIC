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
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	controllerhttpstore "github.com/haproxy-haptic/HAPTIC/pkg/controller/httpstore"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/incremental"
	k8sstore "github.com/haproxy-haptic/HAPTIC/pkg/k8s/store"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
	"github.com/haproxy-haptic/scriggo/native"
)

type batchCapabilityTrackingReader struct {
	input      incremental.Input
	exactReads atomic.Int64
}

type batchCapabilityBlockingReader struct {
	*batchCapabilityTrackingReader
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

type batchCapabilityBlockingSharedRecorder struct {
	recorder *incrementalRecorder
	started  chan struct{}
	release  chan struct{}
	once     sync.Once
}

type batchCapabilityRetainingHTTPFetcher struct {
	mu        sync.Mutex
	callbacks []func() string
	bodyCalls atomic.Int64
}

type batchCapabilityCanceledNativeLease struct {
	*incrementalBatchReaderLease
	canceled context.Context
}

func (l *batchCapabilityCanceledNativeLease) BeforeIncrementalNativeCall(context.Context) error {
	return l.incrementalBatchReaderLease.BeforeIncrementalNativeCall(l.canceled)
}

func (*batchCapabilityRetainingHTTPFetcher) Pending(string) bool { return false }

func (f *batchCapabilityRetainingHTTPFetcher) Fetch(args ...any) (any, error) {
	f.bodyCalls.Add(1)
	if len(args) == 1 {
		if callback, ok := args[0].(func() string); ok {
			f.mu.Lock()
			f.callbacks = append(f.callbacks, callback)
			f.mu.Unlock()
			return "retained", nil
		}
	}
	return "body", nil
}

func (f *batchCapabilityRetainingHTTPFetcher) callback(index int) func() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.callbacks[index]
}

func (f *batchCapabilityRetainingHTTPFetcher) callbackCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.callbacks)
}

func (r *batchCapabilityBlockingSharedRecorder) Unique(cell, key, value string) {
	r.recorder.recordUnique(cell, key, value)
}

func (r *batchCapabilityBlockingSharedRecorder) Publish(cell, key string, value any) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	r.recorder.publishAfterPreflight(cell, key, "", value, "shared.Publish")
}

func (r *batchCapabilityBlockingSharedRecorder) PublishRanked(cell, key, rank string, value any) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	r.recorder.publishAfterPreflight(cell, key, rank, value, "shared.PublishRanked")
}

func newBatchCapabilityBlockingReader(name string) *batchCapabilityBlockingReader {
	return &batchCapabilityBlockingReader{
		batchCapabilityTrackingReader: newBatchCapabilityTrackingReader(name),
		started:                       make(chan struct{}),
		release:                       make(chan struct{}),
	}
}

func (r *batchCapabilityBlockingReader) ExactInput(key incremental.InputKey) (incremental.Input, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	return r.batchCapabilityTrackingReader.ExactInput(key)
}

func (r *batchCapabilityTrackingReader) Input(key incremental.InputKey) (value []byte, found bool, err error) {
	input, err := r.ExactInput(key)
	return slices.Clone(input.Value), input.Found, err
}

func (r *batchCapabilityTrackingReader) ExactInput(key incremental.InputKey) (incremental.Input, error) {
	r.exactReads.Add(1)
	if key != r.input.Key {
		return incremental.Input{}, errors.New("unexpected incremental input")
	}
	input := r.input
	input.Value = slices.Clone(input.Value)
	return input, nil
}

func (*batchCapabilityTrackingReader) Query(context.Context, incremental.QueryKey) ([]byte, error) {
	return nil, errors.New("unexpected incremental query")
}

func newBatchCapabilityTrackingReader(name string) *batchCapabilityTrackingReader {
	spec := &resourceInputSpec{resourceType: "routes", scope: resourceInputList}
	return &batchCapabilityTrackingReader{input: incremental.Input{
		Key:      resourceInputKey(spec),
		Revision: incremental.NewRevision(name),
		Found:    true,
		Value:    []byte(`[{"generation":"` + name + `"}]`),
	}}
}

type batchCapabilityEnv struct {
	ctx context.Context
	mu  sync.Mutex
	err error
}

func (*batchCapabilityEnv) CallPath() string { return "component" }
func (*batchCapabilityEnv) CallLine() int    { return 1 }
func (e *batchCapabilityEnv) Context() context.Context {
	return e.ctx
}
func (*batchCapabilityEnv) Fatal(value any) { panic(value) }
func (*batchCapabilityEnv) MarkdownConverter() native.Converter {
	return nil
}
func (*batchCapabilityEnv) Print(...any)   {}
func (*batchCapabilityEnv) Println(...any) {}
func (e *batchCapabilityEnv) Stop(err error) {
	e.mu.Lock()
	e.err = errors.Join(e.err, err)
	e.mu.Unlock()
}
func (*batchCapabilityEnv) TypeOf(value reflect.Value) reflect.Type {
	if !value.IsValid() {
		return nil
	}
	return value.Type()
}
func (e *batchCapabilityEnv) stopped() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}

func batchCapabilityResourceCall(
	resources any,
	operation string,
) (func(context.Context) error, error) {
	outer := reflect.ValueOf(resources)
	if outer.Kind() != reflect.Pointer || outer.IsNil() || outer.Elem().Kind() != reflect.Struct {
		return nil, errors.New("resources is not a non-nil struct pointer")
	}
	routes := outer.Elem().FieldByName("Routes")
	if !routes.IsValid() || routes.Kind() != reflect.Pointer || routes.IsNil() ||
		routes.Elem().Kind() != reflect.Struct {
		return nil, errors.New("resources.routes is unavailable")
	}
	callable := routes.Elem().FieldByName(operation)
	if !callable.IsValid() || callable.Kind() != reflect.Func || callable.IsNil() {
		return nil, errors.New("resources.routes." + operation + " is unavailable")
	}
	return func(ctx context.Context) error {
		env := &batchCapabilityEnv{ctx: ctx}
		args := []reflect.Value{reflect.ValueOf(native.Env(env))}
		if callable.Type().NumIn() > 1 {
			args = append(args, reflect.ValueOf([]any{"default", "route"}))
		}
		if callable.Type().IsVariadic() {
			callable.CallSlice(args)
		} else {
			callable.Call(args)
		}
		return env.stopped()
	}, nil
}

func TestIncrementalBatchResourceCapabilityRejectsRetainedCrossGenerationCall(t *testing.T) {
	session := &incrementalRenderSession{}
	batch, authority := prepareBatchCapabilityGenerationBatch(t, session)
	preparedA := prepareBatchCapabilityGenerationItem(t, session, batch, authority, "A")
	preparedB := prepareBatchCapabilityGenerationItem(t, session, batch, authority, "B")
	readerA := preparedA.reader.(*batchCapabilityTrackingReader)
	readerB := preparedB.reader.(*batchCapabilityTrackingReader)
	onTime, err := batchCapabilityResourceCall(preparedA.templateContext["resources"], "List")
	require.NoError(t, err)

	require.NoError(t, preparedA.activate())
	require.NoError(t, onTime(preparedA.ctx))
	preparedA.deactivate()
	require.NoError(t, preparedB.activate())
	for _, operation := range []string{"List", "Fetch", "GetSingle"} {
		call, err := batchCapabilityResourceCall(preparedA.templateContext["resources"], operation)
		require.NoError(t, err)
		assert.Error(t, call(preparedB.ctx), operation)
	}
	preparedB.deactivate()

	assert.Equal(t, int64(1), readerA.exactReads.Load())
	assert.Zero(t, readerB.exactReads.Load(), "a retained A capability must not register a dependency in B")
	_, err = session.finishPreparedComponent(preparedA, "")
	assert.Error(t, err, "a swallowed resource rejection must poison A publication")
	_, err = session.finishPreparedComponent(preparedB, "")
	require.NoError(t, err)
}

func TestIncrementalBatchResourceCapabilityRejectsRetainedCallAfterBatch(t *testing.T) {
	session := &incrementalRenderSession{}
	batch, authority := prepareBatchCapabilityGenerationBatch(t, session)
	prepared := prepareBatchCapabilityGenerationItem(t, session, batch, authority, "A")
	reader := prepared.reader.(*batchCapabilityTrackingReader)
	call, err := batchCapabilityResourceCall(prepared.templateContext["resources"], "List")
	require.NoError(t, err)
	require.NoError(t, prepared.activate())
	prepared.deactivate()

	err = call(prepared.ctx)
	require.Error(t, err)
	assert.Zero(t, reader.exactReads.Load())
	_, err = session.finishPreparedComponent(prepared, "")
	assert.Error(t, err, "an after-batch resource rejection must poison publication")
}

func TestIncrementalBatchResourceCapabilityCannotMutatePublishedResult(t *testing.T) {
	session := &incrementalRenderSession{}
	batch, authority := prepareBatchCapabilityGenerationBatch(t, session)
	prepared := prepareBatchCapabilityGenerationItem(t, session, batch, authority, "A")
	reader := prepared.reader.(*batchCapabilityTrackingReader)
	call, err := batchCapabilityResourceCall(prepared.templateContext["resources"], "List")
	require.NoError(t, err)
	require.NoError(t, prepared.activate())
	prepared.deactivate()
	encoded, err := session.finishPreparedComponent(prepared, "stable")
	require.NoError(t, err)
	published := session.freshResults[prepared.queryKey]
	require.NotNil(t, published)

	assert.Error(t, call(prepared.ctx))
	assert.Zero(t, reader.exactReads.Load())
	assert.Same(t, published, session.freshResults[prepared.queryKey])
	assert.Equal(t, encoded, session.freshResults[prepared.queryKey].encoded)
	assert.JSONEq(t, `{"text":"stable"}`, session.freshResults[prepared.queryKey].encoded)
	assert.Error(t, session.resourceErrors.Err(), "a post-publication rejection must still fail an in-flight render")
}

func TestIncrementalBatchPublicationRequiresRevokedLease(t *testing.T) {
	t.Run("never activated", func(t *testing.T) {
		session := &incrementalRenderSession{}
		batch, authority := prepareBatchCapabilityGenerationBatch(t, session)
		prepared := prepareBatchCapabilityGenerationItem(t, session, batch, authority, "prepared")

		_, err := session.finishPreparedComponent(prepared, "must-not-publish")
		require.ErrorContains(t, err, "is not revoked")
		assert.NotContains(t, session.freshResults, prepared.queryKey)
		assert.NotContains(t, session.httpExecuted, prepared.queryKey)

		require.NoError(t, prepared.activate())
		prepared.deactivate()
		_, err = session.finishPreparedComponent(prepared, "published-after-revoke")
		require.NoError(t, err)
		assert.JSONEq(t, `{"text":"published-after-revoke"}`, session.freshResults[prepared.queryKey].encoded)
	})

	t.Run("active", func(t *testing.T) {
		session := &incrementalRenderSession{}
		batch, authority := prepareBatchCapabilityGenerationBatch(t, session)
		prepared := prepareBatchCapabilityGenerationItem(t, session, batch, authority, "active")
		require.NoError(t, prepared.activate())

		_, err := session.finishPreparedComponent(prepared, "must-not-publish")
		require.ErrorContains(t, err, "is not revoked")
		assert.NotContains(t, session.freshResults, prepared.queryKey)
		assert.NotContains(t, session.httpExecuted, prepared.queryKey)

		prepared.deactivate()
		_, err = session.finishPreparedComponent(prepared, "published-after-revoke")
		require.NoError(t, err)
		assert.JSONEq(t, `{"text":"published-after-revoke"}`, session.freshResults[prepared.queryKey].encoded)
	})

	t.Run("revoking", func(t *testing.T) {
		session := &incrementalRenderSession{}
		batch, authority := prepareBatchCapabilityGenerationBatch(t, session)
		reader := newBatchCapabilityBlockingReader("revoking")
		prepared := prepareBatchCapabilityGenerationComponentWithReader(
			t,
			session,
			batch,
			authority,
			&incrementalComponent{name: "revoking"},
			reader,
		)
		call, err := batchCapabilityResourceCall(prepared.templateContext["resources"], "List")
		require.NoError(t, err)
		require.NoError(t, prepared.activate())
		callDone := make(chan error, 1)
		go func() { callDone <- call(prepared.ctx) }()
		<-reader.started
		revoked := make(chan struct{})
		go func() {
			prepared.deactivate()
			close(revoked)
		}()
		for prepared.lease.state.Load() != incrementalCapabilityLeaseRevoking {
			runtime.Gosched()
		}

		_, err = session.finishPreparedComponent(prepared, "must-not-publish")
		require.ErrorContains(t, err, "is not revoked")
		assert.NotContains(t, session.freshResults, prepared.queryKey)
		assert.NotContains(t, session.httpExecuted, prepared.queryKey)

		close(reader.release)
		require.NoError(t, <-callDone)
		<-revoked
		_, err = session.finishPreparedComponent(prepared, "published-after-drain")
		require.NoError(t, err)
		assert.JSONEq(t, `{"text":"published-after-drain"}`, session.freshResults[prepared.queryKey].encoded)
	})
}

func TestIncrementalBatchNativePreflightRejectsRetainedCallbackBeforeBody(t *testing.T) {
	const source = `{% var _, _ = http.Fetch(func() string {
		var value, _ = http.Fetch("body")
		return tostring(value)
	}) %}`
	newEngine := func(t *testing.T) *templating.ScriggoEngine {
		t.Helper()
		engine, err := templating.New(map[string]string{"component": source}, &templating.Options{
			EntryPoints:            []string{"component"},
			IncrementalEntryPoints: []string{"component"},
		})
		require.NoError(t, err)
		return engine
	}

	t.Run("cross generation and after batch", func(t *testing.T) {
		engine := newEngine(t)
		session := &incrementalRenderSession{}
		batch, authority := prepareBatchCapabilityGenerationBatch(t, session)
		preparedA := prepareBatchCapabilityGenerationItem(t, session, batch, authority, "A")
		preparedB := prepareBatchCapabilityGenerationItem(t, session, batch, authority, "B")
		fetcher := &batchCapabilityRetainingHTTPFetcher{}
		preparedA.templateContext["http"] = fetcher
		preparedB.templateContext["http"] = fetcher
		var crossGenerationPanic any

		outputs, err := engine.RenderIncrementalComponents(t.Context(), "component", []templating.IncrementalComponentBatchItem{
			{
				Context:         preparedA.ctx,
				TemplateContext: preparedA.templateContext,
				Activate:        preparedA.activate,
				Deactivate:      preparedA.deactivate,
			},
			{
				Context:         preparedB.ctx,
				TemplateContext: preparedB.templateContext,
				Activate: func() error {
					if activateErr := preparedB.activate(); activateErr != nil {
						return activateErr
					}
					crossGenerationPanic = recoverBatchCapabilityPanic(func() {
						fetcher.callback(0)()
					})
					return nil
				},
				Deactivate: preparedB.deactivate,
			},
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"", ""}, outputs)
		require.NotNil(t, crossGenerationPanic)
		require.Equal(t, 2, fetcher.callbackCount())
		assert.Equal(t, int64(2), fetcher.bodyCalls.Load(), "rejected retained callback reached the helper body")

		afterBatchPanic := recoverBatchCapabilityPanic(func() {
			fetcher.callback(0)()
		})
		require.NotNil(t, afterBatchPanic)
		assert.Equal(t, int64(2), fetcher.bodyCalls.Load(), "after-batch callback reached the helper body")

		_, err = session.finishPreparedComponent(preparedA, "")
		assert.Error(t, err, "swallowed retained A callback rejection must poison A")
		_, err = session.finishPreparedComponent(preparedB, "")
		require.NoError(t, err, "retained A callback must not poison active B")
	})

	t.Run("cancellation", func(t *testing.T) {
		engine := newEngine(t)
		session := &incrementalRenderSession{}
		batch, authority := prepareBatchCapabilityGenerationBatch(t, session)
		prepared := prepareBatchCapabilityGenerationItem(t, session, batch, authority, "canceled")
		fetcher := &batchCapabilityRetainingHTTPFetcher{}
		prepared.templateContext["http"] = fetcher
		canceled, cancel := context.WithCancel(prepared.ctx)
		cancel()
		prepared.ctx = templating.WithIncrementalExecutionLease(prepared.ctx, &batchCapabilityCanceledNativeLease{
			incrementalBatchReaderLease: prepared.lease,
			canceled:                    canceled,
		})

		_, err := engine.RenderIncrementalComponents(t.Context(), "component", []templating.IncrementalComponentBatchItem{{
			Context:         prepared.ctx,
			TemplateContext: prepared.templateContext,
			Activate:        prepared.activate,
			Deactivate:      prepared.deactivate,
		}})
		require.ErrorContains(t, err, context.Canceled.Error())
		assert.Zero(t, fetcher.callbackCount())
		assert.Zero(t, fetcher.bodyCalls.Load(), "canceled native call reached the helper body")

		_, err = session.finishPreparedComponent(prepared, "")
		require.ErrorContains(t, err, context.Canceled.Error())
		assert.NotContains(t, session.freshResults, prepared.queryKey)
		assert.NotContains(t, session.httpExecuted, prepared.queryKey)
	})
}

func TestIncrementalBatchCapabilityCancellationIsStickyWhenSwallowed(t *testing.T) {
	t.Run("resource", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		session := &incrementalRenderSession{}
		batch, authority := prepareBatchCapabilityGenerationBatchWithContext(t, session, ctx)
		prepared := prepareBatchCapabilityGenerationItem(t, session, batch, authority, "A")
		reader := prepared.reader.(*batchCapabilityTrackingReader)
		call, err := batchCapabilityResourceCall(prepared.templateContext["resources"], "List")
		require.NoError(t, err)
		require.NoError(t, prepared.activate())
		cancel()

		err = call(prepared.ctx)
		assert.ErrorIs(t, err, context.Canceled)
		prepared.deactivate()
		assert.Zero(t, reader.exactReads.Load())
		_, err = session.finishPreparedComponent(prepared, "")
		assert.ErrorIs(t, err, context.Canceled, "swallowed resource cancellation must poison publication")
	})

	t.Run("shared", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		session := &incrementalRenderSession{}
		batch, authority := prepareBatchCapabilityGenerationBatchWithContext(t, session, ctx)
		prepared := prepareBatchCapabilityGenerationItem(t, session, batch, authority, "A")
		shared := prepared.templateContext["shared"].(templating.SharedContributionContext)
		require.NoError(t, prepared.activate())
		cancel()

		recovered := recoverBatchCapabilityPanic(func() {
			shared.Publish("values", "late", map[string]any{"poison": true})
		})
		recoveredErr, ok := recovered.(error)
		require.True(t, ok)
		assert.ErrorIs(t, recoveredErr, context.Canceled)
		prepared.deactivate()
		prepared.recorder.mu.Lock()
		assert.Empty(t, prepared.recorder.published)
		prepared.recorder.mu.Unlock()
		_, err := session.finishPreparedComponent(prepared, "")
		assert.ErrorIs(t, err, context.Canceled, "swallowed shared cancellation must poison publication")
	})
}

func TestIncrementalBatchResourceCapabilityRejectsGoroutineCrossingItemEnd(t *testing.T) {
	session := &incrementalRenderSession{}
	batch, authority := prepareBatchCapabilityGenerationBatch(t, session)
	preparedA := prepareBatchCapabilityGenerationItem(t, session, batch, authority, "A")
	preparedB := prepareBatchCapabilityGenerationItem(t, session, batch, authority, "B")
	readerA := preparedA.reader.(*batchCapabilityTrackingReader)
	readerB := preparedB.reader.(*batchCapabilityTrackingReader)
	callA, err := batchCapabilityResourceCall(preparedA.templateContext["resources"], "List")
	require.NoError(t, err)
	require.NoError(t, preparedA.activate())

	release := make(chan struct{})
	completed := make(chan error, 1)
	go func() {
		<-release
		completed <- callA(preparedB.ctx)
	}()

	preparedA.deactivate()
	require.NoError(t, preparedB.activate())
	close(release)
	err = <-completed
	preparedB.deactivate()

	assert.Error(t, err)
	assert.Zero(t, readerA.exactReads.Load())
	assert.Zero(t, readerB.exactReads.Load(), "a late A goroutine must not register a dependency in B")
	_, err = session.finishPreparedComponent(preparedA, "")
	assert.Error(t, err, "a swallowed goroutine rejection must poison A publication")
	_, err = session.finishPreparedComponent(preparedB, "")
	require.NoError(t, err)
}

func TestIncrementalBatchCapabilityRevocationDrainsAcceptedResourceCall(t *testing.T) {
	session := &incrementalRenderSession{}
	batch, authority := prepareBatchCapabilityGenerationBatch(t, session)
	reader := newBatchCapabilityBlockingReader("A")
	component := &incrementalComponent{name: "A"}
	prepared := prepareBatchCapabilityGenerationComponentWithReader(
		t, session, batch, authority, component, reader,
	)
	call, err := batchCapabilityResourceCall(prepared.templateContext["resources"], "List")
	require.NoError(t, err)
	require.NoError(t, prepared.activate())

	callDone := make(chan error, 1)
	go func() { callDone <- call(prepared.ctx) }()
	<-reader.started
	revoked := make(chan struct{})
	go func() {
		prepared.deactivate()
		close(revoked)
	}()
	awaitScheduled(t, func() bool {
		return prepared.lease.state.Load() != incrementalCapabilityLeaseActive
	}, "capability revocation did not enter revoking state")
	assert.Equal(t, incrementalCapabilityLeaseRevoking, prepared.lease.state.Load())
	select {
	case <-revoked:
		t.Fatal("capability revocation returned before the accepted resource call drained")
	default:
	}
	assert.Error(t, call(prepared.ctx), "a revoking lease must reject a new resource call")
	assert.Zero(t, reader.exactReads.Load(), "the rejected call must not reach the reader")

	close(reader.release)
	require.NoError(t, <-callDone)
	<-revoked
	assert.Equal(t, incrementalCapabilityLeaseRevoked, prepared.lease.state.Load())
	assert.Equal(t, int64(1), reader.exactReads.Load())
	_, err = session.finishPreparedComponent(prepared, "")
	assert.Error(t, err, "a swallowed call during revocation must poison publication")
}

func TestIncrementalBatchCapabilityRevocationDrainsAcceptedSharedCall(t *testing.T) {
	session := &incrementalRenderSession{}
	batch, authority := prepareBatchCapabilityGenerationBatch(t, session)
	prepared := prepareBatchCapabilityGenerationComponent(
		t, session, batch, authority, &incrementalComponent{name: "A", publishValue: true},
	)
	blocking := &batchCapabilityBlockingSharedRecorder{
		recorder: prepared.recorder,
		started:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	shared := templating.NewLeasedSharedContributionContext(prepared.ctx, blocking)
	require.NoError(t, prepared.activate())

	callDone := make(chan any, 1)
	go func() {
		callDone <- recoverBatchCapabilityPanic(func() {
			shared.Publish("values", "accepted", map[string]any{"generation": "A"})
		})
	}()
	<-blocking.started
	revoked := make(chan struct{})
	go func() {
		prepared.deactivate()
		close(revoked)
	}()
	awaitScheduled(t, func() bool {
		return prepared.lease.state.Load() != incrementalCapabilityLeaseActive
	}, "capability revocation did not enter revoking state")
	assert.Equal(t, incrementalCapabilityLeaseRevoking, prepared.lease.state.Load())
	select {
	case <-revoked:
		t.Fatal("capability revocation returned before the accepted shared call drained")
	default:
	}

	close(blocking.release)
	assert.Nil(t, <-callDone, "an accepted shared call must complete while its lease drains")
	<-revoked
	assert.NoError(t, prepared.lease.err())
	prepared.recorder.mu.Lock()
	assert.Len(t, prepared.recorder.published, 1)
	prepared.recorder.mu.Unlock()
	_, err := session.finishPreparedComponent(prepared, "")
	require.NoError(t, err, "an accepted shared call must not cause an unwarranted cache rejection")
}

func TestIncrementalBatchCapabilityPanicReleasesAcceptedSharedCall(t *testing.T) {
	session := &incrementalRenderSession{}
	batch, authority := prepareBatchCapabilityGenerationBatch(t, session)
	prepared := prepareBatchCapabilityGenerationComponent(
		t, session, batch, authority, &incrementalComponent{name: "A", publishValue: true},
	)
	shared := prepared.templateContext["shared"].(templating.SharedContributionContext)
	require.NoError(t, prepared.activate())

	recovered := recoverBatchCapabilityPanic(func() {
		shared.Publish("values", "invalid", make(chan int))
	})
	require.NotNil(t, recovered)
	assert.NoError(t, prepared.lease.err())
	assert.NotPanics(t, func() {
		shared.Publish("values", "accepted", map[string]any{"generation": "A"})
	})
	prepared.deactivate()

	prepared.recorder.mu.Lock()
	assert.Len(t, prepared.recorder.published, 1)
	prepared.recorder.mu.Unlock()
	_, err := session.finishPreparedComponent(prepared, "")
	require.NoError(t, err, "a recovered argument panic must release the accepted call gate")
}

func TestIncrementalBatchResourceCapabilitiesAreGenerationIsolatedInParallel(t *testing.T) {
	session := &incrementalRenderSession{}
	batch, authority := prepareBatchCapabilityGenerationBatch(t, session)
	readerA := newBatchCapabilityTrackingReader("shared-snapshot")
	readerB := newBatchCapabilityTrackingReader("shared-snapshot")
	preparedA := prepareBatchCapabilityGenerationComponentWithReader(
		t, session, batch, authority, &incrementalComponent{name: "A"}, readerA,
	)
	preparedB := prepareBatchCapabilityGenerationComponentWithReader(
		t, session, batch, authority, &incrementalComponent{name: "B"}, readerB,
	)
	callA, err := batchCapabilityResourceCall(preparedA.templateContext["resources"], "List")
	require.NoError(t, err)
	callB, err := batchCapabilityResourceCall(preparedB.templateContext["resources"], "List")
	require.NoError(t, err)
	require.NoError(t, preparedA.activate())
	require.NoError(t, preparedB.activate())

	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		results <- callA(preparedA.ctx)
	}()
	go func() {
		<-start
		results <- callB(preparedB.ctx)
	}()
	close(start)
	require.NoError(t, <-results)
	require.NoError(t, <-results)
	preparedA.deactivate()
	preparedB.deactivate()

	assert.Equal(t, int64(1), readerA.exactReads.Load())
	assert.Equal(t, int64(1), readerB.exactReads.Load())
	_, err = session.finishPreparedComponent(preparedA, "")
	require.NoError(t, err)
	_, err = session.finishPreparedComponent(preparedB, "")
	require.NoError(t, err)
}

func TestIncrementalBatchControllerCapabilityRejectsRetainedCrossGenerationCall(t *testing.T) {
	session := &incrementalRenderSession{}
	batch, authority := prepareBatchCapabilityGenerationBatch(t, session)
	preparedA := prepareBatchCapabilityGenerationItem(t, session, batch, authority, "A")
	preparedB := prepareBatchCapabilityGenerationItem(t, session, batch, authority, "B")
	readerA := preparedA.reader.(*batchCapabilityTrackingReader)
	readerB := preparedB.reader.(*batchCapabilityTrackingReader)
	controllerA := preparedA.templateContext["controller"].(map[string]templating.ResourceStore)["routes"]

	require.NoError(t, preparedA.activate())
	assert.Len(t, controllerA.List(), 1, "the controller capability must work during A")
	preparedA.deactivate()
	require.NoError(t, preparedB.activate())
	assert.Empty(t, controllerA.List())
	assert.Empty(t, controllerA.Fetch("default", "route"))
	assert.Nil(t, controllerA.GetSingle("default", "route"))
	preparedB.deactivate()

	assert.Equal(t, int64(1), readerA.exactReads.Load())
	assert.Zero(t, readerB.exactReads.Load(), "a retained controller capability must not register a dependency in B")
	_, err := session.finishPreparedComponent(preparedA, "")
	assert.Error(t, err, "swallowed controller rejections must poison A publication")
	_, err = session.finishPreparedComponent(preparedB, "")
	require.NoError(t, err)
}

func TestIncrementalBatchSharedCapabilityRejectsSwallowedCrossGenerationEffect(t *testing.T) {
	session := &incrementalRenderSession{}
	batch, authority := prepareBatchCapabilityGenerationBatch(t, session)
	preparedA := prepareBatchCapabilityGenerationItem(t, session, batch, authority, "A")
	preparedB := prepareBatchCapabilityGenerationItem(t, session, batch, authority, "B")
	sharedA := preparedA.templateContext["shared"].(templating.SharedContributionContext)

	require.NoError(t, preparedA.activate())
	sharedA.Publish("values", "on-time", map[string]any{"generation": "A"})
	preparedA.deactivate()
	require.NoError(t, preparedB.activate())

	_ = recoverBatchCapabilityPanic(func() {
		sharedA.Publish("values", "late", map[string]any{"generation": "poison"})
	})
	preparedB.deactivate()

	preparedA.recorder.mu.Lock()
	assert.Len(t, preparedA.recorder.published, 1, "a rejected late publish must not reach A's effects")
	preparedA.recorder.mu.Unlock()
	_, err := session.finishPreparedComponent(preparedA, "")
	assert.Error(t, err, "a swallowed call on a revoked capability must poison result publication")
	resultB, err := preparedB.recorder.result("")
	require.NoError(t, err)
	assert.Empty(t, resultB.Published, "a retained A capability must not add an effect to B")
	_, err = session.finishPreparedComponent(preparedB, "")
	require.NoError(t, err)
}

func TestIncrementalBatchSharedCapabilityPreflightsBeforeInvalidValue(t *testing.T) {
	for _, test := range []struct {
		name string
		call func(templating.SharedContributionContext)
	}{
		{
			name: "publish",
			call: func(shared templating.SharedContributionContext) {
				shared.Publish("values", "late", make(chan int))
			},
		},
		{
			name: "publish ranked",
			call: func(shared templating.SharedContributionContext) {
				shared.PublishRanked("values", "late", "rank", make(chan int))
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			session := &incrementalRenderSession{}
			batch, authority := prepareBatchCapabilityGenerationBatch(t, session)
			preparedA := prepareBatchCapabilityGenerationItem(t, session, batch, authority, "A")
			preparedB := prepareBatchCapabilityGenerationItem(t, session, batch, authority, "B")
			sharedA := preparedA.templateContext["shared"].(templating.SharedContributionContext)

			require.NoError(t, preparedA.activate())
			preparedA.deactivate()
			require.NoError(t, preparedB.activate())
			recovered := recoverBatchCapabilityPanic(func() { test.call(sharedA) })
			preparedB.deactivate()

			require.NotNil(t, recovered)
			assert.Contains(t, recovered.(error).Error(), "inactive incremental component capability generation")
			preparedA.recorder.mu.Lock()
			assert.Empty(t, preparedA.recorder.published)
			preparedA.recorder.mu.Unlock()
			_, err := session.finishPreparedComponent(preparedA, "")
			assert.Error(t, err, "swallowed preflight rejection must poison A publication")
			_, err = session.finishPreparedComponent(preparedB, "")
			require.NoError(t, err)
		})
	}
}

func TestIncrementalBatchSelectorCapabilityRejectsSwallowedCrossGenerationRead(t *testing.T) {
	session := &incrementalRenderSession{}
	batch, authority := prepareBatchCapabilityGenerationBatch(t, session)
	preparedA := prepareBatchCapabilityGenerationItem(t, session, batch, authority, "A")
	preparedB := prepareBatchCapabilityGenerationItem(t, session, batch, authority, "B")
	readerA := preparedA.reader.(*batchCapabilityTrackingReader)
	readerB := preparedB.reader.(*batchCapabilityTrackingReader)
	sharedA := preparedA.templateContext["shared"].(templating.SharedContributionContext)

	require.NoError(t, preparedA.activate())
	preparedA.deactivate()
	require.NoError(t, preparedB.activate())
	for _, call := range []func(){
		func() { _, _ = sharedA.Select("group", "cell", "key") },
		func() { _ = sharedA.SelectValues("group", "cell") },
		func() { _ = sharedA.Count("group", "cell") },
	} {
		assert.NotNil(t, recoverBatchCapabilityPanic(call))
	}
	preparedB.deactivate()

	assert.Zero(t, readerA.exactReads.Load())
	assert.Zero(t, readerB.exactReads.Load(), "a retained selector must not register a dependency in B")
	_, err := session.finishPreparedComponent(preparedA, "")
	assert.Error(t, err, "swallowed selector rejections must poison A publication")
	_, err = session.finishPreparedComponent(preparedB, "")
	require.NoError(t, err)
}

func TestIncrementalBatchPlanCapabilityRejectsSwallowedCrossGenerationEffect(t *testing.T) {
	session := &incrementalRenderSession{}
	batch, authority := prepareBatchCapabilityGenerationBatch(t, session)
	componentA := incrementalComponent{name: "A", backendPlan: true}
	componentB := incrementalComponent{name: "B", backendPlan: true}
	preparedA := prepareBatchCapabilityGenerationComponent(t, session, batch, authority, &componentA)
	preparedB := prepareBatchCapabilityGenerationComponent(t, session, batch, authority, &componentB)
	planA := preparedA.templateContext["planRegistry"].(templating.IncrementalBackendPlanRegistrar)

	require.NoError(t, preparedA.activate())
	_, err := planA.Profile(map[string]any{"mode": "http"})
	require.NoError(t, err)
	preparedA.deactivate()
	require.NoError(t, preparedB.activate())

	_, _ = planA.Profile(map[string]any{"mode": "tcp"})
	preparedB.deactivate()

	preparedA.recorder.plan.mu.Lock()
	assert.Len(t, preparedA.recorder.plan.calls, 1, "a rejected late plan call must not reach A's effects")
	preparedA.recorder.plan.mu.Unlock()
	_, err = session.finishPreparedComponent(preparedA, "")
	assert.Error(t, err, "a swallowed call on a revoked plan capability must poison result publication")
	resultB, err := preparedB.recorder.result("")
	require.NoError(t, err)
	assert.Empty(t, resultB.BackendPlan, "a retained A capability must not add a plan effect to B")
	_, err = session.finishPreparedComponent(preparedB, "")
	require.NoError(t, err)
}

func TestIncrementalBatchHTTPCapabilityRejectsSwallowedCrossGenerationEffect(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte("poison"))
	}))
	t.Cleanup(server.Close)
	session := newBatchCapabilityHTTPSession(t)
	batch, authority := prepareBatchCapabilityGenerationBatch(t, session)
	preparedA := prepareBatchCapabilityGenerationItem(t, session, batch, authority, "A")
	preparedB := prepareBatchCapabilityGenerationItem(t, session, batch, authority, "B")
	httpA := preparedA.templateContext["http"].(templating.HTTPFetcher)

	require.NoError(t, preparedA.activate())
	preparedA.deactivate()
	require.NoError(t, preparedB.activate())

	_, _ = httpA.Fetch(server.URL, map[string]any{"critical": true})
	preparedB.deactivate()

	assert.Zero(t, requests.Load(), "a revoked HTTP capability must reject before network I/O")
	assert.Empty(t, preparedA.httpFetcher.result())
	assert.Empty(t, preparedB.httpFetcher.result(), "a retained A capability must not add an HTTP effect to B")
	assert.Empty(t, session.state.httpSpecs, "a revoked HTTP capability must not mutate the shared HTTP registry")
	_, err := session.finishPreparedComponent(preparedA, "")
	assert.Error(t, err, "a swallowed call on a revoked HTTP capability must poison result publication")
	_, err = session.finishPreparedComponent(preparedB, "")
	require.NoError(t, err)
}

func TestIncrementalBatchHTTPCapabilitySupportsConcurrentActiveCalls(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte("content"))
	}))
	t.Cleanup(server.Close)
	session := newBatchCapabilityHTTPSession(t)
	batch, authority := prepareBatchCapabilityGenerationBatch(t, session)
	prepared := prepareBatchCapabilityGenerationItem(t, session, batch, authority, "A")
	httpCapability := prepared.templateContext["http"].(templating.HTTPFetcher)
	require.NoError(t, prepared.activate())

	const callers = 32
	type fetchResult struct {
		value any
		err   error
	}
	start := make(chan struct{})
	results := make(chan fetchResult, callers)
	for range callers {
		go func() {
			<-start
			value, err := httpCapability.Fetch(server.URL, map[string]any{"critical": true})
			results <- fetchResult{value: value, err: err}
		}()
	}
	close(start)
	for range callers {
		result := <-results
		require.NoError(t, result.err)
		assert.Equal(t, "content", result.value)
	}
	prepared.deactivate()

	assert.Equal(t, int64(1), requests.Load(), "the read-only wrapper must coalesce concurrent network reads")
	assert.Len(t, prepared.httpFetcher.result(), 1)
	assert.Len(t, session.state.httpSpecs, 1)
	_, err := session.finishPreparedComponent(prepared, "")
	require.NoError(t, err)
}

type revokedBatchCapabilityProbe struct {
	resource     func(context.Context) error
	shared       templating.SharedContributionContext
	plan         templating.IncrementalBackendPlanRegistrar
	http         templating.HTTPFetcher
	effectEngine templating.IncrementalComponentExecutor
	preparedA    *preparedIncrementalComponent
	preparedB    *preparedIncrementalComponent
	serverURL    string
}

func (p *revokedBatchCapabilityProbe) countUnexpectedSuccesses() int64 {
	var successes int64
	if p.resource(p.preparedB.ctx) == nil {
		successes++
	}
	if recoverBatchCapabilityPanic(func() {
		p.shared.Publish("values", "late", map[string]any{"poison": true})
	}) == nil {
		successes++
	}
	if recoverBatchCapabilityPanic(func() {
		_, _ = p.shared.Select("group", "cell", "key")
	}) == nil {
		successes++
	}
	if _, err := p.plan.Profile(map[string]any{"mode": "http"}); err == nil {
		successes++
	}
	if _, err := p.http.Fetch(p.serverURL, map[string]any{"critical": true}); err == nil {
		successes++
	}
	if _, err := p.effectEngine.RenderIncrementalComponent(
		p.preparedA.ctx, "event", p.preparedA.templateContext,
	); err == nil {
		successes++
	}
	if _, err := p.effectEngine.RenderIncrementalComponent(
		p.preparedA.ctx, "status", p.preparedA.templateContext,
	); err == nil {
		successes++
	}
	if err := p.preparedA.recorder.RecordEvent(
		"default", "route", "example.test/v1", "Route", "Warning", "Late", "poison",
	); err == nil {
		successes++
	}
	if err := p.preparedA.recorder.RecordStatusPatch(
		"default", "route", "example.test/v1", "Route",
		"uid-route", "rv-route",
		map[string]map[string]any{"rendered": {"value": "poison"}}, "component", 1,
	); err == nil {
		successes++
	}
	return successes
}

func TestIncrementalBatchRevokedCapabilitiesRejectConcurrentSwallowedCalls(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte("poison"))
	}))
	t.Cleanup(server.Close)
	session := newBatchCapabilityHTTPSession(t)
	batch, authority := prepareBatchCapabilityGenerationBatch(t, session)
	effectEngine, err := templating.New(map[string]string{
		"event":  `{{ recordEvent(renderSubject, "Late", "poison") }}`,
		"status": `{{ statusPatch(renderSubject, map[string]any{"rendered": map[string]any{"value": "poison"}}) }}`,
	}, &templating.Options{
		EntryPoints:            []string{"event", "status"},
		IncrementalEntryPoints: []string{"event", "status"},
	})
	require.NoError(t, err)
	componentA := incrementalComponent{
		name: "A", backendPlan: true, recordEvent: true, statusPatch: true,
	}
	preparedA := prepareBatchCapabilityGenerationComponent(t, session, batch, authority, &componentA)
	preparedB := prepareBatchCapabilityGenerationItem(t, session, batch, authority, "B")
	readerA := preparedA.reader.(*batchCapabilityTrackingReader)
	readerB := preparedB.reader.(*batchCapabilityTrackingReader)
	resourceA, err := batchCapabilityResourceCall(preparedA.templateContext["resources"], "List")
	require.NoError(t, err)
	require.NoError(t, preparedA.activate())
	preparedA.deactivate()
	require.NoError(t, preparedB.activate())

	const callers = 16
	var unexpectedSuccesses atomic.Int64
	start := make(chan struct{})
	done := make(chan struct{}, callers)
	probe := &revokedBatchCapabilityProbe{
		resource:     resourceA,
		shared:       preparedA.templateContext["shared"].(templating.SharedContributionContext),
		plan:         preparedA.templateContext["planRegistry"].(templating.IncrementalBackendPlanRegistrar),
		http:         preparedA.templateContext["http"].(templating.HTTPFetcher),
		effectEngine: effectEngine,
		preparedA:    preparedA,
		preparedB:    preparedB,
		serverURL:    server.URL,
	}
	for range callers {
		go func() {
			<-start
			unexpectedSuccesses.Add(probe.countUnexpectedSuccesses())
			done <- struct{}{}
		}()
	}
	close(start)
	for range callers {
		<-done
	}
	preparedB.deactivate()

	assert.Zero(t, unexpectedSuccesses.Load())
	assert.Zero(t, requests.Load())
	assert.Zero(t, readerA.exactReads.Load())
	assert.Zero(t, readerB.exactReads.Load())
	assert.Empty(t, preparedA.httpFetcher.result())
	preparedA.recorder.mu.Lock()
	assert.Empty(t, preparedA.recorder.published)
	assert.Nil(t, preparedA.recorder.events)
	assert.Empty(t, preparedA.recorder.patches)
	preparedA.recorder.mu.Unlock()
	preparedA.recorder.plan.mu.Lock()
	assert.Empty(t, preparedA.recorder.plan.calls)
	preparedA.recorder.plan.mu.Unlock()
	_, err = session.finishPreparedComponent(preparedA, "")
	assert.Error(t, err, "concurrent swallowed rejections must poison A publication")
	_, err = session.finishPreparedComponent(preparedB, "")
	require.NoError(t, err)
}

func TestIncrementalBatchEventCapabilityRejectsRetainedContextEffect(t *testing.T) {
	engine, err := templating.New(map[string]string{
		"component": `{{ recordEvent(renderSubject, "Late", "poison") }}`,
	}, &templating.Options{
		EntryPoints:            []string{"component"},
		IncrementalEntryPoints: []string{"component"},
	})
	require.NoError(t, err)
	session := &incrementalRenderSession{}
	batch, authority := prepareBatchCapabilityGenerationBatch(t, session)
	componentA := incrementalComponent{name: "component", recordEvent: true}
	componentB := incrementalComponent{name: "B", recordEvent: true}
	preparedA := prepareBatchCapabilityGenerationComponent(t, session, batch, authority, &componentA)
	preparedB := prepareBatchCapabilityGenerationComponent(t, session, batch, authority, &componentB)

	require.NoError(t, preparedA.activate())
	preparedA.deactivate()
	require.NoError(t, preparedB.activate())

	_, _ = engine.RenderIncrementalComponent(preparedA.ctx, "component", preparedA.templateContext)
	preparedB.deactivate()

	preparedA.recorder.mu.Lock()
	assert.Nil(t, preparedA.recorder.events, "a rejected late event must not reach A's effects")
	preparedA.recorder.mu.Unlock()
	_, err = session.finishPreparedComponent(preparedA, "")
	assert.Error(t, err, "a retained context must not hide a revoked event-capability call")
	resultB, err := preparedB.recorder.result("")
	require.NoError(t, err)
	assert.Empty(t, resultB.Events, "a retained A context must not add an event effect to B")
	_, err = session.finishPreparedComponent(preparedB, "")
	require.NoError(t, err)
}

func TestIncrementalBatchEventCapabilityPreflightsBeforeInvalidResource(t *testing.T) {
	engine, err := templating.New(map[string]string{
		"component": `{{ recordEvent(map[string]any{}, "Late", "poison") }}`,
	}, &templating.Options{
		EntryPoints:            []string{"component"},
		IncrementalEntryPoints: []string{"component"},
	})
	require.NoError(t, err)
	session := &incrementalRenderSession{}
	batch, authority := prepareBatchCapabilityGenerationBatch(t, session)
	componentA := incrementalComponent{name: "component", recordEvent: true}
	preparedA := prepareBatchCapabilityGenerationComponent(t, session, batch, authority, &componentA)
	preparedB := prepareBatchCapabilityGenerationItem(t, session, batch, authority, "B")

	require.NoError(t, preparedA.activate())
	preparedA.deactivate()
	require.NoError(t, preparedB.activate())
	_, err = engine.RenderIncrementalComponent(preparedA.ctx, "component", preparedA.templateContext)
	preparedB.deactivate()

	assert.ErrorContains(t, err, "inactive incremental component capability generation")
	preparedA.recorder.mu.Lock()
	assert.Nil(t, preparedA.recorder.events)
	preparedA.recorder.mu.Unlock()
	_, err = session.finishPreparedComponent(preparedA, "")
	assert.Error(t, err, "swallowed event preflight rejection must poison A publication")
	_, err = session.finishPreparedComponent(preparedB, "")
	require.NoError(t, err)
}

func TestIncrementalBatchStatusCapabilityRejectsRetainedContextEffect(t *testing.T) {
	engine, err := templating.New(map[string]string{
		"component": `{{ statusPatch(renderSubject, ` +
			`map[string]any{"rendered": map[string]any{"value": "poison"}}) }}`,
	}, &templating.Options{
		EntryPoints:            []string{"component"},
		IncrementalEntryPoints: []string{"component"},
	})
	require.NoError(t, err)
	session := &incrementalRenderSession{}
	batch, authority := prepareBatchCapabilityGenerationBatch(t, session)
	componentA := incrementalComponent{name: "component", statusPatch: true}
	componentB := incrementalComponent{name: "B", statusPatch: true}
	preparedA := prepareBatchCapabilityGenerationComponent(t, session, batch, authority, &componentA)
	preparedB := prepareBatchCapabilityGenerationComponent(t, session, batch, authority, &componentB)

	require.NoError(t, preparedA.activate())
	preparedA.deactivate()
	require.NoError(t, preparedB.activate())

	_, _ = engine.RenderIncrementalComponent(preparedA.ctx, "component", preparedA.templateContext)
	preparedB.deactivate()

	preparedA.recorder.mu.Lock()
	assert.Empty(t, preparedA.recorder.patches, "a rejected late status patch must not reach A's effects")
	preparedA.recorder.mu.Unlock()
	_, err = session.finishPreparedComponent(preparedA, "")
	assert.Error(t, err, "a retained context must not hide a revoked status-capability call")
	resultB, err := preparedB.recorder.result("")
	require.NoError(t, err)
	assert.Empty(t, resultB.StatusPatches, "a retained A context must not add a status effect to B")
	_, err = session.finishPreparedComponent(preparedB, "")
	require.NoError(t, err)
}

func TestIncrementalBatchStatusCapabilityPreflightsBeforeInvalidVariants(t *testing.T) {
	engine, err := templating.New(map[string]string{
		"component": `{{ statusPatch(renderSubject, ` +
			`map[string]any{"rendered": "invalid"}) }}`,
	}, &templating.Options{
		EntryPoints:            []string{"component"},
		IncrementalEntryPoints: []string{"component"},
	})
	require.NoError(t, err)
	session := &incrementalRenderSession{}
	batch, authority := prepareBatchCapabilityGenerationBatch(t, session)
	componentA := incrementalComponent{name: "component", statusPatch: true}
	preparedA := prepareBatchCapabilityGenerationComponent(t, session, batch, authority, &componentA)
	preparedB := prepareBatchCapabilityGenerationItem(t, session, batch, authority, "B")

	require.NoError(t, preparedA.activate())
	preparedA.deactivate()
	require.NoError(t, preparedB.activate())
	_, err = engine.RenderIncrementalComponent(preparedA.ctx, "component", preparedA.templateContext)
	preparedB.deactivate()

	assert.ErrorContains(t, err, "inactive incremental component capability generation")
	preparedA.recorder.mu.Lock()
	assert.Empty(t, preparedA.recorder.patches)
	preparedA.recorder.mu.Unlock()
	_, err = session.finishPreparedComponent(preparedA, "")
	assert.Error(t, err, "swallowed status preflight rejection must poison A publication")
	_, err = session.finishPreparedComponent(preparedB, "")
	require.NoError(t, err)
}

func TestIncrementalBatchDeriveCapabilityPreflightsBeforeInvalidPath(t *testing.T) {
	engine, err := templating.New(map[string]string{
		"component": `{{ deriveResource("routes", item, "not[", "poison") | toJSON() }}`,
	}, &templating.Options{
		EntryPoints:            []string{"component"},
		IncrementalEntryPoints: []string{"component"},
	})
	require.NoError(t, err)
	session := &incrementalRenderSession{}
	batch, authority := prepareBatchCapabilityGenerationBatch(t, session)
	componentA := incrementalComponent{name: "component", deriveResource: true}
	preparedA := prepareBatchCapabilityGenerationComponentWithReader(
		t, session, nil, authority, &componentA, newBatchCapabilityTrackingReader("component"),
	)
	preparedB := prepareBatchCapabilityGenerationItem(t, session, batch, authority, "B")

	require.NoError(t, preparedA.activate())
	preparedA.deactivate()
	require.NoError(t, preparedB.activate())
	_, err = engine.RenderIncrementalComponent(preparedA.ctx, "component", preparedA.templateContext)
	preparedB.deactivate()

	assert.ErrorContains(t, err, "inactive incremental component capability generation")
	assert.Empty(t, preparedA.recorder.deriver.freeze())
	_, err = session.finishPreparedComponent(preparedA, "")
	assert.Error(t, err, "swallowed derivation preflight rejection must poison A publication")
	_, err = session.finishPreparedComponent(preparedB, "")
	require.NoError(t, err)
}

func prepareBatchCapabilityGenerationItem(
	t *testing.T,
	session *incrementalRenderSession,
	batch *incrementalBatchCapabilities,
	authority *incrementalCapabilityAuthority,
	name string,
) *preparedIncrementalComponent {
	t.Helper()
	component := &incrementalComponent{name: name}
	return prepareBatchCapabilityGenerationComponent(t, session, batch, authority, component)
}

func prepareBatchCapabilityGenerationComponent(
	t *testing.T,
	session *incrementalRenderSession,
	batch *incrementalBatchCapabilities,
	authority *incrementalCapabilityAuthority,
	component *incrementalComponent,
) *preparedIncrementalComponent {
	t.Helper()
	return prepareBatchCapabilityGenerationComponentWithReader(
		t,
		session,
		batch,
		authority,
		component,
		newBatchCapabilityTrackingReader(component.name),
	)
}

func prepareBatchCapabilityGenerationComponentWithReader(
	t *testing.T,
	session *incrementalRenderSession,
	batch *incrementalBatchCapabilities,
	authority *incrementalCapabilityAuthority,
	component *incrementalComponent,
	reader incremental.Reader,
) *preparedIncrementalComponent {
	t.Helper()
	name := component.name
	item := map[string]any{
		"apiVersion": "example.test/v1",
		"kind":       "Route",
		"metadata": map[string]any{
			"namespace": "default",
			"name":      name,
		},
	}
	itemBytes, err := encodeResourceValue(item)
	require.NoError(t, err)
	props := map[string]any{}
	renderSubject := map[string]any{
		"mode":       "reconcile",
		"apiVersion": "example.test/v1",
		"kind":       "Route",
		"metadata": map[string]any{
			"namespace": "default",
			"name":      name,
		},
	}
	prepared := &preparedIncrementalComponent{
		queryKey:           incremental.NewQueryKey(name),
		component:          component,
		reader:             reader,
		source:             "routes",
		namespace:          "default",
		name:               name,
		itemBytes:          itemBytes,
		item:               item,
		itemCertificate:    templating.CertifyIncrementalImmutableInputs(item),
		props:              props,
		propsCertificate:   templating.CertifyIncrementalImmutableInputs(props),
		renderSubject:      renderSubject,
		subjectCertificate: templating.CertifyIncrementalImmutableInputs(renderSubject),
	}
	ctx := t.Context()
	if batch != nil {
		ctx = batch.ctx
	}
	require.NoError(t, session.prepareComponentRender(ctx, prepared, batch, authority))
	return prepared
}

func prepareBatchCapabilityGenerationBatch(
	t *testing.T,
	session *incrementalRenderSession,
) (*incrementalBatchCapabilities, *incrementalCapabilityAuthority) {
	t.Helper()
	return prepareBatchCapabilityGenerationBatchWithContext(t, session, t.Context())
}

func prepareBatchCapabilityGenerationBatchWithContext(
	t *testing.T,
	session *incrementalRenderSession,
	ctx context.Context,
) (*incrementalBatchCapabilities, *incrementalCapabilityAuthority) {
	t.Helper()
	if session.state == nil {
		session.state = &incrementalRenderState{}
	}
	if session.state.config == nil {
		session.state.config = &config.Config{WatchedResources: map[string]config.WatchedResource{
			"routes": {
				APIVersion: "example.test/v1",
				Resources:  "routes",
				IndexBy:    []string{"metadata.namespace", "metadata.name"},
			},
		}}
	}
	if session.stores == nil {
		session.stores = map[string]stores.Store{"routes": k8sstore.NewMemoryStore(2)}
	}
	if session.baseContext == nil {
		session.baseContext = map[string]any{
			"controller": map[string]templating.ResourceStore{
				"routes": &rendercontext.StoreWrapper{
					Store:        session.stores["routes"],
					ResourceType: "routes",
					Logger:       slog.Default(),
					IndexBy:      []string{"metadata.namespace", "metadata.name"},
				},
			},
		}
	}
	if session.resourceErrors == nil {
		session.resourceErrors = rendercontext.NewResourceErrorCollector()
	}
	if session.loggerContext.logger == nil {
		session.loggerContext = incrementalLoggerContext{
			logger:             slog.Default(),
			typedResourceTypes: map[string]reflect.Type{},
		}
	}
	if session.freshResults == nil {
		session.freshResults = map[incremental.QueryKey]*authenticatedFreshComponentResult{}
	}
	if session.httpExecuted == nil {
		session.httpExecuted = map[incremental.QueryKey][]incrementalHTTPEffect{}
	}
	if session.resourceProofs == nil {
		session.resourceProofs = map[incremental.InputKey]incremental.Input{}
	}
	authority := newIncrementalCapabilityAuthority(session.resourceErrors)
	return session.prepareBatchCapabilities(ctx, authority), authority
}

func recoverBatchCapabilityPanic(call func()) (recovered any) {
	defer func() { recovered = recover() }()
	call()
	return nil
}

func newBatchCapabilityHTTPSession(t *testing.T) *incrementalRenderSession {
	t.Helper()
	bus, logger := testutil.NewTestBusAndLogger()
	httpComponent := controllerhttpstore.New(bus, logger, -time.Hour)
	return &incrementalRenderSession{
		state:            newHTTPRegistryTestState(),
		httpComponent:    httpComponent,
		httpWrapper:      controllerhttpstore.NewHTTPStoreWrapper(t.Context(), httpComponent, logger, nil, controllerhttpstore.SourceModeReadOnly),
		cold:             true,
		cachePublishable: true,
		httpKnown:        map[httpInputIdentity]httpInputSpec{},
		httpRetained:     map[uint64]struct{}{},
		freshResults:     map[incremental.QueryKey]*authenticatedFreshComponentResult{},
		httpExecuted:     map[incremental.QueryKey][]incrementalHTTPEffect{},
	}
}
