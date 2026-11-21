# pkg/ - Package Organization

Development context for working with packages in this directory.

## Resource-Agnostic Engine (RULE #1)

**Every package under `pkg/` must be agnostic to the Kubernetes resources an operator chooses to watch.** No package may bake in Gateway/Ingress/HTTPRoute/Service/Secret knowledge or be more comfortable to use against those well-known kinds than against an arbitrary CRD.

**The litmus test.** If an operator decided to use some CRD instead of Gateway or Ingress resources, they should only need to touch HAPTIC templates and config — no Go code in `pkg/`. Writing templates for the operator's CRD must be just as comfortable as for Ingress or Gateway API resources. **There must be no preferential treatment for well-known resources.**

**Concrete prohibitions:**

- Filters, helpers, or types in `pkg/templating` keyed on specific kinds (`scriggoGatewayListenerStatus`, `scriggoIngressBackendName`, etc.).
- Code-generated per-resource wrappers in `pkg/k8s` or anywhere else. Resource shape comes from the runtime schema source (`pkg/k8s/typegen` consuming the live apiserver or `--schema-dir`), so the controller never knows the resource set at build time.
- "Chart-render-time runtime context" types whose fields encode chart-specific resource concepts (e.g. `GlobalFeatures.TLSCertificates`, `SSLPassthroughBackend`, `GatewayListenerMTLSConfig`). These look generic at a glance but a chart watching different resources would never instantiate them.
- Anything that splits resources into first-class (typed Go shapes) and second-class (only `map[string]any`) citizens. They're all second-class from the engine's perspective; the schema makes them all typed equally if one is provided.

**Acceptable Go surface area:**

- Generic engine utilities on the dig / typed-struct interface.
- Schema-driven type generation that operates uniformly on any input schema.
- Map/`any`-shaped runtime context, including the `shared.Get(...).(map[string]any)` cast cost in chart code. This is the price of generality — accept it.
- **The controller's own operational identity.** Kind-specific Go for the inputs HAPTIC needs to run *itself* is fine and is NOT a RULE #1 violation: the credentials Secret (`secretGVR`, `SecretResourceChangedEvent`, `.data` parsing in `credentialsloader`), the auto-injected `haproxy-pods` self-watch (`names.HAProxyPodsResourceType` and its store separation / discovery filtering), and HAPTIC's own input/output CRDs. These are fixed bootstrap inputs, not resources an operator watches as template inputs. See the "operational-identity exception" in root `CLAUDE.md`. Do not generalise them into config-derived indirection.

**Sweep rule.** Touch one resource-coupled helper in a package, sweep them all. The rule is per-package, not per-helper. See `pkg/templating/CLAUDE.md` for the in-engine version.

See root `CLAUDE.md` ("Resource-Agnostic Design (RULE #1)") for the project-wide statement and `charts/CLAUDE.md` for the chart-side corollary (resource-specific behaviour lives in resource-specific template libraries, never in `base.yaml`).

## Package Architecture

The codebase follows clean architecture with clear separation of concerns:

```
pkg/
├── apis/              # CRD type definitions (haproxytemplate/v1alpha1)
├── compression/       # zstd + base64 helper used by output CRDs
├── core/              # Shared primitives (config, logging)
├── events/            # Generic event bus (domain-agnostic)
├── generated/         # Code-generation output (clientset, DataPlane API clients, validators)
├── httpstore/         # Pure HTTP resource cache (two-version pending/accepted)
├── incremental/       # Generic incremental computation graph (exact deps, revisions)
├── introspection/     # Generic /debug/vars HTTP server
├── lifecycle/         # Component registry, dependency ordering, leader-only gating
├── metrics/           # Prometheus registry/server primitives
├── persistenttree/    # Immutable ordered map (domain-free container)
├── rendercontent/     # Rendered-output value types (Output, TextFragment, Document)
├── stores/            # Store overlay/provider used for webhook dry-run
├── templating/        # Pure template engine library (Scriggo)
├── transportsecurity/ # Reloadable mutual TLS for HTTP connections
├── k8s/               # Kubernetes integration library
├── dataplane/         # HAProxy integration library
├── webhook/           # Pure admission-webhook HTTPS server
└── controller/        # Orchestration and coordination (the only event-bus consumer)
```

