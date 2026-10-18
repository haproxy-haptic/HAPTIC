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
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	iradix "github.com/hashicorp/go-immutable-radix/v2"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/helpers"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/httpstore"
	"github.com/haproxy-haptic/HAPTIC/pkg/incremental"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/typegen"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

type incrementalComponent struct {
	name               string
	entryPoint         string
	source             string
	root               string
	group              string
	consumes           []string
	optionalConsumes   []string
	activationPaths    []templating.ExistenceJSONPath
	resourceProjection bool
	deriveResource     bool
	recordEvent        bool
	backendPlan        bool
	publishValue       bool
	statusPatch        bool
}

type incrementalStoreCursor struct {
	source   stores.RevisionSource
	sequence uint64
	diffBase *incrementalSnapshotDiffBase
}

type incrementalSnapshotDiffBase struct {
	snapshot stores.ReadSnapshot
}

type incrementalHTTPCursor struct {
	token httpstore.ActiveLeaseToken
}

type httpInputIdentity struct {
	url        string
	descriptor httpstore.SourceDescriptor
}

type httpInputSpec struct {
	id uint64
	httpInputIdentity
}

type incrementalHTTPEffect struct {
	inputID  uint64
	snapshot httpstore.ContentSnapshot
}

type httpRefDelta struct {
	added   uint64
	removed uint64
}

type resourceInputScope string

const (
	resourceInputList     resourceInputScope = "list"
	resourceInputGet      resourceInputScope = "get"
	resourceInputIdentity resourceInputScope = "identity"
)

type resourceInputSpec struct {
	resourceType string
	scope        resourceInputScope
	keys         []string
	namespace    string
	name         string
	keyCache     *resourceInputKeyCache
}

type resourceInputKeyCache struct {
	seal         *resourceInputKeyCache
	proof        *resourceInputKeyCacheProof
	key          incremental.InputKey
	resourceType string
	scope        resourceInputScope
	keys         []string
	namespace    string
	name         string
}

type resourceInputKeyCacheProof struct {
	seal         *resourceInputKeyCacheProof
	cache        *resourceInputKeyCache
	key          incremental.InputKey
	resourceType string
	scope        resourceInputScope
	keys         []string
	namespace    string
	name         string
}

type incrementalStateSnapshot struct {
	cursors      map[string]incrementalStoreCursor
	httpCursor   incrementalHTTPCursor
	bindings     *iradix.Tree[string]
	members      *iradix.Tree[struct{}]
	activeGroups *incrementalActiveGroupIndex
	retired      *iradix.Tree[struct{}]
	results      *iradix.Tree[incremental.ExactValueRoot]
	derived      *iradix.Tree[incrementalDerivedResource]
	httpEffects  *iradix.Tree[*iradix.Tree[incrementalHTTPEffect]]
	catalog      *incrementalResourceCatalogSnapshot
	groupIndexes map[string]*incrementalGroupIndex
	groupReady   map[string]bool
	preparedPlan *incrementalPreparedPlan
	statusPlan   *templating.StatusPatchProjectionPlan
	statusAuth   *incrementalStatusPatchPlanAuthentication
	bindingCache *incrementalBindingCache
	auth         *incrementalStateSnapshotAuthentication
}

func newIncrementalStateSnapshot() *incrementalStateSnapshot {
	snapshot := &incrementalStateSnapshot{
		cursors:      map[string]incrementalStoreCursor{},
		bindings:     iradix.New[string](),
		members:      iradix.New[struct{}](),
		activeGroups: newIncrementalActiveGroupIndex(),
		retired:      iradix.New[struct{}](),
		results:      iradix.New[incremental.ExactValueRoot](),
		derived:      iradix.New[incrementalDerivedResource](),
		httpEffects:  iradix.New[*iradix.Tree[incrementalHTTPEffect]](),
		catalog:      newIncrementalResourceCatalogSnapshot(nil),
		groupIndexes: map[string]*incrementalGroupIndex{},
		groupReady:   map[string]bool{},
		statusPlan:   templating.NewStatusPatchProjectionPlan(),
	}
	authenticateIncrementalStatusPatchPlan(snapshot)
	authenticateIncrementalStateSnapshot(snapshot)
	return snapshot
}

type incrementalRenderState struct {
	mu              sync.Mutex
	httpMu          sync.Mutex
	httpLifecycleMu sync.Mutex
	cache           incrementalCacheBuilder
	// coldCommitAwaitsGraph makes a cold commit wait for its graph instead of
	// maxColdCacheBuildWait, so the next render, held by the coldRenderGate,
	// finds it published rather than superseding the build.
	coldCommitAwaitsGraph bool

	graph                    *incremental.Graph
	snapshot                 *incrementalStateSnapshot
	environment              *incrementalEnvironmentAuthority
	environmentErr           error
	components               map[string]incrementalComponent
	activations              map[string][]incrementalComponent
	groups                   map[string][]incrementalComponent
	dependencies             map[string][]string
	required                 map[string]struct{}
	deriveSources            map[string]struct{}
	staticBindingPlan        *incrementalBindingPlan
	dynamicComponents        []string
	bindingEntryPoints       []string
	bindingsUseCurrentConfig bool
	bindingsUseCurrentFiles  bool
	config                   *config.Config
	engine                   templating.IncrementalComponentExecutor
	planner                  templating.IncrementalBindingPlannerExecutor
	httpNextID               uint64
	httpIDs                  map[httpInputIdentity]uint64
	httpSpecs                map[uint64]httpInputSpec
	httpByURL                map[string]map[httpstore.SourceDescriptor]uint64
	httpRefs                 map[uint64]uint64
	httpFlight               map[uint64]uint64
	httpLeaseSet             *httpstore.ActiveLeaseSet
	httpInitial              httpstore.ActiveLeaseToken
	cacheReadyAuthority      *incrementalCacheReadyAuthority
	cacheBuildAuthority      *incrementalCacheBuildAuthority
	cacheBuildObserver       IncrementalCacheBuildObserver
	cachePending             bool
	cachePendingGeneration   uint64
	cacheReadySignal         *incrementalCacheReadySignal
	cachePublicationErr      error
	retiring                 bool
	retired                  bool
	configErr                error
	transitionNow            func(context.Context) (string, error)
	scopeFor                 func(string) *rendercontext.NamespaceScope // see RenderService.namespaceScopes
}

func sampleIncrementalTransitionTime(ctx context.Context) (string, error) {
	if cause := context.Cause(ctx); cause != nil {
		return "", cause
	}
	return time.Now().UTC().Format(time.RFC3339), nil
}

func (s *incrementalRenderState) sampleTransitionTime(ctx context.Context) (string, error) {
	source := s.transitionNow
	if source == nil {
		source = sampleIncrementalTransitionTime
	}
	return source(ctx)
}

func newIncrementalComponentFromSnippet(
	name string,
	snippet *config.TemplateSnippet,
) (incrementalComponent, error) {
	group := snippet.Incremental.Group
	if group == "" {
		group = name
	}
	component := incrementalComponent{
		name:               name,
		entryPoint:         helpers.IncrementalEntryPointName(name),
		source:             snippet.Incremental.Source,
		root:               snippet.Incremental.Root,
		group:              group,
		consumes:           slices.Clone(snippet.Incremental.Consumes),
		optionalConsumes:   slices.Clone(snippet.Incremental.OptionalConsumes),
		resourceProjection: snippet.Incremental.Mode == config.IncrementalModeResourceProjection,
	}
	for index, path := range snippet.Incremental.WhenAnyPathExists {
		compiled, err := templating.CompileExistenceJSONPath(path)
		if err != nil {
			return incrementalComponent{}, fmt.Errorf(
				"template snippet %q incremental.whenAnyPathExists[%d]: %w",
				name,
				index,
				err,
			)
		}
		component.activationPaths = append(component.activationPaths, compiled)
	}
	slices.Sort(component.consumes)
	slices.Sort(component.optionalConsumes)
	for _, effect := range snippet.Incremental.Effects {
		switch effect {
		case config.IncrementalEffectDeriveResource:
			component.deriveResource = true
		case config.IncrementalEffectRecordEvent:
			component.recordEvent = true
		case config.IncrementalEffectBackendPlan:
			component.backendPlan = true
		case config.IncrementalEffectPublishValue:
			component.publishValue = true
		case config.IncrementalEffectStatusPatch:
			component.statusPatch = true
		}
	}
	if len(component.activationPaths) > 0 && component.deriveResource {
		return incrementalComponent{}, fmt.Errorf(
			"template snippet %q incremental.whenAnyPathExists cannot be combined with deriveResource",
			name,
		)
	}
	return component, nil
}

