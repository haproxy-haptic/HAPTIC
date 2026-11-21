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

package httpstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// HTTPStore provides HTTP resource fetching with caching and two-version validation.
//
// The store supports:
//   - Synchronous initial fetch (blocks until complete)
//   - Cached access to previously fetched content
//   - Two-version cache for safe validation (pending vs accepted)
//   - Conditional requests using ETag/If-Modified-Since
//   - Automatic eviction of unused entries based on last access time
//
// Thread-safe for concurrent access.
type HTTPStore struct {
	mu                      sync.RWMutex
	prepareAuthority        chan struct{}
	publicationPoisoned     atomic.Bool
	cache                   map[string]*CacheEntry // URL -> CacheEntry
	nextPendingRevision     uint64
	nextCandidateRevision   Revision
	nextSourceGeneration    uint64
	revisionSource          SourceID
	semanticRevision        Revision
	replayRevision          Revision
	replayJournal           []ReplayChange
	replayJournalStart      int
	replayJournalCapacity   int
	semanticJournal         []SemanticChange
	semanticJournalStart    int
	semanticJournalCapacity int
	nextActiveLeaseSet      uint64
	activeLeaseSets         map[uint64]*activeLeaseState
	activeLeaseURLs         map[string]map[uint64]SourceDescriptor
	acceptanceSequence      uint64
	acceptances             map[string]*acceptance
	httpClient              *http.Client
	blobs                   atomic.Pointer[blobTarget]
	logger                  *slog.Logger
	maxAge                  time.Duration // Maximum time an entry can remain unused before eviction (0 = disabled)

	// validationStuckAfter bounds how long an entry may sit in StateValidating.
	// Only PromotePending/RejectPending leave that state, and both are driven by
	// a ProposalValidationCompletedEvent — so a lost event, or a panic in the
	// validator, would otherwise freeze the URL at its accepted content for the
	// process lifetime. Overridden in tests.
	validationStuckAfter time.Duration
}

var errHTTPStorePublicationPoisoned = errors.New("HTTP store is unavailable after publication authentication failure")

// ErrInputsMoved reports that another render accepted content the failing
// render had read, so it was composed against inputs that no longer hold.
// Rendering again reads the accepted content; refusing leaves the render
// without a trigger, because acceptance raises no watch event.
var ErrInputsMoved = errors.New("changed while the render was running")

// ErrCriticalFetch marks a critical source whose fetch failed, as opposed to a
// render that must retry because its source authority moved.
var ErrCriticalFetch = errors.New("critical HTTP fetch failed")

func (s *HTTPStore) publicationErrorLocked() error {
	if s == nil || s.publicationPoisoned.Load() {
		return errHTTPStorePublicationPoisoned
	}
	return nil
}

func (s *HTTPStore) quarantinePublicationLocked() {
	s.publicationPoisoned.Store(true)
	s.cache = nil
	s.nextPendingRevision = ^uint64(0)
	s.nextCandidateRevision = Revision(^uint64(0))
	s.nextSourceGeneration = ^uint64(0)
	s.revisionSource = 0
	s.semanticRevision = 0
	s.replayRevision = 0
	s.replayJournal = nil
	s.replayJournalStart = 0
	s.semanticJournal = nil
	s.semanticJournalStart = 0
	s.nextActiveLeaseSet = ^uint64(0)
	s.activeLeaseSets = nil
	s.activeLeaseURLs = nil
	s.acceptances = nil
}

// DefaultValidationStuckAfter bounds a pending validation. Render plus
// three-phase HAProxy validation takes seconds; an entry still validating
// minutes later is waiting for a verdict that is never coming.
const DefaultValidationStuckAfter = 5 * time.Minute

