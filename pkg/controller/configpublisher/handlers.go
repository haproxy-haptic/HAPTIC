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
	"fmt"

	"github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/timeouts"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/configpublisher"
)

// lookupCachedConfig fetches the template config and the newest render when it
// is the one this event's correlation ID describes. Returns ok=false (with a
// warning logged) when the correlation ID is missing or the state isn't yet
// complete; callers should bail out in that case. eventName labels the event in
// the missing-correlation-id warning, action labels the intended operation in
// the missing-state warning.
func (c *Component) lookupCachedConfig(eventID, correlationID, eventName, action string) (*v1alpha1.HAProxyTemplateConfig, *renderedConfigEntry, bool) {
	if correlationID == "" {
		c.logger.Warn(eventName+" missing correlation ID, cannot match rendered config",
			"event_id", eventID)
		return nil, nil, false
	}

	c.mu.RLock()
	hasTemplateConfig := c.hasTemplateConfig
	templateConfig := c.templateConfig
	entry, hasRenderedConfig := c.lastRender, c.lastRenderCorrelationID == correlationID && c.lastRender != nil
	c.mu.RUnlock()

	if !hasTemplateConfig || !hasRenderedConfig {
		c.logger.Warn("Cannot "+action+", missing cached state",
			"has_template_config", hasTemplateConfig,
			"has_rendered_config", hasRenderedConfig,
			"correlation_id", correlationID,
		)
		return nil, nil, false
	}

	return templateConfig, entry, true
}

// handleConfigValidated caches the template config for later publishing.
func (c *Component) handleConfigValidated(event *events.ConfigValidatedEvent) {
	// Extract HAProxyTemplateConfig from event.TemplateConfig (not event.Config)
	// event.Config contains *config.Config (parsed config for validation)
	// event.TemplateConfig contains *v1alpha1.HAProxyTemplateConfig (original CRD for metadata)

	if event.TemplateConfig == nil {
		c.logger.Warn("Config validated event contains nil template config - this indicates a bug in event publishing",
			"version", event.Version)
		return
	}

	templateConfig, ok := event.TemplateConfig.(*v1alpha1.HAProxyTemplateConfig)
	if !ok {
		c.logger.Warn("Config validated event contains unexpected template config type - expected *v1alpha1.HAProxyTemplateConfig",
			"actual_type", fmt.Sprintf("%T", event.TemplateConfig),
			"version", event.Version)
		return
	}

	c.logger.Debug("Caching template config for publishing",
		"config_name", templateConfig.Name,
		"config_namespace", templateConfig.Namespace,
		"version", event.Version,
	)

	// Cache the template config
	c.mu.Lock()
	c.templateConfig = templateConfig
	c.hasTemplateConfig = true
	c.mu.Unlock()
}

// handleTemplateRendered caches the rendered config and queues it for
// publishing.
//
// The render is the trigger: HAProxy's verdict on it runs asynchronously in the
// render gate (ADR-0022), so the published HAProxyCfg is what the fleet was
// given, and the gate's verdict lands on it as a condition.
//
// This method is non-blocking: it queues work for the publishWorker instead of
// making K8S API calls directly, so the event loop keeps up with event volume.
func (c *Component) handleTemplateRendered(event *events.TemplateRenderedEvent) {
	entry, err := renderedConfigEntryFromEvent(event)
	if err != nil {
		c.logger.Error("Rendered configuration is invalid; publication was skipped",
			"error", err)
		return
	}

	correlationID := event.CorrelationID()
	if correlationID == "" {
		c.logger.Warn("TemplateRenderedEvent missing correlation ID, using event ID as fallback",
			"event_id", event.EventID(),
			"config_bytes", len(entry.config))
		correlationID = event.EventID()
	}

	// Cache the rendered config indexed by correlation ID
	c.mu.Lock()
	c.renderedConfigs[correlationID] = entry
	c.publicationBlocked = false
	c.lastRender = entry
	c.lastRenderCorrelationID = correlationID
	templateConfig := c.templateConfig
	hasTemplateConfig := c.hasTemplateConfig
	pinned := c.gatePinned
	if pinned {
		c.heldRender = entry
		c.heldCorrelationID = correlationID
	} else {
		c.publishedPlanID = entry.planID
	}
	c.mu.Unlock()

	if !hasTemplateConfig {
		c.logger.Warn("Cannot publish configuration, no template config cached yet",
			"correlation_id", correlationID)
		return
	}

	// The published object is what the fleet was given. While the gate holds
	// renders, the fleet is given nothing, so publishing this one would
	// advertise a config no pod has — it waits for the verdict that releases it.
	if pinned {
		c.logger.Warn("Render gate is holding renders; not publishing this one yet",
			"plan", entry.planID, "correlation_id", correlationID)
		return
	}

	c.logger.Debug("Queuing configuration for async publishing",
		"config_name", templateConfig.Name,
		"config_namespace", templateConfig.Namespace,
		"config_bytes", len(entry.config),
		"auxiliary_file_count", renderedAuxiliaryFileCount(entry),
		"correlation_id", correlationID,
	)

	// Queue work for async processing. Use non-blocking send with coalescing:
	// - If channel is empty, work is queued immediately
	// - If channel has pending work, replace it with newer work (coalescing)
	// This ensures we always publish the latest config, not stale intermediate ones.
	c.queuePublish(templateConfig, entry, correlationID)
	c.resumeAcknowledgedPublication()
}

