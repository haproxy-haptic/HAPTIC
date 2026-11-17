// Copyright 2025 Philipp Hossner
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
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores/storetest"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

func TestNewBuilder(t *testing.T) {
	cfg := &config.Config{}
	pathResolver := &templating.PathResolver{}
	logger := testutil.NewTestLogger()

	builder := NewBuilder(t.Context(), cfg, pathResolver, logger)

	require.NotNil(t, builder)
	assert.Equal(t, cfg, builder.config)
	assert.Equal(t, pathResolver, builder.pathResolver)
	assert.Equal(t, logger, builder.logger)
}

func TestBuilder_WithOptions(t *testing.T) {
	cfg := &config.Config{}
	pathResolver := &templating.PathResolver{}
	logger := testutil.NewTestLogger()

	storeMap := map[string]stores.Store{
		"ingresses": &storetest.MockStore{},
	}
	haproxyPodStore := &storetest.MockStore{}

	builder := NewBuilder(
		t.Context(),
		cfg,
		pathResolver,
		logger,
		WithStores(storeMap),
		WithHAProxyPodStore(haproxyPodStore),
	)

	assert.NotNil(t, builder.stores)
	assert.Equal(t, 1, len(builder.stores))
	assert.NotNil(t, builder.haproxyPodStore)
}

func TestBuilder_Build_BasicContext(t *testing.T) {
	cfg := &config.Config{
		TemplateSnippets: map[string]config.TemplateSnippet{
			"snippet-b": {},
			"snippet-a": {},
		},
	}
	pathResolver := &templating.PathResolver{
		MapsDir: "/etc/haproxy/maps",
	}
	logger := testutil.NewTestLogger()

	builder := NewBuilder(t.Context(), cfg, pathResolver, logger)
	res := builder.Build()
	ctx := res.Context

	require.NotNil(t, ctx)
	require.NotNil(t, res.FileRegistry)
	require.NotNil(t, res.StatusPatchCollector)
	require.NotNil(t, res.RenderedResourceCollector)

	// Check required keys exist
	assert.Contains(t, ctx, "resources")
	assert.Contains(t, ctx, "controller")
	assert.Contains(t, ctx, "templateSnippets")
	assert.Contains(t, ctx, "fileRegistry")
	assert.Contains(t, ctx, "statusPatchCollector")
	assert.Contains(t, ctx, "renderedResourceCollector")
	assert.Contains(t, ctx, "pathResolver")
	assert.Contains(t, ctx, "shared")
	assert.Contains(t, ctx, "runtimeEnvironment")
	assert.Contains(t, ctx, "extraContext")

	// Check snippets are sorted
	snippets := ctx["templateSnippets"].([]string)
	require.Len(t, snippets, 2)
	assert.Equal(t, "snippet-a", snippets[0])
	assert.Equal(t, "snippet-b", snippets[1])
}

func TestBuilder_Build_WithStores(t *testing.T) {
	cfg := &config.Config{
		// WatchedResources is the SOLE source of truth for which
		// fields end up on the `resources` struct — mirrors what
		// typebootstrap.BuildEngineDeclarations iterated. Tests
		// must declare the names they want to assert on; stray
		// stores not listed here are deliberately ignored (see
		// BuildResourcesValue and the prod path comment in
		// renderer.RenderService.buildRenderingContext).
		WatchedResources: map[string]config.WatchedResource{
			"ingresses": {},
			"services":  {},
		},
	}
	pathResolver := &templating.PathResolver{}
	logger := testutil.NewTestLogger()

	storeMap := map[string]stores.Store{
		"ingresses": &storetest.MockStore{},
		"services":  &storetest.MockStore{},
	}

	builder := NewBuilder(t.Context(), cfg, pathResolver, logger, WithStores(storeMap))
	ctx := builder.Build().Context

	// The legacy map[string]templating.ResourceStore fallback was
	// removed alongside the engine's default `resources` declaration
	// (see filters_scriggo.go::registerScriggoRuntimeVars). Even
	// without typed resource types, Build now emits the typed
	// `*resources struct{...}` shape — every consumer goes through
	// this single path, matching what the engine declaration
	// produced by helpers.BuildAdditionalDeclarations expects.
	resourcesVal := ctx["resources"]
	require.NotNil(t, resourcesVal, "resources global must be populated")
	rv := reflect.ValueOf(resourcesVal)
	require.Equal(t, reflect.Ptr, rv.Kind(), "resources global is a pointer to the dynamic struct")
	resourcesStruct := rv.Elem()
	require.Equal(t, reflect.Struct, resourcesStruct.Kind())
	require.Equal(t, 2, resourcesStruct.NumField(), "one field per watched resource")

	fieldNames := make(map[string]bool, resourcesStruct.NumField())
	for i := 0; i < resourcesStruct.NumField(); i++ {
		fieldNames[resourcesStruct.Type().Field(i).Name] = true
	}
	assert.True(t, fieldNames["Ingresses"], "field name follows typegen.GoFieldName")
	assert.True(t, fieldNames["Services"], "field name follows typegen.GoFieldName")
}

