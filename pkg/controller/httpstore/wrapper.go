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
	"math"
	"reflect"
	"slices"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/haproxy-haptic/HAPTIC/pkg/httpstore"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

// SourceMode controls whether a render may replace the shared HTTP source.
type SourceMode uint8

const (
	// SourceModeReadOnly keeps source declarations and fetched content local to one render.
	SourceModeReadOnly SourceMode = iota
	// SourceModeAuthoritative reconciles accepted sources and their refresh timers.
	SourceModeAuthoritative
)

// HTTPStoreWrapper exposes HTTPStore content to one template render.
type HTTPStoreWrapper struct {
	component      *Component
	logger         *slog.Logger
	overlay        stores.HTTPContentOverlay
	ctx            context.Context
	sourceMode     SourceMode
	transientStore *httpstore.HTTPStore
	transaction    *InputTransaction
	readOnlyGroup  singleflight.Group
	mu             sync.Mutex
	declared       map[string]httpstore.SourceDescriptor
	snapshots      map[string]httpstore.ContentSnapshot
	renderFailure  error
}

// NewIsolatedHTTPStoreWrapper fetches into a render-local cache without refresh timers.
func NewIsolatedHTTPStoreWrapper(ctx context.Context, logger *slog.Logger) *HTTPStoreWrapper {
	overlay := httpstore.NewAcceptedHTTPOverlay(httpstore.New(logger, 0))
	return NewHTTPStoreWrapper(ctx, nil, logger, overlay, SourceModeReadOnly)
}

// NewHTTPStoreWrapper creates a new HTTPStoreWrapper.
func NewHTTPStoreWrapper(
	ctx context.Context,
	component *Component,
	logger *slog.Logger,
	overlay stores.HTTPContentOverlay,
	sourceMode SourceMode,
) *HTTPStoreWrapper {
	return NewHTTPStoreWrapperWithRetrySeed(ctx, component, logger, overlay, sourceMode, nil)
}

// NewHTTPStoreWrapperWithRetrySeed creates a wrapper with detached retry inputs.
func NewHTTPStoreWrapperWithRetrySeed(
	ctx context.Context,
	component *Component,
	logger *slog.Logger,
	overlay stores.HTTPContentOverlay,
	sourceMode SourceMode,
	retrySeed *InputRetrySeed,
) *HTTPStoreWrapper {
	if sourceMode == SourceModeReadOnly && overlay == nil {
		overlay = httpstore.NewAcceptedHTTPOverlay(component.GetStore())
	}
	wrapper := &HTTPStoreWrapper{
		component:      component,
		logger:         logger.With("component", "http-wrapper"),
		overlay:        overlay,
		ctx:            ctx,
		sourceMode:     sourceMode,
		transientStore: httpstore.New(logger, 0),
		declared:       make(map[string]httpstore.SourceDescriptor),
		snapshots:      make(map[string]httpstore.ContentSnapshot),
	}
	if sourceMode == SourceModeAuthoritative {
		deferred := deferredCandidatesFromContext(ctx)
		seed := deferred.retrySeed(retrySeed)
		if !CandidatesWithheld(ctx) {
			seed = component.withRecoveredInputs(seed)
		}
		wrapper.transaction = newInputTransaction(component, seed)
		wrapper.transaction.deferredCandidates = deferred
		wrapper.transaction.withholdCandidates = CandidatesWithheld(ctx)
	}
	return wrapper
}

// ErrCandidateWithheld reports a critical source that a render left out
// because its content was not accepted yet.
var ErrCandidateWithheld = errors.New("HTTP content is not accepted yet and this render does not accept new content")

type withheldCandidatesKey struct{}

// WithCandidatesWithheld marks a render that must not accept new HTTP
// content: unaccepted sources render as unavailable and are not fetched.
func WithCandidatesWithheld(ctx context.Context) context.Context {
	return context.WithValue(ctx, withheldCandidatesKey{}, true)
}

