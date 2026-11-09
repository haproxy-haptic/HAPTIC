# ADR-0026: Policies for Gateway HTTP and gRPC rules

## Status

Accepted.

## Context

The native Ingress annotations configure authentication, fleet-wide rate limits,
WAF inspection, and shared caching. Gateway routes need the same enforcement
without copying Ingress annotations or adding resource-specific Go code.

Route names alone cannot identify enforcement state. An HTTPRoute and GRPCRoute
can share a namespace and name, and rules within either route can require
different policies. Backend names must also distinguish the target namespace.

## Decision

The chart defines a namespaced `HAProxyRoutePolicy` resource. An HTTPRoute or
GRPCRoute rule attaches one policy through an `ExtensionRef` filter with group
`haproxy-haptic.org`. The policy must be in the route's namespace. Credential
Secrets must be in the policy's namespace. Backend-level attachments and multiple
policy filters on one rule are rejected; there is no implicit merge or precedence.

Credential Secrets must be immutable. Route and policy admission validate their
contents before attachment, and Kubernetes rejects subsequent data updates.
Rotation creates a new immutable Secret and changes the policy reference. Every
reconciliation rechecks immutability and content, including after deletion and
recreation under the same name.

This replaces the unreleased broad Secret webhook: protecting credentials no
longer depends on a controller being available when a Secret changes. The policy
change still validates the complete affected route set before acceptance. Missing,
mutable, or malformed credentials fail closed during reconciliation. Mutable
frontend and operational TLS Secrets retain their existing validation and renewal
paths. Helm can write its release metadata while repairing a stopped controller.

Gateway WAF catalogs follow the same reference contract. `waf.catalogRef` selects
an immutable ConfigMap in the policy's namespace, with a key defaulting to
`policies.yaml`. Rotation creates a new immutable catalog and updates the policy
reference. Each catalog version has a distinct enforcement identity, so old and
new versions can coexist. Namespace and cluster policy limits count distinct
catalog entries, including the existing Ingress catalogs; sharing a reference
doesn't consume the budget twice. Inline administrator policies remain available;
external trusted catalogs must also be immutable when used by Gateway policies.

Kubernetes rejects catalog data edits even while the controller is stopped.
Route and policy admission validate new selections, and reconciliation rejects
missing, mutable, or invalid replacements under an existing name. This replaces
the unreleased ConfigMap webhook without exempting a mutable policy dependency.
Admission stays in the controller pods. Helm can write its operational ConfigMaps
and Secrets before running repair hooks; policy reference changes still fail
closed while admission is unavailable. Missing or invalid trusted catalogs also
fail closed for their Ingress consumers instead of aborting the entire render.
Admission continues to reject invalid catalogs and unresolved default policies.

A policy can combine source-address allow and deny lists, JWT and API-key
authentication, a shared rate limit, a WAF catalog selection, and HTTP caching.
Source lists are inline or loaded from an immutable same-namespace ConfigMap
under the same reference and rotation contract as WAF catalogs. Deny takes
precedence when both lists match, and invalid lists or dependencies fail closed
for the affected rule. Both authentication checks apply when both are configured.
WAF selection keeps the catalog's enforcement and tenant limits. GRPCRoute rejects
caching and WAF policies that buffer request bodies.

Rate limits apply across the HAProxy fleet. Reusing a policy shares its budget
across attached rules; source IP or authenticated consumer partitions the budget.
Cache entries remain separated by the selected origin and route rule. Each key
component is encoded before joining it to avoid delimiter collisions. Authenticated cached
responses also require a verified consumer identity in the cache key.

The existing engines accept normalized settings and route records through a
shared chart library. Ingress annotation parsing remains in its annotation
library; Gateway attachment and status handling live in the `gateway-policies`
library, loaded with Gateway support.
Neither adapter constructs fake resources for the other. Go continues to discover
schemas and watch resources through the existing generic interfaces.

Runtime policy lookups use an identity containing route kind, namespace, name,
and rule index. Human-readable access-log resource fields retain namespace/name.
Policy identity and route identity are separate: a shared rate budget belongs to
the policy, while filter selection and cache origin belong to the route rule.

Missing or invalid attached policies fail closed for the affected rule. Admission
rejects changes that introduce invalid attachments or dependencies. Reconciliation
must also handle invalid objects that predate admission and dependencies removed
after attachment; these cannot block unrelated routes or retain stale permission.
Route status reports unresolved references and unsupported configurations. A
policy update revalidates every attached rule, including gRPC restrictions.

## Verification

Native chart tests cover schema absence, namespace ownership, same-name route
kinds, multiple rules, missing dependencies, policy updates and deletion, and
disabled feature dependencies. Live tests verify credential rejection, budget
sharing across replicas, WAF rejection, cache isolation, and recovery. Existing
Ingress behavior and Gateway conformance remain required gates.
