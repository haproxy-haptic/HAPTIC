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
	"log/slog"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/helpers"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/typebootstrap"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/incremental"
	k8sstore "github.com/haproxy-haptic/HAPTIC/pkg/k8s/store"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

const annotationConflictChartSnippetName = "features-950-annotation-family-feature-conflict"

type annotationConflictChartFixture struct {
	config    *config.Config
	service   *RenderService
	engine    *dynamicBindingCountingEngine
	ingresses *k8sstore.MemoryStore
	provider  stores.StoreProvider
}

func TestAnnotationConflictChartReplaysEffectsWithoutUnwarrantedExecution(t *testing.T) {
	fixture := newAnnotationConflictChartFixture(t)
	fixture.addIngress(t, annotationConflictIngress("conflict", true, "v1"))
	fixture.addIngress(t, annotationConflictIngress("safe", false, "v1"))

	first := fixture.renderAndCommitCacheReady(t)
	firstEvents := requireRenderEvents(t, first)
	assert.Equal(t, []templating.RenderedEvent{annotationConflictEvent("conflict")}, firstEvents)
	assert.Equal(t, map[string]int{"ingresses/conflict": 1, "ingresses/safe": 1}, fixture.engine.executionCounts())

	warm := fixture.renderAndCommitCacheReady(t)
	assert.Equal(t, firstEvents, requireRenderEvents(t, warm))
	assert.Equal(t, map[string]int{"ingresses/conflict": 1, "ingresses/safe": 1}, fixture.engine.executionCounts())

	fixture.config.TemplatingSettings.ExtraContext["unrelated"] = "changed"
	fixture.annotationLibraries()["futureFamily"] = true
	irrelevant := fixture.renderAndCommitCacheReady(t)
	assert.Equal(t, firstEvents, requireRenderEvents(t, irrelevant))
	assert.Equal(t, map[string]int{"ingresses/conflict": 1, "ingresses/safe": 1}, fixture.engine.executionCounts())

	fixture.updateIngress(t, annotationConflictIngress("safe", false, "v2"))
	changed := fixture.renderAndCommitCacheReady(t)
	assert.Equal(t, firstEvents, requireRenderEvents(t, changed))
	assert.Equal(t, map[string]int{"ingresses/conflict": 1, "ingresses/safe": 2}, fixture.engine.executionCounts())
	assert.Equal(t, uint64(1), fixture.counters("conflict").Executions)
	assert.Equal(t, uint64(2), fixture.counters("safe").Executions)
}

func TestAnnotationConflictChartFamilyPropsInvalidateAndRetireBindings(t *testing.T) {
	fixture := newAnnotationConflictChartFixture(t)
	fixture.addIngress(t, annotationConflictIngress("conflict", true, "v1"))
	fixture.addIngress(t, annotationConflictIngress("safe", false, "v1"))
	require.Len(t, requireRenderEvents(t, fixture.renderAndCommitCacheReady(t)), 1)

	fixture.setEnabledFamilies("haptic", "haproxytech", "nginx")
	enabledChanged := fixture.renderAndCommitCacheReady(t)
	assert.Equal(t, []templating.RenderedEvent{annotationConflictEvent("conflict")}, requireRenderEvents(t, enabledChanged))
	assert.Equal(t, map[string]int{"ingresses/conflict": 2, "ingresses/safe": 2}, fixture.engine.executionCounts())

	fixture.setEnabledFamilies("haptic", "haproxytech")
	conflictDisabled := fixture.renderAndCommitCacheReady(t)
	assert.Empty(t, requireRenderEvents(t, conflictDisabled))
	assert.Equal(t, map[string]int{"ingresses/conflict": 3, "ingresses/safe": 3}, fixture.engine.executionCounts())

	conflictQuery := fixture.query("conflict")
	safeQuery := fixture.query("safe")
	fixture.setEnabledFamilies("haptic")
	retired := fixture.renderAndCommitCacheReady(t)
	assert.Empty(t, requireRenderEvents(t, retired))
	assert.Equal(t, map[string]int{"ingresses/conflict": 3, "ingresses/safe": 3}, fixture.engine.executionCounts())
	_, conflictCached := fixture.service.incremental.graph.Value(conflictQuery)
	_, safeCached := fixture.service.incremental.graph.Value(safeQuery)
	assert.False(t, conflictCached)
	assert.False(t, safeCached)
	assert.Zero(t, fixture.service.incremental.graph.Counters(conflictQuery))
	assert.Zero(t, fixture.service.incremental.graph.Counters(safeQuery))

	fixture.setEnabledFamilies("haptic", "nginx")
	rebound := fixture.renderAndCommitCacheReady(t)
	assert.Equal(t, []templating.RenderedEvent{annotationConflictEvent("conflict")}, requireRenderEvents(t, rebound))
	assert.Equal(t, map[string]int{"ingresses/conflict": 4, "ingresses/safe": 4}, fixture.engine.executionCounts())
	assert.Equal(t, uint64(1), fixture.service.incremental.graph.Counters(conflictQuery).Executions)
	assert.Equal(t, uint64(1), fixture.service.incremental.graph.Counters(safeQuery).Executions)
}

