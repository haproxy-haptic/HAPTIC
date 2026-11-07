// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient"

	"github.com/haproxy-haptic/HAPTIC/tests/e2e/httpclient"
)

func TestHapticJWTMetadata(t *testing.T) {
	t.Parallel()
	for _, root := range []bool{false, true} {
		name := "suffix"
		if root {
			name = "root"
		}
		t.Run(name, func(t *testing.T) { testHapticJWTMetadata(t, name, root) })
	}
}

func testHapticJWTMetadata(t *testing.T, name string, root bool) {
	t.Helper()
	host := "jwt-metadata-" + name + ".localdev.me"
	metadataPath := "/.well-known/oauth-protected-resource/mcp"
	annotations := map[string]string{
		"haproxy-haptic.org/jwt-secret":                      "jwt-keys",
		"haproxy-haptic.org/jwt-issuer":                      "https://login.example.com",
		"haproxy-haptic.org/jwt-audience":                    "mcp",
		"haproxy-haptic.org/jwt-allowed-groups":              "admins",
		"haproxy-haptic.org/jwt-resource-metadata-configmap": "oauth:document.json",
		"haproxy-haptic.org/error-pages":                     "refusal",
	}
	if root {
		metadataPath = "/.well-known/oauth-protected-resource"
		annotations["haproxy-haptic.org/jwt-resource-metadata-path"] = metadataPath
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	public := pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(&key.PublicKey)})
	token := jwtMetadataBearer(t, key, nil)
	wrongGroup := jwtMetadataBearer(t, key, map[string]any{"groups": []string{"guests"}})
	var client klient.Client
	var document *corev1.ConfigMap
	body := `{"resource":"https://` + host + `/mcp","authorization_servers":["https://login.example.com"],"display_name":"%[req.hdr(secret)]","extension":"` + strings.Repeat("/", 5200) + `"}`
	RunSimpleIngressTest(t, &SimpleIngressTest{
		Description: "JWT protected-resource discovery " + name,
		Host:        host, Path: "/mcp", TLSSecretName: "certificate", Annotations: annotations,
		PreSetup: func(ctx context.Context, t *testing.T, k8s klient.Client, namespace string) {
			t.Helper()
			client = k8s
			mustCreateSecret(ctx, t, k8s, namespace, "jwt-keys", map[string][]byte{"pubkey.pem": public})
			document = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "oauth", Namespace: namespace}, Data: map[string]string{"document.json": body}}
			require.NoError(t, k8s.Resources(namespace).Create(ctx, document))
			refusal := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "refusal", Namespace: namespace}, Data: map[string]string{
				"401.type": "lf-file", "401.content-type": "text/plain", "401.body": "Sign in to continue",
			}}
			require.NoError(t, k8s.Resources(namespace).Create(ctx, refusal))
		},
		Assess: []SimpleIngressAssertion{{Name: "public discovery and JWT challenge", Check: func(t *testing.T, host string) {
			t.Helper()
			requests := httpclient.New(t)
			challenge := `Bearer resource_metadata="https://` + host + metadataPath + `"`
			responses := assertJWTMetadataChallenges(t, key, challenge, func() *httpclient.Request {
				return requests.GET(host, "/mcp")
			})
			for _, response := range responses {
				require.Equal(t, "Sign in to continue", string(response.Body))
				require.Equal(t, "text/plain", response.Header.Get("Content-Type"))
			}
			for _, request := range []*httpclient.Request{requests.GET(host, metadataPath), requests.HTTPS(host, metadataPath), requests.GET(host, metadataPath).WithHostHeader(host + ":443")} {
				response := request.WithHeader("secret", "must-not-expand").ExpectMatching(t, "public metadata without backend response", func(response *httpclient.Response) bool {
					return response.Status == http.StatusOK && string(response.Body) == body
				})
				require.True(t, json.Valid(response.Body))
				require.Equal(t, "application/json", response.Header.Get("Content-Type"))
				require.Equal(t, "no-store", response.Header.Get("Cache-Control"))
				require.Empty(t, response.Header.Get("WWW-Authenticate"))
			}
			response := requests.GET(host, "/mcp").WithHeader("Authorization", wrongGroup).ExpectStatus(t, http.StatusForbidden)
			require.Empty(t, response.Header.Get("WWW-Authenticate"))
			response = requests.GET(host, metadataPath).WithMethod(http.MethodHead).ExpectOK(t)
			require.Empty(t, response.Body)
			response = requests.GET(host, metadataPath).WithMethod(http.MethodPost).ExpectStatus(t, http.StatusMethodNotAllowed)
			require.Equal(t, "GET, HEAD", response.Header.Get("Allow"))
			requests.GET("other-"+host, metadataPath).ExpectStatus(t, http.StatusNotFound)
			requests.GET(host, "/mcp").WithHeader("Authorization", token).ExpectEchoPath(t, "/mcp")
			require.NoError(t, client.Resources(document.Namespace).Get(t.Context(), document.Name, document.Namespace, document))
			updated := `{"resource":"https://` + host + `/mcp","authorization_servers":["https://new.example.com"],"revision":9007199254740993}`
			document.Data["document.json"] = updated
			require.NoError(t, client.Resources(document.Namespace).Update(t.Context(), document))
			requests.GET(host, metadataPath).ExpectMatching(t, "ConfigMap edit updates public metadata", func(response *httpclient.Response) bool {
				return response.Status == http.StatusOK && string(response.Body) == updated
			})
			requests.GET(host, "/mcp").ExpectHeader(t, "WWW-Authenticate", challenge)
			// A resource on a host the Ingress doesn't serve denies only this route; other Ingresses stay admissible (#644).
			document.Data["document.json"] = `{"resource":"https://moved-` + host + `/mcp"}`
			require.NoError(t, client.Resources(document.Namespace).Update(t.Context(), document))
			requests.GET(host, "/mcp").WithHeader("Authorization", token).ExpectStatus(t, http.StatusServiceUnavailable)
			ingress := &networkingv1.Ingress{}
			require.NoError(t, client.Resources(document.Namespace).Get(t.Context(), "echo", document.Namespace, ingress))
			backend := ingress.Spec.Rules[0].HTTP.Paths[0].Backend.Service
			NewIngress(t.Context(), t, client, document.Namespace, &IngressSpec{
				Name: "unrelated", Host: "other-" + host, Path: "/alive", BackendService: backend.Name, BackendPort: backend.Port.Number,
			})
			requests.GET("other-"+host, "/alive").ExpectEchoPath(t, "/alive")
			require.NoError(t, client.Resources(document.Namespace).Get(t.Context(), document.Name, document.Namespace, document))
			document.Data["document.json"] = updated
			require.NoError(t, client.Resources(document.Namespace).Update(t.Context(), document))
			requests.GET(host, metadataPath).ExpectMatching(t, "corrected ConfigMap republishes metadata", func(response *httpclient.Response) bool {
				return response.Status == http.StatusOK && string(response.Body) == updated
			})
			require.NoError(t, client.Resources(document.Namespace).Delete(t.Context(), document))
			response = requests.GET(host, "/mcp").WithHeader("Authorization", token).ExpectStatus(t, http.StatusServiceUnavailable)
			require.Empty(t, response.Header.Get("WWW-Authenticate"))
			requests.GET(host, metadataPath).ExpectStatus(t, http.StatusNotFound)
			requests.GET("other-"+host, "/alive").ExpectEchoPath(t, "/alive")
			document = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "oauth", Namespace: document.Namespace}, Data: map[string]string{"document.json": ""}}
			require.NoError(t, client.Resources(document.Namespace).Create(t.Context(), document))
			requests.GET(host, "/mcp").ExpectStatus(t, http.StatusServiceUnavailable)
			document.Data["document.json"] = updated
			require.NoError(t, client.Resources(document.Namespace).Update(t.Context(), document))
			requests.GET(host, metadataPath).ExpectMatching(t, "restored ConfigMap republishes metadata", func(response *httpclient.Response) bool {
				return response.Status == http.StatusOK && string(response.Body) == updated
			})
			requests.GET(host, "/mcp").WithHeader("Authorization", token).ExpectEchoPath(t, "/mcp")
			require.NoError(t, client.Resources(document.Namespace).Delete(t.Context(), ingress))
		}}},
	})
}

