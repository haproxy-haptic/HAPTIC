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

package renderer

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
)

const haproxyIngressUniqueBaselineEnv = "HAPTIC_HAPROXY_INGRESS_UNIQUE_BASELINE"

var haproxyIngressUniqueOnlyComponents = []string{
	"features-912-haproxy-ingress-host-alias-claims",
	"map-path-regex-600-haproxy-ingress",
	"map-path-exact-600-haproxy-ingress",
	"map-path-prefix-600-haproxy-ingress",
	"map-pfxexact-600-haproxy-ingress",
	"map-host-650-haproxy-ingress-alias",
	"map-hostaliasregex-650-haproxy-ingress-alias",
	"frontend-filters-690-haproxy-ingress-mtls-error",
}

func TestHAProxyIngressUniqueOnlyComponentsReuseExactResourceFragments(t *testing.T) {
	snippets := loadHAProxyIngressUniqueOnlySnippets(t, haproxyIngressUniqueOnlyComponents)
	fixture := newHAProxyTechFixtureWithSnippets(
		t, haproxyIngressUniqueOnlyRoot(haproxyIngressUniqueOnlyComponents, false), snippets)
	fixture.addIngress(t, haproxyIngressUniquePathIngress("exact-a", "exact", "exact-a.example.test", "/exact-a", "v1"))
	fixture.addIngress(t, haproxyIngressUniquePathIngress("exact-z", "exact", "exact-z.example.test", "/exact-z", "v1"))
	fixture.addIngress(t, haproxyIngressUniquePathIngress("prefix", "prefix", "prefix.example.test", "/prefix", "v1"))
	fixture.addIngress(t, haproxyIngressUniquePathIngress("begin", "begin", "begin.example.test", "/begin", "v1"))
	fixture.addIngress(t, haproxyIngressUniquePathIngress("regex", "regex", "regex.example.test", "^/items/[0-9]+$", "v1"))
	fixture.addIngress(t, haproxyIngressUniqueIngress("alias", map[string]any{
		"haproxy-ingress.github.io/server-alias": "alias-a.example.test, alias-b.example.test",
	}, "primary.example.test", "v1"))
	fixture.addIngress(t, haproxyIngressUniqueIngress("alias-regex", map[string]any{
		"haproxy-ingress.github.io/server-alias-regex": `^regex-[a-z]+\.example\.test$`,
	}, "regex-primary.example.test", "v1"))
	fixture.addIngress(t, haproxyIngressUniqueIngress("mtls", map[string]any{
		"haproxy-ingress.github.io/auth-tls-cert-header": "true",
	}, "mtls.example.test", "v1"))
	fixture.addIngress(t, haproxyIngressUniqueIngress("inactive", nil, "inactive.example.test", "v1"))

	first := fixture.renderAndCommit(t)
	assert.Equal(t, 1, strings.Count(first.HAProxyConfig, "# haproxy-ingress/map-path-regex-haproxy-ingress"))
	assert.Equal(t, 1, strings.Count(first.HAProxyConfig, "# haproxy-ingress/map-path-exact-haproxy-ingress"))
	assert.Equal(t, 1, strings.Count(first.HAProxyConfig, "# haproxy-ingress/map-path-prefix-haproxy-ingress"))
	assert.Equal(t, 1, strings.Count(first.HAProxyConfig, "# haproxy-ingress/map-pfxexact-haproxy-ingress (begin)"))
	assert.Contains(t, first.HAProxyConfig, "\nexact-a.example.test/exact-a BACKEND:default_exact-a_svc_echo_80")
	assert.Contains(t, first.HAProxyConfig, "\nexact-z.example.test/exact-z BACKEND:default_exact-z_svc_echo_80")
	assert.Contains(t, first.HAProxyConfig, "\nprefix.example.test/prefix/ BACKEND:default_prefix_svc_echo_80")
	assert.Contains(t, first.HAProxyConfig, "\nbegin.example.test/begin BACKEND:default_begin_svc_echo_80")
	assert.Contains(t, first.HAProxyConfig, "\n# Ingress: default/regex (1 regex paths)\n^regex\\.example\\.test(?:/items/[0-9]+$) BACKEND:default_regex_svc_echo_80")
	assert.Contains(t, first.HAProxyConfig, "\n# Ingress: default/alias server-alias\nalias-a.example.test primary.example.test\nalias-b.example.test primary.example.test")
	assert.Contains(t, first.HAProxyConfig, "\n# Ingress: default/alias-regex server-alias-regex\n^(?:^regex-[a-z]+\\.example\\.test$)$ regex-primary.example.test")
	assert.Contains(t, first.HAProxyConfig, "\n# haproxy-ingress/auth-tls-cert-header (default/mtls)\nhttp-request set-header X-SSL-Client-CN %[ssl_c_s_dn(CN)] if { ssl_c_used } { var(txn.resource_key) -m str default/mtls }\nhttp-request set-header X-SSL-Client-DN %[ssl_c_s_dn] if { ssl_c_used } { var(txn.resource_key) -m str default/mtls }\nhttp-request set-header X-SSL-Client-Cert %[ssl_c_der,base64] if { ssl_c_used } { var(txn.resource_key) -m str default/mtls }")
	assert.Equal(t, map[string]int{
		"ingresses/alias":       2,
		"ingresses/alias-regex": 2,
		"ingresses/begin":       4,
		"ingresses/exact-a":     4,
		"ingresses/exact-z":     4,
		"ingresses/mtls":        1,
		"ingresses/prefix":      4,
		"ingresses/regex":       4,
	}, fixture.engine.executionCounts())

	assert.Equal(t, first.HAProxyConfig, fixture.renderAndCommit(t).HAProxyConfig)
	counts := fixture.engine.executionCounts()
	fixture.updateIngress(t, haproxyIngressUniqueIngress("inactive", nil, "inactive-v2.example.test", "v2"))
	assert.Equal(t, first.HAProxyConfig, fixture.renderAndCommit(t).HAProxyConfig)
	assert.Equal(t, counts, fixture.engine.executionCounts())

	fixture.updateIngress(t, haproxyIngressUniquePathIngress("exact-z", "exact", "exact-z.example.test", "/exact-z-v2", "v2"))
	changed := fixture.renderAndCommit(t)
	assert.NotContains(t, changed.HAProxyConfig, "exact-z.example.test/exact-z BACKEND:")
	assert.Contains(t, changed.HAProxyConfig, "\nexact-z.example.test/exact-z-v2 BACKEND:default_exact-z_svc_echo_80")
	assert.Equal(t, counts["ingresses/exact-z"]+4, fixture.engine.executionCounts()["ingresses/exact-z"])

	beforeDelete := fixture.engine.executionCounts()
	fixture.deleteIngress(t, "exact-a")
	retired := fixture.renderAndCommit(t)
	assert.NotContains(t, retired.HAProxyConfig, "exact-a.example.test/exact-a BACKEND:")
	assert.Contains(t, retired.HAProxyConfig, "exact-z.example.test/exact-z-v2 BACKEND:")
	assert.Equal(t, 1, strings.Count(retired.HAProxyConfig, "# haproxy-ingress/map-path-exact-haproxy-ingress"))
	assert.Equal(t, beforeDelete, fixture.engine.executionCounts())
}

