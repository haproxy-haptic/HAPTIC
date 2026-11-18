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
	gatewayRouteCandidateHTTPComponent = "gateway-route-candidates-100-http"
	gatewayRouteAnalysisHTTPComponent  = "gateway-route-analysis-100-http"
	gatewayRoutePathHTTPComponent      = "gateway-route-paths-100-http"
	gatewayRouteAnalysisBaselineEnv    = "HAPTIC_GATEWAY_ROUTE_ANALYSIS_BASELINE"
)

const gatewayFrontendMTLSResolutionRoot = `{{- render "gateway-frontend-ca-permissions-100-gateway" -}}
{{- render "gateway-frontend-mtls-100-gateway" -}}`

const gatewayRouteAnalysisRoot = gatewayFrontendMTLSResolutionRoot + `{{ render "map-path-exact-500-gateway" -}}
{%- if tostring(extraContext | dig("failAfterRouteAnalysis") | fallback(false)) == "true" -%}
{{ fail("forced failure after gateway route analysis") }}
{%- end -%}`

const gatewayRouteAnalysisDifferentialRoot = `{{ render "map-path-exact-500-gateway" -}}
{{ render "map-pfxexact-500-gateway" -}}
{{ render "map-path-prefix-500-gateway" -}}
{{ render "map-path-regex-500-gateway" -}}`

const gatewayRoutePathGetSingleDependencyRoot = `{{ render "test-gateway-route-analysis" -}}
{{- render "gateway-route-paths-100-http" -}}
{{ incremental_ranked_fragments("gateway-route-paths", "exact") }}`

const gatewayRoutePathDependencyProducerTemplate = `{%%
var namespace = dig_string(item, "", "metadata", "namespace")
var name = dig_string(item, "", "metadata", "name")
if name == "subject" {
  var routeKey = "HTTPRoute/" + namespace + "/" + name
  var entryID = routeKey + "/entry"
  show shared.Publish("routes", routeKey, map[string]any{"entries": []any{entryID}})
  show shared.Publish("entries", entryID, map[string]any{
    "rawRepresentative": true,
    "pathType": "Exact",
    "pathValue": "/shared",
    "hostname": "shared.example.com",
    "groupKind": "HTTPRoute",
    "groupRouteNamespace": namespace,
    "groupRouteName": "target",
    "groupRank": "0",
    "pathKey": "shared.example.com|Exact|/shared",
    "conflictGroup": "default_subject_0",
  })
}
%%}`

type gatewayRouteAnalysisFixture struct {
	config          *config.Config
	service         *RenderService
	engine          *dynamicBindingCountingEngine
	gateways        *k8sstore.MemoryStore
	httpRoutes      *k8sstore.MemoryStore
	grpcRoutes      *k8sstore.MemoryStore
	listenerSets    *k8sstore.MemoryStore
	namespaces      *k8sstore.MemoryStore
	referenceGrants *k8sstore.MemoryStore
	configMaps      *k8sstore.MemoryStore
	secrets         *k8sstore.MemoryStore
	provider        stores.StoreProvider
}

func TestGatewayRouteAnalysisDoesNotScanWholeStores(t *testing.T) {
	source, err := os.ReadFile(filepath.Join(chartLibrariesRoot(t), "gateway", "20-route-analysis.yaml"))
	require.NoError(t, err)
	contents := string(source)
	assert.NotContains(t, contents, ".List()")
	for _, snippet := range []string{
		"util-gateway-analysis:",
		"util-listenersets-contrib:",
		"util-listenerset-candidates:",
		"util-effective-listeners:",
		"util-route-effective-hosts:",
		"util-analyze-routes:",
	} {
		assert.NotContains(t, contents, snippet)
	}
}