func jwtMetadataBearer(t *testing.T, key *rsa.PrivateKey, overrides map[string]any) string {
	t.Helper()
	claims := map[string]any{
		"sub": "alice", "iss": "https://login.example.com", "aud": "mcp",
		"groups": []string{"admins"}, "exp": time.Now().Add(time.Hour).Unix(),
	}
	for field, value := range overrides {
		claims[field] = value
	}
	return "Bearer " + signRS256(t, key, map[string]any{"alg": "RS256"}, claims)
}

func assertJWTMetadataChallenges(t *testing.T, key *rsa.PrivateKey, challenge string, newRequest func() *httpclient.Request) []*httpclient.Response {
	t.Helper()
	cases := []struct {
		name, authorization string
		invalid             bool
	}{
		{"missing token", "", false},
		{"empty Bearer", "Bearer ", false},
		{"unsupported scheme", "Basic dXNlcjpwYXNz", false},
		{"malformed token", "Bearer abc.def.ghi", true},
		{"bad signature", "Bearer eyJhbGciOiJSUzI1NiJ9.e30.AAAA", true},
		{"expired token", jwtMetadataBearer(t, key, map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}), true},
		{"future token", jwtMetadataBearer(t, key, map[string]any{"nbf": time.Now().Add(time.Hour).Unix()}), true},
		{"wrong issuer", jwtMetadataBearer(t, key, map[string]any{"iss": "https://other.example.com"}), true},
		{"wrong audience", jwtMetadataBearer(t, key, map[string]any{"aud": "other"}), true},
	}
	responses := make([]*httpclient.Response, 0, len(cases))
	for _, tc := range cases {
		expected := challenge
		if tc.invalid {
			expected += `, error="invalid_token"`
		}
		request := newRequest().WithMethod(http.MethodPost)
		if tc.authorization != "" {
			request = request.WithHeader("Authorization", tc.authorization)
		}
		response := request.ExpectMatching(t, tc.name+" challenge", func(response *httpclient.Response) bool {
			return response.Status == http.StatusUnauthorized && response.Header.Get("WWW-Authenticate") == expected
		})
		require.Len(t, response.Header.Values("WWW-Authenticate"), 1)
		responses = append(responses, response)
	}
	return responses
}
