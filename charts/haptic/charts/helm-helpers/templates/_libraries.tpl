{{/*
Library loading — the chart-time loader for the library subcharts under
charts/haptic/charts/.

Each library declares its loading rules in a top-level `_helm_load:` block
(enable predicate, optional injects, optional unsets). The loader iterates a
fixed ordered list (the merge order is a system property and lives here, not in
the library files). For each library it: parses YAML, evaluates `enable`,
applies injects (each gated by optional `when`), applies unsets, strips
`_helm_load` and every other underscore-prefixed top-level key, and applies
`haptic.filterTests` universally (no-op for libraries with no
`_helm_skip_test`).

**The chart no longer merges libraries.** Each becomes its own
HAProxyTemplateLibrary (content only, tests inline); the operator's
HAProxyTemplateConfig references them in spec.libraryRefs and the controller
merges that order, later wins, with per-source validationTests union and
duplicate-name errors. The duplicate check below is the same guard one stage
earlier — a collision fails `helm template` instead of the controller's load.

Schema for `_helm_load:` is documented in charts/CLAUDE.md. See ADR-0002
(docs/adr/0002-decentralized-helm-library-loader.md) for the design rationale.
*/}}

{{/*
Mutates the library's validationTests in place; callers retain the parsed map.
Args: [library dict, root context].
*/}}
{{- define "haptic.filterTests" -}}
{{- $library := index . 0 }}
{{- $context := index . 1 }}
{{- if $library.validationTests }}
  {{- $filteredTests := dict }}
  {{- range $testName, $testDef := $library.validationTests }}
    {{- /* Drop tests whose _helm_skip_test expression renders to "true"; keep the rest with the marker stripped. */ -}}
    {{- if not (and $testDef._helm_skip_test (eq (tpl ($testDef._helm_skip_test | toString) $context | trim) "true")) }}
      {{- /* Also strip `description`: it's documentation-only metadata the
             testrunner echoes on failure, not part of the test logic. The
             test name already identifies the case. Dropping it from the
             rendered HAProxyTemplateConfig keeps the Helm release Secret
             clear of the K8s 1 MiB limit. This applies at BOTH levels: the
             test-level description and each assertion's `description` (the
             assertion's type/target/pattern fully define it). Together these
             sum to ~120 KB across the bundled libraries. */ -}}
      {{- $stripped := omit $testDef "_helm_skip_test" "description" }}
      {{- if $stripped.assertions }}
        {{- $cleanAssertions := list }}
        {{- range $assertion := $stripped.assertions }}
          {{- $cleanAssertions = append $cleanAssertions (omit $assertion "description") }}
        {{- end }}
        {{- $_ := set $stripped "assertions" $cleanAssertions }}
      {{- end }}
      {{- $_ := set $filteredTests $testName $stripped }}
    {{- end }}
  {{- end }}
  {{- $_ := set $library "validationTests" $filteredTests }}
{{- end }}
{{- end }}

{{/*
Returns "true" when the nginx-ingress template library is disabled (empty
string otherwise), mirroring `haptic.spoaHub.disabled`. validationTests whose
fixtures rely on nginx.ingress.kubernetes.io/* annotations — scanned only by
the nginx-ingress library's util-nginx-ingress-coraza-scan — use this in their
`_helm_skip_test` predicate so they don't run (and fail) when the library is
absent from the bundle.
*/}}
{{- define "haptic.nginxIngress.disabled" -}}
{{- if not .Values.controller.templateLibraries.nginxIngress.enabled -}}true{{- end -}}
{{- end -}}

{{/*
Returns "true" unless every Ingress vendor annotation library is in the bundle
(empty string when all are). A validationTest that pins which family wins when
several annotate one host needs all of them loaded: with a family absent its
producer never runs, the remaining one wins by default, and the test passes
while asserting nothing. Use it in `_helm_skip_test` for cross-family
precedence tests.
*/}}
{{- define "haptic.vendorAnnotationLibraries.incomplete" -}}
{{- $libs := .Values.controller.templateLibraries -}}
{{- if not (and $libs.haproxyIngress.enabled $libs.nginxIngress.enabled $libs.haproxytech.enabled) -}}true{{- end -}}
{{- end -}}

