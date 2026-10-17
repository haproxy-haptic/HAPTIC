# tests/acceptance

End-to-end regression tests. A full controller image is built and loaded into a Kind cluster; tests exercise user-facing behaviour by creating `HAProxyTemplateConfig` CRDs, waiting for reconciliation, and asserting via the controller's debug / metrics / webhook endpoints.

Like `tests/integration`, every file is tagged `//go:build acceptance` — a plain `go test` finds nothing here. `make test-acceptance` adds `-tags=acceptance` for you, and additionally builds the controller image (`docker-build-test`, tagged `haptic:test`) and loads it into the cluster before exercising the suite.

## What's Tested

| File | Scope |
|------|-------|
| `compression_test.go` | `configPublishing.compressionThreshold` and zstd-compressed `HAProxyCfg` content |
| `error_scenarios_test.go` | Template/validation failures stay visible in status + events, do not deploy broken config |
| `http_store_test.go` | HTTP Store: templates calling `http.Fetch(...)` auto-register URLs; the controller fetches, caches, and re-renders on content change. (No top-level `spec.httpResources` field — see `pkg/controller/httpstore/README.md`.) |
| `input_isolation_test.go`, `input_isolation_cases_test.go` | Malformed basic-auth Secret across a controller restart; invalid annotations, Ingress snippets, and fetched lists; Warning events, independent updates, admission, and repair |
| `leader_election_test.go` | Two-replica failover — deleting the leader pod; `haptic_leader_election_*` metrics reflect the transition |
| `metrics_test.go` | `/metrics` exposes every name in the file's local `expectedMetrics` slice (analogous to `pkg/controller/metrics.TestMetrics_AllMetricsRegistered`) |
| `webhook_cert_rotation_test.go` | Rotating the mounted webhook TLS Secret changes the served certificate without a controller restart |
| `parallel_test.go` | `TestAllAcceptanceParallel`, CI's acceptance entry point (`make test-acceptance-parallel`): runs every listed feature on one shared cluster |

Framework-bits files (`env.go`, `fixtures.go`, `debug_client.go`, `constants.go`, `main_test.go`) are shared by every test file.

## Framework

Built on [`kubernetes-sigs/e2e-framework`](https://github.com/kubernetes-sigs/e2e-framework):

- `env.go` — shared test helpers (`GetControllerPod`, `SetupDebugClient`, `SetupMetricsAccess`, `WaitForControllerReadyWithMetrics`, and constants). The `env.Environment` is created in `TestMain` (`main_test.go`), not here.
- `fixtures.go` — factories for `HAProxyTemplateConfig`, `Secret`, Deployment, and the ClusterIP Services used by the API-proxy access pattern below.
- `debug_client.go` — typed wrapper around the controller's `/debug/vars/*` endpoints with wait helpers (`WaitForConfigVersion`, `WaitForControllerReadyWithMetrics`, `GetPipelineStatus`, …).
- `main_test.go` — `TestMain` entry point: creates the `env.NewParallel()` environment, provisions the Kind cluster (or reuses a CI-pre-created one), loads the controller image, installs CRDs, and registers teardown.

### API-server Proxy, Not Port-forward

Tests reach `/debug/vars/*` and `/metrics` via Kubernetes API-server proxy (`client-go` `ProxyGet`), not `kubectl port-forward`. The proxy path is deliberate:

- Port-forwarding uses SPDY, which fails under parallel-test load (EOF, connection reset).
- NodePort would need `extraPortMappings` in the Kind config, which doesn't work inside a Docker-in-Docker CI executor.
- API-server proxy rides the existing kubeconfig and is stable in every environment.

`SetupDebugClient` / `SetupMetricsAccess` return clients wired to use the proxy — no `Start()`/`Stop()` lifecycle. Old notes referencing `NewDebugClient(restConfig, pod, …).Start(ctx)` describe the retired port-forward pattern; don't copy it.

## Running

```bash
make test-acceptance                               # sequential, fresh cluster
make test-acceptance-parallel                      # one shared cluster, cases run in parallel
TEST_RUN_PATTERN=TestLeaderElection make test-acceptance
```

Always go through the Makefile. It rebuilds `haptic:test`, loads it into the cluster, and runs tests with the right flags. Running `go test` directly often leaves tests using a stale image — if a code change isn't reflected, rebuild with `docker build --no-cache -t haptic:test -f Dockerfile .` before re-running.

Kind context: `kind-haproxy-test` (shared with integration).

## Adding a Test

`testEnv` is a package-scope `env.Environment` initialised in `TestMain` (env.go:119). There is no per-test `Setup(t)` constructor — each test calls `testEnv.Test(t, feature)` directly. `SetupDebugClient` / `SetupMetricsAccess` need both the e2e-framework `klient.Client` and a typed `kubernetes.Interface` (they reach the controller via the API-server proxy):

```go
func TestMyFeature(t *testing.T) {
    testEnv.Test(t, buildMyFeature())
}

func buildMyFeature() types.Feature {
    return features.New("my feature").
        Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
            client, err := cfg.NewClient()
            require.NoError(t, err)

            // Create fixtures via fixtures.* helpers — they handle namespace scoping
            // and cleanup.
            return ctx
        }).
        Assess("the observable thing happens", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
            client, _ := cfg.NewClient()
            clientset, err := kubernetes.NewForConfig(cfg.Client().RESTConfig())
            require.NoError(t, err)

            metrics, err := SetupMetricsAccess(ctx, client, clientset, namespace, 30*time.Second)
            require.NoError(t, err)
            _, err = WaitForControllerReadyWithMetrics(ctx, client, namespace, metrics, 2*time.Minute)
            require.NoError(t, err)

            debug, err := SetupDebugClient(ctx, client, clientset, namespace, 30*time.Second)
            require.NoError(t, err)

            // Poll the debug endpoint with debug.WaitFor* rather than sleeping.
            return ctx
        }).
        Feature()
}
```

Keep the `WaitForControllerReady…` step — without it, a new test will race the startup reconciliation and fail intermittently.

CI runs only `TestAllAcceptanceParallel`, so add `buildMyFeature()` to its `features` list in `parallel_test.go`. `make lint` fails on a top-level test whose body isn't one `testEnv.Test(t, <feature>)` call with a listed `<feature>`.

## Debugging a Failing Test

`KEEP_NAMESPACE=true` survives the test's `Teardown` phase so the namespace is still around for inspection:

```bash
KEEP_NAMESPACE=true go test -v ./tests/acceptance -run TestDataplaneUnreachable
# then:
kubectl --context kind-haproxy-test get pods -n test-<short-hash>
kubectl --context kind-haproxy-test logs -n test-<short-hash> -l app=haptic-controller
# ...
kubectl --context kind-haproxy-test delete namespace test-<short-hash>   # cleanup when done
```

For ad-hoc debug access *outside* tests, `kubectl port-forward pod/... 8080:8080` still works — it's only the test framework that avoids it.

## Flaky-Test Policy

Flakes are bugs. `tests/CLAUDE.md` has the investigation checklist — never "retry and merge".

## See Also

- [`tests/README.md`](../README.md) — top-level test layout and Makefile targets
- `tests/acceptance/CLAUDE.md` — API-proxy design rationale, image-tag requirements, namespace-preservation debugging
- [`pkg/controller/debug`](../../pkg/controller/debug/) — source of truth for the `/debug/vars/*` shape these tests consume
- [`pkg/controller/metrics`](../../pkg/controller/metrics/) — source of truth for the `/metrics` names `metrics_test.go` asserts
- [e2e-framework](https://github.com/kubernetes-sigs/e2e-framework)
