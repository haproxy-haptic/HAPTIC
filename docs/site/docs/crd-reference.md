# HAProxyTemplateConfig CRD reference

<a id="overview"></a>

An `HAProxyTemplateConfig` defines the watched resources, templates, deployment
settings, and validation tests for one controller. Reference reusable content
through [`libraryRefs`](#libraryrefs) to [`HAProxyTemplateLibrary`](#haproxytemplatelibrary)
objects. The Helm chart creates these resources for you. Put persistent changes in
`controller.config` in your Helm values; use this reference to look up the fields
inside that configuration.

**API Group**: `haproxy-haptic.org`
**API Version**: `v1alpha1`
**Kind**: `HAProxyTemplateConfig`
**Short Names**: `htplcfg`, `haptpl`

Templates can read any configured resource type, including your own custom resources.

[Try the custom-resource example](/playground/?preset=crd){target=_blank} to generate
a configuration from your own resource types.

## Basic example

This minimal configuration shows how watches and a template fit together.
Click **Run live** to edit it and inspect `haproxy.cfg`; it doesn't define a route
to an application.

<div class="pg-embed" markdown data-tab="haproxy.cfg" data-controls="tabs" data-title="A minimal HAProxyTemplateConfig" data-height="440">

```yaml
apiVersion: haproxy-haptic.org/v1alpha1
kind: HAProxyTemplateConfig
metadata:
  name: haproxy-config
  namespace: default
spec:
  credentialsSecretRef:
    name: haproxy-credentials

  podSelector:
    matchLabels:
      app.kubernetes.io/component: loadbalancer

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
          daemon
      defaults
          timeout connect 5s
      frontend http
          bind *:80
```

</div>

## Spec fields

The API server requires `podSelector`, at least one `watchedResources` entry,
and `haproxyConfig` supplied inline or through `libraryRefs`. Other fields
control credentials, output files, validation, and deployment behavior.

### `credentialsSecretRef`

Names the Secret holding the agent credentials. **Optional** — the schema doesn't require it, and the controller never reads it: it resolves the credentials Secret by the name given in `--secret-name` / the `SECRET_NAME` environment variable, both set by the Helm chart. The field records the wiring for readers and tooling; the `namespace` sub-field has no effect.

| Field | Type | Required | Default |
|-------|------|----------|---------|
| `name` | string | Yes | — |
| `namespace` | string | No | Unused; the controller uses its configured Secret namespace |

```yaml
credentialsSecretRef:
  name: haproxy-credentials
```

The referenced Secret contains `dataplane_username` and `dataplane_password` for the explicit
legacy HTTP transport. The default controller-to-agent connection uses separate
mutual TLS identities. See [credentials](./operations/security.md#credentials).

### `podSelector`

Labels that identify which HAProxy pods the controller manages. **Required.**

| Field | Type | Required | Default |
|-------|------|----------|---------|
| `matchLabels` | `map[string]string` | Yes (at least one label) | — |

```yaml
podSelector:
  matchLabels:
    app.kubernetes.io/component: loadbalancer
```

The Helm chart ships `app.kubernetes.io/component: loadbalancer` (plus dynamically set `app.kubernetes.io/name` / `app.kubernetes.io/instance`); use any labels your HAProxy pods actually carry. See [HAProxy Deployment — Pod Requirements](./haproxy-deployment.md#haproxy-pod-requirements) for what discovered pods must provide.

### `watchedResources`

Defines which Kubernetes resources to watch. Each map key is an arbitrary name that appears in templates as `resources.<key>`. **Required** (at least one entry).

More than one key can target the same Kubernetes group, version, and resource tuple, including with different selectors. Admission validation applies a proposed API write to every matching alias, so the dry-run view matches the post-admission watcher stores.

| Field | Type | Required | Default |
|-------|------|----------|---------|
| `apiVersion` | string | Exactly one of `apiVersion` / `apiVersions` | — |
| `apiVersions` | `[]string` | Exactly one of `apiVersion` / `apiVersions` | — |
| `optional` | bool | No | `false` |
| `resources` | string | Yes | — |
| `indexBy` | `[]string` | Required at config load | At least one indexing expression |
| `labelSelector` | string | No | `""` (equality-only, `"k=v[,k=v]"`; set-based syntax not supported) |
| `fieldSelector` | string | No | `""` (client-side JSONPath equality, `"field.path=value"`; matches any field) |
| `namespaces` | `[]string` | No | — (watches every namespace; a list watches only those namespaces and needs `indexBy` to start with `metadata.namespace`; a template lookup into another namespace fails the render) |
| `namespacesSetting` | string | No | `watchedResources.<name>.namespaces` (the setting the lookup error names; the Helm chart sets it when `controller.rbac.releaseNamespaceSecretsOnly` scopes the Secret watch) |
| `store` | string (`full` / `on-demand`) | No | `full` |
| `enableValidationWebhook` | bool | No | `false` |
| `debounceInterval` | string | No | `""` — empty / invalid uses the `100ms` default; an explicit `"0"` disables debouncing |
| `ignoreFields` | `[]string` | No | — (JSONPath expressions dropped from this resource in addition to `watchedResourcesIgnoreFields`; `[*]` selects every array element; an update that changes only ignored fields triggers no render) |

```yaml
watchedResources:
  ingresses:
    apiVersion: networking.k8s.io/v1
    resources: ingresses
    indexBy:
      - metadata.namespace
      - metadata.name
```

Instead of a single `apiVersion`, an entry can declare an ordered
`apiVersions` candidate list together with `optional: true`. The controller
resolves the entry to the first candidate the cluster serves — at startup
and again whenever a matching CRD is installed, upgraded, or removed — so
your configuration works across CRD releases without redeployment:

```yaml
watchedResources:
  tcproutes:
    apiVersions:
      - gateway.networking.k8s.io/v1
      - gateway.networking.k8s.io/v1alpha2
    optional: true   # no served candidate → drop the watch, strip dependent features
    resources: tcproutes
    indexBy:
      - metadata.namespace
      - metadata.name
```

Rules:

- `apiVersion` and `apiVersions` are mutually exclusive; exactly one must be set.
- A **required** entry (no `optional`) whose candidate versions are all unavailable fails
  startup with an error naming the resource — the controller retries and
  converges when the CRD appears.
- An **optional** entry whose candidate versions are all unavailable is dropped, and every
  `templateSnippets` / `validationTests` entry whose `requires` names it gets
  stripped from the effective configuration.
- Templates read the resolved version via `resources.<name>.APIVersion()`.
- The current resolution is visible at `/debug/vars/effectiveConfigResolution`.

See [Watching Resources](./watching-resources.md) for the store types, indexing semantics, and selector behaviour.

### `watchedResourcesIgnoreFields`

JSONPath expressions for fields to remove from all watched resources before they're indexed, reducing memory usage.

| Field | Type | Required | Default |
|-------|------|----------|---------|
| `watchedResourcesIgnoreFields` | `[]string` | No | — |

```yaml
watchedResourcesIgnoreFields:
  - metadata.managedFields
  - metadata.annotations['kubectl.kubernetes.io/last-applied-configuration']
```

Applies uniformly to every watched resource; fields referenced by `indexBy` must not be trimmed. A resource adds its own entries with `watchedResources.<name>.ignoreFields`. See [Watching Resources — Trimming Fields](./watching-resources.md#trimming-fields).

### `haproxyConfig`

The main HAProxy configuration template. **Required.**

| Field | Type | Required | Default |
|-------|------|----------|---------|
| `template` | string | Yes | — |
| `postProcessing` | `[]PostProcessor` | No | — (see [`postProcessing`](#postprocessing-all-template-entries)) |

```yaml
haproxyConfig:
  template: |
    global
        daemon
        maxconn 4096

    defaults
        mode http
        timeout connect 5s

    frontend http
        bind *:80
        use_backend %[req.hdr(host),map({{ pathResolver.GetPath("host.map", "map") }})]
```

See [Template syntax](template-language.md) for loops and expressions.

### `libraryRefs`

Ordered list of [`HAProxyTemplateLibrary`](#haproxytemplatelibrary) objects whose content is merged into this config (optional).

| Field | Type | Required | Default |
|-------|------|----------|---------|
| `name` | string | Yes | — (a `HAProxyTemplateLibrary` in this config's namespace) |
| `revision` | string | Yes | — (must equal that object's `spec.revision`) |

```yaml
libraryRefs:
  - {name: haproxy-config-base, revision: "base-43dc4467f7e88090"}
  - {name: haproxy-config-ssl,  revision: "ssl-5da793f017afc1c5"}
```

Earlier entries are overridden by later ones, and the config's own inline content wins last — so the object you edit is always the override point, whatever the order of the list.

The controller renders only when every reference resolves to an object reporting exactly that `spec.revision`. Otherwise it keeps serving the last-good configuration and logs `Holding the last-good configuration`. Libraries deliberately override one another, so a half-applied set silently *changes* behaviour rather than removing it — a config missing its WAF library would render fine and serve traffic unarmed.

The revisions are compared as strings and never recomputed from content. A writer that applies the config and its libraries together stamps the same value on each, so a torn apply shows up as a mismatch; editing a snippet's body in place leaves the revision alone and takes effect immediately.

### `templateSnippets`

Reusable template fragments, included in other templates via `{{ render "snippet-name" }}`.

| Field | Type | Required | Default |
|-------|------|----------|---------|
| `template` | string | Yes | — |
| `requires` | `[]string` | No | — (names of `watchedResources` keys) |
| `incremental` | object | No | — |

```yaml
templateSnippets:
  backend-name:
    requires: [ingresses]
    template: |
      ing_{{ ingress.metadata.namespace }}_{{ ingress.metadata.name }}
```

`requires` entries must name `watchedResources` keys: when an optional watched
resource named there is unavailable, the snippet is stripped from the effective
configuration. A snippet that must survive stripping may reach a stripped
resource only through compile-safe seams — `render "..." default ""`,
`render_glob` extension points, or shared state — never a direct typed
`resources.<name>` reference. See [Templating — Template Snippets](./template-files.md#template-snippets).

#### Incremental snippets

Set `incremental` when a snippet renders independently for each watched object.
HAPTIC reuses its output until the object or another resource it reads changes.
The bundled libraries already use this feature; you don't need to configure it
for ordinary snippet overrides.

```yaml
templateSnippets:
  route-backends:
    requires: [routes]
    incremental:
      source: routes
    template: |
      backend {{ item | dig_string("", "metadata", "name") }}
```

`source` names one `watchedResources` key and must also appear in `requires`.
For sources selected from configuration, use `bindingsTemplate` instead. It
must render one JSON object whose keys are watched-resource names and whose
values are immutable `props` objects:

```yaml
incremental:
  bindingsTemplate: |
    {{ toJSON(map[string]any{
      tostring(extraContext["routeResource"]): map[string]any{"class": "edge"},
    }) }}
```

Binding templates can read `extraContext`, `capabilities`,
`currentConfig`, `currentFiles`, `pathResolver`, `runtimeEnvironment`, and
`templateSnippets` and pure helper functions. It can't read watched or
controller resources, HTTP content, admission state, or shared state. Only the
values it emits become component props, so changing an unselected ambient value
doesn't execute the component.

In the default template mode, exactly one of `source` and `bindingsTemplate` is
required. A component receives `source`, `item`, `props`, `renderSubject`,
`resources`, `controller`, `http`, and `shared`. Reads through watched resources,
controller resources, and HTTP content are tracked dynamically, including
lookups that found nothing. `requires` still controls
optional-resource stripping; it isn't a dependency declaration or an access
allowlist.

Set `mode: resourceProjection` to publish one exactly indexed watched object
without running the snippet's template. The binding template must select
the watched-resource alias and emit a canonical projection descriptor:

```yaml
templateSnippets:
  selected-certificate:
    requires: [certificates]
    incremental:
      mode: resourceProjection
      bindingsTemplate: |
        {{ toJSON(map[string]any{"certificates": map[string]any{
          "cell": "selected",
          "key": extraContext["certificateName"],
          "keys": []any{extraContext["namespace"], extraContext["certificateName"]},
        }}) }}
      group: selected-certificates
      effects: [publishValue]
    template: '{{- "" -}}'
```

`keys` is the non-empty exact key vector used by that watched store's `Get`.
`cell` and `key` identify the publication; optional `rank` applies the same
ranked-winner selection as `shared.PublishRanked`. An empty binding object
selects nothing. Zero matches publishes no winner and records the negative read,
so creating the object invalidates the result. More than one match fails the
render. The published value is the complete detached resource object and roots
read it with `incremental_values`.

A resource projection requires `bindingsTemplate` and exactly the
`publishValue` effect. It forbids `source`, `whenAnyPathExists`, `root`,
`consumes`, and `optionalConsumes`. The descriptor accepts only `cell`, `key`,
`keys`, and optional `rank`.

`root` groups components for execution. Each member retains its own bindings,
inputs, effects, and cached output.

`item` is one immutable object-valued prop. While a component is active, any
semantic change to that object executes the component; selected store and HTTP
reads add their own exact dependencies. Use `whenAnyPathExists` to keep a
component inactive when none of its finite trigger fields exists.

Use `whenAnyPathExists` when the component has no output or effects unless its
source object carries one of a finite set of fields:

```yaml
incremental:
  source: ingresses
  whenAnyPathExists:
    - metadata.annotations['haproxy-haptic.org/hsts-enable']
    - metadata.annotations['haproxy-haptic.org/hsts-max-age']
```

The paths accept dotted keys, quoted bracket keys, array indices, and `[*]` for
any array element. Filters are rejected. For example,
`spec.rules[*].filters` activates a component when at least one rule has a
`filters` field. An existing field with a null value counts as present. The
predicate reads the post-derivation `item`, so governance can add or remove a
field to activate or deactivate the component without mutating the watched
store. While the predicate remains false, item, or props changes recompute only
the predicate; they don't execute the component body. A false/true transition
replaces the complete component result, including its declared effects.
`whenAnyPathExists` can't guard a `deriveResource` component because that owner
must run before the derived item exists.

`consumes` names publication groups that must exist. `optionalConsumes` names
publication groups that may be absent only when effective-config resolution
authenticated that every producer was stripped with an unavailable optional
resource. Both lists are validated against the complete declaration graph, so
resource absence can't hide a misspelled group or a dependency cycle. Every
extant producer group must complete its canonical root call before the consumer
group runs. An auxiliary root may consume a producer mounted in `haproxy.cfg`,
which always renders first. Once a root starts its own producer sequence, that
sequence must complete before the root reads the group. A different auxiliary
root can't authorize the read because auxiliary roots render concurrently.

`renderSubject` is an immutable object with `mode`, `source`, `namespace`, and
`name`. During admission, `mode` is `admission` only for the proposed object and
each source selected for that object; every other component instance receives
`reconcile`.

The component entry point is compiled against those deterministic globals and
approved pure helpers. Ambient values are available only through selected
immutable `props`; clock and random sources, custom native functions, and
goroutines are unavailable. The component can mutate new local values, but it
can't mutate its published inputs or values returned by tracked stores.

Components in the same `group` can share keyed results. Without `group`, the
snippet name is the group, so snippets don't share results. Render a group
through one or more complete sequences. Each sequence renders every component
in snippet-name order from one root template. Repeating the sequence mounts
cached text again without re-executing component bodies or effects. Winners
are selected by component name, source name, namespace, object name, and call
order.

`shared.Unique(cell, key, text)` contributes deduplicated output. A component
that calls it must emit no ordinary text, including whitespace.

`shared.Publish(cell, key, value)` publishes a detached structured value. A
root reads the winning values with `incremental_values(group, cell)`, ordered
by their winner locations. The function may evaluate the group before its
normal render call, but roots must still render at least one complete canonical
group sequence. An unknown group or a group without `publishValue` fails; a known
publication group with no winners in that cell returns an empty slice. Every
call returns fresh immutable values, and the same group can be read from the
main configuration, maps, files, certificates, and Kubernetes-resource roots.
Neither `shared.Publish` nor `incremental_values` is available to a binding
template; `incremental_values` is also unavailable inside a component.

`shared.PublishRanked(cell, key, rank, value)` selects the lexicographically
smallest non-empty rank before applying the normal deterministic owner order.
Every publisher for the same cell and key must use either ranked or rank-free
publication consistently.

A component declared with `consumes` or `optionalConsumes` reads one winning
value with `shared.Select(group, cell, key)`, which returns the value and a
boolean. The render graph records only that exact selector. A missing winner is
also recorded, so creating its first publisher executes that consumer, while a
losing publisher change doesn't. Values are detached and immutable. Winner
replacement, deletion, and promotion invalidate the consumer only when the
selected bytes change.

`shared.SelectValues(group, cell)` reads all winners in canonical order.
`shared.Count(group, cell)` reads the number of unique winning keys in the
cell in O(1). The count invalidates its consumer only when it changes, so an
equal-count winner promotion doesn't execute it. Both calls require the group
in `consumes` or `optionalConsumes` and a complete authenticated canonical
producer call before the read.

Declare each supported effect before using it:

| `effects` value | Result |
|-----------------|--------|
| `deriveResource` | Publishes an immutable transformed view of the source object before root templates read resources. |
| `recordEvent` | Records a Warning Event for the resource passed to `recordEvent`. |
| `backendPlan` | Records canonical `planRegistry.Profile` and `planRegistry.Backend` declarations for replay into the current `haproxy.cfg` render. |
| `publishValue` | Enables immutable keyed structured values through `shared.Publish` or `shared.PublishRanked`, read by roots with `incremental_values` or by declared consumers with `shared.Select`, `shared.SelectValues`, or `shared.Count`. |
| `statusPatch` | Records detached raw `statusPatch` calls for deterministic replay after every component group has completed. |

Only one active component may declare `deriveResource` for a source. When
incremental snippets are configured, every derivation producer for that source
must use that owner; a later root-level `deriveResource` call fails.

A component that declares `backendPlan` receives a restricted `planRegistry`
with `Profile`, `Backend`, and `BackendWhenAny`. It must render from
`haproxy.cfg` and can't use `shared.Unique`. Backend calls use first-winner
arbitration by backend name. The renderer orders candidates by component name,
source name, namespace, object name, and call order. A later declaration for
the same backend is suppressed even when its record or text differs; deleting
the winner promotes the next cached candidate.

`BackendWhenAny(record, text, cell, keys)` makes its declaration eligible only
when that same component instance owns a winning `shared.Publish` contribution
for at least one listed key in the cell. Keys must be non-empty; they're sorted
and deduplicated, and every referenced publication must exist in the component
result. Publications from every component in the group participate, including
components without `backendPlan`. Cells in different groups never compete.

Every non-empty winning backend profile needs a matching `Profile` declaration.
A render must succeed completely before HAPTIC retains its output. Admission
checks don't change the live configuration or publish Kubernetes Events.

### `maps`

HAProxy map file templates. Each key is a map filename, referenced in config via `{{ pathResolver.GetPath("host.map", "map") }}`.

| Field | Type | Required | Default |
|-------|------|----------|---------|
| `template` | string | Yes | — |
| `postProcessing` | `[]PostProcessor` | No | — (see [`postProcessing`](#postprocessing-all-template-entries)) |
| `ordered` | bool | No | `true` |

```yaml
maps:
  host.map:
    ordered: false
    template: |
      {% for _, ingress := range resources.ingresses.List() %}
      {% for _, rule := range ingress.spec.rules %}
      {{ rule.host }} {{ ingress.metadata.name }}_backend
      {% end %}
      {% end %}
```

Set `ordered: false` when the configuration reads the map with `map_str`, `map_beg`, `map_ip` or `map_str_int`. Those find a key by its own value, so the controller can update individual entries through the Runtime API. Ordered maps use atomic replacement when needed to preserve match order.

Keep the default `true` for `map_reg`, `map_sub`, `map_dom`, `map_dir` and `map_end`. HAProxy evaluates those as a list and takes the first match, so an entry has to land in its intended position — appending it to the end would silently never match.

See [Templating — Map Files](./template-files.md#map-files).

### `files`

General auxiliary file templates (error pages, etc.). Each key is a filename, referenced in config via `{{ pathResolver.GetPath("503.http", "file") }}`.

| Field | Type | Required | Default |
|-------|------|----------|---------|
| `template` | string | Yes | — |
| `postProcessing` | `[]PostProcessor` | No | — (see [`postProcessing`](#postprocessing-all-template-entries)) |
| `reloadOnPush` | bool | No | `true` |

```yaml
files:
  503.http:
    template: |
      HTTP/1.1 503 Service Unavailable
      Content-Type: text/html

      <html><body><h1>503</h1></body></html>
```

Set `reloadOnPush: false` when a sidecar owns the file and watches it itself — the bundled Vector and SPOA-hub configs both do. HAProxy never opens those, so the controller writes the new content and skips the reload. Keep the default for anything the HAProxy configuration references: only a reload makes that content take effect.

Removing a file also reloads HAProxy when that file was declared with
`reloadOnPush: true`. A sidecar-owned file with `reloadOnPush: false` can be
removed without a reload. Remove references from the HAProxy configuration when
you remove a file it uses.

See [Templating — General Files](./template-files.md#general-files).

### `sslCertificates`

SSL certificate templates, typically assembled from watched Secrets. Each key is a certificate name, referenced in config via `{{ pathResolver.GetPath("example-com", "cert") }}`.

| Field | Type | Required | Default |
|-------|------|----------|---------|
| `template` | string | Yes | — |
| `postProcessing` | `[]PostProcessor` | No | — (see [`postProcessing`](#postprocessing-all-template-entries)) |

```yaml
sslCertificates:
  example-com:
    template: |
      {% var secret = resources.secrets.GetSingle("default", "tls-cert") %}
      {{ b64decode(secret.data["tls.crt"]) }}
      {{ b64decode(secret.data["tls.key"]) }}
```

See [Templating — SSL Certificates](./template-files.md#ssl-certificates).

### `k8sResources`

Templates that emit Kubernetes resources for the controller to apply via Server-Side Apply. Each entry's rendered output is parsed as one or more YAML documents (multi-doc supported via `---` separators); each document must declare `apiVersion`, `kind`, and `metadata.name` (plus `metadata.namespace` for namespaced kinds).

| Field | Type | Required | Default |
|-------|------|----------|---------|
| `template` | string | Yes | — |
| `postProcessing` | `[]PostProcessor` | No | — (see [`postProcessing`](#postprocessing-all-template-entries)) |
| `createOnlyFields` | `[]string` | No | Dotted field paths whose initial values come from the template; subsequent applies preserve the live values |

By default, HAPTIC owns rendered resources: it adds a controller `OwnerReference`
to the `HAProxyTemplateConfig`, prunes resources removed from the rendered set,
and lets Kubernetes garbage-collect them when the configuration is deleted.

For a jointly managed resource, add `haproxy-haptic.org/ownership: partial` to its
rendered annotations. HAPTIC omits the owner reference and `managed-by` label,
excludes the resource from orphan cleanup, and removes the ownership annotation
before applying it. Server-Side Apply tracks the fields HAPTIC contributes.

Templates have full access to the same engine context as `haproxyConfig` — `resources`, filters, `templateSnippets`, `fileRegistry`, `extraContext`, and the per-render `shared` cache — so a `k8sResources` template can render extension points (`render_glob` patterns) and read shared state populated by the main config template.

```yaml
k8sResources:
  edge-service:
    template: |
      apiVersion: v1
      kind: Service
      metadata:
        name: edge
        namespace: {{ extraContext["controllerNamespace"] }}
      spec:
        type: LoadBalancer
        selector:
          app.kubernetes.io/component: loadbalancer
        ports:
          - name: http
            port: 80
            targetPort: http
            protocol: TCP
      ---
      apiVersion: discovery.k8s.io/v1
      kind: EndpointSlice
      metadata:
        name: edge-default
        namespace: {{ extraContext["controllerNamespace"] }}
        labels:
          kubernetes.io/service-name: edge
      addressType: IPv4
      endpoints:
        - addresses: ["10.0.0.1"]
      ports:
        - name: http
          port: 80
          protocol: TCP
```

#### `createOnlyFields`

Dotted field paths whose rendered value applies when the object is created and never again. After that the controller reads the value the object currently has and re-applies that, so the template's value is the state the object *starts* in rather than the state it's held to.

Use it for a field something else legitimately owns while the object runs. `spec.replicas` on a workload is the case it exists for: HAProxy routes to whatever pods are there, so the replica count belongs to an operator draining the workload, or to a HorizontalPodAutoscaler. Without this the field is re-applied on every reconciliation, and a deliberate `kubectl scale` is overwritten a second after it's made.

```yaml
k8sResources:
  cache:
    createOnlyFields: ["spec.replicas"]
    template: |
      apiVersion: apps/v1
      kind: StatefulSet
      metadata:
        name: cache
        namespace: {{ extraContext["controllerNamespace"] }}
      spec:
        replicas: {{ extraContext | dig("cache", "replicas") | fallback(2) }}
        # …
```

Change the value in your configuration and it takes effect only on a workload that doesn't exist yet; to resize a running one, scale it directly. A path the rendered object doesn't set is ignored, and each path must name a field the template itself sets — the controller keeps ownership of it, so a path it never sends would be deleted from the object.

The bundled chart declares this on the two workloads it manages whose size is operational rather than structural: the Varnish cache and the shared rate-limit Valkey store.

Use this when the resource shape derives from observed cluster state (Ingresses, Gateways, Endpoints, …); use the chart's own static `templates/*.yaml` for fixed install-time wiring (RBAC, the internal agent Service, etc.). The chart's `charts/haptic/charts/base/library.yaml` ships a canonical example: the `haproxy-service` entry that renders the user-facing HAProxy LoadBalancer Service from listener state.

### `postProcessing` (all template entries)

Every template-bearing entry — `haproxyConfig` and each entry under `maps`, `files`, `sslCertificates`, and `k8sResources` — accepts an optional `postProcessing` list that transforms the rendered output before it's used. Processors run sequentially.

| Field | Type | Required | Default |
|-------|------|----------|---------|
| `type` | string (`regex_replace` / `template`) | Yes | — |
| `params` | `map[string]string` | Yes | — |

Params per type:

| Type | Params |
|------|--------|
| `regex_replace` | `pattern` (regular expression), `replace` (replacement string) — applied line by line |
| `template` | `source` (a template; the rendered output is available as the `input` variable) |

```yaml
haproxyConfig:
  template: |
    ...
  postProcessing:
    - type: regex_replace
      params:
        pattern: '[ \t]+$'
        replace: ""
    - type: template
      params:
        source: "{{ replace(input, \"__REGION__\", \"eu-west-1\") }}"
```

See [Templating — Post-Processing](./template-files.md#post-processing) for a runnable example.

### `templatingSettings`

Template rendering configuration and custom variables.

| Field | Type | Required | Default |
|-------|------|----------|---------|
| `extraContext` | object (any JSON value) | No | — |
| `testExtraContext` | object (any JSON value) | No | — |
| `engine` | string (`scriggo`) | No | `scriggo` (the only valid value) |

```yaml
templatingSettings:
  extraContext:
    environment: production
    featureFlags:
      rateLimiting: true
```

Custom variables are exposed to templates as the `extraContext` map. Read a key with `extraContext["key"]`, or `extraContext | dig("key") | fallback(default)` when it may be unset:

```go
{% if extraContext["environment"] == "production" %}
  timeout client {{ extraContext | dig("customTimeout") | fallback("300") }}s
{% end %}
```

See [Custom template variables](templating.md#custom-template-variables) for detailed examples.

`testExtraContext` is what validation tests render with instead of `extraContext`, so the values you deploy with can't change what a test asserts. The Helm chart sets it to the context its default values produce. See [Extra context](validation-reference.md#extra-context).

### `validationTests`

Embedded validation tests (optional; run by the pre-rollout validation Job, the `validate` CLI, and the controller itself on config load and on every live config change). Across a merged set, a test name may be defined by only one object — a duplicate is an error naming both — while the reserved `_global` baseline accumulates across objects.

| Field | Type | Required | Default |
|-------|------|----------|---------|
| `description` | string | No | — |
| `fixtures` | `map[string][]object` | No | — (keys must name `watchedResources` entries, plus the reserved `haproxy-pods` key) |
| `assertions` | `[]Assertion` | Yes | — |
| `httpResources` | `[]object` | No | — (mocked responses for `http.Fetch()` calls) |
| `currentServers` | `map[string]map[string]object` | No | — (backend → server → `{address, port}` of a previous deployment, exposed to templates as `currentConfig.ServerIndex`) |
| `currentConfig` | string | No | — (deprecated: a raw HAProxy config parsed down to the same server index as `currentServers`) |
| `currentFiles` | `map[string]string` | No | — (filename → content of the general files currently deployed, exposed to templates as `currentFiles`) |
| `extraContext` | object | No | — (per-test overrides of `templatingSettings.extraContext`) |
| `useDeploymentWatchScopes` | bool | No | `false`; apply configured watch namespaces to fixtures and lookups for this test; can't be `true` on `_global` |
| `minHAProxyVersion` | string | No | — (skip the test on older HAProxy) |
| `requires` | `[]string` | No | — (strip the test when a named optional watched resource is unavailable) |
| `requiresFields` | `[]string` | No | — (strip the test when a schema field path is absent) |

See [Write validation tests](validation-tests.md) for a complete example and local
commands, and the [test reference](validation-reference.md) for fixture and
assertion fields.

### `validators`

Pluggable validator sidecars consulted before rendered output is published or deployed (optional).

| Field | Type | Required | Default |
|-------|------|----------|---------|
| `name` | string | Yes | — (RFC 1123 label, unique across the array) |
| `socketPath` | string | Yes | — (absolute path to a Unix domain socket inside the controller pod) |
| `files` | `[]string` | Yes (at least one) | — (glob patterns matched against rendered file paths) |
| `dataFiles` | `[]string` | No | — (glob patterns for files sent as *data*, never validated on their own) |
| `timeoutMs` | integer | No | `5000` (range 1–60000) |
| `maxConnections` | integer | No | `4` (range 1–32) |

Globs follow Go's `path/filepath.Match` rules and must use the same relative or absolute form as the rendered path: `*` and `?` don't cross `/`, and `**` isn't supported. Malformed patterns fail configuration validation.

`dataFiles` covers files the validator needs in order to check something else but must not check on its own. Every match is attached to every request to that validator, marked as data. A validator sidecar runs in the controller pod and can't read the HAProxy pod's filesystem, so a config that `Include`s a ruleset by path is only checkable if the ruleset's content travels with the request. A file matching both `files` and `dataFiles` is treated as data.

```yaml
validators:
  - name: spoa-hub
    socketPath: /var/run/haptic-validators/spoa-hub.sock
    files:
      - "general/spoa-hub-config.toml"
    dataFiles:
      - "general/*.conf"
```

See [Pluggable Validators](./operations/pluggable-validators.md) to add a sidecar and connect it to generated files.

### `controller`

Controller-level settings for leader election and config publishing.

| Field | Type | Required | Default |
|-------|------|----------|---------|
| `leaderElection.enabled` | bool | No | `true` |
| `leaderElection.leaseName` | string | No | `""` → `haptic-leader` (the Helm chart sets the release `fullname`) |
| `leaderElection.leaseDuration` | string | No | `30s` |
| `leaderElection.renewDeadline` | string | No | `20s` |
| `leaderElection.retryPeriod` | string | No | `5s` |

```yaml
controller:
  leaderElection:
    enabled: true
    leaseDuration: 30s
    renewDeadline: 20s
    retryPeriod: 5s
```

!!! note
    There is no reconciler-level debounce knob. The Reconciler fires immediately on every resource/HTTP event; batching is per-watcher (`spec.watchedResources.<name>.debounceInterval`, default `100ms`) and reload throttling is the deployer's `spec.dataplane.minDeploymentInterval`.

See [High Availability](./operations/high-availability.md) for leader election details.

#### `configPublishing`

Controls how rendered configurations are stored in `HAProxyCfg` CRD resources.

| Field | Type | Required | Default |
|-------|------|----------|---------|
| `compressionThreshold` | int64 | No | `1048576` (1 MiB). A value of `0` is treated as unset — the 1 MiB default applies (compression can't currently be disabled) |

```yaml
controller:
  configPublishing:
    compressionThreshold: 1048576
```

When the rendered configuration exceeds the threshold, it's compressed with zstd and base64-encoded; the `HAProxyCfg` resource stores it with `spec.compressed: true`, reducing etcd storage and speeding up watch events for large configurations. To read a published config back in plaintext, use `haptic config view` — see [Debugging](./operations/debugging.md#common-recipes).

The controller rejects compressed auxiliary files that expand beyond 64 MiB per file, including concatenated zstd frames.

### `logging`

Log level configuration.

| Field | Type | Required | Default |
|-------|------|----------|---------|
| `level` | string (`TRACE`, `DEBUG`, `INFO`, `WARN`, `ERROR`; case-insensitive) | No | `""` → the `LOG_LEVEL` environment variable → `INFO` |

```yaml
logging:
  level: DEBUG
```

### `dataplane`

Connection and update settings for the agent in each HAProxy pod.

| Field | Type | Required | Default |
|-------|------|----------|---------|
| `port` | integer (1–65535) | No | `5555` |
| `minDeploymentInterval` | string | No | `2s` (the Helm chart ships `5s`) |
| `driftPreventionInterval` | string | No | `60s` |
| `deploymentTimeout` | string | No | `30s` |
| `configPublishInterval` | string | No | `10s` |
| `reloadVerificationTimeout` | string | No | `60s` (the agent's ceiling, which is also its maximum) |
| `syncTimeout` | string | No | `2m` |
| `mapsDir` | string | No | `/etc/haproxy/maps` |
| `sslCertsDir` | string | No | `/etc/haproxy/certs` (the Helm chart sets `/etc/haproxy/ssl`) |
| `generalStorageDir` | string | No | `/etc/haproxy/general` |
| `configFile` | string | No | `/etc/haproxy/haproxy.cfg` |

```yaml
dataplane:
  port: 5555
  minDeploymentInterval: 2s
  driftPreventionInterval: 60s
```

The three `*Dir` paths are used by the controller's local `haproxy -c` validation step as well as for rendering the paths the configuration references — they must match where the HAProxy pod mounts each directory. The Helm chart keeps them in sync by deriving both sides from a single set of chart values.

`minDeploymentInterval` and `reloadVerificationTimeout` also become agent flags whenever the chart deploys the HAProxy fleet. The agent rejects either above `60s` and exits at startup, so the chart fails the render instead. For tuning guidance on the interval fields, see [Performance — Deployment Pacing](./operations/performance.md#deployment-pacing).

## Status Subresource

The controller updates the status field with validation results:

| Field | Type | Description |
|-------|------|-------------|
| `observedGeneration` | int64 | The `.metadata.generation` the status reflects |
| `lastValidated` | timestamp | Last successful validation |
| `validationStatus` | string | `Valid`, `Invalid`, or `Unknown` — the printer column shown by `kubectl get htplcfg` |
| `validationMessage` | string | Human-readable summary |
| `validationErrors` | `[]string` | Populated when `Invalid`, with each entry naming the template and error context, or when `Unknown`, with why validation couldn't complete |
| `conditions` | `[]Condition` | Standard `metav1.Condition` list. The controller writes exactly one type, `Validated`. |

The `Validated` condition carries `observedGeneration`. Compare it with
`metadata.generation` before relying on the result: `kubectl wait --for=condition=Validated` alone can succeed on a condition from an older generation. Its reasons are `ValidationSucceeded`, `ConfigInvalid`, `HAProxyValidationFailed`, `LoadGateFailed`, and `ValidationInconclusive`.

`LoadGateFailed` means a starting controller pod's load gate rejected the config.
That pod keeps retrying and stays NotReady, so a rolling upgrade stalls on the
old pods. The kubelet restarts it each time its startup probe runs out
(5 minutes by default). If an old pod also rejects the config while leading,
it writes its own `ConfigInvalid` verdict back, so the reason can alternate
between the two.

`ValidationInconclusive` (condition status `Unknown`) means the controller couldn't validate the generation yet, for example because the API server didn't return the watched resources' schemas in time. The previous configuration keeps running, and the controller validates the generation again with backoff, up to every 30 seconds, until it's accepted or rejected.

Each controller replica exports its view of the condition as the `haptic_config_validated` metric, so you can alert on a configuration that stays `False` or `Unknown`. See [config validation state](operations/metrics-reference.md#config-validation-state).

When the config references libraries (see [`libraryRefs`](#libraryrefs)), the
condition reports validation of the combined configuration on the owning
`HAProxyTemplateConfig`.

```yaml
status:
  observedGeneration: 1
  lastValidated: "2025-01-27T10:00:00Z"
  validationStatus: Valid
  validationMessage: "All validation tests passed"
  validationErrors: []
  conditions:
    - type: Validated
      status: "True"
      reason: ValidationSucceeded
      observedGeneration: 1
      lastTransitionTime: "2025-01-27T10:00:00Z"
```

## `HAProxyCfg` deployment status

The controller publishes the rendered configuration as an `HAProxyCfg` resource and records what each HAProxy pod runs in `status.deployedToPods[]`:

| Field | Type | Description |
|-------|------|-------------|
| `podName` | string | The HAProxy pod this entry describes |
| `podUID` | string | The pod incarnation the entry belongs to |
| `podRuntimeID` | string | The container execution epoch the entry belongs to |
| `checksum` | string | Checksum of the configuration applied to the pod. It equals `spec.checksum` once the pod has converged |
| `appliedPlanID` | string | The render plan the pod last accepted |
| `runningPlanID` | string | The render plan the pod's running HAProxy serves. It trails `appliedPlanID` while a reload is still pending |
| `mode` | string | How the plan was applied: `runtime`, `file_only`, `reload`, `scheduled`, `noop`, or `rejected`. Empty when the applier reports no mode |
| `reasons` | `[]string` | Why the apply took that mode, most significant first, at most 8 entries; when more were recorded the last entry says how many were omitted |
| `lastError` | string | Error message from the most recent failed sync, cleared when a sync succeeds |
| `consecutiveErrors` | int | Number of consecutive sync failures, reset to 0 on success |

## `HAProxyTemplateLibrary`

A reusable collection of templates and tests. Reference a library from one or
more configurations through [`libraryRefs`](#libraryrefs). The Helm chart creates
one object for each enabled bundled library.

**API Group**: `haproxy-haptic.org`
**API Version**: `v1alpha1`
**Kind**: `HAProxyTemplateLibrary`
**Short Name**: `htpllib`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `revision` | string | Yes | Identifies this content to the configs that reference it |
| `templateSnippets` | map | No | Same shape as the config's [`templateSnippets`](#templatesnippets) |
| `validationTests` | map | No | Same shape as [`validationTests`](#validationtests) |
| `maps` | map | No | Same shape as [`maps`](#maps) |
| `files` | map | No | Same shape as [`files`](#files) |
| `sslCertificates` | map | No | Same shape as [`sslCertificates`](#sslcertificates) |
| `k8sResources` | map | No | Same shape as [`k8sResources`](#k8sresources) |
| `templatingSettings` | object | No | Template-context defaults; the config merges last, so an operator always wins |
| `haproxyConfig` | object | No | Exactly one member of a merged set supplies it |

A library carries **no** `podSelector`, `watchedResources`, `dataplane`, `validators`, `controller` or `logging` — it can't redefine the controller's operational identity.

You choose the `revision` value; the controller only ever compares it against the reference and never derives one from the content. That's what lets `kubectl edit` change a snippet in place and take effect immediately — the content moves, the revision doesn't, so the reference still matches. A digest of the content is the convenient source for a generator, because it changes exactly when the content does.

```yaml
apiVersion: haproxy-haptic.org/v1alpha1
kind: HAProxyTemplateLibrary
metadata:
  name: haproxy-config-base
  namespace: default
spec:
  revision: "base-43dc4467f7e88090"
  templateSnippets:
    global-section:
      template: |
        global
            daemon
```

```bash
kubectl get haproxytemplatelibrary   # or: kubectl get htpllib
```

Names must be unique across the merged set for `validationTests`. See [ADR-0017](development/adr/0017-template-library-kind.md) for the rationale, and [`haptic config view --input`](./operations/debugging.md) to print the merged result.

## Command-line management

List configurations and libraries across namespaces:

```bash
kubectl get htplcfg,htpllib -A
```

Print the complete input used by a Helm-installed controller:

```bash
haptic config view --input --namespace haptic
```

For persistent changes to a Helm release, update your values file and
[upgrade the release](deploying-with-helm.md#change-settings).
Direct edits to chart-generated objects are overwritten on the next upgrade.

<a id="validate-before-applying"></a>
<a id="view-configurations"></a>
<a id="edit-configuration"></a>

To validate an exported configuration, prepare the
[offline schemas](validation-tests.md#prepare-schemas), export the complete
configuration, and run its tests:

```bash
haptic config view --input --namespace haptic > haptic-config.yaml
haptic validate -f haptic-config.yaml --schema-dir ./schemas
```

## Validation

The CRD includes OpenAPI schema validation that checks:

- Required fields are present
- Field types are correct
- String lengths meet minimum/maximum requirements
- Integer values are within valid ranges
- Enum values match allowed options

Additional validation occurs when:

1. **Pre-rollout Helm hook** - the chart's `pre-install`/`pre-upgrade` Job runs `haptic preflight`, which renders the chart from your values and runs the embedded tests before Helm applies the release workloads and configuration
2. **Controller startup** - the load gate runs the embedded tests before the controller serves; on a failure the new pod retries and never becomes Ready, so it doesn't replace a working one
3. **Live config change** - the same suite re-runs on every config change; a failure is refused and the last-good config keeps serving
4. **CLI command** - `haptic validate` runs tests locally

<a id="best-practices"></a>

## See also

- [Templating Guide](./templating.md) — template syntax, loops, status patches
- [Template Reference](./template-reference.md) — context variables, functions, `pathResolver`
- [Watching Resources](./watching-resources.md) — store types, indexing, selectors
- [Validation Tests](./validation-tests.md) — writing and running embedded tests
- [CRD & Validation Design](./development/crd-validation-design.md) — rationale behind the CRD shape and validation layers
- [Getting Started](./getting-started.md) — installation walkthrough
