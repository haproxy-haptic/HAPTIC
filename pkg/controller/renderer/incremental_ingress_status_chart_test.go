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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/helpers"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/typebootstrap"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	k8sstore "github.com/haproxy-haptic/HAPTIC/pkg/k8s/store"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

const (
	ingressStatusAddressComponent = "status-patches-190-ingress-addresses"
	ingressStatusPatchComponent   = "status-patches-200-ingress"
)

type ingressStatusChartFixture struct {
	service            *RenderService
	engine             *dynamicBindingCountingEngine
	ingresses          *k8sstore.MemoryStore
	controllerServices *k8sstore.MemoryStore
	provider           stores.StoreProvider
}

func TestIngressStatusChartIncrementalLifecycleAndFanout(t *testing.T) {
	fixture := newIngressStatusChartFixture(t)
	fixture.addIngress(t, ingressStatusResource("a", "v1"))
	fixture.addIngress(t, ingressStatusResource("b", "v1"))
	fixture.addControllerService(t, ingressStatusService("lb", []string{"10.0.0.1"}, "203.0.113.10", ""))

	first := fixture.renderAndCommit(t)
	assertIngressStatusAddresses(t, first, map[string][]any{
		"a": {map[string]any{"ip": "203.0.113.10"}},
		"b": {map[string]any{"ip": "203.0.113.10"}},
	})
	aExec := fixture.executions(ingressStatusPatchComponent, "ingresses", "a")
	bExec := fixture.executions(ingressStatusPatchComponent, "ingresses", "b")
	serviceExec := fixture.executions(ingressStatusAddressComponent, "controller_services", "lb")

	warm := fixture.renderAndCommit(t)
	assert.Equal(t, statusPatchesByName(t, first), statusPatchesByName(t, warm))
	assert.Equal(t, aExec, fixture.executions(ingressStatusPatchComponent, "ingresses", "a"))
	assert.Equal(t, bExec, fixture.executions(ingressStatusPatchComponent, "ingresses", "b"))
	assert.Equal(t, serviceExec, fixture.executions(ingressStatusAddressComponent, "controller_services", "lb"))

	fixture.updateControllerService(t, ingressStatusService("lb", []string{"10.0.0.1"}, "203.0.113.10", "irrelevant"))
	unchangedAddresses := fixture.renderAndCommit(t)
	assert.Equal(t, statusPatchesByName(t, first), statusPatchesByName(t, unchangedAddresses))
	assert.Equal(t, aExec, fixture.executions(ingressStatusPatchComponent, "ingresses", "a"))
	assert.Equal(t, bExec, fixture.executions(ingressStatusPatchComponent, "ingresses", "b"))
	assert.Equal(t, serviceExec+1, fixture.executions(ingressStatusAddressComponent, "controller_services", "lb"))

	fixture.updateControllerService(t, ingressStatusService("lb", []string{"10.0.0.1"}, "203.0.113.20", "changed"))
	aborted, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	assertIngressStatusAddresses(t, aborted, map[string][]any{
		"a": {map[string]any{"ip": "203.0.113.20"}},
		"b": {map[string]any{"ip": "203.0.113.20"}},
	})
	aborted.InputTransaction.Abort()
	assert.Equal(t, aExec, fixture.executions(ingressStatusPatchComponent, "ingresses", "a"))
	assert.Equal(t, bExec, fixture.executions(ingressStatusPatchComponent, "ingresses", "b"))

	retried := fixture.renderAndCommit(t)
	assertIngressStatusAddresses(t, retried, map[string][]any{
		"a": {map[string]any{"ip": "203.0.113.20"}},
		"b": {map[string]any{"ip": "203.0.113.20"}},
	})
	assert.Equal(t, aExec+1, fixture.executions(ingressStatusPatchComponent, "ingresses", "a"))
	assert.Equal(t, bExec+1, fixture.executions(ingressStatusPatchComponent, "ingresses", "b"))

	require.NoError(t, fixture.ingresses.Delete("default", "b", []string{"default", "b"}))
	deleted := fixture.renderAndCommit(t)
	assertIngressStatusAddresses(t, deleted, map[string][]any{
		"a": {map[string]any{"ip": "203.0.113.20"}},
	})
	assert.Zero(t, fixture.executions(ingressStatusPatchComponent, "ingresses", "b"))
}

func TestIngressStatusChartGlobalLoadBalancerFallbackAndColdParity(t *testing.T) {
	fixture := newIngressStatusChartFixture(t)
	fixture.addIngress(t, ingressStatusResource("a", "v1"))
	fixture.addControllerService(t, ingressStatusService("internal", []string{"10.0.0.1"}, "", ""))
	fixture.addControllerService(t, ingressStatusService("public", []string{"10.0.0.2"}, "203.0.113.10", ""))

	warm := fixture.renderAndCommit(t)
	assertIngressStatusAddresses(t, warm, map[string][]any{
		"a": {map[string]any{"ip": "203.0.113.10"}},
	})
	cold, err := renderServiceStaticCold(t, fixture.service, fixture.provider)
	require.NoError(t, err)
	cold.InputTransaction.Abort()
	assert.Equal(t, statusPatchesByName(t, warm), statusPatchesByName(t, cold))

	require.NoError(t, fixture.controllerServices.Delete("default", "public", []string{"default", "public"}))
	fallback := fixture.renderAndCommit(t)
	assertIngressStatusAddresses(t, fallback, map[string][]any{
		"a": {map[string]any{"ip": "10.0.0.1"}},
	})
}

