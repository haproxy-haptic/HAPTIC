// Copyright 2026 Philipp Hossner
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
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
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/helpers"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/typebootstrap"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	k8sstore "github.com/haproxy-haptic/HAPTIC/pkg/k8s/store"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
)

var hapticAnnotationsHSTSComponents = []string{
	"features-155-haproxy-ingress-hsts",
	"features-155-haptic-hsts",
	"features-155-nginx-ingress-hsts",
}

const hapticAnnotationsHSTSRoot = `{{- render "features-155-haproxy-ingress-hsts" -}}
{{- render "features-155-haptic-hsts" -}}
{{- render "features-155-nginx-ingress-hsts" -}}
{%- var incremental = map[string]string{} -%}
{%- for _, value := range incremental_values("hsts-hosts", "hosts") -%}
  {%- var record = value.(map[string]any) -%}
  {%- if tostring(extraContext | dig("poisonRead") | fallback(false)) == "true" -%}
    {%- record["value"] = "poison" -%}
  {%- end -%}
  {%- incremental[tostring(record["host"])] = tostring(record["value"]) -%}
{%- end -%}
{{ "\nI\n" -}}
{%- for _, host := range keys(incremental) -%}
{{ host }} {{ incremental[host] }}{{ "\n" -}}
{%- end -%}
{%%
var legacy = map[string]string{}
var contribute = func(enabledAnnotation string, maxAgeAnnotation string, subdomainsAnnotation string, preloadAnnotation string, defaultMaxAge string) {
  for _, ingress := range resources.ingresses.List() {
    if ingress.Metadata.Annotations[enabledAnnotation] != "true" { continue }
    var maxAge = ingress.Metadata.Annotations[maxAgeAnnotation]
    if maxAge == "" { maxAge = defaultMaxAge }
    var value = "max-age=" + maxAge
    if ingress.Metadata.Annotations[subdomainsAnnotation] == "true" { value += "; includeSubDomains" }
    if ingress.Metadata.Annotations[preloadAnnotation] == "true" { value += "; preload" }
    for _, rule := range ingress.Spec.Rules {
      var host = toLower(rule.Host)
      if host != "" && legacy[host] == "" { legacy[host] = value }
    }
  }
}
contribute("haproxy-ingress.github.io/hsts", "haproxy-ingress.github.io/hsts-max-age", "haproxy-ingress.github.io/hsts-include-subdomains", "haproxy-ingress.github.io/hsts-preload", "15768000")
contribute("haproxy-haptic.org/hsts", "haproxy-haptic.org/hsts-max-age", "haproxy-haptic.org/hsts-include-subdomains", "haproxy-haptic.org/hsts-preload", tostring(extraContext | dig("tls", "hsts", "maxAge") | fallback("31536000")))
contribute("nginx.ingress.kubernetes.io/hsts", "nginx.ingress.kubernetes.io/hsts-max-age", "nginx.ingress.kubernetes.io/hsts-include-subdomains", "nginx.ingress.kubernetes.io/hsts-preload", "15724800")
%%}
{{ "L\n" -}}
{%- for _, host := range keys(legacy) -%}
{{ host }} {{ legacy[host] }}{{ "\n" -}}
{%- end -%}
{%- if tostring(extraContext | dig("failAfterReplay") | fallback(false)) == "true" -%}
  {{- fail("forced failure after HSTS replay") -}}
{%- end -%}`

const hapticAnnotationsWAFPublicationRoot = `{{- render "haptic-waf-ingress-publications" -}}
{%- var incremental = map[string]string{} -%}
{%- for _, value := range incremental_values("haptic-waf-ingresses", "resources") -%}
  {%- var record = value.(map[string]any) -%}
  {%- var annotations = to_str_map(record["annotations"]) -%}
  {%- var id = tostring(record["namespace"]) + "/" + tostring(record["name"]) -%}
  {%- incremental[id] = annotations["haproxy-haptic.org/waf-policy"] -%}
  {%- if tostring(extraContext | dig("failOnPoison") | fallback(false)) == "true" && incremental[id] == "poison" -%}
    {{- fail("forced failure on poison WAF publication") -}}
  {%- end -%}
{%- end -%}
{{ "\nI\n" -}}
{{ "count=" }}{{ len(incremental) }}{{ "\n" -}}
{%- for _, id := range keys(incremental) -%}
  {%- if incremental[id] != "" -%}{{ id }}{{ " " }}{{ incremental[id] }}{{ "\n" -}}{%- end -%}
{%- end -%}
{%%
var legacy = map[string]string{}
var waf = extraContext | dig("waf") | fallback(map[string]any{})
var policies = (waf | dig("policies") | fallback(map[string]any{})).(map[string]any)
var defaultPolicy = tostring(policies | dig("defaultPolicy") | fallback(""))
var allIngresses = tostring(waf | dig("dispatch", "mode") | fallback("opt-in")) == "default-on" || defaultPolicy != ""
for _, ingress := range resources.ingresses.List() {
  var policy = ingress.Metadata.Annotations["haproxy-haptic.org/waf-policy"]
  var mode = ingress.Metadata.Annotations["haproxy-haptic.org/waf-mode"]
  if allIngresses || strip(policy) != "" || strip(mode) != "" {
    legacy[ingress.Metadata.Namespace + "/" + ingress.Metadata.Name] = policy
  }
}
%%}
{{ "L\n" -}}
{{ "count=" }}{{ len(legacy) }}{{ "\n" -}}
{%- for _, id := range keys(legacy) -%}
  {%- if legacy[id] != "" -%}{{ id }}{{ " " }}{{ legacy[id] }}{{ "\n" -}}{%- end -%}
{%- end -%}
{%- if tostring(extraContext | dig("failAfterWAFReplay") | fallback(false)) == "true" -%}
  {{- fail("forced failure after WAF replay") -}}
{%- end -%}`

