# pkg/controller/rendercontext - Template Context Builder

Development context for the centralized template rendering context builder.

## When to Work Here

Modify this package when:

- Adding new context keys that all templates need access to
- Changing how context is built (e.g., new options)
- Modifying helper functions like `MergeExtraContextInto`

**DO NOT** modify this package for:

- Template rendering logic → Use `pkg/templating`
- HAProxy capabilities → Use `pkg/dataplane`

## Package Purpose

Single source of truth for template-rendering context construction. Two
call sites use `NewBuilder` directly:

| Call site | Usage |
|-----------|-------|
| `pkg/controller/testrunner/rendering.go` | Validation tests |
| `cmd/haptic/benchmark_render.go` | Performance benchmarks |

The production renderer (`pkg/controller/renderer/service.go`) builds its context directly via its own `buildRenderingContext` method. It is kept consistent with `Builder` by sharing the same helpers — `BuildResourcesValue` for `resources`, `CapabilitiesToMap` for `capabilities`, and `MergeExtraContextInto` for `extraContext` (always-set + top-level key promotion). When you add or change a context key, update **both** paths (or move the logic into a shared helper both call) so a template can't pass `controller validate` yet behave differently in production. The dry-run validator delegates to `proposalvalidator` → pipeline → renderer and therefore also does not call `NewBuilder` directly.

## Usage

```go
import "github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"

builder := rendercontext.NewBuilder(
    ctx,
    cfg,
    pathResolver,
    logger,
    rendercontext.WithStores(stores),
    rendercontext.WithHAProxyPodStore(haproxyPodStore),
    rendercontext.WithHTTPFetcher(httpWrapper),
    rendercontext.WithCurrentConfig(currentConfig), // optional; nil on first deploy
)

// Build returns the context plus its output collectors and ResourceErrors.
res := builder.Build()
ctx := res.Context
```

Call `res.Err(ctx)` after each render phase. It preserves the context cause and
rejects store-read, ambiguous-lookup, and typed-materialization failures that
the template-facing value-only methods can't return directly.

## Context Structure

The builder creates a context map with these keys (always populated unless
marked "optional"):

| Key | Type | Description |
|-----|------|-------------|
| `resources` | `map[string]templating.ResourceStore` (`*StoreWrapper` per entry) | Wrapped Kubernetes resource stores. When a schema is loaded for an entry, the wrapper's `.List() / .Fetch(...) / .GetSingle(...)` methods return typed pointers (`[]*resources.<name>.T` / `*resources.<name>.T`) sourced from the same typegen-built `reflect.Type` the engine uses for the typed top-level global. The schema-derived `IndexBy` is also propagated into the wrapper for typed `Fetch` lookups. |
| `controller` | `map[string]templating.ResourceStore` | Controller-managed stores; currently `controller["haproxy_pods"]` only |
| `templateSnippets` | `[]string` | Snippet names sorted alphabetically |
| `fileRegistry` | `*FileRegistry` | Dynamic file registration during render |
| `planRegistry` | `*PlanRegistry` | Section, backend and map declarations; `RenderMain` assembles the config from the tokens it hands back |
| `statusPatchCollector` | `*templating.StatusPatchCollector` | Captures status mutations from `filters_status.go` |
| `pathResolver` | `*templating.PathResolver` | File path resolution (relative vs absolute) |
| `dataplane` | `config.DataplaneConfig` | DataPlane API config block |
| `capabilities` | `map[string]any` | HAProxy version capabilities (`supports_crt_list`, `supports_waf`, …) from `CapabilitiesToMap`. Always populated (all-false map when no `WithCapabilities` option is passed) so validation and production expose the identical key. |
| `shared` | `*templating.SharedContext` | Per-render compute-once cache (`ComputeIfAbsent` etc.) |
| `runtimeEnvironment` | `*templating.RuntimeEnvironment` | GOMAXPROCS and related runtime info |
| `currentConfig` | `*renderplan.CurrentConfig` | Servers of the running config (`currentConfig.ServerIndex[backend][server].Address/.Port`) — *optional*, omitted when nil (first deploy) to dodge a Scriggo nil-pointer-initializer panic |
| `http` | `templating.HTTPFetcher` | HTTP resource fetching — *optional*, omitted when no fetcher passed |
| `extraContext` | `map[string]any` | User-defined variables from `cfg.TemplatingSettings.ExtraContext` (always set, possibly empty map). The same map's *top-level keys* are also merged into the context (`maps.Copy(renderCtx, cfg.TemplatingSettings.ExtraContext)` in `MergeExtraContextInto`), so templates can write `{{ debug.enabled }}` directly *and* `{{ extraContext | dig("debug", "enabled") }}` for the Scriggo-safe variant. |

