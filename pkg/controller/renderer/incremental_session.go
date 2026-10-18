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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	iradix "github.com/hashicorp/go-immutable-radix/v2"

	controllerhttpstore "github.com/haproxy-haptic/HAPTIC/pkg/controller/httpstore"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/renderer/internal/queryidentity"
	"github.com/haproxy-haptic/HAPTIC/pkg/httpstore"
	"github.com/haproxy-haptic/HAPTIC/pkg/incremental"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

var (
	errIncrementalUnsupported = errors.New("incremental rendering requires exact source snapshots")
	errIncrementalColdRestart = errors.New("incremental rendering requires a cold restart")
	errIncrementalBaseMoved   = errors.New("incremental render base moved before its graph session began")
)

const (
	incrementalSourceContextName        = "source"
	incrementalItemContextName          = "item"
	incrementalPropsContextName         = "props"
	incrementalRenderSubjectContextName = "renderSubject"
	incrementalRenderModeContextName    = "renderMode"
	incrementalSharedContextName        = "shared"
	incrementalResourcesContextName     = "resources"
	incrementalControllerContextName    = "controller"
	incrementalHTTPContextName          = "http"
	incrementalPlanRegistryContextName  = "planRegistry"
)

type incrementalLoggerContext struct {
	logger             *slog.Logger
	typedResourceTypes map[string]reflect.Type
}

type incrementalCall struct {
	scope     string
	component string
}

type incrementalScopeCallStatus struct {
	count     int
	canonical bool
}

func (s incrementalScopeCallStatus) complete(componentCount int) bool {
	return s.canonical && s.count > 0 && componentCount > 0 && s.count%componentCount == 0
}

type incrementalCallScope struct {
	group string
	scope string
}

type incrementalScopeCalls struct {
	calls  []incrementalCall
	status incrementalScopeCallStatus
}

func recordIncrementalCall(
	calls map[string][]incrementalCall,
	scopedCalls map[incrementalCallScope]incrementalScopeCalls,
	group string,
	expected []incrementalComponent,
	call incrementalCall,
) (updatedCalls map[string][]incrementalCall, updatedScopedCalls map[incrementalCallScope]incrementalScopeCalls) {
	if calls == nil {
		calls = map[string][]incrementalCall{}
	}
	calls[group] = append(calls[group], call)
	if scopedCalls == nil {
		scopedCalls = map[incrementalCallScope]incrementalScopeCalls{}
	}
	key := incrementalCallScope{group: group, scope: call.scope}
	entry, exists := scopedCalls[key]
	if !exists {
		entry.status.canonical = true
	}
	entry.calls = append(entry.calls, call)
	if len(expected) == 0 || call.component != expected[entry.status.count%len(expected)].name {
		entry.status.canonical = false
	}
	entry.status.count++
	scopedCalls[key] = entry
	return calls, scopedCalls
}

func incrementalCallsInScope(
	scopedCalls map[incrementalCallScope]incrementalScopeCalls,
	calls map[string][]incrementalCall,
	group, scope string,
) []incrementalCall {
	if scopedCalls != nil {
		return scopedCalls[incrementalCallScope{group: group, scope: scope}].calls
	}
	var result []incrementalCall
	for _, call := range calls[group] {
		if call.scope == scope {
			result = append(result, call)
		}
	}
	return result
}

type incrementalRenderSession struct {
	state        *incrementalRenderState
	base         *incrementalStateSnapshot
	graphSession *incremental.Session
	readContext  context.Context

	stores          map[string]stores.Store
	baseStores      map[string]stores.Store
	baseSnapshots   map[string]stores.ReadSnapshot
	renderSnapshots map[string]stores.ReadSnapshot
	overlayChanges  map[string][]stores.SnapshotChange
	httpComponent   *controllerhttpstore.Component
	httpWrapper     *controllerhttpstore.HTTPStoreWrapper
	httpLease       *httpstore.ActiveLeaseSnapshot
	beginHTTPLease  *httpstore.ActiveLeaseSnapshot

	members      *iradix.Txn[struct{}]
	activeGroups *iradix.Txn[struct{}]
	bindings     *iradix.Txn[string]
	retired      *iradix.Txn[struct{}]
	results      *iradix.Txn[incremental.ExactValueRoot]
	derived      *iradix.Txn[incrementalDerivedResource]
	httpEffects  *iradix.Txn[*iradix.Tree[incrementalHTTPEffect]]
	catalog      *incrementalResourceCatalog

	cursors                      map[string]incrementalStoreCursor
	httpCursor                   incrementalHTTPCursor
	groupIndexes                 map[string]*incrementalGroupIndex
	groupReady                   map[string]bool
	preparedPlan                 *incrementalPreparedPlan
	preparedPlanColdBuilder      *incrementalPreparedPlanColdBuilder
	statusPlan                   *templating.StatusPatchProjectionPlan
	planReady                    bool
	preparedPlanBootstrapPending bool
	statusPlanBootstrapPending   bool
	reloadSources                map[string]struct{}

	newQueries               map[incremental.QueryKey]struct{}
	activationQueries        map[incremental.QueryKey]struct{}
	activationValues         map[incremental.QueryKey][]string
	dirtyQueries             map[incremental.QueryKey]struct{}
	removed                  map[incremental.QueryKey]struct{}
	groupChanged             map[string]bool
	inputChanges             map[incremental.InputKey]incremental.Input
	httpObserved             map[incremental.InputKey]incremental.Input
	httpProofs               map[incremental.InputKey]httpstore.ObservationToken
	resourceProofs           map[incremental.InputKey]incremental.Input
	rootResourceProofs       map[incremental.InputKey]incremental.InputRevision
	selectorPending          map[incrementalSelectorIdentity]incremental.Input
	httpExecuted             map[incremental.QueryKey][]incrementalHTTPEffect
	freshResults             map[incremental.QueryKey]*authenticatedFreshComponentResult
	componentQueries         *queryidentity.Authority[*incrementalRenderSession]
	rootReuser               *exactCycleRootReuser
	batchResourcesMu         sync.Mutex
	capabilityAuthority      *incrementalCapabilityAuthority
	batchResources           any
	batchResourceView        *incrementalBatchResourceView
	decodedInputs            incrementalDecodedCache[incremental.InputKey, *incrementalDecodedInput]
	decodedObjects           incrementalDecodedCache[string, *incrementalCertifiedObject]
	decodedResourceInputs    incrementalDecodedCache[incremental.InputKey, *incrementalDecodedResourceInput]
	decodedResourceValues    incrementalDecodedCache[incrementalDecodedResourceValueIdentity, *incrementalCertifiedResourceItems]
	resourceMaterializations *incrementalResourceMaterializationArena
	publicationGeneration    *incrementalPublicationSnapshotGeneration
	publicationAuthority     *incrementalPublicationSnapshotAuthority
	resourceItemCache        *rendercontext.ResourceItemCache
	httpKnown                map[httpInputIdentity]httpInputSpec
	httpRetained             map[uint64]struct{}
	httpRefDeltas            map[uint64]httpRefDelta
	membershipPins           map[string]incrementalStoreCursor
	requested                map[string]bool
	calls                    map[string][]incrementalCall
	scopedCalls              map[incrementalCallScope]incrementalScopeCalls

	valueAccesses                map[string]int
	exactCycleRootCalls          map[string][]exactCycleIncrementalObservation
	exactCycleRootAuthority      *exactCycleIncrementalAuthority
	exactCycleRootOccurrence     uint64
	exactCycleRootReplay         bool
	exactCycleHTTPLease          *httpstore.AcceptedReplayState
	exactCycleHTTPPublishedLease []httpstore.ContentSnapshot
	exactCycleCacheCommitted     bool
	exactCycleOutputOnlyReplay   bool
	cacheOutputGeneration        uint64
	pendingHTTPOverlay           bool
	cacheBaseUnavailable         bool
	coldReason                   string
	cachePublicationDeferred     bool
	cachePublicationFinished     bool
	cachePublicationCallbacks    []deferredIncrementalCachePublication
	cachePublishable             bool
	cacheHoldsCandidate          bool // the render read unaccepted HTTP content; its graph must not be published
	cachePublicationEnabled      bool
	commitAcceptsCandidates      bool
	statusPatchesReplayed        bool
	fullCold                     bool
	cold                         bool
	coldVectorDisabled           bool
	bindingPlan                  *incrementalBindingPlan
	bindingCache                 *incrementalBindingCache
	bindingPlanExact             bool
	renderMode                   rendercontext.RenderMode
	transitionTime               string

	baseContext    map[string]any
	resourceErrors *rendercontext.ResourceErrorCollector
	loggerContext  incrementalLoggerContext

	mu              sync.Mutex
	stagingOverlays atomic.Bool
	transitionMu    sync.Mutex
	renderMu        sync.Mutex
	httpMu          sync.Mutex
	releaseMu       sync.Mutex
	released        bool
}

func (s *incrementalRenderState) begin(
	ctx context.Context,
	provider stores.StoreProvider,
	httpComponent *controllerhttpstore.Component,
	mode rendercontext.RenderMode,
	baseContext map[string]any,
	resourceErrors *rendercontext.ResourceErrorCollector,
	loggerContext incrementalLoggerContext,
) (*incrementalRenderSession, error) {
	if s.configErr != nil {
		return nil, s.configErr
	}
	if s.engine == nil {
		return nil, errors.New("template engine has no incremental component executor")
	}

	s.httpLifecycleMu.Lock()
	defer s.httpLifecycleMu.Unlock()
	if err := s.ensureHTTPLeaseAuthority(httpComponent); err != nil {
		return nil, err
	}
	// Resolve the HTTP sources before taking the state lock: each read waits
	// on the HTTP store's lock, which a commit holds while it takes the state
	// lock.
	httpWrapper, err := incrementalHTTPWrapper(baseContext)
	if err != nil {
		return nil, fmt.Errorf("binding http inputs: %w", err)
	}
	httpSources := incrementalHTTPSources{wrapper: httpWrapper.RevisionSource()}
	if httpComponent != nil {
		httpSources.component = httpComponent.RevisionSource()
	}
	var beginLease *httpstore.ActiveLeaseSnapshot
	if httpComponent == nil {
		s.mu.Lock()
	} else if beginLease, err = s.lockStateWithHTTPLease(httpComponent); err != nil {
		return nil, fmt.Errorf("binding http inputs: %w", err)
	}
	stateLocked := true
	defer func() {
		if stateLocked {
			s.mu.Unlock()
		}
	}()
	if err := s.validateBeginPreconditionsLocked(loggerContext); err != nil {
		return nil, err
	}
	bindings, bindingCache, bindingPlanExact, err := s.prepareBindingPlan(ctx, baseContext)
	if err != nil {
		return nil, fmt.Errorf("planning component bindings: %w", err)
	}

	pendingHTTPOverlay, err := validateIncrementalHTTPOverlay(provider, mode, httpSources.wrapper)
	if err != nil {
		return nil, fmt.Errorf("validating http overlay: %w", err)
	}
	snapshots, err := pinIncrementalStoreSnapshotsContext(ctx, s.config, bindings.required(s.required), provider)
	if err != nil {
		return nil, fmt.Errorf("pinning store snapshots: %w", err)
	}
	if err := pinIncrementalControllerSnapshots(baseContext, snapshots); err != nil {
		return nil, fmt.Errorf("pinning controller snapshots: %w", err)
	}
	httpCursor, err := s.beginHTTPBinding(httpWrapper, httpComponent, httpSources)
	if err != nil {
		return nil, fmt.Errorf("binding http inputs: %w", err)
	}

	runtime := &incrementalRenderSession{
		state:              s,
		base:               s.snapshot,
		readContext:        ctx,
		stores:             snapshots.renderStores,
		baseStores:         snapshots.baseStores,
		baseSnapshots:      snapshots.base,
		renderSnapshots:    snapshots.render,
		overlayChanges:     snapshots.overlayChanges,
		httpComponent:      httpComponent,
		httpWrapper:        httpWrapper,
		beginHTTPLease:     beginLease,
		cursors:            mapsCloneCursors(s.snapshot.cursors),
		httpCursor:         httpCursor,
		groupIndexes:       cloneGroupIndexes(s.snapshot.groupIndexes),
		groupReady:         cloneBools(s.snapshot.groupReady),
		preparedPlan:       s.snapshot.preparedPlan,
		statusPlan:         s.snapshot.statusPlan,
		reloadSources:      map[string]struct{}{},
		newQueries:         map[incremental.QueryKey]struct{}{},
		activationQueries:  map[incremental.QueryKey]struct{}{},
		activationValues:   map[incremental.QueryKey][]string{},
		dirtyQueries:       map[incremental.QueryKey]struct{}{},
		removed:            map[incremental.QueryKey]struct{}{},
		groupChanged:       map[string]bool{},
		inputChanges:       map[incremental.InputKey]incremental.Input{},
		httpObserved:       map[incremental.InputKey]incremental.Input{},
		httpProofs:         map[incremental.InputKey]httpstore.ObservationToken{},
		resourceProofs:     map[incremental.InputKey]incremental.Input{},
		rootResourceProofs: map[incremental.InputKey]incremental.InputRevision{},
		selectorPending:    map[incrementalSelectorIdentity]incremental.Input{},
		httpExecuted:       map[incremental.QueryKey][]incrementalHTTPEffect{},
		freshResults:       map[incremental.QueryKey]*authenticatedFreshComponentResult{},
		resourceItemCache:  rendercontext.NewResourceItemCache(),
		httpKnown:          map[httpInputIdentity]httpInputSpec{},
		httpRetained:       map[uint64]struct{}{},
		httpRefDeltas:      map[uint64]httpRefDelta{},
		membershipPins:     map[string]incrementalStoreCursor{},
		requested:          map[string]bool{},
		calls:              map[string][]incrementalCall{},
		scopedCalls:        map[incrementalCallScope]incrementalScopeCalls{},

		valueAccesses:           map[string]int{},
		exactCycleRootCalls:     map[string][]exactCycleIncrementalObservation{},
		exactCycleRootAuthority: newExactCycleIncrementalAuthority(),
		cacheBaseUnavailable:    s.cachePending,
		pendingHTTPOverlay:      pendingHTTPOverlay,
		cachePublishable:        true,
		cachePublicationEnabled: mode == rendercontext.RenderModeReconcile && !snapshots.hasK8sOverlays,
		bindingPlan:             bindings,
		bindingCache:            bindingCache,
		bindingPlanExact:        bindingPlanExact,
		renderMode:              mode,
		baseContext:             baseContext,
		resourceErrors:          resourceErrors,
		loggerContext:           loggerContext,
	}
	runtime.publicationGeneration, runtime.publicationAuthority = newIncrementalPublicationSnapshotGeneration()
	runtime.baseContext[incrementalControllerContextName] = runtime.incrementalControllerValue(
		ctx,
		&incrementalPinnedResourceView{session: runtime},
		true,
	)
	runtime.resetTransactions(false)
	s.mu.Unlock()
	stateLocked = false
	if err := runtime.pinGraphBase(); err != nil {
		runtime.abort()
		return nil, err
	}
	if err := runtime.startGraphSession(ctx); err != nil {
		runtime.abort()
		return nil, err
	}
	return runtime, nil
}

// pinGraphBase begins the graph session on the generation the copied base
// was committed with. The base is copied under the state lock and the graph
// session begins outside it, because a commit holds the graph lock while it
// takes the state lock; a commit landing in between moves the state past the
// copied base, and the session starts over rather than read two generations.
func (r *incrementalRenderSession) pinGraphBase() error {
	graphSession, err := r.state.graph.BeginWithResolver(r.resolveInput)
	if err != nil {
		return fmt.Errorf("starting incremental graph session: %w", err)
	}
	r.state.mu.Lock()
	moved := r.state.snapshot != r.base
	r.state.mu.Unlock()
	if moved {
		graphSession.Abort()
		return errIncrementalBaseMoved
	}
	r.graphSession = graphSession
	return nil
}

func (s *incrementalRenderState) validateBeginPreconditionsLocked(
	loggerContext incrementalLoggerContext,
) error {
	if err := s.authenticateEnvironment(loggerContext.typedResourceTypes); err != nil {
		return err
	}
	if s.retiring || s.retired {
		return errors.New("incremental render cache was retired")
	}
	if s.cachePublicationErr != nil {
		return fmt.Errorf("incremental render cache publication is poisoned: %w", s.cachePublicationErr)
	}
	if err := s.validateIncrementalCacheReadinessLocked(); err != nil {
		return err
	}
	return validateIncrementalStateSnapshotAuthentication(s.snapshot)
}

// incrementalHTTPSources carries the HTTP revision sources a session read
// before it took the state lock.
type incrementalHTTPSources struct {
	wrapper   httpstore.SourceID
	component httpstore.SourceID
}

func (s *incrementalRenderState) beginHTTPBinding(
	httpWrapper *controllerhttpstore.HTTPStoreWrapper,
	httpComponent *controllerhttpstore.Component,
	sources incrementalHTTPSources,
) (incrementalHTTPCursor, error) {
	if httpWrapper != nil && (httpComponent == nil || sources.component == 0 ||
		sources.wrapper != sources.component) {
		return incrementalHTTPCursor{}, fmt.Errorf(
			"%w: template HTTP fetcher has no matching revision source",
			errIncrementalUnsupported,
		)
	}
	httpCursor := s.snapshot.httpCursor
	if httpComponent != nil && !httpCursor.token.Valid() {
		httpCursor.token = s.httpInitial
	}
	return httpCursor, nil
}

// lockStateWithHTTPLease takes the state lock holding an HTTP lease snapshot
// begun on the token the locked base carries. The lease begins outside the
// lock because a commit holds the HTTP store while taking this lock, so a
// commit landing in between moves the token and the pair is taken again.
func (s *incrementalRenderState) lockStateWithHTTPLease(
	component *controllerhttpstore.Component,
) (*httpstore.ActiveLeaseSnapshot, error) {
	for range 8 {
		s.mu.Lock()
		token, leaseSet := s.baseHTTPTokenLocked(), s.httpLeaseSet
		s.mu.Unlock()
		lease, err := component.BeginActiveLeases(leaseSet, token)
		if errors.Is(err, httpstore.ErrActiveLeaseTokenStale) {
			continue
		}
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		if s.baseHTTPTokenLocked() == lease.Token() {
			return lease, nil
		}
		s.mu.Unlock()
	}
	return nil, httpstore.ErrActiveLeaseTokenStale
}

func (s *incrementalRenderState) baseHTTPTokenLocked() httpstore.ActiveLeaseToken {
	token := s.snapshot.httpCursor.token
	if !token.Valid() {
		token = s.httpInitial
	}
	return token
}