// New creates a new HTTPStore with the given logger and maximum cache age.
//
// maxAge is the maximum time an entry can remain unused before becoming eligible
// for eviction. If maxAge is 0, entries are never evicted based on access time.
func New(logger *slog.Logger, maxAge time.Duration) *HTTPStore {
	if logger == nil {
		logger = slog.Default()
	}

	store := &HTTPStore{
		cache:                   make(map[string]*CacheEntry),
		prepareAuthority:        make(chan struct{}, 1),
		revisionSource:          allocateHTTPStoreSource(),
		replayJournalCapacity:   defaultReplayJournalCapacity,
		semanticJournalCapacity: defaultSemanticJournalCapacity,
		activeLeaseSets:         make(map[uint64]*activeLeaseState),
		activeLeaseURLs:         make(map[string]map[uint64]SourceDescriptor),
		httpClient: &http.Client{
			Timeout:       DefaultTimeout,
			CheckRedirect: checkRedirect,
		},
		logger:               logger.With("component", "httpstore"),
		maxAge:               maxAge,
		validationStuckAfter: DefaultValidationStuckAfter,
	}
	store.prepareAuthority <- struct{}{}
	return store
}

// redirectSafeHeaders survive a cross-host redirect. Everything else is dropped:
// net/http only strips Authorization when the host changes, which leaves
// AuthTypeHeader's API keys (and any header a future caller adds) in place.
var redirectSafeHeaders = map[string]bool{
	"User-Agent":        true,
	"Referer":           true,
	"If-None-Match":     true,
	"If-Modified-Since": true,
	"Accept-Encoding":   true,
}

// checkRedirect follows redirects but refuses to downgrade an https fetch to
// plaintext, and drops credentials when the host changes. Fetched bodies become
// HAProxy config and WAF rules, so a plaintext hop is a config-injection path.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("too many redirects")
	}
	// via[0] is the URL the operator configured: an http:// source was never
	// confidential, so only an https:// origin gets downgrade protection.
	if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect from https to %s (%s): plaintext hop can rewrite the fetched content",
			req.URL.Scheme, RedactURL(req.URL.String()))
	}
	if req.URL.Host != via[0].URL.Host {
		for name := range req.Header {
			if !redirectSafeHeaders[name] {
				req.Header.Del(name)
			}
		}
	}
	return nil
}

// Fetch retrieves content from a URL, using cache if available.
//
// On first call for a source declaration, this performs a synchronous HTTP fetch and caches the result.
// Subsequent calls with the same effective options and authentication return cached content immediately.
//
// If the URL has a Delay > 0 in options, the caller is responsible for scheduling
// refreshes (typically done by the event adapter component).
//
// Parameters:
//   - ctx: Context for cancellation and timeout
//   - url: The HTTP(S) URL to fetch
//   - opts: Fetch options (timeout, retries, critical flag, delay for refresh)
//   - auth: Optional authentication configuration
//
// Returns:
//   - Content string (empty if fetch failed and not critical)
//   - Error if critical fetch fails
func (s *HTTPStore) Fetch(ctx context.Context, url string, opts FetchOptions, auth *AuthConfig) (string, error) {
	reconciled, err := s.ReconcileSource(url, opts, auth)
	if err != nil {
		return "", err
	}
	state := reconciled.State
	if snapshot := s.AcceptedSnapshot(url, state.Descriptor); snapshot.Found {
		return snapshot.Content, nil
	}

	snapshot, err := s.initialFetchSnapshot(url, state)
	if err != nil {
		return "", err
	}
	s.logger.Info("Performing initial HTTP fetch",
		"url", RedactURL(url),
		"timeout", snapshot.options.Timeout.String(),
		"retries", snapshot.options.Retries,
		"critical", snapshot.options.Critical)

	content, etag, lastModified, err := s.fetchWithRetry(
		ctx, url, snapshot.options, snapshot.auth, "", "",
	)
	if !s.sourceCurrent(url, &snapshot) {
		return "", fmt.Errorf("HTTP source %s changed while it was being fetched; retry the render", RedactURL(url))
	}
	if err != nil {
		if snapshot.options.Critical {
			return "", fmt.Errorf("%w for %s: %w", ErrCriticalFetch, RedactURL(url), err)
		}
		s.logger.Warn("HTTP fetch failed, returning empty content",
			"url", RedactURL(url),
			"error", err)
		return "", nil
	}

	checksum := checksum(content)
	now := time.Now()
	s.mu.Lock()
	entry, exists := s.cache[url]
	if !exists || !sameCacheEntryVersion(entry, snapshot.entry) || entry.sourceIdentity != snapshot.sourceIdentity ||
		entry.sourceDescriptor != snapshot.sourceDescriptor ||
		entry.sourceGeneration != snapshot.sourceGeneration ||
		entry.mutationRevision != snapshot.mutationRevision || entry.HasPending {
		s.mu.Unlock()
		return "", fmt.Errorf("HTTP source %s changed while it was being fetched; retry the render", RedactURL(url))
	}
	if entry.AcceptedChecksum != "" {
		content = entry.AcceptedContent
		s.mu.Unlock()
		return content, nil
	}
	entry.AcceptedContent = content
	entry.AcceptedChecksum = checksum
	entry.AcceptedTime = now
	entry.LastAccessTime = now
	entry.ValidationState = StateAccepted
	entry.ETag = etag
	entry.LastModified = lastModified
	entry.mutationRevision++
	entry.acceptedRevision = s.recordSemanticChangeLocked(
		url,
		entry.sourceDescriptor,
		entry.sourceDescriptor,
		false,
	)
	s.mu.Unlock()

	s.logger.Debug("Cached HTTP content",
		"url", RedactURL(url),
		"size", len(content),
		"checksum", checksum[:16]+"...")

	return content, nil
}

