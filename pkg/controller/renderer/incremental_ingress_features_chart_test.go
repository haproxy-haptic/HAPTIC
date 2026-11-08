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
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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

const ingressFeatureChartRoot = `{%- import "util-backend-name-ingress" for BackendNameIngress -%}
{%- var _, _ = shared.ComputeIfAbsent("globalFeatures", func() any {
  return map[string]any{
    "tlsCertificates": []any{}, "sslRedirectHosts": []any{},
    "ingReqHeaderMods": []any{}, "ingRespHeaderMods": []any{},
  }
}) -%}
{{- render "features-100-ingress-tls" -}}
{{- render "features-105-ingress-ssl-redirect" -}}
{{- render "features-301-ingress-header-mods" -}}
{%- var gf = shared.Get("globalFeatures").(map[string]any) -%}
{{ "BEGIN-I\n" -}}
{%- for _, certAny := range toSlice(gf["tlsCertificates"]) -%}
  {%- var cert = certAny.(map[string]any) -%}
{{ "T|" + tostring(cert["secret_namespace"]) + "/" + tostring(cert["secret_name"]) + "|" + join(cert["sni_patterns"].([]any), ",") + "\n" -}}
{%- end -%}
{%- for _, redirectAny := range toSlice(gf["sslRedirectHosts"]) -%}
  {%- var redirect = redirectAny.(map[string]any) -%}
{{ "R|" + tostring(redirect["host"]) + "|" + tostring(redirect["code"]) + "\n" -}}
{%- end -%}
{%- for _, headerAny := range toSlice(gf["ingReqHeaderMods"]) -%}
  {%- var header = headerAny.(map[string]any) -%}
{{ "Q|" + tostring(header["backend"]) + "|" + tostring(header["op"]) + "|" + tostring(header["name"]) + "|" + tostring(header["value"]) + "\n" -}}
{%- end -%}
{%- for _, headerAny := range toSlice(gf["ingRespHeaderMods"]) -%}
  {%- var header = headerAny.(map[string]any) -%}
{{ "P|" + tostring(header["backend"]) + "|" + tostring(header["op"]) + "|" + tostring(header["name"]) + "|" + tostring(header["value"]) + "\n" -}}
{%- end -%}
{{ "END-I\nBEGIN-L\n" -}}
{%%
var seenSecrets = map[string]bool{}
for _, ingress := range resources.ingresses.List() {
  for _, tls := range ingress.Spec.Tls {
    var secretName = tls.SecretName
    var secretKey = ingress.Metadata.Namespace + "/" + secretName
    if secretName == "" || seenSecrets[secretKey] { continue }
    var secret = resources.secrets.GetSingle(ingress.Metadata.Namespace, secretName)
    if secret == nil { continue }
    var _, hasCrt = secret.Data["tls.crt"]
    var _, hasKey = secret.Data["tls.key"]
    if !hasCrt || !hasKey { continue }
    seenSecrets[secretKey] = true
    show "T|" + secretKey + "|" + join(tls.Hosts | toSlice(), ",") + "\n"
  }
}
var redirectAll = extraContext | dig("ingressDefaultSSLRedirect") | fallback(false)
if redirectAll.(bool) {
  var code = extraContext | dig("ingressDefaultSSLRedirectCode") | fallback("308") | tostring()
  var tlsDefault = extraContext | dig("ingressDefaultHTTPS") | fallback(true)
  var haveDefaultCert = tostring(extraContext | dig("tls", "defaultCertificate", "name") | fallback("")) != ""
  var httpsByDefault = tlsDefault.(bool) && haveDefaultCert
  for _, ingress := range resources.ingresses.List() {
    var tlsHosts = map[string]bool{}
    for _, tls := range ingress.Spec.Tls {
      for _, host := range tls.Hosts { tlsHosts[toLower(host)] = true }
    }
    for _, rule := range ingress.Spec.Rules {
      var host = toLower(rule.Host)
      if host != "" && (httpsByDefault || tlsHosts[host]) {
        show "R|" + host + "|" + code + "\n"
      }
    }
  }
}
var headerSeen = map[string]bool{}
var legacyHeaders = []string{}
var contribute = func(reqAnn string, respAnn string, recordSep string, kvSep string) {
  for _, ingress := range resources.ingresses.List() {
    var contributeCell = func(ann string, bucket string, marker string) {
      if ann == "" { return }
      var annVal = ingress.Metadata.Annotations[ann]
      if annVal == "" { return }
      for _, record := range split(annVal, recordSep) {
        var text = trimSpace(record)
        if text == "" || !strings_contains(text, kvSep) { continue }
        var parts = split(text, kvSep)
        var name = trimSpace(parts[0])
        var value = trimSpace(parts[1:] | join(kvSep))
        if name == "" || !regex_search(name, "^[A-Za-z0-9!$&*+.^_~-]+$") { continue }
        if regex_search(value, "[[:cntrl:]]") { continue }
        for _, rule := range ingress.Spec.Rules {
          for _, path := range rule.Http.Paths {
            var backend = BackendNameIngress(ingress, path)
            var identity = bucket + "\x00" + backend + "\x00" + toLower(name)
            if headerSeen[identity] { continue }
            headerSeen[identity] = true
            legacyHeaders = append(legacyHeaders, marker + "|" + backend + "|set|" + name + "|" + value + "\n")
          }
        }
      }
    }
    contributeCell(reqAnn, "ingReqHeaderMods", "Q")
    contributeCell(respAnn, "ingRespHeaderMods", "P")
  }
}
contribute("haproxy.org/request-set-header", "haproxy.org/response-set-header", "\n", " ")
contribute("haproxy-ingress.github.io/headers", "", "|", ":")
contribute("nginx.ingress.kubernetes.io/custom-request-headers", "nginx.ingress.kubernetes.io/custom-response-headers", "|", ":")
contribute("haproxy-haptic.org/request-set-header", "haproxy-haptic.org/response-set-header", "\n", " ")
for _, ingress := range resources.ingresses.List() {
  var contributeRemovals = func(ann string, bucket string, marker string) {
    var annVal = ingress.Metadata.Annotations[ann]
    if annVal == "" { return }
    for _, record := range split(replace(annVal, "\n", ","), ",") {
      var tok = trimSpace(record)
      if tok == "" { continue }
      var op = "del"
      var name = tok
      if hasSuffix(tok, "*") {
        op = "del-beg"
        name = tok[:len(tok)-1]
      }
      if name == "" || !regex_search(name, "^[A-Za-z0-9!$&+.^_~-]+$") { continue }
      for _, rule := range ingress.Spec.Rules {
        for _, path := range rule.Http.Paths {
          var backend = BackendNameIngress(ingress, path)
          var identity = bucket + "\x00" + backend + "\x00" + op + "\x00" + toLower(name)
          if headerSeen[identity] { continue }
          headerSeen[identity] = true
          legacyHeaders = append(legacyHeaders, marker + "|" + backend + "|" + op + "|" + name + "|\n")
        }
      }
    }
  }
  contributeRemovals("haproxy-haptic.org/request-remove-header", "ingReqHeaderMods", "Q")
  contributeRemovals("haproxy-haptic.org/response-remove-header", "ingRespHeaderMods", "P")
}
for _, line := range legacyHeaders { show line }
%%}
{{ "END-L\n" -}}
{%- if tostring(extraContext | dig("poisonRead") | fallback(false)) == "true" -%}
  {%- var values = incremental_values("ingress-header-mods", "ingReqHeaderMods") -%}
  {%- if len(values) > 0 -%}{%- values[0].(map[string]any)["value"] = "poison" -%}{%- end -%}
{%- end -%}
{%- if tostring(extraContext | dig("failAfterReplay") | fallback(false)) == "true" -%}
  {{- fail("forced failure after Ingress feature replay") -}}
{%- end -%}`

