// Package types defines core interfaces and types for the k8s package.
//
// This package provides the foundational types used across all k8s subpackages,
// including:
// - Store interface for resource indexing
// - Watcher configuration structures
// - Callback types for change notifications
// - Statistics and status types
package types

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Default values for watcher configuration.
const (
	// DefaultDebounceInterval is the default leading-edge debounce window for
	// resource-watch callbacks. The first change in a quiet period fires the
	// callback immediately; any further change inside the window is coalesced
	// (the watcher fires once when the window closes if anything was
	// suppressed).
	//
	// Kept near zero because it applies to every watched kind, including an
	// operator's own CRDs, so it must not encode a guess about which kinds are
	// latency-critical. Bursts are already coalesced downstream — the
	// reconciler's coordinator drains queued triggers while a render is in
	// flight — and reload pacing lives further down still (the agent, plus the
	// deployer's minDeploymentInterval), so a longer refractory window buys
	// nothing but propagation latency. Override via `debounceInterval` on a
	// watched-resource entry to batch a specific kind harder; `"0"`
	// (DebounceImmediate) removes the window entirely.
	DefaultDebounceInterval = 100 * time.Millisecond

	// DebounceImmediate is the sentinel WatcherConfig.DebounceInterval value
	// for "no debounce — fire the callback on every change event." It exists
	// because zero already means "unset, apply DefaultDebounceInterval" on
	// every WatcherConfig caller (SetDefaults swaps 0 → default), so a third
	// state was needed to express the operator-facing
	// `debounceInterval: "0"` semantic on the CRD without breaking the
	// existing zero-is-unset convention for direct Go callers. Picked as -1
	// because time.Duration is an int64 and negative values have no other
	// meaning in the debouncer state machine.
	//
	// Resource-agnostic: the watcher consults this sentinel; it never
	// learns which Kubernetes Kind it's watching. The chart and the
	// operator decide which watched resources opt into immediate firing
	// (per-resource `debounceInterval: "0"` on the CRD).
	DebounceImmediate time.Duration = -1
)

const (
	fieldGVRResource   = "GVR.Resource"
	fieldIndexBy       = "IndexBy"
	fieldOnChange      = "OnChange"
	fieldLabelSelector = "LabelSelector"
)

// Store defines the interface for storing and retrieving indexed Kubernetes resources.
//
// Implementations must be thread-safe for concurrent access.
// Resources are indexed by one or more keys extracted using JSONPath expressions.
type Store interface {
	// Get retrieves all resources matching the provided index keys.
	// Keys are evaluated in order as specified in the index configuration.
	//
	// Returns:
	//   - A slice of matching resources
	//   - An error if the operation fails
	//
	// Example:
	//   // For index_by: ["metadata.namespace", "metadata.name"]
	//   resources, err := store.Get("default", "my-ingress")
	Get(keys ...string) ([]any, error)

	// List returns all resources in the store.
	//
	// Returns:
	//   - A slice of all stored resources
	//   - An error if the operation fails
	List() ([]any, error)

	// Add inserts a new resource into the store with the provided index keys.
	//
	// Parameters:
	//   - resource: The Kubernetes resource to store
	//   - keys: Index keys extracted from the resource
	//
	// Returns an error if the operation fails.
	Add(resource any, keys []string) error

	// Update modifies an existing resource in the store.
	// If the resource doesn't exist, it will be added.
	//
	// Parameters:
	//   - resource: The updated Kubernetes resource
	//   - keys: Index keys extracted from the resource
	//
	// Returns an error if the operation fails.
	Update(resource any, keys []string) error

	// Delete removes the single resource identified by namespace/name. Index
	// keys are configurable and need not be unique, so identity is authoritative
	// and siblings sharing the bucket remain stored. Keys retain the configured
	// index shape for validation and compatibility.
	//
	// Deleting a resource that is not present is a no-op returning nil.
	// An empty resource name is rejected.
	//
	// Parameters:
	//   - namespace: Namespace of the resource to delete ("" for cluster-scoped)
	//   - name: Name of the resource to delete
	//   - keys: Full index-key shape for this store
	//
	// Returns an error if the operation fails.
	Delete(namespace, name string, keys []string) error

	// Clear removes all resources from the store.
	Clear() error
}

