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

package deployer

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/blob"
	"github.com/haproxy-haptic/HAPTIC/pkg/blobstore"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	agentclient "github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/client"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
)

// blobPod is a real agent storing blobs, reached through a proxy that records
// every call and can misreport the blobs the agent holds.
type blobPod struct {
	endpoint dataplane.Endpoint
	blobDir  string
	proxy    *agentProxy
}

func startBlobPod(t *testing.T, name string, storesBlobs bool) *blobPod {
	t.Helper()
	blobDir := ""
	if storesBlobs {
		blobDir = t.TempDir()
	}
	endpoint, _ := startRealAgentWith(t, name, blobDir)
	proxy := newAgentProxy(t, endpoint.URL)
	endpoint.URL = proxy.server.URL
	return &blobPod{endpoint: endpoint, blobDir: blobDir, proxy: proxy}
}

func (p *blobPod) holds(t *testing.T, ref blob.Ref) bool {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(p.blobDir, ref.Digest))
	return err == nil && blob.Describe(content) == ref
}

// agentProxy forwards to an agent. claimHeld is added to every state the
// agent reports; with swallowPuts the proxy answers blob uploads itself.
type agentProxy struct {
	server *httptest.Server

	mu          sync.Mutex
	calls       []string
	claimHeld   []string
	swallowPuts bool
}

func newAgentProxy(t *testing.T, target string) *agentProxy {
	t.Helper()
	upstream, err := url.Parse(target)
	require.NoError(t, err)
	p := &agentProxy{}
	reverse := httputil.NewSingleHostReverseProxy(upstream)
	reverse.ModifyResponse = p.rewriteState
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.calls = append(p.calls, r.Method+" "+r.URL.Path)
		swallow := p.swallowPuts && r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, api.PathBlobs+"/")
		p.mu.Unlock()
		if swallow {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
			return
		}
		reverse.ServeHTTP(w, r)
	}))
	t.Cleanup(p.server.Close)
	return p
}

