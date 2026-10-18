# pkg/controller/metrics

Domain metrics for the HAProxy Template Ingress Controller. Two things live here:

- A `Metrics` struct that owns every controller-defined Prometheus metric (instance-based `prometheus.Registry`, not the global default).
- A `Component` event adapter that subscribes to controller events and updates metrics accordingly.

User-facing queries, alerting rules, and dashboard templates live in [`docs/site/docs/operations/monitoring.md`](../../../docs/site/docs/operations/monitoring.md). This README is the authoritative developer reference for *which* metrics exist and which component owns them.

## Complete Metric Catalogue

All names are listed exactly as exported. `metrics.go` contains the authoritative list; the `TestMetrics_AllMetricsRegistered` assertion in `metrics_test.go` covers a representative subset (21 of 52) — extend that slice when you add or rename a metric.

### Reconciliation pipeline

| Metric | Type | Labels | What it tracks |
|--------|------|--------|----------------|
| `haptic_reconciliation_total` | counter | — | Reconciliation cycles triggered |
| `haptic_render_warnings` | Gauge | `reason` | Current template warnings from the last successful reconciliation; resolved reasons and follower series are removed |
| `haptic_reconciliation_errors_total` | counter | — | Reconciliations that failed |
| `haptic_reconciliation_duration_seconds` | histogram | — | End-to-end reconciliation wall-clock |
| `haptic_reconciliation_queue_wait_seconds` | histogram | — | Time between `ReconciliationTriggeredEvent` and the pipeline actually picking it up (debounce + queue depth) |
| `haptic_render_total` | counter | `cache_state` | Reconcile renders by cache state: `cold` re-evaluated every template, `warm` reused the incremental graph, `replay` reused the previous output. Counts on every replica: followers render to keep their graph warm for a leadership change |

### Deployment

| Metric | Type | Labels | What it tracks |
|--------|------|--------|----------------|
| `haptic_deployment_total` | counter | — | Deployments dispatched to at least one HAProxy endpoint |
| `haptic_deployment_errors_total` | counter | — | Failed endpoint attempts and deployments with no converged or pending pods |
| `haptic_deployment_duration_seconds` | histogram | — | Deployment duration, aggregated across all parallel endpoint calls |
| `haptic_haproxy_reloads_total` | counter | — | HAProxy reloads triggered by deployments. A reload forks the HAProxy process; reload rate (vs runtime-API updates) is the key capacity/SLO signal |
| `haptic_deploy_apply_total` | counter | `pod`, `mode` | Applies an agent accepted, by the mode it reported: `runtime`, `file_only`, `reload`, `scheduled` or `noop`. The reload-free share of a rollout is the `runtime`+`file_only`+`noop` fraction |
| `haptic_apply_rejected_total` | counter | `pod` | Applies an agent refused. The pod keeps its previous config and its next apply carries the complete file set plus a reload |
| `haptic_agent_version_skew_total` | counter | — | Applies degraded to full state plus a reload because the pod's agent speaks another API major or lacks an op kind. Nonzero during a rolling upgrade, zero after it |

### Fleet convergence & config staleness

Leader-only gauges (only the leader deploys). Followers reset them on leadership loss, so `converged < size` is `0 < 0` (false) on followers and never false-alerts. These answer "did my change reach every pod, and for how long has it not?" — the noise-free replacement for alerting on `haptic_deployment_errors_total` now that transient deploy failures self-heal. Fed leader-only from `DeploymentCompletedEvent` (`Total`/`Succeeded`/`Failed`), reset on `LostLeadershipEvent`.

| Metric | Type | Labels | What it tracks |
|--------|------|--------|----------------|
| `haptic_haproxy_fleet_size` | gauge | — | HAProxy pods the last deployment targeted (`event.Total`) |
| `haptic_haproxy_fleet_converged` | gauge | — | HAProxy pods now at the desired config (`event.Succeeded`). Alert on `haptic_haproxy_fleet_converged < haptic_haproxy_fleet_size` |
| `haptic_last_full_sync_timestamp_seconds` | gauge | — | Unix seconds of the last full-fleet sync (`Succeeded == Total > 0`); seeded to controller start time. Staleness = `time() - <this>` |
| `haptic_deployment_consecutive_failures` | gauge | — | Consecutive deploys that did not fully converge; +1 on any `Failed > 0`, reset to 0 on a full sync |

