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

package controller

import (
	"fmt"
	"log/slog"

	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/indextracker"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/names"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/resourcewatcher"
	coreconfig "github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/client"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/types"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/watcher"
)

const metadataNameIndex = "metadata.name"

// setupResourceWatchers creates and starts resource watchers and index tracker, then waits for sync.
//
// Returns the ResourceWatcherComponent and an error if watcher creation or synchronization fails.
func setupResourceWatchers(
	setup *componentSetup,
	cfg *coreconfig.Config,
	k8sClient *client.Client,
	logger *slog.Logger,
) (*resourcewatcher.ResourceWatcherComponent, error) {
	logger.Info("Stage 3: Starting resource watchers")

	// Extract resource type names for IndexSynchronizationTracker
	// Include haproxy-pods which is auto-injected by ResourceWatcherComponent
	resourceNames := make([]string, 0, len(cfg.WatchedResources)+1)
	for name := range cfg.WatchedResources {
		resourceNames = append(resourceNames, name)
	}
	// Add haproxy-pods (auto-injected)
	resourceNames = append(resourceNames, names.HAProxyPodsResourceType)

	// Create ResourceWatcherComponent
	resourceWatcher, err := resourcewatcher.New(cfg, k8sClient, setup.Bus, logger,
		resourcewatcher.WithSelfWriteFilter(setup.SelfWrites))
	if err != nil {
		return nil, fmt.Errorf("creating resource watcher: %w", err)
	}

	// Create IndexSynchronizationTracker
	indexTracker := indextracker.New(setup.Bus, logger, resourceNames)

	// Start resource watcher and index tracker (tracked by errgroup for graceful shutdown)
	startInErrGroup(setup.ErrGroup, setup.IterCtx, logger, setup.CancelCause, "resource watcher", resourceWatcher.Start)
	startInErrGroup(setup.ErrGroup, setup.IterCtx, logger, setup.CancelCause, "index tracker", indexTracker.Start)

	// Wait for all resource indices to sync
	logger.Debug("Waiting for resource indices to sync")
	if err := resourceWatcher.WaitForAllSync(setup.IterCtx); err != nil {
		return nil, fmt.Errorf("resource watcher sync failed: %w", err)
	}
	logger.Debug("All resource indices synced")

	return resourceWatcher, nil
}

