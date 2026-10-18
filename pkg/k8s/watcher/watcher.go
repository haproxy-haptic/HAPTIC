// Package watcher provides Kubernetes resource watching with indexing,
// field filtering, and debounced callbacks.
//
// This package integrates all k8s subpackages to provide a high-level
// interface for watching Kubernetes resources and reacting to changes.
package watcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/client"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/indexer"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/store"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/types"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

// Watcher watches Kubernetes resources and maintains an indexed store.
//
// Resources are:
// - Filtered by namespace and label selector
// - Filtered by field selector (client-side JSONPath evaluation)
// - Indexed using JSONPath expressions for O(1) lookups
// - Filtered to remove unnecessary fields
// - Stored in memory or API-backed cache
//
// Changes are debounced and delivered via callback with aggregated statistics.
type Watcher struct {
	config               types.WatcherConfig
	client               *client.Client
	indexer              *indexer.Indexer
	fieldSelectorMatcher *indexer.FieldSelectorMatcher // nil if no field selector configured
	store                types.Store
	debouncer            *Debouncer
	informers            []namespaceInformer
	// Serializes event handlers: one informer per namespace delivers events
	// concurrently, while the handlers assume one caller at a time.
	eventMu           sync.Mutex
	stopCh            chan struct{}
	stopOnce          sync.Once // guards stopCh close so Stop() is idempotent
	startOnce         sync.Once
	startErr          error
	synced            bool // True after initial sync completes
	syncMu            sync.RWMutex
	initialCount      int          // Number of resources loaded during initial sync
	lastWatchErrNanos atomic.Int64 // observability: most recent watch-connection error
	labelSelector     string       // serialized form of config.LabelSelector; "" when unset
	logger            *slog.Logger
}

// New creates a new resource watcher with the provided configuration.
//
// Returns an error if:
//   - Configuration validation fails
//   - Client creation fails
//   - Indexer creation fails
//   - Store creation fails
//
// Example:
//
//	watcher, err := watcher.New(types.WatcherConfig{
//	    GVR: schema.GroupVersionResource{
//	        Group:    "networking.k8s.io",
//	        Version:  "v1",
//	        Resource: "ingresses",
//	    },
//	    IndexBy: []string{"metadata.namespace", "metadata.name"},
//	    IgnoreFields: []string{"metadata.managedFields"},
//	    StoreType: types.StoreTypeMemory,
//	    DebounceInterval: 500 * time.Millisecond,
//	    OnChange: func(store types.Store, stats types.ChangeStats) {
//	        slog.Info("Resources changed", "stats", stats)
//	    },
//	})
//

func New(cfg types.WatcherConfig, k8sClient *client.Client, logger *slog.Logger) (*Watcher, error) {
	cfg.SetDefaults()

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	if cfg.NamespacedWatch {
		cfg.Namespaces = []string{k8sClient.Namespace()}
	}

	// Serialize the selector once; Validate already proved it converts.
	var labelSelector string
	if cfg.LabelSelector != nil {
		selector, err := metav1.LabelSelectorAsSelector(cfg.LabelSelector)
		if err != nil {
			return nil, fmt.Errorf("converting label selector: %w", err)
		}
		labelSelector = selector.String()
	}

	// Create indexer
	idx, err := indexer.New(indexer.Config{
		IndexBy:      cfg.IndexBy,
		IgnoreFields: cfg.IgnoreFields,
	})
	if err != nil {
		return nil, fmt.Errorf("creating indexer: %w", err)
	}

	// Create field selector matcher if configured. The field is a concrete
	// *indexer.FieldSelectorMatcher, so a nil pointer compares equal to nil
	// in matchesFieldSelector's gate — no typed-nil interface pitfall.
	var fieldSelectorMatcher *indexer.FieldSelectorMatcher
	if cfg.FieldSelector != "" {
		fieldSelectorMatcher, err = indexer.NewFieldSelectorMatcher(cfg.FieldSelector)
		if err != nil {
			return nil, fmt.Errorf("creating field selector matcher: %w", err)
		}
	}

	// Create store based on type
	var resourceStore types.Store
	switch cfg.StoreType {
	case types.StoreTypeMemory:
		resourceStore = store.NewMemoryStore(len(cfg.IndexBy))

	case types.StoreTypeCached:
		dynamicClient := k8sClient.DynamicClient()
		if dynamicClient == nil {
			return nil, errors.New("cached store requires dynamic client")
		}

		cachedStore, err := store.NewCachedStore(&store.CachedStoreConfig{
			NumKeys:  len(cfg.IndexBy),
			CacheTTL: cfg.CacheTTL,
			Client:   dynamicClient,
			GVR:      cfg.GVR,
			Indexer:  idx,
			Logger:   logger,
			// On-demand kinds are read by live API GET, never from the
			// informer/store-cached body, so the informer copy can be
			// body-stripped to save memory (see createInformer's
			// SetTransform). Projected mode tells the store not to cache the
			// stripped body (ADR-0012).
			Projected: true,
		})
		if err != nil {
			return nil, fmt.Errorf("creating cached store: %w", err)
		}
		resourceStore = cachedStore

	default:
		return nil, fmt.Errorf("unsupported store type: %v", cfg.StoreType)
	}

	// Create debouncer
	// Suppress callbacks during sync if CallOnChangeDuringSync is false (default)
	suppressDuringSync := !cfg.CallOnChangeDuringSync
	debouncer := NewDebouncer(cfg.DebounceInterval, cfg.OnChange, resourceStore, suppressDuringSync)

	w := &Watcher{
		config:               cfg,
		client:               k8sClient,
		indexer:              idx,
		fieldSelectorMatcher: fieldSelectorMatcher,
		store:                resourceStore,
		debouncer:            debouncer,
		stopCh:               make(chan struct{}),
		synced:               false,
		initialCount:         0,
		labelSelector:        labelSelector,
		logger:               logger,
	}

	for _, namespace := range w.watchedNamespaces() {
		informer, err := w.createInformer(namespace)
		if err != nil {
			return nil, fmt.Errorf("creating informer for namespace %q: %w", namespace, err)
		}
		w.informers = append(w.informers, informer)
	}

	return w, nil
}

