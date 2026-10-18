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

package typebootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/schemafetcher"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/typegen"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

// silentLogger returns a discarding *slog.Logger. Bootstrap requires
// a non-nil logger; tests don't want warnings cluttering output.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// gatewaySchemaSeed is the minimum schema shape needed to verify
// type generation end-to-end through bootstrap. It mirrors what
// pkg/k8s/schemafetcher would return for a real Gateway CRD.
func gatewaySchemaSeed() *spec.Schema {
	return &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: spec.StringOrArray{"object"},
			Properties: map[string]spec.Schema{
				"apiVersion": {SchemaProps: spec.SchemaProps{Type: spec.StringOrArray{"string"}}},
				"kind":       {SchemaProps: spec.SchemaProps{Type: spec.StringOrArray{"string"}}},
				"metadata": {SchemaProps: spec.SchemaProps{
					Type: spec.StringOrArray{"object"},
					Properties: map[string]spec.Schema{
						"name":      {SchemaProps: spec.SchemaProps{Type: spec.StringOrArray{"string"}}},
						"namespace": {SchemaProps: spec.SchemaProps{Type: spec.StringOrArray{"string"}}},
					},
				}},
				"spec": {SchemaProps: spec.SchemaProps{
					Type: spec.StringOrArray{"object"},
					Properties: map[string]spec.Schema{
						"gatewayClassName": {SchemaProps: spec.SchemaProps{Type: spec.StringOrArray{"string"}}},
					},
				}},
			},
		},
	}
}

func gatewayGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group: "gateway.networking.k8s.io", Version: "v1", Kind: "Gateway",
	}
}

