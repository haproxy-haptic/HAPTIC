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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

// Supply binary HTTP content directly. Kubernetes JSON string fixtures cannot
// carry the gzip bytes of an OWASP release archive.
func TestFetchedCRSReleaseArchive(t *testing.T) {
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	for name, content := range map[string]string{
		"crs-setup.conf.example":                   "release setup",
		"rules/REQUEST-901-INITIALIZATION.conf":    "request rules",
		"rules/patterns.data":                      "phrase list",
		"util/crs-rules-check/examples/test1.conf": "linter fixture",
	} {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: "coreruleset-4.25.0/" + name, Size: int64(len(content)), Mode: 0o644}))
		_, err := tw.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())

	source, err := os.ReadFile("../../../charts/haptic/charts/spoa-hub/25-crs-source.yaml")
	require.NoError(t, err)
	var library struct {
		Snippets map[string]config.TemplateSnippet `yaml:"templateSnippets"`
	}
	require.NoError(t, yaml.Unmarshal(source, &library))
	const url = "https://example.test/crs.tar.gz"
	const directives = `{%- import "util-waf-crs-directives" for CrsDirectives -%}{{ CrsDirectives("Include @crs-setup.conf.example\nInclude @owasp_crs/*.conf") }}`
	templates := map[string]string{"haproxy.cfg": directives}
	for name, snippet := range library.Snippets {
		templates[name] = snippet.Template
	}
	engine, err := templating.New(templates, &templating.Options{EntryPoints: []string{"haproxy.cfg"}, Declarations: map[string]any{"currentFiles": (*map[string]string)(nil)}})
	require.NoError(t, err)
	cfg := &config.Config{
		HAProxyConfig:    config.HAProxyConfig{Template: directives},
		TemplateSnippets: library.Snippets,
		ValidationTests: map[string]config.ValidationTest{"fetch": {
			ExtraContext: map[string]any{"waf": map[string]any{"crs": map[string]any{"url": url}}},
			HTTPFixtures: []config.HTTPResourceFixture{{URL: url, Content: archive.String()}},
		}},
	}
	result, err := New(cfg, engine, &dataplane.ValidationPaths{}, nil).Render(t.Context(), "fetch")
	require.NoError(t, err)
	require.Regexp(t, `Include [^\n]*/crs-setup\.conf\.example\nInclude [^\n]*/crs-\*\.conf`, result.HAProxyConfig)
	require.Equal(t, map[string]string{
		"crs-setup.conf.example":              "release setup",
		"crs-REQUEST-901-INITIALIZATION.conf": "request rules",
		"patterns.data":                       "phrase list",
	}, result.AuxiliaryFiles.CurrentFiles())
}
