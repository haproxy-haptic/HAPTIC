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

// Package rendercontext provides a centralized builder for template rendering contexts.
//
// This package consolidates the previously duplicated context creation logic from
// renderer, testrunner, benchmark, and dryrunvalidator into a single, reusable builder.
//
// Usage:
//
//	builder := rendercontext.NewBuilder(
//	    ctx,
//	    cfg,
//	    pathResolver,
//	    logger,
//	    rendercontext.WithStores(stores),
//	)
//	res := builder.Build()
//	ctx := res.Context
package rendercontext

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/names"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/typebootstrap"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/typegen"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
	"github.com/haproxy-haptic/scriggo/native"
)

// Builder constructs template rendering contexts with consistent structure.
// Use NewBuilder() to create a builder and functional options to configure it.
type Builder struct {
	// Required dependencies
	readContext  context.Context
	config       *config.Config
	pathResolver *templating.PathResolver
	logger       *slog.Logger

	// Optional dependencies (set via options)
	stores                map[string]stores.Store
	haproxyPodStore       stores.Store
	httpFetcher           templating.HTTPFetcher
	currentConfig         *renderplan.CurrentConfig
	currentConfigSource   CurrentConfigSource
	currentAuxFiles       map[string]string
	currentAuxFilesSource CurrentAuxFilesSource
	typedResourceTypes    map[string]reflect.Type
	capabilities          dataplane.Capabilities
	renderMode            RenderMode
	admissionSubject      map[string]any
	extraContext          map[string]any
	extraContextSet       bool
	runtimeEnvironment    templating.RuntimeEnvironment
	runtimeEnvironmentSet bool
	planTokenAuthority    *PlanTokenAuthority
	planMemo              *PlanMemo
	scopeFor              func(string) *NamespaceScope
}

// admissionSubjectOrEmpty returns the subject map for the template context.
// Always non-nil so templates can `admissionSubject | dig("name")` without a
// presence check regardless of render mode.
func (b *Builder) admissionSubjectOrEmpty() map[string]any {
	if b.admissionSubject == nil {
		return map[string]any{}
	}
	return b.admissionSubject
}

// RenderMode tells conflict-style template checks whether this render is an
// admission dry-run — a proposed change being validated, where the check should
// fail loud so the webhook denies it — or a normal reconcile render of existing
// live state, where the check should warn (via recordEvent) and let the render
// proceed so one already-present bad resource can't block the whole fleet's
// config. It is exposed to templates as the `renderMode` string global.
type RenderMode string

const (
	// RenderModeReconcile is the live reconcile render: lenient (warn, never
	// abort). It is the safe default when unset.
	RenderModeReconcile RenderMode = "reconcile"
	// RenderModeAdmission is a webhook / proposed-change validation render:
	// strict (fail, so admission denies the change).
	RenderModeAdmission RenderMode = "admission"
)

// Option configures a Builder.
type Option func(*Builder)

// WithStores sets the resource stores for the template context.
// Each store is wrapped in a StoreWrapper to provide template-friendly methods.
func WithStores(storeMap map[string]stores.Store) Option {
	return func(b *Builder) {
		b.stores = storeMap
	}
}

// WithHAProxyPodStore sets the HAProxy pod store for controller.haproxy_pods.
// This enables templates to access HAProxy pod count for calculations.
func WithHAProxyPodStore(store stores.Store) Option {
	return func(b *Builder) {
		b.haproxyPodStore = store
	}
}

// WithHTTPFetcher sets the HTTP fetcher for http.Fetch() calls in templates.
// Pass nil to disable HTTP fetching capability.
func WithHTTPFetcher(fetcher templating.HTTPFetcher) Option {
	return func(b *Builder) {
		b.httpFetcher = fetcher
	}
}

// WithCurrentConfig sets the servers of the running config for templates.
// This enables slot-aware server assignment and other config-aware features.
// If nil, templates receive nil currentConfig (first deployment case).
func WithCurrentConfig(cfg *renderplan.CurrentConfig) Option {
	snapshot := cloneCurrentConfig(cfg)
	return func(b *Builder) {
		b.currentConfigSource = nil
		b.currentConfig = cloneCurrentConfig(snapshot)
	}
}

// WithCurrentConfigSource defers projection until a template must execute.
func WithCurrentConfigSource(source CurrentConfigSource) Option {
	return func(b *Builder) {
		b.currentConfig = nil
		b.currentConfigSource = source
	}
}

func cloneCurrentConfig(cfg *renderplan.CurrentConfig) *renderplan.CurrentConfig {
	if cfg == nil {
		return nil
	}
	result := &renderplan.CurrentConfig{ServerIndex: make(map[string]map[string]renderplan.ServerAddr, len(cfg.ServerIndex))}
	for backend, servers := range cfg.ServerIndex {
		cloned := make(map[string]renderplan.ServerAddr, len(servers))
		for name, server := range servers {
			if server.Port != nil {
				port := *server.Port
				server.Port = &port
			}
			cloned[name] = server
		}
		result.ServerIndex[backend] = cloned
	}
	return result
}

// WithCurrentAuxFiles sets the authoritative auxiliary baseline exposed as currentFiles.
func WithCurrentAuxFiles(files map[string]string) Option {
	snapshot := maps.Clone(files)
	return func(b *Builder) {
		b.currentAuxFilesSource = nil
		b.currentAuxFiles = maps.Clone(snapshot)
	}
}

// WithCurrentAuxFilesSource defers projection until a template must execute.
func WithCurrentAuxFilesSource(source CurrentAuxFilesSource) Option {
	return func(b *Builder) {
		b.currentAuxFiles = nil
		b.currentAuxFilesSource = source
	}
}

// WithCapabilities sets the HAProxy version capabilities exposed to templates
// under the top-level "capabilities" key. The production renderer always
// injects this key, so validation and benchmark contexts must too — otherwise
// a template branching on `capabilities.supports_crt_list` (and similar)
// behaves differently between `controller validate` and production, defeating
// the purpose of pre-flight validation. When unset, Build() still populates
// "capabilities" with an all-false map (zero-value Capabilities), matching a
// no-capability HAProxy rather than omitting the key.
func WithCapabilities(caps dataplane.Capabilities) Option {
	return func(b *Builder) {
		b.capabilities = caps
	}
}

// WithRenderMode sets the RenderMode exposed to templates under "renderMode".
// When unset, Build() defaults to RenderModeReconcile — the lenient mode — so a
// caller that forgets to set it degrades to warn-and-proceed rather than failing
// a live render.
func WithRenderMode(mode RenderMode) Option {
	return func(b *Builder) {
		b.renderMode = mode
	}
}

// WithAdmissionSubject identifies one watched-resource alias under admission
// review, exposed to templates as the `admissionSubject` map global.
// Route-scoped template checks use it to
// hard-fail only when the violating route belongs to the resource being
// admitted; violations on other, already-present resources degrade to
// warn-and-fail-closed-per-route so one bad existing object can never block
// an unrelated admission. Resource-agnostic by construction: the subject is
// the store name plus object identity, never a kind-specific shape. When
// unset (reconcile renders, config-proposal renders, bulk overlays), Build()
// emits an empty map so templates can dig() it unconditionally.
func WithAdmissionSubject(store, namespace, name string) Option {
	return WithAdmissionSubjectStores([]string{store}, namespace, name)
}

// WithAdmissionSubjectStores identifies one admitted object across every
// watched-resource alias whose contents the request changes.
func WithAdmissionSubjectStores(aliases []string, namespace, name string) Option {
	return func(b *Builder) {
		storeSet := make(map[string]any, len(aliases))
		for _, store := range aliases {
			storeSet[store] = true
		}
		store := ""
		if len(storeSet) == 1 {
			for onlyAlias := range storeSet {
				store = onlyAlias
			}
		}
		b.admissionSubject = map[string]any{
			"store":     store,
			"stores":    storeSet,
			"namespace": namespace,
			"name":      name,
		}
	}
}

// WithDetachedExtraContext supplies a caller-owned extra-context snapshot.
// The caller keeps it isolated from template mutation: see
// RenderService.attemptExtraContext.
func WithDetachedExtraContext(extraContext map[string]any) Option {
	return func(b *Builder) {
		b.extraContext = extraContext
		b.extraContextSet = true
	}
}

// WithImmutableCertificates guards already-certified inputs without walking
// them again in every render.
func WithImmutableCertificates(certificates ...*templating.IncrementalImmutableCertificate) Option {
	return func(b *Builder) {
		b.readContext = templating.WithIncrementalImmutableCertificates(b.readContext, certificates...)
	}
}

// WithRuntimeEnvironment supplies the runtime values exposed to templates.
func WithRuntimeEnvironment(environment templating.RuntimeEnvironment) Option {
	return func(b *Builder) {
		b.runtimeEnvironment = environment
		b.runtimeEnvironmentSet = true
	}
}

