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
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/e2e-framework/klient"

	"github.com/haproxy-haptic/HAPTIC/tests/e2e/httpclient"
)

func TestHapticJWTGroups(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	public := pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(&key.PublicKey)})
	for _, tc := range []struct{ name, claim, group string }{
		{"default", "", "admins"},
		{"custom", "roles", "operators"},
		{"numeric", "groups", "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			annotations := map[string]string{
				"haproxy-haptic.org/jwt-secret":         "jwt-keys",
				"haproxy-haptic.org/jwt-allowed-groups": tc.group + ",second," + tc.group,
			}
			claim := tc.claim
			if claim == "" {
				claim = "groups"
			} else {
				annotations["haproxy-haptic.org/jwt-groups-claim"] = claim
			}
			RunSimpleIngressTest(t, &SimpleIngressTest{
				Description: "JWT claim membership authorization",
				Host:        "jwt-groups-" + tc.name + ".localdev.me",
				Annotations: annotations,
				PreSetup: func(ctx context.Context, t *testing.T, client klient.Client, namespace string) {
					t.Helper()
					mustCreateSecret(ctx, t, client, namespace, "jwt-keys", map[string][]byte{"pubkey.pem": public})
				},
				Assess: jwtGroupAssertions(t, key, claim, tc.group),
			})
		})
	}
}

func jwtGroupAssertions(t *testing.T, key *rsa.PrivateKey, claim, group string) []SimpleIngressAssertion {
	t.Helper()
	assertions := make([]SimpleIngressAssertion, 0, 18)
	for _, tc := range []struct {
		name    string
		value   any
		expired bool
		status  int
	}{
		{"scalar member", group, false, http.StatusOK},
		{"second allowed group", "second", false, http.StatusOK},
		{"second allowed group array", []string{"other", "second"}, false, http.StatusOK},
		{"array first", []string{group, "other"}, false, http.StatusOK},
		{"array last", []string{"other", group}, false, http.StatusOK},
		{"escaped scalar", json.RawMessage(fmt.Sprintf(`"\u%04x%s"`, group[0], group[1:])), false, http.StatusOK},
		{"escaped array", json.RawMessage(fmt.Sprintf(`["\u%04x%s"]`, group[0], group[1:])), false, http.StatusOK},
		{"missing claim", nil, false, http.StatusForbidden},
		{"wrong group", "outsiders", false, http.StatusForbidden},
		{"substring group", []string{"prefix-" + group, group + "-suffix"}, false, http.StatusForbidden},
		{"empty array", []string{}, false, http.StatusForbidden},
		{"mixed array", []any{group, true}, false, http.StatusForbidden},
		{"nested array", [][]string{{group}}, false, http.StatusForbidden},
		{"array-shaped scalar", fmt.Sprintf(`[%q]`, group), false, http.StatusForbidden},
		{"number", 1, false, http.StatusForbidden},
		{"boolean", true, false, http.StatusForbidden},
		{"object", map[string]string{"name": group}, false, http.StatusForbidden},
		{"expired member", group, true, http.StatusUnauthorized},
	} {
		expires := time.Now().Add(time.Hour)
		if tc.expired {
			expires = time.Now().Add(-time.Hour)
		}
		claims := map[string]any{"sub": "alice", "exp": expires.Unix()}
		if claim != "groups" {
			claims["groups"] = group
		}
		if tc.value != nil {
			claims[claim] = tc.value
		}
		token := signRS256(t, key, map[string]any{"alg": "RS256"}, claims)
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
