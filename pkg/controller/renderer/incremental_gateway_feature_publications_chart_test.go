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
	"context"
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
)

const (
	gatewayTypedAccessComponent           = "global-settings-001-typed-access-smoke"
	gatewayListenerStateComponent         = "gateway-listener-state-100-gateway"
	gatewayTLSCertificateComponent        = "gateway-tls-certificates-100-gateway"
	gatewayTLSPermissionsComponent        = "gateway-tls-permissions-100-gateway"
	gatewayFrontendMTLSComponent          = "gateway-frontend-mtls-100-gateway"
	gatewayFrontendCAPermissionsComponent = "gateway-frontend-ca-permissions-100-gateway"
	gatewayListenerSetPortsComponent      = "gateway-listenerset-service-ports-100-listenerset"
	gatewayRouteCandidateGRPCComponent    = "gateway-route-candidates-200-grpc"
)

const gatewayFeaturePublicationRoot = `{{- render "global-settings-001-typed-access-smoke" -}}
{{- render "gateway-listener-state-100-gateway" -}}
{{- render "gateway-tls-permissions-100-gateway" default "" -}}
{{- render "gateway-tls-certificates-100-gateway" -}}
{{- render "gateway-frontend-ca-permissions-100-gateway" -}}
{{- render "gateway-frontend-mtls-100-gateway" -}}
{{- render "gateway-listenerset-service-ports-100-listenerset" -}}
{{- render "map-hostvalues-479-gateway-listenersets-empty" -}}
{{- render "map-hostvalues-480-gateway-listenersets" default "" -}}
{{- render "map-hostvalues-490-gateway-port-scopes" -}}
{{- render "gateway-route-candidates-100-http" -}}
{{- render "gateway-route-candidates-200-grpc" -}}
# listeners={{ incremental_values("gateway-listener-state", "listeners") | toJSON() }}
# certificates={{ incremental_values("gateway-tls-certificates", "occurrences") | toJSON() }}
# mtls={{ incremental_values("gateway-frontend-mtls", "gateways") | toJSON() }}
# listenerset-ports={{ incremental_values("gateway-listenerset-service-ports", "ports") | toJSON() }}
# http-routes={{ incremental_values("gateway-route-candidates", "presence-http") | toJSON() }}
# grpc-routes={{ incremental_values("gateway-route-candidates", "presence-grpc") | toJSON() }}
{%- if tostring(extraContext | dig("failAfterGatewayFeatures") | fallback(false)) == "true" -%}
{{ fail("forced failure after Gateway feature publications") }}
{%- end -%}`

const gatewayFeatureProjectionRoot = `{{- render "global-settings-001-typed-access-smoke" -}}
{{- render "gateway-listener-state-100-gateway" -}}
{{- render "gateway-tls-permissions-100-gateway" default "" -}}
{{- render "gateway-tls-certificates-100-gateway" -}}
{{- render "gateway-frontend-ca-permissions-100-gateway" -}}
{{- render "gateway-frontend-mtls-100-gateway" -}}
{{- render "gateway-listenerset-service-ports-100-listenerset" -}}
{{- render "map-hostvalues-479-gateway-listenersets-empty" -}}
{{- render "map-hostvalues-480-gateway-listenersets" default "" -}}
{{- render "map-hostvalues-490-gateway-port-scopes" -}}
{{- render "gateway-route-candidates-100-http" -}}
{{- render "gateway-route-candidates-200-grpc" -}}
# listeners={{ incremental_values("gateway-listener-state", "listeners") | toJSON() }}
# certificates={{ incremental_values("gateway-tls-certificates", "occurrences") | toJSON() }}
# mtls={{ incremental_values("gateway-frontend-mtls", "gateways") | toJSON() }}
# listenerset-ports={{ incremental_values("gateway-listenerset-service-ports", "ports") | toJSON() }}
# http-routes={{ incremental_values("gateway-route-candidates", "presence-http") | toJSON() }}
# grpc-routes={{ incremental_values("gateway-route-candidates", "presence-grpc") | toJSON() }}`

