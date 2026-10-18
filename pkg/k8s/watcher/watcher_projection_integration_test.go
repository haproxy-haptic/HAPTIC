package watcher

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/types"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
)

// A CachedStore (on-demand) watcher must install the body-stripping projection
// on its informer so the informer's cache no longer retains full bodies, while
// store reads still serve the full body via the live API GET. See ADR-0012.
func TestNew_CachedStore_InformerProjectsButServesFullBody(t *testing.T) {
	cm := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"namespace": "default", "name": "big-cm"},
		"data":       map[string]any{"blob": "HEAVYVALUE"},
	}}
	k8sClient := newTestClient(t, cm)

	cfg := validWatcherConfig()
	cfg.StoreType = types.StoreTypeCached
	cfg.CacheTTL = time.Minute

	w, err := New(cfg, k8sClient, slog.Default())
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	_, err = w.WaitForSync(ctx)
	require.NoError(t, err)

	// The informer's own cache holds the body-stripped projection.
	items := w.informers[0].informer.GetStore().List()
	require.Len(t, items, 1)
	stored, ok := items[0].(*unstructured.Unstructured)
	require.True(t, ok, "informer cache item should be *unstructured.Unstructured")
	_, hasData, _ := unstructured.NestedMap(stored.Object, "data")
	assert.False(t, hasData, "informer cache should hold the body-stripped projection (no data)")
	assert.Equal(t, "big-cm", stored.GetName(), "identity survives projection")

	// The render read still gets the FULL body via the live API GET. The store
	// returns the converted resource as a map[string]any (ProcessResult.
	// ConvertedResource), not the *unstructured the informer caches.
	results, err := w.Store().Get("default", "big-cm")
	require.NoError(t, err)
	require.Len(t, results, 1)
	gotMap, ok := results[0].(map[string]any)
	require.True(t, ok, "store returns the converted map[string]any body")
	blob, found, _ := unstructured.NestedString(gotMap, "data", "blob")
	assert.True(t, found, "store read must serve the full body via live GET")
	assert.Equal(t, "HEAVYVALUE", blob)
}

func TestNewCachedStoreSnapshotKeepsIgnoredResourceVersionAsInternalProof(t *testing.T) {
	cm := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"namespace":       "default",
			"name":            "proof",
			"resourceVersion": "17",
		},
		"data": map[string]any{"blob": "value"},
	}}
	cfg := validWatcherConfig()
	cfg.StoreType = types.StoreTypeCached
	cfg.CacheTTL = time.Minute
	cfg.IgnoreFields = []string{"metadata.resourceVersion"}
	w, err := New(cfg, newTestClient(t, cm), slog.Default())
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = w.Start(ctx) }()
	_, err = w.WaitForSync(ctx)
	require.NoError(t, err)

	provider, ok := w.Store().(stores.SnapshotProvider)
	require.True(t, ok)
	snapshot, err := provider.Pin()
	require.NoError(t, err)
	items, err := snapshot.Get("default", "proof")
	require.NoError(t, err)
	require.Len(t, items, 1)
	resource, ok := items[0].(map[string]any)
	require.True(t, ok)
	blob, found, err := unstructured.NestedString(resource, "data", "blob")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "value", blob)
	_, found, err = unstructured.NestedString(resource, "metadata", "resourceVersion")
	require.NoError(t, err)
	require.False(t, found)
}

// Regression guard for the generic case the bundled chart doesn't exercise: a
// CachedStore watcher with a NON-metadata index root AND a field selector whose
// root is also non-metadata. Projection must retain those roots so that (a)
// client-side field-selector evaluation runs against a husk that still carries
// the selected field, and (b) key extraction works on the projected object.
// If projectionRoots ever dropped a selector/index root, the handler would
// silently evaluate the selector against a stripped husk (Matches → false) and
// the matching object would never reach the store — a silent regression this
// test catches. See ADR-0012.
func TestNew_CachedStore_ProjectionRetainsFieldSelectorAndIndexRoots(t *testing.T) {
	// Matches the field selector; carries a heavy data field to strip.
	cmMatch := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"namespace": "default", "name": "cm-frontend"},
		"spec":       map[string]any{"tier": "frontend"},
		"data":       map[string]any{"blob": "HEAVYVALUE"},
	}}
	// Does NOT match the field selector → must be filtered out of the store.
	cmNoMatch := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"namespace": "default", "name": "cm-backend"},
		"spec":       map[string]any{"tier": "backend"},
		"data":       map[string]any{"blob": "OTHER"},
	}}
	k8sClient := newTestClient(t, cmMatch, cmNoMatch)

	cfg := validWatcherConfig()
	cfg.StoreType = types.StoreTypeCached
	cfg.CacheTTL = time.Minute
	// Non-metadata index root (spec) + a spec-rooted field selector: projection
	// must keep `spec` for BOTH key extraction and selector evaluation.
	cfg.IndexBy = []string{"spec.tier", "metadata.name"}
	cfg.FieldSelector = "spec.tier=frontend"

	w, err := New(cfg, k8sClient, slog.Default())
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	_, err = w.WaitForSync(ctx)
	require.NoError(t, err)

	// Field selector evaluated correctly on the PROJECTED object: the matching
	// object is indexed under its spec-derived key, and the read serves the
	// full body.
	matched, err := w.Store().Get("frontend", "cm-frontend")
	require.NoError(t, err)
	require.Len(t, matched, 1, "field-selector-matching object must be indexed under its spec-derived key")
	gotMap, ok := matched[0].(map[string]any)
	require.True(t, ok)
	blob, found, _ := unstructured.NestedString(gotMap, "data", "blob")
	assert.True(t, found, "store serves the full body via live GET")
	assert.Equal(t, "HEAVYVALUE", blob)

	// The non-matching object was filtered out by the field selector.
	noMatch, err := w.Store().Get("backend", "cm-backend")
	require.NoError(t, err)
	assert.Empty(t, noMatch, "field-selector-rejected object must not be stored")

	// The informer cache holds body-stripped husks: spec retained (index +
	// selector root), data dropped.
	items := w.informers[0].informer.GetStore().List()
	require.NotEmpty(t, items)
	for _, item := range items {
		u, ok := item.(*unstructured.Unstructured)
		require.True(t, ok)
		_, hasData, _ := unstructured.NestedMap(u.Object, "data")
		assert.False(t, hasData, "informer husk must drop the heavy data field")
		_, hasSpec, _ := unstructured.NestedMap(u.Object, "spec")
		assert.True(t, hasSpec, "informer husk must retain spec (index + field-selector root)")
	}
}