const hapticAnnotationsAuthHeaderRoot = `{%- import "util-auth-validate-header-name" for ValidateAuthHeaderName -%}
{{- render "features-900-haptic-auth-extra-args-publications" -}}
{%- var incremental = render "spoe-message-check-auth-extra-args-820-haptic" -%}
{{ "I\n" }}{{ incremental }}
{%%
var seen = map[string]bool{
  "authorization": true, "cookie": true, "x_forwarded_for": true,
  "x_forwarded_proto": true, "x_forwarded_host": true, "x_forwarded_uri": true,
}
var legacy = ""
for _, ingress := range resources.ingresses.List() {
  var headers = ingress.Metadata.Annotations["haproxy-haptic.org/auth-headers-request"]
  if headers == "" { continue }
  for _, header := range split(headers, ",") {
    var name = strip(header)
    if name == "" { continue }
    show ValidateAuthHeaderName(name, "haproxy-haptic.org/auth-headers-request", ingress.Metadata.Namespace, ingress.Metadata.Name)
    var argument = replace(toLower(name), "-", "_")
    if !seen[argument] {
      seen[argument] = true
      legacy += " hdr_" + argument + "=req.hdr(" + name + ")"
    }
  }
}
%%}
{{ "\nL\n" }}{{ legacy }}
{%- if tostring(extraContext | dig("failAfterReplay") | fallback(false)) == "true" -%}
  {{- fail("forced failure after auth-header replay") -}}
{%- end -%}`

type hapticAnnotationsHSTSLibrary struct {
	TemplateSnippets map[string]hapticAnnotationsHSTSSnippet `yaml:"templateSnippets"`
}

type hapticAnnotationsHSTSSnippet struct {
	Template    string                            `yaml:"template"`
	Requires    []string                          `yaml:"requires"`
	Incremental *hapticAnnotationsHSTSIncremental `yaml:"incremental"`
}

type hapticAnnotationsHSTSIncremental struct {
	Source            string                     `yaml:"source"`
	BindingsTemplate  string                     `yaml:"bindingsTemplate"`
	WhenAnyPathExists []string                   `yaml:"whenAnyPathExists"`
	Group             string                     `yaml:"group"`
	Effects           []config.IncrementalEffect `yaml:"effects"`
}

type hapticAnnotationsHSTSMetadata struct {
	Namespace   string            `json:"namespace"`
	Name        string            `json:"name"`
	Annotations map[string]string `json:"annotations"`
}

type hapticAnnotationsHSTSIngress struct {
	APIVersion string                           `json:"apiVersion"`
	Kind       string                           `json:"kind"`
	Metadata   hapticAnnotationsHSTSMetadata    `json:"metadata"`
	Spec       hapticAnnotationsHSTSIngressSpec `json:"spec"`
}

type hapticAnnotationsHSTSIngressSpec struct {
	Rules    []hapticAnnotationsHSTSIngressRule `json:"rules"`
	Revision string                             `json:"revision"`
}

type hapticAnnotationsHSTSIngressRule struct {
	Host string `json:"host"`
}

type hapticAnnotationsHSTSFixture struct {
	config    *config.Config
	service   *RenderService
	ingresses *k8sstore.MemoryStore
	provider  stores.StoreProvider
}