const gatewayFeatureResolutionRoot = `{{- render "global-settings-001-typed-access-smoke" -}}
{{- render "gateway-tls-permissions-100-gateway" default "" -}}
{{- render "gateway-tls-certificates-100-gateway" -}}
{{- render "gateway-frontend-ca-permissions-100-gateway" -}}
{{- render "gateway-frontend-mtls-100-gateway" -}}
{{- render "gateway-listener-state-100-gateway" -}}
{{- render "gateway-listenerset-service-ports-100-listenerset" -}}
{{- render "map-hostvalues-479-gateway-listenersets-empty" -}}
{{- render "map-hostvalues-480-gateway-listenersets" default "" -}}
{{- render "map-hostvalues-490-gateway-port-scopes" -}}
{{- render "gateway-route-candidates-100-http" -}}
{{- render "gateway-route-candidates-200-grpc" -}}
# certificates={{ incremental_values("gateway-tls-certificates", "resolved") | toJSON() }}
# mtls={{ incremental_values("gateway-frontend-mtls", "resolutions") | toJSON() }}
# gateway-resources={{ incremental_values("gateway-listener-state", "resources") | toJSON() }}`

const gatewayFeatureLegacyProjectionRoot = `{%- import "util-reference-grant-permitted" for ReferenceGrantPermitted -%}
{%- for _, gateway := range resources.gateways.List() %}
# typed-access-smoke: ns={{ gateway.Metadata.Namespace }} name={{ gateway.Metadata.Name }}
{% end %}
{%%
  var listeners = []any{}
  var certificates = []any{}
  var mtls = []any{}
  for _, gateway := range resources.gateways.List() {
    var namespace = gateway.Metadata.Namespace
    var name = gateway.Metadata.Name
    for listenerIndex, listener := range gateway.Spec.Listeners {
      var certificateRefs = []any{}
      for _, certificateRef := range listener.Tls.CertificateRefs {
        var certificateRefKind = certificateRef.Kind
        if certificateRefKind == "" { certificateRefKind = "Secret" }
        var certificateRefNamespace = certificateRef.Namespace
        if certificateRefNamespace == "" { certificateRefNamespace = namespace }
        certificateRefs = append(certificateRefs, map[string]any{
          "group": certificateRef.Group,
          "kind": certificateRefKind,
          "name": certificateRef.Name,
          "namespace": certificateRefNamespace,
        })
      }
      var tlsOptions = map[string]any{}
      for option, value := range listener.Tls.Options { tlsOptions[option] = value }
      listeners = append(listeners, map[string]any{
        "gatewayNamespace": namespace,
        "gatewayName": name,
        "gatewayHasAddresses": len(gateway.Spec.Addresses) > 0,
        "listenerIndex": listenerIndex,
        "name": listener.Name,
        "hostname": listener.Hostname,
        "port": listener.Port,
        "protocol": listener.Protocol,
        "tls": map[string]any{
          "mode": listener.Tls.Mode,
          "options": tlsOptions,
          "certificateRefs": certificateRefs,
        },
      })
      var tlsMode = listener.Tls.Mode
      if tlsMode == "" || tlsMode == "Terminate" {
        for _, certificateRef := range listener.Tls.CertificateRefs {
          var kind = certificateRef.Kind
          if kind == "" { kind = "Secret" }
          if kind != "Secret" { continue }
          var secretNamespace = certificateRef.Namespace
          if secretNamespace == "" { secretNamespace = namespace }
          if secretNamespace != namespace && ReferenceGrantPermitted(
            "gateway.networking.k8s.io", "Gateway", namespace,
            "", "Secret", secretNamespace, certificateRef.Name) != "true" { continue }
          certificates = append(certificates, map[string]any{
            "secret_namespace": secretNamespace,
            "secret_name": certificateRef.Name,
            "listener_hostname": listener.Hostname,
            "listener_port": listener.Port,
          })
        }
      }
    }
    if dig(gateway, "spec", "tls", "frontend") != nil {
      var detachValidation = func(validation any) any {
        if validation == nil { return nil }
        var refs = []any{}
        for _, ref := range toSlice(dig(validation, "caCertificateRefs")) {
          refs = append(refs, map[string]any{
            "group": dig_string(ref, "", "group"),
            "kind": dig_string(ref, "", "kind"),
            "name": dig_string(ref, "", "name"),
            "namespace": dig_string(ref, "", "namespace"),
          })
        }
        return map[string]any{
          "mode": dig_string(validation, "", "mode"),
          "caCertificateRefs": refs,
        }
      }
      var gatewayListeners = []any{}
      for _, listener := range gateway.Spec.Listeners {
        gatewayListeners = append(gatewayListeners, map[string]any{
          "name": listener.Name, "hostname": listener.Hostname,
          "port": listener.Port, "protocol": listener.Protocol, "tls": map[string]any{"mode": listener.Tls.Mode},
        })
      }
      var perPort = []any{}
      for _, entry := range toSlice(dig(gateway, "spec", "tls", "frontend", "perPort")) {
        perPort = append(perPort, map[string]any{
          "port": toint(dig(entry, "port") | fallback(0)),
          "tls": map[string]any{
            "validation": detachValidation(dig(entry, "tls", "validation")),
          },
        })
      }
      mtls = append(mtls, map[string]any{
        "namespace": namespace,
        "name": name,
        "defaultValidation": detachValidation(
          dig(gateway, "spec", "tls", "frontend", "default", "validation")),
        "perPort": perPort,
        "listeners": gatewayListeners,
      })
    }
  }
  var listenerSetPorts = []any{}
  for _, listenerSet := range resources.listenersets.List() {
    for _, listener := range listenerSet.Spec.Listeners {
      listenerSetPorts = append(listenerSetPorts, map[string]any{
        "port": listener.Port, "protocol": listener.Protocol,
      })
    }
  }
  var httpRoutes = []any{}
  for range resources.httproutes.List() { httpRoutes = append(httpRoutes, true) }
  var grpcRoutes = []any{}
  for range resources.grpcroutes.List() { grpcRoutes = append(grpcRoutes, true) }
%%}
# listeners={{ listeners | toJSON() }}
# certificates={{ certificates | toJSON() }}
# mtls={{ mtls | toJSON() }}
# listenerset-ports={{ listenerSetPorts | toJSON() }}
# http-routes={{ httpRoutes | toJSON() }}
# grpc-routes={{ grpcRoutes | toJSON() }}`

