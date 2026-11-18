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
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/helpers"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/typebootstrap"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/schemafetcher"
	k8sstore "github.com/haproxy-haptic/HAPTIC/pkg/k8s/store"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
)

const (
	gatewayHTTPHostMapComponent = "map-host-510-gateway-http"
	gatewayGRPCHostMapComponent = "map-host-520-gateway-grpc"
	gatewayHostMapBaselineEnv   = "HAPTIC_GATEWAY_HOST_MAP_BASELINE"
	gatewayHostMapRouteSource   = "httproutes"
)

const gatewayHostMapRoot = `{{ render "map-host-500-gateway" -}}
{{ render "map-host-510-gateway-http" -}}
{{ render "map-host-515-gateway-separator" -}}
{{ render "map-host-520-gateway-grpc" -}}
{%- if tostring(extraContext | dig("failAfterHostMap") | fallback(false)) == "true" -%}
{{ fail("forced failure after gateway host map") }}
{%- end -%}`

const gatewayHostMapLegacyRoot = `{{- render "util-listenerset-candidates" default "" -}}
{{ render "map-host-500-gateway" }}`

type gatewayHostMapFixture struct {
	config          *config.Config
	service         *RenderService
	engine          *dynamicBindingCountingEngine
	gateways        *k8sstore.MemoryStore
	httpRoutes      *k8sstore.MemoryStore
	grpcRoutes      *k8sstore.MemoryStore
	listenerSets    *k8sstore.MemoryStore
	namespaces      *k8sstore.MemoryStore
	referenceGrants *k8sstore.MemoryStore
	provider        stores.StoreProvider
}

func TestGatewayHostMapExecutionScaling(t *testing.T) {
	for _, routeCount := range []int{300, 1000, 3000} {
		t.Run(fmt.Sprintf("routes=%d", routeCount), func(t *testing.T) {
			fixture := newGatewayHostMapFixture(t)
			fixture.addGateway(t, gatewayHostMapGateway("gateway", "2026-01-01T00:00:00Z", "", 80))
			for index := range routeCount {
				name := fmt.Sprintf("route-%06d", index)
				fixture.addHTTPRoute(t, gatewayHostMapRoute("HTTPRoute", name,
					[]any{name + ".example.com"}, gatewayParentRef("Gateway", "gateway")))
			}

			cold := fixture.renderAndCommitCacheReady(t)
			assert.Contains(t, cold.HAProxyConfig, "route-000000.example.com:")
			assert.Contains(t, cold.HAProxyConfig,
				fmt.Sprintf("route-%06d.example.com:", routeCount-1))
			coldCounts := fixture.engine.executionCounts()
			require.Equal(t, routeCount, gatewayHostMapSourceExecutionTotal(coldCounts))

			warm := fixture.renderAndCommitCacheReady(t)
			assert.Equal(t, cold.HAProxyConfig, warm.HAProxyConfig)
			assert.Equal(t, coldCounts, fixture.engine.executionCounts())

			fixture.updateHTTPRoute(t, gatewayHostMapRoute("HTTPRoute", "route-000000",
				[]any{"changed.example.com"}, gatewayParentRef("Gateway", "gateway")))
			changed := fixture.renderAndCommitCacheReady(t)
			assert.Contains(t, changed.HAProxyConfig, "changed.example.com:")
			assert.NotContains(t, changed.HAProxyConfig, "route-000000.example.com:")
			changedCounts := fixture.engine.executionCounts()
			assert.Equal(t, routeCount+1,
				gatewayHostMapSourceExecutionTotal(changedCounts))
			assert.Equal(t, coldCounts[fmt.Sprintf("httproutes/route-%06d", routeCount-1)],
				changedCounts[fmt.Sprintf("httproutes/route-%06d", routeCount-1)])
		})
	}
}

