# pkg/k8s

Pure Kubernetes integration library. Two watchers, two stores, a JSONPath indexer, and a thin client-go wrapper. No EventBus dependency — the controller wraps this package in event adapters (`pkg/controller/resourcewatcher`) when it wants event-driven behaviour.

Module path: `github.com/haproxy-haptic/HAPTIC`. The authoritative interface is the source (`go doc ./pkg/k8s/...`); this README is a short orientation.

## Sub-Package Map

| Purpose | Package |
|---------|---------|
| Core interfaces (`Store`, `WatcherConfig`, `SingleWatcherConfig`, `ChangeStats`) | `types` |
| client-go wrapper with in-cluster / out-of-cluster autodetection | `client` |
| JSONPath key extraction + metadata trimming | `indexer` |
| `MemoryStore` (full objects, O(1) composite-key lookup) and `CachedStore` (reference + TTL cache, API-backed fetch) | `store` |
| `Watcher` (bulk, debounced) and `SingleWatcher` (one named resource, immediate) | `watcher` |
| `configpublisher` helper used by the controller to publish `HAProxyCfg` / `HAProxyGeneralFile` / `HAProxyMapFile` / `HAProxyCRTListFile` CRDs | `configpublisher` |
| Pure leader-election component (used by `pkg/controller/leaderelection`) | `leaderelection` |

## Two Watchers

**`watcher.Watcher`** — for collections (Ingress, Service, EndpointSlice…). Watches via `SharedInformerFactory`, indexes by JSONPath into a `types.Store`, debounces bursts before calling `OnChange`.

```go
import (
    "github.com/haproxy-haptic/HAPTIC/pkg/k8s/client"
    "github.com/haproxy-haptic/HAPTIC/pkg/k8s/types"
    "github.com/haproxy-haptic/HAPTIC/pkg/k8s/watcher"

    "k8s.io/apimachinery/pkg/runtime/schema"
)

k8sClient, err := client.New(client.Config{})
if err != nil { /* ... */ }

cfg := types.WatcherConfig{
    GVR: schema.GroupVersionResource{
        Group: "networking.k8s.io", Version: "v1", Resource: "ingresses",
    },
    IndexBy:      []string{"metadata.namespace", "metadata.name"},
    IgnoreFields: []string{"metadata.managedFields"},
    OnChange: func(store types.Store, stats types.ChangeStats) {
        if stats.IsInitialSync { return }
        // react to real-time change…
    },
    OnSyncComplete: func(store types.Store, initialCount int) {
        // initial bulk load finished
    },
}

w, err := watcher.New(cfg, k8sClient, logger)
if err != nil { /* ... */ }
go w.Start(ctx)                                 // Start blocks; run in its own goroutine.
if _, err := w.WaitForSync(ctx); err != nil { /* ... */ }
```

Key `WatcherConfig` fields (see `pkg/k8s/types/types.go` for full docs):

- `IndexBy` — JSONPath expressions that form the composite lookup key. Order matters: `.Get(partial…)` does a prefix scan when fewer keys are supplied.
- `Namespaces` — empty watches all namespaces. A list runs one informer per namespace, all feeding the same store and debouncer, so the watcher never lists or watches outside it; `FreshStore` lists the same namespaces.
- `StoreType` — `StoreTypeMemory` (default) or `StoreTypeCached`. `CacheTTL` only applies to the cached store and defaults to ~2 min (auto-derived from drift prevention at the controller layer).
- `DebounceInterval` — default `types.DefaultDebounceInterval` (100ms). Set to 0 for default, or a positive duration to override.
- `CallOnChangeDuringSync` — when true, `OnChange` fires during initial bulk load with `stats.IsInitialSync == true`; when false, only `OnSyncComplete` fires for the initial set.
- `SelfWrites` — optional `types.SelfWriteFilter` (a `*types.SelfWriteRegistry` fed by whatever component writes to the watched objects). An update whose resourceVersion the filter recognises still refreshes the store but doesn't count as a change, so an echo of the controller's own status write never triggers `OnChange`.
- `LabelSelector` / `FieldSelector` — `LabelSelector` is a server-side filter (`*metav1.LabelSelector`, pushed to the API server). `FieldSelector` is a client-side JSONPath filter (evaluated after the list is fetched); it supports any expression but fetches more data than `LabelSelector`. Two earlier conversions land its value: the CRD's `labelSelector` is a *string* like `"app=foo,env=prod"` (see `pkg/apis/haproxytemplate/v1alpha1/types_config.go`), `pkg/controller/conversion.parseLabelSelector` parses it with the Kubernetes label-selector grammar into a `map[string]string` on the internal `config.WatchedResource`, and `pkg/controller/resourcewatcher/watcher.go:134` finally wraps that map as `&metav1.LabelSelector{MatchLabels: m}` for this layer. Anything that grammar cannot express as equality pairs — set-based syntax, `!=` — is **rejected at config load**, not dropped; a dropped selector would widen the watch to every object of that kind. An invalid `*metav1.LabelSelector` fails `WatcherConfig.Validate`, so `watcher.New` refuses to start rather than listing unfiltered. For richer selectors (`MatchExpressions`) construct the `*metav1.LabelSelector` directly when bypassing the CRD path.

