# tests/acceptance - Acceptance Tests

Development context for end-to-end acceptance testing.

**API Documentation**: See `tests/acceptance/README.md`

## When to Work Here

Work in this directory when:

- Writing end-to-end regression tests
- Testing critical user-facing functionality
- Verifying controller lifecycle behavior
- Testing CRD/Secret reload functionality
- Validating controller internal state via debug endpoints

**DO NOT** work here for:

- Unit tests → Place in pkg/ alongside code
- Component integration tests → Use `tests/integration/`
- Quick feedback tests → Use unit tests
- Performance benchmarks → Create separate `tests/performance/`

## Package Purpose

Provides end-to-end acceptance testing infrastructure using the kubernetes-sigs/e2e-framework. Tests verify complete controller behavior including:

- Full controller deployment in Kubernetes
- ConfigMap and Secret watching
- Configuration reload on changes
- Template rendering
- Debug endpoint accessibility

Key differences from integration tests:

- **Integration tests**: Component-level with fixtures (agent, deployplan, etc.)
- **Acceptance tests**: Full controller deployment, user-facing features

## Architecture

```
E2E Framework (kubernetes-sigs/e2e-framework)
    ├── Environment Setup
    │   └── Kind Cluster Creation
    │
    ├── Feature Definition
    │   ├── Setup (create resources)
    │   ├── Assess (verify behavior)
    │   └── Teardown (cleanup)
    │
    └── Test Infrastructure
        ├── DebugClient (pod port-forward + HTTP client)
        ├── Fixtures (ConfigMap, Secret, Deployment, Services)
        └── Helpers (pod finding, waiting, endpoint access)
```

### Reaching the controller's endpoints

Two different mechanisms, because the two endpoints have different access rules:

- **Metrics** go through the Kubernetes API server proxy (`ProxyGet`), for the reasons below.
- **Debug** endpoints are **loopback-only** in the controller, so the API server proxy would arrive from the pod network and get a 403. `DebugClient` port-forwards into ready controller pods instead and rotates across them.

**Why the API server proxy for metrics?**

- Port-forwarding uses SPDY protocol which breaks under parallel test execution (EOF, connection reset errors)
- NodePort requires `extraPortMappings` in Kind configuration, which doesn't work in DinD environments
- API server proxy routes requests through the existing API server connection
- Works reliably in all environments, including Docker-in-Docker (DinD)
- Uses built-in client-go `ProxyGet` method - first-party Kubernetes API

**How it works:**

1. Tests create ClusterIP services for the debug and metrics endpoints
2. `SetupMetricsAccess()` returns a client that reads through `ProxyGet`, routed
   `client → API server → service → pod`
3. `SetupDebugClient()` returns a client that port-forwards to a ready controller pod,
   routed `client → port-forward → pod`, because `/debug/*` only answers on loopback

**Helper functions:**

- `SetupDebugClient()` - creates the debug service and returns a DebugClient that port-forwards to controller pods
- `SetupMetricsAccess()` - creates metrics service and returns MetricsClient using API proxy
- `WaitForServiceEndpoints()` - waits for service endpoints to be ready

## Key Components

### Environment Setup

Uses e2e-framework for test orchestration:

```go
// main_test.go — TestMain is the entry point; env.go holds shared helpers.
func TestMain(m *testing.M) {
    testEnv = env.NewParallel()

    // Detects DinD (CI) vs local and calls setupForDind / setupForLocal
    // which provision the Kind cluster named "haproxy-test", install CRDs,
    // and register teardown.
    if os.Getenv("SKIP_PARALLEL_RUNNER") == "true" {
        setupForCISharding()
    } else {
        setupForLocalDevelopment()
    }

    os.Exit(testEnv.Run(m))
}
```

**Features**:

- Shared environment across acceptance tests
- Automatic setup/teardown
- Kind cluster lifecycle management

### DebugClient

HTTP client for the controller's debug endpoints. Those are **loopback-only**, so it
port-forwards into ready controller pods rather than going through the API server's
service-proxy (which would arrive from the pod network and get a 403):