// namespaceInformer watches the resource in one namespace, or in all of them
// when namespace is metav1.NamespaceAll.
type namespaceInformer struct {
	namespace string
	factory   dynamicinformer.DynamicSharedInformerFactory
	informer  cache.SharedIndexInformer
	// Synced only after the handlers applied the initial list; informer.HasSynced is not.
	handlers cache.ResourceEventHandlerRegistration
}

func (w *Watcher) watchedNamespaces() []string {
	if len(w.config.Namespaces) == 0 {
		return []string{metav1.NamespaceAll}
	}
	return w.config.Namespaces
}

// createInformer creates a SharedIndexInformer for the watched resource in
// one namespace. Every informer feeds the same store and debouncer.
func (w *Watcher) createInformer(namespace string) (namespaceInformer, error) {
	dynamicClient := w.client.DynamicClient()
	if dynamicClient == nil {
		return namespaceInformer{}, errors.New("dynamic client is nil")
	}

	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		dynamicClient,
		0, // No resync
		namespace,
		w.applyListOptions,
	)
	informer := factory.ForResource(w.config.GVR).Informer()

	var transform cache.TransformFunc
	switch w.config.StoreType {
	case types.StoreTypeCached:
		transform = newProjectionTransform(projectionRoots(w.config.IndexBy, w.config.FieldSelector), w.indexer)
	case types.StoreTypeMemory:
		transform = newNormalizeTransform(w.indexer)
	}
	if err := informer.SetTransform(transform); err != nil {
		return namespaceInformer{}, fmt.Errorf("installing %s-store transform: %w", w.config.StoreType, err)
	}

	// Surface watch-connection errors instead of leaving them to client-go's
	// internal logging only. The Reflector retries with exponential backoff
	// on its own; without this handler a watch that starts failing mid-run —
	// e.g. because the watched API version stopped being served after an
	// in-place CRD upgrade — is completely silent while the informer keeps
	// serving its stale cache.
	if err := informer.SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
		w.handleWatchError(namespace, err)
	}); err != nil {
		return namespaceInformer{}, fmt.Errorf("setting watch error handler: %w", err)
	}

	handlers, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			w.eventMu.Lock()
			defer w.eventMu.Unlock()
			w.handleAdd(obj)
		},
		UpdateFunc: func(oldObj, newObj any) {
			w.eventMu.Lock()
			defer w.eventMu.Unlock()
			w.handleUpdate(oldObj, newObj)
		},
		DeleteFunc: func(obj any) {
			w.eventMu.Lock()
			defer w.eventMu.Unlock()
			w.handleDelete(obj)
		},
	})
	if err != nil {
		return namespaceInformer{}, fmt.Errorf("adding event handler: %w", err)
	}

	return namespaceInformer{namespace: namespace, factory: factory, informer: informer, handlers: handlers}, nil
}

// handleWatchError records and logs a dropped watch connection. The Reflector
// retries automatically with exponential backoff after this handler returns.
func (w *Watcher) handleWatchError(namespace string, err error) {
	w.lastWatchErrNanos.Store(time.Now().UnixNano())
	w.logger.Warn("Watcher watch error (Reflector will retry)",
		"gvr", w.config.GVR.String(),
		"namespace", namespace,
		"error", err)
}