// WithTypedResources supplies the per-resource generated Go types
// produced by typebootstrap (pkg/controller/typebootstrap). When set,
// Build emits one *additional* top-level context entry per supplied
// type: the resource's name maps to a *[]*<generated-struct> value
// populated by wrapping each item of the matching store's snapshot
// through typegen.WrapInto.
//
// The typed entries coexist with the existing map-keyed
// resources["<name>"] access — chart templates can adopt the typed
// shape per snippet without breaking templates that still use the
// untyped path. The two access paths may load their snapshots a
// few microseconds apart; templates are expected to use ONE shape
// or the OTHER for a given resource within a single render
// (mixing wouldn't compile anyway — the untyped variable is `any`).
//
// Resources whose names appear in `types` but for which no store
// is registered (via WithStores) are silently skipped. That keeps
// the option safe to use even when typebootstrap successfully
// generated a type for a resource the local controller doesn't
// happen to watch — a common case in tests.
func WithTypedResources(types map[string]reflect.Type) Option {
	return func(b *Builder) {
		b.typedResourceTypes = types
	}
}

// WithPlanTokenAuthority retains authenticated plan placeholders across renders.
func WithPlanTokenAuthority(authority *PlanTokenAuthority) Option {
	return func(b *Builder) {
		b.planTokenAuthority = authority
	}
}

// WithPlanMemo reuses plan derivations across renders.
func WithPlanMemo(memo *PlanMemo) Option {
	return func(b *Builder) {
		b.planMemo = memo
	}
}

// WithNamespaceScopes fails the render for a lookup outside a watch's
// namespaces (see NamespaceScopes). Fixture tests opt in explicitly.
func WithNamespaceScopes(scopeFor func(string) *NamespaceScope) Option {
	return func(b *Builder) {
		b.scopeFor = scopeFor
	}
}

// NewBuilder creates a new context builder with required dependencies.
//
// Parameters:
//   - ctx: Lifetime for store reads performed while rendering
//   - cfg: Controller configuration (required)
//   - pathResolver: Path resolver for file paths (required)
//   - logger: Structured logger (required)
//   - opts: Optional configuration via functional options
func NewBuilder(ctx context.Context, cfg *config.Config, pathResolver *templating.PathResolver, logger *slog.Logger, opts ...Option) *Builder {
	b := &Builder{
		readContext:  templating.WithImmutableResourceInputs(ctx),
		config:       cfg,
		pathResolver: pathResolver,
		logger:       logger,
	}

	for _, opt := range opts {
		opt(b)
	}

	return b
}

// BuildResult is the context and render-scoped collectors returned from Build().
type BuildResult struct {
	Context                   map[string]any
	FileRegistry              *FileRegistry
	PlanRegistry              *PlanRegistry
	StatusPatchCollector      *templating.StatusPatchCollector
	RenderedResourceCollector *templating.RenderedResourceCollector
	EventCollector            *templating.EventCollector
	ResourceErrors            *ResourceErrorCollector
	DerivedResources          *DerivedResourceView
	previousOutputMu          sync.Mutex
	currentConfigSource       CurrentConfigSource
	currentAuxFilesSource     CurrentAuxFilesSource
	currentConfigReady        bool
	currentAuxFilesReady      bool
	previousOutputsErr        error
}

// PreviousOutputSources returns the attempt-owned immutable source roots.
func (r *BuildResult) PreviousOutputSources() (CurrentConfigSource, CurrentAuxFilesSource) {
	if r == nil {
		return nil, nil
	}
	r.previousOutputMu.Lock()
	defer r.previousOutputMu.Unlock()
	return r.currentConfigSource, r.currentAuxFilesSource
}

// Build creates the template rendering context, file registry, status patch
// collector, and rendered resource collector.
//
// The context structure is:
//
//	{
//	  "resources": map of StoreWrappers,
//	  "controller": {"haproxy_pods": StoreWrapper},
//	  "templateSnippets": []string,
//	  "fileRegistry": FileRegistry,
//	  "planRegistry": PlanRegistry,
//	  "statusPatchCollector": StatusPatchCollector,
//	  "renderedResourceCollector": RenderedResourceCollector,
//	  "pathResolver": PathResolver,
//	  "dataplane": Config.Dataplane,
//	  "capabilities": map[string]any (HAProxy version capabilities),
//	  "currentConfig": *renderplan.CurrentConfig (nil on first deployment),
//	  "shared": map[string]any,
//	  "runtimeEnvironment": RuntimeEnvironment,
//	  "http": HTTPFetcher (if set),
//	  "extraContext": map from config,
//	}
func (b *Builder) controllerStores(resourceErrors *ResourceErrorCollector) map[string]templating.ResourceStore {
	controller := make(map[string]templating.ResourceStore)
	if b.haproxyPodStore == nil {
		return controller
	}
	b.logger.Debug("Wrapping HAProxy pods store for rendering context")
	controller["haproxy_pods"] = &StoreWrapper{
		Store:          b.haproxyPodStore,
		ResourceType:   names.HAProxyPodsResourceType,
		Logger:         b.logger,
		IndexBy:        []string{"metadata.namespace", "metadata.name"},
		readContext:    b.readContext,
		resourceErrors: resourceErrors,
	}
	return controller
}

func (b *Builder) Build() *BuildResult {
	resourceErrors := NewResourceErrorCollector()
	derivedResources := NewDerivedResourceView()

	// Create controller namespace with typed ResourceStore values. The
	// haproxy-pods watcher is auto-injected by ResourceWatcherComponent
	// with a fixed IndexBy of ["metadata.namespace", "metadata.name"]
	// (see pkg/controller/resourcewatcher/watcher.go) — mirror that here
	// so the wrapper's snapshot index agrees with the underlying store.
	//
	// `resources` is no longer a map[string]ResourceStore. It is a
	// dynamically-built struct value (see addTypedResources below); chart
	// templates reach it via direct field access (`resources.gateways`)
	// rather than the previous map+method shape (`resources.gateways.List()`).
	controller := b.controllerStores(resourceErrors)

	// Sort template snippet names alphabetically
	snippetNames := SortSnippetNames(b.config.TemplateSnippets)

	// Create file registry for dynamic auxiliary file registration
	fileRegistry := NewFileRegistry(b.pathResolver)

	// Create plan registry so templates can declare the structure of the
	// config they emit; RenderMain assembles the config from its tokens.
	planRegistry := NewPlanRegistry(b.pathResolver)
	if b.planTokenAuthority != nil {
		var err error
		planRegistry, err = NewPlanRegistryWithAuthority(b.pathResolver, b.planTokenAuthority)
		if err != nil {
			resourceErrors.Record(err)
		}
	}
	planRegistry.memo = b.planMemo

	// spec.maps[].ordered belongs to the plan, and the plan is built from this
	// registry by every caller — the reconcile renderer and the validation-test
	// runner alike. Declaring it here rather than at one call site is what keeps
	// the two from disagreeing about which maps tolerate a runtime append.
	for name, mapFile := range b.config.Maps {
		if err := planRegistry.MapMeta(name, mapFile.Ordered == nil || *mapFile.Ordered); err != nil {
			b.logger.Error("Failed to declare map entry order", "map", name, "error", err)
		}
	}

	// Create status patch collector for template-driven status updates
	statusPatchCollector := templating.NewStatusPatchCollector()

	// Create event collector for template-driven Kubernetes Events (recordEvent).
	// Resource-agnostic like the status patch collector: templates pass any
	// apiVersion / kind for the involved object.
	eventCollector := templating.NewEventCollector()

	// Create rendered resource collector for template-driven owned-resource
	// reconciliation — anything where the chart needs the controller to spawn /
	// update / prune Kubernetes resources alongside the HAProxy config. The
	// collector is resource-agnostic: templates pass any apiVersion / kind.
	renderedResourceCollector := templating.NewRenderedResourceCollector()

	b.logger.Debug("Rendering context built",
		"resource_count", len(b.typedResourceTypes),
		"controller_fields", len(controller),
		"snippet_count", len(snippetNames))

	// Build final context. `resources` is populated below by
	// addTypedResources — leaving it absent here lets that helper
	// hand the dynamically-typed struct value into the map without a
	// throwaway placeholder.
	runtimeEnvironment := b.runtimeEnvironment
	if !b.runtimeEnvironmentSet {
		runtimeEnvironment.GOMAXPROCS = runtime.GOMAXPROCS(0)
	}
	templateContext := map[string]any{
		"controller":                          controller,
		"templateSnippets":                    snippetNames,
		"fileRegistry":                        fileRegistry,
		"planRegistry":                        planRegistry,
		"statusPatchCollector":                statusPatchCollector,
		"recordEventCollector":                eventCollector,
		"renderedResourceCollector":           renderedResourceCollector,
		"pathResolver":                        b.pathResolver,
		"dataplane":                           b.config.Dataplane,
		"capabilities":                        CapabilitiesToMap(&b.capabilities),
		"renderMode":                          string(cmp.Or(b.renderMode, RenderModeReconcile)),
		"admissionSubject":                    b.admissionSubjectOrEmpty(),
		templating.ResourceDeriverContextName: derivedResources,
		"shared":                              templating.NewSharedContext(),
		"runtimeEnvironment":                  &runtimeEnvironment,
	}

	// Add current config if provided (NOT added when nil - Scriggo panics with nil pointer initializers)
	// This enables slot-aware server assignment during rolling deployments
	// Templates should use isNil(currentConfig) to check if it's available
	if b.currentConfigSource == nil && b.currentConfig != nil {
		templateContext["currentConfig"] = b.currentConfig
	}

	// Current general aux files (filename → content), for templates that read
	// their own prior output (e.g. self-rotating TLS session-ticket keys).
	// Injected as a *map[string]string (Scriggo variable declarations are
	// pointers, like currentConfig; the engine derefs it so templates index the
	// map directly). Always non-nil — an empty map on first deployment — so
	// templates can index it without a nil guard.
	var auxFiles map[string]string
	if b.currentAuxFilesSource == nil {
		auxFiles = b.currentAuxFiles
		if auxFiles == nil {
			auxFiles = map[string]string{}
		}
		templateContext["currentFiles"] = &auxFiles
	}

	// Add HTTP fetcher if provided
	if b.httpFetcher != nil {
		b.logger.Debug("HTTP object added to template context")
		templateContext["http"] = b.httpFetcher
	}

	// Populate the single `resources` top-level global with a
	// dynamically-built struct value (see typebootstrap's
	// BuildEngineDeclarations for the matching declaration shape).
	// One field per watched resource — typed `[]*GeneratedT` when the
	// schema resolved, untyped `[]any` when it didn't.
	b.addTypedResources(templateContext, resourceErrors, derivedResources)

	// Merge extraContext variables into top-level context
	extraContext := b.config.TemplatingSettings.ExtraContext
	if b.extraContextSet {
		extraContext = b.extraContext
	}
	mergeExtraContextInto(templateContext, extraContext)

	// These values carry controller state and cannot be replaced by extraContext.
	if b.currentAuxFilesSource == nil {
		templateContext["currentFiles"] = &auxFiles
	}
	templateContext["renderMode"] = string(cmp.Or(b.renderMode, RenderModeReconcile))
	templateContext["admissionSubject"] = b.admissionSubjectOrEmpty()
	templateContext[templating.ResourceDeriverContextName] = derivedResources
	if err := templating.BindImmutableResourceInputs(templateContext, b.readContext); err != nil {
		resourceErrors.Record(fmt.Errorf("binding immutable resource inputs: %w", err))
	}

	if b.config.TemplatingSettings.ExtraContext != nil {
		b.logger.Debug("Added extra context variables to template context",
			"variable_count", len(b.config.TemplatingSettings.ExtraContext))
	}

	return &BuildResult{
		Context:                   templateContext,
		FileRegistry:              fileRegistry,
		PlanRegistry:              planRegistry,
		StatusPatchCollector:      statusPatchCollector,
		RenderedResourceCollector: renderedResourceCollector,
		EventCollector:            eventCollector,
		ResourceErrors:            resourceErrors,
		DerivedResources:          derivedResources,
		currentConfigSource:       b.currentConfigSource,
		currentAuxFilesSource:     b.currentAuxFilesSource,
	}
}

