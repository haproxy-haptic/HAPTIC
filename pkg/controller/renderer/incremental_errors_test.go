// Copyright 2026 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package renderer

import (
	"errors"
	"log/slog"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/helpers"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/typebootstrap"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/incremental"
	k8sstore "github.com/haproxy-haptic/HAPTIC/pkg/k8s/store"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

func TestIncrementalComponentErrorsUseConfiguredName(t *testing.T) {
	for name, store := range map[string]stores.Store{
		"warm": incrementalErrorMemoryStore(t),
		"cold": &coldInputStore{items: []any{incrementalErrorResource()}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, engine := incrementalErrorConfig(t, &config.IncrementalTemplate{Source: "routes"}, `{% fail("component failed") %}`)
			provider := stores.NewRealStoreProvider(map[string]stores.Store{"routes": store})

			var err error
			if name == "cold" {
				_, _, err = renderStaticColdIncremental(t, cfg, engine, provider)
			} else {
				service := NewRenderService(&RenderServiceConfig{Engine: engine, Config: cfg, Logger: slog.Default()})
				_, err = service.Render(t.Context(), provider, rendercontext.RenderModeReconcile)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "routes default/route: ")
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "rendering template 'policy-check': component failed")
			assert.NotContains(t, err.Error(), "query")
			assert.Equal(t, "component failed", templating.RenderErrorMessage(err))
			assert.NotContains(t, err.Error(), helpers.IncrementalEntryPointName("policy-check"))
			assertIncrementalRenderErrorRetained(t, err, helpers.IncrementalEntryPointName("policy-check"))
		})
	}
}

func TestIncrementalBindingPlannerErrorsUseConfiguredName(t *testing.T) {
	cfg, engine := incrementalErrorConfig(t, &config.IncrementalTemplate{
		BindingsTemplate: `{% fail("planner failed") %}`,
	}, `{% show "unused" %}`)
	service := NewRenderService(&RenderServiceConfig{Engine: engine, Config: cfg, Logger: slog.Default()})
	provider := stores.NewRealStoreProvider(map[string]stores.Store{"routes": incrementalErrorMemoryStore(t)})

	_, err := service.Render(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rendering template 'policy-check': planner failed")
	assert.NotContains(t, err.Error(), helpers.IncrementalBindingsEntryPointName("policy-check"))
	assertIncrementalRenderErrorRetained(t, err, helpers.IncrementalBindingsEntryPointName("policy-check"))
}

func assertIncrementalRenderErrorRetained(t *testing.T, err error, privateEntryPoint string) {
	t.Helper()
	for current := err; current != nil; current = errors.Unwrap(current) {
		if renderErr, ok := current.(*templating.RenderError); ok && renderErr.TemplateName == privateEntryPoint {
			return
		}
	}
	t.Fatalf("error chain does not retain RenderError for %q: %v", privateEntryPoint, err)
}

func incrementalErrorConfig(
	t *testing.T,
	incrementalConfig *config.IncrementalTemplate,
	template string,
) (*config.Config, templating.Engine) {
	t.Helper()
	cfg := &config.Config{
		Dataplane: testDataplaneConfig(),
		WatchedResources: map[string]config.WatchedResource{
			"routes": {
				APIVersion: "example.test/v1",
				Resources:  "routes",
				IndexBy:    []string{"metadata.namespace", "metadata.name"},
			},
		},
		TemplateSnippets: map[string]config.TemplateSnippet{
			"policy-check": {
				Name:        "policy-check",
				Incremental: incrementalConfig,
				Template:    template,
			},
		},
		HAProxyConfig: config.HAProxyConfig{Template: `{{ render "policy-check" }}`},
	}
	declarations := helpers.BuildAdditionalDeclarations(cfg, &typebootstrap.Result{
		Types:  map[string]reflect.Type{},
		Kinds:  map[string]string{},
		Errors: map[string]error{},
	})
	engine, err := helpers.NewEngineFromConfigWithOptions(cfg, nil, nil, declarations, helpers.EngineOptions{})
	require.NoError(t, err)
	return cfg, engine
}

func incrementalErrorMemoryStore(t *testing.T) stores.Store {
	t.Helper()
	store := k8sstore.NewMemoryStore(2)
	require.NoError(t, store.Add(incrementalErrorResource(), []string{"default", "route"}))
	return store
}

func incrementalErrorResource() map[string]any {
	return map[string]any{
		"apiVersion": "example.test/v1",
		"kind":       "Example",
		"metadata": map[string]any{
			"namespace": "default",
			"name":      "route",
		},
	}
}

func TestAttributeToWatchedObject(t *testing.T) {
	component := &incrementalComponent{name: "policy-check"}
	cause := errors.New("boom")
	namespaced := attributeToWatchedObject(componentQueryKey(component, "routes", "default", "route"), cause)
	assert.Equal(t, "routes default/route: boom", namespaced.Error())
	assert.ErrorIs(t, namespaced, cause)
	assert.Equal(t, "routes default/route: boom",
		attributeToWatchedObject(activationQueryKey("routes", "default", "route"), cause).Error())
	assert.Equal(t, "routes other/route: boom",
		attributeToWatchedObject(derivedProjectionQueryKey("routes", "other", "route"), cause).Error())

	clusterScoped := attributeToWatchedObject(componentQueryKey(component, "classes", "", "haptic"), cause)
	assert.Equal(t, "classes haptic: boom", clusterScoped.Error())

	unknown := attributeToWatchedObject(incremental.NewQueryKey(incremental.EncodeTuple("other", "a\x00b")), cause)
	assert.Equal(t, `incremental query other("a\x00b"): boom`, unknown.Error())
}

func TestDescribeIncrementalGroupLocation(t *testing.T) {
	id := incrementalGroupInstanceID{component: "status", source: "routes", namespace: "default", name: "route"}
	location := string(incrementalGroupLocationKey(id, 3))
	assert.Equal(t, "3 of status for routes default/route", describeIncrementalGroupLocation(location))
	assert.Equal(t, `"\x00"`, describeIncrementalGroupLocation("\x00"))
}
