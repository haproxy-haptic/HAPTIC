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
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/cli"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/files"
)

// planBlobName is where the opaque plan the controller sent is kept. It is not
// in the manifest, so it never enters the ownership set.
const planBlobName = ".haptic-agent-plan.bin"

// applyRun is one pass through the state machine
// staged → verified → backed_up → written → applied | reloaded | scheduled →
// committed | aborted.
type applyRun struct {
	server       *Server
	manifest     *api.Manifest
	staged       map[string]*files.Staged
	planBlob     []byte
	digest       string
	work         []byte
	appliedProof string
	workerProof  string
	result       api.ApplyResult

	tx     *files.Transaction
	opsRan bool
	// deterministic marks a failure that was HAProxy's own verdict on these
	// exact bytes, which is the only kind worth remembering as known-bad.
	deterministic bool
	// invalidations is the count the server had when the run started. A
	// higher one at the end means something declared the running worker
	// unexplained while the run was in flight, from this run or from the
	// concurrent verify path, and the pod may claim no baseline.
	invalidations uint64
	// touchedMaps and touchedBackends drive the asynchronous read-back;
	// retiringBackends are excluded from it because their deferred delete
	// legitimately makes them disappear.
	touchedMaps      []string
	touchedBackends  []string
	retiringBackends []string
	retiringServers  []cli.ServerRef
	// createdCerts and createdCAs are the runtime stores the batch brings into
	// existence, which the inventory has to learn without a reload.
	createdCerts []string
	createdCAs   []string
}

func (s *Server) runApply(
	m *api.Manifest,
	got *received,
	digest string,
	work []byte,
	appliedProof string,
	workerProof string,
	timing api.ApplyTiming,
) api.ApplyResult {
	run := &applyRun{
		server:        s,
		manifest:      m,
		staged:        got.files,
		planBlob:      got.plan,
		digest:        digest,
		work:          work,
		appliedProof:  appliedProof,
		workerProof:   workerProof,
		invalidations: s.invalidationCount(),
		result:        api.ApplyResult{PlanID: m.PlanID, OK: true, At: time.Now().UTC().Format(time.RFC3339), Timing: timing},
	}
	if err := run.execute(); err != nil {
		s.logger.Error("apply failed", "plan_id", m.PlanID, "error", err)
	}
	finishStarted := time.Now()
	s.finish(run)
	s.commitPlanBlob(run)
	s.background.Add(1)
	go func() {
		defer s.background.Done()
		s.readBack(run)
	}()
	// The state file holds run.result from finish on, so the answer is a copy.
	result := run.result
	result.Timing.FinishMs = time.Since(finishStarted).Milliseconds()
	return result
}

func (r *applyRun) execute() error {
	if r.manifest.Mode == api.ModeRevertLKG {
		return r.revertLKG()
	}
	r.server.setPhase(phaseVerified, r.manifest.PlanID)
	writeStarted := time.Now()
	r.tx = r.begin()
	if err := r.tx.Backup(); err != nil {
		return r.abort("backup", err)
	}
	r.server.setPhase(phaseBackedUp, r.manifest.PlanID)
	if err := r.tx.Write(); err != nil {
		return r.abort("write", err)
	}
	r.result.Timing.WriteMs = time.Since(writeStarted).Milliseconds()
	r.server.setPhase(phaseWritten, r.manifest.PlanID)
	r.server.observeTree(r.manifest, r.staged, r.deletions())
	opsStarted := time.Now()
	defer func() { r.result.Timing.OpsMs = time.Since(opsStarted).Milliseconds() }()
	return r.activate()
}

// begin opens the file transaction: every staged part is installed and every
// path the previous manifest owned but this one does not is dropped.
func (r *applyRun) begin() *files.Transaction {
	tx := r.server.store.Begin(r.server.journal(), r.server.cfg.ConfigFile)
	paths := make([]string, 0, len(r.staged))
	for path := range r.staged {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		tx.Install(r.staged[path])
	}
	for _, path := range r.deletions() {
		tx.Delete(path)
	}
	return tx
}

// deletions is the ownership set the manifest gave up. Absence means delete,
// but only for paths the agent itself put there.
func (r *applyRun) deletions() []string {
	desired := make(map[string]struct{}, len(r.manifest.Files))
	for _, f := range r.manifest.Files {
		desired[f.Path] = struct{}{}
	}
	var gone []string
	for _, path := range r.server.ownedPaths() {
		if _, kept := desired[path]; !kept {
			gone = append(gone, path)
		}
	}
	sort.Strings(gone)
	return gone
}

