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

package conversion

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/haproxy-haptic/HAPTIC/pkg/apis/haproxytemplate/v1alpha1"
)

// ValidationTestSource is one contributor of validation tests: the config's own
// inline `spec.validationTests`, or a HAProxyv1alpha1.ValidationTests object selected by
// it.
type ValidationTestSource struct {
	// Origin identifies the contributor in error messages. It is the only way an
	// operator learns which two objects collided, so it must name the object —
	// e.g. "HAProxyv1alpha1.ValidationTests/haptic-config-tests".
	Origin string

	Tests map[string]v1alpha1.ValidationTest
}

// UnionValidationTests combines every source into the single map the test
// runner consumes.
//
// A test name may appear in only one source. Silently letting the last writer
// win would leave the losing definition's author believing an assertion runs
// that does not, so a collision is an error naming both sides.
//
// The reserved `_global` entry is the exception and must be unioned rather than
// rejected: it is a shared baseline that several template libraries each
// contribute part of, so "duplicate" is its normal state. Its fixtures
// accumulate; a field that cannot accumulate (a scalar two sources both set to
// different values) is still a collision.
//
// Sources are processed in the order given, which fixes the order of
// accumulated fixtures so a render is reproducible.
func UnionValidationTests(sources []ValidationTestSource) (map[string]v1alpha1.ValidationTest, error) {
	union := make(map[string]v1alpha1.ValidationTest)
	// Which source contributed each name, for the collision message.
	origin := make(map[string]string)

	for _, src := range sources {
		for _, name := range sortedTestNames(src.Tests) {
			test := src.Tests[name]

			if name == globalValidationTestName {
				merged := union[name]
				if err := mergeGlobalBaseline(&merged, &test, origin[name], src.Origin); err != nil {
					return nil, err
				}
				union[name] = merged
				if origin[name] == "" {
					origin[name] = src.Origin
				}
				continue
			}

			if prev, dup := origin[name]; dup {
				return nil, fmt.Errorf(
					"validationTest %q is defined by both %s and %s: a test may be defined once, "+
						"otherwise one definition silently replaces the other and its assertions never run",
					name, prev, src.Origin)
			}
			origin[name] = src.Origin
			union[name] = test
		}
	}

	return union, nil
}

// mergeGlobalBaseline accumulates one source's `_global` contribution onto what
// earlier sources contributed.
func mergeGlobalBaseline(acc, add *v1alpha1.ValidationTest, accOrigin, addOrigin string) error {
	if add.UseDeploymentWatchScopes {
		return fmt.Errorf("%s: validationTests._global.useDeploymentWatchScopes cannot apply to shared fixtures. Set it on each test that checks deployment scopes", addOrigin)
	}
	if acc.Fixtures == nil && add.Fixtures != nil {
		acc.Fixtures = make(map[string][]runtime.RawExtension, len(add.Fixtures))
	}
	for _, kind := range sortedFixtureKinds(add.Fixtures) {
		acc.Fixtures[kind] = append(acc.Fixtures[kind], add.Fixtures[kind]...)
	}

	acc.HTTPResources = append(acc.HTTPResources, add.HTTPResources...)
	acc.Requires = appendUnique(acc.Requires, add.Requires)
	acc.RequiresFields = appendUnique(acc.RequiresFields, add.RequiresFields)

	if acc.Description == "" {
		acc.Description = add.Description
	}

	// Scalars and same-key map entries cannot accumulate: two different values
	// mean one baseline silently overrides the other, and every test in the
	// suite inherits whichever won.
	var err error
	if acc.MinHAProxyVersion, err = mergeScalar(acc.MinHAProxyVersion, add.MinHAProxyVersion, "minHAProxyVersion", accOrigin, addOrigin); err != nil {
		return err
	}
	if acc.CurrentFiles, err = mergeStringMap(acc.CurrentFiles, add.CurrentFiles, "currentFiles", accOrigin, addOrigin); err != nil {
		return err
	}
	if acc.ExtraContext, err = mergeRawExtension(acc.ExtraContext, add.ExtraContext, "extraContext", accOrigin, addOrigin); err != nil {
		return err
	}

	// `_global` assertions are never executed — the runner treats the entry as a
	// baseline, not a test — so they are carried for completeness only.
	acc.Assertions = append(acc.Assertions, add.Assertions...)

	return nil
}

