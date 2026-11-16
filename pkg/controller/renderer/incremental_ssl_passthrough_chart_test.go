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
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
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

const (
	sslPassthroughHIComponent = "backends-501-haproxy-ingress-ssl-passthrough"
	sslPassthroughHTComponent = "backends-501-haproxytech-ssl-passthrough"
	sslPassthroughNIComponent = "backends-501-nginx-ingress-ssl-passthrough"
	sslPassthroughHAComponent = "backends-840-haptic-ssl-passthrough"
)

var sslPassthroughComponents = []string{
	sslPassthroughHIComponent,
	sslPassthroughHTComponent,
	sslPassthroughNIComponent,
	sslPassthroughHAComponent,
}

const sslPassthroughChartRoot = `{%- import "util-ssl-passthrough-backends" for SSLPassthroughBackends -%}
{%- var _, _ = shared.ComputeIfAbsent("globalFeatures", func() any {
  return map[string]any{
    "sslPassthroughBackends": toSlice(extraContext | dig("gatewayBackends")),
  }
}) -%}
{%- var values = SSLPassthroughBackends() -%}
# values
{%- for _, value := range values %}
{%- var backend = value.(map[string]any) %}
{{ tostring(backend["sni"]) }}={{ tostring(backend["name"]) }}
{%- end %}
# marker-winner={{ shared.Get("degradedBackendRef:default/missing-winner/http") != nil }}
# marker-loser={{ shared.Get("degradedBackendRef:default/missing-loser/http") != nil }}
{{ render "frontends-500-ssl-tcp" -}}
{%- if tostring(extraContext | dig("failAfterValues") | fallback(false)) == "true" -%}
{{ fail("forced failure after ssl passthrough values") }}
{%- end -%}
{{ planRegistry.ProfileGroup() }}
# gateway/backends-ssl-passthrough
backend gateway-pass
{{ render "backends-500-ssl-loopback" }}
{{ render "backends-501-haproxy-ingress-ssl-passthrough" }}
{{ render "backends-501-haproxytech-ssl-passthrough" }}
{{ render "backends-501-nginx-ingress-ssl-passthrough" }}
{{ render "backends-840-haptic-ssl-passthrough" }}`

type sslPassthroughChartLibrary struct {
	TemplateSnippets map[string]sslPassthroughChartSnippet `yaml:"templateSnippets"`
}

type sslPassthroughChartSnippet struct {
	Template    string                          `yaml:"template"`
	Requires    []string                        `yaml:"requires"`
	Incremental *sslPassthroughChartIncremental `yaml:"incremental"`
}

type sslPassthroughChartIncremental struct {
	Source            string                     `yaml:"source"`
	WhenAnyPathExists []string                   `yaml:"whenAnyPathExists"`
	Group             string                     `yaml:"group"`
	Effects           []config.IncrementalEffect `yaml:"effects"`
}

type sslPassthroughChartFixture struct {
	config    *config.Config
	service   *RenderService
	engine    *dynamicBindingCountingEngine
	ingresses *k8sstore.MemoryStore
	services  *k8sstore.MemoryStore
	endpoints *k8sstore.MemoryStore
	provider  stores.StoreProvider
}