func TestHAProxyIngressUniqueOnlyComponentsKeepAbortCachesIsolated(t *testing.T) {
	components := []string{"map-path-exact-600-haproxy-ingress"}
	snippets := loadHAProxyIngressUniqueOnlySnippets(t, components)
	fixture := newHAProxyTechFixtureWithSnippets(t, haproxyIngressUniqueOnlyRoot(components, true), snippets)
	baselineIngress := haproxyIngressUniquePathIngress("subject", "exact", "subject.example.test", "/v1", "v1")
	fixture.addIngress(t, baselineIngress)
	baseline := fixture.renderAndCommit(t)
	assert.Contains(t, baseline.HAProxyConfig, "subject.example.test/v1 BACKEND:default_subject_svc_echo_80")
	assert.Equal(t, 1, fixture.engine.executionCounts()["ingresses/subject"])
	assert.Equal(t, baseline.HAProxyConfig, fixture.renderAndCommit(t).HAProxyConfig)

	invalid := haproxyIngressUniquePathIngress("subject", "exact", "subject.example.test", "/valid\nPOISON", "admission-invalid")
	failed, err := fixture.service.Render(
		t.Context(), haproxyIngressAdmissionProvider(fixture, invalid), rendercontext.RenderModeAdmission,
		rendercontext.WithAdmissionSubject("ingresses", "default", "subject"),
	)
	require.ErrorContains(t, err, "would split the routing map")
	assert.Nil(t, failed)
	assert.Equal(t, 2, fixture.engine.executionCounts()["ingresses/subject"])
	assert.Equal(t, baseline.HAProxyConfig, fixture.renderAndCommit(t).HAProxyConfig)
	assert.Equal(t, 2, fixture.engine.executionCounts()["ingresses/subject"])

	proposed := haproxyIngressUniquePathIngress("subject", "exact", "subject.example.test", "/admission", "admission-valid")
	admission, err := fixture.service.Render(
		t.Context(), haproxyIngressAdmissionProvider(fixture, proposed), rendercontext.RenderModeAdmission,
		rendercontext.WithAdmissionSubject("ingresses", "default", "subject"),
	)
	require.NoError(t, err)
	assert.Contains(t, admission.HAProxyConfig, "subject.example.test/admission BACKEND:default_subject_svc_echo_80")
	assert.Equal(t, 3, fixture.engine.executionCounts()["ingresses/subject"])
	assert.Equal(t, baseline.HAProxyConfig, fixture.renderAndCommit(t).HAProxyConfig)
	assert.Equal(t, 3, fixture.engine.executionCounts()["ingresses/subject"])

	fixture.updateIngress(t, proposed)
	fixture.config.TemplatingSettings.ExtraContext["failAfterAuth"] = true
	failed, err = fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.ErrorContains(t, err, "forced HAProxy Ingress root failure")
	assert.Nil(t, failed)
	assert.Equal(t, 4, fixture.engine.executionCounts()["ingresses/subject"])

	fixture.config.TemplatingSettings.ExtraContext["failAfterAuth"] = false
	retried := fixture.renderAndCommit(t)
	assert.Contains(t, retried.HAProxyConfig, "subject.example.test/admission BACKEND:default_subject_svc_echo_80")
	assert.Equal(t, 5, fixture.engine.executionCounts()["ingresses/subject"])
	assert.Equal(t, retried.HAProxyConfig, fixture.renderAndCommit(t).HAProxyConfig)
	assert.Equal(t, 5, fixture.engine.executionCounts()["ingresses/subject"])
}

