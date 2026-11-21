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

// Package httpstore provides the event adapter for HTTP resource fetching.
//
// This package wraps the pure httpstore component (pkg/httpstore) with event
// coordination. It manages refresh timers and publishes events when content
// changes, allowing the reconciliation pipeline to validate new content before
// accepting it.
package httpstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/component"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/logging"
	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/httpstore"
)

const (
	// ComponentName is the unique identifier for this component.
	ComponentName = "httpstore"

	// EventBufferSize is the size of the event subscription buffer.
	// Low-volume component handling validation events (~1-2 per reconciliation);
	// HTTP refresh operations are timer-driven, not event-driven.
	EventBufferSize = busevents.StandardSubscriberBuffer
)

// Component wraps HTTPStore with event coordination.
//
// It manages:
//   - Refresh timers for URLs with interval > 0
//   - Event publishing when content changes
//   - Pending content promotion/rejection based on proposal validation results
//   - Periodic eviction of unused cache entries
//
// Event subscriptions:
//   - ProposalValidationCompletedEvent: Check if our validation, promote/reject pending
//
// Event publications:
//   - ProposalValidationRequestedEvent: When refreshed content differs from accepted
//   - HTTPResourceAcceptedEvent: When pending content is promoted
//   - ReconciliationTriggeredEvent: After successful validation promotion
type Component struct {
	eventBus         *busevents.EventBus
	eventChan        <-chan busevents.Event
	store            *httpstore.HTTPStore
	refreshStoreURL  func(context.Context, string, uint64) (*httpstore.PendingVersion, error)
	evictStoreUnused func() []string
	logger           *slog.Logger
	prepareAuthority chan struct{}

	// Refresh timer management
	mu                      sync.Mutex
	refreshers              map[string]*time.Timer // URL -> refresh timer
	refreshManaged          map[string]bool
	refreshPending          map[string]bool
	refreshImmediate        map[string]bool
	refreshGeneration       map[string]uint64
	refreshSourceGeneration map[string]uint64
	refreshCallbacks        sync.WaitGroup
	ctx                     context.Context
	cancel                  context.CancelFunc
	stopped                 bool

	// Eviction configuration
	evictionInterval time.Duration // How often to run eviction (0 = disabled)

	pendingValidation      *validationBatch
	queuedValidationSource string

	outages *criticalOutages
}

type validationBatch struct {
	requestID string
	entries   []validationBatchEntry
}

type validationBatchEntry struct {
	url         string
	checksum    string
	revision    uint64
	contentSize int
}

// New creates a new HTTPStore event adapter component.
//
// The component subscribes to the EventBus during construction (before EventBus.Start())
// to ensure proper startup synchronization.
//
// Parameters:
//   - eventBus: The event bus for coordination
//   - logger: Logger for debug messages
//   - evictionMaxAge: Maximum age for unused entries before eviction (0 disables eviction)
func New(eventBus *busevents.EventBus, logger *slog.Logger, evictionMaxAge time.Duration) *Component {
	if logger == nil {
		logger = slog.Default()
	}

	// Subscribe to ProposalValidationCompleted events for HTTP content validation.
	// We handle both valid and invalid results via the same event type (Valid field).
	eventChan := eventBus.SubscribeTypes(ComponentName, EventBufferSize,
		events.EventTypeProposalValidationCompleted,
	)
	store := httpstore.New(logger, evictionMaxAge)

	result := &Component{
		eventBus:                eventBus,
		eventChan:               eventChan,
		store:                   store,
		refreshStoreURL:         store.RefreshURLVersionForGeneration,
		evictStoreUnused:        store.EvictUnused,
		logger:                  logger.With("component", ComponentName),
		refreshers:              make(map[string]*time.Timer),
		refreshManaged:          make(map[string]bool),
		refreshPending:          make(map[string]bool),
		refreshImmediate:        make(map[string]bool),
		refreshGeneration:       make(map[string]uint64),
		refreshSourceGeneration: make(map[string]uint64),
		evictionInterval:        evictionMaxAge, // Run eviction at same cadence as maxAge
		prepareAuthority:        make(chan struct{}, 1),
		outages:                 newCriticalOutages(),
	}
	result.prepareAuthority <- struct{}{}
	return result
}

