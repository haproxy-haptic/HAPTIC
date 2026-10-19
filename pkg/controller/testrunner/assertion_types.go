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
	"errors"
	"fmt"
	"regexp"

	"k8s.io/client-go/util/jsonpath"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/names"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
)

// assertHAProxyValid validates that the HAProxy configuration is syntactically valid.
//
// This assertion uses the HAProxy binary to validate the configuration.
func (r *Runner) assertHAProxyValid(
	ctx context.Context,
	haproxyConfig string,
	auxiliaryFiles *dataplane.AuxiliaryFiles,
	assertion *config.ValidationAssertion,
	validationPaths *dataplane.ValidationPaths,
) AssertionResult {
	result := AssertionResult{
		Type:        "haproxy_valid",
		Description: assertion.Description,
		Passed:      true,
	}

	if result.Description == "" {
		result.Description = "HAProxy configuration must be syntactically valid"
	}

	// Browser/WASM: no haproxy binary. The caller's stand-in check answers
	// instead, and labels its own results — it is weaker than `haproxy -c`.
	if r.checkWithoutBinary != nil {
		if err := r.checkWithoutBinary(haproxyConfig); err != nil {
			result.Passed = false
			result.Error = fmt.Sprintf("HAProxy validation failed (config size: %d bytes): %s",
				len(haproxyConfig), dataplane.SimplifyValidationError(err))
		}
		r.populateTargetMetadata(&result, haproxyConfig, names.MainTemplateName, !result.Passed)
		return result
	}

	// Strict validation (skipDNSValidation=false): a validation test runs on
	// operator input, where a name that does not resolve is worth reporting.
	err := dataplane.ValidateConfigurationContext(ctx, haproxyConfig, auxiliaryFiles, validationPaths, false, r.checkGate)
	if cause := context.Cause(ctx); cause != nil && errors.Is(err, cause) {
		result.incomplete = true
		return result
	}
	failed := err != nil
	if failed {
		result.Passed = false
		result.ConfigRejected = errors.Is(err, dataplane.ErrHAProxyRefused)
		simplifiedError := dataplane.SimplifyValidationError(err)
		result.Error = fmt.Sprintf("HAProxy validation failed (config size: %d bytes): %s", len(haproxyConfig), simplifiedError)
	}

	// Populate target metadata (target is haproxy.cfg for this assertion)
	r.populateTargetMetadata(&result, haproxyConfig, names.MainTemplateName, failed)

	return result
}

// assertContains validates that the target content contains the specified pattern.
func (r *Runner) assertContains(
	haproxyConfig string,
	auxiliaryFiles *dataplane.AuxiliaryFiles,
	k8sResources map[string]string,
	statusPatches map[string]string,
	renderedEvents string,
	assertion *config.ValidationAssertion,
	renderError string,
) AssertionResult {
	result := AssertionResult{
		Type:        "contains",
		Description: assertion.Description,
		Passed:      true,
	}

	if result.Description == "" {
		result.Description = fmt.Sprintf("Target %s must contain pattern %q", assertion.Target, assertion.Pattern)
	}

	// Resolve target to actual content
	target, targetErr := r.resolveTarget(assertion.Target, haproxyConfig, auxiliaryFiles, k8sResources, statusPatches, renderedEvents, renderError)
	if targetErr != nil {
		result.Passed = false
		result.Error = targetErr.Error()
		r.populateTargetMetadata(&result, target, assertion.Target, true)
		return result
	}

	// Check if pattern matches
	matched, err := regexp.MatchString(assertion.Pattern, target)
	if err != nil {
		result.Passed = false
		result.Error = fmt.Sprintf("invalid regex pattern %q: %v", assertion.Pattern, err)
		r.populateTargetMetadata(&result, target, assertion.Target, true)
		return result
	}

	if !matched {
		result.Passed = false
		result.Error = fmt.Sprintf("pattern %q not found in %s (target size: %d bytes). Hint: Use --verbose to see content preview",
			assertion.Pattern, assertion.Target, len(target))
	}

	// Populate target metadata for observability
	r.populateTargetMetadata(&result, target, assertion.Target, !matched)

	return result
}