func incrementalComponentActivations(
	cfg *config.Config,
	components map[string]incrementalComponent,
) map[string][]incrementalComponent {
	activations := map[string][]incrementalComponent{}
	for index := range components {
		component := components[index]
		if len(component.activationPaths) == 0 {
			continue
		}
		if component.source != "" {
			activations[component.source] = append(activations[component.source], component)
			continue
		}
		for source := range cfg.WatchedResources {
			activations[source] = append(activations[source], component)
		}
	}
	for source := range activations {
		slices.SortFunc(activations[source], func(left, right incrementalComponent) int {
			return strings.Compare(left.name, right.name)
		})
	}
	return activations
}

func incrementalGroupDependencies(
	dependencySets map[string]map[string]struct{},
) map[string][]string {
	dependencies := make(map[string][]string, len(dependencySets))
	for group, set := range dependencySets {
		for dependency := range set {
			dependencies[group] = append(dependencies[group], dependency)
		}
		slices.Sort(dependencies[group])
	}
	return dependencies
}

func newIncrementalStaticBindingPlan(components map[string]incrementalComponent) (
	plan *incrementalBindingPlan,
	dynamicComponents []string,
	bindingEntryPoints []string,
	err error,
) {
	plan = newIncrementalBindingPlan()
	dynamicComponents = make([]string, 0, len(components))
	bindingEntryPoints = make([]string, 0, len(components))
	for _, name := range sortedComponentNames(components) {
		component := components[name]
		if component.source == "" {
			dynamicComponents = append(dynamicComponents, name)
			bindingEntryPoints = append(bindingEntryPoints, helpers.IncrementalBindingsEntryPointName(name))
			continue
		}
		if bindErr := plan.addComponentBindings(
			&component,
			[]incrementalBinding{staticIncrementalBinding(component.name, component.source)},
		); bindErr != nil {
			return nil, nil, nil, bindErr
		}
	}
	plan.sort()
	return plan, dynamicComponents, bindingEntryPoints, nil
}

type incrementalComponentCatalog struct {
	components     map[string]incrementalComponent
	groups         map[string][]incrementalComponent
	dependencySets map[string]map[string]struct{}
	required       map[string]struct{}
	deriveSources  map[string]struct{}
}

func newIncrementalComponentCatalog(cfg *config.Config) (*incrementalComponentCatalog, error) {
	catalog := &incrementalComponentCatalog{
		components:     map[string]incrementalComponent{},
		groups:         map[string][]incrementalComponent{},
		dependencySets: map[string]map[string]struct{}{},
		required:       map[string]struct{}{},
		deriveSources:  map[string]struct{}{},
	}
	dynamicDeriveSource := false
	for name := range cfg.TemplateSnippets {
		snippet := cfg.TemplateSnippets[name]
		if snippet.Incremental == nil {
			continue
		}
		dynamic, err := catalog.addSnippet(name, &snippet)
		if err != nil {
			return nil, err
		}
		dynamicDeriveSource = dynamicDeriveSource || dynamic
	}
	if dynamicDeriveSource {
		for source := range cfg.WatchedResources {
			catalog.deriveSources[source] = struct{}{}
		}
	}
	return catalog, nil
}

func (c *incrementalComponentCatalog) addSnippet(
	name string,
	snippet *config.TemplateSnippet,
) (dynamicDeriveSource bool, err error) {
	component, err := newIncrementalComponentFromSnippet(name, snippet)
	if err != nil {
		return false, err
	}
	if component.deriveResource {
		if component.source == "" {
			dynamicDeriveSource = true
		} else {
			c.deriveSources[component.source] = struct{}{}
		}
	}
	c.components[name] = component
	c.groups[component.group] = append(c.groups[component.group], component)
	if c.dependencySets[component.group] == nil {
		c.dependencySets[component.group] = map[string]struct{}{}
	}
	for _, dependency := range append(slices.Clone(component.consumes), component.optionalConsumes...) {
		c.dependencySets[component.group][dependency] = struct{}{}
	}
	for _, resource := range snippet.Requires {
		c.required[resource] = struct{}{}
	}
	return dynamicDeriveSource, nil
}

// incrementalRenderAnalysis is read-only after construction and shared by independent render states.
type incrementalRenderAnalysis struct {
	config                   *config.Config
	engine                   templating.Engine
	err                      error
	components               map[string]incrementalComponent
	activations              map[string][]incrementalComponent
	groups                   map[string][]incrementalComponent
	dependencies             map[string][]string
	required                 map[string]struct{}
	deriveSources            map[string]struct{}
	staticBindingPlan        *incrementalBindingPlan
	dynamicComponents        []string
	bindingEntryPoints       []string
	bindingsUseCurrentConfig bool
	bindingsUseCurrentFiles  bool
}

// analyzeIncrementalRender returns nil when the config declares no
// incremental components.
func analyzeIncrementalRender(cfg *config.Config, engine templating.Engine) *incrementalRenderAnalysis {
	catalog, err := newIncrementalComponentCatalog(cfg)
	if err != nil {
		return &incrementalRenderAnalysis{config: cfg, engine: engine, err: err}
	}
	components := catalog.components
	groups := catalog.groups
	if len(components) == 0 {
		return nil
	}
	activations := incrementalComponentActivations(cfg, components)
	for group := range groups {
		slices.SortFunc(groups[group], func(left, right incrementalComponent) int {
			return strings.Compare(left.name, right.name)
		})
	}
	staticBindingPlan, dynamicComponents, bindingEntryPoints, err := newIncrementalStaticBindingPlan(components)
	if err != nil {
		return &incrementalRenderAnalysis{config: cfg, engine: engine, err: err}
	}
	analysis := &incrementalRenderAnalysis{
		config:             cfg,
		engine:             engine,
		components:         components,
		activations:        activations,
		groups:             groups,
		dependencies:       incrementalGroupDependencies(catalog.dependencySets),
		required:           catalog.required,
		deriveSources:      catalog.deriveSources,
		staticBindingPlan:  staticBindingPlan,
		dynamicComponents:  dynamicComponents,
		bindingEntryPoints: bindingEntryPoints,
	}
	analysis.bindingsUseCurrentConfig, analysis.bindingsUseCurrentFiles =
		bindingPreviousOutputUsage(engine, bindingEntryPoints)
	return analysis
}

func newIncrementalRenderState(cfg *config.Config, engine templating.Engine) *incrementalRenderState {
	return newIncrementalRenderStateFromAnalysis(analyzeIncrementalRender(cfg, engine))
}