```go
// debug_client.go
type DebugClient struct {
    loopback  *testutil.LoopbackPodClient
    haptic    hapticclient.Interface
    namespace string
}

// Create via SetupDebugClient — port-forwards to ready controller pods and
// rotates across them.
func NewDebugClient(config *rest.Config, clientset kubernetes.Interface, namespace string, port int32) (*DebugClient, error) {

func (dc *DebugClient) GetConfig(ctx context.Context) (map[string]any, error) {
    // Port-forwards and fetches /debug/vars/config
}

func (dc *DebugClient) GetRenderedConfig(ctx context.Context) (string, error) {
    // Port-forwards and fetches /debug/vars/rendered
}

func (dc *DebugClient) WaitForConfigVersion(ctx context.Context, expectedVersion string, timeout time.Duration) error {
    // Polls until config version matches
}
```

**Purpose**: Access controller internal state without log parsing.

**Why port-forwarding here, when metrics use the API server proxy?**

- `/debug/*` is loopback-only; a request through the API server proxy arrives from the
  pod network and is refused with 403
- `LoopbackPodClient` rotates across ready controller pods, so a restarting replica
  doesn't fail the test
- Metrics carry no such restriction, so they keep the simpler `ProxyGet` path

**Why not logs?**

- Logs are brittle (format changes break tests)
- Logs don't provide structured state
- Debug endpoints are stable API
- Can query specific state (JSONPath field selection)

### Test Fixtures

Factory functions for creating test resources:

```go
// fixtures.go

// NewConfigMap creates a ConfigMap with given content (used for HTTP-store fixtures
// and similar, NOT the controller's own configuration — that lives on a CRD).
func NewConfigMap(namespace, name, configYAML string) *corev1.ConfigMap

// NewSecret creates a Secret with the HAPTIC agent credentials.
func NewSecret(namespace, name string) *corev1.Secret

// NewHAProxyTemplateConfigBuilder constructs the controller's primary CRD with
// fluent setters; NewHAProxyTemplateConfig is the simpler all-defaults variant.
func NewHAProxyTemplateConfigBuilder(namespace, name, secretName string) *HAProxyTemplateConfigBuilder
func NewHAProxyTemplateConfig(namespace, name, secretName string, leaderElection bool) *haproxyv1alpha1.HAProxyTemplateConfig

// NewControllerDeployment creates the controller Deployment. Six args including
// a serviceAccountName and explicit replica count.
func NewControllerDeployment(namespace, crdName, secretName, serviceAccountName string, debugPort, replicas int32) *appsv1.Deployment

// NewDebugService creates a ClusterIP Service for accessing the debug endpoint via API proxy
func NewDebugService(namespace, deploymentName string, debugPort int32) *corev1.Service

// NewMetricsService creates a ClusterIP Service for accessing the metrics endpoint via API proxy
func NewMetricsService(namespace, deploymentName string, metricsPort int32) *corev1.Service
```

**Predefined configs**:

- `InitialConfigYAML`: Version 1 config (maxconn 2000)
- `UpdatedConfigYAML`: Version 2 config (maxconn 4000)

## Usage Patterns

### Basic Acceptance Test

