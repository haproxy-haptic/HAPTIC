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
)

const (
	gatewaySSLPassthroughBackendDependencyRoot = `{{ planRegistry.ProfileGroup() }}
{{ render "backends-501-gateway-ssl-passthrough" }}`
	gatewaySSLPassthroughHTTPDependencyComponent = "gateway-ssl-passthrough-100-http"
	gatewaySSLPassthroughTLSDependencyComponent  = "gateway-ssl-passthrough-200-tls"
	gatewaySSLPassthroughHTTPBackendComponent    = "backenditems-501-gateway-ssl-passthrough-http"
	gatewaySSLPassthroughTLSBackendComponent     = "backenditems-501-gateway-ssl-passthrough-tls"
)

const gatewaySSLPassthroughHTTPDependencyTemplate = `{%%
var namespace = dig_string(item, "", "metadata", "namespace")
var name = dig_string(item, "", "metadata", "name")
var hostnames = toSlice(dig(item, "spec", "hostnames"))
var hasPassthrough = false
for _, rule := range toSlice(dig(item, "spec", "rules")) {
  for _, filter := range toSlice(dig(rule, "filters")) {
    if dig_string(filter, "", "type") != "ExtensionRef" { continue }
    var extensionRef = dig(filter, "extensionRef")
    if dig_string(extensionRef, "", "kind") == "SSLPassthrough" {
      hasPassthrough = true
    }
  }
}
if name != "" && len(hostnames) > 0 && hasPassthrough {
  var hostname = tostring(hostnames[0])
  show shared.Publish("backends", "0/http/" + hostname, map[string]any{
    "name": "ssl-passthrough-default-" + name,
    "sni": hostname,
    "namespace": namespace,
    "route": name,
    "route_type": "httproute",
  })
}
%%}`

const gatewaySSLPassthroughTLSDependencyTemplate = `{%%
var namespace = dig_string(item, "", "metadata", "namespace")
var name = dig_string(item, "", "metadata", "name")
var hostnames = toSlice(dig(item, "spec", "hostnames"))
var fixed = func(value int) string {
  var encoded = tostring(value)
  var zeroes = "0000000000"
  if len(encoded) >= len(zeroes) { return encoded }
  return zeroes[:len(zeroes)-len(encoded)] + encoded
}
for ruleIndex, rule := range toSlice(dig(item, "spec", "rules")) {
  if name == "" || len(hostnames) == 0 { continue }
  var refs = []any{}
  for refIndex := range toSlice(dig(rule, "backendRefs")) {
    refs = append(refs, map[string]any{"index": refIndex, "weight": 1, "valid": true})
  }
  var definitionKey = "1/tls/" + namespace + "/" + name + "/" + fixed(ruleIndex)
  show shared.Publish("definitions", definitionKey, map[string]any{
    "invalid": false,
    "rank": definitionKey + "/" + tostring(hostnames[0]),
    "backends": []any{map[string]any{
      "name": "gtw_tls_" + namespace + "_" + name + "_" + tostring(ruleIndex),
      "terminate": false,
    }},
    "refs": refs,
  })
}
%%}`

type gatewaySSLPassthroughBackendDependencyFixture struct {
	service    *RenderService
	httpRoutes *k8sstore.MemoryStore
	tlsRoutes  *k8sstore.MemoryStore
	services   *k8sstore.MemoryStore
	endpoints  *k8sstore.MemoryStore
	provider   stores.StoreProvider
}

func TestGatewaySSLPassthroughBackendRetainsExactComponentDependencies(t *testing.T) {
	fixture := newGatewaySSLPassthroughBackendDependencyFixture(t)
	seedGatewaySSLPassthroughDependencyFixture(t, fixture)

	first := fixture.renderAndCommit(t)
	assert.Contains(t, first.HAProxyConfig, "10.0.0.1:8080")
	assert.NotContains(t, first.HAProxyConfig, "10.0.0.2:8080")
	producer := fixture.service.incremental.components[gatewaySSLPassthroughHTTPDependencyComponent]
	producerQuery := componentQueryKey(&producer, "httproutes", "default", "subject")
	consumer := fixture.service.incremental.components[gatewaySSLPassthroughHTTPBackendComponent]
	consumerQuery := componentQueryKey(&consumer, "httproutes", "default", "subject")
	beforeProducer, found := fixture.service.incremental.graph.Value(producerQuery)
	require.True(t, found)
	assert.Equal(t, uint64(1), fixture.service.incremental.graph.Counters(consumerQuery).Executions)
	warm := fixture.renderAndCommit(t)
	assertRenderResultObservablesEqual(t, first, warm)
	assert.Equal(t, uint64(1), fixture.service.incremental.graph.Counters(consumerQuery).Executions)

	requireSSLPassthroughUnrelatedServiceIsInert(t, fixture, first, consumerQuery)
	requireSSLPassthroughHostnameWinnerTakeover(t, fixture, first, consumerQuery)
	second := requireSSLPassthroughBackendSwitch(t, fixture, producerQuery, consumerQuery, beforeProducer)
	requireSSLPassthroughEndpointTracking(t, fixture, second, consumerQuery)
}