// TestBuilder_Build_WithTypedResources covers the typed path: when
// typebootstrap has produced reflect.Types for some resources, the
// builder emits the dynamic *Resources struct (one field per watched
// resource) matching what BuildEngineDeclarations declared. This is
// the chart-render path used in production.
func TestBuilder_Build_WithTypedResources(t *testing.T) {
	cfg := &config.Config{
		WatchedResources: map[string]config.WatchedResource{
			"ingresses": {},
		},
	}
	pathResolver := &templating.PathResolver{}
	logger := testutil.NewTestLogger()

	storeMap := map[string]stores.Store{
		"ingresses": &storetest.MockStore{},
	}
	typedTypes := map[string]reflect.Type{
		"ingresses": reflect.StructOf([]reflect.StructField{
			{Name: "Metadata", Type: reflect.StructOf([]reflect.StructField{
				{Name: "Name", Type: reflect.TypeOf("")},
			})},
		}),
	}

	builder := NewBuilder(t.Context(), cfg, pathResolver, logger,
		WithStores(storeMap),
		WithTypedResources(typedTypes),
	)
	ctx := builder.Build().Context

	resourcesVal := ctx["resources"]
	require.NotNil(t, resourcesVal, "resources global must be populated")
	rv := reflect.ValueOf(resourcesVal)
	require.Equal(t, reflect.Ptr, rv.Kind(),
		"resources global is a pointer to the dynamic struct")
	resourcesStruct := rv.Elem()
	require.Equal(t, reflect.Struct, resourcesStruct.Kind())
	require.Equal(t, 1, resourcesStruct.NumField(),
		"one field per watched resource")
	assert.Equal(t, "Ingresses", resourcesStruct.Type().Field(0).Name,
		"field name follows Go-PascalCase rule (typegen.GoFieldName)")
}

func TestBuilder_Build_WithHAProxyPodStore(t *testing.T) {
	cfg := &config.Config{}
	pathResolver := &templating.PathResolver{}
	logger := testutil.NewTestLogger()

	haproxyPodStore := &storetest.MockStore{}

	builder := NewBuilder(t.Context(), cfg, pathResolver, logger, WithHAProxyPodStore(haproxyPodStore))
	ctx := builder.Build().Context

	controller := ctx["controller"].(map[string]templating.ResourceStore)
	require.Len(t, controller, 1)
	assert.Contains(t, controller, "haproxy_pods")
}

func TestBuilder_Build_WithExtraContext(t *testing.T) {
	cfg := &config.Config{
		TemplatingSettings: config.TemplatingSettings{
			ExtraContext: map[string]any{
				"debug": map[string]any{
					"enabled": true,
				},
				"version": "1.0",
			},
		},
	}
	pathResolver := &templating.PathResolver{}
	logger := testutil.NewTestLogger()

	builder := NewBuilder(t.Context(), cfg, pathResolver, logger)
	ctx := builder.Build().Context

	// Check extraContext map is populated
	extraContext := ctx["extraContext"].(map[string]any)
	assert.Equal(t, "1.0", extraContext["version"])

	// Check values are merged to top level
	assert.Contains(t, ctx, "debug")
	assert.Contains(t, ctx, "version")
}