### Runtime operations

What the controller asked the fleet to run without a reload. A change the render couldn't express as a runtime op is a reload.

| Metric | Type | Labels | What it tracks |
|--------|------|--------|----------------|
| `haptic_runtime_backend_ops_total` | counter | `op` | Backend lifecycle operations the fleet applied at runtime, by op kind |
| `haptic_runtime_server_ops_total` | counter | `op` | Server lifecycle operations the fleet applied at runtime, by op kind |
| `haptic_runtime_map_divergence_total` | counter | `map` | Runtime maps whose post-apply read-back disagreed with the desired content, forcing a reload fallback |

Each agent also exports its own view on the pod's `agent-metrics` port; [monitoring.md](../../../docs/site/docs/operations/monitoring.md) lists those and maps every metric this cutover removed to its replacement.

### Config validation

| Metric | Type | Labels | What it tracks |
|--------|------|--------|----------------|
| `haptic_validation_total` | counter | — | HAProxy's verdicts on rendered configurations: render gate checks and failed render validations (leader) |
| `haptic_validation_errors_total` | counter | — | Rendered configurations HAProxy refused or couldn't check (leader) |
| `haptic_config_validated` | gauge | `status`, `reason` | This replica's view of the `HAProxyTemplateConfig` `Validated` condition: every known status and reason is exported, the current one at `1`, all `0` before the first verdict. Carried across reinitializations |
| `haptic_config_validation_total` | counter | `result` | This replica's verdicts on its configuration, startup load gate included: `valid`, `invalid`, `inconclusive` (each retry counts). Carried across reinitializations |
| `haptic_haproxy_checks_total` | counter | `validator`, `result` | `haproxy -c` processes run on this replica |
| `haptic_haproxy_checks_reused_total` | counter | `validator`, `result` | Checks answered by an identical input set's verdict |
| `haptic_config_rejected_total` | counter | `validator` | `HAProxyTemplateConfig` loads refused by the config-validation gate, labelled by the validator that rejected it (`basic`, `template`, `jsonpath`, `validationtests`), or `coordinator` when one timed out |
| `haptic_config_pinned` | gauge | — | `1` while the render gate holds back renders HAProxy refused twice in a row. Leader-only; `0` on followers |

### Watched resources

| Metric | Type | Labels | What it tracks |
|--------|------|--------|----------------|
| `haptic_resource_count` | gauge | `type` | Current size of each watched-resource store (including `haproxy-pods`) |

### HAProxy pod discovery