const (
	ingressTLSFeatureComponent      = "ingress-tls-certificate-publications"
	ingressRedirectFeatureComponent = "ingress-default-ssl-redirect-publications"
	ingressHapticHeaderComponent    = "ingress-header-mods-0820-haptic"
)

type ingressFeatureChartFixture struct {
	config    *config.Config
	service   *RenderService
	engine    *dynamicBindingCountingEngine
	ingresses *k8sstore.MemoryStore
	services  *k8sstore.MemoryStore
	secrets   *k8sstore.MemoryStore
	provider  stores.StoreProvider
}

type ingressFeatureSnapshot struct {
	config       string
	certificates map[string]string
}

func TestIngressFeatureComponentsStayConstantAcrossScale(t *testing.T) {
	var expectedChangedExecutions int
	for _, count := range []int{300, 1000, 3000} {
		t.Run(fmt.Sprintf("ingresses=%d", count), func(t *testing.T) {
			fixture := newIngressFeatureChartFixture(t)
			fixture.addSecret(t, ingressFeatureSecret("shared", "certificate-a", "key-a"))
			for index := range count {
				name := fmt.Sprintf("route-%06d", index)
				fixture.addIngress(t, ingressFeatureIngress(name, name+".example.com", map[string]string{
					"haproxy-haptic.org/request-set-header": "X-Scale " + name,
				}))
			}

			started := time.Now()
			cold := fixture.renderAndCommit(t)
			requireIngressFeatureDifferential(t, cold)
			coldCounts := fixture.engine.executionCounts()
			for index := range count {
				assert.Equal(t, 3, coldCounts[fmt.Sprintf("ingresses/route-%06d", index)])
			}
			coldDuration := time.Since(started)

			beforeWarm := fixture.engine.executionCounts()
			started = time.Now()
			warm := fixture.renderAndCommit(t)
			require.Equal(t, ingressFeatureResultSnapshot(t, cold), ingressFeatureResultSnapshot(t, warm))
			require.Equal(t, beforeWarm, fixture.engine.executionCounts())
			warmDuration := time.Since(started)

			changedName := "route-000000"
			fixture.updateIngress(t, ingressFeatureIngress(changedName, "changed.example.com", map[string]string{
				"haproxy-haptic.org/request-set-header": "X-Scale changed",
			}))
			beforeChanged := fixture.engine.executionCounts()
			started = time.Now()
			changed := fixture.renderAndCommit(t)
			requireIngressFeatureDifferential(t, changed)
			afterChanged := fixture.engine.executionCounts()
			changedExecutions := afterChanged["ingresses/"+changedName] - beforeChanged["ingresses/"+changedName]
			if expectedChangedExecutions == 0 {
				expectedChangedExecutions = changedExecutions
			}
			require.Equal(t, expectedChangedExecutions, changedExecutions)
			require.Equal(t, 3, changedExecutions)
			for index := 1; index < count; index++ {
				name := fmt.Sprintf("ingresses/route-%06d", index)
				require.Equal(t, beforeChanged[name], afterChanged[name], name)
			}
			changedDuration := time.Since(started)

			requireIngressFeatureColdOracleMatches(t, count, changed)
			t.Logf("ingresses=%d cold=%s warm=%s one-change=%s component-executions=%d",
				count, coldDuration, warmDuration, changedDuration, changedExecutions)
		})
	}
}

