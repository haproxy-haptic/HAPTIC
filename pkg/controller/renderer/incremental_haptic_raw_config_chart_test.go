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
	"log/slog"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/helpers"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/typebootstrap"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	k8sstore "github.com/haproxy-haptic/HAPTIC/pkg/k8s/store"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
)

const (
	hapticRawGlobalComponent   = "global-settings-800-haptic-config-global"
	hapticRawDefaultsComponent = "defaults-settings-800-haptic-config-defaults"
	hapticRawFrontendComponent = "util-haptic-config-frontend-incremental"
	hapticRawFrontendCache     = "global-settings-799-haptic-config-frontend-cache"
	hapticRawFrontendWrapper   = "frontend-extra-800-haptic-config-frontend"
)

func TestHapticRawConfigChartReusesCleanIngressesAndTransfersHeaderOwnership(t *testing.T) {
	service, ingresses, provider := newHapticRawConfigChartFixture(t, true)
	require.NoError(t, ingresses.Add(hapticRawConfigIngress("a-first", map[string]any{
		"haproxy-haptic.org/config-global":   "tune.bufsize 32768",
		"haproxy-haptic.org/config-defaults": "retries 5",
		"haproxy-haptic.org/config-frontend": "http-request set-header X-Haptic-Order first",
	}), []string{"default", "a-first"}))
	require.NoError(t, ingresses.Add(hapticRawConfigIngress("z-last", map[string]any{
		"haproxy-haptic.org/config-global":   "tune.maxrewrite 1024",
		"haproxy-haptic.org/config-defaults": "timeout queue 1234",
		"haproxy-haptic.org/config-frontend": "http-request set-header X-Haptic-Order last",
	}), []string{"default", "z-last"}))

	first := renderAndCommitHapticRawConfigChart(t, service, provider)
	assert.Equal(t, hapticRawConfigExpectedBoth, first)
	assertHapticRawConfigExecutions(t, service, "a-first", 1)
	assertHapticRawConfigExecutions(t, service, "z-last", 1)

	warm := renderAndCommitHapticRawConfigChart(t, service, provider)
	assert.Equal(t, first, warm)
	assertHapticRawConfigExecutions(t, service, "a-first", 1)
	assertHapticRawConfigExecutions(t, service, "z-last", 1)

	require.NoError(t, ingresses.Update(hapticRawConfigIngress("a-first", map[string]any{}), []string{"default", "a-first"}))
	afterRemoval := renderAndCommitHapticRawConfigChart(t, service, provider)
	assert.Equal(t, hapticRawConfigExpectedLast, afterRemoval)
	assertHapticRawConfigExecutions(t, service, "a-first", 0)
	assertHapticRawConfigExecutions(t, service, "z-last", 1)
}

func TestHapticRawConfigChartSkipsInactiveProducersWithoutAFrontend(t *testing.T) {
	service, ingresses, provider := newHapticRawConfigChartFixture(t, false)
	require.NoError(t, ingresses.Add(hapticRawConfigIngress("no-raw-config", map[string]any{}),
		[]string{"default", "no-raw-config"}))

	assert.Equal(t, "G|D\n", renderAndCommitHapticRawConfigChart(t, service, provider))
	assertHapticRawConfigExecutions(t, service, "no-raw-config", 0)
	assert.Equal(t, "G|D\n", renderAndCommitHapticRawConfigChart(t, service, provider))
	assertHapticRawConfigExecutions(t, service, "no-raw-config", 0)
}

