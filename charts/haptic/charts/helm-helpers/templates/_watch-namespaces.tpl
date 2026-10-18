{{/*
haptic.applySecretScope scopes core Secret watches by resource, including aliases.
Args: [watch union, root context].
*/}}
{{- define "haptic.applySecretScope" -}}
{{- $union := index . 0 -}}
{{- $root := index . 1 -}}
{{- if $root.Values.controller.rbac.releaseNamespaceSecretsOnly -}}
  {{- range $name, $resource := $union -}}
    {{- $coreSecret := false -}}
    {{- if eq $resource.resources "secrets" -}}
      {{- range $version := (include "haptic.candidateVersionsOf" $resource | fromJsonArray) -}}
        {{- if and (kindIs "string" $version) (not (contains "/" $version)) -}}
          {{- $coreSecret = true -}}
        {{- end -}}
      {{- end -}}
    {{- end -}}
    {{- if $coreSecret -}}
      {{- if hasKey $resource "namespaces" -}}
        {{- fail (printf "controller.config.watchedResources.%s.namespaces and controller.rbac.releaseNamespaceSecretsOnly both set the Secret watch's namespaces, so one would silently win. Remove controller.config.watchedResources.%s.namespaces." $name $name) -}}
      {{- end -}}
      {{- $_ := set $resource "namespaces" (list $root.Release.Namespace) -}}
      {{- $_ := set $resource "namespacesSetting" "controller.rbac.releaseNamespaceSecretsOnly" -}}
    {{- end -}}
  {{- end -}}
  {{- $default := $root.Values.defaultSSLCertificate -}}
  {{- if and $default.enabled $default.namespace (ne $default.namespace $root.Release.Namespace) -}}
    {{- fail (printf "defaultSSLCertificate.namespace is %s, but controller.rbac.releaseNamespaceSecretsOnly lets the controller read Secrets only in %s, so HTTPS would have no certificate. Leave defaultSSLCertificate.namespace empty." $default.namespace $root.Release.Namespace) -}}
  {{- end -}}
{{- end -}}
{{- end -}}

{{/*
haptic.watchedResourceRules renders the read rules, then the status-patch rules,
for a dict of watchedResources entries. The ClusterRole and the namespaced watch
Roles both use it, so a scoped watch gets exactly the verbs a cluster-wide one would.
*/}}
{{- define "haptic.watchedResourceRules" -}}
{{- range $name, $resource := . }}
{{- $groups := list }}
{{- range $v := include "haptic.candidateVersionsOf" $resource | fromJsonArray }}
{{- $groups = append $groups (include "haptic.apiGroupOf" $v) }}
{{- end }}
- apiGroups:
    {{- range $g := $groups | uniq }}
    - {{ $g }}
    {{- end }}
  resources:
    - {{ $resource.resources }}
  verbs: ["get", "list", "watch"]
{{- end }}
{{- range $name, $resource := . }}
{{- if $resource.statusPatch }}
{{- $groups := list }}
{{- range $v := include "haptic.candidateVersionsOf" $resource | fromJsonArray }}
{{- $groups = append $groups (include "haptic.apiGroupOf" $v) }}
{{- end }}
- apiGroups:
    {{- range $g := $groups | uniq }}
    - {{ $g }}
    {{- end }}
  resources:
    - {{ $resource.resources }}/status
  verbs: ["patch"]
{{- end }}
{{- end }}
{{- end -}}

{{/*
haptic.scopedWatches splits the watch union into namespace -> {name: resource}
for every watch that lists namespaces. A scoped entry no library or operator
value defines fails here, where a typo would otherwise drop the watch.
*/}}
{{- define "haptic.scopedWatches" -}}
{{- $byNamespace := dict -}}
{{- range $name, $resource := (include "haptic.watchedResourcesUnion" . | fromYaml) -}}
  {{- if hasKey $resource "namespaces" -}}
    {{- if not $resource.resources -}}
      {{- fail (printf "controller.config.watchedResources.%s lists namespaces, but no library or value defines a watch named %s, so nothing would be watched. Correct the name." $name $name) -}}
    {{- end -}}
    {{- if not (kindIs "slice" $resource.namespaces) -}}
      {{- fail (printf "controller.config.watchedResources.%s.namespaces must be a list of namespace names." $name) -}}
    {{- end -}}
    {{- if eq (len $resource.namespaces) 0 -}}
      {{- fail (printf "controller.config.watchedResources.%s.namespaces is empty, which would watch nothing. List at least one namespace or remove the field to watch all namespaces." $name) -}}
    {{- end -}}
    {{- range $namespace := $resource.namespaces -}}
      {{- if or (not (kindIs "string" $namespace)) (gt (len $namespace) 63) (not (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?$" $namespace)) -}}
        {{- fail (printf "controller.config.watchedResources.%s.namespaces contains %v, which is not a namespace name." $name $namespace) -}}
      {{- end -}}
      {{- $watches := index $byNamespace $namespace | default dict -}}
      {{- $_ := set $watches $name $resource -}}
      {{- $_ := set $byNamespace $namespace $watches -}}
    {{- end -}}
  {{- end -}}
{{- end -}}
{{- $byNamespace | toYaml -}}
{{- end -}}