func TestSSLPassthroughChartReusesCleanResourcesAndPromotesCachedLoser(t *testing.T) {
	fixture := newSSLPassthroughChartFixture(t)
	fixture.addService(t, sslPassthroughService("echo", "http", 80))
	fixture.addEndpoint(t, sslPassthroughEndpoint("echo", "http", 8080, "10.0.0.1"))
	fixture.addIngress(t, sslPassthroughIngress("a-hi", "haproxy-ingress.github.io/ssl-passthrough",
		[]string{"shared.example", "hi-only.example"}, "echo", map[string]any{"name": "http"}))
	fixture.addIngress(t, sslPassthroughIngress("b-hi", "haproxy-ingress.github.io/ssl-passthrough",
		[]string{"second-hi.example"}, "echo", map[string]any{"name": "http"}))
	fixture.addIngress(t, sslPassthroughIngress("z-hi", "haproxy-ingress.github.io/ssl-passthrough",
		[]string{"shared.example"}, "echo", map[string]any{"name": "http"}))
	fixture.addIngress(t, sslPassthroughIngress("ht", "haproxy.org/ssl-passthrough",
		[]string{"shared.example"}, "echo", map[string]any{"name": "http"}))
	fixture.addIngress(t, sslPassthroughIngress("haptic", "haproxy-haptic.org/ssl-passthrough",
		[]string{"*.wild.example"}, "echo", map[string]any{"name": "http"}))
	fixture.addIngress(t, sslPassthroughIngress("nginx", "nginx.ingress.kubernetes.io/ssl-passthrough",
		[]string{"numeric.example"}, "echo", map[string]any{"number": int64(80)}))

	first := fixture.renderAndCommit(t)
	assertSSLPassthroughColdConfig(t, first.HAProxyConfig)
	for _, ingress := range []string{"a-hi", "b-hi", "z-hi", "ht", "haptic", "nginx"} {
		fixture.assertActivationAwareComponentExecutions(t, ingress, 1)
	}

	warm := fixture.renderAndCommit(t)
	assert.Equal(t, first.HAProxyConfig, warm.HAProxyConfig)
	for _, ingress := range []string{"a-hi", "b-hi", "z-hi", "ht", "haptic", "nginx"} {
		fixture.assertActivationAwareComponentExecutions(t, ingress, 1)
	}

	beforeUnrelated := fixture.engine.executionCounts()
	fixture.addService(t, sslPassthroughService("unrelated", "other", 81))
	fixture.addEndpoint(t, sslPassthroughEndpoint("unrelated", "other", 8181, "10.0.1.1"))
	unrelated := fixture.renderAndCommit(t)
	assert.Equal(t, first.HAProxyConfig, unrelated.HAProxyConfig)
	assert.Equal(t, beforeUnrelated, fixture.engine.executionCounts())

	fixture.updateEndpoint(t, sslPassthroughEndpoint("echo", "http", 8080, "10.0.0.9"))
	updated := fixture.renderAndCommit(t)
	assert.Contains(t, updated.HAProxyConfig, "10.0.0.9:8080")
	assert.NotContains(t, updated.HAProxyConfig, "10.0.0.1:8080")
	fixture.assertComponentExecutions(t, sslPassthroughHIComponent, "a-hi", 2)
	fixture.assertComponentExecutions(t, sslPassthroughHIComponent, "b-hi", 2)
	fixture.assertComponentExecutions(t, sslPassthroughHIComponent, "z-hi", 2)
	fixture.assertComponentExecutions(t, sslPassthroughHTComponent, "ht", 2)
	fixture.assertComponentExecutions(t, sslPassthroughHAComponent, "haptic", 2)
	fixture.assertComponentExecutions(t, sslPassthroughNIComponent, "nginx", 2)

	fixture.deleteEndpoint(t, "echo")
	withoutEndpoint := fixture.renderAndCommit(t)
	assert.NotContains(t, withoutEndpoint.HAProxyConfig, "10.0.0.9:8080")
	assert.Contains(t, withoutEndpoint.HAProxyConfig,
		"backend ssl-passthrough-hi-default-a-hi from ")
	fixture.assertOnlyMatchingComponentExecutions(t, 3, map[string]string{
		"a-hi": sslPassthroughHIComponent, "b-hi": sslPassthroughHIComponent,
		"z-hi": sslPassthroughHIComponent,
		"ht":   sslPassthroughHTComponent, "haptic": sslPassthroughHAComponent,
		"nginx": sslPassthroughNIComponent,
	})

	zExecutions := fixture.componentExecutions(sslPassthroughHIComponent, "z-hi")
	fixture.deleteIngress(t, "a-hi")
	promoted := fixture.renderAndCommit(t)
	assert.Contains(t, promoted.HAProxyConfig, "shared.example=ssl-passthrough-hi-default-z-hi")
	assert.Contains(t, promoted.HAProxyConfig, "backend ssl-passthrough-hi-default-z-hi from ")
	assert.NotContains(t, promoted.HAProxyConfig, "hi-only.example")
	assert.Equal(t, zExecutions, fixture.componentExecutions(sslPassthroughHIComponent, "z-hi"))
}

