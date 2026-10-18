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
	"log/slog"
	"reflect"
	"sync"

	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/schemafetcher"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/typegen"
)

// Resource describes one watched-resource entry that typebootstrap
// should resolve into a generated Go type. Callers build this slice
// from their config of choice (the production wiring derives it
// from HAProxyTemplateConfig.spec.watchedResources, but tests build
// it directly).
type Resource struct {
	// Name is the user-defined identifier templates use to reach
	// this resource — the same key that appears as
	// `resources.<Name>` in chart templates.
	Name string

	// GVK is the fully-resolved (Group, Version, Kind) triple. The
	// caller is responsible for translating the CRD-side
	// (apiVersion, resources-plural) form into Kind via a
	// RESTMapper before handing the resource to typebootstrap;
	// this keeps the package free of REST-mapping deps.
	GVK schema.GroupVersionKind

	// IgnoreFields is the per-resource ignore list. Merged with
	// [Config.GlobalIgnoreFields] before being passed to typegen,
	// matching the merge the resourcewatcher does for its runtime
	// FieldFilter (pkg/controller/resourcewatcher/watcher.go
	// `mergeIgnoreFields`). Order of the inputs doesn't matter —
	// typegen deduplicates effectively by storing patterns in a
	// set.
	IgnoreFields []string
}

// Config wraps the inputs Bootstrap needs.
type Config struct {
	// Resources lists every watched-resource entry to type-resolve.
	// Order doesn't matter; results are keyed by [Resource.Name].
	Resources []Resource

	// GlobalIgnoreFields is the cluster-wide ignore list (from
	// HAProxyTemplateConfig.spec.watchedResourcesIgnoreFields).
	// Each per-resource Bootstrap call merges this with the
	// resource's own [Resource.IgnoreFields] before invoking
	// typegen.
	GlobalIgnoreFields []string

	// Fetcher is the schema-acquisition strategy. Production wires
	// this to [schemafetcher.NewClusterFetcher]; tests use a
	// [schemafetcher.MapFetcher] with pre-baked schemas.
	Fetcher schemafetcher.Fetcher

	// Logger receives the structured error log for the resource
	// whose schema acquisition failed. Required — bootstrap is the
	// only surface that observes per-resource schema problems
	// before they abort iteration startup, so operators need
	// visibility into which resource (and why) caused the failure.
	Logger *slog.Logger
}

// Result is what Bootstrap returns. Types holds the successful
// resolutions Bootstrap completed before any failure; Errors
// holds the per-resource cause for any failure that aborted the
// run. Both maps are always non-nil so callers can range them
// without nil checks.
type Result struct {
	// Types maps the resource's user-defined name to its generated
	// Go type. Pass these to [BuildEngineDeclarations] to produce
	// the engine's additionalDeclarations map.
	Types map[string]reflect.Type

	// Kinds maps the resource's user-defined name to the GVK.Kind
	// string captured from cfg.Resources. Used by
	// [BuildEngineDeclarations] to name the per-resource type
	// declarations (one per watched Kind — e.g. for a CRD named
	// `Widget` the declaration is `Widget`) at engine-declaration
	// time so chart authors can write typed macro signatures
	// (`(w *Widget)`, `(items []*Widget)`) instead of `any`/`[]any`.
	//
	// The Kind is the canonical singular noun for the resource —
	// K8s naming preserves acronyms (HTTPRoute, BackendTLSPolicy)
	// which reads more naturally in chart macros than the
	// PascalCase-of-resource-key form (Httproutes,
	// Backendtlspolicies). Multiple watched resources sharing a
	// Kind (e.g. `services` and `controller_services` both → Kind
	// "Service") deduplicate to a single type declaration; the
	// chart picks up the shared declaration regardless of which
	// watched-resource key carries the schema.
	Kinds map[string]string

	// Errors records why a resource didn't get a typed view.
	// Bootstrap is fail-closed (see Bootstrap doc): the first
	// per-resource failure aborts the run, so Errors typically
	// has at most one entry — but defensively it's a map so
	// downstream code that wants to enumerate is uniform with
	// successful runs.
	Errors map[string]error
}