// MaterializeUsedPreviousOutputs installs only compiled-used lazy prior outputs.
func (r *BuildResult) MaterializeUsedPreviousOutputs(useCurrentConfig, useCurrentFiles bool) error {
	if r == nil {
		return errors.New("render context is nil")
	}
	r.previousOutputMu.Lock()
	defer r.previousOutputMu.Unlock()
	if r.previousOutputsErr != nil {
		return r.previousOutputsErr
	}
	if useCurrentConfig && !r.currentConfigReady && r.currentConfigSource != nil {
		if err := r.currentConfigSource.ValidateAuthentication(); err != nil {
			r.previousOutputsErr = fmt.Errorf("authenticating currentConfig: %w", err)
			return r.previousOutputsErr
		}
		current, err := r.currentConfigSource.MaterializeCurrentConfig()
		if err != nil {
			r.previousOutputsErr = fmt.Errorf("materializing currentConfig: %w", err)
			return r.previousOutputsErr
		}
		if current != nil {
			r.Context["currentConfig"] = current
		}
		r.currentConfigReady = true
	}
	if useCurrentFiles && !r.currentAuxFilesReady && r.currentAuxFilesSource != nil {
		if err := r.currentAuxFilesSource.ValidateAuthentication(); err != nil {
			r.previousOutputsErr = fmt.Errorf("authenticating currentFiles: %w", err)
			return r.previousOutputsErr
		}
		files, err := r.currentAuxFilesSource.MaterializeCurrentAuxFiles()
		if err != nil {
			r.previousOutputsErr = fmt.Errorf("materializing currentFiles: %w", err)
			return r.previousOutputsErr
		}
		if files == nil {
			files = map[string]string{}
		}
		r.Context["currentFiles"] = &files
		r.currentAuxFilesReady = true
	}
	return nil
}

// addTypedResources populates ctx["resources"] with a single
// dynamically-built struct value (matching the shape
// [typebootstrap.BuildEngineDeclarations] declared at engine boot).
// Each outer-struct field is a `*innerStore` pointer to a struct
// whose `List` / `Fetch` / `GetSingle` fields are closures over the
// underlying [stores.Store] for that resource.
//
// The closures preserve the existing chart-facing API
// (`resources.X.List()`, `resources.X.Fetch(ns, name)`,
// `resources.X.GetSingle(ns, name)`) — with two improvements vs the
// previous map-of-StoreWrapper design:
//
//   - Return types are typed (`*GeneratedT` / `[]*GeneratedT`) when
//     the resource's schema resolved, so Scriggo type-checks chart
//     field access at engine boot (`res.Metadata.Namespace`).
//   - The per-resource `indexBy` from the chart's
//     `watchedResources` configuration is honoured by Fetch and
//     GetSingle (closures delegate to the underlying StoreWrapper,
//     which already knows the indexBy keys). Resources indexed by a
//     non-default key (e.g. by label or by namespace) keep working
//     without per-call-site JSONPath plumbing.
//
// Method receiver rather than free function so the closures can
// capture `b.stores` / `b.config.WatchedResources` / `b.logger`
// without threading them as arguments.
func (b *Builder) addTypedResources(
	ctx map[string]any,
	resourceErrors *ResourceErrorCollector,
	derivedResources *DerivedResourceView,
) {
	// Single source of truth — delegates to BuildResourcesValue so
	// production renderer, testrunner, and any other consumer
	// produce byte-identical struct shapes. No dual-shape: the
	// engine no longer declares an untyped `resources` default
	// (registerScriggoRuntimeVars dropped it) and consumers that
	// previously relied on the map fallback must now go through
	// helpers.BuildAdditionalDeclarations / supply their own
	// typed declaration. watchedNames mirrors what
	// typebootstrap.BuildEngineDeclarations iterated as extras —
	// every WatchedResources entry gets a field on the resources
	// struct, even those without a generated type.
	var watchedNames []string
	if b.config != nil {
		watchedNames = make([]string, 0, len(b.config.WatchedResources))
		for name := range b.config.WatchedResources {
			watchedNames = append(watchedNames, name)
		}
	}
	ctx["resources"] = buildResourcesValue(
		b.readContext,
		b.stores,
		b.typedResourceTypes,
		watchedNames,
		func(name string) []string {
			if b.config == nil {
				return nil
			}
			if wr, ok := b.config.WatchedResources[name]; ok {
				return wr.IndexBy
			}
			return nil
		},
		func(name string) bool {
			if b.config == nil {
				return false
			}
			if wr, ok := b.config.WatchedResources[name]; ok {
				return wr.Store == storeKindOnDemand
			}
			return false
		},
		func(name string) string {
			if b.config == nil {
				return ""
			}
			// The effective config's APIVersion IS the resolved version
			// (candidate lists are collapsed at iteration start).
			if wr, ok := b.config.WatchedResources[name]; ok {
				return wr.APIVersion
			}
			return ""
		},
		b.scopeFor,
		b.logger,
		resourceErrors,
		nil,
		derivedResources,
		false,
		false,
	)
}

// storeKindOnDemand is the WatchedResource.Store value that selects
// the CachedStore backend; mirrors pkg/k8s/types.StoreTypeCached.String()
// without importing the heavier types package.
const storeKindOnDemand = "on-demand"

