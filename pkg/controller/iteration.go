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

package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/configchange"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/debug"
	dryrunvalidator "github.com/haproxy-haptic/HAPTIC/pkg/controller/dryrunvalidator"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/metrics"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/pluggablevalidator"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/validator"
	coreconfig "github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/introspection"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/client"
)

// buildAndRegisterPluggableValidatorManager constructs the validator
// manager from the configured spec.validators and registers a
// cleanup callback on the iteration setup so the manager's
// connection pools are drained on teardown (every iteration
// restart, every config change, every shutdown).
func buildAndRegisterPluggableValidatorManager(setup *componentSetup, cfg *coreconfig.Config, logger *slog.Logger) (*pluggablevalidator.Manager, *sharedCleanup, error) {
	configs := make([]pluggablevalidator.ManagerConfig, 0, len(cfg.Validators))
	for _, v := range cfg.Validators {
		mc := pluggablevalidator.ManagerConfig{
			Name:           v.Name,
			SocketPath:     v.SocketPath,
			Files:          v.Files,
			DataFiles:      v.DataFiles,
			MaxConnections: int(v.MaxConnections),
		}
		if v.TimeoutMs > 0 {
			mc.Timeout = time.Duration(v.TimeoutMs) * time.Millisecond
		}
		configs = append(configs, mc)
	}
	// The rendered files land under the parent of the maps directory on the
	// HAProxy pod — the same derivation the renderer uses for the template
	// PathResolver's BaseDir, so what a validator is told matches what the
	// templates emit.
	mgr, err := pluggablevalidator.NewManager(logger, configs,
		pluggablevalidator.WithStagedRoot(filepath.Dir(cfg.Dataplane.MapsDir)))
	if err != nil {
		return nil, nil, err
	}
	if mgr.Configured() {
		logger.Info("Pluggable validators registered",
			slog.Int("count", len(configs)),
			slog.Any("names", mgr.Names()),
		)
	}
	cleanup := newSharedCleanup(func() {
		logger.Debug("Closing pluggable-validator connection pools")
		mgr.Close()
	})
	setup.AddCleanup(cleanup.Release)
	return mgr, cleanup, nil
}

// waitAndLoadInitialConfig polls until the HAProxyTemplateConfig exists (the
// fresh-install race: the controller pod may start before the CR is applied),
// then fetches and structurally validates it together with the credentials
// Secret.
func waitAndLoadInitialConfig(
	ctx context.Context,
	k8sClient *client.Client,
	crdName string,
	secretName string,
	state *configState,
	logger *slog.Logger,
) (*InitialConfigBundle, error) {
	if err := waitForInitialConfig(ctx, k8sClient, crdName, crdGVR, libraryGVR, state, logger); err != nil {
		return nil, err
	}
	return fetchAndValidateInitialConfig(
		ctx, k8sClient, crdName, secretName,
		crdGVR, libraryGVR, secretGVR, logger,
	)
}

// liveIteration is a started iteration: serving, or retired and waiting for
// teardown.
type liveIteration struct {
	setup     *componentSetup
	authority *iterationReloadAuthority
	logger    *slog.Logger
	retired   bool
}

// awaitReload blocks until the iteration records a reload or ends. A reload
// leaves the iteration serving: the caller starts the successor against it
// and tears this one down afterwards.
func (it *liveIteration) awaitReload() (*configchange.ReloadRequest, error) {
	select {
	case <-it.authority.Ready():
		reload := it.authority.Latest()
		it.logger.Info("Configuration change detected, preparing the successor iteration",
			"new_config_version", reload.Snapshot.ConfigVersion)
		return reload, nil
	case <-it.setup.IterCtx.Done():
		return waitForIterationExit(it.setup, it.authority, it.logger)
	}
}

// startupInterruptedError reports a startup that a reload cut short; the
// sequence restarts at once from that reload instead of the retry delay.
type startupInterruptedError struct {
	reload *configchange.ReloadRequest
}

func (e *startupInterruptedError) Error() string {
	return "iteration startup interrupted by a configuration change to " + e.reload.Snapshot.ConfigVersion
}