func TestAnnotationConflictChartFailedRendersDoNotPoisonReconcileState(t *testing.T) {
	fixture := newAnnotationConflictChartFixture(t)
	fixture.addIngress(t, annotationConflictIngress("conflict", true, "v1"))
	fixture.addIngress(t, annotationConflictIngress("safe", false, "v1"))
	baseline := fixture.renderAndCommitCacheReady(t)
	baselineEvents := requireRenderEvents(t, baseline)
	assert.Equal(t, []templating.RenderedEvent{annotationConflictEvent("conflict")}, baselineEvents)
	conflictQuery := fixture.query("conflict")
	safeQuery := fixture.query("safe")
	baselineConflictCounters := fixture.service.incremental.graph.Counters(conflictQuery)
	baselineSafeCounters := fixture.service.incremental.graph.Counters(safeQuery)

	proposed := annotationConflictIngress("safe", true, "admission")
	admissionProvider := stores.NewOverlayStoreProvider(
		fixture.provider,
		stores.NewValidationContext(map[string]*stores.StoreOverlay{
			"ingresses": stores.NewStoreOverlayForUpdate(&unstructured.Unstructured{Object: proposed}),
		}),
	)
	failedAdmission, err := fixture.service.Render(
		t.Context(),
		admissionProvider,
		rendercontext.RenderModeAdmission,
		rendercontext.WithAdmissionSubject("ingresses", "default", "safe"),
	)
	require.ErrorContains(t, err, annotationConflictEvent("safe").Message)
	assert.Nil(t, failedAdmission)
	assert.Equal(t, baselineConflictCounters, fixture.service.incremental.graph.Counters(conflictQuery))
	assert.Equal(t, baselineSafeCounters, fixture.service.incremental.graph.Counters(safeQuery))

	executionsAfterAdmission := fixture.engine.executionCounts()
	afterAdmission := fixture.renderAndCommitCacheReady(t)
	assert.Equal(t, baselineEvents, requireRenderEvents(t, afterAdmission))
	assert.Equal(t, executionsAfterAdmission, fixture.engine.executionCounts())

	fixture.updateIngress(t, proposed)
	committedConflict := fixture.renderAndCommitCacheReady(t)
	assert.Equal(t, []templating.RenderedEvent{
		annotationConflictEvent("conflict"),
		annotationConflictEvent("safe"),
	}, requireRenderEvents(t, committedConflict))
	assert.Equal(t, uint64(1), fixture.service.incremental.graph.Counters(conflictQuery).Executions)
	assert.Equal(t, uint64(2), fixture.service.incremental.graph.Counters(safeQuery).Executions)

	fixture.updateIngress(t, annotationConflictIngress("safe", false, "scratch"))
	fixture.config.TemplatingSettings.ExtraContext["failAfterComponent"] = true
	beforeScratchFailure := fixture.engine.executionCounts()
	failedScratch, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.ErrorContains(t, err, "forced failure after annotation conflict component")
	assert.Nil(t, failedScratch)
	assert.Equal(t, uint64(1), fixture.service.incremental.graph.Counters(conflictQuery).Executions)
	assert.Equal(t, uint64(2), fixture.service.incremental.graph.Counters(safeQuery).Executions)
	assert.Equal(t, beforeScratchFailure["ingresses/safe"]+1, fixture.engine.executionCounts()["ingresses/safe"])

	fixture.config.TemplatingSettings.ExtraContext["failAfterComponent"] = false
	beforeRetry := fixture.engine.executionCounts()
	afterScratchFailure := fixture.renderAndCommitCacheReady(t)
	assert.Equal(t, []templating.RenderedEvent{annotationConflictEvent("conflict")}, requireRenderEvents(t, afterScratchFailure))
	assert.Equal(t, beforeRetry["ingresses/conflict"], fixture.engine.executionCounts()["ingresses/conflict"])
	assert.Equal(t, beforeRetry["ingresses/safe"]+1, fixture.engine.executionCounts()["ingresses/safe"])
	assert.Equal(t, uint64(1), fixture.service.incremental.graph.Counters(conflictQuery).Executions)
	assert.Equal(t, uint64(3), fixture.service.incremental.graph.Counters(safeQuery).Executions)
}