func TestGatewayFeaturePublicationsScaleByChangedGateway(t *testing.T) {
	for _, gatewayCount := range []int{300, 1000, 3000} {
		t.Run(fmt.Sprintf("gateways=%d", gatewayCount), func(t *testing.T) {
			fixture := newGatewayFeaturePublicationFixture(t)
			for index := range gatewayCount {
				fixture.addGateway(t, gatewayFeatureGateway(fmt.Sprintf("gateway-%06d", index), 8080))
			}

			coldStarted := time.Now()
			cold := renderGatewayFeaturesAndCommit(t, fixture)
			assert.Less(t, time.Since(coldStarted), 30*time.Second)
			assert.Contains(t, cold.HAProxyConfig, "# typed-access-smoke: ns=default name=gateway-000000")
			assert.Contains(t, cold.HAProxyConfig,
				fmt.Sprintf("# typed-access-smoke: ns=default name=gateway-%06d", gatewayCount-1))

			first := "gateway-000000"
			last := fmt.Sprintf("gateway-%06d", gatewayCount-1)
			for _, component := range gatewayFeatureAlwaysActiveComponents() {
				assertGatewayFeatureExecutions(t, fixture, component, "gateways", first, 1)
				assertGatewayFeatureExecutions(t, fixture, component, "gateways", last, 1)
			}
			assertGatewayFeatureExecutions(t, fixture, gatewayFrontendMTLSComponent, "gateways", first, 0)
			assertGatewayFeatureExecutions(t, fixture, gatewayFrontendMTLSComponent, "gateways", last, 0)

			warm := renderGatewayFeaturesAndCommit(t, fixture)
			assert.Equal(t, cold.HAProxyConfig, warm.HAProxyConfig)
			for _, component := range gatewayFeatureAlwaysActiveComponents() {
				assertGatewayFeatureExecutions(t, fixture, component, "gateways", first, 1)
				assertGatewayFeatureExecutions(t, fixture, component, "gateways", last, 1)
			}

			changed := gatewayFeatureGateway(last, 8080)
			changed["metadata"].(map[string]any)["labels"] = map[string]any{"revision": "changed"}
			require.NoError(t, fixture.gateways.Update(changed, []string{"default", last}))
			updated := renderGatewayFeaturesAndCommit(t, fixture)
			assert.Equal(t, cold.HAProxyConfig, updated.HAProxyConfig)
			for _, component := range gatewayFeatureAlwaysActiveComponents() {
				assertGatewayFeatureExecutions(t, fixture, component, "gateways", first, 1)
				assertGatewayFeatureExecutions(t, fixture, component, "gateways", last, 2)
			}
			assertGatewayFeatureExecutions(t, fixture, gatewayFrontendMTLSComponent, "gateways", last, 0)
		})
	}
}

