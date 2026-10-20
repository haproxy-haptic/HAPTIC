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
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/deployplan"
)

func testManifest() *api.Manifest {
	return &api.Manifest{
		PlanID:            "plan-2",
		PlanSchemaVersion: 1,
		Token:             api.Token{LeaderEpoch: 7, RenderSeq: 2},
		Mode:              api.ModeAuto,
		Files: []api.File{
			{Path: "haproxy.cfg", Digest: "d1", Size: 5, Kind: api.FileKindConfig, ReloadOnChange: true},
			{Path: "maps/host.map", Digest: "d2", Size: 3, Kind: api.FileKindMap},
		},
	}
}

func newTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	c, err := New(&Config{
		BaseURL:             baseURL,
		Username:            "admin",
		Password:            "adminpwd",
		Timeout:             5 * time.Second,
		PerPodApplyTimeout:  5 * time.Second,
		ConnectRetries:      api.ConnectRetries,
		ConnectRetryBackoff: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(c.Close)
	return c
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	require.NoError(t, json.NewEncoder(w).Encode(body))
}

func TestNewRejectsUnusableBaseURL(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "unix:///tmp/agent.sock", "http://"} {
		_, err := New(&Config{BaseURL: raw})
		assert.Error(t, err, "BaseURL %q", raw)
	}
}

func TestStateSuccess(t *testing.T) {
	t.Parallel()
	var gotPath, gotAuth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.RequestURI())
		user, pass, _ := r.BasicAuth()
		gotAuth.Store(user + ":" + pass)
		writeJSON(t, w, http.StatusOK, api.State{
			APIVersion:    api.Version,
			AgentVersion:  "0.2.0",
			AppliedPlanID: "plan-1",
			HAProxy:       api.HAProxyInfo{Version: "3.4.3", WorkerPID: 12},
		})
	}))
	defer srv.Close()

	state, err := newTestClient(t, srv.URL).State(context.Background(), api.StateRead{Verify: true})
	require.NoError(t, err)
	assert.Equal(t, "plan-1", state.AppliedPlanID)
	assert.Equal(t, "3.4.3", state.HAProxy.Version)
	assert.Equal(t, api.PathState+"?verify=1&plan=0", gotPath.Load(), "a read that holds the plan leaves the blob out")
	assert.Equal(t, "admin:adminpwd", gotAuth.Load())
}

// The query names what the read asks for: the blob is left out unless the
// caller needs it, and an agent that predates the flag ignores it.
func TestStateReadQuery(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "?plan=0", api.StateRead{}.Query())
	assert.Equal(t, "?verify=1&plan=0", api.StateRead{Verify: true}.Query())
	assert.Equal(t, "", api.StateRead{Plan: true}.Query())
	assert.Equal(t, "?verify=1", api.StateRead{Verify: true, Plan: true}.Query())
}

func TestStateUnauthorized(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).State(context.Background(), api.StateRead{})
	var httpErr *HTTPError
	require.ErrorAs(t, err, &httpErr)
	assert.Equal(t, http.StatusUnauthorized, httpErr.Status)
	assert.Contains(t, httpErr.Body, "unauthorized")
}

func TestApplySendsManifestThenPlanThenFilesInManifestOrder(t *testing.T) {
	t.Parallel()
	type received struct {
		name     string
		filename string
		content  string
	}
	var parts []received
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		require.NoError(t, err)
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			require.NoError(t, err)
			body, err := io.ReadAll(p)
			require.NoError(t, err)
			parts = append(parts, received{p.FormName(), p.FileName(), string(body)})
		}
		writeJSON(t, w, http.StatusOK, api.ApplyResult{PlanID: "plan-2", OK: true, Mode: api.ResultRuntime})
	}))
	defer srv.Close()

	result, err := newTestClient(t, srv.URL).Apply(context.Background(), testManifest(),
		map[string]io.Reader{
			"maps/host.map": strings.NewReader("a b"),
			"haproxy.cfg":   strings.NewReader("globa"),
		},
		strings.NewReader("zstd-blob"))
	require.NoError(t, err)
	assert.True(t, result.OK)
	assert.Equal(t, api.ResultRuntime, result.Mode)

	require.Len(t, parts, 4)
	assert.Equal(t, api.PartManifest, parts[0].name)
	assert.Equal(t, api.PartPlan, parts[1].name)
	assert.Equal(t, "zstd-blob", parts[1].content)
	assert.Equal(t, "haproxy.cfg", parts[2].name)
	assert.Equal(t, "haproxy.cfg", parts[2].filename)
	assert.Equal(t, "globa", parts[2].content)
	assert.Equal(t, "maps/host.map", parts[3].name)
	assert.Equal(t, "a b", parts[3].content)

	var manifest api.Manifest
	require.NoError(t, json.Unmarshal([]byte(parts[0].content), &manifest))
	assert.Equal(t, "plan-2", manifest.PlanID)
	assert.Equal(t, uint64(7), manifest.Token.LeaderEpoch)
}