// Name returns the unique identifier for this component.
// Implements the lifecycle.Component interface.
func (c *Component) Name() string {
	return ComponentName
}

// Start begins the component's event loop.
//
// This method blocks until the context is cancelled.
func (c *Component) Start(ctx context.Context) error {
	componentCtx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	c.ctx = componentCtx
	c.cancel = cancel
	c.mu.Unlock()

	c.logger.Debug("HTTP store starting",
		"eviction_interval", c.evictionInterval)

	// Create eviction ticker if eviction is enabled
	var evictionTicker *time.Ticker
	var evictionChan <-chan time.Time
	if c.evictionInterval > 0 {
		evictionTicker = time.NewTicker(c.evictionInterval)
		evictionChan = evictionTicker.C
		defer evictionTicker.Stop()
	}

	for {
		select {
		case event := <-c.eventChan:
			// Recover per-event so a panic in validation-event handling can't
			// tear down the goroutine (this component can't embed component.Base
			// — it adds an eviction-ticker arm).
			component.SafeDispatch(c.logger, ComponentName, event, func() {
				c.handleEvent(event)
			})

		case <-evictionChan:
			evictedURLs := c.evictUnused()
			if len(evictedURLs) > 0 {
				c.logger.Debug("HTTP store eviction ran", "evicted", len(evictedURLs))
			}

		case <-componentCtx.Done():
			c.logger.Info("HTTPStore adapter shutting down")
			c.stopAllRefreshers()
			return nil
		}
	}
}

// handleEvent processes events from the EventBus.
func (c *Component) handleEvent(event busevents.Event) {
	if e, ok := event.(*events.ProposalValidationCompletedEvent); ok {
		c.handleProposalValidationCompleted(e)
	}
}

// handleProposalValidationCompleted handles validation completion for HTTP content.
// Only processes events that match our pending validation request ID.
func (c *Component) handleProposalValidationCompleted(event *events.ProposalValidationCompletedEvent) {
	c.mu.Lock()
	batch := c.pendingValidation
	if batch == nil || event.RequestID != batch.requestID {
		c.mu.Unlock()
		return
	}
	c.pendingValidation = nil

	accepted := false
	if event.Valid {
		accepted = c.handleValidationSuccess(batch)
	} else {
		c.handleValidationFailure(batch, event.Phase, event.Error)
	}

	nextSource := c.queuedValidationSource
	c.queuedValidationSource = ""
	nextRequest := c.beginValidationLocked(nextSource)
	c.mu.Unlock()

	if accepted {
		c.eventBus.Publish(events.NewReconciliationTriggeredEvent("http_content_validated", true, events.WithNewCorrelation()))
	}
	c.publishValidationRequest(nextRequest)
}

func (c *Component) handleValidationSuccess(batch *validationBatch) bool {
	c.logger.Debug("HTTP content validation succeeded, promoting pending content",
		"url_count", len(batch.entries))

	promoted := false
	for _, entry := range batch.entries {
		if c.store.PromotePendingVersion(entry.url, entry.checksum, entry.revision) {
			promoted = true
			c.reconcileURLLocked(entry.url)
			c.eventBus.Publish(events.NewHTTPResourceAcceptedEvent(
				entry.url,
				entry.checksum,
				entry.contentSize,
			))
		}
	}
	return promoted
}

func (c *Component) handleValidationFailure(batch *validationBatch, phase, errMsg string) {
	c.logger.Warn("HTTP content validation failed, rejecting pending content",
		"url_count", len(batch.entries),
		"phase", phase,
		"error", errMsg)

	for _, entry := range batch.entries {
		if c.store.RejectPendingVersion(entry.url, entry.checksum, entry.revision) {
			c.eventBus.Publish(events.NewHTTPContentRejectedEvent(httpstore.RedactURL(entry.url)))
		}
	}
}

// GetStore returns the underlying HTTPStore.
// This is used by the wrapper to access cached content.
func (c *Component) GetStore() *httpstore.HTTPStore {
	return c.store
}

