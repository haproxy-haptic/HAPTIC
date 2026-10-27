{{/*
Image references, binary paths, and the principal UID/GID for the HAProxy pod.

`haptic.haproxy.uid` returns the single UID/GID used for runAsUser, runAsGroup,
fsGroup on the HAProxy pod and runAsUser on the agent container, which must
share the volume group with HAProxy.
*/}}

{{/* Controller image shared by the controller, agent, and lifecycle hooks. */}}
{{- define "haptic.controller.image" -}}
{{- if .Values.controller.image.digest -}}
{{- printf "%s@%s" .Values.controller.image.repository .Values.controller.image.digest -}}
{{- else -}}
{{- printf "%s:%s-haproxy%s" .Values.controller.image.repository (.Values.controller.image.tag | default .Chart.AppVersion) .Values.haproxyVersion -}}
{{- end -}}
{{- end -}}

{{/*
HAProxy image
Uses haproxy.image.tag if set, otherwise looks up the patch/revision version from
haproxyEnterprisePatchVersions (when enterprise.enabled) or haproxyPatchVersions,
falling back to haproxyVersion itself.
Community example:  haproxytech/haproxy-debian:3.2.13
Enterprise example: hapee-registry.haproxy.com/haproxy-enterprise:3.2r1
*/}}
{{- define "haptic.haproxy.image" -}}
{{- $patchVersions := ternary .Values.haproxyEnterprisePatchVersions .Values.haproxyPatchVersions .Values.haproxy.enterprise.enabled -}}
{{- $defaultRepository := ternary "hapee-registry.haproxy.com/haproxy-enterprise" "haproxytech/haproxy-debian" .Values.haproxy.enterprise.enabled -}}
{{- printf "%s:%s" (.Values.haproxy.image.repository | default $defaultRepository) (.Values.haproxy.image.tag | default (index $patchVersions .Values.haproxyVersion) | default .Values.haproxyVersion) -}}
{{- end -}}

{{/*
HAProxy binary path
Enterprise: /opt/hapee-{version}/sbin/hapee-lb
Community: /usr/local/sbin/haproxy
*/}}
{{- define "haptic.haproxy.bin" -}}
{{- .Values.haproxy.haproxyBin | default (ternary (printf "/opt/hapee-%s/sbin/hapee-lb" .Values.haproxyVersion) "/usr/local/sbin/haproxy" .Values.haproxy.enterprise.enabled) -}}
{{- end -}}

{{/*
HAProxy pod principal UID & GID.
Enterprise: 1000 (hapee-lb user / hapee group)
Community:  99   (haproxy user / haproxy group)

Used identically as runAsUser, runAsGroup, fsGroup on the HAProxy pod and as
runAsUser on the agent container (which shares the volume group).
*/}}
{{- define "haptic.haproxy.uid" -}}
{{- ternary 1000 99 .Values.haproxy.enterprise.enabled -}}
{{- end -}}
