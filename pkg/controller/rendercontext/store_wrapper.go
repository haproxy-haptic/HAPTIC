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

package rendercontext

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strconv"
	"sync"

	"github.com/haproxy-haptic/HAPTIC/pkg/core/logging"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/indexer"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

// Verify StoreWrapper implements templating.ResourceStore at compile time.
// This enables Scriggo templates to call methods directly on resource stores:
//
//	{% for _, ing := range resources.ingresses.List() %}
var _ templating.ResourceStore = (*StoreWrapper)(nil)

// toString converts various types to string for template compatibility.
//
// This helper handles type conversions:
// - string: returned as-is
// - fmt.Stringer: any type with String() method
// - other types: formatted using fmt.Sprintf
//
// This allows template methods to accept any arguments.
func toString(v any) string {
	switch val := v.(type) {
	case string:
		// Fast path for regular strings
		return val
	case fmt.Stringer:
		// Handles types with String() method
		return val.String()
	default:
		// Fallback: format as string
		return fmt.Sprint(v)
	}
}

// StoreWrapper exposes one immutable resource view to a template render.
// Lazy snapshots load exact and prefix reads on demand; List still resolves the
// complete store because cache warmth isn't a semantic input.
type StoreWrapper struct {
	Store          stores.Store
	ResourceType   string
	Logger         *slog.Logger
	readContext    context.Context
	resourceErrors *ResourceErrorCollector

	// IndexBy mirrors the JSONPath expressions the underlying store uses
	// to index resources. Required for snapshot-served Fetch/GetSingle.
	IndexBy []string

	// Scope fails the render for a lookup outside the namespaces the store
	// was filled from; nil allows every namespace.
	Scope *NamespaceScope

	// LazySnapshot defers the eager Store.List() until List() is
	// actually called. Set when the underlying store is a CachedStore
	// (WatchedResources[name].Store == "on-demand"). See type doc.
	LazySnapshot bool

	SnapshotView        StoreSnapshotView
	DerivedView         *DerivedResourceView
	MemoizeSnapshotView bool

	cacheMu          sync.Mutex
	loaded           bool
	listed           bool
	indexer          *indexer.Indexer // lazy mode: used to extract keys from items added incrementally
	snapshot         []any
	snapshotByKey    map[string][]any // encoded index components → matching items
	snapshotViewGets map[string][]any
	resolvedKeys     map[string]struct{}
}

// StoreSnapshotView supplies transaction-pinned reads for incremental components.
type StoreSnapshotView interface {
	List(resourceType string, store stores.Store) ([]any, error)
	Get(resourceType string, store stores.Store, keys ...string) ([]any, error)
}

type contextualStoreSnapshotView interface {
	ListContext(context.Context, string, stores.Store) ([]any, error)
	GetContext(context.Context, string, stores.Store, ...string) ([]any, error)
}

type storeInvocationGuard interface {
	BeginStoreInvocation(context.Context) (context.Context, func(), error)
}

type boundStoreInvocationGuard interface {
	BeginBoundStoreInvocation(
		context.Context,
		templating.IncrementalResourceInvocationLease,
	) (context.Context, func(), error)
}

// StoreLookupKeyNormalizer replaces legacy lookup-key coercion for a snapshot view.
type StoreLookupKeyNormalizer interface {
	NormalizeLookupKeys(resourceType string, keys []any) ([]string, error)
}

// StoreLookupKeySource exposes synchronous lookup arguments without materializing a slice.
type StoreLookupKeySource interface {
	Len() int
	Value(index int) any
}

// StoreLookupReflectKeySource preserves an argument's reflection value without boxing it.
type StoreLookupReflectKeySource interface {
	StoreLookupKeySource
	ReflectValue(index int) reflect.Value
}

// StoreLookupKeySourceNormalizer normalizes a non-retainable lookup-key source synchronously.
type StoreLookupKeySourceNormalizer interface {
	NormalizeLookupKeySource(resourceType string, keys StoreLookupKeySource) ([]string, error)
}

type selectiveStoreSnapshotView interface {
	Supports(resourceType string) bool
}

type storeReadContextProvider interface {
	StoreReadContext() context.Context
}

type storeMaterializationPolicy interface {
	MemoizeStoreMaterialization() bool
}

type storeItemMaterializationPolicy interface {
	MemoizeStoreItems() bool
}

type storeValueIsolationPolicy interface {
	PreserveStoreValues() bool
}

