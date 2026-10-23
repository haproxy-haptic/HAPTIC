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

//go:build gateway_conformance

// Package conformance runs the upstream Kubernetes Gateway API conformance
// suite against the chart's GatewayClass. Builds under the
// `gateway_conformance` tag so it stays out of regular test runs (the
// suite has its own slow setup and pulls in the upstream conformance
// fixtures).
//
// Execution model: the test binary runs as a sibling container on the
// kind docker network (see `make test-conformance` / Dockerfile.
// conformance-test). Inside that container, MetalLB-allocated LoadBalancer
// IPs from Gateway.Status.Addresses are directly routable — the test
// dials them with the stock upstream RoundTripper + gRPC client, no
// NodePort tunneling and no DinD-aware dialer required. Same code path
// in CI (sibling container under the DinD daemon) and on a developer
// laptop (sibling container under the host docker daemon).
//
// To run locally:
//
//	make test-e2e            # brings up the haptic-e2e kind cluster
//	make test-conformance    # builds the test image, runs it as a sibling container
//
// The suite expects an existing `haptic-e2e` kind cluster with the chart
// deployed and the `haptic` GatewayClass accepted. `make test-e2e`
// (default `KEEP_CLUSTER=true`) leaves that cluster in place so the
// conformance container can attach to its kube apiserver via the kind
// docker-DNS hostname (e.g. `https://haptic-e2e-control-plane:6443`).
//
// SupportedFeatures pin the chart's actual coverage. Features
// intentionally excluded map to HTTPRoute filter shapes the chart
// currently doesn't implement (h2c, request mirror, redirect filters
// on HTTPRoute — the chart's redirect-via-annotation is Ingress-side,
// not HTTPRoute). Add features as the chart grows.
//
// The test fails on any conformance assertion regression for the
// declared SupportedFeatures set. Genuine open issues against a specific
// upstream test should be added to SkipTests with an issue link, NOT
// hidden behind a t.Skip() blanket-skip — see
// `feedback_skipped_tests_are_shipped_bugs.md`.
package conformance

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/dynamic"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	controllerruntimelog "sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/gateway-api/apis/v1alpha2"
	"sigs.k8s.io/gateway-api/apis/v1alpha3"
	"sigs.k8s.io/gateway-api/apis/v1beta1"
	xv1alpha1 "sigs.k8s.io/gateway-api/apisx/v1alpha1"
	gwconformance "sigs.k8s.io/gateway-api/conformance"
	confv1 "sigs.k8s.io/gateway-api/conformance/apis/v1"
	conformancetests "sigs.k8s.io/gateway-api/conformance/tests"
	conformanceconfig "sigs.k8s.io/gateway-api/conformance/utils/config"
	"sigs.k8s.io/gateway-api/conformance/utils/roundtripper"
	"sigs.k8s.io/gateway-api/conformance/utils/suite"
	"sigs.k8s.io/gateway-api/pkg/features"
)

// metalLBPoolGVR identifies the IPAddressPool CRD MetalLB ships. The e2e
// suite (tests/e2e/metallb.go) creates a pool named "e2e-pool" in the
// metallb-system namespace covering the upper sliver of the kind Docker
// network. We discover that pool here so the SupportGatewayStaticAddresses
// tests get realistic Usable / Unusable addresses without hardcoding IPs
// (kind's network can shift host-to-host).
var metalLBPoolGVR = schema.GroupVersionResource{
	Group:    "metallb.io",
	Version:  "v1beta1",
	Resource: "ipaddresspools",
}

func servesExperimentalChannel(ctx context.Context, c client.Client) (bool, error) {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := c.Get(ctx, client.ObjectKey{Name: "httproutes.gateway.networking.k8s.io"}, crd); err != nil {
		return false, fmt.Errorf("read the HTTPRoute CRD's channel: %w", err)
	}
	return crd.Annotations["gateway.networking.k8s.io/channel"] == "experimental", nil
}

// gatewayClassName is the GatewayClass the chart provisions. The chart's
// values default `gatewayClass.name` to "haptic" — keep this in sync if
// that ever changes.
const gatewayClassName = "haptic"

