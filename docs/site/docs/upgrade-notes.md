# Upgrade notes

Check the notes for every version between your installed version and the version
you plan to deploy. Each section lists configuration changes and the steps needed
to keep your existing routes working. For routine chart upgrades, see
[Upgrading with Helm](deploying-with-helm.md#upgrading).

## Forwarding-header changes after 0.7.0

HAPTIC now removes client-sent `Forwarded`, `X-Forwarded-Prefix`,
`X-Forwarded-Server`, `X-Real-IP`, `X-Client-IP`, and `Client-IP` unless their
source appears in `forwardedHeaders.trustedProxies`. Operator snippets still
run after header filtering, so their header setters and deletions take effect.

If an Ingress uses `src-ip-header: X-Real-IP`, `X-Client-IP`, or `Client-IP`,
list the L7 proxy that overwrites that header in `trustedProxies`. See the
[complete L7 proxy example](operations/security.md#per-route-exceptions).
The annotation alone no longer trusts these client-supplied headers.
For direct clients behind an L4 load balancer sending PROXY protocol, remove
the header annotation: HAPTIC already receives the client address from PROXY
protocol. Don't add client networks to `trustedProxies` to restore the old behavior.

## Upgrading to 0.7

0.7 changes raw configuration permissions and forwarded headers. The pre-upgrade
hook also rolls the controller image before applying templates that need the new
binary. See [controller preparation](deploying-with-helm.md#controller-preparation)
for recovery, custom image tags, and deployments without hooks.

### 1. Check for raw configuration annotations

0.7 rejects raw HAProxy configuration annotations by default. These annotations
insert their value into `haproxy.cfg` verbatim:

- `haproxy-haptic.org/config-global`, `config-defaults`, `config-frontend`, and `config-backend`
- `haproxy-ingress.github.io/config-global`, `config-defaults`, `config-frontend`, and `config-backend`
- `haproxy.org/backend-config-snippet`
- `nginx.ingress.kubernetes.io/configuration-snippet`

After the upgrade, an Ingress that sets one of them gets an `InputRejected`
Warning Event and isn't served until you remove the annotation. An annotation of
an annotation library you haven't enabled has no effect and isn't rejected.

List the Ingresses that set a raw configuration annotation:

```bash
kubectl get ingress --all-namespaces -o json | jq -r '.items[] | select(any(.metadata.annotations // {} | to_entries[]; (.key | test("^((haproxy-haptic\\.org|haproxy-ingress\\.github\\.io)/config-(global|defaults|frontend|backend)|haproxy\\.org/backend-config-snippet|nginx\\.ingress\\.kubernetes\\.io/configuration-snippet)$")) and (.value | test("\\S")))) | "\(.metadata.namespace)/\(.metadata.name)"'
```

If the command prints nothing, continue with the proxy checks below.

If it prints Ingresses, either remove the annotations from them before you
upgrade, or keep the previous behavior by adding this value to your Helm values:

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        ingressPermissions:
          allowRawHAProxyConfig: true
```

Keep the previous behavior only if everyone who can edit an Ingress in a watched
namespace may also edit the HAProxy configuration. While WAF governance is
active, `waf.ingressPermissions.allowRawHAProxyConfig` must also be `true`, as
in 0.6. See [Raw configuration annotations](operations/security.md#raw-configuration-annotations).

### 2. List the proxies in front of HAPTIC

HAPTIC now replaces a client-sent `X-Forwarded-For` with the client address,
sets `X-Forwarded-Proto` to `https` or `http`, and drops a client-sent
`X-Forwarded-Host` and `X-Forwarded-Port`. Before, it appended the client
address to whatever `X-Forwarded-For` the client sent, so any client could
forge the address a backend logs or trusts.

If clients reach HAPTIC directly, or through an L4 load balancer or the
[PROXY protocol](haproxy-deployment.md#proxy-protocol), continue with the next
step. If an L7 load balancer or a content delivery network sets these headers
in front of HAPTIC, list its source addresses, or your backends lose the
original client address:

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        forwardedHeaders:
          trustedProxies:
            - 10.0.0.0/8
```

To keep the old `X-Forwarded-For` handling for every client, which keeps
forged values, trust every source:

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        forwardedHeaders:
          trustedProxies: ["0.0.0.0/0", "::/0"]
```

HAPTIC still sets `X-Forwarded-Proto` on requests that arrive without one. See
[forwarded headers](operations/security.md#forwarded-headers).

### 3. Check `haproxy-ingress.github.io/forwardfor`

If you don't enable the haproxy-ingress library, skip this step. HAPTIC had the
values `add` and `update` the wrong way round. They now match haproxy-ingress:
`add` replaces the client's `X-Forwarded-For` with the client address, and
`update` keeps it and appends the client address. List the Ingresses that set
the annotation:

```bash
kubectl get ingress --all-namespaces -o jsonpath='{range .items[?(@.metadata.annotations.haproxy-ingress\.github\.io/forwardfor)]}{.metadata.namespace}/{.metadata.name}: {.metadata.annotations.haproxy-ingress\.github\.io/forwardfor}{"\n"}{end}'
```

To keep a listed Ingress's previous behaviour, change `add` to `update` or
`update` to `add`.

## Upgrading to 0.6

0.6 requires Kubernetes 1.35 or newer, removes HAProxy 3.1, and moves the chart
and images to the GitHub container registry.

### 1. Check the Kubernetes version

Kubernetes 1.33 is past its upstream end of life, and 1.34 reaches it on
2026-10-27. Helm refuses to install or upgrade the 0.6 chart on an older cluster.

```bash
kubectl version
```

If the server version is older than 1.35, upgrade the cluster before you upgrade
HAPTIC.

### 2. Move off HAProxy 3.1

HAProxy 3.1 is past its upstream end of life and misses the major HTTP/1 and
HTTP/2 fixes released on 2026-10-14. 0.6 no longer builds it, and Helm refuses
the 0.6 chart while `haproxyVersion` is `3.1`. Check the series your release
selects:

```bash
helm get values haptic --namespace haptic --output yaml | grep haproxyVersion
```

If the command prints nothing or another series, continue with the next step.
If it prints `haproxyVersion: "3.1"`, switch your current 0.5 release to a
long-term support (LTS) series before you upgrade HAPTIC, so you can verify the
new HAProxy series on the chart you already run. To select 3.4:

```bash
helm upgrade haptic oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
  --version 0.6.0 --namespace haptic --reuse-values \
  --set-string haproxyVersion=3.4 \
  --set-string haproxy.image.tag= --set-string controller.image.tag=
```

To select 3.2:

```bash
helm upgrade haptic oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
  --version 0.6.0 --namespace haptic --reuse-values \
  --set-string haproxyVersion=3.2 \
  --set-string haproxy.image.tag= --set-string controller.image.tag=
```

These commands use the chart's Community Edition images. For custom or
Enterprise images, follow [HAProxy Enterprise](operations/haproxy-versions.md#haproxy-enterprise)
instead; the chart has a tested Enterprise image for 3.2 but not for 3.4. If you
manage values in Git, set `haproxyVersion` there too.

[Check the Deployments](deploying-with-helm.md#check-the-installation) before you
continue.

### 3. Remove GitLab registry overrides

The chart's default images come from `ghcr.io/haproxy-haptic/haptic`. Find image
overrides that still point at the GitLab registry:

```bash
grep -n 'registry.gitlab.com/haproxy-haptic' haptic-values.yaml
```

Delete every `repository` line the command prints, then follow the
[Helm upgrade procedure](deploying-with-helm.md#upgrading). The chart itself is
published at `oci://ghcr.io/haproxy-haptic/haptic/charts/haptic`.

## Upgrading to 0.5

0.5 lets a new HAProxy pod load a previously acknowledged configuration when a
restarted controller or new leader can't render. During recovery, existing
configured pods keep serving without a reload. Follow the normal
[Helm upgrade procedure](deploying-with-helm.md#upgrading); this feature needs no
new Helm values.

### Establish a recovery checkpoint

Before relying on retained recovery during node maintenance, let a 0.5 controller
complete a successful configuration deployment and publish its acknowledgement.
That creates the retained configuration and auxiliary files in Kubernetes.
An installation upgraded while rendering is already blocked has no checkpoint
from the older controller. Repair the rendering error first; upgrading alone
doesn't make replacement HAProxy pods Ready in that case.

The controller uses a checkpoint only when it matches the latest recorded
deployment and the configured HAProxy pods. A newer deployment without a usable
checkpoint prevents recovery from an older one. Retained recovery also requires
at least one controller to pass its startup checks.

### Account for frozen routing inputs

While rendering is blocked, replacement pods receive the retained endpoint lists,
routes, certificates, and fetched files. They don't receive subsequent input
changes until rendering recovers. A Ready proxy can therefore still point at a
backend address that no longer exists.

The bundled `PrometheusRule` alerts when retained recovery is active. `/healthz`
includes the render error but continues to report controller component health;
HTTP 200 doesn't prove that current routing inputs have been applied. See
[controller failure behavior](operations/high-availability.md#what-happens-when-a-controller-fails)
for recovery checks and monitoring.

## Upgrading to 0.4

0.4 ships a values schema. Helm checks your values against it on every
`helm install`, `helm upgrade`, `helm template`, and `helm lint`, and refuses the
release if a key is unknown or a value has the wrong type. Earlier versions
ignored such keys, so a values file that installs 0.3 can fail on 0.4. If you run
0.2, complete [Upgrading to 0.3](#upgrading-to-03) first.

Confirm that the target version is available on the
[releases page](https://github.com/haproxy-haptic/HAPTIC/releases) before
running the upgrade commands.

### 1. Save your values

Set the installed Helm release and namespace:

```bash
HAPTIC_RELEASE=haptic
HAPTIC_NAMESPACE=haptic
```

Export the values you supplied to Helm:

```bash
umask 077
helm get values "$HAPTIC_RELEASE" --namespace "$HAPTIC_NAMESPACE" \
  --output yaml > haptic-values-0.4.yaml
if [ "$(cat haptic-values-0.4.yaml)" = null ]; then
  printf '{}\n' > haptic-values-0.4.yaml
fi
```

If you manage values in Git, check that file instead, and apply the fixes from
the next step to it.

### 2. Check your values against the schema

Render the 0.4 chart with your values. This contacts no cluster and changes
nothing:

```bash
helm template "$HAPTIC_RELEASE" \
  oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
  --version 0.4.1 --namespace "$HAPTIC_NAMESPACE" \
  --values haptic-values-0.4.yaml > /dev/null
```

The command prints nothing when your values pass. Otherwise Helm names each
rejected value by its path:

```text
Error: values don't meet the specifications of the schema(s) in the following chart(s):
haptic:
- at '/controller/service': additional properties 'tpye' not allowed
- at '/haproxy/ports/http': got string, want integer
```

Fix each reported value, then run the command again until it passes:

- **Unknown key:** correct the spelling, or delete the key. A key the chart
  moved or renamed fails with a message that names its replacement.
- **Wrong type:** remove the quotes from a number or a boolean, for example
  `"8080"` → `8080` and `"true"` → `true`.

Free-form maps keep accepting any key: labels, annotations, selectors,
`resources`, security contexts, affinity, and the top level of
`controller.config.templatingSettings.extraContext`.

### 3. Check auth-headers-request header names

`haproxy-haptic.org/auth-headers-request` and
`haproxy-ingress.github.io/auth-headers-request` no longer forward header names
that contain `_`. List them; this needs `kubectl` and `jq`:

```bash
kubectl get ingress --all-namespaces --output json |
  jq -r '.items[] | .metadata as $m | ($m.annotations // {}) | to_entries[]
    | select(.key == "haproxy-haptic.org/auth-headers-request"
        or .key == "haproxy-ingress.github.io/auth-headers-request")
    | select(.value | contains("_"))
    | "\($m.namespace)/\($m.name) \(.key): \(.value)"'
```

Replace `_` with `-` in each listed header name, and have clients send the
dashed header. The auth service needs no change: HAPTIC already delivered these
headers to it with `-` in their names.

**If you don't:** the header isn't forwarded to the auth service, the Ingress
gets an `InvalidAuthHeader` Warning Event, and the admission webhook denies new
or changed Ingresses that list one.

### 4. Review Gateway route timeouts

HAPTIC now ignores `rules[].timeouts.request` and uses only `backendRequest` for
HAProxy's server inactivity timeout, matching HAProxy Unified Gateway's field
mapping. Previously, `request` took precedence over `backendRequest`.

If you used `request: 30s` to set the server timeout, move that value to
`backendRequest` in the route rule:

```yaml
timeouts:
  backendRequest: 30s
```

If both fields are set, the existing `backendRequest` value now applies. If
`backendRequest` is absent, the backend default applies. A zero `backendRequest`
now uses HAProxy's maximum timeout (about 24.9 days) instead of the backend
default.

These settings limit inactivity. HAPTIC doesn't enforce an overall request
deadline across retries; see [Timeout limits](libraries/gateway.md#timeout-limits).

### 5. Monitor rejected resource changes

Alert when `haptic_rejected_watched_inputs` is greater than zero. HAPTIC now keeps
an invalid resource change out of otherwise valid updates, so healthy traffic
doesn't prove every intended change took effect. A rejected credential rotation
or policy update retains the previous validated behavior. See
[Rejected resource changes](operations/input-isolation.md) for diagnosis and
restart behavior.

### 6. Upgrade the release

Pass the checked values file. `--reset-values` starts from the new chart's
defaults, so the release doesn't carry rejected keys forward:

```bash
helm upgrade "$HAPTIC_RELEASE" \
  oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
  --namespace "$HAPTIC_NAMESPACE" --version 0.4.1 \
  --reset-values \
  --values haptic-values-0.4.yaml
```

## Upgrading to 0.3

Use this checklist to upgrade a 0.2 release. Work through it from top to bottom;
each step says what to change, and what fails if you don't. If you run 0.1.0 or
a 0.2.0 alpha, complete [Upgrading to 0.2](#upgrading-to-02) first.

Confirm that the target version is available on the
[releases page](https://github.com/haproxy-haptic/HAPTIC/releases) before
running the upgrade commands. Development documentation can describe a release
before its artifacts are published.

### 1. Save your values

Set the installed Helm release and namespace:

```bash
HAPTIC_RELEASE=haptic
HAPTIC_NAMESPACE=haptic
```

Export the values you supplied to Helm, and copy them into a working file for
the upgrade:

```bash
umask 077
helm get values "$HAPTIC_RELEASE" --namespace "$HAPTIC_NAMESPACE" \
  --output yaml > haptic-values-before.yaml
cp haptic-values-before.yaml haptic-values-0.3.yaml
```

If the export contains only `null`, replace it with an empty map:

```bash
if [ "$(cat haptic-values-0.3.yaml)" = null ]; then
  printf '{}\n' > haptic-values-0.3.yaml
fi
```

Apply the following steps to `haptic-values-0.3.yaml`. If you manage values in
Git, apply them to that source too.

### 2. Rename the timeout and SSL redirect keys

Under `controller.config.templatingSettings.extraContext`, rename these keys:

| 0.2 key | 0.3 key |
|---------|---------|
| `timeout_connect` | `timeoutConnect` |
| `timeout_client` | `timeoutClient` |
| `timeout_server` | `timeoutServer` |
| `timeout_http_request` | `timeoutHttpRequest` |
| `timeout_http_keep_alive` | `timeoutHttpKeepAlive` |
| `ssl_redirect_default` | `sslRedirectDefault` |

`sslRedirectDefault` is a boolean. Write `true`, not `"true"`:

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        timeoutConnect: "5000"
        sslRedirectDefault: true
```

**If you don't:** Helm refuses to install or upgrade the chart, for example:

```text
controller.config.templatingSettings.extraContext.timeout_connect was renamed in 0.3.0 and no longer has any effect. Rename it to timeoutConnect.
```

A quoted `sslRedirectDefault: "true"` fails the same way, because a string
would never turn the redirect on.

### 3. Move the HSTS max-age

`extraContext.hapticHstsMaxAge` is removed. `haproxy-haptic.org/hsts` without
`haproxy-haptic.org/hsts-max-age` now sends `max-age` from `tls.hsts.maxAge`,
which defaults to one year (`31536000`) instead of two.

If you set `hapticHstsMaxAge`, or you want to keep the two-year default, set
`tls.hsts.maxAge`:

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        tls:
          hsts:
            maxAge: "63072000"
```

`tls.hsts.maxAge` also sets the `max-age` of the global HSTS header when
`tls.hsts.enabled` is `true`.

**If you don't:** a leftover `hapticHstsMaxAge` fails the install with the same
"was renamed in 0.3.0" error as step 2. Without it, Ingresses using
`haproxy-haptic.org/hsts` send `max-age=31536000`.

### 4. Replace basic-auth hashes HAPTIC now refuses

Every `auth-secret` annotation (`haproxy-haptic.org`, `haproxy.org`,
`haproxy-ingress.github.io`, and `nginx.ingress.kubernetes.io`) accepts only
bcrypt, SHA-256 crypt, SHA-512 crypt, and yescrypt hashes by default. It
refuses `$apr1$` (Apache MD5), `{SHA}`, `$1$` (MD5-crypt),
Data Encryption Standard (DES) crypt, and plaintext credentials.

Preflight validation doesn't read your cluster's Secrets, so check them before
upgrading. This lists every user whose hash the 0.3 default refuses; it needs
`kubectl` and `jq`:

```bash
pattern='^\$(2[aby]|5|6|y)\$[./0-9A-Za-z$=]+$'
kubectl get ingress --all-namespaces --output json |
  jq -r '.items[] | .metadata as $m | ($m.annotations // {}) | to_entries[]
    | select(.key | endswith("/auth-secret"))
    | (if (.value | contains("/")) then .value else "\($m.namespace)/\(.value)" end)
      + " " + $m.namespace + "/" + $m.name' |
  sort -u |
  while read -r secret ingress; do
    kubectl get secret --namespace "${secret%%/*}" "${secret#*/}" --output json |
      jq -r --arg secret "$secret" --arg ingress "$ingress" --arg pattern "$pattern" '
        .data
        | if has("auth") then
            .auth | @base64d | split("\n")[] | rtrimstr("\r")
            | select(contains(":")) | capture("^(?<user>[^:]*):(?<hash>.*)$")
          else
            to_entries[] | {user: .key, hash: (.value | @base64d)}
          end
        | select(.hash | test($pattern) | not)
        | "Ingress \($ingress): Secret \($secret), user \(.user)"'
  done
```

For each user listed, generate a new hash and write it into the Secret in the
format the Secret already uses. Each command prompts for the password:

| Format | Command |
|--------|---------|
| bcrypt | `htpasswd -nB <user>` |
| SHA-512 crypt | `openssl passwd -6` |
| SHA-256 crypt | `openssl passwd -5` |
| yescrypt | `mkpasswd -m yescrypt` |

**If you don't:** rendering fails with an error that names the Ingress, the
Secret, and the user. The admission webhook denies new or changed Ingresses
that reference such a Secret, and while one is in the cluster, HAPTIC applies
no configuration changes. To keep accepting a format while you migrate, widen
the patterns as shown in
[Basic-auth password hashes](operations/security.md#basic-auth-password-hashes).

### 5. Check host alias annotations

`haproxy-haptic.org/host-alias-regex` and `haproxy-ingress.github.io/server-alias-regex`
must now match the whole hostname; in 0.2 they matched any part of it. List every
regex alias; this needs `kubectl` and `jq`:

```bash
kubectl get ingress --all-namespaces --output json |
  jq -r '.items[] | .metadata as $m | ($m.annotations // {}) | to_entries[]
    | select(.key == "haproxy-haptic.org/host-alias-regex"
        or .key == "haproxy-ingress.github.io/server-alias-regex")
    | "\($m.namespace)/\($m.name) \(.key): \(.value)"'
```

For each regex that should match only part of a hostname, widen it to cover the
rest. For example, `example\.com` becomes `.*example\.com`, and `example` becomes
`.*example.*`. A regex that already starts with `^` and ends with `$` needs no
change. HAPTIC also rejects a regex whose groups or `[ ]` classes don't close
within it.

Exact aliases (`haproxy-haptic.org/host-alias`, `haproxy-ingress.github.io/server-alias`,
and `nginx.ingress.kubernetes.io/server-alias`) must now be valid hostnames,
optionally starting with `*.`; case doesn't matter. List the aliases HAPTIC now
refuses:

```bash
kubectl get ingress --all-namespaces --output json |
  jq -r '.items[] | .metadata as $m | ($m.annotations // {}) | to_entries[]
    | select(.key == "haproxy-haptic.org/host-alias"
        or .key == "haproxy-ingress.github.io/server-alias"
        or .key == "nginx.ingress.kubernetes.io/server-alias")
    | .key as $key
    | .value | split(if $key == "haproxy-haptic.org/host-alias" then "[, ]" else "," end; null)[]
    | gsub("^\\s+|\\s+$"; "") | select(. != "")
    | select(test("^(\\*\\.)?[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$"; "i") | not)
    | "\($m.namespace)/\($m.name) \($key): \(.)"'
```

Remove or correct each listed alias. A port, a path, or an underscore can't be
part of an alias.

**If you don't:** a regex that relied on matching part of a hostname stops
matching, and requests for those hostnames no longer reach the Ingress. An
invalid alias or regex is skipped with an `InvalidHostAlias`,
`InvalidServerAlias`, `InvalidServerAliasRegex`, `InvalidHostAliasRegex` or
`InvalidAnnotationValue` Warning Event, and the admission webhook denies new or
changed Ingresses that carry one.

### 6. Update custom templates

If your values add templates, snippets, or libraries, make these changes:

| 0.2 | 0.3 |
|-----|-----|
| `http.Fetch` option `delay` | `interval` |
| `strings_replace(s, old, new)` | `replace(s, old, new)` |
| `trim(s)` to strip whitespace | `strip(s)` |

`trim` is now the `trim(s, cutset)` builtin in every render path. To find
candidates in your values file:

```bash
grep -nE 'strings_replace|trim\(|delay' haptic-values-0.3.yaml
```

**If you don't:** the configuration fails to load, and [preflight](#10-validate-the-candidate)
reports the error. An `http.Fetch` call that sets `delay` fails with
`option "delay" was removed, so this call fails. Rename it to "interval"`.

### 7. Update custom validation tests

Assertions in `validationTests` no longer see your `templatingSettings.extraContext`.
They render with the libraries' defaults, `testExtraContext` (which the chart
sets from its default values), `_global`'s `extraContext`, and the test's own
`extraContext`. A test that asserts output of a value you set must set that
value itself:

```yaml
controller:
  config:
    validationTests:
      test-my-timeouts:
        extraContext:
          timeoutConnect: "5000"
        # fixtures and assertions unchanged
```

Each test also renders its fixtures a second time with your `extraContext`.
That render must succeed and, where the test asserts `haproxy_valid`, pass
`haproxy -c`. See [Extra context](validation-reference.md#extra-context).

**If you don't:** a test asserting one of your values fails, and so does a
test whose fixtures your values break. Either failure stops the configuration
from loading, and preflight reports it, for example
`This test's fixtures fail to render with the deployment's extraContext`.

### 8. Prepare for HTTP/3

0.3 enables [HTTP/3](haproxy-deployment.md#http3-quic) by default. Every
TLS-terminating HTTPS listener also listens on UDP, and HTTPS responses
advertise it with an `alt-svc` header. The HAProxy Service gains a UDP port
named `https-quic` with the `https` port's number and nodePort, and each
Gateway's Service gains a UDP port for each TLS-terminating HTTPS listener. The chart's
NetworkPolicy allows it.

Before upgrading, choose one:

- Allow UDP on the HTTPS port (443 by default), or on its nodePort, through
  firewalls, security groups, and load balancers in front of HAProxy. A
  `LoadBalancer` Service now mixes TCP and UDP ports, so check that your
  load-balancer implementation supports that.
- Keep HAProxy TCP-only:

    ```yaml
    controller:
      config:
        templatingSettings:
          extraContext:
            http3:
              enabled: false
    ```

If your load balancer publishes UDP on a different port than TCP, set
`http3.altSvc.port`; see [HTTP/3 (QUIC)](haproxy-deployment.md#http3-quic).

**If you don't:** where UDP is blocked, clients fall back to HTTP/2 or
HTTP/1.1 over TCP after trying QUIC. An entry named `https-quic` in
`haproxy.service.extraPorts` fails the install; rename it or turn HTTP/3 off.

### 9. Check deployment-specific settings

Skip each item that doesn't apply to you.

**Varnish cache (`cache.varnish.enabled: true`).** Varnish runs as a bundled
non-root image that needs the `IPC_LOCK` capability. Kubernetes Baseline and
Restricted Pod Security Standards reject it, so arrange a policy exception with
your cluster administrator before upgrading. If you set `cache.varnish.image`,
remove the override to use the bundled image, or supply an image with
`cap_ipc_lock=ep` on `varnishd`. **If you don't:** your cluster's policy rejects
the Varnish pods. See [shared-log memory](operations/response-cache.md#shared-log-memory).

**Controller probe overrides.** The chart probes controller readiness on
`/readyz` and liveness on `/livez`. If your values set
`controller.readinessProbe.httpGet.path` or `controller.livenessProbe.httpGet.path`,
change them:

```yaml
controller:
  readinessProbe:
    httpGet:
      path: /readyz
  livenessProbe:
    httpGet:
      path: /livez
```

**If you don't:** probes on `/healthz` mark the leading replica unready and
restart it while a new configuration fails to load, so admission requests are
denied.

**Your own controller NetworkPolicy.** If you replace the chart's
NetworkPolicy, allow controller pods to reach each other on the health port
(`controller.ports.healthz`, default 8080).

**Monitoring.** `haptic_events_dropped_total` is removed. Query
`haptic_events_dropped_critical_total`, which counted the same drops.

**Custom agent clients.** If your own code calls the agent's `/v1/apply`, set
`identity_version: 1` in the manifest. **If you don't:** the agent rejects the
manifest with `400`. HAPTIC's own controller already sets it.

### 10. Validate the candidate

Use the `haptic` binary matching the chart version to run
[preflight validation](operations/validate-before-deploy.md):

```bash
helm pull oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
  --version 0.3.0 --untar --untardir ./haptic-0.3-chart
haptic preflight --values ./haptic-values-0.3.yaml \
  --chart ./haptic-0.3-chart/haptic --expect-chart-version 0.3.0 \
  --namespace "$HAPTIC_NAMESPACE" --release "$HAPTIC_RELEASE"
```

Fix every reported error and run it again until it passes.

0.3 adds the `testExtraContext` field to the `HAProxyTemplateConfig` and
`HAProxyTemplateLibrary` CRDs. Keep the chart's CRD upgrade hook enabled. If you
manage CRDs separately, apply the target chart's schemas before upgrading:

```bash
helm show crds oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
  --version 0.3.0 | kubectl apply --server-side --force-conflicts -f -
```

### 11. Upgrade the release

Pass the complete migrated values file. `--reset-values` starts from the new
chart's defaults, so removed keys aren't carried forward from the installed
release:

```bash
helm upgrade "$HAPTIC_RELEASE" \
  oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
  --namespace "$HAPTIC_NAMESPACE" --version 0.3.0 \
  --reset-values \
  --values haptic-values-0.3.yaml
```

If validation rejects the candidate, fix the reported value or template and
repeat the upgrade. For rollout failures, see
[Recover a failed upgrade](deploying-with-helm.md#recover-a-failed-upgrade).

### 12. Verify the deployment

Wait for the controller and HAProxy deployments:

```bash
kubectl --namespace "$HAPTIC_NAMESPACE" get deployments \
  --selector "app.kubernetes.io/instance=$HAPTIC_RELEASE" --output name | \
while read -r deployment; do
  kubectl --namespace "$HAPTIC_NAMESPACE" rollout status "$deployment" --timeout=7m
done
```

The `HAProxyTemplateConfig` should report `Validated=True`; check any `False`
condition on `HAProxyCfg`:

```bash
kubectl --namespace "$HAPTIC_NAMESPACE" get haproxytemplateconfig,haproxycfg -o yaml
```

Test existing HTTP and HTTPS routes, including authentication and custom
annotations.

### Behavior changes to review

These need no action unless you depend on the old behavior:

- **Response headers on generated responses.** The chart sets `Server`, HSTS,
  Ingress custom response headers, and routing diagnostic headers with
  `http-after-response`, so error pages, denials, and redirects that HAProxy
  generates or replaces carry them too. A snippet that changes one of these
  headers with `http-response` runs before the chart's rule; use
  `http-after-response` instead.
- **HSTS default.** `haproxy-haptic.org/hsts` without `hsts-max-age` sends one
  year instead of two; see [step 3](#3-move-the-hsts-max-age).
- **nginx-ingress regex paths.** With `use-regex` or `rewrite-target`, a path
  containing regex syntax matches as a case-insensitive regex anchored at the
  path's start, and `$N` in `rewrite-target` refers to its capture groups. In
  0.2 the path matched as a literal prefix. See
  [`use-regex`](libraries/nginx-ingress.md#nginxingresskubernetesiouse-regex).
- **Host alias conflicts.** An exact alias for a hostname that an older Ingress
  already claims, as a rule host or alias, is no longer routed, and the newer Ingress
  gets a `RouteConflict` Warning Event. See [Host aliases claim hostnames](operations/security.md#host-aliases-claim-hostnames).
- **A failing configuration change.** While a new configuration fails to load,
  the leading controller replica keeps serving and validating the previous one,
  so admission keeps working. Other replicas report unready and restart.
  `/healthz` still reports the failure. See
  [health checks](development/debug-endpoints.md#health-checks-during-configuration-changes).

## Upgrading to 0.2

Use this guide to upgrade from 0.1.0 or a 0.2.0 alpha to a stable 0.2 release. The controller
and chart share one version. The chart replaces the HAProxy Data Plane API with
the HAPTIC agent and changes several values paths.

Confirm that the target version is available on the [releases page](https://github.com/haproxy-haptic/HAPTIC/releases)
before running the upgrade commands. Development documentation can describe a
release before its artifacts are published.

### Check requirements

- Kubernetes 1.33 or newer. The HAProxy pod uses native sidecars to keep its
  agent, Stream Processing Offload Agent (SPOA) hub, and log collector running
  until HAProxy exits.
- Capacity for the whole installation and its rolling upgrade. The defaults
  request about 5.4 GiB of memory, rising to 8.1 GiB during rollout. The
  pre-rollout validation Job has a separate 1 GiB limit. Use the
  [resource sizing guidance](operations/performance.md#controller-resource-sizing)
  for larger installations or customized pod resources.
- Access to the controller registry from HAProxy pods: their agent now uses
  the HAPTIC image. Set image-pull credentials if your cluster requires them.

The chart supports HAProxy 3.0–3.4. Keep your selected `haproxyVersion` unless
you intend to change it; the default is 3.4. See
[HAProxy versions](operations/haproxy-versions.md) for runtime-update coverage.

### Save your values

Set the installed Helm release and namespace, and the 0.2 release to upgrade to:

```bash
HAPTIC_RELEASE=haptic
HAPTIC_NAMESPACE=haptic
HAPTIC_VERSION=0.2.2
```

Export the values you supplied to Helm:

```bash
umask 077
helm get values "$HAPTIC_RELEASE" --namespace "$HAPTIC_NAMESPACE" \
  --output yaml > haptic-values-before.yaml
```

Copy the exported values, which include settings supplied with `--set`, into a
working file for the upgrade:

```bash
cp haptic-values-before.yaml haptic-values-0.2.yaml
```

If the export contains only `null`, replace it with an empty map:

```bash
if [ "$(cat haptic-values-0.2.yaml)" = null ]; then
  printf '{}\n' > haptic-values-0.2.yaml
fi
```

Keep `haptic-values-before.yaml` as the record of your previous configuration.
If you manage values in Git, also apply the migrations below to that source so
subsequent deployments keep them.

### Migrate 0.1.0 values

For an alpha installation, some migrations may already be applied. Check the
values you use against the table and review the changed defaults before validation.

For 0.1.0, update the paths you use in `haptic-values-0.2.yaml`:

| Previous value | Replacement |
| --- | --- |
| Root controller workload settings, such as `image`, `replicaCount`, `resources`, `webhook`, `monitoring`, `networkPolicy`, probes, and autoscaling | The same setting under `controller.*` |
| Root pod settings, such as `imagePullSecrets`, `nodeSelector`, `tolerations`, `affinity`, `podAnnotations`, and `podSecurityContext` | The same setting under `controller.podSpec.*` |
| HAProxy pod settings, such as `haproxy.nodeSelector`, `.tolerations`, `.affinity`, `.podAnnotations`, and `.podSecurityContext` | The same setting under `haproxy.podSpec.*` |
| `controller.crdName` | `controller.configName` |
| `controller.debugPort` | `controller.ports.healthz` |
| `controller.defaultSSLCertificate` | `defaultSSLCertificate` |
| `controller.config.dataplane.port` | `haproxy.ports.dataplane` |
| `controller.config.routing.regexMatchOrder` | `controller.config.templatingSettings.extraContext.routing.regexMatchOrder` |
| `controller.templateLibraries.pathRegexLast.enabled: true` | `controller.config.templatingSettings.extraContext.routing.regexMatchOrder: last` |
| `haproxy.dataplane.logLevel`, `.resources`, `.extraEnv`, and `.service` | The same setting under `haproxy.agent.*` |

For example, controller replicas and a custom default certificate now use:

```yaml
controller:
  replicaCount: 2
defaultSSLCertificate:
  secretName: my-default-certificate
  certManager:
    enabled: false
```

Remove Data Plane API-only values (`haproxy.dataplane.validateConfig`,
`debugSocketPath`, `aclFormat`, and `haproxy.dataplaneBin`),
`haproxy.enterprise.version`, and
`controller.config.templatingSettings.extraContext.serverSlots`.
`haproxyVersion` selects the Enterprise revision when Enterprise is enabled.

Remove `spoaHub.plugins.otel`. Configure tracing through
[the tracing values](reference.md#logging-and-templating) instead.

The chart rejects legacy paths with their replacements. Run the validation step
below to find remaining paths, including renamed `extraContext` settings.

### Review changed defaults and integrations

The default IngressClass and GatewayClass names change from `haproxy` to
`haptic`. To preserve routes that use the 0.1.0 defaults, add these values:

```yaml
ingressClass:
  name: haproxy
gatewayClass:
  name: haproxy
```

Keep your existing class names if you already customized them. Alternatively,
update the class references on your Ingress and Gateway resources to `haptic`.

Enable each vendor annotation library that your existing routes use. For example,
to retain support for ingress-nginx annotations:

```yaml
controller:
  templateLibraries:
    nginxIngress:
      enabled: true
```

Use `haproxyIngress.enabled` for `haproxy-ingress.github.io/*` and
`haproxytech.enabled` for `haproxy.org/*` under the same `templateLibraries` map.
The native `haproxy-haptic.org/*` library remains enabled by default.

Review these behavior changes before upgrading:

- Access logs use JSON. Adapt custom log parsers or override the log-format
  snippets; see [access logging](operations/access-logging.md).
- Ingress serves HTTPS using the default certificate. Set
  `controller.config.templatingSettings.extraContext.ingressDefaultHTTPS: false`
  if you require plaintext-only Ingress without an explicit TLS declaration.
- HAProxy response compression is opt-in. If you used an alpha version's automatic
  compression, set `haproxy-haptic.org/compress-enable: "true"` on appropriate
  routes. Avoid compressing responses that combine secrets with attacker-controlled
  input; see [compression settings](libraries/haptic-annotations.md#compression).
- Hash-based balancing uses consistent hashing. Existing key distribution can
  change during the upgrade.
- Request retries that replay a request apply to idempotent methods by default.
  Review custom retry settings before restoring retries for other methods.
- The backend connection timeout defaults to 100 ms instead of 5 s. For slower
  networks, set `controller.config.templatingSettings.extraContext.timeout_connect`
  to `"5000"` to retain the previous timeout in milliseconds.
- HAProxy's HTTP/HTTPS container ports default to 80/443 instead of 8080/8443.
  Update integrations that address pod ports directly, or retain the previous
  ports through `haproxy.ports.http` and `haproxy.ports.https`.

The webhook uses a chart-managed self-signed certificate by default. To retain
cert-manager renewal, set `controller.webhook.certManager.enabled: true` and
keep cert-manager installed. The chart uses a new default webhook Secret name
to avoid conflicting with the old cert-manager-owned Secret.

Update integrations that invoke `haptic-controller` to invoke `haptic`. Replace
Data Plane API integrations with the [agent interface](development/agent.md)
and update dashboards using the [metric migration table](operations/metrics-reference.md#where-the-old-metrics-went).

If you maintain custom templates, replace reads of parsed `currentConfig`
sections with `currentConfig.ServerIndex` for previous servers, and use
`currentServers` in validation fixtures. Use `toJSON` instead of implicit string
conversion for maps and slices. Pass the watched resource object to
[`statusPatch(resource, variants)`](template-reference.md#statuspatch) instead of
separate namespace, name, API version, and kind arguments.

If you process rendered manifests, accept one `HAProxyTemplateConfig` and its
referenced `HAProxyTemplateLibrary` objects. `haptic config view --input` merges
them for inspection. Helm owns these objects; put persistent overrides in your
values file.

### Validate the candidate

Use the `haptic` binary matching the chart version below to run
[preflight validation](operations/validate-before-deploy.md):

```bash
helm pull oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
  --version "$HAPTIC_VERSION" --untar --untardir ./haptic-0.2-chart
haptic preflight --values ./haptic-values-0.2.yaml \
  --chart ./haptic-0.2-chart/haptic --expect-chart-version "$HAPTIC_VERSION" \
  --namespace "$HAPTIC_NAMESPACE" --release "$HAPTIC_RELEASE"
```

Keep the default CRD upgrade and pre-rollout validation hooks enabled. If your
deployment manages CRDs separately, apply the target chart's schemas before upgrading:

```bash
helm show crds oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
  --version "$HAPTIC_VERSION" | kubectl apply --server-side --force-conflicts -f -
```

GitOps diff tools can need these CRDs before they can map the new library
resources, because diff runs before Helm's hooks.

### Upgrade the release

Pass the complete migrated values file. `--reset-values` starts from the new
chart's defaults before applying that file, so removed legacy settings aren't
carried forward from the installed release:

```bash
helm upgrade "$HAPTIC_RELEASE" \
  oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
  --namespace "$HAPTIC_NAMESPACE" --version "$HAPTIC_VERSION" \
  --reset-values \
  --values haptic-values-0.2.yaml
```

If validation rejects the candidate, fix the reported value or template and
repeat the upgrade. Keep validation enabled. For rollout failures and rollback
limitations, see [Recover a failed upgrade](deploying-with-helm.md#recover-a-failed-upgrade).

### Verify the deployment

Wait for both controller and HAProxy deployments:

```bash
kubectl --namespace "$HAPTIC_NAMESPACE" get deployments \
  --selector "app.kubernetes.io/instance=$HAPTIC_RELEASE" --output name | \
while read -r deployment; do
  kubectl --namespace "$HAPTIC_NAMESPACE" rollout status "$deployment" --timeout=7m
done
```

Inspect configuration validation and per-pod deployment status. The
`HAProxyTemplateConfig` should report `Validated=True`; check any `False`
condition on `HAProxyCfg` for a rejected configuration or failed deployment:

```bash
kubectl --namespace "$HAPTIC_NAMESPACE" get haproxytemplateconfig,haproxycfg -o yaml
```

Test existing HTTP and HTTPS routes, including authentication and custom
annotations. A successful Helm command alone doesn't prove traffic has converged.
See [troubleshooting](troubleshooting.md) if a pod or route remains unavailable.