// RevisionSource identifies this component's accepted HTTP content stream.
func (c *Component) RevisionSource() httpstore.SourceID {
	return c.store.RevisionSource()
}

// Watermark returns the latest accepted HTTP semantic revision.
func (c *Component) Watermark() httpstore.Revision {
	return c.store.Watermark()
}

// ReplayWatermark returns the complete render-relevant HTTP source epoch.
func (c *Component) ReplayWatermark() httpstore.Revision {
	return c.store.ReplayWatermark()
}

// VerifyReplayEpoch verifies the complete render-relevant HTTP root.
func (c *Component) VerifyReplayEpoch(epoch *httpstore.ReplayEpoch) bool {
	return c.store.VerifyReplayEpoch(epoch)
}

// AcceptedSnapshot returns the current accepted bytes for one exact declaration.
func (c *Component) AcceptedSnapshot(
	url string,
	descriptor httpstore.SourceDescriptor,
) (httpstore.ContentSnapshot, bool) {
	snapshot := c.store.AcceptedSnapshot(url, descriptor)
	return snapshot, snapshot.Found
}

// ChangesSince returns exact accepted HTTP changes after revision.
func (c *Component) ChangesSince(revision httpstore.Revision) (
	httpstore.Revision,
	[]httpstore.SemanticChange,
	bool,
) {
	return c.store.ChangesSince(revision)
}

// NewActiveLeaseSet allocates an empty persistent render-cache lease owner.
func (c *Component) NewActiveLeaseSet() (
	*httpstore.ActiveLeaseSet,
	httpstore.ActiveLeaseToken,
	error,
) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return nil, httpstore.ActiveLeaseToken{}, errors.New("HTTP store stopped before lease allocation")
	}
	return c.store.NewActiveLeaseSet()
}

// BeginActiveLeases captures exact relevant changes for one render cache.
func (c *Component) BeginActiveLeases(
	set *httpstore.ActiveLeaseSet,
	token httpstore.ActiveLeaseToken,
) (*httpstore.ActiveLeaseSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return nil, errors.New("HTTP store stopped before lease snapshot")
	}
	return set.BeginActiveLeases(token)
}

// RetireActiveLeases removes one render cache's leases and refresh timers.
func (c *Component) RetireActiveLeases(
	set *httpstore.ActiveLeaseSet,
	token httpstore.ActiveLeaseToken,
) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	urls, err := set.RetireActiveLeases(token)
	if err != nil {
		return err
	}
	for _, url := range urls {
		if !c.store.HasActiveLease(url) {
			c.stopRefresherLocked(url)
		}
	}
	return nil
}

// VerifySnapshots checks that every accepted HTTP token is still current.
func (c *Component) VerifySnapshots(tokens []httpstore.SnapshotToken) bool {
	return c.store.VerifySnapshots(tokens)
}

// VerifyObservations checks exact present and negative HTTP reads.
func (c *Component) VerifyObservations(tokens []httpstore.ObservationToken) bool {
	return c.store.VerifyObservations(tokens)
}

func (c *Component) replayAcceptedSnapshot(
	token httpstore.SnapshotToken,
) (httpstore.ContentSnapshot, *httpstore.StagedSource, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return httpstore.ContentSnapshot{}, nil, false
	}
	return c.store.StageAcceptedSnapshot(token)
}

// AdvanceAcceptedReplayState rebases selective replay state across unrelated changes.
func (c *Component) AdvanceAcceptedReplayState(
	state *httpstore.AcceptedReplayState,
) (*httpstore.AcceptedReplayState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return nil, false
	}
	return c.store.AdvanceAcceptedReplayState(state)
}

func (c *Component) captureAcceptedReplayState(
	snapshots []httpstore.ContentSnapshot,
) (*httpstore.AcceptedReplayState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return nil, false
	}
	return c.store.CaptureAcceptedReplayState(snapshots)
}

func (c *Component) stageSource(
	url string,
	opts httpstore.FetchOptions,
	auth *httpstore.AuthConfig,
) (*httpstore.StagedSource, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return nil, errors.New("HTTP store stopped before source staging")
	}
	return c.store.StageSource(url, opts, auth)
}