For the canonical layout (with sub-packages), see [`docs/site/docs/development/design/package-structure.md`](../docs/site/docs/development/design/package-structure.md).

## Dependency Hierarchy

### Layer 1: Infrastructure (No Dependencies on Other pkg/ Packages)

**pkg/events/** — Generic pub/sub + request/response. NO domain knowledge. Imported by: everything else.

**pkg/introspection/** — Generic `/debug/vars` HTTP server (registry + JSONPath + pprof).

**pkg/metrics/** — Generic Prometheus registry + `/metrics` server.

**pkg/transportsecurity/** — Utility for atomic certificate snapshots and reloadable mutual TLS. No Kubernetes or domain dependencies.

**pkg/lifecycle/** — Component registry, dependency ordering, leader-only gating, health tracking.

**pkg/compression/** — zstd + base64 helper used by output-CRD content.

**pkg/persistenttree/** — Immutable ordered map with structural sharing. Domain-free container; stdlib only.

**pkg/blob/** — Blob declarations (`Spec`), references (`Ref`), and the data-plane path prefix (ADR-0041). Stdlib only; shared by the template engine, the stores, and the agent.

**pkg/rendercontent/** — Value types for rendered output (`Output`, `TextFragment`, `Document`) and their deltas. Shared by the renderer and the dataplane; no HAProxy vocabulary.

**pkg/incremental/** — Generic incremental computation graph: opaque input/query keys, revisions, exact dependency journals, transactional commit. Knows nothing about templating or Kubernetes; depends only on `pkg/persistenttree`. See [ADR-0023](../docs/adr/0023-incremental-render-graph.md).

**pkg/apis/** and **pkg/generated/** — CRD type definitions and code-generation output (clientset, informers, listers, DataPlane API clients per HAProxy version, OpenAPI validators). Authored by `controller-gen` / `oapi-codegen`; treated as pure data shapes.

### Layer 2: Pure Libraries (Minimal Dependencies)

**pkg/core/**

- Configuration types and parsing
- Logging setup
- Depends on: standard library only
- Imported by: most other packages

**pkg/templating/**

- Template compilation and rendering
- Depends on: Scriggo, standard library
- Imported by: controller package

**pkg/k8s/**

- Resource watching, indexing, storage
- Depends on: client-go, events (for coordination)
- Imported by: controller package

**pkg/dataplane/**

- HAProxy configuration sync
- Depends on: client-native, events (for observability)
- Imported by: controller package

**pkg/httpstore/**

- Pure HTTP resource store (two-version pending/accepted)
- Depends on: standard library, `pkg/blob`

**pkg/blobstore/**

- Pure on-disk store of content-addressed blobs (ADR-0041)
- Depends on: standard library, `pkg/blob`
- Imported by: controller package

**pkg/webhook/**

- HTTPS server speaking Kubernetes AdmissionReview v1
- Depends on: net/http, k8s.io/api, k8s.io/apimachinery, k8s.io/client-go
- Imported by: controller package

**pkg/stores/**

- `Store` overlay/provider used to inject hypothetical resources during webhook dry-run validation
- Depends on: no internal packages; its `Store` interface accepts Kubernetes stores directly by structural typing (`arch-go.yml` enforces isolation)
- Imported by: controller package

### Layer 3: Coordination (Depends on Everything)

**pkg/controller/**

- Event-driven orchestration
- Component lifecycle management
- Event adapters wrapping pure components
- Depends on: all above packages
- Defines: domain-specific event types (in controller/events/)

## When to Create a New Package

### Create a new top-level package when

- **Reusable library**: Code could be used by multiple applications
- **Clear boundary**: Package has well-defined responsibility
- **Minimal dependencies**: Package has few dependencies on other packages
- **Pure logic**: Business logic without coordination concerns

**Example**: `pkg/templating` is a pure template engine that could be reused in other projects.

### Create a new sub-package when

- **Related functionality**: Code belongs to parent package's domain
- **Internal organization**: Breaking up a large package for readability
- **Implementation details**: Hide internal types from package users

**Example**: `pkg/dataplane/comparator/sections/` contains section-specific comparison logic.

### Extend existing package when

- **Same responsibility**: Feature fits existing package's purpose
- **Shared types**: Uses same core types and interfaces
- **No new dependencies**: Doesn't introduce new dependencies

## Package Design Patterns

### Pure Components

Packages like `templating`, `k8s`, `dataplane` provide pure business logic:

```go
// pkg/templating/engine_interface.go
package templating

// No event dependencies - pure library (real type is templating.Engine)
type Engine interface {
    Render(ctx context.Context, templateName string, templateContext map[string]any) (string, error)
    // ... HasTemplate, TemplateNames, EnableTracing, etc.
}
```

### Event Adapters

Only `pkg/controller` contains event coordination:

A typical event-adapter wraps a pure component (e.g. `pkg/templating.Engine`)
in a constructor that subscribes to the bus before returning so events
buffered during startup aren't lost, then runs a single goroutine that
dispatches one event at a time. The skeleton below is illustrative; for the
shared scaffold every controller component embeds, see
`pkg/controller/component`. Note that not every "renderer-ish" surface is an
event adapter — the production renderer is in fact a synchronous
`renderer.RenderService` called from the pipeline, with no event hop. Look
at `pkg/controller/renderer/README.md` for the real shape.

```go
// Illustrative — not a real package. Shows the event-adapter shape.
package examplerenderer

import (
    "haptic/pkg/events"
    "haptic/pkg/templating"
)

// Event adapter wraps pure component
type Component struct {
    engine    templating.Engine    // Pure component
    eventBus  *events.EventBus    // Event coordination
    eventChan <-chan events.Event  // Subscribed in constructor
}

func New(bus *events.EventBus, engine templating.Engine) *Component {
    return &Component{
        engine:    engine,
        eventBus:  bus,
        eventChan: bus.Subscribe("examplerenderer", 100),  // Subscribe in constructor, before bus.Start()
    }
}

// Method name is Start (not Run) — that's what the lifecycle.Component
// interface requires; every controller component in pkg/controller/*/component.go
// follows the same shape.
func (c *Component) Start(ctx context.Context) error {
    for {
        select {
        case event := <-c.eventChan:
            // Convert event to pure function call
            switch e := event.(type) {
            case ReconciliationTriggeredEvent:
                output, err := c.engine.Render(ctx, "haproxy.cfg", e.Context)
                // Publish result event
                if err != nil {
                    c.eventBus.Publish(RenderFailedEvent{Error: err})
                } else {
                    c.eventBus.Publish(RenderCompletedEvent{Output: output})
                }
            }
        case <-ctx.Done():
            return ctx.Err()
        }
    }
}
```

## Interface Design

### Guidelines

1. **Keep interfaces small**: Single-method interfaces are idiomatic Go
2. **Define interfaces at use site**: Consumer defines the interface
3. **Accept interfaces, return structs**: Flexibility at boundaries
4. **Avoid interface pollution**: Not everything needs an interface

### Example Pattern

```go
// pkg/dataplane/dataplane.go - Provide concrete type
package dataplane