## Functional Options

| Option | Purpose |
|--------|---------|
| `WithStores(map[string]stores.Store)` | Resource stores keyed by watched-resource name; ends up in `resources`; API-backed reads use the `NewBuilder` context |
| `WithHAProxyPodStore(stores.Store)` | HAProxy pod store; ends up in `controller["haproxy_pods"]` |
| `WithHTTPFetcher(templating.HTTPFetcher)` | Wires the `http` runtime variable so templates can call `http.Fetch(...)` |
| `WithCurrentConfig(*renderplan.CurrentConfig)` | Adds `currentConfig` to the context so templates can reason about the running servers; nil on the first deployment. Production feeds it from the plan the fleet ACKed (or the last reconcile render until a pod ACKs), projected via `plan.CurrentConfig()`; the testrunner builds it from a test's `currentServers:` fixture |
| `WithCapabilities(dataplane.Capabilities)` | Sets the HAProxy version capabilities exposed under `capabilities`. The testrunner passes the detected local capabilities so `controller validate` matches production; omit it and `Build()` still populates an all-false map |
| `WithNamespaceScopes(NamespaceScopes(cfg))` | Fails the render when a `Fetch`/`GetSingle` keys on a namespace outside a watched resource's `namespaces`. The production renderer always passes it; validation tests opt in with `useDeploymentWatchScopes: true` |

`extraContext` is **not** an option — `Build()` reads `cfg.TemplatingSettings.ExtraContext`
directly and always populates the `extraContext` key (with an empty map if the
CRD doesn't set one) so templates can safely chain `extraContext | dig("k") | fallback("v")`.

## Package Contents

This package contains:

- **Builder**: Constructs template rendering contexts with functional options pattern
- **StoreWrapper**: Wraps types.Store to provide template-friendly methods (List, Fetch, GetSingle)
- **FileRegistry**: Enables dynamic auxiliary file registration during template rendering
- **PlanRegistry**: Collects the sections, backend records and map metadata templates declare, assembles the config from their tokens (`Assemble`) and builds the `renderplan.Plan` (`Plan`)
- **RenderMain**: The one render-and-assemble path for `haproxy.cfg`, used by the renderer, the testrunner and the render benchmark
- **MergeAuxiliaryFiles**: Canonicalizes static and dynamic auxiliary files, deduplicating identical definitions and rejecting storage-identity conflicts
- **SortSnippetNames**: Helper to sort template snippet names alphabetically

## Dependencies

This package imports from:

- `pkg/core/config` - Config types
- `pkg/dataplane` - Capabilities, AuxiliaryFiles
- `pkg/k8s/types` - Store interface
- `pkg/templating` - ResourceStore, PathResolver, RuntimeEnvironment

## Testing

The builder is tested indirectly through the components that use it:

- Renderer tests verify production context creation
- TestRunner tests verify fixture-based contexts
- Benchmark tests verify performance characteristics

## Adding New Context Keys

When adding a new key to the template context:

1. Add the key in `Build()` method
2. Add an option if it's configurable
3. Update all callers if needed
4. Document in the table above
5. Update `pkg/templating/filters_scriggo.go` if Scriggo needs type info

## Resources

- Template engine: `pkg/templating/CLAUDE.md`
- Renderer component: `pkg/controller/renderer/README.md` (no CLAUDE.md in that package)
- TestRunner: `pkg/controller/testrunner/CLAUDE.md`
- DryRunValidator: `pkg/controller/dryrunvalidator/CLAUDE.md`
