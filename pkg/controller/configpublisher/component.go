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

package configpublisher

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"

	"github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/component"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/throttle"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderartifact"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderoutput"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/configpublisher"
)

const (
	// ComponentName is the unique identifier for this component.
	ComponentName = "config-publisher"

	// EventBufferSize is the buffer size for the event subscription channel.
	// Large buffer to handle burst traffic during startup: ConfigPublisher makes
	// synchronous k8s API calls, so it processes events slowly compared to the
	// rate at which all-replica components publish them.
	EventBufferSize = busevents.PublishingSubscriberBuffer

	// publishWorkChannelSize is the buffer size for the publish work channel.
	// A size of 1 provides natural coalescing - if new work arrives while
	// previous work is being processed, the old pending work is replaced.
	publishWorkChannelSize = 1

	// statusWorkTriggerSize is the buffer size for the status work trigger channel.
	// A size of 1 is sufficient since we use a separate map for coalescing.
	// The trigger just wakes up the worker to process pending updates.
	statusWorkTriggerSize = 1
)

// renderedConfigEntry holds one render's bytes on their way to the publish
// worker, keyed by the correlation ID of the reconcile that produced them.
type renderedConfigEntry struct {
	confirmedPod     *v1alpha1.PodDeploymentStatus
	config           string
	auxFiles         *dataplane.AuxiliaryFiles
	artifactSnapshot *renderartifact.Snapshot
	outputSnapshot   *renderoutput.Snapshot
	contentChecksum  string
	planID           string
	renderedAt       time.Time
}

// publishWorkItem represents a config publish task for the async worker.
type publishWorkItem struct {
	correlationID string
	config        publishConfigIdentity
	entry         *renderedConfigEntry
	request       *configpublisher.PublishRequest
	generation    uint64
	term          uint64
	superseded    <-chan struct{}
	// deployDriven marks an item that carries the bytes the deployer just
	// applied (from a DeployedConfigPublishRequest), as opposed to the
	// validation-driven publish. Deploy-driven items use their own pending slot
	// so a validation publish can't coalesce them away, keeping every deployed
	// checksum observable as a published spec.Checksum.
	deployDriven bool
}

// validationFailedWorkItem represents a failed config publish task for the async worker.
type validationFailedWorkItem struct {
	correlationID   string
	config          publishConfigIdentity
	entry           *renderedConfigEntry
	request         *configpublisher.PublishRequest
	validationError string
	generation      uint64
	term            uint64
	superseded      <-chan struct{}
}

// statusWorkItem represents a pod status update task for the async worker.
type statusWorkItem struct {
	event *events.ConfigAppliedToPodEvent
}

type podAuthorityKey struct {
	namespace string
	name      string
}

type podAuthority struct {
	uid       string
	runtimeID string
}