{{/*
Returns "true" when the Gateway API *experimental* channel is NOT in use (empty
string otherwise), driven by the explicit `templateLibraries.gateway.experimentalChannel`
value. Experimental-only HTTPRoute fields (sessionPersistence / GEP-1619, retry /
GEP-1731) only exist when experimental-install.yaml is applied, so validationTests
asserting the directives those fields drive use this in their `_helm_skip_test`
predicate — otherwise they fail (and, with the fatal load gate, keep the
controller from starting) on a standard-channel cluster where the snippets correctly emit nothing.

Why a value and not Helm .Capabilities: as of Gateway API v1.6 the Standard and
Experimental installs ship an IDENTICAL CRD set (TCPRoute, ListenerSet, UDPRoute
et al. all graduated to Standard). The only difference is field-level additions
to HTTPRoute, and .Capabilities exposes GVKs, not fields — so no APIVersions.Has
check can tell the channels apart anymore. The operator declares which channel
they installed; offline renders (CI, scripts/test-templates.sh) set the flag true
to exercise these tests against the experimental schemas in tests/schemas.
*/}}
{{- define "haptic.gatewayExperimental.disabled" -}}
{{- if not .Values.controller.templateLibraries.gateway.experimentalChannel -}}true{{- end -}}
{{- end -}}

{{/*
Set a value at a dotted path within a dict, creating intermediate dicts as needed.
Args (list): [obj dict, path string, value any]
Returns: empty (mutates obj by side effect)
*/}}
{{- define "haptic.setNested" -}}
{{- $cursor := index . 0 -}}
{{- $value := index . 2 -}}
{{- $parts := splitList "." (index . 1) -}}
{{- $lastIdx := sub (len $parts) 1 -}}
{{- range $idx, $part := $parts -}}
  {{- if lt $idx $lastIdx -}}
    {{- if not (hasKey $cursor $part) -}}
      {{- $_ := set $cursor $part dict -}}
    {{- end -}}
    {{- $cursor = index $cursor $part -}}
  {{- else -}}
    {{- $_ := set $cursor $part $value -}}
  {{- end -}}
{{- end -}}
{{- end }}

{{/*
Get a value at a dotted path within a dict, returning JSON-encoded.
Callers do `(include "haptic.getNested" ... | fromJson)` to round-trip dicts/lists.
Args (list): [obj dict, path string]
Returns: JSON-encoded value, or "null" if path missing.
*/}}
{{- define "haptic.getNested" -}}
{{- $cursor := index . 0 -}}
{{- $parts := splitList "." (index . 1) -}}
{{- $reachable := true -}}
{{- range $part := $parts -}}
  {{- if and $reachable (kindIs "map" $cursor) (hasKey $cursor $part) -}}
    {{- $cursor = index $cursor $part -}}
  {{- else -}}
    {{- $reachable = false -}}
  {{- end -}}
{{- end -}}
{{- if $reachable -}}{{ $cursor | toJson }}{{- else -}}null{{- end -}}
{{- end }}

{{/*
Unset a key at a dotted path within a dict. No-op if path missing.
Args (list): [obj dict, path string]
Returns: empty (mutates obj by side effect)
*/}}
{{- define "haptic.unsetNested" -}}
{{- $cursor := index . 0 -}}
{{- $parts := splitList "." (index . 1) -}}
{{- $lastIdx := sub (len $parts) 1 -}}
{{- $reachable := true -}}
{{- range $idx, $part := $parts -}}
  {{- if lt $idx $lastIdx -}}
    {{- if and $reachable (kindIs "map" $cursor) (hasKey $cursor $part) -}}
      {{- $cursor = index $cursor $part -}}
    {{- else -}}
      {{- $reachable = false -}}
    {{- end -}}
  {{- else if $reachable -}}
    {{- $_ := unset $cursor $part -}}
  {{- end -}}
{{- end -}}
{{- end }}

{{/*
Project `_migrationCoverage` into extraContext.knownAnnotations.<source> =
{prefixes, names without prefix} for features-955-annotation-unknown-key.
Args: library dict (mutated)
*/}}
{{- define "haptic.projectKnownAnnotations" -}}
{{- $library := . -}}
{{- $known := dict -}}
{{- range $coverage := $library._migrationCoverage | default list -}}
  {{- $prefixes := $coverage.detect.annotationPrefixes | default list -}}
  {{- $names := list -}}
  {{- range $key := keys ($coverage.annotations | default dict) | sortAlpha -}}
    {{- range $prefix := $prefixes -}}
      {{- if hasPrefix $prefix $key -}}
        {{- $names = append $names (trimPrefix $prefix $key) -}}
      {{- end -}}
    {{- end -}}
  {{- end -}}
  {{- $_ := set $known $coverage.source (dict "prefixes" $prefixes "names" $names) -}}
{{- end -}}
{{- if $known -}}
  {{- include "haptic.setNested" (list $library "templatingSettings.extraContext.knownAnnotations" $known) -}}
{{- end -}}
{{- end }}

