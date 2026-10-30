# Monitoring

Use Prometheus to check whether HAPTIC is applying configuration changes and
whether HAProxy is serving traffic successfully. Enable the bundled monitors
and alerts below, then add the Grafana dashboards.

<a id="overview"></a>

<a id="fleet-convergence-config-staleness"></a>

## What to monitor

Scrape both the controller and HAProxy pods:

| Source | What it tells you |
| --- | --- |
| Controller (`9090`) | Whether configuration validation, deployment, and leader election are working |
| HAProxy (`8404`) | Traffic volume, connection state, and backend health |
| HAProxy sidecars | Agent health, request metrics, and plugin results; included in the HAProxy PodMonitor |

## Enable the bundled monitoring

The chart includes controller and HAProxy monitors, alerting rules, and a Grafana
dashboard. If you use Prometheus Operator, enable them together in your Helm
values. This example assumes Prometheus selects resources labeled
`release: prometheus` and runs in namespace `monitoring`:

```yaml
controller:
  monitoring:
    serviceMonitor:
      enabled: true
      labels:
        release: prometheus
    prometheusRule:
      enabled: true
      labels:
        release: prometheus
    grafanaDashboard:
      enabled: true
  networkPolicy:
    ingress:
      monitoring:
        enabled: true
        namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: monitoring
        podSelector: {}
haproxy:
  monitoring:
    podMonitor:
      enabled: true
      labels:
        release: prometheus
```

