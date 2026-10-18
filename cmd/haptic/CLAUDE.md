# cmd/haptic - Main Entry Point

Development context for the controller application entry point.

**Architecture**: See `/docs/site/docs/development/design/sequence-diagrams.md` (startup and initialization)

## When to Work Here

Modify this package when:

- Changing startup sequence
- Adding new command-line flags
- Modifying environment variable handling
- Changing signal handling
- Adding health/metrics endpoints
- Modifying graceful shutdown logic

**DO NOT** modify this package for:

- Business logic → Use appropriate `pkg/` package
- Event coordination → Use `pkg/controller`
- Configuration parsing → Use `pkg/core/config`

## Package Structure

```
cmd/haptic/
├── main.go                    # Cobra root command + init wiring (registers run, validate, benchmark)
├── run.go                     # `run` subcommand (controller daemon)
├── validate.go                # `validate` subcommand (CLI for embedded tests)
├── preflight.go               # `preflight` subcommand (render the chart with operator values, then validate)
├── schemasource.go            # schema access: directory, live cluster, or none (shared by validate/preflight)
├── benchmark.go               # `benchmark` subcommand entry point
├── benchmark_render.go        # benchmark: render-only path
├── benchmark_output.go        # benchmark output formatting
├── clustersource.go           # live-cluster clients (dynamic, discovery, schema fetcher)
├── applycrds.go               # `apply-crds` subcommand (server-side apply of bundled CRDs)
├── chartrender.go             # in-process Helm chart render (embedded-chart config source)
├── config.go                  # `config view` subcommand + CRD loading helpers shared by run/validate/benchmark
├── shared.go                  # Shared flag definitions and bootstrap helpers
├── version.go                 # `version` subcommand (registers itself in init())
└── CLAUDE.md                  # This file
```

`config.go` and `version.go` register their cobra subcommands via their own `init()` functions, so they don't appear in `main.go`'s `init()` block — grep for `rootCmd.AddCommand` to find every wire-up site.

## Commands

### `run` — Controller Daemon (run.go)

The primary controller daemon that watches Kubernetes resources and manages HAProxy configuration. Wired through Cobra in main.go; the actual iteration loop lives in `pkg/controller/iteration.go`.

### `validate` — CLI (validate.go)

CLI tool for validating HAProxyTemplateConfig CRDs with embedded validation tests. Used both by humans (`haptic validate -f config.yaml`) and CI/CD pipelines.

`-f` is repeatable and each file may hold several YAML documents; every
HAProxyTemplateConfig across them is merged in order through the same
`conversion.MergeSpecs` the daemon uses. That means `helm template … | yq
'select(.kind == "HAProxyTemplateConfig")' > all.yaml` then `validate -f
all.yaml` validates exactly what the controller would assemble — which is how
`scripts/test-templates.sh` stays honest now that the chart emits one object per
library. `--dump-merged` prints the merged spec and exits without running a test.

A lone file holding a single document still accepts a bare spec (no
apiVersion/kind), the shape hand-written fixtures use.

### `preflight` — Pre-deploy Gate (preflight.go)

Renders the bundled chart with the operator's **own** values file(s) and runs
the load gate over the result, so a bad configuration fails the pipeline instead
of leaving new controller pods NotReady. Chart CI only ever proves the *defaults* work.

Renders in-process through `renderChart`, then
reuses `validateAndReport` so `preflight` and `validate` cannot drift into
checking different things.

Schemas default to the **live cluster** (`--kubeconfig`, `$KUBECONFIG`, then
in-cluster), unlike `validate`, which is dir-or-nothing. There is no
no-schemas mode here: without schemas the render silently falls back to
untyped access and would pass on a weaker check than the controller runs.
`--schema-dir` switches it fully offline.

`schemasource.go` holds the shared abstraction — `schemaSource` is a directory
fetcher, a `liveCluster`, or the zero value (no schemas, the `validate`
default). Its two methods are the only places the offline/live split is
decided; `validate` and `preflight` both go through them.

