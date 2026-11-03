# HAProxyRoutePolicy reference

Use `HAProxyRoutePolicy` to configure authentication, shared rate limits, a web
application firewall (WAF), or HTTP caching for HTTPRoute and GRPCRoute rules.
The chart installs the CRD at API version `haproxy-haptic.org/v1alpha1`.
For a worked example, see [Protect Gateway routes](operations/gateway-policies.md).

## Attachment

A rule attaches one policy through an `ExtensionRef` filter. Set `group` to
`haproxy-haptic.org`, `kind` to `HAProxyRoutePolicy`, and `name` to the policy name.
The policy must be in the route's namespace. Multiple policy filters on one rule,
backend-level attachments, and SSL passthrough combined with a policy are rejected.
There is no inherited policy or implicit merge.

A policy must configure at least one of `authentication`, `rateLimit`, `waf`, or
`cache`. See [Protect Gateway routes](./operations/gateway-policies.md) for a
complete example.

## Authentication

When both `authentication.jwt` and `authentication.apiKey` are configured, both
checks must pass. Missing or invalid request credentials return 401; invalid
policy configuration returns 503 for the affected rule.

Credential Secrets must set [`immutable: true`](https://kubernetes.io/docs/concepts/configuration/secret/#immutable-secrets). Kubernetes prevents changes to
their contents. Admission validates credentials when you attach a policy or
change an attached policy.
Rotate credentials by creating a new immutable Secret and changing `secretRef.name`;
see [Rotate credentials](./operations/gateway-policies.md#rotate-credentials).
A deleted Secret or a mutable replacement makes the affected rules return 503.

### JWT

JSON Web Tokens (JWTs) use the `Authorization: Bearer` header. The Secret named by `secretRef.name`
must be in the policy's namespace and contain one PEM public key in `pubkey.pem`.
RSA keys require at least 2048 bits. ECDSA curves must match the selected algorithm.

| Field under `authentication.jwt` | Default | Meaning |
| --- | --- | --- |
| `secretRef.name` | Required | Public-key Secret |
| `algorithm` | `RS256` | `RS256`, `RS384`, `RS512`, `PS256`, `PS384`, `PS512`, `ES256`, `ES384`, or `ES512` |
| `issuer` | Unset | Required `iss` value when configured |
| `audience` | Unset | Required `aud` string or exact member of an `aud` string array; identifiers starting with `[` aren't supported |
| `resourceMetadata` | Unset | `{configMapRef: {name, key}, path}` serves a same-namespace ConfigMap as public OAuth metadata and adds a Bearer discovery challenge to JWT `401` responses. The key defaults to `metadata.json`; the path defaults to the resource-path discovery URL. |
| `allowedGroups` | Unset | Permit an exact match in the group claim's string or string array; otherwise return `403`. Up to 32 names of 1–256 characters from `!` (U+0021) through `~` (U+007E), without a leading `[` |
| `groupsClaim` | `groups` when `allowedGroups` is set | Top-level claim name; up to 128 letters, digits, underscores, or hyphens |
| `requiredClaims` | `[]` | Claims that must be present; use `[exp, sub]` for expiring tokens with a consumer identity |
| `forwardClaims` | `[]` | List of `{claim, header}` entries copied into request headers after verification |

Present `exp` and `nbf` claims are checked. JWT `sub` supplies the authenticated
consumer identity. Forwarded headers must be distinct and can't overwrite
`Authorization` or an API-key credential header.

### API key

| Field under `authentication.apiKey` | Default | Meaning |
| --- | --- | --- |
| `secretRef.name` | Required | Same-namespace Secret containing a `keys` entry |
| `header` | `X-API-Key` | Request header carrying the key |
| `consumerHeader` | Unset | Header receiving the verified consumer identity |

`keys` contains newline-separated `key:consumer` entries. A bare key uses itself
as the consumer identity. Duplicate keys with different consumers are invalid.
A configured consumer header replaces the caller's value. When JWT and API-key
authentication both apply, JWT `sub` supplies the consumer identity for forwarded
headers, rate limits, and cache partitioning. The API-key consumer is used when
the JWT has no `sub` claim.

## Shared rate limit

Requires `rateLimit.shared.enabled=true` and the shared rate-limit SPOA plugin.
The chart can manage Valkey with `rateLimit.shared.managedStore.enabled=true`.

| Field under `rateLimit` | Default | Meaning |
| --- | --- | --- |
| `requests` | Required | Positive request budget per period |
| `period` | `1s` | Positive duration with `ms`, `s`, `m`, `h`, or `d` suffix |
| `burst` | `requests` | Positive burst capacity |
| `algorithm` | `token-bucket` | `token-bucket` uses distributed leases; `gcra` checks the shared store for each decision |
| `key` | `ip` | Partition by source IP or authenticated `consumer` |

A policy's budget is shared by all its attached rules and replicas. The refill
horizon, `burst × period ÷ requests`, must not exceed 3600 seconds. Consumer limits
require authentication and reject requests with no authenticated consumer.

## WAF

`waf.policy` selects a web application firewall (WAF) policy. With no
`waf.catalogRef`, it selects an administrator's inline policy or a policy from an
immutable trusted catalog. `waf.catalogRef.name` selects an immutable ConfigMap in
the route policy's namespace; `waf.catalogRef.key` defaults to `policies.yaml`.
Explicit catalog references require `waf.policies.selfService.enabled=true`.

Rotate a catalog by creating a new immutable ConfigMap and changing the policy
reference. Admission validates the affected routes before accepting the change.
Kubernetes rejects data updates to the old catalog. Missing, mutable, or malformed
catalogs fail closed, including after deletion and recreation under the same name.
Existing WAF permissions and limits apply, including
`waf.ingressPermissions.allowPolicySelection`. Distinct catalog entries share the
namespace and cluster budgets with Ingress catalogs. A self-service policy can't
weaken a `default-on` deny baseline. The route policy doesn't override enforcement
mode.

Requires `spoaHub.plugins.coraza.enabled=true`. GRPCRoute requires the selected
catalog policy's `requestBody.mode` to be `none`. See the
[WAF catalog documentation](./operations/waf-policies.md).

## HTTP response cache

Requires `cache.varnish.enabled=true`. Caching applies to GET and HEAD requests;
GRPCRoute rejects this setting.

| Field under `cache` | Default | Meaning |
| --- | --- | --- |
| `ttlSeconds` | Origin `Cache-Control` | Non-negative cache lifetime in seconds |
| `maxObjectSizeBytes` | Cache default | Positive maximum object size |
| `varyHeaders` | `[]` | Additional request headers included in the cache key |

Each route rule has a separate cache partition, including rules that rewrite to
the same origin URL. Authenticated caching also partitions by verified consumer
and rejects requests without one. Key components are encoded separately so
separator characters can't merge distinct identities. Reserved `X-Haptic-*`
headers can't be used as `varyHeaders`.

## Filter order and failures

Authentication, quota checks, and WAF inspection precede a configured
`RequestRedirect`. Gateway CORS preflight handling returns before authentication;
browsers don't send application credentials in preflight requests. Actual requests
still pass the attached policy.

Missing and invalid dependencies deny the affected rule with 503. Route status
sets `Accepted=False` when all rules are invalid, `PartiallyInvalid=True` when
some remain valid, and `ResolvedRefs=False` for unresolved policy or credential
references. Fixing the dependency restores enforcement without changing the
attachment.

JWT `resourceMetadata` uses the same [document and discovery rules](libraries/haptic-annotations.md#oauth-protected-resource-discovery) as the Ingress annotations. Set the document's `resource` to an HTTPS URL on the route's host. Discovery is scoped to the Gateway listeners that accept the route. Removing or invalidating a referenced ConfigMap makes the policy invalid; protected traffic remains denied.
