// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/haproxy-haptic/HAPTIC/tests/e2e/httpclient"
)

// TestHapticOAuthSignIn covers haproxy-haptic.org/oauth: an unauthenticated
// request is sent to oauth2-proxy's sign-in with its own URI as rd (#550),
// and the Ingress's /oauth2 path itself is not authenticated (#551).
//
// The oauth2-proxy Service points at a port nothing listens on, so its auth
// check fails and a request that reaches it gets 503 instead of the 302 an
// authenticated route answers with.
func TestHapticOAuthSignIn(t *testing.T) {
	t.Parallel()

	const host = "ingress-haptic-oauth.localdev.me"

	feature := features.New("Ingress: haptic oauth sign-in (haproxy-haptic.org/oauth)").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			client, err := cfg.NewClient()
			if err != nil {
				t.Fatalf("new client: %v", err)
			}
			ns := NamespaceForTest(ctx, t, client)
			DumpLogsOnFailure(t, ns)
			backend := NewEchoServerBackend(ctx, t, client, ns)

			proxy := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: "oauth2-proxy", Namespace: ns},
				Spec: corev1.ServiceSpec{
					Selector: map[string]string{"app": backend.Service},
					Ports: []corev1.ServicePort{{
						Name:       "http",
						Port:       4180,
						TargetPort: intstr.FromInt32(4180),
						Protocol:   corev1.ProtocolTCP,
					}},
				},
			}
			if err := client.Resources(ns).Create(ctx, proxy); err != nil {
				t.Fatalf("create oauth2-proxy Service: %v", err)
			}

			ing := buildIngress(ns, &IngressSpec{
				Name:           "oauth-app",
				Host:           host,
				BackendService: backend.Service,
				BackendPort:    backend.Port,
				Annotations:    map[string]string{"haproxy-haptic.org/oauth": "oauth2-proxy"},
			})
			prefix := networkingv1.PathTypePrefix
			paths := &ing.Spec.Rules[0].HTTP.Paths
			*paths = append(*paths, networkingv1.HTTPIngressPath{
				Path:     "/oauth2",
				PathType: &prefix,
				Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
					Name: proxy.Name,
					Port: networkingv1.ServiceBackendPort{Number: 4180},
				}},
			})
			if err := client.Resources(ns).Create(ctx, ing); err != nil {
				t.Fatalf("create Ingress: %v", err)
			}
			return ctx
		}).
		Assess("sign-in redirect carries the encoded request URI", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			resp := httpclient.New(t).GET(host, "/agents?x=1").ExpectRedirect(t, "/oauth2/start?rd=")
			if loc := resp.Header.Get("Location"); loc != "/oauth2/start?rd=%2Fagents%3Fx%3D1" {
				t.Fatalf("Location = %q, want /oauth2/start?rd=%%2Fagents%%3Fx%%3D1", loc)
			}
			return ctx
		}).
		Assess("the oauth2-proxy path is not authenticated", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			httpclient.New(t).GET(host, "/oauth2/start").ExpectStatus(t, http.StatusServiceUnavailable)
			return ctx
		}).
		Feature()

	testEnv.Test(t, feature)
}
