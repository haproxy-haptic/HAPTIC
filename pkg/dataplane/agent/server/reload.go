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
	"context"
	"errors"
	"slices"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/cli"
)

// pacerTick is how often the agent checks whether a scheduled reload is due.
// A scheduled reload is never cancelled, so the tick only decides its latency.
const pacerTick = 100 * time.Millisecond

// reloadReasonMode is the reload the controller asked for (mode: reload), as
// opposed to the agent's own fallbacks.
const reloadReasonMode = "mode"

// DefaultReloadTimeout bounds the wait for the new worker to answer after a
// reload. Past it the apply reports what it knows rather than blocking.
const DefaultReloadTimeout = api.MaxReloadMs * time.Millisecond

// reload either reloads now or schedules one, depending on the pacing window.
func (r *applyRun) reload(reason string) error {
	r.server.logger.Info("reloading", "plan_id", r.manifest.PlanID, "reason", reason)
	if due, open := r.server.pacingWindow(); open {
		if err := r.schedule(due); err != nil || reason != reloadReasonMode {
			return err
		}
		// The controller decided this reload and composed the in-place subset
		// for the worker that keeps serving until it fires; a fallback reload
		// has no such subset (its ops were refused or composed against a
		// baseline the worker is not on).
		return r.runInPlace()
	}
	if err := r.performReload(r.manifest.PlanID, r.appliedProof); err != nil {
		return r.abort("reload", err)
	}
	r.result.Mode = api.ResultReload
	r.server.setPhase(phaseReloaded, r.manifest.PlanID)
	return nil
}

// schedule defers the reload to the end of the pacing window. The controller
// polls /v1/state to learn when it happened.
func (r *applyRun) schedule(due time.Time) error {
	r.server.schedulePendingReload(due, r.manifest.PlanID, r.appliedProof)
	r.result.Mode = api.ResultScheduled
	r.result.Reload = &api.ReloadInfo{ScheduledAt: due.UTC().Format(time.RFC3339Nano)}
	r.server.setPhase(phaseScheduled, r.manifest.PlanID)
	return nil
}

// performReload asks the master to re-exec and waits until the new worker
// answers, because an op sent to the outgoing worker would be lost. planID
// names the file set the new worker starts from, which is not always the plan
// this apply carried: a rollback reloads the last known good one.
func (r *applyRun) performReload(planID, planProof string) error {
	r.server.workerObservation.Lock()
	defer r.server.workerObservation.Unlock()
	// A reload consumes the scheduled one: it re-executes from the tree as it
	// is now, which after a rollback is no longer the scheduled plan's.
	r.server.clearPendingReload()
	start := time.Now()
	logs, err := r.server.runtime.Reload()
	info := &api.ReloadInfo{Performed: true, OK: err == nil, Output: logs, TookMs: time.Since(start).Milliseconds()}
	r.result.Reload = info
	if err != nil {
		r.server.metrics.reloads.WithLabelValues("failed").Inc()
		// Only HAProxy's own verdict on these bytes is worth remembering as
		// known-bad; a socket that never answered means it never saw them.
		// The evidence is its startup log, or the master still answering.
		refused := logs != "" || r.server.masterAnswers()
		info.Performed = refused
		r.deterministic = refused
		if logs == "" {
			return err
		}
		return errors.New(logs)
	}
	r.server.metrics.reloads.WithLabelValues("ok").Inc()
	worker, settleErr := r.server.awaitNewWorker()
	info.WorkerPID = worker.WorkerPID
	info.TookMs = time.Since(start).Milliseconds()
	if settleErr != nil {
		return settleErr
	}
	r.server.recordReload(planID, planProof)
	return nil
}

// masterAnswers reports whether the master process is still there, which is
// how a refused reload is told apart from an unreachable socket.
func (s *Server) masterAnswers() bool {
	_, err := s.runtime.ShowProc()
	return err == nil
}

// awaitNewWorker waits for a worker distinct from the recorded process and start time.
func (s *Server) awaitNewWorker() (api.HAProxyInfo, error) {
	previous := s.workerIdentity()
	deadline := time.Now().Add(s.cfg.ReloadTimeout)
	for {
		info, err := s.runtime.Info()
		if err == nil && !info.SameWorker(previous) {
			if err = s.adoptWorker(info); err == nil {
				return info, nil
			}
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return previous, err
		}
		if time.Now().After(deadline) {
			return previous, errors.New("the new worker did not answer show info after the reload")
		}
		time.Sleep(pacerTick)
	}
}

