package watcher

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/types"
)

func TestFreshStoreUsesWatchSelectionAndProjection(t *testing.T) {
	for _, storeType := range []types.StoreType{types.StoreTypeMemory, types.StoreTypeCached} {
		t.Run(storeType.String(), func(t *testing.T) {
			objects := make([]runtime.Object, 0, 4)
			for _, identity := range []struct{ namespace, name, label, field string }{
				{"default", "selected", "yes", "yes"}, {"other", "namespace", "yes", "yes"},
				{"default", "label", "no", "yes"}, {"default", "field", "yes", "no"},
			} {
				objects = append(objects, &unstructured.Unstructured{Object: map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"namespace": identity.namespace, "name": identity.name, "labels": map[string]any{"selected": identity.label}},
					"data":     map[string]any{"selected": identity.field, "ignored": "omit"},
				}})
			}
			client := newTestClient(t, objects...)
			cfg := validWatcherConfig()
			cfg.StoreType = storeType
			cfg.NamespacedWatch = true
			cfg.LabelSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"selected": "yes"}}
			cfg.FieldSelector = "data.selected=yes"
			cfg.IgnoreFields = []string{"data.ignored"}
			watcher, err := New(cfg, client, nil)
			require.NoError(t, err)
			fresh, err := watcher.FreshStore(t.Context())
			require.NoError(t, err)
			items, err := fresh.Get("default", "selected")
			require.NoError(t, err)
			require.Len(t, items, 1)
			assert.Equal(t, map[string]any{"selected": "yes"}, items[0].(map[string]any)["data"])
			all, err := fresh.List()
			require.NoError(t, err)
			assert.Len(t, all, 1)
			cached, err := watcher.Store().List()
			require.NoError(t, err)
			assert.Empty(t, cached)
		})
	}
}

func TestFreshStoreRejectsIncompleteLists(t *testing.T) {
	client := newTestClient(t)
	client.DynamicClient().(*dynamicfake.FakeDynamicClient).PrependReactor("list", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("list failed")
	})
	watcher, err := New(validWatcherConfig(), client, nil)
	require.NoError(t, err)
	fresh, err := watcher.FreshStore(t.Context())
	require.ErrorContains(t, err, "list failed")
	assert.Nil(t, fresh)
}

func TestFreshStoreStopsOnCancellation(t *testing.T) {
	watcher, err := New(validWatcherConfig(), newTestClient(t), nil)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	fresh, err := watcher.FreshStore(ctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, fresh)
}

func TestFreshStoreReadsEveryPageOrRejectsTheSnapshot(t *testing.T) {
	for _, failSecondPage := range []bool{false, true} {
		t.Run(fmt.Sprint(failSecondPage), func(t *testing.T) {
			client := newTestClient(t)
			calls := 0
			client.DynamicClient().(*dynamicfake.FakeDynamicClient).PrependReactor("list", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
				options := action.(k8stesting.ListActionImpl).GetListOptions()
				assert.Empty(t, options.ResourceVersion)
				calls++
				if calls == 2 {
					assert.Equal(t, "next", options.Continue)
					if failSecondPage {
						return true, nil, errors.New("second page failed")
					}
				}
				page := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{{Object: map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"namespace": "default", "name": fmt.Sprint(calls)},
				}}}}
				if calls == 1 {
					page.SetContinue("next")
				}
				return true, page, nil
			})
			cfg := validWatcherConfig()
			cfg.Namespaces = []string{"default"}
			watcher, err := New(cfg, client, nil)
			require.NoError(t, err)
			fresh, err := watcher.FreshStore(t.Context())
			assert.Equal(t, 2, calls)
			if failSecondPage {
				require.ErrorContains(t, err, "second page failed")
				assert.Nil(t, fresh)
			} else {
				require.NoError(t, err)
				items, err := fresh.List()
				require.NoError(t, err)
				assert.Len(t, items, 2)
			}
		})
	}
}