func TestGatewayAPIConformance(t *testing.T) {
	if os.Getenv("CONFORMANCE_REPORT_OUTPUT") != "" {
		require.NoError(t, validateReportInvocation(flag.Lookup("test.run").Value.String(),
			flag.Lookup("test.skip").Value.String(), os.Getenv("CONFORMANCE_IMPL_VERSION"), testing.Short()))
	}
	controllerruntimelog.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))

	// KUBECONFIG must be provided by the caller. When run as a sibling
	// container via `make test-conformance`, the kubeconfig is mounted
	// at /etc/kubeconfig and the env var is set on the docker run command.
	require.NotEmpty(t, os.Getenv("KUBECONFIG"),
		"KUBECONFIG must point at the haptic-e2e cluster's kubeconfig")

	cfg, err := config.GetConfig()
	require.NoError(t, err, "load Kubernetes config")

	// client-go defaults to QPS=5/Burst=10 — a SHARED limiter across the
	// suite's dozens of parallel tests. The upstream status helpers poll at
	// 100ms intervals, so 15-30 concurrently waiting subtests demand a
	// sustained 150-300 QPS; anything lower queues requests in the limiter
	// until plain GETs die with "client rate limiter Wait returned an
	// error: context deadline exceeded" (observed as a rotating 1-5 test
	// failure set across otherwise-identical runs — at QPS=100 roughly
	// every second run still failed). The suite rebuilds its own clients
	// from this RestConfig (suite.go), so tuning it here covers them all.
	// Server-side API Priority & Fairness still protects the apiserver.
	cfg.QPS = 500
	cfg.Burst = 1000

	clientOpts := client.Options{}
	c, err := client.New(cfg, clientOpts)
	require.NoError(t, err, "create controller-runtime client")

	cs, err := clientset.NewForConfig(cfg)
	require.NoError(t, err, "create kubernetes clientset")

	installConformanceSchemes(t, c.Scheme())

	supported := supportedConformanceFeatures(t, c)

	// Upstream defaults are sized for "any compliant implementation,
	// even a slow one" — RequestTimeout=10s with 3 consecutive successes
	// and MaxTimeToConsistency=30s adds up to 30+s per failing sub-test,
	// and GatewayMustHaveAddress=180s lets a doomed test linger for 3
	// minutes before giving up. On our chart Gateways are Programmed in
	// ~1-2s and HTTP requests complete in <1s; the only paths that
	// approach the upstream limits are the failure paths. Tighten so
	// fail-mode shards complete in single-digit minutes instead of
	// 12+ minutes (each TLS / HTTPS sub-test that can't reach a backend
	// burns RequestTimeout × RequiredConsecutiveSuccesses worth of
	// budget before the test framework concludes failure).
	//
	// We tighten only the per-request budget (RequestTimeout below) and
	// stay at upstream defaults for Kubernetes-object timeouts (GetTimeout
	// etc. — reads against the apiserver, not the chart's data plane) and
	// for MaxTimeToConsistency (see below).
	timeoutCfg := conformanceconfig.DefaultTimeoutConfig()
	// MaxTimeToConsistency stays at the upstream default (30s), and NOT
	// at haptic's own 10s convergence ceiling, because this budget does
	// not measure haptic alone. For a test-created Gateway it must
	// absorb, in sequence: (1) kube-proxy programming the fresh
	// per-Gateway LoadBalancer Service's DNAT rules — until then the
	// node answers SYNs to the VIP with ICMP host-unreachable even
	// though MetalLB announced it ("connect: no route to host";
	// mechanism verified by freezing kube-proxy and dialing a fresh VIP
	// from a sibling container), and (2) haptic's reconcile → render →
	// deploy → reload. On CI runners under the suite's service churn,
	// (1) alone was measured eating ~4-10s (MetalLB speaker log shows
	// the VIP cycling announce/withdraw as parallel tests churn
	// Gateways; sibling subtests on the same VIP passed right at the
	// 10s boundary while one missed it). A 10s combined budget
	// therefore fails on cluster-infra latency haptic cannot influence.
	// Haptic's own convergence contract is enforced where it can be
	// isolated: RequestTimeout below stays 10s per request, and the e2e
	// suite asserts endpoint-propagation latency directly.
	timeoutCfg.RequestTimeout = 10 * time.Second
	// The status-wait budgets (GatewayMustHaveCondition,
	// LatestObservedGenerationSet, DefaultTestTimeout, …) deliberately stay
	// at upstream defaults. Earlier revisions tightened them to 20-30s and
	// that produced a ROTATING set of 1-7 spurious failures across
	// otherwise-identical runs: the same subtest context covers helper
	// preambles (NamespacesMustBeReady, initial GETs through the shared
	// client) which under the suite's 16-way parallel churn can consume a
	// tightened budget before the actual status wait even starts — the
	// controller itself updates these statuses in ~1-2s when probed in
	// isolation. The data-plane contract stays enforced by the 10s
	// MaxTimeToConsistency/RequestTimeout above; the object-status budgets
	// are apiserver/test-infra bound and tightening them buys nothing but
	// flakes (this file said so in the comment above all along).
	debug := os.Getenv("CONFORMANCE_DEBUG") != ""

	// Sibling-container execution model: this binary runs on the kind
	// docker network, so Gateway.Status addresses (MetalLB LB IPs in
	// kind's network) are directly routable. The stock upstream
	// RoundTripper + gRPC client dial them verbatim — same code path as
	// any real client. No CustomDialContext, no NodePort tunneling, no
	// DinD remap. The `make test-conformance` Makefile target attaches
	// this container to the kind network (`docker run --network kind`),
	// which gives us identical behaviour locally and under GitLab DinD.
	// Wrap the upstream DefaultRoundTripper so any failing request
	// triggers a snapshot of HAProxy state + the chart-published
	// HAProxyCfg CRD into a labelled ConfigMap. The CI after_script
	// collects those ConfigMaps; see snapshotter.go for the contract.
	// Captures fire at the exact moment of failure, while the
	// upstream test framework's t.Cleanup() (which deletes the
	// conformance fixtures) hasn't run yet — so the snapshot reflects
	// the state HAProxy was actually serving when the request failed,
	// and the HAProxyCfg dump shows what the chart RENDERED before
	// the dataplane API translated it into incremental ops.
	dyn, err := dynamic.NewForConfig(cfg)
	require.NoError(t, err, "create dynamic client for snapshot HAProxyCfg dumps")
	rt := newSnapshottingRoundTripper(
		&roundtripper.DefaultRoundTripper{
			Debug:         debug,
			TimeoutConfig: timeoutCfg,
		},
		cs,
		dyn,
		cfg,
	)
	// Intentionally do NOT set GRPCClient on ConformanceOptions below.
	// Upstream PR #3130 (kubernetes-sigs/gateway-api#3130) makes
	// MakeRequestAndExpectEventuallyConsistentResponse create a fresh
	// *grpc.DefaultClient per call when the suite-level client is nil,
	// avoiding the race where one parallel subtest's `defer c.Close()`
	// tears down a *grpc.ClientConn while siblings are still mid-RPC
	// (upstream issue #3122). Envoy Gateway, Contour, Cilium, Traefik
	// all leave it nil for this reason.

	// SupportGatewayStaticAddresses substitutes PLACEHOLDER_USABLE_ADDRS /
	// PLACEHOLDER_UNUSABLE_ADDRS in its Gateway fixture with the entries
	// of UsableNetworkAddresses / UnusableNetworkAddresses we pass below.
	// Without these, the test panics on
	// `require.Len(currentGW.Spec.Addresses, 3)` because the placeholder
	// substitution drops the entries entirely. We discover the realistic
	// pool from MetalLB at suite setup time so the Usable IP is one
	// MetalLB will actually allocate; Unusable is a reserved-test
	// (RFC 5737 TEST-NET-1) IP MetalLB will never bind.
	usable, unusable, err := discoverStaticAddressPools(t.Context(), cfg)
	require.NoError(t, err, "derive static-addresses pools from MetalLB IPAddressPool")

	// The implementation version recorded in the conformance report. The
	// dedicated report job passes the released chart version; defaults to
	// "main" for ad-hoc local runs.
	implVersion := os.Getenv("CONFORMANCE_IMPL_VERSION")
	if implVersion == "" {
		implVersion = "main"
	}

	opts := suite.ConformanceOptions{
		Client:        c,
		ClientOptions: clientOpts,
		Clientset:     cs,
		RestConfig:    cfg,
		RoundTripper:  rt,
	}
	// gateway-api v1.6.0 moved the run-configuration fields into the embedded
	// ConfigurableOptions. A composite literal can't set promoted fields, but
	// assigning them post-construction works (and reads the same).
	opts.GatewayClassName = gatewayClassName
	opts.Debug = debug
	// Two DISTINCT cleanup knobs — v1.6.0 split them and they must be set
	// differently:
	//
	//   CleanupTestResources (per-TEST routes, deleted at each subtest's
	//   end) MUST be true. It gates the per-test t.Cleanup that deletes a
	//   test's HTTPRoutes/GRPCRoutes/etc. between tests. Left false (the Go
	//   zero value — v1.6.0 moved this from a hardcoded `true` to this new
	//   ConformanceOptions field, so an unset field silently disables it),
	//   every test's routes stay applied to the SHARED `same-namespace`
	//   Gateway for the whole run. They then co-reside: one test's no-hostname
	//   catch-all route (e.g. backend-protocol-h2c) serves another test's
	//   non-matching paths (exact-matching `/Two` → 200 instead of 404),
	//   failing ~all HTTP/GRPC routing tests even though each passes in
	//   isolation. This is test-isolation, not a data-plane bug.
	//
	//   CleanupBaseResources (shared Gateways/backends, deleted at SUITE end)
	//   stays false so the after_script can inspect the base topology after a
	//   shard exits. Per-test route diagnosis does NOT depend on this: the
	//   snapshotting RoundTripper above captures haproxy.cfg + the HAProxyCfg
	//   CRD at the exact moment each request fails, before that test's
	//   t.Cleanup runs. The kind cluster is per-shard ephemeral so the
	//   leftover base fixtures cost nothing.
	opts.CleanupTestResources = true
	opts.CleanupBaseResources = false
	opts.SupportedFeatures = supported.UnsortedList()
	opts.TimeoutConfig = timeoutCfg
	// v1.6.0 removed suite.ParseImplementation; build the report identity inline.
	opts.Implementation = confv1.Implementation{
		Organization: "haproxy-haptic",
		Project:      "haptic",
		URL:          "https://github.com/haproxy-haptic/HAPTIC",
		Version:      implVersion,
		Contact:      []string{"https://github.com/haproxy-haptic/HAPTIC/issues"},
	}
	// SkipTests is the right place to opt-out of *individual* upstream
	// tests when a specific assertion is known broken — never use
	// t.Skip() for the whole suite. Each entry must include an issue
	// link in a comment so it can be revisited.
	opts.SkipTests = []string{
		// SNI determines the verified name; independent DNS/URI SAN matching is unsupported.
		// https://github.com/haproxy/haproxy/issues/3452
		"BackendTLSPolicySANValidation",
	}
	opts.UsableNetworkAddresses = usable
	opts.UnusableNetworkAddresses = unusable

	// When CONFORMANCE_REPORT_OUTPUT is set, emit a submittable
	// ConformanceReport to that path (RunConformanceWithOptions writes it
	// when ReportOutputPath != ""). The report is keyed on ConformanceProfiles;
	// these are REPORT-ONLY — test selection stays SupportedFeatures-driven
	// (suite.go), so the per-shard gate (which leaves both unset) is byte-for-
	// byte unchanged. A report compiled from a single shard would be
	// incomplete — the suite only knows about the tests THIS instance ran — so
	// report generation is reserved for the dedicated UNSHARDED full-suite job.
	// haptic is a Gateway (not mesh) implementation, hence the GATEWAY-* profiles.
	if reportPath := os.Getenv("CONFORMANCE_REPORT_OUTPUT"); reportPath != "" {
		opts.ReportOutputPath = reportPath
		opts.ConformanceProfiles = []suite.ConformanceProfileName{
			suite.GatewayHTTPConformanceProfileName,
			suite.GatewayTLSConformanceProfileName,
			suite.GatewayGRPCConformanceProfileName,
		}
	}

	// The GatewayClass is created by the CONTROLLER at runtime (SSA on the
	// first render after the gatewayclasses CRD resolves) — not by Helm —
	// so a fresh install may not have it the instant the suite starts. The
	// upstream suite's setup fails immediately on a missing class instead
	// of polling, so gate here until it exists.
	waitForGatewayClassExists(t, opts.Client, gatewayClassName)

	isolateSharedReferenceGrants(t)
	gwconformance.RunConformanceWithOptions(t, opts)
}

