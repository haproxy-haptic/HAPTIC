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
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"sort"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/files"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
)

// handleApply is the whole write path. The order is load-bearing: the manifest
// is read and fenced before any part touches the disk, so a rejected apply
// leaves the tree exactly as it was.
func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	if !s.Ready() {
		writeJSON(w, http.StatusServiceUnavailable, api.ApplyError{Stage: "startup", Message: "agent is initialising"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, api.MaxApplyBodyBytes)
	reader, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, api.ApplyError{Stage: stageRequest, Message: err.Error()})
		return
	}
	manifest, err := readManifest(reader)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, api.ApplyError{Stage: "manifest", Message: err.Error()})
		return
	}
	started := time.Now()

	s.apply.Lock()
	defer s.apply.Unlock()

	if conflict := s.fence(manifest); conflict != nil {
		s.metrics.rejected.WithLabelValues("fencing").Inc()
		writeJSON(w, http.StatusConflict, conflict)
		return
	}
	if missing := s.missingBlobs(manifest); len(missing) > 0 {
		s.metrics.rejected.WithLabelValues("blobs").Inc()
		writeJSON(w, http.StatusConflict, api.MissingBlobs{Reason: api.ReasonMissingBlobs, MissingBlobs: missing})
		return
	}
	s.stageAndRun(w, reader, manifest, started)
}

// stageAndRun consumes the file parts and hands the request to the state
// machine.
func (s *Server) stageAndRun(w http.ResponseWriter, reader *multipart.Reader, manifest *api.Manifest, started time.Time) {
	got, err := s.stageParts(reader, manifest)
	timing := api.ApplyTiming{StageMs: time.Since(started).Milliseconds()}
	admitStarted := time.Now()
	defer func() {
		for _, part := range got.files {
			part.Discard()
		}
	}()
	if err != nil {
		s.metrics.rejected.WithLabelValues("parts").Inc()
		writeJSON(w, http.StatusBadRequest, api.ApplyError{Stage: "parts", Message: err.Error()})
		return
	}
	if missing := s.missingParts(manifest, got); len(missing) > 0 {
		writeJSON(w, http.StatusConflict, api.Missing{Missing: missing})
		return
	}
	work, workErr := workIdentity(manifest)
	digest := ""
	if workErr != nil {
		s.logger.Warn("could not build the known-bad identity; cache disabled", "error", workErr)
	} else {
		digest = renderplan.Digest(work)
		if cached := s.cachedNACK(digest, work); cached != nil {
			writeJSON(w, http.StatusOK, cached)
			return
		}
	}
	// Promotion comes after every refusal: neither may move the rollback
	// baseline, and clearing the journal is not undoable.
	if err := s.promoteLKG(manifest); err != nil {
		writeJSON(w, http.StatusInternalServerError, api.ApplyError{Stage: "lkg", Message: err.Error()})
		return
	}
	appliedProof, workerProof := "", ""
	if manifest.Mode != api.ModeRevertLKG {
		appliedProof, workerProof, err = s.reserveRoleProofs(len(manifest.InPlaceOps) > 0)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, api.ApplyError{Stage: "identity", Message: err.Error()})
			return
		}
	}
	timing.AdmitMs = time.Since(admitStarted).Milliseconds()
	result := s.runApply(manifest, got, digest, work, appliedProof, workerProof, timing)
	if result.OK {
		s.collectBlobs(started)
	}
	result.Timing.TotalMs = time.Since(started).Milliseconds()
	writeJSON(w, http.StatusOK, result)
}

