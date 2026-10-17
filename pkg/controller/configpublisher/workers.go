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
	"maps"
	"slices"
	"sync"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/timeouts"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/configpublisher"
)

const haproxyConfigPath = "/etc/haproxy/haproxy.cfg"

// buildPublishRequest assembles the PublishRequest fields shared by both the
// happy-path publish and the validation-failed publish. Callers layer the
// extra fields (NameSuffix, ValidationError) on top.
func (c *Component) buildPublishRequest(identity publishConfigIdentity, entry *renderedConfigEntry) *configpublisher.PublishRequest {
	request := &configpublisher.PublishRequest{
		TemplateConfigName:      identity.name,
		TemplateConfigNamespace: identity.namespace,
		TemplateConfigUID:       identity.uid,
		ConfigPath:              haproxyConfigPath,
		CompressionThreshold:    identity.compressionThreshold,
		ConfirmedPod:            entry.confirmedPod.DeepCopy(),
	}
	if entry.outputSnapshot != nil {
		request.OutputSnapshot = entry.outputSnapshot
		return request
	}
	request.Checksum = entry.contentChecksum
	request.Config = entry.config
	request.AuxiliaryFileSnapshot = entry.artifactSnapshot
	request.AuxiliaryFiles = c.convertAuxiliaryFiles(entry.auxFiles)
	return request
}

// discardCachedConfig drops the rendered config entry for the given correlation
// ID. Used by both worker paths once the entry is no longer needed (after
// successful publish or to clean up after a publish failure).
func (c *Component) discardCachedConfig(correlationID string) {
	c.mu.Lock()
	delete(c.renderedConfigs, correlationID)
	c.mu.Unlock()
}

// publishWorker processes publish work items asynchronously.
// This worker runs in a separate goroutine to prevent blocking the event loop
// on slow K8S API calls.
//
// Throttling is delegated to publishThrottle (a *throttle.LeadingEdge): the
// first publish after idle fires immediately; submissions inside the
// refractory period are stashed in pendingPublish, and the worker flushes
// the latest buffered item when publishThrottle.FiredCh() signals.
func (c *Component) publishWorker(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		// Deploy-driven work first, and drain ALL of it before touching
		// validation work. A select picks uniformly at random among ready
		// cases, so with validation publishes arriving continuously a deployed
		// item could sit unread for seconds — measured 5.2s, with three
		// validation publishes going out ahead of it. For exactly that long the
		// CR contradicts itself: status.deployedToPods already advertises a
		// checksum whose content spec has not published, so no reader can
		// resolve it. flushPendingPublish already orders deploy work first (see
		// below); this makes the queue-level order match.
		// ...but ONLY while the throttle gate is open. Under a closed gate
		// processPublishWork puts the item straight back at the front of the
		// queue, so draining here would pop it again on the very next
		// iteration: a tight loop with nothing to block on, pinning a core for
		// the whole refractory window. That is precisely the burst this queue
		// exists for (the first item closes the gate, the rest arrive during
		// refractory), so it would fire in the common case, not a corner.
		//
		// Falling through to the select instead lets FiredCh drive
		// flushPendingPublish, which drains deploy work first and re-arms
		// itself while the queue is non-empty — same order, no spin. The
		// ScheduleFlush below is what makes that safe: enqueueDeployed only
		// signals deployedTrigger, so without it a queued item under a closed
		// gate would have nothing left to wake it and would stall until an
		// unrelated event arrived.
		allowed := c.publicationAllowed()
		if allowed && c.publishThrottle.Available() {
			if work := c.takeDeployed(); work != nil {
				c.processPublishWork(ctx, work)
				continue
			}
		} else if allowed && c.deployedQueueDepth() > 0 {
			c.publishThrottle.ScheduleFlush()
		}

		select {
		case work := <-c.publishWork:
			if ctx.Err() != nil {
				return
			}
			c.processPublishWork(ctx, work)
		case <-c.deployedTrigger:
			// Wake only; the drain at the top of the loop takes the item.
		case <-c.publishThrottle.FiredCh():
			c.flushPendingPublish(ctx)
		case request := <-c.drainWork:
			request.done <- c.publishNewestAcknowledged(ctx, request.ctx)
		case <-ctx.Done():
			return
		}
	}
}