// CandidatesWithheld reports whether the render must not accept new HTTP content.
func CandidatesWithheld(ctx context.Context) bool {
	withheld, _ := ctx.Value(withheldCandidatesKey{}).(bool)
	return withheld
}

// Pending reports whether this render left the URL out because its content
// waits for a render that can accept it.
func (w *HTTPStoreWrapper) Pending(url string) bool {
	return w.CandidateWithheld(url)
}

// CandidateWithheld reports whether this render left the source out because
// its content was not accepted yet.
func (w *HTTPStoreWrapper) CandidateWithheld(url string) bool {
	return w != nil && w.transaction != nil && w.transaction.Withheld(url)
}

// InputTransaction returns the authoritative render's candidate transaction.
func (w *HTTPStoreWrapper) InputTransaction() *InputTransaction {
	return w.transaction
}

// RevisionSource identifies the accepted content stream observed by this wrapper.
func (w *HTTPStoreWrapper) RevisionSource() httpstore.SourceID {
	if w == nil || w.component == nil {
		return 0
	}
	return w.component.RevisionSource()
}

// ReplaySnapshot pins a cached accepted dependency without performing network I/O.
func (w *HTTPStoreWrapper) ReplaySnapshot(
	snapshot *httpstore.ContentSnapshot,
) (httpstore.ContentSnapshot, bool, error) {
	if snapshot == nil || !snapshot.Found || !snapshot.Cacheable || !snapshot.Token.Valid() ||
		snapshot.Token.Kind() != httpstore.SnapshotAccepted ||
		snapshot.URL != snapshot.Token.URL() || snapshot.Descriptor != snapshot.Token.SourceDescriptor() {
		return httpstore.ContentSnapshot{}, false,
			errors.New("HTTP snapshot replay requires an exact accepted snapshot")
	}
	if err := w.declare(snapshot.URL, snapshot.Descriptor); err != nil {
		return httpstore.ContentSnapshot{}, false, err
	}
	if current, observed := w.observedSnapshot(snapshot); observed {
		return current, true, nil
	}
	if w.sourceMode == SourceModeReadOnly {
		current, found, err := w.getCachedSnapshot(snapshot.URL, snapshot.Descriptor)
		if err != nil || !found {
			return current, false, err
		}
		current = w.recordSnapshot(&current)
		return current, true, nil
	}
	if w.transaction == nil || w.sourceMode != SourceModeAuthoritative {
		return httpstore.ContentSnapshot{}, false,
			errors.New("HTTP snapshot replay requires a supported source mode")
	}
	current, source, ok := w.component.replayAcceptedSnapshot(snapshot.Token)
	if !ok {
		return httpstore.ContentSnapshot{
			URL:        snapshot.URL,
			Descriptor: snapshot.Descriptor,
		}, false, nil
	}
	if err := w.transaction.replay(&current, source); err != nil {
		return httpstore.ContentSnapshot{}, false, err
	}
	return current, true, nil
}

// CaptureAcceptedReplayState binds every accepted read to one selective cursor.
func (w *HTTPStoreWrapper) CaptureAcceptedReplayState(
	snapshots []httpstore.ContentSnapshot,
) (*httpstore.AcceptedReplayState, bool) {
	if w == nil || w.component == nil {
		return nil, false
	}
	return w.component.captureAcceptedReplayState(snapshots)
}

// RequireAcceptedReplayState adds selective accepted HTTP inputs to the end fence.
func (w *HTTPStoreWrapper) RequireAcceptedReplayState(
	state *httpstore.AcceptedReplayState,
) (*httpstore.AcceptedReplayState, error) {
	if w == nil || w.transaction == nil || w.sourceMode != SourceModeAuthoritative {
		return nil, errors.New("accepted HTTP replay state requires authoritative source mode")
	}
	if w.component == nil {
		return nil, errors.New("accepted HTTP replay state has no source store")
	}
	advanced, ok := w.component.AdvanceAcceptedReplayState(state)
	if !ok {
		return nil, errors.New("accepted HTTP replay inputs changed")
	}
	if err := w.transaction.requireAcceptedReplayState(advanced); err != nil {
		return nil, err
	}
	return advanced, nil
}

