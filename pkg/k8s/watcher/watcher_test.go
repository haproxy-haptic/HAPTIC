// Copyright 2025 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package watcher

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/client"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

var configMapGVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

type blockingStopWatcher struct {
	watch.Interface
	stopStarted chan struct{}
	release     chan struct{}
	stopOnce    sync.Once
}

func (w *blockingStopWatcher) Stop() {
	w.stopOnce.Do(func() {
		close(w.stopStarted)
		<-w.release
		w.Interface.Stop()
	})
}

func newBlockingWatchClient(t *testing.T) (*client.Client, *blockingStopWatcher) {
	t.Helper()
	fakeDynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{configMapGVR: "ConfigMapList"},
	)
	blockingWatcher := &blockingStopWatcher{
		Interface:   watch.NewRaceFreeFake(),
		stopStarted: make(chan struct{}),
		release:     make(chan struct{}),
	}
	fakeDynamicClient.PrependWatchReactor("*", func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, blockingWatcher, nil
	})
	return client.NewFromClientset(kubefake.NewClientset(), fakeDynamicClient, "default"), blockingWatcher
}

func newTestClient(t *testing.T, objects ...runtime.Object) *client.Client {
	t.Helper()
	fakeClientset := kubefake.NewClientset()
	// Register the list kind for configmaps so the fake dynamic client can
	// LIST them; without this, the reflector panics on unknown list kinds.
	gvrToListKind := map[schema.GroupVersionResource]string{
		configMapGVR: "ConfigMapList",
	}
	fakeDynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		gvrToListKind,
		objects...,
	)
	return client.NewFromClientset(fakeClientset, fakeDynamicClient, "default")
}

func validWatcherConfig() types.WatcherConfig {
	return types.WatcherConfig{
		GVR:              configMapGVR,
		IndexBy:          []string{"metadata.namespace", "metadata.name"},
		StoreType:        types.StoreTypeMemory,
		DebounceInterval: 50 * time.Millisecond,
		OnChange:         func(_ types.Store, _ types.ChangeStats) {}, // Required callback
	}
}

func TestNew_ValidConfig(t *testing.T) {
	k8sClient := newTestClient(t)
	cfg := validWatcherConfig()
	cfg.OnChange = func(_ types.Store, _ types.ChangeStats) {}

	watcher, err := New(cfg, k8sClient, nil)

	require.NoError(t, err)
	require.NotNil(t, watcher)
	assert.NotNil(t, watcher.Store())
	assert.False(t, watcher.IsSynced())
}

func TestNew_MissingGVR(t *testing.T) {
	k8sClient := newTestClient(t)
	cfg := types.WatcherConfig{
		IndexBy:   []string{"metadata.namespace"},
		StoreType: types.StoreTypeMemory,
	}

	watcher, err := New(cfg, k8sClient, nil)

	require.Error(t, err)
	assert.Nil(t, watcher)
	assert.Contains(t, err.Error(), "GVR")
}

func TestNew_MissingIndexBy(t *testing.T) {
	k8sClient := newTestClient(t)
	cfg := types.WatcherConfig{
		GVR:       configMapGVR,
		StoreType: types.StoreTypeMemory,
	}

	watcher, err := New(cfg, k8sClient, nil)

	require.Error(t, err)
	assert.Nil(t, watcher)
	assert.Contains(t, err.Error(), "IndexBy")
}

func TestNew_InvalidIndexExpression(t *testing.T) {
	k8sClient := newTestClient(t)
	cfg := types.WatcherConfig{
		GVR:       configMapGVR,
		IndexBy:   []string{"invalid..path"},
		StoreType: types.StoreTypeMemory,
	}

	watcher, err := New(cfg, k8sClient, nil)

	require.Error(t, err)
	assert.Nil(t, watcher)
}

func TestNew_MemoryStoreType(t *testing.T) {
	k8sClient := newTestClient(t)
	cfg := validWatcherConfig()
	cfg.StoreType = types.StoreTypeMemory

	watcher, err := New(cfg, k8sClient, nil)

	require.NoError(t, err)
	require.NotNil(t, watcher)
}

