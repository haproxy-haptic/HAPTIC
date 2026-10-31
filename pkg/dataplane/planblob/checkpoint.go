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

package planblob

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/haproxy-haptic/HAPTIC/pkg/compression"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
)

type checkpoint struct {
	Plan     *renderplan.Plan          `json:"plan"`
	Backends map[string]backendContent `json:"backends"`
}

type backendContent struct {
	Body     []string `json:"body"`
	Comments []string `json:"comments"`
}

// EncodeCheckpoint preserves declarations that cannot be recovered from file bytes.
func EncodeCheckpoint(plan *renderplan.Plan) (string, error) {
	if !renderplan.ExactlyEqual(plan, plan) {
		return "", errors.New("checkpoint plan has no exact content")
	}
	readable := *plan
	readable.Files = renderplan.ClearSealedDigests(plan.Files)
	value := checkpoint{Plan: &readable, Backends: make(map[string]backendContent, len(plan.Backends))}
	for name := range plan.Backends {
		backend := plan.Backends[name]
		value.Backends[name] = backendContent{Body: backend.Body, Comments: backend.Comments}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encoding checkpoint plan: %w", err)
	}
	return compression.Compress(string(encoded)), nil
}

// DecodeCheckpoint restores exact bytes without parsing HAProxy configuration.
// The caller must bind the result to an authenticated renderoutput snapshot.
func DecodeCheckpoint(encoded, config string, files map[string]string) (*renderplan.Plan, error) {
	data, err := compression.Decompress(encoded)
	if err != nil {
		return nil, fmt.Errorf("decompressing checkpoint plan: %w", err)
	}
	var value checkpoint
	if err := json.Unmarshal([]byte(data), &value); err != nil {
		return nil, fmt.Errorf("decoding checkpoint plan: %w", err)
	}
	plan := value.Plan
	if plan == nil || plan.SchemaVersion != renderplan.SchemaVersion ||
		plan.ID == "" || plan.ID != renderplan.Digest(plan.Canonical()) {
		return nil, errors.New("checkpoint plan identity is invalid")
	}
	if err := restoreSections(plan, config); err != nil {
		return nil, err
	}
	for i := range plan.Files {
		file := &plan.Files[i]
		content, present := files[file.Path]
		if file.Seal != "" && file.Digest == "" {
			file.Digest = renderplan.DigestString(content)
		}
		if !present || file.Size != int64(len(content)) || file.Digest != renderplan.DigestString(content) {
			return nil, fmt.Errorf("checkpoint file %q differs from its plan", file.Path)
		}
		file.Content, file.ContentKnown = content, true
	}
	if len(files) != len(plan.Files) || len(value.Backends) != len(plan.Backends) {
		return nil, errors.New("checkpoint plan has an incomplete content set")
	}
	for name := range plan.Backends {
		backend := plan.Backends[name]
		content, present := value.Backends[name]
		if !present {
			return nil, fmt.Errorf("checkpoint backend %q has no content", name)
		}
		backend.Body, backend.Comments, backend.ContentKnown = content.Body, content.Comments, true
		plan.Backends[name] = backend
	}
	return plan, nil
}

func restoreSections(plan *renderplan.Plan, config string) error {
	offset := 0
	for i := range plan.Sections {
		section := &plan.Sections[i]
		if section.Length < 0 || section.Length > len(config)-offset {
			return fmt.Errorf("checkpoint section %d has an invalid length", i)
		}
		section.Text = config[offset : offset+section.Length]
		section.TextKnown = true
		if section.TextDigest != renderplan.DigestString(section.Text) {
			return fmt.Errorf("checkpoint section %d differs from its plan", i)
		}
		offset += section.Length
	}
	if offset != len(config) {
		return errors.New("checkpoint sections do not cover the configuration")
	}
	return nil
}
