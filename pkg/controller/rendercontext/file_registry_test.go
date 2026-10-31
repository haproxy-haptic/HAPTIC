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

package rendercontext

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

func TestFileRegistry_Register(t *testing.T) {
	pathResolver := &templating.PathResolver{
		MapsDir:    "/etc/haproxy/maps",
		SSLDir:     "/etc/haproxy/certs",
		CRTListDir: "/etc/haproxy/crt-list",
		GeneralDir: "/etc/haproxy/files",
		SecretDir:  "secrets",
	}

	tests := []struct {
		name        string
		fileType    string
		filename    string
		content     string
		wantPath    string
		wantErr     bool
		errContains string
	}{
		{
			name:     "register map file",
			fileType: "map",
			filename: "hosts.map",
			content:  "host1 backend1\nhost2 backend2",
			wantPath: "/etc/haproxy/maps/hosts.map",
		},
		{
			name:     "register cert file",
			fileType: "cert",
			filename: "server.pem",
			content:  "-----BEGIN CERTIFICATE-----",
			wantPath: "/etc/haproxy/certs/server.pem",
		},
		{
			name:     "register general file",
			fileType: "file",
			filename: "config.txt",
			content:  "key=value",
			wantPath: "/etc/haproxy/files/config.txt",
		},
		{
			name:     "register crt-list file",
			fileType: "crt-list",
			filename: "certificates.txt",
			content:  "/path/to/cert.pem\n/path/to/cert2.pem",
			wantPath: "/etc/haproxy/crt-list/certificates.txt",
		},
		{
			name:     "register secret file",
			fileType: "secret",
			filename: "sso.client-secret",
			content:  "s3cret",
			wantPath: "secrets/sso.client-secret",
		},
		{
			name:        "invalid file type",
			fileType:    "invalid",
			filename:    "test.txt",
			content:     "content",
			wantErr:     true,
			errContains: "invalid file type",
		},
		{
			name:        "wrong argument count",
			wantErr:     true,
			errContains: "requires 3 arguments",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := NewFileRegistry(pathResolver, nil)

			var path string
			var err error

			if tt.name == "wrong argument count" {
				path, err = registry.Register("map", "test.map") // Only 2 args
			} else {
				path, err = registry.Register(tt.fileType, tt.filename, tt.content)
			}

			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errContains)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantPath, path)
		})
	}
}

// TestFileRegistry_Register_RejectsTraversal proves Register inherits the
// containment guard for every routable type, including "ca-file" (which routes
// through the general-file path) and "map" (whose filename is otherwise stored
// verbatim). A traversal or absolute filename must be rejected before it is
// recorded; a clean filename must pass.
func TestFileRegistry_Register_RejectsTraversal(t *testing.T) {
	pathResolver := &templating.PathResolver{
		MapsDir:    "/etc/haproxy/maps",
		SSLDir:     "/etc/haproxy/certs",
		CRTListDir: "/etc/haproxy/crt-list",
		GeneralDir: "/etc/haproxy/files",
		SecretDir:  "secrets",
	}

	for _, fileType := range []string{"map", "file", "ca-file", "cert", "crt-list", "secret"} {
		t.Run(fileType, func(t *testing.T) {
			for _, filename := range []string{"../../etc/passwd", "sub/../../../etc/passwd", "/etc/passwd", `c:\evil`, `\\host\share`} {
				registry := NewFileRegistry(pathResolver, nil)
				_, err := registry.Register(fileType, filename, "content")
				require.Error(t, err, "traversal %q must be rejected for type %q", filename, fileType)
			}

			registry := NewFileRegistry(pathResolver, nil)
			_, err := registry.Register(fileType, "clean-name.dat", "content")
			require.NoError(t, err)
		})
	}
}

func TestFileRegistry_Register_Idempotent(t *testing.T) {
	pathResolver := &templating.PathResolver{
		MapsDir: "/etc/haproxy/maps",
	}
	registry := NewFileRegistry(pathResolver, nil)

	// First registration
	path1, err := registry.Register("map", "test.map", "content1")
	require.NoError(t, err)

	// Same content - should succeed and return same path
	path2, err := registry.Register("map", "test.map", "content1")
	require.NoError(t, err)
	assert.Equal(t, path1, path2)
}

