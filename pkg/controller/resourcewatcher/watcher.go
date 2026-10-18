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

// Package resourcewatcher provides the ResourceWatcherComponent that creates and manages
// watchers for all Kubernetes resources defined in the controller configuration.
//
// The component:
//   - Creates a k8s.Watcher for each resource type in Config.WatchedResources
//   - Merges global WatchedResourcesIgnoreFields with per-resource ignore fields
//   - Publishes ResourceIndexUpdatedEvent on resource changes
//   - Publishes ResourceSyncCompleteEvent when a resource type completes initial sync
//   - Provides access to stores for template rendering
package resourcewatcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"golang.org/x/sync/errgroup"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/names"
	coreconfig "github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/client"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/types"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/watcher"
)

const (
	resourcePods            = "pods"
	indexFieldMetaName      = "metadata.name"
	indexFieldMetaNamespace = "metadata.namespace"
)

// ResourceWatcherComponent creates and manages watchers for all configured resources.
type ResourceWatcherComponent struct {
	watchers  map[string]*watcher.Watcher // resourceTypeName -> watcher
	stores    map[string]types.Store      // resourceTypeName -> store
	eventBus  *busevents.EventBus
	k8sClient *client.Client
	logger    *slog.Logger
}

// Option configures New beyond its required arguments.
type Option func(*options)

type options struct {
	selfWrites types.SelfWriteFilter
}

// WithSelfWriteFilter hands every watcher the filter that recognises this
// controller's own writes, so an echoed status write refreshes the store
// without triggering a reconciliation.
func WithSelfWriteFilter(f types.SelfWriteFilter) Option {
	return func(o *options) { o.selfWrites = f }
}