// installConformanceSchemes registers every API group the upstream suite reads.
func installConformanceSchemes(t *testing.T, scheme *runtime.Scheme) {
	t.Helper()
	require.NoError(t, v1alpha3.Install(scheme))
	require.NoError(t, v1alpha2.Install(scheme))
	require.NoError(t, v1beta1.Install(scheme))
	require.NoError(t, xv1alpha1.Install(scheme))
	require.NoError(t, gatewayv1.Install(scheme))
	require.NoError(t, apiextensionsv1.AddToScheme(scheme))
}

// Mixed TLS uses standard fields; retry codes require the experimental channel.
func supportedConformanceFeatures(t *testing.T, c client.Client) sets.Set[features.FeatureName] {
	t.Helper()
	// Exclude only documented dataplane or architecture gaps from standard features.
	excluded := sets.New[features.FeatureName](
		// HAProxy lacks an overall request deadline; see docs/site/docs/libraries/gateway.md#timeout-limits.
		features.SupportHTTPRouteRequestTimeout,
		// Mesh / GAMMA features are permanently out of scope for haptic.
		// The conformance tests exercise pod-to-pod east-west traffic
		// (`echo.ConnectToApp` / `ConnectToAppInNamespace`) that never
		// traverses the chart's edge HAProxy: HTTPRoutes attach to a
		// Service `parentRef`, ClusterIPMatching requires seeing the
		// original destination IP before NAT, and ConsumerRoute
		// dispatches by the source pod's namespace. All three assume a
		// sidecar-per-pod (or kernel-level interceptor) data plane.
		// haptic is a single front-door HAProxy by design; closing
		// these would mean shipping a different product. Not deferred
		// — abandoned.
		features.SupportMesh,
		features.SupportMeshClusterIPMatching,
		features.SupportMeshConsumerRoute,
		// UDPRoute relies on UDP listeners and backend forwarding.
		// HAProxy OSS has no UDP load balancing — `udp@` listeners
		// exist but are restricted to `log-forward` sections (syslog
		// ingress), QUIC listeners terminate HTTP/3 (not arbitrary
		// UDP), and the resolvers subsystem is outbound DNS only.
		// There is no `mode udp` and no way to forward datagrams to
		// upstream UDP servers. Full UDP routing is a HAPEE-only
		// feature (the `hapee-lb-udp` enterprise module), out of
		// scope for an OSS chart.
		features.SupportUDPRoute,
	)
	supported := sets.Set[features.FeatureName]{}
	for _, f := range features.AllFeatures.UnsortedList() {
		if f.Channel != features.FeatureChannelStandard {
			continue
		}
		if excluded.Has(f.Name) {
			continue
		}
		supported.Insert(f.Name)
	}
	supported.Insert(features.SupportTLSRouteModeMixed)
	experimental, err := servesExperimentalChannel(t.Context(), c)
	require.NoError(t, err)
	if experimental {
		retryFeature, err := retryCodesFeature(features.AllFeatures.UnsortedList())
		require.NoError(t, err)
		supported.Insert(retryFeature)
	}
	return supported
}

