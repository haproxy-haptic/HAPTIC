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
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
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
	gatewayHTTPBackendComponent = "backenditems-500-gateway-http"
	gatewayGRPCBackendComponent = "backenditems-510-gateway-grpc"
	gatewayBackendBaselineEnv   = "HAPTIC_GATEWAY_BACKEND_BASELINE"
)

const gatewayBackendChartRoot = `{%- import "util-replay-gateway-backend-effects" for ReplayGatewayBackendEffects -%}
{{ planRegistry.ProfileGroup() }}
{{- render "backendtlsvalues-490-gateway" default "" -}}
{{- ReplayGatewayBackendEffects("gateway-backends") -}}
# gateway/backends-gateway
{{ render "backenditems-500-gateway-http" }}
{{- render "backenditems-510-gateway-grpc" -}}
{%- if tostring(extraContext | dig("failAfterBackends") | fallback(false)) == "true" -%}
{{ fail("forced failure after gateway backends") }}
{%- end -%}`

const gatewayBackendLegacyChartRoot = `{{ planRegistry.ProfileGroup() }}
{{ render "backends-500-gateway" }}`

type gatewayBackendChartFixture struct {
	config             *config.Config
	service            *RenderService
	engine             *dynamicBindingCountingEngine
	gateways           *k8sstore.MemoryStore
	httpRoutes         *k8sstore.MemoryStore
	grpcRoutes         *k8sstore.MemoryStore
	backendTLSPolicies *k8sstore.MemoryStore
	services           *k8sstore.MemoryStore
	endpoints          *k8sstore.MemoryStore
	secrets            *k8sstore.MemoryStore
	configMaps         *k8sstore.MemoryStore
	referenceGrants    *k8sstore.MemoryStore
	namespaces         *k8sstore.MemoryStore
	provider           stores.StoreProvider
}

func TestGatewayBackendChartCachesExactRouteDependencies(t *testing.T) {
	fixture := newGatewayBackendChartFixture(t)
	fixture.add(t, fixture.gateways, gatewayBackendGateway("gateway"), "default", "gateway")
	fixture.add(t, fixture.services, sslPassthroughService("echo", "http", 80), "default", "echo")
	fixture.add(t, fixture.services, sslPassthroughService("other", "http", 80), "default", "other")
	fixture.add(t, fixture.endpoints, sslPassthroughEndpoint("echo", "http", 8080, "10.0.0.1"), "default", "echo")
	fixture.add(t, fixture.endpoints, sslPassthroughEndpoint("other", "http", 8080, "10.0.0.2"), "default", "other")
	fixture.add(t, fixture.httpRoutes, gatewayBackendRoute("HTTPRoute", "http-a", "echo"), "default", "http-a")
	fixture.add(t, fixture.httpRoutes, gatewayBackendRoute("HTTPRoute", "http-b", "other"), "default", "http-b")
	fixture.add(t, fixture.grpcRoutes, gatewayBackendRoute("GRPCRoute", "grpc-a", "echo"), "default", "grpc-a")

	first := fixture.renderAndCommitCacheReady(t)
	assert.Contains(t, first.HAProxyConfig, "backend gw_h_default_http-a_echo_80")
	assert.Contains(t, first.HAProxyConfig, "backend gw_h_default_http-b_other_80")
	assert.Contains(t, first.HAProxyConfig, "backend gw_g_default_grpc-a_echo_80")
	assert.Contains(t, first.HAProxyConfig, "10.0.0.1:8080")
	assert.Contains(t, first.HAProxyConfig, "10.0.0.2:8080")

	warm := fixture.renderAndCommitCacheReady(t)
	assert.Equal(t, first.HAProxyConfig, warm.HAProxyConfig)
	fixture.assertExecutions(t, gatewayHTTPBackendComponent, "httproutes", "http-a", 1)
	fixture.assertExecutions(t, gatewayHTTPBackendComponent, "httproutes", "http-b", 1)
	fixture.assertExecutions(t, gatewayGRPCBackendComponent, "grpcroutes", "grpc-a", 1)

	fixture.add(t, fixture.services, sslPassthroughService("unrelated", "http", 80), "default", "unrelated")
	fixture.add(t, fixture.endpoints, sslPassthroughEndpoint("unrelated", "http", 8080, "10.0.0.3"), "default", "unrelated")
	fixture.add(t, fixture.gateways, gatewayBackendGateway("unrelated"), "default", "unrelated")
	fixture.add(t, fixture.secrets, gatewayBackendSecret("unrelated", "crt", "key"), "default", "unrelated")
	fixture.add(t, fixture.configMaps, gatewayBackendConfigMap("unrelated", "ca"), "default", "unrelated")
	unrelated := fixture.renderAndCommitCacheReady(t)
	assert.Equal(t, first.HAProxyConfig, unrelated.HAProxyConfig)
	fixture.assertExecutions(t, gatewayHTTPBackendComponent, "httproutes", "http-a", 1)
	fixture.assertExecutions(t, gatewayHTTPBackendComponent, "httproutes", "http-b", 1)
	fixture.assertExecutions(t, gatewayGRPCBackendComponent, "grpcroutes", "grpc-a", 1)

	fixture.update(t, fixture.endpoints, sslPassthroughEndpoint("echo", "http", 8080, "10.0.0.9"), "default", "echo")
	changed := fixture.renderAndCommitCacheReady(t)
	assert.Contains(t, changed.HAProxyConfig, "10.0.0.9:8080")
	assert.NotContains(t, changed.HAProxyConfig, "10.0.0.1:8080")
	fixture.assertExecutions(t, gatewayHTTPBackendComponent, "httproutes", "http-a", 2)
	fixture.assertExecutions(t, gatewayHTTPBackendComponent, "httproutes", "http-b", 1)
	fixture.assertExecutions(t, gatewayGRPCBackendComponent, "grpcroutes", "grpc-a", 2)
}