func TestBootstrap_HappyPath(t *testing.T) {
	fetcher := schemafetcher.NewMapFetcher(map[schema.GroupVersionKind]*spec.Schema{
		gatewayGVK(): gatewaySchemaSeed(),
	})

	result, err := Bootstrap(t.Context(), Config{
		Resources: []Resource{
			{Name: "gateways", GVK: gatewayGVK()},
		},
		Fetcher: fetcher,
		Logger:  silentLogger(),
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Empty(t, result.Errors)
	require.Len(t, result.Types, 1)

	gwType, ok := result.Types["gateways"]
	require.True(t, ok)
	require.Equal(t, reflect.Struct, gwType.Kind())

	// Walking the type confirms the fetched schema flowed through
	// the converter unchanged.
	metaField, ok := gwType.FieldByName("Metadata")
	require.True(t, ok)
	require.Equal(t, reflect.Struct, metaField.Type.Kind())
	_, ok = metaField.Type.FieldByName("Namespace")
	require.True(t, ok)
	_, ok = metaField.Type.FieldByName("Name")
	require.True(t, ok)
}

// TestBootstrap_FailClosedOnFetcherError pins the contract that
// any resource's schema-acquisition failure fails the whole
// bootstrap. Template authors using typed access (gw.Spec.X,
// route.Status.Y) rely on every declared watched resource
// resolving to its real schema; silently degrading a subset to
// envelope-only typed access would break those templates without
// surfacing the root cause (RBAC, CRD installation, apiserver
// health). Result.Errors records the per-resource cause for debug
// surfaces; Result.Types is left in whatever partial state the
// loop reached.
// TestBootstrap_InjectsObjectMetaWhenCRDLeavesItEmpty pins the
// K8s convention fix-up that landed in Phase 11: CRD-backed
// resources commonly declare `metadata: {type: object}` with no
// properties (apiserver auto-validates ObjectMeta), so without
// pre-processing the converter degrades the metadata field to
// interface{} and chart code reaching `gw.Metadata.Name` fails
// at engine-compile time.
//
// Asserts the generated type has Metadata as a typed struct with
// the universal ObjectMeta fields, regardless of what the source
// schema declared for metadata.
func TestBootstrap_InjectsObjectMetaWhenCRDLeavesItEmpty(t *testing.T) {
	// Schema mimics a CRD's openAPIV3Schema: spec is detailed,
	// metadata is an empty-properties object (the K8s norm for
	// CRDs because the apiserver injects ObjectMeta validation).
	gvk := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"}
	bareMetadataSchema := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: spec.StringOrArray{"object"},
			Properties: map[string]spec.Schema{
				"apiVersion": {SchemaProps: spec.SchemaProps{Type: spec.StringOrArray{"string"}}},
				"kind":       {SchemaProps: spec.SchemaProps{Type: spec.StringOrArray{"string"}}},
				"metadata": {SchemaProps: spec.SchemaProps{
					Type: spec.StringOrArray{"object"},
				}},
				"spec": {SchemaProps: spec.SchemaProps{
					Type: spec.StringOrArray{"object"},
					Properties: map[string]spec.Schema{
						"size": {SchemaProps: spec.SchemaProps{Type: spec.StringOrArray{"integer"}}},
					},
				}},
			},
		},
	}
	fetcher := schemafetcher.NewMapFetcher(map[schema.GroupVersionKind]*spec.Schema{
		gvk: bareMetadataSchema,
	})

	result, err := Bootstrap(context.Background(), Config{
		Resources: []Resource{{Name: "widgets", GVK: gvk}},
		Fetcher:   fetcher,
		Logger:    silentLogger(),
	})
	require.NoError(t, err)
	require.Empty(t, result.Errors)

	widgetType, ok := result.Types["widgets"]
	require.True(t, ok)

	meta, ok := widgetType.FieldByName("Metadata")
	require.True(t, ok, "Widget must have a Metadata field")
	require.Equal(t, reflect.Struct, meta.Type.Kind(),
		"Metadata MUST be a typed struct, NOT interface{} — the chart's gw.Metadata.Name access pattern depends on it")

	// The universal ObjectMeta fields chart libraries touch.
	for _, want := range []string{"Name", "Namespace", "Uid", "ResourceVersion", "Labels", "Annotations"} {
		_, ok := meta.Type.FieldByName(want)
		assert.True(t, ok, "synthetic ObjectMeta must include %s", want)
	}

	wrapped, err := typegen.WrapInto(map[string]any{
		"apiVersion": "example.com/v1",
		"kind":       "Widget",
		"metadata": map[string]any{
			"name":            "source",
			"namespace":       "default",
			"uid":             "f6baf710-c079-437f-9cca-259b058e0944",
			"resourceVersion": "27",
		},
	}, widgetType)
	require.NoError(t, err)
	wrappedMetadata := wrapped.FieldByName("Metadata")
	assert.Equal(t, "f6baf710-c079-437f-9cca-259b058e0944", wrappedMetadata.FieldByName("Uid").String())
	assert.Equal(t, "27", wrappedMetadata.FieldByName("ResourceVersion").String())

	resourcePointer := reflect.New(widgetType)
	resourcePointer.Elem().Set(wrapped)
	engine, err := templating.New(map[string]string{
		"main": `{% statusPatch(resource, map[string]any{"rendered": map[string]any{"accepted": true}}) %}`,
	}, &templating.Options{
		EntryPoints: []string{"main"},
		Declarations: map[string]any{
			"resource": reflect.Zero(reflect.PointerTo(widgetType)).Interface(),
		},
	})
	require.NoError(t, err)
	collector := templating.NewStatusPatchCollector()
	output, err := engine.Render(t.Context(), "main", map[string]any{
		"resource":             resourcePointer.Interface(),
		"statusPatchCollector": collector,
	})
	require.NoError(t, err)
	assert.Empty(t, strings.TrimSpace(output))
	patches, err := collector.Patches()
	require.NoError(t, err)
	require.Len(t, patches, 1)
	assert.Equal(t, "default", patches[0].Namespace)
	assert.Equal(t, "source", patches[0].Name)
	assert.Equal(t, "example.com/v1", patches[0].APIVersion)
	assert.Equal(t, "Widget", patches[0].Kind)
	assert.Equal(t, "f6baf710-c079-437f-9cca-259b058e0944", patches[0].UID)
	assert.Equal(t, "27", patches[0].ResourceVersion)

	// Spec stays as the CRD declared it — the pre-process only
	// touches metadata.
	specType, ok := widgetType.FieldByName("Spec")
	require.True(t, ok)
	_, ok = specType.Type.FieldByName("Size")
	assert.True(t, ok, "CRD-declared spec fields must survive the pre-process untouched")
}

func TestBootstrap_AddsLineageToPartialObjectMeta(t *testing.T) {
	gvk := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"}
	partialMetadataSchema := &spec.Schema{SchemaProps: spec.SchemaProps{
		Type: spec.StringOrArray{"object"},
		Properties: map[string]spec.Schema{
			"metadata": {SchemaProps: spec.SchemaProps{
				Type: spec.StringOrArray{"object"},
				Properties: map[string]spec.Schema{
					"name": {SchemaProps: spec.SchemaProps{Type: spec.StringOrArray{"string"}}},
				},
			}},
		},
	}}
	fetcher := schemafetcher.NewMapFetcher(map[schema.GroupVersionKind]*spec.Schema{gvk: partialMetadataSchema})

	result, err := Bootstrap(t.Context(), Config{
		Resources: []Resource{{Name: "widgets", GVK: gvk}},
		Fetcher:   fetcher,
		Logger:    silentLogger(),
	})
	require.NoError(t, err)
	widgetType := result.Types["widgets"]
	metadataType, ok := widgetType.FieldByName("Metadata")
	require.True(t, ok)
	_, hasName := metadataType.Type.FieldByName("Name")
	_, hasUID := metadataType.Type.FieldByName("Uid")
	_, hasResourceVersion := metadataType.Type.FieldByName("ResourceVersion")
	assert.True(t, hasName)
	assert.True(t, hasUID)
	assert.True(t, hasResourceVersion)
	originalMetadata := partialMetadataSchema.Properties["metadata"]
	assert.NotContains(t, originalMetadata.Properties, "uid")
	assert.NotContains(t, originalMetadata.Properties, "resourceVersion")
}