func TestHapticAnnotationsHSTSColdDifferentialAndPromotionStayExact(t *testing.T) {
	fixture := newHapticAnnotationsHSTSFixture(t)
	fixture.add(t, hapticAnnotationsHSTSIngressResource("haproxy-owner", map[string]string{
		"haproxy-ingress.github.io/hsts":         "true",
		"haproxy-ingress.github.io/hsts-max-age": "222",
	}, "shared.example", "v1"))
	fixture.add(t, hapticAnnotationsHSTSIngressResource("haptic-owner", map[string]string{
		"haproxy-haptic.org/hsts":         "true",
		"haproxy-haptic.org/hsts-max-age": "222",
	}, "shared.example", "v1"))
	fixture.add(t, hapticAnnotationsHSTSIngressResource("nginx-owner", map[string]string{
		"nginx.ingress.kubernetes.io/hsts":         "true",
		"nginx.ingress.kubernetes.io/hsts-max-age": "333",
	}, "shared.example", "v1"))
	for index := range 24 {
		fixture.add(t, hapticAnnotationsHSTSIngressResource(
			fmt.Sprintf("unrelated-%02d", index), nil, fmt.Sprintf("unrelated-%02d.example", index), "v1",
		))
	}

	first := fixture.renderAndCommit(t)
	fixture.requireDifferential(t, first, "shared.example max-age=222")
	fixture.assertHSTSActivationExecutions(t)

	warm := fixture.renderAndCommit(t)
	assert.Equal(t, first.HAProxyConfig, warm.HAProxyConfig)
	fixture.assertHSTSActivationExecutions(t)

	fixture.config.TemplatingSettings.ExtraContext["poisonRead"] = true
	poisonedView, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.ErrorContains(t, err, "template mutates an immutable input")
	assert.Nil(t, poisonedView)
	fixture.assertHSTSActivationExecutions(t)
	fixture.config.TemplatingSettings.ExtraContext["poisonRead"] = false
	fixture.requireDifferential(t, fixture.renderAndCommit(t), "shared.example max-age=222")
	fixture.assertHSTSActivationExecutions(t)

	fixture.update(t, hapticAnnotationsHSTSIngressResource(
		"unrelated-07", nil, "unrelated-07.example", "v2",
	))
	fixture.requireDifferential(t, fixture.renderAndCommit(t), "shared.example max-age=222")
	for _, componentName := range hapticAnnotationsHSTSComponents {
		assert.Zero(t, fixture.executions(componentName, "unrelated-07"), componentName)
		assert.Zero(t, fixture.executions(componentName, "unrelated-08"), componentName)
		haproxyWant := uint64(0)
		if componentName == "features-155-haproxy-ingress-hsts" {
			haproxyWant = 1
		}
		assert.Equal(t, haproxyWant, fixture.executions(componentName, "haproxy-owner"), componentName)
	}

	assertHapticAnnotationsHSTSOwnerRemoval(t, fixture)
}

func assertHapticAnnotationsHSTSOwnerRemoval(t *testing.T, fixture *hapticAnnotationsHSTSFixture) {
	t.Helper()
	fixture.delete(t, "haproxy-owner")
	fixture.requireDifferential(t, fixture.renderAndCommit(t), "shared.example max-age=222")
	for _, componentName := range hapticAnnotationsHSTSComponents {
		hapticWant := uint64(0)
		nginxWant := uint64(0)
		if componentName == "features-155-haptic-hsts" {
			hapticWant = 1
		}
		if componentName == "features-155-nginx-ingress-hsts" {
			nginxWant = 1
		}
		assert.Equal(t, hapticWant, fixture.executions(componentName, "haptic-owner"), componentName)
		assert.Equal(t, nginxWant, fixture.executions(componentName, "nginx-owner"), componentName)
		component := fixture.service.incremental.components[componentName]
		query := componentQueryKey(&component, "ingresses", "default", "haproxy-owner")
		_, cached := fixture.service.incremental.graph.Value(query)
		assert.False(t, cached, componentName)
		assert.Zero(t, fixture.service.incremental.graph.Counters(query), componentName)
	}

	fixture.delete(t, "haptic-owner")
	fixture.requireDifferential(t, fixture.renderAndCommit(t), "shared.example max-age=333")
	for _, componentName := range hapticAnnotationsHSTSComponents {
		want := uint64(0)
		if componentName == "features-155-nginx-ingress-hsts" {
			want = 1
		}
		assert.Equal(t, want, fixture.executions(componentName, "nginx-owner"), componentName)
	}
}

func TestHapticAnnotationsHSTSFailuresCannotPoisonCommittedState(t *testing.T) {
	fixture := newHapticAnnotationsHSTSFixture(t)
	baselineIngress := hapticAnnotationsHSTSIngressResource("subject", map[string]string{
		"haproxy-haptic.org/hsts":         "true",
		"haproxy-haptic.org/hsts-max-age": "111",
	}, "stable.example", "v1")
	fixture.add(t, baselineIngress)
	baseline := fixture.renderAndCommit(t)
	fixture.requireDifferential(t, baseline, "stable.example max-age=111")
	baselineExecutions := fixture.executions("features-155-haptic-hsts", "subject")

	proposed := hapticAnnotationsHSTSIngressResource("subject", map[string]string{
		"haproxy-haptic.org/hsts":         "true",
		"haproxy-haptic.org/hsts-max-age": "222\npoison.example max-age=0",
	}, "stable.example", "admission")
	overlay := stores.NewOverlayStoreProvider(
		fixture.provider,
		stores.NewValidationContext(map[string]*stores.StoreOverlay{
			"ingresses": stores.NewStoreOverlayForUpdate(&unstructured.Unstructured{Object: proposed}),
		}),
	)
	failedAdmission, err := fixture.service.Render(
		t.Context(), overlay, rendercontext.RenderModeAdmission,
		rendercontext.WithAdmissionSubject("ingresses", "default", "subject"),
	)
	require.ErrorContains(t, err, "config-injection guard")
	assert.Nil(t, failedAdmission)
	assert.Equal(t, baselineExecutions, fixture.executions("features-155-haptic-hsts", "subject"))
	fixture.requireDifferential(t, fixture.renderAndCommit(t), "stable.example max-age=111")

	fixture.update(t, hapticAnnotationsHSTSIngressResource("subject", map[string]string{
		"haproxy-haptic.org/hsts":         "true",
		"haproxy-haptic.org/hsts-max-age": "444",
	}, "stable.example", "v2"))
	fixture.config.TemplatingSettings.ExtraContext["failAfterReplay"] = true
	failedRoot, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.ErrorContains(t, err, "forced failure after HSTS replay")
	assert.Nil(t, failedRoot)
	assert.Equal(t, baselineExecutions, fixture.executions("features-155-haptic-hsts", "subject"))

	fixture.config.TemplatingSettings.ExtraContext["failAfterReplay"] = false
	fixture.requireDifferential(t, fixture.renderAndCommit(t), "stable.example max-age=444")
	assert.Equal(t, baselineExecutions+1, fixture.executions("features-155-haptic-hsts", "subject"))
}

