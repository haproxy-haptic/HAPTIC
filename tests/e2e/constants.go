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
	"os"
	"time"

	"github.com/haproxy-haptic/HAPTIC/tests/kindutil"
)

// Cluster, deployment, and image constants. The e2e suite is self-contained:
// it creates its own kind cluster and helm-installs the chart. None of these
// constants depend on scripts/start-dev-env.sh having been run.
const (
	defaultCertificateMTLSHost = "ingress-haptic-default-clientmtls.localdev.me"

	// HelmReleaseName is the helm release name used to install the chart.
	HelmReleaseName = "haptic"

	// ControllerNamespace is the namespace the helm release deploys into.
	// Holds the controller, the SPOA hub, and the HAProxy pods.
	ControllerNamespace = "haptic"

	// ControllerDeploymentName follows the helm chart's naming convention:
	// "<release>-controller".
	ControllerDeploymentName = "haptic-controller"

	// HAProxyDeploymentName is the HAProxy Deployment name (SPOA-enabled).
	HAProxyDeploymentName = "haptic-haproxy"

	// HAProxyConfigName is the HAProxyTemplateConfig CRD instance name.
	HAProxyConfigName = "haptic-config"

	// SharedFixturesNamespace holds the stateless backend fixtures
	// (echo-server, auth-server, blocklist-server) that all e2e tests
	// share. Deployed once during TestMain.
	SharedFixturesNamespace = "echo"

	// DefaultHAProxyServiceHTTPPort is the ordinary in-cluster HAProxy
	// Service port. The cache profile deliberately overrides it so its
	// NetworkPolicy e2e covers Service-port to container-port translation.
	DefaultHAProxyServiceHTTPPort = 80
	CacheHAProxyServiceHTTPPort   = 18080
	HTTPPortName                  = "http"

	// HTTPHostPort is the host-side TCP port the kind cluster exposes for
	// HAProxy HTTP traffic. Distinct from the dev cluster's 30080 so the
	// two clusters can coexist on a developer's machine. The chart still
	// configures NodePort 30080 *inside* the cluster; kind's
	// extraPortMappings (in the e2e kind config below) translate
	// containerPort 30080 → hostPort 31080.
	HTTPHostPort = 31080

	// HTTPSHostPort is the host-side TCP port for HAProxy HTTPS traffic.
	// Same coexistence reason as HTTPHostPort.
	HTTPSHostPort = 31443

	// StatsHostPort is the host-side TCP port for HAProxy stats.
	StatsHostPort = 31404

	// DebugPort is the controller debug HTTP server port (matches helm
	// chart's controller.ports.healthz default).
	DebugPort = 8080

	// ControllerMetricsPort is the controller's Prometheus /metrics port
	// (matches the chart's controller.ports.metrics default). The scale
	// tier scrapes it per-pod via the apiserver pod proxy.
	ControllerMetricsPort = 9090

	// VectorMetricsPort is the Prometheus endpoint the vector sidecar serves
	// on every HAProxy pod (matches the chart's vector.metricsPort default):
	// vector's own series, the log-derived and request metrics, and the hub's
	// re-exported ones. haproxy_* is NOT here — Prometheus scrapes HAProxy's
	// exporter directly on HAProxyStatsPort, and so does the suite.
	VectorMetricsPort = 9598

	// HAProxyStatsPort is HAProxy's status frontend (matches the chart's
	// haproxy.ports.stats default): /healthz, /ready and the Prometheus
	// exporter on /metrics, answering on the pod IP.
	HAProxyStatsPort = 8404

	// DebugServiceName is the Service the chart provisions for the
	// controller's debug + metrics endpoints. Named after the helm release
	// (single multi-port Service rather than separate -debug/-metrics
	// services).
	DebugServiceNameValue = HelmReleaseName
)

var runtimeImages kindutil.ChartImages

// ChartHAProxyVersion is resolved from the chart before suite setup.
var ChartHAProxyVersion string

// ControllerImageName is the docker image the chart installs:
// haptic:<ControllerImageTag>-haproxy<version>, which the e2e Makefile target
// tags from haptic:<ControllerImageTag>.
var ControllerImageName string

// ControllerImageTag is the chart's controller.image.tag, "test" unless
// HAPTIC_E2E_IMAGE_TAG names a run's own image.
var ControllerImageTag string