// assertNotContains validates that the target content does NOT contain the specified pattern.
func (r *Runner) assertNotContains(
	haproxyConfig string,
	auxiliaryFiles *dataplane.AuxiliaryFiles,
	k8sResources map[string]string,
	statusPatches map[string]string,
	renderedEvents string,
	assertion *config.ValidationAssertion,
	renderError string,
) AssertionResult {
	result := AssertionResult{
		Type:        "not_contains",
		Description: assertion.Description,
		Passed:      true,
	}

	if result.Description == "" {
		result.Description = fmt.Sprintf("Target %s must not contain pattern %q", assertion.Target, assertion.Pattern)
	}

	// Resolve target to actual content
	target, targetErr := r.resolveTarget(assertion.Target, haproxyConfig, auxiliaryFiles, k8sResources, statusPatches, renderedEvents, renderError)
	if targetErr != nil {
		result.Passed = false
		result.Error = targetErr.Error()
		r.populateTargetMetadata(&result, target, assertion.Target, true)
		return result
	}

	// Check if pattern matches
	matched, err := regexp.MatchString(assertion.Pattern, target)
	if err != nil {
		result.Passed = false
		result.Error = fmt.Sprintf("invalid regex pattern %q: %v", assertion.Pattern, err)
		r.populateTargetMetadata(&result, target, assertion.Target, true)
		return result
	}

	if matched {
		result.Passed = false
		result.Error = fmt.Sprintf("pattern %q unexpectedly found in %s (target size: %d bytes). Hint: Use --verbose to see content preview",
			assertion.Pattern, assertion.Target, len(target))
	}

	// Populate target metadata for observability
	r.populateTargetMetadata(&result, target, assertion.Target, matched)

	return result
}

// assertNotExists checks that the render produced no artefact under this target.
//
// The counterpart of the strict resolution in resolveTarget: "the file is
// absent" is a property worth pinning (a listener whose CA is unresolvable must
// emit no crt-list at all), and a `not_contains` cannot express it without
// relying on a missing target reading as empty.
func (r *Runner) assertNotExists(
	haproxyConfig string,
	auxiliaryFiles *dataplane.AuxiliaryFiles,
	k8sResources, statusPatches map[string]string,
	renderedEvents string,
	assertion *config.ValidationAssertion,
	renderError string,
) AssertionResult {
	result := AssertionResult{
		Type:        "not_exists",
		Description: assertion.Description,
		Passed:      true,
	}

	if result.Description == "" {
		result.Description = fmt.Sprintf("Target %s must not be produced", assertion.Target)
	}

	target, targetErr := r.resolveTarget(assertion.Target, haproxyConfig, auxiliaryFiles, k8sResources, statusPatches, renderedEvents, renderError)
	if targetErr != nil {
		r.populateTargetMetadata(&result, "", assertion.Target, false)
		return result
	}

	result.Passed = false
	result.Error = fmt.Sprintf("target %s was produced by this render (%d bytes)", assertion.Target, len(target))
	r.populateTargetMetadata(&result, target, assertion.Target, true)
	return result
}