func TestGatewayFeaturePublicationColdProjectionMatchesLegacyLoops(t *testing.T) {
	current := newGatewayFeaturePublicationFixtureWithRoot(t, gatewayFeatureProjectionRoot)
	legacy := newGatewayRouteAnalysisFixtureWithTemplates(
		t,
		loadGatewayHostMapSnippets(t, gatewayHostMapChartRoot(t), map[string][]string{
			"gateway/21-route-helpers.yaml": {"util-reference-grant-permitted"},
		}),
		gatewayFeatureLegacyProjectionRoot,
	)

	httpGateway := gatewayFeatureGateway("a-http", 8080)
	current.addGateway(t, httpGateway)
	tlsGateway := gatewayFeatureGateway("b-tls", 8443)
	tlsGateway["spec"].(map[string]any)["listeners"] = []any{map[string]any{
		"name": "https", "hostname": "tls.example", "port": int64(8443), "protocol": "HTTPS",
		"tls": map[string]any{
			"mode":    "Terminate",
			"options": map[string]any{"minVersion": "TLSv1.2"},
			"certificateRefs": []any{map[string]any{
				"group": "", "kind": "Secret", "name": "server-cert",
			}},
		},
	}}
	tlsGateway["spec"].(map[string]any)["tls"] = map[string]any{"frontend": map[string]any{
		"default": map[string]any{"validation": map[string]any{
			"mode": "AllowValidOnly",
			"caCertificateRefs": []any{map[string]any{
				"group": "", "kind": "ConfigMap", "name": "client-ca",
			}},
		}},
	}}
	current.addGateway(t, tlsGateway)
	addGatewayFeatureListenerSet(t, current, gatewayHostMapListenerSet("orphan", "missing", "", 9080))
	current.addHTTPRoute(t, gatewayHostMapRoute("HTTPRoute", "http", nil))
	addGatewayFeatureGRPCRoute(t, current, gatewayHostMapRoute("GRPCRoute", "grpc", nil))
	legacy.provider = current.provider

	currentResult := renderGatewayFeaturesAndCommit(t, current)
	legacyResult := renderGatewayFeaturesAndCommit(t, legacy)
	assert.Equal(t, legacyResult.HAProxyConfig, currentResult.HAProxyConfig)
}

func TestGatewayRouteCandidatePublicationsScaleByChangedRoute(t *testing.T) {
	for _, routeCount := range []int{300, 1000, 3000} {
		t.Run(fmt.Sprintf("routes=%d", routeCount), func(t *testing.T) {
			fixture := newGatewayFeaturePublicationFixture(t)
			for index := range routeCount {
				name := fmt.Sprintf("route-%06d", index)
				fixture.addHTTPRoute(t, gatewayHostMapRoute("HTTPRoute", name, nil))
			}

			cold := renderGatewayFeaturesAndCommit(t, fixture)
			first := "route-000000"
			last := fmt.Sprintf("route-%06d", routeCount-1)
			assertGatewayFeatureExecutions(t, fixture, gatewayRouteCandidateHTTPComponent, "httproutes", first, 1)
			assertGatewayFeatureExecutions(t, fixture, gatewayRouteCandidateHTTPComponent, "httproutes", last, 1)

			warm := renderGatewayFeaturesAndCommit(t, fixture)
			assert.Equal(t, cold.HAProxyConfig, warm.HAProxyConfig)
			assertGatewayFeatureExecutions(t, fixture, gatewayRouteCandidateHTTPComponent, "httproutes", first, 1)
			assertGatewayFeatureExecutions(t, fixture, gatewayRouteCandidateHTTPComponent, "httproutes", last, 1)

			changed := gatewayHostMapRoute("HTTPRoute", last, nil)
			changed["metadata"].(map[string]any)["labels"] = map[string]any{"revision": "changed"}
			fixture.updateHTTPRoute(t, changed)
			updated := renderGatewayFeaturesAndCommit(t, fixture)
			assert.Equal(t, cold.HAProxyConfig, updated.HAProxyConfig)
			assertGatewayFeatureExecutions(t, fixture, gatewayRouteCandidateHTTPComponent, "httproutes", first, 1)
			assertGatewayFeatureExecutions(t, fixture, gatewayRouteCandidateHTTPComponent, "httproutes", last, 2)
		})
	}
}