func TestHAProxyIngressUniqueOnlyColdMatchesDetachedHEAD(t *testing.T) {
	baselineRoot := os.Getenv(haproxyIngressUniqueBaselineEnv)
	if baselineRoot == "" {
		t.Skip("run scripts/test-haproxy-ingress-unique-differential.sh to compare against detached HEAD")
	}

	current := newHAProxyTechFixtureWithSnippets(
		t,
		haproxyIngressUniqueOnlyRoot(haproxyIngressUniqueOnlyComponents, false),
		loadHAProxyIngressUniqueOnlySnippets(t, haproxyIngressUniqueOnlyComponents),
	)
	legacy := newHAProxyTechFixtureWithSnippets(
		t,
		haproxyIngressUniqueOnlyRoot(haproxyIngressUniqueOnlyComponents, false),
		loadHAProxyIngressUniqueOnlySnippetsFromRoot(t, baselineRoot, haproxyIngressUniqueOnlyComponents),
	)
	populateHAProxyIngressUniqueDifferential(t, current)
	legacy.provider = current.provider

	currentResult := current.renderAndCommit(t)
	legacyResult := legacy.renderAndCommit(t)
	assert.Equal(t, legacyResult.HAProxyConfig, currentResult.HAProxyConfig, "haproxy.cfg bytes")
	assert.Equal(t, requireAuxiliaryFiles(t, legacyResult), requireAuxiliaryFiles(t, currentResult), "auxiliary files")
	assert.Equal(t, requireRenderPlan(t, legacyResult), requireRenderPlan(t, currentResult), "canonical render plan")
	assert.Equal(t, legacyResult.PlanID, currentResult.PlanID, "canonical render plan ID")
	assert.Equal(t, materializedStatusPatches(t, legacyResult), materializedStatusPatches(t, currentResult), "status patches")
	assert.Equal(t, requireRenderEvents(t, legacyResult), requireRenderEvents(t, currentResult), "events")
	assert.Equal(t, requireRenderedResources(t, legacyResult), requireRenderedResources(t, currentResult), "rendered resources")
}

