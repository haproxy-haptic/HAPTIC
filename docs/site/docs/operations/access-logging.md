# Access logging

Read request logs from the `vector` container in a default installation. If you
disable Vector, read them from the `haproxy` container instead. These commands
use the `haptic` namespace:

```bash
# Default install (vector.enabled=true)
kubectl logs -n haptic -l app.kubernetes.io/instance=haptic,app.kubernetes.io/component=loadbalancer -c vector

# With vector.enabled=false
kubectl logs -n haptic -l app.kubernetes.io/instance=haptic,app.kubernetes.io/component=loadbalancer -c haproxy
```

HTTP frontends emit JSON records like this one. TCP frontends log connections
and use a smaller set of fields:

```json
{"ts":"2026-07-25T19:05:19.615000Z","req_id":"019f9ae9-3a61-7814-8601-774735249ecd","trace_id":"","client_ip":"10.244.0.1","frontend":"https","backend":"default_echo_echo_80","server":"echo-7c9d8b6f5-2xk9p","method":"GET","host":"echo.example.com","listener_port":"443","path":"/api/v1","http_version":"HTTP/1.1","status":200,"bytes":73,"request_time_ms":0,"queue_time_ms":0,"connect_time_ms":1,"response_time_ms":3,"total_time_ms":4,"retries":0,"term":"----","resource":"default/echo","denied_by":"","tls_version":"TLSv1.3","tls_sni":"echo.example.com"}
```

Raw access records have no syslog prefix; `ts` carries the timestamp. If you
collect HAProxy's stdout directly, allow for non-JSON startup, health-check, and
process messages alongside the access records.

OpenTelemetry Protocol (OTLP) trace export uses a separate, bounded memory buffer.
When the buffer holds 500 spans, Vector drops new spans so access logs and
request metrics keep flowing. Monitor `vector_buffer_discarded_events_total{component_id="otlp_traces"}`
for trace loss during collector outages or congestion.