func TestGatewayRouteAnalysisExecutionScaling(t *testing.T) {
	for _, routeCount := range []int{300, 1000, 3000} {
		t.Run(fmt.Sprintf("routes=%d", routeCount), func(t *testing.T) {
			fixture := newGatewayRouteAnalysisFixture(t)
			fixture.addGateway(t, gatewayRouteAnalysisGateway())
			for index := range routeCount {
				name := fmt.Sprintf("route-%06d", index)
				fixture.addHTTPRoute(t, gatewayRouteAnalysisRoute(name, name+".example.com", "/"+name, ""))
			}

			cold := fixture.renderAndCommitCacheReady(t)
			assert.Contains(t, cold.HAProxyConfig, "route-000000.example.com:")
			assert.Contains(t, cold.HAProxyConfig, "/route-000000 GW_ROUTE_ID:")
			assert.Contains(t, cold.HAProxyConfig, fmt.Sprintf("route-%06d.example.com:", routeCount-1))
			assert.Contains(t, cold.HAProxyConfig,
				fmt.Sprintf("/route-%06d GW_ROUTE_ID:", routeCount-1))
			fixture.assertHTTPRouteExecutions(t, "route-000000", 1, 1, 1)
			fixture.assertHTTPRouteExecutions(t, fmt.Sprintf("route-%06d", routeCount-1), 1, 1, 1)
			coldCounts := fixture.engine.executionCounts()
			assert.Equal(t, routeCount*3, gatewayHostMapSourceExecutionTotal(coldCounts))

			warm := fixture.renderAndCommitCacheReady(t)
			assert.Equal(t, cold.HAProxyConfig, warm.HAProxyConfig)
			assert.Equal(t, coldCounts, fixture.engine.executionCounts())

			fixture.updateHTTPRoute(t,
				gatewayRouteAnalysisRoute("route-000000", "changed.example.com", "/changed", ""))
			changed := fixture.renderAndCommitCacheReady(t)
			assert.Contains(t, changed.HAProxyConfig, "changed.example.com:")
			assert.Contains(t, changed.HAProxyConfig, "/changed GW_ROUTE_ID:")
			assert.NotContains(t, changed.HAProxyConfig, "route-000000.example.com:")
			assert.Equal(t, routeCount*3+3,
				gatewayHostMapSourceExecutionTotal(fixture.engine.executionCounts()))
			fixture.assertHTTPRouteExecutions(t, "route-000000", 2, 2, 2)
			fixture.assertHTTPRouteExecutions(t, fmt.Sprintf("route-%06d", routeCount-1), 1, 1, 1)
		})
	}
}

func TestGatewayRouteAnalysisUnrelatedInputsDoNotExecute(t *testing.T) {
	fixture := newGatewayRouteAnalysisFixture(t)
	fixture.addGateway(t, gatewayRouteAnalysisGateway())
	fixture.addHTTPRoute(t, gatewayRouteAnalysisRoute("subject", "subject.example.com", "/subject", ""))
	baseline := fixture.renderAndCommitCacheReady(t)
	fixture.assertHTTPRouteExecutions(t, "subject", 1, 1, 1)

	fixture.addConfigMap(t, "unrelated", "one")
	assert.Equal(t, baseline.HAProxyConfig, fixture.renderAndCommitCacheReady(t).HAProxyConfig)
	fixture.assertHTTPRouteExecutions(t, "subject", 1, 1, 1)

	fixture.updateConfigMap(t, "unrelated", "two")
	assert.Equal(t, baseline.HAProxyConfig, fixture.renderAndCommitCacheReady(t).HAProxyConfig)
	fixture.assertHTTPRouteExecutions(t, "subject", 1, 1, 1)
}

