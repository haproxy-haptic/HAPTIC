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
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/files"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
)

// phase names the step an in-flight apply reached. It is persisted, so a
// restart knows whether the tree can be a mix of two plans.
type phase string

const (
	phaseIdle      phase = ""
	phaseStaged    phase = "staged"
	phaseVerified  phase = "verified"
	phaseBackedUp  phase = "backed_up"
	phaseWritten   phase = "written"
	phaseApplied   phase = "applied"
	phaseReloaded  phase = "reloaded"
	phaseScheduled phase = "scheduled"
	phaseCommitted phase = "committed"
	phaseAborted   phase = "aborted"
)

// persistentState is `.haptic-agent.json`. It carries no per-file digests:
// disk is the authority and the tree digest is a single observation of it.
type persistentState struct {
	Generation             uint64             `json:"generation"`
	ProofGeneration        uint64             `json:"proof_generation"`
	PlanSchemaVersion      int                `json:"plan_schema_version"`
	AppliedPlanID          string             `json:"applied_plan_id"`
	AppliedPlanProof       string             `json:"applied_plan_proof,omitempty"`
	RunningPlanID          string             `json:"running_plan_id"`
	RunningPlanProof       string             `json:"running_plan_proof,omitempty"`
	WorkerOpsPlanID        string             `json:"worker_ops_plan_id"`
	WorkerOpsPlanProof     string             `json:"worker_ops_plan_proof,omitempty"`
	LKGPlanID              string             `json:"lkg_plan_id"`
	LKGPlanProof           string             `json:"lkg_plan_proof,omitempty"`
	AppliedToken           api.Token          `json:"applied_token"`
	ManifestPaths          []string           `json:"manifest_paths,omitempty"`
	TreeDigest             string             `json:"tree_digest,omitempty"`
	ExpectedWorker         api.HAProxyInfo    `json:"expected_worker"`
	Journal                files.Journal      `json:"journal"`
	Phase                  phase              `json:"phase,omitempty"`
	InFlightPlanID         string             `json:"in_flight_plan_id,omitempty"`
	PendingReloadPlanID    string             `json:"pending_reload_plan_id,omitempty"`
	PendingReloadPlanProof string             `json:"pending_reload_plan_proof,omitempty"`
	PlanBlobPlanID         string             `json:"plan_blob_plan_id,omitempty"`
	PlanBlobPlanProof      string             `json:"plan_blob_plan_proof,omitempty"`
	NACK                   *nackRecord        `json:"nack,omitempty"`
	LastApply              *api.ApplyResult   `json:"last_apply,omitempty"`
	ReloadPendingAt        time.Time          `json:"reload_pending_at,omitzero"`
	PendingDeletes         api.PendingDeletes `json:"pending_deletes"`
	// TreeBlobs and LKGBlobs are the blobs the tree's and the last known good
	// set's configuration require; GC keeps both.
	TreeBlobs []string `json:"tree_blobs,omitempty"`
	LKGBlobs  []string `json:"lkg_blobs,omitempty"`
}

// nackRecord remembers a manifest HAProxy itself rejected, so re-sending the
// same bytes inside the cooldown does no work at all.
type nackRecord struct {
	ManifestDigest string           `json:"manifest_digest"`
	ManifestWork   []byte           `json:"-"`
	Reason         string           `json:"reason"`
	Until          time.Time        `json:"until"`
	Result         *api.ApplyResult `json:"result,omitempty"`
}

// stateStore reads and writes the agent's state file with a temp-and-rename,
// so a crash never leaves a half-written state behind.
type stateStore struct {
	path string
}

func newStateStore(baseDir, name string) *stateStore {
	return &stateStore{path: filepath.Join(baseDir, name)}
}

// load returns an empty state when the file is absent. A corrupt file is a
// refusal to guess: the caller rebuilds the baseline from disk instead.
func (s *stateStore) load() (*persistentState, error) {
	raw, err := os.ReadFile(filepath.Clean(s.path))
	if errors.Is(err, fs.ErrNotExist) {
		return &persistentState{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.path, err)
	}
	state := &persistentState{}
	if err := json.Unmarshal(raw, state); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.path, err)
	}
	return state, nil
}

func (s *stateStore) save(state *persistentState) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode agent state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".haptic-agent-state-")
	if err != nil {
		return fmt.Errorf("write %s: %w", s.path, err)
	}
	_, writeErr := tmp.Write(raw)
	err = errors.Join(writeErr, tmp.Chmod(0o600), tmp.Close())
	if err == nil {
		err = os.Rename(tmp.Name(), s.path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write %s: %w", s.path, err)
	}
	return nil
}

// treeDigest folds the observed tree into one value, which is what the state
// file compares against at startup to decide whether its baseline still holds.
func treeDigest(tree map[string]api.FileAt) string {
	paths := make([]string, 0, len(tree))
	for path := range tree {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var b strings.Builder
	for _, path := range paths {
		b.WriteString(path)
		b.WriteByte(' ')
		b.WriteString(tree[path].Digest)
		b.WriteByte('\n')
	}
	return renderplan.DigestString(b.String())
}
