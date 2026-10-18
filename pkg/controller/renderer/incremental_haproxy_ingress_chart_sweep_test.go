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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
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

const haproxyIngressSweepFailure = `{%- if tostring(extraContext | dig("failAfterAuth") | fallback(false)) == "true" -%}{{ fail("forced HAProxy Ingress root failure") }}{%- end -%}`

var haproxyIngressSnippetDependencyPattern = regexp.MustCompile(`(?:import|render)\s+"([^"]+)"`)

const haproxyIngressForwardForBlock = "forwarded-for-mode-500-ingress"

// haproxyIngressSrcIPGroup declares the src-ip group the forwardfor block also reads.
const haproxyIngressSrcIPGroup = "ingress-src-ip-9999-declaration"

func TestHAProxyIngressIncrementalSweepScalesWithChangedResources(t *testing.T) {
	for _, resourceCount := range []int{300, 1000, 3000} {
		t.Run(fmt.Sprintf("resources-%d", resourceCount), func(t *testing.T) {
			snippets, components := loadHAProxyIngressSweepSnippets(t)
			fixture := newHAProxyTechFixtureWithSnippets(t, haproxyIngressSweepRoot(components), snippets)
			for index := range resourceCount {
				fixture.addIngress(t, haproxyTechIngress(fmt.Sprintf("inactive-%04d", index), nil, "v1"))
			}
			target := fmt.Sprintf("inactive-%04d", resourceCount/2)

			cold := fixture.renderAndCommit(t)
			assert.Empty(t, fixture.engine.executionCounts())
			warm := fixture.renderAndCommit(t)
			assert.Equal(t, cold.HAProxyConfig, warm.HAProxyConfig)
			assert.Empty(t, fixture.engine.executionCounts())

			fixture.updateIngress(t, haproxyTechIngress(target, nil, "v2"))
			unrelated := fixture.renderAndCommit(t)
			assert.Equal(t, cold.HAProxyConfig, unrelated.HAProxyConfig)
			assert.Empty(t, fixture.engine.executionCounts())

			fixture.updateIngress(t, haproxyTechIngress(target, map[string]any{
				"haproxy-ingress.github.io/forwardfor": "add",
			}, "v3"))
			active := fixture.renderAndCommit(t)
			assert.Contains(t, active.HAProxyConfig, "# ingress/forwardfor")
			assert.Contains(t, active.HAProxyConfig, `http-request set-var(txn.xff_mode) var(txn.resource_key),map(`)
			assert.Equal(t, "default/"+target+" replace\n", haproxyIngressXFFRoutes(t, active))
			assert.Equal(t, map[string]int{"ingresses/" + target: 1}, fixture.engine.executionCounts())
			assert.Equal(t, active.HAProxyConfig, fixture.renderAndCommit(t).HAProxyConfig)
			assert.Equal(t, map[string]int{"ingresses/" + target: 1}, fixture.engine.executionCounts())
		})
	}
}

func TestHAProxyIngressIncrementalSweepMatchesDetachedLegacy(t *testing.T) {
	snippets, components := loadHAProxyIngressSweepSnippets(t)
	current := newHAProxyTechFixtureWithSnippets(t, haproxyIngressSweepRoot(components), snippets)
	// A cold fixture over the same stores renders the lane from scratch; the
	// incremental sweep must yield the same bytes after every change.
	cold := func() string {
		fresh := newHAProxyTechFixtureWithSnippets(t, haproxyIngressSweepRoot(components), snippets)
		fresh.provider = current.provider
		return fresh.renderAndCommit(t).HAProxyConfig
	}

	current.addIngress(t, haproxyTechIngress("z", map[string]any{
		"haproxy-ingress.github.io/forwardfor": "ifmissing",
	}, "v1"))
	current.addIngress(t, haproxyTechIngress("a", map[string]any{
		"haproxy-ingress.github.io/forwardfor": "update",
	}, "v1"))
	assert.Equal(t, cold(), current.renderAndCommit(t).HAProxyConfig)

	current.updateIngress(t, haproxyTechIngress("z", map[string]any{
		"haproxy-ingress.github.io/forwardfor": "add",
	}, "v2"))
	assert.Equal(t, cold(), current.renderAndCommit(t).HAProxyConfig)
	current.deleteIngress(t, "a")
	assert.Equal(t, cold(), current.renderAndCommit(t).HAProxyConfig)
}

func TestHAProxyIngressIncrementalSweepAbortsDoNotPoisonCache(t *testing.T) {
	snippets, components := loadHAProxyIngressSweepSnippets(t)
	fixture := newHAProxyTechFixtureWithSnippets(t, haproxyIngressSweepRoot(components), snippets)
	fixture.addIngress(t, haproxyTechIngress("subject", nil, "v1"))
	baseline := fixture.renderAndCommit(t)
	assert.Empty(t, fixture.engine.executionCounts())

	proposed := haproxyTechIngress("subject", map[string]any{
		"haproxy-ingress.github.io/forwardfor": "add",
	}, "admission")
	overlay := stores.NewOverlayStoreProvider(
		fixture.provider,
		stores.NewValidationContext(map[string]*stores.StoreOverlay{
			"ingresses": stores.NewStoreOverlayForUpdate(&unstructured.Unstructured{Object: proposed}),
		}),
	)
	admission, err := fixture.service.Render(
		t.Context(), overlay, rendercontext.RenderModeAdmission,
		rendercontext.WithAdmissionSubject("ingresses", "default", "subject"),
	)
	require.NoError(t, err)
	assert.Contains(t, admission.HAProxyConfig, "ingress/forwardfor")
	assert.Equal(t, map[string]int{"ingresses/subject": 1}, fixture.engine.executionCounts())
	assert.Equal(t, baseline.HAProxyConfig, fixture.renderAndCommit(t).HAProxyConfig)
	assert.Equal(t, map[string]int{"ingresses/subject": 1}, fixture.engine.executionCounts())

	fixture.updateIngress(t, proposed)
	fixture.config.TemplatingSettings.ExtraContext["failAfterAuth"] = true
	failed, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.ErrorContains(t, err, "forced HAProxy Ingress root failure")
	assert.Nil(t, failed)
	assert.Equal(t, map[string]int{"ingresses/subject": 2}, fixture.engine.executionCounts())

	fixture.config.TemplatingSettings.ExtraContext["failAfterAuth"] = false
	retried := fixture.renderAndCommit(t)
	assert.Contains(t, retried.HAProxyConfig, "ingress/forwardfor")
	assert.Equal(t, map[string]int{"ingresses/subject": 3}, fixture.engine.executionCounts())
	assert.Equal(t, retried.HAProxyConfig, fixture.renderAndCommit(t).HAProxyConfig)
	assert.Equal(t, map[string]int{"ingresses/subject": 3}, fixture.engine.executionCounts())
}

