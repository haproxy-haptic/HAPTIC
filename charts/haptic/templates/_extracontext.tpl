{{- /* The templatingSettings.extraContext the chart computes from a values
       tree. haproxytemplateconfig.yaml renders it twice: from the deployment's
       values, and from the chart defaults for validationTests. */ -}}
{{- define "haptic.extraContext" -}}
{{- $controller := .Values.controller }}
{{- $configuredExtraContext := dig "templatingSettings" "extraContext" dict ($controller.config | default dict) }}
{{- /* Trace export needs the vector sidecar: spans are built from access-log
       records inside vector, so an OTLP endpoint without it exports nothing and
       traces would silently never appear.

       This lives in HELM, not in the Scriggo render, on purpose. A validationTest
       may legitimately set `extraContext.vector: null` to exercise the
       sidecar-less path, so the same check at render time would fail the LOAD
       GATE on a good config and keep the controller from starting. Here it is an install
       error instead: loud, immediate, and impossible to hit at runtime. */ -}}
{{- $tracingCtx := dig "tracing" dict $configuredExtraContext }}
{{- $otlpEndpoint := dig "otlp" "endpoint" "" $tracingCtx }}
{{- if and $otlpEndpoint (not (dig "enabled" false $tracingCtx)) }}
  {{- fail (printf "controller.config.templatingSettings.extraContext.tracing.otlp.endpoint is set (%s) but tracing.enabled is false, so no trace ids are minted and there would be nothing to export. Set tracing.enabled=true." $otlpEndpoint) }}
{{- end }}
{{- if and $otlpEndpoint (not .Values.vector.enabled) }}
  {{- fail (printf "controller.config.templatingSettings.extraContext.tracing.otlp.endpoint is set (%s) but vector.enabled is false. Spans are built from access-log records inside the vector sidecar, so nothing would export them and traces would silently never appear. Set vector.enabled=true, or remove the endpoint to keep propagation-only tracing (HAProxy still sets traceparent so your backends join the trace)." $otlpEndpoint) }}
{{- end }}
{{- /* PROXY-protocol binds. Validated in HELM rather than at render time because
       the port numbers drive chart-side wiring too (Service ports, container
       ports, NetworkPolicy) — a bad value has to fail the install, not the load
       gate. */ -}}
{{- $proxyProtocol := include "haptic.proxyProtocol" . | fromYaml }}
{{- if $proxyProtocol.enabled }}
  {{- range $field := list "httpPort" "httpsPort" }}
    {{- $port := int (index $proxyProtocol $field) }}
    {{- if or (lt $port 1) (gt $port 65535) }}
      {{- fail (printf "controller.config.templatingSettings.extraContext.proxyProtocol.%s must be a port between 1 and 65535, got %v." $field (index $proxyProtocol $field)) }}
    {{- end }}
  {{- end }}
  {{- if eq (int $proxyProtocol.httpPort) (int $proxyProtocol.httpsPort) }}
    {{- fail (printf "controller.config.templatingSettings.extraContext.proxyProtocol.httpPort and .httpsPort are both %d. They are separate HAProxy binds and must differ." (int $proxyProtocol.httpPort)) }}
  {{- end }}
  {{- $takenPorts := dict
      (toString (int .Values.haproxy.ports.http)) "haproxy.ports.http"
      (toString (int .Values.haproxy.ports.https)) "haproxy.ports.https"
      (toString (int .Values.haproxy.ports.stats)) "haproxy.ports.stats"
      (toString (int .Values.haproxy.ports.dataplane)) "haproxy.ports.dataplane"
      (toString (int .Values.haproxy.ports.agentMetrics)) "haproxy.ports.agentMetrics" }}
  {{- /* Operator-declared Service ports share the port and name space with the
         two entries the chart appends. A duplicate name is silently dropped by
         the Service template's name dedupe, and a duplicate number renders a
         Service the API server rejects — both worse than an install error. */ -}}
  {{- $takenNames := dict }}
  {{- range $entry := (dig "service" "extraPorts" list .Values.haproxy) }}
    {{- $entryPort := int (dig "port" 0 $entry) }}
    {{- if gt $entryPort 0 }}
      {{- $_ := set $takenPorts (toString $entryPort) "haproxy.service.extraPorts" }}
    {{- end }}
    {{- with (dig "name" "" $entry) }}
      {{- $_ := set $takenNames (toString .) "haproxy.service.extraPorts" }}
    {{- end }}
  {{- end }}
  {{- range $field := list "httpPort" "httpsPort" }}
    {{- $port := toString (int (index $proxyProtocol $field)) }}
    {{- if hasKey $takenPorts $port }}
      {{- fail (printf "controller.config.templatingSettings.extraContext.proxyProtocol.%s (%s) collides with %s. The PROXY-protocol binds are additional listeners on the same pod; give them a free port." $field $port (index $takenPorts $port)) }}
    {{- end }}
  {{- end }}
  {{- range $name := list "proxy-http" "proxy-https" }}
    {{- if hasKey $takenNames $name }}
      {{- fail (printf "haproxy.service.extraPorts declares a port named %q, which the PROXY-protocol listeners also use. Rename your entry, or disable controller.config.templatingSettings.extraContext.proxyProtocol." $name) }}
    {{- end }}
  {{- end }}
{{- end }}
{{- /* HTTP/3 drives the Service, container and NetworkPolicy UDP ports too, so a
       bad value fails the install rather than the load gate. */ -}}
{{- $http3 := include "haptic.http3" . | fromYaml }}
{{- $altSvcPort := toString $http3.altSvc.port }}
{{- if or (not (regexMatch "^[0-9]+$" $altSvcPort)) (gt (atoi $altSvcPort) 65535) }}
  {{- fail (printf "controller.config.templatingSettings.extraContext.http3.altSvc.port must be 0 (the port the client used) or a port between 1 and 65535, got %s." $altSvcPort) }}
{{- end }}
{{- $altSvcMaxAge := toString $http3.altSvc.maxAge }}
{{- if or (not (regexMatch "^[0-9]+$" $altSvcMaxAge)) (eq (atoi $altSvcMaxAge) 0) }}
  {{- fail (printf "controller.config.templatingSettings.extraContext.http3.altSvc.maxAge must be a positive number of seconds, got %s." $altSvcMaxAge) }}
{{- end }}
{{- if $http3.enabled }}
  {{- range $entry := (dig "service" "extraPorts" list .Values.haproxy) }}
    {{- if eq (toString (dig "name" "" $entry)) "https-quic" }}
      {{- fail "haproxy.service.extraPorts declares a port named \"https-quic\", which the HTTP/3 listener also uses. Rename your entry, or set controller.config.templatingSettings.extraContext.http3.enabled=false." }}
    {{- end }}
  {{- end }}
{{- end }}
{{- $apiGatewaySettings := dict }}
{{- if hasKey $configuredExtraContext "apiGateway" }}
  {{- $apiGatewaySettings = $configuredExtraContext.apiGateway }}
{{- end }}
{{- $requestSchemaValidation := dict }}
{{- if hasKey $apiGatewaySettings "requestSchemaValidation" }}
  {{- $requestSchemaValidation = $apiGatewaySettings.requestSchemaValidation }}
{{- end }}
{{- $requestSchemaBody := dict }}
{{- if hasKey $requestSchemaValidation "requestBody" }}
  {{- $requestSchemaBody = $requestSchemaValidation.requestBody }}
{{- end }}
{{- $requestSchemaValidationEnabled := eq (dig "enabled" false $requestSchemaValidation | toString) "true" }}
{{- $wafSettings := dict }}
{{- if hasKey $configuredExtraContext "waf" }}
  {{- $wafSettings = $configuredExtraContext.waf }}
{{- end }}
{{- if hasKey $wafSettings "crs" }}
  {{- $crs := $wafSettings.crs }}
  {{- $crsURL := $crs.url | default "" }}
  {{- if and (ne $crsURL "") (not (or (hasPrefix "https://" $crsURL) (hasPrefix "http://" $crsURL))) }}
    {{- fail (printf "controller.config.templatingSettings.extraContext.waf.crs.url must be an http:// or https:// URL, got %q." $crsURL) }}
  {{- end }}
  {{- /* The ruleset governs what the WAF blocks. Fetching it over plaintext
         lets anyone on the path replace it, and the result still validates and
         still looks healthy — so this is refused rather than warned about. */ -}}
  {{- if hasPrefix "http://" $crsURL }}
    {{- fail (printf "controller.config.templatingSettings.extraContext.waf.crs.url uses plaintext http://, so the ruleset can be replaced in transit by anyone on the network path and the substituted WAF would still validate. Use https://. Got %q." $crsURL) }}
  {{- end }}
{{- end }}
{{- $wafDispatch := dict }}
{{- if hasKey $wafSettings "dispatch" }}
  {{- $wafDispatch = $wafSettings.dispatch }}
{{- end }}
{{- $wafDispatchMode := dig "mode" "opt-in" $wafDispatch | toString }}
{{- $wafPolicies := dict }}
{{- if hasKey $wafSettings "policies" }}
  {{- $wafPolicies = $wafSettings.policies }}
{{- end }}
{{- $wafSelfService := dict }}
{{- if hasKey $wafPolicies "selfService" }}
  {{- $wafSelfService = $wafPolicies.selfService }}
{{- end }}
{{- $wafSelfServiceEnabled := and (hasKey $wafSelfService "enabled") (eq (toString $wafSelfService.enabled) "true") }}
{{- $wafPoliciesConfigured := false }}
{{- if and (hasKey $wafPolicies "inline") (kindIs "map" $wafPolicies.inline) (gt (len $wafPolicies.inline) 0) }}{{- $wafPoliciesConfigured = true }}{{- end }}
{{- if and (hasKey $wafPolicies "configMapRefs") (kindIs "map" $wafPolicies.configMapRefs) (gt (len $wafPolicies.configMapRefs) 0) }}{{- $wafPoliciesConfigured = true }}{{- end }}
{{- if and (hasKey $wafPolicies "defaultPolicy") (kindIs "string" $wafPolicies.defaultPolicy) (ne $wafPolicies.defaultPolicy "") }}{{- $wafPoliciesConfigured = true }}{{- end }}
{{- if $wafSelfServiceEnabled }}{{- $wafPoliciesConfigured = true }}{{- end }}
{{- $wafIngressPermissions := dict }}
{{- if hasKey $wafSettings "ingressPermissions" }}
  {{- $wafIngressPermissions = $wafSettings.ingressPermissions }}
{{- end }}
{{- $wafRequired := or $wafPoliciesConfigured (eq $wafDispatchMode "default-on") }}
{{- if and .Values.rateLimit.shared.enabled (eq (toString .Values.spoaHub.enabled) "false") }}
  {{- fail "rateLimit.shared.enabled=true requires the spoaHub sidecar. Remove spoaHub.enabled=false or disable shared rate limiting." }}
{{- end }}
{{- $rateLimitPlugin := (index (.Values.spoaHub.plugins | default dict) "rate-limit") | default dict }}
{{- if and .Values.rateLimit.shared.enabled (not (include "haptic.spoaHub.pluginEnabled" (dict "plugin" $rateLimitPlugin "root" . "name" "rate-limit"))) }}
  {{- fail "rateLimit.shared.enabled=true requires spoaHub.plugins.rate-limit to be enabled. Remove spoaHub.plugins.rate-limit.enabled=false or disable shared rate limiting." }}
{{- end }}
{{- if and $requestSchemaValidationEnabled (eq (toString .Values.spoaHub.enabled) "false") }}
  {{- fail "controller.config.templatingSettings.extraContext.apiGateway.requestSchemaValidation.enabled=true requires the spoaHub sidecar. Remove spoaHub.enabled=false or disable request validation." }}
{{- end }}
{{- $apiGatewayPlugin := (index (.Values.spoaHub.plugins | default dict) "api-gateway") | default dict }}
{{- if and $requestSchemaValidationEnabled (not (include "haptic.spoaHub.pluginEnabled" (dict "plugin" $apiGatewayPlugin "root" . "name" "api-gateway"))) }}
  {{- fail "controller.config.templatingSettings.extraContext.apiGateway.requestSchemaValidation.enabled=true requires spoaHub.plugins.api-gateway to be enabled. Remove spoaHub.plugins.api-gateway.enabled=false or disable request validation." }}
{{- end }}
{{- if and $requestSchemaValidationEnabled (not $controller.templateLibraries.hapticAnnotations.enabled) }}
  {{- fail "controller.config.templatingSettings.extraContext.apiGateway.requestSchemaValidation.enabled=true requires controller.templateLibraries.hapticAnnotations.enabled=true because request-schema validation is a HAPTIC Ingress annotation feature." }}
{{- end }}
{{- if and $wafRequired (eq (toString .Values.spoaHub.enabled) "false") }}
  {{- fail "The configured WAF dispatch/policy behavior requires the spoaHub sidecar. Remove spoaHub.enabled=false, set waf.dispatch.mode=opt-in, or remove the WAF policy catalog." }}
{{- end }}
{{- $corazaPlugin := (index (.Values.spoaHub.plugins | default dict) "coraza") | default dict }}
{{- if and $wafRequired (not (include "haptic.spoaHub.pluginEnabled" (dict "plugin" $corazaPlugin "root" . "name" "coraza"))) }}
  {{- fail "The configured WAF dispatch/policy behavior requires spoaHub.plugins.coraza. Remove spoaHub.plugins.coraza.enabled=false, set waf.dispatch.mode=opt-in, or remove the WAF policy catalog." }}
{{- end }}
{{- if and $wafPoliciesConfigured (not $controller.templateLibraries.hapticAnnotations.enabled) }}
  {{- fail "controller.config.templatingSettings.extraContext.waf.policies requires controller.templateLibraries.hapticAnnotations.enabled=true because WAF policies are a HAPTIC Ingress annotation feature." }}
{{- end }}
{{- $extraContext := deepCopy $configuredExtraContext }}

  {{- /* Which HAPTIC emitted a span. Several deployments per cluster — one for
         internal traffic and one for external, or one per tenant — otherwise all
         report service.name=haptic and are indistinguishable in the trace
         backend. These become OTLP RESOURCE attributes, which describe the
         emitter, so they are derived from the release rather than configured.
         An operator value always wins; k8s.cluster.name has no default because
         Kubernetes exposes no cluster name to a pod. */ -}}
  {{- if dig "tracing" "otlp" "endpoint" "" $extraContext }}
    {{- $tr := (dig "tracing" dict $extraContext) }}
    {{- $otlp := (dig "otlp" dict $tr) }}
    {{- if not (dig "serviceNamespace" "" $otlp) }}
      {{- $_ := set $otlp "serviceNamespace" .Release.Namespace }}
    {{- end }}
    {{- if not (dig "serviceVersion" "" $otlp) }}
      {{- $_ := set $otlp "serviceVersion" (.Chart.AppVersion | toString) }}
    {{- end }}
    {{- if not (dig "deploymentName" "" $otlp) }}
      {{- $_ := set $otlp "deploymentName" (include "haptic.haproxyFullname" .) }}
    {{- end }}
    {{- $_ := set $tr "otlp" $otlp }}
    {{- $_ := set $extraContext "tracing" $tr }}
  {{- end }}

  {{- /* Which Ingress annotation families are active, for the cross-family
         feature-collision validator in the ingress-annotations-compat
         library. A disabled family's annotations are inert, so the validator
         must only flag collisions between enabled families — and
         templateLibraries.*.enabled is a Helm value the Scriggo runtime can't
         otherwise see. Chart-managed and reserved (see _controller.tpl). */ -}}
  {{- $tl := .Values.controller.templateLibraries }}
  {{- $_ := set $extraContext "annotationLibraries" (dict
        "haptic" (dig "hapticAnnotations" "enabled" false $tl)
        "nginx" (dig "nginxIngress" "enabled" false $tl)
        "haproxyIngress" (dig "haproxyIngress" "enabled" false $tl)
        "haproxytech" (dig "haproxytech" "enabled" false $tl)) }}

  {{- /* Default SSL certificate */ -}}
  {{- $defaultSSL := .Values.defaultSSLCertificate }}
  {{- if $defaultSSL.enabled }}
    {{- $tls := $extraContext.tls | default dict }}
    {{- $_ := set $extraContext "tls" $tls }}
    {{- $_ := set $tls "defaultCertificate" (dict
          "name" $defaultSSL.secretName
          "namespace" ($defaultSSL.namespace | default .Release.Namespace)
          "ecdsaName" ($defaultSSL.ecdsaSecretName | default "")) }}
  {{- end }}

  {{- /* Gateway API controllerName for status patches */ -}}
  {{- if $controller.templateLibraries.gateway.enabled }}
    {{- $_ := set $extraContext "controllerName" .Values.gatewayClass.controllerName }}
  {{- end }}

  {{- /* GatewayClass shape for the runtime emitter (util-emit-gatewayclass).
         The GatewayClass is created by the CONTROLLER via k8sResources — not
         by a Helm template — so it exists exactly when the gatewayclasses CRD
         is served, surviving install-order races and in-place Gateway API
         installation without a helm operation. */ -}}
  {{- if and $controller.templateLibraries.gateway.enabled .Values.gatewayClass.enabled }}
    {{- $gcParams := .Values.gatewayClass.parametersRef }}
    {{- $_ := set $extraContext "gatewayClassResource" (dict
          "name" .Values.gatewayClass.name
          "default" .Values.gatewayClass.default
          "controllerName" .Values.gatewayClass.controllerName
          "parametersGroup" $gcParams.group
          "parametersKind" $gcParams.kind
          "parametersName" ($gcParams.name | default $controller.configName)
          "parametersNamespace" ($gcParams.namespace | default .Release.Namespace)) }}
  {{- end }}

  {{- /* HAProxy version (used by version-gated features such as shm-stats-file) */ -}}
  {{- $_ := set $extraContext "haproxyVersion" .Values.haproxyVersion }}

  {{- /* Session-persistence dynamic-cookie-key: shared across same-shape cookie
         backends so they dedup into one profile, but per-installation so a pod's
         address→cookie hash can't be forged from a world-known salt. Derived from
         release identity (stable across upgrades, no `lookup` — safe on
         template/dry-run/GitOps); an operator can override with
         extraContext.dynamicCookieKey. */ -}}
  {{- if not (hasKey $extraContext "dynamicCookieKey") -}}
    {{- $_ := set $extraContext "dynamicCookieKey" (printf "%s/%s" .Release.Namespace .Release.Name | sha256sum | trunc 32) }}
  {{- end -}}

  {{- /* Vector sidecar: project only the render-facing settings (the root-level
         vector.* value also builds a container and a ConfigMap, which is what
         justifies it living at the root — see charts/CLAUDE.md). */ -}}
  {{- include "haptic.vector.validateValues" . }}
  {{- if .Values.vector.enabled }}
    {{- $hubMetricsPort := include "haptic.spoaHub.metricsPort" . }}
    {{- $hubScrape := 0 }}
    {{- if and (include "haptic.spoaHub.enabled" .) (ne (trim $hubMetricsPort) "") }}
      {{- $hubScrape = $hubMetricsPort | int }}
    {{- end }}
    {{- /* Log-derived metrics. An entry may name a `requires` values path — a
           metric can only work if whatever emits its access-log field is on, and
           rendering a transform that can never match costs a regex per record.
           Resolved here rather than in the library because the feature flags live
           in values, not in extraContext. */ -}}
    {{- $vectorLogMetrics := dict }}
    {{- range $name, $entry := (.Values.vector.logMetrics | default dict) }}
      {{- if not (kindIs "map" $entry) }}{{- fail (printf "vector.logMetrics.%s must be a map." $name) }}{{- end }}
      {{- if not (hasKey $entry "enabled") }}{{- fail (printf "vector.logMetrics.%s must set `enabled`." $name) }}{{- end }}
      {{- $on := $entry.enabled }}
      {{- if and $on (hasKey $entry "requires") }}
        {{- $on = eq (include "haptic.vector.valuePath" (dict "root" $.Values "path" ($entry.requires | toString))) "true" }}
      {{- end }}
      {{- $resolved := omit $entry "requires" }}
      {{- $_ := set $resolved "enabled" $on }}
      {{- $_ := set $vectorLogMetrics $name $resolved }}
    {{- end }}
    {{- /* Prefix normalised and controller_class resolved here, so the library
           renders values rather than re-deriving chart identity. */ -}}
    {{- $rm := .Values.vector.requestMetrics | default dict }}
    {{- $vectorRequestMetrics := dict "enabled" false }}
    {{- if $rm.enabled }}
      {{- $enabledMetrics := dict }}
      {{- range $mname, $mon := ($rm.metrics | default dict) }}
        {{- if $mon }}{{- $_ := set $enabledMetrics $mname true }}{{- end }}
      {{- end }}
      {{- $controllerClass := $rm.controllerClass | toString }}
      {{- if eq (trim $controllerClass) "" }}
        {{- $controllerClass = .Values.ingressClass.controllerName | toString }}
      {{- end }}
      {{- $vectorRequestMetrics = dict
            "enabled" true
            "prefix" ($rm.prefix | toString | trimSuffix "_")
            "controllerClass" $controllerClass
            "controllerNamespace" .Release.Namespace
            "terminationStateLabel" $rm.terminationStateLabel
            "pathLabel" $rm.pathLabel
            "hostLabel" $rm.hostLabel
            "durationBuckets" $rm.durationBuckets
            "sizeBuckets" $rm.sizeBuckets
            "cardinalityLimit" ($rm.cardinalityLimit | default dict)
            "metrics" $enabledMetrics }}
    {{- end }}
    {{- $_ := set $extraContext "vector" (dict
          "socketPath" .Values.vector.socketPath
          "metricsPort" (.Values.vector.metricsPort | int)
          "sizeMetricsPort" (.Values.vector.sizeMetricsPort | int)
          "scrapeIntervalSecs" (.Values.vector.scrapeIntervalSecs | int)
          "spoaHubMetricsPort" $hubScrape
          "omitEmptyLogFields" .Values.vector.omitEmptyLogFields
          "logMetrics" $vectorLogMetrics
          "requestMetrics" $vectorRequestMetrics
          "dataDir" "/var/lib/vector") }}

    {{- /* Point the access log at the socket — but only if the operator hasn't
           chosen targets themselves. Overwriting an explicit `targets` would
           silently redirect a stream they deliberately routed elsewhere. */ -}}
    {{- $accessLog := $extraContext.accessLog | default dict }}
    {{- if not (hasKey $accessLog "targets") }}
      {{- $_ := set $accessLog "targets" (dict "vector" (dict
            "address" .Values.vector.socketPath
            "format" "raw")) }}
    {{- end }}
    {{- $_ := set $extraContext "accessLog" $accessLog }}
  {{- end }}

  {{- /* Shared cache: copy the public feature tree into extraContext and add
         only the release-derived service coordinates. This keeps chart values
         and the standalone annotation library on one structured contract. */ -}}
  {{- $cacheContext := deepCopy .Values.cache }}
  {{- $varnish := $cacheContext.varnish }}
  {{- $_ := set $varnish "image" (include "haptic.varnish.image" .) }}
  {{- if $varnish.enabled }}
    {{- if not .Values.haproxy.enabled }}
      {{- fail "cache.varnish.enabled=true requires the chart-managed HAProxy workload (haproxy.enabled=true): Varnish fetches cache misses from a dedicated backend-fetch frontend on the HAProxy pods." }}
    {{- end }}
    {{- $lp := int ($varnish.loopbackPort | default 8090) }}
    {{- if le $lp 0 }}
      {{- fail "cache.varnish.loopbackPort must be a positive port for the dedicated Varnish backend-fetch frontend." }}
    {{- end }}
    {{- /* The dedicated backend-fetch frontend binds this port on the HAProxy
           pods, so it must not collide with any other statically-configured
           HAProxy/controller listener — otherwise HAProxy fails to bind at
           runtime with a confusing error instead of a clear Helm failure. */ -}}
    {{- $reserved := list (int .Values.haproxy.ports.http) (int .Values.haproxy.ports.https) (int .Values.haproxy.ports.stats) (int .Values.haproxy.ports.dataplane) (int .Values.haproxy.ports.agentMetrics) (int .Values.controller.ports.healthz) (int .Values.controller.ports.metrics) }}
    {{- if $proxyProtocol.enabled }}
      {{- $reserved = append (append $reserved (int $proxyProtocol.httpPort)) (int $proxyProtocol.httpsPort) }}
    {{- end }}
    {{- if has $lp $reserved }}
      {{- fail (printf "cache.varnish.loopbackPort %d collides with an existing HAProxy/controller port (http/https/stats/dataplane/agentMetrics/healthz/metrics/proxy-protocol); choose a free port for the dedicated Varnish backend-fetch frontend." $lp) }}
    {{- end }}
  {{- end }}
  {{- $_ := set $varnish "serviceName" (include "haptic.varnish.cacheName" .) }}
  {{- $_ := set $varnish "namespace" .Release.Namespace }}
  {{- $_ := set $varnish "servicePort" 6081 }}
  {{- $_ := set $extraContext "cache" $cacheContext }}

  {{- /* Shared rate limiting follows the same structured ownership contract. */ -}}
  {{- $rateLimitContext := deepCopy .Values.rateLimit }}
  {{- $rlStore := $rateLimitContext.shared.managedStore }}
  {{- $rlSentinel := $rlStore.sentinel | default dict }}
  {{- if and $rateLimitContext.shared.enabled $rlStore.enabled }}
    {{- $rlReplicas := int $rlStore.replicas }}
    {{- $rlQuorum := int $rlSentinel.quorum }}
    {{- if lt $rlReplicas 3 }}
      {{- fail "rateLimit.shared.managedStore.replicas must be at least 3 for chart-managed Sentinel HA." }}
    {{- end }}
    {{- if or (lt $rlQuorum 1) (gt $rlQuorum $rlReplicas) }}
      {{- fail "rateLimit.shared.managedStore.sentinel.quorum must be between 1 and rateLimit.shared.managedStore.replicas." }}
    {{- end }}
  {{- end }}
  {{- $_ := set $rlStore "serviceName" (include "haptic.rateLimit.storeServiceName" .) }}
  {{- $_ := set $rlStore "namespace" .Release.Namespace }}
  {{- $_ := set $rlSentinel "masterName" (include "haptic.rateLimit.storeSentinelMasterName" .) }}
  {{- $_ := set $rlStore "sentinel" $rlSentinel }}
  {{- $_ := set $extraContext "rateLimit" $rateLimitContext }}

  {{- /* Shared request-body inspection buffer. HAProxy exposes one process-wide
         tune.bufsize, so all body inspectors use one capacity contract. */ -}}
  {{- $requestBodyInspection := dict }}
  {{- if hasKey $extraContext "requestBodyInspection" }}
    {{- $requestBodyInspection = $extraContext.requestBodyInspection }}
  {{- end }}
  {{- $haproxyRequestBuffer := dict }}
  {{- if hasKey $requestBodyInspection "haproxyBuffer" }}
    {{- $haproxyRequestBuffer = $requestBodyInspection.haproxyBuffer }}
  {{- end }}
  {{- $requestBodyBufferSizeRaw := dig "sizeBytes" 16384 $haproxyRequestBuffer | toString }}
  {{- $requestBodyBufferReservedBytesRaw := dig "reservedBytes" 8192 $haproxyRequestBuffer | toString }}
  {{- if not (regexMatch "^[0-9]+$" $requestBodyBufferSizeRaw) }}
    {{- fail "controller.config.templatingSettings.extraContext.requestBodyInspection.haproxyBuffer.sizeBytes must be an integer between 16384 and 2097152 bytes." }}
  {{- end }}
  {{- if not (regexMatch "^[0-9]+$" $requestBodyBufferReservedBytesRaw) }}
    {{- fail "controller.config.templatingSettings.extraContext.requestBodyInspection.haproxyBuffer.reservedBytes must be a positive integer smaller than requestBodyInspection.haproxyBuffer.sizeBytes." }}
  {{- end }}
  {{- $requestBodyBufferSize := int $requestBodyBufferSizeRaw }}
  {{- $requestBodyBufferReservedBytes := int $requestBodyBufferReservedBytesRaw }}
  {{- if or (lt $requestBodyBufferSize 16384) (gt $requestBodyBufferSize 2097152) }}
    {{- fail "controller.config.templatingSettings.extraContext.requestBodyInspection.haproxyBuffer.sizeBytes must be between 16384 and 2097152 bytes." }}
  {{- end }}
  {{- if or (le $requestBodyBufferReservedBytes 0) (ge $requestBodyBufferReservedBytes $requestBodyBufferSize) }}
    {{- fail "controller.config.templatingSettings.extraContext.requestBodyInspection.haproxyBuffer.reservedBytes must be a positive integer smaller than requestBodyInspection.haproxyBuffer.sizeBytes." }}
  {{- end }}

  {{- /* API-gateway request validation. The haptic-annotations library uses
         these flat keys to fail-loud when schema annotations are present
         without the chart opt-in and to pass bounded defaults to the SPOA
         dispatch map. */ -}}
  {{- /* Validate staged settings even while disabled. A disabled feature must
         not make typos or lossy numeric coercion appear valid. */ -}}
  {{- if or $requestSchemaValidationEnabled (gt (len $requestSchemaValidation) 0) }}
    {{- $validation := $requestSchemaValidation }}
    {{- $validationRequestBody := $requestSchemaBody }}
    {{- $maxBodyBytesRaw := dig "defaultMaxBytes" 8192 $validationRequestBody | toString }}
    {{- if not (regexMatch "^[0-9]+$" $maxBodyBytesRaw) }}
      {{- fail "controller.config.templatingSettings.extraContext.apiGateway.requestSchemaValidation.requestBody.defaultMaxBytes must be an integer between 1 and 1048576 bytes." }}
    {{- end }}
    {{- $maxBodyBytes := int $maxBodyBytesRaw }}
    {{- if or (le $maxBodyBytes 0) (gt $maxBodyBytes 1048576) }}
      {{- fail "controller.config.templatingSettings.extraContext.apiGateway.requestSchemaValidation.requestBody.defaultMaxBytes must be between 1 and 1048576 bytes." }}
    {{- end }}
    {{- $maxBufferedBodyBytes := sub $requestBodyBufferSize $requestBodyBufferReservedBytes }}
    {{- if gt $maxBodyBytes $maxBufferedBodyBytes }}
      {{- fail "apiGateway.requestSchemaValidation.requestBody.defaultMaxBytes must not exceed requestBodyInspection.haproxyBuffer.sizeBytes minus requestBodyInspection.haproxyBuffer.reservedBytes, so request validation never sees a truncated HAProxy body." }}
    {{- end }}
    {{- $requestBodyWaitTimeout := (dig "waitTimeout" "100ms" $validationRequestBody | toString) }}
    {{- if not (regexMatch "^[0-9]+(us|ms|s|m|h|d)?$" $requestBodyWaitTimeout) }}
      {{- fail "controller.config.templatingSettings.extraContext.apiGateway.requestSchemaValidation.requestBody.waitTimeout must be a positive HAProxy duration using an optional us, ms, s, m, h, or d suffix." }}
    {{- end }}
    {{- if le (int (regexFind "^[0-9]+" $requestBodyWaitTimeout)) 0 }}
      {{- fail "controller.config.templatingSettings.extraContext.apiGateway.requestSchemaValidation.requestBody.waitTimeout must be a positive HAProxy duration using an optional us, ms, s, m, h, or d suffix." }}
    {{- end }}
  {{- end }}

  {{- /* WAF custom-rule limits apply independently of reusable policies.
         Vendor compatibility annotations must not be able to bypass the
         application-count and rule-byte admission bounds. */ -}}
  {{- $customRules := dict }}
  {{- if hasKey $wafSettings "customRules" }}
    {{- $customRules = $wafSettings.customRules }}
  {{- end }}
  {{- $customRuleLimits := dict }}
  {{- if hasKey $customRules "limits" }}
    {{- $customRuleLimits = $customRules.limits }}
  {{- end }}
  {{- $maxCustomRuleIngressesRaw := dig "maxIngresses" 32 $customRuleLimits | toString }}
  {{- $maxCustomRuleBytesPerIngressRaw := dig "maxBytesPerIngress" 16384 $customRuleLimits | toString }}
  {{- if not (regexMatch "^[0-9]+$" $maxCustomRuleIngressesRaw) }}
    {{- fail "controller.config.templatingSettings.extraContext.waf.customRules.limits.maxIngresses must be an integer between 1 and 1024." }}
  {{- end }}
  {{- if not (regexMatch "^[0-9]+$" $maxCustomRuleBytesPerIngressRaw) }}
    {{- fail "controller.config.templatingSettings.extraContext.waf.customRules.limits.maxBytesPerIngress must be an integer between 1 and 1048576." }}
  {{- end }}
  {{- $maxCustomRuleIngresses := int $maxCustomRuleIngressesRaw }}
  {{- $maxCustomRuleBytesPerIngress := int $maxCustomRuleBytesPerIngressRaw }}
  {{- if or (le $maxCustomRuleIngresses 0) (gt $maxCustomRuleIngresses 1024) }}
    {{- fail "controller.config.templatingSettings.extraContext.waf.customRules.limits.maxIngresses must be between 1 and 1024." }}
  {{- end }}
  {{- if or (le $maxCustomRuleBytesPerIngress 0) (gt $maxCustomRuleBytesPerIngress 1048576) }}
    {{- fail "controller.config.templatingSettings.extraContext.waf.customRules.limits.maxBytesPerIngress must be between 1 and 1048576." }}
  {{- end }}

  {{- /* Reusable WAF policies. The haptic-annotations library
         turns one Ingress annotation into an application policy, body contract,
         and enforcement map. These bounds keep HAProxy buffering and Coraza
         CPU admission explicit and finite. */ -}}
  {{- /* Validate and normalize policy configuration. A non-empty catalog is
         itself the feature activation; there is no redundant enable flag. */ -}}
  {{- if gt (len $wafPolicies) 0 }}
    {{- $waf := $wafPolicies }}
    {{- $policyRequestBody := dict }}
    {{- if hasKey $waf "requestBody" }}
      {{- $policyRequestBody = $waf.requestBody }}
    {{- end }}
    {{- $defaultMaxBodyBytesRaw := dig "defaultMaxBytes" 8192 $policyRequestBody | toString }}
    {{- $maxBodyBytesRaw := dig "maxBytes" 8192 $policyRequestBody | toString }}
    {{- if not (regexMatch "^[0-9]+$" $defaultMaxBodyBytesRaw) }}
      {{- fail "controller.config.templatingSettings.extraContext.waf.policies.requestBody.defaultMaxBytes must be an integer between 1 and 1048576 bytes." }}
    {{- end }}
    {{- if not (regexMatch "^[0-9]+$" $maxBodyBytesRaw) }}
      {{- fail "controller.config.templatingSettings.extraContext.waf.policies.requestBody.maxBytes must be an integer between 1 and 1048576 bytes." }}
    {{- end }}
    {{- $defaultMaxBodyBytes := int $defaultMaxBodyBytesRaw }}
    {{- $maxBodyBytes := int $maxBodyBytesRaw }}
    {{- if or (le $defaultMaxBodyBytes 0) (gt $defaultMaxBodyBytes 1048576) }}
      {{- fail "controller.config.templatingSettings.extraContext.waf.policies.requestBody.defaultMaxBytes must be between 1 and 1048576 bytes." }}
    {{- end }}
    {{- if or (le $maxBodyBytes 0) (gt $maxBodyBytes 1048576) }}
      {{- fail "controller.config.templatingSettings.extraContext.waf.policies.requestBody.maxBytes must be between 1 and 1048576 bytes." }}
    {{- end }}
    {{- if gt $defaultMaxBodyBytes $maxBodyBytes }}
      {{- fail "controller.config.templatingSettings.extraContext.waf.policies.requestBody.defaultMaxBytes must not exceed requestBody.maxBytes." }}
    {{- end }}
    {{- if gt $maxBodyBytes (sub $requestBodyBufferSize $requestBodyBufferReservedBytes) }}
      {{- fail "waf.policies.requestBody.maxBytes must not exceed requestBodyInspection.haproxyBuffer.sizeBytes minus requestBodyInspection.haproxyBuffer.reservedBytes, so the WAF never receives a truncated HAProxy body." }}
    {{- end }}
    {{- $requestBodyWaitTimeout := (dig "waitTimeout" "100ms" $policyRequestBody | toString) }}
    {{- if or (not (regexMatch "^[0-9]+(us|ms|s|m|h|d)?$" $requestBodyWaitTimeout)) (le (int (regexFind "^[0-9]+" $requestBodyWaitTimeout)) 0) }}
      {{- fail "controller.config.templatingSettings.extraContext.waf.policies.requestBody.waitTimeout must be a positive HAProxy duration using an optional us, ms, s, m, h, or d suffix." }}
    {{- end }}
    {{- $defaultPolicy := (dig "defaultPolicy" "" $waf | toString) }}
    {{- if and (ne $defaultPolicy "") (not (regexMatch "^[a-z0-9][a-z0-9._-]{0,62}$" $defaultPolicy)) }}
      {{- fail "controller.config.templatingSettings.extraContext.waf.policies.defaultPolicy must be empty or a valid exact policy name." }}
    {{- end }}
    {{- $allowPolicySelection := true }}
    {{- if hasKey $wafIngressPermissions "allowPolicySelection" }}{{- $allowPolicySelection = $wafIngressPermissions.allowPolicySelection }}{{- end }}
    {{- if and $wafPoliciesConfigured (not $allowPolicySelection) (eq $defaultPolicy "") }}
      {{- fail "waf.policies.defaultPolicy is required when waf.ingressPermissions.allowPolicySelection=false; otherwise no policy could be applied." }}
    {{- end }}
    {{- $limits := dict }}
    {{- if hasKey $waf "limits" }}
      {{- $limits = $waf.limits }}
    {{- end }}
    {{- $maxPoliciesRaw := dig "maxCount" 16 $limits | toString }}
    {{- $maxPolicySecLangBytesRaw := dig "maxSecLangBytes" 65536 $limits | toString }}
    {{- $maxPolicyRuleExclusionsRaw := dig "maxRuleExclusions" 256 $limits | toString }}
    {{- if not (regexMatch "^[0-9]+$" $maxPoliciesRaw) }}
      {{- fail "controller.config.templatingSettings.extraContext.waf.policies.limits.maxCount must be an integer between 1 and 1024." }}
    {{- end }}
    {{- if not (regexMatch "^[0-9]+$" $maxPolicySecLangBytesRaw) }}
      {{- fail "controller.config.templatingSettings.extraContext.waf.policies.limits.maxSecLangBytes must be an integer between 1 and 1048576." }}
    {{- end }}
    {{- if not (regexMatch "^[0-9]+$" $maxPolicyRuleExclusionsRaw) }}
      {{- fail "controller.config.templatingSettings.extraContext.waf.policies.limits.maxRuleExclusions must be an integer between 1 and 4096." }}
    {{- end }}
    {{- $maxPolicies := int $maxPoliciesRaw }}
    {{- $maxPolicySecLangBytes := int $maxPolicySecLangBytesRaw }}
    {{- if or (le $maxPolicies 0) (gt $maxPolicies 1024) }}
      {{- fail "controller.config.templatingSettings.extraContext.waf.policies.limits.maxCount must be between 1 and 1024." }}
    {{- end }}
    {{- if or (le $maxPolicySecLangBytes 0) (gt $maxPolicySecLangBytes 1048576) }}
      {{- fail "controller.config.templatingSettings.extraContext.waf.policies.limits.maxSecLangBytes must be between 1 and 1048576." }}
    {{- end }}
    {{- $maxPolicyRuleExclusions := int $maxPolicyRuleExclusionsRaw }}
    {{- if or (le $maxPolicyRuleExclusions 0) (gt $maxPolicyRuleExclusions 4096) }}
      {{- fail "controller.config.templatingSettings.extraContext.waf.policies.limits.maxRuleExclusions must be between 1 and 4096." }}
    {{- end }}
    {{- $policies := dict }}
    {{- /* Keep in sync with registerPolicy's allowedPolicyFields in
           charts/haptic-annotations/83-waf-policies.yaml — the Scriggo scan
           is the authoritative validator; this pre-check only exists so a
           bad values file fails at `helm template` time instead of at CR
           admission. The values-surface smoke render in
           scripts/test-templates.sh exercises every field listed here, so
           adding a field in one place and not the other fails CI. */}}
    {{- $allowedPolicyFields := list "description" "requestBody" "enforcement" "secLang" "allowedMethods" "paranoiaLevel" "anomalyThreshold" "ruleExclusions" }}
    {{- $inlinePolicies := dict }}
    {{- if hasKey $waf "inline" }}{{- $inlinePolicies = $waf.inline }}{{- end }}
    {{- range $name, $policy := $inlinePolicies }}
      {{- if not (regexMatch "^[a-z0-9][a-z0-9._-]{0,62}$" $name) }}
        {{- fail (printf "waf.policies.inline name %q must contain 1-63 lowercase letters, digits, dots, underscores, or hyphens and start with a letter or digit." $name) }}
      {{- end }}
      {{- if not (kindIs "map" $policy) }}
        {{- fail (printf "waf.policies.inline.%s must be a map." $name) }}
      {{- end }}
      {{- range $field := keys $policy }}
        {{- if not (has $field $allowedPolicyFields) }}
          {{- fail (printf "waf.policies.inline.%s contains unknown field %q. Valid fields: %s." $name $field (join ", " $allowedPolicyFields)) }}
        {{- end }}
      {{- end }}
      {{- if and (hasKey $policy "description") (not (kindIs "string" $policy.description)) }}
        {{- fail (printf "waf.policies.inline.%s.description must be a string." $name) }}
      {{- end }}
      {{- if gt (len ($policy.description | default "")) 512 }}
        {{- fail (printf "waf.policies.inline.%s.description must not exceed 512 bytes." $name) }}
      {{- end }}
      {{- if and (hasKey $policy "secLang") (not (kindIs "string" $policy.secLang)) }}
        {{- fail (printf "waf.policies.inline.%s.secLang must be a string." $name) }}
      {{- end }}
      {{- if hasKey $policy "allowedMethods" }}
        {{- if not (kindIs "slice" $policy.allowedMethods) }}
          {{- fail (printf "waf.policies.inline.%s.allowedMethods must be a non-empty list of HTTP methods." $name) }}
        {{- end }}
        {{- if eq (len $policy.allowedMethods) 0 }}
          {{- fail (printf "waf.policies.inline.%s.allowedMethods must be a non-empty list of HTTP methods." $name) }}
        {{- end }}
        {{- range $method := $policy.allowedMethods }}
          {{- if not (regexMatch "^[A-Z][A-Z-]{1,15}$" (toString $method)) }}
            {{- fail (printf "waf.policies.inline.%s has invalid allowedMethods entry %q. Methods must be uppercase HTTP method tokens such as GET or PATCH." $name (toString $method)) }}
          {{- end }}
        {{- end }}
      {{- end }}
      {{- if hasKey $policy "paranoiaLevel" }}
        {{- if not (regexMatch "^[1-4]$" (toString $policy.paranoiaLevel)) }}
          {{- fail (printf "waf.policies.inline.%s.paranoiaLevel must be an integer between 1 and 4." $name) }}
        {{- end }}
      {{- end }}
      {{- if hasKey $policy "anomalyThreshold" }}
        {{- if not (kindIs "map" $policy.anomalyThreshold) }}
          {{- fail (printf "waf.policies.inline.%s.anomalyThreshold must be a map with inbound and/or outbound integer fields." $name) }}
        {{- end }}
        {{- range $tfield, $tvalue := $policy.anomalyThreshold }}
          {{- if not (has $tfield (list "inbound" "outbound")) }}
            {{- fail (printf "waf.policies.inline.%s.anomalyThreshold contains unknown field %q. Valid fields: inbound, outbound." $name $tfield) }}
          {{- end }}
          {{- if not (regexMatch "^[1-9][0-9]{0,4}$" (toString $tvalue)) }}
            {{- fail (printf "waf.policies.inline.%s.anomalyThreshold.%s must be a positive integer." $name $tfield) }}
          {{- end }}
        {{- end }}
      {{- end }}
      {{- /* Shape-check ruleExclusions here so a bad values file fails at
             `helm template` time; the Scriggo registerPolicy scan is the
             authoritative validator (denylist ranges, target regex, path
             operators). Keep the field set in sync with it. */}}
      {{- if hasKey $policy "ruleExclusions" }}
        {{- if not (kindIs "slice" $policy.ruleExclusions) }}
          {{- fail (printf "waf.policies.inline.%s.ruleExclusions must be a list of exclusion entries." $name) }}
        {{- end }}
        {{- range $entry := $policy.ruleExclusions }}
          {{- if not (kindIs "map" $entry) }}
            {{- fail (printf "waf.policies.inline.%s.ruleExclusions entries must be maps." $name) }}
          {{- end }}
          {{- range $efield := keys $entry }}
            {{- if not (has $efield (list "rules" "tags" "excludeTarget" "onPathPrefix" "onPathContains" "onPathExact" "onPathSuffix")) }}
              {{- fail (printf "waf.policies.inline.%s.ruleExclusions entry contains unknown field %q. Valid fields: rules, tags, excludeTarget, onPathPrefix, onPathContains, onPathExact, onPathSuffix." $name $efield) }}
            {{- end }}
          {{- end }}
          {{- if and (not (hasKey $entry "rules")) (not (hasKey $entry "tags")) }}
            {{- fail (printf "waf.policies.inline.%s.ruleExclusions entry must set 'rules' and/or 'tags'." $name) }}
          {{- end }}
          {{- if and (hasKey $entry "rules") (not (kindIs "slice" $entry.rules)) }}
            {{- fail (printf "waf.policies.inline.%s.ruleExclusions 'rules' must be a list of CRS rule IDs." $name) }}
          {{- end }}
          {{- if and (hasKey $entry "tags") (not (kindIs "slice" $entry.tags)) }}
            {{- fail (printf "waf.policies.inline.%s.ruleExclusions 'tags' must be a list of CRS rule tags." $name) }}
          {{- end }}
        {{- end }}
      {{- end }}
      {{- $requestBody := dict }}
      {{- if hasKey $policy "requestBody" }}
        {{- if not (kindIs "map" $policy.requestBody) }}
          {{- fail (printf "waf.policies.inline.%s.requestBody must be a map with mode and optional maxBytes fields." $name) }}
        {{- end }}
        {{- $requestBody = $policy.requestBody }}
      {{- end }}
      {{- if not (kindIs "map" $requestBody) }}
        {{- fail (printf "waf.policies.inline.%s.requestBody must be a map with mode and optional maxBytes fields." $name) }}
      {{- end }}
      {{- range $field := keys $requestBody }}
        {{- if not (has $field (list "mode" "maxBytes")) }}
          {{- fail (printf "waf.policies.inline.%s.requestBody contains unknown field %q. Valid fields: mode, maxBytes." $name $field) }}
        {{- end }}
      {{- end }}
      {{- $requestBodyMode := "none" }}
      {{- if hasKey $requestBody "mode" }}{{- $requestBodyMode = $requestBody.mode | toString }}{{- end }}
      {{- if not (has $requestBodyMode (list "none" "any" "json")) }}
        {{- fail (printf "waf.policies.inline.%s.requestBody.mode must be one of: none, any, json." $name) }}
      {{- end }}
      {{- if and (eq $requestBodyMode "none") (hasKey $requestBody "maxBytes") }}
        {{- fail (printf "waf.policies.inline.%s.requestBody.maxBytes requires requestBody.mode to be any or json; mode none does not inspect request bodies." $name) }}
      {{- end }}
      {{- $enforcement := "deny" }}
      {{- if hasKey $policy "enforcement" }}{{- $enforcement = $policy.enforcement | toString }}{{- end }}
      {{- if not (has $enforcement (list "deny" "detect")) }}
        {{- fail (printf "waf.policies.inline.%s.enforcement must be one of: deny, detect." $name) }}
      {{- end }}
      {{- $secLang := ($policy.secLang | default "" | toString) }}
      {{- if contains "\"\"\"" $secLang }}
        {{- fail (printf "waf.policies.inline.%s.secLang must not contain a TOML triple-quote delimiter." $name) }}
      {{- end }}
      {{- if gt (len $secLang) $maxPolicySecLangBytes }}
        {{- fail (printf "waf.policies.inline.%s.secLang is %d bytes; the configured limit is %d." $name (len $secLang) $maxPolicySecLangBytes) }}
      {{- end }}
      {{- $policyMaxBodyBytesRaw := $defaultMaxBodyBytes | toString }}
      {{- if hasKey $requestBody "maxBytes" }}{{- $policyMaxBodyBytesRaw = $requestBody.maxBytes | toString }}{{- end }}
      {{- if not (regexMatch "^[0-9]+$" $policyMaxBodyBytesRaw) }}
        {{- fail (printf "waf.policies.inline.%s.requestBody.maxBytes must be an integer between 1 and waf.policies.requestBody.maxBytes (%d)." $name $maxBodyBytes) }}
      {{- end }}
      {{- $policyMaxBodyBytes := int $policyMaxBodyBytesRaw }}
      {{- if or (le $policyMaxBodyBytes 0) (gt $policyMaxBodyBytes $maxBodyBytes) }}
        {{- fail (printf "waf.policies.inline.%s.requestBody.maxBytes must be between 1 and waf.policies.requestBody.maxBytes (%d)." $name $maxBodyBytes) }}
      {{- end }}
      {{- /* This dict feeds only the maxCount check below and the writeback
             at the end of the block; the emitted CR carries the raw values
             ($config is deep-copied before this runs), and Scriggo's
             registerPolicy is the authoritative normalizer. Overlay the
             resolved defaults onto a copy of the original so the structured
             fields the Scriggo scan owns (allowedMethods, paranoiaLevel,
             anomalyThreshold, ruleExclusions) survive rather than being
             dropped by a hand-listed key set. deepCopy keeps the source
             $policy pristine for the outer range. */}}
      {{- $normalized := mergeOverwrite (deepCopy $policy) (dict
          "description" ($policy.description | default "" | toString)
          "requestBody" (dict "mode" $requestBodyMode "maxBytes" ($policyMaxBodyBytes | toString))
          "enforcement" $enforcement
          "secLang" $secLang
      ) }}
      {{- $_ := set $policies $name $normalized }}
    {{- end }}
    {{- if gt (len $policies) $maxPolicies }}
      {{- fail (printf "waf.policies.inline defines %d policies; the configured limit is %d." (len $policies) $maxPolicies) }}
    {{- end }}
    {{- /* Keyed by catalog name, matching its sibling `inline`: Helm replaces a
           list wholesale, so an operator adding one catalog would drop every one
           the chart ships. Presence is the enable, again like `inline` — a
           second flag could leave a configured catalog inert. */ -}}
    {{- $configuredPolicyConfigMapRefs := dict }}
    {{- if hasKey $waf "configMapRefs" }}{{- $configuredPolicyConfigMapRefs = $waf.configMapRefs }}{{- end }}
    {{- $policyConfigMapRefs := dict }}
    {{- $seenPolicyConfigMapRefs := dict }}
    {{- range $refName := (keys $configuredPolicyConfigMapRefs | sortAlpha) }}
      {{- $source := get $configuredPolicyConfigMapRefs $refName }}
      {{- $namespace := ($source.namespace | default $.Release.Namespace | toString) }}
      {{- $name := ($source.name | default "" | toString) }}
      {{- $key := ($source.key | default "policies.yaml" | toString) }}
      {{- if not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $namespace) }}
        {{- fail (printf "waf.policies.configMapRefs.%s.namespace must be a valid Kubernetes namespace." $refName) }}
      {{- end }}
      {{- if not (regexMatch "^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$" $name) }}
        {{- fail (printf "waf.policies.configMapRefs.%s.name must be a valid ConfigMap name." $refName) }}
      {{- end }}
      {{- if not (regexMatch "^[-._A-Za-z0-9]+$" $key) }}
        {{- fail (printf "waf.policies.configMapRefs.%s.key must be a valid ConfigMap data key." $refName) }}
      {{- end }}
      {{- $identity := printf "%s/%s:%s" $namespace $name $key }}
      {{- if hasKey $seenPolicyConfigMapRefs $identity }}
        {{- fail (printf "waf.policies.configMapRefs.%s and .%s both point at %s. Two catalogs cannot share a source." (get $seenPolicyConfigMapRefs $identity) $refName $identity) }}
      {{- end }}
      {{- $_ := set $seenPolicyConfigMapRefs $identity $refName }}
      {{- $_ := set $policyConfigMapRefs $refName (dict "namespace" $namespace "name" $name "key" $key) }}
    {{- end }}
    {{- $_ := set $waf "inline" $policies }}
    {{- $_ := set $waf "configMapRefs" $policyConfigMapRefs }}
  {{- end }}

  {{- /* Controller's own namespace + HAProxy pod selector. Templates that
         emit owned resources via renderResource() (e.g. per-Gateway
         LoadBalancer Services for SupportGatewayStaticAddresses) need
         both: the namespace because the resourceapplier refuses to
         apply outside the controller's namespace by default, and the
         selector so the spawned Service routes traffic to the shared
         HAProxy pods. We compute these here from chart values once
         rather than asking Scriggo to reach for them. */ -}}
  {{- $_ := set $extraContext "controllerNamespace" .Release.Namespace }}
  {{- $_ := set $extraContext "haproxyPodSelector" (include "haptic.haproxy.selectorLabels" . | fromYaml) }}
  {{- /* Resolved name of the chart-emitted HAProxy Service. Templates
         that partial-patch its spec.ports (e.g. the gateway-listener-
         ports snippet) need this so the SSA payload targets the same
         object the chart created. The controller never hardcodes the
         name — the chart computes it via fullname conventions and
         hands it through extraContext. */ -}}
  {{- $_ := set $extraContext "haproxyServiceName" (include "haptic.haproxyFullname" .) }}

  {{- /* Plumb the chart's HAProxy bind-port values through extraContext
         so the base/ssl/gateway libraries' bind directives stay in
         lockstep with `haproxy.ports`. The fallbacks in those libraries
         (8080/8443) only apply when the chart hasn't provided values,
         so chart-managed installs always emit binds that match the
         Service targetPort / containerPort wiring. Defaulting these to
         the literal Gateway listener-port numbers (80/443) keeps each
         Gateway listener port unambiguous on its own bind — see
         comment in values.yaml on `haproxy.ports.http` for the full
         rationale. */ -}}
  {{- $_ := set $extraContext "httpPort" (.Values.haproxy.ports.http | int) }}
  {{- $_ := set $extraContext "httpsPort" (.Values.haproxy.ports.https | int) }}

  {{- /* Plumb the chart's haproxy.service.* values through extraContext so
         the k8sResources.haproxy-service template (in libraries/base.yaml)
         can render the user-facing LoadBalancer Service from listener
         state. This replaces the chart-static templates/haproxy-service.yaml
         main-Service block: the controller now owns the Service end-to-end
         and emits an OwnerReference pointing at the HAProxyTemplateConfig
         CR, so `helm uninstall` cascade-GCs it. The bag is intentionally
         flat so the template can read fields without re-walking
         values.yaml. */ -}}
  {{- if $haproxy := .Values.haproxy }}
  {{- if $haproxy.enabled }}
  {{- $svc := $haproxy.service }}
  {{- $svcLabels := dict }}
  {{- range $k, $v := (include "haptic.labels" . | fromYaml) }}
    {{- $_ := set $svcLabels $k $v }}
  {{- end }}
  {{- $_ := set $svcLabels "app.kubernetes.io/component" "loadbalancer" }}
  {{- $svcSelectorLabels := dict }}
  {{- range $k, $v := (include "haptic.selectorLabels" . | fromYaml) }}
    {{- $_ := set $svcSelectorLabels $k $v }}
  {{- end }}
  {{- $_ := set $svcSelectorLabels "app.kubernetes.io/component" "loadbalancer" }}
  {{- /* Build a single Service-port list with chart-static defaults
         (http / https / stats) and any operator-declared extras
         appended. The k8sResources.haproxy-service template just
         iterates this list — there is no static / dynamic split on
         the Scriggo side. Operators can:
           * drop a default entry by overriding the matching
             `haproxy.service.{http,https,stats}` block with
             `port: 0` (the build below skips zero-port entries)
           * change a default port number via the existing
             `haproxy.service.{http,https,stats}.port` keys
           * append additional ports via
             `haproxy.service.extraPorts` (one entry per extra). */ -}}
  {{- $defaultPorts := list }}
  {{- range $name := list "http" "https" "stats" }}
    {{- $entry := index $svc $name }}
    {{- if $entry }}
      {{- $portNum := int ($entry.port | default 0) }}
      {{- if gt $portNum 0 }}
        {{- $portDict := dict "name" $name "port" $portNum "targetPort" $name "protocol" "TCP" }}
        {{- with ($entry.nodePort | default 0) }}
          {{- $_ := set $portDict "nodePort" (int .) }}
        {{- end }}
        {{- $defaultPorts = append $defaultPorts $portDict }}
        {{- if and (eq $name "https") $http3.enabled }}
          {{- /* Same nodePort as TCP: Kubernetes keys nodePort uniqueness by protocol. */ -}}
          {{- $quicDict := dict "name" "https-quic" "port" $portNum "targetPort" "https-quic" "protocol" "UDP" }}
          {{- with ($entry.nodePort | default 0) }}
            {{- $_ := set $quicDict "nodePort" (int .) }}
          {{- end }}
          {{- $defaultPorts = append $defaultPorts $quicDict }}
        {{- end }}
      {{- end }}
    {{- end }}
  {{- end }}
  {{- /* PROXY-protocol listeners are additional ports on the same pods, so they
         ride the same Service. targetPort is the container-port NAME, matching
         the http/https/stats entries above. */ -}}
  {{- if $proxyProtocol.enabled }}
    {{- $defaultPorts = append $defaultPorts (dict "name" "proxy-http" "port" (int $proxyProtocol.httpPort) "targetPort" "proxy-http" "protocol" "TCP") }}
    {{- $defaultPorts = append $defaultPorts (dict "name" "proxy-https" "port" (int $proxyProtocol.httpsPort) "targetPort" "proxy-https" "protocol" "TCP") }}
  {{- end }}
  {{- $allPorts := concat $defaultPorts ($svc.extraPorts | default list) }}
  {{- $haproxyService := dict
      "type" $svc.type
      "labels" $svcLabels
      "selector" $svcSelectorLabels
      "annotations" ($svc.annotations | default dict)
      "loadBalancerIP" ($svc.loadBalancerIP | default "")
      "loadBalancerClass" ($svc.loadBalancerClass | default "")
      "loadBalancerSourceRanges" ($svc.loadBalancerSourceRanges | default list)
      "externalTrafficPolicy" ($svc.externalTrafficPolicy | default "")
      "internalTrafficPolicy" ($svc.internalTrafficPolicy | default "")
      "healthCheckNodePort" (int ($svc.healthCheckNodePort | default 0))
      "publishNotReadyAddresses" ($svc.publishNotReadyAddresses | default false)
      "ports" $allPorts
  }}
  {{- $_ := set $extraContext "haproxyService" $haproxyService }}
  {{- end }}
  {{- end }}

  {{- /* Pass spoaHub wiring through extraContext when the sidecar is
         enabled AND at least one plugin contributes an SPOE message.
         The spoa-hub template library reads this to render spoe.conf
         and the spoa-hub backend block. We compute the union of
         enabled-plugin messages once here rather than asking Scriggo
         to recompute it on every render. The controller stays generic
         — it never learns the word "spoaHub".

         An empty messages list is rejected below rather than left
         dormant. HAProxy does reject an spoe-agent with no `messages`,
         so the library must not emit one — but staying silent is not
         safe either: `haptic.spoaHub.enabled` still deploys the sidecar
         and the bootstrap initContainer still seeds
         /etc/haproxy/general/spoa-hub-config.toml, while the library
         renders no runtime TOML. The controller's general-file sync
         then sees a file it did not render and orphan-deletes it out
         from under the running hub, and the auto-wired `spoa-hub`
         validator entry below points at a file that never exists. The
         combination is unusable in every direction, so fail at the
         gate instead of shipping it. */ -}}
  {{- if include "haptic.spoaHub.enabled" . }}
    {{- $messages := list }}
    {{- $pluginEntries := list }}
    {{- $messageTimeoutSums := dict }}
    {{- $maxMessageTimeoutMs := 0 }}
    {{- range $name, $plugin := .Values.spoaHub.plugins }}
      {{- if include "haptic.spoaHub.pluginEnabled" (dict "plugin" $plugin "root" $ "name" $name) }}
        {{- $messages = concat $messages $plugin.messages }}
        {{- $pluginTimeoutMs := int (include "haptic.spoaHub.pluginTimeoutMs" (dict "plugin" $plugin "root" $ "name" $name)) }}
        {{- range $message := uniq $plugin.messages }}
          {{- $messageTimeoutMs := add (int (get $messageTimeoutSums $message | default 0)) $pluginTimeoutMs }}
          {{- $_ := set $messageTimeoutSums $message $messageTimeoutMs }}
          {{- if gt $messageTimeoutMs $maxMessageTimeoutMs }}
            {{- $maxMessageTimeoutMs = $messageTimeoutMs }}
          {{- end }}
        {{- end }}
        {{- /* Plugin name in HAProxy variable namespace must be a valid identifier;
               TOML/YAML allow kebab-case but HAProxy var lookups don't. Sanitize
               dashes to underscores to match what the rendered TOML and the
               chart's bootstrap configmap use. */}}
        {{- $normalizedDependencies := list }}
        {{- range ($plugin.dependsOn | default list) }}
          {{- $normalizedDependencies = append $normalizedDependencies (regexReplaceAll "-" . "_") }}
        {{- end }}
        {{- $entry := dict
            "name" (regexReplaceAll "-" $name "_")
            "library" (include "haptic.spoaHub.libName" (dict "name" $name))
            "messages" $plugin.messages
            "timeoutMs" $pluginTimeoutMs
            "dependsOn" $normalizedDependencies
            "params" (include "haptic.spoaHub.effectivePluginParams" (dict "root" $ "name" $name "plugin" $plugin))
        }}
        {{- if hasKey $plugin "maxConcurrency" }}
          {{- $_ := set $entry "maxConcurrency" (int (include "haptic.spoaHub.pluginInteger" (dict "plugin" $plugin "root" $ "name" $name "field" "maxConcurrency" "min" 1))) }}
        {{- end }}
        {{- if hasKey $plugin "maxQueue" }}
          {{- $_ := set $entry "maxQueue" (int (include "haptic.spoaHub.pluginInteger" (dict "plugin" $plugin "root" $ "name" $name "field" "maxQueue" "min" 0))) }}
        {{- end }}
        {{- if hasKey $plugin "queueTimeoutMs" }}
          {{- $_ := set $entry "queueTimeoutMs" (int (include "haptic.spoaHub.pluginInteger" (dict "plugin" $plugin "root" $ "name" $name "field" "queueTimeoutMs" "min" 1))) }}
        {{- end }}
        {{- /* Only surface adaptiveConcurrency when true: false is the hub
               default, so omitting it keeps the rendered config byte-identical
               for the common case and never emits a field an older hub (before
               adaptive support) would have to tolerate. */}}
        {{- if hasKey $plugin "adaptiveConcurrency" }}
          {{- if $plugin.adaptiveConcurrency }}
            {{- $_ := set $entry "adaptiveConcurrency" true }}
          {{- end }}
        {{- end }}
        {{- /* Coraza-specific: surface the directives field separately so
               the runtime render snippet can append per-Ingress
               `nginx.ingress.kubernetes.io/modsecurity-snippet` values
               into it. Other plugins splice `params` verbatim. */}}
        {{- if eq $name "coraza" }}
          {{- $_ := set $entry "directives" ($plugin.directives | default "") }}
        {{- end }}
        {{- $pluginEntries = append $pluginEntries $entry }}
      {{- end }}
    {{- end }}
    {{- if not (uniq $messages) }}
      {{- fail "spoaHub is enabled but no enabled plugin contributes an SPOE message, so the sidecar would be deployed with nothing to serve: HAProxy would get no spoe-agent, and the controller would orphan-delete the sidecar's bootstrap config from general storage. Enable at least one plugin under spoaHub.plugins.* (each supplies its own messages), or leave spoaHub.enabled unset so the sidecar is only deployed when a plugin turns it on." }}
    {{- end }}
    {{- with uniq $messages }}
      {{- $hub := $.Values.spoaHub.haproxy }}
      {{- $hubRuntime := $.Values.spoaHub.hub }}
      {{- $hubMaxConnections := int (include "haptic.spoaHub.hubInteger" (dict "root" $ "field" "maxConnections" "min" 1)) }}
      {{- $hubBlockingThreadKeepAliveSecs := int (include "haptic.spoaHub.hubInteger" (dict "root" $ "field" "blockingThreadKeepAliveSecs" "min" 1)) }}
      {{- $hubMaxBlockingThreads := $hubRuntime.maxBlockingThreads }}
      {{- if ne $hubMaxBlockingThreads nil }}
        {{- $hubMaxBlockingThreads = int (include "haptic.spoaHub.hubInteger" (dict "root" $ "field" "maxBlockingThreads" "min" 1)) }}
      {{- end }}
      {{- $hubReloadDrainTimeoutMs := $hubRuntime.reloadDrainTimeoutMs }}
      {{- if ne $hubReloadDrainTimeoutMs nil }}
        {{- $hubReloadDrainTimeoutMs = int (include "haptic.spoaHub.hubInteger" (dict "root" $ "field" "reloadDrainTimeoutMs" "min" 0)) }}
      {{- end }}
      {{- $hubWorkerThreads := $hubRuntime.workerThreads }}
      {{- if ne $hubWorkerThreads nil }}
        {{- $hubWorkerThreads = int (include "haptic.spoaHub.hubInteger" (dict "root" $ "field" "workerThreads" "min" 1)) }}
      {{- end }}
      {{- $processingMarginMs := int $hub.timeoutProcessingMarginMs }}
      {{- if or (le $processingMarginMs 0) (gt $processingMarginMs 60000) }}
        {{- fail "spoaHub.haproxy.timeoutProcessingMarginMs must be between 1 and 60000 milliseconds." }}
      {{- end }}
      {{- $minimumProcessingUs := mul (add $maxMessageTimeoutMs $processingMarginMs) 1000 }}
      {{- $deriveMessageTimeouts := eq $hub.timeoutProcessing nil }}
      {{- $timeoutProcessing := $hub.timeoutProcessing }}
      {{- if eq $timeoutProcessing nil }}
        {{- $timeoutProcessing = printf "%dms" (add $maxMessageTimeoutMs $processingMarginMs) }}
      {{- else }}
        {{- $timeoutProcessing = toString $timeoutProcessing }}
        {{- if not (regexMatch "^[0-9]+(us|ms|s|m|h|d)?$" $timeoutProcessing) }}
          {{- fail "spoaHub.haproxy.timeoutProcessing must be null or a positive HAProxy duration using an optional us, ms, s, m, h, or d suffix." }}
        {{- end }}
        {{- $durationValue := int64 (regexFind "^[0-9]+" $timeoutProcessing) }}
        {{- if le $durationValue 0 }}
          {{- fail "spoaHub.haproxy.timeoutProcessing must be null or a positive HAProxy duration using an optional us, ms, s, m, h, or d suffix." }}
        {{- end }}
        {{- $durationUnit := regexFind "(us|ms|s|m|h|d)$" $timeoutProcessing }}
        {{- $durationFactorUs := 1000 }}
        {{- if eq $durationUnit "us" }}{{- $durationFactorUs = 1 }}{{- end }}
        {{- if eq $durationUnit "s" }}{{- $durationFactorUs = 1000000 }}{{- end }}
        {{- if eq $durationUnit "m" }}{{- $durationFactorUs = 60000000 }}{{- end }}
        {{- if eq $durationUnit "h" }}{{- $durationFactorUs = 3600000000 }}{{- end }}
        {{- if eq $durationUnit "d" }}{{- $durationFactorUs = 86400000000 }}{{- end }}
        {{- if lt (mul $durationValue $durationFactorUs) $minimumProcessingUs }}
          {{- fail (printf "spoaHub.haproxy.timeoutProcessing (%s) must be at least the largest enabled SPOE message budget (%dms) plus timeoutProcessingMarginMs (%dms). Lower the relevant plugin timeouts or raise the HAProxy processing timeout." $timeoutProcessing $maxMessageTimeoutMs $processingMarginMs) }}
        {{- end }}
      {{- end }}
      {{- $messageTimeouts := dict }}
      {{- range $message, $budgetMs := $messageTimeoutSums }}
        {{- $messageTimeout := $timeoutProcessing }}
        {{- if $deriveMessageTimeouts }}
          {{- $messageTimeout = printf "%dms" (add (int $budgetMs) $processingMarginMs) }}
        {{- end }}
        {{- $_ := set $messageTimeouts $message $messageTimeout }}
      {{- end }}
      {{- $mirrorExtra := dict }}
      {{- if hasKey $.Values.spoaHub.plugins "mirror" }}
        {{- $mirrorPlugin := get $.Values.spoaHub.plugins "mirror" }}
        {{- $_ := set $mirrorExtra "targetTimeoutMs" (int (include "haptic.spoaHub.pluginInteger" (dict "plugin" $mirrorPlugin "root" $ "name" "mirror" "field" "targetTimeoutMs" "min" 1 "max" 60000))) }}
        {{- $_ := set $mirrorExtra "targetRetries" (int (include "haptic.spoaHub.pluginInteger" (dict "plugin" $mirrorPlugin "root" $ "name" "mirror" "field" "targetRetries" "min" 0 "max" 10))) }}
      {{- end }}
      {{- $spoaHubExtra := dict
          "enabled" true
          "haproxy" (dict
              "socketPath" $hub.socketPath
              "modeSpop" $hub.modeSpop
              "messages" .
              "messageTimeouts" $messageTimeouts
              "timeoutHello" (toString $hub.timeoutHello)
              "timeoutIdle" (toString $hub.timeoutIdle)
              "timeoutProcessing" $timeoutProcessing
              "poolMaxConn" (int $hub.poolMaxConn)
              "poolPurgeDelay" (toString $hub.poolPurgeDelay)
          )
          "hub" (dict
              "logLevel" $hubRuntime.logLevel
              "maxConnections" $hubMaxConnections
              "blockingThreadKeepAliveSecs" $hubBlockingThreadKeepAliveSecs
              "maxBlockingThreads" $hubMaxBlockingThreads
              "reloadDrainTimeoutMs" $hubReloadDrainTimeoutMs
              "metricsAddr" (include "haptic.spoaHub.metricsAddrEffective" $)
              "workerThreads" $hubWorkerThreads
          )
          "plugins" $pluginEntries
      }}
      {{- if $mirrorExtra }}{{- $_ := set $spoaHubExtra "mirror" $mirrorExtra }}{{- end }}
      {{- $_ := set $extraContext "spoaHub" $spoaHubExtra }}
    {{- end }}
  {{- end }}

  {{- /* shm-stats-file settings (when enabled) */ -}}
  {{- $shmStats := .Values.haproxy.shmStats }}
  {{- if $shmStats.enabled }}
    {{- $_ := set $extraContext "shmStatsEnabled" true }}
    {{- $_ := set $extraContext "shmStatsPath" $shmStats.path }}
    {{- $_ := set $extraContext "shmStatsMaxObjects" ($shmStats.maxObjects | int) }}
  {{- end }}

  {{- /* nbthread for controller-rendered HAProxy config */ -}}
  {{- with include "haptic.haproxy.nbthread" . }}
    {{- $_ := set $extraContext "nbthread" (. | int) }}
  {{- end }}
{{- toYaml $extraContext }}
{{- end }}

