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
	"strings"
	"testing"

	"github.com/haproxy-haptic/HAPTIC/tests/e2e/httpclient"
)

// TestHapticRequireSniHostMatch covers haproxy-haptic.org/require-sni-host-match
// (#567): a request for the annotated host over a TLS connection opened with
// another SNI is answered 421, while a matching SNI is served.
func TestHapticRequireSniHostMatch(t *testing.T) {
	t.Parallel()
	RunSimpleIngressTest(t, &SimpleIngressTest{
		Description:   "Ingress: require-sni-host-match via haproxy-haptic.org",
		Host:          "ingress-haptic-sni-host.localdev.me",
		TLSSecretName: "haptic-sni-host-tls",
		Annotations: map[string]string{
			"haproxy-haptic.org/require-sni-host-match": "true",
		},
		Assess: []SimpleIngressAssertion{
			{
				Name: "matching SNI and Host are served, Host compared case-insensitively",
				Check: func(t *testing.T, host string) {
					t.Helper()
					client := httpclient.New(t)
					client.HTTPS(host, "/").ExpectOK(t)
					client.HTTPS(host, "/").WithHostHeader(strings.ToUpper(host)).ExpectOK(t)
				},
			},
			{
				Name: "the annotated host reached over another SNI is answered 421",
				Check: func(t *testing.T, host string) {
					t.Helper()
					httpclient.New(t).HTTPS("other-"+host, "/").WithHostHeader(host).ExpectStatus(t, 421)
				},
			},
		},
	})
}

// TestHapticSniHostMatchOffByDefault pins that a host without the annotation
// keeps serving a request whose SNI differs, as a re-encrypting proxy sends.
func TestHapticSniHostMatchOffByDefault(t *testing.T) {
	t.Parallel()
	RunSimpleIngressTest(t, &SimpleIngressTest{
		Description:   "Ingress: Host/SNI mismatch is served without require-sni-host-match",
		Host:          "ingress-haptic-sni-host-off.localdev.me",
		TLSSecretName: "haptic-sni-host-off-tls",
		Assess: []SimpleIngressAssertion{{
			Name: "the host reached over another SNI is served",
			Check: func(t *testing.T, host string) {
				t.Helper()
				httpclient.New(t).HTTPS("other-"+host, "/").WithHostHeader(host).ExpectOK(t)
			},
		}},
	})
}
