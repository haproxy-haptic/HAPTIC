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

package renderer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
	"gopkg.in/yaml.v3"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/httpstore"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/names"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercycle"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderartifact"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderoutput"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
	purehttpstore "github.com/haproxy-haptic/HAPTIC/pkg/httpstore"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

// RenderInputTransaction finalizes external inputs used by one render.
type RenderInputTransaction interface {
	// HasCandidates reports whether this render is the first to accept some
	// external content, which is what the commit gate keys on.
	HasCandidates() bool
	Commit(context.Context) error
	Abort()
}

// RenderResult contains the output of a render operation.
type RenderResult struct {
	// HTTPObservations records fresh accepted reads independently of transaction wrappers.
	HTTPObservations []purehttpstore.ObservationToken

	// CycleSnapshot binds the output and every effect from this render.
	CycleSnapshot *rendercycle.Snapshot

	// OutputSnapshot binds the exact config, plan, and auxiliary artifacts.
	OutputSnapshot *renderoutput.Snapshot

	// HAProxyConfig is the rendered HAProxy configuration.
	HAProxyConfig string

	// AuxiliaryFiles contains all rendered auxiliary files (maps, certs, general).
	// Production renders leave it nil and use AuxiliaryFileSnapshot.
	AuxiliaryFiles *dataplane.AuxiliaryFiles

	// AuxiliaryFileSnapshot is the authenticated immutable production representation.
	AuxiliaryFileSnapshot *renderartifact.Snapshot

	// ContentChecksum covers HAProxyConfig and every auxiliary-file identifier and byte.
	ContentChecksum string

	// Plan is the structure the templates declared about this render: the
	// ordered sections of the config, the backend records behind them, the map
	// entries and the file set.
	Plan *renderplan.Plan

	// PlanID identifies the plan — the digest downstream consumers compare on.
	PlanID string

	// StatusPatches is the detached compatibility representation.
	// Production renders leave it nil and use StatusPatchSnapshot.
	StatusPatches []templating.StatusPatch

	// StatusPatchSnapshot carries the same patches without materializing payload maps.
	StatusPatchSnapshot *templating.StatusPatchSnapshot

	// Events contains Kubernetes Events templates asked to emit via recordEvent()
	// (e.g. a RouteConflict Warning on an Ingress whose route lost to an older
	// one). Resource-agnostic — each carries its own apiVersion/kind/namespace/name.
	// Production renders leave it nil and use EventSnapshot.
	Events []templating.RenderedEvent

	// EventSnapshot is the authenticated immutable production representation.
	EventSnapshot *templating.RenderedEventSnapshot

	// RenderedResources contains full Kubernetes resources the templates declared
	// the controller should own and reconcile (e.g. an auxiliary Service or other
	// object a template emits alongside the HAProxy config). The applier compares
	// each against the last-applied checksum and skips unchanged entries to avoid
	// hammering the API server.
	// Production renders leave it nil and use RenderedResourceSnapshot.
	RenderedResources []templating.RenderedResource

	// RenderedResourceSnapshot is the authenticated immutable production representation.
	RenderedResourceSnapshot *templating.RenderedResourceSnapshot

	// DurationMs is the total render duration in milliseconds.
	DurationMs int64

	// CacheState is "warm" when this render had a graph to build on, "cold" when
	// it re-evaluated everything, and "replay" when it reused the previous output.
	CacheState string

	// CacheBuildMs is what the most recent completed cache build cost, or 0 while
	// none has completed.
	CacheBuildMs int64

	// RootsReused counts the auxiliary roots that handed back their previous
	// output instead of executing, because they observed nothing new.
	RootsReused int

	// IncludeStats holds per-snippet render counts/timing for the main template.
	// Populated only when the engine was built with profiling enabled (nil in
	// production); consumed by the browser playground's render-trace view.
	IncludeStats []templating.IncludeStats

	// AuxFileCount is the total number of auxiliary files.
	AuxFileCount int

	// InputTransaction owns render-local inputs until the full pipeline decides their fate.
	InputTransaction RenderInputTransaction

	renderCachePublication *rendercontext.PreparedRenderCachePublication
	planIdentity           *rendercontext.RenderPlanIdentity
}

// MaterializePlan returns a caller-owned plan from the authenticated output root.
func (r *RenderResult) MaterializePlan() (*renderplan.Plan, error) {
	if r == nil {
		return nil, errors.New("render result is nil")
	}
	output := r.OutputSnapshot
	if r.CycleSnapshot != nil {
		var err error
		output, err = r.CycleSnapshot.OutputSnapshot()
		if err != nil {
			return nil, fmt.Errorf("reading render cycle output: %w", err)
		}
	}
	if output != nil {
		snapshot, err := output.PlanSnapshot()
		if err != nil {
			return nil, fmt.Errorf("reading render output plan: %w", err)
		}
		plan, err := snapshot.LegacyCopy()
		if err != nil {
			return nil, fmt.Errorf("materializing render output plan: %w", err)
		}
		return plan, nil
	}
	return r.Plan.Clone(), nil
}

// MaterializeAuxiliaryFiles returns a caller-isolated compatibility view.
func (r *RenderResult) MaterializeAuxiliaryFiles() (*dataplane.AuxiliaryFiles, error) {
	if r == nil {
		return nil, errors.New("render result is nil")
	}
	output := r.OutputSnapshot
	if r.CycleSnapshot != nil {
		var err error
		output, err = r.CycleSnapshot.OutputSnapshot()
		if err != nil {
			return nil, fmt.Errorf("reading render cycle output: %w", err)
		}
	}
	if output != nil {
		snapshot, err := output.ArtifactSnapshot()
		if err != nil {
			return nil, fmt.Errorf("reading render output artifacts: %w", err)
		}
		return dataplane.MaterializeAuxiliaryFileSnapshot(snapshot)
	}
	if r.AuxiliaryFileSnapshot != nil {
		if r.AuxiliaryFiles != nil {
			return nil, errors.New("render result carries both mutable and immutable auxiliary files")
		}
		return dataplane.MaterializeAuxiliaryFileSnapshot(r.AuxiliaryFileSnapshot)
	}
	return dataplane.CloneAuxiliaryFiles(r.AuxiliaryFiles), nil
}

// MaterializeStatusPatches returns a caller-isolated compatibility view.
func (r *RenderResult) MaterializeStatusPatches() ([]templating.StatusPatch, error) {
	if r == nil {
		return nil, errors.New("render result is nil")
	}
	statusSnapshot := r.StatusPatchSnapshot
	if r.CycleSnapshot != nil {
		var err error
		statusSnapshot, err = r.CycleSnapshot.StatusPatchSnapshot()
		if err != nil {
			return nil, fmt.Errorf("reading render cycle status patches: %w", err)
		}
	}
	if statusSnapshot != nil {
		if len(r.StatusPatches) > 0 {
			return nil, errors.New("render result carries both mutable and immutable status patches")
		}
		return statusSnapshot.Patches()
	}
	if r.StatusPatches == nil {
		return nil, nil
	}
	projection, err := templating.NewStatusPatchProjection(r.StatusPatches)
	if err != nil {
		return nil, fmt.Errorf("materializing status patches: %w", err)
	}
	replay, err := projection.PrepareReplay()
	if err != nil {
		return nil, fmt.Errorf("materializing status patches: %w", err)
	}
	collector := templating.NewStatusPatchCollector()
	if err := collector.ReplayProjections([]*templating.StatusPatchProjectionReplay{replay}); err != nil {
		return nil, fmt.Errorf("materializing status patches: %w", err)
	}
	return collector.Patches()
}

// MaterializeEvents returns a caller-isolated compatibility view.
func (r *RenderResult) MaterializeEvents() ([]templating.RenderedEvent, error) {
	if r == nil {
		return nil, errors.New("render result is nil")
	}
	eventSnapshot := r.EventSnapshot
	if r.CycleSnapshot != nil {
		var err error
		eventSnapshot, err = r.CycleSnapshot.RenderedEventSnapshot()
		if err != nil {
			return nil, fmt.Errorf("reading render cycle events: %w", err)
		}
	}
	if eventSnapshot != nil {
		if len(r.Events) > 0 {
			return nil, errors.New("render result carries both mutable and immutable events")
		}
		return eventSnapshot.Events()
	}
	return slices.Clone(r.Events), nil
}

// MaterializeRenderedResources returns a caller-isolated compatibility view.
func (r *RenderResult) MaterializeRenderedResources() ([]templating.RenderedResource, error) {
	if r == nil {
		return nil, errors.New("render result is nil")
	}
	resourceSnapshot := r.RenderedResourceSnapshot
	if r.CycleSnapshot != nil {
		var err error
		resourceSnapshot, err = r.CycleSnapshot.RenderedResourceSnapshot()
		if err != nil {
			return nil, fmt.Errorf("reading render cycle resources: %w", err)
		}
	}
	if resourceSnapshot != nil {
		if len(r.RenderedResources) > 0 {
			return nil, errors.New("render result carries both mutable and immutable rendered resources")
		}
		return resourceSnapshot.Resources()
	}
	if r.RenderedResources == nil {
		return nil, nil
	}
	collector := templating.NewRenderedResourceCollector()
	for index := range r.RenderedResources {
		resource := &r.RenderedResources[index]
		if err := collector.RegisterWithCreateOnlyFields(
			resource.APIVersion, resource.Kind, resource.Namespace, resource.Name, resource.Object,
			resource.CreateOnlyFields,
		); err != nil {
			return nil, fmt.Errorf("materializing rendered resource %d: %w", index, err)
		}
	}
	return collector.Resources(), nil
}