func TestGatewayRouteAnalysisCollisionFanoutAndDeletionPromotion(t *testing.T) {
	fixture := newGatewayRouteAnalysisFixture(t)
	fixture.addGateway(t, gatewayRouteAnalysisGateway())
	fixture.addHTTPRoute(t, gatewayRouteAnalysisRoute("a", "shared.example.com", "/shared", ""))
	fixture.addHTTPRoute(t, gatewayRouteAnalysisRoute("b", "shared.example.com", "/shared", ""))
	fixture.addHTTPRoute(t, gatewayRouteAnalysisRoute("isolated", "isolated.example.com", "/isolated", ""))

	both := fixture.renderAndCommitCacheReady(t)
	assert.Contains(t, both.HAProxyConfig, "h_default_a_0__h_default_b_0")
	fixture.assertHTTPRouteExecutions(t, "a", 1, 1, 1)
	fixture.assertHTTPRouteExecutions(t, "b", 1, 1, 1)
	fixture.assertHTTPRouteExecutions(t, "isolated", 1, 1, 1)

	fixture.updateHTTPRoute(t, gatewayRouteAnalysisRoute("b", "shared.example.com", "/shared", "POST"))
	conflicted := fixture.renderAndCommitCacheReady(t)
	assert.Equal(t, both.HAProxyConfig, conflicted.HAProxyConfig)
	fixture.assertHTTPRouteExecutions(t, "a", 1, 2, 2)
	fixture.assertHTTPRouteExecutions(t, "b", 2, 2, 2)
	fixture.assertHTTPRouteExecutions(t, "isolated", 1, 1, 1)

	fixture.deleteHTTPRoute(t, "a")
	promoted := fixture.renderAndCommitCacheReady(t)
	assert.NotContains(t, promoted.HAProxyConfig, "h_default_a_0__h_default_b_0")
	assert.Contains(t, promoted.HAProxyConfig, "default_b_0")
	fixture.assertHTTPRouteExecutions(t, "b", 2, 3, 3)
	fixture.assertHTTPRouteExecutions(t, "isolated", 1, 1, 1)

	fixture.addHTTPRoute(t, gatewayRouteAnalysisRoute("a", "shared.example.com", "/shared", ""))
	restored := fixture.renderAndCommitCacheReady(t)
	assert.Equal(t, conflicted.HAProxyConfig, restored.HAProxyConfig)
	fixture.assertHTTPRouteExecutions(t, "a", 1, 1, 1)
	fixture.assertHTTPRouteExecutions(t, "b", 2, 4, 4)
	fixture.assertHTTPRouteExecutions(t, "isolated", 1, 1, 1)
}

func TestGatewayRouteAnalysisRetainsCrossRouteGetSingleDependency(t *testing.T) {
	fixture := newGatewayRouteAnalysisFixture(t)
	fixture.addGateway(t, gatewayRouteAnalysisGateway())
	fixture.addHTTPRoute(t, gatewayRouteAnalysisRouteWithHeader("a", "one"))
	fixture.addHTTPRoute(t, gatewayRouteAnalysisRouteWithHeader("b", "one"))
	fixture.renderAndCommitCacheReady(t)

	candidate := fixture.service.incremental.components[gatewayRouteCandidateHTTPComponent]
	candidateA := componentQueryKey(&candidate, "httproutes", "default", "a")
	beforeCandidate, found := fixture.service.incremental.graph.Value(candidateA)
	require.True(t, found)
	fixture.assertHTTPRouteExecutions(t, "a", 1, 1, 1)
	fixture.assertHTTPRouteExecutions(t, "b", 1, 1, 1)

	changed := gatewayRouteAnalysisRouteWithHeader("a", "two")
	fixture.updateHTTPRoute(t, changed)
	fixture.renderAndCommitCacheReady(t)

	afterCandidate, found := fixture.service.incremental.graph.Value(candidateA)
	require.True(t, found)
	assert.Equal(t, beforeCandidate, afterCandidate, "candidate publication must stay byte-identical")
	fixture.assertHTTPRouteExecutions(t, "a", 2, 2, 2)
	fixture.assertHTTPRouteExecutions(t, "b", 1, 2, 2)
}

func TestGatewayRoutePathsRetainsGroupedRouteGetSingleDependency(t *testing.T) {
	fixture := newGatewayRoutePathDependencyFixture(t)
	fixture.addHTTPRoute(t, gatewayRouteAnalysisRoute("subject", "unused.example.com", "/unused", ""))
	fixture.addHTTPRoute(t, gatewayRouteAnalysisRoute("target", "target.example.com", "/target", ""))

	first := fixture.renderAndCommitCacheReady(t)
	assert.Contains(t, first.HAProxyConfig, "shared.example.com/shared GW_ROUTE_ID:http:default_subject_0")
	assert.Equal(t, uint64(1), fixture.executions(gatewayRoutePathHTTPComponent, "subject"))

	target := gatewayRouteAnalysisRoute("target", "target.example.com", "/target", "")
	target["metadata"].(map[string]any)["labels"] = map[string]any{"changed": "true"}
	fixture.updateHTTPRoute(t, target)
	second := fixture.renderAndCommitCacheReady(t)

	assert.Equal(t, first.HAProxyConfig, second.HAProxyConfig)
	assert.Equal(t, uint64(2), fixture.executions(gatewayRoutePathHTTPComponent, "subject"))
}