// BuildResourcesValue constructs the typed-struct `resources`
// runtime value the engine binds at template-run time. The shape
// matches [typebootstrap.BuildEngineDeclarations] exactly:
//
//	*struct{ <PascalCased-name> *innerStore; … }
//
// with one field per watched resource. The inner store's List /
// Fetch / GetSingle closures return typed pointers when a
// generated type is available, untyped any otherwise — the OUTER
// struct shape stays the same either way, so the runtime value
// keeps binding cleanly against the engine declaration regardless
// of whether typebootstrap produced types for any given resource.
//
// Production is fail-closed on typebootstrap failures (Bootstrap
// aborts iteration startup; helpers.BuildAdditionalDeclarations
// panics on nil Result), so every production engine declares the
// typed struct and this function always produces a typed value.
// There is no map fallback — callers that bypass the typed-engine
// path (a unit test constructing templating.New(...) directly
// without BuildAdditionalDeclarations) must build their own
// map[string]templating.ResourceStore aligned with the engine's
// default declaration in registerScriggoRuntimeVars; do not call
// BuildResourcesValue from that path.
//
// Inputs:
//
//   - ctx: lifetime for API-backed store reads performed by the returned closures.
//   - resourceStores: per-name [stores.Store] for the resources the
//     local controller has live watchers for. Looked up by name; a
//     watched-resource name with no entry gets a struct field whose
//     closures collapse to empty results. Names in this map that are
//     NOT in watchedNames are silently ignored (e.g. the auto-
//     injected haproxy_pods store, which belongs in
//     controller["haproxy_pods"], not `resources`).
//   - typedTypes: per-name generated [reflect.Type] from
//     typebootstrap. Looked up by name; an unset entry yields an
//     untyped-closure field for that resource. Names not in
//     watchedNames are ignored.
//   - watchedNames: every watched-resource name from the config —
//     SOLE iteration source. Must mirror what
//     typebootstrap.BuildEngineDeclarations iterated when it built
//     the engine-side declaration; any field-list drift trips
//     Scriggo's "must have type assignable to struct {...}" bind-
//     time panic.
//   - indexByFor: returns the per-resource IndexBy slice the
//     watcher used to build the underlying store; forwarded to the
//     StoreWrapper so per-render snapshot indices align with the
//     live store state.
//   - lazyFor: returns whether the per-resource wrapper should use
//     LazySnapshot mode. True when the WatchedResource config has
//     `store: on-demand` (CachedStore-backed) — see StoreWrapper's
//     LazySnapshot field documentation for the semantics. A nil
//     callback or one that always returns false keeps the historical
//     eager-snapshot behaviour for every resource.
func BuildResourcesValue(
	ctx context.Context,
	resourceStores map[string]stores.Store,
	typedTypes map[string]reflect.Type,
	watchedNames []string,
	indexByFor func(name string) []string,
	lazyFor func(name string) bool,
	apiVersionFor func(name string) string,
	logger *slog.Logger,
) any {
	return buildResourcesValue(ctx, resourceStores, typedTypes, watchedNames, indexByFor, lazyFor, apiVersionFor, nil, logger, nil, nil, nil, false, false)
}

// BuildResourcesValueWithViews applies transaction-pinned reads and a shared
// derived view. scopeFor returns the namespaces a watch is limited to; a
// lookup into any other namespace fails the render.
func BuildResourcesValueWithViews(
	ctx context.Context,
	resourceStores map[string]stores.Store,
	typedTypes map[string]reflect.Type,
	watchedNames []string,
	indexByFor func(name string) []string,
	lazyFor func(name string) bool,
	apiVersionFor func(name string) string,
	scopeFor func(name string) *NamespaceScope,
	logger *slog.Logger,
	resourceErrors *ResourceErrorCollector,
	snapshotView StoreSnapshotView,
	derivedResources *DerivedResourceView,
	memoizeSnapshotView bool,
) any {
	return buildResourcesValue(
		ctx, resourceStores, typedTypes, watchedNames, indexByFor, lazyFor, apiVersionFor, scopeFor, logger,
		resourceErrors, snapshotView, derivedResources, memoizeSnapshotView, false)
}

// BuildIncrementalResourcesValueWithViews binds every resource call to the
// active component execution environment.
func BuildIncrementalResourcesValueWithViews(
	ctx context.Context,
	resourceStores map[string]stores.Store,
	typedTypes map[string]reflect.Type,
	watchedNames []string,
	indexByFor func(name string) []string,
	lazyFor func(name string) bool,
	apiVersionFor func(name string) string,
	scopeFor func(name string) *NamespaceScope,
	logger *slog.Logger,
	resourceErrors *ResourceErrorCollector,
	snapshotView StoreSnapshotView,
	derivedResources *DerivedResourceView,
	memoizeSnapshotView bool,
) any {
	return buildResourcesValue(
		ctx, resourceStores, typedTypes, watchedNames, indexByFor, lazyFor, apiVersionFor, scopeFor, logger,
		resourceErrors, snapshotView, derivedResources, memoizeSnapshotView, true)
}

func buildResourcesValue(
	ctx context.Context,
	resourceStores map[string]stores.Store,
	typedTypes map[string]reflect.Type,
	watchedNames []string,
	indexByFor func(name string) []string,
	lazyFor func(name string) bool,
	apiVersionFor func(name string) string,
	scopeFor func(name string) *NamespaceScope,
	logger *slog.Logger,
	resourceErrors *ResourceErrorCollector,
	snapshotView StoreSnapshotView,
	derivedResources *DerivedResourceView,
	memoizeSnapshotView bool,
	incrementalEnvironment bool,
) any {
	if indexByFor == nil {
		indexByFor = func(string) []string { return nil }
	}
	if lazyFor == nil {
		lazyFor = func(string) bool { return false }
	}
	if apiVersionFor == nil {
		apiVersionFor = func(string) string { return "" }
	}
	// watchedNames is the SOLE iteration source — it must mirror what
	// typebootstrap.BuildEngineDeclarations iterated when it built the
	// engine-side struct declaration (which itself reduces to "one
	// field per WatchedResource"; see BuildEngineDeclarations comment).
	// Folding extra names in from resourceStores or typedTypes is a
	// trap: provider.StoreNames() in production includes the
	// auto-injected `haproxy_pods` store (it lives in
	// controller["haproxy_pods"], NOT in `resources`), and including
	// it here adds a phantom Haproxy_pods field that doesn't exist on
	// the engine declaration — Scriggo then panics with
	// "must have type assignable to struct {...}" at the first render.
	// typedTypes is always a subset of watchedNames in production, so
	// it adds nothing either. Dedupe watchedNames defensively for the
	// callers that don't.
	seen := make(map[string]struct{}, len(watchedNames))
	for _, name := range watchedNames {
		seen[name] = struct{}{}
	}
	if len(seen) == 0 {
		return reflect.New(reflect.StructOf(nil)).Interface()
	}
	resourceNames := slices.Sorted(maps.Keys(seen))
	var sharedItemCache *ResourceItemCache
	if provider, ok := snapshotView.(interface{ ResourceItemCache() *ResourceItemCache }); ok {
		if candidate := provider.ResourceItemCache(); candidate.valid() {
			sharedItemCache = candidate
		}
	}
	bindingOwner := templating.NewIncrementalResourceFunctionBindingOwner()

	fields := make([]reflect.StructField, 0, len(resourceNames))
	values := make([]reflect.Value, 0, len(resourceNames))
	nativeFunctionBindings := make(
		[]templating.IncrementalResourceFunctionBinding,
		0,
		len(resourceNames)*4,
	)
	var key strings.Builder
	for _, name := range resourceNames {
		elemType := typedTypes[name]
		store := resourceStores[name]
		var wrapper *StoreWrapper
		if store != nil {
			wrapper = &StoreWrapper{
				Store:               store,
				ResourceType:        name,
				Logger:              logger,
				IndexBy:             indexByFor(name),
				Scope:               scopeOf(scopeFor, name),
				LazySnapshot:        lazyFor(name),
				readContext:         ctx,
				resourceErrors:      resourceErrors,
				SnapshotView:        snapshotView,
				DerivedView:         derivedResources,
				MemoizeSnapshotView: memoizeSnapshotView,
			}
		}
		innerType := typebootstrap.BuildPerResourceStoreType(elemType)
		if incrementalEnvironment {
			innerType = typebootstrap.BuildIncrementalPerResourceStoreType(elemType)
		}
		innerValue, innerNativeFunctionBindings := buildPerResourceStoreValue(
			innerType,
			wrapper,
			bindingOwner,
			elemType,
			name,
			apiVersionFor(name),
			logger,
			resourceErrors,
			sharedItemCache,
		)
		nativeFunctionBindings = append(nativeFunctionBindings, innerNativeFunctionBindings...)
		fields = append(fields, reflect.StructField{
			Name: typegen.GoFieldName(name),
			Type: reflect.PointerTo(innerType),
			Tag:  reflect.StructTag(`json:"` + name + `"`),
		})
		values = append(values, innerValue)
		// Key the resources struct type on (field name, inner-type identity).
		// The inner type pointer is stable per config (built once at bootstrap),
		// so a schema reload makes a fresh key rather than a stale hit.
		key.WriteString(name)
		key.WriteByte(0)
		key.WriteString(strconv.FormatUint(uint64(reflect.ValueOf(innerType).Pointer()), 16))
		key.WriteByte(0)
	}
	resourcesType := cachedResourcesType(key.String(), fields)
	resources := reflect.New(resourcesType)
	for i, v := range values {
		resources.Elem().Field(i).Set(v)
	}
	result := resources.Interface()
	if err := templating.RegisterIncrementalResourceFunctionBindings(
		bindingOwner,
		result,
		nativeFunctionBindings...,
	); err != nil {
		panic(fmt.Errorf("registering resource function trampolines: %w", err))
	}
	return result
}

func scopeOf(scopeFor func(string) *NamespaceScope, name string) *NamespaceScope {
	if scopeFor == nil {
		return nil
	}
	return scopeFor(name)
}

// resourcesTypeCache memoises the dynamically-built `resources` struct type.
// buildResourcesValue runs on EVERY render and reflect.StructOf allocates a
// fresh candidate type each call even when its internal cache then returns the
// same *rtype — under render churn this was ~54% of the StructOf allocation
// (issue #178). The type depends only on the watched-resource set + their inner
// store types, all fixed after bootstrap, so caching by that key is safe.
var resourcesTypeCache sync.Map // string key -> reflect.Type

