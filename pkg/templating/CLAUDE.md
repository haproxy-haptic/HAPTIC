# pkg/templating - Template Engine

Development context for the template engine library.

**API Documentation**: See `pkg/templating/README.md`
**Architecture**: See `/docs/site/docs/development/design/design-decisions.md` (template engine selection)

## Engine

The project uses Scriggo as its template engine. Scriggo provides Go template syntax with high performance.

## When to Work Here

Modify this package when:

- Adding custom template filters
- Improving template compilation performance
- Fixing template rendering bugs
- Enhancing error reporting

**DO NOT** modify this package for:

- Rendering coordination → Use `pkg/controller`
- Event handling → Use `pkg/events`
- HAProxy configuration → Use `pkg/dataplane`
- Kubernetes resources → Use `pkg/k8s`

## Key Design Principles

### Pure Library

This is a **pure library** with no controller or Kubernetes dependencies. Its status projection index uses the domain-free immutable container in `pkg/persistenttree`.

Dependencies: Scriggo fork (from `github.com/haproxy-haptic/scriggo`), `pkg/persistenttree`, and standard library.

### Resource-Agnostic Functions (RULE #1)

**CRITICAL**: Template functions MUST be resource-agnostic. This is a generic templatable ingress controller where **no Kubernetes resource gets preferential treatment**.

**Litmus test.** If an operator decided to use some CRD instead of Gateway or Ingress resources, they should only need to touch HAPTIC templates and config — no Go code. Writing templates for the operator's CRD must be just as comfortable as for Ingress or Gateway API resources. The same litmus applies to every filter, helper, type, and global in this package: if it would only feel ergonomic for the chart's bundled well-known resources, redesign or reject.

**DO NOT add functions that:**

- Parse or navigate specific K8s resource structures (Service, Ingress, HTTPRoute, Gateway, GatewayClass, etc.)
- Provide shortcuts for specific resource fields (e.g., `lookup_port_name(service, port)`)
- Assume knowledge of any particular resource schema
- Take an argument that *encodes* a resource path even if the function body is technically generic (e.g. `listenerTransitionTime(gateway, listenerName, type, status)` is wrong — the `gateway` + `listenerName` parameters embed `.status.listeners[byName].conditions` knowledge in the function signature)
- Add typed Go structs for "chart-render-time runtime context" whose fields encode chart-specific resource concepts (e.g. `GlobalFeatures.TLSCertificates`, `SSLPassthroughBackend`, `GatewayListenerMTLSConfig`). These look generic at a glance but a chart watching different CRDs would never use them — that's preferential treatment in disguise. Keep `shared.Get(...)` and the chart-runtime context as `map[string]any`; the `.(map[string]any)` cast cost in chart code is the price of generality. Acceptable generic alternatives if the cast noise becomes intolerable: (a) chart YAML declares its own runtime-shape schemas which typegen consumes uniformly, (b) a generic engine filter that reduces cast verbosity without committing to specific shapes.

**Sweep when refactoring:** if you touch one resource-coupled helper, sweep ALL helpers in the package — `filters_status.go`, `filters_navigation.go`, etc. — and remove every other one too. The principle is per-package, not per-helper.

**Instead:**

- Provide generic utilities: `dig()`, `fallback()`, `toSlice()`, `toint()`, etc.
- Let users write resource-specific logic as **template macros** in their libraries
- Keep the template engine completely ignorant of Kubernetes semantics

**Example of WRONG approach:**

```go
// DON'T: Resource-specific function in templating package
func lookupServicePortName(svc any, portNumber int) string {
    // Navigates Service.spec.ports structure - WRONG!
    ports := dig(svc, "spec", "ports")
    // ...
}
```

**Example of CORRECT approach:**

```scriggo
{#- Users write resource-specific logic as macros in their library files -#}
{% macro LookupPortName(svc any, port int) string %}
  {%- var svcPorts = svc | dig("spec", "ports") | toSlice() -%}
  {%- for _, sp := range svcPorts -%}
    {%- if toint(sp | dig("port") | fallback(0)) == port -%}
      {{- sp | dig("name") | fallback("") -}}
    {%- end -%}
  {%- end -%}
{% end %}
```

This separation ensures the template engine remains generic while users can build any resource-specific functionality they need in their template libraries.

### Resource Emission

The template engine does **not** ship a `renderResource()`-style filter that emits Kubernetes resources as a side effect of rendering. That used to exist; it has been removed.

Resource emission is now a top-level CR concern, owned by the consumer of this engine. The controller's CRD declares a `spec.k8sResources` map (sibling of `templateSnippets`, `maps`, `files`, `sslCertificates`); the renderer renders each entry as a normal template, then parses the output as one or more YAML documents (multi-doc supported via `---`) and registers the parsed objects with the existing `RenderedResourceCollector` synchronously. Downstream consumers (resourceapplier) read the resulting slice off `RenderResult.RenderedResources` exactly as before.

The collector type stays on the consumer-facing path; this package is unchanged in shape. What was removed is the imperative filter that fed it from inside template execution. New work should not reintroduce side-effecting filters — keep template execution pure and let the renderer take responsibility for any "register this resource" semantics.

## Package Structure