func TestGatewayRouteAnalysisMissingGatewayTransitions(t *testing.T) {
	fixture := newGatewayRouteAnalysisFixture(t)
	fixture.addHTTPRoute(t, gatewayRouteAnalysisRoute("route", "missing.example.com", "/route", ""))

	missing := fixture.renderAndCommitCacheReady(t)
	assert.Contains(t, missing.HAProxyConfig, "missing.example.com/route")
	assert.NotContains(t, missing.HAProxyConfig, "missing.example.com:")

	fixture.addGateway(t, gatewayRouteAnalysisGateway())
	present := fixture.renderAndCommitCacheReady(t)
	assert.Contains(t, present.HAProxyConfig, "missing.example.com:")
	assert.Contains(t, present.HAProxyConfig, "/route GW_ROUTE_ID:")
	assert.NotContains(t, present.HAProxyConfig, "missing.example.com/route")

	fixture.deleteGateway(t, "gateway")
	assert.Equal(t, missing.HAProxyConfig, fixture.renderAndCommitCacheReady(t).HAProxyConfig)
	fixture.assertHTTPRouteExecutions(t, "route", 3, 3, 3)
}

func TestGatewayRouteAnalysisAdmissionAndRootAbortDoNotPoisonCache(t *testing.T) {
	fixture := newGatewayRouteAnalysisFixture(t)
	fixture.addGateway(t, gatewayRouteAnalysisGateway())
	fixture.addHTTPRoute(t, gatewayRouteAnalysisRoute("subject", "live.example.com", "/live", ""))
	baseline := fixture.renderAndCommitCacheReady(t)
	fixture.assertHTTPRouteExecutions(t, "subject", 1, 1, 1)

	proposed := gatewayRouteAnalysisRoute("subject", "proposed.example.com", "/proposed", "")
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
	assert.Contains(t, admission.HAProxyConfig, "/proposed GW_ROUTE_ID:")
	admission.InputTransaction.Abort()
	assert.Equal(t, baseline.HAProxyConfig, fixture.renderAndCommitCacheReady(t).HAProxyConfig)
	fixture.assertHTTPRouteExecutions(t, "subject", 1, 1, 1)

	fixture.updateHTTPRoute(t, proposed)
	fixture.config.TemplatingSettings.ExtraContext["failAfterRouteAnalysis"] = true
	failed, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.ErrorContains(t, err, "forced failure after gateway route analysis")
	assert.Nil(t, failed)
	fixture.assertHTTPRouteExecutions(t, "subject", 1, 1, 1)

	fixture.config.TemplatingSettings.ExtraContext["failAfterRouteAnalysis"] = false
	retried := fixture.renderAndCommitCacheReady(t)
	assert.Contains(t, retried.HAProxyConfig, "proposed.example.com:")
	assert.Contains(t, retried.HAProxyConfig, "/proposed GW_ROUTE_ID:")
	assert.NotContains(t, retried.HAProxyConfig, "live.example.com:")
	fixture.assertHTTPRouteExecutions(t, "subject", 2, 2, 2)
}

