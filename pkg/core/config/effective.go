// Copyright 2025 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package config

import (
	"fmt"
	"sort"
	"strings"
)

// ServedVersionChecker reports whether the cluster (or an offline schema
// bundle) serves a resource at an exact API version. Implementations live in
// the coordination layer (live discovery) and the offline validate path
// (schema-dir CRD manifests); this package stays free of Kubernetes imports.
type ServedVersionChecker interface {
	// IsServed returns true when the plural resource is served at the given
	// group/version (e.g. "gateway.networking.k8s.io/v1", "tcproutes").
	IsServed(apiVersion, resources string) bool
}

// SchemaFieldChecker reports whether the RESOLVED schema of a served
// resource contains a specific field path. It backs the RequiresFields
// stripping on validation tests: a cluster may serve a resource at the
// same version string as newer releases while its schema generation
// lacks individual fields (e.g. Gateway API v1.1 serves httproutes at
// "v1" without the CORS filter). Implementations walk the resource's
// OpenAPI schema (live from the apiserver, or from --schema-dir
// offline); this package stays free of Kubernetes imports.
type SchemaFieldChecker interface {
	// FieldServed returns true when the schema served for the plural
	// resource at the given group/version contains the dot-separated
	// field path (descending into array items transparently). Any
	// error is treated as transient and fails the whole resolution —
	// silently stripping on a schema-fetch blip would disable features
	// spuriously.
	FieldServed(apiVersion, resources, fieldPath string) (bool, error)
}

// NamespaceScopeChecker reports whether a served resource is namespaced. A
// ServedVersionChecker that implements it lets ResolveEffective reject
// namespaces on a cluster-scoped resource, whose watch could never sync.
type NamespaceScopeChecker interface {
	IsNamespaced(apiVersion, resources string) bool
}

// Resolution describes the outcome of ResolveEffective: which version each
// watched resource resolved to, which optional resources are unavailable, and
// which config elements were stripped as a consequence.
type Resolution struct {
	// ResolvedVersions maps watched-resource names to the apiVersion the
	// controller actually watches (the first served candidate).
	ResolvedVersions map[string]string

	// Unavailable lists optional watched-resource names with no served
	// candidate version, sorted alphabetically.
	Unavailable []string

	// StrippedSnippets and StrippedTests list the names of templateSnippets /
	// validationTests removed because a resource they require is unavailable,
	// sorted alphabetically.
	StrippedSnippets []string
	StrippedTests    []string

	// AbsentIncrementalGroups lists publication groups whose every component
	// was stripped with an unavailable optional watched resource.
	AbsentIncrementalGroups []string

	// StrippedFieldTests lists the names of validationTests removed because
	// a field named in their RequiresFields is absent from the resolved
	// schema generation, sorted alphabetically. Disjoint from StrippedTests:
	// resource-level stripping wins when both apply. Unlike the other
	// stripped lists this is NOT derived from Unavailable — an in-place CRD
	// upgrade can change it while every resolved version stays the same, so
	// Equal compares it explicitly (the CRD watch relies on that to reload).
	StrippedFieldTests []string
}