func renderedConfigEntryFromEvent(event *events.TemplateRenderedEvent) (*renderedConfigEntry, error) {
	if event == nil {
		return nil, fmt.Errorf("template rendered event is nil")
	}
	occurrence, err := event.RenderOccurrence()
	if err != nil {
		return nil, fmt.Errorf("authenticating render occurrence: %w", err)
	}
	cycle, err := occurrence.Snapshot()
	if err != nil {
		return nil, fmt.Errorf("reading render occurrence: %w", err)
	}
	outputSnapshot, err := cycle.OutputSnapshot()
	if err != nil {
		return nil, fmt.Errorf("reading render cycle output: %w", err)
	}
	renderedConfig, err := outputSnapshot.Config()
	if err != nil {
		return nil, fmt.Errorf("reading output config: %w", err)
	}
	artifacts, err := outputSnapshot.ArtifactSnapshot()
	if err != nil {
		return nil, fmt.Errorf("reading output artifacts: %w", err)
	}
	planID, err := outputSnapshot.PlanID()
	if err != nil {
		return nil, fmt.Errorf("reading output plan ID: %w", err)
	}
	checksum, err := cycle.ContentChecksum()
	if err != nil {
		return nil, fmt.Errorf("reading render cycle checksum: %w", err)
	}
	return &renderedConfigEntry{
		config: renderedConfig, artifactSnapshot: artifacts, outputSnapshot: outputSnapshot,
		contentChecksum: checksum, planID: planID, renderedAt: event.Timestamp(),
	}, nil
}

func renderedAuxiliaryFileCount(entry *renderedConfigEntry) int {
	if entry.artifactSnapshot != nil {
		count, err := entry.artifactSnapshot.Len()
		if err == nil {
			return count
		}
	}
	if entry.auxFiles == nil {
		return 0
	}
	return len(entry.auxFiles.MapFiles) + len(entry.auxFiles.SSLCertificates) +
		len(entry.auxFiles.SSLCaFiles) + len(entry.auxFiles.GeneralFiles) +
		len(entry.auxFiles.CRTListFiles)
}

// handleRenderGateCompleted mirrors the gate's latch and records its verdict on
// the HAProxyCfg the fleet was given, so `kubectl describe haproxycfg` answers
// whether HAProxy would load what is deployed.
//
// Two things make a verdict inapplicable to this object: it may judge a
// superseded plan pods still run (the gate checks those for the revert), and
// it may judge a render this component withheld while pinned. Neither is a
// statement about what the object publishes.
func (c *Component) handleRenderGateCompleted(event *events.RenderGateCompletedEvent) {
	if !event.Newest {
		return
	}
	planID, err := renderGatePlanID(event)
	if err != nil {
		c.logger.Error("Render gate verdict is invalid; publication state was unchanged", "error", err)
		return
	}

	c.mu.Lock()
	templateConfig := c.templateConfig
	hasTemplateConfig := c.hasTemplateConfig
	c.gatePinned = !event.OK
	if !event.OK && c.activePublicationCancel != nil {
		c.activePublicationCancel()
	}
	released := c.releaseHeldRenderLocked(event.OK, planID)
	describesPublished := planID != "" && planID == c.publishedPlanID
	c.mu.Unlock()

	if !hasTemplateConfig || templateConfig == nil {
		return
	}
	if released != nil {
		c.queuePublish(templateConfig, released.entry, released.correlationID)
	}
	c.resumeAcknowledgedPublication()
	if !describesPublished {
		c.logger.Debug("Render gate verdict does not describe the published config",
			"plan", planID)
		return
	}

	c.enqueueVerdict(&configpublisher.GateVerdict{
		Namespace: templateConfig.Namespace,
		Name:      configpublisher.GenerateRuntimeConfigName(templateConfig.Name),
		PlanID:    planID,
		Accepted:  event.OK,
		Refused:   event.Refused,
		Pinned:    event.Pinned,
		Message:   event.Message,
	})
}