// ChartHAProxyServiceHTTPPort is the HAProxy Service port installed by the
// active profile. The cache profile intentionally differs from the pod's port
// 80 so a passing cache test proves the NetworkPolicy permits the post-Service
// destination seen by the policy engine.
var ChartHAProxyServiceHTTPPort = func() int {
	if os.Getenv("HAPTIC_E2E_PROFILE") == "cache" {
		return CacheHAProxyServiceHTTPPort
	}
	return DefaultHAProxyServiceHTTPPort
}()

// VarnishPolicyProbeImage runs the same-namespace NetworkPolicy denial probe.
// Pin the multi-architecture manifest so a fresh cache shard loads exactly the
// image its pod requests instead of depending on a mutable tag or a cold pull.
const VarnishPolicyProbeImage = "alpine/curl@sha256:71597a4f6ac6c7515c77084d2a216aa2f302cd6f9ec311d2f55eb9320f161ce2"

// Debug endpoint paths, mirrored from tests/acceptance/constants.go.
const (
	DebugPathConfig    = "/debug/vars/config"
	DebugPathPipeline  = "/debug/vars/pipeline"
	DebugPathValidated = "/debug/vars/validated"
	DebugPathErrors    = "/debug/vars/errors"
	DebugPathEvents    = "/debug/vars/events"
	DebugPathAuxFiles  = "/debug/vars/auxfiles"
	HealthzPath        = "/healthz"
)

// Pod label selectors for the chart's standard kubernetes.io recommended
// labels. The chart sets app.kubernetes.io/instance=<release> and
// app.kubernetes.io/component=<role>.
const (
	// LabelSelectorController matches the controller pods.
	LabelSelectorController = "app.kubernetes.io/instance=" + HelmReleaseName +
		",app.kubernetes.io/component=controller"

	// LabelSelectorHAProxy matches the HAProxy pods.
	LabelSelectorHAProxy = "app.kubernetes.io/instance=" + HelmReleaseName +
		",app.kubernetes.io/component=loadbalancer"

	// rateLimitStoreName is the StatefulSet backing the shared rate-limit tier.
	rateLimitStoreName = HelmReleaseName + "-rl-store"

	// labelSelectorRateLimitStore matches the store's Valkey + Sentinel pods.
	// Lives here rather than beside the rate-limit test so the failure dump can
	// reach it — a _test.go constant is invisible to cleanup.go.
	labelSelectorRateLimitStore = "app.kubernetes.io/name=haptic-rate-limit-store," +
		"app.kubernetes.io/instance=" + rateLimitStoreName
)

// Default timeouts used across the suite. Anything that needs a different
// budget passes its own to testutil.WaitConfig.
const (
	// DefaultClusterCreateTimeout caps the kind cluster bring-up wait.
	DefaultClusterCreateTimeout = 5 * time.Minute

	// DefaultHelmInstallTimeout caps `helm install --wait`.
	DefaultHelmInstallTimeout = 5 * time.Minute

	// DefaultEnvironmentReadyTimeout is the budget for the suite-level
	// readiness check (controller pipeline succeeded, all HAProxy pods
	// acknowledged the deployed config). 6 minutes, not 3: controller
	// STARTUP legitimately runs the config's embedded validationTests
	// (dozens of haproxy -c semantic checks, parallelized only up to
	// NumCPU) before turning Ready — measured at 2-4 minutes on 2-vCPU
	// shared CI runners, where 2 of 5 otherwise-green e2e jobs timed out
	// at 3m purely on startup. This budget gates one-time initialization
	// only; post-startup convergence stays under the 15s httpclient poll
	// ceiling and the conformance suite's 10s MaxTimeToConsistency.
	DefaultEnvironmentReadyTimeout = 6 * time.Minute

	// DefaultPerTestSetupTimeout is the budget for per-test fixture
	// application + reaching steady-state for the new manifests. This bounds
	// Kubernetes backend-pod readiness (scheduling + container start +
	// readiness probe), NOT haptic's reaction, so it is deliberately NOT
	// subject to the 15s convergence cap — pod scheduling under parallel
	// load can legitimately exceed 15s.
	DefaultPerTestSetupTimeout = 90 * time.Second
)
