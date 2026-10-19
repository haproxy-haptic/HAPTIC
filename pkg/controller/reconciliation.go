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
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/restmapper"

	"github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/configchange"
	ctrlconfigpublisher "github.com/haproxy-haptic/HAPTIC/pkg/controller/configpublisher"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/deployer"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/discovery"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/eventemitter"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/helpers"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/httpstore"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/inputisolation"
	leaderelectionctrl "github.com/haproxy-haptic/HAPTIC/pkg/controller/leaderelection"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/metrics"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/names"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/pipeline"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/proposalvalidator"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/reconciler"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/renderer"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendergate"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/resourceapplier"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/resourcewatcher"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/statusapplier"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/timeouts"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/validation"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/warmer"
	coreconfig "github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/generated/clientset/versioned"
	informers "github.com/haproxy-haptic/HAPTIC/pkg/generated/informers/externalversions"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/client"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/configpublisher"
	"github.com/haproxy-haptic/HAPTIC/pkg/lifecycle"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
)

// reconciliationWiring carries the few construction outputs later startup
// stages consume; the components themselves live on through the lifecycle
// registry and their event-bus subscriptions — everything not referenced
// again after construction is deliberately NOT carried here.
//
// Consumers:
//   - httpStore/capabilities/engine/typedResourceTypes/gvrMapper/publishedCurrentFiles:
//     read by createDryRunValidator when the webhook validators are wired up.
type reconciliationWiring struct {
	// renderService is the reconciliation render service. Admission renders
	// on it too: its graph is warm on every replica (leader renders, follower
	// warmer), so a dry-run rebases its overlay on the current root instead
	// of rebuilding the graph per request, and it sees the fleet capabilities
	// the deploy side sources into it.
	renderService         *renderer.RenderService
	publishedCurrentFiles *publishedAuxFiles
	freshStoreProvider    func(context.Context) (stores.StoreProvider, error)
	inputIsolation        *inputisolation.Service
	gvrMapper             meta.RESTMapper
	// warmer is the follower warmer; a hand-over waits for it so the new
	// leader's first render is warm.
	warmer warmUp
	// admissionChecks records the webhook's `haproxy -c` runs.
	admissionChecks validation.CheckObserver
}

// renderInputs routes the deploy side's two feedback channels: the plan the
// fleet ACKed belongs to the reconciliation render alone, while the fleet's
// capabilities go to every render that feeds a gate.
type renderInputs struct {
	deployer.AckedPlanSink
	deployer.FleetCapabilitiesSink
}

// leadershipFence builds the epoch every apply is fenced by and hands it to
// the leader-election component through setup, so both halves share one
// counter. Nil without leader election: a single writer needs no fence.
//
// Standing down cancels the current election attempt, so the Lease is
// released (ReleaseOnCancel) and superviseElection re-enters election in
// place: nothing short of a fresh acquisition claims a fresh epoch, and the
// iteration — its stores and admission validators — keeps running.
func leadershipFence(setup *componentSetup, cfg *coreconfig.Config, k8sClient *client.Client, logger *slog.Logger) deployer.LeadershipFence {
	if !cfg.Controller.LeaderElection.Enabled {
		return nil
	}
	podName, podNamespace := leaderIdentity(k8sClient, logger)
	epoch := leaderelectionctrl.NewLeaseEpoch(k8sClient.Clientset(),
		podNamespace, cfg.Controller.LeaderElection.LeaseName, podName, logger)
	setup.ElectionRestart = make(chan struct{}, 1)
	setup.LeaderEpoch = leaderelectionctrl.NewTerm(epoch, func(reason string) {
		logger.Error("Giving up leadership, re-electing", "reason", reason, "identity", podName)
		select {
		case setup.ElectionRestart <- struct{}{}:
		default:
		}
	})
	return setup.LeaderEpoch
}

