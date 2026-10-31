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

package testrunner

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
)

// createTestRunner creates a Runner for testing assertions.
func createTestRunner(t *testing.T) *Runner {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return &Runner{
		logger: logger,
	}
}

func TestRunner_AssertNotContains(t *testing.T) {
	runner := createTestRunner(t)

	tests := []struct {
		name       string
		content    string
		pattern    string
		wantPassed bool
		wantErr    string
	}{
		{
			name:       "pattern not in content - passes",
			content:    "hello world",
			pattern:    "foobar",
			wantPassed: true,
		},
		{
			name:       "pattern in content - fails",
			content:    "hello world",
			pattern:    "world",
			wantPassed: false,
			wantErr:    `pattern "world" unexpectedly found`,
		},
		{
			name:       "regex pattern not in content - passes",
			content:    "hello world",
			pattern:    `\d+`,
			wantPassed: true,
		},
		{
			name:       "regex pattern in content - fails",
			content:    "hello 123 world",
			pattern:    `\d+`,
			wantPassed: false,
			wantErr:    "unexpectedly found",
		},
		{
			name:       "invalid regex - fails",
			content:    "hello world",
			pattern:    "[invalid",
			wantPassed: false,
			wantErr:    "invalid regex pattern",
		},
		{
			name:       "empty pattern - passes (matches everything)",
			content:    "hello",
			pattern:    "",
			wantPassed: false, // Empty pattern matches at position 0
			wantErr:    "unexpectedly found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertion := &config.ValidationAssertion{
				Type:    "not_contains",
				Target:  "haproxy.cfg",
				Pattern: tt.pattern,
			}

			result := runner.assertNotContains(tt.content, nil, nil, nil, "", assertion, "")

			assert.Equal(t, tt.wantPassed, result.Passed)
			assert.Equal(t, "not_contains", result.Type)
			if tt.wantErr != "" {
				assert.Contains(t, result.Error, tt.wantErr)
			}
		})
	}
}

func TestRunner_AssertNotContains_WithDescription(t *testing.T) {
	runner := createTestRunner(t)

	assertion := &config.ValidationAssertion{
		Type:        "not_contains",
		Target:      "haproxy.cfg",
		Pattern:     "forbidden",
		Description: "Config must not contain forbidden pattern",
	}

	result := runner.assertNotContains("hello world", nil, nil, nil, "", assertion, "")

	assert.True(t, result.Passed)
	assert.Equal(t, "Config must not contain forbidden pattern", result.Description)
}

func TestRunner_AssertMatchCount(t *testing.T) {
	runner := createTestRunner(t)

	tests := []struct {
		name       string
		content    string
		pattern    string
		expected   string
		wantPassed bool
		wantErr    string
	}{
		{
			name:       "exact match count - passes",
			content:    "foo bar foo baz foo",
			pattern:    "foo",
			expected:   "3",
			wantPassed: true,
		},
		{
			name:       "match count too few - fails",
			content:    "foo bar foo",
			pattern:    "foo",
			expected:   "5",
			wantPassed: false,
			wantErr:    "expected 5 matches, got 2",
		},
		{
			name:       "match count too many - fails",
			content:    "foo foo foo foo",
			pattern:    "foo",
			expected:   "2",
			wantPassed: false,
			wantErr:    "expected 2 matches, got 4",
		},
		{
			name:       "zero matches expected - passes",
			content:    "hello world",
			pattern:    "foo",
			expected:   "0",
			wantPassed: true,
		},
		{
			name:       "regex pattern count - passes",
			content:    "abc123def456ghi789",
			pattern:    `\d+`,
			expected:   "3",
			wantPassed: true,
		},
		{
			name:       "invalid expected count - fails",
			content:    "hello",
			pattern:    "hello",
			expected:   "not-a-number",
			wantPassed: false,
			wantErr:    "invalid expected count",
		},
		{
			name:       "invalid regex pattern - fails",
			content:    "hello",
			pattern:    "[invalid",
			expected:   "1",
			wantPassed: false,
			wantErr:    "invalid regex pattern",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertion := &config.ValidationAssertion{
				Type:     "match_count",
				Target:   "haproxy.cfg",
				Pattern:  tt.pattern,
				Expected: tt.expected,
			}

			result := runner.assertMatchCount(tt.content, nil, nil, nil, "", assertion, "")

			assert.Equal(t, tt.wantPassed, result.Passed)
			assert.Equal(t, "match_count", result.Type)
			if tt.wantErr != "" {
				assert.Contains(t, result.Error, tt.wantErr)
			}
		})
	}
}

