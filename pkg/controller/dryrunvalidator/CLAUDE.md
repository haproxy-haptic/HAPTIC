# pkg/controller/dryrunvalidator - Dry-Run Validation Component

Development context for the dry-run validation component used in admission webhooks.

## When to Work Here

Modify this package when:

- Changing dry-run validation logic
- Adding support for new resource types in validation
- Modifying overlay store usage
- Improving error messages for validation failures
- Adding new validation phases

**DO NOT** modify this package for:

- Template rendering logic → Use `pkg/templating`
- HAProxy validation → Use `pkg/dataplane`
- Resource storage / overlays → Use `pkg/stores`
- Webhook server → Use `pkg/webhook`

## Package Purpose

This package implements validation-by-rendering: it validates Kubernetes resources (like Ingresses) by attempting to render the HAProxy configuration with those resources included, then validating the resulting configuration with HAProxy.

**Key Features:**

- Dry-run validation (no actual deployment)
- Overlay store pattern for simulating resource changes
- Template rendering errors → user-friendly messages
- HAProxy validation errors → user-friendly messages
- Support for CREATE, UPDATE, DELETE operations

## Overlay Store Pattern

The overlay store pattern allows the validator to simulate "what-if" scenarios without modifying the actual resource stores. This is crucial for validating resources before they're admitted to the cluster.

### Problem Statement

When validating a CREATE or UPDATE operation via admission webhook:

1. The resource **doesn't exist yet** in the actual store (CREATE)
2. The resource exists but with **old version** (UPDATE)
3. We need to **validate** the config that would result from the change
4. We **cannot modify** the actual stores (dry-run only)

### Solution: Overlay Stores

Create temporary store overlays that:

- Include the new/updated resource being validated
- Reference the actual stores for all other resources
- Are discarded after validation

```
┌────────────────────────────────────────┐
│     Actual Resource Stores             │
│  (shared, read-only during validation) │
│                                        │
│  ingresses: [...existing ingresses...] │
│  services:  [...existing services...]  │
└────────────────┬───────────────────────┘
                 │
                 │ Reference (read-only)
                 ▼
┌────────────────────────────────────────┐
│      Overlay Store (temporary)         │
│                                        │
│  ingresses: [                          │
│    ...existing from actual store...    │
│    + NEW/UPDATED resource being tested │
│  ]                                     │
│  services: [read from actual store]    │
└────────────────────────────────────────┘
                 │
                 │ Used for rendering
                 ▼
        Template Rendering
                 │
                 ▼
       HAProxy Config (dry-run)
                 │
                 ▼
      Validation (accept/reject)
```

### Implementation

Overlays are built directly by the component via `pkg/stores` constructors —
there's no `StoreManager` involvement. Each admission request gets a fresh
`*stores.StoreOverlay` for every configured alias of the admission GVR. Each
alias applies its own selectors to the old and new objects; the proposal
validator merges those overlays with the live stores for the duration of the
render+validate call only.

```go
// pkg/controller/dryrunvalidator/component.go (validateWithOverlay)
func (c *Component) validateWithOverlay(
    ctx context.Context,
    gvk, namespace, name string, object, oldObject any,
    operation, requestID string, // operation is the admission verb string
) (allowed bool, reason string, warnings []string) {
    aliases, err := c.mapGVKToResourceAliases(gvk)
    if err != nil {
        return false, fmt.Sprintf("unsupported resource type: %v", err), nil
    }

    overlays, subjectAliases, err := c.createOverlays(aliases, namespace, name, object, oldObject, operation)
    if err != nil {
        return false, err.Error(), nil
    }

    _, result := c.proposalValidator.ValidateSyncWithAdmissionSubject(ctx, overlays, subjectAliases, namespace, name)
    if !result.Valid {
        return false, c.simplifyError(result.Phase, result.Error), result.Warnings
    }

    return true, "", result.Warnings
}
```

### Direct call, no StoreManager

The component talks to `pkg/stores` directly:

1. **No utility wrapper required**: `stores.NewStoreOverlayForCreate/Update/Delete`
   already covers every admission verb; wrapping it in a `StoreManager` would just
   add a hop.