// retireLeadership ends this iteration's term for its successor on the same
// replica; the iteration's watchers and bus keep running until teardown.
func (it *liveIteration) retireLeadership() {
	if it.retired {
		return
	}
	it.retired = true
	retireLeadership(it.setup, it.logger)
}

// releaseKeptLease vacates the Lease a retired iteration kept for a successor
// that failed to start, so another replica need not wait for it to expire.
func (it *liveIteration) releaseKeptLease() {
	if it.retired && it.setup.Election != nil {
		it.setup.Election.elector.ReleaseLease()
	}
}

// teardown stops everything the iteration still runs.
func (it *liveIteration) teardown() error {
	return completeIteration(it.setup, nil, it.logger)
}

// handoverWarmupWait is how long a successor waits for a warm graph before it
// waits only for the warm-up render in flight.
const handoverWarmupWait = 30 * time.Second

// startIteration starts a controller iteration and returns once it serves.
//
// The sequence:
//  1. Fetches and validates the initial HAProxyTemplateConfig CRD and credentials Secret
//  2. Creates and starts all event-driven components
//  3. Creates and starts resource watchers, waits for sync
//  4. Creates and starts SingleWatchers for the CRD and credentials Secret, waits for sync
//  5. Starts the EventBus (releases buffered events)
//  6. Starts reconciliation components (Stage 5)
//  7. Retires the previous iteration's leadership, once this one's graph is warm
//  8. Enters leader election and installs the webhook validators
//  9. Starts debug infrastructure (StateCache, EventBuffer, debug server if enabled)
//
// previous is the iteration still serving the old configuration; its
// leadership is handed over right before this one enters election, so the
// fleet sees a warm leader the whole time. On error the started parts are torn
// down and previous keeps whatever it still has.
func startIteration(
	ctx context.Context,
	k8sClient *client.Client,
	crdName string,
	secretName string,
	webhookCertDir string,
	webhookAdmissionTimeouts WebhookAdmissionTimeouts,
	debugPort int,
	webhookPort int,
	infra *persistentInfra,
	startup *configchange.ReloadRequest,
	previous *liveIteration,
	logger *slog.Logger,
) (live *liveIteration, iterationErr error) {
	logger.Info("Starting controller iteration")

	// Reinit-grace accounting (a voluntary restart must not flip /healthz
	// unhealthy for the bounded rebuild window — see InReinitGrace), then
	// clear the persistent introspection registry of the previous
	// iteration's entries, then a fresh per-iteration health state.
	state := beginIteration(infra)
	defer infra.NoteAttemptReturned(state.iterationID)

	// 0. Setup components BEFORE fetching config so we can start servers early.
	// The type bootstrapper is also reused by the step-2.5 startup validationTests
	// gate below, so it's hoisted to a local rather than constructed inline.
	typeBootstrapper := newIterationTypeBootstrapper(k8sClient, logger)
	setup := setupComponents(ctx, infra.IntrospectionRegistry, infra.eventDropMetrics, infra.lastMetrics, typeBootstrapper, crdName, logger)
	infra.lastMetrics = setup.MetricsComponent.Metrics()
	setup.AgentTLS = infra.AgentTLS
	reloadAuthority := newIterationReloadAuthority()
	startIterationReloadObserver(setup, reloadAuthority)
	defer func() {
		if iterationErr != nil {
			iterationErr = completeFailedStartup(setup, reloadAuthority, iterationErr, logger)
		}
	}()

	// 0.25. Create EventBuffer early (subscribes in constructor)
	// Must be created before startEarlyInfrastructureServers() to register /debug/events handler
	// and before EventBus.Start() to ensure proper subscription ordering
	eventBuffer := debug.NewEventBuffer(DebugEventBufferSize, setup.Bus)

	// 0.5. Start infrastructure servers EARLY (before config fetch)
	// This allows debugging startup issues and makes health endpoint available immediately
	// The health checker will report unhealthy until config is loaded
	// Uses two-phase initialization (Setup/Serve) to register /debug/events before serving
	// The introspection server persists across iterations to avoid port rebinding issues
	// We pass the main ctx (not setup.IterCtx) so the server stays alive across iterations
	if err := startEarlyInfrastructureServers(ctx, debugPort, infra, setup, state, eventBuffer, logger); err != nil {
		return nil, err
	}

	active, bundle, err := loadAcceptedIterationConfig(
		setup, state, startup, k8sClient, crdName, secretName, typeBootstrapper, infra, logger)
	if err != nil {
		return nil, err
	}
	cfg, crd, creds := active.Config, bundle.CRD, bundle.Credentials

	// 3. Setup resource watchers
	resourceWatcher, err := setupResourceWatchers(setup, cfg, k8sClient, logger)
	if err != nil {
		return nil, err
	}

	// Build the store provider (used for webhook dry-run validation) from the
	// resource watcher's live stores.
	storeProvider := buildStoreProvider(resourceWatcher.GetAllStores())

	// 4. Setup config watchers
	if err := setupConfigWatchers(
		setup, k8sClient, crdName, secretName,
		crdGVR, libraryGVR, secretGVR, logger,
	); err != nil {
		return nil, err
	}

	// 5. Initialize debug state and the leader-term `currentFiles` authority.
	stateCache, currentFiles, err := initRenderState(setup, resourceWatcher, k8sClient, crdName, logger)
	if err != nil {
		return nil, err
	}

	// 5.5. Construct the validator used by every render pipeline.
	pluggableMgr, pluggableMgrCleanup, err := buildAndRegisterPluggableValidatorManager(setup, cfg, logger)
	if err != nil {
		return nil, fmt.Errorf("creating pluggable validators: %w", err)
	}

	// 6. Create reconciliation components (Stage 5)
	// Components subscribe during construction, before EventBus.Start()
	logger.Info("Stage 5: Creating reconciliation components")
	wiring, err := setupReconciliation(setup, cfg, crd, bundle.Sources, creds, k8sClient, resourceWatcher, currentFiles, storeProvider, pluggableMgr, logger)
	if err != nil {
		return nil, err
	}

	// 6.1. EventBuffer was already created early (step 0.25) for /debug/events handler
	// It subscribes in constructor before EventBus.Start() for proper subscription ordering

	wiring.freshStoreProvider = buildFreshStoreProvider(resourceWatcher)

	// Reconciliation and admission share the same output validator.
	dryrunValidator, err := createDryRunValidator(cfg, storeProvider, wiring, pluggableMgr, logger)
	if err != nil && !errors.Is(err, errNoWebhookRules) {
		return nil, fmt.Errorf("creating webhook validators: %w", err)
	}

	// 6.5. Start the EventBus (releases buffered events and begins normal operation)
	// All components have now subscribed during their construction, so we can safely start
	// the bus without race conditions or timing-based sleeps
	logger.Info("Starting EventBus (all components subscribed)")
	if err := startEventBus(setup); err != nil {
		return nil, err
	}

	takeOverLeadership(setup.IterCtx, previous, wiring.warmer, infra.draining.Load, logger)
	if err := setupLeadershipAndWebhook(ctx, setup, infra, cfg, webhookCertDir, webhookAdmissionTimeouts, webhookPort, k8sClient, dryrunValidator, pluggableMgrCleanup, logger); err != nil {
		return nil, err
	}

	// 9. Setup debug and metrics infrastructure (start pre-created EventBuffer)
	// Note: The introspection server is already started by startEarlyInfrastructureServers
	// This call registers debug variables and updates the health checker
	health := setupInfrastructureServers(setup.IterCtx, setup, state, infra, stateCache, eventBuffer, pluggableMgr, logger)

	// 10. Enable reinitialization signaling now that startup is complete
	// This replays any config or credential update newer than the fetched startup
	// versions, then allows future updates to trigger reinitialization.
	// 11. Flip the "initialized" health bit. /healthz returns 503 until
	// this fires and 200 (assuming other components healthy) after.
	// This is the canonical "controller is ready to accept work"
	// signal — see configState.SetInitialized's docstring and the
	// "initialized" entry in the full health checker installed by
	// setupInfrastructureServers.
	if err := finishIterationStartup(setup, state, infra, reloadAuthority, health, wiring.renderService.FirstGraphPublished, logger); err != nil {
		return nil, err
	}
	return &liveIteration{setup: setup, authority: reloadAuthority, logger: logger}, nil
}

