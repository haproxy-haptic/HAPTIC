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

// Package e2e is the full-stack end-to-end test suite for
// HAPTIC. It is self-contained: TestMain creates its own kind
// cluster, helm-installs the chart, and deploys backend fixtures. Nothing
// outside the test binary needs to run first.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/gateway-api/pkg/consts"

	haproxyv1alpha1 "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	devassets "github.com/haproxy-haptic/HAPTIC/scripts/dev-env-assets"
	"github.com/haproxy-haptic/HAPTIC/tests/e2e/e2ecluster"
	"github.com/haproxy-haptic/HAPTIC/tests/kindutil"
	"github.com/haproxy-haptic/HAPTIC/tests/kubeexec"
	"github.com/haproxy-haptic/HAPTIC/tests/process"
	"github.com/haproxy-haptic/HAPTIC/tests/testutil"
)

// testEnv is the e2e-framework environment shared by all tests in the suite.
// Initialised in TestMain. Tests run via testEnv.Test(t, feature).
var testEnv env.Environment

var e2eCluster = e2ecluster.Default()

// ClusterName is the kind cluster the e2e suite owns.
var ClusterName = e2eCluster.ClusterName

var kubeconfigPath = e2eCluster.KubeconfigPath

var ownedE2ECluster *kindutil.Cluster

func init() {
	// Register the HAProxyTemplateConfig CRD types with the global scheme so
	// the e2e-framework client can read/write them via dynamic typed access.
	if err := haproxyv1alpha1.AddToScheme(clientgoscheme.Scheme); err != nil {
		panic(fmt.Sprintf("e2e: register haproxy scheme: %v", err))
	}
}

// TestMain wires the suite's lifecycle: create or reuse the kind cluster,
// helm-install the chart, deploy backend fixtures, then run the tests.
//
// Cluster lifecycle is governed by:
//   - KEEP_CLUSTER (default true): keep cluster after the suite for fast
//     iteration. Set to false for ephemeral runs.
//   - SKIP_CLUSTER_CREATE: assume cluster already exists; skip the kind
//     provider call entirely. Set by the CI runner when helm/kind-action
//     pre-creates the cluster.
//
// Image expectations: haptic:test-haproxyX.Y must exist in the local Docker daemon
// before running. The Makefile target `test-e2e` depends on
// `docker-build-test` to build it.
func initializeE2ERuntime() error {
	var err error
	runtimeImages, err = kindutil.LoadChartImages(os.Getenv("HAPTIC_HAPROXY_VERSION"))
	if err != nil {
		return fmt.Errorf("chart runtime images: %w", err)
	}
	if err := kindutil.ValidateChartImageTag(runtimeImages.SPOAHub, os.Getenv("SPOA_TAG")); err != nil {
		return fmt.Errorf("SPOA_TAG: %w", err)
	}
	ChartHAProxyVersion = runtimeImages.HAProxyVersion
	ControllerImageTag = os.Getenv("HAPTIC_E2E_IMAGE_TAG")
	if ControllerImageTag == "" {
		ControllerImageTag = "test"
	}
	ControllerImageName = "haptic:" + ControllerImageTag + "-haproxy" + ChartHAProxyVersion
	fmt.Fprintf(os.Stderr, "e2e: chart HAProxy image %s\n", runtimeImages.HAProxy)
	e2eCluster, err = e2ecluster.Load()
	if err != nil {
		return fmt.Errorf("cluster configuration: %w", err)
	}
	ClusterName = e2eCluster.ClusterName
	kubeconfigPath = e2eCluster.KubeconfigPath

	if _, err := expectedControllerIdentity(); err != nil {
		return err
	}
	testEnv = env.NewParallel()

	// SAFETY: Isolate kubeconfig.
	if err := os.Setenv("KUBECONFIG", kubeconfigPath); err != nil {
		return fmt.Errorf("set KUBECONFIG: %w", err)
	}
	return nil
}

func TestMain(m *testing.M) {
	runStagedPostRenderer()
	flag.Parse()
	if flag.Lookup("test.list").Value.String() != "" {
		os.Exit(m.Run())
	}
	if err := initializeE2ERuntime(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}

	// Wire the at-failure HAProxy state snapshotter into the
	// httpclient package's poll-timeout hook. Triggered when a
	// test's HTTP polling exhausts its 180s retry budget; dumps
	// the chart-rendered HAProxyCfg + the running pod's
	// /etc/haproxy tree BEFORE the test's t.Cleanup chain deletes
	// per-test fixtures. Without this, the standard
	// DumpLogsOnFailure runs after fixture deletion and captures
	// only the empty-defaults post-cleanup state — leaving real
	// failures undiagnosable from CI artifacts. See snapshot.go
	// for the per-test throttle.
	InstallFailureSnapshotter()

	// Heartbeat: emit current setup phase + elapsed every 5s so the GitLab
	// job trace shows progress instead of looking frozen for the ~4-minute
	// window between `go: downloading` and the first test PASS line.
	// `go test -v` + `t.Parallel()` buffers per-test output until parents
	// complete, but TestMain-side writes from a goroutine to os.Stderr
	// flush in real time.
	heartbeatCtx, heartbeatStop := context.WithCancel(context.Background())
	startSetupHeartbeat(heartbeatCtx)

	testEnv.Setup(
		phase("cluster-create", setupCluster),
		phase("load-controller-image", func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			return loadControllerImage(ctx)
		}),
		phase("ensure-namespaces", func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			return ctx, ensureNamespaces(ctx)
		}),
		phase("install-cluster-services (parallel chains)", installClusterServices),
		phase("verify-controller-rollout", func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			client, err := cfg.NewClient()
			if err != nil {
				return ctx, fmt.Errorf("new client: %w", err)
			}
			clientset, err := newClientsetForE2E(client.RESTConfig())
			if err != nil {
				return ctx, fmt.Errorf("new clientset: %w", err)
			}
			return ctx, verifyControllerRollout(ctx, clientset)
		}),
		phase("wait-environment-ready", func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			client, err := cfg.NewClient()
			if err != nil {
				return ctx, fmt.Errorf("new client: %w", err)
			}
			return ctx, WaitForE2EEnvironmentReady(ctx, client)
		}),
		phase("configure-traffic-endpoint", configureTrafficEndpoint),
		phase("wait-profile-ready", waitForProfileReady),
		phase("verify-controller-binary", func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			if os.Getenv(scaleEnableEnv) == "1" {
				return ctx, nil
			}
			client, err := cfg.NewClient()
			if err != nil {
				return ctx, fmt.Errorf("new client: %w", err)
			}
			clientset, err := newClientsetForE2E(client.RESTConfig())
			if err != nil {
				return ctx, fmt.Errorf("new clientset: %w", err)
			}
			return ctx, verifyControllerBinary(ctx, clientset, nil)
		}),
		phase("verify-runtime-images", func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			client, err := cfg.NewClient()
			if err != nil {
				return ctx, fmt.Errorf("new client: %w", err)
			}
			clientset, err := newClientsetForE2E(client.RESTConfig())
			if err != nil {
				return ctx, fmt.Errorf("new clientset: %w", err)
			}
			pods, err := clientset.CoreV1().Pods(ControllerNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: "app.kubernetes.io/instance=" + HelmReleaseName,
			})
			if err != nil {
				return ctx, fmt.Errorf("read runtime pods: %w", err)
			}
			return ctx, kindutil.ValidatePodContainerImages(pods.Items, map[string]string{
				"haproxy": runtimeImages.HAProxy, "vector": runtimeImages.Vector,
				"spoa-hub": runtimeImages.SPOAHub, "validators": runtimeImages.SPOAHub,
			})
		}),
		phase("tests-running", func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			return ctx, nil
		}),
	)

	testEnv.Finish(
		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			return teardownCluster(ctx)
		},
	)

	code := testEnv.Run(m)
	heartbeatStop()
	os.Exit(code)
}