// activate decides between the runtime path, a reload and doing nothing. The
// decision is the controller's; the agent only falls back, never upgrades.
func (r *applyRun) activate() error {
	if r.server.reloadPending() {
		return r.inPlace()
	}
	if r.manifest.Mode == api.ModeReload {
		return r.reload(reloadReasonMode)
	}
	if r.server.baselineUnknown() {
		return r.reload("unknown_baseline")
	}
	ops := r.manifest.RuntimeOps()
	programs, err := r.compile(ops)
	if err != nil {
		r.server.metrics.invariant(false, "ops_executable")
		r.server.logger.Error("refusing an op batch; falling back to a reload", "error", err)
		return r.reload("unknown_op")
	}
	if len(programs) == 0 && len(ops) == 0 {
		return r.settle()
	}
	return r.runOps(programs)
}

// compile turns the ops into commands and records what the read-back has to
// look at. A failure here means the batch is not executable at all.
func (r *applyRun) compile(ops []api.Op) ([]cli.Program, error) {
	inline, servers, backends := cli.Split(ops)
	if err := r.server.deferrals.Check(servers, backends); err != nil {
		return nil, err
	}
	r.retiringBackends = backends
	r.retiringServers = servers
	programs := make([]cli.Program, 0, len(inline))
	for i := range inline {
		program, err := cli.Compile(&inline[i], r.readFile)
		if err != nil {
			return nil, err
		}
		programs = append(programs, program)
		r.note(&inline[i])
	}
	return programs, nil
}

func (r *applyRun) note(op *api.Op) {
	switch op.Kind {
	case api.OpMapAdd, api.OpMapSet, api.OpMapDel, api.OpMapReplace:
		r.touchedMaps = append(r.touchedMaps, op.Path)
	case api.OpServerAdd, api.OpServerDel, api.OpServerEnable, api.OpServerDisable,
		api.OpServerSetAddr, api.OpServerSetWeight, api.OpServerSetMaxconn, api.OpServerSetState:
		r.touchedBackends = append(r.touchedBackends, op.Backend)
	case api.OpCertNew:
		r.createdCerts = append(r.createdCerts, op.Path)
	case api.OpCANew:
		r.createdCAs = append(r.createdCAs, op.Path)
	}
}

// readFile serves the ops that push a whole file through the socket. The file
// is already on disk at that point, so the socket and the tree cannot diverge.
func (r *applyRun) readFile(path string) ([]byte, error) {
	abs, err := r.server.store.Abs(path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Clean(abs))
}

// runOps executes the batch between two worker identity checks. A worker that
// changed underneath means the commands may have landed on the outgoing
// process, which makes the baseline unknown.
func (r *applyRun) runOps(programs []cli.Program) error {
	if err := r.server.checkWorker(); err != nil {
		return r.reload("worker_changed")
	}
	worker := r.server.workerIdentity()
	results, err := r.executeBatches(programs)
	r.result.OpResults = results
	if err != nil {
		r.server.recordWorkerOps("", "")
		r.server.metrics.opErrors.WithLabelValues(failedKind(results)).Inc()
		r.server.logger.Warn("an op was rejected; reloading the desired set", "error", err)
		return r.reload("op_rejected")
	}
	if err := r.server.checkWorker(); err != nil {
		return r.reload("worker_changed")
	}
	if err := r.enqueueDeletes(worker); err != nil {
		return r.reload("deferred_delete_commit")
	}
	r.server.foldCreated(r)
	r.result.Mode = api.ResultRuntime
	r.server.setPhase(phaseApplied, r.manifest.PlanID)
	return nil
}

func (r *applyRun) executeBatches(programs []cli.Program) ([]api.OpResult, error) {
	if len(programs) <= api.MaxOpsPerApply {
		r.opsRan = true
		return r.server.runtime.Execute(programs)
	}
	results := make([]api.OpResult, 0, len(programs))
	for batch := range slices.Chunk(programs, api.MaxOpsPerApply) {
		batchResults, err := r.server.runtime.Execute(batch)
		r.opsRan = true
		results = append(results, batchResults...)
		if err != nil {
			return results, err
		}
	}
	return results, nil
}

func (r *applyRun) enqueueDeletes(worker api.HAProxyInfo) error {
	if err := r.server.deferrals.Enqueue(worker, r.retiringServers, r.retiringBackends); err != nil {
		return err
	}
	r.server.deferrals.Wake()
	return nil
}

