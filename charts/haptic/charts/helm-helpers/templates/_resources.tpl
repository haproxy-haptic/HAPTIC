{{/*
Resource math: CPU/memory quantity parsing, HAProxy nbthread, agent
GOMAXPROCS, HAProxy shm-stats sizing, and the
bootstrap-config checksum (which depends on several of these values, so
it lives next to them).
*/}}

{{/*
Convert a Kubernetes CPU quantity to whole cores using ceiling arithmetic.
Supports millicores (e.g. "250m" → 1, "2000m" → 2) and whole cores
(e.g. "2" → 2). Empty / zero inputs render as 0; callers that want a
floor of 1 should clamp with `max 1` themselves.
Input: CPU quantity string.
*/}}
{{- define "haptic.cpuToCores" -}}
{{- $cpu := . | toString -}}
{{- if hasSuffix "m" $cpu -}}
  {{- /* ceil(millis/1000): add 999 then divide */ -}}
  {{- div (add (trimSuffix "m" $cpu | int) 999) 1000 -}}
{{- else -}}
  {{- $cpu | int -}}
{{- end -}}
{{- end -}}

{{/*
Calculate nbthread for HAProxy global section.
If haproxy.nbthread is explicitly set, use that value (rendered via tpl for templatability).
Otherwise, derive from the CPU *limit* (the CPU the pod may actually use),
using ceiling arithmetic: "2500m" -> 3, "2" -> 2. When no CPU limit is set,
return empty so the caller omits the directive entirely and HAProxy
auto-detects usable cores from its CPU affinity mask (= all node cores for a
burstable pod). This works in both worlds without deriving from requests:
  - Cloud: a CPU limit caps threads to the quota (no CFS throttling); an
    autoscaler-sized burstable pod auto-detects its request-sized node.
  - Static on-prem: no limit -> HAProxy uses every node core, without
    inflating CPU requests (which would just fence off CPU from other pods).
Deriving from the limit (never the request) matches Istio, Go 1.25's
container-aware GOMAXPROCS, and uber-go/automaxprocs.
Returns empty string if no CPU limit is configured and no override, or if
override is 0.
*/}}
{{- define "haptic.haproxy.nbthread" -}}
{{- if and .Values.haproxy (hasKey .Values.haproxy "nbthread") -}}
  {{- $override := tpl (toString .Values.haproxy.nbthread) . | int -}}
  {{- if gt $override 0 -}}
    {{- $override -}}
  {{- end -}}
{{- else -}}
  {{- with dig "limits" "cpu" "" .Values.haproxy.resources -}}
    {{- max 1 (include "haptic.cpuToCores" . | int) -}}
  {{- end -}}
{{- end -}}
{{- end -}}

{{/*
Checksum of HAProxy bootstrap ConfigMap inputs.
Changes when any value feeding into the bootstrap config changes,
triggering a rolling update of HAProxy pods.
*/}}
{{- define "haptic.haproxy.bootstrapConfigChecksum" -}}
{{- printf "%v-%v-%v" (tpl (.Values.haproxy.initialConfig | default "") .) (.Values.haproxy.shmStats | toJson) (include "haptic.spoaHub.bootstrapConfigContent" .) | sha256sum -}}
{{- end -}}

{{/*
Calculate /dev/shm emptyDir sizeLimit for HAProxy shm-stats-file.
Auto-calculates from maxObjects if shmSizeLimit is not explicitly set.
Formula: ceil(maxObjects * 4096 * 1.1 / 1048576) MiB
  - 4096 bytes per object (empirically ~3.2KB, 4KB provides safety margin)
  - 1.1 multiplier for filesystem overhead (10%)
  - Converted to MiB, rounded up
*/}}
{{- define "haptic.haproxy.shmSizeLimit" -}}
{{- $shmStats := .Values.haproxy.shmStats -}}
{{- with $shmStats.shmSizeLimit -}}
  {{- . -}}
{{- else -}}
  {{- $bytesNeeded := mul ($shmStats.maxObjects | int) 4096 -}}
  {{- $bytesWithMargin := add $bytesNeeded (div $bytesNeeded 10) -}}
  {{- printf "%dMi" (div (add $bytesWithMargin 1048575) 1048576) -}}
{{- end -}}
{{- end -}}

