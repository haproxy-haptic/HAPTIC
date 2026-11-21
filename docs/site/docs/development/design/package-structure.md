# Package structure

The controller is split into small Go packages with one of three roles:

- **Infrastructure** — domain-agnostic building blocks (`pkg/events`, `pkg/introspection`, `pkg/metrics`, `pkg/compression`, `pkg/lifecycle`, `pkg/transportsecurity`).
- **Domain libraries** — pure business logic with no coupling to the event bus (`pkg/k8s`, `pkg/dataplane`, `pkg/templating`, `pkg/stores`, `pkg/httpstore`, `pkg/webhook`).
- **Coordination** — event adapters that wire the libraries together (`pkg/controller` and its subpackages).

This split keeps the libraries reusable and independently testable; all event choreography is confined to `pkg/controller`.

## Repository layout

```
haptic/
├── cmd/
│   ├── haptic/              # CLI and controller entry point
│   ├── gen-validators/      # Generator for the zero-alloc OpenAPI validators in pkg/generated
│   └── playground/          # WASM browser template playground (drives the production RenderService)
│       └── internal/migratecheck/ # Ingress migration report classifier
├── pkg/
│   ├── apis/                # CRD type definitions (HAProxyTemplateConfig, HAProxyCfg, ...)
│   ├── blob/                # Blob declarations, references, and their data-plane path
│   ├── blobstore/           # Content-addressed blob files on the controller's disk
│   ├── compression/         # zstd helpers used by CRD content compression
│   ├── core/                # Shared primitives (config parsing, logging setup)
│   ├── events/              # Generic EventBus and request/response plumbing
│   │   └── ringbuffer/      # Thread-safe generic ring buffer (event history)
│   ├── generated/           # Code generation output (clientset, informers, listers;
│   │                        #   plus the playground-only OpenAPI validators)
│   ├── httpstore/           # Fetches and caches HTTP resources referenced from templates
│   ├── incremental/         # Generic incremental computation graph (exact deps, revisions)
│   ├── introspection/       # /debug/vars HTTP infrastructure (registry, JSONPath, pprof)
│   ├── k8s/                 # Pure Kubernetes integration library
│   │   ├── client/          # Dynamic client + namespace auto-detection
│   │   ├── configpublisher/ # Publishes HAProxyCfg and HAProxyGeneralFile CRDs
│   │   ├── indexer/         # JSONPath extraction + metadata trimming
│   │   ├── leaderelection/  # Pure leader election (no event bus dependency)
│   │   ├── schemafetcher/   # Fetches OpenAPI schemas from the apiserver (or --schema-dir)
│   │   ├── store/           # MemoryStore and CachedStore
│   │   ├── typegen/         # Runtime typed access to watched resources from their schema
│   │   ├── types/           # Store, WatcherConfig, SingleWatcherConfig, ChangeStats
│   │   └── watcher/         # Bulk Watcher and SingleWatcher with debouncing
│   ├── lifecycle/           # Component lifecycle registry (dependencies, leader-only,
│   │                        #   startup ordering, health tracking)
│   ├── metrics/             # Instance-based Prometheus registry + /metrics server
│   ├── persistenttree/      # Immutable ordered map (domain-free container)
│   ├── rendercontent/       # Rendered-output value types (Output, TextFragment, Document)
│   ├── stores/              # Store overlays/providers used for webhook dry-run validation
│   ├── templating/          # Scriggo-based template engine (pure)
│   ├── dataplane/           # The path from a render to a running HAProxy (pure)
│   │   ├── renderplan/      # What a render declares: sections, backends, maps, files
│   │   ├── deployplan/      # What one pod has to do to reach a render
│   │   ├── agent/           # The HAPTIC agent and its wire contract
│   │   │   ├── api/         #   the contract, compiled by both ends
│   │   │   ├── client/      #   the controller's end: State + streaming Apply
│   │   │   ├── server/      #   the agent: HTTP surface, state machine, transaction
│   │   │   ├── files/       #   the file tree it owns: mounts, journal, temp+rename
│   │   │   └── cli/         #   typed ops → HAProxy runtime commands
│   │   ├── auxiliaryfiles/  # The auxiliary-file types a render produces
│   │   ├── parser/          # playground-only: client-native syntax parse
│   │   └── validators/      # playground-only: per-model OpenAPI validators
│   │   # Public types (Endpoint, AuxiliaryFiles, Capabilities, Version,
│   │   # ValidationPaths) live at the top level — there is no
│   │   # pkg/dataplane/types subpackage.
│   ├── webhook/             # HTTP server shared by validating webhook and health probes
│   └── controller/          # Event-driven orchestration (adapters + components)
│       ├── component/       # Shared event-loop scaffold embedded by most components
│       ├── blobstore/       # Enables blob.Fetch sources and removes unreferenced blobs
│       ├── buffers/         # Byte-buffer pool used by render/validation hot paths
│       ├── coalesce/        # Coalesces bursts of events into a single work item
│       ├── commentator/     # Structured domain-aware log producer
│       ├── configchange/    # Reacts to config changes and coordinates reloads
│       ├── configloader/    # Parses HAProxyTemplateConfig CRD into internal config
│       ├── configpublisher/ # Publishes rendered config to HAProxyCfg CRD
│       ├── configtest/      # Runs a config's embedded validationTests offline
│       ├── conversion/      # Converts CRD types <-> internal config structs
│       ├── crdwatch/        # Reinitializes the controller when watched-resource CRDs change
│       ├── credentialsloader/ # Parses dataplane credentials from Secret
│       ├── debug/           # Controller-specific introspection Vars
│       ├── deployer/        # Scheduler + per-instance deployer + drift-prevention monitor
│       ├── discovery/       # HAProxy pod discovery
│       ├── dryrunvalidator/ # Webhook dry-run validator
│       ├── eventemitter/    # Emits template-requested Kubernetes Events (recordEvent) via EventRecorder
│       ├── events/          # Domain event type catalog
│       ├── helpers/         # Template engine factories (NewEngineFromConfigWithOptions +
│       │                    #   ExtractTemplatesFromConfig) shared by renderer / dryrun /
│       │                    #   testrunner / cmd validate
│       ├── httpstore/       # HTTP resource fetcher + watcher
│       ├── indextracker/    # Tracks initial-sync completion across resources
│       ├── leaderelection/  # Event adapter around pkg/k8s/leaderelection
│       ├── leadership/      # Gating utilities for leader-only components
│       ├── metrics/         # Controller-domain metrics adapter (reconciliation,
│       │                    #   deployment, validation, event-bus counters)
│       ├── names/           # Well-known string constants shared across the controller
│       ├── pipeline/        # Chains stages into a composable reconciliation pipeline
│       ├── pluggablevalidator/ # Client for the pluggable-validator-sidecar wire protocol
│       ├── proposalvalidator/ # Validates proposed configs from the webhook
│       ├── reconciler/      # Triggers reconciliation and drives its pipeline
│       ├── rendercontext/   # Builds the template context from stores and HTTP resources
│       ├── renderer/        # Synchronous template rendering service
│       ├── resourceapplier/ # Reconciles template-declared resources via Server-Side Apply
│       │                    #   (configloader, credentialsloader)
│       ├── resourcewatcher/ # Lifecycle manager for all configured resource watchers
│       ├── statusapplier/   # Applies status subresources on CRDs
│       ├── testrunner/      # Runs embedded validation tests from template libraries
│       ├── throttle/        # Leading-edge refractory throttle helpers
│       ├── timeouts/        # Timeout helpers used by components
│       ├── timers/          # Periodic event emitters (drift prevention, etc.)
│       ├── typebootstrap/   # Wires the typed-watched-resources pipeline at startup
│       ├── validation/      # Shared validation helpers
│       ├── validator/       # Syntax/semantic validators (scatter-gather + HAProxy)
│       └── webhook/         # Event adapter bridging the pure webhook library
├── tests/
│   ├── acceptance/          # End-to-end tests with debug endpoint + metrics assertions
│   └── integration/         # Cross-component integration tests
└── tools/linters/
    └── eventimmutability/   # Custom golangci-lint analyzer (enforces pointer receivers on Event)
```

