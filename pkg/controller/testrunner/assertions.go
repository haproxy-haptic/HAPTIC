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
	"slices"
	"strings"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/names"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

// RenderDependencies holds all dependencies needed to re-render templates.
// This is used by the deterministic assertion to render again and compare.
type RenderDependencies struct {
	Engine          templating.Engine
	Stores          map[string]stores.Store
	ValidationPaths *dataplane.ValidationPaths
	HTTPStore       *FixtureHTTPStoreWrapper
	CurrentConfig   *renderplan.CurrentConfig
	CurrentFiles    map[string]string
	ExtraContext    map[string]any
}

// assertDeterministic validates that rendering the template twice produces identical output.
// This catches non-deterministic template behavior (e.g., map iteration order).
func (r *Runner) assertDeterministic(
	ctx context.Context,
	assertion *config.ValidationAssertion,
	first *RenderOutput,
	deps *RenderDependencies,
) AssertionResult {
	firstConfig := first.HAProxyConfig
	firstAuxFiles := first.AuxiliaryFiles
	result := AssertionResult{
		Type:        "deterministic",
		Description: assertion.Description,
		Passed:      true,
	}

	if result.Description == "" {
		result.Description = "Template rendering must be deterministic (identical output on repeated renders)"
	}

	// If first render failed, we can't check determinism
	if firstConfig == "" && firstAuxFiles == nil {
		result.Passed = false
		result.Error = "cannot verify determinism: first render produced no output"
		return result
	}

	// The second render sees the first one's files, which is what the
	// controller does on every reconcile after the first. Without that, a
	// template that MUST mint state on first sight of an empty input — the
	// TLS session-ticket keys are the case — looks nondeterministic while
	// being exactly right: production hands it the deployed file and it
	// preserves what is already there. The property worth asserting is that
	// a render fed its own output does not churn, because churn is a
	// spurious sync and reload.
	second, err := r.renderWithStores(
		ctx,
		deps.Engine,
		deps.Stores,
		deps.ValidationPaths,
		deps.HTTPStore,
		deps.CurrentConfig,
		mergeRenderedFiles(deps.CurrentFiles, firstAuxFiles),
		deps.ExtraContext,
	)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
			result.incomplete = true
			return result
		}
		result.Passed = false
		result.Error = fmt.Sprintf("second render failed: %v", templating.RenderErrorMessage(err))
		return result
	}

	// Compare main HAProxy config
	if firstConfig != second.HAProxyConfig {
		result.Passed = false
		diff := generateUnifiedDiff(names.MainTemplateName+" (render 1)", names.MainTemplateName+" (render 2)", firstConfig, second.HAProxyConfig)
		result.Error = fmt.Sprintf("%s differs between renders:\n%s", names.MainTemplateName, diff)
		r.populateTargetMetadata(&result, firstConfig, names.MainTemplateName, true)
		return result
	}

	if diffResult := compareAuxiliaryFiles(firstAuxFiles, second.AuxiliaryFiles); diffResult != "" {
		result.Passed = false
		result.Error = diffResult
		return result
	}

	// Status patches are deliberately not compared: their lastTransitionTime
	// is stamped from the clock, so two renders legitimately differ.
	if diffResult := compareRenderedK8sResources(first.K8sResources, second.K8sResources); diffResult != "" {
		result.Passed = false
		result.Error = diffResult
		return result
	}
	if first.Events != second.Events {
		result.Passed = false
		result.Error = fmt.Sprintf("events differ between renders:\n%s",
			generateUnifiedDiff("events (render 1)", "events (render 2)", first.Events, second.Events))
		return result
	}

	return result
}

// compareRenderedK8sResources reports the first k8sResource whose rendered text
// differs between two renders, or one present in only one of them.
func compareRenderedK8sResources(first, second map[string]string) string {
	rendered := make([]string, 0, len(first)+len(second))
	for name := range first {
		rendered = append(rendered, name)
	}
	for name := range second {
		if _, exists := first[name]; !exists {
			rendered = append(rendered, name)
		}
	}
	slices.Sort(rendered)
	for _, name := range rendered {
		before, hadBefore := first[name]
		after, hadAfter := second[name]
		switch {
		case hadBefore && !hadAfter:
			return fmt.Sprintf("k8sResource %s is rendered once and then not at all", name)
		case !hadBefore && hadAfter:
			return fmt.Sprintf("k8sResource %s appears only on the second render", name)
		case before != after:
			return fmt.Sprintf("k8sResource %s differs between renders:\n%s", name,
				generateUnifiedDiff(name+" (render 1)", name+" (render 2)", before, after))
		}
	}
	return ""
}