// ReconcileSource makes one URL's effective fetch authority current.
func (s *HTTPStore) ReconcileSource(url string, opts FetchOptions, auth *AuthConfig) (SourceReconcileResult, error) {
	spec, err := normalizeSource(opts, auth)
	if err != nil {
		return SourceReconcileResult{}, err
	}

	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.publicationErrorLocked(); err != nil {
		return SourceReconcileResult{}, err
	}

	entry, exists := s.cache[url]
	if exists && entry.sourceDescriptor == spec.descriptor {
		entry.LastAccessTime = now
		return SourceReconcileResult{State: sourceState(entry)}, nil
	}

	if s.nextSourceGeneration == ^uint64(0) {
		panic("HTTP store source generation exhausted")
	}
	s.nextSourceGeneration++
	generation := s.nextSourceGeneration
	previousSourceDescriptor := SourceDescriptor{}
	if exists {
		previousSourceDescriptor = entry.sourceDescriptor
	}
	if !exists || !entry.fixture {
		s.cache[url] = &CacheEntry{
			URL:              url,
			LastAccessTime:   now,
			ValidationState:  StateAccepted,
			Options:          spec.options,
			Auth:             spec.auth,
			sourceIdentity:   spec.descriptor.Identity(),
			sourceDescriptor: spec.descriptor,
			sourceGeneration: generation,
		}
		s.recordSemanticChangeLocked(url, previousSourceDescriptor, spec.descriptor, false)
		return SourceReconcileResult{
			State:   sourceState(s.cache[url]),
			Changed: true,
		}, nil
	}

	entry.PendingContent = ""
	entry.PendingChecksum = ""
	entry.PendingRevision = 0
	entry.HasPending = false
	entry.ValidationState = StateAccepted
	entry.ValidationStartedAt = time.Time{}
	entry.Options = spec.options
	entry.Auth = spec.auth
	entry.sourceIdentity = spec.descriptor.Identity()
	entry.sourceDescriptor = spec.descriptor
	entry.sourceGeneration = generation
	entry.LastAccessTime = now
	entry.fixture = false
	entry.mutationRevision++
	entry.acceptedRevision = s.recordSemanticChangeLocked(
		url,
		previousSourceDescriptor,
		spec.descriptor,
		false,
	)
	return SourceReconcileResult{
		State:   sourceState(entry),
		Changed: true,
	}, nil
}