func TestBootstrap_AddsObjectMetaWhenSchemaOmitsIt(t *testing.T) {
	gvk := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Widget"}
	schemaWithoutMetadata := &spec.Schema{SchemaProps: spec.SchemaProps{
		Type: spec.StringOrArray{"object"},
		Properties: map[string]spec.Schema{
			"spec": {SchemaProps: spec.SchemaProps{Type: spec.StringOrArray{"object"}}},
		},
	}}
	fetcher := schemafetcher.NewMapFetcher(map[schema.GroupVersionKind]*spec.Schema{gvk: schemaWithoutMetadata})

	result, err := Bootstrap(t.Context(), Config{
		Resources: []Resource{{Name: "widgets", GVK: gvk}},
		Fetcher:   fetcher,
		Logger:    silentLogger(),
	})
	require.NoError(t, err)
	metadataType, ok := result.Types["widgets"].FieldByName("Metadata")
	require.True(t, ok)
	_, hasName := metadataType.Type.FieldByName("Name")
	_, hasUID := metadataType.Type.FieldByName("Uid")
	_, hasResourceVersion := metadataType.Type.FieldByName("ResourceVersion")
	assert.True(t, hasName)
	assert.True(t, hasUID)
	assert.True(t, hasResourceVersion)
	assert.NotContains(t, schemaWithoutMetadata.Properties, "metadata")
}

func TestBootstrap_FailClosedOnFetcherError(t *testing.T) {
	// Only "gateways" has a schema; the second resource will fail
	// the fetch via schemafetcher's MapFetcher (returns
	// ErrSchemaNotAvailable for misses).
	fetcher := schemafetcher.NewMapFetcher(map[schema.GroupVersionKind]*spec.Schema{
		gatewayGVK(): gatewaySchemaSeed(),
	})

	mysteryGVK := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Mystery"}

	result, err := Bootstrap(t.Context(), Config{
		Resources: []Resource{
			{Name: "gateways", GVK: gatewayGVK()},
			{Name: "mystery", GVK: mysteryGVK},
		},
		Fetcher: fetcher,
		Logger:  silentLogger(),
	})
	require.Error(t, err, "any per-resource schema failure must fail the whole bootstrap")
	require.ErrorContains(t, err, "mystery",
		"hard error must name the failing resource so the operator can investigate")
	require.True(t, schemafetcher.IsNotFound(err),
		"NotFound must propagate through bootstrap's wrap so callers can branch on it")

	// Errors map still records the per-resource cause for debug
	// surfaces (status CRD, log enumeration of which resource broke).
	require.Contains(t, result.Errors, "mystery")
	assert.ErrorContains(t, result.Errors["mystery"], "fetching schema")
}

// barrierFetcher answers no Fetch until want of them are in flight.
type barrierFetcher struct {
	schemafetcher.Fetcher
	want     int32
	arrivals atomic.Int32
	release  chan struct{}
}

