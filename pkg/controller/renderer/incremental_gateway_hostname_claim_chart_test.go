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

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
)

const (
	gatewayHostnameClaimComponent  = "gateway-listener-hostname-claims-100-gateway"
	gatewayHostnameSuffixComponent = "gateway-listener-hostname-suffixes-100-gateway"
)

const gatewayHostnameClaimRoot = `{{ render "frontend-extra-100-gateway-misdirected" }}
{{ render "log-fields-500-gateway" }}`

const gatewayHostnameClaimAbortRoot = gatewayHostnameClaimRoot + `
{%- if tostring(extraContext | dig("failAfterHostMap") | fallback(false)) == "true" -%}
{{ fail("forced failure after gateway hostname claims") }}
{%- end -%}`

const gatewayHostnameSuffixRoot = `{{ render "map-hostregex-500-gateway" }}`

func TestGatewayHostnameSuffixExecutionScaling(t *testing.T) {
	for _, gatewayCount := range []int{300, 1000, 3000} {
		t.Run(fmt.Sprintf("gateways=%d", gatewayCount), func(t *testing.T) {
			fixture := newGatewayHostMapFixtureWithTemplates(
				t, loadGatewayHostnameSuffixSnippets(t), gatewayHostnameSuffixRoot)
			for index := range gatewayCount {
				name := fmt.Sprintf("gateway-%06d", index)
				fixture.addGateway(t, gatewayHostnameClaimGateway(
					name, fmt.Sprintf("*.host-%06d.example.com", index)))
			}

			cold := fixture.renderAndCommitCacheReady(t)
			assert.Contains(t, cold.HAProxyConfig, `\.host-000000\.example\.com$ .host-000000.example.com`)
			assert.Contains(t, cold.HAProxyConfig, fmt.Sprintf(
				`\.host-%06d\.example\.com$ .host-%06d.example.com`, gatewayCount-1, gatewayCount-1))
			assert.Equal(t, uint64(1), fixture.executions(
				gatewayHostnameSuffixComponent, "gateways", "gateway-000000"))

			warm := fixture.renderAndCommitCacheReady(t)
			assert.Equal(t, cold.HAProxyConfig, warm.HAProxyConfig)
			assert.Equal(t, uint64(1), fixture.executions(
				gatewayHostnameSuffixComponent, "gateways", "gateway-000000"))

			require.NoError(t, fixture.gateways.Update(
				gatewayHostnameClaimGateway("gateway-000000", "*.changed.example.com"),
				[]string{"default", "gateway-000000"},
			))
			changed := fixture.renderAndCommitCacheReady(t)
			assert.Contains(t, changed.HAProxyConfig, `\.changed\.example\.com$ .changed.example.com`)
			assert.NotContains(t, changed.HAProxyConfig, `.host-000000.example.com`)
			assert.Equal(t, uint64(2), fixture.executions(
				gatewayHostnameSuffixComponent, "gateways", "gateway-000000"))
			assert.Equal(t, uint64(1), fixture.executions(gatewayHostnameSuffixComponent,
				"gateways", fmt.Sprintf("gateway-%06d", gatewayCount-1)))
		})
	}
}

func TestGatewayHostnameSuffixOrdersMostSpecificAndPromotesCachedDuplicate(t *testing.T) {
	fixture := newGatewayHostMapFixtureWithTemplates(
		t, loadGatewayHostnameSuffixSnippets(t), gatewayHostnameSuffixRoot)
	fixture.addGateway(t, gatewayHostnameClaimGateway("broad", "*.example.com"))
	fixture.addGateway(t, gatewayHostnameClaimGateway("specific", "*.foo.example.com"))
	fixture.addGateway(t, gatewayHostnameClaimGateway("duplicate", "*.example.com"))

	cold := fixture.renderAndCommitCacheReady(t)
	broad := `\.example\.com$ .example.com`
	specific := `\.foo\.example\.com$ .foo.example.com`
	assert.Less(t, strings.Index(cold.HAProxyConfig, specific), strings.Index(cold.HAProxyConfig, broad))
	assert.Equal(t, 1, strings.Count(cold.HAProxyConfig, broad))

	fixture.deleteGateway(t, "broad")
	promoted := fixture.renderAndCommitCacheReady(t)
	assert.Equal(t, cold.HAProxyConfig, promoted.HAProxyConfig)
	assert.Equal(t, uint64(1), fixture.executions(
		gatewayHostnameSuffixComponent, "gateways", "duplicate"))
}