func TestGatewayHostMapPortScopeSelectorsTrackOnlyValueTransitions(t *testing.T) {
	fixture := newGatewayHostMapFixture(t)
	fixture.addGateway(t, gatewayHostMapGateway("gw-old-n", "2026-01-01T00:00:00Z", "", 80))
	fixture.addGateway(t, gatewayHostMapGateway("gw-new-m", "2026-06-01T00:00:00Z", "", 80))
	fixture.addHTTPRoute(t, gatewayHostMapRoute("HTTPRoute", "route", nil,
		gatewayParentRef("Gateway", "gw-new-m")))

	contested := fixture.renderAndCommitCacheReady(t)
	assert.Contains(t, contested.HAProxyConfig, ":18081 :18081")
	fixture.assertExecutions(t, "route", 1)

	fixture.addGateway(t, gatewayHostMapGateway("gw-bystander", "2026-07-01T00:00:00Z", "", 80))
	unrelated := fixture.renderAndCommitCacheReady(t)
	assert.Equal(t, contested.HAProxyConfig, unrelated.HAProxyConfig)
	fixture.assertExecutions(t, "route", 1)

	fixture.deleteGateway(t, "gw-old-n")
	promoted := fixture.renderAndCommitCacheReady(t)
	assert.Contains(t, promoted.HAProxyConfig, ":18080 :18080")
	assert.NotContains(t, promoted.HAProxyConfig, ":18081 :18081")
	fixture.assertExecutions(t, "route", 2)

	fixture.addGateway(t, gatewayHostMapGateway("gw-old-n", "2026-01-01T00:00:00Z", "", 80))
	demoted := fixture.renderAndCommitCacheReady(t)
	assert.Equal(t, contested.HAProxyConfig, demoted.HAProxyConfig)
	fixture.assertExecutions(t, "route", 3)
}

func TestGatewayHostMapMissingGatewayAndListenerSetTransitions(t *testing.T) {
	t.Run("Gateway", func(t *testing.T) {
		fixture := newGatewayHostMapFixture(t)
		fixture.addHTTPRoute(t, gatewayHostMapRoute("HTTPRoute", "route",
			[]any{"missing.example.com"}, gatewayParentRef("Gateway", "missing")))

		missing := fixture.renderAndCommitCacheReady(t)
		assert.Contains(t, missing.HAProxyConfig, "missing.example.com missing.example.com")
		fixture.addGateway(t, gatewayHostMapGateway("missing", "2026-01-01T00:00:00Z", "", 80))
		present := fixture.renderAndCommitCacheReady(t)
		assert.Contains(t, present.HAProxyConfig, "missing.example.com:")
		assert.NotContains(t, present.HAProxyConfig,
			"missing.example.com missing.example.com")
		fixture.deleteGateway(t, "missing")
		assert.Equal(t, missing.HAProxyConfig, fixture.renderAndCommitCacheReady(t).HAProxyConfig)
		fixture.assertExecutions(t, "route", 3)
	})

	t.Run("ListenerSet", func(t *testing.T) {
		fixture := newGatewayHostMapFixture(t)
		gateway := gatewayHostMapGateway("gateway", "2026-01-01T00:00:00Z", "gateway.example", 80)
		gateway["spec"].(map[string]any)["allowedListeners"] = map[string]any{
			"namespaces": map[string]any{"from": "All"},
		}
		fixture.addGateway(t, gateway)
		fixture.addHTTPRoute(t, gatewayHostMapRoute("HTTPRoute", "route",
			[]any{"route.example.com"}, gatewayParentRef("ListenerSet", "listeners")))

		missing := fixture.renderAndCommitCacheReady(t)
		assert.Contains(t, missing.HAProxyConfig, "route.example.com route.example.com")
		fixture.addListenerSet(t, gatewayHostMapListenerSet("listeners", "gateway", "", 8080))
		present := fixture.renderAndCommitCacheReady(t)
		assert.Contains(t, present.HAProxyConfig, "route.example.com:8080 route.example.com:8080")
		assert.NotContains(t, present.HAProxyConfig, "route.example.com route.example.com")
		fixture.deleteListenerSet(t, "listeners")
		assert.Equal(t, missing.HAProxyConfig, fixture.renderAndCommitCacheReady(t).HAProxyConfig)
		fixture.assertExecutions(t, "route", 3)
	})
}

