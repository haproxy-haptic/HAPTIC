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
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
	pb "sigs.k8s.io/gateway-api-conformance-images/echo-basic/grpcechoserver"

	"github.com/haproxy-haptic/HAPTIC/tests/e2e/grpcclient"
	"github.com/haproxy-haptic/HAPTIC/tests/e2e/httpclient"
)

func TestGatewayRoutePolicyAuthentication(t *testing.T) {
	t.Parallel()
	feature := features.New("Gateway policy authentication and credential rotation").
		Assess("HTTP and gRPC enforce policy changes", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			client, err := cfg.NewClient()
			require.NoError(t, err)
			ns := NamespaceForTest(ctx, t, client)
			DumpLogsOnFailure(t, ns)
			scenario := newGatewayPolicyAuthScenario(ctx, t, client, ns)
			scenario.checkHTTP(ctx, t)
			defer scenario.monitorUnauthenticatedGRPC(ctx, t)()
			scenario.checkGRPC(ctx, t, "old-key")
			scenario.checkAdmission(ctx, t)
			scenario.rotateCredentials(ctx, t)
			scenario.checkGRPC(ctx, t, "new-key")
			scenario.checkCredentialRecreation(ctx, t)
			scenario.deleteAndRecover(ctx, t)
			return ctx
		}).Feature()
	testEnv.Test(t, feature)
}

type gatewayPolicyAuthScenario struct {
	client    klient.Client
	namespace string
	host      string
	grpcHost  string
	token     string
	httpsPort int
	requests  *httpclient.Client
	policy    *unstructured.Unstructured
}

func newGatewayPolicyAuthScenario(ctx context.Context, t *testing.T, client klient.Client, namespace string) *gatewayPolicyAuthScenario {
	t.Helper()
	const host = "gateway-policy-auth.localdev.me"
	const grpcHost = "gateway-policy-grpc.localdev.me"
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	public := pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(&key.PublicKey)})
	mustCreateImmutableSecret(ctx, t, client, namespace, "jwt", map[string][]byte{"pubkey.pem": public})
	mustCreateImmutableSecret(ctx, t, client, namespace, "keys", map[string][]byte{"keys": []byte("old-key:consumer-a\n")})
	policy := gatewayRoutePolicy(namespace, "access", map[string]any{
		"authentication": map[string]any{
			"jwt":    map[string]any{"secretRef": map[string]any{"name": "jwt"}, "requiredClaims": []any{"exp", "sub"}, "issuer": "policy-test", "audience": "api", "allowedGroups": []any{"admins", "operators"}, "groupsClaim": "roles"},
			"apiKey": map[string]any{"secretRef": map[string]any{"name": "keys"}, "consumerHeader": "X-Consumer"},
		},
	})
	require.NoError(t, client.Resources().Create(ctx, policy))
	backend := NewEchoServerBackend(ctx, t, client, namespace)
	grpcBackend := NewGRPCEchoBackend(ctx, t, client, namespace)
	NewTLSSecret(ctx, t, client, namespace, "certificate", []string{host, grpcHost})
	NewHTTPSGateway(ctx, t, namespace, "policy", "certificate")
	protected := gatewayPolicyHTTPRule(backend, "/", "access")
	publicRule := gatewayPolicyHTTPRule(backend, "/public", "")
	redirect := gatewayPolicyHTTPRule(backend, "/redirect", "access")
	delete(redirect, "backendRefs")
	redirect["filters"] = append(redirect["filters"].([]any), map[string]any{
		"type": "RequestRedirect", "requestRedirect": map[string]any{"hostname": "target.example.com", "statusCode": int64(302)},
	})
	cors := gatewayPolicyHTTPRule(backend, "/cors", "access")
	cors["filters"] = append(cors["filters"].([]any), map[string]any{
		"type": "CORS", "cors": map[string]any{"allowOrigins": []any{"https://client.example.com"}, "allowMethods": []any{"GET"}, "allowHeaders": []any{"Authorization", "X-API-Key"}},
	})
	httpRoute := gatewayPolicyRoute("HTTPRoute", namespace, host, []any{protected, publicRule, redirect, cors})
	grpcRule := gatewayPolicyHTTPRule(grpcBackend, "", "access")
	delete(grpcRule, "matches")
	grpcRoute := gatewayPolicyRoute("GRPCRoute", namespace, grpcHost, []any{grpcRule})
	require.NoError(t, client.Resources().Create(ctx, httpRoute))
	require.NoError(t, client.Resources().Create(ctx, grpcRoute))
	waitForRouteDeployed(ctx, t, client, httpRouteGVR, namespace, "same-name")
	waitForRouteDeployed(ctx, t, client, grpcRouteGVR, namespace, "same-name")
	forward := ForwardGateway(ctx, t, namespace, "policy", 443)
	token := signRS256(t, key, map[string]any{"alg": "RS256", "typ": "JWT"}, map[string]any{
		"sub": "jwt-consumer", "exp": time.Now().Add(time.Hour).Unix(), "iss": "policy-test", "aud": "api", "roles": []string{"other", "admins"},
	})
	for _, audience := range []any{"api", []string{"other", "api"}, []string{"api", "last"}} {
		arrayToken := signRS256(t, key, map[string]any{"alg": "RS256"}, map[string]any{
			"sub": "jwt-consumer", "exp": time.Now().Add(time.Hour).Unix(), "iss": "policy-test", "aud": audience, "roles": "operators",
		})
		httpclient.ForForwarded(t, 0, forward.HTTPSPort).HTTPS(host, "/").
			WithHeader("Authorization", "Bearer "+arrayToken).WithHeader("X-API-Key", "old-key").ExpectOK(t)
	}
	checkGatewayPolicyGroups(t, key, host, forward.HTTPSPort)
	checkGatewayPolicySharedIngressKey(ctx, t, client, namespace, backend, token)
	return &gatewayPolicyAuthScenario{client: client, namespace: namespace, host: host, grpcHost: grpcHost, token: token,
		httpsPort: forward.HTTPSPort, requests: httpclient.ForForwarded(t, 0, forward.HTTPSPort), policy: policy}
}