2. **Synchronous**: overlay construction is immediate; no async coordination.
3. **Performance**: webhook timeouts are tight (10 s), so the path stays event-free.
4. **Scoped lifetime**: overlays die with the request — they're not registered
   anywhere global.

### Operation Types

The component speaks Kubernetes admission verbs, not an enum — `operation` is
the literal admission string from `AdmissionReview.Request.Operation`:

| String | Per-alias effect |
|--------|------------------|
| `"CREATE"` | Add when the new object matches the selector |
| `"UPDATE"` | Update, add, delete, or ignore from the old/new selector transition |
| `"DELETE"` | Delete when the old object matched the selector |

Anything else is denied because an unknown admission verb cannot be simulated.

### Memory Management

Overlay stores are:

- Created per validation request
- Garbage collected after validation completes
- Do not persist beyond the request
- Share underlying data from actual stores (copy-on-write semantics)

## Validation Flow

```
Webhook Admission Request
    ↓
1. Parse GVK → Determine resource type
    ↓
2. Create overlay stores (includes test resource)
    ↓
3. Build template context (use overlays)
    ↓
4. Render HAProxy config
    ├─ Success → Continue
    └─ Error → RenderErrorMessage → Deny
    ↓
5. Validate HAProxy config with `haproxy -c`
    ├─ Valid → Continue
    └─ Invalid → SimplifyValidationError → Deny
    ↓
6. Validate rendered files with pluggable validators (if configured in the pipeline)
    ├─ All valid → Allow (warnings → AdmissionResponse.Warnings)
    └─ Any errors → Deny (errors → reason, warnings → still propagated)
    ↓
Return (allowed bool, reason string, warnings []string) to webhook
```

## Error Handling

The component uses error simplification at component boundaries:

### Phase-Driven Simplification

`ValidateSync` returns a `*validation.ValidationResult` with `Valid` (bool),
`Error` (the underlying error), and `Phase` (one of `"render"`, `"syntax"`,
`"schema"`, `"semantic"`, `"external"`). There are no sentinel errors to compare against —
phase routing happens by string in `simplifyError`:

```go
// pkg/controller/dryrunvalidator/resource_mapping.go
func (c *Component) simplifyError(phase string, err error) string {
    if err == nil {
        return ""
    }
    switch phase {
    case "render":
        return templating.RenderErrorMessage(err)
    case "syntax", "schema", "semantic":
        return dataplane.SimplifyValidationError(err)
    default:
        return err.Error()
    }
}
```

That single helper covers both render-phase failures (template errors,
`fail()` from a template, missing variables) and validate-phase failures
(client-native syntax errors, OpenAPI schema violations, `haproxy -c`
output). The webhook handler in `validateWithOverlay` calls it once and
returns the result as the admission denial reason — there's no separate
`if errors.Is(...)` branch per phase.

## Direct Component Calls Pattern

The DryRunValidator delegates the render+validate work to a `*proposalvalidator.Service` rather than holding its own engine / validator / store-manager directly. This keeps the validator small (overlay setup + result mapping) and ensures the admission path uses exactly the same pipeline as the leader-driven reconciliation.

```go
type Component struct {
    proposalValidator *proposalvalidator.Service // Performs the full pipeline
    restMapper        meta.RESTMapper              // GVK -> GVR
    aliasesByGVR      map[schema.GroupVersionResource][]resourceAlias
    logger            *slog.Logger
}

func (c *Component) ValidateDirect(ctx context.Context, gvk, namespace, name string, object, oldObject any, operation string) (allowed bool, reason string, warnings []string) {
    // Build the overlay store, delegate render+validate to proposalValidator.ValidateSync,
    // return a flat allow/deny + reason and warnings.
    // No event hop — the webhook holds the request open and gets the answer synchronously.
}
```

**Why a synchronous library, not an event adapter:**

1. **Same pipeline as reconciliation**: webhook validation must reject configs that would also fail at deploy time, so reusing the proposal pipeline is required by spec.
2. **Performance critical**: webhook timeouts are tight (5–10 seconds); a publish/subscribe round-trip adds latency for no observable benefit.
3. **Stateless**: each validation is independent — there is no shared state for the bus to broker.

ADR-0001 settled the analogous question for the renderer; the dry-run validator follows the same shape and was cleaned up for the same reasons.

