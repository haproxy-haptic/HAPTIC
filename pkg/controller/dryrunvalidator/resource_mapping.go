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

package dryrunvalidator

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/indexer"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
)

const (
	operationCreate = "CREATE"
	operationUpdate = "UPDATE"
	operationDelete = "DELETE"

	phaseRender = "render"
)

type resourceAlias struct {
	name          string
	namespaces    []string // empty: every namespace
	labelSelector map[string]string
	fieldSelector *indexer.FieldSelectorMatcher
}

func buildResourceAliases(resources map[string]config.WatchedResource) (map[schema.GroupVersionResource][]resourceAlias, error) {
	aliases := make(map[schema.GroupVersionResource][]resourceAlias)
	names := make([]string, 0, len(resources))
	for name := range resources {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		resource := resources[name]
		gv, err := schema.ParseGroupVersion(resource.APIVersion)
		if err != nil {
			return nil, fmt.Errorf("parsing apiVersion for watched resource %q: %w", name, err)
		}
		var fieldSelector *indexer.FieldSelectorMatcher
		if resource.FieldSelector != "" {
			fieldSelector, err = indexer.NewFieldSelectorMatcher(resource.FieldSelector)
			if err != nil {
				return nil, fmt.Errorf("parsing fieldSelector for watched resource %q: %w", name, err)
			}
		}
		key := schema.GroupVersionResource{Group: gv.Group, Version: gv.Version, Resource: resource.Resources}
		aliases[key] = append(aliases[key], resourceAlias{
			name:          name,
			namespaces:    resource.Namespaces,
			labelSelector: resource.LabelSelector,
			fieldSelector: fieldSelector,
		})
	}
	return aliases, nil
}

func (a resourceAlias) matches(object *unstructured.Unstructured) (bool, error) {
	if object == nil || !a.watchesNamespace(object.GetNamespace()) {
		return false, nil
	}
	labels := object.GetLabels()
	for key, value := range a.labelSelector {
		if labels[key] != value {
			return false, nil
		}
	}
	if a.fieldSelector == nil {
		return true, nil
	}
	return a.fieldSelector.Matches(object.Object)
}

// watchesNamespace reports whether the store can hold objects of the
// namespace; an object it can never hold is not an input of the render.
func (a resourceAlias) watchesNamespace(namespace string) bool {
	return len(a.namespaces) == 0 || slices.Contains(a.namespaces, namespace)
}

func (a resourceAlias) filtered() bool {
	return len(a.labelSelector) > 0 || a.fieldSelector != nil
}

func (c *Component) createOverlays(aliases []resourceAlias, namespace, name string, object, oldObject any, operation string) (overlays map[string]*stores.StoreOverlay, subjectAliases []string, err error) {
	var newResource *unstructured.Unstructured
	if object != nil {
		newResource, err = asUnstructured(object)
		if err != nil {
			return nil, nil, err
		}
	}
	var oldResource *unstructured.Unstructured
	if oldObject != nil {
		oldResource, err = asUnstructured(oldObject)
		if err != nil {
			return nil, nil, err
		}
	}

	overlays = make(map[string]*stores.StoreOverlay, len(aliases))
	subjectAliases = make([]string, 0, len(aliases))
	for _, alias := range aliases {
		overlay, err := createAliasOverlay(alias, namespace, name, newResource, oldResource, operation)
		if err != nil {
			return nil, nil, err
		}
		overlays[alias.name] = overlay
		if !overlay.IsEmpty() {
			subjectAliases = append(subjectAliases, alias.name)
		}
	}
	return overlays, subjectAliases, nil
}

func createAliasOverlay(alias resourceAlias, namespace, name string, newResource, oldResource *unstructured.Unstructured, operation string) (*stores.StoreOverlay, error) {
	switch operation {
	case operationCreate:
		return createAliasOverlayForCreate(alias, newResource)
	case operationUpdate:
		return createAliasOverlayForUpdate(alias, namespace, name, newResource, oldResource)
	case operationDelete:
		return createAliasOverlayForDelete(alias, namespace, name, oldResource)
	default:
		return nil, fmt.Errorf("unsupported admission operation %q", operation)
	}
}

func createAliasOverlayForCreate(alias resourceAlias, newResource *unstructured.Unstructured) (*stores.StoreOverlay, error) {
	if newResource == nil {
		return nil, fmt.Errorf("CREATE has no new object")
	}
	matches, err := alias.matches(newResource)
	if err != nil {
		return nil, fmt.Errorf("matching selector on store %q: %w", alias.name, err)
	}
	if !matches {
		return stores.NewStoreOverlay(), nil
	}
	return stores.NewStoreOverlayForCreate(newResource), nil
}

func createAliasOverlayForUpdate(alias resourceAlias, namespace, name string, newResource, oldResource *unstructured.Unstructured) (*stores.StoreOverlay, error) {
	if newResource == nil {
		return nil, fmt.Errorf("UPDATE has no new object")
	}
	if oldResource == nil && alias.filtered() {
		return nil, fmt.Errorf("UPDATE has no old object required by selector on store %q", alias.name)
	}
	oldMatches := oldResource == nil
	if oldResource != nil {
		var err error
		oldMatches, err = alias.matches(oldResource)
		if err != nil {
			return nil, fmt.Errorf("matching old object selector on store %q: %w", alias.name, err)
		}
	}
	newMatches, err := alias.matches(newResource)
	if err != nil {
		return nil, fmt.Errorf("matching new object selector on store %q: %w", alias.name, err)
	}
	switch {
	case oldMatches && newMatches:
		return stores.NewStoreOverlayForUpdate(newResource), nil
	case oldMatches:
		return stores.NewStoreOverlayForDelete(namespace, name), nil
	case newMatches:
		return stores.NewStoreOverlayForCreate(newResource), nil
	default:
		return stores.NewStoreOverlay(), nil
	}
}