func checkGatewayPolicyGroups(t *testing.T, key *rsa.PrivateKey, host string, port int) {
	t.Helper()
	for _, tc := range []struct {
		groups any
		status int
	}{
		{[]string{"other", "admins"}, http.StatusOK},
		{"operators", http.StatusOK},
		{nil, http.StatusForbidden},
		{[]string{"outsiders"}, http.StatusForbidden},
		{[]any{"admins", 1}, http.StatusForbidden},
	} {
		claims := map[string]any{"sub": "member", "exp": time.Now().Add(time.Hour).Unix(), "iss": "policy-test", "aud": "api", "groups": "admins"}
		if tc.groups != nil {
			claims["roles"] = tc.groups
		}
		token := signRS256(t, key, map[string]any{"alg": "RS256"}, claims)
		for _, path := range []string{"/", "/redirect"} {
			statusCode := tc.status
			if path == "/redirect" && statusCode == http.StatusOK {
				statusCode = http.StatusFound
			}
			httpclient.ForForwarded(t, 0, port).HTTPS(host, path).
				WithHeader("Authorization", "Bearer "+token).WithHeader("X-API-Key", "old-key").ExpectStatus(t, statusCode)
		}
	}
}

func checkGatewayPolicySharedIngressKey(ctx context.Context, t *testing.T, client klient.Client, namespace string, backend BackendRef, token string) {
	t.Helper()
	const host = "gateway-policy-shared-ingress.localdev.me"
	NewIngress(ctx, t, client, namespace, &IngressSpec{
		Name: "shared-jwt", Host: host, BackendService: backend.Service, BackendPort: backend.Port,
		Annotations: map[string]string{"haproxy-haptic.org/jwt-secret": "jwt"},
	})
	registerGatewayPolicyRoutingCleanup(ctx, t, client, namespace)
	httpclient.New(t).GET(host, "/").ExpectStatus(t, http.StatusUnauthorized)
	httpclient.New(t).GET(host, "/").WithHeader("Authorization", "Bearer "+token).ExpectOK(t)
}