func TestGatewayRouteAnalysisColdMatchesDetachedHEAD(t *testing.T) {
	baselineRoot := os.Getenv(gatewayRouteAnalysisBaselineEnv)
	if baselineRoot == "" {
		t.Skip("run scripts/test-gateway-route-analysis-differential.sh to compare against detached HEAD")
	}

	current := newGatewayRouteAnalysisFixtureWithTemplates(
		t, loadGatewayRouteAnalysisSnippets(t), gatewayFrontendMTLSResolutionRoot+gatewayRouteAnalysisDifferentialRoot)
	legacy := newGatewayRouteAnalysisFixtureWithTemplates(
		t, loadGatewayRouteAnalysisLegacySnippets(t, baselineRoot), gatewayRouteAnalysisDifferentialRoot)
	populateGatewayRouteAnalysisDifferentialFixture(t, current)
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

func populateGatewayRouteAnalysisDifferentialFixture(t *testing.T, fixture *gatewayRouteAnalysisFixture) {
	t.Helper()
	fixture.addGateway(t, gatewayRouteAnalysisGateway())
	fixture.addHTTPRoute(t, gatewayRouteAnalysisRoute("exact", "exact.example.com", "/exact", ""))
	fixture.addHTTPRoute(t, gatewayRouteAnalysisRouteWithPathType(
		"prefix", "prefix.example.com", "PathPrefix", "/prefix", ""))
	fixture.addHTTPRoute(t, gatewayRouteAnalysisRouteWithPathType(
		"regex", "regex.example.com", "RegularExpression", "^/items/[0-9]+$", ""))
	fixture.addHTTPRoute(t, gatewayRouteAnalysisRoute("collision-a", "collision.example.com", "/same", ""))
	fixture.addHTTPRoute(t, gatewayRouteAnalysisRoute("collision-b", "collision.example.com", "/same", "POST"))
	fixture.addHTTPRoute(t, gatewayRouteAnalysisRoute("orphan", "orphan.example.com", "/orphan", ""))
	orphan := gatewayRouteAnalysisRoute("orphan", "orphan.example.com", "/orphan", "")
	orphan["spec"].(map[string]any)["parentRefs"] = []any{gatewayParentRef("Gateway", "absent")}
	fixture.updateHTTPRoute(t, orphan)
}

func newGatewayRouteAnalysisFixture(t *testing.T) *gatewayRouteAnalysisFixture {
	t.Helper()
	return newGatewayRouteAnalysisFixtureWithTemplates(t, loadGatewayRouteAnalysisSnippets(t), gatewayRouteAnalysisRoot)
}

func newGatewayRoutePathDependencyFixture(t *testing.T) *gatewayRouteAnalysisFixture {
	t.Helper()
	loaded := loadGatewayRouteAnalysisSnippets(t)
	snippets := map[string]config.TemplateSnippet{
		"util-host-key":                    loaded["util-host-key"],
		"util-regex-map-key":               loaded["util-regex-map-key"],
		"util-webhook-reject-or-warn":      loaded["util-webhook-reject-or-warn"],
		"util-publish-gateway-route-paths": loaded["util-publish-gateway-route-paths"],
		gatewayRoutePathHTTPComponent:      loaded[gatewayRoutePathHTTPComponent],
		"test-gateway-route-analysis": {
			Name:     "test-gateway-route-analysis",
			Requires: []string{"httproutes"},
			Incremental: &config.IncrementalTemplate{
				Source: "httproutes", Group: "gateway-route-analysis",
				Effects: []config.IncrementalEffect{config.IncrementalEffectPublishValue},
			},
			Template: gatewayRoutePathDependencyProducerTemplate,
		},
	}
	return newGatewayRouteAnalysisFixtureWithTemplates(t, snippets, gatewayRoutePathGetSingleDependencyRoot)
}

func newGatewayRouteAnalysisFixtureWithTemplates(
	t *testing.T,
	snippets map[string]config.TemplateSnippet,
	root string,
) *gatewayRouteAnalysisFixture {
	t.Helper()
	cfg := &config.Config{
		Dataplane: testDataplaneConfig(),
		TemplatingSettings: config.TemplatingSettings{ExtraContext: map[string]any{
			"perGatewayPodPortBase": 18000, "perGatewayPodPortRange": 1000,
			"failAfterRouteAnalysis": false,
		}},
		WatchedResources: map[string]config.WatchedResource{
			"gateways":        {APIVersion: "gateway.networking.k8s.io/v1", Resources: "gateways", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"httproutes":      {APIVersion: "gateway.networking.k8s.io/v1", Resources: "httproutes", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"grpcroutes":      {APIVersion: "gateway.networking.k8s.io/v1", Resources: "grpcroutes", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"listenersets":    {APIVersion: "gateway.networking.k8s.io/v1", Resources: "listenersets", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"namespaces":      {APIVersion: "v1", Resources: "namespaces", IndexBy: []string{"metadata.name"}},
			"referencegrants": {APIVersion: "gateway.networking.k8s.io/v1", Resources: "referencegrants", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"configmaps":      {APIVersion: "v1", Resources: "configmaps", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"secrets":         {APIVersion: "v1", Resources: "secrets", IndexBy: []string{"metadata.namespace", "metadata.name"}},
		},
		TemplateSnippets: snippets,
		HAProxyConfig:    config.HAProxyConfig{Template: root},
	}
	raceScaleRenderTimeout(cfg)
	require.NoError(t, config.ValidateTemplateStructure(cfg))
	types := gatewayRouteAnalysisSchemaTypes(t)
	declarations := helpers.BuildAdditionalDeclarations(cfg, types)
	baseEngine, err := helpers.NewEngineFromConfigWithOptions(cfg, nil, nil, declarations, helpers.EngineOptions{})
	require.NoError(t, err)
	engine := newDynamicBindingCountingEngine(t, baseEngine)
	service := NewRenderService(&RenderServiceConfig{
		Engine: engine, Config: cfg, Logger: slog.Default(), Capabilities: defaultCapabilities(),
		TypedResourceTypes: types.Types,
	})
	fixture := &gatewayRouteAnalysisFixture{
		config: cfg, service: service, engine: engine,
		gateways: k8sstore.NewMemoryStore(2), httpRoutes: k8sstore.NewMemoryStore(2),
		grpcRoutes: k8sstore.NewMemoryStore(2), listenerSets: k8sstore.NewMemoryStore(2),
		namespaces: k8sstore.NewMemoryStore(1), referenceGrants: k8sstore.NewMemoryStore(2),
		configMaps: k8sstore.NewMemoryStore(2), secrets: k8sstore.NewMemoryStore(2),
	}
	fixture.provider = stores.NewRealStoreProvider(map[string]stores.Store{
		"gateways": fixture.gateways, "httproutes": fixture.httpRoutes,
		"grpcroutes": fixture.grpcRoutes, "listenersets": fixture.listenerSets,
		"namespaces": fixture.namespaces, "referencegrants": fixture.referenceGrants,
		"configmaps": fixture.configMaps, "secrets": fixture.secrets,
	})
	return fixture
}

func loadGatewayRouteAnalysisSnippets(t *testing.T) map[string]config.TemplateSnippet {
	t.Helper()
	return loadChartSnippets(t, chartLibrariesRoot(t), map[string][]string{
		"base/library.yaml": {
			"util-host-key", "util-regex-map-key", "util-webhook-reject-or-warn",
		},
		"gateway/10-features.yaml": {
			gatewayFrontendCAPermissionsComponent,
			gatewayFrontendMTLSComponent,
		},
		"gateway/15-pod-port-allocator.yaml": {
			"util-gateway-pod-port-allocation",
			"util-gateway-pod-port-bindings",
			"gateway-pod-port-candidates-100-gateway",
			"gateway-pod-port-allocations-200-leader",
		},
		"gateway/20-route-analysis.yaml": {
			"util-gateway-route-effective-hosts-incremental",
			"util-publish-gateway-route-candidates",
			gatewayRouteCandidateHTTPComponent,
			"gateway-route-candidates-200-grpc",
			"util-analyze-gateway-route-incremental",
			gatewayRouteAnalysisHTTPComponent,
			"gateway-route-analysis-200-grpc",
			"util-listenerset-routing-gate",
		},
		"gateway/21-route-helpers.yaml": {
			"util-resource-helpers", "util-hostname-intersect-gateway", "util-gateway-route-identity",
			"util-reference-grant-permitted", "util-gw-mtls-blocked-value",
		},
		"gateway/40-maps-host.yaml": {
			"util-gateway-host-port-suffix",
			"map-hostvalues-479-gateway-listenersets-empty",
			"map-hostvalues-480-gateway-listenersets",
			"map-hostvalues-490-gateway-port-scopes",
			"gateway-host-port-scopes-100-gateway",
		},
		"gateway/41-maps-path.yaml": {
			"util-publish-gateway-route-paths",
			gatewayRoutePathHTTPComponent,
			"gateway-route-paths-200-grpc",
			"util-gateway-route-path-publications",
			"util-path-map-entry-gateway",
			"map-path-exact-500-gateway",
			"map-pfxexact-500-gateway",
			"map-path-prefix-500-gateway",
			"map-path-regex-500-gateway",
		},
	})
}

func loadGatewayRouteAnalysisLegacySnippets(
	t *testing.T,
	baselineRoot string,
) map[string]config.TemplateSnippet {
	t.Helper()
	return loadChartSnippets(t, baselineRoot, map[string][]string{
		"charts/haptic/charts/base/library.yaml": {
			"util-macros", "util-host-key", "util-webhook-reject-or-warn",
		},
		"charts/haptic/charts/gateway/15-pod-port-allocator.yaml": {
			"util-gateway-pod-port-allocator", "util-gateway-port-scope",
		},
		"charts/haptic/charts/gateway/20-route-analysis.yaml": {
			"util-gateway-analysis", "util-listenersets-contrib", "util-listenerset-candidates",
			"util-listenerset-routing-gate", "util-route-effective-hosts", "util-analyze-routes",
		},
		"charts/haptic/charts/gateway/21-route-helpers.yaml": {
			"util-resource-helpers", "util-hostname-intersect-gateway", "util-gateway-route-identity",
			"util-reference-grant-permitted", "util-gw-mtls-blocked",
		},
		"charts/haptic/charts/gateway/41-maps-path.yaml": {
			"util-path-map-entry-gateway-shard", "util-path-map-entry-gateway",
			"map-path-exact-500-gateway", "map-pfxexact-500-gateway",
			"map-path-prefix-500-gateway", "map-path-regex-500-gateway",
		},
	})
}

func gatewayRouteAnalysisSchemaTypes(t *testing.T) *typebootstrap.Result {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	schemaRoot := filepath.Join(filepath.Dir(sourceFile), "..", "..", "..", "tests", "schemas")
	fetcher, err := schemafetcher.NewDirFetcher(schemaRoot)
	require.NoError(t, err)
	result, err := typebootstrap.Bootstrap(t.Context(), typebootstrap.Config{
		Resources: []typebootstrap.Resource{
			{Name: "gateways", GVK: schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "Gateway"}},
			{Name: "httproutes", GVK: schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"}},
			{Name: "grpcroutes", GVK: schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "GRPCRoute"}},
			{Name: "listenersets", GVK: schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "ListenerSet"}},
			{Name: "namespaces", GVK: schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}},
			{Name: "referencegrants", GVK: schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "ReferenceGrant"}},
			{Name: "configmaps", GVK: schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}},
			{Name: "secrets", GVK: schema.GroupVersionKind{Version: "v1", Kind: "Secret"}},
		},
		Fetcher: fetcher,
		Logger:  slog.Default(),
	})
	require.NoError(t, err)
	require.Empty(t, result.Errors)
	require.Len(t, result.Types, 8)
	return result
}

func gatewayRouteAnalysisGateway() map[string]any {
	const (
		name = "gateway"
		port = int64(80)
	)
	return map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1", "kind": "Gateway",
		"metadata": map[string]any{
			"namespace": "default", "name": name, "creationTimestamp": "2026-01-01T00:00:00Z",
		},
		"spec": map[string]any{
			"gatewayClassName": "haptic",
			"listeners": []any{map[string]any{
				"name": "http", "protocol": "HTTP", "port": port,
			}},
		},
	}
}

func gatewayRouteAnalysisRoute(name, hostname, pathValue, method string) map[string]any {
	return gatewayRouteAnalysisRouteWithPathType(name, hostname, "Exact", pathValue, method)
}

func gatewayRouteAnalysisRouteWithPathType(
	name, hostname, pathType, pathValue, method string,
) map[string]any {
	match := map[string]any{"path": map[string]any{"type": pathType, "value": pathValue}}
	if method != "" {
		match["method"] = method
	}
	return map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1", "kind": "HTTPRoute",
		"metadata": map[string]any{
			"namespace": "default", "name": name, "creationTimestamp": "2026-01-01T00:00:00Z",
		},
		"spec": map[string]any{
			"hostnames": []any{hostname},
			"parentRefs": []any{map[string]any{
				"group": "gateway.networking.k8s.io", "kind": "Gateway", "name": "gateway",
			}},
			"rules": []any{map[string]any{"matches": []any{match}}},
		},
	}
}