**`watcher.SingleWatcher`** — for one specific named resource (the controller's own `HAProxyTemplateConfig`, its credentials `Secret`, etc.). No indexing, no debounce, immediate callbacks.

```go
cfg := &types.SingleWatcherConfig{
    GVR:       schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"},
    Namespace: "haptic",
    Name:      "haptic-config",
    // OnChange is types.OnResourceChangeCallback — `func(obj any) error`,
    // distinct from the bulk watcher's `func(types.Store, types.ChangeStats)`.
    // Returning an error logs it and keeps the watcher alive.
    OnChange:  func(obj any) error { return nil },
}
w, err := watcher.NewSingle(cfg, k8sClient)
```

## Two Stores

| | `StoreTypeMemory` (`MemoryStore`) | `StoreTypeCached` (`CachedStore`) |
|--|----|----|
| What's resident | Full objects | Index keys only |
| Lookup | In-memory map | TTL cache + API fallback on miss |
| Good for | Anything templates iterate over | Large, rarely-touched resources (TLS Secrets) |
| `.List()` cost | O(n) in-memory | O(n) **API fetch** — avoid |
| `.Get(keys...)` | O(1) | O(1) on cache hit, API call on miss |

Both implement `types.Store`. `Get(keys...)` with fewer keys than `IndexBy` returns all resources whose composite key starts with those keys (prefix scan) — useful for one-to-many lookups like "all EndpointSlices for service X". Each store owns a reverse `(namespace, name)` index, so an informer update that changes an `IndexBy` value moves the resource atomically instead of leaving it in both lookup buckets.

## Initial Sync

Every watcher distinguishes the initial bulk load from live changes. Call `WaitForSync(ctx)` before the first reconciliation; use `stats.IsInitialSync` inside `OnChange` to skip premature work. `pkg/controller/indextracker` coordinates this across all watchers so the reconciliation pipeline only starts after every store is populated.

## Common Pitfalls

- **Not waiting for sync.** Calling `store.List()` before `WaitForSync` returns can miss pre-existing resources.
- **Field selectors on labels.** `"metadata.labels['app']=x"` is not valid — use `LabelSelector` instead.
- **Long work in callbacks.** Callbacks run on the watcher's informer goroutine. Hand work off to a channel / goroutine; don't block.
- **`List()` on a cached store.** Force-fetches every reference. Use `Get()` with the exact composite key instead. (In templates, use `resources.X.GetSingle()` / `resources.X.Fetch()` from the render-context layer.)
- **Invalid JSONPath.** `indexer.ValidateJSONPath()` is called at watcher construction — invalid expressions fail fast with the field name in the error.

See `pkg/k8s/CLAUDE.md` for the detailed pitfall catalogue, testing patterns (fake clientset, initial-sync assertions), and the walkthrough for adding a new watched resource type.

## Testing

```bash
go test ./pkg/k8s/...           # unit tests
go test ./pkg/k8s/... -race     # race detector
```

Unit tests use `k8s.io/client-go/kubernetes/fake`; integration tests live under `tests/`.

## See Also

- `pkg/k8s/CLAUDE.md` — developer context, adding-a-resource walkthrough, pitfalls in depth
- `pkg/k8s/leaderelection/README.md` — pure leader election component
- `pkg/controller/resourcewatcher` — the event adapter that wires `Watcher`s into the EventBus
- `docs/site/docs/watching-resources.md` — user-facing documentation for the CRD side

## License

Apache-2.0 — see root `LICENSE`.