func TestGatewayBackendChartPolicySelectorsAndFilesStayExact(t *testing.T) {
	fixture, baseline := newGatewayBackendPolicySelectorFixture(t)
	fixture.add(t, fixture.configMaps, gatewayBackendConfigMap("ca", "ca-one"), "default", "ca")
	assert.Equal(t, baseline.HAProxyConfig, fixture.renderAndCommitCacheReady(t).HAProxyConfig)

	fixture.add(t, fixture.backendTLSPolicies,
		gatewayBackendTLSPolicy("winner", "2024-01-01T00:00:00Z", "winner.example", "ca"),
		"default", "winner",
	)
	winner := fixture.renderAndCommitCacheReady(t)
	assert.Contains(t, winner.HAProxyConfig, "ca-file files/btls-default-winner.crt")
	assert.Contains(t, winner.HAProxyConfig, "sni str(winner.example)")
	assert.Equal(t, "ca-one", gatewayBackendGeneralFile(t, winner, "btls-default-winner.crt"))
	fixture.assertExecutions(t, gatewayHTTPBackendComponent, "httproutes", "http-a", 2)
	fixture.assertExecutions(t, gatewayHTTPBackendComponent, "httproutes", "http-b", 1)
	fixture.assertExecutions(t, gatewayGRPCBackendComponent, "grpcroutes", "grpc-a", 2)
	statusOnly := gatewayBackendTLSPolicy("winner", "2024-01-01T00:00:00Z", "winner.example", "ca")
	statusOnly["status"] = map[string]any{"ancestors": []any{map[string]any{"controllerName": "test"}}}
	fixture.update(t, fixture.backendTLSPolicies, statusOnly, "default", "winner")
	assert.Equal(t, winner.HAProxyConfig, fixture.renderAndCommitCacheReady(t).HAProxyConfig)
	fixture.assertExecutions(t, gatewayHTTPBackendComponent, "httproutes", "http-a", 2)
	fixture.assertExecutions(t, gatewayGRPCBackendComponent, "grpcroutes", "grpc-a", 2)

	fixture.add(t, fixture.backendTLSPolicies,
		gatewayBackendTLSPolicy("loser", "2024-01-02T00:00:00Z", "loser.example", ""),
		"default", "loser",
	)
	assert.Equal(t, winner.HAProxyConfig, fixture.renderAndCommitCacheReady(t).HAProxyConfig)
	fixture.assertExecutions(t, gatewayHTTPBackendComponent, "httproutes", "http-a", 2)
	fixture.assertExecutions(t, gatewayGRPCBackendComponent, "grpcroutes", "grpc-a", 2)

	fixture.update(t, fixture.backendTLSPolicies,
		gatewayBackendTLSPolicy("loser", "2024-01-02T00:00:00Z", "changed-loser.example", ""),
		"default", "loser",
	)
	assert.Equal(t, winner.HAProxyConfig, fixture.renderAndCommitCacheReady(t).HAProxyConfig)
	fixture.assertExecutions(t, gatewayHTTPBackendComponent, "httproutes", "http-a", 2)
	fixture.assertExecutions(t, gatewayGRPCBackendComponent, "grpcroutes", "grpc-a", 2)

	fixture.update(t, fixture.backendTLSPolicies,
		gatewayBackendTLSPolicy("winner", "2024-01-01T00:00:00Z", "updated.example", "ca"),
		"default", "winner",
	)
	updated := fixture.renderAndCommitCacheReady(t)
	assert.Contains(t, updated.HAProxyConfig, "sni str(updated.example)")
	fixture.assertExecutions(t, gatewayHTTPBackendComponent, "httproutes", "http-a", 3)
	fixture.assertExecutions(t, gatewayHTTPBackendComponent, "httproutes", "http-b", 1)
	fixture.assertExecutions(t, gatewayGRPCBackendComponent, "grpcroutes", "grpc-a", 3)

	fixture.update(t, fixture.configMaps, gatewayBackendConfigMap("ca", "ca-two"), "default", "ca")
	rotated := fixture.renderAndCommitCacheReady(t)
	assert.Equal(t, "ca-two", gatewayBackendGeneralFile(t, rotated, "btls-default-winner.crt"))
	fixture.assertExecutions(t, gatewayHTTPBackendComponent, "httproutes", "http-a", 4)
	fixture.assertExecutions(t, gatewayHTTPBackendComponent, "httproutes", "http-b", 1)
	fixture.assertExecutions(t, gatewayGRPCBackendComponent, "grpcroutes", "grpc-a", 4)

	fixture.delete(t, fixture.backendTLSPolicies, "winner", "default", "winner")
	promoted := fixture.renderAndCommitCacheReady(t)
	assert.NotContains(t, promoted.HAProxyConfig, "btls-default-winner.crt")
	assert.Contains(t, promoted.HAProxyConfig, "sni str(changed-loser.example)")
	fixture.assertExecutions(t, gatewayHTTPBackendComponent, "httproutes", "http-a", 5)
	fixture.assertExecutions(t, gatewayGRPCBackendComponent, "grpcroutes", "grpc-a", 5)

	fixture.delete(t, fixture.backendTLSPolicies, "loser", "default", "loser")
	missing := fixture.renderAndCommitCacheReady(t)
	assert.NotContains(t, missing.HAProxyConfig, "sni str(")
	fixture.assertExecutions(t, gatewayHTTPBackendComponent, "httproutes", "http-a", 6)
	fixture.assertExecutions(t, gatewayHTTPBackendComponent, "httproutes", "http-b", 1)
	fixture.assertExecutions(t, gatewayGRPCBackendComponent, "grpcroutes", "grpc-a", 6)
}

