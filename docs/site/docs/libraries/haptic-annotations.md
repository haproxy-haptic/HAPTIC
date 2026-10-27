# Native Ingress annotations

<a id="haptic-annotations-library"></a>

Set routing, authentication, and response behavior with `haproxy-haptic.org/*`
annotations on your Ingresses. These bundled templates are enabled by default.
For annotations from another controller, use its [compatibility library](../annotation-compatibility.md).

Put annotations under `metadata.annotations` and quote their values as strings.
For a complete manifest and apply steps, see [using annotations](../annotations.md).
An annotation that fails rendering causes its resource change to be rejected;
[independent valid updates continue](../operations/input-isolation.md).

<a id="overview"></a>

## Try an annotation

Edit the example to see how native annotations change HAProxy configuration:

<div class="pg-embed" markdown data-scenario="haptic-annotations" data-input="resources" data-input-focus="haproxy-haptic.org/load-balance" data-output-focus="balance leastconn" data-tab="haproxy.cfg" data-controls="tabs,resources" data-title="Choose a load-balancing algorithm" data-height="440">

<p class="pg-task" markdown>In the **Resources** panel, change the `shop` Ingress's `haproxy-haptic.org/load-balance` from `leastconn` to `roundrobin`. Watch the `balance` setting change in `haproxy.cfg`.</p>

</div>

## Annotation reference

### Path and host matching

Match request paths and add hostnames to an existing route.

