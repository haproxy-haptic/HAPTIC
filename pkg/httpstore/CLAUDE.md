# pkg/httpstore - HTTP Resource Store

Development context for the pure HTTP resource store component.

## When to Work Here

Work in this package when:

- Modifying HTTP fetching logic (retries, timeouts, authentication)
- Changing the two-version caching behavior (pending/accepted)
- Adding new authentication methods
- Modifying conditional request handling (ETag, If-Modified-Since)

**DO NOT** work here for:

- Event coordination (refresh timers, validation events) → Use `pkg/controller/httpstore`
- Template integration → Use `pkg/controller/renderer`
- Reconciliation triggers → Use `pkg/controller/reconciler`

## Package Purpose

Pure HTTP resource store with two-version caching for safe content updates. Provides the core fetching and caching logic without event bus dependencies.

This is a **pure component** following the codebase's architecture pattern - it has no knowledge of the event bus and can be used independently for testing.

## Two-Version Cache Pattern

The shared store maintains two content versions for each URL. An initial authoritative response stays in a separate render-local `InitialCandidate` until the complete pipeline validates it:

```
         ┌──────────────────────────────────────────────────┐
         │                    CacheEntry                     │
         │                                                   │
         │  AcceptedContent ◄── Used for production render   │
         │  AcceptedChecksum                                 │
         │  AcceptedTime                                     │
         │                                                   │
         │  PendingContent  ◄── New content from refresh     │
         │  PendingChecksum                                  │
         │  HasPending: true                                 │
         └──────────────────────────────────────────────────┘

Validation Success: PromotePendingVersion()
   └── PendingContent → AcceptedContent
       HasPending = false

Validation Failure: RejectPendingVersion()
   └── PendingContent discarded
       HasPending = false
       AcceptedContent preserved
```

This pattern ensures that invalid HTTP content (e.g., malformed IP blocklist) never breaks the HAProxy configuration.

## Key Methods

### Fetching

```go
// Establish source authority, then fetch a render-local initial candidate.
source, err := store.ReconcileSource(url, FetchOptions{
    Timeout:  30 * time.Second,
    Retries:  3,
    Critical: true,  // Return error if fetch fails
    Delay:    5 * time.Minute,  // For periodic refresh
}, &AuthConfig{
    Type:     "bearer",
    Token:    "secret",
})
content, candidate, err := store.PrepareInitial(ctx, url, source.State)

// After the exact complete rendered output passes every validator, accept all
// candidates atomically. Context cancellation or one stale token accepts none.
err = store.CommitInitialCandidates(ctx, []*InitialCandidate{candidate})

// Refresh (stores in pending, returns true if content changed)
changed, err := store.RefreshURL(ctx, url)

// Refresh with exact-version ownership for asynchronous validation
version, err := store.RefreshURLVersion(ctx, url)
```

`Fetch` immediately accepts a first response and is reserved for isolated stores, such as the read-only store owned by one validation render. Controller-owned authoritative state uses the candidate methods above.

### Cache Access

```go
// Get accepted content (for production render)
content, ok := store.Get(url)

// Get pending or accepted (for validation render)
content, ok := store.GetForValidation(url)

// Get all URLs with pending content
urls := store.GetPendingURLs()
```

### Validation Lifecycle

```go
// After successful validation - promote only the version that was validated
promoted := store.PromotePendingVersion(url, version.Checksum, version.Revision)

// After failed validation - reject only the version that was validated
rejected := store.RejectPendingVersion(url, version.Checksum, version.Revision)
```

### Reversible Acceptance (ADR-0030)

Every acceptance (initial candidate commit or promotion) enters a ledger with the version it replaced, outside the authenticated publication state:

```go
seq := store.AcceptanceSequence()     // acceptances so far
store.ConfirmAcceptances(observations) // exact source-version tokens from a passing render
revoked := store.RevokeAcceptances(seq) // the gate refused one: restore the previous version, or none
```

A revocation records a semantic change and clears ETag/Last-Modified, so renders see it and the next refresh fetches unconditionally.

### Blob sources (ADR-0041)

A source whose `FetchOptions.Blob` is declared streams its body into the store's `BlobSink`
(`SetBlobSink`, the pure `pkg/blobstore`) instead of reading it into memory, so the 10 MiB
`MaxContentSize` does not apply. Its content is the encoded `blob.Ref` (`"<sha256> <size>"`),
which moves through pending, accepted, and revocation like any text content. A store without a
sink refuses blob sources before sending a request (`ErrBlobsDisabled`). Refused blobs
(`blob.ErrRefused`) are not retried. `BlobSources()` lists the accepted, pending, and replaced
digests for retention; `RevokedContent.BlobDigest` names a revoked blob. `Blob` is a `blob.Key`,
the spec in one string like `StatusSet`, so `FetchOptions` stays small and comparable; it is
tagged `omitempty`, so text sources keep their source identity.

### Test Fixtures

```go
// Pre-load content for validation tests (no HTTP request)
store.LoadFixture("http://example.com/data.txt", "mock content")

// Content is immediately available as accepted
content, ok := store.Get("http://example.com/data.txt")
// content = "mock content", ok = true
```