// CloneWithSnapshotView returns an empty per-render cache over the supplied view.
func (w *StoreWrapper) CloneWithSnapshotView(view StoreSnapshotView, memoize bool) *StoreWrapper {
	if w == nil {
		return nil
	}
	return w.CloneWithSnapshotViewContext(w.readContext, view, memoize)
}

// CloneWithSnapshotViewContext returns an empty per-render cache with a new read context.
func (w *StoreWrapper) CloneWithSnapshotViewContext(
	ctx context.Context,
	view StoreSnapshotView,
	memoize bool,
) *StoreWrapper {
	if w == nil {
		return nil
	}
	return &StoreWrapper{
		Store:               w.Store,
		ResourceType:        w.ResourceType,
		Logger:              w.Logger,
		readContext:         ctx,
		resourceErrors:      w.resourceErrors,
		IndexBy:             slices.Clone(w.IndexBy),
		Scope:               w.Scope,
		LazySnapshot:        w.LazySnapshot,
		SnapshotView:        view,
		DerivedView:         w.DerivedView,
		MemoizeSnapshotView: memoize,
	}
}

func (w *StoreWrapper) usesSnapshotView() bool {
	if w.SnapshotView == nil {
		return false
	}
	selective, ok := w.SnapshotView.(selectiveStoreSnapshotView)
	return !ok || selective.Supports(w.ResourceType)
}

func (w *StoreWrapper) memoizeStoreMaterialization() bool {
	policy, ok := w.SnapshotView.(storeMaterializationPolicy)
	return !ok || policy.MemoizeStoreMaterialization()
}

func (w *StoreWrapper) memoizeStoreItems() bool {
	policy, ok := w.SnapshotView.(storeItemMaterializationPolicy)
	return !ok || policy.MemoizeStoreItems()
}

func (w *StoreWrapper) storeContext() context.Context {
	if provider, ok := w.SnapshotView.(storeReadContextProvider); ok {
		if ctx := provider.StoreReadContext(); ctx != nil {
			return ctx
		}
	}
	if w.readContext != nil {
		return w.readContext
	}
	return context.Background()
}

func (w *StoreWrapper) beginStoreInvocation(ctx context.Context) (context.Context, func(), error) {
	if ctx == nil {
		ctx = w.storeContext()
	}
	if guard, ok := w.SnapshotView.(storeInvocationGuard); ok {
		return guard.BeginStoreInvocation(ctx)
	}
	return ctx, func() {}, nil
}

func (w *StoreWrapper) supportsBoundStoreInvocation() bool {
	if w == nil || w.memoizeStoreMaterialization() {
		return false
	}
	if w.supportsDirectBoundStoreInvocation() {
		return true
	}
	_, ok := w.SnapshotView.(boundStoreInvocationGuard)
	return ok
}

func (w *StoreWrapper) beginBoundStoreInvocation(
	ctx context.Context,
	lease templating.IncrementalResourceInvocationLease,
) (context.Context, func(), error) {
	if w == nil || lease == nil {
		return nil, nil, errors.New("bound resource invocation is unavailable")
	}
	if ctx == nil {
		ctx = w.storeContext()
	}
	guard, ok := w.SnapshotView.(boundStoreInvocationGuard)
	if !ok || w.memoizeStoreMaterialization() {
		return nil, nil, errors.New("bound resource invocation is unsupported")
	}
	return guard.BeginBoundStoreInvocation(ctx, lease)
}

func (w *StoreWrapper) getStore(keys ...string) ([]any, error) {
	return stores.GetContext(w.storeContext(), w.Store, keys...)
}

func (w *StoreWrapper) listStore() ([]any, error) {
	return stores.ListContext(w.storeContext(), w.Store)
}

func (w *StoreWrapper) warnReadFailure(message string, args ...any) {
	if w.storeContext().Err() == nil {
		w.Logger.Warn(message, args...)
	}
}

func (w *StoreWrapper) recordReadFailure(err error) {
	w.resourceErrors.Record(err)
}

func (w *StoreWrapper) project(items []any, operation string) []any {
	if w.DerivedView == nil {
		return items
	}
	projected, err := w.DerivedView.Project(w.ResourceType, items)
	if err != nil {
		w.recordReadFailure(fmt.Errorf("resource %q %s could not apply its derived view: %w", w.ResourceType, operation, err))
		return nil
	}
	return projected
}

