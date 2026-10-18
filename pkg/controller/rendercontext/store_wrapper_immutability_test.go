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

package rendercontext

import (
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores/storetest"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

type immutableRootMetadata struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type immutableRootSpec struct {
	Value  string   `json:"value"`
	Values []string `json:"values"`
}

type immutableRootResource struct {
	Metadata immutableRootMetadata `json:"metadata"`
	Spec     immutableRootSpec     `json:"spec"`
}

func (resource *immutableRootResource) Poison() string {
	resource.Spec.Value = "changed"
	resource.Spec.Values[0] = "changed"
	return "changed"
}

type immutableRootFixture struct {
	context     map[string]any
	declaration any
}

type immutableRootSharedRecorder struct{}

func (*immutableRootSharedRecorder) Unique(string, string, string) {}

func newImmutableRootFixture(t *testing.T, store stores.Store, typed bool) immutableRootFixture {
	t.Helper()
	ctx := templating.WithImmutableResourceInputs(t.Context())
	typedTypes := map[string]reflect.Type{}
	if typed {
		typedTypes["routes"] = reflect.TypeFor[immutableRootResource]()
	}
	resources := BuildResourcesValue(
		ctx,
		map[string]stores.Store{"routes": store},
		typedTypes,
		[]string{"routes"},
		func(string) []string { return []string{"metadata.namespace", "metadata.name"} },
		func(string) bool { return false },
		func(string) string { return "example.test/v1" },
		testutil.NewTestLogger(),
	)
	templateContext := map[string]any{
		"resources": resources,
		"shared":    templating.NewSharedContext(),
	}
	require.NoError(t, templating.BindImmutableResourceInputs(templateContext, ctx))
	return immutableRootFixture{
		context:     templateContext,
		declaration: reflect.Zero(reflect.TypeOf(resources)).Interface(),
	}
}

func immutableRootStoredResource() map[string]any {
	return map[string]any{
		"apiVersion": "example.test/v1",
		"kind":       "Route",
		"metadata": map[string]any{
			"namespace": "default",
			"name":      "route",
		},
		"spec": map[string]any{
			"value":  "original",
			"values": []any{"original"},
		},
	}
}

func TestRootResourceMutationsFailBeforeChangingCachedInputs(t *testing.T) {
	tests := []struct {
		name     string
		typed    bool
		template string
		run      func(*templating.ScriggoEngine, map[string]any) error
	}{
		{
			name:     "untyped map through Render",
			template: `{%% var values = resources.routes.List() %%}{%% values[0].(map[string]any)["spec"].(map[string]any)["value"] = "changed" %%}`,
			run: func(engine *templating.ScriggoEngine, ctx map[string]any) error {
				_, err := engine.Render(t.Context(), "mutate", ctx)
				return err
			},
		},
		{
			name:     "untyped map through jsonpathSet",
			template: `{%% var values = resources.routes.List() %%}{{ jsonpathSet(values[0], "spec.value", "changed") }}`,
			run: func(engine *templating.ScriggoEngine, ctx map[string]any) error {
				_, err := engine.Render(t.Context(), "mutate", ctx)
				return err
			},
		},
		{
			name:     "typed field through RenderWithProfiling",
			typed:    true,
			template: `{%% var values = resources.routes.List() %%}{%% values[0].Spec.Value = "changed" %%}`,
			run: func(engine *templating.ScriggoEngine, ctx map[string]any) error {
				_, _, err := engine.RenderWithProfiling(t.Context(), "mutate", ctx)
				return err
			},
		},
		{
			name:     "typed pointer through jsonpathSet",
			typed:    true,
			template: `{%% var values = resources.routes.List() %%}{{ jsonpathSet(values[0], "spec.value", "changed") }}`,
			run: func(engine *templating.ScriggoEngine, ctx map[string]any) error {
				_, err := engine.Render(t.Context(), "mutate", ctx)
				return err
			},
		},
		{
			name:     "typed nested slice through RenderWithSourceMap",
			typed:    true,
			template: `{%% var values = resources.routes.List() %%}{%% values[0].Spec.Values[0] = "changed" %%}`,
			run: func(engine *templating.ScriggoEngine, ctx map[string]any) error {
				_, _, err := engine.RenderWithSourceMap(t.Context(), "mutate", ctx)
				return err
			},
		},
		{
			name:     "typed result slice through Render",
			typed:    true,
			template: `{%% var values = resources.routes.List() %%}{%% values[0] = nil %%}`,
			run: func(engine *templating.ScriggoEngine, ctx map[string]any) error {
				_, err := engine.Render(t.Context(), "mutate", ctx)
				return err
			},
		},
		{
			name:     "typed result slice through reverse",
			typed:    true,
			template: `{%% var values = resources.routes.List() %%}{%% reverse(values) %%}`,
			run: func(engine *templating.ScriggoEngine, ctx map[string]any) error {
				_, err := engine.Render(t.Context(), "mutate", ctx)
				return err
			},
		},
		{
			name:     "typed pointer through unmarshalJSON",
			typed:    true,
			template: `{%% var values = resources.routes.List() %%}{{ unmarshalJSON("{\"metadata\":{\"name\":\"changed\"},\"spec\":{\"value\":\"changed\"}}", values[0]) }}`,
			run: func(engine *templating.ScriggoEngine, ctx map[string]any) error {
				_, err := engine.Render(t.Context(), "mutate", ctx)
				return err
			},
		},
		{
			name:     "typed pointer through unmarshalYAML",
			typed:    true,
			template: `{%% var values = resources.routes.List() %%}{{ unmarshalYAML("metadata:\n  name: changed\nspec:\n  value: changed\n", values[0]) }}`,
			run: func(engine *templating.ScriggoEngine, ctx map[string]any) error {
				_, err := engine.Render(t.Context(), "mutate", ctx)
				return err
			},
		},
		{
			name:     "typed pointer through native method",
			typed:    true,
			template: `{%% var values = resources.routes.List() %%}{{ values[0].Poison() }}`,
			run: func(engine *templating.ScriggoEngine, ctx map[string]any) error {
				_, err := engine.Render(t.Context(), "mutate", ctx)
				return err
			},
		},
		{
			name:     "typed pointer through native method value",
			typed:    true,
			template: `{%% var values = resources.routes.List(); var poison = values[0].Poison %%}{{ poison() }}`,
			run: func(engine *templating.ScriggoEngine, ctx map[string]any) error {
				_, err := engine.Render(t.Context(), "mutate", ctx)
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stored := immutableRootStoredResource()
			second := immutableRootStoredResource()
			second["metadata"].(map[string]any)["name"] = "route-second"
			second["spec"].(map[string]any)["value"] = "second"
			store := &storetest.MockStore{Items: []any{stored, second}}
			fixture := newImmutableRootFixture(t, store, test.typed)
			readTemplate := `{%% var values = resources.routes.List() %%}{{ values[0] | dig("spec", "value") }}`
			if test.typed {
				readTemplate = `{%% var values = resources.routes.List() %%}{{ values[0].Spec.Value }}:{{ values[0].Spec.Values[0] }}`
			}
			engine, err := templating.New(map[string]string{
				"mutate": test.template,
				"read":   readTemplate,
			}, &templating.Options{
				Declarations: map[string]any{"resources": fixture.declaration},
				Profiling:    true,
			})
			require.NoError(t, err)

			err = test.run(engine, fixture.context)
			require.ErrorContains(t, err, "template mutates an immutable input")
			assert.Equal(t, "original", stored["spec"].(map[string]any)["value"])
			assert.Equal(t, "original", stored["spec"].(map[string]any)["values"].([]any)[0])

			output, err := engine.Render(t.Context(), "read", fixture.context)
			require.NoError(t, err)
			if test.typed {
				assert.Equal(t, "original:original\n", output)
			} else {
				assert.Equal(t, "original\n", output)
			}

			fresh := newImmutableRootFixture(t, store, test.typed)
			output, err = engine.Render(t.Context(), "read", fresh.context)
			require.NoError(t, err)
			if test.typed {
				assert.Equal(t, "original:original\n", output)
			} else {
				assert.Equal(t, "original\n", output)
			}
		})
	}
}

func TestRootResourceGuardKeepsLocalAndSharedValuesMutable(t *testing.T) {
	stored := immutableRootStoredResource()
	stored["spec"].(map[string]any)["values"] = []any{}
	store := &storetest.MockStore{Items: []any{stored}}
	fixture := newImmutableRootFixture(t, store, true)
	engine, err := templating.New(map[string]string{
		"template": `{%%
			var resourcesSeen = resources.routes.List()
			var local = map[string]any{"value": "before"}
			local["value"] = "local"
			var nativeLocal = map[string]any{"value": "before"}
			var nativeChanged = jsonpathSet(nativeLocal, "value", "native")
			var localSlice = []string{}
			localSlice = append(localSlice, "slice")
			var reversed = []string{"first", "second"}
			reverse(reversed)
			var cached, _ = shared.ComputeIfAbsent("mutable", func() any {
				return map[string]any{"value": "before"}
			})
			cached.(map[string]any)["value"] = "shared"
		%%}{{ local["value"] }} {{ nativeChanged }} {{ nativeLocal["value"] }} {{ localSlice[0] }} {{ reversed[0] }} {{ cached.(map[string]any)["value"] }} {{ resourcesSeen[0].Spec.Value }}`,
	}, &templating.Options{Declarations: map[string]any{"resources": fixture.declaration}})
	require.NoError(t, err)

	output, err := engine.Render(t.Context(), "template", fixture.context)
	require.NoError(t, err)
	assert.Equal(t, "local true native slice second shared original\n", output)
}

func TestRootResourceGuardKeepsControllerReadsUsable(t *testing.T) {
	stored := immutableRootStoredResource()
	ctx := templating.WithImmutableResourceInputs(t.Context())
	wrapper := &StoreWrapper{
		Store:          &storetest.MockStore{Items: []any{stored}},
		ResourceType:   "routes",
		Logger:         testutil.NewTestLogger(),
		readContext:    ctx,
		resourceErrors: NewResourceErrorCollector(),
		IndexBy:        []string{"metadata.namespace", "metadata.name"},
		DerivedView:    NewDerivedResourceView(),
	}
	templateContext := map[string]any{
		"controller": map[string]templating.ResourceStore{"routes": wrapper},
		"shared":     templating.NewSharedContext(),
	}
	require.NoError(t, templating.BindImmutableResourceInputs(templateContext, ctx))
	engine, err := templating.New(map[string]string{
		"template": `{%%
			var listed = controller["routes"].List()
			var fetched = controller["routes"].Fetch("default", "route")
			var single = controller["routes"].GetSingle("default", "route")
		%%}{{ len(listed) }} {{ fetched[0] | dig("metadata", "name") }} {{ single | dig("metadata", "name") }}`,
	}, &templating.Options{
		Declarations: map[string]any{"controller": (*map[string]templating.ResourceStore)(nil)},
	})
	require.NoError(t, err)

	output, err := engine.Render(t.Context(), "template", templateContext)
	require.NoError(t, err)
	assert.Equal(t, "1 route route\n", output)
}

func TestIncrementalResourceGuardKeepsControllerReadsUsable(t *testing.T) {
	stored := immutableRootStoredResource()
	ctx := templating.WithIncrementalImmutableInputs(t.Context())
	wrapper := &StoreWrapper{
		Store:          &storetest.MockStore{Items: []any{stored}},
		ResourceType:   "routes",
		Logger:         testutil.NewTestLogger(),
		readContext:    ctx,
		resourceErrors: NewResourceErrorCollector(),
		IndexBy:        []string{"metadata.namespace", "metadata.name"},
		DerivedView:    NewDerivedResourceView(),
	}
	engine, err := templating.New(map[string]string{
		"component": `{%%
			var listed = controller["routes"].List()
			var fetched = controller["routes"].Fetch("default", "route")
			var single = controller["routes"].GetSingle("default", "route")
		%%}{{ len(listed) }} {{ fetched[0] | dig("metadata", "name") }} {{ single | dig("metadata", "name") }}`,
	}, &templating.Options{
		EntryPoints:            []string{"component"},
		IncrementalEntryPoints: []string{"component"},
		Declarations: map[string]any{
			"controller": (*map[string]templating.ResourceStore)(nil),
		},
	})
	require.NoError(t, err)

	output, err := engine.RenderIncrementalComponent(ctx, "component", map[string]any{
		"source":        "routes",
		"item":          map[string]any{},
		"props":         map[string]any{},
		"renderSubject": map[string]any{"mode": "reconcile"},
		"controller":    map[string]templating.ResourceStore{"routes": wrapper},
		"shared":        templating.NewSharedContributionContext(&immutableRootSharedRecorder{}),
	})
	require.NoError(t, err)
	assert.Equal(t, "1 route route", output)
}

func TestCustomNativeExtensionsCannotReceiveResourceAliases(t *testing.T) {
	for _, extension := range []string{"function", "filter"} {
		t.Run(extension, func(t *testing.T) {
			stored := immutableRootStoredResource()
			fixture := newImmutableRootFixture(t, &storetest.MockStore{Items: []any{stored}}, true)
			var calls atomic.Int64
			mutate := func(value any) any {
				calls.Add(1)
				spec := value.(immutableRootSpec)
				spec.Values[0] = "changed"
				return "changed"
			}
			options := &templating.Options{Declarations: map[string]any{"resources": fixture.declaration}}
			template := `{%% var values = resources.routes.List() %%}{{ poison(values[0].Spec) }}`
			if extension == "function" {
				options.Functions = map[string]templating.GlobalFunc{
					"poison": func(args ...any) (any, error) { return mutate(args[0]), nil },
				}
			} else {
				template = `{%% var values = resources.routes.List() %%}{{ values[0].Spec | poison() }}`
				options.Filters = map[string]templating.FilterFunc{
					"poison": func(value any, _ ...any) (any, error) { return mutate(value), nil },
				}
			}
			engine, err := templating.New(map[string]string{
				"mutate": template,
				"read":   `{%% var values = resources.routes.List() %%}{{ values[0].Spec.Values[0] }}`,
			}, options)
			require.NoError(t, err)

			_, err = engine.Render(t.Context(), "mutate", fixture.context)
			require.ErrorContains(t, err, "template mutates an immutable input")
			assert.Zero(t, calls.Load())

			output, err := engine.Render(t.Context(), "read", fixture.context)
			require.NoError(t, err)
			assert.Equal(t, "original\n", output)
		})
	}
}

type immutableRootSnapshotView struct {
	lists atomic.Int64
}

func (v *immutableRootSnapshotView) List(string, stores.Store) ([]any, error) {
	generation := v.lists.Add(1)
	item := immutableRootStoredResource()
	item["spec"].(map[string]any)["value"] = strconv.FormatInt(generation, 10)
	return []any{item}, nil
}

func (*immutableRootSnapshotView) Get(string, stores.Store, ...string) ([]any, error) {
	return nil, nil
}

func TestTypedResourceListIsAdaptedOncePerRender(t *testing.T) {
	view := &immutableRootSnapshotView{}
	build := func() reflect.Value {
		ctx := templating.WithImmutableResourceInputs(t.Context())
		resources := BuildResourcesValueWithViews(
			ctx,
			map[string]stores.Store{"routes": &storetest.MockStore{}},
			map[string]reflect.Type{"routes": reflect.TypeFor[immutableRootResource]()},
			[]string{"routes"},
			func(string) []string { return []string{"metadata.namespace", "metadata.name"} },
			func(string) bool { return false },
			func(string) string { return "example.test/v1" }, nil,
			testutil.NewTestLogger(),
			NewResourceErrorCollector(),
			view,
			nil,
			false,
		)
		resource := reflect.ValueOf(resources).Elem().Field(0).Elem()
		return resource.FieldByName("List")
	}

	list := build()
	const callers = 64
	start := make(chan struct{})
	pointers := make(chan uintptr, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			result := list.Call(nil)[0]
			pointers <- result.Pointer()
		}()
	}
	close(start)
	wait.Wait()
	close(pointers)

	var first uintptr
	for pointer := range pointers {
		if first == 0 {
			first = pointer
		}
		assert.Equal(t, first, pointer)
	}
	assert.Equal(t, int64(1), view.lists.Load())

	nextList := build()
	next := nextList.Call(nil)[0]
	assert.NotEqual(t, first, next.Pointer())
	assert.Equal(t, int64(2), view.lists.Load())
}

func BenchmarkTypedResourceListMemoized(b *testing.B) {
	for _, benchmark := range []struct {
		name  string
		items int
	}{
		{name: "one", items: 1},
		{name: "three_thousand", items: 3000},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			items := make([]any, benchmark.items)
			for index := range items {
				item := immutableRootStoredResource()
				item["metadata"].(map[string]any)["name"] = "route-" + strconv.Itoa(index)
				items[index] = item
			}
			ctx := templating.WithImmutableResourceInputs(b.Context())
			resources := BuildResourcesValue(
				ctx,
				map[string]stores.Store{"routes": &storetest.MockStore{Items: items}},
				map[string]reflect.Type{"routes": reflect.TypeFor[immutableRootResource]()},
				[]string{"routes"},
				nil,
				nil,
				nil,
				testutil.NewTestLogger(),
			)
			list := reflect.ValueOf(resources).Elem().Field(0).Elem().FieldByName("List")
			first := list.Call(nil)[0].Pointer()

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if pointer := list.Call(nil)[0].Pointer(); pointer != first {
					b.Fatalf("List result pointer = %d, want %d", pointer, first)
				}
			}
		})
	}
}