type Client struct {
    // implementation
}

func (c *Client) GetVersion() (string, error) { ... }
func (c *Client) DeployConfig(cfg string) error { ... }

// At the consumer site (pseudo-example): define a narrow interface
package myconsumer

// Only need a subset of the dataplane.Client methods
type ConfigDeployer interface {
    DeployConfig(cfg string) error
}

type Component struct {
    deployer ConfigDeployer  // Accepts any type implementing this — *dataplane.Client, a fake, etc.
}
```

In the real codebase this pattern shows up in `pkg/controller/reconciler/coordinator.go`, where `Coordinator` accepts a `PipelineExecutor` interface rather than the concrete pipeline type.

## Cross-Package Communication

### Direct Calls (Preferred within layers)

Use direct function calls for pure components:

```go
// In an event-adapter component (e.g. pkg/controller/renderer)
import "github.com/haproxy-haptic/HAPTIC/pkg/templating"

func (c *Component) render(ctx context.Context) (string, error) {
    // Direct call to the pure component — no event needed for this hop
    return c.templateEngine.Render(ctx, "haproxy.cfg", c.context)
}
```

### Events (For cross-layer coordination)

Use events for decoupled coordination:

```go
// Resource watcher publishes event
watcher.eventBus.Publish(ResourceIndexUpdatedEvent{Type: "ingress"})