func (w *StoreWrapper) prepareSnapshotIndex() {
	if len(w.IndexBy) == 0 {
		w.Logger.Warn("StoreWrapper has no IndexBy; Fetch/GetSingle will bypass the snapshot",
			"resource_type", w.ResourceType)
		return
	}

	idx, err := indexer.New(indexer.Config{IndexBy: w.IndexBy})
	if err != nil {
		w.recordReadFailure(fmt.Errorf("resource %q could not build its snapshot index: %w", w.ResourceType, err))
		w.Logger.Warn("Failed to build snapshot indexer; Fetch/GetSingle will bypass the snapshot",
			"resource_type", w.ResourceType,
			"index_by", w.IndexBy,
			"error", err)
		return
	}
	w.indexer = idx
	w.snapshotByKey = map[string][]any{}
	w.resolvedKeys = map[string]struct{}{}
}

func (w *StoreWrapper) loadInitialSnapshot() []any {
	if w.MemoizeSnapshotView && w.usesSnapshotView() {
		if w.LazySnapshot {
			return nil
		}
		items, err := w.SnapshotView.List(w.ResourceType, w.Store)
		if err != nil {
			w.recordReadFailure(fmt.Errorf("resource %q List failed: %w", w.ResourceType, err))
			return nil
		}
		return w.project(w.cloneStoreItems(items, "List"), "List")
	}
	if !w.LazySnapshot {
		items, err := w.listStore()
		if err != nil {
			w.recordReadFailure(fmt.Errorf("resource %q List failed: %w", w.ResourceType, err))
			w.warnReadFailure("Failed to list resources for snapshot",
				"resource_type", w.ResourceType, "error", err)
			return nil
		}
		return w.cloneStoreItems(items, "List")
	}

	return nil
}

func (w *StoreWrapper) cloneStoreItems(items []any, operation string) []any {
	if policy, ok := w.SnapshotView.(storeValueIsolationPolicy); ok && policy.PreserveStoreValues() {
		return items
	}
	cloned, err := cloneTemplateItems(items)
	if err != nil {
		w.recordReadFailure(fmt.Errorf("resource %q %s could not be isolated: %w", w.ResourceType, operation, err))
		return nil
	}
	return cloned
}

func cloneTemplateItems(items []any) ([]any, error) {
	cloned := make([]any, len(items))
	for i, item := range items {
		value, err := cloneTemplateValue(item)
		if err != nil {
			return nil, fmt.Errorf("item %d: %w", i, err)
		}
		cloned[i] = value
	}
	return cloned, nil
}

func cloneTemplateValue(value any) (any, error) {
	switch typed := value.(type) {
	case nil, string, bool, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, float32, float64, json.Number:
		return typed, nil
	case map[string]any:
		cloned := make(map[string]any, len(typed))
		for key, item := range typed {
			value, err := cloneTemplateValue(item)
			if err != nil {
				return nil, err
			}
			cloned[key] = value
		}
		return cloned, nil
	case []any:
		cloned := make([]any, len(typed))
		for i, item := range typed {
			value, err := cloneTemplateValue(item)
			if err != nil {
				return nil, err
			}
			cloned[i] = value
		}
		return cloned, nil
	}

	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var cloned any
	if err := decoder.Decode(&cloned); err != nil {
		return nil, err
	}
	return normalizeTemplateNumbers(cloned)
}

func normalizeTemplateNumbers(value any) (any, error) {
	switch typed := value.(type) {
	case json.Number:
		if integer, err := strconv.ParseInt(string(typed), 10, 64); err == nil {
			return integer, nil
		}
		if integer, err := strconv.ParseUint(string(typed), 10, 64); err == nil {
			return integer, nil
		}
		decimal, err := strconv.ParseFloat(string(typed), 64)
		if err != nil {
			return nil, err
		}
		return decimal, nil
	case map[string]any:
		for key, item := range typed {
			normalized, err := normalizeTemplateNumbers(item)
			if err != nil {
				return nil, err
			}
			typed[key] = normalized
		}
	case []any:
		for i, item := range typed {
			normalized, err := normalizeTemplateNumbers(item)
			if err != nil {
				return nil, err
			}
			typed[i] = normalized
		}
	}
	return value, nil
}

// loadSnapshot initializes the per-render index without forcing a lazy List.
// Caller must hold cacheMu.
func (w *StoreWrapper) loadSnapshot() {
	if w.loaded {
		return
	}
	w.loaded = true
	w.prepareSnapshotIndex()
	items := w.loadInitialSnapshot()
	w.snapshot = items
	w.listed = !w.LazySnapshot
	if w.snapshotByKey == nil {
		return
	}
	for _, item := range items {
		w.indexItemLocked(item)
	}
}