func TestApplyConflict(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		writeJSON(t, w, http.StatusConflict, api.Conflict{
			AppliedPlanID: "plan-9",
			AppliedToken:  api.Token{LeaderEpoch: 8, RenderSeq: 3},
			Reason:        "prev_mismatch",
		})
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).Apply(context.Background(), testManifest(),
		map[string]io.Reader{"haproxy.cfg": strings.NewReader("globa")}, nil)
	var conflict *ConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, "plan-9", conflict.Conflict.AppliedPlanID)
	assert.Equal(t, "prev_mismatch", conflict.Conflict.Reason)
	assert.Equal(t, uint64(8), conflict.Conflict.AppliedToken.LeaderEpoch)
}

func TestApplyMissingParts(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		writeJSON(t, w, http.StatusConflict, api.Missing{Missing: []string{"maps/host.map"}})
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).Apply(context.Background(), testManifest(), nil, nil)
	var missing *MissingError
	require.ErrorAs(t, err, &missing)
	assert.Equal(t, []string{"maps/host.map"}, missing.Missing)
}

func TestApplyNACKIsNotATransportError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		writeJSON(t, w, http.StatusOK, api.ApplyResult{
			PlanID: "plan-2",
			OK:     false,
			Mode:   api.ResultRejected,
			Error:  &api.ApplyError{Stage: "reload", Message: "[ALERT] config invalid"},
		})
	}))
	defer srv.Close()

	result, err := newTestClient(t, srv.URL).Apply(context.Background(), testManifest(), nil, nil)
	require.NoError(t, err)
	assert.False(t, result.OK)
	require.NotNil(t, result.Error)
	assert.Equal(t, "reload", result.Error.Stage)
}

func TestApplyRejectsOversizedBodyBeforeSending(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writeJSON(t, w, http.StatusOK, api.ApplyResult{OK: true})
	}))
	defer srv.Close()

	m := testManifest()
	m.Files = []api.File{{Path: "haproxy.cfg", Digest: "d1", Size: api.MaxApplyBodyBytes + 1, Kind: api.FileKindConfig}}
	_, err := newTestClient(t, srv.URL).Apply(context.Background(), m,
		map[string]io.Reader{"haproxy.cfg": strings.NewReader("x")}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds the limit")
	assert.Zero(t, requests.Load(), "an oversized apply must never reach the wire")
}