func newGatewayBackendPolicySelectorFixture(t *testing.T) (*gatewayBackendChartFixture, *RenderResult) {
	t.Helper()
	fixture := newGatewayBackendChartFixture(t)
	fixture.add(t, fixture.gateways, gatewayBackendGateway("gateway"), "default", "gateway")
	fixture.add(t, fixture.services, sslPassthroughService("echo", "http", 80), "default", "echo")
	fixture.add(t, fixture.services, sslPassthroughService("other", "http", 80), "default", "other")
	fixture.add(t, fixture.endpoints, sslPassthroughEndpoint("echo", "http", 8080, "10.0.0.1"), "default", "echo")
	fixture.add(t, fixture.endpoints, sslPassthroughEndpoint("other", "http", 8080, "10.0.0.2"), "default", "other")
	fixture.add(t, fixture.httpRoutes, gatewayBackendRoute("HTTPRoute", "http-a", "echo"), "default", "http-a")
	fixture.add(t, fixture.httpRoutes, gatewayBackendRoute("HTTPRoute", "http-b", "other"), "default", "http-b")
	fixture.add(t, fixture.grpcRoutes, gatewayBackendRoute("GRPCRoute", "grpc-a", "echo"), "default", "grpc-a")
	baseline := fixture.renderAndCommitCacheReady(t)
	assert.NotContains(t, baseline.HAProxyConfig, "sni str(")
	return fixture, baseline
}

func TestGatewayBackendChartHTTPRouteExecutionScaling(t *testing.T) {
	for _, routeCount := range []int{300, 1000, 3000} {
		t.Run(fmt.Sprintf("routes=%d", routeCount), func(t *testing.T) {
			fixture := newGatewayBackendChartFixture(t)
			fixture.add(t, fixture.gateways, gatewayBackendGateway("gateway"), "default", "gateway")
			fixture.add(t, fixture.services, sslPassthroughService("echo", "http", 80), "default", "echo")
			fixture.add(t, fixture.services, sslPassthroughService("other", "http", 80), "default", "other")
			fixture.add(t, fixture.endpoints, sslPassthroughEndpoint("echo", "http", 8080, "10.0.0.1"), "default", "echo")
			fixture.add(t, fixture.endpoints, sslPassthroughEndpoint("other", "http", 8080, "10.0.0.2"), "default", "other")
			for index := range routeCount {
				name := fmt.Sprintf("route-%06d", index)
				fixture.add(t, fixture.httpRoutes, gatewayBackendRoute("HTTPRoute", name, "echo"), "default", name)
			}

			cold := fixture.renderAndCommitCacheReady(t)
			assert.Contains(t, cold.HAProxyConfig, "backend gw_h_default_route-000000_echo_80")
			assert.Contains(t, cold.HAProxyConfig, fmt.Sprintf("backend gw_h_default_route-%06d_echo_80", routeCount-1))
			coldCounts := fixture.engine.executionCounts()
			require.Len(t, coldCounts, routeCount)

			warm := fixture.renderAndCommitCacheReady(t)
			assert.Equal(t, cold.HAProxyConfig, warm.HAProxyConfig)
			assert.Equal(t, coldCounts, fixture.engine.executionCounts())

			fixture.update(t, fixture.httpRoutes,
				gatewayBackendRoute("HTTPRoute", "route-000000", "other"),
				"default", "route-000000",
			)
			changed := fixture.renderAndCommitCacheReady(t)
			assert.Contains(t, changed.HAProxyConfig, "backend gw_h_default_route-000000_other_80")
			assert.NotContains(t, changed.HAProxyConfig, "backend gw_h_default_route-000000_echo_80")
			changedCounts := fixture.engine.executionCounts()
			assert.Equal(t, coldCounts["httproutes/route-000000"]+1, changedCounts["httproutes/route-000000"])
			assert.Equal(t, coldCounts[fmt.Sprintf("httproutes/route-%06d", routeCount-1)],
				changedCounts[fmt.Sprintf("httproutes/route-%06d", routeCount-1)])
		})
	}
}