// ResolveEffective resolves every watched resource to the first candidate
// version the checker reports as served and returns the EFFECTIVE config the
// rest of the controller consumes:
//
//   - each surviving WatchedResources entry has APIVersion set to the resolved
//     version and APIVersions cleared, so every downstream consumer of the
//     literal APIVersion (informer GVR, stores, schema fetch, webhook
//     registration, dry-run mapping, fixture defaulting) transparently uses
//     the resolved version;
//   - an optional resource with no served candidate is removed, together with
//     every TemplateSnippet / ValidationTest whose Requires names it;
//   - a ValidationTest whose RequiresFields names a field absent from the
//     resolved schema generation (probed via the SchemaFieldChecker) is
//     removed — same feature-absence semantics as Requires, one level finer;
//   - a required resource with no served candidate is an error (fail fast —
//     the alternative is an informer that never syncs and a controller that
//     never becomes Ready, with nothing in the logs naming the cause).
//
// fields may be nil for callers without a schema source; RequiresFields
// entries are then not probed and never strip (the offline validate path
// applies the same leniency when --schema-dir is absent).
//
// The input config is not mutated: the returned config shares everything
// except the three affected maps. The transformation is resource-agnostic —
// it consumes only candidate lists, plural names, and Requires /
// RequiresFields declarations from the configuration.
func ResolveEffective(cfg *Config, served ServedVersionChecker, fields SchemaFieldChecker) (*Config, *Resolution, error) {
	res := &Resolution{ResolvedVersions: make(map[string]string, len(cfg.WatchedResources))}
	unavailable := make(map[string]bool)

	watched := make(map[string]WatchedResource, len(cfg.WatchedResources))
	// Deterministic iteration so multi-resource error messages are stable.
	names := make([]string, 0, len(cfg.WatchedResources))
	for name := range cfg.WatchedResources {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		resource := cfg.WatchedResources[name]
		resolved := ""
		for _, candidate := range resource.CandidateVersions() {
			if served.IsServed(candidate, resource.Resources) {
				resolved = candidate
				break
			}
		}
		switch {
		case resolved != "":
			if err := checkNamespaceScope(served, name, resolved, &resource); err != nil {
				return nil, nil, err
			}
			resource.APIVersion = resolved
			resource.APIVersions = nil
			watched[name] = resource
			res.ResolvedVersions[name] = resolved
		case resource.Optional:
			unavailable[name] = true
			res.Unavailable = append(res.Unavailable, name)
		default:
			return nil, nil, fmt.Errorf(
				"watched resource %q is required but no candidate version is served (candidates: %v, resource: %s)",
				name, resource.CandidateVersions(), resource.Resources)
		}
	}

	snippets := cfg.TemplateSnippets
	if len(unavailable) > 0 {
		snippets = make(map[string]TemplateSnippet, len(cfg.TemplateSnippets))
		for name, snippet := range cfg.TemplateSnippets {
			if requiresAny(snippet.Requires, unavailable) {
				res.StrippedSnippets = append(res.StrippedSnippets, name)
				continue
			}
			snippets[name] = snippet
		}
		sort.Strings(res.StrippedSnippets)
	}

	tests, err := stripTests(cfg, watched, unavailable, fields, res)
	if err != nil {
		return nil, nil, err
	}

	effective := *cfg
	effective.WatchedResources = watched
	effective.TemplateSnippets = snippets
	effective.AbsentIncrementalGroups = absentIncrementalGroups(cfg.TemplateSnippets, snippets)
	for group := range effective.AbsentIncrementalGroups {
		res.AbsentIncrementalGroups = append(res.AbsentIncrementalGroups, group)
	}
	sort.Strings(res.AbsentIncrementalGroups)
	effective.ValidationTests = tests
	return &effective, res, nil
}

func checkNamespaceScope(served ServedVersionChecker, name, apiVersion string, resource *WatchedResource) error {
	scopes, ok := served.(NamespaceScopeChecker)
	if !ok || len(resource.Namespaces) == 0 || scopes.IsNamespaced(apiVersion, resource.Resources) {
		return nil
	}
	return fmt.Errorf("watched resource %q lists namespaces, but %s %s is cluster-scoped, so its watch can never start. Remove namespaces from it",
		name, apiVersion, resource.Resources)
}

func absentIncrementalGroups(
	declared map[string]TemplateSnippet,
	effective map[string]TemplateSnippet,
) map[string]struct{} {
	groups := make(map[string]struct{})
	for name, snippet := range declared {
		if snippet.Incremental != nil {
			groups[incrementalGroupName(name, snippet.Incremental)] = struct{}{}
		}
	}
	for name, snippet := range effective {
		if snippet.Incremental != nil {
			delete(groups, incrementalGroupName(name, snippet.Incremental))
		}
	}
	if len(groups) == 0 {
		return nil
	}
	return groups
}