// completeFailedStartup tears a failed startup down. A startup that a reload
// cut short reports that reload so the sequence restarts from it at once.
func completeFailedStartup(setup *componentSetup, authority *iterationReloadAuthority, startupErr error, logger *slog.Logger) error {
	reload := authority.Latest()
	interrupted := reload != nil && isContextTermination(setup.IterCtx, startupErr)
	if interrupted {
		startupErr = nil
	}
	err := completeIteration(setup, startupErr, logger)
	if interrupted && err == nil {
		return &startupInterruptedError{reload: reload}
	}
	return err
}

// warmUp is the successor's follower warmer as the hand-over sees it.
type warmUp interface {
	Warmed() <-chan struct{}
	RenderInFlight() <-chan struct{}
}

// takeOverLeadership retires the predecessor's term once this iteration's
// graph is warm, right before this one enters election.
func takeOverLeadership(ctx context.Context, previous *liveIteration, warmer warmUp, shuttingDown func() bool, logger *slog.Logger) {
	if previous == nil {
		return
	}
	timer := time.NewTimer(handoverWarmupWait)
	defer timer.Stop()
	awaitHandover(ctx, warmer, timer.C, shuttingDown, logger)
	previous.retireLeadership()
}

// awaitHandover waits for a warm graph and reports a cold take-over.
func awaitHandover(ctx context.Context, warmer warmUp, deadline <-chan time.Time, shuttingDown func() bool, logger *slog.Logger) {
	started := time.Now()
	if awaitWarmGraph(ctx, warmer, deadline) || ctx.Err() != nil {
		return
	}
	waited := time.Since(started)
	if shuttingDown() {
		logger.Info("Taking leadership with a cold graph while the controller shuts down", "waited", waited)
		return
	}
	logger.Warn("Taking leadership with a cold graph: the warm-up render left no graph. "+
		"The first render as leader rebuilds the whole configuration and deployments wait for it. "+
		"Fix the render-warmer error logged before this.",
		"waited", waited)
}

