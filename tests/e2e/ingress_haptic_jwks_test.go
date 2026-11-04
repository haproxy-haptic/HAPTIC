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
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"

	"github.com/haproxy-haptic/HAPTIC/tests/e2e/httpclient"
)

func TestHapticJWKSRotation(t *testing.T) {
	t.Parallel()
	old, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	fresh, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	token := func(key *rsa.PrivateKey, kid string) string {
		return signRS256(t, key, map[string]any{"alg": "RS256", "kid": kid}, map[string]any{"sub": "alice", "exp": time.Now().Add(time.Hour).Unix()})
	}
	oldToken, newToken := token(old, "old"), token(fresh, "new")
	var client klient.Client
	RunSimpleIngressTest(t, &SimpleIngressTest{
		Description: "JWKS refresh, overlap, and retired keys",
		Host:        "jwks-rotation.localdev.me",
		Annotations: map[string]string{"haproxy-haptic.org/jwt-jwks-url": "https://jwks-server.haptic.svc/keys.json"},
		PreSetup: func(ctx context.Context, t *testing.T, c klient.Client, _ string) {
			t.Helper()
			client = c
			updateJWKSFixture(ctx, t, client, map[string]*rsa.PrivateKey{"old": old})
		},
		Assess: append([]SimpleIngressAssertion{{
			Name: "key rotation updates verification without changing the route",
			Check: func(t *testing.T, host string) {
				t.Helper()
				check := func(bearer string, status int) {
					t.Helper()
					httpclient.New(t).GET(host, "/").WithHeader("Authorization", "Bearer "+bearer).ExpectStatus(t, status)
				}
				check(oldToken, http.StatusOK)
				check(newToken, http.StatusUnauthorized)
				check(token(old, "unknown"), http.StatusUnauthorized)
				check(token(old, ""), http.StatusUnauthorized)
				check(token(fresh, "old"), http.StatusUnauthorized)
				updateJWKSFixture(t.Context(), t, client, map[string]*rsa.PrivateKey{"old": old, "new": fresh})
				check(newToken, http.StatusOK)
				check(oldToken, http.StatusOK)
				updateJWKSFixture(t.Context(), t, client, map[string]*rsa.PrivateKey{"new": fresh})
				check(oldToken, http.StatusUnauthorized)
				check(newToken, http.StatusOK)
				updateJWKSFixture(t.Context(), t, client, map[string]*rsa.PrivateKey{})
				check(newToken, http.StatusServiceUnavailable)
				updateJWKSFixture(t.Context(), t, client, map[string]*rsa.PrivateKey{"new": fresh})
				check(newToken, http.StatusOK)
			},
		}}, jwtDenialAssertions("jwt_signature")...),
	})
}

func updateJWKSFixture(ctx context.Context, t *testing.T, client klient.Client, keys map[string]*rsa.PrivateKey) {
	t.Helper()
	entries := make([]map[string]string, 0, len(keys))
	for id, key := range keys {
		entries = append(entries, map[string]string{"kty": "RSA", "kid": id, "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB"})
	}
	encoded, err := json.Marshal(map[string]any{"keys": entries})
	require.NoError(t, err)
	config := &corev1.ConfigMap{}
	require.NoError(t, client.Resources().Get(ctx, "jwks-server", ControllerNamespace, config))
	config.Data["keys.json"] = string(encoded)
	require.NoError(t, client.Resources(ControllerNamespace).Update(ctx, config))
	refreshJWKSFixturePods(ctx, t, client, config.ResourceVersion)
	forward := ForwardService(t, "jwks-server", 443)
	httpclient.ForForwarded(t, 0, forward.HTTPSPort).HTTPS("jwks-server.haptic.svc", "/keys.json").ExpectMatching(t, "issuer serves updated JWKS", func(response *httpclient.Response) bool {
		return response.Status == http.StatusOK && strings.TrimSpace(string(response.Body)) == string(encoded)
	})
}

func refreshJWKSFixturePods(ctx context.Context, t *testing.T, client klient.Client, revision string) {
	t.Helper()
	var pods corev1.PodList
	require.NoError(t, client.Resources(ControllerNamespace).List(ctx, &pods, resources.WithLabelSelector("app=jwks-server")))
	require.NotEmpty(t, pods.Items)
	for index := range pods.Items {
		pod := &pods.Items[index]
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		// Annotation updates refresh projected volumes: kubernetes.io/docs/tasks/configure-pod-container/configure-pod-configmap/#mounted-configmaps-are-updated-automatically.
		pod.Annotations["e2e.haproxy-haptic.org/jwks-revision"] = revision
		require.NoError(t, client.Resources(ControllerNamespace).Update(ctx, pod))
	}
}
