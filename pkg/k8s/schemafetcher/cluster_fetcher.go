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
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kube-openapi/pkg/spec3"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// CRDGetter returns the CRD that serves a GroupVersionKind, or an error
// wrapping [ErrNoCRD] when no CRD serves it. The production implementation
// reads the single CRD by name: a consistent LIST of every CRD costs seconds
// on some apiservers (#329).
type CRDGetter interface {
	GetCRD(ctx context.Context, gvk schema.GroupVersionKind) (*apiextensionsv1.CustomResourceDefinition, error)
}

// ErrNoCRD reports that no CRD serves a GroupVersionKind.
var ErrNoCRD = errors.New("no CRD serves the resource")

// OpenAPIV3Provider returns the components of the cluster's OpenAPI v3
// document for a GroupVersion, or nil when the document has none.
type OpenAPIV3Provider interface {
	GVComponents(ctx context.Context, gv schema.GroupVersion) (*spec3.Components, error)
}

// ClusterFetcher resolves schemas against the cluster. It tries CRDs
// first (one single-resource read per GVK) and
// falls back to the aggregated OpenAPI v3 endpoint for built-ins
// AND for any CRD-defined resource whose schema didn't load via the
// CRD path (e.g. because the controller's RBAC doesn't include
// `apiextensions.k8s.io/customresourcedefinitions get`).
//
// Concurrency: Fetch is safe for concurrent use. Each CRD is read at
// most once per GVK and each OpenAPI per-GroupVersion spec at most
// once per GV; concurrent callers coalesce onto one API call.
// Context-scoped failures (cancellation, deadline) DO NOT poison the
// cache — the entry is evicted so the next caller retries with a
// fresh context.
type ClusterFetcher struct {
	crds    CRDGetter
	openapi OpenAPIV3Provider

	crdCache          coalescingCache[schema.GroupVersionKind, *apiextensionsv1.CustomResourceDefinition]
	gvComponentsCache coalescingCache[schema.GroupVersion, *spec3.Components]
}

// NewClusterFetcher returns a [ClusterFetcher] that uses the supplied
// CRD getter and OpenAPI v3 provider. Both must be non-nil — passing
// nil for either is a programming error and panics on construction
// rather than failing the first Fetch with an obscure nil-pointer.
func NewClusterFetcher(crds CRDGetter, openapi OpenAPIV3Provider) *ClusterFetcher {
	if crds == nil || openapi == nil {
		panic("schemafetcher: NewClusterFetcher requires non-nil CRDGetter and OpenAPIV3Provider")
	}
	return &ClusterFetcher{crds: crds, openapi: openapi}
}

// Fetch implements [Fetcher]. Tries the CRD index first because it's
// cheaper and gives the highest-fidelity schema (the CRD's own
// openAPIV3Schema, including x-kubernetes-preserve-unknown-fields
// subtrees). Falls back to the aggregated OpenAPI v3 endpoint for
// built-ins AND for the case where the CRD path fails for any
// reason — RBAC denial, parse failure, transient read error.
//
// The OpenAPI v3 endpoint is readable by every authenticated user
// on a working cluster and covers every registered CRD, so it's a
// strict superset of the CRD path's coverage. Falling back on
// CRD-path failures (not just NotFound) keeps typed access working
// in security-restricted environments where the controller can't
// read CRDs.
//
// When both paths fail, the OpenAPI error wins as the user-facing
// cause (it's the path with broader coverage and lower permission
// requirements), with the CRD-path error preserved in the chain
// via errors.Join so operators investigating partial-access
// regressions can still see both.
func (f *ClusterFetcher) Fetch(ctx context.Context, gvk schema.GroupVersionKind) (*spec.Schema, map[string]spec.Schema, error) {
	sch, crdErr := f.fetchFromCRDs(ctx, gvk)
	if crdErr == nil {
		// CRD schemas inline every shape — no shared components.
		return sch, nil, nil
	}

	sch, components, openAPIErr := f.fetchFromOpenAPI(ctx, gvk)
	if openAPIErr == nil {
		return sch, components, nil
	}

	// Both paths failed. Preserve both causes so operators can tell
	// "CRD read denied + GV not in OpenAPI" (real RBAC + missing
	// registration) from "CRD read denied + transient OpenAPI 502"
	// (purely RBAC + retryable). errors.Is(...) still works for the
	// IsNotFound sentinel when either leg returned a NotFound.
	return nil, nil, errors.Join(openAPIErr, crdErr)
}