func requireIngressFeatureColdOracleMatches(t *testing.T, count int, changed *RenderResult) {
	t.Helper()
	fresh := newIngressFeatureChartFixture(t)
	fresh.addSecret(t, ingressFeatureSecret("shared", "certificate-a", "key-a"))
	for index := range count {
		name := fmt.Sprintf("route-%06d", index)
		host := name + ".example.com"
		value := name
		if index == 0 {
			host = "changed.example.com"
			value = "changed"
		}
		fresh.addIngress(t, ingressFeatureIngress(name, host, map[string]string{
			"haproxy-haptic.org/request-set-header": "X-Scale " + value,
		}))
	}
	freshChanged := fresh.renderAndCommit(t)
	require.Equal(t, ingressFeatureResultSnapshot(t, freshChanged), ingressFeatureResultSnapshot(t, changed))
}

func TestIngressTLSExactSecretDependenciesAndWinnerPromotion(t *testing.T) {
	fixture := newIngressFeatureChartFixture(t)
	fixture.addSecret(t, ingressFeatureSecret("shared", "certificate-a", "key-a"))
	fixture.addSecret(t, ingressFeatureSecret("unrelated", "ignored", "ignored"))
	fixture.addIngress(t, ingressFeatureIngress("a", "a.example.com", nil))
	fixture.addIngress(t, ingressFeatureIngress("b", "b.example.com", nil))

	first := fixture.renderAndCommit(t)
	requireIngressFeatureDifferential(t, first)
	assert.Contains(t, first.HAProxyConfig, "T|default/shared|a.example.com")
	assert.NotContains(t, first.HAProxyConfig, "T|default/shared|b.example.com")
	assert.Equal(t, "certificate-a\nkey-a", ingressFeatureCertificate(t, first, "default_shared.pem"))

	beforeUnrelated := fixture.engine.executionCounts()
	fixture.updateSecret(t, ingressFeatureSecret("unrelated", "changed", "changed"))
	unrelated := fixture.renderAndCommit(t)
	require.Equal(t, ingressFeatureResultSnapshot(t, first), ingressFeatureResultSnapshot(t, unrelated))
	require.Equal(t, beforeUnrelated, fixture.engine.executionCounts())

	beforeShared := fixture.engine.executionCounts()
	fixture.updateSecret(t, ingressFeatureSecret("shared", "certificate-b", "key-b"))
	shared := fixture.renderAndCommit(t)
	assert.Equal(t, "certificate-b\nkey-b", ingressFeatureCertificate(t, shared, "default_shared.pem"))
	assert.Equal(t, beforeShared["ingresses/a"]+1, fixture.engine.executionCounts()["ingresses/a"])
	assert.Equal(t, beforeShared["ingresses/b"]+1, fixture.engine.executionCounts()["ingresses/b"])

	fixture.deleteIngress(t, "a")
	promoted := fixture.renderAndCommit(t)
	requireIngressFeatureDifferential(t, promoted)
	assert.NotContains(t, promoted.HAProxyConfig, "T|default/shared|a.example.com")
	assert.Contains(t, promoted.HAProxyConfig, "T|default/shared|b.example.com")
}

