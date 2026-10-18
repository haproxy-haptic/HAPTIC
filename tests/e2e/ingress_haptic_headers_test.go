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
	"net"
	"testing"

	"github.com/haproxy-haptic/HAPTIC/tests/e2e/httpclient"
)

// TestHapticHeaders adapts the haproxy.org/* response-header e2e test (plus the
// forwardfor / set-host smoke rows from the annotations table) to the native
// haproxy-haptic.org/* prefix, proving header manipulation to and from the
// upstream takes effect end-to-end.
//
// Keys exercised (all host-scoped, applied by charts/haptic/charts/
// haptic-annotations/30-frontend-filters.yaml and 26-rewrite-affinity.yaml):
//   - haproxy-haptic.org/response-set-header  → http-after-response set-header
//   - haproxy-haptic.org/request-set-header   → http-request set-header
//   - haproxy-haptic.org/forwardfor           → X-Forwarded-For injection
//   - haproxy-haptic.org/set-host             → upstream Host override (reqhdr map)
//   - haproxy-haptic.org/x-forwarded-prefix   → X-Forwarded-Prefix (reqhdr map)
//
// request/response-set-header take newline-delimited "<name> <value>" lines
// (one header per line), matching the haptic fragment's split-on-"\n" parser.
func TestHapticHeaders(t *testing.T) {
	t.Parallel()
	RunSimpleIngressTest(t, &SimpleIngressTest{
		Description: "Ingress: haproxy-haptic.org header manipulation",
		Host:        "ingress-haptic-headers.localdev.me",
		Annotations: map[string]string{
			// Response headers injected on the way back to the client.
			"haproxy-haptic.org/response-set-header": "X-Custom-Response custom-resp-value\nX-Frame-Options DENY",
			// Request headers injected on the way to the upstream.
			"haproxy-haptic.org/request-set-header": "X-Custom-Request custom-req-value\nX-Request-ID req-12345",
			// Overwrite X-Forwarded-For with the connection source.
			"haproxy-haptic.org/forwardfor": "update",
			// Override the Host header the upstream receives.
			"haproxy-haptic.org/set-host": "custom-upstream.example.com",
			// Add an X-Forwarded-Prefix request header for the upstream.
			"haproxy-haptic.org/x-forwarded-prefix": "/haptic-prefix",
		},
		Assess: []SimpleIngressAssertion{
			{
				Name: "response-set-header injects X-Custom-Response",
				Check: func(t *testing.T, host string) {
					t.Helper()
					httpclient.New(t).GET(host, "/").ExpectHeader(t, "X-Custom-Response", "custom-resp-value")
				},
			},
			{
				Name: "response-set-header injects X-Frame-Options DENY",
				Check: func(t *testing.T, host string) {
					t.Helper()
					httpclient.New(t).GET(host, "/").ExpectHeader(t, "X-Frame-Options", "DENY")
				},
			},
			{
				Name: "request-set-header reaches upstream as X-Custom-Request",
				Check: func(t *testing.T, host string) {
					t.Helper()
					httpclient.New(t).GET(host, "/").ExpectEchoHeader(t, "X-Custom-Request", "custom-req-value")
				},
			},
			{
				Name: "request-set-header reaches upstream as X-Request-ID",
				Check: func(t *testing.T, host string) {
					t.Helper()
					httpclient.New(t).GET(host, "/").ExpectEchoHeader(t, "X-Request-ID", "req-12345")
				},
			},
			{
				Name: "set-host overrides upstream Host header",
				Check: func(t *testing.T, host string) {
					t.Helper()
					httpclient.New(t).GET(host, "/").ExpectEchoHeader(t, "Host", "custom-upstream.example.com")
				},
			},
			{
				Name: "x-forwarded-prefix reaches upstream",
				Check: func(t *testing.T, host string) {
					t.Helper()
					httpclient.New(t).GET(host, "/").ExpectEchoHeader(t, "X-Forwarded-Prefix", "/haptic-prefix")
				},
			},
			{
				Name: "forwardfor sets X-Forwarded-For on the upstream request",
				Check: func(t *testing.T, host string) {
					t.Helper()
					// forwardfor=update rewrites X-Forwarded-For to %[src]; the
					// exact source IP isn't predictable, so assert the upstream
					// saw exactly one address.
					httpclient.New(t).GET(host, "/").ExpectMatching(t,
						"upstream received only the client address in X-Forwarded-For",
						func(resp *httpclient.Response) bool {
							return resp.Echo != nil && net.ParseIP(resp.Echo.Headers["x-forwarded-for"]) != nil
						})
				},
			},
		},
	})
}
