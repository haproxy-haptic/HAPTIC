# Share WAF policies across applications

Define a web application firewall (WAF) policy once, then let application teams
select it with an Ingress annotation. HAPTIC uses [Coraza](https://coraza.io/)
to inspect requests against the policy and the Open Worldwide Application Security Project (OWASP)
Core Rule Set (CRS).

If your administrator has already configured a policy catalog, add its
case-sensitive policy name to your Ingress:

```yaml
metadata:
  annotations:
    haproxy-haptic.org/waf-policy: public-web
```

The following sections cover catalog setup, policy tuning, and permissions.
For individual route settings, see the [WAF annotations](../libraries/haptic-annotations.md#authentication-mtls-and-waf).

## Configure a trusted catalog

Add this reference to your [complete Helm values file](../deploying-with-helm.md#change-settings).
Create the referenced ConfigMap before applying the values:

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        waf:
          policies:
            configMapRefs:
              security:
                namespace: security
                name: haptic-waf-policies
                key: policies.yaml
```

Create the `security` namespace if it doesn't exist:

```bash
kubectl create namespace security --dry-run=client -o yaml | kubectl apply -f -
```

Save the following as `haptic-waf-policies.yaml`:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  namespace: security
  name: haptic-waf-policies
data:
  policies.yaml: |
    public-web:
      requestBody:
        mode: none
      enforcement: deny
    json-api:
      requestBody:
        mode: json
        maxBytes: 4096
      enforcement: deny
```

Apply the ConfigMap:

```bash
kubectl apply -f haptic-waf-policies.yaml
```

Then [apply your Helm values](../deploying-with-helm.md#change-settings).

Configuring a catalog activates Coraza and the Ingress permission checks. By
default, Ingress authors can select a policy but can't disable inspection,
change enforcement, or inject custom WAF or HAProxy rules. Protect the ConfigMap
with Kubernetes RBAC: anyone who can edit it can change the approved policies.
To keep the trusted catalog in your Helm values, use `policies.inline` instead
of a ConfigMap.

The default dispatch mode, `opt-in`, inspects annotated routes. Set
`waf.dispatch.mode: default-on` to inspect every route. In either mode,
`waf.dispatch.defaultEnforcement` sets the enforcement of every policy that
leaves out `enforcement`, and of `default-on` routes without a policy.
See the [WAF settings reference](../reference.md#coraza-waf)
for all defaults and limits.

## Tune a policy

Start with the smallest change that resolves a false positive:

| Field | Use it to |
|-------|-----------|
| `description` | Explain which applications should select the policy. |
| `extends` | Build on another policy and state only what differs; see [Extend a policy](#extend-a-policy). |
| `enforcement` | Block matches with `deny`, or record them with `detect`. Leave it out to use `waf.dispatch.defaultEnforcement`. |
| `requestBody` | Choose whether to inspect bodies and set their size limit; see below. |
| `allowedMethods`, `paranoiaLevel`, `anomalyThreshold` | Adjust allowed methods and CRS sensitivity. |
| `crsSettings` | Extend allowed content types or set `maxFileSize`, `maxNumArgs`, and `totalArgLength`. |
| `ruleExclusions` | Exclude a rule ID, optionally on a path, or exclude a variable from a rule ID or CRS tag. |
| `secLang` | Add custom Coraza directives where structured settings aren't enough. |

For example, a Git server can permit its upload media types while retaining CRS
content-type checks for other requests:

```yaml
crsSettings:
  allowedRequestContentTypes:
    - application/x-git-upload-pack-request
    - application/x-git-receive-pack-request
```

Use `ruleExclusions` when changing CRS inputs isn't enough. Path selectors are
`onPathPrefix`, `onPathSuffix`, `onPathExact`, and `onPathContains`. They match
the request path the way CRS rules such as 930130 inspect it:

- The query string is never part of the match: `onPathExact: /api/health` matches
  `/api/health?verbose=1`, and `/.env?x=/api/health` doesn't match.
- Percent-encoding is decoded once, so `/%61pi/` matches `onPathPrefix: /api/`.
  Write the selector as a decoded path; `%` and `?` are rejected.
- `\` counts as `/`, and `//`, `/./`, and `/../` are resolved before matching:
  `/api/../.env` doesn't match `onPathPrefix: /api/`. A selector containing
  `//`, `/./`, or `/../` is rejected.
- Matching is case-sensitive.
- A request whose decoded path contains `;`, `?`, `%`, `#`, a control character,
  or a character outside plain English letters, digits, and punctuation (such
  as `é`) never gets the exclusion. Path parameters such as `/api/..;/.env`,
  double encoding, and null bytes keep every rule active.

This exclusion allows SQL-like search text in `q` while keeping the tagged
checks on other inputs:

```yaml
ruleExclusions:
  - tags: [attack-sqli]
    excludeTarget: "ARGS:q"
```

Policy authors can't override HAPTIC's body-safety rules. Per-Ingress
[nginx-compatible custom rules](../libraries/nginx-ingress.md) are subject to
`waf.customRules.limits`, including when no catalog is configured.

## Extend a policy

Set `extends` to build an application's policy on a shared baseline. The derived
policy states only what differs, and later changes to the baseline reach it
without editing it. This policy lets a Git server's upload media types through
the CRS content-type check and stops rule 930130 from inspecting `.git/` paths:

```yaml
public-web:
  enforcement: deny
  paranoiaLevel: 2
  requestBody:
    mode: none
git-server:
  extends: public-web
  crsSettings:
    allowedRequestContentTypes:
      - application/x-git-upload-pack-request
  ruleExclusions:
    - rules: [930130]
      excludeTarget: REQUEST_FILENAME
      onPathContains: ".git/"
```

`git-server` keeps `enforcement`, `paranoiaLevel`, and `requestBody` from
`public-web`. The derived policy's fields combine with the base like this:

| Field | Result |
|-------|--------|
| `description`, `enforcement`, `paranoiaLevel`, `allowedMethods` | The derived value replaces the base value. |
| `requestBody` | Replaced as a whole, because `maxBytes` is valid only with the `mode` it belongs to. |
| `anomalyThreshold` | Merged by key; setting `inbound` keeps the base `outbound`. |
| `crsSettings` | Merged by key; `allowedRequestContentTypes` entries are added to the base entries, other keys replace the base value. |
| `ruleExclusions` | The derived entries are added after the base entries. |
| `secLang` | The derived directives are appended after the base directives. |

A field neither policy sets keeps its usual default; for `enforcement`, that's
`waf.dispatch.defaultEnforcement`.

`extends` is one level deep: the base must be a policy from your Helm values or a
trusted ConfigMap catalog, and must not set `extends` itself. A derived policy
can live in a different catalog from its base. Every validation and limit applies
to the combined policy. If the base's ConfigMap is missing or invalid, routes
that select the derived policy return `503` until the base is restored. A Gateway
route can't select a derived policy whose base comes from a ConfigMap without
`immutable: true`.

## Choose request-body inspection

| `requestBody.mode` | Behavior |
|--------------------|----------|
| `none` | Inspect request metadata without buffering or limiting uploads. Use for gRPC streaming. |
| `any` | Inspect a complete body within the configured size limit. |
| `json` | Apply the same size limit and require a JSON media type. |

Enforced body inspection requires an unambiguous `Content-Length` and rejects
oversized or incomplete bodies before Coraza. It never inspects only a prefix
and passes an unchecked remainder.

A policy's `requestBody.maxBytes` defaults to
`waf.policies.requestBody.defaultMaxBytes` and can't exceed
`waf.policies.requestBody.maxBytes`. The separate ceiling lets you approve a
larger policy without enlarging every policy that uses the default. Both HAProxy
and Coraza enforce the effective limit. It must fit within the shared
[HAProxy request buffer](../reference.md#request-body-inspection-and-json-schema-validation).

### WAF and gRPC streaming

Use `requestBody.mode: none` for gRPC. Method, path, header, and source-IP checks
still run. Coraza's [body processors](https://coraza.io/docs/reference/body-processing/)
don't decode protobuf or gRPC messages.

Body inspection waits for the complete request, preventing the backend from
processing a stream as messages arrive. An enforced client-streaming or
bidirectional call that exceeds `waf.policies.requestBody.waitTimeout` receives
`408`. Unary calls still face buffering, size limits, and content-type checks.
Detect mode skips body-contract rejections and waits only when `Content-Length`
is present; an unbounded body remains uninspected.

Admission rejects body-inspecting policies on routes declared as `grpc`/`grpcs`
or nginx-compatible `GRPC`/`GRPCS`, including detect policies. An existing
combination emits `WafBodyPolicyOnGRPCRoute` and retains its runtime rules,
including body rejections. Change the policy to `mode: none`. Plain `h2`/`h2-ssl`
doesn't trigger this check because HTTP/2 also carries non-gRPC applications.

Use authentication, source-IP restrictions, and rate limits for gRPC access
control. `allowed-methods` filters HTTP methods, not gRPC method paths;
`max-request-body-size` limits an HTTP body, not individual gRPC messages.
Configure message-size limits in the gRPC application.

## Require a policy on every Ingress

To require a fixed baseline, configure a trusted default policy and prevent
Ingress authors from selecting alternatives:

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        waf:
          ingressPermissions:
            allowPolicySelection: false
            allowEnforcementOverride: false
            allowWafDisable: false
            allowCustomRules: false
            allowRawHAProxyConfig: false
          policies:
            defaultPolicy: public-web
```

Define `public-web` in your trusted catalog before applying these values.
Removing WAF annotations then leaves the default policy in force. HAPTIC rejects
native and vendor annotations that bypass these permissions. Raw HAProxy
configuration is checked across all Ingresses because one frontend or global
snippet can affect other routes.

Grant `allowCustomRules` only to trusted policy authors: SecLang can disable or
rewrite rules. `allowRawHAProxyConfig` grants configuration-administrator
capabilities and takes effect only while `extraContext.ingressPermissions.allowRawHAProxyConfig`
is also `true`; see [Raw configuration annotations](security.md#raw-configuration-annotations).
Enforcement overrides and complete WAF opt-outs have separate permissions;
granting one doesn't grant the other.

## Self-service namespaced policies

Enable namespace-local policy catalogs when application teams should maintain
their own policies:

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        waf:
          policies:
            selfService:
              enabled: true
```

Each team creates a `waf-policies` ConfigMap in its namespace:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  namespace: team-a
  name: waf-policies
data:
  policies.yaml: |
    app-baseline:
      requestBody:
        mode: none
      ruleExclusions:
        - tags: [attack-sqli]
          excludeTarget: "ARGS:q"
```

The policy leaves out `enforcement`, so it uses the administrator's
`waf.dispatch.defaultEnforcement`. To resolve a false positive, exclude the
request field that triggers a rule, as above, rather than disabling the whole
rule.

An Ingress in `team-a` selects it with
`haproxy-haptic.org/waf-policy: app-baseline`. HAPTIC watches ConfigMaps with this
name and enables Coraza and policy permissions automatically.

To adjust a trusted policy for one application, extend it and narrow what a rule
inspects rather than removing the rule. This policy keeps every `public-web`
setting and stops rule 942100 from inspecting the `id` query argument:

```yaml
search:
  extends: public-web
  ruleExclusions:
    - rules: [942100]
      excludeTarget: "ARGS:id"
```

| Boundary | Behavior |
|----------|----------|
| Namespace | A team can select its own policies and trusted policies. Cross-namespace names such as `team-a/app-baseline` are rejected. |
| Name conflicts | A local name that duplicates a trusted policy blocks that namespace's selection; it never silently replaces the trusted policy. |
| Cluster baseline | `defaultPolicy` resolves only from trusted catalogs. With `default-on`/`deny`, a self-service policy can't weaken enforcement to `detect`, including through a base it extends. |
| Enforcement | A self-service policy whose `enforcement`, set or inherited from its base, differs from `waf.dispatch.defaultEnforcement` is rejected unless `waf.ingressPermissions.allowEnforcementOverride` is `true`. |
| Extending | A self-service policy can [extend](#extend-a-policy) a trusted policy, not another namespace policy. It inherits the base's `secLang`, but `selfService.allowSecLang` still governs its own. Limits apply to the combined policy. |
| Body size and catalog growth | Administrator limits cap body sizes, policies per namespace, and total policies. |
| Custom rules | `selfService.allowSecLang` defaults to `false`. Count and size limits don't bound rule CPU cost. |

## Diagnose a rejected policy

Admission rejects invalid definitions and selections. If an invalid selection
already exists, HAPTIC returns `503` for affected routes and emits a Warning
Event. For an invalid self-service catalog, only selecting routes in that
namespace are affected; other namespaces continue rendering.

Inspect the Ingress and catalog ConfigMap Events. `WafPolicyCatalogInvalid`
identifies malformed catalog YAML; `WafPolicyInvalid` identifies a broken policy.
Fix the named definition or select an existing policy. Plugin outages have a
separate [failure policy](../reference.md#coraza-waf).