func mergeScalar(acc, add, field, accOrigin, addOrigin string) (string, error) {
	switch {
	case add == "":
		return acc, nil
	case acc == "", acc == add:
		return add, nil
	default:
		return acc, globalConflict(field, "", accOrigin, addOrigin)
	}
}

func mergeStringMap(acc, add map[string]string, field, accOrigin, addOrigin string) (map[string]string, error) {
	if len(add) == 0 {
		return acc, nil
	}
	if acc == nil {
		acc = make(map[string]string, len(add))
	}
	for _, k := range sortedStringMapKeys(add) {
		if existing, ok := acc[k]; ok && existing != add[k] {
			return acc, globalConflict(field, k, accOrigin, addOrigin)
		}
		acc[k] = add[k]
	}
	return acc, nil
}

// mergeRawExtension merges two `_global.extraContext` documents key by key.
//
// It decodes rather than comparing bytes because the baseline is genuinely
// composed: several libraries each contribute their own keys to it, so byte
// inequality is the normal case and rejecting it would break every multi-library
// install. Only a key both sides set to different values is a conflict.
func mergeRawExtension(acc, add runtime.RawExtension, field, accOrigin, addOrigin string) (runtime.RawExtension, error) {
	if len(add.Raw) == 0 {
		return acc, nil
	}
	if len(acc.Raw) == 0 || bytes.Equal(acc.Raw, add.Raw) {
		return add, nil
	}

	var accMap, addMap map[string]any
	if err := json.Unmarshal(acc.Raw, &accMap); err != nil {
		return acc, fmt.Errorf("%s %s: %s is not a JSON object: %w", globalValidationTestName, field, accOrigin, err)
	}
	if err := json.Unmarshal(add.Raw, &addMap); err != nil {
		return acc, fmt.Errorf("%s %s: %s is not a JSON object: %w", globalValidationTestName, field, addOrigin, err)
	}

	for _, k := range sortedAnyMapKeys(addMap) {
		if existing, ok := accMap[k]; ok && fmt.Sprintf("%v", existing) != fmt.Sprintf("%v", addMap[k]) {
			return acc, globalConflict(field, k, accOrigin, addOrigin)
		}
		accMap[k] = addMap[k]
	}

	merged, err := json.Marshal(accMap)
	if err != nil {
		return acc, err
	}
	return runtime.RawExtension{Raw: merged}, nil
}

func sortedAnyMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func globalConflict(field, key, accOrigin, addOrigin string) error {
	where := field
	if key != "" {
		where = field + "." + key
	}
	if accOrigin == "" {
		accOrigin = "an earlier source"
	}
	return fmt.Errorf(
		"validationTests %s: %s is set to different values by %s and %s: "+
			"the baseline is shared by every test, so one value would silently override the other",
		globalValidationTestName, where, accOrigin, addOrigin)
}

func appendUnique(acc, add []string) []string {
	seen := make(map[string]bool, len(acc))
	for _, v := range acc {
		seen[v] = true
	}
	for _, v := range add {
		if !seen[v] {
			seen[v] = true
			acc = append(acc, v)
		}
	}
	return acc
}

// The map iteration order below is fixed so that accumulated fixtures — and
// therefore the rendered config a test asserts on — do not vary between runs.

func sortedTestNames(m map[string]v1alpha1.ValidationTest) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedFixtureKinds(m map[string][]runtime.RawExtension) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedStringMapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// globalValidationTestName mirrors coreconfig.GlobalValidationTestName. It is
// restated rather than imported because pkg/core/config must not depend on the
// API types, and the two are pinned equal by TestGlobalNameMatchesCore.
const globalValidationTestName = "_global"