// Multiple subscribers can react
// - Reconciler triggers reconciliation
// - Commentator logs the change
// - Metrics collector updates counters
```

## Testing Strategies

### Unit Tests (Same Package)

Test pure components in isolation:

```go
// pkg/templating/engine_scriggo_test.go
package templating

func TestEngine_Render(t *testing.T) {
    engine, _ := New(map[string]string{
        "test": "Hello {{ name }}",
    }, nil)

    output, err := engine.Render(context.Background(), "test", map[string]any{
        "name": "World",
    })

    require.NoError(t, err)
    assert.Equal(t, "Hello World", output)
}
```

### Integration Tests (Cross-Package)

Test package interactions:

```go
// Illustrative — real cross-package wiring lives in
// pkg/controller/reconciler/coordinator_test.go and similar files.
package examplecoordinator

import (
    "haptic/pkg/events"
    "haptic/pkg/templating"
)

func TestCoordinator_Integration(t *testing.T) {
    bus := events.NewEventBus(100)
    engine, _ := templating.New(...)
    coord := New(bus, engine, ...)  // hypothetical adapter

    // Test cross-package interaction
    bus.Publish(events.NewReconciliationTriggeredEvent("test", true))
    // Verify expected behavior
}
```

## Common Pitfalls

### Circular Dependencies

**Problem**: Package A imports B, B imports A.

**Solution**: Extract shared types to new package or use interfaces.

```go
// Bad
pkg/dataplane → imports → pkg/controller
pkg/controller → imports → pkg/dataplane

// Good
pkg/dataplane → returns concrete types
pkg/controller → defines interfaces at use site
```

### Event Type Location

**Problem**: Putting domain events in `pkg/events`.

**Solution**: Domain events go in `pkg/controller/events`, only infrastructure in `pkg/events`.

```go
// Wrong location
pkg/events/types.go:
    type ReconciliationTriggeredEvent struct { ... }  // Domain event

// Correct location
pkg/controller/events/types.go:
    type ReconciliationTriggeredEvent struct { ... }  // Domain event

pkg/events/bus.go:
    type Event interface { ... }  // Infrastructure only
```

### Too Many Small Packages

**Problem**: Creating a package for every file.

**Solution**: Group related functionality. A package can have 5-10 files.

### Leaking Implementation Details

**Problem**: Exposing internal types in public API.

**Solution**: Use interfaces or copy data at package boundaries.

```go
// Bad - leaking internal type
func (c *Client) GetRawParser() *clientnative.Parser { ... }

// Good - return interface or copy
func (c *Client) ParseConfig(cfg string) (*ParsedConfig, error) { ... }
```

## Adding New Features

### Checklist

1. **Identify layer**: Infrastructure, library, or coordination?
2. **Check existing packages**: Does feature fit an existing package?
3. **Define interface**: What API should the feature expose?
4. **Write tests first**: Test-driven development
5. **Implement pure logic**: No event dependencies in libraries
6. **Add event adapter**: If needed, wrap in controller package
7. **Update README.md**: Document public API
8. **Update CLAUDE.md**: Add development context

### Example: Adding Custom Template Filters

`pkg/templating` has no `RegisterFilter` method — filters are registered at engine
construction by passing a `map[string]templating.FilterFunc`. The engine is
immutable after `New()` returns. To add a filter:

```go
// Step 1: Define the filter in pkg/templating (or wherever the filter lives).
// Filters take the piped value as `in` and any extra positional args.
var base64DecodeFilter templating.FilterFunc = func(in any, args ...any) (any, error) {
    s, ok := in.(string)
    if !ok {
        return nil, fmt.Errorf("b64decode: want string, got %T", in)
    }
    return base64.StdEncoding.DecodeString(s)
}

// Step 2: Pass the filter map at engine construction (callers usually
// merge built-in and CRD-provided filters here).
engine, err := templating.New(templates, &templating.Options{
    Filters: map[string]templating.FilterFunc{"b64decode": base64DecodeFilter},
})
```

## Resources

- Root-level architecture: `/CLAUDE.md`
- Package-specific context: `pkg/*/CLAUDE.md`
- Architecture documentation: `/docs/site/docs/development/design.md`
- Package API documentation: `pkg/*/README.md`