// readManifest reads the JSON part, which the controller always sends first.
func readManifest(reader *multipart.Reader) (*api.Manifest, error) {
	part, err := reader.NextPart()
	if err != nil {
		return nil, fmt.Errorf("no manifest part: %w", err)
	}
	defer func() { _ = part.Close() }()
	if part.FormName() != api.PartManifest {
		return nil, fmt.Errorf("first part is %q, expected %q", part.FormName(), api.PartManifest)
	}
	raw, err := io.ReadAll(io.LimitReader(part, api.MaxPlanBlobBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > api.MaxPlanBlobBytes {
		return nil, fmt.Errorf("manifest exceeds the %d-byte limit", api.MaxPlanBlobBytes)
	}
	manifest := &api.Manifest{}
	if err := json.Unmarshal(raw, manifest); err != nil {
		return nil, err
	}
	if err := validateManifest(manifest); err != nil {
		return nil, err
	}
	if manifest.Mode == api.ModeAuto && manifest.ExpectedWorkerOpsPlanProof == "" && len(manifest.InPlaceOps) == 0 {
		manifest.Mode = api.ModeReload
		manifest.Ops = nil
		manifest.OpBatches = nil
	}
	return manifest, nil
}

// workIdentity keys the known-bad cache: the desired set and the ops, with a
// file standing for the digest the agent verified when it staged or last
// observed it. How a file arrived (whole or as a patch) is not part of the work.
func workIdentity(m *api.Manifest) ([]byte, error) {
	declared := make([]api.File, len(m.Files))
	for i, file := range m.Files {
		file.Patch = nil
		declared[i] = file
	}
	return json.Marshal(struct {
		IdentityVersion            int        `json:"identity_version"`
		PlanID                     string     `json:"plan_id"`
		PlanProof                  string     `json:"plan_proof"`
		PlanSchemaVersion          int        `json:"plan_schema_version"`
		Files                      []api.File `json:"files"`
		RequiredBlobs              []string   `json:"required_blobs,omitempty"`
		Ops                        []api.Op   `json:"ops"`
		OpBatches                  [][]api.Op `json:"op_batches"`
		InPlaceOps                 []api.Op   `json:"in_place_ops"`
		ExpectedWorkerOpsPlanID    string     `json:"expected_worker_ops_plan_id"`
		ExpectedWorkerOpsPlanProof string     `json:"expected_worker_ops_plan_proof"`
		WorkerOpsPlanID            string     `json:"worker_ops_plan_id"`
		Mode                       string     `json:"mode"`
	}{
		IdentityVersion:            m.IdentityVersion,
		PlanID:                     m.PlanID,
		PlanProof:                  m.PlanProof,
		PlanSchemaVersion:          m.PlanSchemaVersion,
		Files:                      declared,
		RequiredBlobs:              m.RequiredBlobs,
		Ops:                        m.Ops,
		OpBatches:                  m.OpBatches,
		InPlaceOps:                 m.InPlaceOps,
		ExpectedWorkerOpsPlanID:    m.ExpectedWorkerOpsPlanID,
		ExpectedWorkerOpsPlanProof: m.ExpectedWorkerOpsPlanProof,
		WorkerOpsPlanID:            m.WorkerOpsPlanID,
		Mode:                       m.Mode,
	})
}

// validateManifest enforces the wire limits and the path rules. Everything it
// rejects is a controller bug, so it fails loudly rather than degrading.
func validateManifest(m *api.Manifest) error {
	if err := m.ValidateOpBatches(); err != nil {
		return err
	}
	switch {
	case m.IdentityVersion != api.ExactIdentityVersion:
		return fmt.Errorf("identity_version %d is unsupported; this agent requires %d", m.IdentityVersion, api.ExactIdentityVersion)
	case m.PlanID == "":
		return errors.New("plan_id is empty")
	case len(m.Files) > api.MaxFiles:
		return fmt.Errorf("%d files exceed the %d-file limit", len(m.Files), api.MaxFiles)
	case len(m.InPlaceOps) > 0 && (m.ExpectedWorkerOpsPlanID == "" || m.WorkerOpsPlanID == ""):
		return errors.New("in-place ops need expected_worker_ops_plan_id and worker_ops_plan_id")
	case len(m.InPlaceOps) > 0 && m.ExpectedWorkerOpsPlanProof == "":
		return errors.New("in-place ops need an exact expected worker plan proof")
	case (m.ValidatedPlanID == "") != (m.ValidatedPlanProof == ""):
		return errors.New("validated plan id and proof must be set together")
	case m.Mode == api.ModeRevertLKG && m.PlanProof == "":
		return errors.New("a revert needs the refused plan proof")
	}
	if err := validateEnumeratedMode(m.Mode); err != nil {
		return err
	}
	if err := m.ValidateRequiredBlobs(); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(m.Files))
	for _, f := range m.Files {
		if err := files.ValidatePath(f.Path); err != nil {
			return err
		}
		if _, duplicate := seen[f.Path]; duplicate {
			return fmt.Errorf("path %q appears twice in the manifest", f.Path)
		}
		seen[f.Path] = struct{}{}
		if f.Digest == "" {
			return fmt.Errorf("file %q has no digest", f.Path)
		}
	}
	return nil
}

func validateEnumeratedMode(mode string) error {
	switch mode {
	case api.ModeAuto, api.ModeReload, api.ModeRevertLKG:
		return nil
	}
	return fmt.Errorf("unknown mode %q", mode)
}

const reasonPrevMismatch = "prev_mismatch"

// The apply lock keeps the worker baseline fixed from this gate through activation.
func (s *Server) fence(m *api.Manifest) *api.Conflict {
	s.mu.Lock()
	defer s.mu.Unlock()
	reason := ""
	switch {
	case m.Token.LeaderEpoch < s.state.AppliedToken.LeaderEpoch:
		reason = "stale_epoch"
	case m.Mode == api.ModeRevertLKG:
		if !s.carriesRefusedPlanLocked(m.PlanID, m.PlanProof) {
			reason = "revert_target_mismatch"
		}
	case m.ExpectedPrevPlanID != s.state.AppliedPlanID:
		reason = reasonPrevMismatch
		if s.state.AppliedPlanID == "" {
			reason = "unknown_baseline"
		}
	case m.ExpectedPrevToken != s.state.AppliedToken:
		reason = reasonPrevMismatch
	case m.Mode != api.ModeReload &&
		(m.ExpectedPrevPlanProof == "" || m.ExpectedPrevPlanProof != s.state.AppliedPlanProof):
		reason = reasonPrevMismatch
	case m.Mode == api.ModeReload &&
		s.state.AppliedPlanProof != "" && m.ExpectedPrevPlanProof != s.state.AppliedPlanProof:
		reason = reasonPrevMismatch
	case (m.Mode == api.ModeAuto || s.inPlaceWillRunLocked(m)) && !samePlanRef(
		m.ExpectedWorkerOpsPlanID, m.ExpectedWorkerOpsPlanProof, s.state.WorkerOpsPlanID, s.state.WorkerOpsPlanProof):
		reason = "worker_ops_mismatch"
	}
	if reason == "" {
		return nil
	}
	return &api.Conflict{
		AppliedPlanID:      s.state.AppliedPlanID,
		AppliedPlanProof:   s.state.AppliedPlanProof,
		AppliedToken:       s.state.AppliedToken,
		RunningPlanID:      s.state.RunningPlanID,
		RunningPlanProof:   s.state.RunningPlanProof,
		WorkerOpsPlanID:    s.state.WorkerOpsPlanID,
		WorkerOpsPlanProof: s.state.WorkerOpsPlanProof,
		LKGPlanID:          s.state.LKGPlanID,
		LKGPlanProof:       s.state.LKGPlanProof,
		Reason:             reason,
	}
}

func (s *Server) carriesRefusedPlanLocked(planID, proof string) bool {
	if proof == "" || samePlanRef(planID, proof, s.state.RunningPlanID, s.state.RunningPlanProof) {
		return false
	}
	return samePlanRef(planID, proof, s.state.AppliedPlanID, s.state.AppliedPlanProof) ||
		samePlanRef(planID, proof, s.state.WorkerOpsPlanID, s.state.WorkerOpsPlanProof)
}

func samePlanRef(leftID, leftProof, rightID, rightProof string) bool {
	return leftProof != "" && rightProof != "" && leftID == rightID && leftProof == rightProof
}

// inPlaceWillRunLocked mirrors activate: the in-place batch runs while a
// reload is pending, or when this apply asks for a reload the pod has to pace.
func (s *Server) inPlaceWillRunLocked(m *api.Manifest) bool {
	if len(m.InPlaceOps) == 0 {
		return false
	}
	if !s.state.ReloadPendingAt.IsZero() {
		return true
	}
	return m.Mode == api.ModeReload && time.Now().Before(s.lastReload.Add(s.cfg.ReloadIntervalMin))
}

// received is what the parts of one apply carry: the verified file contents,
// staged in their mounts, the paths whose patch found no base to splice into,
// and the opaque plan blob.
type received struct {
	files     map[string]*files.Staged
	unpatched map[string]bool
	plan      []byte
}

// stageParts writes every received part into its mount's temp directory and
// verifies it against the manifest digest before it can reach the tree.
func (s *Server) stageParts(reader *multipart.Reader, m *api.Manifest) (*received, error) {
	declared := make(map[string]api.File, len(m.Files))
	for _, f := range m.Files {
		declared[f.Path] = f
	}
	got := &received{files: map[string]*files.Staged{}, unpatched: map[string]bool{}}
	for count := 0; count <= api.MaxFiles; count++ {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return got, nil
		}
		if err != nil {
			return got, err
		}
		err = s.stagePart(part, declared, got)
		_ = part.Close()
		if err != nil {
			return got, err
		}
	}
	return got, fmt.Errorf("more than %d parts", api.MaxFiles)
}