func waitForProfileReady(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
	if os.Getenv("HAPTIC_E2E_PROFILE") != "cache" {
		return ctx, nil
	}
	client, err := cfg.NewClient()
	if err != nil {
		return ctx, fmt.Errorf("new client: %w", err)
	}
	return ctx, waitForVarnishReplicasReady(ctx, client, 1)
}

func installClusterServices(ctx context.Context, _ *envconf.Config) (context.Context, error) {
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		_, err := installMetalLB(gctx)
		return err
	})
	g.Go(func() error {
		caBundleB64, err := preInstallParallel(gctx)
		if err != nil {
			return err
		}
		if os.Getenv("HAPTIC_E2E_PROFILE") != "conformance" {
			if _, err := applyBackendFixtures(gctx); err != nil {
				return err
			}
		}
		_, err = helmInstallChart(gctx, caBundleB64)
		return err
	})
	return ctx, g.Wait()
}

// setupPhase is the current TestMain phase, read by the heartbeat goroutine
// and updated by the phase() wrapper before each setup step runs.
var setupPhase atomic.Pointer[string]

// setupStart marks when TestMain began. The heartbeat reports
// time-since-this so each line shows total elapsed setup time.
var setupStart = time.Now()

// phase wraps an env.Func to record the current setup-phase label before
// running it, so the heartbeat reflects which step is currently executing.
func phase(name string, fn env.Func) env.Func {
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		setupPhase.Store(&name)
		return fn(ctx, cfg)
	}
}

// startSetupHeartbeat prints the current TestMain phase + elapsed-since-start
// to stderr every 5 seconds until ctx is cancelled. Restores legibility to
// the GitLab job trace, which otherwise sits silent for ~4 minutes during
// compile + cluster-bring-up + helm install + fixture deploy.
func startSetupHeartbeat(ctx context.Context) {
	initial := "init"
	setupPhase.Store(&initial)
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				p := setupPhase.Load()
				if p == nil {
					continue
				}
				fmt.Fprintf(os.Stderr, "[e2e] phase=%s elapsed=%ds\n", *p, int(time.Since(setupStart).Seconds()))
			}
		}
	}()
}

// setupCluster creates the kind cluster if it doesn't already exist,
// patches the kubeconfig for DinD if applicable, and writes it to the
// suite's isolated kubeconfig path.
func setupCluster(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
	if os.Getenv("SKIP_CLUSTER_CREATE") == "true" {
		return ctx, configurePrecreatedCluster(cfg)
	}

	clusterExists, err := kindutil.ClusterExists(ctx, process.Executor{}, ClusterName, kindutil.DockerEnvironment(os.Getenv))
	if err != nil {
		return ctx, err
	}
	if clusterExists && e2eCluster.RequireNew {
		return ctx, fmt.Errorf("isolated kind cluster %q already exists; choose a unique cluster name", ClusterName)
	}
	if e2eCluster.RequireNew {
		if _, err := os.Lstat(kubeconfigPath); err == nil {
			return ctx, fmt.Errorf("isolated kubeconfig path already exists: %s", kubeconfigPath)
		} else if !os.IsNotExist(err) {
			return ctx, fmt.Errorf("inspect isolated kubeconfig path: %w", err)
		}
	}

	if !clusterExists {
		if err := createE2ECluster(ctx); err != nil {
			return ctx, err
		}
		// Best-effort metrics-server so the rolling-restart failure snapshot's
		// `kubectl top` capture has real utilization data. Non-fatal.
		installMetricsServerBestEffort(ctx)
	} else {
		ownedE2ECluster, _, err = kindutil.ResumeCluster(ctx, process.Executor{}, ClusterName, kindutil.DockerEnvironment(os.Getenv))
		if err != nil {
			return ctx, err
		}
	}

	kubeconfig, err := kindutil.ClusterKubeconfig(ctx, process.Executor{}, ClusterName, kindutil.DockerEnvironment(os.Getenv))
	if err != nil {
		return ctx, fmt.Errorf("get kubeconfig for %q: %w", ClusterName, err)
	}
	if err := e2eCluster.WriteKubeconfig([]byte(kubeconfig)); err != nil {
		return ctx, fmt.Errorf("write kubeconfig: %w", err)
	}
	cfg.WithKubeconfigFile(kubeconfigPath)
	return ctx, nil
}

func configurePrecreatedCluster(cfg *envconf.Config) error {
	if e2eCluster.RequireNew {
		return fmt.Errorf("SKIP_CLUSTER_CREATE=true cannot use e2e isolation overrides")
	}
	kc := os.Getenv("KUBECONFIG")
	if kc == "" {
		return fmt.Errorf("SKIP_CLUSTER_CREATE=true but KUBECONFIG is empty")
	}
	cfg.WithKubeconfigFile(kc)
	return nil
}

func createE2ECluster(ctx context.Context) error {
	artifacts, err := os.MkdirTemp("", "haptic-e2e-")
	if err != nil {
		return err
	}
	ownedE2ECluster, err = kindutil.NewCluster(process.Executor{}, &kindutil.ClusterOptions{
		Name: ClusterName, Kubeconfig: filepath.Join(artifacts, "kubeconfig"), Artifacts: artifacts,
		Config: []byte(e2eCluster.KindConfig()), Environment: kindutil.DockerEnvironment(os.Getenv), ReadyTimeout: DefaultClusterCreateTimeout,
		NetworkLabels: e2eCluster.NetworkLabels,
	})
	if err != nil {
		return err
	}
	if err := ownedE2ECluster.Create(ctx, os.Getenv("KIND_NODE_IMAGE")); err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		return errors.Join(err, ownedE2ECluster.Close(cleanup))
	}
	return nil
}