func cachedResourcesType(key string, fields []reflect.StructField) reflect.Type {
	if cached, ok := resourcesTypeCache.Load(key); ok {
		return cached.(reflect.Type)
	}
	t := reflect.StructOf(fields)
	resourcesTypeCache.Store(key, t)
	return t
}

// buildPerResourceStoreValue returns a `*innerType` whose List /
// Fetch / GetSingle fields are closures. The closures wrap the
// underlying `*StoreWrapper` (already aware of the per-resource
// indexBy) and adapt its `[]any` / `any` returns to the typed
// `[]*T` / `*T` shape Scriggo declared at engine boot.
//
// When `elemType` is nil (schema bootstrap failed for this
// resource) the closures pass through untyped values, so chart code
// reaches the same access surface but loses compile-time field
// validation on the element shape.
//
// When `wrapper` is nil (typebootstrap produced a type the local
// controller doesn't watch) the closures return empty results — the
// outer field exists so chart code that reaches it doesn't fail to
// compile.
func buildPerResourceStoreValue(
	innerType reflect.Type,
	wrapper *StoreWrapper,
	bindingOwner *templating.IncrementalResourceFunctionBindingOwner,
	elemType reflect.Type,
	resourceName string,
	apiVersion string,
	logger *slog.Logger,
	resourceErrors *ResourceErrorCollector,
	itemCache *ResourceItemCache,
) (reflect.Value, []templating.IncrementalResourceFunctionBinding) {
	if !itemCache.valid() {
		itemCache = NewResourceItemCache()
	}
	ptr := reflect.New(innerType)
	adapter := perResourceStoreAdapter{
		wrapper:        wrapper,
		bindingOwner:   bindingOwner,
		elemType:       elemType,
		resourceName:   resourceName,
		logger:         logger,
		resourceErrors: resourceErrors,
		itemWrapper: resourceItemWrapper{
			wrapper:      wrapper,
			elemType:     elemType,
			resourceName: resourceName,
			cache:        itemCache,
		},
	}
	nativeFunctions := adapter.bind(ptr.Elem(), apiVersion)

	return ptr, nativeFunctions
}

type perResourceStoreAdapter struct {
	wrapper        *StoreWrapper
	bindingOwner   *templating.IncrementalResourceFunctionBindingOwner
	elemType       reflect.Type
	resourceName   string
	logger         *slog.Logger
	resourceErrors *ResourceErrorCollector
	itemWrapper    resourceItemWrapper
	listOnce       sync.Once
	listResult     reflect.Value
	listErr        error
}

func (a *perResourceStoreAdapter) bind(
	elem reflect.Value,
	apiVersion string,
) []templating.IncrementalResourceFunctionBinding {
	return []templating.IncrementalResourceFunctionBinding{
		a.bindAPIVersion(elem.FieldByName("APIVersion"), apiVersion),
		a.bindList(elem.FieldByName("List")),
		a.bindFetch(elem.FieldByName("Fetch")),
		a.bindGetSingle(elem.FieldByName("GetSingle")),
	}
}

func (a *perResourceStoreAdapter) bindAPIVersion(
	field reflect.Value,
	apiVersion string,
) templating.IncrementalResourceFunctionBinding {
	trampoline := native.MakeFunctionTrampolineWithFrame(
		field.Type(),
		func(_ []reflect.Value) []reflect.Value {
			runtime.KeepAlive(a.bindingOwner)
			return []reflect.Value{reflect.ValueOf(apiVersion)}
		},
		func(frame native.FunctionCallFrame) {
			frame.SetResultString(0, apiVersion)
			runtime.KeepAlive(a.bindingOwner)
		},
	)
	field.Set(trampoline.Value())
	return templating.IncrementalResourceFunctionBinding{Trampoline: trampoline}
}

func (a *perResourceStoreAdapter) bindList(
	field reflect.Value,
) templating.IncrementalResourceFunctionBinding {
	returnType := field.Type().Out(0)
	materialization := newDirectBoundResourceMaterializationRequest(
		a.itemWrapper.cache,
		a.resourceName,
		a.elemType,
		returnType,
		DirectBoundResourceList,
		func(ctx context.Context, items []any) reflect.Value {
			return a.buildListItemsImmutable(ctx, items, returnType)
		},
		a.resourceErrors,
		a.logger,
	)
	trampoline := native.MakeFunctionTrampolineWithFrame(
		field.Type(),
		func(args []reflect.Value) []reflect.Value {
			env, ctx, _ := resourceInvocationEnvironment(field.Type(), args)
			result, err := a.adaptList(ctx, returnType)
			if err != nil {
				return resourceInvocationFailure(env, returnType, err)
			}
			return []reflect.Value{result}
		},
		func(frame native.FunctionCallFrame) {
			env, ctx := resourceInvocationFrameEnvironment(field.Type(), frame)
			result, err := a.adaptList(ctx, returnType)
			if err != nil {
				resourceInvocationFrameFailure(frame, env, err)
				return
			}
			frame.SetResultValue(0, result)
		},
	)
	field.Set(trampoline.Value())
	return templating.IncrementalResourceFunctionBinding{
		Trampoline: trampoline,
		BoundFrameFactory: a.boundResourceFrameFactory(
			field.Type(),
			returnType,
			func(ctx context.Context, _ []any) (reflect.Value, error) {
				return a.adaptListInInvocation(ctx, returnType)
			},
			func(
				ctx context.Context,
				invocation DirectBoundStoreInvocation,
				_ resourceInvocationKeys,
			) (reflect.Value, error) {
				return a.adaptListDirectBound(ctx, invocation, returnType, materialization)
			},
		),
	}
}

func (a *perResourceStoreAdapter) adaptList(ctx context.Context, returnType reflect.Type) (reflect.Value, error) {
	adapt := func() (reflect.Value, error) {
		if a.wrapper == nil {
			return reflect.MakeSlice(returnType, 0, 0), nil
		}
		invocationCtx, release, err := a.wrapper.beginStoreInvocation(ctx)
		if err != nil {
			return reflect.Value{}, err
		}
		defer release()
		return a.adaptListInInvocation(invocationCtx, returnType)
	}
	if a.elemType == nil || (a.wrapper != nil && !a.wrapper.memoizeStoreMaterialization()) {
		return adapt()
	}
	a.listOnce.Do(func() {
		a.listResult, a.listErr = adapt()
	})
	return a.listResult, a.listErr
}

func (a *perResourceStoreAdapter) adaptListInInvocation(
	ctx context.Context,
	returnType reflect.Type,
) (reflect.Value, error) {
	if a.wrapper == nil {
		return reflect.MakeSlice(returnType, 0, 0), nil
	}
	items, err := a.wrapper.listInInvocation(ctx)
	if err != nil {
		return reflect.Value{}, err
	}
	return a.adaptListItems(ctx, items, returnType)
}

func (a *perResourceStoreAdapter) adaptListDirectBound(
	ctx context.Context,
	invocation DirectBoundStoreInvocation,
	returnType reflect.Type,
	materialization *DirectBoundResourceMaterializationRequest,
) (reflect.Value, error) {
	if result, supported, err := a.wrapper.materializeDirectBoundResource(
		ctx, invocation, materialization, nil,
	); supported {
		return result, err
	}
	items, err := a.wrapper.listDirectBoundStoreInvocation(ctx, invocation)
	if err != nil {
		return reflect.Value{}, err
	}
	return a.adaptListItems(ctx, items, returnType)
}

func (a *perResourceStoreAdapter) adaptListItems(
	ctx context.Context,
	items []any,
	returnType reflect.Type,
) (reflect.Value, error) {
	result := a.buildListItems(ctx, items, returnType)
	if err := registerIncrementalResourceResult(a.wrapper, ctx, result); err != nil {
		return reflect.Value{}, err
	}
	return result, nil
}

func (a *perResourceStoreAdapter) buildListItems(
	ctx context.Context,
	items []any,
	returnType reflect.Type,
) reflect.Value {
	return adaptSliceForResource(
		ctx, items, returnType, a.elemType, a.resourceName, "List",
		a.logger, a.resourceErrors, a.itemWrapper.wrap,
	)
}

func (a *perResourceStoreAdapter) buildListItemsImmutable(
	ctx context.Context,
	items []any,
	returnType reflect.Type,
) reflect.Value {
	return adaptSliceForResource(
		ctx, items, returnType, a.elemType, a.resourceName, "List",
		a.logger, a.resourceErrors, a.itemWrapper.wrapImmutable,
	)
}

