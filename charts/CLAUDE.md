# charts/ - Helm Chart Development

Development context for working with the HAProxy Template Ingress Controller Helm chart.

**No Lua-based features or workarounds, including optional or per-thread scripts.**
Use native HAProxy capabilities; see [the project-wide rule](../CLAUDE.md#no-lua-features-rule-4).

## Upgrades: no can-kicking (RULE)

**A fix for an upgrade path must be exercised by a test that keeps running after
the next release.** A migration that only the *current* newest-release upgrade
happens to hit is not fixed — it is deferred, and it becomes a special case in
the product that nothing runs.

The trap is specific and easy to walk into: `scripts/test-chart-upgrade.sh`
baselines on published releases, so pinning it to "the newest stable release"
retires the previous path's only test the day a new version ships. The
cert-manager Secret adoption in `templates/crd-upgrade-hook.yaml` exists for
0.1.0; had the guard tested only the newest baseline, releasing 0.2.0 would have
left that code in the chart with zero coverage, and the next person to touch the
webhook cert would have had no way to know it mattered. The guard therefore
iterates **every** published stable release, oldest first.

**What this rules out:**

- A version-specific migration whose only test is "upgrade from the newest
  release", when that newest release is about to move.
- "We'll drop the compatibility shim in the next major" with nothing pinning the
  date or failing when it is missed.
- A conditional keyed on a version or a cluster state that no suite ever puts
  the code into.
- Fixing the render for `helm upgrade` only, when `helm template`, `--dry-run`
  and GitOps server-side renders take a different path — `lookup` is empty in
  all three, so a `lookup`-based fix silently does not apply where most
  operators actually upgrade. Put the migration somewhere render-independent (a
  pre-upgrade hook) instead.

**When a migration is genuinely temporary**, say so where it is defined: name
the release that introduced it, the condition under which it becomes dead, and
the test that would fail if it were removed early. "Delete after 1.0" with no
failing test is a comment, not a plan.

Cost is not an argument here either — every published release the guard replays
is another ~20 minutes of CI, and that is cheaper than one operator discovering
their upgrade path was dropped two releases ago.

## Chart Architecture

### Library Loading and Merging

Helm helper definitions live in the unconditional `helm-helpers` library
dependency. Helm packages this source but does not duplicate dependency source
in its release Secret. Resource templates remain in the parent chart.

The chart uses a library-based architecture where multiple YAML files become one
effective configuration. **The chart no longer merges them.** It renders one
`HAProxyTemplateLibrary` per enabled library plus a single
`HAProxyTemplateConfig` for the operator's own config, and the controller merges
the set at startup in the order that config's `spec.libraryRefs` declares (see
ADR-0017). `CRD_NAME` carries one name and no ordering. Read the `$libraryFiles`
list inside `haptic.prepareLibraries` (`charts/helm-helpers/templates/_libraries.tpl`) for the
canonical order:

```
Merge Order (lowest to highest priority):
 1. base                    - Core HAProxy template and snippets
 2. kubernetes-backends     - Kubernetes Service/EndpointSlice backend capability
 3. ssl                     - HTTPS frontend, TLS certs, SSL passthrough infra
 4. ingress                 - Kubernetes Ingress support
 5. http-policies           - Shared HTTP authentication, quota, cache, and WAF engines
 6. gateway                 - Gateway API (optional watches resolved at runtime)
 7. gateway-policies        - Authentication and traffic policies for Gateway routes
 8. ingress-annotations-compat - Shared scaffold for Ingress vendor annotation libraries
 9. governance              - Declarative constraints over any watched resource
 10. haptic-annotations      - haproxy-haptic.org/* native vocabulary
11. haproxytech             - haproxy.org/* annotation compatibility
12. haproxy-ingress         - haproxy-ingress.github.io/* annotation compatibility
13. nginx-ingress           - nginx.ingress.kubernetes.io/* compat (disabled by default)
14. spoa-hub                - SPOA hub sidecar wiring (auto-enabled when sidecar is on)
15. vector                  - Vector sidecar config (reads earlier libraries' decisions)
16. controller.config.*     - User overrides from values.yaml (highest priority)
```

Each entry is a subchart under `charts/haptic/charts/<name>/`, holding either a
single `library.yaml` or an `_index.yaml` plus numbered fragments.

Library `_helm_load.enable` predicates select the emitted objects. Most use a
`controller.templateLibraries.<name>.enabled` flag. `http-policies` loads when
Gateway, native annotations, or the Ingress compatibility scaffold needs it;
`spoa-hub` also loads when the sidecar is enabled. The shared HTTP engines consume
normalized policy records. Resource adapters stay in their Gateway or annotation
libraries. The Ingress compatibility scaffold provides Ingress-scoped macros for
SSL passthrough and CIDR access control; see ADR-0003.

Annotation libraries keep playground-only classification data under `_migrationCoverage`. The playground build extracts one asset per source directly from those declarations; the generic underscore-key strip ensures they never enter a config or library CR.

Library objects are named `<controller.configName>-<library slug>`, with the operator's own config keeping the plain `controller.configName`. The names carry **no** ordering authority — order comes from the `spec.libraryRefs` list on the `HAProxyTemplateConfig` — so inserting a library never renames an existing object.

The frontend path-matching order is selected at base-load time by `controller.config.templatingSettings.extraContext.routing.regexMatchOrder` (`default` or `last`). When `last`, the base library's `_helm_load` swaps `templateSnippets.frontend-routing-logic` for the alternate `frontend-routing-logic-regex-last` variant defined in `base.yaml`. The alternate is unset before rendering so it never appears in the output.

**Loader logic** (`charts/helm-helpers/templates/_libraries.tpl`, `define "haptic.prepareLibraries"`):

The loader iterates a fixed ordered list of library files. The order is a system property and lives in `_libraries.tpl`. Per-library loading rules — enable predicates and any chart-time mutations of the parsed YAML — live next to the resources they parameterize, in each library's top-level `_helm_load:` block. Every underscore-prefixed top-level key is stripped before rendering, so neither `_helm_load` nor `ssl.yaml`'s `_test_tls_*` YAML-anchor scratch values reach an object.

```yaml
{{- define "haptic.prepareLibraries" -}}
{{- $context := .context }}
{{- $prepared := list }}
{{- range $file := include "haptic.libraryFiles" $context | fromYamlArray }}
  {{- $library := $context.Files.Get $file | fromYaml }}   {# or subchart / split-dir #}
  {{- $loadHints := $library._helm_load | default dict }}
  {{- if and (not $skip) (eq (tpl $loadHints.enable $context | trim) "true") }}
    # apply _helm_load.inject items, optionally gated by inject.when
    # apply _helm_load.unset items
    # apply haptic.filterTests universally (no-op for libs without _helm_skip_test)
    # strip every underscore-prefixed top-level key
    {{- $prepared = append $prepared (dict "name" <slug> "config" $library) }}
  {{- end }}
{{- end }}
{{- $_ := set .result "libraries" $prepared }}
{{- end }}
```

The loader accepts `dict "context" $ "result" $prepared` and writes the parsed
libraries into `$prepared.libraries`. Each call owns its maps so the config
emitter's removal of `statusPatch` cannot change RBAC or webhook generation.
`haptic.filterTests` mutates its library map directly; neither helper serializes
the library collection between processing steps.

**One helper drives both the objects and their order**: `haptic.prepareLibraries`
emits the `HAProxyTemplateLibrary` objects *and* the `spec.libraryRefs` list on the
`HAProxyTemplateConfig`, so there is no second list that could disagree with it.
`CRD_NAME` on the Deployment is the single config name and carries no ordering.
`library_loader_test.yaml` pins the emitted set.

**Where merge semantics live now.** `pkg/controller/conversion.MergeSpecs` merges
with `mergo.MergeWithOverwrite`, the exact call sprig's `mustMergeOverwrite` makes
against the same vendored mergo — so chart-time and controller-time semantics
match by construction. Maps deep-merge key-wise and lists replace. A duplicate
`templateSnippets` name across libraries still resolves to the later one, but the
controller now logs each override instead of resolving it silently.

**`haptic.watchedResourcesUnion`** gives the templates that must reason about the
whole watch set (the ClusterRole, the ValidatingWebhookConfiguration) the union
across every library plus the operator's, with the operator winning. It retains
the Helm-only `statusPatch` field, which the object emitter strips.

### `_helm_load:` Schema

Every library file declares its loading rules in a top-level `_helm_load:` block. This is the same convention as `_helm_skip_test` (per-test skip rule); both `_helm_*` keys are chart-time-only metadata and are stripped before the merged config is rendered.

```yaml
_helm_load:
  # Required: Helm template string evaluated by tpl. Library is loaded only if
  # the result trims to "true". Truthy bools render as "true" / "false" naturally;
  # for compound conditions wrap with {{ if ... }}true{{ else }}false{{ end }}.
  enable: '{{ .Values.controller.templateLibraries.foo.enabled }}'

  # Optional: list of injection operations applied to the parsed library AFTER
  # fromYaml, BEFORE merge. Each item:
  #   path: dotted path within the library to write
  #   value: tpl-evaluated string written at that path                     (one of)
  #   from:  dotted path within the library to copy from                   (the other)
  #   when: optional tpl-evaluated condition; injection skipped if not "true"
  inject:
    - path: watchedResources.foo.fieldSelector
      value: 'spec.fooClass={{ .Values.fooClass.name }}'

  # Optional: list of dotted paths to remove from the library AFTER injects,
  # BEFORE merge. Useful for stripping internal-only snippets (variant scaffolding).
  unset:
    - templateSnippets.foo-internal-variant
```

Real examples in the source:

- `charts/haptic/charts/ingress/library.yaml` — simple `enable` + one `inject` for the dynamic `ingressClassName` field selector.
- `charts/haptic/charts/gateway/` — values-only `enable` and `inject`s for the gateway and gateway-class field selectors. The controller resolves optional resources and their `requires` dependencies at runtime.
- `charts/haptic/charts/base/library.yaml` — `enable` + the `controller_services` label-selector inject + a conditional `from:`-style inject that swaps `frontend-routing-logic` to its `-regex-last` variant when `controller.config.templatingSettings.extraContext.routing.regexMatchOrder=last`, and `unset` that always strips the alternate variant from output.
- `charts/haptic/charts/spoa-hub/` — compound `enable` (explicit flag OR derived from `haptic.spoaHub.enabled` helper).

Adding a new library: add a subchart under `charts/haptic/charts/<name>/` with a `library.yaml` (or an `_index.yaml` plus fragments), declare it in `Chart.yaml` `dependencies` with its `condition:`, give the library a `_helm_load:` block, and append `"subchart:<name>"` to the `$libraryFiles` list inside `haptic.prepareLibraries` in `_libraries.tpl`. The loader does not need a new branch, and the new library gets its own `HAProxyTemplateLibrary` object and its own `spec.libraryRefs` entry automatically. Update the expected object count in `tests/library_loader_test.yaml`, and add the subchart name to `values.schema.extras.yaml` (see "Values schema" below).

See ADR-0002 for the rationale (centralized vs decentralized loading rules).

### Library-declared `extraContext` defaults

A library may declare default values for the `templatingSettings.extraContext` parameter bag its own snippets read. Put them at the top level of the library file (or `_index.yaml` for a split library):

```yaml
templatingSettings:
  extraContext:
    nginxHttpRedirectCode: "308"   # consumed by this library's ssl-redirect snippet
```

The controller merges these beneath the config's `templatingSettings.extraContext` at the **lowest precedence** — an operator override via `controller.config.templatingSettings.extraContext.<key>` and any chart-computed key both win. They are also the lowest layer validationTests render with. Snippets read the value the usual way, keeping a literal fallback so the snippet still works if the key is somehow absent (e.g. the library is loaded standalone in a test):

```scriggo
{%- var code = extraContext | dig("nginxHttpRedirectCode") | fallback("308") | tostring() %}
```

Use this for a library-global tunable that mirrors an upstream controller's global ConfigMap setting (the `nginxHttpRedirectCode` ↔ ingress-nginx `http-redirect-code` case is the canonical example). It is **not** for per-resource values — those come from annotations on the resource. The declaration is dropped from the output for a disabled library (its subchart is pruned), so the snippet's `fallback()` is what applies then.

See ADR-0002 for the rationale (centralized vs decentralized loading rules).

The loader also projects every `_migrationCoverage` block into
`extraContext.knownAnnotations.<source>` (`prefixes` plus the declared names
without prefix). `features-955-annotation-unknown-key` in
ingress-annotations-compat reads it to record an `UnknownAnnotation` Event for an
Ingress annotation under an enabled library's prefix that the coverage map
doesn't declare — so a new annotation handler needs its coverage entry for this
reason too, not only for `check-migration-coverage.sh`.

### Operator-facing values default to `extraContext` (RULE)

**A new operator-facing `values.yaml` knob lives under `controller.config.templatingSettings.extraContext.*` by default. Placing one at the values root (or any other top-level key) requires an explicit justification, stated in the value's `values.yaml` comment.**

If a knob's only job is to change what the templates render — a cipher list, an HSTS toggle, a session-ticket switch, a routing mode — it belongs under `extraContext`, grouped with its siblings (e.g. `extraContext.tls.*`). That is where the render engine reads it and where every other render knob already lives; splitting one out to the root fragments the TLS/render surface and forces a translation shim in `templates/haproxytemplateconfig.yaml`. The canonical mistake-and-fix is `tlsSessionTickets.enabled` → `extraContext.tls.sessionTickets.enabled` (MR !1377): it was a pure render toggle wrongly hoisted to the root, needing a shim, until it was moved back under `extraContext.tls`.

**The one accepted justification for a root-level knob is that it does more than feed the render context — the chart creates Kubernetes resources from it.** Then the root-level value is the resource-lifecycle surface, and you project only its render-facing sub-fields into `extraContext`:

- `defaultSSLCertificate` (root) — the chart renders a cert-manager `Certificate` / self-signed `Issuer` / `Secret` from it; only its render-facing projection `extraContext.tls.defaultCertificate` (`name`/`namespace`/`ecdsaName`) reaches the config. `templates/haproxytemplateconfig.yaml` does that projection.
- `cache` / `rateLimit` / `spoaHub` — deploy workloads (Varnish, Valkey, the SPOA sidecar).
- `ingressClass` / `haproxy` — chart-owned resources (IngressClass, the HAProxy Service/pods).

Test before hoisting a value to the root: *does anything other than a template read it?* If the answer is "no, a snippet reads it and that's all", it is misplaced — put it under `extraContext`.

### An operator-settable `extraContext` key is never a list of entities (RULE)

**Helm and mergo replace lists wholesale.** An operator who adds one entry to a list-valued key silently drops every entry the chart shipped — and finds out when the feature they never touched stops working. Use a **keyed map**: the key names the entry and appears in error messages instead of an index.

```yaml
# Wrong — the operator's rule replaces the chart's, silently.
governance:
  rules:
    - resource: ingresses
      path: metadata.annotations['x']

# Right — the two merge; either side can be switched off alone.
governance:
  rules:
    my-rule:
      enabled: true
      resource: ingresses
      path: metadata.annotations['x']
```

**Add a per-entry `enabled` only when the chart itself ships entries** the operator may need to switch off (`governance.rules`, `extraContext.prometheusExporter.excludeMetrics`) — and then **require** it, so a typo fails the render instead of leaving the entry inert. When the chart ships an empty map, **presence is the enable**; a second flag creates exactly the inert-catalog trap `docs/site/docs/reference.md` rules out for `waf.policies.*` ("there is no second enable flag that can leave a configured catalog inert"). `accessLog.targets` and `waf.policies.configMapRefs` follow that shape; `waf.policies.inline` is the original.

