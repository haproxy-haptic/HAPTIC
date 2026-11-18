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
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/helpers"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/typebootstrap"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	k8sstore "github.com/haproxy-haptic/HAPTIC/pkg/k8s/store"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
)

const requestValidationPublicationComponent = "haptic-request-validation-publications"

const requestValidationPublicationRoot = `{{- render "haptic-request-validation-publications" -}}
{%- for _, value := range incremental_values("haptic-request-validation", "schemas") -%}
  {%- var record = value.(map[string]any) -%}
{{ "\n" }}schema={{ tostring(record["id"]) }}|{{ tostring(record["schemaBase64"]) }}|{{ join(record["contentTypes"].([]any), ",") }}
{%- end -%}
{%- for _, value := range incremental_values("haptic-request-validation", "rules") -%}
  {%- var record = value.(map[string]any) -%}
{{ "\n" }}rule={{ tostring(record["resourceID"]) }}|{{ tostring(record["schemaID"]) }}|{{ tostring(record["maxBodyBytes"]) }}|{{ tostring(record["failOpen"]) }}|{{ tostring(record["contentTypes"]) }}
{%- end -%}
{%- if tostring(extraContext | dig("failAfterReplay") | fallback(false)) == "true" -%}
  {{- fail("forced failure after request-validation replay") -}}
{%- end -%}`

type requestValidationMetadata struct {
	Namespace   string            `json:"namespace"`
	Name        string            `json:"name"`
	Annotations map[string]string `json:"annotations"`
}

type requestValidationIngress struct {
	Metadata requestValidationMetadata `json:"metadata"`
}

type requestValidationDataResource struct {
	Metadata requestValidationMetadata `json:"metadata"`
	Data     map[string]string         `json:"data"`
}

type requestValidationFixture struct {
	config     *config.Config
	service    *RenderService
	ingresses  *k8sstore.MemoryStore
	configmaps *k8sstore.MemoryStore
	secrets    *k8sstore.MemoryStore
	provider   stores.StoreProvider
}

func TestRequestValidationPublicationsTrackExactDependenciesAndPromoteOnDelete(t *testing.T) {
	fixture := newRequestValidationFixture(t)
	fixture.addConfigMap(t, "schema", `{"type":"string"}`)
	fixture.addConfigMap(t, "unrelated", `{"type":"null"}`)
	fixture.addIngress(t, requestValidationIngressResource("a-owner", map[string]string{
		"haproxy-haptic.org/request-schema-configmap": "schema:schema.json",
	}))
	fixture.addIngress(t, requestValidationIngressResource("z-owner", map[string]string{
		"haproxy-haptic.org/request-schema-configmap": "schema:schema.json",
	}))

	first := fixture.renderAndCommit(t)
	assert.Equal(t, 1, strings.Count(first.HAProxyConfig, "schema=configmap/default/schema/schema.json/application/json|"))
	assertOrderedSubstrings(t, first.HAProxyConfig, "rule=default/a-owner|", "rule=default/z-owner|")
	fixture.assertExecutions(t, "a-owner", 1)
	fixture.assertExecutions(t, "z-owner", 1)

	warm := fixture.renderAndCommit(t)
	assert.Equal(t, first.HAProxyConfig, warm.HAProxyConfig)
	fixture.updateConfigMap(t, "unrelated", `{"type":"boolean"}`)
	unrelated := fixture.renderAndCommit(t)
	assert.Equal(t, first.HAProxyConfig, unrelated.HAProxyConfig)
	fixture.assertExecutions(t, "a-owner", 1)
	fixture.assertExecutions(t, "z-owner", 1)

	fixture.deleteIngress(t, "a-owner")
	promoted := fixture.renderAndCommit(t)
	assert.Equal(t, 1, strings.Count(promoted.HAProxyConfig, "schema=configmap/default/schema/schema.json/application/json|"))
	assert.NotContains(t, promoted.HAProxyConfig, "rule=default/a-owner|")
	assert.Contains(t, promoted.HAProxyConfig, "rule=default/z-owner|")
	fixture.assertExecutions(t, "z-owner", 1)

	fixture.updateConfigMap(t, "schema", `{"type":"number"}`)
	changed := fixture.renderAndCommit(t)
	assert.NotEqual(t, promoted.HAProxyConfig, changed.HAProxyConfig)
	assert.Contains(t, changed.HAProxyConfig, "eyJ0eXBlIjoibnVtYmVyIn0=")
	assert.NotContains(t, changed.HAProxyConfig, "eyJ0eXBlIjoic3RyaW5nIn0=")
	fixture.assertExecutions(t, "z-owner", 2)
}