## Testing Strategy

### Unit Tests

`TestValidateDirect_GovernanceDenialLeadsWithRuleMessage` drives the chart's
governance snippet through the real render, so it pins the denial text an
operator sees: the `fail()` message alone.

### Integration Tests

The full integration shape is in `component_test.go`. The constructor takes a
`*ComponentConfig` struct:

```go
component := dryrunvalidator.New(&dryrunvalidator.ComponentConfig{
    ProposalValidator:  proposalValidator,
    RESTMapper:         restMapper,
    Logger:             logger,
    PluggableValidator: pluggableValidator, // optional
})
```

Drive validation through `component.ValidateDirect(ctx, gvk, namespace, name, object, operation)`
— there is no event-driven path to exercise.

## Common Pitfalls

### Modifying Actual Stores

**Problem**: Accidentally writing to the live store during validation. Anything
you `Add` to a real `stores.Store` survives the request.

```go
// Bad — pollutes the live store
actualStore.Add(req.Object, []string{namespace, name})
```

**Solution**: Build a `*stores.StoreOverlay` and let `proposalValidator.ValidateSync`
merge it on top of the live data for the duration of the call.

```go
// Good — overlay dies with the request
overlay := stores.NewStoreOverlayForCreate(req.Object.(runtime.Object))
result := proposalValidator.ValidateSync(ctx, map[string]*stores.StoreOverlay{
    "ingresses": overlay,
})
```

### Forgetting Error Simplification

**Problem**: Returning a raw error string to the API server.

```go
// Bad — raw library error reaches the user
return false, result.Error.Error() // "failed to render haproxy.cfg: ..."
```

**Solution**: Route through `simplifyError(phase, err)` so render and validate
errors both get the appropriate `dataplane.Simplify*Error` pass.

```go
// Good — phase-aware simplification
return false, c.simplifyError(result.Phase, result.Error)
```

### Bypassing the Admission Verb

**Problem**: Hard-coding the operation instead of using the admission request's
verb. DELETE arrives with no body — treating it as CREATE will dereference nil.

```go
// Bad — assumes CREATE
overlay := stores.NewStoreOverlayForCreate(req.Object)
```

**Solution**: Switch on the admission verb the way `createOverlay` does.

```go
// Good
switch operation {
case "CREATE": overlay = stores.NewStoreOverlayForCreate(obj)
case "UPDATE": overlay = stores.NewStoreOverlayForUpdate(obj)
case "DELETE": overlay = stores.NewStoreOverlayForDelete(namespace, name)
}
```

## Embedded validationTests are NOT run here

The DryRunValidator validates the *submitted* resource (Ingress/HTTPRoute/etc.)
by rendering with an overlay store and checking the result. It does **not** run
the chart's embedded `validationTests`: those are chart-author scenarios with
their own fixtures (often referencing secrets/ingresses that exist only in the
fixture set, not the live cluster), so running them per-admission would both
waste work and surface fixture-vs-cluster mismatches as admission denials. The
`validationTests` are executed instead by `haptic validate` (CLI /
CI) and the `make test-templates` target, via `pkg/controller/testrunner`.

## Future Enhancements

### Parallel Validation

Each admission request runs serially through `proposalvalidator.ValidateSync`.
Webhook calls are independent, so the proposal pipeline could fan them out:

```go
// Speculative -- not implemented.
var wg sync.WaitGroup
results := make(chan *ValidationResult, len(requests))

for _, req := range requests {
    wg.Add(1)
    go func(r *Request) {
        defer wg.Done()
        results <- c.validate(r)
    }(req)
}

wg.Wait()
close(results)
```

### Verdict reuse

Every admission request executes built-in and matching protocol-v1 external
validation. A content hash cannot prove that the validator executable or
runtime environment is unchanged. Future reuse requires an authenticated
hermetic-environment root bound to the exact rendered output.

## Resources

- Overlay primitives: `pkg/stores/README.md`
- Render-validate pipeline (delegated to): `pkg/controller/proposalvalidator/README.md`
- Error simplification: `pkg/dataplane/CLAUDE.md`
- Event-driven patterns: `pkg/controller/CLAUDE.md`
- Webhook integration: `pkg/controller/webhook/CLAUDE.md`
