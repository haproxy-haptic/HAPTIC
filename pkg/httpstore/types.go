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

// Package httpstore provides HTTP resource fetching with caching and validation.
//
// This package implements a store that fetches resources from HTTP(S) URLs,
// caches them, and supports periodic refresh with a two-version cache for
// safe validation before accepting new content.
package httpstore

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/blob"
)

// levelTrace is a log level below Debug, used for very verbose diagnostic output.
// Following slog convention of 4-level gaps: TRACE=-8, DEBUG=-4, INFO=0, WARN=4, ERROR=8.
// This is duplicated from pkg/core/logging to avoid architecture rule violation.
const levelTrace = slog.Level(-8)

// DefaultTimeout is the default HTTP request timeout.
const DefaultTimeout = 30 * time.Second

// DefaultRetries is the default number of retry attempts. Combined
// with DefaultRetryDelay's exponential backoff (0.5s + 1s = 1.5s
// of waits across 3 total attempts), this caps the worst-case
// budget for an unreachable URL well under the 5-second envelope a
// non-critical HTTP fetch should respect on a template render hot
// path. Raising either value here pushes per-render latency past
// the conformance test convergence windows that gate CI.
const DefaultRetries = 2

// DefaultRetryDelay is the base delay between retry attempts. The
// backoff is exponential — attempt N waits `RetryDelay * 2^(N-1)`
// — so with Retries=2 the per-call wait budget is 0.5s + 1s =
// 1.5s. Each fetch attempt that fails fast on connection refused
// adds at most ~1 RTT, so total worst case is ~2s on a healthy
// network with an unreachable target.
const DefaultRetryDelay = 500 * time.Millisecond

// MaxContentSize is the maximum allowed content size (10MB).
const MaxContentSize = 10 * 1024 * 1024

// FetchOptions configures HTTP fetching behavior.
type FetchOptions struct {
	// Delay is the refresh interval (how often to re-fetch).
	// Zero means no automatic refresh (fetch once).
	Delay time.Duration

	// Timeout is the HTTP request timeout.
	// Default: 30s
	Timeout time.Duration

	// Retries is the number of retry attempts on failure.
	// Default: 3
	Retries int

	// RetryDelay is the wait time between retries.
	// Default: 1s
	RetryDelay time.Duration

	// Critical indicates whether fetch failure should fail the template render.
	// If true, a failed fetch returns an error.
	// If false, a failed fetch returns empty string and logs a warning.
	Critical bool

	// AcceptStatus lists statuses besides 200 whose body is the content,
	// for servers that answer with the status they describe (an error-page
	// service serving its 404 page as a 404).
	AcceptStatus StatusSet

	// Blob makes this a blob source: the body goes to the store's BlobSink,
	// and the content is the blob's encoded reference (ADR-0041). omitempty
	// keeps the identity of every other source unchanged.
	Blob blob.Key `json:",omitempty"`
}

// WithDefaults returns a copy of the options with default values applied.
func (o FetchOptions) WithDefaults() FetchOptions {
	if o.Timeout == 0 {
		o.Timeout = DefaultTimeout
	}
	if o.Retries == 0 {
		o.Retries = DefaultRetries
	}
	if o.RetryDelay == 0 {
		o.RetryDelay = DefaultRetryDelay
	}
	return o
}

// Supported AuthConfig.Type values.
const (
	// AuthTypeBasic selects HTTP basic authentication (Username/Password).
	AuthTypeBasic = "basic"

	// AuthTypeBearer selects bearer-token authentication (Token).
	AuthTypeBearer = "bearer"

	// AuthTypeHeader selects custom-header authentication (Headers).
	AuthTypeHeader = "header"
)

// AuthConfig configures HTTP authentication.
type AuthConfig struct {
	// Type is the authentication type: AuthTypeBasic, AuthTypeBearer, or AuthTypeHeader.
	Type string

	// Username for basic auth.
	Username string

	// Password for basic auth.
	Password string

	// Token for bearer auth.
	Token string

	// Headers for custom header auth (e.g., API keys).
	// These headers are added to every request.
	Headers map[string]string
}

// ValidationState represents the current validation state of a cached entry.
type ValidationState int

const (
	// StateAccepted means the accepted content is in use, no pending content.
	StateAccepted ValidationState = iota

	// StateValidating means pending content exists and is being validated.
	StateValidating

	// StateRejected means the last pending content was rejected, using accepted.
	StateRejected
)

// String returns a string representation of the validation state.
func (s ValidationState) String() string {
	switch s {
	case StateAccepted:
		return "accepted"
	case StateValidating:
		return "validating"
	case StateRejected:
		return "rejected"
	default:
		return "unknown"
	}
}

// CacheEntry holds cached content with two-version support for safe validation.
//
// The two-version design ensures that new content is only accepted after
// successful validation. This is critical for resources like IP blocklists
// where we must not discard the old blocklist before knowing the new one is valid.
type CacheEntry struct {
	mutationRevision uint64
	replayRevision   uint64
	acceptedRevision Revision
	sourceIdentity   string
	sourceDescriptor SourceDescriptor
	sourceGeneration uint64
	fixture          bool

	// URL is the source URL for this entry.
	URL string

	// Accepted version (validated, in production use)
	AcceptedContent  string
	AcceptedChecksum string
	AcceptedTime     time.Time

	// LastAccessTime tracks when this entry was last accessed via Get/Fetch.
	// Used for cache eviction of unused entries.
	LastAccessTime time.Time

	// Pending version (fetched, awaiting validation)
	PendingContent  string
	PendingChecksum string
	PendingRevision uint64
	HasPending      bool

	// ValidationState tracks the current state of this entry.
	ValidationState ValidationState

	// ValidationStartedAt is when ValidationState last became StateValidating.
	// Only StateValidating leaves it meaningful; see HTTPStore.validationStuckAfter.
	ValidationStartedAt time.Time

	// HTTP caching headers for conditional requests
	ETag         string
	LastModified string

	// Configuration for this URL
	Options FetchOptions
	Auth    *AuthConfig
}

// SourceState identifies the current authority and refresh policy for a URL.
type SourceState struct {
	Identity    string
	Descriptor  SourceDescriptor
	Generation  uint64
	Delay       time.Duration
	HasAccepted bool
}

// SourceReconcileResult describes an authoritative source transition.
type SourceReconcileResult struct {
	State   SourceState
	Changed bool
}

// PendingVersion identifies one pending content revision.
type PendingVersion struct {
	Checksum string
	Revision uint64
}

// checksum computes SHA256 checksum of content.
func checksum(content string) string {
	hash := sha256.Sum256([]byte(content))
	return hex.EncodeToString(hash[:])
}
