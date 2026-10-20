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

// Package agenttest is an in-process HAPTIC agent: an httptest server that
// speaks pkg/dataplane/agent/api well enough to drive the controller's
// deployer without a container.
//
// It models what the deployer reasons about — the path-keyed proved file set,
// the four plan ids, the fencing token, the runtime inventory —
// and records every apply for assertions. It executes no HAProxy commands and
// writes no files; the real agent's disk transaction is covered by the docker
// suite in tests/agent.
package agenttest

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/client"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
)

// Default credentials, mirroring the chart's agent Secret.
const (
	DefaultUsername = "admin"
	DefaultPassword = "adminpwd"
)

// defaultWorkerPID is the first worker identity the fake reports; every reload
// increments it, so a test can tell a reload from a runtime apply.
const defaultWorkerPID = 100

// RecordedApply is one received apply and the answer it produced.
type RecordedApply struct {
	Manifest api.Manifest
	Parts    map[string][]byte
	Plan     []byte
	Status   int
	Result   *api.ApplyResult
	Conflict *api.Conflict
	Missing  []string
}

// RecordedPlanPut is one PUT /v1/plan the fake received.
type RecordedPlanPut struct {
	PlanID string
	Proof  string
	Plan   []byte
}

// Agent is the fake. Every method is safe for concurrent use.
type Agent struct {
	server   *httptest.Server
	username string
	password string

	mu sync.Mutex
	// state carries no AppliedPlan: the stored blob is handed back only while
	// it describes the applied plan, which snapshot decides.
	state             api.State
	appliedPlan       []byte
	planBlobPlanID    string
	planBlobPlanProof string
	proofGeneration   uint64
	kinds             map[string]string
	// contents is what the fake holds on its "disk": a patch splices into it.
	contents       map[string][]byte
	lkgFiles       map[string]api.FileAt
	lkgContents    map[string][]byte
	reloadPending  bool
	rejectedOps    map[string]struct{}
	conflictOnce   string
	failOnce       bool
	missingOnce    []string
	applies        []RecordedApply
	planPuts       []RecordedPlanPut
	stateReads     int
	planReads      int
	noPlanEndpoint bool
	planPutDelay   time.Duration
}

// Option customises the fake before it starts serving.
type Option func(*Agent)

// WithCredentials sets the basic-auth pair the fake requires.
func WithCredentials(username, password string) Option {
	return func(a *Agent) {
		a.username = username
		a.password = password
	}
}

// WithHAProxyInfo sets the worker identity the fake reports.
func WithHAProxyInfo(info api.HAProxyInfo) Option {
	return func(a *Agent) { a.state.HAProxy = info }
}

// WithoutFilePatches makes the fake behave like an agent that predates
// File.Patch, so every changed file has to arrive whole.
func WithoutFilePatches() Option {
	return withoutFeature(api.FeatureFilePatch)
}

// WithoutRuntimeBatches models an agent without complete multi-batch transactions.
func WithoutRuntimeBatches() Option { return withoutFeature(api.FeatureRuntimeBatches) }

// WithoutWorkerFence models an agent that predates automatic-apply worker fencing.
func WithoutWorkerFence() Option {
	return withoutFeature(api.FeatureWorkerFence)
}

func withoutFeature(feature string) Option {
	return func(a *Agent) {
		a.state.Features = slices.DeleteFunc(a.state.Features, func(value string) bool { return value == feature })
	}
}

// WithAgentOps restricts the op kinds the fake claims to execute, so a test
// can drive the controller's version-skew path.
func WithAgentOps(ops ...string) Option {
	return func(a *Agent) { a.state.AgentOps = ops }
}

// WithAPIVersion sets the contract version the fake reports.
func WithAPIVersion(version int) Option {
	return func(a *Agent) { a.state.APIVersion = version }
}

// WithInventory seeds what the running worker has loaded.
func WithInventory(inventory *api.Inventory) Option {
	return func(a *Agent) {
		if inventory != nil {
			a.state.Inventory = *inventory
		}
	}
}