func (c *Component) verifyStagedSource(source *httpstore.StagedSource) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.stopped && c.store.VerifyStagedSource(source)
}

// ReconcileSource serializes source, timer, and validation-batch authority.
func (c *Component) ReconcileSource(
	url string,
	opts httpstore.FetchOptions,
	auth *httpstore.AuthConfig,
) (httpstore.SourceState, error) {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return httpstore.SourceState{}, errors.New("HTTP store stopped before source reconciliation")
	}
	reconciled, err := c.store.ReconcileSource(url, opts, auth)
	if err != nil {
		c.mu.Unlock()
		return httpstore.SourceState{}, err
	}

	var nextRequest *events.ProposalValidationRequestedEvent
	if reconciled.Changed {
		c.stopRefresherLocked(url)
		if c.queuedValidationSource == url {
			c.queuedValidationSource = ""
		}
		if c.retireSourceValidationLocked(url) {
			nextRequest = c.beginValidationLocked("")
		}
	}
	c.reconcileURLLocked(url)
	c.mu.Unlock()

	c.publishValidationRequest(nextRequest)
	return reconciled.State, nil
}

// CommitInitialCandidates accepts one complete validated render input set.
func (c *Component) CommitInitialCandidates(
	ctx context.Context,
	candidates []*httpstore.InitialCandidate,
) error {
	_, _, err := c.CommitInitialCandidatesAndVerify(ctx, candidates, nil)
	return err
}

// CommitInitialCandidatesAndVerify accepts candidates only if all content read
// earlier in the render still has the same accepted versions.
func (c *Component) CommitInitialCandidatesAndVerify(
	ctx context.Context,
	candidates []*httpstore.InitialCandidate,
	accepted []httpstore.SnapshotToken,
) ([]httpstore.CandidateCommit, httpstore.Revision, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return nil, 0, errors.New("HTTP store stopped before validated render inputs could be accepted")
	}
	if cause := context.Cause(ctx); cause != nil {
		return nil, 0, fmt.Errorf("accepting validated render inputs: %w", cause)
	}
	commits, watermark, err := c.store.CommitInitialCandidatesAndVerify(ctx, candidates, accepted)
	if err != nil {
		return nil, 0, err
	}
	for _, candidate := range candidates {
		c.reconcileURLLocked(candidate.URL())
	}
	return commits, watermark, nil
}

// CommitInitialCandidatesAndVerifyObservations accepts candidates only if all
// exact present and negative reads still match.
func (c *Component) CommitInitialCandidatesAndVerifyObservations(
	ctx context.Context,
	candidates []*httpstore.InitialCandidate,
	observations []httpstore.ObservationToken,
) ([]httpstore.CandidateCommit, httpstore.Revision, error) {
	prepared, err := c.PrepareInitialCandidatesAndVerifyObservations(ctx, candidates, observations)
	if err != nil {
		return nil, 0, err
	}
	defer prepared.Abort()
	commits, watermark := prepared.Planned()
	if cause := context.Cause(ctx); cause != nil {
		prepared.Abort()
		return nil, 0, fmt.Errorf("committing validated render inputs: %w", cause)
	}
	prepared.Publish()
	prepared.Release()
	return commits, watermark, nil
}

// RequestRenderForAcceptedContent triggers a reconcile for content a render
// accepted without publishing its cache.
func (c *Component) RequestRenderForAcceptedContent() {
	c.eventBus.Publish(events.NewReconciliationTriggeredEvent("http_content_accepted", true, events.WithNewCorrelation()))
}

// RequestRenderForWithheldContent asks for the acceptance attempt for content
// a deploying render left out.
func (c *Component) RequestRenderForWithheldContent() {
	c.eventBus.Publish(events.NewHTTPContentAcceptanceRequestedEvent())
}

// AcceptanceSequence counts the content acceptances on this replica so far.
func (c *Component) AcceptanceSequence() uint64 {
	return c.store.AcceptanceSequence()
}

// ConfirmAcceptances settles exact source versions read by a passing render.
func (c *Component) ConfirmAcceptances(observations []httpstore.ObservationToken) {
	c.store.ConfirmAcceptances(observations)
}