func registerGatewayPolicyRoutingCleanup(ctx context.Context, t *testing.T, client klient.Client, namespace string) {
	t.Helper()
	// Remove Gateway routing before the Ingress cleanup waits for this namespace to disappear.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		for _, resource := range []struct{ kind, name string }{
			{"HTTPRoute", "same-name"}, {"GRPCRoute", "same-name"}, {"Gateway", "policy"},
		} {
			object := &unstructured.Unstructured{}
			object.SetAPIVersion("gateway.networking.k8s.io/v1")
			object.SetKind(resource.kind)
			object.SetNamespace(namespace)
			object.SetName(resource.name)
			if err := client.Resources().Delete(cleanupCtx, object); err != nil && !apierrors.IsNotFound(err) {
				t.Errorf("delete %s %s/%s: %v", resource.kind, namespace, resource.name, err)
			}
		}
	})
}

func (s *gatewayPolicyAuthScenario) request(path, key string) *httpclient.Request {
	return s.requests.HTTPS(s.host, path).WithHeader("Authorization", "Bearer "+s.token).WithHeader("X-API-Key", key)
}

func (s *gatewayPolicyAuthScenario) checkHTTP(ctx context.Context, t *testing.T) {
	t.Helper()
	s.requests.HTTPS(s.host, "/public").ExpectOK(t)
	s.requests.HTTPS(s.host, "/").ExpectStatus(t, http.StatusUnauthorized)
	s.requests.HTTPS(s.host, "/").WithHeader("X-API-Key", "old-key").ExpectStatus(t, http.StatusUnauthorized)
	s.request("/", "wrong-key").ExpectStatus(t, http.StatusUnauthorized)
	s.request("/", "old-key").WithHeader("X-Consumer", "spoofed").ExpectEchoHeader(t, "X-Consumer", "jwt-consumer")
	s.requests.HTTPS(s.host, "/redirect").ExpectStatus(t, http.StatusUnauthorized)
	s.request("/redirect", "old-key").ExpectStatus(t, http.StatusFound)
	s.requests.HTTPS(s.host, "/cors").WithMethod(http.MethodOptions).
		WithHeader("Origin", "https://client.example.com").WithHeader("Access-Control-Request-Method", "GET").
		WithHeader("Access-Control-Request-Headers", "Authorization,X-API-Key").ExpectStatus(t, http.StatusOK)
	response, err := s.requests.HTTPS(s.host, "/cors").WithHeader("Origin", "https://client.example.com").Do(ctx)
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, response.Status)
}

func (s *gatewayPolicyAuthScenario) checkGRPC(ctx context.Context, t *testing.T, key string) {
	t.Helper()
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	connection, err := grpcclient.ForForwarded(t, s.httpsPort).Dial(callCtx, s.grpcHost)
	require.NoError(t, err)
	defer func() { _ = connection.Close() }()
	client := pb.NewGrpcEchoClient(connection)
	_, err = client.Echo(callCtx, &pb.EchoRequest{})
	require.Equal(t, codes.Unauthenticated, status.Code(err))
	authenticated := metadata.NewOutgoingContext(callCtx, metadata.Pairs("authorization", "Bearer "+s.token, "x-api-key", key))
	response, err := client.Echo(authenticated, &pb.EchoRequest{})
	require.NoError(t, err)
	require.NotNil(t, response.GetAssertions())
}

func (s *gatewayPolicyAuthScenario) monitorUnauthenticatedGRPC(ctx context.Context, t *testing.T) func() {
	t.Helper()
	probeCtx, cancel := context.WithCancel(ctx)
	connection, err := grpcclient.ForForwarded(t, s.httpsPort).Dial(probeCtx, s.grpcHost)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		done <- probeUnauthenticatedGRPC(probeCtx, pb.NewGrpcEchoClient(connection))
	}()
	return func() {
		cancel()
		_ = connection.Close()
		require.NoError(t, <-done)
	}
}

