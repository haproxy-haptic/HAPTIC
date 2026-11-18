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
	"log/slog"
	"reflect"
	"strings"
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
	customCRDMapComponent     = "custom-route-header-map-items"
	customCRDRulesComponent   = "custom-route-header-rules"
	customCRDBackendComponent = "backends-990-custom-route"
)

const customCRDChartRoot = `{{ planRegistry.ProfileGroup() }}
{%- if tostring(extraContext | dig("noHTTPFrontend") | fallback(false)) != "true" -%}
{{ render "frontend-filters-990-custom-route-headers" }}{{ "\n" }}
{%- end -%}
{{ render "backends-990-custom-route" }}
{%- if tostring(extraContext | dig("failAfterCustomRoutes") | fallback(false)) == "true" -%}
{{ fail("forced failure after custom routes") }}
{%- end -%}`

type customCRDChartFixture struct {
	config   *config.Config
	service  *RenderService
	routes   *k8sstore.MemoryStore
	provider stores.StoreProvider
}

func TestCustomCRDChartIncrementalLifecycleMatchesColdOracle(t *testing.T) {
	fixture := newCustomCRDChartFixture(t)
	routeA := customCRDRoute("a", "10.0.0.1", 8080, "X-Env", "prod eu")
	routeB := customCRDRoute("b", "10.0.0.2", 8080, "x-env", "prod us")
	fixture.addRoute(t, routeA)
	fixture.addRoute(t, routeB)

	baseline := fixture.renderAndCommitCacheReady(t)
	assert.Equal(t, "# <backend>|set|<name> -> URL-encoded value\n"+
		"default_a|set|x-env prod%20eu\n"+
		"default_b|set|x-env prod%20us\n", customCRDMapContent(t, baseline))
	assert.Contains(t, baseline.HAProxyConfig, "http-request set-header X-Env")
	assert.NotContains(t, baseline.HAProxyConfig, "http-request set-header x-env")
	assert.Contains(t, baseline.HAProxyConfig, "server primary 10.0.0.1:8080")
	assert.Equal(t, 1, strings.Count(baseline.HAProxyConfig, "http-request set-header X-Env"))
	fixture.assertRouteExecutions(t, "a", 1)
	fixture.assertRouteExecutions(t, "b", 1)

	warm := fixture.renderAndCommitCacheReady(t)
	assertCustomCRDObservableEqual(t, baseline, warm)
	fixture.assertRouteExecutions(t, "a", 1)
	fixture.assertRouteExecutions(t, "b", 1)

	changedRouteA := customCRDRoute("a", "10.0.0.9", 9090, "X-Env", "staging")
	fixture.updateRoute(t, changedRouteA)
	changed := fixture.renderAndCommitCacheReady(t)
	assert.Contains(t, changed.HAProxyConfig, "server primary 10.0.0.9:9090")
	assert.NotContains(t, changed.HAProxyConfig, "server primary 10.0.0.1:8080")
	assert.Contains(t, customCRDMapContent(t, changed), "default_a|set|x-env staging\n")
	assertCustomCRDObservableEqual(t, customCRDColdOracle(t, changedRouteA, routeB), changed)
	fixture.assertRouteExecutions(t, "a", 2)
	fixture.assertRouteExecutions(t, "b", 1)

	fixture.updateRoute(t, routeA)
	restored := fixture.renderAndCommitCacheReady(t)
	assertCustomCRDObservableEqual(t, baseline, restored)
	fixture.assertRouteExecutions(t, "a", 3)
	fixture.assertRouteExecutions(t, "b", 1)

	fixture.deleteRoute(t, "a")
	deleted := fixture.renderAndCommitCacheReady(t)
	assert.NotContains(t, deleted.HAProxyConfig, "backend default_a ")
	assert.Contains(t, deleted.HAProxyConfig, "http-request set-header x-env")
	assert.NotContains(t, deleted.HAProxyConfig, "http-request set-header X-Env")
	assertCustomCRDObservableEqual(t, customCRDColdOracle(t, routeB), deleted)
	fixture.assertRouteExecutions(t, "b", 1)

	fixture.addRoute(t, routeA)
	readded := fixture.renderAndCommitCacheReady(t)
	assertCustomCRDObservableEqual(t, baseline, readded)
	fixture.assertRouteExecutions(t, "a", 1)
	fixture.assertRouteExecutions(t, "b", 1)

	assertCustomCRDFailedRenderRetriesCleanly(t, fixture, routeB)

	fixture.updateRoute(t, routeA)
	final := fixture.renderAndCommitCacheReady(t)
	assertCustomCRDObservableEqual(t, baseline, final)
	fixture.assertRouteExecutions(t, "b", 1)
}