// Component is the event adapter for the config publisher.
// It wraps the pure Publisher component and coordinates it with the event bus.
//
// It publishes what the fleet was given: TemplateRenderedEvent is the trigger,
// and the render gate's later verdict lands on the same object as the
// ConfigValidated / ConfigPinned conditions (ADR-0022).
//
// Rendered configs are cached by correlation ID because the publish worker is
// asynchronous and consumes the entry; a single-slot copy of the newest render
// outlives that eviction so a validation failure for the same reconcile can
// still publish those bytes as the invalid variant.
//
// The component uses async workers for K8S API operations to prevent blocking
// the event loop. This ensures new events are processed promptly even when
// K8S API calls are slow.
type Component struct {
	*component.ReadySignal

	publisher *configpublisher.Publisher
	eventBus  *busevents.EventBus
	logger    *slog.Logger

	// Subscribed in Start() when leadership is acquired
	eventChan <-chan busevents.Event

	// Cached state from events (protected by mutex)
	mu                sync.RWMutex
	templateConfig    *v1alpha1.HAProxyTemplateConfig
	hasTemplateConfig bool

	// renderedConfigs maps correlation ID to rendered config data. It is the
	// hand-off to the async publish worker, which consumes and evicts each
	// entry.
	renderedConfigs map[string]*renderedConfigEntry

	// lastRender is the newest render, kept past the worker's eviction so a
	// validation failure for the same reconcile can publish those bytes as the
	// invalid variant. One slot: only the newest render can still be the
	// subject of a verdict.
	lastRender              *renderedConfigEntry
	lastRenderCorrelationID string

	// gatePinned mirrors the render gate's latch. While it is set the fleet is
	// not being given new renders, so publishing one would advertise a config
	// no pod has — the object is the fleet's, not the renderer's.
	gatePinned              bool
	publicationBlocked      bool
	activePublicationCancel context.CancelFunc
	// heldRender is the render withheld while pinned, published by the verdict
	// that releases it.
	heldRender        *renderedConfigEntry
	heldCorrelationID string
	// publishedPlanID is the plan of the render last queued for publishing,
	// which is what a verdict has to name to be a statement about this object.
	publishedPlanID string

	// seedTemplateConfig is the iteration's config, restored on every term.
	seedTemplateConfig *v1alpha1.HAProxyTemplateConfig

	// pendingVerdict is the render gate's verdict awaiting its status write,
	// latest wins. The write is an apiserver round-trip and must not run on
	// the event loop.
	verdictMu      sync.Mutex
	pendingVerdict *configpublisher.GateVerdict
	verdictTrigger chan struct{}

	// Work channels for async K8S API operations.
	// Using channels with small buffers provides natural coalescing:
	// newer work replaces older pending work when the worker is busy.
	publishWork          chan *publishWorkItem
	validationFailedWork chan *validationFailedWorkItem

	// Status update coalescing.
	// Instead of queueing every status update individually, we coalesce updates
	// for the same pod. When multiple updates arrive for the same pod before the
	// worker processes them, only the latest update is applied. This prevents
	// channel overflow during high-frequency reconciliation cycles.
	statusWorkPending map[string]*statusWorkItem // Key: namespace/runtimeConfig/podName
	statusWorkMu      sync.Mutex
	statusWorkTrigger chan struct{} // Signals worker to process pending updates
	// statusWorkParked holds the updates the HAProxyCfg could not take yet until
	// the next publication attempt (statusPublicationAttempts counts them). No
	// key is both parked and pending. statusWorkMu guards both maps and the count.
	statusWorkParked          map[string]*statusWorkItem
	statusPublicationAttempts uint64

	endpointAuthorityMu    sync.RWMutex
	endpointAuthorities    map[podAuthorityKey]podAuthority
	endpointAuthoritiesSet bool

	// deployedPending is every deployed render still awaiting publication, in
	// arrival order and deduplicated by content checksum.
	//
	// A size-1 channel with latest-wins coalescing was wrong here, unlike for
	// the validation path: `status.deployedToPods[].checksum` is written by an
	// independent path, so a dropped deployed checksum leaves the CR
	// advertising a config that `spec.content` never carried — a checksum no
	// reader, and no watcher, can resolve. Measured on a real run: 1 checksum
	// in 31 dropped.
	//
	// What a reader can observe is the bound: a checksum only needs to reach
	// `spec` while some pod is still reported at it. pruneSupersededDeployed
	// therefore drops a queued entry once the fleet has demonstrably moved
	// past it, which caps the queue at the number of distinct checksums live
	// across the fleet rather than at the deploy rate. Each entry holds a full
	// rendered config, and the drain is one entry per configPublishInterval,
	// so without that prune the runtime-raw lane — which skips
	// minDeploymentInterval by design — grows this without bound under
	// endpoint churn. Protected by deployedPendingMu.
	deployedPending   []*publishWorkItem
	deployedPendingMu sync.Mutex

	// deployedChecksumByPod is the checksum each pod last reported running,
	// fed by ConfigAppliedToPodEvent — the same events that write
	// status.deployedToPods, so it mirrors exactly what a reader can see.
	// Protected by deployedPendingMu, which also guards the queue it prunes.
	deployedChecksumByPod map[podAuthorityKey]string
	deployedTrigger       chan struct{} // Wakes publishWorker; cap 1, same as statusWorkTrigger

	// lastPublishedChecksum tracks legacy publications. Authenticated outputs
	// use exact root identity instead of treating a digest as positive proof.
	// Protected by mu.
	lastPublishedChecksum       string
	lastPublishedOutputSnapshot *renderoutput.Snapshot
	lastPublishedEntry          *renderedConfigEntry
	publicationTerm             uint64
	nextPublishGeneration       uint64
	latestPublishGeneration     uint64
	nextInvalidGeneration       uint64
	latestInvalidGeneration     uint64
	publishSuperseded           chan struct{}
	invalidSuperseded           chan struct{}
	publicationRetryWait        func(context.Context, time.Duration, <-chan struct{}) bool
	publicationCallMu           sync.Mutex

	// publishInterval configures the leading-edge refractory period for both
	// publish and status throttles. Decouples CRD writes from reconciliation
	// frequency to reduce etcd write pressure; deployments to HAProxy pods
	// (event-driven) are unaffected.
	publishInterval time.Duration

	// publishThrottle gates spec writes; statusThrottle gates status
	// subresource writes. Each UpdateStatus writes the full ~509 KB object to
	// etcd even though only the status changed, so throttling them at the
	// same cadence as spec publishes is essential for etcd write pressure.
	publishThrottle *throttle.LeadingEdge
	statusThrottle  *throttle.LeadingEdge

	// pendingPublish buffers the latest publish work item that arrived while
	// inside the publish-throttle refractory window. The publish worker
	// flushes it on publishThrottle.FiredCh(). Protected by pendingMu.
	pendingPublish *publishWorkItem
	pendingMu      sync.Mutex

	drainRequests chan drainRequest
	drainWork     chan drainRequest
}

