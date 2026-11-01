// Copyright 2026 Philipp Hossner
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

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/conversion"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testrunner"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

var (
	preflightResourcePaths        []string
	preflightResourcesFromCluster bool
)

func init() {
	preflightCmd.Flags().StringArrayVar(&preflightResourcePaths, "resources", nil,
		"Manifest file or directory (repeatable) holding the resources the controller will watch. "+
			"The configuration is also rendered against them and checked with haproxy -c.")
	preflightCmd.Flags().BoolVar(&preflightResourcesFromCluster, "resources-from-cluster", false,
		"Also render against the watched resources listed read-only from the target cluster, "+
			"with the selectors and namespaces the controller watches.")
}

// watchedSelection is what the controller would watch for one resource name.
type watchedSelection struct {
	name       string
	gvr        schema.GroupVersionResource
	kind       string
	namespaces map[string]bool
	labels     labels.Selector
	fields     fields.Selector
}

func (w *watchedSelection) admits(object *unstructured.Unstructured) bool {
	if len(w.namespaces) > 0 && !w.namespaces[object.GetNamespace()] {
		return false
	}
	if !w.labels.Matches(labels.Set(object.GetLabels())) {
		return false
	}
	values := fields.Set{}
	for _, requirement := range w.fields.Requirements() {
		value, _, _ := unstructured.NestedFieldNoCopy(object.Object, strings.Split(requirement.Field, ".")...)
		if value != nil {
			values[requirement.Field] = fmt.Sprint(value)
		}
	}
	return w.fields.Matches(values)
}

// checkPreflightResources renders the configuration against the operator's
// resources. Nothing to do unless --resources or --resources-from-cluster is set.
func checkPreflightResources(ctx context.Context, configFile string, schemas schemaSource, logger *slog.Logger) error {
	if len(preflightResourcePaths) == 0 && !preflightResourcesFromCluster {
		return nil
	}
	setup, err := setupValidation(ctx, []string{configFile}, schemas, nil, logger)
	if err != nil {
		return err
	}
	defer setup.Cleanup()
	cfg, err := conversion.ConvertSpec(setup.ConfigSpec)
	if err != nil {
		return fmt.Errorf("converting config: %w", err)
	}

	selections, err := watchedSelections(cfg, schemas)
	if err != nil {
		return err
	}
	resources, sources, err := collectResources(ctx, selections, schemas)
	if err != nil {
		return err
	}
	count := 0
	for _, items := range resources {
		count += len(items)
	}
	fmt.Fprintf(os.Stderr, "==> rendering against %d watched resources from %s\n", count, strings.Join(sources, " and "))

	runner := testrunner.New(cfg, setup.Engine, setup.ValidationPaths, &testrunner.Options{
		Logger:             logger,
		Workers:            1,
		Capabilities:       setup.Capabilities,
		HAProxyVersion:     setup.HAProxyVersion,
		TypedResourceTypes: setup.TypedResourceTypes,
	})
	_, valid, err := runner.ValidateResources(ctx, resources)
	if err != nil {
		return fmt.Errorf("the configuration doesn't render against these resources, so the controller would stop "+
			"reconciling: %s", templating.RenderErrorMessage(err))
	}
	if !valid.Passed {
		return fmt.Errorf("against these resources the configuration renders a config HAProxy rejects, so the "+
			"controller would keep its last valid one: %s", valid.Error)
	}
	fmt.Fprintln(os.Stderr, "✓ HAProxy accepts the configuration rendered from these resources")
	return nil
}

// watchedSelections resolves each watched resource's kind through the schema
// source: a manifest names a kind, a watch names a plural.
func watchedSelections(cfg *config.Config, schemas schemaSource) ([]*watchedSelection, error) {
	names := make([]string, 0, len(cfg.WatchedResources))
	for name := range cfg.WatchedResources {
		names = append(names, name)
	}
	sort.Strings(names)

	selections := make([]*watchedSelection, 0, len(names))
	for _, name := range names {
		watched := cfg.WatchedResources[name]
		if watched.APIVersion == "" {
			continue
		}
		gv, err := schema.ParseGroupVersion(watched.APIVersion)
		if err != nil {
			return nil, fmt.Errorf("watched resource %s: %w", name, err)
		}
		kind, err := kindOf(schemas, watched.APIVersion, watched.Resources)
		if err != nil {
			return nil, fmt.Errorf("watched resource %s: %w", name, err)
		}
		fieldSelector, err := fields.ParseSelector(watched.FieldSelector)
		if err != nil {
			return nil, fmt.Errorf("watched resource %s field selector %q: %w", name, watched.FieldSelector, err)
		}
		namespaces := map[string]bool{}
		for _, namespace := range watched.Namespaces {
			namespaces[namespace] = true
		}
		selections = append(selections, &watchedSelection{
			name:       name,
			gvr:        gv.WithResource(watched.Resources),
			kind:       kind,
			namespaces: namespaces,
			labels:     labels.SelectorFromSet(watched.LabelSelector),
			fields:     fieldSelector,
		})
	}
	return selections, nil
}

