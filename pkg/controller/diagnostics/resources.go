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

package diagnostics

import (
	"context"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	hapticv1 "github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/conversion"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/configpublisher"
	"github.com/haproxy-haptic/HAPTIC/pkg/k8s/indexer"
)

var (
	inputGVR   = hapticv1.SchemeGroupVersion.WithResource("haproxytemplateconfigs")
	libraryGVR = hapticv1.SchemeGroupVersion.WithResource("haproxytemplatelibraries")
	outputGVR  = hapticv1.SchemeGroupVersion.WithResource("haproxycfgs")
)

func (c *Collector) collectConfigurations(ctx context.Context, report *Report) string {
	input, err := c.dynamic.Resource(inputGVR).Namespace(c.options.Namespace).Get(ctx, c.options.ConfigName, metav1.GetOptions{})
	if err != nil {
		report.incomplete("configuration-unavailable", c.options.ConfigName, "Check --crd-name and allow reading the template configuration.")
	} else {
		c.appendConfiguration(report, input)
		c.collectInputs(ctx, report, input)
	}
	name := configpublisher.GenerateRuntimeConfigName(c.options.ConfigName)
	output, err := c.dynamic.Resource(outputGVR).Namespace(c.options.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		report.incomplete("published-config-unavailable", name, "Check reconciliation and allow reading the published HAProxyCfg.")
		return ""
	}
	c.appendConfiguration(report, output)
	path, _, _ := unstructured.NestedString(output.Object, "spec", "path")
	return path
}

func (*Collector) appendConfiguration(report *Report, object *unstructured.Unstructured) {
	view, err := configurationView(object)
	if err != nil {
		report.incomplete("configuration-malformed", object.GetName(), "Inspect the configuration's schema and status.")
		return
	}
	if view.Kind == "HAProxyTemplateConfig" && (view.ValidationStatus != "Valid" || view.ObservedGeneration != view.Generation) {
		report.problem("configuration-not-validated", view.Name, "Inspect the current configuration validation status and generation.")
	}
	if view.ValidationErrors > 0 {
		report.problem("configuration-validation-failed", view.Name, "Inspect this configuration's private validation diagnostics.")
	}
	report.Configurations = append(report.Configurations, view)
}

func (c *Collector) collectInputs(ctx context.Context, report *Report, input *unstructured.Unstructured) {
	refs, err := conversion.LibraryRefsOf(input)
	if err != nil {
		report.incomplete("library-references-invalid", input.GetName(), "Repair the template configuration's library references.")
		return
	}
	documents := []*unstructured.Unstructured{input}
	for _, ref := range refs {
		library, err := c.dynamic.Resource(libraryGVR).Namespace(c.options.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			report.incomplete("library-unavailable", ref.Name, "Allow reading the referenced template library and check its revision.")
			return
		}
		documents = append(documents, library)
	}
	ordered, err := conversion.AssembleSources(documents)
	if err != nil {
		report.incomplete("library-revision-mismatch", input.GetName(), "Reconcile the complete chart so all referenced library revisions match.")
		return
	}
	merged, _, err := conversion.MergeSpecs(ordered)
	if err != nil {
		report.incomplete("configuration-merge-failed", input.GetName(), "Inspect the controller's configuration merge diagnostics.")
		return
	}
	var config hapticv1.HAProxyTemplateConfig
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(merged.Object, &config); err != nil {
		report.incomplete("configuration-decode-failed", input.GetName(), "Inspect the template configuration schema.")
		return
	}
	names := make([]string, 0, len(config.Spec.WatchedResources))
	for name := range config.Spec.WatchedResources {
		names = append(names, name)
	}
	slices.Sort(names)
	remaining := c.options.MaxResources
	for _, name := range names {
		watch := config.Spec.WatchedResources[name]
		c.collectWatch(ctx, report, name, &watch, &remaining)
	}
}

func (c *Collector) collectWatch(ctx context.Context, report *Report, name string, watch *hapticv1.WatchedResource, remaining *int) {
	versions := watch.APIVersions
	if watch.APIVersion != "" {
		versions = []string{watch.APIVersion}
	}
	if len(versions) == 0 {
		report.incomplete("watch-version-invalid", name, "Configure at least one API version for this watch.")
		return
	}
	var matcher *indexer.FieldSelectorMatcher
	if watch.FieldSelector != "" {
		var err error
		matcher, err = indexer.NewFieldSelectorMatcher(watch.FieldSelector)
		if err != nil {
			report.incomplete("watch-selector-invalid", name, "Repair this watch's field selector.")
			return
		}
	}
	namespaces := watch.Namespaces
	if len(namespaces) == 0 {
		namespaces = []string{metav1.NamespaceAll}
	}
	for _, version := range versions {
		gv, err := schema.ParseGroupVersion(version)
		if err != nil {
			report.incomplete("watch-version-invalid", name, "Repair this watch's API versions.")
			return
		}
		if c.collectResourcePages(ctx, report, name, gv.WithResource(watch.Resources), namespaces, watch.LabelSelector, matcher, remaining) {
			return
		}
	}
	if !watch.Optional {
		report.incomplete("watch-api-unavailable", name, "Install the required resource API or repair this watch's API version.")
	}
}

// collectResourcePages lists a watch in each namespace it reads. It returns
// false only when the API version is not served.
func (c *Collector) collectResourcePages(ctx context.Context, report *Report, name string, gvr schema.GroupVersionResource, namespaces []string, selector string, matcher *indexer.FieldSelectorMatcher, remaining *int) bool {
	for _, namespace := range namespaces {
		continuation := ""
		for {
			if *remaining <= 0 {
				report.incomplete("resource-limit-reached", name, "Increase --max-resources to collect the remaining watched resources.")
				return true
			}
			objects, err := c.dynamic.Resource(gvr).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector, Limit: int64(min(*remaining, 250)), Continue: continuation})
			if apierrors.IsNotFound(err) {
				return false
			}
			if err != nil {
				report.incomplete("watch-read-failed", name, "Allow listing this watched resource API and retry.")
				return true
			}
			if !report.appendWatchedObjects(name, objects.Items, matcher, remaining) {
				return true
			}
			continuation = objects.GetContinue()
			if continuation == "" {
				break
			}
		}
	}
	return true
}

func (r *Report) appendWatchedObjects(name string, objects []unstructured.Unstructured, matcher *indexer.FieldSelectorMatcher, remaining *int) bool {
	for index := range objects {
		if *remaining <= 0 {
			r.incomplete("resource-limit-reached", name, "Increase --max-resources to collect the remaining watched resources.")
			return false
		}
		*remaining--
		object := &objects[index]
		if matcher != nil {
			matches, err := matcher.Matches(object.Object)
			if err != nil {
				r.incomplete("watch-filter-failed", name, "Repair this watch's field selector.")
				return false
			}
			if !matches {
				continue
			}
		}
		view := resourceView(object, name)
		if len(view.Conditions) > 0 {
			r.Resources = append(r.Resources, view)
		}
	}
	return true
}