// settle is the outcome when nothing needed a command: the files are the whole
// change, or there was no change at all.
func (r *applyRun) settle() error {
	r.result.Mode = api.ResultNoop
	if r.tx != nil && r.tx.Changes() > 0 {
		r.result.Mode = api.ResultFileOnly
	}
	r.server.setPhase(phaseApplied, r.manifest.PlanID)
	return nil
}

// inPlace coalesces the apply into the reload already scheduled and runs the
// in-place ops against the worker that keeps serving until it fires.
func (r *applyRun) inPlace() error {
	r.result.Mode = api.ResultScheduled
	r.server.coalesceIntoPendingReload(r.manifest.PlanID, r.appliedProof)
	_, _, due := r.server.pendingReload()
	r.result.Reload = &api.ReloadInfo{ScheduledAt: due.UTC().Format(time.RFC3339Nano)}
	return r.runInPlace()
}

// runInPlace runs the ops the controller composed against the running worker
// while its reload waits. A rejected one invalidates the pod's baseline
// instead of triggering a second reload.
func (r *applyRun) runInPlace() error {
	if len(r.manifest.InPlaceOps) == 0 {
		return nil
	}
	if !r.server.workerOpsBaselineMatches(
		r.manifest.ExpectedWorkerOpsPlanID,
		r.manifest.ExpectedWorkerOpsPlanProof,
	) {
		r.result.Error = &api.ApplyError{
			Stage:   "in_place",
			Message: "in-place ops were composed against a different worker baseline",
		}
		r.server.invalidateBaseline()
		return nil
	}
	programs, err := r.compile(r.manifest.InPlaceOps)
	worker := r.server.workerIdentity()
	if err == nil {
		err = r.server.checkWorker()
	}
	if err == nil {
		var results []api.OpResult
		results, err = r.server.runtime.Execute(programs)
		r.result.OpResults = results
		r.opsRan = true
	}
	if err == nil {
		err = r.server.checkWorker()
	}
	if err == nil {
		err = r.enqueueDeletes(worker)
	}
	if err != nil {
		r.result.Error = &api.ApplyError{Stage: "in_place", Message: err.Error()}
		r.server.invalidateBaseline()
		return err
	}
	r.server.foldCreated(r)
	r.server.recordWorkerOps(r.manifest.WorkerOpsPlanID, r.workerProof)
	return nil
}

// revertLKG puts the last known good file set back and reloads it. It is the
// controller's answer to a plan its own haproxy -c rejected after the fact.
func (r *applyRun) revertLKG() error {
	r.server.setPhase(phaseBackedUp, r.manifest.PlanID)
	if err := r.server.restoreJournal(); err != nil {
		return r.abort("revert", err)
	}
	r.server.metrics.rollbacks.Inc()
	r.result.Rollback = &api.RollbackInfo{Performed: true}
	lkgID, lkgProof := r.server.lkgPlan()
	if err := r.performReload(lkgID, lkgProof); err != nil {
		return r.abort("revert_reload", err)
	}
	r.result.Rollback.Reloaded = true
	r.result.Mode = api.ResultReload
	return nil
}

// abort restores the last known good set. The recovery reload runs only when
// an op already changed the running worker, because otherwise the worker never
// saw this plan at all.
func (r *applyRun) abort(stage string, cause error) error {
	r.server.metrics.rejected.WithLabelValues(stage).Inc()
	r.server.metrics.rollbacks.Inc()
	r.result.OK = false
	r.result.Mode = api.ResultRejected
	r.result.Error = &api.ApplyError{Stage: stage, Message: cause.Error()}
	r.result.Rollback = &api.RollbackInfo{}

	if r.tx != nil {
		r.tx.Discard()
	}
	if err := r.server.restoreJournal(); err != nil {
		r.server.logger.Error("restoring the last known good set failed", "error", err)
		return errors.Join(cause, err)
	}
	r.result.Rollback.Performed = true
	if r.opsRan {
		lkgID, lkgProof := r.server.lkgPlan()
		if err := r.performReload(lkgID, lkgProof); err != nil {
			r.server.metrics.invariant(false, "recovery_reload")
			r.server.logger.Error("the recovery reload failed; the worker is running an unknown set", "error", err)
		} else {
			r.result.Rollback.Reloaded = true
		}
	}
	if r.deterministic {
		r.server.rememberNACK(r.digest, r.work, stage, &r.result)
	}
	return cause
}