type initialFetchSnapshot struct {
	entry            *CacheEntry
	options          FetchOptions
	auth             *AuthConfig
	sourceIdentity   string
	sourceDescriptor SourceDescriptor
	sourceGeneration uint64
	mutationRevision uint64
}

func (s *HTTPStore) initialFetchSnapshot(url string, state SourceState) (initialFetchSnapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.publicationErrorLocked(); err != nil {
		return initialFetchSnapshot{}, err
	}

	entry, exists := s.cache[url]
	if !exists || entry.sourceDescriptor != state.Descriptor || entry.sourceGeneration != state.Generation {
		return initialFetchSnapshot{}, fmt.Errorf("HTTP source %s changed before it could be fetched; retry the render", RedactURL(url))
	}
	return initialFetchSnapshot{
		entry:            entry,
		options:          entry.Options,
		auth:             entry.Auth,
		sourceIdentity:   entry.sourceIdentity,
		sourceDescriptor: entry.sourceDescriptor,
		sourceGeneration: entry.sourceGeneration,
		mutationRevision: entry.mutationRevision,
	}, nil
}

func (s *HTTPStore) sourceCurrent(url string, snapshot *initialFetchSnapshot) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.publicationErrorLocked() != nil {
		return false
	}

	entry, exists := s.cache[url]
	return exists && sameCacheEntryVersion(entry, snapshot.entry) && entry.sourceIdentity == snapshot.sourceIdentity &&
		entry.sourceDescriptor == snapshot.sourceDescriptor &&
		entry.sourceGeneration == snapshot.sourceGeneration &&
		entry.mutationRevision == snapshot.mutationRevision
}

func sourceState(entry *CacheEntry) SourceState {
	return SourceState{
		Identity:    entry.sourceIdentity,
		Descriptor:  entry.sourceDescriptor,
		Generation:  entry.sourceGeneration,
		Delay:       entry.Options.Delay,
		HasAccepted: entry.AcceptedChecksum != "",
	}
}

// Get returns the accepted content for a URL if it exists in cache.
// Returns empty string and false if not cached.
// Updates LastAccessTime to track usage for cache eviction.
func (s *HTTPStore) Get(url string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.publicationErrorLocked() != nil {
		return "", false
	}

	entry, exists := s.cache[url]
	if !exists || entry.AcceptedChecksum == "" {
		return "", false
	}
	entry.LastAccessTime = time.Now()
	return entry.AcceptedContent, true
}

// GetSource returns accepted content only when it belongs to source.
func (s *HTTPStore) GetSource(url string, source SourceDescriptor) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.publicationErrorLocked() != nil {
		return "", false
	}

	entry, exists := s.cache[url]
	if !exists || entry.sourceDescriptor != source || entry.AcceptedChecksum == "" {
		return "", false
	}
	entry.LastAccessTime = time.Now()
	return entry.AcceptedContent, true
}

// GetForValidation returns content for validation rendering.
// If pending content exists, returns pending; otherwise returns accepted.
// Updates LastAccessTime to track usage for cache eviction.
func (s *HTTPStore) GetForValidation(url string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.publicationErrorLocked() != nil {
		return "", false
	}

	entry, exists := s.cache[url]
	if !exists {
		return "", false
	}

	entry.LastAccessTime = time.Now()

	if entry.HasPending {
		return entry.PendingContent, true
	}
	if entry.AcceptedChecksum != "" {
		return entry.AcceptedContent, true
	}
	return "", false
}