{{/*
Load every enabled template library and return it prepared but NOT merged, as
`libraries: [{name, config}, ...]` in merge order. Each `config` is a spec
fragment ready to be rendered as its own HAProxyTemplateConfig.

Every entry in `$libraryFiles` is a "subchart:<name>" today. A subchart holding
a single library.yaml is read directly; one holding an `_index.yaml` is a "split
library" whose contents are spread across fragment files: `_index.yaml` carries
the load rules (`_helm_load`), and fragments at the same level (and one level
deep) merge into the library accumulator in lexicographic order before any
inject / unset / strip happens. Fragments must NOT carry their own
`_helm_load` — the convention is `_index.yaml`-owns-load-rules,
fragments-own-content. See ADR-0008.

The loader also still accepts a flat path ("libraries/x.yaml") and a split
directory ("libraries/x/") in the parent chart. No library uses either since the
subchart move; they are kept so a library can be added without a subchart, at
the cost of its source being stored in the release Secret.
*/}}
{{- define "haptic.prepareLibraries" -}}
{{- $prepared := list }}
{{- $declared := dict }}
{{- $context := .context }}
{{- $templateLibraries := $context.Values.controller.templateLibraries }}
{{- if not $templateLibraries.kubernetesBackends.enabled }}
  {{- $consumers := list }}
  {{- range $name := list "ingress" "gateway" "ingressAnnotationsCompat" "hapticAnnotations" "haproxyIngress" "nginxIngress" }}
    {{- if (dig $name "enabled" false $templateLibraries) }}
      {{- $consumers = append $consumers $name }}
    {{- end }}
  {{- end }}
  {{- if gt (len $consumers) 0 }}
    {{- fail (printf "controller.templateLibraries.kubernetesBackends.enabled=false leaves enabled consumers without Service/EndpointSlice helpers: %s. Enable kubernetesBackends or disable those libraries." (join ", " $consumers)) }}
  {{- end }}
{{- end }}
{{- if not $templateLibraries.ingressAnnotationsCompat.enabled }}
  {{- $consumers := list }}
  {{- range $name := list "hapticAnnotations" "haproxytech" "haproxyIngress" "nginxIngress" }}
    {{- if (dig $name "enabled" false $templateLibraries) }}
      {{- $consumers = append $consumers $name }}
    {{- end }}
  {{- end }}
  {{- if gt (len $consumers) 0 }}
    {{- fail (printf "controller.templateLibraries.ingressAnnotationsCompat.enabled=false leaves enabled consumers without shared annotation macros: %s. Enable ingressAnnotationsCompat or disable those libraries." (join ", " $consumers)) }}
  {{- end }}
{{- end }}
{{- /* Each entry is "subchart:<name>" — a subchart whose library YAML the
       parent reads via .Subcharts.<name>.Files. A subchart disabled by its
       `condition:` is pruned from the release Secret, so .Subcharts.<name> is
       absent and the entry is skipped. A subchart with a single library.yaml is
       read directly; one with an _index.yaml is a split-library (fragments
       merged in lexicographic order). The order below IS the merge order. */ -}}
{{- $libraryFiles := list
    "subchart:base"
    "subchart:kubernetes-backends"
    "subchart:ssl"
    "subchart:ingress"
    "subchart:http-policies"
    "subchart:gateway"
    "subchart:gateway-policies"
    "subchart:ingress-annotations-compat"
    "subchart:governance"
    "subchart:haptic-annotations"
    "subchart:haproxytech"
    "subchart:haproxy-ingress"
    "subchart:nginx-ingress"
    "subchart:custom-crd-example"
    "subchart:spoa-hub"
    "subchart:vector"
}}
{{- /* vector is last on purpose: it contributes only its own `files:` entry (the
       vector config) and reads what the chart projects into extraContext, so it
       depends on every earlier library's port/feature decisions and nothing
       depends on it. A subchart because its source is ~97 KB the release Secret
       would otherwise store on top of the rendered copy. */}}
{{- range $file := $libraryFiles }}
  {{- $library := dict }}
  {{- $skip := false }}
  {{- if hasPrefix "subchart:" $file }}
    {{- $name := trimPrefix "subchart:" $file }}
    {{- $sub := index $context.Subcharts $name }}
    {{- if not $sub }}
      {{- /* subchart disabled (its condition is false) → pruned from the
             release; nothing to read or merge. */ -}}
      {{- $skip = true }}
    {{- else if $sub.Files.Get "_index.yaml" }}
      {{- /* split-library subchart: _index.yaml + fragments */ -}}
      {{- $library = $sub.Files.Get "_index.yaml" | fromYaml }}
      {{- $fragmentPaths := list }}
      {{- range $path, $_ := $sub.Files.Glob "*.yaml" }}
        {{- /* exclude _index.yaml (the load authority, already read) and the
               subchart's own Chart.yaml/values.yaml metadata */ -}}
        {{- if not (or (eq $path "_index.yaml") (eq $path "Chart.yaml") (eq $path "values.yaml")) }}
          {{- $fragmentPaths = append $fragmentPaths $path }}
        {{- end }}
      {{- end }}
      {{- range $path, $_ := $sub.Files.Glob "*/*.yaml" }}
        {{- $fragmentPaths = append $fragmentPaths $path }}
      {{- end }}
      {{- range $fragmentPath := sortAlpha $fragmentPaths }}
        {{- $fragment := $sub.Files.Get $fragmentPath | fromYaml }}
        {{- if $fragment._helm_load }}
          {{- fail (printf "split-library fragment %q must not declare _helm_load (only _index.yaml does)" $fragmentPath) }}
        {{- end }}
        {{- $library = mustMergeOverwrite $library $fragment }}
      {{- end }}
    {{- else }}
      {{- /* single-file subchart */ -}}
      {{- $library = $sub.Files.Get "library.yaml" | fromYaml }}
      {{- if not $library }}
        {{- fail (printf "subchart %q has neither _index.yaml nor library.yaml" $name) }}
      {{- end }}
    {{- end }}
  {{- else if hasSuffix "/" $file }}
    {{- /* Split library: read _index.yaml as the load-rule authority, */ -}}
    {{- /* then merge fragments in lexicographic order: top-level YAML */ -}}
    {{- /* files plus any one-level-deep YAML files under subdirs.     */ -}}
    {{- /* _index.yaml is excluded from the fragment set. We collect-  */ -}}
    {{- /* then-sort because Helm's Files.Glob returns a map whose     */ -}}
    {{- /* iteration order is unspecified.                             */ -}}
    {{- $indexPath := printf "%s_index.yaml" $file }}
    {{- $library = $context.Files.Get $indexPath | fromYaml }}
    {{- if not $library }}
      {{- fail (printf "split library %q is missing %s" $file $indexPath) }}
    {{- end }}
    {{- $fragmentPaths := list }}
    {{- range $path, $_ := $context.Files.Glob (printf "%s*.yaml" $file) }}
      {{- if ne $path $indexPath }}
        {{- $fragmentPaths = append $fragmentPaths $path }}
      {{- end }}
    {{- end }}
    {{- range $path, $_ := $context.Files.Glob (printf "%s*/*.yaml" $file) }}
      {{- $fragmentPaths = append $fragmentPaths $path }}
    {{- end }}
    {{- range $fragmentPath := sortAlpha $fragmentPaths }}
      {{- $fragment := $context.Files.Get $fragmentPath | fromYaml }}
      {{- if $fragment._helm_load }}
        {{- fail (printf "split-library fragment %q must not declare _helm_load (only _index.yaml does)" $fragmentPath) }}
      {{- end }}
      {{- $library = mustMergeOverwrite $library $fragment }}
    {{- end }}
  {{- else }}
    {{- $library = $context.Files.Get $file | fromYaml }}
  {{- end }}
  {{- $loadHints := $library._helm_load | default dict }}
  {{- if and (not $skip) $library (eq (tpl ($loadHints.enable | default "true") $context | trim) "true") }}
    {{- range $inject := $loadHints.inject | default list }}
      {{- if eq (tpl ($inject.when | default "true") $context | trim) "true" }}
        {{- if hasKey $inject "from" }}
          {{- include "haptic.setNested" (list $library $inject.path (include "haptic.getNested" (list $library $inject.from) | fromJson)) }}
        {{- else }}
          {{- include "haptic.setNested" (list $library $inject.path (tpl ($inject.value | toString) $context)) }}
        {{- end }}
      {{- end }}
    {{- end }}
    {{- range $unsetItem := $loadHints.unset | default list }}
      {{- /* An unset item is either a bare dotted-path string (always removed)
             or a {path, when} map (removed only when `when` tpls to "true").
             The conditional form lets a library strip resource-specific
             watches/snippets/tests when their optional CRD is absent. */ -}}
      {{- if kindIs "string" $unsetItem }}
        {{- include "haptic.unsetNested" (list $library $unsetItem) }}
      {{- else if eq (tpl ($unsetItem.when | default "true") $context | trim) "true" }}
        {{- include "haptic.unsetNested" (list $library $unsetItem.path) }}
      {{- end }}
    {{- end }}
    {{- $_ := unset $library "_helm_load" }}
    {{- include "haptic.projectKnownAnnotations" $library }}
    {{- $_ := include "haptic.filterTests" (list $library $context) }}
    {{- /* Underscore-prefixed top-level keys are chart-time-only scratch
           (ssl.yaml's _test_tls_* YAML anchors); they are not CRD fields and
           server-side apply would reject the object carrying them. */ -}}
    {{- range $key := keys $library }}
      {{- if hasPrefix "_" $key }}
        {{- $_ := unset $library $key }}
      {{- end }}
    {{- end }}
    {{- /* A name two LIBRARIES both declare is an error here — one render
           instead of one failed load. The controller enforces the same rule
           across objects (the operator's config, last in CRD_NAME, is the one
           documented override point; these are not it). _global is the shared
           baseline several libraries each contribute part of. */ -}}
    {{- range $key := list "templateSnippets" "validationTests" "maps" "files" "sslCertificates" "k8sResources" }}
      {{- range $name, $_ := (index $library $key | default dict) }}
        {{- if and (ne $name "_global") (hasKey (index $declared $key | default dict) $name) }}
          {{- fail (printf "template library %s redefines %s.%s, which %s already declared: one definition would silently replace the other and never run" $file $key $name (index (index $declared $key) $name)) }}
        {{- end }}
        {{- if not (hasKey $declared $key) }}{{- $_ := set $declared $key dict }}{{- end }}
        {{- $_ := set (index $declared $key) $name (toString $file) }}
      {{- end }}
    {{- end }}
    {{- $slug := $file }}
    {{- if hasPrefix "subchart:" $file }}
      {{- $slug = trimPrefix "subchart:" $file }}
    {{- else }}
      {{- $slug = $file | trimSuffix "/" | base | trimSuffix ".yaml" }}
    {{- end }}
    {{- $prepared = append $prepared (dict "name" $slug "config" $library) }}
  {{- end }}
{{- end }}
{{- $_ := set .result "libraries" $prepared }}
{{- end }}