Apply the values through your [Helm deployment](../deploying-with-helm.md).
Your Prometheus installation must select these monitor and rule labels and
watch the HAPTIC namespace. Grafana's dashboard sidecar must also watch that
namespace and the `grafana_dashboard: "1"` label. See
[dashboard setup](#dashboard-examples) if you import dashboards manually.

In Prometheus, check that the HAPTIC scrape targets are **UP**. Open the HAPTIC
dashboard in Grafana. Start with fleet convergence, rejected configurations,
HAProxy backend health, and request errors; these show whether configuration
changes and application traffic are working.

The [alert list](#shipped-alerts) explains each supplied alert. The
[metrics reference](#metrics-reference) below supports custom queries and incident
investigation. Without Prometheus Operator, use the
[manual scrape configuration](#prometheus-scrape-configuration).

## Enabling metrics

Metrics are enabled by default. The controller serves Prometheus metrics at `/metrics` on the metrics port (default `:9090`), which is separate from the debug port. With the default NetworkPolicy, also enable controller monitoring ingress; see [Networking](./networking.md#allowing-prometheus-scraping).

The chart sets the controller process, container port, Service, and monitors from
one value. To disable the metrics server, set `controller.ports.metrics: 0`:

```yaml
# values.yaml — disable the metrics server and monitoring resources
controller:
  ports:
    metrics: 0
```

`controller.ports.metrics=0` can't be combined with an enabled ServiceMonitor,
PodMonitor, or PrometheusRule because those resources would target a listener
that doesn't exist. The chart rejects that combination.

## Accessing metrics

### Prometheus scrape configuration

Add a scrape config for the controller:

```yaml
scrape_configs:
  - job_name: 'haptic'
    kubernetes_sd_configs:
      - role: pod
    relabel_configs:
      - source_labels: [__meta_kubernetes_namespace]
        regex: haptic
        action: keep
      - source_labels: [__meta_kubernetes_pod_label_app_kubernetes_io_instance]
        regex: haptic
        action: keep
      - source_labels: [__meta_kubernetes_pod_label_app_kubernetes_io_name]
        regex: haptic
        action: keep
      - source_labels: [__meta_kubernetes_pod_label_app_kubernetes_io_component]
        regex: controller
        action: keep
      - source_labels: [__meta_kubernetes_pod_container_port_number]
        regex: "9090"
        action: keep
```

### ServiceMonitor (Prometheus operator)

If using Prometheus Operator, enable the ServiceMonitor in Helm:

```yaml
# values.yaml
controller:
  monitoring:
    serviceMonitor:
      enabled: true
      interval: 30s
      labels:
        release: prometheus  # Match your Prometheus selector
```

The chart also ships a `PodMonitor` (`controller.monitoring.podMonitor.enabled`) for setups that scrape pods directly instead of via the Service — enable whichever your Prometheus setup uses.

Add custom labels, a scrape timeout, `relabelings`, or `metricRelabelings` for larger setups:

```yaml
# values.yaml
controller:
  monitoring:
    serviceMonitor:
      enabled: true
      interval: 15s
      scrapeTimeout: 10s
      labels:
        release: prometheus
        team: platform
      # Stamp a cluster label onto every scraped series
      relabelings:
        - sourceLabels: [__address__]
          targetLabel: cluster
          replacement: production
      # Drop a metric you don't want to store
      metricRelabelings:
        - sourceLabels: [__name__]
          regex: 'haptic_event_subscribers'
          action: drop
```

If a NetworkPolicy is in effect, also allow Prometheus to reach the metrics port — see [Networking](./networking.md).

### Manual access

```bash
kubectl port-forward -n haptic deployment/haptic-controller 9090:9090
```

In another terminal:

```bash
curl http://localhost:9090/metrics
```

### Other scrapers

Victoria Metrics accepts the same Prometheus scrape configuration shown above. For Datadog, configure the Datadog Agent to scrape Prometheus metrics:

```yaml
# datadog-agent values
datadog:
  prometheusScrape:
    enabled: true
    serviceEndpoints: true
```

## Metrics reference

<a id="reconciliation-metrics"></a>
<a id="deployment-metrics"></a>
<a id="fleet-convergence--config-staleness"></a>
<a id="runtime-operation-metrics"></a>
<a id="agent-metrics"></a>
<a id="where-the-old-metrics-went"></a>
<a id="validation-metrics"></a>
<a id="resource-metrics"></a>
<a id="event-metrics"></a>
<a id="leader-election-metrics"></a>
<a id="webhook-metrics"></a>
<a id="reconciliation-queue"></a>
<a id="event-bus-backpressure"></a>
<a id="build-info"></a>

Find controller and agent metric names, labels, and queries in the
[metrics reference](metrics-reference.md).

## HAProxy data-plane metrics

Controller and agent metrics describe configuration delivery. They don't measure application traffic. HAProxy itself exposes a separate Prometheus endpoint carrying the data-plane signals operators usually watch most closely: per-frontend request rates, per-backend response-code breakdowns, and session counts.

The bundled config enables HAProxy's built-in [Prometheus exporter](https://github.com/haproxy/haproxy/tree/master/addons/promex) on the status frontend (port `8404`, path `/metrics`) by default — it's served from the always-on `status-extra-100-prometheus-exporter` snippet, so no extra flag is required.

### Where to scrape

Prometheus scrapes HAProxy's exporter directly on every HAProxy pod, port `8404`, path `/metrics`. The exporter answers on the pod IP whether or not the [Vector sidecar](../operations/access-logging.md#vector-sidecar) is running — the sidecar carries its own series, the request metrics and the SPOA hub's, never HAProxy's.

Turn on the bundled `PodMonitor` for the HAProxy pod:

```yaml
haproxy:
  monitoring:
    podMonitor:
      enabled: true
```

It declares one endpoint per metrics port the pod exposes: `stats` (`8404`, `haproxy_*`), `agent-metrics` (`5557`, `haptic_agent_*`), and with the sidecar on `vector-metrics` (`9598`, `vector_*`, `spoa_*` and the request counter and duration histograms) plus `vector-sizes` (`9599`, the byte-size histograms, only while a size family is enabled). With the sidecar off and the SPOA hub on it scrapes the hub's `metrics` port directly instead. Every endpoint uses the same `interval`, `scrapeTimeout` and relabeling settings.

If you prefer a ServiceMonitor to the bundled PodMonitor, this example scrapes
HAProxy traffic metrics only. It also requires Prometheus Operator:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: haproxy
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: haptic
      app.kubernetes.io/component: loadbalancer
  endpoints:
    - port: stats   # 8404
      path: /metrics
```

Without Prometheus Operator, use a plain Prometheus job. This collects HAProxy,
agent, and Vector metrics with the default chart ports from release `haptic` in
namespace `haptic`:

```yaml
scrape_configs:
  - job_name: 'haproxy'
    kubernetes_sd_configs:
      - role: pod
    relabel_configs:
      - source_labels: [__meta_kubernetes_namespace]
        regex: haptic
        action: keep
      - source_labels: [__meta_kubernetes_pod_label_app_kubernetes_io_instance]
        regex: haptic
        action: keep
      - source_labels: [__meta_kubernetes_pod_label_app_kubernetes_io_component]
        regex: loadbalancer
        action: keep
      - source_labels: [__meta_kubernetes_pod_container_port_number]
        regex: "8404|5557|9095|9598|9599"
        action: keep
```

The chart applies the default metric exclusions when a scrape has no explicit
query parameters. See the [exporter settings](../reference.md#prometheus-exporter)
to include additional metric families.

### Key queries

HAProxy labels frontend and backend metrics with `proxy` (the section name), and server metrics with `server`:

```promql
# Request rate per frontend
sum by (proxy) (rate(haproxy_frontend_http_requests_total[5m]))

# Active sessions per backend
sum by (proxy) (haproxy_backend_current_sessions)

# Backends with no live endpoint
sum by (proxy) (haproxy_backend_active_servers) == 0
```

The full metric set is HAProxy's own, not HAPTIC's — see the [HAProxy Prometheus exporter reference](https://github.com/haproxy/haproxy/tree/master/addons/promex) for every exposed series and its labels.

To restore omitted metrics, configure the [exporter exclusions](../reference.md#prometheus-exporter).
A scraper with its own query parameters overrides those exclusions.

## Request metrics

These are the rate, errors, and duration signals derived from the access log and dimensioned by **route** rather than by request URI. They answer questions the `haproxy_*` families can't: which Ingress is slow, which path returns 502 responses, whether latency is the backend or the network.

Vector enables these metrics by default, under the `haptic_ingress_controller_*`
prefix. To reuse ingress-nginx dashboards and alerts, check the
[metric compatibility notes](../migrating.md#metrics).

### Families

| Metric | Type | Measures | Endpoint |
|--------|------|----------|----------|
| `haptic_ingress_controller_requests_total` | counter | One per logged request | `9598` |
| `haptic_ingress_controller_requests` | counter | Compatibility alias of `requests_total`, with identical labels and values | `9598` |
| `haptic_ingress_controller_request_duration_seconds` | histogram | Total active time — what the client experienced (`%Ta`) | `9598` |
| `haptic_ingress_controller_response_duration_seconds` | histogram | The whole upstream call: connect, headers, and body transfer | `9598` |
| `haptic_ingress_controller_connect_duration_seconds` | histogram | Establishing the backend connection (`%Tc`) | `9598` |
| `haptic_ingress_controller_header_duration_seconds` | histogram | Waiting for the upstream's response headers (`%Tr`) | `9598` |
| `haptic_ingress_controller_request_size` | histogram | Request body bytes from the client (`%U`) | `9599` |
| `haptic_ingress_controller_response_size` | histogram | Bytes returned to the client (`%B`) | `9599` |

Use the timers to narrow an investigation: rising connect time points toward
backend reachability or connection capacity; rising header time toward the
application; and rising total time alone toward request or response transfer.
Compare these signals with backend logs before assigning a cause.

**The upstream timers are only recorded when the phase happened.** A request HAProxy answered itself — a deny, a redirect, a 503 with no live endpoint — increments both request-counter names and `request_duration_seconds` and contributes to neither `connect_duration_seconds` nor `header_duration_seconds`. Recording a zero there would report that the backend answered instantly on a request that never reached one. Look at `term` instead.

### Request counter compatibility

Use `haptic_ingress_controller_requests_total` in new queries. The existing
`haptic_ingress_controller_requests` name remains a compatibility alias; the
`vector.requestMetrics.metrics.requests` setting controls both. Don't add their
rates: they represent the same observations.

For a fleet that includes releases exposing only the old name, select the new
counter per label set before aggregating:

```promql
sum by (namespace, ingress) (
  rate(haptic_ingress_controller_requests_total[5m])
  or rate(haptic_ingress_controller_requests[5m])
)
```

### Configuration reloads

Adding or removing log metrics keeps existing active request counters and
histogram buckets. Changing the exporter's bucket settings or restarting Vector
resets its state. Idle series can expire after one minute; the next request
starts new counters for that label set.

### Linking metrics to traces

Vector's Prometheus exporter doesn't attach trace IDs as OpenMetrics exemplars,
including when the same access log produces trace spans. Don't add trace IDs to
metric tags: each ID would create another time series. HAPTIC tracks native
Vector support in [#529](https://github.com/haproxy-haptic/HAPTIC/issues/529).

Collector span metrics can provide exemplars on separate trace-derived
metrics. They don't replace these full-request metrics: HAPTIC honors trace
sampling, so requests excluded by sampling don't contribute spans.

### Labels

Every family carries the same labels. `server` is disabled by default:

| Label | Value |
|-------|-------|
| `status` | HTTP status code, exact |
| `method` | Request method |
| `path` | The matched **route** — the path template you wrote, not the request URI |
| `namespace`, `ingress` | The routing resource that owns the route; both empty when HAProxy answered the request itself |
| `service` | The Kubernetes Service behind the chosen backend |
| `host` | Request host |
| `term` | HAProxy's 4-character termination state |
| `server` | Optional selected HAProxy server name; empty when no server was selected |
| `controller_class`, `controller_namespace`, `controller_pod` | Which HAPTIC served it |

`term` is the one label `ingress-nginx` has no equivalent of, and it's usually the fastest route from "5% of requests are failing" to a cause:

| Value | Meaning |
|-------|---------|
| `----` | Normal completion |
| `SC--` | The backend refused or failed the connection |
| `sH--` | The backend accepted the connection, then never sent response headers — a server timeout |
| `sQ--` | The request timed out waiting in the queue, before any backend was picked |
| `cD--` | The client stopped reading mid-transfer |
| `PR--` | HAProxy rejected the request itself, before routing |

The full list is in HAProxy's [session state at disconnection](https://docs.haproxy.org/3.0/configuration.html#8.5) reference.

```promql
# Error rate per Ingress
sum by (namespace, ingress) (rate(haptic_ingress_controller_requests_total{status=~"5.."}[5m]))

# p99 latency per route
histogram_quantile(0.99, sum by (le, namespace, ingress, path) (
  rate(haptic_ingress_controller_request_duration_seconds_bucket[5m])))

# Is it the backend, or the app? Compare connect against header time.
histogram_quantile(0.95, sum by (le) (rate(haptic_ingress_controller_connect_duration_seconds_bucket[5m])))
histogram_quantile(0.95, sum by (le) (rate(haptic_ingress_controller_header_duration_seconds_bucket[5m])))

# Backends timing out or refusing connections
sum by (namespace, ingress, service, term) (
  rate(haptic_ingress_controller_requests_total{term=~"sH..|SC..|sQ.."}[5m])) > 0

# Bandwidth per Ingress
sum by (namespace, ingress) (rate(haptic_ingress_controller_response_size_sum[5m]))
```

### Finding errors on one upstream target

Enable the `server` label to compare targets behind the same route:

```yaml
vector:
  requestMetrics:
    serverLabel: true
```

The value is HAProxy's selected server name (`%s`), which also appears in
`haproxy_server_*{server=...}`. HAPTIC normally derives it from the endpoint's Pod
name, but prefixes, rewritten characters, and endpoints without Pod references can
change it. Treat it as a HAProxy server identifier, not a Kubernetes Pod name.
Names are scoped to a backend; `server` alone isn't a unique key for joining
native exporter series. Requests with no selected server, including HAProxy's
`<NOSRV>` marker, use an empty value.

```promql
sum by (namespace, ingress, service, path, server) (
  rate(haptic_ingress_controller_requests_total{status=~"5..",server!=""}[5m])
)
```

The label applies to both request-counter names and all duration and size
histograms. Each populated target adds series for every existing route and label
combination, multiplied by histogram buckets. It uses the same cardinality limit
as the other request labels.

### Controlling cardinality

Every combination of label values creates a time series. Histograms add a
series for each bucket. Reduce storage and processing costs by removing labels
or metric families you don't use:

```yaml
vector:
  requestMetrics:
    # Each removes a label from ALL families, so the remaining series aggregate
    # exactly as they would have without it.
    terminationStateLabel: false   # `term` — the biggest saving, it multiplies the histograms too
    pathLabel: false               # also switches off the HAProxy-side route lookup, saving per-request work
    hostLabel: false               # the equivalent of ingress-nginx's --metrics-per-host

    # Or drop whole families. The four durations are independent of each other.
    metrics:
      connect_duration_seconds: false
      header_duration_seconds: false
      request_size: false
      response_size: false
```

Bucket boundaries are the other multiplier — `durationBuckets` and `sizeBuckets` in [Chart values reference](../reference.md#vector-sidecar).

`requestMetrics.cardinalityLimit` limits each label to 500 distinct values per
metric by default. Beyond that, new series omit the label and aggregate together.
Request totals remain, but that label's detail is lost. The limit resets when
Vector restarts; remove unbounded labels if it repeatedly triggers.

!!! warning "The access log is lossy under back-pressure"
    These metrics are counted from access-log records, not in the data path, so they report fewer requests than were served whenever HAProxy drops records — see [The access log is lossy under back-pressure](../operations/access-logging.md#the-access-log-is-lossy-under-back-pressure). Keep the `HAProxyAccessLogRecordsDropped` alert on. If you need a request count that stays exact through a drop, set `extraContext.prometheusExporter.excludeMetrics.httpRequestCounters.enabled: false` to keep HAProxy's own counters alongside these.

A route that receives no requests for over a minute drops out of the exposition and its counter restarts from zero when traffic returns. `rate()` and `increase()` handle the reset, and it keeps idle routes from accumulating series.

## Alerting rules

Start with the bundled rules, then add latency and error-rate alerts using
your application's targets.

### Shipped alerts

The chart's `PrometheusRule` deploys these seventeen alerts when `controller.monitoring.prometheusRule.enabled: true`. Each is toggled by its own `controller.monitoring.prometheusRule.defaultRules.<key>` flag (all default to `true`):

| Alert | Toggle key (`defaultRules.<key>`) | Fires when |
|-------|-----------------------------------|------------|
| `HAProxyRetainedConfigurationActive` | `retainedConfiguration` | `haptic_retained_config_active == 1` for 2m |
| `HAProxyControllerReconciliationErrors` | `reconciliationErrors` | `rate(haptic_reconciliation_errors_total[5m]) > 0` for 5m |
| `HAProxyControllerDeploymentFailures` | `deploymentFailures` | `rate(haptic_deployment_errors_total[5m]) > 0` for 2m |
| `HAProxyFleetDiverged` | `fleetDiverged` | `haptic_haproxy_fleet_converged < haptic_haproxy_fleet_size` for 5m |
| `HAProxyControllerHighQueueDepth` | `highQueueDepth` | p95 `haptic_reconciliation_queue_wait_seconds` over `5s` for 5m |
| `HAProxyControllerNoLeader` | `leaderElectionLost` | `sum by (namespace, job) (haptic_leader_election_is_leader) == 0` for 1m |
| `HAProxyControllerConfigRejected` | `configRejected` | `increase(haptic_config_rejected_total[5m]) > 0` for 1m |
| `HAProxyControllerConfigNotValidated` | `configNotValidated` | `haptic_config_validated{status="True"} == 0` for 10m |
| `HAProxyControllerConfigPinned` | `configPinned` | `haptic_config_pinned > 0` for 5m |
| `HAProxyControllerHAProxyPodsRejected` | `haproxyPodsRejected` | `increase(haptic_haproxy_pods_rejected_total[5m]) > 0` for 5m |
| `HAProxyControllerNoHAProxyPods` | `noHAProxyPods` | `haptic_resource_count{type="haproxy-pods"} < 1` for 5m |
| `HAProxyControllerCriticalEventsDropped` | `criticalEventsDropped` | `increase(haptic_events_dropped_critical_total[5m]) > 0` |
| `HAProxyAgentApplyRejected` | `applyRejected` | `increase(haptic_apply_rejected_total[5m]) > 0` for 1m |
| `HAProxyAgentInvariantViolated` | `agentInvariantViolated` | `increase(haptic_agent_invariant_violations_total{name!="recovery_reload"}[5m]) > 0` |
| `HAProxyAgentRecoveryReloadFailed` | `recoveryReloadFailed` | `increase(haptic_agent_invariant_violations_total{name="recovery_reload"}[5m]) > 0` |
| `HAProxyAgentVersionSkew` | `agentVersionSkew` | `increase(haptic_agent_version_skew_total[15m]) > 0` for 30m |
| `HAProxyAccessLogRecordsDropped` | `accessLogDropped` | `increase(haproxy_process_dropped_logs_total[5m]) > 0` |

Turn one rule off, or replace the whole set with your own:

```yaml
# values.yaml
controller:
  monitoring:
    prometheusRule:
      enabled: true
      defaultRules:
        highQueueDepth: false   # drop a single shipped rule; the other sixteen stay
      # Or set `rules:` to a non-empty list to replace ALL default rules with your own:
      # rules:
      #   - alert: MyCustomAlert
      #     expr: ...
```

The full names, toggle keys, and default thresholds also appear on the [Chart Values Reference](../reference.md#monitoring).

<a id="recommended-alerts"></a>

## Dashboard examples

Enable `controller.monitoring.grafanaDashboard.enabled` as shown above to install
the supplied controller and traffic dashboards. If Grafana doesn't use a
dashboard-discovery sidecar, export both JSON documents from the default release
and import them in Grafana:

```bash
kubectl get configmap haptic-grafana-dashboard -n haptic \
  -o jsonpath='{.data.haptic\.json}' > haptic-controller-dashboard.json
kubectl get configmap haptic-grafana-dashboard -n haptic \
  -o jsonpath='{.data.haptic-traffic\.json}' > haptic-traffic-dashboard.json
```

Choose **Dashboards → New → Import** and upload both files. Select the Prometheus
data source that scrapes HAPTIC. The controller dashboard covers control-plane
health; the traffic dashboard covers request rate, errors, duration, routes,
termination states, and backend targets. You don't need to build dashboards
from the metric catalogue.

## Troubleshoot monitoring {#operational-insights}

<a id="key-health-indicators"></a>
<a id="capacity-planning"></a>
<a id="troubleshooting-with-metrics"></a>

| Symptom | Next check |
| --- | --- |
| Controller target is missing | Confirm the ServiceMonitor labels and namespace match your Prometheus selectors. |
| Scrape target is down | Check the metrics port and [monitoring ingress policy](networking.md#allowing-prometheus-scraping). |
| Controller metrics appear but traffic metrics don't | Enable the HAProxy PodMonitor; the controller monitor doesn't scrape HAProxy. |
| Fleet remains behind the desired configuration | Read [fleet diagnostics](diagnostics.md) and the rejected configuration or apply message. |
| No leader or frequent leadership changes | Follow [high-availability troubleshooting](high-availability.md#troubleshooting). |
| Resource use keeps growing | Compare traffic, resource counts, and the [sizing guide](performance.md). |

## See also

- [Debugging Guide](./debugging.md) - Runtime introspection and troubleshooting
- [High Availability](./high-availability.md) - Leader election configuration
- [Troubleshooting Guide](../troubleshooting.md) - General troubleshooting
