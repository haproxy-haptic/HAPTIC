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
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/helpers"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	k8sstore "github.com/haproxy-haptic/HAPTIC/pkg/k8s/store"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
)

var haproxyTechGatedComponents = []string{
	"ingress-forwardfor-0500-haproxytech",
	"ingress-access-control-0200-haproxytech",
	"ingress-cors-0300-haproxytech",
	"haproxytech-ssl-redirect-port-publications",
	"haproxytech-request-redirect-publications",
	"haproxytech-logging-publications",
	"haproxytech-auth-userlist-publications",
	"map-reqhdr-host-250-haproxytech",
	"ingress-header-mods-0300-haproxytech",
	"haproxytech-captured-headers-publications",
}

const haproxyTechGatedRoot = `{{- render "ingress-forwardfor-0500-haproxytech" -}}
{{- render "ingress-access-control-0200-haproxytech" -}}
{{- render "ingress-cors-0300-haproxytech" -}}
{{- render "haproxytech-ssl-redirect-port-publications" -}}
{{- render "haproxytech-request-redirect-publications" -}}
{{- render "haproxytech-logging-publications" -}}
{{- render "haproxytech-auth-userlist-publications" -}}
{{- render "map-reqhdr-host-250-haproxytech" -}}
{{- render "ingress-header-mods-0300-haproxytech" -}}
{{- render "haproxytech-captured-headers-publications" -}}
{{ len(incremental_values("ingress-forwardfor", "route")) }}`

const haproxyTechAuthRoot = `{{- render "ingress-forwardfor-0500-haproxytech" -}}
{{- render "ingress-access-control-0200-haproxytech" -}}
{{- render "ingress-cors-0300-haproxytech" -}}
{{- render "haproxytech-ssl-redirect-port-publications" -}}
{{- render "haproxytech-request-redirect-publications" -}}
{{- render "haproxytech-logging-publications" -}}
{{- render "haproxytech-auth-userlist-publications" -}}
{{- render "map-reqhdr-host-250-haproxytech" -}}
{{- render "ingress-header-mods-0300-haproxytech" -}}
{{- render "haproxytech-captured-headers-publications" -}}
{%- for _, entryAny := range incremental_values("haproxytech-auth-userlists", "userlists") -%}
  {%- var entry = entryAny.(map[string]any) -%}
  {{- "winner=" + tostring(entry["key"]) -}}
  {%- for _, userAny := range toSlice(entry["users"]) -%}
    {%- var user = userAny.(map[string]any) -%}
    {{- "|" + tostring(user["username"]) + "=" + tostring(user["passwordHash"]) -}}
  {%- end -%}
{%- end -%}
{%%
if tostring(extraContext | dig("failAfterAuth") | fallback(false)) == "true" {
  fail("forced failure after auth publications")
}
%%}`

const haproxyTechSetHostCollisionRoot = `{{- render "map-reqhdr-host-250-haproxytech" -}}
{{- render "map-reqhdr-host-760-nginx-ingress" -}}
{{- render "map-reqhdr-host-850-haptic" -}}`

const haproxyTechSetHostRoot = `{{ render "map-reqhdr-host-250-haproxytech" }}`

const haproxyTechLegacySetHostRoot = `{{ render "legacy-map-reqhdr-host-250-haproxytech" }}`

type haproxyTechChartLibrary struct {
	TemplateSnippets map[string]haproxyTechChartSnippet `yaml:"templateSnippets"`
}

type haproxyTechChartSnippet struct {
	Template    string                       `yaml:"template"`
	Requires    []string                     `yaml:"requires"`
	Incremental *haproxyTechChartIncremental `yaml:"incremental"`
}

type haproxyTechChartIncremental struct {
	Source            string                     `yaml:"source"`
	BindingsTemplate  string                     `yaml:"bindingsTemplate"`
	WhenAnyPathExists []string                   `yaml:"whenAnyPathExists"`
	Group             string                     `yaml:"group"`
	Consumes          []string                   `yaml:"consumes"`
	OptionalConsumes  []string                   `yaml:"optionalConsumes"`
	Effects           []config.IncrementalEffect `yaml:"effects"`
}

type haproxyTechGatedFixture struct {
	config    *config.Config
	service   *RenderService
	engine    *dynamicBindingCountingEngine
	ingresses *k8sstore.MemoryStore
	secrets   *k8sstore.MemoryStore
	provider  stores.StoreProvider
}