```go
package acceptance

import (
    "testing"
    "sigs.k8s.io/e2e-framework/pkg/features"
    "sigs.k8s.io/e2e-framework/pkg/types"
)

// testEnv is a package-scope env.Environment initialised in TestMain
// (env.go:119). There is no per-test Setup(t) constructor — each test calls
// testEnv.Test(t, feature) directly.
func TestMyFeature(t *testing.T) {
    testEnv.Test(t, buildMyFeature())
}

// CI runs only TestAllAcceptanceParallel: list buildMyFeature() in its
// features (parallel_test.go). scripts/check-test-inventory.sh enforces it.
func buildMyFeature() types.Feature {
    return features.New("My Feature").
        Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
            client, err := cfg.NewClient()
            require.NoError(t, err)

            // Create test resources
            htplCfg := NewHAProxyTemplateConfigBuilder(namespace, ControllerCRDName, ControllerSecretName).Build()
            err = client.Resources().Create(ctx, htplCfg)
            require.NoError(t, err)

            // ... create Secret, Deployment

            return ctx
        }).
        Assess("Feature works", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
            client, _ := cfg.NewClient()
            clientset, err := kubernetes.NewForConfig(cfg.Client().RESTConfig())
            require.NoError(t, err)

            // Setup debug and metrics access via API proxy. Both helpers need
            // the typed *kubernetes.Clientset *and* the e2e-framework
            // klient.Client because the proxy lookup goes through CoreV1().
            debugClient, err := SetupDebugClient(ctx, client, clientset, namespace, 30*time.Second)
            require.NoError(t, err)

            metricsClient, err := SetupMetricsAccess(ctx, client, clientset, namespace, 30*time.Second)
            require.NoError(t, err)

            // Wait for controller to complete startup reconciliation
            _, err = WaitForControllerReadyWithMetrics(ctx, client, namespace, metricsClient, 2*time.Minute)
            require.NoError(t, err)

            // Use debug client - uses API proxy, no Start() needed!
            config, err := debugClient.GetConfig(ctx)
            require.NoError(t, err)
            assert.NotNil(t, config)

            return ctx
        }).
        Feature()
}
```

## Common Patterns

### Waiting for Resources

```go
// Wait for pod ready
err := WaitForPodReady(ctx, client, namespace, "app=my-app", 2*time.Minute)
require.NoError(t, err)

// Find specific pod
pod, err := GetControllerPod(ctx, client, namespace)
require.NoError(t, err)
```

### Using Debug Endpoints

```go
// Setup debug client via API proxy - no Start() needed.
// The clientset is required because the helper looks up the debug Service
// through CoreV1() before constructing the proxy URL.
clientset, _ := kubernetes.NewForConfig(cfg.Client().RESTConfig())
debugClient, err := SetupDebugClient(ctx, client, clientset, namespace, 30*time.Second)
require.NoError(t, err)

// Get full config
config, err := debugClient.GetConfig(ctx)
require.NoError(t, err)

// Get specific field
version := config["version"].(string)

// Get rendered HAProxy config
rendered, err := debugClient.GetRenderedConfig(ctx)
require.NoError(t, err)
assert.Contains(t, rendered, "frontend http")

// Wait for specific version
err = debugClient.WaitForConfigVersion(ctx, "v2", 30*time.Second)
require.NoError(t, err)

// Get pipeline status (rendering, validation, deployment phases)
status, err := debugClient.GetPipelineStatus(ctx)
require.NoError(t, err)
```

### Resource Creation

```go
client, _ := cfg.NewClient()

// Create the primary HAProxyTemplateConfig CRD (the controller is CRD-driven;
// the older ConfigMap-based path no longer exists).
htplConfig := NewHAProxyTemplateConfigBuilder(namespace, ControllerCRDName, ControllerSecretName).Build()
err := client.Resources().Create(ctx, htplConfig)
require.NoError(t, err)

// Create Secret
secret := NewSecret(namespace, ControllerSecretName)
err = client.Resources().Create(ctx, secret)
require.NoError(t, err)

// Create Deployment — note the six-arg signature.
deployment := NewControllerDeployment(
    namespace,
    ControllerCRDName, ControllerSecretName, ControllerServiceAccountName,
    DebugPort, 1, // replicas
)
err = client.Resources().Create(ctx, deployment)
require.NoError(t, err)
```

## Common Pitfalls

### Not Waiting for Controller Ready

**Problem**: Test tries to access debug endpoints before controller is fully initialized.

```go
// Bad - controller might not be ready
clientset, _ := kubernetes.NewForConfig(cfg.Client().RESTConfig())
debugClient, _ := SetupDebugClient(ctx, client, clientset, namespace, 30*time.Second)
config, _ := debugClient.GetConfig(ctx)  // Might fail or return incomplete data!
```

**Solution**: Wait for controller to complete startup reconciliation using metrics.