// fetchFromCRDs reads the CRD serving gvk and extracts the
// requested version's schema.
//
// Returns the schema if found, an *ErrSchemaNotAvailable wrapping
// errNotFound when no CRD serves the GVK, or a different
// *ErrSchemaNotAvailable when the read failed or the CRD exists but
// its schema doesn't (malformed CRD, missing openAPIV3Schema, etc.).
func (f *ClusterFetcher) fetchFromCRDs(ctx context.Context, gvk schema.GroupVersionKind) (*spec.Schema, error) {
	crd, err := f.crdCache.get(ctx, gvk, func(ctx context.Context) (*apiextensionsv1.CustomResourceDefinition, error) {
		return f.crds.GetCRD(ctx, gvk)
	})
	if errors.Is(err, ErrNoCRD) {
		return nil, &ErrSchemaNotAvailable{GVK: gvk, Cause: errNotFound}
	}
	if err != nil {
		return nil, &ErrSchemaNotAvailable{GVK: gvk, Cause: err}
	}
	if crd.Spec.Group != gvk.Group || crd.Spec.Names.Kind != gvk.Kind {
		return nil, &ErrSchemaNotAvailable{GVK: gvk, Cause: errNotFound}
	}
	for vi := range crd.Spec.Versions {
		v := &crd.Spec.Versions[vi]
		if v.Name != gvk.Version {
			continue
		}
		if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
			return nil, &ErrSchemaNotAvailable{
				GVK:   gvk,
				Cause: fmt.Errorf("CRD %s has no openAPIV3Schema for version %s", crd.Name, gvk.Version),
			}
		}
		sch, err := convertJSONSchemaProps(v.Schema.OpenAPIV3Schema)
		if err != nil {
			return nil, &ErrSchemaNotAvailable{
				GVK:   gvk,
				Cause: fmt.Errorf("converting %s/%s schema: %w", crd.Name, gvk.Version, err),
			}
		}
		return sch, nil
	}
	return nil, &ErrSchemaNotAvailable{
		GVK:   gvk,
		Cause: fmt.Errorf("CRD %s does not serve version %s", crd.Name, gvk.Version),
	}
}

// fetchFromOpenAPI resolves a GVK against the cluster's aggregated
// OpenAPI v3 endpoint. The schema key follows convention
// (io.k8s.api.<group>.<version>.<Kind> for built-ins) but we don't
// rely on it — every schema carries an x-kubernetes-group-version-kind
// extension that we match against directly, so renamed schemas or
// non-canonical group paths still resolve.
//
// Returns the matched schema PLUS the full components map for the
// GV. K8s aggregated OpenAPI v3 uses `allOf: [$ref: ObjectMeta]`
// to attach defaults to refs (canonical pattern for `metadata` on
// every resource); the consuming converter needs the components
// map to resolve those refs into typed sub-structs. Without it,
// `gw.Metadata` collapses to `interface{}` and chart code reaching
// in compiles to nothing.
//
// The components map is shared by reference across all calls for
// the same GV: the same spec3.Components value is cached per-GV in
// gvComponentsCache. Callers must NOT mutate it.
func (f *ClusterFetcher) fetchFromOpenAPI(ctx context.Context, gvk schema.GroupVersionKind) (*spec.Schema, map[string]spec.Schema, error) {
	gvComponents, err := f.gvComponents(ctx, gvk.GroupVersion())
	if err != nil {
		return nil, nil, &ErrSchemaNotAvailable{GVK: gvk, Cause: err}
	}
	if gvComponents == nil {
		return nil, nil, &ErrSchemaNotAvailable{GVK: gvk, Cause: errNotFound}
	}
	// Build a map[string]spec.Schema from the spec3 components for
	// the converter (whose Components signature takes value types
	// rather than pointers — matches the OpenAPI v3 wire form).
	components := make(map[string]spec.Schema, len(gvComponents.Schemas))
	for name, ptr := range gvComponents.Schemas {
		if ptr != nil {
			components[name] = *ptr
		}
	}
	for _, sch := range gvComponents.Schemas {
		if !schemaMatchesGVK(sch, gvk) {
			continue
		}
		return sch, components, nil
	}
	return nil, nil, &ErrSchemaNotAvailable{GVK: gvk, Cause: errNotFound}
}

