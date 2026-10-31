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

package deployplan_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/agent/api"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/deployplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
)

// TestDiffGeneralFiles covers rule 7: a general file is written, and only its
// own reload-on-change flag makes it a reload.
func TestDiffGeneralFiles(t *testing.T) {
	tests := []struct {
		name           string
		reloadOnChange bool
		verdict        deployplan.Verdict
	}{
		{name: "written without a reload", verdict: deployplan.VerdictFileOnly},
		{name: "declared reload-on-change", reloadOnChange: true, verdict: deployplan.VerdictReload},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := renderplan.File{
				Path: "general/errors/503.http", Kind: renderplan.FileKindGeneral, ReloadOnChange: tt.reloadOnChange,
			}
			before := withDigest(&file, "before")
			after := withDigest(&file, "after")
			prev := basePlan(withFile(&before))
			next := basePlan(withFile(&after))

			got := deployplan.Diff(next, on34(prev))

			assert.Equal(t, tt.verdict, got.Verdict)
			assert.Empty(t, got.Ops)
		})
	}
}

// TestDiffRemovedFiles pins the other half of rule 7: the agent's ownership set
// makes absence a delete, so a dropped file is judged like a changed one.
func TestDiffRemovedFiles(t *testing.T) {
	tests := []struct {
		name           string
		reloadOnChange bool
		verdict        deployplan.Verdict
		reason         string
	}{
		{
			name:    "removed without a reload",
			verdict: deployplan.VerdictFileOnly,
			reason:  "was removed, which no runtime op undoes",
		},
		{
			name:           "removed while declared reload-on-change",
			reloadOnChange: true,
			verdict:        deployplan.VerdictReload,
			reason:         "was removed and is declared reload-on-change",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prev := basePlan(withFile(&renderplan.File{
				Path: "general/rules/extra.conf", Kind: renderplan.FileKindGeneral,
				Digest: "before", ReloadOnChange: tt.reloadOnChange,
			}))

			got := deployplan.Diff(basePlan(), on34(prev))

			assert.Equal(t, tt.verdict, got.Verdict)
			assert.Empty(t, got.Ops)
			reasonsContain(t, got.Reasons, tt.reason)
		})
	}
}

// TestDiffSealedSecretFiles pins that a secret file is a plain non-reloading
// write: its rotation costs no reload, and the agent still gets the content
// digest it verifies, not the seal the plan's identity uses.
func TestDiffSealedSecretFiles(t *testing.T) {
	secret := func(content, seal string) renderplan.File {
		return renderplan.File{
			Path: "secrets/sso.client-secret", Kind: renderplan.FileKindGeneral,
			Digest: renderplan.DigestString(content), Size: int64(len(content)), Seal: seal,
			Content: content, ContentKnown: true,
		}
	}
	before, after := secret("old", "seal-old"), secret("new", "seal-new")
	prev, next := basePlan(withFile(&before)), basePlan(withFile(&after))

	got := deployplan.Diff(next, on34(prev))

	assert.Equal(t, deployplan.VerdictFileOnly, got.Verdict)
	assert.Empty(t, got.Ops)
	assert.Contains(t, deployplan.Files(next), api.File{
		Path: "secrets/sso.client-secret", Kind: renderplan.FileKindGeneral,
		Digest: renderplan.DigestString("new"), Proof: renderplan.DigestString("new"), Size: 3,
	})
}

func TestFilesProjectsTheWholeSet(t *testing.T) {
	plan := basePlan(
		withFile(&renderplan.File{
			Path: "general/errors/503.http", Kind: renderplan.FileKindGeneral,
			Digest: "d1", Size: 42, ReloadOnChange: true,
		}),
		withMap(renderplan.Map{Path: routeMap, Entries: []renderplan.Entry{entry("a", "1")}}),
	)

	files := deployplan.Files(plan)

	require.Len(t, files, len(plan.Files))
	assert.Equal(t, api.File{
		Path: "general/errors/503.http", Kind: renderplan.FileKindGeneral,
		Digest: "d1", Proof: "d1", Size: 42, ReloadOnChange: true,
	}, files[0])
	assert.Nil(t, deployplan.Files(nil))
}

// TestFilesCarryAProof pins the field the agent needs to answer "I already
// hold this". Without it every file is reported missing on every apply, so the
// pod rewrites its whole tree and no render can ever settle as a noop.
func TestFilesCarryAProof(t *testing.T) {
	plan := basePlan(
		withFile(&renderplan.File{
			Path: "general/errors/503.http", Kind: renderplan.FileKindGeneral,
			Digest: "d1", Size: 42,
		}),
		withMap(renderplan.Map{Path: routeMap, Entries: []renderplan.Entry{entry("a", "1")}}),
	)

	for _, file := range deployplan.Files(plan) {
		assert.NotEmpty(t, file.Proof, "file %s ships no equality witness", file.Path)
	}
}

func withDigest(f *renderplan.File, digest string) renderplan.File {
	cloned := *f
	cloned.Digest = digest
	return cloned
}
