{{/*
SPOA hub plugin enablement, image reference, ConfigMap name, and
plugin shared-library filename helpers.
*/}}

{{/*
Resolve a single plugin's `enabled` value to true/false. The field is
allowed to be either a literal bool (operator override via values or
--set) OR a templated string (default in values.yaml; the chart evaluates
it with `tpl` so library-driven auto-enable conditions can live in the
default value itself).
Args: dict "plugin" <plugin map> "root" $
*/}}
{{- define "haptic.spoaHub.pluginEnabled" -}}
{{- $val := (default dict .plugin).enabled -}}
{{- if eq (kindOf $val) "bool" -}}
  {{- if $val -}}true{{- end -}}
{{- else if eq (kindOf $val) "string" -}}
  {{- $resolved := trim (tpl $val .root) -}}
  {{- if not (has $resolved (list "true" "false")) -}}
    {{- fail (printf "spoaHub.plugins.%s.enabled must be a boolean or a template resolving exactly to true or false; got %q." (.name | default "<unknown>") $resolved) -}}
  {{- end -}}
  {{- if eq $resolved "true" -}}true{{- end -}}
{{- else if ne $val nil -}}
  {{- fail (printf "spoaHub.plugins.%s.enabled must be a boolean or a template string resolving to true or false." (.name | default "<unknown>")) -}}
{{- end -}}
{{- end -}}

{{/*
Resolve a plugin timeout. Like `enabled`, `timeoutMs` may be a literal number
or a templated string in values.yaml. The authoritative operator-facing value
is always spoaHub.plugins.<name>.timeoutMs; feature settings do not provide
aliases with surprising precedence.
Args: dict "plugin" <plugin map> "root" $ "name" <plugin name>
*/}}
{{- define "haptic.spoaHub.pluginTimeoutMs" -}}
{{- $plugin := default dict .plugin -}}
{{- $raw := 500 -}}
{{- if hasKey $plugin "timeoutMs" -}}
  {{- $raw = $plugin.timeoutMs -}}
{{- end -}}
{{- $resolved := $raw -}}
{{- if eq (kindOf $raw) "string" -}}
  {{- $resolved = (trim (tpl $raw .root)) -}}
{{- end -}}
{{- if not (regexMatch "^[0-9]+$" (toString $resolved)) -}}
  {{- fail (printf "spoaHub.plugins.%s.timeoutMs must resolve to a positive integer milliseconds value." .name) -}}
{{- end -}}
{{- $ms := int $resolved -}}
{{- if le $ms 0 -}}
  {{- fail (printf "spoaHub.plugins.%s.timeoutMs must resolve to a positive integer milliseconds value." .name) -}}
{{- end -}}
{{- $ms -}}
{{- end }}

{{/*
Resolve and validate one integer from spoaHub.hub. Callers decide whether an
optional null field should be emitted before invoking this helper. Keeping the
validation here ensures bootstrap and controller-rendered runtime TOML cannot
silently diverge on zero/negative values.
Args: dict "root" $ "field" <name> "min" <integer>
*/}}
{{- define "haptic.spoaHub.hubInteger" -}}
{{- $raw := index .root.Values.spoaHub.hub .field -}}
{{- if not (regexMatch "^[0-9]+$" (toString $raw)) -}}
  {{- fail (printf "spoaHub.hub.%s must be an integer greater than or equal to %d." .field (int .min)) -}}
{{- end -}}
{{- $value := int $raw -}}
{{- if lt $value (int .min) -}}
  {{- fail (printf "spoaHub.hub.%s must be an integer greater than or equal to %d." .field (int .min)) -}}
{{- end -}}
{{- if and (hasKey . "max") (gt $value (int .max)) -}}
  {{- fail (printf "spoaHub.hub.%s must be an integer between %d and %d." .field (int .min) (int .max)) -}}
{{- end -}}
{{- $value -}}
{{- end }}

{{/*
Resolve a required integer value from one optional plugin field. Like enabled
and timeoutMs, admission fields may be literal numbers or templated strings.
Callers first check hasKey, then pass the allowed minimum.
Args: dict "plugin" <map> "root" $ "name" <name> "field" <field> "min" <n>
*/}}
{{- define "haptic.spoaHub.pluginInteger" -}}
{{- $raw := index .plugin .field -}}
{{- $resolved := $raw -}}
{{- if eq (kindOf $raw) "string" -}}
  {{- $resolved = (trim (tpl $raw .root)) -}}
{{- end -}}
{{- if not (regexMatch "^[0-9]+$" (toString $resolved)) -}}
  {{- fail (printf "spoaHub.plugins.%s.%s must resolve to an integer greater than or equal to %d." .name .field (int .min)) -}}
{{- end -}}
{{- $value := int $resolved -}}
{{- if lt $value (int .min) -}}
  {{- fail (printf "spoaHub.plugins.%s.%s must resolve to an integer greater than or equal to %d." .name .field (int .min)) -}}
{{- end -}}
{{- if and (hasKey . "max") (gt $value (int .max)) -}}
  {{- fail (printf "spoaHub.plugins.%s.%s must resolve to an integer between %d and %d." .name .field (int .min) (int .max)) -}}
{{- end -}}
{{- $value -}}
{{- end }}

{{/*
Validate a positive HAProxy duration. Bare integers retain HAProxy's native
millisecond interpretation; explicit us/ms/s/m/h/d suffixes are accepted.
Args: dict "value" <value> "field" <full values path>
*/}}
{{- define "haptic.spoaHub.duration" -}}
{{- $value := toString .value -}}
{{- if or (not (regexMatch "^[0-9]+(us|ms|s|m|h|d)?$" $value)) (le (int64 (regexFind "^[0-9]+" $value)) 0) -}}
  {{- fail (printf "%s must be a positive HAProxy duration using an optional us, ms, s, m, h, or d suffix." .field) -}}
{{- end -}}
{{- $value -}}
{{- end }}