func TestApplyRejectsMalformedManifest(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, api.ApplyResult{OK: true})
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	tests := []struct {
		name   string
		mutate func(*api.Manifest)
		want   string
	}{
		{"no plan id", func(m *api.Manifest) { m.PlanID = "" }, "no plan_id"},
		{"unknown mode", func(m *api.Manifest) { m.Mode = "sideways" }, "unknown apply mode"},
		{"absolute path", func(m *api.Manifest) { m.Files[0].Path = "/etc/haproxy/haproxy.cfg" }, "must be relative"},
		{"escaping path", func(m *api.Manifest) { m.Files[0].Path = "../haproxy.cfg" }, "escapes the base dir"},
		{"uncleaned path", func(m *api.Manifest) { m.Files[0].Path = "maps/./host.map" }, "cleaned form"},
		{"overlong path", func(m *api.Manifest) { m.Files[0].Path = strings.Repeat("a", api.MaxPathBytes+1) }, "exceeds"},
		{"duplicate path", func(m *api.Manifest) { m.Files[1].Path = m.Files[0].Path }, "declared twice"},
		{"no digest", func(m *api.Manifest) { m.Files[0].Digest = "" }, "no digest"},
		{"negative size", func(m *api.Manifest) { m.Files[0].Size = -1 }, "negative size"},
		{"too many ops", func(m *api.Manifest) {
			m.Ops = make([]api.Op, api.MaxOpsPerApply+1)
		}, "op limit"},
		{"too many operation batches", func(m *api.Manifest) { m.OpBatches = make([][]api.Op, api.MaxOpBatches) }, "batch limit"},
		{"oversized continuation", func(m *api.Manifest) { m.OpBatches = [][]api.Op{make([]api.Op, api.MaxOpsPerApply+1)} }, "must contain"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := testManifest()
			tt.mutate(m)
			_, err := c.Apply(context.Background(), m, nil, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestApplyRejectsPartForUndeclaredFile(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, api.ApplyResult{OK: true})
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).Apply(context.Background(), testManifest(),
		map[string]io.Reader{"maps/other.map": strings.NewReader("x")}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a file of this manifest")
}

func TestApplyRejectsShortPartContent(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		writeJSON(t, w, http.StatusOK, api.ApplyResult{OK: true})
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).Apply(context.Background(), testManifest(),
		map[string]io.Reader{"haproxy.cfg": strings.NewReader("nope")}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "manifest declares 5 bytes, content yielded 4")
}

// resetListener answers the first `resets` connections with a TCP reset — the
// master's re-exec window as the client sees it — and serves the rest.
// resetListener resets the first `remaining` connections instead of serving
// them: a plain close is a FIN, which the client sees as EOF, not ECONNRESET.
// It resets after `resetAfter` has crossed the wire (empty: after the first
// byte), so a test can pin whether the client's single-use parts had been
// consumed at that point.
type resetListener struct {
	net.Listener
	remaining  atomic.Int32
	accepted   atomic.Int32
	resetAfter string
}

func (l *resetListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		l.accepted.Add(1)
		if l.remaining.Add(-1) < 0 {
			return conn, nil
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
			_ = tcp.SetReadDeadline(time.Now().Add(2 * time.Second))
			readUntil(tcp, l.resetAfter)
		}
		_ = conn.Close()
	}
}

// readUntil drains conn until marker was seen (or one byte, for an empty
// marker), so the peer has flushed that much of its request before the reset.
func readUntil(conn net.Conn, marker string) {
	var seen []byte
	buf := make([]byte, 512)
	for {
		n, err := conn.Read(buf)
		seen = append(seen, buf[:n]...)
		if err != nil || marker == "" || strings.Contains(string(seen), marker) {
			return
		}
	}
}

func TestStateRetriesResetConnectionThenSucceeds(t *testing.T) {
	t.Parallel()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, api.State{APIVersion: api.Version, AppliedPlanID: "plan-1"})
	}))
	listener := &resetListener{Listener: srv.Listener}
	listener.remaining.Store(2)
	srv.Listener = listener
	srv.Start()
	defer srv.Close()

	state, err := newTestClient(t, srv.URL).State(context.Background(), api.StateRead{})
	require.NoError(t, err)
	assert.Equal(t, "plan-1", state.AppliedPlanID)
	assert.Equal(t, int32(3), listener.accepted.Load(), "two resets then one served connection")
}

func TestStateGivesUpAfterConnectRetries(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	closed := "http://" + listener.Addr().String()
	require.NoError(t, listener.Close())

	c := newTestClient(t, closed)
	start := time.Now()
	_, err = c.State(context.Background(), api.StateRead{})
	require.Error(t, err)
	assert.ErrorIs(t, err, syscall.ECONNREFUSED)
	// One attempt plus ConnectRetries retries, each preceded by the backoff.
	assert.GreaterOrEqual(t, time.Since(start), time.Duration(api.ConnectRetries)*10*time.Millisecond)
}

