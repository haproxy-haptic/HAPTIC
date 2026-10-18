package watcher

import (
	"context"
	"fmt"

	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/indexer"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/store"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/types"
)

// FreshStore reads persisted objects without changing the informer or its store.
func (w *Watcher) FreshStore(ctx context.Context) (types.Store, error) {
	fresh := store.NewMemoryStore(len(w.config.IndexBy))
	for _, namespace := range w.watchedNamespaces() {
		if err := w.listFresh(ctx, namespace, fresh); err != nil {
			return nil, err
		}
	}
	return fresh, nil
}

func (w *Watcher) listFresh(ctx context.Context, namespace string, fresh types.Store) error {
	options := metav1.ListOptions{Limit: 500}
	w.applyListOptions(&options)
	resource := w.client.DynamicClient().Resource(w.config.GVR).Namespace(namespace)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		objects, err := resource.List(ctx, options)
		if err != nil {
			return fmt.Errorf("listing %s: %w", w.config.GVR, err)
		}
		for i := range objects.Items {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := w.addFreshResource(fresh, &objects.Items[i]); err != nil {
				return err
			}
		}
		options.Continue = objects.GetContinue()
		if options.Continue == "" {
			return nil
		}
	}
}

func (w *Watcher) addFreshResource(fresh types.Store, object *unstructured.Unstructured) error {
	if err := w.indexer.FilterFields(object); err != nil {
		return err
	}
	converted := indexer.ConvertResource(object)
	if w.fieldSelectorMatcher != nil {
		matches, err := w.fieldSelectorMatcher.Matches(converted)
		if err != nil {
			return err
		}
		if !matches {
			return nil
		}
	}
	keys, err := w.indexer.ExtractKeys(converted)
	if err != nil {
		return fmt.Errorf("indexing %s %s/%s: %w", w.config.GVR, object.GetNamespace(), object.GetName(), err)
	}
	return fresh.Add(converted, keys)
}