```
pkg/templating/
├── engine_interface.go         # Engine interface (Render, tracing controls, introspection)
├── engine_scriggo.go           # Scriggo engine core: constructors, Render, RenderWithProfiling
├── engine_scriggo_fs.go        # Virtual filesystem for template compilation
├── engine_scriggo_tracing.go   # Tracing + filter-debug enable/disable plumbing
├── filters_archive.go          # untar_gz filter (bounded tar.gz expansion)
├── render_event.go             # RenderedEvent + EventCollector — the Kubernetes Events templates register during a render
├── func_types.go               # FilterFunc / GlobalFunc signatures
├── types.go                    # Type definitions (EngineType, PathResolver, ResourceStore, etc.)
├── errors.go                   # *CompilationError / *RenderError / *TemplateNotFoundError / *RenderTimeoutError
├── error_formatter.go          # Pretty-prints Scriggo diagnostics for the CLI
├── filter_names.go             # Constants for every filter name (one source of truth)
├── filters.go                  # Filter registration entry point
├── filters_scriggo.go          # Scriggo-specific filter wiring + custom-filter dispatch
├── filters_collection.go       # glob_match / shard_slice / first_seen
├── filters_pipeline.go         # filter / reject / flat_map / unique / unique_by / group_by / sort_by
├── filters_navigation.go       # dig / fallback / coalesce / merge / keys
├── filters_string.go           # toLower / replace / split / hasPrefix / hasSuffix / strip / b64decode / debug / indent
├── filters_type.go             # tostring / toint / tofloat / toSlice
├── filters_status.go           # Generic resource status-patch helpers (statusPatch, condition, transitionTime functions)
├── filters_jsonpath.go         # Generic resource-agnostic helpers: dynamic resource access + concrete-JSONPath get/set (resource / jsonpathGet / jsonpathSet)
├── filters_version.go          # semver_gte and friends
├── filters_guid.go             # GUID generation helpers (make_guid)
├── regex_cache.go              # Engine-scoped bounded regex compilation cache
├── postprocessor.go            # Post-processor pipeline runner
├── postprocessor_regex.go      # regex_replace post-processor
├── postprocessor_template.go   # template post-processor (second Scriggo pass with `input`)
├── profiling.go                # IncludeStats accumulation for --profile-includes
├── shared_context.go           # *SharedContext (ComputeIfAbsent / Get) for per-render caching
├── sorting.go                  # Comparator construction for sort_by
├── sorting_helpers.go          # Sort-modifier parsing (:desc / :exists / | length)
├── status_patch.go             # Apply* helpers consumed by filters_status.go
├── rendered_resource.go        # RenderedResourceCollector — accumulates k8sResources rendered by the renderer
└── *_test.go                   # Unit tests + benchmark_pool_test.go / benchmark_test.go
```

Use `go doc ./pkg/templating` for the authoritative export list — this layout grows.

## Core Design

### Pre-compilation Strategy

Templates are compiled once at initialization for optimal runtime performance:

```go
// Compilation happens once (Scriggo is default)
engine, err := templating.New(templates, nil)
if err != nil {
    // Compilation errors caught early
    log.Fatal(err)
}

// Rendering is fast (microseconds)
for i := 0; i < 1000; i++ {
    output, err := engine.Render(ctx, "template", context)
}
```

**Benefits:**

- Syntax errors detected at startup (fail-fast)
- No runtime compilation overhead
- Thread-safe concurrent rendering
- Predictable performance

### Error Types

Four distinct error types for clear error handling (`pkg/templating/errors.go` is authoritative):

```go
// CompilationError - template has syntax errors
type CompilationError struct {
    TemplateName    string
    TemplateSnippet string  // First 200 chars for context
    Cause           error
}

// RenderError - runtime rendering failed
type RenderError struct {
    TemplateName string
    Cause        error
}

// TemplateNotFoundError - template doesn't exist
type TemplateNotFoundError struct {
    TemplateName       string
    AvailableTemplates []string
}

// RenderTimeoutError - rendering exceeded the context deadline.
// Wraps the underlying context error so errors.Is(err, context.DeadlineExceeded)
// works at the call site.
type RenderTimeoutError struct {
    TemplateName string
    Cause        error
}
```

## Scriggo Runtime Variables

Scriggo requires special handling for runtime variables (passed via `template.Run(vars)`).

### The Nil Pointer Pattern

**Problem:** If a variable is declared in globals with an initial value AND passed at runtime, Scriggo panics with "variable already initialized".

**Solution:** Declare runtime variables with **nil pointers** so Scriggo knows the TYPE at compile time, but the VALUE is provided at runtime:

```go
// In filters_scriggo.go buildScriggoGlobals()
decl["pathResolver"] = (*PathResolver)(nil)           // Not &PathResolver{}
decl["resources"] = (*map[string]ResourceStore)(nil)  // Not &map[string]{}
decl["templateSnippets"] = (*[]string)(nil)           // Not &[]string{}
```

**Why this works:**

- Compile time: Scriggo sees the TYPE from the nil pointer declaration
- Runtime: `template.Run(vars)` provides the actual VALUE
- No conflict because the compile-time declaration has no value

### Common Runtime Variables

| Variable | Type | Purpose |
|----------|------|---------|
| `pathResolver` | `*PathResolver` | File path resolution |
| `resources` | `*map[string]ResourceStore` | Kubernetes resources |
| `controller` | `*map[string]ResourceStore` | Controller state |
| `templateSnippets` | `*[]string` | Available snippet names |
| `fileRegistry` | `*FileRegistrar` | Dynamic file registration |
| `planRegistry` | `*PlanRegistrar` | Section / backend / map declarations assembled into the config |
| `shared` | `*SharedContext` | Cross-template shared state |
| `dataplane` | `*map[string]any` | Dataplane state |
| `capabilities` | `*map[string]any` | Capability flags |
| `extraContext` | `*map[string]any` | Custom template variables |
| `http` | `*HTTPFetcher` | HTTP store for fetching remote content |
| `runtimeEnvironment` | `*RuntimeEnvironment` | Runtime environment info |

### Typed Watched Resources

