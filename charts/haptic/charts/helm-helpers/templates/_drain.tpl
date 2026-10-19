{{/*
"true" when the HAProxy pods' agent observes the routing to its pod during the
drain (docs/adr/0037-drain-observes-routing.md): it needs API read access, a
projected token and egress to the API server and kube-proxy.
*/}}
{{- define "haptic.haproxy.drainRouting" -}}
{{- $drain := .Values.haproxy.drain -}}
{{- if and .Values.haproxy.enabled $drain.enabled (gt (int $drain.kubeProxyHealthzPort) 0) (empty .Values.haproxy.lifecycle) -}}
true
{{- end -}}
{{- end -}}