{{/*
Convert a Kubernetes memory string to megabytes.
Supports: Gi, Mi, G, M, Ki, K formats.
Input: memory string (e.g., "256Mi", "1Gi")
Returns: integer megabytes, or 0 if parsing fails
*/}}
{{- define "haptic.memoryToMB" -}}
{{- $memory := . -}}
{{- if $memory -}}
  {{- /* Result is MiB. G/M/K are decimal units; mulf/divf keep fractions (mul/div truncate first). */ -}}
  {{- if hasSuffix "Gi" $memory -}}
    {{- mulf (trimSuffix "Gi" $memory | float64) 1024 | int -}}
  {{- else if hasSuffix "Mi" $memory -}}
    {{- trimSuffix "Mi" $memory | float64 | int -}}
  {{- else if hasSuffix "G" $memory -}}
    {{- mulf (trimSuffix "G" $memory | float64) 953.67431640625 | int -}}
  {{- else if hasSuffix "M" $memory -}}
    {{- mulf (trimSuffix "M" $memory | float64) 0.95367431640625 | int -}}
  {{- else if hasSuffix "Ki" $memory -}}
    {{- divf (trimSuffix "Ki" $memory | float64) 1024 | int -}}
  {{- else if hasSuffix "K" $memory -}}
    {{- divf (trimSuffix "K" $memory | float64) 1048.576 | int -}}
  {{- else -}}
    {{- divf ($memory | float64) 1048576 | int -}}
  {{- end -}}
{{- else -}}
  {{- 0 -}}
{{- end -}}
{{- end -}}

{{/*
Calculate the effective GOMAXPROCS value for the agent container.
Returns the numeric value in ALL cases (for use in calculations like maxParallel).
Priority:
  1. If user set GOMAXPROCS in extraEnv → use that
  2. If CPU limit exists → estimate from CPU (ceil of limit)
  3. If memory limit exists → calculate from memory (mem_MB / 64)
  4. Fallback → 2
Input: .Values.haproxy.agent context
*/}}
{{- define "haptic.agent.gomaxprocsValue" -}}
{{- $result := 0 -}}
{{- /* 1. Check if user explicitly set GOMAXPROCS */ -}}
{{- range .extraEnv | default list -}}
  {{- if eq .name "GOMAXPROCS" -}}
    {{- $result = .value | int -}}
  {{- end -}}
{{- end -}}
{{- if eq $result 0 -}}
  {{- $limits := .resources.limits | default dict -}}
  {{- if $limits.cpu -}}
    {{- /* 2. If CPU limit exists, estimate from it (automaxprocs behavior) */ -}}
    {{- $result = max 1 (include "haptic.cpuToCores" $limits.cpu | int) -}}
  {{- else if $limits.memory -}}
    {{- /* 3. Calculate from memory limit */ -}}
    {{- $result = div (include "haptic.memoryToMB" $limits.memory | int) 64 -}}
  {{- end -}}
{{- end -}}
{{- /* 4. Ensure minimum of 2 */ -}}
{{- max 2 $result -}}
{{- end -}}

{{/*
Auto-calculate GOMAXPROCS for the agent container.
Returns env var YAML if:
  - No CPU limit is set (automaxprocs won't work correctly)
  - User hasn't provided GOMAXPROCS in extraEnv
  - A memory limit is set (otherwise there's no signal to derive from)
Value comes from haptic.agent.gomaxprocsValue, which uses the same
mem_MB / 64 formula (min 2) when only a memory limit is present.
Input: .Values.haproxy.agent context
*/}}
{{- define "haptic.agent.autoGomaxprocs" -}}
{{- $limits := .resources.limits | default dict -}}
{{- $envNames := list -}}
{{- range .extraEnv | default list -}}{{- $envNames = append $envNames .name -}}{{- end -}}
{{- if and (not (has "GOMAXPROCS" $envNames)) (not $limits.cpu) $limits.memory -}}
- name: GOMAXPROCS
  value: {{ include "haptic.agent.gomaxprocsValue" . | quote }}
{{- end -}}
{{- end -}}