Add the settings below to your [Helm values file](../deploying-with-helm.md#change-settings)
and apply them with `helm upgrade`.

## Core fields

| Field | Meaning |
|-------|---------|
| `ts` | Request accept time in Coordinated Universal Time (UTC), with microsecond precision |
| `req_id` | HAProxy-generated request ID. Forwarding it to your application is opt-in; see [Request IDs](#request-ids) |
| `server_pod` | Backend pod name, also recorded as `server`. No backend pod is identified when HAProxy answers the request itself |
| `namespace`, `service` | Namespace and name of the Kubernetes Service for the selected backend. The Service can be in a different namespace from the routing resource |
| `destination_ip` | Destination address as HAProxy sees it, after any address translation by your load balancer or cluster network |
| `instance_pod`, `instance_node` | HAProxy pod and node that handled the request. Empty when the pod identity environment variables aren't supplied |
| `trace_id` | Distributed trace ID from a valid inbound `traceparent`. With tracing enabled, HAPTIC creates one if the client supplies none; otherwise the field is empty |
| `client_ip` | Client address, after any `src-ip-header` rewrite |
| `frontend`, `backend`, `server` | HAProxy frontend, backend, and server names |
| `method`, `host`, `path`, `http_version` | Request identity. `path` excludes the query string |
| `listener_port` | Port used in the routing lookup, as a string. For a Gateway, this is the allocated pod port, which can differ from the public listener port. Empty on frontends without routing logic |
| `status`, `bytes` | Response status and bytes sent to the client (JSON numbers) |
| `request_time_ms`, `queue_time_ms`, `connect_time_ms`, `response_time_ms`, `total_time_ms` | Milliseconds spent receiving the request, waiting for a connection slot, connecting to the backend, waiting for response headers, and completing the request. `-1` means that phase didn't complete. The HTTP total excludes idle keep-alive time; the TCP total covers the whole session |
| `retries` | Backend connection retries |
| `term` | HAProxy's 4-character termination state — separates a client abort from a server abort, a timeout, and a response HAProxy generated itself |
| `resource` | `<namespace>/<name>` of the Ingress, HTTPRoute or custom resource that owns the matched route — the join key back to Kubernetes |
| `denied_by` | Which gate blocked the request; empty when the backend answered |
| `cache_degraded`, `rate_limit_degraded`, `waf_degraded`, `schema_degraded` | These mark a dependency-degraded cache, limiter, WAF, or schema-validation path. A strict policy can set both its degraded field and `denied_by`. Empty when the corresponding feature didn't encounter a degraded dependency |
| `route` | Matched host and path rule, with prefix matches marked `*`, such as `echo.example.com/api/*`. Present when tracing or the [request metrics](monitoring.md#request-metrics) `path` label needs it |
| `bytes_in` | Request **body** bytes from the client (`%U`) — no request line or headers, which HAProxy doesn't count. Present only when the `request_size` request metric is enabled, since nothing else reads it |

Template libraries add fields for the features you configure, each only when
that feature is in use: `waf_action`, `waf_rule_id` and `waf_score`;
`rate_limit_allowed` and `rate_limit_remaining`; `cache` (`HIT`/`MISS`/`STALE`) and
`app_backend`; `auth_status` and `consumer`; `schema_outcome`; `tls_version`,
`tls_sni` and `tls_resumed`; `mtls_verify` and `mtls_cn`; `gw_route`;
`captured_headers`; `client_ip_peer`.

Use `denied_by` to identify why HAPTIC rejected a request. Values include
`rate_limit_local`, `rate_limit_shared`, `rate_limit_shared_unavailable`, `waf`,
`jwt_signature`, `jwt_expired`, `api_key`, `hmac`, `basic_auth`,
`consumer_groups`, `body_too_large`, `schema_invalid` and the `*_unavailable`
fail-closed variants.

## Add your own fields

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        accessLog:
          fields:
            tenant: req.hdr(X-Tenant)
            region: str(prod-eu)
```

Each value is one HAProxy sample expression, captured into a transaction variable
at request time and emitted as a JSON string. Use `str(<value>)` for a constant
label. Field names must match `^[A-Za-z_][A-Za-z0-9_]{0,39}$` and must not
collide with a built-in field; expressions must not contain whitespace, `#`, `"`
or a backslash. A violation fails the render with a message naming the field.

Because the capture happens at request time, a value that doesn't exist yet reads
empty — a WAF verdict, a cache status, an auth outcome, or anything else a
[SPOA hub](spoa-hub.md) message produces later in the transaction. For
those, contribute a [`log-fields-*` snippet](#contribute-a-field-from-your-own-library)
instead: its items are evaluated when the line is written, after every filter has
run.

To log the query string, opt in with `query: query` — it's excluded by default
because query strings are a common accidental carrier of tokens and session ids.

Raise `accessLog.maxLineBytes` (default `16384`, accepted range 1024–65535) if
custom fields or captured request headers push records past it: HAProxy truncates
a longer line mid-byte, which makes the record unparseable. A value outside the
range fails the render rather than silently truncating every record.

## Where the logs go

Set `accessLog.targets` to send records to your own collector. The following
example replaces the default destination with a TCP collector running in the
same pod:

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        accessLog:
          targets:
            shipper:                      # a name you choose; it keys the target
              ring:
                name: accesslog
                address: 127.0.0.1:6514   # a log-shipper sidecar on loopback
```

HAProxy's process and alert messages keep going to stdout. If you replace the
Vector destination, its log-derived request metrics and traces stop receiving
records too. Choose access controls and retention for the destination: access
records include client addresses and request paths.

Each target gets a copy of every eligible access record. Configure multiple
targets to send records to more than one collector:

| Field | Meaning |
|-------|---------|
| `address` | `stdout`, `stderr`, `fd@<n>`, `<host>:<port>` (UDP), `[<ipv6>]:<port>`, an absolute socket path, or `ring@<name>` |
| `format` | `raw`, `rfc3164`, `rfc5424`, `local`, `priority`, `short`, `timed`, `iso`. Defaults to `raw` for stdout/stderr and `rfc5424` otherwise |
| `facility`, `level` | Syslog facility (default `local0`) and level, either `info` (default) or `debug` |
| `ring` | Send through a buffered TCP ring instead of a bare address |

`level` is a *maximum* severity filter, and HAProxy emits access records at
`info`. Anything stricter — `notice`, `warning`, `err` — therefore drops every
record while `haproxy -c` still reports the config as valid, so the chart accepts
only the two levels that deliver.

An `address` of `ring@<name>` must name a ring some target in this list declares.
HAProxy accepts a dangling reference at config check and then refuses to start
with `unknown ring named`, so the render rejects it instead.

### Why a ring for a sidecar

A `ring` is a buffered TCP client: records queue in memory when the collector is
unavailable and flush when it reconnects, up to the configured buffer capacity.
A plain `<host>:<port>` target uses UDP and provides no replay buffer.

Configure each ring with these fields:

- `name` and `address`: a host and port, such as `collector:514` or `[::1]:514`.
  HAProxy 3.4 doesn't accept a Unix socket as a ring server; use a plain-path
  logging target for a Unix-socket collector.
- `size`: buffer bytes, default `65536`. Keep it at least 256 bytes larger than
  `maxLineBytes` to avoid truncating records into invalid JSON.
- `logProto`: `legacy` for newline-delimited RFC 6587, or `octet-count`.
- `connectTimeout` and `serverTimeout`: connection and server timeouts.
- `serverOptions`: additional HAProxy server keywords, inserted verbatim, such
  as TLS settings.

A collector reads this as ordinary syslog carrying a JSON payload. In Vector, a
`syslog` source parses the envelope and one `remap` recovers the record:

```yaml
sources:
  haproxy_access:
    type: syslog
    mode: tcp
    address: 0.0.0.0:6514
transforms:
  parsed:
    type: remap
    inputs: [haproxy_access]
    source: |
      . = parse_json!(string!(.message))
```

Keep these constraints in mind:

- **A ring server's address is resolved when the config is parsed.** A Service DNS
  name that doesn't resolve at that moment fails the render. Use a loopback
  sidecar address or a literal IP, or pass `resolvers`/`init-addr` through
  `serverOptions`.
- **Any file referenced from `serverOptions`** (a `ca-file`, a client `crt`) must
  exist wherever the config is validated — the controller pod — not only in the
  HAProxy pod. Deliver such material through the chart's file mechanism so both
  see it.
- **A plain-path (Unix socket) target does no buffering.** It's the way to reach a
  collector on a socket, since HAProxy 3.4 rejects a Unix socket as a ring server.
  If the socket is absent, HAProxy continues serving and reports log-delivery
  errors. Those records are lost; use a ring when you need bounded buffering.

## Dropping records you don't need

To reduce log volume, suppress successful HTTP requests:

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        accessLog:
          suppress:
            successful: true
```

Suppression drops 2xx/3xx records only when no gate denied the request. Denials,
4xx, and 5xx remain eligible for logging, subject to the transport's
[loss behavior](#the-access-log-is-lossy-under-back-pressure).

The default retains successful requests because they help diagnose retries and
intermittent failures. Suppression also removes those requests from log-derived
metrics and traces. Choose retention and access controls for the data you log;
this setting doesn't remove sensitive fields from the records you keep.

The rule runs after HTTP responses, including responses HAProxy generates
itself. TCP-mode frontends are unaffected. The internal TCP frontend already
uses `option dontlog-normal`; TLS passthrough logs each connection.

## The access log is lossy under back-pressure

The default connection to Vector uses a Unix datagram socket. If its receive
queue fills, HAProxy discards records and continues serving requests. Access
logs, and the metrics and traces derived from them, can therefore miss traffic.

Monitor `haproxy_process_dropped_logs_total` on HAProxy's Prometheus endpoint,
or `DroppedLogs` in the stats socket's `show info` output. These counters track
records HAProxy discards; they don't detect records lost or rejected later in
your collection pipeline.

The chart's `HAProxyAccessLogRecordsDropped` alert is available through
`controller.monitoring.prometheusRule`. If it fires, check Vector's health and
CPU budget. You can also [reduce log volume](#dropping-records-you-dont-need),
which reduces the requests available for log-derived metrics and traces.

## Vector sidecar

The default Vector sidecar receives access logs, writes them to stdout, and
builds [request metrics and optional traces](monitoring.md#request-metrics) from
them. It also exports the SPOA hub's metrics. See [monitoring setup](monitoring.md)
for scrape endpoints and settings.

To remove the sidecar:

```yaml
vector:
  enabled: false
```

HAProxy then logs to its own stdout. Log-derived metrics and traces are no longer
available; Prometheus can still scrape HAProxy and the SPOA hub directly.

<a id="how-the-config-reaches-it"></a>

HAPTIC updates Vector's configuration as your settings change, and Vector reloads
the file automatically. If Vector exits or repeatedly fails its health check,
its supervisor restarts the process while HAProxy keeps serving. Records can be
lost during startup or recovery. A container-level failure, such as an
out-of-memory termination, can still affect pod readiness.

## Request IDs

HAPTIC generates a UUIDv7 for each request and records it in `req_id`. The ID
contains a timestamp and random bits, with no client or load-balancer address.

To send it to your application as `X-Request-ID`, add this Ingress annotation:

```yaml
metadata:
  annotations:
    haproxy-haptic.org/request-id: "true"
```

The [request ID annotations](../libraries/haptic-annotations.md) also let you
choose a header name or accept an inbound ID. If you accept a client-supplied
ID, the header can differ from the HAProxy-generated `req_id` in the access log.

For UUIDv4 instead, override the directive through a `defaults-settings-*`
snippet with a band above 150:

```yaml
controller:
  config:
    templateSnippets:
      defaults-settings-160-request-id:
        template: |
          unique-id-format %[uuid()]
```

With tracing disabled, `trace_id` records a valid inbound `traceparent` for log
correlation; HAPTIC creates no trace context. With
[`extraContext.tracing.enabled`](../reference.md#logging-and-templating), HAPTIC
adopts valid inbound context or creates a new trace, then propagates it to the
backend. Trace-context propagation is separate from the
[`haproxy-haptic.org/request-id`](../libraries/haptic-annotations.md) annotation,
which forwards the request ID in a header.

## Contribute a field from your own library

`log-fields-*` is the extension point. Use it instead of
[`accessLog.fields`](#add-your-own-fields) when the value only exists at log time,
or when a template library should contribute the field for every install that
enables the feature. A snippet emits named log-format items and nothing else:

```yaml
controller:
  config:
    templateSnippets:
      log-fields-900-my-feature:
        template: |
          %(my_field)[var(txn.my_var)]
```

Only items available at log time are legal. HAProxy rejects `path`, `pathq`,
`req.hdr()`, `res.hdr()` and `req.ssl_sni` inside a `log-format`, so materialise
anything request- or response-scoped into a transaction variable first
(`http-request set-var(txn.my_var) req.hdr(X-Thing)`). Type an item (`:sint`,
`:bool`) only when its fetch always resolves — an unresolved typed item renders
`""` into a numeric slot.

## Change the destination or the whole format

Use [`accessLog.targets`](#where-the-logs-go) to change access log destinations
or facilities. `global-settings-100-logging` controls HAProxy's process logs.

To replace the access record format, override `util-log-format-http` (HTTP-mode
frontends) or `util-log-format-tcp` (TCP-mode frontends). Note that a
`defaults`-section `log-format` can't reference HTTP-scoped fetches at all,
which is why the format is emitted per frontend.
