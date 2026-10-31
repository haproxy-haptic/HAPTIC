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

package planblob_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/compression"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/planblob"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
)

// A checkpoint lands in a readable HAProxyCfg, so a sealed file's digest is
// restored from the file's own bytes rather than stored.
func TestCheckpointOmitsSealedFileDigests(t *testing.T) {
	config := "global\n"
	secret := "hunter2"
	plan := &renderplan.Plan{
		SchemaVersion: renderplan.SchemaVersion,
		Sections:      []renderplan.Section{{Kind: renderplan.SectionKindCore, Name: "core#0", Text: config, TextKnown: true, Length: len(config), TextDigest: renderplan.DigestString(config)}},
		Files: []renderplan.File{
			{Path: renderplan.ConfigFilePath, Kind: renderplan.FileKindConfig, ReloadOnChange: true, Digest: renderplan.DigestString(config), Size: int64(len(config)), Content: config, ContentKnown: true},
			{Path: "secrets/x", Kind: renderplan.FileKindGeneral, Digest: renderplan.DigestString(secret), Size: int64(len(secret)), Seal: "seal", Content: secret, ContentKnown: true},
		},
	}
	plan.ComputeID()
	encoded, err := planblob.EncodeCheckpoint(plan)
	require.NoError(t, err)
	decompressed, err := compression.Decompress(encoded)
	require.NoError(t, err)
	require.NotContains(t, decompressed, renderplan.DigestString(secret))

	files := map[string]string{renderplan.ConfigFilePath: config, "secrets/x": secret}
	restored, err := planblob.DecodeCheckpoint(encoded, config, files)
	require.NoError(t, err)
	require.True(t, renderplan.ExactlyEqual(plan, restored))

	files["secrets/x"] = "short"
	_, err = planblob.DecodeCheckpoint(encoded, config, files)
	require.ErrorContains(t, err, "differs from its plan")
}

func TestCheckpointPreservesStructuredBackendDeclarations(t *testing.T) {
	plan := fleetPlan()
	plan.Sections = append([]renderplan.Section{{Kind: renderplan.SectionKindCore, Name: "core#0", Text: "global\n", TextKnown: true, Length: 7, TextDigest: renderplan.DigestString("global\n")}}, plan.Sections...)
	backend := plan.Backends["be-0000"]
	backend.Body = []string{"balance roundrobin"}
	backend.Comments = []string{"# retained declaration"}
	plan.Backends["be-0000"] = backend
	plan.ComputeID()
	config := plan.Files[0].Content
	files := map[string]string{}
	for _, file := range plan.Files {
		files[file.Path] = file.Content
	}
	encoded, err := planblob.EncodeCheckpoint(plan)
	require.NoError(t, err)
	restored, err := planblob.DecodeCheckpoint(encoded, config, files)
	require.NoError(t, err)
	require.True(t, renderplan.ExactlyEqual(plan, restored))
	require.Equal(t, backend.Body, restored.Backends["be-0000"].Body)
	require.Equal(t, backend.Comments, restored.Backends["be-0000"].Comments)
	_, err = planblob.DecodeCheckpoint(encoded, config+"extra", files)
	require.ErrorContains(t, err, "sections")
	delete(files, "haproxy.cfg")
	_, err = planblob.DecodeCheckpoint(encoded, config, files)
	require.ErrorContains(t, err, "checkpoint file")
}