**Typed access is the preferred idiom for user templates and documentation examples** — a live cluster always serves schemas (the controller fetches them from the apiserver), so `svc.metadata.name` / `ingress.spec.rules` just work and read far better than `dig(...)`. `dig()` is the fallback for the two places a schema is absent or the shape is deliberately open: **chart libraries** (RULE #1 — resource-agnostic code that must not assume any resource's shape) and polymorphic `any` boundaries. Don't let the chart's necessary `dig()` usage become the default reflex for user-facing templates and examples. See root `CLAUDE.md` → "Interactive playground examples" for the doc-authoring rule.

In addition to `resources` (the untyped store map above), the engine declares **one typed top-level global per watched resource** when a schema is loaded. The declaration is built by `pkg/k8s/typegen` from each resource's OpenAPI v3 schema and exposed as `*[]*resources.<name>.T` — a nil pointer whose static type carries the generated struct.

The store-wrapper return types are also typed when a schema is loaded: `resources.<name>.List()` returns `[]*resources.<name>.T`, `resources.<name>.Fetch(...)` returns `[]*resources.<name>.T`, and `resources.<name>.GetSingle(...)` returns `*resources.<name>.T` (nil if not found). Both surfaces share the same typed pointer — the top-level global and the store wrapper are interchangeable at the call site. Without a schema (e.g. `controller validate` without `--schema-dir`), both fall back to `[]any` / `map[string]any`.

The `resources.<name>.T` form is a selector-chain-as-type expression: it resolves to the generated struct's static type wherever a type expression is expected (macro parameters, var declarations, type assertions, slice types, and type-switch case clauses). The case-clause form is the canonical pattern for chart code that dispatches across multiple Kinds at a polymorphic `any` boundary. See `docs/adr/0010-typed-watched-resources.md` for the design rationale and `docs/site/docs/templating.md#typed-resource-access` for the chart-author-facing reference.

#### Filter behaviour against typed shapes

Three filter / function behaviours are load-bearing for the chart's mixed-shape adoption pattern:

- **`dig()` navigates typed structs by JSON tag.** `digMapFast` falls through to `digReflect` when an intermediate value is a typed struct (root cause of an earlier "empty backend name" bug — fast-path returning nil instead of recursing into the struct). `digReflect` also has a generic string-keyed-map fallback for `map[string][]any`, `map[string]int`, and similar shapes. Optional fields (`json:"…,omitempty"`) with zero values normalise to nil so `dig(...) | fallback(default)` behaves identically across typed and untyped shapes.
- **`to_str_map(value)`** normalises any string-keyed map (`map[string]string` from typegen-produced label / matchLabels / annotation fields, `map[string]any` from the untyped store path, or a generic `map[string]<T>`) into `map[string]string`. Non-string values from a `map[string]any` input are coerced via `tostring()`. Use this instead of `.(map[string]any)` assertions on label-shaped fields — those panic against the typed `map[string]string`.
- **`shard_slice(items, idx, n)` is type-preserving.** Declared as a `native.AdaptiveFunc`, the static return type at each call site equals the input slice's static type. `shard_slice([]*resources.gateways.T, i, n)` returns `[]*resources.gateways.T` (not `[]any`), so chart code that shards typed slices for parallel rendering keeps typed access through the shard call.

#### Scriggo fork extensions backing this

The vendored Scriggo fork carries three language-level extensions that the typed surface relies on:

- **Selector-chain-as-type** (`x.T` resolves to its value's static type in type-expression position; MR !91 in the scriggo fork). Multi-level chains supported, so `resources.gateways.T` works.
- **Type-switch case-clause selector chain** (`case *resources.<X>.T`; MR !96). Lets a single `switch r := v.(type)` dispatch across multiple typegen-built types, with `r` statically typed inside each case branch.
- **AdaptiveFunc native declarations.** Per-call-site return type from a closure on the argument types — used by `shard_slice`, `sort_by`, and the whole collection-pipeline family.

If a future template needs a new type-preserving filter, follow `scriggoShardSliceAdaptive`'s shape in `pkg/templating/filters_collection.go`, or the pipeline helpers below.

### Collection pipelines (ADR-0018)

`filters_pipeline.go` holds `filter`, `reject`, `flat_map`, `unique`,
`unique_by`, `group_by` and `sort_by`. All are `AdaptiveFunc`s so a chain over a
typed watched resource keeps its element type at every stage rather than
degrading to `[]any`-with-`dig()`.

Predicates and key functions are **closures, not JSONPath strings**. That is the
whole point: field access inside them is checked at engine compile time, where
the string form fails silently — `selectattr(eps, "targetRef.name", "ne", "")`
matches nothing today because the dotted path reaches `dig` as one key. Trading
that check away for idiom-consistency is a RULE #2 violation.

The idiomatic spelling is the arrow, `reject(e => e.TargetRef.Name == "")`.
Each helper opts in with `LambdaParams: elementLambdaParams`, which types the
lambda from the input slice's element type; a new helper taking an element
closure needs that line or its arrow form is rejected as uninferable. A
parameter with a *declared* function type needs nothing — the fork reads the
types from the signature.

Return-type rules, all computable from argument types alone:

| Helper | Return type |
|---|---|
| `filter` / `reject` / `unique` / `unique_by` / `sort_by` | `argTypes[0]` |
| `flat_map` | the closure's `Out(0)` (already a slice) |
| `group_by` | `map[string][]T` |

Four things to know before touching this file:

- **`unique_by` and `group_by` supersede `scriggo/builtin`'s versions** of the same
  names, which the chart already calls (`group_by(allRoutes, "pathKey")`). They
  accept a key closure *or* an attribute path, and preserve the element type
  where the builtins widen to `[]any`. `registerScriggoBuiltins` must not
  re-register them — it runs last and would silently shadow them.
- **`sort_by`'s debug override in `engine_scriggo.go` must stay an
  `AdaptiveFunc`.** A plain func there shadows the declaration and drops both the
  comparator call shape and type preservation, with no error.
- **A non-slice or nil input passes through as empty, never fails.** Chart code
  reaches optional typed fields that are nil; a guard at every stage would defeat
  the point.
- **`map` is only declarable because of a fork parser change.** `map` followed
  by anything other than `[` used to be an unconditional syntax error; the
  parser now resolves it as an identifier there (scriggo !115). Nothing else in
  the engine may assume `map` is a keyword.

### PathResolver and HAProxy Paths

`PathResolver` returns relative paths (e.g. `maps/host.map`, `general/400.http`) so the same rendered config works in production *and* in the validation sandbox without text rewrites of every file reference. Templates that hand-roll absolute paths sidestep this — the resolver also exposes `BaseDir` if you need to construct one yourself.

**CRITICAL**: For the relative paths to resolve at runtime, the rendered HAProxy
config must contain a `default-path origin <baseDir>` directive in the global
section. The chart's `base` library renders one (see
`charts/haptic/charts/base/library.yaml`'s `global-settings-300-paths` snippet) and
`pkg/controller/validation/service.go` rewrites the `<baseDir>` to a per-call
temp directory at validation time:

```haproxy
global
    default-path origin {{ pathResolver.GetBaseDir() }}   # → /etc/haproxy in production
```

Without the directive, HAProxy resolves the relative paths from its own
working directory (usually `/`) and the file lookups fail.

**Path resolution modes:**

| Mode | `PathResolver` config | Template output | When used |
|------|-----------------------|-----------------|-----------|
| Relative | `BaseDir: "/etc/haproxy"`, `MapsDir: "maps"`, … | `maps/host.map` | Production + validation; `default-path origin <baseDir>` resolves them |
| Absolute | `MapsDir: "/etc/haproxy/maps"`, … | `/etc/haproxy/maps/host.map` | Legacy / debugging only — breaks the validation rewrite trick |

**Template usage:**

```scriggo
{#- Using PathResolver.GetPath() #}
errorfile 400 {{ pathResolver.GetPath("400.http", "file") }}
{#- Output (chart default): general/400.http #}

use_backend %[path,map({{ pathResolver.GetPath("path.map", "map") }})]
{#- Output: maps/path.map #}
```

See `charts/CLAUDE.md` (HAProxy File Path Requirements) for the chart-side
view, including the validation `strings.Replace` contract that depends on
this directive being present and matching `pathResolver.GetBaseDir()`.

### Adding New Runtime Variables

1. Declare with nil pointer in `buildScriggoGlobals()`:

   ```go
   decl["myVar"] = (*MyType)(nil)
   ```

2. Pass value at render time via context:

   ```go
   context["myVar"] = actualValue
   ```

### Calling Methods on Runtime Variables

**Methods work on runtime variables.** When a type is declared with a nil pointer, Scriggo knows the type's methods at compile time. At runtime, when the actual value is provided, those methods can be called:

```go
// In filters_scriggo.go - declare type (nil pointer)
decl["pathResolver"] = (*PathResolver)(nil)

// In templates - call methods directly
{{ pathResolver.GetPath("host.map", "map") }}
```

This works because:

1. Scriggo sees `*PathResolver` type at compile time
2. Scriggo validates that `GetPath` method exists on `*PathResolver`
3. At runtime, the actual `*PathResolver` value is provided via context
4. Method call executes on the runtime value

**No wrapper functions needed.** You can call methods directly on runtime variables. Wrapper functions are only needed when:

- You need access to `native.Env` for Scriggo-specific context
- The method signature doesn't fit Scriggo's calling conventions
- You want to provide a simpler API

**Example - direct method call vs wrapper:**

```go
// Direct method call - preferred, simpler
{{ pathResolver.GetPath("host.map", "map") }}

// Both produce identical results - use the direct method call
```

## Testing Approach

### Test Template Logic, Not Engine Syntax

Focus on testing the library API and error handling:

```go
func TestEngine_Render(t *testing.T) {
    tests := []struct {
        name     string
        template string
        context  map[string]any
        want     string
        wantErr  bool
    }{
        {
            name:     "simple variable substitution",
            template: "Hello {{ name }}",
            context:  map[string]any{"name": "World"},
            want:     "Hello World",
        },
        {
            name:     "missing variable with default",
            template: "Hello {{ name default \"Guest\" }}",
            context:  map[string]any{},
            want:     "Hello Guest",
        },
        {
            name:     "complex context",
            template: "{% for _, item := range items %}{{ item }}{% end %}",
            context:  map[string]any{"items": []string{"a", "b"}},
            want:     "ab",
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            engine, err := templating.New(
                map[string]string{"test": tt.template}, nil)

            if tt.wantErr {
                require.Error(t, err)
                return
            }

            require.NoError(t, err)

            got, err := engine.Render(context.Background(), "test", tt.context)
            require.NoError(t, err)
            assert.Equal(t, tt.want, got)
        })
    }
}
```

### Test Error Handling

```go
func TestEngine_CompilationError(t *testing.T) {
    // Invalid template syntax
    templates := map[string]string{
        "invalid": "{% if true %}\n{% end extra text %}",
    }

    _, err := templating.New(templates, nil)

    require.Error(t, err)

    var compErr *templating.CompilationError
    require.True(t, errors.As(err, &compErr))
    assert.Equal(t, "invalid", compErr.TemplateName)
    assert.NotEmpty(t, compErr.TemplateSnippet)
}

func TestEngine_TemplateNotFound(t *testing.T) {
    engine, _ := templating.New(map[string]string{
        "exists": "content",
    }, nil)

    _, err := engine.Render(context.Background(), "missing", nil)

    require.Error(t, err)

    var notFoundErr *templating.TemplateNotFoundError
    require.True(t, errors.As(err, &notFoundErr))
    assert.Equal(t, "missing", notFoundErr.TemplateName)
    assert.Contains(t, notFoundErr.AvailableTemplates, "exists")
}
```

## Common Pitfalls

### Recreating Engine on Every Render

**Problem**: Recompiling templates on every render.

```go
// Bad - compiles templates every time (milliseconds)
for _, context := range contexts {
    engine, _ := templating.New(templates, nil)
    output, _ := engine.Render(ctx, "template", context)
}
```

**Solution**: Create once, reuse for all renders.

```go
// Good - compile once, render many times (microseconds)
engine, err := templating.New(templates, nil)
if err != nil {
    log.Fatal(err)
}

for _, context := range contexts {
    output, _ := engine.Render(ctx, "template", context)
}
```

### Not Checking Compilation Errors

**Problem**: Syntax errors discovered at runtime in production.

```go
// Bad - ignoring compilation errors
engine, _ := templating.New(templates, nil)

// Later in production...
output, err := engine.Render(ctx, "template", context)
// err: template doesn't exist (because compilation failed)
```

**Solution**: Always check compilation errors at startup.

```go
// Good - fail fast on invalid templates
engine, err := templating.New(templates, nil)
if err != nil {
    var compErr *templating.CompilationError
    if errors.As(err, &compErr) {
        log.Fatal("template compilation failed",
            "template", compErr.TemplateName,
            "error", compErr.Cause)
    }
    log.Fatal(err)
}
```

### Large Template Snippets in Errors

**Problem**: Logging 10KB template in error message.

```go
// Bad - logs entire template
log.Error("compilation failed", "template", templateContent)
```

**Solution**: CompilationError already includes snippet (first 200 chars).

```go
// Good - error contains context snippet
if err != nil {
    var compErr *templating.CompilationError
    if errors.As(err, &compErr) {
        log.Error("compilation failed",
            "template", compErr.TemplateName,
            "snippet", compErr.TemplateSnippet,  // First 200 chars
            "error", compErr.Cause)
    }
}
```

### Ignoring Thread Safety

**Problem**: Assuming Engine is not thread-safe.

```go
// Unnecessary - Engine is already thread-safe
var mu sync.Mutex

func render(engine templating.Engine, tplCtx map[string]any) string {
    mu.Lock()
    defer mu.Unlock()
    output, _ := engine.Render(context.Background(), "template", tplCtx)
    return output
}
```

**Solution**: Use Engine concurrently without locking.

```go
// Good - no lock needed
func render(engine templating.Engine, tplCtx map[string]any) string {
    output, _ := engine.Render(context.Background(), "template", tplCtx)
    return output
}

// Safe to call from multiple goroutines
var wg sync.WaitGroup
for i := 0; i < 100; i++ {
    wg.Add(1)
    go func() {
        defer wg.Done()
        render(engine, tplCtx)
    }()
}
wg.Wait()
```

## Template Tracing

The template engine provides execution tracing for observability and performance debugging.

### Enabling Tracing

```go
engine, _ := templating.New(templates, nil)

// Enable tracing
engine.EnableTracing()

// Render templates (tracing happens automatically)
output, _ := engine.Render(ctx, "haproxy.cfg", context)

// Get trace output
trace := engine.GetTraceOutput()
fmt.Println(trace)

// Disable tracing
engine.DisableTracing()
```

### Trace Output

Tracing logs each template render with name and duration:

```
Rendering: haproxy.cfg
Completed: haproxy.cfg (0.007ms)
Rendering: backends.cfg
  Rendering: backend-snippet
  Completed: backend-snippet (0.003ms)
Completed: backends.cfg (0.012ms)
```

**Output format:**

- Indentation shows nesting depth (includes)
- Duration in milliseconds with 3 decimal places
- Start/complete pairs for each template

### Use Cases

**1. Performance debugging:**

```go
engine.EnableTracing()

// Render all templates
for name := range templates {
    engine.Render(ctx, name, context)
}

// Parse trace to find slow templates
trace := engine.GetTraceOutput()
for _, line := range strings.Split(trace, "\n") {
    if strings.Contains(line, "Completed") {
        // Extract duration and identify >10ms templates
    }
}
```

**2. Template execution verification:**

```go
// Ensure expected templates are executed
engine.EnableTracing()
engine.Render(ctx, "haproxy.cfg", context)
trace := engine.GetTraceOutput()

if !strings.Contains(trace, "Rendering: backends.cfg") {
    log.Warn("backends.cfg was not included")
}
```

**3. Integration with validation tests:**

```go
// In haptic validate command
engine.EnableTracing()

runner := testrunner.New(config, engine, paths, options)
results, _ := runner.RunTests(ctx, "")

// Show trace after results
trace := engine.GetTraceOutput()
fmt.Println("\n=== Template Execution Trace ===")
fmt.Println(trace)
```

### Implementation Details

**Tracing configuration:**

```go
type scriggoTracingConfig struct {
    enabled      bool           // Tracing on/off (protected by mu)
    debugFilters bool           // Reserved for future filter debug logging
    mu           sync.RWMutex   // Protects enabled flag and traces slice
    traces       []string       // Accumulated trace outputs from all renders
}
```

**Thread-safety approach:**

- Per-render state (depth, builder) stored in execution context (isolated per `Render()` call)
- Shared state (enabled flag, traces slice) protected by mutex
- Tracing enabled/disabled flag snapshot taken at render start to avoid repeated locking
- Completed traces appended to shared slice under mutex lock

**How tracing actually works** (see `engine_scriggo.go:259-322` and `engine_scriggo_tracing.go:117-164`):

1. `Render` takes a one-time snapshot of `e.tracing.enabled`. If false, no tracing work happens at all.
2. When enabled, `Render` allocates a per-call `*strings.Builder` and writes `Rendering: <name>\n` to it before executing the template.
3. If the engine has profiling on (also turns on automatically with tracing), Scriggo populates a `*scriggo.Profile` that includes the call tree — every `render "..."` macro call, with its duration.
4. After the template runs, `storeTraceOutput` walks the call tree (`writeCallTreeTrace`), emitting indented `Rendering: <child>` / `Completed: <child> (X.XXXms)` pairs for every nested macro call. Without a profile, it falls back to a flat trace with just the top-level template's start/end.
5. The completed string is appended to `e.tracing.traces` under a mutex; `GetTraceOutput` joins and clears the slice.

**Key design decisions:**

- Per-render isolation: each `Render()` call gets its own builder, so concurrent renders never share mutable trace state.
- Mutex protects only the small enabled-flag snapshot and the final append/clear — no contention during template execution.
- Profile-driven nesting: indent depth and child names come from Scriggo's call tree, not from anything in the template context. There is no `_trace_depth` / `_trace_builder` context variable.
- Tracing only captures **template/macro** boundaries. Filter calls are not in the trace; for filter-level debugging use `EnableFilterDebug()` (slog output, separate channel).

### Performance Overhead

Tracing overhead is minimal:

- Simple render: ~0.001-0.002ms overhead per template
- Complex render: <1% overhead
- String builder prevents repeated allocations
- Two mutex operations per render when enabled (snapshot flag, append trace)
- No contention during rendering (trace building happens in context-local storage)

**Recommendation**: Safe to leave enabled for debugging, but disable in performance-critical production paths.

### What Tracing Does *Not* Capture

`EnableTracing()` only emits `Rendering:` / `Completed:` lines for **template / macro boundaries** (anything Scriggo records as `CallKindMacro`). It does **not** record:

- Filter calls (`sort_by`, `glob_match`, `debug`, …) — use `EnableFilterDebug()` (slog) instead
- Variable lookups, control-flow nodes, individual expressions
- Per-node Go-level allocations or CPU samples

If you need filter-level visibility, combine the two: tracing for which templates ran, filter debug for what `sort_by` did inside them.

### Limitations

**Current limitations:**

1. No filtering by template name
2. Fixed output format (not configurable)
3. No automatic slow template warnings
4. Trace cleared on each `GetTraceOutput()` call

**Workarounds:**

- Filter trace output in calling code
- Parse trace to generate custom reports
- Set thresholds in analysis code
- Copy trace before clearing if needed

### Testing Tracing

**Basic tracing test:**

```go
func TestEngine_Tracing(t *testing.T) {
    templates := map[string]string{
        "main": "{{ render \"sub\" }}",
        "sub":  "content",
    }

    engine, _ := templating.New(templates, nil)
    engine.EnableTracing()

    _, err := engine.Render(context.Background(), "main", nil)
    require.NoError(t, err)

    trace := engine.GetTraceOutput()

    // Verify trace contains expected entries
    assert.Contains(t, trace, "Rendering: main")
    assert.Contains(t, trace, "Rendering: sub")
    assert.Contains(t, trace, "Completed: main")
    assert.Contains(t, trace, "Completed: sub")

    // Verify nesting (sub should be indented)
    assert.Contains(t, trace, "  Rendering: sub")
}
```

**Concurrent tracing test (race detector validation):**

```go
func TestTracing_ConcurrentRenders(t *testing.T) {
    templates := map[string]string{
        "template1": `Result: {{ value }}`,
        "template2": `Output: {{ toUpper(value) }}`,
    }

    engine, _ := templating.New(templates, nil)
    engine.EnableTracing()

    // Run concurrent renders
    const numGoroutines = 10
    done := make(chan bool, numGoroutines)

    for i := 0; i < numGoroutines; i++ {
        go func(id int) {
            defer func() { done <- true }()

            for j := 0; j < 5; j++ {
                tmpl := fmt.Sprintf("template%d", (j%2)+1)
                output, err := engine.Render(context.Background(), tmpl, map[string]any{
                    "value": fmt.Sprintf("goroutine-%d", id),
                })
                assert.NoError(t, err)
                assert.NotEmpty(t, output)
            }
        }(i)
    }

    // Wait for all goroutines to complete
    for i := 0; i < numGoroutines; i++ {
        <-done
    }

    // Verify trace contains entries from all renders
    trace := engine.GetTraceOutput()
    assert.NotEmpty(t, trace)
    assert.Contains(t, trace, "Rendering: template1")
    assert.Contains(t, trace, "Rendering: template2")

    // Run with race detector: go test -race
}
```

## Filter Debug Logging

The template engine provides detailed debug logging for filter operations, particularly useful for debugging sort_by comparisons.

### Enabling Filter Debug

```go
engine, _ := templating.New(templates, nil)

// Enable filter debug logging
engine.EnableFilterDebug()

// Render templates (debug logs appear in slog output)
output, _ := engine.Render(ctx, "test", context)

// Disable filter debug logging
engine.DisableFilterDebug()
```

### Debug Output

When enabled, sort_by filter logs each comparison with structured logging:

```
INFO SORT comparison criterion=$.priority:desc valA=10 valA_type=int valB=5 valB_type=int result=1
INFO SORT comparison criterion=$.priority:desc valA=5 valA_type=int valB=1 valB_type=int result=1
```

**Logged fields:**

- `criterion`: The sort expression being evaluated
- `valA`: Value extracted from first item
- `valA_type`: Go type of first value
- `valB`: Value extracted from second item
- `valB_type`: Go type of second value
- `result`: Comparison result (-1, 0, 1)

### CLI Integration

The `haptic validate` command supports the `--debug-filters` flag:

```bash
# Enable filter debug logging during validation tests
./bin/haptic validate --test test-gateway-route-precedence --debug-filters

# Combine with other debugging flags
./bin/haptic validate --debug-filters --trace-templates --dump-rendered
```

**How it works:**

- Flag passed from CLI → testrunner.Options → testrunner.Runner
- Runner creates worker engines with `EnableFilterDebug()` called
- Each worker engine independently logs filter operations
- Useful for understanding why routes are sorted in a particular order

### Use Cases

**1. Debugging sort_by precedence:**

```go
// Template
routes | sort_by(["$.match.method:exists:desc", "$.match.headers | length:desc"])

// Enable debug to see:
// - Which routes have method field (exists check)
// - Header count for each route
// - Comparison order and results
```

**2. Understanding mixed-type sorting:**

```go
// When sorting items with different value types
items | sort_by(["$.value"])

// Debug shows actual types being compared:
// valA=string valA_type=string
// valB=123 valB_type=int
```

**3. Verifying expression evaluation:**

```go
// Complex expressions with multiple criteria
items | sort_by(["$.type", "$.priority:desc", "$.name"])

// Debug shows evaluation of each criterion
```

### Implementation Notes

- Debug logging uses Go's `log/slog` package
- Logging only happens when `EnableFilterDebug()` is active
- Thread-safe: Multiple concurrent renders each log independently
- Zero overhead when disabled (simple boolean check)
- Only sort_by filter currently logs (most common debugging need)

### Performance Impact

Filter debug logging is lightweight:

- Single boolean check per comparison
- Structured logging (slog) is efficient
- No string formatting when disabled
- Typical overhead: <5% for sort-heavy templates

**Recommendation**: Safe to enable during development and validation testing. Disable in production unless investigating specific issues.

## Debug Filters

The template engine provides the `debug` filter for debugging templates by inspecting variable contents.

### debug Filter

Dumps variable structure as JSON-formatted HAProxy comments:

```scriggo
{%- var routes = []any{
    map[string]any{"name": "api", "priority": 10},
    map[string]any{"name": "web", "priority": 5},
} %}

{{- routes | debug }}

{# Output:
# DEBUG:
# [
#   {
#     "name": "api",
#     "priority": 10
#   },
#   {
#     "name": "web",
#     "priority": 5
#   }
# ]
#}
```

**With label:**

```scriggo
{{ routes | debug("sorted-routes") }}

{# Output:
# DEBUG sorted-routes:
# [...]
#}
```

**Use cases:**

- Inspecting variable structure during template development
- Verifying filter transformations (before/after)
- Understanding data passed to templates
- Safe output (formatted as comments, won't break HAProxy config)

**Features:**

- JSON formatted with indentation
- Prefixed with `#` (HAProxy comment)
- Optional label for identifying debug output
- Works with any data type (objects, arrays, strings, numbers)

### Example Usage

```scriggo
{%- var items = []any{ /* … populate items … */ } %}

{# Before sorting #}
{{ items | debug("unsorted") }}

{# After sorting #}
{%- var sorted = items | sort_by([]string{"$.priority:desc"}) %}
{{ sorted | debug("sorted") }}
```

### Implementation Notes

- Uses `json.MarshalIndent()` for formatting
- Adds `#` prefix to every line
- Falls back to `fmt.Sprintf("%v")` if JSON marshaling fails
- Returns string (can be used in {{ }} expressions)

### Performance Impact

The debug filter is intended for debugging only:

- JSON marshaling overhead (skip in production templates)
- No impact on other filters or rendering when not used
- Safe to leave in templates during development (just comment out)

**Recommendation**: Use during template development and testing. Remove or comment out before production deployment.

### Testing Debug Filter

```go
func TestDebugFilter(t *testing.T) {
    templates := map[string]string{
        "test": `{{ debug(item, "test-item") }}`,
    }

    engine, _ := templating.New(templates, nil)
    output, _ := engine.Render(context.Background(), "test", map[string]any{
        "item": map[string]any{"key": "value"},
    })

    assert.Contains(t, output, "# DEBUG test-item:")
    assert.Contains(t, output, `"key": "value"`)
}
```

## Caching Expensive Computations

Use `shared.ComputeIfAbsent()` for compute-once patterns:

```go
{%- var analysis, _ = shared.ComputeIfAbsent("analysis", func() any {
    return analyzeRoutes(resources)
}) -%}
```

`ComputeIfAbsent` is thread-safe and guarantees exactly-once computation. Use `shared.Get(key)` for read-only access to previously computed values.

**Impact:** Reduces expensive computation calls from N to 1 per render.

## Performance Optimization

Keep the regex compilation cache engine-scoped and bounded; package-global state would outlive a config iteration and let rejected candidate engines displace active patterns.

### Benchmarking

```go
func BenchmarkEngine_Render(b *testing.B) {
    templates := map[string]string{
        "simple": "Hello {{ name }}!",
        "loop": `{% for _, i := range items %}{{ i }}{% end %}`,
        "complex": `
            {% for _, user := range users %}
                Name: {{ user.name }}
                Email: {{ toLower(user.email) }}
                {% if user.admin %}Admin{% end %}
            {% end %}
        `,
    }

    engine, _ := templating.New(templates, nil)

    contexts := map[string]map[string]any{
        "simple": {"name": "World"},
        "loop": {"items": []int{1, 2, 3, 4, 5}},
        "complex": {
            "users": []map[string]any{
                {"name": "Alice", "email": "ALICE@EXAMPLE.COM", "admin": true},
                {"name": "Bob", "email": "BOB@EXAMPLE.COM", "admin": false},
            },
        },
    }

    for name, ctx := range contexts {
        b.Run(name, func(b *testing.B) {
            b.ResetTimer()
            for i := 0; i < b.N; i++ {
                engine.Render(ctx, name, ctx)
            }
        })
    }
}
```

**Expected results:**

- Simple: ~20-50µs per render
- Loop: ~50-100µs per render
- Complex: ~100-200µs per render

### Memory Optimization

```go
// Bad - allocates new map for each render
for i := 0; i < 1000; i++ {
    ctx := map[string]any{
        "name": users[i].Name,
    }
    engine.Render(ctx, "template", ctx)
}

// Good - reuse context map
ctx := make(map[string]any)
for i := 0; i < 1000; i++ {
    ctx["name"] = users[i].Name
    engine.Render(ctx, "template", ctx)
}
```

### Template Size

```go
// Bad - one huge template (slow compilation, poor error messages)
templates := map[string]string{
    "haproxy.cfg": `
        global
            daemon
        defaults
            mode http
        {{ 5000 lines of template }}
    `,
}

// Good - break into logical pieces
templates := map[string]string{
    "haproxy.cfg": `
        {{ render "global" }}
        {{ render "defaults" }}
        {{ render "frontends" }}
        {{ render "backends" }}
    `,
    "global": "global\n    daemon",
    "defaults": "defaults\n    mode http",
    "frontends": "...",
    "backends": "...",
}
```

## Troubleshooting

### Template Not Found

**Diagnosis:**

1. Check template name exactly matches
2. List available templates
3. Verify templates were provided at initialization

```go
if !engine.HasTemplate(templateName) {
    available := engine.TemplateNames()
    log.Error("template not found",
        "requested", templateName,
        "available", available)
}
```

### Rendering Produces Empty Output

**Diagnosis:**

1. Check for silent errors in template (use `{% if var != nil %}`)
2. Verify context contains expected data
3. Check for whitespace stripping (`{%-` and `-%}`)

```go
// Debug context
log.Info("rendering template",
    "template", templateName,
    "context_keys", getKeys(context))

output, err := engine.Render(ctx, templateName, context)

log.Info("render result",
    "output_length", len(output),
    "output_preview", output[:min(100, len(output))])
```

### High Memory Usage

**Diagnosis:**

1. Check template count and size
2. Review context object sizes
3. Look for memory leaks in custom filters

```go
// Monitor engine memory
var m runtime.MemStats
runtime.ReadMemStats(&m)

log.Info("template engine memory",
    "template_count", engine.TemplateCount(),
    "alloc_mb", m.Alloc/1024/1024,
    "total_alloc_mb", m.TotalAlloc/1024/1024)
```

### Slow Rendering

**Diagnosis:**

1. Benchmark specific template
2. Check for expensive operations in context preparation
3. Review loop complexity in templates

```go
// Profile template rendering
start := time.Now()
output, err := engine.Render(ctx, templateName, context)
duration := time.Since(start)

if duration > 100*time.Millisecond {
    log.Warn("slow template render",
        "template", templateName,
        "duration_ms", duration.Milliseconds(),
        "output_size", len(output))
}
```

## Extension Considerations

### Scriggo Fork

This project uses a forked version of Scriggo. The fork adds:

1. **Native `{% include %}` statement**: Syntax for static includes:

   ```go
   {% include "partial.txt" %}  {# Compile-time include #}
   ```

2. **Bug fixes and compatibility improvements** for use with this template engine.

The fork is consumed directly as a normal `require` in `go.mod` — no `replace` directive is involved. The pinned pseudo-version drifts as Renovate updates the dep, so check `go.mod` rather than copying a version from this file:

```sh
grep github.com/haproxy-haptic/scriggo go.mod
```

### Include: Static and Glob

The Scriggo engine provides two ways to include templates:

| Pattern | Syntax | Evaluation | Use Case |
|---------|--------|------------|----------|
| Static render | `{{ render "literal/path" }}` | Compile-time expansion + runtime render | Fixed template paths |
| Glob render | `{{ render_glob "pattern-*" }}` | Compile-time expansion + runtime render | Pattern-matched templates |

**Both `render` and `render_glob` require a string literal** — the path / pattern is expanded against the template filesystem at compile time so the engine can validate paths exist, expand globs into their matching templates, and emit each as a parallelisable render. Variables / computed expressions are not accepted; the parser will reject them with `unexpected X, expecting string`.

**Static renders** (`render` with string literal):

```go
{{ render "header" }}      {# Valid - literal path #}
{{ render "partials/footer" }} {# Valid - literal path #}
```

If you need to dispatch on a runtime value, use an explicit `if` ladder with literal `render` calls:

```go
{% if name == "check-auth" %}
  {{ render "spoe-message-check-auth-body" }}
{% else if name == "fingerprint" %}
  {{ render "spoe-message-fingerprint-body" }}
{% end %}
```

This keeps compile-time path validation; the trade-off is that the dispatch is not data-driven (every new branch requires a code change).

**Glob renders** (`render_glob` function):

- Pattern-based template inclusion
- Matches and renders all templates matching the glob pattern

```go
{{ render_glob "backend-*" }}  {# Renders all backend-* templates #}
```

`render_glob` concatenates the outputs verbatim. `on_own_lines(render_glob "p")`
is the line-safe form: `distributeOnOwnLines` (`filters_lines.go`, the
`ExpandedTransformer` of every chart build path) rewrites it into a balanced tree
of pairwise joins over the matched renders, because a function only ever sees the
already-joined string. `on_own_lines` itself is not declared, so a new
`scriggo.BuildTemplate` call that compiles chart templates without the
transformer fails with `undefined: on_own_lines` rather than joining verbatim.

### Scriggo Template Syntax

Scriggo uses Go template syntax:

- Loops: `{% for x := range items %}...{% end %}`
- Conditionals: `{% if cond %}...{% end %}`
- Variables: `{{ name }}` — bare identifier, the way every chart library uses them. Scriggo also accepts `{{ .name }}` with a leading dot (Go template style); both compile, but production templates stick with the bare form, so prefer it for consistency.
- **Imports**: `{% import "template" for FuncName %}`

**Pipe operator**: Our Scriggo fork supports pipe syntax for filters. The pipe operator passes the left-hand expression as the first argument to the function on the right:

   ```go
   {{ user.name | uppercase() }}        {# Same as: uppercase(user.name) #}
   {{ items | join(", ") }}             {# Same as: join(items, ", ") #}
   {{ value | fallback("N/A") }}        {# Same as: fallback(value, "N/A") #}
   {{ price | format("%.2f") | prefix("$") }}  {# Chaining supported #}
   ```

   Note: The fallback function is named `fallback` (not `default`) because `default` is a reserved Go keyword.

Both function-call and pipe syntax work:

- Pipe syntax: `{{ value | strip() }}`
- Function syntax: `{{ strip(value) }}`

### Filter and Function Name Constants

Filter and function names are defined as constants in `filter_names.go`:

```go
// Filters
const (
    FilterSortBy    = "sort_by"
    FilterGlobMatch = "glob_match"
    FilterStrip     = "strip"
    FilterB64Decode = "b64decode"
    FilterDebug     = "debug"
    FilterIndent    = "indent"
)

// Functions (subset - see filter_names.go for full list)
const (
    FuncFail          = "fail"
    FuncMerge         = "merge"
    FuncKeys          = "keys"
    FuncCoalesce      = "coalesce"
    FuncFallback      = "fallback"
    FuncDig           = "dig"
    FuncToSlice       = "toSlice"
    FuncSanitizeRegex = "sanitize_regex"
    // ... 30+ more functions
)
```

### Scriggo Functions

Scriggo provides functions designed to support idiomatic Go patterns:

#### Dict Operations

**`merge(dict, updates)`** - Merges two maps, returning a new map:

```go
{% var config = map[string]any{"a": 1, "b": 2} %}
{% config = merge(config, map[string]any{"b": 3, "c": 4}) %}
{# Result: {"a": 1, "b": 3, "c": 4} #}
```

**`keys(dict)`** - Returns sorted keys from a map:

```go
{% var config = map[string]any{"c": 3, "a": 1, "b": 2} %}
{% for _, key := range keys(config) %}
{{ key }}: {{ config[key] }}
{% end %}
{# Output: a: 1, b: 2, c: 3 (sorted) #}
```

#### Caching with SharedContext

Use `shared.ComputeIfAbsent()` for compute-once patterns in templates:

```go
{%- var analysis, _ = shared.ComputeIfAbsent("analysis", func() any {
    return analyzeRoutes(resources)
}) -%}
```

`ComputeIfAbsent` is thread-safe and guarantees exactly-once computation. Use `shared.Get(key)` for read-only access to previously computed values.

**Cache Characteristics:**

- Cache is per-render (isolated between `Render()` calls)
- Supports any value type (maps, slices, structs)
- Thread-safe for concurrent template sections

### Type Assertions

In Scriggo templates, type assertions are needed when working with `any` (`interface{}`) values in contexts that require concrete types. The codebase uses the `any` alias throughout — match that style in new docs and templates.

**When type assertions ARE required:**

1. **Ranging over `coalesce()` results** - `coalesce()` returns `any`:

   ```go
   {%- for _, item := range coalesce(map["key"], []any{}).([]any) -%}
   ```

2. **Accessing fields on map values** - map indexing returns `any`:

   ```go
   {%- var data = secret.(map[string]any)["data"].(map[string]any) -%}
   ```

3. **String operations on interface values**:

   ```go
   {%- var name = item["name"].(string) + "_suffix" -%}
   ```

**When type assertions are NOT required:**

1. **Using typed ResourceStore methods** - methods like `Fetch()` return concrete types:

   ```go
   {%- for _, ep := range resources.endpoints.Fetch(svcName) -%}
   ```

   `Fetch()` returns `[]any` (see `pkg/templating/types.go:51`), a concrete slice type — you can range it without an outer assertion. Individual elements still need assertion when you index into them.

2. **Simple variable access** - variables already have their type:

   ```go
   {{ httpsPort default 8443 }}
   ```

3. **Template output** - `{{ }}` handles any type:

   ```go
   {{ item["name"] }}  {# No assertion needed for output #}
   ```

**Rule of thumb:**

- Check function return types in `filters_scriggo.go`.
- If a function returns `any`, assertion is needed for operations.
- If a function returns a concrete type like `[]any` or `map[string]any`, no assertion is needed at the outer level — but elements / values still are.

### Engine Interface Requirements

Engines must:

- Implement the `Engine` interface in `engine_interface.go`
- Support pre-compilation (compile at init, not at render time)
- Be thread-safe for concurrent rendering
- Use filter name constants from `filter_names.go`
- Error types must use existing error types (`CompilationError`, `RenderError`, etc.)

## Resources

- API documentation: `pkg/templating/README.md`
- Scriggo documentation: <https://scriggo.com/templates>
- Scriggo fork: `github.com/haproxy-haptic/scriggo` (fork with `{% include %}` statement)
- HAProxy template examples: `/examples/`