func TestHAProxyIngressCORSColdMatchesDetachedHEAD(t *testing.T) {
	baselineRoot := os.Getenv(haproxyIngressUniqueBaselineEnv)
	if baselineRoot == "" {
		t.Skip("run scripts/test-haproxy-ingress-unique-differential.sh to compare against detached HEAD")
	}
	currentComponents := []string{
		"util-incremental-haproxy-ingress-cors",
		"frontend-filters-660-haproxy-ingress-cors",
	}
	legacyComponents := []string{"frontend-filters-660-haproxy-ingress-cors"}
	current := newHAProxyTechFixtureWithSnippets(
		t,
		`{{- render "util-incremental-haproxy-ingress-cors" -}}{{- render "frontend-filters-660-haproxy-ingress-cors" -}}`,
		loadHAProxyIngressUniqueOnlySnippetsFromRoot(t, haproxyIngressCurrentRepositoryRoot(t), currentComponents),
	)
	legacy := newHAProxyTechFixtureWithSnippets(
		t,
		`{{- render "frontend-filters-660-haproxy-ingress-cors" -}}`,
		loadHAProxyIngressUniqueOnlySnippetsFromRoot(t, baselineRoot, legacyComponents),
	)
	populateHAProxyIngressCORSDifferential(t, current)
	legacy.provider = current.provider

	currentResult := current.renderAndCommit(t)
	legacyResult := legacy.renderAndCommit(t)
	assert.Equal(t, legacyResult.HAProxyConfig, currentResult.HAProxyConfig, "haproxy.cfg bytes")
	assert.Equal(t, requireAuxiliaryFiles(t, legacyResult), requireAuxiliaryFiles(t, currentResult), "auxiliary files")
	assert.Equal(t, requireRenderPlan(t, legacyResult), requireRenderPlan(t, currentResult), "canonical render plan")
	assert.Equal(t, legacyResult.PlanID, currentResult.PlanID, "canonical render plan ID")
	assert.Equal(t, requireRenderEvents(t, legacyResult), requireRenderEvents(t, currentResult), "events")
}

func haproxyIngressUniqueOnlyRoot(components []string, failAfter bool) string {
	root := ""
	for _, component := range components {
		root += `{{- render "` + component + `" -}}`
	}
	if failAfter {
		root += haproxyIngressSweepFailure
	}
	return root
}

func loadHAProxyIngressUniqueOnlySnippets(
	t *testing.T,
	components []string,
) map[string]config.TemplateSnippet {
	t.Helper()
	all, _ := loadHAProxyIngressSweepSnippets(t)
	return selectHAProxyIngressSnippetClosure(t, all, components)
}

func loadHAProxyIngressUniqueOnlySnippetsFromRoot(
	t *testing.T,
	repositoryRoot string,
	components []string,
) map[string]config.TemplateSnippet {
	t.Helper()
	chartRoot := filepath.Join(repositoryRoot, "charts", "haptic", "charts")
	haproxyIngressFiles, err := filepath.Glob(filepath.Join(chartRoot, "haproxy-ingress", "*.yaml"))
	require.NoError(t, err)
	files := append([]string{
		filepath.Join(chartRoot, "base", "library.yaml"),
		filepath.Join(chartRoot, "ingress", "library.yaml"),
		filepath.Join(chartRoot, "ingress-annotations-compat", "library.yaml"),
	}, haproxyIngressFiles...)
	all := map[string]config.TemplateSnippet{}
	for _, path := range files {
		content, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		var library haproxyTechChartLibrary
		require.NoError(t, yaml.Unmarshal(content, &library), path)
		for name, chartSnippet := range library.TemplateSnippets {
			snippet := config.TemplateSnippet{Name: name, Template: chartSnippet.Template, Requires: chartSnippet.Requires}
			if chartSnippet.Incremental != nil {
				snippet.Incremental = &config.IncrementalTemplate{
					Source: chartSnippet.Incremental.Source, BindingsTemplate: chartSnippet.Incremental.BindingsTemplate,
					WhenAnyPathExists: chartSnippet.Incremental.WhenAnyPathExists,
					Group:             chartSnippet.Incremental.Group, Consumes: chartSnippet.Incremental.Consumes,
					OptionalConsumes: chartSnippet.Incremental.OptionalConsumes,
					Effects:          chartSnippet.Incremental.Effects,
				}
			}
			all[name] = snippet
		}
	}
	return selectHAProxyIngressSnippetClosure(t, all, components)
}

func selectHAProxyIngressSnippetClosure(
	t *testing.T,
	all map[string]config.TemplateSnippet,
	components []string,
) map[string]config.TemplateSnippet {
	t.Helper()
	selected := map[string]bool{}
	queue := append([]string(nil), components...)
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if selected[name] {
			continue
		}
		snippet, found := all[name]
		require.True(t, found, name)
		selected[name] = true
		sources := []string{snippet.Template}
		if snippet.Incremental != nil {
			sources = append(sources, snippet.Incremental.BindingsTemplate)
		}
		for _, source := range sources {
			for _, match := range haproxyIngressSnippetDependencyPattern.FindAllStringSubmatch(source, -1) {
				queue = append(queue, match[1])
			}
		}
	}
	result := make(map[string]config.TemplateSnippet, len(selected))
	for name := range selected {
		result[name] = all[name]
	}
	return result
}