func seedGatewaySSLPassthroughDependencyFixture(
	t *testing.T,
	fixture *gatewaySSLPassthroughBackendDependencyFixture,
) {
	t.Helper()
	require.NoError(t, fixture.services.Add(sslPassthroughService("echo", "http", 80), []string{"default", "echo"}))
	require.NoError(t, fixture.services.Add(sslPassthroughService("other", "http", 80), []string{"default", "other"}))
	require.NoError(t, fixture.endpoints.Add(
		sslPassthroughEndpoint("echo", "http", 8080, "10.0.0.1"), []string{"default", "echo"},
	))
	require.NoError(t, fixture.endpoints.Add(
		sslPassthroughEndpoint("other", "http", 8080, "10.0.0.2"), []string{"default", "other"},
	))
	subject := gatewaySSLPassthroughHTTPBackendRoute("subject", "echo", "SSLPassthrough")
	subject["spec"].(map[string]any)["hostnames"] = []any{"subject.example.com"}
	require.NoError(t, fixture.httpRoutes.Add(subject, []string{"default", "subject"}))
}

func requireSSLPassthroughUnrelatedServiceIsInert(
	t *testing.T,
	fixture *gatewaySSLPassthroughBackendDependencyFixture,
	first *RenderResult,
	consumerQuery incremental.QueryKey,
) {
	t.Helper()
	require.NoError(t, fixture.services.Add(sslPassthroughService("unused", "http", 80), []string{"default", "unused"}))
	require.NoError(t, fixture.endpoints.Add(
		sslPassthroughEndpoint("unused", "http", 8080, "10.0.0.9"), []string{"default", "unused"},
	))
	unrelated := fixture.renderAndCommit(t)
	assert.Equal(t, first.HAProxyConfig, unrelated.HAProxyConfig)
	assert.Equal(t, uint64(1), fixture.service.incremental.graph.Counters(consumerQuery).Executions)
}

func requireSSLPassthroughHostnameWinnerTakeover(
	t *testing.T,
	fixture *gatewaySSLPassthroughBackendDependencyFixture,
	first *RenderResult,
	consumerQuery incremental.QueryKey,
) {
	t.Helper()
	winner := gatewaySSLPassthroughHTTPBackendRoute("a-winner", "other", "SSLPassthrough")
	winner["spec"].(map[string]any)["hostnames"] = []any{"subject.example.com"}
	require.NoError(t, fixture.httpRoutes.Add(winner, []string{"default", "a-winner"}))
	winnerResult := fixture.renderAndCommit(t)
	assert.Contains(t, winnerResult.HAProxyConfig, "backend ssl-passthrough-default-a-winner")
	assert.Contains(t, winnerResult.HAProxyConfig, "10.0.0.2:8080")
	assert.NotContains(t, winnerResult.HAProxyConfig, "backend ssl-passthrough-default-subject")
	assert.Equal(t, uint64(2), fixture.service.incremental.graph.Counters(consumerQuery).Executions)

	require.NoError(t, fixture.httpRoutes.Delete(
		"default", "a-winner", []string{"default", "a-winner"},
	))
	promoted := fixture.renderAndCommit(t)
	assert.Equal(t, first.HAProxyConfig, promoted.HAProxyConfig)
	assert.Equal(t, uint64(3), fixture.service.incremental.graph.Counters(consumerQuery).Executions)
}

