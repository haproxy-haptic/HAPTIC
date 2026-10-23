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
	"maps"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
)

func TestInvalidGatewayTLSOptionsDoNotRejectUnrelatedAdmission(t *testing.T) {
	snippets := loadGatewayFeaturePublicationSnippets(t)
	maps.Copy(snippets, loadGatewayHostMapSnippets(t, gatewayHostMapChartRoot(t), map[string][]string{
		"gateway/16-crtlist-per-listener.yaml": {"gateway-terminating-listeners-100-gateway", "util-gateway-listener-crtlist-name"},
		"gateway/92-listener-tls-options.yaml": {"util-gateway-listener-tls-options"},
		"gateway/21-route-helpers.yaml":        {"util-bounded-name"},
		"base/library.yaml":                    {"util-webhook-reject-or-warn", "util-config-injection-kind"},
	}))
	fixture := newGatewayRouteAnalysisFixtureWithTemplates(t, snippets, gatewayFeatureResolutionRoot+`
{{ render "gateway-listener-mtls-100-gateway" }}
{{ render "gateway-pod-port-candidates-100-gateway" }}
{{ render "gateway-pod-port-allocations-200-leader" }}
{{ render "gateway-terminating-listeners-100-gateway" }}
# listeners={{ incremental_values("gateway-terminating-listeners", "listeners") | toJSON() }}`)
	fixture.config.TemplatingSettings.ExtraContext["perGatewayPodPortRange"] = 4096
	gateway := gatewayTLSCertificateGateway("")
	listener := gateway["spec"].(map[string]any)["listeners"].([]any)[0].(map[string]any)
	listener["tls"].(map[string]any)["options"] = map[string]any{
		"gateway.networking.k8s.io/tls-min-version": "1.3 INJECTED-KEYWORD",
	}
	fixture.addGateway(t, gateway)
	addGatewayFeatureSecret(t, fixture, gatewayFeatureTLSSecret("server-cert", "CERT", "KEY", nil))
	baseline := renderGatewayFeaturesAndCommit(t, fixture)
	require.Contains(t, baseline.HAProxyConfig, "# listeners=[]")
	require.NotEmpty(t, requireRenderEvents(t, baseline))

	for _, subject := range []struct {
		store, name string
		reject      bool
	}{
		{"gateways", "unrelated", false},
		{"secrets", "subject", false},
		{"gateways", "subject", true},
	} {
		t.Run(subject.store+"/"+subject.name, func(t *testing.T) {
			provider := stores.NewOverlayStoreProvider(fixture.provider,
				stores.NewValidationContext(map[string]*stores.StoreOverlay{
					"gateways": stores.NewStoreOverlayForUpdate(&unstructured.Unstructured{Object: gateway}),
				}))
			result, err := fixture.service.Render(t.Context(), provider, rendercontext.RenderModeAdmission,
				rendercontext.WithAdmissionSubject(subject.store, "default", subject.name))
			if subject.reject {
				require.ErrorContains(t, err, "would break out of the bind directive")
				return
			}
			require.NoError(t, err)
			require.Contains(t, result.HAProxyConfig, "# listeners=[]")
			result.InputTransaction.Abort()
		})
	}
}
