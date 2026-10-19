# September 2026 performance evidence

Maintainer record, outside the published documentation. These development measurements
use different settings from a default installation; they do not establish release
capacity guarantees. Preserve source identities and fixture conditions when reusing them.

## Measure render cost by object count

A cold render rebuilds the template outputs and their dependency graph. Its cost
depends on the resource data your templates read. Measure several sizes with the
bundled chart's Ingress workload:

```bash
./scripts/test-benchmark.sh --ingress-only --steps 100,1000,5000 --iterations 3
```

The workload creates one Ingress, one Service, and two EndpointSlices per step.
Inspect the per-template timings to identify the dominant cost in configuration
and map generation. Retain the source commit, chart values, and host details
with the output.

Admission uses the render service and can reuse its warm graph. It also runs
synchronous HAProxy validation, so render-only timings aren't admission latency.
Measure the complete request against `controller.webhook.timeoutSeconds`
(10 seconds by default).

## Incremental render cost

When template snippets declare `incremental`, a reconcile render re-executes
components whose recorded inputs changed. Document assembly and changed maps
still contribute to total latency. Followers keep their own render graphs warm;
only the leader deploys.

Compare cold loading, unchanged output, route edits, route additions, and
endpoint changes with the bundled Gateway render benchmark:

```bash
HAPTIC_BENCHMARK_BARE_ENGINE=1 HAPTIC_BENCHMARK_SKIP_ORACLE=1 \
  BENCH='^BenchmarkBundledChartHTTPRouteIncrementalRenderService$' PKG=./cmd/haptic make bench
```

`HAPTIC_BENCHMARK_BARE_ENGINE=1` uses the chart's engine directly, including its
document caches. Without it, the benchmark wraps the engine to count component
executions and those caches aren't used. `HAPTIC_BENCHMARK_SKIP_ORACLE=1`
omits the additional cold render that checks incremental output after each
iteration. Leave it unset when checking correctness.

To compare a committed branch with its main-branch baseline on one host:

```bash
git fetch origin main
BENCH_BASE_REF=$(git merge-base HEAD origin/main) make bench-render-comparison
```

This requires a clean checkout and a new output directory. It measures route
additions at 1,000 and 3,000 routes, with six samples per revision and the cold
output check enabled. Commits, source archive hashes, settings, and logs go to
`build/render-comparison`; set `BENCH_COMPARISON_OUTPUT` to retain another run.
Keep other CPU and memory workloads idle. The manual `render-benchmark-comparison`
job runs the same comparison against the merge request base on one GitLab runner.

## Gateway API implementation benchmark

