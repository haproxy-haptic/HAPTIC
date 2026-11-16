{{/*
Vector sidecar helpers.

The sidecar is HAPTIC's own operational plumbing on the pods it deploys to — the
log/metrics path for the fleet it manages — so kind-specific Helm and Go for it is
correct and NOT a RULE #1 violation (see the "operational-identity exception" in
the root CLAUDE.md). It is not a resource an operator swaps out when describing
their routing.
*/}}

{{/* True when the vector sidecar should be rendered. */}}
{{- define "haptic.vector.enabled" -}}
{{- if .Values.vector.enabled -}}
true
{{- end -}}
{{- end -}}

{{- define "haptic.vector.configMapName" -}}
{{- printf "%s-vector" (include "haptic.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "haptic.vector.image" -}}
{{- $v := .Values.vector.image -}}
{{- printf "%s:%s" $v.repository $v.tag -}}
{{- end -}}

{{/*
UDP address HAProxy sends access-log records to and vector binds. Loopback only,
so nothing outside the pod can inject records. Not a UNIX datagram socket: the
pod's net.unix.max_dgram_qlen of 10 overflowed on every reload burst (#677).
*/}}
{{- define "haptic.vector.logAddress" -}}
127.0.0.1:9514
{{- end -}}

{{/*
The metrics port Prometheus should scrape for an HAProxy pod, and the endpoint
list that goes with it. When vector is enabled it fronts everything on one port;
otherwise callers fall back to the pre-existing direct endpoints.
*/}}
{{- define "haptic.vector.metricsPort" -}}
{{- .Values.vector.metricsPort | int -}}
{{- end -}}

{{/*
Validate the vector values. Duplicated in the Scriggo library on purpose: the
HAProxyTemplateConfig CR is a first-class API that bypasses Helm entirely, so a
guard that lives only here would not protect a hand-written CR.
*/}}
{{- define "haptic.vector.validateValues" -}}
{{- $v := .Values.vector -}}
{{- /* Moved keys fail with the destination, not as "unknown field": HAProxy's
       exporter is scraped directly since the re-export was removed, and one
       PodMonitor now covers every endpoint on the HAProxy pod. */ -}}
{{- if hasKey $v "excludeMetrics" -}}
  {{- fail "vector.excludeMetrics moved to controller.config.templatingSettings.extraContext.prometheusExporter.excludeMetrics: HAProxy applies the exclusions itself, and Prometheus scrapes its exporter directly instead of through vector. Entries keep their names, `enabled`, `families` and `requires`; `pattern` was dropped — the exporter filters by exact family name." -}}
{{- end -}}
{{- if hasKey $v "excludeMaintServerMetrics" -}}
  {{- fail "vector.excludeMaintServerMetrics moved to controller.config.templatingSettings.extraContext.prometheusExporter.excludeMaintServers: HAProxy applies ?no-maint itself, for every scraper." -}}
{{- end -}}
{{- if hasKey $v "socketPath" -}}
  {{- fail "vector.socketPath was removed: HAProxy now sends the access log to Vector over loopback UDP. Delete the value." -}}
{{- end -}}
{{- if hasKey $v "podMonitor" -}}
  {{- fail "vector.podMonitor moved to haproxy.monitoring.podMonitor: one PodMonitor now scrapes every metrics endpoint on the HAProxy pod — HAProxy's exporter (stats port), vector's endpoints and, without vector, the hub's. Same fields (enabled, interval, scrapeTimeout, labels, relabelings, metricRelabelings)." -}}
{{- end -}}
{{- if $v.enabled -}}
  {{- /* Validate the dormant port too, before request_size activates its child listener. */ -}}
  {{- $ports := dict "metricsPort" ($v.metricsPort | int) -}}
  {{- $_ := set $ports "sizeMetricsPort" ($v.sizeMetricsPort | int) -}}
  {{- $hubPort := include "haptic.spoaHub.metricsPort" . -}}
  {{- range $key, $port := $ports -}}
    {{- if or (lt $port 1) (gt $port 65535) -}}
      {{- fail (printf "vector.%s must be a TCP port between 1 and 65535, got %v." $key $port) -}}
    {{- end -}}
    {{- /* A collision here renders a pod that crash-loops on bind, so catch it at
           render time rather than in CrashLoopBackOff. */ -}}
    {{- range $name, $p := $.Values.haproxy.ports -}}
      {{- if and (ne ($p | int) 0) (eq ($p | int) $port) -}}
        {{- fail (printf "vector.%s (%v) collides with haproxy.ports.%s. Both bind in the same pod network namespace; pick a different port." $key $port $name) -}}
      {{- end -}}
    {{- end -}}
    {{- if and (ne $hubPort "") (eq ($hubPort | int) $port) -}}
      {{- fail (printf "vector.%s (%v) collides with the spoa-hub metrics port. Pick a different port." $key $port) -}}
    {{- end -}}
  {{- end -}}
  {{- if eq ($v.metricsPort | int) ($v.sizeMetricsPort | int) -}}
    {{- fail (printf "vector.metricsPort and vector.sizeMetricsPort are both %v. They are two prometheus_exporter sinks with different histogram buckets, so they cannot share a port." ($v.metricsPort | int)) -}}
  {{- end -}}
  {{- if lt ($v.scrapeIntervalSecs | int) 1 -}}
    {{- fail (printf "vector.scrapeIntervalSecs must be a positive integer, got %v." $v.scrapeIntervalSecs) -}}
  {{- end -}}
  {{- include "haptic.vector.validateRequestMetrics" . -}}
  {{- if eq (trim ($v.image.tag | toString)) "" -}}
    {{- fail "vector.image.tag must be pinned to an explicit tag so a silent upstream bump can't change the log pipeline under a running fleet." -}}
  {{- end -}}
{{- end -}}
{{- end -}}

{{/* Shared by the container port, the PodMonitor endpoint and the projection, so
the three cannot disagree about whether sizeMetricsPort is listening. */}}
{{- define "haptic.vector.sizeExporterEnabled" -}}
{{- $rm := .Values.vector.requestMetrics | default dict -}}
{{- if and .Values.vector.enabled $rm.enabled -}}
  {{- $m := $rm.metrics | default dict -}}
  {{- if or (index $m "request_size") (index $m "response_size") -}}
true
  {{- end -}}
{{- end -}}
{{- end -}}

{{/*
Walk a dotted .Values path; emit "true" when truthy. Backs `requires` on
logMetrics. Here, not in the library, because the flags live in values rather
than extraContext.

Usage: include "haptic.vector.valuePath" (dict "root" $.Values "path" "a.b.c")
*/}}
{{- define "haptic.vector.valuePath" -}}
{{- $cur := .root -}}
{{- range $seg := splitList "." .path -}}
  {{- if kindIs "map" $cur -}}
    {{- $cur = index $cur $seg -}}
  {{- else -}}
    {{- $cur = nil -}}
  {{- end -}}
{{- end -}}
{{- if $cur -}}
true
{{- end -}}
{{- end -}}

{{/* Mirrored in the Scriggo library because a hand-written CR bypasses Helm. */}}
{{- define "haptic.vector.validateRequestMetrics" -}}
{{- $rm := .Values.vector.requestMetrics | default dict -}}
{{- if $rm.enabled -}}
  {{- /* A trailing underscore is accepted (operators think of this as
         `nginx_ingress_controller_`) and stripped, so the name never doubles up. */ -}}
  {{- $prefix := $rm.prefix | toString | trimSuffix "_" -}}
  {{- if not (regexMatch "^[A-Za-z_][A-Za-z0-9_]*$" $prefix) -}}
    {{- fail (printf "vector.requestMetrics.prefix must be a Prometheus metric-name prefix (letters, digits, underscore; not starting with a digit), got %q." $rm.prefix) -}}
  {{- end -}}
  {{- /* `le` boundaries are cumulative: out of order the counts are nonsense and
         nothing reports it. */ -}}
  {{- range $which := list "durationBuckets" "sizeBuckets" -}}
    {{- $bs := index $rm $which -}}
    {{- if eq (len $bs) 0 -}}
      {{- fail (printf "vector.requestMetrics.%s is empty. A histogram with no boundaries reports only +Inf, which carries no information beyond the count." $which) -}}
    {{- end -}}
    {{- $prev := 0.0 -}}
    {{- range $i, $b := $bs -}}
      {{- $f := $b | float64 -}}
      {{- if le $f 0.0 -}}
        {{- fail (printf "vector.requestMetrics.%s[%d] must be greater than zero, got %v." $which $i $b) -}}
      {{- end -}}
      {{- /* The render formats with 9 decimals, so anything smaller emits a
             le="0" boundary — and two such values collapse onto one. */ -}}
      {{- if lt $f 0.000000001 -}}
        {{- fail (printf "vector.requestMetrics.%s[%d] is %v, smaller than the exporter renders (9 decimals); it would emit a le=\"0\" boundary." $which $i $b) -}}
      {{- end -}}
      {{- if and (gt $i 0) (le $f $prev) -}}
        {{- fail (printf "vector.requestMetrics.%s must be strictly ascending; %v follows %v. Prometheus reads these as cumulative `le` boundaries, so out-of-order values produce silently wrong quantiles." $which $b $prev) -}}
      {{- end -}}
      {{- $prev = $f -}}
    {{- end -}}
  {{- end -}}
  {{- $cl := $rm.cardinalityLimit | default dict -}}
  {{- if $cl.enabled -}}
    {{- if lt ($cl.valueLimit | int) 1 -}}
      {{- fail (printf "vector.requestMetrics.cardinalityLimit.valueLimit must be a positive integer, got %v." $cl.valueLimit) -}}
    {{- end -}}
    {{- if not (has ($cl.action | toString) (list "drop_tag" "drop_event")) -}}
      {{- fail (printf "vector.requestMetrics.cardinalityLimit.action must be \"drop_tag\" or \"drop_event\", got %q. drop_tag collapses the runaway label and keeps the totals; drop_event discards the requests, so a cardinality problem would read as an outage." $cl.action) -}}
    {{- end -}}
  {{- end -}}
  {{- $m := $rm.metrics | default dict -}}
  {{- $on := 0 -}}
  {{- range $name, $val := $m -}}
    {{- if $val -}}
      {{- $on = add1 $on -}}
    {{- end -}}
  {{- end -}}
  {{- if eq $on 0 -}}
    {{- fail "vector.requestMetrics.enabled is true but every entry in `metrics` is false, so the pipeline would parse each access-log record and emit nothing. Set vector.requestMetrics.enabled: false instead." -}}
  {{- end -}}
{{- end -}}
{{- end -}}