func TestGatewayFeaturePublicationsActivationAndDeletion(t *testing.T) {
	fixture := newGatewayFeaturePublicationFixture(t)
	active := gatewayFeatureGateway("subject", 8443)
	active["spec"].(map[string]any)["tls"] = map[string]any{"frontend": map[string]any{
		"default": map[string]any{"validation": map[string]any{
			"mode": "AllowValidOnly",
			"caCertificateRefs": []any{map[string]any{
				"group": "", "kind": "ConfigMap", "name": "client-ca",
			}},
		}},
	}}
	fixture.addGateway(t, active)

	activeResult := renderGatewayFeaturesAndCommit(t, fixture)
	assert.Contains(t, activeResult.HAProxyConfig, `"name":"client-ca"`)
	assertGatewayFeatureExecutions(t, fixture, gatewayFrontendMTLSComponent, "gateways", "subject", 1)

	inactive := gatewayFeatureGateway("subject", 8443)
	require.NoError(t, fixture.gateways.Update(inactive, []string{"default", "subject"}))
	inactiveResult := renderGatewayFeaturesAndCommit(t, fixture)
	assert.Contains(t, inactiveResult.HAProxyConfig, "# mtls=[]")
	assert.NotContains(t, inactiveResult.HAProxyConfig, `"name":"client-ca"`)
	assertGatewayFeatureExecutions(t, fixture, gatewayFrontendMTLSComponent, "gateways", "subject", 0)

	require.NoError(t, fixture.gateways.Delete(
		"default", "subject", []string{"default", "subject"},
	))
	deleted := renderGatewayFeaturesAndCommit(t, fixture)
	assert.NotContains(t, deleted.HAProxyConfig, "subject")
	assert.Contains(t, deleted.HAProxyConfig, "# listeners=[]")
	assert.Contains(t, deleted.HAProxyConfig, "# certificates=[]")
	assert.Contains(t, deleted.HAProxyConfig, "# mtls=[]")

	orphan := gatewayHostMapListenerSet("orphan", "missing", "", 9080)
	addGatewayFeatureListenerSet(t, fixture, orphan)
	withListenerSet := renderGatewayFeaturesAndCommit(t, fixture)
	assert.Contains(t, withListenerSet.HAProxyConfig, `"port":9080`)
	assertGatewayFeatureExecutions(t, fixture, gatewayListenerSetPortsComponent, "listenersets", "orphan", 1)
	deleteGatewayFeatureListenerSet(t, fixture, "orphan")
	withoutListenerSet := renderGatewayFeaturesAndCommit(t, fixture)
	assert.Contains(t, withoutListenerSet.HAProxyConfig, "# listenerset-ports=[]")
}

