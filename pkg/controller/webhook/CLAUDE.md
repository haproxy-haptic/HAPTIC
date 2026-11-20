# pkg/controller/webhook

This package connects the pure HTTPS server in `pkg/webhook` to the synchronous
`DryRunValidator` and the controller's webhook metrics.

## Responsibilities

- Resolve configured GroupVersionResources to GroupVersionKinds.
- Build one complete validator table for the watched-resource rules.
- Run structural checks and `DryRunValidator.ValidateAdmission` synchronously.
- Apply the controller-side admission deadline and record decisions.

Certificate provisioning and `ValidatingWebhookConfiguration` resources belong
to the Helm chart. Render and HAProxy validation belong to
`pkg/controller/dryrunvalidator` and `pkg/controller/proposalvalidator`.

## Lifecycle

Production passes a process-owned `*pkg/webhook.Server` in `Config.Server`. The
component installs its complete table with `ReplaceValidatorGeneration`; it
never starts or stops the shared listener. On iteration teardown it installs an
empty fail-closed generation. Replacement waits for requests using the retired
generation, then calls `OnGenerationRetired` to release captured iteration
dependencies.

Process termination drains the listener before canceling the iteration, keeping
watchers and validators alive until active admission responses finish.

If `Config.Server` is nil, the component owns the server. This mode is useful for
tests and standalone composition. `Start` binds the listener, serves until
cancellation or failure, shuts it down, and joins the serve loop.

The persistent listener reads the mounted `tls.crt` and `tls.key` through
`CertDir` and reloads changed certificate content during TLS handshakes.

## Admission contract

The chart creates rules only for watched resources with
`enableValidationWebhook: true`. Every rule uses `failurePolicy: Fail` and a
10-second API-server timeout. There is no `HAProxyTemplateConfig` admission
webhook; set-level config validation follows ADR-0016.

Registration is atomic. A rule without a dry-run validator fails component
startup. A request without a registered validator is denied with status 503,
and a validation canceled by iteration teardown is denied as unavailable.

Each registered validator:

1. Checks basic object structure.
2. Derives a 9-second deadline from the iteration context.
3. Calls `DryRunValidator.ValidateAdmission` with the proposed object.
4. Returns the decision, reason, warnings, and any service failure. A missing external verdict yields a denied AdmissionReview with status 503.

Keep validation synchronous. The API server waits for the AdmissionReview
response, so detached work would return before the gate has made a decision.

## Tests

- `component_test.go` covers rule resolution, validation decisions, deadlines,
  and metrics.
- `component_adopted_test.go` covers atomic generation replacement, fail-closed
  teardown, and request draining on the shared listener.
- `pkg/webhook/server_test.go` covers TLS and AdmissionReview handling.

See `pkg/controller/webhook/README.md`, `pkg/webhook/CLAUDE.md`, and
`docs/adr/0016-one-config-kind-many-instances-pre-rollout-validation.md`.