// StoreType defines the type of store implementation to use.
type StoreType int

const (
	// StoreTypeMemory stores complete resources in memory.
	// This is the default and provides fastest access but higher memory usage.
	StoreTypeMemory StoreType = iota

	// StoreTypeCached stores only index keys in memory and fetches resources
	// from the Kubernetes API on access. Responses are cached with a TTL.
	// This reduces memory usage at the cost of API latency on cache misses.
	StoreTypeCached
)

// String returns the string representation of the store type.
func (s StoreType) String() string {
	switch s {
	case StoreTypeMemory:
		return "memory"
	case StoreTypeCached:
		return "cached"
	default:
		return "unknown"
	}
}

// ChangeStats tracks aggregated statistics about resource changes since the last callback.
type ChangeStats struct {
	// Created is the number of resources added to the store.
	Created int

	// Modified is the number of resources updated in the store.
	Modified int

	// Deleted is the number of resources removed from the store.
	Deleted int

	// IsInitialSync is true when this callback is fired during initial synchronization.
	// During initial sync, Created count includes pre-existing resources being bulk-loaded.
	// After sync completes, IsInitialSync is false for all subsequent real-time changes.
	IsInitialSync bool
}

// Total returns the total number of changes.
func (c ChangeStats) Total() int {
	return c.Created + c.Modified + c.Deleted
}

// IsEmpty returns true if no changes occurred.
func (c ChangeStats) IsEmpty() bool {
	return c.Total() == 0
}

// OnChangeCallback is invoked when resources in the store change.
//
// The callback receives:
//   - store: The updated Store instance
//   - stats: Aggregated statistics about what changed
//
// Callbacks are debounced according to the WatcherConfig.DebounceInterval setting.
type OnChangeCallback func(store Store, stats ChangeStats)

// OnSyncCompleteCallback is invoked once after initial synchronization completes.
//
// This provides a clear signal that the store is fully populated with pre-existing
// resources and the watcher is ready for real-time change tracking.
//
// The callback receives:
//   - store: The fully synchronized Store instance
//   - initialCount: The number of pre-existing resources loaded during initial sync
type OnSyncCompleteCallback func(store Store, initialCount int)

// OnResourceChangeCallback is invoked when a single watched resource changes.
//
// This callback is used by SingleWatcher for watching a specific named resource.
// The callback receives the updated resource directly, allowing immediate processing.
//
// Unlike OnChangeCallback (used for bulk watchers), this callback:
//   - Receives the actual resource object, not a Store
//   - Is invoked immediately without debouncing
//   - Returns an error if processing fails
//
// The callback receives:
//   - obj: The Kubernetes resource that changed (runtime.Object)
//
// Returns an error if resource processing fails.
type OnResourceChangeCallback func(obj any) error