{{/*
Validate the one process-wide HAProxy request buffer shared by every body
inspector and return its usable body capacity (sizeBytes - reservedBytes).
The returned capacity is also the chart-managed top-level Coraza body limit,
so HAProxy and the plugin cannot disagree about bytes that are available.
*/}}
{{- define "haptic.requestBodyInspection.capacity" -}}
{{- $configuredExtraContext := dig "templatingSettings" "extraContext" dict (.Values.controller.config | default dict) -}}
{{- $inspection := dig "requestBodyInspection" dict $configuredExtraContext -}}
{{- $buffer := dig "haproxyBuffer" dict $inspection -}}
{{- $sizeRaw := dig "sizeBytes" 16384 $buffer | toString -}}
{{- $reservedRaw := dig "reservedBytes" 8192 $buffer | toString -}}
{{- if not (regexMatch "^[0-9]+$" $sizeRaw) -}}{{- fail "controller.config.templatingSettings.extraContext.requestBodyInspection.haproxyBuffer.sizeBytes must be an integer between 16384 and 2097152 bytes." -}}{{- end -}}
{{- if not (regexMatch "^[0-9]+$" $reservedRaw) -}}{{- fail "controller.config.templatingSettings.extraContext.requestBodyInspection.haproxyBuffer.reservedBytes must be a positive integer smaller than sizeBytes." -}}{{- end -}}
{{- $size := int $sizeRaw -}}
{{- $reserved := int $reservedRaw -}}
{{- if or (lt $size 16384) (gt $size 2097152) -}}{{- fail "controller.config.templatingSettings.extraContext.requestBodyInspection.haproxyBuffer.sizeBytes must be between 16384 and 2097152 bytes." -}}{{- end -}}
{{- if or (le $reserved 0) (ge $reserved $size) -}}{{- fail "controller.config.templatingSettings.extraContext.requestBodyInspection.haproxyBuffer.reservedBytes must be positive and smaller than sizeBytes." -}}{{- end -}}
{{- sub $size $reserved -}}
{{- end }}

{{/*
Validate the complete chart-owned SPOA Hub value surface, including disabled
plugins. Disabled/staged configuration must not hide typos that only explode
on a later enable. This helper intentionally does not validate Kubernetes API
objects such as resources/securityContext; the apiserver owns those schemas.
*/}}
{{- define "haptic.spoaHub.validateValues" -}}
{{- $root := . -}}
{{- $spoa := $root.Values.spoaHub -}}
{{- if hasKey $spoa "monitoring" -}}
  {{- fail "spoaHub.monitoring moved to haproxy.monitoring: one PodMonitor (haproxy.monitoring.podMonitor) now scrapes every metrics endpoint on the HAProxy pod — HAProxy's exporter, vector's endpoints and, without vector, the hub's. Same podMonitor fields." -}}
{{- end -}}

{{- $hub := $spoa.hub | default dict -}}
{{- $_ := include "haptic.spoaHub.hubInteger" (dict "root" $root "field" "maxConnections" "min" 1) -}}
{{- $_ := include "haptic.spoaHub.hubInteger" (dict "root" $root "field" "blockingThreadKeepAliveSecs" "min" 1) -}}
{{- $_ := include "haptic.spoaHub.hubInteger" (dict "root" $root "field" "goGCPercent" "min" 1) -}}
{{- if ne $hub.workerThreads nil -}}{{- $_ := include "haptic.spoaHub.hubInteger" (dict "root" $root "field" "workerThreads" "min" 1) -}}{{- end -}}
{{- if ne $hub.maxBlockingThreads nil -}}{{- $_ := include "haptic.spoaHub.hubInteger" (dict "root" $root "field" "maxBlockingThreads" "min" 1) -}}{{- end -}}
{{- if ne $hub.reloadDrainTimeoutMs nil -}}{{- $_ := include "haptic.spoaHub.hubInteger" (dict "root" $root "field" "reloadDrainTimeoutMs" "min" 0) -}}{{- end -}}
{{- /* `auto` is the default sentinel: it resolves to a loopback bind when the
       vector sidecar fronts the metrics and a pod-routable one when it doesn't.
       Validate the RESOLVED value so an operator's explicit address still gets
       the same numeric-IP check. */ -}}
{{- $resolvedMetricsAddr := include "haptic.spoaHub.metricsAddrEffective" $root -}}
{{- if and (ne $resolvedMetricsAddr "") (not (regexMatch "^(([0-9]{1,3}\\.){3}[0-9]{1,3}|\\[[0-9A-Fa-f:]+\\]):[0-9]{1,5}$" $resolvedMetricsAddr)) -}}
  {{- fail "spoaHub.hub.metricsAddr must be empty, \"auto\", or a numeric IP address and port such as 127.0.0.1:9095 or [::1]:9095; hostnames are not supported by the hub." -}}
{{- end -}}
{{- if ne $resolvedMetricsAddr "" -}}
  {{- $metricsPort := int (regexFind "[0-9]+$" $resolvedMetricsAddr) -}}
  {{- if or (le $metricsPort 0) (gt $metricsPort 65535) -}}{{- fail "spoaHub.hub.metricsAddr port must be between 1 and 65535." -}}{{- end -}}
{{- end -}}

{{- $haproxy := $spoa.haproxy | default dict -}}
{{- if not (regexMatch "^/run/spoa/[A-Za-z0-9._-]+\\.sock$" $haproxy.socketPath) -}}
  {{- fail "spoaHub.haproxy.socketPath must name a .sock file directly under the chart's shared /run/spoa mount." -}}
{{- end -}}
{{- $_ := include "haptic.spoaHub.duration" (dict "value" $haproxy.timeoutHello "field" "spoaHub.haproxy.timeoutHello") -}}
{{- $_ := include "haptic.spoaHub.duration" (dict "value" $haproxy.timeoutIdle "field" "spoaHub.haproxy.timeoutIdle") -}}
{{- if ne $haproxy.timeoutProcessing nil -}}{{- $_ := include "haptic.spoaHub.duration" (dict "value" $haproxy.timeoutProcessing "field" "spoaHub.haproxy.timeoutProcessing") -}}{{- end -}}
{{- $_ := include "haptic.spoaHub.duration" (dict "value" $haproxy.poolPurgeDelay "field" "spoaHub.haproxy.poolPurgeDelay") -}}
{{- if not (regexMatch "^[0-9]+$" (toString $haproxy.timeoutProcessingMarginMs)) -}}{{- fail "spoaHub.haproxy.timeoutProcessingMarginMs must be an integer between 1 and 60000 milliseconds." -}}{{- end -}}
{{- $processingMargin := int $haproxy.timeoutProcessingMarginMs -}}
{{- if or (lt $processingMargin 1) (gt $processingMargin 60000) -}}{{- fail "spoaHub.haproxy.timeoutProcessingMarginMs must be between 1 and 60000 milliseconds." -}}{{- end -}}
{{- if not (regexMatch "^[0-9]+$" (toString $haproxy.poolMaxConn)) -}}{{- fail "spoaHub.haproxy.poolMaxConn must be a positive integer." -}}{{- end -}}
{{- $poolMaxConn := int $haproxy.poolMaxConn -}}
{{- if lt $poolMaxConn 1 -}}{{- fail "spoaHub.haproxy.poolMaxConn must be a positive integer." -}}{{- end -}}
{{- if gt $poolMaxConn (int $hub.maxConnections) -}}
  {{- fail "spoaHub.haproxy.poolMaxConn must not exceed spoaHub.hub.maxConnections; the hub cannot accept the advertised HAProxy pool capacity." -}}
{{- end -}}