func TestRunner_AssertEquals(t *testing.T) {
	runner := createTestRunner(t)

	tests := []struct {
		name       string
		content    string
		expected   string
		wantPassed bool
		wantErr    string
	}{
		{
			name:       "exact match - passes",
			content:    "hello world",
			expected:   "hello world",
			wantPassed: true,
		},
		{
			name:       "different content - fails",
			content:    "hello world",
			expected:   "goodbye world",
			wantPassed: false,
			wantErr:    `expected "goodbye world", got "hello world"`,
		},
		{
			name:       "empty strings - passes",
			content:    "",
			expected:   "",
			wantPassed: true,
		},
		{
			name:       "whitespace difference - fails",
			content:    "hello world",
			expected:   "hello  world",
			wantPassed: false,
		},
		{
			name:       "case sensitive - fails",
			content:    "Hello World",
			expected:   "hello world",
			wantPassed: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertion := &config.ValidationAssertion{
				Type:     "equals",
				Target:   "haproxy.cfg",
				Expected: tt.expected,
			}

			result := runner.assertEquals(tt.content, nil, nil, nil, "", assertion, "")

			assert.Equal(t, tt.wantPassed, result.Passed)
			assert.Equal(t, "equals", result.Type)
			if tt.wantErr != "" {
				assert.Contains(t, result.Error, tt.wantErr)
			}
		})
	}
}

func TestRunner_AssertEquals_TruncatesLongValues(t *testing.T) {
	runner := createTestRunner(t)

	// Create long strings that exceed 100 chars
	longContent := "A" + string(make([]byte, 150))
	longExpected := "B" + string(make([]byte, 150))

	assertion := &config.ValidationAssertion{
		Type:     "equals",
		Target:   "haproxy.cfg",
		Expected: longExpected,
	}

	result := runner.assertEquals(longContent, nil, nil, nil, "", assertion, "")

	assert.False(t, result.Passed)
	assert.Contains(t, result.Error, "Use --verbose for full preview")
}

func TestRunner_AssertJSONPath(t *testing.T) {
	runner := createTestRunner(t)

	tests := []struct {
		name       string
		context    map[string]any
		jsonpath   string
		expected   string
		wantPassed bool
		wantErr    string
	}{
		{
			name: "simple field access - passes",
			context: map[string]any{
				"name": "test-service",
			},
			jsonpath:   "{.name}",
			expected:   "test-service",
			wantPassed: true,
		},
		{
			name: "nested field access - passes",
			context: map[string]any{
				"metadata": map[string]any{
					"name":      "my-pod",
					"namespace": "default",
				},
			},
			jsonpath:   "{.metadata.name}",
			expected:   "my-pod",
			wantPassed: true,
		},
		{
			name: "value mismatch - fails",
			context: map[string]any{
				"status": "running",
			},
			jsonpath:   "{.status}",
			expected:   "pending",
			wantPassed: false,
			wantErr:    `expected "pending", got "running"`,
		},
		{
			name: "field not found - fails",
			context: map[string]any{
				"name": "test",
			},
			jsonpath:   "{.missing}",
			expected:   "value",
			wantPassed: false,
			wantErr:    "is not found",
		},
		{
			name:       "invalid jsonpath syntax - fails",
			context:    map[string]any{},
			jsonpath:   "{invalid",
			expected:   "",
			wantPassed: false,
			wantErr:    "invalid JSONPath expression",
		},
		{
			name: "array index access - passes",
			context: map[string]any{
				"items": []any{"first", "second", "third"},
			},
			jsonpath:   "{.items[1]}",
			expected:   "second",
			wantPassed: true,
		},
		{
			name: "no expected value - passes if path exists",
			context: map[string]any{
				"exists": "yes",
			},
			jsonpath:   "{.exists}",
			expected:   "", // Empty expected means just check if path exists
			wantPassed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertion := &config.ValidationAssertion{
				Type:     "jsonpath",
				JSONPath: tt.jsonpath,
				Expected: tt.expected,
			}

			result := runner.assertJSONPath(tt.context, assertion)

			assert.Equal(t, tt.wantPassed, result.Passed, "result.Passed mismatch")
			assert.Equal(t, "jsonpath", result.Type)
			if tt.wantErr != "" {
				assert.Contains(t, result.Error, tt.wantErr)
			}
		})
	}
}

