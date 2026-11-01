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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/klient"

	"github.com/haproxy-haptic/HAPTIC/tests/e2e/httpclient"
)

// TestHapticErrorPages covers haproxy-haptic.org/error-pages (#577): once the
// backend has no endpoints, HAProxy's 503 is the ConfigMap's lf-file page.
func TestHapticErrorPages(t *testing.T) {
	t.Parallel()
	const configMap = "error-pages"
	var k8s klient.Client
	var ns string
	RunSimpleIngressTest(t, &SimpleIngressTest{
		Description: "Ingress: error-pages via haproxy-haptic.org",
		Host:        "ingress-haptic-error-pages.localdev.me",
		Annotations: map[string]string{"haproxy-haptic.org/error-pages": configMap},
		PreSetup: func(ctx context.Context, t *testing.T, client klient.Client, namespace string) {
			t.Helper()
			k8s, ns = client, namespace
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: configMap, Namespace: namespace},
				Data: map[string]string{
					"503.type":         "lf-file",
					"503.content-type": "text/plain",
					"503.body":         "%[req.hdr(host),lower,url_enc] is down for maintenance",
				},
			}
			if err := client.Resources(namespace).Create(ctx, cm); err != nil {
				t.Fatalf("create error-pages ConfigMap: %v", err)
			}
		},
		Assess: []SimpleIngressAssertion{{
			Name: "a backend without endpoints answers with the ConfigMap's 503 page",
			Check: func(t *testing.T, host string) {
				t.Helper()
				ctx := context.Background()
				var deployment appsv1.Deployment
				if err := k8s.Resources(ns).Get(ctx, EchoServerBackend.Service, ns, &deployment); err != nil {
					t.Fatalf("get echo-server Deployment: %v", err)
				}
				zero := int32(0)
				deployment.Spec.Replicas = &zero
				if err := k8s.Resources(ns).Update(ctx, &deployment); err != nil {
					t.Fatalf("scale echo-server to zero: %v", err)
				}
				want := host + " is down for maintenance"
				resp := httpclient.New(t).GET(host, "/").ExpectMatching(t, "503 with the custom page", func(r *httpclient.Response) bool {
					return r.Status == 503 && strings.TrimSpace(string(r.Body)) == want
				})
				if got := resp.Header.Get("Content-Type"); got != "text/plain" {
					t.Fatalf("Content-Type = %q, want text/plain", got)
				}
			},
		}},
	})
}

// TestHapticErrorPage403 covers the error-pages 403 entry (#571): a source the
// allowlist refuses gets the ConfigMap's lf-file page with the host filled in.
func TestHapticErrorPage403(t *testing.T) {
	t.Parallel()
	const configMap = "error-pages"
	RunSimpleIngressTest(t, &SimpleIngressTest{
		Description: "Ingress: error-pages 403 via haproxy-haptic.org",
		Host:        "ingress-haptic-error-page-403.localdev.me",
		Annotations: map[string]string{
			"haproxy-haptic.org/allowlist-source-range": "192.0.2.10/32",
			"haproxy-haptic.org/error-pages":            configMap,
		},
		PreSetup: func(ctx context.Context, t *testing.T, client klient.Client, namespace string) {
			t.Helper()
			cm := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: configMap, Namespace: namespace},
				Data: map[string]string{
					"403.type":         "lf-file",
					"403.content-type": "text/plain",
					"403.body":         "access to %[req.hdr(host),lower,url_enc] is not enabled for your address",
				},
			}
			if err := client.Resources(namespace).Create(ctx, cm); err != nil {
				t.Fatalf("create error-pages ConfigMap: %v", err)
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