func assertSSLPassthroughColdConfig(t *testing.T, rendered string) {
	t.Helper()
	assertOrderedSubstrings(t, rendered,
		"gateway.example=gateway-pass",
		"shared.example=ssl-passthrough-hi-default-a-hi",
		"hi-only.example=ssl-passthrough-hi-default-a-hi",
		"second-hi.example=ssl-passthrough-hi-default-b-hi",
		"shared.example=ssl-passthrough-default-ht",
		"*.wild.example=ssl-passthrough-haptic-default-haptic",
		"numeric.example=ssl-passthrough-ni-default-nginx",
	)
	assert.Contains(t, rendered,
		"use_backend ssl-passthrough-haptic-default-haptic if { req_ssl_sni -m end .wild.example }")
	assert.Equal(t, 2, strings.Count(rendered, "if { req_ssl_sni -m str shared.example }"))
	assert.Equal(t, 1, strings.Count(rendered, "backend ssl-passthrough-hi-default-a-hi from "))
	assert.Contains(t, rendered,
		"\n\n# haproxy-ingress/backends-ssl-passthrough\nbackend ssl-passthrough-hi-default-b-hi from ")
	assert.NotContains(t, rendered, "backend ssl-passthrough-hi-default-z-hi from ")
	assertOrderedSubstrings(t, rendered,
		"backend gateway-pass",
		"backend ssl-passthrough-hi-default-a-hi from ",
		"backend ssl-passthrough-hi-default-b-hi from ",
		"backend ssl-passthrough-default-ht from ",
		"backend ssl-passthrough-ni-default-nginx from ",
		"backend ssl-passthrough-haptic-default-haptic from ",
	)
}

func TestSSLPassthroughChartUpdatesBindTopologyOnIngressCreateAndDelete(t *testing.T) {
	fixture := newSSLPassthroughChartFixture(t)
	fixture.config.TemplatingSettings.ExtraContext["gatewayBackends"] = []any{}
	fixture.addService(t, sslPassthroughService("echo", "http", 80))
	fixture.addEndpoint(t, sslPassthroughEndpoint("echo", "http", 8080, "10.0.0.1"))

	empty := fixture.renderAndCommit(t)
	assert.NotContains(t, empty.HAProxyConfig, "frontend ssl-tcp")

	fixture.addIngress(t, sslPassthroughIngress("subject", "haproxy-haptic.org/ssl-passthrough",
		[]string{"topology.example"}, "echo", map[string]any{"name": "http"}))
	active := fixture.renderAndCommit(t)
	assert.Contains(t, active.HAProxyConfig, "frontend ssl-tcp")
	assert.Contains(t, active.HAProxyConfig, "bind *:8443")
	assert.Contains(t, active.HAProxyConfig,
		"backend ssl-passthrough-haptic-default-subject from ")

	fixture.deleteIngress(t, "subject")
	removed := fixture.renderAndCommit(t)
	assert.NotContains(t, removed.HAProxyConfig, "frontend ssl-tcp")
	assert.NotContains(t, removed.HAProxyConfig,
		"backend ssl-passthrough-haptic-default-subject from ")
}

func TestSSLPassthroughChartRequiresExactTrueAndFirstUsablePath(t *testing.T) {
	fixture := newSSLPassthroughChartFixture(t)
	fixture.addService(t, sslPassthroughService("echo", "http", 80))
	fixture.addEndpoint(t, sslPassthroughEndpoint("echo", "http", 8080, "10.0.0.1"))
	fixture.addIngress(t, sslPassthroughIngressWithValue("wrong-case", "haproxy-haptic.org/ssl-passthrough",
		"True", []string{"wrong.example"}, "echo", map[string]any{"name": "http"}))
	fixture.addIngress(t, sslPassthroughIngress("hostless", "haproxy-haptic.org/ssl-passthrough",
		[]string{""}, "echo", map[string]any{"name": "http"}))
	fixture.addIngress(t, sslPassthroughIngressWithoutPaths("no-paths",
		"haproxy-haptic.org/ssl-passthrough", "orphan.example"))
	fixture.addIngress(t, sslPassthroughIngressWithLeadingEmptyService("first-usable",
		"haproxy-haptic.org/ssl-passthrough", "echo"))

	result := fixture.renderAndCommit(t)
	assert.NotContains(t, result.HAProxyConfig, "wrong.example")
	assert.NotContains(t, result.HAProxyConfig, "orphan.example")
	assert.NotContains(t, result.HAProxyConfig, "ssl-passthrough-haptic-default-hostless")
	assert.Contains(t, result.HAProxyConfig,
		"first.example=ssl-passthrough-haptic-default-first-usable")
	assert.Contains(t, result.HAProxyConfig,
		"second.example=ssl-passthrough-haptic-default-first-usable")
	assert.Equal(t, 1, strings.Count(result.HAProxyConfig,
		"backend ssl-passthrough-haptic-default-first-usable from "))
}