func (a *perResourceStoreAdapter) bindFetch(
	field reflect.Value,
) templating.IncrementalResourceFunctionBinding {
	returnType := field.Type().Out(0)
	materialization := newDirectBoundResourceMaterializationRequest(
		a.itemWrapper.cache,
		a.resourceName,
		a.elemType,
		returnType,
		DirectBoundResourceFetch,
		func(ctx context.Context, items []any) reflect.Value {
			return a.buildFetchItemsImmutable(ctx, items, returnType)
		},
		a.resourceErrors,
		a.logger,
	)
	trampoline := native.MakeFunctionTrampolineWithFrame(
		field.Type(),
		func(args []reflect.Value) []reflect.Value {
			env, ctx, offset := resourceInvocationEnvironment(field.Type(), args)
			if a.wrapper == nil {
				return []reflect.Value{reflect.MakeSlice(returnType, 0, 0)}
			}
			invocationCtx, release, err := a.wrapper.beginStoreInvocation(ctx)
			if err != nil {
				return resourceInvocationFailure(env, returnType, err)
			}
			defer release()
			keys := args[offset].Interface().([]any)
			result, err := a.adaptFetchInInvocation(invocationCtx, keys, returnType)
			if err != nil {
				return resourceInvocationFailure(env, returnType, err)
			}
			return []reflect.Value{result}
		},
		func(frame native.FunctionCallFrame) {
			env, ctx := resourceInvocationFrameEnvironment(field.Type(), frame)
			if a.wrapper == nil {
				frame.SetResultValue(0, reflect.MakeSlice(returnType, 0, 0))
				return
			}
			invocationCtx, release, err := a.wrapper.beginStoreInvocation(ctx)
			if err != nil {
				resourceInvocationFrameFailure(frame, env, err)
				return
			}
			defer release()
			result, err := a.adaptFetchInInvocation(
				invocationCtx,
				resourceInvocationFrameVariadic(field.Type(), frame),
				returnType,
			)
			if err != nil {
				resourceInvocationFrameFailure(frame, env, err)
				return
			}
			frame.SetResultValue(0, result)
		},
	)
	field.Set(trampoline.Value())
	return templating.IncrementalResourceFunctionBinding{
		Trampoline: trampoline,
		BoundFrameFactory: a.boundResourceFrameFactory(
			field.Type(),
			returnType,
			func(ctx context.Context, keys []any) (reflect.Value, error) {
				return a.adaptFetchInInvocation(ctx, keys, returnType)
			},
			func(
				ctx context.Context,
				invocation DirectBoundStoreInvocation,
				keys resourceInvocationKeys,
			) (reflect.Value, error) {
				return a.adaptFetchDirectBound(ctx, invocation, keys, returnType, materialization)
			},
		),
	}
}

func (a *perResourceStoreAdapter) adaptFetchInInvocation(
	ctx context.Context,
	keys []any,
	returnType reflect.Type,
) (reflect.Value, error) {
	if a.wrapper == nil {
		return reflect.MakeSlice(returnType, 0, 0), nil
	}
	items, err := a.wrapper.fetchInInvocation(ctx, keys)
	if err != nil {
		return reflect.Value{}, err
	}
	return a.adaptFetchItems(ctx, items, returnType)
}

func (a *perResourceStoreAdapter) adaptFetchDirectBound(
	ctx context.Context,
	invocation DirectBoundStoreInvocation,
	keys resourceInvocationKeys,
	returnType reflect.Type,
	materialization *DirectBoundResourceMaterializationRequest,
) (reflect.Value, error) {
	if _, supported := a.wrapper.directBoundResourceMaterializationView(); supported {
		stringKeys, ok := a.wrapper.lookupKeySource(keys, "Fetch")
		if !ok {
			return reflect.MakeSlice(returnType, 0, 0), nil
		}
		result, _, err := a.wrapper.materializeDirectBoundResource(
			ctx, invocation, materialization, stringKeys,
		)
		return result, err
	}
	items, err := a.wrapper.getDirectBoundStoreInvocation(ctx, invocation, keys, "Fetch")
	if err != nil {
		return reflect.Value{}, err
	}
	return a.adaptFetchItems(ctx, items, returnType)
}

func (a *perResourceStoreAdapter) adaptFetchItems(
	ctx context.Context,
	items []any,
	returnType reflect.Type,
) (reflect.Value, error) {
	result := a.buildFetchItems(ctx, items, returnType)
	if err := registerIncrementalResourceResult(a.wrapper, ctx, result); err != nil {
		return reflect.Value{}, err
	}
	return result, nil
}

func (a *perResourceStoreAdapter) buildFetchItems(
	ctx context.Context,
	items []any,
	returnType reflect.Type,
) reflect.Value {
	return adaptSliceForResource(
		ctx, items, returnType, a.elemType, a.resourceName, "Fetch",
		a.logger, a.resourceErrors, a.itemWrapper.wrap,
	)
}

func (a *perResourceStoreAdapter) buildFetchItemsImmutable(
	ctx context.Context,
	items []any,
	returnType reflect.Type,
) reflect.Value {
	return adaptSliceForResource(
		ctx, items, returnType, a.elemType, a.resourceName, "Fetch",
		a.logger, a.resourceErrors, a.itemWrapper.wrapImmutable,
	)
}

func (a *perResourceStoreAdapter) bindGetSingle(
	field reflect.Value,
) templating.IncrementalResourceFunctionBinding {
	returnType := field.Type().Out(0)
	materialization := newDirectBoundResourceMaterializationRequest(
		a.itemWrapper.cache,
		a.resourceName,
		a.elemType,
		returnType,
		DirectBoundResourceGetSingle,
		func(ctx context.Context, items []any) reflect.Value {
			return a.buildSingleItemImmutable(ctx, items[0], returnType)
		},
		a.resourceErrors,
		a.logger,
	)
	trampoline := native.MakeFunctionTrampolineWithFrame(
		field.Type(),
		func(args []reflect.Value) []reflect.Value {
			env, ctx, offset := resourceInvocationEnvironment(field.Type(), args)
			if a.wrapper == nil {
				return []reflect.Value{reflect.Zero(returnType)}
			}
			invocationCtx, release, err := a.wrapper.beginStoreInvocation(ctx)
			if err != nil {
				return resourceInvocationFailure(env, returnType, err)
			}
			defer release()
			keys := args[offset].Interface().([]any)
			result, err := a.adaptGetSingleInInvocation(invocationCtx, keys, returnType)
			if err != nil {
				return resourceInvocationFailure(env, returnType, err)
			}
			return []reflect.Value{result}
		},
		func(frame native.FunctionCallFrame) {
			env, ctx := resourceInvocationFrameEnvironment(field.Type(), frame)
			if a.wrapper == nil {
				frame.SetResultZero(0)
				return
			}
			invocationCtx, release, err := a.wrapper.beginStoreInvocation(ctx)
			if err != nil {
				resourceInvocationFrameFailure(frame, env, err)
				return
			}
			defer release()
			result, err := a.adaptGetSingleInInvocation(
				invocationCtx,
				resourceInvocationFrameVariadic(field.Type(), frame),
				returnType,
			)
			if err != nil {
				resourceInvocationFrameFailure(frame, env, err)
				return
			}
			frame.SetResultValue(0, result)
		},
	)
	field.Set(trampoline.Value())
	return templating.IncrementalResourceFunctionBinding{
		Trampoline: trampoline,
		BoundFrameFactory: a.boundResourceFrameFactory(
			field.Type(),
			returnType,
			func(ctx context.Context, keys []any) (reflect.Value, error) {
				return a.adaptGetSingleInInvocation(ctx, keys, returnType)
			},
			func(
				ctx context.Context,
				invocation DirectBoundStoreInvocation,
				keys resourceInvocationKeys,
			) (reflect.Value, error) {
				return a.adaptGetSingleDirectBound(ctx, invocation, keys, returnType, materialization)
			},
		),
	}
}

func (a *perResourceStoreAdapter) adaptGetSingleInInvocation(
	ctx context.Context,
	keys []any,
	returnType reflect.Type,
) (reflect.Value, error) {
	if a.wrapper == nil {
		return reflect.Zero(returnType), nil
	}
	item, found, err := a.wrapper.getSingleInInvocation(ctx, keys)
	if err != nil {
		return reflect.Value{}, err
	}
	if !found {
		return reflect.Zero(returnType), nil
	}
	return a.adaptSingleItem(ctx, item, returnType)
}

func (a *perResourceStoreAdapter) adaptGetSingleDirectBound(
	ctx context.Context,
	invocation DirectBoundStoreInvocation,
	keys resourceInvocationKeys,
	returnType reflect.Type,
	materialization *DirectBoundResourceMaterializationRequest,
) (reflect.Value, error) {
	if _, supported := a.wrapper.directBoundResourceMaterializationView(); supported {
		stringKeys, ok := a.wrapper.lookupKeySource(keys, "GetSingle")
		if !ok {
			return reflect.Zero(returnType), nil
		}
		result, _, err := a.wrapper.materializeDirectBoundResource(
			ctx, invocation, materialization, stringKeys,
		)
		return result, err
	}
	item, found, err := a.wrapper.getSingleDirectBoundStoreInvocation(ctx, invocation, keys)
	if err != nil {
		return reflect.Value{}, err
	}
	if !found {
		return reflect.Zero(returnType), nil
	}
	return a.adaptSingleItem(ctx, item, returnType)
}

func (a *perResourceStoreAdapter) adaptSingleItem(
	ctx context.Context,
	item any,
	returnType reflect.Type,
) (reflect.Value, error) {
	result := a.buildSingleItem(ctx, item, returnType)
	if err := registerIncrementalResourceResult(a.wrapper, ctx, result); err != nil {
		return reflect.Value{}, err
	}
	return result, nil
}