func TestApplyDoesNotRetryOnceThePartsWereRead(t *testing.T) {
	t.Parallel()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		writeJSON(t, w, http.StatusOK, api.ApplyResult{OK: true})
	}))
	// Reset only once the part's bytes are on the wire, so its reader was
	// provably consumed when the connect error surfaces.
	listener := &resetListener{Listener: srv.Listener, resetAfter: "globa"}
	listener.remaining.Store(1)
	srv.Listener = listener
	srv.Start()
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).Apply(context.Background(), testManifest(),
		map[string]io.Reader{"haproxy.cfg": strings.NewReader("globa")}, nil)
	require.Error(t, err)
	assert.Equal(t, int32(1), listener.accepted.Load(), "a consumed single-use body must not be replayed")
}

func TestApplyHonoursContextCancellation(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	blocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		close(blocked)
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-blocked
		cancel()
	}()
	_, err := newTestClient(t, srv.URL).Apply(ctx, testManifest(), nil, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, int32(1), requests.Load(), "a cancelled apply is never retried")
}

func TestCheckSkew(t *testing.T) {
	t.Parallel()
	full := ComposableOps()
	require.Equal(t, deployplan.ComposedOps(), full, "the skew check measures against what deployplan composes")

	t.Run("matching agent", func(t *testing.T) {
		incompatible, missing := CheckSkew(&api.State{APIVersion: api.Version, AgentOps: full})
		assert.False(t, incompatible)
		assert.Empty(t, missing)
	})
	t.Run("major mismatch", func(t *testing.T) {
		incompatible, missing := CheckSkew(&api.State{APIVersion: api.Version + 1, AgentOps: full})
		assert.True(t, incompatible)
		assert.Empty(t, missing)
	})
	t.Run("missing op kinds", func(t *testing.T) {
		incompatible, missing := CheckSkew(&api.State{APIVersion: api.Version, AgentOps: full[:len(full)-2]})
		assert.False(t, incompatible)
		assert.Equal(t, full[len(full)-2:], missing)
	})
	t.Run("no op kinds reported", func(t *testing.T) {
		incompatible, missing := CheckSkew(&api.State{APIVersion: api.Version})
		assert.True(t, incompatible, "deployplan reads no kinds as every kind, so the ops cannot be gated")
		assert.Equal(t, full, missing)
	})
	t.Run("no state", func(t *testing.T) {
		incompatible, missing := CheckSkew(nil)
		assert.True(t, incompatible)
		assert.Empty(t, missing)
	})
}

// PutPlan tells the keeper apart what it must act on: a pod that moved on
// (drop this blob), an agent without the endpoint (ride the next apply), and
// any other failure (retry with the next offer).
func TestPutPlanMapsTheAgentsAnswers(t *testing.T) {
	t.Parallel()
	var status atomic.Int32
	var gotQuery, gotBody atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery.Store(r.URL.RawQuery)
		body, _ := io.ReadAll(r.Body)
		gotBody.Store(string(body))
		code := int(status.Load())
		switch code {
		case http.StatusOK:
			writeJSON(t, w, code, api.PlanStored{PlanID: "plan-1", Proof: "p"})
		case http.StatusConflict:
			writeJSON(t, w, code, api.PlanMoved{AppliedPlanID: "plan-2"})
		default:
			http.Error(w, "nope", code)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	status.Store(http.StatusOK)
	require.NoError(t, c.PutPlan(context.Background(), "plan-1", "p", []byte("blob")))
	assert.Equal(t, "plan_id=plan-1&proof=p", gotQuery.Load())
	assert.Equal(t, "blob", gotBody.Load())

	status.Store(http.StatusConflict)
	assert.ErrorIs(t, c.PutPlan(context.Background(), "plan-1", "p", []byte("blob")), ErrPlanMoved)

	status.Store(http.StatusNotFound)
	assert.ErrorIs(t, c.PutPlan(context.Background(), "plan-1", "p", []byte("blob")), ErrNoPlanEndpoint)

	status.Store(http.StatusInternalServerError)
	err := c.PutPlan(context.Background(), "plan-1", "p", []byte("blob"))
	var httpErr *HTTPError
	require.ErrorAs(t, err, &httpErr)
	assert.Equal(t, http.StatusInternalServerError, httpErr.Status)

	require.Error(t, c.PutPlan(context.Background(), "", "p", []byte("blob")))
	require.Error(t, c.PutPlan(context.Background(), "plan-1", "p", nil))
}