func newHapticRawConfigChartFixture(
	t *testing.T,
	includeFrontends bool,
) (*RenderService, *k8sstore.MemoryStore, stores.StoreProvider) {
	t.Helper()
	snippets := loadHapticRawConfigChartSnippets(t)
	rootTemplate := `{{ render "global-settings-799-haptic-config-frontend-cache" }}G{{ render "global-settings-800-haptic-config-global" }}|D{{ render "defaults-settings-800-haptic-config-defaults" }}`
	if includeFrontends {
		rootTemplate += `|F{{ render "frontend-extra-800-haptic-config-frontend" }}|F{{ render "frontend-extra-800-haptic-config-frontend" }}|F{{ render "frontend-extra-800-haptic-config-frontend" }}`
	}
	cfg := &config.Config{
		Dataplane: testDataplaneConfig(),
		WatchedResources: map[string]config.WatchedResource{
			"ingresses": {
				APIVersion: "networking.k8s.io/v1",
				Resources:  "ingresses",
				IndexBy:    []string{"metadata.namespace", "metadata.name"},
			},
		},
		TemplateSnippets: snippets,
		HAProxyConfig:    config.HAProxyConfig{Template: rootTemplate},
	}
	declarations := helpers.BuildAdditionalDeclarations(cfg, &typebootstrap.Result{
		Types:  map[string]reflect.Type{},
		Kinds:  map[string]string{},
		Errors: map[string]error{},
	})
	engine, err := helpers.NewEngineFromConfigWithOptions(cfg, nil, nil, declarations, helpers.EngineOptions{})
	require.NoError(t, err)
	service := NewRenderService(&RenderServiceConfig{Engine: engine, Config: cfg, Logger: slog.Default()})
	ingresses := k8sstore.NewMemoryStore(2)
	provider := stores.NewRealStoreProvider(map[string]stores.Store{"ingresses": ingresses})
	return service, ingresses, provider
}

func loadHapticRawConfigChartSnippets(t *testing.T) map[string]config.TemplateSnippet {
	t.Helper()
	return loadChartSnippets(t, chartLibrariesRoot(t), map[string][]string{
		"haptic-annotations/40-features.yaml": {
			hapticRawGlobalComponent,
			hapticRawDefaultsComponent,
			hapticRawFrontendComponent,
			hapticRawFrontendCache,
			hapticRawFrontendWrapper,
		},
	})
}

func hapticRawConfigIngress(name string, annotations map[string]any) map[string]any {
	return map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "Ingress",
		"metadata": map[string]any{
			"namespace":   "default",
			"name":        name,
			"annotations": annotations,
		},
		"spec": map[string]any{},
	}
}

func renderAndCommitHapticRawConfigChart(
	t *testing.T,
	service *RenderService,
	provider stores.StoreProvider,
) string {
	t.Helper()
	result, err := service.Render(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NotNil(t, result.InputTransaction)
	require.NoError(t, result.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, service)
	return result.HAProxyConfig
}

func assertHapticRawConfigExecutions(t *testing.T, service *RenderService, name string, expected uint64) {
	t.Helper()
	for _, componentName := range []string{
		hapticRawGlobalComponent,
		hapticRawDefaultsComponent,
		hapticRawFrontendComponent,
	} {
		component := service.incremental.components[componentName]
		query := componentQueryKey(&component, "ingresses", "default", name)
		assert.Equal(t, expected, service.incremental.graph.Counters(query).Executions, componentName)
	}
}

const hapticRawConfigExpectedBoth = "G\n# haptic/config-global\n# Ingress: default/a-first\n" +
	"tune.bufsize 32768\n# Ingress: default/z-last\ntune.maxrewrite 1024|D\n# haptic/config-defaults\n" +
	"# Ingress: default/a-first\nretries 5\n# Ingress: default/z-last\ntimeout queue 1234|F\n" +
	"# haptic/config-frontend\n# Ingress: default/a-first\nhttp-request set-header X-Haptic-Order first\n" +
	"# Ingress: default/z-last\nhttp-request set-header X-Haptic-Order last|F\n# haptic/config-frontend\n" +
	"# Ingress: default/a-first\nhttp-request set-header X-Haptic-Order first\n# Ingress: default/z-last\n" +
	"http-request set-header X-Haptic-Order last|F\n# haptic/config-frontend\n# Ingress: default/a-first\n" +
	"http-request set-header X-Haptic-Order first\n# Ingress: default/z-last\nhttp-request set-header X-Haptic-Order last\n"

const hapticRawConfigExpectedLast = "G\n# haptic/config-global\n# Ingress: default/z-last\ntune.maxrewrite 1024|" +
	"D\n# haptic/config-defaults\n# Ingress: default/z-last\ntimeout queue 1234|F\n# haptic/config-frontend\n" +
	"# Ingress: default/z-last\nhttp-request set-header X-Haptic-Order last|F\n# haptic/config-frontend\n" +
	"# Ingress: default/z-last\nhttp-request set-header X-Haptic-Order last|F\n# haptic/config-frontend\n" +
	"# Ingress: default/z-last\nhttp-request set-header X-Haptic-Order last\n"