// handlersSynced reports, per informer, whether its handlers applied the initial list.
func (w *Watcher) handlersSynced() []cache.InformerSynced {
	synced := make([]cache.InformerSynced, len(w.informers))
	for i := range w.informers {
		synced[i] = w.informers[i].handlers.HasSynced
	}
	return synced
}

// LastWatchError returns the time of the most recent watch-connection error,
// or the zero time if none has occurred. Observability only — retry is the
// Reflector's job.
func (w *Watcher) LastWatchError() time.Time {
	ns := w.lastWatchErrNanos.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// applyListOptions applies the label selector to list options.
//
// The selector was converted once in New, after Validate accepted it, so there
// is no error to handle per List/Watch. That matters: the previous version
// swallowed a conversion error and left the selector unset, turning a scoped
// watch into a cluster-wide one with no diagnostic.
func (w *Watcher) applyListOptions(options *metav1.ListOptions) {
	if w.labelSelector != "" {
		options.LabelSelector = w.labelSelector
	}
}

// Start begins watching resources.
//
// This method blocks until the context is cancelled or an error occurs.
// Initial sync is performed before continuing, and OnSyncComplete is called if configured.
func (w *Watcher) Start(ctx context.Context) error {
	w.startOnce.Do(func() {
		stopOnCancel := context.AfterFunc(ctx, func() {
			_ = w.Stop()
		})
		defer stopOnCancel()

		for i := range w.informers {
			w.informers[i].factory.Start(w.stopCh)
		}
		if !cache.WaitForCacheSync(w.stopCh, w.handlersSynced()...) {
			w.startErr = ctx.Err()
			if w.startErr == nil {
				w.startErr = errors.New("syncing cache")
			}
			return
		}
		select {
		case <-w.stopCh:
			w.startErr = ctx.Err()
			if w.startErr == nil {
				w.startErr = errors.New("syncing cache")
			}
			return
		default:
		}

		w.markSyncComplete()
		if w.config.OnSyncComplete != nil {
			w.config.OnSyncComplete(w.store, w.initialCount)
		}
	})
	if w.startErr != nil {
		_ = w.Stop()
		return w.startErr
	}

	select {
	case <-ctx.Done():
	case <-w.stopCh:
	}

	return w.Stop()
}

// Stop stops watching resources.
//
// This method is idempotent and safe to call multiple times (mirrors
// SingleWatcher.Stop) — a double close of stopCh would otherwise panic.
func (w *Watcher) Stop() error {
	w.stopOnce.Do(func() {
		close(w.stopCh)
		w.debouncer.Stop()
		for i := range w.informers {
			w.informers[i].factory.Shutdown()
		}
	})

	return nil
}

// Store returns the underlying store for direct access.
func (w *Watcher) Store() types.Store {
	return w.store
}

// WaitForSync blocks until initial synchronization is complete.
//
// This is useful when you need to wait for the store to be fully populated
// before performing operations that depend on complete data.
//
// Returns:
//   - The number of resources loaded during initial sync
//   - An error if sync fails or context is cancelled
//
// Example:
//
//	watcher, _ := watcher.New(cfg, client)
//	go watcher.Start(ctx)
//
//	count, err := watcher.WaitForSync(ctx)
//	if err != nil {
//	    slog.Error("Watcher sync failed", "error", err)
//	    os.Exit(1)
//	}
//	slog.Info("Watcher synced", "resource_count", count)
func (w *Watcher) WaitForSync(ctx context.Context) (int, error) {
	if !cache.WaitForCacheSync(ctx.Done(), w.handlersSynced()...) {
		return 0, errors.New("syncing cache")
	}

	// Mark sync as complete (idempotent - safe if Start() already did this)
	count := w.markSyncComplete()

	return count, nil
}

// IsSynced returns true if initial synchronization has completed.
//
// This provides a non-blocking way to check if the store is fully populated.
func (w *Watcher) IsSynced() bool {
	w.syncMu.RLock()
	defer w.syncMu.RUnlock()

	return w.synced
}

// markSyncComplete transitions the watcher from syncing to synced state.
// It is safe to call multiple times - only the first call takes effect.
// Returns the number of resources loaded during initial sync.
func (w *Watcher) markSyncComplete() int {
	w.syncMu.Lock()
	defer w.syncMu.Unlock()

	if w.synced {
		return w.initialCount
	}

	w.synced = true
	w.initialCount = w.debouncer.GetInitialCount()
	w.debouncer.SetSyncMode(false)

	return w.initialCount
}

// ForceSync forces an immediate callback with current statistics.
func (w *Watcher) ForceSync() {
	w.debouncer.Flush()
}

// Ensure watch.Interface compatibility for informer.
var _ watch.Interface = (*watch.FakeWatcher)(nil)
