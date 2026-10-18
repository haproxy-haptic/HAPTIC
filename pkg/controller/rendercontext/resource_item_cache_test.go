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
	"context"
	"io"
	"log/slog"
	"reflect"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	k8sstore "github.com/haproxy-haptic/HAPTIC/pkg/k8s/store"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

type resourceItemCacheFixture struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
}

type resourceItemCacheAlternateFixture struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
}

type resourceItemCacheTemplateResource struct {
	Metadata struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Value string `json:"value"`
	} `json:"spec"`
}

type resourceItemCacheView struct {
	source map[string]any
	cache  *ResourceItemCache
}

func (v *resourceItemCacheView) ResourceItemCache() *ResourceItemCache {
	return v.cache
}

func (v *resourceItemCacheView) List(string, stores.Store) ([]any, error) {
	return []any{v.source}, nil
}

func (v *resourceItemCacheView) Get(string, stores.Store, ...string) ([]any, error) {
	return []any{v.source}, nil
}

func (*resourceItemCacheView) NormalizeLookupKeys(string, []any) ([]string, error) {
	return []string{"default", "route"}, nil
}

func (*resourceItemCacheView) PreserveStoreValues() bool {
	return true
}

type resourceItemCacheSharedRecorder struct{}

func (*resourceItemCacheSharedRecorder) Unique(string, string, string) {}

func TestResourceItemCacheSharesExactTypedProjectionAcrossWrappers(t *testing.T) {
	cache := NewResourceItemCache()
	first := resourceItemCacheTestWrapper(cache, "routes")
	second := resourceItemCacheTestWrapper(cache, "routes")
	item := resourceItemCacheTestItem("route-a")

	left, err := first.wrap(templating.WithImmutableResourceInputs(t.Context()), item)
	require.NoError(t, err)
	right, err := second.wrap(templating.WithImmutableResourceInputs(t.Context()), item)
	require.NoError(t, err)
	assert.Equal(t, left.Pointer(), right.Pointer())
	assert.Equal(t, "route-a", left.Elem().FieldByName("Metadata").FieldByName("Name").String())
}

func TestResourceItemCacheKeepsDistinctExactSourcesSeparate(t *testing.T) {
	cache := NewResourceItemCache()
	wrapper := resourceItemCacheTestWrapper(cache, "routes")

	left, err := wrapper.wrap(templating.WithImmutableResourceInputs(t.Context()), resourceItemCacheTestItem("same"))
	require.NoError(t, err)
	right, err := wrapper.wrap(templating.WithImmutableResourceInputs(t.Context()), resourceItemCacheTestItem("same"))
	require.NoError(t, err)
	assert.NotEqual(t, left.Pointer(), right.Pointer())
}

func TestResourceItemCacheKeepsResourceTypesSeparate(t *testing.T) {
	cache := NewResourceItemCache()
	item := resourceItemCacheTestItem("route-a")

	left, err := resourceItemCacheTestWrapper(cache, "routes").wrap(templating.WithImmutableResourceInputs(t.Context()), item)
	require.NoError(t, err)
	right, err := resourceItemCacheTestWrapper(cache, "other-routes").wrap(templating.WithImmutableResourceInputs(t.Context()), item)
	require.NoError(t, err)
	assert.NotEqual(t, left.Pointer(), right.Pointer())
}

func TestResourceItemCacheKeepsElementTypesSeparate(t *testing.T) {
	cache := NewResourceItemCache()
	item := resourceItemCacheTestItem("route-a")

	left, err := resourceItemCacheTestWrapper(cache, "routes").wrap(templating.WithImmutableResourceInputs(t.Context()), item)
	require.NoError(t, err)
	alternate := &resourceItemWrapper{
		elemType:     reflect.TypeFor[resourceItemCacheAlternateFixture](),
		resourceName: "routes",
		cache:        cache,
	}
	right, err := alternate.wrap(templating.WithImmutableResourceInputs(t.Context()), item)
	require.NoError(t, err)
	assert.NotEqual(t, left.Pointer(), right.Pointer())
}

func TestResourceItemCacheRetainsExactSourceIdentity(t *testing.T) {
	cache := NewResourceItemCache()
	wrapper := resourceItemCacheTestWrapper(cache, "routes")
	item := resourceItemCacheTestItem("route-a")
	key, ok := resourceItemKey(wrapper.resourceName, wrapper.elemType, item)
	require.True(t, ok)
	sourceAddress := reflect.ValueOf(item).Pointer()

	_, err := wrapper.wrap(templating.WithImmutableResourceInputs(t.Context()), item)
	require.NoError(t, err)
	runtime.GC()

	cache.mu.Lock()
	entry := cache.entries[key]
	require.NotNil(t, entry)
	retainedAddress := reflect.ValueOf(entry.source).Pointer()
	cache.mu.Unlock()
	assert.Equal(t, sourceAddress, retainedAddress)
	runtime.KeepAlive(entry)
}

func TestResourceItemCacheIsRenderScoped(t *testing.T) {
	item := resourceItemCacheTestItem("route-a")
	left, err := resourceItemCacheTestWrapper(NewResourceItemCache(), "routes").wrap(templating.WithImmutableResourceInputs(t.Context()), item)
	require.NoError(t, err)
	right, err := resourceItemCacheTestWrapper(NewResourceItemCache(), "routes").wrap(templating.WithImmutableResourceInputs(t.Context()), item)
	require.NoError(t, err)
	assert.NotEqual(t, left.Pointer(), right.Pointer())
}

