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

// Package discovery provides the Discovery event adapter component.
//
// It tracks the set of HAProxy pods reported by the resource watcher (via the
// auto-injected haproxy-pods watcher), admits the ones whose agent answers
// /v1/state — carrying the HAProxy version and blob support it reports — and publishes
// HAProxyPodsDiscoveredEvent / HAProxyPodTerminatedEvent so the deployer and
// other consumers know which endpoints to talk to.
package discovery

import (
	"context"
	"log/slog"
	"slices"
	"sync"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/component"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/leadership"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/names"
	coreconfig "github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/types"
	"github.com/haproxy-haptic/HAPTIC/pkg/transportsecurity"
)

const (
	// ComponentName is the unique identifier for this component.
	ComponentName = "discovery"

	// EventBufferSize is the buffer size for event subscriptions.
	// High-volume to absorb pod churn bursts during scaling and rolling updates.
	EventBufferSize = busevents.HighVolumeSubscriberBuffer
)

// discoveryWantsEvent drops the resource-index updates for kinds this component
// does not watch, before they reach its buffer.
//
// The watcher publishes ResourceIndexUpdatedEvent for every watched kind, and
// this component acts on exactly one: the auto-injected haproxy-pods self-watch.
// Discarding the rest in the handler is too late — they occupied the buffer to
// get there. Under e2e churn that buffer filled with events destined for the
// first line of handleResourceIndexUpdated, one was dropped, and because this
// subscriber is non-lossy the whole controller iteration restarted and the
// fleet lost its routing until it came back.
func discoveryWantsEvent(event busevents.Event) bool {
	indexUpdate, ok := event.(*events.ResourceIndexUpdatedEvent)
	if !ok {
		return true
	}
	return indexUpdate.ResourceTypeName == names.HAProxyPodsResourceType
}

type endpointIdentity struct {
	podNamespace string
	podName      string
	podUID       string
	podRuntimeID string
	url          string
}

type podIdentity struct {
	podNamespace string
	podName      string
}

type endpointAuthority struct {
	identity             endpointIdentity
	username             string
	password             string
	detectedMajorVersion int
	detectedMinorVersion int
	detectedFullVersion  string
	acceptsBlobs         bool
}

// agentReport is what admission keeps of a pod's /v1/state.
type agentReport struct {
	version      string
	acceptsBlobs bool
}

func reportOf(state *api.State) agentReport {
	return agentReport{
		version:      state.HAProxy.Version,
		acceptsBlobs: slices.Contains(state.Features, api.FeatureBlobs),
	}
}

// applyAgentReport records what the pod's agent reported. The controller
// derives the fleet's template capabilities and each pod's runtime
// capabilities from it.
func applyAgentReport(endpoint *dataplane.Endpoint, report agentReport) {
	endpoint.AcceptsBlobs = report.acceptsBlobs
	endpoint.DetectedFullVersion = report.version
	parsed, err := dataplane.ParseVersionString(report.version)
	if err != nil {
		return
	}
	endpoint.DetectedMajorVersion = parsed.Major
	endpoint.DetectedMinorVersion = parsed.Minor
}

func endpointAuthorityOf(endpoint *dataplane.Endpoint) endpointAuthority {
	return endpointAuthority{
		identity:             endpointIdentityOf(endpoint),
		username:             endpoint.Username,
		password:             endpoint.Password,
		detectedMajorVersion: endpoint.DetectedMajorVersion,
		detectedMinorVersion: endpoint.DetectedMinorVersion,
		detectedFullVersion:  endpoint.DetectedFullVersion,
		acceptsBlobs:         endpoint.AcceptsBlobs,
	}
}

func endpointIdentityOf(endpoint *dataplane.Endpoint) endpointIdentity {
	return endpointIdentity{
		podNamespace: endpoint.PodNamespace,
		podName:      endpoint.PodName,
		podUID:       endpoint.PodUID,
		podRuntimeID: endpoint.PodRuntimeID,
		url:          endpoint.URL,
	}
}