Most packages carry a `README.md` (user-facing API) and a `CLAUDE.md` (developer context); prefer those as the authoritative reference where they exist. Smaller packages — `pkg/controller/eventemitter`, `pkg/controller/crdwatch`, `pkg/controller/throttle`, `pkg/k8s/typegen` — document themselves in package doc comments instead. This file only orients new contributors.

## Dependency rules

The packages form a DAG, enforced at build time by `arch-go.yml`:

1. `pkg/events` depends on nothing in `pkg/` — it's plain pub/sub plumbing.
2. Domain libraries (`pkg/k8s`, `pkg/dataplane`, `pkg/templating`, `pkg/stores`, `pkg/httpstore`, `pkg/webhook`) depend only on `pkg/core`, `pkg/events` (for optional observability hooks), and each other through narrow interfaces. They have no knowledge of the controller.
3. `pkg/controller` is the only package allowed to import everything. It owns the event adapters and the startup/shutdown choreography.
4. Domain event types live in `pkg/controller/events`, never in `pkg/events`.

`pkg/stores` is deliberately isolated from `pkg/k8s` (see `arch-go.yml`); the two declare the same `Store` interface shape, so callers can assign stores directly across the package boundary.

## Key patterns

**Shared component scaffold.** `pkg/controller/component.Base` implements the event-loop boilerplate (subscribe-on-construction, single-flight dispatch, panic recovery, ready/done signalling). Components embed `*Base` and implement `EventHandler`. The configuration and credential loaders use it directly. `BaseValidator` adds request decoding and response publication for configuration validation.