func renderGatePlanID(event *events.RenderGateCompletedEvent) (string, error) {
	occurrence, err := event.RenderOccurrence()
	if err != nil {
		return "", fmt.Errorf("reading render occurrence: %w", err)
	}
	cycle, err := occurrence.Snapshot()
	if err != nil {
		return "", fmt.Errorf("reading render cycle: %w", err)
	}
	output, err := cycle.OutputSnapshot()
	if err != nil {
		return "", fmt.Errorf("reading render output: %w", err)
	}
	planID, err := output.PlanID()
	if err != nil {
		return "", fmt.Errorf("reading output plan ID: %w", err)
	}
	return planID, nil
}

// heldRelease is the render a passing verdict let through.
type heldRelease struct {
	entry         *renderedConfigEntry
	correlationID string
}

// releaseHeldRenderLocked hands back the withheld render when this verdict is
// the pass that names it, and marks it as the plan the object now publishes.
// Caller holds mu.
func (c *Component) releaseHeldRenderLocked(ok bool, planID string) *heldRelease {
	if !ok || c.heldRender == nil || c.heldRender.planID != planID {
		return nil
	}
	released := &heldRelease{entry: c.heldRender, correlationID: c.heldCorrelationID}
	c.heldRender = nil
	c.heldCorrelationID = ""
	c.publishedPlanID = planID
	return released
}

// queuePublish hands a render to the publish worker.
func (c *Component) queuePublish(
	templateConfig *v1alpha1.HAProxyTemplateConfig, entry *renderedConfigEntry, correlationID string,
) {
	workItem := c.makePublishWorkItem(correlationID, templateConfig, entry, false)
	queueWithCoalesce(c, c.publishWork, workItem, "publish", correlationID,
		func(w *publishWorkItem) string { return w.correlationID })
}

// enqueueVerdict parks the verdict for the worker. Latest wins: a verdict is
// the complete current answer, and the newer one supersedes the older.
func (c *Component) enqueueVerdict(verdict *configpublisher.GateVerdict) {
	c.verdictMu.Lock()
	c.pendingVerdict = verdict
	trigger := c.verdictTrigger
	c.verdictMu.Unlock()

	select {
	case trigger <- struct{}{}:
	default: // already signalled; the worker reads the newest slot
	}
}

// handleDeployedConfigPublishRequest publishes, as the HAProxyCfg spec, the
// exact config the deployer just applied. This guarantees the deployed checksum
// — the same value stamped into status.deployedToPods[] — is observable as a
// published spec.Checksum even when the validation-driven publish for that
// render was throttled/coalesced away under churn. The bytes are carried on the
// event (inline entry), so no renderedConfigs cache lookup is needed, and it
// routes through a dedicated channel + pending slot so a validation publish
// cannot coalesce it away.
func (c *Component) handleDeployedConfigPublishRequest(event *events.DeployedConfigPublishRequest) {
	entry, err := renderedConfigEntryFromDeployedRequest(event)
	if err != nil {
		c.logger.Error("Deployed configuration is invalid; publication was skipped",
			"error", err)
		return
	}

	if event.ConfirmedPod != nil {
		entry.confirmedPod = event.ConfirmedPod.DeepCopy()
	}
	c.mu.RLock()
	templateConfig := c.templateConfig
	hasTemplateConfig := c.hasTemplateConfig
	c.mu.RUnlock()

	if !hasTemplateConfig || templateConfig == nil {
		c.logger.Debug("Skipping deployed-config publish, no template config cached yet",
			"checksum", entry.contentChecksum)
		return
	}

	workItem := c.makePublishWorkItem(
		"deployed:"+entry.contentChecksum,
		templateConfig,
		entry,
		true,
	)

	c.enqueueDeployed(workItem)
}