// publishNewestAcknowledged publishes, past the throttle, the newest queued
// render with a worker receipt: the deployment intent names the newest
// deployment, so only its checkpoint keeps recovery available. The queue
// keeps every render until that publication commits.
func (c *Component) publishNewestAcknowledged(ctx, drainCtx context.Context) error {
	if err := drainCtx.Err(); err != nil {
		return err
	}
	publishCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(drainCtx, cancel)()
	work := c.newestAcknowledged()
	if work == nil {
		return nil
	}
	if !c.publicationAllowed() {
		return errors.New("the acknowledged checkpoint stays unpublished while rendering is blocked")
	}
	if !c.executePublish(publishCtx, work) {
		return errors.New("the acknowledged checkpoint was not published")
	}
	c.dropDeployedThrough(work)
	return nil
}

// processPublishWork decides whether to publish immediately or buffer for throttle.
func (c *Component) processPublishWork(ctx context.Context, work *publishWorkItem) {
	if ctx.Err() != nil {
		return
	}
	if !c.publishWorkCurrent(work) {
		c.deferOrDiscardPublication(ctx, work)
		return
	}

	c.logger.Debug("Processing publish work",
		"config_name", work.config.name,
		"config_namespace", work.config.namespace,
		"config_bytes", len(work.entry.config),
		"correlation_id", work.correlationID,
	)

	// Repeated deploy notifications need no desired-state audit: the matching
	// validation path performs that reconciliation.
	if work.deployDriven && c.skipIfAlreadyPublished(work, "skipping publish, config unchanged") {
		return
	}

	// Throttle: leading-edge refractory via publishThrottle. If the gate is
	// closed, buffer the latest work and ask the throttle to wake us when
	// the refractory expires.
	if !c.publishThrottle.Available() {
		// Deploy-driven work goes BACK on the deployed queue, at the front, so
		// it keeps its place. A one-slot buffer here would undo the queue's
		// whole guarantee: draining a burst closes the gate on the first
		// publish, and every remaining deployed checksum would then overwrite
		// that slot, leaving only the newest — exactly the drop this queue
		// exists to prevent. Validation work keeps its latest-wins slot, which
		// is correct for it: only the newest render matters there.
		var superseded *publishWorkItem
		if work.deployDriven {
			c.requeueDeployedFront(work)
		} else {
			c.pendingMu.Lock()
			superseded = c.pendingPublish
			c.pendingPublish = work
			c.pendingMu.Unlock()
		}

		// Drop the superseded item's cached render AFTER releasing pendingMu:
		// discardCachedConfig takes mu, and handleLostLeadership takes mu THEN
		// pendingMu — nesting pendingMu->mu here would invert that order and
		// deadlock. Keeping the two mutexes non-nested at this site makes the
		// mu->pendingMu in handleLostLeadership the only nesting, so no AB-BA
		// lock inversion is possible.
		if superseded != nil {
			c.discardCachedConfig(superseded.correlationID)
		}

		c.logger.Debug("Throttling CRD publish, buffering for later",
			"checksum", work.entry.contentChecksum,
			"correlation_id", work.correlationID,
			"deploy_driven", work.deployDriven,
		)
		c.publishThrottle.ScheduleFlush()
		return
	}

	// Gate open — publish immediately
	c.executePublish(ctx, work)
}

// flushPendingPublish publishes one buffered work item when the throttle timer expires.
func (c *Component) flushPendingPublish(ctx context.Context) {
	if ctx.Err() != nil || !c.publicationAllowed() {
		return
	}

	deployWork := c.takeDeployed()

	c.pendingMu.Lock()
	validationWork := c.pendingPublish
	if deployWork == nil {
		c.pendingPublish = nil
	}
	c.pendingMu.Unlock()

	work := deployWork
	if work == nil {
		work = validationWork
	}
	if work == nil {
		return
	}

	// Every deployed checksum stays ahead of the latest validation publish.
	// Leave validationWork in its slot until the deployed queue is empty.
	if deployWork != nil && (c.deployedQueueDepth() > 0 || validationWork != nil) {
		c.publishThrottle.ScheduleFlush()
	}

	if !c.publishWorkCurrent(work) {
		c.deferOrDiscardPublication(ctx, work)
		return
	}
	// Re-check content deduplication (content may have been published by another path).
	if work.deployDriven && c.skipIfAlreadyPublished(work, "skipping throttled publish, config already published") {
		return
	}
	c.logger.Debug("Flushing throttled CRD publish",
		"correlation_id", work.correlationID,
		"deploy_driven", work.deployDriven,
	)
	c.executePublish(ctx, work)
}