{{- /* The values validationTests render from: the chart defaults, except what
       selects which libraries and tests exist (every value _helm_load and
       _helm_skip_test read), which follows the deployment. */ -}}
{{- define "haptic.testValues" -}}
{{- $helpers := index .Subcharts "helm-helpers" }}
{{- $values := $helpers.Files.Get "files/default-values.json" | fromJson }}
{{- $_ := set $values "haproxyVersion" .Values.haproxyVersion }}
{{- $_ := set $values.controller "templateLibraries" (deepCopy .Values.controller.templateLibraries) }}
{{- $_ := set $values.spoaHub "enabled" (eq (include "haptic.spoaHub.enabled" .) "true") }}
{{- range $name, $plugin := $values.spoaHub.plugins }}
  {{- $deployed := dict "plugin" (index ($.Values.spoaHub.plugins | default dict) $name) "root" $ "name" $name }}
  {{- $_ := set $plugin "enabled" (eq (include "haptic.spoaHub.pluginEnabled" $deployed) "true") }}
{{- end }}
{{- $_ := set $values.rateLimit.shared "enabled" .Values.rateLimit.shared.enabled }}
{{- $_ := set $values.cache.varnish "enabled" .Values.cache.varnish.enabled }}
{{- $_ := set $values.vector "enabled" .Values.vector.enabled }}
{{- $_ := set $values.vector.requestMetrics "enabled" .Values.vector.requestMetrics.enabled }}
{{- $_ := set $values.vector.requestMetrics "pathLabel" .Values.vector.requestMetrics.pathLabel }}
{{- $_ := set $values.vector.requestMetrics.metrics "request_size" (dig "requestMetrics" "metrics" "request_size" false .Values.vector) }}
{{- $deployedContext := dig "templatingSettings" "extraContext" dict (.Values.controller.config | default dict) }}
{{- $testContext := $values.controller.config.templatingSettings.extraContext }}
{{- $_ := set $testContext.tracing "enabled" (dig "tracing" "enabled" false $deployedContext) }}
{{- $_ := set $testContext.tracing.otlp "endpoint" (dig "tracing" "otlp" "endpoint" "" $deployedContext) }}
{{- $_ := set $testContext.apiGateway.requestSchemaValidation "enabled" (dig "apiGateway" "requestSchemaValidation" "enabled" false $deployedContext) }}
{{- toJson $values }}
{{- end }}
