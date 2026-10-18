// Copyright 2025 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// servedSet is a ServedVersionChecker backed by a fixed set of
// "apiVersion|resources" keys.
type servedSet map[string]bool

func (s servedSet) IsServed(apiVersion, resources string) bool {
	return s[apiVersion+"|"+resources]
}

func TestResolveEffective(t *testing.T) {
	cfg := &Config{
		WatchedResources: map[string]WatchedResource{
			"httproutes": {
				APIVersions: []string{"example.io/v1", "example.io/v1beta1"},
				Resources:   "httproutes",
				IndexBy:     []string{"metadata.name"},
			},
			"tcproutes": {
				APIVersions: []string{"example.io/v1", "example.io/v1alpha2"},
				Resources:   "tcproutes",
				Optional:    true,
				IndexBy:     []string{"metadata.name"},
			},
			"services": {
				APIVersion: "v1",
				Resources:  "services",
				IndexBy:    []string{"metadata.name"},
			},
		},
		TemplateSnippets: map[string]TemplateSnippet{
			"shared":      {Name: "shared", Template: "x"},
			"tcp-feature": {Name: "tcp-feature", Template: "x", Requires: []string{"tcproutes"}},
			"http-only":   {Name: "http-only", Template: "x", Requires: []string{"httproutes"}},
		},
		ValidationTests: map[string]ValidationTest{
			"test-shared": {},
			"test-tcp":    {Requires: []string{"tcproutes"}},
		},
	}

	t.Run("resolves first served candidate and strips unavailable optional features", func(t *testing.T) {
		served := servedSet{
			"example.io/v1beta1|httproutes": true, // v1 NOT served -> falls back
			"v1|services":                   true,
			// tcproutes served at no candidate
		}

		effective, res, err := ResolveEffective(cfg, served, nil)
		require.NoError(t, err)

		assert.Equal(t, "example.io/v1beta1", effective.WatchedResources["httproutes"].APIVersion)
		assert.Empty(t, effective.WatchedResources["httproutes"].APIVersions,
			"resolved entries must clear the candidate list so downstream consumers see one version")
		assert.Equal(t, "v1", effective.WatchedResources["services"].APIVersion)
		assert.NotContains(t, effective.WatchedResources, "tcproutes")

		assert.NotContains(t, effective.TemplateSnippets, "tcp-feature")
		assert.Contains(t, effective.TemplateSnippets, "shared")
		assert.Contains(t, effective.TemplateSnippets, "http-only",
			"requires on an AVAILABLE resource must not strip")
		assert.NotContains(t, effective.ValidationTests, "test-tcp")
		assert.Contains(t, effective.ValidationTests, "test-shared")

		assert.Equal(t, map[string]string{
			"httproutes": "example.io/v1beta1",
			"services":   "v1",
		}, res.ResolvedVersions)
		assert.Equal(t, []string{"tcproutes"}, res.Unavailable)
		assert.Equal(t, []string{"tcp-feature"}, res.StrippedSnippets)
		assert.Equal(t, []string{"test-tcp"}, res.StrippedTests)

		// Input config untouched.
		assert.Contains(t, cfg.WatchedResources, "tcproutes")
		assert.Contains(t, cfg.TemplateSnippets, "tcp-feature")
		assert.NotEmpty(t, cfg.WatchedResources["httproutes"].APIVersions)
	})

	t.Run("preference order wins when several candidates are served", func(t *testing.T) {
		served := servedSet{
			"example.io/v1|httproutes":      true,
			"example.io/v1beta1|httproutes": true,
			"example.io/v1|tcproutes":       true,
			"example.io/v1alpha2|tcproutes": true,
			"v1|services":                   true,
		}

		effective, res, err := ResolveEffective(cfg, served, nil)
		require.NoError(t, err)
		assert.Equal(t, "example.io/v1", effective.WatchedResources["httproutes"].APIVersion)
		assert.Equal(t, "example.io/v1", effective.WatchedResources["tcproutes"].APIVersion)
		assert.Empty(t, res.Unavailable)
		assert.Contains(t, effective.TemplateSnippets, "tcp-feature")
		assert.Contains(t, effective.ValidationTests, "test-tcp")
	})

	t.Run("required resource with no served candidate fails fast with a named error", func(t *testing.T) {
		served := servedSet{
			"example.io/v1beta1|httproutes": true,
			// services (required) not served
		}

		_, _, err := ResolveEffective(cfg, served, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `"services"`)
		assert.Contains(t, err.Error(), "required but no candidate version is served")
		assert.Contains(t, err.Error(), "[v1]")
	})

	t.Run("no unavailable resources leaves snippet and test maps untouched", func(t *testing.T) {
		served := servedSet{
			"example.io/v1|httproutes": true,
			"example.io/v1|tcproutes":  true,
			"v1|services":              true,
		}

		effective, res, err := ResolveEffective(cfg, served, nil)
		require.NoError(t, err)
		assert.Empty(t, res.Unavailable)
		assert.Empty(t, res.StrippedSnippets)
		assert.Empty(t, res.StrippedTests)
		assert.Len(t, effective.TemplateSnippets, 3)
		assert.Len(t, effective.ValidationTests, 2)
	})
}