func kindOf(schemas schemaSource, apiVersion, plural string) (string, error) {
	if schemas.dir != nil {
		if gvk, ok := schemas.dir.PluralsFor()[apiVersion][plural]; ok {
			return gvk.Kind, nil
		}
	}
	if schemas.live != nil {
		list, err := schemas.live.discovery().ServerResourcesForGroupVersion(apiVersion)
		if err == nil {
			for i := range list.APIResources {
				if list.APIResources[i].Name == plural {
					return list.APIResources[i].Kind, nil
				}
			}
		}
	}
	return "", fmt.Errorf("no schema names the kind of %s %s, so manifests can't be matched to it; "+
		"pass --schema-dir with its CRD or run against the cluster", apiVersion, plural)
}

func collectResources(ctx context.Context, selections []*watchedSelection, schemas schemaSource) (resources map[string][]any, sources []string, err error) {
	resources = map[string][]any{}
	if len(preflightResourcePaths) > 0 {
		if err := addManifestResources(resources, selections); err != nil {
			return nil, nil, err
		}
		sources = append(sources, strings.Join(preflightResourcePaths, ", "))
	}
	if preflightResourcesFromCluster {
		if err := addClusterResources(ctx, resources, selections, schemas); err != nil {
			return nil, nil, err
		}
		sources = append(sources, "the cluster")
	}
	return resources, sources, nil
}

func addManifestResources(resources map[string][]any, selections []*watchedSelection) error {
	objects, err := readManifests(preflightResourcePaths)
	if err != nil {
		return err
	}
	for _, object := range objects {
		for _, selection := range selections {
			if object.GetAPIVersion() == selection.gvr.GroupVersion().String() && object.GetKind() == selection.kind &&
				selection.admits(object) {
				resources[selection.name] = append(resources[selection.name], fixtureObject(object))
			}
		}
	}
	return nil
}

func addClusterResources(ctx context.Context, resources map[string][]any, selections []*watchedSelection, schemas schemaSource) error {
	live := schemas.live
	if live == nil {
		var err error
		if live, err = connectLiveCluster(preflightKubeconfig); err != nil {
			return fmt.Errorf("--resources-from-cluster: %w", err)
		}
	}
	for _, selection := range selections {
		items, err := listWatched(ctx, live, selection)
		if err != nil {
			return err
		}
		resources[selection.name] = append(resources[selection.name], items...)
	}
	return nil
}

func listWatched(ctx context.Context, live *liveCluster, selection *watchedSelection) ([]any, error) {
	namespaces := []string{metav1.NamespaceAll}
	if len(selection.namespaces) > 0 {
		namespaces = namespaces[:0]
		for namespace := range selection.namespaces {
			namespaces = append(namespaces, namespace)
		}
		sort.Strings(namespaces)
	}
	var items []any
	for _, namespace := range namespaces {
		list, err := live.dyn.Resource(selection.gvr).Namespace(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: selection.labels.String(),
			FieldSelector: selection.fields.String(),
		})
		if err != nil {
			return nil, fmt.Errorf("listing %s for --resources-from-cluster: %w", selection.name, err)
		}
		for i := range list.Items {
			items = append(items, fixtureObject(&list.Items[i]))
		}
	}
	return items, nil
}

func fixtureObject(object *unstructured.Unstructured) any {
	unstructured.RemoveNestedField(object.Object, "metadata", "managedFields")
	return object.Object
}

// readManifests reads every YAML document under the given files and
// directories, expanding `kind: List` items.
func readManifests(paths []string) ([]*unstructured.Unstructured, error) {
	files, err := manifestFiles(paths)
	if err != nil {
		return nil, err
	}
	var objects []*unstructured.Unstructured
	for _, file := range files {
		decoded, err := decodeManifests(file)
		if err != nil {
			return nil, err
		}
		objects = append(objects, decoded...)
	}
	return objects, nil
}

// manifestFiles lists each named file, and the .yaml and .yml files under each
// named directory.
func manifestFiles(paths []string) ([]string, error) {
	var files []string
	for _, path := range paths {
		root := filepath.Clean(path)
		err := filepath.WalkDir(root, func(file string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() && (file == root || strings.HasSuffix(file, ".yaml") || strings.HasSuffix(file, ".yml")) {
				files = append(files, file)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("reading --resources %s: %w", path, err)
		}
	}
	return files, nil
}

func decodeManifests(file string) ([]*unstructured.Unstructured, error) {
	data, err := os.ReadFile(filepath.Clean(file))
	if err != nil {
		return nil, fmt.Errorf("reading --resources %s: %w", file, err)
	}
	var objects []*unstructured.Unstructured
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		var object map[string]any
		if err := decoder.Decode(&object); err != nil {
			if errors.Is(err, io.EOF) {
				return objects, nil
			}
			return nil, fmt.Errorf("parsing --resources %s: %w", file, err)
		}
		if object == nil {
			continue
		}
		item := &unstructured.Unstructured{Object: object}
		if !item.IsList() {
			objects = append(objects, item)
			continue
		}
		list, err := item.ToList()
		if err != nil {
			return nil, fmt.Errorf("parsing list in --resources %s: %w", file, err)
		}
		for i := range list.Items {
			objects = append(objects, &list.Items[i])
		}
	}
}