func TestGatewayHostnameClaimExecutionScaling(t *testing.T) {
	for _, gatewayCount := range []int{300, 1000, 3000} {
		t.Run(fmt.Sprintf("gateways=%d", gatewayCount), func(t *testing.T) {
			fixture := newGatewayHostMapFixtureWithTemplates(
				t, loadGatewayHostnameClaimSnippets(t), gatewayHostnameClaimRoot)
			for index := range gatewayCount {
				name := fmt.Sprintf("gateway-%06d", index)
				fixture.addGateway(t, gatewayHostnameClaimGateway(
					name, fmt.Sprintf("host-%06d.example.com", index)))
			}

			cold := fixture.renderAndCommitCacheReady(t)
			coldContent := gatewayHostnameClaimMapContent(t, cold, "listener-hostname-claim.map")
			assert.Contains(t, coldContent, "host-000000.example.com host-000000.example.com")
			assert.Contains(t, coldContent, fmt.Sprintf(
				"host-%06d.example.com host-%06d.example.com", gatewayCount-1, gatewayCount-1))
			assert.Equal(t, uint64(1), fixture.executions(
				gatewayHostnameClaimComponent, "gateways", "gateway-000000"))
			assert.Equal(t, uint64(1), fixture.executions(gatewayHostnameClaimComponent,
				"gateways", fmt.Sprintf("gateway-%06d", gatewayCount-1)))

			warm := fixture.renderAndCommitCacheReady(t)
			assert.Equal(t, requireAuxiliaryFiles(t, cold), requireAuxiliaryFiles(t, warm))
			assert.Equal(t, uint64(1), fixture.executions(
				gatewayHostnameClaimComponent, "gateways", "gateway-000000"))

			require.NoError(t, fixture.gateways.Update(
				gatewayHostnameClaimGateway("gateway-000000", "changed.example.com"),
				[]string{"default", "gateway-000000"},
			))
			changed := fixture.renderAndCommitCacheReady(t)
			changedContent := gatewayHostnameClaimMapContent(
				t, changed, "listener-hostname-claim.map")
			assert.Contains(t, changedContent, "changed.example.com changed.example.com")
			assert.NotContains(t, changedContent, "host-000000.example.com")
			assert.Equal(t, uint64(2), fixture.executions(
				gatewayHostnameClaimComponent, "gateways", "gateway-000000"))
			assert.Equal(t, uint64(1), fixture.executions(gatewayHostnameClaimComponent,
				"gateways", fmt.Sprintf("gateway-%06d", gatewayCount-1)))
		})
	}
}

func TestGatewayHostnameClaimCollisionPromotesCachedProducer(t *testing.T) {
	fixture := newGatewayHostMapFixtureWithTemplates(
		t, loadGatewayHostnameClaimSnippets(t), gatewayHostnameClaimRoot)
	fixture.addGateway(t, gatewayHostnameClaimGateway("first", "*.example.com"))
	fixture.addGateway(t, gatewayHostnameClaimGateway("second", "*.example.com"))

	cold := fixture.renderAndCommitCacheReady(t)
	coldContent := gatewayHostnameClaimMapContent(
		t, cold, "listener-hostname-claim-regex.map")
	assert.Equal(t, 1, strings.Count(coldContent, "*.example.com"))

	fixture.deleteGateway(t, "first")
	promoted := fixture.renderAndCommitCacheReady(t)
	assert.Equal(t, coldContent, gatewayHostnameClaimMapContent(
		t, promoted, "listener-hostname-claim-regex.map"))
	assert.Equal(t, uint64(1), fixture.executions(
		gatewayHostnameClaimComponent, "gateways", "second"))
}

func TestGatewayHostnameClaimListenerSetInvalidatesOnlyItsGateway(t *testing.T) {
	fixture := newGatewayHostMapFixtureWithTemplates(
		t, loadGatewayHostnameClaimSnippets(t), gatewayHostnameClaimRoot)
	fixture.addGateway(t, gatewayHostnameClaimGatewayWithListenerSets("first"))
	fixture.addGateway(t, gatewayHostnameClaimGatewayWithListenerSets("second"))
	fixture.addListenerSet(t, gatewayHostnameClaimListenerSet("first-listeners", "first", "first.example.com"))
	fixture.addListenerSet(t, gatewayHostnameClaimListenerSet("second-listeners", "second", "second.example.com"))

	cold := fixture.renderAndCommitCacheReady(t)
	coldContent := gatewayHostnameClaimMapContent(t, cold, "listener-hostname-claim.map")
	assert.Contains(t, coldContent, "first.example.com first.example.com")
	assert.Contains(t, coldContent, "second.example.com second.example.com")

	require.NoError(t, fixture.listenerSets.Update(
		gatewayHostnameClaimListenerSet("first-listeners", "first", "changed.example.com"),
		[]string{"default", "first-listeners"},
	))
	changed := fixture.renderAndCommitCacheReady(t)
	changedContent := gatewayHostnameClaimMapContent(t, changed, "listener-hostname-claim.map")
	assert.Contains(t, changedContent, "changed.example.com changed.example.com")
	assert.NotContains(t, changedContent, "first.example.com")
	assert.Contains(t, changedContent, "second.example.com second.example.com")
	assert.Equal(t, uint64(2), fixture.executions(
		gatewayHostnameClaimComponent, "gateways", "first"))
	assert.Equal(t, uint64(1), fixture.executions(
		gatewayHostnameClaimComponent, "gateways", "second"))
}

