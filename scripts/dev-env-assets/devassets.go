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

// Package devassets exposes the dev-environment YAML manifests as embedded
// byte slices so they can be applied programmatically by Go test suites
// (notably tests/e2e) without relying on filesystem layout or external
// scripts.
//
// The same files are still consumed by scripts/start-dev-env.sh via path
// access, so this package is a non-disruptive addition: a single source of
// truth for both the developer's interactive dev loop and the automated
// e2e suite.
//
// What is intentionally not embedded:
//   - ingress-demo.yaml and gateway-demo.yaml — these are demo manifests
//     for the dev loop. The e2e suite uses per-test isolation, building
//     its own Ingress/HTTPRoute resources via Go fixture builders.
//   - haproxy-production.yaml — production values, not a dev-env fixture.
//   - generate-webhook-certs.sh — shell script, not applied to clusters.
package devassets

import _ "embed"

// AuthServerYAML deploys the auth-server fixture used by the SPOA hub
// external-auth plugin.
//
//go:embed auth-server.yaml
var AuthServerYAML []byte

// EchoServerYAML deploys the echo-server fixture, the standard upstream
// for routing tests. Mirrors the inline heredoc in
// scripts/start-dev-env.sh; the script could switch to applying this
// file directly later for a single source of truth.
//
//go:embed echo-server.yaml
var EchoServerYAML []byte

// BlocklistServerYAML deploys the blocklist-server fixture used by the
// HTTP store demo (templates fetch a blocklist over HTTP and gate
// requests by header value).
//
//go:embed blocklist-server.yaml
var BlocklistServerYAML []byte

// HAProxyDemoBackendYAML deploys haproxy-demo-backend, the upstream used
// by Ingress demo manifests for slot-allocation and scaling tests.
//
//go:embed haproxy-demo-backend.yaml
var HAProxyDemoBackendYAML []byte

// HAProxyTestBackendYAML deploys haproxy-test-backend, an additional
// upstream used by selected route tests.
//
//go:embed haproxy-test-backend.yaml
var HAProxyTestBackendYAML []byte

// E2EValuesYAML is the helm values file consumed by the automated
// e2e suite (tests/e2e) as the default profile. Distinct from
// DevValuesYAML because the parallel-test workload (30+ tests
// each creating / deleting Ingresses) creates sustained HAProxy
// worker-fork churn at the dev-values 10ms preset, racing tests'
// polling against reload windows. E2EValuesYAML uses
// 100/100/200ms instead — slow enough for worker-fork transitions
// to settle, fast enough that ingress-create -> serving latency
// stays well under 1s. Otherwise identical to dev-values.yaml.
//
//go:embed e2e-values.yaml
var E2EValuesYAML []byte

// MetricsServerYAML is the upstream metrics-server install manifest
// (v0.7.2) patched with --kubelet-insecure-tls (kind's kubelet serves a
// self-signed cert) and a 10s scrape resolution. The e2e TestMain applies
// it best-effort so `kubectl top pods/nodes` works for the rolling-restart
// failure snapshot's utilization capture. Not used in production.
//
//go:embed metrics-server.yaml
var MetricsServerYAML []byte

// ConformanceValuesYAML is the helm values file used by the upstream
// Gateway API / Ingress conformance suites. Stripped-down version of
// dev-values.yaml: no HTTP-store demo (the blocklist URL was burning
// 7s per render against an unreachable service in conformance), no
// coraza WAF override (e2e-specific). Keeps the 10/10/50ms timing
// preset because the conformance suite's 5s-poll-vs-all-disabled-
// slots race requires it (see comment block in this file). The e2e
// suite selects via the HAPTIC_E2E_PROFILE env var:
//
//	HAPTIC_E2E_PROFILE=conformance → ConformanceValuesYAML
//	(anything else / unset)        → E2EValuesYAML
//
// Same file shape so the e2e helmInstallChart code path is
// indifferent to which profile is selected — only the bytes change.
//
//go:embed conformance-values.yaml
var ConformanceValuesYAML []byte

// JWKSServerYAML serves rotating public keys over HTTPS for authentication tests.
//
//go:embed jwks-server.yaml
var JWKSServerYAML []byte