// assertMatchCount validates that the target content contains exactly the expected number of pattern matches.
func (r *Runner) assertMatchCount(
	haproxyConfig string,
	auxiliaryFiles *dataplane.AuxiliaryFiles,
	k8sResources map[string]string,
	statusPatches map[string]string,
	renderedEvents string,
	assertion *config.ValidationAssertion,
	renderError string,
) AssertionResult {
	result := AssertionResult{
		Type:        "match_count",
		Description: assertion.Description,
		Passed:      true,
	}

	if result.Description == "" {
		result.Description = fmt.Sprintf("Target %s must contain exactly %s matches of pattern %q", assertion.Target, assertion.Expected, assertion.Pattern)
	}

	// Resolve target to actual content
	target, targetErr := r.resolveTarget(assertion.Target, haproxyConfig, auxiliaryFiles, k8sResources, statusPatches, renderedEvents, renderError)
	if targetErr != nil {
		result.Passed = false
		result.Error = targetErr.Error()
		r.populateTargetMetadata(&result, target, assertion.Target, true)
		return result
	}

	// Compile regex pattern
	re, err := regexp.Compile(assertion.Pattern)
	if err != nil {
		result.Passed = false
		result.Error = fmt.Sprintf("invalid regex pattern %q: %v", assertion.Pattern, err)
		r.populateTargetMetadata(&result, target, assertion.Target, true)
		return result
	}

	matches := re.FindAllString(target, -1)
	actualCount := len(matches)

	// Parse expected count from string
	var expectedCount int
	_, err = fmt.Sscanf(assertion.Expected, "%d", &expectedCount)
	if err != nil {
		result.Passed = false
		result.Error = fmt.Sprintf("invalid expected count %q: must be an integer", assertion.Expected)
		r.populateTargetMetadata(&result, target, assertion.Target, true)
		return result
	}

	// Compare counts
	if actualCount != expectedCount {
		result.Passed = false
		result.Error = fmt.Sprintf("expected %d matches, got %d matches of pattern %q in %s (target size: %d bytes). Hint: Use --verbose to see content preview",
			expectedCount, actualCount, assertion.Pattern, assertion.Target, len(target))
	}

	// Populate target metadata for observability
	r.populateTargetMetadata(&result, target, assertion.Target, actualCount != expectedCount)

	return result
}

// assertEquals validates that the target content exactly equals the expected value.
func (r *Runner) assertEquals(
	haproxyConfig string,
	auxiliaryFiles *dataplane.AuxiliaryFiles,
	k8sResources map[string]string,
	statusPatches map[string]string,
	renderedEvents string,
	assertion *config.ValidationAssertion,
	renderError string,
) AssertionResult {
	result := AssertionResult{
		Type:        "equals",
		Description: assertion.Description,
		Passed:      true,
	}

	if result.Description == "" {
		result.Description = fmt.Sprintf("Target %s must equal expected value", assertion.Target)
	}

	// Resolve target to actual content
	target, targetErr := r.resolveTarget(assertion.Target, haproxyConfig, auxiliaryFiles, k8sResources, statusPatches, renderedEvents, renderError)
	if targetErr != nil {
		result.Passed = false
		result.Error = targetErr.Error()
		r.populateTargetMetadata(&result, target, assertion.Target, true)
		return result
	}

	// Compare values
	failed := target != assertion.Expected
	if failed {
		result.Passed = false
		// Truncate long values for error message
		targetPreview := truncateString(target, 100)
		expectedPreview := truncateString(assertion.Expected, 100)

		// Add hint for long values
		if len(target) > 100 || len(assertion.Expected) > 100 {
			result.Error = fmt.Sprintf("expected %q, got %q. Hint: Use --verbose for full preview", expectedPreview, targetPreview)
		} else {
			result.Error = fmt.Sprintf("expected %q, got %q", expectedPreview, targetPreview)
		}
	}

	// Populate target metadata for observability
	r.populateTargetMetadata(&result, target, assertion.Target, failed)

	return result
}

// assertJSONPath evaluates a JSONPath expression against the template context.
func (r *Runner) assertJSONPath(
	templateContext map[string]any,
	assertion *config.ValidationAssertion,
) AssertionResult {
	result := AssertionResult{
		Type:        "jsonpath",
		Description: assertion.Description,
		Passed:      true,
	}

	if result.Description == "" {
		result.Description = fmt.Sprintf("JSONPath %s must match expected value", assertion.JSONPath)
	}

	// Parse JSONPath expression
	jp := jsonpath.New("assertion")
	if err := jp.Parse(assertion.JSONPath); err != nil {
		result.Passed = false
		result.Error = fmt.Sprintf("invalid JSONPath expression %q: %v", assertion.JSONPath, err)
		return result
	}

	// Execute JSONPath against template context
	results, err := jp.FindResults(templateContext)
	if err != nil {
		result.Passed = false
		result.Error = fmt.Sprintf("JSONPath execution failed: %v", err)
		return result
	}

	// Check if we got results
	if len(results) == 0 || len(results[0]) == 0 {
		result.Passed = false
		result.Error = "JSONPath expression returned no results"
		return result
	}

	// If Expected is provided, check against it
	actualValue := fmt.Sprint(results[0][0].Interface())
	failed := false
	if assertion.Expected != "" {
		if actualValue != assertion.Expected {
			result.Passed = false
			result.Error = fmt.Sprintf("expected %q, got %q", assertion.Expected, actualValue)
			failed = true
		}
	}

	// Populate target metadata (target is the JSONPath query result)
	result.Target = assertion.JSONPath
	result.TargetSize = len(actualValue)
	if failed {
		result.TargetPreview = truncateString(actualValue, 200)
	}

	return result
}