{{/*
haptic.watchedResourcesUnion — the union of every enabled library's
watchedResources plus the operator's, operator winning. For the templates that
must reason about the whole watch set: the ClusterRole and the
ValidatingWebhookConfiguration. Retains the Helm-only `statusPatch` field,
which the object emitter strips. controller.rbac.releaseNamespaceSecretsOnly
scopes the secrets watch here, so every consumer sees the same namespaces.
*/}}
{{- define "haptic.watchedResourcesUnion" -}}
{{- /* Memoized: four templates read it, and each computation renders every library. */ -}}
{{- if not (hasKey .Values "_watchedResourcesUnion") }}
{{- $union := dict }}
{{- $prepared := dict }}
{{- $_ := include "haptic.prepareLibraries" (dict "context" . "result" $prepared) }}
{{- range $library := $prepared.libraries }}
  {{- $union = merge $union ($library.config.watchedResources | default dict) }}
{{- end }}
{{- $union = merge (deepCopy (.Values.controller.config.watchedResources | default dict)) $union }}
{{- include "haptic.applySecretScope" (list $union .) }}
{{- $_ := set .Values "_watchedResourcesUnion" ($union | toYaml) }}
{{- end }}
{{- get .Values "_watchedResourcesUnion" }}
{{- end }}

{{/*
True when the frontend should compute txn.route, the matched route key. A
predicate rather than `if tracing` because two features want it: spans for
http.route, request metrics for their `path` label. It costs a four-step lookup
cascade per request. Mirrored Scriggo-side by util-want-route, for a CR that
bypasses Helm.
*/}}
{{- define "haptic.routeKeyEnabled" -}}
{{- $ec := .Values.controller.config.templatingSettings.extraContext | default dict -}}
{{- if dig "tracing" "enabled" false $ec -}}
true
{{- else -}}
  {{- $rm := .Values.vector.requestMetrics | default dict -}}
  {{- if and .Values.vector.enabled $rm.enabled $rm.pathLabel -}}
true
  {{- end -}}
{{- end -}}
{{- end -}}