func (w *HTTPStoreWrapper) observedSnapshot(
	expected *httpstore.ContentSnapshot,
) (httpstore.ContentSnapshot, bool) {
	if w.transaction != nil {
		return w.transaction.observedSnapshot(expected)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	current, exists := w.snapshots[expected.URL]
	if !exists || !sameObservedHTTPSnapshot(&current, expected) {
		return httpstore.ContentSnapshot{}, false
	}
	return current, true
}

func sameObservedHTTPSnapshot(left, right *httpstore.ContentSnapshot) bool {
	if left == nil || right == nil {
		return left == right
	}
	// Unrelated accepted changes may advance the store watermark without changing this read.
	return left.URL == right.URL && left.Descriptor == right.Descriptor &&
		left.Content == right.Content && left.Found == right.Found &&
		left.Cacheable == right.Cacheable && left.Token == right.Token &&
		left.StoreSource == right.StoreSource && left.Observation == right.Observation
}

// ContentSnapshots returns the exact HTTP versions read by this render.
func (w *HTTPStoreWrapper) ContentSnapshots() ([]httpstore.ContentSnapshot, bool) {
	if w.transaction != nil {
		return w.transaction.Snapshots(), w.transaction.Cacheable()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	urls := make([]string, 0, len(w.snapshots))
	for url := range w.snapshots {
		urls = append(urls, url)
	}
	slices.Sort(urls)
	snapshots := make([]httpstore.ContentSnapshot, 0, len(urls))
	cacheable := true
	for _, url := range urls {
		snapshot := w.snapshots[url]
		snapshots = append(snapshots, snapshot)
		cacheable = cacheable && snapshot.Cacheable
	}
	return snapshots, cacheable
}

// CommittedAcceptedReplayState returns the exact HTTP publication retained by this render.
func (w *HTTPStoreWrapper) CommittedAcceptedReplayState() (*httpstore.AcceptedReplayState, bool) {
	if w == nil || w.transaction == nil {
		return nil, false
	}
	return w.transaction.committedAcceptedReplayState()
}

// Fetch fetches content from a URL.
//
// Template usage:
//
//	Basic fetch (no refresh):
//	  {{ http.Fetch("https://example.com/data.txt") }}
//
//	With a refresh interval for re-checking accepted content:
//	  {{ http.Fetch("https://example.com/data.txt", {"interval": "60s"}) }}
//
//	With options:
//	  {{ http.Fetch("https://example.com/data.txt", {"interval": "5m", "timeout": "30s", "retries": 3, "critical": true}) }}
//
//	With authentication:
//	  {{ http.Fetch("https://api.example.com/data", {"interval": "5m"}, {"type": "bearer", "token": token}) }}
//	  {{ http.Fetch("https://api.example.com/data", {"interval": "5m"}, {"type": "basic", "username": user, "password": pass}) }}
//
// Parameters (variadic):
//   - url (string, required): The HTTP(S) URL to fetch
//   - options (map, optional): {"interval": "60s", "timeout": "30s", "retries": 3, "critical": true}
//   - auth (map, optional): {"type": "bearer"|"basic"|"header", "token": "...", "username": "...", "password": "..."}
//
// Returns:
//   - Content string (empty if fetch failed and not critical)
//   - Error if critical fetch fails
func (w *HTTPStoreWrapper) Fetch(args ...any) (any, error) {
	content, _, err := w.FetchSnapshot(args...)
	return content, err
}

// FetchSnapshot performs one fetch and returns the exact input version used by this call.
func (w *HTTPStoreWrapper) FetchSnapshot(args ...any) (any, httpstore.ContentSnapshot, error) {
	url, opts, auth, err := ParseFetchArgs(args)
	if err != nil {
		return nil, httpstore.ContentSnapshot{}, err
	}
	content, snapshot, err := w.fetchSnapshot(url, opts, auth)
	if err != nil && opts.Critical && !errors.Is(err, ErrCandidatePending) && !errors.Is(err, ErrCandidateWithheld) {
		err = &CriticalFetchError{Cause: err}
	}
	if err != nil && (opts.Critical || errors.Is(err, ErrCandidatePending)) {
		w.mu.Lock()
		if w.renderFailure == nil {
			w.renderFailure = err
		}
		w.mu.Unlock()
	}
	if !opts.Critical && errors.Is(err, ErrCandidatePending) {
		return "", snapshot, nil
	}
	return content, snapshot, err
}

// ErrCriticalFetchFailed prevents resource isolation from retrying an unavailable HTTP input.
var ErrCriticalFetchFailed = errors.New("critical http.Fetch failed")

// CriticalFetchError reports a critical source the render could not read.
type CriticalFetchError struct {
	Cause error
}

func (e *CriticalFetchError) Error() string { return "critical http.Fetch failed: " + e.Cause.Error() }

func (e *CriticalFetchError) Unwrap() error { return e.Cause }

func (e *CriticalFetchError) Is(target error) bool { return target == ErrCriticalFetchFailed }

// RenderFailure preserves required fetch errors even when Scriggo discards a call's error value.
func (w *HTTPStoreWrapper) RenderFailure() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.renderFailure
}

func (w *HTTPStoreWrapper) fetchSnapshot(url string, opts httpstore.FetchOptions, auth *httpstore.AuthConfig) (any, httpstore.ContentSnapshot, error) {
	descriptor, err := httpstore.DescribeSource(opts, auth)
	if err != nil {
		return nil, httpstore.ContentSnapshot{}, fmt.Errorf("http.Fetch: %w", err)
	}
	failed := httpstore.ContentSnapshot{URL: url, Descriptor: descriptor}
	if err := w.declare(url, descriptor); err != nil {
		return nil, failed, err
	}
	if err := w.rejectOverlaySourceConflict(url, descriptor); err != nil {
		return nil, failed, err
	}

	var snapshot httpstore.ContentSnapshot
	if w.sourceMode != SourceModeAuthoritative {
		snapshot, err = w.fetchReadOnly(url, descriptor, opts, auth)
	} else {
		snapshot, err = w.fetchAuthoritative(url, opts, auth)
	}
	if err != nil {
		return nil, failed, err
	}
	return snapshot.Content, snapshot, nil
}

func (w *HTTPStoreWrapper) fetchReadOnly(
	url string,
	descriptor httpstore.SourceDescriptor,
	opts httpstore.FetchOptions,
	auth *httpstore.AuthConfig,
) (httpstore.ContentSnapshot, error) {
	if snapshot, exists := w.recordedSnapshot(url); exists {
		return snapshot, nil
	}
	value, err, _ := w.readOnlyGroup.Do(url, func() (any, error) {
		if snapshot, exists := w.recordedSnapshot(url); exists {
			return snapshot, nil
		}
		return w.fetchReadOnlyOnce(url, descriptor, opts, auth)
	})
	if err != nil {
		return httpstore.ContentSnapshot{}, err
	}
	snapshot, ok := value.(httpstore.ContentSnapshot)
	if !ok {
		return httpstore.ContentSnapshot{}, errors.New("HTTP read-only fetch returned an invalid snapshot")
	}
	return snapshot, nil
}

func (w *HTTPStoreWrapper) fetchReadOnlyOnce(
	url string,
	descriptor httpstore.SourceDescriptor,
	opts httpstore.FetchOptions,
	auth *httpstore.AuthConfig,
) (httpstore.ContentSnapshot, error) {
	snapshot, ok, err := w.getCachedSnapshot(url, descriptor)
	if err != nil {
		return httpstore.ContentSnapshot{}, err
	}
	if ok {
		return w.recordSnapshot(&snapshot), nil
	}

	content, err := w.transientStore.Fetch(w.ctx, url, opts, auth)
	if err != nil {
		return httpstore.ContentSnapshot{}, err
	}
	snapshot = w.transientStore.AcceptedSnapshot(url, descriptor)
	if !snapshot.Found {
		snapshot.Content = content
	}
	snapshot.Cacheable = false
	snapshot.Token = httpstore.SnapshotToken{}
	return w.recordSnapshot(&snapshot), nil
}

func (w *HTTPStoreWrapper) recordedSnapshot(url string) (httpstore.ContentSnapshot, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	snapshot, exists := w.snapshots[url]
	return snapshot, exists
}

func (w *HTTPStoreWrapper) recordSnapshot(snapshot *httpstore.ContentSnapshot) httpstore.ContentSnapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	if previous, exists := w.snapshots[snapshot.URL]; exists && previous.Token != snapshot.Token {
		snapshot.Cacheable = false
	}
	if !snapshot.Cacheable {
		snapshot.Token = httpstore.SnapshotToken{}
	}
	w.snapshots[snapshot.URL] = *snapshot
	return *snapshot
}