func failedKind(results []api.OpResult) string {
	for _, result := range results {
		if !result.OK {
			return result.Kind
		}
	}
	return "unknown"
}

// finish commits the outcome to the state file and checks the invariants that
// hold over a completed apply.
func (s *Server) finish(run *applyRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := s.state.Generation
	if run.result.OK {
		s.state.Generation++
		s.commitLocked(run)
	} else {
		s.state.AppliedPlanID = ""
		s.state.AppliedPlanProof = ""
	}
	s.state.Phase = phaseIdle
	s.state.InFlightPlanID = ""
	s.applyResultLocked(&run.result)
	s.state.LastApply = &run.result
	if err := s.states.save(s.state); err != nil {
		s.logger.Error("could not persist the agent state", "error", err)
	}
	s.metrics.applies.WithLabelValues(run.result.Mode).Inc()
	s.metrics.generation.Set(float64(s.state.Generation))
	s.checkInvariantsLocked(run, before)
}

// commitLocked records what a successful apply means for the baseline. A
// revert lands the last known good set, whose paths and digests restoreJournal
// already recorded, not the set the manifest names.
func (s *Server) commitLocked(run *applyRun) {
	applied := run.manifest.PlanID
	appliedProof := run.appliedProof
	if run.manifest.Mode == api.ModeRevertLKG {
		applied = s.state.LKGPlanID
		appliedProof = s.state.LKGPlanProof
	} else {
		s.state.PlanSchemaVersion = run.manifest.PlanSchemaVersion
		s.state.ManifestPaths = manifestPaths(run.manifest)
		s.state.TreeDigest = treeDigest(s.tree)
	}
	if s.baselineInvalidations != run.invalidations {
		s.state.AppliedPlanID = ""
		s.state.AppliedPlanProof = ""
		return
	}
	s.state.AppliedPlanID, s.state.AppliedPlanProof, s.state.AppliedToken = applied, appliedProof, run.manifest.Token
	// With no reload pending, every op of the apply ran on the worker the
	// controller composed against, so the worker holds the applied plan. While
	// one is pending only the in-place batch advances the worker.
	if s.state.ReloadPendingAt.IsZero() {
		s.state.WorkerOpsPlanID = applied
		s.state.WorkerOpsPlanProof = appliedProof
	}
}

// applyResultLocked fills the fields every response reports from the state.
func (s *Server) applyResultLocked(result *api.ApplyResult) {
	result.AppliedPlanID = s.state.AppliedPlanID
	result.AppliedPlanProof = s.state.AppliedPlanProof
	result.RunningPlanID = s.state.RunningPlanID
	result.RunningPlanProof = s.state.RunningPlanProof
	result.WorkerOpsPlanID = s.state.WorkerOpsPlanID
	result.WorkerOpsPlanProof = s.state.WorkerOpsPlanProof
	result.AppliedToken = s.state.AppliedToken
	result.LKGPlanID = s.state.LKGPlanID
	result.LKGPlanProof = s.state.LKGPlanProof
	result.HAProxy = s.worker
	if s.inventory.Generation > s.reportedInventory {
		inventory := s.inventory
		result.Inventory = &inventory
		s.reportedInventory = s.inventory.Generation
	}
}

func manifestPaths(m *api.Manifest) []string {
	paths := make([]string, 0, len(m.Files))
	for _, f := range m.Files {
		paths = append(paths, f.Path)
	}
	sort.Strings(paths)
	return paths
}

// checkInvariantsLocked states, over a completed apply, what must be true of
// the pair (mode, effects).
func (s *Server) checkInvariantsLocked(run *applyRun, generationBefore uint64) {
	m := s.metrics
	if run.result.OK {
		m.invariant(s.state.Generation == generationBefore+1, "generation_monotonic")
		// A revert's desired set is the last known good one, which the
		// manifest it arrived on does not describe.
		if run.manifest.Mode != api.ModeRevertLKG {
			m.invariant(s.treeMatchesLocked(run.manifest), "disk_is_the_desired_set")
		}
	}
	reloaded := run.result.Reload != nil && (run.result.Reload.Performed || run.result.Reload.ScheduledAt != "")
	switch run.result.Mode {
	case api.ResultRuntime:
		m.invariant(!reloaded, "runtime_mode_does_not_reload")
	case api.ResultReload:
		m.invariant(reloaded, "reload_mode_reloads")
	case api.ResultNoop:
		m.invariant(len(run.result.OpResults) == 0, "noop_runs_no_ops")
	}
	// Only this direction holds: a plan id can advance without changing a file
	// (the drift-prevention noop apply), and then there is nothing to back up.
	journalled := !s.state.Journal.Empty()
	diverged := !samePlanRef(
		s.state.AppliedPlanID,
		s.state.AppliedPlanProof,
		s.state.LKGPlanID,
		s.state.LKGPlanProof,
	)
	m.invariant(!journalled || diverged, "journal_only_while_diverged")
	m.invariant(s.store.CrossDeviceCopies() == 0, "mount_probe_found_every_mount")
}