// installMetricsServerBestEffort applies metrics-server to the freshly-created
// kind cluster so `kubectl top pods/nodes` works for the rolling-restart
// failure snapshot's utilization capture. Deliberately best-effort and
// non-fatal: it applies the manifest and returns without waiting for readiness.
// metrics-server needs ~15-30s to start scraping, which the subsequent
// image-load + helm-install + fixture-deploy comfortably covers; and if a
// restricted-egress CI can't pull the image, `kubectl top` simply returns no
// data (empty snapshot capture) — never a reason to fail cluster setup.
func installMetricsServerBestEffort(ctx context.Context) {
	node := ClusterName + "-control-plane"
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", node,
		"kubectl", "--kubeconfig=/etc/kubernetes/admin.conf", "apply", "-f", "-")
	cmd.Stdin = bytes.NewReader(devassets.MetricsServerYAML)
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: metrics-server apply (best-effort) failed: %v: %s\n", err, out)
		return
	}
	fmt.Fprintln(os.Stderr, "e2e: metrics-server applied (best-effort; kubectl top available once it scrapes)")
}

// loadControllerImage loads haptic:test-haproxyX.Y into the kind cluster so Helm
// install can find it (the chart sets imagePullPolicy: Never via dev-values).
// Skipped when SKIP_CLUSTER_CREATE=true (CI does its own load).
//
// We avoid `kind load docker-image` because it stages the image as a tar
// in $TMPDIR and then docker saves into that path — which fails when
// dockerd runs under systemd with PrivateTmp=yes (the daemon and the
// caller see different /tmp namespaces). Instead, pipe `docker save`
// straight into `ctr image import` inside the kind control-plane
// container. Same effect, no host temp files.
func loadControllerImage(ctx context.Context) (context.Context, error) {
	if os.Getenv("SKIP_CLUSTER_CREATE") == "true" {
		return ctx, nil
	}
	if err := loadImageIntoKind(ctx, ControllerImageName); err != nil {
		return ctx, err
	}
	// Every non-conformance shard deploys the echo-server fixture. Preload its
	// pinned image so individual test namespaces never depend on a kind node's
	// Docker Hub path while their short endpoint-readiness deadline is running.
	if os.Getenv("HAPTIC_E2E_PROFILE") != "conformance" {
		if err := pullImageIntoKind(ctx, echoServerImage); err != nil {
			return ctx, err
		}
	}
	if os.Getenv("HAPTIC_E2E_PROFILE") == "cache" {
		if err := loadLocalOrPulledImageIntoKind(ctx, runtimeImages.Varnish); err != nil {
			return ctx, err
		}
		if err := pullImageIntoKind(ctx, VarnishPolicyProbeImage); err != nil {
			return ctx, err
		}
	}
	// The shared rate-limit shard deploys Valkey. Load the stock image into kind
	// for the same reason as the cache shard's Varnish image: deterministic CI
	// and no dependency on the kind node reaching Docker Hub.
	if os.Getenv("HAPTIC_E2E_PROFILE") == "rate-limit" {
		if err := pullImageIntoKind(ctx, runtimeImages.Valkey); err != nil {
			return ctx, err
		}
	}
	if err := loadLocalOrPulledImageIntoKind(ctx, runtimeImages.SPOAHub); err != nil {
		return ctx, err
	}
	return ctx, nil
}

// pullImageRetries is how many times a registry pull is attempted before the
// suite gives up. Every e2e shard pulls these images during TestMain, so a
// single upstream blip used to red a whole pipeline for no local reason: a
// Docker Hub `502 Bad Gateway` on ealen/echo-server took out test-e2e-rate-limit
// on main before any test ran. The retry covers only the network fetch — the
// kind import that follows is local and fails deterministically.
const pullImageRetries = 3

func loadLocalOrPulledImageIntoKind(ctx context.Context, image string) error {
	if err := exec.CommandContext(ctx, "docker", "image", "inspect", image).Run(); err == nil {
		return loadImageIntoKind(ctx, image)
	}
	return pullImageIntoKind(ctx, image)
}

func pullImageIntoKind(ctx context.Context, image string) error {
	var err error
	for attempt := 1; attempt <= pullImageRetries; attempt++ {
		pull := exec.CommandContext(ctx, "docker", "pull", image)
		pull.Stdout, pull.Stderr = os.Stderr, os.Stderr
		if err = pull.Run(); err == nil {
			return loadImageIntoKind(ctx, image)
		}
		if attempt == pullImageRetries {
			break
		}
		backoff := time.Duration(attempt*attempt) * 2 * time.Second
		fmt.Fprintf(os.Stderr, "e2e: docker pull %s failed (attempt %d/%d): %v; retrying in %s\n",
			image, attempt, pullImageRetries, err, backoff)
		select {
		case <-ctx.Done():
			return fmt.Errorf("docker pull %s: %w", image, ctx.Err())
		case <-time.After(backoff):
		}
	}
	return fmt.Errorf("docker pull %s failed after %d attempts: %w", image, pullImageRetries, err)
}

// loadImageIntoKind pipes `docker save <image>` into `ctr image import` inside
// the kind control-plane container. We avoid `kind load docker-image` because it
// stages a tar in $TMPDIR, which fails when dockerd runs under systemd
// PrivateTmp=yes (daemon and caller see different /tmp namespaces).
func loadImageIntoKind(ctx context.Context, image string) error {
	saveCmd := exec.CommandContext(ctx, "docker", "save", image)
	importCmd := exec.CommandContext(ctx, "docker", "exec", "-i",
		ClusterName+"-control-plane",
		"ctr", "--namespace=k8s.io", "images", "import", "-")

	pipe, err := saveCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("pipe docker save: %w", err)
	}
	importCmd.Stdin = pipe
	importCmd.Stdout = os.Stderr
	importCmd.Stderr = os.Stderr
	saveCmd.Stderr = os.Stderr

	if err := importCmd.Start(); err != nil {
		return fmt.Errorf("start ctr import: %w", err)
	}
	if err := saveCmd.Run(); err != nil {
		_ = importCmd.Wait()
		return fmt.Errorf("docker save %s: %w", image, err)
	}
	if err := importCmd.Wait(); err != nil {
		return fmt.Errorf("ctr image import %s: %w", image, err)
	}
	return nil
}

// installCRDs applies the chart's CRD manifests. We do this separately
// from helm install because the chart references the CRDs and they must
// exist before the chart installs.
func installCRDs(ctx context.Context) (context.Context, error) {
	crdDir, err := chartCRDDir()
	if err != nil {
		return ctx, err
	}
	cmd := exec.CommandContext(ctx, "kubectl", "apply", kubeconfigFlag, kubeconfigPath, "-f", crdDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return ctx, fmt.Errorf("kubectl apply CRDs: %w (output: %s)", err, out)
	}
	// Wait for Establishment before the controller starts — applying is not
	// enough. A chart CRD that establishes after the controller's initial
	// discovery forces a reinitialization (leadership flap); production
	// establishes CRDs before the controller Deployment rolls (Helm).
	if err := waitCRDsEstablished(ctx, ".haproxy-haptic.org"); err != nil {
		return ctx, err
	}
	return ctx, nil
}