func (w *StoreWrapper) getWithoutSnapshotIndex(stringKeys []string, op string) []any {
	items, err := w.getStore(stringKeys...)
	if err != nil {
		w.recordReadFailure(fmt.Errorf("resource %q %s lookup %q failed: %w", w.ResourceType, op, stringKeys, err))
		w.warnReadFailure("Failed to get resources from store (no snapshot index)",
			"resource_type", w.ResourceType,
			"op", op,
			"keys", stringKeys,
			"error", err)
		return []any{}
	}
	w.Logger.Log(context.Background(), logging.LevelTrace, "store get called (live, no snapshot)",
		"resource_type", w.ResourceType,
		"op", op,
		"keys", stringKeys,
		"found_count", len(items))
	return w.cloneStoreItems(items, op)
}

func (w *StoreWrapper) getExact(stringKeys []string, op string) []any {
	composite := indexer.EncodeKey(stringKeys)
	if !w.LazySnapshot || w.listed {
		items := w.snapshotByKey[composite]
		w.Logger.Log(context.Background(), logging.LevelTrace, "store get called (snapshot, exact)",
			"resource_type", w.ResourceType,
			"op", op,
			"keys", stringKeys,
			"found_count", len(items))
		return items
	}
	if _, resolved := w.resolvedKeys[composite]; resolved {
		items := w.snapshotByKey[composite]
		w.Logger.Log(context.Background(), logging.LevelTrace, "store get called (snapshot, exact)",
			"resource_type", w.ResourceType,
			"op", op,
			"keys", stringKeys,
			"found_count", len(items))
		return items
	}

	items, err := w.getStore(stringKeys...)
	if err != nil {
		w.recordReadFailure(fmt.Errorf("resource %q %s lookup %q failed: %w", w.ResourceType, op, stringKeys, err))
		w.warnReadFailure("Failed to fetch single key for lazy snapshot",
			"resource_type", w.ResourceType,
			"op", op,
			"keys", stringKeys,
			"error", err)
		return []any{}
	}
	items = w.cloneStoreItems(items, op)
	w.replaceSnapshotBucketLocked(composite, items)
	w.resolvedKeys[composite] = struct{}{}
	if _, indexed := w.snapshotByKey[composite]; !indexed {
		w.snapshotByKey[composite] = []any{}
	}
	w.Logger.Log(context.Background(), logging.LevelTrace, "store get called (lazy fetch)",
		"resource_type", w.ResourceType,
		"op", op,
		"keys", stringKeys,
		"found_count", len(items))
	return items
}

func (w *StoreWrapper) replaceSnapshotBucketLocked(composite string, items []any) {
	remaining := make([]any, 0, len(w.snapshot)+len(items))
	for _, item := range w.snapshot {
		keys, err := w.indexer.ExtractKeys(item)
		if err != nil || indexer.EncodeKey(keys) != composite {
			remaining = append(remaining, item)
		}
	}
	w.snapshot = remaining
	delete(w.snapshotByKey, composite)
	w.addToSnapshotLocked(items)
}

func (w *StoreWrapper) replaceSnapshotPrefixLocked(encodedPrefix string, items []any) {
	remaining := make([]any, 0, len(w.snapshot)+len(items))
	for _, item := range w.snapshot {
		keys, err := w.indexer.ExtractKeys(item)
		if err != nil || !indexer.HasEncodedKeyPrefix(indexer.EncodeKey(keys), encodedPrefix) {
			remaining = append(remaining, item)
		}
	}
	w.snapshot = remaining
	w.snapshotByKey = map[string][]any{}
	for _, item := range w.snapshot {
		w.indexItemLocked(item)
	}
	w.addToSnapshotLocked(items)
}

func (w *StoreWrapper) replaceCompleteSnapshotLocked(items []any) {
	w.snapshot = nil
	if w.snapshotByKey != nil {
		w.snapshotByKey = map[string][]any{}
	}
	w.addToSnapshotLocked(items)
	w.listed = true
}

func (w *StoreWrapper) getPrefix(stringKeys []string, op string) []any {
	var results []any
	encodedPrefix := indexer.EncodeKey(stringKeys)
	for key, items := range w.snapshotByKey {
		if indexer.HasEncodedKeyPrefix(key, encodedPrefix) {
			results = append(results, items...)
		}
	}
	slices.SortStableFunc(results, func(left, right any) int {
		leftIdentity, leftErr := derivedResourceIdentity("", left)
		rightIdentity, rightErr := derivedResourceIdentity("", right)
		if leftErr != nil || rightErr != nil {
			return 0
		}
		if byNamespace := cmp.Compare(leftIdentity.Namespace, rightIdentity.Namespace); byNamespace != 0 {
			return byNamespace
		}
		return cmp.Compare(leftIdentity.Name, rightIdentity.Name)
	})
	w.Logger.Log(context.Background(), logging.LevelTrace, "store get called (snapshot, prefix)",
		"resource_type", w.ResourceType,
		"op", op,
		"keys", stringKeys,
		"found_count", len(results))
	return results
}

