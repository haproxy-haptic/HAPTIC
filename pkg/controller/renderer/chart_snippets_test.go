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

package renderer

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
)

type chartSnippetLibrary struct {
	TemplateSnippets map[string]chartLibrarySnippet `yaml:"templateSnippets"`
}

type chartLibrarySnippet struct {
	Template    string                    `yaml:"template"`
	Requires    []string                  `yaml:"requires"`
	Incremental *chartIncrementalTemplate `yaml:"incremental"`
}

type chartIncrementalTemplate struct {
	Mode              config.IncrementalMode     `yaml:"mode"`
	Source            string                     `yaml:"source"`
	BindingsTemplate  string                     `yaml:"bindingsTemplate"`
	WhenAnyPathExists []string                   `yaml:"whenAnyPathExists"`
	Root              string                     `yaml:"root"`
	Group             string                     `yaml:"group"`
	Consumes          []string                   `yaml:"consumes"`
	OptionalConsumes  []string                   `yaml:"optionalConsumes"`
	Effects           []config.IncrementalEffect `yaml:"effects"`
}

func chartLibrariesRoot(tb testing.TB) string {
	tb.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	require.True(tb, ok)
	return filepath.Join(filepath.Dir(sourceFile), "..", "..", "..", "charts", "haptic", "charts")
}

func loadChartSnippets(
	tb testing.TB,
	root string,
	wantedByFile map[string][]string,
) map[string]config.TemplateSnippet {
	tb.Helper()
	result := map[string]config.TemplateSnippet{}
	for relativePath, names := range wantedByFile {
		content, err := os.ReadFile(filepath.Join(root, relativePath))
		require.NoError(tb, err)
		var library chartSnippetLibrary
		require.NoError(tb, yaml.Unmarshal(content, &library))
		for _, name := range names {
			chartSnippet, found := library.TemplateSnippets[name]
			require.Truef(tb, found, "%s is missing %s", relativePath, name)
			result[name] = config.TemplateSnippet{
				Name:        name,
				Template:    chartSnippet.Template,
				Requires:    chartSnippet.Requires,
				Incremental: (*config.IncrementalTemplate)(chartSnippet.Incremental),
			}
		}
	}
	return result
}