// Component is the Discovery event adapter.
//
// This component:
//   - Subscribes to ConfigValidatedEvent, CredentialsUpdatedEvent, ResourceIndexUpdatedEvent, and BecameLeaderEvent
//   - Maintains current state (dataplanePort, credentials, podStore)
//   - Calls Discovery.DiscoverEndpoints() when relevant events occur
//   - Publishes HAProxyPodsDiscoveredEvent with discovered endpoints
//   - Publishes HAProxyPodTerminatedEvent when pods are removed
//
// Event Flow:
//  1. ConfigValidatedEvent → Update dataplanePort → Trigger discovery
//  2. CredentialsUpdatedEvent → Update credentials → Trigger discovery
//  3. ResourceIndexUpdatedEvent (haproxy-pods) → Trigger discovery
//  4. BecameLeaderEvent → Re-trigger discovery for new leader's DeploymentScheduler
//  5. Discovery completes → Compare with previous endpoints → Publish HAProxyPodTerminatedEvent for removed pods → Publish HAProxyPodsDiscoveredEvent
type Component struct {
	*component.Base

	discovery *Discovery
	tls       *transportsecurity.Source
	// discoveryMu orders endpoint-authority updates with complete discovery
	// publications. SetPodStore runs outside Base's serial event loop, so a
	// store swap must not land mid-pass.
	discoveryMu sync.Mutex

	// State replay for leadership transitions
	discoveredReplayer *leadership.StateReplayer[*events.HAProxyPodsDiscoveredEvent]

	// State protected by mutex
	mu                   sync.RWMutex
	dataplanePort        int
	credentials          *coreconfig.Credentials
	podStore             types.Store
	lastEndpoints        map[podIdentity]endpointAuthority
	hasCredentials       bool
	hasDataplanePort     bool
	initialSyncComplete  bool // Set when ResourceSyncCompleteEvent for haproxy-pods is received
	initialDiscoveryDone bool // Set after the first discovery is performed
	lifecycleCtx         context.Context

	// admitted maps an exact pod identity — namespace, name, UID, container
	// fingerprint and URL — to what its agent reported. A restart or a new
	// address is a different identity and is probed again.
	admitted map[endpointIdentity]agentReport
}

// New creates a new Discovery event adapter component.
//
// Parameters:
//   - eventBus: The event bus for subscribing to and publishing events
//   - logger: Structured logger for observability
//
// Returns a configured Component ready to be started.
//
// Note: The Discovery pure component is created lazily when the dataplane port
// is configured via ConfigValidatedEvent.
func New(eventBus *busevents.EventBus, logger *slog.Logger, options ...Option) *Component {
	c := &Component{
		discoveredReplayer: leadership.NewStateReplayer[*events.HAProxyPodsDiscoveredEvent](eventBus),
		lastEndpoints:      make(map[podIdentity]endpointAuthority),
		admitted:           make(map[endpointIdentity]agentReport),
	}
	// The Base subscribes to the EventBus during construction (before
	// EventBus.Start()). This ensures proper startup synchronization without
	// timing-based sleeps. Typed subscription (EventTypes, not a catch-all)
	// so we only receive events we handle (reduces buffer pressure).
	c.Base = component.New(&component.Config{
		EventBus:   eventBus,
		Logger:     logger,
		Name:       ComponentName,
		BufferSize: EventBufferSize,
		Handler:    c,
		EventTypes: []string{
			events.EventTypeConfigValidated,
			events.EventTypeCredentialsUpdated,
			events.EventTypeResourceIndexUpdated,
			events.EventTypeResourceSyncComplete,
			events.EventTypeBecameLeader,
			events.EventTypeDriftPreventionTriggered,
		},
		EventFilter: discoveryWantsEvent,
	})

	for _, option := range options {
		option(c)
	}
	return c
}

// Option configures discovery's operational connections before startup.
type Option func(*Component)

// WithTLS makes discovery use authenticated HTTPS without a plaintext fallback.
func WithTLS(source *transportsecurity.Source) Option {
	return func(c *Component) { c.tls = source }
}

// Start runs the embedded component.Base event loop until the context is
// cancelled.
//
// Note: Event subscription occurs in the constructor (New()) to ensure proper
// startup synchronization. ResourceSyncCompleteEvent is buffered until EventBus.Start()
// is called, so no events are missed.
func (c *Component) Start(ctx context.Context) error {
	c.mu.Lock()
	c.lifecycleCtx = ctx
	c.mu.Unlock()

	return c.Base.Start(ctx)
}

// HandleEvent implements component.EventHandler: it processes incoming
// events and triggers discovery as needed.
func (c *Component) HandleEvent(event busevents.Event) {
	switch e := event.(type) {
	case *events.ConfigValidatedEvent:
		c.handleConfigValidated(e)

	case *events.CredentialsUpdatedEvent:
		c.handleCredentialsUpdated(e)

	case *events.ResourceIndexUpdatedEvent:
		c.handleResourceIndexUpdated(e)

	case *events.ResourceSyncCompleteEvent:
		c.handleResourceSyncComplete(e)

	case *events.BecameLeaderEvent:
		c.handleBecameLeader(e)

	case *events.DriftPreventionTriggeredEvent:
		c.handleDriftPrevention(e)
	}
}