// indexItemLocked extracts index keys from item and appends it to the
// snapshot index. Caller must hold cacheMu. No-op when the indexer
// isn't available (no IndexBy or indexer build failed).
func (w *StoreWrapper) indexItemLocked(item any) {
	if w.indexer == nil || w.snapshotByKey == nil {
		return
	}
	keys, err := w.indexer.ExtractKeys(item)
	if err != nil {
		w.recordReadFailure(fmt.Errorf("resource %q could not index a snapshot item: %w", w.ResourceType, err))
		// Item appears but isn't reachable via keyed lookup.
		// Mirrors what would happen if the underlying store had
		// also failed to extract keys.
		w.Logger.Warn("Failed to extract snapshot index keys for item",
			"resource_type", w.ResourceType, "error", err)
		return
	}
	composite := indexer.EncodeKey(keys)
	w.snapshotByKey[composite] = append(w.snapshotByKey[composite], item)
}

// addToSnapshotLocked appends items fetched on-demand into the
// snapshot and updates the keyed index. Used by the lazy-mode
// per-key fetch path. Caller must hold cacheMu.
func (w *StoreWrapper) addToSnapshotLocked(items []any) {
	for _, item := range items {
		w.snapshot = append(w.snapshot, item)
		w.indexItemLocked(item)
	}
}

// List returns all resources from the per-render snapshot.
//
// A lazy List resolves the complete pinned store once.
func (w *StoreWrapper) List() (result []any) {
	ctx, release, err := w.beginStoreInvocation(w.storeContext())
	if err != nil {
		w.recordReadFailure(fmt.Errorf("resource %q List failed: %w", w.ResourceType, err))
		return nil
	}
	defer release()
	result, err = w.listInInvocation(ctx)
	if err != nil {
		w.recordReadFailure(fmt.Errorf("resource %q List failed: %w", w.ResourceType, err))
		return nil
	}
	if err := templating.RegisterIncrementalImmutableInputs(ctx, result); err != nil {
		w.recordReadFailure(fmt.Errorf("resource %q List failed: %w", w.ResourceType, err))
		return nil
	}
	return result
}

func (w *StoreWrapper) listInInvocation(ctx context.Context) ([]any, error) {
	if view, ok := w.SnapshotView.(contextualStoreSnapshotView); ok && w.usesSnapshotView() {
		items, err := view.ListContext(ctx, w.ResourceType, w.Store)
		if err != nil {
			return nil, err
		}
		return w.project(w.cloneStoreItems(items, "List"), "List"), nil
	}
	if w.MemoizeSnapshotView && w.usesSnapshotView() {
		w.cacheMu.Lock()
		defer w.cacheMu.Unlock()
		w.loadSnapshot()
		if w.LazySnapshot && !w.listed {
			items, err := w.SnapshotView.List(w.ResourceType, w.Store)
			if err != nil {
				return nil, err
			}
			w.replaceCompleteSnapshotLocked(w.project(w.cloneStoreItems(items, "List"), "List"))
		}
		return w.snapshot, nil
	}
	if w.usesSnapshotView() {
		items, err := w.SnapshotView.List(w.ResourceType, w.Store)
		if err != nil {
			return nil, err
		}
		return w.project(w.cloneStoreItems(items, "List"), "List"), nil
	}
	w.cacheMu.Lock()
	defer w.cacheMu.Unlock()
	w.loadSnapshot()
	if w.LazySnapshot && !w.listed {
		items, err := w.listStore()
		if err != nil {
			w.warnReadFailure("Failed to list resources for snapshot",
				"resource_type", w.ResourceType, "error", err)
			return nil, err
		}
		w.replaceCompleteSnapshotLocked(w.cloneStoreItems(items, "List"))
	}

	w.Logger.Log(context.Background(), logging.LevelTrace, "store list called",
		"resource_type", w.ResourceType,
		"count", len(w.snapshot))

	return w.project(w.snapshot, "List"), nil
}

