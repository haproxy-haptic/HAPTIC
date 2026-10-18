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
	"io"
	"log/slog"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores/storetest"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
	"github.com/haproxy-haptic/scriggo/native"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type directBoundResourceSpecTestResource struct {
	Value string `json:"value"`
}

type directBoundResourceSpecTestLease struct {
	ctx context.Context
}

func (l *directBoundResourceSpecTestLease) ValidateIncrementalResourceInvocation(
	ctx context.Context,
) error {
	if l == nil || ctx != l.ctx {
		return errors.New("direct resource spec test lease has invalid provenance")
	}
	return nil
}

type directBoundResourceSpecCaptureView struct {
	ctx        context.Context
	lease      templating.IncrementalResourceInvocationLease
	generation atomic.Uint64
	active     atomic.Uint64
	mu         sync.Mutex
	request    *rendercontext.DirectBoundResourceMaterializationRequest
	keys       []string
}

func (*directBoundResourceSpecCaptureView) MemoizeStoreMaterialization() bool {
	return false
}

func (*directBoundResourceSpecCaptureView) List(string, stores.Store) ([]any, error) {
	return nil, errors.New("unscoped resource list reached the direct spec capture")
}

func (*directBoundResourceSpecCaptureView) Get(string, stores.Store, ...string) ([]any, error) {
	return nil, errors.New("unscoped resource lookup reached the direct spec capture")
}

func (v *directBoundResourceSpecCaptureView) BeginDirectBoundStoreInvocation(
	ctx context.Context,
	lease templating.IncrementalResourceInvocationLease,
) (rendercontext.DirectBoundStoreInvocation, error) {
	if v == nil || ctx != v.ctx || lease != v.lease {
		return rendercontext.DirectBoundStoreInvocation{}, errors.New("foreign direct spec invocation")
	}
	generation := v.generation.Add(1)
	if generation == 0 || !v.active.CompareAndSwap(0, generation) {
		return rendercontext.DirectBoundStoreInvocation{}, errors.New("concurrent direct spec invocation")
	}
	return rendercontext.NewDirectBoundStoreInvocation(lease, 0, generation)
}

func (v *directBoundResourceSpecCaptureView) EndDirectBoundStoreInvocation(
	invocation rendercontext.DirectBoundStoreInvocation,
) error {
	if v == nil || invocation.Lease() != v.lease || invocation.Slot() != 0 ||
		invocation.Generation() == 0 ||
		!v.active.CompareAndSwap(invocation.Generation(), 0) {
		return errors.New("stale direct spec invocation")
	}
	return nil
}

func (*directBoundResourceSpecCaptureView) ListDirectBound(
	context.Context,
	rendercontext.DirectBoundStoreInvocation,
	string,
	stores.Store,
) ([]any, error) {
	return nil, errors.New("direct list bypassed materialization")
}

func (*directBoundResourceSpecCaptureView) GetDirectBound(
	context.Context,
	rendercontext.DirectBoundStoreInvocation,
	string,
	stores.Store,
	...string,
) ([]any, error) {
	return nil, errors.New("direct lookup bypassed materialization")
}

func (v *directBoundResourceSpecCaptureView) MaterializeDirectBoundResource(
	ctx context.Context,
	invocation rendercontext.DirectBoundStoreInvocation,
	request *rendercontext.DirectBoundResourceMaterializationRequest,
	_ stores.Store,
	keys []string,
) (reflect.Value, error) {
	if v == nil || ctx != v.ctx || invocation.Lease() != v.lease ||
		v.active.Load() != invocation.Generation() {
		return reflect.Value{}, errors.New("unauthenticated direct spec materialization")
	}
	declaration, err := request.Describe()
	if err != nil {
		return reflect.Value{}, err
	}
	v.mu.Lock()
	v.request = request
	v.keys = slices.Clone(keys)
	v.mu.Unlock()
	return reflect.Zero(declaration.ReturnType), nil
}