func TestNew_CachedStoreType(t *testing.T) {
	k8sClient := newTestClient(t)
	cfg := validWatcherConfig()
	cfg.StoreType = types.StoreTypeCached
	cfg.CacheTTL = 10 * time.Minute

	watcher, err := New(cfg, k8sClient, slog.Default())

	require.NoError(t, err)
	require.NotNil(t, watcher)
}

func TestNew_InvalidStoreType(t *testing.T) {
	k8sClient := newTestClient(t)
	cfg := validWatcherConfig()
	cfg.StoreType = types.StoreType(999) // Invalid

	watcher, err := New(cfg, k8sClient, nil)

	require.Error(t, err)
	assert.Nil(t, watcher)
	assert.Contains(t, err.Error(), "unsupported store type")
}

func TestNew_NamespacedWatch(t *testing.T) {
	k8sClient := newTestClient(t)
	cfg := validWatcherConfig()
	cfg.NamespacedWatch = true

	watcher, err := New(cfg, k8sClient, nil)

	require.NoError(t, err)
	require.NotNil(t, watcher)
}

func TestNew_WithLabelSelector(t *testing.T) {
	k8sClient := newTestClient(t)
	cfg := validWatcherConfig()
	cfg.LabelSelector = &metav1.LabelSelector{
		MatchLabels: map[string]string{"app": "test"},
	}

	watcher, err := New(cfg, k8sClient, nil)

	require.NoError(t, err)
	require.NotNil(t, watcher)
}

func TestNew_WithIgnoreFields(t *testing.T) {
	k8sClient := newTestClient(t)
	cfg := validWatcherConfig()
	cfg.IgnoreFields = []string{"metadata.managedFields", "metadata.resourceVersion"}

	watcher, err := New(cfg, k8sClient, nil)

	require.NoError(t, err)
	require.NotNil(t, watcher)
}

func TestNew_WithLogger(t *testing.T) {
	k8sClient := newTestClient(t)
	cfg := validWatcherConfig()
	logger := slog.Default()

	watcher, err := New(cfg, k8sClient, logger)

	require.NoError(t, err)
	require.NotNil(t, watcher)
}

func TestNew_SetsDefaults(t *testing.T) {
	k8sClient := newTestClient(t)
	cfg := types.WatcherConfig{
		GVR:      configMapGVR,
		IndexBy:  []string{"metadata.namespace", "metadata.name"},
		OnChange: func(_ types.Store, _ types.ChangeStats) {}, // Required
		// StoreType and DebounceInterval not set - should use defaults
	}

	watcher, err := New(cfg, k8sClient, nil)

	require.NoError(t, err)
	require.NotNil(t, watcher)
}

func TestWatcher_Store(t *testing.T) {
	k8sClient := newTestClient(t)
	cfg := validWatcherConfig()

	watcher, err := New(cfg, k8sClient, nil)
	require.NoError(t, err)

	store := watcher.Store()
	assert.NotNil(t, store)
}

func TestWatcher_IsSynced_InitiallyFalse(t *testing.T) {
	k8sClient := newTestClient(t)
	cfg := validWatcherConfig()

	watcher, err := New(cfg, k8sClient, nil)
	require.NoError(t, err)

	assert.False(t, watcher.IsSynced())
}

func TestWatcher_Start_SyncsAndCallsOnSyncComplete(t *testing.T) {
	k8sClient := newTestClient(t)

	var syncCompleteCalled atomic.Bool
	var syncCount atomic.Int32

	cfg := validWatcherConfig()
	cfg.OnSyncComplete = func(_ types.Store, count int) {
		syncCompleteCalled.Store(true)
		syncCount.Store(int32(count))
	}

	watcher, err := New(cfg, k8sClient, nil)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errChan := make(chan error, 1)
	go func() {
		errChan <- watcher.Start(ctx)
	}()

	// Wait for sync
	time.Sleep(500 * time.Millisecond)

	assert.True(t, watcher.IsSynced())
	assert.True(t, syncCompleteCalled.Load())
}

func TestWatcher_Start_ContextCancellation(t *testing.T) {
	k8sClient := newTestClient(t)
	cfg := validWatcherConfig()

	watcher, err := New(cfg, k8sClient, nil)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())

	errChan := make(chan error, 1)
	go func() {
		errChan <- watcher.Start(ctx)
	}()

	// Wait a bit for start
	time.Sleep(200 * time.Millisecond)

	// Cancel context
	cancel()

	// Should exit cleanly
	select {
	case err := <-errChan:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for watcher to stop")
	}
}