func TestGatewayHostMapAdmissionAndRootAbortDoNotPoisonCache(t *testing.T) {
	fixture := newGatewayHostMapFixture(t)
	fixture.addGateway(t, gatewayHostMapGateway("gateway", "2026-01-01T00:00:00Z", "", 80))
	fixture.addHTTPRoute(t, gatewayHostMapRoute("HTTPRoute", "subject",
		[]any{"live.example.com"}, gatewayParentRef("Gateway", "gateway")))
	baseline := fixture.renderAndCommitCacheReady(t)
	fixture.assertExecutions(t, "subject", 1)

	proposed := gatewayHostMapRoute("HTTPRoute", "subject",
		[]any{"proposed.example.com"}, gatewayParentRef("Gateway", "gateway"))
	overlay := stores.NewOverlayStoreProvider(
		fixture.provider,
		stores.NewValidationContext(map[string]*stores.StoreOverlay{
			"httproutes": stores.NewStoreOverlayForUpdate(&unstructured.Unstructured{Object: proposed}),
		}),
	)
	admission, err := fixture.service.Render(
		t.Context(), overlay, rendercontext.RenderModeAdmission,
		rendercontext.WithAdmissionSubject("httproutes", "default", "subject"),
	)
	require.NoError(t, err)
	assert.Contains(t, admission.HAProxyConfig, "proposed.example.com:")
	admission.InputTransaction.Abort()
	assert.Equal(t, baseline.HAProxyConfig, fixture.renderAndCommitCacheReady(t).HAProxyConfig)
	fixture.assertExecutions(t, "subject", 1)

	fixture.updateHTTPRoute(t, proposed)
	fixture.config.TemplatingSettings.ExtraContext["failAfterHostMap"] = true
	failed, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.ErrorContains(t, err, "forced failure after gateway host map")
	assert.Nil(t, failed)
	fixture.assertExecutions(t, "subject", 1)

	fixture.config.TemplatingSettings.ExtraContext["failAfterHostMap"] = false
	retried := fixture.renderAndCommitCacheReady(t)
	assert.Contains(t, retried.HAProxyConfig, "proposed.example.com:")
	assert.NotContains(t, retried.HAProxyConfig, "live.example.com:")
	fixture.assertExecutions(t, "subject", 2)
}

func TestGatewayHostMapColdMatchesDetachedHEADGenerator(t *testing.T) {
	baselineRoot := os.Getenv(gatewayHostMapBaselineEnv)
	if baselineRoot == "" {
		t.Skip("run scripts/test-gateway-host-map-differential.sh to compare against detached HEAD")
	}

	current := newGatewayHostMapFixture(t)
	legacy := newGatewayHostMapFixtureWithTemplates(
		t, loadGatewayHostMapLegacySnippets(t, baselineRoot), gatewayHostMapLegacyRoot)
	populateGatewayHostMapDifferentialFixture(t, current)
	legacy.provider = current.provider

	currentResult := current.renderAndCommitCacheReady(t)
	legacyResult := legacy.renderAndCommitCacheReady(t)
	assert.Equal(t, legacyResult.HAProxyConfig, currentResult.HAProxyConfig, "haproxy.cfg bytes")
	assert.Equal(t, requireAuxiliaryFiles(t, legacyResult), requireAuxiliaryFiles(t, currentResult), "auxiliary files")
	assert.Equal(t, requireRenderPlan(t, legacyResult), requireRenderPlan(t, currentResult), "canonical render plan")
	assert.Equal(t, legacyResult.PlanID, currentResult.PlanID, "canonical render plan ID")
	assert.Equal(t, materializedStatusPatches(t, legacyResult), materializedStatusPatches(t, currentResult), "status patches")
	assert.Equal(t, requireRenderEvents(t, legacyResult), requireRenderEvents(t, currentResult), "events")
	assert.Equal(t, requireRenderedResources(t, legacyResult), requireRenderedResources(t, currentResult), "rendered resources")
}