type drainRequest struct {
	ctx  context.Context
	done chan error
}

// Option configures the Component.
type Option func(*Component)

// WithPublishInterval sets the throttle interval for CRD publishes.
// During endpoint churn each reconciliation produces a new config, but writing
// ~500 KB to etcd every 5 s is excessive. This interval limits CRD updates while
// deployments to HAProxy pods (event-driven) remain unaffected.
// A value of 0 disables throttling (every config is published immediately).
func WithPublishInterval(d time.Duration) Option {
	return func(c *Component) {
		c.publishInterval = d
	}
}

// WithTemplateConfig seeds the HAProxyTemplateConfig the publications belong
// to. The iteration is built for exactly one, so the first render of a term
// never has to wait for the ConfigValidatedEvent that re-announces it.
func WithTemplateConfig(templateConfig *v1alpha1.HAProxyTemplateConfig) Option {
	return func(c *Component) {
		c.seedTemplateConfig = templateConfig
	}
}

// New creates a new config publisher component.
func New(
	publisher *configpublisher.Publisher,
	eventBus *busevents.EventBus,
	logger *slog.Logger,
	opts ...Option,
) *Component {
	if logger == nil {
		logger = slog.Default()
	}

	// Note: eventChan is NOT subscribed here - subscription happens in Start().
	// This is a leader-only component that subscribes when Start() is called
	// (after leadership is acquired). All-replica components replay their state
	// on BecameLeaderEvent to ensure leader-only components receive current state.
	c := &Component{
		ReadySignal:          component.NewReadySignal(),
		publisher:            publisher,
		eventBus:             eventBus,
		logger:               logger.With("component", ComponentName),
		renderedConfigs:      make(map[string]*renderedConfigEntry),
		publishWork:          make(chan *publishWorkItem, publishWorkChannelSize),
		deployedTrigger:      make(chan struct{}, statusWorkTriggerSize),
		validationFailedWork: make(chan *validationFailedWorkItem, publishWorkChannelSize),
		statusWorkPending:    make(map[string]*statusWorkItem),
		statusWorkParked:     make(map[string]*statusWorkItem),
		statusWorkTrigger:    make(chan struct{}, statusWorkTriggerSize),
		publishSuperseded:    make(chan struct{}),
		invalidSuperseded:    make(chan struct{}),
		publicationRetryWait: waitForPublicationRetry,
		endpointAuthorities:  make(map[podAuthorityKey]podAuthority),
		drainRequests:        make(chan drainRequest),
		drainWork:            make(chan drainRequest, 1),

		deployedChecksumByPod: make(map[podAuthorityKey]string),
	}

	for _, opt := range opts {
		opt(c)
	}
	c.templateConfig = c.seedTemplateConfig
	c.hasTemplateConfig = c.seedTemplateConfig != nil

	// Throttles are constructed after options so publishInterval is set.
	// A zero interval disables them (Available() always returns true).
	c.publishThrottle = throttle.New(c.publishInterval)
	c.statusThrottle = throttle.New(c.publishInterval)

	return c
}