func requireSSLPassthroughBackendSwitch(
	t *testing.T,
	fixture *gatewaySSLPassthroughBackendDependencyFixture,
	producerQuery, consumerQuery incremental.QueryKey,
	beforeProducer []byte,
) *RenderResult {
	t.Helper()
	subject := gatewaySSLPassthroughHTTPBackendRoute("subject", "other", "SSLPassthrough")
	subject["spec"].(map[string]any)["hostnames"] = []any{"subject.example.com"}
	require.NoError(t, fixture.httpRoutes.Update(subject, []string{"default", "subject"}))
	second := fixture.renderAndCommit(t)
	assert.Contains(t, second.HAProxyConfig, "10.0.0.2:8080")
	assert.NotContains(t, second.HAProxyConfig, "10.0.0.1:8080")
	afterProducer, found := fixture.service.incremental.graph.Value(producerQuery)
	require.True(t, found)
	assert.Equal(t, beforeProducer, afterProducer, "sparse SSL publication must stay byte-identical")
	assert.Equal(t, uint64(4), fixture.service.incremental.graph.Counters(consumerQuery).Executions)
	return second
}

func requireSSLPassthroughEndpointTracking(
	t *testing.T,
	fixture *gatewaySSLPassthroughBackendDependencyFixture,
	second *RenderResult,
	consumerQuery incremental.QueryKey,
) {
	t.Helper()
	metadataOnlyService := sslPassthroughService("other", "http", 80)
	metadataOnlyService["metadata"].(map[string]any)["labels"] = map[string]any{"revision": "metadata-only"}
	require.NoError(t, fixture.services.Update(metadataOnlyService, []string{"default", "other"}))
	metadataOnly := fixture.renderAndCommit(t)
	assert.Equal(t, second.HAProxyConfig, metadataOnly.HAProxyConfig)
	assert.Equal(t, uint64(5), fixture.service.incremental.graph.Counters(consumerQuery).Executions)

	require.NoError(t, fixture.endpoints.Update(
		sslPassthroughEndpoint("other", "http", 8080, "10.0.0.3"), []string{"default", "other"},
	))
	endpointChanged := fixture.renderAndCommit(t)
	assert.Contains(t, endpointChanged.HAProxyConfig, "10.0.0.3:8080")
	assert.NotContains(t, endpointChanged.HAProxyConfig, "10.0.0.2:8080")
	assert.Equal(t, uint64(6), fixture.service.incremental.graph.Counters(consumerQuery).Executions)

	pending, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	subject := gatewaySSLPassthroughHTTPBackendRoute("subject", "echo", "SSLPassthrough")
	subject["spec"].(map[string]any)["hostnames"] = []any{"subject.example.com"}
	require.NoError(t, fixture.httpRoutes.Update(subject, []string{"default", "subject"}))
	require.NoError(t, pending.InputTransaction.Commit(t.Context()))
	fixture.renderAndCommit(t)
	assert.Equal(t, uint64(7), fixture.service.incremental.graph.Counters(consumerQuery).Executions)
}

