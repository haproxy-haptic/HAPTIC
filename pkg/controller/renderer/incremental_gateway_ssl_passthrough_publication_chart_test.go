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
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/helpers"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/typebootstrap"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/schemafetcher"
	k8sstore "github.com/haproxy-haptic/HAPTIC/pkg/k8s/store"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
)

const gatewaySSLPassthroughPublicationRoot = `{{ render_glob "gateway-ssl-passthrough-*" -}}
{%- for _, backend := range incremental_values("gateway-ssl-passthrough", "backends") %}
{%- var value = backend.(map[string]any) %}
{{ tostring(value["sni"]) }}={{ tostring(value["name"]) }}
{%- end %}
{%- if extraContext | dig("fail") | fallback(false) %}{{ fail("forced Gateway SSL passthrough failure") }}{%- end -%}`

type gatewaySSLPassthroughPublicationFixture struct {
	config          *config.Config
	service         *RenderService
	engine          *dynamicBindingCountingEngine
	gateways        *k8sstore.MemoryStore
	httpRoutes      *k8sstore.MemoryStore
	tlsRoutes       *k8sstore.MemoryStore
	services        *k8sstore.MemoryStore
	namespaces      *k8sstore.MemoryStore
	referenceGrants *k8sstore.MemoryStore
	provider        stores.StoreProvider
}

func TestGatewayHTTPSSLPassthroughSurvivesUnavailableTLSRouteSchema(t *testing.T) {
	cfg := gatewaySSLPassthroughPublicationConfig(t)
	effective, resolution, err := config.ResolveEffective(cfg, gatewayRootServedResources{
		"gateways":        true,
		"httproutes":      true,
		"namespaces":      true,
		"referencegrants": true,
		"services":        true,
	}, nil)
	require.NoError(t, err)
	assert.Contains(t, effective.TemplateSnippets, "gateway-ssl-passthrough-100-http")
	assert.NotContains(t, effective.TemplateSnippets, "gateway-ssl-passthrough-200-tls")
	assert.Empty(t, resolution.AbsentIncrementalGroups)
	require.NoError(t, config.ValidateTemplateStructure(effective))

	fixture := newGatewaySSLPassthroughPublicationFixtureWithConfig(t, effective)
	fixture.addHTTPRoute(t, gatewaySSLPassthroughHTTPRoute("http-only", "http-only.example"))
	cold := fixture.renderAndCommit(t)
	assert.Contains(t, cold, "http-only.example=ssl-passthrough-default-http-only")
	assert.Equal(t, cold, fixture.renderAndCommit(t))
}

func TestGatewaySSLPassthroughPublicationsScaleByChangedRoute(t *testing.T) {
	for _, routeCount := range []int{300, 1000, 3000} {
		t.Run(fmt.Sprintf("routes=%d", routeCount), func(t *testing.T) {
			fixture := newGatewaySSLPassthroughPublicationFixture(t)
			for index := range routeCount {
				fixture.addHTTPRoute(t, gatewaySSLPassthroughHTTPRoute(
					fmt.Sprintf("route-%06d", index), fmt.Sprintf("route-%06d.example", index),
				))
			}

			cold := fixture.renderAndCommit(t)
			assert.Contains(t, cold, "route-000000.example=ssl-passthrough-default-route-000000")
			assert.Contains(t, cold, fmt.Sprintf(
				"route-%06d.example=ssl-passthrough-default-route-%06d", routeCount-1, routeCount-1,
			))
			coldCounts := fixture.engine.executionCounts()
			require.Len(t, coldCounts, routeCount)

			assert.Equal(t, cold, fixture.renderAndCommit(t))
			assert.Equal(t, coldCounts, fixture.engine.executionCounts())

			name := fmt.Sprintf("route-%06d", routeCount-1)
			changed := gatewaySSLPassthroughHTTPRoute(name, name+".example")
			changed["metadata"].(map[string]any)["labels"] = map[string]any{"changed": "true"}
			fixture.updateHTTPRoute(t, changed)
			assert.Equal(t, cold, fixture.renderAndCommit(t))
			counts := fixture.engine.executionCounts()
			assert.Equal(t, coldCounts["httproutes/"+name]+1, counts["httproutes/"+name])
			assert.Equal(t, coldCounts["httproutes/route-000000"], counts["httproutes/route-000000"])
		})
	}
}

