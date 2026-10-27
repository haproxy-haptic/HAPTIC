// Copyright 2025 Philipp Hossner
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
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/haproxy-haptic/HAPTIC/tests/e2e/httpclient"
)

var backendTLSPolicyGVR = schema.GroupVersionResource{
	Group: "gateway.networking.k8s.io", Version: "v1", Resource: "backendtlspolicies",
}

func TestGatewayBackendTLSIdentity(t *testing.T) {
	t.Parallel()
	feature := features.New("Gateway backend certificate identity and admission").
		Assess("verify the configured DNS identity and reject unsupported identities", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			client, err := cfg.NewClient()
			require.NoError(t, err)
			ns := NamespaceForTest(ctx, t, client)
			DumpLogsOnFailure(t, ns)
			const host = "backend-san.localdev.me"
			upstream := NewEchoServerBackend(ctx, t, client, ns)
			backend := NewHAProxyMTLSBackend(ctx, t, client, ns, upstream, host)
			gateway := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "gateway.networking.k8s.io/v1", "kind": "Gateway",
				"metadata": map[string]any{"name": "backend-tls", "namespace": ns},
				"spec": map[string]any{
					"gatewayClassName": gatewayClassName,
					"listeners":        []any{map[string]any{"name": "http", "protocol": "HTTP", "port": int64(80)}},
					"tls": map[string]any{"backend": map[string]any{
						"clientCertificateRef": map[string]any{"group": "", "kind": "Secret", "name": backend.ClientCertSecretName},
					}},
				},
			}}
			require.NoError(t, client.Resources().Create(ctx, gateway))
			dyn, err := dynamic.NewForConfig(client.RESTConfig())
			require.NoError(t, err)
			policies := dyn.Resource(backendTLSPolicyGVR).Namespace(ns)
			policy := backendIdentityPolicy(ns, backend)
			_, err = policies.Create(ctx, policy, metav1.CreateOptions{})
			require.NoError(t, err)
			NewHTTPRoute(ctx, t, ns, &HTTPRouteSpec{
				Name: "backend-identity", GatewayName: gateway.GetName(), Hostnames: []string{host},
				Rules: []HTTPRouteRule{{BackendRefs: []HTTPRouteBackendRef{{Service: backend.HTTPS.Service, Port: backend.HTTPS.Port}}}},
			})
			waitForRouteDeployed(ctx, t, client, httpRouteGVR, ns, "backend-identity")
			fwd := ForwardGateway(ctx, t, ns, gateway.GetName(), 80)
			request := httpclient.ForForwarded(t, fwd.HTTPPort, 0).GET(host, "/")
			require.NotNil(t, request.ExpectOK(t).Echo)
			rejectUnsupportedBackendIdentities(ctx, t, policies)
			require.NotNil(t, request.ExpectOK(t).Echo)

			require.NoError(t, patchBackendIdentityValidation(ctx, t, policies, map[string]any{
				"hostname": "different-sni.localdev.me", "subjectAltNames": nil,
			}))
			request.ExpectStatus(t, http.StatusServiceUnavailable)

			require.NoError(t, patchBackendIdentityValidation(ctx, t, policies, map[string]any{
				"hostname":        host,
				"subjectAltNames": []any{map[string]any{"type": "Hostname", "hostname": host}},
			}))
			require.NotNil(t, request.ExpectOK(t).Echo)
			return ctx
		}).Feature()
	testEnv.Test(t, feature)
}

func backendIdentityPolicy(namespace string, backend HAProxyTLSBackend) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1", "kind": "BackendTLSPolicy",
		"metadata": map[string]any{"name": "backend-identity", "namespace": namespace},
		"spec": map[string]any{
			"targetRefs": []any{map[string]any{"group": "", "kind": "Service", "name": backend.HTTPS.Service}},
			"validation": map[string]any{
				"hostname":          "backend-san.localdev.me",
				"caCertificateRefs": []any{map[string]any{"group": "", "kind": "Secret", "name": backend.CASecretName}},
				"subjectAltNames":   []any{map[string]any{"type": "Hostname", "hostname": "backend-san.localdev.me"}},
			},
		},
	}}
}