// RenderService is a pure service that transforms stores into HAProxy configuration.
//
// This service uses absolute paths from the config's Dataplane settings to ensure
// rendered configs reference files at the correct locations where the agent
// stores auxiliary files.
//
// Resources in stores are already converted (floats to ints) at storage time,
// so the service simply passes through store data without additional processing.
type RenderService struct {
	engine                      templating.Engine
	config                      *config.Config
	pathResolver                *templating.PathResolver
	logger                      *slog.Logger
	admissionSlots              chan struct{}
	admissionSlotWaitObserver   func(time.Duration)
	backgroundWorkers           *backgroundWorkerGate
	coldRenders                 *coldRenderGate
	incremental                 *incrementalRenderState
	mainDocumentCache           *rendercontext.RenderDocumentCache
	planTokenAuthority          *rendercontext.PlanTokenAuthority
	planMemo                    *rendercontext.PlanMemo
	exactCycleProgram           *templating.ExactCycleReplayProgram
	extraContextMu              sync.Mutex
	extraContext                map[string]any
	extraContextCertificate     *templating.IncrementalImmutableCertificate
	exactCycleCandidate         *exactCycleCandidate
	skipCurrentConfigProjection bool
	skipCurrentFilesProjection  bool

	// namespaceScopes rejects lookups outside a watch's namespaces.
	namespaceScopes func(string) *rendercontext.NamespaceScope

	// renderTimeout is the maximum time allowed for rendering a single template.
	renderTimeout time.Duration

	// capsMu guards capabilities: what the fleet's HAProxy supports. The local
	// probe seeds it, discovery replaces it with the fleet minimum, and
	// admission renders read it off the reconcile goroutine.
	capsMu       sync.RWMutex
	capabilities dataplane.Capabilities

	// planMu guards both plans below. ackedPlan — the newest plan the fleet
	// confirmed running — is the source for the next render's `currentConfig`;
	// lastPlan, the newest reconcile render's plan, stands in until the first ACK.
	planMu                 sync.Mutex
	ackedPlan              *renderplan.Plan
	lastPlan               *renderplan.Plan
	ackedCurrentConfigRoot *exactCycleCurrentConfigRoot
	lastCurrentConfigRoot  *exactCycleCurrentConfigRoot

	planAuthority              *renderplan.Authority
	planDigestFallbacks        atomic.Uint64
	assemblyReuseMu            sync.Mutex
	assemblyLastReason         *string
	assemblySeenReasons        map[string]bool
	artifactAuthority          *renderartifact.Authority
	outputAuthority            *renderoutput.Authority
	cycleAuthority             *rendercycle.Authority
	lastOutputSnapshot         *renderoutput.Snapshot
	lastCycleSnapshot          *rendercycle.Snapshot
	lastPlanIdentity           *rendercontext.RenderPlanIdentity
	lastRenderCache            *rendercontext.PreparedRenderCachePublication
	nextOutputGeneration       uint64
	publishedOutputGeneration  uint64
	outputGenerationExhausted  bool
	outputReservations         map[uint64]*renderOutputReservation
	committedOutputReservation atomic.Pointer[renderOutputReservation]

	// Optional dependencies for building render context
	haproxyPodStore         stores.Store
	httpStoreComponent      *httpstore.Component
	currentAuxFilesProvider func() map[string]string

	// typedResourceTypes maps watched-resource user-names to the
	// generated Go type produced by pkg/k8s/typegen at iteration
	// start (see pkg/controller/typebootstrap). When non-empty,
	// buildRenderingContext emits one *[]*<generated-struct>
	// top-level context entry per type — the value Scriggo's
	// type-checker pairs with the typed global declared via
	// helpers.NewEngineFromConfigWithOptions.
	//
	// Optional: a nil / empty map means no typed access is
	// available and templates use the untyped resources["<name>"]
	// path as today.
	typedResourceTypes map[string]reflect.Type
}

// RenderServiceConfig contains configuration for creating a RenderService.
type RenderServiceConfig struct {
	// Engine is the template engine to use for rendering.
	Engine templating.Engine

	// Config is the controller configuration.
	Config *config.Config

	// Logger is the structured logger for logging.
	Logger *slog.Logger

	// Capabilities defines HAProxy version capabilities.
	Capabilities dataplane.Capabilities

	// HAProxyPodStore is the store containing HAProxy pods (optional).
	HAProxyPodStore stores.Store

	// HTTPStoreComponent is the HTTP store for dynamic content (optional).
	HTTPStoreComponent *httpstore.Component

	// IncrementalCacheBuildObserver receives optional cold-cache lifecycle notifications.
	IncrementalCacheBuildObserver IncrementalCacheBuildObserver

	// AdmissionSlotWaitObserver, if set, receives how long each admission
	// render waited for its slot, whether or not it got one.
	AdmissionSlotWaitObserver func(time.Duration)

	// CurrentAuxFilesProvider returns the default auxiliary baseline. The
	// Coordinator overrides it with a leader-term snapshot for reconciliation.
	CurrentAuxFilesProvider func() map[string]string

	// ColdRendersRunToCompletion lets a reconcile render that starts without
	// a committed graph run past the render timeout until its context ends,
	// one at a time. Set it only when every reconcile caller's context ends on
	// shutdown or leadership loss; a caller without one, such as the
	// playground, relies on the timeout to stop a template that never returns.
	ColdRendersRunToCompletion bool

	// TypedResourceTypes carries the generated Go types produced
	// by pkg/controller/typebootstrap at iteration start. The
	// renderer emits one *[]*<generated-struct> top-level context
	// entry per type at render time, matching the typed-global
	// declarations the engine constructor received.
	//
	// Optional. nil or empty means typed-resource access isn't
	// available and templates fall back to the untyped
	// resources["<name>"] path.
	TypedResourceTypes map[string]reflect.Type
}

// NewRenderService creates a new RenderService.
//
// The service uses relative paths derived from the config's Dataplane settings.
// The directory names are extracted using path.Base() to get just the final
// directory component (e.g., /etc/haproxy/maps → maps).
//
// These relative paths are resolved by HAProxy using the `default-path origin <baseDir>`
// directive in the global section, which makes HAProxy resolve paths from the specified
// base directory regardless of where the config file is located. This works for:
//   - Local validation: ValidationService replaces baseDir with temp directory
//   - Deployment: baseDir points to where the agent writes files (e.g., /etc/haproxy)
func NewRenderService(cfg *RenderServiceConfig) *RenderService {
	// Create path resolver with relative paths derived from config.
	// Use path.Base() to extract just the directory name from absolute paths.
	// Use path.Dir() to get the base directory from any absolute path.
	// The slash-only path package is used (not filepath) because these are
	// HAProxy target paths, always slash-separated regardless of host OS.
	sslDir := path.Base(cfg.Config.Dataplane.SSLCertsDir)
	generalDir := path.Base(cfg.Config.Dataplane.GeneralStorageDir)

	// BaseDir is the parent of the auxiliary directories (e.g., /etc/haproxy).
	// This is used with "default-path origin" to resolve relative paths.
	baseDir := path.Dir(cfg.Config.Dataplane.MapsDir)

	// CRT-list files are always stored in general file storage, regardless of HAProxy version.
	// This is because the native CRT-list API (POST ssl_crt_lists) triggers a reload without
	// supporting skip_reload, while general file storage returns 201 without triggering reloads.
	// See: pkg/dataplane/auxiliaryfiles/crtlist.go
	crtListDir := generalDir

	pathResolver := &templating.PathResolver{
		BaseDir:    baseDir,
		MapsDir:    path.Base(cfg.Config.Dataplane.MapsDir),
		SSLDir:     sslDir,
		CRTListDir: crtListDir,
		GeneralDir: generalDir,
	}
	mainDocumentCache, _ := rendercontext.NewRenderDocumentCache(cfg.Engine)
	planTokenAuthority := rendercontext.NewPlanTokenAuthority()

	planAuthority := renderplan.NewAuthority()
	artifactAuthority := renderartifact.NewAuthority()
	outputAuthority, err := renderoutput.NewAuthority(planAuthority, artifactAuthority)
	if err != nil {
		panic(fmt.Sprintf("creating render output authority: %v", err))
	}
	cycleAuthority, err := rendercycle.NewAuthority(outputAuthority)
	if err != nil {
		panic(fmt.Sprintf("creating render cycle authority: %v", err))
	}
	service := &RenderService{
		engine:                      cfg.Engine,
		config:                      cfg.Config,
		pathResolver:                pathResolver,
		logger:                      cfg.Logger,
		admissionSlots:              make(chan struct{}, admissionRenderSlots(runtime.GOMAXPROCS(0))),
		admissionSlotWaitObserver:   cfg.AdmissionSlotWaitObserver,
		backgroundWorkers:           newBackgroundWorkerGate(runtime.GOMAXPROCS(0)),
		mainDocumentCache:           mainDocumentCache,
		planTokenAuthority:          planTokenAuthority,
		planMemo:                    rendercontext.NewPlanMemo(),
		planAuthority:               planAuthority,
		artifactAuthority:           artifactAuthority,
		outputAuthority:             outputAuthority,
		cycleAuthority:              cycleAuthority,
		renderTimeout:               cfg.Config.TemplatingSettings.GetRenderTimeout(),
		capabilities:                cfg.Capabilities,
		haproxyPodStore:             cfg.HAProxyPodStore,
		httpStoreComponent:          cfg.HTTPStoreComponent,
		currentAuxFilesProvider:     cfg.CurrentAuxFilesProvider,
		typedResourceTypes:          cfg.TypedResourceTypes,
		skipCurrentConfigProjection: engineProvesGlobalUnused(cfg.Engine, "currentConfig"),
		skipCurrentFilesProjection:  engineProvesGlobalUnused(cfg.Engine, "currentFiles"),
		namespaceScopes:             rendercontext.NamespaceScopes(cfg.Config),
	}
	service.incremental = newIncrementalRenderState(cfg.Config, cfg.Engine)
	if service.incremental != nil {
		service.incremental.scopeFor = service.namespaceScopes
		service.incremental.cacheBuildObserver = cfg.IncrementalCacheBuildObserver
		if cfg.ColdRendersRunToCompletion {
			service.coldRenders = newColdRenderGate(service.IncrementalGraphWarm)
			service.incremental.coldCommitAwaitsGraph = true
		}
	}
	if preparer, ok := cfg.Engine.(exactCycleReplayPreparer); ok {
		program, prepareErr := preparer.PrepareExactCycleReplay(exactCycleRootEntryPoints(cfg.Config))
		if prepareErr == nil {
			service.exactCycleProgram = program
		} else if service.logger != nil {
			service.logger.Debug("Exact cycle replay is unavailable", "reason", prepareErr)
		}
	}
	return service
}

// withRenderTimeout bounds a render by the configured timeout, if any.
func (s *RenderService) withRenderTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.renderTimeout > 0 {
		return context.WithTimeout(ctx, s.renderTimeout)
	}
	return ctx, func() {}
}

// HTTPStoreComponent returns the HTTP store this service renders against, or nil.
func (s *RenderService) HTTPStoreComponent() *httpstore.Component {
	return s.httpStoreComponent
}

// IncrementalGraphWarm reports whether the next reconcile render builds on a
// committed graph: one has been published and no cache build is pending. A
// service without incremental snippets has nothing to warm and reports true.
func (s *RenderService) IncrementalGraphWarm() bool {
	if s.incremental == nil {
		return true
	}
	s.incremental.mu.Lock()
	defer s.incremental.mu.Unlock()
	return s.incremental.graph != nil && s.incremental.graph.Generation() != 0 && !s.incremental.cachePending
}

// incrementalCacheFigures reports whether this render built on a graph and what
// the last completed cache build cost. A fleet steadily reporting "cold" pays
// full render cost on every reconcile.
func (s *RenderService) incrementalCacheFigures(
	transaction RenderInputTransaction,
) (cacheState string, cacheBuildMs int64) {
	if s.incremental == nil {
		return "cold", 0
	}
	state := "warm"
	if combined, ok := transaction.(*combinedRenderInputTransaction); ok {
		if _, session, _ := combined.references(); session != nil && (session.cold || session.exactCycleFullCold()) {
			state = "cold"
		}
	}
	return state, s.incremental.cache.LastBuildMs()
}

// Render transforms the stores into HAProxy configuration.
//
// Parameters:
//   - ctx: Context for cancellation
//   - provider: StoreProvider for accessing resource stores
//
// Returns:
//   - RenderResult containing the rendered configuration and auxiliary files
//   - Error if rendering fails
func (s *RenderService) Render(ctx context.Context, provider stores.StoreProvider, mode rendercontext.RenderMode, extraOpts ...rendercontext.Option) (*RenderResult, error) {
	startTime := time.Now()
	cold, err := s.claimColdRender(ctx, mode)
	if err != nil {
		return nil, err
	}
	return cold.holdUntilSettled(s.render(ctx, provider, mode, startTime, cold.cold(), extraOpts...))
}

