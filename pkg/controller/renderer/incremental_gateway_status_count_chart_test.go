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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
)

const (
	gatewayRouteAttachmentHTTPComponent = "gateway-route-attachments-100-http"
	gatewayStatusCountTestComponent     = "gateway-status-count-test"
	listenerSetStatusCountTestComponent = "listenerset-status-count-test"
)

const gatewayStatusCountRoot = `{{ render "map-hostvalues-480-gateway-listenersets" default "" -}}
{{ render "gateway-route-attachments-100-http" -}}
{{ render "gateway-route-attachments-200-grpc" -}}
{{ render "gateway-status-count-test" -}}
{{ render "listenerset-status-count-test" -}}
{%- if tostring(extraContext | dig("failAfterHostMap") | fallback(false)) == "true" -%}
{{ fail("forced failure after gateway status counts") }}
{%- end -%}`

const gatewayStatusCountTemplate = `{%%
var gateway = resources.gateways.GetSingle(
  dig_string(item, "", "metadata", "namespace"), dig_string(item, "", "metadata", "name"))
if gateway != nil {
  var namespace = gateway.Metadata.Namespace
  var name = gateway.Metadata.Name
  for _, listener := range gateway.Spec.Listeners {
    show "gateway/" + name + "/" + listener.Name + "=" + tostring(shared.Count(
      "gateway-route-attachments", "gateway-listener\x00" + namespace + "\x00" +
      name + "\x00" + listener.Name)) + "\n"
  }
}
%%}`

const listenerSetStatusCountTemplate = `{%%
var listenerSet = resources.listenersets.GetSingle(
  dig_string(item, "", "metadata", "namespace"), dig_string(item, "", "metadata", "name"))
if listenerSet != nil {
  var namespace = listenerSet.Metadata.Namespace
  var name = listenerSet.Metadata.Name
  for _, listener := range listenerSet.Spec.Listeners {
    show "listenerset/" + name + "/" + listener.Name + "=" + tostring(shared.Count(
      "gateway-route-attachments", "listenerset-listener\x00" + namespace + "\x00" +
      name + "\x00" + listener.Name)) + "\n"
  }
}
%%}`

func TestGatewayStatusAttachedRouteExecutionScaling(t *testing.T) {
	for _, routeCount := range []int{300, 1000, 3000} {
		t.Run(fmt.Sprintf("routes=%d", routeCount), func(t *testing.T) {
			fixture := newGatewayStatusCountFixture(t)
			fixture.addGateway(t, gatewayHostMapGateway(
				"gateway", "2026-01-01T00:00:00Z", "*.example.com", 80))
			for index := range routeCount {
				name := fmt.Sprintf("route-%06d", index)
				fixture.addHTTPRoute(t, gatewayHostMapRoute("HTTPRoute", name,
					[]any{name + ".example.com"}, gatewayParentRef("Gateway", "gateway")))
			}
			fixture.addHTTPRoute(t, gatewayHostMapRoute("HTTPRoute", "unrelated",
				[]any{"unrelated.example.net"}, gatewayParentRef("Gateway", "missing")))

			cold := fixture.renderAndCommitCacheReady(t)
			assert.Equal(t, fmt.Sprintf("gateway/gateway/http=%d\n", routeCount), cold.HAProxyConfig)
			assert.Equal(t, uint64(1), fixture.executions(
				gatewayStatusCountTestComponent, "gateways", "gateway"))
			assert.Equal(t, uint64(1), fixture.executions(
				gatewayRouteAttachmentHTTPComponent, "httproutes", "route-000000"))

			warm := fixture.renderAndCommitCacheReady(t)
			assert.Equal(t, cold.HAProxyConfig, warm.HAProxyConfig)
			assert.Equal(t, uint64(1), fixture.executions(
				gatewayStatusCountTestComponent, "gateways", "gateway"))

			fixture.updateHTTPRoute(t, gatewayHostMapRoute("HTTPRoute", "unrelated",
				[]any{"changed.example.net"}, gatewayParentRef("Gateway", "missing")))
			unrelated := fixture.renderAndCommitCacheReady(t)
			assert.Equal(t, cold.HAProxyConfig, unrelated.HAProxyConfig)
			assert.Equal(t, uint64(1), fixture.executions(
				gatewayStatusCountTestComponent, "gateways", "gateway"))

			fixture.updateHTTPRoute(t, gatewayHostMapRoute("HTTPRoute", "route-000000",
				[]any{"no-intersection.example.net"}, gatewayParentRef("Gateway", "gateway")))
			changed := fixture.renderAndCommitCacheReady(t)
			assert.Equal(t, fmt.Sprintf("gateway/gateway/http=%d\n", routeCount-1), changed.HAProxyConfig)
			assert.Equal(t, uint64(2), fixture.executions(
				gatewayStatusCountTestComponent, "gateways", "gateway"))
			assert.Equal(t, uint64(2), fixture.executions(
				gatewayRouteAttachmentHTTPComponent, "httproutes", "route-000000"))
			assert.Equal(t, uint64(1), fixture.executions(gatewayRouteAttachmentHTTPComponent,
				"httproutes", fmt.Sprintf("route-%06d", routeCount-1)))
		})
	}
}

