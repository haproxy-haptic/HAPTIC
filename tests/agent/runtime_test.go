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

//go:build agentdocker

package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
)

// converged brings a fresh pod onto the rendered configuration so a test can
// start from a known running plan.
func converged(t *testing.T) (*env, *session) {
	t.Helper()
	e := newEnv(t)
	s := newSession(e)
	result := s.apply(s.next(api.ModeReload), s.allParts())
	require.True(t, result.OK, "first apply was rejected: %+v", result.Error)
	e.waitForReady(http.StatusOK)
	return e, s
}

// TestRuntimeNamesAreTheManifestPaths pins the convention the whole op surface
// rests on: under `default-path origin`, HAProxy names a map, a certificate and
// a crt-list by the literal base-relative string the config references, which is
// the manifest File.Path and therefore the Op.Path. No translation anywhere.
func TestRuntimeNamesAreTheManifestPaths(t *testing.T) {
	e, _ := converged(t)

	loaded := e.worker("show map")
	assert.Contains(t, loaded, "("+hostMapPath+")")
	assert.Contains(t, loaded, "("+noteMapPath+")")
	assert.NotContains(t, loaded, baseDir+"/"+hostMapPath,
		"an absolute runtime name would mean the agent has to translate paths")

	assert.Contains(t, e.worker("show ssl cert"), defaultCertPath)
	assert.Contains(t, e.worker("show ssl crt-list"), crtListPath)
}

func TestMapOpsRunAtRuntimeAndKeepEveryByte(t *testing.T) {
	e, s := converged(t)
	worker := e.workerPID()

	// Values with a space and a ';': the CLI's line form truncates at the space
	// and executes everything past the ';' as a second command, so only the
	// payload form (map_add) can store these — map_set is for line-safe values
	// and a changed unsafe value travels as map_del + map_add, exactly as
	// deployplan composes it.
	const changed = "changed; value with spaces"
	const added = "added; another value"
	const retouched = "first-value-retouched"
	s.set(noteMapPath, "a.example.com "+retouched+"\nb.example.com "+changed+"\nd.example.com "+added+"\n")

	m := s.next(api.ModeAuto)
	m.Ops = []api.Op{
		{Kind: api.OpMapSet, Path: noteMapPath, Key: "a.example.com", Value: retouched},
		{Kind: api.OpMapDel, Path: noteMapPath, Key: "b.example.com"},
		{Kind: api.OpMapAdd, Path: noteMapPath, Key: "b.example.com", Value: changed},
		{Kind: api.OpMapAdd, Path: noteMapPath, Key: "d.example.com", Value: added},
		{Kind: api.OpMapDel, Path: noteMapPath, Key: "c.example.com"},
	}
	result := s.apply(m, s.allParts())
	require.True(t, result.OK, "map apply was rejected: %+v", result.Error)
	assert.Equal(t, api.ResultRuntime, result.Mode)
	assert.Equal(t, worker, e.workerPID(), "a map-only apply must not reload")
	assert.Equal(t, result.PlanID, result.AppliedPlanID)

	entries := mapEntries(e.worker("show map " + noteMapPath))
	assert.Equal(t, map[string]string{
		"a.example.com": retouched,
		"b.example.com": changed,
		"d.example.com": added,
	}, entries)

	_, header, _ := e.requestWithHost("b.example.com", notePath)
	assert.Equal(t, changed, header.Get("x-note"), "the running worker must serve the new value")
	assert.Equal(t, s.files[noteMapPath], e.read(noteMapPath), "the file on disk must match the desired set")
}