func TestGatewayHostnameClaimAbortDoesNotPublishSpeculativeValue(t *testing.T) {
	fixture := newGatewayHostMapFixtureWithTemplates(
		t, loadGatewayHostnameClaimSnippets(t), gatewayHostnameClaimAbortRoot)
	fixture.addGateway(t, gatewayHostnameClaimGateway("gateway", "before.example.com"))
	fixture.renderAndCommitCacheReady(t)

	require.NoError(t, fixture.gateways.Update(
		gatewayHostnameClaimGateway("gateway", "after.example.com"),
		[]string{"default", "gateway"},
	))
	fixture.config.TemplatingSettings.ExtraContext["failAfterHostMap"] = true
	_, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.ErrorContains(t, err, "forced failure after gateway hostname claims")

	fixture.config.TemplatingSettings.ExtraContext["failAfterHostMap"] = false
	retried := fixture.renderAndCommitCacheReady(t)
	retriedContent := gatewayHostnameClaimMapContent(t, retried, "listener-hostname-claim.map")
	assert.Contains(t, retriedContent, "after.example.com after.example.com")
	assert.NotContains(t, retriedContent, "before.example.com")

	fresh := newGatewayHostMapFixtureWithTemplates(
		t, loadGatewayHostnameClaimSnippets(t), gatewayHostnameClaimAbortRoot)
	fresh.addGateway(t, gatewayHostnameClaimGateway("gateway", "after.example.com"))
	oracle := fresh.renderAndCommitCacheReady(t)
	assert.Equal(t, oracle.HAProxyConfig, retried.HAProxyConfig)
	assert.Equal(t, requireAuxiliaryFiles(t, oracle), requireAuxiliaryFiles(t, retried))
}

func TestGatewayHostnameClaimPresenceTracksEmptyGateway(t *testing.T) {
	fixture := newGatewayHostMapFixtureWithTemplates(
		t, loadGatewayHostnameClaimSnippets(t), gatewayHostnameClaimRoot)

	empty := fixture.renderAndCommitCacheReady(t)
	assert.NotContains(t, empty.HAProxyConfig, "%(gw_route)")

	gateway := gatewayHostnameClaimGateway("empty", "")
	gateway["spec"].(map[string]any)["listeners"] = []any{}
	fixture.addGateway(t, gateway)
	present := fixture.renderAndCommitCacheReady(t)
	assert.Contains(t, present.HAProxyConfig, "%(gw_route)[var(txn.gw_rule_id)]")

	fixture.deleteGateway(t, "empty")
	removed := fixture.renderAndCommitCacheReady(t)
	assert.NotContains(t, removed.HAProxyConfig, "%(gw_route)")
}

func loadGatewayHostnameClaimSnippets(t *testing.T) map[string]config.TemplateSnippet {
	t.Helper()
	return loadChartSnippets(t, chartLibrariesRoot(t), map[string][]string{
		"gateway/40-maps-host.yaml": {
			"map-hostvalues-479-gateway-listenersets-empty",
			"map-hostvalues-480-gateway-listenersets",
		},
		"gateway/60-frontend.yaml": {
			gatewayHostnameClaimComponent,
			"util-gateway-listener-hostname-claim-publications",
			"frontend-extra-100-gateway-misdirected",
			"log-fields-500-gateway",
		},
	})
}

func loadGatewayHostnameSuffixSnippets(t *testing.T) map[string]config.TemplateSnippet {
	t.Helper()
	return loadChartSnippets(t, chartLibrariesRoot(t), map[string][]string{
		"gateway/40-maps-host.yaml": {
			gatewayHostnameSuffixComponent,
			"util-gateway-listener-hostname-suffix-publications",
			"map-hostregex-500-gateway",
		},
	})
}

func gatewayHostnameClaimGateway(name, hostname string) map[string]any {
	return map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "Gateway",
		"metadata": map[string]any{
			"namespace": "default",
			"name":      name,
		},
		"spec": map[string]any{
			"gatewayClassName": "haptic",
			"listeners": []any{map[string]any{
				"name": "https", "protocol": "HTTPS", "port": int64(443), "hostname": hostname,
			}},
		},
	}
}

func gatewayHostnameClaimGatewayWithListenerSets(name string) map[string]any {
	gateway := gatewayHostnameClaimGateway(name, "")
	spec := gateway["spec"].(map[string]any)
	spec["listeners"] = []any{}
	spec["allowedListeners"] = map[string]any{
		"namespaces": map[string]any{"from": "All"},
	}
	return gateway
}

func gatewayHostnameClaimListenerSet(name, gatewayName, hostname string) map[string]any {
	return map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "ListenerSet",
		"metadata": map[string]any{
			"namespace": "default",
			"name":      name,
		},
		"spec": map[string]any{
			"parentRef": map[string]any{"name": gatewayName},
			"listeners": []any{map[string]any{
				"name": "https", "protocol": "HTTPS", "port": int64(443), "hostname": hostname,
			}},
		},
	}
}

func gatewayHostnameClaimMapContent(t *testing.T, result *RenderResult, suffix string) string {
	t.Helper()
	for _, file := range requireAuxiliaryFiles(t, result).MapFiles {
		if strings.HasSuffix(file.Path, suffix) {
			return file.Content
		}
	}
	t.Fatalf("map %q was not rendered", suffix)
	return ""
}