func isolateSharedReferenceGrants(t *testing.T) {
	t.Helper()
	original := conformancetests.ConformanceTests
	isolated := append([]suite.ConformanceTest(nil), original...)
	for i := range isolated {
		// These fixtures change each other's permissions; see tests/README.md.
		switch isolated[i].ShortName {
		case "GatewaySecretMissingReferenceGrant", "GatewaySecretInvalidReferenceGrant",
			"GatewaySecretReferenceGrantAllInNamespace", "GatewaySecretReferenceGrantSpecific",
			"ListenerSetReferenceGrant":
			isolated[i].Parallel = false
		}
	}
	conformancetests.ConformanceTests = isolated
	t.Cleanup(func() { conformancetests.ConformanceTests = original })
}

// waitForGatewayClassExists polls until the named GatewayClass is present.
func waitForGatewayClassExists(t *testing.T, c client.Client, name string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		gc := &gatewayv1.GatewayClass{}
		err := c.Get(context.Background(), client.ObjectKey{Name: name}, gc)
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("GatewayClass %q not created by the controller within 3m: %v", name, err)
		}
		time.Sleep(2 * time.Second)
	}
}

// discoverStaticAddressPools returns sample Usable and Unusable
// GatewaySpecAddress entries for the SupportGatewayStaticAddresses tests
// to substitute into the placeholder fixtures. We discover them at suite
// startup time rather than hardcoding, because kind's docker network can
// shift host-to-host and the e2e suite's IPAddressPool is sized to that
// network.
//
//   - Usable: pulled from the e2e MetalLB IPAddressPool's high end
//     (.249), which is reserved-by-convention for this purpose. The pool
//     covers .200-.250 (see tests/e2e/metallb.go); we pick a single IP
//     from the top so a real allocation against it is improbable but
//     possible.
//
//   - Unusable: 192.0.2.1, the first address of TEST-NET-1 (RFC 5737).
//     MetalLB will never allocate this since it isn't in any
//     IPAddressPool, so the conformance test sees Programmed=False/
//     AddressNotUsable as the spec requires.
//
// Both lists return one address each — the conformance test asserts
// `require.Len(currentGW.Spec.Addresses, 3)` (one invalid type +
// one Usable + one Unusable) so any other count breaks the fixture.
//
// Returns an error rather than t.Fatal so the caller can attach a
// helpful require.NoError message.
func discoverStaticAddressPools(ctx context.Context, restConfig *rest.Config) (usable, unusable []v1beta1.GatewaySpecAddress, err error) {
	// Apply a short timeout so a misconfigured cluster fails fast rather
	// than blocking the whole conformance suite on the static-addresses
	// fixture setup.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	dyn, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, nil, err
	}

	pool, err := dyn.Resource(metalLBPoolGVR).
		Namespace("metallb-system").
		Get(ctx, "e2e-pool", metav1.GetOptions{})
	if err != nil {
		// The e2e-pool only exists when the test was set up by
		// tests/e2e/main_test.go (not in CI matrix where MetalLB is
		// installed differently). Fall back to documented sentinels:
		// the conformance suite will use them and SupportGatewayStaticAddresses
		// tests will fail with a clearer message than a panic.
		ipAddr := v1beta1.IPAddressType
		usableAddr := v1beta1.GatewaySpecAddress{Type: &ipAddr, Value: "192.0.2.10"}
		unusableAddr := v1beta1.GatewaySpecAddress{Type: &ipAddr, Value: "192.0.2.1"}
		return []v1beta1.GatewaySpecAddress{usableAddr},
			[]v1beta1.GatewaySpecAddress{unusableAddr}, nil
	}

	// Pool addresses are recorded under spec.addresses as a string slice
	// like ["172.18.255.200-172.18.255.250"]. Pull the high end (.249)
	// for Usable; treat anything outside as Unusable.
	addresses, _, err := unstructured.NestedStringSlice(pool.Object, "spec", "addresses")
	if err != nil {
		return nil, nil, err
	}
	usableValue := "192.0.2.10"
	if len(addresses) > 0 {
		// Best-effort parse of the first range entry's high octet+249.
		// We intentionally pick a single deterministic IP rather than
		// scanning for a free one; MetalLB takes care of allocation.
		usableValue = pickAddressFromRange(addresses[0])
	}

	ipAddr := v1beta1.IPAddressType
	usable = []v1beta1.GatewaySpecAddress{{Type: &ipAddr, Value: usableValue}}
	unusable = []v1beta1.GatewaySpecAddress{{Type: &ipAddr, Value: "192.0.2.1"}}
	return usable, unusable, nil
}

// pickAddressFromRange returns a single IP from a "<start>-<end>" range
// expression. Picks the second-from-end (.249 of a .200-.250 pool) so
// a colliding e2e test allocation is improbable. Falls back to a
// reserved-test sentinel if the format isn't parseable.
func pickAddressFromRange(rangeStr string) string {
	// Split on "-"; expect "172.18.255.200-172.18.255.250" shape.
	for i := 0; i < len(rangeStr)-1; i++ {
		if rangeStr[i] == '-' {
			high := rangeStr[i+1:]
			// Replace last octet with .249 if the high end ends in .250.
			for j := len(high) - 1; j >= 0; j-- {
				if high[j] == '.' {
					return high[:j+1] + "249"
				}
			}
			break
		}
	}
	return "192.0.2.10"
}