func TestRunner_AssertMatchOrder(t *testing.T) {
	runner := createTestRunner(t)

	tests := []struct {
		name       string
		content    string
		patterns   []string
		wantPassed bool
		wantErr    string
	}{
		{
			name:       "patterns in order - passes",
			content:    "first second third",
			patterns:   []string{"first", "second", "third"},
			wantPassed: true,
		},
		{
			name:       "patterns out of order - fails",
			content:    "first third second",
			patterns:   []string{"first", "second", "third"},
			wantPassed: false,
			wantErr:    "patterns out of order",
		},
		{
			name:       "pattern not found - fails",
			content:    "first second",
			patterns:   []string{"first", "missing", "second"},
			wantPassed: false,
			wantErr:    "not found",
		},
		{
			name:       "empty patterns - fails",
			content:    "any content",
			patterns:   []string{},
			wantPassed: false,
			wantErr:    "no patterns specified",
		},
		{
			name:       "regex patterns in order - passes",
			content:    "backend_foo backend_bar backend_baz",
			patterns:   []string{`backend_foo`, `backend_bar`, `backend_baz`},
			wantPassed: true,
		},
		{
			name:       "invalid regex pattern - fails",
			content:    "content",
			patterns:   []string{"valid", "[invalid"},
			wantPassed: false,
			wantErr:    "invalid regex pattern",
		},
		{
			name:       "single pattern - passes",
			content:    "hello world",
			patterns:   []string{"hello"},
			wantPassed: true,
		},
		{
			name:       "same pattern twice in order - passes",
			content:    "foo bar foo",
			patterns:   []string{"foo", "bar"},
			wantPassed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertion := &config.ValidationAssertion{
				Type:     "match_order",
				Target:   "haproxy.cfg",
				Patterns: tt.patterns,
			}

			result := runner.assertMatchOrder(tt.content, nil, nil, nil, "", assertion, "")

			assert.Equal(t, tt.wantPassed, result.Passed)
			assert.Equal(t, "match_order", result.Type)
			if tt.wantErr != "" {
				assert.Contains(t, result.Error, tt.wantErr)
			}
		})
	}
}

