# Configuration model

The controller is headless: no CLI flags carry domain configuration, no config files are mounted. Operators interact with it through the [`HAProxyTemplateConfig` CRD](../../crd-reference.md) (primary configuration), the credentials `Secret` it references ([Security — Credentials](../../operations/security.md#credentials)), and the HTTP surface — `/metrics` ([Monitoring](../../operations/monitoring.md)) plus `/healthz` and `/debug/*` on a shared listener ([Debugging](../../operations/debugging.md)).

Structured logfmt logs on stdout (via `slog.NewTextHandler`) round out the operational surface — the level is set by `LOG_LEVEL` at startup, then dynamically overridden at runtime by the CRD's `spec.logging.level` once the controller's configloader picks it up.

## What the CRD covers

`HAProxyTemplateConfig.spec` is the source of truth for controller behaviour. There is one such object; it pulls in `HAProxyTemplateLibrary` objects through an ordered `spec.libraryRefs` and the controller merges them, later wins, with the config itself last — so the **merged** spec is what everything downstream sees. It has four top-level groups:

- **Runtime settings** — `controller` (including `controller.configPublishing`), `dataplane`, `logging`, `templatingSettings`.
- **Resource watching** — `podSelector`, `watchedResources`, `watchedResourcesIgnoreFields`. (HTTP fetching is driven by the `http.Fetch()` template function — URLs that appear in templates are auto-registered; there is no top-level `spec.httpResources` field, only `validationTests[].httpResources` (a sibling of `fixtures`, not nested inside it) for mocking responses during tests.)
- **Templates** — `haproxyConfig`, `templateSnippets`, `maps`, `files`, `sslCertificates`, `k8sResources` (declarative Kubernetes resources rendered and applied via Server-Side Apply).
- **Validation** — `validationTests`, the per-resource `enableValidationWebhook` flag, and `validators` (pluggable external validator sidecars).

The full field reference (types, defaults, validation rules) lives in [CRD Reference](../../crd-reference.md), which also opens with a runnable minimal example; the installation walkthrough is [Getting Started](../../getting-started.md). This page shows how the pieces compose.

## Configuration layers

Users commonly compose configuration from three layers, in order of precedence:

1. **Template libraries** shipped in the Helm chart (base, SSL, ingress, gateway, haproxytech, …). The chart renders each enabled one as its own `HAProxyTemplateLibrary`, named `<configName>-<library>`. A library carries content only — `templateSnippets`, `validationTests`, `maps`, `files`, `sslCertificates`, `k8sResources`, `templatingSettings`, `haproxyConfig` — never `podSelector`, `watchedResources` or `dataplane`.
2. **`controller.config`** in Helm values — rendered as the single `HAProxyTemplateConfig` named `<configName>` and merged last, so anything set here wins over every library.
3. **Direct `HAProxyTemplateConfig` edits** (via `kubectl edit htplcfg <configName>`) for ad-hoc overrides. That object stays small — about 1% of etcd's per-object limit — because the bulk lives in the libraries. Editing a library in place works too and takes effect immediately; `helm upgrade` overwrites it.

Merge order is declared once, in `spec.libraryRefs`, and nowhere else. Each entry
also names a `revision` that the referenced object must report: the controller
compares the two strings and never derives either from the content, so a
half-applied set shows up as a mismatch and the controller keeps serving the
last-good configuration rather than rendering a set with a library missing. An
in-place edit leaves the revision untouched, which is why it takes effect
immediately.

The merge uses the same primitive Helm's `mustMergeOverwrite` does, so the result
is what a chart-side merge would have produced. `validationTests` from every source are combined — a test name
defined by two sources is an error naming both. See
[ADR-0017](https://github.com/haproxy-haptic/HAPTIC/blob/main/docs/adr/0017-template-library-kind.md).

One object per library exists because the single merged object had reached 99.4%
of the ~1.5 MiB limit Kubernetes enforces per object. `make cr-size-check` gates
each object on every chart-test run.

Because templates are just strings inside a CRD, the chart layers and the user's own values can both contribute snippets and be composed at render time. See [Templating Guide](../../templating.md) for how snippets and extension points interact.

## Runtime API resolution

The merged config declares which resource versions the templates support. The
controller resolves each ordered `apiVersions` list against live discovery and
selects the first served candidate. It doesn't substitute the cluster's preferred
version: a version absent from the list may have a shape the templates can't use.

This produces an effective config before template compilation. Unavailable optional
watches and entries whose `requires` dependencies are unavailable are removed;
missing required resources fail initialization. A discovery error can't establish
that an optional resource is absent. See [Watching resources](../../watching-resources.md)
for the configuration fields and template access rules.

## Reloading behaviour

Changes to the `HAProxyTemplateConfig` or its referenced libraries are merged and
validated before replacement. A rejected proposal leaves the current iteration
running. Accepted changes trigger an internal reinitialization; the debounce
combines bursts such as a Helm upgrade. The Secret referenced by
`credentialsSecretRef` is also watched, so credential rotation requires no pod
restart. Only a changed `dataplane_username` or `dataplane_password` restarts the
iteration; an update that only changes labels or annotations, such as a Helm
upgrade, doesn't.

The serving iteration remains active while its successor starts from the accepted
config, credentials, and discovery snapshot. The successor waits for its render
graph to warm, with a bounded timeout, then takes over leadership on the same
replica. Only after successful startup is the predecessor torn down. A failure
before leadership handover leaves the predecessor serving; retries fetch live
state instead of reusing the consumed snapshot. A reload during unfinished startup
cancels that startup and carries the newer snapshot into the next attempt.

Process-owned health and webhook listeners remain available across iterations;
admission fails closed whenever no ready validator is installed. HAProxy continues
serving its last applied configuration throughout the transition.

Installing, upgrading, or removing a relevant CRD can change API resolution. The
controller rechecks discovery and restarts the iteration when the effective config
changes. Rebuilding watchers, schemas, templates, and admission together prevents
them from using different resource versions. Re-resolution runs all config
validators before activation, including when no embedded validation tests exist.
The controller doesn't swap individual informers into a running iteration.