// detectLocalCapabilities seeds the template `capabilities` input from the
// controller image's own HAProxy binary. Discovery replaces it with the fleet's
// lowest reported version once the pods answer; the chart pins the same
// haproxyVersion for both images, so the seed is right on a healthy fleet and
// only an in-flight upgrade moves it.
func detectLocalCapabilities(ctx context.Context, logger *slog.Logger) (
	*renderer.CapabilitiesFanout, error,
) {
	localVersion, err := dataplane.DetectLocalVersionContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("detecting local HAProxy version: %w", err)
	}
	capabilities := renderer.NewCapabilitiesFanout(dataplane.CapabilitiesFromVersion(localVersion))

	local := capabilities.Capabilities()
	logger.Info("Detected local HAProxy version",
		"version", localVersion.Full,
		"supports_crt_list", local.SupportsCrtList,
		"supports_map_storage", local.SupportsMapStorage,
		"supports_general_storage", local.SupportsGeneralStorage)
	return capabilities, nil
}

// createReconciliationComponents creates all reconciliation components and
// registers them with the lifecycle registry (setup.Registry). It returns
// only the slim reconciliationWiring — every component not referenced again
// after construction lives on through the registry and its event-bus
// subscriptions.
func createReconciliationComponents(
	setup *componentSetup,
	cfg *coreconfig.Config,
	crd *v1alpha1.HAProxyTemplateConfig,
	k8sClient *client.Client,
	resourceWatcher *resourcewatcher.ResourceWatcherComponent,
	currentFiles *currentFilesAuthority,
	storeProvider stores.StoreProvider,
	outputValidator pipeline.RenderedOutputValidator,
	logger *slog.Logger,
) (*reconciliationWiring, error) {
	// Create Reconciler. It fires immediately on every resource/HTTP event;
	// there is no reconciler-level refractory. Batching is per-watcher
	// (debounceInterval), reload throttling is the deployer's
	// minDeploymentInterval (which the runtime-eligible fast path bypasses).
	reconcilerComponent := reconciler.New(setup.Bus, logger)

	// The controller image's own HAProxy binary seeds the template
	capabilities, err := detectLocalCapabilities(setup.IterCtx, logger)
	if err != nil {
		return nil, err
	}

	// Get haproxy-pods store for pod-maxconn calculations in templates
	haproxyPodStore := resourceWatcher.GetStore(names.HAProxyPodsResourceType)
	if haproxyPodStore == nil {
		return nil, fmt.Errorf("%s store not found (should be auto-injected)", names.HAProxyPodsResourceType)
	}

	// Create HTTPStore component for dynamic HTTP content fetching
	// This component manages periodic refreshes and content validation coordination
	// Eviction maxAge is 2x drift prevention interval to catch stale URLs
	driftInterval := cfg.Dataplane.GetDriftPreventionInterval()
	httpStoreEvictionMaxAge := 2 * driftInterval
	httpStoreComponent := httpstore.New(setup.Bus, logger, httpStoreEvictionMaxAge)

	wiring, err := buildEngineWiring(setup.IterCtx, cfg, k8sClient, logger)
	if err != nil {
		return nil, err
	}
	engine, err := helpers.NewEngineFromConfigWithOptions(cfg, nil, nil, wiring.Declarations, helpers.EngineOptions{})
	if err != nil {
		return nil, fmt.Errorf("creating template engine for reconciliation: %w", err)
	}

	// Create RenderService with full dependencies for production rendering.
	// TypedResourceTypes feeds the render-time path that wraps each store
	// snapshot into the *[]*<generated-struct> shape Scriggo's typed globals
	// are declared against.
	renderService := renderer.NewRenderService(&renderer.RenderServiceConfig{
		Engine:                    engine,
		Config:                    cfg,
		Logger:                    logger,
		Capabilities:              capabilities.Capabilities(),
		HAProxyPodStore:           haproxyPodStore,
		HTTPStoreComponent:        httpStoreComponent,
		TypedResourceTypes:        wiring.TypedResourceTypes,
		AdmissionSlotWaitObserver: setup.MetricsComponent.Metrics().RecordAdmissionRenderSlotWait,
		// Its reconcile callers, the Coordinator and the Warmer, render under
		// the leadership term and the iteration lifecycle respectively.
		ColdRendersRunToCompletion: true,
	})
	setup.AddCleanup(func() {
		if err := renderService.RetireIncrementalCache(); err != nil {
			logger.Error("Retiring incremental render cache failed", "error", err)
		}
	})

	// Input acceptance requires complete validation before publication (ADR-0032).
	checks := newCheckServices(cfg, logger, setup.MetricsComponent.Metrics())
	inputSelector := newInputSelector(setup, cfg, pipeline.New(&pipeline.PipelineConfig{
		Renderer:        renderService,
		Validator:       checks.input,
		OutputValidator: outputValidator,
		CommitValidator: checks.input,
		Logger:          logger,
	}), logger)
	proposalPipeline := pipeline.New(&pipeline.PipelineConfig{
		Renderer:        renderService,
		Validator:       checks.httpContent,
		OutputValidator: outputValidator,
		Logger:          logger,
	})

	// Coordinator: leader-side render + deploy.
	coordinatorComponent := reconciler.NewCoordinator(&reconciler.CoordinatorConfig{
		EventBus:       setup.Bus,
		Pipeline:       inputSelector,
		StoreProvider:  storeProvider,
		CurrentFiles:   currentFiles,
		HTTPAcceptance: httpStoreComponent,
		GraphWarm:      renderService.IncrementalGraphWarm,
		Metrics:        setup.MetricsComponent.Metrics(),
		Logger:         logger,
	})

	// Followers establish the same validated input boundary before serving admission.
	warmerComponent := warmer.New(&warmer.Config{
		EventBus:           setup.Bus,
		Pipeline:           inputSelector,
		StoreProvider:      storeProvider,
		CurrentFilesSource: currentFiles.PublishedExactSource,
		GraphWarm:          renderService.IncrementalGraphWarm,
		Metrics:            setup.MetricsComponent.Metrics(),
		Logger:             logger,
	})

	// The event adapter validates HTTP-store content promotion.
	proposalValidatorComponent := proposalvalidator.New(setup.Bus, &proposalvalidator.ServiceConfig{
		Pipeline:              proposalPipeline,
		BaseStoreProvider:     storeProvider,
		AcceptedStoreProvider: inputSelector.AcceptedInputs,
		CurrentFilesProvider:  currentFiles.publishedSnapshot,
		Logger:                logger,
	})

	// RenderGate: the reconcile path's `haproxy -c`, off the wall clock, on its
	// own semaphore slot so admission never queues behind a fleet-sized check.
	renderGateComponent := rendergate.New(&rendergate.Config{
		EventBus: setup.Bus,
		Logger:   logger,
		Checker:  rendergate.ServiceChecker{Service: checks.renderGate},
		Metrics:  setup.MetricsComponent.Metrics(),
	})

	// One constructor, wired inside the deployer package: the connections
	// between these three used to be optional setters a caller could forget.
	capabilities.Add(renderService)
	deployStack := deployer.NewDeployStack(setup.Bus, cfg, logger,
		setup.MetricsComponent.Metrics(),
		renderInputs{AckedPlanSink: renderService, FleetCapabilitiesSink: capabilities},
		leadershipFence(setup, cfg, k8sClient, logger), setup.AgentTLS)

	// Create Discovery component and set pod store
	discoveryComponent := discovery.New(setup.Bus, logger, discovery.WithTLS(setup.AgentTLS))
	podStore := resourceWatcher.GetStore(names.HAProxyPodsResourceType)
	if podStore == nil {
		return nil, fmt.Errorf("%s store not found (should be auto-injected)", names.HAProxyPodsResourceType)
	}
	discoveryComponent.SetPodStore(podStore)

	// Create Config Publisher (pure publisher + event adapter)
	// Publishes runtime config resources after successful validation
	crdClientset, err := versioned.NewForConfig(k8sClient.RestConfig())
	if err != nil {
		return nil, fmt.Errorf("creating CRD clientset: %w", err)
	}

	// Create publisher with informer-backed listers for cached reads
	purePublisher, stopPublisherInformers, err := createConfigPublisher(
		setup.IterCtx, crdClientset, k8sClient, cfg.Dataplane.GetDriftPreventionInterval(), logger)
	if err != nil {
		return nil, err
	}
	setup.AddCleanup(stopPublisherInformers)
	deployStack.EnableRetainedRecovery(purePublisher,
		rendergate.ServiceChecker{Service: checks.retained}, crd, setup.MetricsComponent.Metrics(), cfg.Controller.LeaderElection.LeaseName)
	configPublisherComponent := ctrlconfigpublisher.New(purePublisher, setup.Bus, logger,
		ctrlconfigpublisher.WithPublishInterval(cfg.Dataplane.GetConfigPublishInterval()),
		ctrlconfigpublisher.WithTemplateConfig(crd),
	)

	// Create Status Updater (updates HAProxyTemplateConfig CRD status with validation results)
	// This allows users to see validation errors via `kubectl describe haproxytemplateconfig`
	statusUpdaterComponent := configchange.NewStatusUpdater(crdClientset, k8sClient.Clientset(), setup.Bus, logger)

	// Build a RESTMapper from the cluster's discovery so the status/resource
	// appliers resolve apiVersion+kind → GroupVersionResource from authoritative
	// cluster data, never a hardcoded or guessed plural (RULE #1). Shared by both
	// appliers; the deferred mapper fetches discovery lazily on first use.
	gvrMapper := restmapper.NewDeferredDiscoveryRESTMapper(
		memory.NewMemCacheClient(k8sClient.Clientset().Discovery()),
	)

	// Create StatusApplier (applies template-driven status patches to Kubernetes resources via SSA)
	// All-replica: subscribes in constructor to cache patches from renders; only the leader applies.
	statusApplierComponent := statusapplier.New(&statusapplier.Config{
		EventBus:      setup.Bus,
		DynamicClient: k8sClient.DynamicClient(),
		GVRResolver:   statusapplier.NewRestMapperResolver(gvrMapper),
		Logger:        logger,
		SelfWrites:    setup.SelfWrites,
	})

	resourceApplierComponent := newResourceApplier(crd, cfg, k8sClient, gvrMapper, setup.Bus, logger)

	// EventEmitter forwards template-recorded Kubernetes Events (recordEvent, e.g.
	// a RouteConflict Warning on an Ingress) to the API server. All-replica:
	// subscribes on every replica but only the leader emits (the source
	// ReconciliationCompletedEvent is leader-only, and an internal leader flag
	// double-gates it).
	eventEmitterComponent := eventemitter.New(&eventemitter.Config{
		EventBus:   setup.Bus,
		KubeClient: k8sClient.Clientset(),
		Logger:     logger,
	})

	// Register components with the lifecycle registry.
	// Coordinator is leader-only because it performs rendering (state changes).
	// DriftMonitor is leader-only to avoid multi-replica race conditions.
	// StatusUpdater is leader-only to avoid API conflicts from concurrent updates.
	// ProposalValidator is all-replica because HTTPStore depends on it for HTTP content validation.
	registerLifecycleComponents(setup.Registry,
		[]lifecycle.Component{
			reconcilerComponent,
			warmerComponent,
			discoveryComponent,
			httpStoreComponent,
			proposalValidatorComponent,
			statusApplierComponent,
			resourceApplierComponent,
			eventEmitterComponent,
		},
		// DrainLeaderOnly follows this order: Scheduler, then Deployer, then ConfigPublisher.
		[]lifecycle.Component{
			coordinatorComponent,
			renderGateComponent,
			deployStack.DriftMonitor,
			deployStack.Scheduler,
			deployStack.Deployer,
			configPublisherComponent,
			statusUpdaterComponent,
		},
	)

	return &reconciliationWiring{
		renderService:         renderService,
		publishedCurrentFiles: currentFiles.published,
		inputIsolation:        inputSelector,
		gvrMapper:             gvrMapper,
		warmer:                warmerComponent,
		admissionChecks:       observeChecks(setup.MetricsComponent.Metrics(), "admission"),
	}, nil
}