func TestGatewayBackendChartSameNameRouteKindsRemainIndependent(t *testing.T) {
	fixture := newGatewayBackendChartFixture(t)
	fixture.add(t, fixture.gateways, gatewayBackendGateway("gateway"), "default", "gateway")
	fixture.add(t, fixture.services, sslPassthroughService("echo", "http", 80), "default", "echo")
	fixture.add(t, fixture.endpoints, sslPassthroughEndpoint("echo", "http", 8080, "10.0.0.1"), "default", "echo")
	fixture.add(t, fixture.httpRoutes, gatewayBackendRoute("HTTPRoute", "same", "echo"), "default", "same")
	fixture.add(t, fixture.grpcRoutes, gatewayBackendRoute("GRPCRoute", "same", "echo"), "default", "same")

	both := fixture.renderAndCommitCacheReady(t)
	assert.Contains(t, both.HAProxyConfig, "# Backend for: HTTPRoute default/same")
	assert.Contains(t, both.HAProxyConfig, "# Backend for: GRPCRoute default/same")
	require.Equal(t, 1, strings.Count(both.HAProxyConfig, "backend gw_h_default_same_echo_80 "))
	require.Equal(t, 1, strings.Count(both.HAProxyConfig, "backend gw_g_default_same_echo_80 "))

	fixture.delete(t, fixture.httpRoutes, "same", "default", "same")
	remaining := fixture.renderAndCommitCacheReady(t)
	assert.NotContains(t, remaining.HAProxyConfig, "# Backend for: HTTPRoute default/same")
	assert.Contains(t, remaining.HAProxyConfig, "# Backend for: GRPCRoute default/same")
	assert.NotContains(t, remaining.HAProxyConfig, "backend gw_h_default_same_echo_80 ")
	require.Equal(t, 1, strings.Count(remaining.HAProxyConfig, "backend gw_g_default_same_echo_80 "))
	fixture.assertExecutions(t, gatewayGRPCBackendComponent, "grpcroutes", "same", 1)
}

func TestGatewayBackendChartAbortAdmissionAndConcurrencyStayIsolated(t *testing.T) {
	fixture := newGatewayBackendChartFixture(t)
	fixture.add(t, fixture.gateways, gatewayBackendGateway("gateway"), "default", "gateway")
	fixture.add(t, fixture.services, sslPassthroughService("echo", "http", 80), "default", "echo")
	fixture.add(t, fixture.services, sslPassthroughService("other", "http", 80), "default", "other")
	fixture.add(t, fixture.endpoints, sslPassthroughEndpoint("echo", "http", 8080, "10.0.0.1"), "default", "echo")
	fixture.add(t, fixture.endpoints, sslPassthroughEndpoint("other", "http", 8080, "10.0.0.2"), "default", "other")
	fixture.add(t, fixture.httpRoutes, gatewayBackendRoute("HTTPRoute", "subject", "echo"), "default", "subject")
	fixture.add(t, fixture.httpRoutes, gatewayBackendRoute("HTTPRoute", "stable", "echo"), "default", "stable")
	baseline := fixture.renderAndCommitCacheReady(t)

	results := make([]*RenderResult, 2)
	errors := make([]error, 2)
	var wait sync.WaitGroup
	for index := range results {
		wait.Add(1)
		go func() {
			defer wait.Done()
			results[index], errors[index] = fixture.service.Render(
				t.Context(), fixture.provider, rendercontext.RenderModeReconcile,
			)
		}()
	}
	wait.Wait()
	for index := range results {
		require.NoError(t, errors[index])
		assert.Equal(t, baseline.HAProxyConfig, results[index].HAProxyConfig)
		require.NoError(t, results[index].InputTransaction.Commit(t.Context()))
	}
	fixture.assertExecutions(t, gatewayHTTPBackendComponent, "httproutes", "subject", 1)
	fixture.assertExecutions(t, gatewayHTTPBackendComponent, "httproutes", "stable", 1)

	proposed := gatewayBackendRoute("HTTPRoute", "subject", "other")
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
	assert.Contains(t, admission.HAProxyConfig, "backend gw_h_default_subject_other_80")
	assert.Contains(t, admission.HAProxyConfig, "backend gw_h_default_stable_echo_80")
	admission.InputTransaction.Abort()
	fixture.assertExecutions(t, gatewayHTTPBackendComponent, "httproutes", "subject", 1)
	fixture.assertExecutions(t, gatewayHTTPBackendComponent, "httproutes", "stable", 1)
	assert.Equal(t, baseline.HAProxyConfig, fixture.renderAndCommitCacheReady(t).HAProxyConfig)

	fixture.update(t, fixture.httpRoutes, proposed, "default", "subject")
	fixture.config.TemplatingSettings.ExtraContext["failAfterBackends"] = true
	failed, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.ErrorContains(t, err, "forced failure after gateway backends")
	assert.Nil(t, failed)
	fixture.assertExecutions(t, gatewayHTTPBackendComponent, "httproutes", "subject", 1)
	fixture.assertExecutions(t, gatewayHTTPBackendComponent, "httproutes", "stable", 1)

	fixture.config.TemplatingSettings.ExtraContext["failAfterBackends"] = false
	retried := fixture.renderAndCommitCacheReady(t)
	assert.Contains(t, retried.HAProxyConfig, "backend gw_h_default_subject_other_80")
	assert.NotContains(t, retried.HAProxyConfig, "backend gw_h_default_subject_echo_80")
	fixture.assertExecutions(t, gatewayHTTPBackendComponent, "httproutes", "subject", 2)
	fixture.assertExecutions(t, gatewayHTTPBackendComponent, "httproutes", "stable", 1)
}