// New creates a new ResourceWatcherComponent.
//
// For each entry in cfg.WatchedResources, this creates a k8s.Watcher that:
//   - Watches the specified Kubernetes resource type
//   - Indexes resources using the configured IndexBy expressions
//   - Filters fields by merging global WatchedResourcesIgnoreFields with per-resource ignore fields
//   - Publishes events to the EventBus on resource changes
//
// Returns an error if:
//   - Configuration validation fails
//   - Watcher creation fails for any resource type
func New(
	cfg *coreconfig.Config,
	k8sClient *client.Client,
	eventBus *busevents.EventBus,
	logger *slog.Logger,
	opts ...Option,
) (*ResourceWatcherComponent, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if cfg == nil {
		return nil, errors.New("config is nil")
	}
	if k8sClient == nil {
		return nil, errors.New("k8s client is nil")
	}
	if eventBus == nil {
		return nil, errors.New("event bus is nil")
	}
	if logger == nil {
		return nil, errors.New("logger is nil")
	}

	rwc := &ResourceWatcherComponent{
		watchers:  make(map[string]*watcher.Watcher),
		stores:    make(map[string]types.Store),
		eventBus:  eventBus,
		k8sClient: k8sClient,
		logger:    logger,
	}

	// Auto-inject HAProxy pods watcher based on PodSelector
	// This watcher is always created regardless of WatchedResources configuration
	resourcesWithHAProxyPods := make(map[string]coreconfig.WatchedResource)

	// Copy user-configured resources
	maps.Copy(resourcesWithHAProxyPods, cfg.WatchedResources)

	// Add haproxy-pods watcher (or override if user configured it)
	resourcesWithHAProxyPods[names.HAProxyPodsResourceType] = coreconfig.WatchedResource{
		APIVersion:    "v1",
		Resources:     resourcePods,
		LabelSelector: cfg.PodSelector.MatchLabels,
		IndexBy: []string{
			indexFieldMetaNamespace,
			indexFieldMetaName,
		},
	}

	logger.Debug("Auto-injected haproxy-pods watcher",
		"label_selector", cfg.PodSelector.MatchLabels)

	// Create a watcher for each resource type (including auto-injected haproxy-pods)
	for resourceTypeName := range resourcesWithHAProxyPods {
		watchedResource := resourcesWithHAProxyPods[resourceTypeName]
		// Convert APIVersion/Kind to GVR
		gvr, err := toGVR(&watchedResource)
		if err != nil {
			return nil, fmt.Errorf("invalid resource %q: %w", resourceTypeName, err)
		}

		ignoreFields := mergeIgnoreFields(cfg.WatchedResourcesIgnoreFields, watchedResource.IgnoreFields)

		// Convert label selector map to metav1.LabelSelector
		var labelSelector *metav1.LabelSelector
		if len(watchedResource.LabelSelector) > 0 {
			labelSelector = &metav1.LabelSelector{
				MatchLabels: watchedResource.LabelSelector,
			}
		}

		// Calculate cache TTL as slightly over 2x drift prevention interval
		// This allows one rendering cycle to fail while still keeping resources cached
		driftInterval := cfg.Dataplane.GetDriftPreventionInterval()
		cacheTTL := driftInterval * 22 / 10 // 2.2x drift interval

		// Create watcher configuration. DebounceInterval comes from the per-resource
		// override on the CRD; GetDebounceInterval returns 0 when the field is empty
		// or unparseable, and WatcherConfig.SetDefaults treats zero as "use the
		// pkg/k8s/types.DefaultDebounceInterval".
		watcherConfig := &types.WatcherConfig{
			GVR:              gvr,
			Namespaces:       determineNamespaces(resourceTypeName, &watchedResource, k8sClient),
			LabelSelector:    labelSelector,
			FieldSelector:    watchedResource.FieldSelector,
			IndexBy:          watchedResource.IndexBy,
			IgnoreFields:     ignoreFields,
			StoreType:        determineStoreType(watchedResource.Store),
			CacheTTL:         cacheTTL,
			DebounceInterval: watchedResource.GetDebounceInterval(),
			SelfWrites:       o.selfWrites,

			// OnChange publishes ResourceIndexUpdatedEvent
			OnChange: func(store types.Store, changeStats types.ChangeStats) {
				eventBus.Publish(events.NewResourceIndexUpdatedEvent(
					resourceTypeName,
					changeStats,
				))
			},

			// OnSyncComplete publishes ResourceSyncCompleteEvent
			OnSyncComplete: func(store types.Store, initialCount int) {
				eventBus.Publish(events.NewResourceSyncCompleteEvent(
					resourceTypeName,
					initialCount,
				))
			},

			// Don't call OnChange during initial sync (wait for full state)
			CallOnChangeDuringSync: false,
		}

		// Create watcher (dereference pointer to pass value)
		w, err := watcher.New(*watcherConfig, k8sClient, logger)
		if err != nil {
			return nil, fmt.Errorf("creating watcher for %q: %w", resourceTypeName, err)
		}

		rwc.watchers[resourceTypeName] = w
		rwc.stores[resourceTypeName] = w.Store()

		rwc.logger.Debug("Created resource watcher",
			"resource_type", resourceTypeName,
			"gvr", gvr.String(),
			"index_by", watchedResource.IndexBy,
			"namespaces", watcherConfig.Namespaces,
			"ignore_fields", len(ignoreFields),
			"field_selector", watchedResource.FieldSelector)
	}

	return rwc, nil
}

// Start begins watching all configured resources.
//
// This method:
//   - Starts all watchers in separate goroutines
//   - Continues running until ctx is cancelled
//   - Waits for every watcher to stop before returning
//
// Use WaitForAllSync() to wait for initial synchronization to complete.
func (r *ResourceWatcherComponent) Start(ctx context.Context) error {
	r.logger.Debug("Starting resource watchers", "count", len(r.watchers))

	watchers, watcherCtx := errgroup.WithContext(ctx)
	for resourceTypeName, w := range r.watchers {
		name := resourceTypeName
		resourceWatcher := w

		watchers.Go(func() error {
			r.logger.Debug("Starting watcher", "resource_type", name)

			if err := resourceWatcher.Start(watcherCtx); err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
					return nil
				}
				return fmt.Errorf("watcher %q failed: %w", name, err)
			}
			return nil
		})
	}
	watchers.Go(func() error {
		<-watcherCtx.Done()
		return nil
	})

	r.logger.Debug("All resource watchers started")
	if err := watchers.Wait(); err != nil {
		return err
	}
	r.logger.Info("Resource watchers stopped")
	return nil
}