func TestGatewaySSLPassthroughHTTPBackendActivationTracksFilters(t *testing.T) {
	fixture := newGatewaySSLPassthroughBackendDependencyFixture(t)
	require.NoError(t, fixture.services.Add(
		sslPassthroughService("echo", "http", 80), []string{"default", "echo"},
	))
	require.NoError(t, fixture.endpoints.Add(
		sslPassthroughEndpoint("echo", "http", 8080, "10.0.0.1"), []string{"default", "echo"},
	))

	route := gatewaySSLPassthroughHTTPBackendRoute("subject", "echo", "")
	require.NoError(t, fixture.httpRoutes.Add(route, []string{"default", "subject"}))
	consumer := fixture.service.incremental.components[gatewaySSLPassthroughHTTPBackendComponent]
	query := componentQueryKey(&consumer, "httproutes", "default", "subject")
	resultCacheKey := resultKey(&consumer, "httproutes", "default", "subject")

	inactive := fixture.renderAndCommit(t)
	assert.NotContains(t, inactive.HAProxyConfig, "ssl-passthrough-default-subject")
	assert.Zero(t, fixture.service.incremental.graph.Counters(query).Executions)
	_, cached := fixture.service.incremental.snapshot.results.Get(resultCacheKey)
	assert.False(t, cached)
	assertRenderResultObservablesEqual(t, inactive, fixture.renderAndCommit(t))
	assert.Zero(t, fixture.service.incremental.graph.Counters(query).Executions)

	route = gatewaySSLPassthroughHTTPBackendRoute("subject", "echo", "Unrelated")
	require.NoError(t, fixture.httpRoutes.Update(route, []string{"default", "subject"}))
	unrelated := fixture.renderAndCommit(t)
	assert.NotContains(t, unrelated.HAProxyConfig, "ssl-passthrough-default-subject")
	assert.Equal(t, uint64(1), fixture.service.incremental.graph.Counters(query).Executions)

	route = gatewaySSLPassthroughHTTPBackendRoute("subject", "echo", "SSLPassthrough")
	require.NoError(t, fixture.httpRoutes.Update(route, []string{"default", "subject"}))
	active := fixture.renderAndCommit(t)
	assert.Contains(t, active.HAProxyConfig, "backend ssl-passthrough-default-subject")
	assert.Contains(t, active.HAProxyConfig, "10.0.0.1:8080")
	assert.Equal(t, uint64(2), fixture.service.incremental.graph.Counters(query).Executions)
	assertRenderResultObservablesEqual(t, active, fixture.renderAndCommit(t))
	assert.Equal(t, uint64(2), fixture.service.incremental.graph.Counters(query).Executions)

	route = gatewaySSLPassthroughHTTPBackendRoute("subject", "echo", "Unrelated")
	require.NoError(t, fixture.httpRoutes.Update(route, []string{"default", "subject"}))
	activeEmpty := fixture.renderAndCommit(t)
	assert.NotContains(t, activeEmpty.HAProxyConfig, "ssl-passthrough-default-subject")
	assert.Equal(t, uint64(3), fixture.service.incremental.graph.Counters(query).Executions)
	_, cached = fixture.service.incremental.snapshot.results.Get(resultCacheKey)
	assert.True(t, cached)

	route = gatewaySSLPassthroughHTTPBackendRoute("subject", "echo", "")
	require.NoError(t, fixture.httpRoutes.Update(route, []string{"default", "subject"}))
	removed := fixture.renderAndCommit(t)
	assert.NotContains(t, removed.HAProxyConfig, "ssl-passthrough-default-subject")
	assert.Zero(t, fixture.service.incremental.graph.Counters(query).Executions)
	_, cached = fixture.service.incremental.snapshot.results.Get(resultCacheKey)
	assert.False(t, cached)

	route = gatewaySSLPassthroughHTTPBackendRoute("subject", "echo", "SSLPassthrough")
	require.NoError(t, fixture.httpRoutes.Update(route, []string{"default", "subject"}))
	recreated := fixture.renderAndCommit(t)
	assert.Equal(t, active.HAProxyConfig, recreated.HAProxyConfig)
	assert.Equal(t, uint64(1), fixture.service.incremental.graph.Counters(query).Executions)
}