func newIngressStatusChartFixture(t *testing.T) *ingressStatusChartFixture {
	t.Helper()
	cfg := &config.Config{
		Dataplane: testDataplaneConfig(),
		TemplatingSettings: config.TemplatingSettings{ExtraContext: map[string]any{
			"statusPatches": map[string]any{"enabled": true},
		}},
		WatchedResources: map[string]config.WatchedResource{
			"ingresses": {
				APIVersion: "networking.k8s.io/v1", Resources: "ingresses",
				IndexBy: []string{"metadata.namespace", "metadata.name"},
			},
			"controller_services": {
				APIVersion: "v1", Resources: "services",
				IndexBy: []string{"metadata.namespace", "metadata.name"},
			},
		},
		TemplateSnippets: loadIngressStatusChartSnippets(t),
		HAProxyConfig: config.HAProxyConfig{Template: `global
{{ render "status-patches-190-ingress-addresses" }}{{ render "status-patches-200-ingress" }}`},
	}
	declarations := helpers.BuildAdditionalDeclarations(cfg, &typebootstrap.Result{
		Types: map[string]reflect.Type{}, Kinds: map[string]string{}, Errors: map[string]error{},
	})
	baseEngine, err := helpers.NewEngineFromConfigWithOptions(cfg, nil, nil, declarations, helpers.EngineOptions{})
	require.NoError(t, err)
	engine := newDynamicBindingCountingEngine(t, baseEngine)
	ingresses := k8sstore.NewMemoryStore(2)
	controllerServices := k8sstore.NewMemoryStore(2)
	return &ingressStatusChartFixture{
		service: NewRenderService(&RenderServiceConfig{Engine: engine, Config: cfg, Logger: slog.Default()}),
		engine:  engine, ingresses: ingresses, controllerServices: controllerServices,
		provider: stores.NewRealStoreProvider(map[string]stores.Store{
			"ingresses": ingresses, "controller_services": controllerServices,
		}),
	}
}

func loadIngressStatusChartSnippets(t *testing.T) map[string]config.TemplateSnippet {
	t.Helper()
	names := []string{ingressStatusAddressComponent, ingressStatusPatchComponent}
	snippets := loadChartSnippets(t, chartLibrariesRoot(t), map[string][]string{
		"ingress/library.yaml": names,
	})
	for _, name := range names {
		require.NotNil(t, snippets[name].Incremental, name)
	}
	return snippets
}

func ingressStatusResource(name, revision string) map[string]any {
	return map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "Ingress",
		"metadata": map[string]any{
			"namespace": "default", "name": name,
			"annotations": map[string]any{"test.haptic/revision": revision},
		},
		"spec": map[string]any{},
	}
}

func ingressStatusService(name string, clusterIPs []string, loadBalancerIP, revision string) map[string]any {
	addresses := []any{}
	clusterValues := make([]any, len(clusterIPs))
	for index := range clusterIPs {
		clusterValues[index] = clusterIPs[index]
	}
	if loadBalancerIP != "" {
		addresses = append(addresses, map[string]any{"ip": loadBalancerIP})
	}
	return map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{
			"namespace": "default", "name": name,
			"annotations": map[string]any{"test.haptic/revision": revision},
		},
		"spec":   map[string]any{"clusterIPs": clusterValues},
		"status": map[string]any{"loadBalancer": map[string]any{"ingress": addresses}},
	}
}

func (f *ingressStatusChartFixture) addIngress(t *testing.T, resource map[string]any) {
	t.Helper()
	name := resource["metadata"].(map[string]any)["name"].(string)
	require.NoError(t, f.ingresses.Add(resource, []string{"default", name}))
}

func (f *ingressStatusChartFixture) addControllerService(t *testing.T, resource map[string]any) {
	t.Helper()
	name := resource["metadata"].(map[string]any)["name"].(string)
	require.NoError(t, f.controllerServices.Add(resource, []string{"default", name}))
}

func (f *ingressStatusChartFixture) updateControllerService(t *testing.T, resource map[string]any) {
	t.Helper()
	name := resource["metadata"].(map[string]any)["name"].(string)
	require.NoError(t, f.controllerServices.Update(resource, []string{"default", name}))
}

func (f *ingressStatusChartFixture) renderAndCommit(t *testing.T) *RenderResult {
	t.Helper()
	result, err := f.service.Render(t.Context(), f.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, result.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, f.service)
	return result
}

func (f *ingressStatusChartFixture) executions(component, source, name string) uint64 {
	definition := f.service.incremental.components[component]
	return f.service.incremental.graph.Counters(
		componentQueryKey(&definition, source, "default", name),
	).Executions
}

func assertIngressStatusAddresses(t *testing.T, result *RenderResult, expected map[string][]any) {
	t.Helper()
	patches := statusPatchesByName(t, result)
	require.Len(t, patches, len(expected))
	for name, addresses := range expected {
		patch, exists := patches[name]
		require.True(t, exists, name)
		deployed := patch.Variants["deployed"]
		loadBalancer := deployed["loadBalancer"].(map[string]any)
		assert.Equal(t, addresses, loadBalancer["ingress"], name)
		assert.Equal(t, []any{}, patch.Variants["deployFailed"]["loadBalancer"].(map[string]any)["ingress"], name)
	}
}

func statusPatchesByName(t *testing.T, result *RenderResult) map[string]templating.StatusPatch {
	t.Helper()
	materialized := materializedStatusPatches(t, result)
	patches := make(map[string]templating.StatusPatch, len(materialized))
	for index := range materialized {
		patch := &materialized[index]
		_, duplicate := patches[patch.Name]
		require.False(t, duplicate, patch.Name)
		patches[patch.Name] = *patch
	}
	return patches
}
