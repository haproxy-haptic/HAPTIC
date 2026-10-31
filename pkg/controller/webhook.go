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
	"time"

	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/restmapper"

	"github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	dryrunvalidator "github.com/haproxy-haptic/HAPTIC/pkg/controller/dryrunvalidator"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/pipeline"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/proposalvalidator"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/renderer"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/resourcewatcher"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/timeouts"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/validation"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/webhook"
	coreconfig "github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/client"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	pkgwebhook "github.com/haproxy-haptic/HAPTIC/pkg/webhook"
)

// webhookWriteTimeoutHeadroom pads the HTTPS write deadline past the slowest
// admission path, so a validation that uses its full budget still gets its
// response written instead of being cut off mid-body.
const webhookWriteTimeoutHeadroom = 2 * time.Second

// WebhookAdmissionTimeouts contains the controller-side deadline for
// watched-resource admission. (HAProxyTemplateConfig admission no longer
// exists — ADR-0016.)
type WebhookAdmissionTimeouts struct {
	Resource time.Duration
}

// setupWebhook creates and starts the webhook component if webhook validation is enabled.
//
// This function:
//  1. Extracts webhook rules from configuration
//  2. Uses the iteration's compiled template engine for dry-run validation
//  3. Starts DryRunValidator component
//  4. Creates and starts webhook component with mounted certificates
//
// The webhook component validates Kubernetes resources via admission webhook.
// Certificates are expected to be mounted at /etc/webhook/certs/ (provided by Helm).
func setupWebhook(
	procCtx context.Context,
	iterCtx context.Context,
	infra *persistentInfra,
	cfg *coreconfig.Config,
	webhookCertDir string,
	admissionTimeouts WebhookAdmissionTimeouts,
	webhookPort int,
	k8sClient *client.Client,
	dryrunValidator *dryrunvalidator.Component, // Pre-created validator (may be nil)
	pluggableMgrCleanup *sharedCleanup,
	logger *slog.Logger,
	metricsRecorder webhook.MetricsRecorder,
	cancelCause context.CancelCauseFunc,
	errGroup *errgroup.Group,
) error {
	resourceAdmissionTimeout := effectiveResourceAdmissionTimeout(admissionTimeouts.Resource)

	// Extract webhook rules from config
	rules := webhook.ExtractWebhookRules(cfg)
	if len(rules) == 0 {
		infra.webhookMu.Lock()
		sharedServer := infra.WebhookServer
		infra.webhookMu.Unlock()
		if sharedServer == nil {
			logger.Debug("No webhook rules extracted; webhook setup skipped")
			return nil
		}
		logger.Info("No webhook rules extracted; clearing the persistent validator table")
	} else {
		logger.Info("Webhook validation enabled", "rule_count", len(rules))
	}

	var mapper meta.RESTMapper
	if len(rules) > 0 {
		logger.Debug("Creating RESTMapper for resource kind resolution")
		discoveryClient := k8sClient.Clientset().Discovery()
		mapper = restmapper.NewDeferredDiscoveryRESTMapper(
			memory.NewMemCacheClient(discoveryClient),
		)
	}

	// Keep the admission listener bound while iterations replace validator generations.
	sharedServer, err := infra.EnsureWebhookServer(procCtx, &pkgwebhook.ServerConfig{
		Port:         webhookPort,
		Path:         webhook.DefaultWebhookPath,
		CertDir:      webhookCertDir,
		ReadTimeout:  timeouts.HTTPServerTimeout,
		WriteTimeout: max(resourceAdmissionTimeout, timeouts.HTTPServerTimeout) + webhookWriteTimeoutHeadroom,
	}, logger)
	if err != nil {
		return fmt.Errorf("starting webhook listener: %w", err)
	}
	serverRun := infra.currentWebhookRun()
	if serverRun == nil {
		return errors.New("persistent webhook server has no completion owner")
	}
	monitorPersistentWebhookRun(
		procCtx, iterCtx, serverRun,
		errGroup, logger, cancelCause,
	)
	var onGenerationRetired func()
	if len(rules) > 0 && pluggableMgrCleanup != nil {
		onGenerationRetired = pluggableMgrCleanup.Retain()
	}

	// Create webhook component with DryRunValidator for direct validation (no scatter-gather).
	// It adopts the shared listener above and installs this iteration's
	// validator table onto it.
	webhookComponent := webhook.New(
		logger,
		&webhook.Config{
			Port:                     webhookPort,
			Path:                     webhook.DefaultWebhookPath,
			Rules:                    rules,
			CertDir:                  webhookCertDir,
			DryRunValidator:          dryrunValidator,
			ResourceAdmissionTimeout: resourceAdmissionTimeout,
			Server:                   sharedServer,
			OnGenerationRetired:      onGenerationRetired,
		},
		mapper,
		metricsRecorder,
	)

	// Start webhook component (tracked by errgroup for graceful shutdown)
	startInErrGroup(errGroup, iterCtx, logger, cancelCause, "webhook component", webhookComponent.Start)

	select {
	case <-webhookComponent.Listening():
		logger.Info("Webhook component listening", "port", webhookPort)
		return nil
	case <-iterCtx.Done():
		return context.Cause(iterCtx)
	}
}