func loadHAProxyIngressSweepSnippets(t *testing.T) (snippets map[string]config.TemplateSnippet, components []string) {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	chartRoot := filepath.Join(filepath.Dir(sourceFile), "..", "..", "..", "charts", "haptic", "charts")
	haproxyFiles, err := filepath.Glob(filepath.Join(chartRoot, "haproxy-ingress", "*.yaml"))
	require.NoError(t, err)
	dependencyFiles := []string{
		filepath.Join(chartRoot, "base", "library.yaml"),
		filepath.Join(chartRoot, "ingress", "library.yaml"),
		filepath.Join(chartRoot, "ingress-annotations-compat", "library.yaml"),
		filepath.Join(chartRoot, "http-policies", "81-waf-governance.yaml"),
		filepath.Join(chartRoot, "kubernetes-backends", "library.yaml"),
		filepath.Join(chartRoot, "spoa-hub", "10-features.yaml"),
	}

	all := map[string]haproxyTechChartSnippet{}
	owners := map[string]string{}
	for _, path := range append(dependencyFiles, haproxyFiles...) {
		content, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		if strings.Contains(filepath.ToSlash(path), "/haproxy-ingress/") {
			assert.NotContains(t, string(content), "resources.ingresses.List()", path)
		}
		var library haproxyTechChartLibrary
		require.NoError(t, yaml.Unmarshal(content, &library), path)
		for name, snippet := range library.TemplateSnippets {
			all[name] = snippet
			owners[name] = path
		}
	}

	selected := map[string]bool{}
	components = []string{}
	for name, snippet := range all {
		if !strings.Contains(filepath.ToSlash(owners[name]), "/haproxy-ingress/") || snippet.Incremental == nil {
			continue
		}
		selected[name] = true
		components = append(components, name)
	}
	require.NotEmpty(t, components)
	sort.Strings(components)

	// The forwardfor block is not incremental itself; it reads the lane the
	// publisher above fills, so the root renders it after the components.
	selected[haproxyIngressForwardForBlock] = true
	selected[haproxyIngressSrcIPGroup] = true
	expandHAProxyIngressSweepSelection(t, all, selected, append(append([]string(nil), components...), haproxyIngressForwardForBlock))
	return haproxyIngressSweepConfigSnippets(all, selected), components
}

func haproxyIngressXFFRoutes(t *testing.T, result *RenderResult) string {
	t.Helper()
	for _, file := range requireAuxiliaryFiles(t, result).MapFiles {
		if strings.HasSuffix(file.Path, "/ing-xff-routes.map") || file.Path == "ing-xff-routes.map" {
			return file.Content
		}
	}
	require.FailNow(t, "ing-xff-routes.map is missing")
	return ""
}

func expandHAProxyIngressSweepSelection(
	t *testing.T,
	all map[string]haproxyTechChartSnippet,
	selected map[string]bool,
	components []string,
) {
	t.Helper()
	queue := append([]string(nil), components...)
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		snippet, found := all[name]
		require.True(t, found, name)
		for _, source := range []string{snippet.Template, func() string {
			if snippet.Incremental == nil {
				return ""
			}
			return snippet.Incremental.BindingsTemplate
		}()} {
			for _, match := range haproxyIngressSnippetDependencyPattern.FindAllStringSubmatch(source, -1) {
				dependency := match[1]
				if selected[dependency] {
					continue
				}
				_, exists := all[dependency]
				require.True(t, exists, "%s imports missing snippet %s", name, dependency)
				selected[dependency] = true
				queue = append(queue, dependency)
			}
		}
	}
}

func haproxyIngressSweepConfigSnippets(
	all map[string]haproxyTechChartSnippet,
	selected map[string]bool,
) map[string]config.TemplateSnippet {
	result := make(map[string]config.TemplateSnippet, len(selected))
	for name := range selected {
		chartSnippet := all[name]
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
		result[name] = snippet
	}
	return result
}

func haproxyIngressSweepRoot(components []string) string {
	var root strings.Builder
	for _, name := range components {
		fmt.Fprintf(&root, `{{- render %q -}}`, name)
	}
	fmt.Fprintf(&root, `{{- render %q -}}`, haproxyIngressSrcIPGroup)
	fmt.Fprintf(&root, `{{- render %q -}}`, haproxyIngressForwardForBlock)
	root.WriteString(haproxyIngressSweepFailure)
	return root.String()
}