// render runs a cold reconcile render without the render timeout: its cost is
// the whole resource set, so a deadline it exceeds once it exceeds on every
// retry and the graph never commits (#285). Its context still ends with the
// caller's leadership term or shutdown.
func (s *RenderService) render(
	ctx context.Context,
	provider stores.StoreProvider,
	mode rendercontext.RenderMode,
	startTime time.Time,
	cold bool,
	extraOpts ...rendercontext.Option,
) (*RenderResult, error) {
	if cold {
		defer s.warnWhenColdRenderOutlivesTimeout(startTime)()
	} else {
		var cancel context.CancelFunc
		ctx, cancel = s.withRenderTimeout(ctx)
		defer cancel()
	}
	if mode == rendercontext.RenderModeAdmission {
		release, err := s.acquireAdmissionSlot(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
		if s.backgroundWorkers != nil {
			defer s.backgroundWorkers.admissionRendering()()
		}
	} else if s.backgroundWorkers != nil {
		ctx = templating.WithWorkerGate(ctx, s.backgroundWorkers)
	}
	attemptInputs, err := s.captureRenderAttemptInputs(mode)
	if err != nil {
		return nil, err
	}

	retryInputs := &renderRetryInputs{}
	tryExactReuse := true
	forceCold := false
	for range 3 {
		result, restart, err := s.renderAttemptOnCurrentBase(
			ctx, provider, mode, startTime, forceCold, tryExactReuse, attemptInputs, retryInputs, extraOpts...,
		)
		if errors.Is(err, errExactCycleInvalidCandidateRetry) && tryExactReuse {
			tryExactReuse = false
			forceCold = true
			attemptInputs.renderCache = nil
			continue
		}
		if errors.Is(err, errExactCycleRetry) && tryExactReuse {
			tryExactReuse = false
			continue
		}
		if err != nil || !restart {
			return result, err
		}
		if forceCold {
			return nil, errors.New("exact cold incremental render requested another restart")
		}
		tryExactReuse = false
		forceCold = true
	}
	return nil, errors.New("render attempt restart limit exceeded")
}

// warnWhenColdRenderOutlivesTimeout logs once a cold render passes the render
// timeout, so a template that never returns stays visible; the returned func
// stops the timer.
func (s *RenderService) warnWhenColdRenderOutlivesTimeout(startTime time.Time) func() {
	if s.renderTimeout <= 0 || s.logger == nil {
		return func() {}
	}
	timer := time.AfterFunc(s.renderTimeout, func() {
		s.logger.Warn("First full render is still running past the render timeout; "+
			"admission and deployments wait for it. If it never finishes, raise the controller's memory and CPU limits",
			"running", time.Since(startTime).Round(100*time.Millisecond), "render_timeout", s.renderTimeout)
	})
	return func() { timer.Stop() }
}

// admissionRenderSlots is how many admission renders may run at once on a
// pod with cpus available: half of them, at least one. Admission requests
// arrive in parallel from every apiserver worker, each render allocates tens
// of megabytes while it runs and needs a CPU of its own, so more in flight
// than this only piles up memory and slows every one of them; the ones that
// wait finish no later than they would have running side by side.
func admissionRenderSlots(cpus int) int {
	return max(1, cpus/2)
}

// acquireAdmissionSlot waits for a slot or for the request to expire; a
// request that expires waiting is refused with the context's error, as one
// that expired rendering would be.
func (s *RenderService) acquireAdmissionSlot(ctx context.Context) (func(), error) {
	if s.admissionSlots == nil {
		return func() {}, nil
	}
	if s.admissionSlotWaitObserver != nil {
		defer func(start time.Time) { s.admissionSlotWaitObserver(time.Since(start)) }(time.Now())
	}
	select {
	case s.admissionSlots <- struct{}{}:
		return func() { <-s.admissionSlots }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for an admission render slot: %w", ctx.Err())
	}
}

// renderBaseMoveLimit bounds how often one render restarts because a commit
// replaced the base it began on. The window is the few instructions between
// copying the base and pinning it, so a second restart in a row means a
// commit storm rather than bad luck.
const renderBaseMoveLimit = 4

// renderAttemptOnCurrentBase runs one attempt, beginning again on the current
// base when a concurrent commit replaced the one it copied. These restarts
// have their own budget: they are not the exact-cycle restarts the caller
// counts.
func (s *RenderService) renderAttemptOnCurrentBase(
	ctx context.Context,
	provider stores.StoreProvider,
	mode rendercontext.RenderMode,
	startTime time.Time,
	forceCold, tryExactReuse bool,
	attemptInputs *renderAttemptInputs,
	retryInputs *renderRetryInputs,
	extraOpts ...rendercontext.Option,
) (*RenderResult, bool, error) {
	var err error
	for range renderBaseMoveLimit {
		var result *RenderResult
		var restart bool
		result, restart, err = s.renderAttempt(
			ctx, provider, mode, startTime, forceCold, tryExactReuse, attemptInputs, retryInputs, extraOpts...,
		)
		if errors.Is(err, purehttpstore.ErrActiveLeaseTokenStale) || errors.Is(err, errIncrementalBaseMoved) {
			continue
		}
		return result, restart, err
	}
	return nil, false, fmt.Errorf("render base moved %d times in a row: %w", renderBaseMoveLimit, err)
}

type renderRetryInputs struct {
	http *httpstore.InputRetrySeed
}

type renderAttemptInputs struct {
	capabilities        dataplane.Capabilities
	currentConfig       *renderplan.CurrentConfig
	currentConfigSource rendercontext.CurrentConfigSource
	currentAuxFiles     rendercontext.CurrentAuxFilesSource
	extraContext        map[string]any
	// extraContextCertificate guards extraContext when the attempt shares the
	// service's copy; nil when the attempt owns a fresh copy.
	extraContextCertificate *templating.IncrementalImmutableCertificate
	runtimeEnvironment      templating.RuntimeEnvironment
	outputGeneration        uint64
	renderCache             *rendercontext.PreparedRenderCachePublication
	exactCycle              *exactCycleCandidate
}

func (s *RenderService) captureRenderAttemptInputs(modes ...rendercontext.RenderMode) (*renderAttemptInputs, error) {
	if len(modes) > 1 {
		return nil, errors.New("capturing render inputs for more than one mode")
	}
	extraContext, certificate, err := s.attemptExtraContext(modes)
	if err != nil {
		return nil, fmt.Errorf("detaching render extra context: %w", err)
	}
	result := &renderAttemptInputs{
		capabilities:            s.currentCapabilities(),
		extraContext:            extraContext,
		extraContextCertificate: certificate,
		runtimeEnvironment:      templating.RuntimeEnvironment{GOMAXPROCS: runtime.GOMAXPROCS(0)},
	}
	s.planMu.Lock()
	if !s.skipCurrentConfigProjection {
		plan := s.ackedPlan
		root := s.ackedCurrentConfigRoot
		if plan == nil {
			plan = s.lastPlan
			root = s.lastCurrentConfigRoot
		}
		if len(modes) == 0 {
			switch {
			case plan != nil:
				current := plan.CurrentConfig()
				result.currentConfig = &current
			case root != nil:
				current, materializeErr := root.materialize()
				if materializeErr != nil {
					s.planMu.Unlock()
					return nil, fmt.Errorf("materializing currentConfig: %w", materializeErr)
				}
				result.currentConfig = &current
			}
		} else {
			result.currentConfigSource = newExactCycleCurrentConfigSource(root)
		}
	}
	result.renderCache = s.lastRenderCache
	result.exactCycle = s.exactCycleCandidate
	if len(modes) == 1 && modes[0] == rendercontext.RenderModeReconcile {
		result.outputGeneration, err = s.reserveOutputGenerationLocked()
	}
	s.planMu.Unlock()
	if err != nil {
		return nil, err
	}
	if !s.skipCurrentFilesProjection {
		if s.currentAuxFilesProvider != nil {
			result.currentAuxFiles = newUnversionedCurrentAuxFilesSource(s.currentAuxFilesProvider)
		} else {
			result.currentAuxFiles = newExactCycleCurrentAuxFilesSource(emptyExactCycleCurrentAuxFilesRoot)
		}
	}
	return result, nil
}

func engineProvesGlobalUnused(engine templating.Engine, name string) bool {
	introspector, ok := engine.(templating.GlobalUsageIntrospector)
	if !ok {
		return false
	}
	used, known := introspector.GlobalUsage(name)
	return known && !used
}

// attemptExtraContext returns the extra context an attempt renders with. A
// reconcile render under the exact cycle program guards its root inputs, so
// such attempts share one detached copy and its certificate while the
// configured value still equals it, instead of cloning and walking the tree
// again; any other render owns a fresh copy because nothing stops its
// templates from mutating it.
func (s *RenderService) attemptExtraContext(
	modes []rendercontext.RenderMode,
) (map[string]any, *templating.IncrementalImmutableCertificate, error) {
	source := s.config.TemplatingSettings.ExtraContext
	guarded := len(modes) == 1 && modes[0] == rendercontext.RenderModeReconcile && s.exactCycleProgram != nil
	if !guarded {
		extraContext, err := rendercontext.DetachExtraContext(source)
		return extraContext, nil, err
	}
	s.extraContextMu.Lock()
	defer s.extraContextMu.Unlock()
	same := s.extraContext != nil &&
		(len(source) == 0 && len(s.extraContext) == 0 || reflect.DeepEqual(source, s.extraContext))
	if !same {
		extraContext, err := rendercontext.DetachExtraContext(source)
		if err != nil {
			return nil, nil, err
		}
		s.extraContext = extraContext
		s.extraContextCertificate = templating.CertifyIncrementalImmutableInputs(extraContext)
	}
	return s.extraContext, s.extraContextCertificate, nil
}

func (i *renderAttemptInputs) options() []rendercontext.Option {
	opts := []rendercontext.Option{
		rendercontext.WithCapabilities(i.capabilities),
		rendercontext.WithDetachedExtraContext(i.extraContext),
		rendercontext.WithRuntimeEnvironment(i.runtimeEnvironment),
	}
	if i.currentConfigSource != nil {
		opts = append(opts, rendercontext.WithCurrentConfigSource(i.currentConfigSource))
	} else {
		opts = append(opts, rendercontext.WithCurrentConfig(i.currentConfig))
	}
	if i.currentAuxFiles != nil {
		opts = append(opts, rendercontext.WithCurrentAuxFilesSource(i.currentAuxFiles))
	}
	if i.extraContextCertificate != nil {
		opts = append(opts, rendercontext.WithImmutableCertificates(i.extraContextCertificate))
	}
	return opts
}

func (s *RenderService) renderAttempt(
	ctx context.Context,
	provider stores.StoreProvider,
	mode rendercontext.RenderMode,
	startTime time.Time,
	forceCold bool,
	tryExactReuse bool,
	attemptInputs *renderAttemptInputs,
	retryInputs *renderRetryInputs,
	extraOpts ...rendercontext.Option,
) (result *RenderResult, restart bool, err error) {
	var postProcessTransaction templating.PostProcessTransaction
	ctx, postProcessTransaction = s.engine.BeginPostProcessTransaction(ctx)
	postProcessTransactionHandedOff := false
	defer func() {
		if postProcessTransaction != nil && !postProcessTransactionHandedOff {
			postProcessTransaction.Abort()
		}
	}()

	// Build rendering context from stores
	bctx := s.buildRenderingContextFromAttemptInputs(
		ctx,
		provider,
		mode,
		httpSourceModeForRender(mode),
		attemptInputs,
		retryInputs.http,
		extraOpts...,
	)
	cacheSession, err := s.mainDocumentCache.Begin(
		s.engine,
		attemptInputs.outputGeneration,
		attemptInputs.renderCache,
	)
	if err != nil {
		return nil, false, fmt.Errorf("starting render cache transaction: %w", err)
	}
	ctx, err = templating.WithBoundImmutableResourceInputs(ctx, bctx.Context)
	if err != nil {
		return nil, false, fmt.Errorf("binding immutable render inputs: %w", err)
	}
	inputTransaction := bctx.inputTransaction
	transactionHandedOff := false
	defer func() {
		if restart {
			retryInputs.http = inputRetrySeed(inputTransaction)
		}
		if inputTransaction != nil && !transactionHandedOff {
			inputTransaction.Abort()
		}
	}()
	renderContext := bctx.Context
	var incrementalValidator incrementalCallValidator
	var renderSession *incrementalRenderSession
	ctx, inputTransaction, incrementalValidator, renderSession, err = s.beginIncrementalRender(
		ctx, provider, mode, bctx, inputTransaction, forceCold, attemptInputs.outputGeneration,
	)
	if err != nil {
		return nil, false, err
	}
	reused, hit, reuseErr := s.tryExactCycleReuseAttempt(
		ctx, bctx, renderSession, cacheSession, inputTransaction, attemptInputs, startTime,
		mode, tryExactReuse, forceCold,
	)
	if reuseErr != nil {
		return nil, false, reuseErr
	}
	if hit {
		transactionHandedOff = true
		return reused, false, nil
	}
	if err := bctx.MaterializeUsedPreviousOutputs(
		!s.skipCurrentConfigProjection,
		!s.skipCurrentFilesProjection,
	); err != nil {
		return nil, false, err
	}
	ctx, exactInputs, err := s.beginExactCycleReplay(
		ctx, renderContext, renderSession, mode, attemptInputs.outputGeneration,
	)
	if err != nil {
		return nil, false, err
	}
	mainRender, staticFiles, restart, err := s.renderDocuments(
		ctx, bctx, renderContext, forceCold, renderSession, cacheSession, inputTransaction,
	)
	if err != nil || restart {
		return nil, restart, err
	}
	if err := validateExecutedIncrementalCalls(incrementalValidator, exactInputs != nil); err != nil {
		return nil, false, err
	}
	exactInputs = s.finalizedExactCycleInputs(exactInputs)

	var handedOff bool
	result, handedOff, err = s.publishRenderAttempt(ctx, bctx, mode, &renderArtifacts{
		main:             mainRender,
		staticFiles:      staticFiles,
		rootsReused:      renderSession.exactCycleRootReuser().reused(),
		inputTransaction: inputTransaction,
		startTime:        startTime,
		outputGeneration: attemptInputs.outputGeneration,
		cacheSession:     cacheSession,
	}, renderSession, exactInputs, postProcessTransaction)
	if err != nil {
		return nil, false, err
	}
	postProcessTransactionHandedOff = handedOff
	transactionHandedOff = true
	return result, false, nil
}

func (s *RenderService) publishRenderAttempt(
	ctx context.Context,
	bctx *builtRenderingContext,
	mode rendercontext.RenderMode,
	artifacts *renderArtifacts,
	renderSession *incrementalRenderSession,
	exactInputs *templating.ExactCycleReplayInputs,
	postProcessTransaction templating.PostProcessTransaction,
) (*RenderResult, bool, error) {
	result, err := s.finishRender(ctx, bctx, mode, artifacts)
	if err != nil {
		return nil, false, err
	}
	if err := s.attachExactCycleCandidate(
		result, bctx, renderSession, exactInputs, mode, artifacts.outputGeneration, artifacts.staticFiles,
	); err != nil {
		return nil, false, err
	}
	handedOff, stageErr := s.stagePostProcessPublication(ctx, postProcessTransaction, result)
	if stageErr != nil {
		return nil, false, stageErr
	}
	return result, handedOff, nil
}

func (s *RenderService) beginIncrementalRender(
	ctx context.Context,
	provider stores.StoreProvider,
	mode rendercontext.RenderMode,
	bctx *builtRenderingContext,
	inputTransaction RenderInputTransaction,
	forceCold bool,
	outputGeneration uint64,
) (context.Context, RenderInputTransaction, incrementalCallValidator, *incrementalRenderSession, error) {
	if mode == rendercontext.RenderModeReconcile && s.incremental != nil && outputGeneration != 0 {
		s.incremental.cache.supersede(outputGeneration)
	}
	if s.incremental != nil {
		// Binding planners snapshot prior outputs at session begin, before the
		// post-reuse materialization point, so install what they read now.
		if err := bctx.MaterializeUsedPreviousOutputs(
			s.incremental.bindingsUseCurrentConfig && !s.skipCurrentConfigProjection,
			s.incremental.bindingsUseCurrentFiles && !s.skipCurrentFilesProjection,
		); err != nil {
			return ctx, inputTransaction, nil, nil, err
		}
	}
	nextCtx, nextTransaction, validator, session, err := s.startIncrementalRender(
		ctx,
		provider,
		mode,
		bctx,
		inputTransaction,
		forceCold,
	)
	if err != nil {
		return nextCtx, nextTransaction, validator, session, err
	}
	if session != nil {
		session.bindCacheOutputGeneration(outputGeneration)
	}
	return nextCtx, nextTransaction, validator, session, nil
}

func (s *RenderService) tryExactCycleReuseAttempt(
	ctx context.Context,
	bctx *builtRenderingContext,
	renderSession *incrementalRenderSession,
	cacheSession *rendercontext.RenderCacheSession,
	inputTransaction RenderInputTransaction,
	attemptInputs *renderAttemptInputs,
	startTime time.Time,
	mode rendercontext.RenderMode,
	tryExactReuse bool,
	forceCold bool,
) (*RenderResult, bool, error) {
	if !tryExactReuse || forceCold || mode != rendercontext.RenderModeReconcile {
		return nil, false, nil
	}
	return s.tryExactCycleReuse(
		ctx, bctx, renderSession, cacheSession, inputTransaction, attemptInputs, startTime,
	)
}

func (s *RenderService) beginExactCycleReplay(
	ctx context.Context,
	renderContext map[string]any,
	renderSession *incrementalRenderSession,
	mode rendercontext.RenderMode,
	outputGeneration uint64,
) (context.Context, *templating.ExactCycleReplayInputs, error) {
	if mode != rendercontext.RenderModeReconcile || s.exactCycleProgram == nil {
		return ctx, nil, nil
	}
	var exactInputs *templating.ExactCycleReplayInputs
	if renderSession != nil && renderSession.cachePublicationEnabled {
		exactCtx, inputs, beginErr := s.exactCycleProgram.BeginWithInvocations(
			ctx, outputGeneration, renderContext, exactCycleRootInvocations(s.config),
		)
		if beginErr == nil {
			ctx = exactCtx
			exactInputs = inputs
		} else if s.logger != nil {
			s.logger.Debug("Exact cycle candidate is unavailable", "reason", beginErr)
		}
	}
	if exactInputs == nil {
		exactCtx, executionErr := s.exactCycleProgram.ExecutionContext(ctx)
		if executionErr != nil {
			return nil, nil, executionErr
		}
		ctx = exactCtx
	}
	return ctx, exactInputs, nil
}

func (s *RenderService) renderDocuments(
	ctx context.Context,
	bctx *builtRenderingContext,
	renderContext map[string]any,
	forceCold bool,
	renderSession *incrementalRenderSession,
	cacheSession *rendercontext.RenderCacheSession,
	inputTransaction RenderInputTransaction,
) (rendercontext.MainDocumentRender, *dataplane.AuxiliaryFiles, bool, error) {
	mainRender, restart, err := s.renderMainAttempt(
		ctx, bctx, renderContext, forceCold, renderSession, cacheSession, inputTransaction,
	)
	if err != nil || restart {
		return rendercontext.MainDocumentRender{}, nil, restart, bctx.preserveFetchFailure(err)
	}
	staticFiles, restart, err := s.renderAuxiliaryAttempt(
		ctx, bctx, renderContext, forceCold, renderSession, inputTransaction,
	)
	if err != nil || restart {
		return rendercontext.MainDocumentRender{}, nil, restart, bctx.preserveFetchFailure(err)
	}
	restart, err = s.renderResourcesAttempt(
		ctx, bctx, renderContext, forceCold, renderSession, inputTransaction,
	)
	if err != nil || restart {
		return rendercontext.MainDocumentRender{}, nil, restart, bctx.preserveFetchFailure(err)
	}
	if err := bctx.httpFetcher.RenderFailure(); err != nil {
		return rendercontext.MainDocumentRender{}, nil, false, err
	}
	return mainRender, staticFiles, false, nil
}

// validateExecutedIncrementalCalls holds a render to its calls only when it
// executed something. A render that executed nothing while a replay was engaged
// reused the recorded cycle's invocations wholesale, so every group reads as
// silent -- 20 of 20 at once, in the failure that found this -- and that cycle
// was validated when it was produced.
func validateExecutedIncrementalCalls(validator incrementalCallValidator, replayed bool) error {
	if replayed && validator != nil && !validator.HasIncrementalCalls() {
		return nil
	}
	return validateIncrementalRenderCalls(validator)
}

func validateIncrementalRenderCalls(validator incrementalCallValidator) error {
	if validator == nil {
		return nil
	}
	return validator.ValidateIncrementalCalls()
}

func (s *RenderService) finalizedExactCycleInputs(
	exactInputs *templating.ExactCycleReplayInputs,
) *templating.ExactCycleReplayInputs {
	if exactInputs == nil {
		return nil
	}
	if finalizeErr := exactInputs.Finalize(); finalizeErr != nil {
		if s.logger != nil {
			s.logger.Debug("Exact cycle candidate is incomplete", "reason", finalizeErr)
		}
		return nil
	}
	return exactInputs
}

func (s *RenderService) attachExactCycleCandidate(
	result *RenderResult,
	bctx *builtRenderingContext,
	renderSession *incrementalRenderSession,
	exactInputs *templating.ExactCycleReplayInputs,
	mode rendercontext.RenderMode,
	outputGeneration uint64,
	staticFiles *dataplane.AuxiliaryFiles,
) error {
	var exactCandidate *exactCycleCandidate
	roots := captureExactCycleRootOutputs(staticFiles)
	if exactInputs != nil {
		candidate, err := captureExactCycleCandidate(
			s.exactCycleProgram,
			exactInputs,
			bctx,
			renderSession,
			s.httpStoreComponent,
			result.CycleSnapshot,
			result.renderCachePublication,
			result.planIdentity,
			roots,
		)
		if err != nil && !errors.Is(err, errExactCycleUnavailable) {
			return exactCycleCaptureError("candidate", err)
		}
		if err == nil {
			exactCandidate = candidate
		}
	}
	if mode != rendercontext.RenderModeReconcile || outputGeneration == 0 {
		return nil
	}
	if exactInputs != nil && exactCandidate == nil {
		result.InputTransaction = s.stageExactCycleCandidateCapture(
			result.InputTransaction,
			outputGeneration,
			exactInputs,
			bctx,
			renderSession,
			result.CycleSnapshot,
			result.renderCachePublication,
			result.planIdentity,
			roots,
		)
		return nil
	}
	result.InputTransaction = s.stageExactCycleCandidatePublication(
		result.InputTransaction, outputGeneration, exactCandidate, renderSession,
	)
	return nil
}

func (s *RenderService) stagePostProcessPublication(
	ctx context.Context,
	postProcessTransaction templating.PostProcessTransaction,
	result *RenderResult,
) (bool, error) {
	if postProcessTransaction == nil {
		return false, nil
	}
	publication, stageErr := postProcessTransaction.Stage(ctx)
	if stageErr != nil {
		return false, fmt.Errorf("staging post-process cache: %w", stageErr)
	}
	result.InputTransaction = newPostProcessPublicationTransaction(result.InputTransaction, publication)
	return true, nil
}

func (s *RenderService) renderMainAttempt(
	ctx context.Context,
	bctx *builtRenderingContext,
	renderContext map[string]any,
	forceCold bool,
	renderSession *incrementalRenderSession,
	cacheSession *rendercontext.RenderCacheSession,
	inputTransaction RenderInputTransaction,
) (mainRender rendercontext.MainDocumentRender, restart bool, err error) {
	mainCtx := templating.WithIncrementalScope(ctx, names.MainTemplateName)
	mainCtx = templating.WithExactCycleRootInvocation(mainCtx, templating.ExactCycleRootInvocation{
		Kind: "main", Name: names.MainTemplateName,
	})
	mainRender, err = rendercontext.RenderMainDocument(
		mainCtx,
		s.engine,
		renderContext,
		bctx.PlanRegistry,
		true,
		cacheSession,
	)
	if resourceErr := bctx.Err(ctx); resourceErr != nil {
		return rendercontext.MainDocumentRender{}, false, resourceErr
	}
	if err != nil {
		if !forceCold && errors.Is(err, errIncrementalColdRestart) {
			return rendercontext.MainDocumentRender{}, true, nil
		}
		return rendercontext.MainDocumentRender{}, false, fmt.Errorf("rendering %s: %w", names.MainTemplateName, err)
	}
	if !forceCold && incrementalAttemptRequiresColdRestart(renderSession, inputTransaction) {
		return rendercontext.MainDocumentRender{}, true, nil
	}
	return mainRender, false, nil
}

func (s *RenderService) renderAuxiliaryAttempt(
	ctx context.Context,
	bctx *builtRenderingContext,
	renderContext map[string]any,
	forceCold bool,
	renderSession *incrementalRenderSession,
	inputTransaction RenderInputTransaction,
) (*dataplane.AuxiliaryFiles, bool, error) {
	staticFiles, err := s.renderAuxiliaryFiles(ctx, renderContext, renderSession.exactCycleRootReuser())
	if resourceErr := bctx.Err(ctx); resourceErr != nil {
		return nil, false, resourceErr
	}
	if err != nil {
		if !forceCold && errors.Is(err, errIncrementalColdRestart) {
			return nil, true, nil
		}
		return nil, false, err
	}
	if !forceCold && incrementalAttemptRequiresColdRestart(renderSession, inputTransaction) {
		return nil, true, nil
	}
	return staticFiles, false, nil
}

func (s *RenderService) renderResourcesAttempt(
	ctx context.Context,
	bctx *builtRenderingContext,
	renderContext map[string]any,
	forceCold bool,
	renderSession *incrementalRenderSession,
	inputTransaction RenderInputTransaction,
) (bool, error) {
	err := s.renderK8sResources(ctx, renderContext, bctx.RenderedResourceCollector)
	if resourceErr := bctx.Err(ctx); resourceErr != nil {
		return false, resourceErr
	}
	if err != nil {
		if !forceCold && errors.Is(err, errIncrementalColdRestart) {
			return true, nil
		}
		return false, err
	}
	if !forceCold && incrementalAttemptRequiresColdRestart(renderSession, inputTransaction) {
		return true, nil
	}
	return false, nil
}

func inputRetrySeed(transaction RenderInputTransaction) *httpstore.InputRetrySeed {
	seeder, ok := transaction.(interface {
		RetrySeed() *httpstore.InputRetrySeed
	})
	if !ok {
		return nil
	}
	return seeder.RetrySeed()
}

type renderArtifacts struct {
	main             rendercontext.MainDocumentRender
	staticFiles      *dataplane.AuxiliaryFiles
	rootsReused      int
	inputTransaction RenderInputTransaction
	startTime        time.Time
	outputGeneration uint64
	cacheSession     *rendercontext.RenderCacheSession
}

func (s *RenderService) materializeRenderDocuments(
	ctx context.Context,
	bctx *builtRenderingContext,
	artifacts *renderArtifacts,
) (string, *dataplane.AuxiliaryFiles, error) {
	haproxyConfig, err := artifacts.main.Document.String()
	if err != nil {
		return "", nil, fmt.Errorf("materializing %s: %w", names.MainTemplateName, err)
	}
	dynamicFiles := bctx.FileRegistry.GetFiles()
	auxiliaryFiles, err := rendercontext.MergeAuxiliaryFiles(artifacts.staticFiles, dynamicFiles)
	if err != nil {
		return "", nil, fmt.Errorf("merging auxiliary files: %w", err)
	}
	consistencyErr := validateAuxiliaryFilesConsistency(haproxyConfig, auxiliaryFiles)
	if resourceErr := bctx.Err(ctx); resourceErr != nil {
		return "", nil, resourceErr
	}
	if consistencyErr != nil {
		return "", nil, fmt.Errorf("rendering %s: %w", names.MainTemplateName, consistencyErr)
	}
	collectorErr := bctx.RenderedResourceCollector.Validate()
	if resourceErr := bctx.Err(ctx); resourceErr != nil {
		return "", nil, resourceErr
	}
	if collectorErr != nil {
		return "", nil, fmt.Errorf("rendering %s: %w", names.MainTemplateName, collectorErr)
	}
	return haproxyConfig, auxiliaryFiles, nil
}

func (s *RenderService) resolveRenderPlan(
	bctx *builtRenderingContext,
	haproxyConfig string,
	auxiliaryFiles *dataplane.AuxiliaryFiles,
	artifacts *renderArtifacts,
	planTransition *rendercontext.DocumentPlanTransition,
	transitionErr error,
	previousOutputSnapshot *renderoutput.Snapshot,
) (*renderplan.Plan, *rendercontext.RenderPlanIdentity, bool, error) {
	var plan *renderplan.Plan
	var planIdentity *rendercontext.RenderPlanIdentity
	err := transitionErr
	fullOutputBuild := previousOutputSnapshot == nil
	if errors.Is(err, renderplan.ErrDocumentTransitionRequiresRebuild) {
		fullOutputBuild = true
		plan, planIdentity, err = s.buildPlan(
			bctx.PlanRegistry,
			haproxyConfig,
			auxiliaryFiles,
			artifacts.cacheSession,
		)
	}
	if err != nil {
		return nil, nil, false, fmt.Errorf("building the render plan: %w", err)
	}
	if fullOutputBuild && plan == nil {
		plan, err = planTransition.Plan.LegacyCopy()
		if err != nil {
			return nil, nil, false, fmt.Errorf("materializing the render plan: %w", err)
		}
	}
	return plan, planIdentity, fullOutputBuild, nil
}

func (s *RenderService) collectRenderSnapshots(
	bctx *builtRenderingContext,
	previousStatusPatchSnapshot *templating.StatusPatchSnapshot,
	previousEventSnapshot *templating.RenderedEventSnapshot,
	previousRenderedResourceSnapshot *templating.RenderedResourceSnapshot,
) (
	*templating.StatusPatchSnapshot,
	*templating.RenderedEventSnapshot,
	*templating.RenderedResourceSnapshot,
	error,
) {
	statusPatchSnapshot, err := bctx.StatusPatchCollector.Snapshot(previousStatusPatchSnapshot)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("snapshotting status patches: %w", err)
	}
	eventSnapshot, err := bctx.EventCollector.Snapshot(previousEventSnapshot)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("snapshotting rendered events: %w", err)
	}
	renderedResourceSnapshot, err := bctx.RenderedResourceCollector.Snapshot(previousRenderedResourceSnapshot)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("snapshotting rendered resources: %w", err)
	}
	return statusPatchSnapshot, eventSnapshot, renderedResourceSnapshot, nil
}