func TestGatewaySSLPassthroughPromotesCachedHTTPWinnerAndTracksTLSGateway(t *testing.T) {
	fixture := newGatewaySSLPassthroughPublicationFixture(t)
	fixture.addHTTPRoute(t, gatewaySSLPassthroughHTTPRoute("a-winner", "shared.example"))
	fixture.addHTTPRoute(t, gatewaySSLPassthroughHTTPRoute("z-loser", "shared.example"))
	fixture.addService(t)
	fixture.addTLSRoute(t, gatewaySSLPassthroughTLSRoute("tls"))

	withoutGateway := fixture.renderAndCommit(t)
	assert.Contains(t, withoutGateway, "shared.example=ssl-passthrough-default-a-winner")
	assert.NotContains(t, withoutGateway, "tls.example")
	loserExecutions := fixture.engine.executionCounts()["httproutes/z-loser"]
	tlsExecutions := fixture.engine.executionCounts()["tlsroutes/tls"]

	fixture.addGateway(t)
	withGateway := fixture.renderAndCommit(t)
	assert.Contains(t, withGateway, "tls.example=gtw_tls_default_tls_0")
	assert.Equal(t, tlsExecutions+1, fixture.engine.executionCounts()["tlsroutes/tls"])
	assert.Equal(t, loserExecutions, fixture.engine.executionCounts()["httproutes/z-loser"])

	require.NoError(t, fixture.httpRoutes.Delete(
		"default", "a-winner", []string{"default", "a-winner"},
	))
	fixture.config.TemplatingSettings.ExtraContext["fail"] = true
	failed, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.ErrorContains(t, err, "forced Gateway SSL passthrough failure")
	assert.Nil(t, failed)
	assert.Equal(t, loserExecutions, fixture.engine.executionCounts()["httproutes/z-loser"])

	fixture.config.TemplatingSettings.ExtraContext["fail"] = false
	promoted := fixture.renderAndCommit(t)
	assert.Contains(t, promoted, "shared.example=ssl-passthrough-default-z-loser")
	assert.NotContains(t, promoted, "ssl-passthrough-default-a-winner")
	assert.Equal(t, loserExecutions, fixture.engine.executionCounts()["httproutes/z-loser"])

	oracle := newGatewaySSLPassthroughPublicationFixture(t)
	oracle.provider = fixture.provider
	assert.Equal(t, promoted, oracle.renderAndCommit(t))
}

func newGatewaySSLPassthroughPublicationFixture(t *testing.T) *gatewaySSLPassthroughPublicationFixture {
	t.Helper()
	return newGatewaySSLPassthroughPublicationFixtureWithConfig(
		t,
		gatewaySSLPassthroughPublicationConfig(t),
	)
}

func gatewaySSLPassthroughPublicationConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		Dataplane: testDataplaneConfig(),
		WatchedResources: map[string]config.WatchedResource{
			"gateways":        {APIVersion: "gateway.networking.k8s.io/v1", Resources: "gateways", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"httproutes":      {APIVersion: "gateway.networking.k8s.io/v1", Resources: "httproutes", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"tlsroutes":       {APIVersion: "gateway.networking.k8s.io/v1", Resources: "tlsroutes", Optional: true, IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"services":        {APIVersion: "v1", Resources: "services", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"namespaces":      {APIVersion: "v1", Resources: "namespaces", IndexBy: []string{"metadata.name"}},
			"referencegrants": {APIVersion: "gateway.networking.k8s.io/v1", Resources: "referencegrants", IndexBy: []string{"metadata.namespace", "metadata.name"}},
		},
		TemplateSnippets: loadGatewaySSLPassthroughPublicationSnippets(t),
		HAProxyConfig:    config.HAProxyConfig{Template: gatewaySSLPassthroughPublicationRoot},
		TemplatingSettings: config.TemplatingSettings{
			ExtraContext: map[string]any{"fail": false},
		},
	}
}

func newGatewaySSLPassthroughPublicationFixtureWithConfig(
	t *testing.T,
	cfg *config.Config,
) *gatewaySSLPassthroughPublicationFixture {
	t.Helper()
	require.NoError(t, config.ValidateTemplateStructure(cfg))
	types := gatewaySSLPassthroughPublicationSchemaTypes(t)
	declarations := helpers.BuildAdditionalDeclarations(cfg, types)
	baseEngine, err := helpers.NewEngineFromConfigWithOptions(cfg, nil, nil, declarations, helpers.EngineOptions{})
	require.NoError(t, err)
	engine := newDynamicBindingCountingEngine(t, baseEngine)
	service := NewRenderService(&RenderServiceConfig{
		Engine: engine, Config: cfg, Logger: slog.Default(), Capabilities: defaultCapabilities(),
		TypedResourceTypes: types.Types,
	})
	fixture := &gatewaySSLPassthroughPublicationFixture{
		config: cfg, service: service, engine: engine,
		gateways: k8sstore.NewMemoryStore(2), httpRoutes: k8sstore.NewMemoryStore(2),
		tlsRoutes: k8sstore.NewMemoryStore(2), services: k8sstore.NewMemoryStore(2),
		namespaces: k8sstore.NewMemoryStore(1), referenceGrants: k8sstore.NewMemoryStore(2),
	}
	fixture.provider = stores.NewRealStoreProvider(map[string]stores.Store{
		"gateways": fixture.gateways, "httproutes": fixture.httpRoutes, "tlsroutes": fixture.tlsRoutes,
		"services": fixture.services, "namespaces": fixture.namespaces,
		"referencegrants": fixture.referenceGrants,
	})
	return fixture
}

func loadGatewaySSLPassthroughPublicationSnippets(t *testing.T) map[string]config.TemplateSnippet {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	chartRoot := filepath.Join(filepath.Dir(sourceFile), "..", "..", "..", "charts", "haptic", "charts", "gateway")
	wanted := map[string]bool{
		"util-resource-helpers": true, "util-reference-grant-permitted": true,
		"util-hostname-intersect-gateway": true, "util-publish-gateway-http-ssl-passthrough": true,
		"util-publish-gateway-tls-ssl-passthrough": true,
		"util-gateway-http-ssl-passthrough":        true,
		"gateway-ssl-passthrough-100-http":         true, "gateway-ssl-passthrough-200-tls": true,
	}
	result := make(map[string]config.TemplateSnippet, len(wanted))
	for _, filename := range []string{"21-route-helpers.yaml", "22-ssl-passthrough.yaml"} {
		content, err := os.ReadFile(filepath.Join(chartRoot, filename))
		require.NoError(t, err)
		var library ingressBackendChartLibrary
		require.NoError(t, yaml.Unmarshal(content, &library))
		for name, chartSnippet := range library.TemplateSnippets {
			if !wanted[name] {
				continue
			}
			snippet := config.TemplateSnippet{Name: name, Template: chartSnippet.Template, Requires: chartSnippet.Requires}
			if chartSnippet.Incremental != nil {
				snippet.Incremental = &config.IncrementalTemplate{
					Source: chartSnippet.Incremental.Source, Root: chartSnippet.Incremental.Root,
					Group:   chartSnippet.Incremental.Group,
					Effects: chartSnippet.Incremental.Effects,
				}
			}
			result[name] = snippet
		}
	}
	require.Len(t, result, len(wanted))
	return result
}

func gatewaySSLPassthroughPublicationSchemaTypes(t *testing.T) *typebootstrap.Result {
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
			{Name: "tlsroutes", GVK: schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "TLSRoute"}},
			{Name: "services", GVK: schema.GroupVersionKind{Version: "v1", Kind: "Service"}},
			{Name: "namespaces", GVK: schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}},
			{Name: "referencegrants", GVK: schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "ReferenceGrant"}},
		},
		Fetcher: fetcher,
		Logger:  slog.Default(),
	})
	require.NoError(t, err)
	require.Empty(t, result.Errors)
	require.Len(t, result.Types, 6)
	return result
}

