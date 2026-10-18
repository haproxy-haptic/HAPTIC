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
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewCompilationError(t *testing.T) {
	cause := errors.New("syntax error at line 1")

	tests := []struct {
		name        string
		template    string
		wantSnippet string
	}{
		{
			name:        "short template kept verbatim",
			template:    "Hello {{ name }}",
			wantSnippet: "Hello {{ name }}",
		},
		{
			name:        "empty template",
			template:    "",
			wantSnippet: "",
		},
		{
			name:        "exactly 200 chars kept verbatim",
			template:    strings.Repeat("a", 200),
			wantSnippet: strings.Repeat("a", 200),
		},
		{
			name:        "201 chars truncated with ellipsis",
			template:    strings.Repeat("a", 201),
			wantSnippet: strings.Repeat("a", 200) + "...",
		},
		{
			name:        "much longer template truncated",
			template:    strings.Repeat("xy", 500),
			wantSnippet: strings.Repeat("xy", 100) + "...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := NewCompilationError("haproxy.cfg", tt.template, cause)
			require.NotNil(t, err)
			assert.Equal(t, "haproxy.cfg", err.TemplateName)
			assert.Equal(t, tt.wantSnippet, err.TemplateSnippet)
			assert.Same(t, cause, err.Unwrap())
			assert.Equal(t, "compiling template 'haproxy.cfg': "+cause.Error(), err.Error())
		})
	}
}

func TestNewRenderError(t *testing.T) {
	cause := errors.New("missing variable")
	err := NewRenderError("backend.cfg", cause)

	require.NotNil(t, err)
	assert.Equal(t, "backend.cfg", err.TemplateName)
	assert.Same(t, cause, err.Unwrap())
	assert.Equal(t, "rendering template 'backend.cfg': missing variable", err.Error())
}

func TestNewTemplateNotFoundError(t *testing.T) {
	err := NewTemplateNotFoundError("missing", []string{"a", "b"})

	require.NotNil(t, err)
	assert.Equal(t, "missing", err.TemplateName)
	assert.Equal(t, []string{"a", "b"}, err.AvailableTemplates)
	assert.Equal(t, "template 'missing' not found", err.Error())
}

func TestRenderTimeoutError(t *testing.T) {
	cause := errors.New("context deadline exceeded")
	err := &RenderTimeoutError{TemplateName: "slow.cfg", Cause: cause}

	assert.Equal(t, "template 'slow.cfg' render timed out: context deadline exceeded", err.Error())
	assert.Same(t, cause, err.Unwrap())
}

// TestErrorsAs verifies all custom error types unwrap correctly via errors.As,
// which is the documented use case for the helper functions: callers do
// `errors.As(err, &compErr)` to get the structured fields.
func TestErrorsAs(t *testing.T) {
	cause := errors.New("inner")

	tests := []struct {
		name string
		err  error
		want any
	}{
		{name: "CompilationError", err: NewCompilationError("t", "body", cause), want: new(*CompilationError)},
		{name: "RenderError", err: NewRenderError("t", cause), want: new(*RenderError)},
		{name: "TemplateNotFoundError", err: NewTemplateNotFoundError("t", nil), want: new(*TemplateNotFoundError)},
		{name: "RenderTimeoutError", err: &RenderTimeoutError{TemplateName: "t", Cause: cause}, want: new(*RenderTimeoutError)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok := errors.As(tt.err, tt.want)
			assert.True(t, ok, "errors.As should match %T", tt.err)
		})
	}
}

// TestErrorsIs verifies that unwrappable errors propagate Cause for errors.Is
// matching (so callers can match sentinel causes through wrappers).
func TestErrorsIs(t *testing.T) {
	cause := errors.New("specific cause")

	tests := []struct {
		name string
		err  error
	}{
		{name: "CompilationError", err: NewCompilationError("t", "body", cause)},
		{name: "RenderError", err: NewRenderError("t", cause)},
		{name: "RenderTimeoutError", err: &RenderTimeoutError{TemplateName: "t", Cause: cause}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.True(t, errors.Is(tt.err, cause), "errors.Is should propagate cause through %T", tt.err)
		})
	}
}

func TestRenderErrorMessage(t *testing.T) {
	tests := []struct {
		name     string
		template string
		want     string
	}{
		{name: "top level", template: `{{ fail("ingress a/b is invalid") }}`, want: "ingress a/b is invalid"},
		{name: "nested render", template: `{{ render "inner" }}`, want: "inner failed"},
		{name: "inside a closure", template: `{%% var check = func() { fail("closure failed") }; check() %%}`, want: "closure failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine, err := New(map[string]string{
				"outer": tt.template,
				"inner": `{{ fail("inner failed") }}`,
			}, &Options{EntryPoints: []string{"outer"}})
			require.NoError(t, err)
			_, err = engine.Render(t.Context(), "outer", nil)
			require.Error(t, err)
			assert.Equal(t, tt.want, RenderErrorMessage(fmt.Errorf("render failed: %w", err)))
		})
	}

	t.Run("not a fail() error", func(t *testing.T) {
		err := fmt.Errorf("render failed: %w", NewRenderError("outer", errors.New("index out of range")))
		assert.Equal(t, "render failed: rendering template 'outer': index out of range", RenderErrorMessage(err))
	})
	t.Run("nil", func(t *testing.T) {
		assert.Empty(t, RenderErrorMessage(nil))
	})
}