{{- $plugins := $spoa.plugins | default dict -}}
{{- $normalizedNames := dict -}}
{{- $enabledNames := dict -}}
{{- $enabledConcurrency := 0 -}}
{{- range $name, $plugin := $plugins -}}
  {{- if not (regexMatch "^[a-z][a-z0-9-]*$" $name) -}}{{- fail (printf "spoaHub.plugins key %q must contain lowercase letters, digits, and hyphens and start with a letter." $name) -}}{{- end -}}
  {{- /* Migration guard, not a general name allowlist — a custom hub image may
         legitimately ship plugins this chart has never heard of. Without it a
         values block left over from before the otel plugin was removed renders
         happily and writes a plugin the bundled image no longer contains into
         config.toml, so the hub fails to load it at RUNTIME instead of the
         install failing with a reason. Drop this guard once upgrades from
         pre-removal versions are no longer a concern. */ -}}
  {{- if eq $name "otel" -}}
    {{- fail "spoaHub.plugins.otel was removed: the bundled hub image no longer ships the otel plugin, and a leftover block would be written into config.toml for a plugin that cannot be loaded. Remove the block. Distributed tracing no longer needs it — HAProxy mints and propagates W3C trace context itself and the Vector sidecar derives OTLP spans from the access log (controller.config.templatingSettings.extraContext.tracing)." -}}
  {{- end -}}
  {{- $normalizedName := regexReplaceAll "-" $name "_" -}}
  {{- if hasKey $normalizedNames $normalizedName -}}{{- fail (printf "spoaHub plugin names %q and %q both normalize to %q; choose distinct names." (index $normalizedNames $normalizedName) $name $normalizedName) -}}{{- end -}}
  {{- $_ := set $normalizedNames $normalizedName $name -}}
  {{- $enabled := include "haptic.spoaHub.pluginEnabled" (dict "plugin" $plugin "root" $root "name" $name) -}}
  {{- if $enabled -}}{{- $_ := set $enabledNames $normalizedName true -}}{{- end -}}
  {{- if not (hasKey $plugin "timeoutMs") -}}{{- fail (printf "spoaHub.plugins.%s.timeoutMs is required so every plugin has an explicit request-path deadline." $name) -}}{{- end -}}
  {{- $_ := include "haptic.spoaHub.pluginTimeoutMs" (dict "plugin" $plugin "root" $root "name" $name) -}}
  {{- if and $enabled (eq (len $plugin.messages) 0) -}}{{- fail (printf "spoaHub.plugins.%s.messages must contain at least one message while the plugin is enabled." $name) -}}{{- end -}}
  {{- $seenMessages := dict -}}
  {{- range $message := $plugin.messages -}}
    {{- if not (regexMatch "^[A-Za-z][A-Za-z0-9_-]*$" $message) -}}{{- fail (printf "spoaHub.plugins.%s message %q contains unsupported characters." $name $message) -}}{{- end -}}
    {{- if hasKey $seenMessages $message -}}{{- fail (printf "spoaHub.plugins.%s.messages contains duplicate %q." $name $message) -}}{{- end -}}
    {{- $_ := set $seenMessages $message true -}}
  {{- end -}}
  {{- $dependencies := $plugin.dependsOn | default list -}}
  {{- range $dependency := $dependencies -}}
    {{- if not (regexMatch "^[a-z][a-z0-9-]*$" $dependency) -}}{{- fail (printf "spoaHub.plugins.%s dependency %q is not a valid plugin name." $name $dependency) -}}{{- end -}}
  {{- end -}}
  {{- $maxConcurrency := 0 -}}
  {{- if hasKey $plugin "maxConcurrency" -}}
    {{- $maxConcurrency = int (include "haptic.spoaHub.pluginInteger" (dict "plugin" $plugin "root" $root "name" $name "field" "maxConcurrency" "min" 1)) -}}
    {{- if $enabled -}}{{- $enabledConcurrency = add $enabledConcurrency $maxConcurrency -}}{{- end -}}
  {{- end -}}
  {{- $maxQueue := 0 -}}
  {{- if hasKey $plugin "maxQueue" -}}{{- $maxQueue = int (include "haptic.spoaHub.pluginInteger" (dict "plugin" $plugin "root" $root "name" $name "field" "maxQueue" "min" 0)) -}}{{- end -}}
  {{- if hasKey $plugin "queueTimeoutMs" -}}
    {{- $_ := include "haptic.spoaHub.pluginInteger" (dict "plugin" $plugin "root" $root "name" $name "field" "queueTimeoutMs" "min" 1) -}}
    {{- if eq $maxQueue 0 -}}{{- fail (printf "spoaHub.plugins.%s.queueTimeoutMs has no effect when maxQueue is zero; remove it or configure a bounded queue." $name) -}}{{- end -}}
  {{- end -}}
  {{- if eq $name "mirror" -}}
    {{- if not (hasKey $plugin "targetTimeoutMs") -}}{{- fail "spoaHub.plugins.mirror.targetTimeoutMs is required." -}}{{- end -}}
    {{- if not (hasKey $plugin "targetRetries") -}}{{- fail "spoaHub.plugins.mirror.targetRetries is required." -}}{{- end -}}
    {{- $_ := include "haptic.spoaHub.pluginInteger" (dict "plugin" $plugin "root" $root "name" $name "field" "targetTimeoutMs" "min" 1 "max" 60000) -}}
    {{- $_ := include "haptic.spoaHub.pluginInteger" (dict "plugin" $plugin "root" $root "name" $name "field" "targetRetries" "min" 0 "max" 10) -}}
  {{- end -}}
  {{- if eq $name "rate-limit" -}}
    {{- if not (hasKey $plugin "storeOperationTimeoutMs") -}}{{- fail "spoaHub.plugins.rate-limit.storeOperationTimeoutMs is required." -}}{{- end -}}
    {{- $_ := include "haptic.spoaHub.pluginInteger" (dict "plugin" $plugin "root" $root "name" $name "field" "storeOperationTimeoutMs" "min" 1) -}}
  {{- end -}}
{{- end -}}
{{- range $name, $plugin := $plugins -}}
  {{- $enabled := include "haptic.spoaHub.pluginEnabled" (dict "plugin" $plugin "root" $root "name" $name) -}}
  {{- if $enabled -}}
    {{- range $dependency := ($plugin.dependsOn | default list) -}}
      {{- $normalizedDependency := regexReplaceAll "-" $dependency "_" -}}
      {{- if not (hasKey $enabledNames $normalizedDependency) -}}{{- fail (printf "spoaHub.plugins.%s.dependsOn references %q, which is not an enabled plugin." $name $dependency) -}}{{- end -}}
    {{- end -}}
  {{- end -}}
{{- end -}}
{{- if and (ne $hub.maxBlockingThreads nil) (gt $enabledConcurrency 0) (lt (int $hub.maxBlockingThreads) $enabledConcurrency) -}}
  {{- fail (printf "spoaHub.hub.maxBlockingThreads (%d) must be at least the sum of explicit maxConcurrency values for enabled plugins (%d); the hub performs the final check including CPU-derived plugin defaults." (int $hub.maxBlockingThreads) $enabledConcurrency) -}}
{{- end -}}
{{- end }}