func TestHapticAnnotationsWAFPublicationsStayChangeLocalAndTransactional(t *testing.T) {
	fixture := newHapticAnnotationsWAFPublicationFixture(t)
	fixture.add(t, hapticAnnotationsHSTSIngressResource("subject", map[string]string{
		"haproxy-haptic.org/waf-policy": "stable",
	}, "subject.example", "v1"))
	for index := range 24 {
		fixture.add(t, hapticAnnotationsHSTSIngressResource(
			fmt.Sprintf("unrelated-%02d", index), nil, fmt.Sprintf("unrelated-%02d.example", index), "v1",
		))
	}

	baseline := fixture.renderAndCommit(t)
	fixture.requireDifferential(t, baseline, "count=1\ndefault/subject stable")
	assert.Equal(t, uint64(1), fixture.executions("haptic-waf-ingress-publications", "subject"))
	assert.Equal(t, uint64(1), fixture.executions("haptic-waf-ingress-publications", "unrelated-07"))
	assert.Equal(t, baseline.HAProxyConfig, fixture.renderAndCommit(t).HAProxyConfig)
	assert.Equal(t, uint64(1), fixture.executions("haptic-waf-ingress-publications", "subject"))

	fixture.update(t, hapticAnnotationsHSTSIngressResource(
		"unrelated-07", nil, "unrelated-07.example", "v2",
	))
	fixture.requireDifferential(t, fixture.renderAndCommit(t), "count=1\ndefault/subject stable")
	assert.Equal(t, uint64(1), fixture.executions("haptic-waf-ingress-publications", "subject"))
	assert.Equal(t, uint64(2), fixture.executions("haptic-waf-ingress-publications", "unrelated-07"))
	assert.Equal(t, uint64(1), fixture.executions("haptic-waf-ingress-publications", "unrelated-08"))

	proposed := hapticAnnotationsHSTSIngressResource("subject", map[string]string{
		"haproxy-haptic.org/waf-policy": "poison",
	}, "subject.example", "admission")
	overlay := stores.NewOverlayStoreProvider(
		fixture.provider,
		stores.NewValidationContext(map[string]*stores.StoreOverlay{
			"ingresses": stores.NewStoreOverlayForUpdate(&unstructured.Unstructured{Object: proposed}),
		}),
	)
	fixture.config.TemplatingSettings.ExtraContext["failOnPoison"] = true
	failedAdmission, err := fixture.service.Render(
		t.Context(), overlay, rendercontext.RenderModeAdmission,
		rendercontext.WithAdmissionSubject("ingresses", "default", "subject"),
	)
	require.ErrorContains(t, err, "forced failure on poison WAF publication")
	assert.Nil(t, failedAdmission)
	assert.Equal(t, uint64(1), fixture.executions("haptic-waf-ingress-publications", "subject"))
	fixture.config.TemplatingSettings.ExtraContext["failOnPoison"] = false
	fixture.requireDifferential(t, fixture.renderAndCommit(t), "count=1\ndefault/subject stable")

	fixture.update(t, hapticAnnotationsHSTSIngressResource("subject", map[string]string{
		"haproxy-haptic.org/waf-policy": "updated",
	}, "subject.example", "v2"))
	fixture.config.TemplatingSettings.ExtraContext["failAfterWAFReplay"] = true
	failedRoot, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.ErrorContains(t, err, "forced failure after WAF replay")
	assert.Nil(t, failedRoot)
	assert.Equal(t, uint64(1), fixture.executions("haptic-waf-ingress-publications", "subject"))
	fixture.config.TemplatingSettings.ExtraContext["failAfterWAFReplay"] = false
	fixture.requireDifferential(t, fixture.renderAndCommit(t), "count=1\ndefault/subject updated")
	assert.Equal(t, uint64(2), fixture.executions("haptic-waf-ingress-publications", "subject"))

	fixture.config.TemplatingSettings.ExtraContext["waf"] = map[string]any{
		"dispatch": map[string]any{"mode": "default-on"},
	}
	fixture.requireDifferential(t, fixture.renderAndCommit(t), "count=25\ndefault/subject updated")
	fixture.config.TemplatingSettings.ExtraContext["waf"] = map[string]any{}
	fixture.requireDifferential(t, fixture.renderAndCommit(t), "count=1\ndefault/subject updated")

	fixture.config.TemplatingSettings.ExtraContext["waf"] = map[string]any{
		"policies": map[string]any{
			"defaultPolicy": "baseline",
			"inline":        map[string]any{"baseline": map[string]any{}},
		},
	}
	fixture.requireDifferential(t, fixture.renderAndCommit(t), "count=25\ndefault/subject updated")
	fixture.config.TemplatingSettings.ExtraContext["waf"] = map[string]any{}
	fixture.requireDifferential(t, fixture.renderAndCommit(t), "count=1\ndefault/subject updated")

	fixture.delete(t, "subject")
	fixture.requireDifferential(t, fixture.renderAndCommit(t), "count=0")
	component := fixture.service.incremental.components["haptic-waf-ingress-publications"]
	query := componentQueryKey(&component, "ingresses", "default", "subject")
	_, cached := fixture.service.incremental.graph.Value(query)
	assert.False(t, cached)
	assert.Zero(t, fixture.service.incremental.graph.Counters(query))
}