func TestSSLPassthroughChartRestoresDegradedMarkerOnlyForCurrentWinner(t *testing.T) {
	fixture := newSSLPassthroughChartFixture(t)
	fixture.addIngress(t, sslPassthroughIngress("a-winner", "haproxy-ingress.github.io/ssl-passthrough",
		[]string{"degraded.example"}, "missing-winner", map[string]any{"name": "http"}))
	fixture.addIngress(t, sslPassthroughIngress("z-loser", "haproxy-ingress.github.io/ssl-passthrough",
		[]string{"degraded.example"}, "missing-loser", map[string]any{"name": "http"}))

	first := fixture.renderAndCommit(t)
	assert.Contains(t, first.HAProxyConfig, "# marker-winner=true")
	assert.Contains(t, first.HAProxyConfig, "# marker-loser=false")
	assert.Contains(t, first.HAProxyConfig, "backend ssl-passthrough-hi-default-a-winner from ")
	assert.NotContains(t, first.HAProxyConfig, "backend ssl-passthrough-hi-default-z-loser from ")

	loserExecutions := fixture.componentExecutions(sslPassthroughHIComponent, "z-loser")
	fixture.deleteIngress(t, "a-winner")
	promoted := fixture.renderAndCommit(t)
	assert.Contains(t, promoted.HAProxyConfig, "# marker-winner=false")
	assert.Contains(t, promoted.HAProxyConfig, "# marker-loser=true")
	assert.Contains(t, promoted.HAProxyConfig, "backend ssl-passthrough-hi-default-z-loser from ")
	assert.Equal(t, loserExecutions, fixture.componentExecutions(sslPassthroughHIComponent, "z-loser"))

	fixture.addService(t, sslPassthroughService("missing-loser", "http", 80))
	resolved := fixture.renderAndCommit(t)
	assert.Contains(t, resolved.HAProxyConfig, "# marker-loser=false")
	assert.Equal(t, loserExecutions+1, fixture.componentExecutions(sslPassthroughHIComponent, "z-loser"))
}

func TestSSLPassthroughChartAbortedAdmissionAndConcurrentRendersStayIsolated(t *testing.T) {
	fixture := newSSLPassthroughChartFixture(t)
	fixture.addService(t, sslPassthroughService("echo", "http", 80))
	fixture.addEndpoint(t, sslPassthroughEndpoint("echo", "http", 8080, "10.0.0.1"))
	baselineIngress := sslPassthroughIngress("subject", "haproxy-haptic.org/ssl-passthrough",
		[]string{"stable.example"}, "echo", map[string]any{"name": "http"})
	fixture.addIngress(t, baselineIngress)
	baseline := fixture.renderAndCommit(t)
	baselineExecutions := fixture.engine.executionCounts()

	results := make([]*RenderResult, 2)
	errors := make([]error, 2)
	var wait sync.WaitGroup
	for index := range results {
		wait.Add(1)
		go func() {
			defer wait.Done()
			results[index], errors[index] = fixture.service.Render(
				t.Context(), fixture.provider, rendercontext.RenderModeReconcile,
			)
		}()
	}
	wait.Wait()
	for index := range results {
		require.NoError(t, errors[index])
		assert.Equal(t, baseline.HAProxyConfig, results[index].HAProxyConfig)
		require.NoError(t, results[index].InputTransaction.Commit(t.Context()))
	}
	assert.Equal(t, baselineExecutions, fixture.engine.executionCounts())

	proposed := sslPassthroughIngress("subject", "haproxy-haptic.org/ssl-passthrough",
		[]string{"poison.example"}, "echo", map[string]any{"name": "http"})
	overlay := stores.NewOverlayStoreProvider(
		fixture.provider,
		stores.NewValidationContext(map[string]*stores.StoreOverlay{
			"ingresses": stores.NewStoreOverlayForUpdate(&unstructured.Unstructured{Object: proposed}),
		}),
	)
	fixture.config.TemplatingSettings.ExtraContext["failAfterValues"] = true
	failed, err := fixture.service.Render(
		t.Context(), overlay, rendercontext.RenderModeAdmission,
		rendercontext.WithAdmissionSubject("ingresses", "default", "subject"),
	)
	require.ErrorContains(t, err, "forced failure after ssl passthrough values")
	assert.Nil(t, failed)
	fixture.config.TemplatingSettings.ExtraContext["failAfterValues"] = false
	afterAdmissionExecutions := fixture.engine.executionCounts()
	afterAdmission := fixture.renderAndCommit(t)
	assert.Equal(t, baseline.HAProxyConfig, afterAdmission.HAProxyConfig)
	assert.NotContains(t, afterAdmission.HAProxyConfig, "poison.example")
	assert.Equal(t, afterAdmissionExecutions, fixture.engine.executionCounts())

	updatedIngress := sslPassthroughIngress("subject", "haproxy-haptic.org/ssl-passthrough",
		[]string{"updated.example"}, "echo", map[string]any{"name": "http"})
	fixture.updateIngress(t, updatedIngress)
	fixture.config.TemplatingSettings.ExtraContext["failAfterValues"] = true
	failed, err = fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.ErrorContains(t, err, "forced failure after ssl passthrough values")
	assert.Nil(t, failed)
	beforeRetry := fixture.engine.executionCounts()
	fixture.config.TemplatingSettings.ExtraContext["failAfterValues"] = false
	retried := fixture.renderAndCommit(t)
	assert.Contains(t, retried.HAProxyConfig, "updated.example")
	assert.NotContains(t, retried.HAProxyConfig, "stable.example")
	assert.Equal(t, beforeRetry["ingresses/subject"]+1,
		fixture.engine.executionCounts()["ingresses/subject"])
}