**Pure libraries, event adapters.** Business logic (`pkg/templating`, `pkg/dataplane`, `pkg/k8s`) exposes plain Go APIs. Corresponding adapters in `pkg/controller/renderer`, `pkg/controller/deployer`, `pkg/controller/resourcewatcher`, etc. translate events into calls and publish result events.

**Scatter-gather validation.** Configuration validation uses `pkg/events` request/response to fan a `ConfigValidationRequest` out to independent validators (structural, template syntax, JSONPath, HAProxy config) and aggregate responses.

**Single-resource vs. bulk watching.** `pkg/k8s/watcher` provides `Watcher` (collections, debounced) and `SingleWatcher` (one named resource, immediate callbacks) so the `HAProxyTemplateConfig` CRD and credentials Secret don't pay indexing overhead.

**HAProxy validation.** `pkg/dataplane` runs `haproxy -c` against the rendered
configuration and auxiliary files. Production binaries contain no HAProxy
configuration parser; the browser playground uses a separate syntax/schema
fallback. `pkg/controller/validation.ValidationService` isolates each call in a
temporary directory and adjusts `default-path origin` accordingly. Admission
checks run synchronously; reconciliation uses the render gate, which tracks
verdicts by plan identity. See [ADR-0022](../adr/0022-haptic-agent.md).

**Component lifecycle.** `pkg/lifecycle` centralises registration, dependency ordering, leader-only flags, and health tracking; the controller registers every component there instead of starting goroutines directly.

## Finding things

- **Public API of a library** → `pkg/<name>/README.md`.
- **Developer guidance when editing a package** → `pkg/<name>/CLAUDE.md`.
- **Event types and their producers/consumers** → `pkg/controller/events/`.
- **Where a particular event is handled** → grep for the event type name; the event adapter's `HandleEvent` method names the case.
