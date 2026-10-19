# tests/ - Test Organization

Development context for the test directory structure.

**API Documentation**: See `tests/README.md`

## When to Work Here

Work in this directory when:

- Writing architecture validation tests
- Organizing test infrastructure
- Adding new test types or frameworks
- Creating shared test utilities

**DO NOT** work here for:

- Unit tests → Place in same package as code (e.g., `pkg/templating/engine_scriggo_test.go`)
- Integration tests → Use `tests/integration/`
- HAPTIC agent wire-contract tests → Use `tests/agent/`
- Acceptance tests → Use `tests/acceptance/`
- Full-stack e2e tests → Use `tests/e2e/`

## Shared infrastructure policy

Use Go for new test orchestration and shared helpers. Reuse the capability
packages and embedded fixtures listed in [INFRASTRUCTURE.md](INFRASTRUCTURE.md).
Keep unit tests free of subprocesses; real process tests run through
`make test-process`. Keep each suite's distinct assertions and framework.
Acceptance uses `pkg/k8s/podclient` for in-process SPDY forwarding. Upstream
conformance retains its own transport; CLI tunnels use the shared `tests/tunnel` package.

## Directory Purpose

This directory serves as the root for all non-unit tests and contains:

- Architecture validation tests (arch-go)
- Test subdirectories for integration and acceptance tests
- Shared test infrastructure (in subdirectories)

## Directory Structure

```
tests/
├── architecture_test.go          # Architecture rule validation (arch-go)
├── kindutil/                     # Kind cluster helpers shared by integration & acceptance
├── testutil/                     # Generic helpers (fixtures, assertions) shared across suites
├── integration/                  # Integration tests (fixenv + Kind, //go:build integration)
│   ├── env.go                   # fixenv fixtures: shared cluster, HAProxy, clients
│   ├── kind_cluster.go          # Kind cluster management for the integration suite
│   ├── haproxy.go               # HAProxy deployment helpers
│   ├── testutil.go              # Suite-internal helpers
│   ├── sync_*_test.go           # 10 sync test files (auxiliary, backends, ca_file, common,
│   │                            # frontends, global_defaults, idempotency, observability,
│   │                            # sections, servers)
│   ├── auxiliaryfiles_test.go   # Auxiliary file (maps, SSL, general) sync tests
│   ├── enterprise_botmgmt_test.go # Enterprise-edition Bot Management sync tests
│   └── testdata/                # Test configuration files
├── agent/                        # HAPTIC agent wire-contract tests
│   │                            # (docker only, //go:build agentdocker)
│   ├── image.go                 # Builds the haptic binary into the HAProxy image
│   ├── docker.go                # docker CLI wrapper, dind-aware published ports
│   ├── env.go                   # The pod fixture: haproxy + agent containers, socket CLI
│   ├── config.go                # Bootstrap/rendered/broken HAProxy configs, certificates
│   ├── session.go               # Manifest and fencing bookkeeping over the client
│   └── *_test.go                # apply, runtime (maps/servers/backends), tls, recovery
├── acceptance/                   # Acceptance tests (e2e-framework)
│   ├── main_test.go             # TestMain — Kind setup/teardown
│   ├── env.go                   # E2E framework setup
│   ├── constants.go             # Shared timing/port constants
│   ├── fixtures.go              # Test resource factories
│   ├── debug_client.go          # Debug HTTP client
│   ├── parallel_test.go         # Shared-cluster parallel test driver
│   └── *_test.go                # Per-feature tests: compression, error_scenarios,
│                                # http_store, leader_election, metrics
├── e2e/                          # Full-stack e2e tests (e2e-framework, //go:build e2e)
│   ├── main_test.go             # TestMain — owns kind cluster, helm install, fixtures
│   ├── env.go                   # WaitForE2EEnvironmentReady (controller debug endpoint)
│   ├── fixtures.go              # NamespaceForTest, NewIngress, NewEchoServerBackend, ...
│   ├── gateway_fixtures.go      # NewGateway, NewHTTPRoute, NewHTTPSGateway
│   ├── haproxy_demo_backend.go  # PROXY-protocol / TLS-terminating per-test backend
│   ├── webhook_certs.go         # Self-signed CA + client/server cert generators
│   ├── cleanup.go               # DumpLogsOnFailure → debug-logs/<test-name>/
│   ├── httpclient/              # Fluent HTTP/HTTPS/mTLS client (DinD-aware)
│   └── *_test.go                # ~30 full-stack routing tests (Ingress, HTTPRoute, mTLS, …)
└── conformance/                  # Gateway API upstream conformance suite
                                  # (gated on `gateway_conformance` build tag; run via
                                  #  `make test-gateway-conformance` after `make test-e2e`)
```