// abandonStuckValidation reports whether the caller should proceed with a
// refresh over an entry that is still validating. It returns false while the
// verdict may legitimately still arrive; past the deadline it discards the
// pending content and returns true.
//
// Without this the entry never leaves StateValidating: only a verdict clears
// that state, every later refresh short-circuits on it, and eviction skips
// entries with pending content — so one lost verdict freezes the URL at its
// accepted content for the process lifetime.
func (s *HTTPStore) abandonStuckValidation(url string, stuckFor time.Duration) bool {
	if stuckFor <= s.validationStuckAfter {
		s.logger.Log(context.Background(), levelTrace, "skipping refresh, validation in progress", "url", RedactURL(url))
		return false
	}

	s.logger.Warn("Abandoning stuck HTTP content validation, no verdict arrived",
		"url", RedactURL(url),
		"stuck_for", stuckFor.Round(time.Second),
		"timeout", s.validationStuckAfter)
	s.RejectPending(url)
	return true
}

// RefreshURL fetches fresh content for a URL and stores it as pending.
//
// This does NOT replace accepted content immediately. The caller must:
// 1. Trigger re-render with pending content (using GetForValidation)
// 2. On successful validation, call PromotePending
// 3. On failed validation, call RejectPending
//
// Returns:
//   - changed: true if content changed from accepted version
//   - err: fetch error (nil if successful or 304 Not Modified)
func (s *HTTPStore) RefreshURL(ctx context.Context, url string) (changed bool, err error) {
	outcome, err := s.refreshURL(ctx, url, 0)
	return outcome.changed, err
}

// RefreshURLVersion refreshes a URL and returns the exact pending version it created.
func (s *HTTPStore) RefreshURLVersion(ctx context.Context, url string) (*PendingVersion, error) {
	outcome, err := s.refreshURL(ctx, url, 0)
	return outcome.pendingVersion(), err
}

// RefreshURLVersionForGeneration refreshes only the matching source generation.
func (s *HTTPStore) RefreshURLVersionForGeneration(
	ctx context.Context,
	url string,
	sourceGeneration uint64,
) (*PendingVersion, error) {
	outcome, err := s.refreshURL(ctx, url, sourceGeneration)
	return outcome.pendingVersion(), err
}

type refreshSnapshot struct {
	entry            *CacheEntry
	mutationRevision uint64
	sourceDescriptor SourceDescriptor
	sourceGeneration uint64
	options          FetchOptions
	auth             *AuthConfig
	etag             string
	lastModified     string
	acceptedContent  string
	acceptedChecksum string
}

type refreshOutcome struct {
	version PendingVersion
	changed bool
}

func (o refreshOutcome) pendingVersion() *PendingVersion {
	if !o.changed {
		return nil
	}
	return &o.version
}

func (s *HTTPStore) refreshSnapshot(url string, sourceGeneration uint64) (refreshSnapshot, bool, error) {
	s.mu.RLock()
	if err := s.publicationErrorLocked(); err != nil {
		s.mu.RUnlock()
		return refreshSnapshot{}, false, err
	}
	entry, exists := s.cache[url]
	if !exists {
		s.mu.RUnlock()
		return refreshSnapshot{}, false, fmt.Errorf("URL not in cache: %s", RedactURL(url))
	}
	if sourceGeneration != 0 && entry.sourceGeneration != sourceGeneration {
		s.mu.RUnlock()
		return refreshSnapshot{}, false, nil
	}

	if entry.ValidationState == StateValidating {
		stuckFor := time.Since(entry.ValidationStartedAt)
		s.mu.RUnlock()

		if !s.abandonStuckValidation(url, stuckFor) {
			return refreshSnapshot{}, false, nil
		}

		s.mu.RLock()
		entry, exists = s.cache[url]
		if !exists {
			s.mu.RUnlock()
			return refreshSnapshot{}, false, fmt.Errorf("URL not in cache: %s", RedactURL(url))
		}
		if sourceGeneration != 0 && entry.sourceGeneration != sourceGeneration {
			s.mu.RUnlock()
			return refreshSnapshot{}, false, nil
		}
	}

	snapshot := refreshSnapshot{
		entry:            entry,
		mutationRevision: entry.mutationRevision,
		sourceDescriptor: entry.sourceDescriptor,
		sourceGeneration: entry.sourceGeneration,
		options:          entry.Options,
		auth:             entry.Auth,
		etag:             entry.ETag,
		lastModified:     entry.LastModified,
		acceptedContent:  entry.AcceptedContent,
		acceptedChecksum: entry.AcceptedChecksum,
	}
	s.mu.RUnlock()
	return snapshot, true, nil
}

