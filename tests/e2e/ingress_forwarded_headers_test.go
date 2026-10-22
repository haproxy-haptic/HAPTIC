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
	"github.com/stretchr/testify/require"
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
		Assess("alternative forwarding headers are removed for every route mode and HTTP protocol",
			func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
				t.Helper()
				for _, protocol := range []string{"http", "h2c", "https"} {
					headers := operatorForwardedHeaders(ctx, t, hosts[""], protocol, true)
					expectAlternativeForwardedHeaders(t, headers, "")
				}
				for _, mode := range modes[1:] {
					headers := operatorForwardedHeaders(ctx, t, hosts[mode], "http", true)
					expectAlternativeForwardedHeaders(t, headers, "")
				}
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

func forgedAlternativeForwardedHeaders() map[string]string {
	return map[string]string{
		"Forwarded":          "for=203.0.113.2;host=evil.example;proto=http",
		"X-Forwarded-Prefix": "/evil",
		"X-Forwarded-Server": "evil.example",
		"X-Real-IP":          "203.0.113.3",
		"X-Client-IP":        "203.0.113.4",
		"Client-IP":          "203.0.113.5",
	}
}

type forwardedSourceRoute struct {
	host   string
	header string
}

func TestForwardedHeadersSourceIP(t *testing.T) {
	// Changes and restores the global trust policy before parallel tests run.
	feature := features.New("src-ip-header uses sanitized request headers").
		Assess("untrusted inputs are removed while trusted inputs and operator decisions survive",
			func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
				t.Helper()
				client, err := cfg.NewClient()
				require.NoError(t, err)
				ns := NamespaceForTest(ctx, t, client)
				DumpLogsOnFailure(t, ns)
				echo := NewEchoServerBackend(ctx, t, client, ns)
				routes := make([]forwardedSourceRoute, 0, 6)
				routeNames := make([]string, 0, 12)
				for _, family := range []string{"haproxy-haptic.org", "haproxy.org"} {
					for _, header := range []string{"X-Real-IP", "x-client-ip", "Client-IP"} {
						name := strings.ReplaceAll(family, ".", "-") + "-" + strings.ToLower(header)
						host := "xfwd-source." + name + "." + ns + ".localdev.me"
						NewIngress(ctx, t, client, ns, &IngressSpec{Name: name, Host: host, Path: "/selected",
							BackendService: echo.Service, BackendPort: echo.Port,
							Annotations: map[string]string{family + "/src-ip-header": header}})
						NewIngress(ctx, t, client, ns, &IngressSpec{Name: name + "-control", Host: host, Path: "/",
							BackendService: echo.Service, BackendPort: echo.Port})
						routes = append(routes, forwardedSourceRoute{host: host, header: header})
						routeNames = append(routeNames, name, name+"-control")
					}
				}
				for _, name := range routeNames {
					waitForIngressDeployed(ctx, t, client, ns, name)
				}
				assessForwardedSourceHeaders(t, routes, false)
				trustForwardedHeadersDuringTest(ctx, t, cfg, routes[0].host, ns, routeNames, forgedForwardedFor)
				assessForwardedSourceHeaders(t, routes, true)
				return ctx
			}).Feature()
	testEnv.Test(t, feature)
}

func expectAlternativeForwardedHeaders(t *testing.T, headers map[string]string, want string) {
	t.Helper()
	for header := range forgedAlternativeForwardedHeaders() {
		expectHeader(t, headers, strings.ToLower(header), want)
	}
}

func assessForwardedSourceHeaders(t *testing.T, routes []forwardedSourceRoute, trusted bool) {
	t.Helper()
	for _, route := range routes {
		for _, path := range []string{"/selected", "/"} {
			for _, action := range []string{"", "set", "delete"} {
				assessForwardedSourceHeader(t, route, path, action, trusted)
			}
		}
	}
}

func assessForwardedSourceHeader(t *testing.T, route forwardedSourceRoute, path, action string, trusted bool) {
	t.Helper()
	req := httpclient.New(t).GET(route.host, path).WithHeader("X-Forwarded-For", forgedForwardedFor)
	for name := range forgedAlternativeForwardedHeaders() {
		req = req.WithHeader(name, "203.0.113.9")
	}
	if action != "" {
		req = req.WithHeader("X-Test-"+action+"-Source", route.header)
	}
	headers := forwardedHeaders(t, req)
	for name := range forgedAlternativeForwardedHeaders() {
		want := ""
		if trusted {
			want = "203.0.113.9"
		}
		if strings.EqualFold(name, route.header) {
			switch action {
			case "set":
				want = "198.51.100.8"
			case "delete":
				want = ""
			}
		}
		expectHeader(t, headers, strings.ToLower(name), want)
	}
	address := headers["x-forwarded-for"]
	if trusted {
		first, last, found := strings.Cut(address, ", ")
		require.True(t, found)
		require.Equal(t, forgedForwardedFor, first)
		address = last
	}
	switch {
	case path == "/selected" && action == "set":
		require.Equal(t, "198.51.100.8", address)
	case path == "/selected" && action == "" && trusted:
		require.Equal(t, "203.0.113.9", address)
	default:
		requireClientAddress(t, address)
		require.NotContains(t, []string{"203.0.113.9", "198.51.100.8"}, address)
	}
}