// skipIfAlreadyPublished uses exact root identity for authenticated outputs and
// exact content comparison for legacy callers. A match drops the cached entry.
func (c *Component) skipIfAlreadyPublished(work *publishWorkItem, msg string) bool {
	if work.entry.confirmedPod != nil {
		return false
	}
	c.mu.RLock()
	lastOutput := c.lastPublishedOutputSnapshot
	lastEntry := c.lastPublishedEntry
	c.mu.RUnlock()

	if work.entry.outputSnapshot != nil {
		if lastOutput == nil {
			return false
		}
		same, err := work.entry.outputSnapshot.SameRoot(lastOutput)
		if err != nil || !same {
			return false
		}
	} else if lastOutput != nil || !sameDeployedOutput(work.entry, lastEntry) {
		return false
	}
	c.logger.Debug(msg,
		"checksum", work.entry.contentChecksum,
		"correlation_id", work.correlationID,
	)
	c.discardCachedConfig(work.correlationID)
	return true
}

// executePublish performs the actual K8S API call to publish the config CRD
// and reports whether the publication committed.
func (c *Component) executePublish(ctx context.Context, work *publishWorkItem) bool {
	request := work.request
	if request == nil {
		request = c.buildPublishRequest(work.config, work.entry)
		work.request = request
	}

	result, complete := c.publishUntilComplete(ctx, request, work.correlationID, work.superseded, func() bool {
		return c.publishWorkCurrent(work)
	})
	if !complete {
		c.deferOrDiscardPublication(ctx, work)
		return false
	}
	if !c.commitPublish(ctx, work, result) {
		c.deferOrDiscardPublication(ctx, work)
		return false
	}

	c.logger.Debug("Runtime configuration published successfully",
		"runtime_config_name", result.RuntimeConfigName,
		"runtime_config_namespace", result.RuntimeConfigNamespace,
		"checksum", work.entry.contentChecksum,
		"correlation_id", work.correlationID,
	)
	return true
}

func (c *Component) commitPublish(
	ctx context.Context,
	work *publishWorkItem,
	result *configpublisher.PublishResult,
) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ctx.Err() != nil || !c.publishWorkCurrentLocked(work) {
		delete(c.renderedConfigs, work.correlationID)
		return false
	}

	alreadyPublished := false
	if work.entry.outputSnapshot != nil && c.lastPublishedOutputSnapshot != nil {
		alreadyPublished, _ = work.entry.outputSnapshot.SameRoot(c.lastPublishedOutputSnapshot)
	} else if work.entry.outputSnapshot == nil && c.lastPublishedOutputSnapshot == nil {
		alreadyPublished = sameDeployedOutput(work.entry, c.lastPublishedEntry)
	}
	c.lastPublishedChecksum = work.entry.contentChecksum
	c.lastPublishedOutputSnapshot = work.entry.outputSnapshot
	c.lastPublishedEntry = cloneRenderedConfigEntry(work.entry)
	delete(c.renderedConfigs, work.correlationID)
	c.publishThrottle.MarkFired()
	if alreadyPublished {
		return true
	}
	c.eventBus.Publish(events.NewConfigPublishedEvent(
		result.RuntimeConfigName,
		result.RuntimeConfigNamespace,
		len(result.MapFileNames),
		len(result.SecretNames)+len(result.SSLCaFileNames),
	))
	return true
}

// validationFailedWorker processes validation failed work items asynchronously.
func (c *Component) validationFailedWorker(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		select {
		case work := <-c.validationFailedWork:
			if ctx.Err() != nil {
				return
			}
			c.processValidationFailedWork(ctx, work)
		case <-ctx.Done():
			return
		}
	}
}