## Test Types

### Architecture Tests

**File**: `architecture_test.go`

**Purpose**: Validates that the codebase follows architectural constraints defined in `arch-go.yml`.

**What it tests**:

- Package dependency rules
- No circular dependencies
- Layer separation (controller can depend on all, libraries are independent)

**Example constraints**:

```yaml
# arch-go.yml
dependencies_rules:
  - package: "pkg/core"
    should_not_depend_on:
      - "pkg/controller"
      - "pkg/dataplane"
      - "pkg/k8s"
      - "pkg/templating"

  - package: "pkg/controller"
    may_depend_on:
      - "pkg/**"  # Controller can depend on everything
```

**Running**:

```bash
go test ./tests -run TestArchitecture
```

**Output on failure**:

```
Architecture validation failed!
Dependencies rule violations:
  Rule: pkg/core should not depend on pkg/controller
    Package: pkg/core/config
      - imports pkg/controller/events (forbidden)
```

### Integration Tests

**Directory**: `tests/integration/`

**Framework**: fixenv + Kind

**Purpose**: Test against a real Kubernetes cluster and real HAProxy pods, each
with the agent as its second container. A case declares two file sets; the suite
diffs them, applies the result, and reads the pod back — the tree through
`kubectl exec`, HAProxy's runtime state through its worker stats socket.

**See**: `tests/integration/CLAUDE.md` for details

### Agent Tests

**Directory**: `tests/agent/`

**Framework**: plain `testing` + the `docker` CLI (no cluster)

**Purpose**: Drive the HAPTIC agent as a black box through `pkg/dataplane/agent/client`
against a real HAProxy in master-worker mode: apply/reload, runtime map, server,
backend and certificate ops, rollback on a refused config, fencing, and the
`general/` mount. Imports no agent package, so it tests the wire contract.

**Run**: `make test-agent-docker HAPROXY_VERSION=3.4`. The suite builds the
binary itself, or takes one from `HAPTIC_BINARY`, and skips when docker is
unreachable or the build has no `agent` subcommand.

**See**: `docs/site/docs/development/agent.md`

### Acceptance Tests

**Directory**: `tests/acceptance/`

**Framework**: kubernetes-sigs/e2e-framework + Kind

**Purpose**: End-to-end regression tests for critical user-facing functionality.

**See**: `tests/acceptance/CLAUDE.md` for details

## Test Tags

### agentdocker

Agent wire-contract tests are tagged with `//go:build agentdocker`, so only
`make test-agent-docker` compiles them.

### integration

Integration tests are tagged with `//go:build integration`:

```go
//go:build integration

package integration

func TestSyncMyFeature(t *testing.T) {
    // Integration test...
}
```

**Run integration tests only**:

```bash
go test -tags=integration ./tests/integration/...
```

**Run without integration tests** (default):

```bash
go test ./...  # Skips integration tests
```

### Why tags?

- Integration tests are slow (create Kind cluster, deploy HAProxy)
- Should not run in quick feedback loops
- Separate CI steps for unit vs integration tests
- Developers can choose when to run slow tests

## Running Tests

### All Tests (Unit + Architecture)

```bash
make test
```

This runs:

- All package unit tests
- Architecture validation test

**Does NOT run**:

- Integration tests (requires `-tags=integration`)
- Acceptance tests (separate target)

### Integration Tests Only

```bash
make test-integration
```

This runs:

- Creates Kind cluster (or reuses existing)
- Runs all integration tests in `tests/integration/`
- Keeps cluster by default for faster subsequent runs

**Environment variables**:

```bash
# Force cleanup after tests
KEEP_CLUSTER=false make test-integration

# Use specific Kind node image (default: kindest/node:v1.32.0)
KIND_NODE_IMAGE=kindest/node:v1.31.0 make test-integration
```