func TestGatewayFeatureResourceDependenciesAreExactAndDetectABA(t *testing.T) {
	fixture := newGatewayFeaturePublicationFixtureWithRoot(t, gatewayFeatureResolutionRoot)
	gateway := gatewayFeatureGateway("subject", 8443)
	gateway["spec"].(map[string]any)["listeners"] = []any{map[string]any{
		"name": "https", "hostname": "subject.example.com", "port": int64(8443), "protocol": "HTTPS",
		"tls": map[string]any{
			"mode": "Terminate",
			"certificateRefs": []any{map[string]any{
				"group": "", "kind": "Secret", "name": "server-cert",
			}},
		},
	}}
	gateway["spec"].(map[string]any)["tls"] = map[string]any{"frontend": map[string]any{
		"default": map[string]any{"validation": map[string]any{
			"mode": "AllowValidOnly",
			"caCertificateRefs": []any{map[string]any{
				"group": "", "kind": "ConfigMap", "name": "client-ca",
			}},
		}},
	}}
	fixture.addGateway(t, gateway)
	addGatewayFeatureSecret(t, fixture, gatewayFeatureTLSSecret("server-cert", "CERT-A", "KEY-A", nil))
	addGatewayFeatureConfigMap(t, fixture, gatewayFeatureCAConfigMap("client-ca", "CA-A"))

	cold := renderGatewayFeatureDependenciesAndCommit(t, fixture)
	assert.Contains(t, cold.HAProxyConfig, "CERT-A")
	assert.Contains(t, cold.HAProxyConfig, "CA-A")
	assert.Contains(t, cold.HAProxyConfig, `"apiVersion":"gateway.networking.k8s.io/v1"`)
	assertGatewayFeatureExecutions(t, fixture, gatewayTLSCertificateComponent, "gateways", "subject", 1)
	assertGatewayFeatureExecutions(t, fixture, gatewayFrontendMTLSComponent, "gateways", "subject", 1)
	warm := renderGatewayFeatureDependenciesAndCommit(t, fixture)
	assertRenderResultObservablesEqual(t, cold, warm)
	assertGatewayFeatureExecutions(t, fixture, gatewayTLSCertificateComponent, "gateways", "subject", 1)
	assertGatewayFeatureExecutions(t, fixture, gatewayFrontendMTLSComponent, "gateways", "subject", 1)

	addGatewayFeatureSecret(t, fixture, gatewayFeatureTLSSecret("unrelated", "CERT-X", "KEY-X", nil))
	addGatewayFeatureConfigMap(t, fixture, gatewayFeatureCAConfigMap("unrelated", "CA-X"))
	unrelated := renderGatewayFeatureDependenciesAndCommit(t, fixture)
	assert.Equal(t, cold.HAProxyConfig, unrelated.HAProxyConfig)
	assertGatewayFeatureExecutions(t, fixture, gatewayTLSCertificateComponent, "gateways", "subject", 1)
	assertGatewayFeatureExecutions(t, fixture, gatewayFrontendMTLSComponent, "gateways", "subject", 1)

	updateGatewayFeatureSecret(t, fixture, gatewayFeatureTLSSecret(
		"server-cert", "CERT-A", "KEY-A", map[string]any{"revision": "metadata-only"},
	))
	metadataOnly := renderGatewayFeatureDependenciesAndCommit(t, fixture)
	assert.Equal(t, cold.HAProxyConfig, metadataOnly.HAProxyConfig)
	assertGatewayFeatureExecutions(t, fixture, gatewayTLSCertificateComponent, "gateways", "subject", 2)
	assertGatewayFeatureExecutions(t, fixture, gatewayFrontendMTLSComponent, "gateways", "subject", 1)

	updateGatewayFeatureSecret(t, fixture, gatewayFeatureTLSSecret("server-cert", "CERT-B", "KEY-B", nil))
	certificateChanged := renderGatewayFeatureDependenciesAndCommit(t, fixture)
	assert.Contains(t, certificateChanged.HAProxyConfig, "CERT-B")
	assert.NotContains(t, certificateChanged.HAProxyConfig, "CERT-A")
	assertGatewayFeatureExecutions(t, fixture, gatewayTLSCertificateComponent, "gateways", "subject", 3)

	updateGatewayFeatureConfigMap(t, fixture, gatewayFeatureCAConfigMap("client-ca", "CA-B"))
	caChanged := renderGatewayFeatureDependenciesAndCommit(t, fixture)
	assert.Contains(t, caChanged.HAProxyConfig, "CA-B")
	assert.NotContains(t, caChanged.HAProxyConfig, "CA-A")
	assertGatewayFeatureExecutions(t, fixture, gatewayFrontendMTLSComponent, "gateways", "subject", 2)

	require.NoError(t, fixture.secrets.Delete("default", "server-cert", []string{"default", "server-cert"}))
	deleted := renderGatewayFeatureDependenciesAndCommit(t, fixture)
	assert.Contains(t, deleted.HAProxyConfig, "# certificates=[]")
	assertGatewayFeatureExecutions(t, fixture, gatewayTLSCertificateComponent, "gateways", "subject", 4)

	addGatewayFeatureSecret(t, fixture, gatewayFeatureTLSSecret("server-cert", "CERT-B", "KEY-B", nil))
	recreated := renderGatewayFeatureDependenciesAndCommit(t, fixture)
	assert.Contains(t, recreated.HAProxyConfig, "CERT-B")
	assert.NotContains(t, recreated.HAProxyConfig, "# certificates=[]")
	assertGatewayFeatureExecutions(t, fixture, gatewayTLSCertificateComponent, "gateways", "subject", 5)
}

func assertRenderResultObservablesEqual(t *testing.T, want, got *RenderResult) {
	t.Helper()
	require.Equal(t, want.HAProxyConfig, got.HAProxyConfig)
	require.Equal(t, want.ContentChecksum, got.ContentChecksum)
	require.Equal(t, want.PlanID, got.PlanID)
	require.Equal(t, want.AuxFileCount, got.AuxFileCount)

	wantPlan, err := want.MaterializePlan()
	require.NoError(t, err)
	gotPlan, err := got.MaterializePlan()
	require.NoError(t, err)
	require.Equal(t, wantPlan, gotPlan)

	wantFiles, err := want.MaterializeAuxiliaryFiles()
	require.NoError(t, err)
	gotFiles, err := got.MaterializeAuxiliaryFiles()
	require.NoError(t, err)
	require.Equal(t, wantFiles, gotFiles)

	wantPatches, err := want.MaterializeStatusPatches()
	require.NoError(t, err)
	gotPatches, err := got.MaterializeStatusPatches()
	require.NoError(t, err)
	require.Equal(t, wantPatches, gotPatches)

	wantEvents, err := want.MaterializeEvents()
	require.NoError(t, err)
	gotEvents, err := got.MaterializeEvents()
	require.NoError(t, err)
	require.Equal(t, wantEvents, gotEvents)

	wantResources, err := want.MaterializeRenderedResources()
	require.NoError(t, err)
	gotResources, err := got.MaterializeRenderedResources()
	require.NoError(t, err)
	require.Equal(t, wantResources, gotResources)
}