func TestHapticAnnotationsAuthHeaderPublicationsStayExactAndTransactional(t *testing.T) {
	fixture := newHapticAnnotationsAuthHeaderFixture(t)
	fixture.add(t, hapticAnnotationsHSTSIngressResource("a-owner", map[string]string{
		"haproxy-haptic.org/auth-headers-request": "X-Tenant, X-Shared, Authorization",
	}, "a.example", "v1"))
	fixture.add(t, hapticAnnotationsHSTSIngressResource("b-owner", map[string]string{
		"haproxy-haptic.org/auth-headers-request": "X-Shared, X-Later",
	}, "b.example", "v1"))
	for index := range 24 {
		fixture.add(t, hapticAnnotationsHSTSIngressResource(
			fmt.Sprintf("unrelated-%02d", index), nil, fmt.Sprintf("unrelated-%02d.example", index), "v1",
		))
	}

	baseline := fixture.renderAndCommit(t)
	requireHapticAnnotationsAuthHeaderDifferential(t, baseline,
		"hdr_x_tenant=req.hdr(X-Tenant)",
		"hdr_x_shared=req.hdr(X-Shared)",
		"hdr_x_later=req.hdr(X-Later)",
	)
	component := "features-900-haptic-auth-extra-args-publications"
	assert.Equal(t, uint64(1), fixture.executions(component, "a-owner"))
	assert.Equal(t, uint64(1), fixture.executions(component, "b-owner"))
	assert.Equal(t, baseline.HAProxyConfig, fixture.renderAndCommit(t).HAProxyConfig)
	assert.Equal(t, uint64(1), fixture.executions(component, "a-owner"))

	fixture.update(t, hapticAnnotationsHSTSIngressResource(
		"unrelated-07", nil, "unrelated-07.example", "v2",
	))
	requireHapticAnnotationsAuthHeaderDifferential(t, fixture.renderAndCommit(t),
		"hdr_x_tenant=req.hdr(X-Tenant)",
		"hdr_x_shared=req.hdr(X-Shared)",
		"hdr_x_later=req.hdr(X-Later)",
	)
	assert.Equal(t, uint64(1), fixture.executions(component, "a-owner"))
	assert.Equal(t, uint64(1), fixture.executions(component, "b-owner"))
	assert.Zero(t, fixture.executions(component, "unrelated-07"))
	assert.Zero(t, fixture.executions(component, "unrelated-08"))

	invalid := hapticAnnotationsHSTSIngressResource("a-owner", map[string]string{
		"haproxy-haptic.org/auth-headers-request": "X-Stable, Invalid Header",
	}, "a.example", "admission")
	overlay := stores.NewOverlayStoreProvider(
		fixture.provider,
		stores.NewValidationContext(map[string]*stores.StoreOverlay{
			"ingresses": stores.NewStoreOverlayForUpdate(&unstructured.Unstructured{Object: invalid}),
		}),
	)
	failedAdmission, err := fixture.service.Render(
		t.Context(), overlay, rendercontext.RenderModeAdmission,
		rendercontext.WithAdmissionSubject("ingresses", "default", "a-owner"),
	)
	require.ErrorContains(t, err, "Invalid HTTP header name 'Invalid Header'")
	assert.Nil(t, failedAdmission)
	assert.Equal(t, uint64(1), fixture.executions(component, "a-owner"))
	assert.Equal(t, baseline.HAProxyConfig, fixture.renderAndCommit(t).HAProxyConfig)

	fixture.update(t, hapticAnnotationsHSTSIngressResource("a-owner", map[string]string{
		"haproxy-haptic.org/auth-headers-request": "X-Tenant-V2, X-Shared",
	}, "a.example", "v2"))
	fixture.config.TemplatingSettings.ExtraContext["failAfterReplay"] = true
	failedRoot, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.ErrorContains(t, err, "forced failure after auth-header replay")
	assert.Nil(t, failedRoot)
	assert.Equal(t, uint64(1), fixture.executions(component, "a-owner"))
	fixture.config.TemplatingSettings.ExtraContext["failAfterReplay"] = false
	requireHapticAnnotationsAuthHeaderDifferential(t, fixture.renderAndCommit(t),
		"hdr_x_tenant_v2=req.hdr(X-Tenant-V2)",
		"hdr_x_shared=req.hdr(X-Shared)",
		"hdr_x_later=req.hdr(X-Later)",
	)
	assert.Equal(t, uint64(2), fixture.executions(component, "a-owner"))

	fixture.delete(t, "a-owner")
	requireHapticAnnotationsAuthHeaderDifferential(t, fixture.renderAndCommit(t),
		"hdr_x_shared=req.hdr(X-Shared)",
		"hdr_x_later=req.hdr(X-Later)",
	)
	assert.Equal(t, uint64(1), fixture.executions(component, "b-owner"))
}

