package watcher

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/types"
)

func namespacedConfigMap(namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"namespace": namespace, "name": name},
	}}
}

func storedIdentities(t *testing.T, store types.Store) []string {
	t.Helper()
	items, err := store.List()
	require.NoError(t, err)
	identities := make([]string, 0, len(items))
	for _, item := range items {
		metadata := item.(map[string]any)["metadata"].(map[string]any)
		identities = append(identities, metadata["namespace"].(string)+"/"+metadata["name"].(string))
	}
	slices.Sort(identities)
	return identities
}

// requestedNamespaces returns the distinct namespaces of every request with
// the verb; "" is a request across all namespaces.
func requestedNamespaces(client *dynamicfake.FakeDynamicClient, verb string) []string {
	var namespaces []string
	for _, action := range client.Actions() {
		if action.GetVerb() == verb {
			namespaces = append(namespaces, action.GetNamespace())
		}
	}
	slices.Sort(namespaces)
	return slices.Compact(namespaces)
}

func TestWatcher_NamespacesScopeEveryRequest(t *testing.T) {
	tests := []struct {
		name       string
		namespaces []string
		want       []string
		requested  []string
	}{
		{
			name:      "unset watches all namespaces",
			want:      []string{"a/one", "b/two", "c/three"},
			requested: []string{""},
		},
		{
			name:       "listed namespaces only",
			namespaces: []string{"a", "b"},
			want:       []string{"a/one", "b/two"},
			requested:  []string{"a", "b"},
		},
		{
			name:       "single namespace",
			namespaces: []string{"c"},
			want:       []string{"c/three"},
			requested:  []string{"c"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k8sClient := newTestClient(t,
				namespacedConfigMap("a", "one"), namespacedConfigMap("b", "two"), namespacedConfigMap("c", "three"))
			cfg := validWatcherConfig()
			cfg.Namespaces = tt.namespaces
			w, err := New(cfg, k8sClient, slog.Default())
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			go func() { _ = w.Start(ctx) }()
			_, err = w.WaitForSync(ctx)
			require.NoError(t, err)

			assert.Equal(t, tt.want, storedIdentities(t, w.Store()))
			fake := k8sClient.DynamicClient().(*dynamicfake.FakeDynamicClient)
			require.Eventually(t, func() bool {
				return slices.Equal(tt.requested, requestedNamespaces(fake, "watch"))
			}, 5*time.Second, 10*time.Millisecond, "every informer opens a watch in its own namespace")
			assert.Equal(t, tt.requested, requestedNamespaces(fake, "list"))

			fresh, err := w.FreshStore(ctx)
			require.NoError(t, err)
			assert.Equal(t, tt.want, storedIdentities(t, fresh))
			assert.Equal(t, tt.requested, requestedNamespaces(fake, "list"))
		})
	}
}

func TestWatcher_NamespacesFollowLiveChanges(t *testing.T) {
	k8sClient := newTestClient(t, namespacedConfigMap("a", "one"))
	cfg := validWatcherConfig()
	cfg.Namespaces = []string{"a", "b"}
	w, err := New(cfg, k8sClient, slog.Default())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = w.Start(ctx) }()
	_, err = w.WaitForSync(ctx)
	require.NoError(t, err)
	fake := k8sClient.DynamicClient().(*dynamicfake.FakeDynamicClient)
	require.Eventually(t, func() bool {
		return slices.Equal([]string{"a", "b"}, requestedNamespaces(fake, "watch"))
	}, 5*time.Second, 10*time.Millisecond)

	resource := k8sClient.DynamicClient().Resource(configMapGVR)
	for _, object := range []*unstructured.Unstructured{namespacedConfigMap("c", "ignored"), namespacedConfigMap("b", "two")} {
		_, err := resource.Namespace(object.GetNamespace()).Create(ctx, object, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	require.Eventually(t, func() bool {
		return slices.Equal([]string{"a/one", "b/two"}, storedIdentities(t, w.Store()))
	}, 5*time.Second, 10*time.Millisecond, "b/two arrives; c/ignored never does")
}

func TestWatcher_NamespacesSyncWaitsForEveryNamespace(t *testing.T) {
	k8sClient := newTestClient(t)
	fake := k8sClient.DynamicClient().(*dynamicfake.FakeDynamicClient)
	release := make(chan struct{})
	fake.PrependReactor("list", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() == "b" {
			<-release
		}
		return false, nil, nil
	})
	cfg := validWatcherConfig()
	cfg.Namespaces = []string{"a", "b"}
	w, err := New(cfg, k8sClient, slog.Default())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	pending, stop := context.WithTimeout(ctx, 200*time.Millisecond)
	_, err = w.WaitForSync(pending)
	stop()
	require.Error(t, err, "namespace b has not listed yet")
	close(release)
	_, err = w.WaitForSync(ctx)
	require.NoError(t, err)
}