`make bench-gateway-api` runs the exact probe, route-change, and route-scale programs used by the [`gateway-api-bench` Part 2 report](https://github.com/howardjohn/gateway-api-bench/blob/e81292ed876472804e0a2245876a7c445ab80881/README-v2.md). The runner checks out commit `e81292ed876472804e0a2245876a7c445ab80881` by default, verifies it, and builds the upstream Go programs in a temporary directory. For that snapshot, it also verifies and installs the Gateway API `v1.4.0` experimental bundle by its exact manifest digest.

Pinned source doesn't make the resulting numbers interchangeable with the public report. The report used joined, multi-controller runs with shared status-write contention and imported results through VictoriaMetrics. This runner uses one HAPTIC target in a dedicated Kind cluster, executes each self-paced upstream program from a static sibling container, and analyzes its raw logs directly. Use the result to find material gaps; don't present it as a reproduced public score.

### Workloads and verdicts

| Scenario | Upstream workload | Additional HAPTIC checks |
|----------|-------------------|--------------------------|
| `probe` | Create 3,000 HTTPRoutes sequentially; wait for each route's first HTTP `200` | Start with a ready backend. Require one sample per route and Gateway. Unexpected HTTP responses make the result negative even if each route eventually returns `200`. |
| `routechange` | Send continuous traffic through 20 route changes, 200 ms apart | Observe the response-header marker on every Gateway and HAProxy pod while its route variant is active. After cleanup, require `404`, no marker, and baseline config and map checksums. |
| `scale` | Create 50 namespaces with 100 applications each: one Pod, Service, and HTTPRoute per application, plus 20 simulated nodes | Require current route status, deployed config and maps, and two matching runtime-map reads from every current HAProxy worker before starting the 10-minute measurement window. |

The upstream probe starts its latency timer after the Kubernetes apply call
returns. Its numbers exclude API admission and apply time. Expected `404`
responses while a new route propagates don't count as HTTP errors; other
non-`200` responses do.

Scale retains the upstream 500 ms grace period, 1 s configuration jitter, and
2 s workload jitter. During the measurement window, route identities must stay
stable, route paths and generations must advance, upstream refresh logs must
show activity, and HAPTIC must reconcile and deploy changes. These checks prove
temporal overlap between churn and controller activity; they don't map each
mutation to a deployment. A readiness deadline produces a negative result
without steady-state CPU or memory figures.

Across every scenario, error-counter increases or restarted/unhealthy supervised
children make the product result negative. Missing evidence, changed controller
identities, decreasing counters, or incomplete cleanup invalidate the run.
Cleanup must restore the workload, configuration, and map baselines.

Read `runner-summary.json`:

| Field | Meaning |
|-------|---------|
| `measured_result.pass`, `negative_scenarios` | Product quality: propagation, availability, convergence, and error outcomes |
| `harness.pass`, `harness.final_exit_code` | Evidence and cleanup validity, finalized after terminal checks |

A valid negative measurement exits `0`. A nonzero exit means the evidence or
cleanup is incomplete or invalid. **Check the measured result as well as the
process exit code.** Reload counts are diagnostic, not an availability verdict.

The runner covers these three Part 2 control-plane scenarios. It doesn't cover
attached routes, traffic throughput, ListenerSet, backend failover, or the
100-route propagation test running alongside scale.

### Resource measurements and provenance

The runner installs the pinned upstream Prometheus manifest and samples every
5 seconds. It retains values and source timestamps, requiring matching labels
and grids, advancing timestamps, and source ages no greater than 20 seconds.
Missing, duplicate, restarted, replaced, stale, or misaligned series fail analysis.

- `upstream_compatible_pod_cgroups` uses pod-root CPU and working-set series,
  excluding pause-container duplicates.
- `haptic_container_diagnostics` reports CPU, working set, and resident set for
  each real container.
- Both report time-aligned mean, p95, maximum, and last values. CPU also reports
  counter deltas, window length, and normalized cores.

Probe and route-change resource windows cover the upstream process only. Too few
fresh samples yield `analysis_status: not_gated`, `gating: false`, and `pass: null`;
malformed or stale samples still invalidate the evidence. Scale requires a
complete resource series for its proven steady-churn window.

The default profile starts from chart defaults, enables experimental Gateway API
fields to match the pinned CRDs, and removes CPU/memory limits and explicit
`GOMEMLIMIT` from measured pods. Artifacts retain effective settings, source digests, manifest digests,
image/binary/pod/container identities, child-process identities,
logs, metric responses, and terminal status. The mutable upstream backend image
is recorded by its actual runtime digest. The local SPOA bundle is checked
against `versions-spoa.env` before and after measurement.

The runner records the network daemon's (calico-node) settings and identity, and
rejects runs where the daemon restarts, becomes unready, or is replaced during a
scenario.

Before accepting artifacts, the runner scans for raw and base64 forms of live
Secret values at least eight bytes long. Only the controller-owned SSL Secret's
`path` metadata is exempt; its certificate remains covered. A match is redacted
and fails the run. An incomplete Secret inventory, scan, or redaction replaces
the artifact tree with five files recording the invalid result. Helm captures
also redact passwords, Secret data, and webhook CA bundles.

### CI smoke profile

The manual `gateway-api-benchmark-smoke` GitLab job uses the same pinned source,
Gateway API bundle, HAProxy 3.4, 5,000-route scale workload, and route-change
workload. It reduces the probe to 300 routes and sets deployment and watcher
intervals to 100 ms. Its probe, scale-startup, scale-window, and route-change
bounds are 45, 20, 10, and 10 minutes. An outer watchdog and cleanup grace leave
time for checked artifact staging within the 2-hour-45-minute job limit.

This hosted-runner smoke records `published_workload_inputs_match: false` and
`controlled_default_profile: false`. Use it to check runner integration; use the
local 3,000-route default profile for Part 2 gap measurements. CI publishes full
artifacts only after trusted Secret inventory and scan verdicts, otherwise the
validated five-file failure result or `ci-wrapper-invalid.json`. It preserves
runner failures and fails if artifact staging fails.

### Controlled comparison: 2026-09-21

Both revisions passed the 5,000-route readiness checks, ten minutes of active
churn, the 300-route probe, and 20 route changes. Startup and propagation latency
were similar in this pair. Controller memory and agent CPU increased; a single
pair doesn't establish the cause or repeatability of those differences.

| Source | Baseline | Candidate |
| --- | --- | --- |
| Product commit | [`80a69872b`](https://github.com/haproxy-haptic/HAPTIC/commit/80a69872b36c1497a69afe84f39f9b6fbe305609) | [`858e6d058`](https://github.com/haproxy-haptic/HAPTIC/commit/858e6d05833aa5956105d08acad365b9166ef728) |
| Diagnostic commit | `8235b2ac7` | `7517f2228` |

The diagnostic commits change only three harness and CI files, which are
identical across both revisions. Runtime and chart sources match their product
commits. The candidate became part of main through
[commit d20bf9dd5](https://github.com/haproxy-haptic/HAPTIC/commit/d20bf9dd5d5e10482d783d2acb3fe6f384ece643).
These measurements precede the runtime-map replay fix in
[commit 28169f723](https://github.com/haproxy-haptic/HAPTIC/commit/28169f723e3a7cfe4745f664ce658c5eaf002f8e);
they don't measure subsequent main commits or a released version.

The baseline ran first, then the candidate, on the same `AMD Ryzen 7 5700X3D`
host with 8 physical cores, 16 hardware threads, and 31 GiB RAM. Each revision used
a fresh Kind cluster, with no other Kind cluster running. Both used Kubernetes
1.37.0, HAProxy 3.4, and the pinned Gateway API 1.4.0 experimental bundle.

This is a **tuned profile**, with these differences from the default benchmark:

- The probe creates 300 routes instead of 3,000.
- The minimum deployment interval is 100 ms instead of 5 seconds. Gateway and
  HTTPRoute debounce intervals are 100 ms.
- Pre-rollout validation has an 8-CPU limit; its 1 GiB memory limit and all
  validation checks remain enabled.

As in the default benchmark, measured HAPTIC pods have no CPU or memory limits
and no explicit `GOMEMLIMIT`. This isn't the chart's default resource profile.
Scale still creates 50 namespaces with 100 routes each, proves current status
and runtime maps on every worker, then measures ten minutes of churn.

| Metric | Baseline | Candidate | Change |
| --- | ---: | ---: | ---: |
| 5,000-route verified readiness | 951.38 s | 948.10 s | -0.35% |
| Probe median after apply | 107.19 ms | 106.90 ms | -0.27% |
| Probe p95 after apply | 123.20 ms | 120.87 ms | -1.89% |
| Controller pods mean CPU | 1.197 cores | 1.178 cores | -1.62% |
| Controller pods mean resident memory | 4.49 GiB | 4.67 GiB | +3.91% |
| Controller pods p95 working set | 4.70 GiB | 5.07 GiB | +7.91% |
| Load balancer pods mean CPU | 0.193 cores | 0.204 cores | +5.96% |
| Load balancer pods p95 CPU | 0.361 cores | 0.485 cores | +34.31% |
| Load balancer pods mean resident memory | 513.96 MiB | 526.05 MiB | +2.35% |
| Agent containers mean CPU | 0.06889 cores | 0.08087 cores | +17.38% |
| Agent containers mean resident memory | 43.93 MiB | 43.50 MiB | -0.99% |
| Whole HAPTIC fleet mean CPU | 1.390 cores | 1.382 cores | -0.57% |

Resource rows sum real containers in two controller pods and two load balancer
pods, including sidecars. The agent rows isolate the two agents. Fleet totals
exclude Kubernetes, workload backends, and monitoring. Both resource windows
contain 118 aligned samples over 585 seconds after source-freshness filtering.
Percent changes use unrounded values.

The roughly 180 MiB increase in controller mean resident memory comes from the
controller containers: about 112 MiB on the leader and 68 MiB on the follower.
Validator sidecars account for less than 1 MiB of difference. The leader's sampled
maximum resident memory decreased by 27 MiB, and its final sample increased by
7 MiB. The follower's final sample increased by 83 MiB. These observations don't
identify a leak or explain retained memory; the run didn't capture heap profiles
or Go memory statistics.

The agents account for the load balancer pods' mean CPU increase: together they
use about 0.012 more cores, while HAProxy CPU stays close to the baseline.
Deployment apply counts rose from 2,162 to 2,182, so workload activity wasn't
identical. Aggregate p95 CPU doesn't identify which component caused a peak.
Attributing either resource increase to a source change requires repeated pairs
and aligned CPU and heap profiles.

Both revisions passed the supervised-child, error-counter, Secret-scan, and
cleanup checks. Route-change traffic completed 21,532 baseline requests and
20,516 candidate requests without unexpected responses or failures. These short
continuity windows don't measure sustained throughput. Passing this pair doesn't
prove zero regression across all resource metrics or establish a safe memory
limit for 5,000 routes.

The [controlled comparison record](performance-results/2026-09-21-controlled.json)
contains exact source and binary identities, settings, component measurements,
activity counts, and hashes of the retained artifacts.

### Earlier candidate results: 2026-09-20 {#candidate-results-2026-09-20}

The fresh run uses HAPTIC commit `d11b94d1b`, Kubernetes 1.33.0, HAProxy 3.4.4,
and the pinned Gateway API 1.4.0 experimental bundle. The host is a 16-thread
`AMD Ryzen 7 5700X3D` with 31 GiB RAM. Another Kind cluster shared the host, and unused
task caches were removed during probe and scale startup because disk space was low. The
runner marks this environment `fresh-coscheduled-non-comparable`; these numbers
aren't a normalized comparison with the public report or the earlier HAPTIC run.

The profile removes CPU and memory limits from measured pods and keeps HAPTIC's
default deployment and watch timings. It differs from the bounded regression
profile in [Measured startup, scale, and churn](#measured-startup-scale-and-churn).

For 3,000 sequential HTTPRoute creations, propagation after apply returned was:

| Mean | Median | p95 | p99 | Maximum | Unexpected HTTP responses |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 83 ms | 81 ms | 118 ms | 159 ms | 261 ms | 0 |

Every route produced a sample. The probe passed the resource-series, supervised
child, error-counter, and cleanup checks. Admission and apply time are excluded
from these latency figures.

During the probe's sampled 865-second window, pod-level resource totals were:

| Pods | Mean CPU cores | Mean working set | Maximum working set |
| --- | ---: | ---: | ---: |
| Two controllers, including validator sidecars | 2.30 | 3.12 GiB | 5.08 GiB |
| Two load balancers, including agent and other sidecars | 0.81 | 0.41 GiB | 0.47 GiB |

These cover route creation and cleanup, rather than a steady 3,000-route
workload. The unrestricted Go heap also differs from the bounded profile below.

The subsequent scale scenario in this earlier run is **invalid**. At a sampled
peak of 4,690 routes, the host's `earlyoom` service sent SIGTERM to a controller
because available memory fell below its threshold while swap was full. The controller had a
3,725 MiB resident set at termination and restarted; admission rejected an
in-flight route while validation was unavailable. The run never reached the
5,000-route readiness proof or steady-state window. This establishes neither
5,000-route capacity nor a product capacity ceiling.

The [upstream workload record](performance-results/2026-09-20-upstream.json)
preserves the passing probe analysis, invalid overall verdict, host interruption,
and artifact hashes.

A separate fresh cluster ran the unchanged 20-change, 200 ms route-change
scenario against the same candidate. All 20,283 requests completed without
unexpected responses or request failures. Live header observations proved the
changed route behavior reached both HAProxy pods; cleanup restored the baseline
configuration and maps. The scenario and final evidence checks passed. Its
7.2-second workload was too short for a resource-series verdict, so no CPU or
memory result is reported for it.

### Choose the appropriate performance test

| Test | Use it for | Don't use it for |
|---|---|---|
| `make bench-gateway-api` | Finding ballpark gaps against Part 2 with the pinned upstream programs and a controlled, isolated HAPTIC profile | Treating the local and published numbers as interchangeable scores, claiming full-suite coverage, or enforcing HAPTIC-specific budgets |
| `TestScale` | HAPTIC regression testing for full convergence, single-change latency at scale, controller memory, reloads, and explicit budgets | Cross-controller comparison with published `gateway-api-bench` results |
| `TestGatewayChurn` | Sustained parallel Gateway and HTTPRoute create/delete correctness, allocator isolation, oscillation bounds, and final quiescence | Published cross-controller latency, CPU, or memory comparison |

Run all three scenarios from the repository root:

```bash
make bench-gateway-api
```

Set `BENCH_SCENARIOS` to run a subset:

```bash
BENCH_SCENARIOS=routechange make bench-gateway-api
```

Set both HAPTIC timing controls to measure a tuned profile:

```bash
BENCH_DEPLOY_INTERVAL=100ms \
BENCH_WATCH_DEBOUNCE=100ms \
make bench-gateway-api
```

The artifacts record the requested, product-default, configured, and effective timing values. Any timing override makes the run a profile deviation, which is recorded in `metadata.json`.

The runner accepts these environment variables:

| Variable | Default | Effect |
|---|---|---|
| `BENCH_REF` | `e81292ed876472804e0a2245876a7c445ab80881` | Exact `gateway-api-bench` commit to check out and record |
| `BENCH_GATEWAY_API_VERSION` | `v1.4.0` | Gateway API release whose experimental CRD bundle is installed and verified |
| `BENCH_KIND_NODE_IMAGE` | Kind's bundled default | Node image for a new cluster; use a digest-pinned image to control the Kubernetes version |
| `BENCH_GATEWAY_API_CHANNEL` | `experimental` | Gateway API release channel (`experimental` or `standard`) |
| `BENCH_SCENARIOS` | `probe,scale,routechange` | Comma-separated scenario subset and execution order |
| `BENCH_OUTPUT_DIR` | `artifacts/gateway-api-bench/<YYYYMMDDtHHMMSSz>-<runner PID>` | Per-run result directory |
| `BENCH_GATEWAYS` | `haptic-bench/haptic` | Comma-separated namespace/name Gateway targets |
| `BENCH_PROBE_ROUTES` | `3000` | Sequential routes in the propagation scenario |
| `BENCH_PROBE_TIMEOUT` | `6h` | Hard timeout for the propagation program |
| `BENCH_ROUTECHANGE_ITERATIONS` | `20` | Route updates while traffic continues |
| `BENCH_ROUTECHANGE_GRACE_PERIOD` | `200ms` | Delay between route updates |
| `BENCH_ROUTECHANGE_TIMEOUT` | `10m` | Hard timeout for the route-change program |
| `BENCH_SCALE_NAMESPACES` | `50` | Namespaces in the scale workload |
| `BENCH_SCALE_ROUTES_PER_NAMESPACE` | `100` | Applications and routes per scale namespace |
| `BENCH_SCALE_DURATION` | `10m` | HAPTIC analysis duration after the scale readiness proof; accepts a positive integer followed by `s`, `m`, or `h` |
| `BENCH_SCALE_STARTUP_TIMEOUT` | `20m` | Maximum time for the scale workload to pass the HAPTIC readiness proof |
| `BENCH_DEPLOY_INTERVAL` | unset; chart default (`5s` at this commit) | Override HAPTIC's minimum structural-deployment interval |
| `BENCH_WATCH_DEBOUNCE` | unset; controller default (`100ms` at this commit) | Override the Gateway and HTTPRoute watcher debounce |
| `BENCH_KEEP_CLUSTER` | `false` | Keep a cluster that this runner created |
| `BENCH_ALLOW_DIRTY` | `false` | Allow an uncommitted HAPTIC tree and mark the result non-comparable |
| `BENCH_ALLOW_COSCHEDULED_CLUSTERS` | `false` | Allow other Kind clusters for a non-comparable smoke or debug run |
| `REUSE_CLUSTER` | `false` | Reuse an owned benchmark cluster and mark the result non-comparable |
| `BENCH_CLUSTER_NAME` | none | Existing benchmark cluster name required with `REUSE_CLUSTER=true` |
| `BENCH_DOCKER_NETWORK` | none | Existing benchmark Docker network required with `REUSE_CLUSTER=true` |
| `BENCH_CLUSTER_TOKEN` | none | Ownership token required with `REUSE_CLUSTER=true` |
| `BUILD_ONLY` | `false` | Verify the upstream checkout and build its programs without using a cluster |
| `HAPROXY_VERSION` | `3.4` from `versions.env` | HAProxy version for the fresh HAPTIC environment |

`BENCH_GATEWAY_API_CHANNEL` defaults to `experimental`; use `standard` only when intentionally measuring a different Gateway API schema profile. The Make target passes no positional arguments to the runner.

By default, the runner creates a unique `haptic-gwbench-*` Kind cluster and Docker network with an ownership token. Its kubeconfig is `/tmp/<cluster-name>.kubeconfig`, and its static workload container joins only that network. Cleanup verifies the ownership token before deleting the cluster or network. The controlled default profile rejects every other active Kind cluster, including `haptic-e2e` and `haptic-dev`, but never owns, reuses, or changes them. Set `BENCH_ALLOW_COSCHEDULED_CLUSTERS=true` only for smoke or debug runs; metadata marks their CPU, memory, and latency results non-comparable.

Set `BENCH_KEEP_CLUSTER=true` to retain a newly created benchmark cluster and its mode `0600` kubeconfig for inspection. Reuse requires `REUSE_CLUSTER=true` plus the exact cluster name, Docker network, and ownership token recorded by that run. Before replacing the retained kubeconfig, the runner generates a temporary one and uses it to verify the network, in-cluster ownership record, and HAPTIC release. Reused runs are recorded as non-comparable because they inherit cluster state.

The default clean-tree check stops uncommitted source from being mistaken for the recorded HAPTIC commit. `BENCH_ALLOW_DIRTY=true` marks the run non-comparable and retains a binary patch of tracked changes. For `untracked` files, it retains only the path list and content hashes, not their contents. Don't publish the artifacts when the tracked patch contains credentials or other private data.

`metadata.json` reports both `published_workload_inputs_match` and `controlled_default_profile`. The first means the pinned upstream commit, Gateway API bundle, Gateway target, and selected workload sizes match this wrapper's public-snapshot inputs. The second also requires a clean fresh cluster, no co-scheduling, product-default HAProxy and HAPTIC timings, and no reuse. Neither field claims that the local topology or score reproduces the joined public run.

The published Part 2 results are a reference, not a hardware-normalized score. They came from a single-node Kind cluster on a 16-core `AMD Ryzen 9 9950X` CPU with 96 GB RAM. Your CPU, memory, container runtime, co-scheduled workloads, Kubernetes version, isolated topology, and raw-log analysis affect absolute values. Retain the provenance artifacts and treat the published controller numbers as a ballpark.

Part 2's `Agentgateway` result uses `kgateway` as its control plane, so it's the relevant result in that report when looking for a HAPTIC control-plane gap. The [Part 1 result for `kgateway` v2.0.1](https://github.com/howardjohn/gateway-api-bench/blob/95b8373e4e2994c4c8c4b3119340cfa98af645fe/README.md) came from an older benchmark commit and isn't directly comparable with the default Part 2 profile.

## Measured startup, scale, and churn

These measurements use candidate commit `d11b94d1b` dated 2026-09-20, Kubernetes
1.33.0, Gateway API 1.6.2, and HAProxy 3.4.4 on a 16-thread `AMD Ryzen 7 5700X3D` with
31 GiB RAM. Each test used a fresh Kind cluster. Another Kind cluster shared the
host, so these results establish observed behavior on this machine, without a
normalized comparison to other implementations or earlier runs.

The [measurement record](performance-results/2026-09-20-candidate.json) retains
source, binary, and image digests, workload settings, resource summaries, and
collection errors. It describes a development candidate, not a released build.

### Scale and cold loading

`TestScale` created 800 Ingresses, 20 Gateways, and 20 HTTPRoutes across
20 namespaces. Both controllers had a 4-CPU limit and a 2 GiB memory limit.
The test fixture explicitly set `GOMEMLIMIT=966367641` (922 MiB), overriding
`automemlimit`. This historical result therefore doesn't measure automatic memory
sizing at a 2 GiB limit. The override is a test condition, not a deployment
recommendation. Requests were 100m CPU and 128 MiB memory; the memory request also
differs from the chart default.

| Measurement | Result |
| --- | ---: |
| Seed to complete convergence | 53.98 s |
| Single-change convergence, median / p95 of five samples | 4.12 s / 4.17 s |
| HAProxy reloads during the scale measurement | 4 |
| Cold controller rollout with all routing objects retained | 82 s |
| Largest sampled controller working set, including cold loading | 957 MiB |
| Largest controller cgroup charged-memory peak | 1,008 MiB |
| Container restarts | 0 |

The unchanged test budgets were 600 seconds for seeding, 15 seconds for change
p95, and 1 GiB for the test's working-set snapshot. All passed. The legacy
`controller_rss_bytes` output names the working set; the separate
`controller_memory_rss_bytes` field measures resident memory.

After the cold rollout, a 120-second observation kept every routing object's UID
and generation unchanged. `haptic doctor` confirmed healthy controllers and
matching deployment evidence on both agents.

| Controller | Working set, mean / maximum | Resident memory, mean / maximum |
| --- | ---: | ---: |
| Leader | 804 / 826 MiB | 725 / 766 MiB |
| Follower | 909 / 957 MiB | 764 / 845 MiB |

These are sample means and maxima, not guaranteed bounds. The observer retained
three startup collection errors: one completed hook's cgroup disappeared and two
controller metrics endpoints weren't addressable yet. The post-rollout window
had no collection errors. The cgroup peak includes cache and differs from resident memory.
The tested 2 GiB limit provided headroom; this run doesn't establish that a
1 GiB container limit is safe for the same workload.

Across 845 admission requests during the run, the combined webhook mean was
434 ms and the p95 histogram bucket upper bound was 1 second. This combines
resource types and both controllers; it isn't a per-kind latency guarantee.

### Parallel Gateway churn

`TestGatewayChurn` ran six workers for five minutes and completed 356
create/converge/delete/prune cycles. All 152 allocator observations succeeded.
Three surviving Gateways kept their marker Services unchanged throughout the
churn; their routing, final allocator state, and idle quiescence checks passed.

This run used the end-to-end fixture's 4-CPU and 1 GiB controller limits. No
container restarted. The largest sampled controller working set was 490 MiB;
the largest charged-memory peak was 571 MiB. Across 718 admission requests, the
combined mean was 99 ms and the p95 histogram bucket upper bound was 250 ms.
These resource observations include startup and cleanup, not just the five
minutes of churn.

Run the regression workloads with the repository's
[end-to-end test instructions](https://github.com/haproxy-haptic/HAPTIC/blob/main/tests/e2e/CLAUDE.md).
Select `TestScale` with `HAPTIC_E2E_SCALE=1`, or `TestGatewayChurn` with
`HAPTIC_E2E_CHURN=1`. Keep their default workload sizes and budgets when comparing
regression results.
