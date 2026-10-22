# Security

Configure permissions, credentials, pod security, and network access for your
HAPTIC installation. Review the defaults below before exposing production traffic.

## What the controller needs

### RBAC

The Helm chart creates a ServiceAccount, a ClusterRole, and a Role in the release
namespace. Role-based access control (RBAC) permissions depend on the enabled
libraries and watched resources.

The **ClusterRole** grants:

| Resource | Verbs | Purpose |
|----------|-------|---------|
| Each watched resource | get, list, watch | Read the resources declared by the configuration and enabled libraries |
| Watched resources with `statusPatch: true`, via `/status` | patch | Publish routing and policy conditions |
| `customresourcedefinitions` | get, list, watch | Load schemas and detect CRD changes |
| `events` | create, update, patch, delete | Publish template-generated Kubernetes Events |
| `services` | get, list, watch, create, update, patch, delete | Gateway library only: manage Services for Gateway listeners |
| `gatewayclasses` | create, update, patch, delete | Gateway library only: manage the configured GatewayClass |
| `pods`, and `pods/status` | get, list, watch; patch | [Pod readiness gate](pod-readiness-gate.md) only: read backend pods in every namespace and set the gate condition |

The **namespace Role** grants:

| Resource | Verbs | Purpose |
|----------|-------|---------|
| `pods` | get, list, watch | Discover the HAProxy fleet |
| `secrets` | get, list, watch, create, update, patch, delete | Read agent credentials and manage certificate Secrets |
| `leases` | get, create, update | Coordinate leadership |
| `haproxytemplateconfigs`, `haproxytemplatelibraries` | get, list, watch | Load the configuration and its libraries |
| `haproxytemplatelibraries` | patch | Set ownership references |
| `haproxytemplateconfigs/status` | update, patch | Report configuration validation status |
| `haproxycfgs`, `haproxymapfiles`, `haproxygeneralfiles`, `haproxycrtlistfiles` | get, list, watch, create, update, patch, delete | Publish and prune rendered artifacts |
| The four artifact kinds above, via `/status` | update, patch | Report deployment status |
| `services` | get, list, watch, create, update, patch, delete | Manage Services in the release namespace |
| `configmaps` | get, list, watch, create, update, patch, delete | Managed Varnish only: publish Varnish Configuration Language (VCL) |
| `statefulsets`, `deployments` | get, list, watch, create, update, patch, delete | Managed Varnish or Valkey only: manage workloads |
| `poddisruptionbudgets` | get, list, watch, create, update, patch, delete | Managed Varnish or Valkey with disruption budgets enabled |
| `horizontalpodautoscalers` | get, list, watch, create, update, patch, delete | Managed Varnish with autoscaling enabled |

Inspect your release's rendered grants with `helm get manifest haptic -n haptic`.
If you set `controller.rbac.create: false`, maintain these permissions yourself.
The chart sources are `templates/clusterrole.yaml`, `templates/role.yaml`, and
`templates/watched-resource-roles.yaml`. A watched resource that lists
`namespaces` gets a Role in each of those namespaces instead of a ClusterRole rule.