func (w *HTTPStoreWrapper) fetchAuthoritative(
	url string,
	opts httpstore.FetchOptions,
	auth *httpstore.AuthConfig,
) (httpstore.ContentSnapshot, error) {
	snapshot, err := w.transaction.fetch(w.ctx, url, opts, auth)
	if err != nil {
		return httpstore.ContentSnapshot{}, err
	}
	return snapshot, nil
}

func (w *HTTPStoreWrapper) declare(url string, descriptor httpstore.SourceDescriptor) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	previous, exists := w.declared[url]
	if exists && previous != descriptor {
		return fmt.Errorf(
			"http.Fetch: URL %s uses conflicting authentication or options in one render; use one declaration per URL",
			httpstore.RedactURL(url),
		)
	}
	w.declared[url] = descriptor
	return nil
}

func (w *HTTPStoreWrapper) rejectOverlaySourceConflict(
	url string,
	descriptor httpstore.SourceDescriptor,
) error {
	if w.overlay == nil || !w.overlay.HasPendingURL(url) {
		return nil
	}
	provider, ok := w.overlay.(interface {
		GetContentForDescriptor(string, httpstore.SourceDescriptor) (string, bool)
	})
	if ok {
		_, ok = provider.GetContentForDescriptor(url, descriptor)
	}
	if ok {
		return nil
	}
	return fmt.Errorf(
		"http.Fetch: URL %s has pending content from different authentication or options; retry after the source change settles",
		httpstore.RedactURL(url),
	)
}