func planTransitionDelta(transition *rendercontext.DocumentPlanTransition) *renderplan.Delta {
	if transition == nil {
		return nil
	}
	return transition.PlanDelta
}

// reportPlanDigestFallbacks surfaces a plan digest that had to rebuild the whole
// plan instead of streaming it. The render is still correct, so this is the only
// signal that the incremental digest stopped engaging.
func (s *RenderService) reportPlanDigestFallbacks() {
	total := s.planAuthority.DigestFallbacks()
	if s.planDigestFallbacks.Swap(total) == total || s.logger == nil {
		return
	}
	s.logger.Warn("Incremental plan digest is unavailable",
		"reason", "snapshot walk order is unproven", "fallbacks", total)
}

// reportAssemblyReuse surfaces how much of the previous assembled configuration
// the render kept. A fallback reason that never clears means the incremental
// assembly stopped engaging, which is otherwise invisible.
func (s *RenderService) reportAssemblyReuse(reuse rendercontext.AssemblyReuse) {
	if s.logger == nil {
		return
	}
	s.logger.Debug("Assembled configuration reuse",
		"reused", reuse.Reused, "rebuilt", reuse.Rebuilt, "fallback", reuse.FallbackReason)
	changed, firstSight := s.assemblyReuseTransition(reuse.FallbackReason)
	if !changed {
		return
	}
	level := slog.LevelInfo
	if !firstSight {
		// A non-identity post-process chain makes engaged↔fallback flip on
		// every content change; the oscillation is Debug, each state's first
		// sight keeps the Info visibility the fallback needs.
		level = slog.LevelDebug
	}
	if reuse.FallbackReason == "" {
		s.logger.Log(context.Background(), level, "Incremental configuration assembly is engaged",
			"reused", reuse.Reused, "rebuilt", reuse.Rebuilt)
		return
	}
	s.logger.Log(context.Background(), level, "Incremental configuration assembly is unavailable",
		"reason", reuse.FallbackReason)
}

