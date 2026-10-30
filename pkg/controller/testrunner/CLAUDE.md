# pkg/controller/testrunner - Validation Test Runner

Development context for the validation test runner component.

## When to Work Here

Modify this package when:

- Changing test execution logic
- Adding new assertion types
- Modifying fixture processing
- Improving test result formatting
- Fixing test runner bugs

**DO NOT** modify this package for:

- CLI command implementation → Use `cmd/haptic`
- Webhook integration → Use `pkg/controller/dryrunvalidator`
- Template rendering → Use `pkg/templating`
- HAProxy validation → Use `pkg/dataplane`

## Package Purpose

This package implements a pure test runner component that executes embedded validation tests defined in HAProxyTemplateConfig CRDs. It's designed to be called directly from:

1. **CLI** (`haptic validate` command) - For local development and CI/CD
2. **Webhook** (via DryRunValidator) - For admission control validation

**Key Design Principle**: Pure component with no EventBus dependency. This allows direct function calls without event coordination overhead.

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                     Test Runner (Pure)                      │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│  1. Fixture Processing                                     │
│     - Parse test fixtures from CRD                         │
│     - Create resource stores with indexing                 │
│     - Populate stores with test data                       │
│                                                             │
│  2. Template Rendering                                     │
│     - Build rendering context with fixture stores          │
│     - Render HAProxy config + auxiliary files              │
│     - Handle rendering errors                              │
│                                                             │
│  3. Assertion Execution                                    │
│     - Run all assertions for each test                     │
│     - Collect pass/fail results                            │
│     - Capture detailed error messages                      │
│                                                             │
│  4. Result Aggregation                                     │
│     - Aggregate test results                               │
│     - Calculate pass/fail counts                           │
│     - Format output for CLI/webhook                        │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

## Files Overview

### runner.go - Main Test Orchestration

**Key Functions:**

- `(*Runner).RunTests(ctx, testName)` - Executes all tests, or just `testName` when non-empty
- `(*Runner).runSingleTest(ctx, name, test, engine, paths)` - Executes one test with its own engine + validation paths

### Validation render contexts

