# pkg/k8s - Kubernetes Integration

Development context for Kubernetes resource watching and indexing.

**API Documentation**: See `pkg/k8s/README.md`
**Architecture**: See `/docs/site/docs/development/design.md` (design documentation index)

## When to Work Here

Modify this package when:

- Adding support for new Kubernetes resource types
- Changing indexing or storage strategies
- Modifying watcher behavior (debouncing, callbacks)
- Fixing Kubernetes client issues
- Adding new JSONPath expressions

**DO NOT** modify this package for:

- Event type definitions → Use `pkg/controller/events`
- Controller coordination → Use `pkg/controller`
- Template rendering → Use `pkg/templating`
- HAProxy sync → Use `pkg/dataplane`

## Package Structure

```
pkg/k8s/
├── types/             # Core interfaces (Store, WatcherConfig)
├── client/            # Kubernetes client wrapper
├── indexer/           # JSONPath evaluation and field filtering
├── store/             # MemoryStore and CachedStore implementations
├── watcher/           # Resource watching (Watcher and SingleWatcher)
├── configpublisher/   # Publishes rendered config + aux files as observable CRDs
│                      # (HAProxyCfg, HAProxyMapFile, HAProxyGeneralFile,
│                      # HAProxyCRTListFile) plus SSL Secrets — for
│                      # `kubectl describe`, GitOps diffs, and audit trails.
│                      # Does NOT push to HAProxy pods; that's pkg/dataplane.
├── leaderelection/    # Pure leader election component (no events)
├── typegen/           # OpenAPI v3 schema → reflect.Type conversion for the
│                      # template engine's typed watched-resource globals.
│                      # `converter.go::goFieldName` is the canonical
│                      # Go-PascalCase rule (no acronym preservation).
│                      # See ADR-0010 for design rationale.
└── schemafetcher/     # Schema sources for typegen: ClusterFetcher (live
                       # kube-apiserver), DirFetcher (offline --schema-dir
                       # with CRD-wrapped YAML or bare OpenAPI v3 +
                       # x-kubernetes-group-version-kind), MapFetcher (in
                       # memory, tests). No embedded fallback in the
                       # binary — see ADR-0010 Alternative 3.
```

## Key Concepts

### Two Watcher Types

**Watcher**: For bulk resource watching (Ingress, Service, EndpointSlice)

- Watches collections of resources
- Debounces rapid changes
- Indexed storage for fast lookups
- Tracks initial sync state

**SingleWatcher**: For single resource watching (ConfigMap, Secret)

- Watches one specific resource (namespace + name)
- Immediate callbacks (no debouncing)
- No indexing or store overhead
- Ideal for configuration and credentials

### Store Types

**MemoryStore**: Fast in-memory storage

- Complete resources stored in memory
- O(1) lookups via composite index keys
- Default choice for most resources
- Memory usage: ~1KB per resource

**CachedStore**: API-backed with TTL cache

- Only caches recently accessed resources
- Falls back to Kubernetes API for cache misses
- Use for large resources (Secrets, ConfigMaps with big data)
- Memory usage: Bounded by cache size

### Initial Sync Handling

Distinguishes between initial bulk load and real-time changes:

```go
// Watcher tracks sync state
w.IsSynced()  // Returns bool

// Callbacks receive context
func onChange(store Store, stats ChangeStats) {
    if stats.IsInitialSync {
        // Resource exists at startup, don't trigger reconciliation yet
        log.Debug("resource loaded", "count", stats.Created)
    } else {
        // Real-time change, trigger reconciliation
        log.Info("resource changed", "created", stats.Created)
        triggerReconciliation()
    }
}
```

## Sub-Package Guidelines

### types/ - Core Interfaces

Defines interfaces used across k8s package:

```go
// Store interface for indexed resource storage (declared in pkg/k8s/types/types.go)
type Store interface {
    Get(keys ...string) ([]any, error)
    List() ([]any, error)
    Add(resource any, keys []string) error
    Update(resource any, keys []string) error
    Delete(namespace, name string, keys []string) error
    Clear() error
}

// Watcher configuration
type WatcherConfig struct {
    GVR           schema.GroupVersionResource
    Namespaces    []string
    LabelSelector *metav1.LabelSelector
    IndexBy       []string
    StoreType     StoreType
    Callbacks     ...
}
```