| Metric | Type | Labels | What it tracks |
|--------|------|--------|----------------|
| `haptic_haproxy_pods_rejected_total` | counter | `reason` | HAProxy pods refused admission by the discovery component. Persistent non-zero growth typically means the controller cannot talk to the deployed HAProxy pods (e.g. bundled HAProxy major.minor differs from the chart's `haproxyVersion`). |

### Event bus

| Metric | Type | Labels | What it tracks |
|--------|------|--------|----------------|
| `haptic_event_subscribers` | gauge | — | Live subscribers on the `EventBus`. Drops during normal ops usually indicate a crash |
| `haptic_events_published_total` | counter | — | Total publishes |
| `haptic_events_dropped_critical_total` | counter | — | Drops where the buffered event was marked critical |
| `haptic_events_dropped_by_subscriber_total` | counter | `subscriber`, `event_type` | Drops attributed to each subscriber/event-type pair (the second label lets dashboards split by which event type the subscriber couldn't keep up with) |
| `haptic_events_dropped_observability_total` | gauge | — | Drops to the observability subscribers (commentator, debug buffer); expected to be low but non-zero on bursts |
| `haptic_component_mailbox_depth` | gauge | `component` | Events queued in a component's coalescing mailbox. Each queued event pins the render it carries, so a depth that grows under churn is memory as much as latency; the mailbox only logs from 256 |

### Webhook

| Metric | Type | Labels | What it tracks |
|--------|------|--------|----------------|
| `haptic_webhook_requests_total` | counter | `gvk`, `result` | Admission requests by kind and allow/deny/error |
| `haptic_webhook_request_duration_seconds` | histogram | — | Per-request wall-clock |
| `haptic_admission_render_slot_wait_seconds` | histogram | — | Admission render's wait for a render slot |
| `haptic_webhook_validation_total` | counter | `gvk`, `result` | Validation-only tally (no timing) — handy for ratios |

### Leader election

| Metric | Type | Labels | What it tracks |
|--------|------|--------|----------------|
| `haptic_leader_election_is_leader` | gauge | — | `1` if this replica holds the lease, else `0` |
| `haptic_leader_election_transitions_total` | counter | — | Leadership changes observed |
| `haptic_leader_election_time_as_leader_seconds_total` | counter | — | Cumulative seconds spent as leader |

### Build info

| Metric | Type | Labels | What it tracks |
|--------|------|--------|----------------|
| `haptic_build_info` | gauge (always 1) | `version`, `haproxy_version`, `go_version` | Static metadata for dashboards |

## Component

`Component` subscribes to reconciliation, deployment, validation, resource, event-bus, webhook, leader-election, and parser events, and records them inside the internal `handleEvent` dispatcher. A few callers record directly: the render paths and the render gate, and the startup load gate (`RecordLoadGate`), which runs before the event bus starts.

`haptic_config_validated` follows the events the `StatusUpdater` writes the `Validated` condition from, on every replica. A `ConfigValidatedEvent` without a candidate generation (the running config's startup announcement or a leadership replay) sets `True` only while that config is the newest one parsed and no verdict has replaced it, so a replay never hides a newer `False` or `Unknown`.

```go
import (
    "github.com/haproxy-haptic/HAPTIC/pkg/controller/metrics"
    pkgmetrics "github.com/haproxy-haptic/HAPTIC/pkg/metrics"

    "github.com/prometheus/client_golang/prometheus"
)

registry := prometheus.NewRegistry()          // instance-based, swapped per controller iteration
m := metrics.NewMetrics(registry)              // single arg — Registerer only
m.SetBuildInfo(version, haproxyVersion, goVersion) // populate the haptic_build_info gauge

comp := metrics.New(m, bus)                    // (*Metrics, *EventBus) — subscribes during construction
go comp.Start(ctx)                             // runs the event loop

// Serve on the port pkg/metrics exposes
pkgmetrics.NewServer(":9090", registry).Start(ctx)
```

The `Metrics` struct is intentionally safe to use stand-alone (CLI validation, tests) without the `Component` — call the typed update methods directly when you already have a value.

### Why an instance-based registry?

The controller's reinitialisation loop creates a fresh `EventBus` on every config change. Using the global default registry would leak prior-iteration collectors into the new one and produce duplicate-registration panics. `NewMetrics` always takes a caller-provided registry, which the controller swaps per iteration; Prometheus sees a clean slate with the same metric names each time.

Critical EventBus drop counters are the exception: the controller carries their totals into each new registry. A critical drop triggers the registry swap itself, so resetting those counters would hide the failure before the next Prometheus scrape.

### Resource-count tracking

`ResourceCount` is a gauge with a `type` label. The component seeds it from `IndexSynchronizedEvent` (absolute counts per resource type) and then applies deltas from `ResourceIndexUpdatedEvent` (created − deleted), skipping events marked `IsInitialSync`. The running totals live in a `map[string]int` on the component struct, so gauge values match reality across churn without re-listing from the API server.

## Dropping or Renaming a Metric

- The `TestMetrics_AllMetricsRegistered` assertion covers a representative subset of the exported names. Update that slice when you add, rename, or remove one — and ideally extend it so every name is guarded.
- Dashboards and alert rules in `docs/site/docs/operations/monitoring.md` reference names too; keep that file in sync or link the dashboard PR to the metric PR.

## Testing

```bash
go test ./pkg/controller/metrics/...          # unit tests
go test ./pkg/controller/metrics/... -race    # race detector
```

`component_test.go` asserts that publishing each event type produces exactly the expected metric update — no surprise side-effects, no accidental double counting.

## See Also

- [`pkg/metrics`](../../metrics/) — generic HTTP `/metrics` server + `NewCounter`/`NewHistogramWithBuckets`/`NewGauge` helpers
- [`pkg/controller/events`](../events/) — event catalogue; this package subscribes to nearly all of it
- [`docs/site/docs/operations/monitoring.md`](../../../docs/site/docs/operations/monitoring.md) — user-facing queries, alerts, dashboards

## License

Apache-2.0 — see root `LICENSE`.