func populateGatewayHostMapDifferentialFixture(t *testing.T, fixture *gatewayHostMapFixture) {
	t.Helper()
	fixture.addNamespace(t, gatewayHostMapNamespace("default", map[string]any{"team": "edge"}))
	fixture.addGateway(t, gatewayHostMapGateway("gw-old-n", "2026-01-01T00:00:00Z", "", 80))
	fixture.addGateway(t, gatewayHostMapGateway("gw-new-m", "2026-06-01T00:00:00Z", "", 80))
	listenerGateway := gatewayHostMapGateway("listener-parent", "2026-03-01T00:00:00Z", "parent.example.com", 80)
	listenerGateway["spec"].(map[string]any)["allowedListeners"] = map[string]any{
		"namespaces": map[string]any{"from": "Selector", "selector": map[string]any{
			"matchLabels": map[string]any{"team": "edge"},
		}},
	}
	fixture.addGateway(t, listenerGateway)
	fixture.addListenerSet(t, gatewayHostMapListenerSet("listeners", "listener-parent", "ls.example.com", 8080))
	fixture.addHTTPRoute(t, gatewayHostMapRoute("HTTPRoute", "catchall", nil,
		gatewayParentRef("Gateway", "gw-new-m")))
	fixture.addHTTPRoute(t, gatewayHostMapRoute("HTTPRoute", "orphan",
		[]any{"one.example.com", "two.example.com"}))
	fixture.addHTTPRoute(t, gatewayHostMapRoute("HTTPRoute", "listener-set",
		[]any{"ls.example.com"}, gatewayParentRef("ListenerSet", "listeners")))
	fixture.addGRPCRoute(t, gatewayHostMapRoute("GRPCRoute", "grpc",
		[]any{"grpc.example.com"}, gatewayParentRef("Gateway", "gw-old-n")))
}

func newGatewayHostMapFixture(tb testing.TB) *gatewayHostMapFixture {
	tb.Helper()
	return newGatewayHostMapFixtureWithTemplates(tb, loadGatewayHostMapCurrentSnippets(tb), gatewayHostMapRoot)
}

func newGatewayHostMapFixtureWithTemplates(
	tb testing.TB,
	snippets map[string]config.TemplateSnippet,
	root string,
) *gatewayHostMapFixture {
	tb.Helper()
	cfg := &config.Config{
		Dataplane: testDataplaneConfig(),
		TemplatingSettings: config.TemplatingSettings{ExtraContext: map[string]any{
			"perGatewayPodPortBase": 18000, "perGatewayPodPortRange": 1000,
			"failAfterHostMap": false,
		}},
		WatchedResources: map[string]config.WatchedResource{
			"gateways":        {APIVersion: "gateway.networking.k8s.io/v1", Resources: "gateways", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"httproutes":      {APIVersion: "gateway.networking.k8s.io/v1", Resources: "httproutes", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"grpcroutes":      {APIVersion: "gateway.networking.k8s.io/v1", Resources: "grpcroutes", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"listenersets":    {APIVersion: "gateway.networking.k8s.io/v1", Resources: "listenersets", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"namespaces":      {APIVersion: "v1", Resources: "namespaces", IndexBy: []string{"metadata.name"}},
			"referencegrants": {APIVersion: "gateway.networking.k8s.io/v1", Resources: "referencegrants", IndexBy: []string{"metadata.namespace", "metadata.name"}},
		},
		TemplateSnippets: snippets,
		HAProxyConfig:    config.HAProxyConfig{Template: root},
	}
	raceScaleRenderTimeout(cfg)
	require.NoError(tb, config.ValidateTemplateStructure(cfg))
	types := gatewayHostMapSchemaTypes(tb)
	declarations := helpers.BuildAdditionalDeclarations(cfg, types)
	baseEngine, err := helpers.NewEngineFromConfigWithOptions(cfg, nil, nil, declarations, helpers.EngineOptions{})
	require.NoError(tb, err)
	engine := newDynamicBindingCountingEngine(tb, baseEngine)
	service := NewRenderService(&RenderServiceConfig{
		Engine: engine, Config: cfg, Logger: slog.Default(), Capabilities: defaultCapabilities(),
		TypedResourceTypes: types.Types,
	})
	fixture := &gatewayHostMapFixture{
		config: cfg, service: service, engine: engine,
		gateways: k8sstore.NewMemoryStore(2), httpRoutes: k8sstore.NewMemoryStore(2),
		grpcRoutes: k8sstore.NewMemoryStore(2), listenerSets: k8sstore.NewMemoryStore(2),
		namespaces: k8sstore.NewMemoryStore(1), referenceGrants: k8sstore.NewMemoryStore(2),
	}
	fixture.provider = stores.NewRealStoreProvider(map[string]stores.Store{
		"gateways": fixture.gateways, "httproutes": fixture.httpRoutes,
		"grpcroutes": fixture.grpcRoutes, "listenersets": fixture.listenerSets,
		"namespaces": fixture.namespaces, "referencegrants": fixture.referenceGrants,
	})
	return fixture
}

func gatewayHostMapSchemaTypes(tb testing.TB) *typebootstrap.Result {
	tb.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	require.True(tb, ok)
	schemaRoot := filepath.Join(filepath.Dir(sourceFile), "..", "..", "..", "tests", "schemas")
	fetcher, err := schemafetcher.NewDirFetcher(schemaRoot)
	require.NoError(tb, err)
	result, err := typebootstrap.Bootstrap(tb.Context(), typebootstrap.Config{
		Resources: []typebootstrap.Resource{
			{Name: "gateways", GVK: schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "Gateway"}},
			{Name: "httproutes", GVK: schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"}},
			{Name: "grpcroutes", GVK: schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "GRPCRoute"}},
			{Name: "listenersets", GVK: schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "ListenerSet"}},
			{Name: "namespaces", GVK: schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}},
			{Name: "referencegrants", GVK: schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "ReferenceGrant"}},
		},
		Fetcher: fetcher,
		Logger:  slog.Default(),
	})
	require.NoError(tb, err)
	require.Empty(tb, result.Errors)
	require.Len(tb, result.Types, 6)
	return result
}