// registerLifecycleComponents registers the all-replica components first, then
// the leader-only ones (started later, after leadership is acquired).
func registerLifecycleComponents(reg *lifecycle.Registry, allReplica, leaderOnly []lifecycle.Component) {
	for _, c := range allReplica {
		reg.Register(c, false)
	}
	for _, c := range leaderOnly {
		reg.Register(c, true)
	}
}

// checkVerdictCapacity bounds the verdicts kept for reuse. A render is checked
// by input acceptance and then by the render gate; input isolation can add a
// handful of trial renders in between.
const checkVerdictCapacity = 32

// checkServices are the `haproxy -c -dr` services of one iteration. They share
// one verdict store: the render gate re-checks exactly the bytes input
// acceptance has just checked with the same flags, so it reuses that verdict.
type checkServices struct {
	input       *validation.ValidationService
	httpContent *validation.ValidationService
	renderGate  *validation.ValidationService
	retained    *validation.ValidationService
}

func newCheckServices(cfg *coreconfig.Config, logger *slog.Logger, m *metrics.Metrics) *checkServices {
	verdicts := dataplane.NewCheckVerdicts(checkVerdictCapacity)
	return &checkServices{
		input:       newFullValidator(cfg, logger, verdicts, observeChecks(m, "input")),
		httpContent: newFullValidator(cfg, logger, verdicts, observeChecks(m, "http_content")),
		renderGate:  newRenderGateValidator(cfg, logger, verdicts, observeChecks(m, "render_gate")),
		retained:    newRenderGateValidator(cfg, logger, verdicts, observeChecks(m, "retained")),
	}
}

