package store

import (
	"errors"
	"testing"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/indexer"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

// configMapGVR is the resource these tests watch.
var configMapGVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

// createTestIndexer creates a minimal indexer for testing.
func createTestIndexer() *indexer.Indexer {
	return &indexer.Indexer{}
}

// createTestResource creates a test resource with metadata.
func createTestResource(namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"namespace": namespace,
				"name":      name,
			},
			"data": map[string]any{
				"key": "value",
			},
		},
	}
}

// newTestCachedStore builds a CachedStore over client, failing the test if the
// config is rejected. numKeys and ttl are the only settings the tests vary.
func newTestCachedStore(t *testing.T, client *fake.FakeDynamicClient, idx *indexer.Indexer,
	numKeys int, ttl time.Duration) *CachedStore {
	t.Helper()
	store, err := NewCachedStore(&CachedStoreConfig{
		NumKeys:  numKeys,
		CacheTTL: ttl,
		Client:   client,
		GVR:      configMapGVR,
		Indexer:  idx,
	})
	if err != nil {
		t.Fatalf("NewCachedStore failed: %v", err)
	}
	return store
}

// TestNewCachedStore verifies store creation.
func TestNewCachedStore(t *testing.T) {
	scheme := runtime.NewScheme()
	client := fake.NewSimpleDynamicClient(scheme)
	testIndexer := createTestIndexer()

	store := newTestCachedStore(t, client, testIndexer, 2, 5*time.Minute)

	if store == nil {
		t.Fatal("NewCachedStore returned nil")
	}

	if store.Size() != 0 {
		t.Errorf("expected size 0, got %d", store.Size())
	}
}

