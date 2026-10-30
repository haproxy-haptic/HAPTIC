{{/*
Image references, binary paths, and the principal UID/GID for the HAProxy pod.

`haptic.haproxy.uid` returns the single UID/GID used for runAsUser, runAsGroup,
fsGroup on the HAProxy pod and runAsUser on the agent container, which must
share the volume group with HAProxy.
*/}}

{{/* Controller image shared by the controller, agent, and lifecycle hooks. */}}
{{- define "haptic.controller.image" -}}
{{- $releaseDigest := include "haptic.controller.releaseDigest" . -}}
{{- if .Values.controller.image.digest -}}
{{- printf "%s@%s" .Values.controller.image.repository .Values.controller.image.digest -}}
{{- else if .Values.controller.image.tag -}}
{{- printf "%s:%s-haproxy%s" .Values.controller.image.repository .Values.controller.image.tag .Values.haproxyVersion -}}
{{- else if $releaseDigest -}}
{{- printf "%s@%s" .Values.controller.image.repository $releaseDigest -}}
{{- else -}}
{{- printf "%s:%s-haproxy%s" .Values.controller.image.repository .Chart.AppVersion .Values.haproxyVersion -}}
{{- end -}}
{{- end -}}

{{- define "haptic.controller.releaseDigest" -}}
{{- if .Files.Glob "release-images.json" -}}
{{- $metadata := .Files.Get "release-images.json" | mustFromJson -}}
{{- if or (ne $metadata.version .Chart.Version) (ne $metadata.version .Chart.AppVersion) -}}
{{- fail "release-images.json version differs from the chart; use an unmodified release chart" -}}
{{- end -}}
{{- if not (kindIs "map" $metadata.digests) -}}
{{- fail "release-images.json has no digest map; use an unmodified release chart" -}}
{{- end -}}
{{- if ne (len $metadata.digests) (len .Values.haproxyPatchVersions) -}}
{{- fail "release-images.json does not cover the supported HAProxy series; use an unmodified release chart" -}}
{{- end -}}
{{- range $series, $_ := .Values.haproxyPatchVersions -}}
{{- $digest := index $metadata.digests $series -}}
{{- if not (and (kindIs "string" $digest) (regexMatch "^sha256:[a-f0-9]{64}$" (toString $digest))) -}}
{{- fail (printf "release-images.json has no valid digest for HAProxy %s; use an unmodified release chart" $series) -}}
{{- end -}}
{{- end -}}
{{- required "release-images.json has no digest for the selected HAProxy series" (index $metadata.digests .Values.haproxyVersion) -}}
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
