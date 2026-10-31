# Add request-processing plugins

<a id="spoa-hub"></a>

<a id="overview"></a>

The SPOA hub runs beside HAProxy and handles request processing through plugins:
Web Application Firewall (WAF) inspection, authentication, rate limits, mirroring, geolocation, and TLS
fingerprinting. HAPTIC's `spoa-hub` image includes
[haproxy-spoa-hub](https://github.com/haproxy-haptic/haproxy-spoa-hub) and the plugin
libraries listed below.

SPOA means Stream Processing Offload Agent. HAProxy sends requests to this
sidecar for processing and uses the result to allow, reject, or modify them.

Use this reference to enable plugins, inspect their versions and health, and
configure HAProxy's connection to the hub.

## Enabling the hub

Enable a plugin under `spoaHub.plugins` in your
[complete Helm values file](../deploying-with-helm.md#change-settings), then apply
the values. The chart starts the hub when at least one plugin is enabled. For
example, to make TLS fingerprinting available:

```yaml
spoaHub:
  plugins:
    fingerprinting:
      enabled: true
```

Some features enable their plugins automatically:

- **api-gateway** follows `controller.config.templatingSettings.extraContext.apiGateway.requestSchemaValidation.enabled`,
- **coraza** follows a non-empty WAF policy catalog, `waf.dispatch.mode=default-on`, `controller.templateLibraries.nginxIngress.enabled`, or `controller.templateLibraries.haproxyIngress.enabled`,
- **external-auth** follows `controller.templateLibraries.nginxIngress.enabled`,
- **mirror** follows `controller.templateLibraries.gateway.enabled`,
- **rate-limit** follows `rateLimit.shared.enabled`.

A default installation runs the hub with `mirror`. Enable `fingerprinting`, `maxmind`, and `sso-auth` explicitly if you need them.

Configure [WAF policies](waf-policies.md) under `extraContext.waf`. Set Coraza timeouts, concurrency limits, and plugin parameters under `spoaHub.plugins.coraza`.

An explicit boolean on `spoaHub.enabled` always wins: `false` forces the sidecar off even with plugins enabled; `true` renders it with none. See the [Chart Values Reference](../reference.md#spoa-hub-sidecar) for every `spoaHub.*` value.

## Bundled components

The image is published at `ghcr.io/haproxy-haptic/haptic/spoa-hub:<HAPTIC version>` and is built from the following pinned upstream releases:

<!-- BEGIN: spoa-hub-bundle -->

| Component       | Pinned version                          |
| --------------- | --------------------------------------- |
| Hub               | `v0.13.0`                     |
| `api-gateway`    | `v0.1.0`      |
| `coraza`          | `v0.10.1`           |
| `external-auth`   | `v0.6.0`    |
| `fingerprinting`  | `v0.3.0`   |
| `maxmind`         | `v0.4.0`          |
| `mirror`          | `v0.6.0`           |
| `rate-limit`      | `v0.4.2`       |
| `sso-auth`        | `v0.4.1`         |

Plugin `.so` files target glibc `2.36` (Debian bookworm).

<!-- END: spoa-hub-bundle -->

### Reload and upgrade behavior

Plugin configuration changes reload in place while in-flight work drains.
Upgrading the bundled plugin binaries rolls the HAProxy pods. Keep multiple
HAProxy replicas available during an upgrade.

## What each plugin does

- **api-gateway** — performs bounded JSON request validation against schemas compiled at plugin initialization/reload.
- **coraza** — embeds the [Coraza WAF](https://coraza.io/) engine and runs HTTP request inspection against the Open Worldwide Application Security Project (OWASP) Core Rule Set v4. HAPTIC wires the request phase only — there's no response-body inspection stage, so response compression doesn't interact with the WAF.
- **external-auth** — implements nginx-style `auth_request` semantics: makes an HTTP subrequest to an upstream auth service and returns allow/deny plus identity headers to HAProxy.
- **fingerprinting** — computes JA3, JA3N, and JA4 TLS fingerprints from the ClientHello.
- **maxmind** — performs in-memory MaxMind MMDB lookups against operator-provided database files: City, Country, Autonomous System Number (ASN), and so on.
- **mirror** — mirrors HTTP requests to a secondary backend for traffic shadowing; used by the gateway library to implement the Gateway API `HTTPRouteFilter` of type `RequestMirror`.
- **rate-limit** — enforces a shared request budget across HAProxy replicas. It uses the [managed Valkey store or your existing store](#managed-shared-rate-limit-store); choose how requests behave during store or plugin failures there.
- **sso-auth** — handles OpenID Connect (OIDC) and Security Assertion Markup Language (SAML) 2.0 single sign-on flows with encrypted session cookies.

Source-IP rate limits run before WAF, external authentication, and JSON request
validation. Consumer rate limits run after authentication establishes the identity.

## Update the WAF rule set

The Coraza plugin embeds an Open Worldwide Application Security Project (OWASP)
Core Rule Set (CRS) v4 release, so the WAF has rules the moment you enable it.
That embedded ruleset only moves when the plugin image does. To pick up a CRS
release without waiting for a HAPTIC release, point HAPTIC at the release
tarball:

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        waf:
          crs:
            url: https://github.com/coreruleset/coreruleset/releases/download/v4.19.0/coreruleset-4.19.0-minimal.tar.gz
```

The URL must be `https://`. The ruleset decides what the WAF blocks, so a
plaintext fetch could be replaced in transit and the substituted rules would
still validate.

HAPTIC deploys the fetched rule files alongside your configuration. Your custom
Coraza directives remain in place; only the bundled CRS includes are replaced.

### What a refresh costs

HAPTIC checks the URL at `waf.crs.refreshInterval` (default `1h`). Unchanged
content causes no update. Coraza compiles changed rules before activating them;
requests in progress finish with the previous rules. A refresh doesn't reload
HAProxy or the SPOA hub.

Automatic rule refresh needs Coraza plugin v0.10.0 or later, which is what this HAPTIC version
bundles. If you pin an older SPOA hub bundle through `spoaHub.image`, the files
arrive but nothing rebuilds, and the WAF keeps running the previous rules until
something else reloads the hub.

### Confirm which ruleset is running

The plugin logs its rule count whenever it compiles — at startup and on every
successful refresh. The embedded ruleset compiles to 661 rules:

```console
$ kubectl -n haptic logs -l app.kubernetes.io/component=loadbalancer -c spoa-hub | grep -i coraza
Coraza default WAF initialized with 661 rules
Coraza reloaded its rule set after an on-disk change
```

The rule count alone doesn't identify the ruleset, since a CRS release can
compile to a similar number. List the files to see which one is on the pods —
`crs-` prefixed files are there only when a fetched ruleset is in use:

```bash
kubectl -n haptic exec deploy/haptic-haproxy -c haproxy -- ls /etc/haproxy/general/ | grep '^crs-'
```

`plugin_coraza_rule_reloads_total{result="failed"}` counts refreshes that didn't
compile. New increments mean a refresh failed and the previous rules remained active.
A later successful refresh doesn't reset this counter.

### If the ruleset can't be obtained

If a download or archive expansion fails, HAPTIC keeps the last deployed ruleset,
even after a controller restart. If none is available, it uses the plugin's
embedded rules. A download failure doesn't block unrelated configuration changes.

HAPTIC rejects archives with no `.conf` rule files. If new rules reach the pods
but fail to compile, Coraza keeps the active rules and increments
`plugin_coraza_rule_reloads_total{result="failed"}`.

## Tune a WAF policy from detect to deny

<a id="read-the-per-rule-hit-metrics"></a>
<a id="identify-would-block-rules"></a>
<a id="classify-hits-from-the-access-log"></a>
<a id="see-the-value-a-rule-matched-on"></a>
<a id="last-resort-the-coraza-audit-log"></a>
<a id="flip-to-deny"></a>

Use the [WAF tuning guide](waf-tuning.md) to inspect rule matches, address false
positives, and move a policy from observation to blocking.

## Correlating hub logs with the access log

The hub's log lines carry `span.req_id`, holding the same value as the JSON access
log's `req_id` field for that request. So a hub warning and the HAProxy record for
the request that caused it can be joined on one key:

```console
# the access-log record
kubectl logs -n haptic deployment/haptic-haproxy -c vector | jq 'select(.req_id=="019f9e64-e9de-7d1b-88c9-76644f0e9b86")'

# and anything the hub said about the same request
kubectl logs -n haptic deployment/haptic-haproxy -c spoa-hub | jq 'select(.["span.req_id"]=="019f9e64-e9de-7d1b-88c9-76644f0e9b86")'
```

The chart sends HAProxy's own `unique-id` on every SPOE message, and the hub adopts
it. Nothing to configure. Requires spoa-hub v0.11.0 or later; an older hub ignores
the argument and logs its own internal id instead.

`spoa_request_id_source_total{source}` reports which id each message used —
`adopted`, `generated`, or `rejected`. **Alert on `rejected`**: it means the hub
replaced a supplied id, so its logs and the access log name every request
differently, and no other signal shows that.

## Managed shared rate-limit store

Enable shared rate limiting with:

```yaml
rateLimit:
  shared:
    enabled: true
```

The managed store is enabled by default once shared rate limiting is enabled:

```yaml
rateLimit:
  shared:
    managedStore:
      enabled: true
      replicas: 3
      sentinel:
        quorum: 2
```

HAPTIC renders a fixed-size HA Valkey topology:

- one `StatefulSet` with three pods by default;
- one writable Valkey primary and replicas;
- one Sentinel sidecar per pod for failover;
- a PodDisruptionBudget with `maxUnavailable: 1`;
- a NetworkPolicy that admits HAProxy/SPOA traffic plus store-internal Valkey/Sentinel traffic.

The managed store provides failover at a fixed size. Use your own Redis or
Valkey infrastructure if you need horizontal store scaling.

When Valkey can't answer, the default policy enforces an emergency token bucket in each SPOA sidecar. Lease mode spends any tokens it already leased before using that emergency budget. Exact mode switches to the same local tier after the store-operation timeout. This bounds each process, not the fleet: during an outage, each pod can admit its emergency burst plus tokens refilled at the configured rate, in addition to outstanding lease tokens. If the local registry is full or the hub returns no verdict, the request is allowed and marked degraded. A sidecar restart loses its emergency state and starts a new process budget. Set `rateLimit.shared.failClosed=true` when denial is safer than any of those outage grants.

If you already run a Redis/Valkey platform, disable the managed store and provide the endpoint directly:

```yaml
rateLimit:
  shared:
    enabled: true
    managedStore:
      enabled: false
    externalStore:
      urls:
        - "redis-sentinel://valkey-sentinel.data.svc:26379/0?sentinelServiceName=mymaster"
```

Configure the external store with a non-evicting memory policy. Supply one URL
through `externalStore.urls`; the chart rejects multiple URLs and manual
`store_url` or `store_urls` entries in the plugin's `params`.

Configure the hub-side plugin budget and store-operation budget together:

```yaml
spoaHub:
  plugins:
    rate-limit:
      timeoutMs: 50
      storeOperationTimeoutMs: 10
```

`timeoutMs` bounds the rate-limit plugin call inside the hub; the chart derives that message's outer HAProxy deadline from it. `storeOperationTimeoutMs` is the important request-latency bound for exact `gcra` mode because that mode performs a synchronous store operation per request. Set it from measured in-cluster Valkey/Sentinel round-trip time plus a small margin; raising it improves tolerance for slow cross-zone or external stores, but also delays the switch to local fallback when the store is unhealthy. Keep the default token-bucket mode for DoS-facing edge limits.

## Geolocation lookups

The `maxmind` plugin looks up client IP addresses in a MaxMind MMDB database. Enable the plugin, mount your database, and add a template snippet to use the result. This example adds an `X-Country` request header.

### 1. Enable the plugin and declare the lookup

Turn the plugin on and define, under `params:`, which MMDB files to open and which fields to extract. Each `[[lookups]]` entry sets `output_var` (the variable the hub writes back) and `message` (the SPOE message that triggers it — keep it equal to the plugin's `messages` entry, the default `geoip-enrich`):

```yaml
# values.yaml
spoaHub:
  plugins:
    maxmind:
      enabled: true
      messages: ["geoip-enrich"]   # chart default; drives the generated SPOE group name
      params: |
        [databases]
        country = { path = "/data/GeoLite2-Country.mmdb" }

        [[lookups]]
        name       = "country_code"
        message    = "geoip-enrich"
        database   = "country"
        path       = ["country", "iso_code"]
        output_var = "geo_country"
```

### 2. Mount the MMDB database

The database file lives in the HAProxy pod, where the `spoa-hub` sidecar runs. Declare a pod volume with `haproxy.extraVolumes` and mount it into the sidecar with `spoaHub.extraVolumeMounts` at the path your `params:` references (`/data` above).

MMDB files exceed the 1 MiB `ConfigMap`/`Secret` size limit (GeoLite2-Country alone is several MB), so don't try to mount one from a `Secret`. Use a `PersistentVolumeClaim`, or — as below — an `emptyDir` populated by an init container that downloads the database. The init container needs your MaxMind license key. Create its Secret in the
release namespace:

```bash
read -r -s -p "MaxMind license key: " maxmind_license
printf '%s' "$maxmind_license" |
  kubectl create secret generic maxmind-license --namespace haptic \
    --from-file=license_key=/dev/stdin --dry-run=client -o yaml | kubectl apply -f -
unset maxmind_license
```

Then add the download container and volume to your Helm values:

```yaml
# values.yaml
haproxy:
  extraVolumes:
    - name: maxmind-data
      emptyDir: {}
  initContainers:
    - name: fetch-maxmind
      image: curlimages/curl:latest
      command:
        - sh
        - -c
        - |
          set -eu
          curl -fsSL -o /tmp/maxmind.tar.gz \
            "https://download.maxmind.com/app/geoip_download?edition_id=GeoLite2-Country&license_key=$LICENSE_KEY&suffix=tar.gz"
          tar -xzf /tmp/maxmind.tar.gz --strip-components=1 -C /data
      env:
        - name: LICENSE_KEY
          valueFrom:
            secretKeyRef:
              name: maxmind-license
              key: license_key
      volumeMounts:
        - name: maxmind-data
          mountPath: /data

spoaHub:
  extraVolumeMounts:
    - name: maxmind-data
      mountPath: /data
      readOnly: true
```

### 3. Dispatch the lookup and use the result

Add a `frontend-spoe-filters-*` snippet to run the lookup and set the header.
The engine and group names below match the `geoip-enrich` message configured
in step 1. The result is available as `txn.hub.maxmind.geo_country`.

```yaml
# values.yaml
controller:
  config:
    templateSnippets:
      frontend-spoe-filters-300-geoip:
        template: |
          http-request send-spoe-group spoa-hub-geoip-enrich geoip-enrich-group
          http-request set-header X-Country %[var(txn.hub.maxmind.geo_country)]
```

Apply the combined values, then send a request through one of your routes.
Check the application's received `X-Country` header. Addresses absent from the
database, including private cluster addresses, don't produce a country code.

## OIDC sign-in

The `sso-auth` plugin signs users in with OpenID Connect for Ingresses that set
[`haproxy-haptic.org/sso-auth`](../libraries/haptic-annotations.md#oidc-sign-in).
Application teams describe their provider in a Secret in the Ingress's namespace;
you decide which identity providers they may use.

```yaml
spoaHub:
  plugins:
    sso-auth:
      enabled: true
controller:
  config:
    templatingSettings:
      extraContext:
        ssoAuth:
          allowedIssuers:
            keycloak: https://keycloak.example.com/realms/main/
```

A provider Secret's `discovery-url` must start with one of the
`allowedIssuers` prefixes; otherwise its routes answer `503`. The hub fetches
the discovery document, signing keys and tokens from the HAProxy pods, so allow
egress from those pods to the issuer if you restrict it with NetworkPolicy.

HAPTIC reads each provider Secret from the Ingress's namespace. With
`controller.rbac.releaseNamespaceSecretsOnly: true`, only Ingresses in the
release namespace can use `sso-auth`. The client secret and cookie secret reach
the hub as Secret-backed files, never in a readable custom resource.

## Verifying the published image

The image is signed by digest with cosign keyless. The CycloneDX Software Bill
of Materials (SBOM) is attached as an in-toto attestation. Releases after 0.5.0
are signed by the GitHub release workflow:

```bash
read -r -p "HAPTIC image version: " haptic_version
release_identity="https://github.com/haproxy-haptic/HAPTIC/.github/workflows/release.yml@refs/tags/v$haptic_version"
cosign verify "ghcr.io/haproxy-haptic/haptic/spoa-hub:$haptic_version" \
  --certificate-identity "$release_identity" \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com'
cosign verify-attestation "ghcr.io/haproxy-haptic/haptic/spoa-hub:$haptic_version" \
  --type cyclonedx \
  --certificate-identity "$release_identity" \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com'
```

Releases up to 0.5.0 keep their original GitLab CI signatures:

```bash
read -r -p "HAPTIC image version: " haptic_version
cosign verify "ghcr.io/haproxy-haptic/haptic/spoa-hub:$haptic_version" \
  --certificate-identity-regexp '^https://gitlab\.com/haproxy-haptic/haptic//\.gitlab-ci\.yml@refs/tags/.*$' \
  --certificate-oidc-issuer 'https://gitlab.com'
cosign verify-attestation "ghcr.io/haproxy-haptic/haptic/spoa-hub:$haptic_version" \
  --type cyclonedx \
  --certificate-identity-regexp '^https://gitlab\.com/haproxy-haptic/haptic//\.gitlab-ci\.yml@refs/tags/.*$' \
  --certificate-oidc-issuer 'https://gitlab.com'
```

## Performance tuning

HAProxy communicates with the hub through a shared Unix socket using the
[Stream Processing Offload Protocol](https://docs.haproxy.org/spoe.html) (SPOP).
HAProxy's side of that connection is the Stream Processing Offload Engine (SPOE).

An unhealthy hub is removed from service without affecting HAProxy readiness.
Use these settings to adjust the connection and processing deadlines:

| Values key | Default | When to change |
| ---------- | ------- | -------------- |
| `spoaHub.haproxy.socketPath` | `/run/spoa/hub.sock` | Match a different socket path configured for the sidecar. |
| `spoaHub.haproxy.modeSpop` | `true` | Set `false` to use TCP mode on HAProxy 3.1+. HAProxy 3.0 uses TCP mode automatically. |
| `spoaHub.haproxy.timeoutHello` | `2s` | Raise if plugin initialization causes handshake timeouts. |
| `spoaHub.haproxy.timeoutIdle` | `5m` | Lower to release idle connections sooner. |
| `spoaHub.haproxy.timeoutProcessing` | Each message's budget + `100ms` | Leave unset for automatic deadlines. An explicit value applies to every message and must cover the longest processing budget. |
| `spoaHub.haproxy.timeoutProcessingMarginMs` | `100` | Allow more scheduling and serialization time beyond plugin processing budgets. |
| `spoaHub.haproxy.poolMaxConn` | `100` | Match peak concurrent messages; estimate with request rate × p99 processing latency. |
| `spoaHub.haproxy.poolPurgeDelay` | `30s` | Lower to release idle pooled connections sooner during traffic dips. |

Set each plugin's processing limit with `spoaHub.plugins.<name>.timeoutMs`.
Automatic HAProxy deadlines include sequential plugin stages plus
`timeoutProcessingMarginMs`.

## See also

- [haproxy-spoa-hub](https://github.com/haproxy-haptic/haproxy-spoa-hub) — upstream hub binary and SPOP gateway.
- [Chart Values Reference — SPOA Hub Sidecar](../reference.md#spoa-hub-sidecar) — every `spoaHub.*` value.
- [HAProxy versions matrix](./haproxy-versions.md) — supported HAProxy versions for the controller image.