func TestBuilder_Build_WithCurrentAuxFiles(t *testing.T) {
	cfg := &config.Config{}
	pathResolver := &templating.PathResolver{}
	logger := testutil.NewTestLogger()

	// With files: exposed under "currentFiles" as a *map[string]string
	// (Scriggo pointer decl) that derefs to the provided map.
	files := map[string]string{"tls-ticket-keys": "line1\nline2\nline3"}
	ctx := NewBuilder(t.Context(), cfg, pathResolver, logger, WithCurrentAuxFiles(files)).Build().Context
	got, ok := ctx["currentFiles"].(*map[string]string)
	require.True(t, ok, "currentFiles must be *map[string]string, got %T", ctx["currentFiles"])
	assert.Equal(t, files, *got)

	// Without the option: still non-nil (empty) so templates index it
	// without a nil guard.
	ctxEmpty := NewBuilder(t.Context(), cfg, pathResolver, logger).Build().Context
	gotEmpty, ok := ctxEmpty["currentFiles"].(*map[string]string)
	require.True(t, ok)
	assert.NotNil(t, *gotEmpty)
	assert.Empty(t, *gotEmpty)
}

func TestWithCurrentAuxFilesIsolatesEachRenderingContext(t *testing.T) {
	files := map[string]string{"gate": "published"}
	option := WithCurrentAuxFiles(files)
	files["gate"] = "caller-mutated"

	first := NewBuilder(t.Context(), &config.Config{}, &templating.PathResolver{}, testutil.NewTestLogger(), option).Build().Context
	firstFiles := first["currentFiles"].(*map[string]string)
	assert.Equal(t, "published", (*firstFiles)["gate"])
	(*firstFiles)["gate"] = "template-mutated"

	second := NewBuilder(t.Context(), &config.Config{}, &templating.PathResolver{}, testutil.NewTestLogger(), option).Build().Context
	secondFiles := second["currentFiles"].(*map[string]string)
	assert.Equal(t, "published", (*secondFiles)["gate"])
}

func TestWithCurrentConfigIsolatesEachRenderingContext(t *testing.T) {
	port := int64(8080)
	current := &renderplan.CurrentConfig{ServerIndex: map[string]map[string]renderplan.ServerAddr{
		"backend": {"server": {Address: "192.0.2.1", Port: &port}},
	}}
	option := WithCurrentConfig(current)
	current.ServerIndex["backend"]["server"] = renderplan.ServerAddr{Address: "caller-mutated"}

	first := NewBuilder(t.Context(), &config.Config{}, &templating.PathResolver{}, testutil.NewTestLogger(), option).Build().Context
	firstCurrent := first["currentConfig"].(*renderplan.CurrentConfig)
	server := firstCurrent.ServerIndex["backend"]["server"]
	assert.Equal(t, "192.0.2.1", server.Address)
	*server.Port = 9000
	firstCurrent.ServerIndex["backend"]["server"] = renderplan.ServerAddr{Address: "template-mutated"}

	second := NewBuilder(t.Context(), &config.Config{}, &templating.PathResolver{}, testutil.NewTestLogger(), option).Build().Context
	secondServer := second["currentConfig"].(*renderplan.CurrentConfig).ServerIndex["backend"]["server"]
	assert.Equal(t, "192.0.2.1", secondServer.Address)
	assert.Equal(t, int64(8080), *secondServer.Port)
}

func TestBuilder_Build_CurrentFilesCannotBeOverriddenByExtraContext(t *testing.T) {
	cfg := &config.Config{TemplatingSettings: config.TemplatingSettings{
		ExtraContext: map[string]any{
			"currentFiles": map[string]string{"gate": "override"},
		},
	}}
	files := map[string]string{"gate": "authoritative"}

	ctx := NewBuilder(
		t.Context(),
		cfg,
		&templating.PathResolver{},
		testutil.NewTestLogger(),
		WithCurrentAuxFiles(files),
	).Build().Context

	got, ok := ctx["currentFiles"].(*map[string]string)
	require.True(t, ok)
	assert.Equal(t, files, *got)
}