// A versioned replace pushes the entries the plan declares, never the file's
// own bytes: HAProxy's payload parser has no comment syntax, so a '#' header
// would be stored as a key, and a blank line ends the default payload block.
func TestMapReplaceInstallsExactlyTheEntries(t *testing.T) {
	e, s := converged(t)
	worker := e.workerPID()

	// The shape every chart-rendered map has: a header, blank lines between
	// the per-library snippets, and a TAB-separated entry.
	s.set(noteMapPath, "# host to note mapping\n\n"+
		"a.example.com first value\n\n"+
		"# ingress/default\n"+
		"b.example.com replaced; value\n"+
		"c.example.com\tthird value\n")
	m := s.next(api.ModeAuto)
	m.Ops = []api.Op{{Kind: api.OpMapReplace, Path: noteMapPath}}
	result := s.apply(m, s.allParts())

	require.True(t, result.OK, "map_replace was rejected: %+v", result.Error)
	assert.Equal(t, api.ResultRuntime, result.Mode)
	assert.Equal(t, worker, e.workerPID(), "a map replace must not reload")
	assert.Equal(t, map[string]string{
		"a.example.com": "first value",
		"b.example.com": "replaced; value",
		"c.example.com": "third value",
	}, mapEntries(e.worker("show map "+noteMapPath)), "the running map must be the plan's entries")

	_, header, _ := e.requestWithHost("b.example.com", notePath)
	assert.Equal(t, "replaced; value", header.Get("x-note"), "the running worker must serve the new value")

	// The read-back compares that same file against `show map`; a divergence
	// would reload, so an unchanged worker pid is what proves they agree.
	state, err := e.client.State(context.Background(), api.StateRead{})
	require.NoError(t, err)
	assert.Equal(t, result.PlanID, state.AppliedPlanID)
	assert.Equal(t, worker, e.workerPID(), "the read-back found no divergence")
}

func TestServerOpsRunAtRuntime(t *testing.T) {
	e, s := converged(t)
	worker := e.workerPID()

	added := s.next(api.ModeAuto)
	added.Ops = []api.Op{
		{
			Kind: api.OpServerAdd, Backend: "be-1", Server: "srv2",
			Address: "127.0.0.1", Port: upstreamPort,
			Keywords: []api.KeywordArg{{Name: "check"}, {Name: "weight", Args: []string{"10"}}},
		},
		{Kind: api.OpServerEnable, Backend: "be-1", Server: "srv2", Health: true},
		{
			Kind: api.OpServerAdd, Backend: "be-1", Server: "srv0",
			Address: "127.0.0.1", Port: upstreamPort,
			Keywords: []api.KeywordArg{{Name: "weight", Args: []string{"0"}}},
		},
		{Kind: api.OpServerEnable, Backend: "be-1", Server: "srv0"},
	}
	result := s.apply(added, nil)
	require.True(t, result.OK, "server_add was rejected: %+v", result.Error)
	assert.Equal(t, api.ResultRuntime, result.Mode)
	assert.Equal(t, worker, e.workerPID(), "adding a server must not reload")
	assert.Equal(t, "10", e.mustServerRow("srv2")["weight"])
	// weight 0 must survive as a real weight: a zero-weighted backend takes no
	// traffic, and HAProxy would default a weightless add server to 1.
	assert.Equal(t, "0", e.mustServerRow("srv0")["weight"])

	weight := 5
	reweighted := s.next(api.ModeAuto)
	reweighted.Ops = []api.Op{{Kind: api.OpServerSetWeight, Backend: "be-1", Server: "srv2", Weight: &weight}}
	result = s.apply(reweighted, nil)
	require.True(t, result.OK, "server_set_weight was rejected: %+v", result.Error)
	assert.Equal(t, strconv.Itoa(weight), e.mustServerRow("srv2")["weight"])

	// HAProxy answers `set server addr` at WARNING severity both when it
	// changed something and when it refused; the words decide, and "nothing
	// changed" is not a refusal.
	moved := s.next(api.ModeAuto)
	moved.Ops = []api.Op{{Kind: api.OpServerSetAddr, Backend: "be-1", Server: "srv2", Address: "127.0.0.2", Port: upstreamPort}}
	result = s.apply(moved, nil)
	require.True(t, result.OK, "server_set_addr was rejected: %+v", result.Error)
	assert.Equal(t, "127.0.0.2:"+strconv.Itoa(upstreamPort), e.mustServerRow("srv2")["addr"])
	unchanged := s.next(api.ModeAuto)
	unchanged.Ops = moved.Ops
	result = s.apply(unchanged, nil)
	require.True(t, result.OK, "an address the server already has was rejected: %+v", result.Error)
	assert.Equal(t, api.ResultRuntime, result.Mode)

	drained := s.next(api.ModeAuto)
	drained.Ops = []api.Op{{Kind: api.OpServerSetState, Backend: "be-1", Server: "srv2", State: "maint"}}
	result = s.apply(drained, nil)
	require.True(t, result.OK, "server_set_state was rejected: %+v", result.Error)
	assert.Contains(t, e.mustServerRow("srv2")["status"], "MAINT")

	// An idle keep-alive client holds a connection to the frontend while the
	// server is removed: `disable server` empties the idle pool, so the wait
	// returns instead of expiring.
	idle := keepAliveClient(t, e)
	defer idle.CloseIdleConnections()

	removed := s.next(api.ModeAuto)
	removed.Ops = []api.Op{
		{Kind: api.OpServerDisable, Backend: "be-1", Server: "srv2"},
		{Kind: api.OpServerWaitRemovable, Backend: "be-1", Server: "srv2", TimeoutMs: 3000},
		{Kind: api.OpServerDel, Backend: "be-1", Server: "srv2"},
	}
	result = s.apply(removed, nil)
	require.True(t, result.OK, "the delete sequence was rejected: %+v", result.Error)
	waitFor(t, "the deferred server delete", time.Second, func() error {
		state, err := e.client.State(t.Context(), api.StateRead{})
		if err != nil {
			return err
		}
		_, present := e.statRow("be-1", "srv2")
		if present || len(state.PendingDeletes.Servers) != 0 {
			return errors.New("srv2 deletion has not completed")
		}
		return nil
	})
	assert.Equal(t, worker, e.workerPID(), "the whole server lifecycle must stay reload-free")
}