type directBoundResourceSpecTestEnv struct {
	ctx context.Context
	mu  sync.Mutex
	err error
}

func (*directBoundResourceSpecTestEnv) CallPath() string                        { return "direct-spec" }
func (*directBoundResourceSpecTestEnv) CallLine() int                           { return 1 }
func (e *directBoundResourceSpecTestEnv) Context() context.Context              { return e.ctx }
func (*directBoundResourceSpecTestEnv) Fatal(any)                               {}
func (*directBoundResourceSpecTestEnv) MarkdownConverter() native.Converter     { return nil }
func (*directBoundResourceSpecTestEnv) Print(...any)                            {}
func (*directBoundResourceSpecTestEnv) Println(...any)                          {}
func (*directBoundResourceSpecTestEnv) TypeOf(value reflect.Value) reflect.Type { return value.Type() }

func (e *directBoundResourceSpecTestEnv) Stop(err error) {
	e.mu.Lock()
	e.err = errors.Join(e.err, err)
	e.mu.Unlock()
}

func (e *directBoundResourceSpecTestEnv) stopError() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}

func directBoundResourceSpecTestDeclaration(
	tb testing.TB,
	resourceType string,
	operation rendercontext.DirectBoundResourceOperation,
	elementType reflect.Type,
	keys []string,
) rendercontext.DirectBoundResourceMaterialization {
	tb.Helper()
	ctx := tb.Context()
	lease := &directBoundResourceSpecTestLease{ctx: ctx}
	view := &directBoundResourceSpecCaptureView{ctx: ctx, lease: lease}
	typedTypes := map[string]reflect.Type{}
	if elementType != nil {
		typedTypes[resourceType] = elementType
	}
	resources := rendercontext.BuildIncrementalResourcesValueWithViews(
		ctx,
		map[string]stores.Store{resourceType: &storetest.MockStore{}},
		typedTypes,
		[]string{resourceType},
		func(string) []string { return nil },
		func(string) bool { return false },
		func(string) string { return "example.test/v1" }, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		rendercontext.NewResourceErrorCollector(),
		view,
		nil,
		false,
	)
	bound, err := templating.BindAllIncrementalResources(resources, lease)
	require.NoError(tb, err)
	resource := reflect.ValueOf(bound).Elem().Field(0).Elem()
	method := map[rendercontext.DirectBoundResourceOperation]string{
		rendercontext.DirectBoundResourceList:      "List",
		rendercontext.DirectBoundResourceFetch:     "Fetch",
		rendercontext.DirectBoundResourceGetSingle: "GetSingle",
	}[operation]
	require.NotEmpty(tb, method)
	callable := resource.FieldByName(method)
	env := &directBoundResourceSpecTestEnv{ctx: ctx}
	args := []reflect.Value{reflect.ValueOf(native.Env(env))}
	if callable.Type().IsVariadic() {
		boxed := make([]any, len(keys))
		for index := range keys {
			boxed[index] = keys[index]
		}
		args = append(args, reflect.ValueOf(boxed))
		callable.CallSlice(args)
	} else {
		callable.Call(args)
	}
	require.NoError(tb, env.stopError())
	view.mu.Lock()
	request := view.request
	observedKeys := slices.Clone(view.keys)
	view.mu.Unlock()
	require.NotNil(tb, request)
	require.True(tb, slices.Equal(keys, observedKeys))
	declaration, err := request.Describe()
	require.NoError(tb, err)
	return declaration
}

