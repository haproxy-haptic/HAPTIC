# tests/e2e - Full-Stack End-to-End Tests

Development context for full-stack end-to-end tests.

## Scope

This suite tests the **deployed system as a whole**: the controller, the helm
chart, the template libraries, the SPOA hub and modules, external services
(auth-server, blocklist-server), and actual HTTP/TLS routing through HAProxy.

## Self-contained

The suite owns its dependencies. `make test-e2e` will:

1. Build the controller image as `haptic:test` via `docker-build-test`.
   Build the bundled non-root Varnish image; explicit image overrides are preserved.
   Load the chart-selected SPOA image into Kind for both the hub and validator
   containers, pulling it unless it exists locally (CI builds it from
   `versions-spoa.env`; see `docs/agents/test-images.md`). A conflicting
   `SPOA_TAG` is rejected before setup.
2. Tag it as `haptic:test-haproxyX.Y` and verify its source hash and binary digest.
3. Create kind cluster `haptic-e2e` if it doesn't exist (or reuse it).
4. Load `haptic:test-haproxyX.Y` into kind.
5. Apply CRDs.
6. Apply embedded backend fixtures (auth-server, blocklist-server,
   echo-server, haproxy-demo-backend, haproxy-test-backend).
7. Helm-install the chart from `charts/haptic` with `--wait` and values pointing
   at the local image.
8. Verify that every controller pod belongs to the expected rollout.
9. Wait for the controller pipeline to reach `deployment.status=succeeded`.
10. In the cache profile, wait for the Varnish StatefulSet to reach its configured
    replica count before inspecting the running processes.
11. Verify every controller pod's binary checksum, then run the tests. The scale
    test defers this checksum until after its memory and CPU samples.

Nothing outside the suite is required. `scripts/start-dev-env.sh` is the
**developer's interactive dev loop** and is not invoked by the test suite.

## Distinction from sibling suites

| Suite | Scope | Cluster | Talks to controller via |
| --- | --- | --- | --- |
| `tests/integration/` | Component config-generation logic | Per-test kind | Direct fixtures |
| `tests/acceptance/` | Controller-only behavior (debug endpoints, leader election, metrics) | `kind-haproxy-test` | API server proxy |
| **`tests/e2e/`** | **Full deployed stack — real HTTP through HAProxy** | **`kind-haptic-e2e`** | **API server proxy + NodePort** |

## Conventions

- Build tag: `e2e`. Run with `make test-e2e`; the target stamps and verifies the exact controller image and binary.
- **Per-test namespace.** Each test creates its own namespace and applies its own
  routing fixtures (Ingress, HTTPRoute, Secret, etc.); cleaned up via `t.Cleanup`.
  Stateless backends (echo-server, auth-server, blocklist-server) stay shared
  and are deployed once per cluster.
- **Condition-based waits, never sleeps.** All waits poll a real condition
  (HTTP probe, debug-endpoint state, endpoint count) under
  `testutil.WaitConfig` exponential backoff. No `time.Sleep(N)` in test code.
- **Per-test logs on failure.** A standard `t.Cleanup` gated on `t.Failed()`
  dumps controller / HAProxy / SPOA / namespace-event logs to
  `debug-logs/<test-name>/` for CI artifact upload.
- **All full-stack tests live here as Go tests** under the `e2e` build tag.

## Reading the code

- `main_test.go` — `TestMain`, fully self-contained: kind cluster, helm install,
  fixture apply, controller-ready wait.
- `constants.go` — cluster, namespace, port constants.
- `env.go` — `WaitForE2EEnvironmentReady`, debug-client construction,
  pod/log helpers.
- `fixtures.go` — namespace and routing-resource (Ingress / HTTPRoute / Secret)
  builders for per-test manifests.
- `cleanup.go` — `DumpLogsOnFailure`.
- `httpclient/` — fluent HTTP/HTTPS/mTLS client with retry/backoff and
  DinD-aware host resolution.

The embedded backend fixtures are sourced from `scripts/dev-env-assets/`
via the `devassets` Go package added there. This keeps a single source of
truth for both the test suite and `start-dev-env.sh`.

## Running

```bash
# All e2e tests
make test-e2e

# A specific test
TEST_RUN_PATTERN=TestIngressBasic make test-e2e

# Keep cluster after run for debugging (default: keep)
KEEP_CLUSTER=true make test-e2e

# Keep namespaces after failure for debugging
KEEP_NAMESPACE=true make test-e2e

```

`E2E_IMAGE_TAG=mine SKIP_DOCKER_BUILD=1` installs `haptic:mine-haproxyX.Y`,
tagged from an image you built with `IMAGE_TAG=mine make docker-build`, instead
of the shared `haptic:test`.

The defaults remain `haptic-e2e`, `/tmp/haproxy-e2e-kubeconfig`, the `kind`
Docker network, and host ports `31080`, `31443`, and `31404`. The custom-cluster
variables are an internal seam for a caller that owns and verifies the Docker
network. Use `make bench-gateway-api` for an isolated benchmark environment.
HTTP and gRPC clients use the isolated cluster's node address and service NodePorts;
they reject requests until that endpoint is configured during setup.
`HAPTIC_E2E_GWAPI_CHANNEL=experimental` installs the experimental CRDs and
enables the chart's matching experimental-field validation tests.
`KIND_NODE_IMAGE` selects the node image when creating a cluster; it does not
replace an existing cluster. Omit it to use Kind's bundled default.

The test cluster uses a five-second kubelet sync interval so certificate-rotation
tests spend less time waiting for mounted Secrets. Projection still happens
automatically through Kubernetes; the tests retain every rotation and revocation
check. See [Secret projection](https://kubernetes.io/docs/concepts/configuration/secret/#using-secrets-as-files-from-a-pod).

## CI

CI enumerates the compiled tests with `go test -list` before creating clusters.
Core HAProxy and minimum-Kubernetes profiles each run three disjoint shards
through `make test-e2e`; their union covers every listed test. Cache, rate-limit,
and API-gateway each run one focused job through `make test-e2e-profile`, using
`tests/e2e/profiles/<profile>.txt`. Keep every profile-gated test in its list,
including experimental Gateway API tests in the API-gateway profile. Preserve
the listed ordinary-routing, authentication, and WAF interaction checks.
`make test-ci-rules` checks the lists; the runner rejects stale names against the
compiled inventory before creating a cluster. Inventory and selection artifacts
are retained in `debug-logs/`, along with diagnostics on failure.

Local `make test-e2e` still runs the complete suite. To reproduce a focused job:

```bash
HAPTIC_E2E_PROFILE=cache make test-e2e-profile
HAPTIC_E2E_PROFILE=rate-limit make test-e2e-profile
HAPTIC_E2E_PROFILE=api-gateway HAPTIC_E2E_GWAPI_CHANNEL=experimental make test-e2e-profile
```

See [feature and matrix coverage](../../docs/agents/ci.md#feature-and-matrix-coverage)
for how CI runs them.

### Constrained runner startup

`HAPTIC_E2E_CONTROLLER_STARTUP=staged` installs the requested Helm values with one controller replica, then restores the rendered replica count through Kubernetes scaling. All hooks run and every replica completes its full startup validation before tests begin, within the existing five-minute installation deadline. Helm's stored Deployment manifest records the initial replica count; this option is for ephemeral E2E clusters, not upgrade testing.
