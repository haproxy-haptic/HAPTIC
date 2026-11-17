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

package helpers

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/typebootstrap"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
)

// TestBuildAdditionalDeclarations_WithResult covers the happy path
// every production engine constructor takes: typebootstrap ran,
// the typed resources surface as fields on the single `resources`
// struct (not per-resource top-level globals), and currentConfig
// is present unchanged.
func TestBuildAdditionalDeclarations_WithResult(t *testing.T) {
	gwType := reflect.StructOf([]reflect.StructField{
		{Name: "Metadata", Type: reflect.StructOf([]reflect.StructField{
			{Name: "Name", Type: reflect.TypeOf("")},
		})},
	})
	result := &typebootstrap.Result{
		Types:  map[string]reflect.Type{"gateways": gwType},
		Kinds:  map[string]string{"gateways": "Gateway"},
		Errors: map[string]error{},
	}

	decls := BuildAdditionalDeclarations(&config.Config{}, result)

	require.Contains(t, decls, "currentConfig",
		"static currentConfig declaration must be present in every consumer path")
	require.Contains(t, decls, "resources",
		"single 'resources' declaration must surface from BuildEngineDeclarations")

	// The declared shape MUST match what rendercontext.addTypedResources
	// populates at render time — a *Resources struct with one field per
	// watched resource — otherwise Scriggo's runtime variable lookup
	// mismatches the engine's compile-time declaration.
	resourcesDecl := decls["resources"]
	rv := reflect.ValueOf(resourcesDecl)
	require.Equal(t, reflect.Ptr, rv.Type().Kind(),
		"resources is a typed-nil pointer to the dynamic struct")
	resourcesType := rv.Type().Elem()
	require.Equal(t, reflect.Struct, resourcesType.Kind(),
		"resources points at the dynamic per-resource struct")
	require.Equal(t, 1, resourcesType.NumField(),
		"one field per watched resource (gateways here)")
	// Field is *innerStore for the per-resource access surface.
	gwField := resourcesType.Field(0)
	require.Equal(t, reflect.Ptr, gwField.Type.Kind(),
		"per-resource field is a pointer")
	assert.Equal(t, reflect.Struct, gwField.Type.Elem().Kind(),
		"per-resource field points at the closure-bearing store struct")
}

func TestBuildAdditionalDeclarationsHasNoTopLevelFunctions(t *testing.T) {
	result := &typebootstrap.Result{
		Types:  map[string]reflect.Type{"routes": reflect.TypeFor[struct{}]()},
		Kinds:  map[string]string{"routes": "Route"},
		Errors: map[string]error{},
	}
	declarations := BuildAdditionalDeclarations(&config.Config{}, result)
	require.Len(t, declarations, 4)
	for _, name := range []string{"currentConfig", "currentFiles", "currentSensitiveFiles", "resources"} {
		require.Contains(t, declarations, name)
	}
	for name, declaration := range declarations {
		require.NotEqualf(t, reflect.Func, reflect.TypeOf(declaration).Kind(), "%s must not inject a native function", name)
	}
}

// TestBuildAdditionalDeclarations_NilResultPanics pins the
// contract that callers MUST provide a real typebootstrap.Result.
// The previous envelope-only fallback was removed because it
// false-positively rejected charts using typed Spec/Status
// access and silently bound them to a Metadata-only shape that
// would mismatch at render time. Callers that don't have a real
// Result yet (e.g. Stage-1 template validator) must obtain one
// via the injected TypeBootstrapper before calling this helper.
func TestBuildAdditionalDeclarations_NilResultPanics(t *testing.T) {
	assert.Panics(t, func() {
		BuildAdditionalDeclarations(&config.Config{}, nil)
	})
}