func TestIngressHeaderWinnerPromotionAndRemovalStayExact(t *testing.T) {
	fixture := newIngressFeatureChartFixture(t)
	fixture.addSecret(t, ingressFeatureSecret("shared", "certificate", "key"))
	annotations := map[string]string{
		"haproxy.org/request-set-header":                     "X-Winner haproxytech",
		"haproxy-ingress.github.io/headers":                  "X-Winner:haproxy-ingress",
		"nginx.ingress.kubernetes.io/custom-request-headers": "X-Winner:nginx",
		"haproxy-haptic.org/request-set-header":              "X-Winner haptic",
	}
	fixture.addIngress(t, ingressFeatureIngress("subject", "subject.example.com", annotations))

	result := fixture.renderAndCommit(t)
	requireIngressFeatureDifferential(t, result)
	assert.Contains(t, result.HAProxyConfig, "|X-Winner|haproxytech")

	delete(annotations, "haproxy.org/request-set-header")
	fixture.updateIngress(t, ingressFeatureIngress("subject", "subject.example.com", annotations))
	result = fixture.renderAndCommit(t)
	requireIngressFeatureDifferential(t, result)
	assert.Contains(t, result.HAProxyConfig, "|X-Winner|haproxy-ingress")

	delete(annotations, "haproxy-ingress.github.io/headers")
	fixture.updateIngress(t, ingressFeatureIngress("subject", "subject.example.com", annotations))
	result = fixture.renderAndCommit(t)
	requireIngressFeatureDifferential(t, result)
	assert.Contains(t, result.HAProxyConfig, "|X-Winner|nginx")

	delete(annotations, "nginx.ingress.kubernetes.io/custom-request-headers")
	fixture.updateIngress(t, ingressFeatureIngress("subject", "subject.example.com", annotations))
	result = fixture.renderAndCommit(t)
	requireIngressFeatureDifferential(t, result)
	assert.Contains(t, result.HAProxyConfig, "|X-Winner|haptic")

	delete(annotations, "haproxy-haptic.org/request-set-header")
	fixture.updateIngress(t, ingressFeatureIngress("subject", "subject.example.com", annotations))
	result = fixture.renderAndCommit(t)
	requireIngressFeatureDifferential(t, result)
	assert.NotContains(t, result.HAProxyConfig, "|X-Winner|")

	annotations["haproxy-haptic.org/request-remove-header"] = "X-Internal, x-debug-*"
	annotations["haproxy-haptic.org/response-remove-header"] = "Server\nx-amz-*"
	fixture.updateIngress(t, ingressFeatureIngress("subject", "subject.example.com", annotations))
	result = fixture.renderAndCommit(t)
	requireIngressFeatureDifferential(t, result)
	assert.Contains(t, result.HAProxyConfig, "|del|X-Internal|")
	assert.Contains(t, result.HAProxyConfig, "|del-beg|x-debug-|")
	assert.Contains(t, result.HAProxyConfig, "|del|Server|")
	assert.Contains(t, result.HAProxyConfig, "|del-beg|x-amz-|")
}