func effectiveResourceAdmissionTimeout(configured time.Duration) time.Duration {
	if configured <= 0 {
		return webhook.DefaultResourceAdmissionTimeout
	}
	return configured
}

func monitorPersistentWebhookRun(
	procCtx context.Context,
	iterCtx context.Context,
	serverRun *persistentServerRun,
	errGroup *errgroup.Group,
	logger *slog.Logger,
	iterationCancel context.CancelCauseFunc,
) {
	startInErrGroup(errGroup, iterCtx, logger, iterationCancel, "persistent webhook server", func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return nil
		case <-serverRun.Done():
			if serverRun.stopping.Load() {
				<-ctx.Done()
				return nil
			}
			if err := context.Cause(procCtx); err != nil {
				return err
			}
			if err := context.Cause(ctx); err != nil {
				return err
			}
			err := serverRun.Wait()
			if err == nil {
				err = errors.New("server exited without an error")
			}
			return &persistentWebhookServerError{err: err}
		}
	})
}

// errNoWebhookRules reports that no watched resource enables admission
// validation — the caller continues with a nil DryRunValidator rather than
// treating it as a failure.
var errNoWebhookRules = errors.New("no watched-resource webhook rules")

// createDryRunValidator builds admission validation when watched-resource rules exist.
func createDryRunValidator(
	cfg *coreconfig.Config,
	storeProvider stores.StoreProvider,
	wiring *reconciliationWiring,
	outputValidator pipeline.RenderedOutputValidator,
	logger *slog.Logger,
) (*dryrunvalidator.Component, error) {
	rules := webhook.ExtractWebhookRules(cfg)
	if len(rules) == 0 {
		logger.Debug("No watched-resource webhook rules; DryRunValidator skipped")
		return nil, errNoWebhookRules
	}

	logger.Debug("Creating webhook validators", "watched_resource_rules", len(rules))

	if wiring.renderService == nil {
		return nil, errors.New("dry-run validation requires the reconciliation render service")
	}
	renderService := wiring.renderService

	// Create ValidationService (pure service for validation)
	// Use strict DNS validation for webhook (catch DNS issues before admission)
	dirConfig := extractValidationDirConfig(&cfg.Dataplane)
	validationService := validation.NewValidationService(&validation.ValidationServiceConfig{
		Logger:            logger,
		SkipDNSValidation: false, // Strict mode for webhook validation
		CheckObserver:     wiring.admissionChecks,
		BaseDir:           dirConfig.BaseDir,
		MapsDir:           dirConfig.MapsDir,
		SSLCertsDir:       dirConfig.SSLCertsDir,
		GeneralDir:        dirConfig.GeneralDir,
	})

	var admissionStoreProvider func(context.Context, stores.StoreProvider) (stores.StoreProvider, bool, error)
	var observedStoreProvider func(context.Context, stores.StoreProvider) (stores.StoreProvider, error)
	if wiring.inputIsolation != nil {
		admissionStoreProvider = wiring.inputIsolation.IsolatedInputs
		observedStoreProvider = wiring.inputIsolation.ObservedInputs
	}
	return buildDryRunValidator(renderService, validationService, storeProvider, outputValidator, wiring.gvrMapper, cfg.WatchedResources, wiring.publishedCurrentFiles.get, wiring.freshStoreProvider, admissionStoreProvider, observedStoreProvider, logger)
}

