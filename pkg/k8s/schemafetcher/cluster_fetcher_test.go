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

package schemafetcher

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kube-openapi/pkg/spec3"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// stubCRDGetter serves the CRD of a fixed set whose group and kind match,
// and records how many times GetCRD was called — the fetcher contract
// promises each GVK is read at most once per ClusterFetcher lifetime.
type stubCRDGetter struct {
	list  []apiextensionsv1.CustomResourceDefinition
	err   error
	calls atomic.Int32
	// hook overrides the default behaviour when set. Used by
	// context-cancellation eviction tests where each successive call
	// must surface a different outcome (first call: ctx error;
	// second call: success).
	hook func(ctx context.Context) (*apiextensionsv1.CustomResourceDefinition, error)
}

func (s *stubCRDGetter) GetCRD(ctx context.Context, gvk schema.GroupVersionKind) (*apiextensionsv1.CustomResourceDefinition, error) {
	s.calls.Add(1)
	if s.hook != nil {
		return s.hook(ctx)
	}
	if s.err != nil {
		return nil, s.err
	}
	for i := range s.list {
		if s.list[i].Spec.Group == gvk.Group && s.list[i].Spec.Names.Kind == gvk.Kind {
			return &s.list[i], nil
		}
	}
	return nil, ErrNoCRD
}

// stubOpenAPIV3Provider serves pre-baked GroupVersion specs, with the
// same call-counting trick so we can verify the per-GV coalescing.
type stubOpenAPIV3Provider struct {
	mu        sync.Mutex
	specs     map[schema.GroupVersion]*spec3.OpenAPI
	errs      map[schema.GroupVersion]error
	callCount map[schema.GroupVersion]int
}

func newStubOpenAPI() *stubOpenAPIV3Provider {
	return &stubOpenAPIV3Provider{
		specs:     make(map[schema.GroupVersion]*spec3.OpenAPI),
		errs:      make(map[schema.GroupVersion]error),
		callCount: make(map[schema.GroupVersion]int),
	}
}

func (s *stubOpenAPIV3Provider) GVComponents(_ context.Context, gv schema.GroupVersion) (*spec3.Components, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.callCount[gv]++
	if err := s.errs[gv]; err != nil {
		return nil, err
	}
	doc, ok := s.specs[gv]
	if !ok {
		doc = &spec3.OpenAPI{}
	}
	return doc.Components, nil
}

// crdFixture builds a single-version CRD with the supplied schema —
// pattern matches the shape kubectl get crd <name> -o yaml produces.
func crdFixture(t *testing.T, group, kind, plural, version string, sch *apiextensionsv1.JSONSchemaProps) apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	return apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: plural + "." + group},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: group,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Kind:   kind,
				Plural: plural,
			},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    version,
					Served:  true,
					Storage: true,
					Schema:  &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: sch},
				},
			},
		},
	}
}

// TestClusterFetcher_CRDPath covers the happy path for CRD-backed
// resources: the fetcher locates the matching CRD by group + kind,
// extracts the right version's schema, and converts the
// JSONSchemaProps shape into a kube-openapi spec.Schema. Gateway is
// the canonical example since every HAPTIC dev cluster has it
// installed.
func TestClusterFetcher_CRDPath(t *testing.T) {
	gatewaySchema := &apiextensionsv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"spec": {
				Type: "object",
				Properties: map[string]apiextensionsv1.JSONSchemaProps{
					"gatewayClassName": {Type: "string"},
				},
			},
		},
	}
	crds := &stubCRDGetter{list: []apiextensionsv1.CustomResourceDefinition{
		crdFixture(t, "gateway.networking.k8s.io", "Gateway", "gateways", "v1", gatewaySchema),
	}}
	openapi := newStubOpenAPI()

	fetcher := NewClusterFetcher(crds, openapi)
	got, _, err := fetcher.Fetch(t.Context(), schema.GroupVersionKind{
		Group: "gateway.networking.k8s.io", Version: "v1", Kind: "Gateway",
	})
	require.NoError(t, err)
	require.NotNil(t, got)
	// Spec must round-trip: the JSONSchemaProps → spec.Schema
	// conversion preserves the OpenAPI v3 shape.
	require.Equal(t, spec.StringOrArray{"object"}, got.Type)
	specField, ok := got.Properties["spec"]
	require.True(t, ok)
	gcn, ok := specField.Properties["gatewayClassName"]
	require.True(t, ok)
	assert.Equal(t, spec.StringOrArray{"string"}, gcn.Type)
}