**When to modify:**

- Adding new Store method
- Changing watcher configuration options
- Adding new callback types

### client/ - Kubernetes Client Wrapper

Wraps client-go for simplified usage:

```go
// Auto-detects in-cluster vs out-of-cluster
client, err := client.New(client.Config{})

// Provides both typed and dynamic clients
typedClient := client.Clientset()
dynamicClient := client.DynamicClient()
```

**Common tasks:**

Adding custom client configuration:

```go
// client/client.go
type Config struct {
    Kubeconfig string  // empty = in-cluster
    Namespace  string  // empty = auto-detect from SA token
    QPS        float32 // <= 0 (default) disables client-side throttling → APF
    Burst      int     // used only when QPS > 0; 0 → 2*QPS
}

// New is the only constructor; NewWithConfig does not exist.
// QPS <= 0 sets rest.Config.QPS = -1 (rely on apiserver Priority & Fairness);
// a positive QPS installs one shared RateLimiter across every derived clientset.
func New(cfg Config) (*Client, error) { ... }
```

### indexer/ - JSONPath and Field Filtering

Extracts index keys and filters unnecessary fields:

```go
// Create an indexer (validates expressions at construction time)
idx, err := indexer.New(indexer.Config{
    IndexBy: []string{
        "metadata.namespace",
        "metadata.labels['app']",
    },
    IgnoreFields: []string{
        "metadata.managedFields",
        "metadata.annotations['kubectl.kubernetes.io/last-applied-configuration']",
    },
})

// Extract index keys — method on *Indexer, resource arg only
keys, err := idx.ExtractKeys(resource)
// Result: ["default", "myapp"]

// Remove unnecessary fields — mutates in place, returns error
err = idx.FilterFields(resource)
```

**JSONPath Validation:**

```go
// Validates at startup (fail-fast)
err := indexer.ValidateJSONPath("metadata.namespace")  // OK
err := indexer.ValidateJSONPath("invalid..path")       // Error
```

**When to modify:**

- Supporting new JSONPath expressions
- Optimizing field filtering
- Adding field validation

**Common pitfall**: JSONPath expressions are case-sensitive and must match exact field names.

### store/ - Storage Implementations

**MemoryStore** - Default choice. Constructor takes the number of index keys
the indexer will produce — the store uses it to validate every Add/Update call.

```go
// Two index keys (e.g. namespace + name)
s := store.NewMemoryStore(2)

s.Add(resource, []string{"default", "myapp"})

// O(1) lookup using any prefix of the configured key set
all, _ := s.Get("default")            // All resources in namespace
single, _ := s.Get("default", "myapp") // Specific resource
```

**CachedStore** - For large resources. Construct via the config struct, not a
positional constructor:

```go
s, err := store.NewCachedStore(&store.CachedStoreConfig{
    NumKeys:      2,
    CacheTTL:     10 * time.Minute, // optional; default 2m10s
    MaxCacheSize: 100,              // optional; default 256 (bounded LRU)
    Client:       dynamicClient,
    GVR:          schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"},
    Indexer:      idx,
})

// Rendering paths use GetContext so cancellation stops an in-flight API fetch.
resource, _ := s.GetContext(ctx, "default", "my-secret")
```

**When to use CachedStore:**

- Resources with large data fields (Secrets with certificates)
- Resources accessed infrequently
- Memory pressure from many resources

**When to use MemoryStore:**

- Most resources (Ingress, Service, EndpointSlice)
- Resources accessed frequently
- Need for fast iteration (List() operations)

### watcher/ - Resource Watching

**Watcher** - Bulk resource watching. Configs live in `pkg/k8s/types`; constructors
in `pkg/k8s/watcher`. `LabelSelector` is a real `*metav1.LabelSelector`, not the
serialized string form.

