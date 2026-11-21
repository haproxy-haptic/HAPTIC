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
| Hub               | `v0.13.1-rc.5`                     |
| `api-gateway`    | `v0.1.0`      |
| `coraza`          | `v0.12.0-rc.8`           |
| `external-auth`   | `v0.6.0`    |
| `fingerprinting`  | `v0.3.0`   |
| `maxmind`         | `v0.5.0`          |
| `mirror`          | `v0.6.0`           |
| `rate-limit`      | `v0.5.0`       |
| `sso-auth`        | `v0.4.1`         |

Plugin `.so` files target glibc `2.36` (Debian bookworm).

<!-- END: spoa-hub-bundle -->

### Reload and upgrade behavior

Plugin configuration changes reload in place while in-flight work drains.
Upgrading the bundled plugin binaries rolls the HAProxy pods. Keep multiple
HAProxy replicas available during an upgrade.

## What each plugin does

- **api-gateway** — performs bounded JSON request validation against schemas compiled at plugin initialization/reload.
- **coraza** — runs HTTP request inspection against the Open Worldwide Application Security Project (OWASP) Core Rule Set v4. HAPTIC wires the request phase only — there's no response-body inspection stage, so response compression doesn't interact with the WAF.
- **external-auth** — implements nginx-style `auth_request` semantics: makes an HTTP subrequest to an upstream auth service and returns allow/deny plus identity headers to HAProxy.
- **fingerprinting** — computes JA3, JA3N, and JA4 TLS fingerprints from the ClientHello.
- **maxmind** — performs in-memory MaxMind MMDB lookups: City, Country, Autonomous System Number (ASN), and so on. HAPTIC [delivers the databases](#geolocation-lookups).
- **mirror** — mirrors HTTP requests to a secondary backend for traffic shadowing; used by the gateway library to implement the Gateway API `HTTPRouteFilter` of type `RequestMirror`.
- **rate-limit** — enforces a shared request budget across HAProxy replicas. It uses the [managed Valkey store or your existing store](#managed-shared-rate-limit-store); choose how requests behave during store or plugin failures there.
- **sso-auth** — handles OpenID Connect (OIDC) and Security Assertion Markup Language (SAML) 2.0 single sign-on flows with encrypted session cookies.

Source-IP rate limits run before WAF, external authentication, and JSON request
validation. Consumer rate limits run after authentication establishes the identity.

## Choose the WAF engine

The `coraza` plugin defaults to Go Coraza. Select the pure Rust
zentinel-modsec engine for an installation by adding this to your Helm values:

```yaml
spoaHub:
  plugins:
    coraza:
      engine: rust
```

Both engines use the chart's WAF policy catalog and embedded OWASP Core Rule
Set. The HAProxy sidecar inspects requests and the controller validates policy
changes with the selected engine. Changing it rolls the affected pods.
Set `engine: go` to switch back. Both engines support the
[JSON audit log configuration](waf-tuning.md#last-resort-the-coraza-audit-log).

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
Coraza directives remain in place. HAPTIC loads the archive's setup, applies
per-policy tuning, and then loads the files from the release's `rules/` directory.
A rule-only archive uses the configured setup.

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

The plugin logs the rule count of each distinct rule set it compiles — at
startup and on every successful refresh. The embedded ruleset compiles to 661
rules:

```console
$ kubectl -n haptic logs -l app.kubernetes.io/component=loadbalancer -c spoa-hub | grep -i coraza
Coraza WAF app 'policy:base' initialized with 661 rules
Coraza compiled 1 distinct rule set(s) for 1 application(s)
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

The `maxmind` plugin looks up client IP addresses in GeoIP databases in
MaxMind's MMDB format, such as MaxMind GeoLite2 or DB-IP Lite. HAPTIC downloads
each database on the controller, refreshes it, and delivers it to every HAProxy
pod before the configuration that uses it, so the HAProxy pods need no internet
access or license key. This example adds `X-Country` and `X-Region` request
headers.

### 1. Declare the database and the lookups

Add the database's download URL and the fields to extract to your Helm values:

```yaml
# values.yaml
controller:
  config:
    templatingSettings:
      extraContext:
        maxmind:
          databases:
            city:
              url: https://downloads.example.com/geoip/city-lite.mmdb.gz
              unpack: gzip
              refreshInterval: 24h
          lookups:
            - name: country
              database: city
              path: [country, iso_code]
              outputVar: geo_country
            - name: region
              database: city
              path: [subdivisions, "0", iso_code]
              outputVar: geo_region
```

Declaring a database enables the plugin and gives the controller and HAProxy
pods a disk-backed `blobs` volume of `blobs.sizeLimit` (default `1Gi`). The URL
must be `https://`; add `sha256` to accept only one exact file. For a
`tar.gz` download, set `unpack: tar.gz` and the archive path of the database as
`member`. A download whose archive path changes with every release can't
refresh under a fixed `member`, so prefer the plain `.mmdb` or `.mmdb.gz` file.

The `path` of each lookup walks the database record: map keys, or array indexes
such as `"0"` for the first element and `"-1"` for the last. Read the record of
an address with `mmdblookup` to find the paths your database offers.

The controller fetches the URL itself. Its default NetworkPolicy allows HTTPS to
any address; if you narrowed `controller.networkPolicy.egress`, add a rule for
the download host.

### 2. Use the results

Every request is looked up once, and each result is available as
`txn.hub.maxmind.<outputVar>`, with `txn.hub.maxmind.<outputVar>_status` set to
`ok`, `not_found`, or `error`. Add a snippet that uses them:

```yaml
# values.yaml
controller:
  config:
    templateSnippets:
      frontend-spoe-filters-300-geoip:
        template: |
          http-request set-header X-Country %[var(txn.hub.maxmind.geo_country)]
          http-request set-header X-Region %[var(txn.hub.maxmind.geo_region)]
```

Apply the combined values, then send a request through one of your routes.
Addresses absent from the database, including private cluster addresses, don't
produce a country code.

### Budget the memory

The hub keeps each database in memory and holds the old and the new version
while it switches to a new one. Budget `spoaHub.resources.limits.memory` at
64 MiB plus twice the size of your databases, plus what other plugins need: a
130 MB City database needs about 324 MiB, which the default `384Mi` holds when
it's the only plugin. HAPTIC refuses a configuration whose databases don't fit,
and the hub keeps the previous version. The controller's validator doesn't load
databases.

### When a database changes or can't be downloaded

A new version reaches the pods only after a configuration that uses it passes
validation; the hub then reloads the plugin, without restarting HAProxy. If a
download fails, the pods keep the version they have. Until a database's first
version has been downloaded, HAPTIC leaves its lookups out, and requests pass
without those variables. `haptic_blob_fetch_errors_total` counts failed
downloads.

While an HAProxy pod runs an agent from a release without blob delivery, for
example during an upgrade, HAPTIC delivers no database and leaves the lookups
out until every pod has been replaced.

To use a database you mount into the spoa-hub yourself instead, leave
`extraContext.maxmind` empty, set `spoaHub.plugins.maxmind.enabled: true`, and
write the plugin's
[configuration](https://github.com/haproxy-haptic/haproxy-spoa-hub-plugin-maxmind)
under `spoaHub.plugins.maxmind.params`. Dispatch it yourself with
`http-request send-spoe-group spoa-hub-geoip-enrich geoip-enrich-group`.

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
`allowedIssuers` prefixes; otherwise its routes answer `503`.

The hub fetches the discovery document, signing keys and tokens from the HAProxy
pods. Their default NetworkPolicy allows egress only to DNS and pods in the
cluster, so an identity provider inside the cluster works as is. For one outside
the cluster, allow HTTPS to its address:

```yaml
haproxy:
  networkPolicy:
    extraEgress:
      - to:
          - ipBlock:
              cidr: 203.0.113.10/32   # your identity provider
        ports:
          - port: 443
            protocol: TCP
```

With `pass-token`, the hub refreshes a token shortly before it expires. Requests
that arrive together can refresh it in parallel, so turn off refresh-token reuse
detection at the identity provider, or sessions may end early.

HAPTIC reads each provider Secret from the Ingress's namespace. With
`controller.rbac.releaseNamespaceSecretsOnly: true`, only Ingresses in the
release namespace can use `sso-auth`. The client secret and cookie secret reach
the hub as Secret-backed files, never in a readable custom resource. Their
change markers in custom resources are keyed with the dataplane password, so if
you set `credentials.existingSecret`, give it a long random password.

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