func (w *HTTPStoreWrapper) getCachedSnapshot(
	url string,
	descriptor httpstore.SourceDescriptor,
) (httpstore.ContentSnapshot, bool, error) {
	if w.overlay == nil {
		snapshot := w.component.GetStore().AcceptedSnapshot(url, descriptor)
		return snapshot, snapshot.Found, nil
	}
	missing := httpstore.ContentSnapshot{URL: url, Descriptor: descriptor}
	snapshotProvider, ok := w.overlay.(interface {
		Snapshot(string, httpstore.SourceDescriptor) httpstore.ContentSnapshot
	})
	if ok {
		snapshot := snapshotProvider.Snapshot(url, descriptor)
		snapshot.URL = url
		snapshot.Descriptor = descriptor
		if snapshot.Found {
			return snapshot, true, nil
		}
		missing = snapshot
	} else if provider, exact := w.overlay.(interface {
		GetContentForDescriptor(string, httpstore.SourceDescriptor) (string, bool)
	}); exact {
		if content, found := provider.GetContentForDescriptor(url, descriptor); found {
			return httpstore.ContentSnapshot{
				URL:        url,
				Descriptor: descriptor,
				Content:    content,
				Found:      true,
			}, true, nil
		}
	} else {
		return httpstore.ContentSnapshot{}, false,
			errors.New("http.Fetch: HTTP overlay does not support exact source matching")
	}
	if w.overlay.HasPendingURL(url) {
		return httpstore.ContentSnapshot{}, false, fmt.Errorf(
			"http.Fetch: URL %s has pending content from different authentication or options; retry after the source change settles",
			httpstore.RedactURL(url),
		)
	}
	return missing, false, nil
}