func newIncrementalRenderStateFromAnalysis(analysis *incrementalRenderAnalysis) *incrementalRenderState {
	if analysis == nil {
		return nil
	}
	if analysis.err != nil {
		return newInvalidIncrementalRenderState(analysis.config, analysis.err)
	}
	executor, _ := analysis.engine.(templating.IncrementalComponentExecutor)
	planner, _ := analysis.engine.(templating.IncrementalBindingPlannerExecutor)
	state := &incrementalRenderState{
		snapshot:                 newIncrementalStateSnapshot(),
		components:               analysis.components,
		activations:              analysis.activations,
		groups:                   analysis.groups,
		dependencies:             analysis.dependencies,
		required:                 analysis.required,
		deriveSources:            analysis.deriveSources,
		staticBindingPlan:        analysis.staticBindingPlan,
		dynamicComponents:        analysis.dynamicComponents,
		bindingEntryPoints:       analysis.bindingEntryPoints,
		bindingsUseCurrentConfig: analysis.bindingsUseCurrentConfig,
		bindingsUseCurrentFiles:  analysis.bindingsUseCurrentFiles,
		config:                   analysis.config,
		engine:                   executor,
		planner:                  planner,
		httpIDs:                  map[httpInputIdentity]uint64{},
		httpSpecs:                map[uint64]httpInputSpec{},
		httpByURL:                map[string]map[httpstore.SourceDescriptor]uint64{},
		httpRefs:                 map[uint64]uint64{},
		httpFlight:               map[uint64]uint64{},
		transitionNow:            sampleIncrementalTransitionTime,
	}
	for group := range analysis.groups {
		state.snapshot.groupIndexes[group] = newIncrementalGroupIndex()
	}
	plan, err := newIncrementalPreparedPlan(
		state.backendPlanGroups(), state.snapshot.groupIndexes, state.snapshot.results.Root(),
	)
	if err != nil {
		panic(err)
	}
	state.snapshot.preparedPlan = plan
	authenticateIncrementalStateSnapshot(state.snapshot)
	graph, err := incremental.NewWithProviderOptions(
		state.definition,
		incremental.Options{RetireUnreferencedInputs: true},
	)
	if err != nil {
		panic(err)
	}
	state.graph = graph
	state.environment = newIncrementalEnvironmentAuthority(state, graph)
	state.cacheReadyAuthority = newIncrementalCacheReadyAuthority(state)
	state.cacheBuildAuthority = newIncrementalCacheBuildAuthority(state)
	return state
}

func newInvalidIncrementalRenderState(cfg *config.Config, err error) *incrementalRenderState {
	return &incrementalRenderState{config: cfg, configErr: err}
}

// bindingPreviousOutputUsage fails closed: an engine without entry-point
// introspection materializes both prior outputs for every dynamic binding.
func bindingPreviousOutputUsage(
	engine templating.Engine,
	entryPoints []string,
) (usesConfig, usesFiles bool) {
	if len(entryPoints) == 0 {
		return false, false
	}
	introspector, ok := engine.(interface{ EntryPointUsedGlobals(string) []string })
	if !ok {
		return true, true
	}
	for _, entryPoint := range entryPoints {
		for _, global := range introspector.EntryPointUsedGlobals(entryPoint) {
			usesConfig = usesConfig || global == "currentConfig"
			usesFiles = usesFiles || global == "currentFiles"
		}
	}
	return usesConfig, usesFiles
}

func (s *incrementalRenderState) definition(key incremental.QueryKey) (incremental.QueryFunc, bool) {
	run, ok := s.unattributedDefinition(key)
	if !ok {
		return nil, false
	}
	return func(ctx context.Context, reader incremental.Reader) ([]byte, error) {
		value, err := run(ctx, reader)
		if err != nil {
			return nil, attributeToWatchedObject(key, err)
		}
		return value, nil
	}, true
}

func (s *incrementalRenderState) unattributedDefinition(key incremental.QueryKey) (incremental.QueryFunc, bool) {
	if source, namespace, name, ok := parseActivationQueryKey(key); ok {
		if len(s.activations[source]) == 0 {
			return nil, false
		}
		return func(ctx context.Context, reader incremental.Reader) ([]byte, error) {
			runtime, available := ctx.Value(incrementalRunContextKey{}).(*incrementalRenderSession)
			if !available || runtime == nil {
				return nil, errors.New("incremental activation predicate has no render session")
			}
			return runtime.executeActivationPredicate(ctx, reader, source, namespace, name)
		}, true
	}
	if source, namespace, name, ok := parseDerivedProjectionQueryKey(key); ok {
		return func(ctx context.Context, reader incremental.Reader) ([]byte, error) {
			runtime, available := ctx.Value(incrementalRunContextKey{}).(*incrementalRenderSession)
			if !available || runtime == nil {
				return nil, errors.New("incremental derived projection has no render session")
			}
			return runtime.executeDerivedProjection(ctx, reader, source, namespace, name)
		}, true
	}
	componentName, source, namespace, name, ok := parseComponentQueryKey(key)
	if !ok {
		return nil, false
	}
	component, ok := s.components[componentName]
	if !ok {
		return nil, false
	}
	return func(ctx context.Context, reader incremental.Reader) ([]byte, error) {
		runtime, ok := ctx.Value(incrementalRunContextKey{}).(*incrementalRenderSession)
		if !ok || runtime == nil {
			return nil, errors.New("incremental component has no render session")
		}
		return runtime.executeComponent(ctx, reader, &component, source, namespace, name)
	}, true
}

type incrementalRunContextKey struct{}

func newOpaqueDecoder(value, kind string) (incremental.TupleDecoder, bool) {
	decoder := incremental.NewTupleDecoder(value)
	decodedKind, ok := decoder.Next()
	return decoder, ok && kind != "" && decodedKind == kind
}

func decodeOpaque(value, kind string, decoded []string) bool {
	decoder, ok := newOpaqueDecoder(value, kind)
	if !ok {
		return false
	}
	for index := range decoded {
		decoded[index], ok = decoder.Next()
		if !ok {
			return false
		}
	}
	return decoder.Done()
}

func componentQueryKey(component *incrementalComponent, source, namespace, name string) incremental.QueryKey {
	return incremental.NewQueryKey(incremental.EncodeTuple("component", component.name, source, namespace, name))
}

func componentQueryKeyMatches(
	key incremental.QueryKey,
	component *incrementalComponent,
	source, namespace, name string,
) bool {
	return opaqueMatches(key.Opaque(), "component", component.name, source, namespace, name)
}

func opaqueMatches(value, kind string, parts ...string) bool {
	decoder, ok := newOpaqueDecoder(value, kind)
	if !ok {
		return false
	}
	for _, part := range parts {
		actual, exists := decoder.Next()
		if !exists || actual != part {
			return false
		}
	}
	return decoder.Done()
}

func parseComponentQueryKey(key incremental.QueryKey) (component, source, namespace, name string, ok bool) {
	var parts [4]string
	if !decodeOpaque(key.Opaque(), "component", parts[:]) {
		return "", "", "", "", false
	}
	return parts[0], parts[1], parts[2], parts[3], true
}

func activationQueryKey(source, namespace, name string) incremental.QueryKey {
	return incremental.NewQueryKey(incremental.EncodeTuple("activation", source, namespace, name))
}