func TestFileRegistry_Register_Conflict(t *testing.T) {
	pathResolver := &templating.PathResolver{
		MapsDir: "/etc/haproxy/maps",
	}
	registry := NewFileRegistry(pathResolver, nil)

	// First registration
	_, err := registry.Register("map", "test.map", "content1")
	require.NoError(t, err)

	// Different content - should fail
	_, err = registry.Register("map", "test.map", "content2")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "content conflict")
}

// The 4th argument marks a general file as sidecar-owned so a content change
// deploys without reloading HAProxy. Omitting it must keep reloading — every
// other registration in the chart depends on that default.
func TestFileRegistry_Register_ReloadOnPush(t *testing.T) {
	newRegistry := func() *FileRegistry {
		return NewFileRegistry(&templating.PathResolver{
			GeneralDir: "/etc/haproxy/general",
			MapsDir:    "/etc/haproxy/maps",
		}, nil)
	}

	t.Run("omitted defaults to reloading", func(t *testing.T) {
		r := newRegistry()
		_, err := r.Register("file", "500.http", "body")
		require.NoError(t, err)
		require.Len(t, r.GetFiles().GeneralFiles, 1)
		assert.True(t, r.GetFiles().GeneralFiles[0].ReloadsOnPush())
	})

	t.Run("false marks the file sidecar-owned", func(t *testing.T) {
		r := newRegistry()
		_, err := r.Register("file", "spoa-hub-config.toml", "toml", false)
		require.NoError(t, err)
		require.Len(t, r.GetFiles().GeneralFiles, 1)
		assert.False(t, r.GetFiles().GeneralFiles[0].ReloadsOnPush())
	})

	t.Run("true is accepted and reloads", func(t *testing.T) {
		r := newRegistry()
		_, err := r.Register("file", "500.http", "body", true)
		require.NoError(t, err)
		assert.True(t, r.GetFiles().GeneralFiles[0].ReloadsOnPush())
	})

	t.Run("re-registering with a different flag conflicts", func(t *testing.T) {
		r := newRegistry()
		_, err := r.Register("file", "x.toml", "same")
		require.NoError(t, err)
		_, err = r.Register("file", "x.toml", "same", false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "reloadOnPush conflict")
	})

	t.Run("rejected on types with their own reload rules", func(t *testing.T) {
		r := newRegistry()
		_, err := r.Register("map", "hosts.map", "a b", false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `applies to type "file" only`)
	})

	t.Run("rejected when not a bool", func(t *testing.T) {
		r := newRegistry()
		_, err := r.Register("file", "x.toml", "body", "false")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be a bool")
	})

	t.Run("five arguments rejected", func(t *testing.T) {
		r := newRegistry()
		_, err := r.Register("file", "x.toml", "body", false, "extra")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "requires 3 arguments")
	})
}

func TestFileRegistry_GetFiles(t *testing.T) {
	pathResolver := &templating.PathResolver{
		MapsDir:    "/etc/haproxy/maps",
		SSLDir:     "/etc/haproxy/certs",
		CRTListDir: "/etc/haproxy/crt-list",
		GeneralDir: "/etc/haproxy/files",
	}
	registry := NewFileRegistry(pathResolver, nil)

	// Register various file types
	_, _ = registry.Register("map", "hosts.map", "host content")
	_, _ = registry.Register("cert", "server.pem", "cert content")
	_, _ = registry.Register("file", "config.txt", "file content")
	_, _ = registry.Register("crt-list", "certs.txt", "crt-list content")

	files := registry.GetFiles()

	require.Len(t, files.MapFiles, 1)
	assert.Equal(t, "hosts.map", files.MapFiles[0].Path, "MapFile.Path should be just the filename, not the full path from pathResolver")
	assert.Equal(t, "host content", files.MapFiles[0].Content)

	require.Len(t, files.SSLCertificates, 1)
	assert.Equal(t, "cert content", files.SSLCertificates[0].Content)

	// GeneralFiles includes only "file" type files
	require.Len(t, files.GeneralFiles, 1)
	assert.Equal(t, "file content", files.GeneralFiles[0].Content)
	assert.Equal(t, "config.txt", files.GeneralFiles[0].Filename)

	require.Len(t, files.CRTListFiles, 1)
	assert.Equal(t, "crt-list content", files.CRTListFiles[0].Content)
}

