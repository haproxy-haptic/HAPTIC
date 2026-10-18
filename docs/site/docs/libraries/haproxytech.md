# haproxytech library

Use this library when migrating Ingresses with `haproxy.org/*` annotations from
[HAProxy Technologies' Kubernetes Ingress Controller](https://github.com/haproxytech/kubernetes-ingress).
Review the supported annotations and caveats before switching traffic.

<a id="overview"></a>

The library is disabled by default. Enable it through the [configuration](#configuration)
below. For new routes, use [native HAPTIC annotations](haptic-annotations.md).
Both libraries can be enabled during migration; configure each feature through
one annotation family to avoid conflicts.

Watch the `haproxy.org/*` annotations render to HAProxy config live:

<div class="pg-embed" markdown data-scenario="haproxytech" data-facade="spec.templateSnippets.backend-directives-150-haproxytech-load-balance" data-tab="haproxy.cfg" data-controls="tabs,resources" data-title="haproxy.org/* annotations rendered" data-height="440">

<p class="pg-task" markdown>In the **Resources** panel, change the `shop` Ingress's `haproxy.org/load-balance` value from `leastconn` to `source`, then watch the shop backend's `balance` line update in the `haproxy.cfg` tab.</p>

<details class="pg-hint" markdown>
<summary>What to expect</summary>

The shop backend changes from `balance leastconn` to `balance source`. HAProxy
then selects a server by client IP hash instead of the current connection count.

</details>

</div>

Before moving traffic, check [annotation compatibility](../annotation-compatibility.md)
and follow the [migration guide](../migrating.md#from-haproxytechkubernetes-ingress).

**Important notes:**

- Annotations apply to **Ingress resources only** (not Services)
- Gateway API resources (HTTPRoute, GRPCRoute) use filters instead of annotations — see [Gateway API Library](gateway.md)
- All annotations use the `haproxy.org/` prefix

## Configuration

Apply the following Helm values through your [values file](../deploying-with-helm.md#change-settings).
The annotation examples on this page belong under an Ingress's `metadata.annotations`;
quote all annotation values.

```yaml
controller:
  templateLibraries:
    haproxytech:
      enabled: true  # Set to enable this opt-in library (disabled by default)
```

## Access control & IP filtering

### `haproxy.org/allow-list`

Allow only the listed IP addresses or CIDR ranges to access the Ingress.

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: protected-api
  annotations:
    haproxy.org/allow-list: "192.168.1.0/24, 10.0.0.1"
spec:
  ingressClassName: haptic
  rules:
    - host: api.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: api-service
                port:
                  number: 8080
```

**Related annotations**: Can be combined with `deny-list`

### `haproxy.org/deny-list`

Deny access from the listed IP addresses or CIDR ranges.

```yaml
annotations:
  haproxy.org/deny-list: "203.0.113.0/24, 198.51.100.50"
```

**Related annotations**: Can be combined with `allow-list`

### `haproxy.org/whitelist`

**Status**: Supported (deprecated alias)

Deprecated alias for `allow-list`, honoured only when `allow-list` is absent on the same Ingress. Kept for upstream parity — prefer `allow-list` for new Ingresses.

**Note**: If both `allow-list` and `whitelist` are set, `allow-list` wins and `whitelist` is ignored.

### `haproxy.org/blacklist`

**Status**: Supported (deprecated alias)

Deprecated alias for `deny-list`, honoured only when `deny-list` is absent on the same Ingress. Kept for upstream parity — prefer `deny-list` for new Ingresses.

**Note**: If both `deny-list` and `blacklist` are set, `deny-list` wins and `blacklist` is ignored.

## CORS configuration

### `haproxy.org/cors-enable`

Enable CORS (Cross-Origin Resource Sharing) processing for the ingress.

```yaml
annotations:
  haproxy.org/cors-enable: "true"
  haproxy.org/cors-allow-origin: "*"
  haproxy.org/cors-allow-methods: "GET, POST, PUT, DELETE"
  haproxy.org/cors-allow-headers: "Content-Type, Authorization"
```

CORS headers are added to backend responses. To have HAProxy answer preflight
requests itself, set [`cors-respond-to-options`](#haproxyorgcors-respond-to-options).

**Dependencies**: All other `cors-*` annotations require `cors-enable: "true"`

### `haproxy.org/cors-allow-origin`

Specifies allowed origins for CORS requests. Supports wildcard (`*`), exact URL, or regex pattern.

Choose one value, for example an exact origin:

```yaml
haproxy.org/cors-allow-origin: "https://example.com"
```

Use `"*"` for any origin, or a regular expression such as
`^https://(.+\.)?example\.com$` to match a domain and its subdomains.

**Dependencies**: Requires `cors-enable: "true"`

### `haproxy.org/cors-allow-methods`

Specifies allowed HTTP methods for CORS requests.

**Valid values**: GET, POST, PUT, DELETE, HEAD, CONNECT, OPTIONS, TRACE, PATCH

```yaml
haproxy.org/cors-allow-methods: "GET, POST, PUT, DELETE, OPTIONS"
```

**Dependencies**: Requires `cors-enable: "true"`

### `haproxy.org/cors-allow-headers`

Specifies allowed request headers for CORS requests.

```yaml
haproxy.org/cors-allow-headers: "Content-Type, Authorization, X-Requested-With"
```

**Dependencies**: Requires `cors-enable: "true"`

### `haproxy.org/cors-allow-credentials`

Indicates whether credentials (cookies, authorization headers) can be included in CORS requests.

```yaml
haproxy.org/cors-allow-credentials: "true"
```

**Dependencies**: Requires `cors-enable: "true"`

**Note**: When `cors-allow-credentials: "true"`, `cors-allow-origin` can't be `*` (must be specific origin)

### `haproxy.org/cors-max-age`

Specifies how long (in seconds) preflight request results can be cached.

```yaml
haproxy.org/cors-max-age: "3600"  # 1 hour
```

**Dependencies**: Requires `cors-enable: "true"`

### `haproxy.org/cors-respond-to-options`

When `"true"`, HAProxy answers the CORS preflight (an `OPTIONS` request) itself with a `204 No Content` instead of forwarding it to the backend. The `Access-Control-*` headers are added via `http-after-response`, so they apply to this synthetic response too. This matches the upstream HAProxy Kubernetes Ingress Controller, where preflight answering is opt-in.

```yaml
haproxy.org/cors-enable: "true"
haproxy.org/cors-allow-origin: "https://app.example.com"
haproxy.org/cors-respond-to-options: "true"
```

**Dependencies**: Requires `cors-enable: "true"`

## Rate limiting

### `haproxy.org/rate-limit-requests`

Maximum number of requests allowed in the specified period (per source IP).

```yaml
annotations:
  haproxy.org/rate-limit-requests: "100"
  haproxy.org/rate-limit-period: "1m"
  haproxy.org/rate-limit-size: "100k"
  haproxy.org/rate-limit-status-code: "429"
```

Each route has a separate rate budget per client IP. Counters survive HAProxy
reloads. Changing an existing route's limit deploys without a reload.

**Dependencies**: Other rate-limit annotations require this to be set

**Related annotations**: `rate-limit-period`, `rate-limit-size`, `rate-limit-status-code`

### `haproxy.org/rate-limit-period`

Time window for rate limiting. Supports duration format (for example, `10s`, `1m`, `1h`).

**Default**: `1s` (1 second)

```yaml
haproxy.org/rate-limit-period: "1m"
```

**Dependencies**: Requires `rate-limit-requests` to be set

### `haproxy.org/rate-limit-size`

Size of the stick-table that tracks the route's clients. Supports suffixes `k` (thousands) or `M` (millions). Routes sharing a period share one table, sized by the largest request among them.

**Default**: `100k` (100,000 entries)

```yaml
haproxy.org/rate-limit-size: "100k"  # Track 100,000 IPs
```

**Dependencies**: Requires `rate-limit-requests` to be set

### `haproxy.org/rate-limit-status-code`

HTTP status code to return when rate limit is exceeded.

**Default**: `403` (Forbidden)

**Common values**: `403`, `429` (Too Many Requests), `503` (Service Unavailable)

```yaml
haproxy.org/rate-limit-status-code: "429"
```

**Dependencies**: Requires `rate-limit-requests` to be set

### `haproxy.org/rate-limit-whitelist`

Comma-separated IP addresses or CIDR ranges that are exempt from the rate-limit deny. Whitelisted sources are still tracked but are never denied, mirroring haproxy-ingress' `limit-whitelist`.

```yaml
annotations:
  haproxy.org/rate-limit-requests: "10"
  haproxy.org/rate-limit-period: "10s"
  haproxy.org/rate-limit-whitelist: "10.0.0.0/8, 192.168.1.5"
```

**Dependencies**: Requires `rate-limit-requests` to be set

## Request/response header manipulation

### `haproxy.org/request-set-header`

Set or modify request headers before forwarding to backend. Multiline format with each line containing `HeaderName HeaderValue`.

```yaml
annotations:
  haproxy.org/request-set-header: |
    X-Forwarded-Proto https
    X-Custom-Header custom-value
```

Changing a value for an existing header name deploys without a reload.

**Related annotations**: `response-set-header`, `set-host`

### `haproxy.org/response-set-header`

Set or modify response headers before returning to client. Multiline format with each line containing `HeaderName HeaderValue`.

```yaml
annotations:
  haproxy.org/response-set-header: |
    Strict-Transport-Security "max-age=31536000; includeSubDomains"
    X-Frame-Options DENY
    X-Content-Type-Options nosniff
```

Changing a value for an existing header name deploys without a reload.

**Related annotations**: `request-set-header`

### `haproxy.org/set-host`

Modify the Host header after backend selection. Different from `request-set-header Host` in timing.

```yaml
haproxy.org/set-host: "internal-api.example.svc.cluster.local"
```

The override value lives in `reqhdr-host.map` keyed on the backend, so changing the upstream Host is a map-only, reload-free update.

**Note**: This happens after backend selection, while `request-set-header Host` happens before.

### `haproxy.org/forwarded-for`

`"true"` keeps the [default `X-Forwarded-For` handling](../operations/security.md#forwarded-headers):
the backend receives the client address, and a value the client sent is dropped
unless it comes from a trusted proxy. Upstream keeps the client's value instead.
`"false"` forwards the client's `X-Forwarded-For` unchanged. Any other value
fails the render.

**Default**: `true`

```yaml
haproxy.org/forwarded-for: "false"
```

## Path manipulation

### `haproxy.org/path-rewrite`

Rewrite request path using regex patterns before forwarding to backend. Supports two formats: single parameter (matches all paths) or two parameters (regex pattern and replacement). A bare value, or a prefix strip (`^<prefix>(.*)` with `<new prefix>\1` or a plain `<new path>` as the replacement, `<prefix>` without regex metacharacters), is applied from a per-route map on the HTTP frontends and keeps the route reload-free; any other pattern is a `replace-path` rule in the backend.

```yaml
# Strip prefix: /api/v1/users -> /users
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: path-rewrite-example
  annotations:
    haproxy.org/path-rewrite: |
      ^/api/v1/(.*) /\1
spec:
  ingressClassName: haptic
  rules:
    - host: api.example.com
      http:
        paths:
          - path: /api/v1
            pathType: Prefix
            backend:
              service:
                name: api-service
                port:
                  number: 8080
```

**Related annotations**: Similar to Gateway API URLRewrite filter

## Request redirect

### `haproxy.org/request-redirect`

Redirect requests to a different host/port. Supports formats: `example.com`, `example.com:8888`, `https://example.com`, `http://example.com`.

```yaml
annotations:
  haproxy.org/request-redirect: "https://new.example.com"
  haproxy.org/request-redirect-code: "301"
```

**Related annotations**: `request-redirect-code`

### `haproxy.org/request-redirect-code`

HTTP status code for redirect.

**Default**: `302` (Found)

**Valid values**: `301` (Moved Permanently), `302` (Found), `303` (See Other), `307` (Temporary Redirect), `308` (Permanent Redirect)

```yaml
haproxy.org/request-redirect-code: "301"
```

**Dependencies**: Requires `request-redirect` to be set

## SSL/TLS Configuration

### `haproxy.org/ssl-redirect`

Force HTTPS redirect for HTTP requests. Automatically enabled when TLS secrets are present in the ingress.

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: ssl-redirect-example
  annotations:
    haproxy.org/ssl-redirect: "true"
    haproxy.org/ssl-redirect-code: "301"
spec:
  ingressClassName: haptic
  tls:
    - hosts:
        - example.com
      secretName: example-tls
  rules:
    - host: example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: web-service
                port:
                  number: 80
```

**Related annotations**: `ssl-redirect-code`, `ssl-redirect-port`

### `haproxy.org/ssl-redirect-code`

HTTP status code for SSL redirect.

**Default**: `302`

**Valid values**: `301`, `302`, `303`, `307`, `308`

```yaml
haproxy.org/ssl-redirect-code: "301"
```

**Dependencies**: Requires `ssl-redirect: "true"`

### `haproxy.org/ssl-redirect-port`

Redirect HTTP requests to HTTPS on an explicit port instead of the default `https://` scheme. The original request URI is preserved. Requires `ssl-redirect: "true"` (or `extraContext.sslRedirectDefault: true`), and uses `ssl-redirect-code` for the status code (default `302`). Must be a positive integer port — other values fail the render.

```yaml
annotations:
  haproxy.org/ssl-redirect: "true"
  haproxy.org/ssl-redirect-port: "8443"
```

**Dependencies**: Requires `ssl-redirect: "true"`

**Related annotations**: `ssl-redirect`, `ssl-redirect-code`

### `haproxy.org/ssl-passthrough`

Enable TCP mode SSL passthrough (Layer 4) for specific ingresses while allowing SSL termination for others. Uses SNI-based routing with Unix socket loopback to support mixed passthrough and termination traffic.

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: ssl-passthrough-example
  annotations:
    haproxy.org/ssl-passthrough: "true"
spec:
  ingressClassName: haptic
  tls:
    - hosts:
        - secure.example.com
      secretName: example-tls
  rules:
    - host: secure.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: secure-service
                port:
                  number: 443
```

**Notes**:

- Uses Unix socket loopback pattern to support mixed passthrough and termination
- TCP frontend extracts SNI without terminating SSL
- Passthrough traffic routes directly to backend pods
- Non-passthrough traffic routes to Unix socket frontend for SSL termination
- PROXY protocol v2 preserves client IP information

**Warning**: For passthrough traffic, HTTP-level features (headers, path rewriting, etc.) are unavailable. Non-passthrough traffic on other hosts continues to support all HTTP features.

### `haproxy.org/server-ssl`

Enable SSL/TLS connection to backend servers.

```yaml
haproxy.org/server-ssl: "true"
```

**Related annotations**: `server-proto`, `server-crt`, `server-ca`

### `haproxy.org/server-proto`

Backend protocol (typically `h2` for HTTP/2).

```yaml
haproxy.org/server-ssl: "true"
haproxy.org/server-proto: "h2"
```

**Dependencies**: Typically used with `server-ssl`

### `haproxy.org/server-crt`

Client certificate for mTLS (mutual TLS) to backend. References a Secret containing `tls.crt` and `tls.key`. Supports cross-namespace format `namespace/secretname`. Unlike Gateway API cross-namespace references, this isn't gated by a ReferenceGrant — the Secret resolves against any namespace the controller watches, bounded only by the `watchedResources` scope and the controller's RBAC.

```yaml
haproxy.org/server-ssl: "true"
haproxy.org/server-crt: "default/client-cert"
haproxy.org/server-ca: "default/ca-cert"
```

**Dependencies**: Requires `server-ssl: "true"`

**Related annotations**: `server-ca` (required for verification)

### `haproxy.org/server-ca`

CA certificate for verifying backend server certificates. References a Secret containing `tls.crt`. Supports cross-namespace format `namespace/secretname`. Unlike Gateway API cross-namespace references, this isn't gated by a ReferenceGrant — the Secret resolves against any namespace the controller watches, bounded only by the `watchedResources` scope and the controller's RBAC.

```yaml
haproxy.org/server-ssl: "true"
haproxy.org/server-ca: "default/ca-cert"
```

**Dependencies**: Requires `server-ssl: "true"`

**Related annotations**: `server-crt` (for mTLS)

## Backend health checks & connection management

### `haproxy.org/check`

Toggle active health checks for the backend's servers. Health checks are on by default; set the value to `"false"` to turn them off. Only `"true"` and `"false"` are accepted — any other value fails the render. The toggle renders as the first token of the backend's `default-server` line, so it applies to every server in the pool and endpoint changes still avoid a reload.

```yaml
annotations:
  haproxy.org/check: "false"
```

**Related annotations**: `check-http`, `check-interval`, `timeout-check`

### `haproxy.org/check-http`

HTTP URI or full HTTP request for health checks.

```yaml
# Simple URI
haproxy.org/check: "true"
haproxy.org/check-http: "/health"

# Full HTTP request
haproxy.org/check-http: "HEAD /health HTTP/1.1"
```

**Dependencies**: Requires `check: "true"`

### `haproxy.org/check-interval`

Set the interval between active health checks (for example, `10s`, `1m`). The value becomes the `inter` parameter on the backend's `default-server`. Ignored when health checks are disabled with `haproxy.org/check: "false"`, since an interval on an unchecked server has no meaning.

```yaml
annotations:
  haproxy.org/check-interval: "10s"
```

**Dependencies**: None (has no effect when `haproxy.org/check: "false"`)

**Related annotations**: `check`, `check-http`, `timeout-check`

### `haproxy.org/timeout-check`

Timeout for health check responses. Supports duration format.

**Default**: Not set by this library; HAProxy uses its health-check timeout defaults

```yaml
haproxy.org/timeout-check: "3s"
```

### `haproxy.org/pod-maxconn`

Maximum connections to each backend server across the HAProxy replicas. HAPTIC divides the value among ready HAProxy pods, rounding the pod count up to a power of two.

```yaml
haproxy.org/pod-maxconn: "100"
```

**Behavior**:

The annotation value represents the **total** maximum connections across all HAProxy replicas. The controller automatically:

- Counts only **Running and Ready** HAProxy pods (Pending, CrashLoopBackOff, SysctlForbidden, and other non-ready pods are excluded)
- Quantizes the pod count to the **next power of 2** to avoid HAProxy reload cascades when pods scale up or down
- Divides the total by the quantized count (ceiling rounding)
- Applies the per-pod value to each server line

!!! note
    The power-of-2 quantization means the effective per-pod `maxconn` only changes when the ready pod count crosses a power-of-2 boundary (1, 2, 4, 8, 16, and so on). This prevents unnecessary HAProxy reloads during scaling events. The trade-off is that the actual total capacity may be lower than the annotation value when the pod count isn't an exact power of 2.

**Quantization reference** (for `pod-maxconn: 200`):

| Ready pods | Effective count | `maxconn` per pod |
|------------|-----------------|-----------------|
| 1          | 1               | 200             |
| 2          | 2               | 100             |
| 3-4        | 4               | 50              |
| 5-8        | 8               | 25              |
| 9-16       | 16              | 13              |

**Fallback behavior**: If no Running and Ready HAProxy pods are discovered yet (for example, during initial startup), the full annotation value is used temporarily until pod discovery completes.

**Dependencies**: Requires HAProxy pod discovery to be operational for automatic division

### `haproxy.org/scale-server-slots`

**Status**: Not Implemented (removed)

This annotation has no effect. HAPTIC adds and removes backend servers as endpoints change; no reserved slots are needed.

**Migration**: Remove the annotation. Setting it emits an `UnsupportedAnnotation` Warning Event on the Ingress and changes no configuration.

## Load balancing algorithms

### `haproxy.org/load-balance`

Load balancing algorithm for distributing traffic across backend servers.

**Default**: `roundrobin`

**Valid values**: `roundrobin`, `static-rr`, `leastconn`, `first`, `source`, `random`, plus the parameterized `uri`, `url_param(name)`, `hdr(name)`, `rdp-cookie(name)`

```yaml
annotations:
  haproxy.org/load-balance: "leastconn"
```

## Session persistence

### `haproxy.org/cookie-persistence`

Enable sticky sessions using dynamic cookies. The cookie value is dynamically generated per server.

```yaml
annotations:
  haproxy.org/cookie-persistence: "SERVERID"
```

**Note**: This annotation emits the cookie directive without a shared
`dynamic-cookie-key`. For affinity across HAProxy replicas, use the
[native affinity annotation](haptic-annotations.md#rewriting-retries-and-session-affinity), which also
configures a consistent key. Remove the vendor cookie annotation when switching.

### `haproxy.org/cookie-persistence-no-dynamic`

Emit a static cookie directive. This annotation alone doesn't
assign cookie values to servers; it requires custom server templates that do so.
Use [native cookie affinity](haptic-annotations.md#rewriting-retries-and-session-affinity) for the bundled
server templates.

```yaml
annotations:
  haproxy.org/cookie-persistence-no-dynamic: "SERVERID"
```

**Note**: Mutually exclusive with `cookie-persistence`. Custom static server cookie
values must identify the same backend endpoint on every HAProxy replica.

## Timeouts

### `haproxy.org/timeout-server`

Maximum time to wait for backend server response. Supports duration format.

**Default**: `50s`

```yaml
haproxy.org/timeout-server: "30s"
```

Changing this timeout deploys without a reload.

### `haproxy.org/timeout-client`

**Status**: Not Implemented

Maximum inactivity time on the client side. The haproxytech library doesn't emit a per-backend `timeout client` (it would have no effect — `timeout client` only applies in frontend/defaults sections).

**Workaround**: Set the global `timeout client` via the `defaults-settings-300-timeouts` snippet override. See [Base Library](base.md#injecting-custom-configuration).

### `haproxy.org/timeout-connect`

Maximum time to wait for backend connection. Supports duration format.

**Default**: `100ms` with the default base library

```yaml
haproxy.org/timeout-connect: "10s"
```

### `haproxy.org/timeout-http-request`

**Status**: Not Implemented

The haproxytech library doesn't process this annotation. The equivalent exists in the `haproxy-ingress` library as `haproxy-ingress.github.io/timeout-http-request`.

**Workaround**: Either add the `haproxy-ingress.github.io/timeout-http-request` annotation (see [haproxy-ingress library](haproxy-ingress.md)), or override `defaults-settings-300-timeouts` globally.

### `haproxy.org/timeout-http-keep-alive`

**Status**: Not Implemented

The haproxytech library doesn't process this annotation. The equivalent exists in the `haproxy-ingress` library as `haproxy-ingress.github.io/timeout-keep-alive`.

**Workaround**: Either add the `haproxy-ingress.github.io/timeout-keep-alive` annotation (see [haproxy-ingress library](haproxy-ingress.md)), or override `defaults-settings-300-timeouts` globally.

### `haproxy.org/timeout-queue`

Maximum time a request can wait in queue when all backend servers are busy. Supports duration format.

**Default**: Not set by this library; HAProxy falls back to the connection timeout

```yaml
haproxy.org/timeout-queue: "30s"
```

### `haproxy.org/timeout-tunnel`

Maximum inactivity time on tunnel connections (WebSocket, CONNECT). Supports duration format.

**Default**: Not set by this library; HAProxy uses the client/server timeouts

```yaml
haproxy.org/timeout-tunnel: "2h"
```

Changing this timeout deploys without a reload.

## Request capture & logging

### `haproxy.org/request-capture`

Capture request data for logging. Multiline format with HAProxy sample expressions.

```yaml
annotations:
  haproxy.org/request-capture: |
    hdr(User-Agent)
    path
    method
  haproxy.org/request-capture-len: "256"
```

**Related annotations**: `request-capture-len`

### `haproxy.org/request-capture-len`

Maximum length for captured request data.

**Default**: `128`

```yaml
haproxy.org/request-capture-len: "256"
```

**Dependencies**: Applies to `request-capture` expressions

## Source IP detection

### `haproxy.org/src-ip-header`

Extract true client IP from a specific header (useful when behind proxies/CDNs).

```yaml
haproxy.org/src-ip-header: "CF-Connecting-IP"
```

Use this only when traffic reaches HAProxy through a trusted proxy that overwrites
the header. Direct clients can forge it. Choose the header your proxy supplies,
such as `CF-Connecting-IP`, `X-Forwarded-For`, or `True-Client-IP`. Naming
`X-Forwarded-For` also keeps the client's `X-Forwarded-For` for the route.

## Advanced backend configuration

### `haproxy.org/backend-config-snippet`

Inject raw HAProxy configuration directives into backend section. Multiline YAML string.

```yaml
annotations:
  haproxy.org/backend-config-snippet: |
    stick-table type string len 32 size 100k expire 30m
    stick store-response res.cook(JSESSIONID)
    http-send-name-header X-Backend-Server
```

HAPTIC rejects the annotation unless
`controller.config.templatingSettings.extraContext.ingressPermissions.allowRawHAProxyConfig`
is `true`; see [Raw configuration annotations](../operations/security.md#raw-configuration-annotations). It inserts the snippet verbatim and checks
the resulting configuration with `haproxy -c` before deployment. Admission
rejects syntax errors. Valid syntax doesn't establish that the directives are
safe.

### `haproxy.org/send-proxy-protocol`

Enable PROXY protocol for backend connections to preserve client IP information.

**Valid values**: `proxy`, `proxy-v1`, `proxy-v2`, `proxy-v2-ssl`, `proxy-v2-ssl-cn`

```yaml
haproxy.org/send-proxy-protocol: "proxy-v2"
```

**Note**: Backend application must support PROXY protocol.

### `haproxy.org/standalone-backend`

**Status**: Not Implemented (Not Planned)

Create a dedicated backend for this ingress instead of sharing backends across ingresses.

**Note**: This controller's architecture already generates standalone backends (one backend per ingress+service+port combination) rather than sharing backends across ingresses. Each unique combination of `<namespace>_<ingress-name>_svc_<service-name>_<port-name>` gets its own dedicated backend, making this annotation redundant. Implementation isn't planned.

## Authentication

### `haproxy.org/auth-type`

Type of authentication to enforce. Currently only `basic-auth` is supported.

**Valid values**: `basic-auth`

```yaml
annotations:
  haproxy.org/auth-type: basic-auth
  haproxy.org/auth-secret: auth-credentials
  haproxy.org/auth-realm: "API Access"
```

**Dependencies**: Requires `auth-secret` to be set

**Related annotations**: `auth-secret`, `auth-realm`

**Notes**:

- Secret format: Opaque secret where key=username, value=base64-encoded password hash
- Supports cross-namespace secrets: `namespace/secretname`
- Automatic deduplication: multiple ingresses sharing the same secret generate a single userlist
- Accepted hashes by default: bcrypt (`$2a$`, `$2b$`, `$2y$`), SHA-256 crypt (`$5$`), SHA-512 crypt (`$6$`), and yescrypt (`$y$`). HAPTIC refuses `$apr1$`, `$1$`, Data Encryption Standard (DES) crypt, and plaintext; see [accepted formats](../operations/security.md#basic-auth-password-hashes). See [password hash performance](../operations/performance.md#password-hash-performance) for the cost trade-off.

### `haproxy.org/auth-secret`

Reference to Kubernetes Secret containing authentication credentials. Supports cross-namespace format `namespace/secretname`. Unlike Gateway API cross-namespace references, this isn't gated by a ReferenceGrant — the Secret resolves against any namespace the controller watches, bounded only by the `watchedResources` scope and the controller's RBAC.

```yaml
haproxy.org/auth-secret: "auth-credentials"
```

For a cross-namespace Secret, use `"auth-system/shared-credentials"`.

Create the Secret in the Ingress namespace. Each key is a username; its value is
the password hash, without a `username:` prefix. With Apache `htpasswd` installed:

```bash
umask 077
htpasswd -n -B admin | cut -d: -f2 > admin.hash
kubectl -n default create secret generic auth-credentials --from-file=admin=admin.hash
rm admin.hash
```

Serve the protected route over HTTPS. For hash algorithms and validation cost,
see [password hash performance](../operations/performance.md#password-hash-performance).

### `haproxy.org/auth-realm`

Authentication realm displayed in browser's authentication prompt.

**Default**: `Protected-Content`

```yaml
haproxy.org/auth-realm: "API Access"
```

**Dependencies**: Requires `auth-type: basic-auth` and `auth-secret`

**Note**: Like the upstream controller, the library automatically replaces spaces with dashes, so `"API Access"` renders as `realm "API-Access"` — you don't need to hyphenate the value yourself.

## Known limitations

### Not implemented

1. **Service-level annotations** - Annotations on Service resources aren't supported. Only Ingress annotations are implemented.

2. **Deprecated annotations** - `whitelist` and `blacklist` are honoured as deprecated aliases of `allow-list` / `deny-list` (only when the canonical key is absent). `ingress.class` isn't implemented — set `spec.ingressClassName` instead.

3. **RequestMirror equivalent** - No annotation-based traffic mirroring. Use the Gateway API `RequestMirror` filter and the bundled [mirror plugin](../operations/spoa-hub.md).

<a id="implementation-differences-from-haproxy-tech"></a>
<a id="watched-resources"></a>
<a id="implementation-status-summary"></a>

See [annotation compatibility](../annotation-compatibility.md#haproxytech) for the
complete migration table, including changed behavior and unsupported keys.

## Access-log fields

The library contributes `captured_headers` (HAProxy's `%hr`) to the
[structured access log](../operations/access-logging.md) when any Ingress
sets `haproxy.org/request-capture` — without it, the annotation configures
captures that nothing reads.

<a id="extension-points"></a>
<a id="features-shared-state-initialization"></a>
<a id="frontend-filters-http-frontend-requestresponse-filters"></a>
<a id="backend-directives-per-backend-directives"></a>
<a id="other-extension-points"></a>
<a id="injecting-custom-annotations"></a>

For custom behavior, use the [base extension points](base.md#extension-points)
and [write a template snippet](../templating.md).

## See also

- [HAProxy Ingress Controller Documentation](https://www.haproxy.com/documentation/kubernetes-ingress/community/configuration-reference/ingress)
- [HAProxy Ingress Controller Source Code](https://github.com/haproxytech/kubernetes-ingress)
- [Gateway API Library](gateway.md) - For Gateway API resources (HTTPRoute, GRPCRoute)
- [Template Libraries Overview](../template-libraries.md) - How template libraries work
- [Base Library](base.md) - Extension points
- [Ingress Library](ingress.md) - Ingress resource support
