# Migrating to HAPTIC

Move Ingresses from **ingress-nginx**, **haproxy-ingress**, or
**haproxytech/kubernetes-ingress** to HAPTIC. Check annotation compatibility,
install HAPTIC alongside the existing controller, and test routes before
transferring production traffic.

With default values, HAPTIC uses the `haptic` IngressClass and its own HAProxy
Service. It watches Ingresses that select that class. Review class names and
watch filters if you've customized an existing HAPTIC installation.

Keep the old controller available until routing and DNS changes are verified.
See [troubleshooting](#troubleshooting) for differences that commonly affect a migration.

## Before you start

- Your incumbent controller (ingress-nginx / haproxy-ingress / haproxytech kubernetes-ingress) is still running and serving traffic. **Leave it running** until cutover is complete.
- You have Helm and cluster access. Step 1 includes commands for a new installation and for an existing HAPTIC release.
- You can edit Ingress manifests (to change `ingressClassName`) or you accept renaming HAPTIC's class to match — see below.

<a id="step-0-check-what-will-change"></a>

## Step 0: Check what changes

Run the migration report before changing routes. It classifies source-controller
annotations as supported, different, or dropped, and renders the Ingress through
HAPTIC's template engine.

It runs in your browser, on your own manifests. Paste the Ingresses you want to
audit into the **Resources** panel — `kubectl get ingress -A -o yaml` output
works as-is — and read the **migration** tab. Nothing leaves your machine and
nothing touches your cluster. From ingress-nginx, also paste the controller
ConfigMap: the report classifies each of its keys against the
[controller ConfigMap table](#controller-configmap-settings).

The same report runs live below on a preset ingress-nginx setup:

<div class="pg-embed" markdown data-scenario="nginx-ingress" data-facade="resources" data-input="resources" data-input-focus="nginx.ingress.kubernetes.io/proxy-connect-timeout" data-tab="migration" data-controls="tabs,resources" data-title="ingress-nginx annotation migration report" data-height="440">

<p class="pg-task" markdown>In the **Resources** panel, add <code>nginx.ingress.kubernetes.io/server-snippet: "more_set_headers X-From: nginx;"</code> to the `shop` Ingress, then watch a new **dropped** verdict appear in the **migration** report.</p>

<details class="pg-hint" markdown>
<summary>What to expect</summary>

The report marks `server-snippet` as **dropped** because nginx server-level
directives have no HAProxy equivalent. Replace that behavior before migrating
a route that depends on it.

</details>

</div>

Read the verdicts the same way whatever you paste in:

- **supported** — works the same after migrating.
- **different** — carries over, but behaves differently; read the note.
- **dropped** — has no HAProxy equivalent and doesn't carry over.
- A **render failure** means HAPTIC can't build a configuration for that
  Ingress as-is. Fix those before cutover.

For the full editor — more resources, every template, the rendered HAProxy
config — open the [playground](/playground/).

## The cutover, step by step

!!! warning "Plan the traffic transition"
    Changing `ingressClassName` removes the route from the old controller before
    DNS changes take effect. This procedure can interrupt traffic. Use a test
    Ingress first and schedule production changes for an acceptable interruption
    window. For continuous service, keep a separate route on the old controller
    until clients have moved to HAPTIC.

1. **Install HAPTIC alongside** your existing controller. Give HAProxy a real
   external address. This example uses `LoadBalancer`, which requires a load-balancer
   implementation in your cluster. A NodePort behind your existing load balancer
   also works; see [Service access](haproxy-deployment.md#haproxy-service):

    ```bash
    helm install haptic oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
      --version 0.9.0 \
      --namespace haptic --create-namespace \
      --set haproxy.service.type=LoadBalancer \
      --set controller.config.templatingSettings.extraContext.statusPatches.enabled=false   # Hold route status until verification
    ```

    If HAPTIC is already installed, apply the same flags with `helm upgrade`:

    ```bash
    helm upgrade haptic oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
      --version 0.9.0 \
      --namespace haptic --reuse-values \
      --set haproxy.service.type=LoadBalancer \
      --set controller.config.templatingSettings.extraContext.statusPatches.enabled=false   # Hold route status until verification
    ```

2. **Enable the right annotation library** for your source controller — see
   [From ingress-nginx](#from-ingress-nginx),
   [From haproxy-ingress](#from-haproxy-ingress), or
   [From haproxytech/kubernetes-ingress](#from-haproxytechkubernetes-ingress).

3. **Move one test Ingress** to HAPTIC by changing only its class:

    ```bash
    kubectl patch ingress my-test-app \
      --type merge -p '{"spec":{"ingressClassName":"haptic"}}'
    ```

    The incumbent controller drops it; HAPTIC picks it up. Verify routing
    with a local port forward before changing DNS:

    ```bash
    kubectl port-forward --namespace haptic service/haptic-haproxy 8080:80
    ```

    In another terminal, enter the hostname from the test Ingress:

    ```bash
    read -r -p "Test Ingress hostname: " test_hostname
    curl -i --header "Host: $test_hostname" http://127.0.0.1:8080/
    ```

    Check the application response and any authentication, redirects, or headers
    your route requires. Stop the port forward when you finish.

4. **Bulk cut over** once you're confident: change `ingressClassName` on the
   remaining Ingresses (in batches you can roll back).

5. **Enable status writes, then flip DNS.** Turn status patches back on (step 1
   installed with them off) so `external-dns` and dashboards see HAPTIC's address:

    ```bash
    helm upgrade haptic oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
      --version 0.9.0 \
      --namespace haptic --reuse-values \
      --set controller.config.templatingSettings.extraContext.statusPatches.enabled=true
    ```

    Then repoint DNS to HAPTIC's load balancer if you manage it manually. Watch
    traffic.

6. **Decommission** the old controller once all Ingresses are served by HAPTIC
   and traffic is stable.

!!! tip "Rolling back"
    Restore the original `ingressClassName` to return route ownership to the old
    controller. If DNS has changed, restore its previous address too and account
    for cached records. Keep the old controller installed until step 6.

## Key settings that affect migration

| Setting | Default | Why it matters |
|---------|---------|----------------|
| `ingressClass.name` | `haptic` | Only Ingresses with this exact `ingressClassName` are served. |
| `ingressClass.default` | `false` | Class-less Ingresses **aren't** adopted; leave `false` during migration. |
| `controller.templateLibraries.hapticAnnotations.enabled` | `true` | Native `haproxy-haptic.org/*` annotations — on by default and the recommended target vocabulary once you've migrated. |
| `controller.templateLibraries.nginxIngress.enabled` | `false` | Opt-in: turn on to keep `nginx.ingress.kubernetes.io/*` annotations working. |
| `controller.templateLibraries.haproxyIngress.enabled` | `false` | Opt-in: turn on to keep `haproxy-ingress.github.io/*` annotations working. |
| `controller.templateLibraries.haproxytech.enabled` | `false` | Opt-in: turn on to keep `haproxy.org/*` annotations working. |
| `controller.config.templatingSettings.extraContext.statusPatches.enabled` | `true` | Writes Ingress/Gateway status. Disable temporarily if a DNS controller would move traffic before verification; this affects all routes in the release. |
| `haproxy.service.type` | `NodePort` | Set to `LoadBalancer` for a routable external address. |

---

## From ingress-nginx

### Match the IngressClass

HAPTIC serves Ingresses whose `spec.ingressClassName` matches its configured
class. For a gradual migration, change each selected Ingress to `haptic`:

```bash
kubectl patch ingress my-test-app --type merge \
  -p '{"spec":{"ingressClassName":"haptic"}}'
```

Keep HAPTIC's class distinct while both controllers run. Reusing an incumbent's
class name also requires transferring ownership of the existing IngressClass;
changing the Helm value alone doesn't perform that transfer.

Keep `ingressClass.default: false` during migration. A
[default IngressClass](https://kubernetes.io/docs/concepts/services-networking/ingress/#default-ingress-class)
is assigned to new Ingresses that omit a class; it doesn't migrate existing
class-less resources.

### Enable the annotation library

```bash
helm upgrade haptic oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
  --version 0.9.0 --namespace haptic --reuse-values \
  --set controller.templateLibraries.nginxIngress.enabled=true
```

!!! warning "The flag is `camelCase`"
    It's `nginxIngress`, not `nginx-ingress`. `--set …nginx-ingress.enabled=true`
    silently has no effect.

Enabling it also auto-enables the Coraza Web Application Firewall (WAF) and external-auth plugins in the
[Stream Processing Offload Agent (SPOA) hub sidecar](operations/spoa-hub.md). The gateway library (on by
default) already auto-enables the `mirror` plugin, so a default install may already be running the sidecar.
Basic host/path routing works without this library; only the `nginx.ingress.kubernetes.io/*` annotations need it.

### Control the DNS cutover

Keep `controller.config.templatingSettings.extraContext.statusPatches.enabled=false` until you've verified routing.
With it on, the moment HAPTIC's HAProxy Service has an address it stamps
`.status.loadBalancer` onto every adopted Ingress, and `external-dns`
switches DNS to it — a premature, unverified cutover.

### Metrics

HAPTIC provides compatible request metric names and labels for many
ingress-nginx dashboards. Set the prefix and controller class below, then review
the differences before reusing your alerts:

```yaml
vector:
  requestMetrics:
    prefix: nginx_ingress_controller
    controllerClass: k8s.io/ingress-nginx
```

That reproduces `nginx_ingress_controller_requests_total` and the six histograms, with
the label set `ingress-nginx` uses — `status`, `method`, `path`, `namespace`,
`ingress`, `service`, `host`, `controller_class`, `controller_namespace`,
`controller_pod`. See [Request metrics](operations/monitoring.md#request-metrics)
for what each family measures.

The original `nginx_ingress_controller_requests` remains a compatibility alias.
Use the `_total` name in new queries; don't sum the two aliases.

You gain one label: `term`, HAProxy's termination state, which distinguishes a
backend that refused the connection (`SC--`) from one that timed out without
sending headers (`sH--`) from a client that gave up (`cD--`). Turn it off with
`vector.requestMetrics.terminationStateLabel: false` if the extra cardinality
isn't worth it.

Four differences to expect:

- **Two scrape ports, not one.** Byte sizes and latencies can't share one set of
  histogram buckets, so the size families are exported on `9599` and everything
  else on `9598`. The bundled `PodMonitor` declares both — set
  `haproxy.monitoring.podMonitor.enabled: true`.
- **No `canary` label.** HAPTIC has no per-request canary marker. PromQL
  `{canary=""}`, which the stock dashboards use, matches a series without the
  label, so those queries are unaffected.
- **Different bucket boundaries.** Durations are a strict superset of
  `--time-buckets`, so a query hardcoding `le="0.5"` still resolves. Sizes
  use different buckets. Queries that name a particular size bucket may need
  updating; `_sum` and `_count` are unaffected. To use these ingress-nginx
  bucket lists:

    ```yaml
    vector:
      requestMetrics:
        durationBuckets: [0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10]
        sizeBuckets: [10, 20, 30, 40, 50, 60, 70, 80, 90, 100]
    ```

- **`request_size` reads lower.** It counts request **body** bytes; nginx's
  `$request_length` also counts the request line and headers. HAProxy exposes no
  headers-inclusive counter.

!!! warning "These are counted from the access log, not in the data path"
    They report fewer requests than were served whenever HAProxy drops log records under back-pressure. Keep
    the `HAProxyAccessLogRecordsDropped` alert on. If a request count has to stay
    exact through a drop, set
    `extraContext.prometheusExporter.excludeMetrics.httpRequestCounters.enabled: false`
    to keep HAProxy's own counters alongside these.

### Annotation support

The library covers the common `nginx.ingress.kubernetes.io/*` annotations —
backend timeouts, `load-balance`, `proxy-body-size`, `rewrite-target`,
`ssl-redirect`/`force-ssl-redirect`, HTTP Strict Transport Security (HSTS), Cross-Origin Resource Sharing (CORS), `whitelist`/`denylist-source-range`,
custom headers, basic + external auth, `ssl-passthrough`, canary, client mTLS,
request mirroring (`mirror-target`), and ModSecurity. Full per-annotation reference:
[nginx-ingress library docs](libraries/nginx-ingress.md).

See [differences from ingress-nginx](annotation-compatibility.md#ingress-nginx) before moving routes.

### Controller ConfigMap settings

HAPTIC doesn't read the ingress-nginx controller ConfigMap. Set the equivalent
Helm values instead. The migration report in
[Step 0](#step-0-check-what-will-change) classifies every key of the controller
ConfigMap against this table when you paste the ConfigMap into the
**Resources** panel:

```bash
kubectl -n ingress-nginx get configmap ingress-nginx-controller -o yaml
```

The report recognizes the controller ConfigMap by its name,
`ingress-nginx-controller` or `nginx-configuration`. A ConfigMap with another
name counts when it carries both labels `app.kubernetes.io/name: ingress-nginx`
and `app.kubernetes.io/component: controller` and at least one key from this
table. Keys missing from the table show as **unknown**.

In the table, `extraContext.` is short for
`controller.config.templatingSettings.extraContext.`, and defaults are in
parentheses. **Governance default** means HAPTIC sets the value per Ingress
rather than fleet-wide: a [governance rule](operations/governance.md) with a
`default` supplies the native annotation to every Ingress that doesn't set it.

<!-- BEGIN generated: migration-configmap-coverage ingress-nginx -->
| ingress-nginx key | HAPTIC setting | Status | What to check |
|-------------------|----------------|--------|---------------|
| `proxy-connect-timeout` (`5`) | `extraContext.timeoutConnect` (`100` ms) | **different** | The short default lets a failed connect retry another pod quickly. Raise it only if healthy backends take longer to connect. See [timeouts](libraries/base.md#connection-reliability-and-timeouts). |
| `proxy-read-timeout` (`60`), `proxy-send-timeout` (`60`) | `extraContext.timeoutServer` (`50000` ms) | **different** | One inactivity timeout covers both directions. |
| `client-header-timeout` (`60`) | `extraContext.timeoutHttpRequest` (unset: `timeoutClient`, `50000` ms) | supported |  |
| `client-body-timeout` (`60`) | `extraContext.requestBuffering.waitTimeout` (`10s`) | **different** | The total wait for the body before HAProxy forwards the request, not the gap between reads. See [request buffering](libraries/base.md#request-buffering). |
| `keep-alive` (`75`) | `extraContext.timeoutHttpKeepAlive` (unset: `timeoutHttpRequest`, then `timeoutClient`) | supported |  |
| `worker-shutdown-timeout` (`240s`) | `extraContext.hardStopAfter` (`60s`) | supported | See [graceful reload drain bound](operations/performance.md#graceful-reload-drain-bound). |
| `proxy-next-upstream`, `proxy-next-upstream-tries` | `extraContext.retryOn` (`conn-failure empty-response response-timeout`); HAProxy retries 3 times | **different** | Takes HAProxy `retry-on` conditions, not nginx ones. |
| `retry-non-idempotent` (`false`) | `extraContext.retryNonIdempotent` (`false`) | supported |  |
| `upstream-keepalive-*` | — | **dropped** | HAProxy reuses idle backend connections without configuration. |
| `proxy-body-size` (`1m`) | Governance default for `haproxy-haptic.org/max-request-body-size` | **different** | HAPTIC doesn't limit the body size unless you set a limit. |
| `client-header-buffer-size`, `large-client-header-buffers` | `extraContext.tune.bufsize` (`16384`) | **different** | The request line and all headers must fit in one HAProxy buffer. |
| `proxy-request-buffering` (`on`) | `extraContext.requestBuffering.enabled` (`true`) | **different** | Buffers at most `tune.bufsize` bytes of the body. |
| `ssl-protocols` (`TLSv1.2 TLSv1.3`) | `extraContext.tls.minVersion` (`TLSv1.2`) | **different** | Sets a minimum version, not a list. See [TLS cipher suites and protocol versions](ssl-certificates.md#tls-cipher-suites-and-protocol-versions). |
| `ssl-ciphers` | `extraContext.tls.ciphers` (TLS 1.2), `extraContext.tls.ciphersuites` (TLS 1.3) | supported |  |
| `ssl-session-tickets` (`false`) | `extraContext.tls.sessionTickets.enabled` (`false`) | supported | See [TLS session resumption](ssl-certificates.md#tls-session-resumption). |
| `enable-ocsp` (`false`) | — | **different** | Always on: every certificate loads with `ocsp-update on`. |
| `ssl-redirect` (`true`) | `extraContext.ingressDefaultSSLRedirect` (`false`) | **different** | Off by default. When on, it redirects every host HAPTIC serves over HTTPS, including hosts without `spec.tls`. See [Redirect HTTP to HTTPS](libraries/ingress.md#redirect-http-to-https). |
| `http-redirect-code` (`308`) | `extraContext.ingressDefaultSSLRedirectCode` (`"308"`); `extraContext.nginxHttpRedirectCode` (`"308"`) for the `ssl-redirect` annotation | supported |  |
| `hsts` (`true`) | `extraContext.tls.hsts.enabled` (`false`) | **different** | Off by default. See [HSTS](ssl-certificates.md#http-strict-transport-security-hsts). |
| `hsts-max-age` (`31536000`) | `extraContext.tls.hsts.maxAge` (`"31536000"`) | supported |  |
| `hsts-include-subdomains` (`true`) | `extraContext.tls.hsts.includeSubdomains` (`false`) | **different** | Off by default. |
| `hsts-preload` (`false`) | `extraContext.tls.hsts.preload` (`false`) | supported |  |
| `use-forwarded-headers` (`false`), `compute-full-forwarded-for` (`false`), `proxy-real-ip-cidr` | `extraContext.forwardedHeaders.trustedProxies` (`[]`) | **different** | Like `use-forwarded-headers: false`, HAPTIC replaces `X-Forwarded-For` with the client address and sets `X-Forwarded-Proto`, but drops a client-sent `X-Forwarded-Host` and `X-Forwarded-Port` instead of setting them. Listed sources keep these headers, with the client address appended to `X-Forwarded-For`; HAPTIC doesn't take the client address from them. See [forwarded headers](operations/security.md#forwarded-headers). |
| `forwarded-for-header`, `enable-real-ip` | Per Ingress: `haproxy-haptic.org/src-ip-header` | **different** | Reads the named header after [forwarded-header filtering](operations/security.md#forwarded-headers); there's no separate per-annotation trusted-CIDR setting. |
| `use-proxy-protocol` (`false`) | `extraContext.proxyProtocol.enabled` (`false`) | **different** | Adds separate PROXY-protocol ports (`8081`, `8444`) and leaves the HTTP and HTTPS ports unchanged. See [PROXY protocol](haproxy-deployment.md#proxy-protocol). |
| `use-gzip` (`false`), `gzip-types` | Governance default for `haproxy-haptic.org/compress-enable`: set the `default` of the bundled `haptic-compress-enable` rule to `"true"`. Types: `haproxy-haptic.org/compress-types` | supported | See [compression](libraries/haptic-annotations.md#compression). |
| `enable-brotli` (`false`) | — | **dropped** | The community HAProxy build has no Brotli. |
| `load-balance` (`round_robin`) | Governance default for `haproxy-haptic.org/load-balance` (`roundrobin`) | **different** | No `ewma`; `leastconn` is the closest. |
| `whitelist-source-range`, `denylist-source-range` | Governance default for `haproxy-haptic.org/allowlist-source-range` or `haproxy-haptic.org/denylist-source-range` | supported |  |
| `custom-http-errors` | Override `400.http` … `504.http` under `controller.config.files` | **different** | Replaces the error pages HAProxy generates; error responses from backends pass through unchanged unless the Ingress sets the [`custom-http-errors` annotation](libraries/nginx-ingress.md#nginxingresskubernetesiocustom-http-errors). See [general files](template-files.md#general-files). |
| `server-tokens` (`false`), `allow-backend-server-header` (`false`) | — | **different** | Every response carries `Server: haptic`, without a version, replacing any backend `Server` header. |
| `allow-snippet-annotations` (`false`) | `extraContext.ingressPermissions.allowRawHAProxyConfig` (`false`) | supported | Also covers the raw configuration annotations of the other annotation libraries. See [raw configuration annotations](operations/security.md#raw-configuration-annotations). |
| `main-snippet`, `http-snippet`, `server-snippet` | `controller.config.templateSnippets` at a base-library [extension point](libraries/base.md#available-extension-points) | **different** | The content must be HAProxy configuration; rewrite nginx snippets. |
| `log-format-upstream`, `log-format-escape-json` | `extraContext.accessLog.fields` | **different** | The access log is JSON with a fixed core field set. You add fields; you can't redefine the format. See [access logging](operations/access-logging.md). |
| `access-log-path`, `enable-syslog`, `syslog-host`, `syslog-port` | `extraContext.accessLog.targets` | supported |  |
| `generate-request-id` (`true`) | — | supported | Every access-log record carries a `req_id`. Forward it upstream with `haproxy-haptic.org/request-id`. |
| `enable-opentelemetry`, `otlp-collector-host`, `otlp-collector-port`, `otel-service-name`, `otel-sampler-ratio` | `extraContext.tracing.enabled`, `.otlp.endpoint`, `.otlp.serviceName`, `.sampleRate` (a percentage) | **different** | Spans are built from access-log records. |
| `enable-modsecurity`, `enable-owasp-modsecurity-crs` | `extraContext.waf.dispatch.mode: default-on` | **different** | Coraza with the Core Rule Set replaces ModSecurity. See [WAF policies](operations/waf-policies.md). |
| `worker-processes` | `haproxy.nbthread` | **different** | HAProxy runs threads, not processes. |
<!-- END generated: migration-configmap-coverage ingress-nginx -->

Keys missing from the table have no HAPTIC setting. Most tune nginx internals,
such as hash-table sizes, worker connections, Lua, GeoIP, and Jaeger.

These values restore the ingress-nginx defaults where HAPTIC's default differs.
`ingressDefaultHTTPS: false` stops HAPTIC from serving hosts without `spec.tls`
over HTTPS, so only hosts with `spec.tls` redirect, as in ingress-nginx:

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        timeoutServer: "60s"
        timeoutHttpRequest: "60s"
        timeoutHttpKeepAlive: "75s"
        ingressDefaultHTTPS: false
        ingressDefaultSSLRedirect: true
        tls:
          hsts:
            enabled: true
            includeSubdomains: true
        governance:
          rules:
            nginx-proxy-body-size:
              enabled: true
              resource: ingresses
              path: metadata.annotations['haproxy-haptic.org/max-request-body-size']
              default: "1m"
```

HAPTIC rejects `configuration-snippet` by default, as ingress-nginx does with
`allow-snippet-annotations: "false"`. If your Ingresses use it and everyone who
can edit an Ingress in a watched namespace may also edit the HAProxy
configuration, accept raw configuration annotations:

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        ingressPermissions:
          allowRawHAProxyConfig: true
```

See [Raw configuration annotations](operations/security.md#raw-configuration-annotations).

### TCP and UDP services

HAPTIC doesn't read the `tcp-services` or `udp-services` ConfigMaps.

**TCP:** create a Gateway with a `TCP` listener and a TCPRoute for each entry.
The Gateway API library is on by default. TCPRoute requires the Gateway API v1.6
standard channel or the experimental channel. This `tcp-services` entry:

```yaml
data:
  "5432": "default/postgres:5432"
```

becomes:

```bash
kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: tcp-services
  namespace: default
spec:
  gatewayClassName: haptic
  listeners:
    - name: postgres
      protocol: TCP
      port: 5432
      allowedRoutes:
        namespaces:
          from: Same
        kinds:
          - kind: TCPRoute
---
apiVersion: gateway.networking.k8s.io/v1
kind: TCPRoute
metadata:
  name: postgres
  namespace: default
spec:
  parentRefs:
    - name: tcp-services
      sectionName: postgres
  rules:
    - backendRefs:
        - name: postgres
          port: 5432
EOF
```

HAPTIC adds the listener port to its HAProxy Service. TCPRoute has no equivalent
of the `:PROXY` suffixes in a `tcp-services` entry. See
[TCPRoute support](libraries/gateway.md#tcproute-support) for listener rules and
limitations.

**UDP:** HAPTIC doesn't proxy UDP and doesn't support UDPRoute. Expose a UDP
workload through its own `LoadBalancer` Service, or keep it on ingress-nginx.

---

## From `haproxy-ingress`

Enable the compatibility library on your existing HAPTIC release:

```bash
helm upgrade haptic oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
  --version 0.9.0 --namespace haptic --reuse-values \
  --set controller.templateLibraries.haproxyIngress.enabled=true
```

Review the linked annotation differences, [match the IngressClass](#match-the-ingressclass),
and [control the DNS cutover](#control-the-dns-cutover). You can then migrate to
native `haproxy-haptic.org/*` annotations at your own pace.

Most routing, SSL, session-affinity, redirect, HSTS, CORS, access-control,
basic/external auth, client-mTLS, and WAF annotations are supported. Full
reference:
[haproxy-ingress library docs](libraries/haproxy-ingress.md).

See [differences from haproxy-ingress](annotation-compatibility.md#haproxy-ingress) before moving routes.

---

## From `haproxytech/kubernetes-ingress`

Enable the compatibility library on your existing HAPTIC release:

```bash
helm upgrade haptic oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
  --version 0.9.0 --namespace haptic --reuse-values \
  --set controller.templateLibraries.haproxytech.enabled=true
```

Review the linked annotation differences, [match the IngressClass](#match-the-ingressclass),
and [control the DNS cutover](#control-the-dns-cutover).

HAPTIC reads these annotations on **Ingress** resources only. haproxytech's
controller also reads many of them on Service and ConfigMap resources; that
Service/ConfigMap-level configuration doesn't carry over. Full reference:
[haproxytech library docs](libraries/haproxytech.md).

See [differences from HAProxy Technologies](annotation-compatibility.md#haproxytech) before moving routes.

---

## Troubleshooting

Check these settings when a migrated route behaves differently:

- **Existing Ingresses aren't being routed.** HAPTIC only serves Ingresses whose
  `spec.ingressClassName` equals `ingressClass.name` (default **`haptic`**) —
  an `ingressClassName: nginx` Ingress is filtered out before it reaches the
  controller's store. Fix: [match the IngressClass](#match-the-ingressclass).

- **Annotations seem to be ignored.** All three vendor compatibility libraries
  (`nginx.ingress.kubernetes.io/*`, `haproxy-ingress.github.io/*`, `haproxy.org/*`)
  are **disabled by default** — only the native `haproxy-haptic.org/*` library is
  on. A vendor annotation (timeouts, auth, CORS, rate-limits, redirects) is a
  silent no-op until you enable its matching library. Fix: [enable the annotation
  library](#enable-the-annotation-library) for the controller you're migrating from.

- **DNS cut over before you were ready.** Ingress status writes are **on by
  default** — install with them off and enable them only after you've verified
  routing. Fix: [control the DNS cutover](#control-the-dns-cutover).

For symptoms beyond these three, see the general
[troubleshooting](troubleshooting.md) guide.

## See also

- [Getting Started](getting-started.md) — install HAPTIC and route your first Ingress.
- [Playground](/playground/) — render an annotated Ingress live and see per-annotation migration warnings.
- [Watching Resources](watching-resources.md) — how `ingressClassName` scoping and field selectors work.