// assemblyReuseTransition reports whether the fallback state changed since the
// last render and whether this state appears for the first time.
func (s *RenderService) assemblyReuseTransition(reason string) (changed, firstSight bool) {
	s.assemblyReuseMu.Lock()
	defer s.assemblyReuseMu.Unlock()
	if s.assemblyLastReason != nil && *s.assemblyLastReason == reason {
		return false, false
	}
	current := reason
	s.assemblyLastReason = &current
	if s.assemblySeenReasons == nil {
		s.assemblySeenReasons = make(map[string]bool)
	}
	firstSight = !s.assemblySeenReasons[reason]
	s.assemblySeenReasons[reason] = true
	return true, firstSight
}

func outputSnapshotIdentity(snapshot *renderoutput.Snapshot) (contentChecksum, planID string, err error) {
	contentChecksum, err = snapshot.ContentChecksum()
	if err != nil {
		return "", "", fmt.Errorf("reading rendered output checksum: %w", err)
	}
	planID, err = snapshot.PlanID()
	if err != nil {
		return "", "", fmt.Errorf("reading rendered plan ID: %w", err)
	}
	return contentChecksum, planID, nil
}

func (s *RenderService) finishRender(
	ctx context.Context,
	bctx *builtRenderingContext,
	mode rendercontext.RenderMode,
	artifacts *renderArtifacts,
) (*RenderResult, error) {
	haproxyConfig, auxiliaryFiles, err := s.materializeRenderDocuments(ctx, bctx, artifacts)
	if err != nil {
		return nil, err
	}
	// The content checksum hashes the whole configuration; its document part
	// only needs the document, so it runs alongside the plan work below.
	preparedHash := make(chan *renderoutput.PreparedContentHash, 1)
	go func() { preparedHash <- renderoutput.PrepareContentHash(artifacts.main.Document) }()
	previousCycleSnapshot, previousPlanIdentity, previousCurrentConfigRoot := s.previousCycleState()
	previousOutputSnapshot, previousStatusPatchSnapshot, previousEventSnapshot,
		previousRenderedResourceSnapshot, err := renderCycleChildren(previousCycleSnapshot)
	if err != nil {
		return nil, fmt.Errorf("reading previous render cycle: %w", err)
	}
	planTransition, transitionErr := bctx.PlanRegistry.PlanDocument(
		artifacts.main.Document,
		auxiliaryFiles,
		s.planAuthority,
		previousOutputSnapshot,
	)
	plan, planIdentity, fullOutputBuild, err := s.resolveRenderPlan(
		bctx, haproxyConfig, auxiliaryFiles, artifacts,
		planTransition, transitionErr, previousOutputSnapshot,
	)
	if err != nil {
		return nil, err
	}
	statusPatchSnapshot, eventSnapshot, renderedResourceSnapshot, err := s.collectRenderSnapshots(
		bctx, previousStatusPatchSnapshot, previousEventSnapshot, previousRenderedResourceSnapshot,
	)
	if err != nil {
		return nil, err
	}
	artifactSnapshot, artifactDelta, auxFileCount, err := s.buildAuxiliaryFileTransition(
		previousOutputSnapshot, auxiliaryFiles,
	)
	if err != nil {
		return nil, err
	}
	var outputSnapshot *renderoutput.Snapshot
	if fullOutputBuild {
		outputSnapshot, err = renderoutput.NewSnapshotFromDocument(
			s.outputAuthority,
			artifacts.main.Document,
			plan,
			artifactSnapshot,
			previousOutputSnapshot,
		)
	} else {
		outputSnapshot, err = s.commitIncrementalOutput(
			previousOutputSnapshot,
			planTransition.DocumentDelta,
			planTransition.PlanDelta,
			artifactDelta,
		)
	}
	if err != nil {
		s.reportOutputPublicationMismatch(err, previousOutputSnapshot, auxiliaryFiles, planTransition, artifactSnapshot)
		return nil, fmt.Errorf("sealing rendered output: %w", err)
	}
	planDelta := planTransitionDelta(planTransition)
	var currentConfigRoot *exactCycleCurrentConfigRoot
	if !s.skipCurrentConfigProjection {
		currentConfigRoot, err = currentConfigRootForOutputTransition(
			previousCurrentConfigRoot,
			previousOutputSnapshot,
			outputSnapshot,
			planDelta,
			plan,
		)
		if err != nil {
			return nil, fmt.Errorf("projecting currentConfig: %w", err)
		}
	}
	if outputSnapshot == previousOutputSnapshot {
		planIdentity = previousPlanIdentity
	}
	cycleSnapshot, contentChecksum, planID, err := s.sealRenderCycle(
		outputSnapshot, <-preparedHash, statusPatchSnapshot, eventSnapshot,
		renderedResourceSnapshot, previousCycleSnapshot,
	)
	if err != nil {
		return nil, err
	}
	s.reportPlanDigestFallbacks()
	s.reportAssemblyReuse(artifacts.main.Reuse)
	cachePublication, err := artifacts.cacheSession.Prepare(ctx)
	if err != nil {
		return nil, fmt.Errorf("preparing render cache publication: %w", err)
	}
	inputTransaction, err := s.stageCyclePublicationWithPlan(
		artifacts.inputTransaction,
		mode,
		artifacts.outputGeneration,
		planIdentity,
		plan,
		currentConfigRoot,
		cycleSnapshot,
		cachePublication,
	)
	if err != nil {
		return nil, fmt.Errorf("staging rendered output: %w", err)
	}
	cacheState, cacheBuildMs := s.incrementalCacheFigures(artifacts.inputTransaction)
	return &RenderResult{
		HTTPObservations:         observedHTTPInputs(bctx.inputTransaction),
		CycleSnapshot:            cycleSnapshot,
		OutputSnapshot:           outputSnapshot,
		HAProxyConfig:            haproxyConfig,
		AuxiliaryFileSnapshot:    artifactSnapshot,
		ContentChecksum:          contentChecksum,
		Plan:                     plan,
		PlanID:                   planID,
		StatusPatchSnapshot:      statusPatchSnapshot,
		EventSnapshot:            eventSnapshot,
		RenderedResourceSnapshot: renderedResourceSnapshot,
		DurationMs:               time.Since(artifacts.startTime).Milliseconds(),
		CacheState:               cacheState,
		CacheBuildMs:             cacheBuildMs,
		RootsReused:              artifacts.rootsReused,
		AuxFileCount:             auxFileCount,
		IncludeStats:             artifacts.main.IncludeStats,
		InputTransaction:         inputTransaction,
		renderCachePublication:   cachePublication,
		planIdentity:             planIdentity,
	}, nil
}