func (a *perResourceStoreAdapter) buildSingleItem(
	ctx context.Context,
	item any,
	returnType reflect.Type,
) reflect.Value {
	return adaptSingleForResource(
		ctx, item, returnType, a.elemType, a.resourceName,
		a.logger, a.resourceErrors, a.itemWrapper.wrap,
	)
}

func (a *perResourceStoreAdapter) buildSingleItemImmutable(
	ctx context.Context,
	item any,
	returnType reflect.Type,
) reflect.Value {
	return adaptSingleForResource(
		ctx, item, returnType, a.elemType, a.resourceName,
		a.logger, a.resourceErrors, a.itemWrapper.wrapImmutable,
	)
}

type boundResourceInvocation func(context.Context, []any) (reflect.Value, error)

type directBoundResourceInvocation func(
	context.Context,
	DirectBoundStoreInvocation,
	resourceInvocationKeys,
) (reflect.Value, error)

type resourceInvocationKeys struct {
	values    []any
	arguments *native.FunctionCallArguments
}

func (k resourceInvocationKeys) Len() int {
	if k.arguments != nil {
		return k.arguments.Len()
	}
	return len(k.values)
}

func (k resourceInvocationKeys) Value(index int) any {
	if k.arguments != nil {
		return native.FunctionCallArgumentAt[any](*k.arguments, index)
	}
	return k.values[index]
}

func (k resourceInvocationKeys) ReflectValue(index int) reflect.Value {
	if k.arguments != nil {
		return k.arguments.Value(index)
	}
	if k.values[index] == nil {
		return reflect.Value{}
	}
	return reflect.ValueOf(k.values[index])
}

func (k resourceInvocationKeys) slice() []any {
	if k.arguments == nil {
		return k.values
	}
	values := make([]any, k.arguments.Len())
	for index := range values {
		values[index] = native.FunctionCallArgumentAt[any](*k.arguments, index)
	}
	return values
}

func (a *perResourceStoreAdapter) boundResourceFrameFactory(
	functionType reflect.Type,
	returnType reflect.Type,
	invoke boundResourceInvocation,
	directInvoke directBoundResourceInvocation,
) templating.IncrementalResourceBoundFrameFactory {
	if !a.wrapper.supportsBoundStoreInvocation() || functionType == nil ||
		functionType.Kind() != reflect.Func || functionType.NumIn() == 0 ||
		functionType.In(0) != reflect.TypeFor[native.Env]() || functionType.NumOut() != 1 ||
		functionType.Out(0) != returnType || invoke == nil || directInvoke == nil {
		return nil
	}
	return func(
		lease templating.IncrementalResourceInvocationLease,
	) (*native.FunctionTrampoline, error) {
		if lease == nil {
			return nil, errors.New("bound resource frame requires an invocation lease")
		}
		call := func(args []reflect.Value) []reflect.Value {
			env, ctx, offset := resourceInvocationEnvironment(functionType, args)
			var keys resourceInvocationKeys
			if functionType.IsVariadic() {
				keys.values = args[offset].Interface().([]any)
			}
			result, err := a.invokeBoundResource(ctx, lease, keys, invoke, directInvoke)
			if err != nil {
				return resourceInvocationFailure(env, returnType, err)
			}
			return []reflect.Value{result}
		}
		return native.MakeFunctionTrampolineWithFrame(
			functionType,
			call,
			func(frame native.FunctionCallFrame) {
				env, ctx := resourceInvocationFrameEnvironment(functionType, frame)
				var keys resourceInvocationKeys
				if functionType.IsVariadic() {
					arguments := frame.VariadicArguments()
					keys.arguments = &arguments
				}
				result, err := a.invokeBoundResource(ctx, lease, keys, invoke, directInvoke)
				if err != nil {
					resourceInvocationFrameFailure(frame, env, err)
					return
				}
				frame.SetResultValue(0, result)
			},
		), nil
	}
}

func (a *perResourceStoreAdapter) invokeBoundResource(
	ctx context.Context,
	lease templating.IncrementalResourceInvocationLease,
	keys resourceInvocationKeys,
	invoke boundResourceInvocation,
	directInvoke directBoundResourceInvocation,
) (reflect.Value, error) {
	result := a.invokeBoundResourceResult(ctx, lease, keys, invoke, directInvoke)
	return result.value, result.err
}

type boundResourceInvocationResult struct {
	value reflect.Value
	err   error
}

func (a *perResourceStoreAdapter) invokeBoundResourceResult(
	ctx context.Context,
	lease templating.IncrementalResourceInvocationLease,
	keys resourceInvocationKeys,
	invoke boundResourceInvocation,
	directInvoke directBoundResourceInvocation,
) (result boundResourceInvocationResult) {
	if a.wrapper.supportsDirectBoundStoreInvocation() {
		invocation, beginErr := a.wrapper.beginDirectBoundStoreInvocation(ctx, lease)
		if beginErr != nil {
			return boundResourceInvocationResult{err: beginErr}
		}
		defer finishDirectBoundStoreInvocation(a.wrapper, invocation, &result)
		result.value, result.err = directInvoke(ctx, invocation, keys)
		return result
	}
	invocationCtx, release, err := a.wrapper.beginBoundStoreInvocation(ctx, lease)
	if err != nil {
		return boundResourceInvocationResult{err: err}
	}
	defer release()
	result.value, result.err = invoke(invocationCtx, keys.slice())
	return result
}

func finishDirectBoundStoreInvocation(
	wrapper *StoreWrapper,
	invocation DirectBoundStoreInvocation,
	result *boundResourceInvocationResult,
) {
	if endErr := wrapper.endDirectBoundStoreInvocation(invocation); endErr != nil {
		result.value = reflect.Value{}
		result.err = errors.Join(result.err, endErr)
	}
}

func resourceInvocationEnvironment(
	functionType reflect.Type,
	args []reflect.Value,
) (native.Env, context.Context, int) {
	if functionType.NumIn() > 0 && functionType.In(0) == reflect.TypeFor[native.Env]() {
		env := args[0].Interface().(native.Env)
		return env, env.Context(), 1
	}
	return nil, nil, 0
}

func resourceInvocationFailure(env native.Env, returnType reflect.Type, err error) []reflect.Value {
	if env != nil {
		env.Stop(err)
	}
	return []reflect.Value{reflect.Zero(returnType)}
}

func resourceInvocationFrameEnvironment(
	functionType reflect.Type,
	frame native.FunctionCallFrame,
) (native.Env, context.Context) {
	if functionType.NumIn() > 0 && functionType.In(0) == reflect.TypeFor[native.Env]() {
		env := frame.ArgEnv(0)
		return env, env.Context()
	}
	return nil, nil
}

func resourceInvocationFrameVariadic(
	functionType reflect.Type,
	frame native.FunctionCallFrame,
) []any {
	count := frame.VariadicLen()
	if count < 0 {
		values, _ := frame.ArgValue(functionType.NumIn() - 1).Interface().([]any)
		return values
	}
	values := make([]any, count)
	for index := range count {
		values[index] = frame.VariadicValue(index).Interface()
	}
	return values
}

func resourceInvocationFrameFailure(
	frame native.FunctionCallFrame,
	env native.Env,
	err error,
) {
	if env != nil {
		env.Stop(err)
	}
	for index := range frame.Type().NumOut() {
		frame.SetResultZero(index)
	}
}

type resourceItemWrapper struct {
	wrapper      *StoreWrapper
	elemType     reflect.Type
	resourceName string
	cache        *ResourceItemCache
}

func (w *resourceItemWrapper) wrap(ctx context.Context, item any) (reflect.Value, error) {
	return w.wrapWith(ctx, item, w.wrapAndBind)
}

func (w *resourceItemWrapper) wrapImmutable(ctx context.Context, item any) (reflect.Value, error) {
	return w.wrapWith(ctx, item, w.wrapAndBindImmutable)
}

func (w *resourceItemWrapper) wrapWith(
	ctx context.Context,
	item any,
	build func(any) (reflect.Value, error),
) (reflect.Value, error) {
	if w.usesUnmemoizedSnapshotView() {
		return build(item)
	}
	key, cacheable := resourceItemKey(w.resourceName, w.elemType, item)
	if !cacheable {
		return build(item)
	}
	return w.wrapMemoized(ctx, item, key, build)
}

func (w *resourceItemWrapper) usesUnmemoizedSnapshotView() bool {
	return w.wrapper != nil && w.wrapper.usesSnapshotView() && !w.wrapper.memoizeStoreItems()
}

func (w *resourceItemWrapper) wrapMemoized(
	ctx context.Context,
	item any,
	key resourceItemCacheKey,
	build func(any) (reflect.Value, error),
) (reflect.Value, error) {
	value, found, err := w.cache.load(key, item)
	if err != nil {
		return reflect.Value{}, err
	}
	if found {
		if err := templating.RegisterIncrementalImmutableCertificate(ctx, value.certificate); err != nil {
			return reflect.Value{}, err
		}
		return value.value, nil
	}
	wrapped, err := build(item)
	if err != nil {
		return reflect.Value{}, err
	}
	value, err = w.cache.loadOrStore(
		key,
		item,
		wrapped,
		templating.CertifyIncrementalImmutableInputs(wrapped.Interface()),
	)
	if err != nil {
		return reflect.Value{}, err
	}
	if err := templating.RegisterIncrementalImmutableCertificate(ctx, value.certificate); err != nil {
		return reflect.Value{}, err
	}
	return value.value, nil
}