// processValidationFailedWork performs the actual invalid config publishing.
func (c *Component) processValidationFailedWork(ctx context.Context, work *validationFailedWorkItem) {
	if !c.invalidWorkCurrent(work) {
		c.discardCachedConfig(work.correlationID)
		return
	}

	c.logger.Debug("Processing validation failed work",
		"config_name", work.config.name,
		"config_namespace", work.config.namespace,
		"correlation_id", work.correlationID,
	)

	request := work.request
	if request == nil {
		request = c.buildPublishRequest(work.config, work.entry)
		request.NameSuffix = "-invalid"
		request.ValidationError = work.validationError
		work.request = request
	}

	result, complete := c.publishUntilComplete(ctx, request, work.correlationID, work.superseded, func() bool {
		return c.invalidWorkCurrent(work)
	})
	if !complete {
		c.discardCachedConfig(work.correlationID)
		return
	}

	c.mu.Lock()
	if ctx.Err() != nil || !c.invalidWorkCurrentLocked(work) {
		delete(c.renderedConfigs, work.correlationID)
		c.mu.Unlock()
		return
	}
	delete(c.renderedConfigs, work.correlationID)
	c.mu.Unlock()

	c.logger.Warn("Invalid runtime configuration published",
		"runtime_config_name", result.RuntimeConfigName,
		"runtime_config_namespace", result.RuntimeConfigNamespace,
		"validation_error", work.validationError,
		"correlation_id", work.correlationID,
	)
}

func (c *Component) publishUntilComplete(
	ctx context.Context,
	request *configpublisher.PublishRequest,
	correlationID string,
	superseded <-chan struct{},
	current func() bool,
) (*configpublisher.PublishResult, bool) {
	for retry := 0; ; retry++ {
		if ctx.Err() != nil || !current() {
			return nil, false
		}

		result, err := c.attemptPublication(ctx, request, current)
		if err == nil {
			return result, true
		}
		if ctx.Err() != nil || !current() {
			return nil, false
		}
		if !configpublisher.IsRetryablePublicationError(err) {
			c.logger.Error("Configuration publication rejected; fix the API error before retrying",
				"error", err,
				"config_name", request.TemplateConfigName,
				"correlation_id", correlationID,
			)
			return nil, false
		}

		delay := publicationRetryBackoff(retry + 1)
		c.logger.Warn("Configuration publication incomplete; retrying",
			"error", err,
			"config_name", request.TemplateConfigName,
			"correlation_id", correlationID,
			"retry_in", delay,
		)
		wait := c.publicationRetryWait
		if wait == nil {
			wait = waitForPublicationRetry
		}
		if !wait(ctx, delay, superseded) || !current() {
			return nil, false
		}
	}
}

func (c *Component) attemptPublication(ctx context.Context, request *configpublisher.PublishRequest, current func() bool) (*configpublisher.PublishResult, error) {
	c.publicationCallMu.Lock()
	defer c.publicationCallMu.Unlock()
	if ctx.Err() != nil || !current() {
		return nil, context.Canceled
	}
	publishCtx, cancel := context.WithTimeout(ctx, timeouts.KubernetesAPILongTimeout)
	defer cancel()
	c.mu.Lock()
	if request.NameSuffix == "" {
		c.activePublicationCancel = cancel
		if c.publicationBlocked || c.gatePinned {
			cancel()
		}
	}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.activePublicationCancel = nil
		c.mu.Unlock()
	}()
	cancelledBeforeStart := publishCtx.Err() != nil
	result, err := c.publisher.PublishConfig(publishCtx, request)
	if request.NameSuffix == "" && !cancelledBeforeStart {
		c.wakeParkedStatusWork()
	}
	if err == nil {
		err = c.publisher.PublishRetained(publishCtx, request)
	}
	return result, err
}