It also compiles what the render produces for the *other* processes in the
fleet, which the load gate cannot judge: `vector validate` on `RenderedFiles`
`vector.yaml`, and `varnishd -C` on every `*.vcl` in a rendered ConfigMap
(`RenderedK8sResources`). Each distinct config and image pair is checked; a
shared file name never discards another test's config. Images come from the
rendered Helm workload for Vector and the workload mounting the VCL ConfigMap
for Varnish. Missing or ambiguous images fail, as do conflicting
`HAPTIC_VECTOR_IMAGE` or `HAPTIC_VARNISH_IMAGE` overrides.
Both run the real binaries in containers (`HAPTIC_CONTAINER_RUNTIME`);
without a runtime they warn and skip, but an explicitly configured runtime that
is missing is an error. `varnishd` resolves backend hostnames at compile time,
so each `.host` in the VCL is pointed at loopback via `--add-host`.

Negative-controlled against every failure class that motivated it, each
discriminating (the same input passes when the defect is absent): a bogus
`global` directive, a vector section indented past its siblings, a VCL symbol
that regex assertions accept but `varnishd` rejects, and — the incident that
prompted the command — a validationTest pinning `service.namespace` to
`default`, which passes under `--namespace default` (what chart CI renders in)
and fails under the namespace the operator actually deploys to. User docs:
`docs/site/docs/operations/validate-before-deploy.md`.

### `benchmark` — Render Performance (benchmark*.go)

Renders the templates in a HAProxyTemplateConfig repeatedly against fixture data and reports timings. Useful for spotting template regressions before they hit reconciliation.

### `apply-crds` — Server-Side Apply Bundled CRDs (applycrds.go)

Reads the CRDs from the image-embedded chart (`resolveChartDir` → `<chart>/crds`,
shared with `chartrender.go`) and server-side applies each to the
cluster with field manager `haptic-crd-installer`. Closes the "Helm never upgrades
`crds/`" gap for additive schema changes. Flags: `--chart`, `--kubeconfig`.

