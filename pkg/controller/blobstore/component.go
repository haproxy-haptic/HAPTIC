// Copyright 2026 Philipp Hossner
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

// Package blobstore connects the pure blob store to one iteration's HTTP
// store: blob.Fetch sources stream into it, and digests the HTTP store no
// longer holds are removed from disk (ADR-0041).
package blobstore

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/blobstore"
	"github.com/haproxy-haptic/HAPTIC/pkg/httpstore"
)

const (
	// ComponentName is the unique identifier for this component.
	ComponentName = "blobstore"

	// RetentionInterval is how often unreferenced blobs are removed.
	RetentionInterval = time.Minute
)

// Sources is the HTTP store's view of its blob sources.
type Sources interface {
	EnableBlobs(sink httpstore.BlobSink)
	BlobSources() []httpstore.BlobSource
}

// Component retains the blobs one HTTP store references.
type Component struct {
	store        *blobstore.Store
	sources      Sources
	inUse        func() []string
	logger       *slog.Logger
	removeHolder func()

	mu sync.Mutex
	// accepted and replaced remember each source's accepted digest and the
	// one it replaced, so a confirmed predecessor survives one more change.
	accepted map[string]string
	replaced map[string]string
}

// Option configures a Component.
type Option func(*Component)

// WithInUse adds digests that must stay although no source holds them, such
// as the blobs HAProxy pods still report.
func WithInUse(inUse func() []string) Option {
	return func(c *Component) { c.inUse = inUse }
}

// New enables blob sources on sources, streams them into store, and holds
// their digests until Close.
func New(logger *slog.Logger, store *blobstore.Store, sources Sources, options ...Option) *Component {
	c := &Component{
		store:    store,
		sources:  sources,
		logger:   logger.With("component", ComponentName),
		accepted: make(map[string]string),
		replaced: make(map[string]string),
	}
	for _, option := range options {
		option(c)
	}
	sources.EnableBlobs(store)
	c.removeHolder = sync.OnceFunc(store.AddHolder(c.Digests))
	return c
}

// Close releases this component's digests to the next retention pass.
func (c *Component) Close() {
	c.removeHolder()
}

// Name returns the component name.
func (c *Component) Name() string {
	return ComponentName
}

// Start removes unreferenced blobs every RetentionInterval until ctx ends.
func (c *Component) Start(ctx context.Context) error {
	ticker := time.NewTicker(RetentionInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			c.Retain()
		}
	}
}

// Retain removes the blobs no live HTTP store and no in-use digest references.
func (c *Component) Retain() {
	var inUse []string
	if c.inUse != nil {
		inUse = c.inUse()
	}
	if _, err := c.store.Retain(inUse); err != nil {
		c.logger.Warn("Removing unreferenced blobs failed; they stay on disk until the next attempt", "error", err)
	}
}

// Digests returns every digest this component's sources still need: accepted,
// pending, the one an unconfirmed acceptance replaced, and each source's
// previous accepted digest.
func (c *Component) Digests() []string {
	sources := c.sources.BlobSources()
	c.mu.Lock()
	defer c.mu.Unlock()
	accepted := make(map[string]string, len(sources))
	replaced := make(map[string]string, len(sources))
	var digests []string
	for _, source := range sources {
		digests = append(digests, source.Digests()...)
		accepted[source.URL] = source.Accepted
		previous := c.replaced[source.URL]
		if last := c.accepted[source.URL]; last != "" && last != source.Accepted {
			previous = last
		}
		if previous != "" {
			replaced[source.URL] = previous
			digests = append(digests, previous)
		}
	}
	c.accepted = accepted
	c.replaced = replaced
	return digests
}

// Open opens a held blob for streaming.
func (c *Component) Open(digest string) (*os.File, error) {
	return c.store.Open(digest)
}