func probeUnauthenticatedGRPC(ctx context.Context, client pb.GrpcEchoClient) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_, err := client.Echo(callCtx, &pb.EchoRequest{})
			cancel()
			if err == nil {
				return errors.New("unauthenticated gRPC request succeeded during policy update")
			}
			if ctx.Err() != nil {
				return nil
			}
			if code := status.Code(err); code != codes.Unauthenticated && code != codes.Unavailable {
				return fmt.Errorf("unauthenticated gRPC request during policy update returned %s: %w", code, err)
			}
		}
	}
}

func (s *gatewayPolicyAuthScenario) checkAdmission(ctx context.Context, t *testing.T) {
	t.Helper()
	require.NoError(t, s.client.Resources().Get(ctx, s.policy.GetName(), s.namespace, s.policy))
	invalid := s.policy.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(invalid.Object, "missing", "spec", "authentication", "jwt", "secretRef", "name"))
	require.ErrorContains(t, s.client.Resources().Update(ctx, invalid), "missing")
	invalid = s.policy.DeepCopy()
	require.NoError(t, unstructured.SetNestedMap(invalid.Object, map[string]any{"ttlSeconds": int64(60)}, "spec", "cache"))
	require.ErrorContains(t, s.client.Resources().Update(ctx, invalid), "GRPCRoute")
	var secret corev1.Secret
	require.NoError(t, s.client.Resources().Get(ctx, "jwt", s.namespace, &secret))
	secret.Data["pubkey.pem"] = []byte("invalid public key")
	require.ErrorContains(t, s.client.Resources().Update(ctx, &secret), "immutable")
	mustCreateImmutableSecret(ctx, t, s.client, s.namespace, "invalid-jwt", secret.Data)
	invalid = s.policy.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(invalid.Object, "invalid-jwt", "spec", "authentication", "jwt", "secretRef", "name"))
	require.ErrorContains(t, s.client.Resources().Update(ctx, invalid), "public key")
	mustCreateSecret(ctx, t, s.client, s.namespace, "mutable-keys", map[string][]byte{"keys": []byte("other:consumer\n")})
	invalid = s.policy.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(invalid.Object, "mutable-keys", "spec", "authentication", "apiKey", "secretRef", "name"))
	require.ErrorContains(t, s.client.Resources().Update(ctx, invalid), "mutable")
	s.request("/", "old-key").ExpectOK(t)
}

func (s *gatewayPolicyAuthScenario) rotateCredentials(ctx context.Context, t *testing.T) {
	t.Helper()
	var secret corev1.Secret
	require.NoError(t, s.client.Resources().Get(ctx, "keys", s.namespace, &secret))
	secret.Data["keys"] = []byte("new-key:consumer-b\n")
	require.ErrorContains(t, s.client.Resources().Update(ctx, &secret), "immutable")
	mustCreateImmutableSecret(ctx, t, s.client, s.namespace, "keys-v2", secret.Data)
	require.NoError(t, s.client.Resources().Get(ctx, s.policy.GetName(), s.namespace, s.policy))
	require.NoError(t, unstructured.SetNestedField(s.policy.Object, "keys-v2", "spec", "authentication", "apiKey", "secretRef", "name"))
	require.NoError(t, s.client.Resources().Update(ctx, s.policy))
	s.request("/", "new-key").ExpectEchoHeader(t, "X-Consumer", "jwt-consumer")
	response, err := s.request("/", "old-key").Do(ctx)
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, response.Status)
	s.requests.HTTPS(s.host, "/public").ExpectOK(t)
}

