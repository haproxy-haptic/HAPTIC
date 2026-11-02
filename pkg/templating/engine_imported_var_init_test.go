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
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// A util exporting a var func, imported by a snippet rendered directly and by
// one rendered through an imported macro, rendered nil before scriggo dd2ff14
// (#585): the chart's defaults-settings-* and frontend-filters-* globs.
func TestEngine_ImportedVarInitializedForEveryImporter(t *testing.T) {
	engine, err := New(map[string]string{
		"main":       "{% import \"chain\" for Chain %}defaults\n{{- on_own_lines(render_glob \"defaults-*\") }}\nfrontend\n{{ Chain() }}\n",
		"chain":      "{% macro Chain() %}{{- on_own_lines(render_glob \"filters-*\") -}}{% end %}",
		"defaults-a": `{% import "util" for F %}{{ F() }}`,
		"filters-a":  `{% import "util" for F %}{{ F() }}`,
		"util":       `{% var F = func() string { return "f" } %}`,
	}, nil)
	require.NoError(t, err)
	out, err := engine.Render(context.Background(), "main", map[string]any{})
	require.NoError(t, err)
	require.Equal(t, "defaults\nf\nfrontend\n\nf\n", out)
}
