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

package httpstore

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/blob"
)

// memorySink keeps ingested blobs in memory and refuses bodies over limit.
type memorySink struct {
	mu      sync.Mutex
	limit   int
	ingests int
	blobs   map[string][]byte
}

func newMemorySink(limit int) *memorySink {
	return &memorySink{limit: limit, blobs: make(map[string][]byte)}
}

func (s *memorySink) Ingest(_ context.Context, body io.Reader, _ blob.Spec) (blob.Ref, error) {
	data, err := io.ReadAll(io.LimitReader(body, int64(s.limit)+1))
	if err != nil {
		return blob.Ref{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ingests++
	if len(data) > s.limit {
		return blob.Ref{}, fmt.Errorf("%w: too large", blob.ErrRefused)
	}
	ref := blob.Describe(data)
	s.blobs[ref.Digest] = data
	return ref, nil
}

func (s *memorySink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ingests
}

type failureRecorder struct {
	mu     sync.Mutex
	errors []error
}

func (r *failureRecorder) record(_ string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, err)
}

func (r *failureRecorder) all() []error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]error(nil), r.errors...)
}

var blobOptions = FetchOptions{Delay: time.Minute, RetryDelay: time.Millisecond, Blob: blob.Spec{Unpack: blob.UnpackNone}.Key()}

func newBlobStore(sink BlobSink) (*HTTPStore, *failureRecorder) {
	store := New(slog.New(slog.NewTextHandler(io.Discard, nil)), 0)
	failures := &failureRecorder{}
	if sink != nil {
		store.SetBlobSink(sink, failures.record)
	}
	return store, failures
}

func TestBlobSourceContentIsTheBlobReference(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("database"))
	}))
	defer server.Close()
	sink := newMemorySink(1 << 20)
	store, _ := newBlobStore(sink)

	content, err := store.Fetch(t.Context(), server.URL, blobOptions, nil)

	require.NoError(t, err)
	assert.Equal(t, blob.Describe([]byte("database")).Encode(), content)
	assert.Equal(t, []byte("database"), sink.blobs[blob.Describe([]byte("database")).Digest])
}

func TestBlobSourceIsRefusedWithoutASink(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte("database"))
	}))
	defer server.Close()
	store, _ := newBlobStore(nil)
	options := blobOptions
	options.Critical = true

	_, err := store.Fetch(t.Context(), server.URL, options, nil)

	require.ErrorIs(t, err, ErrBlobsDisabled)
	assert.Zero(t, requests.Load(), "a store without a sink never downloads a blob")
}

func TestRefusedBlobIsNotRetried(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte("too large"))
	}))
	defer server.Close()
	store, failures := newBlobStore(newMemorySink(3))
	options := blobOptions
	options.Critical = true

	_, err := store.Fetch(t.Context(), server.URL, options, nil)

	require.ErrorIs(t, err, blob.ErrRefused)
	assert.Equal(t, int32(1), requests.Load())
	require.Len(t, failures.all(), 1)
	assert.ErrorIs(t, failures.all()[0], blob.ErrRefused)
}

func TestBlobRefreshKeepsTheAcceptedVersion(t *testing.T) {
	var version atomic.Int32
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch version.Load() {
		case 0:
			w.Header().Set("ETag", `"v1"`)
			if r.Header.Get("If-None-Match") == `"v1"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			_, _ = w.Write([]byte("v1"))
		default:
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	defer server.Close()
	sink := newMemorySink(1 << 20)
	store, failures := newBlobStore(sink)
	accepted, err := store.Fetch(t.Context(), server.URL, blobOptions, nil)
	require.NoError(t, err)

	unchanged, err := store.RefreshURLVersion(t.Context(), server.URL)
	require.NoError(t, err)
	assert.Nil(t, unchanged, "a 304 keeps the accepted blob")
	assert.Equal(t, 1, sink.count(), "a 304 downloads nothing")

	version.Store(1)
	requests.Store(0)
	_, err = store.RefreshURLVersion(t.Context(), server.URL)
	require.Error(t, err)
	assert.Equal(t, int32(3), requests.Load(), "a server error is retried")
	content, ok := store.Get(server.URL)
	require.True(t, ok)
	assert.Equal(t, accepted, content, "a failed refresh keeps the accepted blob")
	assert.Len(t, failures.all(), 1)
}

func TestBlobSourcesListEveryHeldDigest(t *testing.T) {
	var body atomic.Value
	body.Store("v1")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body.Load().(string)))
	}))
	defer server.Close()
	store, _ := newBlobStore(newMemorySink(1 << 20))
	textURL := server.URL + "/text"
	textState := acceptInitial(t, store, textURL, FetchOptions{})
	state := acceptInitial(t, store, server.URL, blobOptions)
	textSnapshot := store.AcceptedSnapshot(textURL, textState.Descriptor)
	snapshot := store.AcceptedSnapshot(server.URL, state.Descriptor)
	store.ConfirmAcceptances([]ObservationToken{textSnapshot.ObservationToken(), snapshot.ObservationToken()})
	v1, v2, v3 := blob.Describe([]byte("v1")).Digest, blob.Describe([]byte("v2")).Digest, blob.Describe([]byte("v3")).Digest

	assert.Equal(t, []BlobSource{{URL: server.URL, Accepted: v1}}, store.BlobSources(), "text sources hold no blobs")

	body.Store("v2")
	pending, err := store.RefreshURLVersion(t.Context(), server.URL)
	require.NoError(t, err)
	require.True(t, store.PromotePendingVersion(server.URL, pending.Checksum, pending.Revision))
	body.Store("v3")
	_, err = store.RefreshURLVersion(t.Context(), server.URL)
	require.NoError(t, err)

	sources := store.BlobSources()
	require.Equal(t, []BlobSource{{URL: server.URL, Accepted: v2, Pending: v3, Replaced: v1}}, sources)
	assert.Equal(t, []string{v2, v3, v1}, sources[0].Digests())

	revoked := store.RevokeAcceptances(store.AcceptanceSequence())
	require.Len(t, revoked, 1)
	assert.Equal(t, v2, revoked[0].BlobDigest)
	assert.True(t, revoked[0].Restored)
	content, ok := store.Get(server.URL)
	require.True(t, ok)
	assert.Equal(t, blob.Describe([]byte("v1")).Encode(), content, "a revocation restores the previous blob")
}

func TestBlobDeclarationLeavesOtherSourceIdentitiesUnchanged(t *testing.T) {
	text, err := DescribeSource(FetchOptions{}, nil)
	require.NoError(t, err)
	assert.NotContains(t, text.canonical, "Blob")
	blobSource, err := DescribeSource(FetchOptions{Blob: blob.Spec{Unpack: blob.UnpackNone}.Key()}, nil)
	require.NoError(t, err)
	assert.NotEqual(t, text, blobSource)
	assert.True(t, strings.Contains(blobSource.canonical, `Unpack`))
}