// RevokeAcceptances takes back the unconfirmed acceptances up to sequence,
// because HAProxy refused a render that may have read them, and asks for the
// render without them. It returns how many it revoked.
func (c *Component) RevokeAcceptances(sequence uint64) int {
	revoked := c.store.RevokeAcceptances(sequence)
	for _, content := range revoked {
		c.ReconcileURL(content.URL)
		if content.BlobDigest != "" {
			c.eventBus.Publish(events.NewBlobContentRevokedEvent(
				httpstore.RedactURL(content.URL), content.BlobDigest, content.Restored,
			))
			continue
		}
		c.eventBus.Publish(events.NewHTTPContentRevokedEvent(
			httpstore.RedactURL(content.URL), content.Checksum, content.Restored, content.Critical,
		))
	}
	if len(revoked) > 0 {
		c.eventBus.Publish(events.NewReconciliationTriggeredEvent("http_content_revoked", true, events.WithNewCorrelation()))
	}
	return len(revoked)
}

// EnableBlobs lets blob.Fetch sources stream into sink (ADR-0041). Call it
// before the component fetches anything.
func (c *Component) EnableBlobs(sink httpstore.BlobSink) {
	c.store.SetBlobSink(sink, func(url string, err error) {
		c.eventBus.Publish(events.NewBlobFetchFailedEvent(httpstore.RedactURL(url), err.Error()))
	})
}

// BlobSources lists the digests each blob source holds.
func (c *Component) BlobSources() []httpstore.BlobSource {
	return c.store.BlobSources()
}

// RegisterURL reconciles a URL's timer with its current source policy.
func (c *Component) RegisterURL(url string) {
	c.ReconcileURL(url)
}

// ReconcileURL re-arms or stops a URL timer when its source policy changes.
func (c *Component) ReconcileURL(url string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reconcileURLLocked(url)
}

func (c *Component) reconcileURLLocked(url string) {
	state, exists := c.store.GetSourceState(url)
	c.reconcilePreparedURLLocked(url, state, exists)
}

func (c *Component) reconcilePreparedURLLocked(url string, state httpstore.SourceState, exists bool) {
	if c.stopped {
		return
	}

	if !exists || !state.HasAccepted || state.Delay <= 0 {
		c.stopRefresherLocked(url)
		return
	}
	if _, registered := c.refreshers[url]; registered &&
		c.refreshSourceGeneration[url] == state.Generation {
		return
	}
	c.stopRefresherLocked(url)

	c.logger.Debug("Registering URL for periodic refresh",
		"url", httpstore.RedactURL(url),
		"interval", state.Delay.String())

	c.refreshGeneration[url]++
	c.refreshSourceGeneration[url] = state.Generation
	c.armRefresherLocked(url, state.Delay, c.refreshGeneration[url])
}

// refreshURL performs a refresh of the given URL.
func (c *Component) refreshURL(url string) {
	c.refreshURLForGeneration(url, 0)
}

func (c *Component) armRefresherLocked(url string, delay time.Duration, generation uint64) {
	c.refreshCallbacks.Add(1)
	c.refreshManaged[url] = true
	c.refreshPending[url] = true
	c.refreshers[url] = time.AfterFunc(delay, func() {
		c.runRefresher(url, generation)
	})
}

func (c *Component) runRefresher(url string, generation uint64) {
	c.mu.Lock()
	active := !c.stopped && c.refreshManaged[url] && c.refreshGeneration[url] == generation
	if active {
		c.refreshPending[url] = false
	}
	c.mu.Unlock()
	defer c.refreshCallbacks.Done()
	if active {
		c.refreshURLForGeneration(url, generation)
	}
}