- **Assertions** render with `AssertionExtraContext`: `templatingSettings.testExtraContext` < `_global` < the test's own `extraContext`. The deployment's `extraContext` never reaches them, like live resources never reach a fixture store.
- **`assertDeploymentRenders`** renders the same fixtures with `DeploymentExtraContext` (`extraContext` < `_global` < the test's) and checks validity only: the render succeeds, and `haproxy -c` passes where the test asserts `haproxy_valid` and the bytes differ from the first render. Any render error fails it, `fail()` included: in a reconcile render a `fail()` stops the whole config. Skipped when the two contexts are equal, when the first render failed, and for `rendering_error` tests.

The runner retains one sealed plan-token authority per immutable engine, matching production. Each render still creates its own registry, declarations, and output.

Every render of the runner's engine (assertion, determinism re-render, deployment) shares one `renderer.ColdIncrementalRenderAnalysis`, built once per runner from the config and engine alone. Share only such config-derived, read-only data across renders; never a render's output or state, or the determinism re-render stops being independent.

`extraContext.renderMode` and `extraContext.admissionSubject` become builder options (`ExtraContextOptions`): the builder owns both globals and would overwrite promoted keys.

Tests with `useDeploymentWatchScopes: true` filter fixtures to the configured watch namespaces and enforce namespace lookup failures in direct and incremental renders. Other tests retain fixture isolation. Scope-only failures may satisfy `rendering_error`; mixed store or materialization failures still fail. Enabling this setting on `_global` is invalid.

### rendering.go - Template Rendering for Tests

**Key Functions:**

- `(*Runner).renderWithStores(...)` - Renders the HAProxy config + auxiliary files using fixture stores and a worker-specific engine
- `(*Runner).buildRenderingContext(...)` - Wraps stores with `StoreWrapper` and assembles the full template context (resources, paths, HTTP store, current config)

**Follows DryRunValidator Pattern**: The rendering logic mirrors `DryRunValidator`'s overlay-store rendering to ensure consistency between admission webhook validation and `haptic validate` runs.

### fixtures.go - Fixture Store Creation

**Key Functions:**

- `(*Runner).CreateStoresFromFixtures(fixtures)` - Converts test fixtures into typed `stores.Store` instances per resource type
- `MergeFixtures(global, test)` - Merges global and per-test fixture maps (later overrides earlier when identities collide)

**Implementation Details:**

- Creates one `stores.Store` per watched resource type plus the auto-injected `haproxy-pods` store
- Uses each `WatchedResource`'s configured `IndexBy` paths so fixture lookups exercise the same indexes the controller uses at runtime
- Ensures resources have proper TypeMeta (APIVersion, Kind) before insertion

### http_fixtures.go - HTTP Fixture Handling

**Key Types:**

- `FixtureHTTPStoreWrapper` - Template-callable wrapper that satisfies `httpstore.Wrapper`

**Key Functions:**

- `NewFixtureHTTPStoreWrapper(store, logger)` - Creates the wrapper around a pre-loaded `*httpstore.HTTPStore`
- `(*FixtureHTTPStoreWrapper).Fetch(args...)` - Returns fixture content or error if the URL isn't a fixture
- `CreateHTTPStoreFromFixtures(fixtures, logger)` - Creates an `*httpstore.HTTPStore` with fixtures pre-loaded
- `MergeHTTPFixtures(global, test)` - Merges global and per-test HTTP-fixture lists (later wins on URL conflict)

**Behavior:**

- Always returns fixture content for known URLs
- Fails with descriptive error for unknown URLs (no network requests)
- Test-specific fixtures override global fixtures for same URL

### assertions.go - Assertion Types

Implements 8 assertion types (see the dispatch switch in `assertions.go`):

1. **haproxy_valid** - `haproxy -c` validation of the rendered config
2. **contains** - Regex must match the target at least once
3. **not_contains** - Regex must not match anywhere
4. **match_count** - Regex must match exactly `count` times
5. **equals** - Exact string comparison against `expected`
6. **jsonpath** - JSONPath query against the template context evaluated to `expected`
7. **match_order** - Sequence of regexes must match in order within the target
8. **deterministic** - Renders the template a second time and verifies the output (config + every auxiliary file) is byte-identical to the first render. **The runner appends this to every rendering test automatically**, so declaring it is only needed to attach a custom description; a test that declares it is not checked twice. It is skipped for tests whose render failed, and for `rendering_error` tests.

   The second render is fed the first render's general files as `currentFiles`, which is what the controller does on every reconcile after the first. Without that, a template that mints state on first sight of an empty input (the TLS session-ticket keys) reads as nondeterministic while being correct. The property asserted is therefore a fixed point: a render fed its own output does not churn, because churn is a spurious sync and reload.

   **Detection is probabilistic, and needs a fixture with at least two keys.** Go cannot reorder a single-element map, so a redirect-code fixture with one code passes whether or not the template sorts — that is a coverage gap in the fixture, not in the check, and the fix is a fixture with two. Even then the two renders may coincidentally agree: measured against a deliberately unsorted two-code site, a single run caught it 5 times in 6. One green run is therefore not proof a site is sorted; a green CI history is.

Each test samples its `cycleTimeBucket` input once before rendering. Both
renders receive that snapshot; crossing a wall-clock bucket boundary cannot
change the inputs to the determinism assertion.

**Target Resolution** (see `assertion_helpers.go:resolveTarget`):

- `haproxy.cfg` (or empty) - Main HAProxy configuration
- `map:<name>` - Map file content; matches by full path or basename
- `file:<name>` - General file content; matches by filename
- `cert:<name>` - SSL certificate content
- `crt-list:<name>` - CRT-list file content (matched against the rendered file's basename or full path; works on any HAProxy version because crt-list files always render into the auxiliary files irrespective of how they're synced — see `pkg/dataplane/auxiliaryfiles/crtlist.go`)
- `rendering_error` - The simplified template error string when rendering fails; resource-input failures always fail the test except expected namespace-scope rejections in opted-in tests

Unknown targets fall back to the main HAProxy config silently.

### output.go - Result Formatting

Formats test results in three modes:

- **Summary** - Human-readable with ✓/✗ symbols
- **JSON** - Structured output for CI/CD tools
- **YAML** - Structured output for readability

## Testing Strategy

### Unit Tests (runner_test.go)

**Coverage Areas:**

- Basic rendering with assertions
- Test filtering by name
- Mixed pass/fail results
- Fixtures used in templates
- Rendering error handling
- Edge cases

**Testing Pattern:**

```go
func TestRunner_Feature(t *testing.T) {
    // 1. Build the internal config (testrunner takes *config.Config from
    //    pkg/core/config, not the CRD spec — conversion happens upstream).
    cfg := &config.Config{
        HAProxyConfig: config.HAProxyConfig{Template: "..."},
        ValidationTests: map[string]config.ValidationTest{
            "my-test": {Description: "...", Assertions: []config.ValidationAssertion{...}},
        },
    }

    // 2. Create template engine
    engine, err := templating.New(templates, nil, nil, nil)

    // 3. Create test runner — validationPaths can be nil for tests that don't
    //    exercise the HAProxy binary; pass real paths for "haproxy -c" assertions.
    runner := testrunner.New(cfg, engine, nil, testrunner.Options{})

    // 4. Run tests ("" = all tests; pass a name to filter)
    results, err := runner.RunTests(ctx, "")

    // 5. Verify results
    assert.Equal(t, expectedPassed, results.PassedTests)
}
```

### Integration Tests (Future)

Should test:

- CLI command execution with real CRD files
- Webhook validation with embedded tests
- Full validation flow with HAProxy binary

## Observability Features

The test runner provides rich observability to help debug failing tests, both via CLI flags and programmatically.

### Content Preview in Assertions

All assertions populate target metadata for observability:

```go
type AssertionResult struct {
    Type        string
    Description string
    Passed      bool
    Error       string

    // Observability fields
    Target        string  // e.g., "map:path-prefix.map"
    TargetSize    int     // Content size in bytes
    TargetPreview string  // First 200 chars (failed assertions only)
}
```

**Implementation:**

- `populateTargetMetadata()` called by all assertion methods
- Preview only for failed assertions (keeps output manageable)
- Truncated to 200 chars to prevent huge outputs

**Usage in assertions:**

```go
// Example from assertContains
func (r *Runner) assertContains(...) AssertionResult {
    target := r.resolveTarget(assertion.Target, haproxyConfig, auxiliaryFiles, renderError)

    matched, err := regexp.MatchString(assertion.Pattern, target)
    if !matched {
        result.Passed = false
        result.Error = fmt.Sprintf("pattern %q not found in %s (target size: %d bytes). Hint: Use --verbose to see content preview",
            assertion.Pattern, assertion.Target, len(target))
    }

    // Populate target metadata for observability
    r.populateTargetMetadata(&result, target, assertion.Target, !matched)

    return result
}
```

### Rendered Content Storage

Test results include complete rendered content for debugging:

```go
type TestResult struct {
    // ... existing fields ...

    // Rendered content (for --dump-rendered)
    RenderedConfig string              // HAProxy configuration
    RenderedMaps   map[string]string   // Map files (path → content)
    RenderedFiles  map[string]string   // General files (filename → content)
    RenderedCerts  map[string]string   // SSL certificates (path → content)
}
```

**Populated in `runSingleTest()`:**

- Captured immediately after successful rendering
- Only populated on successful render (empty if render fails)
- Maps use file path/name as key
- Available in JSON/YAML output formats

**Example population:**

```go
// After successful rendering
if err == nil {
    result.RenderedConfig = haproxyConfig

    if len(auxiliaryFiles.MapFiles) > 0 {
        result.RenderedMaps = make(map[string]string)
        for _, mapFile := range auxiliaryFiles.MapFiles {
            result.RenderedMaps[mapFile.Path] = mapFile.Content
        }
    }
}
```

### Verbose Mode

Verbose mode shows target metadata for failed assertions:

```go
output, err := testrunner.FormatResults(results, testrunner.OutputOptions{
    Format:  testrunner.OutputFormatSummary,
    Verbose: true,  // Enable verbose mode
})
```

**Output formatting** (`formatSummary()` in output.go):

- Shows target name and size for all failed assertions
- Shows content preview if available
- Adds hint about --dump-rendered for large targets (>200 chars)

**Example verbose output:**

```
✗ Path map must use MULTIBACKEND qualifier
  Error: pattern "..." not found in map:path-prefix.map (target size: 61 bytes)
  Target: map:path-prefix.map (61 bytes)
  Content preview:
    split.example.com/app MULTIBACKEND:0:default_split-route_0/
  Hint: Use --dump-rendered to see full content
```

### Enhanced Error Messages

All assertion methods produce enhanced error messages by default:

**Pattern not found:**

```
pattern "X" not found in map:path-prefix.map (target size: 61 bytes).
Hint: Use --verbose to see content preview
```

**Match count:**

```
expected 2 matches, got 0 matches of pattern "X" in map:path-prefix.map (target size: 61 bytes).
Hint: Use --verbose to see content preview
```

**HAProxy validation:**

```
HAProxy validation failed (config size: 1234 bytes): maxconn: integer expected
```

**Benefits:**

- Users immediately see target size without flags
- Clear hint about --verbose flag
- Context included in all error messages
- Discoverability of debugging features

### Template Tracing Integration

If the template engine has tracing enabled, render operations are traced:

```go
engine.EnableTracing()

// All Render() calls are traced
runner := testrunner.New(cfg, engine, paths, options)
results, _ := runner.RunTests(ctx, "")

// Get trace output
trace := engine.GetTraceOutput()
fmt.Println(trace)
```

**Trace output shows:**

- Which templates were rendered
- Render duration in milliseconds
- Nesting depth (for includes)

**Example trace:**

```
Rendering: haproxy.cfg
Completed: haproxy.cfg (0.007ms)
Rendering: path-prefix.map
Completed: path-prefix.map (3.347ms)
```

### Programmatic Usage

**Enable verbose output:**

```go
results, err := runner.RunTests(ctx, "")

output, err := testrunner.FormatResults(results, testrunner.OutputOptions{
    Format:  testrunner.OutputFormatSummary,
    Verbose: true,
})
```

**Access rendered content:**

```go
for _, test := range results.TestResults {
    if !test.Passed {
        fmt.Printf("Test %s failed\n", test.TestName)
        fmt.Printf("Rendered config:\n%s\n", test.RenderedConfig)

        for mapName, content := range test.RenderedMaps {
            fmt.Printf("Map %s:\n%s\n", mapName, content)
        }
    }
}
```

**Access assertion metadata:**

```go
for _, assertion := range test.Assertions {
    if !assertion.Passed {
        fmt.Printf("Assertion failed: %s\n", assertion.Description)
        fmt.Printf("Target: %s (%d bytes)\n", assertion.Target, assertion.TargetSize)
        if assertion.TargetPreview != "" {
            fmt.Printf("Preview: %s\n", assertion.TargetPreview)
        }
    }
}
```

## Common Debugging Patterns

### Debugging Empty Map Files

```bash
# 1. Check what was rendered
haptic validate -f config.yaml --dump-rendered

# 2. See if template executed
haptic validate -f config.yaml --trace-templates

# 3. Look for template errors in verbose output
haptic validate -f config.yaml --verbose
```

**Common causes:**

- Empty loops (no resources match filters)
- Incorrect variable names in templates
- Missing `| default([])` filters on arrays
- Conditional logic preventing execution

### Debugging Pattern Mismatches

```bash
# See actual content vs expected pattern
haptic validate -f config.yaml --verbose
```

**Look for:**

- Whitespace differences (extra newlines, trailing spaces)
- Case sensitivity issues
- Regex special characters that need escaping
- Multiline patterns missing `(?m)` flag

**Example:**

```
Expected: "backend foo"
Got:      " backend foo"  (extra leading space)
```

### Debugging Slow Tests

```bash
# See template render times
haptic validate -f config.yaml --trace-templates
```

**Templates taking >10ms may need optimization:**

- Simplify complex loops
- Reduce nested includes
- Avoid expensive filters in loops
- Cache repeated computations

**Example trace showing slow template:**

```
Rendering: haproxy.cfg (0.005ms)
Rendering: backends.cfg (45.123ms)  ← Needs optimization
```

### Debugging Test Fixtures

```bash
# Dump rendered content to see if fixtures loaded correctly
haptic validate -f config.yaml --dump-rendered
```

**Common fixture issues:**

- Missing `apiVersion` or `kind` fields
- Incorrect index keys (resource not findable)
- Wrong namespace or name in fixture data

## Common Patterns

### Running All Tests

```go
runner := testrunner.New(
    config,
    engine,
    validationPaths,
    testrunner.Options{
        Logger: logger,
    },
)

results, err := runner.RunTests(ctx, "")
if err != nil {
    return err
}

if !results.AllPassed() {
    // Handle test failures
}
```

### Running Specific Test

```go
results, err := runner.RunTests(ctx, "my-test-name")
if err != nil {
    return fmt.Errorf("test %q failed: %w", "my-test-name", err)
}
```

### Custom Validation Paths

`testrunner.New` takes `*dataplane.ValidationPaths` (pointer; pass `nil` to skip
binary-backed assertions). The struct has no `HAProxyBinary` field — the
binary is discovered from `$PATH` at validation time. The fields it does have
mirror HAProxy's runtime layout:

```go
validationPaths := &dataplane.ValidationPaths{
    TempDir:           "/tmp/haproxy-validation",
    ConfigFile:        "/tmp/haproxy-validation/haproxy.cfg",
    MapsDir:           "/tmp/haproxy-validation/maps",
    SSLCertsDir:       "/tmp/haproxy-validation/ssl",
    CRTListDir:        "/tmp/haproxy-validation/ssl",
    GeneralStorageDir: "/tmp/haproxy-validation/general",
}

runner := testrunner.New(config, engine, validationPaths, options)
```

For HAProxy-binary assertions to actually run, every directory in the struct must
exist on disk before `RunTests` is called — `pkg/controller/validation/service.go`
shows the canonical pattern of `os.MkdirAll`-ing each field after picking a temp
root.

## Error Handling

### Rendering Errors

Rendering errors pass through `templating.RenderErrorMessage()`:

```go
rendered, err := r.renderWithStores(ctx, engine, stores, validationPaths, httpStore, currentConfig, currentFiles, extraContext)
if err != nil {
    result.RenderError = templating.RenderErrorMessage(err)
}
```

A render a template stopped with `fail()` reports only the `fail()` message
(`*templating.FailError`); any other failure keeps its full error text.

### Validation Errors

HAProxy validation errors are simplified using `dataplane.SimplifyValidationError()`:

```go
// Real signature: ValidateConfigurationContext(ctx, mainConfig, auxFiles, paths, skipDNSValidation, gate)
// returns nil or a ValidationError. Every call executes HAProxy. The gate is a
// *dataplane.CheckGate; the runner passes its own multi-slot r.checkGate so the
// worker pool's `haproxy -c` runs go wide instead of serializing on the default
// single-slot gate. nil uses that shared default gate.
err := dataplane.ValidateConfigurationContext(ctx, haproxyConfig, auxiliaryFiles, validationPaths, false, r.checkGate)
if err != nil {
    result.Error = dataplane.SimplifyValidationError(err)
}
```

**Example**:

- Raw: `[ALERT] 350/123456 (12345) : parsing [/tmp/haproxy.cfg:15] : 'maxconn' : integer expected, got 'invalid' (line 15, column 12)`
- Simplified: `maxconn: integer expected, got 'invalid' (line 15)`

## Fixture Processing

### Creating Stores from Fixtures

Fixtures are converted to resource stores for template rendering:

```go
stores, err := r.createStoresFromFixtures(test.Fixtures)
// stores["services"] contains a types.Store with indexed service resources
```

### Index Key Extraction

Uses the same indexing as production watchers:

```go
// From CRD spec
watchedResource := r.config.WatchedResources["services"]
// IndexBy: ["metadata.namespace", "metadata.name"]

// Extract keys using indexer
idx, _ := indexer.New(indexer.Config{
    IndexBy: watchedResource.IndexBy,
})
keys, _ := idx.ExtractKeys(&resource)
// keys: ["default", "my-service"]

// Add to store with keys
store.Add(&resource, keys)
```

### TypeMeta Inference

Fixtures may omit TypeMeta fields. The runner infers them:

```go
if resource.GetAPIVersion() == "" {
    resource.SetAPIVersion(watchedResource.APIVersion)
}
if resource.GetKind() == "" {
    kind := SingularizeResourceType(watchedResource.Resources)
    resource.SetKind(kind)
}
```

**Example**: `"services"` → `"Service"`

## Template Context Building

### StoreWrapper Usage

Fixtures are wrapped with `rendercontext.StoreWrapper` for template access via the centralized `rendercontext.Builder`:

```go
builder := rendercontext.NewBuilder(
    ctx,
    r.config, pathResolver, r.logger,
    rendercontext.WithStores(resourceStores),
    rendercontext.WithHAProxyPodStore(haproxyPodStore),
    rendercontext.WithHTTPFetcher(httpStore),
    rendercontext.WithCurrentConfig(currentConfig),
    rendercontext.WithTypedResources(r.typedResourceTypes), // nil unless CLI wired typebootstrap
)
renderCtx := builder.Build().Context
```

The Builder wraps each store entry into a `*rendercontext.StoreWrapper` with the configured `IndexBy` paths. Direct `StoreWrapper` construction is not used in the testrunner.

**Template Usage**:

```go
{% for _, svc := range resources.services.List() %}
  {{ svc.metadata.name }}
{% end %}
```

### Context Structure

```go
context := map[string]any{
    "resources": map[string]*rendercontext.StoreWrapper{
        "services": ...,
        "ingresses": ...,
    },
    "templateSnippets": []string{"snippet1", "snippet2"},
}
```

## Assertion Implementation

### Pattern Matching (contains, not_contains)

Uses Go's regexp package:

```go
matched, err := regexp.MatchString(assertion.Pattern, target)
if err != nil {
    result.Error = fmt.Sprintf("invalid regex pattern: %v", err)
}
```

### Exact Comparison (equals)

Direct string comparison with truncation for long values:

```go
if target != assertion.Expected {
    targetPreview := truncateString(target, 100)
    expectedPreview := truncateString(assertion.Expected, 100)
    result.Error = fmt.Sprintf("expected %q, got %q", expectedPreview, targetPreview)
}
```

### JSONPath Queries (jsonpath)

Uses client-go's JSONPath implementation:

```go
jp := jsonpath.New("assertion")
jp.Parse(assertion.JSONPath)
results, _ := jp.FindResults(templateContext)

actualValue := fmt.Sprintf("%v", results[0][0].Interface())
if actualValue != assertion.Expected {
    result.Error = fmt.Sprintf("expected %q, got %q", assertion.Expected, actualValue)
}
```

## Common Pitfalls

### Using the Wrong Store Interface

**Problem**: Importing the wrong `Store` type. The codebase has two — they're
deliberately structurally identical (Go satisfies both implicitly), but the one
testrunner expects is `pkg/stores.Store`, not `pkg/k8s/types.Store`.

```go
// Bad — testrunner returns map[string]stores.Store, not this
var fixtureStores map[string]types.Store
fixtureStores, _ = runner.CreateStoresFromFixtures(...) // does not compile

// Good
var fixtureStores map[string]stores.Store
fixtureStores, _ = runner.CreateStoresFromFixtures(...)
```

**Why**: `pkg/stores` is the controller-side store interface used by everything
that wraps fixtures or overlays for template rendering. `pkg/k8s/types.Store`
exists only so the watcher layer can stay independent of the controller; both
interfaces declare the same methods so the same concrete store satisfies both.

### Forgetting to Extract Index Keys

**Problem**: Adding resources without index keys.

```go
// Bad
store.Add(&resource)  // Missing keys parameter!

// Good
keys, _ := indexer.ExtractKeys(&resource)
store.Add(&resource, keys)
```

### Wrong Template Method Name

**Problem**: Using lowercase `list()` instead of `List()`.

```go
{# Bad #}
{% for _, svc := range resources.services.list() %}

{# Good #}
{% for _, svc := range resources.services.List() %}
```

**Why**: StoreWrapper methods are capitalized (Go convention).

### Not Handling Rendering Errors

**Problem**: Assuming rendering always succeeds.

```go
// Bad - doesn't check for rendering errors
result.Passed = true

// Good - checks for rendering errors
if result.RenderError != "" {
    result.Passed = false
    return result
}
```

## Adding New Assertion Types

### Checklist

1. Add assertion type constant to CRD
2. Implement assertion method in assertions.go
3. Add case in `runAssertion()` switch
4. Add unit tests
5. Document in user documentation

### Example: Adding "regex_match" Assertion

```go
// Step 1: Add to runner.go switch
case "regex_match":
    result = r.assertRegexMatch(haproxyConfig, auxiliaryFiles, assertion)

// Step 2: Implement assertion method
func (r *Runner) assertRegexMatch(
    haproxyConfig string,
    auxiliaryFiles *dataplane.AuxiliaryFiles,
    assertion config.ValidationAssertion,
) AssertionResult {
    result := AssertionResult{
        Type:        "regex_match",
        Description: assertion.Description,
        Passed:      true,
    }

    target := r.resolveTarget(assertion.Target, haproxyConfig, auxiliaryFiles)

    re, err := regexp.Compile(assertion.Pattern)
    if err != nil {
        result.Passed = false
        result.Error = fmt.Sprintf("invalid regex: %v", err)
        return result
    }

    matches := re.FindAllString(target, -1)
    if len(matches) == 0 {
        result.Passed = false
        result.Error = "no matches found"
    }

    return result
}

// Step 3: Add unit tests
func TestRunner_RegexMatch(t *testing.T) {
    // Test implementation...
}
```

## Performance Considerations

### Memory Usage

- **Fixtures**: Stored in memory as unstructured resources (~1KB per resource)
- **Stores**: MemoryStore with O(1) lookups via composite keys
- **Rendering**: Single render per test (not cached across tests)

### What's Already Optimized

- **Parallel test execution**: A worker pool (`testWorker` in `runner.go`, sized to `Options.Workers`, or automatically limited by twice `GOMAXPROCS` and one worker per 128 MiB of `GOMEMLIMIT`) overlaps rendering with HAProxy checks. Simultaneous HAProxy processes remain capped at `GOMAXPROCS`. Each worker gets its own `ValidationPaths` temp directory so `haproxy -c` runs don't collide.
- **Concurrent `haproxy -c`**: `RunTests` builds a multi-slot `dataplane.CheckGate` (`r.checkGate`, sized to the worker count but capped at `GOMAXPROCS`) and passes it to every `haproxy_valid` assertion. Without it the workers serialize behind dataplane's single-slot *default* gate, so the pool gains nothing on `haproxy_valid`-heavy suites — measured 12.8s → 2.3s on a 529-test / 411-`haproxy_valid` corpus (16 cores). This path is the startup and reinit config-load gate, so the win is also faster startup and shorter leaderless windows on a config-change reinit.
- **Template engine reuse**: The pre-compiled `templating.Engine` is shared across workers. Per-render state (filter context, current config) is passed in `additionalDeclarations`, not stored on the engine.
- **Fresh HAProxy verdicts**: repeated `haproxy_valid` assertions invoke HAProxy again. A content-only cache can't observe a changed executable or runtime environment.

### Remaining Opportunities

- **Store reuse across tests with identical fixtures** — currently each test rebuilds its `stores.Store`s from scratch, even when fixtures match the previous test verbatim.

## Resources

- API documentation: `pkg/controller/testrunner/README.md`
- User documentation: `docs/site/docs/validation-tests.md`
- DryRunValidator pattern: `pkg/controller/dryrunvalidator/CLAUDE.md`
- StoreWrapper: `pkg/controller/rendercontext/CLAUDE.md` (lives there, not in `pkg/controller/renderer/`)
- Architecture: `/docs/site/docs/development/design.md`