func observeChecks(m *metrics.Metrics, validator string) validation.CheckObserver {
	if m == nil {
		return nil
	}
	return func(result string, reused bool) { m.RecordHAProxyCheck(validator, result, reused) }
}

// newRenderGateValidator builds the render gate's validation service: the
// `haproxy -c -dr` run, on a gate of its own.
//
// `-dr` matches the shipped pod and today's reconcile path — a DNS blip must
// never revert a fleet. The gate's own CheckGate keeps the webhook's 9 s
// failurePolicy: Fail budget clear of it, and its duty-cycle interval bounds
// the CPU a render storm can take from admission.
func newRenderGateValidator(cfg *coreconfig.Config, logger *slog.Logger, verdicts *dataplane.CheckVerdicts, observer validation.CheckObserver) *validation.ValidationService {
	dirConfig := extractValidationDirConfig(&cfg.Dataplane)
	return validation.NewValidationService(&validation.ValidationServiceConfig{
		Logger:            logger.With("validation", "rendergate"),
		SkipDNSValidation: true,
		CheckGate:         dataplane.NewCheckGate(cfg.Controller.GetRenderGateInterval()),
		Verdicts:          verdicts,
		CheckObserver:     observer,
		BaseDir:           dirConfig.BaseDir,
		MapsDir:           dirConfig.MapsDir,
		SSLCertsDir:       dirConfig.SSLCertsDir,
		GeneralDir:        dirConfig.GeneralDir,
	})
}