func TestRequestValidationFailedRootAndAdmissionCannotPoisonPublications(t *testing.T) {
	fixture := newRequestValidationFixture(t)
	fixture.addConfigMap(t, "schema", `{"type":"string"}`)
	stable := requestValidationIngressResource("subject", map[string]string{
		"haproxy-haptic.org/request-schema-configmap": "schema:schema.json",
	})
	fixture.addIngress(t, stable)
	baseline := fixture.renderAndCommit(t)
	fixture.assertExecutions(t, "subject", 1)

	fixture.updateConfigMap(t, "schema", `{"type":"number"}`)
	fixture.config.TemplatingSettings.ExtraContext["failAfterReplay"] = true
	failed, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.ErrorContains(t, err, "forced failure after request-validation replay")
	assert.Nil(t, failed)
	fixture.assertExecutions(t, "subject", 1)

	fixture.config.TemplatingSettings.ExtraContext["failAfterReplay"] = false
	retried := fixture.renderAndCommit(t)
	assert.NotEqual(t, baseline.HAProxyConfig, retried.HAProxyConfig)
	assert.Contains(t, retried.HAProxyConfig, "eyJ0eXBlIjoibnVtYmVyIn0=")
	fixture.assertExecutions(t, "subject", 2)

	proposed := requestValidationIngressResource("subject", map[string]string{
		"haproxy-haptic.org/request-schema-configmap":     "schema:schema.json",
		"haproxy-haptic.org/request-schema-max-body-size": "65536",
	})
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
	require.ErrorContains(t, err, "Must not exceed requestBodyInspection.haproxyBuffer.sizeBytes")
	assert.Nil(t, admission)
	fixture.assertExecutions(t, "subject", 2)

	after := fixture.renderAndCommit(t)
	assert.Equal(t, retried.HAProxyConfig, after.HAProxyConfig)
	fixture.assertExecutions(t, "subject", 2)
}