func (w *resourceItemWrapper) wrapAndBind(item any) (reflect.Value, error) {
	return w.wrapAndBindWith(item, wrapItemToPointer)
}

func (w *resourceItemWrapper) wrapAndBindImmutable(item any) (reflect.Value, error) {
	return w.wrapAndBindWith(item, wrapImmutableItemToPointer)
}

func (w *resourceItemWrapper) wrapAndBindWith(
	item any,
	wrap func(any, reflect.Type) (reflect.Value, error),
) (reflect.Value, error) {
	wrapped, err := wrap(item, w.elemType)
	if err != nil {
		return reflect.Value{}, err
	}
	if w.wrapper == nil || w.wrapper.DerivedView == nil {
		return wrapped, nil
	}
	if err := w.wrapper.DerivedView.Bind(w.resourceName, wrapped.Interface(), item); err != nil {
		return reflect.Value{}, err
	}
	return wrapped, nil
}

func registerIncrementalResourceResult(wrapper *StoreWrapper, ctx context.Context, result reflect.Value) error {
	if wrapper == nil || !result.IsValid() {
		return nil
	}
	return templating.RegisterIncrementalImmutableInputs(ctx, result.Interface())
}

// adaptSliceForResource converts `items []any` to the static return
// type (`[]*T` typed or `[]any` untyped). For typed slices it runs
// each item through `typegen.WrapInto` — that's the same path the
// pre-pivot direct-WrapSlice approach used, just deferred to call
// time so per-render List() picks up the freshest store snapshot.
func adaptSliceForResource(
	ctx context.Context,
	items []any,
	returnType reflect.Type,
	elemType reflect.Type,
	resourceName, op string,
	logger *slog.Logger,
	resourceErrors *ResourceErrorCollector,
	wrapItem func(context.Context, any) (reflect.Value, error),
) reflect.Value {
	if elemType == nil {
		// Untyped fallback: return type is []any. Direct copy.
		out := reflect.MakeSlice(returnType, len(items), len(items))
		for i, item := range items {
			if item == nil {
				continue
			}
			out.Index(i).Set(reflect.ValueOf(item))
		}
		return out
	}
	// Typed: each item becomes *T via the per-render memoized wrapItem. A
	// failed conversion is recorded so the render can't publish partial input.
	out := reflect.MakeSlice(returnType, 0, len(items))
	for _, item := range items {
		ptr, err := wrapItem(ctx, item)
		if err != nil {
			resourceErrors.Record(fmt.Errorf("resource %q %s could not materialize its typed object: %w", resourceName, op, err))
			logger.Warn("Typed resource: WrapInto failed; skipping item",
				"resource", resourceName, "op", op, "error", err)
			continue
		}
		out = reflect.Append(out, ptr)
	}
	return out
}

// adaptSingleForResource wraps the wrapper's `any` return value into
// the static return type (`*T` typed, or `any` untyped). Returns the
// zero value of the return type for nil input.
func adaptSingleForResource(
	ctx context.Context,
	item any,
	returnType reflect.Type,
	elemType reflect.Type,
	resourceName string,
	logger *slog.Logger,
	resourceErrors *ResourceErrorCollector,
	wrapItem func(context.Context, any) (reflect.Value, error),
) reflect.Value {
	if item == nil {
		return reflect.Zero(returnType)
	}
	if elemType == nil {
		// Untyped fallback: return type is `any`. Reflect-wrap so
		// the interface assignment goes through cleanly.
		out := reflect.New(returnType).Elem()
		out.Set(reflect.ValueOf(item))
		return out
	}
	// Memoized wrap so GetSingle returns the SAME *T as List/Fetch for the
	// same snapshot item within this render.
	ptr, err := wrapItem(ctx, item)
	if err != nil {
		resourceErrors.Record(fmt.Errorf("resource %q GetSingle could not materialize its typed object: %w", resourceName, err))
		logger.Warn("Typed resource: WrapInto failed; returning nil",
			"resource", resourceName, "op", "GetSingle", "error", err)
		return reflect.Zero(returnType)
	}
	return ptr
}

// wrapItemToPointer converts a single store item (typically
// map[string]any from the dynamic client) into a typed `*elemType`
// via typegen.WrapInto. Returns a reflect.Value wrapping the
// pointer.
func wrapItemToPointer(item any, elemType reflect.Type) (reflect.Value, error) {
	return wrapItemToPointerWith(item, elemType, typegen.WrapInto)
}

func wrapImmutableItemToPointer(item any, elemType reflect.Type) (reflect.Value, error) {
	m, ok := item.(map[string]any)
	if !ok {
		return reflect.Value{}, fmt.Errorf("expected map[string]any, got %T", item)
	}
	return typegen.WrapImmutableIntoPointer(m, elemType)
}

func wrapItemToPointerWith(
	item any,
	elemType reflect.Type,
	wrap func(map[string]any, reflect.Type) (reflect.Value, error),
) (reflect.Value, error) {
	m, ok := item.(map[string]any)
	if !ok {
		return reflect.Value{}, fmt.Errorf("expected map[string]any, got %T", item)
	}
	v, err := wrap(m, elemType)
	if err != nil {
		return reflect.Value{}, err
	}
	ptr := reflect.New(elemType)
	ptr.Elem().Set(v)
	return ptr, nil
}

// SortSnippetNames sorts template snippet names alphabetically.
// Returns a slice of snippet names in sorted order.
//
// Snippet ordering is controlled by encoding priority in the snippet name
// (e.g., "features-050-ssl" for priority 50). This is required because
// render_glob sorts templates alphabetically.
func SortSnippetNames(snippets map[string]config.TemplateSnippet) []string {
	sorted := make([]string, 0, len(snippets))
	for name := range snippets {
		sorted = append(sorted, name)
	}
	slices.Sort(sorted)
	return sorted
}

// MergeExtraContextInto merges the extraContext variables from the config into the provided template context.
//
// This allows templates to access custom variables directly (e.g., {{ debug.enabled }})
// instead of wrapping them in a "config" object.
//
// The extraContext key is always populated (with an empty map if nil) to prevent
// nil pointer dereferences in templates that use extraContext | dig("key") | fallback(default).
func MergeExtraContextInto(renderCtx map[string]any, cfg *config.Config) {
	mergeExtraContextInto(renderCtx, cfg.TemplatingSettings.ExtraContext)
}

func mergeExtraContextInto(renderCtx, extraContext map[string]any) {
	if extraContext != nil {
		// Merge at top level
		maps.Copy(renderCtx, extraContext)
		// Also populate the extraContext map for Scriggo templates
		// Scriggo requires compile-time variable declarations, so templates
		// access extraContext values via: extraContext | dig("key") | fallback(default)
		renderCtx["extraContext"] = extraContext
	} else {
		// Always set extraContext, even if empty, to prevent nil pointer dereferences
		// when templates use: extraContext | dig("key") | fallback(default)
		renderCtx["extraContext"] = map[string]any{}
	}
}

// DetachExtraContext returns a recursively isolated template value tree.
func DetachExtraContext(extraContext map[string]any) (map[string]any, error) {
	if extraContext == nil {
		return map[string]any{}, nil
	}
	cloned, err := cloneTemplateValue(extraContext)
	if err != nil {
		return nil, err
	}
	result, ok := cloned.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("detached extra context has type %T", cloned)
	}
	return result, nil
}

// CapabilitiesToMap converts a Capabilities struct to a template-friendly map.
// The map uses snake_case keys matching the Capabilities struct field names
// (e.g., "supports_crt_list" for SupportsCrtList) for consistency with template
// conventions.
func CapabilitiesToMap(caps *dataplane.Capabilities) map[string]any {
	if caps == nil {
		return map[string]any{}
	}

	return map[string]any{
		// Storage capabilities
		"supports_crt_list":        caps.SupportsCrtList,
		"supports_map_storage":     caps.SupportsMapStorage,
		"supports_general_storage": caps.SupportsGeneralStorage,
		"supports_ssl_ca_files":    caps.SupportsSslCaFiles,
		"supports_ssl_crl_files":   caps.SupportsSslCrlFiles,

		// Configuration capabilities
		"supports_http2":              caps.SupportsHTTP2,
		"supports_quic":               caps.SupportsQUIC,
		"supports_quic_initial_rules": caps.SupportsQUICInitialRules,

		// Observability capabilities
		"supports_log_profiles": caps.SupportsLogProfiles,
		"supports_traces":       caps.SupportsTraces,

		// Certificate automation
		"supports_acme_providers": caps.SupportsAcmeProviders,

		// Runtime capabilities
		"supports_runtime_maps":    caps.SupportsRuntimeMaps,
		"supports_runtime_servers": caps.SupportsRuntimeServers,
	}
}