func TestIncrementalDirectBoundResourceSpecReusesExactShape(t *testing.T) {
	firstDeclaration := directBoundResourceSpecTestDeclaration(
		t,
		"routes",
		rendercontext.DirectBoundResourceGetSingle,
		nil,
		[]string{"default", "route"},
	)
	secondDeclaration := directBoundResourceSpecTestDeclaration(
		t,
		"routes",
		rendercontext.DirectBoundResourceGetSingle,
		nil,
		[]string{"default", "route"},
	)
	arena := newIncrementalResourceMaterializationArena()
	callerKeys := []string{"default", "route"}
	first, err := arena.directBoundResourceSpec(firstDeclaration, callerKeys)
	require.NoError(t, err)
	callerKeys[0] = "poison"
	second, err := arena.directBoundResourceSpec(
		secondDeclaration,
		[]string{"default", "route"},
	)
	require.NoError(t, err)

	assert.Same(t, first, second)
	assert.Equal(t, []string{"default", "route"}, first.keys)
	assert.Equal(t, 1, arena.direct.len())
}

func TestIncrementalDirectBoundResourceSpecUsesExactKeyBytes(t *testing.T) {
	declaration := directBoundResourceSpecTestDeclaration(
		t,
		"routes",
		rendercontext.DirectBoundResourceFetch,
		nil,
		[]string{"a", "b", "c", "d", "e"},
	)
	tests := [][]string{
		{"a", "b", "c", "d", "e"},
		{"a", "b", "c", "d", "e\x00"},
		{"a", "b", "c", "d\x00", "e"},
		{"ab", "", "c", "d", "e"},
		{"a", "b", "c.d", "d", "e"},
		{"a", "b", "c", "d", "e", ""},
	}
	arena := newIncrementalResourceMaterializationArena()
	results := make(map[*resourceInputSpec]struct{}, len(tests))
	for _, keys := range tests {
		spec, err := arena.directBoundResourceSpec(declaration, keys)
		require.NoError(t, err)
		assert.Equal(t, keys, spec.keys)
		results[spec] = struct{}{}
	}
	assert.Len(t, results, len(tests))
	assert.Equal(t, len(tests), arena.direct.len())
}

func TestIncrementalDirectBoundResourceSpecNormalizesEmptyKeys(t *testing.T) {
	declaration := directBoundResourceSpecTestDeclaration(
		t,
		"routes",
		rendercontext.DirectBoundResourceFetch,
		nil,
		nil,
	)
	arena := newIncrementalResourceMaterializationArena()
	fromNil, err := arena.directBoundResourceSpec(declaration, nil)
	require.NoError(t, err)
	fromEmpty, err := arena.directBoundResourceSpec(declaration, []string{})
	require.NoError(t, err)

	assert.Same(t, fromNil, fromEmpty)
	assert.Equal(t, 1, arena.direct.len())
}

func TestIncrementalDirectBoundResourceSpecSeparatesDeclarationShape(t *testing.T) {
	requests := []struct {
		resourceType string
		operation    rendercontext.DirectBoundResourceOperation
		elementType  reflect.Type
		keys         []string
	}{
		{"routes", rendercontext.DirectBoundResourceList, nil, nil},
		{"routes", rendercontext.DirectBoundResourceFetch, nil, []string{"default", "route"}},
		{"routes", rendercontext.DirectBoundResourceGetSingle, nil, []string{"default", "route"}},
		{
			"routes",
			rendercontext.DirectBoundResourceGetSingle,
			reflect.TypeFor[directBoundResourceSpecTestResource](),
			[]string{"default", "route"},
		},
		{"others", rendercontext.DirectBoundResourceGetSingle, nil, []string{"default", "route"}},
	}
	arena := newIncrementalResourceMaterializationArena()
	results := make(map[*resourceInputSpec]struct{}, len(requests))
	for _, request := range requests {
		declaration := directBoundResourceSpecTestDeclaration(
			t, request.resourceType, request.operation, request.elementType, request.keys,
		)
		spec, err := arena.directBoundResourceSpec(declaration, request.keys)
		require.NoError(t, err)
		results[spec] = struct{}{}
	}
	assert.Len(t, results, len(requests))
	assert.Equal(t, len(requests), arena.direct.len())
}