func TestGatewayStatusAttachedRouteMissingPresentDeletionAndParentMultiplicity(t *testing.T) {
	fixture := newGatewayStatusCountFixture(t)
	route := gatewayHostMapRoute("HTTPRoute", "route", []any{"route.example.com"},
		gatewayParentRef("Gateway", "gateway"), gatewayParentRef("Gateway", "gateway"))
	fixture.addHTTPRoute(t, route)
	assert.Empty(t, strings.TrimSpace(fixture.renderAndCommitCacheReady(t).HAProxyConfig))

	fixture.addGateway(t, gatewayHostMapGateway(
		"gateway", "2026-01-01T00:00:00Z", "*.example.com", 80))
	present := fixture.renderAndCommitCacheReady(t)
	assert.Equal(t, "gateway/gateway/http=2\n", present.HAProxyConfig)
	assert.Equal(t, uint64(2), fixture.executions(
		gatewayRouteAttachmentHTTPComponent, "httproutes", "route"))

	require.NoError(t, fixture.httpRoutes.Delete("default", "route", []string{"default", "route"}))
	deleted := fixture.renderAndCommitCacheReady(t)
	assert.Equal(t, "gateway/gateway/http=0\n", deleted.HAProxyConfig)
	assert.Equal(t, uint64(2), fixture.executions(
		gatewayStatusCountTestComponent, "gateways", "gateway"))

	fixture.deleteGateway(t, "gateway")
	assert.Empty(t, strings.TrimSpace(fixture.renderAndCommitCacheReady(t).HAProxyConfig))
}

func TestGatewayStatusAttachedRouteListenerSetNamespaceRules(t *testing.T) {
	fixture := newGatewayStatusCountFixture(t)
	fixture.addNamespace(t, gatewayHostMapNamespace("default", map[string]any{"team": "edge"}))
	fixture.addGateway(t, gatewayHostMapGateway(
		"gateway", "2026-01-01T00:00:00Z", "gateway.example.com", 80))
	listenerSet := gatewayHostMapListenerSet("listeners", "gateway", "", 8080)
	listenerSet["spec"].(map[string]any)["listeners"] = []any{
		map[string]any{"name": "same", "protocol": "HTTP", "port": int64(8080),
			"allowedRoutes": map[string]any{"namespaces": map[string]any{"from": "Same"}}},
		map[string]any{"name": "all", "protocol": "HTTP", "port": int64(8081),
			"allowedRoutes": map[string]any{"namespaces": map[string]any{"from": "All"}}},
		map[string]any{"name": "selector", "protocol": "HTTP", "port": int64(8082),
			"allowedRoutes": map[string]any{"namespaces": map[string]any{"from": "Selector",
				"selector": map[string]any{"matchLabels": map[string]any{"team": "edge"}}}}},
	}
	fixture.addListenerSet(t, listenerSet)
	fixture.addHTTPRoute(t, gatewayHostMapRoute("HTTPRoute", "all-listeners", nil,
		gatewayParentRef("ListenerSet", "listeners")))
	sectionRef := gatewayParentRef("ListenerSet", "listeners")
	sectionRef["sectionName"] = "all"
	fixture.addHTTPRoute(t, gatewayHostMapRoute("HTTPRoute", "one-listener", nil, sectionRef))

	result := fixture.renderAndCommitCacheReady(t)
	assert.Equal(t, "gateway/gateway/http=0\n"+
		"listenerset/listeners/same=1\n"+
		"listenerset/listeners/all=2\n"+
		"listenerset/listeners/selector=1\n", result.HAProxyConfig)
}

func TestGatewayStatusAttachedRouteColdAdmissionAndAbortStayIsolated(t *testing.T) {
	fixture := newGatewayStatusCountFixture(t)
	fixture.addGateway(t, gatewayHostMapGateway(
		"gateway", "2026-01-01T00:00:00Z", "*.example.com", 80))
	fixture.addHTTPRoute(t, gatewayHostMapRoute("HTTPRoute", "route",
		[]any{"route.example.com"}, gatewayParentRef("Gateway", "gateway")))
	live := fixture.renderAndCommitCacheReady(t)
	assert.Equal(t, "gateway/gateway/http=1\n", live.HAProxyConfig)

	cold, err := renderServiceStaticCold(t, fixture.service, fixture.provider)
	require.NoError(t, err)
	assert.Equal(t, live.HAProxyConfig, cold.HAProxyConfig)
	cold.InputTransaction.Abort()

	proposed := gatewayHostMapRoute("HTTPRoute", "proposed",
		[]any{"proposed.example.com"}, gatewayParentRef("Gateway", "gateway"))
	overlay := stores.NewOverlayStoreProvider(fixture.provider, stores.NewValidationContext(
		map[string]*stores.StoreOverlay{"httproutes": stores.NewStoreOverlayForCreate(
			&unstructured.Unstructured{Object: proposed})},
	))
	admission, err := fixture.service.Render(t.Context(), overlay, rendercontext.RenderModeAdmission,
		rendercontext.WithAdmissionSubject("httproutes", "default", "proposed"))
	require.NoError(t, err)
	assert.Equal(t, "gateway/gateway/http=2\n", admission.HAProxyConfig)
	require.NoError(t, admission.InputTransaction.Commit(t.Context()))
	assert.Equal(t, live.HAProxyConfig, fixture.renderAndCommitCacheReady(t).HAProxyConfig)

	fixture.updateHTTPRoute(t, gatewayHostMapRoute("HTTPRoute", "route",
		[]any{"route.example.net"}, gatewayParentRef("Gateway", "gateway")))
	committed := fixture.service.incremental.snapshot
	fixture.config.TemplatingSettings.ExtraContext["failAfterHostMap"] = true
	failed, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.ErrorContains(t, err, "forced failure after gateway status counts")
	assert.Nil(t, failed)
	assert.Same(t, committed, fixture.service.incremental.snapshot)
	fixture.config.TemplatingSettings.ExtraContext["failAfterHostMap"] = false
	assert.Equal(t, "gateway/gateway/http=0\n", fixture.renderAndCommitCacheReady(t).HAProxyConfig)
}