// keepAliveClient leaves one pooled connection to the HTTP frontend open, the
// state a real client keeps between requests.
func keepAliveClient(t *testing.T, e *env) *http.Client {
	t.Helper()
	c := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{MaxIdleConnsPerHost: 2},
	}
	for range 4 {
		resp, err := c.Get(e.httpURL("/"))
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}
	return c
}

func TestReloadRetiresDeferredBackendDeletes(t *testing.T) {
	requireDynamicBackends(t)
	e, s := converged(t)
	worker := e.workerPID()
	added := s.next(api.ModeAuto)
	added.Ops = []api.Op{
		{Kind: api.OpBackendAdd, Backend: "retiring", Profile: defaultsProfile, Mode: "http"},
		{Kind: api.OpServerAdd, Backend: "retiring", Server: "srv1", Address: "127.0.0.1", Port: upstreamPort},
	}
	require.True(t, s.apply(added, nil).OK)
	removed := s.next(api.ModeAuto)
	removed.Ops = []api.Op{{Kind: api.OpBackendDel, Backend: "retiring"}}
	require.True(t, s.apply(removed, nil).OK)
	metricsURL := fmt.Sprintf("http://%s:%d/metrics", connectHost(), publishedPort(t, e.agent, metricsPort))
	waitFor(t, "a backend waiting for its server removal", convergeBudget, func() error {
		status, body, err := e.get(metricsURL)
		if err != nil {
			return err
		}
		if status != http.StatusOK || !strings.Contains(body, `haptic_agent_deferred_deletes_total{kind="backend",outcome="deferred"}`) {
			return errors.New("backend deletion has not deferred")
		}
		return nil
	})
	result := s.apply(s.next(api.ModeReload), nil)
	require.True(t, result.OK, "%+v", result.Error)
	require.NotEqual(t, worker, e.workerPID())
	state, err := e.client.State(t.Context(), api.StateRead{})
	require.NoError(t, err)
	assert.Empty(t, state.PendingDeletes.Backends)
	status, metrics, err := e.get(metricsURL)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, metrics, "haptic_agent_deferred_deletes_total{kind=\"backend\",outcome=\"superseded\"} 1\n")
	assert.Contains(t, metrics, "haptic_agent_deferred_deletes_total{kind=\"backend\",outcome=\"abandoned\"} 0\n")
	assert.NotContains(t, e.logs(e.agent), "giving up on a deferred")
}