// waitCRDsEstablished blocks until every currently-installed CRD whose name
// contains nameSubstr reports Established. Listing after apply keeps it correct
// regardless of which exact CRDs a given channel/version installed.
func waitCRDsEstablished(ctx context.Context, nameSubstr string) error {
	listed, err := exec.CommandContext(ctx, "kubectl", "get", "crd",
		kubeconfigFlag, kubeconfigPath, "-o", "name").Output()
	if err != nil {
		return fmt.Errorf("list CRDs (%s): %w", nameSubstr, err)
	}
	var crds []string
	for _, line := range strings.Split(string(listed), "\n") {
		if name := strings.TrimSpace(line); strings.Contains(name, nameSubstr) {
			crds = append(crds, name)
		}
	}
	if len(crds) == 0 {
		return fmt.Errorf("no CRDs matching %q found after install", nameSubstr)
	}
	client := kubeexec.Client{Runner: process.Executor{}, Kubeconfig: kubeconfigPath, Context: "kind-" + ClusterName}
	if err := client.WaitCRDsEstablished(ctx, crds, time.Minute); err != nil {
		return fmt.Errorf("wait for CRDs established (%s): %w", nameSubstr, err)
	}
	return nil
}

// defaultGatewayAPIVersion is the Gateway API release whose standard-channel
// CRDs the suite installs by default. Read from the module rather than pinned
// by hand: it is the exact value the CRDs carry in their bundle-version
// annotation, and the conformance suite refuses to start when that disagrees
// with its own module version. A hand-written copy goes stale the moment
// sigs.k8s.io/gateway-api is bumped in go.mod (job 15627486657).
const defaultGatewayAPIVersion = consts.BundleVersion

// installGatewayAPICRDs installs the selected upstream Gateway API channel.
//
// HAPTIC_E2E_GWAPI_VERSION overrides the installed release — the nightly
// gwapi-matrix CI job sets an old release tag (e.g. v1.1.0) to verify
// runtime version detection against a live old cluster, and the nightly
// canary job sets a git ref ("main") to test against unreleased upstream
// CRDs. TestGatewayAPIReleaseMatrix only runs under an override.
//
// Idempotent: if CRDs already exist (KEEP_CLUSTER=true reuse), kubectl apply
// is a no-op and the wait condition just returns immediately.
func installGatewayAPICRDs(ctx context.Context) (context.Context, error) {
	version := os.Getenv("HAPTIC_E2E_GWAPI_VERSION")
	if version == "" {
		version = defaultGatewayAPIVersion
	}
	channel := os.Getenv("HAPTIC_E2E_GWAPI_CHANNEL")
	return ctx, applyGatewayAPICRDs(ctx, version, channel)
}

// kubectlWithBackoff runs a kubectl command that fetches a manifest from GitHub,
// whose release assets fail for about a minute at a time (run 38571838388).
func kubectlWithBackoff(ctx context.Context, args ...string) (out []byte, err error) {
	const attempts = 7
	for attempt := 1; attempt <= attempts; attempt++ {
		if out, err = exec.CommandContext(ctx, "kubectl", args...).CombinedOutput(); err == nil || ctx.Err() != nil {
			return out, err
		}
		if attempt < attempts {
			select {
			case <-ctx.Done():
			case <-time.After(time.Duration(1<<attempt) * time.Second): // 2s, 4s, ... 64s
			}
		}
	}
	return out, err
}

func applyGatewayAPICRDs(ctx context.Context, version, channel string) error {
	args, err := e2ecluster.GatewayAPIInstallArgs(version, channel, kubeconfigPath)
	if err != nil {
		return err
	}
	out, err := kubectlWithBackoff(ctx, args...)
	if err != nil {
		return fmt.Errorf("install Gateway API CRDs (%s, %s) after retries: %w (output: %s)", version, channel, err, out)
	}

	// Wait for EVERY installed Gateway API CRD to be Established before the
	// controller starts — not just the standard four. The experimental channel
	// also installs tcproutes/tlsroutes/backendtlspolicies/listenersets, which
	// the controller watches; a CRD that only becomes Established after the
	// controller's initial discovery is seen as "added" moments later, forcing a
	// full reinitialization and a leadership flap (~30s leaderless while both
	// replicas re-run the startup load gate). Production establishes all CRDs
	// before the controller Deployment rolls (Helm), so match that here. Listing
	// the CRDs after apply keeps this correct across channels and Gateway API
	// versions instead of hardcoding a per-channel list.
	return waitCRDsEstablished(ctx, ".gateway.networking.k8s.io")
}