### Acceptance Tests Only

```bash
make test-acceptance
```

This runs:

- Creates Kind cluster
- Builds and loads controller Docker image
- Runs all acceptance tests in `tests/acceptance/`
- Each test is fully isolated (new namespace per test)

### All Tests (Including Integration and Acceptance)

There is no single `make test-all` target -- run the top-level targets in sequence when you need full coverage:

```bash
make test && make test-integration && make test-acceptance && make test-e2e
```

**Duration**: ~5-10 minutes depending on cluster state. Use `make test-acceptance-parallel` to share a single Kind cluster across acceptance test cases (~half the wall-clock time).

## Common Patterns

### Synthetic backends must not leave the cluster

A backend address that exists only so HAProxy has something to fail against
(`gateway-api-bench`'s pilot-load pods at 10.0.0.10 upward, fixtures with
RFC 5737 addresses) matches no cluster route, so every health check is a SYN
that the node NATs to the host and the host forwards to the LAN's default
gateway. At 5,000 routes that was 126,000 concurrent connections and took a
home router down. Every kind cluster creator in the repo therefore blackholes
`kindutil.SyntheticBackendRanges()` (`10.0.0.0/12` and the three RFC 5737 nets)
on every node through `kindutil.Cluster.Create`. The dev-environment wrapper
`kind_blackhole_synthetic_backends` calls the same Go implementation. Pick synthetic
addresses inside those ranges, and add any new creator to the list.

### Test clusters run Calico, not kind's bundled kindnet