func newHapticAnnotationsHSTSFixture(t *testing.T) *hapticAnnotationsHSTSFixture {
	t.Helper()
	cfg := &config.Config{
		Dataplane: testDataplaneConfig(),
		TemplatingSettings: config.TemplatingSettings{ExtraContext: map[string]any{
			"tls":             map[string]any{"hsts": map[string]any{"maxAge": "31536000"}},
			"poisonRead":      false,
			"failAfterReplay": false,
		}},
		WatchedResources: map[string]config.WatchedResource{
			"ingresses": {
				APIVersion: "networking.k8s.io/v1", Resources: "ingresses",
				IndexBy: []string{"metadata.namespace", "metadata.name"},
			},
		},
		TemplateSnippets: loadHapticAnnotationsHSTSSnippets(t),
		HAProxyConfig:    config.HAProxyConfig{Template: hapticAnnotationsHSTSRoot},
	}
	types := &typebootstrap.Result{
		Types: map[string]reflect.Type{
			"ingresses": reflect.TypeOf(hapticAnnotationsHSTSIngress{}),
		},
		Kinds:  map[string]string{"ingresses": "Ingress"},
		Errors: map[string]error{},
	}
	declarations := helpers.BuildAdditionalDeclarations(cfg, types)
	engine, err := helpers.NewEngineFromConfigWithOptions(
		cfg, nil, nil, declarations, helpers.EngineOptions{},
	)
	require.NoError(t, err)
	service := NewRenderService(&RenderServiceConfig{
		Engine: engine, Config: cfg, Logger: slog.Default(), Capabilities: defaultCapabilities(),
		TypedResourceTypes: types.Types,
	})
	ingresses := k8sstore.NewMemoryStore(2)
	return &hapticAnnotationsHSTSFixture{
		config: cfg, service: service, ingresses: ingresses,
		provider: stores.NewRealStoreProvider(map[string]stores.Store{"ingresses": ingresses}),
	}
}

func newHapticAnnotationsWAFPublicationFixture(t *testing.T) *hapticAnnotationsHSTSFixture {
	t.Helper()
	cfg := &config.Config{
		Dataplane: testDataplaneConfig(),
		TemplatingSettings: config.TemplatingSettings{ExtraContext: map[string]any{
			"failOnPoison":       false,
			"failAfterWAFReplay": false,
		}},
		WatchedResources: map[string]config.WatchedResource{
			"ingresses": {
				APIVersion: "networking.k8s.io/v1", Resources: "ingresses",
				IndexBy: []string{"metadata.namespace", "metadata.name"},
			},
		},
		TemplateSnippets: loadHapticAnnotationsWAFPublicationSnippet(t),
		HAProxyConfig:    config.HAProxyConfig{Template: hapticAnnotationsWAFPublicationRoot},
	}
	types := &typebootstrap.Result{
		Types: map[string]reflect.Type{
			"ingresses": reflect.TypeOf(hapticAnnotationsHSTSIngress{}),
		},
		Kinds:  map[string]string{"ingresses": "Ingress"},
		Errors: map[string]error{},
	}
	declarations := helpers.BuildAdditionalDeclarations(cfg, types)
	engine, err := helpers.NewEngineFromConfigWithOptions(
		cfg, nil, nil, declarations, helpers.EngineOptions{},
	)
	require.NoError(t, err)
	service := NewRenderService(&RenderServiceConfig{
		Engine: engine, Config: cfg, Logger: slog.Default(), Capabilities: defaultCapabilities(),
		TypedResourceTypes: types.Types,
	})
	ingresses := k8sstore.NewMemoryStore(2)
	return &hapticAnnotationsHSTSFixture{
		config: cfg, service: service, ingresses: ingresses,
		provider: stores.NewRealStoreProvider(map[string]stores.Store{"ingresses": ingresses}),
	}
}