// get is the shared snapshot-keyed lookup used by Fetch and GetSingle.
//
// When the snapshot was indexed (IndexBy was set and the indexer built
// cleanly), exact-match (full key set) hits the in-memory map and
// partial-match (prefix) does a small scan — same shape as
// MemoryStore.Get.
//
// In LazySnapshot mode, each exact or prefix scope resolves once from the
// pinned store; List resolves every item.
//
// When the snapshot wasn't indexed (no IndexBy), falls back to
// Store.Get(stringKeys...) — that path can disagree with the snapshot
// returned by List(), but the wrapper logged a warning when the
// snapshot was loaded so operators can see why.
func (w *StoreWrapper) get(stringKeys []string, op string) []any {
	if len(w.IndexBy) > 0 && (len(stringKeys) == 0 || len(stringKeys) > len(w.IndexBy)) {
		w.recordReadFailure(fmt.Errorf(
			"resource %q %s lookup has %d keys; pass between 1 and %d",
			w.ResourceType, op, len(stringKeys), len(w.IndexBy)))
		w.Logger.Error("Store lookup has an invalid key count and returns no resources; pass one to the configured number of index keys",
			"resource_type", w.ResourceType,
			"op", op,
			"key_count", len(stringKeys),
			"index_key_count", len(w.IndexBy))
		return []any{}
	}
	if w.MemoizeSnapshotView && w.usesSnapshotView() {
		return w.getMemoizedSnapshotView(stringKeys, op)
	}
	if w.usesSnapshotView() {
		items, err := w.SnapshotView.Get(w.ResourceType, w.Store, stringKeys...)
		if err != nil {
			w.recordReadFailure(fmt.Errorf("resource %q %s failed: %w", w.ResourceType, op, err))
			return nil
		}
		return w.project(w.cloneStoreItems(items, op), op)
	}

	w.cacheMu.Lock()
	defer w.cacheMu.Unlock()
	w.loadSnapshot()

	if w.snapshotByKey == nil {
		return w.project(w.getWithoutSnapshotIndex(stringKeys, op), op)
	}

	if len(stringKeys) == len(w.IndexBy) {
		return w.project(w.getExact(stringKeys, op), op)
	}
	encodedPrefix := indexer.EncodeKey(stringKeys)
	if w.LazySnapshot && !w.listed {
		if _, resolved := w.resolvedKeys[encodedPrefix]; !resolved {
			items, err := w.getStore(stringKeys...)
			if err != nil {
				w.recordReadFailure(fmt.Errorf("resource %q %s lookup %q failed: %w", w.ResourceType, op, stringKeys, err))
				return nil
			}
			w.replaceSnapshotPrefixLocked(encodedPrefix, w.cloneStoreItems(items, op))
			w.resolvedKeys[encodedPrefix] = struct{}{}
		}
	}
	return w.project(w.getPrefix(stringKeys, op), op)
}

func (w *StoreWrapper) getMemoizedSnapshotView(stringKeys []string, op string) []any {
	w.cacheMu.Lock()
	defer w.cacheMu.Unlock()
	w.loadSnapshot()
	if w.snapshotByKey == nil {
		return w.getMemoizedUnindexedSnapshotView(stringKeys, op)
	}
	if len(stringKeys) == len(w.IndexBy) {
		return w.getMemoizedExactSnapshotView(stringKeys, op)
	}
	return w.getMemoizedPrefixSnapshotView(stringKeys, op)
}

func (w *StoreWrapper) getMemoizedExactSnapshotView(stringKeys []string, op string) []any {
	composite := indexer.EncodeKey(stringKeys)
	if !w.LazySnapshot || w.listed {
		return w.snapshotByKey[composite]
	}
	if _, resolved := w.resolvedKeys[composite]; resolved {
		return w.snapshotByKey[composite]
	}
	items := w.readSnapshotView(stringKeys, op)
	w.replaceSnapshotBucketLocked(composite, items)
	w.resolvedKeys[composite] = struct{}{}
	if _, indexed := w.snapshotByKey[composite]; !indexed {
		w.snapshotByKey[composite] = []any{}
	}
	return w.snapshotByKey[composite]
}

func (w *StoreWrapper) getMemoizedPrefixSnapshotView(stringKeys []string, op string) []any {
	encodedPrefix := indexer.EncodeKey(stringKeys)
	if !w.LazySnapshot || w.listed {
		return w.getPrefix(stringKeys, op)
	}
	if _, resolved := w.resolvedKeys[encodedPrefix]; resolved {
		return w.getPrefix(stringKeys, op)
	}
	items := w.readSnapshotView(stringKeys, op)
	w.replaceSnapshotPrefixLocked(encodedPrefix, items)
	w.resolvedKeys[encodedPrefix] = struct{}{}
	return w.getPrefix(stringKeys, op)
}