func newAnnotationConflictChartFixture(t *testing.T) *annotationConflictChartFixture {
	t.Helper()
	snippet := loadAnnotationConflictChartSnippet(t)
	cfg := &config.Config{
		Dataplane: testDataplaneConfig(),
		TemplatingSettings: config.TemplatingSettings{ExtraContext: map[string]any{
			"annotationLibraries": enabledAnnotationLibraries("haptic", "nginx"),
			"failAfterComponent":  false,
		}},
		WatchedResources: map[string]config.WatchedResource{
			"ingresses": {
				APIVersion: "networking.k8s.io/v1",
				Resources:  "ingresses",
				IndexBy:    []string{"metadata.namespace", "metadata.name"},
			},
		},
		TemplateSnippets: map[string]config.TemplateSnippet{
			annotationConflictChartSnippetName: snippet,
		},
		HAProxyConfig: config.HAProxyConfig{Template: `{{ render "features-950-annotation-family-feature-conflict" }}{%%
if tostring(extraContext | dig("failAfterComponent") | fallback(false)) == "true" {
  fail("forced failure after annotation conflict component")
}
%%}`},
	}
	declarations := helpers.BuildAdditionalDeclarations(cfg, &typebootstrap.Result{
		Types:  map[string]reflect.Type{},
		Kinds:  map[string]string{},
		Errors: map[string]error{},
	})
	baseEngine, err := helpers.NewEngineFromConfigWithOptions(cfg, nil, nil, declarations, helpers.EngineOptions{})
	require.NoError(t, err)
	engine := newDynamicBindingCountingEngine(t, baseEngine)
	service := NewRenderService(&RenderServiceConfig{Engine: engine, Config: cfg, Logger: slog.Default()})
	ingresses := k8sstore.NewMemoryStore(2)
	return &annotationConflictChartFixture{
		config:    cfg,
		service:   service,
		engine:    engine,
		ingresses: ingresses,
		provider:  stores.NewRealStoreProvider(map[string]stores.Store{"ingresses": ingresses}),
	}
}

func loadAnnotationConflictChartSnippet(t *testing.T) config.TemplateSnippet {
	t.Helper()
	snippets := loadChartSnippets(t, chartLibrariesRoot(t), map[string][]string{
		"ingress-annotations-compat/library.yaml": {annotationConflictChartSnippetName},
	})
	snippet := snippets[annotationConflictChartSnippetName]
	require.NotNil(t, snippet.Incremental)
	return snippet
}

func (f *annotationConflictChartFixture) addIngress(t *testing.T, ingress map[string]any) {
	t.Helper()
	name := ingress["metadata"].(map[string]any)["name"].(string)
	require.NoError(t, f.ingresses.Add(ingress, []string{"default", name}))
}

func (f *annotationConflictChartFixture) updateIngress(t *testing.T, ingress map[string]any) {
	t.Helper()
	name := ingress["metadata"].(map[string]any)["name"].(string)
	require.NoError(t, f.ingresses.Update(ingress, []string{"default", name}))
}

func (f *annotationConflictChartFixture) annotationLibraries() map[string]any {
	return f.config.TemplatingSettings.ExtraContext["annotationLibraries"].(map[string]any)
}

func (f *annotationConflictChartFixture) setEnabledFamilies(families ...string) {
	f.config.TemplatingSettings.ExtraContext["annotationLibraries"] = enabledAnnotationLibraries(families...)
}

func (f *annotationConflictChartFixture) query(name string) incremental.QueryKey {
	tempComponent5 := f.service.incremental.components[annotationConflictChartSnippetName]
	return componentQueryKey(
		&tempComponent5,
		"ingresses",
		"default",
		name,
	)
}

func (f *annotationConflictChartFixture) counters(name string) incremental.NodeCounters {
	return f.service.incremental.graph.Counters(f.query(name))
}

func (f *annotationConflictChartFixture) renderAndCommitCacheReady(
	t *testing.T,
) *RenderResult {
	t.Helper()
	result, err := f.service.Render(t.Context(), f.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NotNil(t, result.InputTransaction)
	require.NoError(t, result.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, f.service)
	return result
}

func enabledAnnotationLibraries(families ...string) map[string]any {
	result := map[string]any{
		"haptic":         false,
		"haproxyIngress": false,
		"haproxytech":    false,
		"nginx":          false,
	}
	for _, family := range families {
		result[family] = true
	}
	return result
}

func annotationConflictIngress(name string, conflict bool, revision string) map[string]any {
	annotations := map[string]any{}
	if conflict {
		annotations["haproxy-haptic.org/cors-enable"] = "true"
		annotations["nginx.ingress.kubernetes.io/enable-cors"] = "true"
	}
	return map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "Ingress",
		"metadata": map[string]any{
			"namespace":   "default",
			"name":        name,
			"annotations": annotations,
		},
		"spec": map[string]any{"revision": revision},
	}
}

func annotationConflictEvent(name string) templating.RenderedEvent {
	message := "Ingress default/" + name + " configures the CORS feature through annotations from multiple families " +
		"(haproxy-haptic.org/cors-enable; nginx.ingress.kubernetes.io/enable-cors). " +
		"Configure each feature through a single annotation family: mixing families for one feature is rejected even when " +
		"the values agree, because the effective configuration is order-dependent."
	return templating.RenderedEvent{
		Namespace:  "default",
		Name:       name,
		APIVersion: "networking.k8s.io/v1",
		Kind:       "Ingress",
		Type:       templating.EventTypeWarning,
		Reason:     "AnnotationFamilyConflict",
		Message:    message,
	}
}