func (p *agentProxy) rewriteState(resp *http.Response) error {
	p.mu.Lock()
	claimed := append([]string(nil), p.claimHeld...)
	p.mu.Unlock()
	if resp.Request.URL.Path != api.PathState || len(claimed) == 0 || resp.StatusCode != http.StatusOK {
		return nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var state api.State
	if err := json.Unmarshal(body, &state); err != nil {
		return err
	}
	for _, digest := range claimed {
		state.Blobs = append(state.Blobs, api.Blob{SHA256: digest})
	}
	if body, err = json.Marshal(&state); err != nil {
		return err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Del("Content-Length")
	return nil
}

func (p *agentProxy) count(call string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, recorded := range p.calls {
		if recorded == call {
			n++
		}
	}
	return n
}

func (p *agentProxy) blobPuts() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, recorded := range p.calls {
		if strings.HasPrefix(recorded, http.MethodPut+" "+api.PathBlobs+"/") {
			n++
		}
	}
	return n
}

const applyCall = http.MethodPost + " " + api.PathApply

func newBlobStore(t *testing.T, contents ...string) (*blobstore.Store, []blob.Ref) {
	t.Helper()
	store, err := blobstore.New(t.TempDir(), blob.DefaultMaxSize, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	refs := make([]blob.Ref, 0, len(contents))
	for _, content := range contents {
		ref, err := store.Ingest(t.Context(), strings.NewReader(content), blob.Spec{Unpack: blob.UnpackNone})
		require.NoError(t, err)
		refs = append(refs, ref)
	}
	return store, refs
}

// renderNeeding is renderFor plus a general file that names each blob's path,
// the way a plugin's configuration names a database.
func renderNeeding(address string, refs ...blob.Ref) (*renderplan.Plan, string, *dataplane.AuxiliaryFiles) {
	plan, config, aux := renderFor("plan", address, mapEntry)
	var content strings.Builder
	for _, ref := range refs {
		content.WriteString("db = \"" + ref.Path() + "\"\n")
	}
	const path = "general/hub.toml"
	plan.Files = append(plan.Files, renderplan.File{
		Path: path, Kind: renderplan.FileKindGeneral,
		Digest: renderplan.DigestString(content.String()), Size: int64(content.Len()),
		Content: content.String(), ContentKnown: true,
	})
	noReload := false
	aux.GeneralFiles = append(aux.GeneralFiles, auxiliaryfiles.GeneralFile{Path: path, Content: content.String(), ReloadOnPush: &noReload})
	plan.ComputeID()
	return plan, config, aux
}

// blobDeployer is a deployer with a blob store and a bus that also keeps the
// per-pod failures.
type blobDeployer struct {
	component *Component
	bus       *deployerBus
	failures  <-chan busevents.Event
}

func newBlobDeployer(t *testing.T, store BlobStore) *blobDeployer {
	t.Helper()
	bus := testutil.NewTestBus()
	published := bus.Subscribe("deployer-test", 200)
	failures := bus.SubscribeTypes("deployer-failures", 20, events.EventTypeInstanceDeploymentFailed)
	bus.Start()
	d := &blobDeployer{bus: &deployerBus{EventBus: bus, Events: published}, failures: failures}
	d.component = createTestDeployer(bus)
	if store != nil {
		d.component.blobs = store
	}
	return d
}

func (d *blobDeployer) deploy(t *testing.T, address string, refs []blob.Ref, pods ...*blobPod) *events.DeploymentCompletedEvent {
	t.Helper()
	plan, config, aux := renderNeeding(address, refs...)
	endpoints := make([]dataplane.Endpoint, 0, len(pods))
	for _, pod := range pods {
		endpoints = append(endpoints, pod.endpoint)
	}
	return deployTo(t, d.component, d.bus, plan, config, aux, "config_validation", endpoints...)
}

func (d *blobDeployer) failure(t *testing.T) string {
	t.Helper()
	return testutil.WaitForEvent[*events.InstanceDeploymentFailedEvent](t, d.failures, testutil.EventTimeout).Error
}

// Every pod receives the blobs its configuration names before the apply that
// needs them, and only once: a later deployment finds them held. A pod that
// joins later gets them before its first apply.
func TestBlobs_DeliveredBeforeTheApplyThatNeedsThem(t *testing.T) {
	store, refs := newBlobStore(t, "city database")
	deployer := newBlobDeployer(t, store)
	pods := []*blobPod{startBlobPod(t, "haproxy-0", true), startBlobPod(t, "haproxy-1", true)}

	// One pod first: two in-process agents must not take their first reload at
	// once, because client-native's runtime client races on a package variable.
	first := deployer.deploy(t, "10.0.0.1", refs, pods[0])
	joined := deployer.deploy(t, "10.0.0.1", refs, pods...)
	changed := deployer.deploy(t, "10.0.0.2", refs, pods...)

	assert.Equal(t, 1, first.Succeeded)
	assert.Equal(t, 2, joined.Succeeded)
	assert.Equal(t, 2, changed.Succeeded)
	for _, pod := range pods {
		assert.True(t, pod.holds(t, refs[0]))
		assert.Equal(t, 1, pod.proxy.blobPuts(), "a pod that reports the blob is not sent it again")
	}
}

// A pod that loses a blob between its state read and the apply refuses the
// apply; the controller pushes what it names and applies again, once.
func TestBlobs_MissingBlobsRefusalPushesAndRetries(t *testing.T) {
	store, refs := newBlobStore(t, "city database")
	deployer := newBlobDeployer(t, store)
	pod := startBlobPod(t, "haproxy-0", true)
	pod.proxy.claimHeld = []string{refs[0].Digest}

	completed := deployer.deploy(t, "10.0.0.1", refs, pod)

	assert.Equal(t, 1, completed.Succeeded)
	assert.Equal(t, 1, pod.proxy.blobPuts())
	assert.Equal(t, 2, pod.proxy.count(applyCall))
	assert.True(t, pod.holds(t, refs[0]))
}

// The retry happens once per apply: an agent that still lacks the blob fails
// the pod instead of looping.
func TestBlobs_SecondMissingBlobsRefusalFailsThePod(t *testing.T) {
	store, refs := newBlobStore(t, "city database")
	deployer := newBlobDeployer(t, store)
	pod := startBlobPod(t, "haproxy-0", true)
	pod.proxy.claimHeld = []string{refs[0].Digest}
	pod.proxy.swallowPuts = true

	completed := deployer.deploy(t, "10.0.0.1", refs, pod)

	assert.Equal(t, 1, completed.Failed)
	assert.Equal(t, 2, pod.proxy.count(applyCall))
	assert.Contains(t, deployer.failure(t), refs[0].Digest)
}

// An agent that cannot store blobs never receives a configuration naming one:
// the pod keeps what it runs.
func TestBlobs_AgentWithoutBlobsIsNotSentTheApply(t *testing.T) {
	store, refs := newBlobStore(t, "city database")
	deployer := newBlobDeployer(t, store)
	pod := startBlobPod(t, "haproxy-0", false)

	completed := deployer.deploy(t, "10.0.0.1", refs, pod)

	assert.Equal(t, 1, completed.Failed)
	assert.Zero(t, pod.proxy.count(applyCall))
	assert.Contains(t, deployer.failure(t), "blob")
}

// A controller that lost a blob takes it back from a pod that reports it, so
// a pod that lacks it can still be brought to the configuration.
func TestBlobs_LostBlobIsTakenFromAPodThatHoldsIt(t *testing.T) {
	store, _ := newBlobStore(t)
	ref := blob.Describe([]byte("city database"))
	deployer := newBlobDeployer(t, store)
	holder, newcomer := startBlobPod(t, "haproxy-0", true), startBlobPod(t, "haproxy-1", true)
	client, err := agentclient.New(&agentclient.Config{
		BaseURL: holder.endpoint.URL, Username: holder.endpoint.Username, Password: holder.endpoint.Password,
	})
	require.NoError(t, err)
	t.Cleanup(client.Close)
	require.NoError(t, client.PutBlob(t.Context(), ref.Digest, ref.Size, strings.NewReader("city database")))
	// The holder takes its first reload alone (see the delivery test).
	require.Equal(t, 1, deployer.deploy(t, "10.0.0.1", []blob.Ref{ref}, holder).Succeeded)

	completed := deployer.deploy(t, "10.0.0.1", []blob.Ref{ref}, holder, newcomer)

	assert.Equal(t, 2, completed.Succeeded)
	assert.True(t, store.Has(ref.Digest), "the recovered blob is the controller's again")
	assert.True(t, newcomer.holds(t, ref))
	assert.Equal(t, 1, holder.proxy.count(http.MethodGet+" "+api.BlobPath(ref.Digest)))
}

// With the blob neither in the store nor on a pod, the pods that lack it keep
// their configuration and the failure names the blob.
func TestBlobs_UnavailableBlobFailsThePodsThatLackIt(t *testing.T) {
	store, _ := newBlobStore(t)
	ref := blob.Describe([]byte("city database"))
	deployer := newBlobDeployer(t, store)
	pod := startBlobPod(t, "haproxy-0", true)

	completed := deployer.deploy(t, "10.0.0.1", []blob.Ref{ref}, pod)

	assert.Equal(t, 1, completed.Failed)
	assert.Zero(t, pod.proxy.count(applyCall))
	assert.Contains(t, deployer.failure(t), ref.Digest)
}

// The digests the pods hold are kept on the controller's disk; a pod that left
// the fleet no longer holds any.
func TestBlobs_HeldBlobsAreWhatTheFleetHolds(t *testing.T) {
	store, refs := newBlobStore(t, "city database", "asn database")
	deployer := newBlobDeployer(t, store)
	first, second := startBlobPod(t, "haproxy-0", true), startBlobPod(t, "haproxy-1", true)

	deployer.deploy(t, "10.0.0.1", refs[:1], first)
	assert.Equal(t, []string{refs[0].Digest}, deployer.component.HeldBlobs())

	deployer.deploy(t, "10.0.0.1", refs[1:], second)
	assert.Equal(t, []string{refs[1].Digest}, deployer.component.HeldBlobs())
}

// Without a blob store the deployer delivers nothing and requires nothing, so
// a configuration that merely contains a blob path deploys as before.
func TestBlobs_NothingRequiredWithoutABlobStore(t *testing.T) {
	deployer := newBlobDeployer(t, nil)
	pod := startBlobPod(t, "haproxy-0", false)
	ref := blob.Describe([]byte("city database"))

	completed := deployer.deploy(t, "10.0.0.1", []blob.Ref{ref}, pod)

	assert.Equal(t, 1, completed.Succeeded)
	assert.Zero(t, pod.proxy.blobPuts())
}
