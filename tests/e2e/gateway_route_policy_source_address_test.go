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
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/haproxy-haptic/HAPTIC/tests/e2e/httpclient"
	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
)

func TestGatewayRoutePolicySourceAddress(t *testing.T) {
	t.Parallel()
	feature := features.New("Gateway policy source-address allow and deny lists").
		Assess("HTTP rules enforce inline lists and immutable ConfigMap rotation", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			client, err := cfg.NewClient()
			require.NoError(t, err)
			ns := NamespaceForTest(ctx, t, client)
			DumpLogsOnFailure(t, ns)
			backend := NewEchoServerBackend(ctx, t, client, ns)
			allow := gatewayRoutePolicy(ns, "allow", map[string]any{
				"sourceAddress": map[string]any{"allow": []any{"0.0.0.0/0"}},
			})
			deny := gatewayRoutePolicy(ns, "deny", map[string]any{
				"sourceAddress": map[string]any{"deny": []any{"0.0.0.0/0"}},
			})
			require.NoError(t, client.Resources().Create(ctx, allow))
			require.NoError(t, client.Resources().Create(ctx, deny))
			NewGateway(ctx, t, ns, "policy")
			const host = "gateway-policy-source.localdev.me"
			route := gatewayPolicyRoute("HTTPRoute", ns, host, []any{
				gatewayPolicyHTTPRule(backend, "/allow", "allow"),
				gatewayPolicyHTTPRule(backend, "/deny", "deny"),
				gatewayPolicyHTTPRule(backend, "/public", ""),
			})
			require.NoError(t, client.Resources().Create(ctx, route))
			waitForRouteDeployed(ctx, t, client, httpRouteGVR, ns, route.GetName())
			forward := ForwardGateway(ctx, t, ns, "policy", 80)
			requests := httpclient.ForForwarded(t, forward.HTTPPort, 0)
			requests.GET(host, "/allow").ExpectOK(t)
			requests.GET(host, "/deny").ExpectStatus(t, http.StatusForbidden)
			requests.GET(host, "/public").ExpectOK(t)

			require.NoError(t, client.Resources().Get(ctx, allow.GetName(), ns, allow))
			invalid := allow.DeepCopy()
			require.NoError(t, unstructured.SetNestedSlice(invalid.Object, []any{"999.0.0.0/8"}, "spec", "sourceAddress", "allow"))
			require.ErrorContains(t, client.Resources().Update(ctx, invalid), "invalid CIDR")
			requests.GET(host, "/allow").ExpectOK(t)

			v1 := gatewaySourceAddressConfigMap(ns, "source-v1", "allow: [0.0.0.0/0]\n")
			require.NoError(t, client.Resources().Create(ctx, v1))
			require.NoError(t, updateGatewaySourceAddressReference(ctx, client, allow, v1.Name))
			requests.GET(host, "/allow").ExpectOK(t)
			v2 := gatewaySourceAddressConfigMap(ns, "source-v2", "allow: [192.0.2.1/32]\n")
			require.NoError(t, client.Resources().Create(ctx, v2))
			require.NoError(t, updateGatewaySourceAddressReference(ctx, client, allow, v2.Name))
			requests.GET(host, "/allow").ExpectStatus(t, http.StatusForbidden)
			requests.GET(host, "/public").ExpectOK(t)

			bad := gatewaySourceAddressConfigMap(ns, "source-bad", "allow: [999.0.0.0/8]\n")
			require.NoError(t, client.Resources().Create(ctx, bad))
			require.ErrorContains(t, updateGatewaySourceAddressReference(ctx, client, allow, bad.Name), "invalid CIDR")
			requests.GET(host, "/allow").ExpectStatus(t, http.StatusForbidden)

			require.NoError(t, client.Resources().Delete(ctx, v2))
			requests.GET(host, "/allow").ExpectStatus(t, http.StatusServiceUnavailable)
			mutable := gatewaySourceAddressConfigMap(ns, v2.Name, v2.Data["source-address.yaml"])
			mutable.Immutable = nil
			require.NoError(t, client.Resources().Create(ctx, mutable))
			waitForGatewaySourceAddressResolvedRefs(ctx, t, client, ns, route.GetName(), "is mutable")
			requests.GET(host, "/allow").ExpectStatus(t, http.StatusServiceUnavailable)
			require.NoError(t, client.Resources().Get(ctx, mutable.Name, ns, mutable))
			immutable := true
			mutable.Immutable = &immutable
			require.NoError(t, client.Resources().Update(ctx, mutable))
			requests.GET(host, "/allow").ExpectStatus(t, http.StatusForbidden)

			v3 := gatewaySourceAddressConfigMap(ns, "source-v3", "allow: [0.0.0.0/0]\n")
			require.NoError(t, client.Resources().Create(ctx, v3))
			require.NoError(t, updateGatewaySourceAddressReference(ctx, client, allow, v3.Name))
			requests.GET(host, "/allow").ExpectOK(t)
			requests.GET(host, "/deny").ExpectStatus(t, http.StatusForbidden)
			requests.GET(host, "/public").ExpectOK(t)
			return ctx
		}).Feature()
	testEnv.Test(t, feature)
}

func gatewaySourceAddressConfigMap(namespace, name, document string) *corev1.ConfigMap {
	immutable := true
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Immutable:  &immutable,
		Data:       map[string]string{"source-address.yaml": document},
	}
}

func waitForGatewaySourceAddressResolvedRefs(ctx context.Context, t *testing.T, client klient.Client, namespace, routeName, message string) {
	t.Helper()
	waitForResourceDeployed(ctx, t, client, httpRouteGVR, namespace, routeName, func(route *unstructured.Unstructured) (bool, string) {
		parents, _, err := unstructured.NestedSlice(route.Object, "status", "parents")
		if err != nil {
			return false, err.Error()
		}
		for _, parent := range parents {
			conditions, _, _ := unstructured.NestedSlice(parent.(map[string]any), "conditions")
			for _, condition := range conditions {
				value := condition.(map[string]any)
				text, _ := value["message"].(string)
				if value["type"] == "ResolvedRefs" && value["status"] == "False" && strings.Contains(text, message) {
					return true, ""
				}
			}
		}
		return false, "waiting for ResolvedRefs=False with message " + message
	})
}

func updateGatewaySourceAddressReference(ctx context.Context, client klient.Client, policy *unstructured.Unstructured, name string) error {
	if err := client.Resources().Get(ctx, policy.GetName(), policy.GetNamespace(), policy); err != nil {
		return err
	}
	unstructured.RemoveNestedField(policy.Object, "spec", "sourceAddress", "allow")
	unstructured.RemoveNestedField(policy.Object, "spec", "sourceAddress", "deny")
	if err := unstructured.SetNestedField(policy.Object, name, "spec", "sourceAddress", "configMapRef", "name"); err != nil {
		return err
	}
	missing := "Source-address ConfigMap " + policy.GetNamespace() + "/" + name + " key source-address.yaml is missing."
	var updateErr error
	waitErr := testutil.WaitForCondition(ctx, testutil.FastWaitConfig(), func(ctx context.Context) (bool, error) {
		updateErr = client.Resources().Update(ctx, policy)
		return updateErr == nil || !strings.Contains(updateErr.Error(), missing), updateErr
	})
	if waitErr != nil {
		return waitErr
	}
	return updateErr
}