func assertCustomCRDFailedRenderRetriesCleanly(
	t *testing.T,
	fixture *customCRDChartFixture,
	routeB map[string]any,
) {
	t.Helper()
	poisonCandidate := customCRDRoute("a", "10.0.0.7", 7070, "X-Env", "candidate")
	fixture.updateRoute(t, poisonCandidate)
	beforeFailure := fixture.routeExecutionCounts("a")
	fixture.config.TemplatingSettings.ExtraContext["failAfterCustomRoutes"] = true
	failed, err := fixture.render(t)
	require.ErrorContains(t, err, "forced failure after custom routes")
	assert.Nil(t, failed)
	assert.Equal(t, beforeFailure, fixture.routeExecutionCounts("a"))
	fixture.assertRouteExecutions(t, "b", 1)

	fixture.config.TemplatingSettings.ExtraContext["failAfterCustomRoutes"] = false
	retried := fixture.renderAndCommitCacheReady(t)
	assertCustomCRDObservableEqual(t, customCRDColdOracle(t, poisonCandidate, routeB), retried)
	fixture.assertRouteExecutions(t, "a", beforeFailure[customCRDMapComponent]+1)
	fixture.assertRouteExecutions(t, "b", 1)
}

func newCustomCRDChartFixture(tb testing.TB) *customCRDChartFixture {
	tb.Helper()
	return newCustomCRDChartFixtureWithConfig(tb, customCRDChartConfig(tb), &typebootstrap.Result{
		Types:  map[string]reflect.Type{},
		Kinds:  map[string]string{},
		Errors: map[string]error{},
	})
}

func customCRDChartConfig(tb testing.TB) *config.Config {
	tb.Helper()
	return &config.Config{
		Dataplane: testDataplaneConfig(),
		TemplatingSettings: config.TemplatingSettings{ExtraContext: map[string]any{
			"failAfterCustomRoutes": false,
			"noHTTPFrontend":        false,
		}},
		WatchedResources: map[string]config.WatchedResource{
			"routes": {
				APIVersion: "haptic-example.org/v1",
				Resources:  "routes",
				IndexBy:    []string{"metadata.namespace", "metadata.name"},
			},
		},
		TemplateSnippets: loadChartSnippets(tb, chartLibrariesRoot(tb), map[string][]string{
			"base/library.yaml": {"util-backend"},
			"custom-crd-example/library.yaml": {
				"util-custom-route-header-bindings",
				customCRDMapComponent,
				customCRDRulesComponent,
				"frontend-filters-990-custom-route-headers",
				customCRDBackendComponent,
			},
		}),
		HAProxyConfig: config.HAProxyConfig{Template: customCRDChartRoot},
	}
}

func newCustomCRDChartFixtureWithConfig(
	tb testing.TB,
	cfg *config.Config,
	types *typebootstrap.Result,
) *customCRDChartFixture {
	tb.Helper()
	declarations := helpers.BuildAdditionalDeclarations(cfg, types)
	engine, err := helpers.NewEngineFromConfigWithOptions(cfg, nil, nil, declarations, helpers.EngineOptions{})
	require.NoError(tb, err)
	service := NewRenderService(&RenderServiceConfig{
		Engine: engine, Config: cfg, Logger: slog.Default(), Capabilities: defaultCapabilities(),
		TypedResourceTypes: types.Types,
	})
	routes := k8sstore.NewMemoryStore(2)
	return &customCRDChartFixture{
		config:   cfg,
		service:  service,
		routes:   routes,
		provider: stores.NewRealStoreProvider(map[string]stores.Store{"routes": routes}),
	}
}

func customCRDColdOracle(t *testing.T, routes ...map[string]any) *RenderResult {
	t.Helper()
	fixture := newCustomCRDChartFixture(t)
	for _, route := range routes {
		fixture.addRoute(t, route)
	}
	return fixture.renderAndCommitCacheReady(t)
}

func customCRDRoute(name, address string, port int, headerName, headerValue string) map[string]any {
	return map[string]any{
		"apiVersion": "haptic-example.org/v1",
		"kind":       "Route",
		"metadata": map[string]any{
			"namespace": "default",
			"name":      name,
		},
		"spec": map[string]any{
			"backend": map[string]any{"address": address, "port": port},
			"requestHeaders": []any{
				map[string]any{"name": headerName, "value": headerValue},
			},
		},
	}
}

func (f *customCRDChartFixture) addRoute(tb testing.TB, route map[string]any) {
	tb.Helper()
	name := route["metadata"].(map[string]any)["name"].(string)
	require.NoError(tb, f.routes.Add(route, []string{"default", name}))
}

func (f *customCRDChartFixture) updateRoute(tb testing.TB, route map[string]any) {
	tb.Helper()
	name := route["metadata"].(map[string]any)["name"].(string)
	require.NoError(tb, f.routes.Update(route, []string{"default", name}))
}

func (f *customCRDChartFixture) deleteRoute(tb testing.TB, name string) {
	tb.Helper()
	require.NoError(tb, f.routes.Delete("default", name, []string{"default", name}))
}