func TestSortSnippetNames(t *testing.T) {
	tests := []struct {
		name     string
		snippets map[string]config.TemplateSnippet
		want     []string
	}{
		{
			name:     "empty",
			snippets: map[string]config.TemplateSnippet{},
			want:     []string{},
		},
		{
			name: "already sorted",
			snippets: map[string]config.TemplateSnippet{
				"a": {},
				"b": {},
				"c": {},
			},
			want: []string{"a", "b", "c"},
		},
		{
			name: "reverse order",
			snippets: map[string]config.TemplateSnippet{
				"z": {},
				"m": {},
				"a": {},
			},
			want: []string{"a", "m", "z"},
		},
		{
			name: "with priority prefixes",
			snippets: map[string]config.TemplateSnippet{
				"features-100-ssl":     {},
				"features-050-logging": {},
				"features-200-waf":     {},
			},
			want: []string{"features-050-logging", "features-100-ssl", "features-200-waf"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SortSnippetNames(tt.snippets)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestMergeExtraContextInto(t *testing.T) {
	t.Run("nil extraContext", func(t *testing.T) {
		cfg := &config.Config{}
		renderCtx := make(map[string]any)

		MergeExtraContextInto(renderCtx, cfg)

		// Should create empty extraContext to prevent nil dereference
		assert.Contains(t, renderCtx, "extraContext")
		extraContext := renderCtx["extraContext"].(map[string]any)
		assert.Empty(t, extraContext)
	})

	t.Run("with extraContext", func(t *testing.T) {
		cfg := &config.Config{
			TemplatingSettings: config.TemplatingSettings{
				ExtraContext: map[string]any{
					"key1": "value1",
					"key2": 42,
				},
			},
		}
		renderCtx := make(map[string]any)

		MergeExtraContextInto(renderCtx, cfg)

		// Check top-level merge
		assert.Equal(t, "value1", renderCtx["key1"])
		assert.Equal(t, 42, renderCtx["key2"])

		// Check extraContext map
		extraContext := renderCtx["extraContext"].(map[string]any)
		assert.Equal(t, "value1", extraContext["key1"])
	})
}

func TestBuilder_Build_AdmissionSubject(t *testing.T) {
	pathResolver := &templating.PathResolver{}
	logger := testutil.NewTestLogger()

	t.Run("unset yields empty map", func(t *testing.T) {
		builder := NewBuilder(t.Context(), &config.Config{}, pathResolver, logger)
		ctx := builder.Build().Context

		subject, ok := ctx["admissionSubject"].(map[string]any)
		require.True(t, ok, "admissionSubject must always be a map")
		assert.Empty(t, subject)
	})

	t.Run("set exposes store, namespace, name", func(t *testing.T) {
		builder := NewBuilder(t.Context(), &config.Config{}, pathResolver, logger,
			WithAdmissionSubject("ingresses", "team-a", "app"))
		ctx := builder.Build().Context

		subject := ctx["admissionSubject"].(map[string]any)
		assert.Equal(t, "ingresses", subject["store"])
		assert.Equal(t, map[string]any{"ingresses": true}, subject["stores"])
		assert.Equal(t, "team-a", subject["namespace"])
		assert.Equal(t, "app", subject["name"])
	})

	t.Run("multiple aliases expose the complete store set", func(t *testing.T) {
		builder := NewBuilder(t.Context(), &config.Config{}, pathResolver, logger,
			WithAdmissionSubjectStores([]string{"internal-routes", "public-routes"}, "team-a", "app"))
		subject := builder.Build().Context["admissionSubject"].(map[string]any)

		assert.Empty(t, subject["store"])
		assert.Equal(t, map[string]any{"internal-routes": true, "public-routes": true}, subject["stores"])
		assert.Equal(t, "team-a", subject["namespace"])
		assert.Equal(t, "app", subject["name"])
	})

	t.Run("extraContext cannot spoof the subject", func(t *testing.T) {
		cfg := &config.Config{
			TemplatingSettings: config.TemplatingSettings{
				ExtraContext: map[string]any{
					"admissionSubject": map[string]any{
						"store": "ingresses", "namespace": "evil", "name": "spoof",
					},
				},
			},
		}
		builder := NewBuilder(t.Context(), cfg, pathResolver, logger)
		ctx := builder.Build().Context

		subject := ctx["admissionSubject"].(map[string]any)
		assert.Empty(t, subject, "user extraContext must not set the admission subject")
	})
}

// TestBuildResourcesValue_MemoizedTypedPointers pins the invariant the
// generic governance layer relies on: within one render, List/Fetch/GetSingle
// return the SAME *T for the same snapshot item (so a template write to a field
// — a governance injection — is observed by every later read), while a fresh
// render re-wraps from the immutable store snapshot (so the write is gone).
func TestBuildResourcesValue_MemoizedTypedPointers(t *testing.T) {
	logger := testutil.NewTestLogger()

	metaType := reflect.StructOf([]reflect.StructField{
		{Name: "Name", Type: reflect.TypeOf(""), Tag: `json:"name"`},
		{Name: "Namespace", Type: reflect.TypeOf(""), Tag: `json:"namespace"`},
		{Name: "Annotations", Type: reflect.TypeOf(map[string]string(nil)), Tag: `json:"annotations,omitempty"`},
	})
	elemType := reflect.StructOf([]reflect.StructField{
		{Name: "Metadata", Type: metaType, Tag: `json:"metadata"`},
	})

	item := map[string]any{"metadata": map[string]any{"name": "app", "namespace": "ns"}}
	indexBy := func(string) []string { return []string{"metadata.namespace", "metadata.name"} }

	// newRender builds a fresh resources struct (fresh memo) over the same
	// immutable store item, returning the per-resource inner store struct.
	newRender := func() reflect.Value {
		storeMap := map[string]stores.Store{"ingresses": &storetest.MockStore{Items: []any{item}}}
		typedTypes := map[string]reflect.Type{"ingresses": elemType}
		res := BuildResourcesValue(
			templating.WithImmutableResourceInputs(t.Context()),
			storeMap, typedTypes, []string{"ingresses"}, indexBy, nil, nil, logger,
		)
		return reflect.ValueOf(res).Elem().Field(0).Elem()
	}
	list := func(inner reflect.Value) reflect.Value { return inner.FieldByName("List").Call(nil)[0] }
	name := func(ptr reflect.Value) string {
		return ptr.Elem().FieldByName("Metadata").FieldByName("Name").String()
	}

	inner := newRender()

	// Within one render, List() returns the same *T each call.
	l1, l2 := list(inner), list(inner)
	require.Equal(t, 1, l1.Len())
	require.Equal(t, l1.Index(0).Pointer(), l2.Index(0).Pointer(),
		"List() must return the same *T for the same snapshot item within a render")

	// GetSingle shares the memo — the map snippets read via GetSingle must see
	// a write the governance snippet made while iterating List().
	single := inner.FieldByName("GetSingle").CallSlice(
		[]reflect.Value{reflect.ValueOf([]any{"ns", "app"})})[0]
	require.Equal(t, l1.Index(0).Pointer(), single.Pointer(),
		"GetSingle must return the same *T as List() within a render")

	// A write to the memoized *T persists across later reads in the same render.
	l1.Index(0).Elem().FieldByName("Metadata").FieldByName("Name").SetString("mutated")
	assert.Equal(t, "mutated", name(list(inner).Index(0)),
		"a write to a memoized *T is observed by a later List() in the same render")

	// A fresh render re-wraps from the untouched snapshot: the write is gone.
	assert.Equal(t, "app", name(list(newRender()).Index(0)),
		"cross-render isolation: a new render sees the original store value")

	// Concurrent List() calls exercise the memo mutex (run with -race).
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = list(inner) }()
	}
	wg.Wait()
}

func TestBuilder_Build_SensitiveFilesStayOutOfCurrentFiles(t *testing.T) {
	cfg := &config.Config{TemplatingSettings: config.TemplatingSettings{
		ExtraContext: map[string]any{"currentSensitiveFiles": map[string]string{"k": "override"}},
	}}
	previous := map[string]string{"gate": "plain", dataplane.SensitiveFileKeyPrefix + "k": "secret"}
	ctx := NewBuilder(t.Context(), cfg, &templating.PathResolver{}, testutil.NewTestLogger(),
		WithCurrentAuxFiles(previous)).Build().Context

	files, ok := ctx["currentFiles"].(*map[string]string)
	require.True(t, ok)
	assert.Equal(t, map[string]string{"gate": "plain"}, *files)
	sensitive, ok := ctx["currentSensitiveFiles"].(*map[string]string)
	require.True(t, ok)
	assert.Equal(t, map[string]string{"k": "secret"}, *sensitive)
}