func (w *StoreWrapper) getMemoizedUnindexedSnapshotView(stringKeys []string, op string) []any {
	if w.snapshotViewGets == nil {
		w.snapshotViewGets = map[string][]any{}
	}
	key := indexer.EncodeKey(stringKeys)
	if items, exists := w.snapshotViewGets[key]; exists {
		return items
	}
	items := w.readSnapshotView(stringKeys, op)
	w.snapshotViewGets[key] = items
	return items
}

func (w *StoreWrapper) readSnapshotView(stringKeys []string, op string) []any {
	items, err := w.SnapshotView.Get(w.ResourceType, w.Store, stringKeys...)
	if err != nil {
		w.recordReadFailure(fmt.Errorf("resource %q %s failed: %w", w.ResourceType, op, err))
		return nil
	}
	return w.project(w.cloneStoreItems(items, op), op)
}

// Fetch performs O(1) indexed lookup over the per-render snapshot using
// the provided keys.
//
// This method enables efficient lookups in templates and supports
// non-unique index keys by returning all resources matching the provided
// keys:
//
//	{% for endpoint_slice in resources.endpoints.Fetch(service_name) %}
//	  {{ endpoint_slice.metadata.name }}
//	{% endfor %}
//
// The keys must match the index configuration for the resource type. For
// example, if EndpointSlices are indexed by service name:
//
//	index_by: ["metadata.labels['kubernetes.io/service-name']"]
//
// Then you can look them up with:
//
//	resources.endpoints.Fetch("my-service")
//
// This will return ALL EndpointSlices for that service (typically
// multiple).
//
// Accepts any arguments for template compatibility.
//
// A failed read returns an empty slice to the template and fails the render at
// its next phase boundary.
func (w *StoreWrapper) Fetch(keys ...any) (result []any) {
	ctx, release, err := w.beginStoreInvocation(w.storeContext())
	if err != nil {
		w.recordReadFailure(fmt.Errorf("resource %q Fetch failed: %w", w.ResourceType, err))
		return nil
	}
	defer release()
	result, err = w.fetchInInvocation(ctx, keys)
	if err != nil {
		w.recordReadFailure(fmt.Errorf("resource %q Fetch failed: %w", w.ResourceType, err))
		return nil
	}
	if err := templating.RegisterIncrementalImmutableInputs(ctx, result); err != nil {
		w.recordReadFailure(fmt.Errorf("resource %q Fetch failed: %w", w.ResourceType, err))
		return nil
	}
	return result
}

func (w *StoreWrapper) fetchInInvocation(ctx context.Context, keys []any) ([]any, error) {
	stringKeys, ok := w.lookupKeys(keys, "Fetch")
	if !ok {
		return []any{}, nil
	}
	if view, ok := w.SnapshotView.(contextualStoreSnapshotView); ok && w.usesSnapshotView() {
		items, err := view.GetContext(ctx, w.ResourceType, w.Store, stringKeys...)
		if err != nil {
			return nil, err
		}
		return w.project(w.cloneStoreItems(items, "Fetch"), "Fetch"), nil
	}
	return w.get(stringKeys, "Fetch"), nil
}

func (w *StoreWrapper) lookupKeys(keys []any, operation string) ([]string, bool) {
	stringKeys, ok := w.normalizeLookupKeys(keys, operation)
	if !ok {
		return nil, false
	}
	return stringKeys, w.inNamespaceScope(stringKeys)
}

func (w *StoreWrapper) inNamespaceScope(keys []string) bool {
	if err := w.Scope.Check(keys); err != nil {
		w.recordReadFailure(err)
		return false
	}
	return true
}

func (w *StoreWrapper) normalizeLookupKeys(keys []any, operation string) ([]string, bool) {
	if w.usesSnapshotView() {
		if normalizer, ok := w.SnapshotView.(StoreLookupKeyNormalizer); ok {
			stringKeys, err := normalizer.NormalizeLookupKeys(w.ResourceType, keys)
			if err != nil {
				w.recordReadFailure(fmt.Errorf(
					"resource %q %s lookup keys were rejected: %w",
					w.ResourceType, operation, err,
				))
				return nil, false
			}
			if len(stringKeys) != len(keys) {
				w.recordReadFailure(fmt.Errorf(
					"resource %q %s lookup key normalizer returned %d keys for %d inputs",
					w.ResourceType, operation, len(stringKeys), len(keys),
				))
				return nil, false
			}
			return stringKeys, true
		}
	}
	stringKeys := make([]string, len(keys))
	for index := range keys {
		stringKeys[index] = toString(keys[index])
	}
	return stringKeys, true
}