func TestIngressFeatureFailedRendersAndAdmissionCannotPoisonCache(t *testing.T) {
	fixture := newIngressFeatureChartFixture(t)
	fixture.addSecret(t, ingressFeatureSecret("shared", "certificate", "key"))
	baselineResource := ingressFeatureIngress("subject", "baseline.example.com", map[string]string{
		"haproxy-haptic.org/request-set-header": "X-Test baseline",
	})
	fixture.addIngress(t, baselineResource)
	baseline := fixture.renderAndCommit(t)
	requireIngressFeatureDifferential(t, baseline)

	fixture.config.TemplatingSettings.ExtraContext["poisonRead"] = true
	poisoned, err := fixture.render(rendercontext.RenderModeReconcile)
	require.ErrorContains(t, err, "template mutates an immutable input")
	assert.Nil(t, poisoned)
	fixture.config.TemplatingSettings.ExtraContext["poisonRead"] = false
	beforeWarm := fixture.engine.executionCounts()
	afterPoison := fixture.renderAndCommit(t)
	require.Equal(t, ingressFeatureResultSnapshot(t, baseline), ingressFeatureResultSnapshot(t, afterPoison))
	require.Equal(t, beforeWarm, fixture.engine.executionCounts())

	changedResource := ingressFeatureIngress("subject", "changed.example.com", map[string]string{
		"haproxy-haptic.org/request-set-header": "X-Test changed",
	})
	fixture.updateIngress(t, changedResource)
	fixture.config.TemplatingSettings.ExtraContext["failAfterReplay"] = true
	failed, err := fixture.render(rendercontext.RenderModeReconcile)
	require.ErrorContains(t, err, "forced failure after Ingress feature replay")
	assert.Nil(t, failed)
	afterFailure := fixture.engine.executionCounts()
	fixture.config.TemplatingSettings.ExtraContext["failAfterReplay"] = false
	retried := fixture.renderAndCommit(t)
	requireIngressFeatureDifferential(t, retried)
	assert.Contains(t, retried.HAProxyConfig, "changed.example.com")
	assert.Equal(t, afterFailure["ingresses/subject"]+3, fixture.engine.executionCounts()["ingresses/subject"])

	invalid := ingressFeatureIngress("subject", "admission.example.com", map[string]string{
		"haproxy-haptic.org/request-set-header": "X-Test invalid\tvalue",
	})
	overlay := stores.NewOverlayStoreProvider(
		fixture.provider,
		stores.NewValidationContext(map[string]*stores.StoreOverlay{
			"ingresses": stores.NewStoreOverlayForUpdate(&unstructured.Unstructured{Object: invalid}),
		}),
	)
	admissionResult, err := fixture.service.Render(
		t.Context(), overlay, rendercontext.RenderModeAdmission,
		rendercontext.WithAdmissionSubject("ingresses", "default", "subject"),
	)
	require.ErrorContains(t, err, "would split the frontend directive")
	assert.Nil(t, admissionResult)
	afterAdmission := fixture.engine.executionCounts()
	baseAfterAdmission := fixture.renderAndCommit(t)
	require.Equal(t, ingressFeatureResultSnapshot(t, retried), ingressFeatureResultSnapshot(t, baseAfterAdmission))
	require.Equal(t, afterAdmission, fixture.engine.executionCounts())
}