// setupConfigWatchers creates and starts the HAProxyTemplateConfig CRD and
// credentials Secret watchers, then waits for sync.
//
// Returns an error if watcher creation or synchronization fails.
func setupConfigWatchers(
	setup *componentSetup,
	k8sClient *client.Client,
	crdName string,
	secretName string,
	crdGVR schema.GroupVersionResource,
	libraryGVR schema.GroupVersionResource,
	secretGVR schema.GroupVersionResource,
	logger *slog.Logger,
) error {
	logger.Info("Stage 4: Starting config watchers")

	crdWatcher, err := watcher.NewSingle(&types.SingleWatcherConfig{
		GVR:       crdGVR,
		Namespace: k8sClient.Namespace(),
		Name:      crdName,
		OnChange: func(obj any) error {
			setup.Bus.Publish(events.NewConfigResourceChangedEvent(obj))
			return nil
		},
		// OnSyncComplete delivers the current state after initial sync.
		// This ensures eventual consistency: if updates arrived during the sync window
		// (when OnChange callbacks are suppressed), the current state is delivered here.
		OnSyncComplete: func(obj any) error {
			if obj == nil {
				logger.Debug("CRD watcher sync complete, no resource in cache (skipping event)", "name", crdName)
				return nil
			}
			logger.Debug("CRD watcher sync complete, publishing current state", "name", crdName)
			setup.Bus.Publish(events.NewConfigResourceChangedEvent(obj))
			return nil
		},
	}, k8sClient)
	if err != nil {
		return fmt.Errorf("creating HAProxyTemplateConfig watcher for %q: %w", crdName, err)
	}
	crdWatchers := []*watcher.SingleWatcher{crdWatcher}

	publish := func(store types.Store) {
		libraries, err := store.List()
		if err != nil {
			// Publishing a partial set would unresolve references that are
			// actually fine, so skip and wait for the next notification.
			logger.Error("Listing HAProxyTemplateLibrary failed, skipping this update", "error", err)
			return
		}
		setup.Bus.Publish(events.NewLibrarySetChangedEvent(libraries))
	}
	libraryWatcher, err := watcher.New(
		libraryWatcherConfig(libraryGVR, k8sClient.Namespace(), publish), k8sClient, logger)
	if err != nil {
		return fmt.Errorf("creating HAProxyTemplateLibrary watcher: %w", err)
	}

	secretWatcher, secretErr := watcher.NewSingle(&types.SingleWatcherConfig{
		GVR:       secretGVR,
		Namespace: k8sClient.Namespace(),
		Name:      secretName,
		OnChange: func(obj any) error {
			setup.Bus.Publish(events.NewSecretResourceChangedEvent(obj))
			return nil
		},
		// OnSyncComplete delivers the current state after initial sync.
		// This ensures eventual consistency: if updates arrived during the sync window
		// (when OnChange callbacks are suppressed), the current state is delivered here.
		OnSyncComplete: func(obj any) error {
			if obj == nil {
				logger.Debug("Secret watcher sync complete, no resource in cache (skipping event)")
				return nil
			}
			logger.Debug("Secret watcher sync complete, publishing current state")
			setup.Bus.Publish(events.NewSecretResourceChangedEvent(obj))
			return nil
		},
	}, k8sClient)
	if secretErr != nil {
		return fmt.Errorf("creating Secret watcher: %w", secretErr)
	}

	// Start watchers (tracked by errgroup for graceful shutdown)
	for _, w := range crdWatchers {
		startInErrGroup(setup.ErrGroup, setup.IterCtx, logger, setup.CancelCause,
			fmt.Sprintf("HAProxyTemplateConfig watcher (%s)", crdName), w.Start)
	}
	startInErrGroup(setup.ErrGroup, setup.IterCtx, logger, setup.CancelCause,
		"HAProxyTemplateLibrary watcher", libraryWatcher.Start)
	startInErrGroup(setup.ErrGroup, setup.IterCtx, logger, setup.CancelCause, "Secret watcher", secretWatcher.Start)

	logger.Debug("Watchers started, waiting for initial sync")

	// Wait for watchers to complete initial sync in parallel
	watcherGroup, watcherCtx := errgroup.WithContext(setup.IterCtx)

	for _, w := range crdWatchers {
		watcherGroup.Go(func() error {
			if err := w.WaitForSync(watcherCtx); err != nil {
				return fmt.Errorf("HAProxyTemplateConfig watcher sync failed for %q: %w", crdName, err)
			}
			return nil
		})
	}

	watcherGroup.Go(func() error {
		if _, err := libraryWatcher.WaitForSync(watcherCtx); err != nil {
			return fmt.Errorf("HAProxyTemplateLibrary watcher sync failed: %w", err)
		}
		return nil
	})

	watcherGroup.Go(func() error {
		if err := secretWatcher.WaitForSync(watcherCtx); err != nil {
			return fmt.Errorf("secret watcher sync failed: %w", err)
		}
		return nil
	})

	// Wait for watchers to sync
	if err := watcherGroup.Wait(); err != nil {
		return fmt.Errorf("config watcher sync failed: %w", err)
	}

	logger.Debug("Config and secret watchers synced")

	// Initial config already passed via bootstrap event. Watchers handle subsequent changes only.

	return nil
}

// libraryWatcherConfig describes the HAProxyTemplateLibrary watch.
//
// Libraries are watched by KIND rather than by name: which ones matter is
// declared in the config's spec.libraryRefs, and that changes without any
// library object changing. Each notification carries the whole set, so a
// deletion needs no handling of its own — the object is simply absent from the
// next snapshot.
//
// Extracted so its validity is testable: watcher.New rejects a config with no
// IndexBy at RUNTIME, which surfaces only as a controller that never becomes
// ready.
func libraryWatcherConfig(
	gvr schema.GroupVersionResource,
	namespace string,
	publish func(types.Store),
) types.WatcherConfig {
	return types.WatcherConfig{
		GVR:        gvr,
		Namespaces: []string{namespace},
		// Name alone identifies a library within the namespace, and it is what
		// spec.libraryRefs names.
		IndexBy: []string{metadataNameIndex},
		OnChange: func(store types.Store, _ types.ChangeStats) {
			publish(store)
		},
		// Always fires, so a namespace with no libraries still delivers the
		// empty set the loader needs to distinguish "none exist" from "not
		// observed yet" — without it such a cluster would wait forever.
		OnSyncComplete: func(store types.Store, _ int) {
			publish(store)
		},
	}
}