func TestGatewayBackendChartColdMatchesDetachedHEADGenerator(t *testing.T) {
	baselinePath := os.Getenv(gatewayBackendBaselineEnv)
	if baselinePath == "" {
		t.Skip("run scripts/test-gateway-backend-differential.sh to compare against detached HEAD")
	}

	current := newGatewayBackendChartFixtureWithTemplates(
		t, loadGatewayBackendChartSnippets(t), gatewayBackendLegacyChartRoot,
	)
	baselineSnippets := loadGatewayBackendLegacySnippets(t, baselinePath)
	baseline := newGatewayBackendChartFixtureWithTemplates(t, baselineSnippets, gatewayBackendLegacyChartRoot)
	populateGatewayBackendDifferentialFixture(t, current)
	populateGatewayBackendDifferentialFixture(t, baseline)

	currentResult := current.renderAndCommitCacheReady(t)
	baselineResult := baseline.renderAndCommitCacheReady(t)
	assert.Equal(t, baselineResult.HAProxyConfig, currentResult.HAProxyConfig, "haproxy.cfg bytes")
	assert.Equal(t, requireAuxiliaryFiles(t, baselineResult), requireAuxiliaryFiles(t, currentResult), "auxiliary files")
	assert.Equal(t, requireRenderPlan(t, baselineResult), requireRenderPlan(t, currentResult), "canonical render plan")
	assert.Equal(t, baselineResult.PlanID, currentResult.PlanID, "canonical render plan ID")
	assert.Equal(t, materializedStatusPatches(t, baselineResult), materializedStatusPatches(t, currentResult), "status patches")
	assert.Equal(t, requireRenderEvents(t, baselineResult), requireRenderEvents(t, currentResult), "events")
	assert.Equal(t, requireRenderedResources(t, baselineResult), requireRenderedResources(t, currentResult), "rendered resources")
	assert.Equal(t, baselineResult.AuxFileCount, currentResult.AuxFileCount, "auxiliary file count")
}

func populateGatewayBackendDifferentialFixture(t *testing.T, fixture *gatewayBackendChartFixture) {
	t.Helper()
	gateway := gatewayBackendGateway("gateway")
	gatewaySpec := gateway["spec"].(map[string]any)
	gatewaySpec["tls"] = map[string]any{"backend": map[string]any{
		"clientCertificateRef": map[string]any{"group": "", "kind": "Secret", "name": "client-cert"},
	}}
	fixture.add(t, fixture.gateways, gateway, "default", "gateway")

	echo := sslPassthroughService("echo", "http", 80)
	echoPort := echo["spec"].(map[string]any)["ports"].([]any)[0].(map[string]any)
	echoPort["appProtocol"] = "kubernetes.io/h2c"
	fixture.add(t, fixture.services, echo, "default", "echo")
	fixture.add(t, fixture.services, sslPassthroughService("grpc", "grpc", 9090), "default", "grpc")
	fixture.add(t, fixture.endpoints, sslPassthroughEndpoint("echo", "http", 8080, "10.0.0.1"), "default", "echo")
	fixture.add(t, fixture.endpoints, sslPassthroughEndpoint("grpc", "grpc", 9090, "10.0.0.2"), "default", "grpc")

	httpRoute := gatewayBackendRoute("HTTPRoute", "http-route", "echo")
	httpRule := httpRoute["spec"].(map[string]any)["rules"].([]any)[0].(map[string]any)
	httpRule["retry"] = map[string]any{"attempts": int64(3), "codes": []any{int64(500), int64(503)}}
	httpRule["sessionPersistence"] = map[string]any{
		"type": "Cookie", "sessionName": "HTTPSESSION", "absoluteTimeout": "1h", "idleTimeout": "10m",
	}
	fixture.add(t, fixture.httpRoutes, httpRoute, "default", "http-route")

	grpcRoute := gatewayBackendRoute("GRPCRoute", "grpc-route", "grpc")
	grpcRef := grpcRoute["spec"].(map[string]any)["rules"].([]any)[0].(map[string]any)["backendRefs"].([]any)[0].(map[string]any)
	grpcRef["port"] = int64(9090)
	grpcRule := grpcRoute["spec"].(map[string]any)["rules"].([]any)[0].(map[string]any)
	grpcRule["sessionPersistence"] = map[string]any{"type": "Cookie", "sessionName": "GRPCSESSION"}
	fixture.add(t, fixture.grpcRoutes, grpcRoute, "default", "grpc-route")

	fixture.add(t, fixture.configMaps, gatewayBackendConfigMap("ca", "fixed-ca-bundle"), "default", "ca")
	fixture.add(t, fixture.secrets, gatewayBackendSecret("client-cert", "Y2VydA==", "a2V5"), "default", "client-cert")
	fixture.add(t, fixture.backendTLSPolicies,
		gatewayBackendTLSPolicy("winner", "2024-01-01T00:00:00Z", "winner.example", "ca"),
		"default", "winner",
	)
	fixture.add(t, fixture.backendTLSPolicies,
		gatewayBackendTLSPolicy("loser", "2024-01-02T00:00:00Z", "loser.example", ""),
		"default", "loser",
	)
}

