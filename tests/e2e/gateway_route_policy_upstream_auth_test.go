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

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/haproxy-haptic/HAPTIC/tests/e2e/httpclient"
)

// TestGatewayRoutePolicyUpstreamAuthorization mirrors
// TestHapticUpstreamAuthorization for HAProxyRoutePolicy upstreamAuthorization.
func TestGatewayRoutePolicyUpstreamAuthorization(t *testing.T) {
	t.Parallel()

	const (
		host    = "gateway-policy-upstream-auth.localdev.me"
		first   = "Basic Z2l0bGFiK2RlcGxveS10b2tlbi00MjpmaXJzdA=="
		rotated = "Bearer rotated-gateway-upstream-token"
		apiKey  = "gw-apikey-abc123"
		caller  = "Bearer caller-token"
	)

	feature := features.New("Gateway policy: upstream credential from a Secret").
		Assess("the upstream receives the policy's credential", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			client, err := cfg.NewClient()
			require.NoError(t, err)
			ns := NamespaceForTest(ctx, t, client)
			DumpLogsOnFailure(t, ns)
			backend := NewEchoServerBackend(ctx, t, client, ns)
			mustCreateImmutableSecret(ctx, t, client, ns, "keys", map[string][]byte{"keys": []byte(apiKey + ":alice\n")})
			mustCreateImmutableSecret(ctx, t, client, ns, "git-deploy-v1", map[string][]byte{"authorization": []byte(first)})
			policy := gatewayRoutePolicy(ns, "private", map[string]any{
				"authentication":        map[string]any{"apiKey": map[string]any{"secretRef": map[string]any{"name": "keys"}}},
				"upstreamAuthorization": map[string]any{"secretRef": map[string]any{"name": "git-deploy-v1"}},
			})
			require.NoError(t, client.Resources().Create(ctx, policy))
			NewGateway(ctx, t, ns, "policy")
			route := gatewayPolicyRoute("HTTPRoute", ns, host, []any{
				gatewayPolicyHTTPRule(backend, "/", "private"),
				gatewayPolicyHTTPRule(backend, "/public", ""),
			})
			require.NoError(t, client.Resources().Create(ctx, route))
			waitForRouteDeployed(ctx, t, client, httpRouteGVR, ns, "same-name")
			requests := httpclient.ForForwarded(t, ForwardGateway(ctx, t, ns, "policy", 80).HTTPPort, 0)
			protected := func() *httpclient.Request {
				return requests.GET(host, "/").WithHeader("X-API-Key", apiKey).WithHeader("Authorization", caller)
			}

			protected().ExpectEchoHeader(t, "Authorization", first)
			requests.GET(host, "/").WithHeader("Authorization", caller).ExpectStatus(t, http.StatusUnauthorized)
			requests.GET(host, "/public").WithHeader("Authorization", caller).ExpectEchoHeader(t, "Authorization", caller)
			protected().WithMethod(http.MethodTrace).ExpectStatus(t, http.StatusMethodNotAllowed)

			setRef := func(name string) {
				t.Helper()
				require.NoError(t, client.Resources().Get(ctx, policy.GetName(), ns, policy))
				require.NoError(t, unstructured.SetNestedField(policy.Object, name, "spec", "upstreamAuthorization", "secretRef", "name"))
				require.NoError(t, client.Resources().Update(ctx, policy))
			}
			mustCreateImmutableSecret(ctx, t, client, ns, "git-deploy-v2", map[string][]byte{"authorization": []byte(rotated)})
			setRef("git-deploy-v2")
			protected().ExpectEchoHeader(t, "Authorization", rotated)

			setRef("absent")
			protected().ExpectStatus(t, http.StatusServiceUnavailable)
			requests.GET(host, "/public").ExpectOK(t)
			setRef("git-deploy-v2")
			protected().ExpectEchoHeader(t, "Authorization", rotated)

			foreign := fmt.Sprintf(`apiVersion: haproxy-haptic.org/v1alpha1
kind: HAProxyRoutePolicy
metadata: {name: foreign, namespace: %s}
spec:
  upstreamAuthorization:
    secretRef: {name: git-deploy-v2, namespace: kube-system}
`, ns)
			err = kubectlApplyStdin(ctx, []byte(foreign))
			require.ErrorContains(t, err, "unknown field")

			for _, kind := range []string{"haproxycfgs", "haproxymapfiles"} {
				out, err := kubectlWithBackoff(ctx, kubeconfigFlag, kubeconfigPath, "get", kind, "-A", "-o", "yaml")
				require.NoError(t, err, "list %s: %s", kind, out)
				for _, value := range []string{first, rotated, "rotated-gateway-upstream-token", apiKey} {
					require.False(t, strings.Contains(string(out), value), "%s contains %q", kind, value)
				}
			}
			return ctx
		}).
		Feature()

	testEnv.Test(t, feature)
}