// Name returns the unique identifier for this component.
// Implements the lifecycle.Component interface.
func (c *Component) Name() string {
	return ComponentName
}

// Start begins the config publisher's event loop.
//
// This method blocks until the context is cancelled or an error occurs.
// It subscribes to events when called (after leadership is acquired).
//
// Parameters:
//   - ctx: Context for cancellation and lifecycle management
//
// Returns:
//   - nil when context is cancelled (graceful shutdown)
//   - Error only in exceptional circumstances
func (c *Component) Start(ctx context.Context) error {
	c.preparePublicationTerm()
	defer c.Rearm()
	c.endpointAuthorityMu.Lock()
	c.endpointAuthorities = make(map[podAuthorityKey]podAuthority)
	c.endpointAuthoritiesSet = false
	c.endpointAuthorityMu.Unlock()

	// Subscribe when starting (after leadership acquired).
	// Use SubscribeTypesLeaderOnly() to suppress late subscription warning.
	// All-replica components replay their cached state on BecameLeaderEvent.
	c.eventChan = c.eventBus.SubscribeTypesLeaderOnly(ComponentName, EventBufferSize,
		events.EventTypeConfigValidated,
		events.EventTypeTemplateRendered,
		events.EventTypeReconciliationFailed,
		events.EventTypeRenderGateCompleted,
		events.EventTypeValidationFailed,
		events.EventTypeConfigAppliedToPod,
		events.EventTypeDeployedConfigPublishRequest,
		events.EventTypeHAProxyPodTerminated,
		events.EventTypeHAProxyPodsDiscovered,
		events.EventTypeLostLeadership,
	)
	// Unsubscribe on loop exit: without this, every leadership re-acquisition on
	// the same instance would stack another subscription whose orphaned channel
	// fills up and logs critical drops forever (mirrors Coordinator).
	defer c.eventBus.UnsubscribeTyped(c.eventChan)

	// Signal that subscription is complete for SubscriptionReadySignaler interface.
	c.MarkReady()

	c.logger.Debug("Config publisher starting")

	// Start async workers for K8S API operations.
	// These workers process work items from their channels, allowing the main
	// event loop to continue processing events without blocking on slow API calls.
	var workers sync.WaitGroup
	workers.Go(func() { c.publishWorker(ctx) })
	workers.Go(func() { c.validationFailedWorker(ctx) })
	workers.Go(func() { c.statusWorker(ctx) })
	workers.Go(func() { c.verdictWorker(ctx) })
	defer func() {
		c.publishThrottle.Stop()
		c.statusThrottle.Stop()
		workers.Wait()
	}()

	for {
		select {
		case event := <-c.eventChan:
			c.handleEvent(ctx, event)

		case request := <-c.drainRequests:
			c.acceptDrain(ctx, request)

		case <-ctx.Done():
			c.logger.Info("Config publisher shutting down", "reason", ctx.Err())
			return ctx.Err()
		}
	}
}