// Bootstrap orchestrates the schema-fetch → type-generate pipeline
// for every resource in cfg.Resources. Returns a *Result aggregating
// successful types and per-resource errors. The outer error is
// reserved for catastrophic failures (missing required config); a
// resource that individually fails goes into Result.Errors so the
// chart can keep booting with reduced typing rather than refusing
// to start.
//
// Resources resolve concurrently, so the wall-clock cost is the slowest
// resource's apiserver round trips rather than the sum over all of them.
func Bootstrap(ctx context.Context, cfg Config) (*Result, error) {
	if cfg.Fetcher == nil {
		return nil, errors.New("typebootstrap: Fetcher is required")
	}
	if cfg.Logger == nil {
		return nil, errors.New("typebootstrap: Logger is required (per-resource degradations need operator visibility)")
	}

	result := &Result{
		Types:  make(map[string]reflect.Type, len(cfg.Resources)),
		Kinds:  make(map[string]string, len(cfg.Resources)),
		Errors: make(map[string]error, 0),
	}

	named := make([]*Resource, 0, len(cfg.Resources))
	for i := range cfg.Resources {
		res := &cfg.Resources[i]
		if res.Name == "" {
			result.Errors[res.Name] = errors.New("watched resource has empty Name")
			cfg.Logger.Warn("Typebootstrap: skipping watched resource with empty name",
				"gvk", res.GVK.String())
			continue
		}
		named = append(named, res)
	}

	err := bootstrapAll(ctx, &cfg, named, result)
	var failure *resourceFailure
	switch {
	case err == nil:
		return result, nil
	case errors.Is(ctx.Err(), context.Canceled):
		return result, context.Cause(ctx)
	case !errors.As(err, &failure):
		return result, err
	}
	res := failure.resource
	// Hard failure: template authors using typed access
	// (gw.Spec.X, route.Status.Y) need the guarantee that
	// every declared watched resource resolved to its real
	// schema. Falling back to envelope-only typed access
	// for a subset of resources would silently break
	// templates whose chart predates the schema-fetch
	// failure, which is exactly the regression mode this
	// pipeline exists to prevent.
	//
	// Surface the failure to operators via a hard boot
	// error so the cluster's RBAC / CRD installation /
	// apiserver health gets investigated. Recording the
	// per-resource cause in result.Errors keeps it
	// available for debug surfaces (status CRD, log) that
	// want to enumerate which resource broke.
	result.Errors[res.Name] = failure.err
	cfg.Logger.Error("Typebootstrap: schema acquisition failed for resource — failing iteration startup",
		"resource", res.Name,
		"gvk", res.GVK.String(),
		"error", failure.err)
	return result, fmt.Errorf("schema acquisition failed for watched resource %q (%s): %w",
		res.Name, res.GVK, failure.err)
}

// maxConcurrentFetches bounds one bootstrap's in-flight schema fetches so a
// config with many watched resources doesn't burst the apiserver.
const maxConcurrentFetches = 16

// resourceFailure carries the resource whose schema acquisition failed.
type resourceFailure struct {
	resource *Resource
	err      error
}

func (f *resourceFailure) Error() string { return f.err.Error() }

