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
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/haproxy-haptic/HAPTIC/tests/e2e/httpclient"
)

// TestHapticClientMTLS covers incoming client-certificate mTLS driven by the
// HAPTIC-native annotations (prefix haproxy-haptic.org/*), the canonical
// superset of the haproxy-ingress vendor keys exercised by
// TestIngressAuthTLSSecret.
//
// Keys under test:
//   - haproxy-haptic.org/auth-tls-secret         → Opaque Secret holding ca.crt
//   - haproxy-haptic.org/auth-tls-verify-client  → "on" (maps to verify required)
//   - haproxy-haptic.org/auth-tls-cert-header    → "true" (forward X-SSL-Client-*)
//
// The 50-auth-spoe.yaml fragment's features-110-haptic-auth-tls registers the
// referenced Secret's ca.crt into gf["clientCertVerifyHosts"]; ssl.yaml then
// emits `[ca-file <path> verify required]` on the crt-list line for the host,
// and frontend-filters-820-haptic-auth-tls-cert-header forwards the client
// cert's CN/DN/DER to the backend as request headers. Sub-cases:
//
//   - valid client cert (signed by the trusted CA) → 200, and the backend sees
//     X-SSL-Client-CN echoing the client cert CN ("test-client")
//   - no client cert                               → TLS handshake fails
//   - client cert signed by an UNTRUSTED CA        → TLS handshake fails
func TestHapticClientMTLS(t *testing.T) {
	t.Parallel()
	host := "ingress-haptic-clientmtls.localdev.me"

	// Generate the cert bundle once for the whole test (Setup + all
	// Asserts share it). Done outside the e2e-framework Setup callback
	// because the bundle is a value, not a k8s resource.
	bundle, err := generateMTLSBundle(host)
	if err != nil {
		t.Fatalf("generate mTLS bundle: %v", err)
	}

	feature := features.New("Ingress: client-mTLS via haproxy-haptic.org/auth-tls-secret").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			client, err := cfg.NewClient()
			if err != nil {
				t.Fatalf("new client: %v", err)
			}
			ns := NamespaceForTest(ctx, t, client)
			DumpLogsOnFailure(t, ns)
			backend := NewEchoServerBackend(ctx, t, client, ns)

			// Server-side TLS Secret (haproxy frontend cert).
			serverSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "haptic-mtls-server-tls",
					Namespace: ns,
				},
				Type: corev1.SecretTypeTLS,
				Data: map[string][]byte{
					"tls.crt": bundle.ServerCertPEM,
					"tls.key": bundle.ServerKeyPEM,
				},
			}
			if err := client.Resources(ns).Create(ctx, serverSecret); err != nil {
				t.Fatalf("create server TLS secret: %v", err)
			}

			// Client-CA bundle Secret (auth-tls-secret references this).
			// The fragment reads data.ca.crt; matches the vendor shape.
			caSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "haptic-mtls-client-ca",
					Namespace: ns,
				},
				Type: corev1.SecretTypeOpaque,
				Data: map[string][]byte{
					"ca.crt": bundle.CACertPEM,
				},
			}
			if err := client.Resources(ns).Create(ctx, caSecret); err != nil {
				t.Fatalf("create client-CA secret: %v", err)
			}

			NewIngress(ctx, t, client, ns, &IngressSpec{
				Name:           "echo-haptic-mtls",
				Host:           host,
				Path:           "/",
				BackendService: backend.Service,
				BackendPort:    backend.Port,
				TLSSecretName:  "haptic-mtls-server-tls",
				Annotations: map[string]string{
					"haproxy-haptic.org/auth-tls-secret":        "haptic-mtls-client-ca",
					"haproxy-haptic.org/auth-tls-verify-client": "on",
					"haproxy-haptic.org/auth-tls-cert-header":   "true",
				},
			})
			return ctx
		}).
		Assess("valid client cert is admitted and the backend sees the forwarded cert header", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			// Poll on the echo'd X-SSL-Client-CN header: a 200 can land
			// from the crt-list `verify required` state before the
			// cert-header forwarding rule is live, so polling on the
			// forwarded header closes that window.
			httpclient.New(t).HTTPS(host, "/").
				WithClientCert(bundle.ClientCertPEM, bundle.ClientKeyPEM, bundle.CACertPEM).
				ExpectEchoHeader(t, "X-SSL-Client-CN", "test-client")
			return ctx
		}).
		Assess("plain HTTP is redirected to HTTPS, except the ACME HTTP-01 path", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			resp := httpclient.New(t).GET(host, "/").ExpectRedirect(t, "https://"+host+"/")
			if resp.Status != http.StatusPermanentRedirect {
				t.Fatalf("expected 308 for plain HTTP to a route that requires a client certificate, got %d", resp.Status)
			}
			httpclient.New(t).GET(host, "/.well-known/acme-challenge/probe").ExpectOK(t)
			return ctx
		}).
		Assess("a request for the host over another name's TLS connection is misdirected", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			// The other name's handshake verifies no client certificate, so serving
			// this Host there would skip mTLS entirely.
			httpclient.New(t).HTTPS("ingress-haptic-clientmtls-other.localdev.me", "/").
				WithHostHeader(host).
				ExpectStatus(t, http.StatusMisdirectedRequest)
			return ctx
		}).
		Assess("missing client cert is rejected at the TLS layer", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			// No WithClientCert → TLS handshake should fail. The
			// httpclient default is insecure-skip-verify, so the only
			// failure mode is HAProxy aborting the handshake on its
			// `verify required` setting. We expect Do() to return an
			// error rather than a Response.
			_, err := httpclient.New(t).HTTPS(host, "/").Do(ctx)
			if err == nil {
				t.Fatalf("expected TLS handshake error without client cert, got success")
			}
			return ctx
		}).
		Assess("client cert from untrusted CA is rejected", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			// Present a client cert signed by a *different* CA than the
			// one in auth-tls-secret; HAProxy's `verify required` must
			// reject it at the TLS layer.
			_, err := httpclient.New(t).HTTPS(host, "/").
				WithClientCert(bundle.WrongCertPEM, bundle.WrongKeyPEM, bundle.CACertPEM).
				Do(ctx)
			if err == nil {
				t.Fatalf("expected TLS handshake error with untrusted client cert, got success")
			}
			return ctx
		}).
		Feature()

	testEnv.Test(t, feature)
}