// Drain publishes the newest acknowledged checkpoint without waiting for the
// publish throttle, after handling every event delivered before the call.
// The publication runs under the term's context, so a lost Lease stops it.
func (c *Component) Drain(ctx context.Context) error {
	request := drainRequest{ctx: ctx, done: make(chan error, 1)}
	select {
	case c.drainRequests <- request:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-request.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Component) acceptDrain(ctx context.Context, request drainRequest) {
	// A new term starts only after the previous term's context ended.
	if err := request.ctx.Err(); err != nil {
		request.done <- err
		return
	}
	c.handleDeliveredEvents(ctx)
	select {
	case c.drainWork <- request:
	default:
		request.done <- errors.New("a checkpoint flush is already pending")
	}
}

func (c *Component) handleDeliveredEvents(ctx context.Context) {
	for {
		select {
		case event := <-c.eventChan:
			c.handleEvent(ctx, event)
		default:
			return
		}
	}
}

func (c *Component) preparePublicationTerm() {
	// A new leadership term must not trust status stamps a previous term (or
	// another replica) populated. Independent of the mutexes below — the
	// publisher guards the cache with its own lock.
	if c.publisher != nil {
		c.publisher.ResetStatusStamps()
	}

	c.mu.Lock()
	c.publicationTerm++
	c.publicationBlocked = false
	if c.activePublicationCancel != nil {
		c.activePublicationCancel()
		c.activePublicationCancel = nil
	}
	c.templateConfig = c.seedTemplateConfig
	c.hasTemplateConfig = c.seedTemplateConfig != nil
	c.renderedConfigs = make(map[string]*renderedConfigEntry)
	c.lastRender = nil
	c.lastRenderCorrelationID = ""
	c.gatePinned = false
	c.heldRender = nil
	c.heldCorrelationID = ""
	c.publishedPlanID = ""
	c.lastPublishedChecksum = ""
	c.lastPublishedOutputSnapshot = nil
	c.lastPublishedEntry = nil
	c.latestPublishGeneration = 0
	c.latestInvalidGeneration = 0
	c.publishSuperseded = supersedePublication(c.publishSuperseded)
	c.invalidSuperseded = supersedePublication(c.invalidSuperseded)
	c.mu.Unlock()

	c.publishWork = make(chan *publishWorkItem, publishWorkChannelSize)
	c.validationFailedWork = make(chan *validationFailedWorkItem, publishWorkChannelSize)
	c.drainWork = make(chan drainRequest, 1)

	c.deployedPendingMu.Lock()
	c.deployedPending = nil
	c.deployedTrigger = make(chan struct{}, statusWorkTriggerSize)
	c.deployedPendingMu.Unlock()

	c.pendingMu.Lock()
	c.pendingPublish = nil
	c.pendingMu.Unlock()

	c.statusWorkMu.Lock()
	c.statusWorkPending = make(map[string]*statusWorkItem)
	c.statusWorkParked = make(map[string]*statusWorkItem)
	c.statusWorkTrigger = make(chan struct{}, statusWorkTriggerSize)
	c.statusWorkMu.Unlock()

	c.verdictMu.Lock()
	c.pendingVerdict = nil
	c.verdictTrigger = make(chan struct{}, statusWorkTriggerSize)
	c.verdictMu.Unlock()

	c.publishThrottle = throttle.New(c.publishInterval)
	c.statusThrottle = throttle.New(c.publishInterval)
}

// handleEvent processes events from the event bus. ctx is the component's
// lifecycle context, forwarded to handlers that issue Kubernetes API calls so
// those calls are cancelled on shutdown.
func (c *Component) handleEvent(ctx context.Context, event busevents.Event) {
	switch e := event.(type) {
	case *events.ConfigValidatedEvent:
		c.handleConfigValidated(e)

	case *events.TemplateRenderedEvent:
		c.handleTemplateRendered(e)

	case *events.ReconciliationFailedEvent:
		c.mu.Lock()
		c.publicationBlocked = true
		if c.activePublicationCancel != nil {
			c.activePublicationCancel()
		}
		c.mu.Unlock()

	case *events.RenderGateCompletedEvent:
		c.handleRenderGateCompleted(e)

	case *events.ValidationFailedEvent:
		c.handleValidationFailed(e)

	case *events.ConfigAppliedToPodEvent:
		c.handleConfigAppliedToPod(e)

	case *events.DeployedConfigPublishRequest:
		c.handleDeployedConfigPublishRequest(e)

	case *events.HAProxyPodTerminatedEvent:
		c.handlePodTerminated(ctx, e)

	case *events.HAProxyPodsDiscoveredEvent:
		c.handlePodsDiscovered(ctx, e)

	case *events.LostLeadershipEvent:
		c.handleLostLeadership(e)
	}
}