// ParseFetchArgs extracts and validates the URL, options, and auth of an http.Fetch call.
func ParseFetchArgs(args []any) (string, httpstore.FetchOptions, *httpstore.AuthConfig, error) {
	if len(args) < 1 {
		return "", httpstore.FetchOptions{}, nil, errors.New("http.Fetch requires at least 1 argument (url)")
	}

	// Extract URL
	url, err := toString(args[0])
	if err != nil {
		return "", httpstore.FetchOptions{}, nil, fmt.Errorf("http.Fetch: url must be a string, got %T", args[0])
	}

	// Parse options (optional second argument)
	opts, err := parseOptionsArg(args)
	if err != nil {
		return "", httpstore.FetchOptions{}, nil, err
	}

	// Parse auth (optional third argument)
	var auth *httpstore.AuthConfig
	if len(args) >= 3 && args[2] != nil {
		auth, err = parseAuthFromArg(args[2])
		if err != nil {
			return "", httpstore.FetchOptions{}, nil, err
		}
	}

	return url, opts, auth, nil
}

// parseOptionsArg extracts FetchOptions from the second argument if present.
func parseOptionsArg(args []any) (httpstore.FetchOptions, error) {
	if len(args) < 2 || args[1] == nil {
		return httpstore.FetchOptions{}, nil
	}

	optMap, ok := toMap(args[1])
	if !ok {
		return httpstore.FetchOptions{}, fmt.Errorf("http.Fetch: options must be a map, got %T", args[1])
	}

	opts, err := parseFetchOptions(optMap)
	if err != nil {
		return httpstore.FetchOptions{}, fmt.Errorf("http.Fetch: %w", err)
	}
	return opts, nil
}

// parseAuthFromArg extracts AuthConfig from a non-nil argument.
func parseAuthFromArg(arg any) (*httpstore.AuthConfig, error) {
	authMap, ok := toMap(arg)
	if !ok {
		return nil, fmt.Errorf("http.Fetch: auth must be a map, got %T", arg)
	}

	auth, err := parseAuthConfig(authMap)
	if err != nil {
		return nil, fmt.Errorf("http.Fetch: %w", err)
	}
	return auth, nil
}

// parseFetchOptions parses a map into FetchOptions.
func parseFetchOptions(m map[string]any) (httpstore.FetchOptions, error) {
	opts := httpstore.FetchOptions{}

	if _, ok := m["delay"]; ok {
		return opts, templating.ErrHTTPFetchDelayOption
	}
	if v, ok := m["interval"]; ok {
		d, err := parseDuration(v)
		if err != nil {
			return opts, fmt.Errorf("invalid interval: %w", err)
		}
		opts.Delay = d
	}

	if v, ok := m["timeout"]; ok {
		d, err := parseDuration(v)
		if err != nil {
			return opts, fmt.Errorf("invalid timeout: %w", err)
		}
		opts.Timeout = d
	}

	if v, ok := m["retries"]; ok {
		n, err := toInt(v)
		if err != nil {
			return opts, fmt.Errorf("invalid retries: %w", err)
		}
		opts.Retries = n
	}

	if v, ok := m["critical"]; ok {
		b, err := toBool(v)
		if err != nil {
			return opts, fmt.Errorf("invalid critical: %w", err)
		}
		opts.Critical = b
	}

	if v, ok := m["acceptStatus"]; ok {
		codes, err := toIntList(v)
		if err != nil {
			return opts, fmt.Errorf("invalid acceptStatus: %w", err)
		}
		set, err := httpstore.NewStatusSet(codes...)
		if err != nil {
			return opts, err
		}
		opts.AcceptStatus = set
	}

	return opts, nil
}