func (f *barrierFetcher) Fetch(ctx context.Context, gvk schema.GroupVersionKind) (*spec.Schema, map[string]spec.Schema, error) {
	if f.arrivals.Add(1) == f.want {
		close(f.release)
	}
	select {
	case <-f.release:
		return f.Fetcher.Fetch(ctx, gvk)
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

// Each fetch is an apiserver round trip; resolved one after another, a
// chart's resources cost the sum of them (#410).
func TestBootstrap_ResolvesResourcesConcurrently(t *testing.T) {
	const resources = 8
	schemas := map[schema.GroupVersionKind]*spec.Schema{}
	cfg := Config{Logger: silentLogger()}
	for i := range resources {
		gvk := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: fmt.Sprintf("Kind%d", i)}
		schemas[gvk] = gatewaySchemaSeed()
		cfg.Resources = append(cfg.Resources, Resource{Name: fmt.Sprintf("kind%d", i), GVK: gvk})
	}
	cfg.Fetcher = &barrierFetcher{Fetcher: schemafetcher.NewMapFetcher(schemas), want: resources, release: make(chan struct{})}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, err := Bootstrap(ctx, cfg)
	require.NoError(t, err, "fetching one resource at a time never fills the barrier")
	assert.Len(t, result.Types, resources)
}

func TestBootstrap_IgnoreFieldsMerged(t *testing.T) {
	// Schema where both `spec` and `metadata.managedFields` should
	// be visible by default. Globals strip managedFields; per-
	// resource ignore strips spec. The resulting type must lose
	// BOTH — the per-resource list is additive, not a replacement.
	sch := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: spec.StringOrArray{"object"},
			Properties: map[string]spec.Schema{
				"metadata": {SchemaProps: spec.SchemaProps{
					Type: spec.StringOrArray{"object"},
					Properties: map[string]spec.Schema{
						"name":          {SchemaProps: spec.SchemaProps{Type: spec.StringOrArray{"string"}}},
						"managedFields": {SchemaProps: spec.SchemaProps{Type: spec.StringOrArray{"array"}}},
					},
				}},
				"spec": {SchemaProps: spec.SchemaProps{Type: spec.StringOrArray{"object"}}},
			},
		},
	}
	gvk := schema.GroupVersionKind{Group: "g", Version: "v1", Kind: "K"}
	fetcher := schemafetcher.NewMapFetcher(map[schema.GroupVersionKind]*spec.Schema{
		gvk: sch,
	})

	result, err := Bootstrap(t.Context(), Config{
		GlobalIgnoreFields: []string{"metadata.managedFields"},
		Resources: []Resource{
			{Name: "thing", GVK: gvk, IgnoreFields: []string{"spec"}},
		},
		Fetcher: fetcher,
		Logger:  silentLogger(),
	})
	require.NoError(t, err)
	require.Empty(t, result.Errors)

	typ := result.Types["thing"]
	require.NotNil(t, typ)
	_, ok := typ.FieldByName("Spec")
	assert.False(t, ok, "per-resource ignore must strip Spec")

	meta, ok := typ.FieldByName("Metadata")
	require.True(t, ok)
	_, ok = meta.Type.FieldByName("ManagedFields")
	assert.False(t, ok, "global ignore must strip ManagedFields")
	_, ok = meta.Type.FieldByName("Name")
	assert.True(t, ok, "non-ignored fields must remain")
}

func TestBootstrap_RejectsMissingDependencies(t *testing.T) {
	_, err := Bootstrap(t.Context(), Config{
		// Fetcher omitted on purpose.
		Logger: silentLogger(),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Fetcher is required")

	_, err = Bootstrap(t.Context(), Config{
		Fetcher: schemafetcher.NewMapFetcher(nil),
		// Logger omitted — per-resource degradations need
		// operator visibility, so silent fail-open isn't OK.
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Logger is required")
}

func TestBootstrap_EmptyResourceNameSkipped(t *testing.T) {
	// An empty Name is a programming error in the caller — every
	// chart-side reference uses the name as the resources map
	// key. Skipping with a logged warning is friendlier than
	// failing the whole boot for a malformed entry; the caller
	// can react to the entry being absent from Result.Types.
	result, err := Bootstrap(t.Context(), Config{
		Resources: []Resource{
			{Name: "", GVK: gatewayGVK()},
		},
		Fetcher: schemafetcher.NewMapFetcher(nil),
		Logger:  silentLogger(),
	})
	require.NoError(t, err)
	assert.Empty(t, result.Types)
	require.Contains(t, result.Errors, "")
	assert.ErrorContains(t, result.Errors[""], "empty Name")
}

func TestBootstrap_RespectsContextCancellation(t *testing.T) {
	// A cancelled context mid-loop must stop immediately. Useful
	// when the controller iteration is being torn down (config
	// change, shutdown, leadership loss).
	fetcher := schemafetcher.NewMapFetcher(map[schema.GroupVersionKind]*spec.Schema{
		gatewayGVK(): gatewaySchemaSeed(),
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := Bootstrap(ctx, Config{
		Resources: []Resource{{Name: "gateways", GVK: gatewayGVK()}},
		Fetcher:   fetcher,
		Logger:    silentLogger(),
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
}

func TestMergeIgnoreFields(t *testing.T) {
	cases := []struct {
		name        string
		global, per []string
		want        []string
	}{
		{name: "both empty", want: nil},
		{name: "global only", global: []string{"a", "b"}, want: []string{"a", "b"}},
		{name: "per only", per: []string{"x"}, want: []string{"x"}},
		{name: "both supplied — concat global first", global: []string{"a"}, per: []string{"x"}, want: []string{"a", "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, mergeIgnoreFields(tc.global, tc.per))
		})
	}
}
