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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/haproxytest"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/server"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/transportsecurity"
)

const (
	testUser     = "haptic"
	testPassword = "s3cret"
	configPath   = "haproxy.cfg"
)

// harness runs a real agent against the HAProxy model over a loopback socket.
type harness struct {
	t        testing.TB
	agent    *server.Server
	model    *haproxytest.HAProxy
	baseDir  string
	registry *prometheus.Registry
	url      string
	client   *http.Client
	stopOnce func()
}

type options struct {
	reloadIntervalMin time.Duration
	baseDir           string
	model             *haproxytest.HAProxy
	drainSocket       string
	adminSocket       string
	serverTLS         *transportsecurity.Source
	clientTLS         *transportsecurity.Source
	drainQuietPeriod  time.Duration
	drainMaxWait      time.Duration
	blobDir           string
	blobMaxSize       int64
}

func newHarness(tb testing.TB, opts ...func(*options)) *harness {
	tb.Helper()
	t := tb
	settings := options{baseDir: t.TempDir()}
	for _, opt := range opts {
		opt(&settings)
	}
	model := settings.model
	if model == nil {
		model = haproxytest.Start(t)
	}
	registry := prometheus.NewRegistry()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	agent, err := server.New(ctx, &server.Config{
		BaseDir:              settings.baseDir,
		ConfigFile:           configPath,
		MasterSocket:         model.MasterSocket(),
		WorkerSocket:         model.WorkerSocket(),
		StateFile:            ".haptic-agent.json",
		Listen:               "127.0.0.1:0",
		ReloadIntervalMin:    settings.reloadIntervalMin,
		Username:             testUser,
		Password:             testPassword,
		AgentVersion:         "test",
		Logger:               slog.New(slog.DiscardHandler),
		Registry:             registry,
		TLS:                  settings.serverTLS,
		AdminSocket:          settings.adminSocket,
		DrainSocket:          settings.drainSocket,
		DrainQuietPeriod:     settings.drainQuietPeriod,
		DrainMaxWait:         settings.drainMaxWait,
		DrainIgnoreFrontends: []string{"status"},
		BlobDir:              settings.blobDir,
		BlobMaxSize:          settings.blobMaxSize,
	})
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- agent.Start(ctx) }()
	stop := sync.OnceFunc(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the agent did not shut down")
		}
	})
	t.Cleanup(stop)

	h := &harness{
		t: t, agent: agent, model: model, baseDir: settings.baseDir,
		registry: registry, client: &http.Client{Timeout: 30 * time.Second},
		stopOnce: stop,
	}
	require.Eventually(t, func() bool { return agent.Ready() }, 10*time.Second, 10*time.Millisecond)
	for _, socket := range []string{settings.adminSocket, settings.drainSocket} {
		if socket != "" {
			_, err := os.Stat(socket)
			require.NoError(t, err, "a ready agent serves its local sockets")
		}
	}
	h.url = "http://" + agent.Addr()
	if settings.serverTLS != nil {
		h.url = "https://" + agent.Addr()
		h.client.Transport = transportsecurity.NewTransport(settings.clientTLS, &http.Transport{})
	}
	t.Cleanup(h.client.CloseIdleConnections)
	return h
}

func withReloadInterval(d time.Duration) func(*options) {
	return func(o *options) { o.reloadIntervalMin = d }
}

func withBaseDir(dir string) func(*options) {
	return func(o *options) { o.baseDir = dir }
}

func withDrain(socket string, quiet, bound time.Duration) func(*options) {
	return func(o *options) { o.drainSocket, o.drainQuietPeriod, o.drainMaxWait = socket, quiet, bound }
}

func withBlobs(dir string, maxSize int64) func(*options) {
	return func(o *options) { o.blobDir, o.blobMaxSize = dir, maxSize }
}

func withModel(model *haproxytest.HAProxy) func(*options) {
	return func(o *options) { o.model = model }
}

// stop shuts the agent down so a second one can take over the same tree.
func (h *harness) stop() {
	h.t.Helper()
	h.stopOnce()
}

// restart replaces the agent with a fresh one on the same tree and the same
// HAProxy, which is what a container restart of the agent alone looks like.
func (h *harness) restart() *harness {
	h.t.Helper()
	h.stop()
	return newHarness(h.t, withBaseDir(h.baseDir), withModel(h.model))
}

// file is one desired file of a test manifest, content included.
type file struct {
	Path    string
	Content string
	Kind    string
	Reload  bool
}

// buildManifest turns the test's files into the wire manifest.
func buildManifest(planID string, list []file) api.Manifest {
	m := api.Manifest{
		IdentityVersion:   api.ExactIdentityVersion,
		PlanID:            planID,
		PlanSchemaVersion: 1,
		Mode:              api.ModeAuto,
	}
	for _, f := range list {
		kind := f.Kind
		if kind == "" {
			kind = api.FileKindMap
		}
		if f.Path == configPath {
			kind = api.FileKindConfig
		}
		m.Files = append(m.Files, api.File{
			Path:           f.Path,
			Digest:         renderplan.DigestString(f.Content),
			Proof:          renderplan.DigestString(f.Content),
			Size:           int64(len(f.Content)),
			Kind:           kind,
			ReloadOnChange: f.Reload,
		})
	}
	return m
}