// mergeRenderedFiles overlays a render's own general files onto the test's
// declared currentFiles, keyed the way templates read them back
// (currentFiles["tls-ticket-keys"]). The test's own entries lose to the
// rendered ones: a test that declares a file is describing what was deployed
// BEFORE this render, and the second render's "before" is the first render's
// output.
func mergeRenderedFiles(declared map[string]string, rendered *dataplane.AuxiliaryFiles) map[string]string {
	merged := make(map[string]string, len(declared))
	for k, v := range declared {
		merged[k] = v
	}
	if rendered != nil {
		for _, f := range rendered.GeneralFiles {
			merged[f.Filename] = f.Content
		}
	}
	return merged
}

// generateUnifiedDiff generates a line-by-line diff between two strings.
// Identical lines are prefixed with a space, removed lines with "-", and
// added lines with "+", under "--- fromName" / "+++ toName" headers. It is
// not a minimal-edit (LCS) diff — it compares lines positionally, which is
// sufficient for the deterministic-render check, where the two inputs are
// the same template rendered twice and any divergence is a bug to surface.
func generateUnifiedDiff(fromName, toName, from, to string) string {
	if from == to {
		return "(no visible diff - whitespace or newline difference)"
	}

	fromLines := strings.Split(from, "\n")
	toLines := strings.Split(to, "\n")

	var b strings.Builder
	fmt.Fprintf(&b, "--- %s\n", fromName)
	fmt.Fprintf(&b, "+++ %s\n", toName)

	maxLen := len(fromLines)
	if len(toLines) > maxLen {
		maxLen = len(toLines)
	}
	for i := 0; i < maxLen; i++ {
		var fromLine, toLine string
		hasFrom := i < len(fromLines)
		hasTo := i < len(toLines)
		if hasFrom {
			fromLine = fromLines[i]
		}
		if hasTo {
			toLine = toLines[i]
		}

		switch {
		case hasFrom && hasTo && fromLine == toLine:
			fmt.Fprintf(&b, " %s\n", fromLine)
		default:
			if hasFrom {
				fmt.Fprintf(&b, "-%s\n", fromLine)
			}
			if hasTo {
				fmt.Fprintf(&b, "+%s\n", toLine)
			}
		}
	}

	return b.String()
}

// compareAuxiliaryFiles compares two sets of auxiliary files and returns a diff description if they differ.
func compareAuxiliaryFiles(first, second *dataplane.AuxiliaryFiles) string {
	if first == nil && second == nil {
		return ""
	}
	if first == nil || second == nil {
		return "auxiliary files: one render produced files, the other did not"
	}

	if diff := compareFileList("map files", extractFileMap(first.MapFiles), extractFileMap(second.MapFiles)); diff != "" {
		return diff
	}
	if diff := compareFileList("general files", extractFileMap(first.GeneralFiles), extractFileMap(second.GeneralFiles)); diff != "" {
		return diff
	}
	if diff := compareFileList("SSL certificates", extractFileMap(first.SSLCertificates), extractFileMap(second.SSLCertificates)); diff != "" {
		return diff
	}
	if diff := compareFileList("crt-list files", extractFileMap(first.CRTListFiles), extractFileMap(second.CRTListFiles)); diff != "" {
		return diff
	}

	return ""
}

// compareFileList compares two maps of filename to content and returns a diff if they differ.
func compareFileList(fileType string, first, second map[string]string) string {
	// Check for files only in first
	for name := range first {
		if _, ok := second[name]; !ok {
			return fmt.Sprintf("%s: file %q exists in first render but not in second", fileType, name)
		}
	}

	// Check for files only in second
	for name := range second {
		if _, ok := first[name]; !ok {
			return fmt.Sprintf("%s: file %q exists in second render but not in first", fileType, name)
		}
	}

	// Compare content of each file
	for name, content1 := range first {
		content2 := second[name]
		if content1 != content2 {
			diff := generateUnifiedDiff(
				fmt.Sprintf("%s (render 1)", name),
				fmt.Sprintf("%s (render 2)", name),
				content1,
				content2,
			)
			return fmt.Sprintf("%s: %s differs between renders:\n%s", fileType, name, diff)
		}
	}

	return ""
}