// assertMatchOrder validates that patterns appear in the specified order in the target content.
// This matters whenever ordering encodes precedence — more specific match rules must be emitted
// before less specific ones (first match wins in HAProxy).
func (r *Runner) assertMatchOrder(
	haproxyConfig string,
	auxiliaryFiles *dataplane.AuxiliaryFiles,
	k8sResources map[string]string,
	statusPatches map[string]string,
	renderedEvents string,
	assertion *config.ValidationAssertion,
	renderError string,
) AssertionResult {
	result := AssertionResult{
		Type:        "match_order",
		Description: assertion.Description,
		Passed:      true,
	}

	if result.Description == "" {
		result.Description = fmt.Sprintf("Patterns in target %s must appear in specified order", assertion.Target)
	}

	// Resolve target to actual content
	target, targetErr := r.resolveTarget(assertion.Target, haproxyConfig, auxiliaryFiles, k8sResources, statusPatches, renderedEvents, renderError)
	if targetErr != nil {
		result.Passed = false
		result.Error = targetErr.Error()
		r.populateTargetMetadata(&result, target, assertion.Target, true)
		return result
	}

	// Check that we have patterns to match
	if len(assertion.Patterns) == 0 {
		result.Passed = false
		result.Error = "no patterns specified for match_order assertion"
		r.populateTargetMetadata(&result, target, assertion.Target, true)
		return result
	}

	// Find the position of each pattern's first match
	type patternMatch struct {
		pattern string
		index   int
		found   bool
	}
	matches := make([]patternMatch, len(assertion.Patterns))

	for i, pattern := range assertion.Patterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			result.Passed = false
			result.Error = fmt.Sprintf("invalid regex pattern %q: %v", pattern, err)
			r.populateTargetMetadata(&result, target, assertion.Target, true)
			return result
		}

		loc := re.FindStringIndex(target)
		if loc == nil {
			matches[i] = patternMatch{
				pattern: pattern,
				index:   -1,
				found:   false,
			}
		} else {
			matches[i] = patternMatch{
				pattern: pattern,
				index:   loc[0],
				found:   true,
			}
		}
	}

	// Check if all patterns were found
	for i, match := range matches {
		if !match.found {
			result.Passed = false
			result.Error = fmt.Sprintf("pattern %d %q not found in %s (target size: %d bytes). Hint: Use --verbose to see content preview",
				i+1, match.pattern, assertion.Target, len(target))
			r.populateTargetMetadata(&result, target, assertion.Target, true)
			return result
		}
	}

	// Verify patterns appear in order
	for i := 1; i < len(matches); i++ {
		if matches[i].index < matches[i-1].index {
			result.Passed = false
			result.Error = fmt.Sprintf("patterns out of order: pattern %d %q (at position %d) appears before pattern %d %q (at position %d) in %s. Expected order: 1→2→3...",
				i+1, matches[i].pattern, matches[i].index,
				i, matches[i-1].pattern, matches[i-1].index,
				assertion.Target)
			r.populateTargetMetadata(&result, target, assertion.Target, true)
			return result
		}
	}

	// All patterns found in correct order
	r.populateTargetMetadata(&result, target, assertion.Target, false)
	return result
}
