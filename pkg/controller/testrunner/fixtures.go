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

package testrunner

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/names"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/logging"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/indexer"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/store"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
)

// MergeFixtures deep merges global fixtures with test fixtures by resource identity.
//
// Resource identity is defined by: apiVersion + kind + namespace + name
//
// Merge strategy:
//   - Global fixtures are added first
//   - Test fixtures override global fixtures when same resource identity exists
//   - Resource types not in global fixtures are taken from test fixtures
//
// Parameters:
//   - globalFixtures: Fixtures from validationTests._global
//   - testFixtures: Fixtures from specific test
//
// Returns:
//   - Merged fixtures map (resource type → list of resources)
func MergeFixtures(globalFixtures, testFixtures map[string][]any) map[string][]any {
	// Build identity map for test fixtures to detect overrides
	testIdentities := buildFixtureIdentityMap(testFixtures)

	allResourceTypes := collectResourceTypes(globalFixtures, testFixtures)

	// Merge fixtures for each resource type
	merged := make(map[string][]any)
	for resourceType := range allResourceTypes {
		mergedResources := mergeResourceType(
			resourceType,
			globalFixtures[resourceType],
			testFixtures[resourceType],
			testIdentities,
		)

		if len(mergedResources) > 0 {
			merged[resourceType] = mergedResources
		}
	}

	return merged
}

// buildFixtureIdentityMap creates a map of resource identities for deduplication.
func buildFixtureIdentityMap(fixtures map[string][]any) map[string]any {
	identities := make(map[string]any)

	for resourceType, resources := range fixtures {
		for _, resourceObj := range resources {
			resourceMap, ok := resourceObj.(map[string]any)
			if !ok {
				continue
			}

			resource := &unstructured.Unstructured{Object: resourceMap}
			identity := buildResourceIdentity(resourceType, resource)
			identities[identity] = resourceObj
		}
	}

	return identities
}

// collectResourceTypes collects all unique resource types from multiple fixture maps.
func collectResourceTypes(fixtureMaps ...map[string][]any) map[string]bool {
	allTypes := make(map[string]bool)

	for _, fixtures := range fixtureMaps {
		for resourceType := range fixtures {
			allTypes[resourceType] = true
		}
	}

	return allTypes
}

// mergeResourceType merges global and test fixtures for a single resource type.
func mergeResourceType(
	resourceType string,
	globalResources []any,
	testResources []any,
	testIdentities map[string]any,
) []any {
	var merged []any

	// Add global fixtures that are NOT overridden by test fixtures
	for _, resourceObj := range globalResources {
		resourceMap, ok := resourceObj.(map[string]any)
		if !ok {
			continue
		}

		resource := &unstructured.Unstructured{Object: resourceMap}
		identity := buildResourceIdentity(resourceType, resource)

		// Only add if not overridden by test fixture
		if _, overridden := testIdentities[identity]; !overridden {
			merged = append(merged, resourceObj)
		}
	}

	// Add all test fixtures (these override global fixtures)
	merged = append(merged, testResources...)

	return merged
}

// buildResourceIdentity creates a unique identity key for a resource.
//
// Identity format: "resourceType|apiVersion|kind|namespace|name"
//
// This ensures resources are uniquely identified across different resource types
// and API versions.
func buildResourceIdentity(resourceType string, resource *unstructured.Unstructured) string {
	return fmt.Sprintf("%s|%s|%s|%s|%s",
		resourceType,
		resource.GetAPIVersion(),
		resource.GetKind(),
		resource.GetNamespace(),
		resource.GetName(),
	)
}

// CreateStoresFromFixtures creates resource stores from test fixtures.
//
// This converts the test fixtures (map of resource type → list of resources)
// into resource stores that can be used for template rendering.
//
// Implementation:
//   - Phase 1: Create empty stores for ALL watched resources
//   - Phase 2: Populate stores with fixture data where provided
//
// This ensures templates can safely call .List() on any watched resource type,
// even if that resource type is not present in test fixtures.
//
// Parameters:
//   - fixtures: Map of resource type names to lists of Kubernetes resources
//
// Returns:
//   - Map of resource type names to resource stores
//   - error if fixture processing fails
func (r *Runner) CreateStoresFromFixtures(fixtures map[string][]any) (map[string]stores.Store, error) {
	storeMap := r.createEmptyStores()

	for resourceType, resources := range fixtures {
		r.logger.Log(context.Background(), logging.LevelTrace, "Populating fixture store",
			"resource_type", resourceType,
			"count", len(resources))

		if resourceType == names.HAProxyPodsResourceType {
			if err := r.populateHAProxyPodsStore(storeMap[names.HAProxyPodsResourceType], resources); err != nil {
				return nil, err
			}
			continue
		}

		if err := r.populateWatchedResourceStore(storeMap, resourceType, resources); err != nil {
			return nil, err
		}
	}

	return storeMap, nil
}