func TestResolveEffectiveAuthenticatesOnlyFullyStrippedIncrementalGroups(t *testing.T) {
	cfg := &Config{
		WatchedResources: map[string]WatchedResource{
			"routes":   {APIVersion: "example.io/v1", Resources: "routes"},
			"policies": {APIVersion: "example.io/v1", Resources: "policies", Optional: true},
		},
		TemplateSnippets: map[string]TemplateSnippet{
			"policy-a": {
				Template: "x", Requires: []string{"policies"},
				Incremental: &IncrementalTemplate{
					BindingsTemplate: "{}", Group: "policies",
					Effects: []IncrementalEffect{IncrementalEffectPublishValue},
				},
			},
			"route": {
				Template: "x", Requires: []string{"routes"},
				Incremental: &IncrementalTemplate{
					BindingsTemplate: "{}", Group: "routes", OptionalConsumes: []string{"policies"},
				},
			},
		},
	}
	require.NoError(t, ValidateTemplateStructure(cfg))
	effective, resolution, err := ResolveEffective(cfg, servedSet{"example.io/v1|routes": true}, nil)
	require.NoError(t, err)
	assert.Equal(t, map[string]struct{}{"policies": {}}, effective.AbsentIncrementalGroups)
	assert.Equal(t, []string{"policies"}, resolution.AbsentIncrementalGroups)
	assert.NoError(t, ValidateTemplateStructure(effective))

	present, resolution, err := ResolveEffective(cfg, servedSet{
		"example.io/v1|routes": true, "example.io/v1|policies": true,
	}, nil)
	require.NoError(t, err)
	assert.Empty(t, present.AbsentIncrementalGroups)
	assert.Empty(t, resolution.AbsentIncrementalGroups)
	assert.NoError(t, ValidateTemplateStructure(present))
}

// fieldSet is a SchemaFieldChecker backed by a fixed set of
// "apiVersion|resources|fieldPath" keys; unknown keys are absent.
// A non-nil err makes every probe fail (the transient case).
type fieldSet struct {
	present map[string]bool
	err     error
	probes  []string
}

func (f *fieldSet) FieldServed(apiVersion, resources, fieldPath string) (bool, error) {
	key := apiVersion + "|" + resources + "|" + fieldPath
	f.probes = append(f.probes, key)
	if f.err != nil {
		return false, f.err
	}
	return f.present[key], nil
}