// buildDryRunValidator connects the admission service to resource overlay handling.
func buildDryRunValidator(
	renderService *renderer.RenderService,
	validationService *validation.ValidationService,
	baseStoreProvider stores.StoreProvider,
	outputValidator pipeline.RenderedOutputValidator,
	gvrMapper meta.RESTMapper,
	watchedResources map[string]coreconfig.WatchedResource,
	currentFilesProvider func() (map[string]string, error),
	freshStoreProvider func(context.Context) (stores.StoreProvider, error),
	admissionStoreProvider func(context.Context, stores.StoreProvider) (stores.StoreProvider, bool, error),
	observedStoreProvider func(context.Context, stores.StoreProvider) (stores.StoreProvider, error),
	logger *slog.Logger,
) (*dryrunvalidator.Component, error) {
	pipelineInstance := pipeline.New(&pipeline.PipelineConfig{
		Renderer:        renderService,
		Validator:       validationService,
		OutputValidator: outputValidator,
		Logger:          logger,
	})

	proposalValidatorInstance := proposalvalidator.NewService(&proposalvalidator.ServiceConfig{
		Pipeline:               pipelineInstance,
		BaseStoreProvider:      baseStoreProvider,
		FreshStoreProvider:     freshStoreProvider,
		AdmissionStoreProvider: admissionStoreProvider,
		ObservedStoreProvider:  observedStoreProvider,
		CurrentFilesProvider:   currentFilesProvider,
		Logger:                 logger,
	})

	// The admission webhook only validates the *submitted* resource
	// (Ingress / HTTPRoute / etc.) by rendering with an overlay store and
	// checking the result. The chart's embedded `validationTests` are NOT
	// run here — they are chart-author scenarios with their own fixtures,
	// executed in CI via `haptic validate` / `make test-templates`.
	return dryrunvalidator.New(&dryrunvalidator.ComponentConfig{
		ProposalValidator: proposalValidatorInstance,
		RESTMapper:        gvrMapper,
		WatchedResources:  watchedResources,
		Logger:            logger,
	})
}

// setupReconciliation creates and starts the reconciliation components (Stage 5).
//
// The Reconciler debounces resource changes and triggers reconciliation events.
// The Coordinator orchestrates the render-validate pipeline by calling Pipeline.Execute()
// directly and publishing TemplateRenderedEvent for downstream components like
// DeploymentScheduler.
//
// All components are started after initial resource synchronization to ensure we
// have a complete view of the cluster state before beginning reconciliation cycles.
//
// Returns the reconciliation wiring for use in leader election callbacks.
func setupReconciliation(
	setup *componentSetup,
	cfg *coreconfig.Config,
	crd *v1alpha1.HAProxyTemplateConfig,
	sources []events.ConfigSourceRef,
	creds *coreconfig.Credentials,
	k8sClient *client.Client,
	resourceWatcher *resourcewatcher.ResourceWatcherComponent,
	currentFiles *currentFilesAuthority,
	storeProvider stores.StoreProvider,
	outputValidator pipeline.RenderedOutputValidator,
	logger *slog.Logger,
) (*reconciliationWiring, error) {
	wiring, err := createReconciliationComponents(setup, cfg, crd, creds.SecretFileSealKey(), k8sClient, resourceWatcher, currentFiles, storeProvider, outputValidator, logger)
	if err != nil {
		return nil, err
	}

	// Start all-replica components in background
	// Leader-only components (Deployer, DeploymentScheduler, ConfigPublisher) are NOT started here
	// Note: Components already subscribed during construction, so they're ready to receive events
	startReconciliationComponents(setup.IterCtx, setup.Registry, logger, setup.CancelCause, setup.ErrGroup)

	// Publish initial config and credentials events
	// These events are buffered by EventBus until Start() is called in the main controller loop
	// This ensures reconciliation components (especially Discovery) receive the initial state
	// even though they were created after the initial CRD/Secret watcher events
	// Note: We pass the actual CRD (not nil) so ConfigPublisher can cache it for creating HAProxyCfg resources
	initialValidated := events.NewConfigValidatedEvent(cfg, crd, "initial", "initial")
	initialValidated.Sources = sources
	setup.Bus.Publish(initialValidated)
	logger.Debug("Published initial ConfigValidatedEvent (buffered until EventBus.Start())")

	setup.Bus.Publish(events.NewCredentialsUpdatedEvent(creds, "initial"))
	logger.Debug("Published initial CredentialsUpdatedEvent (buffered until EventBus.Start())")

	// Trigger initial reconciliation to bootstrap the pipeline
	// This ensures at least one reconciliation cycle runs even with 0 resources
	// A new correlation ID is generated to trace this initial reconciliation cycle
	// Initial sync is NOT coalescible - it must be processed to establish initial state
	initialReconciliation := events.NewReconciliationTriggeredEvent("initial_sync_complete", false, events.WithNewCorrelation())
	setup.Bus.Publish(initialReconciliation)
	logger.Debug("Published initial reconciliation trigger (buffered until EventBus.Start())",
		"correlation_id", initialReconciliation.CorrelationID())

	return wiring, nil
}