func (f *ClusterFetcher) gvComponents(ctx context.Context, gv schema.GroupVersion) (*spec3.Components, error) {
	return f.gvComponentsCache.get(ctx, gv, func(ctx context.Context) (*spec3.Components, error) {
		return f.openapi.GVComponents(ctx, gv)
	})
}

// coalescingCache memoizes one load per key. Concurrent callers for a
// key coalesce on the entry's done channel so only one API call is in
// flight.
//
// Context-scoped failures (cancellation, deadline) DO NOT poison the
// cache: if the leader's context expires mid-load, the entry is
// evicted and waiters re-enter with their own context. Other errors
// ARE cached — they reflect real cluster state. A dedicated `evicted`
// field (rather than "zero value and nil error") avoids a retry loop
// when a nil result is a legitimate persistent outcome.
type coalescingCache[K comparable, V any] struct {
	mu      sync.Mutex
	entries map[K]*cacheEntry[V]
}

type cacheEntry[V any] struct {
	done    chan struct{}
	value   V
	err     error
	evicted bool
}

func (c *coalescingCache[K, V]) get(ctx context.Context, key K, load func(context.Context) (V, error)) (V, error) {
	for {
		c.mu.Lock()
		entry, ok := c.entries[key]
		if !ok {
			if c.entries == nil {
				c.entries = make(map[K]*cacheEntry[V])
			}
			entry = &cacheEntry[V]{done: make(chan struct{})}
			c.entries[key] = entry
			c.mu.Unlock()

			value, err := load(ctx)
			if err != nil && ctx.Err() != nil {
				c.mu.Lock()
				delete(c.entries, key)
				c.mu.Unlock()
				entry.evicted = true
				close(entry.done)
				var zero V
				return zero, err
			}
			entry.value, entry.err = value, err
			close(entry.done)
			return value, err
		}
		c.mu.Unlock()

		select {
		case <-entry.done:
			if entry.evicted {
				continue
			}
			return entry.value, entry.err
		case <-ctx.Done():
			var zero V
			return zero, ctx.Err()
		}
	}
}

// schemaMatchesGVK reports whether the OpenAPI schema's
// x-kubernetes-group-version-kind extension lists the requested GVK.
// Real K8s schemas usually list exactly one GVK, but the wire format
// is an array so we tolerate multiple entries.
func schemaMatchesGVK(sch *spec.Schema, gvk schema.GroupVersionKind) bool {
	if sch == nil {
		return false
	}
	ext, ok := sch.Extensions["x-kubernetes-group-version-kind"]
	if !ok {
		return false
	}
	entries, ok := ext.([]any)
	if !ok {
		return false
	}
	for _, e := range entries {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if m["group"] == gvk.Group && m["version"] == gvk.Version && m["kind"] == gvk.Kind {
			return true
		}
	}
	return false
}

// convertJSONSchemaProps adapts the CRD-shaped schema type
// (apiextensionsv1.JSONSchemaProps) into the kube-openapi
// spec.Schema typegen consumes. Both types serialise to compatible
// OpenAPI v3 JSON, so a JSON round-trip is the simplest viable
// conversion — it picks up every JSON-encoded field including the
// x-kubernetes-* extensions we care about for preserve-unknown
// detection. The alternative (a hand-rolled field-by-field copy)
// would duplicate the apiextensions-apiserver's internal
// ConvertJSONSchemaProps and require keeping the two in sync as
// JSONSchemaProps grows new fields.
//
// The conversion is the only allocation per CRD-version in the
// fetcher's hot path, but CRD schemas are read once per controller
// boot, so it's not a perf concern.
func convertJSONSchemaProps(in *apiextensionsv1.JSONSchemaProps) (*spec.Schema, error) {
	data, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("marshal JSONSchemaProps: %w", err)
	}
	var out spec.Schema
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("unmarshal into spec.Schema: %w", err)
	}
	return &out, nil
}