```go
// Good - wait for controller ready using metrics
clientset, _ := kubernetes.NewForConfig(cfg.Client().RESTConfig())
metricsClient, _ := SetupMetricsAccess(ctx, client, clientset, namespace, 30*time.Second)
_, err := WaitForControllerReadyWithMetrics(ctx, client, namespace, metricsClient, 2*time.Minute)
require.NoError(t, err)

debugClient, _ := SetupDebugClient(ctx, client, clientset, namespace, 30*time.Second)
config, _ := debugClient.GetConfig(ctx)  // Works!
```

### Hand-rolling a NodePort or per-pod client

**Problem**: Old code builds its own access path instead of using the helpers.

```go
// Bad - NodePort doesn't work in DinD without extraPortMappings
debugClient := NewDebugClient(nodeHost, nodePort)  // OLD PATTERN

// Bad - pins one pod, so a restarting replica fails the test
debugClient := NewDebugClient(restConfig, pod, DebugPort)
```

Use `SetupDebugClient()` / `SetupMetricsAccess()` — they own the access path and the
pod rotation.

**Solution**: Use API proxy-based SetupDebugClient - no Start/Stop needed.

```go
// Good - API proxy is reliable in all environments
clientset, _ := kubernetes.NewForConfig(cfg.Client().RESTConfig())
debugClient, err := SetupDebugClient(ctx, client, clientset, namespace, 30*time.Second)
require.NoError(t, err)
// No Start() or Stop() needed - client uses API proxy
```

### Not Waiting for Config Reload

**Problem**: Test checks config immediately after update, sees old version.

```go
// Bad - doesn't wait for reload
client.Resources().Update(ctx, &cm)
config, _ := debugClient.GetConfig(ctx)  // Still old version!
```

**Solution**: Use WaitForConfigVersion to poll until reloaded.

```go
// Good - wait for reload
client.Resources().Update(ctx, &cm)
err := debugClient.WaitForConfigVersion(ctx, cm.ResourceVersion, 30*time.Second)
require.NoError(t, err)

config, _ := debugClient.GetConfig(ctx)  // New version!
```

### Hardcoding Configuration Values

**Problem**: Test breaks when config format changes.

```go
// Bad - hardcoded field paths
maxconn := config["config"].(map[string]any)["templates"].(map[string]any)["main"]
```

**Solution**: Use string matching or JSONPath via debug client.

```go
// Good - flexible matching
configStr := fmt.Sprint(config)
assert.Contains(t, configStr, "maxconn 2000")

// Or use rendered config
rendered, _ := debugClient.GetRenderedConfig(ctx)
assert.Contains(t, rendered, "maxconn 2000")
```

## Docker Image Requirements

**CRITICAL**: Acceptance tests require the Docker image to be tagged as `haptic:test` (NOT `:dev` or any other tag).

The test framework automatically loads this image into the kind cluster during test setup. If you make code changes, you must rebuild the image with the correct tag:

```bash
# Standard rebuild (uses Docker cache for faster builds)
make docker-build-test

# Force complete rebuild (necessary if cached layers are stale)
make docker-build-test DOCKER_BUILD_FLAGS=--no-cache
```

**When to use `--no-cache`:**

- After making code changes that aren't reflected in test behavior
- When you suspect Docker is using old cached layers
- When debugging mysterious test failures that don't match your code changes

**Common mistake**: Building with the wrong tag and wondering why tests don't use your latest code:

```bash
# WRONG - tests won't use this image
make docker-build IMAGE_TAG=dev

# CORRECT - tests will use this image
make docker-build-test
```

## Running Acceptance Tests

**CRITICAL: Always use Make commands to run acceptance tests.** Never run `go test` directly or manually build Docker images - the Makefile handles all setup correctly.

```bash
# Run all acceptance tests (recommended)
make test-acceptance

# Run acceptance tests in parallel (faster, uses shared cluster)
make test-acceptance-parallel

```

Use `TEST_RUN_PATTERN` to select acceptance tests without editing the recipe:

```bash
make test-acceptance TEST_RUN_PATTERN='^TestBasicAuthInputIsolation$'
```

To leave the shared `haptic:test` image and `haproxy-test` cluster alone, build
your own image and name your own cluster:

```bash
IMAGE_TAG=mine make docker-build
SKIP_DOCKER_BUILD=1 HAPTIC_ACCEPTANCE_IMAGE=haptic:mine HAPTIC_ACCEPTANCE_CLUSTER_NAME=haptic-mine \
  make test-acceptance TEST_RUN_PATTERN='^TestBasicAuthInputIsolation$'
```

The Make targets automatically:

1. Build the Docker image with the correct `:test` tag
2. Create/reuse the Kind cluster
3. Load the image into the cluster
4. Run the tests with proper flags

**Why not run `go test` directly?**

- The Docker image won't include your code changes
- The Kind cluster may not have the latest image loaded
- You'll waste time debugging image/cluster state issues

## Debugging Acceptance Tests

### Keep Namespace After Test (KEEP_NAMESPACE)

By default, test namespaces are deleted in the Teardown phase. To preserve namespaces for debugging (especially useful for flaky tests):

```bash
# Run test with namespace preservation
KEEP_NAMESPACE=true go test -tags=acceptance -v ./tests/acceptance -run TestDataplaneUnreachable

# After test completes (or fails), inspect the namespace
kubectl --context kind-haproxy-test get pods -n test-dp-unreach-<hash>
kubectl --context kind-haproxy-test logs -n test-dp-unreach-<hash> -l app=haptic-controller
kubectl --context kind-haproxy-test describe pod -n test-dp-unreach-<hash> <pod-name>

# Clean up manually when done
kubectl --context kind-haproxy-test delete namespace test-dp-unreach-<hash>
```

**When to use KEEP_NAMESPACE:**

- Debugging flaky tests
- Investigating controller behavior after test failure
- Inspecting controller logs when namespace is deleted before you can read them

### Inspect While Test Runs

E2E framework manages cluster lifecycle. To inspect during test execution:

```bash
# Run test
make test-acceptance

# While test is running or failed, inspect (the namespace name is the per-test
# value, not the static "haproxy-test"; use KEEP_NAMESPACE=true to keep it
# around — see "Keep Namespace After Test" above).
kubectl config use-context kind-haproxy-test
NS=$(kubectl get namespaces -o name | grep test- | head -1)
kubectl get pods -n ${NS#namespace/}
kubectl logs -n ${NS#namespace/} -l app=haptic-controller

# For ad-hoc debug access *outside* tests, port-forward works fine (the test
# framework avoids it because parallel SPDY connections are unstable, but a
# single manual session is OK):
POD=$(kubectl get pod -n ${NS#namespace/} -l app=haptic-controller -o name | head -1)
kubectl port-forward -n ${NS#namespace/} $POD 8080:8080
curl http://localhost:8080/debug/vars/config

# Note: tests create ClusterIP Services (not NodePort) and reach them via the
# API-server proxy; the older NodePort instructions in this section have been
# removed because Kind/DinD doesn't expose NodePorts without extraPortMappings.
```

### View Controller Logs

```bash
# Follow logs during test
kubectl logs -n haproxy-test haptic-xxx -f
```

### Manual Test Execution (Advanced - Avoid If Possible)

**Prefer `make test-acceptance` over manual steps.** Only use manual execution when debugging complex issues that require step-by-step control.

```bash
# Create cluster manually
kind create cluster --name haproxy-test

# Build controller image with CORRECT tag
make docker-build-test

# Load image into kind cluster
kind load docker-image haptic:test --name haproxy-test

# Run test
go test -tags=acceptance -v ./tests/acceptance

# Cleanup
kind delete cluster --name haproxy-test
```

**Troubleshooting tip**: If tests fail after code changes, ensure the image was rebuilt with `--no-cache`:

```bash
# Rebuild without cache to ensure latest code is included
make docker-build-test DOCKER_BUILD_FLAGS=--no-cache

# Load into kind cluster
kind load docker-image haptic:test --name haproxy-test

# Run test again
go test -tags=acceptance -v ./tests/acceptance
```

## Resources

- E2E Framework: <https://github.com/kubernetes-sigs/e2e-framework>
- Kind: <https://kind.sigs.k8s.io/>
- Debug endpoints: `pkg/introspection/README.md`