// pacer fires reloads whose window has passed. It is the only input to the
// state machine besides the apply handler.
func (s *Server) pacer(ctx context.Context) error {
	ticker := time.NewTicker(pacerTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.reloadWake:
		case <-ticker.C:
		}
		s.firePendingReload()
	}
}

// firePendingReload performs the reload an earlier apply scheduled. A failure
// here restores the last known good set, exactly like a synchronous one.
func (s *Server) firePendingReload() {
	if !s.Ready() {
		return
	}
	s.apply.Lock()
	defer s.apply.Unlock()
	planID, planProof, due := s.pendingReload()
	if planID == "" || planProof == "" || time.Now().Before(due) {
		return
	}
	run := &applyRun{
		server:   s,
		manifest: &api.Manifest{PlanID: planID, PlanProof: planProof, Mode: api.ModeReload},
		result:   api.ApplyResult{PlanID: planID, OK: true, Mode: api.ResultReload, At: time.Now().UTC().Format(time.RFC3339)},
	}
	if err := run.performReload(planID, planProof); err != nil {
		s.logger.Error("the scheduled reload failed", "plan_id", planID, "error", err)
		_ = run.abort("scheduled_reload", err)
	}
	s.mu.Lock()
	if !run.result.OK {
		s.state.AppliedPlanID = ""
		s.state.AppliedPlanProof = ""
	}
	s.applyResultLocked(&run.result)
	s.state.LastApply = &run.result
	if err := s.states.save(s.state); err != nil {
		s.logger.Error("could not persist the agent state", "error", err)
	}
	s.mu.Unlock()
	s.metrics.applies.WithLabelValues(run.result.Mode).Inc()
}

// readBack compares the running state with the desired one after a runtime
// apply. A lost or truncated command must not latch, so a divergence reloads.
func (s *Server) readBack(run *applyRun) {
	if run.result.Mode != api.ResultRuntime || !run.result.OK || s.stopped.Load() {
		return
	}
	s.apply.Lock()
	defer s.apply.Unlock()
	if s.stopped.Load() {
		return
	}
	diverged := false
	for _, backend := range dedupe(run.touchedBackends) {
		if slices.Contains(run.retiringBackends, backend) {
			continue
		}
		if _, err := s.runtime.ServerNames(backend); err != nil {
			diverged = true
			s.logger.Warn("read-back could not read a backend", "backend", backend, "error", err)
		}
	}
	for _, path := range dedupe(run.touchedMaps) {
		if s.mapDiverged(path) {
			diverged = true
		}
	}
	if !diverged || s.stopped.Load() {
		return
	}
	s.metrics.divergence.Inc()
	due, open := s.pacingWindow()
	if open {
		state := s.snapshot()
		s.schedulePendingReload(due, state.AppliedPlanID, state.AppliedPlanProof)
		return
	}
	s.selfReload()
}

// mapDiverged reports whether the map file on disk and the map the worker
// holds disagree on their key sets. A map too large to read back is reported
// as unverified, not as diverged: reloading over it would make every apply on
// that map a reload without evidence that anything is wrong.
func (s *Server) mapDiverged(path string) bool {
	running, err := s.runtime.MapEntries(path)
	if err != nil {
		s.logger.Warn("read-back could not read a map", "map", path, "error", err)
		return !errors.Is(err, cli.ErrTooManyEntries)
	}
	desired, err := s.readMapFile(path)
	if err != nil {
		s.logger.Warn("read-back could not read a map file", "map", path, "error", err)
		return !errors.Is(err, cli.ErrTooManyEntries)
	}
	if len(running) != len(desired) {
		return true
	}
	for key := range desired {
		if _, present := running[key]; !present {
			return true
		}
	}
	return false
}

// selfReload reloads outside an apply, after a read-back found a divergence.
func (s *Server) selfReload() {
	state := s.snapshot()
	planID, planProof := state.AppliedPlanID, state.AppliedPlanProof
	run := &applyRun{
		server:   s,
		manifest: &api.Manifest{PlanID: planID, PlanProof: planProof, Mode: api.ModeReload},
		result:   api.ApplyResult{PlanID: planID, OK: true, Mode: api.ResultReload},
	}
	if err := run.performReload(planID, planProof); err != nil {
		s.logger.Error("the divergence reload failed", "error", err)
	}
	// The reload cleared the journal on disk, so the state file has to say so
	// before a restart trusts backups that are gone.
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.states.save(s.state); err != nil {
		s.logger.Error("could not persist the agent state", "error", err)
	}
}

func dedupe(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := values[:0]
	for _, v := range values {
		if _, duplicate := seen[v]; duplicate {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
