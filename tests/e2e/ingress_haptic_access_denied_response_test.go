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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient"

	"github.com/haproxy-haptic/HAPTIC/tests/e2e/httpclient"
)

// TestHapticAccessDeniedResponse covers haproxy-haptic.org/access-denied-response
// (#571): a source the allowlist refuses gets the ConfigMap's lf-file page,
// with the requested host filled in, instead of the default 403 page.
func TestHapticAccessDeniedResponse(t *testing.T) {
	t.Parallel()
	const configMap = "access-denied-page"
	RunSimpleIngressTest(t, &SimpleIngressTest{
		Description: "Ingress: access-denied-response via haproxy-haptic.org",
		Host:        "ingress-haptic-access-denied.localdev.me",
		Annotations: map[string]string{
			"haproxy-haptic.org/allowlist-source-range": "192.0.2.10/32",
			"haproxy-haptic.org/access-denied-response": configMap,
		},
		PreSetup: func(ctx context.Context, t *testing.T, client klient.Client, namespace string) {
			t.Helper()
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: configMap, Namespace: namespace},
				Data: map[string]string{
					"type":         "lf-file",
					"status":       "403",
					"content-type": "text/plain",
					"body":         "access to %[req.hdr(host)] is not enabled for your address",
				},
			}
			if err := client.Resources(namespace).Create(ctx, cm); err != nil {
				t.Fatalf("create access-denied ConfigMap: %v", err)
			}
		},
		Assess: []SimpleIngressAssertion{{
			Name: "a refused source gets the ConfigMap's page with the host filled in",
			Check: func(t *testing.T, host string) {
				t.Helper()
				want := "access to " + host + " is not enabled for your address"
				resp := httpclient.New(t).GET(host, "/").ExpectMatching(t, "403 with the custom page", func(r *httpclient.Response) bool {
					return r.Status == 403 && strings.TrimSpace(string(r.Body)) == want
				})
				if got := resp.Header.Get("Content-Type"); got != "text/plain" {
					t.Fatalf("Content-Type = %q, want text/plain", got)
				}
			},
		}},
	})
}
