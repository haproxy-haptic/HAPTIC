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

package server_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
)

func blobDigest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// blobHarness runs an agent with a blob directory capped at maxSize bytes.
func blobHarness(t *testing.T, maxSize int64) (h *harness, dir string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "blobs")
	return newHarness(t, withBlobs(dir, maxSize)), dir
}

// blobRequest sends one blob call; a negative length streams the body chunked.
func (h *harness) blobRequest(method, digest string, body io.Reader, length int64, auth bool) (status int, answer []byte) {
	h.t.Helper()
	request, err := http.NewRequestWithContext(h.t.Context(), method, h.url+api.BlobPath(digest), body)
	require.NoError(h.t, err)
	if body != nil {
		request.ContentLength = length
	}
	if auth {
		request.SetBasicAuth(testUser, testPassword)
	}
	response, err := h.client.Do(request)
	require.NoError(h.t, err)
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	require.NoError(h.t, err)
	return response.StatusCode, raw
}

func (h *harness) putBlob(content string) string {
	h.t.Helper()
	digest := blobDigest(content)
	status, raw := h.blobRequest(http.MethodPut, digest, strings.NewReader(content), int64(len(content)), true)
	require.Equal(h.t, http.StatusOK, status, string(raw))
	return digest
}

// streamOnly hides the reader's length so the request goes out chunked.
type streamOnly struct{ io.Reader }

func TestBlobRoutesAreAuthenticatedAndValidateTheName(t *testing.T) {
	h, dir := blobHarness(t, 1<<20)
	digest := blobDigest("db")

	status, _ := h.blobRequest(http.MethodPut, digest, strings.NewReader("db"), 2, false)
	assert.Equal(t, http.StatusUnauthorized, status)
	status, _ = h.blobRequest(http.MethodGet, digest, nil, 0, false)
	assert.Equal(t, http.StatusUnauthorized, status)

	for _, name := range []string{strings.ToUpper(digest), digest[:63], digest + "0", "not-a-digest"} {
		status, _ = h.blobRequest(http.MethodPut, name, strings.NewReader("db"), 2, true)
		assert.Equal(t, http.StatusBadRequest, status, name)
		status, _ = h.blobRequest(http.MethodGet, name, nil, 0, true)
		assert.Equal(t, http.StatusBadRequest, status, name)
	}

	status, _ = h.blobRequest(http.MethodGet, digest, nil, 0, true)
	assert.Equal(t, http.StatusNotFound, status, "a blob the agent never received")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestBlobPutVerifiesContentAndGetServesIt(t *testing.T) {
	h, dir := blobHarness(t, 16)

	status, raw := h.blobRequest(http.MethodPut, blobDigest("expected"), strings.NewReader("tampered"), 8, true)
	assert.Equal(t, http.StatusBadRequest, status, string(raw))
	assert.Contains(t, string(raw), "does not match")

	big := strings.Repeat("x", 17)
	status, _ = h.blobRequest(http.MethodPut, blobDigest(big), strings.NewReader(big), 17, true)
	assert.Equal(t, http.StatusRequestEntityTooLarge, status, "a declared length over the cap")
	status, _ = h.blobRequest(http.MethodPut, blobDigest(big), streamOnly{strings.NewReader(big)}, -1, true)
	assert.Equal(t, http.StatusRequestEntityTooLarge, status, "a chunked body over the cap, not bounded by the apply limit")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "a refused put keeps nothing")

	digest := h.putBlob("mmdb v1")
	status, raw = h.blobRequest(http.MethodGet, digest, nil, 0, true)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "mmdb v1", string(raw))

	state := h.state(false)
	assert.Contains(t, state.Features, api.FeatureBlobs)
	assert.Equal(t, []api.Blob{{SHA256: digest, Size: int64(len("mmdb v1"))}}, state.Blobs)
}

