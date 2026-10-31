// Copyright 2026 Philipp Hossner
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

package dataplane

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderartifact"
)

func secretFiles(content, seal string) *AuxiliaryFiles {
	return &AuxiliaryFiles{SecretFiles: []auxiliaryfiles.SecretFile{{Path: "secrets/x", Content: content, Seal: seal}}}
}

// The content checksum is readable in HAProxyCfg, so it hashes a secret
// file's seal and never its content; equality still compares the bytes.
func TestSecretFileIdentityUsesTheSeal(t *testing.T) {
	tests := []struct {
		name         string
		left, right  *AuxiliaryFiles
		sameChecksum bool
		equal        bool
	}{
		{name: "identical", left: secretFiles("a", "s"), right: secretFiles("a", "s"), sameChecksum: true, equal: true},
		{name: "content differs under one seal", left: secretFiles("a", "s"), right: secretFiles("b", "s"), sameChecksum: true},
		{name: "seal differs", left: secretFiles("a", "s1"), right: secretFiles("a", "s2")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			left, right := ComputeContentChecksum("cfg", tt.left), ComputeContentChecksum("cfg", tt.right)
			assert.Equal(t, tt.sameChecksum, left == right)
			assert.Equal(t, tt.equal, ContentEqual("cfg", tt.left, "cfg", tt.right))

			leftSnapshot, rightSnapshot := buildForeignSnapshot(t, tt.left), buildForeignSnapshot(t, tt.right)
			leftSum, err := ComputeSnapshotContentChecksum("cfg", leftSnapshot)
			require.NoError(t, err)
			rightSum, err := ComputeSnapshotContentChecksum("cfg", rightSnapshot)
			require.NoError(t, err)
			assert.Equal(t, left, leftSum, "legacy and snapshot checksums agree")
			assert.Equal(t, right, rightSum)
			equal, err := SnapshotContentEqual("cfg", leftSnapshot, "cfg", rightSnapshot)
			require.NoError(t, err)
			assert.Equal(t, tt.equal, equal)
		})
	}
}

func TestSecretFilesAreNeverCurrentFiles(t *testing.T) {
	assert.Empty(t, secretFiles("hunter2", "s").CurrentFiles())
}

func TestCanonicalizeSecretFiles(t *testing.T) {
	duplicate := secretFiles("a", "s")
	duplicate.SecretFiles = append(duplicate.SecretFiles, duplicate.SecretFiles[0])
	canonical, err := CanonicalizeAuxiliaryFiles(duplicate)
	require.NoError(t, err)
	assert.Len(t, canonical.SecretFiles, 1)

	conflicting := secretFiles("a", "s")
	conflicting.SecretFiles = append(conflicting.SecretFiles, auxiliaryfiles.SecretFile{Path: "secrets/x", Content: "b", Seal: "t"})
	_, err = CanonicalizeAuxiliaryFiles(conflicting)
	require.ErrorContains(t, err, `Secret file "x" has conflicting definitions`)
}

func TestSecretFileArtifactRequiresASeal(t *testing.T) {
	_, err := BuildAuxiliaryFileSnapshot(renderartifact.NewAuthority(), nil, secretFiles("a", ""))
	require.Error(t, err)
}