func TestHAProxyTechGatedComponentsSkipAnnotationFreeIngresses(t *testing.T) {
	for _, resourceCount := range []int{1, 64, 512} {
		t.Run(fmt.Sprintf("resources-%d", resourceCount), func(t *testing.T) {
			fixture := newHAProxyTechGatedFixture(t)
			for index := range resourceCount {
				fixture.addIngress(t, haproxyTechIngress(fmt.Sprintf("inactive-%03d", index), nil, "v1"))
			}
			target := fmt.Sprintf("inactive-%03d", resourceCount/2)

			assert.Equal(t, "0\n", fixture.renderAndCommit(t).HAProxyConfig)
			assert.Empty(t, fixture.engine.executionCounts())
			assert.Equal(t, "0\n", fixture.renderAndCommit(t).HAProxyConfig)
			assert.Empty(t, fixture.engine.executionCounts())

			fixture.updateIngress(t, haproxyTechIngress(target, nil, "v2"))
			assert.Equal(t, "0\n", fixture.renderAndCommit(t).HAProxyConfig)
			assert.Empty(t, fixture.engine.executionCounts())

			fixture.updateIngress(t, haproxyTechIngress(target, map[string]any{
				"haproxy.org/forwarded-for": "false",
			}, "v3"))
			assert.Equal(t, "1\n", fixture.renderAndCommit(t).HAProxyConfig)
			assert.Equal(t, map[string]int{"ingresses/" + target: 1}, fixture.engine.executionCounts())
			assert.Equal(t, "1\n", fixture.renderAndCommit(t).HAProxyConfig)
			assert.Equal(t, map[string]int{"ingresses/" + target: 1}, fixture.engine.executionCounts())

			fixture.updateIngress(t, haproxyTechIngress(target, nil, "v4"))
			assert.Equal(t, "0\n", fixture.renderAndCommit(t).HAProxyConfig)
			assert.Equal(t, map[string]int{"ingresses/" + target: 1}, fixture.engine.executionCounts())
		})
	}
}

func TestHAProxyTechAuthPublicationsDoNotPoisonWinnerCache(t *testing.T) {
	fixture := newHAProxyTechFixture(t, haproxyTechAuthRoot)
	fixture.addSecret(t, haproxyTechAuthSecret("shared-auth", "JDJ5JDA1JGZpcnN0"))
	fixture.addIngress(t, haproxyTechAuthIngress("a", "v1"))
	fixture.addIngress(t, haproxyTechAuthIngress("b", "v1"))

	first := fixture.renderAndCommit(t)
	assert.Contains(t, first.HAProxyConfig, "winner=default/a|alice=$2y$05$first")
	assert.Equal(t, map[string]int{"ingresses/a": 1, "ingresses/b": 1}, fixture.engine.executionCounts())
	assert.Equal(t, first.HAProxyConfig, fixture.renderAndCommit(t).HAProxyConfig)
	assert.Equal(t, map[string]int{"ingresses/a": 1, "ingresses/b": 1}, fixture.engine.executionCounts())

	fixture.updateIngress(t, haproxyTechAuthIngress("b", "v2"))
	assert.Equal(t, first.HAProxyConfig, fixture.renderAndCommit(t).HAProxyConfig)
	assert.Equal(t, map[string]int{"ingresses/a": 1, "ingresses/b": 2}, fixture.engine.executionCounts())

	fixture.deleteIngress(t, "a")
	promoted := fixture.renderAndCommit(t)
	assert.Contains(t, promoted.HAProxyConfig, "winner=default/b|alice=$2y$05$first")
	assert.NotContains(t, promoted.HAProxyConfig, "winner=default/a")
	assert.Equal(t, map[string]int{"ingresses/a": 1, "ingresses/b": 2}, fixture.engine.executionCounts())

	fixture.updateSecret(t, haproxyTechAuthSecret("shared-auth", "JDJ5JDA1JHNlY29uZA=="))
	rotated := fixture.renderAndCommit(t)
	assert.Contains(t, rotated.HAProxyConfig, "winner=default/b|alice=$2y$05$second")
	assert.Equal(t, map[string]int{"ingresses/a": 1, "ingresses/b": 3}, fixture.engine.executionCounts())

	committedExecutions := fixture.authExecutions()
	proposed := haproxyTechAuthIngress("b", "admission")
	proposed["metadata"].(map[string]any)["annotations"].(map[string]any)["haproxy.org/auth-type"] = "digest"
	overlay := stores.NewOverlayStoreProvider(
		fixture.provider,
		stores.NewValidationContext(map[string]*stores.StoreOverlay{
			"ingresses": stores.NewStoreOverlayForUpdate(&unstructured.Unstructured{Object: proposed}),
		}),
	)
	failed, err := fixture.service.Render(
		t.Context(), overlay, rendercontext.RenderModeAdmission,
		rendercontext.WithAdmissionSubject("ingresses", "default", "b"),
	)
	require.ErrorContains(t, err, "Invalid value 'digest'")
	assert.Nil(t, failed)
	assert.Equal(t, committedExecutions, fixture.authExecutions())
	afterAdmissionExecutions := fixture.engine.executionCounts()
	assert.Equal(t, rotated.HAProxyConfig, fixture.renderAndCommit(t).HAProxyConfig)
	assert.Equal(t, afterAdmissionExecutions, fixture.engine.executionCounts())
	assert.Equal(t, committedExecutions, fixture.authExecutions())

	fixture.updateIngress(t, haproxyTechAuthIngress("b", "v3"))
	fixture.config.TemplatingSettings.ExtraContext["failAfterAuth"] = true
	failed, err = fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.ErrorContains(t, err, "forced failure after auth publications")
	assert.Nil(t, failed)
	assert.Equal(t, committedExecutions, fixture.authExecutions())
	beforeRetry := fixture.engine.executionCounts()
	fixture.config.TemplatingSettings.ExtraContext["failAfterAuth"] = false
	retried := fixture.renderAndCommit(t)
	assert.Equal(t, rotated.HAProxyConfig, retried.HAProxyConfig)
	assert.Equal(t, beforeRetry["ingresses/b"]+1, fixture.engine.executionCounts()["ingresses/b"])
	assert.Equal(t, committedExecutions+1, fixture.authExecutions())
}