// Blobs are not bounded by the apply body limit: a blob larger than one apply
// still lands when --blob-max-size allows it.
func TestBlobPutIsBoundedByItsOwnLimitNotTheApplyLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("streams more than one apply body")
	}
	const size = api.MaxApplyBodyBytes + 1
	h, _ := blobHarness(t, size)
	summer := sha256.New()
	_, err := io.Copy(summer, io.LimitReader(zeros{}, size))
	require.NoError(t, err)
	digest := hex.EncodeToString(summer.Sum(nil))

	status, raw := h.blobRequest(http.MethodPut, digest, io.LimitReader(zeros{}, size), size, true)
	require.Equal(t, http.StatusOK, status, string(raw))
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestApplyWaitsForItsRequiredBlobs(t *testing.T) {
	h, _ := blobHarness(t, 1<<20)
	first := firstApply(t, h)
	content := "mmdb v1"
	digest := blobDigest(content)

	files := baseFiles("global\n  # db " + digest + "\n")
	m := buildManifest("plan-2", files)
	m.Mode = api.ModeReload
	m.ExpectedPrevPlanID = first.AppliedPlanID
	m.ExpectedPrevToken = first.AppliedToken
	m.RequiredBlobs = []string{digest}

	status, raw := h.post(&m, files)
	require.Equal(t, http.StatusConflict, status, string(raw))
	refusal := api.MissingBlobs{}
	require.NoError(t, json.Unmarshal(raw, &refusal))
	assert.Equal(t, api.MissingBlobs{Reason: api.ReasonMissingBlobs, MissingBlobs: []string{digest}}, refusal)
	assert.Equal(t, "global\n", h.read(configPath), "nothing is written before the blobs are there")
	assert.Equal(t, first.AppliedPlanID, h.state(false).AppliedPlanID)
	assert.Equal(t, 1.0, h.metric("haptic_agent_apply_rejected_total", "blobs"))

	h.putBlob(content)
	result := h.apply(&m, files)
	require.True(t, result.OK, "%+v", result.Error)
	assert.Equal(t, "plan-2", result.AppliedPlanID)
	assert.Equal(t, []api.Blob{{SHA256: digest, Size: int64(len(content))}}, h.state(false).Blobs)
	assert.Zero(t, h.metric("haptic_agent_invariant_violations_total"), h.violations())
}

func TestAManifestNamingABadBlobIsRejected(t *testing.T) {
	h, _ := blobHarness(t, 1<<20)
	first := firstApply(t, h)
	digest := blobDigest("x")
	for name, required := range map[string][]string{
		"not a digest": {"NOT-A-DIGEST"},
		"duplicate":    {digest, digest},
	} {
		t.Run(name, func(t *testing.T) {
			files := baseFiles("global\n")
			m := buildManifest("plan-2", files)
			m.Mode = api.ModeReload
			m.ExpectedPrevPlanID = first.AppliedPlanID
			m.ExpectedPrevToken = first.AppliedToken
			m.RequiredBlobs = required
			status, raw := h.post(&m, files)
			assert.Equal(t, http.StatusBadRequest, status, string(raw))
		})
	}
}

// Without --blob-dir the agent has no blob routes, reports no blobs, and
// refuses a configuration that needs one rather than loading it without.
func TestBlobsOffRefusesWhatNeedsThem(t *testing.T) {
	h := newHarness(t)
	first := firstApply(t, h)
	digest := blobDigest("db")

	status, _ := h.blobRequest(http.MethodPut, digest, strings.NewReader("db"), 2, true)
	assert.Equal(t, http.StatusNotFound, status)
	status, _ = h.blobRequest(http.MethodGet, digest, nil, 0, true)
	assert.Equal(t, http.StatusNotFound, status)
	state := h.state(false)
	assert.NotContains(t, state.Features, api.FeatureBlobs)
	assert.Nil(t, state.Blobs)

	files := baseFiles("global\n")
	m := buildManifest("plan-2", files)
	m.Mode = api.ModeReload
	m.ExpectedPrevPlanID = first.AppliedPlanID
	m.ExpectedPrevToken = first.AppliedToken
	m.RequiredBlobs = []string{digest}
	status, raw := h.post(&m, files)
	require.Equal(t, http.StatusConflict, status, string(raw))
	refusal := api.MissingBlobs{}
	require.NoError(t, json.Unmarshal(raw, &refusal))
	assert.Equal(t, []string{digest}, refusal.MissingBlobs)
}