func renderedConfigEntryFromDeployedRequest(event *events.DeployedConfigPublishRequest) (*renderedConfigEntry, error) {
	if event == nil {
		return nil, fmt.Errorf("deployed config publish request is nil")
	}
	occurrence, err := event.RenderOccurrence()
	if err != nil {
		return nil, fmt.Errorf("authenticating deployed render occurrence: %w", err)
	}
	cycle, err := occurrence.Snapshot()
	if err != nil {
		return nil, fmt.Errorf("reading deployed render occurrence: %w", err)
	}
	output, err := cycle.OutputSnapshot()
	if err != nil {
		return nil, fmt.Errorf("reading deployed render output: %w", err)
	}
	renderedConfig, err := output.Config()
	if err != nil {
		return nil, fmt.Errorf("reading deployed output config: %w", err)
	}
	artifacts, err := output.ArtifactSnapshot()
	if err != nil {
		return nil, fmt.Errorf("reading deployed output artifacts: %w", err)
	}
	planID, err := output.PlanID()
	if err != nil {
		return nil, fmt.Errorf("reading deployed output plan ID: %w", err)
	}
	checksum, err := cycle.ContentChecksum()
	if err != nil {
		return nil, fmt.Errorf("reading deployed output checksum: %w", err)
	}
	return &renderedConfigEntry{
		config:           renderedConfig,
		artifactSnapshot: artifacts,
		outputSnapshot:   output,
		contentChecksum:  checksum,
		planID:           planID,
	}, nil
}

// handleValidationFailed queues the invalid configuration for async publishing.
// Uses correlation ID to match with the corresponding TemplateRenderedEvent.
//
// This method is non-blocking - it queues work for the validationFailedWorker instead
// of making K8S API calls directly.
func (c *Component) handleValidationFailed(event *events.ValidationFailedEvent) {
	correlationID := event.CorrelationID()
	templateConfig, entry, ok := c.lookupCachedConfig(event.EventID(), correlationID, "ValidationFailedEvent", "publish invalid configuration")
	if !ok {
		return
	}

	c.logger.Debug("Queuing invalid configuration for async publishing",
		"config_name", templateConfig.Name,
		"config_namespace", templateConfig.Namespace,
		"error_count", len(event.Errors),
		"correlation_id", correlationID,
	)

	// Queue work for async processing
	workItem := c.makeValidationFailedWorkItem(correlationID, event, templateConfig, entry)

	queueWithCoalesce(c, c.validationFailedWork, workItem, "validation failed", correlationID,
		func(w *validationFailedWorkItem) string { return w.correlationID })
}

// queueWithCoalesce sends workItem on ch with non-blocking semantics and
// "latest wins" coalescing: if the channel is full, drain the pending item,
// drop its renderedConfigs entry (so we don't leak the cached render that
// will never be processed), then push the new item. If the worker grabs the
// drained slot before we can push, log and move on. logName is the work-item
// kind shown in debug logs ("publish" / "validation failed"), and
// correlationOf extracts the correlation ID from a drained item so the
// renderedConfigs cleanup can target the right entry.
func queueWithCoalesce[T any](
	c *Component,
	ch chan T,
	workItem T,
	logName, correlationID string,
	correlationOf func(T) string,
) {
	select {
	case ch <- workItem:
		return
	default:
	}

	// Channel full - drain old work and queue new work (coalescing).
	select {
	case oldWork := <-ch:
		oldID := correlationOf(oldWork)
		c.logger.Debug("Coalescing "+logName+" work",
			"old_correlation_id", oldID,
			"new_correlation_id", correlationID,
		)
		// Cleanup the old entry since we're skipping it.
		c.discardCachedConfig(oldID)
	default:
		// Channel was drained by worker between our checks - just try again.
	}

	select {
	case ch <- workItem:
	default:
		c.logger.Debug(logName+" work channel busy",
			"correlation_id", correlationID)
	}
}

// statusWorkKey returns the coalescing key for a pod status update.
// Format: namespace/runtimeConfigName/podName.
func statusWorkKey(event *events.ConfigAppliedToPodEvent) string {
	return fmt.Sprintf("%s/%s/%s", event.RuntimeConfigNamespace, event.RuntimeConfigName, event.PodName)
}