func newSSLPassthroughChartFixture(t *testing.T) *sslPassthroughChartFixture {
	t.Helper()
	snippets := loadSSLPassthroughChartSnippets(t)
	cfg := &config.Config{
		Dataplane: testDataplaneConfig(),
		TemplatingSettings: config.TemplatingSettings{ExtraContext: map[string]any{
			"annotationLibraries": map[string]any{
				"haptic": true, "haproxyIngress": true, "haproxytech": true, "nginx": true,
			},
			"gatewayBackends": []any{map[string]any{
				"name": "gateway-pass", "sni": "gateway.example", "mode": "Passthrough",
			}},
			"failAfterValues": false,
		}},
		WatchedResources: map[string]config.WatchedResource{
			"ingresses": {
				APIVersion: "networking.k8s.io/v1", Resources: "ingresses",
				IndexBy: []string{"metadata.namespace", "metadata.name"},
			},
			"services": {
				APIVersion: "v1", Resources: "services",
				IndexBy: []string{"metadata.namespace", "metadata.name"},
			},
			"endpoints": {
				APIVersion: "discovery.k8s.io/v1", Resources: "endpointslices",
				IndexBy: []string{"metadata.namespace", "metadata.labels.kubernetes\\.io/service-name"},
			},
		},
		TemplateSnippets: snippets,
		HAProxyConfig:    config.HAProxyConfig{Template: sslPassthroughChartRoot},
	}
	types := &typebootstrap.Result{
		Types: map[string]reflect.Type{
			"services":  reflect.TypeOf(backendServersService{}),
			"endpoints": reflect.TypeOf(backendServersEndpointSlice{}),
		},
		Kinds:  map[string]string{"services": "Service", "endpoints": "EndpointSlice"},
		Errors: map[string]error{},
	}
	declarations := helpers.BuildAdditionalDeclarations(cfg, types)
	baseEngine, err := helpers.NewEngineFromConfigWithOptions(
		cfg, nil, nil, declarations, helpers.EngineOptions{},
	)
	require.NoError(t, err)
	engine := newDynamicBindingCountingEngine(t, baseEngine)
	service := NewRenderService(&RenderServiceConfig{
		Engine: engine, Config: cfg, Logger: slog.Default(), Capabilities: defaultCapabilities(),
		TypedResourceTypes: types.Types,
	})
	ingresses := k8sstore.NewMemoryStore(2)
	services := k8sstore.NewMemoryStore(2)
	endpoints := k8sstore.NewMemoryStore(2)
	return &sslPassthroughChartFixture{
		config: cfg, service: service, engine: engine,
		ingresses: ingresses, services: services, endpoints: endpoints,
		provider: stores.NewRealStoreProvider(map[string]stores.Store{
			"ingresses": ingresses, "services": services, "endpoints": endpoints,
		}),
	}
}