func (c *Component) refreshURLForGeneration(url string, generation uint64) {
	c.mu.Lock()
	ctx := c.ctx
	stopped := c.stopped
	timerActive := c.refresherActiveLocked(url, generation)
	sourceGeneration := uint64(0)
	if generation != 0 {
		sourceGeneration = c.refreshSourceGeneration[url]
	}
	c.mu.Unlock()
	if stopped || !timerActive || (generation != 0 && sourceGeneration == 0) {
		return
	}
	if ctx == nil {
		c.rearmRefresher(url, generation)
		return
	}
	if ctx.Err() != nil {
		return
	}

	// Defensive check: verify entry still exists (may have been evicted)
	// This handles the race condition where the eviction timer fires between
	// EvictUnused() and StopRefresher() calls.
	state, exists := c.store.GetSourceState(url)
	if !exists {
		c.logger.Log(context.Background(), logging.LevelTrace, "skipping refresh for evicted URL", "url", httpstore.RedactURL(url))
		return
	}
	if generation != 0 && state.Generation != sourceGeneration {
		return
	}

	c.logger.Log(context.Background(), logging.LevelTrace, "refreshing HTTP URL", "url", httpstore.RedactURL(url))

	// Perform refresh
	version, err := c.refreshStoreURL(ctx, url, sourceGeneration)
	if err != nil {
		c.logger.Warn("HTTP refresh failed",
			"url", httpstore.RedactURL(url),
			"error", err)
	}

	c.rearmRefresher(url, generation)

	// If content changed, trigger proposal validation before accepting
	state, sourceExists := c.store.GetSourceState(url)
	c.mu.Lock()
	active := c.refresherActiveLocked(url, generation) && (generation == 0 ||
		sourceExists && c.refreshSourceGeneration[url] == state.Generation)
	c.mu.Unlock()
	if version != nil && (!active || ctx.Err() != nil) {
		if c.store.DiscardPendingVersion(url, version.Checksum, version.Revision) {
			c.requestImmediateRefresh(url)
		}
		return
	}
	if version != nil {
		c.triggerProposalValidation(url)
	}
}

func (c *Component) rearmRefresher(url string, generation uint64) {
	state, exists := c.store.GetSourceState(url)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.refresherActiveLocked(url, generation) {
		return
	}
	if !exists || !state.HasAccepted || state.Delay <= 0 ||
		(generation != 0 && c.refreshSourceGeneration[url] != state.Generation) {
		c.stopRefresherLocked(url)
		return
	}
	delay := state.Delay
	if c.refreshImmediate[url] {
		delay = 0
		delete(c.refreshImmediate, url)
	}
	timer, exists := c.refreshers[url]
	if !exists {
		return
	}
	if generation != 0 {
		c.refreshCallbacks.Add(1)
		c.refreshPending[url] = true
	}
	timer.Reset(delay)
}

func (c *Component) requestImmediateRefresh(url string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	timer, exists := c.refreshers[url]
	if !exists || c.stopped || !c.refreshManaged[url] {
		return
	}
	if c.refreshPending[url] && timer.Stop() {
		timer.Reset(0)
		return
	}
	c.refreshImmediate[url] = true
}

func (c *Component) refresherActiveLocked(url string, generation uint64) bool {
	return !c.stopped && (generation == 0 ||
		(c.refreshManaged[url] && c.refreshGeneration[url] == generation))
}

// triggerProposalValidation publishes a ProposalValidationRequestedEvent with HTTPOverlay.
// This validates the pending HTTP content before promoting it to accepted.
func (c *Component) triggerProposalValidation(changedURL string) {
	entry := c.store.GetEntry(changedURL)
	if entry == nil || !entry.HasPending {
		return
	}

	c.logger.Debug("HTTP content changed, triggering proposal validation",
		"url", httpstore.RedactURL(changedURL),
		"new_checksum", entry.PendingChecksum[:min(16, len(entry.PendingChecksum))]+"...")

	c.mu.Lock()
	c.retireSupersededValidationLocked(changedURL, entry.PendingRevision)
	if c.pendingValidation != nil {
		c.queuedValidationSource = changedURL
	}
	req := c.beginValidationLocked(changedURL)
	c.mu.Unlock()
	c.publishValidationRequest(req)

	// Also publish HTTPResourceUpdatedEvent for observability
	c.eventBus.Publish(events.NewHTTPResourceUpdatedEvent(
		changedURL,
		entry.PendingChecksum,
		len(entry.PendingContent),
	))
}