// WatcherConfig configures a Kubernetes resource watcher.
type WatcherConfig struct {
	// GroupVersionResource identifies the Kubernetes resource type to watch.
	//
	// Example for Ingress:
	//   GVR: schema.GroupVersionResource{
	//       Group:    "networking.k8s.io",
	//       Version:  "v1",
	//       Resource: "ingresses",
	//   }
	GVR schema.GroupVersionResource

	// Namespaces restricts watching to these namespaces, one informer each.
	// Empty watches all namespaces.
	Namespaces []string

	// NamespacedWatch, when true, restricts watching to the controller's own
	// namespace (from the service account token). Overrides Namespaces.
	NamespacedWatch bool

	// LabelSelector filters resources by label selector.
	// Uses Kubernetes label selector syntax.
	//
	// Example (the field is a pointer, so the literal needs `&`):
	//   LabelSelector: &metav1.LabelSelector{
	//       MatchLabels: map[string]string{
	//           "app": "haproxy",
	//           "component": "loadbalancer",
	//       },
	//   }
	LabelSelector *metav1.LabelSelector

	// FieldSelector filters resources by field value using client-side JSONPath evaluation.
	// Unlike Kubernetes' native fieldSelector (which only supports limited fields like
	// metadata.name), this supports any JSONPath expression and evaluates client-side.
	//
	// Format: "field.path=value"
	// Example: "spec.ingressClassName=haproxy-internal"
	//
	// Resources that don't match the field selector are filtered out before being
	// added to the store. This is evaluated after LabelSelector filtering.
	FieldSelector string

	// IndexBy specifies JSONPath expressions for extracting index keys from resources.
	//
	// Resources are indexed by the values of these expressions in order.
	// For O(1) lookup, use expressions that uniquely identify resources.
	//
	// Examples:
	//   // Index by namespace and name (standard iteration)
	//   IndexBy: []string{"metadata.namespace", "metadata.name"}
	//
	//   // Index by service name from label (O(1) service-to-endpoints lookup)
	//   IndexBy: []string{"metadata.labels['kubernetes.io/service-name']"}
	IndexBy []string

	// IgnoreFields specifies JSONPath expressions for fields to remove from resources
	// before storing them.
	//
	// This reduces memory usage by removing unnecessary fields.
	//
	// Examples:
	//   IgnoreFields: []string{
	//       "metadata.managedFields",  // Remove managed fields (verbose)
	//       "metadata.annotations",    // Remove annotations if not needed
	//   }
	IgnoreFields []string

	// StoreType determines the storage implementation to use.
	// See StoreType constants for available options.
	//
	// Default: StoreTypeMemory
	StoreType StoreType

	// CacheTTL sets the cache duration for StoreTypeCached.
	// Ignored for other store types.
	//
	// Cache entries are invalidated on resource updates. The TTL is reset
	// on every Get() access, implementing LRU-like behavior based on access time.
	// This ensures frequently accessed resources remain cached even if the original
	// TTL would have expired.
	//
	// Default: 2.2x drift prevention interval (allows one rendering cycle to fail
	// while still keeping resources cached)
	CacheTTL time.Duration

	// DebounceInterval sets the minimum time between OnChange callback invocations.
	//
	// Rapid resource changes within this interval are batched into a single callback
	// with aggregated statistics.
	//
	// Default: DefaultDebounceInterval (100ms) — applied in WatcherConfig.SetDefaults
	// when DebounceInterval is zero. With leading-edge triggering, the first
	// change in a quiet period fires immediately; only subsequent changes
	// within the window are batched. Override only when a specific resource
	// needs slower batching (e.g. high-volume EndpointSlice churn on large
	// clusters where 1s renders are too frequent) or no window at all.
	DebounceInterval time.Duration

	// OnChange is called when resources in the store change.
	// This callback is debounced according to DebounceInterval.
	//
	// The callback receives the updated Store and aggregated ChangeStats.
	// The ChangeStats.IsInitialSync field indicates if changes are from initial sync or real-time.
	OnChange OnChangeCallback

	// OnSyncComplete is called once after initial synchronization completes.
	// This provides a clear signal that the store is fully populated with pre-existing resources.
	//
	// The callback receives the store and the count of resources loaded during initial sync.
	// This is called after the informer's HasSynced() returns true.
	//
	// Optional: If not provided, no sync complete notification is sent.
	OnSyncComplete OnSyncCompleteCallback

	// SelfWrites, when set, identifies watch events that echo this controller's
	// own writes by resourceVersion. Echoed events still refresh the store; an
	// exact unchanged identity revision suppresses OnChange.
	//
	// Optional: nil treats every event as a change.
	SelfWrites SelfWriteFilter

	// CallOnChangeDuringSync determines if OnChange is called during initial synchronization.
	//
	// If false (default), OnChange is suppressed until sync completes, and only OnSyncComplete
	// is called with the final state. This avoids overwhelming the callback with bulk load events.
	//
	// If true, OnChange is called for every change during initial sync with IsInitialSync=true,
	// allowing incremental processing of pre-existing resources.
	//
	// Default: false
	CallOnChangeDuringSync bool

	// Context is used for cancellation of the watcher.
	// If nil, context.Background() is used.
	Context context.Context
}

// SetDefaults applies default values to unset configuration fields.
func (c *WatcherConfig) SetDefaults() {
	if c.CacheTTL == 0 {
		// Default TTL: 2.2x the default drift prevention interval (60s)
		// This results in ~132s, allowing one rendering cycle to fail
		// while still keeping resources cached
		c.CacheTTL = 2*time.Minute + 10*time.Second
	}
	if c.DebounceInterval == 0 {
		c.DebounceInterval = DefaultDebounceInterval
	}
	// Negative durations are the DebounceImmediate sentinel (or any other
	// negative, which we treat as equivalent). Normalise to 0 here so the
	// Debouncer sees a single representation of "fire immediately on every
	// event" and its existing leading-edge code handles it without a
	// separate branch (timeSinceLastFire >= 0 always holds, the
	// time.AfterFunc path is never taken).
	if c.DebounceInterval < 0 {
		c.DebounceInterval = 0
	}
	if c.Context == nil {
		c.Context = context.Background()
	}
}

