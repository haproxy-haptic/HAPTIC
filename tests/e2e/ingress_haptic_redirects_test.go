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
	"testing"

	"github.com/haproxy-haptic/HAPTIC/tests/e2e/httpclient"
)

// TestHapticRedirects covers the haptic-native redirect annotations under the
// haproxy-haptic.org/ prefix:
//
//   - haproxy-haptic.org/redirect-* (+ -code): a request routed to the Ingress
//     is redirected to the target built from the annotations, with every part
//     they leave unset taken from the request (route-redirect.map), including
//     redirect-path-prefix, which swaps the matched Ingress path prefix.
//   - haproxy-haptic.org/root-redirect: a request to "/" is redirected to the
//     configured sub-path (path="/"-gated redirect served from a reload-free
//     map); other paths reach the backend.
//
// A catch-all redirect would shadow the root-redirect "/"-gated redirect, so
// the behaviours use distinct hosts under the shared "ingress-haptic-redirects"
// theme rather than one Ingress.
func TestHapticRedirects(t *testing.T) {
	t.Parallel()

	// Host alias: only the host changes, so the path and query survive.
	RunSimpleIngressTest(t, &SimpleIngressTest{
		Description: "Ingress: haproxy-haptic.org/redirect-host",
		Host:        "ingress-haptic-redirects-alias.localdev.me",
		Annotations: map[string]string{
			"haproxy-haptic.org/redirect-host": "new.example.com",
			"haproxy-haptic.org/redirect-code": "308",
		},
		Assess: []SimpleIngressAssertion{{
			Name: "keeps the path and query",
			Check: func(t *testing.T, host string) {
				t.Helper()
				resp := httpclient.New(t).GET(host, "/login/start?state=abc").ExpectStatus(t, 308)
				if got, want := resp.Header.Get("Location"), "http://new.example.com/login/start?state=abc"; got != want {
					t.Fatalf("expected Location %q, got %q", want, got)
				}
			},
		}},
	})

	// Fixed URL: every part is set and the query is dropped.
	RunSimpleIngressTest(t, &SimpleIngressTest{
		Description: "Ingress: haproxy-haptic.org/redirect-* fixed target",
		Host:        "ingress-haptic-redirects-fixed.localdev.me",
		Annotations: map[string]string{
			"haproxy-haptic.org/redirect-scheme": "https",
			"haproxy-haptic.org/redirect-host":   "example.com",
			"haproxy-haptic.org/redirect-path":   "/relocated",
			"haproxy-haptic.org/redirect-query":  "drop",
			"haproxy-haptic.org/redirect-code":   "301",
		},
		Assess: []SimpleIngressAssertion{{
			Name: "any request returns 301 to the configured URL",
			Check: func(t *testing.T, host string) {
				t.Helper()
				resp := httpclient.New(t).GET(host, "/some/path?q=1").ExpectStatus(t, 301)
				if got, want := resp.Header.Get("Location"), "https://example.com/relocated"; got != want {
					t.Fatalf("expected Location %q, got %q", want, got)
				}
			},
		}},
	})

	// Prefix replacement: the matched Ingress path prefix is swapped and the
	// rest of the path and the query are kept.
	RunSimpleIngressTest(t, &SimpleIngressTest{
		Description: "Ingress: haproxy-haptic.org/redirect-path-prefix",
		Host:        "ingress-haptic-redirects-prefix.localdev.me",
		Path:        "/old",
		Annotations: map[string]string{
			"haproxy-haptic.org/redirect-path-prefix": "/new",
		},
		Assess: []SimpleIngressAssertion{{
			Name: "replaces the matched prefix and keeps the rest",
			Check: func(t *testing.T, host string) {
				t.Helper()
				resp := httpclient.New(t).GET(host, "/old/a/b?q=1").ExpectStatus(t, 302)
				if got, want := resp.Header.Get("Location"), "http://"+host+"/new/a/b?q=1"; got != want {
					t.Fatalf("expected Location %q, got %q", want, got)
				}
			},
		}},
	})

	// Root redirect: a request to "/" is redirected to the configured sub-path;
	// any other path (here the root-redirect target itself) reaches the backend.
	RunSimpleIngressTest(t, &SimpleIngressTest{
		Description: "Ingress: haproxy-haptic.org/root-redirect",
		Host:        "ingress-haptic-redirects-rootredirect.localdev.me",
		Annotations: map[string]string{
			"haproxy-haptic.org/root-redirect": "/welcome",
		},
		Assess: []SimpleIngressAssertion{
			{
				Name: "GET / redirects to /welcome",
				Check: func(t *testing.T, host string) {
					t.Helper()
					httpclient.New(t).GET(host, "/").ExpectRedirect(t, "/welcome")
				},
			},
			{
				Name: "GET /welcome reaches the backend (no redirect loop)",
				Check: func(t *testing.T, host string) {
					t.Helper()
					httpclient.New(t).GET(host, "/welcome").ExpectOK(t)
				},
			},
		},
	})
}
