# Annotation compatibility

Compare the annotations HAPTIC accepts from other ingress controllers.
Enable only the compatibility libraries you need and check each library's limits
before moving traffic. The [migration guide](migrating.md) covers the cutover;
for new configuration, use [native annotations](libraries/haptic-annotations.md).

An annotation that fails rendering causes its resource change to be rejected.
Independent valid updates can continue; see
[Rejected resource changes](operations/input-isolation.md).

## Supported features

Compare the vendor libraries below. For native annotations and additional
capabilities, see the [HAPTIC annotation reference](./libraries/haptic-annotations.md).

| Feature | `haproxy.org/` | `haproxy-ingress.github.io/` | `nginx.ingress.kubernetes.io/` |
|---------|----------------|-------------------------------|--------------------------------|
| Basic authentication | `auth-type`, `auth-secret`, `auth-realm` | `auth-secret`, `auth-realm` | `auth-type`, `auth-secret`, `auth-secret-type`, `auth-realm`, `satisfy` |
| External authentication ([Stream Processing Offload Agent (SPOA) hub](operations/spoa-hub.md)) | — | `auth-url`, `auth-signin`, `auth-method`, `auth-headers-request`, `auth-headers-succeed`, `auth-headers-fail` | `auth-url`, `auth-signin`, `auth-method`, `auth-response-headers` |
| OAuth2 proxy | — | `oauth`, `oauth-uri-prefix`, `oauth-headers` | — |
| Client certificate (incoming mTLS) | — | `auth-tls-secret`, `auth-tls-verify-client`, `auth-tls-match-subject`, `auth-tls-error-page`, `auth-tls-cert-header`, `auth-tls-cn-header` | `auth-tls-secret`, `auth-tls-verify-client`, `auth-tls-match-cn`, `auth-tls-error-page`, `auth-tls-pass-certificate-to-upstream` |
| Allowlist / Denylist | `allow-list`, `deny-list` | `allowlist-source-range`, `denylist-source-range` | `whitelist-source-range`, `denylist-source-range` |
| SSL redirect | `ssl-redirect`, `ssl-redirect-code` | `ssl-redirect`, `ssl-redirect-code` | `ssl-redirect`, `force-ssl-redirect` |
| URL redirects | `request-redirect`, `request-redirect-code` | `redirect-to`, `redirect-to-code`, `app-root`, `default-backend-redirect`, … | `permanent-redirect`, `temporal-redirect`, `from-to-www-redirect`, `app-root`, … |
| SSL passthrough | `ssl-passthrough` | `ssl-passthrough` | `ssl-passthrough` |
| Backend SSL / mTLS | `server-ssl`, `server-proto`, `server-ca`, `server-crt` | `secure-backends`, `backend-protocol`, `secure-sni`, `secure-verify-ca-secret`, `secure-crt-secret`, `ssl-ciphers-backend`, … | `backend-protocol`, `proxy-ssl-secret`, `proxy-ssl-verify`, `proxy-ssl-name`, … |
| Cross-Origin Resource Sharing (CORS) | `cors-enable`, `cors-allow-origin`, … | `cors-enable`, `cors-allow-origin`, … | `enable-cors`, `cors-allow-origin`, … |
| Load balancing | `load-balance` | `balance-algorithm` | `load-balance`, `upstream-hash-by` |
| Session affinity / sticky sessions (cookies) | `cookie-persistence` | `affinity`, `session-cookie-*` | `affinity`, `affinity-mode`, `session-cookie-*` |
| Rate limiting | `rate-limit-requests`, `rate-limit-period`, … | `limit-rps`, `limit-rpm`, `limit-whitelist` | `limit-rps`, `limit-rpm`, `limit-connections`, `limit-whitelist`, `limit-burst-multiplier` |
| Bandwidth throttling | — | — | `limit-rate`, `limit-rate-after` |
| Request body size limit | — | `proxy-body-size` | `proxy-body-size` |
| Request buffering | — | — | `proxy-request-buffering`, `proxy-buffer-size` |
| Timeouts | `timeout-server`, `timeout-connect`, … | `timeout-server`, `timeout-connect`, … | `proxy-connect-timeout`, `proxy-read-timeout`, `proxy-send-timeout` |
| Retries | — | — | `proxy-next-upstream`, `proxy-next-upstream-tries` |
| Health checks | `check`, `check-http`, `check-interval` | `backend-check-interval`, `health-check-uri`, … | — |
| Agent checks | — | `agent-check-port`, `agent-check-addr`, … | — |
| HTTP Strict Transport Security (HSTS) | — | `hsts`, `hsts-max-age`, … | `hsts`, `hsts-max-age`, … |
| Request / response headers | `request-set-header`, `response-set-header` | `headers`, `forwardfor` | `custom-request-headers`, `custom-response-headers` |
| Path rewriting | `path-rewrite` | `rewrite-target` | `rewrite-target` |
| Server aliases | — | `server-alias`, `server-alias-regex` | `server-alias` |
| Per-host default backend | — | — | `default-backend` |
| Canary deployments | — | — | `canary`, `canary-by-header`, `canary-weight`, … |
| Request mirroring | — | — | `mirror-target` |
| Web Application Firewall (WAF) / ModSecurity | — | `waf`, `waf-mode` | `modsecurity-snippet`, `enable-modsecurity` |
| PROXY protocol | `send-proxy-protocol` | `proxy-protocol` | `use-proxy-protocol` |
| Raw backend config | `backend-config-snippet` | `config-backend` | `configuration-snippet` |
| Raw global / frontend / defaults config | — | `config-global`, `config-frontend`, `config-defaults` | — |

