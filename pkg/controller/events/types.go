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

// Package events contains all domain event type definitions for the HAPTIC controller.
//
// # Event Immutability Contract
//
// Events in this system are intended to be immutable after creation. They represent
// historical facts about what happened in the system and should not be modified after
// being published to the EventBus.
//
// To support this immutability contract:
//
//  1. All event types use pointer receivers for their Event interface methods.
//     This avoids copying large structs (200+ bytes) and follows Go best practices.
//
//  2. All event fields are exported to support JSON serialization and idiomatic Go access.
//     This follows industry standards (Kubernetes, NATS) rather than enforcing immutability
//     through unexported fields and getters.
//
//  3. Constructors perform defensive copying of slices and maps to prevent mutations
//     from affecting the published event. Publishers cannot modify events after creation.
//
//  4. Consumers MUST NOT modify event fields. This immutability contract is enforced through:
//     - A custom static analyzer (tools/linters/eventimmutability) that detects parameter mutations
//     - Code review for cases not caught by the analyzer
//     - Team discipline and documentation
//
// This approach balances performance, Go idioms, and practical immutability for an
// internal project where all consumers are controlled.
//
// # Event Categories
//
// Events are organized into separate files by category:
//
//   - config.go:              HAProxyTemplateConfig CRD changes and validation events
//   - resource.go:            Kubernetes resource indexing and synchronization events
//   - reconciliation.go:      Template rendering and deployment cycle events
//   - template.go:            Template rendering operation events
//   - validation.go:          Configuration validation (syntax and semantics) events
//   - rendergate.go:          Asynchronous `haproxy -c` verdict on a render
//   - deployment.go:          HAProxy configuration deployment events
//   - discovery.go:           HAProxy pod discovery events
//   - credentials.go:         Credentials loading and validation events
//   - leader.go:              Leader election events
//   - publishing.go:          Config publishing events (including SyncMetadata types)
//   - certificate.go:         Webhook certificate events
//   - http.go:                HTTP resource events
//   - proposal.go:            Speculative validation requests/responses (used by webhook and HTTP store)
//   - status.go:              Status patch application events
package events

const (
	// Configuration event types.
	EventTypeConfigParsed                 = "config.parsed"
	EventTypeConfigValidationRequest      = "config.validation.request"
	EventTypeConfigValidationResponse     = "config.validation.response"
	EventTypeConfigValidated              = "config.validated"
	EventTypeConfigInvalid                = "config.invalid"
	EventTypeConfigValidationInconclusive = "config.validation.inconclusive"
	EventTypeConfigVerdictReplayed        = "config.verdict.replayed"
	EventTypeConfigResourceChanged        = "config.resource.changed"
	EventTypeLibrarySetChanged            = "snippets.set.changed"

	// Resource event types.
	EventTypeWatchedInputsRejected = "resource.input.rejected"
	EventTypeResourceIndexUpdated  = "resource.index.updated"
	EventTypeResourceSyncComplete  = "resource.sync.complete"
	EventTypeIndexSynchronized     = "index.synchronized"

	// Reconciliation event types.
	EventTypeReconciliationTriggered = "reconciliation.triggered"
	EventTypeReconciliationStarted   = "reconciliation.started"
	EventTypeReconciliationCompleted = "reconciliation.completed"

	// EventTypeResourcesApplied is published by the ResourceApplier after a
	// cycle's rendered resources are applied; carries the cycle's status
	// patches forward so the rendered status variant applies after the
	// resources exist.
	EventTypeResourcesApplied     = "resources.applied"
	EventTypeResourcesProcessed   = "resources.processed"
	EventTypeReconciliationFailed = "reconciliation.failed"

	// Template event types.
	EventTypeTemplateRendered     = "template.rendered"
	EventTypeTemplateRenderFailed = "template.render.failed"

	// Validation event types (the render gate's `haproxy -c`).
	EventTypeValidationFailed = "validation.failed"

	// Render gate verdict: the controller's own `haproxy -c` on a render,
	// which runs off the reconcile wall clock (ADR-0022).
	EventTypeRenderGateCompleted = "rendergate.completed"

	// Deployment event types.
	EventTypeDeploymentScheduled      = "deployment.scheduled"
	EventTypeDeploymentStarted        = "deployment.started"
	EventTypeInstanceDeploymentFailed = "instance.deployment.failed"
	EventTypeDeploymentCompleted      = "deployment.completed"
	EventTypeDeploymentSkipped        = "deployment.skipped"
	EventTypeDeploymentCancelRequest  = "deployment.cancel.request"
	EventTypeDriftPreventionTriggered = "drift.prevention.triggered"

	// DeploymentSkippedEvent reasons: no deployment ran, and the data plane
	// serves the config anyway.
	SkipReasonConfigUnchanged = "config_unchanged" // the render equals the last deployed one
	SkipReasonReloadObserved  = "reload_observed"  // a later deployment's ACKs report the fleet running it

	// HAProxy pod event types.
	EventTypeHAProxyPodsDiscovered = "haproxy.pods.discovered"
	EventTypeHAProxyPodTerminated  = "haproxy.pod.terminated"
	EventTypeHAProxyPodRejected    = "haproxy.pod.rejected"

	// Config publishing event types.
	EventTypeConfigPublished              = "config.published"
	EventTypeConfigAppliedToPod           = "config.applied.to.pod"
	EventTypeDeployedConfigPublishRequest = "config.deployed.publish.request"

	// Credentials event types.
	EventTypeSecretResourceChanged = "secret.resource.changed"
	EventTypeCredentialsUpdated    = "credentials.updated"

	// Leader election event types.
	EventTypeLeaderElectionStarted = "leader.election.started"
	EventTypeBecameLeader          = "leader.became"
	EventTypeLostLeadership        = "leader.lost"
	EventTypeNewLeaderObserved     = "leader.observed"

	// HTTP resource event types.
	EventTypeHTTPResourceUpdated  = "http.resource.updated"
	EventTypeHTTPResourceAccepted = "http.resource.accepted"

	EventTypeHTTPContentAcceptanceRequested = "http.content.acceptance.requested"
	EventTypeHTTPContentRevoked             = "http.content.revoked"
	EventTypeHTTPContentRejected            = "http.content.rejected"

	// Proposal validation event types.
	// Used for validating hypothetical configuration changes before committing them.
	// See proposal.go for event definitions.
	EventTypeProposalValidationRequested = "proposal.validation.requested"
	EventTypeProposalValidationCompleted = "proposal.validation.completed"

	// Status update event types.
	// Published by StatusApplier after applying template-driven status patches to Kubernetes resources.
	EventTypeStatusUpdateCompleted = "status.update.completed"
	EventTypeStatusUpdateFailed    = "status.update.failed"
)

// TriggerReason constants for reconciliation events.
// These are propagated through the event chain via TriggerReason fields.
const (
	TriggerReasonDriftPrevention = "drift_prevention"
)
