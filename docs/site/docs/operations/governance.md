# Set defaults and enforce routing policies

Use governance rules to require annotations, supply defaults, or constrain values
on watched resources. Define named rules under
`controller.config.templatingSettings.extraContext.governance` in your Helm values.
Your rules merge with those supplied by libraries; disable one by setting its
`enabled` field to `false`.

Start in audit mode to find affected resources, then enable rejection when those
resources meet your policy. The [rule reference](../reference.md#policy-guardrails-governance)
lists the available checks.

## How a rule behaves

Each rule names an entry in `watchedResources` and applies to its matching objects:

- **Supply a default:** If `path` is absent, use the rule's `default` when generating configuration. This doesn't modify the Kubernetes object.
- **Check a value:** Apply the rule's constraints, such as a required annotation, an allowed list, or a numeric range.

A rule's `enforcement` controls what a *violation* does:

- `audit` — records a `GovernanceViolation` Warning Event on the resource and keeps serving. Nothing is blocked.
- `reject` — denies a **new or edited** violating resource at the admission webhook. An already-present violator isn't blocked; it records the same Warning Event and keeps serving.

An existing violation doesn't block changes to other resources.

## Require a WAF policy on every Ingress

This example requires every Ingress to select a Web Application Firewall (WAF)
policy. First [define a WAF policy](waf-policies.md) that your Ingresses can select. Start the guardrail in audit mode, fix reported violations, then enable rejection.

### 1. Turn the rule on in audit mode

Merge the rule into your [complete values file](../deploying-with-helm.md#change-settings)
and apply it. These commands use release `haptic` in namespace `haptic`:

```yaml
# haptic-values.yaml
controller:
  config:
    templatingSettings:
      extraContext:
        governance:
          enabled: true
          rules:
            ingress-waf-policy:
              enabled: true
              resource: ingresses
              path: metadata.annotations['haproxy-haptic.org/waf-policy']
              required: true
              enforcement: audit
```

```bash
helm upgrade haptic oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
  --version 0.6.0 --namespace haptic --values haptic-values.yaml
```

If you deploy with GitOps, commit the values change and let your controller apply it.

### 2. See which resources violate

List the Warning Events the audit rule recorded:

```bash
kubectl get events --all-namespaces --field-selector reason=GovernanceViolation
```

Each event names one Ingress that has no `haproxy-haptic.org/waf-policy` annotation, and the message states the rule it broke.

### 3. Fix the flagged resources, then enforce

For each flagged Ingress, either add the annotation:

```bash
read -r -p "Ingress namespace: " ingress_namespace
read -r -p "Ingress name: " ingress_name
read -r -p "WAF policy name: " waf_policy
kubectl annotate ingress "$ingress_name" --namespace "$ingress_namespace" \
  "haproxy-haptic.org/waf-policy=$waf_policy" --overwrite
```

or exempt its namespace from the guardrail (see [Exempt namespaces](#exempt-namespaces) below).

Once you have corrected or exempted the reported resources, switch the rule to
`reject` and apply. Events expire, so an empty Event list alone doesn't prove
that every resource complies:

```yaml
# haptic-values.yaml — same rule, now enforcing
controller:
  config:
    templatingSettings:
      extraContext:
        governance:
          enabled: true
          rules:
            ingress-waf-policy:
              enabled: true
              resource: ingresses
              path: metadata.annotations['haproxy-haptic.org/waf-policy']
              required: true
              enforcement: reject
```

```bash
helm upgrade haptic oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
  --version 0.6.0 --namespace haptic --values haptic-values.yaml
```

### 4. Confirm enforcement

Send a non-compliant Ingress to the admission webhook without creating it:

```bash
kubectl apply --dry-run=server -f - <<'EOF'
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: governance-check
  namespace: default
spec:
  ingressClassName: haptic
  rules:
    - host: governance-check.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service: { name: example, port: { number: 80 } }
EOF
```

The webhook should deny the request with a message naming the governance rule.
If it reports a different problem, resolve that first and repeat the check.

## Inject a safe default instead of requiring

Rather than reject Ingresses that lack a policy, give them one automatically. This rule injects a detect-mode WAF policy on every Ingress that doesn't already select one; Ingresses that already have a policy keep theirs.

```yaml
# haptic-values.yaml
controller:
  config:
    templatingSettings:
      extraContext:
        waf:
          policies:
            inline:
              baseline-detect:
                description: OWASP CRS in detect mode — monitor only, never blocks
                enforcement: detect
        governance:
          enabled: true
          rules:
            ingress-waf-policy:
              enabled: true
              resource: ingresses
              path: metadata.annotations['haproxy-haptic.org/waf-policy']
              default: baseline-detect
```

Detect mode records WAF matches without denying requests for those matches.
Review WAF metrics and audit logs before selecting deny mode; see [Security](security.md).

## Require every Ingress to terminate TLS

`satisfiedBy: tls` passes when the Ingress has a `spec.tls` block **or** the chart-wide default HTTPS is on (`extraContext.ingressDefaultHTTPS`, the default). It fails only for an Ingress that would be served plaintext.

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        governance:
          enabled: true
          rules:
            ingress-tls:
              enabled: true
              resource: ingresses
              satisfiedBy: tls
              enforcement: audit
```

## Clamp a value to a ceiling

`min`/`max` bound a numeric annotation. With `onViolation: clamp`, an out-of-range value is rewritten to the nearest bound for that render instead of being rejected. The Kubernetes object keeps its original value; the generated configuration
uses the bounded value.

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        governance:
          enabled: true
          rules:
            rate-limit-ceiling:
              enabled: true
              resource: ingresses
              path: metadata.annotations['haproxy-haptic.org/rate-limit-rps']
              max: 5000
              onViolation: clamp
```

## Exempt namespaces

`exemptNamespaces` skips a namespace entirely — no rule is checked, injected, or enforced there. Use it for infrastructure or system namespaces that shouldn't answer to tenant policy. To exempt a namespace from one rule only, see [Scope a rule to namespaces](#scope-a-rule-to-namespaces).

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        governance:
          enabled: true
          exemptNamespaces: [kube-system, monitoring]
          rules:
            ingress-waf-policy:
              enabled: true
              resource: ingresses
              path: metadata.annotations['haproxy-haptic.org/waf-policy']
              required: true
              enforcement: reject
```

## Scope a rule to namespaces

To apply one rule to some namespaces only, set namespace lists on that rule.
The other rules aren't affected.

- `exemptNamespaces` skips the rule in the listed namespaces and applies it everywhere else, including namespaces created later.
- `namespaces` applies the rule only in the listed namespaces. A namespace that isn't listed, including one created later, isn't checked.

Use `exemptNamespaces` for a rule that protects the cluster, so a new namespace is covered by default.
An omitted or empty list doesn't narrow the rule. A namespace can't be in both lists of the same rule.
The global `exemptNamespaces` still applies on top of both lists.

This example lets only the `platform` namespace set a host alias and requires
a `tier` label only in the tenant namespaces:

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
            tenant-tier:
              enabled: true
              resource: ingresses
              path: metadata.labels['tier']
              required: true
              namespaces: [tenant-a, tenant-b]
```

`pattern: '^$'` forbids a value: any non-empty annotation fails the rule.

## Govern any watched resource

Rules are generic — set `resource` to any name in your `watchedResources`, not just `ingresses`. The same rule shape governs `httproutes`, a custom CRD, or anything else HAPTIC watches:

```yaml
rules:
  httproute-owner:
    enabled: true
    resource: httproutes
    path: metadata.labels['team']
    required: true
    enforcement: audit
```

## The rule the chart ships

One governance rule is enabled out of the box: `haptic-compress-enable` injects
`haproxy-haptic.org/compress-enable: "false"` on an Ingress that doesn't set it.
[Response compression](../libraries/haptic-annotations.md#compression) is opt-in.
Change the rule's `default` to `"true"` to enable it across Ingresses that don't
choose their own value.

## Switch off a single rule

Set `enabled: false` on a named rule to disable it while preserving the other
rules, including those supplied by template libraries:

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

Every rule must set `enabled`; omitting it causes rendering to fail.

## See also

- [Policy guardrails (governance)](../reference.md#policy-guardrails-governance) — every rule field, in the Chart Values Reference
- [Security](security.md) — annotation input as a trust boundary, WAF policies, and the audit trail
- [Watching Resources](../watching-resources.md) — the `watchedResources` a rule's `resource` refers to
