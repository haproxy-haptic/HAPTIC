package resourcewatcher

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	coreconfig "github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/client"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/types"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/watcher"
)

// newFakeClient returns a *client.Client backed by in-memory fake clientsets.
// The watcher component constructor only stores the client and reads its
// namespace; no API calls happen until Start(), so a fake is sufficient for
// construction-only tests.
func newFakeClient() *client.Client {
	return client.NewFromClientset(
		fake.NewClientset(),
		dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()),
		"default",
	)
}

type blockingResourceWatch struct {
	watch.Interface
	stopStarted chan struct{}
	release     chan struct{}
	stopOnce    sync.Once
}

func (w *blockingResourceWatch) Stop() {
	w.stopOnce.Do(func() {
		close(w.stopStarted)
		<-w.release
		w.Interface.Stop()
	})
}

func newBlockingPodWatchClient() (*client.Client, *blockingResourceWatch) {
	podGVR := schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	fakeDynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{podGVR: "PodList"},
	)
	blockingWatch := &blockingResourceWatch{
		Interface:   watch.NewRaceFreeFake(),
		stopStarted: make(chan struct{}),
		release:     make(chan struct{}),
	}
	fakeDynamicClient.PrependWatchReactor("*", func(k8stesting.Action) (bool, watch.Interface, error) {
		return true, blockingWatch, nil
	})
	return client.NewFromClientset(fake.NewClientset(), fakeDynamicClient, "default"), blockingWatch
}

