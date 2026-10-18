package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/indexer"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/types"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
)

// cacheEntry holds a cached resource with its expiration time.
type cacheEntry struct {
	resource        any
	resourceVersion string
	expiresAt       time.Time
	generation      uint64
}

// resourceRef holds a reference to a Kubernetes resource for API fetching.
// Stores both the unique identifier (namespace+name) and the index keys.
type resourceRef struct {
	namespace       string   // Resource namespace (empty for cluster-scoped)
	name            string   // Resource name
	indexKeys       []string // Index key values for this resource
	generation      uint64
	resourceVersion string
}

// CachedStore stores only resource references in memory and fetches resources from
// the Kubernetes API on access. Fetched resources are cached with a TTL.
//
// This reduces memory usage for large resources (e.g., Secrets) at the cost
// of API latency on cache misses.
//
// Supports non-unique index keys by storing multiple resource references per composite key.
//
// Thread-safe for concurrent access.
type CachedStore struct {
	snapshotCommitFence stores.SnapshotCommitMutex
	mu                  sync.RWMutex
	refs                map[string][]resourceRef        // Composite key -> slice of resource references
	locations           map[resourceIdentity]string     // Resource identity -> composite key
	cache               *lru.Cache[string, *cacheEntry] // LRU cache: encoded resource identity -> cached resource
	refGenerations      map[string]uint64
	nextGeneration      uint64
	numKeys             int                         // Number of index keys
	cacheTTL            time.Duration               // Cache entry TTL
	client              dynamic.Interface           // Kubernetes dynamic client
	gvr                 schema.GroupVersionResource // Resource type to fetch
	indexer             *indexer.Indexer            // Indexer for processing fetched resources
	logger              *slog.Logger                // Logger for debug and warning messages
	projected           bool                        // Informer delivers body-stripped objects (see Projected)
	revisions           revisionState
	readRootMu          sync.Mutex
	readRoot            atomic.Pointer[cachedReadRoot]
}

// DefaultMaxCacheSize is the default maximum number of entries in the LRU cache.
const DefaultMaxCacheSize = 256

// CachedStoreConfig configures a CachedStore.
type CachedStoreConfig struct {
	// NumKeys is the number of index keys (must match indexer configuration)
	NumKeys int

	// CacheTTL is the cache entry time-to-live
	CacheTTL time.Duration

	// MaxCacheSize is the maximum number of entries in the LRU cache.
	// When exceeded, the least recently used entry is evicted.
	// Default: 256
	MaxCacheSize int

	// Client is the Kubernetes dynamic client for fetching resources
	Client dynamic.Interface

	// GVR identifies the resource type to fetch
	GVR schema.GroupVersionResource

	// Indexer processes fetched resources (field filtering)
	Indexer *indexer.Indexer

	// Logger for debug and warning messages (optional, uses slog.Default if nil)
	Logger *slog.Logger

	// Projected indicates the informer feeding this store delivers
	// body-stripped (projected) objects — only identity, indexBy, and
	// fieldSelector fields survive (see pkg/k8s/watcher projection, ADR-0012).
	//
	// In projected mode the store must NOT cache the projected body on
	// Add/Update (a warm render read would otherwise be served a husk).
	// Instead the value cache is populated only by the live API GET in
	// fetchResourceByRef (full body), and Update/Delete invalidate any stale
	// cached body so the next read re-fetches. Off by default; on only for
	// `store: on-demand` (CachedStore) watchers.
	Projected bool
}

