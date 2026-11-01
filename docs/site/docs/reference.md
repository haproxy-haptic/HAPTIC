# Chart values reference

Look up a Helm value's type, default, and behavior. Put these values in the file
you pass to `helm install` or `helm upgrade`; see [Deploying with Helm](deploying-with-helm.md)
for complete commands.

The chart's values schema rejects unknown keys and values of the wrong type, so a
misspelled key fails the install instead of being ignored. Free-form maps, such
as labels, annotations, `resources`, and the top level of
`controller.config.templatingSettings.extraContext`, accept any key.

## CRD lifecycle

Helm installs the CRDs in `crds/` once and never upgrades them on a subsequent
`helm upgrade`. This hook Job runs `haptic apply-crds` (server-side
apply) so additive CRD schema changes reach the cluster on install and upgrade.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `crds.upgradeJob.enabled` | bool | `true` | Apply the bundled CRDs before installation or upgrade. Disable only when another process installs and updates those CRDs. |
| `crds.upgradeJob.backoffLimit` | int | `2` | Job retry limit (the apply is idempotent, so retries are safe) |
| `crds.upgradeJob.activeDeadlineSeconds` | int | `300` | Job wall-clock deadline |
| `crds.upgradeJob.resources` | object | cpu `50m` / memory `64Mi`–`128Mi` | Resource requests and limits for the apply Job pod |
| `crds.upgradeJob.annotations` | map | `{}` | Extra annotations for the Job (merged with chart defaults) |
| `crds.upgradeJob.labels` | map | `{}` | Extra labels for the Job (merged with chart defaults) |

## Pre-rollout validation

Before installation or upgrade changes workloads and configuration, the chart
runs [preflight validation](operations/validate-before-deploy.md). Failed validation
blocks the release; existing HAProxy workers keep serving. CRD and certificate
hooks may already have run. Keep validation enabled in [GitOps](operations/gitops.md)
workflows too.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `preRolloutValidation.enabled` | bool | `true` | Run the validation gate. The controller image and chart must come from the same build; use matching artifacts if the version check fails |
| `preRolloutValidation.backoffLimit` | int | `1` | Job retry limit |
| `preRolloutValidation.activeDeadlineSeconds` | int | `600` | Maximum time for the validation Job, including schema discovery and the bundled tests. |
| `preRolloutValidation.resources` | object | cpu `200m` / memory `512Mi`–`1Gi` | Resource requests and limits for the validation Job pod |
| `preRolloutValidation.annotations` | map | `{}` | Extra annotations for the Job |
| `preRolloutValidation.labels` | map | `{}` | Extra labels for the Job |

<a id="deployment-image"></a>

## Deployment and images

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `controller.replicaCount` | int | `2` | Number of controller replicas (2+ recommended for HA with leader election) |
| `haproxyVersion` | string | `"3.4"` | HAProxy major.minor series: `3.0`, `3.2`, `3.3`, or `3.4`. Selects the controller release digest or tag suffix and, with `haproxyPatchVersions`, the HAProxy pod image tag |
| `haproxyPatchVersions` | map | See values.yaml | Tested Community Edition patch versions for each HAProxy series. |
| `haproxyEnterprisePatchVersions` | map | See values.yaml | Per-`haproxyVersion` enterprise revision pins (for example `"3.2": "3.2r1"`). Used when `haproxy.enterprise.enabled=true` |
| `controller.image.repository` | string | `ghcr.io/haproxy-haptic/haptic` | Controller image repository |
| `controller.image.pullPolicy` | string | `IfNotPresent` | Image pull policy |
| `controller.image.tag` | string | `""` | Explicit base tag renders `<tag>-haproxy<haproxyVersion>`. Empty selects the packaged release digest, or the chart `appVersion` tag in source and development charts without digest metadata |
| `controller.image.digest` | string | `""` | SHA-256 image digest (`sha256:` plus 64 lowercase hex digits); overrides `tag` and renders `<repository>@<digest>` for the controller, agent, and lifecycle hooks. Choose the image built for your `haproxyVersion` and chart version |
| `nameOverride` | string | `""` | Override chart name |
| `fullnameOverride` | string | `""` | Override full release name |
| `commonLabels` | map | `{}` | Labels added to every chart-rendered resource on top of the standard `app.kubernetes.io/*` set |
| `commonAnnotations` | map | `{}` | Annotations added to every chart-rendered resource that has an `annotations` block |
| `controller.deploymentAnnotations` | map | `{}` | Extra annotations on the controller Deployment. |
| `extraDeploy` | list or map | `{}` | Free-form Kubernetes resources to render alongside the chart. Each entry is rendered through `tpl` so it can reference chart values. Map form (keys → manifests) is convenient for composing across multiple values files |

Published release charts bundle multi-platform controller image digests for every
supported HAProxy series. Changing only `controller.image.repository` retains the
release digest; your mirror must preserve the image index bytes. An explicit
`controller.image.digest` takes precedence over an explicit `controller.image.tag`,
which takes precedence over the packaged digest. Invalid release metadata fails
rendering. The pre-rollout hook receives the same metadata and retains the
chart/image version check.

Source and development charts without release metadata use
`<repository>:<appVersion>-haproxy<haproxyVersion>`. Set an explicit tag or digest
when testing another build.