func newGatewayBackendChartFixture(t *testing.T) *gatewayBackendChartFixture {
	t.Helper()
	return newGatewayBackendChartFixtureWithTemplates(t, loadGatewayBackendChartSnippets(t), gatewayBackendChartRoot)
}

func newGatewayBackendChartFixtureWithTemplates(
	t *testing.T,
	snippets map[string]config.TemplateSnippet,
	root string,
) *gatewayBackendChartFixture {
	t.Helper()
	cfg := &config.Config{
		Dataplane: testDataplaneConfig(),
		TemplatingSettings: config.TemplatingSettings{ExtraContext: map[string]any{
			"dynamicCookieKey": "test-key", "failAfterBackends": false,
		}},
		WatchedResources: map[string]config.WatchedResource{
			"gateways":           {APIVersion: "gateway.networking.k8s.io/v1", Resources: "gateways", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"httproutes":         {APIVersion: "gateway.networking.k8s.io/v1", Resources: "httproutes", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"grpcroutes":         {APIVersion: "gateway.networking.k8s.io/v1", Resources: "grpcroutes", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"backendtlspolicies": {APIVersion: "gateway.networking.k8s.io/v1", Resources: "backendtlspolicies", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"services":           {APIVersion: "v1", Resources: "services", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"endpoints":          {APIVersion: "discovery.k8s.io/v1", Resources: "endpointslices", IndexBy: []string{"metadata.namespace", "metadata.labels.kubernetes\\.io/service-name"}},
			"secrets":            {APIVersion: "v1", Resources: "secrets", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"configmaps":         {APIVersion: "v1", Resources: "configmaps", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"referencegrants":    {APIVersion: "gateway.networking.k8s.io/v1", Resources: "referencegrants", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"namespaces":         {APIVersion: "v1", Resources: "namespaces", IndexBy: []string{"metadata.name"}},
		},
		TemplateSnippets: snippets,
		HAProxyConfig:    config.HAProxyConfig{Template: root},
	}
	require.NoError(t, config.ValidateTemplateStructure(cfg))
	types := gatewayBackendSchemaTypes(t)
	declarations := helpers.BuildAdditionalDeclarations(cfg, types)
	baseEngine, err := helpers.NewEngineFromConfigWithOptions(cfg, nil, nil, declarations, helpers.EngineOptions{})
	require.NoError(t, err)
	engine := newDynamicBindingCountingEngine(t, baseEngine)
	service := NewRenderService(&RenderServiceConfig{
		Engine: engine, Config: cfg, Logger: slog.Default(), Capabilities: defaultCapabilities(),
		TypedResourceTypes: types.Types,
	})
	fixture := &gatewayBackendChartFixture{
		config: cfg, service: service, engine: engine,
		gateways: k8sstore.NewMemoryStore(2), httpRoutes: k8sstore.NewMemoryStore(2),
		grpcRoutes: k8sstore.NewMemoryStore(2), backendTLSPolicies: k8sstore.NewMemoryStore(2),
		services: k8sstore.NewMemoryStore(2), endpoints: k8sstore.NewMemoryStore(2),
		secrets: k8sstore.NewMemoryStore(2), configMaps: k8sstore.NewMemoryStore(2),
		referenceGrants: k8sstore.NewMemoryStore(2), namespaces: k8sstore.NewMemoryStore(1),
	}
	fixture.provider = stores.NewRealStoreProvider(map[string]stores.Store{
		"gateways": fixture.gateways, "httproutes": fixture.httpRoutes, "grpcroutes": fixture.grpcRoutes,
		"backendtlspolicies": fixture.backendTLSPolicies, "services": fixture.services,
		"endpoints": fixture.endpoints, "secrets": fixture.secrets, "configmaps": fixture.configMaps,
		"referencegrants": fixture.referenceGrants, "namespaces": fixture.namespaces,
	})
	return fixture
}

func gatewayBackendSchemaTypes(t *testing.T) *typebootstrap.Result {
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
			{Name: "backendtlspolicies", GVK: schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "BackendTLSPolicy"}},
			{Name: "referencegrants", GVK: schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "ReferenceGrant"}},
			{Name: "services", GVK: schema.GroupVersionKind{Version: "v1", Kind: "Service"}},
			{Name: "endpoints", GVK: schema.GroupVersionKind{Group: "discovery.k8s.io", Version: "v1", Kind: "EndpointSlice"}},
			{Name: "secrets", GVK: schema.GroupVersionKind{Version: "v1", Kind: "Secret"}},
			{Name: "configmaps", GVK: schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}},
			{Name: "namespaces", GVK: schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}},
		},
		Fetcher: fetcher,
		Logger:  slog.Default(),
	})
	require.NoError(t, err)
	require.Empty(t, result.Errors)
	require.Len(t, result.Types, 10)
	return result
}

func loadGatewayBackendChartSnippets(t *testing.T) map[string]config.TemplateSnippet {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	chartRoot := filepath.Join(filepath.Dir(sourceFile), "..", "..", "..", "charts", "haptic", "charts")
	files := []string{
		"base/library.yaml", "kubernetes-backends/library.yaml",
		"gateway/20-route-analysis.yaml", "gateway/21-route-helpers.yaml", "gateway/30-backends.yaml",
		"gateway/73-status-policy.yaml",
	}
	wanted := map[string]bool{
		"util-backend": true, "util-macros": true, "util-backend-timeouts": true, "util-haproxy-duration": true,
		"util-webhook-reject-or-warn": true, "util-config-injection-kind": true,
		"util-escape-dquote-value": true, "util-escape-logformat-value": true,
		"util-backend-servers-helpers": true,
		"util-backend-servers-result":  true, "util-backend-servers": true,
		"util-gated-pod-routable":        true,
		"util-backend-name-gateway":      true,
		"util-backend-name-gateway-rule": true,
		"util-gateway-route-identity":    true,
		"util-reference-grant-permitted": true, "util-backend-ref-valid": true,
		"util-bounded-name":                        true,
		"util-resolve-backend-tls":                 true,
		"util-backend-tls-identity":                true,
		"util-gateway-retry-policy":                true,
		"util-gateway-session-persistence":         true,
		"util-generate-httproute-backends-gateway": true,
		"util-generate-grpcroute-backends-gateway": true,
		"backendtlsvalues-490-gateway":             true, "util-gateway-backend-bindings": true,
		"util-gateway-http-backend-bindings": true, "util-gateway-grpc-backend-bindings": true,
		"util-replay-gateway-backend-effects": true,
		"util-gateway-client-certificate":     true, "util-gateway-accepts-route": true, "util-gateway-route-client-cert": true,
		"util-listenerset-routing-gate": true, "util-hostname-intersect-gateway": true,
		gatewayHTTPBackendComponent: true, gatewayGRPCBackendComponent: true,
		"backends-500-gateway": true,
	}
	result := make(map[string]config.TemplateSnippet, len(wanted))
	for _, relativePath := range files {
		content, err := os.ReadFile(filepath.Join(chartRoot, relativePath))
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
					Source: chartSnippet.Incremental.Source, BindingsTemplate: chartSnippet.Incremental.BindingsTemplate,
					Group: chartSnippet.Incremental.Group, Consumes: chartSnippet.Incremental.Consumes,
					OptionalConsumes: chartSnippet.Incremental.OptionalConsumes,
					Effects:          chartSnippet.Incremental.Effects,
				}
			}
			result[name] = snippet
		}
	}
	require.Len(t, result, len(wanted))
	result["util-gateway-analysis"] = config.TemplateSnippet{Name: "util-gateway-analysis", Template: ""}
	return result
}