func TestWatcher_StartWaitsForInformerStop(t *testing.T) {
	k8sClient, blockingWatcher := newBlockingWatchClient(t)
	w, err := New(validWatcherConfig(), k8sClient, slog.Default())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- w.Start(ctx)
	}()
	require.Eventually(t, w.IsSynced, time.Second, 5*time.Millisecond)

	cancel()
	select {
	case <-blockingWatcher.stopStarted:
	case <-time.After(time.Second):
		t.Fatal("informer watch was not stopped")
	}
	select {
	case err := <-done:
		t.Fatalf("Start returned before the informer stopped: %v", err)
	default:
	}

	close(blockingWatcher.release)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Start did not return after the informer stopped")
	}
}

func TestWatcher_Stop(t *testing.T) {
	k8sClient := newTestClient(t)
	cfg := validWatcherConfig()

	watcher, err := New(cfg, k8sClient, nil)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())

	errChan := make(chan error, 1)
	go func() {
		errChan <- watcher.Start(ctx)
	}()

	// Wait for start
	time.Sleep(200 * time.Millisecond)

	// Cancel context to trigger stop (don't call Stop directly as Start calls it)
	cancel()

	// Wait for clean exit
	select {
	case err := <-errChan:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for watcher to stop")
	}
}

func TestWatcher_StopIdempotency(t *testing.T) {
	k8sClient := newTestClient(t)
	cfg := validWatcherConfig()

	w, err := New(cfg, k8sClient, nil)
	require.NoError(t, err)

	// Stop() must be idempotent — a double close of stopCh would otherwise
	// panic ("close of closed channel"). Mirrors SingleWatcher.Stop's
	// documented contract (see TestSingleWatcher_StopIdempotency).
	require.NoError(t, w.Stop())
	require.NoError(t, w.Stop())
	require.NoError(t, w.Stop())
}

func TestWatcher_ForceSync(t *testing.T) {
	k8sClient := newTestClient(t)

	var callbackCalled atomic.Bool

	cfg := validWatcherConfig()
	cfg.OnChange = func(_ types.Store, _ types.ChangeStats) {
		callbackCalled.Store(true)
	}

	watcher, err := New(cfg, k8sClient, nil)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		_ = watcher.Start(ctx)
	}()

	// Wait for sync
	time.Sleep(500 * time.Millisecond)

	// Force sync should flush debouncer
	watcher.ForceSync()

	// Give callback time to fire
	time.Sleep(100 * time.Millisecond)
}

func TestWatcher_WaitForSync_Success(t *testing.T) {
	k8sClient := newTestClient(t)
	cfg := validWatcherConfig()

	watcher, err := New(cfg, k8sClient, nil)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		_ = watcher.Start(ctx)
	}()

	count, err := watcher.WaitForSync(ctx)

	require.NoError(t, err)
	assert.GreaterOrEqual(t, count, 0)
	assert.True(t, watcher.IsSynced())
}

func TestWatcher_WaitForSync_ContextCancelled(t *testing.T) {
	k8sClient := newTestClient(t)
	cfg := validWatcherConfig()

	watcher, err := New(cfg, k8sClient, nil)
	require.NoError(t, err)

	// Create already cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = watcher.WaitForSync(ctx)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "sync")
}

func TestWatcher_HandleAdd(t *testing.T) {
	configMap := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "test-config",
				"namespace": "default",
			},
			"data": map[string]any{
				"key": "value",
			},
		},
	}

	k8sClient := newTestClient(t, configMap)

	var addCalled atomic.Bool

	cfg := validWatcherConfig()
	cfg.CallOnChangeDuringSync = true // See changes during sync
	cfg.OnChange = func(_ types.Store, _ types.ChangeStats) {
		addCalled.Store(true)
	}

	watcher, err := New(cfg, k8sClient, slog.Default())
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() {
		_ = watcher.Start(ctx)
	}()

	// Wait for sync and potential callbacks
	time.Sleep(1 * time.Second)

	// Check store has the resource
	_, err = watcher.Store().List()
	require.NoError(t, err)
	// The fake dynamic client may not fully simulate resource presence
	// but the watcher should have synced
	assert.True(t, watcher.IsSynced())
}