// createEmptyStores creates empty stores for all watched resources and haproxy-pods.
func (r *Runner) createEmptyStores() map[string]stores.Store {
	storeMap := make(map[string]stores.Store)

	for resourceType := range r.config.WatchedResources {
		watchedResource := r.config.WatchedResources[resourceType]
		r.logger.Log(context.Background(), logging.LevelTrace, "Creating empty store for watched resource",
			"resource_type", resourceType)

		numKeys := max(len(watchedResource.IndexBy), 1)
		storeMap[resourceType] = store.NewMemoryStore(numKeys)
	}

	r.logger.Log(context.Background(), logging.LevelTrace, "Creating empty store for haproxy-pods (controller metadata)")
	storeMap[names.HAProxyPodsResourceType] = store.NewMemoryStore(2)

	return storeMap
}

// populateHAProxyPodsStore populates the haproxy-pods store with fixture data.
func (r *Runner) populateHAProxyPodsStore(storeInstance stores.Store, resources []any) error {
	for i, resourceObj := range resources {
		resourceMap, ok := resourceObj.(map[string]any)
		if !ok {
			return fmt.Errorf("haproxy-pods fixture at index %d is not a map", i)
		}

		// Deep-copy to avoid mutating shared fixture data
		resource := (&unstructured.Unstructured{Object: resourceMap}).DeepCopy()

		if resource.GetAPIVersion() == "" {
			resource.SetAPIVersion("v1")
		}
		if resource.GetKind() == "" {
			resource.SetKind("Pod")
		}

		r.logger.Log(context.Background(), logging.LevelTrace, "Adding haproxy-pods fixture to store",
			"index", i,
			"name", resource.GetName(),
			"namespace", resource.GetNamespace())

		keys := []string{resource.GetNamespace(), resource.GetName()}

		converted := indexer.ConvertResource(resource)
		if err := storeInstance.Add(converted, keys); err != nil {
			return fmt.Errorf("adding haproxy-pods fixture: %w", err)
		}
	}

	return nil
}

// populateWatchedResourceStore populates a watched resource store with fixture data.
func (r *Runner) populateWatchedResourceStore(storeMap map[string]stores.Store, resourceType string, resources []any) error {
	watchedResource, exists := r.config.WatchedResources[resourceType]
	if !exists {
		return fmt.Errorf("resource type %q in fixtures not found in watched resources", resourceType)
	}

	idx, err := indexer.New(indexer.Config{
		IndexBy:      watchedResource.IndexBy,
		IgnoreFields: r.config.WatchedResourcesIgnoreFields,
	})
	if err != nil {
		return fmt.Errorf("creating indexer for %s: %w", resourceType, err)
	}

	storeInstance := storeMap[resourceType]

	for i, resourceObj := range resources {
		resourceMap, ok := resourceObj.(map[string]any)
		if !ok {
			return fmt.Errorf("fixture resource at index %d in %s is not a map", i, resourceType)
		}

		// Deep-copy to avoid mutating shared fixture data
		resource := (&unstructured.Unstructured{Object: resourceMap}).DeepCopy()

		if resource.GetAPIVersion() == "" {
			resource.SetAPIVersion(watchedResource.APIVersion)
		}
		if resource.GetKind() == "" {
			kind := SingularizeResourceType(watchedResource.Resources)
			resource.SetKind(kind)
		}

		r.logger.Log(context.Background(), logging.LevelTrace, "Adding fixture resource to store",
			"resource_type", resourceType,
			"index", i,
			"name", resource.GetName(),
			"namespace", resource.GetNamespace())

		keys, err := idx.ExtractKeys(resource)
		if err != nil {
			return fmt.Errorf("extracting index keys from fixture resource: %w", err)
		}

		if r.namespaceScopes != nil && r.namespaceScopes(resourceType).Check(keys) != nil {
			continue
		}

		converted := indexer.ConvertResource(resource)
		if err := storeInstance.Add(converted, keys); err != nil {
			return fmt.Errorf("adding fixture resource to %s store: %w", resourceType, err)
		}
	}

	return nil
}