// handleConfigAppliedToPod queues a pod status update for async processing with coalescing.
//
// This method is non-blocking - it stores work in a map for the statusWorker instead of
// queueing it directly. When multiple updates arrive for the same pod before the worker
// processes them, only the latest update is applied. This prevents channel overflow
// during high-frequency reconciliation cycles.
func (c *Component) handleConfigAppliedToPod(event *events.ConfigAppliedToPodEvent) {
	if !c.isCurrentPodAuthority(event.PodNamespace, event.PodName, event.PodUID, event.PodRuntimeID) {
		c.logger.Debug("Ignoring deployment status from retired pod authority",
			"pod_name", event.PodName,
			"pod_namespace", event.PodNamespace,
			"pod_uid", event.PodUID)
		return
	}
	c.recordPodChecksum(event.PodNamespace, event.PodName, event.Checksum)

	c.logger.Debug("Queuing deployment status update for pod",
		"runtime_config_name", event.RuntimeConfigName,
		"runtime_config_namespace", event.RuntimeConfigNamespace,
		"pod_name", event.PodName,
		"pod_namespace", event.PodNamespace,
		"pod_uid", event.PodUID,
		"checksum", event.Checksum,
		"is_drift_check", event.IsDriftCheck,
	)

	key := statusWorkKey(event)

	workItem := &statusWorkItem{
		event: event,
	}

	// Store (or replace) the pending update for this pod.
	// This provides natural coalescing - newer updates replace older ones.
	c.statusWorkMu.Lock()
	c.statusWorkPending[key] = workItem
	delete(c.statusWorkParked, key)
	c.statusWorkMu.Unlock()

	signalStatusWorker(c.statusWorkTrigger)
}

// handlePodTerminated cleans up pod references when a pod is terminated.
func (c *Component) handlePodTerminated(ctx context.Context, event *events.HAProxyPodTerminatedEvent) {
	c.endpointAuthorityMu.Lock()
	key := podAuthorityKey{namespace: event.PodNamespace, name: event.PodName}
	if current, ok := c.endpointAuthorities[key]; ok && (event.PodUID == "" || current.uid == event.PodUID) {
		delete(c.endpointAuthorities, key)
	}
	c.endpointAuthorityMu.Unlock()

	c.forgetPodChecksum(event.PodNamespace, event.PodName)
	c.dropParkedStatusWork(event.PodNamespace, event.PodName, event.PodUID)

	// Get the namespace from cached templateConfig (namespace-scoped operations).
	c.mu.RLock()
	hasConfig := c.hasTemplateConfig
	namespace := ""
	if c.templateConfig != nil {
		namespace = c.templateConfig.Namespace
	}
	c.mu.RUnlock()

	if !hasConfig || namespace == "" {
		c.logger.Debug("Skipping pod cleanup - no template config available yet",
			"pod_name", event.PodName,
		)
		return
	}

	c.logger.Debug("Cleaning up pod references after termination",
		"pod_name", event.PodName,
		"pod_namespace", event.PodNamespace,
		"crd_namespace", namespace,
	)

	// Convert event to cleanup request with namespace
	cleanupReq := configpublisher.PodCleanupRequest{
		PodName:   event.PodName,
		PodUID:    event.PodUID,
		Namespace: namespace,
	}

	// Call pure publisher (non-blocking - log errors but don't fail)
	ctx, cancel := context.WithTimeout(ctx, timeouts.KubernetesAPITimeout)
	defer cancel()

	if err := c.publisher.CleanupPodReferences(ctx, &cleanupReq); err != nil {
		c.logger.Warn("Failed to cleanup pod references",
			"error", err,
			"pod_name", event.PodName,
			"pod_namespace", event.PodNamespace,
		)
		// Non-blocking - just log the error
		return
	}

	c.logger.Debug("Pod references cleaned up successfully",
		"pod_name", event.PodName,
		"pod_namespace", event.PodNamespace,
	)
}