func TestIncrementalDirectBoundResourceSpecHashCollisionKeepsExactKeys(t *testing.T) {
	declaration := directBoundResourceSpecTestDeclaration(
		t,
		"routes",
		rendercontext.DirectBoundResourceFetch,
		nil,
		[]string{"left"},
	)
	left := newIncrementalDirectBoundResourceSpecKey(declaration, []string{"left"})
	right := newIncrementalDirectBoundResourceSpecKey(declaration, []string{"right"})
	require.NotEqual(t, left, right)
	const collisionHash = uint64(42)
	var cache incrementalDecodedCache[incrementalDirectBoundResourceSpecKey, string]
	leftValue, err := cache.loadOrCompute(left, collisionHash, func() (string, error) {
		return "left", nil
	})
	require.NoError(t, err)
	rightValue, err := cache.loadOrCompute(right, collisionHash, func() (string, error) {
		return "right", nil
	})
	require.NoError(t, err)

	assert.Equal(t, "left", leftValue)
	assert.Equal(t, "right", rightValue)
	assert.Equal(t, 2, cache.len())
}

func TestIncrementalDirectBoundResourceSpecRejectsPoison(t *testing.T) {
	declaration := directBoundResourceSpecTestDeclaration(
		t,
		"routes",
		rendercontext.DirectBoundResourceGetSingle,
		nil,
		[]string{"default", "route"},
	)
	keys := []string{"default", "route"}
	tests := map[string]func(*incrementalDirectBoundResourceSpec){
		"seal": func(entry *incrementalDirectBoundResourceSpec) {
			entry.seal = nil
		},
		"proof seal": func(entry *incrementalDirectBoundResourceSpec) {
			entry.proof.seal = nil
		},
		"authority": func(entry *incrementalDirectBoundResourceSpec) {
			foreign := &incrementalResourceMaterializationAuthority{}
			foreign.seal.Store(foreign)
			entry.authority = foreign
			entry.proof.authority = foreign
		},
		"key": func(entry *incrementalDirectBoundResourceSpec) {
			entry.key.keys[0] = "poison"
		},
		"proof key": func(entry *incrementalDirectBoundResourceSpec) {
			entry.proof.key.keys[0] = "poison"
		},
		"input key": func(entry *incrementalDirectBoundResourceSpec) {
			entry.inputKey = resourceInputKey(&resourceInputSpec{
				resourceType: "other", scope: resourceInputList,
			})
		},
		"spec keys": func(entry *incrementalDirectBoundResourceSpec) {
			entry.spec.keys[0] = "poison"
		},
	}
	for name, poison := range tests {
		t.Run(name, func(t *testing.T) {
			arena := newIncrementalResourceMaterializationArena()
			_, err := arena.directBoundResourceSpec(declaration, keys)
			require.NoError(t, err)
			key := newIncrementalDirectBoundResourceSpecKey(declaration, keys)
			entry, found, err := arena.direct.load(key, key.hash())
			require.NoError(t, err)
			require.True(t, found)
			poison(entry)

			_, err = arena.directBoundResourceSpec(declaration, keys)
			require.ErrorContains(t, err, "invalid provenance")
		})
	}
}

func TestIncrementalDirectBoundResourceSpecRejectsCopiedAndForeignEntry(t *testing.T) {
	declaration := directBoundResourceSpecTestDeclaration(
		t,
		"routes",
		rendercontext.DirectBoundResourceGetSingle,
		nil,
		[]string{"default", "route"},
	)
	keys := []string{"default", "route"}
	key := newIncrementalDirectBoundResourceSpecKey(declaration, keys)
	firstArena := newIncrementalResourceMaterializationArena()
	_, err := firstArena.directBoundResourceSpec(declaration, keys)
	require.NoError(t, err)
	entry, found, err := firstArena.direct.load(key, key.hash())
	require.NoError(t, err)
	require.True(t, found)
	copied := *entry
	require.ErrorContains(t, copied.authenticate(firstArena, &key), "invalid provenance")

	secondArena := newIncrementalResourceMaterializationArena()
	_, err = secondArena.direct.loadOrCompute(key, key.hash(), func() (
		*incrementalDirectBoundResourceSpec,
		error,
	) {
		return entry, nil
	})
	require.NoError(t, err)
	_, err = secondArena.directBoundResourceSpec(declaration, keys)
	require.ErrorContains(t, err, "invalid provenance")
}