func TestHAProxyTechSetHostSharedGroupCollisionAndPromotion(t *testing.T) {
	fixture := newHAProxyTechFixtureWithSnippets(
		t, haproxyTechSetHostCollisionRoot, loadHAProxyTechSetHostCollisionSnippets(t))
	fixture.addIngress(t, haproxyTechIngress("collision", map[string]any{
		"haproxy.org/set-host":                       "haproxy.internal",
		"nginx.ingress.kubernetes.io/upstream-vhost": "nginx.internal",
		"haproxy-haptic.org/set-host":                "haptic.internal",
	}, "v1"))

	backend := "default_collision_svc_echo_80"
	haproxyWinner := fixture.renderAndCommit(t)
	assert.Contains(t, haproxyWinner.HAProxyConfig, backend+" haproxy.internal")
	assert.NotContains(t, haproxyWinner.HAProxyConfig, "nginx.internal")
	assert.NotContains(t, haproxyWinner.HAProxyConfig, "haptic.internal")
	assert.Equal(t, 1, strings.Count(haproxyWinner.HAProxyConfig, backend+" "))
	assert.Equal(t, haproxyWinner.HAProxyConfig, fixture.renderAndCommit(t).HAProxyConfig)

	fixture.updateIngress(t, haproxyTechIngress("collision", map[string]any{
		"nginx.ingress.kubernetes.io/upstream-vhost": "nginx.internal",
		"haproxy-haptic.org/set-host":                "haptic.internal",
	}, "v1"))
	nginxPromoted := fixture.renderAndCommit(t)
	assert.Contains(t, nginxPromoted.HAProxyConfig, backend+" nginx.internal")
	assert.NotContains(t, nginxPromoted.HAProxyConfig, "haproxy.internal")
	assert.NotContains(t, nginxPromoted.HAProxyConfig, "haptic.internal")
	assert.Equal(t, 1, strings.Count(nginxPromoted.HAProxyConfig, backend+" "))

	fixture.updateIngress(t, haproxyTechIngress("collision", map[string]any{
		"haproxy-haptic.org/set-host": "haptic.internal",
	}, "v1"))
	hapticPromoted := fixture.renderAndCommit(t)
	assert.Contains(t, hapticPromoted.HAProxyConfig, backend+" haptic.internal")
	assert.NotContains(t, hapticPromoted.HAProxyConfig, "haproxy.internal")
	assert.NotContains(t, hapticPromoted.HAProxyConfig, "nginx.internal")
	assert.Equal(t, 1, strings.Count(hapticPromoted.HAProxyConfig, backend+" "))

	fixture.updateIngress(t, haproxyTechIngress("collision", map[string]any{
		"haproxy.org/set-host":        "haproxy.internal",
		"haproxy-haptic.org/set-host": "haptic.internal",
	}, "v1"))
	assert.Equal(t, haproxyWinner.HAProxyConfig, fixture.renderAndCommit(t).HAProxyConfig)
}

