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

package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
)

func blobDigest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func TestPutBlobStreamsTheBlobUnderItsDigest(t *testing.T) {
	t.Parallel()
	digest := blobDigest("mmdb")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPut, r.Method)
		assert.Equal(t, api.BlobPath(digest), r.URL.Path)
		assert.Equal(t, int64(4), r.ContentLength)
		user, password, ok := r.BasicAuth()
		assert.True(t, ok)
		assert.Equal(t, "admin", user)
		assert.Equal(t, "adminpwd", password)
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		assert.Equal(t, "mmdb", string(body))
		writeJSON(t, w, http.StatusOK, api.Blob{SHA256: digest, Size: 4})
	}))
	defer srv.Close()

	require.NoError(t, newTestClient(t, srv.URL).PutBlob(context.Background(), digest, 4, strings.NewReader("mmdb")))
}

func TestPutBlobClassifiesItsRefusals(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	status := http.StatusNotFound
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeJSON(t, w, status, api.ApplyError{Stage: "blob", Message: "refused"})
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	err := c.PutBlob(context.Background(), blobDigest("x"), 1, strings.NewReader("x"))
	require.ErrorIs(t, err, ErrNoBlobEndpoint, "an agent without --blob-dir, or one that predates blobs")

	status = http.StatusRequestEntityTooLarge
	err = c.PutBlob(context.Background(), blobDigest("x"), 1, strings.NewReader("x"))
	var httpErr *HTTPError
	require.ErrorAs(t, err, &httpErr)
	assert.Equal(t, http.StatusRequestEntityTooLarge, httpErr.Status)

	before := calls.Load()
	require.Error(t, c.PutBlob(context.Background(), "NOT-A-DIGEST", 1, strings.NewReader("x")))
	require.Error(t, c.PutBlob(context.Background(), blobDigest("x"), -1, strings.NewReader("x")))
	assert.Equal(t, before, calls.Load(), "an invalid put never reaches the wire")
}

func TestGetBlobVerifiesWhatItReads(t *testing.T) {
	t.Parallel()
	good, other := blobDigest("db v1"), blobDigest("db v2")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case api.BlobPath(good):
			_, _ = io.WriteString(w, "db v1")
		case api.BlobPath(other):
			_, _ = io.WriteString(w, "corrupted")
		default:
			writeJSON(t, w, http.StatusNotFound, api.ApplyError{Stage: "blob", Message: "not held"})
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	body, size, err := c.GetBlob(context.Background(), good)
	require.NoError(t, err)
	content, err := io.ReadAll(body)
	require.NoError(t, err)
	require.NoError(t, body.Close())
	assert.Equal(t, "db v1", string(content))
	assert.Equal(t, int64(5), size)

	body, _, err = c.GetBlob(context.Background(), other)
	require.NoError(t, err)
	_, err = io.ReadAll(body)
	require.ErrorIs(t, err, ErrBlobDigestMismatch)
	require.NoError(t, body.Close())

	_, _, err = c.GetBlob(context.Background(), blobDigest("absent"))
	require.ErrorIs(t, err, ErrBlobNotHeld)
}

func TestApplyReportsMissingBlobs(t *testing.T) {
	t.Parallel()
	digest := blobDigest("db")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusConflict, api.MissingBlobs{Reason: api.ReasonMissingBlobs, MissingBlobs: []string{digest}})
	}))
	defer srv.Close()

	m := testManifest()
	m.RequiredBlobs = []string{digest}
	_, err := newTestClient(t, srv.URL).Apply(context.Background(), m,
		map[string]io.Reader{"haproxy.cfg": strings.NewReader("globa")}, nil)
	var missing *MissingBlobsError
	require.ErrorAs(t, err, &missing)
	assert.Equal(t, []string{digest}, missing.Missing)
}
