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

package templating

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSanitizeStorageName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "simple domain with extension",
			input: "example.com.pem",
			want:  "example_com.pem",
		},
		{
			name:  "subdomain with extension",
			input: "sub.example.com.pem",
			want:  "sub_example_com.pem",
		},
		{
			name:  "namespace_secret pattern (no dots in name)",
			input: "keycloak_keycloak-tls.pem",
			want:  "keycloak_keycloak-tls.pem",
		},
		{
			name:  "namespace_secret.domain pattern",
			input: "keycloak_sso.example.com-tls.pem",
			want:  "keycloak_sso_example_com-tls.pem",
		},
		{
			name:  "multiple dots - last is treated as extension",
			input: "no.extension.here",
			want:  "no_extension.here", // .here is the extension, only basename dots replaced
		},
		{
			name:  "no dots - unchanged",
			input: "nodots.pem",
			want:  "nodots.pem",
		},
		{
			name:  "crt-list file with domain",
			input: "example.com.crtlist",
			want:  "example_com.crtlist",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeStorageName(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestPathResolver_GetPath(t *testing.T) {
	resolver := &PathResolver{
		MapsDir:    "/etc/haproxy/maps",
		SSLDir:     "/etc/haproxy/ssl",
		CRTListDir: "/etc/haproxy/general", // CRT-list files stored in general directory to avoid reload
		GeneralDir: "/etc/haproxy/general",
		SecretDir:  "/etc/haproxy/secrets",
	}

	tests := []struct {
		name     string
		filename any
		args     []any
		want     string
		wantErr  bool
	}{
		{
			name:     "map file",
			filename: "host.map",
			args:     []any{"map"},
			want:     "/etc/haproxy/maps/host.map",
		},
		{
			name:     "general file",
			filename: "503.http",
			args:     []any{"file"},
			want:     "/etc/haproxy/general/503.http",
		},
		{
			name:     "ssl certificate",
			filename: "cert.pem",
			args:     []any{"cert"},
			want:     "/etc/haproxy/ssl/cert.pem",
		},
		{
			name:     "crt-list file",
			filename: "certificate-list.txt",
			args:     []any{"crt-list"},
			want:     "/etc/haproxy/general/certificate-list.txt",
		},
		{
			name:     "secret file with dots - NOT sanitized",
			filename: "sso.client-secret",
			args:     []any{"secret"},
			want:     "/etc/haproxy/secrets/sso.client-secret",
		},
		// Sanitization tests for SSL certificates
		{
			name:     "ssl certificate with domain dots - sanitized",
			filename: "example.com.pem",
			args:     []any{"cert"},
			want:     "/etc/haproxy/ssl/example_com.pem",
		},
		{
			name:     "ssl certificate with subdomain - sanitized",
			filename: "sub.example.com.pem",
			args:     []any{"cert"},
			want:     "/etc/haproxy/ssl/sub_example_com.pem",
		},
		{
			name:     "ssl certificate production pattern - sanitized",
			filename: "keycloak_sso.example.com-tls.pem",
			args:     []any{"cert"},
			want:     "/etc/haproxy/ssl/keycloak_sso_example_com-tls.pem",
		},
		// Sanitization tests for CRT-list files
		{
			name:     "crt-list with domain dots - sanitized",
			filename: "example.com.crtlist",
			args:     []any{"crt-list"},
			want:     "/etc/haproxy/general/example_com.crtlist",
		},
		// Map files should NOT be sanitized
		{
			name:     "map file with dots - NOT sanitized",
			filename: "domain.map",
			args:     []any{"map"},
			want:     "/etc/haproxy/maps/domain.map",
		},
		{
			name:     "map file with multiple dots - NOT sanitized",
			filename: "sub.domain.com.map",
			args:     []any{"map"},
			want:     "/etc/haproxy/maps/sub.domain.com.map",
		},
		// General files should NOT be sanitized
		{
			name:     "general file with dots - NOT sanitized",
			filename: "error.page.http",
			args:     []any{"file"},
			want:     "/etc/haproxy/general/error.page.http",
		},
		{
			name:     "empty filename returns directory",
			filename: "",
			args:     []any{"map"},
			want:     "/etc/haproxy/maps",
		},
		{
			name:     "non-string filename",
			filename: 123,
			args:     []any{"map"},
			wantErr:  true,
		},
		{
			name:     "missing file type arg",
			filename: "test.map",
			args:     []any{},
			wantErr:  true,
		},
		{
			name:     "invalid file type",
			filename: "test.txt",
			args:     []any{"invalid"},
			wantErr:  true,
		},
		{
			name:     "non-string file type",
			filename: "test.map",
			args:     []any{123},
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// GetPath expects all arguments in a single variadic call
			args := []any{tt.filename}
			args = append(args, tt.args...)
			got, err := resolver.GetPath(args...)

			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestPathResolver_GetPath_SecretNeedsADirectory(t *testing.T) {
	_, err := (&PathResolver{GeneralDir: "general"}).GetPath("x", "secret")
	require.ErrorContains(t, err, "no secret file directory")
}

// TestPathResolver_GetPath_RejectsTraversal proves the containment guard fires
// for every file type: a resource-derived filename that escapes its directory
// (or is an absolute/Windows path) must be rejected, while a clean name passes.
func TestPathResolver_GetPath_RejectsTraversal(t *testing.T) {
	resolver := &PathResolver{
		MapsDir:    "maps",
		SSLDir:     "ssl",
		CRTListDir: "ssl",
		GeneralDir: "files",
		SecretDir:  "secrets",
	}

	for _, fileType := range []string{"map", "file", "cert", "crt-list", "secret"} {
		t.Run(fileType, func(t *testing.T) {
			traversals := []struct {
				name     string
				filename string
			}{
				{"parent traversal", "../../etc/passwd"},
				{"traversal mid-path", "sub/../../../etc/passwd"},
				{"absolute path", "/etc/passwd"},
				{"windows drive", `c:\evil`},
				{"windows unc", `\\host\share`},
			}
			for _, tc := range traversals {
				t.Run(tc.name, func(t *testing.T) {
					_, err := resolver.GetPath(tc.filename, fileType)
					require.Error(t, err, "traversal %q must be rejected for type %q", tc.filename, fileType)
				})
			}

			t.Run("clean filename passes", func(t *testing.T) {
				got, err := resolver.GetPath("valid-name.dat", fileType)
				require.NoError(t, err)
				assert.NotEmpty(t, got)
			})
		})
	}
}

func TestStrip(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "leading and trailing spaces",
			input: "  hello world  ",
			want:  "hello world",
		},
		{
			name:  "tabs and newlines",
			input: "\t\nhello\n\t",
			want:  "hello",
		},
		{
			name:  "no whitespace",
			input: "already-trimmed",
			want:  "already-trimmed",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "only whitespace",
			input: "   \t\n   ",
			want:  "",
		},
		{
			name:  "internal whitespace preserved",
			input: "  hello   world  ",
			want:  "hello   world",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := strip(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDebug(t *testing.T) {
	tests := []struct {
		name         string
		value        any
		label        string
		wantContains []string
	}{
		{
			name: "simple object without label",
			value: map[string]any{
				"key": "value",
			},
			label: "",
			wantContains: []string{
				"# DEBUG:",
				`#   "key": "value"`,
			},
		},
		{
			name: "simple object with label",
			value: map[string]any{
				"name": "test",
			},
			label: "my-label",
			wantContains: []string{
				"# DEBUG my-label:",
				`#   "name": "test"`,
			},
		},
		{
			name:  "array value",
			value: []string{"a", "b", "c"},
			label: "",
			wantContains: []string{
				"# DEBUG:",
				`#   "a"`,
				`#   "b"`,
				`#   "c"`,
			},
		},
		{
			name:  "nested structure",
			value: map[string]any{"outer": map[string]any{"inner": "value"}},
			label: "nested",
			wantContains: []string{
				"# DEBUG nested:",
				`#   "outer"`,
				`#     "inner": "value"`,
			},
		},
		{
			name:  "nil value",
			value: nil,
			label: "",
			wantContains: []string{
				"# DEBUG:",
				"# null",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := debug(tt.value, tt.label)
			for _, want := range tt.wantContains {
				assert.Contains(t, got, want)
			}
		})
	}
}