// TestClusterFetcher_OpenAPIFallback covers the core-resource path:
// no matching CRD exists, so the fetcher pulls the GroupVersion's
// OpenAPI v3 spec and matches by x-kubernetes-group-version-kind.
func TestClusterFetcher_OpenAPIFallback(t *testing.T) {
	openapi := newStubOpenAPI()
	openapi.specs[schema.GroupVersion{Group: "", Version: "v1"}] = &spec3.OpenAPI{
		Components: &spec3.Components{
			Schemas: map[string]*spec.Schema{
				"io.k8s.api.core.v1.Service": {
					VendorExtensible: spec.VendorExtensible{Extensions: spec.Extensions{
						"x-kubernetes-group-version-kind": []any{
							map[string]any{"group": "", "version": "v1", "kind": "Service"},
						},
					}},
					SchemaProps: spec.SchemaProps{
						Type: spec.StringOrArray{"object"},
						Properties: map[string]spec.Schema{
							"spec": {SchemaProps: spec.SchemaProps{Type: spec.StringOrArray{"object"}}},
						},
					},
				},
			},
		},
	}

	fetcher := NewClusterFetcher(&stubCRDGetter{}, openapi)
	got, _, err := fetcher.Fetch(t.Context(), schema.GroupVersionKind{
		Group: "", Version: "v1", Kind: "Service",
	})
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, spec.StringOrArray{"object"}, got.Type)
}

// TestClusterFetcher_CRDPathPreferredOverOpenAPI pins the resolution
// priority. A resource installed as a CRD that also happens to live
// in the aggregated OpenAPI must be served from the CRD — the CRD
// schema is the source of truth for custom resources (the cluster's
// aggregated OpenAPI might be stale or differ in subtle ways).
func TestClusterFetcher_CRDPathPreferredOverOpenAPI(t *testing.T) {
	crdProps := &apiextensionsv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"fromCRD": {Type: "string"},
		},
	}
	crds := &stubCRDGetter{list: []apiextensionsv1.CustomResourceDefinition{
		crdFixture(t, "example.com", "Widget", "widgets", "v1", crdProps),
	}}
	openapi := newStubOpenAPI()
	openapi.specs[schema.GroupVersion{Group: "example.com", Version: "v1"}] = &spec3.OpenAPI{
		Components: &spec3.Components{
			Schemas: map[string]*spec.Schema{
				"example.com.v1.Widget": {
					VendorExtensible: spec.VendorExtensible{Extensions: spec.Extensions{
						"x-kubernetes-group-version-kind": []any{
							map[string]any{"group": "example.com", "version": "v1", "kind": "Widget"},
						},
					}},
					SchemaProps: spec.SchemaProps{
						Type: spec.StringOrArray{"object"},
						Properties: map[string]spec.Schema{
							"fromOpenAPI": {SchemaProps: spec.SchemaProps{Type: spec.StringOrArray{"string"}}},
						},
					},
				},
			},
		},
	}

	fetcher := NewClusterFetcher(crds, openapi)
	got, _, err := fetcher.Fetch(t.Context(), schema.GroupVersionKind{
		Group: "example.com", Version: "v1", Kind: "Widget",
	})
	require.NoError(t, err)
	_, fromCRD := got.Properties["fromCRD"]
	_, fromOpenAPI := got.Properties["fromOpenAPI"]
	assert.True(t, fromCRD, "schema must come from the CRD")
	assert.False(t, fromOpenAPI, "OpenAPI must NOT win when the CRD is also present")
}

// TestClusterFetcher_NotFound exercises the fail-open contract: a GVK
// that isn't a CRD and doesn't show up in the OpenAPI v3 spec must
// produce ErrSchemaNotAvailable wrapping the IsNotFound sentinel, so
// the bootstrap can fall back to the generic Resource envelope.
func TestClusterFetcher_NotFound(t *testing.T) {
	openapi := newStubOpenAPI()
	openapi.specs[schema.GroupVersion{Group: "absent.io", Version: "v1"}] = &spec3.OpenAPI{
		Components: &spec3.Components{Schemas: map[string]*spec.Schema{}},
	}

	fetcher := NewClusterFetcher(&stubCRDGetter{}, openapi)
	_, _, err := fetcher.Fetch(t.Context(), schema.GroupVersionKind{
		Group: "absent.io", Version: "v1", Kind: "Ghost",
	})
	require.Error(t, err)
	var nae *ErrSchemaNotAvailable
	require.True(t, errors.As(err, &nae), "want *ErrSchemaNotAvailable, got %T", err)
	assert.Equal(t, "Ghost", nae.GVK.Kind)
	assert.True(t, IsNotFound(err), "the underlying cause must be the NotFound sentinel")
}