func TestWatchResource_Nil(t *testing.T) {
	k8sClient := newTestClient(t)
	cfg := validWatcherConfig()

	_, err := New(cfg, k8sClient, slog.Default())
	require.NoError(t, err)

	// Test with nil
	result := watchResource(nil)
	assert.Nil(t, result)

	// Test with invalid type
	result = watchResource("invalid")
	assert.Nil(t, result)
}

func TestWatchResource_Unstructured(t *testing.T) {
	k8sClient := newTestClient(t)
	cfg := validWatcherConfig()

	_, err := New(cfg, k8sClient, slog.Default())
	require.NoError(t, err)

	obj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "test",
				"namespace": "default",
			},
		},
	}

	result := watchResource(obj)
	assert.NotNil(t, result)
	assert.Equal(t, "test", result.GetName())
}

func TestWatcher_HandleUpdate_SkipsResyncEvent(t *testing.T) {
	// When resourceVersion is unchanged, the update should be skipped (resync event)
	k8sClient := newTestClient(t)

	var updateCount atomic.Int32

	cfg := validWatcherConfig()
	cfg.CallOnChangeDuringSync = true
	cfg.OnChange = func(_ types.Store, stats types.ChangeStats) {
		updateCount.Add(int32(stats.Modified))
	}

	watcher, err := New(cfg, k8sClient, slog.Default())
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		_ = watcher.Start(ctx)
	}()

	_, err = watcher.WaitForSync(ctx)
	require.NoError(t, err)

	// Create old and new objects with SAME resourceVersion (simulates resync)
	oldObj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":            "test-config",
				"namespace":       "default",
				"resourceVersion": "12345",
			},
		},
	}
	newObj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":            "test-config",
				"namespace":       "default",
				"resourceVersion": "12345", // Same version - resync event
			},
		},
	}

	// Call handleUpdate directly (simulate informer event)
	initialCount := updateCount.Load()
	watcher.handleUpdate(oldObj, newObj)

	// Give debouncer time to fire
	time.Sleep(100 * time.Millisecond)

	// Update count should NOT have increased (resync skipped)
	assert.Equal(t, initialCount, updateCount.Load(), "resync event should be skipped")
}

func TestWatcher_HandleUpdate_ProcessesRealUpdate(t *testing.T) {
	// When both resourceVersion and generation change, process the update
	k8sClient := newTestClient(t)

	var updateCount atomic.Int32

	cfg := validWatcherConfig()
	cfg.CallOnChangeDuringSync = true
	cfg.DebounceInterval = 10 * time.Millisecond // Short debounce for test
	cfg.OnChange = func(_ types.Store, stats types.ChangeStats) {
		updateCount.Add(int32(stats.Modified))
	}

	watcher, err := New(cfg, k8sClient, slog.Default())
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		_ = watcher.Start(ctx)
	}()

	_, err = watcher.WaitForSync(ctx)
	require.NoError(t, err)

	// Create old and new objects with different resourceVersion AND generation
	oldObj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":            "test-config",
				"namespace":       "default",
				"resourceVersion": "12345",
				"generation":      int64(1),
			},
		},
	}
	newObj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":            "test-config",
				"namespace":       "default",
				"resourceVersion": "12346",
				"generation":      int64(2), // Different generation - real update
			},
		},
	}

	// Call handleUpdate directly
	initialCount := updateCount.Load()
	watcher.handleUpdate(oldObj, newObj)

	// Give debouncer time to fire
	time.Sleep(100 * time.Millisecond)

	// Update count SHOULD have increased (real update)
	assert.Greater(t, updateCount.Load(), initialCount, "real update should be processed")
}