kindnet enforces NetworkPolicy in userspace through nfqueue, so every new
connection to a policy-selected pod waits for kindnetd's verdict; under CI load
that verdict stalls past the 1 s connect timeout (#465). Calico's iptables
dataplane enforces policy in the kernel. Every kind cluster creator therefore
sets `disableDefaultCNI` and `podSubnet: 10.244.0.0/16` (Calico's IP pool) and
installs `tests/kindutil/calico.yaml` through `kindutil.Cluster.Create` (or
`kind_install_cni` for the dev environment). `TestHapticVarnishCache` proves a
policy-denied connection is denied.

### Measuring controller memory at scale

`TestScale` is the instrument; the `scale` CI job enforces its RSS budget at 800
Ingresses. To measure larger installs locally (#284), run it on an isolated
cluster with the workload kept:

```bash
C=haptic-gwbench-<you>
HAPTIC_E2E_CLUSTER_NAME=$C HAPTIC_E2E_KUBECONFIG_PATH=$PWD/$C.kubeconfig \
HAPTIC_E2E_EXPOSE_HOST_PORTS=false KIND_EXPERIMENTAL_DOCKER_NETWORK=$C \
HAPTIC_E2E_SCALE=1 TEST_RUN_PATTERN=TestScale SCALE_NAMESPACES=60 SCALE_INGRESSES_PER_NS=50 \
SCALE_BUDGET_SEED_SECONDS=1500 KEEP_CLUSTER=true KEEP_NAMESPACE=true \
HAPTIC_E2E_EXTRA_SET="controller.resources.limits.cpu=4" make test-e2e
```

Then, against the kept cluster:

- **Steady state:** `kubectl port-forward` the controller's port 8080 and fetch
  `/debug/pprof/heap?gc=1` (live heap) and `/debug/pprof/allocs` before and
  after a churn that creates Ingresses with new names each round.
- **Cold start:** delete one controller pod, and read the replacement's
  `memory.peak` from its cgroup on the kind node (`crictl inspect` gives the
  PID; `/proc/<pid>/cgroup` the path). A restart peaks far above steady state,
  so check `haptic_reconciliation_total` exceeds `haptic_reconciliation_errors_total`
  on port 9090: a cold render that times out under GC pressure never commits.
- The scale tier pins `GOMEMLIMIT` to 90% of the RSS budget under a 2Gi limit;
  remove it (`kubectl set env ... GOMEMLIMIT-`) to measure the automemlimit
  default a real install gets.

### Scale profiles

The CI `scale` job runs `.github/scripts/profile-scale.py` alongside the test,
and the `ci-evidence-scale` artifact keeps `.pilot/scale-profile/window-NNN/`:
30-second windows from controller start to test end. Each window holds, per
controller, `cpu.prof`, `heap.prof`, `mutex.prof`, `block.prof`, goroutines and
metrics, plus `containers-start.json`/`containers-end.json` (`crictl stats` for
every container on the node). Mutex and block profiles are cumulative; compare
windows with `go tool pprof -base window-007/<pod>/mutex.prof window-008/<pod>/mutex.prof`.
Locally, run the script beside `make test-e2e` and `touch .pilot/test-finished`
afterwards; each run replaces `.pilot/scale-profile`.

### Architecture Validation

Tests enforce clean architecture via `arch-go.yml`:

```go
func TestArchitecture(t *testing.T) {
    moduleInfo := configuration.Load("haptic")
    config, err := configuration.LoadConfig("../arch-go.yml")
    require.NoError(t, err)

    result := api.CheckArchitecture(moduleInfo, *config)

    if !result.Pass {
        // Print detailed violations
        t.Fatal("Architecture validation failed")
    }
}
```

**When it fails**:

1. Check error output for specific violation
2. Either fix the dependency (move code to correct package)
3. Or update `arch-go.yml` if rule is incorrect

### Test Organization

**Unit tests**: Same package as implementation

```
pkg/templating/
    engine.go
    engine_test.go  # Unit tests for engine.go
```

**Integration tests**: Separate directory with fixtures

```
tests/integration/
    env.go          # Shared fixtures
    sync_test.go    # Integration tests using fixtures
```

**Acceptance tests**: Separate directory with framework

```
tests/acceptance/
    main_test.go         # TestMain wires Kind setup/teardown
    env.go               # Per-test helpers (GetControllerPod, SetupDebugClient, ...)
    leader_election_test.go
    metrics_test.go
    http_store_test.go
    error_scenarios_test.go
    compression_test.go
    parallel_test.go
```

## Common Pitfalls

### Running Integration Tests Without Tag

**Problem**: Integration tests don't run.

```bash
go test ./tests/integration/...
# No tests run - all are tagged with //go:build integration
```

**Solution**: Add `-tags=integration` flag.

```bash
go test -tags=integration ./tests/integration/...
```

### Architecture Test Fails on New Dependency

**Problem**: Added new import, architecture test fails.

```
Package: pkg/core/config
  - imports pkg/controller/events (forbidden)
```

**Solution**: Either:

1. Remove the import (core shouldn't depend on controller)
2. Move event types to a shared location
3. Update `arch-go.yml` if rule is wrong

### Integration Tests Slow

**Problem**: Integration tests take 2+ minutes every run.

**Solution**: Keep cluster between runs (default behavior).

```bash
# First run: creates cluster (~2 min)
make test-integration

# Subsequent runs: reuses cluster (~30 sec)
make test-integration

# Manual cleanup when done
kind delete cluster --name=haproxy-test
```

Or set `KEEP_CLUSTER=false` to always cleanup:

```bash
KEEP_CLUSTER=false make test-integration
```

### An e2e Shard Outgrows Its Job Timeout

CI splits the e2e suite by measured cost (`scripts/shard-go-tests.py --weights
tests/e2e/shard-weights.json`): serial tests add up on a shard's wall clock, so
an unbalanced split can push one shard past the 25-minute timeout while the
others idle. When a slow test is added or a shard nears the timeout, regenerate
the weights from the `go test -v` traces of every shard of one full run:

```bash
python3 scripts/go-test-weights.py shard1.log shard2.log shard3.log > tests/e2e/shard-weights.json
```

A test without a weight, including one the measured run skipped, is treated as
serial with the median duration of the serial tests that took a second or more.

### Test Namespaces Left Behind

**Problem**: Many `test-*` namespaces accumulating.

**Solution**: Kind cluster automatically cleans up old test namespaces in background on startup. Or manually cleanup:

```bash
kubectl delete ns -l 'kubernetes.io/metadata.name~=test-'
# Or delete entire cluster
kind delete cluster --name=haproxy-test
```

## Flaky Test Policy

**Flaky tests are NEVER acceptable.** A test that sometimes passes and sometimes fails indicates a real problem that must be investigated and fixed.

### What NOT to Do

- **Never blindly retry** - Re-running a pipeline without investigation masks the problem
- **Never "merge and monitor"** - This pushes broken code to main and wastes everyone's time
- **Never ignore intermittent failures** - They will get worse, not better

### Why Flaky Tests Matter

Flaky tests indicate real bugs:

- **Timing issues** - Race conditions, missing synchronization, inadequate timeouts
- **Resource contention** - CI environment constraints, DinD limitations
- **State leakage** - Tests not properly isolated, shared state corruption
- **Infrastructure bugs** - Container startup timing, network delays

These are all **real problems** that affect production reliability.

### A Timeout States the Operation, Not the Stress

A test timeout declares how long an operation *should* take. Size it to that,
never to what you observed under load. **An operation that takes milliseconds
or a few seconds must not need tens of seconds even under the heaviest test
stress. If it does, the stress has exposed a scalability bug in the product —
fix the bug, never raise the timeout to accommodate it.**

A long timeout (30s, 60s, 90s) on a fundamentally fast operation is not
patience — it is a scalability defect the test is now hiding. Raising it turns
a red pipeline green by deleting the only signal that the product falls over
under load. That is trading validation away (RULE #2): the operator hits the
same wall in production, with no test to warn them.

When a wait "needs" to be long under the parallel-test wave, root-cause *why
the operation slowed down under concurrency* and fix that:

- **Client-side apiserver throttling** — the controller's client-go
  `RateLimiter` (once a per-clientset 50-QPS bucket) queued status writes under
  the ~70-test parallel wave, so `waitForResourceDeployed` blew a 12s budget on
  a random shard each run (#172/#173/#174). The fix was to **stop client-side
  throttling by default** in `pkg/k8s/client` (`Config.QPS <= 0` → `rest.Config.QPS = -1`,
  relying on apiserver Priority & Fairness, which returns `429`+`Retry-After`
  that client-go retries — a client-side-throttled request only *blocks*), not a
  90s wait. `reason="client-side throttling, not priority and fairness"` in the
  controller logs is the fingerprint.
- **Reload backlog on a `< 3.4` fleet** is the one legitimate case for a
  *progress-bounded* wait (stay patient only while HAProxy workers keep
  reloading, fail on a real stall) — bounded by observable progress, still not
  by a flat long deadline. See `reloadFreeReaction` in
  `tests/e2e/reloadfree_helpers_test.go`.

Distinguish the two: a *flat* long timeout is almost always the bug; a wait
bounded by an *observable progress signal* (worker restarts, processed
counters) is correct because it fails the instant progress stops.

**Never shrug off an unusually high time. Ever.** An operation that normally
takes ~4s and one run takes 9s has told you something — chase it, do not widen
the wait until it stops failing. "It's flaky, bump the timeout" and "convergence
just takes a while under load" are the same mistake wearing two hats: both
delete the signal instead of reading it. Apply a *realistic* budget — sized to
what the operation actually costs plus honest margin — and let an unusual
overshoot **fail loud** so the regression surfaces, rather than hiding inside a
generous ceiling. If a legitimately slow path exists (a `< 3.4` reload), wait on
its *completion signal* (the resource's deployed status), not on an HTTP client's
flat retry budget racing the deploy: the completion wait returns the instant the
work is done and fails cleanly if it stalls, so a genuine slowdown is reported,
not absorbed. `RunSimpleIngressTest` (`tests/e2e/feature_helpers.go`) is the
worked example — it now calls `waitForIngressDeployed` before asserting instead
of hoping the redirect goes live within the HTTP retry window.

### Investigation Steps

When a test fails intermittently:

1. **Collect evidence** - Download CI logs, artifacts, container logs
2. **Identify the failure mode** - Timeout? Wrong value? Missing resource?
3. **Reproduce locally** - Run the test in a loop, simulate CI conditions
4. **Find root cause** - Don't guess; use logging, debugging, profiling
5. **Fix properly** - Address the underlying issue, not just the symptom
6. **Verify the fix** - Run the test many times to confirm stability

### Example Investigation

```bash
# Download CI run artifacts
gh run download <run_id> --repo haproxy-haptic/HAPTIC --dir artifacts

# View job logs
gh run view --job <job_id> --repo haproxy-haptic/HAPTIC --log

# Run test in loop locally to reproduce
for i in {1..50}; do
    echo "Run $i"
    go test -tags=integration ./tests/integration -run TestFlakyTest -v || break
done
```

### Acceptable Responses to Test Failures

1. **Find and fix the bug** - The only correct response
2. **Improve test infrastructure** - Better timeouts, retries with backoff, improved isolation
3. **Skip with tracking issue** - Only if fix requires significant work; must have issue linked

**Unacceptable:**

- "Just retry it"
- "Works on my machine"
- "CI is flaky, merge anyway"
- "We'll fix it later"

### Scheduling Independence for Concurrency Tests

Concurrency tests SHALL assert convergence contracts, never exact interleavings:

- **Assert**: "ends on the latest state, then quiescent" — the outcome every
  legal schedule converges to
- **Never assert**: "exactly one dispatch", "N events, in this order" — the Go
  scheduler is free to interleave goroutines differently on every run
- **Pace on observable state** via introspection/debug helpers (queue depth,
  processed counters, drained-channel checks) — never on `time.Sleep()`

Both flaky tests of the gateway-api v1.6.0 campaign (issue #58) violated this
rule identically before being rewritten scheduling-independent:
`TestBase_MailboxNeverDropsUnderBurst`
(`pkg/controller/component/base_mailbox_test.go`) and
`TestHandleDeploymentScheduled_CoalesceDrain_LatestWins`
(`pkg/controller/deployer/handle_deployment_scheduled_coalesce_test.go`).

## Adding New Test Types

### Checklist

1. **Identify test type**: Unit, integration, or acceptance?
2. **Choose location**: Same package (unit) or tests/ subdirectory?
3. **Select framework**: Standard testing, fixenv, or e2e-framework?
4. **Add build tags**: If slow tests, add `//go:build integration`
5. **Update Makefile**: Add target if needed
6. **Document**: Update relevant CLAUDE.md and README.md

### Example: Adding Performance Tests

```bash
# Create new subdirectory
mkdir tests/performance

# Create framework setup
cat > tests/performance/env.go <<EOF
//go:build performance

package performance

import "testing"

func Setup(t *testing.T) {
    // Performance test infrastructure
}
EOF

# Create test
cat > tests/performance/render_bench_test.go <<EOF
//go:build performance

package performance

func BenchmarkTemplateRendering(b *testing.B) {
    // Benchmark template rendering
}
EOF

# Add Makefile target
echo 'test-performance: ## Run performance tests' >> Makefile
echo '\tgo test -tags=performance -bench=. ./tests/performance/...' >> Makefile
```

## Test Infrastructure

### Shared Fixtures

Integration and acceptance tests use different fixture systems:

**Integration** (fixenv):

```go
// tests/integration/env.go
func SharedCluster(env fixenv.Env) *KindCluster
func TestNamespace(env fixenv.Env) *Namespace
func TestHAProxy(env fixenv.Env) *HAProxyInstance
```

**Acceptance** (e2e-framework):

```go
// tests/acceptance/main_test.go drives the cluster lifecycle via TestMain;
// individual tests use the shared env.Environment exposed by e2e-framework.

// tests/acceptance/env.go provides the per-test helpers:
func GetControllerPod(ctx context.Context, client klient.Client, namespace string) (*corev1.Pod, error)
func SetupDebugClient(ctx context.Context, client klient.Client, clientset kubernetes.Interface, namespace string, timeout time.Duration) (*DebugClient, error)
func SetupMetricsAccess(ctx context.Context, client klient.Client, clientset kubernetes.Interface, namespace string, timeout time.Duration) (*MetricsClient, error)
```

### Test Data

Test data lives in subdirectories:

```
tests/integration/testdata/
    # One subdirectory per HAProxy concept: acls/, backends/, binds/,
    # frontends/, global/, rules/, servers/, ssl-certs/, etc.
    # See directory listing for the full set.
```

## Resources

- Architecture validation: `arch-go.yml` (project root)
- Integration tests: `tests/integration/CLAUDE.md`
- Acceptance tests: `tests/acceptance/CLAUDE.md`
- Makefile targets: `Makefile` (search for `test-*`)