func TestGatewayFeaturePublicationsAdmissionAndFailureDoNotPoisonCache(t *testing.T) {
	fixture := newGatewayFeaturePublicationFixture(t)
	fixture.config.TemplatingSettings.ExtraContext["failAfterGatewayFeatures"] = false
	fixture.addGateway(t, gatewayFeatureGateway("subject", 8080))
	baseline := renderGatewayFeaturesAndCommit(t, fixture)
	assertGatewayFeatureExecutions(t, fixture, gatewayListenerStateComponent, "gateways", "subject", 1)

	proposed := gatewayFeatureGateway("subject", 9090)
	overlay := stores.NewOverlayStoreProvider(
		fixture.provider,
		stores.NewValidationContext(map[string]*stores.StoreOverlay{
			"gateways": stores.NewStoreOverlayForUpdate(&unstructured.Unstructured{Object: proposed}),
		}),
	)
	admission, err := fixture.service.Render(
		t.Context(), overlay, rendercontext.RenderModeAdmission,
		rendercontext.WithAdmissionSubject("gateways", "default", "subject"),
	)
	require.NoError(t, err)
	assert.Contains(t, admission.HAProxyConfig, `"port":9090`)
	admission.InputTransaction.Abort()
	assert.Equal(t, baseline.HAProxyConfig, renderGatewayFeaturesAndCommit(t, fixture).HAProxyConfig)
	assertGatewayFeatureExecutions(t, fixture, gatewayListenerStateComponent, "gateways", "subject", 1)

	require.NoError(t, fixture.gateways.Update(proposed, []string{"default", "subject"}))
	fixture.config.TemplatingSettings.ExtraContext["failAfterGatewayFeatures"] = true
	failed, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.ErrorContains(t, err, "forced failure after Gateway feature publications")
	assert.Nil(t, failed)
	assertGatewayFeatureExecutions(t, fixture, gatewayListenerStateComponent, "gateways", "subject", 1)

	fixture.config.TemplatingSettings.ExtraContext["failAfterGatewayFeatures"] = false
	retried := renderGatewayFeaturesAndCommit(t, fixture)
	assert.Contains(t, retried.HAProxyConfig, `"port":9090`)
	assert.NotEqual(t, baseline.HAProxyConfig, retried.HAProxyConfig)
	assertGatewayFeatureExecutions(t, fixture, gatewayListenerStateComponent, "gateways", "subject", 2)

	oracle := newGatewayFeaturePublicationFixture(t)
	oracle.provider = fixture.provider
	assert.Equal(t, retried.HAProxyConfig, renderGatewayFeaturesAndCommit(t, oracle).HAProxyConfig)
}

func newGatewayFeaturePublicationFixture(t *testing.T) *gatewayRouteAnalysisFixture {
	t.Helper()
	return newGatewayFeaturePublicationFixtureWithRoot(t, gatewayFeaturePublicationRoot)
}

func newGatewayFeaturePublicationFixtureWithRoot(t *testing.T, root string) *gatewayRouteAnalysisFixture {
	t.Helper()
	fixture := newGatewayRouteAnalysisFixtureWithTemplates(t, loadGatewayFeaturePublicationSnippets(t), root)
	fixture.config.TemplatingSettings.ExtraContext["perGatewayPodPortRange"] = 4096
	fixture.config.TemplatingSettings.ExtraContext["failAfterGatewayFeatures"] = false
	return fixture
}

func loadGatewayFeaturePublicationSnippets(t *testing.T) map[string]config.TemplateSnippet {
	t.Helper()
	return loadGatewayHostMapSnippets(t, gatewayHostMapChartRoot(t), map[string][]string{
		"gateway/05-typed-access-smoke.yaml": {gatewayTypedAccessComponent},
		"gateway/10-features.yaml": {
			gatewayListenerStateComponent,
			gatewayTLSPermissionsComponent,
			gatewayTLSCertificateComponent,
			gatewayFrontendMTLSComponent,
			gatewayFrontendCAPermissionsComponent,
			"util-gateway-listener-mtls-bindings",
			"gateway-listener-mtls-100-gateway",
			"features-110-gateway-frontend-mtls",
			gatewayListenerSetPortsComponent,
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
			gatewayRouteCandidateGRPCComponent,
			"util-listenerset-routing-gate",
		},
		"gateway/21-route-helpers.yaml": {
			"util-resource-helpers",
			"util-hostname-intersect-gateway",
			"util-reference-grant-permitted",
			"util-gw-mtls-blocked-value",
		},
		"gateway/40-maps-host.yaml": {
			"util-gateway-host-port-suffix",
			"map-hostvalues-479-gateway-listenersets-empty",
			"map-hostvalues-480-gateway-listenersets",
			"map-hostvalues-490-gateway-port-scopes",
			"gateway-host-port-scopes-100-gateway",
		},
	})
}