func parseActivationQueryKey(key incremental.QueryKey) (source, namespace, name string, ok bool) {
	var parts [3]string
	if !decodeOpaque(key.Opaque(), "activation", parts[:]) {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

func memberKey(source, namespace, name string) []byte {
	return []byte(incremental.EncodeTuple("member", source, namespace, name))
}

func memberPrefix(source string) []byte {
	return []byte(incremental.EncodeTuple("member", source))
}

func parseMemberKey(key []byte) (namespace, name string, ok bool) {
	var parts [3]string
	if !decodeOpaque(string(key), "member", parts[:]) {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func resultKey(component *incrementalComponent, source, namespace, name string) []byte {
	return []byte(incremental.EncodeTuple("result", component.group, component.name, source, namespace, name))
}

type incrementalResultIdentity struct {
	group     string
	component string
	source    string
	namespace string
	name      string
}

func parseResultKey(key []byte) (incrementalResultIdentity, bool) {
	var parts [5]string
	if !decodeOpaque(string(key), "result", parts[:]) {
		return incrementalResultIdentity{}, false
	}
	return incrementalResultIdentity{
		group:     parts[0],
		component: parts[1],
		source:    parts[2],
		namespace: parts[3],
		name:      parts[4],
	}, true
}

func bindingKey(component, source string) []byte {
	return []byte(incremental.EncodeTuple("binding", component, source))
}

func parseBindingKey(key []byte) (component, source string, ok bool) {
	var parts [2]string
	if !decodeOpaque(string(key), "binding", parts[:]) {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func bindingInputKey(component, source string) incremental.InputKey {
	return incremental.NewInputKey(incremental.EncodeTuple("binding-input", component, source))
}

func parseBindingInputKey(key incremental.InputKey) (component, source string, ok bool) {
	var parts [2]string
	if !decodeOpaque(key.Opaque(), "binding-input", parts[:]) {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func renderSubjectInputKey(source, namespace, name string) incremental.InputKey {
	return incremental.NewInputKey(incremental.EncodeTuple("render-subject", source, namespace, name))
}

func parseRenderSubjectInputKey(key incremental.InputKey) (source, namespace, name string, ok bool) {
	var parts [3]string
	if !decodeOpaque(key.Opaque(), "render-subject", parts[:]) {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

func deriveOwnerInputKey(source string) incremental.InputKey {
	return incremental.NewInputKey(incremental.EncodeTuple("derive-owner", source))
}

func parseDeriveOwnerInputKey(key incremental.InputKey) (string, bool) {
	var parts [1]string
	if !decodeOpaque(key.Opaque(), "derive-owner", parts[:]) {
		return "", false
	}
	return parts[0], true
}

func derivedProjectionQueryKey(source, namespace, name string) incremental.QueryKey {
	return incremental.NewQueryKey(incremental.EncodeTuple("derived", source, namespace, name))
}

func parseDerivedProjectionQueryKey(key incremental.QueryKey) (source, namespace, name string, ok bool) {
	var parts [3]string
	if !decodeOpaque(key.Opaque(), "derived", parts[:]) {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

func derivedKey(identity rendercontext.DerivedResourceIdentity) []byte {
	return []byte(incremental.EncodeTuple("derived-value", identity.Resource, identity.Namespace, identity.Name))
}

func exactBytesRevision(kind string, value []byte) incremental.Revision {
	var prefix [binary.MaxVarintLen64]byte
	prefixLength := binary.PutUvarint(prefix[:], uint64(len(kind)))
	var revision strings.Builder
	revision.Grow(prefixLength + len(kind) + len(value))
	revision.Write(prefix[:prefixLength])
	revision.WriteString(kind)
	revision.Write(value)
	return incremental.NewRevision(revision.String())
}

func resourceInputKey(spec *resourceInputSpec) incremental.InputKey {
	if cache := spec.keyCache; cache != nil && cache.matches(spec) {
		return cache.key
	}
	return buildResourceInputKey(spec)
}

func buildResourceInputKey(spec *resourceInputSpec) incremental.InputKey {
	parts := []string{spec.resourceType, string(spec.scope)}
	switch spec.scope {
	case resourceInputList:
	case resourceInputGet:
		parts = append(parts, spec.keys...)
	case resourceInputIdentity:
		parts = append(parts, spec.namespace, spec.name)
	}
	return incremental.NewInputKey(incremental.EncodeTuple("resource", parts...))
}

func sealResourceInputSpec(spec *resourceInputSpec) resourceInputSpec {
	sealed := *spec
	sealed.keys = slices.Clone(sealed.keys)
	cache := &resourceInputKeyCache{
		key: buildResourceInputKey(&sealed), resourceType: sealed.resourceType, scope: sealed.scope,
		keys: slices.Clone(sealed.keys), namespace: sealed.namespace, name: sealed.name,
	}
	cache.seal = cache
	cache.proof = &resourceInputKeyCacheProof{
		cache: cache, key: cache.key, resourceType: cache.resourceType, scope: cache.scope,
		keys: slices.Clone(cache.keys), namespace: cache.namespace, name: cache.name,
	}
	cache.proof.seal = cache.proof
	sealed.keyCache = cache
	return sealed
}

func (c *resourceInputKeyCache) matches(spec *resourceInputSpec) bool {
	if c == nil || spec == nil || c.seal != c || c.proof == nil || c.proof.seal != c.proof ||
		c.proof.cache != c || c.key.Opaque() == "" || c.key != c.proof.key ||
		c.resourceType != c.proof.resourceType || c.scope != c.proof.scope ||
		c.namespace != c.proof.namespace || c.name != c.proof.name ||
		!slices.Equal(c.keys, c.proof.keys) {
		return false
	}
	return c.resourceType == spec.resourceType && c.scope == spec.scope &&
		c.namespace == spec.namespace && c.name == spec.name && slices.Equal(c.keys, spec.keys)
}

func resourceInputPrefix(resourceType string) []byte {
	return []byte(incremental.EncodeTuple("resource", resourceType))
}

func (s *incrementalRenderState) acquireHTTPInput(
	spec httpInputIdentity,
) (httpInputSpec, incremental.InputKey, error) {
	s.httpMu.Lock()
	defer s.httpMu.Unlock()
	if id, exists := s.httpIDs[spec]; exists {
		known := s.httpSpecs[id]
		if s.httpFlight[id] == ^uint64(0) {
			return httpInputSpec{}, incremental.InputKey{}, errors.New("incremental HTTP input reference count exhausted")
		}
		s.httpFlight[id]++
		return known, httpInputKey(id), nil
	}
	if s.httpNextID == ^uint64(0) {
		return httpInputSpec{}, incremental.InputKey{}, errors.New("incremental HTTP input identity exhausted")
	}
	s.httpNextID++
	known := httpInputSpec{id: s.httpNextID, httpInputIdentity: spec}
	s.httpIDs[spec] = known.id
	s.httpSpecs[known.id] = known
	byDescriptor := s.httpByURL[known.url]
	if byDescriptor == nil {
		byDescriptor = map[httpstore.SourceDescriptor]uint64{}
		s.httpByURL[known.url] = byDescriptor
	}
	byDescriptor[known.descriptor] = known.id
	s.httpFlight[known.id] = 1
	return known, httpInputKey(known.id), nil
}

func (s *incrementalRenderState) httpInputSpec(key incremental.InputKey) (httpInputSpec, bool) {
	id, ok := parseHTTPInputKey(key)
	if !ok {
		return httpInputSpec{}, false
	}
	s.httpMu.Lock()
	defer s.httpMu.Unlock()
	spec, exists := s.httpSpecs[id]
	return spec, exists
}

func parseHTTPInputKey(key incremental.InputKey) (uint64, bool) {
	var parts [1]string
	if !decodeOpaque(key.Opaque(), "http", parts[:]) {
		return 0, false
	}
	id, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil || id == 0 || strconv.FormatUint(id, 10) != parts[0] {
		return 0, false
	}
	return id, true
}

func (s *incrementalRenderState) retainHTTPInputSpec(id uint64) error {
	s.httpMu.Lock()
	defer s.httpMu.Unlock()
	if _, exists := s.httpSpecs[id]; !exists {
		return incremental.ErrRevisionConflict
	}
	if s.httpFlight[id] == ^uint64(0) {
		return errors.New("incremental HTTP input reference count exhausted")
	}
	s.httpFlight[id]++
	return nil
}

func (s *incrementalRenderState) httpInputsForChange(change *httpstore.SemanticChange) []httpInputSpec {
	if change == nil {
		return nil
	}
	s.httpMu.Lock()
	defer s.httpMu.Unlock()
	byDescriptor := s.httpByURL[change.URL]
	if len(byDescriptor) == 0 {
		return nil
	}
	ids := map[uint64]struct{}{}
	for _, descriptor := range []httpstore.SourceDescriptor{change.PreviousDescriptor, change.Descriptor} {
		if id := byDescriptor[descriptor]; id != 0 {
			ids[id] = struct{}{}
		}
	}
	result := make([]httpInputSpec, 0, len(ids))
	for id := range ids {
		result = append(result, s.httpSpecs[id])
	}
	slices.SortFunc(result, func(left, right httpInputSpec) int {
		return cmp.Compare(left.id, right.id)
	})
	return result
}

func (s *incrementalRenderState) httpInputForActiveChange(
	change *httpstore.ActiveLeaseChange,
) (httpInputSpec, bool, error) {
	if change == nil {
		return httpInputSpec{}, false, incremental.ErrRevisionConflict
	}
	s.httpMu.Lock()
	defer s.httpMu.Unlock()
	id := s.httpByURL[change.URL][change.Descriptor]
	if id == 0 || s.httpRefs[id] == 0 {
		return httpInputSpec{}, false, nil
	}
	spec, err := s.validateHTTPInputLocked(id)
	if err != nil {
		return httpInputSpec{}, false, err
	}
	return spec, true, nil
}

type preparedHTTPInputCommit struct {
	state         *incrementalRenderState
	replaceRefs   bool
	refs          map[uint64]uint64
	refUpdates    map[uint64]uint64
	flightUpdates map[uint64]uint64
	prune         []httpInputSpec
	baseRefs      map[uint64]uint64
	baseFlight    map[uint64]uint64
	baseSpecs     map[uint64]httpInputSpec
	baseIDs       map[httpInputIdentity]uint64
	baseByURL     map[string]map[httpstore.SourceDescriptor]uint64
	nextRefs      map[uint64]uint64
	nextFlight    map[uint64]uint64
	nextSpecs     map[uint64]httpInputSpec
	nextIDs       map[httpInputIdentity]uint64
	nextByURL     map[string]map[httpstore.SourceDescriptor]uint64
	baseRoot      *authenticatedHTTPInputRegistry
	nextRoot      *authenticatedHTTPInputRegistry
	seal          *preparedHTTPInputCommit
	published     bool
	committed     bool
	released      bool
}

func (c *preparedHTTPInputCommit) validatePublication() error {
	if c == nil || c.seal != c || c.released || c.published || c.state == nil ||
		c.baseRoot == nil || c.nextRoot == nil {
		return errors.New("prepared incremental HTTP ownership publication is invalid")
	}
	if err := c.baseRoot.validate(
		c,
		c.state.httpRefs,
		c.state.httpFlight,
		c.state.httpSpecs,
		c.state.httpIDs,
		c.state.httpByURL,
	); err != nil {
		return fmt.Errorf("prepared incremental HTTP ownership base: %w", err)
	}
	if err := c.nextRoot.validate(c, c.nextRefs, c.nextFlight, c.nextSpecs, c.nextIDs, c.nextByURL); err != nil {
		return fmt.Errorf("prepared incremental HTTP ownership future: %w", err)
	}
	return nil
}

func (c *preparedHTTPInputCommit) validatePublishedPublication() error {
	if c == nil || c.seal != c || c.released || !c.published || c.state == nil ||
		c.baseRoot == nil || c.nextRoot == nil {
		return errors.New("published incremental HTTP ownership publication is invalid")
	}
	if err := c.nextRoot.validate(
		c,
		c.state.httpRefs,
		c.state.httpFlight,
		c.state.httpSpecs,
		c.state.httpIDs,
		c.state.httpByURL,
	); err != nil {
		return fmt.Errorf("published incremental HTTP ownership future: %w", err)
	}
	if err := c.baseRoot.validate(
		c,
		c.baseRefs,
		c.baseFlight,
		c.baseSpecs,
		c.baseIDs,
		c.baseByURL,
	); err != nil {
		return fmt.Errorf("published incremental HTTP ownership rollback: %w", err)
	}
	return nil
}

func (c *preparedHTTPInputCommit) commitPublishedPublication() error {
	if c == nil {
		return errors.New("published incremental HTTP ownership publication is invalid")
	}
	if c.committed {
		return nil
	}
	if err := c.validatePublishedPublication(); err != nil {
		return err
	}
	c.committed = true
	return nil
}

func (c *preparedHTTPInputCommit) validPlanState() bool {
	return c != nil && c.state != nil && c.state.httpRefs != nil && c.state.httpFlight != nil &&
		c.state.httpSpecs != nil && c.state.httpIDs != nil && c.state.httpByURL != nil &&
		c.refUpdates != nil && c.flightUpdates != nil
}

func (c *preparedHTTPInputCommit) validatePlanLocked() error {
	if !c.validPlanState() {
		return errors.New("prepared incremental HTTP ownership publication is invalid")
	}
	if c.replaceRefs && c.refs == nil {
		return errors.New("prepared incremental HTTP ownership replacement is missing")
	}
	for id, count := range c.refs {
		if count == 0 {
			return fmt.Errorf("prepared incremental HTTP input %d has no references", id)
		}
		if _, err := c.state.validateHTTPInputLocked(id); err != nil {
			return err
		}
	}
	for id := range c.refUpdates {
		if _, err := c.state.validateHTTPInputLocked(id); err != nil {
			return err
		}
	}
	for id, count := range c.flightUpdates {
		if c.state.httpFlight[id] == 0 || count+1 != c.state.httpFlight[id] {
			return fmt.Errorf("prepared incremental HTTP input %d flight count is invalid", id)
		}
		if _, err := c.state.validateHTTPInputLocked(id); err != nil {
			return err
		}
	}
	return c.validatePrunedInputsLocked()
}

func (c *preparedHTTPInputCommit) validatePrunedInputsLocked() error {
	seen := make(map[uint64]struct{}, len(c.prune))
	for index := range c.prune {
		spec := c.prune[index]
		if _, exists := seen[spec.id]; exists {
			return fmt.Errorf("prepared incremental HTTP input %d is pruned twice", spec.id)
		}
		seen[spec.id] = struct{}{}
		current, err := c.state.validateHTTPInputLocked(spec.id)
		if err != nil {
			return err
		}
		if current != spec {
			return fmt.Errorf("prepared incremental HTTP input %d prune identity changed", spec.id)
		}
	}
	return nil
}

type authenticatedHTTPInputCount struct {
	id    uint64
	count uint64
}

type authenticatedHTTPURLCount struct {
	url   string
	count int
}

type authenticatedHTTPInputRegistry struct {
	seal      *authenticatedHTTPInputRegistry
	owner     *preparedHTTPInputCommit
	specs     []httpInputSpec
	refs      []authenticatedHTTPInputCount
	flight    []authenticatedHTTPInputCount
	urlCounts []authenticatedHTTPURLCount
}

func newAuthenticatedHTTPInputRegistry(
	owner *preparedHTTPInputCommit,
	refs,
	flight map[uint64]uint64,
	specs map[uint64]httpInputSpec,
	ids map[httpInputIdentity]uint64,
	byURL map[string]map[httpstore.SourceDescriptor]uint64,
) (*authenticatedHTTPInputRegistry, error) {
	if err := validateHTTPInputRegistry(refs, flight, specs, ids, byURL); err != nil {
		return nil, err
	}
	root := &authenticatedHTTPInputRegistry{owner: owner}
	root.specs = make([]httpInputSpec, 0, len(specs))
	for _, spec := range specs {
		root.specs = append(root.specs, spec)
	}
	slices.SortFunc(root.specs, func(left, right httpInputSpec) int {
		return cmp.Compare(left.id, right.id)
	})
	root.refs = authenticatedHTTPInputCounts(refs)
	root.flight = authenticatedHTTPInputCounts(flight)
	root.urlCounts = make([]authenticatedHTTPURLCount, 0, len(byURL))
	for url, descriptors := range byURL {
		root.urlCounts = append(root.urlCounts, authenticatedHTTPURLCount{url: url, count: len(descriptors)})
	}
	slices.SortFunc(root.urlCounts, func(left, right authenticatedHTTPURLCount) int {
		return strings.Compare(left.url, right.url)
	})
	root.seal = root
	return root, nil
}

func authenticatedHTTPInputCounts(values map[uint64]uint64) []authenticatedHTTPInputCount {
	result := make([]authenticatedHTTPInputCount, 0, len(values))
	for id, count := range values {
		result = append(result, authenticatedHTTPInputCount{id: id, count: count})
	}
	slices.SortFunc(result, func(left, right authenticatedHTTPInputCount) int {
		return cmp.Compare(left.id, right.id)
	})
	return result
}

func (r *authenticatedHTTPInputRegistry) matchesRegistryShape(
	owner *preparedHTTPInputCommit,
	refs,
	flight map[uint64]uint64,
	specs map[uint64]httpInputSpec,
	ids map[httpInputIdentity]uint64,
	byURL map[string]map[httpstore.SourceDescriptor]uint64,
) bool {
	return r != nil && r.seal == r && r.owner == owner && refs != nil && flight != nil &&
		specs != nil && ids != nil && byURL != nil && len(refs) == len(r.refs) &&
		len(flight) == len(r.flight) && len(specs) == len(r.specs) && len(ids) == len(r.specs) &&
		len(byURL) == len(r.urlCounts)
}

func (r *authenticatedHTTPInputRegistry) validate(
	owner *preparedHTTPInputCommit,
	refs,
	flight map[uint64]uint64,
	specs map[uint64]httpInputSpec,
	ids map[httpInputIdentity]uint64,
	byURL map[string]map[httpstore.SourceDescriptor]uint64,
) error {
	if !r.matchesRegistryShape(owner, refs, flight, specs, ids, byURL) {
		return errors.New("incremental HTTP input registry has invalid provenance")
	}
	for index := range r.refs {
		expected := r.refs[index]
		if refs[expected.id] != expected.count {
			return fmt.Errorf("incremental HTTP input %d reference root changed", expected.id)
		}
	}
	for index := range r.flight {
		expected := r.flight[index]
		if flight[expected.id] != expected.count {
			return fmt.Errorf("incremental HTTP input %d flight root changed", expected.id)
		}
	}
	for index := range r.specs {
		expected := r.specs[index]
		if specs[expected.id] != expected || ids[expected.httpInputIdentity] != expected.id ||
			byURL[expected.url] == nil || byURL[expected.url][expected.descriptor] != expected.id {
			return fmt.Errorf("incremental HTTP input %d identity root changed", expected.id)
		}
	}
	for index := range r.urlCounts {
		expected := r.urlCounts[index]
		if len(byURL[expected.url]) != expected.count {
			return fmt.Errorf("incremental HTTP URL %q root changed", expected.url)
		}
	}
	return nil
}

func (s *incrementalRenderState) finishHTTPInputs(
	retained map[uint64]struct{},
	deltas map[uint64]httpRefDelta,
	rebuild *iradix.Tree[*iradix.Tree[incrementalHTTPEffect]],
	commit bool,
) error {
	prepared, err := s.prepareHTTPInputCommit(retained, deltas, rebuild, commit)
	if err != nil {
		return err
	}
	prepared.Publish()
	if err := prepared.commitPublishedPublication(); err != nil {
		prepared.Abort()
		return err
	}
	prepared.Release()
	return nil
}

func (s *incrementalRenderState) prepareHTTPInputCommit(
	retained map[uint64]struct{},
	deltas map[uint64]httpRefDelta,
	rebuild *iradix.Tree[*iradix.Tree[incrementalHTTPEffect]],
	commit bool,
) (*preparedHTTPInputCommit, error) {
	s.httpMu.Lock()
	prepared := &preparedHTTPInputCommit{
		state:         s,
		refUpdates:    map[uint64]uint64{},
		flightUpdates: map[uint64]uint64{},
	}

	touched := make(map[uint64]struct{}, len(retained)+len(deltas))
	if err := s.prepareHTTPRefUpdatesLocked(prepared, touched, deltas, rebuild, commit); err != nil {
		s.httpMu.Unlock()
		return nil, err
	}
	if err := s.prepareHTTPFlightUpdatesLocked(prepared, touched, retained); err != nil {
		s.httpMu.Unlock()
		return nil, err
	}
	if err := s.prepareHTTPPrunesLocked(prepared, touched); err != nil {
		s.httpMu.Unlock()
		return nil, err
	}
	if err := prepared.validatePlanLocked(); err != nil {
		s.httpMu.Unlock()
		return nil, err
	}
	if err := s.prepareHTTPReplacementLocked(prepared); err != nil {
		s.httpMu.Unlock()
		return nil, err
	}
	prepared.seal = prepared
	baseRoot, err := newAuthenticatedHTTPInputRegistry(
		prepared,
		prepared.baseRefs,
		prepared.baseFlight,
		prepared.baseSpecs,
		prepared.baseIDs,
		prepared.baseByURL,
	)
	if err != nil {
		s.httpMu.Unlock()
		return nil, err
	}
	prepared.baseRoot = baseRoot
	nextRoot, err := newAuthenticatedHTTPInputRegistry(
		prepared,
		prepared.nextRefs,
		prepared.nextFlight,
		prepared.nextSpecs,
		prepared.nextIDs,
		prepared.nextByURL,
	)
	if err != nil {
		s.httpMu.Unlock()
		return nil, err
	}
	prepared.nextRoot = nextRoot
	if err := prepared.validatePublication(); err != nil {
		s.httpMu.Unlock()
		return nil, err
	}
	return prepared, nil
}

func (s *incrementalRenderState) prepareHTTPReplacementLocked(c *preparedHTTPInputCommit) error {
	c.baseRefs = s.httpRefs
	c.baseFlight = s.httpFlight
	c.baseSpecs = s.httpSpecs
	c.baseIDs = s.httpIDs
	c.baseByURL = s.httpByURL
	c.nextRefs = maps.Clone(s.httpRefs)
	if c.replaceRefs {
		c.nextRefs = maps.Clone(c.refs)
	} else {
		applyHTTPCountUpdates(c.nextRefs, c.refUpdates)
	}
	c.nextFlight = maps.Clone(s.httpFlight)
	applyHTTPCountUpdates(c.nextFlight, c.flightUpdates)
	c.nextSpecs = maps.Clone(s.httpSpecs)
	c.nextIDs = maps.Clone(s.httpIDs)
	c.nextByURL = maps.Clone(s.httpByURL)
	clonedURLs := make(map[string]struct{}, len(c.prune))
	for _, spec := range c.prune {
		delete(c.nextRefs, spec.id)
		delete(c.nextFlight, spec.id)
		delete(c.nextSpecs, spec.id)
		delete(c.nextIDs, spec.httpInputIdentity)
		if _, cloned := clonedURLs[spec.url]; !cloned {
			c.nextByURL[spec.url] = maps.Clone(c.nextByURL[spec.url])
			clonedURLs[spec.url] = struct{}{}
		}
		byDescriptor := c.nextByURL[spec.url]
		delete(byDescriptor, spec.descriptor)
		if len(byDescriptor) == 0 {
			delete(c.nextByURL, spec.url)
		}
	}
	return validateHTTPInputRegistry(c.nextRefs, c.nextFlight, c.nextSpecs, c.nextIDs, c.nextByURL)
}

func applyHTTPCountUpdates(target, updates map[uint64]uint64) {
	for id, count := range updates {
		if count == 0 {
			delete(target, id)
		} else {
			target[id] = count
		}
	}
}

func validateHTTPInputRegistry(
	refs,
	flight map[uint64]uint64,
	specs map[uint64]httpInputSpec,
	ids map[httpInputIdentity]uint64,
	byURL map[string]map[httpstore.SourceDescriptor]uint64,
) error {
	if refs == nil || flight == nil || specs == nil || ids == nil || byURL == nil || len(specs) != len(ids) {
		return errors.New("incremental HTTP input registry is invalid")
	}
	for id, spec := range specs {
		if id == 0 || spec.id != id || ids[spec.httpInputIdentity] != id ||
			byURL[spec.url] == nil || byURL[spec.url][spec.descriptor] != id {
			return fmt.Errorf("incremental HTTP input %d registry is inconsistent", id)
		}
	}
	for identity, id := range ids {
		if spec, exists := specs[id]; !exists || spec.httpInputIdentity != identity {
			return fmt.Errorf("incremental HTTP input %d identity index is inconsistent", id)
		}
	}
	if err := validateHTTPURLIndex(specs, byURL); err != nil {
		return err
	}
	if err := validateHTTPReferenceCounts(refs, specs); err != nil {
		return err
	}
	return validateHTTPFlightCounts(flight, specs)
}

func validateHTTPURLIndex(
	specs map[uint64]httpInputSpec,
	byURL map[string]map[httpstore.SourceDescriptor]uint64,
) error {
	indexed := 0
	for url, descriptors := range byURL {
		if descriptors == nil {
			return fmt.Errorf("incremental HTTP URL %q index is invalid", url)
		}
		for descriptor, id := range descriptors {
			spec, exists := specs[id]
			if !exists || spec.url != url || spec.descriptor != descriptor {
				return fmt.Errorf("incremental HTTP input %d URL index is inconsistent", id)
			}
			indexed++
		}
	}
	if indexed != len(specs) {
		return errors.New("incremental HTTP URL index is incomplete")
	}
	return nil
}

func validateHTTPReferenceCounts(
	refs map[uint64]uint64,
	specs map[uint64]httpInputSpec,
) error {
	for id, count := range refs {
		if count == 0 {
			return fmt.Errorf("incremental HTTP input %d has no references", id)
		}
		if _, exists := specs[id]; !exists {
			return fmt.Errorf("incremental HTTP reference %d has no registered input", id)
		}
	}
	return nil
}

func validateHTTPFlightCounts(
	flight map[uint64]uint64,
	specs map[uint64]httpInputSpec,
) error {
	for id, count := range flight {
		if count == 0 {
			return fmt.Errorf("incremental HTTP input %d has an empty flight", id)
		}
		if _, exists := specs[id]; !exists {
			return fmt.Errorf("incremental HTTP flight %d has no registered input", id)
		}
	}
	return nil
}

func (s *incrementalRenderState) prepareHTTPRefUpdatesLocked(
	prepared *preparedHTTPInputCommit,
	touched map[uint64]struct{},
	deltas map[uint64]httpRefDelta,
	rebuild *iradix.Tree[*iradix.Tree[incrementalHTTPEffect]],
	commit bool,
) error {
	if commit && rebuild != nil {
		for id := range s.httpRefs {
			touched[id] = struct{}{}
		}
		refs, err := countHTTPRefs(rebuild)
		if err != nil {
			return err
		}
		prepared.replaceRefs = true
		prepared.refs = refs
		for id := range refs {
			touched[id] = struct{}{}
		}
	} else if commit {
		for id, delta := range deltas {
			current := s.httpRefs[id]
			if current < delta.removed || ^uint64(0)-(current-delta.removed) < delta.added {
				return fmt.Errorf("incremental HTTP input %d reference count is inconsistent", id)
			}
			next := current - delta.removed + delta.added
			prepared.refUpdates[id] = next
			touched[id] = struct{}{}
		}
	}
	return nil
}

func (s *incrementalRenderState) prepareHTTPFlightUpdatesLocked(
	prepared *preparedHTTPInputCommit,
	touched map[uint64]struct{},
	retained map[uint64]struct{},
) error {
	for id := range retained {
		if s.httpFlight[id] == 0 {
			return fmt.Errorf("incremental HTTP input %d has no in-flight reference", id)
		}
		prepared.flightUpdates[id] = s.httpFlight[id] - 1
		touched[id] = struct{}{}
	}
	return nil
}

func (s *incrementalRenderState) prepareHTTPPrunesLocked(
	prepared *preparedHTTPInputCommit,
	touched map[uint64]struct{},
) error {
	ids := make([]uint64, 0, len(touched))
	for id := range touched {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		spec, err := s.validateHTTPInputLocked(id)
		if err != nil {
			return err
		}
		refCount := s.httpRefs[id]
		if prepared.replaceRefs {
			refCount = prepared.refs[id]
		} else if next, changed := prepared.refUpdates[id]; changed {
			refCount = next
		}
		flightCount := s.httpFlight[id]
		if next, changed := prepared.flightUpdates[id]; changed {
			flightCount = next
		}
		if refCount == 0 && flightCount == 0 {
			prepared.prune = append(prepared.prune, spec)
		}
	}
	return nil
}

func (c *preparedHTTPInputCommit) Publish() {
	if c == nil || c.released {
		return
	}
	if err := c.validatePublication(); err != nil {
		panic(err)
	}
	c.state.httpRefs = c.nextRefs
	c.state.httpFlight = c.nextFlight
	c.state.httpSpecs = c.nextSpecs
	c.state.httpIDs = c.nextIDs
	c.state.httpByURL = c.nextByURL
	c.published = true
}

func (c *preparedHTTPInputCommit) Release() {
	if c == nil || c.released {
		return
	}
	if !c.published || !c.committed {
		panic("incremental HTTP ownership publication was released before commit")
	}
	c.released = true
	c.state.httpMu.Unlock()
}

func (c *preparedHTTPInputCommit) Abort() {
	if c == nil || c.released {
		return
	}
	if c.published {
		c.state.httpRefs = c.baseRefs
		c.state.httpFlight = c.baseFlight
		c.state.httpSpecs = c.baseSpecs
		c.state.httpIDs = c.baseIDs
		c.state.httpByURL = c.baseByURL
	}
	c.released = true
	c.state.httpMu.Unlock()
}

func (s *incrementalRenderState) validateHTTPInputLocked(id uint64) (httpInputSpec, error) {
	spec, exists := s.httpSpecs[id]
	if !exists {
		return httpInputSpec{}, fmt.Errorf("incremental HTTP input %d has no registered identity", id)
	}
	if registered, exists := s.httpIDs[spec.httpInputIdentity]; !exists || registered != id {
		return httpInputSpec{}, fmt.Errorf("incremental HTTP input %d identity index is inconsistent", id)
	}
	byDescriptor := s.httpByURL[spec.url]
	if registered, exists := byDescriptor[spec.descriptor]; !exists || registered != id {
		return httpInputSpec{}, fmt.Errorf("incremental HTTP input %d URL index is inconsistent", id)
	}
	return spec, nil
}

func countHTTPRefs(tree *iradix.Tree[*iradix.Tree[incrementalHTTPEffect]]) (map[uint64]uint64, error) {
	if tree == nil {
		return nil, errors.New("incremental HTTP effect root is unavailable")
	}
	refs := map[uint64]uint64{}
	var countErr error
	tree.Root().Walk(func(_ []byte, effects *iradix.Tree[incrementalHTTPEffect]) bool {
		if err := validateIndexedHTTPEffects(effects); err != nil {
			countErr = err
			return true
		}
		effects.Root().Walk(func(_ []byte, effect incrementalHTTPEffect) bool {
			if refs[effect.inputID] == ^uint64(0) {
				countErr = fmt.Errorf("incremental HTTP input %d reference count is exhausted", effect.inputID)
				return true
			}
			refs[effect.inputID]++
			return false
		})
		return countErr != nil
	})
	return refs, countErr
}

func httpInputKey(id uint64) incremental.InputKey {
	return incremental.NewInputKey(incremental.EncodeTuple("http", strconv.FormatUint(id, 10)))
}

func httpInputRevision(source httpstore.SourceID, snapshot *httpstore.ContentSnapshot) incremental.Revision {
	if snapshot == nil {
		return incremental.Revision{}
	}
	return incremental.NewRevision(incremental.EncodeTuple(
		"http-observation",
		strconv.FormatUint(uint64(source), 10),
		strconv.FormatUint(uint64(snapshot.Observation), 10),
		strconv.FormatBool(snapshot.Found),
	))
}

func scratchHTTPRevision(snapshot *httpstore.ContentSnapshot) incremental.Revision {
	if snapshot == nil {
		return incremental.Revision{}
	}
	return incremental.NewRevision(incremental.EncodeTuple(
		"http-scratch",
		strconv.FormatBool(snapshot.Found),
		snapshot.Content,
	))
}

func parseResourceInputKey(key incremental.InputKey) (resourceInputSpec, bool) {
	decoder, ok := newOpaqueDecoder(key.Opaque(), "resource")
	if !ok {
		return resourceInputSpec{}, false
	}
	resourceType, resourceTypeOK := decoder.Next()
	scope, scopeOK := decoder.Next()
	if !resourceTypeOK || !scopeOK {
		return resourceInputSpec{}, false
	}
	spec := resourceInputSpec{resourceType: resourceType, scope: resourceInputScope(scope)}
	switch spec.scope {
	case resourceInputList:
		return spec, decoder.Done()
	case resourceInputGet:
		for !decoder.Done() {
			value, exists := decoder.Next()
			if !exists {
				return resourceInputSpec{}, false
			}
			spec.keys = append(spec.keys, value)
		}
		return spec, len(spec.keys) > 0
	case resourceInputIdentity:
		namespace, namespaceOK := decoder.Next()
		name, nameOK := decoder.Next()
		if !namespaceOK || !nameOK || name == "" || !decoder.Done() {
			return resourceInputSpec{}, false
		}
		spec.namespace, spec.name = namespace, name
		return spec, true
	default:
		return resourceInputSpec{}, false
	}
}

func storeRevision(source stores.RevisionSource, revision stores.Revision) incremental.Revision {
	if source == 0 || revision == "" {
		return incremental.Revision{}
	}
	return incremental.NewRevision(incremental.EncodeTuple("store-revision", fmt.Sprint(uint64(source)), string(revision)))
}

func cloneGroupIndexes(source map[string]*incrementalGroupIndex) map[string]*incrementalGroupIndex {
	return maps.Clone(source)
}

func cloneHTTPEffects(source []incrementalHTTPEffect) []incrementalHTTPEffect {
	return slices.Clone(source)
}

func resourceIdentity(value any) (namespace, name string, found bool) {
	item, ok := value.(map[string]any)
	if !ok {
		return "", "", false
	}
	metadata, ok := item["metadata"].(map[string]any)
	if !ok {
		return "", "", false
	}
	namespace, _ = metadata["namespace"].(string)
	name, _ = metadata["name"].(string)
	return namespace, name, name != ""
}

func encodeResourceValue(value any) ([]byte, error) {
	encoded, err := typegen.MarshalImmutableJSON(value)
	if err != nil {
		return nil, fmt.Errorf("encoding incremental resource input: %w", err)
	}
	return encoded, nil
}

func decodeResourceValue(encoded []byte) (any, error) {
	if !utf8.Valid(encoded) {
		return nil, errors.New("incremental resource input is not valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("incremental resource input contains multiple JSON values")
		}
		return nil, err
	}
	return normalizeDecodedResourceNumbers(value, 0)
}

func normalizeDecodedResourceNumbers(value any, depth int) (any, error) {
	if depth > resourceValueMaxDepth {
		return nil, errors.New("resource value exceeds the maximum depth")
	}
	switch typed := value.(type) {
	case nil, bool, string:
		return typed, nil
	case json.Number:
		return normalizeDecodedResourceNumber(typed)
	case map[string]any:
		for key, item := range typed {
			normalized, err := normalizeDecodedResourceNumbers(item, depth+1)
			if err != nil {
				return nil, fmt.Errorf("resource map key %q: %w", key, err)
			}
			typed[key] = normalized
		}
		return typed, nil
	case []any:
		for index, item := range typed {
			normalized, err := normalizeDecodedResourceNumbers(item, depth+1)
			if err != nil {
				return nil, fmt.Errorf("resource list index %d: %w", index, err)
			}
			typed[index] = normalized
		}
		return typed, nil
	default:
		return nil, fmt.Errorf("resource value type %T is unavailable", value)
	}
}

const resourceValueMaxDepth = 256

type resourceCodecVisit struct {
	kind    reflect.Kind
	pointer uintptr
}

func normalizeDecodedResourceNumber(value json.Number) (any, error) {
	text := string(value)
	if !strings.ContainsAny(text, ".eE") {
		if integer, err := strconv.ParseInt(text, 10, 64); err == nil {
			return integer, nil
		}
		if integer, err := strconv.ParseUint(text, 10, 64); err == nil {
			return integer, nil
		}
	}
	decimal, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid resource number %q: %w", text, err)
	}
	if math.IsNaN(decimal) || math.IsInf(decimal, 0) {
		return nil, fmt.Errorf("resource number %q is non-finite", text)
	}
	return decimal, nil
}

func beginResourceCodecVisit(
	value any,
	active map[resourceCodecVisit]struct{},
) (resourceCodecVisit, error) {
	reflected := reflect.ValueOf(value)
	current := resourceCodecVisit{kind: reflected.Kind(), pointer: reflected.Pointer()}
	if _, exists := active[current]; exists {
		return resourceCodecVisit{}, errors.New("resource value contains a reference cycle")
	}
	active[current] = struct{}{}
	return current, nil
}

func watchedResourceNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.WatchedResources))
	for name := range cfg.WatchedResources {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (s *incrementalRenderState) resourcesValue(
	ctx context.Context,
	storesByName map[string]stores.Store,
	resourceErrors *rendercontext.ResourceErrorCollector,
	view rendercontext.StoreSnapshotView,
	derived *rendercontext.DerivedResourceView,
	loggerContext incrementalLoggerContext,
	memoizeSnapshotView bool,
) any {
	return rendercontext.BuildResourcesValueWithViews(
		ctx,
		storesByName,
		loggerContext.typedResourceTypes,
		watchedResourceNames(s.config),
		func(name string) []string { return slices.Clone(s.config.WatchedResources[name].IndexBy) },
		func(name string) bool { return s.config.WatchedResources[name].Store == "on-demand" },
		func(name string) string { return s.config.WatchedResources[name].APIVersion },
		s.scopeFor,
		loggerContext.logger,
		resourceErrors,
		view,
		derived,
		memoizeSnapshotView,
	)
}

func (s *incrementalRenderState) incrementalResourcesValue(
	ctx context.Context,
	storesByName map[string]stores.Store,
	resourceErrors *rendercontext.ResourceErrorCollector,
	view rendercontext.StoreSnapshotView,
	derived *rendercontext.DerivedResourceView,
	loggerContext incrementalLoggerContext,
) any {
	return rendercontext.BuildIncrementalResourcesValueWithViews(
		ctx,
		storesByName,
		loggerContext.typedResourceTypes,
		watchedResourceNames(s.config),
		func(name string) []string { return slices.Clone(s.config.WatchedResources[name].IndexBy) },
		func(name string) bool { return s.config.WatchedResources[name].Store == "on-demand" },
		func(name string) string { return s.config.WatchedResources[name].APIVersion },
		s.scopeFor,
		loggerContext.logger,
		resourceErrors,
		view,
		derived,
		false,
	)
}