// ensureNamespaces idempotently creates the controller, shared-fixture, and
// security namespaces upfront so the parallel install/fixture phases don't
// race on namespace creation. It also installs the centrally owned WAF policy
// catalog that e2e-values.yaml references before the controller starts.
func ensureNamespaces(ctx context.Context) error {
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %s
---
apiVersion: v1
kind: Namespace
metadata:
  name: %s
---
apiVersion: v1
kind: Namespace
metadata:
  name: security
---
apiVersion: v1
kind: ConfigMap
metadata:
  namespace: security
  name: haptic-waf-policies
immutable: true
data:
  policies.yaml: |
    streaming-search:
      description: Metadata inspection with a narrow search exception and no request-body buffering
      requestBody:
        mode: none
      enforcement: deny
      ruleExclusions:
        - tags: [attack-sqli, attack-xss]
          excludeTarget: "ARGS:q"
    form-body-inspection:
      description: Form request policy with bounded complete body inspection
      requestBody:
        mode: any
      enforcement: deny
`, ControllerNamespace, SharedFixturesNamespace)
	return kubectlApplyStdin(ctx, []byte(manifest))
}

// preInstallParallel runs the helm-install prerequisites concurrently:
// chart CRDs, Gateway API CRDs, the webhook CA + server-cert Secret, and
// the default-ssl-cert Secret. They all depend only on the cluster + the
// pre-created namespace, not on each other, so fanning them out cuts the
// sequential cost of this phase.
//
// Returns the base64 CA bundle from setupWebhookCerts so the helm step
// can pass it as --set controller.webhook.caBundle=...
func preInstallParallel(ctx context.Context) (string, error) {
	g, gctx := errgroup.WithContext(ctx)
	var caBundleB64 string

	g.Go(func() error {
		_, err := installCRDs(gctx)
		return err
	})
	g.Go(func() error {
		_, err := installGatewayAPICRDs(gctx)
		return err
	})
	g.Go(func() error {
		b, err := setupWebhookCerts(gctx)
		if err != nil {
			return err
		}
		caBundleB64 = b
		return nil
	})
	g.Go(func() error {
		return setupDefaultSSLCert(gctx)
	})
	// The custom-CRD example library is enabled in the e2e install (see
	// helmInstallChart) so the reload-free suite can prove the runtime lane is
	// resource-agnostic. Its Route CRD ships with no chart schema, so install it
	// before the controller starts watching `routes` — and wait for it to be
	// Established, not merely applied: a CRD that establishes after the
	// controller's initial discovery is seen as "added" moments later and forces
	// a reinitialization (leadership flap).
	g.Go(func() error {
		if err := kubectlApplyStdin(gctx, []byte(customRouteCRD)); err != nil {
			return err
		}
		return waitCRDsEstablished(gctx, ".haptic-example.org")
	})

	if err := g.Wait(); err != nil {
		return "", err
	}
	return caBundleB64, nil
}

// customRouteCRD is the schema for the custom-crd-example library's Route kind.
// The library reads it untyped, so a permissive spec schema is enough.
const customRouteCRD = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: routes.haptic-example.org
spec:
  group: haptic-example.org
  scope: Namespaced
  names: {plural: routes, singular: route, kind: Route}
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          x-kubernetes-preserve-unknown-fields: true
`

// helmInstallChart installs the chart from charts/haptic with dev-values.yaml
// as the base, layering a controller.image.tag=test override to point at the local
// haptic:test-haproxyX.Y image. Idempotent: if the release already exists (e.g.,
// from a previous run with KEEP_CLUSTER=true), this becomes a `helm upgrade`.
//
// Implementation note: this uses the `helm` CLI rather than the helm Go
// SDK. The SDK would be the cleaner choice but its transitive imports drag
// in `k8s.io/api/scheduling/v1alpha1`, a package removed in k8s 1.32+ and
// thus incompatible with this repo's k8s.io/* v0.36 dependency. Switch to
// the SDK once an upstream helm release ships against k8s 1.36 client-go.
func helmInstallChart(ctx context.Context, caBundleB64 string) (context.Context, error) {
	chartDir, err := chartPath()
	if err != nil {
		return ctx, err
	}

	// HAPTIC_E2E_PROFILE selects which embedded values file to
	// install with. Default uses E2EValuesYAML (the e2e suite has
	// parallel-ingress-create timing requirements that diverge
	// from the dev loop — see its doc comment). The conformance
	// profile strips out the HTTP-store demo and other dev-loop
	// fixtures that don't belong in conformance runs.
	profile := os.Getenv("HAPTIC_E2E_PROFILE")
	var valuesBytes []byte
	switch profile {
	case "conformance":
		valuesBytes = devassets.ConformanceValuesYAML
		fmt.Fprintln(os.Stderr, "e2e: using conformance values profile")
	default:
		valuesBytes = devassets.E2EValuesYAML
	}
	if os.Getenv(scaleEnableEnv) == "1" {
		budget := envInt64OrDefault(scaleBudgetRSSEnv, scaleDefaultBudgetRSSBytes)
		// The GC target follows the asserted RSS budget, not the container's OOM headroom.
		valuesBytes, err = e2ecluster.WithControllerEnv(valuesBytes, "GOMEMLIMIT", strconv.FormatInt(budget*90/100, 10))
		if err != nil {
			return ctx, fmt.Errorf("configure scale memory budget: %w", err)
		}
		// Lock-wait samples for .github/scripts/profile-scale.py: 1 in 10 mutex contentions, blocking per 1 ms.
		for _, setting := range [][2]string{{"MUTEX_PROFILE_FRACTION", "10"}, {"BLOCK_PROFILE_RATE", "1000000"}} {
			if valuesBytes, err = e2ecluster.WithControllerEnv(valuesBytes, setting[0], setting[1]); err != nil {
				return ctx, fmt.Errorf("configure scale contention profiling: %w", err)
			}
		}
	}
	// Write the chosen values bytes to a temp file so helm can consume them.
	valuesFile, err := os.CreateTemp("", "haptic-e2e-values-*.yaml")
	if err != nil {
		return ctx, fmt.Errorf("create temp values file: %w", err)
	}
	defer os.Remove(valuesFile.Name())
	if _, err := valuesFile.Write(valuesBytes); err != nil {
		return ctx, fmt.Errorf("write temp values: %w", err)
	}
	if err := valuesFile.Close(); err != nil {
		return ctx, fmt.Errorf("close temp values: %w", err)
	}

	args := []string{
		"upgrade", "--install", HelmReleaseName, chartDir,
		kubeconfigFlag, kubeconfigPath,
		"--namespace", ControllerNamespace,
		"--create-namespace",
		"--wait",
		"--values", valuesFile.Name(),
		"--set", "controller.image.tag=" + ControllerImageTag,
		// haproxyVersion gates two things in the chart: the controller image
		// tag suffix (`<image.tag>-haproxy<haproxyVersion>`, e.g.
		// "test-haproxy3.0") and the haproxy:VERSION sidecar image. The
		// Makefile re-tags haptic:test → haptic:test-haproxy${HAPROXY_VERSION}
		// per matrix entry; without forcing the chart's haproxyVersion to
		// match, the chart falls back to its values.yaml default (currently
		// 3.2) and only the 3.2 matrix entry happens to find its image.
		// Every other matrix entry hits ImagePullBackOff.
		"--set", "haproxyVersion=" + ChartHAProxyVersion,
		"--set", "controller.webhook.caBundle=" + caBundleB64,
		// Enable the custom-CRD example library so ingress_reloadfree_test's
		// custom-CRD cycle can watch its Route kind. With no Route objects it
		// renders an empty map and no static lines — benign for other suites —
		// and its own validationTest passes in the load gate (test-templates.sh).
		"--set", "controller.templateLibraries.customCrdExample.enabled=true",
		// LoadBalancer for the haproxy frontend service so MetalLB
		// (installed in the install-metallb phase above) assigns a
		// real reachable IP. The Gateway API conformance suite uses
		// Gateway.status.addresses to construct test traffic URLs;
		// without a LoadBalancer IP the suite times out waiting for
		// the address to populate. Existing e2e tests still work via
		// the same Service: kind+MetalLB allocates a NodePort on
		// LoadBalancer-typed Services too, and the dind-aware
		// httpclient picks the right destination automatically.
		"--set", "haproxy.service.type=LoadBalancer",
		"--timeout", DefaultHelmInstallTimeout.String(),
	}
	gatewayAPIArgs, err := e2ecluster.GatewayAPIHelmArgs(os.Getenv("HAPTIC_E2E_GWAPI_CHANNEL"))
	if err != nil {
		return ctx, fmt.Errorf("gateway API Helm channel: %w", err)
	}
	args = append(args, gatewayAPIArgs...)
	if valuesPath := os.Getenv("HAPTIC_E2E_EXTRA_VALUES"); valuesPath != "" {
		args = append(args, "--values", valuesPath)
	}
	args = append(args, extraHelmSetArgs()...)
	identity, err := expectedControllerIdentity()
	if err != nil {
		return ctx, err
	}
	args = append(args,
		"--set-string", "controller.podSpec.podAnnotations.haproxy-haptic\\.org/source-hash="+identity.sourceHash,
		"--set-string", "controller.podSpec.podAnnotations.haproxy-haptic\\.org/e2e-rollout-id="+identity.rolloutID,
		"--set-string", "controller.podSpec.podAnnotations.haproxy-haptic\\.org/controller-binary-sha256="+identity.binarySHA256)
	args = append(args, e2eProfileHelmArgs(profile)...)
	// Churn tier (issue #64): expose the Gateway pod-port allocator's
	// assignments as `# gw-pod-port:` comment lines in the rendered config
	// so TestGatewayChurn can assert zero cross-wiring through the
	// `rendered` introspection var. The flag only gates debug-comment
	// emission (see charts/haptic/charts/gateway/15-pod-port-allocator.yaml);
	// ordinary e2e runs never set it, matching production. --set-string keeps
	// the value a YAML string — the chart snippet compares against "true".
	// The value path is controller.config.templatingSettings: the chart's
	// haproxytemplateconfig.yaml deep-copies `.Values.controller.config`
	// into the CR spec (a controller.templatingSettings sibling would be
	// silently ignored).
	if os.Getenv(churnEnableEnv) == "1" {
		args = append(args, "--set-string",
			"controller.config.templatingSettings.extraContext.dumpPodPortAllocations=true")
	}
	// Agent debug logging on request: the agent then logs a line per apply
	// with its verdict, the ops it ran and the reload it performed, which is
	// the pod-side half of diagnosing a stalled deploy (#159). Set in the
	// nightly-scale job.
	if os.Getenv("HAPTIC_E2E_AGENT_DEBUG") == "1" {
		args = append(args, "--set", "haproxy.agent.logLevel=debug")
	}
	// Scale tier: TestScale measures controller RSS against a 1 GiB budget
	// and times convergence at 800+ Ingresses. Two deviations from the
	// dev-oriented e2e profile make those measurements honest:
	//   - memory limit 2Gi: the profile's 512Mi limit would OOM-kill the
	//     controller before the RSS budget could ever be observed — the
	//     budget must be enforced by the test, not masked by the kubelet.
	//   - INFO logging: the profile's DEBUG level emits per-resource lines;
	//     at 800+ resources the log volume itself would distort the timing
	//     measurements, and production runs INFO.
	//
	// The sidecar is deliberately NOT given its own budget here: it runs on the
	// chart default, so this tier is the one place that exercises what actually
	// ships. Forcing it to 2Gi (as this did) meant no run anywhere validated the
	// shipped cap, and issue #111 sat ambiguous for a day because a green
	// nightly proved nothing about it.
	if os.Getenv(scaleEnableEnv) == "1" {
		args = append(args,
			"--set", "controller.resources.limits.memory=2Gi",
			"--set", "controller.logLevel=INFO",
			"--set", "controller.config.logging.level=INFO",
		)
	}
	if err := installControllerRelease(ctx, args); err != nil {
		return ctx, err
	}
	return ctx, nil
}

func e2eProfileHelmArgs(profile string) []string {
	switch profile {
	case "cache":
		fmt.Fprintf(os.Stderr, "e2e: cache shard — enabling Varnish with HAProxy Service port %d and kindnet policy enforcement\n", ChartHAProxyServiceHTTPPort)
		return []string{
			"--set", "cache.varnish.enabled=true",
			"--set", "cache.varnish.replicas=1",
			"--set", "cache.varnish.podDisruptionBudget.enabled=false",
			"--set", "cache.haproxy.responseTimeoutMs=500",
			"--set", fmt.Sprintf("haproxy.service.http.port=%d", ChartHAProxyServiceHTTPPort),
		}
	case "rate-limit":
		fmt.Fprintln(os.Stderr, "e2e: rate-limit shard — enabling shared rate limiting with Valkey")
		return []string{
			"--set", "rateLimit.shared.enabled=true",
			"--set", "rateLimit.shared.managedStore.enabled=true",
		}
	case "api-gateway":
		fmt.Fprintln(os.Stderr, "e2e: api-gateway shard — enabling request validation")
		return []string{"--set", "controller.config.templatingSettings.extraContext.apiGateway.requestSchemaValidation.enabled=true"}
	default:
		return nil
	}
}

// applyBackendFixtures installs the stateless backend fixtures into the
// SharedFixturesNamespace. These are deployed once per cluster and shared
// across all tests; they don't carry per-test state. The namespace itself
// is created by ensureNamespaces upstream so all five applies can fan out
// concurrently without racing on namespace creation.
func applyBackendFixtures(ctx context.Context) (context.Context, error) {
	demo, err := kindutil.SetDeploymentFixtureImage(devassets.HAProxyDemoBackendYAML, "haproxy", runtimeImages.HAProxy)
	if err != nil {
		return ctx, fmt.Errorf("selecting demo backend image: %w", err)
	}
	testBackend, err := kindutil.SetDeploymentFixtureImage(devassets.HAProxyTestBackendYAML, "haproxy", runtimeImages.HAProxy)
	if err != nil {
		return ctx, fmt.Errorf("selecting test backend image: %w", err)
	}
	fixtures := []struct {
		name string
		yaml []byte
	}{
		{"echo-server", devassets.EchoServerYAML},
		{"blocklist-server", devassets.BlocklistServerYAML},
		{"auth-server", devassets.AuthServerYAML},
		{"haproxy-demo-backend", demo},
		{"haproxy-test-backend", testBackend},
	}

	g, gctx := errgroup.WithContext(ctx)
	for _, f := range fixtures {
		g.Go(func() error {
			if err := kubectlApplyStdin(gctx, f.yaml); err != nil {
				return fmt.Errorf("apply %s: %w", f.name, err)
			}
			return nil
		})
	}
	return ctx, g.Wait()
}

// teardownCluster destroys the kind cluster unless KEEP_CLUSTER is true
// (default). In CI we also keep so after_script can collect logs.
func teardownCluster(ctx context.Context) (context.Context, error) {
	if os.Getenv("SKIP_CLUSTER_CREATE") == "true" || os.Getenv("KEEP_CLUSTER") != "false" || ownedE2ECluster == nil {
		return ctx, nil
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	return ctx, ownedE2ECluster.Close(cleanup)
}

// kubectlApplyStdin pipes a YAML manifest through `kubectl apply -f -`.
// Used for fixture YAMLs that don't need typed Go fixtures.
func kubectlApplyStdin(ctx context.Context, yaml []byte) error {
	apply := func(c context.Context) error {
		cmd := exec.CommandContext(c, "kubectl", "apply", kubeconfigFlag, kubeconfigPath, "-f", "-")
		cmd.Stdin = bytes.NewReader(yaml)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%w (output: %s)", err, out)
		}
		return nil
	}
	err := apply(ctx)
	// The webhook denies with this message while its replica reinitializes (a
	// config push; lost leadership no longer reinitializes); the denial says
	// to retry. Bounded and scoped to that one message, like NewIngress.
	if err != nil && strings.Contains(err.Error(), "retry after controller initialization") {
		_ = testutil.WaitForCondition(ctx, testutil.FastWaitConfig(), func(c context.Context) (bool, error) {
			err = apply(c)
			return err == nil || !strings.Contains(err.Error(), "retry after controller initialization"), nil
		})
	}
	return err
}

// kubectlGetSecretData returns the .data map of a Secret as a map of
// base64-encoded string values (the same encoding the apiserver wires).
// Returns an error if the Secret is missing or unreadable; the cert-
// reuse path treats any error as "regenerate from scratch", so distinct
// error types are intentionally not surfaced.
func kubectlGetSecretData(ctx context.Context, namespace, name string) (map[string]string, error) {
	cmd := exec.CommandContext(ctx, "kubectl", "get", "secret", name,
		kubeconfigFlag, kubeconfigPath, "-n", namespace, "-o", "json")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("kubectl get secret %s/%s: %w", namespace, name, err)
	}
	var secret struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(out, &secret); err != nil {
		return nil, fmt.Errorf("decode secret %s/%s: %w", namespace, name, err)
	}
	return secret.Data, nil
}