func gatewayFeatureAlwaysActiveComponents() []string {
	return []string{
		gatewayTypedAccessComponent,
		gatewayListenerStateComponent,
		gatewayTLSPermissionsComponent,
		gatewayTLSCertificateComponent,
	}
}

func gatewayFeatureGateway(name string, port int64) map[string]any {
	return gatewayHostMapGateway(name, "2026-01-01T00:00:00Z", "", port)
}

func addGatewayFeatureGRPCRoute(
	t *testing.T,
	fixture *gatewayRouteAnalysisFixture,
	resource map[string]any,
) {
	t.Helper()
	name := resource["metadata"].(map[string]any)["name"].(string)
	require.NoError(t, fixture.grpcRoutes.Add(resource, []string{"default", name}))
}

func addGatewayFeatureListenerSet(
	t *testing.T,
	fixture *gatewayRouteAnalysisFixture,
	resource map[string]any,
) {
	t.Helper()
	name := resource["metadata"].(map[string]any)["name"].(string)
	require.NoError(t, fixture.listenerSets.Add(resource, []string{"default", name}))
}

func deleteGatewayFeatureListenerSet(t *testing.T, fixture *gatewayRouteAnalysisFixture, name string) {
	t.Helper()
	require.NoError(t, fixture.listenerSets.Delete("default", name, []string{"default", name}))
}

func addGatewayFeatureSecret(t *testing.T, fixture *gatewayRouteAnalysisFixture, resource map[string]any) {
	t.Helper()
	name := resource["metadata"].(map[string]any)["name"].(string)
	require.NoError(t, fixture.secrets.Add(resource, []string{"default", name}))
}

func updateGatewayFeatureSecret(t *testing.T, fixture *gatewayRouteAnalysisFixture, resource map[string]any) {
	t.Helper()
	name := resource["metadata"].(map[string]any)["name"].(string)
	require.NoError(t, fixture.secrets.Update(resource, []string{"default", name}))
}

func gatewayFeatureTLSSecret(name, certificate, key string, labels map[string]any) map[string]any {
	metadata := map[string]any{"namespace": "default", "name": name}
	if labels != nil {
		metadata["labels"] = labels
	}
	return map[string]any{
		"apiVersion": "v1", "kind": "Secret", "metadata": metadata,
		"data": map[string]any{
			"tls.crt": base64.StdEncoding.EncodeToString([]byte(
				"-----BEGIN CERTIFICATE-----\n" + certificate,
			)),
			"tls.key": base64.StdEncoding.EncodeToString([]byte(
				"-----BEGIN PRIVATE KEY-----\n" + key,
			)),
		},
	}
}

func addGatewayFeatureConfigMap(t *testing.T, fixture *gatewayRouteAnalysisFixture, resource map[string]any) {
	t.Helper()
	name := resource["metadata"].(map[string]any)["name"].(string)
	require.NoError(t, fixture.configMaps.Add(resource, []string{"default", name}))
}

func updateGatewayFeatureConfigMap(t *testing.T, fixture *gatewayRouteAnalysisFixture, resource map[string]any) {
	t.Helper()
	name := resource["metadata"].(map[string]any)["name"].(string)
	require.NoError(t, fixture.configMaps.Update(resource, []string{"default", name}))
}

func gatewayFeatureCAConfigMap(name, certificate string) map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"namespace": "default", "name": name},
		"data": map[string]any{"ca.crt": certificate},
	}
}

func assertGatewayFeatureExecutions(
	t *testing.T,
	fixture *gatewayRouteAnalysisFixture,
	componentName, source, name string,
	want uint64,
) {
	t.Helper()
	component := fixture.service.incremental.components[componentName]
	query := componentQueryKey(&component, source, "default", name)
	assert.Equal(t, want, fixture.service.incremental.graph.Counters(query).Executions, componentName+"/"+name)
}

func renderGatewayFeaturesAndCommit(t *testing.T, fixture *gatewayRouteAnalysisFixture) *RenderResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	result, err := fixture.service.Render(ctx, fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, result.InputTransaction.Commit(ctx))
	waitForIncrementalCache(t, fixture.service)
	return result
}

func renderGatewayFeatureDependenciesAndCommit(
	t *testing.T,
	fixture *gatewayRouteAnalysisFixture,
) *RenderResult {
	t.Helper()
	result := renderGatewayFeaturesAndCommit(t, fixture)
	return result
}
