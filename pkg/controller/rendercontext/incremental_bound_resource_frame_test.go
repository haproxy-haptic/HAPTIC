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
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores/storetest"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
	"github.com/haproxy-haptic/scriggo/native"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type boundResourceFrameInvocationKey struct{}

type boundResourceFrameViewCore struct {
	item         any
	beginErr     error
	readErr      error
	legacyBegins atomic.Int64
	boundBegins  atomic.Int64
	releases     atomic.Int64
	reads        atomic.Int64
}

type boundResourceFrameView struct {
	*boundResourceFrameViewCore
	expectedLease templating.IncrementalResourceInvocationLease
}

type legacyResourceFrameView struct {
	*boundResourceFrameViewCore
}

func (v *boundResourceFrameViewCore) List(string, stores.Store) ([]any, error) {
	return nil, errors.New("unscoped resource list reached the snapshot view")
}

func (v *boundResourceFrameViewCore) Get(string, stores.Store, ...string) ([]any, error) {
	return nil, errors.New("unscoped resource lookup reached the snapshot view")
}

func (v *boundResourceFrameViewCore) ListContext(
	ctx context.Context,
	_ string,
	_ stores.Store,
) ([]any, error) {
	return v.read(ctx)
}

func (v *boundResourceFrameViewCore) GetContext(
	ctx context.Context,
	_ string,
	_ stores.Store,
	_ ...string,
) ([]any, error) {
	return v.read(ctx)
}

func (v *boundResourceFrameViewCore) read(ctx context.Context) ([]any, error) {
	if ctx == nil || ctx.Value(boundResourceFrameInvocationKey{}) != v {
		return nil, errors.New("resource read escaped its invocation")
	}
	v.reads.Add(1)
	if v.readErr != nil {
		return nil, v.readErr
	}
	return []any{v.item}, nil
}

func (*boundResourceFrameViewCore) MemoizeStoreMaterialization() bool {
	return false
}

func (*boundResourceFrameViewCore) MemoizeStoreItems() bool {
	return true
}

func (*boundResourceFrameViewCore) PreserveStoreValues() bool {
	return true
}

func (v *boundResourceFrameViewCore) BeginStoreInvocation(
	ctx context.Context,
) (context.Context, func(), error) {
	v.legacyBegins.Add(1)
	return v.begin(ctx)
}

func (v *boundResourceFrameView) BeginBoundStoreInvocation(
	ctx context.Context,
	lease templating.IncrementalResourceInvocationLease,
) (context.Context, func(), error) {
	if lease != v.expectedLease {
		return nil, nil, errors.New("bound resource invocation used the wrong lease")
	}
	v.boundBegins.Add(1)
	return v.begin(ctx)
}

func (v *boundResourceFrameViewCore) begin(
	ctx context.Context,
) (context.Context, func(), error) {
	if v.beginErr != nil {
		return nil, nil, v.beginErr
	}
	if ctx == nil {
		return nil, nil, errors.New("resource invocation has no context")
	}
	var once sync.Once
	return context.WithValue(ctx, boundResourceFrameInvocationKey{}, v), func() {
		once.Do(func() { v.releases.Add(1) })
	}, nil
}

type boundResourceFrameLease struct {
	expectedCtx context.Context
	err         error
	validations atomic.Int64
}

func (l *boundResourceFrameLease) ValidateIncrementalResourceInvocation(ctx context.Context) error {
	l.validations.Add(1)
	if l.err != nil {
		return l.err
	}
	if ctx != l.expectedCtx {
		return errors.New("resource invocation used the wrong context")
	}
	return nil
}

type boundResourceFrameEnv struct {
	ctx context.Context
	mu  sync.Mutex
	err error
}

func (*boundResourceFrameEnv) CallPath() string                    { return "" }
func (*boundResourceFrameEnv) CallLine() int                       { return 0 }
func (e *boundResourceFrameEnv) Context() context.Context          { return e.ctx }
func (*boundResourceFrameEnv) Fatal(any)                           {}
func (*boundResourceFrameEnv) MarkdownConverter() native.Converter { return nil }
func (*boundResourceFrameEnv) Print(...any)                        {}
func (*boundResourceFrameEnv) Println(...any)                      {}
func (e *boundResourceFrameEnv) Stop(err error) {
	e.mu.Lock()
	e.err = errors.Join(e.err, err)
	e.mu.Unlock()
}
func (*boundResourceFrameEnv) TypeOf(value reflect.Value) reflect.Type { return value.Type() }