func haproxyIngressUniqueIngress(name string, annotations map[string]any, host, revision string) map[string]any {
	ingress := haproxyTechIngress(name, annotations, revision)
	rules := ingress["spec"].(map[string]any)["rules"].([]any)
	rules[0].(map[string]any)["host"] = host
	return ingress
}

func haproxyIngressUniquePathIngress(name, pathType, host, path, revision string) map[string]any {
	ingress := haproxyIngressUniqueIngress(name, map[string]any{
		"haproxy-ingress.github.io/path-type": pathType,
	}, host, revision)
	rules := ingress["spec"].(map[string]any)["rules"].([]any)
	paths := rules[0].(map[string]any)["http"].(map[string]any)["paths"].([]any)
	paths[0].(map[string]any)["path"] = path
	paths[0].(map[string]any)["pathType"] = "ImplementationSpecific"
	return ingress
}

func populateHAProxyIngressUniqueDifferential(t *testing.T, fixture *haproxyTechGatedFixture) {
	t.Helper()
	fixture.addIngress(t, haproxyIngressUniquePathIngress("exact-a", "exact", "exact-a.example.test", "/exact-a", "v1"))
	fixture.addIngress(t, haproxyIngressUniquePathIngress("exact-z", "exact", "exact-z.example.test", "/exact-z", "v1"))
	fixture.addIngress(t, haproxyIngressUniquePathIngress("prefix", "prefix", "prefix.example.test", "/prefix", "v1"))
	fixture.addIngress(t, haproxyIngressUniquePathIngress("begin", "begin", "begin.example.test", "/begin", "v1"))
	fixture.addIngress(t, haproxyIngressUniquePathIngress("regex", "regex", "regex.example.test", "^/items/[0-9]+$", "v1"))
	fixture.addIngress(t, haproxyIngressUniquePathIngress("invalid", "exact", "invalid.example.test", "/valid\nPOISON", "v1"))
	fixture.addIngress(t, haproxyIngressUniqueIngress("alias", map[string]any{
		"haproxy-ingress.github.io/server-alias": "alias-a.example.test, alias-b.example.test",
	}, "primary.example.test", "v1"))
	fixture.addIngress(t, haproxyIngressUniqueIngress("alias-regex", map[string]any{
		"haproxy-ingress.github.io/server-alias-regex": `^regex-[a-z]+\.example\.test$`,
	}, "regex-primary.example.test", "v1"))
	fixture.addIngress(t, haproxyIngressUniqueIngress("mtls", map[string]any{
		"haproxy-ingress.github.io/auth-tls-cert-header": "true",
	}, "mtls.example.test", "v1"))
}

func populateHAProxyIngressCORSDifferential(t *testing.T, fixture *haproxyTechGatedFixture) {
	t.Helper()
	fixture.addIngress(t, haproxyIngressUniqueIngress("cors-any", map[string]any{
		"haproxy-ingress.github.io/cors-enable":       "true",
		"haproxy-ingress.github.io/cors-allow-origin": "*",
	}, "any.example.test", "v1"))
	fixture.addIngress(t, haproxyIngressUniqueIngress("cors-exact", map[string]any{
		"haproxy-ingress.github.io/cors-enable":            "true",
		"haproxy-ingress.github.io/cors-allow-origin":      "https://example.com",
		"haproxy-ingress.github.io/cors-allow-credentials": "true",
		"haproxy-ingress.github.io/cors-expose-headers":    "X-Trace-ID",
	}, "exact.example.test", "v1"))
	fixture.addIngress(t, haproxyIngressUniqueIngress("cors-invalid", map[string]any{
		"haproxy-ingress.github.io/cors-enable":        "true",
		"haproxy-ingress.github.io/cors-allow-methods": "GET\nPOISON",
	}, "invalid.example.test", "v1"))
}

func haproxyIngressCurrentRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", "..", ".."))
}

func haproxyIngressAdmissionProvider(
	fixture *haproxyTechGatedFixture,
	ingress map[string]any,
) stores.StoreProvider {
	return stores.NewOverlayStoreProvider(
		fixture.provider,
		stores.NewValidationContext(map[string]*stores.StoreOverlay{
			"ingresses": stores.NewStoreOverlayForUpdate(&unstructured.Unstructured{Object: ingress}),
		}),
	)
}