// stripTests applies both stripping levels to the validation tests:
// resource-level Requires against the unavailable set (wins when both
// apply), then field-level RequiresFields against the resolved schemas.
// Stripped names are recorded on the resolution, sorted.
func stripTests(cfg *Config, watched map[string]WatchedResource, unavailable map[string]bool, fields SchemaFieldChecker, res *Resolution) (map[string]ValidationTest, error) {
	tests := make(map[string]ValidationTest, len(cfg.ValidationTests))
	for name := range cfg.ValidationTests {
		test := cfg.ValidationTests[name]
		if requiresAny(test.Requires, unavailable) {
			res.StrippedTests = append(res.StrippedTests, name)
			continue
		}
		missing, err := missingRequiredField(&test, watched, unavailable, fields)
		if err != nil {
			return nil, fmt.Errorf("validation test %q: %w", name, err)
		}
		if missing != "" {
			res.StrippedFieldTests = append(res.StrippedFieldTests, name)
			continue
		}
		tests[name] = test
	}
	sort.Strings(res.StrippedTests)
	sort.Strings(res.StrippedFieldTests)
	return tests, nil
}

// missingRequiredField probes the test's RequiresFields entries against the
// resolved schemas and returns the first entry whose field is absent ("" when
// all are present, or when fields is nil). An entry referencing an
// unavailable optional resource counts as absent — the field trivially
// doesn't exist. A dangling first segment is an error; the structural
// validator rejects it at load time, so hitting it here means the config
// bypassed validation.
func missingRequiredField(test *ValidationTest, watched map[string]WatchedResource, unavailable map[string]bool, fields SchemaFieldChecker) (string, error) {
	if fields == nil {
		return "", nil
	}
	for _, entry := range test.RequiresFields {
		key, fieldPath, ok := strings.Cut(entry, ".")
		if !ok || fieldPath == "" {
			return "", fmt.Errorf("requiresFields entry %q is not of the form \"<watchedResource>.<field.path>\"", entry)
		}
		if unavailable[key] {
			return entry, nil
		}
		resource, ok := watched[key]
		if !ok {
			return "", fmt.Errorf("requiresFields entry %q does not name a watched resource", entry)
		}
		servedField, err := fields.FieldServed(resource.APIVersion, resource.Resources, fieldPath)
		if err != nil {
			return "", fmt.Errorf("probing schema field %q: %w", entry, err)
		}
		if !servedField {
			return entry, nil
		}
	}
	return "", nil
}

func requiresAny(requires []string, set map[string]bool) bool {
	for _, r := range requires {
		if set[r] {
			return true
		}
	}
	return false
}

// Equal reports whether two resolutions describe the same outcome — the same
// resolved version per resource, the same unavailable set, and the same
// field-stripped test set. Used by the CRD watch to decide whether a CRD
// change actually alters the effective config. StrippedSnippets and
// StrippedTests are derived from Unavailable and need no comparison of their
// own; StrippedFieldTests is derived from schema CONTENTS and must be
// compared — an in-place CRD upgrade that adds the missing fields changes it
// while every resolved version stays identical, and that difference is what
// makes the CRD watch reload and un-strip the tests.
func (r *Resolution) Equal(other *Resolution) bool {
	if r == nil || other == nil {
		return r == other
	}
	if len(r.ResolvedVersions) != len(other.ResolvedVersions) || len(r.Unavailable) != len(other.Unavailable) {
		return false
	}
	for name, version := range r.ResolvedVersions {
		if other.ResolvedVersions[name] != version {
			return false
		}
	}
	for i := range r.Unavailable {
		if r.Unavailable[i] != other.Unavailable[i] {
			return false
		}
	}
	if len(r.StrippedFieldTests) != len(other.StrippedFieldTests) {
		return false
	}
	for i := range r.StrippedFieldTests {
		if r.StrippedFieldTests[i] != other.StrippedFieldTests[i] {
			return false
		}
	}
	return true
}
