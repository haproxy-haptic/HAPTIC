// Copyright 2026 Philipp Hossner
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"cmp"
	"context"
	"net"
	"net/http"
	"strings"
	"testing"

	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/haproxy-haptic/HAPTIC/tests/e2e/httpclient"
)

const forgedForwardedFor = "203.0.113.7"

// TestForwardedHeaders asserts the X-Forwarded-* headers a backend receives
// from a client that forges them (#406): by default, and under each
// haproxy-haptic.org/forwardfor mode.
func TestForwardedHeaders(t *testing.T) {
	t.Parallel()
	hosts := map[string]string{}
	modes := []string{"", "add", "update", "ifmissing", "ignore"}
	feature := features.New("Ingress: the X-Forwarded-* headers a backend receives").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			t.Helper()
			client, err := cfg.NewClient()
			if err != nil {
				t.Fatalf("new client: %v", err)
			}
			ns := NamespaceForTest(ctx, t, client)
			DumpLogsOnFailure(t, ns)
			echo := NewEchoServerBackend(ctx, t, client, ns)
			for _, mode := range modes {
				name := forwardedHeadersRoute(mode)
				host := name + "." + ns + ".localdev.me"
				hosts[mode] = host
				spec := &IngressSpec{Name: name, Host: host, Path: "/", BackendService: echo.Service, BackendPort: echo.Port}
				if mode == "" {
					spec.TLSSecretName = NewTLSSecret(ctx, t, client, ns, name+"-tls", []string{host})
				} else {
					spec.Annotations = map[string]string{"haproxy-haptic.org/forwardfor": mode}
				}
				NewIngress(ctx, t, client, ns, spec)
			}
			for _, mode := range modes {
				waitForIngressDeployed(ctx, t, client, ns, forwardedHeadersRoute(mode))
			}
			return ctx
		}).
		Assess("by default a forged X-Forwarded-For, -Host and -Port are dropped and X-Forwarded-Proto is http",
			func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
				t.Helper()
				headers := forwardedHeaders(t, httpclient.New(t).GET(hosts[""], "/").
					WithHeader("X-Forwarded-For", forgedForwardedFor).
					WithHeader("X-Forwarded-Proto", "https").
					WithHeader("X-Forwarded-Host", "evil.example").
					WithHeader("X-Forwarded-Port", "4443"))
				requireClientAddress(t, headers["x-forwarded-for"])
				expectHeader(t, headers, "x-forwarded-proto", "http")
				expectHeader(t, headers, "x-forwarded-host", "")
				expectHeader(t, headers, "x-forwarded-port", "")
				return ctx
			}).
		Assess("TLS terminated by HAPTIC sets X-Forwarded-Proto https",
			func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
				t.Helper()
				headers := forwardedHeaders(t, httpclient.New(t).HTTPS(hosts[""], "/").
					WithHeader("X-Forwarded-For", forgedForwardedFor).
					WithHeader("X-Forwarded-Proto", "http"))
				requireClientAddress(t, headers["x-forwarded-for"])
				expectHeader(t, headers, "x-forwarded-proto", "https")
				return ctx
			}).
		Assess("forwardfor add keeps the client's X-Forwarded-For and appends the client address",
			func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
				t.Helper()
				got := forwardedHeaders(t, httpclient.New(t).GET(hosts["add"], "/").
					WithHeader("X-Forwarded-For", forgedForwardedFor))["x-forwarded-for"]
				forged, appended, found := strings.Cut(got, ", ")
				if !found || forged != forgedForwardedFor {
					t.Fatalf("X-Forwarded-For = %q, want %q followed by the client address", got, forgedForwardedFor)
				}
				requireClientAddress(t, appended)
				return ctx
			}).
		Assess("forwardfor update replaces the client's X-Forwarded-For",
			func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
				t.Helper()
				requireClientAddress(t, forwardedHeaders(t, httpclient.New(t).GET(hosts["update"], "/").
					WithHeader("X-Forwarded-For", forgedForwardedFor))["x-forwarded-for"])
				return ctx
			}).
		Assess("forwardfor ifmissing keeps the client's X-Forwarded-For and sets it when absent",
			func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
				t.Helper()
				expectHeader(t, forwardedHeaders(t, httpclient.New(t).GET(hosts["ifmissing"], "/").
					WithHeader("X-Forwarded-For", forgedForwardedFor)), "x-forwarded-for", forgedForwardedFor)
				requireClientAddress(t, forwardedHeaders(t, httpclient.New(t).GET(hosts["ifmissing"], "/"))["x-forwarded-for"])
				return ctx
			}).
		Assess("forwardfor ignore forwards the client's X-Forwarded-For unchanged",
			func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
				t.Helper()
				expectHeader(t, forwardedHeaders(t, httpclient.New(t).GET(hosts["ignore"], "/").
					WithHeader("X-Forwarded-For", forgedForwardedFor)), "x-forwarded-for", forgedForwardedFor)
				expectHeader(t, forwardedHeaders(t, httpclient.New(t).GET(hosts["ignore"], "/")), "x-forwarded-for", "")
				return ctx
			}).
		Feature()
	testEnv.Test(t, feature)
}

func forwardedHeadersRoute(mode string) string { return "xfwd-" + cmp.Or(mode, "default") }

// forwardedHeaders returns the request headers the echo backend received,
// once the route answers.
func forwardedHeaders(t *testing.T, req *httpclient.Request) map[string]string {
	t.Helper()
	resp := req.ExpectMatching(t, "echo backend answers", func(resp *httpclient.Response) bool {
		return resp.Status == http.StatusOK && resp.Echo != nil
	})
	return resp.Echo.Headers
}

// requireClientAddress fails unless value is exactly one IP address other
// than the forged one.
func requireClientAddress(t *testing.T, value string) {
	t.Helper()
	if net.ParseIP(value) == nil || value == forgedForwardedFor {
		t.Fatalf("X-Forwarded-For = %q, want only the client address", value)
	}
}

func expectHeader(t *testing.T, headers map[string]string, name, want string) {
	t.Helper()
	if got := headers[name]; got != want {
		t.Fatalf("%s = %q, want %q", name, got, want)
	}
}