func TestToGVR(t *testing.T) {
	tests := []struct {
		name    string
		wr      coreconfig.WatchedResource
		want    schema.GroupVersionResource
		wantErr bool
	}{
		{
			name: "core resource",
			wr: coreconfig.WatchedResource{
				APIVersion: "v1",
				Resources:  "services",
			},
			want: schema.GroupVersionResource{
				Group:    "",
				Version:  "v1",
				Resource: "services",
			},
		},
		{
			name: "networking resource",
			wr: coreconfig.WatchedResource{
				APIVersion: "networking.k8s.io/v1",
				Resources:  "ingresses",
			},
			want: schema.GroupVersionResource{
				Group:    "networking.k8s.io",
				Version:  "v1",
				Resource: "ingresses",
			},
		},
		{
			name: "discovery resource",
			wr: coreconfig.WatchedResource{
				APIVersion: "discovery.k8s.io/v1",
				Resources:  "endpointslices",
			},
			want: schema.GroupVersionResource{
				Group:    "discovery.k8s.io",
				Version:  "v1",
				Resource: "endpointslices",
			},
		},
		{
			name: "missing api_version",
			wr: coreconfig.WatchedResource{
				Resources: "ingresses",
			},
			wantErr: true,
		},
		{
			name: "missing kind",
			wr: coreconfig.WatchedResource{
				APIVersion: "v1",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := toGVR(&tt.wr)

			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseAPIVersion(t *testing.T) {
	tests := []struct {
		name        string
		apiVersion  string
		wantGroup   string
		wantVersion string
	}{
		{
			name:        "core resource",
			apiVersion:  "v1",
			wantGroup:   "",
			wantVersion: "v1",
		},
		{
			name:        "namespaced resource",
			apiVersion:  "networking.k8s.io/v1",
			wantGroup:   "networking.k8s.io",
			wantVersion: "v1",
		},
		{
			name:        "custom resource",
			apiVersion:  "example.com/v1alpha1",
			wantGroup:   "example.com",
			wantVersion: "v1alpha1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			group, version := parseAPIVersion(tt.apiVersion)
			assert.Equal(t, tt.wantGroup, group)
			assert.Equal(t, tt.wantVersion, version)
		})
	}
}

func TestDedupIgnoreFields(t *testing.T) {
	tests := []struct {
		name   string
		fields []string
		want   []string
	}{
		{
			name:   "no duplicates preserves order",
			fields: []string{"metadata.managedFields", "metadata.annotations"},
			want:   []string{"metadata.managedFields", "metadata.annotations"},
		},
		{
			name:   "deduplicate keeps first occurrence",
			fields: []string{"metadata.managedFields", "metadata.annotations", "metadata.managedFields", "spec.template"},
			want:   []string{"metadata.managedFields", "metadata.annotations", "spec.template"},
		},
		{
			name:   "empty",
			fields: []string{},
			want:   []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dedupIgnoreFields(tt.fields)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestMergeIgnoreFields(t *testing.T) {
	global := []string{"metadata.managedFields"}
	got := mergeIgnoreFields(global, []string{"status.listeners[*].attachedRoutes", "metadata.managedFields"})
	assert.Equal(t, []string{"metadata.managedFields", "status.listeners[*].attachedRoutes"}, got)
	assert.Equal(t, []string{"metadata.managedFields"}, global, "the global list is not modified")
	assert.Equal(t, []string{"metadata.managedFields"}, mergeIgnoreFields(global, nil))
}

func TestDetermineStoreType(t *testing.T) {
	tests := []struct {
		name        string
		storeConfig string
		want        types.StoreType
	}{
		{
			name:        "on-demand returns cached",
			storeConfig: "on-demand",
			want:        types.StoreTypeCached,
		},
		{
			name:        "full returns memory",
			storeConfig: "full",
			want:        types.StoreTypeMemory,
		},
		{
			name:        "empty returns memory",
			storeConfig: "",
			want:        types.StoreTypeMemory,
		},
		{
			name:        "other value returns memory",
			storeConfig: "some-other",
			want:        types.StoreTypeMemory,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := determineStoreType(tt.storeConfig)
			assert.Equal(t, tt.want, got)
		})
	}
}

// mockStore implements types.Store for testing.
type mockStore struct {
	name string
}

func (m *mockStore) Get(...string) ([]any, error)            { return nil, nil }
func (m *mockStore) List() ([]any, error)                    { return nil, nil }
func (m *mockStore) Add(any, []string) error                 { return nil }
func (m *mockStore) Update(any, []string) error              { return nil }
func (m *mockStore) Delete(string, string, []string) error   { return nil }
func (m *mockStore) Clear() error                            { return nil }
func (m *mockStore) GetKeys(any, []string) ([]string, error) { return nil, nil }

func (m *mockStore) Refresh(any, []string, []string) (changed, deleted bool) {
	return false, false
}
func (m *mockStore) Count() int { return 0 }

// createTestComponent creates a minimal ResourceWatcherComponent for testing
// without requiring a Kubernetes cluster.
func createTestComponent() *ResourceWatcherComponent {
	return &ResourceWatcherComponent{
		watchers: map[string]*watcher.Watcher{
			"services":  nil,
			"ingresses": nil,
		},
		stores: map[string]types.Store{
			"services":  &mockStore{name: "services"},
			"ingresses": &mockStore{name: "ingresses"},
		},
		eventBus:  busevents.NewEventBus(10),
		k8sClient: nil,
		logger:    slog.Default(),
	}
}

func TestGetStore_UnitTest(t *testing.T) {
	rwc := createTestComponent()

	t.Run("existing resource type", func(t *testing.T) {
		store := rwc.GetStore("services")
		assert.NotNil(t, store)
	})

	t.Run("another existing resource type", func(t *testing.T) {
		store := rwc.GetStore("ingresses")
		assert.NotNil(t, store)
	})

	t.Run("non-existent resource type", func(t *testing.T) {
		store := rwc.GetStore("pods")
		assert.Nil(t, store)
	})

	t.Run("empty string", func(t *testing.T) {
		store := rwc.GetStore("")
		assert.Nil(t, store)
	})
}

func TestGetAllStores_UnitTest(t *testing.T) {
	rwc := createTestComponent()

	stores := rwc.GetAllStores()

	t.Run("returns correct count", func(t *testing.T) {
		assert.Len(t, stores, 2)
	})

	t.Run("contains expected stores", func(t *testing.T) {
		assert.NotNil(t, stores["services"])
		assert.NotNil(t, stores["ingresses"])
	})

	t.Run("returns copy not original", func(t *testing.T) {
		// Modify returned map
		stores["services"] = nil
		// Original should be unchanged
		assert.NotNil(t, rwc.stores["services"])
	})
}

func TestDetermineNamespaces(t *testing.T) {
	k8sClient := client.NewFromClientset(fake.NewClientset(), dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()), "haptic")

	tests := []struct {
		name             string
		resourceTypeName string
		namespaces       []string
		want             []string
	}{
		{
			name:             "haproxy-pods are pinned to the controller namespace",
			resourceTypeName: "haproxy-pods",
			namespaces:       []string{"elsewhere"},
			want:             []string{"haptic"},
		},
		{
			name:             "unscoped resource watches every namespace",
			resourceTypeName: "secrets",
			want:             nil,
		},
		{
			name:             "scoped resource watches its namespaces",
			resourceTypeName: "secrets",
			namespaces:       []string{"team-a", "team-b"},
			want:             []string{"team-a", "team-b"},
		},
		{
			name:             "scoped custom resource watches its namespaces",
			resourceTypeName: "my-custom-resource",
			namespaces:       []string{"team-a"},
			want:             []string{"team-a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resource := coreconfig.WatchedResource{Namespaces: tt.namespaces}
			assert.Equal(t, tt.want, determineNamespaces(tt.resourceTypeName, &resource, k8sClient))
		})
	}
}

// TestStart_EmptyWatchers verifies Start behavior with no watchers configured.
func TestStart_EmptyWatchers(t *testing.T) {
	rwc := &ResourceWatcherComponent{
		watchers: map[string]*watcher.Watcher{},
		stores:   map[string]types.Store{},
		logger:   slog.Default(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// Start should return after context cancellation
	done := make(chan error, 1)
	go func() {
		done <- rwc.Start(ctx)
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(1 * time.Second):
		t.Fatal("Start() did not return after context cancellation")
	}
}

func TestStart_WaitsForWatchersToStop(t *testing.T) {
	k8sClient, blockingWatch := newBlockingPodWatchClient()
	w, err := watcher.New(types.WatcherConfig{
		GVR:       schema.GroupVersionResource{Version: "v1", Resource: "pods"},
		IndexBy:   []string{"metadata.namespace", "metadata.name"},
		StoreType: types.StoreTypeMemory,
		OnChange:  func(types.Store, types.ChangeStats) {},
	}, k8sClient, slog.Default())
	require.NoError(t, err)
	rwc := &ResourceWatcherComponent{
		watchers: map[string]*watcher.Watcher{"pods": w},
		stores:   map[string]types.Store{"pods": w.Store()},
		logger:   slog.Default(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- rwc.Start(ctx)
	}()
	syncCtx, stopSync := context.WithTimeout(t.Context(), time.Second)
	defer stopSync()
	require.NoError(t, rwc.WaitForAllSync(syncCtx))

	cancel()
	select {
	case <-blockingWatch.stopStarted:
	case <-time.After(time.Second):
		t.Fatal("resource watch was not stopped")
	}
	select {
	case err := <-done:
		t.Fatalf("component returned before its watcher stopped: %v", err)
	default:
	}

	close(blockingWatch.release)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("component did not return after its watcher stopped")
	}
}

func TestStart_PropagatesWatcherFailure(t *testing.T) {
	w, err := watcher.New(types.WatcherConfig{
		GVR:       schema.GroupVersionResource{Version: "v1", Resource: "pods"},
		IndexBy:   []string{"metadata.namespace", "metadata.name"},
		StoreType: types.StoreTypeMemory,
		OnChange:  func(types.Store, types.ChangeStats) {},
	}, newFakeClient(), slog.Default())
	require.NoError(t, err)
	require.NoError(t, w.Stop())
	rwc := &ResourceWatcherComponent{
		watchers: map[string]*watcher.Watcher{"pods": w},
		stores:   map[string]types.Store{"pods": w.Store()},
		logger:   slog.Default(),
	}

	err = rwc.Start(t.Context())
	require.Error(t, err)
	assert.ErrorContains(t, err, `watcher "pods" failed`)
	assert.ErrorContains(t, err, "syncing cache")
}

// TestWaitForAllSync_EmptyWatchers verifies WaitForAllSync with no watchers.
func TestWaitForAllSync_EmptyWatchers(t *testing.T) {
	rwc := &ResourceWatcherComponent{
		watchers: map[string]*watcher.Watcher{},
		stores:   map[string]types.Store{},
		logger:   slog.Default(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	// With no watchers, WaitForAllSync should return immediately
	err := rwc.WaitForAllSync(ctx)
	require.NoError(t, err)
}

// TestWaitForAllSync_ContextCancelled verifies WaitForAllSync respects context cancellation.
func TestWaitForAllSync_ContextCancelled(t *testing.T) {
	rwc := &ResourceWatcherComponent{
		watchers: map[string]*watcher.Watcher{},
		stores:   map[string]types.Store{},
		logger:   slog.Default(),
	}

	// Create already-cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Should return immediately without error (no watchers to wait for)
	err := rwc.WaitForAllSync(ctx)
	require.NoError(t, err)
}

func TestNew_NilParameters(t *testing.T) {
	cfg := &coreconfig.Config{}
	bus := busevents.NewEventBus(10)
	logger := slog.Default()

	// Create a mock k8s client (nil is okay for these tests since we're testing nil validation)
	// We use a placeholder that's non-nil but won't be used
	dummyClient := &client.Client{}

	tests := []struct {
		name      string
		cfg       *coreconfig.Config
		k8sClient *client.Client
		bus       *busevents.EventBus
		logger    *slog.Logger
		wantErr   string
	}{
		{
			name:      "nil config",
			cfg:       nil,
			k8sClient: dummyClient,
			bus:       bus,
			logger:    logger,
			wantErr:   "config is nil",
		},
		{
			name:      "nil k8s client",
			cfg:       cfg,
			k8sClient: nil,
			bus:       bus,
			logger:    logger,
			wantErr:   "k8s client is nil",
		},
		{
			name:      "nil event bus",
			cfg:       cfg,
			k8sClient: dummyClient,
			bus:       nil,
			logger:    logger,
			wantErr:   "event bus is nil",
		},
		{
			name:      "nil logger",
			cfg:       cfg,
			k8sClient: dummyClient,
			bus:       bus,
			logger:    nil,
			wantErr:   "logger is nil",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.cfg, tt.k8sClient, tt.bus, tt.logger)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestNew_EmptyConfig(t *testing.T) {
	cfg := &coreconfig.Config{
		WatchedResources: map[string]coreconfig.WatchedResource{},
	}
	bus := busevents.NewEventBus(10)
	logger := slog.Default()

	rwc, err := New(cfg, newFakeClient(), bus, logger)
	require.NoError(t, err)
	require.NotNil(t, rwc)

	// New() always auto-injects a haproxy-pods watcher driven by PodSelector,
	// regardless of WatchedResources content.
	assert.Len(t, rwc.watchers, 1)
	assert.Len(t, rwc.stores, 1)
	assert.Contains(t, rwc.watchers, "haproxy-pods")
	assert.Contains(t, rwc.stores, "haproxy-pods")
}

func TestNew_InvalidResource(t *testing.T) {
	cfg := &coreconfig.Config{
		WatchedResources: map[string]coreconfig.WatchedResource{
			"invalid": {
				// Missing APIVersion and Kind
				IndexBy: []string{"metadata.namespace"},
			},
		},
	}
	bus := busevents.NewEventBus(10)
	logger := slog.Default()

	_, err := New(cfg, newFakeClient(), bus, logger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid resource")
}

func TestGetStore(t *testing.T) {
	cfg := &coreconfig.Config{
		WatchedResources: map[string]coreconfig.WatchedResource{
			"services": {
				APIVersion: "v1",
				Resources:  "services",
				IndexBy:    []string{"metadata.namespace"},
			},
		},
	}
	bus := busevents.NewEventBus(10)
	logger := slog.Default()

	rwc, err := New(cfg, newFakeClient(), bus, logger)
	require.NoError(t, err)

	// Existing resource type
	assert.NotNil(t, rwc.GetStore("services"))

	// Auto-injected haproxy-pods is also addressable.
	assert.NotNil(t, rwc.GetStore("haproxy-pods"))

	// Non-existent resource type
	assert.Nil(t, rwc.GetStore("ingresses"))
}

func TestGetAllStores(t *testing.T) {
	cfg := &coreconfig.Config{
		WatchedResources: map[string]coreconfig.WatchedResource{
			"services": {
				APIVersion: "v1",
				Resources:  "services",
				IndexBy:    []string{"metadata.namespace"},
			},
			"pods": {
				APIVersion: "v1",
				Resources:  "pods",
				IndexBy:    []string{"metadata.namespace"},
			},
		},
	}
	bus := busevents.NewEventBus(10)
	logger := slog.Default()

	rwc, err := New(cfg, newFakeClient(), bus, logger)
	require.NoError(t, err)

	// Two configured resources + one auto-injected haproxy-pods watcher.
	stores := rwc.GetAllStores()
	assert.Len(t, stores, 3)
	assert.NotNil(t, stores["services"])
	assert.NotNil(t, stores["pods"])
	assert.NotNil(t, stores["haproxy-pods"])

	// Verify it returns a copy (modifying return value doesn't affect internal state)
	stores["services"] = nil
	assert.NotNil(t, rwc.stores["services"])
}

// Event-publishing coverage (ResourceIndexUpdatedEvent / ResourceSyncCompleteEvent
// firing when resources change or initial sync completes) lives end-to-end in
// the integration suite under tests/, where a real informer drives the watcher.
// A unit-level recreation would need either a fake informer or a hand-rolled
// dispatch harness, neither of which is cheap enough to justify here.

// TestStart is intentionally a skip-only placeholder. Start() drives real
// informers, and dynamicfake.NewSimpleDynamicClient panics when the
// reflector tries to LIST a resource whose list-kind is not pre-registered
// on the fake's scheme. Setting that up per resource would mean teaching
// every fake fixture about every WatchedResource — the integration suite
// already covers Start() with a real informer, which is the right place.
func TestStart(t *testing.T) {
	t.Skip("Start() drives real informers; covered by tests/integration")
}