func observedHTTPInputs(transaction RenderInputTransaction) []purehttpstore.ObservationToken {
	reader, ok := transaction.(*httpstore.InputTransaction)
	if !ok {
		return nil
	}
	return reader.HTTPObservations()
}

// sealRenderCycle completes the output's checksum from the document hash
// prepared earlier and seals the cycle over it.
func (s *RenderService) sealRenderCycle(
	outputSnapshot *renderoutput.Snapshot,
	preparedHash *renderoutput.PreparedContentHash,
	statusPatchSnapshot *templating.StatusPatchSnapshot,
	eventSnapshot *templating.RenderedEventSnapshot,
	renderedResourceSnapshot *templating.RenderedResourceSnapshot,
	previousCycleSnapshot *rendercycle.Snapshot,
) (cycleSnapshot *rendercycle.Snapshot, contentChecksum, planID string, err error) {
	if _, err := outputSnapshot.ContentChecksumWith(preparedHash); err != nil {
		return nil, "", "", fmt.Errorf("computing rendered output checksum: %w", err)
	}
	cycleSnapshot, err = rendercycle.NewSnapshot(
		s.cycleAuthority,
		outputSnapshot,
		statusPatchSnapshot,
		eventSnapshot,
		renderedResourceSnapshot,
		previousCycleSnapshot,
	)
	if err != nil {
		return nil, "", "", fmt.Errorf("sealing render cycle: %w", err)
	}
	contentChecksum, planID, err = outputSnapshotIdentity(outputSnapshot)
	if err != nil {
		return nil, "", "", err
	}
	return cycleSnapshot, contentChecksum, planID, nil
}

func (s *RenderService) buildAuxiliaryFileTransition(
	previousOutputSnapshot *renderoutput.Snapshot,
	auxiliaryFiles *dataplane.AuxiliaryFiles,
) (*renderartifact.Snapshot, *renderartifact.Delta, int, error) {
	var previousArtifactSnapshot *renderartifact.Snapshot
	if previousOutputSnapshot != nil {
		var err error
		previousArtifactSnapshot, err = previousOutputSnapshot.ArtifactSnapshot()
		if err != nil {
			return nil, nil, 0, fmt.Errorf("reading previous render output: %w", err)
		}
	}
	artifactSnapshot, artifactDelta, err := dataplane.BuildAuxiliaryFileTransitionWithRuntimePaths(
		s.artifactAuthority,
		previousArtifactSnapshot,
		auxiliaryFiles,
		s.resolveAuxiliaryRuntimePath,
	)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("snapshotting auxiliary files: %w", err)
	}
	auxFileCount, err := artifactSnapshot.Len()
	if err != nil {
		return nil, nil, 0, fmt.Errorf("counting auxiliary files: %w", err)
	}
	return artifactSnapshot, artifactDelta, auxFileCount, nil
}

func renderCycleChildren(
	cycle *rendercycle.Snapshot,
) (
	*renderoutput.Snapshot,
	*templating.StatusPatchSnapshot,
	*templating.RenderedEventSnapshot,
	*templating.RenderedResourceSnapshot,
	error,
) {
	if cycle == nil {
		return nil, nil, nil, nil, nil
	}
	output, err := cycle.OutputSnapshot()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	status, err := cycle.StatusPatchSnapshot()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	events, err := cycle.RenderedEventSnapshot()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	resources, err := cycle.RenderedResourceSnapshot()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return output, status, events, resources, nil
}

func (s *RenderService) resolveAuxiliaryRuntimePath(
	family renderartifact.Family,
	name string,
) (string, error) {
	var kind string
	switch family {
	case renderartifact.Map:
		kind = "map"
	case renderartifact.Certificate:
		kind = "cert"
	case renderartifact.CRTList:
		kind = "crt-list"
	default:
		return "", fmt.Errorf("resolving auxiliary runtime path: unsupported family %d", family)
	}
	directoryValue, err := s.pathResolver.GetPath("", kind)
	if err != nil {
		return "", fmt.Errorf("resolving %s directory: %w", kind, err)
	}
	directory, ok := directoryValue.(string)
	if !ok {
		return "", fmt.Errorf("resolving %s directory returned %T", kind, directoryValue)
	}
	resolved, err := s.pathResolver.GetPath(strings.TrimPrefix(name, directory+"/"), kind)
	if err != nil {
		return "", fmt.Errorf("resolving %s %q: %w", kind, name, err)
	}
	runtimePath, ok := resolved.(string)
	if !ok {
		return "", fmt.Errorf("resolving %s %q returned %T", kind, name, resolved)
	}
	return runtimePath, nil
}

type incrementalCallValidator interface {
	ValidateIncrementalCalls() error
	// HasIncrementalCalls reports whether this render executed any component.
	// A replay executes none, so its call bookkeeping describes the recorded
	// cycle's walk rather than this render.
	HasIncrementalCalls() bool
}

func incrementalAttemptRequiresColdRestart(
	renderSession *incrementalRenderSession,
	transaction RenderInputTransaction,
) bool {
	if renderSession == nil {
		return false
	}
	if renderSession.requiresColdRestart() {
		return true
	}
	provisional, ok := transaction.(interface{ ProvisionalURLs() []string })
	if ok && renderSession.provisionalHTTPAffectsReplayedOutput(provisional.ProvisionalURLs()) {
		return true
	}
	return false
}