| Annotation | Behavior |
|------------|----------|
| `haproxy-haptic.org/path-type` | Overrides how the path matches when the Ingress `pathType` is `ImplementationSpecific`: `regex`, `exact`, `prefix` (trailing slash normalized), or `begin`. A `regex` path matches from the start of the path; HAPTIC rejects one whose groups or `[ ]` classes don't close within the path. |
| `haproxy-haptic.org/host-alias` | Adds comma- or space-separated hostnames that use the primary host's routes. Each must be a valid hostname, optionally starting with `*.`; case doesn't matter. A hostname an older Ingress already claims stays with that Ingress; see [Host aliases claim hostnames](../operations/security.md#host-aliases-claim-hostnames). |
| `haproxy-haptic.org/host-alias-regex` | Adds a regular expression that must match the whole hostname, for example `app[0-9]+\.example\.com`. Matching hostnames use the primary host's routes when no rule host, wildcard host, or exact alias matches. HAPTIC rejects a pattern whose groups or `[ ]` classes don't close within it. |

### Backend tuning

Set backend timeouts, load balancing, connection limits, and health checks.

| Annotation | Behavior |
|------------|----------|
| `haproxy-haptic.org/agent-check-addr` | Sets the agent-check address via `agent-addr`; requires `agent-check-port`. |
| `haproxy-haptic.org/agent-check-interval` | Sets the agent-check interval via `agent-inter`; requires `agent-check-port`. |
| `haproxy-haptic.org/agent-check-port` | Enables the agent check on the given port (1-65535) via `agent-check` and `agent-port`; required by the other `agent-check-*` keys. |
| `haproxy-haptic.org/agent-check-send` | Sets the string sent to the agent check via `agent-send`; requires `agent-check-port`. |
| `haproxy-haptic.org/check` | Toggles server health checks; `off` emits `no-check` so servers aren't health-checked. |
| `haproxy-haptic.org/config-backend` | Injects raw, operator-authored HAProxy directives verbatim into the `backend` section. Rejected unless [`ingressPermissions.allowRawHAProxyConfig`](../operations/security.md#raw-configuration-annotations) is `true`. |
| `haproxy-haptic.org/fullconn` | Emits `fullconn <n>` on the backend. HAProxy uses this threshold to scale each server's `minconn`/`maxconn` range as backend load rises. For a hard per-server cap, use `maxconn-server`. |
| `haproxy-haptic.org/health-check-fall` | Sets the failed-check count before a server is marked down via `fall`. |
| `haproxy-haptic.org/health-check-interval` | Sets the health-check interval via `inter`; ignored when `check` is `off`. |
| `haproxy-haptic.org/health-check-port` | Sets the health-check port (1-65535) via `port`. |
| `haproxy-haptic.org/health-check-rise` | Sets the successful-check count before a server is marked up via `rise`. |
| `haproxy-haptic.org/health-check-uri` | Enables HTTP health checks via `option httpchk`; a bare path becomes `GET <path>`, and a value containing a space is used verbatim. |
| `haproxy-haptic.org/initial-weight` | Sets the initial server weight (0-256) via `weight`. |
| `haproxy-haptic.org/load-balance` | Sets the backend `balance` algorithm: `roundrobin`, `static-rr`, `leastconn`, `first`, `source`, `random`, or a parameterized `uri`, `url_param(<name>)`, `hdr(<name>)`, or `rdp-cookie(<name>)`; an invalid value fails the render. |
| `haproxy-haptic.org/maxconn-server` | Sets the per-server maximum concurrent connections via `maxconn`. |
| `haproxy-haptic.org/maxqueue-server` | Sets the per-server maximum queued connections via `maxqueue`. |
| `haproxy-haptic.org/pod-maxconn` | Sets a cluster-wide connection budget, divided across the ready HAProxy pods and rounded up to a power of two, then applied as each server's `maxconn`. |
| `haproxy-haptic.org/proxy-protocol` | Sends the PROXY protocol header to servers: `proxy`/`proxy-v1` emit `send-proxy`, and `proxy-v2`, `proxy-v2-ssl`, `proxy-v2-ssl-cn` emit the matching `send-proxy-v2` variant; any other value fails the render. |
| `haproxy-haptic.org/timeout-check` | Sets the check timeout via `timeout check`. |
| `haproxy-haptic.org/timeout-connect` | Sets the connect timeout via `timeout connect`. |
| `haproxy-haptic.org/timeout-http-request` | Sets the request timeout via `timeout http-request`. |
| `haproxy-haptic.org/timeout-keep-alive` | Sets the keep-alive timeout via `timeout http-keep-alive`. |
| `haproxy-haptic.org/timeout-queue` | Sets the queue timeout via `timeout queue`. |
| `haproxy-haptic.org/timeout-server` | Sets the server timeout. Updating an existing route's timeout doesn't require a reload. |
| `haproxy-haptic.org/timeout-tunnel` | Sets the tunnel timeout. Updating an existing route's timeout doesn't require a reload. |
| `haproxy-haptic.org/consistent-hash-by` | Configures consistent hashing on the backend, emitting a `balance` directive plus `hash-type consistent`. Accepts a hash key: `uri`, `source`, `$http_<name>`, `$arg_<name>`, or `$cookie_<name>`; any other value is used verbatim as a HAProxy fetch expression via `balance hash <value>`. |

### Backend TLS (to the upstream)

Speak TLS to the backend Service — protocol, verification, client certs, SNI, ciphers.

| Annotation | Behavior |
|------------|----------|
| `haproxy-haptic.org/backend-ca-secret` | Loads the Secret's `ca.crt` as the backend `ca-file` and requires TLS verification; a missing Secret or key is skipped with a warning comment. |
| `haproxy-haptic.org/backend-ciphers` | Sets the cipher list for TLS 1.2 and earlier via `ciphers` on a TLS-enabled backend. |
| `haproxy-haptic.org/backend-ciphersuites` | Sets the cipher suites for TLS 1.3 via `ciphersuites` on a TLS-enabled backend. |
| `haproxy-haptic.org/backend-crt-secret` | Presents the Secret's `tls.crt` and `tls.key` as a client certificate to the upstream via `crt`; a missing Secret is skipped with a warning. |
| `haproxy-haptic.org/backend-protocol` | Selects the upstream protocol from `h1`, `h2`, `h1-ssl`, `h2-ssl`, `http`, `https`, `grpc`, or `grpcs`; the `h2`, `grpc`, `h2-ssl`, and `grpcs` values add `proto h2`, and `h1-ssl`, `https`, `h2-ssl`, and `grpcs` speak TLS to the upstream. |
| `haproxy-haptic.org/backend-sni` | Sets the SNI sent to the upstream: `host` or `sni` forwards the request Host via `sni req.hdr(host)`, and any other value is sent literally via `sni str(<value>)`. |
| `haproxy-haptic.org/backend-ssl-protocols` | Maps a space-separated TLS version list to `ssl-min-ver` (lowest) and `ssl-max-ver` (highest). HAProxy expresses only a contiguous span, so a gap in the list (for example, skipping `TLSv1.2`) can't be represented. |
| `haproxy-haptic.org/backend-verify` | A truthy value (`on`, `true`, `yes`, `1`) requires upstream certificate verification, and fails closed rather than silently downgrading to `verify none` when no CA is available. |
| `haproxy-haptic.org/backend-verify-host` | Sets the expected upstream certificate hostname via `verifyhost`, independent of the SNI value. |

### Rate and bandwidth limiting

Per-source request-rate caps (reload-surviving stick-tables), shared fleet-wide request budgets through the rate-limit SPOA plugin, and download/upload bandwidth throttling.

Limiters run before cache lookup, so cache hits consume the same budget as origin
requests. Per-source caps and shared bandwidth limits are keyed by route and
client and can coexist. Limit values and allowlists live in maps; updates avoid
a reload unless a new rate window, denial status, or shared-scope rate requires
a new frontend directive.

Bandwidth limits have two scopes to consider:

- **The limit applies per stream, not per connection.** An HTTP/2 or HTTP/3 client that opens ten streams gets ten times the configured rate. Use `bandwidth-limit-scope: client` when you want one budget per client regardless of how many streams it opens.
- **Only the HTTP payload is metered.** Headers are never counted toward the limit.

| Annotation | Behavior |
|------------|----------|
| `haproxy-haptic.org/download-bandwidth-limit` | Caps the bytes per second sent toward the client, using a `bwlim-out` filter plus `http-request set-bandwidth-limit`. Independent of the request-rate caps; both can apply to the same Ingress. Byte-size values are validated before interpolation. |
| `haproxy-haptic.org/upload-bandwidth-limit` | Caps the bytes per second received from the client, using a `bwlim-in` filter. Can be combined with `download-bandwidth-limit`; each direction gets its own filter. Byte-size values are validated before interpolation. |
| `haproxy-haptic.org/bandwidth-limit-scope` | Who shares the budget: `stream` (default, each stream gets the full limit), `client` (all streams from one source IP share it), or `service` (every stream of this route shares it). The first route using a new shared rate requires a reload. `service` scopes to one Ingress route to a service, not to a Kubernetes Service shared by several Ingresses. |
| `haproxy-haptic.org/rate-limit-algorithm` | Shared limiter algorithm: `token-bucket` (default, low-latency lease mode) or `gcra` (exact mode, one synchronous store check per request). `gcra` is for low-volume contractual limits; use the default token-bucket mode for public-edge DoS protection. During a store failure, both modes follow `rateLimit.shared.failClosed`. Requires `rate-limit-requests`, `rateLimit.shared.enabled=true`, and an effective Redis/Valkey store endpoint. |
| `haproxy-haptic.org/rate-limit-burst` | Shared limiter burst allowance; defaults to `rate-limit-requests`. Must be a positive integer. |
| `haproxy-haptic.org/rate-limit-connections` | Caps concurrent connections per source IP; ignored when `rate-limit-rps` or `rate-limit-rpm` is set. |
| `haproxy-haptic.org/rate-limit-key` | Shared limiter key dimension: `ip` (default) or `consumer`. Source-IP limits run in the frontend before Coraza and request-schema validation, making them the correct DoS guard. Consumer limits run on the same frontend after the API-key/JWT rules have established the identity, falling back to source IP when no identity is present; use them for authenticated quotas, not as the sole public-edge flood control. |
| `haproxy-haptic.org/rate-limit-period` | Overrides the rate window. For the per-pod stick-table limiter, when unset the window derives from the active cap: 1 second for requests per second, 60 seconds for requests per minute, and a 30-second table TTL for connection caps. For the shared limiter it defaults to `1s` and accepts `ms`/`s`/`m`/`h`/`d`; zero or malformed values fail the render. The shared rule's full refill horizon (`burst × period / requests`) must not exceed 3600 seconds, the bundled plugin's maximum safe state TTL. |
| `haproxy-haptic.org/rate-limit-requests` | Fleet-wide request budget. Requires `rateLimit.shared.enabled: true` and the managed Valkey/Sentinel store or one external store endpoint. Store failure uses per-sidecar emergency limits by default; `rateLimit.shared.failClosed: true` denies requests instead. See [shared rate limiting](../operations/spoa-hub.md#managed-shared-rate-limit-store) for setup and failure behavior. |
| `haproxy-haptic.org/rate-limit-rpm` | Caps requests per minute per source IP (a 60-second `http_req_rate` window); ignored when `rate-limit-rps` is also set. |
| `haproxy-haptic.org/rate-limit-rps` | Caps requests per second per source IP via an `http_req_rate` stick-table; requests over the cap are rejected with the deny status (default `429`), with no burst allowance. |
| `haproxy-haptic.org/rate-limit-size` | Sets the stick-table size (default `100k`); routes sharing a rate window share one table, sized to the largest value any of them asks for. |
| `haproxy-haptic.org/rate-limit-status-code` | Sets the HTTP status returned to rejected requests (default 429). Only a status HAProxy has a built-in error page for is accepted (200, 400, 401, 403, 404, 405, 407, 408, 410, 413, 414, 425, 429, 431, 500 to 504); it becomes the `http-request deny deny_status` code. |
| `haproxy-haptic.org/rate-limit-allowlist` | Exempts comma-separated CIDRs (IPv4 or IPv6) from the route's rate limit, per-pod or shared; invalid CIDRs fail the render, and an allowlist on a route with no rate limit is refused at admission (a Warning Event on reconcile). Changing the allowlist doesn't require a reload. |

### Compression

Response compression is opt-in. Enable it for an Ingress whose responses are
suitable for compression, such as a public static-asset service:

```yaml
metadata:
  annotations:
    haproxy-haptic.org/compress-enable: "true"
    haproxy-haptic.org/compress-types: "text/css,application/javascript,image/svg+xml"
```

| Annotation | Behavior |
|------------|----------|
| `haproxy-haptic.org/compress-enable` | `true` enables HAProxy response compression for this Ingress; unset or `false` leaves it off. |
| `haproxy-haptic.org/compress-algorithm` | Algorithm: `gzip` (default), `deflate`, or `raw-deflate`. The community HAProxy build doesn't provide `brotli` or `zstd`; selecting either fails rendering. |
| `haproxy-haptic.org/compress-types` | Comma-separated MIME types. Defaults to text, JSON, XML, and SVG types when compression is enabled. |

!!! note "Choose routes that can be compressed safely"
    A response containing both a secret and attacker-controlled input can leak
    that secret through its compressed size, even over HTTPS ([BREACH](https://www.breachattack.com/)).
    Keep compression off for those routes unless the application mitigates this
    attack. Disabling it in HAPTIC doesn't disable compression in the application
    or a proxy in front of HAPTIC.

HAProxy leaves already-compressed responses and responses marked
`Cache-Control: no-transform` unchanged. It compresses only matching content types
and algorithms accepted by the client, and adds `Vary: Accept-Encoding`.
Compression runs before download bandwidth limits, which count the compressed bytes.

See [response compression limits](../operations/performance.md#response-compression)
for CPU controls and [reload behavior](reload-free.md) for configuration changes.

### Shared response cache

Serve repeated GET and HEAD requests from [Varnish](https://varnish-cache.org/)
to reduce work on your application servers. First [enable the shared cache](../operations/response-cache.md),
then set `haproxy-haptic.org/cache-enable: "true"` on the Ingress.

For authenticated responses, use `cache-key: consumer` so each identity has a
separate cache entry. Other keys don't enable caching for requests with
`Authorization` or `Cookie` headers. See [cache keys and authentication](../operations/response-cache.md#cache-keys-and-authentication).

| Annotation | Behavior |
|------------|----------|
| `haproxy-haptic.org/cache-enable` | The value `true` routes GET/HEAD requests through healthy Varnish shards. Other methods and all-shards-unhealthy periods use the application backend directly. |
| `haproxy-haptic.org/cache-exclude-content-types` | Comma-separated response media types never cached (for example `text/html`); matched after stripping the `; charset=…` suffix. |
| `haproxy-haptic.org/cache-exclude-paths` | Comma-separated request path prefixes that bypass the cache and go straight to the app. Changing these prefixes doesn't require a reload. A <code>&#124;</code> in a path is refused. |
| `haproxy-haptic.org/cache-key` | Adds a vary component to the cache key: `consumer`, `src`, `header:<h>`, `cookie:<c>`, `query:<q>`, or a comma-separated composite. Only `consumer` lets a route cache responses to **authenticated** requests, because it identifies the caller; see [cache keys](../operations/response-cache.md#cache-keys-and-authentication). The same variance is declared to caches downstream of HAPTIC. |
| `haproxy-haptic.org/cache-negative-ttl` | Seconds to cache `404` and `410` responses independently of `cache-ttl`. Keep it short: a cached failure remains visible until expiry. When unset, routes with `cache-ttl` (including `auto`) don't cache these statuses. With both TTL annotations unset, Varnish follows origin headers and its built-in defaults. |
| `haproxy-haptic.org/cache-max-object-size` | Maximum cacheable response size in bytes; a larger response (by `Content-Length`) stays uncacheable. |
| `haproxy-haptic.org/cache-revalidate` | Seconds past expiry the object is kept so the refresh can be a conditional request the origin answers with `304 Not Modified` instead of a full body. Costs cache memory; defaults to `0`, which keeps nothing. |
| `haproxy-haptic.org/cache-stale-if-error` | Seconds past expiry a stale response may still be served, but **only** when the refresh fails. On its own it doesn't change what an ordinary expiry does: that still fetches from the origin and waits. An origin error never replaces a good cached response. |
| `haproxy-haptic.org/cache-stale-while-revalidate` | Seconds past expiry a stale response is served immediately while the cache refreshes it in the background. Without it, a route gets the cache's 10-second default. |
| `haproxy-haptic.org/cache-strip-set-cookie` | The value `true` drops `Set-Cookie` from the response before the cache decides whether it can be stored, so a public asset behind an analytics cookie stays cacheable. Never set it where `Set-Cookie` carries a session. |
| `haproxy-haptic.org/cache-ttl` | Cache lifetime in seconds for `200` responses; `auto` follows the origin's `Cache-Control` and `Expires`. Responses with `Set-Cookie`, `Cache-Control: no-cache`/`no-store`/`private`, or `Vary: *` aren't stored. Use `cache-negative-ttl` for `404` and `410`. |

### Rewriting, retries, and session affinity

Path/target rewriting, body-size limits, upstream retries, Host/header overrides, and cookie-based stickiness.

| Annotation | Behavior |
|------------|----------|
| `haproxy-haptic.org/affinity` | The value `cookie` enables cookie-based session affinity via the backend `cookie` directive. |
| `haproxy-haptic.org/backend-connection-header` | Overrides the `Connection` header sent to the backend server. |
| `haproxy-haptic.org/path-rewrite` | Rewrites the request path: a `<from> <to>` pair replaces the whole path with `<to>` wherever `<from>` matches (`\1`..`\9` carry captured groups), and a bare value replaces it unconditionally. A bare value, or a prefix strip (`^<prefix>(.*)` with `<new prefix>\1` or a plain `<new path>` as `<to>`, `<prefix>` without regex metacharacters), is applied from a per-route map and keeps the route reload-free; any other pattern is a `replace-path` rule in the backend. |
| `haproxy-haptic.org/max-request-body-size` | Limits the request body size (accepts `k`, `m`, or `g` suffixes), returning `413` when exceeded; `0` means unlimited. |
| `haproxy-haptic.org/request-buffering` | `on` or `off`, overriding the fleet-wide `requestBuffering.enabled` default for this route. See [Request buffering](#request-buffering). |
| `haproxy-haptic.org/retry-on` | Sets the conditions under which HAProxy retries a failed request against the next server, emitting `retry-on`. Conditions cover connection failures, response timeouts, malformed responses, and per-status-code retries (`http_<code>`); a disable value emits `retries 0`. `option redispatch` in defaults sends the retry to a different server. |
| `haproxy-haptic.org/retries` | Sets the number of retry attempts against backend servers via HAProxy `retries`; `0` keeps the default. |
| `haproxy-haptic.org/session-cookie-domain` | Sets the session cookie's `Domain` via the `domain` cookie keyword. |
| `haproxy-haptic.org/session-cookie-dynamic` | Enables dynamically generated cookie values via the `dynamic` cookie keyword (default on). |
| `haproxy-haptic.org/session-cookie-keywords` | Appends extra keywords to the `cookie` directive verbatim (for example, `httponly`). |
| `haproxy-haptic.org/session-cookie-max-age` | Sets the browser cookie lifetime in seconds via `attr Max-Age`; this is the cookie's `Max-Age`, not HAProxy's server-affinity lifetime. |
| `haproxy-haptic.org/session-cookie-name` | Sets the session cookie name (default `INGRESSCOOKIE`). |
| `haproxy-haptic.org/session-cookie-path` | Sets the session cookie's `Path` via `attr Path`. |
| `haproxy-haptic.org/session-cookie-preserve` | The value `true` adds the `preserve` keyword to the `cookie` directive. |
| `haproxy-haptic.org/session-cookie-samesite` | Sets the cookie `SameSite` attribute (`None`, `Lax`, or `Strict`) via `attr SameSite`. |
| `haproxy-haptic.org/session-cookie-secure` | The value `true` sets the cookie `Secure` attribute via `attr Secure`. |
| `haproxy-haptic.org/session-cookie-strategy` | Selects the cookie mode: `insert` (default), `rewrite`, or `prefix`; `insert` and `prefix` add `indirect nocache`. |
| `haproxy-haptic.org/set-host` | Overrides the `Host` header sent to the upstream. |
| `haproxy-haptic.org/x-forwarded-prefix` | Sets the `X-Forwarded-Prefix` header sent to the upstream. |

#### Request buffering

HAProxy buffers request bodies by default, so a client that trickles its upload holds an HAProxy buffer instead of a backend server slot. Set `haproxy-haptic.org/request-buffering` to change that for one route:

```yaml
metadata:
  annotations:
    haproxy-haptic.org/request-buffering: "off"
```

Use `off` when the route's clients declare a `Content-Length` but still expect a response before the request body ends, such as a resumable-upload endpoint. Use `on` to buffer one route while [`requestBuffering.enabled`](base.md#request-buffering) is `false` fleet-wide.

Requests without `Content-Length` stream directly to the backend. Requests with
a declared length can be buffered even if the application expects streaming;
use `off` for those routes. See [request buffering](base.md#request-buffering)
for the wait timeout and size limits.

### Headers, CORS, and access control

Request/response header manipulation, capture, CORS, source-IP allow/deny, and upstream cookie/redirect rewriting.

| Annotation | Behavior |
|------------|----------|
| `haproxy-haptic.org/allowlist-source-range` | Allows only the listed CIDRs and denies all other source IPs for the host. |
| `haproxy-haptic.org/cors-allow-credentials` | Sets `Access-Control-Allow-Credentials: true` when enabled. |
| `haproxy-haptic.org/cors-allow-headers` | Sets the `Access-Control-Allow-Headers` response header. |
| `haproxy-haptic.org/cors-allow-methods` | Sets the `Access-Control-Allow-Methods` response header. |
| `haproxy-haptic.org/cors-allow-origin` | Sets the allowed origins (comma-separated, with a single-level `*.` wildcard); the matching request `Origin` is echoed back (default `*`). |
| `haproxy-haptic.org/cors-enable` | Enables CORS response headers and answers `OPTIONS` requests with `204`; the headers come from per-route maps, so the route stays reload-free. |
| `haproxy-haptic.org/cors-expose-headers` | Sets the `Access-Control-Expose-Headers` response header. |
| `haproxy-haptic.org/cors-max-age` | Sets the `Access-Control-Max-Age` response header (default `86400`). |
| `haproxy-haptic.org/denylist-source-range` | Denies the listed CIDRs and allows all other source IPs for the host. |
| `haproxy-haptic.org/forwardfor` | Overrides the [default `X-Forwarded-For` handling](../operations/security.md#forwarded-headers) for the route: `add` keeps the client's value and appends the client address, `update` replaces it with the client address even from a trusted proxy, `ifmissing` keeps it or sets the client address when there is none, and `ignore` forwards it unchanged. Any other value fails the render. Applied from a per-route map. |
| `haproxy-haptic.org/response-cookie-domain` | Rewrites the `Domain` attribute of upstream `Set-Cookie` response headers, given a `<from> <to>` pair, preserving the rest of the cookie string. Host-scoped; a wrong-arity value fails the render. Applied from a per-route map; the first route with a new pair reloads once. |
| `haproxy-haptic.org/response-cookie-path` | Rewrites the `Path` attribute of upstream `Set-Cookie` response headers, given a `<from> <to>` pair, preserving the rest of the cookie string. Host-scoped; a wrong-arity value fails the render. Applied from a per-route map; the first route with a new pair reloads once. |
| `haproxy-haptic.org/response-location-rewrite-from` | Names the literal text to match in the `Location` and `Refresh` response headers; the matched text is regex-escaped and replaced with the value of `response-location-rewrite-to`. Host-scoped. Applied from a per-route map; the first route with a new pair reloads once. |
| `haproxy-haptic.org/response-location-rewrite-to` | Supplies the replacement text for `response-location-rewrite-from`; required whenever a match pattern is set, or the render fails. |
| `haproxy-haptic.org/request-capture` | Captures the named request headers (newline-separated) in the logs via `capture request header`, across the whole frontend; each header and length pair is emitted once, so a route sharing a known pair is reload-free. |
| `haproxy-haptic.org/request-capture-len` | Sets the capture length for `request-capture` (default `128`). |
| `haproxy-haptic.org/request-set-header` | Sets request headers sent to the upstream, one `<name> <value>` per line. Changing a value avoids a reload; the first use of a new header name requires one. |
| `haproxy-haptic.org/response-set-header` | Sets response headers, one `<name> <value>` per line. Changing a value avoids a reload; the first use of a new header name requires one. |
| `haproxy-haptic.org/src-ip-header` | Derives the client source IP from the named request header via `http-request set-src`, from a per-route map; the first route to name a new header reloads once. Naming `X-Forwarded-For` also keeps the client's `X-Forwarded-For` for the route, as `forwardfor: add` does, unless the route sets `forwardfor`. `X-Real-IP`, `X-Client-IP`, and `Client-IP` must come from a listed trusted proxy or an operator snippet; this annotation doesn't exempt them from header filtering. |

### Canary and traffic mirroring

Header/cookie/weight-based canary routing and request mirroring via the SPOA hub.

| Annotation | Behavior |
|------------|----------|
| `haproxy-haptic.org/canary` | Marks the Ingress as a canary for a host owned by another Ingress, overlaying a `use_backend` split instead of owning the route. |
| `haproxy-haptic.org/canary-by-cookie` | Routes to the canary backend when the named cookie is present. |
| `haproxy-haptic.org/canary-by-header` | Routes to the canary backend when the named header is present. |
| `haproxy-haptic.org/canary-by-header-pattern` | Routes to the canary backend when the named header matches this regular expression; takes precedence over `canary-by-header-value`. |
| `haproxy-haptic.org/canary-by-header-value` | Routes to the canary backend only when the named header equals this value. |
| `haproxy-haptic.org/canary-weight` | Sends a percentage of traffic (an integer 0-100) to the canary backend via a weighted random split. |
| `haproxy-haptic.org/mirror-target` | Mirrors requests fire-and-forget to a `scheme://host[:port]` target through the SPOA hub's mirror plugin, buffering the request body via `option http-buffer-request`; requires the mirror plugin and a host on the rule. |

### Redirects, HSTS, passthrough, and config injection

HTTP→HTTPS and host redirects (reload-free maps), HSTS, SSL passthrough, a default backend, and raw section injection.

| Annotation | Behavior |
|------------|----------|
| `haproxy-haptic.org/root-redirect` | Redirects requests for the host root path (`/`) to the given sub-path. |
| `haproxy-haptic.org/config-defaults` | Injects raw operator-authored directives verbatim into the `defaults` section. Rejected unless [`ingressPermissions.allowRawHAProxyConfig`](../operations/security.md#raw-configuration-annotations) is `true`. |
| `haproxy-haptic.org/config-frontend` | Injects raw operator-authored directives into every frontend, before routing. Rejected unless [`ingressPermissions.allowRawHAProxyConfig`](../operations/security.md#raw-configuration-annotations) is `true`. |
| `haproxy-haptic.org/config-global` | Injects raw operator-authored HAProxy directives verbatim into the `global` section. Rejected unless [`ingressPermissions.allowRawHAProxyConfig`](../operations/security.md#raw-configuration-annotations) is `true`. |
| `haproxy-haptic.org/default-backend` | Routes requests that match the host but none of its configured paths to a named Service as a catch-all backend pool, using the Service's first port. Produces no backend, and no error, when the Service or its first port can't be resolved. |
| `haproxy-haptic.org/default-backend-redirect` | Redirects requests that match the host but no path to the given URL. |
| `haproxy-haptic.org/default-backend-redirect-code` | Sets the status code for `default-backend-redirect` (default `302`). |
| `haproxy-haptic.org/apex-www-redirect` | Issues a `301` redirect between the apex domain and its `www` subdomain, in both directions, preserving the request path and scheme. |
| `haproxy-haptic.org/hsts` | Enables HSTS by adding the `Strict-Transport-Security` response header for the host. |
| `haproxy-haptic.org/hsts-include-subdomains` | Appends `includeSubDomains` to the `Strict-Transport-Security` header when set to `true`. |
| `haproxy-haptic.org/hsts-max-age` | Sets the HSTS `max-age` in seconds (default: `controller.config.templatingSettings.extraContext.tls.hsts.maxAge`, `31536000`). |
| `haproxy-haptic.org/hsts-preload` | Appends `preload` to the `Strict-Transport-Security` header when set to `true`. |
| `haproxy-haptic.org/permanent-redirect` | Redirects the host to the given URL with a permanent status code (default `301`). |
| `haproxy-haptic.org/permanent-redirect-code` | Sets the status code for `permanent-redirect` (default `301`). |
| `haproxy-haptic.org/ssl-passthrough` | Passes TLS through to the backend without terminating it, routed by SNI on a dedicated TCP frontend. |
| `haproxy-haptic.org/https-redirect` | Redirects plain HTTP requests for the host to HTTPS. Hosts that also set `https-redirect-port` are handled there instead, avoiding a double redirect. |
| `haproxy-haptic.org/https-redirect-code` | Sets the HTTP-to-HTTPS redirect status code (`301`, `302`, `303`, `307`, or `308`; default `302`). |
| `haproxy-haptic.org/https-redirect-port` | Redirects plain HTTP requests to HTTPS on an explicit port, preserving the request URI. |
| `haproxy-haptic.org/temporary-redirect` | Redirects the host to the given URL with a temporary status code (default `302`). |
| `haproxy-haptic.org/temporary-redirect-code` | Sets the status code for `temporary-redirect` (default `302`). |

### Authentication, mTLS, and WAF

Basic auth, client-certificate verification, external/forward auth, OAuth2-proxy, and the Coraza WAF via the SPOA hub.

| Annotation | Behavior |
|------------|----------|
| `haproxy-haptic.org/auth-headers-fail` | Adds response headers on failed external authentication via `http-after-response set-header`. |
| `haproxy-haptic.org/auth-headers-request` | Lists the request headers forwarded to the external authentication service. Names containing `_` aren't forwarded: admission rejects them, and an existing Ingress gets an `InvalidAuthHeader` Warning Event; if no other name is listed, the Ingress forwards the default headers. Use the dashed name. |
| `haproxy-haptic.org/auth-headers-succeed` | Adds request headers to the upstream on successful external authentication. |
| `haproxy-haptic.org/auth-method` | Overrides the HTTP method used for the external authentication subrequest. |
| `haproxy-haptic.org/auth-realm` | Sets the basic-auth realm (default `Restricted`). |
| `haproxy-haptic.org/auth-secret` | Names the Secret holding basic-auth credentials, as `name` or `namespace/name`; while the Secret is absent the route answers 503. Each credential must be a bcrypt, SHA-256/SHA-512 crypt, or yescrypt hash; see [accepted formats](../operations/security.md#basic-auth-password-hashes). |
| `haproxy-haptic.org/auth-secret-type` | Selects the credentials Secret format: `auth-file` (htpasswd in the `auth` key) or `auth-map` (one key per user); default `auth-file`. |
| `haproxy-haptic.org/auth-signin` | Sets the sign-in redirect URL for failed external authentication. |
| `haproxy-haptic.org/auth-tls-cert-header` | Forwards the client certificate details (`X-SSL-Client-CN`, `X-SSL-Client-DN`, `X-SSL-Client-Cert`) to the upstream when a client certificate was presented. Applied from a per-route map. |
| `haproxy-haptic.org/auth-tls-cn-header` | Sets the named request header (for example `X-Scope-OrgID`) to the verified client certificate's CN, and removes any value the client sent. Requires `auth-tls-secret`. |
| `haproxy-haptic.org/auth-tls-error-page` | Redirects to the given URL when client-certificate (mTLS) verification fails. |
| `haproxy-haptic.org/auth-tls-match-subject` | Answers `403` unless the client certificate's subject distinguished name (DN), in RFC 2253 form such as `CN=a,O=Example`, matches this regular expression. Requires `auth-tls-secret`. |
| `haproxy-haptic.org/auth-tls-secret` | Enables client-certificate (mTLS) verification for the host using the CA in the named Secret; a host is required. |
| `haproxy-haptic.org/auth-tls-verify-client` | Sets client-certificate verification: `on` requires it, `optional` and `optional_no_ca` both map to `verify optional` (HAProxy has no distinct `optional_no_ca` mode), and `off` disables it. |
| `haproxy-haptic.org/auth-type` | Enables basic authentication; the only accepted value is `basic`. |
| `haproxy-haptic.org/auth-url` | Sets the external authentication service URL; requires the SPOA hub's external-auth plugin. |
| `haproxy-haptic.org/waf-policy` | Selects a case-sensitive policy name from an administrator-approved catalog or an enabled namespace-local catalog. See [WAF policy setup](../operations/waf-policies.md). |
| `haproxy-haptic.org/oauth` | Enables authentication through `oauth2-proxy` (the only supported provider), building on external auth; skipped when `auth-url` is set. |
| `haproxy-haptic.org/oauth-headers` | Lists headers forwarded from the `oauth2-proxy` response on success (default `X-Auth-Request-Email`). |
| `haproxy-haptic.org/oauth-uri-prefix` | Sets the `oauth2-proxy` callback path prefix (default `/oauth2`). |
| `haproxy-haptic.org/satisfy` | The value `any` grants access when either the source-IP allowlist or basic authentication passes, instead of requiring both. An IPv6 allowlist requires a backend rule and can change reload behavior. |
| `haproxy-haptic.org/waf-mode` | Sets `deny` or `detect`, overriding the selected policy's enforcement only when `waf.ingressPermissions.allowEnforcementOverride` permits it. Requires a selected `waf-policy`. |

#### Reusable WAF policies

Select a policy approved by your platform administrator:

```yaml
metadata:
  annotations:
    haproxy-haptic.org/waf-policy: public-web
```

Policies define what Coraza inspects and whether it reports or blocks matches.
Follow [Configure WAF policies](../operations/waf-policies.md) to create a catalog,
set a cluster baseline, or grant application teams control over their own policies.

<a id="waf-and-grpc-streaming"></a>
<a id="self-service-namespaced-policies"></a>

For gRPC, select a policy with `requestBody.mode: none` so inspection doesn't
buffer a streaming request. See [WAF and gRPC streaming](../operations/waf-policies.md#waf-and-grpc-streaming)
and [self-service namespaced policies](../operations/waf-policies.md#self-service-namespaced-policies).

### API gateway

Authenticate callers, authorize consumer groups, validate requests, or return
fixed responses with these annotations. API-key and JWT authentication establish
a shared consumer identity for authorization, rate limits, and caching.

| Annotation | Behavior |
|------------|----------|
| `haproxy-haptic.org/allowed-consumer-groups` | Comma-separated group set the route permits; a request whose consumer isn't in an allowed group is denied `403`. Requires `consumer-groups-secret` and an authenticated consumer. |
| `haproxy-haptic.org/allowed-methods` | Restricts the accepted HTTP methods (comma-separated); any other method is denied with `405`. Applied from a per-route map. |
| `haproxy-haptic.org/api-key-consumer-header` | Forwards the resolved consumer id to the upstream in the named header. |
| `haproxy-haptic.org/api-key-header` | Header carrying the API key (default `X-API-Key`); mutually exclusive with `api-key-query`. |
| `haproxy-haptic.org/api-key-query` | Query parameter carrying the API key; mutually exclusive with `api-key-header`. |
| `haproxy-haptic.org/api-key-secret` | Names the Secret (data key `keys`, one `apikey[:consumer]` per line). An unknown key returns `401`; a valid key identifies the consumer. Fails closed (`503`) while the Secret is absent. Required by the other `api-key-*` annotations: any of them without it fails closed (`503`) and the Ingress is refused at admission. |
| `haproxy-haptic.org/consumer-groups-secret` | Names the Secret (data key `groups`, one `<consumer>:<group>` per line) mapping each consumer to a group; combined with `allowed-consumer-groups` to authorize. Requires an authenticated consumer and fails closed (`503`) while the Secret is absent. |
| `haproxy-haptic.org/hmac-algorithm` | HMAC digest algorithm (default `sha256`; `sha1`/`sha224`/`sha384`/`sha512`). |
| `haproxy-haptic.org/hmac-header` | Header carrying the client HMAC signature (default `X-Signature`; lowercase hex). |
| `haproxy-haptic.org/hmac-secret` | Names the Secret (data key `secret`) for HMAC request-signature verification (deny `401` on mismatch). The shared key travels (base64) in `haptic-hmac-routes.map`, visible wherever the map is, and the compare isn't constant-time — prefer JWT. Fails closed (`503`) when the Secret is absent. Required by the other `hmac-*` annotations: admission rejects these options without a secret, and reconciliation returns `503`. |
| `haproxy-haptic.org/hmac-signed-string` | What the signature covers: `body` (default) or `path`. Body mode requires `Content-Length`, including `0` for an empty body; unknown-length requests return `411`. It waits up to `extraContext.requestBuffering.waitTimeout` and verifies the complete body. Bodies exceeding HAProxy's available request buffer return `413`. |
| `haproxy-haptic.org/jwt-algorithm` | JWT signature algorithm (default `RS256`); asymmetric only (`RS`/`ES`/`PS` `256`/`384`/`512`) — symmetric `HS*` is rejected so no shared secret is inlined. |
| `haproxy-haptic.org/jwt-audience` | Required `aud` claim value (exact match); an array `aud` (multiple audiences) isn't matched — scalar only. |
| `haproxy-haptic.org/jwt-forward-claims` | Comma-separated `<claim>:<header>` pairs forwarded upstream after verification; each header is stripped from the client request first (anti-spoof). |
| `haproxy-haptic.org/jwt-issuer` | Required `iss` claim value (exact match). |
| `haproxy-haptic.org/jwt-required-claims` | Comma-separated claim names that must be present in the payload; a missing claim is denied `401`. |
| `haproxy-haptic.org/jwt-secret` | Names the Secret (data key `pubkey.pem`) for asymmetric JWT verification with an alg-confusion guard, `exp`/`iss`/`aud`/required-claim checks, and the shared consumer identity from `sub`. Fails closed (`503`) when the Secret is absent. A new key Secret reloads once; routes sharing a key are added and removed without one, and key rotation needs a reload. |
| `haproxy-haptic.org/mock-response` | A non-empty value returns it as a canned response body, short-circuiting the backend (for stubbing an API). The body comes from a per-route map; the first route with a new status and content-type pair reloads once. |
| `haproxy-haptic.org/mock-response-code` | HTTP status for `mock-response` (default `200`). |
| `haproxy-haptic.org/mock-response-content-type` | Content-Type for the `mock-response` body (default `application/json`). |
| `haproxy-haptic.org/request-id` | The value `true` forwards HAProxy's request ID to the application in a header. The access log already records this ID as `req_id`. The first route with a new header name requires a reload. |
| `haproxy-haptic.org/request-id-accept-inbound` | The value `true` preserves an inbound header matching `^[A-Za-z0-9._:-]{1,128}$`. An absent or invalid header is replaced with HAProxy's ID. The access log's `req_id` remains HAProxy-generated, so it can differ from a preserved inbound ID. |
| `haproxy-haptic.org/request-id-header` | Header carrying the correlation id (default `X-Request-ID`). |
| `haproxy-haptic.org/request-schema-configmap` | Enables JSON request-body validation using a ConfigMap schema reference: `[namespace/]name[:key]`, default key `schema.json`. Exactly one schema source is required. Requires `extraContext.apiGateway.requestSchemaValidation.enabled=true`. |
| `haproxy-haptic.org/request-schema-content-types` | Comma-separated accepted media types for the schema (default `application/json`). The plugin strips `; charset=...` parameters before matching; mismatches return `415`. |
| `haproxy-haptic.org/request-schema-fail-open` | Per-route policy for missing plugin verdicts/schema ids (`true` or `false`, default from `extraContext.apiGateway.requestSchemaValidation.defaultFailOpen`, chart default `true`). The default allows the request and records `schema_degraded`; `false` returns `422` with `denied_by=schema_unavailable`. |
| `haproxy-haptic.org/request-schema-max-body-size` | Per-route validator input cap (1..1048576; default `requestSchemaValidation.requestBody.defaultMaxBytes`, chart default `8192`). It must fit within `requestBodyInspection.haproxyBuffer.sizeBytes - reservedBytes`. Oversized requests return `413` before SPOE. This doesn't replace `haproxy-haptic.org/max-request-body-size`, the general backend body-size limit. |
| `haproxy-haptic.org/request-schema-secret` | Enables JSON request-body validation using a Secret schema reference: `[namespace/]name[:key]`, default key `schema.json`. The Secret data value must be base64-encoded JSON Schema. Exactly one schema source is required. |
| `haproxy-haptic.org/fixed-response` | The value `true` returns a fixed response for every request on the route via `http-request return` — for maintenance windows or sunset routes. Runs before mocking and the validators. Defaults to status 503 / `text/plain`, and can return a bare status with no body. The body comes from a per-route map; the first route with a new status and content-type pair reloads once. |
| `haproxy-haptic.org/fixed-response-body` | Optional response body for `fixed-response`. |
| `haproxy-haptic.org/fixed-response-code` | HTTP status for `fixed-response` (default 503; must be 100-599). |
| `haproxy-haptic.org/fixed-response-content-type` | Content-Type for the `fixed-response` body (default `text/plain`). |
| `haproxy-haptic.org/require-content-type` | Requires an allowed `Content-Type` (comma-separated) on body methods (POST/PUT/PATCH); a disallowed type is rejected with `415` (prefix-matched, so charset suffixes still match). Applied from a per-route map. |
| `haproxy-haptic.org/require-headers` | Requires the listed request headers (comma-separated); a request missing any is rejected with `400`. Applied from a per-route map; the first route to require a new header name reloads once. |

#### Authentication and reloads

JWT and API-key authentication share a consumer identity for consumer-group
authorization and shared rate limits. JWT `sub` takes precedence when both apply;
the API-key consumer supplies the identity when the token has no `sub` claim.

Authentication rules read route settings from shared maps. Changing an existing
map value can avoid a reload. A new literal used by a rule—such as an API-key
header, JWT key file, HMAC algorithm, signature header, or required JWT claim—adds
configuration and requires a reload. Enabling a feature on its first route or
removing its last route also adds or removes the shared rules.

#### Request-body validation

JSON request-body validation is opt-in via `controller.config.templatingSettings.extraContext.apiGateway.requestSchemaValidation.enabled=true`. Supply a JSON schema in a ConfigMap or Secret. HAProxy rejects bodies above the route cap before SPOE, waits up to `requestBody.waitTimeout` only on matching POST/PUT/PATCH routes, and then validates the complete body. The process-global `tune.bufsize` comes from `extraContext.requestBodyInspection.haproxyBuffer.sizeBytes`; `reservedBytes` (default `8192`) protects request headers and rewrite space. Any validator or policy body cap above the remaining capacity fails. Requests without `Content-Length` return `411`, duplicate lengths return `400`, and incomplete buffering returns `413` instead of validating truncated input. Request-body transformation isn't supported.

`haproxy-haptic.org/request-schema-max-body-size` is a validator input cap, not the general upload/body-size policy. Use `haproxy-haptic.org/max-request-body-size` when you want to limit the body size a backend may receive. Use `request-schema-max-body-size` to bound how much body data HAProxy may pass to the API-gateway validator and how much JSON the plugin may parse. If both apply to a validated POST/PUT/PATCH request, either one may return `413`; in practice the stricter applicable limit wins.

## Access-log fields

The library contributes these fields to the [structured access log](../operations/access-logging.md),
each only when the corresponding annotation or feature is in use:

| Field | Contributed when | Meaning |
|-------|------------------|---------|
| `consumer` | any resource sets `jwt-secret` or `api-key-secret` | Authenticated consumer identity — the key a per-consumer rate limit buckets on |
| `cache`, `app_backend` | the Varnish tier is enabled | Varnish's `HIT`/`MISS`/`STALE` verdict, and the application backend the route resolved to (the core `backend` field reads `varnish_cache` for cached routes) |
| `client_ip_peer` | any resource sets `src-ip-header` | The real TCP peer, which is how you spot a client claiming an address it doesn't own once `set-src` has rewritten `client_ip` |
| `captured_headers` | any resource sets `request-capture` | The captured request headers |
| `mtls_verify`, `mtls_cn` | any resource sets `auth-tls-secret` or `auth-tls-cert-header` | The certificate verification result (0 on success, otherwise an X509 error code) and the client's CN |

The built-in fields omit API keys, HMAC signatures, and full client certificates.
Review any [custom log fields](../operations/access-logging.md#add-your-own-fields)
or header captures you add; those can include sensitive request values.

## Configuration

```yaml
controller:
  templateLibraries:
    hapticAnnotations:
      enabled: true  # Enabled by default
```

## Troubleshooting

### Don't mix annotation families for one feature

Use one annotation family for each feature. For example, setting both native
`cors-enable` and nginx-compatible `enable-cors` is a conflict even when their
values agree. Different features can use different families, and only enabled
libraries count. Separate settings such as connect and server timeouts can come
from different families.

Admission rejects conflicting annotations and names them in the error. An
existing conflict produces an `AnnotationFamilyConflict` Warning Event during
rendering. Remove the duplicate annotation from the Ingress.

### How HAPTIC handles a misconfigured annotation

Admission rejects invalid values and identifies the annotation to fix. If an
invalid value already exists in the cluster, handling depends on the feature:

| Feature | Effect during rendering |
|---------|-------------------------|
| Redirect, CORS, or other values reported with `InvalidAnnotationValue` or `InvalidAnnotation` | Skip the invalid feature on that Ingress; the Event names the setting and effect. |
| A WAF policy that can't be resolved or compiled | Return `503` on selecting routes and emit a policy Warning Event; see [WAF troubleshooting](../operations/waf-policies.md#diagnose-a-rejected-policy). |
| Values that prevent valid configuration, such as an unsupported compression algorithm | Hold that resource at its last validated revision while independent updates continue. See [rejected resource changes](../operations/input-isolation.md). |

Inspect Events to find the affected resource and setting:

```bash
kubectl get events --all-namespaces --field-selector type=Warning
```

### Removed annotations

Remove these annotations from existing manifests:

| Annotation | Behavior |
|------------|----------|
| `haproxy-haptic.org/scale-server-slots` | No longer has any effect. HAPTIC manages server membership without reserved slots. Setting it emits a Warning Event; remove the annotation. |