func (s *gatewayPolicyAuthScenario) checkCredentialRecreation(ctx context.Context, t *testing.T) {
	t.Helper()
	var secret corev1.Secret
	require.NoError(t, s.client.Resources().Get(ctx, "keys-v2", s.namespace, &secret))
	require.NoError(t, s.client.Resources().Delete(ctx, &secret))
	s.request("/", "new-key").ExpectStatus(t, http.StatusServiceUnavailable)
	s.requests.HTTPS(s.host, "/public").ExpectOK(t)
	mustCreateSecret(ctx, t, s.client, s.namespace, "keys-v2", secret.Data)
	s.waitCondition(ctx, t, "ResolvedRefs", "False", "RefNotPermitted")
	s.request("/", "new-key").ExpectStatus(t, http.StatusServiceUnavailable)
	require.NoError(t, s.client.Resources().Get(ctx, "keys-v2", s.namespace, &secret))
	immutable := true
	secret.Immutable = &immutable
	require.NoError(t, s.client.Resources().Update(ctx, &secret))
	s.request("/", "new-key").ExpectOK(t)
	s.checkGRPC(ctx, t, "new-key")
}

func (s *gatewayPolicyAuthScenario) deleteAndRecover(ctx context.Context, t *testing.T) {
	t.Helper()
	require.NoError(t, s.client.Resources().Delete(ctx, s.policy))
	s.request("/", "new-key").ExpectStatus(t, http.StatusServiceUnavailable)
	s.requests.HTTPS(s.host, "/public").ExpectOK(t)
	s.waitCondition(ctx, t, "PartiallyInvalid", "True", "")
	spec, _, err := unstructured.NestedMap(s.policy.Object, "spec")
	require.NoError(t, err)
	s.policy = gatewayRoutePolicy(s.namespace, s.policy.GetName(), spec)
	require.NoError(t, s.client.Resources().Create(ctx, s.policy))
	s.request("/", "new-key").ExpectOK(t)
	s.checkGRPC(ctx, t, "new-key")
}

func (s *gatewayPolicyAuthScenario) waitCondition(ctx context.Context, t *testing.T, kind, conditionStatus, reason string) {
	t.Helper()
	waitForResourceDeployed(ctx, t, s.client, httpRouteGVR, s.namespace, "same-name", func(route *unstructured.Unstructured) (bool, string) {
		parents, _, err := unstructured.NestedSlice(route.Object, "status", "parents")
		if err != nil {
			return false, err.Error()
		}
		for _, parent := range parents {
			conditions, _, _ := unstructured.NestedSlice(parent.(map[string]any), "conditions")
			for _, condition := range conditions {
				value := condition.(map[string]any)
				if value["type"] == kind && value["status"] == conditionStatus && (reason == "" || value["reason"] == reason) {
					return true, ""
				}
			}
		}
		return false, "waiting for " + kind + "=" + conditionStatus + " " + reason
	})
}

func gatewayRoutePolicy(namespace, name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "haproxy-haptic.org/v1alpha1", "kind": "HAProxyRoutePolicy",
		"metadata": map[string]any{"namespace": namespace, "name": name}, "spec": spec,
	}}
}

func gatewayPolicyRoute(kind, namespace, host string, rules []any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1", "kind": kind,
		"metadata": map[string]any{"namespace": namespace, "name": "same-name"},
		"spec":     map[string]any{"parentRefs": []any{map[string]any{"name": "policy"}}, "hostnames": []any{host}, "rules": rules},
	}}
}

func gatewayPolicyHTTPRule(backend BackendRef, path, policy string) map[string]any {
	rule := map[string]any{
		"matches":     []any{map[string]any{"path": map[string]any{"type": "PathPrefix", "value": path}}},
		"backendRefs": []any{map[string]any{"name": backend.Service, "port": int64(backend.Port)}},
	}
	if policy != "" {
		rule["filters"] = []any{map[string]any{"type": "ExtensionRef", "extensionRef": map[string]any{
			"group": "haproxy-haptic.org", "kind": "HAProxyRoutePolicy", "name": policy,
		}}}
	}
	return rule
}