func TestGatewaySSLPassthroughTLSBackendTracksExactRouteAndEndpoints(t *testing.T) {
	fixture := newGatewaySSLPassthroughBackendDependencyFixture(t)
	require.NoError(t, fixture.services.Add(sslPassthroughService("echo", "tls", 8443), []string{"default", "echo"}))
	require.NoError(t, fixture.services.Add(sslPassthroughService("other", "tls", 8443), []string{"default", "other"}))
	require.NoError(t, fixture.endpoints.Add(
		sslPassthroughEndpoint("echo", "tls", 9443, "10.0.1.1"), []string{"default", "echo"},
	))
	require.NoError(t, fixture.endpoints.Add(
		sslPassthroughEndpoint("other", "tls", 9443, "10.0.1.2"), []string{"default", "other"},
	))
	route := gatewaySSLPassthroughTLSRoute("subject")
	require.NoError(t, fixture.tlsRoutes.Add(route, []string{"default", "subject"}))

	cold := fixture.renderAndCommit(t)
	assert.Contains(t, cold.HAProxyConfig, "backend gtw_tls_default_subject_0")
	assert.Contains(t, cold.HAProxyConfig, "10.0.1.1:9443")
	consumer := fixture.service.incremental.components[gatewaySSLPassthroughTLSBackendComponent]
	query := componentQueryKey(&consumer, "tlsroutes", "default", "subject")
	assert.Equal(t, uint64(1), fixture.service.incremental.graph.Counters(query).Executions)

	warm := fixture.renderAndCommit(t)
	assertRenderResultObservablesEqual(t, cold, warm)
	assert.Equal(t, uint64(1), fixture.service.incremental.graph.Counters(query).Executions)

	require.NoError(t, fixture.endpoints.Add(
		sslPassthroughEndpoint("unused", "tls", 9443, "10.0.1.9"), []string{"default", "unused"},
	))
	unrelated := fixture.renderAndCommit(t)
	assert.Equal(t, cold.HAProxyConfig, unrelated.HAProxyConfig)
	assert.Equal(t, uint64(1), fixture.service.incremental.graph.Counters(query).Executions)

	changed := gatewaySSLPassthroughTLSRoute("subject")
	changed["spec"].(map[string]any)["rules"].([]any)[0].(map[string]any)["backendRefs"] = []any{
		map[string]any{"name": "other", "port": int64(8443)},
	}
	require.NoError(t, fixture.tlsRoutes.Update(changed, []string{"default", "subject"}))
	updated := fixture.renderAndCommit(t)
	assert.Contains(t, updated.HAProxyConfig, "10.0.1.2:9443")
	assert.NotContains(t, updated.HAProxyConfig, "10.0.1.1:9443")
	assert.Equal(t, uint64(2), fixture.service.incremental.graph.Counters(query).Executions)

	require.NoError(t, fixture.tlsRoutes.Delete(
		"default", "subject", []string{"default", "subject"},
	))
	deleted := fixture.renderAndCommit(t)
	assert.NotContains(t, deleted.HAProxyConfig, "backend gtw_tls_default_subject_0")

	require.NoError(t, fixture.tlsRoutes.Add(changed, []string{"default", "subject"}))
	recreated := fixture.renderAndCommit(t)
	assert.Equal(t, updated.HAProxyConfig, recreated.HAProxyConfig)
}