func (e *boundResourceFrameEnv) stopError() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}

func TestBoundResourceFrameUsesOneInvocationGateForProductionCallVolume(t *testing.T) {
	const calls = 69_011
	ctx := templating.WithImmutableResourceInputs(t.Context())
	core := &boundResourceFrameViewCore{item: map[string]any{"name": "route"}}
	view := &boundResourceFrameView{boundResourceFrameViewCore: core}
	lease := &boundResourceFrameLease{
		expectedCtx: ctx,
		err:         errors.New("legacy resource validation must not run"),
	}
	view.expectedLease = lease
	resources := bindBoundResourceFrameTestResources(t, ctx, view, lease)
	env := &boundResourceFrameEnv{ctx: ctx}

	for range calls {
		result := callBoundResourceFrameGetSingle(resources, env)
		if result == nil {
			t.Fatal("bound resource call returned an empty result")
		}
	}

	require.NoError(t, env.stopError())
	assert.Equal(t, int64(calls), core.boundBegins.Load())
	assert.Equal(t, int64(calls), core.releases.Load())
	assert.Equal(t, int64(calls), core.reads.Load())
	assert.Zero(t, core.legacyBegins.Load())
	assert.Zero(t, lease.validations.Load())
}

func TestBoundResourceFrameFallbackMatchesLegacyResultAndAccounting(t *testing.T) {
	ctx := templating.WithImmutableResourceInputs(t.Context())
	item := map[string]any{"name": "route"}
	boundCore := &boundResourceFrameViewCore{item: item}
	boundView := &boundResourceFrameView{boundResourceFrameViewCore: boundCore}
	boundLease := &boundResourceFrameLease{
		expectedCtx: ctx,
		err:         errors.New("legacy resource validation must not run"),
	}
	boundView.expectedLease = boundLease
	bound := bindBoundResourceFrameTestResources(t, ctx, boundView, boundLease)
	boundEnv := &boundResourceFrameEnv{ctx: ctx}
	boundResult := callBoundResourceFrameGetSingle(bound, boundEnv)

	legacyCore := &boundResourceFrameViewCore{item: item}
	legacyView := &legacyResourceFrameView{boundResourceFrameViewCore: legacyCore}
	legacyLease := &boundResourceFrameLease{expectedCtx: ctx}
	legacy := bindBoundResourceFrameTestResources(t, ctx, legacyView, legacyLease)
	legacyEnv := &boundResourceFrameEnv{ctx: ctx}
	legacyResult := callBoundResourceFrameGetSingle(legacy, legacyEnv)

	require.NoError(t, boundEnv.stopError())
	require.NoError(t, legacyEnv.stopError())
	assert.Equal(t, legacyResult, boundResult)
	assert.Equal(t, int64(1), boundCore.boundBegins.Load())
	assert.Zero(t, boundCore.legacyBegins.Load())
	assert.Zero(t, boundLease.validations.Load())
	assert.Zero(t, legacyCore.boundBegins.Load())
	assert.Equal(t, int64(1), legacyCore.legacyBegins.Load())
	assert.Equal(t, int64(1), legacyLease.validations.Load())
}

func TestBoundResourceFrameFailureZerosResultAndReleasesInvocation(t *testing.T) {
	ctx := templating.WithImmutableResourceInputs(t.Context())
	core := &boundResourceFrameViewCore{
		item:    map[string]any{"name": "route"},
		readErr: errors.New("resource read failed"),
	}
	view := &boundResourceFrameView{boundResourceFrameViewCore: core}
	lease := &boundResourceFrameLease{expectedCtx: ctx}
	view.expectedLease = lease
	resources := bindBoundResourceFrameTestResources(t, ctx, view, lease)
	env := &boundResourceFrameEnv{ctx: ctx}

	result := callBoundResourceFrameGetSingle(resources, env)
	assert.Nil(t, result)
	require.ErrorContains(t, env.stopError(), "resource read failed")
	assert.Equal(t, int64(1), core.boundBegins.Load())
	assert.Equal(t, int64(1), core.releases.Load())
	assert.Equal(t, int64(1), core.reads.Load())
	assert.Zero(t, core.legacyBegins.Load())
	assert.Zero(t, lease.validations.Load())
}

