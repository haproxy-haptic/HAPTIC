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
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/helpers"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/typebootstrap"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/auxiliaryfiles"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/planblob"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
	k8sstore "github.com/haproxy-haptic/HAPTIC/pkg/k8s/store"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
)

const sensitiveRenderSecret = "hunter2-route-key"

// A declared map that marks itself sensitive and a registered one marked by
// the main template; the hosts map is plain and changes independently.
func newSensitiveMapRenderFixture(t *testing.T) (*RenderService, stores.StoreProvider, *k8sstore.MemoryStore) {
	t.Helper()
	cfg := &config.Config{
		Dataplane: testDataplaneConfig(),
		WatchedResources: map[string]config.WatchedResource{
			"hosts": {
				APIVersion: "example.test/v1", Resources: "hosts",
				IndexBy: []string{"metadata.namespace", "metadata.name"},
			},
		},
		TemplateSnippets: map[string]config.TemplateSnippet{
			"host-lines": {
				Name: "host-lines", Requires: []string{"hosts"},
				Incremental: &config.IncrementalTemplate{Source: "hosts", Group: "hosts"},
				Template:    `{{ item | dig_string("", "metadata", "name") }} {{ item | dig_string("", "spec", "value") }}` + "\n",
			},
		},
		HAProxyConfig: config.HAProxyConfig{Template: `global
  daemon
{%- var _, _ = fileRegistry.Register("map", "registered.map", "route-a ` + sensitiveRenderSecret + `\n") %}
{%- var _ = planRegistry.SensitiveMap("registered.map") %}
hosts:
{{ render "host-lines" }}`},
		Maps: map[string]config.MapFile{
			"hosts.map":   {Template: `{{ render "host-lines" }}`},
			"secrets.map": {Template: `{{- planRegistry.SensitiveMap("secrets.map") -}}route-b ` + sensitiveRenderSecret + "\n"},
		},
	}
	declarations := helpers.BuildAdditionalDeclarations(cfg, &typebootstrap.Result{
		Types: map[string]reflect.Type{}, Kinds: map[string]string{}, Errors: map[string]error{},
	})
	engine, err := helpers.NewEngineFromConfigWithOptions(cfg, nil, nil, declarations, helpers.EngineOptions{})
	require.NoError(t, err)
	_, logger := testutil.NewTestBusAndLogger()
	service := NewRenderService(&RenderServiceConfig{
		Engine: engine, Config: cfg, Logger: logger, SecretFileSealKey: []byte("seal key"),
	})
	hosts := k8sstore.NewMemoryStore(2)
	require.NoError(t, hosts.Add(
		incrementalTestResource("default", "h1", map[string]any{"value": "v1"}), []string{"default", "h1"},
	))
	return service, stores.NewRealStoreProvider(map[string]stores.Store{"hosts": hosts}), hosts
}

func renderSensitive(t *testing.T, service *RenderService, provider stores.StoreProvider) *RenderResult {
	t.Helper()
	result, err := service.Render(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, result.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, service)
	return result
}

func assertSensitiveMapsSealed(t *testing.T, result *RenderResult) {
	t.Helper()
	files, err := result.MaterializeAuxiliaryFiles()
	require.NoError(t, err)
	plain := make([]string, 0, len(files.MapFiles))
	for _, file := range files.MapFiles {
		plain = append(plain, file.Path)
	}
	assert.Equal(t, []string{"hosts.map"}, plain)
	require.Len(t, files.SensitiveMapFiles, 2)
	for _, file := range files.SensitiveMapFiles {
		assert.True(t, auxiliaryfiles.IsKeyedSeal(file.Seal), file.Path)
		assert.Contains(t, file.Content, sensitiveRenderSecret)
	}

	plan, err := result.MaterializePlan()
	require.NoError(t, err)
	assert.NotContains(t, plan.Maps, "maps/secrets.map")
	assert.NotContains(t, plan.Maps, "maps/registered.map")
	assert.NotContains(t, string(plan.Canonical()), sensitiveRenderSecret)
	checkpoint, err := planblob.EncodeCheckpoint(plan)
	require.NoError(t, err)
	decoded, err := planblob.DecodeCheckpoint(checkpoint, plan.Files[0].Content, planFileContents(plan))
	require.NoError(t, err)
	assert.Contains(t, decoded.Maps, "maps/hosts.map")
	assert.NotContains(t, decoded.Maps, "maps/secrets.map")
}

func planFileContents(plan *renderplan.Plan) map[string]string {
	contents := make(map[string]string, len(plan.Files))
	for _, file := range plan.Files {
		contents[file.Path] = file.Content
	}
	return contents
}

func TestSensitiveMapsStaySealedAcrossReusedRenders(t *testing.T) {
	service, provider, hosts := newSensitiveMapRenderFixture(t)
	assertSensitiveMapsSealed(t, renderSensitive(t, service, provider))
	replayed := renderSensitive(t, service, provider)
	assert.Equal(t, "replay", replayed.CacheState)
	assertSensitiveMapsSealed(t, replayed)

	require.NoError(t, hosts.Update(
		incrementalTestResource("default", "h1", map[string]any{"value": "v2"}), []string{"default", "h1"},
	))
	assertSensitiveMapsSealed(t, renderSensitive(t, service, provider))
}