// handlePodsDiscovered reconciles deployedToPods status against currently running pods.
//
// This cleans up stale entries from pods that terminated while the controller was
// restarting (or before the controller started). It is called whenever HAProxy pods
// are discovered, including on startup and when pods change.
func (c *Component) handlePodsDiscovered(ctx context.Context, event *events.HAProxyPodsDiscoveredEvent) {
	currentAuthorities := make(map[podAuthorityKey]podAuthority, len(event.Endpoints))
	runningPods := make([]configpublisher.PodIdentity, 0, len(event.Endpoints))
	for i := range event.Endpoints {
		endpoint := &event.Endpoints[i]
		currentAuthorities[podAuthorityKey{namespace: endpoint.PodNamespace, name: endpoint.PodName}] = podAuthority{
			uid: endpoint.PodUID, runtimeID: endpoint.PodRuntimeID,
		}
		runningPods = append(runningPods, configpublisher.PodIdentity{
			PodName: endpoint.PodName, PodUID: endpoint.PodUID, PodRuntimeID: endpoint.PodRuntimeID,
		})
	}
	c.endpointAuthorityMu.Lock()
	c.endpointAuthorities = currentAuthorities
	c.endpointAuthoritiesSet = true
	c.endpointAuthorityMu.Unlock()

	// Get the namespace from cached templateConfig (namespace-scoped operations).
	c.mu.RLock()
	hasConfig := c.hasTemplateConfig
	namespace := ""
	if c.templateConfig != nil {
		namespace = c.templateConfig.Namespace
	}
	c.mu.RUnlock()

	if !hasConfig || namespace == "" {
		c.logger.Debug("Skipping pod reconciliation - no template config available yet",
			"pod_count", len(event.Endpoints),
		)
		return
	}

	// Create timeout context derived from the lifecycle ctx (same pattern as handlePodTerminated)
	ctx, cancel := context.WithTimeout(ctx, timeouts.KubernetesAPILongTimeout)
	defer cancel()

	// Reconcile status against running pods (namespace-scoped)
	if err := c.publisher.ReconcileDeployedToPods(ctx, namespace, runningPods); err != nil {
		c.logger.Warn("Failed to reconcile deployed pods status", "error", err)
	} else {
		c.logger.Debug("Reconciled deployed pods status",
			"namespace", namespace,
			"running_pods", len(runningPods))
	}
}

func (c *Component) isCurrentPodAuthority(namespace, name, uid, runtimeID string) bool {
	c.endpointAuthorityMu.RLock()
	defer c.endpointAuthorityMu.RUnlock()
	if !c.endpointAuthoritiesSet {
		return true
	}
	current, ok := c.endpointAuthorities[podAuthorityKey{namespace: namespace, name: name}]
	return ok && current == (podAuthority{uid: uid, runtimeID: runtimeID})
}

// convertAuxiliaryFiles converts dataplane auxiliary files to publisher auxiliary files.
func (c *Component) convertAuxiliaryFiles(dataplaneFiles *dataplane.AuxiliaryFiles) *configpublisher.AuxiliaryFiles {
	if dataplaneFiles == nil {
		return nil
	}

	return &configpublisher.AuxiliaryFiles{
		MapFiles:        dataplaneFiles.MapFiles,
		SSLCertificates: dataplaneFiles.SSLCertificates,
		SSLCaFiles:      dataplaneFiles.SSLCaFiles,
		GeneralFiles:    dataplaneFiles.GeneralFiles,
		CRTListFiles:    dataplaneFiles.CRTListFiles,
	}
}

// getCompressionThreshold returns the effective compression threshold,
// applying the default value when not set in the CRD.
func (c *Component) getCompressionThreshold(templateConfig *v1alpha1.HAProxyTemplateConfig) int64 {
	threshold := templateConfig.Spec.Controller.ConfigPublishing.CompressionThreshold
	if threshold == 0 {
		return config.DefaultCompressionThreshold
	}
	return threshold
}