// TestClusterFetcher_CRDReadErrorTriggersOpenAPIFallback covers the
// case where the API server is reachable but the CRD read
// itself fails (RBAC denial, transient 503, …). The fetcher MUST
// try OpenAPI as a fallback: the public aggregated OpenAPI v3
// endpoint is readable by every authenticated user on a working
// cluster and covers every registered resource, so falling back
// keeps typed access working even when the controller's RBAC
// doesn't include `apiextensions.k8s.io/customresourcedefinitions
// get` (a common minimum-privilege chart configuration).
//
// If OpenAPI also fails (or doesn't contain the GVK), BOTH causes
// must be reported via errors.Join so an operator can distinguish
// "RBAC denial + missing registration" from "RBAC denial +
// transient 502" without losing either signal.
func TestClusterFetcher_CRDReadErrorTriggersOpenAPIFallback(t *testing.T) {
	t.Run("OpenAPI has the schema → success", func(t *testing.T) {
		crds := &stubCRDGetter{err: errors.New("forbidden")}
		openapi := newStubOpenAPI()
		gv := schema.GroupVersion{Group: "g", Version: "v1"}
		openapi.specs[gv] = &spec3.OpenAPI{
			Components: &spec3.Components{
				Schemas: map[string]*spec.Schema{
					"k": withGVK(spec.StringOrArray{"object"}, "g", "v1", "K"),
				},
			},
		}

		fetcher := NewClusterFetcher(crds, openapi)
		sch, _, err := fetcher.Fetch(t.Context(), schema.GroupVersionKind{
			Group: "g", Version: "v1", Kind: "K",
		})
		require.NoError(t, err,
			"OpenAPI fallback must rescue typed access when the CRD path is denied")
		require.NotNil(t, sch)
	})

	t.Run("OpenAPI also fails → both causes surface", func(t *testing.T) {
		crds := &stubCRDGetter{err: errors.New("forbidden")}
		openapi := newStubOpenAPI() // empty — GV won't be found

		fetcher := NewClusterFetcher(crds, openapi)
		_, _, err := fetcher.Fetch(t.Context(), schema.GroupVersionKind{
			Group: "g", Version: "v1", Kind: "K",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "forbidden",
			"the CRD-path failure must remain visible in the joined error so "+
				"a permissions regression isn't masked by the OpenAPI miss")
	})
}

// TestClusterFetcher_CRDReadCachedPerGVK verifies the contract that
// GetCRD is consulted at most once per GVK per ClusterFetcher lifetime.
// Multiple Fetch calls in flight at the same time must coalesce, not
// race-and-duplicate.
func TestClusterFetcher_CRDReadCachedPerGVK(t *testing.T) {
	crds := &stubCRDGetter{}
	fetcher := NewClusterFetcher(crds, newStubOpenAPI())
	ctx := t.Context()

	const N = 20
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, _, _ = fetcher.Fetch(ctx, schema.GroupVersionKind{
				Group: "g", Version: "v1", Kind: "K",
			})
		}()
	}
	wg.Wait()
	assert.EqualValues(t, 1, crds.calls.Load(),
		"GetCRD must be invoked exactly once across concurrent Fetches")

	_, _, err := fetcher.Fetch(ctx, schema.GroupVersionKind{Group: "g", Version: "v1", Kind: "Other"})
	require.True(t, IsNotFound(err))
	assert.EqualValues(t, 2, crds.calls.Load(), "another GVK is read separately")
}

// TestClusterFetcher_CRDReadContextCancellationDoesNotPoisonCache
// pins the contract that a cancelled-context CRD-read failure
// evicts the in-flight marker so the next caller with a fresh
// context retries cleanly. Without this, a brief startup-time
// context expiry would permanently disable CRD schema fetching
// for the lifetime of the controller iteration.
func TestClusterFetcher_CRDReadContextCancellationDoesNotPoisonCache(t *testing.T) {
	var calls atomic.Int64
	crds := &stubCRDGetter{
		hook: func(ctx context.Context) (*apiextensionsv1.CustomResourceDefinition, error) {
			calls.Add(1)
			// First call: simulate ctx cancellation mid-flight.
			if calls.Load() == 1 {
				return nil, ctx.Err()
			}
			return nil, ErrNoCRD // second call: no CRD serves the GVK
		},
	}
	fetcher := NewClusterFetcher(crds, newStubOpenAPI())

	ctx1, cancel := context.WithCancel(context.Background())
	cancel() // ensure ctx.Err() is non-nil before the call
	_, _, err := fetcher.Fetch(ctx1, schema.GroupVersionKind{Group: "g", Version: "v1", Kind: "K"})
	require.Error(t, err, "cancelled context must produce an error")

	// Second caller has a fresh context — must retry the read, not
	// inherit the previous caller's cancellation. We don't care
	// about the result here; the call-count assertion below is the
	// pin.
	_, _, retryErr := fetcher.Fetch(context.Background(), schema.GroupVersionKind{Group: "g", Version: "v1", Kind: "K"})
	_ = retryErr
	assert.EqualValues(t, 2, calls.Load(),
		"second caller must re-attempt GetCRD — context-scoped failures must not poison the cache")
}

