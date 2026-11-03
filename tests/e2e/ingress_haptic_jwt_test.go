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
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"testing"
	"time"

	"sigs.k8s.io/e2e-framework/klient"

	"github.com/haproxy-haptic/HAPTIC/tests/e2e/httpclient"
)

// b64url is JWT base64url (no padding).
func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// signRS256 assembles a JWT with the given header/payload and RS256-signs it.
func signRS256(t *testing.T, priv *rsa.PrivateKey, header, payload map[string]any) string {
	t.Helper()
	hb, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal JWT header: %v", err)
	}
	pb, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal JWT payload: %v", err)
	}
	signingInput := b64url(hb) + "." + b64url(pb)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	return signingInput + "." + b64url(sig)
}

// TestHapticJWT verifies asymmetric JWT verification (60-jwt-auth.yaml): a valid
// RS256 token reaches the upstream; a missing, expired, or algorithm-confused
// token is denied 401. The Secret holds only the RSA public key.
func TestHapticJWT(t *testing.T) {
	t.Parallel()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})

	rs256 := map[string]any{"alg": "RS256", "typ": "JWT"}
	validToken := signRS256(t, priv, rs256, map[string]any{
		"sub": "alice",
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	})
	expiredToken := signRS256(t, priv, rs256, map[string]any{
		"sub": "alice",
		"exp": time.Now().Add(-1 * time.Hour).Unix(),
	})
	// Alg-confusion attempt: header claims HS256 (attacker downgrade). The
	// signature is irrelevant — the alg guard rejects it before verify.
	algConfusedToken := signRS256(t, priv, map[string]any{"alg": "HS256", "typ": "JWT"}, map[string]any{
		"sub": "mallory",
		"exp": time.Now().Add(1 * time.Hour).Unix(),
	})

	RunSimpleIngressTest(t, &SimpleIngressTest{
		Description: "Ingress: HAPTIC-native JWT verification",
		Host:        "ingress-haptic-jwt.localdev.me",
		Annotations: map[string]string{
			"haproxy-haptic.org/jwt-secret":    "jwt-keys",
			"haproxy-haptic.org/jwt-algorithm": "RS256",
		},
		PreSetup: func(ctx context.Context, t *testing.T, client klient.Client, namespace string) {
			t.Helper()
			mustCreateSecret(ctx, t, client, namespace, "jwt-keys", map[string][]byte{
				"pubkey.pem": pubPEM,
			})
		},
		Assess: []SimpleIngressAssertion{
			{
				Name: "no token returns 401",
				Check: func(t *testing.T, host string) {
					t.Helper()
					httpclient.New(t).GET(host, "/").ExpectStatus(t, http.StatusUnauthorized)
				},
			},
			{
				Name: "valid RS256 token reaches upstream (200)",
				Check: func(t *testing.T, host string) {
					t.Helper()
					httpclient.New(t).GET(host, "/").WithHeader("Authorization", "Bearer "+validToken).ExpectStatus(t, http.StatusOK)
				},
			},
			{
				Name: "expired token returns 401",
				Check: func(t *testing.T, host string) {
					t.Helper()
					httpclient.New(t).GET(host, "/").WithHeader("Authorization", "Bearer "+expiredToken).ExpectStatus(t, http.StatusUnauthorized)
				},
			},
			{
				Name: "algorithm-confused (HS256 header) token returns 401",
				Check: func(t *testing.T, host string) {
					t.Helper()
					httpclient.New(t).GET(host, "/").WithHeader("Authorization", "Bearer "+algConfusedToken).ExpectStatus(t, http.StatusUnauthorized)
				},
			},
		},
	})
}

func TestHapticJWTClaimConstraints(t *testing.T) {
	t.Parallel()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	for _, tc := range []struct {
		name, issuer, audience string
	}{
		{"ordinary", "https://issuer.example.com", "api"},
		{"underscore", "_", "_"},
		{"numeric", "https://issuer.example.com", "123"},
		{"boolean", "https://issuer.example.com", "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			RunSimpleIngressTest(t, &SimpleIngressTest{
				Description: "JWT issuer and audience constraints",
				Host:        "jwt-claims-" + tc.name + ".localdev.me",
				Annotations: map[string]string{
					"haproxy-haptic.org/jwt-secret":   "jwt-keys",
					"haproxy-haptic.org/jwt-issuer":   tc.issuer,
					"haproxy-haptic.org/jwt-audience": tc.audience,
				},
				PreSetup: func(ctx context.Context, t *testing.T, client klient.Client, namespace string) {
					t.Helper()
					mustCreateSecret(ctx, t, client, namespace, "jwt-keys", map[string][]byte{"pubkey.pem": pubPEM})
				},
				Assess: jwtClaimAssertions(t, priv, tc.issuer, tc.audience),
			})
		})
	}
}

func jwtClaimAssertions(t *testing.T, priv *rsa.PrivateKey, issuer, audience string) []SimpleIngressAssertion {
	t.Helper()
	assertions := make([]SimpleIngressAssertion, 0, 18)
	for _, tc := range []struct {
		name, issuer string
		audience     any
		status       int
	}{
		{"matching claims", issuer, audience, http.StatusOK},
		{"wrong issuer", "wrong", audience, http.StatusUnauthorized},
		{"missing issuer", "", audience, http.StatusUnauthorized},
		{"wrong audience", issuer, "wrong", http.StatusUnauthorized},
		{"missing audience", issuer, nil, http.StatusUnauthorized},
		{"array first", issuer, []string{audience, "other"}, http.StatusOK},
		{"array middle", issuer, []string{"other", audience, "last"}, http.StatusOK},
		{"array last", issuer, []string{"other", audience}, http.StatusOK},
		{"escaped array value", issuer, json.RawMessage(fmt.Sprintf(`["\u%04x%s"]`, audience[0], audience[1:])), http.StatusOK},
		{"substring only", issuer, []string{"prefix-" + audience, audience + "-suffix"}, http.StatusUnauthorized},
		{"empty array", issuer, []string{}, http.StatusUnauthorized},
		{"nested array", issuer, [][]string{{audience}}, http.StatusUnauthorized},
		{"mixed array", issuer, []any{audience, true}, http.StatusUnauthorized},
		{"array-shaped string", issuer, fmt.Sprintf(`[%q]`, audience), http.StatusUnauthorized},
		{"numeric audience", issuer, 123, http.StatusUnauthorized},
		{"boolean audience", issuer, true, http.StatusUnauthorized},
	} {
		claims := map[string]any{"sub": "alice", "exp": time.Now().Add(time.Hour).Unix()}
		if tc.issuer != "" {
			claims["iss"] = tc.issuer
		}
		if tc.audience != nil {
			claims["aud"] = tc.audience
		}
		token := signRS256(t, priv, map[string]any{"alg": "RS256", "typ": "JWT"}, claims)
		assertions = append(assertions, SimpleIngressAssertion{
			Name: tc.name,
			Check: func(t *testing.T, host string) {
				t.Helper()
				httpclient.New(t).GET(host, "/").WithHeader("Authorization", "Bearer "+token).ExpectStatus(t, tc.status)
			},
		})
	}
	return assertions
}