// handleLostLeadership handles LostLeadershipEvent by clearing cached configuration state.
//
// When a replica loses leadership, leader-only components (including this publisher)
// are stopped via context cancellation. However, we defensively clear cached state
// to ensure clean state if leadership is reacquired.
//
// This prevents scenarios where:
//   - Stale templateConfig from previous leadership period is used
//   - Old renderedConfig is incorrectly published
//   - Cached auxiliary files reference non-existent resources
func (c *Component) handleLostLeadership(_ *events.LostLeadershipEvent) {
	// A reacquiring leader must not trust status stamps from this term.
	if c.publisher != nil {
		c.publisher.ResetStatusStamps()
	}

	c.endpointAuthorityMu.Lock()
	c.endpointAuthorities = make(map[podAuthorityKey]podAuthority)
	c.endpointAuthoritiesSet = false
	c.endpointAuthorityMu.Unlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.hasTemplateConfig || len(c.renderedConfigs) > 0 {
		c.logger.Info("Lost leadership, clearing cached configuration state",
			"had_template_config", c.hasTemplateConfig,
			"rendered_configs_count", len(c.renderedConfigs),
		)
	}

	c.templateConfig = c.seedTemplateConfig
	c.hasTemplateConfig = c.seedTemplateConfig != nil
	c.renderedConfigs = make(map[string]*renderedConfigEntry)
	c.lastRender = nil
	c.lastRenderCorrelationID = ""
	c.lastPublishedChecksum = ""
	c.publicationBlocked = false
	if c.activePublicationCancel != nil {
		c.activePublicationCancel()
		c.activePublicationCancel = nil
	}
	c.lastPublishedOutputSnapshot = nil
	c.lastPublishedEntry = nil
	c.publicationTerm++
	c.latestPublishGeneration = 0
	c.latestInvalidGeneration = 0
	c.publishSuperseded = supersedePublication(c.publishSuperseded)
	c.invalidSuperseded = supersedePublication(c.invalidSuperseded)

	// Drop every queued deploy-driven publish so a lost leader doesn't later
	// flush a stale spec write. The Component outlives a leadership transition,
	// so anything queued under the previous term would otherwise be published
	// after the term ends — writing a spec the new leader never deployed.
	// (mu is held here; the queue and pending mutexes are always acquired after
	// mu, never before, so this nesting can't deadlock.)
	c.deployedPendingMu.Lock()
	c.deployedPending = nil
	c.deployedChecksumByPod = make(map[podAuthorityKey]string)
	c.deployedPendingMu.Unlock()

	c.pendingMu.Lock()
	c.pendingPublish = nil
	c.pendingMu.Unlock()
}

// enqueueDeployed appends a deployed render to the pending queue and wakes the
// publish worker.
//
// Exact output roots are deduplicated rather than coalesced: an output already
// queued is replaced in place (keeping its position), but a different output
// never displaces one. Legacy entries retain checksum identity. Dropping an
// entry can leave status.deployedToPods advertising unpublished content.
func (c *Component) enqueueDeployed(work *publishWorkItem) {
	c.deployedPendingMu.Lock()
	replaced := false
	for i, pending := range c.deployedPending {
		if sameDeployedOutput(pending.entry, work.entry) {
			c.deployedPending[i] = work
			replaced = true
			break
		}
	}
	if !replaced {
		c.deployedPending = append(c.deployedPending, work)
	}
	pruned := c.pruneSupersededDeployedLocked()
	depth := len(c.deployedPending)
	c.deployedPendingMu.Unlock()

	c.logger.Debug("Queued deployed config for publishing",
		"checksum", work.entry.contentChecksum,
		"correlation_id", work.correlationID,
		"replaced_same_output", replaced,
		"pruned_superseded", pruned,
		"queue_depth", depth)

	select {
	case c.deployedTrigger <- struct{}{}:
	default: // already signalled; the worker drains the whole queue
	}
}

// recordPodChecksum notes the checksum a pod reports running, so
// pruneSupersededDeployedLocked can tell a checksum a reader may still
// observe in status.deployedToPods from one the fleet has moved past.
func (c *Component) recordPodChecksum(namespace, name, checksum string) {
	if checksum == "" {
		return
	}
	c.deployedPendingMu.Lock()
	defer c.deployedPendingMu.Unlock()
	if c.deployedChecksumByPod == nil {
		c.deployedChecksumByPod = make(map[podAuthorityKey]string)
	}
	c.deployedChecksumByPod[podAuthorityKey{namespace: namespace, name: name}] = checksum
}

// forgetPodChecksum drops a pod's reported checksum once it is gone, so a
// departed pod can't pin a queue entry forever.
func (c *Component) forgetPodChecksum(namespace, name string) {
	c.deployedPendingMu.Lock()
	defer c.deployedPendingMu.Unlock()
	delete(c.deployedChecksumByPod, podAuthorityKey{namespace: namespace, name: name})
}