func TestHAProxyTechSetHostColdMatchesLegacyPublicationReplay(t *testing.T) {
	currentSnippets := loadHAProxyTechSetHostCollisionSnippets(t)
	delete(currentSnippets, "map-reqhdr-host-760-nginx-ingress")
	delete(currentSnippets, "map-reqhdr-host-850-haptic")
	current := newHAProxyTechFixtureWithSnippets(t, haproxyTechSetHostRoot, currentSnippets)

	legacySnippets := loadHAProxyTechSetHostCollisionSnippets(t)
	delete(legacySnippets, "map-reqhdr-host-250-haproxytech")
	delete(legacySnippets, "map-reqhdr-host-760-nginx-ingress")
	delete(legacySnippets, "map-reqhdr-host-850-haptic")
	legacySnippets["legacy-map-reqhdr-host-250-haproxytech"] = config.TemplateSnippet{
		Name: "legacy-map-reqhdr-host-250-haproxytech",
		Template: `{%- import "util-validate-config-value" for ValidateConfigValue -%}
{{- render "legacy-haproxytech-set-host-publications" -}}
{%- for _, entryAny := range incremental_values("legacy-haproxytech-set-host", "entries") -%}
  {%- var entry = entryAny.(map[string]any) -%}
  {%- var be = tostring(entry["backend"]) -%}
  {%- if first_seen("map-reqhdr-host", be) -%}
{{ "\n" }}{{ be }} {{ queryEscape(ValidateConfigValue(tostring(entry["host"]), "haproxy.org/set-host", tostring(entry["key"]), true)) }}
  {%- end -%}
{%- end -%}
`,
	}
	legacySnippets["legacy-haproxytech-set-host-publications"] = config.TemplateSnippet{
		Name:     "legacy-haproxytech-set-host-publications",
		Requires: []string{"ingresses"},
		Incremental: &config.IncrementalTemplate{
			Source: "ingresses", WhenAnyPathExists: []string{"metadata.annotations['haproxy.org/set-host']"},
			Group: "legacy-haproxytech-set-host", Effects: []config.IncrementalEffect{config.IncrementalEffectPublishValue},
		},
		Template: `{%- import "util-backend-name-ingress" for BackendNameIngress -%}
{%- var ingress = resources.ingresses.GetSingle(
  dig_string(item, "", "metadata", "namespace"), dig_string(item, "", "metadata", "name")) -%}
{%- if ingress != nil -%}
  {%- var host = ingress.Metadata.Annotations["haproxy.org/set-host"] -%}
  {%- if host != "" -%}
    {%- var key = ingress.Metadata.Namespace + "/" + ingress.Metadata.Name -%}
    {%- var seen = map[string]bool{} -%}
    {%- for _, rule := range ingress.Spec.Rules -%}
      {%- for _, path := range rule.Http.Paths -%}
        {%- var be = BackendNameIngress(ingress, path) -%}
        {%- if !seen[be] -%}
          {%- seen[be] = true -%}
          {{- shared.Publish("entries", key + "/" + be, map[string]any{
            "key": key, "backend": be, "host": host,
          }) -}}
        {%- end -%}
      {%- end -%}
    {%- end -%}
  {%- end -%}
{%- end -%}`,
	}
	legacy := newHAProxyTechFixtureWithSnippets(t, haproxyTechLegacySetHostRoot, legacySnippets)

	ingress := haproxyTechIngress("cold", map[string]any{
		"haproxy.org/set-host": "internal.svc.local",
	}, "v1")
	current.addIngress(t, ingress)
	legacy.provider = current.provider
	assert.Equal(t, legacy.renderAndCommit(t).HAProxyConfig, current.renderAndCommit(t).HAProxyConfig)
}

func newHAProxyTechGatedFixture(t *testing.T) *haproxyTechGatedFixture {
	t.Helper()
	return newHAProxyTechFixture(t, haproxyTechGatedRoot)
}

func newHAProxyTechFixture(t *testing.T, root string) *haproxyTechGatedFixture {
	t.Helper()
	return newHAProxyTechFixtureWithSnippets(t, root, loadHAProxyTechGatedSnippets(t))
}