// extractFileMap converts a slice of FileItem to an identifier→content map for
// comparison. Works for any auxiliary file type since they all implement
// FileItem (GetIdentifier returns Filename for general files and Path for the
// rest).
func extractFileMap[T auxiliaryfiles.FileItem](files []T) map[string]string {
	result := make(map[string]string, len(files))
	for _, f := range files {
		result[f.GetIdentifier()] = f.GetContent()
	}
	return result
}

// executeAssertions runs all assertions for a test and updates the result.
func (r *Runner) executeAssertions(
	ctx context.Context,
	result *TestResult,
	test *config.ValidationTest,
	haproxyConfig string,
	auxiliaryFiles *dataplane.AuxiliaryFiles,
	k8sResources map[string]string,
	statusPatches map[string]string,
	renderedEvents string,
	templateContext map[string]any,
	validationPaths *dataplane.ValidationPaths,
	renderDeps *RenderDependencies,
) bool {
	for i := range test.Assertions {
		if ctx.Err() != nil {
			return true
		}
		assertionResult := r.runAssertion(ctx, &test.Assertions[i], haproxyConfig, auxiliaryFiles, k8sResources, statusPatches, renderedEvents, templateContext, result.RenderError, validationPaths, renderDeps)
		if appendAssertionResult(ctx, result, &assertionResult) {
			return true
		}
	}

	if determinism := r.checkDeterminism(
		ctx, test, haproxyConfig, auxiliaryFiles, renderedEvents, result, renderDeps,
	); determinism != nil {
		if appendAssertionResult(ctx, result, determinism) {
			return true
		}
	}
	if fused := checkFusedDirectives(test, haproxyConfig, result); fused != nil {
		if appendAssertionResult(ctx, result, fused) {
			return true
		}
	}
	return false
}

// fusedDirectiveNames are directives distinctive enough that finding one glued
// to the end of a comment is a rendering defect, not prose. Short, common words
// (acl, bind, server) are deliberately absent: they appear inside comment text.
var fusedDirectiveNames = []string{
	"http-request ",
	"http-response ",
	"http-after-response ",
	"tcp-request ",
	"tcp-response ",
	"use_backend ",
	"default_backend ",
	"use-server ",
}

// checkFusedDirectives fails a test whose config has a directive swallowed by a
// comment. A template that strips the newline between a section-marker comment
// and the directive behind it renders valid config that silently does nothing --
// `haproxy -c` accepts it, and a `contains` assertion still matches the fused
// line. Every such site so far cost a whole feature: Gateway routes answering
// 404, fixed-response and consumer-group denial never applying.
func checkFusedDirectives(test *config.ValidationTest, haproxyConfig string, result *TestResult) *AssertionResult {
	if result.RenderError != "" || hasRenderingErrorAssertions(test.Assertions) {
		return nil
	}
	check := AssertionResult{
		Type:        "no_fused_directive",
		Description: "No HAProxy directive may be swallowed by a comment",
		Passed:      true,
	}
	for line := range strings.SplitSeq(haproxyConfig, "\n") {
		if !strings.HasPrefix(strings.TrimLeft(line, " \t"), "#") {
			continue
		}
		for _, directive := range fusedDirectiveNames {
			at := strings.Index(line, directive)
			if at <= 0 || line[at-1] == ' ' || line[at-1] == '\t' {
				continue
			}
			check.Passed = false
			check.Error = fmt.Sprintf(
				"comment swallowed a %sdirective, which HAProxy then ignores: %s",
				directive, strings.TrimSpace(line))
			return &check
		}
	}
	return &check
}

func appendAssertionResult(ctx context.Context, result *TestResult, assertion *AssertionResult) bool {
	if assertion.incomplete {
		return true
	}
	result.Assertions = append(result.Assertions, *assertion)
	if !assertion.Passed {
		result.Passed = false
	}
	return ctx.Err() != nil
}

