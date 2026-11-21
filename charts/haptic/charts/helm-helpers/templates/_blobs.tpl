{{/*
Binary blobs (ADR-0041). "true" when a blob source is configured: blobs.enabled
for templates of your own, or a database under extraContext.maxmind.databases.
Empty otherwise, so neither Deployment changes.
*/}}
{{- define "haptic.blobs.enabled" -}}
{{- $databases := dig "templatingSettings" "extraContext" "maxmind" "databases" dict (.Values.controller.config | default dict) -}}
{{- if or .Values.blobs.enabled (not (empty $databases)) -}}true{{- end -}}
{{- end -}}

{{/*
Where every container that holds blobs mounts them. It is blob.PathPrefix, the
path a rendered configuration names.
*/}}
{{- define "haptic.blobs.dir" -}}
/var/lib/haptic/blobs
{{- end -}}
