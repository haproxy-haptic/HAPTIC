# `nginx-ingress` library

Use this library when migrating Ingresses with `nginx.ingress.kubernetes.io/*`
annotations from [ingress-nginx](https://kubernetes.github.io/ingress-nginx/).
It's disabled by default.

<a id="overview"></a>

The library translates supported annotations into HAProxy configuration,
including backend settings, session affinity, rate limits, rewrites, redirects,
Cross-Origin Resource Sharing (CORS), authentication, and canary routing.
Review the compatibility report and the limits below before cutover.

Try the migration report on a sample Ingress. It identifies annotations that work unchanged, behave differently, or need replacing:

<div class="pg-embed" markdown data-scenario="nginx-ingress" data-facade="resources" data-input="resources" data-input-focus="nginx.ingress.kubernetes.io/proxy-connect-timeout" data-tab="migration" data-controls="tabs,resources" data-title="ingress-nginx annotation migration report" data-height="440">

<p class="pg-task" markdown>In the **Resources** panel, add <code>nginx.ingress.kubernetes.io/server-snippet: "more_set_headers X-From: nginx;"</code> to the `shop` Ingress, then watch a new **dropped** verdict appear in the **migration** report.</p>

<details class="pg-hint" markdown>
<summary>What to expect</summary>

The migration report gains a red `dropped` badge for `server-snippet` — "nginx server-level directives have no HAProxy equivalent" — and the dropped count rises by one. Because the annotation is dropped, it adds nothing to `haproxy.cfg`; the migration report is exactly where HAPTIC flags the annotations that won't carry over.

</details>

</div>

Before moving traffic, check [annotation compatibility](../annotation-compatibility.md)
and follow the [migration guide](../migrating.md#from-ingress-nginx).

## Configuration

Apply the following Helm values through your [values file](../deploying-with-helm.md#change-settings).
The annotation examples on this page belong under an Ingress's `metadata.annotations`;
quote all annotation values.

```yaml
controller:
  templateLibraries:
    nginxIngress:
      enabled: true  # Disabled by default
```

Enabling the library also auto-enables two Stream Processing Offload Agent (SPOA) hub plugins — `external-auth` (backing the `auth-url` family) and `coraza` (the Web Application Firewall (WAF) backing `modsecurity-snippet`) — which deploys the [SPOA hub sidecar](../operations/spoa-hub.md) in the HAProxy pod. An explicit `spoaHub.plugins.<name>.enabled` value overrides the auto-enable in either direction.

## Backend configuration

### Timeouts

**Annotations**:

| Annotation | Description | Default |
|------------|-------------|---------|
| `proxy-connect-timeout` | Backend connection timeout (seconds) | - |
| `proxy-read-timeout` | Backend response timeout (seconds) | - |
| `proxy-send-timeout` | Backend send timeout (seconds) | - |

Timeout values are seconds, such as `"60"`.

If both `proxy-read-timeout` and `proxy-send-timeout` are set, the larger value
becomes HAProxy's server timeout.

```yaml
annotations:
  nginx.ingress.kubernetes.io/proxy-connect-timeout: "10"
  nginx.ingress.kubernetes.io/proxy-read-timeout: "60"
  nginx.ingress.kubernetes.io/proxy-send-timeout: "30"
```

`proxy-connect-timeout` renders as a literal `timeout connect`. The server timeout is reload-free: its value moves into `backend-timeouts.map` as milliseconds keyed `<backend>|server` (here `my-backend|server 60000`), read by the uniform `set-timeout` line every backend carries.

### `nginx.ingress.kubernetes.io/load-balance`

Load balancing algorithm for the backend.

**Valid values**: `round_robin`, `least_conn`, `ip_hash`, `random`, `ewma`

**Mapping to HAProxy**:

| Nginx Value | HAProxy Value |
|-------------|---------------|
| `round_robin` | `roundrobin` |
| `least_conn` | `leastconn` |
| `ip_hash` | `source` |
| `random` | `random` |
| `ewma` | `leastconn` (closest equivalent) |

```yaml
annotations:
  nginx.ingress.kubernetes.io/load-balance: "least_conn"
```

### `nginx.ingress.kubernetes.io/proxy-body-size`

Maximum allowed request body size. Requests exceeding this limit receive a 413 response.

**Valid values**: Plain number (bytes), or with `k`/`m`/`g` suffix. Value `0` means unlimited (no map entry emitted).

```yaml
annotations:
  nginx.ingress.kubernetes.io/proxy-body-size: "10m"
```

### `nginx.ingress.kubernetes.io/backend-protocol`

Protocol used to communicate with the backend.

**Valid values**: `HTTP`, `HTTPS`, `GRPC`, `GRPCS`

**Mapping to HAProxy server options**:

| Value | HAProxy Server Flags |
|-------|---------------------|
| `HTTP` | (default, no additional flags) |
| `HTTPS` | `ssl verify none` |
| `GRPC` | `proto h2` |
| `GRPCS` | `ssl verify none proto h2` |

`AJP` and `FCGI` are unsupported and fail validation.

```yaml
annotations:
  nginx.ingress.kubernetes.io/backend-protocol: "GRPC"
```

### `nginx.ingress.kubernetes.io/use-proxy-protocol`

Send PROXY protocol v2 header to the backend.

```yaml
annotations:
  nginx.ingress.kubernetes.io/use-proxy-protocol: "true"
```

`send-proxy-v2` lives on `default-server`, not on individual server lines, so pods can be added or removed over the runtime API without a HAProxy reload.

### `nginx.ingress.kubernetes.io/configuration-snippet`

Insert HAProxy directives into the backend section. Existing nginx directives
must be rewritten in HAProxy syntax; HAPTIC doesn't translate them. HAPTIC
rejects the annotation unless
`controller.config.templatingSettings.extraContext.ingressPermissions.allowRawHAProxyConfig`
is `true`; see [Raw configuration annotations](../operations/security.md#raw-configuration-annotations).

```yaml
annotations:
  nginx.ingress.kubernetes.io/configuration-snippet: |
    http-send-name-header X-Backend-Server
    retries 5
```

### `nginx.ingress.kubernetes.io/upstream-hash-by`

Hash-based load balancing using a nginx variable or HAProxy fetch expression.

**Supported nginx variable translations**:

| Nginx Variable | HAProxy Fetch |
|----------------|---------------|
| `$request_uri` | `url` |
| `$remote_addr` | `src` |
| `$cookie_XXXX` | `req.cook(XXXX)` |
| `$http_xxxx` | `req.hdr(xxxx)` (underscores replaced with hyphens) |
| `$arg_XXXX` | `url_param(XXXX)` |

Values not starting with `$` are passed through as-is (assumed to be HAProxy fetch expressions). Unrecognized `$variables` fail with an error.

```yaml
annotations:
  nginx.ingress.kubernetes.io/upstream-hash-by: "$request_uri"
```

### `nginx.ingress.kubernetes.io/proxy-next-upstream`

Conditions under which a failed request is retried against another server, mapped to HAProxy's `retry-on`.

**Mapping to HAProxy**:

| Nginx condition | HAProxy `retry-on` term |
|-----------------|-------------------------|
| `error` | `conn-failure` |
| `timeout` | `response-timeout` |
| `invalid_header` | `junk-response` |
| `http_<NNN>` (for example `http_503`) | `<NNN>` |
| `off` | `retries 0` (retries disabled) |
| `non_idempotent` | ignored (no HAProxy equivalent) |

**Related annotations**:

| Annotation | Description |
|------------|-------------|
| `proxy-next-upstream-tries` | Maps to HAProxy `retries`; `"0"` (nginx meaning unlimited) falls back to HAProxy's default retry count |

```yaml
annotations:
  nginx.ingress.kubernetes.io/proxy-next-upstream: "error timeout http_503"
  nginx.ingress.kubernetes.io/proxy-next-upstream-tries: "3"
```

`option redispatch` is already set in the defaults section, so a retry lands on a different server.

### Upstream request headers

**Annotations**:

| Annotation | Description |
|------------|-------------|
| `upstream-vhost` | Sets the `Host` header toward the backend |
| `x-forwarded-prefix` | Sets the `X-Forwarded-Prefix` request header |
| `connection-proxy-header` | Sets the `Connection` header toward the backend |

```yaml
annotations:
  nginx.ingress.kubernetes.io/upstream-vhost: "internal.example.com"
  nginx.ingress.kubernetes.io/x-forwarded-prefix: "/app"
```

### Buffering

**Annotations**:

| Annotation | Description |
|------------|-------------|
| `proxy-request-buffering` | `on` or `off`: overrides the [fleet-wide request buffering](base.md#request-buffering) for the Ingress's routes |
| `proxy-buffer-size` | Response-header size the backend needs, for example `16k` |

HAProxy buffers a request only when it carries a `Content-Length`, and holds at most `tune.bufsize` bytes, so `on` protects backends from slow clients rather than spooling a whole upload as nginx does.

HAProxy has no per-route response buffer: every route's response headers must fit in `tune.bufsize` minus 1 KiB, which is 15 KiB with the default `tune.bufsize` of 16384. When `proxy-buffer-size` asks for more, HAPTIC records a `ProxyBufferSizeExceeded` Warning Event on the Ingress naming the `controller.config.templatingSettings.extraContext.tune.bufsize` value that fits. Raising it increases memory use for every connection.

```yaml
annotations:
  nginx.ingress.kubernetes.io/proxy-request-buffering: "off"
  nginx.ingress.kubernetes.io/proxy-buffer-size: "8k"
```

### Rate limiting

**Annotations**:

| Annotation | Description |
|------------|-------------|
| `limit-rps` | Maximum requests per second per source IP |
| `limit-rpm` | Maximum requests per minute per source IP |
| `limit-connections` | Maximum concurrent connections per source IP |
| `limit-whitelist` | Comma-separated CIDRs exempt from the limits |
| `limit-burst-multiplier` | Burst allowance for `limit-rps` and `limit-rpm`, from 1 to 100 |

Exceeding a limit returns HTTP 429 — ingress-nginx rejects with 503. ingress-nginx allows a 5x burst by default; HAPTIC enforces a hard cap unless you set `limit-burst-multiplier`, so expect stricter enforcement at the same value after migrating. With a multiplier, HAPTIC counts requests over a window that many times longer at the same average rate: `limit-rps: "10"` with `limit-burst-multiplier: "5"` allows 50 requests in any 5 seconds. A stick-table stores each data type once, so the three limits are mutually exclusive with precedence `limit-rps` > `limit-rpm` > `limit-connections`; HAPTIC records a `RateLimitCapIgnored` Event on the Ingress naming the ones it ignored. Invalid CIDRs in `limit-whitelist` fail the render.

```yaml
annotations:
  nginx.ingress.kubernetes.io/limit-rps: "100"
  nginx.ingress.kubernetes.io/limit-whitelist: "10.0.0.0/8"
```

Rate-limit settings and source-IP exemptions are stored in shared maps. Updating
an existing route's settings changes those maps without changing its backend.
Counters are keyed by route and client address, so each route has a separate
per-client budget. The `peers localinstance` section preserves counters across
HAProxy reloads.

### `nginx.ingress.kubernetes.io/limit-rate`

**Status**: Caveat

Download throttle — limits the bytes per second HAProxy sends toward the client, via an outbound bandwidth-limit filter. The limit applies per stream, so an HTTP/2 client that opens several streams gets a multiple of it.

**Related annotations**:

| Annotation | Description |
|------------|-------------|
| `limit-rate` | Maximum bytes per second per stream (`k`/`m`/`g` suffixes accepted) |
| `limit-rate-after` | Mapped to the bandwidth filter's `min-size` — the smallest chunk the filter forwards at a time, which trades CPU use against latency. It doesn't delay the throttle the way nginx's offset does, and HAProxy has no equivalent for that. A large value adds latency. Leave it unset unless you want to tune the forward chunk size, where roughly two TCP maximum segment sizes (about 2896 bytes) is HAProxy's suggested starting point. |

For a per-client or per-service budget rather than a per-stream one, the native library's [`bandwidth-limit-scope`](haptic-annotations.md#rate-and-bandwidth-limiting) covers what nginx can't express.

```yaml
annotations:
  nginx.ingress.kubernetes.io/limit-rate: "100k"
```

Bandwidth rates are stored in `ing-bw-routes.map`, so changing a rate can use a
map update. HAProxy requires `limit-rate-after` in the filter declaration:
introducing a new size requires a reload, while routes using an existing size
share its filter. Values normalize to bytes, so `1m` and `1048576` share a filter.

The download cap counts compressed bytes when response compression is enabled.

## Backend TLS (`proxy-ssl-*`)

The `proxy-ssl-*` family configures TLS toward the upstream: a client certificate, a CA to verify the upstream's certificate against, SNI, ciphers, and protocol bounds. The whole family requires backend TLS to be on — set `backend-protocol: "HTTPS"` (or `"GRPCS"`), otherwise the annotations have no effect.

### `nginx.ingress.kubernetes.io/proxy-ssl-secret`

Reference to a `kubernetes.io/tls` Secret: `tls.crt` + `tls.key` become the client certificate presented to the upstream, and `ca.crt` becomes the CA the upstream certificate is verified against when `proxy-ssl-verify` is on. The client certificate is presented regardless of the verify mode.

**Format**: `name` (resolves in the Ingress namespace) or `namespace/name`.

```yaml
annotations:
  nginx.ingress.kubernetes.io/backend-protocol: "HTTPS"
  nginx.ingress.kubernetes.io/proxy-ssl-secret: "upstream-tls"
  nginx.ingress.kubernetes.io/proxy-ssl-verify: "on"
  nginx.ingress.kubernetes.io/proxy-ssl-name: "backend.internal"
```

### `nginx.ingress.kubernetes.io/proxy-ssl-verify`

`"on"` verifies the upstream certificate against the referenced Secret's `ca.crt` (`verify required`); the default is off (`verify none`), matching ingress-nginx. The truthy spellings `on`/`true`/`yes`/`1` are matched case-insensitively so a spelling variant can't silently disable verification. Fail-closed: `"on"` without a resolvable `proxy-ssl-secret` containing `ca.crt` fails the render instead of silently skipping verification.

### `nginx.ingress.kubernetes.io/proxy-ssl-name`

Hostname used as SNI toward the upstream and — when verification is on — as `verifyhost` for certificate-name checking.

### `nginx.ingress.kubernetes.io/proxy-ssl-ciphers`

Cipher list for the upstream TLS connection (HAProxy's `ciphers` server option).

### `nginx.ingress.kubernetes.io/proxy-ssl-protocols`

Space-separated list of enabled TLS versions, for example `"TLSv1.2 TLSv1.3"`. HAProxy expresses a version span, not a list: the lowest listed version becomes `ssl-min-ver` and the highest `ssl-max-ver`, so gaps in the list can't be expressed.

`proxy-ssl-verify-depth` is unsupported; limiting trusted certificate authorities doesn't enforce a
chain-depth limit. `proxy-ssl-server-name` is ignored; use `proxy-ssl-name` for SNI.

## Upstream response rewriting

### `nginx.ingress.kubernetes.io/proxy-cookie-domain`

Rewrites the `Domain=` attribute of upstream `Set-Cookie` response headers. Only the two-argument `"<from> <to>"` form is supported; any other value (including nginx's `"off"`) fails the render.

```yaml
annotations:
  nginx.ingress.kubernetes.io/proxy-cookie-domain: "backend.internal example.com"
```

### `nginx.ingress.kubernetes.io/proxy-cookie-path`

Rewrites the `Path=` attribute of upstream `Set-Cookie` response headers. Same `"<from> <to>"`-only contract as `proxy-cookie-domain`.

```yaml
annotations:
  nginx.ingress.kubernetes.io/proxy-cookie-path: "/internal /app"
```

### `nginx.ingress.kubernetes.io/proxy-redirect-from`

Rewrites the `Location` and `Refresh` response headers coming from the upstream, replacing the `from` text with `proxy-redirect-to`'s value. Both annotations are required together, and neither value may contain spaces. `"default"` isn't supported — nginx derives it from `proxy_pass`, which has no HAProxy equivalent, so a warning comment is rendered and no rewrite happens; `"off"` disables the rewrite.

**Related annotations**:

| Annotation | Description |
|------------|-------------|
| `proxy-redirect-to` | Replacement text for the matched `from` value |

```yaml
annotations:
  nginx.ingress.kubernetes.io/proxy-redirect-from: "http://backend.internal/"
  nginx.ingress.kubernetes.io/proxy-redirect-to: "https://example.com/"
```

## Session affinity

Cookie-based session affinity — also called sticky sessions — pins a client to the same backend server across requests.

### `nginx.ingress.kubernetes.io/affinity`

Enable cookie-based session affinity.

**Valid values**: `cookie`

**Related annotations**:

| Annotation | Description | Default |
|------------|-------------|---------|
| `session-cookie-name` | Cookie name | `INGRESSCOOKIE` |
| `session-cookie-path` | `Path` cookie attribute | - |
| `session-cookie-domain` | `Domain` cookie attribute (HAProxy's `domain` keyword) | - |
| `session-cookie-secure` | Adds the `Secure` attribute when `"true"` | - |
| `session-cookie-samesite` | `SameSite` attribute: `Strict`, `Lax`, or `None` (other values fail the render) | - |
| `session-cookie-max-age` | `Max-Age` attribute (seconds) — the browser cookie lifetime | - |
| `session-cookie-expires` | Also emitted as `Max-Age` — HAProxy can't compute an absolute `Expires` date, and browsers treat both equivalently; `session-cookie-max-age` wins when both are set | - |
| `session-cookie-hash` | Accepted but not configurable — HAProxy's dynamic cookies always hash via `dynamic-cookie-key`, so the value is ignored with a rendered warning | - |
| `affinity-mode` | `balanced` or `persistent`. HAProxy's stickiness is always `persistent`: sessions stay on their pod when the Service scales up | `balanced` |

```yaml
annotations:
  nginx.ingress.kubernetes.io/affinity: "cookie"
  nginx.ingress.kubernetes.io/session-cookie-name: "SERVERID"
  nginx.ingress.kubernetes.io/session-cookie-path: "/app"
  nginx.ingress.kubernetes.io/session-cookie-secure: "true"
  nginx.ingress.kubernetes.io/session-cookie-samesite: "Lax"
  nginx.ingress.kubernetes.io/session-cookie-max-age: "86400"
```

## URL rewriting

### `nginx.ingress.kubernetes.io/rewrite-target`

Replaces the whole request path before HAProxy forwards the request to the backend. The query string is kept.

`$1`, `$2`, … refer to the capture groups of the Ingress path's regex. As in ingress-nginx, setting `rewrite-target` makes the Ingress's paths regex paths, so you don't need `use-regex` as well. A path equal to the target isn't rewritten.

```yaml
metadata:
  annotations:
    nginx.ingress.kubernetes.io/rewrite-target: /$2
spec:
  rules:
    - host: example.com
      http:
        paths:
          - path: /api(/|$)(.*)
            pathType: ImplementationSpecific
            backend:
              service: {name: api, port: {number: 80}}
```

With this Ingress, `/api/users?page=2` reaches the backend as `/users?page=2`, and `/api` reaches it as `/`.

### `nginx.ingress.kubernetes.io/use-regex`

**Status**: Caveat

Set to `"true"` to match the Ingress's paths as regular expressions. A path containing regex syntax matches like ingress-nginx's `location ~* "^<path>"`: case-insensitive and anchored at the start of the path only. HAPTIC rejects a regex path whose groups or `[ ]` classes don't close within it, or that uses `\Q`, `\E`, `\c` or `(*`, because such a path could match on other hosts.

Differences from ingress-nginx:

- A path without regex syntax keeps its Kubernetes `Prefix` or `Exact` match, which is case-sensitive and matches whole path segments.
- Other Ingresses on the same host keep their own matching. ingress-nginx turns every path of the host into a regex.
- Regex paths are tried before prefix paths, as set by [`regexMatchOrder`](../template-libraries.md#path-matching-order), not longest path first.

```yaml
annotations:
  nginx.ingress.kubernetes.io/use-regex: "true"
```

### `nginx.ingress.kubernetes.io/app-root`

Redirect requests to root path (`/`) to the specified path.

```yaml
annotations:
  nginx.ingress.kubernetes.io/app-root: "/dashboard"
```

## Redirects

### `nginx.ingress.kubernetes.io/ssl-redirect`

Redirect HTTP requests to HTTPS.

**Related annotations**:

| Annotation | Description | Redirect Code |
|------------|-------------|---------------|
| `ssl-redirect` | Enable SSL redirect | `308` |
| `force-ssl-redirect` | Force SSL redirect | `308` |

Both emit a `308` (Permanent Redirect), matching ingress-nginx, which sends both
via its `http-redirect-code` (default `308`). To change the code, set
`nginxHttpRedirectCode` (HAPTIC's equivalent of nginx's global
`http-redirect-code`) in values:

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        nginxHttpRedirectCode: "301"   # valid: 301, 302, 303, 307, 308
```

```yaml
annotations:
  nginx.ingress.kubernetes.io/ssl-redirect: "true"
```

### `nginx.ingress.kubernetes.io/permanent-redirect`

Redirect all requests for the Ingress's hosts to the specified URL. Host-scoped via a reload-free map; rules without a host are skipped.

**Related annotations**:

| Annotation | Description | Default |
|------------|-------------|---------|
| `permanent-redirect-code` | HTTP status code for the redirect | `301` |

```yaml
annotations:
  nginx.ingress.kubernetes.io/permanent-redirect: "https://new.example.com"
  nginx.ingress.kubernetes.io/permanent-redirect-code: "308"
```

### `nginx.ingress.kubernetes.io/temporal-redirect`

Redirect all requests for the Ingress's hosts to the specified URL. Host-scoped via a reload-free map; rules without a host are skipped.

**Related annotations**:

| Annotation | Description | Default |
|------------|-------------|---------|
| `temporal-redirect-code` | HTTP status code for the redirect | `302` |

```yaml
annotations:
  nginx.ingress.kubernetes.io/temporal-redirect: "https://maintenance.example.com"
```

### `nginx.ingress.kubernetes.io/from-to-www-redirect`

301-redirect between each rule host and its `www.` counterpart, in whichever direction applies: host `example.com` redirects to `www.example.com`, host `www.example.com` redirects to `example.com`. The request path and scheme are preserved.

```yaml
annotations:
  nginx.ingress.kubernetes.io/from-to-www-redirect: "true"
```

## `hsts`

### `nginx.ingress.kubernetes.io/hsts`

Enable HTTP Strict Transport Security headers.

**Related annotations**:

| Annotation | Description | Default |
|------------|-------------|---------|
| `hsts` | Enable HSTS | - |
| `hsts-max-age` | Max-age in seconds | `15724800` |
| `hsts-include-subdomains` | Include subdomains | - |
| `hsts-preload` | Enable preload | - |

```yaml
annotations:
  nginx.ingress.kubernetes.io/hsts: "true"
  nginx.ingress.kubernetes.io/hsts-max-age: "31536000"
  nginx.ingress.kubernetes.io/hsts-include-subdomains: "true"
  nginx.ingress.kubernetes.io/hsts-preload: "true"
```

## `cors`

### `nginx.ingress.kubernetes.io/enable-cors`

Enable CORS handling for the ingress. The headers come from per-route maps read by one frontend rule block, so adding or removing a CORS route is reload-free.

**Related annotations**:

| Annotation | Description | Default |
|------------|-------------|---------|
| `enable-cors` | Enable CORS | - |
| `cors-allow-origin` | Allowed origins — comma-separated list, single-level `*.` wildcards; matched Origin is echoed back | `*` |
| `cors-allow-methods` | Allowed methods | `GET, PUT, POST, DELETE, PATCH, OPTIONS` |
| `cors-allow-headers` | Allowed headers | Common headers |
| `cors-allow-credentials` | Allow credentials | - |
| `cors-expose-headers` | Exposed headers | - |
| `cors-max-age` | Preflight cache time | `1728000` |

```yaml
annotations:
  nginx.ingress.kubernetes.io/enable-cors: "true"
  nginx.ingress.kubernetes.io/cors-allow-origin: "https://example.com"
  nginx.ingress.kubernetes.io/cors-allow-credentials: "true"
```

## Access control

### `nginx.ingress.kubernetes.io/whitelist-source-range`

Comma-separated list of CIDRs allowed to access this ingress.

```yaml
annotations:
  nginx.ingress.kubernetes.io/whitelist-source-range: "10.0.0.0/8, 192.168.0.0/16"
```

The route's CIDRs live in maps shared by every annotation library (`ing-ac-routes.map`, `ing-ac-partitions.map`, `ing-ac-allow.map`, `ing-ac-deny.map`), so adding or removing an allowlisted route is a map update, not a reload. A list with an IPv6 entry keeps a per-route `acl`/`deny` pair, which reloads on add and remove.

### `nginx.ingress.kubernetes.io/denylist-source-range`

Comma-separated list of CIDRs denied access to this ingress.

```yaml
annotations:
  nginx.ingress.kubernetes.io/denylist-source-range: "203.0.113.0/24"
```

## Custom headers

### Custom request and response headers

**Annotations**:

| Annotation | Description |
|------------|-------------|
| `custom-request-headers` | Pipe-separated `name:value` pairs for request headers |
| `custom-response-headers` | Pipe-separated `name:value` pairs for response headers |

```yaml
annotations:
  nginx.ingress.kubernetes.io/custom-request-headers: "X-Custom-Header:value|X-Another:test"
  nginx.ingress.kubernetes.io/custom-response-headers: "X-Frame-Options:DENY"
```

## Server alias and default backend

### `nginx.ingress.kubernetes.io/server-alias`

Comma-separated extra hostnames that route exactly like the Ingress's first rule host. Each alias becomes a `host.map` entry pointing at the rule host's routing key, so every path already registered for that host applies to the alias — no backend or path duplication. Wildcard aliases (`*.example.com`) are normalized the same way rule hosts are. Each alias must be a valid hostname; case doesn't matter. A hostname an older Ingress already claims stays with that Ingress; see [Host aliases claim hostnames](../operations/security.md#host-aliases-claim-hostnames).

```yaml
annotations:
  nginx.ingress.kubernetes.io/server-alias: "example.org,www.example.org"
```

### `nginx.ingress.kubernetes.io/default-backend`

Names a Service that serves requests matching one of this Ingress's hosts but none of its rule paths. The chart builds a dedicated backend pool for the Service's first port and adds a per-host catch-all entry to the path-prefix map — longest-prefix matching prefers the Ingress's own paths and falls through to the catch-all. Silently skipped when the Service doesn't resolve.

**Format**: `name` (resolves in the Ingress namespace) or `namespace/name`.

```yaml
annotations:
  nginx.ingress.kubernetes.io/default-backend: "error-pages"
```

### `nginx.ingress.kubernetes.io/custom-http-errors`

Replaces an upstream response whose status is in the list with an error page from the default backend. The list holds statuses from 400 to 599, separated by commas.

```yaml
annotations:
  nginx.ingress.kubernetes.io/custom-http-errors: "404,503"
  nginx.ingress.kubernetes.io/default-backend: "error-pages"
```

The pages come from the Service named by `default-backend`. If the Ingress doesn't set `default-backend`, they come from the Service in `nginxDefaultBackendService`, HAPTIC's equivalent of ingress-nginx's `--default-backend-service`:

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        nginxDefaultBackendService: "ingress-errors/error-pages"   # namespace/name
```

HAProxy can't send a request to another backend after the upstream has answered, so the controller fetches each page when it renders the configuration and HAProxy serves the copy. For each code the controller sends two `GET /` requests to the Service's first port, one with `X-Format: text/html` and one with `X-Format: application/json`, each with the headers `X-Code`, `X-Namespace`, and `X-Ingress-Name`. These are the headers ingress-nginx sends, so a default backend written for ingress-nginx needs no changes.

The replaced page carries the response headers HAPTIC adds to the route, such as HSTS, CORS, `Server`, and `custom-response-headers`. Headers the upstream sent aren't kept, as in ingress-nginx.

A client whose first `Accept` entry is `application/json` gets the JSON page with `Content-Type: application/json`; every other client gets the HTML page with `Content-Type: text/html`. The JSON page is served only if the default backend's answer to the JSON request starts with `{` or `[`, so a backend without JSON pages serves its HTML page to every client.

The controller re-fetches every page on a fixed interval, 5 minutes by default:

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        nginxCustomHTTPErrorsRefreshInterval: "1m"   # whole s, m or h; at least 30s
```

A page changed on the default backend reaches clients after the next refresh plus one render and rollout, and costs one hitless HAProxy reload. An unchanged page costs nothing.

Differences from ingress-nginx:

- The page is a snapshot. Per-request headers (`X-Original-URI`, `X-Request-ID`, `X-Service-Name`, `X-Service-Port`) can't influence it, and `Accept` only selects between the HTML and JSON pages, so a page that varies by request shows the same content to every client.
- The client receives the listed status, whatever status the default backend answered with.
- Only responses from the upstream are replaced. Errors HAProxy generates itself, such as a `503` when no endpoint is ready, keep the chart's error pages.
- A page that changes on every fetch reloads HAProxy on every refresh. Serve a stable page.
- A page must fit in HAProxy's response buffer: `tune.bufsize` minus 2048 bytes, 14336 bytes at the default `bufsize`.

When a page can't be fetched, is empty, or is too large, the controller records a `CustomErrorPageUnavailable` Warning Event on the Ingress. HAProxy keeps serving the page it already has; if it has none, the upstream's response reaches the client unchanged. An Ingress whose default backend Service doesn't exist gets a `CustomHTTPErrorsNoDefaultBackend` Event.

The controller fetches the pages itself, so its NetworkPolicy must allow egress to the default backend. The chart's default `controller.networkPolicy.egress.additionalRules` allows every in-cluster pod; if you restricted it, add a rule for the default backend's pods.

## Authentication

### `nginx.ingress.kubernetes.io/auth-type`

Enable basic authentication using credentials from a Kubernetes Secret.

**Related annotations**:

| Annotation | Description | Default |
|------------|-------------|---------|
| `auth-type` | Authentication type (only `basic` supported; `digest` fails the render) | - |
| `auth-secret` | Secret name (or `namespace/name`) | - |
| `auth-secret-type` | Secret layout: `auth-file` or `auth-map` (other values fail the render) | `auth-file` |
| `auth-realm` | Authentication realm | `Restricted` |

With `auth-secret-type: auth-file` (the default), put `username:hash` lines in
the Secret's `auth` key. With `auth-map`, each key is a username and its value
is that user's password hash.

```yaml
annotations:
  nginx.ingress.kubernetes.io/auth-type: "basic"
  nginx.ingress.kubernetes.io/auth-secret: "basic-auth"
  nginx.ingress.kubernetes.io/auth-realm: "Protected Area"
```

Create credentials with Apache `htpasswd`, which prompts for a password. This
example uses bcrypt; serve the protected route over HTTPS.

```bash
umask 077
htpasswd -n -B admin > auth
kubectl -n default create secret generic basic-auth --from-file=auth=auth
rm auth
```

Without `-B`, `-2`, or `-5`, `htpasswd` writes `$apr1$` hashes, which HAPTIC
refuses, as it does `{SHA}`, `$1$`, Data Encryption Standard (DES) crypt, and plaintext. See
[accepted formats](../operations/security.md#basic-auth-password-hashes).

The challenge is one rule block per HTTP frontend fed by a per-route map, so a route on an existing credentials Secret is added and removed at runtime. A realm that needs escaping (`"`, `\` or `$`) keeps a backend `http-request auth` rule, which reloads on add and remove.

### `nginx.ingress.kubernetes.io/satisfy`

With `"any"`, a request passes if **either** its source IP is in `whitelist-source-range` **or** it authenticates via basic auth — instead of the default `"all"`, which requires both. The combined gate only forms when the Ingress has a whitelist, `auth-type: basic`, and a resolvable `auth-secret`. Unlike ingress-nginx, `satisfy` doesn't extend to external auth (`auth-url`).

```yaml
annotations:
  nginx.ingress.kubernetes.io/satisfy: "any"
  nginx.ingress.kubernetes.io/whitelist-source-range: "10.0.0.0/8"
  nginx.ingress.kubernetes.io/auth-type: "basic"
  nginx.ingress.kubernetes.io/auth-secret: "basic-auth"
```

Changes involving IPv6 allowlists or realms with escaped characters require a
reload. Other changes can use runtime updates when the credentials and realm
already exist.

## External authentication

The library wires the `nginx.ingress.kubernetes.io/auth-*` family to the SPOA hub's `external-auth` plugin (v0.3.0+). When set, each request hits an HTTP auth subrequest before reaching the backend; the auth service's status code decides whether HAProxy forwards the request, redirects to a sign-in URL, or returns 401.

External auth is enforced independently of basic auth. When a route carries both `auth-url` and `auth-type: basic` + `auth-secret`, the two stack: a request must pass the external-auth subrequest *and* present valid basic-auth credentials — external auth denies at the frontend, basic auth challenges at the backend. You can't OR them; `satisfy: any` only OR-combines basic auth with the IP whitelist, not with external auth.

### Prerequisites

The SPOA hub sidecar with the `external-auth` plugin must be enabled:

```yaml
spoaHub:
  plugins:
    external-auth:
      enabled: true
```

The hub auto-enables when any plugin is on, and the spoa-hub template library auto-loads when the hub is enabled. Note: enabling `controller.templateLibraries.nginxIngress.enabled` **also** auto-enables `external-auth` (the nginx-ingress library is opt-in for this reason). See the [SPOA Hub operations guide](../operations/spoa-hub.md) for the full deployment surface.

Set an explicit `host` on every protected Ingress rule. HAPTIC rejects
protected rules without one.

### `nginx.ingress.kubernetes.io/auth-url`

Auth service URL the SPOA hub calls per request. The plugin appends the original request path, sends a GET (overridable via `auth-method`), and gates the request based on the response status: 2xx allows, 3xx with `auth-signin` redirects, anything else returns 401.

```yaml
annotations:
  nginx.ingress.kubernetes.io/auth-url: "https://auth.example.com/check"
```

### `nginx.ingress.kubernetes.io/auth-signin`

Browser-flow sign-in URL. When set, an auth failure produces a 302 redirect instead of a 401 — the standard pattern for OpenID Connect (OIDC) / Security Assertion Markup Language (SAML) flows. The deny rule still emits, so routes without `auth-signin` keep the API-friendly 401.

```yaml
annotations:
  nginx.ingress.kubernetes.io/auth-url: "https://auth.example.com/check"
  nginx.ingress.kubernetes.io/auth-signin: "https://login.example.com/oauth2/start"
```

Redirect URLs are used verbatim: variables such as `$escaped_request_uri`
aren't expanded. Have the authentication service preserve the original request
URL if your login flow needs it.

### `nginx.ingress.kubernetes.io/auth-method`

HTTP method for the auth subrequest. Defaults to `GET` (or whatever the plugin's TOML config sets); set this to override per-route.

**Valid values**: `GET`, `HEAD`, `POST`, `PUT`, `PATCH`, `DELETE`, `OPTIONS`

```yaml
annotations:
  nginx.ingress.kubernetes.io/auth-url: "https://auth.example.com/check"
  nginx.ingress.kubernetes.io/auth-method: "POST"
```

`POST`, `PUT`, and `PATCH` auth requests have an empty body; the original
request payload isn't forwarded.

### `nginx.ingress.kubernetes.io/auth-response-headers`

Comma-separated list of response header names from the auth service to forward to the upstream backend on auth success. Common pattern: the auth service returns `X-Auth-User: alice` on 200, this annotation makes that header available to the backend application.

```yaml
annotations:
  nginx.ingress.kubernetes.io/auth-url: "https://auth.example.com/check"
  nginx.ingress.kubernetes.io/auth-response-headers: "X-Auth-User, X-Auth-Roles"
```

One `set-header` directive per unique header across all ingresses; per-route gating happens via the plugin's per-ingress `extract_headers` SPOE arg — routes that didn't list a header have its `txn` var unset, so the `var ... -m found` gate skips them.

For headers on failed authentication, such as `WWW-Authenticate`, enable the
[haproxy-ingress library](haproxy-ingress.md#configuration) and use its
`haproxy-ingress.github.io/auth-headers-fail` annotation.

`auth-snippet` is unsupported. For auth request headers, use
[`haproxy-ingress.github.io/auth-headers-request`](haproxy-ingress.md#haproxy-ingressgithubioauth-headers-request).

## SSL features

### `nginx.ingress.kubernetes.io/ssl-passthrough`

Enable TCP-level SSL passthrough (Layer 4) where HAProxy routes based on SNI without terminating SSL.

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: ssl-passthrough-example
  annotations:
    nginx.ingress.kubernetes.io/ssl-passthrough: "true"
spec:
  ingressClassName: haptic
  tls:
    - hosts:
        - secure.example.com
  rules:
    - host: secure.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: secure-backend
                port:
                  number: 443
```

**Notes**:

- Uses SNI-based routing in TCP mode
- Backend receives encrypted traffic and terminates SSL
- HTTP-level features (headers, path rewriting) aren't available for passthrough traffic
- Incoming client-cert mTLS (`auth-tls-*`) can't run on a passthrough host. Passthrough routes the connection by SNI to the TCP frontend in `mode tcp` and never terminates TLS, so HAProxy never sees the client certificate. If a host enables both, passthrough wins and the client-cert verification silently never runs.

## Canary deployments

### `nginx.ingress.kubernetes.io/canary`

Route a percentage or subset of traffic to a canary backend.

**Related annotations**:

| Annotation | Description |
|------------|-------------|
| `canary` | Mark this Ingress as a canary (`"true"`) |
| `canary-by-header` | Route to canary when this header is present |
| `canary-by-header-value` | Required header value (default: `always`) |
| `canary-by-header-pattern` | Regex pattern for header matching |
| `canary-by-cookie` | Route to canary when cookie value is `always` |
| `canary-weight` | Percentage of traffic to route to canary (0-100) |

**Priority order**: header > cookie > weight

The canary Ingress must share a host with a non-canary (main) Ingress. Mark the secondary Ingress with `canary: "true"` and it routes to the parent's host.

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: my-app-canary
  annotations:
    nginx.ingress.kubernetes.io/canary: "true"
    nginx.ingress.kubernetes.io/canary-by-header: "X-Canary"
    nginx.ingress.kubernetes.io/canary-weight: "20"
spec:
  ingressClassName: haptic
  rules:
    - host: app.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: my-app-canary
                port:
                  number: 80
```

Canary backends, header values, and weights are stored in maps. Introducing a
new header or cookie name adds a processing rule and requires a reload. A
`canary-by-header-pattern` also requires a reload when it changes. If two
canaries for one host define the same rule type, the first by namespace/name wins.

Main and canary Ingresses have separate rate limits. Set the rate-limit
annotation on each; a limit on the main Ingress doesn't cover canary traffic.

## Client certificate auth (mTLS)

Use `auth-tls-*` annotations to require client certificates signed by a trusted
CA. The Ingress also needs an HTTPS certificate for its hosts.

### `nginx.ingress.kubernetes.io/auth-tls-secret`

Reference to a Secret whose `ca.crt` field contains the CA bundle that signs the clients' certificates. The chart writes the CA to `ssl/<ns>-<secret>-client-ca.pem` and adds `[ca-file <path> verify <mode>]` to the crt-list line for every host on the annotated Ingress.

**Format**: `name` (resolves in the Ingress namespace) or `namespace/name`.

```yaml
annotations:
  nginx.ingress.kubernetes.io/auth-tls-secret: "client-ca"
```

Create the Secret from your client CA bundle in the Ingress's namespace:

```bash
kubectl -n default create secret generic client-ca --from-file=ca.crt=ca.crt
```

A host without a TLS certificate of its own keeps the certificate that answers it today (a wildcard or the default certificate) and still verifies the client. A request routed to the Ingress over a TLS connection whose Server Name Indication (SNI) isn't one of its hosts, or is absent, gets `421 Misdirected Request`, so the client reconnects with the right SNI.

Set an explicit `host` on every protected Ingress rule. HAPTIC rejects
protected rules without one.

### `nginx.ingress.kubernetes.io/auth-tls-verify-client`

Client certificate verification mode.

**Valid values**:

| nginx value | HAProxy verify mode | Behaviour |
|-------------|---------------------|-----------|
| `on` (default) | `required` | Reject connections without a valid client cert |
| `off` | (no-op) | Don't enable verification on this host — the entry is skipped, falling through to the default crt-list line |
| `optional` | `optional` | Verify when a cert is presented; allow connections without |
| `optional_no_ca` | `optional` | Same as `optional`; a certificate from an unknown CA still fails verification. Add the issuing CA to the trusted bundle to accept it |

```yaml
annotations:
  nginx.ingress.kubernetes.io/auth-tls-secret: "client-ca"
  nginx.ingress.kubernetes.io/auth-tls-verify-client: "optional"
```

`auth-tls-verify-depth` is unsupported. Restrict the trusted certificate authorities to those you
intend to accept; this doesn't enforce a maximum chain depth.

### `nginx.ingress.kubernetes.io/auth-tls-match-cn`

Regular expression the client certificate's subject distinguished name (DN) must match. HAPTIC matches the DN in RFC 2253 form, the same string nginx's `$ssl_client_s_dn` holds, and answers `403` when it doesn't match or no certificate was sent. Like ingress-nginx, the annotation only takes effect together with `auth-tls-secret`.

```yaml
annotations:
  nginx.ingress.kubernetes.io/auth-tls-secret: "client-ca"
  nginx.ingress.kubernetes.io/auth-tls-match-cn: "CN=(alice|bob)\\.example\\.com"
```

### `nginx.ingress.kubernetes.io/auth-tls-error-page`

URL to redirect to (302) when client certificate verification fails.

```yaml
annotations:
  nginx.ingress.kubernetes.io/auth-tls-secret: "client-ca"
  nginx.ingress.kubernetes.io/auth-tls-error-page: "https://example.com/cert-required"
```

The redirect can only run after a successful TLS handshake. With the default
`verify required`, a missing or invalid certificate aborts the handshake: the
client sees a TLS error, not this page. `optional` still rejects an invalid
certificate; it only permits clients that send no certificate.

### `nginx.ingress.kubernetes.io/auth-tls-pass-certificate-to-upstream`

When `"true"`, forwards the verified client certificate and subject DN to the upstream backend as HTTP headers.

```yaml
annotations:
  nginx.ingress.kubernetes.io/auth-tls-secret: "client-ca"
  nginx.ingress.kubernetes.io/auth-tls-pass-certificate-to-upstream: "true"
```

## Web application firewall (`modsecurity`)

`nginx.ingress.kubernetes.io/modsecurity-snippet` and `enable-modsecurity` **are** supported, via the bundled SPOA hub **Coraza** WAF plugin (auto-enabled when the nginx-ingress or haproxy-ingress library is on). The `modsecurity-snippet` body (ModSecurity `SecRule` directives) is scanned into a per-Ingress `coraza-app.map` entry; `enable-modsecurity: "true"` sends the route to the WAF in detect mode, like ingress-nginx's detection-only default under the default opt-in dispatch; `enable-modsecurity: "false"` adds the route to `coraza-disabled.map` so the WAF skips it. See the [SPOA Hub operations guide](../operations/spoa-hub.md) for the Coraza plugin's full configuration surface.

## Request mirroring

Set `nginx.ingress.kubernetes.io/mirror-target` to copy requests to another
backend. Enable the [mirror plugin](../operations/spoa-hub.md#enabling-the-hub)
with `spoaHub.plugins.mirror.enabled: true`. The target's response is discarded.

The annotation uses `scheme://host[:port]$request_uri`; the plugin preserves the
incoming path and query. Multiple mirror targets are supported. Adding,
changing, or removing a target updates routing maps without reloading HAProxy
or changing the hub configuration.

These constraints **fail the config** with an actionable message rather than silently doing nothing: the mirror plugin must be enabled, and the Ingress must define a `host` (host-less / default-backend mirroring is unsupported). `mirror-host` and `mirror-request-body: off` **aren't** honoured — the plugin always forces the mirrored Host to the target authority and always forwards the buffered request body.

## Unsupported annotations

The following nginx-ingress annotations aren't supported:

| Annotation | Reason |
|------------|--------|
| `mirror-host`, `mirror-request-body: off` | Only `mirror-target` is honoured (see [Request Mirroring](#request-mirroring)); the plugin forces the mirrored Host to the target authority and always forwards the buffered body |
| `enable-opentelemetry`, `opentelemetry-*` | Not mapped; use [HAPTIC tracing settings](../reference.md#logging-and-templating) |
| `enable-opentracing`, `opentracing-*` | Not mapped; use [HAPTIC tracing settings](../reference.md#logging-and-templating) |
| `server-snippet` | Nginx server-level directives have no HAProxy equivalent |
| `proxy-max-temp-file-size` | HAProxy uses in-memory buffering, no temp file concept |
| `stream-snippet` | Nginx stream directives have no HAProxy equivalent |
| `auth-snippet` | Freeform nginx configuration can't be translated to HAProxy; the haproxy-ingress library's `auth-headers-request` covers the common use case |
| `session-cookie-hash` | HAProxy's dynamic-cookie hashing isn't selectable; the value is ignored with a rendered warning |
| `auth-tls-verify-depth`, `proxy-ssl-verify-depth` | No per-host or per-server depth limit; configuring trusted certificate authorities doesn't enforce a maximum chain depth |
| `proxy-ssl-server-name` | Not read; control SNI toward the upstream via `proxy-ssl-name` |
| `canary-weight-total` | The canary weight base is fixed at 100 |
| `custom-headers` | Not read; set response headers with `custom-response-headers` |
| `ssl-ciphers` | Ciphers apply to every TLS host; set them with `extraContext.tls.ciphers` |
| `enable-access-log` | Access logging can't be switched off per Ingress; `extraContext.accessLog.suppress.successful` drops successful requests fleet-wide |
| `proxy-buffering`, `proxy-buffers-number`, `proxy-busy-buffers-size`, `client-body-buffer-size` | HAProxy streams through fixed-size memory buffers; see [Buffering](#buffering) |
| `auth-cache-key`, `auth-cache-duration`, `auth-keepalive*`, `auth-proxy-set-headers`, `auth-request-redirect`, `auth-signin-redirect-param`, `auth-always-set-cookie` | Not configurable for the external-auth plugin; every request is checked against `auth-url` |

[Annotation compatibility](../annotation-compatibility.md#ingress-nginx) lists every ingress-nginx annotation with its status.

An `nginx.ingress.kubernetes.io/*` annotation that isn't in that list — usually a typo — has no effect. HAPTIC records an `UnknownAnnotation` Warning Event on the Ingress naming it:

```bash
kubectl get events --field-selector reason=UnknownAnnotation
```

## Watched Resources

This library watches the following additional resources:

- **Secrets** (`v1/secrets`) — read for basic-auth credentials (`auth-secret`), incoming client-CA bundles (`auth-tls-secret`), and upstream TLS material (`proxy-ssl-secret`)

<a id="annotation-inventory"></a>

See [annotation compatibility](../annotation-compatibility.md#ingress-nginx) for the complete migration table.

## Access-log fields

The library contributes `mtls_verify` and `mtls_cn` to the
[structured access log](../operations/access-logging.md) when any Ingress
sets `auth-tls-secret` or `auth-tls-pass-certificate-to-upstream`. Its
rate-limit and WAF fail-closed gates also name themselves in the `denied_by`
field (`rate_limit_local`, `rate_limit_connections`, `basic_auth`,
`waf_policy_unavailable`).

<a id="extension-points"></a>
<a id="extension-points-used"></a>

For custom behavior, use the [base extension points](base.md#extension-points)
and [write a template snippet](../templating.md).

## See also

- [Template Libraries Overview](../template-libraries.md) - How template libraries work
- [Base Library](base.md) - Core HAProxy template
- [Ingress Library](ingress.md) - Standard Ingress support
- [HAProxy Ingress Library](haproxy-ingress.md) - `haproxy-ingress.github.io/*` annotations
- [HAProxyTech Library](haproxytech.md) - `haproxy.org/*` annotations
- [Nginx Ingress Documentation](https://kubernetes.github.io/ingress-nginx/user-guide/nginx-configuration/annotations/) - Original annotation reference