```go
import (
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "github.com/haproxy-haptic/HAPTIC/pkg/k8s/types"
    "github.com/haproxy-haptic/HAPTIC/pkg/k8s/watcher"
)

cfg := types.WatcherConfig{
    GVR: schema.GroupVersionResource{
        Group: "networking.k8s.io", Version: "v1", Resource: "ingresses",
    },
    Namespaces: nil, // nil = all namespaces; one informer per listed namespace (NamespacedWatch overrides this)
    LabelSelector: &metav1.LabelSelector{
        MatchLabels: map[string]string{"app": "myapp"},
    },
    IndexBy: []string{"metadata.namespace", "metadata.name"},
    StoreType: types.StoreTypeMemory,
    // DebounceInterval omitted → defaults to types.DefaultDebounceInterval (100ms, leading-edge)

    OnChange: func(store types.Store, stats types.ChangeStats) {
        if !stats.IsInitialSync {
            handleChange(stats)
        }
    },
    OnSyncComplete: func(store types.Store, count int) {
        log.Info("initial sync complete", "count", count)
    },
    CallOnChangeDuringSync: false, // wait for full sync before OnChange fires
}

w, err := watcher.New(cfg, k8sClient, logger)
go w.Start(ctx)

// WaitForSync returns the count of resources loaded; pass through if you don't need it.
count, err := w.WaitForSync(ctx)
```

**SingleWatcher** - Single resource watching. `NewSingle` takes a *pointer* to
the config; `WaitForSync` here returns just an error.

```go
cfg := &types.SingleWatcherConfig{
    GVR:       schema.GroupVersionResource{Version: "v1", Resource: "configmaps"},
    Namespace: "default",
    Name:      "haproxy-config",

    // SingleWatcherConfig.OnChange is typed as types.OnResourceChangeCallback —
    // distinct from the bulk watcher's OnChangeCallback. It returns an error so
    // the watcher can log and continue rather than crash on a bad payload.
    OnChange: func(resource any) error {
        return handleConfigChange(resource)
    },
}

w, err := watcher.NewSingle(cfg, k8sClient)
go w.Start(ctx)
err = w.WaitForSync(ctx)
```

**Debouncing:**

```go
// Rapid changes batched (default DebounceInterval = 100ms, leading-edge)
// t=0ms:   Create Ingress A → fires immediately, refractory window starts
// t=20ms:  Update Ingress A → suppressed (within window)
// t=60ms:  Create Ingress B → suppressed (within window)
// t=100ms: Window closes → OnChange called once with the suppressed stats
```

## Testing Strategies

### Unit Tests with Fake Client

```go
import "k8s.io/client-go/kubernetes/fake"

func TestWatcher(t *testing.T) {
    // Create fake Kubernetes client
    fakeClient := fake.NewSimpleClientset()

    // Add test resources
    ingress := &networkingv1.Ingress{
        ObjectMeta: metav1.ObjectMeta{
            Name:      "test-ingress",
            Namespace: "default",
        },
    }
    fakeClient.NetworkingV1().Ingresses("default").Create(ctx, ingress, metav1.CreateOptions{})

    // Create watcher with fake client
    cfg := types.WatcherConfig{...}
    w, err := watcher.New(cfg, k8sClient, logger)

    // Test watcher behavior
    go w.Start(ctx)
    _, err = w.WaitForSync(ctx) // returns (count, error)

    // Verify resource was indexed
    resources, _ := store.Get("default", "test-ingress")
    assert.Len(t, resources, 1)
}
```

### Testing Initial Sync vs Real-Time Changes

```go
func TestWatcher_InitialSync(t *testing.T) {
    var syncCompleted bool
    var changesDuringSync []ChangeStats
    var changesAfterSync []ChangeStats

    cfg := types.WatcherConfig{
        OnChange: func(store types.Store, stats types.ChangeStats) {
            if !syncCompleted {
                changesDuringSync = append(changesDuringSync, stats)
            } else {
                changesAfterSync = append(changesAfterSync, stats)
            }
        },
        OnSyncComplete: func(store types.Store, count int) {
            syncCompleted = true
        },
        CallOnChangeDuringSync: true,  // Test incremental processing
    }

    w, err := watcher.New(cfg, k8sClient, logger)
    go w.Start(ctx)

    // Add resources before sync completes
    addTestResource()

    _, err = w.WaitForSync(ctx)

    // Add resources after sync
    addTestResource()

    // Verify IsInitialSync flag
    assert.True(t, changesDuringSync[0].IsInitialSync)
    assert.False(t, changesAfterSync[0].IsInitialSync)
}
```

