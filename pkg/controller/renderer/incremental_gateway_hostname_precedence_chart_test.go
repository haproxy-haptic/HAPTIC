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
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestGatewayRouteHostnamePrecedenceIncrementalTransitions(t *testing.T) {
	const root = gatewayRouteAnalysisRoot + `
# entries={{ incremental_values("gateway-route-analysis", "entries") | toJSON() }}`
	fixture := newGatewayRouteAnalysisFixtureWithTemplates(t, loadGatewayRouteAnalysisSnippets(t), root)
	gateway := gatewayRouteAnalysisGateway()
	listeners, _, err := unstructured.NestedSlice(gateway, "spec", "listeners")
	require.NoError(t, err)
	listeners[0].(map[string]any)["hostname"] = "abc.example.com"
	require.NoError(t, unstructured.SetNestedSlice(gateway, listeners, "spec", "listeners"))
	fixture.addGateway(t, gateway)
	route := gatewayRouteAnalysisRouteWithPathType("specific", "abc.example.com", "PathPrefix", "/app", "")
	fixture.addHTTPRoute(t, route)
	baseline := fixture.renderAndCommitCacheReady(t).HAProxyConfig
	assertGatewayHostnamePrecedence(t, baseline, false, 1)

	broader := gatewayRouteAnalysisRouteWithPathType("broader", "*.example.com", "PathPrefix", "/app/longer", "")
	fixture.addHTTPRoute(t, broader)
	collision := fixture.renderAndCommitCacheReady(t).HAProxyConfig
	assertGatewayHostnamePrecedence(t, collision, true, 2)
	counts := fixture.engine.executionCounts()
	assert.Equal(t, collision, fixture.renderAndCommitCacheReady(t).HAProxyConfig)
	assert.Equal(t, counts, fixture.engine.executionCounts())

	cold := newGatewayRouteAnalysisFixtureWithTemplates(t, loadGatewayRouteAnalysisSnippets(t), root)
	cold.addGateway(t, gateway)
	cold.addHTTPRoute(t, route)
	cold.addHTTPRoute(t, broader)
	assert.Equal(t, collision, cold.renderAndCommitCacheReady(t).HAProxyConfig)

	fixture.addHTTPRoute(t, gatewayRouteAnalysisRouteWithPathType("broader-peer", "*.example.com", "PathPrefix", "/app/peer", ""))
	sharedRank := fixture.renderAndCommitCacheReady(t).HAProxyConfig
	assertGatewayHostnamePrecedence(t, sharedRank, true, 3)
	fixture.deleteHTTPRoute(t, "broader-peer")
	assert.Equal(t, collision, fixture.renderAndCommitCacheReady(t).HAProxyConfig)

	fixture.updateHTTPRoute(t, gatewayRouteAnalysisRouteWithPathType(
		"broader", "abc.example.com", "PathPrefix", "/app/longer", ""))
	sameRank := fixture.renderAndCommitCacheReady(t).HAProxyConfig
	assertGatewayHostnamePrecedence(t, sameRank, false, 2)

	fixture.updateHTTPRoute(t, gatewayRouteAnalysisRouteWithPathType(
		"broader", "*.example.com", "PathPrefix", "/app", ""))
	samePath := fixture.renderAndCommitCacheReady(t).HAProxyConfig
	assertGatewayHostnamePrecedence(t, samePath, false, 2)

	fixture.updateHTTPRoute(t, gatewayRouteAnalysisRouteWithPathType(
		"broader", "*.example.com", "PathPrefix", "/app", "GET"))
	conditional := fixture.renderAndCommitCacheReady(t).HAProxyConfig
	assertGatewayHostnamePrecedence(t, conditional, true, 2)

	fixture.updateHTTPRoute(t, broader)
	assert.Equal(t, collision, fixture.renderAndCommitCacheReady(t).HAProxyConfig)
	fixture.deleteHTTPRoute(t, "broader")
	assert.Equal(t, baseline, fixture.renderAndCommitCacheReady(t).HAProxyConfig)
}

func assertGatewayHostnamePrecedence(t *testing.T, rendered string, expected bool, count int) {
	t.Helper()
	_, encoded, found := strings.Cut(rendered, "# entries=")
	require.True(t, found)
	var entries []struct {
		HostnamePrecedence bool `json:"hostnamePrecedence"`
		AdvancedScope      bool `json:"advancedScope"`
	}
	require.NoError(t, json.Unmarshal([]byte(encoded), &entries))
	require.Len(t, entries, count)
	for _, entry := range entries {
		assert.Equal(t, expected, entry.HostnamePrecedence)
		assert.Equal(t, expected, entry.AdvancedScope)
	}
}