func (s *RenderService) startIncrementalRender(
	ctx context.Context,
	provider stores.StoreProvider,
	mode rendercontext.RenderMode,
	bctx *builtRenderingContext,
	inputTransaction RenderInputTransaction,
	forceCold bool,
) (context.Context, RenderInputTransaction, incrementalCallValidator, *incrementalRenderSession, error) {
	if s.incremental == nil {
		return ctx, newCombinedRenderInputTransaction(inputTransaction, nil, s.logger), nil, nil, nil
	}
	loggerContext := incrementalLoggerContext{logger: s.logger, typedResourceTypes: s.typedResourceTypes}
	renderSession, err := s.incremental.begin(
		ctx,
		provider,
		s.httpStoreComponent,
		mode,
		bctx.Context,
		bctx.ResourceErrors,
		loggerContext,
	)
	if err != nil {
		if forceCold {
			return ctx, inputTransaction, nil, nil,
				fmt.Errorf("starting exact cold incremental render: %w", err)
		}
		return ctx, inputTransaction, nil, nil, fmt.Errorf("starting incremental render: %w", err)
	}
	if forceCold {
		renderSession.usePinnedColdRenderer()
		coldRenderer, coldErr := newPinnedColdIncrementalRenderer(
			ctx,
			renderSession,
			bctx.Context,
			bctx.ResourceErrors,
			loggerContext,
		)
		if coldErr != nil {
			renderSession.abort()
			return ctx, inputTransaction, nil, nil,
				fmt.Errorf("starting exact cold incremental render: %w", coldErr)
		}
		ctx = templating.WithIncrementalRenderer(ctx, coldRenderer)
		bctx.Context[incrementalResourcesContextName] = s.incremental.resourcesValue(
			ctx,
			renderSession.stores,
			bctx.ResourceErrors,
			&incrementalPinnedResourceView{session: renderSession},
			bctx.DerivedResources,
			loggerContext,
			true,
		)
		return ctx, newCombinedRenderInputTransaction(inputTransaction, renderSession, s.logger),
			coldRenderer, renderSession, nil
	}
	if err := renderSession.prepareDerivedStage(ctx); err != nil {
		renderSession.abort()
		return ctx, inputTransaction, nil, nil, fmt.Errorf("preparing incremental derived resources: %w", err)
	}
	if len(renderSession.bindingPlan.owners) > 0 {
		if err := bctx.DerivedResources.SetResolver(&incrementalDerivedResourceResolver{session: renderSession}); err != nil {
			renderSession.abort()
			return ctx, inputTransaction, nil, nil, fmt.Errorf("configuring incremental derived resources: %w", err)
		}
	}
	if err := renderSession.prepareColdComponentGraph(ctx); err != nil {
		renderSession.abort()
		return ctx, inputTransaction, nil, nil, fmt.Errorf("preparing incremental cold graph: %w", err)
	}
	bctx.DerivedResources.Freeze()
	ctx = templating.WithIncrementalRenderer(ctx, renderSession)
	bctx.Context[incrementalResourcesContextName] = s.incremental.resourcesValue(
		ctx,
		renderSession.stores,
		bctx.ResourceErrors,
		&incrementalPinnedResourceView{session: renderSession},
		bctx.DerivedResources,
		loggerContext,
		true,
	)
	return ctx, newCombinedRenderInputTransaction(inputTransaction, renderSession, s.logger),
		renderSession, renderSession, nil
}

type builtRenderingContext struct {
	httpFetcher *httpstore.HTTPStoreWrapper
	*rendercontext.BuildResult
	inputTransaction RenderInputTransaction
}

func (b *builtRenderingContext) preserveFetchFailure(err error) error {
	fetchErr := b.httpFetcher.RenderFailure()
	if fetchErr == nil || errors.Is(err, fetchErr) {
		return err
	}
	if err == nil {
		return fetchErr
	}
	// A template's fail(string) must retain the original fetch error identity.
	return &renderFetchError{renderErr: err, fetchErr: fetchErr}
}

type renderFetchError struct {
	renderErr error
	fetchErr  error
}

func (e *renderFetchError) Error() string { return e.renderErr.Error() }

func (e *renderFetchError) Unwrap() []error { return []error{e.renderErr, e.fetchErr} }

// buildRenderingContext constructs the template rendering context from stores.
//
// This goes through the shared rendercontext.Builder — the exact same path
// testrunner and the render benchmark use — so a template can't pass
// `controller validate` yet behave differently in production. The only
// production-specific plumbing is reading the live stores off the
// StoreProvider, resolving the current deployed config, and wiring the HTTP
// fetcher (whose overlay depends on the provider type); everything else is the
// Builder's responsibility.
func (s *RenderService) buildRenderingContext(ctx context.Context, provider stores.StoreProvider, mode rendercontext.RenderMode, extraOpts ...rendercontext.Option) (*builtRenderingContext, error) {
	return s.buildRenderingContextWithHTTPSourceMode(ctx, provider, mode, httpSourceModeForRender(mode), extraOpts...)
}

func httpSourceModeForRender(mode rendercontext.RenderMode) httpstore.SourceMode {
	if mode == rendercontext.RenderModeReconcile {
		return httpstore.SourceModeAuthoritative
	}
	return httpstore.SourceModeReadOnly
}

func (s *RenderService) buildRenderingContextWithHTTPSourceMode(
	ctx context.Context,
	provider stores.StoreProvider,
	mode rendercontext.RenderMode,
	httpSourceMode httpstore.SourceMode,
	extraOpts ...rendercontext.Option,
) (*builtRenderingContext, error) {
	attemptInputs, err := s.captureRenderAttemptInputs()
	if err != nil {
		return nil, err
	}
	return s.buildRenderingContextFromAttemptInputs(
		ctx, provider, mode, httpSourceMode, attemptInputs, nil, extraOpts...,
	), nil
}

func (s *RenderService) buildRenderingContextFromAttemptInputs(
	ctx context.Context,
	provider stores.StoreProvider,
	mode rendercontext.RenderMode,
	httpSourceMode httpstore.SourceMode,
	attemptInputs *renderAttemptInputs,
	httpRetrySeed *httpstore.InputRetrySeed,
	extraOpts ...rendercontext.Option,
) *builtRenderingContext {
	// Snapshot the live stores off the provider. The haproxy-pods store is
	// separated out by the Builder (WithHAProxyPodStore) into
	// controller.haproxy_pods; the rest land in `resources`.
	storesByName := make(map[string]stores.Store, len(provider.StoreNames()))
	for _, name := range provider.StoreNames() {
		if store := provider.GetStore(name); store != nil {
			storesByName[name] = store
		}
	}
	resourceStores, haproxyPodStore := rendercontext.SeparateHAProxyPodStore(storesByName)
	if haproxyPodStore == nil {
		// Production injects the haproxy-pods store directly (it may not be
		// registered with the provider under names.HAProxyPodsResourceType).
		haproxyPodStore = s.haproxyPodStore
	}

	opts := []rendercontext.Option{
		rendercontext.WithStores(resourceStores),
		rendercontext.WithHAProxyPodStore(haproxyPodStore),
		rendercontext.WithTypedResources(s.typedResourceTypes),
		rendercontext.WithRenderMode(mode),
		rendercontext.WithPlanTokenAuthority(s.planTokenAuthority),
		rendercontext.WithPlanMemo(s.planMemo),
		rendercontext.WithNamespaceScopes(s.namespaceScopes),
	}
	opts = append(opts, attemptInputs.options()...)

	var inputTransaction RenderInputTransaction
	var httpFetcher *httpstore.HTTPStoreWrapper
	if s.httpStoreComponent != nil {
		var httpOverlay stores.HTTPContentOverlay
		if overlayProvider, ok := provider.(*stores.OverlayStoreProvider); ok {
			httpOverlay = overlayProvider.GetHTTPOverlay()
		}
		httpFetcher = httpstore.NewHTTPStoreWrapperWithRetrySeed(
			ctx,
			s.httpStoreComponent,
			s.logger,
			httpOverlay,
			httpSourceMode,
			httpRetrySeed,
		)
		if transaction := httpFetcher.InputTransaction(); transaction != nil {
			inputTransaction = transaction
		}
		opts = append(opts, rendercontext.WithHTTPFetcher(httpFetcher))
	}

	// Call-specific options override service defaults. The Coordinator uses this
	// to pin currentFiles to its leader-term snapshot for the whole render.
	opts = append(opts, extraOpts...)

	return &builtRenderingContext{
		httpFetcher:      httpFetcher,
		BuildResult:      rendercontext.NewBuilder(ctx, s.config, s.pathResolver, s.logger, opts...).Build(),
		inputTransaction: inputTransaction,
	}
}

// renderAuxiliaryFiles renders all auxiliary files in parallel.
// It respects the caller's context for cancellation.
func (s *RenderService) renderAuxiliaryFiles(
	ctx context.Context,
	renderCtx map[string]any,
	reuser *exactCycleRootReuser,
) (*dataplane.AuxiliaryFiles, error) {
	totalFiles := len(s.config.Maps) + len(s.config.Files) + len(s.config.SSLCertificates)
	if totalFiles == 0 {
		return &dataplane.AuxiliaryFiles{}, nil
	}

	var mu sync.Mutex
	// Pre-allocate slices with known capacity to avoid grow-from-zero
	auxFiles := &dataplane.AuxiliaryFiles{
		MapFiles:        make([]auxiliaryfiles.MapFile, 0, len(s.config.Maps)),
		GeneralFiles:    make([]auxiliaryfiles.GeneralFile, 0, len(s.config.Files)),
		SSLCertificates: make([]auxiliaryfiles.SSLCertificate, 0, len(s.config.SSLCertificates)),
	}
	if templating.ExactCycleReplayExecutionActive(ctx) {
		if err := renderAuxGroupSerial(
			ctx, s.engine, renderCtx, reuser, s.config.Maps, "map", &auxFiles.MapFiles,
			func(name, content string) auxiliaryfiles.MapFile {
				return auxiliaryfiles.MapFile{Path: name, Content: content}
			},
		); err != nil {
			return nil, err
		}
		if err := renderAuxGroupSerial(
			ctx, s.engine, renderCtx, reuser, s.config.Files, "file", &auxFiles.GeneralFiles,
			func(name, content string) auxiliaryfiles.GeneralFile {
				return auxiliaryfiles.GeneralFile{
					Filename: name, Path: path.Join(s.pathResolver.GeneralDir, name),
					Content: content, ReloadOnPush: s.config.Files[name].ReloadOnPush,
				}
			},
		); err != nil {
			return nil, err
		}
		if err := renderAuxGroupSerial(
			ctx, s.engine, renderCtx, reuser, s.config.SSLCertificates, "SSL certificate",
			&auxFiles.SSLCertificates,
			func(name, content string) auxiliaryfiles.SSLCertificate {
				return auxiliaryfiles.SSLCertificate{Path: name, Content: content}
			},
		); err != nil {
			return nil, err
		}
		return auxFiles, nil
	}

	// Create errgroup for parallel rendering. We discard the derived context because:
	// 1. Template rendering is CPU-bound and doesn't benefit from early cancellation
	// 2. errgroup still coordinates completion and returns the first error via Wait()
	// 3. The caller's ctx is available for overall timeout/cancellation if needed
	g, _ := errgroup.WithContext(ctx)

	// Render map files in parallel
	renderAuxGroup(ctx, g, &mu, s.engine, renderCtx,
		s.config.Maps, "map", &auxFiles.MapFiles,
		func(name, content string) auxiliaryfiles.MapFile {
			return auxiliaryfiles.MapFile{Path: name, Content: content}
		})

	// Render general files in parallel
	renderAuxGroup(ctx, g, &mu, s.engine, renderCtx,
		s.config.Files, "file", &auxFiles.GeneralFiles,
		func(name, content string) auxiliaryfiles.GeneralFile {
			return auxiliaryfiles.GeneralFile{
				Filename:     name,
				Path:         path.Join(s.pathResolver.GeneralDir, name),
				Content:      content,
				ReloadOnPush: s.config.Files[name].ReloadOnPush,
			}
		})

	// Render SSL certificates in parallel
	renderAuxGroup(ctx, g, &mu, s.engine, renderCtx,
		s.config.SSLCertificates, "SSL certificate", &auxFiles.SSLCertificates,
		func(name, content string) auxiliaryfiles.SSLCertificate {
			return auxiliaryfiles.SSLCertificate{Path: name, Content: content}
		})

	if err := g.Wait(); err != nil {
		return nil, err
	}

	return auxFiles, nil
}