// NewCachedStore creates a new API-backed store with caching.
func NewCachedStore(cfg *CachedStoreConfig) (*CachedStore, error) {
	if cfg.NumKeys < 1 {
		return nil, errors.New("numKeys must be at least 1")
	}
	if cfg.Client == nil {
		return nil, errors.New("client is required")
	}
	if cfg.Indexer == nil {
		return nil, errors.New("indexer is required")
	}
	if cfg.CacheTTL == 0 {
		cfg.CacheTTL = 2*time.Minute + 10*time.Second
	}
	if cfg.MaxCacheSize <= 0 {
		cfg.MaxCacheSize = DefaultMaxCacheSize
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	store := &CachedStore{
		refs:           make(map[string][]resourceRef),
		locations:      make(map[resourceIdentity]string),
		refGenerations: make(map[string]uint64),
		numKeys:        cfg.NumKeys,
		cacheTTL:       cfg.CacheTTL,
		client:         cfg.Client,
		gvr:            cfg.GVR,
		indexer:        cfg.Indexer,
		logger:         logger,
		projected:      cfg.Projected,
		revisions:      newRevisionState(defaultRevisionJournalCapacity),
	}
	store.readRoot.Store(newCachedReadRoot(cfg.NumKeys, &store.revisions))
	cache, err := lru.NewWithEvict[string, *cacheEntry](cfg.MaxCacheSize, store.removeCachedWarmValue)
	if err != nil {
		return nil, fmt.Errorf("creating LRU cache: %w", err)
	}
	store.cache = cache
	return store, nil
}

// Get retrieves all resources matching the provided index keys.
func (s *CachedStore) Get(keys ...string) ([]any, error) {
	return s.GetContext(context.Background(), keys...)
}

// GetContext retrieves matching resources and cancels in-flight API fetches with ctx.
func (s *CachedStore) GetContext(ctx context.Context, keys ...string) ([]any, error) {
	if len(keys) == 0 {
		return nil, &StoreError{
			Operation: opGet,
			Keys:      keys,
			Cause:     errors.New("at least one key required"),
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if len(keys) > s.numKeys {
		return nil, &StoreError{
			Operation: opGet,
			Keys:      keys,
			Cause:     fmt.Errorf("too many keys: got %d, expected %d", len(keys), s.numKeys),
		}
	}

	return s.fetchRefs(ctx, s.matchingRefs(keys))
}

func (s *CachedStore) matchingRefs(keys []string) []resourceRef {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var matchingRefs []resourceRef
	if len(keys) == s.numKeys {
		keyStr := indexer.EncodeKey(keys)
		if refs, ok := s.refs[keyStr]; ok {
			matchingRefs = s.appendLiveRefsLocked(matchingRefs, refs)
		}
		sortResourceRefs(matchingRefs)
		return matchingRefs
	}

	encodedPrefix := indexer.EncodeKey(keys)
	for keyStr, refs := range s.refs {
		if indexer.HasEncodedKeyPrefix(keyStr, encodedPrefix) {
			matchingRefs = s.appendLiveRefsLocked(matchingRefs, refs)
		}
	}
	sortResourceRefs(matchingRefs)
	return matchingRefs
}

func (s *CachedStore) appendLiveRefsLocked(dst, refs []resourceRef) []resourceRef {
	for _, ref := range refs {
		generation, ok := s.refGenerations[resourceCacheKey(ref.namespace, ref.name)]
		if !ok || generation != ref.generation {
			continue
		}
		dst = append(dst, ref)
	}
	return dst
}

func (s *CachedStore) fetchRefs(ctx context.Context, refs []resourceRef) ([]any, error) {
	results := make([]any, 0, len(refs))
	for index := range refs {
		ref := &refs[index]
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		resource, err := s.fetchResourceByRef(ctx, ref)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if isNotFound(err) {
				continue
			}
			return nil, err
		}
		results = append(results, resource)
	}
	return results, ctx.Err()
}

func isNotFound(err error) bool {
	for current := err; current != nil; current = errors.Unwrap(current) {
		if apierrors.IsNotFound(current) {
			return true
		}
	}
	return false
}

// ListCached returns only resources currently warm in the LRU cache —
// no API fetches. Used by callers that want to prime a per-render
// snapshot with whatever's free, without paying for the full
// store-wide List() fan-out. The slice may be a small subset of
// what's in the cluster (the LRU is `MaxCacheSize` entries; unaccessed
// references contribute nothing). Expired entries are skipped.
//
// Callers that need cluster-wide iteration should still call List(),
// accepting the WARN and per-reference API fetch cost.
func (s *CachedStore) ListCached() ([]any, error) {
	s.mu.RLock()
	resources := make([]any, 0, s.cache.Len())
	now := time.Now()
	for _, cacheKey := range s.cache.Keys() {
		entry, ok := s.cache.Peek(cacheKey)
		generation, live := s.refGenerations[cacheKey]
		if !ok || !live || generation != entry.generation || now.After(entry.expiresAt) {
			continue
		}
		resources = append(resources, entry.resource)
	}
	s.mu.RUnlock()

	results, err := cloneMemorySnapshotItems(resources)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(results, compareByNamespaceName)
	return results, nil
}

// List returns all resources in the store.
func (s *CachedStore) List() ([]any, error) {
	return s.ListContext(context.Background())
}

// ListContext returns all resources and cancels in-flight API fetches with ctx.
func (s *CachedStore) ListContext(ctx context.Context) ([]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	allRefs := s.allRefsLocked()
	s.mu.RUnlock()

	// Warn about potential performance impact
	s.logger.Warn("Listing cached store causes individual API lookups which may be expensive",
		"gvr", s.gvr.String(),
		"resource_count", len(allRefs),
		"recommendation", "consider using store=full for frequently listed resources")

	return s.fetchRefs(ctx, allRefs)
}

func (s *CachedStore) allRefsLocked() []resourceRef {
	var allRefs []resourceRef
	for _, refs := range s.refs {
		allRefs = s.appendLiveRefsLocked(allRefs, refs)
	}
	sortResourceRefs(allRefs)
	return allRefs
}

func sortResourceRefs(refs []resourceRef) {
	slices.SortFunc(refs, func(left, right resourceRef) int {
		if left.namespace < right.namespace {
			return -1
		}
		if left.namespace > right.namespace {
			return 1
		}
		if left.name < right.name {
			return -1
		}
		if left.name > right.name {
			return 1
		}
		return 0
	})
}

// Add inserts a resource reference, replacing the same identity if present.
func (s *CachedStore) Add(resource any, keys []string) error {
	if err := validateKeyCount("add", keys, s.numKeys); err != nil {
		return err
	}
	if !s.projected {
		owned, err := cloneMemorySnapshotValue(resource)
		if err != nil {
			return &StoreError{Operation: "add", Keys: keys, Cause: err}
		}
		resource = owned
	}

	s.snapshotCommitFence.Lock()
	defer s.snapshotCommitFence.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()

	ns, name := extractNamespaceName(resource)
	keyStr := indexer.EncodeKey(keys)
	identity, identified := identifyResource(resource)
	dataKeys := []string{keyStr}
	var identities []resourceIdentity
	var oldKeys []string
	if identified {
		identities = append(identities, identity)
		if s.identityUnchangedLocked(identity, keyStr, resource) {
			return nil
		}
		oldKeys = cloneStrings(s.revisions.identityKeys[identity])
		if oldKey, exists := s.locations[identity]; exists {
			dataKeys = append(dataKeys, oldKey)
		}
		s.removeReferenceLocked(identity)
		s.locations[identity] = keyStr
	}
	generation := s.advanceGenerationLocked(ns, name)
	s.refs[keyStr] = append(s.refs[keyStr], resourceRef{
		namespace:       ns,
		name:            name,
		indexKeys:       cloneStrings(keys),
		generation:      generation,
		resourceVersion: extractResourceVersion(resource),
	})
	sortResourceRefs(s.refs[keyStr])
	if !s.projected {
		// Non-projected: the informer delivered a full body, so cache it for
		// free. Projected: the body is a husk — leave the value cache to be
		// populated by the live API GET in fetchResourceByRef.
		s.cacheResource(ns, name, resource, generation, extractResourceVersion(resource))
	} else {
		s.cache.Remove(resourceCacheKey(ns, name))
	}
	s.revisions.recordUpsert(identity, identified, keys)
	s.publishCachedReadRootLocked(dataKeys, identities, oldKeys, keys)

	return nil
}

// Update modifies a resource reference and moves it if its index key changed.
func (s *CachedStore) Update(resource any, keys []string) error {
	if err := validateKeyCount("update", keys, s.numKeys); err != nil {
		return err
	}
	if !s.projected {
		owned, err := cloneMemorySnapshotValue(resource)
		if err != nil {
			return &StoreError{Operation: "update", Keys: keys, Cause: err}
		}
		resource = owned
	}

	s.snapshotCommitFence.Lock()
	defer s.snapshotCommitFence.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()

	ns, name := extractNamespaceName(resource)
	keyStr := indexer.EncodeKey(keys)
	identity, identified := identifyResource(resource)
	dataKeys := []string{keyStr}
	var identities []resourceIdentity
	var oldKeys []string
	if identified {
		identities = append(identities, identity)
		if s.identityUnchangedLocked(identity, keyStr, resource) {
			return nil
		}
		oldKeys = cloneStrings(s.revisions.identityKeys[identity])
		if oldKey, exists := s.locations[identity]; exists {
			dataKeys = append(dataKeys, oldKey)
		}
		s.removeReferenceLocked(identity)
		s.locations[identity] = keyStr
	} else {
		s.removeReferenceFromBucketLocked(keyStr, resourceIdentity{namespace: ns, name: name})
	}
	generation := s.advanceGenerationLocked(ns, name)
	s.refs[keyStr] = append(s.refs[keyStr], resourceRef{
		namespace:       ns,
		name:            name,
		indexKeys:       cloneStrings(keys),
		generation:      generation,
		resourceVersion: extractResourceVersion(resource),
	})
	sortResourceRefs(s.refs[keyStr])

	if s.projected {
		// Projected: the new body is a husk. Invalidate any stale full body
		// so the next render read re-fetches the current object live.
		s.cache.Remove(resourceCacheKey(ns, name))
	} else {
		s.cacheResource(ns, name, resource, generation, extractResourceVersion(resource))
	}
	s.revisions.recordUpsert(identity, identified, keys)
	s.publishCachedReadRootLocked(dataKeys, identities, oldKeys, keys)

	return nil
}

// Delete removes the single resource identified by namespace/name from its
// recorded bucket, leaving any siblings in place. The keys validate shape.
// Deleting a resource that is not present is a no-op.
//
// Only the deleted resource's cache entry is evicted. Purging the whole
// bucket's entries would drop warm bodies still referenced elsewhere, forcing
// a live API GET on the next render.
func (s *CachedStore) Delete(namespace, name string, keys []string) error {
	s.snapshotCommitFence.Lock()
	defer s.snapshotCommitFence.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := validateKeyCount(opDelete, keys, s.numKeys); err != nil {
		return err
	}
	if err := validateDeleteName(name, keys); err != nil {
		return err
	}

	identity := resourceIdentity{namespace: namespace, name: name}
	keyStr, exists := s.locations[identity]
	if !exists {
		return nil
	}
	oldKeys := cloneStrings(s.revisions.identityKeys[identity])
	cacheKey := resourceCacheKey(namespace, name)
	delete(s.refGenerations, cacheKey)
	s.cache.Remove(cacheKey)
	s.removeReferenceLocked(identity)
	s.revisions.recordDelete(identity)
	s.publishCachedReadRootLocked([]string{keyStr}, []resourceIdentity{identity}, oldKeys)

	return nil
}

func (s *CachedStore) removeReferenceLocked(identity resourceIdentity) {
	keyStr, ok := s.locations[identity]
	if !ok {
		return
	}
	s.removeReferenceFromBucketLocked(keyStr, identity)
	delete(s.locations, identity)
}

func (s *CachedStore) identityUnchangedLocked(identity resourceIdentity, key string, resource any) bool {
	currentKey, exists := s.locations[identity]
	if !exists || currentKey != key {
		return false
	}
	for _, ref := range s.refs[currentKey] {
		if ref.namespace != identity.namespace || ref.name != identity.name {
			continue
		}
		resourceVersion := extractResourceVersion(resource)
		if resourceVersion != "" && resourceVersion == ref.resourceVersion {
			return true
		}
		if s.projected {
			return false
		}
		entry, cached := s.cache.Peek(resourceCacheKey(identity.namespace, identity.name))
		return cached && entry.generation == ref.generation && reflect.DeepEqual(entry.resource, resource)
	}
	return false
}

func extractResourceVersion(resource any) string {
	if accessor, ok := resource.(interface{ GetResourceVersion() string }); ok {
		return accessor.GetResourceVersion()
	}
	if object, ok := resource.(map[string]any); ok {
		if metadata, ok := object["metadata"].(map[string]any); ok {
			resourceVersion, _ := metadata["resourceVersion"].(string)
			return resourceVersion
		}
	}
	return ""
}

func (s *CachedStore) removeReferenceFromBucketLocked(keyStr string, identity resourceIdentity) {
	refs, ok := s.refs[keyStr]
	if !ok {
		return
	}

	remaining := make([]resourceRef, 0, len(refs))
	for _, ref := range refs {
		if ref.namespace == identity.namespace && ref.name == identity.name {
			continue
		}
		remaining = append(remaining, ref)
	}

	if len(remaining) == 0 {
		delete(s.refs, keyStr)
		return
	}
	s.refs[keyStr] = remaining
}

// Clear removes all resources from the store.
func (s *CachedStore) Clear() error {
	s.snapshotCommitFence.Lock()
	defer s.snapshotCommitFence.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()

	resourceCount := s.sizeLocked()
	if resourceCount == 0 {
		s.cache.Purge()
		return nil
	}
	dataKeys := make([]string, 0, len(s.refs))
	for key := range s.refs {
		dataKeys = append(dataKeys, key)
	}
	identities := make([]resourceIdentity, 0, len(s.revisions.identityKeys))
	keySets := make([][]string, 0, len(s.revisions.identityKeys))
	for identity, keys := range s.revisions.identityKeys {
		identities = append(identities, identity)
		keySets = append(keySets, cloneStrings(keys))
	}
	s.revisions.recordClear(resourceCount)
	s.refs = make(map[string][]resourceRef)
	s.locations = make(map[resourceIdentity]string)
	s.refGenerations = make(map[string]uint64)
	s.cache.Purge()
	s.publishCachedReadRootLocked(dataKeys, identities, keySets...)

	return nil
}

// fetchResourceByRef fetches a resource from cache or API using a resource reference.
func (s *CachedStore) fetchResourceByRef(ctx context.Context, ref *resourceRef) (any, error) {
	resource, ok, err := s.loadCachedResource(ref)
	if err != nil {
		return nil, err
	}
	if ok {
		return resource, nil
	}

	resource, resourceVersion, err := s.fetchProcessedResource(ctx, ref)
	if err != nil {
		return nil, err
	}
	resource, err = s.cacheFetchedResource(ref, resource, resourceVersion)
	if err != nil {
		return nil, err
	}
	return cloneMemorySnapshotValue(resource)
}

func (s *CachedStore) fetchProcessedResource(
	ctx context.Context,
	ref *resourceRef,
) (converted any, resourceVersion string, resultErr error) {
	// Cache miss - fetch from API using namespace+name
	var resource *unstructured.Unstructured
	var err error

	fetchStart := time.Now()

	fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if ref.namespace != "" {
		resource, err = s.client.Resource(s.gvr).Namespace(ref.namespace).Get(fetchCtx, ref.name, metav1.GetOptions{})
	} else {
		// Cluster-scoped resource
		resource, err = s.client.Resource(s.gvr).Get(fetchCtx, ref.name, metav1.GetOptions{})
	}

	fetchDuration := time.Since(fetchStart)

	if err != nil {
		return nil, "", &StoreError{
			Operation: "fetch",
			Keys:      []string{ref.namespace, ref.name},
			Cause:     err,
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	resourceVersion = resource.GetResourceVersion()

	// Log cache miss with timing info
	s.logger.Debug("Fetching uncached resource from API",
		"gvr", s.gvr.String(),
		"namespace", ref.namespace,
		"name", ref.name,
		"duration_ms", fetchDuration.Milliseconds(),
	)

	// Process resource (field filtering and conversion)
	result, err := s.indexer.Process(resource)
	if err != nil {
		return nil, "", &StoreError{
			Operation: "process",
			Keys:      []string{ref.namespace, ref.name},
			Cause:     err,
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	return result.ConvertedResource, resourceVersion, nil
}

func (s *CachedStore) loadCachedResource(ref *resourceRef) (resource any, found bool, resultErr error) {
	cacheKey := resourceCacheKey(ref.namespace, ref.name)
	s.mu.Lock()

	generation, live := s.refGenerations[cacheKey]
	if !live || generation != ref.generation {
		s.mu.Unlock()
		return nil, false, nil
	}
	entry, ok := s.cache.Peek(cacheKey)
	if !ok {
		s.mu.Unlock()
		return nil, false, nil
	}
	if entry.generation != ref.generation {
		s.cache.Remove(cacheKey)
		s.mu.Unlock()
		return nil, false, nil
	}
	now := time.Now()
	if !now.Before(entry.expiresAt) {
		s.mu.Unlock()
		return nil, false, nil
	}

	entry.expiresAt = now.Add(s.cacheTTL)
	s.cache.Get(cacheKey)
	s.publishCachedWarmValue(cacheKey, cachedSnapshotValue{
		resource:        entry.resource,
		resourceVersion: entry.resourceVersion,
		generation:      entry.generation,
		expiresAt:       entry.expiresAt,
	})
	cachedResource := entry.resource
	s.mu.Unlock()
	detached, err := cloneMemorySnapshotValue(cachedResource)
	if err != nil {
		return nil, false, err
	}
	return detached, true, nil
}

func (s *CachedStore) cacheFetchedResource(
	ref *resourceRef,
	resource any,
	resourceVersion string,
) (any, error) {
	owned, err := cloneMemorySnapshotValue(resource)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	cacheKey := resourceCacheKey(ref.namespace, ref.name)
	if generation, live := s.refGenerations[cacheKey]; !live || generation != ref.generation {
		return owned, nil
	}
	s.cacheResource(ref.namespace, ref.name, owned, ref.generation, resourceVersion)
	return owned, nil
}

// cacheResource stores a resource in the LRU cache under its encoded identity.
// The caller must hold s.mu.
func (s *CachedStore) cacheResource(
	namespace, name string,
	resource any,
	generation uint64,
	resourceVersion string,
) {
	cacheKey := resourceCacheKey(namespace, name)
	entry := &cacheEntry{
		resource:        resource,
		resourceVersion: resourceVersion,
		expiresAt:       time.Now().Add(s.cacheTTL),
		generation:      generation,
	}
	s.cache.Add(cacheKey, entry)
	s.publishCachedWarmValue(cacheKey, cachedSnapshotValue{
		resource:        entry.resource,
		resourceVersion: entry.resourceVersion,
		generation:      entry.generation,
		expiresAt:       entry.expiresAt,
	})
}

func (s *CachedStore) advanceGenerationLocked(namespace, name string) uint64 {
	s.nextGeneration++
	generation := s.nextGeneration
	s.refGenerations[resourceCacheKey(namespace, name)] = generation
	return generation
}

// Size returns the number of tracked resources in the store.
func (s *CachedStore) Size() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sizeLocked()
}

func (s *CachedStore) sizeLocked() int {
	count := 0
	for _, refs := range s.refs {
		count += len(refs)
	}
	return count
}

// GetIdentity returns one resource through the namespace/name location index.
func (s *CachedStore) GetIdentity(namespace, name string) (item any, found bool, err error) {
	identity := resourceIdentity{namespace: namespace, name: name}
	s.mu.RLock()
	key, exists := s.locations[identity]
	if !exists {
		s.mu.RUnlock()
		return nil, false, nil
	}
	var foundRef resourceRef
	for _, ref := range s.refs[key] {
		if ref.namespace == namespace && ref.name == name {
			foundRef = ref
			break
		}
	}
	s.mu.RUnlock()
	if foundRef.name == "" {
		return nil, false, nil
	}
	resource, err := s.fetchResourceByRef(context.Background(), &foundRef)
	if err != nil {
		if isNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return resource, true, nil
}

func (s *CachedStore) AcquireSnapshotCommitFence(ctx context.Context) (func(), error) {
	return s.snapshotCommitFence.Acquire(ctx)
}

// Ensure CachedStore implements types.Store interface.
var (
	_ types.Store                 = (*CachedStore)(nil)
	_ stores.IdentityGetter       = (*CachedStore)(nil)
	_ stores.SnapshotCommitFencer = (*CachedStore)(nil)
)
