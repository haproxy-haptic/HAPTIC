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
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGatewayMixedTLSAttachmentsTrackListenerTransitions(t *testing.T) {
	cfg := gatewaySSLPassthroughPublicationConfig(t)
	cfg.HAProxyConfig.Template = `{{ render_glob "gateway-ssl-passthrough-*" }}
{{ incremental_values("gateway-ssl-passthrough", "backends") | toJSON() }}`
	fixture := newGatewaySSLPassthroughPublicationFixtureWithConfig(t, cfg)
	fixture.addService(t)
	fixture.addTLSRoute(t, gatewaySSLPassthroughTLSRoute("both"))
	gateway := gatewayTLSCertificateGateway("")
	gateway["metadata"].(map[string]any)["name"] = "gateway"
	listeners := gateway["spec"].(map[string]any)["listeners"].([]any)
	for _, raw := range listeners {
		listener := raw.(map[string]any)
		listener["protocol"] = "TLS"
		listener["port"] = int64(9443)
		listener["hostname"] = "tls.example"
	}
	listeners[1].(map[string]any)["tls"] = map[string]any{"mode": "Passthrough"}
	listeners[1].(map[string]any)["hostname"] = "*.example"
	require.NoError(t, fixture.gateways.Add(gateway, []string{"default", "gateway"}))
	assertCurrent := func(terminate, passthrough bool) {
		t.Helper()
		rendered := fixture.renderAndCommit(t)
		require.Equal(t, terminate, strings.Contains(rendered, `"mode":"Terminate"`))
		require.Equal(t, passthrough, strings.Contains(rendered, `"mode":"Passthrough"`))
		require.Equal(t, rendered, fixture.renderAndCommit(t))
		oracle := newGatewaySSLPassthroughPublicationFixtureWithConfig(t, cfg)
		oracle.provider = fixture.provider
		require.Equal(t, rendered, oracle.renderAndCommit(t))
	}
	assertCurrent(true, true)
	gateway["spec"].(map[string]any)["listeners"] = listeners[:1]
	require.NoError(t, fixture.gateways.Update(gateway, []string{"default", "gateway"}))
	assertCurrent(true, false)
	gateway["spec"].(map[string]any)["listeners"] = listeners
	require.NoError(t, fixture.gateways.Update(gateway, []string{"default", "gateway"}))
	assertCurrent(true, true)
	require.NoError(t, fixture.tlsRoutes.Delete("default", "both", []string{"default", "both"}))
	assertCurrent(false, false)
	fixture.addTLSRoute(t, gatewaySSLPassthroughTLSRoute("both"))
	assertCurrent(true, true)
}

