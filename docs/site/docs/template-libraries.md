# Template libraries

A template library is a set of text templates that turns Kubernetes resource
fields into HAProxy configuration. The bundled libraries handle Ingress, Gateway
API, annotations, and TLS, and include validation tests for their output.
Enable or disable them through Helm values.

## Available libraries

| Library | Default | Purpose |
|---------|---------|---------|
| [Base](libraries/base.md) | Enabled | Core HAProxy configuration, extension point definitions; disabling drops the `haproxyConfig` the other libraries plug into |
| kubernetes-backends | Enabled | Service port and EndpointSlice resolution for the routing libraries |
| [SSL](libraries/ssl.md) | Enabled | TLS certificate management, HTTPS frontend |
| [Ingress](libraries/ingress.md) | Enabled | Kubernetes Ingress resource support |
| [Gateway API](libraries/gateway.md) | Enabled | Gateway API (HTTP, gRPC, TLS and TCP routes) support |
| [ingress-annotations-compat](libraries/ingress-annotations-compat.md) | Enabled | Shared helpers for native and vendor Ingress annotation libraries |
| [governance](operations/governance.md) | Enabled | Defaults and constraints on watched resource fields; add rules under `controller.config.templatingSettings.extraContext.governance.rules` |
| [haptic-annotations](libraries/haptic-annotations.md) | Enabled | `haproxy-haptic.org/*` — HAPTIC's native vocabulary; the only annotation library enabled by default |
| [haproxytech](libraries/haproxytech.md) | Disabled | `haproxy.org/*` annotations ([haproxytech/kubernetes-ingress](https://github.com/haproxytech/kubernetes-ingress) compat) — opt-in migration aid |
| [haproxy-ingress](libraries/haproxy-ingress.md) | Disabled | `haproxy-ingress.github.io/*` annotations ([jcmoraisjr/haproxy-ingress](https://haproxy-ingress.github.io/) compat) — opt-in migration aid |
| [nginx-ingress](libraries/nginx-ingress.md) | Disabled | `nginx.ingress.kubernetes.io/*` annotations ([kubernetes/ingress-nginx](https://kubernetes.github.io/ingress-nginx/) compat) — opt-in migration aid |
| vector | Loaded with `vector.enabled` (default on) | Configures access-log processing and traffic metrics |
| [spoa-hub](operations/spoa-hub.md) | Loaded when the hub or a plugin is enabled | Connects HAProxy to plugins for authentication, request inspection, and other policies |

## Enabling and disabling libraries

Configure libraries in your [complete Helm values file](deploying-with-helm.md#change-settings).
The [values reference](reference.md#template-libraries) lists every switch:

```yaml
controller:
  templateLibraries:
    base:
      enabled: true   # Default — disabling drops the haproxyConfig the other libraries plug into
    ssl:
      enabled: true   # TLS/HTTPS support
    ingress:
      enabled: true   # Kubernetes Ingress
    gateway:
      enabled: true   # Gateway API
    hapticAnnotations:
      enabled: true   # haproxy-haptic.org native annotations (default)
    haproxytech:
      enabled: false  # haproxy.org compat — opt-in migration aid
    haproxyIngress:
      enabled: false  # haproxy-ingress.github.io compat — opt-in migration aid
    nginxIngress:
      enabled: false  # nginx-ingress compat — opt-in migration aid
  config:
    templatingSettings:
      extraContext:
        routing:
          regexMatchOrder: default  # "default" or "last" — see Path Matching Order below
```

<a id="overview"></a>

## How your settings combine with libraries

Each enabled library becomes an `HAProxyTemplateLibrary` resource. The chart's
`HAProxyTemplateConfig` references these libraries in merge order and contains
your `controller.config` overrides. See [merge order](#library-merge-order) for how overrides apply.

Inspect the merged configuration:

```bash
haptic config view --input --namespace haptic
```

Try changing a hostname in an example that combines the routing libraries:

<div class="pg-embed" markdown data-scenario="all" data-facade="spec.templateSnippets.map-host-500-ingress" data-tab="haproxy.cfg" data-controls="tabs,resources" data-title="Change a hostname and inspect its routing map" data-height="440">

<p class="pg-task" markdown>In the **Resources** panel, change the `blog` Ingress's host from `blog.example.com` to `news.example.com`, then open the `maps` tab and watch the `host.map` entry follow.</p>

<details class="pg-hint" markdown>
<summary>What to expect</summary>

The `host.map` entry changes from `blog.example.com blog.example.com` to
`news.example.com news.example.com`. The Ingress library generates this mapping
from the Ingress's host field.

</details>

</div>

## Path matching order

Path-based routing inside the rendered `frontend-routing-logic` snippet evaluates four map types: exact, regex, prefix-exact, and prefix. The evaluation order is selected by `controller.config.templatingSettings.extraContext.routing.regexMatchOrder`:

| Value | Order | Use case |
|-------|-------|----------|
| `default` (default) | Exact > Regex > Prefix-exact > Prefix | A matching regex takes precedence over a prefix. |
| `last` | Exact > Prefix-exact > Prefix > Regex | A matching prefix takes precedence over a regex. |

## Library merge order

Libraries are merged in a specific order, with later libraries overriding earlier ones:

```
 1. base/                 (lowest priority)
 2. ssl/
 3. ingress/
 4. gateway/
 5. ingress-annotations-compat/  (level 2.5 - Ingress-only shared scaffold)
 6. governance/
 7. haptic-annotations/   (native haproxy-haptic.org/* superset)
 8. haproxytech/
 9. haproxy-ingress/
10. nginx-ingress/
11. spoa-hub/            (auto-loaded when SPOA hub sidecar is enabled)
12. vector/              (loaded with vector.enabled; contributes only the sidecar's config file)
13. controller.config.*  (highest priority - your values.yaml overrides for templateSnippets / maps / files / sslCertificates / haproxyConfig / validationTests / watchedResources)
```

Your custom configuration in `controller.config` always takes precedence.

## Extension points

An extension point includes snippets whose names match a pattern. Use it to add
directives or routing entries without replacing the surrounding template.

### How extension points work

The base library uses `render_glob "prefix-*"` to automatically include all template snippets matching a glob pattern:

```scriggo
{# In the base library #}
{{- on_own_lines(render_glob "backends-*") }}
```

This includes all snippets whose names start with `backends-` (for example `backends-500-ingress`, `backends-500-gateway`, any user-provided `backends-*`). Snippets render in alphabetical order, so numeric prefixes control execution order — see the [snippet priority numbering table](#snippet-priority) below.

[`on_own_lines()`](template-reference.md#functions-and-filters) starts each snippet on a new line, so a snippet doesn't need a leading or trailing newline. Use it for extension points you add to your own templates.

### Available extension points

Choose an extension point by where your configuration belongs:

| Add | Snippet prefix |
| --- | --- |
| Global HAProxy settings | `global-settings-*` |
| Shared HTTP frontend directives | `frontend-extra-*` |
| Ingress backend directives | `backend-directives-*` |
| Your own backends | `backends-*` |

The [extension point reference](libraries/base.md#available-extension-points)
lists every hook, its position, and the variables available there.

<a id="injecting-custom-configuration"></a>

For a complete snippet and deployment procedure, follow
[Write your first template](templating.md).

### Library Configuration via `extraContext`

`extraContext` supplies settings to every snippet. It contains values computed by
the chart, such as ports and Service names, plus your settings under
`controller.config.templatingSettings.extraContext`.

Use it to override library defaults. For example, set the nginx-ingress library's
HTTP-to-HTTPS redirect status code (default `308`):

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        nginxHttpRedirectCode: "301"   # override the library default of 308
```

A value you set here always wins over the library's default. Custom snippets
read any key the same way:

```scriggo
{%- var code = extraContext | dig("nginxHttpRedirectCode") | fallback("308") | tostring() %}
```

### Snippet priority

Snippets within a `render_glob` pattern execute in **alphabetical order**. Priority is encoded in the snippet name via a numeric prefix:

For example, `features-050-my-init` runs before `features-500-*`, while
`features-700-my-finalize` runs after it. Use fixed-width numbers so their
alphabetical order matches the intended order.

Reserved numeric ranges used by the built-in libraries:

| Range | Purpose |
|-------|---------|
| 000-099 | Infrastructure / initialization |
| 100-199 | Feature registration |
| 200-499 | Security, Cross-Origin Resource Sharing (CORS), header manipulation, redirects |
| 500-599 | Core features (ingress, gateway) |
| 600-699 | haproxy-ingress (`haproxy-ingress.github.io/*`) compatibility |
| 700-799 | nginx-ingress (`nginx.ingress.kubernetes.io/*`) compatibility |
| 800-899 | haptic-annotations (`haproxy-haptic.org/*`) native vocabulary |
| 900-999 | Finalization / cleanup |

These ranges determine snippet order. They don't resolve conflicting annotations
on one Ingress: HAPTIC rejects contradictory values from different annotation
families. Use one annotation family per feature; see [annotations](annotations.md).

<a id="which-libraries-use-which-extension-points"></a>

See each library's reference for the hooks and variables it provides. Start
with [your first template](templating.md) for a complete customization workflow.

## Custom libraries

To route from your own resources, add a watch and write snippets for the relevant
extension points. This example reads ConfigMaps and generates backends and host
routing entries through `backends-*` and `map-host-*`.

<div class="pg-embed" markdown data-tab="haproxy.cfg" data-controls="tabs,resources" data-title="ConfigMaps → backends and host.map" data-height="480">

<p class="pg-task" markdown>Open the **Resources** panel and add `routing: enabled` to the `blog` ConfigMap's `metadata.labels`, then watch a `backend cm_content_blog` block appear in `haproxy.cfg` and a matching line show up in the `maps` tab.</p>

```yaml
apiVersion: haproxy-haptic.org/v1alpha1
kind: HAProxyTemplateConfig
metadata:
  name: configmap-library-demo
spec:
  watchedResources:
    configmaps:
      apiVersion: v1
      resources: configmaps
      indexBy: ["metadata.namespace", "metadata.name"]

  # A minimal base that invokes the extension points your snippets plug into.
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
        use_backend %[req.hdr(host),lower,map({{ pathResolver.GetPath("host.map", "map") }})]
        default_backend not-found
      {{ render_glob "backends-*" }}
      backend not-found
        http-request deny deny_status 404
  maps:
    host.map:
      template: |
        {{ render_glob "map-host-*" }}

  templateSnippets:
    # Emit one backend per labeled ConfigMap (matches backends-*).
    backends-configmap-routes:
      template: |
        {%- for cm in resources.configmaps.List() %}
        {%- if cm.metadata.labels["routing"] == "enabled" %}
        backend cm_{{ cm.metadata.namespace }}_{{ cm.metadata.name }}
            server app {{ cm.data["target"] }}
        {%- end %}
        {%- end %}

    # Emit one host.map entry per labeled ConfigMap (matches map-host-*).
    map-host-configmap-routes:
      template: |
        {%- for cm in resources.configmaps.List() %}
        {%- if cm.metadata.labels["routing"] == "enabled" %}
        {{ cm.data["hostname"] }} cm_{{ cm.metadata.namespace }}_{{ cm.metadata.name }}
        {%- end %}
        {%- end %}
```

```yaml
apiVersion: v1
kind: List
items:
  - apiVersion: v1
    kind: ConfigMap
    metadata:
      name: shop
      namespace: storefront
      labels: {routing: enabled}
    data:
      hostname: shop.example.com
      target: 10.0.1.10:8080
  - apiVersion: v1
    kind: ConfigMap
    metadata:
      name: blog
      namespace: content
    data:
      hostname: blog.example.com
      target: 10.0.2.20:8080
```

</div>

This example supplies its own minimal routing configuration. Its `host.map` maps
hostnames directly to backends; the bundled base library uses a different map
contract. For an example that extends the bundled chart, use the
[custom-CRD library](https://github.com/haproxy-haptic/HAPTIC/tree/main/examples/byo-crd).

<a id="library-architecture"></a>

Library dependencies determine which snippets can call one another. When writing
a library, use the [extension point reference](libraries/base.md#available-extension-points)
and the [merge order](#library-merge-order) above to choose where your snippets belong.

## See also

- [Annotations](./annotations.md) — which vendor annotation library covers which annotation prefix
- [Templating Guide](./templating.md) — writing your own snippets and templates
- [Chart Values Reference → Template Libraries](./reference.md#template-libraries) — every `controller.templateLibraries.*` value