func (s *HTTPStore) updateRefreshMetadata(url string, snapshot *refreshSnapshot, etag, lastModified string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, exists := s.cache[url]
	if !exists || !sameCacheEntryVersion(entry, snapshot.entry) || entry.mutationRevision != snapshot.mutationRevision ||
		entry.sourceDescriptor != snapshot.sourceDescriptor || entry.sourceGeneration != snapshot.sourceGeneration ||
		entry.HasPending || entry.AcceptedChecksum != snapshot.acceptedChecksum {
		return
	}
	entry.ETag = etag
	entry.LastModified = lastModified
	entry.mutationRevision++
}

func (s *HTTPStore) commitPendingRefresh(
	url, content, newChecksum, etag, lastModified string,
	snapshot *refreshSnapshot,
) (PendingVersion, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, exists := s.cache[url]
	if !exists || !sameCacheEntryVersion(entry, snapshot.entry) || entry.mutationRevision != snapshot.mutationRevision ||
		entry.sourceDescriptor != snapshot.sourceDescriptor || entry.sourceGeneration != snapshot.sourceGeneration ||
		entry.HasPending || entry.AcceptedChecksum != snapshot.acceptedChecksum {
		return PendingVersion{}, false
	}

	if s.nextPendingRevision == ^uint64(0) {
		panic("HTTP store pending revision exhausted")
	}
	s.nextPendingRevision++
	version := PendingVersion{Checksum: newChecksum, Revision: s.nextPendingRevision}
	entry.PendingContent = content
	entry.PendingChecksum = version.Checksum
	entry.PendingRevision = version.Revision
	entry.HasPending = true
	entry.ValidationState = StateValidating
	entry.ValidationStartedAt = time.Now()
	entry.ETag = etag
	entry.LastModified = lastModified
	entry.mutationRevision++
	entry.replayRevision++
	s.recordReplayChangeLocked(url)
	s.recordActiveLeaseChangeLocked(url, entry.sourceDescriptor, entry.sourceDescriptor)
	return version, true
}

func (s *HTTPStore) refreshURL(
	ctx context.Context,
	url string,
	sourceGeneration uint64,
) (refreshOutcome, error) {
	snapshot, ready, err := s.refreshSnapshot(url, sourceGeneration)
	if err != nil || !ready {
		return refreshOutcome{}, err
	}

	content, newEtag, newLastModified, err := s.fetchWithRetry(
		ctx,
		url,
		snapshot.options,
		snapshot.auth,
		snapshot.etag,
		snapshot.lastModified,
	)
	if err != nil {
		if errors.Is(err, errNotModified) {
			s.logger.Log(context.Background(), levelTrace, "content not modified (304)",
				"url", RedactURL(url),
				"etag", snapshot.etag)
			return refreshOutcome{}, nil
		}
		s.logger.Warn("Refresh fetch failed",
			"url", RedactURL(url),
			"error", err)
		return refreshOutcome{}, err
	}

	newChecksum := checksum(content)
	if content == snapshot.acceptedContent {
		s.logger.Log(context.Background(), levelTrace, "content unchanged",
			"url", RedactURL(url),
			"checksum", newChecksum[:16]+"...")
		s.updateRefreshMetadata(url, &snapshot, newEtag, newLastModified)
		return refreshOutcome{}, nil
	}

	version, committed := s.commitPendingRefresh(url, content, newChecksum, newEtag, newLastModified, &snapshot)
	if !committed {
		return refreshOutcome{}, nil
	}

	s.logger.Debug("Content changed, stored as pending",
		"url", RedactURL(url),
		"old_checksum", snapshot.acceptedChecksum[:min(16, len(snapshot.acceptedChecksum))]+"...",
		"new_checksum", newChecksum[:16]+"...",
		"new_size", len(content))
	return refreshOutcome{version: version, changed: true}, nil
}