func newHapticAnnotationsAuthHeaderFixture(t *testing.T) *hapticAnnotationsHSTSFixture {
	t.Helper()
	cfg := &config.Config{
		Dataplane: testDataplaneConfig(),
		TemplatingSettings: config.TemplatingSettings{ExtraContext: map[string]any{
			"failAfterReplay": false,
		}},
		WatchedResources: map[string]config.WatchedResource{
			"ingresses": {
				APIVersion: "networking.k8s.io/v1", Resources: "ingresses",
				IndexBy: []string{"metadata.namespace", "metadata.name"},
			},
		},
		TemplateSnippets: loadHapticAnnotationsAuthHeaderSnippets(t),
		HAProxyConfig:    config.HAProxyConfig{Template: hapticAnnotationsAuthHeaderRoot},
	}
	types := &typebootstrap.Result{
		Types: map[string]reflect.Type{
			"ingresses": reflect.TypeOf(hapticAnnotationsHSTSIngress{}),
		},
		Kinds:  map[string]string{"ingresses": "Ingress"},
		Errors: map[string]error{},
	}
	declarations := helpers.BuildAdditionalDeclarations(cfg, types)
	engine, err := helpers.NewEngineFromConfigWithOptions(
		cfg, nil, nil, declarations, helpers.EngineOptions{},
	)
	require.NoError(t, err)
	service := NewRenderService(&RenderServiceConfig{
		Engine: engine, Config: cfg, Logger: slog.Default(), Capabilities: defaultCapabilities(),
		TypedResourceTypes: types.Types,
	})
	ingresses := k8sstore.NewMemoryStore(2)
	return &hapticAnnotationsHSTSFixture{
		config: cfg, service: service, ingresses: ingresses,
		provider: stores.NewRealStoreProvider(map[string]stores.Store{"ingresses": ingresses}),
	}
}

func loadHapticAnnotationsHSTSSnippets(t *testing.T) map[string]config.TemplateSnippet {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	chartRoot := filepath.Join(filepath.Dir(sourceFile), "..", "..", "..", "charts", "haptic", "charts")
	wanted := map[string]bool{"util-register-annotation-hsts": true}
	for _, name := range hapticAnnotationsHSTSComponents {
		wanted[name] = true
	}
	result := make(map[string]config.TemplateSnippet, len(wanted))
	for _, file := range []string{
		"ingress-annotations-compat/library.yaml",
		"haproxy-ingress/40-features.yaml",
		"haptic-annotations/40-features.yaml",
		"nginx-ingress/30-features.yaml",
	} {
		content, err := os.ReadFile(filepath.Join(chartRoot, file))
		require.NoError(t, err)
		var library hapticAnnotationsHSTSLibrary
		require.NoError(t, yaml.Unmarshal(content, &library))
		for name, chartSnippet := range library.TemplateSnippets {
			if !wanted[name] {
				continue
			}
			snippet := config.TemplateSnippet{
				Name: name, Template: chartSnippet.Template, Requires: chartSnippet.Requires,
			}
			if chartSnippet.Incremental != nil {
				snippet.Incremental = &config.IncrementalTemplate{
					Source:            chartSnippet.Incremental.Source,
					BindingsTemplate:  chartSnippet.Incremental.BindingsTemplate,
					WhenAnyPathExists: chartSnippet.Incremental.WhenAnyPathExists,
					Group:             chartSnippet.Incremental.Group,
					Effects:           chartSnippet.Incremental.Effects,
				}
			}
			result[name] = snippet
		}
	}
	require.Len(t, result, len(wanted))
	return result
}

func loadHapticAnnotationsWAFPublicationSnippet(t *testing.T) map[string]config.TemplateSnippet {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	chartRoot := filepath.Join(filepath.Dir(sourceFile), "..", "..", "..", "charts", "haptic", "charts")
	wanted := map[string]bool{
		"haptic-waf-ingress-publications": true,
		"util-waf-governance":             true,
		"util-policy-waf-governance":      true,
	}
	result := make(map[string]config.TemplateSnippet, len(wanted))
	for _, path := range []string{
		"haptic-annotations/83-waf-policies.yaml",
		"http-policies/81-waf-governance.yaml",
	} {
		content, err := os.ReadFile(filepath.Join(chartRoot, path))
		require.NoError(t, err)
		var library hapticAnnotationsHSTSLibrary
		require.NoError(t, yaml.Unmarshal(content, &library))
		for name, chartSnippet := range library.TemplateSnippets {
			if !wanted[name] {
				continue
			}
			snippet := config.TemplateSnippet{
				Name: name, Template: chartSnippet.Template, Requires: chartSnippet.Requires,
			}
			if chartSnippet.Incremental != nil {
				snippet.Incremental = &config.IncrementalTemplate{
					Source:            chartSnippet.Incremental.Source,
					BindingsTemplate:  chartSnippet.Incremental.BindingsTemplate,
					WhenAnyPathExists: chartSnippet.Incremental.WhenAnyPathExists,
					Group:             chartSnippet.Incremental.Group,
					Effects:           chartSnippet.Incremental.Effects,
				}
			}
			result[name] = snippet
		}
	}
	require.Len(t, result, len(wanted))
	return result
}