{{/*
Whether the SPOA hub sidecar should be rendered.
True when:
  - spoaHub.enabled is explicitly true, OR
  - spoaHub.enabled is null/empty AND any plugin resolves to enabled.
False when spoaHub.enabled is explicitly false (operator override).
Returns "true" or "" (Helm-truthy convention).
*/}}
{{- define "haptic.spoaHub.enabled" -}}
{{- $root := . -}}
{{- $hub := $root.Values.spoaHub | default dict -}}
{{- if eq (kindOf $hub.enabled) "bool" -}}
  {{- if $hub.enabled -}}true{{- end -}}
{{- else -}}
  {{- /* Track "any plugin enabled" via a mutable wrapper so the answer is
         emitted at most once. Emitting "true" inside the range concatenates
         a separate "true" per enabled plugin (`"truetrue"`, …) which
         compares not-equal to the literal `"true"` that callers like
         libraries/spoa-hub/_index.yaml's `_helm_load.enable` predicate test
         against — silently dropping the spoa-hub library when ≥2 plugins
         are enabled. */}}
  {{- $any := dict "v" false -}}
  {{- range $name, $plugin := $hub.plugins -}}
    {{- if include "haptic.spoaHub.pluginEnabled" (dict "plugin" $plugin "root" $root "name" $name) -}}
      {{- $_ := set $any "v" true -}}
    {{- end -}}
  {{- end -}}
  {{- if $any.v -}}true{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Inverse of `haptic.spoaHub.enabled` — returns "true" when SPOA hub will NOT
be rendered, empty string otherwise. The library `_helm_skip_test`
predicates use this to skip validation tests that depend on snippets the
spoa-hub library contributes.
*/}}
{{- define "haptic.spoaHub.disabled" -}}
{{- if not (include "haptic.spoaHub.enabled" .) -}}true{{- end -}}
{{- end -}}

{{/*
"true" when the named plugin will not run: off itself, or the hub is off.
Args: dict "root" $ "name" <plugin name>
*/}}
{{- define "haptic.spoaHub.pluginUnavailable" -}}
{{- $plugin := index (.root.Values.spoaHub.plugins | default dict) .name -}}
{{- if or (include "haptic.spoaHub.disabled" .root) (not (include "haptic.spoaHub.pluginEnabled" (dict "plugin" $plugin "root" .root "name" .name))) -}}true{{- end -}}
{{- end -}}