func gatewayRouteAnalysisRouteWithHeader(name, value string) map[string]any {
	route := gatewayRouteAnalysisRoute(name, "shared.example.com", "/shared", "")
	match := route["spec"].(map[string]any)["rules"].([]any)[0].(map[string]any)["matches"].([]any)[0].(map[string]any)
	match["headers"] = []any{map[string]any{"name": "x-test", "value": value}}
	return route
}

func (f *gatewayRouteAnalysisFixture) addGateway(t *testing.T, resource map[string]any) {
	t.Helper()
	name := resource["metadata"].(map[string]any)["name"].(string)
	require.NoError(t, f.gateways.Add(resource, []string{"default", name}))
}

func (f *gatewayRouteAnalysisFixture) deleteGateway(t *testing.T, name string) {
	t.Helper()
	require.NoError(t, f.gateways.Delete("default", name, []string{"default", name}))
}

func (f *gatewayRouteAnalysisFixture) addHTTPRoute(t *testing.T, resource map[string]any) {
	t.Helper()
	name := resource["metadata"].(map[string]any)["name"].(string)
	require.NoError(t, f.httpRoutes.Add(resource, []string{"default", name}))
}

func (f *gatewayRouteAnalysisFixture) updateHTTPRoute(t *testing.T, resource map[string]any) {
	t.Helper()
	name := resource["metadata"].(map[string]any)["name"].(string)
	require.NoError(t, f.httpRoutes.Update(resource, []string{"default", name}))
}