// New starts a fake agent on loopback and stops it when the test ends.
func New(tb testing.TB, opts ...Option) *Agent {
	tb.Helper()
	a := &Agent{
		username:    DefaultUsername,
		password:    DefaultPassword,
		kinds:       map[string]string{},
		contents:    map[string][]byte{},
		rejectedOps: map[string]struct{}{},
		state: api.State{
			APIVersion:        api.Version,
			AgentVersion:      "agenttest",
			PlanSchemaVersion: 1,
			AgentOps:          client.ComposableOps(),
			Features:          []string{api.FeatureFilePatch, api.FeatureWorkerFence, api.FeatureRuntimeBatches},
			HAProxy:           api.HAProxyInfo{Version: "3.4.3", FullVersion: "3.4.3-1", WorkerPID: defaultWorkerPID},
			Files:             map[string]api.FileAt{},
		},
	}
	for _, opt := range opts {
		opt(a)
	}
	a.server = httptest.NewServer(a.routes())
	tb.Cleanup(a.server.Close)
	return a
}

// URL is the base URL a client.Config takes.
func (a *Agent) URL() string { return a.server.URL }

// Username and Password are the credentials the fake requires.
func (a *Agent) Username() string { return a.username }

// Password is the basic-auth password the fake requires.
func (a *Agent) Password() string { return a.password }

// State is a snapshot of what the fake would report on /v1/state, blob
// included.
func (a *Agent) State() api.State {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.snapshot(true)
}

// Content is what the fake holds at path, nil when it holds nothing there.
func (a *Agent) Content(path string) []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.contents[path])
}

// TamperFile rewrites path behind the fake's back, as something else in the
// pod would. Like the agent, the fake reports it only on a verified read.
func (a *Agent) TamperFile(path string, content []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.contents[path] = slices.Clone(content)
}

// Applies returns every request the fake received, in order.
func (a *Agent) Applies() []RecordedApply {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]RecordedApply(nil), a.applies...)
}

// StateReads is how many times /v1/state was answered, which is what a caller
// that caches per pod is measured by.
func (a *Agent) StateReads() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stateReads
}

// PlanReads is how many of those reads asked for the plan blob, which a
// controller that holds the plan leaves out.
func (a *Agent) PlanReads() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.planReads
}

// SetReloadPending makes the fake behave as if a paced reload were already
// scheduled: files are written and coalesced, ops are ignored, and only the
// in-place ops run.
func (a *Agent) SetReloadPending(pending bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reloadPending = pending
	a.state.ReloadPendingAt = ""
	if pending {
		a.state.ReloadPendingAt = fixedTimestamp
	}
}

// FirePendingReload is the pacer firing: the worker re-executes from the
// applied file set, so the applied plan becomes the running one.
func (a *Agent) FirePendingReload() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.reloadPending {
		return
	}
	a.performReload()
	a.state.RunningPlanID = a.state.AppliedPlanID
	a.state.RunningPlanProof = a.state.AppliedPlanProof
	a.state.WorkerOpsPlanID = a.state.AppliedPlanID
	a.state.WorkerOpsPlanProof = a.state.AppliedPlanProof
	a.state.LKGPlanID = a.state.AppliedPlanID
	a.state.LKGPlanProof = a.state.AppliedPlanProof
	a.lkgFiles = maps.Clone(a.state.Files)
	a.lkgContents = maps.Clone(a.contents)
}

// SetAppliedEpoch raises the leader epoch the fake has accepted. The agent
// persists the applied token, so a pod a previous leader wrote to outranks a
// controller whose epoch counter is behind — every apply below it is a 409.
func (a *Agent) SetAppliedEpoch(epoch uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.AppliedToken.LeaderEpoch = epoch
}

// RejectOp makes every apply carrying this op kind come back as a NACK, the
// way HAProxy refusing a runtime command does.
func (a *Agent) RejectOp(kind string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rejectedOps[kind] = struct{}{}
}

// AcceptOp undoes RejectOp for this kind.
func (a *Agent) AcceptOp(kind string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.rejectedOps, kind)
}

// SetPendingDeletes seeds the deferred deletes this pod is still waiting to
// complete. A pod at the cap refuses another batch, so what the controller
// composes for it has to be judged against its own count.
func (a *Agent) SetPendingDeletes(servers, backends []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.PendingDeletes = api.PendingDeletes{Servers: servers, Backends: backends}
}

// FailOnce makes the next apply answer 500 and write nothing, the way an agent
// that hit an internal error does. The caller sees a failure, not a judgement:
// nothing about the pod's state is known to have changed.
func (a *Agent) FailOnce() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failOnce = true
}

// ConflictOnce makes the next apply answer this 409 reason and write nothing,
// which is what the agent does when its baseline moved between the caller's
// state read and its apply. The reason is one of prev_mismatch, stale_epoch or
// unknown_baseline.
func (a *Agent) ConflictOnce(reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.conflictOnce = reason
}