func TestResourceItemCacheRejectsTypedMutationBeforeReuse(t *testing.T) {
	cache := NewResourceItemCache()
	source := map[string]any{
		"metadata": map[string]any{"namespace": "default", "name": "route"},
		"spec":     map[string]any{"value": "original"},
	}
	view := &resourceItemCacheView{
		source: source,
		cache:  cache,
	}
	item := map[string]any{}
	props := map[string]any{}
	renderSubject := map[string]any{"mode": "reconcile"}
	parent := templating.WithIncrementalImmutableInputs(t.Context(), item, props, renderSubject)
	declarationValue := resourceItemCacheTestSurface(t, parent, view, false)
	declaration := reflect.Zero(reflect.TypeOf(declarationValue)).Interface()
	templating.RegisterIncrementalResourceDeclaration(declaration)
	engine, err := templating.New(map[string]string{
		"mutate": `{%% value := resources.routes.GetSingle("default", "route"); value.Spec.Value = "poison" %%}`,
		"read":   `{%% value := resources.routes.GetSingle("default", "route") %%}{{ value.Spec.Value }}`,
	}, &templating.Options{
		EntryPoints:            []string{"mutate", "read"},
		IncrementalEntryPoints: []string{"mutate", "read"},
		Declarations:           map[string]any{"resources": declaration},
	})
	require.NoError(t, err)

	render := func(template string) (string, error) {
		resources := resourceItemCacheTestSurface(t, parent, view, true)
		componentCtx := templating.WithIncrementalImmutableInputs(parent, resources)
		return engine.RenderIncrementalComponent(componentCtx, template, map[string]any{
			"source":        "routes",
			"item":          item,
			"props":         props,
			"renderSubject": renderSubject,
			"resources":     resources,
			"shared":        templating.NewSharedContributionContext(&resourceItemCacheSharedRecorder{}),
		})
	}

	_, err = render("mutate")
	require.ErrorContains(t, err, "mutates an immutable input")
	assert.Equal(t, "original", source["spec"].(map[string]any)["value"])
	output, err := render("read")
	require.NoError(t, err)
	assert.Equal(t, "original", output)
	cache.mu.Lock()
	assert.Len(t, cache.entries, 1)
	cache.mu.Unlock()
}

func TestResourceItemCacheRejectsPoisonedEntry(t *testing.T) {
	tests := []struct {
		name   string
		poison func(*wrappedResourceItem)
	}{
		{
			name: "seal",
			poison: func(entry *wrappedResourceItem) {
				entry.seal = nil
			},
		},
		{
			name: "key",
			poison: func(entry *wrappedResourceItem) {
				entry.key.resourceName = "other-routes"
			},
		},
		{
			name: "source",
			poison: func(entry *wrappedResourceItem) {
				entry.source = resourceItemCacheTestItem("route-a")
			},
		},
		{
			name: "typed value",
			poison: func(entry *wrappedResourceItem) {
				value := &resourceItemCacheAlternateFixture{}
				entry.value = reflect.ValueOf(value)
				entry.certificate = templating.CertifyIncrementalImmutableInputs(value)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cache := NewResourceItemCache()
			wrapper := resourceItemCacheTestWrapper(cache, "routes")
			item := resourceItemCacheTestItem("route-a")
			_, err := wrapper.wrap(templating.WithImmutableResourceInputs(t.Context()), item)
			require.NoError(t, err)
			key, ok := resourceItemKey(wrapper.resourceName, wrapper.elemType, item)
			require.True(t, ok)

			cache.mu.Lock()
			test.poison(cache.entries[key])
			cache.mu.Unlock()

			_, err = wrapper.wrap(templating.WithImmutableResourceInputs(t.Context()), item)
			require.ErrorContains(t, err, "invalid provenance")
		})
	}
}

func TestResourceItemCacheConcurrentExactReuse(t *testing.T) {
	cache := NewResourceItemCache()
	item := resourceItemCacheTestItem("route-a")
	const workers = 64
	results := make(chan uintptr, workers)
	errors := make(chan error, workers)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			value, err := resourceItemCacheTestWrapper(cache, "routes").wrap(templating.WithImmutableResourceInputs(t.Context()), item)
			if err != nil {
				errors <- err
				return
			}
			results <- value.Pointer()
		}()
	}
	group.Wait()
	close(results)
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	var expected uintptr
	for actual := range results {
		if expected == 0 {
			expected = actual
		}
		assert.Equal(t, expected, actual)
	}
}

func resourceItemCacheTestWrapper(cache *ResourceItemCache, resourceName string) *resourceItemWrapper {
	return &resourceItemWrapper{
		elemType: reflect.TypeFor[resourceItemCacheFixture](), resourceName: resourceName, cache: cache,
	}
}

func resourceItemCacheTestItem(name string) map[string]any {
	return map[string]any{"metadata": map[string]any{"name": name}}
}

func resourceItemCacheTestSurface(
	t *testing.T,
	ctx context.Context,
	view StoreSnapshotView,
	incremental bool,
) any {
	t.Helper()
	build := BuildResourcesValueWithViews
	if incremental {
		build = BuildIncrementalResourcesValueWithViews
	}
	return build(
		ctx,
		map[string]stores.Store{"routes": k8sstore.NewMemoryStore(2)},
		map[string]reflect.Type{"routes": reflect.TypeFor[resourceItemCacheTemplateResource]()},
		[]string{"routes"},
		func(string) []string { return []string{"metadata.namespace", "metadata.name"} },
		func(string) bool { return false },
		func(string) string { return "cache.test/v1" }, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		NewResourceErrorCollector(),
		view,
		nil,
		false,
	)
}
