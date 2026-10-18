# Base library

The base library provides the main HAProxy configuration and the places where
you can add your own snippets. Use it to change global settings, add a frontend,
or extend routing without replacing the complete configuration.

It's enabled by default. The routing libraries use its frontends, maps, error
pages, and shared template functions.

<a id="overview"></a>

The Ingress preset uses base to assemble its configuration:

<div class="pg-embed" markdown data-scenario="ingress" data-tab="haproxy.cfg" data-controls="tabs,resources" data-title="Add a global HAProxy setting" data-height="440">

<p class="pg-task" markdown>In the **Templates** pane, add a `global-settings-500-tuning` snippet under `spec.templateSnippets` (the YAML is in the hint), then watch `maxconn 10000` appear inside the `global` section of the `haproxy.cfg` tab.</p>

<details class="pg-hint" markdown>
<summary>What to expect</summary>

Add a `global-settings-*` snippet to insert directives in HAProxy's `global`
section. Matching snippets render alphabetically. For example, add this under
`spec.templateSnippets`:

```yaml
global-settings-500-tuning:
  template: |
    maxconn 10000
```

The `global` section gains `maxconn 10000` after the built-in settings.

</details>

</div>

## Try it: emit a header per map entry

A snippet can use a loop to emit one directive per map entry:

```go
{%- for name, value := range headers %}
http-response set-header {{ name }} {{ value }}
{%- end %}
```

Try the same pattern with the map below.

<div class="pg-embed" markdown data-scriggo data-title="Turn a map into response headers" data-difficulty="2" data-height="280">

<p class="pg-task" markdown>Loop over `extraHeaders` and emit one `http-response set-header` directive per entry.</p>

```go
{%- var extraHeaders = map[string]string{
  "X-Frame-Options": "DENY",
  "X-Content-Type-Options": "nosniff",
} %}
{# Add the loop here. #}
```

<details class="pg-solution" markdown>
<summary>Show the solution</summary>

```go
{%- var extraHeaders = map[string]string{
  "X-Frame-Options": "DENY",
  "X-Content-Type-Options": "nosniff",
} %}
{%- for name, value := range extraHeaders %}
http-response set-header {{ name }} {{ value }}
{%- end %}
```

Map iteration order is unspecified. These independent header directives work in either order.

</details>

</div>

The bundled community images use AWS-LC and don't support
`tune.ssl.default-dh-param`. If you use an OpenSSL-based image and need
finite-field Diffie-Hellman, add `ssl-dh-param-file` in a `global-settings-*`
snippet.

## Configuration

Keep base enabled when using its extension points. To disable it, supply your own `haproxyConfig` and the snippets required by any libraries you retain.

```yaml
controller:
  templateLibraries:
    base:
      enabled: true  # Default; leave on unless you supply a complete replacement haproxyConfig
```

## Extension points

The base library defines extension points using the `render_glob "prefix-*"` operator. Any template snippet with a matching prefix is automatically rendered at the designated location in the HAProxy configuration.

Each snippet starts on its own line, so a snippet doesn't need a leading or trailing newline. The bundled libraries wrap every extension point that emits lines in [`on_own_lines()`](../template-reference.md#functions-and-filters), which adds a line break only where a snippet would otherwise continue the previous line.

`frontend-switching-*` emits `use_backend` rules after request filters, avoiding HAProxy rule-ordering warnings. Switching snippets run in lexical order before the default backend selection.

### Available extension points