// Independent gates keep input acceptance from queuing behind admission (ADR-0032).
func newFullValidator(
	cfg *coreconfig.Config,
	logger *slog.Logger,
	verdicts *dataplane.CheckVerdicts,
	observer validation.CheckObserver,
) *validation.ValidationService {
	dirConfig := extractValidationDirConfig(&cfg.Dataplane)
	return validation.NewValidationService(&validation.ValidationServiceConfig{
		Logger:            logger.With("validation", "full"),
		SkipDNSValidation: true,
		CheckGate:         dataplane.NewCheckGate(0),
		Verdicts:          verdicts,
		CheckObserver:     observer,
		BaseDir:           dirConfig.BaseDir,
		MapsDir:           dirConfig.MapsDir,
		SSLCertsDir:       dirConfig.SSLCertsDir,
		GeneralDir:        dirConfig.GeneralDir,
	})
}

// newResourceApplier builds the all-replica/leader-only ResourceApplier
// component. Extracted from createReconciliationComponents so the parent
// stays under the function-length lint cap; the inputs (CR identity for
// ownerRef, namespace for restriction default) and outputs are otherwise
// independent of the rest of the wiring.
//
// All-replica subscriber, leader-only applier — same shape as StatusApplier.
// Resource-agnostic: templates declare resources under spec.k8sResources and
// the applier reconciles whatever the renderer parsed out of them, with
// checksum dedup so unchanged resources don't hammer kube-api. Cross-
// namespace SSA is allowed at the controller boundary; the security gate is
// the chart's RBAC (a misbehaving template still gets Forbidden when the
// granted Role/ClusterRole doesn't cover the target namespace).
func newResourceApplier(crd *v1alpha1.HAProxyTemplateConfig, cfg *coreconfig.Config, k8sClient *client.Client, gvrMapper meta.RESTMapper, bus *busevents.EventBus, logger *slog.Logger) *resourceapplier.Component {
	ownNamespace := os.Getenv("POD_NAMESPACE")
	if ownNamespace == "" {
		ownNamespace = k8sClient.Namespace()
	}
	// OwnerReference identity from the live HAProxyTemplateConfig CR. The
	// applier injects this into every full-ownership SSA payload so
	// Kubernetes garbage collection cascade-deletes the rendered resources
	// when the CR is removed (e.g. `helm uninstall`).
	ownerRef := resourceapplier.OwnerReference{
		APIVersion: v1alpha1.SchemeGroupVersion.String(),
		Kind:       configKind,
		Name:       crd.GetName(),
		UID:        string(crd.GetUID()),
	}
	return resourceapplier.New(&resourceapplier.Config{
		EventBus:               bus,
		DynamicClient:          k8sClient.DynamicClient(),
		DiscoveryClient:        k8sClient.Clientset().Discovery(),
		GVRResolver:            statusapplier.NewRestMapperResolver(gvrMapper),
		Logger:                 logger,
		OwnNamespace:           ownNamespace,
		RestrictToOwnNamespace: false,
		OwnerRef:               ownerRef,
		// Identical cycles skip the SSA pass for one drift interval; the
		// expiry re-apply is the periodic authoritative live-state check.
		ReapplyInterval: cfg.Dataplane.GetDriftPreventionInterval(),
	})
}

