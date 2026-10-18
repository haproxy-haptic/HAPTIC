# `haproxy-ingress` library

Use this library when migrating Ingresses with `haproxy-ingress.github.io/*`
annotations from [haproxy-ingress](https://haproxy-ingress.github.io/). It covers
path matching, backend settings, session affinity, TLS, access control, HTTP
Strict Transport Security (HSTS), and Cross-Origin Resource Sharing (CORS).

<a id="overview"></a>

The library is disabled by default. Enable it to retain supported annotations,
and review the caveats below before switching traffic. For new configuration,
use [native HAPTIC annotations](haptic-annotations.md).

See the `haproxy-ingress.github.io/*` annotations render to HAProxy config live:

<div class="pg-embed" markdown data-scenario="haproxy-ingress" data-facade="spec.templateSnippets.backend-directives-630-haproxy-ingress-health-checks" data-tab="haproxy.cfg" data-controls="tabs,resources" data-title="haproxy-ingress.github.io/* annotations rendered" data-height="440">

<p class="pg-task" markdown>In the **Resources** panel, change the `shop` Ingress's `haproxy-ingress.github.io/health-check-uri` from `/healthz` to `/readyz`, then watch the shop backend's health-check line update in the `haproxy.cfg` tab.</p>

<details class="pg-hint" markdown>
<summary>What to expect</summary>

The `backend storefront_shop_svc_shop_http` section changes `option httpchk GET /healthz` to `option httpchk GET /readyz`, so HAProxy's active health check now probes `/readyz`. The `default-server` line keeps its `check inter 2s` (from `backend-check-interval`) — only the probed URI moves.

</details>

</div>

Before moving traffic, check [annotation compatibility](../annotation-compatibility.md)
and follow the [migration guide](../migrating.md#from-haproxy-ingress).

## Configuration

Apply the following Helm values through your [values file](../deploying-with-helm.md#change-settings).
The annotation examples on this page belong under an Ingress's `metadata.annotations`;
quote all annotation values.

```yaml
controller:
  templateLibraries:
    haproxyIngress:
      enabled: true  # Set to enable this opt-in library (disabled by default)
```

## Path matching

### `haproxy-ingress.github.io/path-type`

Controls how path matching is performed for paths with `pathType: ImplementationSpecific`.

**Valid values**: `regex`, `exact`, `prefix`, `begin`

A `regex` path matches from the start of the path, on the rule's host only. HAPTIC rejects a regex path whose groups or `[ ]` classes don't close within it, or that uses `\Q`, `\E`, `\c` or `(*`.

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: path-matching-example
  annotations:
    haproxy-ingress.github.io/path-type: "regex"
spec:
  ingressClassName: haptic
  rules:
    - host: api.example.com
      http:
        paths:
          - path: "^/api/v[0-9]+/users/[0-9]+$"
            pathType: ImplementationSpecific
            backend:
              service:
                name: users-service
                port:
                  number: 80
```

**Path type behaviors**:

| Value | Behavior | Example Path | Matches |
|-------|----------|--------------|---------|
| `regex` | Regular expression matching | `^/api/v[0-9]+/` | `/api/v1/`, `/api/v2/users` |
| `exact` | Exact string match | `/api/users` | Only `/api/users` |
| `prefix` | Path prefix (with segment boundaries) | `/api/` | `/api/`, `/api/users`, `/api/v1/` |
| `begin` | Legacy prefix (simple string prefix) | `/api` | `/api`, `/api/users`, `/apikey` |

This annotation affects only `pathType: ImplementationSpecific`. It's
ignored for `Exact` and `Prefix` paths.

## Backend configuration

### Timeouts

**Annotations**:

| Annotation | Description | Default |
|------------|-------------|---------|
| `timeout-connect` | Backend connection timeout | - |
| `timeout-server` | Backend response timeout | - |
| `timeout-queue` | Queue wait timeout | - |
| `timeout-tunnel` | Tunnel/WebSocket timeout | - |
| `timeout-http-request` | HTTP request timeout | - |
| `timeout-keep-alive` | Keep-alive timeout | - |

```yaml
annotations:
  haproxy-ingress.github.io/timeout-server: "60s"
  haproxy-ingress.github.io/timeout-connect: "10s"
  haproxy-ingress.github.io/timeout-queue: "30s"
```

`timeout-connect` and `timeout-queue` render as literal backend directives. `timeout-server` (and `timeout-tunnel`) are reload-free: the value moves into `backend-timeouts.map` as milliseconds keyed `<backend>|server` (here `my-backend|server 60000`), read by the uniform `set-timeout` line every backend carries.

### `haproxy-ingress.github.io/balance-algorithm`

Load balancing algorithm for the backend.

**Valid values**: `roundrobin`, `leastconn`, `source`, `first`, `random`, `static-rr`, `uri`, `url_param`, `hdr`, `rdp-cookie`

```yaml
annotations:
  haproxy-ingress.github.io/balance-algorithm: "leastconn"
```

### Connection limits

**Annotations**:

| Annotation | Description |
| ------------ | ------------- |
| `limit-connections` | Backend `fullconn` limit |
| `maxconn-server` | Per-server `maxconn` on the `default-server` line |
| `maxqueue-server` | Per-server `maxqueue` on the `default-server` line |

```yaml
annotations:
  haproxy-ingress.github.io/limit-connections: "1000"
  haproxy-ingress.github.io/maxconn-server: "50"
  haproxy-ingress.github.io/maxqueue-server: "100"
```

### Health checks

**Annotations**:

| Annotation | Description |
| ------------ | ------------- |
| `health-check-uri` | HTTP health check path — emitted as `option httpchk GET <uri>` |
| `backend-check-interval` | Emitted as `inter <value>` on the `default-server` line |
| `health-check-port` | Emitted as `port <n>` on the `default-server` line |
| `health-check-fall-count` | Emitted as `fall <n>` on the `default-server` line |
| `health-check-rise-count` | Emitted as `rise <n>` on the `default-server` line |

```yaml
annotations:
  haproxy-ingress.github.io/health-check-uri: "/healthz"
  haproxy-ingress.github.io/backend-check-interval: "5s"
  haproxy-ingress.github.io/health-check-port: "8082"
  haproxy-ingress.github.io/health-check-fall-count: "3"
  haproxy-ingress.github.io/health-check-rise-count: "2"
```

### `haproxy-ingress.github.io/agent-check-port`

Enable HAProxy's auxiliary agent check: HAProxy connects to an agent on each server at the given port, and the agent's reply can adjust the server's weight or state. `agent-check-port` is the enabler — setting `agent-check-addr`, `agent-check-interval`, or `agent-check-send` without it fails the render, because HAProxy disables an agent check that has no port.

**Related annotations**:

| Annotation | Description | Default |
|------------|-------------|---------|
| `agent-check-port` | Port the agent listens on (required to enable) | - |
| `agent-check-addr` | Address the agent listens on | server address |
| `agent-check-interval` | Interval between agent checks | HAProxy's `agent-inter` default |
| `agent-check-send` | String sent to the agent on connect | - |

```yaml
annotations:
  haproxy-ingress.github.io/agent-check-port: "9998"
  haproxy-ingress.github.io/agent-check-addr: "10.0.0.50"
  haproxy-ingress.github.io/agent-check-interval: "5s"
  haproxy-ingress.github.io/agent-check-send: "check"
```

### `haproxy-ingress.github.io/proxy-protocol`

Enable PROXY protocol when connecting to backend servers.

**Valid values**: `v1`, `v2`, `v2-ssl`, `v2-ssl-cn`

```yaml
annotations:
  haproxy-ingress.github.io/proxy-protocol: "v2"
```

### Backend SSL

**Annotations**:

| Annotation | Description |
|------------|-------------|
| `secure-backends` | Enable SSL to backend (`true`/`false`) |
| `backend-protocol` | Protocol: `h1`, `h2`, `h1-ssl`, `h2-ssl` |
| `secure-sni` | SNI value for backend connection |
| `secure-verify-hostname` | Hostname for certificate verification |
| `secure-verify-ca-secret` | CA secret for backend verification |
| `secure-crt-secret` | Client certificate for mTLS |
| `ssl-ciphers-backend` | Cipher list (TLS ≤ 1.2) for the backend connection |
| `ssl-cipher-suites-backend` | Cipher suites (TLS 1.3) for the backend connection |

```yaml
annotations:
  haproxy-ingress.github.io/secure-backends: "true"
  haproxy-ingress.github.io/backend-protocol: "h2"
  haproxy-ingress.github.io/secure-verify-ca-secret: "backend-ca"
  haproxy-ingress.github.io/secure-crt-secret: "client-cert"
  haproxy-ingress.github.io/ssl-ciphers-backend: "ECDHE-ECDSA-AES128-GCM-SHA256:ECDHE-RSA-AES128-GCM-SHA256"
```

The SSL keywords live on `default-server`, not on individual server lines, so pods can be added or removed over the runtime API without a HAProxy reload. `ssl-ciphers-backend` (HAProxy's `ciphers` server option) and `ssl-cipher-suites-backend` (`ciphersuites`) apply only when TLS to the backend is on — `secure-backends: "true"` or an `-ssl` `backend-protocol`. Without backend TLS they're ignored, because HAProxy rejects the keywords on a plaintext server line.

### `haproxy-ingress.github.io/initial-weight`

Initial weight for backend servers, from `0` to `256`.

```yaml
annotations:
  haproxy-ingress.github.io/initial-weight: "100"
```

### `haproxy-ingress.github.io/proxy-body-size`

Maximum allowed request body size. Requests exceeding the limit receive a 413 response.

**Valid values**: Plain number (bytes), or with `k`/`m`/`g` suffix (case-insensitive). Value `0` (the upstream default) means unlimited — no limit is emitted.

```yaml
annotations:
  haproxy-ingress.github.io/proxy-body-size: "10m"
```

Changing a body-size limit deploys without reloading HAProxy.

## Rate limiting

### `haproxy-ingress.github.io/limit-rps`

Reject a source IP's requests with HTTP 429 once it exceeds the configured rate.

**Related annotations**:

| Annotation | Description |
|------------|-------------|
| `limit-rps` | Maximum requests per second per source IP |
| `limit-rpm` | Maximum requests per minute per source IP |
| `limit-whitelist` | Comma-separated CIDRs exempt from the limit |

The cap is hard — jcmoraisjr/haproxy-ingress grants a burst allowance on top of the configured rate, so expect stricter enforcement at the same value after migrating. A route carries one request-rate counter, so when both are set `limit-rps` wins and HAPTIC records a `RateLimitCapIgnored` Event on the Ingress for the ignored `limit-rpm`. Invalid CIDRs in `limit-whitelist` fail the render.

```yaml
annotations:
  haproxy-ingress.github.io/limit-rps: "10"
  haproxy-ingress.github.io/limit-whitelist: "10.0.0.0/8"
```

Rate-limit settings and source-IP exemptions are stored in shared maps. Updating
an existing route's settings changes those maps without changing its backend.
Counters are keyed by route and client address, so each route has a separate
per-client budget. The `peers localinstance` section preserves counters across
HAProxy reloads.

## URL rewriting

### `haproxy-ingress.github.io/rewrite-target`

Rewrite the request path before forwarding to the backend. Capture groups written in the nginx-compatible `$1`–`$9` form are translated to HAProxy's `\1`–`\9` backreferences.

```yaml
annotations:
  haproxy-ingress.github.io/rewrite-target: "/$1"
```

## Raw configuration injection

The four `config-*` annotations inject operator-authored HAProxy directives verbatim into a configuration section. HAPTIC rejects them unless `controller.config.templatingSettings.extraContext.ingressPermissions.allowRawHAProxyConfig` is `true`; see [Raw configuration annotations](../operations/security.md#raw-configuration-annotations). HAPTIC validates the resulting config before deploying it, but the directives are yours — a typo fails the render.

### `haproxy-ingress.github.io/config-backend`

Raw HAProxy directives injected into each of the Ingress's backend sections.

```yaml
annotations:
  haproxy-ingress.github.io/config-backend: |
    http-send-name-header X-Backend-Server
    retries 5
```

### `haproxy-ingress.github.io/config-global`

Raw HAProxy directives injected into the `global` section. The section is process-wide: every Ingress carrying the annotation contributes its block (prefixed with an `# Ingress: <namespace>/<name>` comment), and deduplication across Ingresses is your responsibility.

```yaml
annotations:
  haproxy-ingress.github.io/config-global: |
    tune.bufsize 65536
```

### `haproxy-ingress.github.io/config-frontend`

Raw HAProxy directives injected into HAPTIC's shared HTTP frontend — not a per-Ingress frontend, so the directives apply to all HTTP traffic. They render before the routing logic, so captures, ACLs, and early `http-request` rules you inject are in scope for the routing that follows. Like `config-global`, every annotated Ingress contributes its block.

```yaml
annotations:
  haproxy-ingress.github.io/config-frontend: |
    capture request header X-Request-Id len 64
```

### `haproxy-ingress.github.io/config-defaults`

Raw HAProxy directives injected into the `defaults` section. Same process-wide semantics as `config-global`.

```yaml
annotations:
  haproxy-ingress.github.io/config-defaults: |
    option httplog
```

## Session affinity

Cookie-based session affinity — also called sticky sessions — pins a client to the same backend server across requests.

### `haproxy-ingress.github.io/affinity`

Enable cookie-based session affinity.

**Valid values**: `cookie`

**Related annotations**:

| Annotation | Description | Default |
|------------|-------------|---------|
| `session-cookie-name` | Cookie name | `INGRESSCOOKIE` |
| `session-cookie-strategy` | `insert`, `rewrite`, `prefix` | `insert` |
| `session-cookie-dynamic` | Use dynamic cookie key | `true` |
| `session-cookie-keywords` | Additional cookie options | - |
| `session-cookie-domain` | Cookie domain | - |
| `session-cookie-same-site` | `None`, `Lax`, `Strict` | - |
| `session-cookie-preserve` | Preserve backend cookies | - |

```yaml
annotations:
  haproxy-ingress.github.io/affinity: "cookie"
  haproxy-ingress.github.io/session-cookie-name: "SERVERID"
  haproxy-ingress.github.io/session-cookie-strategy: "insert"
  haproxy-ingress.github.io/session-cookie-same-site: "Lax"
```

## Access control

### `haproxy-ingress.github.io/allowlist-source-range`

Comma-separated list of CIDRs allowed to access this ingress.

```yaml
annotations:
  haproxy-ingress.github.io/allowlist-source-range: "10.0.0.0/8, 192.168.0.0/16"
```

### `haproxy-ingress.github.io/whitelist-source-range`

**Status**: Supported (deprecated alias)

Deprecated alias of `allowlist-source-range`, honoured only when `allowlist-source-range` is absent on the same Ingress. Prefer `allowlist-source-range` for new Ingresses.

### `haproxy-ingress.github.io/denylist-source-range`

Comma-separated list of CIDRs denied access to this ingress.

```yaml
annotations:
  haproxy-ingress.github.io/denylist-source-range: "203.0.113.0/24"
```

## Redirects

### `haproxy-ingress.github.io/ssl-redirect`

Redirect HTTP requests to HTTPS.

**Related annotations**:

| Annotation | Description | Default |
|------------|-------------|---------|
| `ssl-redirect` | Enable SSL redirect | - |
| `ssl-redirect-code` | HTTP status code | `302` |

```yaml
annotations:
  haproxy-ingress.github.io/ssl-redirect: "true"
  haproxy-ingress.github.io/ssl-redirect-code: "301"
```

### `haproxy-ingress.github.io/app-root`

Redirect requests to root path (`/`) to the specified path.

```yaml
annotations:
  haproxy-ingress.github.io/app-root: "/dashboard"
```

### `haproxy-ingress.github.io/redirect-to`

Redirect all requests to the specified URL.

**Related annotations**:

| Annotation | Description | Default |
|------------|-------------|---------|
| `redirect-to` | Target URL | - |
| `redirect-to-code` | HTTP status code | `302` |

```yaml
annotations:
  haproxy-ingress.github.io/redirect-to: "https://new.example.com"
  haproxy-ingress.github.io/redirect-to-code: "301"
```

### `haproxy-ingress.github.io/default-backend-redirect`

Redirect requests that match one of the Ingress's hosts but none of its paths, instead of letting them fall through to the default backend.

**Related annotations**:

| Annotation | Description | Default |
|------------|-------------|---------|
| `default-backend-redirect` | Target URL | - |
| `default-backend-redirect-code` | HTTP status code: 301, 302, 303, 307, or 308 (other values fail the render) | `302` |

```yaml
annotations:
  haproxy-ingress.github.io/default-backend-redirect: "https://landing.example.com"
  haproxy-ingress.github.io/default-backend-redirect-code: "301"
```

The host→URL pairs live in a per-code map, so changing the target URL is a map-only, reload-free update. The `!{ var(txn.backend_name) -m found }` guard fires exactly when the routing cascade matched no path — the same condition that otherwise selects the default backend.

## HSTS

### `haproxy-ingress.github.io/hsts`

Enable HTTP Strict Transport Security (HSTS) headers.

**Related annotations**:

| Annotation | Description | Default |
|------------|-------------|---------|
| `hsts` | Enable HSTS | - |
| `hsts-max-age` | Max-age in seconds | `15768000` |
| `hsts-include-subdomains` | Include subdomains | - |
| `hsts-preload` | Enable preload | - |

```yaml
annotations:
  haproxy-ingress.github.io/hsts: "true"
  haproxy-ingress.github.io/hsts-max-age: "31536000"
  haproxy-ingress.github.io/hsts-include-subdomains: "true"
  haproxy-ingress.github.io/hsts-preload: "true"
```

One shared rule for all hosts; each host's value (here `max-age=31536000; includeSubDomains; preload`) lives in `hsts.map` keyed by host, so adding or changing an HSTS value is a map-only, reload-free update.

## CORS

### `haproxy-ingress.github.io/cors-enable`

Enable Cross-Origin Resource Sharing (CORS) handling for the ingress. The headers come from per-route maps read by one frontend rule block, so adding or removing a CORS route is reload-free.

**Related annotations**:

| Annotation | Description | Default |
|------------|-------------|---------|
| `cors-enable` | Enable CORS | - |
| `cors-allow-origin` | Allowed origins — comma-separated list, single-level `*.` wildcards; matched Origin is echoed back | `*` |
| `cors-allow-methods` | Allowed methods | `GET, PUT, POST, DELETE, PATCH, OPTIONS` |
| `cors-allow-headers` | Allowed headers | Common headers |
| `cors-allow-credentials` | Allow credentials | - |
| `cors-expose-headers` | Exposed headers | - |
| `cors-max-age` | Preflight cache time | `86400` |

```yaml
annotations:
  haproxy-ingress.github.io/cors-enable: "true"
  haproxy-ingress.github.io/cors-allow-origin: "https://example.com"
  haproxy-ingress.github.io/cors-allow-credentials: "true"
```

## Headers

### `haproxy-ingress.github.io/forwardfor`

Overrides the [default `X-Forwarded-For` handling](../operations/security.md#forwarded-headers)
for the Ingress, with upstream's values:

| Value | What the backend receives |
|-------|---------------------------|
| `add` | Only the client address, even from a trusted proxy. An unknown value is `add` too. |
| `update` | The client's `X-Forwarded-For` with the client address appended. |
| `ifmissing` | The client's `X-Forwarded-For`, or the client address when there is none. |
| `ignore` | The client's `X-Forwarded-For` unchanged, or no header. |

Unlike upstream, HAPTIC applies it per Ingress and doesn't copy the client's
value to `X-Original-Forwarded-For`.

```yaml
annotations:
  haproxy-ingress.github.io/forwardfor: "add"
```

### `haproxy-ingress.github.io/headers`

Add request headers. Pipe-separated `name:value` pairs.

```yaml
annotations:
  haproxy-ingress.github.io/headers: "X-Custom-Header:value|X-Another:test"
```

## Server alias

### `haproxy-ingress.github.io/server-alias`

Comma-separated extra exact hostnames that route like the Ingress's first rule host. Each alias becomes a `host.map` entry pointing at the primary host's routing key, so every path already registered for that host applies to the alias — no backend or path duplication. Each alias must be a valid hostname, optionally starting with `*.`; case doesn't matter. A hostname an older Ingress already claims stays with that Ingress; see [Host aliases claim hostnames](../operations/security.md#host-aliases-claim-hostnames).

```yaml
annotations:
  haproxy-ingress.github.io/server-alias: "example.org, www.example.org"
```

### `haproxy-ingress.github.io/server-alias-regex`

A regular expression matching extra hostnames, routed to the Ingress's first rule host. The expression must match the whole hostname, and applies only when no rule host, wildcard host, or exact alias matches. It must be a HAProxy-compatible Perl Compatible Regular Expression (PCRE) whose groups and `[ ]` classes close within it.

```yaml
annotations:
  haproxy-ingress.github.io/server-alias-regex: "^www\\.example\\.(com|org)$"
```

## SSL features

### `haproxy-ingress.github.io/ssl-passthrough`

Enable TCP-level SSL passthrough (Layer 4) where HAProxy routes based on SNI without terminating SSL.

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: ssl-passthrough-example
  annotations:
    haproxy-ingress.github.io/ssl-passthrough: "true"
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
- Mixed passthrough and termination on different hosts is supported

## Authentication

### `haproxy-ingress.github.io/auth-secret`

Enable basic authentication using credentials from a Kubernetes Secret.

**Related annotations**:

| Annotation | Description | Default |
|------------|-------------|---------|
| `auth-secret` | Secret name (or `namespace/name`) | - |
| `auth-realm` | Authentication realm | `Restricted` |

```yaml
annotations:
  haproxy-ingress.github.io/auth-secret: "basic-auth"
  haproxy-ingress.github.io/auth-realm: "Protected Area"
```

Create the Secret in the Ingress namespace. Each key is a username; its value is
that user's password hash, without the `username:` prefix. With `htpasswd` installed:

```bash
umask 077
htpasswd -n -B admin | cut -d: -f2 > admin.hash
kubectl -n default create secret generic basic-auth --from-file=admin=admin.hash
rm admin.hash
```

HAPTIC refuses `$apr1$`, `$1$`, Data Encryption Standard (DES) crypt, and plaintext credentials by default;
see [accepted formats](../operations/security.md#basic-auth-password-hashes).

Use HTTPS for the protected route; HTTP Basic authentication doesn't encrypt credentials.

## External authentication

The `haproxy-ingress.github.io/auth-*` annotations use the bundled `external-auth` plugin. When set, each request hits an HTTP auth subrequest before reaching the backend; the auth service's status code decides whether HAProxy forwards the request, redirects to a sign-in URL, or returns 401.

### Prerequisites

The SPOA hub sidecar with the `external-auth` plugin must be enabled:

```yaml
spoaHub:
  plugins:
    external-auth:
      enabled: true
```

The hub auto-enables when any plugin is on, and the spoa-hub template library auto-loads when the hub is enabled. See the [SPOA Hub operations guide](../operations/spoa-hub.md) for the full deployment surface.

!!! warning "Not auto-enabled with this library"
    Unlike the nginx-ingress library, enabling the haproxy-ingress library doesn't auto-enable the `external-auth` plugin. Without the plugin, `auth-url` is silently not enforced — set `spoaHub.plugins.external-auth.enabled=true` explicitly.

Set an explicit `host` on every protected Ingress rule. HAPTIC rejects
protected rules without one.

### `haproxy-ingress.github.io/auth-url`

Auth service URL the SPOA hub calls per request. The plugin appends the original request path, sends a GET (overridable via `auth-method`), and gates the request based on the response status: 2xx allows, 3xx with `auth-signin` redirects, anything else returns 401.

```yaml
annotations:
  haproxy-ingress.github.io/auth-url: "https://auth.example.com/check"
```

The matching `auth-url.map` entry:

```
app.example.com/api https://auth.example.com/check
```

### `haproxy-ingress.github.io/auth-signin`

Browser-flow sign-in URL. When set, an auth failure produces a 302 redirect instead of a 401 — the standard pattern for OpenID Connect (OIDC) / Security Assertion Markup Language (SAML) flows where unauthenticated users go to a login page. The deny rule still emits, so routes without `auth-signin` keep the API-friendly 401.

```yaml
annotations:
  haproxy-ingress.github.io/auth-url: "https://auth.example.com/check"
  haproxy-ingress.github.io/auth-signin: "https://login.example.com/oauth2/start"
```

### `haproxy-ingress.github.io/auth-method`

HTTP method for the auth subrequest. Defaults to `GET` (or whatever the plugin's TOML config sets); set this to override per-route.

**Valid values**: `GET`, `HEAD`, `POST`, `PUT`, `PATCH`, `DELETE`, `OPTIONS`

```yaml
annotations:
  haproxy-ingress.github.io/auth-url: "https://auth.example.com/check"
  haproxy-ingress.github.io/auth-method: "POST"
```

`POST`, `PUT`, and `PATCH` auth requests have an empty body. Use header-based
authentication; the original request payload isn't forwarded.

### `haproxy-ingress.github.io/auth-headers-request`

Comma-separated list of request header names to forward to the auth service. The chart auto-extends the Stream Processing Offload Engine (SPOE) message body to capture every header listed across ingresses (deduped, the six standard headers — Authorization, Cookie, X-Forwarded-{For,Proto,Host,Uri} — are always captured), and the plugin then narrows the per-route forwarded set to exactly the headers the annotation lists.

```yaml
annotations:
  haproxy-ingress.github.io/auth-url: "https://auth.example.com/check"
  haproxy-ingress.github.io/auth-headers-request: "Authorization, X-Tenant-Id, X-Request-Id"
```

The corresponding entries in the SPOE message:

```
spoe-message check-auth
    args ... forward_headers=var(txn.auth_forward_headers) ... hdr_authorization=req.hdr(Authorization) hdr_x_tenant_id=req.hdr(X-Tenant-Id) hdr_x_request_id=req.hdr(X-Request-Id)
```

Header names are validated against the RFC 7230 token grammar; values containing whitespace, fetch syntax (`%[var(...)]`), or other non-tchar characters fail the Helm render. Names containing `_` aren't forwarded: SPOE argument names can't contain `-`, so `X_Api_Key` and `X-Api-Key` would share one capture. The admission webhook rejects them, and an Ingress that already has one gets an `InvalidAuthHeader` Warning Event; if no other name is listed, the Ingress forwards the default headers. Use the dashed name.

### `haproxy-ingress.github.io/auth-headers-succeed`

Comma-separated list of response header names from the auth service to forward to the upstream backend on auth success. Common pattern: the auth service returns `X-Auth-User: alice` on 200, this annotation makes that header available to the backend application.

```yaml
annotations:
  haproxy-ingress.github.io/auth-url: "https://auth.example.com/check"
  haproxy-ingress.github.io/auth-headers-succeed: "X-Auth-User, X-Auth-Roles"
```

One `set-header` directive per unique header across all ingresses; the per-route gating happens via the plugin's per-ingress `extract_headers` SPOE arg — routes that didn't list a header have its `txn` var unset, so the `var ... -m found` gate skips them.

### `haproxy-ingress.github.io/auth-headers-fail`

Comma-separated list of response header names from the auth service to forward to the *client* on auth failure. Drives, for example, `WWW-Authenticate` for Bearer challenges or `X-Error-Reason` for diagnostics on 401 / 5xx.

```yaml
annotations:
  haproxy-ingress.github.io/auth-url: "https://auth.example.com/check"
  haproxy-ingress.github.io/auth-headers-fail: "WWW-Authenticate, X-Error-Reason"
```

The conditions ensure the directive only fires on the deny response (auth path ran, not allowed, plugin actually extracted the header). The plugin v0.3.0+ extracts headers on every reply path (2xx, 3xx, 4xx, 5xx, fail-policy), so 401 and 5xx replies populate the `txn` vars too.

### `haproxy-ingress.github.io/oauth`

Use `oauth: "oauth2_proxy"` or `oauth: "oauth2-proxy"` with
[oauth2-proxy](https://oauth2-proxy.github.io/oauth2-proxy/). HAPTIC configures the
authentication URL, sign-in redirect, subrequest method, and identity headers.
The Ingress must route `oauth-uri-prefix` (default `/oauth2`) to the oauth2-proxy
Service; rendering fails if that path is missing.

**Related annotations**:

| Annotation | Description | Default |
|------------|-------------|---------|
| `oauth` | `oauth2_proxy` / `oauth2-proxy` (other values fail the render) | - |
| `oauth-uri-prefix` | The Ingress path routing to the oauth2-proxy Service | `/oauth2` |
| `oauth-headers` | Auth-reply headers forwarded to the backend on success | `X-Auth-Request-Email` |

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: oauth-protected
  annotations:
    haproxy-ingress.github.io/oauth: "oauth2_proxy"
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
                name: app-backend
                port:
                  number: 80
          - path: /oauth2
            pathType: Prefix
            backend:
              service:
                name: oauth2-proxy
                port:
                  number: 4180
```

**Effective external-auth configuration** (what the desugaring derives):

```text
auth-url     http://oauth2-proxy.<namespace>.svc:4180/oauth2/auth
auth-signin  /oauth2/start?rd=%[path]
auth-method  HEAD
forwarded success headers: X-Auth-Request-Email
```

An explicit `auth-url` on the same Ingress takes precedence and disables the oauth desugaring entirely; an explicit `auth-signin`, `auth-method`, or `auth-headers-succeed` overrides only its derived value.

!!! warning "Plaintext auth URL"
    The derived auth URL is plain `http://` toward the in-cluster oauth2-proxy Service, so the external-auth plugin must allow plaintext: set `allow_plaintext = true` in `spoaHub.plugins.external-auth.params`.

### Combined example

A protected API route with browser sign-in, custom request header forwarding, identity propagation to the backend, and a Bearer challenge on failure.

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: protected-api
  annotations:
    haproxy-ingress.github.io/auth-url: "https://auth.example.com/check"
    haproxy-ingress.github.io/auth-signin: "https://login.example.com/oauth2/start"
    haproxy-ingress.github.io/auth-method: "GET"
    haproxy-ingress.github.io/auth-headers-request: "Authorization, X-Tenant-Id"
    haproxy-ingress.github.io/auth-headers-succeed: "X-Auth-User, X-Auth-Roles"
    haproxy-ingress.github.io/auth-headers-fail: "WWW-Authenticate"
spec:
  ingressClassName: haptic
  rules:
    - host: api.example.com
      http:
        paths:
          - path: /v1
            pathType: Prefix
            backend:
              service:
                name: api-backend
                port:
                  number: 80
```

## Client certificate auth (mTLS)

Use `auth-tls-*` annotations to require client certificates signed by a trusted
CA. The Ingress also needs an HTTPS certificate for its hosts.

### `haproxy-ingress.github.io/auth-tls-secret`

Reference to a Secret whose `ca.crt` field contains the CA bundle that signs the clients' certificates. The chart writes the CA to `ssl/<ns>-<secret>-client-ca.pem` and adds `[ca-file <path> verify <mode>]` to the crt-list line for every host on the annotated Ingress.

**Format**: `name` (resolves in the Ingress namespace) or `namespace/name`.

```yaml
annotations:
  haproxy-ingress.github.io/auth-tls-secret: "client-ca"
```

Create the Secret from your client CA bundle in the Ingress's namespace:

```bash
kubectl -n default create secret generic client-ca --from-file=ca.crt=ca.crt
```

Set an explicit `host` on every protected Ingress rule. HAPTIC rejects
protected rules without one.

### `haproxy-ingress.github.io/auth-tls-verify-client`

Client certificate verification mode.

**Valid values**:

| Value | HAProxy verify mode | Behaviour |
|-------|---------------------|-----------|
| `on` (default) | `required` | Reject connections without a valid client cert |
| `off` | (no-op) | Don't enable verification on this host — the entry is skipped, falling through to the default crt-list line |
| `optional` | `optional` | Verify when a cert is presented; allow connections without |
| `optional_no_ca` | `optional` | Same as `optional`; a certificate from an unknown CA still fails verification. Add the issuing CA to the trusted bundle to accept it |

Other values fail the render.

```yaml
annotations:
  haproxy-ingress.github.io/auth-tls-secret: "client-ca"
  haproxy-ingress.github.io/auth-tls-verify-client: "optional"
```

`auth-tls-strict` has no separate effect. To allow clients without a
certificate, use `auth-tls-verify-client: optional`; invalid certificates
are still rejected.

### `haproxy-ingress.github.io/auth-tls-error-page`

URL to redirect to (302) when client certificate verification fails.

```yaml
annotations:
  haproxy-ingress.github.io/auth-tls-secret: "client-ca"
  haproxy-ingress.github.io/auth-tls-error-page: "https://example.com/cert-required"
```

The redirect can only run after a successful TLS handshake. With the default
`verify required`, a missing or invalid certificate aborts the handshake: the
client sees a TLS error, not this page. `optional` still rejects an invalid
certificate; it only permits clients that send no certificate.

### `haproxy-ingress.github.io/auth-tls-cert-header`

When `"true"`, forwards the verified client certificate (base64-encoded DER), subject CN, and full subject DN to the upstream backend as HTTP headers.

```yaml
annotations:
  haproxy-ingress.github.io/auth-tls-secret: "client-ca"
  haproxy-ingress.github.io/auth-tls-cert-header: "true"
```

The `ssl_fc_has_crt` gate ensures the headers only flow when a cert was actually presented (relevant when `auth-tls-verify-client: optional` is in effect — connections without a cert get no headers rather than empty ones).

## Web application firewall (ModSecurity / Coraza)

Set `haproxy-ingress.github.io/waf: "modsecurity"` to inspect requests with the
Coraza Web Application Firewall (WAF). This is the only supported `waf` value.
Choose `waf-mode: deny` (the default) to block matches or `waf-mode: detect` to
record them. Other modes, or `waf-mode` without `waf`, fail validation.

This annotation library enables Coraza automatically. See the
[plugin settings](../operations/spoa-hub.md) to configure it.

## Unsupported annotations

The library reads only the annotations documented on this page; it ignores any other `haproxy-ingress.github.io/*` key. Two annotations are accepted for compatibility but have no effect:

| Annotation | Reason |
|------------|--------|
| `auth-tls-strict` | Upstream defaults it to true (fail-closed on a missing/invalid client CA); here a missing CA skips mTLS for the Ingress (fail-open), and this annotation can't restore fail-closed. For soft verification use `auth-tls-verify-client: optional` instead. |
| `docs` | A pointer to jcmoraisjr/haproxy-ingress documentation, not a configuration key. |

Before cutting over, paste your manifests into the [playground](/playground/) migration report to get a per-annotation verdict for exactly the annotations you use — see [Check what changes](../migrating.md#step-0-check-what-changes).

## Watched resources

This library watches the following additional resources:

- **Secrets** (`v1/secrets`) — read for basic-auth credentials (`auth-secret`), backend TLS material (`secure-verify-ca-secret`, `secure-crt-secret`), and incoming client-CA bundles (`auth-tls-secret`)

<a id="annotation-inventory"></a>

See the [annotation compatibility table](../annotation-compatibility.md#haproxy-ingress)
for migration differences.

## Access-log fields

The library contributes `mtls_verify` and `mtls_cn` to the
[structured access log](../operations/access-logging.md) when any Ingress
sets `auth-tls-secret` or `auth-tls-cert-header`: the certificate verification
result (0 on success, otherwise an X509 error code) and the client's CN. The
other annotation libraries contribute identical fields, so several can be enabled
at once.

<a id="extension-points"></a>
<a id="features-shared-state-initialization"></a>
<a id="map-path-path-map-extension-points"></a>
<a id="backend-directives-per-backend-directives"></a>
<a id="frontend-filters-http-frontend-requestresponse-filters"></a>
<a id="other-extension-points"></a>

For custom behavior, use the [base extension points](base.md#extension-points)
and [write a template snippet](../templating.md).

## See also

- [Template Libraries Overview](../template-libraries.md) - How template libraries work
- [Base Library](base.md) - Path matching infrastructure
- [Ingress Library](ingress.md) - Standard Ingress support
- [HAProxyTech Library](haproxytech.md) - `haproxy.org/*` annotations
- [HAProxy Ingress Documentation](https://haproxy-ingress.github.io/docs/configuration/keys/) - Original annotation reference