// Validate checks if the configuration is valid.
// Returns an error if any required field is missing or invalid.
func (c *WatcherConfig) Validate() error {
	if c.GVR.Resource == "" {
		return &ConfigError{Field: fieldGVRResource, Message: "resource is required"}
	}
	if len(c.IndexBy) == 0 {
		return &ConfigError{Field: fieldIndexBy, Message: "at least one index key is required"}
	}
	if c.OnChange == nil {
		return &ConfigError{Field: fieldOnChange, Message: "callback is required"}
	}
	// A selector that cannot be converted would otherwise be dropped at list
	// time, silently widening the watch to every object of this kind. The nil
	// check is required: LabelSelectorAsSelector(nil) returns a selector
	// matching NOTHING without erroring, so nil must keep meaning "unfiltered".
	if c.LabelSelector != nil {
		if _, err := metav1.LabelSelectorAsSelector(c.LabelSelector); err != nil {
			return &ConfigError{Field: fieldLabelSelector, Message: err.Error()}
		}
	}
	return nil
}

// ConfigError represents a configuration validation error.
type ConfigError struct {
	Field   string
	Message string
}

func (e *ConfigError) Error() string {
	return "config error in " + e.Field + ": " + e.Message
}

// SingleWatcherConfig configures a watcher for a single named Kubernetes resource.
//
// Unlike WatcherConfig (which watches collections of resources), SingleWatcherConfig
// watches one specific resource identified by namespace and name.
//
// This is ideal for watching:
//   - Configuration stored in a specific ConfigMap
//   - Credentials stored in a specific Secret
//   - Any other single resource that the controller depends on
type SingleWatcherConfig struct {
	// GroupVersionResource identifies the Kubernetes resource type to watch.
	//
	// Example for ConfigMap:
	//   GVR: schema.GroupVersionResource{
	//       Group:    "",
	//       Version:  "v1",
	//       Resource: "configmaps",
	//   }
	GVR schema.GroupVersionResource

	// Namespace is the namespace containing the resource.
	// This is required for SingleWatcher (unlike bulk watchers which can watch all namespaces).
	//
	// Example:
	//   Namespace: "kube-system"
	Namespace string

	// Name is the name of the specific resource to watch.
	// This is required and identifies the single resource to monitor.
	//
	// Example:
	//   Name: "haproxy-config"
	Name string

	// OnChange is called when the watched resource changes (add, update, delete).
	// This callback is invoked immediately without debouncing.
	//
	// The callback receives the resource object directly and returns an error if processing fails.
	OnChange OnResourceChangeCallback

	// OnSyncComplete is called once after initial synchronization completes.
	// It receives the current resource from the informer cache.
	//
	// This ensures the controller has the latest state even if updates
	// arrived during the sync window (when OnChange callbacks are suppressed).
	//
	// This is optional. If nil, no callback is made after sync.
	OnSyncComplete OnResourceChangeCallback

	// Context is used for cancellation of the watcher.
	// If nil, context.Background() is used.
	Context context.Context
}

// SetDefaults applies default values to unset configuration fields.
func (c *SingleWatcherConfig) SetDefaults() {
	if c.Context == nil {
		c.Context = context.Background()
	}
}

// Validate checks if the configuration is valid.
// Returns an error if any required field is missing or invalid.
func (c *SingleWatcherConfig) Validate() error {
	if c.GVR.Resource == "" {
		return &ConfigError{Field: fieldGVRResource, Message: "resource is required"}
	}
	if c.Namespace == "" {
		return &ConfigError{Field: "Namespace", Message: "namespace is required"}
	}
	if c.Name == "" {
		return &ConfigError{Field: "Name", Message: "resource name is required"}
	}
	if c.OnChange == nil {
		return &ConfigError{Field: fieldOnChange, Message: "callback is required"}
	}
	return nil
}