// GetDelay returns the configured delay for a URL.
// Returns 0 if URL not in cache or no delay configured.
func (s *HTTPStore) GetDelay(url string) time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.publicationErrorLocked() != nil {
		return 0
	}

	if entry, exists := s.cache[url]; exists {
		return entry.Options.Delay
	}
	return 0
}

// GetSourceState returns the current authority and timer policy for a URL.
func (s *HTTPStore) GetSourceState(url string) (SourceState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.publicationErrorLocked() != nil {
		return SourceState{}, false
	}

	entry, exists := s.cache[url]
	if !exists {
		return SourceState{}, false
	}
	return sourceState(entry), true
}

// GetEntry returns a copy of the cache entry for a URL.
// Returns nil if not cached.
func (s *HTTPStore) GetEntry(url string) *CacheEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.publicationErrorLocked() != nil {
		return nil
	}

	entry, exists := s.cache[url]
	if !exists {
		return nil
	}

	// Return a copy to prevent external modification
	entryCopy := *entry
	if entry.Auth != nil {
		authCopy := *entry.Auth
		if entry.Auth.Headers != nil {
			authCopy.Headers = make(map[string]string, len(entry.Auth.Headers))
			maps.Copy(authCopy.Headers, entry.Auth.Headers)
		}
		entryCopy.Auth = &authCopy
	}
	return &entryCopy
}

// LoadFixture loads a single HTTP fixture directly into the store as accepted content.
// This is used by validation tests to provide mock HTTP responses without making
// actual HTTP requests.
//
// The fixture is stored directly as accepted content, bypassing the normal
// fetch and validation workflow.
func (s *HTTPStore) LoadFixture(url, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.publicationErrorLocked(); err != nil {
		panic(err)
	}

	previous, exists := s.cache[url]
	if exists && previous.fixture && previous.sourceDescriptor == (SourceDescriptor{}) &&
		previous.AcceptedChecksum != "" && previous.AcceptedContent == content {
		replayChanged := previous.PendingContent != "" || previous.PendingChecksum != "" ||
			previous.PendingRevision != 0 || previous.HasPending ||
			previous.ValidationState != StateAccepted || !previous.ValidationStartedAt.IsZero()
		previous.LastAccessTime = time.Now()
		previous.PendingContent = ""
		previous.PendingChecksum = ""
		previous.PendingRevision = 0
		previous.HasPending = false
		previous.ValidationState = StateAccepted
		previous.ValidationStartedAt = time.Time{}
		previous.mutationRevision++
		if replayChanged {
			previous.replayRevision++
			s.recordReplayChangeLocked(url)
			s.recordActiveLeaseChangeLocked(url, previous.sourceDescriptor, previous.sourceDescriptor)
		}
		return
	}
	previousSourceDescriptor := SourceDescriptor{}
	if exists {
		previousSourceDescriptor = previous.sourceDescriptor
	}
	checksum := checksum(content)
	now := time.Now()
	if s.nextSourceGeneration == ^uint64(0) {
		panic("HTTP store source generation exhausted")
	}
	s.nextSourceGeneration++
	entry := &CacheEntry{
		URL:              url,
		AcceptedContent:  content,
		AcceptedChecksum: checksum,
		AcceptedTime:     now,
		LastAccessTime:   now,
		ValidationState:  StateAccepted,
		sourceGeneration: s.nextSourceGeneration,
		fixture:          true,
		// No pending content, no ETag - fixtures are immediately accepted
	}
	s.cache[url] = entry
	entry.acceptedRevision = s.recordSemanticChangeLocked(
		url,
		previousSourceDescriptor,
		SourceDescriptor{},
		false,
	)

	s.logger.Debug("Loaded HTTP fixture",
		"url", RedactURL(url),
		"size", len(content),
		"checksum", checksum[:min(16, len(checksum))]+"...")
}