func TestIncrementalDirectBoundResourceSpecRejectsCopiedStaleAndABAArena(t *testing.T) {
	declaration := directBoundResourceSpecTestDeclaration(
		t,
		"routes",
		rendercontext.DirectBoundResourceGetSingle,
		nil,
		[]string{"default", "route"},
	)
	keys := []string{"default", "route"}
	arena := newIncrementalResourceMaterializationArena()
	copied := incrementalResourceMaterializationArena{
		seal: arena.seal, authority: arena.authority,
	}
	_, err := copied.directBoundResourceSpec(declaration, keys)
	require.ErrorContains(t, err, "invalid provenance")

	_, err = arena.directBoundResourceSpec(declaration, keys)
	require.NoError(t, err)
	authority := arena.authority
	arena.revoke()
	authority.seal.Store(authority)
	_, err = arena.directBoundResourceSpec(declaration, keys)
	require.ErrorContains(t, err, "invalid provenance")
	assert.Zero(t, arena.direct.len())
}

func TestIncrementalDirectBoundResourceSpecRejectsTamperedDeclaration(t *testing.T) {
	declaration := directBoundResourceSpecTestDeclaration(
		t,
		"routes",
		rendercontext.DirectBoundResourceGetSingle,
		nil,
		[]string{"default", "route"},
	)
	tampered := declaration
	tampered.ReturnType = reflect.TypeFor[string]()
	arena := newIncrementalResourceMaterializationArena()
	_, err := arena.directBoundResourceSpec(tampered, []string{"default", "route"})
	require.ErrorContains(t, err, "invalid provenance")
	assert.Zero(t, arena.direct.len())
}

func TestIncrementalDirectBoundResourceSpecConcurrentExactKey(t *testing.T) {
	declaration := directBoundResourceSpecTestDeclaration(
		t,
		"routes",
		rendercontext.DirectBoundResourceGetSingle,
		nil,
		[]string{"default", "route"},
	)
	arena := newIncrementalResourceMaterializationArena()
	const workerCount = 64
	results := make(chan *resourceInputSpec, workerCount)
	errs := make(chan error, workerCount)
	start := make(chan struct{})
	var group sync.WaitGroup
	for range workerCount {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			spec, err := arena.directBoundResourceSpec(
				declaration,
				[]string{"default", "route"},
			)
			if err != nil {
				errs <- err
				return
			}
			results <- spec
		}()
	}
	close(start)
	group.Wait()
	close(results)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var expected *resourceInputSpec
	for result := range results {
		if expected == nil {
			expected = result
		}
		assert.Same(t, expected, result)
	}
	assert.Equal(t, 1, arena.direct.len())
}

func BenchmarkIncrementalDirectBoundResourceSpecHit(b *testing.B) {
	declaration := directBoundResourceSpecTestDeclaration(
		b,
		"routes",
		rendercontext.DirectBoundResourceGetSingle,
		nil,
		[]string{"default", "route"},
	)
	arena := newIncrementalResourceMaterializationArena()
	keys := []string{"default", "route"}
	_, err := arena.directBoundResourceSpec(declaration, keys)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchmarkDirectBoundResourceInputSpecPointer, err = arena.directBoundResourceSpec(
			declaration, keys,
		)
		if err != nil {
			b.Fatal(err)
		}
	}
}

var benchmarkDirectBoundResourceInputSpecPointer *resourceInputSpec