func TestGatewayMixedTLSCertificateReferenceLifecycle(t *testing.T) {
	fixture := newMixedTLSCertificateFixture(t)
	gateway := gatewayTLSCertificateGateway("cert-system")
	listeners := gateway["spec"].(map[string]any)["listeners"].([]any)
	for _, raw := range listeners {
		listener := raw.(map[string]any)
		listener["protocol"] = "TLS"
		listener["port"] = int64(9443)
	}
	listeners[1].(map[string]any)["tls"] = map[string]any{"mode": "Passthrough"}
	listeners[1].(map[string]any)["hostname"] = "*.example.org"
	fixture.addGateway(t, gateway)
	secret := gatewayFeatureTLSSecret("server-cert", "REMOTE-CERT", "REMOTE-KEY", nil)
	secret["metadata"].(map[string]any)["namespace"] = "cert-system"
	require.NoError(t, fixture.secrets.Add(secret, []string{"cert-system", "server-cert"}))
	assertCurrent := func(resolved bool) {
		t.Helper()
		result := renderGatewayFeaturesAndCommit(t, fixture)
		require.Equal(t, resolved, strings.Contains(result.HAProxyConfig, `"protocol":"TLS"`))
		if !resolved {
			require.Contains(t, result.HAProxyConfig, "# terminating=[]")
		}
		assertRenderResultObservablesEqual(t, result, renderGatewayFeaturesAndCommit(t, fixture))
		oracle := newMixedTLSCertificateFixture(t)
		oracle.provider = fixture.provider
		assertRenderResultObservablesEqual(t, result, renderGatewayFeaturesAndCommit(t, oracle))
	}
	assertCurrent(false)
	grant := gatewayMTLSReferenceGrant("cert-system")
	grant["spec"].(map[string]any)["to"] = []any{map[string]any{"group": "", "kind": "Secret", "name": "server-cert"}}
	require.NoError(t, fixture.referenceGrants.Add(grant, []string{"cert-system", "allow-client-ca"}))
	assertCurrent(true)
	require.NoError(t, fixture.referenceGrants.Delete("cert-system", "allow-client-ca", []string{"cert-system", "allow-client-ca"}))
	assertCurrent(false)
	require.NoError(t, fixture.referenceGrants.Add(grant, []string{"cert-system", "allow-client-ca"}))
	assertCurrent(true)
	require.NoError(t, fixture.secrets.Delete("cert-system", "server-cert", []string{"cert-system", "server-cert"}))
	assertCurrent(false)
	require.NoError(t, fixture.secrets.Add(secret, []string{"cert-system", "server-cert"}))
	assertCurrent(true)
}

func newMixedTLSCertificateFixture(t *testing.T) *gatewayRouteAnalysisFixture {
	t.Helper()
	snippets := loadGatewayFeaturePublicationSnippets(t)
	maps.Copy(snippets, loadGatewayHostMapSnippets(t, gatewayHostMapChartRoot(t), map[string][]string{
		"gateway/16-crtlist-per-listener.yaml": {"gateway-terminating-listeners-100-gateway", "util-gateway-listener-crtlist-name"},
		"gateway/92-listener-tls-options.yaml": {"util-gateway-listener-tls-options"},
		"gateway/21-route-helpers.yaml":        {"util-bounded-name"},
		"base/library.yaml":                    {"util-webhook-reject-or-warn", "util-config-injection-kind"},
	}))
	return newGatewayRouteAnalysisFixtureWithTemplates(t, snippets, gatewayTLSCertificateRoot+`
{{ render "gateway-listener-state-100-gateway" }}
{{ render "map-hostvalues-479-gateway-listenersets-empty" }}
{{ render "map-hostvalues-480-gateway-listenersets" default "" }}
{{ render "gateway-frontend-ca-permissions-100-gateway" }}
{{ render "gateway-frontend-mtls-100-gateway" }}
{{ render "gateway-listener-mtls-100-gateway" }}
{{ render "gateway-pod-port-candidates-100-gateway" }}
{{ render "gateway-pod-port-allocations-200-leader" }}
{{ render "gateway-terminating-listeners-100-gateway" }}
# terminating={{ incremental_values("gateway-terminating-listeners","listeners") | toJSON() }}`)
}

