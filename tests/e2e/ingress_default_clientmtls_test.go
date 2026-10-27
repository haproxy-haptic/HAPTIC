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

func TestHapticDefaultCertificateMTLS(t *testing.T) {
	// Hostname-less Gateway tests replace the shared default certificate.
	bundle, err := generateMTLSBundle(defaultCertificateMTLSHost)
	if err != nil {
		t.Fatalf("generate mTLS bundle: %v", err)
	}
	var serverCA []byte
	publicHost := "ingress-haptic-default-public.localdev.me"
	feature := features.New("Ingress: mTLS for an exact SAN on the default certificate").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			client, err := cfg.NewClient()
			if err != nil {
				t.Fatalf("new client: %v", err)
			}
			ns := NamespaceForTest(ctx, t, client)
			DumpLogsOnFailure(t, ns)
			backend := NewEchoServerBackend(ctx, t, client, ns)
			var defaultCert corev1.Secret
			if err := client.Resources(ControllerNamespace).Get(ctx, "default-ssl-cert", ControllerNamespace, &defaultCert); err != nil {
				t.Fatalf("read default certificate: %v", err)
			}
			serverCA = defaultCert.Data[corev1.TLSCertKey]
			clientCA := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "client-ca", Namespace: ns},
				Data:       map[string][]byte{"ca.crt": bundle.CACertPEM},
			}
			if err := client.Resources(ns).Create(ctx, clientCA); err != nil {
				t.Fatalf("create client CA: %v", err)
			}
			NewIngress(ctx, t, client, ns, &IngressSpec{
				Name: "verified", Host: defaultCertificateMTLSHost, Path: "/",
				BackendService: backend.Service, BackendPort: backend.Port,
				Annotations: map[string]string{
					"haproxy-haptic.org/auth-tls-secret":        "client-ca",
					"haproxy-haptic.org/auth-tls-verify-client": "on",
					"haproxy-haptic.org/auth-tls-match-subject": "CN=test-client",
					"haproxy-haptic.org/auth-tls-cn-header":     "X-MTLS-CN",
					"haproxy-haptic.org/auth-tls-cert-header":   "true",
				},
			})
			NewIngress(ctx, t, client, ns, &IngressSpec{
				Name: "public", Host: publicHost, Path: "/",
				BackendService: backend.Service, BackendPort: backend.Port,
			})
			return ctx
		}).
		Assess("trusted client passes subject matching and forwards its CN", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			httpclient.New(t).HTTPS(defaultCertificateMTLSHost, "/").
				WithClientCert(bundle.ClientCertPEM, bundle.ClientKeyPEM, serverCA).
				ExpectEchoHeader(t, "X-MTLS-CN", "test-client")
			return ctx
		}).
		Assess("client identity survives TLS session resumption", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			assertClientCertificateResumption(ctx, t, bundle, serverCA, defaultCertificateMTLSHost, "X-MTLS-CN")
			return ctx
		}).
		Assess("missing or untrusted client certificates fail TLS", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			if _, err := httpclient.New(t).HTTPS(defaultCertificateMTLSHost, "/").Do(ctx); err == nil {
				t.Fatal("request without a client certificate passed TLS")
			}
			if _, err := httpclient.New(t).HTTPS(defaultCertificateMTLSHost, "/").
				WithClientCert(bundle.WrongCertPEM, bundle.WrongKeyPEM, serverCA).Do(ctx); err == nil {
				t.Fatal("request with an untrusted client certificate passed TLS")
			}
			return ctx
		}).
		Assess("an unmatched public host keeps default certificate fallback", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			httpclient.New(t).HTTPS(publicHost, "/").ExpectStatus(t, http.StatusOK)
			return ctx
		}).
		Feature()
	testEnv.Test(t, feature)
}
