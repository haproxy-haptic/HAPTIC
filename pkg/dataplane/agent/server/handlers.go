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
	"encoding/json"
	"maps"
	"net/http"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/cli"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/files"
)

// handleState answers the controller's view of this pod. With verify=1 the
// digests are a fresh observation of the tree rather than the last one.
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	// A baseline is usable only after disk adoption and crash recovery finish.
	select {
	case <-s.initialized:
	case <-r.Context().Done():
		return
	}
	state, err := s.stateResponse(r.URL.Query().Get("verify") == "1", r.URL.Query().Get("plan") != "0")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, api.ApplyError{Stage: "state", Message: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// stateResponse reports the agent's state; with verify it re-observes the tree
// and the worker identity, so drift prevention sees a restarted HAProxy
// container without waiting for the next apply. withPlan carries the stored
// plan blob, which only a controller without the plan needs: at 2,600 routes it
// is 360 KB on every read.
func (s *Server) stateResponse(verify, withPlan bool) (api.State, error) {
	if verify {
		if err := s.reobserve(); err != nil {
			return api.State{}, err
		}
	}
	held, err := s.heldBlobs()
	if err != nil {
		return api.State{}, err
	}
	features := []string{api.FeatureFilePatch, api.FeatureWorkerFence, api.FeatureRuntimeBatches}
	if s.blobs != nil {
		features = append(features, api.FeatureBlobs)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := api.State{
		APIVersion:         api.Version,
		AgentVersion:       s.cfg.AgentVersion,
		PlanSchemaVersion:  s.state.PlanSchemaVersion,
		AgentOps:           cli.Kinds(),
		Features:           features,
		HAProxy:            s.worker,
		Generation:         s.state.Generation,
		AppliedPlanID:      s.state.AppliedPlanID,
		AppliedPlanProof:   s.state.AppliedPlanProof,
		RunningPlanID:      s.state.RunningPlanID,
		RunningPlanProof:   s.state.RunningPlanProof,
		WorkerOpsPlanID:    s.state.WorkerOpsPlanID,
		WorkerOpsPlanProof: s.state.WorkerOpsPlanProof,
		AppliedToken:       s.state.AppliedToken,
		LKGPlanID:          s.state.LKGPlanID,
		LKGPlanProof:       s.state.LKGPlanProof,
		// Cloned: the apply path mutates the tree in place and the answer is
		// marshalled after this lock is released.
		Files:              maps.Clone(s.tree),
		Blobs:              held,
		Inventory:          s.inventory,
		PendingDeletes:     s.deferrals.Pending(),
		LastApply:          s.state.LastApply,
		InvariantViolation: s.metrics.LastViolation(),
	}
	if s.state.PlanBlobPlanID != "" && s.state.PlanBlobPlanID == s.state.AppliedPlanID &&
		s.state.PlanBlobPlanProof != "" && s.state.PlanBlobPlanProof == s.state.AppliedPlanProof {
		out.AppliedPlanStored = true
		if withPlan {
			out.AppliedPlan = s.appliedPlan
		}
	}
	if !s.state.ReloadPendingAt.IsZero() {
		out.ReloadPendingAt = s.state.ReloadPendingAt.UTC().Format(time.RFC3339Nano)
	}
	return out, nil
}

// reobserve runs between applies: a re-hash that overlaps one installs the
// file set from before it, and the apply then trips disk_is_the_desired_set.
func (s *Server) reobserve() error {
	s.apply.Lock()
	defer s.apply.Unlock()
	if err := s.refreshTree(); err != nil {
		return err
	}
	if err := s.checkWorker(); err != nil {
		s.logger.Warn("state verify: worker check", "error", err)
	}
	return nil
}

// refreshTree re-hashes the ownership set so the reported digests are an
// observation, which is what drift prevention compares against. A file keeps
// its proof only while it is still the file the proof was issued for: an equal
// digest is not an exact observation.
func (s *Server) refreshTree() error {
	s.mu.Lock()
	paths := append([]string(nil), s.state.ManifestPaths...)
	s.mu.Unlock()

	observed, err := s.store.Observe(paths)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tree := make(map[string]api.FileAt, len(observed))
	prints := make(map[string]files.Fingerprint, len(observed))
	for path, file := range observed {
		if written, recorded := s.treePrints[path]; recorded && written == file.Fingerprint {
			file.At.Proof = s.tree[path].Proof
			prints[path] = written
		}
		tree[path] = file.At
	}
	s.tree = tree
	s.treePrints = prints
	s.state.TreeDigest = treeDigest(tree)
	return nil
}

// handlePlan stores the plan blob for the pod's applied plan after the apply
// that landed it, so the apply itself carries none: the controller encodes and
// uploads the blob in the background, and a cold leader still finds it here.
func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	if !s.Ready() {
		writeJSON(w, http.StatusServiceUnavailable, api.ApplyError{Stage: "startup", Message: "agent is initialising"})
		return
	}
	id, proof := r.URL.Query().Get("plan_id"), r.URL.Query().Get("proof")
	if id == "" || proof == "" {
		writeJSON(w, http.StatusBadRequest, api.ApplyError{Stage: stageRequest, Message: "plan_id and proof are required"})
		return
	}
	blob, err := readPlanBlob(http.MaxBytesReader(w, r.Body, api.MaxPlanBlobBytes+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, api.ApplyError{Stage: "plan", Message: err.Error()})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.storePlanBlobLocked(id, proof, blob) {
		writeJSON(w, http.StatusConflict, api.PlanMoved{
			AppliedPlanID: s.state.AppliedPlanID, AppliedPlanProof: s.state.AppliedPlanProof,
		})
		return
	}
	writeJSON(w, http.StatusOK, api.PlanStored{PlanID: id, Proof: proof})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		writeText(w, http.StatusInternalServerError, "the agent could not encode its answer")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(raw, '\n'))
}

func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body + "\n"))
}