Used by `pkg/controller/testrunner` to mock HTTP resources in validation tests.

## Accepted Statuses

`FetchOptions.AcceptStatus` is a `StatusSet` (one sorted string, so `FetchOptions`
stays comparable and canonical). Build it with `NewStatusSet(codes...)`, which
rejects anything outside 100–599 and 304 (the conditional-refresh answer) and
drops 200, which is always accepted. A listed status's body is content exactly
like a 200's; every other status keeps its error. The set is part of the source
identity, so changing it is a new declaration.

## Authentication

Three authentication methods are supported:

```go
// Basic authentication
auth := &AuthConfig{
    Type:     "basic",
    Username: "user",
    Password: "pass",
}

// Bearer token
auth := &AuthConfig{
    Type:  "bearer",
    Token: "secret-token",
}

// Custom headers
auth := &AuthConfig{
    Type: "header",
    Headers: map[string]string{
        "X-API-Key": "my-key",
    },
}
```

## Conditional Requests

The store automatically uses conditional requests when refreshing:

- Stores `ETag` and `Last-Modified` headers from responses
- Sends `If-None-Match` and `If-Modified-Since` on refresh
- Returns `changed=false` on 304 Not Modified responses

This minimizes bandwidth usage for frequently-refreshed resources.

## Cache Eviction

The HTTP store automatically evicts cache entries that haven't been accessed for a configurable duration. This prevents memory growth when templates change and old URLs are no longer used.

### Eviction Rules

1. **Access time tracking**: Every `Get()` and `GetForValidation()` call (plus the cache-hit branch of `Fetch()`) updates the entry's `LastAccessTime`. There is no exported `GetPending()` method — listing pending URLs is done via `GetPendingURLs()`, which is a snapshot and does *not* touch access time.
2. **Never evict pending**: Entries with pending validation (`HasPending=true`) are never evicted, even if expired
3. **Configurable maxAge**: Set via constructor parameter, typically 2x the drift prevention interval
4. **Periodic cleanup**: The event adapter runs eviction at regular intervals

### Default Behavior

With default drift prevention interval of 60s:

- `maxAge` = 2 minutes (2x drift interval)
- Eviction runs every 2 minutes
- Entries unused for 2+ minutes are evicted (unless they have pending content)

### Eviction API

```go
// Create store with 2-minute eviction
store := New(logger, 2*time.Minute)

// Manually trigger eviction (called periodically by event adapter).
// Returns the URLs that were evicted (nil/empty when none evicted or
// when eviction is disabled), so callers can react to specific URLs
// disappearing — e.g. cancel pending refresh timers.
evicted := store.EvictUnused() // []string

// Disable eviction (e.g. for test fixtures); EvictUnused() then returns nil.
store := New(logger, 0)
```

## Common Pitfalls

### Non-Critical Fetch Returns Empty String

**Problem**: Fetch returns empty string without error.

**Solution**: Set `Critical: true` if you need to fail on fetch errors.

```go
// Bad - silently returns empty on failure
content, err := store.Fetch(ctx, url, FetchOptions{}, nil)
// err is nil, content is ""

// Good - returns error on failure
content, err := store.Fetch(ctx, url, FetchOptions{Critical: true}, nil)
// err contains the actual error
```

### Forgetting to Promote/Reject Pending

**Problem**: Pending content stays in pending state forever.

**Solution**: Capture the `PendingVersion` returned by `RefreshURLVersion()` and
finalize that exact version with `PromotePendingVersion()` or
`RejectPendingVersion()`.

### Using Get() During Validation Render

**Problem**: Validation render uses old content, passes, but production uses different content.

**Solution**: Use `GetForValidation()` during validation render to see pending content.

## Integration with Event Adapter

The event adapter (`pkg/controller/httpstore`) wraps this pure component:

```go
import (
    purehttpstore "github.com/haproxy-haptic/HAPTIC/pkg/httpstore"
    httpstore    "github.com/haproxy-haptic/HAPTIC/pkg/controller/httpstore"
)

// Pure component (this package). maxAge=0 disables eviction.
store := purehttpstore.New(logger, 2*time.Minute)

// Event adapter — different package, same name, different signature.
component := httpstore.New(eventBus, logger, 2*time.Minute)

// Only live reconciliation uses SourceModeAuthoritative. It returns a generic
// render-input transaction that the pipeline commits after full validation.
wrapper := httpstore.NewHTTPStoreWrapper(ctx, component, logger, overlay, httpstore.SourceModeReadOnly)
```

## Testing

Unit tests are in `store_test.go` and use `httptest.NewServer` for HTTP mocking.

```bash
# Run tests
go test ./pkg/httpstore/... -v

# Run specific test
go test ./pkg/httpstore/... -v -run TestHTTPStore_FetchAndGet
```

## Resources

- Event adapter: `pkg/controller/httpstore/CLAUDE.md`
- Architecture: `/docs/site/docs/development/design.md`
- Controller integration: `pkg/controller/CLAUDE.md`