// awaitWarmGraph reports whether the graph is warm. Past the deadline it still
// waits for the render in flight: that is the warm-up progressing, and a cold
// leader would discard and repeat it while nothing deploys.
func awaitWarmGraph(ctx context.Context, warmer warmUp, deadline <-chan time.Time) bool {
	select {
	case <-warmer.Warmed():
		return true
	case <-ctx.Done():
		return false
	case <-deadline:
	}
	if running := warmer.RenderInFlight(); running != nil {
		select {
		case <-running:
		case <-ctx.Done():
			return false
		}
	}
	select {
	case <-warmer.Warmed():
		return true
	default:
		return false
	}
}

func loadAcceptedIterationConfig(
	setup *componentSetup,
	state *configState,
	startup *configchange.ReloadRequest,
	k8sClient *client.Client,
	crdName string,
	secretName string,
	typeBootstrapper validator.TypeBootstrapper,
	infra *persistentInfra,
	logger *slog.Logger,
) (*configchange.ValidatedSnapshot, *InitialConfigBundle, error) {
	// The first iteration loads live state. Later iterations consume the exact
	// accepted snapshot carried by the reload request.
	bundle, err := loadIterationBundle(setup.IterCtx, k8sClient, crdName, secretName, state, startup, logger)
	if err != nil {
		return nil, nil, err
	}

	// A direct config reload already passed every live validator. Fresh startup
	// and served-CRD changes resolve against discovery and run the load gate.
	cfg, resolution, alreadyValidated, err := effectiveConfigForIteration(
		setup.IterCtx, startup, bundle.Config, k8sClient, logger)
	if err != nil {
		return nil, nil, err
	}
	if !alreadyValidated {
		err := validateInitialConfig(setup.IterCtx, cfg, bundle, k8sClient, typeBootstrapper, infra.loadGateBudget(), logger)
		// The event bus isn't running yet, so the load gate reports directly.
		setup.MetricsComponent.Metrics().RecordLoadGate(err == nil)
		if err != nil {
			return nil, nil, fmt.Errorf("initial HAProxyTemplateConfig %q failed validation on load: %w", crdName, err)
		}
	}

	active := &configchange.ValidatedSnapshot{
		RawConfig:          bundle.Config,
		Config:             cfg,
		Resolution:         resolution,
		TemplateConfig:     bundle.CRD,
		ConfigVersion:      bundle.ConfigVersion,
		Credentials:        bundle.Credentials,
		CredentialsVersion: bundle.CredentialsVersion,
		Sources:            bundle.Sources,
	}
	setup.ConfigChangeHandler.SetInitialSnapshot(active)
	configureEffectiveConfig(setup.IterCtx, active, k8sClient, setup, infra, logger)
	finalizeConfigLoad(state, setup, bundle.ConfigVersion, bundle.CredentialsVersion)
	return active, bundle, nil
}