type controllerIdentity struct {
	sourceHash   string
	rolloutID    string
	binarySHA256 string
}

type controllerRuntimeIdentity struct {
	podUID       string
	containerID  string
	restartCount int32
}

func expectedControllerIdentity() (controllerIdentity, error) {
	identity := controllerIdentity{
		sourceHash:   os.Getenv("HAPTIC_EXPECTED_SOURCE_HASH"),
		rolloutID:    os.Getenv("HAPTIC_EXPECTED_CONTROLLER_ROLLOUT_ID"),
		binarySHA256: os.Getenv("HAPTIC_EXPECTED_CONTROLLER_BINARY_SHA256"),
	}
	if identity == (controllerIdentity{}) {
		return controllerIdentity{}, fmt.Errorf("controller identity is missing; run the e2e suite with make test-e2e")
	}
	if !isLowerHex(identity.sourceHash, 12) {
		return controllerIdentity{}, fmt.Errorf("HAPTIC_EXPECTED_SOURCE_HASH must be 12 lowercase hex characters, got %q", identity.sourceHash)
	}
	if !strings.HasPrefix(identity.rolloutID, "sha256:") || !isLowerHex(strings.TrimPrefix(identity.rolloutID, "sha256:"), 64) {
		return controllerIdentity{}, fmt.Errorf("HAPTIC_EXPECTED_CONTROLLER_ROLLOUT_ID must be a sha256 digest, got %q", identity.rolloutID)
	}
	if !isLowerHex(identity.binarySHA256, 64) {
		return controllerIdentity{}, fmt.Errorf("HAPTIC_EXPECTED_CONTROLLER_BINARY_SHA256 must be 64 lowercase hex characters, got %q", identity.binarySHA256)
	}
	return identity, nil
}

func isLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func verifyControllerRollout(ctx context.Context, clientset kubernetes.Interface) error {
	expected, err := expectedControllerIdentity()
	if err != nil {
		return err
	}
	deployment, err := clientset.AppsV1().Deployments(ControllerNamespace).Get(ctx, ControllerDeploymentName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read controller deployment: %w", err)
	}
	if err := verifyIdentityAnnotations("controller deployment", deployment.Spec.Template.Annotations, expected); err != nil {
		return err
	}
	rollout := exec.CommandContext(ctx, "kubectl", "rollout", "status",
		kubeconfigFlag, kubeconfigPath, "-n", ControllerNamespace,
		"deployment/"+ControllerDeploymentName, "--timeout=5m")
	if out, err := rollout.CombinedOutput(); err != nil {
		return fmt.Errorf("wait for controller rollout: %w (output: %s)", err, out)
	}

	desiredReplicas := int32(1)
	if deployment.Spec.Replicas != nil {
		desiredReplicas = *deployment.Spec.Replicas
	}
	if desiredReplicas < 1 {
		return fmt.Errorf("controller deployment has %d desired replicas; set at least one replica for e2e", desiredReplicas)
	}
	pods, err := waitForControllerIdentityPods(ctx, clientset, expected, desiredReplicas)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "e2e: verified source annotation %s on %d controller pods (rollout %s)\n",
		expected.sourceHash, len(pods), expected.rolloutID)
	return nil
}

func verifyControllerBinary(
	ctx context.Context,
	clientset kubernetes.Interface,
	measuredRuntimes map[string]controllerRuntimeIdentity,
) error {
	expected, err := expectedControllerIdentity()
	if err != nil {
		return err
	}
	deployment, err := clientset.AppsV1().Deployments(ControllerNamespace).Get(ctx, ControllerDeploymentName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read controller deployment: %w", err)
	}
	desiredReplicas := int32(1)
	if deployment.Spec.Replicas != nil {
		desiredReplicas = *deployment.Spec.Replicas
	}
	pods, err := controllerPodsForBinaryVerification(ctx, clientset, expected, desiredReplicas, measuredRuntimes)
	if err != nil {
		return err
	}
	for _, pod := range pods {
		checksum := exec.CommandContext(ctx, "kubectl", "exec",
			kubeconfigFlag, kubeconfigPath, "-n", ControllerNamespace,
			pod, "-c", "controller", "--", "sha256sum", "/usr/local/bin/haptic")
		checksumOut, err := checksum.CombinedOutput()
		if err != nil {
			return fmt.Errorf("hash controller binary in pod %s: %w (output: %s)", pod, err, checksumOut)
		}
		if fields := strings.Fields(string(checksumOut)); len(fields) != 2 || fields[0] != expected.binarySHA256 {
			return fmt.Errorf("controller pod %s binary digest is %q, expected %q", pod, strings.TrimSpace(string(checksumOut)), expected.binarySHA256)
		}
	}
	if measuredRuntimes != nil {
		current, err := readControllerRuntimeIdentities(ctx, clientset)
		if err != nil {
			return err
		}
		if err := controllerRuntimeIdentitiesEqual(measuredRuntimes, current); err != nil {
			return fmt.Errorf("measured controller changed during binary verification: %w", err)
		}
	}
	fmt.Fprintf(os.Stderr, "e2e: verified binary %s on %d controller pods\n", expected.binarySHA256, len(pods))
	return nil
}