## Controller core

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `controller.configName` | string | `haptic-config` | Name of the HAProxyTemplateConfig object the controller watches |
| `controller.logLevel` | string | `INFO` | Initial controller log level (`LOG_LEVEL` env var) — see [Logging and templating](#logging-and-templating) for the runtime override |
| `controller.kubeClient.qps` | float | `-1` | API requests per second across all controller clients. `<= 0` disables client throttling and relies on Kubernetes API Priority and Fairness. |
| `controller.kubeClient.burst` | int | `0` | Client-side apiserver burst (`KUBE_CLIENT_BURST` env var); used only when `controller.kubeClient.qps > 0` (`0` means `2*qps`) |
| `controller.ports.healthz` | int | `8080` | Port for health probes and the loopback-only debug endpoints; also updates the Service and NetworkPolicy. |
| `controller.ports.metrics` | int | `9090` | Metrics port; also updates the Service and monitors. `0` disables metrics and requires all monitor resources to be disabled. |
| `controller.ports.webhook` | int | `9443` | Admission webhook HTTPS port |
| `controller.config.templatingSettings.extraContext.statusPatches.enabled` | bool | `true` | Publish addresses and conditions on managed Ingress and Gateway API resources. Disable address publication during migration if it would trigger a premature DNS cutover; this setting affects all managed routes. See [migration](migrating.md#control-the-dns-cutover). |

## Template libraries

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `controller.templateLibraries.base.enabled` | bool | `true` | Core HAProxy configuration. Disabling drops the `haproxyConfig` template the other libraries plug into; leave on unless you supply a complete replacement |
| `controller.templateLibraries.kubernetesBackends.enabled` | bool | `true` | Shared Kubernetes Service port and EndpointSlice backend resolution used by the bundled routing libraries |
| `controller.templateLibraries.kubernetesBackends.podReadinessGate.enabled` | bool | `false` | Set the `haproxy-haptic.org/load-balancer-ready` readiness gate on pods that list it; see [Roll out without dropped requests](operations/pod-readiness-gate.md) |
| `controller.templateLibraries.ssl.enabled` | bool | `true` | SSL/TLS and HTTPS frontend support |
| `controller.templateLibraries.ingress.enabled` | bool | `true` | Kubernetes Ingress resource support |
| `controller.templateLibraries.gateway.enabled` | bool | `true` | Gateway API support (HTTP, gRPC, TLS and TCP routes) |
| `controller.templateLibraries.gateway.experimentalChannel` | bool | `false` | Enable tests for experimental HTTPRoute fields such as `retry` and `sessionPersistence`. Set this only with the experimental Gateway API schemas installed. It doesn't enable route kinds or change how routes are rendered. |
| `controller.templateLibraries.ingressAnnotationsCompat.enabled` | bool | `true` | Shared helpers required by the Ingress annotation compatibility libraries. |
| `controller.templateLibraries.governance.enabled` | bool | `true` | Governance rule engine. Enforces the bundled safety rules and any additional rules in `controller.config.templatingSettings.extraContext.governance.rules` |
| `controller.templateLibraries.hapticAnnotations.enabled` | bool | `true` | `haproxy-haptic.org/*` — HAPTIC's native annotation vocabulary; the default vocabulary for new configurations |
| `controller.templateLibraries.haproxytech.enabled` | bool | `false` | `haproxy.org/*` annotation compatibility (haproxytech/kubernetes-ingress migration) — opt-in |
| `controller.templateLibraries.haproxyIngress.enabled` | bool | `false` | `haproxy-ingress.github.io/*` annotation compatibility (jcmoraisjr/haproxy-ingress migration) — opt-in |
| `controller.templateLibraries.nginxIngress.enabled` | bool | `false` | `nginx.ingress.kubernetes.io/*` annotation compatibility (ingress-nginx migration) — opt-in |
| `controller.templateLibraries.customCrdExample.enabled` | bool | `false` | Worked example of a resource-agnostic library for a custom `Route` CRD, for learning or exercising the reload-free author contract. Not for production routing (it watches a demo kind) — opt-in |
| `controller.templateLibraries.spoaHub.enabled` | bool | `false` | HAProxy-side Stream Processing Offload Agent (SPOA) hub wiring. Auto-loaded when the SPOA hub sidecar is rendered (any `spoaHub.plugins.*` enabled, or `spoaHub.enabled: true`); set this to `true` to force-load the library standalone |

## Shared response cache

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `cache.haproxy.hashBalanceFactor` | int | `150` | HAProxy bounded-load consistent hashing: cap any one Varnish shard's share to this factor of the mean (`0` disables) |
| `cache.haproxy.responseTimeoutMs` | int | `2000` | Inactivity timeout on the Varnish hop. A miss spends it waiting for application response headers. Before headers, a timeout or transport failure retries GET/HEAD directly; after delivery starts, HAProxy terminates an idle partial response because it can't safely replay it. Set this above normal application time to first byte |
| `cache.varnish.enabled` | bool | `false` | Deploy the shared Varnish cache tier and emit the cache routing/backend. Cache-enabled GET/HEAD requests use healthy Varnish shards; other methods and requests observed while every shard is unhealthy go directly to the application backend. The bypass is recorded in `cache_degraded` and `haptic_degraded_cache_total` |
| `cache.varnish.loopbackPort` | int | `8090` | Dedicated internal HAProxy port Varnish fetches cache misses from (the "sandwich" backend leg), so the WAF/rate-limit/auth/routing chain runs once on the client request and never on the miss. Reached only by Varnish (via `originServiceName`, gated by the HAProxy NetworkPolicy); never published on the LoadBalancer |
| `cache.varnish.originServiceName` | string | `haptic-cache-origin` | Name of the internal ClusterIP Service (in the release namespace) that fronts the dedicated backend-fetch port on the HAProxy pods |
| `cache.varnish.workload` | string | `statefulset` | Workload type: `statefulset` updates one shard at a time; `deployment` uses the configured Deployment rollout strategy. |
| `cache.varnish.replicas` | int | `2` | Number of Varnish cache shards |
| `cache.varnish.image` | string | `""` | Empty selects the bundled non-root Varnish image for this chart version; set a full image reference to override it |
| `cache.varnish.imagePullPolicy` | string | `IfNotPresent` | Kubernetes pull policy for the Varnish image (`Always`, `IfNotPresent`, or `Never`) |
| `cache.varnish.malloc` | string | `256m` | Varnish `-s malloc,<size>` object storage per shard; excludes shared logs, compiled configuration, and process/object overhead |
| `cache.varnish.resources` | object | cpu `100m` / memory `384Mi` | Varnish pod resource requests and limits. A CPU request is required for autoscaling. Budget `malloc`, the memory-backed working directory (including the default 80 MiB shared log), compiled configuration, and process/object overhead |
| `cache.varnish.podDisruptionBudget` | object | enabled `true`, `maxUnavailable` `1` | PodDisruptionBudget settings for the Varnish shards. Pods also prefer separate nodes and use a soft hostname topology spread, so the default still runs on single-node clusters |
| `cache.varnish.networkPolicy.enabled` | bool | `true` | Emit a NetworkPolicy that only allows this release's HAProxy pods to reach Varnish and only allows Varnish egress to DNS plus this release's HAProxy HTTP origin |
| `cache.varnish.autoscaling.enabled` | bool | `false` | Autoscale the Varnish tier with a HorizontalPodAutoscaler. When on, the HPA owns the replica count (the static `replicas` is ignored) |
| `cache.varnish.autoscaling.minReplicas` | int | `2` | Minimum Varnish shards the HPA keeps |
| `cache.varnish.autoscaling.maxReplicas` | int | `6` | Maximum Varnish shards the HPA scales to |
| `cache.varnish.autoscaling.targetCPUUtilizationPercentage` | int | `70` | Target average CPU utilization that drives scaling |
| `cache.varnish.autoscaling.scaleDownStabilizationSeconds` | int | `600` | Seconds the HPA waits before acting on a scale-down, to protect cache warmth |

## Shared request rate limiting

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `rateLimit.shared.enabled` | bool | `false` | Enable the native shared rate-limit annotations and auto-enable the bundled `rate-limit` SPOA plugin. When false, Ingresses carrying `haproxy-haptic.org/rate-limit-requests` fail the render loudly instead of being silently unprotected. When true, leave `rateLimit.shared.managedStore.enabled=true` for the chart-managed HA Valkey/Sentinel store or provide `rateLimit.shared.externalStore.urls`; HAPTIC fails the render instead of silently using a per-pod fallback |
| `rateLimit.shared.failClosed` | bool | `false` | Runtime dependency policy. By default, a Valkey failure degrades both algorithms to a bounded per-sidecar limiter; if the hub/plugin returns no verdict, HAProxy allows the request. These paths set `rate_limit_degraded`, and plugin metrics distinguish local fallback decisions. Set true to deny when the store or plugin can't answer. Lease mode still spends its existing local lease before applying the store-failure policy |
| `spoaHub.plugins.rate-limit.timeoutMs` | int | `50` | Per-plugin processing timeout for shared rate-limit checks. The chart derives the HAProxy engine's outer deadline from this value; keep it low on public edges so overload reaches the configured lease/exact failure policy quickly |
| `spoaHub.plugins.rate-limit.storeOperationTimeoutMs` | int | `10` | Per-operation Redis/Valkey timeout rendered as the rate-limit plugin's `store_timeout_ms`. Exact `gcra` mode waits on this path per request; tune according to measured store round-trip time and failover behavior |
| `rateLimit.shared.managedStore.enabled` | bool | `true` | Use the bundled Valkey/Sentinel store when `rateLimit.shared.enabled` is true. Set false to supply your own store with `rateLimit.shared.externalStore.urls`. This setting alone deploys nothing. |
| `rateLimit.shared.externalStore.urls` | list | `[]` | One bring-your-own HA Redis/Valkey/Sentinel/Cluster endpoint, used with `managedStore.enabled=false` (setting both fails the render). Multiple URLs fail validation because the bundled plugin shares one circuit breaker across its shards. Configure the external store with a non-evicting memory policy. The chart owns the generated `store_url` and rejects a manual `store_url`/`store_urls` in `spoaHub.plugins.rate-limit.params` |
| `rateLimit.shared.managedStore.image` | string | `valkey/valkey:9.2-alpine` | Valkey image for the chart-managed shared rate-limit store |
| `rateLimit.shared.managedStore.imagePullPolicy` | string | `IfNotPresent` | Kubernetes pull policy for both the Valkey and Sentinel containers (`Always`, `IfNotPresent`, or `Never`) |
| `rateLimit.shared.managedStore.port` | int | `6379` | Valkey Service port for the chart-managed shared rate-limit store |
| `rateLimit.shared.managedStore.replicas` | int | `3` | Fixed Valkey pod count for the chart-managed Sentinel topology: one writable primary plus replicas for failover. Must be at least 3. This is HA, not automatic horizontal Valkey scaling |
| `rateLimit.shared.managedStore.maxMemory` | string | `96mb` | Valkey `--maxmemory` for the chart-managed shared rate-limit store |
| `rateLimit.shared.managedStore.maxMemoryPolicy` | string | `noeviction` | Valkey memory policy for the chart-managed shared rate-limit store. This must remain `noeviction`: evicting a limiter key silently recreates a full budget. At the memory limit, writes fail and follow the configured lease/exact dependency policy instead |
| `rateLimit.shared.managedStore.sentinel` | object | port `26379`, quorum `2` | Sentinel settings for managed-store failover: port, quorum, down-after, failover timeout, parallel syncs, and Sentinel container resources |
| `rateLimit.shared.managedStore.podDisruptionBudget` | object | enabled `true`, `maxUnavailable` `1` | PodDisruptionBudget settings for the managed Valkey pods |
| `rateLimit.shared.managedStore.networkPolicy.enabled` | bool | `true` | Emit a NetworkPolicy that only allows HAProxy/SPOA pods and store-internal Valkey/Sentinel traffic to the managed store |
| `rateLimit.shared.managedStore.resources` | object | cpu `50m` / memory `128Mi` | Valkey pod resource requests and limits. The chart-managed store is a fixed-size HA Sentinel topology; use bring-your-own Redis/Valkey infrastructure when you need horizontal store scaling |

## Request-body inspection and JSON schema validation

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `controller.config.templatingSettings.extraContext.tune.bufsize` | int | `16384` | HAProxy buffer size. Body inspection can raise it through `requestBodyInspection.haproxyBuffer.sizeBytes`; the larger value is used. |
| `controller.config.templatingSettings.extraContext.tune.cliMaxPayloadSize` | int | `131072` | HAProxy's `tune.cli.max-payload-size`, the ceiling on one runtime CLI payload. Emitted on HAProxy 3.4 and above only: the keyword doesn't exist below it, where a payload is capped by `tune.bufsize` instead |
| `controller.config.templatingSettings.extraContext.requestBodyInspection.haproxyBuffer.sizeBytes` | int | `16384` | Request-body inspectors need the whole body buffered, so this raises the shared HAProxy `tune.bufsize` whenever it names the larger of the two. API validation and Coraza policy body caps must fit within `sizeBytes - reservedBytes` |
| `controller.config.templatingSettings.extraContext.requestBodyInspection.haproxyBuffer.reservedBytes` | int | `8192` | Bytes reserved for the request line, headers, and rewrite space; increase for large cookies, JWTs, or tracing headers |
| `controller.config.templatingSettings.extraContext.requestBuffering.enabled` | bool | `true` | Buffer requests with `Content-Length` before taking a backend connection. Requests without that header bypass buffering. See [request buffering](libraries/base.md#request-buffering) for limits and per-route overrides |
| `controller.config.templatingSettings.extraContext.requestBuffering.waitTimeout` | string | `10s` | How long HAProxy waits for the declared body before returning `408`. HAProxy also releases the request once `tune.bufsize` is full |
| `controller.config.templatingSettings.extraContext.apiGateway.requestSchemaValidation.enabled` | bool | `false` | Enable native JSON request-schema annotations and auto-enable the bundled `api-gateway` plugin. Matching annotations fail loudly while disabled |
| `controller.config.templatingSettings.extraContext.apiGateway.requestSchemaValidation.requestBody.waitTimeout` | duration | `100ms` | HAProxy-side maximum wait for a matching POST/PUT/PATCH body. Unrelated routes don't wait |
| `controller.config.templatingSettings.extraContext.apiGateway.requestSchemaValidation.requestBody.defaultMaxBytes` | int | `8192` | Default validation input cap when an Ingress omits `request-schema-max-body-size`. The effective cap must fit in the shared HAProxy body capacity |
| `controller.config.templatingSettings.extraContext.apiGateway.requestSchemaValidation.defaultFailOpen` | bool | `true` | Default when an Ingress omits `request-schema-fail-open`. A missing plugin verdict allows the request and sets `schema_degraded`, exported as `haptic_degraded_schema_total`; schema validation reduces risk, it doesn't establish identity. Missing referenced Kubernetes objects still fail at render time. Set `false` where a malformed body reaching the backend is worse than refusing the caller |
| `controller.config.templatingSettings.extraContext.ssoAuth.allowedIssuers` | map | `{}` | Issuer URL prefixes that an [sso-auth](libraries/haptic-annotations.md#oidc-sign-in) provider Secret's `discovery-url` may start with, keyed by a name of your choice. A prefix matches only up to a `/`. While empty, every sso-auth route answers `503` |
| `spoaHub.plugins.api-gateway.timeoutMs` | int | `25` | Hub-side processing timeout for JSON validation. The chart derives the message's outer HAProxy deadline from this value; increase only for measured CPU or scheduling pressure |
| `spoaHub.plugins.api-gateway.maxConcurrency` | int/tpl | derived from sidecar memory (16 at the default 256Mi) | Ceiling for concurrent JSON parse/schema evaluations. With `adaptiveConcurrency` on (default) this is the controller's upper bound, not a fixed limit; derived from `spoaHub.resources` memory so it self-scales. Set a literal to override |

## Coraza WAF

Template-side routing, policy catalogs, and Ingress-author permissions live in the structured `extraContext.waf` bag so raw `HAProxyTemplateConfig` users get the same behavior. Process and plugin execution settings live under `spoaHub.plugins.coraza`; neither tree aliases or overrides the other. A non-empty `policies.inline`, `policies.configMapRefs`, or `policies.defaultPolicy` activates policy governance—there is no second enable flag that can leave a configured catalog inert.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `controller.config.templatingSettings.extraContext.waf.failClosed` | bool | `false` | Deny requests when the WAF can't return a verdict. The default allows these requests and records `waf_degraded` in access logs and `haptic_degraded_waf_total` in metrics. Requests the WAF inspects and rejects receive `403` with either setting. |
| `controller.config.templatingSettings.extraContext.waf.dispatch.mode` | string | `opt-in` | Which requests the rendered config sends to Coraza: `opt-in` runs it only on routes carrying a native or compatibility WAF annotation; `default-on` runs it on every route unless an authorized `nginx.ingress.kubernetes.io/enable-modsecurity: "false"` opts out. `default-on` auto-enables the Coraza plugin |
| `controller.config.templatingSettings.extraContext.waf.dispatch.defaultEnforcement` | string | `deny` | WAF enforcement (`deny` or `detect`) for requests dispatched by `mode: default-on`; selected policies and authorized per-route overrides take precedence. Ignored when `mode: opt-in` |
| `controller.config.templatingSettings.extraContext.waf.crs.url` | string | `""` | HTTPS URL of a gzip-compressed Core Rule Set (CRS) release archive. Empty uses the embedded rules. Failed refreshes retain the deployed rules, falling back to the embedded rules when none have been deployed. See [custom rulesets](operations/spoa-hub.md#update-the-waf-rule-set). |
| `controller.config.templatingSettings.extraContext.waf.crs.refreshInterval` | duration | `1h` | How often to re-fetch the ruleset. The request is conditional (`If-None-Match` / `If-Modified-Since`), so an unchanged ruleset costs one 304 and triggers no re-render, no push, and no WAF recompile |
| `controller.config.templatingSettings.extraContext.waf.crs.timeout` | duration | `30s` | Per-attempt HTTP timeout for the ruleset fetch |
| `controller.config.templatingSettings.extraContext.waf.crs.retries` | int | `3` | Retry attempts per fetch. A fetch that ultimately fails never fails the render — it falls back rather than blocking unrelated configuration changes |
| `controller.config.templatingSettings.extraContext.waf.customRules.limits.maxIngresses` | int | `32` | Maximum Ingress-specific Coraza applications from native or compatibility SecLang. This applies even when no reusable policy catalog is configured |
| `controller.config.templatingSettings.extraContext.waf.customRules.limits.maxBytesPerIngress` | int | `16384` | Maximum custom SecLang bytes accepted from one Ingress, independent of whether a reusable policy catalog is configured |
| `controller.config.templatingSettings.extraContext.waf.policies.defaultPolicy` | string | `""` | Optional policy applied to every managed Ingress without a selection. Required when `allowPolicySelection=false` |
| `controller.config.templatingSettings.extraContext.waf.policies.requestBody.waitTimeout` | duration | `100ms` | Maximum HAProxy wait only for policies whose `requestBody.mode` is `any` or `json` |
| `controller.config.templatingSettings.extraContext.waf.policies.requestBody.defaultMaxBytes` | int | `8192` | Body cap used when a policy omits `requestBody.maxBytes` |
| `controller.config.templatingSettings.extraContext.waf.policies.requestBody.maxBytes` | int | `8192` | Highest body cap any policy may request. This is deliberately separate from the default, so approving a larger policy doesn't enlarge every policy. The effective value is enforced by HAProxy and written into that Coraza application |
| `controller.config.templatingSettings.extraContext.waf.ingressPermissions.allowPolicySelection` | bool | `true` | Permit Ingress authors to select an approved policy. Disable with a `defaultPolicy` for an immutable baseline |
| `controller.config.templatingSettings.extraContext.waf.ingressPermissions.allowEnforcementOverride` | bool | `false` | Permit native or compatibility annotations to select `deny`/`detect` instead of administrator enforcement while Coraza governance is active |
| `controller.config.templatingSettings.extraContext.waf.ingressPermissions.allowWafDisable` | bool | `false` | Permit an Ingress to opt out through a compatibility annotation such as `enable-modsecurity: "false"`. This stronger capability is separate from enforcement-mode overrides |
| `controller.config.templatingSettings.extraContext.waf.ingressPermissions.allowCustomRules` | bool | `false` | Permit arbitrary per-Ingress WAF rules through the nginx-compatible `modsecurity-snippet` annotation while Coraza governance is active; this grants WAF-policy-author capability |
| `controller.config.templatingSettings.extraContext.waf.ingressPermissions.allowRawHAProxyConfig` | bool | `false` | While Coraza governance is active, also required to accept raw configuration annotations. It can't accept them while `extraContext.ingressPermissions.allowRawHAProxyConfig` is `false`. See [raw configuration annotations](operations/security.md#raw-configuration-annotations) |
| `controller.config.templatingSettings.extraContext.waf.policies.inline` | map | `{}` | Administrator-owned policies. Fields: `description`, `enforcement`, nested `requestBody.mode`/`maxBytes`, `allowedMethods`, `paranoiaLevel`, `anomalyThreshold`, `ruleExclusions`, and advanced `secLang` |
| `controller.config.templatingSettings.extraContext.waf.policies.configMapRefs` | map | `{}` | Trusted ConfigMap catalogs keyed by catalog name, each an exact `namespace`/`name`/`key` triple using the same policy schema. A map, like its sibling `inline`, so adding one catalog keeps the rest |
| `controller.config.templatingSettings.extraContext.waf.policies.selfService.enabled` | bool | `false` | Namespaced self-service authoring: each namespace may define policies for its own Ingresses in its well-known catalog ConfigMap |
| `controller.config.templatingSettings.extraContext.waf.policies.selfService.configMapName` | string | `waf-policies` | Well-known ConfigMap name discovered per namespace for self-service catalogs |
| `controller.config.templatingSettings.extraContext.waf.policies.selfService.key` | string | `policies.yaml` | Data key inside each self-service catalog ConfigMap |
| `controller.config.templatingSettings.extraContext.waf.policies.selfService.allowSecLang` | bool | `false` | Permit `secLang` in self-service policies (arbitrary rule code in the shared Coraza process — grant deliberately) |
| `controller.config.templatingSettings.extraContext.waf.policies.selfService.limits.maxPoliciesPerNamespace` | int | `4` | Maximum self-service policies loaded per namespace (deterministic sorted cut; excess policies fail closed for their selectors) |
| `controller.config.templatingSettings.extraContext.waf.policies.selfService.limits.maxTotalPolicies` | int | `64` | Cluster-wide self-service policy budget, separate from the trusted catalog's `limits.maxCount` |
| `controller.config.templatingSettings.extraContext.waf.policies.limits.maxCount` | int | `16` | Maximum policies across inline definitions and all trusted ConfigMaps |
| `controller.config.templatingSettings.extraContext.waf.policies.limits.maxSecLangBytes` | int | `65536` | Maximum advanced SecLang bytes in one reusable policy |
| `controller.config.templatingSettings.extraContext.waf.policies.limits.maxRuleExclusions` | int | `256` | Maximum structured Core Rule Set (CRS) rule-exclusion entries in one reusable policy |
| `spoaHub.plugins.coraza.timeoutMs` | int | `15` | Hub-side processing timeout for WAF evaluation. The chart derives the message's outer HAProxy deadline from this value; this is a failure bound, not expected latency |
| `spoaHub.plugins.coraza.maxConcurrency` | int/tpl | derived from sidecar memory (16 at the default 256Mi) | Ceiling for concurrent Coraza evaluations. With `adaptiveConcurrency` on (default) this is the controller's upper bound, not a fixed limit; derived from `spoaHub.resources` memory so it self-scales (give the sidecar more memory → higher ceiling). Set a literal to override |
| `spoaHub.plugins.coraza.adaptiveConcurrency` | bool | `true` | Adjust concurrent WAF evaluations from measured latency, up to `maxConcurrency`. Set `false` for a fixed limit. Requires a hub image with adaptive concurrency support. |

## Policy guardrails (governance)

Define named rules under `extraContext.governance` to supply defaults or constrain
fields on watched resources. Rules run in sorted key order and don't modify the
stored Kubernetes objects. The bundled `haptic-compress-enable` rule defaults
response compression to `"false"`; explicit Ingress annotations take precedence.
An administrator can change the rule's `default` to `"true"` after reviewing
[compression safety](libraries/haptic-annotations.md#compression).

Start in audit mode to find violations before enforcing a rule. Follow the
[policy rollout guide](operations/governance.md) for the procedure.

With `enforcement: reject`, a new or edited violating resource is denied at the
admission webhook, scoped to that resource's own admission (so one violator never
blocks an unrelated apply), while an already-present violator records a
`GovernanceViolation` Warning Event and keeps serving. `enforcement: audit` only
ever warns (a roll-out mode).

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `…extraContext.governance.enabled` | bool | `true` with the default library set | Master switch for the guardrails. The governance library itself declares `false`; the haptic-annotations library (on by default) merges after it and raises it to `true` so its shipped `haptic-compress-enable` rule applies. Set it to `false` explicitly to switch the engine off |
| `…extraContext.governance.exemptNamespaces` | list | `[]` | Namespaces skipped by every rule (infra/system). To skip one rule only, use that rule's `exemptNamespaces` |
| `…extraContext.governance.rules` | map | `{}` | Admin-declared rules keyed by rule name (see the fields below). A map, not a list, so your rules merge with — rather than replace — the ones the chart ships |

Each value of `rules` is an object:

| Field | Type | Description |
|-------|------|-------------|
| `enabled` | bool | **Required.** `true` applies the rule, `false` switches it off. Required rather than defaulted, so a typo fails the render instead of leaving the rule silently inert |
| `resource` | string | **Required.** Watched-resource name the rule targets (`ingresses`, `httproutes`, a custom CRD, …) |
| `path` | string | Concrete JSONPath into the resource: dotted keys, `['bracket']` keys (for dots/slashes), `[n]` indices. For example, `metadata.annotations['haproxy-haptic.org/rate-limit-rps']` |
| `default` | string | Inject this value when `path` is absent. Only a **concrete** `path` may carry a default (filtered/wildcard paths are validate-only) |
| `required` | bool | The value at `path` must be present and non-empty |
| `min` / `max` | int | Numeric bounds for the value at `path` |
| `onViolation` | string | `reject` (default) or `clamp` — on a `min`/`max` violation, rewrite the value to the nearest bound instead of rejecting |
| `allowed` | list | Allowed values (enum) for `path` |
| `pattern` | string | Regex the value at `path` must match |
| `anyOf` | list | At least one of the listed JSONPath expressions must be present |
| `satisfiedBy` | string | `tls` — satisfied by `spec.tls` on the resource **or** the chart-wide default HTTPS |
| `namespaces` | list | Apply the rule only to resources in these namespaces. Omitted or empty: every namespace |
| `exemptNamespaces` | list | Skip the rule for resources in these namespaces. Other rules still apply there |
| `enforcement` | string | `reject` (default) or `audit` |
| `message` | string | Custom violation message (optional) |

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        governance:
          enabled: true
          exemptNamespaces: [kube-system]
          rules:
            # Inject a default per-source rate limit; clamp anything above the ceiling.
            rate-limit-floor:
              enabled: true
              resource: ingresses
              path: metadata.annotations['haproxy-haptic.org/rate-limit-rps']
              default: "100"
              max: 10000
              onViolation: clamp
            # Require a WAF policy annotation on every HTTPRoute (cross-resource).
            httproute-waf-policy:
              enabled: true
              resource: httproutes
              path: metadata.annotations['haproxy-haptic.org/waf-policy']
              required: true
              enforcement: audit
            # Require TLS (spec.tls or the chart-wide default HTTPS satisfies it).
            ingress-tls:
              enabled: true
              resource: ingresses
              satisfiedBy: tls
```

To switch off a single rule — including one a template library ships — set its
`enabled` to `false`. You don't restate the others:

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        governance:
          rules:
            haptic-compress-enable:
              enabled: false
```

## Routing behavior

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `controller.config.templatingSettings.extraContext.routing.regexMatchOrder` | string | `default` | Path matching order: `default` (Exact > Regex > Prefix-exact > Prefix) or `last` (Exact > Prefix-exact > Prefix > Regex, performance-first) |

## PROXY protocol

For HAProxy behind a layer-4 load balancer. See [PROXY protocol](haproxy-deployment.md#proxy-protocol).

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `controller.config.templatingSettings.extraContext.proxyProtocol.enabled` | bool | `false` | Add HTTP and HTTPS binds that require a PROXY protocol header, so `client_ip`, `src`-keyed rate limiting, the WAF, and IP ACLs see the real client instead of the balancer. Adds the ports to the HAProxy Service, the container ports, and the NetworkPolicy |
| `controller.config.templatingSettings.extraContext.proxyProtocol.httpPort` | int | `8081` | PROXY-protocol HTTP port. Additional to `haproxy.ports.http`, which stays open and header-free — a connection reaching this port without the header is dropped, so only the balancer may target it |
| `controller.config.templatingSettings.extraContext.proxyProtocol.httpsPort` | int | `8444` | PROXY-protocol HTTPS port, using the same certificates, ciphers, and protocol negotiation as `haproxy.ports.https`. With TLS-Passthrough configured it attaches to the SNI-routing frontend instead |
| `controller.config.templatingSettings.extraContext.forwardedHeaders.trustedProxies` | list | `[]` | Source addresses (IPs or CIDRs) whose forwarding headers HAPTIC keeps, appending the client address to `X-Forwarded-For`. HAPTIC replaces or drops these headers from every other source. See [forwarded headers](operations/security.md#forwarded-headers) |
| `controller.config.templatingSettings.extraContext.http3.enabled` | bool | `true` | Serve [HTTP/3](haproxy-deployment.md#http3-quic) over UDP on every TLS-terminating HTTPS port and advertise it with `alt-svc`. Adds a UDP port to the HAProxy and Gateway Services, the container, and the NetworkPolicy |
| `controller.config.templatingSettings.extraContext.http3.altSvc.port` | int | `0` | Port advertised in `alt-svc`. `0` advertises the port the client used for HTTPS (the `Host` port, else 443); set it when UDP is published on a different port than TCP |
| `controller.config.templatingSettings.extraContext.http3.altSvc.maxAge` | int | `86400` | Seconds a client may cache the `alt-svc` advertisement |

## Default SSL certificate

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `defaultSSLCertificate.enabled` | bool | `true` | Enable default SSL certificate requirement. When the cert-manager API is absent and no inline cert is set, the chart generates a self-signed Secret (never touching an existing one) so the install converges out of the box |
| `defaultSSLCertificate.secretName` | string | `default-ssl-cert` | TLS Secret name containing certificate |
| `defaultSSLCertificate.namespace` | string | `""` | Secret namespace (defaults to `Release.Namespace`) |
| `defaultSSLCertificate.ecdsaSecretName` | string | `""` | Optional ECDSA companion Secret for the default cert. When set, HAProxy serves ECDSA to modern clients and `secretName` (RSA) to the rest on the no-SNI / unmatched-SNI path. Must be in the same namespace. Empty = single default cert. See [Dual RSA and ECDSA certificates](ssl-certificates.md#default-certificate) |
| `defaultSSLCertificate.certManager.enabled` | bool | `true` | Use cert-manager for certificate provisioning |
| `defaultSSLCertificate.certManager.createIssuer` | bool | `true` | Create self-signed Issuer (dev/test only) |
| `defaultSSLCertificate.certManager.dnsNames` | list | `["localdev.me", "*.localdev.me"]` | DNS names for the certificate |
| `defaultSSLCertificate.certManager.issuerRef.name` | string | `""` | Issuer name (auto-set when createIssuer=true) |
| `defaultSSLCertificate.certManager.issuerRef.kind` | string | `Issuer` | Issuer kind |
| `defaultSSLCertificate.certManager.duration` | duration | `8760h` | Certificate validity (1 year) |
| `defaultSSLCertificate.certManager.renewBefore` | duration | `720h` | Renew before expiry (30 days) |
| `defaultSSLCertificate.create` | bool | `false` | Create Secret from inline cert/key (testing only). Requires `defaultSSLCertificate.certManager.enabled=false` so exactly one actor owns the Secret |
| `defaultSSLCertificate.cert` | string | `""` | PEM certificate (when create=true) |
| `defaultSSLCertificate.key` | string | `""` | PEM private key (when create=true) |
| `controller.config.templatingSettings.extraContext.tls.sessionTickets.enabled` | bool | `false` | Share ticket keys across HAProxy pods so clients can resume sessions after reconnecting to another pod. HAPTIC rotates the three-key file on the first render of each day (Coordinated Universal Time) and reloads HAProxy. See [TLS session resumption](ssl-certificates.md#tls-session-resumption) |

<a id="controller-config"></a>

## Controller configuration

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `controller.config.credentialsSecretRef.name` | string | Auto-generated | Secret containing the agent credentials |
| `controller.config.credentialsSecretRef.namespace` | string | `""` | Credentials secret namespace |
| `controller.config.podSelector.matchLabels` | map | `{app.kubernetes.io/component: loadbalancer}` | Labels to match HAProxy pods |

## Leader election

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `controller.config.controller.leaderElection.enabled` | bool | `true` | Enable leader election (recommended for HA) |
| `controller.config.controller.leaderElection.leaseName` | string | `""` | Lease resource name (defaults to release `fullname`) |
| `controller.config.controller.leaderElection.leaseDuration` | duration | `30s` | Failover timeout duration |
| `controller.config.controller.leaderElection.renewDeadline` | duration | `20s` | Leader renewal timeout |
| `controller.config.controller.leaderElection.retryPeriod` | duration | `5s` | Retry interval between attempts |
| `controller.config.controller.renderGateInterval` | duration | `1s` | Minimum interval between background HAProxy configuration checks. Increase to reduce validation CPU use during frequent updates. |

## Agent configuration

These settings control how HAPTIC applies configuration to each HAProxy pod.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `controller.config.dataplane.minDeploymentInterval` | duration | `5s` | Minimum interval between reloads of one HAProxy pod. A reload requested sooner waits until the interval ends. Maximum `60s` with the bundled fleet. |
| `controller.config.dataplane.driftPreventionInterval` | duration | `60s` | How often HAPTIC checks each HAProxy pod for configuration drift and reapplies the desired configuration. |
| `controller.config.dataplane.reloadVerificationTimeout` | duration | `60s` | Maximum wait for a reload to finish before the agent restores the last known good files. Maximum `60s`. |
| `controller.config.dataplane.syncTimeout` | duration | `2m` | How long the controller waits for one pod to answer an apply |
| `controller.config.dataplane.mapsDir` | string | `/etc/haproxy/maps` | HAProxy maps directory. With the bundled fleet (`haproxy.enabled=true`) it must sit directly under `/etc/haproxy`, which is where the pod mounts its config volume and resolves every auxiliary path |
| `controller.config.dataplane.sslCertsDir` | string | `/etc/haproxy/ssl` | SSL certificates directory. Same `/etc/haproxy` constraint as `mapsDir` when the bundled fleet is enabled; the directory name itself is free |
| `controller.config.dataplane.generalStorageDir` | string | `/etc/haproxy/general` | Directory for generated auxiliary files. Must remain `/etc/haproxy/general` with the bundled fleet so the sidecars can read them. |
| `controller.config.dataplane.secretsDir` | string | unset (`/etc/haproxy/secrets`) | Directory for secret files that sidecars read. Must remain `/etc/haproxy/secrets` with the bundled fleet, where the pod mounts a memory-backed volume in the agent and SPOA hub containers only. |
| `controller.config.dataplane.configFile` | string | `/etc/haproxy/haproxy.cfg` | HAProxy config file path. Same `/etc/haproxy` constraint as `mapsDir` when the bundled fleet is enabled |

## Watched resources

`controller.config.watchedResources.<name>` is a map of resource entries. The chart's template libraries contribute most entries (Ingress, Service, EndpointSlice, Secret, plus the Gateway API route kinds when the gateway library is on); operators can add or override entries here. Each entry accepts:

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `apiVersion` | string | one of `apiVersion`/`apiVersions` required | API group/version (for example `networking.k8s.io/v1`, `v1` for core). Mutually exclusive with `apiVersions` |
| `apiVersions` | list | `[]` | Ordered candidate API versions; the controller watches the first one the apiserver serves (for example `["gateway.networking.k8s.io/v1", "gateway.networking.k8s.io/v1alpha2"]`). Mutually exclusive with `apiVersion` |
| `optional` | bool | `false` | Marks the resource non-essential: when no candidate version is served, the watch is dropped and every `templateSnippet`/`validationTest` naming it in `requires` is stripped from the effective config, instead of failing startup |
| `resources` | string | required | Plural resource name (for example `ingresses`) |
| `indexBy` | list | `[]` | JSONPath expressions used to index resources for O(1) template lookup |
| `fieldSelector` | string | `""` | Client-side JSONPath filter (for example `spec.ingressClassName=haptic`); supports any JSONPath expression unlike Kubernetes' built-in `fieldSelector` |
| `labelSelector` | string | `""` | Server-side label selector for watch-time filtering (equality-only `key=value` pairs joined by commas) |
| `namespaces` | list | unset | Watch only these namespaces. The chart then grants the resource with a Role in each listed namespace instead of the ClusterRole, and a template lookup into another namespace fails the render. Requires `indexBy` to start with `metadata.namespace` |
| `enableValidationWebhook` | bool | `false` | Include this resource in the chart-rendered `ValidatingWebhookConfiguration` |
| `statusPatch` | bool | `false` | Allow the controller to patch this resource's `/status` subresource |
| `ignoreFields` | list | `[]` | JSONPath expressions dropped from this resource before storing it, in addition to `watchedResourcesIgnoreFields`; `[*]` selects every array element. An update that changes only ignored fields triggers no render |
| `store` | string | `full` | `full` keeps all resources in memory; `on-demand` fetches with caching (lower memory, slower lookups). Useful for very large Secret stores |
| `debounceInterval` | duration | `""` (`100ms`) | Per-resource debounce window; empty/unparseable falls back to the controller-wide default (`DefaultDebounceInterval`, `100ms`). Avoid raising the value for resources that drive backend membership — `EndpointSlices` and `pods` in particular — because the debounce delays Pod removal from the HAProxy server pool by that whole window, so live traffic continues hitting Terminating pods until the next render fires |

## Logging and templating

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `controller.logLevel` | string | `INFO` | Initial log level (`LOG_LEVEL` env var): `TRACE`, `DEBUG`, `INFO`, `WARN`, `ERROR` (case-insensitive) |
| `controller.config.logging.level` | string | `""` | Log level in the `HAProxyTemplateConfig` CRD (`spec.logging.level`); overrides `controller.logLevel` at runtime when non-empty |
| `controller.config.templatingSettings.engine` | string | `scriggo` | Template engine used for rendering; `scriggo` is the only supported value |
| `controller.config.templatingSettings.extraContext.diagnostics.routingHeaders.enabled` | bool | `false` | Add `X-HAProxy-Backend` and matching-detail response headers; see [debug headers](libraries/base.md#debug-headers) |
| `controller.config.templatingSettings.extraContext.tracing.enabled` | bool | `false` | Adopt valid inbound W3C Trace Context or create a new trace, then propagate `traceparent` to backends. Independent of the SPOA hub. Enables trace fields in access logs. When off, inbound trace IDs remain available for log correlation, but HAPTIC creates and propagates no trace context |
| `controller.config.templatingSettings.extraContext.tracing.sampleRate` | int | `100` | Percentage sampled when HAPTIC starts a trace (0–100). Existing traces retain the incoming sampling decision. |
| `controller.config.templatingSettings.extraContext.tracing.otlp.endpoint` | string | `""` | OpenTelemetry Protocol (OTLP) HTTP endpoint, for example `http://tempo.observability.svc.cluster.local:4318/v1/traces`. Empty means propagation only. Export requires `vector.enabled=true`; spans derive from delivered access-log records, so suppressed or lost records produce no span. SERVER spans carry client timings; CLIENT spans carry upstream timings. Spans omit client IPs; correlate with logs using `haptic.req_id`. Names use the method, host, and matched route template, with a resource-name or method-only fallback when no route matches |
| `controller.config.templatingSettings.extraContext.tracing.otlp.serviceName` | string | `haptic` | `service.name` resource attribute reported for HAPTIC's own spans |
| `controller.config.templatingSettings.extraContext.tracing.otlp.clusterName` | string | `""` | Cluster name reported as `k8s.cluster.name` on exported spans. Empty omits the attribute. |
| `controller.config.templatingSettings.extraContext.accessLog.fields` | map | `{}` | Extra JSON fields, captured at request time and logged as strings. Use `str(<value>)` for a constant. See [field names and expression limits](operations/access-logging.md#add-your-own-fields). |
| `controller.config.templatingSettings.extraContext.accessLog.targets` | map | `{vector: {address: /run/vector/haproxy.sock, format: raw}}` while `vector.enabled` is true (the chart default); `{stdout: {address: stdout}}` otherwise | Destinations keyed by name. Adding a key preserves existing targets; each receives a copy of each record. Supports UDP, local sockets, stdout, and buffered TCP rings. See [destination fields and limits](operations/access-logging.md#where-the-logs-go). |
| `controller.config.templatingSettings.extraContext.accessLog.maxLineBytes` | int | `16384` | `log ... len <bytes>`. HAProxy truncates a longer record mid-byte, which makes it invalid JSON; raise it if custom fields or captured request headers push records past the limit (1024–65535) |
| `controller.config.templatingSettings.extraContext.accessLog.suppress.successful` | bool | `false` | Drop access-log records for 2xx/3xx requests that no gate denied. Denials, 4xx, and 5xx remain. Suppression also removes these requests from log-derived metrics and traces. See [Access logging](operations/access-logging.md) |
| `controller.config.templatingSettings.extraContext.ingressPermissions.allowRawHAProxyConfig` | bool | `false` | Accept annotations that insert raw HAProxy configuration (`haproxy-haptic.org/config-*`, `haproxy-ingress.github.io/config-*`, `haproxy.org/backend-config-snippet`, `nginx.ingress.kubernetes.io/configuration-snippet`). While `false`, an Ingress that sets one is rejected. Enable only when every Ingress writer may edit the HAProxy configuration. See [raw configuration annotations](operations/security.md#raw-configuration-annotations) |
| `controller.config.templatingSettings.extraContext.annotationCompatibility.basicAuth.passwordHashValidation.regex` | string | `"^\$(2[aby]|5|6|y)\$[./0-9A-Za-z$=]+$"` | Regex every credential in a basic-auth Secret must match (the `auth-secret` annotations of the haproxytech, haproxy-ingress, and nginx-ingress libraries). The default accepts bcrypt, SHA-256/SHA-512 crypt, and yescrypt; see [accepted formats](operations/security.md#basic-auth-password-hashes). A non-matching credential fails the render. Go RE2 syntax — no lookaheads, so express the policy as the *allowed* format |
| `controller.config.templatingSettings.extraContext.annotationCompatibility.basicAuth.passwordHashValidation.errorMessage` | string | `Replace it with a bcrypt, SHA-256/SHA-512 crypt or yescrypt hash, for example from htpasswd -nB <user>` | How to fix a refused credential; appended to the error that names the Ingress, Secret, user, and pattern |
| `controller.config.templatingSettings.extraContext.tls.hsts.enabled` | bool | `false` | Emit a global `Strict-Transport-Security` header on TLS responses. Opt-in; per-Ingress HSTS annotations still win |
| `controller.config.templatingSettings.extraContext.tls.hsts.maxAge` | string | `"31536000"` | HSTS `max-age` in seconds for the global header and the default for `haproxy-haptic.org/hsts-max-age` |
| `controller.config.templatingSettings.extraContext.tls.hsts.includeSubdomains` | bool | `false` | Add `includeSubDomains` to the global HSTS header |
| `controller.config.templatingSettings.extraContext.tls.hsts.preload` | bool | `false` | Add `preload` to the global HSTS header |
| `controller.config.watchedResourcesIgnoreFields` | list | `[metadata.managedFields, metadata.annotations['kubectl.kubernetes.io/last-applied-configuration']]` | Fields to ignore in watched resources |

## Prometheus exporter

HAProxy's built-in Prometheus exporter answers on the `stats` port (`8404`, `/metrics`) on every HAProxy pod and is scraped directly. These values become the query HAProxy applies to a scrape that sends none, so every scraper — the bundled `haproxy.monitoring.podMonitor` and a hand-written job alike — gets the same exposition; a scraper's own query wins wholesale, so `/metrics?` returns the raw exposition. See [HAProxy data-plane metrics](operations/monitoring.md#haproxy-data-plane-metrics).

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `controller.config.templatingSettings.extraContext.prometheusExporter.excludeMaintServers` | bool | `true` | Omit individual servers in maintenance state using HAProxy's `no-maint` filter. The backend aggregate still counts them. Set `false` if you monitor individual drained servers. Supported on HAProxy 3.0–3.4 |
| `controller.config.templatingSettings.extraContext.prometheusExporter.excludeMetrics` | map | eight exclusions, seven on | Named exclusions with required `enabled` and `families` (exact metric names). Optional `requires` gates an entry on a dotted `extraContext` path. Disable an entry or add a new key without replacing the map. Defaults omit host maxima, redundant check aggregates, and unused cache/backup families; compression metrics remain. When request metrics are enabled, exclusions also remove overlapping HTTP counters and rolling time averages. Per-server HTTP responses and the backend server-state census remain. See [HAProxy data-plane metrics](operations/monitoring.md#haproxy-data-plane-metrics) |

## Webhook configuration

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `controller.webhook.enabled` | bool | `true` | Enable admission webhook validation |
| `controller.webhook.timeoutSeconds` | int | `10` | API-server timeout for watched-resource admission. HAPTIC configures the controller deadline one second shorter. Allowed range: `2..30` |
| `controller.webhook.secretName` | string | Auto-generated | Webhook TLS certificate secret name |
| `controller.webhook.service.port` | int | `443` | Webhook service port |
| `controller.webhook.certManager.enabled` | bool | `false` | cert-manager integration for the webhook cert. Default `false`: the chart issues a self-signed cert itself. Set `true` for cert-manager-managed issuance and auto-rotation; for manual certs keep `false` and set `controller.webhook.caBundle` |
| `controller.webhook.certManager.createIssuer` | bool | `true` | Create a self-signed Issuer for webhook certs |
| `controller.webhook.certManager.issuerRef.name` | string | `""` | Issuer name (auto-set when createIssuer=true) |
| `controller.webhook.certManager.issuerRef.kind` | string | `Issuer` | Issuer kind |
| `controller.webhook.certManager.duration` | duration | `8760h` | Certificate validity (1 year) |
| `controller.webhook.certManager.renewBefore` | duration | `720h` | Renew before expiry (30 days) |
| `controller.webhook.selfSigned.certValidityDays` | int | `3650` | Validity in days of the chart-generated self-signed webhook cert (used when `certManager.enabled=false` and `caBundle` is empty). Long by default because the chart doesn't auto-rotate it: the cert is generated once and reused across upgrades via `lookup`. Rotate manually by deleting the Secret (`controller.webhook.secretName`) and running `helm upgrade`, or use cert-manager for automatic rotation |
| `controller.webhook.caBundle` | string | `""` | Base64-encoded CA bundle (manual certs) |

## Pluggable validators

Validate generated plugin configuration before HAPTIC deploys it. The chart adds
the bundled validator when the SPOA hub is enabled. See [custom validators](operations/pluggable-validators.md)
to add checks for other generated files.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `controller.validators.enabled` | bool/null | `null` | Master enable for the validator sidecar. `null` auto-derives from the SPOA hub sidecar's own enable; `true` renders it even when `spoaHub` is off; `false` forces it off |
| `controller.validators.socketDir` | string | `/var/run/haptic-validators` | Directory for the validator Unix socket — a shared emptyDir mounted into both the controller container and the validator sidecar |
| `controller.validators.socketName` | string | `spoa-hub.sock` | Socket filename. The controller dials `<socketDir>/<socketName>`, and the chart writes that path into the auto-wired `spec.validators` entry |
| `controller.validators.resources.requests.cpu` | string | `25m` | Validator sidecar CPU request |
| `controller.validators.resources.requests.memory` | string | `64Mi` | Validator sidecar memory request |
| `controller.validators.resources.limits.memory` | string | `256Mi` | Validator sidecar memory limit |
| `controller.validators.securityContext` | map | See values.yaml | Container security context for the validator sidecar. Default runs user and group 65532 (matching the controller's nonroot user) so the Unix socket is readable and writable by the controller without extra `fsGroup` plumbing; read-only root filesystem, no privilege escalation, all capabilities dropped |
| `controller.validators.extraVolumeMounts` | list | `[]` | Extra volume mounts added to the validator sidecar only (rendered through `tpl`); for auxiliary data a plugin's `validate()` needs, such as MaxMind MMDB files or Open Worldwide Application Security Project (OWASP) Core Rule Set (CRS) files. Same shape as `spoaHub.extraVolumeMounts` |
| `controller.validators.entries` | list | `[]` | Entries appended to the CRD's `spec.validators` (each: `name`, `socketPath`, `files` glob list, optional `timeoutMs`/`maxConnections`). The chart auto-appends a `spoa-hub` entry validating `general/spoa-hub-config.toml` when both the SPOA hub sidecar and the validator sidecar are enabled; an operator entry named `spoa-hub` takes precedence |

## IngressClass

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `ingressClass.enabled` | bool | `true` | Create IngressClass resource |
| `ingressClass.name` | string | `haptic` | IngressClass name; default avoids conflict with other HAProxy-based ingress controllers (override to `haproxy` when replacing one) |
| `ingressClass.default` | bool | `false` | Mark as default IngressClass |
| `ingressClass.controllerName` | string | `haproxy-haptic.org/controller` | Controller identifier |

## GatewayClass

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `gatewayClass.enabled` | bool | `true` | Create GatewayClass resource |
| `gatewayClass.name` | string | `haptic` | GatewayClass name; default matches `ingressClass.name` |
| `gatewayClass.default` | bool | `false` | Emit the `gateway.networking.k8s.io/is-default-class` annotation; Gateways still require `spec.gatewayClassName` |
| `gatewayClass.controllerName` | string | `haproxy-haptic.org/controller` | Controller identifier |
| `gatewayClass.parametersRef.group` | string | `haproxy-haptic.org` | HAProxyTemplateConfig API group |
| `gatewayClass.parametersRef.kind` | string | `HAProxyTemplateConfig` | HAProxyTemplateConfig kind |
| `gatewayClass.parametersRef.name` | string | `""` | Config name (defaults to `controller.configName`) |
| `gatewayClass.parametersRef.namespace` | string | `""` | Config namespace (defaults to `Release.Namespace`) |

## Credentials

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `credentials.existingSecret` | string | `""` | Existing Secret with `dataplane_username` and `dataplane_password` keys. The chart doesn't create or change it. Use a different name when migrating from chart-managed credentials. |
| `credentials.dataplane.username` | string | `admin` | Username for chart-managed credentials; used by the explicit legacy HTTP agent transport. |
| `credentials.dataplane.password` | string | `""` | Password for chart-managed credentials. Empty reuses the live Secret during Helm install or upgrade, otherwise generates a random password. For rendering without cluster access, use `credentials.existingSecret` or supply an explicit value through your secret manager. |

## ServiceAccount & RBAC

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `controller.serviceAccount.create` | bool | `true` | Create ServiceAccount |
| `controller.serviceAccount.automount` | bool | `true` | Automount API credentials |
| `controller.serviceAccount.annotations` | map | `{}` | ServiceAccount annotations |
| `controller.serviceAccount.name` | string | `""` | ServiceAccount name (auto-generated if empty) |
| `controller.rbac.create` | bool | `true` | Create RBAC resources |
| `controller.rbac.releaseNamespaceSecretsOnly` | bool | `false` | Read Secrets only in the release namespace: the ClusterRole drops `secrets`, a Role grants it there, and references to Secrets elsewhere are rejected. See [Read Secrets only in the release namespace](operations/security.md#read-secrets-only-in-the-release-namespace) |

## Pod configuration (controller)

Configure controller placement, pod metadata, and runtime settings under `controller.podSpec.*`.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `controller.podSpec.imagePullSecrets` | list | `[]` | Image pull secrets for private registries |
| `controller.podSpec.podAnnotations` | map | `{}` | Pod annotations |
| `controller.podSpec.podLabels` | map | `{}` | Additional pod labels |
| `controller.podSpec.priorityClassName` | string | `""` | Pod priority class name |
| `controller.podSpec.runtimeClassName` | string | `""` | Runtime class (for example gVisor, Kata) |
| `controller.podSpec.terminationGracePeriodSeconds` | int | `90` | Termination grace period |
| `controller.podSpec.dnsPolicy` | string | `ClusterFirst` | DNS policy |
| `controller.podSpec.dnsConfig` | map | `{}` | DNS config |
| `controller.podSpec.hostAliases` | list | `[]` | /etc/hosts entries |
| `controller.podSpec.topologySpreadConstraints` | list | `[]` | Pod topology spread constraints |
| `controller.podSpec.nodeSelector` | map | `{}` | Node selector |
| `controller.podSpec.tolerations` | list | `[]` | Pod tolerations |
| `controller.podSpec.affinity` | map | `{}` | Pod affinity rules |
| `controller.podSpec.podSecurityContext.runAsNonRoot` | bool | `true` | Run as non-root user |
| `controller.podSpec.podSecurityContext.runAsUser` | int | `65532` | User ID |
| `controller.podSpec.podSecurityContext.runAsGroup` | int | `65532` | Group ID |
| `controller.podSpec.podSecurityContext.fsGroup` | int | `65532` | Filesystem group ID |
| `controller.podSpec.podSecurityContext.seccompProfile.type` | string | `RuntimeDefault` | Seccomp profile type |

## Container security context

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `controller.securityContext.allowPrivilegeEscalation` | bool | `false` | Allow privilege escalation |
| `controller.securityContext.capabilities.drop` | list | `[ALL]` | Dropped capabilities |
| `controller.securityContext.readOnlyRootFilesystem` | bool | `true` | Read-only root filesystem |
| `controller.securityContext.runAsNonRoot` | bool | `true` | Run as non-root |
| `controller.securityContext.runAsUser` | int | `65532` | Container user ID |

## Service & health probes

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `controller.service.type` | string | `ClusterIP` | Controller service type |
| `controller.service.annotations` | map | `{}` | Service annotations (cloud-LB hints, etc.) |
| `controller.service.clusterIP` | string | `""` | Pin a specific ClusterIP; leave empty for auto-assignment |
| `controller.service.loadBalancerIP` | string | `""` | LoadBalancer IP (when `type: LoadBalancer`) |
| `controller.service.loadBalancerSourceRanges` | list | `[]` | CIDR allowlist for LoadBalancer traffic |
| `controller.service.loadBalancerClass` | string | `""` | LoadBalancer class (Kubernetes 1.24+ multi-LB) |
| `controller.service.externalTrafficPolicy` | string | `""` | `Cluster` or `Local`; `Local` preserves client source IP at the cost of uneven distribution |
| `controller.service.internalTrafficPolicy` | string | `""` | `Cluster` or `Local` for in-cluster traffic |
| `controller.service.sessionAffinity` | string | `""` | `None` or `ClientIP` |
| `controller.service.sessionAffinityConfig` | map | `{}` | Session-affinity tuning (when `sessionAffinity: ClientIP`) |
| `controller.livenessProbe.httpGet.path` | string | `/livez` | Liveness probe path. `/livez` keeps a replica alive while it serves the previous configuration and leads; see [health checks during configuration changes](development/debug-endpoints.md#health-checks-during-configuration-changes) |
| `controller.livenessProbe.httpGet.port` | string | `healthz` | Named container port the probe targets (declared by `controller.ports.healthz`) |
| `controller.livenessProbe.initialDelaySeconds` | int | `10` | Initial delay |
| `controller.livenessProbe.periodSeconds` | int | `10` | Probe period |
| `controller.livenessProbe.failureThreshold` | int | `3` | Failure threshold |
| `controller.readinessProbe.httpGet.path` | string | `/readyz` | Readiness probe path. `/readyz` also requires installed admission validators and, while another replica can validate, a finished first full render, so only replicas that can validate receive admission requests |
| `controller.readinessProbe.httpGet.port` | string | `healthz` | Named container port the probe targets |
| `controller.readinessProbe.initialDelaySeconds` | int | `5` | Initial delay |
| `controller.readinessProbe.periodSeconds` | int | `5` | Probe period |
| `controller.readinessProbe.failureThreshold` | int | `3` | Failure threshold |
| `controller.startupProbe.enabled` | bool | `true` | Allow startup validation to finish before liveness and readiness probes begin. Disable only if your replacement probes allow enough time for the full validation suite. |
| `controller.startupProbe.httpGet.path` | string | `/healthz` | Startup probe path |
| `controller.startupProbe.httpGet.port` | string | `healthz` | Named container port the probe targets |
| `controller.startupProbe.initialDelaySeconds` | int | `0` | Initial delay |
| `controller.startupProbe.periodSeconds` | int | `10` | Probe period |
| `controller.startupProbe.timeoutSeconds` | int | `1` | Probe timeout |
| `controller.startupProbe.successThreshold` | int | `1` | Success threshold |
| `controller.startupProbe.failureThreshold` | int | `30` | Failure threshold (with `periodSeconds: 10` this gives 5 minutes for startup). Startup validation may use 80% of the window; raise it if a large set of validation tests needs longer |

## Resources & scheduling

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `controller.resources.requests.cpu` | string | `100m` | CPU request |
| `controller.resources.requests.memory` | string | `1Gi` | Memory request (matches `limits.memory`) |
| `controller.resources.limits.memory` | string | `1Gi` | Memory limit. The floor is the load gate, which runs the bundled `validationTests` on every config load and peaks above 512Mi — see [Controller resource sizing](./operations/performance.md#controller-resource-sizing) |

Pod-level scheduling fields (`nodeSelector`, `tolerations`, `affinity`, etc.) live under `controller.podSpec.*` — see [Pod configuration (controller)](#pod-configuration-controller).

## Controller extras & rollout

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `controller.extraEnv` | list | `[]` | Extra environment variables for the controller container |
| `controller.extraVolumes` | list | `[]` | Extra volumes for the controller pod; rendered through `tpl` so values can reference chart values |
| `controller.extraVolumeMounts` | list | `[]` | Extra volume mounts for the controller container; rendered through `tpl` |
| `controller.initContainers` | list | `[]` | Init containers run before the controller starts |
| `controller.sidecars` | list | `[]` | Additional sidecar containers in the controller pod; rendered through `tpl` |
| `controller.lifecycle` | map | `{}` | Container lifecycle hooks (`preStop`, `postStart`) for the controller container |
| `controller.updateStrategy.type` | string | `RollingUpdate` | Controller Deployment update strategy |
| `controller.updateStrategy.rollingUpdate.maxSurge` | int/string | `25%` | Maximum surge during rolling updates |
| `controller.updateStrategy.rollingUpdate.maxUnavailable` | int/string | `25%` | Maximum unavailable during rolling updates |
| `controller.minReadySeconds` | int | `0` | Minimum seconds a new controller pod must be ready before counting as available |
| `controller.revisionHistoryLimit` | int | `10` | Number of old ReplicaSets to retain |

## Autoscaling & PDB

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `controller.autoscaling.enabled` | bool | `false` | Enable HorizontalPodAutoscaler |
| `controller.autoscaling.minReplicas` | int | `1` | Minimum replicas |
| `controller.autoscaling.maxReplicas` | int | `10` | Maximum replicas |
| `controller.autoscaling.targetCPUUtilizationPercentage` | int | `80` | Target CPU utilization (omitted from the rendered HPA when empty) |
| `controller.autoscaling.targetMemoryUtilizationPercentage` | int | unset | Target memory utilization (omitted from the rendered HPA when empty) |
| `controller.podDisruptionBudget.enabled` | bool | `true` | Enable PodDisruptionBudget; only rendered when `controller.replicaCount > 1` |
| `controller.podDisruptionBudget.minAvailable` | int/string | `1` | Minimum available pods (mutually exclusive with `maxUnavailable`) |
| `controller.podDisruptionBudget.maxUnavailable` | int/string | unset | Maximum unavailable pods (mutually exclusive with `minAvailable`); leave unset to use `minAvailable` |

## Monitoring

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `controller.monitoring.serviceMonitor.enabled` | bool | `false` | Create ServiceMonitor for Prometheus |
| `controller.monitoring.serviceMonitor.interval` | duration | `30s` | Scrape interval |
| `controller.monitoring.serviceMonitor.scrapeTimeout` | duration | `10s` | Scrape timeout |
| `controller.monitoring.serviceMonitor.labels` | map | `{}` | ServiceMonitor labels (used by Prometheus to select which ServiceMonitors to use) |
| `controller.monitoring.serviceMonitor.relabelings` | list | `[]` | Prometheus relabelings applied before scraping |
| `controller.monitoring.serviceMonitor.metricRelabelings` | list | `[]` | Metric relabelings applied to scraped metrics |
| `controller.monitoring.podMonitor.enabled` | bool | `false` | Create PodMonitor (alternative to ServiceMonitor when scraping pods directly) |
| `controller.monitoring.podMonitor.interval` | duration | `30s` | PodMonitor scrape interval |
| `controller.monitoring.podMonitor.scrapeTimeout` | duration | `10s` | PodMonitor scrape timeout |
| `controller.monitoring.podMonitor.labels` | map | `{}` | PodMonitor labels |
| `controller.monitoring.podMonitor.relabelings` | list | `[]` | PodMonitor relabelings |
| `controller.monitoring.podMonitor.metricRelabelings` | list | `[]` | PodMonitor metric relabelings |
| `controller.monitoring.prometheusRule.enabled` | bool | `false` | Create PrometheusRule with alerting rules |
| `controller.monitoring.prometheusRule.labels` | map | `{}` | PrometheusRule labels |
| `controller.monitoring.prometheusRule.rules` | list | `[]` | Custom alerting rules; overrides the default rule set when non-empty |
| `controller.monitoring.prometheusRule.defaultRules.enabled` | bool | `true` | Emit the chart's default rule set, with individual switches below; only consulted when `rules` is empty |
| `controller.monitoring.prometheusRule.defaultRules.retainedConfiguration` | bool | `true` | Include the `HAProxyRetainedConfigurationActive` warning after a leader uses retained configuration for two minutes |
| `controller.monitoring.prometheusRule.defaultRules.reconciliationErrors` | bool | `true` | Include the `HAProxyControllerReconciliationErrors` warning rule |
| `controller.monitoring.prometheusRule.defaultRules.deploymentFailures` | bool | `true` | Include the `HAProxyControllerDeploymentFailures` critical rule |
| `controller.monitoring.prometheusRule.defaultRules.highQueueDepth` | bool | `true` | Include the `HAProxyControllerHighQueueDepth` warning rule |
| `controller.monitoring.prometheusRule.defaultRules.leaderElectionLost` | bool | `true` | Include the `HAProxyControllerNoLeader` critical rule |
| `controller.monitoring.prometheusRule.defaultRules.fleetDiverged` | bool | `true` | Include the `HAProxyFleetDiverged` warning rule: some HAProxy pods haven't converged to the desired config for 5 minutes. Transient deploy failures self-heal, so sustained divergence is a real fault — this is the noise-free replacement for alerting on raw deployment errors |
| `controller.monitoring.prometheusRule.defaultRules.configRejected` | bool | `true` | Include the `HAProxyControllerConfigRejected` warning rule: the validation gate rejected a config change — the controller keeps serving the last-good config and the latest change isn't live |
| `controller.monitoring.prometheusRule.defaultRules.configNotValidated` | bool | `true` | Include the `HAProxyControllerConfigNotValidated` warning rule: a controller replica hasn't validated its newest `HAProxyTemplateConfig` for 10 minutes, because validation failed or stayed inconclusive — the latest change isn't live |
| `controller.monitoring.prometheusRule.defaultRules.configPinned` | bool | `true` | Include the `HAProxyControllerConfigPinned` critical rule: HAProxy refused two renders in a row, so the pods keep serving the last configuration it accepted and nothing new reaches them until the input is fixed |
| `controller.monitoring.prometheusRule.defaultRules.haproxyPodsRejected` | bool | `true` | Include the `HAProxyControllerHAProxyPodsRejected` warning rule: discovered HAProxy pods refused admission (often a HAProxy major.minor mismatch with `haproxyVersion`) |
| `controller.monitoring.prometheusRule.defaultRules.noHAProxyPods` | bool | `true` | Include the `HAProxyControllerNoHAProxyPods` critical rule: the controller finds no HAProxy pods to manage, so no config reaches the data plane |
| `controller.monitoring.prometheusRule.defaultRules.accessLogDropped` | bool | `true` | Include the `HAProxyAccessLogRecordsDropped` warning rule: HAProxy discarded access-log records because the Vector sidecar stopped draining the Unix datagram socket. Traffic is unaffected; the log is incomplete |
| `controller.monitoring.prometheusRule.defaultRules.criticalEventsDropped` | bool | `true` | Include the `HAProxyControllerCriticalEventsDropped` critical rule: a critical event-bus subscriber's buffer overflowed — reconciliation work was lost and the data plane may be stale |
| `controller.monitoring.prometheusRule.defaultRules.applyRejected` | bool | `true` | Include the `HAProxyAgentApplyRejected` warning rule: an HAProxy pod refused an apply and serves its last known good file set; HAProxy's message is in the pod's status condition |
| `controller.monitoring.prometheusRule.defaultRules.agentInvariantViolated` | bool | `true` | Include the `HAProxyAgentInvariantViolated` critical rule: an agent observed one of its own invariants failing — a defect, not an operator error |
| `controller.monitoring.prometheusRule.defaultRules.recoveryReloadFailed` | bool | `true` | Include the `HAProxyAgentRecoveryReloadFailed` critical rule: a pod rolled back to its last known good file set but the recovery reload failed, so its worker matches neither the rejected apply nor the restored files |
| `controller.monitoring.prometheusRule.defaultRules.agentVersionSkew` | bool | `true` | Include the `HAProxyAgentVersionSkew` warning rule: a pod's agent doesn't match the controller, so some or all applies reload; expected during a rolling upgrade, a defect after one |
| `controller.monitoring.grafanaDashboard.enabled` | bool | `false` | Create a ConfigMap holding the controller and traffic Grafana dashboard JSON (picked up by the Grafana sidecar via the configured discovery label) |
| `controller.monitoring.grafanaDashboard.labels` | map | `{grafana_dashboard: "1"}` | Discovery labels for the Grafana sidecar |
| `controller.monitoring.grafanaDashboard.annotations` | map | `{grafana_folder: "HAProxy"}` | Annotations on the dashboard ConfigMap; the folder annotation must match `grafana.sidecar.dashboards.folderAnnotation` |
| `controller.monitoring.grafanaDashboard.namespace` | string | `""` | Namespace for the dashboard ConfigMap (defaults to release namespace) |
| `controller.monitoring.grafanaDashboard.useBuiltIn` | bool | `true` | Use the built-in controller and traffic dashboards; set `false` to provide one JSON document via `customDashboard` |
| `controller.monitoring.grafanaDashboard.customDashboard` | map | `{}` | Custom dashboard JSON, only consulted when `useBuiltIn: false` |

## HAProxy deployment

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `haproxy.enabled` | bool | `true` | Deploy HAProxy pods with this chart |
| `haproxy.replicaCount` | int | `2` | Number of HAProxy replicas |
| `haproxy.podDisruptionBudget` | object | enabled `true`, `maxUnavailable` derived | Preserve at least one HAProxy pod during voluntary disruptions. The default is `0` for a one-pod minimum fleet and `1` otherwise. An explicit value must be smaller than the static replica count, or KEDA's minimum replica count when autoscaling is enabled |
| `haproxy.image.repository` | string | `""` (derived) | HAProxy image repository. Empty selects `haproxytech/haproxy-debian` or `hapee-registry.haproxy.com/haproxy-enterprise` from `haproxy.enterprise.enabled` |
| `haproxy.image.pullPolicy` | string | `IfNotPresent` | Image pull policy |
| `haproxy.image.tag` | string | `""` | HAProxy image tag; empty = derive from `haproxyVersion` plus the matching entry in `haproxyPatchVersions` (for example `3.2` → whichever 3.2.x patch the chart currently pins). Override to pin a specific patch yourself. |
| `haproxy.enterprise.enabled` | bool | `false` | Use HAProxy Enterprise. `haproxyVersion` selects the compatibility series, image revision map, and binary path together |
| `haproxy.haproxyBin` | string | Auto-detected | HAProxy binary path |
| `haproxy.initialConfig` | string | See values.yaml | HAProxy bootstrap config served until the controller pushes the first rendered config; processed via Helm `tpl`. Keep the `/ready` 503 gate or clients hit an empty backend set — see the [HAProxy deployment guide](./haproxy-deployment.md) |

## HAProxy pod configuration

Configure HAProxy pod placement, metadata, and runtime settings under `haproxy.podSpec.*`.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `haproxy.podSpec.imagePullSecrets` | list | `[]` | Image pull secrets for the HAProxy pod, which pulls both the HAProxy image and the HAPTIC image its agent container runs from. Empty follows `controller.podSpec.imagePullSecrets` |
| `haproxy.podSpec.podAnnotations` | map | `{}` | Extra pod annotations for HAProxy pods (supports template expressions) |
| `haproxy.podSpec.shareProcessNamespace` | bool | `false` | Share process namespace between containers (required for signal-based sidecar reload) |
| `haproxy.podSpec.priorityClassName` | string | `""` | Pod priority class |
| `haproxy.podSpec.terminationGracePeriodSeconds` | int | `90` | Termination grace period |
| `haproxy.podSpec.dnsPolicy` | string | `ClusterFirst` | DNS policy |
| `haproxy.podSpec.dnsConfig` | map | `{}` | DNS config |
| `haproxy.podSpec.hostAliases` | list | `[]` | /etc/hosts entries |
| `haproxy.podSpec.runtimeClassName` | string | `""` | Runtime class (for example gVisor, Kata) |
| `haproxy.podSpec.topologySpreadConstraints` | list | `[]` | Topology spread constraints |
| `haproxy.podSpec.nodeSelector` | map | `{}` | Node selector |
| `haproxy.podSpec.tolerations` | list | `[]` | Tolerations |
| `haproxy.podSpec.affinity` | map | `{}` | Affinity rules |
| `haproxy.podSpec.podSecurityContext` | map | See values.yaml | Pod-level security context (seccomp, sysctls). UIDs auto-derived from `haproxy.enterprise.enabled` |
| `haproxy.sidecars` | list | `[]` | Extra containers for the HAProxy pod, run as regular containers that stop together with HAProxy; the bundled agent, SPOA hub and Vector are native sidecars that outlive it |
| `haproxy.initContainers` | list | `[]` | Init containers for the HAProxy pod, run after the bundled bootstrap copies and before the native sidecars |
| `haproxy.extraVolumes` | list | `[]` | Extra volumes for HAProxy pod |
| `haproxy.extraVolumeMounts` | list | `[]` | Extra volume mounts for HAProxy container |
| `haproxy.extraEnv` | list | `[]` | Extra env vars for the HAProxy container |

## HAProxy ports

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `haproxy.ports.http` | int | `80` | HTTP frontend container port |
| `haproxy.ports.https` | int | `443` | HTTPS frontend container port |
| `haproxy.ports.stats` | int | `8404` | Stats/health page port |
| `haproxy.ports.dataplane` | int | `5555` | Single source of truth for the agent's apply/state API: its listener, the Service, the NetworkPolicy, the container probes, and the controller's connection port |
| `haproxy.ports.agentMetrics` | int | `5557` | The agent's Prometheus endpoint. Scraped by `haproxy.monitoring.podMonitor` through the named container port `agent-metrics`, and allowed from the NetworkPolicy's metrics sources |

## HAProxy Service

The controller renders the user-facing HAProxy Service from these values (the base library's `k8sResources.haproxy-service` template) and owns it via Server-Side Apply — the chart itself only creates the internal agent Service. Changes therefore land when the controller reconciles, not at `helm upgrade` time.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `haproxy.service.type` | string | `NodePort` | HAProxy service type |
| `haproxy.service.annotations` | map | `{}` | Service annotations |
| `haproxy.service.loadBalancerIP` | string | `""` | LoadBalancer IP (when `type: LoadBalancer`) |
| `haproxy.service.loadBalancerSourceRanges` | list | `[]` | CIDR allowlist for LoadBalancer traffic |
| `haproxy.service.loadBalancerClass` | string | `""` | LoadBalancer class (Kubernetes 1.24+ multi-LB) |
| `haproxy.service.externalTrafficPolicy` | string | `""` | `Cluster` or `Local`; `Local` preserves client source IP at the cost of uneven distribution |
| `haproxy.service.internalTrafficPolicy` | string | `""` | `Cluster` or `Local` for in-cluster traffic |
| `haproxy.service.healthCheckNodePort` | int | `""` | Fixed health-check NodePort for `type: LoadBalancer` with `externalTrafficPolicy: Local`; empty lets Kubernetes allocate one |
| `haproxy.service.publishNotReadyAddresses` | bool | `false` | Include not-ready HAProxy pods in the Service endpoints |
| `haproxy.service.http.port` | int | `80` | HTTP service port |
| `haproxy.service.http.nodePort` | int | `30080` | HTTP NodePort |
| `haproxy.service.https.port` | int | `443` | HTTPS service port |
| `haproxy.service.https.nodePort` | int | `30443` | HTTPS NodePort |
| `haproxy.service.stats.port` | int | `8404` | Stats service port |
| `haproxy.service.stats.nodePort` | int | `30404` | Stats NodePort |
| `haproxy.service.extraPorts` | list | `[]` | Additional Service ports (`corev1.ServicePort` shape) appended to the http/https/stats entries — for example a raw TCP frontend declared via a custom `haproxyConfig` snippet. Drop a default entry by setting `haproxy.service.{http,https,stats}.port: 0` |

## HAPTIC agent container

The agent applies configuration inside each HAProxy pod. The chart uses the same
image version for the agent and controller; their versions can differ briefly
during a rolling upgrade.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `haproxy.agent.service.type` | string | `ClusterIP` | Type of the internal Service that fronts the agent port |
| `haproxy.agent.tls.enabled` | bool | `true` | Require mutual TLS for controller-to-agent requests |
| `haproxy.agent.tls.managed` | bool | `true` | Create and automatically renew identities; false uses externally managed Secrets |
| `haproxy.agent.tls.issuerSecretName` | string | `""` | Internal issuer state or cert-manager root Secret; empty derives a release-specific name |
| `haproxy.agent.tls.certManager.enabled` | bool | `false` | Select cert-manager instead of HAPTIC renewal Jobs; requires cert-manager installed |
| `haproxy.agent.tls.certManager.createIssuer` | bool | `true` | Create a self-signed root Certificate and CA Issuer |
| `haproxy.agent.tls.certManager.issuerRef` | object | `{name: "", kind: Issuer, group: cert-manager.io}` | Existing issuer when `createIssuer` is false |
| `haproxy.agent.tls.renewal.resources` | object | requests: `10m` CPU, `32Mi` memory; limit: `128Mi` memory | Internal bootstrap and renewal Job resources |
| `haproxy.agent.tls.serverSecretName` | string | `""` | Server identity Secret; empty derives a release-specific name |
| `haproxy.agent.tls.clientSecretName` | string | `""` | Controller identity Secret; empty derives a release-specific name |
| `haproxy.agent.tls.serverName` | string | `""` | Required server DNS identity; empty derives it from the server Secret name and namespace |
| `haproxy.agent.tls.clientName` | string | `""` | Required controller DNS identity; empty derives it from the client Secret name and namespace |
| `haproxy.agent.tls.certValidityDays` | int | `365` | Identity lifetime, from 1 to 3650 days; also the CA lifetime with internal renewal. Applies at next issuance. See [renewal monitoring](./operations/agent-certificates.md#check-expiry) |
| `haproxy.agent.logLevel` | string | `info` | Agent log level: `trace`, `debug`, `info`, `warning`, or `error`. Use `debug` to inspect each configuration apply, its result, and any reload. |
| `haproxy.agent.resources.requests.cpu` | string | `50m` | Agent CPU request |
| `haproxy.agent.resources.requests.memory` | string | `256Mi` | Agent memory request (matches `limits.memory`) |
| `haproxy.agent.resources.limits.memory` | string | `256Mi` | Agent memory limit |
| `haproxy.agent.extraEnv` | list | `[]` | Extra environment variables for the agent container |

The agent's reload pacing and reload deadline aren't separate values: the chart
templates them from [`controller.config.dataplane.minDeploymentInterval` and
`controller.config.dataplane.reloadVerificationTimeout`](#agent-configuration),
so the controller and the agent can't disagree.

Agent credentials are the top-level `credentials.dataplane.*` section — see [Credentials](#credentials) above.

## HAProxy tuning

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `haproxy.nbthread` | int/string | absent | HAProxy `nbthread` directive value. Default (key absent): if `haproxy.resources.limits.cpu` is set, `ceil(cpu_limit)` clamped to ≥1; otherwise the directive is omitted and HAProxy auto-detects all node cores from its CPU affinity. Set `0` to force-omit the directive. Templatable — `"{{ mul 2 (...) }}"` is allowed |
| `haproxy.shmStats.enabled` | bool | `false` | Persist HAProxy stats counters across reloads via `shm-stats-file` (HAProxy 3.3+ only) |
| `haproxy.shmStats.path` | string | `/dev/shm/haproxy-stats` | Path to the shared-memory stats file |
| `haproxy.shmStats.maxObjects` | int | `50000` | Maximum object count in the shm-stats file. Each frontend, backend, listen, and server counts as one object — pick a value with headroom; HAProxy can't resize the file on reload |
| `haproxy.shmStats.shmSizeLimit` | string | `""` | `/dev/shm` emptyDir size limit. Empty auto-calculates from `maxObjects` (~4 KB/object + 10% overhead, rounded to MiB) |
| `haproxy.drain.enabled` | bool | `true` | Hold the HAProxy container's `preStop` hook on the agent's drain socket until no new connection has reached the pod for `quietPeriodSeconds`; only then does kubelet's SIGUSR1 run HAProxy's own soft stop |
| `haproxy.drain.quietPeriodSeconds` | int | `2` | The drain ends once the traffic frontends accepted no new connection for this many seconds. A client that pauses longer while kube-proxy still routes to the pod can be refused; see [Pod termination](haproxy-deployment.md#pod-termination) |
| `haproxy.drain.maxWaitSeconds` | int | `10` | Upper bound of the drain in seconds; with `extraContext.hardStopAfter` and the sidecars' shutdown it must fit in `terminationGracePeriodSeconds` |
| `haproxy.lifecycle` | map | `{}` | Container lifecycle hooks for the HAProxy container (`preStop`, `postStart`); when set they replace the drain hook |
| `haproxy.updateStrategy.type` | string | `RollingUpdate` | HAProxy deployment update strategy |
| `haproxy.updateStrategy.rollingUpdate.maxSurge` | int/string | `1` | Maximum surge during rolling updates |
| `haproxy.updateStrategy.rollingUpdate.maxUnavailable` | int/string | `0` | Maximum unavailable during rolling updates |
| `haproxy.minReadySeconds` | int | `0` | Minimum seconds a new HAProxy pod must be ready before it counts as available |
| `haproxy.revisionHistoryLimit` | int | `10` | Number of old ReplicaSets to retain |

## HAProxy KEDA autoscaling

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `haproxy.keda.enabled` | bool | `false` | Enable KEDA `ScaledObject` for HAProxy pods (event-driven autoscaling) |
| `haproxy.keda.minReplicaCount` | int | `2` | Minimum HAProxy replica count |
| `haproxy.keda.maxReplicaCount` | int | `10` | Maximum HAProxy replica count |
| `haproxy.keda.pollingInterval` | int | `30` | KEDA trigger polling interval in seconds |
| `haproxy.keda.cooldownPeriod` | int | `300` | Seconds to wait before scaling down |
| `haproxy.keda.fallback.failureThreshold` | int | `3` | Consecutive trigger failures before falling back |
| `haproxy.keda.fallback.replicas` | int | `2` | Replica count to fall back to on trigger failure |
| `haproxy.keda.advanced` | map | `{}` | Advanced HPA `behavior` overrides (scale-up/-down stabilisation windows, etc.) |
| `haproxy.keda.triggers` | list | required | Scaling triggers; see KEDA docs for the per-source schema (Prometheus, CPU, cron, …) |

## HAProxy monitoring

One `PodMonitor` for every metrics endpoint on the HAProxy pod. See [Where to scrape](operations/monitoring.md#where-to-scrape).

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `haproxy.monitoring.podMonitor.enabled` | bool | `false` | Create a PodMonitor for HAProxy, the agent, and enabled sidecar metrics endpoints. Requires Prometheus Operator CRDs. See [Where to scrape](operations/monitoring.md#where-to-scrape) |
| `haproxy.monitoring.podMonitor.interval` | duration | `30s` | Scrape interval, applied to every endpoint |
| `haproxy.monitoring.podMonitor.scrapeTimeout` | duration | `10s` | Scrape timeout, applied to every endpoint |
| `haproxy.monitoring.podMonitor.labels` | map | `{}` | Extra labels on the `PodMonitor` (for a Prometheus `podMonitorSelector`) |
| `haproxy.monitoring.podMonitor.relabelings` | list | `[]` | `relabelings` applied to every endpoint |
| `haproxy.monitoring.podMonitor.metricRelabelings` | list | `[]` | `metricRelabelings` applied to every endpoint |

## SPOA hub sidecar

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `spoaHub.enabled` | bool/null | `null` | Master enable. `null` auto-derives from `spoaHub.plugins.*.enabled`. Set `true` to force the sidecar on with no plugins enabled (test rendering); set `false` to force it off even when a plugin is enabled |
| `spoaHub.image.repository` | string | `ghcr.io/haproxy-haptic/haptic/spoa-hub` | SPOA hub image repository |
| `spoaHub.image.pullPolicy` | string | `IfNotPresent` | Image pull policy |
| `spoaHub.image.tag` | string | `""` | Override the tag; empty uses `.Chart.AppVersion` |
| `spoaHub.resources.requests.cpu` | string | `50m` | SPOA hub CPU request |
| `spoaHub.resources.requests.memory` | string | `128Mi` | SPOA hub memory request |
| `spoaHub.resources.limits.memory` | string | `256Mi` | SPOA hub memory limit |
| `spoaHub.hub.logLevel` | string | `info` | Hub log level (`info`/`debug`/`warn`/`error`) |
| `spoaHub.hub.workerThreads` | int/null | `null` | Tokio worker thread count; `null` defaults to CPU count |
| `spoaHub.hub.maxConnections` | int | `1000` | Maximum concurrent connections per listener |
| `spoaHub.hub.blockingThreadKeepAliveSecs` | int | `30` | Keep-alive seconds for blocking-thread workers |
| `spoaHub.hub.maxBlockingThreads` | int/null | `null` | Process-wide blocking-pool cap. Null derives the sum of resolved per-plugin concurrency; an explicit value must be at least that sum. Changing it rolls the HAProxy pods because Tokio fixes this pool at process start |
| `spoaHub.hub.reloadDrainTimeoutMs` | int/null | `null` | Hot-reload quiesce-and-drain budget. Null derives 1.5 times the largest plugin timeout, clamped to 1–30 seconds. `0` restores unsafe legacy immediate retirement and can lose in-flight/background work |
| `spoaHub.hub.metricsAddr` | string | `auto` | Metrics listen address. `auto` uses `127.0.0.1:9095` when Vector re-exports the metrics, otherwise `0.0.0.0:9095` for direct scraping. Set an explicit address to override, or `""` to disable. |
| `spoaHub.hub.goGCPercent` | int | `300` | Coraza garbage collection target percentage. Lower values trade more CPU work for more frequent memory reclamation |
| `spoaHub.haproxy.socketPath` | string | `/run/spoa/hub.sock` | Unix socket path shared between HAProxy and the hub |
| `spoaHub.haproxy.modeSpop` | bool | `true` | Use HAProxy 3.1+ `mode spop` backend; auto-falls back to `mode tcp` on 3.0. Set `false` to force `mode tcp` on 3.1+ |
| `spoaHub.haproxy.timeoutHello` | duration | `2s` | Stream Processing Offload Engine (SPOE) hello timeout |
| `spoaHub.haproxy.timeoutIdle` | duration | `5m` | SPOE idle timeout |
| `spoaHub.haproxy.timeoutProcessing` | duration/null | `null` | HAProxy's outer per-message processing timeout. Null derives each enabled message's budget plus `timeoutProcessingMarginMs`; a message budget sums all plugin timeouts sharing that message, covering sequential dependency stages without adding unrelated plugins. An explicit value applies to every message and fails rendering when it's below any enabled message's minimum |
| `spoaHub.haproxy.timeoutProcessingMarginMs` | int | `100` | Scheduling and serialization margin added between each enabled message's plugin budget and its HAProxy deadline |
| `spoaHub.haproxy.poolMaxConn` | int | `100` | Connection pool maximum |
| `spoaHub.haproxy.poolPurgeDelay` | duration | `30s` | Idle-connection purge delay |
| `spoaHub.plugins.<name>.enabled` | bool/string | `'{{ false }}'` (templatable) | Per-plugin enable. Default value is a chart-evaluated `tpl` string so a plugin can auto-enable when the template libraries that rely on it are on; explicit `--set` bool always wins |
| `spoaHub.plugins.<name>.timeoutMs` | int | per-plugin | Plugin processing timeout in milliseconds |
| `spoaHub.plugins.<name>.maxConcurrency` | int/null | plugin default | Maximum plugin calls executing concurrently. Use this as the single owner of plugin CPU admission |
| `spoaHub.plugins.<name>.maxQueue` | int/null | plugin default | Maximum calls waiting for a concurrency slot. Coraza defaults to `0`, rejecting excess work instead of inflating latency under attack |
| `spoaHub.plugins.<name>.queueTimeoutMs` | int/null | plugin default | Maximum queue wait when `maxQueue` is non-zero |
| `spoaHub.plugins.<name>.adaptiveConcurrency` | bool | plugin default (on for coraza/api-gateway) | Adjust plugin concurrency from measured latency, up to `maxConcurrency`. Requires a hub image with adaptive concurrency support. |
| `spoaHub.plugins.<name>.messages` | list | per-plugin | SPOE messages this plugin handles |
| `spoaHub.plugins.<name>.dependsOn` | list | `[]` | Other plugin names this plugin must run after |
| `spoaHub.plugins.<name>.params` | string | per-plugin | Free-form TOML blob spliced verbatim under `[plugins.params]` — use dotted keys (`x.y = "..."`) or fully qualified headers (`[plugins.params.x]`) for nested values; bare `[x]` headers close the params scope and break the config |
| `spoaHub.plugins.coraza.directives` | string | OWASP CRS includes + `SecRuleEngine On` | Chart-wide Coraza WAF directives. Reusable policy applications and authorized per-Ingress rules layer on this base. Keep `SecRuleEngine On` *after* the includes — `@coraza.conf-recommended` sets `DetectionOnly`, so an earlier `On` is silently overridden and the WAF never blocks. Don't also set `directives` inside `params:`; the duplicate TOML field breaks the config |
| `spoaHub.plugins.mirror.targetTimeoutMs` | int | `2000` | Per-target timeout for asynchronous mirror requests. It's independent of application backend timeouts so a dead mirror releases shared hub capacity quickly |
| `spoaHub.plugins.mirror.targetRetries` | int | `0` | Retry count for asynchronous mirror requests. The default avoids multiplying work against an unavailable observability target |
| `spoaHub.securityContext` | map | See values.yaml | Container security context for the spoa-hub container. Default runs user and group 99, matching the pod's `fsGroup`, so the Unix socket the hub creates under `/run/spoa` is accessible to the HAProxy container; read-only root filesystem, no privilege escalation, all capabilities dropped |
| `spoaHub.extraVolumeMounts` | list | `[]` | Extra volume mounts added to the spoa-hub container only (rendered through `tpl`) — for MMDB files (`maxmind`), OpenID Connect (OIDC) client secrets (`sso-auth`), and similar plugin data |

Available plugin names (`<name>`): `api-gateway`, `coraza`, `external-auth`, `fingerprinting`, `maxmind`, `mirror`, `rate-limit`, `sso-auth`. See `values.yaml` for each plugin's defaults and the upstream plugin README for the `params:` schema.

## Vector sidecar

A [Vector](https://vector.dev) container on every HAProxy pod. It receives the access log over a Unix datagram socket, derives [per-request metrics](operations/monitoring.md#request-metrics) from it, and re-exports the SPOA hub's Prometheus metrics alongside its own. HAProxy's own exporter is scraped directly (see [Prometheus exporter](#prometheus-exporter)). See [Access logging](operations/access-logging.md).

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `vector.enabled` | bool | `true` | Run the Vector sidecar on each HAProxy pod. When enabled, the access log goes to `vector.socketPath` instead of stdout and surfaces under `kubectl logs <pod> -c vector`. Set to `false` to log straight to the HAProxy container's stdout and scrape the hub directly; HAProxy's exporter is scraped directly either way |
| `vector.image.repository` | string | `timberio/vector` | Vector image repository |
| `vector.image.pullPolicy` | string | `IfNotPresent` | Image pull policy |
| `vector.image.tag` | string | `0.60.0-debian` | Pinned Vector version. An empty tag is rejected |
| `vector.metricsPort` | int | `9598` | Port serving Vector's `/metrics`: its own series, the log-derived and request metrics, and the hub's re-exported ones. Rejected at render time if it collides with an `haproxy.ports.*` entry or the hub's metrics port |
| `vector.sizeMetricsPort` | int | `9599` | Exporter port for request and response size histograms, with byte buckets separate from duration buckets. Present only when either size family is enabled; must not collide with another metrics or HAProxy port |
| `vector.socketPath` | string | `/run/vector/haproxy.sock` | Unix datagram socket HAProxy writes access-log records to. Must be an absolute path with no whitespace — HAProxy's `log <path>` form requires one |
| `vector.omitEmptyLogFields` | bool | `true` | Remove empty string fields from access logs while preserving field order and numeric zeros. Set `false` if your log schema requires every field or your queries match empty strings |
| `vector.logMetrics` | map | eight entries, all on | Access-log metrics keyed by name, so custom entries preserve defaults. Each entry requires `enabled`, `field`, `metric`, and `kind`. Use `enum` with allowed `values` and an optional `tag` (default `value`), or `numeric` to increment by the field value. Optional `requires` names a Helm values path that must be truthy. Names are validated at render time. See [Log-derived metrics](operations/monitoring.md#haproxy-data-plane-metrics) |
| `vector.requestMetrics.enabled` | bool | `true` | Emit one counter and six histograms from access logs, with route labels and separate upstream timing phases. See [Request metrics](operations/monitoring.md#request-metrics) |
| `vector.requestMetrics.prefix` | string | `haptic_ingress_controller` | Metric namespace. Set `nginx_ingress_controller` for existing ingress-nginx dashboards, then check [metric and label differences](migrating.md#metrics). A trailing underscore is stripped |
| `vector.requestMetrics.controllerClass` | string | `""` | Value of the `controller_class` label. Empty means `ingressClass.controllerName`. `ingress-nginx` puts its `--controller-class` here (`k8s.io/ingress-nginx`), so set that if a dashboard selects on it |
| `vector.requestMetrics.terminationStateLabel` | bool | `true` | Add HAProxy's four-character termination state as `term` to every family. It distinguishes connection failures, timeouts, and aborts. Set `false` to reduce series count |
| `vector.requestMetrics.pathLabel` | bool | `true` | Add `path`, the matched **route** — the path template you wrote, so it's bounded by the number of rules rather than by traffic. Turning it off also switches off the HAProxy-side route lookup, saving four map lookups per request as well as series |
| `vector.requestMetrics.hostLabel` | bool | `true` | Add `host`. The equivalent of `ingress-nginx`'s `--metrics-per-host` |
| `vector.requestMetrics.serverLabel` | bool or `counters` | `false` | Add HAProxy's selected server name as `server`: `true` to every request metric, `counters` to `requests_total` only. Matches the native exporter's `server` label; empty when no server was selected. Each target adds series per route and label combination, and on the histograms one per bucket |
| `vector.requestMetrics.durationBuckets` | list | 15 boundaries, 1 ms to 60 s | Positive, strictly ascending boundaries in seconds for the four duration histograms. Defaults include ingress-nginx's time buckets, plus 1 ms, 2.5 ms, 30 s, and 60 s |
| `vector.requestMetrics.sizeBuckets` | list | 12 boundaries, 100 B to 100 MB | Boundaries in bytes for the two size histograms. These differ from ingress-nginx's buckets; queries using `_sum` and `_count` don't depend on bucket boundaries |
| `vector.requestMetrics.cardinalityLimit.enabled` | bool | `true` | Limit distinct label values per metric. Applies to request metrics; state resets when Vector restarts. Investigate a tripped limit for unexpected label values |
| `vector.requestMetrics.cardinalityLimit.valueLimit` | int | `500` | Distinct values allowed per label per metric before the limit trips |
| `vector.requestMetrics.cardinalityLimit.action` | string | `drop_tag` | Action when a label exceeds its limit. `drop_tag` removes that label from subsequent observations while retaining totals; `drop_event` discards those observations |
| `vector.requestMetrics.metrics` | map | seven, all on | Enable individual metric families. Unknown keys and an all-disabled set are rejected. Request size counts body bytes, unlike ingress-nginx's request-line-plus-headers measurement. Upstream timing phases contribute only when they occurred. See [Request metrics](operations/monitoring.md#request-metrics) |
| `vector.scrapeIntervalSecs` | int | `15` | How often Vector scrapes the hub endpoint it re-exports. Keep at or below Prometheus's own interval, or Prometheus samples a value Vector hasn't refreshed |
| `vector.resources.requests.cpu` | string | `50m` | CPU request for the Vector container |
| `vector.resources.requests.memory` | string | `256Mi` | Memory request. Size for the number of metric series; lower `cardinalityLimit.valueLimit` or disable labels to reduce demand |
| `vector.resources.limits.memory` | string | `1Gi` | Memory limit. The supervisor restarts an exited or unresponsive Vector child; a whole-container OOM can briefly affect pod readiness |
| `vector.securityContext.allowPrivilegeEscalation` | bool | `false` | Container security context for Vector |
| `vector.securityContext.readOnlyRootFilesystem` | bool | `true` | Read-only root filesystem; Vector's writable paths are the `data_dir` and `/tmp` emptyDir volumes |
| `vector.securityContext.runAsNonRoot` | bool | `true` | Refuse to run as root |
| `vector.securityContext.capabilities.drop` | list | `[ALL]` | Linux capabilities to drop |
| `vector.extraVolumeMounts` | list | `[]` | Extra volume mounts added to the Vector container only (rendered through `tpl`) — for credentials a downstream sink needs |

## HAProxy resources & scheduling

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `haproxy.resources.requests.cpu` | string | `250m` | CPU request |
| `haproxy.resources.requests.memory` | string | `1Gi` | Memory request (matches `limits.memory`) |
| `haproxy.resources.limits.memory` | string | `1Gi` | Memory limit |

No CPU limit is set by default to avoid throttling. With no limit, HAProxy's `nbthread` auto-detects all node cores from its CPU affinity — so HAProxy uses every core on a static node without inflating CPU requests. Set `haproxy.resources.limits.cpu` to cap both the CPU quota and `nbthread` to `ceil(limit)`.

## HAProxy NetworkPolicy

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `haproxy.networkPolicy.enabled` | bool | `true` | Enable HAProxy NetworkPolicy |
| `haproxy.networkPolicy.allowExternal` | bool | `true` | Allow external traffic |
| `haproxy.networkPolicy.allowedSources` | list | `[]` | Allowed traffic sources (when allowExternal=false) |
| `haproxy.networkPolicy.extraIngress` | list | `[]` | Additional ingress rules |
| `haproxy.networkPolicy.extraEgress` | list | `[]` | Additional egress rules |

## Controller NetworkPolicy

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `controller.networkPolicy.enabled` | bool | `true` | Enable controller NetworkPolicy |
| `controller.networkPolicy.egress.allowDNS` | bool | `true` | Allow DNS resolution |
| `controller.networkPolicy.egress.kubernetesApi` | list | See values.yaml | Kubernetes API access rules |
| `controller.networkPolicy.egress.haproxyPods.enabled` | bool | `true` | Allow controller egress to the agent on each HAProxy pod (release namespace unless `namespaceSelector` is set) |
| `controller.networkPolicy.egress.haproxyPods.podSelector` | map | See values.yaml | Pod-label selector matching the HAProxy pods to reach |
| `controller.networkPolicy.egress.haproxyPods.namespaceSelector` | map | `{}` | Namespace selector. **`{}` emits no selector**, restricting the rule to the release namespace — set `matchLabels` to reach HAProxy pods in other namespaces |
| `controller.networkPolicy.egress.additionalRules` | list | See values.yaml | Additional egress rules; the chart default allows egress to every in-cluster pod (keeps `http.Fetch()` working) — set `[]` to lock down |
| `controller.networkPolicy.ingress.monitoring.enabled` | bool | `false` | Allow Prometheus scraping |
| `controller.networkPolicy.ingress.monitoring.podSelector` | map | `{}` | Prometheus pod selector. **`{}` means every pod** — set `matchLabels` to identify your Prometheus deployment |
| `controller.networkPolicy.ingress.monitoring.namespaceSelector` | map | `{}` | Prometheus namespace selector. **`{}` emits no selector**, so only same-namespace scrapers match — set `matchLabels` to admit your monitoring namespace |
| `controller.networkPolicy.ingress.healthChecks.enabled` | bool | `true` | Allow health check access |
| `controller.networkPolicy.ingress.healthChecks.from` | list | `[{podSelector: {}}]` | NetworkPolicy peers allowed to reach the health port (`controller.ports.healthz`). The default `podSelector: {}` admits every pod in the release namespace |
| `controller.networkPolicy.ingress.webhook.enabled` | bool | `true` | Allow webhook access |
| `controller.networkPolicy.ingress.webhook.from` | list | `[]` | NetworkPolicy peers allowed to reach the webhook port (`controller.ports.webhook`). Empty omits `from`, so any source may connect: the kube-apiserver often calls the webhook from the host network, which pod and namespace selectors don't match and Cilium doesn't match against `ipBlock`. List your apiserver/node peers to restrict it for production |
| `controller.networkPolicy.ingress.additionalRules` | list | `[]` | Additional ingress rules |

## See also

- [Deploying with Helm](./deploying-with-helm.md) — install, upgrade, and a task-based tour of the chart
- [Template Libraries](./template-libraries.md) — what each `controller.templateLibraries.*` toggle loads
- [CRD Reference](./crd-reference.md) — every field of the `HAProxyTemplateConfig` the chart renders from `controller.config`

## Value ownership and upgrade migration

Use the current value paths below when upgrading. The chart rejects obsolete
paths to keep process settings, Services, and generated configuration consistent.
See the [0.2 upgrade notes](upgrade-notes.md#upgrading-to-02) for the full procedure.
Rows marked (0.3) changed in 0.3; see the [0.3 upgrade notes](upgrade-notes.md#upgrading-to-03).

| Previous value | Authoritative value |
|----------------|---------------------|
| `controller.crdName` | `controller.configName` |
| `controller.debugPort` | `controller.ports.healthz` |
| `controller.config.controller.healthzPort` | `controller.ports.healthz` |
| `controller.config.controller.metricsPort` or `controller.extraEnv[].name=METRICS_PORT` | `controller.ports.metrics` (`0` disables metrics) |
| `controller.config.dataplane.port` | `haproxy.ports.dataplane` |
| `haproxy.dataplane.logLevel` | `haproxy.agent.logLevel` |
| `haproxy.dataplane.resources` | `haproxy.agent.resources` |
| `haproxy.dataplane.extraEnv` | `haproxy.agent.extraEnv` |
| `haproxy.dataplane.service` | `haproxy.agent.service` |
| `haproxy.dataplane.validateConfig` | Removed. The pod's own HAProxy binary judges the configuration at reload, and the webhook and the config-load gate still run the full `haproxy -c` |
| `haproxy.dataplane.debugSocketPath` | Removed. Profile the agent through its own metrics and `GET /v1/state` |
| `haproxy.dataplane.aclFormat` | Removed. It formatted the Data Plane API's own access log; the agent logs one structured line per apply instead |
| `haproxy.dataplaneBin` | Removed. The agent is the controller's binary, in the controller's image |
| `controller.config.routing.regexMatchOrder` | `controller.config.templatingSettings.extraContext.routing.regexMatchOrder` |
| `controller.defaultSSLCertificate` | `defaultSSLCertificate` |
| `haproxy.enterprise.version` | `haproxyVersion` |
| Root-level controller workload values (`replicaCount`, `image`, `deploymentAnnotations`, `webhook`, `monitoring`, `networkPolicy`, `autoscaling`, `podDisruptionBudget`, `service`, `serviceAccount`, `rbac`, `securityContext`, `resources`, probes, rollout, and extras) | The same key under `controller.*` (for example `controller.replicaCount`) |
| `controller.config.templatingSettings.extraContext.debug` | `controller.config.templatingSettings.extraContext.diagnostics.routingHeaders.enabled` (now defaults to `false`) |
| `controller.statusPatches.enabled` and `controller.config.templatingSettings.extraContext.statusPatchesDisabled` | `controller.config.templatingSettings.extraContext.statusPatches.enabled` (inverted: `statusPatchesDisabled: true` becomes `enabled: false`) |
| `controller.config.templatingSettings.extraContext.password_hash_validation_regex` and `…password_hash_validation_error_message` | `controller.config.templatingSettings.extraContext.annotationCompatibility.basicAuth.passwordHashValidation.regex` and `.errorMessage` |
| `controller.config.templatingSettings.extraContext.hstsEnabled`, `hstsMaxAge`, `hstsIncludeSubdomains`, `hstsPreload` | `controller.config.templatingSettings.extraContext.tls.hsts.enabled`, `.maxAge`, `.includeSubdomains`, `.preload` |
| `controller.config.templatingSettings.extraContext.hapticHstsMaxAge` (0.3) | `controller.config.templatingSettings.extraContext.tls.hsts.maxAge`, which also sets the global HSTS header's `max-age` |
| `controller.config.templatingSettings.extraContext.timeout_connect`, `timeout_client`, `timeout_server`, `timeout_http_request`, `timeout_http_keep_alive` (0.3) | `controller.config.templatingSettings.extraContext.timeoutConnect`, `timeoutClient`, `timeoutServer`, `timeoutHttpRequest`, `timeoutHttpKeepAlive` |
| `controller.config.templatingSettings.extraContext.ssl_redirect_default` (0.3, string `"true"`) | `controller.config.templatingSettings.extraContext.sslRedirectDefault` (boolean `true`) |
| `vector.excludeMaintServerMetrics` | `controller.config.templatingSettings.extraContext.prometheusExporter.excludeMaintServers` — HAProxy applies `?no-maint` itself, for every scraper |
| `vector.excludeMetrics` | `controller.config.templatingSettings.extraContext.prometheusExporter.excludeMetrics` — same entry names, `enabled`, `families` and `requires`; `pattern` is gone, HAProxy's exporter filters by exact family name |
| `vector.podMonitor` and `spoaHub.monitoring.podMonitor` | `haproxy.monitoring.podMonitor` — one PodMonitor for every metrics endpoint on the HAProxy pod |

Cache and rate-limit settings introduced after the previous release use their
final ownership from the start: `cache.varnish` owns the Varnish workload,
`cache.haproxy` owns HAProxy cache integration, `rateLimit.shared` owns the
feature, and `rateLimit.shared.managedStore` owns the optional bundled Valkey
topology. Plugin execution remains under `spoaHub.plugins.*`.