func loadIterationBundle(
	ctx context.Context,
	k8sClient *client.Client,
	crdName string,
	secretName string,
	state *configState,
	startup *configchange.ReloadRequest,
	logger *slog.Logger,
) (*InitialConfigBundle, error) {
	if startup != nil {
		return initialConfigBundleFromSnapshot(startup.Snapshot)
	}
	return waitAndLoadInitialConfig(ctx, k8sClient, crdName, secretName, state, logger)
}

func effectiveConfigForIteration(
	ctx context.Context,
	startup *configchange.ReloadRequest,
	rawCfg *coreconfig.Config,
	k8sClient *client.Client,
	logger *slog.Logger,
) (*coreconfig.Config, *coreconfig.Resolution, bool, error) {
	if startup != nil && !startup.Reasons.Has(configchange.ReloadReasonEffectiveConfig) {
		return startup.Snapshot.Config, startup.Snapshot.Resolution, true, nil
	}
	effective, resolution, err := resolveEffectiveConfig(ctx, rawCfg, k8sClient, logger)
	if err != nil {
		return nil, nil, false, fmt.Errorf("resolving effective config: %w", err)
	}
	return effective, resolution, false, nil
}

func setupLeadershipAndWebhook(
	procCtx context.Context,
	setup *componentSetup,
	infra *persistentInfra,
	cfg *coreconfig.Config,
	webhookCertDir string,
	webhookAdmissionTimeouts WebhookAdmissionTimeouts,
	webhookPort int,
	k8sClient *client.Client,
	dryrunValidator *dryrunvalidator.Component,
	pluggableMgrCleanup *sharedCleanup,
	logger *slog.Logger,
) error {
	logger.Info("Stage 6: Initializing leader election")
	state, err := setupLeaderElection(setup, cfg, k8sClient, logger)
	setup.LeaderState = state
	infra.leadership.Store(state)
	if err != nil {
		return err
	}
	return maybeSetupWebhook(procCtx, infra, cfg, webhookCertDir, webhookAdmissionTimeouts, webhookPort, setup, k8sClient, dryrunValidator, pluggableMgrCleanup, logger)
}

func finishIterationStartup(
	setup *componentSetup,
	state *configState,
	infra *persistentInfra,
	authority *iterationReloadAuthority,
	health introspection.HealthCheckFunc,
	graphPublished func() bool,
	logger *slog.Logger,
) error {
	if err := iterationContextError(setup.IterCtx); err != nil {
		return err
	}
	// Serving before EnableReinitialization opens reload delivery: a reload
	// landing in between must start the successor, not cancel this iteration.
	authority.MarkServing()
	markIterationInitialized(setup, state, infra, logger)
	id := state.iterationID
	infra.markServing(&servingIteration{
		id: id, health: health, leading: setup.LeaderState.isLeading, graphPublished: graphPublished,
	})
	setup.AddCleanup(func() { infra.clearServing(id) })
	return nil
}

func waitForIterationExit(
	setup *componentSetup,
	reloadAuthority *iterationReloadAuthority,
	logger *slog.Logger,
) (*configchange.ReloadRequest, error) {
	<-setup.IterCtx.Done()
	if reload := reloadAuthority.Latest(); reload != nil {
		logger.Info("Configuration change detected, triggering reinitialization",
			"new_config_version", reload.Snapshot.ConfigVersion)
		logger.Info("Reinitialization triggered - starting new iteration")
		return reload, nil
	}
	err := iterationContextError(setup.IterCtx)
	logger.Info("Controller iteration cancelled", "reason", err)
	return nil, err
}