func loadGatewayBackendLegacySnippets(t *testing.T, baselinePath string) map[string]config.TemplateSnippet {
	t.Helper()
	result := loadGatewayBackendChartSnippets(t)
	for _, name := range []string{
		"backendtlsvalues-490-gateway",
		"util-gateway-backend-bindings",
		"util-gateway-http-backend-bindings",
		"util-gateway-grpc-backend-bindings",
		"util-replay-gateway-backend-effects",
		gatewayHTTPBackendComponent,
		gatewayGRPCBackendComponent,
	} {
		delete(result, name)
	}

	content, err := os.ReadFile(filepath.Clean(baselinePath))
	require.NoError(t, err)
	var library ingressBackendChartLibrary
	require.NoError(t, yaml.Unmarshal(content, &library))
	for _, name := range []string{
		"util-generate-httproute-backends-gateway",
		"util-generate-grpcroute-backends-gateway",
		"util-sharded-gateway-backends",
		"backends-500-gateway",
	} {
		chartSnippet, found := library.TemplateSnippets[name]
		require.Truef(t, found, "detached baseline is missing %s", name)
		result[name] = config.TemplateSnippet{Name: name, Template: chartSnippet.Template, Requires: chartSnippet.Requires}
	}

	statusContent, err := os.ReadFile(filepath.Join(filepath.Dir(baselinePath), "73-status-policy.yaml"))
	require.NoError(t, err)
	var statusLibrary ingressBackendChartLibrary
	require.NoError(t, yaml.Unmarshal(statusContent, &statusLibrary))
	const legacyPolicySeam = "util-backendtlspolicies-contrib"
	policySeam, found := statusLibrary.TemplateSnippets[legacyPolicySeam]
	require.Truef(t, found, "detached baseline is missing %s", legacyPolicySeam)
	result[legacyPolicySeam] = config.TemplateSnippet{
		Name: legacyPolicySeam, Template: policySeam.Template, Requires: policySeam.Requires,
	}
	result["util-gateway-analysis"] = config.TemplateSnippet{Name: "util-gateway-analysis", Template: ""}
	return result
}

