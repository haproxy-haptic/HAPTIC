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
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/blobs"
)

// ApplyError stages of the request checks and the blob calls.
const (
	stageRequest = "request"
	stageBlob    = "blob"
)

// blobTransferTimeout replaces the server's read and write timeouts for one
// blob, which at the default 256 MiB cap needs well over a config bundle's.
const blobTransferTimeout = 10 * time.Minute

func openBlobs(cfg *Config) (*blobs.Store, error) {
	maxSize := cfg.BlobMaxSize
	if maxSize == 0 {
		maxSize = api.DefaultMaxBlobBytes
	}
	store, err := blobs.New(cfg.BlobDir, maxSize)
	if err != nil {
		return nil, fmt.Errorf("--blob-dir: %w", err)
	}
	return store, nil
}

// handlePutBlob stores one blob. It takes no apply lock: a blob is immutable
// and only becomes reachable through a later apply that requires it.
func (s *Server) handlePutBlob(w http.ResponseWriter, r *http.Request) {
	digest := r.PathValue("sha256")
	if !api.ValidBlobDigest(digest) {
		writeJSON(w, http.StatusBadRequest, api.ApplyError{Stage: stageRequest, Message: blobs.ErrInvalidDigest.Error()})
		return
	}
	if r.ContentLength > s.blobs.MaxSize() {
		writeJSON(w, http.StatusRequestEntityTooLarge, api.ApplyError{
			Stage: stageBlob, Message: fmt.Sprintf("blob of %d bytes exceeds --blob-max-size %d", r.ContentLength, s.blobs.MaxSize()),
		})
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(blobTransferTimeout))
	body := &requestBody{reader: r.Body}
	size, err := s.blobs.Put(digest, body)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, api.Blob{SHA256: digest, Size: size})
	case errors.Is(err, blobs.ErrTooLarge):
		writeJSON(w, http.StatusRequestEntityTooLarge, api.ApplyError{Stage: stageBlob, Message: err.Error()})
	case errors.Is(err, blobs.ErrDigestMismatch), body.err != nil:
		writeJSON(w, http.StatusBadRequest, api.ApplyError{Stage: stageBlob, Message: err.Error()})
	default:
		s.logger.Error("could not store a blob", "sha256", digest, "error", err)
		writeJSON(w, http.StatusInternalServerError, api.ApplyError{Stage: stageBlob, Message: err.Error()})
	}
}

// requestBody tells a failed upload from a failed disk.
type requestBody struct {
	reader io.Reader
	err    error
}

func (b *requestBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		b.err = err
	}
	return n, err
}

// handleGetBlob serves a held blob, so a controller that lost its copy can
// take it back from a pod.
func (s *Server) handleGetBlob(w http.ResponseWriter, r *http.Request) {
	digest := r.PathValue("sha256")
	if !api.ValidBlobDigest(digest) {
		writeJSON(w, http.StatusBadRequest, api.ApplyError{Stage: stageRequest, Message: blobs.ErrInvalidDigest.Error()})
		return
	}
	f, size, err := s.blobs.Open(digest)
	if errors.Is(err, fs.ErrNotExist) {
		writeJSON(w, http.StatusNotFound, api.ApplyError{Stage: stageBlob, Message: "the agent does not hold blob " + digest})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, api.ApplyError{Stage: stageBlob, Message: err.Error()})
		return
	}
	defer func() { _ = f.Close() }()
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(blobTransferTimeout))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, f); err != nil {
		s.logger.Warn("serving a blob was cut short", "sha256", digest, "error", err)
	}
}

// heldBlobs is what the state reports; nil while the feature is off.
func (s *Server) heldBlobs() ([]api.Blob, error) {
	if s.blobs == nil {
		return nil, nil
	}
	return s.blobs.List()
}

// missingBlobs names the required blobs the agent does not hold; every one of
// them while the feature is off. A revert lands the last known good set, whose
// blobs GC keeps, not the set its manifest names.
func (s *Server) missingBlobs(m *api.Manifest) []string {
	if m.Mode == api.ModeRevertLKG || len(m.RequiredBlobs) == 0 {
		return nil
	}
	if s.blobs == nil {
		return slices.Clone(m.RequiredBlobs)
	}
	return s.blobs.Missing(m.RequiredBlobs)
}

// collectBlobs removes the blobs neither the tree nor the last known good set
// requires. cutoff is when the apply arrived: a blob put since then may be for
// the next apply.
func (s *Server) collectBlobs(cutoff time.Time) {
	if s.blobs == nil {
		return
	}
	s.mu.Lock()
	keep := append(slices.Clone(s.state.TreeBlobs), s.state.LKGBlobs...)
	s.mu.Unlock()
	removed, err := s.blobs.GC(keep, cutoff)
	if len(removed) > 0 {
		s.logger.Info("removed blobs no configuration requires", "sha256", removed)
	}
	if err != nil {
		s.logger.Warn("could not remove unreferenced blobs", "error", err)
	}
}