func (s *incrementalRenderState) ensureHTTPLeaseAuthority(
	component *controllerhttpstore.Component,
) error {
	if component == nil {
		return nil
	}
	s.mu.Lock()
	if s.retiring || s.retired {
		s.mu.Unlock()
		return errors.New("incremental render cache was retired")
	}
	missing := s.httpLeaseSet == nil
	s.mu.Unlock()
	if !missing {
		return nil
	}
	leaseSet, initial, err := component.NewActiveLeaseSet()
	if err != nil {
		return fmt.Errorf("allocating incremental HTTP leases: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retiring || s.retired || s.httpLeaseSet != nil {
		return errors.New("incremental HTTP lease authority changed during allocation")
	}
	s.httpLeaseSet = leaseSet
	s.httpInitial = initial
	return nil
}

func (r *incrementalRenderSession) incrementalTransitionTime(ctx context.Context) (string, error) {
	r.transitionMu.Lock()
	defer r.transitionMu.Unlock()
	if r.transitionTime != "" {
		return r.transitionTime, nil
	}
	value, err := r.state.sampleTransitionTime(ctx)
	if err != nil {
		return "", err
	}
	r.transitionTime = value
	return value, nil
}

func pinIncrementalControllerSnapshots(
	baseContext map[string]any,
	snapshots *incrementalStoreSnapshots,
) error {
	controller, _ := baseContext[incrementalControllerContextName].(map[string]templating.ResourceStore)
	fields := make([]string, 0, len(controller))
	for field := range controller {
		fields = append(fields, field)
	}
	slices.Sort(fields)
	for _, field := range fields {
		wrapper, ok := controller[field].(*rendercontext.StoreWrapper)
		if !ok || wrapper == nil || wrapper.Store == nil {
			return fmt.Errorf("%w: controller resource %q cannot pin an immutable root",
				errIncrementalUnsupported, field)
		}
		alias := wrapper.ResourceType
		if alias == "" {
			return fmt.Errorf("%w: controller resource %q has no resource type",
				errIncrementalUnsupported, field)
		}
		pinned, supported, err := pinStoreSnapshot(wrapper.Store)
		if err != nil {
			return fmt.Errorf("pinning controller resource %q: %w", field, err)
		}
		if !supported {
			return fmt.Errorf("%w: controller resource %q cannot pin an immutable root",
				errIncrementalUnsupported, field)
		}
		if err := validateIncrementalStoreProtocol("controller resource", field, wrapper.Store); err != nil {
			return err
		}
		if existing := snapshots.base[alias]; existing != nil &&
			(existing.RevisionSource() != pinned.RevisionSource() || snapshots.baseStores[alias] != wrapper.Store) {
			return fmt.Errorf("%w: controller resource %q conflicts with watched resource %q",
				errIncrementalUnsupported, field, alias)
		}
		snapshots.baseStores[alias] = wrapper.Store
		snapshots.renderStores[alias] = wrapper.Store
		snapshots.base[alias] = pinned
		snapshots.render[alias] = pinned
	}
	return nil
}

func (r *incrementalRenderSession) incrementalControllerValue(
	ctx context.Context,
	view rendercontext.StoreSnapshotView,
	memoize bool,
) map[string]templating.ResourceStore {
	return incrementalControllerValue(ctx, r.baseContext, view, memoize)
}

func incrementalControllerValue(
	ctx context.Context,
	baseContext map[string]any,
	view rendercontext.StoreSnapshotView,
	memoize bool,
) map[string]templating.ResourceStore {
	controller, _ := baseContext[incrementalControllerContextName].(map[string]templating.ResourceStore)
	cloned := make(map[string]templating.ResourceStore, len(controller))
	for field, resourceStore := range controller {
		wrapper, ok := resourceStore.(*rendercontext.StoreWrapper)
		if !ok || wrapper == nil {
			cloned[field] = resourceStore
			continue
		}
		cloned[field] = wrapper.CloneWithSnapshotViewContext(ctx, view, memoize)
	}
	return cloned
}

// logColdFallback names which precondition sent this render down the cold path.
// A cold graph costs roughly thirty times a warm one, so the reason is what
// distinguishes a one-off from a rate worth fixing.
func (r *incrementalRenderSession) logColdFallback() {
	if r.loggerContext.logger == nil {
		return
	}
	reason := r.coldReason
	if reason == "" {
		reason = "unattributed"
	}
	r.loggerContext.logger.Debug("Incremental render fell back to a cold graph", "reason", reason)
}

func (r *incrementalRenderSession) startGraphSession(ctx context.Context) error {
	if err := r.applyBindingPlan(); err != nil {
		return fmt.Errorf("applying binding plan: %w", err)
	}
	cold, err := r.selectFreshColdStart()
	if err != nil {
		return fmt.Errorf("selecting cold start: %w", err)
	}
	if !cold {
		cold, err = r.prepareBaseChanges(ctx)
		if err != nil {
			return fmt.Errorf("preparing base changes: %w", err)
		}
	}
	if cold {
		r.logColdFallback()
		r.resetTransactions(true)
		if err := r.applyBindingPlan(); err != nil {
			return fmt.Errorf("applying cold binding plan: %w", err)
		}
		if err := r.loadAllSources(ctx); err != nil {
			return fmt.Errorf("loading sources: %w", err)
		}
		if err := r.loadHTTPCursor(); err != nil {
			return fmt.Errorf("loading http cursor: %w", err)
		}
	}
	if err := r.applyOverlays(ctx); err != nil {
		return fmt.Errorf("applying overlays: %w", err)
	}
	if err := r.applyRenderSubject(); err != nil {
		return fmt.Errorf("applying render subject: %w", err)
	}
	inputs := sortedInputs(r.inputChanges)
	if r.cold {
		r.graphSession.Abort()
		r.graphSession, err = r.state.graph.BeginColdResetWithConcurrentResolver(r.resolveInput, inputs...)
	} else {
		err = r.graphSession.ApplyInputs(inputs...)
	}
	if err != nil {
		return fmt.Errorf("starting incremental graph session: %w", err)
	}
	r.collectRetiredQueries()
	if err := r.removeQueries(); err != nil {
		return fmt.Errorf("removing retired queries: %w", err)
	}
	if err := r.collectDirtyQueries(); err != nil {
		return fmt.Errorf("collecting dirty queries: %w", err)
	}
	return nil
}

func (r *incrementalRenderSession) collectRetiredQueries() {
	keys := make([]string, 0)
	r.retired.Root().Walk(func(key []byte, _ struct{}) bool {
		keys = append(keys, string(key))
		return false
	})
	slices.Sort(keys)
	for _, opaque := range keys {
		key := incremental.NewQueryKey(opaque)
		_, _, _, activation := parseActivationQueryKey(key)
		if !activation && r.baseHasDependents(key) {
			continue
		}
		r.removed[key] = struct{}{}
		r.retired.Delete([]byte(opaque))
	}
}

func (r *incrementalRenderSession) removeQueries() error {
	if len(r.removed) == 0 {
		return nil
	}
	removed := make([]incremental.QueryKey, 0, len(r.removed))
	for key := range r.removed {
		removed = append(removed, key)
	}
	slices.SortFunc(removed, func(left, right incremental.QueryKey) int {
		return strings.Compare(left.Opaque(), right.Opaque())
	})
	if err := r.graphSession.RemoveQueries(removed...); err != nil {
		r.graphSession.Abort()
		return fmt.Errorf("removing incremental component instances: %w", err)
	}
	return nil
}

func (r *incrementalRenderSession) collectDirtyQueries() error {
	dirty, err := r.graphSession.DirtyQueries()
	if err != nil {
		r.graphSession.Abort()
		return fmt.Errorf("enumerating invalidated incremental components: %w", err)
	}
	for _, key := range dirty {
		r.dirtyQueries[key] = struct{}{}
	}
	return nil
}

func validateIncrementalHTTPOverlay(provider stores.StoreProvider, mode rendercontext.RenderMode, source httpstore.SourceID) (bool, error) {
	overlayProvider, ok := provider.(*stores.OverlayStoreProvider)
	if !ok {
		return false, nil
	}
	overlay := overlayProvider.GetHTTPOverlay()
	if overlay == nil || overlay.IsEmpty() {
		return false, nil
	}
	frozen, exact := overlay.(*httpstore.HTTPOverlay)
	if mode != rendercontext.RenderModeAdmission || !exact || source == 0 || frozen.RevisionSource() != source {
		return false, fmt.Errorf("%w: pending HTTP overlay requires a matching immutable source in proposal validation", errIncrementalUnsupported)
	}
	return true, nil
}

func incrementalHTTPWrapper(
	baseContext map[string]any,
) (*controllerhttpstore.HTTPStoreWrapper, error) {
	rawHTTP := baseContext[incrementalHTTPContextName]
	httpWrapper, ok := rawHTTP.(*controllerhttpstore.HTTPStoreWrapper)
	if rawHTTP != nil && !ok {
		return nil, fmt.Errorf("%w: template HTTP fetcher is not revisioned", errIncrementalUnsupported)
	}
	return httpWrapper, nil
}

func (r *incrementalRenderSession) resetTransactions(cold bool) {
	if r.resourceMaterializations != nil {
		r.resourceMaterializations.revoke()
	}
	r.resourceMaterializations = newIncrementalResourceMaterializationArena()
	r.cold = cold
	r.planReady = false
	r.preparedPlanBootstrapPending = cold
	r.statusPlanBootstrapPending = cold
	r.statusPatchesReplayed = false
	r.componentQueries = queryidentity.NewAuthority(r)
	if cold {
		r.resetColdSnapshotState()
		r.resetColdTrackingState()
		return
	}
	r.resetWarmTransactions()
}

func (r *incrementalRenderSession) resetColdSnapshotState() {
	empty := newIncrementalStateSnapshot()
	r.bindings = empty.bindings.Txn()
	r.members = empty.members.Txn()
	r.activeGroups = empty.activeGroups.instances.Txn()
	r.retired = empty.retired.Txn()
	r.results = empty.results.Txn()
	r.derived = empty.derived.Txn()
	r.httpEffects = empty.httpEffects.Txn()
	r.resetCatalog(empty.catalog)
	r.cursors = map[string]incrementalStoreCursor{}
	r.httpCursor = incrementalHTTPCursor{token: r.base.httpCursor.token}
	if !r.httpCursor.token.Valid() {
		r.httpCursor.token = r.state.httpInitial
	}
	r.httpLease = nil
	r.groupIndexes = make(map[string]*incrementalGroupIndex, len(r.state.groups))
	for group := range r.state.groups {
		r.groupIndexes[group] = newIncrementalGroupIndex()
	}
	preparedPlan, err := newIncrementalPreparedPlan(
		r.state.backendPlanGroups(), r.groupIndexes, r.results.Root(),
	)
	if err != nil {
		panic(err)
	}
	preparedPlanColdBuilder, err := newIncrementalPreparedPlanColdBuilder(
		r.state.backendPlanGroups(), r.state.components,
	)
	if err != nil {
		panic(err)
	}
	r.preparedPlan = preparedPlan
	r.preparedPlanColdBuilder = preparedPlanColdBuilder
	r.statusPlan = empty.statusPlan
}

func (r *incrementalRenderSession) resetColdTrackingState() {
	r.groupReady = map[string]bool{}
	r.newQueries = map[incremental.QueryKey]struct{}{}
	r.activationQueries = map[incremental.QueryKey]struct{}{}
	r.activationValues = map[incremental.QueryKey][]string{}
	r.removed = map[incremental.QueryKey]struct{}{}
	r.groupChanged = map[string]bool{}
	r.inputChanges = map[incremental.InputKey]incremental.Input{}
	r.httpObserved = map[incremental.InputKey]incremental.Input{}
	r.httpProofs = map[incremental.InputKey]httpstore.ObservationToken{}
	r.resourceProofs = map[incremental.InputKey]incremental.Input{}
	r.rootResourceProofs = map[incremental.InputKey]incremental.InputRevision{}
	r.exactCycleRootCalls = map[string][]exactCycleIncrementalObservation{}
	r.exactCycleRootReplay = false
	r.selectorPending = map[incrementalSelectorIdentity]incremental.Input{}
	r.httpExecuted = map[incremental.QueryKey][]incrementalHTTPEffect{}
	r.freshResults = map[incremental.QueryKey]*authenticatedFreshComponentResult{}
	r.decodedInputs.reset()
	r.decodedObjects.reset()
	r.decodedResourceInputs.reset()
	r.decodedResourceValues.reset()
	r.httpRefDeltas = map[uint64]httpRefDelta{}
	r.membershipPins = map[string]incrementalStoreCursor{}
	r.reloadSources = map[string]struct{}{}
}

func (r *incrementalRenderSession) resetWarmTransactions() {
	r.bindings = r.base.bindings.Txn()
	r.members = r.base.members.Txn()
	r.activeGroups = r.base.activeGroups.instances.Txn()
	r.retired = r.base.retired.Txn()
	r.results = r.base.results.Txn()
	r.derived = r.base.derived.Txn()
	r.httpEffects = r.base.httpEffects.Txn()
	r.resetCatalog(r.base.catalog)
	r.groupIndexes = cloneGroupIndexes(r.base.groupIndexes)
	r.preparedPlan = r.base.preparedPlan
	r.preparedPlanColdBuilder = nil
	r.statusPlan = r.base.statusPlan
	r.httpRefDeltas = map[uint64]httpRefDelta{}
	r.activationQueries = map[incremental.QueryKey]struct{}{}
	r.activationValues = map[incremental.QueryKey][]string{}
}

func mapsCloneCursors(source map[string]incrementalStoreCursor) map[string]incrementalStoreCursor {
	result := make(map[string]incrementalStoreCursor, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneBools(source map[string]bool) map[string]bool {
	result := make(map[string]bool, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func sortedInputs(changes map[incremental.InputKey]incremental.Input) []incremental.Input {
	keys := make([]incremental.InputKey, 0, len(changes))
	for key := range changes {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(left, right incremental.InputKey) int {
		return strings.Compare(left.Opaque(), right.Opaque())
	})
	result := make([]incremental.Input, 0, len(keys))
	for _, key := range keys {
		result = append(result, changes[key])
	}
	return result
}

func (r *incrementalRenderSession) markSourceMembershipPin(
	alias string,
	snapshot stores.ReadSnapshot,
	trackCursor bool,
) error {
	cursor, err := resourceSnapshotCursor(alias, snapshot)
	if err != nil {
		return err
	}
	r.mu.Lock()
	if previous, exists := r.membershipPins[alias]; exists && (previous.source != cursor.source || previous.sequence != cursor.sequence) {
		r.mu.Unlock()
		return incremental.ErrRevisionConflict
	}
	r.membershipPins[alias] = cursor
	if trackCursor {
		r.cursors[alias] = cursor
	}
	r.mu.Unlock()
	return nil
}

func (r *incrementalRenderSession) updateResourceCursor(
	alias string,
	snapshot stores.ReadSnapshot,
) error {
	cursor, err := resourceSnapshotCursor(alias, snapshot)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.cursors[alias] = cursor
	r.mu.Unlock()
	return nil
}

func resourceSnapshotCursor(alias string, snapshot stores.ReadSnapshot) (incrementalStoreCursor, error) {
	if snapshot == nil || snapshot.RevisionSource() == 0 {
		return incrementalStoreCursor{}, fmt.Errorf(
			"%w: watched resource %q has no immutable root",
			errIncrementalUnsupported,
			alias,
		)
	}
	cursor := incrementalStoreCursor{source: snapshot.RevisionSource(), sequence: snapshot.Sequence()}
	if _, supported := snapshot.(stores.ExactSnapshotDiffer); supported {
		cursor.diffBase = &incrementalSnapshotDiffBase{snapshot: snapshot}
	}
	return cursor, nil
}

func (r *incrementalRenderSession) prepareBaseChanges(ctx context.Context) (bool, error) {
	if err := r.initializeActiveSources(ctx); err != nil {
		return false, err
	}
	cold, err := r.applyResourceJournals(ctx)
	if err != nil || cold {
		return cold, err
	}
	if err := r.reloadActiveSources(ctx); err != nil {
		return false, err
	}
	return r.prepareHTTPChanges()
}

func (r *incrementalRenderSession) initializeActiveSources(ctx context.Context) error {
	for source := range r.bindingPlan.bySource {
		if _, initialized := r.cursors[source]; !initialized {
			if err := r.loadSource(ctx, source); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *incrementalRenderSession) applyResourceJournals(ctx context.Context) (bool, error) {
	aliases := make([]string, 0, len(r.cursors))
	for alias := range r.cursors {
		aliases = append(aliases, alias)
	}
	slices.Sort(aliases)
	for _, alias := range aliases {
		cold, err := r.applyResourceJournal(ctx, alias)
		if err != nil || cold {
			return cold, err
		}
	}
	return false, nil
}

func (r *incrementalRenderSession) applyResourceJournal(ctx context.Context, alias string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	store := r.baseStores[alias]
	snapshot := r.baseSnapshots[alias]
	journal, journalOK := store.(stores.ExactRevisionJournal)
	cursor := r.cursors[alias]
	if snapshot == nil || !journalOK || journal.ExactRevisionJournalSource() != snapshot.RevisionSource() ||
		snapshot.RevisionSource() != cursor.source {
		r.coldReason = "store-revision-source-changed:" + alias
		return true, nil
	}
	changes, revisionBase, complete, err := snapshotCursorChanges(ctx, journal, cursor, snapshot)
	if err != nil {
		return false, err
	}
	if !complete {
		r.coldReason = "journal-incomplete:" + alias
		return true, nil
	}
	_, isComponentSource := r.bindingPlan.bySource[alias]
	if isComponentSource {
		err = r.markSourceMembershipPin(alias, snapshot, true)
	} else {
		err = r.updateResourceCursor(alias, snapshot)
	}
	if err != nil {
		return false, err
	}
	if err := r.applyJournalChanges(alias, changes); err != nil {
		if errors.Is(err, errIncrementalUnsupported) {
			r.coldReason = "journal-change-unsupported:" + alias
			return true, nil
		}
		return false, err
	}
	if revisionBase != nil {
		if err := r.refreshSnapshotRevisions(ctx, alias, revisionBase, snapshot); err != nil {
			return false, err
		}
	}
	return false, nil
}

func (r *incrementalRenderSession) refreshSnapshotRevisions(
	ctx context.Context,
	alias string,
	before, after stores.ReadSnapshot,
) error {
	catalog, err := r.authenticatedCatalog()
	if err != nil {
		return err
	}
	if !catalog.base.valid() {
		return errors.New("incremental resource catalog has invalid base")
	}
	affected := map[string]resourceInputSpec{}
	var walkErr error
	catalog.base.Root().WalkPrefix(resourceInputPrefix(alias), func(rawKey []byte, _ struct{}) bool {
		if walkErr = ctx.Err(); walkErr != nil {
			return true
		}
		key := incremental.NewInputKey(string(rawKey))
		walkErr = r.collectChangedSnapshotRevision(key, before, after, affected)
		return walkErr != nil
	})
	if walkErr != nil {
		return walkErr
	}
	return r.refreshKnownInputs(alias, affected)
}

func (r *incrementalRenderSession) collectChangedSnapshotRevision(
	key incremental.InputKey,
	before, after stores.ReadSnapshot,
	affected map[string]resourceInputSpec,
) error {
	spec, exists, err := r.catalogGet(key)
	if err != nil || !exists {
		return err
	}
	previous, err := resourceSnapshotRevision(before, &spec)
	if err != nil {
		return err
	}
	current, err := resourceSnapshotRevision(after, &spec)
	if err != nil || current == previous {
		return err
	}
	return r.collectKnownInput(&spec, affected)
}

func (r *incrementalRenderSession) reloadActiveSources(ctx context.Context) error {
	sources := make([]string, 0, len(r.reloadSources))
	for source := range r.reloadSources {
		sources = append(sources, source)
	}
	slices.Sort(sources)
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.loadSource(ctx, source); err != nil {
			return err
		}
	}
	return nil
}

func (r *incrementalRenderSession) prepareHTTPChanges() (bool, error) {
	if r.httpComponent == nil {
		return r.httpCursor.token.Valid(), nil
	}
	if r.state.httpLeaseSet == nil || !r.httpCursor.token.Valid() ||
		r.httpCursor.token.Source() != r.httpComponent.RevisionSource() {
		return false, fmt.Errorf("%w: HTTP lease authority changed", errIncrementalUnsupported)
	}
	snapshot, err := r.beginActiveLeases(r.httpCursor.token)
	if err != nil {
		return false, fmt.Errorf("beginning incremental HTTP leases: %w", err)
	}
	r.httpLease = snapshot
	for _, change := range snapshot.Changes() {
		spec, cached, specErr := r.state.httpInputForActiveChange(&change)
		if specErr != nil {
			return false, specErr
		}
		if !cached {
			if snapshot.ReplayContains(change.URL, change.Descriptor) {
				continue
			}
			return false, fmt.Errorf("active HTTP lease %s has no cached dependency", change.URL)
		}
		if err := r.retainHTTPInputSpec(spec); err != nil {
			return false, err
		}
		input, err := r.readHTTPInput(spec)
		if err != nil {
			return false, err
		}
		r.inputChanges[input.Key] = input
	}
	return false, nil
}

func (r *incrementalRenderSession) applyJournalChanges(
	alias string,
	changes []stores.RevisionChange,
) error {
	affected := map[string]resourceInputSpec{}
	for changeIndex := range changes {
		if err := r.applyJournalChange(alias, &changes[changeIndex], affected); err != nil {
			return err
		}
	}
	return r.refreshKnownInputs(alias, affected)
}

func (r *incrementalRenderSession) applyJournalChange(
	alias string,
	change *stores.RevisionChange,
	affected map[string]resourceInputSpec,
) error {
	if change.Name == "" {
		return errIncrementalUnsupported
	}
	if err := r.collectKnownInput(
		&resourceInputSpec{resourceType: alias, scope: resourceInputList}, affected,
	); err != nil {
		return err
	}
	if err := r.collectKnownInput(&resourceInputSpec{
		resourceType: alias,
		scope:        resourceInputIdentity,
		namespace:    change.Namespace,
		name:         change.Name,
	}, affected); err != nil {
		return err
	}
	for _, keys := range [][]string{change.OldKeys, change.NewKeys} {
		for count := 1; count <= len(keys); count++ {
			if err := r.collectKnownInput(&resourceInputSpec{
				resourceType: alias,
				scope:        resourceInputGet,
				keys:         slices.Clone(keys[:count]),
			}, affected); err != nil {
				return err
			}
		}
	}
	if len(r.bindingPlan.bySource[alias]) > 0 {
		if err := r.refreshSourceIdentity(
			alias,
			r.renderSnapshots[alias],
			change.Namespace,
			change.Name,
		); err != nil {
			return err
		}
	}
	return nil
}

func (r *incrementalRenderSession) collectKnownInput(
	spec *resourceInputSpec,
	affected map[string]resourceInputSpec,
) error {
	key := resourceInputKey(spec)
	known, exists, err := r.catalogGet(key)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if !r.baseHasInputDependents(key) {
		if err := r.catalogDelete(key); err != nil {
			return err
		}
		return nil
	}
	affected[key.Opaque()] = known
	return nil
}

func (r *incrementalRenderSession) refreshKnownInputs(
	alias string,
	affected map[string]resourceInputSpec,
) error {
	keys := make([]string, 0, len(affected))
	for key := range affected {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		spec := affected[key]
		input, err := r.readResourceInput(r.renderSnapshots[alias], &spec)
		if err != nil {
			return err
		}
		r.inputChanges[input.Key] = input
	}
	return nil
}

func (r *incrementalRenderSession) loadAllSources(ctx context.Context) error {
	sources := make([]string, 0, len(r.bindingPlan.bySource))
	for source := range r.bindingPlan.bySource {
		sources = append(sources, source)
	}
	slices.Sort(sources)
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.loadSource(ctx, source); err != nil {
			return err
		}
	}
	return nil
}

func (r *incrementalRenderSession) loadSource(ctx context.Context, source string) error {
	snapshot := r.baseSnapshots[source]
	if snapshot == nil || snapshot.RevisionSource() == 0 {
		return fmt.Errorf("%w: watched resource %q", errIncrementalUnsupported, source)
	}
	items, err := readIncrementalSnapshotList(ctx, snapshot)
	if err != nil {
		return fmt.Errorf("snapshotting incremental source %q: %w", source, err)
	}
	if err := r.markSourceMembershipPin(source, snapshot, true); err != nil {
		return err
	}
	remaining := map[string][2]string{}
	r.members.Root().WalkPrefix(memberPrefix(source), func(key []byte, _ struct{}) bool {
		namespace, name, ok := parseMemberKey(key)
		if ok {
			remaining[string(key)] = [2]string{namespace, name}
		}
		return false
	})
	seen := map[string]struct{}{}
	for _, item := range items {
		namespace, name, ok := resourceIdentity(item)
		if !ok {
			return fmt.Errorf("%w: source %q has an object without metadata.name", errIncrementalUnsupported, source)
		}
		identity := namespace + "\x00" + name
		if _, duplicate := seen[identity]; duplicate {
			return fmt.Errorf("%w: source %q repeats identity %q/%q", errIncrementalUnsupported, source, namespace, name)
		}
		seen[identity] = struct{}{}
		delete(remaining, string(memberKey(source, namespace, name)))
		if err := r.refreshSourceIdentity(source, snapshot, namespace, name); err != nil {
			return err
		}
	}
	removed := make([]string, 0, len(remaining))
	for key := range remaining {
		removed = append(removed, key)
	}
	slices.Sort(removed)
	for _, key := range removed {
		identity := remaining[key]
		if err := r.updateMembership(source, identity[0], identity[1], false); err != nil {
			return err
		}
	}
	return nil
}

// beginActiveLeases returns the lease pinned with the base at begin for the
// base token, so a commit after begin cannot strand the session on a token the
// store no longer accepts.
func (r *incrementalRenderSession) beginActiveLeases(
	token httpstore.ActiveLeaseToken,
) (*httpstore.ActiveLeaseSnapshot, error) {
	if r.beginHTTPLease != nil && r.beginHTTPLease.Token() == token {
		return r.beginHTTPLease, nil
	}
	return r.httpComponent.BeginActiveLeases(r.state.httpLeaseSet, token)
}

func (r *incrementalRenderSession) loadHTTPCursor() error {
	if r.httpComponent == nil {
		return nil
	}
	if r.state.httpLeaseSet == nil {
		return fmt.Errorf("%w: HTTP store has no lease set", errIncrementalUnsupported)
	}
	token := r.base.httpCursor.token
	if !token.Valid() {
		token = r.state.httpInitial
	}
	if !token.Valid() || token.Source() != r.httpComponent.RevisionSource() {
		return fmt.Errorf("%w: HTTP store has no revision source", errIncrementalUnsupported)
	}
	snapshot, err := r.beginActiveLeases(token)
	if err != nil {
		return err
	}
	r.httpCursor = incrementalHTTPCursor{token: token}
	r.httpLease = snapshot
	return nil
}

func (r *incrementalRenderSession) httpInput(
	identity httpInputIdentity,
) (httpInputSpec, incremental.InputKey, error) {
	r.httpMu.Lock()
	defer r.httpMu.Unlock()
	if spec, exists := r.httpKnown[identity]; exists {
		return spec, httpInputKey(spec.id), nil
	}
	spec, key, err := r.state.acquireHTTPInput(identity)
	if err != nil {
		return httpInputSpec{}, incremental.InputKey{}, err
	}
	r.httpKnown[identity] = spec
	r.httpRetained[spec.id] = struct{}{}
	return spec, key, nil
}

func (r *incrementalRenderSession) retainHTTPInputSpec(spec httpInputSpec) error {
	r.httpMu.Lock()
	defer r.httpMu.Unlock()
	if _, retained := r.httpRetained[spec.id]; retained {
		return nil
	}
	if err := r.state.retainHTTPInputSpec(spec.id); err != nil {
		return err
	}
	r.httpKnown[spec.httpInputIdentity] = spec
	r.httpRetained[spec.id] = struct{}{}
	return nil
}

func (r *incrementalRenderSession) httpInputSpec(
	key incremental.InputKey,
) (httpInputSpec, bool, error) {
	spec, exists := r.state.httpInputSpec(key)
	if !exists {
		return httpInputSpec{}, false, nil
	}
	if err := r.retainHTTPInputSpec(spec); err != nil {
		return httpInputSpec{}, false, err
	}
	return spec, true, nil
}

func (r *incrementalRenderSession) readHTTPInput(spec httpInputSpec) (incremental.Input, error) {
	if r.httpComponent == nil {
		return incremental.Input{}, fmt.Errorf("%w: HTTP store is unavailable", errIncrementalUnsupported)
	}
	source := r.httpComponent.RevisionSource()
	if source == 0 || !r.httpCursor.token.Valid() || r.httpCursor.token.Source() != source {
		return incremental.Input{}, incremental.ErrRevisionConflict
	}
	snapshot, found := r.httpComponent.AcceptedSnapshot(spec.url, spec.descriptor)
	snapshot.Found = found
	if found && (!snapshot.Cacheable || !snapshot.Token.Valid() || snapshot.Token.Kind() != httpstore.SnapshotAccepted) {
		return incremental.Input{}, fmt.Errorf("%w: HTTP input %s has no accepted revision", errIncrementalUnsupported, spec.url)
	}
	input := incremental.Input{
		Key:      httpInputKey(spec.id),
		Revision: httpInputRevision(source, &snapshot),
		Found:    found,
		Value:    []byte(snapshot.Content),
	}
	if err := r.observeHTTPInput(input, &snapshot); err != nil {
		return incremental.Input{}, err
	}
	return input, nil
}

func (r *incrementalRenderSession) observeHTTPInput(
	input incremental.Input,
	snapshot *httpstore.ContentSnapshot,
) error {
	if snapshot == nil {
		return incremental.ErrRevisionConflict
	}
	proof := snapshot.ObservationToken()
	r.httpMu.Lock()
	defer r.httpMu.Unlock()
	if previous, exists := r.httpObserved[input.Key]; exists {
		if previous.Revision != input.Revision || previous.Found != input.Found ||
			!bytes.Equal(previous.Value, input.Value) {
			return incremental.ErrRevisionConflict
		}
		return nil
	}
	input.Value = slices.Clone(input.Value)
	r.httpObserved[input.Key] = input
	if proof.Valid() && r.httpComponent != nil &&
		snapshot.StoreSource == r.httpComponent.RevisionSource() {
		r.httpProofs[input.Key] = proof
	}
	return nil
}

func (r *incrementalRenderSession) refreshSourceIdentity(
	alias string,
	snapshot stores.ReadSnapshot,
	namespace, name string,
) error {
	spec := resourceInputSpec{
		resourceType: alias,
		scope:        resourceInputIdentity,
		namespace:    namespace,
		name:         name,
	}
	input, err := r.readResourceInput(snapshot, &spec)
	if err != nil {
		return err
	}
	if err := r.catalogInsert(input.Key, &spec); err != nil {
		return err
	}
	r.inputChanges[input.Key] = input
	return r.updateMembership(alias, namespace, name, input.Found)
}

func (r *incrementalRenderSession) updateMembership(alias, namespace, name string, found bool) error {
	key := memberKey(alias, namespace, name)
	_, existed := r.members.Get(key)
	if found {
		r.members.Insert(key, struct{}{})
		r.admitMembership(alias, namespace, name, existed)
		return nil
	}
	if !existed {
		return nil
	}
	r.members.Delete(key)
	activation := activationQueryKey(alias, namespace, name)
	r.retired.Insert([]byte(activation.Opaque()), struct{}{})
	delete(r.activationQueries, activation)
	delete(r.activationValues, activation)
	return r.retireMembership(alias, namespace, name)
}

func (r *incrementalRenderSession) admitMembership(
	alias, namespace, name string,
	existed bool,
) {
	activation := activationQueryKey(alias, namespace, name)
	for index := range r.bindingPlan.bySource[alias] {
		component := &r.bindingPlan.bySource[alias][index]
		query := r.membershipQueryKey(component, alias, namespace, name)
		r.retired.Delete([]byte(query.Opaque()))
		if len(component.activationPaths) > 0 {
			r.retired.Delete([]byte(activation.Opaque()))
			r.activationQueries[activation] = struct{}{}
		}
		if component.deriveResource {
			r.retired.Delete([]byte(derivedProjectionQueryKey(alias, namespace, name).Opaque()))
		}
		if len(component.activationPaths) == 0 {
			_, hasResult := r.results.Get(resultKey(component, alias, namespace, name))
			if !hasResult {
				r.newQueries[query] = struct{}{}
			}
		}
		if !existed {
			r.groupChanged[component.group] = true
		}
	}
}

func (r *incrementalRenderSession) retireMembership(alias, namespace, name string) error {
	for index := range r.bindingPlan.bySource[alias] {
		component := &r.bindingPlan.bySource[alias][index]
		query := r.membershipQueryKey(component, alias, namespace, name)
		r.retired.Insert([]byte(query.Opaque()), struct{}{})
		if len(component.activationPaths) > 0 {
			if err := r.setActivationInstanceActive(component, alias, namespace, name, false); err != nil {
				return err
			}
		}
		if component.deriveResource {
			r.retired.Insert([]byte(derivedProjectionQueryKey(alias, namespace, name).Opaque()), struct{}{})
		}
		delete(r.newQueries, query)
		if err := r.deleteResult(component, alias, namespace, name); err != nil {
			return err
		}
	}
	return nil
}

func (r *incrementalRenderSession) replaceHTTPEffects(
	key []byte,
	effects []incrementalHTTPEffect,
) (bool, error) {
	next, err := newIncrementalIndexedHTTPEffects(effects)
	if err != nil {
		return false, err
	}
	previous, existed := r.httpEffects.Get(key)
	if existed {
		same, compareErr := sameIndexedHTTPEffects(previous, next)
		if compareErr != nil {
			return false, compareErr
		}
		if same {
			return false, nil
		}
	} else if next.Len() == 0 {
		return false, nil
	}
	if previous != nil {
		previous.Root().Walk(func(_ []byte, effect incrementalHTTPEffect) bool {
			r.adjustHTTPRefDelta(effect.inputID, false)
			return false
		})
	}
	next.Root().Walk(func(_ []byte, effect incrementalHTTPEffect) bool {
		r.adjustHTTPRefDelta(effect.inputID, true)
		return false
	})
	if next.Len() == 0 {
		r.httpEffects.Delete(key)
	} else {
		r.httpEffects.Insert(key, next)
	}
	return true, nil
}

func (r *incrementalRenderSession) adjustHTTPRefDelta(id uint64, add bool) {
	if err := adjustHTTPRefDeltaIn(r.httpRefDeltas, id, add); err != nil {
		panic(err)
	}
}

func adjustHTTPRefDeltaIn(deltas map[uint64]httpRefDelta, id uint64, add bool) error {
	delta := deltas[id]
	if add {
		if delta.removed > 0 {
			delta.removed--
		} else {
			if delta.added == ^uint64(0) {
				return errors.New("incremental HTTP reference addition delta exhausted")
			}
			delta.added++
		}
	} else if delta.added > 0 {
		delta.added--
	} else {
		if delta.removed == ^uint64(0) {
			return errors.New("incremental HTTP reference removal delta exhausted")
		}
		delta.removed++
	}
	if delta == (httpRefDelta{}) {
		delete(deltas, id)
	} else {
		deltas[id] = delta
	}
	return nil
}

func (r *incrementalRenderSession) applyOverlays(ctx context.Context) error {
	r.stagingOverlays.Store(true)
	defer r.stagingOverlays.Store(false)
	aliases := make([]string, 0, len(r.overlayChanges))
	for alias := range r.overlayChanges {
		aliases = append(aliases, alias)
	}
	slices.Sort(aliases)
	for _, alias := range aliases {
		if err := ctx.Err(); err != nil {
			return err
		}
		changes := r.overlayChanges[alias]
		for changeIndex := range changes {
			if err := r.applyOverlayChange(alias, &changes[changeIndex]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *incrementalRenderSession) applyOverlayChange(
	alias string,
	change *stores.SnapshotChange,
) error {
	spec := resourceInputSpec{
		resourceType: alias,
		scope:        resourceInputIdentity,
		namespace:    change.Namespace,
		name:         change.Name,
	}
	input, err := r.readResourceInput(r.renderSnapshots[alias], &spec)
	if err != nil {
		return err
	}
	if err := r.catalogInsert(input.Key, &spec); err != nil {
		return err
	}
	r.inputChanges[input.Key] = input
	if len(r.bindingPlan.bySource[alias]) > 0 {
		if err := r.updateMembership(alias, change.Namespace, change.Name, input.Found); err != nil {
			return err
		}
	}
	return r.applyOverlayIndexes(alias, change)
}

func (r *incrementalRenderSession) applyOverlayIndexes(
	alias string,
	change *stores.SnapshotChange,
) error {
	affected := map[string]resourceInputSpec{}
	if err := r.collectKnownInput(&resourceInputSpec{resourceType: alias, scope: resourceInputList}, affected); err != nil {
		return err
	}
	for _, keys := range [][]string{change.OldKeys, change.NewKeys} {
		for count := 1; count <= len(keys); count++ {
			if err := r.collectKnownInput(&resourceInputSpec{
				resourceType: alias,
				scope:        resourceInputGet,
				keys:         slices.Clone(keys[:count]),
			}, affected); err != nil {
				return err
			}
		}
	}
	keys := make([]string, 0, len(affected))
	for key := range affected {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		spec := affected[key]
		input, err := r.readResourceInput(r.renderSnapshots[alias], &spec)
		if err != nil {
			return err
		}
		r.inputChanges[input.Key] = input
	}
	return nil
}

func (r *incrementalRenderSession) readResourceInput(
	snapshot stores.ReadSnapshot,
	spec *resourceInputSpec,
) (incremental.Input, error) {
	return r.readResourceInputContext(r.contextForReads(), snapshot, spec)
}

func (r *incrementalRenderSession) contextForReads() context.Context {
	if r.readContext != nil {
		return r.readContext
	}
	return context.Background()
}

func (r *incrementalRenderSession) readResourceInputContext(
	ctx context.Context,
	snapshot stores.ReadSnapshot,
	spec *resourceInputSpec,
) (incremental.Input, error) {
	if snapshot == nil || snapshot.RevisionSource() == 0 {
		r.disableCachePublication()
		return incremental.Input{}, fmt.Errorf(
			"%w: incremental component read undeclared or unsupported watched resource %q",
			errIncrementalUnsupported,
			spec.resourceType,
		)
	}
	input, materialized, err := r.materializeResourceInputContext(ctx, snapshot, spec)
	if err != nil {
		return incremental.Input{}, err
	}
	if materialized != nil {
		input = materialized.input()
	}
	err = r.updateResourceCursor(spec.resourceType, snapshot)
	if err != nil {
		return incremental.Input{}, err
	}
	if !r.cachePublicationEnabled {
		if err := r.recordResourceProof(input); err != nil {
			return incremental.Input{}, err
		}
	}
	return input, nil
}

func (r *incrementalRenderSession) materializeResourceInputContext(
	ctx context.Context,
	snapshot stores.ReadSnapshot,
	spec *resourceInputSpec,
) (incremental.Input, *incrementalResourceMaterialization, error) {
	if r.resourceMaterializations != nil && r.resourceSnapshotMatchesRenderGeneration(snapshot, spec) {
		materialized, supported, err := r.resourceMaterializations.ensure(ctx, snapshot, spec)
		if err != nil {
			return incremental.Input{}, nil, err
		}
		if supported {
			return incremental.Input{}, materialized, nil
		}
	}
	input, err := readResourceSnapshotInput(ctx, snapshot, spec)
	return input, nil, err
}

func (r *incrementalRenderSession) resourceSnapshotMatchesRenderGeneration(
	snapshot stores.ReadSnapshot,
	spec *resourceInputSpec,
) bool {
	if snapshot == nil || spec == nil {
		return false
	}
	renderSnapshot := r.renderSnapshots[spec.resourceType]
	return renderSnapshot != nil && renderSnapshot.RevisionSource() == snapshot.RevisionSource() &&
		renderSnapshot.Sequence() == snapshot.Sequence()
}

func readResourceSnapshotInput(
	ctx context.Context,
	snapshot stores.ReadSnapshot,
	spec *resourceInputSpec,
) (incremental.Input, error) {
	var (
		value any
		found bool
		err   error
	)
	switch spec.scope {
	case resourceInputList:
		value, err = readIncrementalSnapshotList(ctx, snapshot)
		found = true
	case resourceInputGet:
		value, err = readIncrementalSnapshotGet(ctx, snapshot, spec.keys...)
		if err == nil {
			found = len(value.([]any)) > 0
		}
	case resourceInputIdentity:
		value, found, err = readIncrementalSnapshotIdentity(ctx, snapshot, spec.namespace, spec.name)
	default:
		return incremental.Input{}, errors.New("incremental resource input has an invalid scope")
	}
	if err != nil {
		return incremental.Input{}, err
	}
	return encodeResourceSnapshotInput(snapshot, spec, value, found)
}

func encodeResourceSnapshotInput(
	snapshot stores.ReadSnapshot,
	spec *resourceInputSpec,
	value any,
	found bool,
) (incremental.Input, error) {
	encoded := []byte(nil)
	if found || spec.scope == resourceInputList {
		var err error
		encoded, err = encodeResourceValue(value)
		if err != nil {
			return incremental.Input{}, err
		}
	}
	return encodedResourceSnapshotInput(snapshot, spec, found, encoded)
}

func encodedResourceSnapshotInput(
	snapshot stores.ReadSnapshot,
	spec *resourceInputSpec,
	found bool,
	encoded []byte,
) (incremental.Input, error) {
	revision, err := resourceSnapshotRevision(snapshot, spec)
	if err != nil {
		return incremental.Input{}, err
	}
	if revision == "" {
		return incremental.Input{}, errIncrementalUnsupported
	}
	return incremental.Input{
		Key:      resourceInputKey(spec),
		Revision: storeRevision(snapshot.RevisionSource(), revision),
		Found:    found,
		Value:    encoded,
	}, nil
}

func resourceSnapshotRevision(
	snapshot stores.ReadSnapshot,
	spec *resourceInputSpec,
) (stores.Revision, error) {
	switch spec.scope {
	case resourceInputList:
		return snapshot.ListRevision(), nil
	case resourceInputGet:
		return snapshot.GetRevision(spec.keys...), nil
	case resourceInputIdentity:
		return snapshot.IdentityRevision(spec.namespace, spec.name), nil
	default:
		return "", errors.New("incremental resource input has an invalid scope")
	}
}

func (r *incrementalRenderSession) resolveInput(
	ctx context.Context,
	key incremental.InputKey,
) (incremental.Input, error) {
	if component, source, ok := parseBindingInputKey(key); ok {
		props, exists := r.bindingPlan.props[string(bindingKey(component, source))]
		if !exists {
			return absentBindingInput(incrementalBinding{component: component, source: source}), nil
		}
		return bindingInput(incrementalBinding{component: component, source: source, props: props}), nil
	}
	if source, namespace, name, ok := parseRenderSubjectInputKey(key); ok {
		return r.renderSubjectInput(source, namespace, name)
	}
	if source, ok := parseDeriveOwnerInputKey(key); ok {
		owner, found := r.bindingPlan.owners[source]
		return deriveOwnerInput(source, &owner, found), nil
	}
	if input, handled, err := r.resolveSelectorInput(key); handled {
		return input, err
	}
	r.httpMu.Lock()
	observed, httpObserved := r.httpObserved[key]
	r.httpMu.Unlock()
	if httpObserved {
		return observed, nil
	}
	if spec, ok, err := r.httpInputSpec(key); err != nil {
		return incremental.Input{}, err
	} else if ok {
		return r.readHTTPInput(spec)
	}
	spec, ok := parseResourceInputKey(key)
	if !ok {
		return incremental.Input{}, fmt.Errorf("incremental input %s has no resolver", key)
	}
	if catalogErr := r.catalogLoadOrStore(key, &spec); catalogErr != nil {
		return incremental.Input{}, catalogErr
	}
	return r.readResourceInputContext(ctx, r.renderSnapshots[spec.resourceType], &spec)
}

func (r *incrementalRenderSession) resolveSelectorInput(
	key incremental.InputKey,
) (input incremental.Input, handled bool, err error) {
	if identity, ok := parseIncrementalSelectorInputKey(key); ok {
		index := r.groupIndexes[identity.group]
		if groupErr := r.validateSelectorGroup(identity.group, index); groupErr != nil {
			return incremental.Input{}, true, groupErr
		}
		input, err = incrementalSelectorInput(index, identity.group, identity.cell, identity.key)
		return input, true, err
	}
	if identity, ok := parseIncrementalSelectorValuesInputKey(key); ok {
		index := r.groupIndexes[identity.group]
		if groupErr := r.validateSelectorGroup(identity.group, index); groupErr != nil {
			return incremental.Input{}, true, groupErr
		}
		input, err = incrementalSelectorValuesInput(index, identity.group, identity.cell)
		return input, true, err
	}
	if identity, ok := parseIncrementalSelectorCountInputKey(key); ok {
		index := r.groupIndexes[identity.group]
		if groupErr := r.validateSelectorGroup(identity.group, index); groupErr != nil {
			return incremental.Input{}, true, groupErr
		}
		input, err = incrementalSelectorCountInput(index, identity.group, identity.cell)
		return input, true, err
	}
	return incremental.Input{}, false, nil
}

func (r *incrementalRenderSession) validateSelectorGroup(
	group string,
	index *incrementalGroupIndex,
) error {
	if index != nil {
		return nil
	}
	if _, absent := r.state.config.AbsentIncrementalGroups[group]; absent {
		return nil
	}
	return fmt.Errorf("incremental publication group %q is unavailable", group)
}

type incrementalResourceView struct {
	ctx     context.Context
	reader  incremental.Reader
	session *incrementalRenderSession
	lease   *incrementalBatchReaderLease
}

type incrementalBatchResourceView struct {
	seal      *incrementalBatchResourceView
	session   *incrementalRenderSession
	authority *incrementalCapabilityAuthority
}

func (*incrementalResourceView) MemoizeStoreItems() bool {
	return true
}

func (*incrementalResourceView) PreserveStoreValues() bool {
	return true
}

func (*incrementalBatchResourceView) NormalizeLookupKeys(_ string, keys []any) ([]string, error) {
	return templating.CanonicalIncrementalResourceKeys(keys...)
}

func (v *incrementalBatchResourceView) BeginStoreInvocation(
	ctx context.Context,
) (context.Context, func(), error) {
	if v == nil || v.seal != v || v.authority == nil || v.authority.seal != v.authority {
		return nil, nil, errors.New("incremental component batch resource view has invalid provenance")
	}
	lease, _ := ctx.Value(incrementalCapabilityLeaseContextKey{}).(*incrementalBatchReaderLease)
	if lease == nil || lease.authority != v.authority {
		err := errors.New("incremental component batch resource view has no matching capability lease")
		if v.authority.resourceErrors != nil {
			v.authority.resourceErrors.Record(err)
		}
		if lease != nil {
			lease.fail(err)
		}
		return nil, nil, err
	}
	return lease.begin(ctx, "resource capability")
}

func (v *incrementalBatchResourceView) BeginBoundStoreInvocation(
	ctx context.Context,
	lease templating.IncrementalResourceInvocationLease,
) (context.Context, func(), error) {
	if v == nil || v.seal != v || v.authority == nil || v.authority.seal != v.authority {
		return nil, nil, errors.New("incremental component batch resource view has invalid provenance")
	}
	boundLease, ok := lease.(*incrementalBatchReaderLease)
	if !ok || boundLease == nil || boundLease.authority != v.authority {
		err := errors.New("incremental component batch resource view has no matching bound capability lease")
		if v.authority.resourceErrors != nil {
			v.authority.resourceErrors.Record(err)
		}
		if boundLease != nil {
			boundLease.fail(err)
		}
		return nil, nil, err
	}
	return boundLease.beginResourceInvocation(ctx)
}

func (*incrementalBatchResourceView) MemoizeStoreMaterialization() bool {
	return false
}

func (*incrementalBatchResourceView) MemoizeStoreItems() bool {
	return true
}

func (*incrementalBatchResourceView) PreserveStoreValues() bool {
	return true
}

func (v *incrementalBatchResourceView) List(resourceType string, _ stores.Store) ([]any, error) {
	return nil, fmt.Errorf("incremental component batch resource %q requires an execution context", resourceType)
}

func (v *incrementalBatchResourceView) Get(
	resourceType string,
	_ stores.Store,
	keys ...string,
) ([]any, error) {
	return nil, fmt.Errorf("incremental component batch resource %q lookup %q requires an execution context", resourceType, keys)
}

func (v *incrementalBatchResourceView) ListContext(
	ctx context.Context,
	resourceType string,
	_ stores.Store,
) ([]any, error) {
	return v.readContext(ctx, &resourceInputSpec{resourceType: resourceType, scope: resourceInputList})
}

func (v *incrementalBatchResourceView) GetContext(
	ctx context.Context,
	resourceType string,
	_ stores.Store,
	keys ...string,
) ([]any, error) {
	return v.readContext(ctx, &resourceInputSpec{
		resourceType: resourceType,
		scope:        resourceInputGet,
		keys:         slices.Clone(keys),
	})
}

func (v *incrementalBatchResourceView) readContext(
	ctx context.Context,
	spec *resourceInputSpec,
) ([]any, error) {
	lease, _ := ctx.Value(incrementalCapabilityInvocationContextKey{}).(*incrementalBatchReaderLease)
	if lease == nil || lease.authority != v.authority {
		return nil, errors.New("incremental component batch resource invocation has no matching lease")
	}
	if err := lease.validateInvocation(ctx); err != nil {
		lease.fail(err)
		return nil, err
	}
	items, err := (&incrementalResourceView{
		ctx: lease.ctx, reader: lease.reader, session: v.session,
	}).readContext(lease.ctx, spec)
	if err != nil {
		return nil, err
	}
	return lease.derived.Project(spec.resourceType, items)
}

func (*incrementalResourceView) NormalizeLookupKeys(_ string, keys []any) ([]string, error) {
	return templating.CanonicalIncrementalResourceKeys(keys...)
}

func (v *incrementalResourceView) BeginStoreInvocation(
	ctx context.Context,
) (context.Context, func(), error) {
	if v.lease == nil {
		return ctx, func() {}, nil
	}
	return v.lease.begin(ctx, "resource capability")
}

func (v *incrementalResourceView) ListContext(
	ctx context.Context,
	resourceType string,
	_ stores.Store,
) ([]any, error) {
	return v.readContext(ctx, &resourceInputSpec{resourceType: resourceType, scope: resourceInputList})
}

func (v *incrementalResourceView) GetContext(
	ctx context.Context,
	resourceType string,
	_ stores.Store,
	keys ...string,
) ([]any, error) {
	return v.readContext(ctx, &resourceInputSpec{
		resourceType: resourceType,
		scope:        resourceInputGet,
		keys:         slices.Clone(keys),
	})
}

type incrementalPinnedResourceView struct {
	session *incrementalRenderSession
}

// recordResourceProof holds a read to the value the render already saw for that
// key, except while the admission overlay is being staged: the overlay's whole
// purpose is to give the key a different value than the base reads recorded, so
// checking it there denies every update to an already-read resource.
func (r *incrementalRenderSession) recordResourceProof(input incremental.Input) error {
	if r.stagingOverlays.Load() {
		r.overrideResourceProof(input)
		return nil
	}
	return r.observeResourceProof(input)
}

func (r *incrementalRenderSession) overrideResourceProof(input incremental.Input) {
	r.mu.Lock()
	defer r.mu.Unlock()
	input.Value = slices.Clone(input.Value)
	r.resourceProofs[input.Key] = input
}

func (r *incrementalRenderSession) observeResourceProof(input incremental.Input) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if previous, exists := r.resourceProofs[input.Key]; exists {
		if previous.Revision != input.Revision || previous.Found != input.Found ||
			!bytes.Equal(previous.Value, input.Value) {
			return incremental.ErrRevisionConflict
		}
		return nil
	}
	input.Value = slices.Clone(input.Value)
	r.resourceProofs[input.Key] = input
	return nil
}

func (r *incrementalRenderSession) recordPinnedResourceInput(
	input incremental.Input,
) error {
	if err := r.observeResourceProof(input); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rootResourceProofs == nil {
		r.rootResourceProofs = map[incremental.InputKey]incremental.InputRevision{}
	}
	observation := incremental.InputRevision{Key: input.Key, Revision: input.Revision, Found: input.Found}
	if previous, exists := r.rootResourceProofs[input.Key]; exists {
		if previous != observation {
			return incremental.ErrRevisionConflict
		}
		return nil
	}
	r.rootResourceProofs[input.Key] = observation
	return nil
}

func (*incrementalPinnedResourceView) NormalizeLookupKeys(_ string, keys []any) ([]string, error) {
	return templating.CanonicalIncrementalResourceKeys(keys...)
}

func (v *incrementalPinnedResourceView) Supports(resourceType string) bool {
	_, supported := v.session.renderSnapshots[resourceType]
	return supported
}

func (v *incrementalPinnedResourceView) List(resourceType string, _ stores.Store) ([]any, error) {
	snapshot := v.session.renderSnapshots[resourceType]
	if snapshot == nil {
		return nil, stores.ErrSnapshotUnsupported
	}
	items, err := readIncrementalSnapshotList(v.session.contextForReads(), snapshot)
	if err != nil {
		return nil, err
	}
	spec := resourceInputSpec{resourceType: resourceType, scope: resourceInputList}
	input, err := encodeResourceSnapshotInput(snapshot, &spec, items, true)
	if err != nil {
		return nil, err
	}
	if err := v.session.recordPinnedResourceInput(input); err != nil {
		return nil, err
	}
	return items, nil
}

func (v *incrementalPinnedResourceView) Get(
	resourceType string,
	_ stores.Store,
	keys ...string,
) ([]any, error) {
	snapshot := v.session.renderSnapshots[resourceType]
	if snapshot == nil {
		return nil, stores.ErrSnapshotUnsupported
	}
	items, err := readIncrementalSnapshotGet(v.session.contextForReads(), snapshot, keys...)
	if err != nil {
		return nil, err
	}
	spec := resourceInputSpec{resourceType: resourceType, scope: resourceInputGet, keys: slices.Clone(keys)}
	input, err := encodeResourceSnapshotInput(snapshot, &spec, items, len(items) > 0)
	if err != nil {
		return nil, err
	}
	if err := v.session.recordPinnedResourceInput(input); err != nil {
		return nil, err
	}
	return items, nil
}

func (v *incrementalPinnedResourceView) ListContext(
	ctx context.Context,
	resourceType string,
	_ stores.Store,
) ([]any, error) {
	return v.readContext(ctx, &resourceInputSpec{resourceType: resourceType, scope: resourceInputList})
}

func (v *incrementalPinnedResourceView) GetContext(
	ctx context.Context,
	resourceType string,
	_ stores.Store,
	keys ...string,
) ([]any, error) {
	return v.readContext(ctx, &resourceInputSpec{
		resourceType: resourceType,
		scope:        resourceInputGet,
		keys:         slices.Clone(keys),
	})
}

func (v *incrementalPinnedResourceView) readContext(
	ctx context.Context,
	spec *resourceInputSpec,
) ([]any, error) {
	snapshot := v.session.renderSnapshots[spec.resourceType]
	if snapshot == nil {
		return nil, stores.ErrSnapshotUnsupported
	}
	input, err := readResourceSnapshotInput(ctx, snapshot, spec)
	if err != nil {
		return nil, err
	}
	if err := v.session.recordPinnedResourceInput(input); err != nil {
		return nil, err
	}
	if spec.scope == resourceInputList {
		return readIncrementalSnapshotList(ctx, snapshot)
	}
	return readIncrementalSnapshotGet(ctx, snapshot, spec.keys...)
}

type incrementalHTTPFetcher struct {
	session *incrementalRenderSession
	reader  incremental.Reader
	lease   *incrementalBatchReaderLease
	mu      sync.Mutex
	effects map[uint64]incrementalHTTPEffect
}

func (f *incrementalHTTPFetcher) Pending(url string) bool {
	return f.session.httpWrapper.CandidateWithheld(url)
}

func (f *incrementalHTTPFetcher) Fetch(args ...any) (any, error) {
	release, err := beginIncrementalCapability(f.lease, "http.Fetch")
	if err != nil {
		return nil, err
	}
	defer release()
	canonical, err := templating.CanonicalIncrementalHTTPArgs(args...)
	if err != nil {
		return nil, err
	}
	content, snapshot, err := f.session.httpWrapper.FetchSnapshot(canonical...)
	if err != nil {
		f.session.disableCachePublication()
		return content, err
	}
	spec, key, err := f.session.httpInput(httpInputIdentity{
		url:        snapshot.URL,
		descriptor: snapshot.Descriptor,
	})
	if err != nil {
		return nil, err
	}
	cacheable := snapshot.Cacheable && snapshot.Token.Valid() && snapshot.Token.Kind() == httpstore.SnapshotAccepted
	revision := scratchHTTPRevision(&snapshot)
	if cacheable {
		revision = httpInputRevision(f.session.httpComponent.RevisionSource(), &snapshot)
	} else if snapshot.Token.Kind() == httpstore.SnapshotInitialCandidate ||
		f.session.httpWrapper.CandidateWithheld(snapshot.URL) {
		// A cold restart here made acceptance need a cold render with no input
		// change for its whole duration, which constant churn never grants (#276).
		f.session.withholdCacheForCandidate()
	} else {
		f.session.disableCachePublication()
		if !f.session.completeGraphRender() {
			return nil, errIncrementalColdRestart
		}
	}
	observed := incremental.Input{
		Key:      key,
		Revision: revision,
		Found:    snapshot.Found,
		Value:    []byte(snapshot.Content),
	}
	if cacheable {
		if err := f.session.observeHTTPInput(observed, &snapshot); err != nil {
			return nil, err
		}
		actual, err := f.reader.ExactInput(key)
		if err != nil {
			return nil, err
		}
		if actual.Revision != observed.Revision || actual.Found != observed.Found ||
			!bytes.Equal(actual.Value, observed.Value) {
			return nil, incremental.ErrRevisionConflict
		}
	}
	effect := incrementalHTTPEffect{inputID: spec.id, snapshot: snapshot}
	f.mu.Lock()
	defer f.mu.Unlock()
	if previous, exists := f.effects[spec.id]; exists && !sameHTTPSnapshot(&previous.snapshot, &snapshot) {
		return nil, fmt.Errorf("HTTP input %s changed within one incremental component", snapshot.URL)
	}
	f.effects[spec.id] = effect
	return snapshot.Content, nil
}

func (r *incrementalRenderSession) withholdCacheForCandidate() {
	r.mu.Lock()
	r.cacheHoldsCandidate = true
	r.mu.Unlock()
}

func (r *incrementalRenderSession) disableCachePublication() {
	r.mu.Lock()
	r.cachePublishable = false
	r.mu.Unlock()
}

func (r *incrementalRenderSession) setExactCycleHTTPLease(
	state *httpstore.AcceptedReplayState,
) error {
	if state == nil || state.ValidateAuthentication() != nil {
		return errors.New("exact cycle HTTP lease has invalid provenance")
	}
	if r.httpComponent == nil || state.Source() != r.httpComponent.RevisionSource() {
		return errors.New("exact cycle HTTP lease has no matching authority")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.exactCycleHTTPLease != nil && r.exactCycleHTTPLease != state {
		return errors.New("exact cycle HTTP lease was configured twice")
	}
	r.exactCycleHTTPLease = state
	return nil
}

func (r *incrementalRenderSession) setExactCycleHTTPPublishedLease(
	snapshots []httpstore.ContentSnapshot,
) error {
	if len(snapshots) == 0 {
		return errors.New("published exact cycle HTTP lease has no inputs")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.exactCycleHTTPLease != nil || len(r.exactCycleHTTPPublishedLease) != 0 {
		return errors.New("exact cycle HTTP lease was configured twice")
	}
	r.exactCycleHTTPPublishedLease = slices.Clone(snapshots)
	return nil
}

func (r *incrementalRenderSession) requiresColdRestart() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.cachePublishable && !r.fullCold && !r.cold
}

func (r *incrementalRenderSession) usePinnedColdRenderer() {
	r.mu.Lock()
	graphSession := r.graphSession
	r.graphSession = nil
	r.cachePublishable = false
	r.fullCold = true
	r.cursors = map[string]incrementalStoreCursor{}
	r.membershipPins = map[string]incrementalStoreCursor{}
	r.resourceProofs = map[incremental.InputKey]incremental.Input{}
	r.rootResourceProofs = map[incremental.InputKey]incremental.InputRevision{}
	r.exactCycleRootCalls = map[string][]exactCycleIncrementalObservation{}
	r.exactCycleRootReplay = false
	r.resetCatalog(nil)
	r.mu.Unlock()
	r.httpMu.Lock()
	r.httpObserved = map[incremental.InputKey]incremental.Input{}
	r.httpProofs = map[incremental.InputKey]httpstore.ObservationToken{}
	r.httpMu.Unlock()
	if graphSession != nil {
		graphSession.Abort()
	}
}

func (r *incrementalRenderSession) completeGraphRender() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cold || r.fullCold
}

func (r *incrementalRenderSession) provisionalHTTPAffectsReplayedOutput(urls []string) bool {
	if len(urls) == 0 || r.completeGraphRender() {
		return false
	}
	if r.httpLease == nil {
		return true
	}
	for _, url := range urls {
		if r.httpLease.ContainsURL(url) {
			return true
		}
	}
	return false
}

func (f *incrementalHTTPFetcher) result() []incrementalHTTPEffect {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]uint64, 0, len(f.effects))
	for id := range f.effects {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	result := make([]incrementalHTTPEffect, 0, len(ids))
	for _, id := range ids {
		result = append(result, f.effects[id])
	}
	return result
}

func (v *incrementalResourceView) List(resourceType string, _ stores.Store) ([]any, error) {
	return v.read(&resourceInputSpec{resourceType: resourceType, scope: resourceInputList})
}

func (v *incrementalResourceView) Get(
	resourceType string,
	_ stores.Store,
	keys ...string,
) ([]any, error) {
	return v.read(&resourceInputSpec{resourceType: resourceType, scope: resourceInputGet, keys: slices.Clone(keys)})
}

func (v *incrementalResourceView) read(spec *resourceInputSpec) ([]any, error) {
	ctx, release, err := v.BeginStoreInvocation(v.ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return v.readContext(ctx, spec)
}

func (v *incrementalResourceView) readContext(
	ctx context.Context,
	spec *resourceInputSpec,
) ([]any, error) {
	if v.session != nil {
		items, certificate, err := v.session.decodeResourceInput(v.reader, spec)
		if err != nil {
			return nil, err
		}
		if err := templating.RegisterIncrementalImmutableCertificate(ctx, certificate); err != nil {
			return nil, err
		}
		return items, nil
	}
	input, err := v.reader.ExactInput(resourceInputKey(spec))
	if err != nil {
		return []any{}, err
	}
	if !input.Found {
		return []any{}, nil
	}
	decoded, err := decodeResourceValue(input.Value)
	if err != nil {
		return nil, fmt.Errorf("decoding incremental resource %q input: %w", spec.resourceType, err)
	}
	result, ok := decoded.([]any)
	if !ok {
		return nil, fmt.Errorf("decoding incremental resource %q input: expected a list, got %T", spec.resourceType, decoded)
	}
	return result, nil
}

func (r *incrementalRenderSession) executeComponent(
	ctx context.Context,
	reader incremental.Reader,
	component *incrementalComponent,
	source string,
	namespace, name string,
) ([]byte, error) {
	if component.resourceProjection {
		return r.executeResourceProjection(ctx, reader, component, source, namespace, name)
	}
	prepared, immediate, err := r.prepareComponentInputs(ctx, reader, component, source, namespace, name)
	if err != nil || prepared == nil {
		return immediate, err
	}
	if err := r.prepareComponentRender(ctx, prepared, nil, nil); err != nil {
		return nil, err
	}
	text, err := r.state.engine.RenderIncrementalComponent(
		prepared.ctx,
		component.entryPoint,
		prepared.templateContext,
	)
	if err != nil {
		return nil, remapIncrementalTemplateError(component.name, component.entryPoint, err)
	}
	encoded, err := r.finishPreparedComponent(prepared, text)
	return []byte(encoded), err
}

// membershipQueryKey leaves an activation-gated query unregistered until its
// activation evaluates it: most members never activate (#284).
func (r *incrementalRenderSession) membershipQueryKey(
	component *incrementalComponent,
	source, namespace, name string,
) incremental.QueryKey {
	if len(component.activationPaths) > 0 {
		return componentQueryKey(component, source, namespace, name)
	}
	return r.registerComponentQuery(component, source, namespace, name)
}

func (r *incrementalRenderSession) registerComponentQuery(
	component *incrementalComponent,
	source, namespace, name string,
) incremental.QueryKey {
	if r.componentQueries == nil {
		r.componentQueries = queryidentity.NewAuthority(r)
	}
	key := componentQueryKey(component, source, namespace, name)
	if !r.componentQueries.Register(r, key, queryidentity.Fields{
		Component: component.name,
		Source:    source,
		Namespace: namespace,
		Name:      name,
	}) {
		return incremental.QueryKey{}
	}
	return key
}

// resolveQueryComponent answers only the component half of
// resolveComponentQuery, for callers that do not need the resource identity.
func (r *incrementalRenderSession) resolveQueryComponent(
	key incremental.QueryKey,
) (component incrementalComponent, resolved bool) {
	resolution := r.resolveQuery(key)
	return resolution.component, resolution.resolved
}

type resolvedComponentQuery struct {
	component incrementalComponent
	source    string
	namespace string
	name      string
	resolved  bool
}

func (r *incrementalRenderSession) resolveComponentQuery(
	key incremental.QueryKey,
) (component incrementalComponent, source, namespace, name string, resolved bool) {
	resolution := r.resolveQuery(key)
	return resolution.component, resolution.source, resolution.namespace, resolution.name, resolution.resolved
}

func (r *incrementalRenderSession) resolveQuery(key incremental.QueryKey) resolvedComponentQuery {
	component, source, namespace, name, resolved := r.resolveComponentQueryFields(key)
	return resolvedComponentQuery{
		component: component, source: source, namespace: namespace, name: name, resolved: resolved,
	}
}

func (r *incrementalRenderSession) resolveComponentQueryFields(
	key incremental.QueryKey,
) (component incrementalComponent, source, namespace, name string, resolved bool) {
	if identity, cached := r.componentQueries.Lookup(r, key); cached {
		component, exists := r.state.components[identity.Component]
		if !exists || component.name != identity.Component {
			return incrementalComponent{}, "", "", "", false
		}
		return component, identity.Source, identity.Namespace, identity.Name, true
	}
	componentName, source, namespace, name, parsed := parseComponentQueryKey(key)
	if !parsed {
		return incrementalComponent{}, "", "", "", false
	}
	component, exists := r.state.components[componentName]
	if !exists || componentQueryKey(&component, source, namespace, name) != key {
		return incrementalComponent{}, "", "", "", false
	}
	if r.componentQueries == nil {
		r.componentQueries = queryidentity.NewAuthority(r)
	}
	if !r.componentQueries.Register(r, key, queryidentity.Fields{
		Component: componentName,
		Source:    source,
		Namespace: namespace,
		Name:      name,
	}) {
		return incrementalComponent{}, "", "", "", false
	}
	return component, source, namespace, name, true
}

type preparedIncrementalComponent struct {
	queryKey           incremental.QueryKey
	component          *incrementalComponent
	reader             incremental.Reader
	source             string
	namespace          string
	name               string
	itemBytes          []byte
	item               map[string]any
	itemCertificate    *templating.IncrementalImmutableCertificate
	props              map[string]any
	propsCertificate   *templating.IncrementalImmutableCertificate
	renderSubject      map[string]any
	subjectCertificate *templating.IncrementalImmutableCertificate
	ctx                context.Context
	templateContext    map[string]any
	recorder           *incrementalRecorder
	httpFetcher        *incrementalHTTPFetcher
	lease              *incrementalBatchReaderLease
	activate           func() error
	deactivate         func()
}

func (r *incrementalRenderSession) prepareComponentInputs(
	ctx context.Context,
	reader incremental.Reader,
	component *incrementalComponent,
	source string,
	namespace, name string,
) (*preparedIncrementalComponent, []byte, error) {
	prepared, immediate, executed, err := r.prepareComponentInputsDetached(
		ctx,
		reader,
		component,
		source,
		namespace,
		name,
	)
	if executed {
		r.httpExecuted[componentQueryKey(component, source, namespace, name)] = nil
	}
	return prepared, immediate, err
}

func (r *incrementalRenderSession) prepareComponentInputsDetached(
	ctx context.Context,
	reader incremental.Reader,
	component *incrementalComponent,
	source string,
	namespace, name string,
) (prepared *preparedIncrementalComponent, retiredEncoded []byte, retired bool, err error) {
	return r.prepareComponentInputsDetachedWithSourceFrames(
		ctx,
		reader,
		component,
		source,
		namespace,
		name,
		nil,
	)
}

func (r *incrementalRenderSession) loadComponentBinding(
	ctx context.Context,
	reader incremental.Reader,
	component *incrementalComponent,
	source string,
	frames *incrementalColdSourceFrameRefs,
	sourceFrame incrementalColdSourceFrameView,
) (
	props map[string]any,
	certificate *templating.IncrementalImmutableCertificate,
	found bool,
	err error,
) {
	if frames == nil {
		props, _, certificate, found, err = r.decodeComponentInputWithEncoding(
			reader, bindingInputKey(component.name, source), component.name, incrementalPropsContextName, false,
		)
		return props, certificate, found, err
	}
	binding, loadErr := sourceFrame.binding.load(ctx, reader, sourceFrame.generation)
	if loadErr != nil {
		return nil, nil, false, loadErr
	}
	return binding.value, binding.certificate, binding.found, nil
}

func (r *incrementalRenderSession) loadComponentSourceItem(
	ctx context.Context,
	reader incremental.Reader,
	component *incrementalComponent,
	source, namespace, name string,
	frames *incrementalColdSourceFrameRefs,
	sourceFrame incrementalColdSourceFrameView,
) (
	item map[string]any,
	itemBytes []byte,
	certificate *templating.IncrementalImmutableCertificate,
	found bool,
	err error,
) {
	if frames == nil {
		item, itemBytes, certificate, found, err = r.decodeComponentInputWithEncoding(
			reader, resourceInputKey(&resourceInputSpec{
				resourceType: source,
				scope:        resourceInputIdentity,
				namespace:    namespace,
				name:         name,
			}), component.name, incrementalSourceContextName, component.deriveResource)
		return item, itemBytes, certificate, found, err
	}
	sourceInput, loadErr := sourceFrame.item.load(ctx, reader, sourceFrame.generation)
	if loadErr != nil {
		return nil, nil, nil, false, loadErr
	}
	if component.deriveResource {
		itemBytes = []byte(sourceInput.encoded)
	}
	return sourceInput.value, itemBytes, sourceInput.certificate, sourceInput.found, nil
}

func (r *incrementalRenderSession) loadComponentRenderSubject(
	ctx context.Context,
	reader incremental.Reader,
	component *incrementalComponent,
	source, namespace, name string,
	frames *incrementalColdSourceFrameRefs,
	sourceFrame incrementalColdSourceFrameView,
) (
	renderSubject map[string]any,
	certificate *templating.IncrementalImmutableCertificate,
	found bool,
	err error,
) {
	if frames == nil {
		renderSubject, _, certificate, found, err = r.decodeComponentInputWithEncoding(
			reader,
			renderSubjectInputKey(source, namespace, name),
			component.name,
			"render subject",
			false,
		)
		return renderSubject, certificate, found, err
	}
	subject, loadErr := sourceFrame.renderSubject.load(ctx, reader, sourceFrame.generation)
	if loadErr != nil {
		return nil, nil, false, loadErr
	}
	return subject.value, subject.certificate, subject.found, nil
}

func (r *incrementalRenderSession) prepareComponentInputsDetachedWithSourceFrames(
	ctx context.Context,
	reader incremental.Reader,
	component *incrementalComponent,
	source string,
	namespace, name string,
	frames *incrementalColdSourceFrameRefs,
) (prepared *preparedIncrementalComponent, retiredEncoded []byte, retired bool, err error) {
	queryKey := r.registerComponentQuery(component, source, namespace, name)
	var sourceFrame incrementalColdSourceFrameView
	if frames != nil {
		var err error
		sourceFrame, err = frames.authenticateDetached(
			queryKey,
			component,
			source,
			namespace,
			name,
		)
		if err != nil {
			return nil, nil, false, err
		}
	}
	props, propsCertificate, bindingFound, err := r.loadComponentBinding(
		ctx, reader, component, source, frames, sourceFrame,
	)
	if err != nil {
		return nil, nil, false, err
	}
	if !bindingFound {
		if _, retired := r.retired.Get([]byte(queryKey.Opaque())); !retired {
			return nil, nil, false, fmt.Errorf("incremental component %q binding %q disappeared", component.name, source)
		}
		encoded, encodeErr := json.Marshal(incrementalComponentResult{})
		return nil, encoded, true, encodeErr
	}
	item, itemBytes, itemCertificate, found, err := r.loadComponentSourceItem(
		ctx, reader, component, source, namespace, name, frames, sourceFrame,
	)
	if err != nil {
		return nil, nil, false, err
	}
	if !found {
		encoded, encodeErr := json.Marshal(incrementalComponentResult{})
		return nil, encoded, true, encodeErr
	}
	projectedItem, projectedBytes, projected, err := r.projectComponentItem(
		ctx, reader, component, source, item,
	)
	if err != nil {
		return nil, nil, false, fmt.Errorf("projecting incremental component %q item: %w", component.name, err)
	}
	item, itemCertificate, err = r.authenticateComponentProjection(
		component.name,
		projectedItem,
		projectedBytes,
		itemCertificate,
		projected,
	)
	if err != nil {
		return nil, nil, false, err
	}
	renderSubject, subjectCertificate, found, err := r.loadComponentRenderSubject(
		ctx, reader, component, source, namespace, name, frames, sourceFrame,
	)
	if err != nil {
		return nil, nil, false, err
	}
	if !found {
		return nil, nil, false, fmt.Errorf("incremental component %q render subject disappeared", component.name)
	}
	return &preparedIncrementalComponent{
		queryKey:           queryKey,
		component:          component,
		reader:             reader,
		source:             source,
		namespace:          namespace,
		name:               name,
		itemBytes:          itemBytes,
		item:               item,
		itemCertificate:    itemCertificate,
		props:              props,
		propsCertificate:   propsCertificate,
		renderSubject:      renderSubject,
		subjectCertificate: subjectCertificate,
	}, nil, false, nil
}

type incrementalBatchCapabilities struct {
	ctx       context.Context
	resources any
	view      *incrementalBatchResourceView
}

// prepareBatchCapabilities binds the batch's context to the session's shared
// resources value. The value is built once per session: it reads through a
// view of the session and the session's read context, neither of which a
// batch changes, and building it costs a reflective walk over every watched
// resource.
func (r *incrementalRenderSession) prepareBatchCapabilities(
	ctx context.Context,
	authority *incrementalCapabilityAuthority,
) *incrementalBatchCapabilities {
	batchCtx := templating.WithIncrementalImmutableInputs(
		templating.WithImmutableResourceInputs(ctx),
	)
	r.batchResourcesMu.Lock()
	defer r.batchResourcesMu.Unlock()
	if r.batchResources == nil || r.batchResourceView.authority != authority {
		view := &incrementalBatchResourceView{session: r, authority: authority}
		view.seal = view
		readContext := r.readContext
		if readContext == nil {
			readContext = ctx
		}
		r.batchResourceView = view
		r.batchResources = r.state.incrementalResourcesValue(
			templating.WithIncrementalImmutableInputs(templating.WithImmutableResourceInputs(readContext)),
			r.stores,
			r.resourceErrors,
			view,
			nil,
			r.loggerContext,
		)
	}
	batchCtx = templating.WithIncrementalImmutableCapabilityInputs(batchCtx, r.batchResources)
	return &incrementalBatchCapabilities{
		ctx: batchCtx, resources: r.batchResources, view: r.batchResourceView,
	}
}

func (r *incrementalRenderSession) prepareComponentRender(
	ctx context.Context,
	prepared *preparedIncrementalComponent,
	batch *incrementalBatchCapabilities,
	authority *incrementalCapabilityAuthority,
) error {
	component := prepared.component
	recorder, err := r.newComponentRecorder(prepared)
	if err != nil {
		return err
	}
	preflightRecorder := &incrementalPreflightRecorder{recorder: recorder}
	httpFetcher := &incrementalHTTPFetcher{
		session: r,
		reader:  prepared.reader,
		effects: map[uint64]incrementalHTTPEffect{},
	}
	componentCtx, err := r.componentRenderContext(ctx, prepared, batch, recorder, preflightRecorder)
	if err != nil {
		return err
	}
	var lease *incrementalBatchReaderLease
	if authority != nil {
		lease, componentCtx, err = authority.newLease(componentCtx, prepared.reader, r)
		if err != nil {
			return fmt.Errorf("preparing incremental component %q capability lease: %w", component.name, err)
		}
		recorder.lease = lease
		httpFetcher.lease = lease
		if recorder.plan != nil {
			recorder.plan.lease = lease
		}
		if recorder.deriver != nil {
			recorder.deriver.lease = lease
		}
		prepared.lease = lease
		prepared.activate = lease.activate
		prepared.deactivate = lease.revoke
	} else if batch != nil {
		return fmt.Errorf("incremental component %q batch has no capability authority", component.name)
	}
	var resources any
	var controller map[string]templating.ResourceStore
	componentCtx, resources, controller, err = r.bindComponentCapabilities(
		componentCtx, prepared, batch, recorder, lease,
	)
	if err != nil {
		return err
	}
	selector := &incrementalPublicationSelector{
		ctx: componentCtx, reader: prepared.reader, session: r, component: component, lease: lease,
	}
	componentContext := map[string]any{
		incrementalSourceContextName:        prepared.source,
		incrementalItemContextName:          prepared.item,
		incrementalPropsContextName:         prepared.props,
		incrementalRenderSubjectContextName: prepared.renderSubject,
		incrementalSharedContextName: templating.NewLeasedSharedContributionContext(
			componentCtx,
			preflightRecorder,
			&incrementalPreflightPublicationSelector{selector: selector},
		),
		incrementalResourcesContextName:  resources,
		incrementalControllerContextName: controller,
	}
	if r.httpWrapper != nil {
		componentContext[incrementalHTTPContextName] = httpFetcher
	}
	if recorder.plan != nil {
		componentContext[incrementalPlanRegistryContextName] = recorder.plan
	}
	if err := templating.BindIncrementalImmutableInputs(componentContext, componentCtx); err != nil {
		return fmt.Errorf("binding incremental component %q immutable inputs: %w", component.name, err)
	}
	prepared.ctx = componentCtx
	prepared.templateContext = componentContext
	prepared.recorder = recorder
	prepared.httpFetcher = httpFetcher
	return nil
}

func (r *incrementalRenderSession) newComponentRecorder(
	prepared *preparedIncrementalComponent,
) (*incrementalRecorder, error) {
	component := prepared.component
	recorder := &incrementalRecorder{
		publicationGeneration: r.publicationGeneration,
		publicationGroup:      component.group,
		publicationOwner: incrementalGroupInstanceID{
			component: component.name,
			source:    prepared.source,
			namespace: prepared.namespace,
			name:      prepared.name,
		},
	}
	if component.backendPlan {
		recorder.plan = newIncrementalBackendPlanRecorder()
	}
	if component.deriveResource {
		deriver, err := newIncrementalResourceDeriver(
			prepared.source,
			prepared.namespace,
			prepared.name,
			prepared.itemBytes,
		)
		if err != nil {
			return nil, fmt.Errorf("preparing incremental component %q derivation: %w", component.name, err)
		}
		recorder.deriver = deriver
	}
	return recorder, nil
}

func (r *incrementalRenderSession) componentRenderContext(
	ctx context.Context,
	prepared *preparedIncrementalComponent,
	batch *incrementalBatchCapabilities,
	recorder *incrementalRecorder,
	preflightRecorder *incrementalPreflightRecorder,
) (context.Context, error) {
	component := prepared.component
	componentBaseCtx := ctx
	if batch == nil {
		componentBaseCtx = templating.WithImmutableResourceInputs(ctx)
	}
	componentCtx := templating.WithIncrementalImmutableCertificates(
		componentBaseCtx,
		prepared.itemCertificate,
		prepared.propsCertificate,
		prepared.subjectCertificate,
	)
	if component.deriveResource {
		componentCtx = templating.WithIncrementalResourceDeriver(componentCtx, &incrementalPreflightResourceDeriver{
			deriver: recorder.deriver,
		})
	}
	if component.recordEvent {
		componentCtx = templating.WithIncrementalEventRecorder(componentCtx, preflightRecorder)
	}
	if component.statusPatch {
		transitionTime, transitionErr := r.incrementalTransitionTime(ctx)
		if transitionErr != nil {
			return nil, fmt.Errorf("sampling incremental transition time: %w", transitionErr)
		}
		componentCtx = templating.WithIncrementalStatusPatchRecorder(componentCtx, preflightRecorder)
		componentCtx = templating.WithIncrementalTransitionTime(componentCtx, transitionTime)
	}
	return componentCtx, nil
}

func (r *incrementalRenderSession) bindComponentCapabilities(
	componentCtx context.Context,
	prepared *preparedIncrementalComponent,
	batch *incrementalBatchCapabilities,
	recorder *incrementalRecorder,
	lease *incrementalBatchReaderLease,
) (
	boundCtx context.Context,
	resources any,
	controller map[string]templating.ResourceStore,
	err error,
) {
	component := prepared.component
	if batch == nil {
		derived := r.incrementalDerivedResources(componentCtx, prepared.reader)
		if recorder.deriver != nil {
			derived = recorder.deriver.view
		}
		resourceView := &incrementalResourceView{
			ctx: componentCtx, reader: prepared.reader, session: r, lease: lease,
		}
		resources = r.state.incrementalResourcesValue(
			componentCtx,
			r.stores,
			r.resourceErrors,
			resourceView,
			derived,
			r.loggerContext,
		)
		controller = r.incrementalControllerValue(componentCtx, resourceView, false)
		componentCtx = templating.WithIncrementalImmutableCapabilityInputs(componentCtx, resources, controller)
		return componentCtx, resources, controller, nil
	}
	if component.deriveResource {
		return nil, nil, nil, fmt.Errorf(
			"incremental component %q cannot share a derived-resource capability", component.name,
		)
	}
	if binder, available := r.state.engine.(templating.IncrementalResourceBinder); available {
		resources, err = binder.BindIncrementalResources(component.entryPoint, batch.resources, lease)
	} else {
		resources, err = templating.BindAllIncrementalResources(batch.resources, lease)
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf(
			"binding incremental component %q resource capability: %w", component.name, err,
		)
	}
	controller = r.incrementalControllerValue(componentCtx, batch.view, false)
	for _, store := range controller {
		if wrapper, ok := store.(*rendercontext.StoreWrapper); ok && wrapper != nil {
			wrapper.DerivedView = nil
		}
	}
	componentCtx = templating.WithIncrementalImmutableCapabilityInputs(componentCtx, resources, controller)
	return componentCtx, resources, controller, nil
}

type finalizedIncrementalComponent struct {
	key         incremental.QueryKey
	encoded     string
	fresh       *authenticatedFreshComponentResult
	httpEffects []incrementalHTTPEffect
}

func (r *incrementalRenderSession) finalizePreparedComponent(
	prepared *preparedIncrementalComponent,
	text string,
) (*finalizedIncrementalComponent, error) {
	if err := prepared.lease.publicationError(); err != nil {
		return nil, fmt.Errorf("incremental component %q capability lease: %w", prepared.component.name, err)
	}
	encoded, fresh, err := prepared.recorder.authenticatedResult(
		prepared.queryKey,
		prepared.component,
		prepared.source,
		prepared.namespace,
		prepared.name,
		text,
	)
	if err != nil {
		return nil, fmt.Errorf("incremental component %q result: %w", prepared.component.name, err)
	}
	return &finalizedIncrementalComponent{
		key:         prepared.queryKey,
		encoded:     encoded,
		fresh:       fresh,
		httpEffects: prepared.httpFetcher.result(),
	}, nil
}

func (r *incrementalRenderSession) finalizePreparedComponentIntoArena(
	prepared *preparedIncrementalComponent,
	text string,
	arena *incrementalColdResultArena,
	slot int,
) error {
	if prepared == nil || prepared.component == nil || prepared.recorder == nil ||
		prepared.httpFetcher == nil || arena == nil {
		return errors.New("incremental component arena finalization is incomplete")
	}
	if err := prepared.lease.publicationError(); err != nil {
		return fmt.Errorf("incremental component %q capability lease: %w", prepared.component.name, err)
	}
	result, effects, err := prepared.recorder.validatedResult(
		prepared.component,
		prepared.source,
		prepared.namespace,
		prepared.name,
		text,
	)
	if err != nil {
		return fmt.Errorf("incremental component %q result: %w", prepared.component.name, err)
	}
	_, err = arena.initialize(
		slot,
		prepared.queryKey,
		&result,
		effects,
		prepared.httpFetcher.result(),
	)
	if err != nil {
		return fmt.Errorf("incremental component %q arena result: %w", prepared.component.name, err)
	}
	return nil
}

func (r *incrementalRenderSession) installColdResultArena(arena *incrementalColdResultArena) error {
	if r == nil || r.httpExecuted == nil || r.freshResults == nil || arena == nil {
		return errors.New("incremental cold result arena destination is unavailable")
	}
	if err := arena.validateAuthority(); err != nil {
		return err
	}
	for index := range arena.fresh {
		key := arena.keys[index]
		fresh := &arena.fresh[index]
		if err := validatePendingAuthenticatedFreshComponentResult(fresh, key); err != nil {
			return fmt.Errorf("incremental cold result arena has invalid provenance: %w", err)
		}
		if _, exists := r.freshResults[key]; exists {
			return errors.New("incremental cold result arena query was already installed")
		}
		if _, exists := r.httpExecuted[key]; exists {
			return errors.New("incremental cold result arena HTTP effects were already installed")
		}
	}
	httpEffects, err := arena.takeHTTPEffectsMany()
	if err != nil {
		return fmt.Errorf("incremental cold result arena has invalid HTTP provenance: %w", err)
	}
	for index := range arena.fresh {
		key := arena.keys[index]
		r.freshResults[key] = &arena.fresh[index]
		r.httpExecuted[key] = httpEffects[index]
	}
	return nil
}

func (r *incrementalRenderSession) installFinalizedComponents(
	finalized ...*finalizedIncrementalComponent,
) error {
	if r == nil || r.httpExecuted == nil || r.freshResults == nil {
		return errors.New("finalized incremental component destination is unavailable")
	}
	keys := make(map[incremental.QueryKey]struct{}, len(finalized))
	httpEffects := make([][]incrementalHTTPEffect, len(finalized))
	for itemIndex, item := range finalized {
		if item == nil {
			return errors.New("finalized incremental component has invalid provenance")
		}
		if item.fresh == nil || item.fresh.encoded != item.encoded {
			return errors.New("finalized incremental component has invalid provenance")
		}
		if err := validatePendingAuthenticatedFreshComponentResult(item.fresh, item.key); err != nil {
			return fmt.Errorf("finalized incremental component has invalid provenance: %w", err)
		}
		if _, duplicate := keys[item.key]; duplicate {
			return errors.New("finalized incremental component set has a duplicate query")
		}
		if _, exists := r.freshResults[item.key]; exists {
			return errors.New("finalized incremental component query was already installed")
		}
		if _, exists := r.httpExecuted[item.key]; exists {
			return errors.New("finalized incremental component HTTP effects were already installed")
		}
		if item.fresh.arena != nil {
			return errors.New("finalized incremental component requires arena installation")
		}
		httpEffects[itemIndex] = item.httpEffects
		keys[item.key] = struct{}{}
	}
	for itemIndex, item := range finalized {
		r.freshResults[item.key] = item.fresh
		r.httpExecuted[item.key] = httpEffects[itemIndex]
	}
	return nil
}

func (r *incrementalRenderSession) finishPreparedComponent(
	prepared *preparedIncrementalComponent,
	text string,
) (string, error) {
	finalized, err := r.finalizePreparedComponent(prepared, text)
	if err != nil {
		return "", err
	}
	if err := r.installFinalizedComponents(finalized); err != nil {
		return "", err
	}
	return finalized.encoded, nil
}

type incrementalEntryPointBatch struct {
	entryPoint string
	indexes    []int
	prepared   []*preparedIncrementalComponent
}

func (r *incrementalRenderSession) executeComponentBatch(
	ctx context.Context,
	queries []incremental.BatchQuery,
) ([]incremental.ExactBatchValue, error) {
	executor, ok := r.state.engine.(templating.IncrementalComponentBatchExecutor)
	if !ok {
		return nil, errors.New("template engine has no incremental component batch executor")
	}
	values := make([]incremental.ExactBatchValue, len(queries))
	r.batchResourcesMu.Lock()
	if r.capabilityAuthority == nil {
		r.capabilityAuthority = newIncrementalCapabilityAuthority(r.resourceErrors)
	}
	authority := r.capabilityAuthority
	r.batchResourcesMu.Unlock()
	batches, err := r.groupComponentBatchQueries(ctx, queries, values)
	if err != nil {
		return nil, err
	}
	var sharedCapabilities *incrementalBatchCapabilities
	for batchIndex := range batches {
		batch := &batches[batchIndex]
		var capabilities *incrementalBatchCapabilities
		if len(batch.prepared) > 0 && !batch.prepared[0].component.deriveResource {
			if sharedCapabilities == nil {
				sharedCapabilities = r.prepareBatchCapabilities(ctx, authority)
			}
			capabilities = sharedCapabilities
		}
		if err := r.runComponentEntryPointBatch(
			ctx, executor, queries, values, batch, capabilities, authority,
		); err != nil {
			return nil, err
		}
	}
	return values, nil
}

func (r *incrementalRenderSession) groupComponentBatchQueries(
	ctx context.Context,
	queries []incremental.BatchQuery,
	values []incremental.ExactBatchValue,
) ([]incrementalEntryPointBatch, error) {
	batches := []incrementalEntryPointBatch{}
	batchByEntryPoint := map[string]int{}
	for index := range queries {
		component, source, namespace, name, parsed := r.resolveComponentQuery(queries[index].Key)
		if !parsed {
			return nil, fmt.Errorf(
				"incremental component batch received non-component query %s", queries[index].Key,
			)
		}
		if component.resourceProjection {
			encoded, err := r.executeResourceProjection(
				ctx,
				queries[index].Reader,
				&component,
				source,
				namespace,
				name,
			)
			if err != nil {
				return nil, attributeToWatchedObject(queries[index].Key, err)
			}
			values[index].Value, values[index].Err = queries[index].NewExactValue(string(encoded))
			continue
		}
		prepared, immediate, err := r.prepareComponentInputs(
			ctx,
			queries[index].Reader,
			&component,
			source,
			namespace,
			name,
		)
		if err != nil {
			return nil, attributeToWatchedObject(queries[index].Key, err)
		}
		if prepared == nil {
			values[index].Value, values[index].Err = queries[index].NewExactValue(string(immediate))
			continue
		}
		batchIndex, exists := batchByEntryPoint[component.entryPoint]
		if !exists {
			batchIndex = len(batches)
			batchByEntryPoint[component.entryPoint] = batchIndex
			batches = append(batches, incrementalEntryPointBatch{entryPoint: component.entryPoint})
		}
		batches[batchIndex].indexes = append(batches[batchIndex].indexes, index)
		batches[batchIndex].prepared = append(batches[batchIndex].prepared, prepared)
	}
	return batches, nil
}

func (r *incrementalRenderSession) runComponentEntryPointBatch(
	ctx context.Context,
	executor templating.IncrementalComponentBatchExecutor,
	queries []incremental.BatchQuery,
	values []incremental.ExactBatchValue,
	batch *incrementalEntryPointBatch,
	capabilities *incrementalBatchCapabilities,
	authority *incrementalCapabilityAuthority,
) error {
	items := make([]templating.IncrementalComponentBatchItem, len(batch.prepared))
	for index := range batch.prepared {
		prepareCtx := ctx
		if capabilities != nil {
			prepareCtx = capabilities.ctx
		}
		if err := r.prepareComponentRender(prepareCtx, batch.prepared[index], capabilities, authority); err != nil {
			queryIndex := batch.indexes[index]
			return attributeToWatchedObject(queries[queryIndex].Key, err)
		}
		items[index] = templating.IncrementalComponentBatchItem{
			Context:         batch.prepared[index].ctx,
			TemplateContext: batch.prepared[index].templateContext,
			Activate:        batch.prepared[index].activate,
			Deactivate:      batch.prepared[index].deactivate,
		}
	}
	texts, err := executor.RenderIncrementalComponents(ctx, batch.entryPoint, items)
	if err != nil {
		var itemErr *templating.IncrementalComponentBatchError
		if errors.As(err, &itemErr) && itemErr.Index >= 0 && itemErr.Index < len(batch.indexes) {
			queryIndex := batch.indexes[itemErr.Index]
			component := batch.prepared[itemErr.Index].component
			templateErr := remapIncrementalTemplateError(component.name, component.entryPoint, itemErr.Err)
			return attributeToWatchedObject(queries[queryIndex].Key, templateErr)
		}
		return err
	}
	if len(texts) != len(batch.prepared) {
		return fmt.Errorf(
			"incremental component batch returned %d outputs for %d items",
			len(texts),
			len(batch.prepared),
		)
	}
	for index := range batch.prepared {
		encoded, err := r.finishPreparedComponent(batch.prepared[index], texts[index])
		if err != nil {
			queryIndex := batch.indexes[index]
			return attributeToWatchedObject(queries[queryIndex].Key, err)
		}
		queryIndex := batch.indexes[index]
		values[queryIndex].Value, values[queryIndex].Err = queries[queryIndex].NewExactValue(encoded)
	}
	return nil
}

func (r *incrementalRenderSession) bindFreshComponentRoot(
	key incremental.QueryKey,
	root incremental.ExactValueRoot,
) error {
	if err := r.state.graph.ValidateExactValue(key, root); err != nil {
		return err
	}
	fresh := r.freshResults[key]
	if fresh == nil {
		return nil
	}
	return bindAuthenticatedFreshComponentResult(fresh, key, root)
}

func (r *incrementalRenderSession) evaluateComponentQueries(
	ctx context.Context,
	keys []incremental.QueryKey,
) ([]incremental.ExactResult, error) {
	var (
		results []incremental.ExactResult
		err     error
	)
	var vectorRenderer templating.IncrementalComponentVectorRenderer
	vectorEligible := false
	if r.cold && !r.coldVectorDisabled {
		var preflightErr error
		vectorRenderer, vectorEligible, preflightErr = r.preflightColdComponentVector(keys)
		if preflightErr != nil {
			return nil, preflightErr
		}
	}
	if r.cold && vectorEligible {
		results, err = r.evaluateColdComponentVector(ctx, vectorRenderer, keys)
	} else {
		if _, available := r.state.engine.(templating.IncrementalComponentBatchExecutor); !available {
			return nil, errors.New("template engine has no incremental component batch executor")
		}
		for _, key := range keys {
			_, ok := r.resolveQueryComponent(key)
			if !ok {
				return nil, fmt.Errorf("incremental component evaluation received non-component query %s", key)
			}
		}
		results, err = r.graphSession.EvaluateAllExactBatch(ctx, r.executeComponentBatch, keys...)
	}
	if err != nil {
		return nil, err
	}
	for index := range results {
		if err := r.bindFreshComponentRoot(results[index].Key, results[index].Value); err != nil {
			return nil, err
		}
	}
	return results, nil
}

func decodeIncrementalComponentObject(component, label string, encoded []byte) (map[string]any, error) {
	decoded, err := decodeResourceValue(encoded)
	if err != nil {
		return nil, fmt.Errorf("decoding incremental component %q %s: %w", component, label, err)
	}
	result, ok := decoded.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("decoding incremental component %q %s: expected an object, got %T",
			component, label, decoded)
	}
	return result, nil
}

func (r *incrementalRenderSession) RenderIncremental(ctx context.Context, name string) (string, error) {
	fragment, err := r.RenderIncrementalTextFragment(ctx, name)
	if err != nil {
		return "", err
	}
	return materializeIncrementalTextFragment(fragment)
}

func (r *incrementalRenderSession) RenderIncrementalTextFragment(
	ctx context.Context,
	name string,
) (templating.TextFragment, error) {
	r.renderMu.Lock()
	defer r.renderMu.Unlock()

	component, ok := r.state.components[name]
	if !ok {
		return nil, fmt.Errorf("incremental component %q is not configured", name)
	}
	group := component.group
	scope, _ := templating.IncrementalScope(ctx)
	if err := validateIncrementalBackendPlanScope(&component, scope); err != nil {
		return nil, err
	}
	r.calls, r.scopedCalls = recordIncrementalCall(
		r.calls,
		r.scopedCalls,
		group,
		r.state.groups[group],
		incrementalCall{scope: scope, component: name},
	)
	if component.backendPlan && !r.planReady {
		if err := r.prepareBackendPlans(ctx); err != nil {
			return nil, err
		}
	}
	if r.requested[group] {
		fragment, err := r.incrementalOutputTextFragment(group, &component)
		if err != nil {
			return nil, err
		}
		if err := r.recordExactCycleIncrementalObservation(
			ctx, exactCycleIncrementalComponent, group, component.name, "", "", fragment,
		); err != nil {
			return nil, err
		}
		return fragment, nil
	}

	if err := r.evaluateGroup(ctx, group); err != nil {
		return nil, err
	}
	if err := r.refreshGroup(group); err != nil {
		return nil, err
	}
	r.requested[group] = true
	if err := r.replayGroupEvents(group); err != nil {
		return nil, err
	}
	fragment, err := r.incrementalOutputTextFragment(group, &component)
	if err != nil {
		return nil, err
	}
	if err := r.recordExactCycleIncrementalObservation(
		ctx, exactCycleIncrementalComponent, group, component.name, "", "", fragment,
	); err != nil {
		return nil, err
	}
	return fragment, nil
}

func (r *incrementalRenderSession) evaluateGroup(ctx context.Context, group string) error {
	scope, _ := templating.IncrementalScope(ctx)
	if err := r.requireGroupDependencies(group, scope); err != nil {
		return err
	}
	keys, err := r.queriesForGroup(group)
	if err != nil {
		return err
	}
	if len(keys) > 0 {
		runCtx := context.WithValue(ctx, incrementalRunContextKey{}, r)
		results, err := r.evaluateComponentQueries(runCtx, keys)
		if err != nil {
			return err
		}
		batched, err := r.applyColdGroupAdditions(group, results)
		if err != nil {
			return err
		}
		if !batched {
			for index := range results {
				if err := r.applyEvaluatedResult(group, &results[index]); err != nil {
					return err
				}
			}
		}
	}
	return r.applyIncrementalSelectorChanges(group)
}

type incrementalColdGroupAddition struct {
	evaluated *incremental.ExactResult
	target    evaluatedResultTarget
	fresh     *authenticatedFreshComponentResult
	effects   []incrementalHTTPEffect
}

type incrementalPreparedColdGroupAdditions struct {
	authority *incrementalPreparedColdGroupAdditionsAuthority
	seal      *incrementalPreparedColdGroupAdditions
}

type incrementalPreparedColdGroupAdditionsAuthority struct {
	install func(
		*incrementalPreparedColdGroupAdditions,
		*incrementalRenderSession,
	) (bool, error)
	seal *incrementalPreparedColdGroupAdditionsAuthority
}

func (r *incrementalRenderSession) applyColdGroupAdditions(
	group string,
	evaluated []incremental.ExactResult,
) (bool, error) {
	prepared, applicable, err := r.prepareColdGroupAdditions(group, evaluated)
	if err != nil || !applicable {
		return false, err
	}
	return r.installPreparedColdGroupAdditions(prepared)
}

func (r *incrementalRenderSession) prepareColdGroupAdditions(
	group string,
	evaluated []incremental.ExactResult,
) (*incrementalPreparedColdGroupAdditions, bool, error) {
	if len(evaluated) == 0 {
		return nil, false, nil
	}
	index, exists := r.groupIndexes[group]
	if !exists || index == nil {
		return nil, false, nil
	}
	if err := index.validateAuthentication(); err != nil {
		return nil, false, err
	}
	additions, eligible, err := r.collectColdGroupAdditions(group, slices.Clone(evaluated))
	if err != nil || !eligible {
		return nil, false, err
	}

	groupInstances, planAdditions := buildColdGroupPreparedInstances(additions)
	updated, ownedResults, err := index.addPreparedBatch(groupInstances)
	if err != nil {
		return nil, false, err
	}
	for additionIndex := range planAdditions {
		planAdditions[additionIndex].result = &ownedResults[additionIndex]
	}
	var planBatch *incrementalPreparedPlanColdBatch
	if r.preparedPlanBootstrapPending {
		if r.preparedPlanColdBuilder == nil {
			return nil, false, errors.New("incremental prepared plan cold builder is unavailable")
		}
		if r.preparedPlanColdBuilder.covers(group) {
			planBatch, err = r.preparedPlanColdBuilder.prepareValidatedGroupAdditions(
				group, updated, planAdditions,
			)
			if err != nil {
				return nil, false, err
			}
		}
	}
	prepared := &incrementalPreparedColdGroupAdditions{}
	authority := &incrementalPreparedColdGroupAdditionsAuthority{}
	authority.install = func(
		owner *incrementalPreparedColdGroupAdditions,
		session *incrementalRenderSession,
	) (bool, error) {
		if owner != prepared || owner.seal != owner || owner.authority != authority ||
			authority.seal != authority || session != r {
			return false, errors.New("prepared cold incremental group additions have invalid provenance")
		}
		return session.installColdGroupAdditions(
			group, index, updated, additions, planAdditions, planBatch,
		)
	}
	authority.seal = authority
	prepared.authority = authority
	prepared.seal = prepared
	return prepared, true, nil
}

func (r *incrementalRenderSession) collectColdGroupAdditions(
	group string,
	ownedEvaluated []incremental.ExactResult,
) (additions []incrementalColdGroupAddition, eligible bool, err error) {
	additions = make([]incrementalColdGroupAddition, len(ownedEvaluated))
	seenResults := make(map[string]struct{}, len(ownedEvaluated))
	seenInstances := make(map[string]struct{}, len(ownedEvaluated))
	for resultIndex := range ownedEvaluated {
		result := &ownedEvaluated[resultIndex]
		if err := r.state.graph.ValidateExactValue(result.Key, result.Value); err != nil {
			return nil, false, fmt.Errorf(
				"authenticating evaluated incremental result %s: %w", result.Key, err,
			)
		}
		fresh := r.freshResults[result.Key]
		if fresh == nil {
			return nil, false, nil
		}
		target, targetErr := r.evaluatedResultTarget(group, result)
		if targetErr != nil {
			return nil, false, targetErr
		}
		if _, retired := r.retired.Get([]byte(result.Key.Opaque())); retired {
			return nil, false, nil
		}
		if _, duplicate := seenResults[string(target.key)]; duplicate {
			return nil, false, errors.New("cold incremental group batch repeats a result identity")
		}
		seenResults[string(target.key)] = struct{}{}
		id := incrementalGroupInstanceID{
			component: target.component.name,
			source:    target.source,
			namespace: target.namespace,
			name:      target.name,
		}
		instanceKey := incrementalGroupInstanceKey(id)
		if _, duplicate := seenInstances[string(instanceKey)]; duplicate {
			return nil, false, errors.New("cold incremental group batch repeats an instance identity")
		}
		seenInstances[string(instanceKey)] = struct{}{}
		previous, cached := r.results.Get(target.key)
		if err := r.verifyGroupIndexResult(
			target.component, target.source, target.namespace, target.name, previous, cached, target.key,
		); err != nil {
			return nil, false, err
		}
		if cached {
			return nil, false, nil
		}
		effects, executed := r.httpExecuted[result.Key]
		if !executed {
			return nil, false, errors.New("fresh incremental component result has no HTTP execution record")
		}
		additions[resultIndex] = incrementalColdGroupAddition{
			evaluated: result,
			target:    target,
			fresh:     fresh,
			effects:   cloneHTTPEffects(effects),
		}
	}
	return additions, true, nil
}

func buildColdGroupPreparedInstances(
	additions []incrementalColdGroupAddition,
) ([]incrementalPreparedGroupInstance, []incrementalPreparedPlanGroupAddition) {
	groupInstances := make([]incrementalPreparedGroupInstance, len(additions))
	planAdditions := make([]incrementalPreparedPlanGroupAddition, len(additions))
	for additionIndex := range additions {
		addition := &additions[additionIndex]
		instance := &incrementalInstanceResult{
			component: addition.target.component.name,
			source:    addition.target.source,
			namespace: addition.target.namespace,
			name:      addition.target.name,
		}
		groupInstances[additionIndex] = incrementalPreparedGroupInstance{
			instance:    instance,
			component:   addition.target.component,
			queryKey:    addition.evaluated.Key,
			fresh:       addition.fresh,
			encoded:     addition.evaluated.Value,
			httpEffects: addition.effects,
		}
		planAdditions[additionIndex] = incrementalPreparedPlanGroupAddition{
			component: addition.target.component,
			id: incrementalGroupInstanceID{
				component: instance.component,
				source:    instance.source,
				namespace: instance.namespace,
				name:      instance.name,
			},
		}
	}
	return groupInstances, planAdditions
}

func (r *incrementalRenderSession) installPreparedColdGroupAdditions(
	prepared *incrementalPreparedColdGroupAdditions,
) (bool, error) {
	if prepared == nil || prepared.seal != prepared || prepared.authority == nil ||
		prepared.authority.seal != prepared.authority || prepared.authority.install == nil {
		return false, errors.New("prepared cold incremental group additions have invalid provenance")
	}
	return prepared.authority.install(prepared, r)
}

func (r *incrementalRenderSession) installColdGroupAdditions(
	group string,
	index, updated *incrementalGroupIndex,
	additions []incrementalColdGroupAddition,
	planAdditions []incrementalPreparedPlanGroupAddition,
	planBatch *incrementalPreparedPlanColdBatch,
) (bool, error) {
	if r == nil || index == nil || updated == nil || len(additions) == 0 ||
		len(additions) != len(planAdditions) || r.groupIndexes[group] != index {
		return false, errors.New("prepared cold incremental group additions no longer match the session")
	}
	if err := index.validateAuthentication(); err != nil {
		return false, err
	}
	if err := updated.validateAuthentication(); err != nil {
		return false, err
	}
	if err := r.validateColdGroupAdditions(additions, planAdditions); err != nil {
		return false, err
	}

	oldResultRoot := r.results.Root()
	staged, err := r.stageColdGroupAdditions(group, index, updated, additions, planAdditions)
	if err != nil {
		return false, err
	}
	preparedPlan, statusPlan, err := r.applyColdGroupPlans(
		group, index, updated, planAdditions, oldResultRoot, staged.results.Root(),
	)
	if err != nil {
		return false, err
	}
	if r.preparedPlanBootstrapPending {
		if r.preparedPlanColdBuilder == nil {
			return false, errors.New("incremental prepared plan cold builder is unavailable")
		}
		if err := r.preparedPlanColdBuilder.commit(planBatch); err != nil {
			return false, err
		}
	}
	r.results = staged.results
	r.derived = staged.derived
	r.httpEffects = staged.httpEffects
	r.httpRefDeltas = staged.httpRefDeltas
	r.selectorPending = staged.selectorPending
	r.groupIndexes[group] = updated
	r.preparedPlan = preparedPlan
	r.statusPlan = statusPlan
	r.groupChanged[group] = true
	for additionIndex := range additions {
		addition := &additions[additionIndex]
		delete(r.httpExecuted, addition.evaluated.Key)
		r.finishEvaluatedQuery(addition.evaluated.Key)
	}
	return true, nil
}

func (r *incrementalRenderSession) validateColdGroupAdditions(
	additions []incrementalColdGroupAddition,
	planAdditions []incrementalPreparedPlanGroupAddition,
) error {
	for additionIndex := range additions {
		addition := &additions[additionIndex]
		if addition.evaluated == nil || addition.fresh == nil || planAdditions[additionIndex].result == nil {
			return errors.New("prepared cold incremental group addition is incomplete")
		}
		if err := r.state.graph.ValidateExactValue(addition.evaluated.Key, addition.evaluated.Value); err != nil {
			return fmt.Errorf(
				"authenticating prepared incremental result %s: %w",
				addition.evaluated.Key,
				err,
			)
		}
		if r.freshResults[addition.evaluated.Key] != addition.fresh {
			return errors.New("prepared cold incremental group addition has a different fresh result")
		}
		if err := validateAuthenticatedFreshComponentResult(
			addition.fresh, addition.evaluated.Key, addition.evaluated.Value,
		); err != nil {
			return err
		}
		if _, retired := r.retired.Get([]byte(addition.evaluated.Key.Opaque())); retired {
			return errors.New("prepared cold incremental group addition was retired")
		}
		if _, cached := r.results.Get(addition.target.key); cached {
			return errors.New("prepared cold incremental group addition already has a cached result")
		}
		effects, executed := r.httpExecuted[addition.evaluated.Key]
		if !executed || !sameHTTPEffects(effects, addition.effects) {
			return errors.New("prepared cold incremental group addition has different HTTP effects")
		}
	}
	return nil
}

type stagedColdGroupAdditions struct {
	results         *iradix.Txn[incremental.ExactValueRoot]
	derived         *iradix.Txn[incrementalDerivedResource]
	httpEffects     *iradix.Txn[*iradix.Tree[incrementalHTTPEffect]]
	httpRefDeltas   map[uint64]httpRefDelta
	selectorPending map[incrementalSelectorIdentity]incremental.Input
}

func (r *incrementalRenderSession) stageColdGroupAdditions(
	group string,
	index, updated *incrementalGroupIndex,
	additions []incrementalColdGroupAddition,
	planAdditions []incrementalPreparedPlanGroupAddition,
) (*stagedColdGroupAdditions, error) {
	staged := &stagedColdGroupAdditions{
		results:         r.results.Clone(),
		derived:         r.derived.Clone(),
		httpEffects:     r.httpEffects.Clone(),
		httpRefDeltas:   maps.Clone(r.httpRefDeltas),
		selectorPending: maps.Clone(r.selectorPending),
	}
	for additionIndex := range additions {
		addition := &additions[additionIndex]
		result := planAdditions[additionIndex].result
		if err := stageValidatedIncrementalColdDerivations(staged.derived, result); err != nil {
			return nil, err
		}
		staged.results.Insert(addition.target.key, addition.evaluated.Value)
		if err := stageIncrementalColdHTTPEffects(
			staged.httpEffects, staged.httpRefDeltas, addition.target.key, addition.effects,
		); err != nil {
			return nil, err
		}
	}
	for additionIndex := range additions {
		if err := stageIncrementalSelectorReplacementInto(
			staged.selectorPending,
			r.sessionHasInputDependents,
			group,
			index,
			updated,
			planAdditions[additionIndex].id,
			planAdditions[additionIndex].result,
		); err != nil {
			return nil, err
		}
	}
	return staged, nil
}

func (r *incrementalRenderSession) applyColdGroupPlans(
	group string,
	index, updated *incrementalGroupIndex,
	planAdditions []incrementalPreparedPlanGroupAddition,
	oldResultRoot, newResultRoot *iradix.Node[incremental.ExactValueRoot],
) (*incrementalPreparedPlan, *templating.StatusPatchProjectionPlan, error) {
	var err error
	preparedPlan := r.preparedPlan
	if preparedPlan != nil && !r.preparedPlanBootstrapPending {
		preparedPlan, err = preparedPlan.applyGroupAdditions(
			group, index, updated, planAdditions, oldResultRoot, newResultRoot,
		)
		if err != nil {
			return nil, nil, err
		}
	}
	statusPlan := r.statusPlan
	if !r.statusPlanBootstrapPending {
		for additionIndex := range planAdditions {
			statusPlan, err = replaceIncrementalStatusPatchPlanInstance(
				statusPlan,
				group,
				index,
				updated,
				planAdditions[additionIndex].id,
			)
			if err != nil {
				return nil, nil, err
			}
		}
	}
	return preparedPlan, statusPlan, nil
}

func stageValidatedIncrementalColdDerivations(
	derived *iradix.Txn[incrementalDerivedResource],
	result *incrementalComponentResult,
) error {
	for index := range result.Derivations {
		entry := ownValidatedIncrementalDerivedResource(&result.Derivations[index])
		identityKey := derivedKey(entry.Identity)
		if current, exists := derived.Get(identityKey); exists &&
			current != entry {
			return fmt.Errorf("incremental derived resource %q has conflicting owners", entry.Identity.Name)
		}
		derived.Insert(identityKey, entry)
	}
	return nil
}

func stageIncrementalColdHTTPEffects(
	httpEffects *iradix.Txn[*iradix.Tree[incrementalHTTPEffect]],
	refDeltas map[uint64]httpRefDelta,
	key []byte,
	effects []incrementalHTTPEffect,
) error {
	if _, exists := httpEffects.Get(key); exists {
		return errors.New("cold incremental HTTP effects already have a cached result")
	}
	indexed, err := newIncrementalIndexedHTTPEffects(effects)
	if err != nil {
		return err
	}
	for index := range effects {
		if err := adjustHTTPRefDeltaIn(refDeltas, effects[index].inputID, true); err != nil {
			return err
		}
	}
	if indexed.Len() != 0 {
		httpEffects.Insert(key, indexed)
	}
	return nil
}

func (r *incrementalRenderSession) applyEvaluatedResult(
	group string,
	evaluated *incremental.ExactResult,
) error {
	if err := r.state.graph.ValidateExactValue(evaluated.Key, evaluated.Value); err != nil {
		return fmt.Errorf("authenticating evaluated incremental result %s: %w", evaluated.Key, err)
	}
	fresh, err := r.authenticatedFreshResult(evaluated.Key, evaluated.Value)
	if err != nil {
		return err
	}
	target, err := r.evaluatedResultTarget(group, evaluated)
	if err != nil {
		return err
	}
	if _, retired := r.retired.Get([]byte(evaluated.Key.Opaque())); retired {
		return r.applyRetiredResult(
			group, evaluated, target.component, target.source, target.namespace, target.name, target.key, fresh,
		)
	}
	return r.applyActiveResult(
		group, evaluated, target.component, target.source, target.namespace, target.name, target.key, fresh,
	)
}

func (r *incrementalRenderSession) authenticatedFreshResult(
	key incremental.QueryKey,
	root incremental.ExactValueRoot,
) (*incrementalComponentResult, error) {
	fresh, found, err := r.authenticateFreshComponentResult(key, root)
	if err != nil || !found {
		return nil, err
	}
	result, err := takeAuthenticatedFreshComponentResult(fresh, key, root)
	if err != nil {
		return nil, fmt.Errorf("fresh incremental component result %s: %w", key, err)
	}
	return &result, nil
}

func (r *incrementalRenderSession) authenticateFreshComponentResult(
	key incremental.QueryKey,
	root incremental.ExactValueRoot,
) (*authenticatedFreshComponentResult, bool, error) {
	fresh := r.freshResults[key]
	if fresh == nil {
		return nil, false, nil
	}
	if err := validateAuthenticatedFreshComponentResult(fresh, key, root); err != nil {
		return nil, false, fmt.Errorf("fresh incremental component result %s: %w", key, err)
	}
	return fresh, true, nil
}

type evaluatedResultTarget struct {
	component *incrementalComponent
	source    string
	namespace string
	name      string
	key       []byte
}

func (r *incrementalRenderSession) evaluatedResultTarget(
	group string,
	evaluated *incremental.ExactResult,
) (evaluatedResultTarget, error) {
	definition, source, namespace, objectName, parsed := r.resolveComponentQuery(evaluated.Key)
	if !parsed {
		return evaluatedResultTarget{},
			fmt.Errorf("incremental graph returned an invalid component key %s", evaluated.Key)
	}
	if definition.group != group {
		return evaluatedResultTarget{},
			fmt.Errorf("incremental graph returned component %q outside group %q", definition.name, group)
	}
	return evaluatedResultTarget{
		component: &definition,
		source:    source,
		namespace: namespace,
		name:      objectName,
		key:       resultKey(&definition, source, namespace, objectName),
	}, nil
}

func (r *incrementalRenderSession) applyRetiredResult(
	group string,
	evaluated *incremental.ExactResult,
	definition *incrementalComponent,
	source, namespace, objectName string,
	key []byte,
	fresh *incrementalComponentResult,
) error {
	result := fresh
	if result == nil {
		decoded, err := decodeExactComponentResult(evaluated.Value)
		if err != nil {
			return fmt.Errorf("decoding retired incremental component %q result: %w", definition.name, err)
		}
		result = &decoded
	}
	if result.Text != "" || len(result.Unique) != 0 || len(result.Derivations) != 0 || len(result.Events) != 0 ||
		len(result.Published) != 0 || result.PublishedDigest != "" ||
		len(result.BackendPlan) != 0 || len(result.BackendPlanOutput) != 0 || result.BackendPlanDigest != "" {
		return fmt.Errorf("retired incremental component %q returned effects", definition.name)
	}
	effects, executed := r.httpExecuted[evaluated.Key]
	if executed {
		if err := r.applyRetiredHTTPEffects(group, definition, source, namespace, objectName, key, effects); err != nil {
			return err
		}
		delete(r.httpExecuted, evaluated.Key)
	}
	r.finishEvaluatedQuery(evaluated.Key)
	return nil
}

func (r *incrementalRenderSession) applyRetiredHTTPEffects(
	group string,
	definition *incrementalComponent,
	source, namespace, objectName string,
	key []byte,
	effects []incrementalHTTPEffect,
) error {
	if len(effects) != 0 {
		return fmt.Errorf("retired incremental component %q returned HTTP effects", definition.name)
	}
	previous, existed := r.results.Get(key)
	resultRoot := r.results.Root()
	httpChanged, err := r.replaceHTTPEffects(key, effects)
	if err != nil {
		return err
	}
	if !httpChanged {
		return nil
	}
	if !existed {
		return fmt.Errorf("retired incremental component %q has HTTP effects without a cached result", definition.name)
	}
	if err := r.replaceGroupIndexResult(
		definition, source, namespace, objectName, previous, key, resultRoot, nil,
	); err != nil {
		return err
	}
	r.groupChanged[group] = true
	return nil
}

func (r *incrementalRenderSession) applyActiveResult(
	group string,
	evaluated *incremental.ExactResult,
	definition *incrementalComponent,
	source, namespace, objectName string,
	key []byte,
	fresh *incrementalComponentResult,
) error {
	previous, existed := r.results.Get(key)
	resultRoot := r.results.Root()
	if err := r.verifyGroupIndexResult(definition, source, namespace, objectName, previous, existed, key); err != nil {
		return err
	}
	resultChanged := !existed
	if existed {
		same, err := previous.SameRoot(evaluated.Value)
		if err != nil {
			return err
		}
		resultChanged = !same
	}
	if resultChanged {
		if err := r.replaceDerivations(
			key, previous, evaluated.Value, fresh, definition, source, namespace, objectName,
		); err != nil {
			return err
		}
		r.results.Insert(key, evaluated.Value)
	}
	httpChanged := false
	if effects, executed := r.httpExecuted[evaluated.Key]; executed {
		changed, replaceErr := r.replaceHTTPEffects(key, effects)
		if replaceErr != nil {
			return replaceErr
		}
		httpChanged = changed
		delete(r.httpExecuted, evaluated.Key)
	}
	if resultChanged || httpChanged {
		if err := r.replaceGroupIndexResult(
			definition, source, namespace, objectName, evaluated.Value, key, resultRoot, fresh,
		); err != nil {
			return err
		}
		r.groupChanged[group] = true
	}
	r.finishEvaluatedQuery(evaluated.Key)
	return nil
}

func (r *incrementalRenderSession) finishEvaluatedQuery(key incremental.QueryKey) {
	delete(r.newQueries, key)
	delete(r.dirtyQueries, key)
	delete(r.freshResults, key)
}

func (r *incrementalRenderSession) replaceGroupIndexResult(
	component *incrementalComponent,
	source, namespace, name string,
	encoded incremental.ExactValueRoot,
	resultKey []byte,
	previousResultRoot *iradix.Node[incremental.ExactValueRoot],
	fresh *incrementalComponentResult,
) error {
	queryKey := componentQueryKey(component, source, namespace, name)
	if err := r.state.graph.ValidateExactValue(queryKey, encoded); err != nil {
		return fmt.Errorf("authenticating incremental component %q result: %w", component.name, err)
	}
	index, exists := r.groupIndexes[component.group]
	if !exists || index == nil {
		return fmt.Errorf("incremental group %q has no assembly index", component.group)
	}
	result := fresh
	if result == nil {
		decoded, err := decodeExactComponentResult(encoded)
		if err != nil {
			return fmt.Errorf("decoding incremental component %q result for its assembly index: %w", component.name, err)
		}
		result = &decoded
	}
	if err := validateIncrementalEffects(component, source, namespace, name, result); err != nil {
		return err
	}
	indexedEffects, effectsExist := r.httpEffects.Get(resultKey)
	if effectsExist && indexedEffects == nil {
		return errors.New("incremental component HTTP effect set is unavailable")
	}
	effects := indexedHTTPEffects(indexedEffects)
	instance := &incrementalInstanceResult{
		component: component.name,
		source:    source,
		namespace: namespace,
		name:      name,
		result:    *result,
	}
	if err := validateIncrementalPublicationResultGroup(result, component.group); err != nil {
		return err
	}
	var updated *incrementalGroupIndex
	var err error
	if fresh == nil {
		updated, err = index.replace(instance, effects)
	} else {
		immutable, valueErr := encoded.String()
		if valueErr != nil {
			return valueErr
		}
		updated, err = index.replacePrepared(instance, immutable, result, effects)
	}
	if err != nil {
		return err
	}
	id := incrementalGroupInstanceID{
		component: component.name,
		source:    source,
		namespace: namespace,
		name:      name,
	}
	if err := r.stageIncrementalSelectorReplacement(component.group, index, updated, id, result); err != nil {
		return err
	}
	preparedPlan, statusPlan, err := r.applyGroupReplacementPlans(
		component, index, updated, id, previousResultRoot,
	)
	if err != nil {
		return err
	}
	r.groupIndexes[component.group] = updated
	r.preparedPlan = preparedPlan
	r.statusPlan = statusPlan
	return nil
}

func (r *incrementalRenderSession) applyGroupReplacementPlans(
	component *incrementalComponent,
	index, updated *incrementalGroupIndex,
	id incrementalGroupInstanceID,
	previousResultRoot *iradix.Node[incremental.ExactValueRoot],
) (*incrementalPreparedPlan, *templating.StatusPatchProjectionPlan, error) {
	var err error
	preparedPlan := r.preparedPlan
	if preparedPlan != nil && !r.preparedPlanBootstrapPending {
		preparedPlan, err = preparedPlan.applyGroupReplacement(
			component,
			component.group,
			index,
			updated,
			id,
			previousResultRoot,
			r.results.Root(),
		)
		if err != nil {
			return nil, nil, err
		}
	}
	statusPlan := r.statusPlan
	if !r.statusPlanBootstrapPending {
		statusPlan, err = replaceIncrementalStatusPatchPlanInstance(
			r.statusPlan,
			component.group,
			index,
			updated,
			id,
		)
		if err != nil {
			return nil, nil, err
		}
	}
	return preparedPlan, statusPlan, nil
}

func (r *incrementalRenderSession) refreshGroup(group string) error {
	if r.groupReady[group] && !r.groupChanged[group] {
		return nil
	}
	index, exists := r.groupIndexes[group]
	if !exists || index == nil {
		return fmt.Errorf("incremental group %q has no assembly index", group)
	}
	if err := index.validateAuthentication(); err != nil {
		return err
	}
	r.groupReady[group] = true
	r.groupChanged[group] = false
	return nil
}

func (r *incrementalRenderSession) HasIncrementalCalls() bool {
	r.renderMu.Lock()
	defer r.renderMu.Unlock()
	for group := range r.calls {
		if len(r.calls[group]) != 0 {
			return true
		}
	}
	return false
}

func (r *incrementalRenderSession) ValidateIncrementalCalls() error {
	r.renderMu.Lock()
	defer r.renderMu.Unlock()
	if err := validateIncrementalCallsWithValues(r.state.groups, r.calls, r.valueAccesses); err != nil {
		return fmt.Errorf("%w%s", err, r.groupCallDiagnostics())
	}
	return r.finalizeIncrementalStatusPlanLocked()
}

// completeExactCycleReplayScope finishes a replay that matched its recorded
// observations. It deliberately does not validate calls: a replay invokes only
// the components the recorded cycle observed, so the live call bookkeeping
// describes that walk rather than a full render, and every group the walk did
// not touch reads as silent. The recorded cycle was validated when it was
// produced, and matching it is what proves this render's output. The
// unchanged-roots replay has always completed without this check; a matched
// replay owes no more.
func (r *incrementalRenderSession) completeExactCycleReplayScope() error {
	r.renderMu.Lock()
	defer r.renderMu.Unlock()
	return r.finalizeIncrementalStatusPlanLocked()
}

func (r *incrementalRenderSession) finalizeIncrementalStatusPlanLocked() error {
	if r.statusPatchesReplayed {
		return nil
	}
	if err := r.finalizeStatusPatchPlanBootstrap(); err != nil {
		return err
	}
	statusPlan, err := stageIncrementalStatusPatchPlan(r.baseContext, r.statusPlan)
	if err != nil {
		return err
	}
	r.statusPlan = statusPlan
	r.statusPatchesReplayed = true
	return nil
}

func validateIncrementalCalls(
	groups map[string][]incrementalComponent,
	calls map[string][]incrementalCall,
) error {
	return validateIncrementalCallsWithValues(groups, calls, nil)
}

// validateIncrementalCallsWithValues holds every group that took part in this
// render to its complete canonical sequence. A group that neither ran a
// component nor had a value read took no part: the chart's own conditions
// excluded its consumer, the way an HTTP filter library renders nothing when no
// frontend exists to hold it. The root template runs in full on every
// non-replay render, so an untouched group is a chart decision, never skipped
// work -- and it contributes the same nothing a cold render would.
func validateIncrementalCallsWithValues(
	groups map[string][]incrementalComponent,
	calls map[string][]incrementalCall,
	valueAccesses map[string]int,
) error {
	groupNames := make([]string, 0, len(groups))
	for group := range groups {
		groupNames = append(groupNames, group)
	}
	slices.Sort(groupNames)
	for _, group := range groupNames {
		if len(calls[group]) == 0 && valueAccesses[group] == 0 {
			continue
		}
		if _, err := validateIncrementalGroupCalls(group, groups[group], calls[group]); err != nil {
			return err
		}
	}
	for group := range calls {
		if _, exists := groups[group]; !exists {
			return fmt.Errorf("incremental group %q is not configured", group)
		}
	}
	for group := range valueAccesses {
		if _, exists := groups[group]; !exists {
			return fmt.Errorf("incremental group %q is not configured", group)
		}
	}
	return nil
}

// groupCallDiagnostics names why each silent group was expected to render, so a
// torn render is diagnosable from the failure alone. Without it the message
// says only that a group made no calls, which is the same text whether the
// group was read before it ran or genuinely has none.
func (r *incrementalRenderSession) groupCallDiagnostics() string {
	names := make([]string, 0, len(r.state.groups))
	for group := range r.state.groups {
		if len(r.calls[group]) == 0 {
			names = append(names, group)
		}
	}
	if len(names) == 0 {
		return ""
	}
	slices.Sort(names)
	var out strings.Builder
	out.WriteString(" (silent groups:")
	for _, group := range names {
		fmt.Fprintf(&out, " %s[reads=%d components=%d cold=%t mode=%s]",
			group, r.valueAccesses[group],
			len(r.state.groups[group]), r.cold, r.renderMode)
	}
	out.WriteString(")")
	return out.String()
}

func validateIncrementalGroupCalls(
	group string,
	expected []incrementalComponent,
	actual []incrementalCall,
) (int, error) {
	if len(expected) == 0 {
		return 0, fmt.Errorf("incremental group %q has no configured components", group)
	}
	byScope := map[string][]incrementalCall{}
	for index, call := range actual {
		if call.scope == "" {
			return 0, fmt.Errorf("incremental group %q ran outside a root template at call %d", group, index+1)
		}
		byScope[call.scope] = append(byScope[call.scope], call)
	}
	if len(byScope) == 0 {
		return 0, fmt.Errorf(
			"incremental group %q must render complete canonical sequences of its %d components; got 0 calls",
			group, len(expected),
		)
	}
	scopes := make([]string, 0, len(byScope))
	for scope := range byScope {
		scopes = append(scopes, scope)
	}
	slices.Sort(scopes)
	completed := 0
	for _, scope := range scopes {
		count, err := validateIncrementalGroupCallsInScope(group, scope, expected, byScope[scope])
		completed += count
		if err != nil {
			return completed, err
		}
	}
	return completed, nil
}

func validateIncrementalGroupCallsInScope(
	group, scope string,
	expected []incrementalComponent,
	actual []incrementalCall,
) (int, error) {
	if len(expected) == 0 {
		return 0, fmt.Errorf("incremental group %q has no configured components", group)
	}
	for index, call := range actual {
		sequence := index / len(expected)
		position := index % len(expected)
		if call.scope == "" {
			return sequence, fmt.Errorf("incremental group %q ran outside a root template in sequence %d", group, sequence+1)
		}
		if call.scope != scope {
			return sequence, fmt.Errorf(
				"incremental group %q scope index for %q contains a call from root template %q",
				group, scope, call.scope,
			)
		}
		if call.component != expected[position].name {
			return sequence, fmt.Errorf(
				"incremental group %q must render each sequence in canonical order within root template %q; expected %q at position %d of sequence %d, got %q",
				group, scope, expected[position].name, position, sequence+1, call.component,
			)
		}
	}
	completed := len(actual) / len(expected)
	trailing := len(actual) % len(expected)
	if completed == 0 || trailing != 0 {
		return completed, fmt.Errorf(
			"incremental group %q must render complete canonical sequences of its %d components within root template %q; got %d calls (%d complete sequences and %d trailing calls)",
			group, len(expected), scope, len(actual), completed, trailing,
		)
	}
	return completed, nil
}

func (r *incrementalRenderSession) queriesForGroup(group string) ([]incremental.QueryKey, error) {
	set := map[incremental.QueryKey]struct{}{}
	if !r.groupReady[group] {
		if err := r.addInitialGroupQueries(group, set); err != nil {
			return nil, err
		}
		return r.sortedActiveQueries(set), nil
	}
	r.addTrackedGroupQueries(group, r.newQueries, set)
	r.addTrackedGroupQueries(group, r.dirtyQueries, set)
	return r.sortedActiveQueries(set), nil
}

func (r *incrementalRenderSession) addInitialGroupQueries(
	group string,
	set map[incremental.QueryKey]struct{},
) error {
	for componentIndex := range r.state.groups[group] {
		component := &r.state.groups[group][componentIndex]
		for _, binding := range r.bindingPlan.byComponent[component.name] {
			if err := r.addInitialBindingQueries(component, binding, set); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *incrementalRenderSession) addInitialBindingQueries(
	component *incrementalComponent,
	binding incrementalBinding,
	set map[incremental.QueryKey]struct{},
) error {
	if component.resourceProjection {
		query, namespace, name, err := incrementalResourceProjectionQueryKey(component, binding)
		if err != nil {
			return fmt.Errorf("incremental component %q: %w", component.name, err)
		}
		registered := r.registerComponentQuery(component, binding.source, namespace, name)
		if registered != query {
			return errors.New("resource projection query identity is inconsistent")
		}
		set[registered] = struct{}{}
		return nil
	}
	var activationErr error
	r.members.Root().WalkPrefix(memberPrefix(binding.source), func(key []byte, _ struct{}) bool {
		namespace, name, ok := parseMemberKey(key)
		if !ok {
			return false
		}
		active, err := r.activationActive(component, binding.source, namespace, name)
		if err != nil {
			activationErr = err
			return true
		}
		if active {
			set[r.registerComponentQuery(component, binding.source, namespace, name)] = struct{}{}
		}
		return false
	})
	return activationErr
}

func (r *incrementalRenderSession) addTrackedGroupQueries(
	group string,
	tracked map[incremental.QueryKey]struct{},
	set map[incremental.QueryKey]struct{},
) {
	for key := range tracked {
		component, ok := r.resolveQueryComponent(key)
		if ok && component.group == group {
			set[key] = struct{}{}
		}
	}
}

func (r *incrementalRenderSession) sortedActiveQueries(
	set map[incremental.QueryKey]struct{},
) []incremental.QueryKey {
	keys := make([]incremental.QueryKey, 0, len(set))
	for key := range set {
		if _, removed := r.removed[key]; !removed {
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, func(left, right incremental.QueryKey) int {
		return strings.Compare(left.Opaque(), right.Opaque())
	})
	return keys
}

func sameHTTPSnapshot(left, right *httpstore.ContentSnapshot) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.URL == right.URL && left.Descriptor == right.Descriptor &&
		left.Content == right.Content && left.Found == right.Found &&
		left.Cacheable == right.Cacheable && left.Token == right.Token &&
		left.StoreSource == right.StoreSource && left.Observation == right.Observation
}

func sameHTTPSemanticValue(left, right *httpstore.ContentSnapshot) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.URL == right.URL && left.Descriptor == right.Descriptor &&
		left.Content == right.Content && left.Found == right.Found && left.Cacheable == right.Cacheable
}

func sameHTTPReusableSnapshot(left, right *httpstore.ContentSnapshot) bool {
	if sameHTTPSnapshot(left, right) {
		return true
	}
	if left == nil || right == nil || left.StoreSource != right.StoreSource ||
		!left.Cacheable || !right.Cacheable || left.Watermark < left.Observation ||
		right.Watermark < right.Observation {
		return false
	}
	leftObservation := left.ObservationToken()
	rightObservation := right.ObservationToken()
	return leftObservation.Valid() && rightObservation.Valid() && sameHTTPSemanticValue(left, right)
}

func sameHTTPEffects(left, right []incrementalHTTPEffect) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].inputID != right[index].inputID ||
			!sameHTTPReusableSnapshot(&left[index].snapshot, &right[index].snapshot) {
			return false
		}
	}
	return true
}

func sameIndexedHTTPEffects(
	left, right *iradix.Tree[incrementalHTTPEffect],
) (bool, error) {
	if err := validateIndexedHTTPEffects(left); err != nil {
		return false, err
	}
	if err := validateIndexedHTTPEffects(right); err != nil {
		return false, err
	}
	if left.Len() != right.Len() {
		return false, nil
	}
	same := true
	left.Root().Walk(func(key []byte, effect incrementalHTTPEffect) bool {
		other, exists := right.Root().Get(key)
		if !exists || other.inputID != effect.inputID ||
			!sameHTTPReusableSnapshot(&effect.snapshot, &other.snapshot) {
			same = false
		}
		return false
	})
	return same, nil
}

func validateIndexedHTTPEffects(tree *iradix.Tree[incrementalHTTPEffect]) error {
	if tree == nil {
		return errors.New("incremental HTTP effect set is unavailable")
	}
	var validationErr error
	tree.Root().Walk(func(key []byte, effect incrementalHTTPEffect) bool {
		if effect.inputID == 0 || !bytes.Equal(key, incrementalHTTPIdentityKey(effect.inputID)) {
			validationErr = errors.New("incremental HTTP effect set has an invalid identity")
			return true
		}
		return false
	})
	return validationErr
}

func (r *incrementalRenderSession) replayGroupEvents(group string) error {
	index := r.groupIndexes[group]
	if index == nil {
		return fmt.Errorf("incremental group %q has no assembly index", group)
	}
	events, err := index.renderedEvents()
	if err != nil {
		return err
	}
	collector, _ := r.baseContext["recordEventCollector"].(*templating.EventCollector)
	if collector == nil && len(events) > 0 {
		return errors.New("incremental recordEvent collector is unavailable")
	}
	for index := range events {
		event := &events[index]
		if err := collector.Register(event.Namespace, event.Name, event.APIVersion, event.Kind,
			event.Type, event.Reason, event.Message); err != nil {
			return err
		}
	}
	return nil
}

// verifyResources verifies what this commit publishes.
//
// A committed graph records the cursors it pinned, so the next render replays
// every change that landed after them; checking the live store here only
// withholds the graph under churn, where a relevant input has always moved by
// commit time.
func (r *incrementalRenderSession) verifyResources(
	ctx context.Context,
	inputs []incremental.InputRevision,
) (bool, error) {
	return r.verifyInputs(ctx, inputs, r.commitOutlivesNextRender())
}

// commitOutlivesNextRender reports whether this commit must match the live
// store: an admission verdict cannot be revised by a later render. Accepted
// content is checked against this render's own snapshot; the render gate
// revokes it if a later render containing it fails (ADR-0030).
func (r *incrementalRenderSession) commitOutlivesNextRender() bool {
	return r.renderMode == rendercontext.RenderModeAdmission
}

// verifyCachePublicationResources verifies what the background cache builder
// publishes: the render's own observations, never the live store.
func (r *incrementalRenderSession) verifyCachePublicationResources(
	ctx context.Context,
	inputs []incremental.InputRevision,
) (bool, error) {
	return r.verifyInputs(ctx, inputs, false)
}

func (r *incrementalRenderSession) verifyInputs(
	ctx context.Context,
	inputs []incremental.InputRevision,
	liveStore bool,
) (bool, error) {
	verified, err := r.verifyBindingPlan(ctx)
	if err != nil || !verified {
		return verified, err
	}
	verified, err = r.verifyResourceInputs(ctx, inputs, liveStore)
	if err != nil || !verified {
		return verified, err
	}
	if inputs == nil {
		return true, nil
	}
	return r.verifyHTTPInputs(inputs)
}

func (r *incrementalRenderSession) verifyBindingPlan(ctx context.Context) (bool, error) {
	if r.state == nil || r.bindingPlan == nil {
		return true, nil
	}
	if r.bindingPlanExact {
		return true, nil
	}
	bindings, err := r.state.planBindings(ctx, r.baseContext)
	if err != nil {
		return false, err
	}
	return sameIncrementalBindingPlans(r.bindingPlan, bindings), nil
}

func (r *incrementalRenderSession) verifyHTTPInputs(inputs []incremental.InputRevision) (bool, error) {
	r.httpMu.Lock()
	observedInputs := make(map[incremental.InputKey]incremental.Input, len(r.httpObserved))
	for key, input := range r.httpObserved {
		observedInputs[key] = input
	}
	proofs := make([]httpstore.ObservationToken, 0, len(r.httpProofs))
	proofKeys := make(map[incremental.InputKey]struct{}, len(r.httpProofs))
	for key := range r.httpProofs {
		proofKeys[key] = struct{}{}
		proofs = append(proofs, r.httpProofs[key])
	}
	r.httpMu.Unlock()
	for _, observed := range inputs {
		if _, encoded := parseHTTPInputKey(observed.Key); !encoded {
			continue
		}
		spec, exists := r.state.httpInputSpec(observed.Key)
		if !exists {
			return false, nil
		}
		current, observedNow := observedInputs[observed.Key]
		if !observedNow {
			if r.httpLease == nil || !r.httpLease.Contains(spec.url, spec.descriptor) {
				return false, nil
			}
			continue
		}
		if current.Revision != observed.Revision || current.Found != observed.Found {
			return false, nil
		}
		if _, proved := proofKeys[observed.Key]; !proved {
			return false, nil
		}
	}
	if len(proofs) == 0 {
		return true, nil
	}
	if r.httpComponent == nil {
		return false, nil
	}
	return r.httpComponent.VerifyObservations(proofs), nil
}

func (r *incrementalRenderSession) verifyResourceInputs(
	ctx context.Context,
	observations []incremental.InputRevision,
	liveStore bool,
) (bool, error) {
	r.mu.Lock()
	proofs := make(map[incremental.InputKey]incremental.Input, len(r.resourceProofs))
	for key, input := range r.resourceProofs {
		proofs[key] = input
	}
	r.mu.Unlock()

	verified, err := r.verifyObservedResourceInputs(observations, proofs)
	if err != nil || !verified || !liveStore {
		return verified, err
	}
	currentRoots, deltaVerified, complete, err := r.collectLateResourceProofs(ctx, proofs)
	if err != nil || !complete {
		return complete, err
	}
	return r.verifyCurrentResourceInputs(ctx, proofs, currentRoots, deltaVerified)
}

func (r *incrementalRenderSession) verifyObservedResourceInputs(
	observations []incremental.InputRevision,
	proofs map[incremental.InputKey]incremental.Input,
) (bool, error) {
	for _, observed := range observations {
		if _, encoded := parseResourceInputKey(observed.Key); !encoded {
			continue
		}
		if r.graphSession == nil {
			return false, nil
		}
		verified, err := r.verifyObservedResourceInput(observed, proofs)
		if err != nil || !verified {
			return verified, err
		}
	}
	return true, nil
}

func (r *incrementalRenderSession) verifyObservedResourceInput(
	observed incremental.InputRevision,
	proofs map[incremental.InputKey]incremental.Input,
) (bool, error) {
	if previous, proved := proofs[observed.Key]; proved {
		if previous.Key != observed.Key || previous.Revision != observed.Revision ||
			previous.Found != observed.Found {
			return false, nil
		}
		return r.graphSession.MatchesExactInput(previous)
	}
	expected, exists, err := r.graphSession.ExactInput(observed.Key)
	if err != nil || !exists {
		return false, err
	}
	if expected.Revision != observed.Revision || expected.Found != observed.Found {
		return false, nil
	}
	proofs[observed.Key] = expected
	return true, nil
}

func (r *incrementalRenderSession) collectLateResourceProofs(
	ctx context.Context,
	proofs map[incremental.InputKey]incremental.Input,
) (
	currentRoots map[string]stores.ReadSnapshot,
	deltaVerified map[incremental.InputKey]struct{},
	complete bool,
	err error,
) {
	r.mu.Lock()
	cursors := mapsCloneCursors(r.cursors)
	membershipPins := mapsCloneCursors(r.membershipPins)
	r.mu.Unlock()
	aliases, exact := resourceProofAliases(cursors, membershipPins, proofs)
	if !exact {
		return nil, nil, false, nil
	}
	currentRoots = make(map[string]stores.ReadSnapshot, len(aliases))
	deltaVerified = make(map[incremental.InputKey]struct{})
	for _, alias := range aliases {
		if err := ctx.Err(); err != nil {
			return nil, nil, false, err
		}
		current, complete, err := r.collectLateResourceProofForAlias(
			ctx, alias, cursors, membershipPins, proofs, deltaVerified,
		)
		if err != nil || !complete {
			return nil, nil, complete, err
		}
		currentRoots[alias] = current
	}
	return currentRoots, deltaVerified, true, nil
}

func resourceProofAliases(
	cursors, membershipPins map[string]incrementalStoreCursor,
	proofs map[incremental.InputKey]incremental.Input,
) ([]string, bool) {
	aliasSet := make(map[string]struct{}, len(cursors)+len(membershipPins)+len(proofs))
	for alias := range cursors {
		aliasSet[alias] = struct{}{}
	}
	for alias := range membershipPins {
		aliasSet[alias] = struct{}{}
	}
	for key := range proofs {
		spec, ok := parseResourceInputKey(key)
		if !ok {
			return nil, false
		}
		aliasSet[spec.resourceType] = struct{}{}
	}
	aliases := make([]string, 0, len(aliasSet))
	for alias := range aliasSet {
		aliases = append(aliases, alias)
	}
	slices.Sort(aliases)
	return aliases, true
}

func (r *incrementalRenderSession) collectLateResourceProofForAlias(
	ctx context.Context,
	alias string,
	cursors, membershipPins map[string]incrementalStoreCursor,
	proofs map[incremental.InputKey]incremental.Input,
	deltaVerified map[incremental.InputKey]struct{},
) (stores.ReadSnapshot, bool, error) {
	current, pinned, err := pinStoreSnapshot(r.baseStores[alias])
	if err != nil {
		return nil, false, err
	}
	original := r.baseSnapshots[alias]
	if !pinned || current == nil || original == nil || current.RevisionSource() != original.RevisionSource() {
		return nil, false, nil
	}
	deltas, changes, exact := r.lateResourceIdentityDeltas(alias, original, current, cursors, membershipPins)
	if !exact {
		return nil, false, nil
	}
	if len(r.overlayChanges[alias]) > 0 {
		return r.collectLateOverlayProofForAlias(
			ctx, alias, original, current, deltas, changes, membershipPins, proofs,
		)
	}
	if _, verifyFull := membershipPins[alias]; verifyFull {
		same, err := deltas.sameScopeSemantics(ctx, original, current, &resourceInputSpec{
			resourceType: alias,
			scope:        resourceInputList,
		})
		if err != nil || !same {
			return nil, same, err
		}
	}
	affected, complete, err := r.collectAffectedResourceProofs(ctx, alias, changes, proofs)
	if err != nil || !complete {
		return nil, complete, err
	}
	for _, key := range sortedResourceInputSpecs(affected) {
		spec := affected[key]
		same, err := deltas.sameScopeSemantics(ctx, original, current, &spec)
		if err != nil || !same {
			return nil, same, err
		}
		deltaVerified[key] = struct{}{}
	}
	return current, true, nil
}

func (r *incrementalRenderSession) collectLateOverlayProofForAlias(
	ctx context.Context,
	alias string,
	originalBase stores.ReadSnapshot,
	currentBase stores.ReadSnapshot,
	deltas *resourceIdentityDeltas,
	changes []stores.RevisionChange,
	membershipPins map[string]incrementalStoreCursor,
	proofs map[incremental.InputKey]incremental.Input,
) (stores.ReadSnapshot, bool, error) {
	if r.state == nil || r.state.config == nil {
		return nil, false, nil
	}
	resourceConfig, configured := r.state.config.WatchedResources[alias]
	if !configured {
		return nil, false, nil
	}
	original := r.renderSnapshots[alias]
	if original == nil || original.RevisionSource() != originalBase.RevisionSource() {
		return nil, false, nil
	}
	current, err := rebaseIncrementalOverlaySnapshot(
		ctx,
		resourceConfig.IndexBy,
		currentBase,
		r.overlayChanges[alias],
	)
	if err != nil {
		return nil, false, err
	}
	if _, verifyMembership := membershipPins[alias]; verifyMembership {
		same, err := sameChangedResourceMembership(ctx, original, current, deltas)
		if err != nil || !same {
			return nil, same, err
		}
	}
	_, complete, err := r.collectAffectedResourceProofs(ctx, alias, changes, proofs)
	if err != nil || !complete {
		return nil, complete, err
	}
	return current, true, nil
}

func sameChangedResourceMembership(
	ctx context.Context,
	left stores.ReadSnapshot,
	right stores.ReadSnapshot,
	deltas *resourceIdentityDeltas,
) (bool, error) {
	if deltas == nil {
		return false, nil
	}
	for _, delta := range deltas.ordered {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		_, leftFound, err := readIncrementalSnapshotIdentity(
			ctx, left, delta.key.namespace, delta.key.name,
		)
		if err != nil {
			return false, err
		}
		_, rightFound, err := readIncrementalSnapshotIdentity(
			ctx, right, delta.key.namespace, delta.key.name,
		)
		if err != nil {
			return false, err
		}
		if leftFound != rightFound {
			return false, nil
		}
	}
	return true, nil
}

func (r *incrementalRenderSession) lateResourceIdentityDeltas(
	alias string,
	original, current stores.ReadSnapshot,
	cursors, membershipPins map[string]incrementalStoreCursor,
) (*resourceIdentityDeltas, []stores.RevisionChange, bool) {
	cursor, tracked := cursors[alias]
	if !tracked {
		cursor, tracked = membershipPins[alias]
	}
	if !tracked {
		cursor = incrementalStoreCursor{source: original.RevisionSource(), sequence: original.Sequence()}
	}
	journal, supported := r.baseStores[alias].(stores.ExactRevisionJournal)
	if !supported || cursor.source != original.RevisionSource() ||
		journal.ExactRevisionJournalSource() != cursor.source || cursor.sequence != original.Sequence() ||
		cursor.source != current.RevisionSource() {
		return nil, nil, false
	}
	changes, complete := journalChangesThrough(journal, cursor.sequence, current.Sequence())
	if !complete {
		return nil, nil, false
	}
	deltas, exact := newResourceIdentityDeltas(changes)
	if !exact {
		return nil, nil, false
	}
	return deltas, changes, true
}

func (r *incrementalRenderSession) collectAffectedResourceProofs(
	ctx context.Context,
	alias string,
	changes []stores.RevisionChange,
	proofs map[incremental.InputKey]incremental.Input,
) (affected map[incremental.InputKey]resourceInputSpec, complete bool, err error) {
	affected, exact, err := r.affectedResourceInputs(alias, changes, proofs)
	if err != nil || !exact {
		return nil, exact, err
	}
	for key, spec := range affected {
		if _, exists := proofs[key]; exists {
			continue
		}
		expected, exists, err := r.expectedResourceInput(ctx, alias, key, &spec)
		if err != nil || !exists {
			return nil, false, err
		}
		proofs[key] = expected
	}
	return affected, true, nil
}

func (r *incrementalRenderSession) expectedResourceInput(
	ctx context.Context,
	alias string,
	key incremental.InputKey,
	spec *resourceInputSpec,
) (incremental.Input, bool, error) {
	if r.graphSession != nil {
		expected, exists, err := r.graphSession.ExactInput(key)
		if err == nil {
			return expected, exists, nil
		}
		if !errors.Is(err, incremental.ErrCommitConflict) && !errors.Is(err, incremental.ErrSessionClosed) {
			return incremental.Input{}, false, err
		}
	}
	expected, err := readResourceSnapshotInput(ctx, r.renderSnapshots[alias], spec)
	if errors.Is(err, stores.ErrSnapshotChanged) {
		return incremental.Input{}, false, nil
	}
	return expected, err == nil, err
}

type resourceIdentityDeltaKey struct {
	namespace string
	name      string
}

type resourceIdentityDeltaValue struct {
	found   bool
	encoded []byte
}

type resourceIdentityDelta struct {
	key     resourceIdentityDeltaKey
	oldKeys []string
	newKeys []string
	loaded  bool
	before  resourceIdentityDeltaValue
	after   resourceIdentityDeltaValue
}

type resourceIdentityDeltas struct {
	ordered []*resourceIdentityDelta
}

func newResourceIdentityDeltas(changes []stores.RevisionChange) (*resourceIdentityDeltas, bool) {
	byIdentity := make(map[resourceIdentityDeltaKey]*resourceIdentityDelta, len(changes))
	for index := range changes {
		change := &changes[index]
		if change.Name == "" {
			return nil, false
		}
		key := resourceIdentityDeltaKey{namespace: change.Namespace, name: change.Name}
		delta, exists := byIdentity[key]
		if !exists {
			delta = &resourceIdentityDelta{
				key:     key,
				oldKeys: slices.Clone(change.OldKeys),
			}
			byIdentity[key] = delta
		} else if !slices.Equal(delta.newKeys, change.OldKeys) {
			return nil, false
		}
		delta.newKeys = slices.Clone(change.NewKeys)
	}
	ordered := make([]*resourceIdentityDelta, 0, len(byIdentity))
	for _, delta := range byIdentity {
		ordered = append(ordered, delta)
	}
	slices.SortFunc(ordered, func(left, right *resourceIdentityDelta) int {
		if compared := strings.Compare(left.key.namespace, right.key.namespace); compared != 0 {
			return compared
		}
		return strings.Compare(left.key.name, right.key.name)
	})
	return &resourceIdentityDeltas{ordered: ordered}, true
}

func (d *resourceIdentityDeltas) sameScopeSemantics(
	ctx context.Context,
	original stores.ReadSnapshot,
	current stores.ReadSnapshot,
	spec *resourceInputSpec,
) (bool, error) {
	for _, delta := range d.ordered {
		same, err := delta.sameScopeSemantics(ctx, original, current, spec)
		if err != nil {
			if errors.Is(err, stores.ErrSnapshotChanged) {
				return false, nil
			}
			return false, err
		}
		if !same {
			return false, nil
		}
	}
	return true, nil
}

func (d *resourceIdentityDelta) sameScopeSemantics(
	ctx context.Context,
	original stores.ReadSnapshot,
	current stores.ReadSnapshot,
	spec *resourceInputSpec,
) (bool, error) {
	if spec.scope == resourceInputIdentity &&
		(spec.namespace != d.key.namespace || spec.name != d.key.name) {
		return true, nil
	}
	oldIncluded, newIncluded := resourceDeltaScopeMembership(d, spec)
	if !oldIncluded && !newIncluded {
		return true, nil
	}
	if err := d.load(ctx, original, current); err != nil {
		return false, err
	}
	if d.before.found != (len(d.oldKeys) > 0) || d.after.found != (len(d.newKeys) > 0) {
		return false, nil
	}
	oldIncluded = oldIncluded && d.before.found
	newIncluded = newIncluded && d.after.found
	if oldIncluded != newIncluded {
		return false, nil
	}
	return !oldIncluded || bytes.Equal(d.before.encoded, d.after.encoded), nil
}

func resourceDeltaScopeMembership(
	delta *resourceIdentityDelta,
	spec *resourceInputSpec,
) (oldIncluded, newIncluded bool) {
	switch spec.scope {
	case resourceInputList, resourceInputIdentity:
		return len(delta.oldKeys) > 0, len(delta.newKeys) > 0
	case resourceInputGet:
		return resourceKeysMatch(delta.oldKeys, spec.keys), resourceKeysMatch(delta.newKeys, spec.keys)
	default:
		return false, false
	}
}

func resourceKeysMatch(projected, query []string) bool {
	return len(query) > 0 && len(query) <= len(projected) && slices.Equal(projected[:len(query)], query)
}

func (d *resourceIdentityDelta) load(
	ctx context.Context,
	original stores.ReadSnapshot,
	current stores.ReadSnapshot,
) error {
	if d.loaded {
		return nil
	}
	before, beforeFound, err := readIncrementalSnapshotIdentity(
		ctx, original, d.key.namespace, d.key.name,
	)
	if err != nil {
		return err
	}
	after, afterFound, err := readIncrementalSnapshotIdentity(
		ctx, current, d.key.namespace, d.key.name,
	)
	if err != nil {
		return err
	}
	d.before.found = beforeFound
	d.after.found = afterFound
	if beforeFound {
		d.before.encoded, err = encodeResourceValue(before)
		if err != nil {
			return err
		}
	}
	if afterFound {
		d.after.encoded, err = encodeResourceValue(after)
		if err != nil {
			return err
		}
	}
	d.loaded = true
	return nil
}

func (r *incrementalRenderSession) affectedResourceInputs(
	alias string,
	changes []stores.RevisionChange,
	proofs map[incremental.InputKey]incremental.Input,
) (affected map[incremental.InputKey]resourceInputSpec, exact bool, err error) {
	affected = map[incremental.InputKey]resourceInputSpec{}
	for index := range changes {
		change := &changes[index]
		if change.Name == "" {
			return nil, false, nil
		}
		candidates := resourceInputCandidates(alias, change)
		for candidateIndex := range candidates {
			candidate := &candidates[candidateIndex]
			key := resourceInputKey(candidate)
			if _, proved := proofs[key]; proved {
				affected[key] = *candidate
			}
			known, exists, catalogErr := r.catalogGet(key)
			if catalogErr != nil {
				return nil, false, catalogErr
			}
			if !exists {
				continue
			}
			retained, err := r.resourceInputRetained(key)
			if err != nil {
				return nil, false, err
			}
			if retained {
				affected[key] = known
			}
		}
	}
	return affected, true, nil
}

func sortedResourceInputSpecs(
	inputs map[incremental.InputKey]resourceInputSpec,
) []incremental.InputKey {
	keys := make([]incremental.InputKey, 0, len(inputs))
	for key := range inputs {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(left, right incremental.InputKey) int {
		return strings.Compare(left.Opaque(), right.Opaque())
	})
	return keys
}

func resourceInputCandidates(alias string, change *stores.RevisionChange) []resourceInputSpec {
	candidates := []resourceInputSpec{
		{resourceType: alias, scope: resourceInputList},
		{
			resourceType: alias,
			scope:        resourceInputIdentity,
			namespace:    change.Namespace,
			name:         change.Name,
		},
	}
	for _, keys := range [][]string{change.OldKeys, change.NewKeys} {
		for count := 1; count <= len(keys); count++ {
			candidates = append(candidates, resourceInputSpec{
				resourceType: alias,
				scope:        resourceInputGet,
				keys:         slices.Clone(keys[:count]),
			})
		}
	}
	return candidates
}

func (r *incrementalRenderSession) resourceInputRetained(key incremental.InputKey) (bool, error) {
	if r.graphSession == nil {
		return true, nil
	}
	retained, err := r.graphSession.HasInputDependents(key)
	if errors.Is(err, incremental.ErrCommitConflict) || errors.Is(err, incremental.ErrSessionClosed) {
		return true, nil
	}
	return retained, err
}

func (r *incrementalRenderSession) verifyCurrentResourceInputs(
	ctx context.Context,
	proofs map[incremental.InputKey]incremental.Input,
	currentRoots map[string]stores.ReadSnapshot,
	deltaVerified map[incremental.InputKey]struct{},
) (bool, error) {
	for _, key := range sortedInputKeys(proofs) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if _, verified := deltaVerified[key]; verified {
			continue
		}
		spec, ok := parseResourceInputKey(key)
		if !ok {
			return false, nil
		}
		current, complete, err := r.currentResourceRoot(&spec, currentRoots)
		if err != nil || !complete {
			return complete, err
		}
		verified, err := verifyCurrentResourceInput(ctx, current, &spec, proofs[key])
		if err != nil || !verified {
			return verified, err
		}
	}
	return true, nil
}

func sortedInputKeys(proofs map[incremental.InputKey]incremental.Input) []incremental.InputKey {
	keys := make([]incremental.InputKey, 0, len(proofs))
	for key := range proofs {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(left, right incremental.InputKey) int {
		return strings.Compare(left.Opaque(), right.Opaque())
	})
	return keys
}

func (r *incrementalRenderSession) currentResourceRoot(
	spec *resourceInputSpec,
	currentRoots map[string]stores.ReadSnapshot,
) (stores.ReadSnapshot, bool, error) {
	if current := currentRoots[spec.resourceType]; current != nil {
		return current, true, nil
	}
	current, supported, err := pinStoreSnapshot(r.baseStores[spec.resourceType])
	if err != nil {
		return nil, false, err
	}
	original := r.baseSnapshots[spec.resourceType]
	if !supported || current == nil || original == nil || current.RevisionSource() != original.RevisionSource() {
		return nil, false, nil
	}
	currentRoots[spec.resourceType] = current
	return current, true, nil
}

func verifyCurrentResourceInput(
	ctx context.Context,
	current stores.ReadSnapshot,
	spec *resourceInputSpec,
	expected incremental.Input,
) (bool, error) {
	revision, err := resourceSnapshotRevision(current, spec)
	if err != nil || revision == "" {
		return false, err
	}
	if storeRevision(current.RevisionSource(), revision) == expected.Revision {
		return true, nil
	}
	actual, err := readResourceSnapshotInput(ctx, current, spec)
	if errors.Is(err, stores.ErrSnapshotChanged) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return sameIncrementalInput(actual, expected), nil
}

func sameIncrementalInput(left, right incremental.Input) bool {
	return left.Key == right.Key && left.Found == right.Found && bytes.Equal(left.Value, right.Value)
}

// exactCycleRootReuser is set when the previous cycle's external inputs
// matched but the cycle as a whole could not be replayed, so single roots
// that observed nothing new can still hand back their previous output.
func (r *incrementalRenderSession) exactCycleRootReuser() *exactCycleRootReuser {
	if r == nil {
		return nil
	}
	return r.rootReuser
}