func gatewayBackendGateway(name string) map[string]any {
	return map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1", "kind": "Gateway",
		"metadata": map[string]any{"namespace": "default", "name": name},
		"spec": map[string]any{"gatewayClassName": "haptic", "listeners": []any{
			map[string]any{"name": "http", "protocol": "HTTP", "port": int64(80)},
		}},
	}
}

func gatewayBackendRoute(kind, name, service string) map[string]any {
	return map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1", "kind": kind,
		"metadata": map[string]any{"namespace": "default", "name": name},
		"spec": map[string]any{
			"parentRefs": []any{map[string]any{"name": "gateway"}},
			"rules": []any{map[string]any{"backendRefs": []any{map[string]any{
				"group": "", "kind": "Service", "name": service, "port": int64(80),
			}}}},
		},
	}
}

func gatewayBackendSecret(name, certificate, key string) map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"namespace": "default", "name": name},
		"data":     map[string]any{"tls.crt": certificate, "tls.key": key},
	}
}

func gatewayBackendConfigMap(name, ca string) map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"namespace": "default", "name": name},
		"data":     map[string]any{"ca.crt": ca},
	}
}

func gatewayBackendTLSPolicy(name, creationTimestamp, hostname, caConfigMap string) map[string]any {
	validation := map[string]any{"hostname": hostname}
	if caConfigMap == "" {
		validation["wellKnownCACertificates"] = "System"
	} else {
		validation["caCertificateRefs"] = []any{map[string]any{
			"group": "", "kind": "ConfigMap", "name": caConfigMap,
		}}
	}
	return map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1", "kind": "BackendTLSPolicy",
		"metadata": map[string]any{
			"namespace": "default", "name": name, "creationTimestamp": creationTimestamp,
		},
		"spec": map[string]any{
			"targetRefs": []any{map[string]any{
				"group": "", "kind": "Service", "name": "echo",
			}},
			"validation": validation,
		},
	}
}

func (f *gatewayBackendChartFixture) add(
	t *testing.T,
	store *k8sstore.MemoryStore,
	resource map[string]any,
	index ...string,
) {
	t.Helper()
	require.NoError(t, store.Add(resource, index))
}

func (f *gatewayBackendChartFixture) update(
	t *testing.T,
	store *k8sstore.MemoryStore,
	resource map[string]any,
	index ...string,
) {
	t.Helper()
	require.NoError(t, store.Update(resource, index))
}

func (f *gatewayBackendChartFixture) delete(
	t *testing.T,
	store *k8sstore.MemoryStore,
	name string,
	index ...string,
) {
	t.Helper()
	require.NoError(t, store.Delete("default", name, index))
}

func (f *gatewayBackendChartFixture) renderAndCommitCacheReady(t *testing.T) *RenderResult {
	t.Helper()
	result, err := f.service.Render(t.Context(), f.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, result.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, f.service)
	return result
}

func (f *gatewayBackendChartFixture) executions(componentName, source, name string) uint64 {
	component := f.service.incremental.components[componentName]
	query := componentQueryKey(&component, source, "default", name)
	return f.service.incremental.graph.Counters(query).Executions
}

func (f *gatewayBackendChartFixture) assertExecutions(
	t *testing.T,
	componentName, source, name string,
	want uint64,
) {
	t.Helper()
	assert.Equal(t, want, f.executions(componentName, source, name), componentName+"/"+name)
}

func gatewayBackendGeneralFile(t *testing.T, result *RenderResult, name string) string {
	t.Helper()
	for _, file := range requireAuxiliaryFiles(t, result).GeneralFiles {
		if file.Filename == name || strings.HasSuffix(file.Path, "/"+name) {
			return file.Content
		}
	}
	return ""
}
