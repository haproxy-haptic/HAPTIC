# CRD & validation design notes

This page captures the durable design decisions behind the `HAProxyTemplateConfig` CRD and the validation stack. For field-by-field reference see [CRD Reference](../crd-reference.md); for user-facing validation workflows see [Validation Tests](../validation-tests.md).

## Why a CRD Instead of a ConfigMap

The controller originally accepted its configuration via `ConfigMap`. That was replaced with a typed, namespaced Kubernetes API (`scope: Namespaced`, defined in `pkg/apis/haproxytemplate/v1alpha1`) because:

- **Schema validation at admission.** OpenAPI rules in the CRD reject malformed YAML before it reaches the controller — a `ConfigMap` accepts any strings.
- **Embedded test fixtures.** `spec.validationTests` lets users ship assertions alongside templates and run them from `haptic validate` and `haptic preflight`, and on every controller start via the load gate, without duplicating fixtures elsewhere.
- **Native tooling.** `kubectl get htplcfg`, `kubectl describe`, status subresource, RBAC on a real Kind — all come for free.
- **Typed client.** The generated clientset (`pkg/generated`) gives both the controller and test harnesses a typed view of the config.

## Defence in depth

Validation of the config itself and validation of watched resources take different paths. Four layers run in sequence:

1. **OpenAPI schema and validation rules** (Kubernetes API server). Rejects structural errors the moment `kubectl apply` hits the apiserver — invalid enum values, missing required fields, bad types. A spec-level `x-kubernetes-validations` rule additionally enforces that a config carries `podSelector`, at least one `watchedResources` entry, and a `haproxyConfig` either inline or from a `spec.libraryRefs` entry. This covers *structure* only; whether the templates compile and render to a config `haproxy -c` accepts is layers 3 and 4.
2. **Validating admission webhook** (the controller itself, served at `/validate`). The chart emits one rule per watched resource marked `enableValidationWebhook: true`. Bundled libraries enable admission for routing resources and Gateway policies. Each rule validates the proposed object over observed inputs, falling back to the accepted input view when necessary. Both paths require complete output validation. Services, EndpointSlices, ConfigMaps, and Secrets remain watched without an admission webhook. Gateway policy credentials and external WAF catalogs require immutable Secrets and ConfigMaps: Kubernetes prevents data changes, and route/policy admission validates each new reference. See [ADR-0026](adr/0026-gateway-route-policies.md).
3. **Authoritative render validation** (leader and proposal paths). The `HAProxyTemplateConfig` has **no** admission webhook — [ADR-0016](adr/0016-one-config-kind-many-instances-pre-rollout-validation.md) removed it, because admission sees one object at a time and a configuration is a set. The leader instead validates the merged set. Watched-resource admission and HTTP-store promotion run `haproxy -c` and every configured rendered-output validator through the proposal pipeline. Reconciliation runs rendered-output validators in its pipeline and `haproxy -c` in the leader-side render gate. Protocol-v1 external checks run on every applicable invocation. The render gate tracks HAProxy verdicts by plan identity. [ADR-0022](adr/0022-haptic-agent.md) defines the current validation paths.
4. **Validation at apply** (HAProxy itself). The pod's own binary parses the configuration at reload and rejects a command it can't run. This is defence in depth, not a replacement for the controller-side gate: no invalid render may publish a success event or reach deployment. A rejection carries HAProxy's own message back, and the agent restores the last known good file set.

A fifth gate sits outside the reconcile path: the **startup load gate** runs the config's embedded `validationTests` on every fresh or upgraded controller pod and fails the iteration if they don't pass. The process retries the iteration and the pod stays NotReady, so a rolling upgrade stalls on the old pods; the kubelet restarts the pod when its startup probe runs out. Each failed attempt stamps the reason onto `status.conditions[Validated]` with reason `LoadGateFailed`. Operators can run the same checks ahead of `helm upgrade` with [`haptic preflight`](../operations/validate-before-deploy.md).

A rejected watched-resource change retains its previous accepted revision while independent valid updates, including endpoint changes, continue. See [Rejected resource changes](../operations/input-isolation.md). A rejected controller configuration leaves the previous configuration active. The watched-resource webhooks ship `failurePolicy: Fail` (`charts/haptic/templates/validatingwebhookconfiguration.yaml`), so creates and updates of *opted-in resources* are rejected when the controller is unreachable; that's deliberate, since rendering an admission decision from an unvalidated overlay is riskier than asking the user to retry.

## Credentials Stay in a Secret

`spec.credentialsSecretRef` points at a `Secret` rather than embedding credentials inline:

- Keeps `HAProxyTemplateConfig` non-sensitive, so it can be stored in Git / Helm values / etc.
- Allows independent rotation: the credentials loader watches the Secret and re-publishes `CredentialsUpdatedEvent` without a full reinitialization cycle.
- Follows the conventional Kubernetes split between "what to do" (typed API) and "secrets needed to do it" (opaque Secret).

Required keys: `dataplane_username`, `dataplane_password`.

## Webhook Architecture

The validating webhook runs inside the controller pod rather than a dedicated deployment. Rationale:

- **Single source of truth.** The webhook uses the same render/validate code path as the reconciler, so there's no way for the admission decision to drift from the runtime decision.
- **No extra moving parts.** One deployment, one Service, one Lease, one cert.
- **Shared store.** The webhook reuses the watched-resource store to build a realistic render context with proposed changes overlaid (see `pkg/stores/overlay.go`).

TLS certificates come from the chart's own self-signed issuance (the default), from cert-manager (`controller.webhook.certManager.enabled=true`, auto-rotating), or supplied manually via `controller.webhook.caBundle`; the chart wires all three options.

### Multi-controller isolation

Each Helm release has its own webhook Service. Kubernetes calls every webhook
whose rules match the proposed resource; different Service names don't isolate
admission. Each controller applies its configured watch selectors when building
the proposal. Keep class ownership distinct as described in
[multiple HAPTIC installations](../deploying-with-helm.md#running-multiple-haptic-instances-in-one-cluster).
Don't narrow webhook coverage to work around a rejected configuration change.

## CRD versioning posture

The API is at `v1alpha1`. The project's posture during alpha:

- Breaking changes are allowed but batched: new sub-fields should be added as optional; renames/removals wait for a minor bump.
- No conversion webhooks yet. Graduating to `v1beta1` adds one, plus a conversion strategy in the release notes.
- The generated clientset, informers, and listers live in `pkg/generated`; regenerate via `make generate` after changing types in `pkg/apis/haproxytemplate/v1alpha1/`.

## Relationship to other docs

| Concern | Canonical doc |
|---------|---------------|
| Every field with types and defaults | [CRD Reference](../crd-reference.md) |
| Writing and debugging validation tests | [Validation Tests](../validation-tests.md) |
| How configuration composes (chart layers, snippets) | [Configuration Model](./design/configuration.md) |
| Webhook TLS, RBAC, chart deployment | [Deploying with Helm](../deploying-with-helm.md), [SSL Certificates](../ssl-certificates.md) |
| Go type definitions | `pkg/apis/haproxytemplate/v1alpha1/types_*.go` |
| Generated code | `pkg/generated/` (`make generate` to refresh) |