func BenchmarkGatewayStatusAttachedRouteIncrementalScaling(b *testing.B) {
	for _, routeCount := range []int{300, 1000, 3000} {
		b.Run(fmt.Sprintf("no-change-%d", routeCount), func(b *testing.B) {
			fixture := benchmarkGatewayStatusCountFixture(b, routeCount)
			before := gatewayStatusCountExecutions(fixture)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				fixture.renderAndCommitAuthoritative(b)
			}
			b.StopTimer()
			b.ReportMetric(float64(gatewayStatusCountExecutions(fixture)-before)/float64(b.N),
				"component-executions/op")
		})
		b.Run(fmt.Sprintf("one-route-change-%d", routeCount), func(b *testing.B) {
			fixture := benchmarkGatewayStatusCountFixture(b, routeCount)
			before := gatewayStatusCountExecutions(fixture)
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := range b.N {
				hostname := "route-000000.example.com"
				if iteration&1 == 0 {
					hostname = "route-000000.example.net"
				}
				fixture.updateHTTPRoute(b, gatewayHostMapRoute("HTTPRoute", "route-000000",
					[]any{hostname}, gatewayParentRef("Gateway", "gateway")))
				fixture.renderAndCommitAuthoritative(b)
			}
			b.StopTimer()
			b.ReportMetric(float64(gatewayStatusCountExecutions(fixture)-before)/float64(b.N),
				"component-executions/op")
		})
	}
}

func benchmarkGatewayStatusCountFixture(tb testing.TB, routeCount int) *gatewayHostMapFixture {
	tb.Helper()
	fixture := newGatewayStatusCountFixture(tb)
	fixture.addGateway(tb, gatewayHostMapGateway(
		"gateway", "2026-01-01T00:00:00Z", "*.example.com", 80))
	for index := range routeCount {
		name := fmt.Sprintf("route-%06d", index)
		fixture.addHTTPRoute(tb, gatewayHostMapRoute("HTTPRoute", name,
			[]any{name + ".example.com"}, gatewayParentRef("Gateway", "gateway")))
	}
	fixture.renderAndCommitCacheReady(tb)
	return fixture
}

func gatewayStatusCountExecutions(fixture *gatewayHostMapFixture) uint64 {
	return fixture.executions(gatewayRouteAttachmentHTTPComponent, "httproutes", "route-000000") +
		fixture.executions(gatewayStatusCountTestComponent, "gateways", "gateway")
}

func newGatewayStatusCountFixture(tb testing.TB) *gatewayHostMapFixture {
	tb.Helper()
	snippets := loadChartSnippets(tb, chartLibrariesRoot(tb), map[string][]string{
		"gateway/21-route-helpers.yaml": {"util-hostname-intersect-gateway"},
		"gateway/40-maps-host.yaml":     {"map-hostvalues-480-gateway-listenersets"},
		"gateway/70-status-gateway.yaml": {
			"util-publish-gateway-route-attachments",
			gatewayRouteAttachmentHTTPComponent,
			"gateway-route-attachments-200-grpc",
		},
	})
	snippets[gatewayStatusCountTestComponent] = config.TemplateSnippet{
		Name:     gatewayStatusCountTestComponent,
		Template: gatewayStatusCountTemplate,
		Requires: []string{"gateways"},
		Incremental: &config.IncrementalTemplate{
			Source: "gateways", Group: "gateway-status-count-test",
			Consumes: []string{"gateway-route-attachments"},
		},
	}
	snippets[listenerSetStatusCountTestComponent] = config.TemplateSnippet{
		Name:     listenerSetStatusCountTestComponent,
		Template: listenerSetStatusCountTemplate,
		Requires: []string{"listenersets"},
		Incremental: &config.IncrementalTemplate{
			Source: "listenersets", Group: "listenerset-status-count-test",
			Consumes: []string{"gateway-route-attachments"},
		},
	}
	return newGatewayHostMapFixtureWithTemplates(tb, snippets, gatewayStatusCountRoot)
}