This table is the authoritative registry of every `render_glob` extension point `base.yaml` defines. The [Template Libraries overview](../template-libraries.md#available-extension-points) lists the commonly used subset.

| Extension Point | Prefix Pattern | Location in Config | Purpose |
|-----------------|----------------|-------------------|---------|
| Global Settings | `global-settings-*` | Inside `global` section | Global directives (logging, process, paths, SSL tuning) |
| Defaults Settings | `defaults-settings-*` | Inside `defaults` section | Defaults directives (options, balance, timeouts, errorfiles) |
| Features | `features-*` | Early in config generation | Feature initialization and registration |
| Global Top | `global-top-*` | After `defaults` section | Top-level HAProxy elements (userlists, peers, etc.) |
| HTTP Bind Extra | `http-bind-extra-*` | Inside the outer plaintext TCP frontend, after the chart-static bind | Additional plaintext-HTTP `bind` lines (for example, Gateway HTTP listeners on non-default ports); every added port goes through the same [h2c detection](#h2c-cleartext-detection) |
| Frontend Extra | `frontend-extra-*` | After frontend bind, before routing | Early frontend directives (options, captures, ACLs) |
| Listener Port Translation | `frontend-routing-listener-port-*` | Routing prologue, after `txn.listener_port` is seeded from `dst_port` | Remap `txn.listener_port` when a library binds a pod port that differs from the user-facing listener port (for example, Gateway per-Gateway HTTPS binds) |
| Host Matching | `frontend-routing-host-match-*` | Before the standard hostname lookups | Set `txn.host_match` to select a routing group; leave it empty to use the standard lookups |
| Frontend Matchers | `frontend-matchers-advanced-*` | Within frontend routing logic | Advanced request matching (method, headers, query params) |
| Frontend Filters | `frontend-filters-*` | HTTP frontend, after routing | Request/response filters (header modification, redirects) |
| Forwarded-For Mode | `forwarded-for-mode-*` | HTTP frontend, first in the filter chain | Rules that set `txn.xff_mode` to `replace`, `append`, `ifmissing`, or `ignore` for the requests a library claims; see [forwarded headers](#forwarded-headers) |
| Frontend Switching | `frontend-switching-*` | HTTP/HTTPS frontend, after filters and before the default backend selection | Conditional `use_backend` rules, including canary splits |
| Access Log Fields | `log-fields-*` | Inside the per-frontend `log-format` line | Named JSON fields contributed to the [structured access log](../operations/access-logging.md) |
| Custom Frontends | `frontends-*` | After HTTP frontend | Additional frontend definitions |
| Custom Backends | `backends-*` | Before `default_backend` | Backend definitions from resource libraries |
| Host Map | `map-host-*` | host.map file | Host-to-group mapping entries |
| Host Regex Map | `map-hostregex-*` | host-regex.map file | Regex hostname fallback entries, tried after the exact and wildcard host lookups miss |
| Host Alias Regex Map | `map-hostaliasregex-*` | host-alias-regex.map file | Host alias regular expressions, each matching the whole hostname, tried after host-regex.map misses. Listener-scoped host lookups don't read it |
| Path Exact Map | `map-path-exact-*` | path-exact.map file | Exact path match entries |
| Path Prefix Exact Map | `map-pfxexact-*` | path-prefix-exact.map file | Prefix-exact path match entries |
| Path Prefix Map | `map-path-prefix-*` | path-prefix.map file | Prefix path match entries |
| Path Regex Map | `map-path-regex-*` | path-regex.map file | Regex path match entries |
| Weighted Backend Map | `map-weighted-backend-*` | weighted-multi-backend.map file | Weighted routing entries |
| Body Size Map | `map-body-size-*` | body-size.map file | Per-backend request body-size limits (bytes), enforced by `frontend-filters-250-request-body-size` |
| Request Host Map | `map-reqhdr-host-*` | reqhdr-host.map file | Per-backend upstream `Host` header override, applied by `frontend-filters-260-request-set-host` |
| X-Forwarded-Prefix Map | `map-reqhdr-xfwd-prefix-*` | reqhdr-xfwd-prefix.map file | Per-backend `X-Forwarded-Prefix` header, applied by `frontend-filters-261-request-set-xfwd-prefix` |
| Connection Header Map | `map-reqhdr-connection-*` | reqhdr-connection.map file | Per-backend `Connection` header override, applied by `frontend-filters-262-request-set-connection` |
| Path Rewrite Map | `map-path-rewrite-*` | path-rewrite.map file | Per-backend literal full-path rewrite, applied by `frontend-filters-400-path-rewrite` (capture/regex rewrites stay in the backend) |
| Request Buffering Map | `map-request-buffering-*` | request-buffering.map file | Per-backend request-buffering override (`on`/`off`), applied by `frontend-filters-090-request-buffering` |
| Status Patches | `status-patches-*` | After features, before backends | Resource status patch registration (side effects only) |
| Status Extra | `status-extra-*` | Inside the status frontend | Extra status-frontend directives (Prometheus exporter, custom endpoints) |

The `map-body-size-*` through `map-path-rewrite-*` family shares one design: a resource library writes a per-backend value into a map keyed by backend name, and a static base-library filter looks it up at request time. A backend with no entry is unaffected, so adding or changing one of these values is a map-only (reload-free) change.

Further extension points are defined by other bundled libraries, not by the base library:

- `https-bind-extra-*` — invoked by the SSL library's HTTPS frontend for additional TLS `bind` lines; see [SSL Library](ssl.md).
- `ssl-tcp-bind-extra-*` — also SSL-defined, for the TCP-mode passthrough listener's `bind` lines.
- `backend-directives-*` — invoked by the Ingress library's `backends-500-ingress` snippet (with `inherit_context`) so per-backend annotation libraries can extend each Ingress backend block; see [haproxytech library](haproxytech.md) for the producer side. Templates outside the ingress backend loop won't see it.
- `spoe-agents-*`, `frontend-spoe-filters-*`, `frontend-spoe-set-pass-headers-*`, `frontend-spoe-set-fail-headers-*` — defined by the auto-loaded spoa-hub library; see [SPOA Hub](../operations/spoa-hub.md).

<a id="how-extension-points-work"></a>

### Injecting custom configuration

You can inject custom HAProxy configuration by adding template snippets with the appropriate prefix in your values.yaml:

```yaml
controller:
  config:
    templateSnippets:
      # Override default timeouts (replaces the base library snippet)
      defaults-settings-300-timeouts:
        template: |
          timeout connect 5000
          timeout client 30000
          timeout server 30000
          timeout tunnel 600000
          timeout http-request 10000

      # Add custom security rules to the HTTP frontend
      frontend-filters-custom-security:
        template: |
          http-request deny if { path_beg /admin } !{ src 10.0.0.0/8 }
          http-request deny if { path_beg /.env }

      # Add custom backend
      backends-custom-maintenance:
        template: |
          backend maintenance_backend
              http-request return status 503 content-type text/html string "<h1>Under Maintenance</h1>"
```

### Snippet priority

Snippets within a `render_glob` pattern execute in **alphabetical order**. Encode priority in the snippet name via a numeric prefix (lower numbers run first):

For example, `frontend-filters-050-headers` renders before
`frontend-filters-500-auth`. Use the prefix of the extension point you need;
priorities only order snippets within that prefix.

See [Template Libraries → Snippet Priority](../template-libraries.md#snippet-priority) for the reserved range conventions used by the built-in libraries.

## Features

### Frontend routing logic

The base library implements the routing system using HAProxy maps and transaction variables. The rendered config keeps the per-frontend directives terse; this section is the reference the generated comments link to.

**1. Host matching** — `txn.host_match` is resolved through a fall-through cascade, each step tried only if the previous left it empty (`-m len 0`):

| Order | Lookup | Purpose |
|-------|--------|---------|
| 1 | `host_full` (Host header verbatim, incl. `:port`) | Port-pinned routes — for example Gateway listeners on non-default ports — match before the port-stripped lookups. Only fires when the request actually carried a port, so the common case has zero overhead. |
| 2 | `host` (port stripped) | Normal hostname match. |
| 3 | `host` with leading label removed (`regsub(^[^.]*,,)`) | Wildcard hosts (`*.example.com` stored as `.example.com`). |
| 4 | `host-regex.map` | Regex hostnames. |
| 5 | `host-alias-regex.map` | Host alias regular expressions, such as `haproxy-haptic.org/host-alias-regex`. |
| 6 | `host:listener_port` / `:listener_port` | Per-listener-port fallback when no hostname matched (Gateway listeners on dedicated ports). |

**Listener-port translation.** `txn.listener_port` is the user-facing port the request arrived on. For chart-static binds it equals `dst_port`. Resource libraries that map a pod-port to a different listener port (for example Gateway API per-Gateway HTTPS binds listening on an allocated pod port like `18002` while the map keys use the original `8443`) plug a translation into the `frontend-routing-listener-port-*` extension point. With no such library, `dst_port` passes through unchanged.

**2. Path matching** — evaluated in order Exact > Regex > Prefix-exact > Prefix:

Set `controller.config.templatingSettings.extraContext.routing.regexMatchOrder`
to `last` to try prefix matches before regex matches. This changes which route
wins when both match; see [path matching order](../template-libraries.md#path-matching-order).

Custom matching snippets use `txn.path_match` to select a backend:
`BACKEND:<name>` selects one backend; `MULTIBACKEND:<weight>:<key>` selects a
weighted group. Use the `frontend-matchers-advanced-*` extension point to add
matching conditions.

### Connection reliability and timeouts

The defaults section is tuned for a Kubernetes ingress workload, where backends are pod IPs from EndpointSlices reached directly over the cluster's Container Network Interface (CNI) fabric.

**`timeout connect` defaults to `100ms`.** A short connection timeout limits how
long a request waits on an unreachable pod before HAProxy can retry another
server. Increase it if healthy backends in your network take longer to connect;
the default isn't a bound on total request or failover time.

Set a longer timeout in milliseconds:

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        timeoutConnect: "5000"   # ms; also timeoutClient / timeoutServer / timeoutHttpRequest / timeoutHttpKeepAlive
```

**`option redispatch`** allows a failed connection attempt to retry another
server. The default retry count is `3`. See HAProxy's
[retry documentation](https://www.haproxy.com/documentation/hapee/latest/service-reliability/retries/retries/).

**`retry-on conn-failure empty-response response-timeout`** covers failed
connections, connections closed before a response, and response timeouts. The
latter two also cover a backend that accepts a connection while shutting down
but doesn't complete the response. Override the list with `extraContext.retryOn`.

Retries that replay a request are limited to idempotent methods: GET, HEAD, PUT,
DELETE, OPTIONS, and TRACE. Replaying POST or PATCH can submit an operation twice.
Connection failures can still retry for any method because no request reached
the server. Set `extraContext.retryNonIdempotent: true` only when your backends
can safely process replayed requests.

### `h2c` cleartext detection

The HTTP listener accepts both HTTP/1.1 and cleartext HTTP/2 clients that send
the HTTP/2 connection preface directly, including gRPC clients. It doesn't
support the HTTP/1.1 `Upgrade: h2c` handshake. Both protocols use the same routing
rules and frontend snippets.

### gRPC request handling

Unmatched gRPC requests receive `grpc-status: 12` (Unimplemented). Other unmatched
HTTP requests receive `404`.

### Forwarded headers

Every HTTP frontend drops the `X-Forwarded-*` headers a client sends and sets its
own; the [security page](../operations/security.md#forwarded-headers) describes
what backends receive and how to trust a load balancer. Without trusted proxies
and per-route modes, the rules are:

```haproxy
http-request set-header X-Forwarded-Proto %[ssl_fc,iif(https,http)]
http-request del-header X-Forwarded-Host
http-request del-header X-Forwarded-Port
http-request del-header X-Forwarded-For
# ... every other frontend-filters-* rule, including set-src ...
http-request add-header X-Forwarded-For %[src]
```

`frontend-filters-010-forwarded-headers` runs first in the filter chain, so SPOE
messages and every later rule see the cleaned headers.
`frontend-filters-998-forwarded-for` appends the client address after any
`set-src` rewrite. With `extraContext.forwardedHeaders.trustedProxies` set, the
deletions skip a source listed in `forwarded-trusted-proxies.map`, and such a
source gets `X-Forwarded-Proto` only when it sent none.

A library overrides `X-Forwarded-For` per request with a `forwarded-for-mode-*`
snippet that sets `txn.xff_mode`:

| Mode | Client's `X-Forwarded-For` | Client address appended |
|------|----------------------------|-------------------------|
| `replace` | Dropped, even from a trusted proxy | Yes |
| `append` | Kept | Yes |
| `ifmissing` | Kept | Only when there is none |
| `ignore` | Kept | No |

### Request buffering

HAProxy waits for the request body before it takes a backend connection, so a client that trickles its upload holds an HAProxy buffer instead of a backend connection. This is the standard defence against the slow POST attack, where an attacker declares a large body and sends it a byte at a time to exhaust the application's worker pool.

Buffering is on by default with a 10-second wait. These are the default values;
set `enabled: false` to disable buffering fleet-wide, or change `waitTimeout` to
adjust the wait:

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        requestBuffering:
          enabled: true
          waitTimeout: 10s
```

Override it for a single route with the [`haproxy-haptic.org/request-buffering`](haptic-annotations.md#request-buffering) annotation (`on` or `off`), or [`nginx.ingress.kubernetes.io/proxy-request-buffering`](nginx-ingress.md#buffering) with the nginx-ingress library. The override lives in `request-buffering.map`, so changing one route's setting reloads nothing, and it's authoritative: a route set to `off` isn't buffered even when a sibling route on the same frontend enables [request mirroring](haptic-annotations.md#canary-and-traffic-mirroring), which buffers bodies of its own accord. The consequence for a mirrored route that opts out is that its mirror receives an empty body.

HAProxy releases the request as soon as *either* the body is complete or `tune.bufsize` is full, so this is slow-client protection rather than an upload buffer — a 1 GB upload proceeds once the first 16 KiB arrive. When the wait expires with neither condition met, the client gets a `408` and the backend is never contacted.

<a id="streaming-requests-are-never-buffered"></a>

#### Streaming requests

Only requests that declare a `Content-Length` are held. Requests without that
header bypass this buffering step.

Buffering a bidirectional stream can prevent progress: the client waits for a
response while HAProxy waits for more request data. Excluding requests without
`Content-Length` lets HAProxy forward them without waiting for a complete body.

Chunked HTTP/1.1 uploads and gRPC clients that omit `Content-Length` bypass this
step. The decision depends on the header, not the application protocol.

Use `off` for a route whose clients declare a `Content-Length` but expect a
response before the body ends, such as a resumable-upload endpoint.

### Built-in operators and functions

#### `render_glob` (operator)

Renders all snippets matching a glob pattern. No import is needed:

```scriggo
{{ render_glob "backends-*" }}
{{ render_glob "map-host-*" inherit_context }}
```

See the [templating guide](../templating.md) for details.

#### `sanitize_regex` (function)

Built-in function that escapes regex metacharacters so a user-supplied literal (a host or path containing `.`, `+`, …) can be safely embedded in a `map_reg()` lookup. Edit the value and watch each metacharacter pick up a backslash:

<div class="pg-embed" markdown data-scriggo data-title="sanitize_regex escapes a literal for map_reg" data-height="320">

```go
{# sanitize_regex(s) escapes every regex metacharacter in s (regexp.QuoteMeta),
   so the string matches literally inside a map_reg() lookup. Edit it. #}
{%- var literal = "/api.v1/users" -%}
{{ sanitize_regex(literal) }} api_backend
```

</div>

!!! warning "Validate values before inserting them into configuration"
    HAProxy templates emit plain text. A newline in an unchecked annotation can
    introduce another directive that still passes syntax validation. Use
    `ValidateConfigValue` for single-token values and `ValidateCidrList` for CIDR
    lists from the [annotation helpers](ingress-annotations-compat.md).
    Use `sanitize_regex` when a literal value must be escaped inside a regex.
    The bundled annotation libraries apply these checks to their inputs.

### Utility macros

The following macros are available in the bundled library stack. `BackendServers()`
and its server helpers come from `kubernetes-backends`; the other utilities come
from base:

| Macro | Purpose |
|-------|---------|
| `CalculateShardCount(resourceCount, itemsPerShard)` | Chooses a bounded number of shards for the resource count |
| `HostMatchCondition(hosts)` | Builds a host-match ACL condition (in `util-ingress-helpers`) |
| `BuildServerOptions(serverOpts)` | Renders server-line option flags (in `util-backend-servers-helpers`) |
| `Backend(spec)` | Emits one `backend` section (`from` a content-addressed profile) from a record (in `util-backend`) |
| `BackendServers(serviceName, _, port, opts, portName, backendName, namespace)` | Resolves a Service into one server record per endpoint, named after the pod (in `util-backend-servers`); the second argument is unused (kept for signature stability) |
| `ServerName(podName)` | Sanitises a pod name into an HAProxy server name (in `util-backend-servers-helpers`) |

Usage:

```scriggo
{%- import "util-backend" for Backend %}
{%- import "util-backend-servers" for BackendServers %}
{{ Backend(map[string]any{
     "name":    backendKey,
     "guid":    make_guid("be", backendKey),
     "body":    []any{"default-server check"},
     "servers": BackendServers(serviceName, 0, port, serverOpts, nil, backendKey, namespace),
   }) }}
```

Emit every backend through `Backend()`. It builds the section text from the
record it declares to the controller, which is how the controller knows what a
config change actually changed. A `backend` section written by hand still
renders, but the controller can only treat it as opaque text.

`mode`, `balance`, `hash-type`, `default-server` and the `profile` directive
lines don't go in the backend section — they go in a shared, content-addressed
`defaults haptic-be-<hash> from haptic-base` that the backend inherits with
`from`. Two backends of the same shape share one profile section, which is what
lets a route of an existing shape be added at runtime without a reload. The
backend section itself is then only `from`/`guid`/`body`/servers; keep `body`
empty (put per-backend values in `profile`, per-server values on the server
line's `extra`) for a dynamic-eligible backend.

`Backend()` accepts these keys, and fails the render on any other:

| Key | Purpose |
|-----|---------|
| `name` | Backend name (required) |
| `guid` | Value of the `guid` line |
| `mode` | `http`, `tcp` or `spop`; carried by the profile; omit to inherit `http` from `haptic-base` |
| `balance`, `hashType` | `balance` and `hash-type`, carried by the profile |
| `profile` | Directive lines shared by same-shape backends (timeouts, retries, cookie, `http-request` rules) — go into the named `defaults`; comments and blank lines are dropped |
| `defaultServer` | `default-server` keyword records (`name`, `args`), formatted into the profile's `default-server` line |
| `body` | Directive lines that must stay in this section (stick-table, filter, raw injections) — a non-empty `body` makes the backend structural (reloads on create/delete/body change) |
| `servers` | Server records: `name`, `address`, `port`, `weight`, `disabled`, `guid`, `comment`, `extra` |
| `comments` | Provenance lines emitted above the section header |
| `shape` | `dynamic` (default when `body` is empty) or `structural`; set it to force structural (a unix-socket loopback backend) |

A library that routes to something other than a Kubernetes Service passes its
own `servers` list instead of calling `BackendServers()`.

### Backend servers

The `util-backend-servers` snippet resolves endpoints into server records:

- One `server` per endpoint, named after its pod (`server <pod> <ip>:<port>`). Rolling updates add and remove servers through the Runtime API when supported.
- Not-ready and terminating endpoints render as `disabled` servers (they take no traffic), so a readiness flip is a runtime `set server state` rather than a del+add.
- Per-server options (`maxconn`, SSL, weight, health-check params) via `serverOpts` / the server record's `extra`.

The snippet holds only the `BackendServers` helper, so import it and call it — rendering the snippet emits nothing. It returns records, so pass the result to `Backend()` rather than showing it:

```scriggo
{%- var service_name = "my-service" %}
{%- var port = 8080 %}
{%- import "util-backend" for Backend %}
{%- import "util-backend-servers" for BackendServers %}
{{ Backend(map[string]any{
     "name":    backendName,
     "servers": BackendServers(service_name, 0, port, serverOpts, nil, backendName, namespace),
   }) }}
```

### Error pages

Pre-configured error response templates for common HTTP errors:

| File | HTTP Status |
|------|-------------|
| 400.http | Bad Request |
| 403.http | Forbidden |
| 408.http | Request Timeout |
| 500.http | Internal Server Error |
| 502.http | Bad Gateway |
| 503.http | Service Unavailable |
| 504.http | Gateway Timeout |

### Structured access log

Every frontend emits one JSON object per request, assembled from HAProxy's native
JSON log encoding. `base.yaml` owns the core field set — request identity,
timers, the owning Kubernetes resource, and `denied_by` — and every library adds
fields for the features it implements through the `log-fields-*` extension point,
each gated on that feature actually being configured.

Add your own fields without writing a snippet:

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        accessLog:
          fields:
            tenant: req.hdr(X-Tenant)
```

Or contribute one from a library snippet:

```yaml
controller:
  config:
    templateSnippets:
      log-fields-900-my-feature:
        template: |
          %(my_field)[var(txn.my_var)]
```

<div class="pg-embed" markdown data-scenario="ingress" data-tab="haproxy.cfg" data-controls="tabs" data-title="Challenge: add a field to the access log" data-difficulty="2" data-height="440">

<p class="pg-task" markdown>In the **Templates** pane, add a `log-fields-900-scheme` snippet under `spec.templateSnippets` that contributes a `scheme` field, then find it inside the `log-format` line of each HTTP frontend in the `haproxy.cfg` tab.</p>

<details class="pg-solution" markdown>
<summary>Solution</summary>

A `log-fields-*` snippet emits items, not directives. `ssl_fc` is connection-scoped, so it's available at log time and needs no transaction variable:

```yaml
log-fields-900-scheme:
  template: |
    %(scheme:bool)[ssl_fc]
```

Band 900 sorts after every bundled contribution, so the field lands at the end of
the record. Typing it `:bool` is safe here because `ssl_fc` always resolves —
`false` on a plaintext connection. Try `%(scheme)[req.hdr(X-Forwarded-Proto)]`
instead and the render fails: HAProxy rejects request-header fetches inside a
`log-format`, which is why request-scoped values go through
`http-request set-var(txn.…)` first.

</details>

</div>

A `log-fields-*` snippet emits named log-format items and nothing else. Only
items available at log time are legal: HAProxy rejects `path`, `pathq`,
`req.hdr()`, `res.hdr()` and `req.ssl_sni` inside a `log-format`, so materialise
request- or response-scoped values into a transaction variable first. Because the
assembled format string is shared by every frontend, a snippet must not branch on
which frontend is rendering — that's what keeps one schema across the whole log
stream.

See [Access logging](../operations/access-logging.md) for the field
reference, the `denied_by` values, request-id and trace-context behaviour, and
how to replace the format wholesale.

### Debug headers

When debug mode is enabled, the frontend adds response headers for routing introspection:

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        diagnostics:
          routingHeaders:
            enabled: true
```

Debug headers include:

- `X-HAProxy-Backend`: Selected backend name
- `X-HAProxy-Host-Match`: Matched host group
- `X-HAProxy-Path-Match`: Full path match result
- `X-HAProxy-Path-Match-Qualifier`: BACKEND or MULTIBACKEND

### Shared memory stats (HAProxy 3.3+)

When `haproxy.shmStats.enabled` is `true` and HAProxy version is 3.3 or later, the base library adds `shm-stats-file` and `shm-stats-file-max-objects` to the global section. This persists stats counters (frontend/backend/server metrics) across HAProxy reloads via shared memory, eliminating counter resets during configuration changes.

The `shm-stats-file-max-objects` value is a configurable fixed value (default 50000) set via `haproxy.shmStats.maxObjects`. Since the shm-stats file is fixed-size and can't be resized on reload, a large fixed value prevents reload failures when new ingresses are added. HAProxy allocates object slots lazily, so memory overhead is proportional to actual objects, not the configured maximum.

When `shmStats` is enabled, the chart automatically adds a `/dev/shm` emptyDir volume with `medium: Memory` to the HAProxy pod. The volume's `sizeLimit` is auto-calculated from `maxObjects` (~4KB per object with 10% margin), or can be overridden via `haproxy.shmStats.shmSizeLimit`. This volume counts against the pod's memory limit.

### Address discovery

HAPTIC publishes addresses from the matching HAProxy Services to Ingress and
Gateway status. It combines and deduplicates their LoadBalancer addresses. If
none are assigned, it uses their cluster IPs; these aren't public addresses.

Set `controller.config.templatingSettings.extraContext.statusPatches.enabled: false`
to stop status updates for all managed routes, for example while controlling a
[DNS cutover](../migrating.md#control-the-dns-cutover).

### Status-patch extension point

The `status-patches-*` extension point renders at priority 200 — after feature analysis (`features-*` at 050-150) but before backends and frontends (500+). This ensures status patches are captured even when later config generation fails, allowing the `renderFailed` variant to be applied.

Status patch snippets produce no HAProxy configuration output. They call `statusPatch()` as a side effect to register patches for later application by the controller.

<a id="declarative-kubernetes-resources-k8sresourceshaproxy-service"></a>

### HAProxy Service

HAPTIC creates the user-facing HAProxy Service after its first successful render.
The Service is removed when its owning configuration is deleted, including on
`helm uninstall`.

Use [`haproxy.service`](../reference.md#haproxy-service) values to change the
Service type, addresses, and ports. Set a default port to `0` to remove it, or
add ports under `haproxy.service.extraPorts` for your own frontends. Declaring a
Service port alone doesn't create a listening HAProxy frontend.

Gateway listeners add their ports automatically. Gateways with `spec.addresses`
receive a dedicated Service; see [Gateway address handling](gateway.md).

## Map files

The base library generates these map files for routing:

| Map File | Purpose | Matcher |
|----------|---------|---------|
| host.map | Host header to group mapping | Exact match |
| host-regex.map | Regex hostname fallback (multi-label hosts under a wildcard listener) | `map_reg()` |
| host-alias-regex.map | Host alias regular expressions, each anchored to the whole hostname | `map_reg()` |
| path-exact.map | Exact path matching | `map()` |
| path-prefix-exact.map | Prefix paths that should match exactly | `map()` |
| path-prefix.map | Prefix path matching | `map_beg()` |
| path-regex.map | Regex path matching | `map_reg()` |
| weighted-multi-backend.map | Weighted backend selection | `map()` |

And these per-backend feature maps, all keyed by backend name and looked up with `map()`:

| Map File | Purpose |
|----------|---------|
| body-size.map | Request body-size limit in bytes |
| request-buffering.map | Per-route request-buffering override (`on`/`off`) |
| reqhdr-host.map | Upstream `Host` header override (URL-encoded) |
| reqhdr-xfwd-prefix.map | `X-Forwarded-Prefix` header value (URL-encoded) |
| reqhdr-connection.map | `Connection` header override (URL-encoded) |
| path-rewrite.map | Literal full-path rewrite (URL-encoded) |
| backend-timeouts.map | Settable server/tunnel timeouts, keyed <code>&lt;backend&gt;&#124;server</code> / <code>&lt;backend&gt;&#124;tunnel</code>, integer milliseconds |
| backend-service.map | `<backend>` to `<namespace>/<service>`, read at log time (keyed by `var(txn.backend_name)`) for the `namespace`/`service` access-log fields — keeps them off the backend section so it stays dynamic |
| ing-reqhdr.map | Ingress request-header modifiers, keyed <code>&lt;backend&gt;&#124;&lt;op&gt;&#124;&lt;name&gt;</code> (op ∈ set/add/del), value URL-encoded (`1` for del) |
| ing-reshdr.map | Ingress response-header modifiers, keyed <code>&lt;backend&gt;&#124;&lt;op&gt;&#124;&lt;name&gt;</code>, value URL-encoded (`1` for del) |

Values a request-time reader takes from a map are URL-encoded by the writer
(`queryEscape`) and decoded with `url_dec(1)`, so a space, `;` or `%` in a value
can neither split the map line on the runtime CLI nor be re-read as a log-format
fetch. See [Reload-free routing](reload-free.md).

### Writing a map from a library

`RegisterMap` writes a map file and declares whether the order of its entries is
load-bearing. It returns the path the configuration references the file by:

```scriggo
{%- import "util-register-map" for RegisterMap -%}
{%- var lines = []string{"# <backend> -> upstream host"} %}
{%- for _, e := range entries %}
  {%- lines = append(lines, tostring(e | dig("backend")) + " " + queryEscape(tostring(e | dig("host")))) %}
{%- end %}
{%- var path = RegisterMap("my-feature.map", lines, map[string]any{"ordered": false}) %}
http-request set-header Host %[var(txn.backend_name),map({{ path }}),url_dec(1)] if { var(txn.backend_name),map({{ path }}) -m found }
```

Three rules make the difference between a map that deploys without a reload and
one that doesn't:

- **Register it even when it has no entries.** Creating a map file on the first
  entry, and deleting it with the last, both change `haproxy.cfg` and reload
  HAProxy. Pass a header comment as the first line so an empty file is still
  self-describing.
- **Declare `ordered: false`** when the configuration reads the map with
  `map_str`, `map_beg`, `map_ip` or `map_str_int`. Those find a key by its own
  value, so a new entry can be appended over the runtime API. Leave the default
  `true` for `map_reg`, `map_sub`, `map_dom`, `map_dir` and `map_end`, which
  HAProxy evaluates as a first-match-wins list — an appended entry there would
  silently never match. Declaring it wrong in either direction is silent, so a
  static map declares it through [`spec.maps.<name>.ordered`](../crd-reference.md#maps)
  instead and the two can never disagree about one file.
- **URL-encode any value that can carry a space, a `;` or a `%`**, with
  `queryEscape` on the way in and `url_dec(1)` on the way out. The runtime CLI
  splits a map value at the first space and truncates it at a `;`, and a value
  inlined into a `set-header` directive is re-read as a log-format string, where
  a `%` fetches request state.

The caller owns the order of `lines`, because only it knows whether that order is
cosmetic — sort it, or the next render's Go map iteration produces a different
file and costs a sync for a configuration nobody changed.

### One line per header name: `HeaderModifierRules`

`HeaderModifierRules(direction, keyExpr, mapPath, setNames, addNames, delNames)`
emits one `set-header` / `add-header` / `del-header` line per distinct header
*name*, each reading its value from a map keyed `<key>|<operation>|<name>`. A
resource that modifies a header name some other resource already uses adds a map
entry and no configuration line at all:

```scriggo
{%- import "util-header-modifier-rules" for HeaderModifierRules -%}
{{- HeaderModifierRules("request", "var(txn.backend_name)", mapPath,
      setNames, addNames, delNames) -}}
```

A name lands unquoted in the emitted directive, so the macro drops anything
outside `[A-Za-z0-9!$&*+.^_~-]` rather than emitting it — a quote in a header
name leaves the emitted directive unbalanced, and HAProxy refuses the whole
configuration. Reject the name against the same charset in your own library and report it,
or the tenant sees a header silently not applied.

`direction` is `request` or `response`. `response` emits `http-after-response` rules, so the header also reaches responses HAProxy generates or replaces, such as error pages and redirects. `keyExpr` is the sample expression
producing the key prefix; the macro appends `,concat(|<operation>|<name>)` to it,
so a caller needing more in the key ends its own expression with a `concat` (the
Gateway library's backendRef-level form passes
`var(txn.gw_rule_id),concat(|,txn.backend_name,)`). The three name lists carry the
spelling to emit, already deduplicated by lower-case name — the key always uses
the lower-case form, because HTTP header names are case-insensitive and two
resources spelling one header differently must share a line.

<a id="haproxy-configuration-structure"></a>

To inspect the complete generated configuration, run
`haptic config view --namespace haptic`, or open the example at the top of this page.
Use the [extension-point table](#available-extension-points) to locate where your
snippet belongs.

## See also

- [Template Libraries Overview](../template-libraries.md) - How template libraries work
- [SSL Library](ssl.md) - TLS certificate management and HTTPS frontend
- [Template Libraries → Path Matching Order](../template-libraries.md#path-matching-order) - Switching path-matching order