func newGatewaySSLPassthroughBackendDependencyFixture(
	t *testing.T,
) *gatewaySSLPassthroughBackendDependencyFixture {
	t.Helper()
	snippets := loadGatewayHostMapSnippets(t, gatewayHostMapChartRoot(t), map[string][]string{
		"base/library.yaml": {
			"util-backend-servers-helpers", "util-backend",
			"util-webhook-reject-or-warn",
		},
		"kubernetes-backends/library.yaml": {
			"util-backend-servers-result", "util-gated-pod-routable", "util-backend-servers",
		},
		"gateway/22-ssl-passthrough.yaml": {"util-gateway-http-ssl-passthrough"},
		"gateway/30-backends.yaml": {
			gatewaySSLPassthroughHTTPBackendComponent,
			"backenditems-501-gateway-ssl-passthrough-tls",
			"backends-501-gateway-ssl-passthrough",
			"util-resolve-backend-tls", "util-backend-tls-identity", "util-gateway-backend-bindings",
			"util-gateway-tls-backend-bindings", "util-replay-gateway-backend-effects",
			"backendtlsvalues-490-gateway",
		},
	})
	snippets[gatewaySSLPassthroughHTTPDependencyComponent] = config.TemplateSnippet{
		Name:     gatewaySSLPassthroughHTTPDependencyComponent,
		Requires: []string{"httproutes"},
		Incremental: &config.IncrementalTemplate{
			Source: "httproutes", Root: "gateway-http-route-pre-analysis",
			Group:   "gateway-ssl-passthrough",
			Effects: []config.IncrementalEffect{config.IncrementalEffectPublishValue},
		},
		Template: gatewaySSLPassthroughHTTPDependencyTemplate,
	}
	snippets[gatewaySSLPassthroughTLSDependencyComponent] = config.TemplateSnippet{
		Name:     gatewaySSLPassthroughTLSDependencyComponent,
		Requires: []string{"tlsroutes"},
		Incremental: &config.IncrementalTemplate{
			Source: "tlsroutes", Group: "gateway-ssl-passthrough",
			Effects: []config.IncrementalEffect{config.IncrementalEffectPublishValue},
		},
		Template: gatewaySSLPassthroughTLSDependencyTemplate,
	}
	cfg := &config.Config{
		Dataplane: testDataplaneConfig(),
		WatchedResources: map[string]config.WatchedResource{
			"gateways":           {APIVersion: "gateway.networking.k8s.io/v1", Resources: "gateways", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"httproutes":         {APIVersion: "gateway.networking.k8s.io/v1", Resources: "httproutes", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"tlsroutes":          {APIVersion: "gateway.networking.k8s.io/v1", Resources: "tlsroutes", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"namespaces":         {APIVersion: "v1", Resources: "namespaces", IndexBy: []string{"metadata.name"}},
			"referencegrants":    {APIVersion: "gateway.networking.k8s.io/v1", Resources: "referencegrants", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"services":           {APIVersion: "v1", Resources: "services", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"endpoints":          {APIVersion: "discovery.k8s.io/v1", Resources: "endpointslices", IndexBy: []string{"metadata.namespace", "metadata.labels.kubernetes\\.io/service-name"}},
			"configmaps":         {APIVersion: "v1", Resources: "configmaps", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"secrets":            {APIVersion: "v1", Resources: "secrets", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"backendtlspolicies": {APIVersion: "gateway.networking.k8s.io/v1", Resources: "backendtlspolicies", IndexBy: []string{"metadata.namespace", "metadata.name"}},
		},
		TemplateSnippets: snippets,
		HAProxyConfig: config.HAProxyConfig{
			Template: gatewaySSLPassthroughBackendDependencyRoot,
		},
	}
	require.NoError(t, config.ValidateTemplateStructure(cfg))
	types := gatewaySSLPassthroughBackendDependencyTypes(t)
	declarations := helpers.BuildAdditionalDeclarations(cfg, types)
	engine, err := helpers.NewEngineFromConfigWithOptions(cfg, nil, nil, declarations, helpers.EngineOptions{})
	require.NoError(t, err)
	service := NewRenderService(&RenderServiceConfig{
		Engine: engine, Config: cfg, Logger: slog.Default(), Capabilities: defaultCapabilities(),
		TypedResourceTypes: types.Types,
	})
	fixture := &gatewaySSLPassthroughBackendDependencyFixture{
		service: service, httpRoutes: k8sstore.NewMemoryStore(2),
		tlsRoutes: k8sstore.NewMemoryStore(2), services: k8sstore.NewMemoryStore(2),
		endpoints: k8sstore.NewMemoryStore(2),
	}
	fixture.provider = stores.NewRealStoreProvider(map[string]stores.Store{
		"gateways": k8sstore.NewMemoryStore(2), "httproutes": fixture.httpRoutes,
		"tlsroutes": fixture.tlsRoutes, "namespaces": k8sstore.NewMemoryStore(1),
		"referencegrants": k8sstore.NewMemoryStore(2), "services": fixture.services,
		"endpoints": fixture.endpoints, "configmaps": k8sstore.NewMemoryStore(2),
		"secrets": k8sstore.NewMemoryStore(2), "backendtlspolicies": k8sstore.NewMemoryStore(2),
	})
	return fixture
}

func gatewaySSLPassthroughBackendDependencyTypes(t *testing.T) *typebootstrap.Result {
	t.Helper()
	types := gatewaySSLPassthroughPublicationSchemaTypes(t)
	backendTypes := gatewayBackendSchemaTypes(t)
	types.Types["endpoints"] = backendTypes.Types["endpoints"]
	types.Kinds["endpoints"] = backendTypes.Kinds["endpoints"]
	for _, name := range []string{"configmaps", "secrets", "backendtlspolicies"} {
		types.Types[name] = backendTypes.Types[name]
		types.Kinds[name] = backendTypes.Kinds[name]
	}
	return types
}

func (f *gatewaySSLPassthroughBackendDependencyFixture) renderAndCommit(t *testing.T) *RenderResult {
	t.Helper()
	result, err := f.service.Render(t.Context(), f.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, result.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, f.service)
	return result
}

func gatewaySSLPassthroughHTTPBackendRoute(name, service, filterKind string) map[string]any {
	route := gatewayBackendRoute("HTTPRoute", name, service)
	route["spec"].(map[string]any)["hostnames"] = []any{"subject.example.com"}
	if filterKind == "" {
		return route
	}
	rules := route["spec"].(map[string]any)["rules"].([]any)
	route["spec"].(map[string]any)["rules"] = append(rules, map[string]any{
		"filters": []any{map[string]any{
			"type": "ExtensionRef",
			"extensionRef": map[string]any{
				"group": "example.test", "kind": filterKind, "name": "filter",
			},
		}},
	})
	return route
}