func TestRunner_FindGeneralFile(t *testing.T) {
	runner := createTestRunner(t)

	tests := []struct {
		name     string
		fileName string
		files    *dataplane.AuxiliaryFiles
		want     string
	}{
		{
			name:     "file found",
			fileName: "error.http",
			files: &dataplane.AuxiliaryFiles{
				GeneralFiles: []auxiliaryfiles.GeneralFile{
					{Filename: "error.http", Content: "HTTP/1.0 503 Service Unavailable"},
				},
			},
			want: "HTTP/1.0 503 Service Unavailable",
		},
		{
			name:     "file not found",
			fileName: "missing.http",
			files: &dataplane.AuxiliaryFiles{
				GeneralFiles: []auxiliaryfiles.GeneralFile{
					{Filename: "error.http", Content: "content"},
				},
			},
			want: "",
		},
		{
			name:     "nil auxiliary files",
			fileName: "any.http",
			files:    nil,
			want:     "",
		},
		{
			name:     "empty general files",
			fileName: "error.http",
			files: &dataplane.AuxiliaryFiles{
				GeneralFiles: []auxiliaryfiles.GeneralFile{},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := runner.findGeneralFile(tt.fileName, tt.files)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRunner_FindCertificate(t *testing.T) {
	runner := createTestRunner(t)

	tests := []struct {
		name     string
		certName string
		files    *dataplane.AuxiliaryFiles
		want     string
	}{
		{
			name:     "certificate found by basename",
			certName: "server.pem",
			files: &dataplane.AuxiliaryFiles{
				SSLCertificates: []auxiliaryfiles.SSLCertificate{
					{Path: "/etc/haproxy/ssl/server.pem", Content: "-----BEGIN CERTIFICATE-----"},
				},
			},
			want: "-----BEGIN CERTIFICATE-----",
		},
		{
			name:     "certificate not found",
			certName: "missing.pem",
			files: &dataplane.AuxiliaryFiles{
				SSLCertificates: []auxiliaryfiles.SSLCertificate{
					{Path: "/etc/haproxy/ssl/server.pem", Content: "content"},
				},
			},
			want: "",
		},
		{
			name:     "nil auxiliary files",
			certName: "any.pem",
			files:    nil,
			want:     "",
		},
		{
			name:     "matches basename only",
			certName: "cert.pem",
			files: &dataplane.AuxiliaryFiles{
				SSLCertificates: []auxiliaryfiles.SSLCertificate{
					{Path: "/deep/nested/path/cert.pem", Content: "found"},
				},
			},
			want: "found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := runner.findCertificate(tt.certName, tt.files)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRunner_FindCRTListFile(t *testing.T) {
	runner := createTestRunner(t)

	tests := []struct {
		name        string
		crtListName string
		files       *dataplane.AuxiliaryFiles
		want        string
	}{
		{
			name:        "crt-list found by basename",
			crtListName: "certificates.txt",
			files: &dataplane.AuxiliaryFiles{
				CRTListFiles: []auxiliaryfiles.CRTListFile{
					{Path: "/etc/haproxy/ssl/certificates.txt", Content: "/etc/haproxy/ssl/cert1.pem"},
				},
			},
			want: "/etc/haproxy/ssl/cert1.pem",
		},
		{
			name:        "crt-list not found",
			crtListName: "missing.txt",
			files: &dataplane.AuxiliaryFiles{
				CRTListFiles: []auxiliaryfiles.CRTListFile{
					{Path: "/etc/haproxy/ssl/certs.txt", Content: "content"},
				},
			},
			want: "",
		},
		{
			name:        "nil auxiliary files",
			crtListName: "any.txt",
			files:       nil,
			want:        "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := runner.findCRTListFile(tt.crtListName, tt.files)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRunner_ResolveTarget(t *testing.T) {
	runner := createTestRunner(t)

	haproxyConfig := "global\n  maxconn 1000"
	renderError := "template error occurred"
	auxFiles := &dataplane.AuxiliaryFiles{
		MapFiles: []auxiliaryfiles.MapFile{
			{Path: "backends.map", Content: "example.com backend1"},
		},
		GeneralFiles: []auxiliaryfiles.GeneralFile{
			{Filename: "error.http", Content: "HTTP/1.0 503"},
		},
		SSLCertificates: []auxiliaryfiles.SSLCertificate{
			{Path: "/ssl/cert.pem", Content: "CERT"},
		},
		CRTListFiles: []auxiliaryfiles.CRTListFile{
			{Path: "/ssl/list.txt", Content: "LIST"},
		},
		SecretFiles: []auxiliaryfiles.SecretFile{
			{Path: "secrets/client-secret", Content: "SECRET", Seal: "seal"},
		},
	}
	k8sResources := map[string]string{
		"haproxy-service": "kind: Service\nmetadata:\n  name: haptic-haproxy\n",
		"gateway-extras":  "---\nkind: ConfigMap\n",
	}

	tests := []struct {
		name   string
		target string
		want   string
	}{
		{
			name:   "haproxy.cfg target",
			target: "haproxy.cfg",
			want:   haproxyConfig,
		},
		{
			name:   "empty target defaults to haproxy.cfg",
			target: "",
			want:   haproxyConfig,
		},
		{
			name:   "rendering_error target",
			target: "rendering_error",
			want:   renderError,
		},
		{
			name:   "map target",
			target: "map:backends.map",
			want:   "example.com backend1",
		},
		{
			name:   "file target",
			target: "file:error.http",
			want:   "HTTP/1.0 503",
		},
		{
			name:   "cert target",
			target: "cert:cert.pem",
			want:   "CERT",
		},
		{
			name:   "crt-list target",
			target: "crt-list:list.txt",
			want:   "LIST",
		},
		{
			name:   "secret target",
			target: "secret:client-secret",
			want:   "SECRET",
		},
		{
			name:   "k8s target",
			target: "k8s:haproxy-service",
			want:   "kind: Service\nmetadata:\n  name: haptic-haproxy\n",
		},
		{
			name:   "unknown target defaults to haproxy.cfg",
			target: "unknown:something",
			want:   haproxyConfig,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := runner.resolveTarget(tt.target, haproxyConfig, auxFiles, k8sResources, nil, "", renderError)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// A prefixed target the render did not produce must be an error, never a silent
// fallback to haproxy.cfg: an absence assertion would otherwise be re-evaluated
// against a file that never held the string and pass with its property gone.
func TestRunner_ResolveTarget_MissingArtefactIsAnError(t *testing.T) {
	runner := createTestRunner(t)

	haproxyConfig := "global\n  maxconn 1000"
	auxFiles := &dataplane.AuxiliaryFiles{
		MapFiles: []auxiliaryfiles.MapFile{{Path: "backends.map", Content: "example.com backend1"}},
	}

	for _, target := range []string{
		"map:does-not-exist.map",
		"file:does-not-exist.http",
		"cert:does-not-exist.pem",
		"crt-list:does-not-exist.txt",
		"secret:does-not-exist",
		"k8s:does-not-exist",
		"status:default/nope:rendered",
		"backend:does-not-exist",
	} {
		t.Run(target, func(t *testing.T) {
			got, err := runner.resolveTarget(target, haproxyConfig, auxFiles, map[string]string{}, map[string]string{}, "", "")
			require.Error(t, err)
			assert.Empty(t, got)
			assert.Contains(t, err.Error(), target)
		})
	}
}

// backend:<name> resolves to the backend section plus its whole profile `from`
// chain, so a test asserts on inherited and own directives at once without a
// (?ms) regex or the generated haptic-be-<hash> name.
func TestRunner_ResolveTarget_Backend(t *testing.T) {
	runner := createTestRunner(t)

	haproxyConfig := strings.Join([]string{
		"global",
		"  maxconn 1000",
		"",
		"defaults haptic-base",
		"  timeout connect 5s",
		"",
		"defaults haptic-be-abc123 from haptic-base",
		"  balance source",
		"  hash-type consistent",
		"",
		"backend default_svc_echo_80 from haptic-be-abc123",
		"  guid be:default_svc_echo_80",
		"  server echo-pod-1 10.0.0.1:8080 check",
		"",
		"backend other_svc from haptic-be-abc123",
		"  server other-1 10.0.0.2:80",
	}, "\n")

	got, err := runner.resolveTarget("backend:default_svc_echo_80", haproxyConfig, nil, nil, nil, "", "")
	require.NoError(t, err)

	// The backend's own directives.
	assert.Contains(t, got, "backend default_svc_echo_80 from haptic-be-abc123")
	assert.Contains(t, got, "server echo-pod-1 10.0.0.1:8080 check")
	// The immediate profile it inherits, and the base at the chain's root.
	assert.Contains(t, got, "balance source")
	assert.Contains(t, got, "hash-type consistent")
	assert.Contains(t, got, "timeout connect 5s")
	// Never another backend that happens to share the profile.
	assert.NotContains(t, got, "server other-1 10.0.0.2:80")
}

// A backend that declares no profile resolves to just its own section.
func TestRunner_ResolveTarget_BackendWithoutProfile(t *testing.T) {
	runner := createTestRunner(t)

	haproxyConfig := "backend plain_be\n  server s1 10.0.0.9:80"

	got, err := runner.resolveTarget("backend:plain_be", haproxyConfig, nil, nil, nil, "", "")
	require.NoError(t, err)
	assert.Equal(t, "backend plain_be\n  server s1 10.0.0.9:80", got)
}

// A `from` parent the render did not emit — whether the backend's immediate
// profile or a deeper ancestor — must error, never resolve to a truncated
// chain: silently dropping the missing profile would let an assertion pass
// against a regression that stopped emitting it.
func TestRunner_ResolveTarget_BackendBrokenProfileChain(t *testing.T) {
	runner := createTestRunner(t)

	tests := map[string]struct {
		haproxyConfig string
		wantProfile   string
	}{
		"immediate profile missing": {
			haproxyConfig: strings.Join([]string{
				"backend default_svc_echo_80 from haptic-be-gone",
				"  guid be:default_svc_echo_80",
				"  server echo-pod-1 10.0.0.1:8080 check",
			}, "\n"),
			wantProfile: "haptic-be-gone",
		},
		"deeper ancestor missing": {
			haproxyConfig: strings.Join([]string{
				"defaults haptic-be-abc123 from haptic-base-gone",
				"  balance source",
				"",
				"backend default_svc_echo_80 from haptic-be-abc123",
				"  server echo-pod-1 10.0.0.1:8080 check",
			}, "\n"),
			wantProfile: "haptic-base-gone",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := runner.resolveTarget("backend:default_svc_echo_80", tt.haproxyConfig, nil, nil, nil, "", "")
			require.Error(t, err)
			assert.Empty(t, got)
			assert.Contains(t, err.Error(), tt.wantProfile)
		})
	}
}

// A registered map with no entries is found; only its content is empty. The
// previous "content != \"\"" test resolved it to haproxy.cfg instead.
func TestRunner_ResolveTarget_EmptyRegisteredMapIsFound(t *testing.T) {
	runner := createTestRunner(t)

	auxFiles := &dataplane.AuxiliaryFiles{
		MapFiles: []auxiliaryfiles.MapFile{{Path: "empty.map", Content: ""}},
	}

	got, err := runner.resolveTarget("map:empty.map", "global", auxFiles, nil, nil, "", "")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestRunner_ResolveAuxiliaryFile_NilFiles(t *testing.T) {
	runner := createTestRunner(t)

	// Should return empty string for all target types when auxiliaryFiles is nil
	targets := []string{"map:test.map", "file:test.http", "cert:test.pem", "crt-list:test.txt"}

	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			got, found := runner.resolveAuxiliaryFile(target, nil)
			assert.Equal(t, "", got)
			assert.False(t, found)
		})
	}
}

func TestTruncateString(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{
			name:   "string shorter than max - unchanged",
			input:  "hello",
			maxLen: 10,
			want:   "hello",
		},
		{
			name:   "string at max length - unchanged",
			input:  "hello",
			maxLen: 5,
			want:   "hello",
		},
		{
			name:   "string longer than max - truncated",
			input:  "hello world",
			maxLen: 5,
			want:   "hello...",
		},
		{
			name:   "empty string - unchanged",
			input:  "",
			maxLen: 10,
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateString(tt.input, tt.maxLen)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRunner_PopulateTargetMetadata(t *testing.T) {
	runner := createTestRunner(t)

	tests := []struct {
		name          string
		target        string
		targetName    string
		hasFailed     bool
		wantPreview   bool
		wantTargetSet bool
	}{
		{
			name:          "failed assertion - has preview",
			target:        "hello world content",
			targetName:    "haproxy.cfg",
			hasFailed:     true,
			wantPreview:   true,
			wantTargetSet: true,
		},
		{
			name:          "passed assertion - no preview",
			target:        "hello world content",
			targetName:    "haproxy.cfg",
			hasFailed:     false,
			wantPreview:   false,
			wantTargetSet: true,
		},
		{
			name:          "empty target - no preview even if failed",
			target:        "",
			targetName:    "map:test.map",
			hasFailed:     true,
			wantPreview:   false,
			wantTargetSet: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := &AssertionResult{}
			runner.populateTargetMetadata(result, tt.target, tt.targetName, tt.hasFailed)

			assert.Equal(t, tt.targetName, result.Target)
			assert.Equal(t, len(tt.target), result.TargetSize)

			if tt.wantPreview {
				assert.NotEmpty(t, result.TargetPreview)
			} else {
				assert.Empty(t, result.TargetPreview)
			}
		})
	}
}

// A directive glued to the end of a comment renders as valid config that does
// nothing: `haproxy -c` accepts it and a `contains` assertion still matches the
// fused line, so only a structural check catches it.
func TestCheckFusedDirectives(t *testing.T) {
	tests := []struct {
		name   string
		config string
		passed bool
	}{
		{
			name:   "directive on its own line",
			config: "frontend fe\n  # gateway/900-path-match\n  http-request set-var(txn.a) str(x)\n",
			passed: true,
		},
		{
			name:   "directive fused into the marker comment",
			config: "frontend fe\n  # gateway/900-path-matchhttp-request set-var(txn.a) str(x)\n",
			passed: false,
		},
		{
			name:   "prose naming a directive is not a fusion",
			config: "frontend fe\n  # the http-request return action strips it\n",
			passed: true,
		},
		{
			name:   "fused use_backend",
			config: "frontend fe\n  # haptic/route (default/api)use_backend be_api\n",
			passed: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check := checkFusedDirectives(&config.ValidationTest{}, tt.config, &TestResult{})
			require.NotNil(t, check)
			assert.Equal(t, tt.passed, check.Passed, check.Error)
		})
	}
}

// A test that expects rendering to fail has no config to inspect.
func TestCheckFusedDirectivesSkipsRenderErrors(t *testing.T) {
	assert.Nil(t, checkFusedDirectives(&config.ValidationTest{}, "", &TestResult{RenderError: "boom"}))

	renderErrorTest := &config.ValidationTest{
		Assertions: []config.ValidationAssertion{{Type: "contains", Target: "rendering_error"}},
	}
	assert.Nil(t, checkFusedDirectives(renderErrorTest, "# x)http-request return 503\n", &TestResult{}))
}