## Common Pitfalls

### Not Waiting for Initial Sync

**Problem**: Processing resources before all watchers synced.

```go
// Bad
w, err := watcher.New(config, client, logger)
go w.Start(ctx)

// Immediately access store - might be empty!
resources, _ := store.List()
reconcile(resources)  // Missing resources!
```

**Solution**: Always wait for sync.

```go
// Good
w, err := watcher.New(cfg, k8sClient, logger)
go w.Start(ctx)

// Wait for initial sync (returns the count of resources loaded)
if _, err := w.WaitForSync(ctx); err != nil {
    return err
}

// Now store has all pre-existing resources
resources, _ := store.List()
reconcile(resources)
```

### Incorrect JSONPath Expressions

**Problem**: Runtime errors from invalid JSONPath.

```go
// Bad - invalid expression not caught
config.IndexBy = []string{
    "metadata..namespace",  // Double dot - invalid
}
```

**Solution**: Validate at startup.

```go
// Good - validate before creating watcher
for _, expr := range config.IndexBy {
    if err := indexer.ValidateJSONPath(expr); err != nil {
        return fmt.Errorf("invalid JSONPath %s: %w", expr, err)
    }
}

w, err := watcher.New(config, client, logger)
```

### Blocking in Callbacks

**Problem**: Long-running work in callbacks blocks watcher.

```go
// Bad - blocks watcher thread
OnChange: func(store Store, stats ChangeStats) {
    resources, _ := store.List()
    // Complex processing (5 seconds)
    processResources(resources)
}
```

**Solution**: Spawn goroutine or use channels.

```go
// Good - non-blocking
OnChange: func(store Store, stats ChangeStats) {
    // Trigger via channel
    select {
    case reconcileChan <- struct{}{}:
    default:
        // Already pending, skip
    }
}

// Separate goroutine handles reconciliation
go func() {
    for range reconcileChan {
        processResources()
    }
}()
```

### Using Watcher for Single Resources

**Problem**: Overhead from indexing and debouncing for a single resource.

```go
// Bad - unnecessary complexity
cfg := types.WatcherConfig{
    GVR:       configMapGVR,
    Namespace: "default",
    IndexBy:   []string{"metadata.name"},
    OnChange:  func(store types.Store, stats types.ChangeStats) { /* … */ },
}
w, err := watcher.New(cfg, k8sClient, logger)
```

**Solution**: Use SingleWatcher (note: takes a *pointer* to the config).

```go
// Good - optimized for single resource
cfg := &types.SingleWatcherConfig{
    GVR:       configMapGVR,
    Namespace: "default",
    Name:      "haproxy-config",
    OnChange:  func(resource any) error { return nil },
}
w, err := watcher.NewSingle(cfg, k8sClient)
```

### Picking Selectors

`WatcherConfig.LabelSelector` is a `*metav1.LabelSelector` (the structured form),
not the serialized `"app=myapp"` string. `WatcherConfig.FieldSelector` does
**not** go through Kubernetes' native field-selector machinery — the watcher
evaluates JSONPath expressions client-side, so `metadata.labels['app']=myapp`
works fine, just at the cost of fetching matching resources first and filtering
afterwards. Prefer `LabelSelector` whenever the predicate fits the selector
grammar; the API server will do the filtering for you.

```go
// Server-side filter — preferred when possible
cfg.LabelSelector = &metav1.LabelSelector{
    MatchLabels: map[string]string{"app": "myapp"},
}

// Client-side JSONPath filter — supports anything, but pulls more data
cfg.FieldSelector = "spec.ingressClassName=haproxy-internal"
```

## Adding New Resource Types

### Checklist

1. Define GVR (GroupVersionResource)
2. Choose index expressions (what lookups do you need?)
3. Determine store type (Memory vs Cached)
4. Leave `DebounceInterval` at zero to use `types.DefaultDebounceInterval` (100ms, leading-edge); only override for resources that need slower batching (e.g. high-volume churn) or near-zero latency (the chart sets `"0"` on EndpointSlice for instant rolling-restart reaction)
5. Implement callbacks
6. Add validation for index expressions
7. Write tests with fake client

