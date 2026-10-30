# ADR-0039: Request-metric exemplar ownership

## Status

Accepted (2026-10-30): pursue native Vector exemplar support and keep HAPTIC’s per-pod metrics path unchanged. Tracks [#529](https://github.com/haproxy-haptic/HAPTIC/issues/529).

## Verified findings

A local Vector 0.60.0 container received a histogram observation with a valid,
sampled trace ID. A scrape advertising OpenMetrics returned
`text/plain; version=0.0.4`, the histogram observation, and no exemplar. Its
request counter also retained its configured name without adding `_total`.
The container was removed after the probe.

Vector 0.60.0 uses `StringCollector` and a fixed Prometheus 0.0.4 content type.
The collector writes values, tags, and timestamps without exemplar data.
The same exporter behavior remains in 0.61.0, released on 2026-10-29, and
master at `b6ef27666b2816443e3a242f4ec4ecb220c57281`. Upgrading Vector or changing
the scrape Accept header does not add this capability. Its remote-write exemplar
request is also still open; switching sinks is not a verified solution.

HAPTIC's request metrics consume access-log records independently of trace
sampling. Its OTLP branch exports only records whose trace-flags sampled bit is
set. Root sampling defaults to 100 percent, but adopted unsampled parents are
honored. Request response-duration observations measure `Ta - TR - Tw`, while
span metrics depend on which emitted span kind is selected.

## Options

| Path | Existing request metrics | Runtime ownership and cost |
|------|--------------------------|----------------------------|
| Native Vector exemplar support | Can preserve names, labels, observations, and aggregation in one pipeline | Upstream metric-model, transform, aggregation, and exporter work; no additional HAPTIC data-plane service |
| Existing Collector or Tempo span metrics | Adds separate trace-derived metrics; does not fix exemplars on the current full-request histogram | Operator-owned collector configuration; spans absent because of sampling cannot contribute exact request observations |
| HAPTIC-owned observation exporter | Can preserve the requested metric contract with explicit migration and compatibility tests | A private per-pod observation input, bounded aggregation and exemplars, configuration lifecycle, and an OpenMetrics endpoint |
| Exemplar overlay on Vector scrapes | Preserves Vector's counts in principle | Additional per-scrape reads/parsing, exemplar input and retention, and reset/generation matching; avoid presenting stale observations as exemplars after Vector resets |

Trace IDs never become ordinary metric labels. A change of metric family or
counting semantics must be explicit; trace-derived metrics cannot silently
replace access-log metrics.

## Proposed constraints for an owned exporter

- Keep metric names, label opt-outs, histogram boundaries, timer validity gates,
  and cardinality protection consistent with the existing chart.
- Keep observations and exemplar reservoirs in the same aggregation lifecycle;
  do not duplicate the counter/histogram for a trace ID.
- Use valid sampled trace context, a configurable exemplar label name, and bounded
  exemplar retention. Unsampled observations still update ordinary metrics.
- Isolate observation ingestion from agent reconciliation and HAProxy traffic.
  Add no Kubernetes permissions, routing-resource knowledge in Go, or Lua.
- Specify queue exhaustion, malformed input, reconfiguration, process restart,
  and rollback behavior before implementation. Avoid duplicate scrape targets
  and double counting while migrating request metrics between exporters.
- Measure throughput, CPU, RSS, allocation rate, scrape latency, and dropped
  observations against the current pipeline on a fresh owned cluster.
- Verify real OpenMetrics exposition and Prometheus `query_exemplars`, existing
  metrics with unsampled traffic, mixed flags, invalid IDs, bucket boundaries,
  label limits, and exporter failure/recovery.

## Complexity gate

| Question | Current answer |
|----------|----------------|
| 1: Observed impact | 1 reported missing metric-to-trace link; metrics and traces still arrive |
| 2: Risk accepted already | No explicit acceptance of a new per-pod telemetry implementation |
| 3: Common paths | Scraped request metrics with traced traffic; not controller reconciliation metrics |
| 4: Prior art | Yes: Prometheus clients and Collector exporters support exemplars; Vector's current path does not |
| 5: New attack surface | Owned exporter: 1 private observation API/parser per pod; 0 necessary new Kubernetes grants |
| 6: New modes | At least 3: ingestion failure/exhaustion, aggregation reconfiguration, and exporter lifecycle failure |
| 7: New knobs | At least 2: activation and the requested exemplar label name; resource-budget controls remain unspecified |
| 8: Coupling | New Vector-to-HAPTIC observation contract if HAPTIC owns aggregation |
| 9: Fleet cost | Additional processing grows as pods × observations; at 0.5 KiB/record and 10,000 requests/s, transport alone is about 5 MiB/s per pod, before copies and aggregation; this is an estimate, not a benchmark |
| 10: Upgrade/rollback/disable | Not yet qualified; source ownership and scrape-target migration need explicit tests |
| 11: Smaller sufficient change | Collector span metrics satisfy trace-derived correlation, but not the exact existing full-request histogram contract; whether that alternative suffices needs a maintainer decision |

The maintainer selected the upstream path after reviewing these costs. Keep issue #529
open until a supported Vector release preserves exemplar metadata through
metric conversion, aggregation, and OpenMetrics exposition. Do not replace the
full-request metrics with sampled span metrics or add a HAPTIC-owned exporter.
Collector span metrics remain a separate operator-managed alternative.

## References

- [Upstream exporter feature request](https://github.com/vectordotdev/vector/issues/26963)
- [Vector 0.60.0 exporter](https://github.com/vectordotdev/vector/blob/v0.60.0/src/sinks/prometheus/exporter.rs#L394-L415)
- [Vector 0.60.0 text collector](https://github.com/vectordotdev/vector/blob/v0.60.0/src/sinks/prometheus/collector.rs#L275-L303)
- [Vector 0.61.0 exporter](https://github.com/vectordotdev/vector/blob/v0.61.0/src/sinks/prometheus/exporter.rs#L414-L435)
- [Vector remote-write exemplar request](https://github.com/vectordotdev/vector/issues/6748)
- [Collector span-metrics connector](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/connector/spanmetricsconnector)
- [Collector Prometheus exporter](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/exporter/prometheusexporter)