func createAliasOverlayForDelete(alias resourceAlias, namespace, name string, oldResource *unstructured.Unstructured) (*stores.StoreOverlay, error) {
	if oldResource == nil && alias.filtered() {
		return nil, fmt.Errorf("DELETE has no old object required by selector on store %q", alias.name)
	}
	if oldResource == nil {
		if !alias.watchesNamespace(namespace) {
			return stores.NewStoreOverlay(), nil
		}
		return stores.NewStoreOverlayForDelete(namespace, name), nil
	}
	matches, err := alias.matches(oldResource)
	if err != nil {
		return nil, fmt.Errorf("matching deleted object selector on store %q: %w", alias.name, err)
	}
	if !matches {
		return stores.NewStoreOverlay(), nil
	}
	return stores.NewStoreOverlayForDelete(namespace, name), nil
}

func asUnstructured(object any) (*unstructured.Unstructured, error) {
	if resource, ok := object.(*unstructured.Unstructured); ok {
		return resource, nil
	}
	if _, ok := object.(runtime.Object); ok {
		converted, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object)
		if err != nil {
			return nil, fmt.Errorf("converting %T to unstructured: %w", object, err)
		}
		return &unstructured.Unstructured{Object: converted}, nil
	}
	return nil, fmt.Errorf("object has unsupported type %T", object)
}

// simplifyError simplifies an error message based on the validation phase.
func (c *Component) simplifyError(phase string, err error) string {
	if err == nil {
		return ""
	}
	switch phase {
	case phaseRender:
		return dataplane.SimplifyRenderingError(err)
	case "syntax", "schema", "semantic":
		return dataplane.SimplifyValidationError(err)
	default:
		return err.Error()
	}
}

// mapGVKToResourceAliases resolves an admission GVK to every configured store
// alias backed by the same GVR.
//
// The GVK string is "group/version.Kind" or "version.Kind" — the identifier the
// webhook registers its validators under.
func (c *Component) mapGVKToResourceAliases(gvk string) ([]resourceAlias, error) {
	// Split off the Kind (the segment after the final dot); everything before
	// it is the apiVersion ("group/version" or "version").
	parts := strings.Split(gvk, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid GVK format: %s", gvk)
	}
	kind := parts[len(parts)-1]
	apiVersion := strings.Join(parts[:len(parts)-1], ".")

	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return nil, fmt.Errorf("invalid GVK %q: %w", gvk, err)
	}

	gk := schema.GroupKind{Group: gv.Group, Kind: kind}
	mapping, err := c.restMapper.RESTMapping(gk, gv.Version)
	if err != nil && meta.IsNoMatchError(err) {
		// A deferred discovery mapper caches discovery for its lifetime, so a
		// CRD registered after that cache was first populated would resolve to
		// NoMatch permanently. Refresh discovery once and retry so a
		// late-registered watched resource validates without a controller
		// iteration restart.
		if resettable, ok := c.restMapper.(meta.ResettableRESTMapper); ok {
			resettable.Reset()
			mapping, err = c.restMapper.RESTMapping(gk, gv.Version)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("resolving resource type for GVK %q: %w", gvk, err)
	}
	key := mapping.Resource
	aliases := c.aliasesByGVR[key]
	if len(aliases) == 0 {
		// The alias map is keyed by the ONE apiVersion the config resolved to,
		// but the webhook intercepts every version the cluster serves for this
		// resource (the chart renders its rules from the full candidate list).
		// A watched resource is identified by group + plural; the version is an
		// encoding of the same object, so fall back to matching on those.
		// Without this an HTTPRoute written as v1beta1 is denied outright while
		// the identical v1 object is admitted.
		aliases = c.aliasesByGroupResource(key.Group, key.Resource)
	}
	if len(aliases) == 0 {
		return nil, fmt.Errorf("GVK %q resolves to unconfigured resource %s", gvk, mapping.Resource.String())
	}
	return slices.Clone(aliases), nil
}

// aliasesByGroupResource finds the watched-resource aliases for a group and
// plural, ignoring the apiVersion. Used only when the exact GVR misses, so the
// configured version keeps its direct hit.
//
// Two watched-resource entries may name the same group+plural under different
// apiVersions — nothing rejects that config, and buildResourceAliases keys by
// the full GVR, so they land under separate keys. Every one of them watches the
// object being admitted, so all their aliases are returned, exactly as the
// exact-GVR path returns every alias registered for its key. Returning the first
// match instead would drop the others, and pick which to drop at random: Go
// randomizes map iteration order.
//
// Sorted by alias name so the overlay set is built in a stable order.
func (c *Component) aliasesByGroupResource(group, resource string) []resourceAlias {
	var matched []resourceAlias
	for gvr, aliases := range c.aliasesByGVR {
		if gvr.Group == group && gvr.Resource == resource {
			matched = append(matched, aliases...)
		}
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].name < matched[j].name })
	return matched
}