// age backdates every blob so the GC of the next apply sees it as older than
// that apply, whatever the filesystem's timestamp granularity.
func age(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	past := time.Now().Add(-time.Hour)
	for _, e := range entries {
		require.NoError(t, os.Chtimes(filepath.Join(dir, e.Name()), past, past))
	}
}

func (h *harness) heldDigests() []string {
	h.t.Helper()
	state := h.state(false)
	out := make([]string, 0, len(state.Blobs))
	for _, b := range state.Blobs {
		out = append(out, b.SHA256)
	}
	return out
}

// GC keeps what the tree and the last known good set require, so a revert
// always finds its blobs, and drops a blob once neither needs it.
func TestGCKeepsTheBlobsOfTheTreeAndTheLastKnownGoodSet(t *testing.T) {
	h, dir := blobHarness(t, 1<<20)
	v1 := h.putBlob("db v1")
	age(t, dir)
	files := baseFiles("global\n  # " + v1 + "\n")
	m := buildManifest("plan-1", files)
	m.Mode = api.ModeReload
	m.RequiredBlobs = []string{v1}
	first := h.apply(&m, files)
	require.True(t, first.OK, "%+v", first.Error)

	v2 := h.putBlob("db v2")
	h.putBlob("never required")
	age(t, dir)
	files = baseFiles("global\n  # " + v2 + "\n")
	m = buildManifest("plan-2", files)
	m.ExpectedPrevPlanID = first.AppliedPlanID
	m.ExpectedPrevToken = first.AppliedToken
	m.RequiredBlobs = []string{v2}
	second := h.apply(&m, files)
	require.True(t, second.OK, "%+v", second.Error)
	require.Equal(t, "plan-1", second.LKGPlanID, "a file-only apply does not promote")
	assert.ElementsMatch(t, []string{v1, v2}, h.heldDigests(),
		"the last known good set's blob stays; the one nothing requires goes")
	persisted := h.persisted()
	assert.Equal(t, []any{v2}, persisted["tree_blobs"])
	assert.Equal(t, []any{v1}, persisted["lkg_blobs"])

	revert := buildManifest(second.AppliedPlanID, nil)
	revert.Mode = api.ModeRevertLKG
	revert.Token = api.Token{LeaderEpoch: 1, RenderSeq: 3}
	reverted := h.apply(&revert, nil)
	require.True(t, reverted.OK, "%+v", reverted.Error)
	assert.Equal(t, []string{v1}, h.heldDigests(), "the revert landed v1's configuration; v2 went")

	v3 := h.putBlob("db v3")
	age(t, dir)
	files = baseFiles("global\n  # " + v3 + "\n")
	m = buildManifest("plan-3", files)
	m.Mode = api.ModeReload
	m.ExpectedPrevPlanID = reverted.AppliedPlanID
	m.ExpectedPrevToken = reverted.AppliedToken
	m.Token = api.Token{LeaderEpoch: 1, RenderSeq: 4}
	m.RequiredBlobs = []string{v3}
	third := h.apply(&m, files)
	require.True(t, third.OK, "%+v", third.Error)
	require.Equal(t, "plan-3", third.LKGPlanID)
	assert.Equal(t, []string{v3}, h.heldDigests(), "a reload made plan-3 the last known good set")
	assert.Zero(t, h.metric("haptic_agent_invariant_violations_total"), h.violations())
}