**Where the render wants an ordered list, resolve the map with sorted `keys()`** — chart-side in `haproxytemplateconfig.yaml`, or snippet-side. Map iteration is unordered, so without the sort the rendered config churns between renders and every downstream consumer sees a spurious change.

**A map cannot express "empty" through a merge.** `{}` merged over a non-empty map is a no-op. Two consequences, both load-bearing:

- A guard that rejects an *explicitly empty* value can only fire where the operator's input is read directly — in Helm, not in the render. Keep the render-side guard for the CR-direct path, but expect the Helm guard to be the one with test coverage.
- A `validationTest` (or `_global`) that needs to CLEAR a map must use the runner's `__replace__: true` sentinel (`pkg/controller/testrunner/rendering.go`), not `{}`. `_global`'s `governance.rules` and the WAF tests' `configMapRefs` both depend on this — without it an operator's production entries leak into every bundled test and the load gate rejects their config.

**Two exceptions stay lists:**

- **Scalar-value lists**, where the list *is* the value: `governance.exemptNamespaces`, `governance.rules.<n>.namespaces`/`exemptNamespaces`, `waf.policies.inline.<n>.allowedMethods`, `crsSettings.allowedRequestContentTypes`, `spoaHub.haproxy.messages`, `haproxyService.loadBalancerSourceRanges`.
- **Lists inside a document that must round-trip through a non-Helm source.** `waf.policies.inline.<n>.ruleExclusions` is authored identically in `values.yaml`, a trusted ConfigMap catalog and a self-service catalog; only the first is Helm-merged, so a keyed map buys nothing and costs parity.

Chart-generated projections already fronted by a values.yaml keyed map (`spoaHub.plugins`, `haproxyService.ports`) are already correct — the operator never edits the list.

### Values schema (`values.schema.json`)

