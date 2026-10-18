// Copyright 2025 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

// TestCachedStore_ListCached_ReturnsOnlyWarmEntries pins the contract
// `rendercontext.StoreWrapper` depends on for LazySnapshot priming:
// ListCached must hand back exactly the LRU's currently-warm
// (non-expired) entries WITHOUT contacting the API. A regression that
// shadowed this onto the API-fetching List() path would silently
// re-introduce the "Listing cached store" WARN — the exact bug
// LazySnapshot exists to avoid.
func TestCachedStore_ListCached_ReturnsOnlyWarmEntries(t *testing.T) {
	scheme := runtime.NewScheme()
	resource := createTestResource("default", "warm")

	gvr := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}

	store, err := NewCachedStore(&CachedStoreConfig{
		NumKeys:  2,
		CacheTTL: 5 * time.Minute,
		// Fake client has NO resources — confirms ListCached doesn't
		// call out to the API. If it did, every entry would 404
		// because the fake's tracker is empty.
		Client:  fake.NewSimpleDynamicClient(scheme),
		GVR:     gvr,
		Indexer: createTestIndexer(),
	})
	require.NoError(t, err)

	// Empty cache: ListCached returns empty without touching the API.
	cached, err := store.ListCached()
	require.NoError(t, err)
	assert.Empty(t, cached,
		"ListCached on an empty store must return [] — NOT call out to the API")

	require.NoError(t, store.Add(resource, []string{"default", "warm"}))

	cached, err = store.ListCached()
	require.NoError(t, err)
	require.Len(t, cached, 1,
		"ListCached must surface entries cached by Add")
	assert.Equal(t, "warm", cached[0].(*unstructured.Unstructured).GetName())
}

// TestCachedStore_ListCached_SkipsExpiredEntries pins the TTL gate:
// expired entries must not surface through ListCached. The
// StoreWrapper's lazy-mode prime would otherwise hand stale data to
// templates that the Store.Get fast path would have refreshed.
func TestCachedStore_ListCached_SkipsExpiredEntries(t *testing.T) {
	scheme := runtime.NewScheme()
	resource := createTestResource("default", "expired")

	store, err := NewCachedStore(&CachedStoreConfig{
		NumKeys: 2,
		// Very short TTL so we can blow past it deterministically.
		CacheTTL: 10 * time.Millisecond,
		Client:   fake.NewSimpleDynamicClient(scheme),
		GVR:      schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"},
		Indexer:  createTestIndexer(),
	})
	require.NoError(t, err)

	require.NoError(t, store.Add(resource, []string{"default", "expired"}))

	// Fresh entry is visible.
	cached, _ := store.ListCached()
	require.Len(t, cached, 1)

	// After TTL elapses, the entry is filtered out — even though the
	// LRU still holds it (eviction is independent of expiry).
	time.Sleep(20 * time.Millisecond)
	cached, err = store.ListCached()
	require.NoError(t, err)
	assert.Empty(t, cached,
		"expired entries must not surface through ListCached — "+
			"stale data leaking into the StoreWrapper's primed snapshot "+
			"would defeat the wrapper's per-render consistency claim")
}