{{/*
ConfigMap name for the SPOA hub's config.toml. Used by
templates/spoa-hub-configmap.yaml (metadata.name) and by
templates/haproxy-deployment.yaml (volumes.configMap.name).
*/}}
{{- define "haptic.spoaHub.configMapName" -}}
{{- printf "%s-spoa-hub" (include "haptic.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end -}}

{{/*
SPOA hub container image reference.
Uses spoaHub.image.tag if set, otherwise falls back to .Chart.AppVersion.
Example: ghcr.io/haproxy-haptic/haptic/spoa-hub:0.1.0
*/}}
{{- define "haptic.spoaHub.image" -}}
{{- printf "%s:%s" .Values.spoaHub.image.repository (.Values.spoaHub.image.tag | default .Chart.AppVersion) -}}
{{- end -}}

{{/*
Whether the validator sidecar should be rendered.
True when:
  - controller.validators.enabled is explicitly true, OR
  - controller.validators.enabled is null AND `haptic.spoaHub.enabled`
    is truthy (i.e. at least one plugin is on).
False when controller.validators.enabled is explicitly false.
Returns "true" or "" (Helm-truthy convention).
*/}}
{{- define "haptic.validators.enabled" -}}
{{- $val := (default dict (default dict .Values.controller).validators).enabled -}}
{{- if eq (kindOf $val) "bool" -}}
  {{- if $val -}}true{{- end -}}
{{- else -}}
  {{- include "haptic.spoaHub.enabled" . -}}
{{- end -}}
{{- end -}}

{{/*
Validator sidecar Unix-socket path. Concat of
controller.validators.socketDir and controller.validators.socketName.
Used as the shared mountpoint between controller and validator sidecar
and as the dial address the controller writes into spec.validators[].
*/}}
{{- define "haptic.validators.socketPath" -}}
{{- $v := .Values.controller.validators -}}
{{- printf "%s/%s" (trimSuffix "/" $v.socketDir) $v.socketName -}}
{{- end -}}

{{/*
Bootstrap TOML for the SPOA hub sidecar. The sidecar starts with this content
from a ConfigMap, then reloads the controller-rendered runtime TOML from
general storage after the first successful reconciliation.
*/}}
{{- define "haptic.spoaHub.bootstrapConfigContent" -}}
{{- $_ := include "haptic.spoaHub.validateValues" . -}}
{{- $_ := include "haptic.rateLimit.validateValues" . -}}
{{- $spoaHub := .Values.spoaHub -}}
{{- $hub := $spoaHub.hub -}}
plugin_dir = "/etc/haproxy-spoa-hub/plugins"
default_timeout_ms = 500
log_level = {{ $hub.logLevel | quote }}
max_connections = {{ include "haptic.spoaHub.hubInteger" (dict "root" . "field" "maxConnections" "min" 1) }}
blocking_thread_keep_alive_secs = {{ include "haptic.spoaHub.hubInteger" (dict "root" . "field" "blockingThreadKeepAliveSecs" "min" 1) }}
{{- if ne $hub.maxBlockingThreads nil }}
max_blocking_threads = {{ include "haptic.spoaHub.hubInteger" (dict "root" . "field" "maxBlockingThreads" "min" 1) }}
{{- end }}
{{- if ne $hub.reloadDrainTimeoutMs nil }}
reload_drain_timeout_ms = {{ include "haptic.spoaHub.hubInteger" (dict "root" . "field" "reloadDrainTimeoutMs" "min" 0) }}
{{- end }}
{{- with include "haptic.spoaHub.metricsAddrEffective" . }}
{{- /* Bootstrap-side metrics_addr — mirrors the runtime-rendered
       libraries/spoa-hub/ `metrics_addr` line so the /metrics
       endpoint is bound from process start, not just after the
       controller pushes the first runtime config. The two have to
       stay in sync: the bootstrap is what the sidecar loads on pod
       startup (read-only, mounted from this ConfigMap), and the
       hub's file-watch swaps in the runtime config later. Without
       this, scraping /metrics during early-cluster boot returns
       empty — what bit issue #45's first artifact run. */}}
metrics_addr = {{ . | quote }}
{{- end }}
{{- if ne $hub.workerThreads nil }}
worker_threads = {{ include "haptic.spoaHub.hubInteger" (dict "root" . "field" "workerThreads" "min" 1) }}
{{- end }}

[[listeners]]
type = "unix"
address = {{ $spoaHub.haproxy.socketPath | quote }}

{{- range $name, $plugin := $spoaHub.plugins }}
{{- if include "haptic.spoaHub.pluginEnabled" (dict "plugin" $plugin "root" $ "name" $name) }}

{{- /* The hub builds SPOP response variable names as `<plugin>.<var>`,
       so the plugin name shows up directly in HAProxy's variable namespace
       (e.g. `txn.<var-prefix>.<plugin>.allowed`). HAProxy variable names
       are restricted identifiers — dashes aren't allowed mid-identifier
       the same way they are in YAML keys, so a TOML name of `external-auth`
       produces a var like `external-auth.allowed` which the deny rule
       can't match. The upstream plugin's example hub.toml uses the
       snake_case form for this reason. Convert dashes to underscores
       here to keep the YAML key kebab-case (idiomatic) while emitting
       a HAProxy-friendly identifier. */}}
[[plugins]]
name = {{ regexReplaceAll "-" $name "_" | quote }}
library = {{ include "haptic.spoaHub.libName" (dict "name" $name "plugin" $plugin) | quote }}
messages = {{ $plugin.messages | toJson }}
timeout_ms = {{ include "haptic.spoaHub.pluginTimeoutMs" (dict "plugin" $plugin "root" $ "name" $name) }}
{{- if hasKey $plugin "maxConcurrency" }}
max_concurrency = {{ include "haptic.spoaHub.pluginInteger" (dict "plugin" $plugin "root" $ "name" $name "field" "maxConcurrency" "min" 1) }}
{{- end }}
{{- if hasKey $plugin "maxQueue" }}
max_queue = {{ include "haptic.spoaHub.pluginInteger" (dict "plugin" $plugin "root" $ "name" $name "field" "maxQueue" "min" 0) }}
{{- end }}
{{- if hasKey $plugin "queueTimeoutMs" }}
queue_timeout_ms = {{ include "haptic.spoaHub.pluginInteger" (dict "plugin" $plugin "root" $ "name" $name "field" "queueTimeoutMs" "min" 1) }}
{{- end }}
{{- if hasKey $plugin "adaptiveConcurrency" }}
{{- if $plugin.adaptiveConcurrency }}
adaptive_concurrency = true
{{- end }}
{{- end }}
{{- $normalizedDependencies := list }}
{{- range ($plugin.dependsOn | default list) }}
  {{- $normalizedDependencies = append $normalizedDependencies (regexReplaceAll "-" . "_") }}
{{- end }}
{{- with $normalizedDependencies }}
depends_on = {{ . | toJson }}
{{- end }}

[plugins.params]
{{- /* Coraza's directives lives in its own values field so the
       controller-rendered TOML (rendered by libraries/spoa-hub/)
       can append per-Ingress modsecurity-snippet values into it.
       The bootstrap placeholder rendered here is just enough to let
       the hub start; the controller pushes the real config through
       the agent and the hub reloads on file-watch. */}}
{{- if and (eq $name "coraza") $plugin.directives }}
directives = """
{{- $plugin.directives | nindent 0 }}
"""
{{- end }}
{{- with (include "haptic.spoaHub.effectivePluginParams" (dict "root" $ "name" $name "plugin" $plugin)) }}
{{ . | trim }}
{{- end }}
{{- end }}
{{- end }}
{{- end -}}

{{/*
Names and URLs for the chart-managed Valkey store used by the shared
rate-limit plugin. The workload itself is emitted by the haptic-annotations
library through k8sResources so it is owned by the HAProxyTemplateConfig CR.
*/}}
{{- define "haptic.rateLimit.storeServiceName" -}}
{{- printf "%s-rl-store" (include "haptic.fullname" .) | trunc 43 | trimSuffix "-" -}}
{{- end -}}

{{/*
Validate the chart-managed shared rate-limit configuration even while the
feature is disabled. These values are later interpolated into shell scripts and
Valkey/Sentinel configuration, so silently coercing zero values or accepting
unknown keys is unsafe and especially surprising when a staged feature is
enabled months later.
*/}}
{{- define "haptic.rateLimit.validateValues" -}}
{{- $rateLimit := .Values.rateLimit -}}
{{- $shared := $rateLimit.shared | default dict -}}

{{- $external := dict -}}
{{- if hasKey $shared "externalStore" -}}{{- $external = $shared.externalStore -}}{{- end -}}
{{- $externalURLs := list -}}
{{- if hasKey $external "urls" -}}{{- $externalURLs = $external.urls -}}{{- end -}}
{{- if gt (len $externalURLs) 1 -}}
  {{- fail "rateLimit.shared.externalStore.urls accepts one endpoint. Multiple URLs share one plugin circuit breaker, so one failed shard disables all shards. Use one HA Redis/Valkey/Sentinel/Cluster endpoint." -}}
{{- end -}}
{{- range $url := $externalURLs -}}
  {{- if or (not (kindIs "string" $url)) (eq (trim $url) "") -}}{{- fail "rateLimit.shared.externalStore.urls entries must be non-empty URL strings." -}}{{- end -}}
{{- end -}}

{{- $store := $shared.managedStore | default dict -}}
{{- if or (hasKey $store "maxmemory") (hasKey $store "maxmemoryPolicy") -}}{{- fail "rateLimit.shared.managedStore.maxmemory and maxmemoryPolicy were renamed to maxMemory and maxMemoryPolicy to follow the chart's camelCase value convention." -}}{{- end -}}
{{- if and $store.enabled (gt (len $externalURLs) 0) -}}
  {{- fail "rateLimit.shared.managedStore.enabled=true and rateLimit.shared.externalStore.urls are mutually exclusive; disable the managed store to bring your own Redis/Valkey." -}}
{{- end -}}
{{- if or (not (kindIs "string" $store.image)) (eq (trim $store.image) "") -}}{{- fail "rateLimit.shared.managedStore.image must be a non-empty image reference string." -}}{{- end -}}
{{- if not (regexMatch "^[0-9]+$" (toString $store.port)) -}}{{- fail "rateLimit.shared.managedStore.port must be an integer between 1 and 65535." -}}{{- end -}}
{{- $port := int $store.port -}}
{{- if or (lt $port 1) (gt $port 65535) -}}{{- fail "rateLimit.shared.managedStore.port must be between 1 and 65535." -}}{{- end -}}
{{- if not (regexMatch "^[0-9]+$" (toString $store.replicas)) -}}{{- fail "rateLimit.shared.managedStore.replicas must be an integer greater than or equal to 3 for chart-managed Sentinel HA." -}}{{- end -}}
{{- $replicas := int $store.replicas -}}
{{- if lt $replicas 3 -}}{{- fail "rateLimit.shared.managedStore.replicas must be at least 3 for chart-managed Sentinel HA." -}}{{- end -}}
{{- if or (not (kindIs "string" $store.maxMemory)) (not (regexMatch "(?i)^[1-9][0-9]*(b|k|kb|m|mb|g|gb|t|tb)$" $store.maxMemory)) -}}
  {{- fail "rateLimit.shared.managedStore.maxMemory must be a positive Valkey byte size such as 96mb or 1gb." -}}
{{- end -}}
{{- if or (not (kindIs "string" $store.maxMemoryPolicy)) (ne $store.maxMemoryPolicy "noeviction") -}}
  {{- fail "rateLimit.shared.managedStore.maxMemoryPolicy must be noeviction. Evicting a limiter key resets its budget; set maxMemoryPolicy: noeviction." -}}
{{- end -}}

{{- $sentinel := $store.sentinel | default dict -}}
{{- range $field := list "port" "quorum" "downAfterMilliseconds" "failoverTimeoutMilliseconds" "parallelSyncs" -}}
  {{- if not (regexMatch "^[0-9]+$" (toString (index $sentinel $field))) -}}{{- fail (printf "rateLimit.shared.managedStore.sentinel.%s must be a positive integer." $field) -}}{{- end -}}
{{- end -}}
{{- $sentinelPort := int $sentinel.port -}}
{{- if or (lt $sentinelPort 1) (gt $sentinelPort 65535) -}}{{- fail "rateLimit.shared.managedStore.sentinel.port must be between 1 and 65535." -}}{{- end -}}
{{- if eq $sentinelPort $port -}}{{- fail "rateLimit.shared.managedStore.sentinel.port must differ from rateLimit.shared.managedStore.port." -}}{{- end -}}
{{- $quorum := int $sentinel.quorum -}}
{{- if or (lt $quorum 1) (gt $quorum $replicas) -}}{{- fail "rateLimit.shared.managedStore.sentinel.quorum must be between 1 and rateLimit.shared.managedStore.replicas." -}}{{- end -}}
{{- $downAfter := int $sentinel.downAfterMilliseconds -}}
{{- if lt $downAfter 1 -}}{{- fail "rateLimit.shared.managedStore.sentinel.downAfterMilliseconds must be positive." -}}{{- end -}}
{{- $failoverTimeout := int $sentinel.failoverTimeoutMilliseconds -}}
{{- if lt $failoverTimeout $downAfter -}}{{- fail "rateLimit.shared.managedStore.sentinel.failoverTimeoutMilliseconds must be greater than or equal to downAfterMilliseconds." -}}{{- end -}}
{{- $parallelSyncs := int $sentinel.parallelSyncs -}}
{{- if or (lt $parallelSyncs 1) (ge $parallelSyncs $replicas) -}}{{- fail "rateLimit.shared.managedStore.sentinel.parallelSyncs must be between 1 and replicas minus 1." -}}{{- end -}}

{{- $pdb := $store.podDisruptionBudget | default dict -}}
{{- if not (regexMatch "^[0-9]+$" (toString $pdb.maxUnavailable)) -}}{{- fail "rateLimit.shared.managedStore.podDisruptionBudget.maxUnavailable must be a non-negative integer." -}}{{- end -}}
{{- $maxUnavailable := int $pdb.maxUnavailable -}}
{{- if gt $maxUnavailable (sub $replicas $quorum) -}}
  {{- fail "rateLimit.shared.managedStore.podDisruptionBudget.maxUnavailable must not exceed replicas minus Sentinel quorum, so voluntary disruptions preserve failover quorum." -}}
{{- end -}}
{{- $networkPolicy := $store.networkPolicy | default dict -}}
{{- end }}

{{- define "haptic.rateLimit.storeSentinelServiceName" -}}
{{- printf "%s-sentinel" (include "haptic.rateLimit.storeServiceName" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "haptic.rateLimit.storeSentinelMasterName" -}}
{{- "haptic-rate-limit" -}}
{{- end -}}

{{- define "haptic.rateLimit.storeSentinelURL" -}}
{{- $root := . -}}
{{- $store := $root.Values.rateLimit.shared.managedStore -}}
{{- $sentinel := $store.sentinel | default dict -}}
{{- $sentinelPort := int $sentinel.port -}}
{{- $svc := include "haptic.rateLimit.storeSentinelServiceName" $root -}}
{{- $storeSvc := include "haptic.rateLimit.storeServiceName" $root -}}
{{- $master := include "haptic.rateLimit.storeSentinelMasterName" $root -}}
{{- $url := printf "redis-sentinel://%s.%s.svc:%d/0?sentinelServiceName=%s" $svc $root.Release.Namespace $sentinelPort $master -}}
{{- range $i := until (int $store.replicas) -}}
  {{- $node := printf "%s-%d.%s.%s.svc.cluster.local:%d" $storeSvc $i $storeSvc $root.Release.Namespace $sentinelPort -}}
  {{- $url = printf "%s&node=%s" $url $node -}}
{{- end -}}
{{- $url -}}
{{- end -}}

{{- define "haptic.rateLimit.storeURLTOML" -}}
store_url = {{ include "haptic.rateLimit.storeSentinelURL" . | quote }}
{{- end -}}

{{/*
Return the effective params TOML for one spoaHub plugin. Most plugins pass
their values.yaml params through unchanged. rate-limit gets chart-managed
store_url appended when rateLimit.shared.enabled=true and
rateLimit.shared.managedStore.enabled=true, so the bootstrap ConfigMap and the
runtime-rendered hub config stay byte-for-byte consistent about the store
endpoints.
*/}}
{{/*
Adaptive-concurrency ceiling for a spoa-hub plugin, derived from the sidecar's
MEMORY rather than CPU or a hand-set number — so it self-scales across small and
large deployments with no manual tuning. Concurrency in the hub is realised as
blocking OS threads (one per in-flight plugin call), so the binding resource is
memory, not cores; deriving from cores would be both the wrong dimension and
unreliable in a container (see haproxy-spoa-hub ADR-0002). The controller then
finds the live operating point under this ceiling from request latency, so the
ceiling is a backstop, not the operating point.

Formula: reserve ~128MB for the coraza ruleset + Go runtime, budget ~8MB per
concurrent call (thread stack + per-transaction buffers), clamp to [8, 256].
Reads the sidecar memory limit (the hard OOM boundary), falling back to the
request. Argument: the root context (`.`).
*/}}
{{- define "haptic.spoaHub.adaptiveCeiling" -}}
{{- $res := .Values.spoaHub.resources | default dict -}}
{{- $mem := dig "limits" "memory" (dig "requests" "memory" "" $res) $res -}}
{{- $memMB := include "haptic.memoryToMB" $mem | int -}}
{{- $ceiling := div (sub $memMB 128) 8 -}}
{{- max 8 (min 256 $ceiling) -}}
{{- end -}}

{{/*
Go-runtime environment for a hub sidecar. The coraza plugin embeds a Go runtime,
and under sustained request load Go's default GOGC=100 triggers GC often; each
cycle steals CPU from the request path (GC assist) and drains the plugin's
transaction sync.Pool, so raising GOGC trims the p99 tail (measured ~15-20% at
moderate load on a memory-headroom-rich sidecar). GOMEMLIMIT is a soft cap at
90% of the container memory limit so the higher GOGC can never breach it. Set
spoaHub.hub.goGCPercent to tune (100 restores Go's default); GOMEMLIMIT is
derived automatically and omitted when no memory limit is set.
Args: dict "root" $ "resources" <container resources map>
*/}}
{{- define "haptic.spoaHub.goRuntimeEnv" -}}
- name: GOGC
  value: {{ .root.Values.spoaHub.hub.goGCPercent | toString | quote }}
{{- $memLimit := dig "limits" "memory" "" (.resources | default dict) -}}
{{- if $memLimit -}}
{{- $memMB := int (include "haptic.memoryToMB" $memLimit) -}}
{{- if gt $memMB 0 }}
- name: GOMEMLIMIT
  value: {{ printf "%dMiB" (div (mul $memMB 90) 100) | quote }}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Numeric port the hub binds its Prometheus /metrics endpoint on, parsed from
spoaHub.hub.metricsAddr. Empty when metricsAddr is empty (metrics disabled).
Used to declare the sidecar's named `metrics` containerPort so a PodMonitor can
reference it by name.
*/}}
{{- define "haptic.spoaHub.metricsPort" -}}
{{- $addr := include "haptic.spoaHub.metricsAddrEffective" . -}}
{{- if ne (trim $addr) "" -}}
{{- regexFind "[0-9]+$" $addr -}}
{{- end -}}
{{- end -}}

{{/*
Resolve spoaHub.hub.metricsAddr, expanding the `auto` default.

`auto` binds the hub's /metrics where the thing that scrapes it can reach it:

  vector enabled  -> 127.0.0.1:9095   vector scrapes over loopback from inside
                                      the pod and re-exports on its own port, so
                                      there is no reason to answer on the pod IP
  vector disabled -> 0.0.0.0:9095     Prometheus scrapes the pod IP directly, and
                                      a loopback bind would be a dead target

An explicit operator value always wins, including `""` (metrics disabled). This
is a sentinel rather than a plain default because `""` is already taken to mean
"off", so it cannot double as "derive it for me".
*/}}
{{- define "haptic.spoaHub.metricsAddrEffective" -}}
{{- $addr := .Values.spoaHub.hub.metricsAddr | default "" -}}
{{- if eq (trim $addr) "auto" -}}
{{- if .Values.vector.enabled -}}
127.0.0.1:9095
{{- else -}}
0.0.0.0:9095
{{- end -}}
{{- else -}}
{{- $addr -}}
{{- end -}}
{{- end -}}

{{- define "haptic.spoaHub.effectivePluginParams" -}}
{{- $root := .root -}}
{{- $name := .name -}}
{{- $plugin := default dict .plugin -}}
{{- $rawParams := "" -}}
{{- if hasKey $plugin "params" -}}
  {{- $rawParams = $plugin.params -}}
{{- end -}}
{{- $params := $rawParams -}}
{{- if eq (kindOf $rawParams) "string" -}}
  {{- $params = tpl $rawParams $root -}}
{{- end -}}
{{- $managedLines := list -}}
{{- if eq $name "api-gateway" -}}
  {{- if regexMatch "(?m)^\\s*(default_fail_open|max_body_bytes)\\s*=" $params -}}
    {{- fail "spoaHub.plugins.api-gateway.params must not define default_fail_open or max_body_bytes; controller.config.templatingSettings.extraContext.apiGateway.requestSchemaValidation is their single owner." -}}
  {{- end -}}
  {{- $validation := $root.Values.controller.config.templatingSettings.extraContext.apiGateway.requestSchemaValidation -}}
  {{- $managedLines = append $managedLines (printf "default_fail_open = %s" ($validation.defaultFailOpen | toString)) -}}
  {{- $managedLines = append $managedLines (printf "max_body_bytes = %d" (int $validation.requestBody.defaultMaxBytes)) -}}
{{- end -}}
{{- if eq $name "coraza" -}}
  {{- if regexMatch "(?m)^\\s*directives\\s*=" $params -}}
    {{- fail "spoaHub.plugins.coraza.params must not define directives; spoaHub.plugins.coraza.directives is its single owner." -}}
  {{- end -}}
  {{- if regexMatch "(?m)^\\s*(request_body_limit|request_body_in_memory_limit|response_body_limit|response_check)\\s*=" $params -}}
    {{- fail "spoaHub.plugins.coraza.params must not define request_body_limit, request_body_in_memory_limit, response_body_limit, or response_check. The chart derives request limits from controller.config.templatingSettings.extraContext.requestBodyInspection.haproxyBuffer and disables unsupported response-body inspection." -}}
  {{- end -}}
  {{- $bodyCapacity := int (include "haptic.requestBodyInspection.capacity" $root) -}}
  {{- $managedLines = append $managedLines (printf "request_body_limit = %d" $bodyCapacity) -}}
  {{- $managedLines = append $managedLines (printf "request_body_in_memory_limit = %d" $bodyCapacity) -}}
  {{- $managedLines = append $managedLines "response_check = false" -}}
{{- end -}}
{{- if eq $name "rate-limit" -}}
  {{- if regexMatch "(?m)^\\s*(fail_open|store_failure_mode|invalid_rule_fail_open)\\s*=" $params -}}
    {{- fail "spoaHub.plugins.rate-limit.params must not define fail_open, store_failure_mode, or invalid_rule_fail_open. rateLimit.shared.failClosed owns store failures; malformed rules always deny." -}}
  {{- end -}}
  {{- $managedLines = append $managedLines (printf "fail_open = %t" (not $root.Values.rateLimit.shared.failClosed)) -}}
  {{- $managedLines = append $managedLines "invalid_rule_fail_open = false" -}}
  {{- $managedLines = append $managedLines (printf "store_failure_mode = %q" (ternary "closed" "local" $root.Values.rateLimit.shared.failClosed)) -}}
  {{- if regexMatch "(?m)^\\s*store_timeout_ms\\s*=" $params -}}
    {{- fail "spoaHub.plugins.rate-limit.params must not define store_timeout_ms; spoaHub.plugins.rate-limit.storeOperationTimeoutMs is its single owner." -}}
  {{- end -}}
  {{- if not (regexMatch "^[0-9]+$" (toString $plugin.storeOperationTimeoutMs)) -}}
    {{- fail "spoaHub.plugins.rate-limit.storeOperationTimeoutMs must be a positive integer milliseconds value." -}}
  {{- end -}}
  {{- $storeOperationTimeoutMs := int $plugin.storeOperationTimeoutMs -}}
  {{- if le $storeOperationTimeoutMs 0 -}}
    {{- fail "spoaHub.plugins.rate-limit.storeOperationTimeoutMs must be a positive integer milliseconds value." -}}
  {{- end -}}
  {{- $managedLines = append $managedLines (printf "store_timeout_ms = %d" $storeOperationTimeoutMs) -}}
  {{- if regexMatch "(?m)^\\s*ttl_ms\\s*=" $params -}}
    {{- fail "spoaHub.plugins.rate-limit.params must not define ttl_ms; the chart fixes it at the plugin's 3600000 ms maximum and the native annotation library validates its rules against that horizon." -}}
  {{- end -}}
  {{- $managedLines = append $managedLines "ttl_ms = 3600000" -}}
  {{- if regexMatch "(?m)^\\s*store_urls?\\s*=" $params -}}
    {{- fail "spoaHub.plugins.rate-limit.params must not define store_url or store_urls; rateLimit.shared.managedStore (chart-managed HA Valkey) or rateLimit.shared.externalStore.urls (bring your own store) is their single owner." -}}
  {{- end -}}
  {{- $externalURLs := $root.Values.rateLimit.shared.externalStore.urls | default list -}}
  {{- if and $root.Values.rateLimit.shared.enabled $root.Values.rateLimit.shared.managedStore.enabled -}}
    {{- $managedLines = append $managedLines (include "haptic.rateLimit.storeURLTOML" $root | trim) -}}
  {{- else if gt (len $externalURLs) 0 -}}
    {{- $managedLines = append $managedLines (printf "store_url = %s" (first $externalURLs | quote)) -}}
  {{- else if $root.Values.rateLimit.shared.enabled -}}
    {{- fail "rateLimit.shared.enabled=true requires a shared store. Leave rateLimit.shared.managedStore.enabled=true for chart-managed HA Valkey, or set rateLimit.shared.externalStore.urls to bring your own Redis/Valkey." -}}
  {{- end -}}
{{- end -}}
{{- $sections := list -}}
{{- if gt (len $managedLines) 0 -}}{{- $sections = append $sections (join "\n" $managedLines) -}}{{- end -}}
{{- if ne (trim $params) "" -}}{{- $sections = append $sections (trimSuffix "\n" $params) -}}{{- end -}}
{{- join "\n" $sections -}}
{{- end -}}

{{/*
SPOA hub plugin shared-library filename.
Maps the plugin shortname (as it appears under spoaHub.plugins.<X>) to the
.so filename produced by the upstream build. Most plugins use
`lib<name>_plugin.so` (with dashes mapped to underscores), except sso-auth
whose Cargo crate name produces `libhaproxy_spoa_hub_plugin_sso_auth.so`.
Coraza ships both a Go and a Rust library under one plugin name.
Argument: dict with `name` and `plugin` keys.
*/}}
{{- define "haptic.spoaHub.libName" -}}
{{- if and (eq .name "coraza") (eq .plugin.engine "rust") -}}
libcoraza_rust_plugin.so
{{- else if eq .name "sso-auth" -}}
libhaproxy_spoa_hub_plugin_sso_auth.so
{{- else -}}
lib{{ regexReplaceAll "-" .name "_" }}_plugin.so
{{- end -}}
{{- end -}}

{{/*
Refuses extraContext.maxmind settings the plugin would only reject on the
HAProxy pods: the hub validator checks the params' shape, not that a lookup's
database exists, and a refused plugin leaves every pod without GeoIP data.
*/}}
{{- define "haptic.maxmind.validate" -}}
{{- $root := .root -}}
{{- $prefix := "controller.config.templatingSettings.extraContext.maxmind" -}}
{{- $databases := dig "databases" dict .maxmind | default dict -}}
{{- $lookups := dig "lookups" list .maxmind | default list -}}
{{- if and (gt (len $lookups) 0) (eq (len $databases) 0) -}}
  {{- fail (printf "%s.lookups is set without databases, so no lookup would run. Declare the databases under %s.databases." $prefix $prefix) -}}
{{- end -}}
{{- if gt (len $databases) 0 -}}
  {{- $plugin := (index ($root.Values.spoaHub.plugins | default dict) "maxmind") | default dict -}}
  {{- if or (eq (toString $root.Values.spoaHub.enabled) "false") (not (include "haptic.spoaHub.pluginEnabled" (dict "plugin" $plugin "root" $root "name" "maxmind"))) -}}
    {{- fail (printf "%s.databases is set, but spoaHub.plugins.maxmind is disabled, so the databases would be downloaded and never used. Remove spoaHub.plugins.maxmind.enabled=false and spoaHub.enabled=false, or remove the databases." $prefix) -}}
  {{- end -}}
{{- end -}}
{{- range $name, $database := $databases -}}
  {{- if not (regexMatch "^[A-Za-z0-9_-]+$" $name) -}}
    {{- fail (printf "%s.databases.%s: a database name may contain only letters, digits, '_' and '-'." $prefix $name) -}}
  {{- end -}}
  {{- $url := dig "url" "" ($database | default dict) | toString -}}
  {{- /* A database decides lookups such as a client's country. Over plaintext
         anyone on the path could replace it, and the result would still load. */ -}}
  {{- if not (hasPrefix "https://" $url) -}}
    {{- fail (printf "%s.databases.%s.url must be an https:// URL, so the database cannot be replaced in transit; got %q." $prefix $name $url) -}}
  {{- end -}}
{{- end -}}
{{- $names := dict -}}
{{- $outputs := dict -}}
{{- range $index, $lookup := $lookups -}}
  {{- $name := dig "name" "" $lookup | toString -}}
  {{- $output := dig "outputVar" "" $lookup | toString -}}
  {{- $database := dig "database" "" $lookup | toString -}}
  {{- if not (hasKey $databases $database) -}}
    {{- fail (printf "%s.lookups[%d] (%s) names database %q, which %s.databases does not declare." $prefix $index $name $database $prefix) -}}
  {{- end -}}
  {{- if or (eq $name "") (hasKey $names $name) -}}
    {{- fail (printf "%s.lookups[%d] needs a name no other lookup has; got %q." $prefix $index $name) -}}
  {{- end -}}
  {{- if or (not (regexMatch "^[A-Za-z_][A-Za-z0-9_]*$" $output)) (hasKey $outputs $output) -}}
    {{- fail (printf "%s.lookups[%d] (%s) needs an outputVar of letters, digits and '_' that no other lookup uses, because it becomes the HAProxy variable txn.hub.maxmind.<outputVar>; got %q." $prefix $index $name $output) -}}
  {{- end -}}
  {{- if eq (len (dig "path" list $lookup | default list)) 0 -}}
    {{- fail (printf "%s.lookups[%d] (%s) needs a path, such as [country, iso_code]." $prefix $index $name) -}}
  {{- end -}}
  {{- $_ := set $names $name true -}}
  {{- $_ := set $outputs $output true -}}
{{- end -}}
{{- end -}}