It strips `.status` and `metadata.creationTimestamp` before applying — the API
server owns CRD `status` (`storedVersions`), so applying an empty status would
clobber the stored-version bookkeeping. The hardcoded `customresourcedefinitions`
GVR is the operational-identity exception (HAPTIC's own API surface), not a RULE #1
violation. The chart wires this as a `pre-install`/`pre-upgrade` hook Job
(`templates/crd-upgrade-hook.yaml`, gated by `crds.upgradeJob.enabled`).

### `config` — Inspect Live HAProxy Config (config.go)

`haptic config view` fetches the published `HAProxyCfg` CRD from the cluster (the rendered HAProxy configuration the controller deployed last), decompresses it if needed, and prints the raw config to stdout. It is a **live-cluster** command — it talks to the API server, not a local file. Flags: `--crd-name`, `--namespace`, `--kubeconfig`, `--input` (no `-f`).

`--input` prints the merged **input** config instead: it fetches the named
`HAProxyTemplateConfig` plus every `HAProxyTemplateLibrary` its ordered
`spec.libraryRefs` names, and merges them the way the controller does. Since the
chart splits the content across one library object per template library, no
single object shows the whole picture, and this is how an operator gets it back.
The name defaults via `--crd-name` → `CRD_NAME` env → `haproxy-config`. Useful for
`haptic config view | bat -l haproxy` style inspection on a running
deployment.

### `version` — Build Info (version.go)

Prints the build's `version`, `commit`, and `buildDate` (set via ldflags at build time).

**Usage:**

```bash
haptic validate -f config.yaml [flags]
```

**Observability Flags:**

```bash
# Show rendered content preview for failed assertions (first 200 chars)
haptic validate -f config.yaml --verbose

# Dump complete rendered content (haproxy.cfg, maps, files, certs)
haptic validate -f config.yaml --dump-rendered

# Show template execution trace with timing
haptic validate -f config.yaml --trace-templates

# Combine flags for comprehensive debugging
haptic validate -f config.yaml --verbose --dump-rendered --trace-templates
```

**Flag Details:**

- `--verbose` - Shows content preview for failed assertions
    - Displays target name and size
    - Shows first 200 characters of content
    - Includes hints for further debugging
    - Default: false

- `--dump-rendered` - Dumps all rendered content
    - HAProxy configuration (haproxy.cfg)
    - Map files with full content
    - General files with full content
    - SSL certificates with full content
    - Shown after test results
    - Default: false

- `--trace-templates` - Shows template execution trace
    - Template names and render order
    - Timing information in milliseconds
    - Useful for identifying slow templates
    - Default: false
    - Note: Shows top-level renders only. Use with `--profile-includes` for full call tree

**Enhanced Error Messages:**

All validation errors include helpful context by default (no flags needed):

```
Error: pattern "backend api-.*" not found in haproxy.cfg (target size: 1234 bytes).
       Hint: Use --verbose to see content preview
```

**Implementation:**

The validate command uses `pkg/controller/testrunner` to execute tests and format results. It creates a temporary directory for HAProxy validation and cleans up afterward.

**Example Debugging Workflow:**

```bash
# 1. Run tests and see enhanced error messages
haptic validate -f config.yaml
# Output: "pattern X not found in map:foo.map (target size: 61 bytes). Hint: Use --verbose"

# 2. Enable verbose mode to see content preview
haptic validate -f config.yaml --verbose
# Output: Shows first 200 chars of map:foo.map

# 3. See full content if needed
haptic validate -f config.yaml --dump-rendered
# Output: Complete content of all rendered files

# 4. Identify slow templates
haptic validate -f config.yaml --trace-templates
# Output: Template execution trace with timing
```

## Key Responsibilities

1. **Initialize logging**: Set up structured logging
2. **Parse flags/env vars**: Load configuration from environment
3. **Create Kubernetes client**: Connect to cluster
4. **Create EventBus**: Initialize event infrastructure
5. **Start components**: Boot components in correct order (eight stages)
6. **Handle signals**: Graceful shutdown on SIGTERM/SIGINT
7. **Expose endpoints**: Health checks, metrics, profiling

**Not responsible for:**

- Configuration validation (done in pkg/controller/validator)
- Resource watching (done in pkg/k8s)
- Template rendering (done in pkg/templating)
- Event coordination (done in pkg/controller)

## Eight-Stage Startup

The controller uses event-driven staged startup:

```
Stage 1: Config Management Components
  - ConfigLoader (parses HAProxyTemplateConfig CRD)
  - CredentialsLoader (parses credentials Secret)
  - BasicValidator + TemplateValidator + JSONPathValidator (scatter-gather over `ConfigValidationRequest`) + ConfigChangeHandler (orchestrator)
  - Commentator (subscribes to all events for domain-aware logging)

Stage 2: Wait for Valid Config
  - Fetch HAProxyTemplateConfig CRD and credentials Secret synchronously
  - Block until ConfigValidatedEvent received

Stage 3: Resource Watchers
  - Create a watcher.Watcher for each spec.watchedResources entry
  - IndexSynchronizationTracker waits for every store's initial sync

Stage 4: Config Watchers
  - Start the CRD and credentials SingleWatchers (immediate-callback mode)

Stage 5: Reconciliation & Observability Components
  (EventBus.Start() runs immediately after this stage, once all components have subscribed)
  - Reconciler (debounces resource-index updates)
  - Coordinator (drives the render → validate → publish pipeline; leader-only)
  - DeploymentScheduler, Deployer, DriftMonitor, ConfigPublisher, StatusApplier (leader-only)
  - Discovery, HTTPStore, ProposalValidator, Metrics
  - Initial ReconciliationTriggeredEvent is published

Stage 6: Leader Election
  - Starts the lease-backed elector; on BecameLeaderEvent the leader-only
    components (Coordinator, Deployer, DeploymentScheduler, DriftMonitor,
    ConfigPublisher, StatusApplier) start their goroutines and subscribe.

Stage 7: Webhook Validation (only if `enableValidationWebhook: true`
on at least one watched resource)
  - Starts the HTTPS admission server; the DryRunValidator was created in
    Stage 5 so its proposal-validation subscriptions were in place before
    EventBus.Start().

Stage 8: Debug & Health Wiring
  - Registers debug variables with the introspection server, starts the
    pre-created EventBuffer goroutine, and swaps the bootstrap health
    checker for one backed by the full lifecycle registry.

Controller operational: subsequent CRD or Secret changes cancel the iteration
context, components shut down, and the loop restarts with the new config (no pod
restart required).
```

**Why staged?**

- Prevents reconciliation before config is valid
- Ensures all resources loaded before first reconciliation
- Clear startup progression for debugging
- Testable stages

## Flags and Environment Variables

Authoritative source: `cmd/haptic/run.go` (`init()` registers flags) and `cmd/haptic/main.go` (package doc). Each flag falls back to its env var, then to the listed default.

| Flag | Env var | Default | Purpose |
|------|---------|---------|---------|
| `--crd-name` | `CRD_NAME` | `haproxy-config` | Name of the **single** `HAProxyTemplateConfig` the controller reads. Not repeatable and not a list. Template library content is pulled in through that object's ordered `spec.libraryRefs`, which the controller resolves into `HAProxyTemplateLibrary` objects and merges later-wins with the config last (ADR-0017). |
| `--secret-name` | `SECRET_NAME` | `haproxy-credentials` | Name of the `Secret` with `dataplane_username` / `dataplane_password`. |
| `--webhook-cert-dir` | `WEBHOOK_CERT_DIR` | `""` (disabled) | Directory holding the validating-admission-webhook server's TLS cert (`tls.crt`/`tls.key`); the chart mounts the cert Secret here and sets this to `/etc/webhook/certs`. The server reads and hot-reloads the files on rotation. Empty disables the webhook entirely. |
| `--webhook-resource-admission-timeout` | `WEBHOOK_RESOURCE_ADMISSION_TIMEOUT` | `9s` | Controller-side deadline for watched-resource dry-run admission. Keep it below the matching `ValidatingWebhookConfiguration.timeoutSeconds`; the chart derives it automatically. |
| `--debug-port` | `DEBUG_PORT` | `0` (disabled) | Port for the introspection HTTP server (`/healthz` + `/debug/vars` + `/debug/pprof`). The Helm chart sets this to `8080` by default. |
| `--kube-client-qps` | `KUBE_CLIENT_QPS` | `-1` (APF) | Client-side apiserver QPS. `<= 0` disables client-side throttling so the controller relies on apiserver Priority & Fairness; a positive value installs one shared client-side rate limiter across all the controller's clients. Chart value `controller.kubeClient.qps`. |
| `--kube-client-burst` | `KUBE_CLIENT_BURST` | `0` | Client-side apiserver burst; used only when `--kube-client-qps > 0` (`0` means `2*qps`). Chart value `controller.kubeClient.burst`. |
| `--kubeconfig` | — | (in-cluster) | Out-of-cluster development. |
| — | `MUTEX_PROFILE_FRACTION` | `0` (off) | Samples 1 in N mutex contentions into `/debug/pprof/mutex`. |
| — | `BLOCK_PROFILE_RATE` | `0` (off) | Samples blocking events into `/debug/pprof/block`, about one per this many nanoseconds blocked. |
| — | `LOG_LEVEL` | `INFO` | Initial log level: `TRACE`, `DEBUG`, `INFO`, `WARN`, `ERROR` (case-insensitive; `WARNING` accepted as alias for `WARN`). The CRD's `spec.logging.level`, when non-empty, takes over at runtime via the dynamic logger. |

The chart injects `POD_NAME` and `POD_NAMESPACE` via the downward API (`fieldRef` to `metadata.name` / `metadata.namespace`). `POD_NAMESPACE` is the controller's own namespace — used for owned-resource `OwnerReference`s and the leader-election lease — and falls back to the service-account token mount (`/var/run/secrets/kubernetes.io/serviceaccount/namespace`) when unset; `POD_NAME` is the leader-election lease identity and falls back to the OS hostname. There is no `CONTROLLER_NAMESPACE` env var. Other surfaces:

- **Log output** — always structured slog (logfmt-ish text on stdout); no `LOG_FORMAT` env var.
- **Metrics port** — read from the `METRICS_PORT` env var (default `9090`; set to `0` to disable). The Helm chart owns that env var through `controller.ports.metrics` and rejects a duplicate `extraEnv` override so the process, pod, Service, and monitors cannot drift.
- **Healthz port** — runs on the same listener as `--debug-port` (default `0` = disabled when running the binary directly; the chart sets `DEBUG_PORT`, the container port, Service, probes, and NetworkPolicy from `controller.ports.healthz`, default `8080`). There is no separate healthz listener, and the chart requires this port because its probes depend on `/healthz`.
- **Webhook port** — read from `WEBHOOK_PORT` (default `9443`). The Helm chart owns it through `controller.ports.webhook`, alongside the container port, Service target, and NetworkPolicy. Disabled entirely when `--webhook-cert-dir` is empty.
- **pprof** — always mounted at `/debug/pprof/*` whenever the introspection server is enabled; no `ENABLE_PPROF` env var.

## Memory limit (GOMEMLIMIT via automemlimit)

`configureMemoryLimit` first disables Linux transparent huge pages for the
process, keeping physical memory overhead within the container headroom.
Other operating systems are unchanged. See
[ADR-0027](../../docs/adr/0027-process-memory-page-policy.md) for the measured
failure and the throughput trade-off.

`run.go` sets Go's `GOMEMLIMIT` from the container's cgroup memory limit at
startup — `memlimit.SetGoMemLimitWithOpts` (`github.com/KimMachineGun/automemlimit`),
mirroring automemlimit's defaults (FromCgroup provider, **0.9 ratio** → ~90% of
the limit). It is called explicitly rather than via the blank-import init so its
"GOMEMLIMIT is updated" line goes through the slog handler.

