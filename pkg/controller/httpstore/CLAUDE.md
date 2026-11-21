# pkg/controller/httpstore - HTTP Store Event Adapter

Development context for the HTTP store event adapter component.

## When to Work Here

Work in this package when:

- Modifying refresh timer behavior
- Changing validation event handling (promote/reject)
- Updating the template-callable wrapper interface
- Adding new event types for HTTP resources

**DO NOT** work here for:

- Core HTTP fetching logic → Use `pkg/httpstore`
- Template rendering → Use `pkg/controller/renderer`
- Reconciliation triggers → Use `pkg/controller/reconciler`

## Package Purpose

Event adapter wrapping the pure HTTP store (`pkg/httpstore`) with event bus coordination. This is a **Stage 5 component** that runs on all replicas.

Responsibilities:

- Manages periodic refresh timers for URLs with `delay > 0`
- Listens for validation events to promote/reject pending content
- Publishes HTTP resource events when content changes
- Provides template-callable wrapper for `http.Fetch()`
- Owns render-local candidate transactions for cold and replaced sources
- Periodic eviction of unused cache entries to prevent memory growth

## Architecture

```
Template calls http.Fetch()
        │
        ▼
┌─────────────────────────────────────────────────────────────┐
│               HTTPStoreWrapper (wrapper.go)                  │
│   - Callable from templates                                  │
│   - Delegates to pure HTTPStore                              │
│   - Owns one render-local InputTransaction                   │
└─────────────────────────────────────────────────────────────┘
        │
        ▼
┌─────────────────────────────────────────────────────────────┐
│                 Component (component.go)                     │
│   - Manages refresh timers                                   │
│   - Atomically commits validated initial candidate sets      │
│   - Publishes ProposalValidationRequestedEvent on refresh    │
│   - Subscribes to ProposalValidationCompletedEvent           │
│     (branches on event.Valid → promote or reject)            │
│   - Publishes HTTPResourceUpdated/AcceptedEvent              │
└─────────────────────────────────────────────────────────────┘
        │
        ▼
┌─────────────────────────────────────────────────────────────┐
│             pkg/httpstore.HTTPStore (pure)                   │
│   - HTTP fetching with retries                               │
│   - Render-local initial candidates                          │
│   - Two-version cache (pending/accepted)                     │
│   - Conditional requests (ETag)                              │
└─────────────────────────────────────────────────────────────┘
```

## Event Flow

### On Content Refresh

```
Timer expires
    │
    ▼
Component.refreshURL()
    │
    ├── Content unchanged (304 Not Modified)
    │   └── Reset timer, no event
    │
    └── Content changed
        ├── Store in pending
        ├── triggerProposalValidation(url):
        │     ├── Start an immutable validation batch, or queue behind the active one
        │     ├── Publish ProposalValidationRequestedEvent for a new batch
        │     └── Publish HTTPResourceUpdatedEvent (observability sibling)
        └── Reset timer
```

### On Proposal Validation Complete

The component subscribes to a *single* event — `ProposalValidationCompletedEvent`
— and branches on `event.Valid`. There is no separate "validation failed"
subscription:

```
ProposalValidationCompletedEvent received
    │  (dropped if event.RequestID doesn't match the active validation batch)
    ▼
event.Valid?
    │
    ├── true  → handleValidationSuccess()
    │           For each URL/checksum in that batch:
    │             ├── PromotePendingVersion() - pending → accepted
    │             └── Publish HTTPResourceAcceptedEvent
    │           Then publish ReconciliationTriggeredEvent("http_content_validated")
    │
    └── false → handleValidationFailure(event.Phase, event.Error)
                For each URL/checksum in that batch:
                  └── RejectPendingVersion() - discard pending and log diagnostics

Any pending URL outside the completed batch starts the next validation request.
```

## Template Usage

The `HTTPStoreWrapper` provides a `Fetch()` method callable from templates:

```scriggo
{# Basic fetch — Scriggo declares variables with {% var %} (or {% x := y %}),
   not Jinja's {% set %}. #}
{% var content = http.Fetch("https://example.com/blocklist.txt") %}

{# With refresh interval #}
{% var content = http.Fetch("https://api.example.com/data", {"interval": "5m"}) %}

{# With authentication. There is no top-level `secrets` variable — read the
   Secret like any other watched resource via the `resources` map. The token
   value comes back base64-encoded from the API server. #}
{% var apiSecret = resources.secrets.GetSingle("kube-system", "my-api-secret") %}
{% var token = apiSecret | dig("data", "token") | b64decode %}
{% var content = http.Fetch("https://api.example.com/protected",
    {"interval": "10m"},
    {"type": "bearer", "token": token}
) %}

{# With all options #}
{% var ips = http.Fetch("https://blocklist.example.com/ips.txt",
    {"interval": "1h", "timeout": "30s", "retries": 3, "critical": true},
    {"type": "basic", "username": "user", "password": "pass"}
) %}
```

`acceptStatus` (a list of 100–599, not 304) makes those statuses' bodies
content, for servers that answer with the status they describe. 200 is always
accepted. `parseFetchOptions` validates it via `httpstore.NewStatusSet`; the
incremental canonicalizer (`pkg/templating/incremental_http.go`) and the
validationTest fixture wrapper (`pkg/controller/testrunner`, through
`ParseFetchArgs`) reject the same invalid values, so a bad list fails a
validationTest as it fails production.

### Source authority and overlays

Accepting reconcile pipelines use `WithDeferredCandidates`: a discovering render
aborts, releases the sole cold-render slot, and waits for HTTP outside the renderer.
The next attempt adopts the exact staged source and candidate through
`InputRetrySeed`; descriptor, publication, and lease fences remain required.
Deployment renders withhold candidates instead, and admission keeps read-only
fetches under its request deadline. See ADR-0030.

`NewHTTPStoreWrapper` takes an explicit `SourceMode`. `SourceModeAuthoritative`
is reserved for live reconciliation: it reconciles the shared declaration and
returns matching accepted content or a render-local initial candidate. The
renderer passes the wrapper's transaction to the pipeline. Only a successful
complete validation commits all candidates atomically and arms their timers;
every error or cancellation aborts them. `SourceModeReadOnly` is used for
admission validation and source-map introspection. It reads a matching pending
overlay first, then matching accepted content, and fetches a miss into a store
owned by that render. Read-only wrappers never replace shared source or timer
state and have no input transaction.

The HTTP overlay is independent of source authority. It selects the exact
pending bytes a periodic HTTP refresh asks the proposal pipeline to validate. A pending
overlay from another source identity fails the render instead of mixing source
declarations. Initial candidates never enter this asynchronous pending flow.

### Unavailable critical sources (`critical_outage.go`)

