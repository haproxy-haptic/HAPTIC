// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/haproxy-haptic/HAPTIC/tests/e2e/httpclient"
)

func TestGatewayJWTMetadata(t *testing.T) {
	t.Parallel()
	feature := features.New("Gateway JWT metadata discovery").Assess("listener isolation and metadata deletion", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		t.Helper()
		client, err := cfg.NewClient()
		require.NoError(t, err)
		namespace := NamespaceForTest(ctx, t, client)
		DumpLogsOnFailure(t, namespace)
		testGatewayJWTMetadata(ctx, t, client, namespace)
		return ctx
	}).Feature()
	testEnv.Test(t, feature)
}

func testGatewayJWTMetadata(ctx context.Context, t *testing.T, client klient.Client, namespace string) {
	t.Helper()
	const host = "gateway-jwt-metadata.localdev.me"
	const path = "/.well-known/oauth-protected-resource/mcp"
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	public := pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(&key.PublicKey)})
	mustCreateImmutableSecret(ctx, t, client, namespace, "jwt", map[string][]byte{"pubkey.pem": public})
	body := `{"resource":"https://` + host + `/mcp","authorization_servers":["https://login.example.com"]}`
	document := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "oauth", Namespace: namespace}, Data: map[string]string{"metadata.json": body}}
	require.NoError(t, client.Resources(namespace).Create(ctx, document))
	for _, discovery := range []bool{false, true} {
		name := "ordinary"
		jwt := map[string]any{"secretRef": map[string]any{"name": "jwt"}, "allowedGroups": []any{"admins"}}
		if discovery {
			name = "discovery"
			jwt["resourceMetadata"] = map[string]any{"configMapRef": map[string]any{"name": "oauth"}}
		}
		policy := gatewayRoutePolicy(namespace, name, map[string]any{"authentication": map[string]any{"jwt": jwt}})
		require.NoError(t, client.Resources().Create(ctx, policy))
	}
	backend := NewEchoServerBackend(ctx, t, client, namespace)
	NewTLSSecret(ctx, t, client, namespace, "certificate", []string{host})
	for _, name := range []string{"policy", "other"} {
		NewHTTPSGateway(ctx, t, namespace, name, "certificate")
	}
	route := gatewayPolicyRoute("HTTPRoute", namespace, host, []any{gatewayPolicyHTTPRule(backend, "/mcp", "discovery")})
	require.NoError(t, client.Resources().Create(ctx, route))
	other := gatewayPolicyRoute("HTTPRoute", namespace, host, []any{gatewayPolicyHTTPRule(backend, "/", "ordinary")})
	other.SetName("other")
	require.NoError(t, unstructured.SetNestedSlice(other.Object, []any{map[string]any{"name": "other"}}, "spec", "parentRefs"))
	require.NoError(t, client.Resources().Create(ctx, other))
	waitForRouteDeployed(ctx, t, client, httpRouteGVR, namespace, route.GetName())
	waitForRouteDeployed(ctx, t, client, httpRouteGVR, namespace, other.GetName())
	primaryPort := ForwardGateway(ctx, t, namespace, "policy", 443)
	otherPort := ForwardGateway(ctx, t, namespace, "other", 443)
	primary := httpclient.ForForwarded(t, 0, primaryPort.HTTPSPort)
	secondary := httpclient.ForForwarded(t, 0, otherPort.HTTPSPort)
	challenge := `Bearer resource_metadata="https://` + host + path + `"`
	primary.HTTPS(host, "/mcp").ExpectHeader(t, "WWW-Authenticate", challenge)
	primary.HTTPS(host, path).ExpectMatching(t, "Gateway public metadata", func(response *httpclient.Response) bool {
		return response.Status == http.StatusOK && string(response.Body) == body
	})
	assertJWTMetadataGroups(t, primary, key, host)
	response := secondary.HTTPS(host, path).ExpectStatus(t, http.StatusUnauthorized)
	require.Empty(t, response.Header.Get("WWW-Authenticate"))
	require.NoError(t, client.Resources(namespace).Delete(ctx, document))
	response = primary.HTTPS(host, "/mcp").ExpectStatus(t, http.StatusServiceUnavailable)
	require.Empty(t, response.Header.Get("WWW-Authenticate"))
	primary.HTTPS(host, path).ExpectStatus(t, http.StatusNotFound)
	document = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "oauth", Namespace: namespace}, Data: map[string]string{"metadata.json": body}}
	require.NoError(t, client.Resources(namespace).Create(ctx, document))
	primary.HTTPS(host, path).ExpectOK(t)
	primary.HTTPS(host, "/mcp").ExpectHeader(t, "WWW-Authenticate", challenge)
}

func assertJWTMetadataGroups(t *testing.T, requests *httpclient.Client, key *rsa.PrivateKey, host string) {
	t.Helper()
	wrongGroup := signRS256(t, key, map[string]any{"alg": "RS256"}, map[string]any{"groups": []string{"guests"}, "exp": time.Now().Add(time.Hour).Unix()})
	response := requests.HTTPS(host, "/mcp").WithHeader("Authorization", "Bearer "+wrongGroup).ExpectStatus(t, http.StatusForbidden)
	require.Empty(t, response.Header.Get("WWW-Authenticate"))
	allowed := signRS256(t, key, map[string]any{"alg": "RS256"}, map[string]any{"groups": []string{"admins"}, "exp": time.Now().Add(time.Hour).Unix()})
	requests.HTTPS(host, "/mcp").WithHeader("Authorization", "Bearer "+allowed).ExpectEchoPath(t, "/mcp")
}