// MissingOnce makes the next apply answer 409 with these paths and write
// nothing, which is what the agent does when its tree does not hold a file the
// manifest declares and the caller did not send it.
func (a *Agent) MissingOnce(paths ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.missingOnce = paths
}

func (a *Agent) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(api.PathHealthz, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc(api.PathReadyz, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ready"))
	})
	mux.HandleFunc(api.PathState, a.authorized(a.handleState))
	mux.HandleFunc(api.PathApply, a.authorized(a.handleApply))
	if !a.noPlanEndpoint {
		mux.HandleFunc(api.PathPlan, a.authorized(a.handlePlan))
	}
	return mux
}

// handlePlan mirrors the agent: the blob is kept only while (plan id, proof)
// still name the applied plan.
func (a *Agent) handlePlan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, proof := r.URL.Query().Get("plan_id"), r.URL.Query().Get("proof")
	blob, err := io.ReadAll(io.LimitReader(r.Body, api.MaxPlanBlobBytes+1))
	if err != nil || id == "" || proof == "" || len(blob) == 0 || len(blob) > api.MaxPlanBlobBytes {
		http.Error(w, "bad plan upload", http.StatusBadRequest)
		return
	}
	if a.planPutDelay > 0 {
		select {
		case <-time.After(a.planPutDelay):
		case <-r.Context().Done():
			return
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.planPuts = append(a.planPuts, RecordedPlanPut{PlanID: id, Proof: proof, Plan: blob})
	if !samePlanRef(a.state.AppliedPlanID, a.state.AppliedPlanProof, id, proof) {
		writeJSON(w, http.StatusConflict, api.PlanMoved{
			AppliedPlanID: a.state.AppliedPlanID, AppliedPlanProof: a.state.AppliedPlanProof,
		})
		return
	}
	a.appliedPlan = blob
	a.planBlobPlanID = id
	a.planBlobPlanProof = proof
	writeJSON(w, http.StatusOK, api.PlanStored{PlanID: id, Proof: proof})
}

// PlanPuts returns every plan blob upload the fake received, in order.
func (a *Agent) PlanPuts() []RecordedPlanPut {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]RecordedPlanPut(nil), a.planPuts...)
}

// WithoutPlanEndpoint makes the fake behave like an agent that predates
// PUT /v1/plan, so the blob has to ride the apply.
func WithoutPlanEndpoint() Option {
	return func(a *Agent) { a.noPlanEndpoint = true }
}

// WithPlanPutDelay makes every PUT /v1/plan take this long before it is
// stored, and gives it up when the request's context ends first.
func WithPlanPutDelay(delay time.Duration) Option {
	return func(a *Agent) { a.planPutDelay = delay }
}

func (a *Agent) authorized(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(a.username)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(a.password)) == 1
		if !ok || !userOK || !passOK {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (a *Agent) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	withPlan := r.URL.Query().Get("plan") != "0"
	a.mu.Lock()
	if r.URL.Query().Get("verify") == "1" {
		a.reobserve()
	}
	a.stateReads++
	if withPlan {
		a.planReads++
	}
	state := a.snapshot(withPlan)
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, state)
}

// reobserve is a verified read: a file whose content changed behind the fake
// is reported as found, without a proof. Callers hold a.mu.
func (a *Agent) reobserve() {
	for path, at := range a.state.Files {
		content := a.contents[path]
		if digest := renderplan.Digest(content); digest != at.Digest || int64(len(content)) != at.Size {
			a.state.Files[path] = api.FileAt{Digest: digest, Size: int64(len(content))}
		}
	}
}

// snapshot copies the maps the state carries so a caller cannot observe a
// later apply through them. The blob rides only a read that asked for it, as
// on the real agent. Callers hold a.mu.
func (a *Agent) snapshot(withPlan bool) api.State {
	state := a.state
	state.Files = make(map[string]api.FileAt, len(a.state.Files))
	for path, at := range a.state.Files {
		state.Files[path] = at
	}
	if a.planBlobPlanID != "" && a.planBlobPlanProof != "" &&
		a.planBlobPlanID == a.state.AppliedPlanID && a.planBlobPlanProof == a.state.AppliedPlanProof {
		state.AppliedPlanStored = true
		if withPlan {
			state.AppliedPlan = a.appliedPlan
		}
	}
	return state
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}