func (f *gatewayRouteAnalysisFixture) deleteHTTPRoute(t *testing.T, name string) {
	t.Helper()
	require.NoError(t, f.httpRoutes.Delete("default", name, []string{"default", name}))
}

func (f *gatewayRouteAnalysisFixture) addConfigMap(t *testing.T, name, value string) {
	t.Helper()
	resource := gatewayRouteAnalysisConfigMap(name, value)
	require.NoError(t, f.configMaps.Add(resource, []string{"default", name}))
}

func (f *gatewayRouteAnalysisFixture) updateConfigMap(t *testing.T, name, value string) {
	t.Helper()
	resource := gatewayRouteAnalysisConfigMap(name, value)
	require.NoError(t, f.configMaps.Update(resource, []string{"default", name}))
}

func gatewayRouteAnalysisConfigMap(name, value string) map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"namespace": "default", "name": name},
		"data":     map[string]any{"value": value},
	}
}

func (f *gatewayRouteAnalysisFixture) renderAndCommitCacheReady(t *testing.T) *RenderResult {
	t.Helper()
	result, err := f.service.Render(t.Context(), f.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, result.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, f.service)
	return result
}

func (f *gatewayRouteAnalysisFixture) executions(componentName, routeName string) uint64 {
	component := f.service.incremental.components[componentName]
	query := componentQueryKey(&component, "httproutes", "default", routeName)
	return f.service.incremental.graph.Counters(query).Executions
}

func (f *gatewayRouteAnalysisFixture) assertHTTPRouteExecutions(
	t *testing.T,
	routeName string,
	wantCandidates, wantAnalysis, wantPaths uint64,
) {
	t.Helper()
	assert.Equal(t, wantCandidates, f.executions(gatewayRouteCandidateHTTPComponent, routeName), "candidate/"+routeName)
	assert.Equal(t, wantAnalysis, f.executions(gatewayRouteAnalysisHTTPComponent, routeName), "analysis/"+routeName)
	assert.Equal(t, wantPaths, f.executions(gatewayRoutePathHTTPComponent, routeName), "path/"+routeName)
}