func (f *customCRDChartFixture) render(tb testing.TB) (*RenderResult, error) {
	tb.Helper()
	return f.service.Render(tb.Context(), f.provider, rendercontext.RenderModeReconcile)
}

func (f *customCRDChartFixture) renderAndCommitCacheReady(t *testing.T) *RenderResult {
	t.Helper()
	result, err := f.render(t)
	require.NoError(t, err)
	require.NoError(t, result.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, f.service)
	return result
}

func (f *customCRDChartFixture) routeExecutionCounts(name string) map[string]uint64 {
	counts := make(map[string]uint64, 3)
	for _, componentName := range []string{
		customCRDMapComponent,
		customCRDRulesComponent,
		customCRDBackendComponent,
	} {
		component := f.service.incremental.components[componentName]
		query := componentQueryKey(&component, "routes", "default", name)
		counts[componentName] = f.service.incremental.graph.Counters(query).Executions
	}
	return counts
}

func (f *customCRDChartFixture) assertRouteExecutions(tb testing.TB, name string, want uint64) {
	tb.Helper()
	for component, got := range f.routeExecutionCounts(name) {
		assert.Equal(tb, want, got, component+"/"+name)
	}
}

func assertCustomCRDObservableEqual(t *testing.T, want, got *RenderResult) {
	t.Helper()
	assert.Equal(t, want.HAProxyConfig, got.HAProxyConfig, "haproxy.cfg bytes")
	assert.Equal(t, requireAuxiliaryFiles(t, want), requireAuxiliaryFiles(t, got), "auxiliary files")
	assert.Equal(t, requireRenderPlan(t, want), requireRenderPlan(t, got), "canonical render plan")
	assert.Equal(t, want.PlanID, got.PlanID, "canonical render plan ID")
	assert.Equal(t, materializedStatusPatches(t, want), materializedStatusPatches(t, got), "status patches")
	assert.Equal(t, requireRenderEvents(t, want), requireRenderEvents(t, got), "events")
	assert.Equal(t, requireRenderedResources(t, want), requireRenderedResources(t, got), "rendered resources")
}

func customCRDMapContent(t *testing.T, result *RenderResult) string {
	t.Helper()
	for _, file := range requireAuxiliaryFiles(t, result).MapFiles {
		if strings.HasSuffix(file.Path, "/route-reqhdr.map") || file.Path == "route-reqhdr.map" {
			return file.Content
		}
	}
	require.FailNow(t, "route-reqhdr.map is missing")
	return ""
}

// TestCustomCRDChartFirstRouteAfterEmptyRenderMatchesColdOracle covers the
// transition the lifecycle test starts past: a render with no routes at all,
// then the first one. The e2e custom-CRD suite failed exactly here -- the
// warm render made no component call for a group that now has an instance.
func TestCustomCRDChartFirstRouteAfterEmptyRenderMatchesColdOracle(t *testing.T) {
	fixture := newCustomCRDChartFixture(t)
	empty := fixture.renderAndCommitCacheReady(t)
	assert.NotContains(t, empty.HAProxyConfig, "http-request set-header")

	routeA := customCRDRoute("a", "10.0.0.1", 8080, "X-Env", "prod eu")
	fixture.addRoute(t, routeA)
	first := fixture.renderAndCommitCacheReady(t)

	assertCustomCRDObservableEqual(t, customCRDColdOracle(t, routeA), first)
	assert.Contains(t, first.HAProxyConfig, "http-request set-header X-Env")
	assert.Contains(t, first.HAProxyConfig, "server primary 10.0.0.1:8080")
	fixture.assertRouteExecutions(t, "a", 1)
}

// TestCustomCRDChartRoutesWithoutAFrontendConsumerRender covers the e2e failure:
// the library's header groups have a live Route, but the chart emits no HTTP
// frontend, so nothing renders the filters snippet that consumes them. The
// groups take no part in the render and contribute the nothing a cold render
// would; a render that refused this could not serve a cluster whose first Route
// predates its first Ingress.
func TestCustomCRDChartRoutesWithoutAFrontendConsumerRender(t *testing.T) {
	fixture := newCustomCRDChartFixture(t)
	fixture.config.TemplatingSettings.ExtraContext["noHTTPFrontend"] = true
	fixture.addRoute(t, customCRDRoute("a", "10.0.0.1", 8080, "X-Env", "prod eu"))

	result := fixture.renderAndCommitCacheReady(t)
	assert.NotContains(t, result.HAProxyConfig, "http-request set-header")
	assert.Contains(t, result.HAProxyConfig, "server primary 10.0.0.1:8080")

	fixture.addRoute(t, customCRDRoute("b", "10.0.0.2", 8080, "X-Env", "prod us"))
	warm := fixture.renderAndCommitCacheReady(t)
	assert.Contains(t, warm.HAProxyConfig, "server primary 10.0.0.2:8080")
}
