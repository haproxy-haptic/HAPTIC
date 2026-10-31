# pkg/controller/testrunner

Pure component that executes the embedded validation tests under `spec.validationTests` in a `HAProxyTemplateConfig`. No EventBus dependency — it's a library that CLI and webhook paths both call directly.

## What It Does

For each test case:

1. Build a fixture-driven render context (the test's `fixtures` are injected as a parallel resource store, no cluster calls).
2. Render every template in the config.
3. Evaluate each assertion in the test (`haproxy_valid`, `contains`, `not_contains`, `match_count`, `equals`, `jsonpath`, `match_order`, `deterministic`). The `haproxy_valid` assertion type runs `haproxy -c` against the rendered output using the supplied `ValidationPaths`; other assertion types do not invoke the HAProxy binary.
4. Collect timing, rendered content, and assertion results into a `TestResult`.

The whole suite runs in a worker pool (`Options.Workers`, defaults to the CPU and memory budget). `TestResults` aggregates pass/fail/skip counts and optional summary timings.

## Minimal Usage

```go
import (
    "context"
    "path/filepath"

    "github.com/haproxy-haptic/HAPTIC/pkg/controller/testrunner"
    "github.com/haproxy-haptic/HAPTIC/pkg/core/config"
    "github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
    "github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

cfg, _, _ := conversion.ParseCRD(unstructuredHAProxyTemplateConfig)
config.SetDefaults(cfg)

engine, _ := templating.New(buildTemplates(cfg), nil)

paths := &dataplane.ValidationPaths{
    TempDir:           tempDir,                               // created + cleaned up by the caller
    MapsDir:           filepath.Join(tempDir, "maps"),
    SSLCertsDir:       filepath.Join(tempDir, "ssl"),
    CRTListDir:        filepath.Join(tempDir, "crt-list"),    // on HAProxy < 3.2 this may equal SSLCertsDir
    GeneralStorageDir: filepath.Join(tempDir, "general"),
    ConfigFile:        filepath.Join(tempDir, "haproxy.cfg"),
}

runner := testrunner.New(cfg, engine, paths, &testrunner.Options{
    Workers:         0,            // 0 → CPU and memory budget
    DebugFilters:    false,        // set for `--debug-filters`
    ProfileIncludes: false,        // set for `--profile-includes`
    Capabilities:    capabilities, // from dataplane.CapabilitiesFromVersion
    HAProxyVersion:  haproxyVer,
})

results, err := runner.RunTests(context.Background(), "") // "" = all tests

out, _ := testrunner.FormatResults(results, testrunner.OutputOptions{
    Format:       testrunner.OutputFormatSummary, // or OutputFormatJSON / OutputFormatYAML
    Verbose:      false,
    DumpRendered: false,
})
fmt.Println(out)
```

The HAProxy binary is looked up via `exec.LookPath("haproxy")` by `pkg/dataplane` — there's no field on `ValidationPaths` for it. If no binary is on `$PATH`, tests whose only assertion is `haproxy_valid` are *skipped* (not failed) and surface as `SkipReason: "haproxy binary not found"` in the results.

## API Surface

```go
func New(
    cfg *config.Config,
    engine templating.Engine,
    validationPaths *dataplane.ValidationPaths,
    options *Options,
) *Runner

func (r *Runner) RunTests(ctx context.Context, testName string) (*TestResults, error)

func FormatResults(results *TestResults, options OutputOptions) (string, error)
```

`testName == ""` runs every test; otherwise the runner filters by exact name and returns a `TestResults` containing just that one (errors if the name doesn't exist). Context cancellation aborts in-flight tests and returns whatever completed.

## Options

```go
type Options struct {
    TestName        string                 // filter; empty = all tests (alternative to RunTests' second arg)
    Logger          *slog.Logger           // defaults to slog.Default()
    Workers         int                    // 0 → CPU and memory budget; 1 → sequential
    DebugFilters    bool                   // wired to --debug-filters
    ProfileIncludes bool                   // wired to --profile-includes
    Capabilities    dataplane.Capabilities // value, not pointer; gates tests that need specific DP API features
    HAProxyVersion  *dataplane.Version
}
```

`DebugFilters` and `ProfileIncludes` control diagnostic output for each render. Workers share the compiled template engine; render context and paths remain separate.

## Result Types

- `TestResults` — suite-level: totals, duration, slice of `TestResult`.
- `TestResult` — per-test: name, pass/fail, duration, skip reason, rendered content (populated for `--dump-rendered`), auxiliary files, assertion results.
- `AssertionResult` — per-assertion: type, target (`haproxy.cfg`, `map:<name>`, `file:<name>`, `cert:<name>`, `crt-list:<name>`, `secret:<name>`, `rendering_error`), pass/fail, human-readable error message, target size in bytes, and a 200-char preview of the target on failure.

See `pkg/controller/testrunner/types.go` for the full schema; `FormatResults` can render any of them as `summary`, `json`, or `yaml`.

## Concurrency Model

The automatic worker count is the smaller of twice `GOMAXPROCS` and one worker per 128 MiB of Go's soft memory limit, with a minimum of one. This lets workers render while others wait for HAProxy checks; simultaneous HAProxy processes remain capped at `GOMAXPROCS`. Explicit `Options.Workers` or `haptic validate --workers` values override the worker count. Every selected test and assertion still runs.

`haproxy -c` subprocesses consume container memory outside Go's soft limit. In a 1 GiB container, 32 validation workers reached 29 concurrent HAProxy processes using 415 MiB alongside the Go process and were OOM-killed. Eight workers completed the same suite with a 578 MiB container peak. The 128 MiB allowance bounds both rendering and subprocess concurrency; it is not a hard limit on an individual test. Larger fixtures can still require more memory.

- Workers share the compiled `templating.Engine` and pass filter state through each render context.
- Fixtures are in-memory overlays built per-test; no shared mutable state between tests.
- `RunTests` fans tests out over a channel, fans results back in, and preserves deterministic ordering in the result slice.
- Safe to call `RunTests` multiple times on the same `Runner` — per-run state (rendered content, stores) is re-created each invocation.

## Error Simplification

Rendering and validation errors pass through `templating.RenderErrorMessage` / `dataplane.SimplifyValidationError` before landing in `TestResult.RenderError` / `TestResult.Error`. This surfaces the user-relevant message (a template's `fail()` text) instead of the full Go error chain, matching what the admission webhook emits — so CLI and webhook failure output stay consistent.

## See Also

- [`docs/site/docs/validation-tests.md`](../../../docs/site/docs/validation-tests.md) — user-facing authoring guide (fixtures, assertion types, CLI flags)
- [`pkg/dataplane/validator.go`](../../dataplane/validator.go) — `ValidationPaths` definition + the `haproxy -c` semantic validator
- [`pkg/controller/rendercontext`](../rendercontext/) — how fixtures become render contexts
- [`cmd/haptic/validate.go`](../../../cmd/haptic/validate.go) — CLI wiring (populates `Options` from flags)
- `pkg/controller/testrunner/CLAUDE.md` — developer context (adding new assertion types, fixture processing internals)

## License

Apache-2.0 — see root `LICENSE`.