// TestResolveEffective_RequiresFields pins the field-level stripping: a test
// whose requiresFields names a field absent from the RESOLVED schema
// generation strips even though the resource itself is served — the exact
// gap that kept the controller from starting on Gateway API v1.1/v1.4 clusters
// (issue #59), where resource-level requires can never fire.
func TestResolveEffective_RequiresFields(t *testing.T) {
	baseCfg := func() *Config {
		return &Config{
			WatchedResources: map[string]WatchedResource{
				"httproutes": {
					APIVersions: []string{"example.io/v1"},
					Resources:   "httproutes",
					IndexBy:     []string{"metadata.name"},
				},
				"tcproutes": {
					APIVersions: []string{"example.io/v1"},
					Resources:   "tcproutes",
					Optional:    true,
					IndexBy:     []string{"metadata.name"},
				},
			},
			ValidationTests: map[string]ValidationTest{
				"test-plain": {},
				"test-cors":  {RequiresFields: []string{"httproutes.spec.rules.filters.cors"}},
				"test-both": {
					Requires:       []string{"tcproutes"},
					RequiresFields: []string{"httproutes.spec.rules.filters.cors"},
				},
				"test-on-unavailable": {RequiresFields: []string{"tcproutes.spec.rules"}},
			},
		}
	}
	served := servedSet{"example.io/v1|httproutes": true} // tcproutes unserved

	t.Run("absent field strips the test and reports it separately", func(t *testing.T) {
		fields := &fieldSet{present: map[string]bool{}}
		effective, res, err := ResolveEffective(baseCfg(), served, fields)
		require.NoError(t, err)

		assert.NotContains(t, effective.ValidationTests, "test-cors")
		assert.Contains(t, effective.ValidationTests, "test-plain")
		assert.NotContains(t, effective.ValidationTests, "test-on-unavailable",
			"a field on an unavailable optional resource is trivially absent")
		assert.Equal(t, []string{"test-cors", "test-on-unavailable"}, res.StrippedFieldTests)
		assert.Equal(t, []string{"test-both"}, res.StrippedTests,
			"resource-level requires stripping wins over field stripping")
	})

	t.Run("present field keeps the test", func(t *testing.T) {
		fields := &fieldSet{present: map[string]bool{
			"example.io/v1|httproutes|spec.rules.filters.cors": true,
		}}
		effective, res, err := ResolveEffective(baseCfg(), served, fields)
		require.NoError(t, err)
		assert.Contains(t, effective.ValidationTests, "test-cors")
		assert.Equal(t, []string{"test-on-unavailable"}, res.StrippedFieldTests,
			"only the unavailable-resource field entry strips")
		assert.Contains(t, fields.probes, "example.io/v1|httproutes|spec.rules.filters.cors",
			"the probe must run against the RESOLVED version and plural")
	})

	t.Run("field-checker error fails the whole resolution", func(t *testing.T) {
		fields := &fieldSet{err: assert.AnError}
		_, _, err := ResolveEffective(baseCfg(), served, fields)
		require.Error(t, err, "transient schema-probe failures must not silently strip")
		assert.ErrorIs(t, err, assert.AnError)
	})

	t.Run("nil checker skips field probing entirely", func(t *testing.T) {
		effective, res, err := ResolveEffective(baseCfg(), served, nil)
		require.NoError(t, err)
		assert.Contains(t, effective.ValidationTests, "test-cors")
		assert.Empty(t, res.StrippedFieldTests)
	})

	t.Run("malformed entry fails resolution", func(t *testing.T) {
		cfg := baseCfg()
		cfg.ValidationTests["test-bad"] = ValidationTest{RequiresFields: []string{"httproutes"}}
		_, _, err := ResolveEffective(cfg, served, &fieldSet{present: map[string]bool{}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "requiresFields")
	})
}

// TestResolutionEqual_StrippedFieldTests pins that an in-place CRD upgrade
// which only changes the field-stripped set — every resolved version and the
// unavailable set identical — produces a DIFFERENT resolution, so the CRD
// watch reloads and un-strips the tests.
func TestResolutionEqual_StrippedFieldTests(t *testing.T) {
	base := &Resolution{
		ResolvedVersions:   map[string]string{"httproutes": "example.io/v1"},
		StrippedFieldTests: []string{"test-cors"},
	}
	same := &Resolution{
		ResolvedVersions:   map[string]string{"httproutes": "example.io/v1"},
		StrippedFieldTests: []string{"test-cors"},
	}
	upgraded := &Resolution{
		ResolvedVersions: map[string]string{"httproutes": "example.io/v1"},
		// CRD upgraded in place: the field now exists, nothing strips.
	}
	renamed := &Resolution{
		ResolvedVersions:   map[string]string{"httproutes": "example.io/v1"},
		StrippedFieldTests: []string{"test-other"},
	}

	assert.True(t, base.Equal(same))
	assert.False(t, base.Equal(upgraded), "field appearing after CRD upgrade must change the resolution")
	assert.False(t, upgraded.Equal(base))
	assert.False(t, base.Equal(renamed))
}

// scopedServedSet also reports which served resources are namespaced.
type scopedServedSet struct {
	servedSet
	namespaced map[string]bool
}

func (s scopedServedSet) IsNamespaced(apiVersion, resources string) bool {
	return s.namespaced[apiVersion+"|"+resources]
}

func TestResolveEffective_NamespacesRequireANamespacedResource(t *testing.T) {
	served := scopedServedSet{
		servedSet:  servedSet{"v1|secrets": true, "v1|namespaces": true},
		namespaced: map[string]bool{"v1|secrets": true},
	}
	resolve := func(resources string, namespaces []string) error {
		cfg := &Config{WatchedResources: map[string]WatchedResource{
			"watched": {APIVersion: "v1", Resources: resources, IndexBy: []string{"metadata.namespace"}, Namespaces: namespaces},
		}}
		_, _, err := ResolveEffective(cfg, served, nil)
		return err
	}

	require.NoError(t, resolve("secrets", []string{"team-a"}))
	require.NoError(t, resolve("namespaces", nil))
	err := resolve("namespaces", []string{"team-a"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `watched resource "watched" lists namespaces, but v1 namespaces is cluster-scoped`)

	// A checker that cannot tell scopes resolves without the check.
	cfg := &Config{WatchedResources: map[string]WatchedResource{
		"watched": {APIVersion: "v1", Resources: "namespaces", IndexBy: []string{"metadata.namespace"}, Namespaces: []string{"team-a"}},
	}}
	_, _, err = ResolveEffective(cfg, served.servedSet, nil)
	require.NoError(t, err)
}