// treeMatchesLocked compares the maintained tree with the manifest. The tree is
// built from verified writes, so this costs no hashing on the apply path.
func (s *Server) treeMatchesLocked(m *api.Manifest) bool {
	if len(s.tree) != len(m.Files) {
		return false
	}
	for _, f := range m.Files {
		at, present := s.tree[f.Path]
		if !present || at.Digest != f.Digest || at.Proof != f.Proof || at.Size != f.Size {
			return false
		}
	}
	return true
}

// readPlanBlob takes the opaque plan part into memory. It is written to disk
// on commit, so what /v1/state hands back always belongs to the applied plan.
func readPlanBlob(part io.Reader) ([]byte, error) {
	blob, err := io.ReadAll(io.LimitReader(part, api.MaxPlanBlobBytes+1))
	if err != nil {
		return nil, err
	}
	if len(blob) > api.MaxPlanBlobBytes {
		return nil, fmt.Errorf("plan blob exceeds the %d-byte limit", api.MaxPlanBlobBytes)
	}
	return blob, nil
}

// commitPlanBlob keeps the plan of the apply that just landed, next to the
// state file. A blob for any other plan id is dropped: it describes a set this
// pod is not on, and the controller reads it back as a baseline.
func (s *Server) commitPlanBlob(run *applyRun) {
	if run.planBlob == nil {
		s.rebindPlanBlobProof(run)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.storePlanBlobLocked(run.manifest.PlanID, run.appliedProof, run.planBlob)
}

// storePlanBlobLocked keeps blob as the plan of the pod's applied plan, when
// id and proof still name it. Callers hold s.mu.
func (s *Server) storePlanBlobLocked(id, proof string, blob []byte) bool {
	if !samePlanRef(s.state.AppliedPlanID, s.state.AppliedPlanProof, id, proof) {
		return false
	}
	if err := os.WriteFile(s.planBlobPath(), blob, 0o600); err != nil {
		s.logger.Error("could not store the applied plan", "error", err)
		return false
	}
	s.appliedPlan = blob
	s.state.PlanBlobPlanID = id
	s.state.PlanBlobPlanProof = proof
	if err := s.states.save(s.state); err != nil {
		s.logger.Error("could not persist the agent state", "error", err)
	}
	return true
}

// rebindPlanBlobProof re-binds the stored blob to the proof of an apply that did
// not carry one. The controller sends the blob only when the plan changes, so
// without this the first unchanged apply strands it: the bytes still describe
// the applied plan, but the proof no longer matches, the pod stops handing it
// back, and the next leader has no baseline to diff against -- it reloads the
// fleet instead. Only the proof moves; a different plan id still drops it.
func (s *Server) rebindPlanBlobProof(run *applyRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.PlanBlobPlanID == "" || s.state.PlanBlobPlanID != s.state.AppliedPlanID ||
		s.state.AppliedPlanID != run.manifest.PlanID ||
		s.state.AppliedPlanProof != run.appliedProof ||
		s.state.PlanBlobPlanProof == run.appliedProof {
		return
	}
	s.state.PlanBlobPlanProof = run.appliedProof
	if err := s.states.save(s.state); err != nil {
		s.logger.Error("could not persist the agent state", "error", err)
	}
}

// loadPlanBlob reads back the plan blob a previous run stored, so a restarted
// agent still answers the baseline question.
func (s *Server) loadPlanBlob() {
	if s.state.PlanBlobPlanID == "" || s.state.PlanBlobPlanProof == "" {
		return
	}
	blob, err := os.ReadFile(s.planBlobPath())
	if err != nil {
		s.logger.Warn("could not read back the applied plan", "error", err)
		s.state.PlanBlobPlanID = ""
		s.state.PlanBlobPlanProof = ""
		return
	}
	s.appliedPlan = blob
}

func (s *Server) planBlobPath() string {
	return filepath.Join(s.store.BaseDir(), planBlobName)
}