[Watch selectors](../watching-resources.md#narrowing-the-watch) limit what templates
consume; they don't narrow the ServiceAccount's RBAC permissions.

### Read Secrets only in the release namespace

By default the ClusterRole lets the controller read every Secret in the cluster,
because an Ingress or Gateway can reference a TLS or authentication Secret in its
own namespace. RBAC can't limit access by Secret type. If your routes use only the
default or a wildcard certificate from the release namespace, and no per-route
certificate or authentication Secrets, limit Secret access to that namespace:

```yaml
controller:
  rbac:
    releaseNamespaceSecretsOnly: true
```

With this setting:

- The ClusterRole has no `secrets` rule. A Role and RoleBinding ending in
  `-watch` grant `get`, `list`, and `watch` on Secrets in the release namespace.
- The controller watches Secrets only in the release namespace.
- The admission webhook rejects an Ingress, Gateway, route, or policy that
  references a Secret in another namespace:

  ```text
  secrets team-a/web-tls cannot be read: controller.rbac.releaseNamespaceSecretsOnly limits the secrets watch to haptic. Reference secrets in haptic, or change controller.rbac.releaseNamespaceSecretsOnly
  ```

- `defaultSSLCertificate.namespace` must stay empty, which keeps the default
  certificate in the release namespace.

Before you turn it on, remove Secret references outside the release namespace.
When the controller loads this setting, it withholds an object that still has
such a reference, so its routes stop serving, and records an `InputRejected` event. See
[rejected resource changes](input-isolation.md).

Changing this setting on an existing installation updates permissions and watch
configuration separately. During a Helm upgrade or GitOps sync, cluster-wide
Secret access can be removed before every controller has loaded the narrower
watch. That ordering can produce Secret-watch `Forbidden` warnings; the setting
doesn't provide an atomic transition between permissions and watches.

Confirm the narrowed access:

```bash
kubectl auth can-i list secrets --all-namespaces \
  --as=system:serviceaccount:haptic:haptic
```

The answer is `no`. Replace `haptic:haptic` with your release namespace and the
controller's ServiceAccount name.

If warnings continue after the upgrade or sync, check the applied watch scope
and the permissions in the release namespace. These commands use the default
configuration name and the `haptic` release and namespace:

```bash
kubectl --namespace haptic get haproxytemplateconfig haptic-config \
  -o jsonpath='{.spec.watchedResources.secrets.namespaces}{"\n"}'
kubectl auth can-i list secrets --namespace haptic \
  --as=system:serviceaccount:haptic:haptic
kubectl auth can-i watch secrets --namespace haptic \
  --as=system:serviceaccount:haptic:haptic
```

Expect only `haptic` in the watch scope and two `yes` answers. A different scope
or a missing grant needs a configuration or RBAC correction. These checks don't establish
which scope each running watcher uses. If they match but warnings continue,
collect logs from each controller replica for further diagnosis.

### Credentials

The chart encrypts controller-to-agent requests with mutual TLS (mTLS). Each end
checks its peer's certificate authority and distinct DNS subject alternative name.
Agent control requests require the controller identity; a Basic-auth password
doesn't grant access in TLS mode. Both peers require TLS 1.3.

The chart renews its agent certificates automatically. Monitor renewal failures
and protect the issuer Secret, which contains the private certificate-authority
key. To supply your own certificates, set `haproxy.agent.tls.managed: false` and
manage their renewal yourself. See [agent certificates](agent-certificates.md)
for Secret formats, rotation, and recovery.

The chart still creates the Secret named by `--secret-name` or `SECRET_NAME` for
controller bootstrap compatibility. Its `dataplane_username` and
`dataplane_password` authenticate the agent only when
`haproxy.agent.tls.enabled: false`. That mode uses plain HTTP. The controller
reloads password changes live; legacy agents read them at startup, so password
rotation requires a coordinated pod rollout and can interrupt configuration
updates to a mixed fleet.

!!! warning "Secrets with offline Helm rendering"
    `helm template` and Argo CD can't preserve generated Secrets through `lookup`.
    Supply a stable `credentials.dataplane.password` through your deployment's
    secret management. Agent TLS identities are generated at runtime and remain
    stable across offline renders.

Debug endpoints expose configuration and rendered files, which can contain
credentials. Limit `pods/portforward` access to trusted operators. See
[accessing diagnostics](debugging.md#accessing-the-server).

## Pod hardening

The chart ships with a restrictive default pod spec. The relevant `securityContext` (container-level) / `controller.podSpec.podSecurityContext` (pod-level) defaults:

| Setting | Default |
|---------|---------|
| `runAsNonRoot` | `true` |
| `runAsUser` / `runAsGroup` / `fsGroup` | `65532` (`nonroot`) |
| `readOnlyRootFilesystem` | `true` |
| `allowPrivilegeEscalation` | `false` |
| `capabilities.drop` | `[ALL]` |
| `seccompProfile.type` | `RuntimeDefault` |

The controller writes temporary files (for `haproxy -c` validation) to `/tmp`, which is mounted as an `emptyDir`. Everything else is read-only.

The chart is compatible with the "restricted" Pod Security Standard out of the box:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: haptic
  labels:
    pod-security.kubernetes.io/enforce: restricted
    pod-security.kubernetes.io/warn: restricted
```

### HAProxy pod hardening

The HAProxy pods the chart deploys also run restricted: `runAsNonRoot: true`, `allowPrivilegeEscalation: false`, `capabilities.drop: [ALL]`, and `seccompProfile.type: RuntimeDefault` — compatible with the restricted Pod Security Standard. The chart auto-derives the UID from `haproxy.enterprise.enabled` and applies it identically as `runAsUser`, `runAsGroup`, and `fsGroup`: community images use `99` (the `haproxy` user), enterprise images use `1000` (the `hapee-lb` user).

!!! warning "HAProxy binds privileged ports 80 and 443"
    HAProxy binds the literal `:80` and `:443`. Community images drop all capabilities and run non-root, so binding these privileged ports relies on the node permitting unprivileged low-port binds — the kernel `sysctl` `net.ipv4.ip_unprivileged_port_start` must be `<= 80` (the default in kind and Docker). On a cluster that keeps the kernel default of `1024`, either lower that `sysctl` or add `CAP_NET_BIND_SERVICE` to the HAProxy container's capabilities. Enterprise images run as UID `1000` and their binaries carry `CAP_NET_BIND_SERVICE` file capabilities, so the chart adds that capability automatically when `haproxy.enterprise.enabled: true`.

## Network exposure

The controller pod exposes these ports by default:

| Port | Endpoint | Notes |
|------|----------|-------|
| `8080` | `/healthz`, `/readyz`, `/livez`, `/debug/vars`, `/debug/events`, `/debug/pprof/` | `controller.ports.healthz` configures the process, pod, Service, probes, and policy together. The health paths serve probes and controller replicas, which read each other's `/healthz`; `/debug/*` accepts loopback connections only. Restrict `pods/portforward` with RBAC |
| `9090` | `/metrics` | `controller.ports.metrics` configures the process, pod, Service, and monitors together; set it to `0` to disable metrics |
| `9443` | Validating webhook over HTTPS | Required when the webhook is enabled |

Outbound, the controller talks to the Kubernetes API server and to the agent on
each HAProxy pod (default port `5555`). Mutual TLS protects deployment requests,
including the TLS private keys carried as auxiliary files. Disabling agent TLS
requires equivalent network encryption, such as an encrypted Container Network
Interface (CNI) or service mesh.

NetworkPolicies are enabled by default. Two defaults need attention when you
restrict access:

- Controller egress allows connections to all cluster pods. Replace
  `controller.networkPolicy.egress.additionalRules` with the destinations your
  templates need.
- `haproxy.networkPolicy.allowExternal: true` allows ingress on all HAProxy pod
  ports, including dynamically configured Gateway listeners.

Controller metrics ingress is disabled until you set
`controller.networkPolicy.ingress.monitoring.enabled: true`. See
[network access](networking.md) for complete rules, cache and rate-limit store
policies, and Prometheus selectors.

## Forwarded headers

Backends such as GitLab and Keycloak take the client address from
`X-Forwarded-For` and the scheme from `X-Forwarded-Proto`. HAPTIC sets these
headers itself on every HTTP listener, for Ingress and Gateway routes alike, so a
client can't forge them:

| Header | What the backend receives |
|--------|---------------------------|
| `X-Forwarded-For` | The client address. A value the client sent is dropped. |
| `X-Forwarded-Proto` | `https` when HAPTIC terminated TLS, otherwise `http`. |
| `X-Forwarded-Host`, `X-Forwarded-Port` | Nothing. A value the client sent is dropped. |

With [PROXY protocol](../haproxy-deployment.md#proxy-protocol), the client
address is the one the load balancer reports.

HAPTIC doesn't set `X-Forwarded-Port`: its listeners bind container ports behind
the HAProxy Service, so the port it sees isn't always the port the client used.
`X-Forwarded-Host` would repeat the `Host` header.

HAPTIC removes or replaces client `X-Forwarded-Host`, `-Port`, and `-Proto`
before operator `frontend-extra-*` snippets run, so these snippets can set their
own values.
`X-Forwarded-For` remains subject to the [per-route policy](../libraries/base.md#forwarded-headers),
which runs after these snippets.

### Trust an L7 load balancer

If an L7 load balancer or a content delivery network in front of HAPTIC sets
these headers, list its source addresses. From a listed source, HAPTIC keeps all
four headers and appends the address it received the request from to
`X-Forwarded-For`. It still sets `X-Forwarded-Proto` when the request has none.

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        forwardedHeaders:
          trustedProxies:
            - 10.0.0.0/8
            - 2001:db8::/32
```

List only the proxies: a client that connects from a listed address can set
every one of these headers.

### Per-route exceptions

An Ingress can choose its own `X-Forwarded-For` handling with
[`haproxy-haptic.org/forwardfor`](../libraries/haptic-annotations.md#headers-cors-and-access-control),
[`haproxy-ingress.github.io/forwardfor`](../libraries/haproxy-ingress.md#haproxy-ingressgithubioforwardfor),
or [`haproxy.org/forwarded-for`](../libraries/haproxytech.md#haproxyorgforwarded-for).
The values that keep the client's header (`add`, `ifmissing`, and `ignore` for
the native annotation) bring forged addresses back for that route's backend.
An Ingress whose `src-ip-header` names `X-Forwarded-For` already trusts that
header, so HAPTIC keeps the client's `X-Forwarded-For` for its routes.

## Secrets in templates

Templates can read Secrets included in your watches. Decode `.data` values with
`b64decode`. This example reads a password hash into an HAProxy userlist; it
doesn't enable authentication on a route by itself. For that, use the
[basic-auth setup](../annotations.md#quick-start-basic-authentication).

<div class="pg-embed" markdown data-tab="haproxy.cfg" data-controls="tabs,resources" data-focus="20" data-title="Watched Secret → userlist" data-height="480">

<p class="pg-task">Run the example and find the decoded password hash in the generated userlist.</p>

```yaml
apiVersion: haproxy-haptic.org/v1alpha1
kind: HAProxyTemplateConfig
metadata:
  name: secret-userlist-demo
spec:
  watchedResources:
    secrets:
      apiVersion: v1
      resources: secrets
      indexBy:
        - metadata.namespace
        - metadata.name
  haproxyConfig:
    template: |
      global
        log stdout format raw local0
      defaults
        mode http
        timeout connect 5s
        timeout client 30s
        timeout server 30s
      {%- var secret = resources.secrets.GetSingle("auth", "basic-auth") %}
      {%- if secret != nil %}
      userlist authenticated_users
          user admin password {{ secret.data.password_hash | b64decode() }}
      {%- end %}
```

```yaml
apiVersion: v1
kind: List
items:
  - apiVersion: v1
    kind: Secret
    metadata:
      name: basic-auth
      namespace: auth
    data:
      password_hash: JDJ5JDA1JFp0MENrMXFZdzhwMXNRbTltUjNuUWVKOHlxM3ZKaEY1eEo4ZFEwb1YyYk43Y1gxa0w5bVNl
```

</div>

Store password hashes in the Secret. To generate a bcrypt hash interactively:

```bash
htpasswd -n -B -C 10 admin | cut -d: -f2
```

Hash checks cost CPU on authentication and configuration parsing. Choose a work factor for your authentication policy and measure it; see [Password hash performance](./performance.md#password-hash-performance).

## Basic-auth password hashes

The bundled basic-auth annotations accept these hash formats by default. Each
command prompts for the password:

| Format | Prefix | Generate with |
| --- | --- | --- |
| bcrypt | `$2a$`, `$2b$`, `$2y$` | `htpasswd -nB <user>` |
| SHA-512 crypt | `$6$` | `openssl passwd -6` |
| SHA-256 crypt | `$5$` | `openssl passwd -5` |
| yescrypt | `$y$` | `mkpasswd -m yescrypt` |

HAPTIC refuses every other credential with an error that names the Ingress, the
Secret, and the user. The admission webhook denies a new or changed Ingress that
references such a Secret. If a Secret already in the cluster holds one, HAPTIC
applies no configuration changes until you replace the hash. The default refuses:

- `$apr1$` (Apache MD5, the `htpasswd -m` format). HAProxy can't verify it, so
  every login would fail.
- `$1$` (MD5-crypt) and Data Encryption Standard (DES) crypt. HAProxy verifies them, but they're fast
  enough to crack offline, and DES crypt ignores everything after the eighth
  password character.
- Plaintext, including `user::password` lines in an htpasswd file.

To accept another format while you migrate, widen both patterns in your values.
This example also accepts `$1$`:

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        annotationCompatibility:
          basicAuth:
            passwordHashValidation:
              regex: '^\$(2[aby]|1|5|6|y)\$[./0-9A-Za-z$=]+$'
        hapticPasswordHashRegex: '^\$(2[aby]|1|5|6|y)\$[./0-9A-Za-z$=]+$'
```

`annotationCompatibility.basicAuth.passwordHashValidation.regex` applies to the
`haproxy.org`, `haproxy-ingress.github.io`, and `nginx.ingress.kubernetes.io`
annotations; `hapticPasswordHashRegex` applies to `haproxy-haptic.org`
annotations.

## Annotation input as a trust boundary

Most annotation values reach the config as validated or escaped data: CIDR-list annotations are parsed as CIDRs, and header, cookie, SNI, and rewrite-target values are checked against a strict character set that rejects control characters, so they can't break out of their directive and inject arbitrary HAProxy config. Invalid values reject the proposed resource change. If a watched change prevents complete configuration validation, [input isolation](input-isolation.md) keeps its previous validated revision while independent valid changes continue.

[Raw configuration annotations](#raw-configuration-annotations) are the exception, and HAPTIC rejects them by default.

### Raw configuration annotations

These annotations insert their value into `haproxy.cfg` verbatim:

| Prefix | Annotations |
|---|---|
| `haproxy-haptic.org/` | `config-global`, `config-defaults`, `config-frontend`, `config-backend` |
| `haproxy-ingress.github.io/` | `config-global`, `config-defaults`, `config-frontend`, `config-backend` |
| `haproxy.org/` | `backend-config-snippet` |
| `nginx.ingress.kubernetes.io/` | `configuration-snippet` |

Their directives can affect every host the controller serves. The backend
annotations aren't narrower: a backend directive can serve any file HAProxy can
read, TLS private keys included, and the value can start a new `global` or
`defaults` section.

By default, the admission webhook denies a new or edited Ingress that sets one
of these annotations. An Ingress that already carries one is
[rejected](input-isolation.md): it gets an `InputRejected` Warning Event and
keeps its previous validated revision, or stays inactive if it has none. Other
Ingresses keep serving. An annotation of a disabled annotation library has no
effect and isn't rejected.

To accept raw configuration annotations, set:

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        ingressPermissions:
          allowRawHAProxyConfig: true
```

Set it only if everyone who can create or edit an Ingress in a watched namespace
may also edit the HAProxy configuration.

While [WAF governance](waf-policies.md#require-a-policy-on-every-ingress) is
active, `extraContext.waf.ingressPermissions.allowRawHAProxyConfig` must also be
`true`. Neither setting can accept what the other rejects:

| `ingressPermissions.allowRawHAProxyConfig` | WAF governance | `waf.ingressPermissions.allowRawHAProxyConfig` | Raw configuration annotations |
|---|---|---|---|
| `false` (default) | Active or inactive | `true` or `false` | Rejected |
| `true` | Inactive | `true` or `false` (ignored) | Accepted |
| `true` | Active | `false` (default) | Rejected |
| `true` | Active | `true` | Accepted |

### Host aliases claim hostnames

Host alias annotations let an Ingress serve hostnames that aren't in its rules:
`haproxy-haptic.org/host-alias`, `haproxy-haptic.org/host-alias-regex`,
`haproxy-ingress.github.io/server-alias`, `haproxy-ingress.github.io/server-alias-regex`,
and `nginx.ingress.kubernetes.io/server-alias`. HAPTIC routes them as follows:

- An exact alias claims its hostname unless an older Ingress already claims it, as a
  rule host or as an alias. The newer Ingress gets a `RouteConflict` Warning Event.
  The older claim keeps winning, so an alias also keeps another Ingress from taking
  that hostname later.
- An alias regex matches the whole hostname. It applies only to a hostname that no
  rule host, wildcard rule host, or exact alias matches, and never to a Gateway
  listener.

An Ingress in any watched namespace can therefore claim every hostname that no other
Ingress has claimed yet. If your tenants mustn't do that, allow these annotations only
in the namespaces you trust.

#### Restrict host aliases with governance

These [governance rules](governance.md#scope-a-rule-to-namespaces) reject a new or
edited Ingress that sets a host alias annotation outside the `platform` namespace.
Replace `platform` with your trusted namespaces:

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        governance:
          enabled: true
          rules:
            host-alias-platform-only:
              enabled: true
              resource: ingresses
              path: metadata.annotations['haproxy-haptic.org/host-alias']
              pattern: '^$'
              exemptNamespaces: [platform]
              message: host aliases are allowed only in platform namespaces
            host-alias-regex-platform-only:
              enabled: true
              resource: ingresses
              path: metadata.annotations['haproxy-haptic.org/host-alias-regex']
              pattern: '^$'
              exemptNamespaces: [platform]
              message: host aliases are allowed only in platform namespaces
            server-alias-platform-only:
              enabled: true
              resource: ingresses
              path: metadata.annotations['haproxy-ingress.github.io/server-alias']
              pattern: '^$'
              exemptNamespaces: [platform]
              message: host aliases are allowed only in platform namespaces
            server-alias-regex-platform-only:
              enabled: true
              resource: ingresses
              path: metadata.annotations['haproxy-ingress.github.io/server-alias-regex']
              pattern: '^$'
              exemptNamespaces: [platform]
              message: host aliases are allowed only in platform namespaces
            nginx-server-alias-platform-only:
              enabled: true
              resource: ingresses
              path: metadata.annotations['nginx.ingress.kubernetes.io/server-alias']
              pattern: '^$'
              exemptNamespaces: [platform]
              message: host aliases are allowed only in platform namespaces
```

A rule doesn't remove an alias from an Ingress that already has one: that Ingress
keeps serving the alias and gets a `GovernanceViolation` Warning Event. List those
Events and remove the annotations:

```bash
kubectl get events --all-namespaces --field-selector reason=GovernanceViolation
```

#### Restrict host aliases with a ValidatingAdmissionPolicy

To enforce the restriction in the Kubernetes API server, independently of HAPTIC, use a
`ValidatingAdmissionPolicy` (Kubernetes 1.30 or later). It denies the annotations
in every namespace that doesn't have the label `example.com/host-aliases=allowed`.
Make sure that tenants can't label their own namespaces.

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: restrict-host-alias-annotations
spec:
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
      - apiGroups: ["networking.k8s.io"]
        apiVersions: ["v1"]
        operations: ["CREATE", "UPDATE"]
        resources: ["ingresses"]
    namespaceSelector:
      matchExpressions:
        - {key: example.com/host-aliases, operator: NotIn, values: ["allowed"]}
  validations:
    - expression: >-
        !has(object.metadata.annotations) ||
        !object.metadata.annotations.exists(k, k in [
          'haproxy-haptic.org/host-alias', 'haproxy-haptic.org/host-alias-regex',
          'haproxy-ingress.github.io/server-alias', 'haproxy-ingress.github.io/server-alias-regex',
          'nginx.ingress.kubernetes.io/server-alias'])
      message: Host alias annotations are allowed only in namespaces labelled example.com/host-aliases=allowed.
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: restrict-host-alias-annotations
spec:
  policyName: restrict-host-alias-annotations
  validationActions: [Deny]
```

Label each namespace that may use host aliases:

```bash
read -r -p "Namespace allowed to use host aliases: " alias_namespace
kubectl label namespace "$alias_namespace" example.com/host-aliases=allowed
```

Like the governance rules, the policy checks only creates and updates; it doesn't
remove an alias from an existing Ingress.

## Admission validation coverage

The admission webhook renders the proposed state and runs `haproxy -c` before
accepting creates and updates for watched resources with
`enableValidationWebhook: true`. The bundled libraries enable this for Ingress
and Gateway routing resources and BackendTLSPolicy. See
[Webhook integration](../watching-resources.md#validating-webhook-scope).

The Gateway library leaves GatewayClass admission disabled because the controller
manages the configured class itself. GatewayClass changes therefore reach
reconciliation without a HAPTIC admission check; config-load tests don't validate
arbitrary future changes to live resources. For checks during reconciliation, see
[Render validation](./debugging.md#haproxy-refused-the-config-the-fleet-was-given-configvalidatedfalse).

## Audit trail

Kubernetes audit logs can record who changes HAPTIC configuration and which
Secrets the controller reads. A cluster administrator must add these rules to
the API server's [audit policy](https://kubernetes.io/docs/tasks/debug/debug-cluster/audit/);
this isn't a resource you apply with `kubectl`.

The following policy uses `Metadata` to record identities and operations without
recording configuration or Secret contents. It targets the default `haptic`
release in namespace `haptic`:

```yaml
apiVersion: audit.k8s.io/v1
kind: Policy
rules:
  - level: Metadata
    resources:
      - group: haproxy-haptic.org
        resources: ["haproxytemplateconfigs", "haproxytemplatelibraries"]
  - level: Metadata
    users: ["system:serviceaccount:haptic:haptic"]
    resources:
      - group: ""
        resources: ["secrets"]
```

Put these rules before broader rules in an existing policy: Kubernetes uses the
first matching rule. If you use a different release name or ServiceAccount,
check the account used by the controller and update the `users` entry:

```bash
kubectl get deployment haptic-controller --namespace haptic \
  -o jsonpath='{.spec.template.spec.serviceAccountName}{"\n"}'
```

## Checklist

Before exposing a HAPTIC deployment to production traffic:

- [ ] Default mutual TLS retained, with failed certificate-renewal Jobs monitored. If you supply external certificates, monitor their expiry and manage CA rotation.
- [ ] RBAC that limits `pods/portforward` access to loopback-only `/debug/*` endpoints.
- [ ] Watched-resource selectors scoped to the namespaces you intend to serve.
- [ ] `extraContext.ingressPermissions.allowRawHAProxyConfig` left `false` unless every Ingress writer may edit the HAProxy configuration.
- [ ] `controller.rbac.releaseNamespaceSecretsOnly: true` if your routes reference no Secrets outside the release namespace.
- [ ] Release namespace labelled with `pod-security.kubernetes.io/enforce=restricted`.
- [ ] NetworkPolicy allowing DNS, Kubernetes API, agent traffic, and any configured HTTP resources.
- [ ] `extraContext.forwardedHeaders.trustedProxies` lists every L7 proxy in front of HAPTIC, and nothing else.
- [ ] Audit policy in place for `HAProxyTemplateConfig` changes.

## See also

- [Networking](./networking.md) — NetworkPolicy mechanics: default rules, hardening, replacement policies
- [Monitoring](./monitoring.md) — signals for auth failures, webhook drops, leader flaps
- [Debugging](./debugging.md) — accessing `/debug/*` safely
- [High Availability](./high-availability.md) — leader election RBAC and lease ownership