func TestDynamicBackendLifecycle(t *testing.T) {
	requireDynamicBackends(t)
	e, s := converged(t)
	worker := e.workerPID()

	const host = "b3.example.com"
	s.set(hostMapPath, hostMapContent+host+" be-3\n")
	published := s.next(api.ModeAuto)
	published.Ops = []api.Op{
		{Kind: api.OpBackendAdd, Backend: "be-3", Profile: defaultsProfile, Mode: "http", GUID: "be:be-3"},
		{
			Kind: api.OpServerAdd, Backend: "be-3", Server: "srv1",
			Address: "127.0.0.1", Port: upstreamPort,
			Keywords: []api.KeywordArg{{Name: "check"}},
		},
		{Kind: api.OpServerEnable, Backend: "be-3", Server: "srv1", Health: true},
		{Kind: api.OpBackendPublish, Backend: "be-3"},
		{Kind: api.OpMapAdd, Path: hostMapPath, Key: host, Value: "be-3"},
	}
	result := s.apply(published, s.allParts())
	require.True(t, result.OK, "publishing a dynamic backend was rejected: %+v", result.Error)
	assert.Equal(t, api.ResultRuntime, result.Mode)
	assert.Equal(t, worker, e.workerPID())

	status, _, body := e.requestWithHost(host, "/")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "upstream-ok", body, "traffic must reach the dynamic backend")

	s.set(hostMapPath, hostMapContent)
	retired := s.next(api.ModeAuto)
	retired.Ops = []api.Op{
		{Kind: api.OpMapDel, Path: hostMapPath, Key: host},
		{Kind: api.OpBackendUnpublish, Backend: "be-3"},
		{Kind: api.OpServerDisable, Backend: "be-3", Server: "srv1"},
		{Kind: api.OpServerWaitRemovable, Backend: "be-3", Server: "srv1", TimeoutMs: 3000},
		{Kind: api.OpServerDel, Backend: "be-3", Server: "srv1"},
		{Kind: api.OpBackendWaitRemovable, Backend: "be-3", TimeoutMs: 3000},
		{Kind: api.OpBackendDel, Backend: "be-3"},
	}
	result = s.apply(retired, s.allParts())
	require.True(t, result.OK, "retiring a dynamic backend was rejected: %+v", result.Error)
	assert.Equal(t, worker, e.workerPID())
	// The wait + del tail runs off the apply path (deferred deletes), so the
	// backend disappears shortly after the ACK, never inside it.
	waitFor(t, "the deferred delete of be-3", convergeBudget, func() error {
		if _, present := e.statRow("be-3", "BACKEND"); present {
			return errors.New("be-3 is still in show stat")
		}
		return nil
	})
	assert.Equal(t, worker, e.workerPID(), "deferred deletes must not reload")
	state, err := e.client.State(context.Background(), api.StateRead{})
	require.NoError(t, err)
	assert.Empty(t, state.PendingDeletes.Servers)
	assert.Empty(t, state.PendingDeletes.Backends)
}