func (c *Component) beginValidationLocked(source string) *events.ProposalValidationRequestedEvent {
	if c.pendingValidation != nil {
		return nil
	}

	overlay := httpstore.NewHTTPOverlay(c.store)
	req, batch := prepareValidationRequest(overlay, source)
	if batch == nil {
		return nil
	}
	c.pendingValidation = batch
	return req
}

func prepareValidationRequest(
	overlay *httpstore.HTTPOverlay,
	source string,
) (*events.ProposalValidationRequestedEvent, *validationBatch) {
	if overlay.IsEmpty() {
		return nil, nil
	}
	if source == "" || !overlay.HasPendingURL(source) {
		source = overlay.PendingURLs()[0]
	}
	req := events.NewProposalValidationRequestedEvent(nil, overlay, "httpstore", source)
	batch := &validationBatch{
		requestID: req.ID,
		entries:   make([]validationBatchEntry, 0, len(overlay.PendingURLs())),
	}
	for _, url := range overlay.PendingURLs() {
		pendingContent, contentExists := overlay.GetContent(url)
		pendingChecksum, pendingRevision, versionExists := overlay.PendingVersion(url)
		if !contentExists || !versionExists {
			continue
		}
		batch.entries = append(batch.entries, validationBatchEntry{
			url:         url,
			checksum:    pendingChecksum,
			revision:    pendingRevision,
			contentSize: len(pendingContent),
		})
	}
	if len(batch.entries) == 0 {
		return nil, nil
	}
	return req, batch
}

func (c *Component) retireSupersededValidationLocked(url string, revision uint64) {
	if c.pendingValidation == nil {
		return
	}
	for _, entry := range c.pendingValidation.entries {
		if entry.url == url && entry.revision != revision {
			c.pendingValidation = nil
			c.queuedValidationSource = ""
			return
		}
	}
}

func (c *Component) retireSourceValidationLocked(url string) bool {
	if c.pendingValidation == nil {
		return false
	}
	for _, entry := range c.pendingValidation.entries {
		if entry.url != url {
			continue
		}
		c.pendingValidation = nil
		c.queuedValidationSource = ""
		return true
	}
	return false
}

func (c *Component) publishValidationRequest(req *events.ProposalValidationRequestedEvent) {
	if req != nil {
		c.eventBus.Publish(req)
	}
}

// stopAllRefreshers stops all refresh timers.
func (c *Component) stopAllRefreshers() {
	c.mu.Lock()
	c.stopped = true
	cancel := c.cancel

	for url, timer := range c.refreshers {
		c.refreshGeneration[url]++
		if timer.Stop() && c.refreshManaged[url] && c.refreshPending[url] {
			c.refreshPending[url] = false
			c.refreshCallbacks.Done()
		}
		c.logger.Log(context.Background(), logging.LevelTrace, "stopped refresh timer", "url", httpstore.RedactURL(url))
	}

	c.refreshers = make(map[string]*time.Timer)
	c.refreshManaged = make(map[string]bool)
	c.refreshPending = make(map[string]bool)
	c.refreshImmediate = make(map[string]bool)
	c.refreshSourceGeneration = make(map[string]uint64)
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	c.refreshCallbacks.Wait()
	c.stopOutages()
}

func (c *Component) evictUnused() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	evictedURLs := c.evictStoreUnused()
	for _, url := range evictedURLs {
		c.stopRefresherLocked(url)
	}
	return evictedURLs
}

// StopRefresher stops the refresh timer for a specific URL.
func (c *Component) StopRefresher(url string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopRefresherLocked(url)
}

func (c *Component) stopRefresherLocked(url string) {
	if timer, exists := c.refreshers[url]; exists {
		c.refreshGeneration[url]++
		if timer.Stop() && c.refreshManaged[url] && c.refreshPending[url] {
			c.refreshPending[url] = false
			c.refreshCallbacks.Done()
		}
		delete(c.refreshers, url)
		delete(c.refreshManaged, url)
		delete(c.refreshPending, url)
		delete(c.refreshImmediate, url)
		delete(c.refreshSourceGeneration, url)
		c.logger.Log(context.Background(), logging.LevelTrace, "stopped refresh timer", "url", httpstore.RedactURL(url))
	}
}