A critical source without accepted content whose fetch fails is recorded as an
outage and refetched outside any render (its `interval`, else
`DefaultOutageRetryInterval`). Authoritative renders read the recorded failure
instead of fetching. The reconcile pipeline brackets each render with
`BeginReconcileRender` / `HoldReconcileRenders` / `ReconcileRenderCompleted`:
after a render fails on an outage, reconcile renders return that failure
without rendering until the outage epoch moves (ADR-0030, issue #332).
Input isolation checks `Pipeline.HeldRender` before capturing inputs (#354).

## Component Lifecycle

```go
// Created during controller Stage 5
// evictionMaxAge is typically 2x drift prevention interval
driftInterval := cfg.Dataplane.GetDriftPreventionInterval()
httpStoreEvictionMaxAge := 2 * driftInterval
httpStoreComponent := httpstore.New(eventBus, logger, httpStoreEvictionMaxAge)

// HTTPStoreComponent is passed to RenderService via the config struct.
// renderService := renderer.NewRenderService(&renderer.RenderServiceConfig{
//     ...
//     HTTPStoreComponent: httpStoreComponent,
// })

// Started as all-replica component
go httpStoreComponent.Start(ctx)
```

The eviction interval runs at the same cadence as `evictionMaxAge`. Entries not accessed within `evictionMaxAge` are evicted (unless they have pending validation content).

## Event Types

Published events (defined in `pkg/controller/events/`):

| Event | When | Purpose |
|-------|------|---------|
| `ProposalValidationRequestedEvent` | Refresh produced new content; before promoting it | Asks the proposal pipeline to validate one immutable pending-content batch and records its request ID and URL checksums |
| `HTTPResourceUpdatedEvent` | Same call as above — sibling event for observability | Lets `commentator` / metrics see that content changed without subscribing to validation events |
| `HTTPContentRejectedEvent` | Validation rejected the exact pending URL version | Warning Event on the active HAProxyTemplateConfig and a rejection counter; saved content stays active |
| `HTTPResourceAcceptedEvent` | After a matching `ProposalValidationCompletedEvent` with `Valid == true` | Observability that pending → accepted promotion happened |
| `ReconciliationTriggeredEvent("http_content_validated", true)` | After a successful promotion (in `handleValidationSuccess`) | Coalescible reconciliation request so HAProxy picks up the new content |
| `HTTPContentAcceptanceRequestedEvent` | A deploying render (`WithCandidatesWithheld`) left out an unaccepted source | The leader's coordinator runs one acceptance attempt beside the reconcile loop (ADR-0030) |
| `BlobContentRevokedEvent` | `RevokeAcceptances` took back a blob source's version (instead of `HTTPContentRevokedEvent`) | `BlobContentRevoked` Warning Event and `haptic_blob_content_revoked_total` |
| `BlobFetchFailedEvent` | A blob source's fetch failed after its retries (`EnableBlobs` hook) | `haptic_blob_fetch_errors_total` |
| `HTTPContentRevokedEvent` + `ReconciliationTriggeredEvent("http_content_revoked", true)` | `RevokeAcceptances` took back content after the render gate refused a render that read it | Warning Event, metric, and the render without the content |
| `ReconciliationTriggeredEvent("critical_http_content_available", true)` | The refetch of an unavailable critical source succeeded | Resumes held renders; the next accepting render adopts the refetched candidate |
| `ReconciliationTriggeredEvent("critical_http_content_recheck", true)` | The refetch failed after held renders were skipped | One render re-checks whether the skipped inputs still need the source |

Subscribed events:

| Event | Action |
|-------|--------|
| `ProposalValidationCompletedEvent` | Match the active batch; promote or reject only URL versions with the checksums that batch validated |

## Common Pitfalls

### Timer Leak on Shutdown

**Problem**: Refresh timers keep running after context cancelled.

**Solution**: `stopAllRefreshers()` is called in the event loop's `ctx.Done()` case.

### Missing URL Registration

**Problem**: URL fetched but never refreshes.

**Solution**: `RegisterURL()` must be called after successful fetch. The wrapper does this automatically when `delay > 0`.

### Validation Event Race

**Problem**: Validation event arrives before refresh completes.

**Solution**: Store uses mutex protection. Pending content is only promoted/rejected if it exists.

## Testing

The component uses the EventBus for all coordination, making it easy to test:

```go
// Create components
bus := events.NewEventBus(100)
component := httpstore.New(bus, logger, 2*time.Minute) // eviction maxAge

// Start component
go component.Start(ctx)

// Simulate proposal validation completion
bus.Publish(events.NewProposalValidationCompletedEvent(...))

// Verify accepted content is now available
```

## Resources

- Pure store: `pkg/httpstore/CLAUDE.md`
- Events catalog: `pkg/controller/events/types.go`
- Controller startup: `pkg/controller/controller.go`