func TestGatewayMixedTLSStaticClaimsTrackListenerMembership(t *testing.T) {
	cfg := gatewaySSLPassthroughPublicationConfig(t)
	maps.Copy(cfg.TemplateSnippets, loadGatewayHostMapSnippets(t, gatewayHostMapChartRoot(t), map[string][]string{
		"gateway/10-features.yaml":          {"gateway-listener-state-100-gateway"},
		"gateway/52-static-tls-claims.yaml": {"util-gateway-static-tls-groups"},
	}))
	cfg.HAProxyConfig.Template = `{%- var _, _ = shared.ComputeIfAbsent("globalFeatures", func() any {
		return map[string]any{"tlsInspectionRequired": false}
	}) -%}
{{ render "util-gateway-static-tls-groups" }}
{{ map[string]any{"groups": shared.Get("gatewayStaticTLSGroups"),
	"inspection": dig(shared.Get("globalFeatures"), "tlsInspectionRequired")} | toJSON() }}`
	fixture := newGatewaySSLPassthroughPublicationFixtureWithConfig(t, cfg)
	assertCurrent := func(keys ...string) string {
		t.Helper()
		rendered := fixture.renderAndCommit(t)
		var result struct {
			Groups     map[string]any `json:"groups"`
			Inspection bool           `json:"inspection"`
		}
		require.NoError(t, json.Unmarshal([]byte(rendered), &result))
		require.ElementsMatch(t, keys, slices.Collect(maps.Keys(result.Groups)))
		require.Equal(t, len(keys) > 0, result.Inspection)
		counts := fixture.engine.executionCounts()
		require.Equal(t, rendered, fixture.renderAndCommit(t))
		require.Equal(t, counts, fixture.engine.executionCounts())
		oracle := newGatewaySSLPassthroughPublicationFixtureWithConfig(t, cfg)
		oracle.provider = fixture.provider
		require.Equal(t, rendered, oracle.renderAndCommit(t))
		return rendered
	}
	assertCurrent()
	first := gatewayMixedTLSClaimFixture("first")
	listeners := first["spec"].(map[string]any)["listeners"].([]any)
	listeners[0].(map[string]any)["tls"] = map[string]any{"mode": "Terminate", "certificateRefs": []any{map[string]any{"name": "server-cert"}}}
	require.NoError(t, fixture.gateways.Add(first, []string{"default", "first"}))
	assertCurrent()
	listeners[0].(map[string]any)["tls"] = map[string]any{"mode": "Passthrough"}
	require.NoError(t, fixture.gateways.Update(first, []string{"default", "first"}))
	assertCurrent("default/first/9443")
	second := gatewayMixedTLSClaimFixture("second")
	require.NoError(t, fixture.gateways.Add(second, []string{"default", "second"}))
	assertCurrent("default/first/9443", "default/second/9443")
	counts := fixture.engine.executionCounts()
	listeners[0].(map[string]any)["name"] = "renamed"
	listeners[0].(map[string]any)["hostname"] = "renamed.first.example"
	require.NoError(t, fixture.gateways.Update(first, []string{"default", "first"}))
	changed := assertCurrent("default/first/9443", "default/second/9443")
	require.Contains(t, changed, "renamed.first.example")
	require.NotContains(t, changed, "exact.first.example")
	require.Equal(t, counts["gateways/first"]+1, fixture.engine.executionCounts()["gateways/first"])
	require.Equal(t, counts["gateways/second"], fixture.engine.executionCounts()["gateways/second"])
	listeners[0].(map[string]any)["tls"] = map[string]any{"mode": "Terminate", "certificateRefs": []any{map[string]any{"name": "server-cert"}}}
	require.NoError(t, fixture.gateways.Update(first, []string{"default", "first"}))
	assertCurrent("default/second/9443")
	require.NoError(t, fixture.gateways.Delete("default", "second", []string{"default", "second"}))
	assertCurrent()
	listeners[0].(map[string]any)["tls"] = map[string]any{"mode": "Passthrough"}
	require.NoError(t, fixture.gateways.Update(first, []string{"default", "first"}))
	assertCurrent("default/first/9443")
	require.NoError(t, fixture.gateways.Delete("default", "first", []string{"default", "first"}))
	assertCurrent()
}

func gatewayMixedTLSClaimFixture(name string) map[string]any {
	gateway := gatewayTLSCertificateGateway("")
	gateway["metadata"].(map[string]any)["name"] = name
	listeners := gateway["spec"].(map[string]any)["listeners"].([]any)
	for index, raw := range listeners {
		listener := raw.(map[string]any)
		listener["protocol"] = "TLS"
		listener["port"] = int64(9443)
		listener["tls"] = map[string]any{"mode": "Passthrough"}
		listener["hostname"] = "exact." + name + ".example"
		if index == 1 {
			listener["hostname"] = "*." + name + ".example"
		}
	}
	return gateway
}