func verifyIdentityAnnotations(owner string, annotations map[string]string, expected controllerIdentity) error {
	for key, want := range map[string]string{
		"haproxy-haptic.org/source-hash":              expected.sourceHash,
		"haproxy-haptic.org/e2e-rollout-id":           expected.rolloutID,
		"haproxy-haptic.org/controller-binary-sha256": expected.binarySHA256,
	} {
		if got := annotations[key]; got != want {
			return fmt.Errorf("%s annotation %s is %q, expected %q", owner, key, got, want)
		}
	}
	return nil
}

func controllerPodsForBinaryVerification(
	ctx context.Context, clientset kubernetes.Interface, expected controllerIdentity, desiredReplicas int32,
	measuredRuntimes map[string]controllerRuntimeIdentity,
) ([]string, error) {
	if measuredRuntimes == nil {
		return waitForControllerIdentityPods(ctx, clientset, expected, desiredReplicas)
	}
	current, err := readControllerRuntimeIdentities(ctx, clientset)
	if err != nil {
		return nil, err
	}
	if err := controllerRuntimeIdentitiesEqual(measuredRuntimes, current); err != nil {
		return nil, fmt.Errorf("measured controller changed before binary verification: %w", err)
	}
	pods := make([]string, 0, len(measuredRuntimes))
	for pod := range measuredRuntimes {
		pods = append(pods, pod)
	}
	slices.Sort(pods)
	return pods, nil
}

func readControllerRuntimeIdentities(
	ctx context.Context,
	clientset kubernetes.Interface,
) (map[string]controllerRuntimeIdentity, error) {
	podList, err := clientset.CoreV1().Pods(ControllerNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: LabelSelectorController,
	})
	if err != nil {
		return nil, fmt.Errorf("list controller pods: %w", err)
	}
	runtimes := make(map[string]controllerRuntimeIdentity, len(podList.Items))
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.DeletionTimestamp != nil {
			return nil, fmt.Errorf("controller pod %s is terminating", pod.Name)
		}
		found := false
		for index := range pod.Status.ContainerStatuses {
			status := &pod.Status.ContainerStatuses[index]
			if status.Name != "controller" {
				continue
			}
			if status.ContainerID == "" {
				return nil, fmt.Errorf("controller pod %s has no container ID", pod.Name)
			}
			runtimes[pod.Name] = controllerRuntimeIdentity{
				podUID:       string(pod.UID),
				containerID:  status.ContainerID,
				restartCount: status.RestartCount,
			}
			found = true
			break
		}
		if !found {
			return nil, fmt.Errorf("controller pod %s has no controller container status", pod.Name)
		}
	}
	return runtimes, nil
}

func controllerRuntimeIdentitiesEqual(before, after map[string]controllerRuntimeIdentity) error {
	if len(before) != len(after) {
		return fmt.Errorf("pod count changed: %d before, %d after", len(before), len(after))
	}
	for pod, want := range before {
		got, ok := after[pod]
		if !ok {
			return fmt.Errorf("pod %s was replaced", pod)
		}
		if got.podUID != want.podUID {
			return fmt.Errorf("pod %s UID changed", pod)
		}
		if got.containerID != want.containerID {
			return fmt.Errorf("pod %s container changed", pod)
		}
		if got.restartCount != want.restartCount {
			return fmt.Errorf("pod %s restart count changed", pod)
		}
	}
	return nil
}

func waitForControllerIdentityPods(
	ctx context.Context,
	clientset kubernetes.Interface,
	expected controllerIdentity,
	desiredReplicas int32,
) ([]string, error) {
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var lastMismatch error
	for {
		podList, err := clientset.CoreV1().Pods(ControllerNamespace).List(waitCtx, metav1.ListOptions{
			LabelSelector: LabelSelectorController,
		})
		if err != nil {
			lastMismatch = fmt.Errorf("list controller pods: %w", err)
		} else {
			var pods []string
			pods, lastMismatch = verifyControllerIdentityPods(podList.Items, expected, desiredReplicas)
			if lastMismatch == nil {
				return pods, nil
			}
		}

		select {
		case <-waitCtx.Done():
			return nil, fmt.Errorf("wait for controller pod replacement: %w (last state: %v)", waitCtx.Err(), lastMismatch)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func verifyControllerIdentityPods(items []corev1.Pod, expected controllerIdentity, desiredReplicas int32) ([]string, error) {
	if len(items) != int(desiredReplicas) {
		return nil, fmt.Errorf("%d controller pods found, expected %d", len(items), desiredReplicas)
	}
	pods := make([]string, 0, len(items))
	for index := range items {
		pod := &items[index]
		if pod.DeletionTimestamp != nil {
			return nil, fmt.Errorf("old controller pod %s is still terminating", pod.Name)
		}
		if pod.Status.Phase != corev1.PodRunning || !podConditionsReady(pod.Status.Conditions) {
			return nil, fmt.Errorf("controller pod %s is phase %s and not Ready", pod.Name, pod.Status.Phase)
		}
		if err := verifyIdentityAnnotations("controller pod "+pod.Name, pod.Annotations, expected); err != nil {
			return nil, err
		}
		pods = append(pods, pod.Name)
	}
	return pods, nil
}

func podConditionsReady(conditions []corev1.PodCondition) bool {
	for _, condition := range conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// chartPath returns the absolute path to charts/haptic, walking up from the
// current working directory until it finds the repo root.
func chartPath() (string, error) {
	root, err := repoRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "charts", "haptic"), nil
}

// chartCRDDir returns the absolute path to the chart's CRDs directory.
func chartCRDDir() (string, error) {
	root, err := repoRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "charts", "haptic", "crds"), nil
}

// repoRoot walks up from the current working directory until it finds a
// directory containing go.mod (the repo root).
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found walking up from cwd")
		}
		dir = parent
	}
}

// extraHelmSetArgs turns HAPTIC_E2E_EXTRA_SET into --set flags, so a local run
// can match the shape CI installs. The one that matters is CPU: with no CPU
// limit, GOMAXPROCS follows the node, and it sizes the validationTests gate's
// worker pool — a 16-core box finishes the gate ~4x faster than CI's 4-core
// runner, which is long enough to hide propagation gaps that fail there.
//
//	HAPTIC_E2E_EXTRA_SET='controller.resources.limits.cpu=4'
//
// Separate several with ';'.
func extraHelmSetArgs() []string {
	raw := strings.TrimSpace(os.Getenv("HAPTIC_E2E_EXTRA_SET"))
	if raw == "" {
		return nil
	}
	var args []string
	for _, pair := range strings.Split(raw, ";") {
		if pair = strings.TrimSpace(pair); pair != "" {
			args = append(args, "--set", pair)
		}
	}
	fmt.Fprintf(os.Stderr, "e2e: HAPTIC_E2E_EXTRA_SET applied: %v\n", args)
	return args
}