func loadSSLPassthroughChartSnippets(t *testing.T) map[string]config.TemplateSnippet {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	chartRoot := filepath.Join(filepath.Dir(sourceFile), "..", "..", "..", "charts", "haptic", "charts")
	files := []string{
		"base/library.yaml",
		"kubernetes-backends/library.yaml",
		"ssl/library.yaml",
		"ingress-annotations-compat/library.yaml",
		"haproxy-ingress/40-features.yaml",
		"haproxytech/library.yaml",
		"haptic-annotations/40-features.yaml",
		"nginx-ingress/30-features.yaml",
	}
	wanted := map[string]bool{
		"util-log-format-tcp":                       true,
		"util-service-port-resolution":              true,
		"util-webhook-reject-or-warn":               true,
		"util-backend":                              true,
		"util-backend-servers-helpers":              true,
		"util-backend-servers-result":               true,
		"util-gated-pod-routable":                   true,
		"util-ssl-passthrough-backends":             true,
		"util-ssl-sni-use-backend-lines":            true,
		"frontends-500-ssl-tcp":                     true,
		"backends-500-ssl-loopback":                 true,
		"util-annotation-ssl-passthrough-component": true,
		"util-annotation-ssl-passthrough-values":    true,
		sslPassthroughHIComponent:                   true,
		sslPassthroughHTComponent:                   true,
		sslPassthroughNIComponent:                   true,
		sslPassthroughHAComponent:                   true,
	}
	result := make(map[string]config.TemplateSnippet, len(wanted))
	for _, relativePath := range files {
		content, err := os.ReadFile(filepath.Join(chartRoot, relativePath))
		require.NoError(t, err)
		var library sslPassthroughChartLibrary
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

func sslPassthroughService(name, portName string, port int64) map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"namespace": "default", "name": name},
		"spec":     map[string]any{"ports": []any{map[string]any{"name": portName, "port": port}}},
	}
}

func sslPassthroughEndpoint(service, portName string, port int64, address string) map[string]any {
	return map[string]any{
		"apiVersion": "discovery.k8s.io/v1", "kind": "EndpointSlice",
		"metadata": map[string]any{
			"namespace": "default", "name": service + "-slice",
			"labels": map[string]any{"kubernetes.io/service-name": service},
		},
		"ports": []any{map[string]any{"name": portName, "port": port}},
		"endpoints": []any{map[string]any{
			"addresses": []any{address}, "targetRef": map[string]any{"name": service + "-pod"},
		}},
	}
}

func sslPassthroughIngress(
	name, annotation string,
	hosts []string,
	service string,
	port map[string]any,
) map[string]any {
	return sslPassthroughIngressWithValue(name, annotation, "true", hosts, service, port)
}

func sslPassthroughIngressWithValue(
	name, annotation, annotationValue string,
	hosts []string,
	service string,
	port map[string]any,
) map[string]any {
	rules := make([]any, 0, len(hosts))
	for index, host := range hosts {
		rule := map[string]any{"host": host}
		if index == 0 {
			rule["http"] = map[string]any{"paths": []any{map[string]any{
				"backend": map[string]any{"service": map[string]any{"name": service, "port": port}},
			}}}
		}
		rules = append(rules, rule)
	}
	return map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "Ingress",
		"metadata": map[string]any{
			"namespace": "default", "name": name,
			"annotations": map[string]any{annotation: annotationValue},
		},
		"spec": map[string]any{"rules": rules},
	}
}

func sslPassthroughIngressWithoutPaths(name, annotation, host string) map[string]any {
	return map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "Ingress",
		"metadata": map[string]any{
			"namespace": "default", "name": name,
			"annotations": map[string]any{annotation: "true"},
		},
		"spec": map[string]any{"rules": []any{map[string]any{"host": host}}},
	}
}

func sslPassthroughIngressWithLeadingEmptyService(name, annotation, service string) map[string]any {
	return map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "Ingress",
		"metadata": map[string]any{
			"namespace": "default", "name": name,
			"annotations": map[string]any{annotation: "true"},
		},
		"spec": map[string]any{"rules": []any{
			map[string]any{
				"host": "first.example",
				"http": map[string]any{"paths": []any{map[string]any{
					"backend": map[string]any{"service": map[string]any{"name": ""}},
				}}},
			},
			map[string]any{
				"host": "second.example",
				"http": map[string]any{"paths": []any{map[string]any{
					"backend": map[string]any{"service": map[string]any{
						"name": service, "port": map[string]any{"name": "http"},
					}},
				}}},
			},
		}},
	}
}