func loadGatewayHostMapCurrentSnippets(tb testing.TB) map[string]config.TemplateSnippet {
	tb.Helper()
	return loadChartSnippets(tb, chartLibrariesRoot(tb), map[string][]string{
		"base/library.yaml": {"util-host-key"},
		"gateway/15-pod-port-allocator.yaml": {
			"util-gateway-pod-port-allocation",
			"util-gateway-pod-port-bindings",
			"gateway-pod-port-candidates-100-gateway",
			"gateway-pod-port-allocations-200-leader",
		},
		"gateway/21-route-helpers.yaml": {"util-hostname-intersect-gateway"},
		"gateway/40-maps-host.yaml": {
			"util-gateway-host-port-suffix",
			"map-hostvalues-479-gateway-listenersets-empty",
			"map-hostvalues-480-gateway-listenersets",
			"map-hostvalues-490-gateway-port-scopes",
			"gateway-host-port-scopes-100-gateway",
			"util-generate-httproute-host-map-gateway",
			"util-generate-grpcroute-host-map-gateway",
			"map-host-500-gateway",
			gatewayHTTPHostMapComponent,
			"map-host-515-gateway-separator",
			gatewayGRPCHostMapComponent,
		},
	})
}

func loadGatewayHostMapLegacySnippets(tb testing.TB, baselineRoot string) map[string]config.TemplateSnippet {
	tb.Helper()
	return loadChartSnippets(tb, baselineRoot, map[string][]string{
		"charts/haptic/charts/base/library.yaml": {"util-macros", "util-host-key"},
		"charts/haptic/charts/gateway/15-pod-port-allocator.yaml": {
			"util-gateway-pod-port-allocator", "util-gateway-port-scope",
		},
		"charts/haptic/charts/gateway/20-route-analysis.yaml": {
			"util-listenerset-candidates", "util-listenerset-routing-gate",
		},
		"charts/haptic/charts/gateway/21-route-helpers.yaml": {
			"util-hostname-intersect-gateway", "util-reference-grant-permitted",
		},
		"charts/haptic/charts/gateway/40-maps-host.yaml": {
			"util-generate-httproute-host-map-gateway",
			"util-generate-grpcroute-host-map-gateway",
			"util-sharded-host-map-gateway",
			"map-host-500-gateway",
		},
	})
}

func gatewayHostMapGateway(name, creationTimestamp, hostname string, port int64) map[string]any {
	return map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1", "kind": "Gateway",
		"metadata": map[string]any{
			"namespace": "default", "name": name, "creationTimestamp": creationTimestamp,
		},
		"spec": map[string]any{
			"gatewayClassName": "haptic",
			"listeners": []any{map[string]any{
				"name": "http", "protocol": "HTTP", "port": port, "hostname": hostname,
			}},
		},
	}
}

func gatewayHostMapRoute(kind, name string, hostnames []any, parentRefs ...any) map[string]any {
	return map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1", "kind": kind,
		"metadata": map[string]any{"namespace": "default", "name": name},
		"spec":     map[string]any{"hostnames": hostnames, "parentRefs": parentRefs},
	}
}