func loadHapticAnnotationsAuthHeaderSnippets(t *testing.T) map[string]config.TemplateSnippet {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	chartRoot := filepath.Join(filepath.Dir(sourceFile), "..", "..", "..", "charts", "haptic", "charts")
	wanted := map[string]bool{
		"util-webhook-reject-or-warn":                      true,
		"util-auth-validate-header-name":                   true,
		"features-900-haptic-auth-extra-args-publications": true,
		"spoe-message-check-auth-extra-args-820-haptic":    true,
	}
	result := make(map[string]config.TemplateSnippet, len(wanted))
	for _, file := range []string{
		"base/library.yaml", "ingress-annotations-compat/library.yaml", "haptic-annotations/50-auth-spoe.yaml",
	} {
		content, err := os.ReadFile(filepath.Join(chartRoot, file))
		require.NoError(t, err)
		var library hapticAnnotationsHSTSLibrary
		require.NoError(t, yaml.Unmarshal(content, &library))
		for name, chartSnippet := range library.TemplateSnippets {
			if !wanted[name] {
				continue
			}
			snippet := config.TemplateSnippet{
				Name: name, Template: chartSnippet.Template, Requires: chartSnippet.Requires,
			}
			if chartSnippet.Incremental != nil {
				snippet.Incremental = &config.IncrementalTemplate{
					Source:            chartSnippet.Incremental.Source,
					WhenAnyPathExists: chartSnippet.Incremental.WhenAnyPathExists,
					Group:             chartSnippet.Incremental.Group,
					Effects:           chartSnippet.Incremental.Effects,
				}
			}
			result[name] = snippet
		}
	}
	require.Len(t, result, len(wanted))
	return result
}

func hapticAnnotationsHSTSIngressResource(
	name string,
	annotations map[string]string,
	host string,
	revision string,
) map[string]any {
	annotationValues := make(map[string]any, len(annotations))
	for key, value := range annotations {
		annotationValues[key] = value
	}
	return map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "Ingress",
		"metadata": map[string]any{
			"namespace": "default", "name": name, "annotations": annotationValues,
		},
		"spec": map[string]any{
			"rules": []any{map[string]any{"host": host}}, "revision": revision,
		},
	}
}

func (f *hapticAnnotationsHSTSFixture) add(t *testing.T, ingress map[string]any) {
	t.Helper()
	name := ingress["metadata"].(map[string]any)["name"].(string)
	require.NoError(t, f.ingresses.Add(ingress, []string{"default", name}))
}

func (f *hapticAnnotationsHSTSFixture) update(t *testing.T, ingress map[string]any) {
	t.Helper()
	name := ingress["metadata"].(map[string]any)["name"].(string)
	require.NoError(t, f.ingresses.Update(ingress, []string{"default", name}))
}

func (f *hapticAnnotationsHSTSFixture) delete(t *testing.T, name string) {
	t.Helper()
	require.NoError(t, f.ingresses.Delete("default", name, []string{"default", name}))
}

func (f *hapticAnnotationsHSTSFixture) renderAndCommit(t *testing.T) *RenderResult {
	t.Helper()
	result, err := f.service.Render(t.Context(), f.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NotNil(t, result.InputTransaction)
	require.NoError(t, result.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, f.service)
	return result
}

func (f *hapticAnnotationsHSTSFixture) executions(componentName, ingress string) uint64 {
	component := f.service.incremental.components[componentName]
	query := componentQueryKey(&component, "ingresses", "default", ingress)
	return f.service.incremental.graph.Counters(query).Executions
}

func (f *hapticAnnotationsHSTSFixture) assertHSTSActivationExecutions(t *testing.T) {
	t.Helper()
	const activeExecutions = uint64(1)
	for _, componentName := range hapticAnnotationsHSTSComponents {
		for _, ingress := range []string{"haproxy-owner", "haptic-owner", "nginx-owner", "unrelated-00", "unrelated-23"} {
			expected := uint64(0)
			if componentName == "features-155-haproxy-ingress-hsts" && ingress == "haproxy-owner" ||
				componentName == "features-155-haptic-hsts" && ingress == "haptic-owner" ||
				componentName == "features-155-nginx-ingress-hsts" && ingress == "nginx-owner" {
				expected = activeExecutions
			}
			assert.Equal(t, expected, f.executions(componentName, ingress), componentName+"/"+ingress)
		}
	}
}

func (f *hapticAnnotationsHSTSFixture) requireDifferential(t *testing.T, result *RenderResult, expected string) {
	t.Helper()
	incrementalOutput, legacyOutput := splitHapticAnnotationsHSTSOutput(t, result.HAProxyConfig)
	assert.Equal(t, legacyOutput, incrementalOutput)
	assert.Equal(t, expected, incrementalOutput)
}

func splitHapticAnnotationsHSTSOutput(t *testing.T, output string) (incrementalOutput, legacyOutput string) {
	t.Helper()
	trimmed := strings.TrimSpace(output)
	require.True(t, strings.HasPrefix(trimmed, "I\n"), trimmed)
	parts := strings.Split(strings.TrimPrefix(trimmed, "I\n"), "\nL\n")
	require.Len(t, parts, 2, trimmed)
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
}

func requireHapticAnnotationsAuthHeaderDifferential(t *testing.T, result *RenderResult, ordered ...string) {
	t.Helper()
	trimmed := strings.TrimSpace(result.HAProxyConfig)
	require.True(t, strings.HasPrefix(trimmed, "I\n"), trimmed)
	parts := strings.Split(strings.TrimPrefix(trimmed, "I\n"), "\nL\n")
	require.Len(t, parts, 2, trimmed)
	incremental := strings.TrimSuffix(parts[0], "\n")
	legacy := strings.TrimSuffix(parts[1], "\n")
	assert.Equal(t, legacy, incremental)
	position := -1
	for _, fragment := range ordered {
		next := strings.Index(incremental, fragment)
		assert.Greater(t, next, position, fragment)
		position = next
	}
}