func newRequestValidationFixture(t *testing.T) *requestValidationFixture {
	t.Helper()
	cfg := &config.Config{
		Dataplane: testDataplaneConfig(),
		TemplatingSettings: config.TemplatingSettings{ExtraContext: map[string]any{
			"apiGateway": map[string]any{"requestSchemaValidation": map[string]any{
				"enabled": true, "defaultFailOpen": true,
				"requestBody": map[string]any{"defaultMaxBytes": 8192, "waitTimeout": "100ms"},
			}},
			"requestBodyInspection": map[string]any{"haproxyBuffer": map[string]any{
				"sizeBytes": 65536, "reservedBytes": 8192,
			}},
			"failAfterReplay": false,
		}},
		WatchedResources: map[string]config.WatchedResource{
			"ingresses": {
				APIVersion: "networking.k8s.io/v1", Resources: "ingresses",
				IndexBy: []string{"metadata.namespace", "metadata.name"},
			},
			"configmaps": {
				APIVersion: "v1", Resources: "configmaps",
				IndexBy: []string{"metadata.namespace", "metadata.name"},
			},
			"secrets": {
				APIVersion: "v1", Resources: "secrets",
				IndexBy: []string{"metadata.namespace", "metadata.name"},
			},
		},
		TemplateSnippets: loadRequestValidationSnippets(t),
		HAProxyConfig:    config.HAProxyConfig{Template: requestValidationPublicationRoot},
	}
	types := &typebootstrap.Result{
		Types: map[string]reflect.Type{
			"ingresses":  reflect.TypeOf(requestValidationIngress{}),
			"configmaps": reflect.TypeOf(requestValidationDataResource{}),
			"secrets":    reflect.TypeOf(requestValidationDataResource{}),
		},
		Kinds: map[string]string{
			"ingresses": "Ingress", "configmaps": "ConfigMap", "secrets": "Secret",
		},
		Errors: map[string]error{},
	}
	declarations := helpers.BuildAdditionalDeclarations(cfg, types)
	engine, err := helpers.NewEngineFromConfigWithOptions(cfg, nil, nil, declarations, helpers.EngineOptions{})
	require.NoError(t, err)
	service := NewRenderService(&RenderServiceConfig{
		Engine: engine, Config: cfg, Logger: slog.Default(), Capabilities: defaultCapabilities(),
		TypedResourceTypes: types.Types,
	})
	ingresses := k8sstore.NewMemoryStore(2)
	configmaps := k8sstore.NewMemoryStore(2)
	secrets := k8sstore.NewMemoryStore(2)
	return &requestValidationFixture{
		config: cfg, service: service, ingresses: ingresses, configmaps: configmaps, secrets: secrets,
		provider: stores.NewRealStoreProvider(map[string]stores.Store{
			"ingresses": ingresses, "configmaps": configmaps, "secrets": secrets,
		}),
	}
}

func loadRequestValidationSnippets(t *testing.T) map[string]config.TemplateSnippet {
	t.Helper()
	return loadChartSnippets(t, chartLibrariesRoot(t), map[string][]string{
		"haptic-annotations/82-request-validation.yaml": {
			"util-haptic-request-validation-bindings",
			"util-haptic-request-schema-secret-foreign",
			requestValidationPublicationComponent,
		},
	})
}

func requestValidationIngressResource(name string, annotations map[string]string) map[string]any {
	annotationValues := make(map[string]any, len(annotations))
	for key, value := range annotations {
		annotationValues[key] = value
	}
	return map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "Ingress",
		"metadata": map[string]any{
			"namespace": "default", "name": name, "annotations": annotationValues,
		},
	}
}

func requestValidationConfigMapResource(name, schema string) map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"namespace": "default", "name": name},
		"data":     map[string]any{"schema.json": schema},
	}
}

func (f *requestValidationFixture) addIngress(t *testing.T, ingress map[string]any) {
	t.Helper()
	name := ingress["metadata"].(map[string]any)["name"].(string)
	require.NoError(t, f.ingresses.Add(ingress, []string{"default", name}))
}

func (f *requestValidationFixture) deleteIngress(t *testing.T, name string) {
	t.Helper()
	require.NoError(t, f.ingresses.Delete("default", name, []string{"default", name}))
}

func (f *requestValidationFixture) addConfigMap(t *testing.T, name, schema string) {
	t.Helper()
	require.NoError(t, f.configmaps.Add(requestValidationConfigMapResource(name, schema), []string{"default", name}))
}

func (f *requestValidationFixture) updateConfigMap(t *testing.T, name, schema string) {
	t.Helper()
	require.NoError(t, f.configmaps.Update(requestValidationConfigMapResource(name, schema), []string{"default", name}))
}

func (f *requestValidationFixture) renderAndCommit(t *testing.T) *RenderResult {
	t.Helper()
	result, err := f.service.Render(t.Context(), f.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, result.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, f.service)
	return result
}

func (f *requestValidationFixture) assertExecutions(t *testing.T, name string, expected uint64) {
	t.Helper()
	component := f.service.incremental.components[requestValidationPublicationComponent]
	query := componentQueryKey(&component, "ingresses", "default", name)
	assert.Equal(t, expected, f.service.incremental.graph.Counters(query).Executions)
}
