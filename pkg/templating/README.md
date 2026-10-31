# pkg/templating

Pure template rendering library. Wraps a fork of [Scriggo](https://scriggo.com/) with a pre-compile-then-render lifecycle, HAProxy-specific filters and context helpers, and structured errors. Zero dependencies on other `pkg/` packages — this is a reusable library.

Module path: `github.com/haproxy-haptic/HAPTIC`. The source is authoritative (`go doc ./pkg/templating`); this README is a short orientation. `docs/site/docs/templating.md` covers the template *author's* side (syntax, filters, custom variables) — this page is for Go callers.

## Minimal Usage

```go
import (
    "context"
    "log"

    "github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

templates := map[string]string{
    "greeting": "Hello {{ name }}!",
    "config":   "server {{ host }}:{{ port }}",
}

engine, err := templating.New(templates, nil)
if err != nil {
    log.Fatal(err)   // compilation errors surface here, fail fast
}

out, err := engine.Render(context.Background(), "greeting",
    map[string]any{"name": "World"})
```

The engine is safe for concurrent use — compile once at startup, render concurrently from many goroutines.

## `New` Signature

```go
func New(templates map[string]string, opts *Options) (*ScriggoEngine, error)

type Options struct {
    EntryPoints                   []string                         // template names compiled explicitly; nil = all
    IncrementalEntryPoints        []string                         // deterministic component entry points
    IncrementalBindingEntryPoints []string                         // deterministic binding-planner entry points
    Filters                       map[string]FilterFunc            // custom filters merged over the built-in set
    Functions                     map[string]GlobalFunc            // custom global functions merged over the built-in set
    PostProcessors                map[string][]PostProcessorConfig // per-template post-processing chains
    Declarations                  map[string]any                   // domain-specific Scriggo type declarations
    Profiling                     bool                             // enable Scriggo's built-in profiler
}
```

A nil `*Options` (or the zero value) compiles every template as an entry point with no custom filters, functions, post-processors, declarations, or profiling. Set `EntryPoints` to compile only some templates explicitly — the rest are snippets, discovered and compiled on demand via `render`/`render_glob` statements with `inherit_context`. `Filters` plug into the pipe syntax (`{{ value | myFilter }}`), `Functions` into the call syntax (`{{ myFunc(value) }}`), and `PostProcessors` chain per-template transformations (regex replace, or a Scriggo template whose `input` variable is the previously rendered output).

`IncrementalEntryPoints` must be a subset of `EntryPoints`. Those templates and their imports can access only tracked watched-resource, controller-resource, and HTTP inputs; `item`, immutable `props`, `renderSubject`, `shared`; and deterministic built-ins. Custom functions and ambient render globals aren't declared there.

`IncrementalBindingEntryPoints` must also be a disjoint subset of `EntryPoints`. A binding planner receives detached, immutable `extraContext`, `capabilities`, `currentConfig`, `currentFiles`, `pathResolver`, `runtimeEnvironment`, and `templateSnippets` values. It has no watched-resource, controller-resource, HTTP, admission, or shared-state globals and must emit one JSON object. The selected values become the component's canonical immutable `props`; changing an unselected ambient value doesn't execute the component. `RenderIncrementalBindings` returns that object in canonical JSON form.

## Engine Interface (Highlights)

```go
type Engine interface {
    Render(ctx context.Context, templateName string, templateContext map[string]any) (string, error)

    HasTemplate(name string) bool
    TemplateNames() []string
    TemplateCount() int
    GetRawTemplate(name string) (string, error)

    EnableTracing()
    DisableTracing()
    GetTraceOutput() string

    EnableFilterDebug()
    DisableFilterDebug()
}
```

`ctx` controls rendering timeouts (`RenderTimeoutError` is returned on cancellation). Tracing produces a nested indented trace of every `render` / `render_glob` call; filter debug logs `sort_by` comparisons via `log/slog` at INFO level. Both are off by default and have negligible overhead when disabled — they're wired up to the `--trace-templates` and `--debug-filters` flags on `haptic validate`.

## Error Types

```go
var compErr *templating.CompilationError     // syntax error; has TemplateName + first 200 chars via .TemplateSnippet
var renderErr *templating.RenderError        // runtime failure during Render
var timeoutErr *templating.RenderTimeoutError // ctx deadline exceeded
var notFoundErr *templating.TemplateNotFoundError // unknown name; has .AvailableTemplates
var failErr *templating.FailError            // a template called fail(); .Message is its text
```

Always check with `errors.As`; the wrapped `.Cause` carries the underlying Scriggo diagnostic. `RenderErrorMessage(err)` returns the `fail()` message alone, which is what an operator-facing surface shows.

## What Ships Inside

### Filters (pipe syntax, `{{ v | filter(args) }}`)

`b64decode`, `glob_match`, `indent`, `sort_by` (JSONPath criteria with `:desc`, `:exists`, `| length` modifiers, **or** a `func(a, b T) int` comparator), `debug`, `toJSON`, `strip`, `to_str_map` (normalises any string-keyed map — typed `map[string]string`, untyped `map[string]any`, or generic `map[string]<T>` — into `map[string]string` for uniform iteration over labels / matchLabels / annotations).

Collection pipeline (ADR-0018), all type-preserving: `map`, `filter`, `reject`, `flat_map`, `unique`, `unique_by`, `group_by`. Predicates and key functions are closures, so field access inside them is checked at engine compile time; `unique_by` and `group_by` also accept an attribute path for `any`-shaped data. A chain over a typed watched resource keeps typed field access at every stage:

```scriggo
{%%
  type EP = resources.endpoints.Endpoints
  var ready = resources.endpoints.List() |
    flat_map(func(s *resources.endpoints.T) []EP { return s.Endpoints }) |
    reject(func(e EP) bool { return e.TargetRef.Name == "" })
%%}
```

Trailing pipes only (Go's semicolon insertion), chains live in `{%% %%}` rather than `{{ }}`, and a multi-return function such as `sort_by` cannot end a pipe.

### Functions (call syntax, `{{ fn(args) }}`)

Selection: `fallback`, `coalesce`, `fail`, `merge`, `keys`, `sort_strings`, `sanitize_regex`, `semver_gte`, `toLower`, `tostring`, `dig` (navigates nested maps **and** typed structs via JSON-tag → Go-field lookup), `shard_slice` (type-preserving slice shard via a `native.AdaptiveFunc` — return type at each call site matches the input element type), plus Scriggo's standard library.

Canonical reference: `pkg/templating/filter_names.go`.

### Runtime Context Variables

Scriggo needs to know the *type* of each runtime variable at compile time even though values arrive at `Render`. The library declares these with nil-pointer typedefs in `buildScriggoGlobals` — the `(*T)(nil)` pattern — so callers just pass values in the render context:

| Variable | Type | Purpose |
|----------|------|---------|
| `resources` | `*map[string]ResourceStore` | Watched Kubernetes resources (`.List`, `.Fetch`, `.GetSingle`); when a schema is loaded for an entry, the wrapper returns typed pointers (`[]*resources.<name>.T` / `*resources.<name>.T`) |
| `controller` | `*map[string]ResourceStore` | Controller-managed stores; currently `controller["haproxy_pods"]` only |
| `pathResolver` | `*PathResolver` | `pathResolver.GetPath(name, kind)` for map / SSL / file / crt-list / secret paths |
| `fileRegistry` | `*FileRegistrar` | Templates can register dynamically generated auxiliary files via this |
| `templateSnippets` | `*[]string` | Names of available snippets; useful with `render_glob` |
| `shared` | `*SharedContext` | Root memoisation; incremental components use keyed unique output, publication, ranked publication, and exact selection |
| `dataplane` | `*map[string]any` | The CRD's `spec.dataplane` block — port, timeouts, paths |
| `capabilities` | `*map[string]any` | HAProxy feature flags derived from the local HAProxy version |
| `http` | `*HTTPFetcher` | `http.Fetch(url, opts)` for HTTP resources |
| `runtimeEnvironment` | `*RuntimeEnvironment` | Runtime info (`GOMAXPROCS`, etc.) |
| `extraContext` | `*map[string]any` | User-defined variables from `templatingSettings.extraContext` |
| typed-resource globals | `*[]*resources.<name>.T` | One per `watchedResources` entry when a schema is loaded — same name as the watched-resource key (e.g. `gateways`, `httproutes`). The `resources.<name>.T` selector chain is also a usable type expression in macro signatures, type assertions, and type-switch case clauses. |

Callers can inject additional per-render declarations through `Options.Declarations` — for example, the renderer and template validator both add `currentConfig` (`*renderplan.CurrentConfig`, nil on first deployment) so slot-preserving templates can guard with `{% if !isNil(currentConfig) %}`. The typegen-derived typed globals are injected via the same mechanism — `pkg/k8s/typegen` builds the `reflect.Type` declarations the engine merges in before compile.

To add a new runtime variable, declare it in `buildScriggoGlobals` with a nil pointer of the right type, then pass the value via the render context map — there's a walkthrough in `pkg/templating/CLAUDE.md`.

## Post-Processing

After rendering, a template can pass through a chain of post-processors — useful for fixing up indentation or running a second Scriggo pass with access to the first pass's output:

```yaml
postProcessing:
  - type: regex_replace
    params:
      pattern: "^[ ]+"
      replace: "  "
  - type: template
    params:
      source: |
        {%- if strings_contains(input, "__PLACEHOLDER__") -%}
        {{ replace(input, "__PLACEHOLDER__", "computed") }}
        {%- else -%}
        {{ input }}
        {%- end -%}
```

The `template` post-processor compiles at engine init (so syntax errors fail fast) and receives the rendered output as `input`.

## Design Rule: Resource-Agnostic

This package intentionally does **not** understand Kubernetes resources. There is no `lookup_service_port`, no `is_ingress`, no Gateway API helpers — those would turn the template engine into a policy layer for specific resource shapes. Users write resource-specific logic as Scriggo macros inside their own template libraries, and the engine stays generic enough to template anything. If you find yourself wanting to add a function that navigates a specific resource's fields, write a macro instead.

This is also why there is no `renderResource()` template function. Earlier versions of the codebase shipped one — an imperative collector populated as a side effect of rendering — but it has been removed. Resource emission is now a top-level CR concern: callers declare templates under `spec.k8sResources` (sibling of `templateSnippets`, `maps`, `files`, `sslCertificates`), the renderer renders each one, parses the rendered YAML (multi-doc supported via `---`), and registers the resulting `*RenderedResourceCollector` as a synchronous accumulator on the `RenderResult`. Downstream consumers (the controller's resourceapplier) read that slice off `RenderResult.RenderedResources`. The collector type still exists as the consumer-facing accumulator, but it is no longer fed by templates calling a side-effecting filter.

## Scriggo Fork

The engine depends on a forked Scriggo (`github.com/haproxy-haptic/scriggo`) consumed as a normal `require` in `go.mod` — there is no `replace` directive. The pinned pseudo-version drifts as Renovate updates the dep; check `grep github.com/haproxy-haptic/scriggo go.mod` for the live value rather than copying one out of this README. The fork adds:

- A native `{% include "..." %}` statement for compile-time includes.
- A `callNative` fast path that eliminates `reflect.Value.Call` for the haptic function signatures — the hot render loop is effectively zero-allocation after warm-up.
- Nil-safety fixes around `reflect.Value.Interface()` for dynamic includes.

Nothing in here expects vanilla Scriggo; don't swap the dep for upstream without running the template benchmarks.

Each engine owns the bounded `regex_search` cache, so concurrent renders share compiled patterns and replacing the engine makes them eligible for garbage collection.

## Sandbox Posture

Templates come from `HAProxyTemplateConfig` / `HAProxyTemplateLibrary` objects and execute in-process on the controller's goroutines — including on the admission path, before an operator has necessarily reviewed them. What that execution can and cannot reach:

| Capability | Contained | How |
|---|---|---|
| Go package imports | Yes | `BuildOptions.Packages` is never set, so `{% import "os" %}` fails to compile |
| Native function surface | Yes | Only the `native.Declarations` map built in `filters_scriggo.go` is nameable — crypto, encoding, html, math, regexp, sort, strconv, time, strings. No `os`, `net`, `exec`, `reflect`, `unsafe` |
| Filesystem | Yes | The compile FS serves an in-memory template map only |
| `{% go f() %}` statement | Yes | `AllowGoStmt: false`. Parallel rendering uses `{{ go Macro(...) }}`, a different node (`OpGoRender`), which stays available |
| Panics | Yes | Recovered into `*RenderError`. Exception: `native.Env.Fatal` is deliberately unrecoverable — `regex_search` relies on it to reject an uncompilable operator-supplied pattern |
| Unbounded loop | Yes, if the caller passes a cancellable context | The VM re-checks cancellation between instructions, so it stops a running template rather than abandoning its result |
| Archive expansion | Yes | `untar_gz` caps entries, per-entry bytes, and total bytes |
| Network egress | **No** | `http.Fetch(url)` performs an outbound request from inside template execution |
| Allocation | **No** | No memory or instruction budget; `seq(n)` allocates `n` ints with no ceiling |

The two uncontained rows are the real residual risk: a template can fetch an arbitrary URL, and one that allocates without bound is limited only by the render timeout and the container memory limit. Both matter most on the admission path, where the render is on the apiserver's request path.

**Upstream tracking.** Renovate follows the fork's own branch; nothing watches upstream Scriggo, and `govulncheck` keys on module path, so an advisory against `github.com/open2b/scriggo` would not match this dependency. Taking an upstream security fix is a manual rebase today. The divergence map, sync cadence, and advisory-watch process live in [Scriggo fork maintenance](../../docs/site/docs/development/scriggo-fork-maintenance.md).

## Testing

```bash
go test ./pkg/templating/...          # unit tests
go test ./pkg/templating/... -race    # race detector (engine is concurrent-safe)
go test ./pkg/templating/... -bench=. # benchmarks
```

The benchmarks in `benchmark_pool_test.go` and `benchmark_test.go` are the authoritative numbers; don't trust ad-hoc "~X µs per render" claims in prose documentation.

## See Also

- `pkg/templating/CLAUDE.md` — runtime-variable pattern, adding new filters, Scriggo fork notes
- `docs/site/docs/templating.md` — template-author reference (syntax, filters, context variables)
- `pkg/controller/rendercontext` — builds the render context from watched stores and HTTP resources
- `pkg/controller/renderer` — event adapter that wires this engine into the reconciliation pipeline
- [Scriggo Templates](https://scriggo.com/templates) — base syntax reference

## License

Apache-2.0 — see root `LICENSE`.