// TestNewCachedStore_Errors verifies error handling in store creation.
func TestNewCachedStore_Errors(t *testing.T) {
	scheme := runtime.NewScheme()
	client := fake.NewSimpleDynamicClient(scheme)
	testIndexer := createTestIndexer()
	gvr := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}

	tests := []struct {
		name    string
		cfg     *CachedStoreConfig
		wantErr bool
	}{
		{
			name: "missing numKeys",
			cfg: &CachedStoreConfig{
				NumKeys:  0,
				Client:   client,
				GVR:      gvr,
				Indexer:  testIndexer,
				CacheTTL: 5 * time.Minute,
			},
			wantErr: true,
		},
		{
			name: "missing client",
			cfg: &CachedStoreConfig{
				NumKeys:  2,
				Client:   nil,
				GVR:      gvr,
				Indexer:  testIndexer,
				CacheTTL: 5 * time.Minute,
			},
			wantErr: true,
		},
		{
			name: "missing indexer",
			cfg: &CachedStoreConfig{
				NumKeys:  2,
				Client:   client,
				GVR:      gvr,
				Indexer:  nil,
				CacheTTL: 5 * time.Minute,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewCachedStore(tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewCachedStore() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestCachedStore_AddAndGet verifies adding and retrieving resources.
func TestCachedStore_AddAndGet(t *testing.T) {
	scheme := runtime.NewScheme()
	resource := createTestResource("default", "test-cm")

	client := fake.NewSimpleDynamicClient(scheme, resource)
	testIndexer := createTestIndexer()

	store := newTestCachedStore(t, client, testIndexer, 2, 5*time.Minute)

	err := store.Add(resource, []string{"default", "test-cm"})
	if err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	// Verify size
	if store.Size() != 1 {
		t.Errorf("expected size 1, got %d", store.Size())
	}

	// Get resource (should hit cache)
	results, err := store.Get("default", "test-cm")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	retrieved := results[0].(*unstructured.Unstructured)
	if retrieved.GetName() != "test-cm" {
		t.Errorf("expected name 'test-cm', got %q", retrieved.GetName())
	}
}

func TestCachedStore_ForbiddenFailsRead(t *testing.T) {
	wantErr := apierrors.NewForbidden(configMapGVR.GroupResource(), "test-cm", errors.New("access denied"))
	results, err := fetchCachedStoreWithAPIError(t, wantErr)

	if !errors.Is(err, wantErr) {
		t.Fatalf("expected wrapped API error %v, got %v", wantErr, err)
	}
	if results != nil {
		t.Fatalf("failed read returned partial results: %#v", results)
	}
}

func TestCachedStore_InternalErrorFailsRead(t *testing.T) {
	wantErr := apierrors.NewInternalError(errors.New("apiserver unavailable"))
	results, err := fetchCachedStoreWithAPIError(t, wantErr)

	if !errors.Is(err, wantErr) {
		t.Fatalf("expected wrapped API error %v, got %v", wantErr, err)
	}
	if results != nil {
		t.Fatalf("failed read returned partial results: %#v", results)
	}
}

func TestCachedStore_StaleNotFoundReferenceIsAbsent(t *testing.T) {
	notFound := apierrors.NewNotFound(configMapGVR.GroupResource(), "test-cm")
	results, err := fetchCachedStoreWithAPIError(t, notFound)

	if err != nil {
		t.Fatalf("stale NotFound reference returned error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("stale NotFound reference returned %d results", len(results))
	}
}

func fetchCachedStoreWithAPIError(t *testing.T, apiErr error) ([]any, error) {
	t.Helper()
	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	client.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apiErr
	})

	store := newTestCachedStore(t, client, createTestIndexer(), 2, 5*time.Minute)
	resource := createTestResource("default", "test-cm")
	if err := store.Add(resource, []string{"default", "test-cm"}); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	store.cache.Purge()
	return store.Get("default", "test-cm")
}

// TestCachedStore_NonUniqueKeys verifies multiple resources with same index keys.
func TestCachedStore_NonUniqueKeys(t *testing.T) {
	scheme := runtime.NewScheme()

	// Create multiple resources with the same service label
	resources := []*unstructured.Unstructured{
		createTestResource("default", "nginx-slice-1"),
		createTestResource("default", "nginx-slice-2"),
		createTestResource("default", "nginx-slice-3"),
	}

	client := fake.NewSimpleDynamicClient(scheme, resources[0], resources[1], resources[2])
	testIndexer := createTestIndexer()

	cfg := &CachedStoreConfig{
		NumKeys:  1, // Index by service name only
		CacheTTL: 5 * time.Minute,
		Client:   client,
		GVR:      configMapGVR,
		Indexer:  testIndexer,
	}

	store, err := NewCachedStore(cfg)
	if err != nil {
		t.Fatalf("NewCachedStore failed: %v", err)
	}

	// Add all resources with the same index key
	for _, res := range resources {
		if err := store.Add(res, []string{"nginx"}); err != nil {
			t.Fatalf("Add failed: %v", err)
		}
	}

	// Verify all 3 resources are tracked
	if store.Size() != 3 {
		t.Errorf("expected size 3, got %d", store.Size())
	}

	// Get should return all resources with the same index key
	results, err := store.Get("nginx")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	// Verify we got all three resources
	names := make(map[string]bool)
	for _, res := range results {
		r := res.(*unstructured.Unstructured)
		names[r.GetName()] = true
	}

	expectedNames := []string{"nginx-slice-1", "nginx-slice-2", "nginx-slice-3"}
	for _, expectedName := range expectedNames {
		if !names[expectedName] {
			t.Errorf("expected to find resource %q in results", expectedName)
		}
	}
}

// TestCachedStore_UpdateWithNonUniqueKeys verifies updating specific resource.
func TestCachedStore_UpdateWithNonUniqueKeys(t *testing.T) {
	scheme := runtime.NewScheme()

	slice1 := createTestResource("default", "nginx-slice-1")
	slice1.Object["version"] = "v1"

	slice2 := createTestResource("default", "nginx-slice-2")
	slice2.Object["version"] = "v1"

	client := fake.NewSimpleDynamicClient(scheme, slice1, slice2)
	testIndexer := createTestIndexer()

	store := newTestCachedStore(t, client, testIndexer, 1, 5*time.Minute)

	// Add both resources
	if err := store.Add(slice1, []string{"nginx"}); err != nil {
		t.Fatalf("Add slice1 failed: %v", err)
	}
	if err := store.Add(slice2, []string{"nginx"}); err != nil {
		t.Fatalf("Add slice2 failed: %v", err)
	}

	// Update slice1 specifically
	updatedSlice1 := createTestResource("default", "nginx-slice-1")
	updatedSlice1.Object["version"] = "v2"

	if err := store.Update(updatedSlice1, []string{"nginx"}); err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	// Verify both resources still tracked
	if store.Size() != 2 {
		t.Errorf("expected size 2 after update, got %d", store.Size())
	}

	// Get all resources
	results, err := store.Get("nginx")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// Find slice1 and verify it was updated (cache should have v2)
	var foundSlice1 *unstructured.Unstructured
	for _, res := range results {
		r := res.(*unstructured.Unstructured)
		if r.GetName() == "nginx-slice-1" {
			foundSlice1 = r
			break
		}
	}

	if foundSlice1 == nil {
		t.Fatal("slice1 not found after update")
		return
	}

	if foundSlice1.Object["version"] != "v2" {
		t.Errorf("slice1 version: expected 'v2', got %v", foundSlice1.Object["version"])
	}
}

// TestCachedStore_DeleteWithNonUniqueKeys verifies that deleting one resource
// leaves its siblings under the same index key in place, and evicts only the
// deleted resource's cache entry.
func TestCachedStore_DeleteWithNonUniqueKeys(t *testing.T) {
	scheme := runtime.NewScheme()

	slice1 := createTestResource("default", "nginx-slice-1")
	slice2 := createTestResource("default", "nginx-slice-2")

	client := fake.NewSimpleDynamicClient(scheme, slice1, slice2)
	testIndexer := createTestIndexer()

	store := newTestCachedStore(t, client, testIndexer, 1, 5*time.Minute)

	// Add resources
	if err := store.Add(slice1, []string{"nginx"}); err != nil {
		t.Fatalf("Add slice1 failed: %v", err)
	}
	if err := store.Add(slice2, []string{"nginx"}); err != nil {
		t.Fatalf("Add slice2 failed: %v", err)
	}

	// Verify both exist
	if store.Size() != 2 {
		t.Errorf("expected size 2 before delete, got %d", store.Size())
	}

	// Warm the cache for both so eviction is observable.
	if _, err := store.Get("nginx"); err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if err := store.Delete("default", "nginx-slice-1", []string{"nginx"}); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	if store.Size() != 1 {
		t.Errorf("expected size 1 after deleting one sibling, got %d", store.Size())
	}

	// Only the deleted resource's entry is evicted; the sibling stays warm.
	if got := cacheLen(store); got != 1 {
		t.Errorf("expected cache size 1 after delete, got %d", got)
	}

	results, err := store.Get("nginx")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result after delete, got %d", len(results))
	}
	if _, name := extractNamespaceName(results[0]); name != "nginx-slice-2" {
		t.Errorf("expected the surviving resource to be nginx-slice-2, got %q", name)
	}
}

// TestCachedStore_DeleteRemovesEmptyBucket verifies the refs entry is dropped
// once its last resource is deleted.
func TestCachedStore_DeleteRemovesEmptyBucket(t *testing.T) {
	scheme := runtime.NewScheme()

	only := createTestResource("default", "only")
	client := fake.NewSimpleDynamicClient(scheme, only)

	store := newTestCachedStore(t, client, createTestIndexer(), 1, 5*time.Minute)

	if err := store.Add(only, []string{"nginx"}); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	if err := store.Delete("default", "only", []string{"nginx"}); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	if len(store.refs) != 0 {
		t.Errorf("expected the bucket's refs entry to be removed, got %d entries", len(store.refs))
	}
}

// TestCachedStore_List verifies listing all resources.
func TestCachedStore_List(t *testing.T) {
	scheme := runtime.NewScheme()

	// Create resources for two different services
	resources := []*unstructured.Unstructured{
		createTestResource("default", "nginx-slice-1"),
		createTestResource("default", "nginx-slice-2"),
		createTestResource("default", "apache-slice-1"),
	}

	client := fake.NewSimpleDynamicClient(scheme, resources[0], resources[1], resources[2])
	testIndexer := createTestIndexer()

	store := newTestCachedStore(t, client, testIndexer, 1, 5*time.Minute)

	// Add resources with different index keys
	if err := store.Add(resources[0], []string{"nginx"}); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	if err := store.Add(resources[1], []string{"nginx"}); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	if err := store.Add(resources[2], []string{"apache"}); err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	// List should return all resources
	results, err := store.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	// Verify all resource names are present
	names := make(map[string]bool)
	for _, res := range results {
		r := res.(*unstructured.Unstructured)
		names[r.GetName()] = true
	}

	expectedNames := []string{"nginx-slice-1", "nginx-slice-2", "apache-slice-1"}
	for _, expectedName := range expectedNames {
		if !names[expectedName] {
			t.Errorf("expected to find resource %q in List results", expectedName)
		}
	}
}

// TestCachedStore_CacheTTL verifies cache expiration.
func TestCachedStore_CacheTTL(t *testing.T) {
	scheme := runtime.NewScheme()
	resource := createTestResource("default", "test-cm")

	client := fake.NewSimpleDynamicClient(scheme, resource)
	testIndexer := createTestIndexer()

	store := newTestCachedStore(t, client, testIndexer, 2, 100*time.Millisecond) // Very short TTL for testing

	// Add resource (caches it)
	if err := store.Add(resource, []string{"default", "test-cm"}); err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	// Verify cached
	if got := cacheLen(store); got != 1 {
		t.Errorf("expected cache size 1, got %d", got)
	}

	// Wait for TTL expiration
	time.Sleep(150 * time.Millisecond)

	// The entry is still present in the LRU but its TTL has elapsed: the
	// read path (fetchResourceByRef) treats a past expiresAt as a miss.
	store.mu.RLock()
	entry, ok := store.cache.Peek(resourceCacheKey("default", "test-cm"))
	store.mu.RUnlock()
	if !ok {
		t.Fatal("expected entry to still be present in the LRU before a read")
	}
	if !time.Now().After(entry.expiresAt) {
		t.Errorf("expected entry to be expired after waiting past TTL, expiresAt=%v", entry.expiresAt)
	}
}

// TestCachedStore_Clear verifies clearing the store.
func TestCachedStore_Clear(t *testing.T) {
	scheme := runtime.NewScheme()

	resources := []*unstructured.Unstructured{
		createTestResource("default", "cm-1"),
		createTestResource("default", "cm-2"),
	}

	client := fake.NewSimpleDynamicClient(scheme, resources[0], resources[1])
	testIndexer := createTestIndexer()

	store := newTestCachedStore(t, client, testIndexer, 2, 5*time.Minute)

	// Add resources
	for _, res := range resources {
		if err := store.Add(res, []string{"default", res.GetName()}); err != nil {
			t.Fatalf("Add failed: %v", err)
		}
	}

	// Verify size
	if store.Size() != 2 {
		t.Errorf("expected size 2, got %d", store.Size())
	}

	if err := store.Clear(); err != nil {
		t.Fatalf("Clear failed: %v", err)
	}

	// Verify empty
	if store.Size() != 0 {
		t.Errorf("expected size 0 after clear, got %d", store.Size())
	}

	if got := cacheLen(store); got != 0 {
		t.Errorf("expected cache size 0 after clear, got %d", got)
	}
}

// TestCachedStore_PartialMatch verifies partial key matching.
func TestCachedStore_PartialMatch(t *testing.T) {
	scheme := runtime.NewScheme()

	// Create resources in different namespaces
	resources := []*unstructured.Unstructured{
		createTestResource("default", "cm-1"),
		createTestResource("default", "cm-2"),
		createTestResource("kube-system", "cm-3"),
	}

	client := fake.NewSimpleDynamicClient(scheme, resources[0], resources[1], resources[2])
	testIndexer := createTestIndexer()

	store := newTestCachedStore(t, client, testIndexer, 2, 5*time.Minute)

	// Add resources
	for _, res := range resources {
		if err := store.Add(res, []string{res.GetNamespace(), res.GetName()}); err != nil {
			t.Fatalf("Add failed: %v", err)
		}
	}

	// Get all resources in "default" namespace (partial match)
	results, err := store.Get("default")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results for 'default' namespace, got %d", len(results))
	}

	// Verify both are from default namespace
	for _, res := range results {
		r := res.(*unstructured.Unstructured)
		if r.GetNamespace() != "default" {
			t.Errorf("expected namespace 'default', got %q", r.GetNamespace())
		}
	}
}

// TestCachedStore_TTLReset verifies that TTL is reset on cache hits.
func TestCachedStore_TTLReset(t *testing.T) {
	scheme := runtime.NewScheme()
	resource := createTestResource("default", "test-cm")

	client := fake.NewSimpleDynamicClient(scheme, resource)
	testIndexer := createTestIndexer()

	// Short TTL for testing
	cacheTTL := 200 * time.Millisecond

	store := newTestCachedStore(t, client, testIndexer, 2, cacheTTL)

	// Add resource to cache
	if err := store.Add(resource, []string{"default", "test-cm"}); err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	// Record initial expiration time
	store.mu.RLock()
	entry, _ := store.cache.Peek(resourceCacheKey("default", "test-cm"))
	initialExpiry := entry.expiresAt
	store.mu.RUnlock()

	// Wait a bit
	time.Sleep(50 * time.Millisecond)

	// Access resource (should reset TTL)
	results, err := store.Get("default", "test-cm")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	// Check that expiration time was extended
	store.mu.RLock()
	entry, _ = store.cache.Peek(resourceCacheKey("default", "test-cm"))
	newExpiry := entry.expiresAt
	store.mu.RUnlock()

	if !newExpiry.After(initialExpiry) {
		t.Errorf("expiration time was not extended: initial=%v, new=%v", initialExpiry, newExpiry)
	}

	// The new expiry should be approximately cacheTTL from now
	expectedExpiry := time.Now().Add(cacheTTL)
	diff := newExpiry.Sub(expectedExpiry).Abs()
	if diff > 100*time.Millisecond {
		t.Errorf("new expiry time is not approximately cacheTTL from now: diff=%v", diff)
	}

	// Now wait for the original TTL to pass (without accessing)
	// The entry should still be cached because we reset the TTL
	time.Sleep(cacheTTL)

	// Entry should still be in cache (not expired yet)
	store.mu.RLock()
	_, ok := store.cache.Peek(resourceCacheKey("default", "test-cm"))
	store.mu.RUnlock()

	if !ok {
		t.Error("entry was evicted even though TTL was reset")
	}

	// Now wait for the new TTL to expire
	time.Sleep(cacheTTL + 50*time.Millisecond)

	// The entry is now past its (reset) TTL: the read path treats a past
	// expiresAt as a miss and re-fetches.
	store.mu.RLock()
	entry, ok = store.cache.Peek(resourceCacheKey("default", "test-cm"))
	store.mu.RUnlock()
	if !ok {
		t.Fatal("expected entry to still be present in the LRU before a read")
	}
	if !time.Now().After(entry.expiresAt) {
		t.Errorf("expected entry to be expired after waiting past the reset TTL, expiresAt=%v", entry.expiresAt)
	}
}

// TestCachedStore_WrongKeyCount verifies error handling for wrong key counts.
func TestCachedStore_WrongKeyCount(t *testing.T) {
	scheme := runtime.NewScheme()
	resource := createTestResource("default", "test-cm")

	client := fake.NewSimpleDynamicClient(scheme, resource)
	testIndexer := createTestIndexer()

	store := newTestCachedStore(t, client, testIndexer, 2, 5*time.Minute)

	// Test Add with wrong key count
	err := store.Add(resource, []string{"only-one-key"})
	if err == nil {
		t.Error("expected error for wrong key count in Add")
	}

	// Test Update with wrong key count
	err = store.Update(resource, []string{"only-one-key"})
	if err == nil {
		t.Error("expected error for wrong key count in Update")
	}

	// Test Delete with wrong key count
	err = store.Delete("default", "obj", []string{"only-one-key"})
	if err == nil {
		t.Error("expected error for wrong key count in Delete")
	}

	// Test Get with no keys
	_, err = store.Get()
	if err == nil {
		t.Error("expected error for no keys in Get")
	}

	// Test Get with too many keys
	_, err = store.Get("a", "b", "c")
	if err == nil {
		t.Error("expected error for too many keys in Get")
	}
}

// TestCachedStore_UpdateAddsNewResourceToExistingKey verifies Update adds new
// resource when refs exist but specific resource not found.
func TestCachedStore_UpdateAddsNewResourceToExistingKey(t *testing.T) {
	scheme := runtime.NewScheme()

	slice1 := createTestResource("default", "nginx-slice-1")
	slice2 := createTestResource("default", "nginx-slice-2")

	client := fake.NewSimpleDynamicClient(scheme, slice1, slice2)
	testIndexer := createTestIndexer()

	cfg := &CachedStoreConfig{
		NumKeys:  1, // Index by service name only
		CacheTTL: 5 * time.Minute,
		Client:   client,
		GVR:      configMapGVR,
		Indexer:  testIndexer,
	}

	store, err := NewCachedStore(cfg)
	if err != nil {
		t.Fatalf("NewCachedStore failed: %v", err)
	}

	// Add first resource
	if err := store.Add(slice1, []string{"nginx"}); err != nil {
		t.Fatalf("Add slice1 failed: %v", err)
	}

	// Update with a different resource that doesn't exist yet
	// This tests the "resource not found - append" path in Update
	if err := store.Update(slice2, []string{"nginx"}); err != nil {
		t.Fatalf("Update slice2 failed: %v", err)
	}

	// Verify both resources now exist
	if store.Size() != 2 {
		t.Errorf("expected size 2, got %d", store.Size())
	}

	// Verify both can be retrieved
	results, err := store.Get("nginx")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
}

// TestCachedStore_UpdateToNewKey verifies Update creates entry when no refs exist.
func TestCachedStore_UpdateToNewKey(t *testing.T) {
	scheme := runtime.NewScheme()
	resource := createTestResource("default", "test-cm")

	client := fake.NewSimpleDynamicClient(scheme, resource)
	testIndexer := createTestIndexer()

	store := newTestCachedStore(t, client, testIndexer, 2, 5*time.Minute)

	runUpdateToNewKeyTest(t, store, store, resource, []string{"default", "test-cm"})
}

// TestCachedStore_DeleteNonExistent verifies deleting non-existent resource is a no-op.
func TestCachedStore_DeleteNonExistent(t *testing.T) {
	scheme := runtime.NewScheme()
	resource := createTestResource("default", "test-cm")

	client := fake.NewSimpleDynamicClient(scheme, resource)
	testIndexer := createTestIndexer()

	store := newTestCachedStore(t, client, testIndexer, 2, 5*time.Minute)

	runDeleteNonExistentTest(t, store, store, resource,
		[]string{"default", "test-cm"},
		[]string{"other", "nonexistent"})
}

// TestCachedStore_CacheMissLogging verifies that cache misses trigger API fetches and logging.
// This test verifies the logging infrastructure is working, but doesn't
// actually check the log output (that would require a test logger).
func TestCachedStore_CacheMissLogging(t *testing.T) {
	scheme := runtime.NewScheme()
	resource := createTestResource("default", "test-cm")

	client := fake.NewSimpleDynamicClient(scheme, resource)

	// Create a properly initialized indexer
	idx, err := indexer.New(indexer.Config{
		IndexBy:      []string{"metadata.namespace", "metadata.name"},
		IgnoreFields: []string{},
	})
	if err != nil {
		t.Fatalf("creating indexer: %v", err)
	}

	cfg := &CachedStoreConfig{
		NumKeys:  2,
		CacheTTL: 5 * time.Minute,
		Client:   client,
		GVR:      configMapGVR,
		Indexer:  idx,
		// Logger is optional, will use slog.Default()
	}

	store, err := NewCachedStore(cfg)
	if err != nil {
		t.Fatalf("NewCachedStore failed: %v", err)
	}

	// Add resource reference (but not in cache)
	if err := store.Add(resource, []string{"default", "test-cm"}); err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	// Clear cache to force API fetch
	store.cache.Purge()

	// Get resource (should fetch from API and log)
	results, err := store.Get("default", "test-cm")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	// Resource should now be cached
	if got := cacheLen(store); got != 1 {
		t.Errorf("expected cache size 1 after fetch, got %d", got)
	}
}

// TestCachedStore_LRUEviction verifies that the LRU cache evicts the least recently used
// entry when the cache exceeds its maximum size.
func TestCachedStore_LRUEviction(t *testing.T) {
	scheme := runtime.NewScheme()

	// Create 4 resources but set max cache size to 3
	resources := []*unstructured.Unstructured{
		createTestResource("default", "cm-1"),
		createTestResource("default", "cm-2"),
		createTestResource("default", "cm-3"),
		createTestResource("default", "cm-4"),
	}

	client := fake.NewSimpleDynamicClient(scheme, resources[0], resources[1], resources[2], resources[3])
	testIndexer := createTestIndexer()

	cfg := &CachedStoreConfig{
		NumKeys:      2,
		CacheTTL:     5 * time.Minute,
		MaxCacheSize: 3, // Only 3 entries fit
		Client:       client,
		GVR:          configMapGVR,
		Indexer:      testIndexer,
	}

	store, err := NewCachedStore(cfg)
	if err != nil {
		t.Fatalf("NewCachedStore failed: %v", err)
	}

	// Add 4 resources - cache should evict the oldest
	for _, res := range resources {
		if err := store.Add(res, []string{res.GetNamespace(), res.GetName()}); err != nil {
			t.Fatalf("Add failed: %v", err)
		}
	}

	// All 4 refs should exist
	if store.Size() != 4 {
		t.Errorf("expected size 4, got %d", store.Size())
	}

	// But cache should only hold 3 entries (LRU evicted the first)
	if got := cacheLen(store); got != 3 {
		t.Errorf("expected cache size 3 (LRU limit), got %d", got)
	}
}