`make chart-schema` generates `charts/haptic/values.schema.json` with
[helm-values-schema-json](https://github.com/losisin/helm-values-schema-json)
from `values.yaml` and `values.schema.extras.yaml`. Never edit the JSON;
`make chart-schema-check` (part of `lint-chart`) fails when it drifts.

- **Every object is closed** (`additionalProperties: false`). Mark a free-form map
  or a Kubernetes passthrough (labels, `resources`, `affinity`, …) in values.yaml
  with `# @schema skipProperties; additionalProperties: true`.
- **Types come from the default.** Annotate where the default can't show the
  accepted types: `type: [integer, null]`, a tpl string (`type: [boolean, string]`),
  an empty list's `item:`.
- **A key with no default** (optional, commented out in values.yaml) goes in
  `values.schema.extras.yaml` with a sample value of its type.
- **A removed or renamed key** goes in the extras as `anyOf: [{}]`, and its
  `fail()` guard names the replacement. Helm validates the schema before any
  template runs, so a key the schema rejects never reaches its guard.
- **Helm guards don't repeat the schema.** No guard for an unknown field, a plain
  type, or a bare enum; guards carry ranges, cross-field rules, and messages that
  explain more than the allowed values.
- **A new `Chart.yaml` dependency** needs its name in the extras: Helm adds
  `<dependency>: {global: …}` to the parent's values before validating them.

### Split-library directories

A library that has grown past comfortable one-file size may live as a directory of fragments instead of a single YAML file. The convention (see ADR-0008):

- The subchart holds an `_index.yaml` plus fragments instead of a single `library.yaml`; its `$libraryFiles` entry stays `"subchart:foo"` either way.
- Inside the directory, `_index.yaml` is required and acts as the load-rule authority — it carries the `_helm_load` block and any small structural pieces (typically `watchedResources`).
- All other YAML files at the top level, plus any YAML files one level deep (e.g. `tests/foo.yaml`), are fragment files. Fragments contribute entries to `templateSnippets`, `validationTests`, `k8sResources`, etc., but must NOT carry their own `_helm_load` block.
- Fragments merge into the per-library accumulator in lexicographic order (numeric prefixes like `10-features.yaml` are the idiomatic ordering hint) before inject/unset/strip/cross-library merge runs. Each `templateSnippets` / `validationTests` / `k8sResources` entry must be declared in exactly one fragment — duplicates would have the lexicographically-later file win, silently.
- Glob depth is one level (Helm's `Files.Glob` doesn't recurse). One optional `tests/` subdirectory per split library is the supported convention; deeper nesting is not.

### Library Knowledge Hierarchy

Libraries form a dependency hierarchy - each library may only reference snippets and variables from libraries it "knows about":

```
Level 0: base.yaml
         │
         ├── Knows: nothing (completely resource-agnostic)
         │
Level 1: kubernetes-backends.yaml, ssl.yaml
         │
         ├── Know: base
         ├── kubernetes-backends owns the Service/EndpointSlice watch aliases
         │   and translates them into base's generic Backend records
         │
Level 2: ingress.yaml, gateway/
         │
         ├── Know: base, kubernetes-backends, ssl
         ├── Don't know: each other
         │
Level 2.5: ingress-annotations-compat.yaml
         │
         ├── Knows: base, kubernetes-backends, ingress
         ├── Provides: parameterized macros for patterns shared across the
         │             three Ingress vendor annotation libraries below
         │             (currently SSL passthrough scan + CIDR allow/deny
         │             ACL emission)
         ├── Scope: Ingress only — macros walk `resources.ingresses.List()`
         │             or take typed `*resources.ingresses.T` parameters.
         │             Vendor libraries for non-Ingress CRDs write their
         │             own equivalents.
         │
Level 3: haproxy-ingress/, haproxytech.yaml, nginx-ingress/
         │
         ├── Know: all libraries above (including ingress-annotations-compat)
         └── Don't know: each other
```

This hierarchy prevents circular dependencies and ensures predictable behavior during library merging. Violating the hierarchy (e.g., base.yaml referencing ingress-specific snippets) will cause runtime errors when that library is disabled.

### Library Structure

Each library file (`charts/haptic/charts/<name>/library.yaml`, or `_index.yaml` plus fragments) contains:

```yaml
watchedResources:
  # Resources this library needs to watch
  ingresses:
    apiVersion: networking.k8s.io/v1
    resources: ingresses
    indexBy: ["metadata.namespace", "metadata.name"]

haproxyConfig:
  # ONLY base.yaml should define this
  # Other libraries will override it if they include this section!
  template: |
    # Full HAProxy configuration template

templateSnippets:
  # Reusable template snippets
  resource_ingress_backend-name:
    template: >-
      ing_{{ ingress.metadata.namespace }}_{{ ingress.metadata.name }}

validationTests:
  # Embedded validation tests for this library
  test-ingress-basic:
    description: Basic ingress routing
    fixtures: ...
    assertions: ...
```

The `haproxyConfig` section also supports a `postProcessing` list that transforms rendered output. Available types: `regex_replace` (line-by-line regex find/replace) and `template` (Scriggo template transformation with `input` variable). See `pkg/templating/README.md` for details.

### Plugin Pattern

Libraries use a **plugin pattern** where base.yaml defines extension points:

```yaml
# base.yaml
haproxyConfig:
  template: |
    frontend http-in
      # Extension point for routing backends
      {% include "resource_ingress_backends" %}
      {% include "resource_gateway_backends" %}
```

Libraries implement these extension points:

```yaml
# ingress.yaml
templateSnippets:
  resource_ingress_backends:
    template: |
      {%- for ingress in resources.ingresses.List() %}
      # Generate backends from ingress resources
      {%- endfor %}
```

**Critical Rule**: Libraries should ONLY provide `templateSnippets`, not override `haproxyConfig`. The base template calls your snippets via `{% include %}`.

**CRITICAL ARCHITECTURE RULE - base.yaml MUST Be Resource-Agnostic** (RULE #1, chart-side):

This is the chart-side mirror of the project-wide rule in the root `CLAUDE.md` ("Resource-Agnostic Design (RULE #1)"). The litmus test for the whole project: **if an operator decided to use some CRD instead of Gateway or Ingress resources, they should only need to touch HAPTIC templates and config — no Go code. Writing templates for the operator's CRD must be just as comfortable as for Ingress or Gateway API resources. There must be no preferential treatment for well-known resources.**

The Go-side guarantee (engine ignores all resource specifics; schemas come from the apiserver at runtime) puts the burden of resource-aware behaviour entirely on chart-side libraries — and the chart in turn must keep that burden out of `base.yaml`.

**base.yaml MUST be completely resource-agnostic**. It must NOT access:

- `ingress.metadata.*`, `ingress.spec.*`
- `httproute.metadata.*`, `httproute.spec.*`
- `grpcroute.metadata.*`, `grpcroute.spec.*`
- `resources.services`, `resources.endpoints`, or any other watched-resource alias
- Any other resource-specific fields or annotations

The bundled resource-specific libraries (`kubernetes-backends.yaml`, `ingress.yaml`, `gateway/`, `haproxytech.yaml`, `haproxy-ingress/`, `nginx-ingress/`) are illustrative implementations of the pattern; an operator using a different CRD writes their own library file alongside, and `base.yaml` consumes its output through the same shared-context seam without modification.

Resource-specific libraries (ingress.yaml, gateway/, haproxytech.yaml) are responsible for:

1. Extracting annotations and resource-specific data
2. Performing resource-specific calculations
3. Setting generic context variables for base.yaml to consume

**Pattern**: Resource libraries extract data and write into a shared
`serverOpts` map → base.yaml reads back from the same map.

**Example** (real shape; see `haproxytech.yaml`'s
`backend-directives-100-haproxytech-pod-maxconn` snippet for the production
version):

```scriggo
{#- ingress.yaml or haproxytech.yaml (resource-specific): typed access when
    the macro parameter is typed (`ingress *resources.ingresses.T`), or
    dig + fallback when ingress arrived as `any` (polymorphic boundary).
    Stash the per-pod value on the `serverOpts` map base.yaml hands every
    snippet. -#}
{%- var podMaxconn = ingress.Metadata.Annotations["haproxy.org/pod-maxconn"] | fallback("") | tostring() %}
{%- if podMaxconn != "" %}
  {%- var perPod = calculate_per_pod_value(podMaxconn) %}
  {% serverOpts["maxconn"] = perPod -%}
{%- end %}

{#- base.yaml (resource-agnostic): consume the map entry. The key is absent
    when the snippet didn't write it, so check existence with the comma-ok
    form rather than Jinja's `is defined`. -#}
{%- var perPod, ok = serverOpts["maxconn"] %}
{%- if ok %}
  server {{ endpoint.pod }} {{ endpoint.address }}:{{ endpoint.port }} maxconn {{ perPod }}
{%- end %}
```

Notes for readers coming from Jinja: Scriggo declares with `{% var %}`
(or `:=`), terminates blocks with `{% end %}` (no `{% endif %}` /
`{% endfor %}`), and has no `is defined` — use the Go `value, ok :=` idiom
or `dig | fallback` for safe map access. Resource objects arrive as
typed pointers (`*resources.ingresses.T`, `*resources.httproutes.T`, …)
when a schema is loaded for the kind; in that case use dot-field access
(`ingress.Metadata.Annotations[...]`). Resources still come in as `any`
at polymorphic boundaries (untyped macro parameter, `routeInfo["route"]`,
`shared.Get(...)` returns); in those spots reach for `dig(...)` instead.
See the "Typed Resource Access" section below for the full pattern set.

**Why This Matters**: This separation allows Gateway API and Ingress resources to coexist without base.yaml needing to know which resource type it's processing. Resource-specific logic stays in resource-specific libraries.

### Extension Point Reference

The base template uses `render_glob` to discover and render snippets from all libraries. Snippets are rendered in alphabetical order, so numeric prefixes control execution order.

Every extension point whose output is lines wraps its glob in `on_own_lines(render_glob "x-*")`, so a member needs no leading or trailing newline. Add a new extension point the same way, normally after `{{-`: `on_own_lines` treats the text before it as an unfinished line, so after a finished one a member that starts with content gets a blank line before it. Map files keep `{{ ... -}}` because their header comment must end its line even when the glob is empty.

In addition to the snippet-based extension points below, libraries may declare full Kubernetes resources via the top-level `k8sResources:` map (sibling of `templateSnippets:`, `maps:`, `files:`, `sslCertificates:`). Each entry is a Scriggo template with full engine context (`resources`, filters, snippets, `fileRegistry`, `extraContext`, `shared`); the rendered output is parsed as one or more YAML documents (multi-doc supported via `---`) and applied via Server-Side Apply with field manager `haptic`. The controller injects an `OwnerReference` to the `HAProxyTemplateConfig` CR (`controller=true`, `blockOwnerDeletion=true`) so cascade-delete (e.g. `helm uninstall`) removes the resources. Use this for resources whose shape derives from listener / Ingress state — the `k8sResources.haproxy-service` entry in `base.yaml` is the canonical example. The `haproxy-haptic.org/ownership: partial` annotation on a rendered resource opts into partial-ownership SSA (no `managed-by` label, no orphan-cleanup tracking) for objects shared with another field manager.

| Pattern | Purpose | Contributing Libraries |
|---------|---------|----------------------|
| `global-settings-*` | Global section directives (logging, process, paths, SSL) | base |
| `global-top-*` | Top-level sections after global (userlist, resolvers) | haproxytech |
| `defaults-settings-*` | Defaults section directives (options, balance, timeouts, errorfiles) | base |
| `features-*` | Feature registration (SSL, TLS certs) | gateway, haproxytech, ingress, ssl |
| `backends-*` | Backend definitions | gateway, ingress, ssl |
| `default-backend-*` | Optional `default_backend` declaration; base emits its gRPC/404 fallback when every contribution is empty | kubernetes-backends, user libraries |
| `frontends-*` | Additional frontends (HTTPS, TCP) | ssl |
| `http-bind-extra-*` | Additional HTTP-frontend `bind *:<port>` directives (Gateway HTTP listener ports, the PROXY-protocol port) | base, gateway |
| `https-bind-extra-*` | Additional HTTPS-frontend `bind *:<port> ssl crt-list ...` directives (Gateway HTTPS listener ports, the PROXY-protocol port) | gateway, ssl |
| `ssl-tcp-bind-extra-*` | Additional `frontend ssl-tcp` binds. Only rendered when TLS-Passthrough puts the HTTPS port on the SNI-routing frontend instead of `frontend https` — use it for anything that must attach to the wire connection | ssl |
| `frontend-routing-listener-port-*` | Per-listener-port frontend routing logic (Gateway listener ports) | gateway |
| `frontend-extra-*` | Early frontend directives after bind (options, captures, ACLs) | (user) |
| `frontend-matchers-advanced-*` | Advanced route matching (method, headers) | gateway |
| `frontend-filters-*` | Request/response filters (after routing) | gateway, haproxytech |
| `forwarded-for-mode-*` | Rules setting `txn.xff_mode` (`replace`, `append`, `ifmissing`, `ignore`) for the requests a library claims; base renders them first in the filter chain and applies the mode to `X-Forwarded-For` | ingress-annotations-compat |
| `log-fields-*` | Named JSON fields for the structured access log; emits log-format items only, no directives. Gate on the feature being configured; only log-time-available fetches are legal (no `path`/`req.hdr()`/`res.hdr()` — materialise into a `txn` var first); never branch on which frontend is rendering | base, ssl, gateway, haptic-annotations, haproxytech, haproxy-ingress, nginx-ingress, spoa-hub |
| `backend-directives-*` | Backend configuration directives | haproxytech |
| `map-host-*` | Host map entries | gateway, ingress |
| `map-hostregex-*` | Regex host map entries (wildcard hostnames) — distinct prefix from `map-host-*` so the exact-host glob doesn't subsume it. Listener-scoped `frontend-routing-host-match-*` lookups read this map too | gateway |
| `map-hostaliasregex-*` | Whole-hostname alias regexes (`^(?:<regex>)$`), read only by the unscoped cascade so an alias can't match a listener-scoped lookup | haptic-annotations, haproxy-ingress |
| `map-path-exact-*` | Exact path map entries | gateway, ingress |
| `map-path-prefix-*` | Prefix path map entries | gateway, ingress |
| `map-pfxexact-*` | Prefix-exact map entries | gateway, haproxy-ingress, ingress |
| `map-path-regex-*` | Regex path map entries | gateway, ingress, haproxy-ingress |
| `map-weighted-backend-*` | Weighted routing map | gateway |
| `status-extra-*` | Status frontend directives (Prometheus exporter, custom endpoints) | base |
| `status-patches-*` | Status patch registration (side effects only) | gateway, ingress |

**Extension Point Variable Passing:**

Not every extension point inherits the caller's locals — this is the most common source of "why is my variable nil" snippet bugs. Only `backend-directives-*` passes locals via `inherit_context`; the frontend and feature points render in a shared scope where the snippet reads globals (`resources`) or the shared context (`shared.Get(...)`) itself. Check the actual `render_glob` call before assuming a variable is in scope.

| Extension Point | Available variables | How they reach the snippet |
|-----------------|---------------------|----------------------------|
| `backend-directives-*` | `ingress`, `serverOpts`, `serviceName`, `port` | `inherit_context`, from the per-backend loop in `backends-500-ingress` (`ingress.yaml`) |
| `frontend-filters-*` | none inherited — use globals | rendered by a bare `render_glob "frontend-filters-*"` (no `inherit_context`) **once per frontend** (plain HTTP, h2c, and the SSL `https` frontend), after routing and before backend selection. There is no `ingress`/`rule`/`path`; a snippet that needs per-Ingress data iterates `resources.ingresses.List()` itself (see `frontend-filters-300-request-id` in the playground `extend` preset) |
| `features-*` | `globalFeatures` (conventionally bound to `gf`) | via `shared.Get("globalFeatures")` inside the snippet, **not** `inherit_context` — this glob renders once for side effects (feature registration) |

**Example - backend-directives extension:**

```scriggo
{#- In backends-500-ingress (ingress.yaml) #}
{%- var serverOpts = map[string]any{"flags": []any{}} %}
{{- render_glob "backend-directives-*" inherit_context }}
{{ BackendServers(tostring(serviceName), 0, toint(port), serverOpts) }}

{#- In backend-directives-900-haproxytech (haproxytech.yaml) #}
{#- These variables are available via inherit_context: #}
{%- if ingress != nil %}
  {#- ingress: the current Ingress resource #}
  {#- serverOpts: map for accumulating server options #}
  {%- var snippet = ingress | dig("metadata", "annotations", "haproxy.org/backend-config-snippet") %}
{%- end %}
```

**What a backend directive costs — the three lanes.** Base's `PrepareBackend`
sorts every line a `backend-directives-*` snippet renders before it decides a
backend's shape (`charts/haptic/charts/base/library.yaml`, `ProfileDirective`):

| Lane | What lands there | Reload cost on HAProxy 3.4 |
|---|---|---|
| Comment (`# ...`) | Recorded on the plan record so a change explains the text | none, never shapes the backend |
| Profile | `timeout *`, `fullconn`, `retries`, `retry-on`, `compression *`, `cookie`, `dynamic-cookie-key`, `option httpchk`, `http-check`; also `serverOpts["profile"]`, `balance`, `hashType` | none while a profile with the same values exists; the first route with a new combination reloads once to add it |
| Frontend map | a uniform rule block per HTTP frontend reading `<ns>/<name>`-keyed maps (`frontend-filters-858..899` in haptic-annotations: JWT, API key, HMAC, consumer groups, rate and bandwidth limits, route-unavailable) | none; literals the block must spell out (a header name, a key file, a rate window) reload once when new. The block itself is gated on `incremental_value_count(<group>, "any") > 0`: it appears with the first route using the feature and leaves with the last, one reload each way, so an unused feature costs no rule evaluation per request |
| Body | everything else: `filter`, `stick-table`, `http-request`/`http-response` rules, `acl`, raw injections | the backend is structural: add and remove reload |

`serverOpts["flags"]` (`default-server` keywords `add server` accepts) and the
per-backend maps (`backend-timeouts.map`, `ing-reshdr.map`) are runtime-only.
So before a new snippet emits a line, ask which lane it lands in: a setting HAProxy
accepts in a `defaults` section costs nothing, a map-driven uniform line costs
nothing, and a per-route rule costs the route its dynamic shape. A per-route
feature is written as a `util-haptic-<feature>-route` macro resolving the
annotations, a `map-haptic-<feature>-routes-*` producer emitting one map line
per route, a `features-*-names` publisher for the distinct literals (a component
cannot both emit text and publish), and one `frontend-filters-*` block; HAProxy
takes converter arguments from variables (`jwt_verify`, `hmac`, `sub`,
`set-bandwidth-limit limit <expr>`) but header names, key files and table names
only as literals. A `filter`
cannot live in a defaults, which is why compression's filter sits on the
frontends (`frontend-filters-110-haptic-compression-filter`) while its settings
ride the profile; a body that declares another filter next to inherited
compression gets `filter compression` prepended by base.

### Snippet Priority Numbering

Snippets use numeric prefixes (e.g., `backends-500-ingress`) to control execution order within `render_glob` patterns. Lower numbers execute first.

**Reserved ranges:**

| Range | Purpose | Examples |
|-------|---------|----------|
| 000-099 | Infrastructure/initialization | `features-050-ssl-initialization` |
| 100-199 | Feature registration, basic config | `features-100-gateway-tls`, `frontend-filters-100-server-header` |
| 200-299 | Access control, security | `frontend-filters-200-haproxytech-access-control` |
| 300-399 | CORS, header manipulation | `frontend-filters-300-haproxytech-cors` |
| 400-499 | Redirects, rewrites | `frontend-filters-450-haproxytech-redirects` |
| 500-599 | Core functionality | `backends-500-ingress`, `map-host-500-gateway` |
| 600-699 | Compatibility layers | `map-path-regex-600-haproxy-ingress` |
| 700-799 | Compatibility layers (nginx) | `backend-directives-700-nginx-ingress-timeouts` |
| 900-999 | Finalization, cleanup | `frontend-matchers-advanced-900-path-match` |

### Snippet Implementation Patterns

**Use macros (import pattern)** when:

- Snippet produces output that needs parameters
- Called multiple times with different inputs
- Output is inline within another template

```scriggo
{# Definition in util-backend-name-ingress #}
{% macro BackendNameIngress(ingress any, path any) string %}
  ...
{% end %}

{# Usage #}
{% import "util-backend-name-ingress" for BackendNameIngress %}
{{ BackendNameIngress(ingress, path) }}
```

**Use shared variables (render pattern)** when:

- Expensive computation should run once per render
- Result needed by multiple unrelated snippets
- Caching across template boundaries required

```scriggo
{# Definition in util-gateway-analysis #}
{%- var _, _ = shared.ComputeIfAbsent("gatewayAnalysis", func() any {
    return expensive_computation()
}) %}

{# Usage from any snippet — re-fetch (and re-cache if needed) in one call. #}
{{ render "util-gateway-analysis" }}
{%- var ga, _ = shared.ComputeIfAbsent("gatewayAnalysis", func() any {
    return expensive_computation() {# only runs if util- snippet wasn't rendered first #}
}) %}
```

## Naming access-log fields and span attributes (RULE)

Three namespaces, in priority order. Apply the first that matches.

**1. A semantic-convention name, if one exists.** OpenTelemetry wins over both
prefixes below: `http.route`, `url.path`, `server.address`, `server.port`,
`tls.protocol.version`, `tls.client.subject`, `network.local.address`,
`network.protocol.version`, `k8s.pod.name`, `user.id`. Check the
[registry](https://opentelemetry.io/docs/specs/semconv/registry/attributes/)
before inventing a name — it covers more than people expect, including TLS
client certificates and Kubernetes objects.

**2. `haproxy.` when you need HAProxy's documentation to interpret the value.**
`haproxy.term_state` (you cannot decode `----` without it), `haproxy.time.*`
(`%Tr` stops at the response headers, `%Td` does not — a distinction that has
already caused one bug), `haproxy.retries`, `haproxy.mtls_verify` (an
`ssl_c_verify` result code), and `haproxy.backend` / `server` / `frontend`,
whose strings HAPTIC generates but which denote objects in HAProxy's proxy
model.

**3. `haptic.` when you need HAPTIC's documentation.** `haptic.resource`,
`haptic.req_id`, `haptic.denied_by`, `haptic.waf_*`, `haptic.rate_limit_*`,
`haptic.schema_outcome`, `haptic.cache`, `haptic.gw_route`,
`haptic.instance_pod`.

**The test is whose docs you need, NOT whether plain HAProxy could produce the
value.** That weaker test collapses, because nearly everything could:
`haptic.req_id` is `%ID` from a stock `unique-id-format`, yet the contract you
actually rely on — a UUIDv7, never embedding a client IP, adopted from an
inbound header only when well-formed — is HAPTIC's, and HAProxy's manual says
none of it.

**A `haptic.` attribute is named `haptic.` + its exact access-log field name.**
`waf_action` → `haptic.waf_action`. Keeping the two identical is what lets you
move from a log line to a span attribute without a translation table. Rules 1
and 2 do not follow this — those names are fixed by the specification and by
HAProxy respectively, so the log keeps its own short name (`mtls_cn` →
`tls.client.subject`, `destination_ip` → `network.local.address`).

**Personal data does not go into spans at all.** No client IP, forwarded or
peer: a trace is retained and shared far more widely than an access log.
Correlate through `haptic.req_id`, which both carry.

## HAProxy File Path Requirements

### The `default-path origin` Directive

**CRITICAL**: the base library renders `default-path origin <baseDir>` into the global section so relative paths in the rendered config resolve to the right place at runtime *and* during validation.

```haproxy
global
    default-path origin {{ pathResolver.GetBaseDir() }}   # → /etc/haproxy in production
    # ... other global settings
```

The directive lives in the `global-settings-300-paths` snippet of `charts/haptic/charts/base/library.yaml` (around line 1769). It tells HAProxy to resolve relative paths from the explicit base directory passed as an argument, **not** from the config file's directory or HAProxy's working directory. We use `origin <baseDir>` rather than `default-path config` because the validation pipeline rewrites this single directive (replacing the production base with a per-call temp dir) instead of mutating every file path in the rendered config.

### How Path Resolution Works

**Production:**

```haproxy
global
    default-path origin /etc/haproxy

defaults
    errorfile 400 general/400.http   # → /etc/haproxy/general/400.http
```

**Validation:** `pkg/controller/validation/service.go` rewrites `default-path origin <baseDir>` → `default-path origin <tempDir>` once per call (`strings.Replace(..., 1)`), then writes the auxiliary tree under `<tempDir>` mirroring the production layout. Relative paths in the rest of the config are unchanged, so the same rendered output works in both contexts.

**Without the directive (broken):**

```haproxy
# HAProxy resolves paths from its working directory (usually /)
errorfile 400 general/400.http    # Looks for /general/400.http — NOT FOUND
```

### Single Render with Relative Paths

The codebase uses **relative paths** that work in every consumer:

1. **RenderService** (`pkg/controller/renderer/service.go`) constructs a `PathResolver` whose `MapsDir` / `SSLDir` / `CRTListDir` / `GeneralDir` are the **basenames** of the production directories (e.g. `maps`, `ssl`, `general`), and whose `BaseDir` is the parent (e.g. `/etc/haproxy`).
2. **Templates** call `pathResolver.GetPath(name, type)` which returns relative paths like `general/400.http`, `maps/host.map`.
3. **ValidationService** writes the auxiliary tree under a per-call temp directory and patches `default-path origin` to point at it.
4. **The agent** writes the rendered files under the production `BaseDir` on the pod, so the same relative paths resolve there too.

### PathResolver Construction

`NewRenderService` derives the resolver from `cfg.Dataplane`, using `filepath.Base` and `filepath.Dir` so a single relative-path layout falls out of whatever the operator configured for the production directories:

```go
pathResolver := &templating.PathResolver{
    BaseDir:    filepath.Dir(cfg.Config.Dataplane.MapsDir),    // /etc/haproxy
    MapsDir:    filepath.Base(cfg.Config.Dataplane.MapsDir),   // maps
    SSLDir:     filepath.Base(cfg.Config.Dataplane.SSLCertsDir),
    CRTListDir: filepath.Base(cfg.Config.Dataplane.GeneralStorageDir),
    GeneralDir: filepath.Base(cfg.Config.Dataplane.GeneralStorageDir),
}
```

Templates then use:

```scriggo
errorfile 400 {{ pathResolver.GetPath("400.http", "file") }}
{#- Output: general/400.http (chart default GeneralStorageDir basename) #}

use_backend %[path,map({{ pathResolver.GetPath("path.map", "map") }})]
{#- Output: maps/path.map #}
```

### ValidationService Directory Structure

`ValidationService` mirrors the production layout under a per-call temp directory:

```
/tmp/haptic-validation-xxx/
├── haproxy.cfg          # rendered config; default-path origin patched to point here
├── maps/
│   └── host.map
├── ssl/
│   └── cert.pem
└── general/
    ├── 400.http
    └── 504.http
```

`haproxy -c -f /tmp/haptic-validation-xxx/haproxy.cfg` then resolves every relative path against the patched `default-path origin`, so validation sees the same file layout HAProxy would in production.

### Common Pitfall

Do not remove or rename the `default-path origin` line in `base.yaml` — `ValidationService.ValidateWithChecksum` does an exact `strings.Replace(config, "default-path origin "+s.baseDir, "default-path origin "+tempDir, 1)`. If the directive is missing, or the rendered base differs from the configured `Dataplane.MapsDir` parent, the replacement silently no-ops and validation will look for files in the production paths inside a sandbox that doesn't have them.

## Development Workflow

### Testing Library Changes

A library is only meaningful merged with the others, so you must test the **merged output**, not individual library files. `helm template` now emits one object per library; `controller validate -f` merges every document in a stream, so piping the whole rendered set into it validates exactly what the controller would assemble. `--dump-merged` prints that merged spec without running any test.

**Recommended: Use the Test Script**

The `scripts/test-templates.sh` script automates the correct workflow (helm template + yq + controller validate):

```bash
# Run all validation tests
./scripts/test-templates.sh

# Run specific test
./scripts/test-templates.sh --test test-httproute-method-matching

# Run test with debugging output
./scripts/test-templates.sh --test test-httproute-method-matching --dump-rendered --verbose

# Show all available tests
./scripts/test-templates.sh --output yaml | yq '.tests[].name'
```

**Why use the script?**

- Ensures you don't forget the helm template step
- Extracts the config and its referenced libraries, and supplies offline schemas
- Handles error checking and temp file cleanup
- Provides helpful error messages

**Manual Workflow (Advanced)**

For custom values, retain both the config and its referenced libraries:

```bash
helm template haptic charts/haptic \
  --set controller.templateLibraries.ingress.enabled=true \
  --set controller.templateLibraries.gateway.enabled=false \
  | yq 'select(.kind == "HAProxyTemplateConfig" or .kind == "HAProxyTemplateLibrary")' \
  > /tmp/ingress-config.yaml

make build
./bin/haptic validate -f /tmp/ingress-config.yaml --schema-dir tests/schemas \
  --test test-ingress-duplicate-backend-different-ports
```

Filtering out the libraries leaves unresolved `libraryRefs`. The schema bundle is
also required for typed access in the bundled templates during offline validation.

**Gateway API tests**

Gateway availability is resolved at runtime from the configured `apiVersions`,
`optional`, and `requires` fields. Helm's `--api-versions` doesn't select the
controller's effective configuration. Run a focused test through the wrapper:

```bash
./scripts/test-templates.sh --test test-httproute-method-matching
```

The full wrapper run also exercises reduced schema bundles. When changing Gateway
API support, regenerate dependency metadata and verify older-release coverage as
described in the [upgrade playbook](../docs/site/docs/development/gateway-api-upgrade-playbook.md).

### Testing specific libraries

To test Gateway API's experimental fields alongside the default libraries, render
both configuration kinds and validate with the schema bundle:

```bash
helm template haptic charts/haptic \
  --set controller.templateLibraries.gateway.enabled=true \
  --set controller.templateLibraries.gateway.experimentalChannel=true \
  | yq 'select(.kind == "HAProxyTemplateConfig" or .kind == "HAProxyTemplateLibrary")' \
  > /tmp/gateway-config.yaml

./bin/haptic validate -f /tmp/gateway-config.yaml --schema-dir tests/schemas
```

For custom values, pass the file to Helm before validating its output:

```bash
helm template haptic charts/haptic --values my-test-values.yaml \
  | yq 'select(.kind == "HAProxyTemplateConfig" or .kind == "HAProxyTemplateLibrary")' \
  > /tmp/custom-config.yaml

./bin/haptic validate -f /tmp/custom-config.yaml --schema-dir tests/schemas
```

### Adding Validation Tests to Libraries

Libraries can include validation tests that are merged into the final config:

```yaml
# ingress.yaml
validationTests:
  test-ingress-basic:
    description: Basic ingress routing
    fixtures:
      services:
        - apiVersion: v1
          kind: Service
          metadata:
            name: my-service
            namespace: default
          spec:
            ports:
              - port: 80
      endpoints:
        - apiVersion: discovery.k8s.io/v1
          kind: EndpointSlice
          metadata:
            name: my-service-abc
            namespace: default
            labels:
              kubernetes.io/service-name: my-service
          endpoints:
            - addresses: ["10.0.0.1"]
          ports:
            - port: 8080
      ingresses:
        - apiVersion: networking.k8s.io/v1
          kind: Ingress
          metadata:
            name: my-ingress
            namespace: default
          spec:
            ingressClassName: haproxy
            rules:
              - host: example.com
                http:
                  paths:
                    - path: /
                      pathType: Prefix
                      backend:
                        service:
                          name: my-service
                          port:
                            number: 80
    assertions:
      - type: haproxy_valid
        description: HAProxy config must be valid

      - type: contains
        target: haproxy.cfg
        pattern: "backend default_my-ingress_svc_my-service_http"
        description: Must generate backend for ingress
```

**Test Execution:**

Tests run against the **merged configuration**, so they can validate cross-library interactions.

**Every rendering test is checked for determinism automatically.** The runner renders twice and compares the config and every auxiliary file, so a template whose output depends on Go's map-iteration order fails the suite that covers it rather than the one test whose author thought to ask. Iterate a map with `keys()`, never a bare `range` — a reordered map file or rule block is a changed file to the controller, costing a sync and a reload on a config nobody edited. The check only sees what the fixture can express — a map with one key cannot be reordered, so give a map-ordered site a fixture with at least two — and detection is probabilistic even then, since two renders can coincidentally agree.

**Assertions see chart defaults, never the operator's values.** A test renders with the libraries' `extraContext`, the config's `templatingSettings.testExtraContext`, `_global`, and its own `extraContext` — the chart computes `testExtraContext` from its default values (`haptic.testValues` in `charts/helm-helpers/templates/_extracontext.tpl`). An operator's `extraContext` reaches only a second render of each test's fixtures, which must succeed and pass `haproxy -c` but runs no content assertion. So assert on defaults freely; don't pin a value just because an operator might change it.

Four rules follow:

- **Pin the input policy your fixtures depend on.** That second render fails on any `fail()`, so a fixture an operator's stricter policy refuses (a password hash outside `annotationCompatibility.basicAuth.passwordHashValidation`) must carry the policy it was written for in the test's own `extraContext`.
- **A value a `_helm_load` or `_helm_skip_test` reads must be copied in `haptic.testValues`.** Such a value selects which tests exist, so their context has to agree with it. A test gated on a toggle the helper doesn't copy runs against the default, fails in the profile that enables the toggle, and never in an operator's cluster.
- **A test asserting on what `_helm_load` can drop carries the same predicate.** A snippet removed by `unset`, or a library whose `enable` is false, takes its tests' assertions with it; a test's `extraContext` cannot bring it back. Give such tests `_helm_skip_test` with the drop condition — reuse the `when:` as a YAML anchor (`*routeKeyOff`) or the helper it calls (`haptic.spoaHub.pluginUnavailable`) — and add the value to the opt-out profiles in `scripts/test-templates.sh`.
- **Scope the pattern.** A whole-config `not_contains` fails on any unrelated line that happens to match. See "Absence assertions need scoping" — the pattern must name the frontend, backend, or bind it is really about.
- **Give every new opt-in a profile in `scripts/test-templates.sh`** that renders with it ON and runs the **whole** test set (the PROXY-protocol profile is the model). The deployment-values render of every test is what catches an opt-in that breaks an unrelated scenario.

Before bumping a chart an operator already runs, validate against **their** values, not the defaults:

```bash
helm template <release> charts/haptic -f /path/to/their/values.yaml \
  | yq 'select(.kind == "HAProxyTemplateConfig")' > /tmp/cfg.yaml
./bin/haptic validate -f /tmp/cfg.yaml --schema-dir tests/schemas
```

## Common Patterns

### Adding a New Resource Type

```yaml
# 1. Add to watchedResources
watchedResources:
  configmaps:
    apiVersion: v1
    resources: configmaps
    indexBy: ["metadata.namespace", "metadata.name"]

# 2. Create template snippets that use the resource
templateSnippets:
  resource_configmap_backends:
    template: |
      {%- for cm in resources.configmaps.List() %}
      # Process configmap
      {%- endfor %}
```

### Typed Resource Access (Tier-2 typed-watched-resources)

When a schema is loaded for a watched resource (live in production, or via `--schema-dir` offline), both the `resources.<name>` store wrapper *and* the typed top-level global named `<name>` return typed pointers. Field access is direct — no `dig()` needed:

```scriggo
{#- Either surface yields *resources.gateways.T; identical behaviour -#}
{%- for _, gw := range resources.gateways.List() %}
  # {{ gw.Metadata.Namespace }}/{{ gw.Metadata.Name }}: {{ len(gw.Spec.Listeners) }} listeners
{%- end %}

{%- if gateways != nil %}
  {%- for _, gw := range gateways %}
    # {{ gw.Metadata.Namespace }}/{{ gw.Metadata.Name }}
  {%- end %}
{%- end %}
```

The `gateways != nil` guard on the top-level global is defensive: the engine always declares it, but the runtime binding only fires when a store is registered (skipped in offline-validate paths that don't pre-register stores for some kinds). The store wrapper (`resources.gateways.List()`) doesn't need the guard — it returns an empty typed slice when the store is absent.

**Typed return types from store methods** (when a schema is loaded):

| Call | Return type |
|------|-------------|
| `resources.<name>.List()` | `[]*resources.<name>.T` |
| `resources.<name>.Fetch(keys...)` | `[]*resources.<name>.T` |
| `resources.<name>.GetSingle(keys...)` | `*resources.<name>.T` (nil if not found) |

Without a schema, the same calls fall back to `[]any` / `map[string]any` as before.

**`<name>.T` as a type expression.** It's usable in every type-expression position the Scriggo fork supports: macro parameters, var declarations, type assertions, slice types, and type-switch case clauses. This is what lets the chart's libraries push typed access end-to-end.

```scriggo
{#- Macro parameter typed against one kind -#}
{% macro RenderGateway(gw *resources.gateways.T) %}
  # {{ gw.Metadata.Namespace }}/{{ gw.Metadata.Name }}
{% end %}

{#- Type-switch dispatch at a polymorphic any boundary
   (this is the canonical pattern — used in
   `charts/haptic/charts/gateway/60-frontend.yaml` for HTTPRoute/GRPCRoute/TLSRoute) -#}
{%- switch r := routeInfo["route"].(type) %}
{%- case *resources.httproutes.T %}
  # r is statically *resources.httproutes.T here
  # {{ r.Metadata.Name }}: {{ len(r.Spec.Rules) }} rules
{%- case *resources.grpcroutes.T %}
  # {{ r.Metadata.Name }} (gRPC)
{%- case *resources.tlsroutes.T %}
  # {{ r.Metadata.Name }} (TLS passthrough)
{%- end %}

{#- Slice type for sharded parallel rendering -#}
{% var shard []*resources.gateways.T = shard_slice(allGateways, i, n) %}
```

**`shard_slice` is type-preserving.** It's declared as an AdaptiveFunc — the static return type at each call site matches the input element type. `shard_slice([]*resources.gateways.T, i, n)` returns `[]*resources.gateways.T`, not `[]any`, so the downstream loop variable stays statically typed.

**Field-name convention.** Go-PascalCase of the JSON tag, NO acronym preservation:

| JSON tag (source YAML) | Typed field        |
|------------------------|--------------------|
| `metadata`             | `Metadata`         |
| `apiVersion`           | `ApiVersion`       |
| `tls`                  | `Tls`              |
| `ingressClassName`     | `IngressClassName` |
| `matchLabels`          | `MatchLabels`      |
| `clusterIP`            | `ClusterIP`        |
| `loadBalancerIP`       | `LoadBalancerIP`   |
| `kubernetes.io/foo`    | `Kubernetes_io_foo` (non-letter/digit → `_`) |

The rule is canonicalised in `pkg/k8s/typegen/converter.go::goFieldName`. Templates write `gw.ApiVersion`, not `gw.APIVersion`. The reason for no acronym dictionary is in [ADR-0010](../docs/adr/0010-typed-watched-resources.md).

**Worked example.** `charts/haptic/charts/gateway/05-typed-access-smoke.yaml` is the canonical single-snippet example. Its companion test `test-gateway-typed-access-smoke` pins the wiring end-to-end and is the regression canary for typed access generally — if it goes red, the offline validate path has drifted from the production renderer. For the polymorphic-dispatch pattern, see `charts/gateway/60-frontend.yaml`'s route-emission switch.

**Schema source.**

- **Production:** the controller fetches schemas live from the kube-apiserver — CRDs via their embedded `openAPIV3Schema`, K8s core resources via the apiserver's OpenAPI v3 endpoint.
- **Offline (`controller validate` / chart `validationTests` / `scripts/test-templates.sh`):** schemas come from `--schema-dir` / `HAPTIC_SCHEMA_DIR`. The repo's `tests/schemas/` is the canonical bundle covering both the Gateway API CRDs + haptic CRDs **and** the K8s built-ins the chart watches (Namespace, Service, Secret, ConfigMap, EndpointSlice, Ingress); all are CRD-wrapped so the offline GVK resolver picks up the (apiVersion, plural) mapping. The test script auto-wires it. `controller validate --schema-dir tests/schemas` therefore unlocks typed access for every chart-watched resource — not just the CRDs. To refresh the bundle from a running cluster, run `scripts/fetch-k8s-openapi-schemas.sh` (`kubectl get --raw '/openapi/v3/...'` → `$ref`-inlined CRD-wrapped YAML). Without `--schema-dir`, no resources receive typed support — chart code that uses only `resources["<name>"]` / `dig()` validates fine; templates that reach for typed access fail at engine compile time with a clear "no schema for X" pointer back to `--schema-dir`.

**When to use which** (cross-references the same guidance in [`docs/site/docs/templating.md`](../docs/site/docs/templating.md#typed-resource-access)):

- **Inside typed scopes** (typed for-range, typed macro parameter, type-switch case branch), use dot-field access (`gw.Metadata.Name`, `svc.Spec.Ports[i].Port`) — no `dig()`, no `tostring()`, no `fallback()` on already-typed primitives.
- **Macros that take a single Kind** should use typed parameters: `(gws []*resources.gateways.T)`, `(ingresses []*resources.ingresses.T)`, `(route *resources.httproutes.T)`.
- **Polymorphic macros** that handle multiple Kinds keep `[]any` parameters; consumers add `| toSlice()` at the call site if passing a typed slice.
- **Reach for `dig()` only at genuine polymorphic boundaries.** The remaining call sites in the chart are: `routeInfo["route"]` (type-switch dispatch entry), `shared.Get(...)` returns, `listenerOwner any` (Gateway-or-ListenerSet shape), `allowedSelector` (polymorphic matchLabels). New `dig()` usage should be questioned at review.
- **`dig()` continues to work** on both typed structs and untyped maps; mixing approaches across snippets is the expected adoption pattern.
- **Optional fields normalise to nil.** A typegen-produced struct field whose schema entry is *not* in the OpenAPI `required` list carries a `json:"…,omitempty"` tag; `dig()` returns nil for such fields when the value is the type's zero value (`""`, `0`, `false`, empty slice). This makes the universal `dig(obj, "field") | fallback(default)` chart pattern behave identically across typed and untyped shapes — without it, an unpopulated optional string would return `""` (not nil), `fallback()` would skip, and downstream key composition would silently produce malformed strings. Required fields keep their zero values intact. Pinned by `TestDigContract_TypedPointer_NestedTLSCertificateRefs/absent_optional_field_normalised_to_nil`.

**`to_str_map(value)` filter for label / matchLabels / annotation maps.** Typegen produces label / matchLabels / annotation fields as `map[string]string` (matching the K8s OpenAPI schema), while the untyped store path produces `map[string]any`. The `to_str_map(value)` filter normalises any string-keyed map (`map[string]string`, `map[string]any`, or a generic `map[string]<T>`) into a uniform `map[string]string` for template iteration; non-string values from a `map[string]any` input are coerced via `tostring()`. Use it instead of `.(map[string]any)` assertions on label-shaped fields — those panic against typed `map[string]string` from typegen.

```scriggo
{#- Works against both typed and untyped shapes -#}
{%- for k, v := range route.Metadata.Labels | to_str_map() %}
  # {{ k }}={{ v }}
{%- end %}
```

### Implementing Extension Points

If base.yaml defines an extension point like `{% include "resource_ingress_backends" %}`, implement it:

```yaml
templateSnippets:
  resource_ingress_backends:
    template: |
      {%- for ingress in resources.ingresses.List() %}
      backend {{ ingress.metadata.name }}
        # Backend configuration
      {%- endfor %}
```

### Runtime dependency failure: which controls may fail open (RULE)

Two different questions get confused. This section is about the **second**:

1. **Render time** — an annotation is wrong or a Secret is missing. Covered by
   the section below (`fail()` vs `WebhookRejectOrWarn`), and the answer there is
   fail-closed for security controls.
2. **Request time** — the control's own dependency cannot answer. The SPOA hub
   is reloading, a plugin timed out, the rate-limit store is unreachable. That
   is what this rule governs, and the answer is **not** the same.

**A control that cannot answer must fail OPEN unless the failure is a critical
security error.**

**Critical means: the malfunction is itself an immediate security incident, or it
threatens business continuity.** Ask what the next hour looks like if the control
is simply absent.

| Control | If it silently stops working | Posture |
|---|---|---|
| Authentication, authorization, mTLS identity | Unauthenticated callers reach customer data. That is a disclosure incident from the first request, and no later fix undoes it. | **DENY** |
| WAF, rate limiting, request-schema validation | Higher system load, and a risk window a second layer may still cover. Fixed promptly, customers likely never notice. | **ALLOW**, and record it |

**Weigh the harm the control prevents against the harm the denial causes.** For a
rate limiter the honest comparison is *elevated load* versus *every legitimate
caller refused*. Failing closed there manufactures a customer-facing outage where
none existed — we cause the incident we were trying to avoid. For authentication
there is no comparison to make: the leak is unbounded and irreversible, so
refusing traffic is strictly the smaller harm.

**Absent is not the same as bypassed.** An attacker who can take the hub down to
evade the WAF has already achieved more than the evasion. Designing the
degraded path around them costs every honest caller and buys little.

**Failing open is not failing silently.** Every allowed-because-unavailable
event must reach an access-log field or a component metric. Missing SPOA
verdicts set `txn.<control>_degraded`; a plugin that returns an explicit
store-failure verdict records its own outcome counter. If you add a fail-open
path without a signal, you have not applied this rule.

**Offer the strict posture, do not impose it.** Operators enforcing a contractual
cap may genuinely prefer denial. `rateLimit.shared.failClosed` is the shape:
default Valkey-to-local degradation and open on a missing plugin verdict, opt-in
strict, both pinned by tests.

Provenance: the shared rate limiter denied on a missing SPOA verdict, so a single
HAProxy reload returned 429 to a caller who was nowhere near their budget. It was
documented as deliberate — "fails closed with 429 to avoid a rate-limit bypass" —
which is how it survived review.

### Validating watched-resource input: `fail()` vs `WebhookRejectOrWarn`

When a snippet validates a value that came off a **watched resource** (an Ingress
annotation, a route field), do **not** reach for a bare `fail()` by default —
`fail()` rejects the candidate configuration. The controller isolates invalid
watched revisions before publication (ADR-0032), retaining their last validated
revision while independent updates advance. Use a route-local result where the
library already defines one.

Instead decide by the render's `renderMode` (a global string: `"admission"` for a
webhook dry-run of a proposed change, `"reconcile"` for the live config and the
load gate). The shared macro `WebhookRejectOrWarn(resource, reason, message)` in
`charts/haptic/charts/base/library.yaml` encapsulates the split: it `fail()`s
under admission (so the API server denies the proposed resource) but records a
`Warning` Event and returns on any other render (so the fleet keeps serving).

Run watched-object guards in the owning incremental component: its `renderMode`
and `renderSubject.mode` identify whether that object is under admission. A root
that iterates every object must not turn an existing warning into an unrelated
admission failure. Keep the caller's skip or route-denial behavior intact.

```scriggo
{%- import "util-webhook-reject-or-warn" for WebhookRejectOrWarn -%}
...
{%- for _, ingress := range resources.ingresses.List() %}
  {%- if <value is invalid> %}
    {{- WebhookRejectOrWarn(ingress, "InvalidAnnotationValue", "<message>") -}}
    {%- continue %}   {#- caller MUST skip this resource's output in the warn path -#}
  {%- end %}
  ... emit this Ingress's config ...
{%- end %}
```

**Decide fail vs warn by what a *skip* would mean** — the warn path skips the
offending resource's contribution, so it must be safe to serve *without* that
feature:

- **Warn (use `WebhookRejectOrWarn` + skip)** — routing/presentation features
  where skipping just drops that one behaviour: redirects, CORS, cookie/header/
  location rewrites, canary, compression, traffic mirroring, host rewrites,
  fixed/mock responses. Injection guards on these values are still fine to warn —
  the `continue` means the rejected value is never emitted.
- **Fail closed per-route (WAF-library `rejectRoute` pattern)** — security
  features that have their own per-route fail-closed machinery. The WAF
  libraries don't skip-and-serve (that would be fail-open) and don't
  `fail()` globally (one pre-existing violator would abort every render
  and, because the webhook live-renders each admission, deny every config
  and Ingress update — the wedge class). Instead a violating route is
  denied with 503 (`frontend-filters-113`/`-114`) plus a Warning Event,
  and hard-fails ONLY when the route's own resource is the admission
  subject (`admissionSubject` global, controller-set). Use this shape for
  any security guard whose enforcement can be scoped to the offending
  route.
- **Keep `fail()` (hard-fail every mode)** in these cases:
    - **Security features without a per-route fail-closed path** — auth,
      client/backend mTLS or TLS-verify, rate limiting, request-body
      validation, `X-Forwarded-For` handling. Silently skipping a security
      control is fail-**open**; hard-fail instead so the misconfiguration is
      loud. (Root `CLAUDE.md` → "No useless fail-open".) This is the
      **render-time** rule — a misconfiguration an operator can fix. It does not
      govern what happens when a control's dependency is unreachable at request
      time; see "Runtime dependency failure" above, where WAF and rate limiting
      fail OPEN. When the library CAN
      deny the offending route itself (the WAF `rejectRoute` pattern above),
      prefer that — it is equally fail-closed without the global blast
      radius.
    - **Skip isn't clean** — the guard sits in a value-returning macro, or in a
      `backend-directives-*` snippet rendered via `render_glob … inherit_context`
      (no enclosing `resources.ingresses.List()` loop, so `{% continue %}` can't
      skip the resource and the fall-through would emit partial/corrupt config).
    - **Global config / engine errors** — `extraContext.*` validation, "failed to
      register … map", missing Secret/ConfigMap, aggregate limits ("… more than
      the configured limit"). These aren't tied to one watched resource.

When in doubt, keep `fail()`. If you convert one guard in a package, sweep the
package for sibling guards that qualify. Whenever you add a `WebhookRejectOrWarn`
guard whose message a `validationTest` asserts via `rendering_error`, pin that
test with `extraContext.renderMode: admission` so it still exercises the fail.

### Annotation Template Documentation Standards

**Every annotation template MUST include comprehensive inline documentation** to prevent confusion about expected formats and behavior.

**Required Documentation Sections:**

```scriggo
{#-
  <Template Name>

  Documentation: <URL to official HAProxy Ingress or HAProxy docs>

  Annotations:
    - annotation.name: "<value-format>" (required/optional)
    - ...list all annotations this template uses...

  Resource Format (if template reads secrets, configmaps, etc.):
    Detailed explanation of expected structure, especially for base64-encoded data.

    IMPORTANT: Explicitly state format expectations (e.g., "hash only, NOT username:hash")

  Example:
    <Complete working example manifest>

  Generated HAProxy Config:
    <Show what HAProxy configuration this template produces>

  Notes:
    - Any gotchas, limitations, or special behaviors
    - Cross-references to related templates
-#}
```

**Real Example:**

See `charts/haptic/charts/haproxytech/library.yaml` for the `global-top-500-haproxytech-ingress-auth` template (around line 237) which demonstrates proper documentation including:

- Link to HAProxy Ingress documentation
- List of all annotations
- Detailed secret format explanation with WARNING about htpasswd vs hash-only
- Example secret manifest
- Command to generate correct password hash
- Description of generated HAProxy config
- Deduplication behavior

**Why This Matters:**

Without inline documentation, developers must:

1. Search external documentation
2. Guess at format requirements
3. Potentially implement incorrect parsing logic

Proper documentation prevents bugs and makes templates self-documenting.

### Cross-Library Shared State (globalFeatures / gf)

Libraries communicate across boundaries using the `globalFeatures` map (commonly aliased as `gf`). This enables features like SSL to be configured in one library (ingress.yaml, gateway/) and consumed by another (ssl.yaml).

**Pattern:**

```scriggo
{#- ssl.yaml: Initialize shared state during feature registration -#}
{%- if gf["sslPassthroughBackends"] == nil %}
  {%- gf["sslPassthroughBackends"] = []any{} %}
{%- end %}
{%- if gf["tlsCertificates"] == nil %}
  {%- gf["tlsCertificates"] = []any{} %}
{%- end %}

{#- ingress.yaml or gateway/: Append data to shared state -#}
{%- var sslBackends []any = gf["sslPassthroughBackends"].([]any) %}
{%- gf["sslPassthroughBackends"] = append(sslBackends, backend) %}

{#- ssl.yaml: Consume shared state to generate output -#}
{%- var backends = gf["sslPassthroughBackends"] | fallback([]any{}) %}
{%- for _, backend := range backends.([]any) %}
  use_backend {{ backend["name"] }} if { req.ssl_sni -i {{ backend["sni"] }} }
{%- end %}
```

**Canonical Shared State Keys** (the SSL examples — the cross-library set is larger; see the note below):

| Key | Type | Purpose | Initialized By | Written By |
|-----|------|---------|----------------|------------|
| `sslPassthroughBackends` | `[]any` | SSL passthrough backend definitions | ssl.yaml | gateway/, haproxytech.yaml |
| `tlsCertificates` | `[]any` | TLS certificate references for crt-list | ssl.yaml | gateway/, ingress.yaml |

This table is illustrative, not exhaustive — other genuinely cross-library keys include `sslRedirectHosts`, `clientCertVerifyHosts`, and `needHTTPSTermination`. Before introducing a new key, check it isn't an existing one under a different spelling — grep the authoritative set: `grep -rhoE '(gf|globalFeatures)\["[a-zA-Z_]+"\]' charts/haptic/charts/ | sort -u`.

!!! warning "Map Key Consistency is Critical"
    All libraries **MUST** use the exact same map key names. The codebase uses **camelCase** for shared state keys. Using different key names (e.g., `tls_certificates` vs `tlsCertificates`) will cause silent failures where data written by one library is invisible to another.

**The `gf` Alias:**

`gf` is a shorthand alias for `globalFeatures`. Both refer to the same shared map. Use `gf` for brevity in templates:

```scriggo
{#- These are equivalent: #}
{%- var certs = globalFeatures["tlsCertificates"] %}
{%- var certs = gf["tlsCertificates"] %}
```

### Backend Deduplication

When multiple paths route to the same service+port, deduplicate backends:

```yaml
templateSnippets:
  resource_ingress_backends:
    template: |
      {#- Backend deduplication #}
      {% var seen = map[string]bool{} %}
      {%- for _, ingress := range resources.ingresses.List() %}
      {%- for _, path := range ingress.spec.paths %}
      {% var backend_key = path.service.name + "_" + path.service.port %}
      {%- if !seen[backend_key] %}
      {% seen[backend_key] = true %}

      backend {{ backend_key }}
        # Only generated once per unique service+port
      {%- end %}
      {%- end %}
      {%- end %}
```

### Server Options and Runtime API

To enable HAProxy runtime API updates without reloads, server options must be in `default-server`, not on individual server lines.

**Runtime-supported server fields (no reload):**

- `Weight`, `Address`, `Port` - Core properties
- `Maintenance` (`enabled`/`disabled`) - Server state
- `AgentCheck`, `AgentAddr`, `AgentSend`, `HealthCheckPort` - Agent checks

**Important:** The `disabled`/`enabled` state does NOT cause a reload — a not-ready endpoint renders as a `disabled` server and a readiness flip is a runtime `set server state`.

**All other options trigger reloads** including: `check`, `proto`, `ssl`, `verify`, `ca-file`, `crt`

**Correct pattern in templates:**

```scriggo
{{ Backend(map[string]any{
     "name":          backendKey,
     "guid":          make_guid("be", backendKey),
     "defaultServer": []any{map[string]any{"name": "check"}},
     "servers":       BackendServers(serviceName, 0, port, serverOpts, nil, backendKey, ns),
   }) }}
```

Every backend goes through base's `Backend()` (`util-backend`): it builds the
section text from the record it declares to the render plan, so a change to a
backend is data the controller can act on instead of text it has to re-read. A
hand-written `backend` section still renders, but any change to it forces a
reload. `mode`/`balance`/`hash-type`/`default-server` and the shared `profile`
lines go into a content-addressed `defaults haptic-be-<hash> from haptic-base`
the backend inherits with `from`; keep `body` empty for a dynamic-eligible
backend.

`BackendServers` returns one server record per endpoint — `name` (the pod),
`address`, `port`, `disabled`, `weight`, `guid`, `comment`, `extra` — and
`Backend()` formats them into `server <pod> <ip>:<port>` lines. There is no
slot pool: the file always equals the current pod set.

**Example output:**

```haproxy
backend default_my-ingress_svc_my-service_http from haptic-be-ef46db3751d8
    guid be:default_my-ingress_svc_my-service_http
    server my-app-6d4b-abc 10.0.0.1:8080
    server my-app-6d4b-def 10.0.0.2:8080
    server my-app-6d4b-ghi 10.0.0.3:8080 disabled
```

**Why this matters:** When pods scale up/down, servers are added and removed by pod name over the runtime API (no reload). Because `default-server` lives in the profile, keeping per-backend options there (not on the server line) keeps the backend dynamic-eligible.

### Optimizing Expensive Computations with Utility Snippets

Libraries provide **utility snippets** that cache expensive computations. These snippets encapsulate all the caching complexity, making it easy to use cached data from any template.

**Problem**: Expensive computations run multiple times per render

Without caching, analyzing routes or scanning resources runs every time a snippet includes the computation:

```jinja2
{# snippet1.yaml - analyzes all HTTPRoutes #}
{%- for route in analyze_all_routes() %}  {# Expensive! #}

{# snippet2.yaml - analyzes all HTTPRoutes AGAIN #}
{%- for route in analyze_all_routes() %}  {# Runs again! #}

{# Result: expensive computation runs N times for N snippets #}
```

**Solution**: Use utility snippets for cached access

Utility snippets handle all caching internally. Just render them and use the result:

```go
{# Any snippet that needs route analysis #}
{{ render "util-gateway-analysis" }}

{# The gateway_analysis variable is now available with cached data #}
{%- for _, route := range gateway_analysis.sortedRoutes %}
  ... process route ...
{%- end %}
```

**Available Utility Snippets:**

| Snippet | Library | Provides | Description |
|---------|---------|----------|-------------|
| `util-gateway-analysis` | gateway/ | `gatewayAnalysis` | HTTPRoute sorting, grouping, conflict detection |
| `util-gateway-ssl-passthrough` | gateway/ | `gateway_ssl_passthrough` | Gateway SSL passthrough backend scanning |
| `util-haproxytech-ssl-passthrough` | haproxytech.yaml | `haproxytech_sslPassthrough` | Ingress SSL passthrough backend scanning |

**Example Usage:**

```go
{# Gateway route analysis - used by 7+ snippets #}
{{ render "util-gateway-analysis" }}
{%- for _, route := range gateway_analysis.sortedRoutes %}
backend {{ route.metadata.namespace }}_{{ route.metadata.name }}
    {# ... backend config ... #}
{%- end %}

{# SSL passthrough backends - used by 2 snippets #}
{{ render "util-gateway-ssl-passthrough" }}
{%- for _, backend := range gateway_ssl_passthrough.backends %}
    use_backend {{ backend.name }} if { req.ssl_sni -i {{ backend.sni }} }
{%- end %}
```

**How It Works (Internal Architecture):**

`shared.ComputeIfAbsent(key, fn)` stores values by key with compute-once
semantics. When the same key is used across different template contexts, the
cached data is reused without re-running the expensive computation:

1. First call: Runs the closure, stores the result, returns `(value, true)`.
2. Subsequent calls: Returns the existing value with `(value, false)`.
3. Works across different template contexts (haproxyConfig, map files, etc.)
   because they all share the same `*SharedContext` for one render.

```go
{# Inside util-gateway-analysis (simplified) #}
{# ComputeIfAbsent runs the closure at most once per render, even with
   parallel sub-renders — singleflight serialises duplicate keys. #}
{% var gateway_analysis, _ = shared.ComputeIfAbsent("gatewayAnalysis", func() any {
    {% import "util-analyze-routes" for analyze_routes %}
    return analyze_routes(resources)
}) %}
{# gateway_analysis now contains cached data from first computation #}
```

**Creating New Utility Snippets:**

When you have expensive computations used by multiple snippets:

```yaml
# In your library file (e.g., my-library.yaml)
templateSnippets:
  util-my-expensive-computation:
    template: |
      {#-
        My Expensive Computation Cache

        Description of what this computes and why it's expensive.

        After rendering this snippet, the following variable is available:
          my_computation - map containing:
            .results - list of computed results
            .lookup  - dict for fast lookups

        Usage:
          {{ render "util-my-expensive-computation" }}
          {%- for _, item := range my_computation.results %}
            ... use item ...
          {%- end %}
      -#}
      {% var my_computation, _ = shared.ComputeIfAbsent("my_computation", func() any {
          {# Runs at most once per render. #}
          var results = []any{}
          for _, resource := range resources.my_resources.List() {
              results = append(results, resource)
          }
          return map[string]any{"results": results}
      }) %}
```

**Key Requirements:**

1. Use a descriptive cache key (string name).
2. Wrap the expensive work in a closure passed to `shared.ComputeIfAbsent`.
3. Discard the second return value (`_`) for plain memoisation; capture it
   as `wasComputed` if you want the `first_seen` deduplication semantics.

**Performance Impact**: Reduces expensive computations from N to 1 per render (up to 70-90% reduction for heavy operations).

**When to Create a Utility Snippet:**

✅ **Good candidates:**

- Expensive loops over all resources (HTTPRoutes, Ingresses, etc.)
- Complex sorting, grouping, or conflict detection
- Resource scanning with filtering logic
- Any computation used by 2+ snippets

❌ **Don't create for:**

- Simple variable assignments
- Computations specific to one snippet
- Fast operations (under 10ms)

## Scriggo Templating Guide

**Scriggo is the template engine.** It uses Go's type system natively.

**Official Documentation:** <https://scriggo.com/templates>

### Template Syntax Overview

Scriggo uses three primary delimiter types:

| Syntax | Purpose | Example |
|--------|---------|---------|
| `{{ expr }}` | Output expression (show) | `{{ product.Name }}` |
| `{% stmt %}` | Statements/declarations | `{% if stock > 10 %}` |
| `{%% ... %%}` | Multi-line statement blocks | Complex logic |
| `{# comment #}` | Comments (nestable) | `{# TODO #}` |

**Show statement:** The `{{ }}` syntax is shorthand for `{% show expr %}`. You can show multiple expressions: `{% show 5 + 2, " = ", 7 %}`.

### Multi-line Statement Blocks (Preferred)

**Always prefer multi-line statement blocks (`{%% ... %%}`) over multiple single-line statements** when you have consecutive logic statements. This improves readability and reduces visual clutter.

**Single-line syntax** (`{% ... %}`): Use for single statements or when embedded in output:

```scriggo
{%- var name = "value" %}
{%- if condition %}output{% end %}
```

**Multi-line syntax** (`{%% ... %%}`): Use for blocks of consecutive statements. Inside multi-line blocks, use Go-style syntax with curly braces:

```scriggo
{%%
  var name = route.metadata.name
  var namespace = route.metadata.namespace
  var backendKey = namespace + "_" + name

  if !first_seen("backends", backendKey) {
    continue
  }
%%}
```

**When to use multi-line blocks:**

- ✅ Multiple variable declarations in sequence
- ✅ Complex conditional logic with multiple statements
- ✅ Loop setup with pre-computed variables
- ✅ Any block with 3+ consecutive statement lines

**When to use single-line:**

- ✅ Single statement followed by output
- ✅ Simple `if`/`for` wrapping output content
- ✅ Statements interspersed with template output

**Example refactoring:**

```scriggo
{#- AVOID: Many single-line statements #}
{%- var name = route.metadata.name %}
{%- var namespace = route.metadata.namespace %}
{%- var annotations = route.metadata.annotations %}
{%- var backendKey = namespace + "_" + name %}
{%- if !first_seen("backends", backendKey) %}
  {%- continue %}
{%- end %}

{#- PREFER: Multi-line block #}
{%%
  var name = route.metadata.name
  var namespace = route.metadata.namespace
  var annotations = route.metadata.annotations
  var backendKey = namespace + "_" + name

  if !first_seen("backends", backendKey) {
    continue
  }
%%}
```

### Variables and Types

**Variable declaration:** Variables require the `var` keyword or short assignment syntax:

```scriggo
{%- var name = "value" %}
{%- var count = 0 %}
{%- var items = []any{} %}
{%- var config = map[string]any{} %}

{#- Short declaration syntax #}
{%- welcome := "hello" %}
```

Type can be explicit or inferred from the assigned value. Uninitialized variables receive default values (empty string, 0, false, nil).

**Assignment (reassignment):** Once declared, variables can be reassigned with `=`. The type cannot change.

```scriggo
{%- name = "new value" %}
{%- count = count + 1 %}

{#- Compound operators supported #}
{%- count++ %}
{%- count += 5 %}

{#- Multiple assignment #}
{%- a, b = b, a %}
```

**Variable scope:**

- **File-level (outside blocks)**: Visible throughout the file and in extended/imported files
- **Within blocks** (macros, conditionals, loops): Visible from declaration to block end
- **Render statements**: Variables don't cross file boundaries unless passed via macro arguments

**Basic types:**

| Type | Description | Default |
|------|-------------|---------|
| `bool` | Boolean values (`true`/`false`) | `false` |
| `string` | Text in double quotes or backticks | `""` |
| `int` | Integer numeric values | `0` |
| `float64` | Floating-point numbers | `0.0` |

**Format types** (string-based with context-aware escaping):

- `html` - HTML code that won't be escaped in HTML contexts
- `css`, `js`, `json`, `markdown` - Similar context-aware types

**Collection types:**

```scriggo
{#- Slices (ordered sequences) #}
{%- var items = []any{} %}
{%- var names = []string{"alice", "bob"} %}
{%- var numbers = []int{1, 2, 3} %}

{#- Maps (key-value associations) #}
{%- var config = map[string]any{} %}
{%- var labels = map[string]string{"app": "web"} %}
```

**Slice operations:**

- Indexing: `s[0]`
- Length: `len(s)`
- Slicing: `s[1:3]` (end index excluded)
- Appending: `append(s, value)`

**Map operations:**

- Bracket notation: `map["key"]`
- Dot notation: `map.key`
- Iteration: `for key, value := range map`

### Control Flow

**If statement:**

```scriggo
{%- if condition %}
  content
{%- else if other_condition %}
  other content
{%- else %}
  fallback
{%- end %}
```

**Truthiness rules:** A condition is false for: `false`, `0`, `0.0`, `""`, `nil`, empty collections (slices/maps), and a **struct whose every field is its zero value**. All other values are truthy.

That last one is how you ask whether an optional object was set, without a `dig()` presence probe:

```scriggo
{%- if ingress.Spec.DefaultBackend.Service %}      {# set #}
{%- if not gateway.Spec.Tls.Frontend %}            {# absent or empty #}
```

Use the template-style `not` / `and` / `or` rather than `!` / `&&` / `||` when an operand is a struct: the Go operators require a `bool`, while these coerce any value through the rules above — including inside a pipeline predicate (`filter(o => not o.Spec.Tls.Frontend)`).

**For loops:**

```scriggo
{#- For-in loop (most common) - note the "in" keyword #}
{%- for item in items %}
  {{ item }}
{%- end %}

{#- For-range with index and value #}
{%- for i, item := range items %}
  {{ i }}: {{ item }}
{%- end %}

{#- For with else (runs if collection is empty) #}
{%- for item in items %}
  {{ item }}
{%- else %}
  No items found
{%- end %}

{#- C-style for loop with condition #}
{%- for i := 0; i < 10; i++ %}
  {{ i }}
{%- end %}

{#- For with just condition (while-style) #}
{%- for condition %}
  content
{%- end %}
```

**Loop control:**

- `{% break %}` - Exit the loop immediately
- `{% continue %}` - Skip to the next iteration

**Switch statement:**

```scriggo
{%- switch value %}
{%- case "option1" %}
  First option
{%- case "option2", "option3" %}
  Second or third option
{%- default %}
  Default case
{%- end %}

{#- Switch without expression (uses boolean cases) #}
{%- switch %}
{%- case stock > 100 %}
  High stock
{%- case stock > 10 %}
  Medium stock
{%- default %}
  Low stock
{%- end %}
```

Only the first matching case executes (no fallthrough).

### Operators

**Comparison:** `==`, `!=`, `<`, `<=`, `>`, `>=`

**Arithmetic:** `+`, `-`, `*`, `/`, `%` (remainder for integers only)

**Logical operators:**

| Operator | Go-style | Template-style | Notes |
|----------|----------|----------------|-------|
| AND | `&&` | `and` | Returns `true`/`false`, accepts any type |
| OR | `\|\|` | `or` | Returns `true`/`false`, accepts any type |
| NOT | `!` | `not` | Returns `true`/`false`, accepts any type |

The template-style operators (`and`, `or`, `not`) differ from Go's boolean operators by accepting any type and evaluating truthiness. Unlike Jinja2 where `and`/`or` return one of the operands, Scriggo always returns boolean.

**String concatenation:** `+` (not `~` like Jinja2)

```scriggo
{%- var fullname = firstname + " " + lastname %}
```

**Contains operator:** `contains`, `not contains`

Template-specific operators for checking slice membership, map keys, and substring presence:

```scriggo
{%- if colors contains "red" %}
{%- if product.Name contains "bundle" %}
{%- if name not contains "test" %}
```

**Default operator:**

```scriggo
{{ value default "fallback" }}
```

!!! warning "Default Operator Limitation"
    The `default` operator only works with simple identifiers, not field access.
    Use `fallback()` function for field access: `{{ obj.field | fallback("default") }}`

### Functions and Filters

Scriggo supports both function call syntax and pipe syntax:

```scriggo
{#- Function syntax #}
{{ toLower(name) }}
{{ join(items, ", ") }}

{#- Pipe syntax (Jinja2-style) #}
{{ name | toLower() }}
{{ items | join(", ") }}
```

!!! warning "Pipe Operator Requires Parentheses"
    In Scriggo, the pipe operator requires a function call on the right side:
    - `{{ value | toLower() }}` ✓
    - `{{ value | toLower }}` ✗ (error: pipe operator requires function call)

**Style preferences:**

- **Prefer pipe syntax over nested function calls** for readability:

  ```scriggo
  {#- Good - reads left to right #}
  {{ value | dig("metadata", "name") | fallback("") | tostring() }}

  {#- Avoid - harder to read #}
  {{ tostring(fallback(dig(value, "metadata", "name"), "")) }}
  ```

- **Prefer dot notation over bracket notation** for map access when keys are valid identifiers:

  ```scriggo
  {#- Good - cleaner syntax #}
  {{ route.metadata.namespace }}
  {{ config.server.port }}

  {#- Use brackets only when necessary #}
  {{ labels["kubernetes.io/name"] }}  {# Key contains special chars #}
  {{ data[variableKey] }}              {# Dynamic key access #}
  ```

**Available functions:**

| Function | Description | Example |
|----------|-------------|---------|
| `tostring(v)` | Convert to string | `tostring(123)` → `"123"` |
| `toint(v)` | Convert to int | `toint("42")` → `42` |
| `tofloat(v)` | Convert to float64 | `tofloat("3.14")` → `3.14` |
| `len(v)` | Length of slice/map/string | `len(items)` |
| `toLower(s)` | Lowercase string | `toLower("ABC")` → `"abc"` |
| `toUpper(s)` | Uppercase string | `toUpper("abc")` → `"ABC"` |
| `strip(s)` | Trim whitespace | `strip("  x  ")` → `"x"` |
| `replace(s, old, new)` | Replace all occurrences | `replace("a-b", "-", "_")` |
| `split(s, sep)` | Split string | `split("a,b,c", ",")` → `[]string` |
| `join(slice, sep)` | Join slice | `join([]string{"a","b"}, ",")` → `"a,b"` |
| `hasPrefix(s, p)` | Check prefix | `hasPrefix("hello", "he")` → `true` |
| `hasSuffix(s, p)` | Check suffix | `hasSuffix("hello", "lo")` → `true` |
| `b64decode(s)` | Decode base64 | `b64decode("SGVsbG8=")` → `"Hello"` |
| `keys(m)` | Sorted map keys | `keys(config)` → `[]string` |
| `merge(m1, m2)` | Merge maps | `merge(base, overrides)` |
| `dig(obj, keys...)` | Navigate nested maps **and** typed structs (via JSON-tag → Go-field lookup); optional `omitempty` fields with zero values normalise to nil | `dig(obj, "meta", "name")` |
| `fallback(v, default)` | Return default if nil | `fallback(obj.field, "")` |
| `dig_string(obj, default, keys...)` | Fused `dig + fallback + tostring` — string access at polymorphic boundaries (annotation / metadata lookups on `any`-typed values) | `v \| dig_string("", "metadata", "name")` |
| `append(slice, item)` | Go's builtin: type-preserving, and `append(dst, src...)` spreads a slice of the **same** type. Widening into `[]any` is a compile error — box per element with a loop. A slice reached through `any` (a `map[string]any` value, a `coalesce()` result) is asserted at the boundary, which is the house style in ~50 places | `append(items, newItem)`, `append(gf["hosts"].([]any), h)` |
| `toSlice(v)` | Convert to []any | `toSlice(maybeNil)` |
| `toStringSlice(v)` | Convert `[]any` to `[]string` | `toStringSlice(items)` |
| `ceil(n)` | Ceiling of a float | `ceil(1.2)` → `2` |
| `to_str_map(v)` | Normalise any string-keyed map (`map[string]string` from typegen, `map[string]any` from the untyped store path) into `map[string]string` — use on labels / matchLabels / annotations | `route.Metadata.Labels \| to_str_map()` |
| `shard_slice(items, idx, n)` | Type-preserving slice shard for parallel rendering (AdaptiveFunc; return element type matches input) | `shard_slice([]*resources.gateways.T, i, n)` |
| `resource(name)` | Per-render items of a watched resource named **dynamically** (`[]any` of boxed `*T`). Governance layer only | `resource("ingresses")` |
| `jsonpathGet(item, path)` | Read a **concrete** JSONPath out of any watched-resource item (dotted keys, `['bracket']` keys, `[n]` indices). Returns nil if absent | `jsonpathGet(ing, "metadata.annotations['x']")` |
| `deriveResource(name, item, path, value)` | Publish an immutable transformed value to subsequent resource accessors; returns the transformed item for chained writes | `ing = deriveResource("ingresses", ing, "metadata.annotations['x']", "100")` |
| `jsonpathSet(item, path, value)` | Write a concrete JSONPath into a resource item **in place** (annotations via a reflect fast path, other fields via a JSON round-trip); returns bool. Filtered/wildcard paths are rejected (validate-only) | `jsonpathSet(ing, "metadata.annotations['x']", "100")` |
| `map(slice, fn)` | Apply a function to every element; length preserved, result type from the closure | `eps \| map(e => e.TargetRef.Name)` |
| `filter(slice, pred)` / `reject(slice, pred)` | Keep / drop elements a closure accepts. Type-preserving | `eps \| reject(e => e.Ready)` |
| `flat_map(slice, fn)` | Map each element to a slice and concatenate, flattening ONE level | `resources.endpoints.List() \| flat_map(s => s.Endpoints)` |
| `unique(slice)` / `unique_by(slice, key)` | First occurrence per element / per key, input order preserved. `key` is a closure or an attribute path | `pairs \| unique_by("host")` |
| `group_by(slice, key)` | Bucket into `map[string][]T`; iterate via `keys()` for deterministic output | `routes \| group_by(r => r.Host)` |
| `sort_by(slice, criteria)` | Sort by JSONPath criteria **or** a `func(a, b T) int` comparator | See sorting section |
| `sort_ints(slice)` | Sort `[]any` of ints numerically (non-ints coerced via `toint`, sort to front) — use for ports/IDs where `sort_strings` would misorder (`"10"` before `"2"`) | `sort_ints(ports)` |
| `glob_match(names, pattern)` | Filter by glob | `glob_match(templates, "backend-*")` |
| `selectattr(items, attr[, op, v])` | Jinja2-style filter: items where `attr` is truthy, or where `op` ∈ {`eq`,`ne`,`in`} matches `v` | `selectattr(rules, "host", "eq", h)` |
| `first_seen(prefix, keys...)` | Deduplication helper | See deduplication section |
| `regex_search(s, pattern)` | Regex match | `regex_search(name, "^test")` |
| `sanitize_regex(s)` | Escape regex chars | `sanitize_regex("a.b")` → `"a\\.b"` |
| `indent(s, spaces, first)` | Indent lines | `indent(text, 4, true)` |

**Scriggo built-in functions** (see <https://scriggo.com/templates/builtins>):

| Function | Description | Example |
|----------|-------------|---------|
| `len(v)` | Length of string/slice/map | `len("hello")` → `5` |
| `runeCount(s)` | Character count (vs bytes) | `runeCount("日本")` → `2` |
| `abs(n)` | Absolute value | `abs(-5)` → `5` |
| `max(a, b)` | Maximum of two ints | `max(3, 7)` → `7` |
| `min(a, b)` | Minimum of two ints | `min(3, 7)` → `3` |
| `pow(x, y)` | Power (float64) | `pow(2.0, 3.0)` → `8.0` |
| `sort(slice)` | Sort slice | `sort([]int{3, 1, 2})` |
| `reverse(slice)` | Reverse slice | `reverse(items)` |
| `capitalize(s)` | Capitalize first letter | `capitalize("hello")` → `"Hello"` |
| `capitalizeAll(s)` | Capitalize all words | `capitalizeAll("hello world")` |
| `index(s, substr)` | Find substring index | `index("hello", "ll")` → `2` |
| `abbreviate(s, n)` | Truncate with ellipsis | `abbreviate("hello world", 8)` |
| `toKebab(s)` | CamelCase to kebab-case | `toKebab("borderTop")` → `"border-top"` |
| `sprintf(fmt, args...)` | Format string | `sprintf("%d items", count)` |
| `base64(s)` | Encode to base64 | `base64("hello")` |
| `hex(s)` | Encode to hex | `hex("AB")` → `"4142"` |
| `md5(s)` | MD5 hash | `md5("test")` |
| `sha1(s)` | SHA1 hash | `sha1("test")` |
| `sha256(s)` | SHA256 hash | `sha256("test")` |
| `queryEscape(s)` | Percent-encode every byte outside `[0-9A-Za-z-._]`, lowercase hex | `queryEscape("a b")` → `"a%20b"` |
| `htmlEscape(s)` | Escape HTML | `htmlEscape("<b>")` → `"&lt;b&gt;"` |
| `marshalJSON(v)` | Convert to JSON | `marshalJSON(obj)` |
| `unmarshalJSON(s)` | Parse JSON | `unmarshalJSON(jsonStr)` |
| `regexp(pattern)` | Compile regex | See regex section |
| `now()` | Current time | `now()` |
| `date(y, m, d, ...)` | Create time | `date(2024, 1, 15)` |

### Macros

Macros are reusable template functions. They must have **uppercase** first letter to be importable/exportable across files.

**Definition:**

```scriggo
{%- macro BackendName(namespace string, name string) %}
backend_{{ namespace }}_{{ name }}
{%- end %}
```

**Calling macros:**

```scriggo
{{ BackendName("default", "myservice") }}
```

**Macro parameters with types:**

```scriggo
{%- macro ProcessRoute(route map[string]any, index int) %}
  {#- route is typed as map[string]any #}
  {%- var name = route["name"].(string) %}
{%- end %}
```

Type annotations can be omitted if consecutive parameters share the same type:

```scriggo
{%- macro Image(url string, width, height int) %}
```

**Macro scope:** Macros can access global variables and other macros/variables declared earlier in the same file. Variables declared within a macro body remain local to that macro.

**Distraction-free macros:** In files with an `extends` declaration, parameter-less macros can use simplified syntax:

```scriggo
{% Main %}
  Content here...
```

This is equivalent to `{% macro Main() %}...{% end %}` and extends to the end of the file.

!!! note "Macro Parameter Types"
    Macro parameters can use any type declared in the template globals.
    For custom types like `ResourceStore`, you need to expose them via `reflect.TypeOf()`.
    See "Exposing Custom Types" section below.

### Extends and Import

**Extends declaration:** Allows a template to inherit layout from another file. Must appear at the beginning of the file before other declarations.

```scriggo
{% extends "/layouts/base.html" %}

{#- The layout file calls macros like {{ Title() }} and {{ Body() }} #}
{#- Child files define those macros to fill in the layout: #}

{% macro Title() %}My Page Title{% end %}

{% macro Body() %}
  <p>Page content here</p>
{% end %}
```

**Import declaration:** Retrieves declarations from other files.

```scriggo
{#- Import specific macros #}
{% import "util-backend-helpers" for BackendName %}
{% import "util-helpers" for Helper1, Helper2 %}

{#- Import all exported declarations #}
{% import "util-helpers" %}

{#- Import with prefix (namespace) #}
{% import utils "util-helpers" %}
{{ utils.BackendName("default", "svc") }}
```

**Export rules:**

- Only declarations with **uppercase first letter** are exported
- Imported files can only contain declarations (no standalone content outside macros)

### The using Statement

The `using` statement evaluates a block of content and makes it available through the special `itea` identifier:

```scriggo
{%- var content = itea; using %}
  <p>This content is assigned to the variable</p>
{%- end using %}

{{ content }}  {#- Outputs the evaluated content #}
```

**Type specification:** You can declare itea's type:

```scriggo
{% show itea; using markdown %}
# Markdown Heading
Some **bold** text.
{% end %}
```

Supported types: `string`, `html`, `markdown`, `css`, `js`, `json`

**Passing content to functions:**

```scriggo
{% sendEmail(from, to, itea); using %}
Hello {{ name }},
Your order has been shipped.
{% end %}
```

**Lazy evaluation with using macro:** Defer body execution until actually needed:

```scriggo
{% show Dialog("Warning", itea); using macro %}
  <p>This is only evaluated if Dialog actually uses its content parameter</p>
{% end %}
```

**Macro with parameters:**

```scriggo
{% show UserList(users, itea); using macro(user User) %}
  <li>{{ user.Name }} - {{ user.Email }}</li>
{% end %}
```

### Type Assertions

When working with `interface{}` (any) values, you need type assertions to access fields or use type-specific operations. **Note:** for watched-resource values the chart's preferred shape is typed access (`*resources.<name>.T`) — see the "Typed Resource Access" section above. The assertions below apply to genuinely-polymorphic values (`shared.Get(...)`, `globalFeatures[...]`, `routeInfo["route"]`, etc.). For label / matchLabels / annotation values use `| to_str_map()` instead of `.(map[string]any)` — typegen builds those as `map[string]string`, which a `map[string]any` assertion would panic on.

```scriggo
{#- Type assertions on genuinely-polymorphic values #}
{%- var name = value.(string) %}
{%- var items = value.([]any) %}
{%- var config = value.(map[string]any) %}

{#- Safe type assertion with check #}
{%- var name, ok = value.(string) %}
{%- if ok %}
  {{ name }}
{%- end %}

{#- Typed pointer assertion at a polymorphic boundary -#}
{%- if gw, ok := listenerOwner.(*resources.gateways.T); ok %}
  # {{ gw.Metadata.Name }}
{%- end %}
```

**Common type assertion patterns:**

```scriggo
{#- Accessing nested map values #}
{%- var data = obj["data"].(map[string]any) %}
{%- var name = data["name"].(string) %}

{#- Iterating over interface slice #}
{%- for _, item := range items.([]any) %}
  {%- var m = item.(map[string]any) %}
  {{ m["name"] }}
{%- end %}
```

### Template Inclusion (render Operator)

The `render` operator includes and processes template files, returning a string representation.

**Basic syntax:**

```scriggo
{{ render "template-name" }}
```

**Path types:**

- Absolute paths: `{{ render "/templates/header.html" }}`
- Relative paths: `{{ render "../shared/footer.html" }}`

**Scope isolation:** By default, rendered templates cannot access variables from the calling template. This is intentional for encapsulation.

**Render as expression:** The render operator returns a string, so it can be used in assignments:

```scriggo
{%- var header = render "header.html" %}
```

**Default expression (error handling):** When a file might not exist, use the `default` clause:

```scriggo
{%- promo := render "promotions.html" default "No promotions" %}
{{ render "specials.html" default render "no-specials.html" }}
```

The default expression is only evaluated if the primary file cannot be found.

### Passing Variables with inherit_context (Fork Feature)

**This is a fork-specific feature not in upstream Scriggo.**

The `inherit_context` modifier allows rendered templates to access local variables from the calling scope:

```scriggo
{%- var name = "World" %}
{%- var count = 42 %}
{{ render "greeting.html" inherit_context }}

{#- In greeting.html, name and count are accessible: #}
{#- Hello {{ name }}, count is {{ count }} #}
```

**When to use inherit_context:**

- Passing context to included templates without restructuring as macros
- Quick prototyping before converting to proper macro parameters
- Sharing computed values across multiple snippet includes

**When NOT to use:**

- For reusable templates (use macros with explicit parameters instead)
- When you need clear documentation of dependencies
- For templates that might be used in different contexts

### render_glob (Fork Feature)

**This is a fork-specific feature not in upstream Scriggo.**

The `render_glob` operator renders all templates matching a glob pattern:

```scriggo
{{ render_glob "backend-*" }}
{{ render_glob "features-gateway-*" }}
{{ render_glob "widgets/*.html" }}
```

**Glob patterns supported:**

- `*` matches any sequence of characters (not including path separator)
- `?` matches any single character
- `[abc]` matches any character in the set

**With inherit_context:**

```scriggo
{%- var config = loadConfig() %}
{{ render_glob "plugins/*.html" inherit_context }}
```

**Default for no matches:**

```scriggo
{{ render_glob "optional-*.html" default "" }}
```

**How it works:**

1. At compile time, Scriggo expands the glob pattern against the template filesystem
2. Matching templates are rendered in sorted order (alphabetical)
3. If no templates match, returns empty string (or default if specified)
4. Outputs are concatenated verbatim; `on_own_lines(render_glob "x-*")` joins them so each starts on a new line (see Extension Point Reference)

**Example - rendering all backend snippets:**

```scriggo
{#- In base.yaml haproxyConfig template #}
{{ render_glob "backend-*" inherit_context }}

{#- This expands to render all matching snippets: #}
{#- backend-ingress, backend-gateway, backend-passthrough, etc. #}
```

### Exposing Custom Types to Templates

To use custom Go types in macro signatures, you must expose them via `reflect.TypeOf()` in the globals declarations. This is done in `pkg/templating/filters_scriggo.go`.

**Example - exposing a custom type:**

```go
// In filters_scriggo.go
import "reflect"

func registerScriggoRuntimeVars(decl native.Declarations) {
    // Variables (nil pointers for runtime injection)
    decl["resources"] = (*map[string]ResourceStore)(nil)

    // Types (for use in macro signatures)
    decl["ResourceStore"] = reflect.TypeOf(ResourceStore{})
    decl["MapStringAny"] = reflect.TypeOf(map[string]any{})
}
```

**Using the type in templates:**

```scriggo
{%- macro ProcessData(data MapStringAny) %}
  {#- data is now properly typed #}
  {%- var name = data["name"].(string) %}
{%- end %}
```

**Available global types for template use:**

| Declaration | Type | Purpose |
|-------------|------|---------|
| `resources` | `*map[string]ResourceStore` | Kubernetes resource stores |
| `pathResolver` | `*PathResolver` | File path resolution |
| `fileRegistry` | `*FileRegistrar` | Dynamic file registration |
| `shared` | `*templating.SharedContext` | Per-render cache; `shared.ComputeIfAbsent(key, fn)` memoises expensive work |
| `templateSnippets` | `*[]string` | Available snippet names |
| `globalFeatures` / `gf` | `map[string]any` | Cross-library shared state (see "Cross-Library Shared State" section) |

### Caching with shared.ComputeIfAbsent

For expensive computations that should run only once per render, use
`shared.ComputeIfAbsent(key, fn)` — a single thread-safe atomic call that
runs `fn` exactly once per render and returns the cached result on
subsequent calls:

```scriggo
{%- var result, _ = shared.ComputeIfAbsent("analysis_key", func() any {
    {#- Runs at most once per render, even with parallel sub-renders #}
    return expensive_computation()
}) %}
```

`ComputeIfAbsent` returns `(value, wasComputed)`. The boolean is useful for the
deduplication / `first_seen` pattern; ignore it (`_`) for plain memoisation.

### Deduplication with first_seen

The `first_seen` function atomically checks if a key has been seen before:

```scriggo
{%- for _, item := range items %}
  {%- if first_seen("backends", item.namespace, item.name) %}
    {#- Only runs for first occurrence of this namespace+name combination #}
    backend {{ item.namespace }}_{{ item.name }}
  {%- end %}
{%- end %}
```

### Sorting with sort_by

Sort slices using JSONPath criteria:

```scriggo
{%- var sorted = items | sort_by([]string{
  "$.priority:desc",           {#- Descending by priority #}
  "$.path.value | length:desc", {#- Descending by path length #}
  "$.name",                    {#- Ascending by name #}
}) %}
```

**Sort modifiers:**

- `:desc` - Descending order (default is ascending)
- `:exists` - Sort by field existence (exists first)
- `| length` - Sort by length of value

`sort_by` also takes a comparator, for orderings the criteria language can't
state. The sort is stable either way, and both forms preserve the element type:

```scriggo
{%- var byName = eps | sort_by(func(a EP, b EP) int {
  if a.TargetRef.Name < b.TargetRef.Name { return -1 }
  return 1
}) %}
```

Prefer criteria for multi-key ordering — `[]string{"$.priority:desc", "$.name"}`
is one line where the equivalent comparator is six.

### Collection pipelines (ADR-0018)

For the pure resource→text paths, chain type-preserving helpers instead of
nesting loops around a hand-rolled `map[string]bool{}`:

```scriggo
{%%
  var ready = resources.endpoints.List() |
    flat_map(s => s.Endpoints) |
    reject(e => e.TargetRef.Name == "") |
    unique_by(e => e.TargetRef.Name)
%%}
```

Every field access is checked at engine compile time, so a typo or a renamed
CRD field fails the load gate instead of rendering an empty map file.

**`x => expr` is the default spelling.** The parameter type is the stage's
element type and the result type is the expression's; both are inferred, both
are still checked. Write the long `func(e EP) bool { … }` form only when the
body needs more than one expression — and note that the long form is where a
stage can silently widen to `any`, which an arrow cannot do.

An arrow is accepted anywhere a function-typed parameter is, including a
chart-local helper: `Where(pods, p => p.Ready)` types `p` from `Where`'s own
signature.

**Nested types are nameable.** Alongside `resources.<name>.T`, each nested shape
is declared under its field path — `resources.endpoints.Endpoints`,
`resources.gateways.SpecListeners`. Arrows removed most of the need to spell
them; alias one (`type EP = …`) when a long-form closure still needs the name.
Resources without a loaded schema get no nested types, same as they get no
typed `T`.

**A macro cannot be a mid-chain stage.** It returns text, so it can end a chain
(`… | Render()`) or be a stage closure (`… | map(Label)`). A shared helper that
returns a *collection* is an exported `var` holding a func — imported like a
macro, with any return type (`map[string][]T` included).

**Rules that bite:**

- **Trailing pipes, not leading.** Go's semicolon insertion ends the statement
  otherwise: a line may end with `|`, but may not begin with one.
- **Chains live in `{%% %%}`**, not `{{ }}` — a show expression cannot span
  lines.
- **A pipe carries one value, which is what makes `sort_by` pipeable.** It
  returns `(value, error)`; the pipe keeps only the first, so
  `x | sort_by(…)` assigns to one variable. A *direct* call returns both and
  needs `var rows, err = sort_by(…)`.
- **`map` preserves length; `flat_map` concatenates.** Use `flat_map` when the
  closure returns a slice whose elements should be flattened in.

**When NOT to chain.** Anything with side effects — `fail()`, `gf[…] =`,
`fileRegistry.Register`, `WebhookRejectOrWarn`, `recordEvent`, `statusPatch` —
or that needs `break`. Those stay `{%% %%}` blocks with typed accumulators.
Chains longer than ~5 stages should also drop to a block. `features-075-address-discovery`
in `base/library.yaml` is the worked example: a `flat_map | reject | unique_by | map`
pipeline for the LoadBalancer address list, then a plain block for the fallback
that needs the whole list at once.

### Whitespace Control

Control whitespace around template tags:

- `{%-` Strip whitespace before tag
- `-%}` Strip whitespace after tag

```scriggo
{%- for _, item := range items -%}
{{ item }}
{%- end -%}
```

**Both flavours strip ALL adjacent whitespace including newlines** —
Jinja2-compatible "trim across line boundaries" semantics
(`internal/compiler/parser.go:trimAllTrailing` / `trimAllLeading` in
the Scriggo fork). This applies equally to comment blocks:

| Form | Effect on surrounding whitespace |
|------|----------------------------------|
| `{# ... #}` | Preserve both sides |
| `{#- ... #}` | Strip ALL leading whitespace (incl. newline) |
| `{# ... -#}` | Strip ALL trailing whitespace (incl. newline) |
| `{#- ... -#}` | Strip ALL whitespace (incl. newlines) on both sides |

When the directive following a comment block must remain on its own
line (e.g. an HAProxy `# section-marker` followed by `http-request
set-var ...`), use the non-stripping `{# ... #}` form. The stripping
form will fuse the marker with the directive into a single comment
line and silently drop the directive — visible in the rendered config
as `# section-markerhttp-request set-var(...)` (no separator).

### Scriggo vs Jinja2 Syntax Comparison

See also: <https://scriggo.com/templates/switch-from-jinja-to-scriggo>

**Key differences:**

| Feature | Jinja2 | Scriggo |
|---------|--------------|---------|
| Type system | Dynamic | Static (compile-time type checking) |
| Variable declaration | `{% set x = 1 %}` | `{% var x = 1 %}` or `{% x := 1 %}` |
| Variable reassignment | `{% set x = 2 %}` | `{% x = 2 %}` |
| End tags | `{% endif %}`, `{% endfor %}` | `{% end %}` or `{% end if %}`, `{% end for %}` |
| String concat | `x ~ y` | `x + y` |
| Default value | `x \| default(y)` | `x default y` or `fallback(x, y)` |
| Length | `x \| length` | `len(x)` |
| Import macro | `{% from "x" import y %}` | `{% import "x" for y %}` |
| Include | `{% include "x" %}` | `{{ render "x" }}` |
| Mutable state | `namespace(a=1)` | `map[string]any{"a": 1}` |

**Data structure syntax:**

| Type | Jinja2 | Scriggo |
|------|--------------|---------|
| List/Array | `[1, 2, 3]` | `[]int{1, 2, 3}` (typed) |
| Dictionary/Map | `{'key': 'value'}` | `map[string]string{"key": "value"}` |
| Tuple | `(1, 5, 2020)` | Use slice or map |

**Operator differences:**

| Operation | Jinja2 | Scriggo |
|-----------|--------------|---------|
| Logical AND/OR | Returns operand | Returns boolean |
| Contains check | `1 in [1, 2, 3]` | `[]int{1, 2, 3} contains 1` (reversed) |
| Conditional expr | `x if cond else y` | Use if statement |

**Loop syntax:**

```scriggo
{#- Jinja2 #}
{% for item in items %}
  {{ item }}
{% endfor %}

{#- Scriggo (for-in) #}
{% for item in items %}
  {{ item }}
{% end %}

{#- Scriggo (for-range with index) #}
{% for i, item := range items %}
  {{ i }}: {{ item }}
{% end %}
```

**Filter to function migration:**

| Jinja2 Filter | Scriggo Function |
|---------------|------------------|
| `{{ value\|abs }}` | `{{ abs(value) }}` |
| `{{ value\|length }}` | `{{ len(value) }}` |
| `{{ foo\|attr("bar") }}` | `{{ foo.bar }}` |
| `{{ items\|join(",") }}` | `{{ join(items, ",") }}` |
| `{{ s\|upper }}` | `{{ toUpper(s) }}` |
| `{{ s\|lower }}` | `{{ toLower(s) }}` |
| `{{ s\|capitalize }}` | `{{ capitalize(s) }}` |

**HTML escaping:**

- Jinja2: Auto-escapes by default, use `{{ value\|safe }}` to disable
- Scriggo: Auto-escapes in HTML context, cast to `html` type: `{{ html("<b>Bold</b>") }}`

**Block assignments (Jinja2 call blocks):**

```scriggo
{#- Jinja2 #}
{% set content %}
  HTML content here
{% endset %}

{#- Scriggo #}
{% var content = itea; using %}
  HTML content here
{% end using %}
```

## Common Pitfalls

### Overriding haproxyConfig in Libraries

**Problem**: Adding `haproxyConfig` to a library file.

```yaml
# ingress.yaml - WRONG!
haproxyConfig:
  template: |
    # This will override base.yaml's template!
```

**Why Bad**: The library merge uses `mustMergeOverwrite`, so your library's `haproxyConfig` will completely replace base.yaml's template, breaking other libraries.

**Solution**: Only define `templateSnippets`, let base.yaml call them via `{{ render "snippet-name" }}`.

### Testing Individual Library Files

**Problem**: Running `controller validate` directly on a library file.

```bash
# WRONG - library file is incomplete!
./bin/haptic validate -f charts/haptic/charts/ingress/library.yaml
```

**Why Bad**: Library files are meant to be merged. Testing them individually will fail because:

- Missing base template (`haproxyConfig`)
- Missing snippets from other libraries
- Missing watched resources from other libraries

**Solution**: Always test the merged Helm output:

```bash
# CORRECT
helm template charts/haptic \
  | yq 'select(.kind == "HAProxyTemplateConfig")' \
  | ./bin/haptic validate -f -
```

### Missing watchedResources

**Problem**: Template uses resources not declared in `watchedResources`.

```yaml
templateSnippets:
  my-snippet:
    template: |
      {%- for _, svc := range resources.services.List() %}
      # ERROR: services not in watchedResources!
```

**Solution**: Declare all used resources:

```yaml
watchedResources:
  services:
    apiVersion: v1
    resources: services
    indexBy: ["metadata.namespace", "metadata.name"]
```

### Inconsistent Shared State Map Keys

**Problem**: Using different key names for the same shared state across libraries.

```scriggo
{#- ssl.yaml initializes with snake_case #}
{%- gf["tls_certificates"] = []any{} %}

{#- ingress.yaml writes with camelCase - WRONG KEY! #}
{%- gf["tlsCertificates"] = append(gf["tlsCertificates"].([]any), cert) %}

{#- ssl.yaml reads snake_case - gets empty array! #}
{%- var certs = gf["tls_certificates"] %}  {# Empty because ingress wrote to different key #}
```

**Why Bad**: Go maps are case-sensitive. `tls_certificates` and `tlsCertificates` are completely different keys. Data written to one key is invisible when reading the other. This causes subtle bugs where features silently fail.

**Solution**: Use consistent **camelCase** for all shared state keys:

```scriggo
{#- All libraries use the same key name #}
{%- gf["tlsCertificates"] = []any{} %}           {# ssl.yaml initializes #}
{%- gf["tlsCertificates"] = append(..., cert) %}  {# ingress.yaml writes #}
{%- var certs = gf["tlsCertificates"] %}          {# ssl.yaml reads #}
```

**Canonical Key Names:**

| Correct (camelCase) | Wrong (various) |
|---------------------|-----------------|
| `tlsCertificates` | `tls_certificates`, `TLSCertificates` |
| `sslPassthroughBackends` | `ssl_passthroughBackends`, `sslPassthrough_backends`, `ssl_passthrough_backends` |

### Annotation Ownership

**Problem**: Processing annotations in the wrong library.

```yaml
# ingress.yaml - WRONG!
templateSnippets:
  backends-500-ingress:
    template: |
      {#- This annotation belongs in haproxytech.yaml! #}
      {%- var snippet = ingress | dig("metadata", "annotations", "haproxy.org/backend-config-snippet") %}
```

**Why Bad**: The `haproxy.org/*` annotations are HAProxy-specific compatibility features. Placing them in ingress.yaml:

- Violates separation of concerns
- Makes annotation documentation harder to find
- Prevents the gateway/ library from using the same annotations

**Solution**: Process annotations in the library that owns them:

| Annotation Prefix | Owner Library |
|-------------------|---------------|
| `haproxy.org/*` | haproxytech.yaml |
| `haproxy-ingress.github.io/*` | haproxy-ingress/ |
| `nginx.ingress.kubernetes.io/*` | nginx-ingress/ |
| (none - standard fields) | ingress.yaml, gateway/ |

**Pattern for Annotation Libraries:**

```yaml
# haproxytech.yaml - processes haproxy.org/* annotations
templateSnippets:
  backend-directives-900-haproxytech-advanced:
    template: |
      {%- if ingress != nil %}
        {#- All haproxy.org/* annotations handled here #}
        {%- var snippet = ingress | dig("metadata", "annotations", "haproxy.org/backend-config-snippet") | fallback("") %}
        {%- if snippet != "" %}
      # haproxytech/backend-config-snippet
      {{ snippet }}
        {%- end %}
      {%- end %}
```

The annotation library receives the `ingress` variable via `inherit_context` from the calling snippet (backends-500-ingress).

### JSONPath Escaping in Labels

**Problem**: Label keys with dots (like `kubernetes.io/service-name`) break JSONPath.

```yaml
# WRONG
indexBy: ["metadata.labels.kubernetes.io/service-name"]
# Error: JSONPath thinks "io" is a field of "kubernetes"
```

**Solution**: Escape dots with double backslash:

```yaml
# CORRECT
indexBy: ["metadata.labels.kubernetes\\.io/service-name"]
```

## Chart Files Overview

```
charts/haptic/
├── Chart.yaml                   # Helm chart metadata
├── values.yaml                  # Default configuration values
├── README.md                    # User-facing chart documentation
├── CLAUDE.md                    # This file - development context
│
├── charts/                      # Template libraries, one subchart each
│   ├── base/library.yaml       # Core HAProxy template (defines haproxyConfig)
│   ├── ssl/library.yaml        # HTTPS frontend, TLS certs, SSL passthrough
│   ├── ingress/library.yaml    # Kubernetes Ingress support
│   ├── gateway/                # Gateway API support (split library: _index.yaml + fragments)
│   ├── ingress-annotations-compat/library.yaml  # Shared scaffold for Ingress vendor annotation libraries
│   ├── governance/library.yaml # Declarative constraints over any watched resource
│   ├── haptic-annotations/     # haproxy-haptic.org/* native vocabulary (split library)
│   ├── haproxytech/library.yaml  # HAProxy annotation compatibility
│   ├── haproxy-ingress/        # haproxy-ingress annotation compatibility
│   ├── nginx-ingress/          # nginx-ingress annotation compatibility (disabled by default)
│   ├── spoa-hub/               # SPOA hub sidecar wiring (auto-enabled with spoaHub)
│   └── vector/library.yaml     # Vector sidecar config
│
├── templates/                   # Helm templates
│   ├── _libraries.tpl          # Library loading (haptic.prepareLibraries, haptic.watchedResourcesUnion)
│   ├── _naming.tpl             # Names, labels, apiGroup/apiVersion split
│   ├── _image.tpl              # Image refs, binary paths, runAsUser
│   ├── _credentials.tpl        # Agent username/password
│   ├── _resources.tpl          # CPU/mem math, nbthread, GOMAXPROCS, checksums
│   ├── _spoa-hub.tpl           # SPOA-hub helpers (enabled/disabled, image, libName)
│   ├── _pod-spec.tpl           # Shared pod-spec scheduling/runtime fields
│   ├── haproxytemplateconfig.yaml  # Renders merged HAProxyTemplateConfig CRD
│   ├── deployment.yaml         # Controller deployment
│   ├── service.yaml            # Controller service
│   ├── clusterrole.yaml        # RBAC permissions
│   └── ...                     # Other K8s resources
│
└── crds/                        # Custom Resource Definitions
    ├── haproxy-haptic.org_haproxytemplateconfigs.yaml
    ├── haproxy-haptic.org_haproxytemplatelibraries.yaml
    ├── haproxy-haptic.org_haproxycfgs.yaml
    ├── haproxy-haptic.org_haproxymapfiles.yaml
    ├── haproxy-haptic.org_haproxygeneralfiles.yaml
    └── haproxy-haptic.org_haproxycrtlistfiles.yaml
```

## Debugging Tips

### View Merged Template Output

```bash
# See the complete merged HAProxyTemplateConfig
helm template charts/haptic \
  | yq 'select(.kind == "HAProxyTemplateConfig")'
```

### Check Template Snippet Merging

```bash
# Extract just the templateSnippets section
helm template charts/haptic \
  | yq 'select(.kind == "HAProxyTemplateConfig") | .spec.templateSnippets | keys'
```

### Verify watchedResources

```bash
# See which resources will be watched
helm template charts/haptic \
  | yq 'select(.kind == "HAProxyTemplateConfig") | .spec.watchedResources | keys'
```

### Test Specific Library Combinations

```bash
# Disable all libraries, enable only one
helm template charts/haptic \
  --set controller.templateLibraries.ingress.enabled=false \
  --set controller.templateLibraries.gateway.enabled=false \
  --set controller.templateLibraries.haproxytech.enabled=true \
  | yq 'select(.kind == "HAProxyTemplateConfig")'
```

## Resources

- Helm template reference: <https://helm.sh/docs/chart_template_guide/>
- yq documentation: <https://github.com/mikefarah/yq>
- HAProxyTemplateConfig CRD: `crds/haproxy-haptic.org_haproxytemplateconfigs.yaml`
- Controller validation: `pkg/controller/testrunner/CLAUDE.md`
- Template engine: `pkg/templating/CLAUDE.md`

## Changelog Guidelines

Helm chart changes are documented in the root `CHANGELOG.md`, under the `### Helm chart` subsection of each release (with `#### Added`/`#### Changed`/… sub-headings). `charts/haptic/CHANGELOG.md` is only a pointer to the root changelog. Keep entries concise - one line per change, focus on what changed. Avoid verbose justifications or explanations in parentheses.

**Belongs in the `### Helm chart` subsection:**

- New Helm values and configuration options
- Changes to default values (replicas, resources, etc.)
- Service configuration changes (ports, types, annotations)
- RBAC and security context changes
- Template library additions/changes
- CRD updates

**Belongs in the release's top-level sections instead:**

- Controller behavior and features

**Exclude entirely:**

- Internal implementation details
- Development workflow changes

**Don't call changes "BREAKING" when the feature being broken was itself introduced after the last released chart version.** The CHANGELOG is read by operators upgrading between released chart versions; if the affected behavior never shipped to a real release, the only people impacted are snapshot/main consumers — note the change but don't tag it as a `BREAKING` migration. Check `git tag -l 'v*' | sort -V | tail` for the latest release (chart releases up to 0.1.0 used `haptic-chart-v*` tags) and `git log <last-tag>..HEAD -- charts/haptic/` for post-release chart changes.
