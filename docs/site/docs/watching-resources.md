# Watching resources

Make a Kubernetes resource available to your templates by adding it to
`watchedResources`. The bundled libraries already watch the types they need;
add a watch when your templates need another type, such as your own custom resource.

With Helm, put watches under `controller.config.watchedResources`. In an
`HAProxyTemplateConfig`, use `spec.watchedResources`. The examples below show
individual entries; the [CRD reference](./crd-reference.md#watchedresources)
lists every field and default.

The example below loads an Ingress watch and sample resources.

<div class="pg-embed" markdown data-scenario="ingress" data-facade="spec.watchedResources" data-tab="haproxy.cfg" data-controls="tabs,resources" data-input="resources" data-input-focus="name: shop" data-title="Read an Ingress from a resource watch" data-height="440">

<p class="pg-task">Change the sample Ingress name, then find the renamed backend in the generated configuration.</p>

</div>

With a schema, read resource fields directly, such as `ing.spec.rules`. The
controller loads schemas from the Kubernetes API server; offline tools use a
schema directory. For untyped maps or resources without a schema, use `dig()` and
supply missing-value defaults with `fallback()`. See [typed access](#typed-access-in-templates)
and [safe iteration](./template-resources.md#safe-iteration) for examples.

## Anatomy of an entry

```yaml
watchedResources:
  ingresses:
    apiVersion: networking.k8s.io/v1
    resources: ingresses
    indexBy:
      - metadata.namespace
      - metadata.name
    labelSelector: "app=shop"
```

The key `ingresses` makes the selected objects available as `resources.ingresses`
in templates. `apiVersion` and `resources` identify the Kubernetes type;
`indexBy` defines the keys you can use to look up one object. This example only
includes objects labeled `app=shop`. Omit `labelSelector` to watch all objects
of the selected type.

`labelSelector` accepts an equality-based selector string, such as `"app=shop"`; it doesn't accept a `matchLabels`/`matchExpressions` object. `fieldSelector` uses JSONPath equality syntax. See [Narrowing the watch](#narrowing-the-watch).

There is no fixed limit on watched resource types. Each entry consumes an API
watch stream, and `store: full` keeps its objects in memory. Size the controller
and narrow watches to the resources your templates need; see [watch optimization](./operations/performance.md#resource-watching-optimization).

## Two store types

Every entry uses one of two store backends. The choice controls memory footprint and rendering latency.

### Keep resources in memory {#store-full-default-memorystore}

Use `store: full` (the default):

- Keeps the full resource object in-process after trimming fields listed in `watchedResourcesIgnoreFields`.
- `.List()`, `.Fetch(...)`, `.GetSingle(...)` all resolve from memory with no API hit.
- Right for anything the templates iterate over (Ingresses, Services, EndpointSlices, small ConfigMaps).

### Fetch resources when needed {#store-on-demand-cachedstore}

Use `store: on-demand`:

- Stores only the index keys in-process; fetches the full object lazily on `.Fetch()` / `.GetSingle()` and caches the result.
- Right for large, rarely touched resources — TLS Secrets with 20 kB certificate bodies, ConfigMaps used only for a handful of entries.
- `.List()` on a cached store forces a fetch for every reference; avoid it.

Use a mix: `store: full` for everything the templates iterate, `store: on-demand` for Secrets holding certificates or auth data.

## Typed access in templates

With a schema available, read fields using their Kubernetes JSON names:

```go
{% for _, ingress := range resources.ingresses.List() %}
# {{ ingress.metadata.namespace }}/{{ ingress.metadata.name }}
{% end %}
```

The compiler catches misspelled field names when it loads the configuration.
The controller reads schemas from your API server; offline validation needs a
[schema directory](validation-tests.md#prepare-schemas). See
[reading resources](template-resources.md) for lookups, optional fields, and
resources without a schema.

## Indexing (`indexBy`)

The `.Fetch()` template method takes the index keys in the order you listed them. With:

```yaml
indexBy: ["metadata.namespace", "metadata.name"]
```

these are all valid:

```scriggo
{% for _, ing := range resources.ingresses.Fetch("default", "my-app") %}  {# exact match #}
{% for _, ing := range resources.ingresses.Fetch("default") %}           {# prefix: all in namespace #}
{% for _, ing := range resources.ingresses.List() %}                     {# everything #}
```

Supply fewer keys than `indexBy` defines to get a prefix scan — useful for one-to-many relationships. Pass at least one key and no more than `indexBy` defines; an invalid count rejects the render. Each argument remains one component, so `/`, empty strings, and Unicode text don't create extra components. A miss produces an empty slice. When an indexed value changes, HAPTIC moves the object to the new key; if the updated object no longer has every indexed field, HAPTIC removes it until all indexed fields exist again.

Run the prefix scan below: three Ingresses across two namespaces, but `Fetch("shop")` returns only the two in `shop`.

<div class="pg-embed" markdown data-tab="haproxy.cfg" data-controls="tabs,resources" data-focus="25" data-title="Prefix scan: Fetch one key of a two-key index" data-height="460">

```yaml
apiVersion: haproxy-haptic.org/v1alpha1
kind: HAProxyTemplateConfig
metadata:
  name: indexby-demo
spec:
  watchedResources:
    ingresses:
      apiVersion: networking.k8s.io/v1
      resources: ingresses
      indexBy:
        - metadata.namespace
        - metadata.name
  haproxyConfig:
    template: |
      global
        log stdout format raw local0
      defaults
        mode http
        timeout connect 5s
        timeout client 30s
        timeout server 30s
      frontend http
        bind :80
        default_backend unmatched
      {%- for _, ing := range resources.ingresses.Fetch("shop") %}
      backend {{ ing.metadata.name }}
        server app {{ ing.metadata.name }}.svc:80
      {%- end %}
      backend unmatched
        http-request deny deny_status 404
```

```yaml
apiVersion: v1
kind: List
items:
  - apiVersion: networking.k8s.io/v1
    kind: Ingress
    metadata:
      name: web
      namespace: shop
  - apiVersion: networking.k8s.io/v1
    kind: Ingress
    metadata:
      name: api
      namespace: shop
  - apiVersion: networking.k8s.io/v1
    kind: Ingress
    metadata:
      name: blog
      namespace: content
```

</div>

### Canonical index shapes

| Resource | `indexBy` | Why |
|----------|-----------|-----|
| Ingress / Service / ConfigMap | `["metadata.namespace", "metadata.name"]` | Standard unique lookup |
| EndpointSlice | `["metadata.namespace", "metadata.labels.kubernetes\\.io/service-name"]` | One-to-many lookup scoped to the Service's namespace |
| Secret (when sharded by type) | `["metadata.namespace", "type"]` | Group TLS vs. basic-auth vs. opaque |
| Cluster-scoped resource (Namespace, GatewayClass) | `["metadata.name"]` | No namespace to index by |

Escape dots in JSONPath keys that contain them (`labels.kubernetes\\.io/service-name`), otherwise the path parser reads the dot as a subfield separator.

### `.GetSingle()` vs `.Fetch()`

- `GetSingle(...)` returns a single object or `nil` — use when the index is unique and you want nil-safe access.
- `Fetch(...)` always returns a slice — use in `for` loops and when the index may match multiple resources.
- `List()` returns everything in the store — avoid on `on-demand` stores (fetches everything).

`GetSingle(...)` fails the render if its key matches multiple objects. Kubernetes
read errors and schema-to-typed-value conversion errors also fail the render;
they never turn into an empty resource set. A missing object remains `nil` or an
empty slice.

## Narrowing the watch

Three settings narrow what lands in the store:

- `labelSelector:` — equality-only label-selector string applied to the resource itself (`"app=myapp"` or `"app=nginx,env=prod"`). Set-based syntax (`"tier in (frontend,api)"`, `"!disabled"`) and malformed selectors fail configuration validation. Use comma-separated equality pairs.
- `fieldSelector:` — a client-side JSONPath equality filter applied *after* the list is fetched (format `"field.path=value"`, for example `"spec.ingressClassName=haproxy"`). Unlike Kubernetes' native field selectors it can target **any** field, not just the server-supported ones, because the watcher evaluates it itself (at the cost of fetching the full list first). A resource that stops matching is handled as a delete; one that starts matching, as an add. This is what the bundled ingress / gateway libraries use to scope by `ingressClassName` / `gatewayClassName`.
- `namespaces:` — a list of namespaces the controller watches the resource in, one watch per namespace. It needs `indexBy` to start with `metadata.namespace`. The Helm chart grants the resource with a Role in each listed namespace instead of the ClusterRole, so unlike the two filters above it also narrows what the ServiceAccount can read. A `GetSingle` or `Fetch` keyed on another namespace fails the render instead of returning nothing.

Watch the filter in action: two Ingresses reach the playground, but only the `haptic`-class one survives the `fieldSelector` and reaches a backend.

<div class="pg-embed" markdown data-tab="haproxy.cfg" data-controls="tabs,resources" data-focus="10" data-title="fieldSelector scopes the watch by ingress class" data-height="460">

```yaml
apiVersion: haproxy-haptic.org/v1alpha1
kind: HAProxyTemplateConfig
metadata:
  name: fieldselector-demo
spec:
  watchedResources:
    ingresses:
      apiVersion: networking.k8s.io/v1
      resources: ingresses
      fieldSelector: "spec.ingressClassName=haptic"
      indexBy:
        - metadata.namespace
        - metadata.name
  haproxyConfig:
    template: |
      global
        log stdout format raw local0
      defaults
        mode http
        timeout connect 5s
        timeout client 30s
        timeout server 30s
      frontend http
        bind :80
        default_backend unmatched
      {%- for _, ing := range resources.ingresses.List() %}
      backend {{ ing.metadata.name }}
        server app {{ ing.metadata.name }}.svc:80
      {%- end %}
      backend unmatched
        http-request deny deny_status 404
```

```yaml
apiVersion: v1
kind: List
items:
  - apiVersion: networking.k8s.io/v1
    kind: Ingress
    metadata:
      name: shop
      namespace: default
    spec:
      ingressClassName: haptic
  - apiVersion: networking.k8s.io/v1
    kind: Ingress
    metadata:
      name: legacy
      namespace: default
    spec:
      ingressClassName: nginx
```

</div>

Need to scope by namespace *labels* rather than a single name? Watch the `namespaces` resource and gate inside the template, or run separate controller instances per scope.

## Trimming fields

`spec.watchedResourcesIgnoreFields` drops noisy subtrees before they're indexed, cutting memory use. Reasonable defaults:

```yaml
watchedResourcesIgnoreFields:
  - metadata.managedFields
  - metadata.annotations['kubectl.kubernetes.io/last-applied-configuration']
```

Applies uniformly to every watched-resource store. Fields that are referenced by `indexBy` must not be trimmed.

A single resource adds its own list with `ignoreFields`; `[*]` selects every element of an array. Ignored fields are unavailable to templates, so only remove fields your templates don't read.

With `store: full`, updates that change only ignored fields and `metadata.resourceVersion` don't trigger reconciliation, even with `debounceInterval: "0"`. Changes to retained fields still trigger reconciliation, even if the rendered HAProxy configuration stays identical. On-demand stores retain resource versions to invalidate cached reads.

The bundled Gateway API library ignores the per-listener `attachedRoutes` counter on Gateways and ListenerSets to prevent its own status writes from triggering reconciliation:

```yaml
watchedResources:
  gateways:
    apiVersion: gateway.networking.k8s.io/v1
    resources: gateways
    indexBy: [metadata.namespace, metadata.name]
    ignoreFields:
      - status.listeners[*].attachedRoutes
```

### Database operator annotations

Database operators such as Patroni update coordination annotations without changing backend addresses. If your EndpointSlices carry changing `renewTime`, `optime`, or `slots` annotations that your templates don't read, add them to the `endpoints` watch's `ignoreFields` in your Helm values:

```yaml
controller:
  config:
    watchedResources:
      endpoints:
        ignoreFields:
          - metadata.annotations.renewTime
          - metadata.annotations.optime
          - metadata.annotations.slots
```

Keep any existing entries for this watch: Helm replaces lists. These entries add to the global `watchedResourcesIgnoreFields` list and leave endpoint address, port, and readiness changes observable. The chart preserves these annotations by default because custom templates may use them.

## HTTP Resources

Use `http.Fetch(url, options, authentication)` to read a response body into a
template. It returns two values: the response body and an error. Assign both
values and check the error before using the body; `fail(tostring(fetchErr))`
stops rendering when the error isn't `nil`. For example, you can maintain an IP
blocklist separately from your routing resources.

Deploying renders don't fetch a new URL's first response. Until that content passes
`haproxy -c` together with the rest of the configuration, deploying renders
leave it out: a non-critical source deploys without it, and a `critical: true`
source fails the render, so nothing deploys until the content is accepted. The
next successful render includes accepted content. Set `interval` to refresh the
content; HAPTIC uses conditional requests and validates new content before it
replaces the old. Each controller replica fetches its own copy. Admission checks
don't replace the content used by the running configuration. During startup,
first renders and their validation checks run one at a time; waiting for HTTP
responses releases that slot so other renders can proceed.

If refreshed content fails validation before acceptance, HAPTIC keeps any previously
accepted content and records an `HTTPContentRejected` Warning Event on the
`HAProxyTemplateConfig`. The `haptic_http_content_rejected_total` counter increases
once per rejected URL version. Fix the fetched content or the template that uses
it; a repaired response can replace the saved content after validation.

If HAProxy refuses a configuration containing newly accepted content, HAPTIC
takes the content back and renders without it, or with the version it accepted
before. It records an `HTTPContentRevoked` Warning Event on the
`HAProxyTemplateConfig`. Content that still produces the refused configuration stays pending. Fix the
source content or the template that uses it.

If a `critical: true` source without accepted content can't be fetched, the
replica stops rendering instead of rendering the whole configuration again on
every change. It fetches the source again every `interval`, or every 30 seconds
without one, and renders as soon as a fetch succeeds. Changes that arrive in
the meantime are rendered once after each failed fetch, so a change that no
longer needs the source still deploys. `haptic_render_errors_total` counts the
failed renders.

### Fetch parameters

The second argument is an options map. All keys are optional:

| Key | Type | Default | Effect |
|-----|------|---------|--------|
| `interval` | Go duration string | none | Refresh interval after the first fetch. Omit it or set `"0"` to fetch once. |
| `timeout` | Go duration string | `30s` | Per-request timeout. |
| `retries` | integer | 2 | Retry attempts on a failed request, with a growing delay between attempts. |
| `critical` | boolean | `false` | Failure mode. With `false`, a failed fetch returns an empty string and rendering continues (a warning is logged). With `true`, a failed fetch aborts the render with an error, like [`fail()`](./template-reference.md#functions-and-filters). |
| `acceptStatus` | list of integers | none | Statuses besides `200` whose body is the content, for a server that answers with the status it describes, such as an error-page service serving its 404 page as a `404`. Each entry must be a status from 100 to 599 other than `304`, or the call fails regardless of `critical`. Any other status fails the fetch. |

Set `critical: true` only when an empty body would produce a dangerously wrong config (for example, a security blocklist that must not silently become empty); leave it `false` when a stale-or-empty body is safer than blocking every render on one unreachable URL.

A render can also return an empty string for content that hasn't failed: new content waiting for a render that can accept it. `http.Pending(url)` returns `true` in that case, and a later render returns the content. Check it before reporting a failed fetch:

```
{%- var page, fetchErr = http.Fetch(url) %}
{%- if fetchErr != nil %}
{{ fail(tostring(fetchErr)) }}
{%- end %}
{%- if page == "" && !http.Pending(url) %}
{# The fetch failed or returned an empty body. #}
{%- end %}
```

A third optional argument supplies authentication: `{"type": "bearer", "token": "..."}`, `{"type": "basic", "username": "...", "password": "..."}`, or `{"type": "header", "headers": {"X-API-Key": "..."}}`. Unknown authentication types fail the render before any request is sent, regardless of `critical`.

Use the same options and authentication for every call to a URL within one
render. Conflicting declarations fail the render. Changing a declaration on a
later render triggers a fresh request; failed validation prevents that response
from becoming the accepted input.

Response bodies are capped at 10 MiB. A larger response fails the fetch with `response body exceeds maximum size of N bytes` — it isn't truncated — and the limit is fixed with no per-call override. A failed fetch is then handled per the `critical` setting above.

### Example

This configuration fragment denies client IPs from a blocklist and refreshes it
every five minutes. Supply your blocklist URL and application backend before
using it. `critical: true` prevents a failed fetch from removing the blocklist:

```yaml
spec:
  haproxyConfig:
    template: |
      global
        log stdout format raw local0
      defaults
        mode http
        timeout connect 5s
        timeout client 30s
        timeout server 30s
      frontend web
        bind :80
        {%- var blocklist, fetchErr = http.Fetch("https://example.com/ip-blocklist.txt", map[string]any{"interval": "5m", "critical": true}) %}
        {%- if fetchErr != nil %}
        {{ fail(tostring(fetchErr)) }}
        {%- end %}
        {%- for _, ip := range split(tostring(blocklist), "\n") %}
        {%- if strip(ip) != "" %}
        http-request deny if { src {{ strip(ip) }} }
        {%- end %}
        {%- end %}
        default_backend app
      backend app
        server s1 10.0.0.1:8080 check
```

You can test this without contacting the URL: supply a mocked response in the
test's `httpResources` field, alongside `fixtures`. See
[HTTP response fixtures](validation-reference.md#test-structure). There is no
top-level `spec.httpResources` field.

## Validating webhook scope

Set `enableValidationWebhook: true` on a watch to validate creates and updates
before Kubernetes accepts them. HAPTIC renders the proposed change against the
current resources and rejects it if validation fails.

If several watch entries select the same Kubernetes resource type, HAPTIC applies
the proposed change to all of them during validation. It adds the object to
entries whose selectors it now matches and removes it from entries it no longer
matches. The map keys can differ from the Kubernetes resource's plural name.

The flag defaults to `false`. Enable it for types whose changes you need to
validate during admission. If any alias enables it, validation includes every
alias for that resource type.

## Debounce override

HAPTIC batches watched-resource changes for `100ms` by default. A render
already in progress can delay the next change. Reload pacing adds a separate
delay when HAProxy needs a reload; see [deployment pacing](operations/performance.md#deployment-pacing).

Set `debounceInterval` per watched resource when delayed updates are acceptable.
For example, coalesce updates to a custom ConfigMap watch for half a second:

```yaml
watchedResources:
  routingConfig:
    apiVersion: v1
    resources: configmaps
    indexBy: ["metadata.namespace", "metadata.name"]
    debounceInterval: "500ms"
```

Keep the bundled EndpointSlice watch at `"0"`: delaying endpoint updates can
leave HAProxy sending requests to pods that have stopped serving.

Use a Go duration such as `"500ms"`, `"10s"`, or `"1m30s"`. Set `"0"` to disable
watcher debouncing. Empty or invalid values use the `100ms` default without a
validation error, so check the spelling if the observed delay differs from your setting.

## Troubleshooting

| Symptom | Likely cause |
|---------|--------------|
| `.List()` returns empty | Check the watch's resource type and selectors; no matching objects are available to this template |
| `.Fetch(ns, name)` returns empty for a resource that exists | `indexBy` doesn't match what you passed, or `labelSelector` / `fieldSelector` is filtering it out |
| OOMKilled on controller | Check [resource sizing](operations/performance.md#controller-resource-sizing) first; then narrow unnecessary watches or use `store: on-demand` for large, rarely read objects |
| Template rendering slow, many API logs | You're calling `.List()` on an `on-demand` store, or `.Fetch()` consistently missing the cache — profile with `/debug/pprof/profile`, consider `store: full` if the total size is modest |
| `kubectl apply` rejected with `a HAProxyTemplateConfig needs podSelector, at least one watchedResources entry, and haproxyConfig …` | The CRD's validation rule requires `podSelector`, at least one `watchedResources` entry, and a `haproxyConfig` — inline or from a `spec.libraryRefs` entry; see [CRD Reference](./crd-reference.md) |

## See also

- [Bring-your-own-CRD example](https://github.com/haproxy-haptic/HAPTIC/tree/main/examples/byo-crd) — a runnable, self-validating example: watch a custom CRD, route on it, and write status back, with no Go
- [CRD Reference](./crd-reference.md#watchedresources) — field-level documentation
- [Templating Guide — The `resources` Variable](./template-resources.md#the-resources-variable) — `.List()` / `.Fetch()` / `.GetSingle()` semantics from the template side
- [Performance](./operations/performance.md) — deciding when to narrow the watch versus scale the controller