func TestBackendDeletionWithSaturatedHAProxyMessages(t *testing.T) {
	requireDynamicBackends(t)
	e := newEnv(t)
	s := newSession(e)
	s.set(configPath, strings.Replace(renderedConfig, "global\n", "global\n    nbthread 1\n", 1))
	require.True(t, s.apply(s.next(api.ModeReload), s.allParts()).OK)
	worker := e.workerPID()
	added := s.next(api.ModeAuto)
	added.Ops = []api.Op{{Kind: api.OpBackendAdd, Backend: "retiring", Profile: defaultsProfile, Mode: "http"}}
	// 65 server deletion notices fill HAProxy 3.4's 1024-byte thread-local message buffer.
	const servers = 65
	deletes := make([]api.Op, 0, servers+1)
	for i := range servers {
		name := fmt.Sprintf("srv%d", i)
		added.Ops = append(added.Ops, api.Op{
			Kind: api.OpServerAdd, Backend: "retiring", Server: name, Address: "127.0.0.1", Port: upstreamPort,
		})
		deletes = append(deletes, api.Op{Kind: api.OpServerDel, Backend: "retiring", Server: name})
	}
	deletes = append(deletes, api.Op{Kind: api.OpBackendDel, Backend: "retiring"})
	require.True(t, s.apply(added, nil).OK)
	removed := s.next(api.ModeAuto)
	removed.Ops = deletes
	require.True(t, s.apply(removed, nil).OK)
	waitFor(t, "confirmed backend deletion", convergeBudget, func() error {
		state, err := e.client.State(t.Context(), api.StateRead{})
		if err != nil {
			return err
		}
		if len(state.PendingDeletes.Servers)+len(state.PendingDeletes.Backends) != 0 {
			return errors.New("deletions are still pending")
		}
		return nil
	})
	assert.Equal(t, worker, e.workerPID())
	assert.NotContains(t, e.worker("show backend"), "retiring")
	metricsURL := fmt.Sprintf("http://%s:%d/metrics", connectHost(), publishedPort(t, e.agent, metricsPort))
	status, metrics, err := e.get(metricsURL)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, metrics, "haptic_agent_deferred_deletes_total{kind=\"server\",outcome=\"done\"} 65\n")
	assert.Contains(t, metrics, "haptic_agent_deferred_deletes_total{kind=\"backend\",outcome=\"done\"} 1\n")
	assert.Contains(t, metrics, "haptic_agent_deferred_deletes_total{kind=\"backend\",outcome=\"abandoned\"} 0\n")
	assert.NotContains(t, metrics, "haptic_agent_deferred_deletes_total{kind=\"backend\",outcome=\"deferred\"}")
}