func gatewayParentRef(kind, name string) map[string]any {
	return map[string]any{
		"group": "gateway.networking.k8s.io", "kind": kind, "name": name,
	}
}

func gatewayHostMapListenerSet(name, gatewayName, hostname string, port int64) map[string]any {
	return map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1", "kind": "ListenerSet",
		"metadata": map[string]any{"namespace": "default", "name": name},
		"spec": map[string]any{
			"parentRef": map[string]any{"name": gatewayName},
			"listeners": []any{map[string]any{
				"name": "listener", "protocol": "HTTP", "port": port, "hostname": hostname,
			}},
		},
	}
}

func gatewayHostMapNamespace(name string, labels map[string]any) map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{"name": name, "labels": labels},
	}
}

func gatewayHostMapSourceExecutionTotal(counts map[string]int) int {
	const prefix = "httproutes/"
	total := 0
	for key, count := range counts {
		if strings.HasPrefix(key, prefix) {
			total += count
		}
	}
	return total
}

func (f *gatewayHostMapFixture) addGateway(tb testing.TB, resource map[string]any) {
	tb.Helper()
	require.NoError(tb, f.gateways.Add(resource, []string{"default", resource["metadata"].(map[string]any)["name"].(string)}))
}

func (f *gatewayHostMapFixture) deleteGateway(tb testing.TB, name string) {
	tb.Helper()
	require.NoError(tb, f.gateways.Delete("default", name, []string{"default", name}))
}

func (f *gatewayHostMapFixture) addHTTPRoute(tb testing.TB, resource map[string]any) {
	tb.Helper()
	name := resource["metadata"].(map[string]any)["name"].(string)
	require.NoError(tb, f.httpRoutes.Add(resource, []string{"default", name}))
}

func (f *gatewayHostMapFixture) updateHTTPRoute(tb testing.TB, resource map[string]any) {
	tb.Helper()
	name := resource["metadata"].(map[string]any)["name"].(string)
	require.NoError(tb, f.httpRoutes.Update(resource, []string{"default", name}))
}

func (f *gatewayHostMapFixture) addGRPCRoute(tb testing.TB, resource map[string]any) {
	tb.Helper()
	name := resource["metadata"].(map[string]any)["name"].(string)
	require.NoError(tb, f.grpcRoutes.Add(resource, []string{"default", name}))
}

func (f *gatewayHostMapFixture) addListenerSet(tb testing.TB, resource map[string]any) {
	tb.Helper()
	name := resource["metadata"].(map[string]any)["name"].(string)
	require.NoError(tb, f.listenerSets.Add(resource, []string{"default", name}))
}

func (f *gatewayHostMapFixture) deleteListenerSet(tb testing.TB, name string) {
	tb.Helper()
	require.NoError(tb, f.listenerSets.Delete("default", name, []string{"default", name}))
}

func (f *gatewayHostMapFixture) addNamespace(tb testing.TB, resource map[string]any) {
	tb.Helper()
	name := resource["metadata"].(map[string]any)["name"].(string)
	require.NoError(tb, f.namespaces.Add(resource, []string{name}))
}

func (f *gatewayHostMapFixture) renderAndCommitCacheReady(tb testing.TB) *RenderResult {
	tb.Helper()
	result := f.renderAndCommitAuthoritative(tb)
	waitForIncrementalCache(tb, f.service)
	return result
}

func (f *gatewayHostMapFixture) renderAndCommitAuthoritative(tb testing.TB) *RenderResult {
	tb.Helper()
	result, err := f.service.Render(tb.Context(), f.provider, rendercontext.RenderModeReconcile)
	require.NoError(tb, err)
	require.NoError(tb, result.InputTransaction.Commit(tb.Context()))
	return result
}

func (f *gatewayHostMapFixture) executions(componentName, source, name string) uint64 {
	component := f.service.incremental.components[componentName]
	query := componentQueryKey(&component, source, "default", name)
	return f.service.incremental.graph.Counters(query).Executions
}

func (f *gatewayHostMapFixture) assertExecutions(
	tb testing.TB,
	name string,
	want uint64,
) {
	tb.Helper()
	const componentName = gatewayHTTPHostMapComponent
	executions := f.executions(componentName, gatewayHostMapRouteSource, name)
	assert.Equal(tb, want, executions, componentName+"/"+name)
}