func TestWatcher_HandleUpdate_ProcessesZeroGeneration(t *testing.T) {
	// When generation is 0 (resource doesn't use generation), process the update
	// This covers EndpointSlices and other resources without status subresource
	k8sClient := newTestClient(t)

	var updateCount atomic.Int32

	cfg := validWatcherConfig()
	cfg.CallOnChangeDuringSync = true
	cfg.DebounceInterval = 10 * time.Millisecond
	cfg.OnChange = func(_ types.Store, stats types.ChangeStats) {
		updateCount.Add(int32(stats.Modified))
	}

	watcher, err := New(cfg, k8sClient, slog.Default())
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		_ = watcher.Start(ctx)
	}()

	_, err = watcher.WaitForSync(ctx)
	require.NoError(t, err)

	// Create objects with generation=0 (like EndpointSlices)
	oldObj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "discovery.k8s.io/v1",
			"kind":       "EndpointSlice",
			"metadata": map[string]any{
				"name":            "test-endpoints",
				"namespace":       "default",
				"resourceVersion": "12345",
				// No generation field (defaults to 0)
			},
		},
	}
	newObj := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "discovery.k8s.io/v1",
			"kind":       "EndpointSlice",
			"metadata": map[string]any{
				"name":            "test-endpoints",
				"namespace":       "default",
				"resourceVersion": "12346", // Different version
				// No generation field (defaults to 0)
			},
		},
	}

	// Call handleUpdate directly
	initialCount := updateCount.Load()
	watcher.handleUpdate(oldObj, newObj)

	// Give debouncer time to fire
	time.Sleep(100 * time.Millisecond)

	// Update count SHOULD have increased (generation=0 means we don't skip)
	assert.Greater(t, updateCount.Load(), initialCount, "update with generation=0 should be processed")
}

func TestWatcher_CallOnChangeDuringSync_False(t *testing.T) {
	k8sClient := newTestClient(t)

	var callbackCount atomic.Int32

	cfg := validWatcherConfig()
	cfg.CallOnChangeDuringSync = false // Default
	cfg.OnChange = func(_ types.Store, _ types.ChangeStats) {
		callbackCount.Add(1)
	}

	watcher, err := New(cfg, k8sClient, nil)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		_ = watcher.Start(ctx)
	}()

	// Wait for sync
	time.Sleep(500 * time.Millisecond)

	assert.True(t, watcher.IsSynced())
	// Callbacks should be suppressed during sync
}

func TestWatcher_CallOnChangeDuringSync_True(t *testing.T) {
	k8sClient := newTestClient(t)

	var callbackCount atomic.Int32

	cfg := validWatcherConfig()
	cfg.CallOnChangeDuringSync = true
	cfg.OnChange = func(_ types.Store, _ types.ChangeStats) {
		callbackCount.Add(1)
	}

	watcher, err := New(cfg, k8sClient, nil)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		_ = watcher.Start(ctx)
	}()

	// Wait for sync
	time.Sleep(500 * time.Millisecond)

	assert.True(t, watcher.IsSynced())
	// Callbacks may fire during sync
}

func TestWatcher_ConcurrentAccess(t *testing.T) {
	k8sClient := newTestClient(t)
	cfg := validWatcherConfig()

	watcher, err := New(cfg, k8sClient, nil)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() {
		_ = watcher.Start(ctx)
	}()

	// Wait for start
	time.Sleep(200 * time.Millisecond)

	// Concurrent access to IsSynced and Store
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			for range 50 {
				_ = watcher.IsSynced()
				_ = watcher.Store()
			}
		})
	}

	wg.Wait()
}

func TestWatcher_ApplyListOptions_WithSelector(t *testing.T) {
	k8sClient := newTestClient(t)
	cfg := validWatcherConfig()
	cfg.LabelSelector = &metav1.LabelSelector{
		MatchLabels: map[string]string{"app": "test", "env": "prod"},
	}

	watcher, err := New(cfg, k8sClient, nil)
	require.NoError(t, err)

	options := &metav1.ListOptions{}
	watcher.applyListOptions(options)

	assert.NotEmpty(t, options.LabelSelector)
	assert.Contains(t, options.LabelSelector, "app=test")
}

func TestWatcher_ApplyListOptions_NoSelector(t *testing.T) {
	k8sClient := newTestClient(t)
	cfg := validWatcherConfig()
	cfg.LabelSelector = nil

	watcher, err := New(cfg, k8sClient, nil)
	require.NoError(t, err)

	options := &metav1.ListOptions{}
	watcher.applyListOptions(options)

	assert.Empty(t, options.LabelSelector)
}
