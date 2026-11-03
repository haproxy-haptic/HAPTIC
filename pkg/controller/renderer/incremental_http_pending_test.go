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
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/helpers"
	controllerhttpstore "github.com/haproxy-haptic/HAPTIC/pkg/controller/httpstore"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/typebootstrap"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	purehttpstore "github.com/haproxy-haptic/HAPTIC/pkg/httpstore"
	k8sstore "github.com/haproxy-haptic/HAPTIC/pkg/k8s/store"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
)

// A component tells content waiting for acceptance apart from a failed fetch
// through http.Pending, so it can stay silent instead of reporting a failure.
func TestComponentSeesWithheldContentAsPending(t *testing.T) {
	pages := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("page"))
	}))
	t.Cleanup(pages.Close)
	cfg := &config.Config{
		Dataplane: testDataplaneConfig(),
		WatchedResources: map[string]config.WatchedResource{
			"routes": {APIVersion: "example.test/v1", Resources: "routes", IndexBy: []string{"metadata.namespace", "metadata.name"}},
		},
		TemplateSnippets: map[string]config.TemplateSnippet{
			"routes": {
				Name:        "routes",
				Requires:    []string{"routes"},
				Incremental: &config.IncrementalTemplate{Source: "routes"},
				Template: `{%- var url = item | dig_string("", "spec", "url") -%}
{%- var page, _ = http.Fetch(url, map[string]any{"retries": 1, "timeout": "1s"}) -%}
{{ item | dig_string("", "metadata", "name") }}={{ page }} pending={{ http.Pending(url) }}
`,
			},
		},
		HAProxyConfig: config.HAProxyConfig{Template: `{{ render "routes" }}`},
	}
	declarations := helpers.BuildAdditionalDeclarations(cfg, &typebootstrap.Result{
		Types: map[string]reflect.Type{}, Kinds: map[string]string{}, Errors: map[string]error{},
	})
	engine, err := helpers.NewEngineFromConfigWithOptions(cfg, nil, nil, declarations, helpers.EngineOptions{})
	require.NoError(t, err)
	bus, logger := testutil.NewTestBusAndLogger()
	component := controllerhttpstore.New(bus, logger, -time.Hour)
	service := NewRenderService(&RenderServiceConfig{
		Engine: engine, Config: cfg, Logger: logger, HTTPStoreComponent: component,
	})
	routes := k8sstore.NewMemoryStore(2)
	require.NoError(t, routes.Add(
		incrementalTestResource("default", "a", map[string]any{"url": pages.URL + "/a"}),
		[]string{"default", "a"},
	))
	provider := stores.NewRealStoreProvider(map[string]stores.Store{"routes": routes})
	renderAndCommitIncrementalCacheReady(t, service, provider)
	renderAndCommitIncrementalCacheReady(t, service, provider)
	require.NoError(t, routes.Add(
		incrementalTestResource("default", "b", map[string]any{"url": pages.URL + "/b"}),
		[]string{"default", "b"},
	))

	withheld, err := service.Render(controllerhttpstore.WithCandidatesWithheld(t.Context()), provider,
		rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.Contains(t, withheld.HAProxyConfig, "a=page pending=false")
	require.Contains(t, withheld.HAProxyConfig, "b= pending=true")
	require.NoError(t, withheld.InputTransaction.Commit(t.Context()))

	accepting, err := service.Render(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.Contains(t, accepting.HAProxyConfig, "b=page pending=false")
	require.NoError(t, accepting.InputTransaction.Commit(t.Context()))
	require.NoError(t, service.RetireIncrementalCache())
}

func TestPendingHTTPOverlayValidatesFrozenContentWithoutPublishing(t *testing.T) {
	fixture := newIncrementalHTTPTestFixture(t)
	require.Equal(t, "a=first\nb=stable\n", fixture.render(t))
	require.Equal(t, "a=first\nb=stable\n", fixture.render(t))
	acceptedCache := fixture.service.incremental.snapshot
	store := fixture.httpComponent.GetStore()
	fixture.bodyA.Store("candidate")
	candidate, err := store.RefreshURLVersion(t.Context(), fixture.urlA)
	require.NoError(t, err)
	require.NotNil(t, candidate)
	overlay := purehttpstore.NewHTTPOverlay(store)

	require.True(t, store.RejectPendingVersion(fixture.urlA, candidate.Checksum, candidate.Revision))
	fixture.bodyA.Store("replacement")
	replacement, err := store.RefreshURLVersion(t.Context(), fixture.urlA)
	require.NoError(t, err)
	require.NotNil(t, replacement)
	provider := stores.NewOverlayStoreProvider(fixture.provider, stores.NewValidationContext(nil).WithHTTPOverlay(overlay))
	result, err := fixture.service.Render(t.Context(), provider, rendercontext.RenderModeAdmission)
	require.NoError(t, err)
	require.Equal(t, "a=candidate\nb=stable\n", result.HAProxyConfig)
	require.NoError(t, result.InputTransaction.Commit(t.Context()))
	require.Same(t, acceptedCache, fixture.service.incremental.snapshot)
	accepted, ok := store.Get(fixture.urlA)
	require.True(t, ok)
	require.Equal(t, "first", accepted)
	require.False(t, store.PromotePendingVersion(fixture.urlA, candidate.Checksum, candidate.Revision))
	require.Equal(t, "a=first\nb=stable\n", fixture.render(t))

	require.True(t, store.PromotePendingVersion(fixture.urlA, replacement.Checksum, replacement.Revision))
	require.Equal(t, "a=replacement\nb=stable\n", fixture.render(t))
	require.NoError(t, fixture.service.RetireIncrementalCache())
}

func TestPendingHTTPOverlayRequiresMatchingSourceAndValidationMode(t *testing.T) {
	fixture := newIncrementalHTTPTestFixture(t)
	require.Equal(t, "a=first\nb=stable\n", fixture.render(t))
	fixture.bodyA.Store("candidate")
	_, err := fixture.httpComponent.GetStore().RefreshURLVersion(t.Context(), fixture.urlA)
	require.NoError(t, err)
	overlay := purehttpstore.NewHTTPOverlay(fixture.httpComponent.GetStore())
	provider := stores.NewOverlayStoreProvider(fixture.provider, stores.NewValidationContext(nil).WithHTTPOverlay(overlay))
	_, err = fixture.service.Render(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.ErrorIs(t, err, errIncrementalUnsupported)

	other := newIncrementalHTTPTestFixture(t)
	require.Equal(t, "a=first\nb=stable\n", other.render(t))
	foreignProvider := stores.NewOverlayStoreProvider(other.provider, stores.NewValidationContext(nil).WithHTTPOverlay(overlay))
	_, err = other.service.Render(t.Context(), foreignProvider, rendercontext.RenderModeAdmission)
	require.ErrorIs(t, err, errIncrementalUnsupported)
	require.NoError(t, fixture.service.RetireIncrementalCache())
	require.NoError(t, other.service.RetireIncrementalCache())
}

func TestAdmissionHTTPColdRestartSurvivesTemplateErrorHandling(t *testing.T) {
	fixture := newIncrementalHTTPTestFixtureWithTemplate(t, `{%- var body, fetchErr = http.Fetch(item | dig_string("", "spec", "url"), map[string]any{"critical": true}) -%}
{%- if fetchErr != nil -%}{{ fail("upstream unavailable") }}{%- end -%}
{%- if body == "invalid" -%}{{ fail("invalid upstream content") }}{%- end -%}
{{ item | dig_string("", "metadata", "name") }}={{ body }}
`)
	require.Equal(t, "a=first\nb=stable\n", fixture.render(t))
	require.Equal(t, "a=first\nb=stable\n", fixture.render(t))
	accepted := fixture.service.incremental.snapshot
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.URL.Path[1:]))
	}))
	t.Cleanup(server.Close)
	for _, body := range []string{"fresh", "invalid"} {
		t.Run(body, func(t *testing.T) {
			url := server.URL + "/" + body
			resource := &unstructured.Unstructured{Object: incrementalTestResource("default", "c", map[string]any{"url": url})}
			provider := stores.NewOverlayStoreProvider(fixture.provider, stores.NewValidationContext(map[string]*stores.StoreOverlay{
				"routes": stores.NewStoreOverlayForCreate(resource),
			}))
			result, err := fixture.service.Render(t.Context(), provider, rendercontext.RenderModeAdmission)
			if body == "invalid" {
				require.ErrorContains(t, err, "invalid upstream content")
				require.Nil(t, result)
			} else {
				require.NoError(t, err)
				require.Equal(t, "a=first\nb=stable\nc=fresh\n", result.HAProxyConfig)
				result.InputTransaction.Abort()
			}
			require.Same(t, accepted, fixture.service.incremental.snapshot)
			_, found := fixture.httpComponent.GetStore().Get(url)
			require.False(t, found)
		})
	}
}