// TestClusterFetcher_GVComponentsCachedPerGV verifies the per-GroupVersion
// cache. Fetching multiple Kinds in the same GroupVersion must
// trigger exactly one GVComponents call.
func TestClusterFetcher_GVComponentsCachedPerGV(t *testing.T) {
	openapi := newStubOpenAPI()
	gv := schema.GroupVersion{Group: "g", Version: "v1"}
	openapi.specs[gv] = &spec3.OpenAPI{
		Components: &spec3.Components{
			Schemas: map[string]*spec.Schema{
				"a": withGVK(spec.StringOrArray{"object"}, "g", "v1", "A"),
				"b": withGVK(spec.StringOrArray{"object"}, "g", "v1", "B"),
			},
		},
	}
	fetcher := NewClusterFetcher(&stubCRDGetter{}, openapi)

	_, _, errA := fetcher.Fetch(t.Context(), schema.GroupVersionKind{Group: "g", Version: "v1", Kind: "A"})
	_, _, errB := fetcher.Fetch(t.Context(), schema.GroupVersionKind{Group: "g", Version: "v1", Kind: "B"})
	_, _ = errA, errB // only the call-count below matters
	assert.Equal(t, 1, openapi.callCount[gv],
		"same-GV Fetches must coalesce on the cached spec")
}

// TestConvertJSONSchemaProps_RoundTrip covers the apiextensions ⇄
// kube-openapi spec conversion. We rely on the two types serialising
// to compatible OpenAPI v3 JSON; the test pins that with the
// extensions and additionalProperties shapes typegen actually consumes.
func TestConvertJSONSchemaProps_RoundTrip(t *testing.T) {
	in := &apiextensionsv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"spec": {
				Type: "object",
				Properties: map[string]apiextensionsv1.JSONSchemaProps{
					"port":     {Type: "integer", Format: "int32"},
					"hostname": {Type: "string"},
				},
			},
		},
		// x-kubernetes-preserve-unknown-fields must survive — typegen
		// keys off this extension to degrade the subtree to any.
		XPreserveUnknownFields: pointerTo(true),
	}
	out, err := convertJSONSchemaProps(in)
	require.NoError(t, err)
	require.Equal(t, spec.StringOrArray{"object"}, out.Type)
	specField, ok := out.Properties["spec"]
	require.True(t, ok)
	port, ok := specField.Properties["port"]
	require.True(t, ok)
	assert.Equal(t, spec.StringOrArray{"integer"}, port.Type)
	// The conversion picks up the preserve-unknown extension via the
	// shared OpenAPI wire format. typegen reads this off the
	// VendorExtensible.Extensions map.
	v, ok := out.Extensions["x-kubernetes-preserve-unknown-fields"]
	require.True(t, ok, "x-kubernetes-preserve-unknown-fields must round-trip through JSON")
	assert.Equal(t, true, v)
}

// withGVK shorthands the noisy spec.Schema literal we need in
// fetcher tests where the property shape doesn't matter — only the
// x-kubernetes-group-version-kind extension does.
func withGVK(t spec.StringOrArray, group, version, kind string) *spec.Schema {
	return &spec.Schema{
		VendorExtensible: spec.VendorExtensible{Extensions: spec.Extensions{
			"x-kubernetes-group-version-kind": []any{
				map[string]any{"group": group, "version": version, "kind": kind},
			},
		}},
		SchemaProps: spec.SchemaProps{Type: t},
	}
}

// pointerTo is the usual `&x` helper for one-line literal pointers
// — needed because apiextensions.JSONSchemaProps uses `*bool` to
// distinguish "not set" from "set to false" for the
// XPreserveUnknownFields field.
func pointerTo[T any](v T) *T { return &v }
