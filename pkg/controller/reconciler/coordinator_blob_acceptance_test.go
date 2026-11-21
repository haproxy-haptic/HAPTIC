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

package reconciler

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/blob"
	"github.com/haproxy-haptic/HAPTIC/pkg/blobstore"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
)

// newBlobAcceptanceHarness renders "db=<blob path>" followed by the route's suffix.
func newBlobAcceptanceHarness(t *testing.T, blobURL string, subscribe ...func(*busevents.EventBus)) (*acceptanceHarness, *blobstore.Store) {
	t.Helper()
	cfg := acceptanceConfig()
	cfg.TemplateSnippets = map[string]config.TemplateSnippet{
		"routes": {
			Name:        "routes",
			Requires:    []string{"routes"},
			Incremental: &config.IncrementalTemplate{Source: "routes"},
			Template:    `{{ item | dig_string("", "spec", "suffix") }}`,
		},
	}
	cfg.HAProxyConfig = config.HAProxyConfig{Template: `{%- var db, _ = blob.Fetch("` + blobURL +
		`", map[string]any{"retries": 1, "timeout": "1s"}) -%}` + "\ndb={{ db.path }}{{ render \"routes\" }}"}
	h := newAcceptanceHarnessWithConfig(t, blobURL, cfg, subscribe...)
	store, err := blobstore.New(t.TempDir(), blob.DefaultMaxSize, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	h.component.EnableBlobs(store)
	return h, store
}

func TestRefusedBlobIsRevokedThroughTheCoordinator(t *testing.T) {
	database := []byte("database v1")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(database)
	}))
	t.Cleanup(server.Close)
	ref := blob.Describe(database)
	var observed <-chan busevents.Event
	h, store := newBlobAcceptanceHarness(t, server.URL, func(bus *busevents.EventBus) {
		observed = bus.Subscribe("blob-revocations", 64)
	})

	h.reconcile("start")
	assert.Equal(t, "db=\n", h.deployedFor("start").config, "a deploying render withholds the pending blob")
	accepted := h.deployedFor("http_content_accepted")
	require.Equal(t, "db="+ref.Path()+"\n", accepted.config)
	assert.True(t, store.Has(ref.Digest))
	h.waitForAttempts(0)
	attempts := h.coordinator.ledger.attempted.Load()

	h.verdict(accepted, false)

	revoked := testutil.WaitForEvent[*events.BlobContentRevokedEvent](t, observed, testutil.EventTimeout)
	assert.Equal(t, ref.Digest, revoked.Digest)
	assert.False(t, revoked.Restored)
	assert.Equal(t, "db=\n", h.deployedFor("http_content_revoked").config, "the render without the revoked blob deploys")
	h.waitForAttempts(attempts)
	h.reconcile("same inputs")
	assert.Equal(t, "db=\n", h.deployedFor("same inputs").config, "the refused output is not accepted again against the same inputs")

	require.NoError(t, h.routes.Update(h.route(server.URL, "!"), []string{"default", "a"}))
	h.reconcile("inputs change")
	assert.Equal(t, "db=!\n", h.deployedFor("inputs change").config)
	assert.Equal(t, "db="+ref.Path()+"!\n", h.deployedFor("http_content_accepted").config,
		"new inputs let the same blob be accepted again")
}

func TestConfirmedBlobSurvivesALaterRefusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("database"))
	}))
	t.Cleanup(server.Close)
	ref := blob.Describe([]byte("database"))
	h, _ := newBlobAcceptanceHarness(t, server.URL)

	h.reconcile("start")
	h.deployedFor("start")
	accepted := h.deployedFor("http_content_accepted")
	h.waitForAttempts(0)
	window, recorded := h.coordinator.ledger.window(accepted.occurrence)
	require.True(t, recorded)
	require.NotEmpty(t, window.observations, "a blob read is exact confirmation evidence")
	h.verdict(accepted, true)

	require.NoError(t, h.routes.Update(h.route(server.URL, "!"), []string{"default", "a"}))
	h.reconcile("broken change")
	refused := h.deployedFor("broken change")
	require.Equal(t, "db="+ref.Path()+"!\n", refused.config)
	h.verdict(refused, false)

	h.reconcile("next change")
	assert.Equal(t, "db="+ref.Path()+"!\n", h.deployedFor("next change").config)
	assert.Equal(t, []string{ref.Digest}, blob.ReferencedDigests(accepted.config))
}
