# Test infrastructure contracts

This inventory records the migration in [issue #291](https://github.com/haproxy-haptic/HAPTIC/issues/291).
The starting tree is `00a69a3120ff0437a04100bf3b5bb98fc0da2679`.
The shared Go helpers and scenarios replace the original implementations below.

## Capability ownership

| Capability | Starting implementations and consumers | Shared destination |
| --- | --- | --- |
| Kind lifecycle, kubeconfig, Docker-in-Docker networking | `scripts/lib/cluster.sh`; install, upgrade, GitOps, dev environment, CI setup; integration and acceptance cluster setup; `tests/e2e/e2ecluster` | `tests/kindutil`, with explicit cluster identity and owned kubeconfig |
| Synthetic backend isolation | `scripts/lib/cluster.sh`, `tests/kindutil/blackhole.go` | One range list in `tests/kindutil`; shell consumers call the Go entry point |
| Polling and retry decisions | `tests/testutil/wait.go`, admission shell loop, GitOps Python waits, suite-local loops | `tests/testutil`; terminal, transient, and success are separate outcomes |
| Command execution and process cleanup | Bash traps, Python subprocess calls, Go `exec.CommandContext` calls | Test-only process adapter with injectable execution and explicit environment |
| Admission readiness | `scripts/lib/admission.sh`, upgrade driver | Shared server-side create dry run; never substitute unchanged apply |
| Port forwarding and recovery | `scripts/lib/upgrade-traffic.sh`, helm-defaults script, e2e tunnel lifecycle, acceptance access helpers | Shared tunnel lifecycle, with caller-specific readiness predicates |
| HTTP and TLS transport | Upgrade shell probes, helm-defaults probes, `tests/e2e/httpclient`, acceptance clients | Shared transport; assertions stay with each scenario |
| Publication identity | GitOps Python predicates, Go suite assertions | Shared observation/transport; independent assertions retain their coverage |
| Fixtures and image provenance | `scripts/testdata/chart-upgrade`, suite fixtures, `tests/kindutil` image helpers | Embedded static fixtures or factories returning fresh objects; one owner for identical assets |

## Scenario entry points

| Entry point | Preserved interface and behavior | Required CI consumers |
| --- | --- | --- |
| `make test-chart-upgrade` | `BASELINE_CHART_VERSION`, registry baseline discovery, `--list-baselines`, `--keep`, `UPGRADE_CLUSTER_NAME`, namespace/release/image overrides, per-baseline artifacts, historical 0.1.0 values | `test-chart-upgrade`, `test-chart-upgrade-minimum-kubernetes` |
| `make test-install-without-gateway-api` | `PLAIN_CLUSTER_NAME`, `--keep`, selected HAProxy/Kind versions, absence of Gateway API CRDs, default installation and later compatibility checks | Default and minimum-Kubernetes install jobs |
| `make test-release-namespace-secrets` | `SECRETS_CLUSTER_NAME`, `--image`, `--keep`; `controller.rbac.releaseNamespaceSecretsOnly` RBAC answers, TLS from the release namespace, admission and reconcile rejection of Secrets elsewhere | Default and minimum-Kubernetes install jobs |
| `make test-helm-defaults` | `--image`, `--namespace`, `--keep-cluster`, `CLUSTER_NAME`, `NAMESPACE`, `RELEASE_NAME`, `TIMEOUT`, `KEEP_CLUSTER`, cert-manager version; exit codes 1–11 retain their named failure phases | `test-helm-defaults` |
| `make test-gitops-lifecycle` | `--provider argo\|flux`, `--image`, `--certificates external\|cert-manager`, `--cluster`, `--artifacts`, `--keep`; real GitOps controllers | Existing provider/certificate matrix |
| Acceptance and e2e suites | Existing frameworks, test names, tags, parallel runner, sharding weights, JUnit, image identity, and failure artifacts | Existing acceptance/e2e jobs and full merge train |
| Integration, agent-wire, and conformance suites | Existing test and upstream contracts; adopt applicable shared infrastructure without changing scenario assertions | Existing integration/agent/conformance jobs |

Every cluster operation receives its kubeconfig and cluster identity explicitly.
Creating a test cluster must not modify the user's active context or delete a
cluster belonging to another invocation. `KEEP_CLUSTER` preserves the owned
cluster and reports its kubeconfig. Cleanup errors must not replace an earlier
scenario failure.

Tunnel startup and process ownership use separate contexts. A successful readiness
poll cancels its startup context; the forwarder remains owned by the scenario
until cleanup. Startup failure stops the process; readiness budgets stay unchanged.

## Regression coverage that must survive

| Original regression group | Required replacement evidence |
| --- | --- |
| `test_admission_readiness.py`: ready webhook | One successful server-side create dry run establishes readiness |
| Admission routing/startup failures | Refused connections, missing service endpoints, and recognized transport timeouts recover within the original deadline |
| Admission semantic failures | Denial, authorization failure, malformed input, unknown output, and mixed terminal/transient output fail immediately |
| Admission deadline | Persistent transient failure expires; each request uses the remaining budget, capped at ten seconds |
| `test_upgrade_traffic.py`: fleet coverage | HTTP and HTTPS reach every required replica with the expected host and response identity |
| Forwarder startup/reconnect | No probes before readiness; a failed connection obtains fresh ports; persistent failure expires |
| Forwarder ownership | Every success, failure, and cancellation path joins and cleans up its processes |
| Controller diagnostics | A disappearing terminating pod is tolerated; a running controller's rejected output and a failed controller listing fail the phase |
| Released-chart compatibility | Historical container ports and baseline-specific values remain supported |
| `test_gitops_lifecycle.py`: complete publication | Pod UID, checksum, applied plan, running-worker proof, and auxiliary set agree; old-fleet traffic is insufficient |
| Incomplete GitOps publication | New plan before publication, pending reload, replaced pod, partial fleet, and stale auxiliary references remain failures |
| Certificate publication | Creation and cleanup complete; reused content is accepted; changed identity, wrong role, missing/terminating certificate, and pending cleanup are rejected |
| Retained checkpoint publication | Referenced checkpoints cover the applied plan; obsolete checkpoints and their asynchronously deleted certificate Secrets must disappear before the unchanged-state snapshot |
| GitOps observation retries | Temporary resource observation errors recover; persistent errors expire; invalid agent state is terminal |
| Upgrade rejection and repair | Every previously fingerprinted input, including template libraries, stays unchanged after rejection; ordinary Helm recovery and existing zero-restart assertions remain |
| Shell cluster image regression | Default/explicit Kind image and Docker-in-Docker behavior survive the move; synthetic ranges remain isolated |

Unit tests inject process, Kubernetes, and timing dependencies. Tests that launch
binaries use an explicit integration tag and target. Local listeners bind to
loopback. Reusing transport or fixtures must not make expected results depend on
the production function under test.

## Native-language exceptions

Go owns infrastructure orchestration and shared helpers. Bash may forward arguments
and environment to the Go entry point; migrated wrappers contain no retry,
process supervision, JSON handling, or scenario assertions.

Helm YAML tests and Scriggo fixtures retain their native formats. JavaScript tests
continue to exercise browser/playground behavior. Maintained Python tools for CI
budgeting, verification, release metadata, provenance, test inventory/sharding,
and benchmark/report analysis retain their Python unit tests. Moving those tools
requires a separate scope; this migration does not claim to remove Python from
the repository. HAPTIC's no-Lua architecture constraint also applies here.

### Retained tooling

| Tool group | Classification |
| --- | --- |
| `scripts/ci/*.py` and their Python tests | CI budget, selection, and merge/publication evidence tooling |
| Release, image provenance, inventory, and sharding scripts | Maintained developer tools; their regression tests stay with the implementation |
| Benchmark drivers and report analysis | Measurement tooling with separate provenance and scheduling contracts |
| `scripts/test-templates.sh` and Vector validation scripts | Offline controller/Helm or native VRL validation; no shared Kind lifecycle |
| `scripts/check-controller-output.sh` | Retrospective node-log diagnostic, including retired pods; distinct from the scenario's live-pod log check |
| Playground and browser tests | Native JavaScript/browser behavior |
| `scripts/start-dev-env.sh` | Interactive development workflow; synthetic backend isolation delegates to the shared Go entry point |

## Verification and selection

The upgrade pilot compares old and new drivers on separate fresh clusters with
the same immutable images and baselines, including negative controls. The old
drivers were removed after the comparisons passed. Hosted CI retains one matrix.

Complete local E2E verification runs every shard of an `e2e-hp*` job group in
`.github/workflows/ci-speed-proof.yml`, with a fresh cluster per shard.
The serial lifecycle tests can exceed one invocation's deadline; split coverage
without increasing the existing deadlines.

Changes to shared helpers and fixtures must select every affected required job.
Selection tests cover moved paths, and merge/publication verification continues
to reject missing, skipped, canceled, failed, and wrong-source evidence. Record
local elapsed time, hosted compute cost, cleanup results, and diagnostic parity
with the pull request evidence; do not infer savings from the language change.

## Coverage map

| Previous regression file | Replacement |
| --- | --- |
| `scripts/tests/test_admission_readiness.py` | `tests/admission/readiness_test.go`, `tests/testutil/poll_test.go` |
| `scripts/tests/test_upgrade_traffic.py` | `tests/traffic/pod_test.go`, `tests/tunnel/forward_test.go`, `tests/scenarios/upgrade_test.go` |
| `scripts/tests/test_gitops_lifecycle.py` | `tests/publication/observations_test.go`, `tests/scenarios/gitops_test.go` |
| `scripts/tests/test_cluster_node_image.sh` | `tests/kindutil/cluster_test.go`, `cluster_resume_test.go`, `cluster_network_test.go` |
| `scripts/tests/test_chart_upgrade_baselines.py` | `tests/scenarios/baselines_test.go` |
| E2E HTTP retry and tunnel watchdog tests | Moved intact to `tests/httpclient` and `tests/tunnel`, with explicit transport options |

Acceptance uses the shared `pkg/k8s/podclient` for in-process SPDY forwarding,
while upstream conformance owns its transport. These remain distinct from CLI
port forwarding.
Shared setup, certificates, and process supervision do not change their assertions.
The process adapter requires Unix; other operating systems return an explicit
unsupported error instead of providing incomplete child cleanup.

## Migration measurements

The upgrade pilot used baseline `0.4.0`, Kubernetes `1.37.0`, and controller image
`sha256:e8c6f40840aa93be27ffd264608d6a2dc6cd40d3e94684bcdeac6092fe414707`.
Both drivers passed installation, upgrade, rejection without mutation, and repair.
The Bash driver took 525.929 seconds and the Go driver took 517.291 seconds.
The Helm-defaults comparison took 125.899 seconds in Bash and 121.017 seconds in Go.
Each run used a fresh cluster and cleaned up its own resources. These single local
comparisons do not establish hosted CI savings.

Go command logs retain arguments, exit status, stdout, and stderr. Failure
collection adds pod descriptions, current/previous container logs, events, and
stored configuration before teardown. The initial scenario error remains in the
returned error if diagnostics or cleanup also fail.