// WaitForAllSync blocks until all watchers have completed initial synchronization.
//
// Returns:
//   - nil if all watchers synced successfully
//   - error if sync fails or context is cancelled
func (r *ResourceWatcherComponent) WaitForAllSync(ctx context.Context) error {
	r.logger.Debug("Waiting for all resource watchers to sync", "count", len(r.watchers))

	// Wait for all watchers to sync in parallel using errgroup
	g, gCtx := errgroup.WithContext(ctx)

	for resourceTypeName, w := range r.watchers {
		g.Go(func() error {
			r.logger.Debug("Waiting for watcher sync", "resource_type", resourceTypeName)

			if _, err := w.WaitForSync(gCtx); err != nil {
				return fmt.Errorf("watcher sync failed for %q: %w", resourceTypeName, err)
			}

			r.logger.Debug("Watcher synced", "resource_type", resourceTypeName)
			return nil
		})
	}

	// Wait for all watchers to complete
	if err := g.Wait(); err != nil {
		return err
	}

	r.logger.Debug("All resource watchers synced successfully")
	return nil
}

// GetStore returns the store for a specific resource type.
//
// Returns:
//   - The store if the resource type exists
//   - nil if the resource type is not watched
func (r *ResourceWatcherComponent) GetStore(resourceTypeName string) types.Store {
	return r.stores[resourceTypeName]
}

// GetAllStores returns a map of all stores keyed by resource type name.
//
// Returns a copy of the internal map to prevent external modification.
func (r *ResourceWatcherComponent) GetAllStores() map[string]types.Store {
	stores := make(map[string]types.Store, len(r.stores))
	maps.Copy(stores, r.stores)
	return stores
}

// determineStoreType returns the appropriate store type based on the configuration.
// Supported values:
//   - "on-demand": Uses CachedStore for memory-efficient storage with API-backed retrieval
//   - "full" or empty: Uses MemoryStore for fast in-memory storage (default)
func determineStoreType(storeConfig string) types.StoreType {
	if storeConfig == "on-demand" {
		return types.StoreTypeCached
	}
	return types.StoreTypeMemory // Default to full in-memory store
}

// determineNamespaces returns the namespaces a resource watcher covers; nil
// means all. HAProxy pods are pinned to the controller namespace; every other
// resource follows its configured namespaces.
func determineNamespaces(resourceTypeName string, resource *coreconfig.WatchedResource, k8sClient *client.Client) []string {
	if resourceTypeName == names.HAProxyPodsResourceType {
		return []string{k8sClient.Namespace()}
	}
	return resource.Namespaces
}

// toGVR converts a WatchedResource configuration to a GroupVersionResource.
func toGVR(wr *coreconfig.WatchedResource) (schema.GroupVersionResource, error) {
	if wr.APIVersion == "" {
		return schema.GroupVersionResource{}, errors.New("api_version is required")
	}
	if wr.Resources == "" {
		return schema.GroupVersionResource{}, errors.New("resources is required")
	}

	group, version := parseAPIVersion(wr.APIVersion)

	// Use the explicit plural resource name from configuration
	resource := wr.Resources

	return schema.GroupVersionResource{
		Group:    group,
		Version:  version,
		Resource: resource,
	}, nil
}

// parseAPIVersion splits an API version string into group and version components.
//
// Examples:
//   - "v1" → ("", "v1")  // Core resources
//   - "networking.k8s.io/v1" → ("networking.k8s.io", "v1")
func parseAPIVersion(apiVersion string) (group, version string) {
	parts := strings.SplitN(apiVersion, "/", 2)
	if len(parts) == 1 {
		// Core resources like "v1" have no group
		return "", parts[0]
	}
	return parts[0], parts[1]
}

// mergeIgnoreFields joins the global ignore list with one resource's own.
func mergeIgnoreFields(global, resource []string) []string {
	return dedupIgnoreFields(append(slices.Clone(global), resource...))
}

// dedupIgnoreFields returns a new slice containing the entries of fields with
// duplicates removed, preserving first-occurrence order.
func dedupIgnoreFields(fields []string) []string {
	seen := make(map[string]bool, len(fields))
	result := make([]string, 0, len(fields))
	for _, field := range fields {
		if !seen[field] {
			result = append(result, field)
			seen[field] = true
		}
	}
	return result
}