func newHAProxyTechFixtureWithSnippets(
	t *testing.T,
	root string,
	snippets map[string]config.TemplateSnippet,
) *haproxyTechGatedFixture {
	t.Helper()
	cfg := &config.Config{
		Dataplane: testDataplaneConfig(),
		TemplatingSettings: config.TemplatingSettings{ExtraContext: map[string]any{
			"failAfterAuth": false,
		}},
		WatchedResources: map[string]config.WatchedResource{
			"ingresses": {APIVersion: "networking.k8s.io/v1", Resources: "ingresses", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"services":  {APIVersion: "v1", Resources: "services", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"endpoints": {APIVersion: "discovery.k8s.io/v1", Resources: "endpointslices", IndexBy: []string{"metadata.namespace", "metadata.labels.kubernetes\\.io/service-name"}},
			"secrets":   {APIVersion: "v1", Resources: "secrets", IndexBy: []string{"metadata.namespace", "metadata.name"}},
		},
		TemplateSnippets: snippets,
		HAProxyConfig:    config.HAProxyConfig{Template: root},
	}
	types := ingressBackendSchemaTypes(t)
	declarations := helpers.BuildAdditionalDeclarations(cfg, types)
	baseEngine, err := helpers.NewEngineFromConfigWithOptions(cfg, nil, nil, declarations, helpers.EngineOptions{})
	require.NoError(t, err)
	engine := newDynamicBindingCountingEngine(t, baseEngine)
	service := NewRenderService(&RenderServiceConfig{
		Engine: engine, Config: cfg, Logger: slog.Default(), Capabilities: defaultCapabilities(),
		TypedResourceTypes: types.Types,
	})
	ingresses := k8sstore.NewMemoryStore(2)
	secrets := k8sstore.NewMemoryStore(2)
	provider := stores.NewRealStoreProvider(map[string]stores.Store{
		"ingresses": ingresses,
		"services":  k8sstore.NewMemoryStore(2),
		"endpoints": k8sstore.NewMemoryStore(2),
		"secrets":   secrets,
	})
	return &haproxyTechGatedFixture{
		config: cfg, service: service, engine: engine, ingresses: ingresses, secrets: secrets, provider: provider,
	}
}

func loadHAProxyTechSetHostCollisionSnippets(t *testing.T) map[string]config.TemplateSnippet {
	t.Helper()
	wanted := map[string]bool{
		"util-backend-name-ingress":         true,
		"util-validate-config-value":        true,
		"map-reqhdr-host-250-haproxytech":   true,
		"map-reqhdr-host-760-nginx-ingress": true,
		"map-reqhdr-host-850-haptic":        true,
	}
	_, sourceFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	chartRoot := filepath.Join(filepath.Dir(sourceFile), "..", "..", "..", "charts", "haptic", "charts")
	files := []string{
		"base/library.yaml",
		"ingress/library.yaml",
		"haproxytech/library.yaml",
		"nginx-ingress/10-backend-directives.yaml",
		"haptic-annotations/26-rewrite-affinity.yaml",
	}
	result := make(map[string]config.TemplateSnippet, len(wanted))
	for _, relativePath := range files {
		content, err := os.ReadFile(filepath.Join(chartRoot, relativePath))
		require.NoError(t, err)
		var library haproxyTechChartLibrary
		require.NoError(t, yaml.Unmarshal(content, &library))
		for name, chartSnippet := range library.TemplateSnippets {
			if !wanted[name] {
				continue
			}
			snippet := config.TemplateSnippet{Name: name, Template: chartSnippet.Template, Requires: chartSnippet.Requires}
			if chartSnippet.Incremental != nil {
				snippet.Incremental = &config.IncrementalTemplate{
					Source:            chartSnippet.Incremental.Source,
					BindingsTemplate:  chartSnippet.Incremental.BindingsTemplate,
					WhenAnyPathExists: chartSnippet.Incremental.WhenAnyPathExists,
					Group:             chartSnippet.Incremental.Group,
					Consumes:          chartSnippet.Incremental.Consumes,
					OptionalConsumes:  chartSnippet.Incremental.OptionalConsumes,
					Effects:           chartSnippet.Incremental.Effects,
				}
			}
			result[name] = snippet
		}
	}
	require.Len(t, result, len(wanted))
	return result
}

func loadHAProxyTechGatedSnippets(t *testing.T) map[string]config.TemplateSnippet {
	t.Helper()
	wanted := map[string]bool{
		"util-webhook-reject-or-warn":         true,
		"util-config-injection-kind":          true,
		"util-validate-config-value":          true,
		"util-backend-name-ingress":           true,
		"util-ingress-header-publish":         true,
		"util-emit-annotation-access-control": true,
		"util-validate-cidr-list":             true,
		"util-publish-access-control":         true,
		"util-cors-publish":                   true,
		"util-forwardfor-publish":             true,
	}
	for _, name := range haproxyTechGatedComponents {
		wanted[name] = true
	}
	_, sourceFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	chartRoot := filepath.Join(filepath.Dir(sourceFile), "..", "..", "..", "charts", "haptic", "charts")
	files := []string{
		"base/library.yaml",
		"ingress/library.yaml",
		"ingress-annotations-compat/library.yaml",
		"haproxytech/library.yaml",
	}
	result := make(map[string]config.TemplateSnippet, len(wanted))
	for _, relativePath := range files {
		content, err := os.ReadFile(filepath.Join(chartRoot, relativePath))
		require.NoError(t, err)
		var library haproxyTechChartLibrary
		require.NoError(t, yaml.Unmarshal(content, &library))
		for name, chartSnippet := range library.TemplateSnippets {
			if !wanted[name] {
				continue
			}
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
	}
	require.Len(t, result, len(wanted))
	return result
}

func haproxyTechIngress(name string, annotations map[string]any, revision string) map[string]any {
	if annotations == nil {
		annotations = map[string]any{}
	}
	return map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "Ingress",
		"metadata": map[string]any{
			"namespace": "default", "name": name, "annotations": annotations,
		},
		"spec": map[string]any{
			"rules": []any{
				map[string]any{
					"host": revision + ".example.test",
					"http": map[string]any{
						"paths": []any{
							map[string]any{
								"path":     "/",
								"pathType": "Prefix",
								"backend": map[string]any{
									"service": map[string]any{
										"name": "echo",
										"port": map[string]any{"number": 80},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func haproxyTechAuthIngress(name, revision string) map[string]any {
	const secret = "shared-auth"
	return haproxyTechIngress(name, map[string]any{
		"haproxy.org/auth-type":   "basic-auth",
		"haproxy.org/auth-secret": secret,
	}, revision)
}

func haproxyTechAuthSecret(name, passwordHash string) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"namespace": "default", "name": name},
		"data":       map[string]any{"alice": passwordHash},
	}
}

func (f *haproxyTechGatedFixture) addIngress(t *testing.T, resource map[string]any) {
	t.Helper()
	name := resource["metadata"].(map[string]any)["name"].(string)
	require.NoError(t, f.ingresses.Add(resource, []string{"default", name}))
}

func (f *haproxyTechGatedFixture) updateIngress(t *testing.T, resource map[string]any) {
	t.Helper()
	name := resource["metadata"].(map[string]any)["name"].(string)
	require.NoError(t, f.ingresses.Update(resource, []string{"default", name}))
}

func (f *haproxyTechGatedFixture) deleteIngress(t *testing.T, name string) {
	t.Helper()
	require.NoError(t, f.ingresses.Delete("default", name, []string{"default", name}))
}

func (f *haproxyTechGatedFixture) addSecret(t *testing.T, resource map[string]any) {
	t.Helper()
	name := resource["metadata"].(map[string]any)["name"].(string)
	require.NoError(t, f.secrets.Add(resource, []string{"default", name}))
}

func (f *haproxyTechGatedFixture) updateSecret(t *testing.T, resource map[string]any) {
	t.Helper()
	name := resource["metadata"].(map[string]any)["name"].(string)
	require.NoError(t, f.secrets.Update(resource, []string{"default", name}))
}

func (f *haproxyTechGatedFixture) authExecutions() uint64 {
	const name = "b"
	return f.componentExecutions("haproxytech-auth-userlist-publications", name)
}

func (f *haproxyTechGatedFixture) componentExecutions(componentName, name string) uint64 {
	component := f.service.incremental.components[componentName]
	query := componentQueryKey(&component, "ingresses", "default", name)
	return f.service.incremental.graph.Counters(query).Executions
}

func (f *haproxyTechGatedFixture) renderAndCommit(t *testing.T) *RenderResult {
	t.Helper()
	result, err := f.service.Render(t.Context(), f.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, result.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, f.service)
	return result
}