// pruneSupersededDeployedLocked drops queued deployed renders the fleet has
// demonstrably moved past, returning how many it removed. Caller holds
// deployedPendingMu. A deployed entry owns its rendered bytes inline (it is
// never in renderedConfigs), so dropping it from the queue frees them.
//
// The guarantee this preserves is the observable one: a checksum must reach
// `spec` while some pod is still reported at it, because that is the only
// window in which a reader can see it in status.deployedToPods and try to
// resolve it. So an entry is dropped only when BOTH hold:
//
//   - no pod currently reports its checksum, and
//   - a STRICTLY NEWER queued entry's checksum IS reported by some pod.
//
// The second condition is what makes this safe against the status lag: a pod
// running the newer checksum proves the fleet advanced past everything queued
// before it. Entries newer than the newest reported one are always kept, so a
// deploy whose ConfigAppliedToPodEvent hasn't arrived yet can never be pruned.
func (c *Component) pruneSupersededDeployedLocked() int {
	if len(c.deployedPending) < 2 {
		return 0
	}

	live := make(map[string]struct{}, len(c.deployedChecksumByPod))
	for _, checksum := range c.deployedChecksumByPod {
		live[checksum] = struct{}{}
	}

	// Newest queued entry that some pod actually reports. Everything before it
	// has been superseded on the fleet; everything from it onward is kept.
	newestLive := -1
	for i := len(c.deployedPending) - 1; i >= 0; i-- {
		if _, ok := live[c.deployedPending[i].entry.contentChecksum]; ok {
			newestLive = i
			break
		}
	}
	if newestLive <= 0 {
		return 0 // nothing reported yet, or the oldest entry is the live one
	}

	pruned := 0
	kept := c.deployedPending[:0]
	for i, work := range c.deployedPending {
		if i < newestLive {
			if _, ok := live[work.entry.contentChecksum]; !ok {
				pruned++
				continue
			}
		}
		kept = append(kept, work)
	}
	clear(c.deployedPending[len(kept):]) // release the dropped entries' configs
	c.deployedPending = kept
	return pruned
}

// requeueDeployedFront puts a deployed render back at the head of the queue,
// for the throttle path: the gate closed before it could be published, and it
// must keep its place ahead of anything queued behind it.
func (c *Component) requeueDeployedFront(work *publishWorkItem) {
	c.deployedPendingMu.Lock()
	defer c.deployedPendingMu.Unlock()
	for i, pending := range c.deployedPending {
		if sameDeployedOutput(pending.entry, work.entry) {
			c.deployedPending[i] = work
			return
		}
	}
	c.deployedPending = append([]*publishWorkItem{work}, c.deployedPending...)
}

func sameDeployedOutput(left, right *renderedConfigEntry) bool {
	if left == nil || right == nil {
		return false
	}
	if left.outputSnapshot != nil || right.outputSnapshot != nil {
		if left.outputSnapshot == nil || right.outputSnapshot == nil {
			return false
		}
		same, err := left.outputSnapshot.SameRoot(right.outputSnapshot)
		return err == nil && same
	}
	if left.artifactSnapshot != nil || right.artifactSnapshot != nil {
		if left.artifactSnapshot == nil || right.artifactSnapshot == nil {
			return false
		}
		same, err := dataplane.SnapshotContentEqual(
			left.config, left.artifactSnapshot,
			right.config, right.artifactSnapshot,
		)
		return err == nil && same
	}
	return dataplane.ContentEqual(left.config, left.auxFiles, right.config, right.auxFiles)
}

// takeDeployed pops the oldest pending deployed render, or nil when empty.
func (c *Component) takeDeployed() *publishWorkItem {
	c.deployedPendingMu.Lock()
	defer c.deployedPendingMu.Unlock()
	if len(c.deployedPending) == 0 {
		return nil
	}
	work := c.deployedPending[0]
	c.deployedPending = c.deployedPending[1:]
	return work
}

// newestAcknowledged returns the newest queued render that carries a worker
// receipt, leaving the queue untouched.
func (c *Component) newestAcknowledged() *publishWorkItem {
	c.deployedPendingMu.Lock()
	defer c.deployedPendingMu.Unlock()
	for i := len(c.deployedPending) - 1; i >= 0; i-- {
		if work := c.deployedPending[i]; work.entry.confirmedPod != nil {
			return work
		}
	}
	return nil
}

// dropDeployedThrough removes a published render and every render queued
// before it: publishing an older one afterwards would move spec backwards.
func (c *Component) dropDeployedThrough(work *publishWorkItem) {
	c.deployedPendingMu.Lock()
	defer c.deployedPendingMu.Unlock()
	for i, pending := range c.deployedPending {
		if sameDeployedOutput(pending.entry, work.entry) {
			clear(c.deployedPending[:i+1])
			c.deployedPending = c.deployedPending[i+1:]
			return
		}
	}
}

// deployedQueueDepth reports how many deployed renders are still queued.
func (c *Component) deployedQueueDepth() int {
	c.deployedPendingMu.Lock()
	defer c.deployedPendingMu.Unlock()
	return len(c.deployedPending)
}