func newIngressFeatureChartFixture(t *testing.T) *ingressFeatureChartFixture {
	t.Helper()
	cfg := &config.Config{
		Dataplane: testDataplaneConfig(),
		TemplatingSettings: config.TemplatingSettings{ExtraContext: map[string]any{
			"ingressDefaultSSLRedirect":     true,
			"ingressDefaultSSLRedirectCode": "308",
			"ingressDefaultHTTPS":           true,
			"tls":                           map[string]any{"defaultCertificate": map[string]any{"name": "default"}},
			"failAfterReplay":               false,
			"poisonRead":                    false,
		}},
		WatchedResources: map[string]config.WatchedResource{
			"ingresses": {APIVersion: "networking.k8s.io/v1", Resources: "ingresses", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"services":  {APIVersion: "v1", Resources: "services", IndexBy: []string{"metadata.namespace", "metadata.name"}},
			"endpoints": {APIVersion: "discovery.k8s.io/v1", Resources: "endpointslices", IndexBy: []string{"metadata.namespace", "metadata.labels.kubernetes\\.io/service-name"}},
			"secrets":   {APIVersion: "v1", Resources: "secrets", IndexBy: []string{"metadata.namespace", "metadata.name"}},
		},
		TemplateSnippets: loadIngressFeatureChartSnippets(t),
		HAProxyConfig:    config.HAProxyConfig{Template: ingressFeatureChartRoot},
	}
	raceScaleRenderTimeout(cfg)
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
	services := k8sstore.NewMemoryStore(2)
	endpoints := k8sstore.NewMemoryStore(2)
	secrets := k8sstore.NewMemoryStore(2)
	provider := stores.NewRealStoreProvider(map[string]stores.Store{
		"ingresses": ingresses, "services": services, "endpoints": endpoints, "secrets": secrets,
	})
	return &ingressFeatureChartFixture{
		config: cfg, service: service, engine: engine, ingresses: ingresses, services: services,
		secrets: secrets, provider: provider,
	}
}

func loadIngressFeatureChartSnippets(t *testing.T) map[string]config.TemplateSnippet {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	chartRoot := filepath.Join(filepath.Dir(sourceFile), "..", "..", "..", "charts", "haptic", "charts")
	files := []string{
		"base/library.yaml",
		"ingress/library.yaml",
		"haproxytech/library.yaml",
		"haproxy-ingress/30-frontend-filters.yaml",
		"nginx-ingress/20-frontend-filters.yaml",
		"haptic-annotations/30-frontend-filters.yaml",
	}
	wanted := map[string]bool{
		"util-webhook-reject-or-warn":                true,
		"util-config-injection-kind":                 true,
		"util-backend-name-ingress":                  true,
		"features-100-ingress-tls":                   true,
		ingressTLSFeatureComponent:                   true,
		"util-ingress-default-ssl-redirect-bindings": true,
		"features-105-ingress-ssl-redirect":          true,
		ingressRedirectFeatureComponent:              true,
		"util-ingress-header-publish":                true,
		"ingress-header-mods-9999-declaration":       true,
		"features-301-ingress-header-mods":           true,
		"ingress-header-mods-0300-haproxytech":       true,
		"ingress-header-mods-0670-haproxy-ingress":   true,
		"ingress-header-mods-0740-nginx-ingress":     true,
		ingressHapticHeaderComponent:                 true,
	}
	result := make(map[string]config.TemplateSnippet, len(wanted))
	for _, relativePath := range files {
		content, err := os.ReadFile(filepath.Join(chartRoot, relativePath))
		require.NoError(t, err)
		var library ingressBackendChartLibrary
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

func ingressFeatureIngress(name, host string, annotations map[string]string) map[string]any {
	const secret = "shared"
	annotationValues := map[string]any{}
	for key, value := range annotations {
		annotationValues[key] = value
	}
	spec := map[string]any{
		"rules": []any{map[string]any{
			"host": host,
			"http": map[string]any{"paths": []any{map[string]any{
				"path": "/", "pathType": "Prefix",
				"backend": map[string]any{"service": map[string]any{
					"name": "app", "port": map[string]any{"name": "http"},
				}},
			}}},
		}},
	}
	if secret != "" {
		spec["tls"] = []any{map[string]any{"hosts": []any{host}, "secretName": secret}}
	}
	return map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "Ingress",
		"metadata": map[string]any{"namespace": "default", "name": name, "annotations": annotationValues},
		"spec":     spec,
	}
}

func ingressFeatureSecret(name, certificate, key string) map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"namespace": "default", "name": name},
		"data": map[string]any{
			"tls.crt": base64.StdEncoding.EncodeToString([]byte(certificate)),
			"tls.key": base64.StdEncoding.EncodeToString([]byte(key)),
		},
	}
}