HAPTIC rejects the raw config annotations unless
`controller.config.templatingSettings.extraContext.ingressPermissions.allowRawHAProxyConfig`
is `true`; see [Raw configuration annotations](operations/security.md#raw-configuration-annotations).

For the complete per-annotation reference with examples and generated HAProxy configuration output, see the library docs:

- [haproxytech library →](./libraries/haproxytech.md)
- [haproxy-ingress library →](./libraries/haproxy-ingress.md)
- [nginx-ingress library →](./libraries/nginx-ingress.md)

You can mix prefixes on one Ingress, but configure each feature through one
annotation family. Configuring the same feature through two enabled families
causes admission rejection and a warning during live rendering.

An annotation under an enabled library's prefix that the library doesn't
list has no effect. HAPTIC records an `UnknownAnnotation` Warning Event on the
Ingress naming it, so a typo doesn't go unnoticed.

See [Template Libraries](./template-libraries.md) for how to enable or disable individual libraries.

See the nginx-ingress compatibility verdict render live:

<div class="pg-embed" markdown data-scenario="nginx-ingress" data-facade="resources" data-input="resources" data-input-focus="nginx.ingress.kubernetes.io/proxy-connect-timeout" data-tab="migration" data-controls="tabs,resources" data-title="nginx-ingress annotation migration report" data-height="440">

<p class="pg-task" markdown>In **Resources**, remove `nginx.ingress.kubernetes.io/load-balance` from the `shop` Ingress. Its verdict disappears from the **migration** report.</p>

</div>

## Differences from ingress-nginx {#ingress-nginx}

<!-- BEGIN generated: migration-coverage ingress-nginx -->
The library classifies 141 `nginx.ingress.kubernetes.io/*` annotations: 56 supported, 37 with behaviour differences, 48 not carried over, 0 failing.

| Annotation | Status | What to check |
|------------|--------|---------------|
| `nginx.ingress.kubernetes.io/affinity-canary-behavior` | Not carried over | The canary is picked per request and session affinity doesn't pin a client to it, which is ingress-nginx's "legacy" behaviour; "sticky" isn't available. |
| `nginx.ingress.kubernetes.io/affinity-mode` | Behaviour differs | HAProxy's cookie stickiness is always "persistent"; with "balanced" (the ingress-nginx default) existing sessions also stay on their pod when the Service scales up. Other values are rejected at admission and warned about otherwise. |
| `nginx.ingress.kubernetes.io/auth-always-set-cookie` | Not carried over | Cookies set by the auth service aren't passed to the client. |
| `nginx.ingress.kubernetes.io/auth-cache-duration` | Not carried over | Auth responses aren't cached; every request is checked against `auth-url`. |
| `nginx.ingress.kubernetes.io/auth-cache-key` | Not carried over | Auth responses aren't cached; every request is checked against `auth-url`. |
| `nginx.ingress.kubernetes.io/auth-keepalive` | Not carried over | Connection reuse toward the auth service is managed by the external-auth plugin and isn't configurable per Ingress. |
| `nginx.ingress.kubernetes.io/auth-keepalive-requests` | Not carried over | Connection reuse toward the auth service is managed by the external-auth plugin and isn't configurable per Ingress. |
| `nginx.ingress.kubernetes.io/auth-keepalive-share-vars` | Not carried over | nginx variables don't exist in HAProxy; the auth subrequest doesn't share request state with the backend request. |
| `nginx.ingress.kubernetes.io/auth-keepalive-timeout` | Not carried over | Connection reuse toward the auth service is managed by the external-auth plugin and isn't configurable per Ingress. |
| `nginx.ingress.kubernetes.io/auth-method` | Behaviour differs | Overrides the auth subrequest method; POST/PUT/PATCH are sent with an empty body. |
| `nginx.ingress.kubernetes.io/auth-proxy-set-headers` | Not carried over | The auth subrequest forwards the client's request headers; extra headers from a ConfigMap can't be added. |
| `nginx.ingress.kubernetes.io/auth-request-redirect` | Not carried over | No X-Auth-Request-Redirect header is sent to the auth service. |
| `nginx.ingress.kubernetes.io/auth-secret` | Behaviour differs | By default only bcrypt, SHA-256/SHA-512 crypt and yescrypt hashes are accepted. An htpasswd Secret with `$apr1$`, `{SHA}`, `$1$` or plaintext entries works under ingress-nginx but is refused here; regenerate those hashes, for example with `htpasswd -nB <user>`. |
| `nginx.ingress.kubernetes.io/auth-signin` | Behaviour differs | a trailing `$escaped_request_uri` becomes the URL-encoded request URI; other nginx variables are used verbatim. |
| `nginx.ingress.kubernetes.io/auth-signin-redirect-param` | Not carried over | auth-signin is used verbatim; no parameter carrying the original URL is appended. |
| `nginx.ingress.kubernetes.io/auth-snippet` | Not carried over | Freeform nginx configuration can't be translated to HAProxy; the haproxy-ingress library's auth-headers-request annotation covers the common use case. |
| `nginx.ingress.kubernetes.io/auth-tls-error-page` | Behaviour differs | 302 redirect on client-certificate verification failure, applied reload-free via a map — but it only fires when auth-tls-verify-client is optional/optional_no_ca. Under the default "on" (HAProxy verify required) an invalid/missing client cert aborts the TLS handshake, so the request never reaches the redirect and the client sees a TLS error instead of the page. |
| `nginx.ingress.kubernetes.io/auth-tls-pass-certificate-to-upstream` | Behaviour differs | Forwards ssl-client-cert (base64 DER — ingress-nginx sends URL-encoded PEM) and ssl-client-subject-dn; ssl-client-verify and ssl-client-issuer-dn aren't set. |
| `nginx.ingress.kubernetes.io/auth-tls-secret` | Behaviour differs | Client-CA verification is keyed by SNI — every rule needs an explicit host or the render fails; a missing Secret (or missing ca.crt) skips mTLS for the Ingress with a rendered warning. |
| `nginx.ingress.kubernetes.io/auth-tls-verify-client` | Behaviour differs | "on"→required; "optional" and "optional_no_ca"→optional (HAProxy has no verify-but-accept-invalid mode); other values fail the render. |
| `nginx.ingress.kubernetes.io/auth-tls-verify-depth` | Not carried over | HAProxy has no per-server/per-crt-list chain-depth option; a warning comment is rendered and the CA bundle scope bounds the accepted chain instead. |
| `nginx.ingress.kubernetes.io/auth-type` | Behaviour differs | Only "basic" is supported; "digest" fails the render. |
| `nginx.ingress.kubernetes.io/backend-protocol` | Behaviour differs | HTTP, HTTPS, `GRPC` and `GRPCS` map to HAProxy server options; `AJP` and `FCGI` have no HAProxy equivalent and fail the render with an error. |
| `nginx.ingress.kubernetes.io/canary-weight-total` | Not carried over | The weight base is fixed at 100. |
| `nginx.ingress.kubernetes.io/client-body-buffer-size` | Not carried over | HAProxy holds request bodies in tune.bufsize-sized memory buffers and never spools them to disk; see proxy-request-buffering. |
| `nginx.ingress.kubernetes.io/configuration-snippet` | Behaviour differs | Injected verbatim into the backend section — the value must contain HAProxy directives, not nginx configuration; existing nginx snippets need rewriting. Rejected unless `extraContext.ingressPermissions.allowRawHAProxyConfig` is `true`. |
| `nginx.ingress.kubernetes.io/cors-allow-credentials` | Behaviour differs | The header is only sent when explicitly "true" — ingress-nginx defaults it to true. |
| `nginx.ingress.kubernetes.io/custom-headers` | Not carried over | Response headers from a ConfigMap aren't read; set them with custom-response-headers. |
| `nginx.ingress.kubernetes.io/custom-http-errors` | Behaviour differs | Upstream responses with a listed status are replaced by a snapshot of the default backend's page — JSON when the client's first Accept entry is application/json, HTML otherwise. Per-request headers (X-Original-URI, X-Request-ID, X-Service-Name, …) can't influence the page. A changed page is served after the next refresh (extraContext.nginxCustomHTTPErrorsRefreshInterval, default 5m) plus a render and rollout, with one hitless reload. HAProxy's own error responses keep the chart's error pages. |
| `nginx.ingress.kubernetes.io/denylist-source-range` | Behaviour differs | Host-scoped — the denylist only gates rules with an explicit host, so an Ingress without rule hosts gets no filtering; invalid CIDRs fail the render. |
| `nginx.ingress.kubernetes.io/enable-access-log` | Not carried over | Access logging can't be switched off per Ingress; extraContext.accessLog.suppress.successful drops successful requests fleet-wide. |
| `nginx.ingress.kubernetes.io/enable-global-auth` | Not carried over | HAPTIC has no global `auth-url`, so there is nothing to opt out of. |
| `nginx.ingress.kubernetes.io/enable-modsecurity` | Behaviour differs | "false" opts the route out of the WAF; "true" sends the route to the Coraza WAF in detect mode, like the detection-only ingress-nginx default (under `default-on` dispatch every route is already inspected); other values fail the render. |
| `nginx.ingress.kubernetes.io/enable-opentelemetry` | Not carried over | This annotation requires the nginx OpenTelemetry module. Configure HAPTIC tracing through extraContext.tracing instead. |
| `nginx.ingress.kubernetes.io/enable-opentracing` | Not carried over | This annotation requires the nginx OpenTracing module. Configure HAPTIC tracing through extraContext.tracing instead. |
| `nginx.ingress.kubernetes.io/enable-owasp-core-rules` | Not carried over | The Coraza WAF runs the Open Worldwide Application Security Project (OWASP) Core Rule Set v4 on every route it inspects; set `enable-modsecurity` to `false` to opt a route out. |
| `nginx.ingress.kubernetes.io/enable-rewrite-log` | Not carried over | nginx rewrite debugging has no HAProxy equivalent. |
| `nginx.ingress.kubernetes.io/hsts` | Behaviour differs | By default the header is emitted only when the Ingress sets `hsts` to `"true"`, whereas ingress-nginx enables HSTS globally by default. Set extraContext.tls.hsts.enabled=true to send HSTS on all TLS hosts (matching ingress-nginx); a per-Ingress `hsts` annotation still overrides the value for its hosts. |
| `nginx.ingress.kubernetes.io/hsts-include-subdomains` | Behaviour differs | includeSubDomains is added only when explicitly "true" — ingress-nginx defaults it to true. |
| `nginx.ingress.kubernetes.io/http2-push-preload` | Not carried over | HAProxy doesn't implement HTTP/2 server push, which browsers have also removed. |
| `nginx.ingress.kubernetes.io/limit-burst-multiplier` | Behaviour differs | The limit-rps/limit-rpm window becomes multiplier times longer at the same average rate (limit-rps 10 with multiplier 5 allows 50 requests per 5 s), so bursts pass without queueing; it has no effect on limit-connections, and values outside 1-100 fail the render. Without it the limit stays a hard cap rather than ingress-nginx's default 5x burst. |
| `nginx.ingress.kubernetes.io/limit-connections` | Behaviour differs | Rejects with 429, and ignored when limit-rps or limit-rpm is set (one stick-table per backend). |
| `nginx.ingress.kubernetes.io/limit-rate` | Behaviour differs | Download throttle via an outbound bandwidth-limit filter, but applied per stream — an HTTP/2 client gets the limit once per stream, not once per connection. |
| `nginx.ingress.kubernetes.io/limit-rate-after` | Behaviour differs | Mapped to the filter's `min-size`, the smallest chunk forwarded at a time — not the start-throttling-after-N-bytes offset nginx applies, which HAProxy can't express. A large value adds latency rather than delaying the throttle. |
| `nginx.ingress.kubernetes.io/limit-rpm` | Behaviour differs | Same hard-cap/429 semantics as limit-rps, and ignored when limit-rps is also set (HAProxy stores one request-rate counter per backend). |
| `nginx.ingress.kubernetes.io/limit-rps` | Behaviour differs | Hard per-source-IP cap rejecting with 429 — ingress-nginx allows a 5x burst and rejects with 503. |
| `nginx.ingress.kubernetes.io/mirror-host` | Not carried over | The mirror plugin forces the mirrored Host header to the target authority. |
| `nginx.ingress.kubernetes.io/mirror-request-body` | Not carried over | The buffered request body is always forwarded to the mirror target. |
| `nginx.ingress.kubernetes.io/mirror-target` | Behaviour differs | Mirrors via the SPOA hub mirror plugin — requires spoaHub.plugins.mirror and a rule host (the render fails otherwise); only the URL's authority is used, the live request path/query is re-attached. |
| `nginx.ingress.kubernetes.io/modsecurity-transaction-id` | Not carried over | Coraza assigns its own transaction ID; the nginx variable expression can't be evaluated. |
| `nginx.ingress.kubernetes.io/opentelemetry-operation-name` | Not carried over | This annotation requires the nginx OpenTelemetry module. Configure HAPTIC tracing through extraContext.tracing instead. |
| `nginx.ingress.kubernetes.io/opentelemetry-trust-incoming-span` | Not carried over | This annotation requires the nginx OpenTelemetry module. Configure HAPTIC tracing through extraContext.tracing instead. |
| `nginx.ingress.kubernetes.io/opentracing-trust-incoming-span` | Not carried over | This annotation requires the nginx OpenTracing module. Configure HAPTIC tracing through extraContext.tracing instead. |
| `nginx.ingress.kubernetes.io/preserve-trailing-slash` | Not carried over | The HTTPS redirect always keeps the request path unchanged, trailing slash included, which is ingress-nginx's "true" behaviour. |
| `nginx.ingress.kubernetes.io/proxy-buffer-size` | Behaviour differs | HAProxy has no per-route response buffer; response headers must fit the fleet-wide tune.bufsize minus 1 KiB (15 KiB by default). A larger value records a ProxyBufferSizeExceeded Event naming the extraContext.tune.bufsize that fits. |
| `nginx.ingress.kubernetes.io/proxy-buffering` | Not carried over | HAProxy streams responses through fixed-size buffers; there is no response spooling to switch on or off. |
| `nginx.ingress.kubernetes.io/proxy-buffers-number` | Not carried over | HAProxy streams responses through fixed-size buffers; there is no response spooling to switch on or off. |
| `nginx.ingress.kubernetes.io/proxy-busy-buffers-size` | Not carried over | HAProxy streams responses through fixed-size buffers; there is no response spooling to switch on or off. |
| `nginx.ingress.kubernetes.io/proxy-cookie-domain` | Behaviour differs | Only the "<from> <to>" rewrite form is supported; any other value (including "off") fails the render. |
| `nginx.ingress.kubernetes.io/proxy-cookie-path` | Behaviour differs | Only the "<from> <to>" rewrite form is supported; any other value (including "off") fails the render. |
| `nginx.ingress.kubernetes.io/proxy-http-version` | Not carried over | HAProxy always speaks HTTP/1.1 to the backend (HTTP/2 for `backend-protocol` `GRPC`), which is the ingress-nginx default; HTTP/1.0 isn't available. |
| `nginx.ingress.kubernetes.io/proxy-max-temp-file-size` | Not carried over | HAProxy buffers in memory; there is no temp-file spooling. |
| `nginx.ingress.kubernetes.io/proxy-next-upstream` | Behaviour differs | Maps to HAProxy retry-on (error→conn-failure, timeout→response-timeout, invalid_header→junk-response, `http_NNN`→`NNN`); `non_idempotent` is ignored per route — non-idempotent methods are excluded from L7 retries globally (matching nginx's own default), liftable via extraContext.retryNonIdempotent; "off" emits retries 0. |
| `nginx.ingress.kubernetes.io/proxy-next-upstream-timeout` | Not carried over | HAProxy bounds each retry by the connect and server timeouts; there is no overall retry deadline. |
| `nginx.ingress.kubernetes.io/proxy-read-timeout` | Behaviour differs | Collapses with proxy-send-timeout into HAProxy's single timeout server — the larger value wins, asymmetric read/send timeouts are lost. |
| `nginx.ingress.kubernetes.io/proxy-redirect-from` | Behaviour differs | "default" isn't supported (warning comment, no rewrite); requires proxy-redirect-to; values must be space-free. |
| `nginx.ingress.kubernetes.io/proxy-request-buffering` | Behaviour differs | on/off overrides extraContext.requestBuffering.enabled for the route via a reload-free map, but HAProxy buffers only requests with a Content-Length and at most tune.bufsize, so "on" guards against slow clients rather than spooling a whole upload; other values are rejected at admission and warned about otherwise. |
| `nginx.ingress.kubernetes.io/proxy-send-timeout` | Behaviour differs | Collapses with proxy-read-timeout into HAProxy's single timeout server — the larger value wins, asymmetric read/send timeouts are lost. |
| `nginx.ingress.kubernetes.io/proxy-ssl-server-name` | Not carried over | Not read; SNI toward the upstream is controlled via proxy-ssl-name instead. |
| `nginx.ingress.kubernetes.io/proxy-ssl-verify-depth` | Not carried over | HAProxy has no per-server chain-depth option; a warning comment is rendered. |
| `nginx.ingress.kubernetes.io/satisfy` | Behaviour differs | "any" OR-combines whitelist-source-range with basic auth only; unlike ingress-nginx it doesn't extend to external auth (`auth-url`). |
| `nginx.ingress.kubernetes.io/server-snippet` | Not carried over | nginx server-level directives have no HAProxy equivalent. |
| `nginx.ingress.kubernetes.io/service-upstream` | Not carried over | Backends always list the Service's ready endpoints and update them without a reload, so routing through the ClusterIP isn't needed. |
| `nginx.ingress.kubernetes.io/session-cookie-change-on-failure` | Not carried over | When the pinned pod is gone HAProxy always sends the request to another pod and re-pins the cookie, which is ingress-nginx's "true" behaviour. |
| `nginx.ingress.kubernetes.io/session-cookie-conditional-samesite-none` | Not carried over | HAProxy can't vary cookie attributes by User-Agent; SameSite=None is sent to every browser. |
| `nginx.ingress.kubernetes.io/session-cookie-expires` | Behaviour differs | Emitted as a Max-Age attribute — HAProxy can't compute an absolute Expires date; browsers treat both equivalently. |
| `nginx.ingress.kubernetes.io/session-cookie-hash` | Not carried over | HAProxy's dynamic-cookie hashing isn't selectable; the value is ignored and a warning comment is rendered. |
| `nginx.ingress.kubernetes.io/ssl-ciphers` | Not carried over | Ciphers apply to every TLS host; set them with extraContext.tls.ciphers. |
| `nginx.ingress.kubernetes.io/ssl-prefer-server-ciphers` | Not carried over | HAProxy always prefers the server's cipher order, which is ingress-nginx's "true" behaviour. |
| `nginx.ingress.kubernetes.io/ssl-redirect` | Behaviour differs | Redirects only when explicitly "true" — ingress-nginx redirects TLS-enabled Ingresses by default; the code comes from extraContext.nginxHttpRedirectCode (default 308, matching ingress-nginx's http-redirect-code). |
| `nginx.ingress.kubernetes.io/stream-snippet` | Not carried over | nginx stream directives have no HAProxy equivalent. |
| `nginx.ingress.kubernetes.io/upstream-hash-by-subset` | Not carried over | HAProxy's consistent hashing maps a key to one server; subset hashing isn't available. |
| `nginx.ingress.kubernetes.io/upstream-hash-by-subset-size` | Not carried over | HAProxy's consistent hashing maps a key to one server; subset hashing isn't available. |
| `nginx.ingress.kubernetes.io/use-regex` | Behaviour differs | A path with regex syntax matches as a case-insensitive regex anchored at the path start; a literal path keeps its Kubernetes Prefix/Exact match, other Ingresses on the host are unaffected, and regex paths are tried before prefix paths instead of longest first. |
| `nginx.ingress.kubernetes.io/whitelist-source-range` | Behaviour differs | Host-scoped — the allowlist only gates rules with an explicit host, so an Ingress without rule hosts gets no filtering; invalid CIDRs fail the render. |
<!-- END generated: migration-coverage ingress-nginx -->

## Differences from haproxy-ingress {#haproxy-ingress}

<!-- BEGIN generated: migration-coverage haproxy-ingress -->
The library classifies 92 `haproxy-ingress.github.io/*` annotations: 58 supported, 32 with behaviour differences, 2 not carried over, 0 failing.

| Annotation | Status | What to check |
|------------|--------|---------------|
| `haproxy-ingress.github.io/agent-check-addr` | Behaviour differs | Has no effect without agent-check-port — and setting it (or -interval/-send) without agent-check-port fails the render. |
| `haproxy-ingress.github.io/agent-check-interval` | Behaviour differs | Has no effect without agent-check-port — and setting it without agent-check-port fails the render. |
| `haproxy-ingress.github.io/agent-check-send` | Behaviour differs | Has no effect without agent-check-port — and setting it without agent-check-port fails the render. |
| `haproxy-ingress.github.io/allowlist-source-range` | Behaviour differs | Host-scoped — only gates rules with an explicit host; invalid CIDRs fail the render. |
| `haproxy-ingress.github.io/auth-method` | Behaviour differs | Overrides the auth subrequest method; POST/PUT/PATCH are sent with an empty body. |
| `haproxy-ingress.github.io/auth-tls-cert-header` | Behaviour differs | Forwards X-SSL-Client-CN, X-SSL-Client-DN and X-SSL-Client-Cert when "true"; jcmoraisjr/haproxy-ingress additionally forwards the SHA-1 and serial headers, which aren't set. |
| `haproxy-ingress.github.io/auth-tls-secret` | Behaviour differs | Verification is keyed by SNI — every rule needs an explicit host or the render fails. A missing Secret (or missing ca.crt) currently **skips** mTLS for the Ingress (fail-open — requests with no/any client cert pass), where upstream with auth-tls-strict defaulting to true installs a fake CA and rejects all clients (fail-closed). |
| `haproxy-ingress.github.io/auth-tls-strict` | Not carried over | Accepted for compatibility but has no separate effect. Upstream defaults it to true (fail-closed on a missing/invalid CA); here a missing CA fails open (see auth-tls-secret), and this annotation can't restore fail-closed. For soft verification use auth-tls-verify-client optional instead. |
| `haproxy-ingress.github.io/auth-tls-verify-client` | Behaviour differs | "on"→required; "optional"/"optional_no_ca"→optional (HAProxy has no verify-but-accept-invalid mode); other values fail the render. |
| `haproxy-ingress.github.io/auth-url` | Behaviour differs | External auth via the SPOA hub external-auth plugin, which (unlike the nginx-ingress library) **isn't** auto-enabled — enable spoaHub.plugins.external-auth, otherwise the auth is silently not enforced. |
| `haproxy-ingress.github.io/backend-protocol` | Behaviour differs | h1, h2, h1-ssl and h2-ssl are accepted (h1-ssl/h2-ssl enable TLS); other values fail the render — note this is a different value set from ingress-nginx's `HTTP/HTTPS/GRPC/GRPCS`. |
| `haproxy-ingress.github.io/config-backend` | Behaviour differs | Raw HAProxy directives injected into the backend section (already HAProxy syntax in this source controller). Rejected unless `extraContext.ingressPermissions.allowRawHAProxyConfig` is `true`. |
| `haproxy-ingress.github.io/config-defaults` | Behaviour differs | Raw HAProxy directives injected into the defaults section. Rejected unless `extraContext.ingressPermissions.allowRawHAProxyConfig` is `true`. |
| `haproxy-ingress.github.io/config-frontend` | Behaviour differs | Injected into HAPTIC's shared HTTP frontend (before routing), not a per-Ingress frontend — directives apply process-wide; deduplication is the operator's responsibility. Rejected unless `extraContext.ingressPermissions.allowRawHAProxyConfig` is `true`. |
| `haproxy-ingress.github.io/config-global` | Behaviour differs | Raw HAProxy directives injected into the global section. Rejected unless `extraContext.ingressPermissions.allowRawHAProxyConfig` is `true`. |
| `haproxy-ingress.github.io/default-backend-redirect-code` | Behaviour differs | Default 302; an invalid code fails the render. |
| `haproxy-ingress.github.io/denylist-source-range` | Behaviour differs | Host-scoped — only gates rules with an explicit host; invalid CIDRs fail the render. |
| `haproxy-ingress.github.io/docs` | Not carried over | A pointer to jcmoraisjr/haproxy-ingress documentation, not a configuration key; not read. |
| `haproxy-ingress.github.io/forwardfor` | Behaviour differs | `add`, `update`, `ifmissing`, and `ignore` handle `X-Forwarded-For` as upstream does, per Ingress; the client's original value isn't copied to `X-Original-Forwarded-For`. |
| `haproxy-ingress.github.io/hsts` | Behaviour differs | By default the header is emitted only when the Ingress sets `hsts` to `"true"`, whereas jcmoraisjr/haproxy-ingress enables HSTS globally by default. Set extraContext.tls.hsts.enabled=true to send HSTS on all TLS hosts (matching that default); a per-Ingress `hsts` annotation still overrides the value for its hosts. |
| `haproxy-ingress.github.io/limit-connections` | Behaviour differs | Maps to backend `fullconn` (a soft full-queue threshold) rather than a hard per-server connection cap; must be a positive integer. |
| `haproxy-ingress.github.io/limit-rpm` | Behaviour differs | Same hard-cap/429 semantics, and ignored when limit-rps is also set (one stick-table per backend). |
| `haproxy-ingress.github.io/limit-rps` | Behaviour differs | Hard per-source-IP cap rejecting with 429 — jcmoraisjr/haproxy-ingress applies a burst allowance. |
| `haproxy-ingress.github.io/oauth` | Behaviour differs | Only "oauth2_proxy"/"oauth2-proxy" is supported and requires an Ingress path (oauth-uri-prefix, default /oauth2) routing to the oauth2-proxy Service — otherwise the render fails; `auth-url` takes precedence; needs the external-auth plugin (not auto-enabled) with plaintext allowed. |
| `haproxy-ingress.github.io/path-type` | Behaviour differs | regex, exact, prefix, and begin are honoured, but only for paths with `pathType` ImplementationSpecific — the annotation is ignored on Prefix/Exact-typed paths. |
| `haproxy-ingress.github.io/redirect-to-code` | Behaviour differs | Default 302; an out-of-range code silently falls back to 302 rather than failing. |
| `haproxy-ingress.github.io/secure-crt-secret` | Behaviour differs | Presents a client certificate to the upstream from the Secret; a missing Secret or missing tls.crt/tls.key renders a warning comment and skips the client cert instead of failing. |
| `haproxy-ingress.github.io/secure-verify-ca-secret` | Behaviour differs | Verifies the upstream certificate against the Secret's ca.crt; a missing Secret or missing ca.crt renders a warning comment and silently downgrades to no verification instead of failing. |
| `haproxy-ingress.github.io/ssl-cipher-suites-backend` | Behaviour differs | TLS 1.3 cipher suites for upstream TLS — only applied when backend TLS is enabled; otherwise silently ignored. |
| `haproxy-ingress.github.io/ssl-ciphers-backend` | Behaviour differs | Cipher list for upstream TLS — only applied when backend TLS is enabled (secure-backends or an -ssl backend-protocol); otherwise silently ignored. |
| `haproxy-ingress.github.io/ssl-redirect-code` | Behaviour differs | Default 302 (jcmoraisjr/haproxy-ingress redirects with 302); an out-of-range code silently falls back to 302 rather than failing. |
| `haproxy-ingress.github.io/waf` | Behaviour differs | Only "modsecurity" is supported (enforced by the bundled Coraza WAF via the SPOA hub); requires the coraza plugin (auto-enabled with this library) or the render fails. |
| `haproxy-ingress.github.io/waf-mode` | Behaviour differs | "deny" (default) enforces, "detect" runs in shadow mode; other values, or `waf-mode` without `waf`, fail the render. |
| `haproxy-ingress.github.io/whitelist-source-range` | Behaviour differs | Deprecated alias of allowlist-source-range, honoured only when allowlist-source-range is absent; host-scoped. |
<!-- END generated: migration-coverage haproxy-ingress -->

## Differences from HAProxy Technologies {#haproxytech}

<!-- BEGIN generated: migration-coverage haproxytech -->
The library classifies 56 `haproxy.org/*` annotations: 35 supported, 16 with behaviour differences, 5 not carried over, 0 failing.

| Annotation | Status | What to check |
|------------|--------|---------------|
| `haproxy.org/allow-list` | Behaviour differs | Host-scoped source-IP allowlist — only gates rules with an explicit host; invalid CIDRs fail the render. |
| `haproxy.org/auth-realm` | Behaviour differs | Default "Protected-Content" (matching the upstream controller); spaces in the realm are replaced with dashes, as upstream does. |
| `haproxy.org/auth-secret` | Behaviour differs | Secret format is one key per username with a base64(hash) value — different from ingress-nginx's htpasswd. By default only bcrypt, SHA-256/SHA-512 crypt and yescrypt hashes are accepted; `$1$` (MD5-crypt) is refused. A missing Secret makes the route serve 503 until it appears, and writing such an Ingress is rejected by the admission webhook. |
| `haproxy.org/auth-type` | Behaviour differs | Only "basic-auth" is supported; other values fail the render (note the value differs from ingress-nginx's "basic"). |
| `haproxy.org/backend-config-snippet` | Behaviour differs | Raw HAProxy directives injected into the backend section. Rejected unless `extraContext.ingressPermissions.allowRawHAProxyConfig` is `true`. |
| `haproxy.org/blacklist` | Behaviour differs | Deprecated alias of deny-list, honoured only when deny-list is absent; host-scoped. |
| `haproxy.org/cookie-persistence-no-dynamic` | Behaviour differs | Static (non-dynamic) cookie stickiness; setting it together with cookie-persistence fails the render. |
| `haproxy.org/deny-list` | Behaviour differs | Host-scoped source-IP denylist — only gates rules with an explicit host; invalid CIDRs fail the render. |
| `haproxy.org/forwarded-for` | Behaviour differs | "true" keeps the controller-wide X-Forwarded-For handling, which replaces a client-sent value unless the source is a trusted proxy; "false" forwards the client's header unchanged; other values fail the render. |
| `haproxy.org/pod-maxconn` | Behaviour differs | Divided across the number of ready HAProxy pods (quantized to a power of two) rather than applied per-server verbatim; must be a positive integer. |
| `haproxy.org/request-redirect-code` | Behaviour differs | Default 302; an invalid code fails the render. |
| `haproxy.org/scale-server-slots` | Not carried over | Inert; servers are named after their pods (ADR-0011), so there is no slot pool to size. Setting it emits an UnsupportedAnnotation Warning Event. |
| `haproxy.org/send-proxy-protocol` | Behaviour differs | proxy, proxy-v1, proxy-v2, proxy-v2-ssl and proxy-v2-ssl-cn map to the matching send-proxy flags; any other value is silently ignored. |
| `haproxy.org/server-ca` | Behaviour differs | Verifies the upstream certificate against the Secret's ca.crt; a missing Secret or missing ca.crt renders a warning comment and silently skips verification instead of failing. |
| `haproxy.org/server-crt` | Behaviour differs | Presents a client certificate to the upstream from the Secret; a missing Secret or missing tls.crt/tls.key renders a warning comment and skips the client cert instead of failing. |
| `haproxy.org/src-ip-header` | Behaviour differs | Rewrites the source IP from the named header (set-src) for the route, from a per-route map. |
| `haproxy.org/standalone-backend` | Not carried over | Not implemented; HAPTIC always shares the backend model. |
| `haproxy.org/timeout-client` | Not carried over | A frontend-level timeout owned by HAPTIC's shared frontend, not settable per Ingress. |
| `haproxy.org/timeout-http-keep-alive` | Not carried over | A frontend-level timeout owned by HAPTIC's shared frontend, not settable per Ingress. |
| `haproxy.org/timeout-http-request` | Not carried over | A frontend-level timeout owned by HAPTIC's shared frontend, not settable per Ingress. |
| `haproxy.org/whitelist` | Behaviour differs | Deprecated alias of allow-list, honoured only when allow-list is absent; host-scoped. |
<!-- END generated: migration-coverage haproxytech -->