### Example: Adding ConfigMap Watching

```go
// Step 1: Define GVR
configMapGVR := schema.GroupVersionResource{
    Group:    "",
    Version:  "v1",
    Resource: "configmaps",
}

// Step 2: Choose index expressions
// Need to look up by namespace and name
indexBy := []string{
    "metadata.namespace",
    "metadata.name",
}

// Step 3: Choose store type
// ConfigMaps can be large, but we need fast access → MemoryStore

// Step 4: Create config
cfg := types.WatcherConfig{
    GVR:       configMapGVR,
    Namespaces: nil, // All namespaces
    LabelSelector: &metav1.LabelSelector{
        MatchLabels: map[string]string{"app": "myapp"},
    },
    IndexBy:   indexBy,
    StoreType: types.StoreTypeMemory,
    // DebounceInterval omitted → uses types.DefaultDebounceInterval (100ms)
    OnChange: func(store types.Store, stats types.ChangeStats) {
        if !stats.IsInitialSync {
            handleConfigMapChange(stats)
        }
    },
    OnSyncComplete: func(store types.Store, count int) {
        log.Info("configmaps synced", "count", count)
    },
}

// Step 5: Create and start watcher
w, err := watcher.New(cfg, k8sClient, logger)
go w.Start(ctx)

// Step 6: Wait for sync — returns (count, error)
if _, err := w.WaitForSync(ctx); err != nil {
    return err
}
```

## Performance Considerations

### Memory Usage

**MemoryStore:**

- Per resource: ~1KB (Ingress) to ~10KB (Service with many endpoints)
- 1000 ingresses ≈ 1MB memory
- Suitable for most use cases

**CachedStore:**

- Per cached entry: Same as resource size
- TTL eviction reduces memory pressure
- Cache size limit prevents unbounded growth

### CPU Usage

**Indexing:**

- O(N) where N = number of index expressions
- Typically <1ms per resource
- Runs during Add/Update operations

**Lookups:**

- O(1) with proper indexing
- Map lookup with composite key
- Typically <100ns

**Debouncing:**

- Single timer per watcher
- Minimal CPU overhead
- Reduces reconciliation frequency

### Network Usage

**Watcher:**

- Initial list: Downloads all resources
- Watch: Receives only changes
- Efficient for long-running processes

**CachedStore:**

- Fetches on cache miss
- Adds API calls vs pure MemoryStore
- Trade-off: Memory vs network

## Troubleshooting

### Watcher Not Receiving Events

**Diagnosis:**

1. Verify RBAC permissions
2. Check GVR is correct (group, version, resource)
3. Verify namespace and label selectors
4. Check if resources exist matching selectors

```bash
# Test RBAC
kubectl auth can-i list ingresses --all-namespaces

# Verify GVR
kubectl api-resources | grep ingress

# Check resources exist
kubectl get ingresses -A -l app=myapp
```

### Store Returns Empty Results

**Diagnosis:**

1. Check if WaitForSync() was called
2. Verify index expressions match query keys
3. Check if resources match watch selectors
4. Inspect store contents

```go
// Debug store contents
resources, _ := store.List()
log.Info("store contents", "count", len(resources))

// Verify indexing
for _, res := range resources {
    keys, _ := idx.ExtractKeys(res)
    log.Info("resource indexed", "keys", keys)
}
```

### High Memory Usage

**Diagnosis:**

1. Count resources in each watcher
2. Check for memory leaks (growing without bound)
3. Consider CachedStore for large resources
4. Review field filtering

```go
// Monitor store size
ticker := time.NewTicker(1 * time.Minute)
go func() {
    for range ticker.C {
        count, _ := store.List()
        log.Info("store size", "count", len(count))
    }
}()
```

## Resources

- API documentation: `pkg/k8s/README.md`
- Architecture: `/docs/site/docs/development/design.md`
- Leader election: `pkg/k8s/leaderelection/CLAUDE.md`
- client-go documentation: <https://github.com/kubernetes/client-go>
- JSONPath syntax: <https://kubernetes.io/docs/reference/kubectl/jsonpath/>