func (s *Server) stagePart(part *multipart.Part, declared map[string]api.File, got *received) error {
	if part.FormName() == api.PartPlan {
		blob, err := readPlanBlob(part)
		got.plan = blob
		return err
	}
	path, err := partPath(part)
	if err != nil {
		return err
	}
	declaration, known := declared[path]
	if !known {
		return fmt.Errorf("part %q is not in the manifest", path)
	}
	if _, duplicate := got.files[path]; duplicate || got.unpatched[path] {
		return fmt.Errorf("part %q appears twice", path)
	}
	var verified *files.Staged
	if declaration.Patch != nil {
		verified, err = s.store.StagePatch(path, part, declaration.Patch, declaration.Digest, declaration.Size)
		if errors.Is(err, files.ErrPatchBaseMissing) {
			// The whole file follows once the controller learns this.
			s.logger.Info("patch base is not the file held, asking for the whole file", "path", path, "error", err)
			got.unpatched[path] = true
			return nil
		}
	} else {
		verified, err = s.store.Stage(path, part, declaration.Digest, declaration.Size)
	}
	if err != nil {
		return err
	}
	got.files[path] = verified
	return nil
}

// partPath reads the manifest path out of the part's Content-Disposition.
// multipart.Part.FileName strips the directory, which would collapse every
// map file of a manifest onto its base name.
func partPath(part *multipart.Part) (string, error) {
	_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	if err != nil {
		return "", fmt.Errorf("part has no usable Content-Disposition: %w", err)
	}
	path := params["filename"]
	if path == "" {
		return "", errors.New("a file part carries no filename")
	}
	return path, nil
}

// missingParts names the files whose content the agent does not hold, a
// patch without its base among them. The controller resends exactly these,
// whole.
func (s *Server) missingParts(m *api.Manifest, got *received) []string {
	s.mu.Lock()
	tree := s.tree
	s.mu.Unlock()

	var missing []string
	for _, f := range m.Files {
		if _, have := got.files[f.Path]; have {
			continue
		}
		if got.unpatched[f.Path] {
			missing = append(missing, f.Path)
			continue
		}
		if at, held := tree[f.Path]; held && f.Proof != "" && at.Proof == f.Proof &&
			at.Digest == f.Digest && at.Size == f.Size {
			continue
		}
		missing = append(missing, f.Path)
	}
	sort.Strings(missing)
	return missing
}