func (f *sslPassthroughChartFixture) addIngress(t *testing.T, ingress map[string]any) {
	t.Helper()
	name := ingress["metadata"].(map[string]any)["name"].(string)
	require.NoError(t, f.ingresses.Add(ingress, []string{"default", name}))
}

func (f *sslPassthroughChartFixture) updateIngress(t *testing.T, ingress map[string]any) {
	t.Helper()
	name := ingress["metadata"].(map[string]any)["name"].(string)
	require.NoError(t, f.ingresses.Update(ingress, []string{"default", name}))
}

func (f *sslPassthroughChartFixture) deleteIngress(t *testing.T, name string) {
	t.Helper()
	require.NoError(t, f.ingresses.Delete("default", name, []string{"default", name}))
}

func (f *sslPassthroughChartFixture) addService(t *testing.T, service map[string]any) {
	t.Helper()
	name := service["metadata"].(map[string]any)["name"].(string)
	require.NoError(t, f.services.Add(service, []string{"default", name}))
}

func (f *sslPassthroughChartFixture) addEndpoint(t *testing.T, endpoint map[string]any) {
	t.Helper()
	metadata := endpoint["metadata"].(map[string]any)
	service := metadata["labels"].(map[string]any)["kubernetes.io/service-name"].(string)
	require.NoError(t, f.endpoints.Add(endpoint, []string{"default", service}))
}

func (f *sslPassthroughChartFixture) updateEndpoint(t *testing.T, endpoint map[string]any) {
	t.Helper()
	metadata := endpoint["metadata"].(map[string]any)
	service := metadata["labels"].(map[string]any)["kubernetes.io/service-name"].(string)
	require.NoError(t, f.endpoints.Update(endpoint, []string{"default", service}))
}

func (f *sslPassthroughChartFixture) deleteEndpoint(t *testing.T, service string) {
	t.Helper()
	require.NoError(t, f.endpoints.Delete(
		"default", service+"-slice", []string{"default", service},
	))
}

func (f *sslPassthroughChartFixture) renderAndCommit(t *testing.T) *RenderResult {
	t.Helper()
	result, err := f.service.Render(t.Context(), f.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NotNil(t, result.InputTransaction)
	require.NoError(t, result.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, f.service)
	return result
}

func (f *sslPassthroughChartFixture) componentExecutions(componentName, ingress string) uint64 {
	component := f.service.incremental.components[componentName]
	query := componentQueryKey(&component, "ingresses", "default", ingress)
	return f.service.incremental.graph.Counters(query).Executions
}

func (f *sslPassthroughChartFixture) assertComponentExecutions(
	t *testing.T,
	componentName, ingress string,
	want uint64,
) {
	t.Helper()
	assert.Equal(t, want, f.componentExecutions(componentName, ingress), componentName+"/"+ingress)
}

func (f *sslPassthroughChartFixture) assertActivationAwareComponentExecutions(
	t *testing.T,
	ingress string,
	activeWant uint64,
) {
	t.Helper()
	for _, componentName := range sslPassthroughComponents {
		want := uint64(0)
		if componentName == sslPassthroughHIComponent && (ingress == "a-hi" || ingress == "b-hi" || ingress == "z-hi") ||
			componentName == sslPassthroughHTComponent && ingress == "ht" ||
			componentName == sslPassthroughHAComponent && ingress == "haptic" ||
			componentName == sslPassthroughNIComponent && ingress == "nginx" {
			want = activeWant
		}
		f.assertComponentExecutions(t, componentName, ingress, want)
	}
}

func (f *sslPassthroughChartFixture) assertOnlyMatchingComponentExecutions(
	t *testing.T,
	matchingWant uint64,
	matchingByIngress map[string]string,
) {
	t.Helper()
	for ingress, matching := range matchingByIngress {
		for _, component := range sslPassthroughComponents {
			want := uint64(0)
			if component == matching {
				want = matchingWant
			}
			f.assertComponentExecutions(t, component, ingress, want)
		}
	}
}

func assertOrderedSubstrings(t *testing.T, text string, substrings ...string) {
	t.Helper()
	position := 0
	for _, substring := range substrings {
		index := strings.Index(text[position:], substring)
		require.NotEqual(t, -1, index, substring)
		position += index + len(substring)
	}
}