// TestDynamicBackendRenameKeepsTraffic pins the A4 fixed ordering: a single
// apply that both creates a backend and retires another — what a backend rename
// composes — publishes and re-points the map at the new backend before it
// unpublishes the old one, so a request in flight the whole time never 503s.
// The agent's own Split runs the create/publish/map/unpublish half in one
// worker session and defers only the drain+delete tail, which is what makes the
// make-before-break hold.
func TestDynamicBackendRenameKeepsTraffic(t *testing.T) {
	requireDynamicBackends(t)
	e, s := converged(t)
	worker := e.workerPID()

	const host = "rename.example.com"
	s.set(hostMapPath, hostMapContent+host+" be-old\n")
	published := s.next(api.ModeAuto)
	published.Ops = []api.Op{
		{Kind: api.OpBackendAdd, Backend: "be-old", Profile: defaultsProfile, Mode: "http", GUID: "be:be-old"},
		{Kind: api.OpServerAdd, Backend: "be-old", Server: "srv1", Address: "127.0.0.1", Port: upstreamPort,
			Keywords: []api.KeywordArg{{Name: "check"}}},
		{Kind: api.OpServerEnable, Backend: "be-old", Server: "srv1", Health: true},
		{Kind: api.OpBackendPublish, Backend: "be-old"},
		{Kind: api.OpMapAdd, Path: hostMapPath, Key: host, Value: "be-old"},
	}
	require.True(t, s.apply(published, s.allParts()).OK, "publishing the first backend was rejected")
	status, _, body := e.requestWithHost(host, "/")
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "upstream-ok", body)

	// The rename in one apply, ops in the A4 order: create + publish the new
	// backend and re-point the map at it, THEN unpublish and drain the old one.
	s.set(hostMapPath, hostMapContent+host+" be-new\n")
	rename := s.next(api.ModeAuto)
	rename.Ops = []api.Op{
		{Kind: api.OpBackendAdd, Backend: "be-new", Profile: defaultsProfile, Mode: "http", GUID: "be:be-new"},
		{Kind: api.OpServerAdd, Backend: "be-new", Server: "srv1", Address: "127.0.0.1", Port: upstreamPort,
			Keywords: []api.KeywordArg{{Name: "check"}}},
		{Kind: api.OpServerEnable, Backend: "be-new", Server: "srv1", Health: true},
		{Kind: api.OpBackendPublish, Backend: "be-new"},
		{Kind: api.OpMapSet, Path: hostMapPath, Key: host, Value: "be-new"},
		{Kind: api.OpBackendUnpublish, Backend: "be-old"},
		{Kind: api.OpServerDisable, Backend: "be-old", Server: "srv1"},
		{Kind: api.OpServerWaitRemovable, Backend: "be-old", Server: "srv1", TimeoutMs: 3000},
		{Kind: api.OpServerDel, Backend: "be-old", Server: "srv1"},
		{Kind: api.OpBackendWaitRemovable, Backend: "be-old", TimeoutMs: 3000},
		{Kind: api.OpBackendDel, Backend: "be-old"},
	}

	probe := newHostProbe(e, host)
	probe.waitLive(t, 3) // the apply is milliseconds; make sure traffic is flowing first
	result := s.apply(rename, s.allParts())
	observed := probe.stop()

	require.True(t, result.OK, "the rename apply was rejected: %+v", result.Error)
	assert.Equal(t, api.ResultRuntime, result.Mode)
	assert.Equal(t, worker, e.workerPID(), "a runtime rename must not reload")
	assert.Zero(t, observed.failures,
		"%d of %d requests dropped during the rename (first: %s); create-and-publish must precede unpublish-and-delete",
		observed.failures, observed.attempts, observed.first)
	assert.Greater(t, observed.attempts, 3, "the probe stopped before the rename overlapped it")

	// The old backend's drain runs off the apply path; the new one keeps serving.
	waitFor(t, "the deferred delete of be-old", convergeBudget, func() error {
		if _, present := e.statRow("be-old", "BACKEND"); present {
			return errors.New("be-old is still in show stat")
		}
		return nil
	})
	status, _, body = e.requestWithHost(host, "/")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "upstream-ok", body, "the renamed-to backend must serve the host")
	assert.Equal(t, worker, e.workerPID(), "the deferred delete must not reload")
}

// TestNameCollisionFallsBackToReload pins A5: `add backend` on a name HAProxy
// already knows answers "already used by other proxy". No runtime command
// reveals a backend's shape, so the agent cannot know the leftover is safe to
// publish traffic onto — it stops and reloads the desired file set instead. The
// apply is an ACK (mode reload), not a NACK, and its op results carry HAProxy's
// own words so the controller can label the fallback.
func TestNameCollisionFallsBackToReload(t *testing.T) {
	requireDynamicBackends(t)
	e, s := converged(t)
	worker := e.workerPID()

	// be-1 is defined in the rendered config, so adding it again collides.
	collide := s.next(api.ModeAuto)
	collide.Ops = []api.Op{
		{Kind: api.OpBackendAdd, Backend: "be-1", Profile: defaultsProfile, Mode: "http"},
	}
	// Read the first ACK directly: session.apply would follow the paced reload,
	// whose fresh run carries no op results.
	result, _ := s.timedApply(collide, nil)

	require.True(t, result.OK, "a name collision must fall back to a reload, not a NACK: %+v", result.Error)
	require.Nil(t, result.Error, "a fallback reload is an ACK with no error: the controller only counts the fallback when OK and Error is nil")
	require.NotEqual(t, api.ResultRuntime, result.Mode, "the colliding op must not report a runtime success")

	failed := failedOpResult(result.OpResults)
	require.NotNil(t, failed, "the ACK carried no failed op result:\n%+v", result.OpResults)
	assert.Equal(t, api.OpBackendAdd, failed.Kind)
	assert.Contains(t, strings.ToLower(failed.Output), "already used by other proxy")

	// The fallback reloads the desired set: the worker is re-executed.
	waitFor(t, "the fallback reload", convergeBudget, func() error {
		if e.workerPID() == worker {
			return errors.New("the worker has not reloaded yet")
		}
		return nil
	})
	e.waitForReady(http.StatusOK)
}

