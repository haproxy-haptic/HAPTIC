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

// TestHapticSSLRedirect covers haproxy-haptic.org/redirect-scheme: https:
// plain HTTP gets the configured status and the same URL on https://, and the
// HTTPS request it points at is served instead of redirected again. The
// explicit default port must not break that loop guard.
func TestHapticSSLRedirect(t *testing.T) {
	t.Parallel()
	RunSimpleIngressTest(t, &SimpleIngressTest{
		Description:   "Ingress: haptic redirect-scheme https",
		Host:          "ingress-haptic-sslredirect.localdev.me",
		TLSSecretName: "tls",
		Annotations: map[string]string{
			"haproxy-haptic.org/redirect-scheme": "https",
			"haproxy-haptic.org/redirect-port":   "443",
			"haproxy-haptic.org/redirect-code":   "301",
		},
		Assess: []SimpleIngressAssertion{
			{
				Name: "HTTP request returns 301 to the same URL on https",
				Check: func(t *testing.T, host string) {
					t.Helper()
					resp := httpclient.New(t).GET(host, "/some/path?q=1").ExpectStatus(t, 301)
					if got, want := resp.Header.Get("Location"), "https://"+host+"/some/path?q=1"; got != want {
						t.Fatalf("expected Location %q, got %q", want, got)
					}
				},
			},
			{
				Name: "HTTPS request is served, not redirected",
				Check: func(t *testing.T, host string) {
					t.Helper()
					httpclient.New(t).HTTPS(host, "/some/path").ExpectOK(t)
				},
			},
		},
	})
}
