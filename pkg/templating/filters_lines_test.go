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
	"fmt"
	"strings"
	"testing"

	"github.com/haproxy-haptic/scriggo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOnOwnLinesRenderGlob(t *testing.T) {
	const main = "frontend f\n  bind :80{{- on_own_lines(render_glob \"x-*\") }}\n  default_backend b"
	tests := []struct {
		name     string
		snippets map[string]string
		want     string
	}{
		{
			name:     "no snippets",
			snippets: map[string]string{},
			want:     "frontend f\n  bind :80\n  default_backend b\n",
		},
		{
			name:     "snippets without newlines",
			snippets: map[string]string{"x-1": "a", "x-2": "b"},
			want:     "frontend f\n  bind :80\na\nb\n  default_backend b\n",
		},
		{
			name:     "snippets with leading newlines render unchanged",
			snippets: map[string]string{"x-1": "\na", "x-2": "\nb"},
			want:     "frontend f\n  bind :80\na\nb\n  default_backend b\n",
		},
		{
			name:     "trailing newline before a snippet without one adds no blank line",
			snippets: map[string]string{"x-1": "a\n", "x-2": "b\n"},
			want:     "frontend f\n  bind :80\na\nb\n\n  default_backend b\n",
		},
		{
			name:     "snippet without newlines after one with a leading newline",
			snippets: map[string]string{"x-1": "\na", "x-2": "b"},
			want:     "frontend f\n  bind :80\na\nb\n  default_backend b\n",
		},
		{
			name:     "empty and blank snippets add no line",
			snippets: map[string]string{"x-1": "", "x-2": "a", "x-3": "  ", "x-4": "b"},
			want:     "frontend f\n  bind :80\na\n  b\n  default_backend b\n",
		},
		{
			name:     "only blank snippets",
			snippets: map[string]string{"x-1": "\n", "x-2": "  "},
			want:     "frontend f\n  bind :80\n  \n  default_backend b\n",
		},
		{
			name:     "multi-line snippets",
			snippets: map[string]string{"x-1": "a1\na2", "x-2": "b1\nb2"},
			want:     "frontend f\n  bind :80\na1\na2\nb1\nb2\n  default_backend b\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			templates := map[string]string{"main": main}
			for name, source := range tt.snippets {
				templates[name] = source
			}
			engine, err := New(templates, &Options{EntryPoints: []string{"main"}})
			require.NoError(t, err)
			got, err := engine.Render(context.Background(), "main", nil)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestOnOwnLinesJoin(t *testing.T) {
	tests := []struct {
		left, right, want string
	}{
		{"", "", ""},
		{"a", "", "a"},
		{"", "b", "b"},
		{"a", "b", "a\nb"},
		{"a\n", "b", "a\nb"},
		{"a", "\nb", "a\nb"},
		{"x\n  ", "b", "x\n  b"},
		{"a", "  \nb", "a  \nb"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q+%q", tt.left, tt.right), func(t *testing.T) {
			assert.Equal(t, tt.want, scriggoOnOwnLinesJoin(tt.left, tt.right))
		})
	}
}

func TestOnOwnLinesCompilesManySnippets(t *testing.T) {
	templates := map[string]string{"main": `{{- on_own_lines(render_glob "x-*") -}}`}
	want := make([]string, 0, 200)
	for i := range 200 {
		name := fmt.Sprintf("x-%03d", i)
		templates[name] = name
		want = append(want, name)
	}
	engine, err := New(templates, &Options{EntryPoints: []string{"main"}})
	require.NoError(t, err)
	got, err := engine.Render(context.Background(), "main", nil)
	require.NoError(t, err)
	assert.Equal(t, "\n"+strings.Join(want, "\n")+"\n", got)
}

func TestOnOwnLinesInheritsCallerContext(t *testing.T) {
	engine, err := New(map[string]string{
		"main": `{%- var name = "api" -%}
backend {{ name }}{{- on_own_lines(render_glob "x-*" inherit_context) }}`,
		"x-1": `# for {{ name }}`,
		"x-2": `timeout server 5s`,
	}, &Options{EntryPoints: []string{"main"}})
	require.NoError(t, err)
	got, err := engine.Render(context.Background(), "main", nil)
	require.NoError(t, err)
	assert.Equal(t, "backend api\n# for api\ntimeout server 5s\n", got)
}

func TestOnOwnLinesInRenderedAndImportedTemplates(t *testing.T) {
	engine, err := New(map[string]string{
		"main":  `{%- import "lib" for Block -%}{{ render "inner" }}|{{ Block() }}`,
		"inner": `inner{{- on_own_lines(render_glob "x-*") }}`,
		"lib":   `{% macro Block() string %}macro{{- render_glob "x-*" | on_own_lines() }}{% end %}`,
		"x-1":   `a`,
		"x-2":   `b`,
	}, &Options{EntryPoints: []string{"main"}})
	require.NoError(t, err)
	got, err := engine.Render(context.Background(), "main", nil)
	require.NoError(t, err)
	assert.Equal(t, "inner\na\nb|macro\na\nb\n", got)
}

func TestOnOwnLinesRejectsOtherArguments(t *testing.T) {
	tests := map[string]string{
		"string":        `{{ on_own_lines("a") }}`,
		"render":        `{{ on_own_lines(render "x-1") }}`,
		"glob variable": `{%- var s = render_glob "x-*" -%}{{ on_own_lines(s) }}`,
		"pipe argument": `{{ render_glob "x-*" | on_own_lines("a") }}`,
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := New(map[string]string{"main": source, "x-1": "a"}, &Options{EntryPoints: []string{"main"}})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "on_own_lines takes one render_glob expression")
		})
	}
}

func TestOnOwnLinesInIncrementalComponent(t *testing.T) {
	engine, err := New(map[string]string{
		"component": `head{{- on_own_lines(render_glob "x-*") }}`,
		"x-1":       `a`,
		"x-2":       `b`,
	}, &Options{
		EntryPoints:            []string{"component"},
		IncrementalEntryPoints: []string{"component"},
		Declarations:           map[string]any{"resources": incrementalBatchResourcesDeclaration()},
	})
	require.NoError(t, err)
	compiled := engine.compiledTemplates["component"]
	require.True(t, compiled.BatchSafe(), "callables: %#v", compiled.UsedNativeCallables())
	require.NoError(t, compiled.DeterministicSafe())
}

func TestOnOwnLinesTemplatesWithTypeDeclarations(t *testing.T) {
	engine, err := New(map[string]string{
		"main": `{% type Pair struct { A, B string } %}{%% p := struct { V []int }{[]int{7}} %%}` +
			`{{ Pair{"x", "y"}.A }}{{ len(p.V) }}{{- on_own_lines(render_glob "x-*") }}`,
		"x-1": `a`,
	}, &Options{EntryPoints: []string{"main"}})
	require.NoError(t, err)
	got, err := engine.Render(context.Background(), "main", nil)
	require.NoError(t, err)
	assert.Equal(t, "x1\na\n", got)
}

func TestOnOwnLinesNeedsTheTransformer(t *testing.T) {
	_, err := scriggo.BuildTemplate(
		&scriggoTemplateFS{templates: map[string]string{"main": `{{ on_own_lines(render_glob "x-*") }}`, "x-1": "a"}},
		"main",
		&scriggo.BuildOptions{Globals: buildScriggoGlobals(nil, nil, nil)},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "undefined: on_own_lines")
}