// ClearVMPool releases pooled template engine VMs.
// Call after rendering completes to reduce memory from parallel rendering spikes.
func (s *RenderService) ClearVMPool() {
	if s.engine != nil {
		s.engine.ClearVMPool()
	}
}

// sourceMapper is the optional engine capability RenderSourceMaps needs. The
// production ScriggoEngine implements it; this keeps the templating.Engine
// interface unchanged (source maps are a playground-only concern).
type sourceMapper interface {
	RenderWithSourceMap(ctx context.Context, name string, tctx map[string]any) (string, []templating.SourceSpan, error)
}

// TemplateSourceMap is the raw (pre-post-processing) render of one template plus
// its output-to-source spans. Length fields of Spans sum to len(Raw).
type TemplateSourceMap struct {
	Raw   string
	Spans []templating.SourceSpan
}

// RenderSourceMaps renders the main config and each map/file template a second
// time with source-map collection, over the same context Render builds, and
// returns name→source map. It is a playground-only pass (provenance); it returns
// nil if the engine doesn't support source maps. Names are the template registry
// keys: names.MainTemplateName for the config, and the map/file key otherwise.
func (s *RenderService) RenderSourceMaps(ctx context.Context, provider stores.StoreProvider) (map[string]TemplateSourceMap, error) {
	sm, ok := s.engine.(sourceMapper)
	if !ok {
		return map[string]TemplateSourceMap{}, nil
	}
	// Source-map introspection is read-only provenance, not enforcement — use
	// the lenient reconcile mode so it never fails on a conflict.
	bctx, err := s.buildRenderingContextWithHTTPSourceMode(
		ctx,
		provider,
		rendercontext.RenderModeReconcile,
		httpstore.SourceModeReadOnly,
	)
	if err != nil {
		return nil, err
	}
	renderCtx := bctx.Context
	coldRender, err := NewColdIncrementalRender(ctx, &ColdIncrementalRenderConfig{
		Config:             s.config,
		Engine:             s.engine,
		StoreProvider:      provider,
		Mode:               rendercontext.RenderModeReconcile,
		TemplateContext:    renderCtx,
		ResourceErrors:     bctx.ResourceErrors,
		Logger:             s.logger,
		TypedResourceTypes: s.typedResourceTypes,
	})
	if err != nil {
		return nil, fmt.Errorf("starting cold incremental source-map render: %w", err)
	}
	ctx = coldRender.Context(ctx)
	out := make(map[string]TemplateSourceMap)
	add := func(name string) {
		scopedCtx := templating.WithIncrementalScope(ctx, name)
		if raw, spans, err := sm.RenderWithSourceMap(scopedCtx, name, renderCtx); err == nil {
			out[name] = TemplateSourceMap{Raw: raw, Spans: spans}
		}
	}
	add(names.MainTemplateName)
	for name := range s.config.Maps {
		add(name)
	}
	for name := range s.config.Files {
		add(name)
	}
	for name := range s.config.SSLCertificates {
		add(name)
	}
	// k8sResources templates back the "applied" tab. Their rendered YAML is
	// re-marshaled for display (keys reordered), so the playground content-matches
	// displayed lines against this raw source map to attribute each one.
	for name := range s.config.K8sResources {
		add(name)
	}
	if err := coldRender.ValidateIncrementalCalls(); err != nil {
		return nil, err
	}
	if err := bctx.Err(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// renderK8sResources renders every entry in spec.k8sResources in parallel,
// parses the rendered output as one or more YAML documents (multi-doc
// supported via `---` separators), and registers each document with the
// supplied RenderedResourceCollector. The collector is the same input
// downstream consumers (resourceapplier) read off RenderResult.
//
// Each YAML document must declare apiVersion, kind, and metadata.name
// (plus metadata.namespace for namespaced kinds). A bad document aborts
// the render with an error scoped to the offending template name so
// authors can locate it.
func (s *RenderService) renderK8sResources(ctx context.Context, renderCtx map[string]any, collector *templating.RenderedResourceCollector) error {
	if len(s.config.K8sResources) == 0 {
		return nil
	}
	if templating.ExactCycleReplayExecutionActive(ctx) {
		templateNames := make([]string, 0, len(s.config.K8sResources))
		for name := range s.config.K8sResources {
			templateNames = append(templateNames, name)
		}
		slices.Sort(templateNames)
		for _, name := range templateNames {
			scopedCtx := templating.WithIncrementalScope(ctx, name)
			scopedCtx = templating.WithExactCycleRootInvocation(
				scopedCtx, templating.ExactCycleRootInvocation{Kind: "k8s resource", Name: name},
			)
			rendered, err := s.engine.Render(scopedCtx, name, renderCtx)
			if err != nil {
				return fmt.Errorf("rendering k8sResources %s: %w", name, err)
			}
			if err := RegisterK8sResourceDocs(name, rendered, collector, s.config.K8sResources[name].CreateOnlyFields); err != nil {
				return err
			}
		}
		return nil
	}
	g, _ := errgroup.WithContext(ctx)
	for name := range s.config.K8sResources {
		g.Go(func() error {
			scopedCtx := templating.WithIncrementalScope(ctx, name)
			rendered, err := s.engine.Render(scopedCtx, name, renderCtx)
			if err != nil {
				return fmt.Errorf("rendering k8sResources %s: %w", name, err)
			}
			return RegisterK8sResourceDocs(name, rendered, collector, s.config.K8sResources[name].CreateOnlyFields)
		})
	}
	return g.Wait()
}

// RegisterK8sResourceDocs parses rendered YAML (one or more documents
// separated by `---`), validates each, and adds it to the collector.
func RegisterK8sResourceDocs(
	templateName, rendered string,
	collector *templating.RenderedResourceCollector,
	createOnlyFields []string,
) error {
	if strings.TrimSpace(rendered) == "" {
		// Empty render is a valid "no resources to emit this cycle"
		// signal — common when a template gates its output on a
		// resource state that doesn't currently exist.
		return nil
	}
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	docIdx := 0
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("parsing k8sResources %s document %d: %w", templateName, docIdx, err)
		}
		docIdx++
		if len(doc) == 0 {
			continue
		}
		normalizeYAMLTimestamps(doc)
		apiVersion, _ := doc["apiVersion"].(string)
		kind, _ := doc["kind"].(string)
		metadata, _ := doc["metadata"].(map[string]any)
		var name, namespace string
		if metadata != nil {
			name, _ = metadata["name"].(string)
			namespace, _ = metadata["namespace"].(string)
		}
		if apiVersion == "" || kind == "" || name == "" {
			return fmt.Errorf("k8sResources %s document %d: apiVersion, kind, and metadata.name are required", templateName, docIdx)
		}
		// Strip the identifying fields before handing the object to
		// Register — Register re-injects them from the explicit
		// arguments, and leaving them in would have Register copy
		// them back over no-ops. metadata is intentionally kept
		// since templates may add labels / annotations / ownerRefs
		// the applier then merges with the resource it sends.
		if err := collector.RegisterWithCreateOnlyFields(
			apiVersion, kind, namespace, name, doc, createOnlyFields,
		); err != nil {
			return fmt.Errorf("k8sResources %s document %d: %w", templateName, docIdx, err)
		}
	}
}

// normalizeYAMLTimestamps rewrites the time.Time values yaml.v3 produces for
// unquoted RFC3339 scalars back into strings, in place.
//
// A Kubernetes object is JSON-shaped and has no timestamp type, so a template
// writing `lastTimestamp: 2026-01-01T00:00:00Z` means the string. Without this
// the value reaches the immutable projection, which rejects it and turns a
// legitimate object into an admission denial.
//
// Nano rather than plain RFC3339: it renders a whole second identically and
// keeps a fraction the template wrote, instead of silently truncating it.
func normalizeYAMLTimestamps(value any) any {
	switch typed := value.(type) {
	case time.Time:
		return typed.Format(time.RFC3339Nano)
	case map[string]any:
		for key, nested := range typed {
			typed[key] = normalizeYAMLTimestamps(nested)
		}
	case []any:
		for i, nested := range typed {
			typed[i] = normalizeYAMLTimestamps(nested)
		}
	}
	return value
}

// renderAuxGroup renders one auxiliary-file group in parallel via g. For each
// name in sources it submits a render goroutine that, on success, appends the
// per-item value built by build to *out under mu. Render errors are wrapped
// with label so the eventual g.Wait() failure makes clear which group failed
// (e.g. "map", "file", "SSL certificate"). The map values are unused — only
// the keys (template names) drive the rendering.
func renderAuxGroup[V any, T any](
	ctx context.Context,
	g *errgroup.Group,
	mu *sync.Mutex,
	engine templating.Engine,
	renderCtx map[string]any,
	sources map[string]V,
	label string,
	out *[]T,
	build func(name, content string) T,
) {
	for name := range sources {
		g.Go(func() error {
			scopedCtx := templating.WithIncrementalScope(ctx, name)
			rendered, err := engine.Render(scopedCtx, name, renderCtx)
			if err != nil {
				return fmt.Errorf("rendering %s %s: %w", label, name, err)
			}
			mu.Lock()
			*out = append(*out, build(name, rendered))
			mu.Unlock()
			return nil
		})
	}
}

func renderAuxGroupSerial[V any, T any](
	ctx context.Context,
	engine templating.Engine,
	renderCtx map[string]any,
	reuser *exactCycleRootReuser,
	sources map[string]V,
	label string,
	out *[]T,
	build func(name, content string) T,
) error {
	templateNames := make([]string, 0, len(sources))
	for name := range sources {
		templateNames = append(templateNames, name)
	}
	slices.Sort(templateNames)
	for _, name := range templateNames {
		scopedCtx := templating.WithIncrementalScope(ctx, name)
		scopedCtx = templating.WithExactCycleRootInvocation(
			scopedCtx, templating.ExactCycleRootInvocation{Kind: label, Name: name},
		)
		rendered, reused, err := reuser.reuse(scopedCtx, label, name)
		if err != nil {
			return fmt.Errorf("reusing %s %s: %w", label, name, err)
		}
		if !reused {
			rendered, err = engine.Render(scopedCtx, name, renderCtx)
			if err != nil {
				return fmt.Errorf("rendering %s %s: %w", label, name, err)
			}
		}
		*out = append(*out, build(name, rendered))
	}
	return nil
}