// toIntList converts a template list of whole numbers to []int.
func toIntList(v any) ([]int, error) {
	list := reflect.ValueOf(v)
	if v == nil || list.Kind() != reflect.Slice && list.Kind() != reflect.Array {
		return nil, fmt.Errorf("expected a list of status codes, got %T", v)
	}
	codes := make([]int, 0, list.Len())
	for i := range list.Len() {
		code, err := toWholeNumber(list.Index(i).Interface())
		if err != nil {
			return nil, fmt.Errorf("element %d: %w", i, err)
		}
		codes = append(codes, code)
	}
	return codes, nil
}

func toWholeNumber(v any) (int, error) {
	value := reflect.ValueOf(v)
	switch value.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return int(value.Int()), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		unsigned := value.Uint()
		if unsigned > math.MaxInt32 {
			return 0, fmt.Errorf("%d is out of range", unsigned)
		}
		return int(unsigned), nil
	case reflect.Float32, reflect.Float64:
		f := value.Float()
		if f != math.Trunc(f) || math.Abs(f) > math.MaxInt32 {
			return 0, fmt.Errorf("expected a whole number, got %v", f)
		}
		return int(f), nil
	default:
		return 0, fmt.Errorf("expected a whole number, got %T", v)
	}
}

// parseAuthConfig parses a map into AuthConfig.
func parseAuthConfig(m map[string]any) (*httpstore.AuthConfig, error) {
	auth := &httpstore.AuthConfig{}

	if v, ok := m["type"]; ok {
		s, err := toString(v)
		if err != nil {
			return nil, fmt.Errorf("invalid auth type: %w", err)
		}
		auth.Type = s
	}

	if v, ok := m["username"]; ok {
		s, err := toString(v)
		if err != nil {
			return nil, fmt.Errorf("invalid username: %w", err)
		}
		auth.Username = s
	}

	if v, ok := m["password"]; ok {
		s, err := toString(v)
		if err != nil {
			return nil, fmt.Errorf("invalid password: %w", err)
		}
		auth.Password = s
	}

	if v, ok := m["token"]; ok {
		s, err := toString(v)
		if err != nil {
			return nil, fmt.Errorf("invalid token: %w", err)
		}
		auth.Token = s
	}

	if v, ok := m["headers"]; ok {
		headers, ok := toMap(v)
		if !ok {
			return nil, errors.New("invalid headers: expected map")
		}
		auth.Headers = make(map[string]string)
		for k, val := range headers {
			s, err := toString(val)
			if err != nil {
				return nil, fmt.Errorf("invalid header value for %s: %w", k, err)
			}
			auth.Headers[k] = s
		}
	}

	return auth, nil
}

// toString converts an interface to string.
func toString(v any) (string, error) {
	switch val := v.(type) {
	case string:
		return val, nil
	case fmt.Stringer:
		return val.String(), nil
	default:
		return "", fmt.Errorf("expected string, got %T", v)
	}
}

// toMap converts an interface to map[string]any.
func toMap(v any) (map[string]any, bool) {
	switch val := v.(type) {
	case map[string]any:
		return val, true
	case map[any]any:
		// Convert to string keys
		result := make(map[string]any)
		for k, v := range val {
			switch key := k.(type) {
			case string:
				result[key] = v
			case fmt.Stringer:
				result[key.String()] = v
			}
		}
		return result, true
	default:
		return nil, false
	}
}

// toInt converts an interface to int.
func toInt(v any) (int, error) {
	switch val := v.(type) {
	case int:
		return val, nil
	case int64:
		return int(val), nil
	case float64:
		return int(val), nil
	default:
		return 0, fmt.Errorf("expected number, got %T", v)
	}
}

// toBool converts an interface to bool.
func toBool(v any) (bool, error) {
	switch val := v.(type) {
	case bool:
		return val, nil
	default:
		return false, fmt.Errorf("expected bool, got %T", v)
	}
}

// parseDuration parses a duration from string or time.Duration.
func parseDuration(v any) (time.Duration, error) {
	switch val := v.(type) {
	case time.Duration:
		return val, nil
	case string:
		return time.ParseDuration(val)
	case fmt.Stringer:
		return time.ParseDuration(val.String())
	default:
		return 0, fmt.Errorf("expected duration string, got %T", v)
	}
}