// post sends an apply and returns the status and the body. Every file of list
// travels as a part unless omit names it, which is how the 409-missing path is
// exercised.
func (h *harness) post(m *api.Manifest, list []file, omit ...string) (status int, answer []byte) {
	h.t.Helper()
	return h.postWithPlan(m, list, nil, omit...)
}

// applyWithPlan sends the opaque plan blob alongside the manifest, which is
// what the controller does when a pod may have to answer the baseline
// question for it.
func (h *harness) applyWithPlan(m *api.Manifest, list []file, plan []byte) api.ApplyResult {
	h.t.Helper()
	status, raw := h.postWithPlan(m, list, plan)
	require.Equal(h.t, http.StatusOK, status, string(raw))
	result := api.ApplyResult{}
	require.NoError(h.t, json.Unmarshal(raw, &result))
	return result
}

func (h *harness) postWithPlan(m *api.Manifest, list []file, plan []byte, omit ...string) (status int, answer []byte) {
	h.t.Helper()
	h.prepareExactManifest(m)
	return h.postPrepared(m, list, plan, omit...)
}

func (h *harness) postRaw(m *api.Manifest, list []file) (status int, answer []byte) {
	h.t.Helper()
	return h.postPrepared(m, list, nil)
}

func (h *harness) postPrepared(m *api.Manifest, list []file, plan []byte, omit ...string) (status int, answer []byte) {
	h.t.Helper()
	skip := map[string]struct{}{}
	for _, path := range omit {
		skip[path] = struct{}{}
	}
	payload := &bytes.Buffer{}
	writer := multipart.NewWriter(payload)
	manifestPart, err := writer.CreateFormField(api.PartManifest)
	require.NoError(h.t, err)
	require.NoError(h.t, json.NewEncoder(manifestPart).Encode(m))
	if plan != nil {
		planPart, planErr := writer.CreateFormField(api.PartPlan)
		require.NoError(h.t, planErr)
		_, planErr = planPart.Write(plan)
		require.NoError(h.t, planErr)
	}
	for _, f := range list {
		if _, omitted := skip[f.Path]; omitted {
			continue
		}
		part, err := writer.CreateFormFile(f.Path, f.Path)
		require.NoError(h.t, err)
		_, err = part.Write([]byte(f.Content))
		require.NoError(h.t, err)
	}
	require.NoError(h.t, writer.Close())

	request, err := http.NewRequestWithContext(h.t.Context(), http.MethodPost, h.url+api.PathApply, payload)
	require.NoError(h.t, err)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.SetBasicAuth(testUser, testPassword)
	response, err := h.client.Do(request)
	require.NoError(h.t, err)
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	require.NoError(h.t, err)
	return response.StatusCode, raw
}

func (h *harness) prepareExactManifest(m *api.Manifest) {
	state := h.state(false)
	m.IdentityVersion = api.ExactIdentityVersion
	m.ExpectedPrevPlanProof = ""
	if m.ExpectedPrevPlanID == state.AppliedPlanID {
		m.ExpectedPrevPlanProof = state.AppliedPlanProof
	}
	m.ExpectedWorkerOpsPlanProof = ""
	if m.ExpectedWorkerOpsPlanID == "" && m.Mode == api.ModeAuto {
		m.ExpectedWorkerOpsPlanID = state.WorkerOpsPlanID
	}
	if m.ExpectedWorkerOpsPlanID == state.WorkerOpsPlanID {
		m.ExpectedWorkerOpsPlanProof = state.WorkerOpsPlanProof
	}
	if len(m.InPlaceOps) > 0 && m.ExpectedWorkerOpsPlanProof == "" {
		m.ExpectedWorkerOpsPlanProof = "test:stale-worker"
	}
	m.ValidatedPlanProof = ""
	if m.ValidatedPlanID == state.AppliedPlanID {
		m.ValidatedPlanProof = state.AppliedPlanProof
	}
	if m.ValidatedPlanID != "" && m.ValidatedPlanProof == "" {
		m.ValidatedPlanProof = "test:stale-validated"
	}
	if m.Mode == api.ModeRevertLKG {
		m.PlanProof = ""
		switch m.PlanID {
		case state.AppliedPlanID:
			m.PlanProof = state.AppliedPlanProof
		case state.WorkerOpsPlanID:
			m.PlanProof = state.WorkerOpsPlanProof
		}
		if m.PlanProof == "" {
			m.PlanProof = "test:stale-revert"
		}
		return
	}
	if state.AppliedPlanID != "" && state.AppliedPlanProof == "" {
		m.Mode = api.ModeReload
		m.Ops = nil
		m.OpBatches = nil
		m.InPlaceOps = nil
		m.ExpectedWorkerOpsPlanID = ""
		m.ExpectedWorkerOpsPlanProof = ""
		m.WorkerOpsPlanID = ""
	}
}

