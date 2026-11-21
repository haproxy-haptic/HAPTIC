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

package blobstore

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/blob"
	pureblobstore "github.com/haproxy-haptic/HAPTIC/pkg/blobstore"
	"github.com/haproxy-haptic/HAPTIC/pkg/httpstore"
)

type fakeSources struct {
	mu      sync.Mutex
	sink    httpstore.BlobSink
	sources []httpstore.BlobSource
}

func (f *fakeSources) EnableBlobs(sink httpstore.BlobSink) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sink = sink
}

func (f *fakeSources) BlobSources() []httpstore.BlobSource {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]httpstore.BlobSource(nil), f.sources...)
}

func (f *fakeSources) set(sources ...httpstore.BlobSource) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sources = sources
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type testRig struct {
	component *Component
	sources   *fakeSources
	store     *pureblobstore.Store
	clock     *testClock
}

func newTestComponent(t *testing.T, options ...Option) testRig {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	clock := &testClock{now: time.Unix(1_700_000_000, 0)}
	store, err := pureblobstore.New(t.TempDir(), 1<<20, logger, pureblobstore.WithClock(clock.Now))
	require.NoError(t, err)
	sources := &fakeSources{}
	return testRig{component: New(logger, store, sources, options...), sources: sources, store: store, clock: clock}
}

func TestNewEnablesBlobSourcesOnTheHTTPStore(t *testing.T) {
	rig := newTestComponent(t)
	assert.Same(t, rig.store, rig.sources.sink)
}

func TestDigestsKeepEachSourcesPredecessor(t *testing.T) {
	rig := newTestComponent(t)
	component, sources := rig.component, rig.sources
	v1, v2, v3 := blob.Describe([]byte("v1")).Digest, blob.Describe([]byte("v2")).Digest, blob.Describe([]byte("v3")).Digest

	sources.set(httpstore.BlobSource{URL: "a", Accepted: v1})
	assert.ElementsMatch(t, []string{v1}, component.Digests())

	sources.set(httpstore.BlobSource{URL: "a", Accepted: v2, Pending: v3})
	assert.ElementsMatch(t, []string{v2, v3, v1}, component.Digests(), "a confirmed predecessor stays")
	assert.ElementsMatch(t, []string{v2, v3, v1}, component.Digests(), "until the source changes again")

	sources.set(httpstore.BlobSource{URL: "a", Accepted: v3})
	assert.ElementsMatch(t, []string{v3, v2}, component.Digests())

	sources.set()
	assert.Empty(t, component.Digests(), "a source the HTTP store evicted holds nothing")
}

func TestRetainRemovesWhatNoSourceOrPodNeeds(t *testing.T) {
	inUse := ""
	rig := newTestComponent(t, WithInUse(func() []string { return []string{inUse} }))
	component, sources, store, clock := rig.component, rig.sources, rig.store, rig.clock
	ingest := func(data string) string {
		ref, err := store.Ingest(t.Context(), strings.NewReader(data), blob.Spec{Unpack: blob.UnpackNone})
		require.NoError(t, err)
		return ref.Digest
	}
	accepted, onPod, orphan := ingest("accepted"), ingest("on a pod"), ingest("orphan")
	inUse = onPod
	sources.set(httpstore.BlobSource{URL: "a", Accepted: accepted})
	clock.Advance(pureblobstore.OrphanGrace)

	component.Retain()

	assert.True(t, store.Has(accepted))
	assert.True(t, store.Has(onPod))
	assert.False(t, store.Has(orphan))

	component.Close()
	component.Close()
	inUse = ""
	component.Retain()
	assert.False(t, store.Has(accepted), "a closed component holds nothing")
}

func TestStartStopsWithItsContext(t *testing.T) {
	component := newTestComponent(t).component
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan error, 1)
	go func() { stopped <- component.Start(ctx) }()
	cancel()
	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the component did not stop")
	}
	assert.Equal(t, ComponentName, component.Name())
}