// bootstrapAll resolves every resource into result. The first failure,
// a *resourceFailure, cancels the others.
func bootstrapAll(ctx context.Context, cfg *Config, resources []*Resource, result *Result) error {
	var mu sync.Mutex
	group, fetchCtx := errgroup.WithContext(ctx)
	group.SetLimit(maxConcurrentFetches)
	for _, res := range resources {
		if fetchCtx.Err() != nil {
			break
		}
		group.Go(func() error {
			typ, err := bootstrapOne(fetchCtx, cfg, res)
			if err != nil {
				return &resourceFailure{resource: res, err: err}
			}
			mu.Lock()
			defer mu.Unlock()
			result.Types[res.Name] = typ
			result.Kinds[res.Name] = res.GVK.Kind
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return err
	}
	return ctx.Err()
}

// bootstrapOne is the single-resource path extracted from Bootstrap
// so each branch (fetch error, convert error, success) is testable
// in isolation. Returns the generated type or wraps the underlying
// schemafetcher / typegen failure for the caller to record.
//
// res is passed by pointer to avoid copying the Resource value (the
// IgnoreFields slice header makes the struct >64 bytes).
func bootstrapOne(ctx context.Context, cfg *Config, res *Resource) (reflect.Type, error) {
	sch, components, err := cfg.Fetcher.Fetch(ctx, res.GVK)
	if err != nil {
		return nil, fmt.Errorf("fetching schema: %w", err)
	}
	if sch == nil {
		// Defensive: the Fetcher contract promises a non-nil
		// schema when err == nil, but if some future implementation
		// breaks that contract we want a comprehensible error
		// rather than a nil-pointer in the converter.
		return nil, errors.New("schemafetcher returned a nil schema with no error")
	}

	// Fill in the K8s convention that CRDs leave implicit: the
	// apiserver auto-validates `metadata` against ObjectMeta, so
	// CRD authors typically declare `metadata: {type: object}`
	// with no properties (or omit it entirely). Without this
	// pre-process the converter degrades the empty-properties
	// metadata to interface{}, and chart templates lose typed
	// access to <resource>.Metadata.{Name,Namespace,...} on every
	// CRD-backed resource — which is most of what watchedResources
	// typically contains.
	sch = injectObjectMetaFields(sch)

	// Components nil for CRD-backed schemas (they inline every
	// shape); non-nil for OpenAPI v3-backed schemas where K8s wraps
	// shared types like ObjectMeta in `allOf: [$ref: ...]` patterns.
	// The converter needs the map to walk those refs.
	conv := typegen.NewConverter(components)
	conv.IgnoreFields = mergeIgnoreFields(cfg.GlobalIgnoreFields, res.IgnoreFields)

	typ, err := conv.Convert(sch)
	if err != nil {
		return nil, fmt.Errorf("converting schema to Go type: %w", err)
	}
	return typ, nil
}

// Schema-author shorthands used by the synthetic ObjectMeta builder
// and any other in-package schema literals. Extracted so goconst stops
// flagging the repeated string literals.
const (
	schemaTypeString = "string"
	schemaTypeObject = "object"
	metadataFieldKey = "metadata"
	nameFieldKey     = "name"
)

// injectObjectMetaFields adds the ObjectMeta fields supplied by the apiserver
// when a resource schema omits all or part of them.
//
// Returns a shallow copy of the original schema with the metadata
// property replaced; the original is not mutated (other watched
// resources might share the same schema instance via $ref
// caching in upstream OpenAPI sources).
func injectObjectMetaFields(sch *spec.Schema) *spec.Schema {
	if sch == nil || sch.Properties == nil {
		return sch
	}
	meta, ok := sch.Properties[metadataFieldKey]
	if !ok {
		meta = syntheticObjectMetaSchema()
	} else {
		enriched, complete := enrichObjectMetaSchema(&meta)
		if complete {
			return sch
		}
		meta = enriched
	}
	// Shallow-copy so we don't mutate upstream's cached schema.
	// Range by key + map index (not by value) to avoid copying
	// the ~528-byte spec.Schema struct on every iteration.
	out := *sch
	out.Properties = make(map[string]spec.Schema, len(sch.Properties))
	for k := range sch.Properties {
		out.Properties[k] = sch.Properties[k]
	}
	out.Properties[metadataFieldKey] = meta
	return &out
}

// enrichObjectMetaSchema fills in the missing apiserver-supplied metadata
// fields. complete means the schema already carries them (or delegates via
// $ref/allOf) and must be kept as is.
func enrichObjectMetaSchema(meta *spec.Schema) (enriched spec.Schema, complete bool) {
	if meta.Ref.String() != "" || len(meta.AllOf) > 0 {
		return spec.Schema{}, true
	}
	synthetic := syntheticObjectMetaSchema()
	if len(meta.Properties) == 0 {
		return synthetic, false
	}
	_, hasUID := meta.Properties["uid"]
	_, hasResourceVersion := meta.Properties["resourceVersion"]
	if hasUID && hasResourceVersion {
		return spec.Schema{}, true
	}
	metadataCopy := *meta
	metadataCopy.Properties = make(map[string]spec.Schema, len(meta.Properties)+2)
	for name := range meta.Properties {
		metadataCopy.Properties[name] = meta.Properties[name]
	}
	if !hasUID {
		metadataCopy.Properties["uid"] = synthetic.Properties["uid"]
	}
	if !hasResourceVersion {
		metadataCopy.Properties["resourceVersion"] = synthetic.Properties["resourceVersion"]
	}
	return metadataCopy, false
}

// syntheticObjectMetaSchema returns a spec.Schema with the
// ObjectMeta fields chart libraries reach into, expressed as a
// schema so it composes with the rest of the converter pipeline
// (IgnoreFields stripping, depth cap, etc.).
//
// Lives next to its only caller; if a future caller needs the
// shape elsewhere, lift to pkg/k8s/typegen.
func syntheticObjectMetaSchema() spec.Schema {
	stringSchema := spec.Schema{SchemaProps: spec.SchemaProps{
		Type: spec.StringOrArray{schemaTypeString},
	}}
	stringMapSchema := spec.Schema{SchemaProps: spec.SchemaProps{
		Type: spec.StringOrArray{schemaTypeObject},
		AdditionalProperties: &spec.SchemaOrBool{
			Schema: &spec.Schema{SchemaProps: spec.SchemaProps{
				Type: spec.StringOrArray{schemaTypeString},
			}},
		},
	}}
	return spec.Schema{SchemaProps: spec.SchemaProps{
		Type: spec.StringOrArray{schemaTypeObject},
		Properties: map[string]spec.Schema{
			nameFieldKey:        stringSchema,
			"namespace":         stringSchema,
			"uid":               stringSchema,
			"resourceVersion":   stringSchema,
			"generation":        {SchemaProps: spec.SchemaProps{Type: spec.StringOrArray{"integer"}, Format: "int64"}},
			"creationTimestamp": stringSchema,
			"labels":            stringMapSchema,
			"annotations":       stringMapSchema,
		},
	}}
}

// mergeIgnoreFields combines the cluster-wide ignore list with a
// resource's own. Mirrors what
// pkg/controller/resourcewatcher/watcher.go does for the runtime
// field filter so the typed view and the watcher agree on which
// fields exist. Duplicates are tolerated — typegen deduplicates
// internally when building its ignoreSet.
func mergeIgnoreFields(global, perResource []string) []string {
	if len(global) == 0 {
		return perResource
	}
	if len(perResource) == 0 {
		return global
	}
	out := make([]string, 0, len(global)+len(perResource))
	out = append(out, global...)
	out = append(out, perResource...)
	return out
}