// createConfigPublisher creates a config publisher with informer-backed listers for cached reads.
// This significantly reduces API calls by checking cached state before doing status updates.
// republishInterval bounds how long an unchanged republish is skipped; the
// expiry republish is the periodic authoritative self-heal, matching the
// deployment drift cadence.
func createConfigPublisher(ctx context.Context, crdClientset versioned.Interface, k8sClient *client.Client, republishInterval time.Duration, logger *slog.Logger) (*configpublisher.Publisher, func(), error) {
	// Create shared informer factory for HAProxy CRDs
	// The informers provide cached reads for status updates, significantly reducing API calls.
	// We use a 30-second resync period to keep the cache reasonably fresh while minimizing overhead.
	informerFactory := informers.NewSharedInformerFactoryWithOptions(
		crdClientset,
		timeouts.InformerResyncPeriod,
		informers.WithNamespace(k8sClient.Namespace()),
	)

	// Initialize informers by calling Lister() - this creates the underlying SharedIndexInformer
	// The informers won't start watching until Start() is called below
	haproxyInformers := informerFactory.HaproxyTemplateIC().V1alpha1()
	listers := &configpublisher.Listers{
		MapFiles:     haproxyInformers.HAProxyMapFiles().Lister(),
		GeneralFiles: haproxyInformers.HAProxyGeneralFiles().Lister(),
		CRTListFiles: haproxyInformers.HAProxyCRTListFiles().Lister(),
		HAProxyCfgs:  haproxyInformers.HAProxyCfgs().Lister(),
	}

	informerCtx, cancelInformers := context.WithCancel(ctx)
	stopInformers := sync.OnceFunc(func() {
		cancelInformers()
		informerFactory.Shutdown()
	})
	informerFactory.StartWithContext(informerCtx)

	// Wait for cache to sync before creating publisher
	// This ensures listers have initial data before first use
	logger.Debug("Waiting for HAProxy CRD informer caches to sync")
	syncResult := informerFactory.WaitForCacheSyncWithContext(informerCtx)
	for informerType, synced := range syncResult.Synced {
		if !synced {
			stopInformers()
			return nil, nil, fmt.Errorf("informer cache sync failed for %v: %w", informerType, syncResult.Err)
		}
	}
	logger.Debug("HAProxy CRD informer caches synced")

	// Create publisher with listers for cached reads
	publisher := configpublisher.NewWithListers(k8sClient.Clientset(), crdClientset, listers, logger)
	publisher.SetRepublishInterval(republishInterval)
	// Clients built from fake clientsets have no REST config.
	if restConfig := k8sClient.RestConfig(); restConfig != nil {
		metadataClient, err := metadata.NewForConfig(restConfig)
		if err != nil {
			stopInformers()
			return nil, nil, fmt.Errorf("creating metadata client: %w", err)
		}
		publisher.SetMetadataClient(metadataClient)
	}
	return publisher, stopInformers, nil
}
