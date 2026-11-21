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

package server

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/blobs"
)

// slowReader yields one chunk per tick, so an upload outlasts the server's
// read and write timeouts.
type slowReader struct {
	chunks []string
	tick   time.Duration
}

func (r *slowReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	time.Sleep(r.tick)
	n := copy(p, r.chunks[0])
	r.chunks = r.chunks[1:]
	return n, nil
}

// A blob upload gets the whole blobTransferTimeout for its body and its answer,
// not the server's timeouts sized for a config bundle.
func TestBlobPutOutlastsTheServerTimeouts(t *testing.T) {
	store, err := blobs.New(filepath.Join(t.TempDir(), "blobs"), 1<<20)
	require.NoError(t, err)
	s := &Server{cfg: Config{Username: "u", Password: "p"}, logger: slog.New(slog.DiscardHandler), blobs: store}
	srv := httptest.NewUnstartedServer(s.routes())
	srv.Config.ReadTimeout = 200 * time.Millisecond
	srv.Config.WriteTimeout = 200 * time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)

	chunks := []string{"geo", "ip", "-", "data", "base", "v1"}
	sum := sha256.Sum256([]byte(strings.Join(chunks, "")))
	digest := hex.EncodeToString(sum[:])
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPut, srv.URL+api.BlobPath(digest),
		&slowReader{chunks: chunks, tick: 100 * time.Millisecond})
	require.NoError(t, err)
	request.SetBasicAuth("u", "p")
	response, err := srv.Client().Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
}
