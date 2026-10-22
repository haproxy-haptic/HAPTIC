// Copyright 2025 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package conversion

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
)

// convertValidationTests is the per-test converter that ConvertSpec
// funnels validation_tests through. It composes the four already-
// covered helpers (convertFixtures, convertHTTPFixtures,
// convertAssertions, plus the optional ExtraContext JSON parse) into
// a single map keyed by test name.
//
// Pin every contract:
//   - empty input yields an empty (non-nil) map (callers iterate)
//   - simple fields (Description, MinHAProxyVersion)
//     are copied verbatim
//   - nested fixtures / HTTP fixtures / assertions flow through to
//     the per-helper converters
//   - the optional ExtraContext JSON is parsed when present, left
//     unset when absent, and surfaces an error with the test name
//     when malformed
func TestConvertValidationTests(t *testing.T) {
	t.Run("nil/empty input yields empty map", func(t *testing.T) {
		got, err := convertValidationTests(nil)
		require.NoError(t, err)
		assert.NotNil(t, got)
		assert.Empty(t, got)

		got, err = convertValidationTests(map[string]v1alpha1.ValidationTest{})
		require.NoError(t, err)
		assert.NotNil(t, got)
		assert.Empty(t, got)
	})

	t.Run("simple fields are copied verbatim", func(t *testing.T) {
		in := map[string]v1alpha1.ValidationTest{
			"basic": {
				Description:       "smoke test",
				MinHAProxyVersion: "3.0",
				Requires:          []string{"httproutes"},
				RequiresFields:    []string{"httproutes.spec.rules.filters.cors"},
				Fixtures:          map[string][]runtime.RawExtension{},
			},
		}
		got, err := convertValidationTests(in)
		require.NoError(t, err)
		require.Contains(t, got, "basic")

		test := got["basic"]
		assert.Equal(t, "smoke test", test.Description)
		assert.Equal(t, "3.0", test.MinHAProxyVersion)
		assert.Equal(t, []string{"httproutes"}, test.Requires)
		assert.Equal(t, []string{"httproutes.spec.rules.filters.cors"}, test.RequiresFields)
		assert.Nil(t, test.ExtraContext, "ExtraContext must remain unset when absent in input")
	})

	t.Run("multiple tests are independently converted", func(t *testing.T) {
		in := map[string]v1alpha1.ValidationTest{
			"alpha": {Description: "first", Fixtures: map[string][]runtime.RawExtension{}},
			"beta":  {Description: "second", Fixtures: map[string][]runtime.RawExtension{}},
			"gamma": {Description: "third", Fixtures: map[string][]runtime.RawExtension{}},
		}
		got, err := convertValidationTests(in)
		require.NoError(t, err)
		assert.Len(t, got, 3)
		assert.Equal(t, "first", got["alpha"].Description)
		assert.Equal(t, "second", got["beta"].Description)
		assert.Equal(t, "third", got["gamma"].Description)
	})

	t.Run("ExtraContext is parsed when valid JSON", func(t *testing.T) {
		in := map[string]v1alpha1.ValidationTest{
			"with_ctx": {
				Description: "uses extra context",
				Fixtures:    map[string][]runtime.RawExtension{},
				ExtraContext: runtime.RawExtension{
					Raw: []byte(`{"region": "us-east", "tier": "premium"}`),
				},
			},
		}
		got, err := convertValidationTests(in)
		require.NoError(t, err)
		require.Contains(t, got, "with_ctx")

		ctx := got["with_ctx"].ExtraContext
		require.NotNil(t, ctx, "ExtraContext must be populated when JSON is present")
		assert.Equal(t, "us-east", ctx["region"])
		assert.Equal(t, "premium", ctx["tier"])
	})

	t.Run("malformed ExtraContext surfaces an error with the test name", func(t *testing.T) {
		in := map[string]v1alpha1.ValidationTest{
			"broken": {
				Description: "broken extra context",
				Fixtures:    map[string][]runtime.RawExtension{},
				ExtraContext: runtime.RawExtension{
					Raw: []byte(`{this is not valid JSON`),
				},
			},
		}
		_, err := convertValidationTests(in)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "broken",
			"error must mention the test name so users can locate the malformed input")
		assert.Contains(t, err.Error(), "extra_context",
			"error must mention the field name so users know what's wrong")
	})

	t.Run("HTTPResources flow through to convertHTTPFixtures", func(t *testing.T) {
		in := map[string]v1alpha1.ValidationTest{
			"with_http": {
				Description: "http fixtures",
				Fixtures:    map[string][]runtime.RawExtension{},
				HTTPResources: []v1alpha1.HTTPResourceFixture{
					{URL: "http://example.com/list.txt", Content: "blocked-1\nblocked-2"},
				},
			},
		}
		got, err := convertValidationTests(in)
		require.NoError(t, err)
		require.Contains(t, got, "with_http")

		http := got["with_http"].HTTPFixtures
		require.Len(t, http, 1)
		assert.Equal(t, "http://example.com/list.txt", http[0].URL)
		assert.Equal(t, "blocked-1\nblocked-2", http[0].Content)
	})
}

func TestConvertValidationTestDeploymentWatchScopes(t *testing.T) {
	tests, err := convertValidationTests(map[string]v1alpha1.ValidationTest{
		"scoped":   {UseDeploymentWatchScopes: true},
		"isolated": {},
	})
	require.NoError(t, err)
	assert.True(t, tests["scoped"].UseDeploymentWatchScopes)
	assert.False(t, tests["isolated"].UseDeploymentWatchScopes)
}

func TestUnionRejectsGlobalDeploymentWatchScopes(t *testing.T) {
	_, err := UnionValidationTests([]ValidationTestSource{{Origin: "operator", Tests: map[string]v1alpha1.ValidationTest{"_global": {UseDeploymentWatchScopes: true}}}})
	require.ErrorContains(t, err, "validationTests._global.useDeploymentWatchScopes")
	assert.Contains(t, err.Error(), "Set it on each test")
}