// applyConflict posts and requires a 409, returning the conflict.
func (h *harness) applyConflict(m *api.Manifest, list []file) api.Conflict {
	h.t.Helper()
	status, raw := h.post(m, list)
	require.Equal(h.t, http.StatusConflict, status, string(raw))
	conflict := api.Conflict{}
	require.NoError(h.t, json.Unmarshal(raw, &conflict))
	return conflict
}

// apply posts and requires a 200, returning the ACK.
func (h *harness) apply(m *api.Manifest, list []file) api.ApplyResult {
	h.t.Helper()
	status, raw := h.post(m, list)
	require.Equal(h.t, http.StatusOK, status, string(raw))
	result := api.ApplyResult{}
	require.NoError(h.t, json.Unmarshal(raw, &result))
	return result
}

func (h *harness) state(verify bool) api.State {
	h.t.Helper()
	return h.stateRead(api.StateRead{Verify: verify, Plan: true})
}

// putPlan uploads a blob for (planID, proof) and returns the status.
func (h *harness) putPlan(planID, proof string, blob []byte) int {
	h.t.Helper()
	url := h.url + api.PathPlan + "?plan_id=" + planID + "&proof=" + proof
	request, err := http.NewRequestWithContext(h.t.Context(), http.MethodPut, url, bytes.NewReader(blob))
	require.NoError(h.t, err)
	request.SetBasicAuth(testUser, testPassword)
	response, err := h.client.Do(request)
	require.NoError(h.t, err)
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}

// stateRead reads the state the way the controller asks for it.
func (h *harness) stateRead(read api.StateRead) api.State {
	h.t.Helper()
	url := h.url + api.PathState + read.Query()
	request, err := http.NewRequestWithContext(h.t.Context(), http.MethodGet, url, http.NoBody)
	require.NoError(h.t, err)
	request.SetBasicAuth(testUser, testPassword)
	response, err := h.client.Do(request)
	require.NoError(h.t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(h.t, http.StatusOK, response.StatusCode)
	state := api.State{}
	require.NoError(h.t, json.NewDecoder(response.Body).Decode(&state))
	return state
}

// persisted decodes the agent's state file, which is what a restart reads and
// therefore the only evidence that in-memory state was recorded.
func (h *harness) persisted() map[string]any {
	h.t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.baseDir, ".haptic-agent.json"))
	require.NoError(h.t, err)
	out := map[string]any{}
	require.NoError(h.t, json.Unmarshal(raw, &out))
	return out
}

func (h *harness) read(path string) string {
	h.t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.baseDir, path))
	require.NoError(h.t, err)
	return string(raw)
}

func (h *harness) exists(path string) bool {
	h.t.Helper()
	_, err := os.Lstat(filepath.Join(h.baseDir, path))
	return err == nil
}

// tree lists the manifest-owned files on disk, so a test can assert that the
// set is the desired one and not a mix. The agent's own dot-prefixed state,
// temp and backup entries are not part of it.
func (h *harness) tree() map[string]string {
	h.t.Helper()
	out := map[string]string{}
	walk := func(path string, entry os.DirEntry, err error) error {
		require.NoError(h.t, err)
		if strings.HasPrefix(entry.Name(), ".") && path != h.baseDir {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(h.baseDir, path)
		require.NoError(h.t, relErr)
		out[filepath.ToSlash(rel)] = h.read(rel)
		return nil
	}
	require.NoError(h.t, filepath.WalkDir(h.baseDir, walk))
	return out
}

func (h *harness) metric(name string, labels ...string) float64 {
	h.t.Helper()
	families, err := h.registry.Gather()
	require.NoError(h.t, err)
	total := 0.0
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			if !matchesLabels(metric, labels) {
				continue
			}
			total += metric.GetCounter().GetValue() + metric.GetGauge().GetValue()
		}
	}
	return total
}

func matchesLabels(metric *dto.Metric, wanted []string) bool {
	if len(wanted) == 0 {
		return true
	}
	got := make([]string, 0, len(metric.GetLabel()))
	for _, pair := range metric.GetLabel() {
		got = append(got, pair.GetValue())
	}
	sort.Strings(got)
	sort.Strings(wanted)
	if len(got) != len(wanted) {
		return false
	}
	for i := range got {
		if got[i] != wanted[i] {
			return false
		}
	}
	return true
}

// writeFile changes the tree behind the agent's back, which is how the tests
// stand in for a container restart that copied the bootstrap files.
func writeFile(h *harness, path, content string) error {
	return os.WriteFile(filepath.Join(h.baseDir, path), []byte(content), 0o600)
}

// violations names the invariants that fired, which is what a failure needs to
// be actionable.
func (h *harness) violations() []string {
	h.t.Helper()
	families, err := h.registry.Gather()
	require.NoError(h.t, err)
	var names []string
	for _, family := range families {
		if family.GetName() != "haptic_agent_invariant_violations_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, pair := range metric.GetLabel() {
				names = append(names, fmt.Sprintf("%s=%v", pair.GetValue(), metric.GetCounter().GetValue()))
			}
		}
	}
	sort.Strings(names)
	return names
}
