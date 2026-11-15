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

package dataplane

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
)

// TestResolveAuxiliaryFilePath_Containment proves a path that would escape its
// base directory is rejected, while clean names and legitimate subdirectories
// resolve inside the temp validation tree.
func TestResolveAuxiliaryFilePath_Containment(t *testing.T) {
	const configDir = "/tmp/validate-xyz"
	const fallbackDir = "/tmp/validate-xyz/maps"

	tests := []struct {
		name     string
		filePath string
		wantErr  bool
	}{
		{"simple filename", "hosts.map", false},
		{"relative subdirectory", "sub/hosts.map", false},
		{"absolute path uses basename", "/etc/haproxy/maps/hosts.map", false},
		{"parent traversal", "../../etc/passwd", true},
		{"traversal mid-path", "maps/../../../etc/passwd", true},
		{"lone parent", "..", true},
		{"absolute climbing to basename dotdot", "/tmp/validate-xyz/maps/..", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolved, err := resolveAuxiliaryFilePath(tt.filePath, configDir, fallbackDir)
			if tt.wantErr {
				require.Error(t, err, "path %q must be rejected", tt.filePath)
				return
			}
			require.NoError(t, err)
			assert.NotEmpty(t, resolved)
		})
	}
}

// TestWriteAuxiliaryFiles_RejectsTraversal proves the write chain refuses a
// traversal path end-to-end and never creates a file outside the temp tree.
func TestWriteAuxiliaryFiles_RejectsTraversal(t *testing.T) {
	tmpDir := t.TempDir()
	paths := &ValidationPaths{
		MapsDir:           filepath.Join(tmpDir, "maps"),
		SSLCertsDir:       filepath.Join(tmpDir, "certs"),
		GeneralStorageDir: filepath.Join(tmpDir, "general"),
		CRTListDir:        filepath.Join(tmpDir, "general"),
		ConfigFile:        filepath.Join(tmpDir, "haproxy.cfg"),
	}

	auxFiles := &AuxiliaryFiles{
		MapFiles: []auxiliaryfiles.MapFile{
			{Path: "../../../../etc/haptic-escape", Content: "pwned"},
		},
	}

	err := writeAuxiliaryFiles(auxFiles, paths)
	require.Error(t, err)

	_, statErr := os.Stat(filepath.Join(filepath.Dir(tmpDir), "etc", "haptic-escape"))
	assert.True(t, os.IsNotExist(statErr), "traversal write must not land outside the temp tree")
}

// haproxy -c must read a sensitive map from the maps directory like any other.
func TestWriteAuxiliaryFilesWritesSensitiveMaps(t *testing.T) {
	tmpDir := t.TempDir()
	paths := &ValidationPaths{MapsDir: filepath.Join(tmpDir, "maps"), ConfigFile: filepath.Join(tmpDir, "haproxy.cfg")}

	require.NoError(t, writeAuxiliaryFiles(&AuxiliaryFiles{
		SensitiveMapFiles: []auxiliaryfiles.SensitiveMapFile{{Path: "keys.map", Content: "route-a key\n", Seal: "s"}},
	}, paths))

	content, err := os.ReadFile(filepath.Join(tmpDir, "maps", "keys.map"))
	require.NoError(t, err)
	assert.Equal(t, "route-a key\n", string(content))
}
