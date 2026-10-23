# Gateway API library

Route HTTP, gRPC, TLS, and TCP traffic with Gateway API resources. This library
is enabled by default; HAPTIC activates the route types whose CRDs are installed
and detects newly installed types without a restart.

For your first route, follow [Expose a Service through a Gateway](../gateway-api.md#expose-a-service-through-a-gateway).
Use this reference for request matching, weighted backends, header changes,
rewrites, redirects, and TLS settings. [Gateway route policies](../operations/gateway-policies.md)
add authentication, shared rate limits, web application firewall (WAF) inspection,
and HTTP caching.

<a id="overview"></a>

Try adding a hostname to the sample HTTPRoute:

<div class="pg-embed" markdown data-scenario="gateway" data-facade="spec.templateSnippets.map-host-500-gateway" data-tab="haproxy.cfg" data-controls="tabs,resources" data-input="resources" data-input-focus="api.example.com" data-title="Gateway API → HAProxy config" data-height="440">

<p class="pg-task" markdown>In the **Resources** panel, add `- www.example.com` under the `api` HTTPRoute's `spec.hostnames`, then open the **maps** tab and watch `host.map` gain a second entry.</p>

<details class="pg-hint" markdown>
<summary>What to expect</summary>

The output gains a `www.example.com` entry alongside `api.example.com`. Both
route through the same Gateway. The map keys include a port suffix because
this example's listener accepts any hostname on its port.

</details>

</div>

For release-specific core and extended coverage, see
[Gateway API conformance evidence](../operations/gateway-conformance.md).

## Configuration

```yaml
controller:
  templateLibraries:
    gateway:
      enabled: true  # Enabled by default
```

## Available resource types

<a id="watched-resources"></a>

HAPTIC uses the first API version available for each kind and watches for CRD
changes. Kinds that aren't installed remain inactive; installing them later
doesn't require a chart upgrade.

| Resource | API version candidates (preferred first) | Purpose |
|----------|-------------------------------------------|---------|
| Gateways | v1, v1beta1 | Gateway definitions (filtered by `gatewayClass.name` — see [GatewayClass](../gateway-class.md)) |
| GatewayClasses | v1, v1beta1 | GatewayClass definitions (field-selector scoped to owned class) |
| HTTPRoutes | v1, v1beta1 | HTTP routing rules |
| GRPCRoutes | v1, v1alpha2 | gRPC routing rules |
| TLSRoutes | v1, v1alpha3, v1alpha2 | TLS passthrough routing rules |
| TCPRoutes | v1, v1alpha2 | Raw-TCP listener forwarding |
| ReferenceGrants | v1, v1beta1 | Cross-namespace reference policy |
| ListenerSets | v1 | Additional listeners attached to Gateways (Gateway Enhancement Proposal (GEP) 1713) |
| BackendTLSPolicies | v1, v1alpha3 | Backend TLS validation (v1alpha2 is excluded: incompatible shape) |
| Namespaces | v1 (required) | Namespace metadata for listener attachment evaluation |
| Services | v1 (required) | Service discovery |
| EndpointSlices | `discovery.k8s.io/v1` (required) | Backend endpoints |

Features whose *fields* don't exist in an older release's schemas (for
example the HTTPRoute Cross-Origin Resource Sharing (CORS) filter before Gateway API v1.6, or Gateway
frontend mTLS) stay inactive on that release. TLS certificates come from
Kubernetes Secrets; see [certificate configuration](../ssl-certificates.md).

## Supported Gateway API versions and channels

Check both the Gateway API release and installation channel when choosing
features. See [conformance coverage](../operations/gateway-conformance.md) for
the versions and profiles tested with HAPTIC.

Which kinds a Gateway API install provides depends on its **channel**. The standard channel (`standard-install.yaml`) covers most kinds; a few graduated from the experimental channel (`experimental-install.yaml`) only in recent releases:

| Kind | Channel |
|------|---------|
| Gateway, GatewayClass | Standard |
| HTTPRoute | Standard |
| GRPCRoute | Standard (since Gateway API v1.1) |
| ReferenceGrant | Standard |
| BackendTLSPolicy | Standard |
| TLSRoute | Standard since Gateway API v1.5; experimental channel before |
| TCPRoute | Standard since Gateway API v1.6; experimental channel before |
| ListenerSet | Experimental channel (GEP-1713) |

Installing the v1.6.0 standard channel gives you every route kind, including TLSRoute and TCPRoute. On Gateway API v1.5, install the experimental channel for TCPRoute; before v1.5, install it for both TLSRoute and TCPRoute.

The word "experimental" describes two independent things, which don't gate each other:

- **Channel** — which route *kinds* (CRDs) a Gateway API install ships, shown in the table above.
- **The `controller.templateLibraries.gateway.experimentalChannel` value** — a separate switch that tells HAPTIC's `validationTests` the experimental **HTTPRoute schema** is installed, so tests exercising experimental HTTPRoute *fields* (`retry` per GEP-1731, `sessionPersistence` per GEP-1619, the `ExternalAuth` filter) run. HAPTIC emits those directives whenever the fields are present, regardless of the flag; see the [Chart Values Reference](../reference.md). This value gates no route kind.

<a id="architecture"></a>

## `ListenerSet` delegation

A ListenerSet adds listeners to a Gateway. The parent Gateway must explicitly
allow it through `spec.allowedListeners.namespaces`: `Same`, `All`, or a namespace
`Selector`. Without that permission, HAPTIC reports `Accepted=False` and doesn't
use the ListenerSet.

Set the ListenerSet's `spec.parentRef` to the Gateway and declare its listeners
under `spec.listeners`. Routes attach with `parentRefs[].kind: ListenerSet`, its
name, and an optional `sectionName` selecting one listener. The listener's
`allowedRoutes` controls which route namespaces can attach. HAPTIC publishes
ListenerSet and parent Gateway status for these attachments.

## Gateway TLS policies

Server certificates for HTTPS and terminating TLS listeners come from
`listeners[].tls.certificateRefs`; see [Gateway certificates](../ssl-certificates.md#gateway-api).
Client authentication and upstream verification are separate settings:

| Purpose | Configuration | Behavior |
| --- | --- | --- |
| Authenticate clients | Gateway `spec.tls.frontend.default.validation` | `mode: AllowValidOnly` requires a valid client certificate from `caCertificateRefs`; core ConfigMaps and Secrets supply `ca.crt`. |
| Override client authentication by port | Gateway `spec.tls.frontend.perPort[].tls.validation` | Applies the selected validation policy to listeners on that entry's `port`. |
| Verify upstream servers | BackendTLSPolicy `spec.targetRefs` and `spec.validation` | Targets a Service, optionally a named port, and checks its certificate against the configured CA and hostname. |

Frontend validation requires a Gateway API schema that serves `spec.tls.frontend`.
Cross-namespace CA and server-certificate references require a covering
[ReferenceGrant](#cross-namespace-routes-referencegrant). Invalid references appear
in status; a listener without usable required client trust doesn't accept traffic.

A BackendTLSPolicy's `validation.hostname` sets upstream SNI and the certificate
name to verify. If you set `validation.subjectAltNames`, it must contain one
Hostname entry equal to `validation.hostname`. HAProxy uses
[SNI for certificate verification](https://docs.haproxy.org/3.4/configuration.html#5.2-verifyhost),
so it can't verify a different SAN independently. URI identities and multiple
alternative names are also unsupported. Admission rejects these policies;
existing policies report `Accepted=False` and block backend traffic.
Supply trust through
`validation.caCertificateRefs` or
`validation.wellKnownCACertificates: System`. Policies apply to HTTPRoute,
GRPCRoute, TCPRoute, and terminating TLSRoute backends. A policy with no usable
CA blocks that backend rather than sending plaintext. Passthrough TLSRoute
connections retain the client's TLS session.

## HTTPRoute support

The `spec:` examples below show the fields to add to an existing route. Keep its
`parentRefs` so it stays attached to your Gateway. The named backend Services must
exist and expose the listed ports. For complete manifests and a test request, use
the [Gateway tutorial](../gateway-api.md).

### spec.parentRefs

| Field | Notes |
| ------- | ------- |
| `parentRefs[].name` | Gateway or ListenerSet reference; set `kind: ListenerSet` for a ListenerSet |
| `parentRefs[].namespace` | Parent namespace; attachment must satisfy the listener's `allowedRoutes` |
| `parentRefs[].sectionName` | Selects a named listener for attachment, routing, and status |
| `parentRefs[].port` | Pins the route to Gateway listeners on the named port (attachment selection per spec); a route only attaches to listeners whose port matches |

### spec.hostnames

| Field | Status | Notes |
|-------|--------|-------|
| `hostnames[]` | Supported | Multiple hostnames per route |
| Wildcard hostnames (for example `*.example.com`) | Untested | Wildcard matching is implemented but lacks a dedicated validation test |
| Empty hostnames list | Supported | Matches all hosts |

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: example
spec:
  parentRefs:
    - name: edge
  hostnames:
    - "example.com"
    - "www.example.com"
  rules:
    - backendRefs:
        - name: example-svc
          port: 80
```

### `spec.rules[].matches` - path matching

| Field | Notes |
| ------- | ------- |
| `matches[].path.type: Exact` | Matches the whole path |
| `matches[].path.type: PathPrefix` | Matches the path prefix at a segment boundary |
| `matches[].path.type: RegularExpression` | Matches a regular expression from the start of the path; HAPTIC rejects a pattern whose groups or `[ ]` classes don't close within the pattern |
| `matches[].path.value` | Path value used in matching |
| Empty matches list | Defaults to PathPrefix `/` |

**Path priority:** Exact > Regex > Prefix-exact > Prefix. See [change path matching order](../template-libraries.md#path-matching-order).

**Example - Path matching:**

```yaml
spec:
  rules:
    # Exact path match
    - matches:
        - path:
            type: Exact
            value: /api/v1/users
      backendRefs:
        - name: users-api-svc
          port: 8080

    # Prefix match
    - matches:
        - path:
            type: PathPrefix
            value: /api
      backendRefs:
        - name: api-svc
          port: 8080

    # Regex match
    - matches:
        - path:
            type: RegularExpression
            value: ^/api/v[0-9]+/.*
      backendRefs:
        - name: versioned-api-svc
          port: 8080
```

The path type decides which map file HAProxy consults — flip it live:

<div class="pg-embed" markdown data-scenario="gateway" data-facade="spec.templateSnippets.map-path-exact-500-gateway" data-tab="maps" data-controls="tabs,resources" data-title="Path type → map file" data-height="440">

<p class="pg-task" markdown>In the **Resources** panel, change the `api` HTTPRoute's `path.type` from `PathPrefix` to `Exact`, then watch the entry move between map files in the **maps** tab.</p>

<details class="pg-hint" markdown>
<summary>What to expect</summary>

With `PathPrefix`, the route's entry (`api.example.com…/ GW_ROUTE_ID:http:platform_api_0`) sits in `path-prefix.map`. Switch to `Exact` and the same entry moves to `path-exact.map`, leaving `path-prefix.map` empty. Each path type is filled by its own snippet — `map-path-exact-500-gateway` and `map-path-prefix-500-gateway` — and a route's entry only lands in the map whose `pathType` matches.

</details>

</div>

### `spec.rules[].matches` - method, header, and query matching

| Field | Notes |
| ------- | ------- |
| `matches[].method` | HTTP method matching (GET, POST, etc.) |
| `matches[].headers[]` | Header-based routing with exact and regex matching |
| `matches[].headers[].type: Exact` | Exact header value matching |
| `matches[].headers[].type: RegularExpression` | Regex header value matching |
| `matches[].headers[].name` | Case-insensitive header name |
| `matches[].headers[].value` | Header value to match |
| `matches[].queryParams[]` | Query parameter matching |
| `matches[].queryParams[].type: Exact` | Exact query parameter value matching |
| `matches[].queryParams[].type: RegularExpression` | Regex query parameter matching |
| `matches[].queryParams[].name` | Query parameter name |
| `matches[].queryParams[].value` | Query parameter value to match |

**Match precedence in HAPTIC:**

When multiple routes match the same request, ties are broken in the following order:

1. **Path specificity** - Exact > RegularExpression > PathPrefix (by length)
2. **Method matchers** - Routes with method matchers have higher priority
3. **Header matchers** - More header matchers = higher priority
4. **Query parameter matchers** - More query matchers = higher priority
5. **Creation timestamp** - Older routes have priority
6. **Alphabetical order** - By namespace/name as final tie-breaker

**Example - Method matching:**

```yaml
spec:
  rules:
    # Match only GET requests
    - matches:
        - path:
            type: PathPrefix
            value: /api
          method: GET
      backendRefs:
        - name: api-read-svc
          port: 8080

    # Match only POST requests
    - matches:
        - path:
            type: PathPrefix
            value: /api
          method: POST
      backendRefs:
        - name: api-write-svc
          port: 8080
```

**Example - Header matching:**

```yaml
spec:
  rules:
    # Exact header match
    - matches:
        - path:
            type: PathPrefix
            value: /api
          headers:
            - name: X-API-Version
              type: Exact
              value: "v2"
      backendRefs:
        - name: api-v2-svc
          port: 8080

    # Regex header match
    - matches:
        - path:
            type: PathPrefix
            value: /api
          headers:
            - name: User-Agent
              type: RegularExpression
              value: ".*Mobile.*"
      backendRefs:
        - name: mobile-api-svc
          port: 8080
```

**Example - Query parameter matching:**

```yaml
spec:
  rules:
    # Exact query parameter match
    - matches:
        - path:
            type: PathPrefix
            value: /search
          queryParams:
            - name: category
              type: Exact
              value: electronics
      backendRefs:
        - name: electronics-search-svc
          port: 8080

    # Regex query parameter match
    - matches:
        - path:
            type: PathPrefix
            value: /api
          queryParams:
            - name: version
              type: RegularExpression
              value: "^v[2-3]$"
      backendRefs:
        - name: modern-api-svc
          port: 8080
```

**Example - Complex matching with precedence:**

```yaml
spec:
  rules:
    # Higher priority: method + headers + query
    - matches:
        - path:
            type: Exact
            value: /api/users
          method: POST
          headers:
            - name: Content-Type
              type: Exact
              value: application/json
          queryParams:
            - name: action
              type: Exact
              value: create
      backendRefs:
        - name: user-create-svc
          port: 8080

    # Lower priority: only path matching
    - matches:
        - path:
            type: Exact
            value: /api/users
      backendRefs:
        - name: user-generic-svc
          port: 8080
```

Add a matcher to the demo route and watch the frontend gain a condition:

<div class="pg-embed" markdown data-scenario="gateway" data-facade="spec.templateSnippets.frontend-matchers-advanced-500-gateway" data-tab="haproxy.cfg" data-controls="tabs,resources" data-title="Method / header / query matchers" data-height="440">

<p class="pg-task" markdown>In the **Resources** panel, add `method: GET` to the `api` HTTPRoute's match (as a sibling of its `path`), then find the matcher line in the `haproxy.cfg` tab.</p>

<details class="pg-hint" markdown>
<summary>What to expect</summary>

Under `# Advanced route matching`, the rule's provenance comment changes from `- path-only` to `- method GET`, and its `http-request set-var(txn.gw_rule_id) …` guard gains a `{ method GET }` condition. `frontend-matchers-advanced-500-gateway` emits that condition and the comment. Header and query matchers build the same guard: a `headers:` entry adds `{ req.hdr(<name>) "<value>" }` and a `queryParams:` entry adds `{ urlp(<name>) "<value>" }`.

</details>

</div>

### `spec.rules[].filters`

Header modifiers, redirects, rewrites, and mirrors store route values in maps.
Changing those values avoids a reload when the required processing rules already
exist. A new header name or filter type can add a rule and require a reload;
CORS and advanced matchers also change configuration text. See
[reload constraints](#known-limitations).

| Filter Type | Conformance | Status | Notes |
|-------------|-------------|--------|-------|
| `RequestHeaderModifier` | Core | Supported | Add/Set/Remove request headers |
| `ResponseHeaderModifier` | Extended | Supported | Add/Set/Remove response headers |
| `RequestRedirect` | Core | Supported | HTTP redirects with scheme/hostname/port/path/statusCode |
| `URLRewrite` | Extended | Supported | Path and hostname rewriting |
| `RequestMirror` | Extended | Supported | Enable `spoaHub.plugins.mirror`; supports percentage or fraction sampling and multiple mirrors per rule |
| `CORS` | Extended (GEP-1767) | Supported | HTTPRoute only. Supports `allowOrigins` (exact values, a bare `*`, and `*.`-prefixed wildcards compiled to a regex against the request `Origin`), `allowMethods`, `allowHeaders`, `exposeHeaders`, `allowCredentials`, and `maxAge` |
| `ExternalAuth` | Extended (experimental channel) | Partial | HTTPRoute only. `protocol: HTTP` through the SPOA hub external-auth plugin; `GRPC` and `forwardBody` are unsupported. See [`ExternalAuth` filter](#externalauth-filter) |
| `ExtensionRef` | Implementation-specific | Partial | Supports `HAProxyRoutePolicy` for route policies and `SSLPassthrough` for TLS passthrough; other kinds are unsupported |

#### `RequestHeaderModifier` filter

Modify request headers before forwarding to backends:

- `set` - Sets a header value, replacing any existing values
- `add` - Adds a header value, appending to existing values
- `remove` - Removes all values for a header

**Example - Set and add headers:**

```yaml
spec:
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /api
      filters:
        - type: RequestHeaderModifier
          requestHeaderModifier:
            set:
              - name: X-API-Version
                value: "v2"
            add:
              - name: X-Deployment
                value: "canary"
            remove:
              - Authorization
      backendRefs:
        - name: api-svc
          port: 8080
```

Header names are case-insensitive. A modifier on a `backendRef` runs after the
rule-level modifier and takes precedence for the same header. HAPTIC rejects
invalid header names during validation.

#### `ResponseHeaderModifier` filter

The `ResponseHeaderModifier` filter modifies HTTP response headers before returning to clients. Supports the same set/add/remove operations as RequestHeaderModifier.

**Example - Add security headers:**

```yaml
spec:
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /
      filters:
        - type: ResponseHeaderModifier
          responseHeaderModifier:
            set:
              - name: Strict-Transport-Security
                value: "max-age=31536000; includeSubDomains"
              - name: X-Frame-Options
                value: "DENY"
            add:
              - name: X-Custom-Header
                value: "custom-value"
            remove:
              - Server
              - X-Powered-By
      backendRefs:
        - name: web-svc
          port: 80
```

#### `RequestRedirect` filter

The `RequestRedirect` filter implements HTTP redirects with support for scheme, hostname, port, path, and status code modifications. **Only available for HTTPRoute** (not applicable to gRPC).

**Supported Fields:**

- `scheme` - Change protocol (http/https)
- `hostname` - Change destination hostname
- `port` - Change destination port
- `path.type` - ReplaceFullPath or ReplacePrefixMatch
- `path.replaceFullPath` - New absolute path
- `path.replacePrefixMatch` - New path prefix
- `statusCode` - HTTP status code (default: 302)

**Example - HTTPS redirect:**

```yaml
spec:
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /
      filters:
        - type: RequestRedirect
          requestRedirect:
            scheme: https
            statusCode: 301
```

**Example - Path rewrite with redirect:**

```yaml
spec:
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /old-api
      filters:
        - type: RequestRedirect
          requestRedirect:
            path:
              type: ReplacePrefixMatch
              replacePrefixMatch: /api/v2
            statusCode: 308
```

Redirects preserve the request's query string. Supported status codes are
`301`, `302`, `303`, `307`, and `308`.

#### `URLRewrite` filter

The `URLRewrite` filter rewrites request URLs before forwarding to backends, supporting both hostname and path modifications. **Only available for HTTPRoute** (not applicable to gRPC).

**Supported Fields:**

- `hostname` - Rewrite the Host header
- `path.type` - ReplaceFullPath or ReplacePrefixMatch
- `path.replaceFullPath` - New absolute path
- `path.replacePrefixMatch` - New path prefix

**Example - Strip path prefix:**

```yaml
spec:
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /api/v1
      filters:
        - type: URLRewrite
          urlRewrite:
            path:
              type: ReplacePrefixMatch
              replacePrefixMatch: /
      backendRefs:
        - name: api-svc
          port: 8080
```

**Example - Hostname and path rewrite:**

```yaml
spec:
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /external
      filters:
        - type: URLRewrite
          urlRewrite:
            hostname: internal-api.example.svc.cluster.local
            path:
              type: ReplacePrefixMatch
              replacePrefixMatch: /api
      backendRefs:
        - name: internal-api-svc
          port: 8080
```

`ReplacePrefixMatch` requires a `PathPrefix` match. HAPTIC rejects it with a
`RegularExpression` match because the replacement prefix length is undefined.

**Difference from RequestRedirect:**

- **URLRewrite** rewrites the request and forwards to backend (transparent to client)
- **RequestRedirect** sends HTTP redirect response to client (client sees new URL)

#### `ExternalAuth` filter

The `ExternalAuth` filter asks an authorization service about each request that
matches the rule, before the request reaches a backend. **Only available for
HTTPRoute.** HAPTIC sends the check through the SPOA hub external-auth plugin.

The filter is a Gateway API experimental-channel field. The controller reads
route schemas when it starts, so install the experimental channel CRDs first. If
you install them later, restart the controller.

Enable the plugin. For an authorization Service reached over plain HTTP, also
allow plaintext requests:

```yaml
spoaHub:
  plugins:
    external-auth:
      enabled: true
      params: |
        timeout_ms = 5000
        fail_open = false
        allow_plaintext = true
```

Attach the filter to a rule:

```yaml
spec:
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /api
      filters:
        - type: ExternalAuth
          externalAuth:
            protocol: HTTP
            backendRef:
              name: auth-service
              port: 8080
            http:
              path: /verify
              allowedHeaders: [X-Api-Key]
              allowedResponseHeaders: [X-Auth-User]
      backendRefs:
        - name: api-svc
          port: 8080
```

For each request, HAPTIC:

- Sends the client's method to
  `http://<service>.<namespace>.svc.cluster.local:<port><http.path><request path>`,
  without the query string or body. The request uses HTTPS when the Service port
  has `appProtocol: https`. A `BackendTLSPolicy` isn't applied to this request.
- Forwards the `Host` and `Authorization` headers, plus the headers in
  `http.allowedHeaders`. Other headers, including `Cookie`, aren't forwarded
  unless you list them.
- Lets the request through only when the authorization service answers `200`.
  Any other status, a timeout, or an unreachable service returns `401` to the
  client, and the access log records `denied_by: external_auth`. The plugin's
  `fail_open` setting doesn't apply to Gateway routes.
- Copies the headers in `http.allowedResponseHeaders` from the authorization
  response to the backend request. A copy the client sent is removed first, even
  when the authorization response doesn't include that header.

A `backendRef` in another namespace needs a covering
[ReferenceGrant](#cross-namespace-routes-referencegrant).

HAPTIC can't enforce the following settings. A rule using them answers `500`
(`denied_by: external_auth_unavailable`), and the route status reports the
problem:

| Setting | Fix |
|---------|-----|
| `protocol: GRPC` | Use an authorization service that speaks HTTP and set `protocol: HTTP` |
| `forwardBody.maxSize` greater than `0` | Remove `forwardBody`; the plugin doesn't forward request bodies |
| Empty `http.allowedResponseHeaders` | List the headers to copy. Gateway API asks for every header in this case, but HAProxy needs each header name in its configuration |
| A header name with characters other than letters, digits, and `-` | Rename the header |
| `Host`, `Content-Length`, `Transfer-Encoding`, `Connection`, `Keep-Alive`, `TE`, `Trailer`, `Upgrade`, `Proxy-Connection`, `Allowed`, `Status-Code`, `Redirect-URL`, or `Error` in `http.allowedResponseHeaders` | Remove the header from the list |
| The filter on a `backendRefs[]` entry | Move the filter to the rule's `filters` |
| The external-auth plugin is disabled | Enable `spoaHub.plugins.external-auth` |
| A plain-HTTP authorization Service without `allow_plaintext = true` | Set `allow_plaintext = true` in the plugin's `params` |

A missing Service or a missing ReferenceGrant also answers `500` and sets
`ResolvedRefs=False` on the route.

Add a header modifier to the demo route and inspect its generated configuration:

<div class="pg-embed" markdown data-scenario="gateway" data-facade="spec.templateSnippets.frontend-filters-495-gateway-route-filters" data-tab="haproxy.cfg" data-controls="tabs,resources" data-title="Filter → http-request directive" data-height="440">

<p class="pg-task" markdown>In the **Resources** panel, give the `api` HTTPRoute's rule a `filters:` list (a sibling of `backendRefs`) with a `RequestHeaderModifier` that sets a header — `set: [{name: X-API-Version, value: "v2"}]` — then find the generated `http-request set-header X-API-Version` line in the `haproxy.cfg` tab, and its value in the `gw-reqhdr.map` tab.</p>

<details class="pg-hint" markdown>
<summary>What to expect</summary>

`gw-reqhdr.map` gains `default_api_0|set|x-api-version v2`, and one
`http-request set-header X-API-Version` line appears under the filters, reading
that entry through the route's `gw_rule_id` — so it fires only for requests the
`api` HTTPRoute selected, not for every request on the frontend. The
`frontend-filters-495-gateway-route-filters` snippet emits it; `add`/`remove`
operations become `http-request add-header` / `http-request del-header` lines the
same way. Add a second route setting the same header name and the map grows by one
entry while the configuration stays byte-identical — that's what lets the change
deploy without a reload.

</details>

</div>

### `spec.rules[].backendRefs`

| Field | Status | Notes |
|-------|--------|-------|
| `backendRefs[].name` | Supported | Service name |
| `backendRefs[].namespace` | Supported | Defaults to the route namespace; cross-namespace Services require a covering [ReferenceGrant](#cross-namespace-routes-referencegrant) |
| `backendRefs[].port` | Supported | Service port number |
| `backendRefs[].weight` | Supported | Traffic splitting with weighted distribution |
| `backendRefs[].filters[]` | Partial | Supports `RequestHeaderModifier`, `ResponseHeaderModifier`, `RequestRedirect`, and `URLRewrite`. `RequestMirror` and `ExtensionRef` are unsupported here; an `ExternalAuth` filter here makes the rule answer `500` |
| Multiple backends | Supported | Traffic splits according to weights |
| Single backend | Supported | All matching traffic goes to this backend |
| Omitted weight | Supported | Defaults to weight 1 |
| Explicit `weight: 0` | Supported | The backend remains configured but receives no traffic |

Weights express relative shares: `70` and `30` send about 70% and 30% of requests
to the two Services. Omitted weights default to `1`; a weight of `0` receives no traffic.

**Example - Weighted traffic splitting:**

```yaml
spec:
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /app
      backendRefs:
        # 70% of traffic
        - name: app-v1
          port: 80
          weight: 70
        # 30% of traffic
        - name: app-v2
          port: 80
          weight: 30
```

**Example - Default weights:**

```yaml
spec:
  rules:
    - backendRefs:
        # Omitted weight defaults to 1 (50/50 split)
        - name: backend-a
          port: 80
        - name: backend-b
          port: 80
```

Split the demo route's traffic and inspect the generated weight map:

<div class="pg-embed" markdown data-scenario="gateway" data-facade="spec.templateSnippets.map-weighted-backend-500-gateway" data-tab="maps" data-controls="tabs,resources" data-title="Weighted traffic split → map" data-height="440">

<p class="pg-task" markdown>In the **Resources** panel, give the `api` HTTPRoute a second backend: add `weight: 90` to its existing `api` ref and append `- {name: api-canary, port: 80, weight: 10}`, then open `weighted-multi-backend.map` in the **maps** tab.</p>

<details class="pg-hint" markdown>
<summary>What to expect</summary>

`weighted-multi-backend.map` contains 100 entries keyed `<0-99>:h_platform_api_0`: indexes 0–89 select `gw_h_platform_api_api_80`, and 90–99 select `gw_h_platform_api_api-canary_80`. Only rules with multiple backend references need this weight map. The new `backend gw_h_platform_api_api-canary_80` block appears in the `haproxy.cfg` tab; it has no servers until an `api-canary` Service exists.

</details>

</div>

### Timeouts, retries, and session persistence

These fields require an installed Gateway API schema that serves them. The chart
uses them when present; `controller.templateLibraries.gateway.experimentalChannel`
also enables their experimental-channel validation fixtures.

| Field | Behavior |
| --- | --- |
| HTTPRoute/GRPCRoute `rules[].timeouts.request` | Ignored. HAProxy has no native overall request deadline. See [Timeout limits](#timeout-limits). |
| HTTPRoute/GRPCRoute `rules[].timeouts.backendRequest` | Sets HAProxy's server inactivity timeout for the selected rule. An absent value keeps the backend default; zero uses the maximum timeout. See [Timeout limits](#timeout-limits). |
| HTTPRoute `rules[].retry.attempts` | Sets the retry count for the rule's backends, including h2c backends; `0` disables retries. The default is `3`. |
| HTTPRoute `rules[].retry.codes` | Retries on the listed statuses as well as on connection failures, empty responses, and response timeouts. Without `codes`, the statuses are 500, 502, 503, and 504. |
| HTTPRoute `rules[].retry.backoff` | Not applied: HAProxy retries without waiting. See [Retry limits](#retry-limits). |
| HTTPRoute/GRPCRoute `rules[].sessionPersistence` | `type: Cookie` keeps a client on one pod through a cookie, `type: Header` through a response header the client sends back. `absoluteTimeout`, and `idleTimeout` on schemas that serve it, end the session. |

#### Timeout limits

HAPTIC follows [HAProxy Unified Gateway's timeout mapping](https://github.com/haproxytech/haproxy-unified-gateway/blob/a115676aa573761ced20b688cb5caef585d83b91/k8s/gate/haproxy/backends.go#L656-L665):
only `backendRequest` configures HAProxy's server timeout. Setting `request`
alone leaves the backend default unchanged; setting both fields applies only
`backendRequest`.

This is an inactivity timeout: traffic restarts the timer, and retries can make
the total request take longer. HAPTIC doesn't enforce the Gateway API's overall
request deadline or an absolute deadline for each backend attempt. Applications
that need a total deadline must enforce it themselves.

A zero `backendRequest` value uses HAProxy's maximum timeout of 2,147,483,647 ms
(about 24.9 days). HAProxy's per-request `set-timeout server 0` restores the backend
default instead of disabling the timer. Other connection and client timeouts
still apply.

#### Retry limits

- HAProxy retries on these statuses only: 401, 403, 404, 408, 421 (HAProxy 3.1
  and later), 425, 429, 500, 501, 502, 503, and 504. HAPTIC drops any other
  code from `retry.codes` and records a `RetryCodeUnsupported` Warning event on
  the route.
- HAProxy has no delay between retries. It waits `min(timeout connect, 1s)`
  (100 ms with the chart's default `timeout connect`) only before reconnecting
  to the same server after a failed connection, and retries everything else
  immediately. A route that sets a nonzero `backoff` gets a
  `RetryBackoffUnsupported` Warning event.
- Retries have no overall deadline; `timeouts.backendRequest` limits server inactivity.
- HAProxy retries a request on a status code, an empty response, or a response
  timeout only when the request fits in one buffer (`tune.bufsize`) and its
  method is idempotent. Set
  `controller.config.templatingSettings.extraContext.retryNonIdempotent` to
  `true` to also retry other methods.

#### Session persistence

Name the cookie with `sessionPersistence.cookie.name` and the header with
`sessionPersistence.header.name` when the installed schema serves those fields,
or with `sessionPersistence.sessionName` on earlier schemas. The default names
are `SESSION` for a cookie and `X-Session` for a header. A name may contain
letters, digits, and `!$&*+.^_~-`. The admission webhook rejects a route using
any other character; a route stored anyway gets no session persistence
and an `InvalidSessionPersistence` Warning event.

With `type: Header`, the first response carries the session in the header, and
a request that sends the header back reaches the same pod for as long as that
pod serves the backend. The token is the cookie that `type: Cookie` would use,
so the response also carries a `Set-Cookie` for it with `Max-Age=0`, which
makes the client discard it. `lifetimeType: Permanent` gives a cookie a
`Max-Age` of `absoluteTimeout`.

A rule with several `backendRefs` picks the Service by weight before session
persistence applies, so a session can move to another Service of the same rule.

A rule with a `retry` or `sessionPersistence` policy gets its own backend,
named after the shared one with a `_r<rule index>` suffix, so its policy
applies to that rule only. Changing a policy changes the backend profile and can
require a reload.

<a id="advanced-features"></a>

Rules within one route reuse a backend for the same Service and port unless a
rule has its own `retry` or `sessionPersistence` policy. Separate routes have
separate backends.

### Misdirected requests on HTTPS listeners

When a Gateway has multiple HTTPS listeners with distinct hostnames, HAPTIC enforces RFC 9110 listener isolation. If a request's TLS SNI selects one HTTPS listener but its `Host` header canonically belongs to a *different* HTTPS listener on the Gateway, HAPTIC returns `421 Misdirected Request`.

The check applies only to HTTPS connections that carry an SNI. Plain-HTTP requests are unaffected.

---

## GRPCRoute support

### spec.parentRefs

| Field | Status | Notes |
|-------|--------|-------|
| All fields | Similar to HTTPRoute | Same template pattern and limitations |

### spec.hostnames

| Field | Notes |
| ------- | ------- |
| `hostnames[]` | Multiple hostnames per route |

### `spec.rules[].matches`

| Field | Notes |
| ------- | ------- |
| `matches[].method.type: Exact` | Exact match for gRPC service/method |
| `matches[].method.type: RegularExpression` | Regex match for gRPC service/method |
| `matches[].method.service` | gRPC service name (for example `com.example.User`) |
| `matches[].method.method` | gRPC method name (for example `GetUser`) |
| `matches[].headers[]` | Header matching (same as HTTPRoute) |

**gRPC Method Routing:**

Match gRPC calls by service and method, using their `/package.Service/Method` path.

**Example - gRPC method routing:**

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: GRPCRoute
metadata:
  name: grpc-users
spec:
  parentRefs:
    - name: edge
  hostnames:
    - "api.example.com"
  rules:
    # Route GetUser calls to read-only service
    - matches:
        - method:
            type: Exact
            service: com.example.UserService
            method: GetUser
      backendRefs:
        - name: user-read-svc
          port: 9090

    # Route CreateUser calls to write service
    - matches:
        - method:
            type: Exact
            service: com.example.UserService
            method: CreateUser
      backendRefs:
        - name: user-write-svc
          port: 9090

    # Route all other UserService calls with regex
    - matches:
        - method:
            type: RegularExpression
            service: com\.example\.UserService
            # Matches any method
      backendRefs:
        - name: user-general-svc
          port: 9090
```

### `spec.rules[].filters`

| Filter Type | Conformance | Status | Notes |
|-------------|-------------|--------|-------|
| `RequestHeaderModifier` | Core | Supported | Same behavior as HTTPRoute |
| `ResponseHeaderModifier` | Extended | Supported | Same behavior as HTTPRoute |
| `RequestRedirect` | Core | N/A | HTTPRoute only - not applicable to gRPC |
| `URLRewrite` | Extended | N/A | HTTPRoute only - not applicable to gRPC |
| `RequestMirror` | Extended | Supported | Enable `spoaHub.plugins.mirror`; supports percentage or fraction sampling and multiple mirrors per rule |
| `ExtensionRef` | Implementation-specific | Partial | Supports `HAProxyRoutePolicy`; other kinds are unsupported |

### `spec.rules[].backendRefs`

| Field | Notes |
| ------- | ------- |
| All `backendRefs` fields | Same behavior as HTTPRoute |
| HTTP/2 protocol | Backends generated with `proto h2` flag |

**Example - GRPCRoute:**

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: GRPCRoute
metadata:
  name: grpc-example
spec:
  parentRefs:
    - name: edge
  hostnames:
    - "grpc.example.com"
  rules:
    - backendRefs:
        - name: grpc-svc
          port: 9090
```

---

## TLSRoute support

TLSRoute selects backends by Server Name Indication (SNI). A `Passthrough`
listener forwards the client's encrypted stream unchanged. A `Terminate` listener
decrypts it; a BackendTLSPolicy can then require TLS and certificate verification
on the connection to the backend.

A rule attached to both modes gets separate backends. BackendTLSPolicy applies to
the terminating connection, while passthrough preserves the original TLS session.

### Example: passthrough Gateway and TLSRoute

This Gateway opens a `Passthrough` TLS listener on port 6443 and forwards `secure.example.com` — matched by SNI — to a backend that terminates TLS itself:

```bash
kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: tls-edge
  namespace: default
spec:
  gatewayClassName: haptic
  listeners:
    - name: tls
      protocol: TLS
      port: 6443
      tls:
        mode: Passthrough
      allowedRoutes:
        namespaces:
          from: Same
        kinds:
          - kind: TLSRoute
---
apiVersion: gateway.networking.k8s.io/v1
kind: TLSRoute
metadata:
  name: secure-app
  namespace: default
spec:
  parentRefs:
    - name: tls-edge
  hostnames:
    - secure.example.com
  rules:
    - backendRefs:
        - name: secure-app
          port: 8443
EOF
```

The `secure-app` Service must exist in `default` and accept TLS on port `8443`.
Clients connect to listener port `6443` with SNI `secure.example.com`; the backend
terminates their TLS sessions. Each TLSRoute needs a hostname. See
[TLSRoute limitations](#tlsroute-limitations).

### Attachment semantics

A TLSRoute attaches to a Gateway listener when every check in this table passes:

| Check | Behavior |
|-------|----------|
| `parentRefs[]` kind | Must reference a `Gateway` in group `gateway.networking.k8s.io` (both default when omitted) |
| `parentRefs[].sectionName` | When set, only the named listener is considered |
| `parentRefs[].port` | When set, only listeners on that port are considered |
| Listener protocol | Must be `TLS`; both `tls.mode: Passthrough` and `tls.mode: Terminate` accept TLSRoutes (an empty mode defaults to `Terminate`) |
| Mixed modes on one port | `Passthrough` and `Terminate` listeners share a port; SNI selects the listener before TLSRoute matching |
| `allowedRoutes.kinds` | Honored; when omitted, the protocol default (TLSRoute on TLS listeners) applies |
| `allowedRoutes.namespaces.from` | `Same`, `All`, and `Selector` (with `matchLabels`; `matchExpressions` isn't supported) |
| `spec.hostnames` | The route needs at least one hostname — routing is SNI-based, so a TLSRoute without hostnames attaches nowhere. Each route hostname is intersected with the listener hostname (wildcards supported); the intersection becomes the SNI the frontend matches |

### Forwarding behavior

- Passthrough routes can share the chart's HTTPS port with Ingress passthrough routes.
- On that shared address, Gateway ports whose TLS listeners all use `Passthrough` retain their listener claims even after their final TLSRoute is removed. A matching listener with no valid matching route rejects the connection, including when an Ingress also matches. Remove or narrow the Gateway listener to release that hostname. Names outside all Gateway listener claims can still reach Ingress routes.
- Use the per-Gateway Service for terminating or mixed TLS listeners. It forwards each listener port to an allocated HAProxy port, so the chart's HTTPS Service can use the same port number on its own address. The chart keeps ownership of its HTTPS bind.
- Wildcard server names such as `*.example.com` match by suffix.
- Connections with an unclaimed server name are rejected.
- A rule splits its connections across its `backendRefs` by `weight` (default `1`; a `weight: 0` ref receives none). A ref's port defaults to 443.
- The share of an invalid `backendRef` — a missing Service, another kind, or a cross-namespace ref without a ReferenceGrant — is rejected; a rule whose valid refs all have weight 0 rejects every connection.
- BackendTLSPolicy can re-encrypt traffic after termination; it doesn't affect passthrough traffic.

### Mixed termination on one port

Use separate TLS listeners with different hostnames to terminate some connections
and pass others through on the same port. HAPTIC selects the most specific
listener hostname first: exact names precede wildcards, longer wildcard suffixes
precede shorter ones, and a listener without a hostname comes last. If that
listener has no matching route or a required certificate is unavailable, HAPTIC
rejects the connection. It doesn't try a less specific listener.

Each terminating listener uses its own certificate and client-certificate
validation settings. Passthrough listeners forward the original TLS stream;
they don't use the Gateway's frontend client-validation policy. A
BackendTLSPolicy can encrypt the backend connection after termination.

This example expects two Services in `default`: `plaintext-app` accepts plaintext
TCP on port `3000`, and `secure-app` accepts TLS for `encrypted.example.com` on
port `8443`. Save the certificate and private key for `terminate.example.com` as
`server.crt` and `server.key`, then create the Secret and routes:

```bash
kubectl create secret tls mixed-termination --namespace default \
  --cert=server.crt --key=server.key --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f - <<'EOF'
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: mixed-tls
  namespace: default
spec:
  gatewayClassName: haptic
  listeners:
    - name: terminate
      protocol: TLS
      port: 8883
      hostname: terminate.example.com
      tls:
        mode: Terminate
        certificateRefs:
          - name: mixed-termination
    - name: passthrough
      protocol: TLS
      port: 8883
      hostname: encrypted.example.com
      tls:
        mode: Passthrough
---
apiVersion: gateway.networking.k8s.io/v1
kind: TLSRoute
metadata:
  name: terminated
  namespace: default
spec:
  parentRefs:
    - name: mixed-tls
      sectionName: terminate
  hostnames: [terminate.example.com]
  rules:
    - backendRefs:
        - name: plaintext-app
          port: 3000
---
apiVersion: gateway.networking.k8s.io/v1
kind: TLSRoute
metadata:
  name: encrypted
  namespace: default
spec:
  parentRefs:
    - name: mixed-tls
      sectionName: passthrough
  hostnames: [encrypted.example.com]
  rules:
    - backendRefs:
        - name: secure-app
          port: 8443
EOF
```

For mixed ports, HAPTIC inspects the ClientHello on a TCP frontend and forwards
terminating connections through a private Unix socket to a TCP TLS frontend.
PROXY protocol v2 preserves the client address across that local connection.
The Unix socket doesn't expose another network port. Ports with multiple
terminating listeners use the same isolation so their bind-level TLS options
remain separate. A single terminating listener uses a direct TLS bind.

Terminating TLSRoute listeners retain the `h2,http/1.1` defaults for
Application-Layer Protocol Negotiation (ALPN). Per-listener ALPN configuration
isn't supported.

Each connection through a private TLS frontend adds one local connection and
two file descriptors, plus its stream and buffer memory. It consumes two accepted
connection slots in HAProxy's global `maxconn` limit. Size that limit and the pod's
file-descriptor and memory budgets for direct connections plus twice the bridged
connections, with headroom for concurrent handshakes. At the global limit, a
public connection can wait for the private frontend to accept its local hop.
Passthrough connections don't take that extra hop. Removing a passthrough sibling
keeps the remaining terminating listener's certificate and client-validation policy.

To return to a HAPTIC version that rejects mixed TLS modes, first move the
listeners onto separate ports and verify their routes. The older version can't
serve a mixed-mode Gateway.

### TLSRoute status

Each `parentRef` targeting a Gateway owned by this controller receives two conditions:

- **Accepted**: `True`, or `False` with reason `NoMatchingParent` (`sectionName` or `port` matched no listener), `NoMatchingListenerHostname`, or `NotAllowedByListeners`
- **ResolvedRefs**: `True`, or `False` with reason `InvalidKind` (a `backendRef` isn't a core/v1 Service), `RefNotPermitted` (cross-namespace ref without a ReferenceGrant), or `BackendNotFound`

TLSRoutes count toward `attachedRoutes` on each TLS listener to which they attach. Status is written on the `deployed` outcome — `Accepted` turns `True` once HAProxy serves the route, not at render time.

### TLSRoute limitations

- `backendRefs` must be core/v1 Services.
- At least one `spec.hostnames` entry is required.

---

## TCPRoute support

TCPRoute forwards connections from a listener port to a rule's backend Services.
A port belongs to one rule; TCPRoute doesn't match hostnames or paths.

!!! note "TCPRoute needs Gateway API v1.6 standard channel (or the experimental channel)"
    TCPRoute is in the Gateway API standard channel (`standard-install.yaml`) since v1.6. On v1.5 and earlier, install it from the experimental channel (`experimental-install.yaml`). HAPTIC activates TCPRoute support automatically once the CRD is served — no chart redeploy. See [Supported Gateway API versions and channels](#supported-gateway-api-versions-and-channels).

### Example: TCP Gateway and TCPRoute

This Gateway opens a `TCP` listener on port 5432 and forwards every connection to a PostgreSQL Service:

```bash
kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: tcp-edge
  namespace: default
spec:
  gatewayClassName: haptic
  listeners:
    - name: postgres
      protocol: TCP
      port: 5432
      allowedRoutes:
        namespaces:
          from: Same
        kinds:
          - kind: TCPRoute
---
apiVersion: gateway.networking.k8s.io/v1
kind: TCPRoute
metadata:
  name: postgres
  namespace: default
spec:
  parentRefs:
    - name: tcp-edge
  rules:
    - backendRefs:
        - name: postgres
          port: 5432
EOF
```

The `postgres` Service must exist in `default` and expose port `5432`.
Choose a listener port other than `haproxy.ports.http` or `haproxy.ports.https`
(default `80` and `443`); HAPTIC ignores TCP listeners that collide with those ports.

A Gateway with only TCP listeners uses the shared HAProxy Service. HAPTIC adds
its listener ports to that Service. A Gateway with HTTP or HTTPS listeners also
gets a dedicated Service that includes its TCP ports.

### Attachment semantics

| Check | Behavior |
|-------|----------|
| `parentRefs[]` kind | Must reference a `Gateway` in group `gateway.networking.k8s.io` (both default when omitted) |
| `parentRefs[].sectionName` / `parentRefs[].port` | When set, restrict which listeners are considered |
| Listener protocol | Must be `TCP` |
| `allowedRoutes.kinds` | Honored; when omitted, the protocol default (TCPRoute on TCP listeners) applies |
| `allowedRoutes.namespaces.from` | `Same`, `All`, and `Selector` (with `matchLabels`; `matchExpressions` isn't supported) |
| Port ownership | Each listener port belongs to exactly one route rule. When several TCPRoutes claim the same port, the **oldest** route wins (`creationTimestamp`, then `namespace/name` as tie-breaker) |

### Forwarding behavior

- **One frontend per claimed port**: `frontend gateway-tcp-port-<port>` with `mode tcp`, `bind *:<port>`, and a `default_backend` — no ACLs.
- **Backends**: `mode tcp` blocks named `gtw_tcp_<namespace>_<route>_<ruleIndex>`. A single `backendRef` resolves to that Service's endpoint servers; multiple `backendRefs` use `balance roundrobin` with each Service's servers carrying its `weight` (default 1; a `weight: 0` ref stays in the config but takes no traffic).
- A route without `sectionName` attaches to every TCP listener on the Gateway: each port gets its own frontend, all sharing one backend.
- TCP listeners whose port equals the chart-static `httpPort` or `httpsPort` are dropped to avoid a duplicate bind.

### TCPRoute status

Each `parentRef` targeting a Gateway owned by this controller receives two conditions:

- **Accepted**: `True`, or `False` with reason `NoMatchingParent` (`sectionName` or `port` matched no listener) or `NotAllowedByListeners`
- **ResolvedRefs**: `True`, or `False` with reason `InvalidKind`, `RefNotPermitted`, or `BackendNotFound` (same semantics as TLSRoute)

TCPRoutes count toward `attachedRoutes` on TCP listeners only. Status is written on the `deployed` outcome, as for TLSRoute.

### TCPRoute limitations

For incoming and outgoing PROXY protocol, use a
[platform-owned TCP frontend](../operations/tcp-proxy-protocol.md). TCPRoute
listeners don't currently expose PROXY protocol options.

- **One backend per port**: TCP can't be multiplexed by hostname or path; a port maps to a single route rule, and competing claims resolve oldest-first.
- `backendRefs` must be core/v1 Services.
- Listener ports colliding with the chart-static `httpPort` / `httpsPort` are dropped.

---

## Cross-namespace routes (ReferenceGrant)

Cross-namespace routing has two independent gates, and both apply to every route kind (HTTPRoute, GRPCRoute, TLSRoute, TCPRoute):

- **Listener attachment** — a Gateway listener's `allowedRoutes.namespaces.from` decides which namespaces' routes may attach. HAPTIC honors `Same` (the default — routes in the Gateway's own namespace), `All` (routes in any namespace), and `Selector` (routes in namespaces matching `matchLabels`; `matchExpressions` isn't supported). Attaching a route to a Gateway in another namespace needs no ReferenceGrant — only a permissive `allowedRoutes`.
- **Backend references** — a rule's `backendRef.namespace` pointing at a Service in another namespace is permitted only by a ReferenceGrant in the **target** (Service) namespace whose `from` clause names the route's group, kind, and namespace and whose `to` clause names the Service group and kind. Without a matching grant, the route's `ResolvedRefs` condition turns `False` with reason `RefNotPermitted` and the backend isn't served.

The following resources attach a route in `store-a` to a Gateway in `infra`, and
allow it to reach the `shop` Service on port `80` in `store-b`. Create those
namespaces and the backend Service before applying this example:

```bash
kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: edge
  namespace: infra
spec:
  gatewayClassName: haptic
  listeners:
    - name: http
      protocol: HTTP
      port: 80
      allowedRoutes:
        namespaces:
          from: All
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: shop
  namespace: store-a
spec:
  parentRefs:
    - name: edge
      namespace: infra
  hostnames:
    - shop.example.com
  rules:
    - backendRefs:
        - name: shop
          namespace: store-b
          port: 80
---
apiVersion: gateway.networking.k8s.io/v1beta1
kind: ReferenceGrant
metadata:
  name: allow-store-a-httproutes
  namespace: store-b
spec:
  from:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
      namespace: store-a
  to:
    - group: ""
      kind: Service
      name: shop
EOF
```

The grant permits references only to the `shop` Service. Omitting `to[].name`
would permit references to every Service in `store-b`. To permit a different route kind, set `from[].kind` to `GRPCRoute`, `TLSRoute`, or `TCPRoute`. Cross-namespace Gateway certificate references (a listener's `tls.certificateRefs` pointing at a Secret in another namespace) follow the same rule with a `to` clause of `group: "", kind: Secret`.

---

## Debug headers

When debug headers are enabled, the gateway library adds response headers to help troubleshoot routing decisions:

```yaml
# values.yaml
controller:
  config:
    templatingSettings:
      extraContext:
        diagnostics:
          routingHeaders:
            enabled: true
```

**Response Headers:**

- `X-Gateway-Matched-Route` - The namespace/name of the matched HTTPRoute or GRPCRoute
- `X-Gateway-Match-Reason` - Additional information about why the route was selected (for example `method match`, `header match`)

`X-Gateway-Filters-Applied` identifies the first filter that ran. If an expected
filter is absent, check the matched route and its filter configuration.

## Per-Gateway Kubernetes Resources

HAPTIC creates Services for Gateways as well as the chart's shared HAProxy
Service. HTTP and HTTPS Gateways get separate listening addresses while using
the same HAProxy pods:

| Gateway configuration | Generated Service |
| --- | --- |
| HTTP or HTTPS listeners, without `spec.addresses` | A `LoadBalancer` Service named `gw-<gateway-namespace>-<gateway-name>` in the controller namespace. It maps public listener ports to dedicated pod ports that isolate the Gateway's routes. |
| `spec.addresses` with `IPAddress` entries | One `LoadBalancer` Service per requested IP, named `gw-<gateway-namespace>-<gateway-name>-<ip-with-dashes>` in the controller namespace. The MetalLB annotation requests that IP. HTTP and HTTPS ports use the same listener isolation as dynamically assigned addresses. |
| `spec.infrastructure` labels or annotations | A headless Service in the Gateway namespace with the propagated metadata. It has a placeholder port and no pod selector; it doesn't carry application traffic. |

Names over 63 characters are shortened with a hash suffix. For local HTTP or
HTTPS testing, [port-forward to the Gateway's Service](../gateway-api.md#step-4-test-the-routing)
so the connection reaches its isolated listener. Use a load-balancer
implementation in your cluster for external access.

### Request a static IP for a Gateway

Set `spec.addresses` to an IP from your MetalLB address pool. This example uses
`203.0.113.5` and an existing `platform` namespace; choose an address allocated to
your cluster before applying it:

```bash
kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: edge
  namespace: platform
spec:
  gatewayClassName: haptic
  addresses:
    - type: IPAddress
      value: 203.0.113.5
  listeners:
    - name: http
      protocol: HTTP
      port: 80
      allowedRoutes:
        namespaces:
          from: Same
EOF
```

HAPTIC emits a `LoadBalancer` Service named `gw-platform-edge-203-0-113-5` in the controller's namespace, annotated `metallb.universe.tf/loadBalancerIPs: 203.0.113.5` and selecting the shared HAProxy pods. Find it by its Gateway label:

```bash
kubectl get svc -n haptic -l gateway.networking.k8s.io/gateway-name=edge
```

Use MetalLB with an address pool containing the requested IPs. Once MetalLB allocates the IP, it appears in the Gateway's `status.addresses`. Listing several `spec.addresses[]` entries emits one Service per IP; an IP that can't be allocated is left out of `status.addresses` while the usable ones still bind.

<a id="features-summary"></a>

## Status reporting

The Gateway library reports processing results in the status of GatewayClass,
Gateway, ListenerSet, HTTPRoute, GRPCRoute, TLSRoute, TCPRoute, and BackendTLSPolicy
resources.
TLSRoute and TCPRoute conditions are described in their sections above.

### Gateway status

Each Gateway receives:

- **Conditions**: `Accepted` reports whether HAPTIC can handle the Gateway. `Programmed` reports whether its configuration has reached HAProxy; invalid listeners or unusable requested addresses can keep it false.
- **Addresses**: Addresses from the Gateway's dedicated Service, or the shared HAProxy Service for Gateways without HTTP or HTTPS listeners. Explicit `spec.addresses` reports only requested IPs that have been allocated.
- **Listener status**: Per-listener conditions (`Accepted`, `Programmed`, `ResolvedRefs`, `Conflicted`), `supportedKinds` based on protocol, and `attachedRoutes` count

### HTTPRoute and GRPCRoute status

Each route receives a `parents[]` entry for each `parentRef` that matches a Gateway managed by this controller:

- **Accepted**: Whether the route can attach to the selected parent listener, including hostname, route-kind, and namespace checks
- **ResolvedRefs**: Whether backend and other references resolve and are permitted; the reason identifies missing resources, unsupported kinds, or missing cross-namespace permission

The `controllerName` in route status is set from `gatewayClass.controllerName` in the Helm values — see [GatewayClass](../gateway-class.md) for the class configuration and ownership rules.

### Address discovery

HAPTIC watches the generated Services and updates Gateway addresses when the
load balancer assigns them. An HTTP or HTTPS Gateway whose dedicated Service
has no external address reports an empty address list. It doesn't borrow the
shared HAProxy Service's address, because that would reach a different listener.

### Phase-aware status

HAPTIC updates status as each stage finishes:

| Phase | Gateway | Routes |
|-------|---------|--------|
| Rendered | Acceptance, reference checks, listener details, and discovered addresses | Parent attachment and reference checks |
| Deployed | `Programmed` reflects the deployed listener configuration | Attachment and reference results remain independent of deployment |
| Deployment failed | `Programmed=False` | Attachment and reference results remain independent of deployment |

TLSRoute and TCPRoute status is written on the `deployed` outcome only (see their sections above).

---

## Known limitations

**Not implemented:**

1. **Other ExtensionRef kinds** — use `HAProxyRoutePolicy` for supported route policies or the HTTPRoute `SSLPassthrough` extension. Arbitrary custom filter kinds are unsupported.
2. **Per-backend `RequestMirror`** — `RequestHeaderModifier`, `ResponseHeaderModifier`, `RequestRedirect`, and `URLRewrite` on a `backendRef` **are** honored, keyed by rule id and backend; a rule-level `RequestRedirect` or `URLRewrite` takes precedence over a backend-level one. `RequestMirror` applies at the rule level only.

**Reloads even though the filter itself is map-driven:**

- The first route in the cluster to name a given header in a `RequestHeaderModifier`
  or `ResponseHeaderModifier`. Every later route using that header name is a map entry.
- The first route in the cluster to carry a `RequestRedirect`, `URLRewrite` or
  `RequestMirror` filter, and removing the last one: the filter's rule block
  exists only while a route uses it. The same holds for the route-id and
  misdirected-request blocks with the first route and the first Gateway.
- The first route in the cluster to carry an `ExternalAuth` filter, and removing the
  last one. A header name that no other route lists in `allowedHeaders` or
  `allowedResponseHeaders` also reloads once.
- A rule whose `matches` carry several different path prefixes, combined with
  `ReplacePrefixMatch`: one map value carries one prefix length, so that rule keeps a
  configuration line per match.
- A rule whose `RequestMirror` filters sample at different percentages: one map value
  carries one percentage, so that rule keeps a line per mirror target.
- Advanced matchers (method, header, query parameter, gRPC method), the `CORS` filter,
  and `RegularExpression` path rewrites, which stay structural — see the zero-reload
  table in [Supported configuration](../supported-configuration.md).

TLSRoute- and TCPRoute-specific limitations are listed in their sections above. If one of these gaps matters to you, [open an issue](https://github.com/haproxy-haptic/HAPTIC/issues).

---

## Access-log fields

The library contributes `gw_route` to the [structured access log](../operations/access-logging.md)
when at least one Gateway exists. It carries `<namespace>_<name>_<ruleIndex>` of
the HTTPRoute rule that won, which answers "which rule of this route matched?" —
the core `resource` field already names the route itself.

<a id="extension-points"></a>
<a id="injecting-custom-configuration"></a>

## Extend Gateway routing

Use [HAProxyRoutePolicy](../operations/gateway-policies.md) for authentication,
rate limits, WAF inspection, and caching. For behavior beyond the bundled
settings, add [custom templates](../templating.md) through the
[base library's extension points](base.md#extension-points).

## See also

- [Gateway API Documentation](https://gateway-api.sigs.k8s.io/)
- [GatewayClass](../gateway-class.md) - Configuring the class this controller owns (`gatewayClass.name`, `gatewayClass.controllerName`)
- [Template Reference](../template-reference.md) - Template context, typed resource access, and functions the library's snippets build on
- [Template Libraries Overview](../template-libraries.md) - How template libraries work
- [Base Library](base.md) - Extension points and routing infrastructure
- [SSL Library](ssl.md) - TLS certificate management
- [haproxytech library](haproxytech.md) - Annotation-based configuration
