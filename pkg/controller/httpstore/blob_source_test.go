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
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/blob"
	"github.com/haproxy-haptic/HAPTIC/pkg/blobstore"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

func blobFetchOptions(spec blob.Spec) map[string]any {
	return map[string]any{"retries": 0, "timeout": "1s", templating.HTTPOptionBlob: spec}
}

func TestBlobSourceStreamsIntoTheEnabledStore(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("database"))
	}))
	defer server.Close()
	bus, logger := testutil.NewTestBusAndLogger()
	component := New(bus, logger, 0)
	store, err := blobstore.New(t.TempDir(), 1<<20, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	component.EnableBlobs(store)
	bus.Start()

	wrapper := NewHTTPStoreWrapper(t.Context(), component, logger, nil, SourceModeAuthoritative)
	content, err := wrapper.Fetch(server.URL, blobFetchOptions(blob.Spec{Unpack: blob.UnpackNone}))
	require.NoError(t, err)
	commitInputTransaction(t, wrapper)

	ref := blob.Describe([]byte("database"))
	assert.Equal(t, ref.Encode(), content)
	assert.True(t, store.Has(ref.Digest))
	sources := component.BlobSources()
	require.Len(t, sources, 1)
	assert.Equal(t, ref.Digest, sources[0].Accepted)
}

func TestBlobFetchFailurePublishesAnEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("larger than allowed"))
	}))
	defer server.Close()
	bus, logger := testutil.NewTestBusAndLogger()
	failures := bus.Subscribe("blob-failures", 10)
	component := New(bus, logger, 0)
	store, err := blobstore.New(t.TempDir(), 4, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	component.EnableBlobs(store)
	bus.Start()

	wrapper := NewHTTPStoreWrapper(t.Context(), component, logger, nil, SourceModeAuthoritative)
	content, err := wrapper.Fetch(server.URL+"/db?token=secret", blobFetchOptions(blob.Spec{Unpack: blob.UnpackNone}))
	require.NoError(t, err, "a blob that cannot be fetched never fails the render")
	assert.Empty(t, content)
	wrapper.InputTransaction().Abort()

	failed := testutil.WaitForEvent[*events.BlobFetchFailedEvent](t, failures, testutil.EventTimeout)
	assert.Equal(t, server.URL+"/db", failed.URL, "the URL is redacted")
	assert.Contains(t, failed.Error, "size limit of 4 bytes")
}

func TestReadOnlyRendersNeverDownloadABlob(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte("database"))
	}))
	defer server.Close()
	_, logger := testutil.NewTestBusAndLogger()

	wrapper := NewIsolatedHTTPStoreWrapper(t.Context(), logger)
	content, err := wrapper.Fetch(server.URL, blobFetchOptions(blob.Spec{Unpack: blob.UnpackNone}))

	require.NoError(t, err)
	assert.Empty(t, content)
	assert.Zero(t, requests.Load())
}

func TestFetchArgsRefuseABlobOptionTemplatesCanBuild(t *testing.T) {
	_, opts, _, err := ParseFetchArgs([]any{"https://example.com", map[string]any{templating.HTTPOptionBlob: "x"}})
	require.ErrorContains(t, err, "reserved for blob.Fetch")
	assert.False(t, opts.Blob.Declared())
	_, opts, _, err = ParseFetchArgs([]any{"https://example.com", map[string]any{templating.HTTPOptionBlob: blob.Spec{Unpack: "zip"}}})
	require.ErrorContains(t, err, "invalid blob")
	assert.False(t, opts.Blob.Declared())
	_, opts, _, err = ParseFetchArgs([]any{"https://example.com", blobFetchOptions(blob.Spec{Unpack: blob.UnpackGzip})})
	require.NoError(t, err)
	assert.Equal(t, blob.Spec{Unpack: blob.UnpackGzip}.Key(), opts.Blob)
}