func (w *StoreWrapper) lookupKeySource(
	keys StoreLookupKeySource,
	operation string,
) ([]string, bool) {
	stringKeys, ok := w.normalizeLookupKeySource(keys, operation)
	if !ok {
		return nil, false
	}
	return stringKeys, w.inNamespaceScope(stringKeys)
}

func (w *StoreWrapper) normalizeLookupKeySource(
	keys StoreLookupKeySource,
	operation string,
) ([]string, bool) {
	if w.usesSnapshotView() {
		if normalizer, ok := w.SnapshotView.(StoreLookupKeySourceNormalizer); ok {
			stringKeys, err := normalizer.NormalizeLookupKeySource(w.ResourceType, keys)
			if err != nil {
				w.recordReadFailure(fmt.Errorf(
					"resource %q %s lookup keys were rejected: %w",
					w.ResourceType, operation, err,
				))
				return nil, false
			}
			if len(stringKeys) != keys.Len() {
				w.recordReadFailure(fmt.Errorf(
					"resource %q %s lookup key normalizer returned %d keys for %d inputs",
					w.ResourceType, operation, len(stringKeys), keys.Len(),
				))
				return nil, false
			}
			return stringKeys, true
		}
	}
	values := make([]any, keys.Len())
	for index := range values {
		values[index] = keys.Value(index)
	}
	return w.normalizeLookupKeys(values, operation)
}

// GetSingle performs O(1) indexed lookup over the per-render snapshot
// and expects exactly one matching resource.
//
// This method is useful when you know the index keys uniquely identify
// a resource:
//
//	{% set ingress = resources.ingresses.GetSingle("default", "my-ingress") %}
//	{% if ingress %}
//	  {{ ingress.metadata.name }}
//	{% endif %}
//
//	{# Cross-namespace reference #}
//	{% set ref = "namespace/name".split("/") %}
//	{% set secret = resources.secrets.GetSingle(ref[0], ref[1]) %}
//
// Accepts any arguments for template compatibility.
//
// Returns:
//   - nil if no resources match (this is NOT an error - allows templates
//     to check existence)
//   - The single matching resource if exactly one matches
//   - nil if multiple resources match; the ambiguity then fails the render
//
// A failed read returns nil to the template and fails the render at its next
// phase boundary.
func (w *StoreWrapper) GetSingle(keys ...any) (result any) {
	ctx, release, err := w.beginStoreInvocation(w.storeContext())
	if err != nil {
		w.recordReadFailure(fmt.Errorf("resource %q GetSingle failed: %w", w.ResourceType, err))
		return nil
	}
	defer release()
	result, _, err = w.getSingleInInvocation(ctx, keys)
	if err != nil {
		w.recordReadFailure(fmt.Errorf("resource %q GetSingle failed: %w", w.ResourceType, err))
		return nil
	}
	if err := templating.RegisterIncrementalImmutableInputs(ctx, result); err != nil {
		w.recordReadFailure(fmt.Errorf("resource %q GetSingle failed: %w", w.ResourceType, err))
		return nil
	}
	return result
}

func (w *StoreWrapper) getSingleInInvocation(
	ctx context.Context,
	keys []any,
) (item any, found bool, err error) {
	stringKeys, ok := w.lookupKeys(keys, "GetSingle")
	if !ok {
		return nil, false, nil
	}

	var items []any
	if view, ok := w.SnapshotView.(contextualStoreSnapshotView); ok && w.usesSnapshotView() {
		resolved, err := view.GetContext(ctx, w.ResourceType, w.Store, stringKeys...)
		if err != nil {
			return nil, false, err
		}
		items = w.project(w.cloneStoreItems(resolved, "GetSingle"), "GetSingle")
	} else {
		items = w.get(stringKeys, "GetSingle")
	}

	if len(items) == 0 {
		return nil, false, nil
	}

	if len(items) > 1 {
		w.recordReadFailure(fmt.Errorf(
			"resource %q GetSingle lookup %q matched %d objects; use Fetch or configure unique indexBy values",
			w.ResourceType, stringKeys, len(items)))
		w.Logger.Error("GetSingle found multiple resources (ambiguous lookup)",
			"resource_type", w.ResourceType,
			"keys", stringKeys,
			"count", len(items))
		return nil, false, nil
	}

	return items[0], true, nil
}