**It is set programmatically, NOT via a `GOMEMLIMIT` env var.** A
`kubectl get deploy … -o jsonpath='…env'` grep will not show it and must not be
read as "GOMEMLIMIT is unset" — the value comes from the cgroup at runtime. To
see the effective limit, read the `gomemlimit` field on the startup
"…starting" log line, or `debug.SetMemoryLimit(-1)` in a debugger. `GOGC` is
left at Go's default (the Green Tea GC makes manual GOGC tuning
unnecessary); automemlimit's `GOMEMLIMIT` is the OOM safety net.

`GOMEMLIMIT` is a **soft** limit: it makes the GC run harder as the heap
approaches it, but under a severe transient-allocation burst on a CPU-saturated
pod the GC can fall behind and the heap can still breach the **hard** cgroup
limit → OOMKill. So it reduces, but does not eliminate, OOM risk under extreme
concurrent-render bursts; bounding render concurrency and per-render allocation
is the structural fix (see issue #178).

## Signal Handling

`cmd/haptic/main.go` is a thin Cobra wrapper — it just calls `rootCmd.Execute()`.
The signal-to-context bridge lives in `cmd/haptic/run.go` (`runRun`); the
errgroup / per-component goroutines are inside `pkg/controller/iteration.go`,
not here. The shape in `runRun` is intentionally minimal:

```go
// cmd/haptic/run.go (runRun, paraphrased)
ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
defer cancel()

if err := controller.Run(ctx, k8sClient,
    runCRDName, runSecretName, runWebhookCertDir, runDebugPort,
); err != nil {
    // Only surface the error if it's not just the signal-driven cancellation
    if ctx.Err() == nil {
        return fmt.Errorf("controller failed: %w", err)
    }
}
```

`controller.Run` owns the per-iteration lifecycle: each iteration constructs an
`errgroup`, fans out `Run`/`Start` calls for every component, and exits when
either the parent context cancels (signal) or the iteration context cancels
(config change, leader change, etc.). The shutdown deadline lives there too —
don't add another shutdown-timeout layer in `cmd/haptic`.

## Health and Metrics

### Health Endpoint

The controller doesn't hand-roll its HTTP servers — three reusable infra packages own the surfaces:

- **`pkg/introspection`** — `/healthz` (also aliased as `/health`), `/debug/vars`, `/debug/vars/<name>?field={…}`, `/debug/events`, `/debug/pprof/*`. Backed by an instance-based registry of `Var` implementations. Listening port comes from `--debug-port` / `DEBUG_PORT` (default 0 = disabled; the Helm chart sets 8080). Setting it to 0 disables both `/debug/*` and `/healthz` (no separate healthz listener exists), so probes break — restrict access via NetworkPolicy instead. The chart's `controller.ports.healthz` only configures the Service port and container-port declaration used by probes; it doesn't open an extra listener. `/readyz` and `/livez` judge the serving iteration and back the chart's readiness and liveness probes; `/healthz` stays the startup probe (ADR-0028).
- **`pkg/metrics`** — `/metrics` via Prometheus `promhttp` against an instance-based `prometheus.Registerer`. Port comes from the `METRICS_PORT` env var (default 9090, set to 0 to disable; chart owner `controller.ports.metrics`). The instance-scoped registry is critical: every reinitialization iteration creates a fresh registry so metrics get GC'd cleanly when the iteration ends.
- **`pkg/webhook`** — admission webhook HTTPS (`/validate`) and a sidecar `/healthz`. Disabled when `--webhook-cert-dir` is empty. Port comes from `WEBHOOK_PORT` (default `9443`; chart owner `controller.ports.webhook`).

Don't add new HTTP surfaces in `cmd/haptic`. Add a `Var` to `pkg/introspection`, a metric to `pkg/controller/metrics`, or a handler on the existing webhook server.

## Testing Approach

### Integration Tests

Test full startup sequence:

```go
func TestController_Startup(t *testing.T) {
    // Create fake clients: typed for Secrets, dynamic for the haproxy-haptic.org CRD.
    // The controller reads its config from an HAProxyTemplateConfig CRD via the
    // dynamic client; CRDs aren't part of the typed clientset.
    fakeKube := fake.NewSimpleClientset()
    scheme := runtime.NewScheme()
    require.NoError(t, haproxyv1alpha1.AddToScheme(scheme))
    fakeDynamic := dynamicfake.NewSimpleDynamicClient(scheme, &haproxyv1alpha1.HAProxyTemplateConfig{
        ObjectMeta: metav1.ObjectMeta{
            Name:      "haproxy-config",
            Namespace: "default",
        },
        Spec: haproxyv1alpha1.HAProxyTemplateConfigSpec{
            // … fill in podSelector, watchedResources, haproxyConfig.template …
        },
    })

    // Credentials Secret — only dataplane_username and dataplane_password;
    // there are no validation_* keys.
    secret := &corev1.Secret{
        ObjectMeta: metav1.ObjectMeta{
            Name:      "haproxy-credentials",
            Namespace: "default",
        },
        Data: map[string][]byte{
            "dataplane_username": []byte("admin"),
            "dataplane_password": []byte("pass"),
        },
    }
    fakeKube.CoreV1().Secrets("default").Create(ctx, secret, metav1.CreateOptions{})

    // Start controller. There is no struct + NewController() — the entry point
    // is a package-level function `controller.Run(ctx, k8sClient, crdName,
    // secretName, webhookCertDir, debugPort)`. Build a `*client.Client`
    // around the fake clients and pass it in.
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()

    k8sClient := &client.Client{
        Clientset: fakeKube,
        Dynamic:   fakeDynamic,
    }

    done := make(chan error)
    go func() {
        done <- controller.Run(ctx, k8sClient, "haproxy-config", "haproxy-credentials", "", 0)
    }()

    // There is no readiness event and no public `IsReady()` method, and Run
    // owns its EventBus internally (a test can't subscribe to it), so there is
    // nothing to wait on here — cancel and assert a clean shutdown. Behavioural
    // assertions on what a full iteration does belong in the integration suite
    // (tests/integration), which drives a real apiserver.
    cancel()
    require.NoError(t, <-done)
}
```

### End-to-End Tests

Test complete workflow with kind cluster:

```bash
# Run e2e test with real cluster
KEEP_CLUSTER=true go test ./cmd/haptic/... -tags=e2e -v
```

## Common Pitfalls

### Starting Components Out of Order

**Problem**: Components started before dependencies ready.

```go
// Bad — race condition: resource watchers spin up before the CRD has loaded
resourceWatcher := resourcewatcher.New(eventBus, ...)
go resourceWatcher.Run(ctx)

// CRD might not be loaded yet — the configloader hasn't published
// ConfigValidatedEvent.
configLoader := configloader.NewConfigLoaderComponent(eventBus, logger)
go configLoader.Run(ctx)
```

**Solution**: Follow staged startup pattern.

```go
// Good — stages ensure dependencies (mirrors pkg/controller/iteration.go)

// Stage 1–2: configloader + credentialsloader run, the iteration blocks
// on the synchronous fetch+validate of the CRD and Secret before any
// resource watchers exist.
configLoader := configloader.NewConfigLoaderComponent(eventBus, logger)
go configLoader.Run(ctx)
config := <-validatedConfigCh   // ConfigValidatedEvent

// Stage 3: only after the config is in hand, build resource watchers
// from spec.watchedResources and wait for their initial sync.
resourceWatcher := resourcewatcher.New(eventBus, config, ...)
go resourceWatcher.Run(ctx)
```

### Not Handling Shutdown Timeout

**Problem**: Components don't stop within deadline.

```go
// Bad - no timeout
<-ctx.Done()
g.Wait()  // Might hang forever
```

**Solution**: Enforce shutdown timeout.

```go
// Good - timeout enforced
<-ctx.Done()

shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

done := make(chan error)
go func() {
    done <- g.Wait()
}()

select {
case <-done:
    // Clean shutdown
case <-shutdownCtx.Done():
    log.Error("Shutdown timeout - forcing exit")
    os.Exit(1)
}
```

### Logging Before Logger Initialized

**Problem**: Using `slog` before the controller's logger is installed as the
default — early lines render with the stdlib default handler (text on stderr,
INFO threshold) instead of the configured one.

```go
// Bad — relies on whatever the package-level default happens to be
slog.Info("starting")
logger := logging.NewLogger(os.Getenv("LOG_LEVEL"))
slog.SetDefault(logger)
```

**Solution**: Build and install the logger before the first `slog` call. The
real API is `logging.NewLogger(level string)` for a fixed level or
`logging.NewDynamicLogger(level)` for one whose level can be bumped at runtime
via `logging.SetLevel(...)`. There's no `logging.New(...)`, no
`logging.Config`, and no JSON output (everything is logfmt to stdout).

```go
// Good
logger := logging.NewDynamicLogger(os.Getenv("LOG_LEVEL")) // case-insensitive
slog.SetDefault(logger)
slog.Info("controller starting") // uses configured logger
// ... later, e.g. when CRD spec.logging.level changes:
logging.SetLevel("DEBUG")
```

### Ignoring Component Errors

**Problem**: Component error doesn't stop controller.

```go
// Bad - errors ignored
go component1.Run(ctx)  // Error lost
go component2.Run(ctx)  // Error lost
```

**Solution**: Use errgroup to propagate errors.

```go
// Good - errors propagate
g, gCtx := errgroup.WithContext(ctx)

g.Go(func() error { return component1.Run(gCtx) })
g.Go(func() error { return component2.Run(gCtx) })

if err := g.Wait(); err != nil {
    log.Error("component error", "error", err)
    os.Exit(1)
}
```

## Adding New Startup Stage

If you need to add a new stage:

1. Determine stage position (before/after existing stages)
2. Create component
3. Add to startup sequence
4. Add wait condition (if needed)
5. Update tests
6. Document new stage

### Example: Wiring an Extra Component into Stage 5

There's no `Controller` struct or `(c *Controller).Run` method — `runIteration`
in `pkg/controller/iteration.go` is the canonical wiring point. Add new
components there, alongside the existing Stage-5 constructors. The pattern
mirrors the metrics component already wired into the iteration:

```go
// Inside runIteration (pkg/controller/iteration.go, Stage 5).
// Construct first so the subscription happens before bus.Start() releases
// the pre-start buffer; only then spin up the goroutine.
domainMetrics := pkgmetrics.NewMetrics(infra.MetricsRegistry)
metricsComponent := metricsadapter.New(domainMetrics, bus)

bus.Start() // pre-start buffer flushes to every existing subscriber

go func() {
    if err := metricsComponent.Start(iterCtx); err != nil {
        logger.Error("metrics component failed", "error", err)
    }
}()
```

There is no `WaitForReady()` on the metrics component — readiness is signaled
by the `*component.ReadySignal` embedded in components that need it (renderer,
validator, …); cross-iteration ordering instead relies on the
"subscribe-before-Start" contract. If you need the new component to publish a
state event on `BecameLeaderEvent`, see the leadership-transition pattern in
`pkg/controller/CLAUDE.md`.

## Debugging Startup Issues

### Enable Debug Logging

```bash
# Local (running the binary directly)
export LOG_LEVEL=DEBUG  # also: TRACE, INFO (default), WARN, ERROR

# In a Kubernetes deployment installed via the chart
kubectl set env -n haptic deployment/haptic-controller LOG_LEVEL=DEBUG
```

For runtime changes without a pod restart, set `spec.logging.level` on the `HAProxyTemplateConfig` CRD instead — the configloader picks it up live and the dynamic logger switches without re-init.

### Check Stage Progress

```bash
kubectl logs -f -n haptic deployment/haptic-controller | grep -i "stage\|operational"

# Expected progression:
# Stage 5: Creating reconciliation components
# Stage 6: Initializing leader election
# Stage 7: Setting up webhook validation  (only if webhook enabled)
# Stage 8: Registering debug variables and updating health checker
# Controller iteration initialized successfully - entering event loop
```

### Identify Stuck Stage

```bash
# If startup hangs, look at the last log line
kubectl logs -n haptic deployment/haptic-controller | tail -1

# Stage 2 stuck → check the HAProxyTemplateConfig CRD exists and validates
kubectl get htplcfg -n haptic
kubectl get htplcfg -n haptic haproxy-config -o yaml | yq '.status'

# Startup stuck before 'Stage 5' log → at least one watcher's initial sync isn't completing
kubectl logs -n haptic deployment/haptic-controller | grep -i "sync\|watcher"
```

### Enable Profiling

`pprof` is part of the introspection HTTP server, not a separate binary. The Helm chart already enables it on port 8080 by default (same port as `/healthz`); no env-var toggle is needed.

```bash
# Port-forward the introspection port
kubectl port-forward -n haptic deployment/haptic-controller 8080:8080

# Profile CPU
go tool pprof http://localhost:8080/debug/pprof/profile?seconds=30

# Profile memory
go tool pprof http://localhost:8080/debug/pprof/heap
```

`/debug/pprof/*` and `/healthz` share the same listener. In the Helm chart,
`controller.ports.healthz` moves the process listener, pod, Service, probes, and
NetworkPolicy together. To shield profiling endpoints in production, restrict
access via NetworkPolicy rather than disabling the required health listener.

## Kubernetes Deployment

### RBAC Requirements

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: haptic-controller
rules:
  # Primary CRD (input)
  - apiGroups: ["haproxy-haptic.org"]
    resources: ["haproxytemplateconfigs"]
    verbs: ["get", "watch", "list"]

  # Output CRDs (rendered config + auxiliary files)
  - apiGroups: ["haproxy-haptic.org"]
    resources:
      - "haproxycfgs"
      - "haproxygeneralfiles"
      - "haproxycrtlistfiles"
      - "haproxymapfiles"
    verbs: ["get", "watch", "list", "create", "update", "patch", "delete"]

  # Credentials Secret + watched-resource Secrets (TLS)
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get", "watch", "list"]

  # Leader election
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    verbs: ["get", "create", "update"]

  # Watched resources (Ingress, Service, EndpointSlice, etc.)
  - apiGroups: ["networking.k8s.io"]
    resources: ["ingresses"]
    verbs: ["get", "watch", "list"]

  - apiGroups: [""]
    resources: ["services", "pods", "namespaces"]
    verbs: ["get", "watch", "list"]

  # Add per-watched-resource rules as `spec.watchedResources` grows;
  # the Helm chart auto-generates these. See operations/security.md.
```

### Deployment Manifest

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: haptic-controller
  namespace: default
spec:
  replicas: 2  # Default; leader election handles deploy-side exclusivity
  selector:
    matchLabels:
      app: haptic
  template:
    metadata:
      labels:
        app: haptic
    spec:
      serviceAccountName: haptic-controller
      containers:
      - name: controller
        image: haptic:latest
        args:
          - run
          - --crd-name=haproxy-config
          - --secret-name=haproxy-credentials
          - --debug-port=8080
        env:
        - name: LOG_LEVEL
          value: "INFO"  # TRACE / DEBUG / INFO / WARN / ERROR
        ports:
        - name: healthz
          containerPort: 8080  # also serves /debug/* — see --debug-port
        - name: metrics
          containerPort: 9090
        livenessProbe:
          httpGet:
            path: /healthz
            port: healthz
          initialDelaySeconds: 30
          periodSeconds: 10
        readinessProbe:
          httpGet:
            path: /healthz       # the controller exposes /healthz only;
            port: healthz        # /readyz is not served separately
          initialDelaySeconds: 5
          periodSeconds: 5
        resources:
          requests:
            cpu: 100m
            memory: 512Mi
          limits:
            memory: 512Mi        # CPU limit deliberately omitted
```

## Resources

- Architecture: `/docs/site/docs/development/design.md`
- Controller orchestration: `pkg/controller/CLAUDE.md`
- Configuration: `pkg/core/CLAUDE.md`
- Helm chart: `charts/haptic/`