func gatewaySSLPassthroughHTTPRoute(name, hostname string) map[string]any {
	return map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1", "kind": "HTTPRoute",
		"metadata": map[string]any{"namespace": "default", "name": name},
		"spec": map[string]any{
			"hostnames": []any{hostname},
			"rules": []any{map[string]any{"filters": []any{map[string]any{
				"type": "ExtensionRef", "extensionRef": map[string]any{
					"group": "haproxy-haptic.org", "kind": "SSLPassthrough", "name": "enabled",
				},
			}}}},
		},
	}
}

func gatewaySSLPassthroughTLSRoute(name string) map[string]any {
	return map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1", "kind": "TLSRoute",
		"metadata": map[string]any{"namespace": "default", "name": name},
		"spec": map[string]any{
			"hostnames":  []any{"tls.example"},
			"parentRefs": []any{map[string]any{"name": "gateway"}},
			"rules": []any{map[string]any{"backendRefs": []any{map[string]any{
				"name": "echo", "port": int64(8443),
			}}}},
		},
	}
}

func (f *gatewaySSLPassthroughPublicationFixture) addGateway(t *testing.T) {
	t.Helper()
	require.NoError(t, f.gateways.Add(map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1", "kind": "Gateway",
		"metadata": map[string]any{"namespace": "default", "name": "gateway"},
		"spec": map[string]any{
			"gatewayClassName": "haptic",
			"listeners": []any{map[string]any{
				"name": "tls", "protocol": "TLS", "port": int64(9443),
				"tls":           map[string]any{"mode": "Passthrough"},
				"allowedRoutes": map[string]any{"namespaces": map[string]any{"from": "Same"}},
			}},
		},
	}, []string{"default", "gateway"}))
}

func (f *gatewaySSLPassthroughPublicationFixture) addService(t *testing.T) {
	t.Helper()
	require.NoError(t, f.services.Add(map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"namespace": "default", "name": "echo"},
		"spec":     map[string]any{"ports": []any{map[string]any{"name": "tls", "port": int64(8443)}}},
	}, []string{"default", "echo"}))
}

func (f *gatewaySSLPassthroughPublicationFixture) addHTTPRoute(t *testing.T, route map[string]any) {
	t.Helper()
	name := route["metadata"].(map[string]any)["name"].(string)
	require.NoError(t, f.httpRoutes.Add(route, []string{"default", name}))
}

func (f *gatewaySSLPassthroughPublicationFixture) updateHTTPRoute(t *testing.T, route map[string]any) {
	t.Helper()
	name := route["metadata"].(map[string]any)["name"].(string)
	require.NoError(t, f.httpRoutes.Update(route, []string{"default", name}))
}

func (f *gatewaySSLPassthroughPublicationFixture) addTLSRoute(t *testing.T, route map[string]any) {
	t.Helper()
	name := route["metadata"].(map[string]any)["name"].(string)
	require.NoError(t, f.tlsRoutes.Add(route, []string{"default", name}))
}

func (f *gatewaySSLPassthroughPublicationFixture) renderAndCommit(t *testing.T) string {
	t.Helper()
	result, err := f.service.Render(t.Context(), f.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, result.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, f.service)
	return result.HAProxyConfig
}