func failedOpResult(results []api.OpResult) *api.OpResult {
	for i := range results {
		if !results[i].OK {
			return &results[i]
		}
	}
	return nil
}

// hostProbe keeps a request to one host in flight for the length of an apply,
// so a test can prove the change dropped no traffic. A non-200 or a transport
// error is a failure; it never calls t from its own goroutine.
type hostProbe struct {
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	result probeResult
}

type probeResult struct {
	attempts int
	failures int
	first    string
}

func newHostProbe(e *env, host string) *hostProbe {
	ctx, cancel := context.WithCancel(context.Background())
	p := &hostProbe{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		// The request carries no context: a call already in flight when stop
		// cancels still completes and is counted, so a dropped answer is never
		// mistaken for the probe simply ending. The loop exits at the top.
		for ctx.Err() == nil {
			req, err := http.NewRequest(http.MethodGet, e.httpURL("/"), http.NoBody)
			if err != nil {
				return
			}
			req.Host = host
			resp, err := e.http.Do(req)
			p.mu.Lock()
			p.result.attempts++
			switch {
			case err != nil:
				p.result.failures++
				if p.result.first == "" {
					p.result.first = err.Error()
				}
			case resp.StatusCode != http.StatusOK:
				p.result.failures++
				if p.result.first == "" {
					p.result.first = "status " + strconv.Itoa(resp.StatusCode)
				}
			}
			p.mu.Unlock()
			if resp != nil {
				_ = resp.Body.Close()
			}
		}
	}()
	return p
}

// waitLive blocks until the probe has completed at least n requests, so a test
// that follows with a millisecond-long apply is sure traffic is already flowing
// when it starts.
func (p *hostProbe) waitLive(t *testing.T, n int) {
	t.Helper()
	waitFor(t, fmt.Sprintf("%d probe requests", n), convergeBudget, func() error {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.result.failures > 0 {
			return fmt.Errorf("the probe failed before the apply: %s", p.result.first)
		}
		if p.result.attempts < n {
			return fmt.Errorf("only %d of %d probe requests so far", p.result.attempts, n)
		}
		return nil
	})
}

func (p *hostProbe) stop() probeResult {
	p.cancel()
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.result
}

func TestLargeMapDeletionCompletesBeforeReadBack(t *testing.T) {
	e, s := converged(t)
	var content strings.Builder
	ops := make([]api.Op, api.MaxOpsPerApply+1)
	for i := range ops {
		key := fmt.Sprintf("key-%d", i)
		fmt.Fprintf(&content, "%s value\n", key)
		ops[i] = api.Op{Kind: api.OpMapDel, Path: noteMapPath, Key: key}
	}
	s.set(noteMapPath, content.String())
	require.True(t, s.apply(s.next(api.ModeReload), s.allParts()).OK)
	worker := e.workerPID()
	s.set(noteMapPath, "")
	manifest := s.next(api.ModeAuto)
	manifest.Ops = ops[:api.MaxOpsPerApply]
	manifest.OpBatches = [][]api.Op{ops[api.MaxOpsPerApply:]}
	result := s.apply(manifest, s.allParts())
	require.True(t, result.OK, "%+v", result.Error)
	assert.Equal(t, api.ResultRuntime, result.Mode)
	assert.Len(t, result.OpResults, len(ops))
	assert.Empty(t, mapEntries(e.worker("show map "+noteMapPath)))
	assert.Equal(t, worker, e.workerPID())
	assert.Equal(t, "", e.read(noteMapPath))
}