func markIterationInitialized(setup *componentSetup, state *configState, infra *persistentInfra, logger *slog.Logger) {
	setup.ConfigChangeHandler.EnableReinitialization()
	state.SetInitialized()
	infra.NoteInitialized(state.iterationID)
	// Counted on arrival rather than on teardown: an iteration that never
	// finishes starting is not one the fleet got back, and this metric answers
	// "how often has the controller rebuilt itself", which is what an operator
	// correlates a propagation gap against.
	//
	// Add the running total, never Inc: a reinit swaps the metrics registry, so
	// this counter is born at zero each time. Inc made it report 1 after every
	// rebuild, and any two samples therefore differed by nothing.
	if setup.MetricsComponent != nil {
		recordReinitializations(setup.MetricsComponent.Metrics(), state.iterationID)
	}
	logger.Info("Controller iteration initialized successfully - entering event loop")
}

// recordReinitializations publishes the process's running rebuild total onto a
// counter that a reinit has just re-created at zero.
func recordReinitializations(m *metrics.Metrics, id iterationID) {
	if m == nil || m.ControllerReinitializationsTotal == nil || id <= 1 {
		return
	}
	m.ControllerReinitializationsTotal.Add(float64(id - 1))
}

func teardownIteration(setup *componentSetup, logger *slog.Logger) error {
	setup.Cancel()
	if setup.LeaderState != nil {
		setup.LeaderState.cancel()
	}
	err := waitForGoroutinesToFinish(setup.ErrGroup, logger, "Iteration teardown", ShutdownTimeout)
	var timeoutErr *iterationTeardownTimeoutError
	if errors.As(err, &timeoutErr) {
		return err
	}
	if isContextTermination(setup.IterCtx, err) {
		err = nil
	}
	setup.RunCleanups()
	return err
}

func completeIteration(setup *componentSetup, iterationErr error, logger *slog.Logger) error {
	if iterationErr != nil && setup.CancelCause != nil && !isContextTermination(setup.IterCtx, iterationErr) {
		setup.CancelCause(iterationErr)
	}
	cause := context.Cause(setup.IterCtx)
	result := errors.Join(iterationErr, teardownIteration(setup, logger))
	if cause != nil && !errors.Is(cause, context.Canceled) && !errors.Is(result, cause) {
		result = errors.Join(result, cause)
	}
	return result
}

// maybeSetupWebhook sets up the webhook server when the chart has mounted a
// TLS cert directory. The cert-dir path — not the CRD's watchedResources
// section — is the operative gate: the chart's `webhook.enabled` toggle
// controls the cert Secret mount + WEBHOOK_CERT_DIR, and the
// ValidatingWebhookConfiguration may cover HAProxyTemplateConfig admission
// even when no watched-resource has `enableValidationWebhook: true`. Without a
// cert dir, the server can't bind the TLS listener regardless of what we'd
// want to register.
func maybeSetupWebhook(
	procCtx context.Context,
	infra *persistentInfra,
	cfg *coreconfig.Config,
	webhookCertDir string,
	webhookAdmissionTimeouts WebhookAdmissionTimeouts,
	webhookPort int,
	setup *componentSetup,
	k8sClient *client.Client,
	dryrunValidator *dryrunvalidator.Component,
	pluggableMgrCleanup *sharedCleanup,
	logger *slog.Logger,
) error {
	if webhookCertDir == "" {
		logger.Debug("No webhook TLS cert directory configured; skipping webhook setup")
		return nil
	}
	logger.Info("Stage 7: Setting up webhook validation")
	return setupWebhook(procCtx, setup.IterCtx, infra, cfg, webhookCertDir, webhookAdmissionTimeouts, webhookPort, k8sClient, dryrunValidator, pluggableMgrCleanup, logger, setup.MetricsComponent.Metrics(), setup.CancelCause, setup.ErrGroup)
}