func (f *ingressFeatureChartFixture) addIngress(t *testing.T, resource map[string]any) {
	t.Helper()
	name := resource["metadata"].(map[string]any)["name"].(string)
	require.NoError(t, f.ingresses.Add(resource, []string{"default", name}))
}

func (f *ingressFeatureChartFixture) updateIngress(t *testing.T, resource map[string]any) {
	t.Helper()
	name := resource["metadata"].(map[string]any)["name"].(string)
	require.NoError(t, f.ingresses.Update(resource, []string{"default", name}))
}

func (f *ingressFeatureChartFixture) deleteIngress(t *testing.T, name string) {
	t.Helper()
	require.NoError(t, f.ingresses.Delete("default", name, []string{"default", name}))
}

func (f *ingressFeatureChartFixture) addSecret(t *testing.T, resource map[string]any) {
	t.Helper()
	name := resource["metadata"].(map[string]any)["name"].(string)
	require.NoError(t, f.secrets.Add(resource, []string{"default", name}))
}

func (f *ingressFeatureChartFixture) updateSecret(t *testing.T, resource map[string]any) {
	t.Helper()
	name := resource["metadata"].(map[string]any)["name"].(string)
	require.NoError(t, f.secrets.Update(resource, []string{"default", name}))
}

func (f *ingressFeatureChartFixture) render(mode rendercontext.RenderMode) (*RenderResult, error) {
	// Hang guard, not a latency assertion: race instrumentation slows the
	// 3,000-ingress cold render past 30s.
	timeout := 30 * time.Second
	if raceDetectorEnabled {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return f.service.Render(ctx, f.provider, mode)
}

func (f *ingressFeatureChartFixture) renderAndCommit(t *testing.T) *RenderResult {
	t.Helper()
	result, err := f.render(rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, result.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, f.service)
	return result
}

func ingressFeatureResultSnapshot(t *testing.T, result *RenderResult) ingressFeatureSnapshot {
	t.Helper()
	snapshot := ingressFeatureSnapshot{config: result.HAProxyConfig, certificates: map[string]string{}}
	for _, certificate := range requireAuxiliaryFiles(t, result).SSLCertificates {
		snapshot.certificates[certificate.GetIdentifier()] = certificate.GetContent()
	}
	return snapshot
}

func ingressFeatureCertificate(t *testing.T, result *RenderResult, suffix string) string {
	t.Helper()
	for _, certificate := range requireAuxiliaryFiles(t, result).SSLCertificates {
		if strings.HasSuffix(certificate.GetIdentifier(), suffix) {
			return certificate.GetContent()
		}
	}
	require.FailNow(t, "certificate not found", suffix)
	return ""
}

func requireIngressFeatureDifferential(t *testing.T, result *RenderResult) {
	t.Helper()
	configText := result.HAProxyConfig
	incrementalStart := strings.Index(configText, "BEGIN-I\n")
	incrementalEnd := strings.Index(configText, "END-I\n")
	legacyStart := strings.Index(configText, "BEGIN-L\n")
	legacyEnd := strings.Index(configText, "END-L\n")
	require.NotEqual(t, -1, incrementalStart)
	require.NotEqual(t, -1, incrementalEnd)
	require.NotEqual(t, -1, legacyStart)
	require.NotEqual(t, -1, legacyEnd)
	incremental := configText[incrementalStart+len("BEGIN-I\n") : incrementalEnd]
	legacy := configText[legacyStart+len("BEGIN-L\n") : legacyEnd]
	require.Equal(t, legacy, incremental)
}