// statusWorker processes pod status update work items asynchronously with coalescing.
//
// Instead of processing each update as it arrives, this worker waits for a trigger
// signal and then processes all pending updates at once. This ensures that when
// multiple updates arrive for the same pod, only the latest one is applied.
//
// Throttling is delegated to statusThrottle (a *throttle.LeadingEdge): the
// first flush after idle fires immediately; while inside the refractory
// period, pending updates keep accumulating in statusWorkPending and the
// worker flushes them when statusThrottle.FiredCh() signals.
func (c *Component) statusWorker(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		select {
		case <-c.statusWorkTrigger:
			if ctx.Err() != nil {
				return
			}
			c.handleStatusTrigger(ctx)
		case <-c.statusThrottle.FiredCh():
			c.processAllPendingStatusWork(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// handleStatusTrigger decides whether to process status updates immediately or defer.
func (c *Component) handleStatusTrigger(ctx context.Context) {
	if c.statusThrottle.Available() {
		c.processAllPendingStatusWork(ctx)
		return
	}
	// Inside refractory — wake up at the end. Pending updates already
	// accumulate (and coalesce per pod) in statusWorkPending.
	c.statusThrottle.ScheduleFlush()
}

// processAllPendingStatusWork drains the pending status work map and applies
// each pod's latest entry in parallel.
//
// Two design choices, both load-bearing:
//
//  1. Per-pod fan-out. Each pod's status entry lives under its own server-
//     side-apply field manager (`haptic-pod-status-<podName>`), so concurrent
//     Applies to different pods can't conflict at the apiserver. Spawning
//     one goroutine per pod cuts batch latency from `N × per-apply-time` to
//     ~`1 × per-apply-time`. Under heavy parallel-test churn this was the
//     bottleneck that let spec advance faster than status could catch up;
//     symptom on CI pipeline 2559961278 was pods stuck at intermediate
//     checksums for the full 90 s `waitForControllerDeployed` budget.
//
//  2. Latest-at-apply-time per pod. The old implementation snapshotted the
//     entire pending map at batch start, then iterated. If a newer event
//     for a pod arrived during a (serial) apply, the snapshot value was
//     already obsolete — we'd push the stale value to the apiserver and
//     pick up the newer one only on the NEXT throttle window. Instead,
//     each goroutine atomically pops the LATEST entry for its pod at the
//     moment the apply starts. The overwrite-by-pod-key semantics in
//     handleConfigAppliedToPod guarantee that whatever the map holds is
//     the most recent event seen for that pod up to the pop.
func (c *Component) processAllPendingStatusWork(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}

	c.statusWorkMu.Lock()
	if len(c.statusWorkPending) == 0 {
		c.statusWorkMu.Unlock()
		return
	}
	podKeys := slices.Collect(maps.Keys(c.statusWorkPending))
	c.statusWorkMu.Unlock()

	c.logger.Debug("Processing coalesced status updates",
		"pending_pod_count", len(podKeys),
	)

	var wg sync.WaitGroup
	for _, podKey := range podKeys {
		wg.Add(1)
		go func(podKey string) {
			defer wg.Done()

			c.statusWorkMu.Lock()
			work, ok := c.statusWorkPending[podKey]
			if ok {
				delete(c.statusWorkPending, podKey)
			}
			c.statusWorkMu.Unlock()
			if !ok {
				// Another goroutine raced and popped first — only possible
				// if a future change introduces a second drain path. Today
				// this fan-out is the sole reader, so the branch is
				// defensive rather than expected.
				return
			}
			c.processStatusWork(ctx, work)
		}(podKey)
	}
	wg.Wait()

	// Record write time for throttle refractory.
	c.statusThrottle.MarkFired()
}

// processStatusWork performs the actual pod status update.
func (c *Component) processStatusWork(ctx context.Context, work *statusWorkItem) {
	event := work.event
	c.endpointAuthorityMu.RLock()
	if c.endpointAuthoritiesSet {
		current, ok := c.endpointAuthorities[podAuthorityKey{namespace: event.PodNamespace, name: event.PodName}]
		if !ok || current != (podAuthority{uid: event.PodUID, runtimeID: event.PodRuntimeID}) {
			c.endpointAuthorityMu.RUnlock()
			return
		}
	}
	defer c.endpointAuthorityMu.RUnlock()

	c.logger.Debug("Processing status update for pod",
		"runtime_config_name", event.RuntimeConfigName,
		"runtime_config_namespace", event.RuntimeConfigNamespace,
		"pod_name", event.PodName,
		"checksum", event.Checksum,
	)

	// Convert event to status update
	update := configpublisher.DeploymentStatusUpdate{
		RuntimeConfigName:      event.RuntimeConfigName,
		RuntimeConfigNamespace: event.RuntimeConfigNamespace,
		PodName:                event.PodName,
		PodUID:                 event.PodUID,
		PodRuntimeID:           event.PodRuntimeID,
		Checksum:               event.Checksum,
		IsDriftCheck:           event.IsDriftCheck,
	}

	// Extract error information from sync metadata if available
	if event.SyncMetadata != nil {
		update.Error = event.SyncMetadata.Error
		update.AppliedPlanID = event.SyncMetadata.AppliedPlanID
		update.RunningPlanID = event.SyncMetadata.RunningPlanID
		update.Mode = event.SyncMetadata.Mode
		update.Reasons = event.SyncMetadata.Reasons
	}

	// Call pure publisher with timeout context
	updateCtx, cancel := context.WithTimeout(ctx, timeouts.KubernetesAPITimeout)
	defer cancel()

	c.statusWorkMu.Lock()
	attempts := c.statusPublicationAttempts
	c.statusWorkMu.Unlock()
	if err := c.publisher.UpdateDeploymentStatus(updateCtx, &update); err != nil {
		if errors.Is(err, configpublisher.ErrRuntimeConfigNotPublished) && ctx.Err() == nil {
			c.logger.Debug("HAProxyCfg does not publish the pod's checksum yet, parking pod status update",
				"runtime_config_name", event.RuntimeConfigName,
				"pod_name", event.PodName,
				"checksum", event.Checksum,
			)
			c.parkStatusWork(work, attempts)
			return
		}
		c.logger.Warn("Failed to update deployment status",
			"error", err,
			"runtime_config_name", event.RuntimeConfigName,
			"pod_name", event.PodName,
		)
		return
	}

	c.logger.Debug("Deployment status updated successfully",
		"runtime_config_name", event.RuntimeConfigName,
		"pod_name", event.PodName,
	)
}

// parkStatusWork holds an update the HAProxyCfg could not take until the next
// publication attempt, the only writer of what it waits for. An attempt made
// since attemptsSeen may already have written it, so the update is pending
// again at once. A newer pending update for the pod supersedes it.
func (c *Component) parkStatusWork(work *statusWorkItem, attemptsSeen uint64) {
	key := statusWorkKey(work.event)
	c.statusWorkMu.Lock()
	defer c.statusWorkMu.Unlock()
	if _, newer := c.statusWorkPending[key]; newer {
		return
	}
	if c.statusPublicationAttempts == attemptsSeen {
		c.statusWorkParked[key] = work
		return
	}
	c.statusWorkPending[key] = work
	signalStatusWorker(c.statusWorkTrigger)
}

// wakeParkedStatusWork makes every parked update pending: the publication
// attempt that calls it may have published the checksum one waits for.
func (c *Component) wakeParkedStatusWork() {
	c.statusWorkMu.Lock()
	defer c.statusWorkMu.Unlock()
	c.statusPublicationAttempts++
	if len(c.statusWorkParked) == 0 {
		return
	}
	maps.Copy(c.statusWorkPending, c.statusWorkParked)
	clear(c.statusWorkParked)
	signalStatusWorker(c.statusWorkTrigger)
}

// dropParkedStatusWork forgets a terminated pod's parked update, so a later
// publication cannot re-apply the entries its cleanup removed. An empty uid
// matches every incarnation.
func (c *Component) dropParkedStatusWork(namespace, name, uid string) {
	c.statusWorkMu.Lock()
	defer c.statusWorkMu.Unlock()
	maps.DeleteFunc(c.statusWorkParked, func(_ string, work *statusWorkItem) bool {
		return work.event.PodNamespace == namespace && work.event.PodName == name &&
			(uid == "" || work.event.PodUID == uid)
	})
}

func signalStatusWorker(trigger chan<- struct{}) {
	select {
	case trigger <- struct{}{}:
	default: // already signalled; the worker drains every pending update
	}
}

// verdictWorker writes the render gate's verdicts to the HAProxyCfg status.
//
// It is a worker and not an event-loop call because the write is an apiserver
// round-trip: on the loop it would stall every other event the publisher
// handles — pod status, publishes, leadership — behind one slow API call.
func (c *Component) verdictWorker(ctx context.Context) {
	for {
		select {
		case <-c.verdictTrigger:
			c.processPendingVerdict(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (c *Component) processPendingVerdict(ctx context.Context) {
	c.verdictMu.Lock()
	verdict := c.pendingVerdict
	c.pendingVerdict = nil
	c.verdictMu.Unlock()
	if verdict == nil {
		return
	}

	writeCtx, cancel := context.WithTimeout(ctx, timeouts.KubernetesAPITimeout)
	defer cancel()
	if err := c.publisher.ApplyGateVerdict(writeCtx, verdict); err != nil {
		c.logger.Warn("Failed to write the render gate verdict to HAProxyCfg",
			"error", err, "plan", verdict.PlanID)
	}
}