func TestFileRegistry_SecretFiles(t *testing.T) {
	newRegistry := func(key string) *FileRegistry {
		return NewFileRegistry(&templating.PathResolver{SecretDir: "secrets", GeneralDir: "general"}, []byte(key))
	}

	t.Run("maps to a sealed secret file, never a general file", func(t *testing.T) {
		r := newRegistry("key")
		_, err := r.Register("secret", "sso.client-secret", "s3cret")
		require.NoError(t, err)
		files := r.GetFiles()
		assert.Empty(t, files.GeneralFiles)
		assert.Empty(t, files.CurrentFiles())
		require.Len(t, files.SecretFiles, 1)
		assert.Equal(t, auxiliaryfiles.SecretFile{
			Path: "secrets/sso.client-secret", Content: "s3cret", Seal: auxiliaryfiles.SealContent([]byte("key"), "s3cret"),
		}, files.SecretFiles[0])
	})

	t.Run("the seal depends on the key", func(t *testing.T) {
		seal := func(key string) string {
			r := newRegistry(key)
			_, err := r.Register("secret", "x", "s3cret")
			require.NoError(t, err)
			return r.GetFiles().SecretFiles[0].Seal
		}
		assert.Equal(t, seal("a"), seal("a"))
		assert.NotEqual(t, seal("a"), seal("b"))
	})

	t.Run("conflicting content is rejected", func(t *testing.T) {
		r := newRegistry("key")
		_, err := r.Register("secret", "x", "one")
		require.NoError(t, err)
		_, err = r.Register("secret", "x", "two")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "content conflict")
	})

	t.Run("reloadOnPush is rejected", func(t *testing.T) {
		_, err := newRegistry("key").Register("secret", "x", "one", false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `applies to type "file" only`)
	})

	t.Run("an unconfigured directory is rejected", func(t *testing.T) {
		_, err := NewFileRegistry(&templating.PathResolver{}, nil).Register("secret", "x", "one")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no secret file directory")
	})
}

func TestMergeAuxiliaryFiles(t *testing.T) {
	tests := []struct {
		name    string
		static  *dataplane.AuxiliaryFiles
		dynamic *dataplane.AuxiliaryFiles
		want    *dataplane.AuxiliaryFiles
	}{
		{
			name:    "both nil",
			static:  nil,
			dynamic: nil,
			want:    &dataplane.AuxiliaryFiles{},
		},
		{
			name:   "static nil",
			static: nil,
			dynamic: &dataplane.AuxiliaryFiles{
				MapFiles: []auxiliaryfiles.MapFile{{Path: "/map1", Content: "c1"}},
			},
			want: &dataplane.AuxiliaryFiles{
				MapFiles: []auxiliaryfiles.MapFile{{Path: "/map1", Content: "c1"}},
			},
		},
		{
			name: "dynamic nil",
			static: &dataplane.AuxiliaryFiles{
				MapFiles: []auxiliaryfiles.MapFile{{Path: "/map1", Content: "c1"}},
			},
			dynamic: nil,
			want: &dataplane.AuxiliaryFiles{
				MapFiles: []auxiliaryfiles.MapFile{{Path: "/map1", Content: "c1"}},
			},
		},
		{
			name: "merge both",
			static: &dataplane.AuxiliaryFiles{
				MapFiles: []auxiliaryfiles.MapFile{{Path: "/static.map", Content: "s"}},
			},
			dynamic: &dataplane.AuxiliaryFiles{
				MapFiles: []auxiliaryfiles.MapFile{{Path: "/dynamic.map", Content: "d"}},
			},
			want: &dataplane.AuxiliaryFiles{
				MapFiles: []auxiliaryfiles.MapFile{
					{Path: "/dynamic.map", Content: "d"},
					{Path: "/static.map", Content: "s"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := MergeAuxiliaryFiles(tt.static, tt.dynamic)
			require.NoError(t, err)
			assert.Equal(t, len(tt.want.MapFiles), len(got.MapFiles))
			if len(tt.want.MapFiles) > 0 {
				assert.Equal(t, tt.want.MapFiles[0].Path, got.MapFiles[0].Path)
			}
		})
	}
}

func TestMergeAuxiliaryFiles_RejectsStaticDynamicConflict(t *testing.T) {
	static := &dataplane.AuxiliaryFiles{MapFiles: []auxiliaryfiles.MapFile{{
		Path: "routes.map", Content: "static",
	}}}
	dynamic := &dataplane.AuxiliaryFiles{MapFiles: []auxiliaryfiles.MapFile{{
		Path: "routes.map", Content: "dynamic",
	}}}

	_, err := MergeAuxiliaryFiles(static, dynamic)

	require.ErrorContains(t, err, `Map file "routes.map" has conflicting definitions`)
}