func TestBoundResourceFrameConcurrentCallsKeepInvocationsSeparate(t *testing.T) {
	const (
		workers = 16
		calls   = 1_000
	)
	ctx := templating.WithImmutableResourceInputs(t.Context())
	core := &boundResourceFrameViewCore{item: map[string]any{"name": "route"}}
	view := &boundResourceFrameView{boundResourceFrameViewCore: core}
	lease := &boundResourceFrameLease{
		expectedCtx: ctx,
		err:         errors.New("legacy resource validation must not run"),
	}
	view.expectedLease = lease
	resources := bindBoundResourceFrameTestResources(t, ctx, view, lease)
	var group sync.WaitGroup
	group.Add(workers)
	for range workers {
		go func() {
			defer group.Done()
			env := &boundResourceFrameEnv{ctx: ctx}
			for range calls {
				if callBoundResourceFrameGetSingle(resources, env) == nil {
					return
				}
			}
			assert.NoError(t, env.stopError())
		}()
	}
	group.Wait()

	want := int64(workers * calls)
	assert.Equal(t, want, core.boundBegins.Load())
	assert.Equal(t, want, core.releases.Load())
	assert.Equal(t, want, core.reads.Load())
	assert.Zero(t, core.legacyBegins.Load())
	assert.Zero(t, lease.validations.Load())
}

func BenchmarkBoundResourceFrameProductionCallVolume(b *testing.B) {
	const calls = 69_011
	for _, test := range []struct {
		name  string
		bound bool
	}{
		{name: "bound", bound: true},
		{name: "legacy", bound: false},
	} {
		b.Run(test.name, func(b *testing.B) {
			ctx := templating.WithImmutableResourceInputs(b.Context())
			core := &boundResourceFrameViewCore{item: map[string]any{"name": "route"}}
			lease := &boundResourceFrameLease{expectedCtx: ctx}
			var view StoreSnapshotView = &legacyResourceFrameView{
				boundResourceFrameViewCore: core,
			}
			if test.bound {
				lease.err = errors.New("legacy resource validation must not run")
				boundView := &boundResourceFrameView{boundResourceFrameViewCore: core}
				boundView.expectedLease = lease
				view = boundView
			}
			resources := bindBoundResourceFrameTestResources(b, ctx, view, lease)
			env := &boundResourceFrameEnv{ctx: ctx}
			b.ReportAllocs()
			b.ReportMetric(calls, "resource-calls/op")
			b.ResetTimer()
			for range b.N {
				for range calls {
					_ = callBoundResourceFrameGetSingle(resources, env)
				}
			}
			b.StopTimer()
			require.NoError(b, env.stopError())
			want := int64(b.N * calls)
			if test.bound {
				assert.Equal(b, want, core.boundBegins.Load())
				assert.Zero(b, core.legacyBegins.Load())
				assert.Zero(b, lease.validations.Load())
				return
			}
			assert.Zero(b, core.boundBegins.Load())
			assert.Equal(b, want, core.legacyBegins.Load())
			assert.Equal(b, want, lease.validations.Load())
		})
	}
}

func bindBoundResourceFrameTestResources(
	tb testing.TB,
	ctx context.Context,
	view StoreSnapshotView,
	lease templating.IncrementalResourceInvocationLease,
) any {
	tb.Helper()
	base := BuildIncrementalResourcesValueWithViews(
		ctx,
		map[string]stores.Store{"routes": &storetest.MockStore{}},
		nil,
		[]string{"routes"},
		func(string) []string { return []string{"metadata.namespace", "metadata.name"} },
		func(string) bool { return false },
		func(string) string { return "example.test/v1" }, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		NewResourceErrorCollector(),
		view,
		nil,
		false,
	)
	bound, err := templating.BindAllIncrementalResources(base, lease)
	require.NoError(tb, err)
	return bound
}

func callBoundResourceFrameGetSingle(resources any, env native.Env) any {
	resource := reflect.ValueOf(resources).Elem().FieldByName("Routes").Elem()
	callable := resource.FieldByName("GetSingle")
	results := callable.CallSlice([]reflect.Value{
		reflect.ValueOf(env).Convert(reflect.TypeFor[native.Env]()),
		reflect.ValueOf([]any{"default", "route"}),
	})
	return results[0].Interface()
}