// checkDeterminism renders every test a second time and compares, so a
// template whose output depends on map-iteration order fails the suite that
// covers it rather than the one test whose author thought to ask.
//
// It was an opt-in assertion and 6 of 722 tests opted in, which is how two
// route host-map builders shipped ranging a map[string]bool unsorted: every
// assertion about them passed, because assertions match entries and the bug
// is in their order. A reordered map file is a changed file to the
// controller, so it costs a sync and a reload on a config nobody edited.
//
// Returns nil when there is nothing to compare (a render-error test, or a
// test that already declares the assertion and has just run it).
func (r *Runner) checkDeterminism(
	ctx context.Context,
	test *config.ValidationTest,
	haproxyConfig string,
	auxiliaryFiles *dataplane.AuxiliaryFiles,
	renderedEvents string,
	result *TestResult,
	renderDeps *RenderDependencies,
) *AssertionResult {
	if renderDeps == nil || result.RenderError != "" || hasRenderingErrorAssertions(test.Assertions) {
		return nil
	}
	for i := range test.Assertions {
		if test.Assertions[i].Type == "deterministic" {
			return nil
		}
	}
	if haproxyConfig == "" && auxiliaryFiles == nil {
		return nil
	}
	check := r.assertDeterministic(ctx, &config.ValidationAssertion{Type: "deterministic"}, &RenderOutput{
		HAProxyConfig:  haproxyConfig,
		AuxiliaryFiles: auxiliaryFiles,
		K8sResources:   result.RenderedK8sResources,
		Events:         renderedEvents,
	}, renderDeps)
	return &check
}

// hasRenderingErrorAssertions checks if the test has any assertions targeting rendering_error.
// This is used to determine if a test expects rendering to fail (negative test).
func hasRenderingErrorAssertions(assertions []config.ValidationAssertion) bool {
	for _, assertion := range assertions {
		if assertion.Target == "rendering_error" {
			return true
		}
	}
	return false
}

// runAssertion executes a single assertion.
// SupportedAssertionTypes is every assertion type runAssertion dispatches. The
// CRD's ValidationAssertion.Type enum must list exactly these: a type the enum
// omits makes the apiserver refuse the whole library object at apply time,
// which no offline gate sees.
var SupportedAssertionTypes = []string{
	"haproxy_valid",
	"contains",
	"not_contains",
	"equals",
	"jsonpath",
	"match_count",
	"match_order",
	"not_exists",
	"deterministic",
}

func (r *Runner) runAssertion(
	ctx context.Context,
	assertion *config.ValidationAssertion,
	haproxyConfig string,
	auxiliaryFiles *dataplane.AuxiliaryFiles,
	k8sResources map[string]string,
	statusPatches map[string]string,
	renderedEvents string,
	templateContext map[string]any,
	renderError string,
	validationPaths *dataplane.ValidationPaths,
	renderDeps *RenderDependencies,
) AssertionResult {
	result := AssertionResult{
		Type:        assertion.Type,
		Description: assertion.Description,
		Passed:      true,
	}

	switch assertion.Type {
	case "haproxy_valid":
		result = r.assertHAProxyValid(ctx, haproxyConfig, auxiliaryFiles, assertion, validationPaths)

	case "contains":
		result = r.assertContains(haproxyConfig, auxiliaryFiles, k8sResources, statusPatches, renderedEvents, assertion, renderError)

	case "not_contains":
		result = r.assertNotContains(haproxyConfig, auxiliaryFiles, k8sResources, statusPatches, renderedEvents, assertion, renderError)

	case "match_count":
		result = r.assertMatchCount(haproxyConfig, auxiliaryFiles, k8sResources, statusPatches, renderedEvents, assertion, renderError)

	case "equals":
		result = r.assertEquals(haproxyConfig, auxiliaryFiles, k8sResources, statusPatches, renderedEvents, assertion, renderError)

	case "jsonpath":
		result = r.assertJSONPath(templateContext, assertion)

	case "match_order":
		result = r.assertMatchOrder(haproxyConfig, auxiliaryFiles, k8sResources, statusPatches, renderedEvents, assertion, renderError)

	case "not_exists":
		result = r.assertNotExists(haproxyConfig, auxiliaryFiles, k8sResources, statusPatches, renderedEvents, assertion, renderError)

	case "deterministic":
		if renderDeps == nil {
			result.Passed = false
			result.Error = "deterministic assertion requires render dependencies (internal error)"
		} else {
			result = r.assertDeterministic(ctx, assertion, &RenderOutput{
				HAProxyConfig:  haproxyConfig,
				AuxiliaryFiles: auxiliaryFiles,
				K8sResources:   k8sResources,
				Events:         renderedEvents,
			}, renderDeps)
		}

	default:
		result.Passed = false
		result.Error = fmt.Sprintf("unknown assertion type: %s (known: %s)", assertion.Type, strings.Join(SupportedAssertionTypes, ", "))
	}

	return result
}