func rejectUnsupportedBackendIdentities(ctx context.Context, t *testing.T, policies dynamic.ResourceInterface) {
	t.Helper()
	hostname := map[string]any{"type": "Hostname", "hostname": "backend-san.localdev.me"}
	uri := map[string]any{"type": "URI", "uri": "spiffe://cluster.local/ns/backend/sa/app"}
	for _, sans := range [][]any{
		{map[string]any{"type": "Hostname", "hostname": "different-san.localdev.me"}},
		{uri},
		{hostname, uri},
		{hostname, map[string]any{"type": "Hostname", "hostname": "second.localdev.me"}},
	} {
		policy, err := policies.Get(ctx, "backend-identity", metav1.GetOptions{})
		require.NoError(t, err)
		generation := policy.GetGeneration()
		originalSpec, _, err := unstructured.NestedMap(policy.Object, "spec")
		require.NoError(t, err)
		require.NoError(t, unstructured.SetNestedSlice(policy.Object, sans, "spec", "validation", "subjectAltNames"))
		err = patchBackendIdentityValidation(ctx, t, policies, map[string]any{"subjectAltNames": sans})
		require.ErrorContains(t, err, "subjectAltNames must contain exactly one Hostname entry")
		stored, err := policies.Get(ctx, policy.GetName(), metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, generation, stored.GetGeneration())
		require.Equal(t, originalSpec, stored.Object["spec"])
		policy.SetName("unsupported-identity")
		policy.SetResourceVersion("")
		policy.SetUID("")
		_, err = policies.Create(ctx, policy, metav1.CreateOptions{})
		require.ErrorContains(t, err, "subjectAltNames must contain exactly one Hostname entry")
	}
}

func patchBackendIdentityValidation(ctx context.Context, t *testing.T, policies dynamic.ResourceInterface, validation map[string]any) error {
	t.Helper()
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"validation": validation}})
	require.NoError(t, err)
	_, err = policies.Patch(ctx, "backend-identity", types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

func TestGatewayTCPBackendTLS(t *testing.T) {
	t.Parallel()
	feature := features.New("TCPRoute backend TLS").
		Assess("encrypt TCP backend traffic and verify its certificate", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			client, err := cfg.NewClient()
			require.NoError(t, err)
			ns := NamespaceForTest(ctx, t, client)
			DumpLogsOnFailure(t, ns)
			upstream := NewEchoServerBackend(ctx, t, client, ns)
			backend := NewHAProxyTLSBackend(ctx, t, client, ns, upstream, "backend-san.localdev.me")
			gateway := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "gateway.networking.k8s.io/v1", "kind": "Gateway",
				"metadata": map[string]any{"name": "backend-tls", "namespace": ns},
				"spec": map[string]any{
					"gatewayClassName": gatewayClassName,
					"listeners":        []any{map[string]any{"name": "tcp", "protocol": "TCP", "port": int64(9100)}},
				},
			}}
			require.NoError(t, client.Resources().Create(ctx, gateway))
			require.NoError(t, client.Resources().Create(ctx, backendIdentityPolicy(ns, backend)))
			route := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "gateway.networking.k8s.io/v1", "kind": "TCPRoute",
				"metadata": map[string]any{"name": "backend-identity", "namespace": ns},
				"spec": map[string]any{
					"parentRefs": []any{map[string]any{"name": gateway.GetName()}},
					"rules": []any{map[string]any{"backendRefs": []any{
						map[string]any{"name": backend.HTTPS.Service, "port": int64(backend.HTTPS.Port)},
					}}},
				},
			}}
			require.NoError(t, client.Resources().Create(ctx, route))
			waitForRouteDeployed(ctx, t, client, schema.GroupVersionResource{
				Group: "gateway.networking.k8s.io", Version: "v1", Resource: "tcproutes",
			}, ns, route.GetName())
			fwd := ForwardService(t, HAProxyDeploymentName, 9100)
			request := httpclient.ForForwarded(t, fwd.Ports[9100], 0).GET("backend-san.localdev.me", "/")
			require.NotNil(t, request.ExpectOK(t).Echo)
			return ctx
		}).Feature()
	testEnv.Test(t, feature)
}
