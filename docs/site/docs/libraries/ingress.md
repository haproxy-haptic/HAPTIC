# Route with Ingress

<a id="ingress-library"></a>

Route requests by hostname and path using Kubernetes Ingress resources. The
library is enabled by default and watches Ingresses with `ingressClassName: haptic`.
HAPTIC discovers backend endpoints from Services and EndpointSlices.

With the chart defaults, every Ingress also serves HTTPS with the default certificate. Add `spec.tls`
for your own host certificates, or see [TLS configuration](#tls-configuration)
for HTTP-only routing. To change authentication, timeouts, or other route
behavior, use [Ingress annotations](../annotations.md).

<a id="overview"></a>

Try adding a hostname to the sample Ingress:

<div class="pg-embed" markdown data-scenario="ingress" data-facade="spec.templateSnippets.map-host-500-ingress" data-tab="haproxy.cfg" data-controls="tabs,resources" data-input="resources" data-input-focus="shop.example.com" data-title="Ingress → HAProxy config" data-height="440">

<p class="pg-task" markdown>In the **Resources** panel, add a second host to the `shop` Ingress — copy its existing rule and change the host to `www.shop.example.com`. Then open the **maps** tab and watch `www.shop.example.com` join `host.map` and `path-prefix.map`, both routing to the existing `storefront_shop_svc_shop_http` backend.</p>

<details class="pg-hint" markdown>
<summary>What to expect</summary>

The new hostname appears in `host.map` and `path-prefix.map`. Both hosts point
to `storefront_shop_svc_shop_http`, so the **haproxy.cfg** output still has one
backend for the shop Service.

</details>

</div>

## Configuration

```yaml
controller:
  templateLibraries:
    ingress:
      enabled: true  # Enabled by default
```

### Ingress class filtering

The chart creates IngressClass `haptic` and selects Ingresses that name it.
See [class selection](../ingress-class.md) for a different name or filter. When
replacing another controller, follow the [migration guide](../migrating.md) to
transfer class ownership and traffic.

## Routing rules {#features}

### Host-based routing

Each entry in `spec.rules` carries its own `host`, so a single Ingress can serve several hostnames — list one rule per host:

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: multi-host
  namespace: default
spec:
  ingressClassName: haptic
  rules:
    - host: shop.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: storefront
                port:
                  number: 80
    - host: admin.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: admin-console
                port:
                  number: 80
```

A rule with no `host` matches every hostname (the catch-all listener).

#### Wildcard hosts

A wildcard matches exactly one hostname label. For example, `*.example.com`
matches `shop.example.com` and `admin.example.com`, but not `example.com` or
`a.b.example.com`. For other patterns, use the native
[`haproxy-haptic.org/host-alias-regex`](haptic-annotations.md#path-and-host-matching)
annotation.

```yaml
spec:
  ingressClassName: haptic
  rules:
    - host: "*.example.com"
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: wildcard-service
                port:
                  number: 80
```

### Path types

The Ingress library supports all standard Kubernetes Ingress path types:

| Path type | Matching behavior |
| --- | --- |
| `Exact` | `/shop` matches `/shop`, but not `/shop/` or `/shop/cart` |
| `Prefix` | `/shop` matches `/shop`, `/shop/`, and `/shop/cart`, but not `/shopping` |
| `ImplementationSpecific` | Uses prefix matching unless a [path-type annotation](haptic-annotations.md#path-and-host-matching) selects another behavior |

!!! note "Path match precedence"
    When more than one path could match a request, HAProxy evaluates the path maps in a fixed order: Exact, then Regex, then Prefix-exact, then Prefix. Host matching runs first (exact host, then single-label wildcard, then host regex). Set `controller.config.templatingSettings.extraContext.routing.regexMatchOrder=last` to move regex evaluation after the prefix matchers (Exact > Prefix-exact > Prefix > Regex). See [Frontend routing logic](base.md#frontend-routing-logic) for the complete cascade.

**Example Ingress:**

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: my-app
  namespace: default
spec:
  ingressClassName: haptic
  rules:
    - host: app.example.com
      http:
        paths:
          - path: /api
            pathType: Prefix
            backend:
              service:
                name: api-service
                port:
                  number: 80
          - path: /health
            pathType: Exact
            backend:
              service:
                name: health-service
                port:
                  number: 8080
```

Watch the three path-type maps populate as you add Exact and Prefix paths:

<div class="pg-embed" markdown data-scenario="ingress" data-facade="spec.templateSnippets.map-path-exact-500-ingress" data-tab="maps" data-controls="tabs,resources" data-title="Path types → map entries" data-height="440">

<p class="pg-task" markdown>In the **Resources** panel, add two paths to the `shop` Ingress rule (alongside the existing `/`): a `/api` path with `pathType: Prefix` and a `/health` path with `pathType: Exact`, both pointing at the `shop` service on port `80`. Then open the **maps** tab and watch each path land in a different map.</p>

<details class="pg-hint" markdown>
<summary>What to expect</summary>

- `map-path-exact-500-ingress` adds `shop.example.com/health BACKEND:storefront_shop_svc_shop_http` to `path-exact.map` — the `Exact` path lowers to a `map()` lookup.
- `map-path-prefix-500-ingress` adds `shop.example.com/api/ BACKEND:storefront_shop_svc_shop_http` to `path-prefix.map` — the `Prefix` path lowers to a `map_beg()` lookup.
- `map-pfxexact-500-ingress` also adds `shop.example.com/api BACKEND:storefront_shop_svc_shop_http` to `path-prefix-exact.map` — the exact-boundary entry so a request to exactly `/api` (no trailing slash) still matches the Prefix rule. A root `/` Prefix path emits no boundary entry, which is why the original `/` path isn't in this map.

All three route to the same `storefront_shop_svc_shop_http` backend: they share one Service and port, so `backends-500-ingress` emits a single backend.

</details>

</div>

### Conflicting routes: the oldest Ingress wins

A host and path can be routed to only one backend. When two Ingresses declare the same host, path, and path type, the controller resolves the collision deterministically: the **older** Ingress — by `creationTimestamp`, with the namespace and name as a tiebreaker — keeps the route, and the newer Ingress's conflicting route is dropped.

`Prefix` and `ImplementationSpecific` paths share the same routing slot, so a `Prefix` path on one Ingress and an `ImplementationSpecific` path with the same host and path on another still collide and are resolved together. `Exact` paths match separately and never collide with prefix paths.

The comparison uses object creation time, not the time a path was added. An
older Ingress can therefore take precedence when edited to add a conflicting path.

The controller records a `Warning` Event with reason `RouteConflict` on the Ingress that lost the route, naming the winner, so the dropped route is visible without reading the controller logs:

```console
$ kubectl describe ingress route-new -n team-b
...
Events:
  Type     Reason         Age   From              Message
  ----     ------         ----  ----              -------
  Warning  RouteConflict  10s   haptic-controller  host "shop.example.com" path "/checkout" (Prefix) is already served by Ingress team-a/route-old, which takes precedence; this Ingress's route is not applied
```

Different paths on the same host don't collide, so you can split a host across several Ingresses by giving each a distinct path. An [nginx canary](nginx-ingress.md) Ingress (`nginx.ingress.kubernetes.io/canary: "true"`) intentionally shares its main Ingress's host and path; it never competes for the base route — the main Ingress owns it, and the canary only overlays a traffic split on top.

!!! warning "Avoid overlapping Ingress and Gateway routes"
    Oldest-wins conflict handling applies only between Ingresses. An Ingress and
    a Gateway route claiming the same host and path can produce duplicate map
    entries with an order-dependent result. Give them distinct host/path pairs.

### Default backend and custom error pages

Set `spec.defaultBackend` to route requests that match none of an Ingress's rule paths to a fallback Service:

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: catch-all
  namespace: default
spec:
  ingressClassName: haptic
  defaultBackend:
    service:
      name: fallback-service
      port:
        number: 80
  rules:
    - host: app.example.com
      http:
        paths:
          - path: /api
            pathType: Prefix
            backend:
              service:
                name: api-service
                port:
                  number: 80
```

HAPTIC honours `spec.defaultBackend` in three shapes:

- **Rule-less Ingress** — with only `spec.defaultBackend` and no `rules`, every request that doesn't match a more specific route goes to the default backend.
- **Alongside rules** — a request that matches the Ingress's host but none of its paths falls through to the default backend; requests to other hosts aren't caught.
- **Newest wins per host** — when several Ingresses declare a default backend for the same host, the most recently created one wins, so a rollout switches the fallback deterministically.

To serve a custom page for unmatched requests — a branded 404 or a maintenance notice — point `spec.defaultBackend` at a small Service that returns it. For HAProxy's own error responses (for example the 503 shown when a backend has no ready endpoints), render the page as a file and wire it with an `errorfile` directive instead; see [Auxiliary files](../template-files.md#general-files) for the `files` and `errorfile` pattern.

### TLS Configuration

TLS certificates are automatically loaded from Kubernetes Secrets and registered with the SSL library:

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: secure-app
  namespace: default
spec:
  ingressClassName: haptic
  tls:
    - hosts:
        - secure.example.com
      secretName: tls-secret
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
                  number: 80
```

Create the `kubernetes.io/tls` Secret in the same namespace as the Ingress.
With your certificate chain in `tls.crt` and private key in `tls.key`:

```bash
kubectl create secret tls tls-secret --namespace default --cert=tls.crt --key=tls.key
```

This configures TLS from clients to HAProxy. For TLS from HAProxy to the
application, also configure [backend TLS](haptic-annotations.md#backend-tls-to-the-upstream).

!!! warning "Check the certificate served for your hostname"
    If a TLS Secret is missing or lacks `tls.crt` or `tls.key`, HAPTIC skips it
    and serves the default certificate. This doesn't fail the render or emit an
    Event. Check the Secret with `kubectl get -n <namespace> secret <secretName>`
    and the served certificate with
    `openssl s_client -connect <host>:443 -servername <host>`.

#### HTTPS on by default

Every Ingress is served over **both HTTP and HTTPS** out of the box, even without a `spec.tls` entry. HAPTIC binds the chart's https port (`haproxy.ports.https`, default `443`) and terminates TLS with the [default certificate](../ssl-certificates.md) — a self-signed cert out of the box — routing HTTPS requests through the same host and path rules as HTTP. A `spec.tls` entry layers a host-specific certificate on top: that host is served with its own certificate instead of the default.

Two `extraContext` settings control the default HTTPS bind:

| Key | Default | Effect |
|-----|---------|--------|
| `ingressDefaultHTTPS` | `true` | Bind the https port for every Ingress using the default certificate. Set `false` to serve Ingress over plain HTTP only until a host opts in with `spec.tls`. |
| `tls.defaultCertificate.name` / `tls.defaultCertificate.namespace` | chart-set | The Secret backing the default certificate. When the default certificate is disabled (`defaultSSLCertificate.enabled=false`), there is no cert to bind, so the https port stays closed regardless of `ingressDefaultHTTPS`. |

To serve Ingress over plain HTTP only, disable the default bind through the chart:

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        ingressDefaultHTTPS: false
```

#### Redirect HTTP to HTTPS

To send all Ingress traffic to HTTPS, turn on the global redirect. HAPTIC then emits an `http-request redirect scheme https` rule for every Ingress host that's served over HTTPS (the default-HTTPS bind is on, or the host has its own `spec.tls`), so it never redirects to a closed port:

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        ingressDefaultSSLRedirect: true
```

| Key | Default | Effect |
|-----|---------|--------|
| `ingressDefaultSSLRedirect` | `false` | Redirect every HTTPS-served Ingress host from HTTP to HTTPS. Opt-in. |
| `ingressDefaultSSLRedirectCode` | `"308"` | HTTP status code for the redirect — one of `301`, `302`, `303`, `307`, `308`. |

The global toggle redirects all HTTPS-served Ingress hosts at once. For per-host control, leave it off and use the native [`haproxy-haptic.org/https-redirect`](haptic-annotations.md#redirects-hsts-passthrough-and-config-injection)
annotation on selected Ingresses.

### Backend generation

Backends are generated with:

- Automatic endpoint discovery via EndpointSlices
- TCP-connect health checks (`default-server check`) — the Ingress path isn't used as an HTTP health-check URI
- Round-robin load balancing
- Backend deduplication (multiple paths to same service share one backend)

Not-ready and terminating endpoints are disabled. With no usable endpoints,
HAProxy returns `503`; check the Service and EndpointSlices before changing the
routing template.

#### Backend namespace scope

An Ingress backend references a Service by name only — the Kubernetes API has no per-backend namespace field. HAPTIC therefore always resolves the Service, its EndpointSlices, and any `spec.tls` Secret in the Ingress's own namespace. A Service that doesn't exist in that namespace renders as an empty backend that serves 503 (and, for a port referenced by name, raises the `BackendUnresolved` Warning Event — see [Degraded backend events](#degraded-backend-events)). To route to a Service in a different namespace, use a Gateway API HTTPRoute with a `backendRef.namespace` and a matching ReferenceGrant — see [Cross-namespace routes](gateway.md#cross-namespace-routes-referencegrant); Ingress can't express a cross-namespace backend.

### WebSocket backends

WebSocket backends work without extra configuration. HAProxy tunnels the `Upgrade` handshake, so an Ingress routing to a WebSocket service needs no special annotation. Long-lived connections are bounded by HAProxy's `timeout tunnel`. Raise it per-backend with `haproxy-haptic.org/timeout-tunnel` when a connection must stay open longer:

```yaml
metadata:
  annotations:
    haproxy-haptic.org/timeout-tunnel: "1h"
```

### gRPC backends

gRPC runs over HTTP/2. Tell HAPTIC to speak HTTP/2 to a cleartext (h2c) backend with the native `haproxy-haptic.org/backend-protocol: grpc` annotation:

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: grpc-app
  namespace: default
  annotations:
    haproxy-haptic.org/backend-protocol: grpc
spec:
  ingressClassName: haptic
  rules:
    - host: grpc.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: grpc-service
                port:
                  number: 50051
```

For a TLS backend, use `haproxy-haptic.org/backend-protocol: grpcs` and configure
[backend certificate verification](haptic-annotations.md#backend-tls-to-the-upstream).
Use these gRPC-specific values so HAPTIC can also detect incompatible
[body-inspection policies](../operations/waf-policies.md#waf-and-grpc-streaming).
Clients can connect through HTTPS or use HTTP/2 cleartext prior knowledge on
the HTTP listener.

### Backend config snippet

Use `haproxy-haptic.org/config-backend` for trusted, operator-authored backend
directives. HAPTIC rejects it unless raw configuration annotations are enabled; see
[Raw configuration annotations](../operations/security.md#raw-configuration-annotations) and the [native annotation reference](haptic-annotations.md#backend-tuning).
The equivalent `haproxy.org/backend-config-snippet` requires the opt-in
[haproxytech library](haproxytech.md).

## Status reporting

The Ingress library automatically propagates LoadBalancer addresses to Ingress `.status.loadBalancer` fields. This enables DNS controllers (like external-dns) and `kubectl get ingress` to display the correct external address.

Addresses are discovered from the HAProxy LoadBalancer Service. Once an address is available, each Ingress processed by the controller receives its `status.loadBalancer.ingress` entries. If deployment fails, the status is cleared to empty.

### Degraded backend events

A backend whose named Service port can't be resolved has no usable servers and
returns `503`. Check the `BackendUnresolved` Event for the Service and port name:

```bash
kubectl describe ingress <name>
kubectl get events --field-selector reason=BackendUnresolved -A
```

If the Service hasn't arrived yet, HAPTIC can resolve the named port from an
EndpointSlice. The warning disappears once that lookup succeeds. If the Service
exists but doesn't declare the requested port name, the render fails with the
available port names; correct the Ingress reference.

Numeric port references don't produce `BackendUnresolved` Events. For those,
check endpoints and the [routing troubleshooting guide](../troubleshooting.md#routing-issues).
Gateway API routes report reference problems in their route conditions.

<a id="watched-resources"></a>
<a id="field-selector"></a>
<a id="generated-map-files"></a>

For custom watches and routing maps, see [watching resources](../watching-resources.md)
and the [base extension points](base.md#extension-points).

<a id="extension-points"></a>
<a id="injecting-custom-configuration"></a>

## Extend Ingress routing

Start with [native annotations](haptic-annotations.md) for common routing
settings. To add an annotation or routing rule of your own, follow the
[template customization guide](../templating.md) and
[extension-point reference](base.md#extension-points).

## See also

- [Template Libraries Overview](../template-libraries.md) - How template libraries work
- [Base Library](base.md) - Extension points and routing infrastructure
- [SSL Library](ssl.md) - TLS certificate management
- [Native annotations](haptic-annotations.md) - Authentication, timeouts, redirects, and other route settings
- [Migration libraries](../migrating.md) - Annotation compatibility with other controllers
