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
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/helpers"
	controllerhttpstore "github.com/haproxy-haptic/HAPTIC/pkg/controller/httpstore"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/typebootstrap"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
	purehttpstore "github.com/haproxy-haptic/HAPTIC/pkg/httpstore"
	"github.com/haproxy-haptic/HAPTIC/pkg/incremental"
	k8sstore "github.com/haproxy-haptic/HAPTIC/pkg/k8s/store"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
)

func TestRenderServiceIncrementalInvalidatesExactDependencies(t *testing.T) {
	cfg := &config.Config{
		Dataplane: testDataplaneConfig(),
		WatchedResources: map[string]config.WatchedResource{
			"routes": {
				APIVersion: "example.test/v1",
				Resources:  "routes",
				IndexBy:    []string{"metadata.namespace", "metadata.name"},
			},
			"services": {
				APIVersion: "example.test/v1",
				Resources:  "services",
				IndexBy:    []string{"metadata.namespace", "metadata.name"},
			},
		},
		TemplateSnippets: map[string]config.TemplateSnippet{
			"routes": {
				Name:     "routes",
				Requires: []string{"routes", "services"},
				Incremental: &config.IncrementalTemplate{
					Source: "routes",
				},
				Template: `{{ item | dig_string("", "metadata", "name") }}={{ resources.services.GetSingle(item | dig_string("", "metadata", "namespace"), item | dig_string("", "spec", "backend")) | dig_string("", "spec", "value") }}
`,
			},
		},
		HAProxyConfig: config.HAProxyConfig{Template: `{{ render "routes" }}`},
	}
	declarations := helpers.BuildAdditionalDeclarations(cfg, &typebootstrap.Result{
		Types:  map[string]reflect.Type{},
		Kinds:  map[string]string{},
		Errors: map[string]error{},
	})
	engine, err := helpers.NewEngineFromConfigWithOptions(cfg, nil, nil, declarations, helpers.EngineOptions{})
	require.NoError(t, err)
	service := NewRenderService(&RenderServiceConfig{
		Engine: engine,
		Config: cfg,
		Logger: slog.Default(),
	})
	routes := k8sstore.NewMemoryStore(2)
	services := k8sstore.NewMemoryStore(2)
	require.NoError(t, routes.Add(incrementalTestResource("default", "a", map[string]any{"backend": "one"}), []string{"default", "a"}))
	require.NoError(t, routes.Add(incrementalTestResource("default", "b", map[string]any{"backend": "two"}), []string{"default", "b"}))
	require.NoError(t, services.Add(incrementalTestResource("default", "one", map[string]any{"value": "v1"}), []string{"default", "one"}))
	require.NoError(t, services.Add(incrementalTestResource("default", "two", map[string]any{"value": "v2"}), []string{"default", "two"}))
	provider := stores.NewRealStoreProvider(map[string]stores.Store{
		"routes":   routes,
		"services": services,
	})

	first := renderAndCommitIncrementalCacheReady(t, service, provider)
	assert.Equal(t, "a=v1\nb=v2\n", first)
	tempComponent71 := service.incremental.components["routes"]
	aKey := componentQueryKey(&tempComponent71, "routes", "default", "a")
	tempComponent72 := service.incremental.components["routes"]
	bKey := componentQueryKey(&tempComponent72, "routes", "default", "b")
	assert.Equal(t, uint64(1), service.incremental.graph.Counters(aKey).Executions)
	assert.Equal(t, uint64(1), service.incremental.graph.Counters(bKey).Executions)

	second := renderAndCommitIncrementalCacheReady(t, service, provider)
	assert.Equal(t, first, second)
	assert.Equal(t, uint64(1), service.incremental.graph.Counters(aKey).Executions)
	assert.Equal(t, uint64(1), service.incremental.graph.Counters(bKey).Executions)

	require.NoError(t, services.Update(
		incrementalTestResource("default", "one", map[string]any{"value": "changed"}),
		[]string{"default", "one"},
	))
	third := renderAndCommitIncrementalCacheReady(t, service, provider)
	assert.Equal(t, "a=changed\nb=v2\n", third)
	assert.Equal(t, uint64(2), service.incremental.graph.Counters(aKey).Executions)
	assert.Equal(t, uint64(1), service.incremental.graph.Counters(bKey).Executions)

	require.NoError(t, services.Add(
		incrementalTestResource("default", "unreferenced", map[string]any{"value": "ignored"}),
		[]string{"default", "unreferenced"},
	))
	fourth := renderAndCommitIncrementalCacheReady(t, service, provider)
	assert.Equal(t, third, fourth)
	assert.Equal(t, uint64(2), service.incremental.graph.Counters(aKey).Executions)
	assert.Equal(t, uint64(1), service.incremental.graph.Counters(bKey).Executions)
}

func TestRenderServiceIncrementalRejectsResourceResultMutation(t *testing.T) {
	cfg := &config.Config{
		Dataplane: testDataplaneConfig(),
		WatchedResources: map[string]config.WatchedResource{
			"routes": {
				APIVersion: "example.test/v1",
				Resources:  "routes",
				IndexBy:    []string{"metadata.namespace", "metadata.name"},
			},
			"services": {
				APIVersion: "example.test/v1",
				Resources:  "services",
				IndexBy:    []string{"metadata.namespace", "metadata.name"},
			},
		},
		TemplateSnippets: map[string]config.TemplateSnippet{
			"routes": {
				Name:     "routes",
				Requires: []string{"routes", "services"},
				Incremental: &config.IncrementalTemplate{
					Source: "routes",
				},
				Template: `{% var service = resources.services.GetSingle("default", "backend").(map[string]any) %}{% service["spec"].(map[string]any)["value"] = "poison" %}`,
			},
		},
		HAProxyConfig: config.HAProxyConfig{Template: `{{ render "routes" }}`},
	}
	declarations := helpers.BuildAdditionalDeclarations(cfg, &typebootstrap.Result{
		Types: map[string]reflect.Type{}, Kinds: map[string]string{}, Errors: map[string]error{},
	})
	engine, err := helpers.NewEngineFromConfigWithOptions(cfg, nil, nil, declarations, helpers.EngineOptions{})
	require.NoError(t, err)

	for _, cold := range []bool{false, true} {
		name := "warm"
		var routes stores.Store = k8sstore.NewMemoryStore(2)
		var services stores.Store = k8sstore.NewMemoryStore(2)
		if cold {
			name = "cold"
			routes = &coldInputStore{}
			services = &coldInputStore{}
		}
		t.Run(name, func(t *testing.T) {
			route := incrementalTestResource("default", "route", nil)
			service := incrementalTestResource("default", "backend", map[string]any{"value": "original"})
			require.NoError(t, routes.Add(route, []string{"default", "route"}))
			require.NoError(t, services.Add(service, []string{"default", "backend"}))
			renderer := NewRenderService(&RenderServiceConfig{Engine: engine, Config: cfg, Logger: slog.Default()})
			provider := stores.NewRealStoreProvider(map[string]stores.Store{
				"routes": routes, "services": services,
			})
			var err error
			if cold {
				_, _, err = renderStaticColdIncremental(t, cfg, engine, provider)
			} else {
				_, err = renderer.Render(t.Context(), provider, rendercontext.RenderModeReconcile)
			}
			require.ErrorContains(t, err, "mutates an immutable input")

			items, listErr := services.List()
			require.NoError(t, listErr)
			require.Len(t, items, 1)
			assert.Equal(t, "original", items[0].(map[string]any)["spec"].(map[string]any)["value"])
		})
	}
}

type incrementalHTTPTestFixture struct {
	service       *RenderService
	provider      stores.StoreProvider
	httpComponent *controllerhttpstore.Component
	urlA          string
	urlB          string
	queryA        incremental.QueryKey
	queryB        incremental.QueryKey
	bodyA         atomic.Value
	bodyB         atomic.Value
	requestsA     atomic.Int32
	requestsB     atomic.Int32
	triggers      <-chan busevents.Event
}

func newIncrementalHTTPTestFixture(t *testing.T) *incrementalHTTPTestFixture {
	t.Helper()
	return newIncrementalHTTPTestFixtureWithTemplate(t, `{{ item | dig_string("", "metadata", "name") }}={{ http.Fetch(item | dig_string("", "spec", "url"), map[string]any{"critical": true}) }}
`)
}

func newIncrementalHTTPTestFixtureWithTemplate(t *testing.T, template string) *incrementalHTTPTestFixture {
	t.Helper()
	fixture := &incrementalHTTPTestFixture{}
	fixture.bodyA.Store("first")
	fixture.bodyB.Store("stable")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/a":
			fixture.requestsA.Add(1)
			_, _ = w.Write([]byte(fixture.bodyA.Load().(string)))
		case "/b":
			fixture.requestsB.Add(1)
			_, _ = w.Write([]byte(fixture.bodyB.Load().(string)))
		default:
			http.NotFound(w, request)
		}
	}))
	t.Cleanup(server.Close)
	fixture.urlA = server.URL + "/a"
	fixture.urlB = server.URL + "/b"

	cfg := &config.Config{
		Dataplane: testDataplaneConfig(),
		WatchedResources: map[string]config.WatchedResource{
			"routes": {
				APIVersion: "example.test/v1",
				Resources:  "routes",
				IndexBy:    []string{"metadata.namespace", "metadata.name"},
			},
		},
		TemplateSnippets: map[string]config.TemplateSnippet{
			"routes": {
				Name:     "routes",
				Requires: []string{"routes"},
				Incremental: &config.IncrementalTemplate{
					Source: "routes",
				},
				Template: template,
			},
		},
		HAProxyConfig: config.HAProxyConfig{Template: `{{ render "routes" }}`},
	}
	declarations := helpers.BuildAdditionalDeclarations(cfg, &typebootstrap.Result{
		Types:  map[string]reflect.Type{},
		Kinds:  map[string]string{},
		Errors: map[string]error{},
	})
	engine, err := helpers.NewEngineFromConfigWithOptions(cfg, nil, nil, declarations, helpers.EngineOptions{})
	require.NoError(t, err)
	bus, logger := testutil.NewTestBusAndLogger()
	fixture.triggers = bus.Subscribe("incremental-http-fixture", 100)
	fixture.httpComponent = controllerhttpstore.New(bus, logger, -time.Hour)
	bus.Start()
	fixture.service = NewRenderService(&RenderServiceConfig{
		Engine:             engine,
		Config:             cfg,
		Logger:             logger,
		HTTPStoreComponent: fixture.httpComponent,
	})
	routes := k8sstore.NewMemoryStore(2)
	require.NoError(t, routes.Add(
		incrementalTestResource("default", "a", map[string]any{"url": fixture.urlA}),
		[]string{"default", "a"},
	))
	tempComponent73 := fixture.service.incremental.components["routes"]
	tempComponent74 := fixture.service.incremental.components["routes"]
	require.NoError(t, routes.Add(
		incrementalTestResource("default", "b", map[string]any{"url": fixture.urlB}),
		[]string{"default", "b"},
	))
	fixture.provider = stores.NewRealStoreProvider(map[string]stores.Store{"routes": routes})
	fixture.queryA = componentQueryKey(&tempComponent73, "routes", "default", "a")
	fixture.queryB = componentQueryKey(&tempComponent74, "routes", "default", "b")
	return fixture
}

func (f *incrementalHTTPTestFixture) render(t *testing.T) string {
	t.Helper()
	return renderAndCommitIncrementalCacheReady(t, f.service, f.provider)
}

func (f *incrementalHTTPTestFixture) assertState(
	t *testing.T,
	output string,
	executionsA, executionsB uint64,
	requestsA, requestsB int32,
) {
	t.Helper()
	assert.Equal(t, output, f.render(t))
	assert.Equal(t, executionsA, f.service.incremental.graph.Counters(f.queryA).Executions)
	assert.Equal(t, executionsB, f.service.incremental.graph.Counters(f.queryB).Executions)
	assert.Equal(t, requestsA, f.requestsA.Load())
	assert.Equal(t, requestsB, f.requestsB.Load())
}

func TestRenderServiceIncrementalTracksActiveHTTPInputsWithoutReplay(t *testing.T) {
	fixture := newIncrementalHTTPTestFixture(t)

	fixture.assertState(t, "a=first\nb=stable\n", 0, 0, 1, 1)
	fixture.assertState(t, "a=first\nb=stable\n", 1, 1, 1, 1)
	fixture.assertState(t, "a=first\nb=stable\n", 1, 1, 1, 1)

	fixture.bodyB.Store("pending")
	pendingB, err := fixture.httpComponent.GetStore().RefreshURLVersion(t.Context(), fixture.urlB)
	require.NoError(t, err)
	require.NotNil(t, pendingB)
	require.Empty(t, fixture.httpComponent.GetStore().EvictUnused())
	fixture.assertState(t, "a=first\nb=stable\n", 1, 1, 1, 2)
	descriptor, err := purehttpstore.DescribeSource(purehttpstore.FetchOptions{Critical: true}, nil)
	require.NoError(t, err)
	assert.Equal(t, "first", fixture.httpComponent.GetStore().AcceptedSnapshot(fixture.urlA, descriptor).Content)
	fixture.assertState(t, "a=first\nb=stable\n", 1, 1, 1, 2)
	require.True(t, fixture.httpComponent.GetStore().RejectPendingVersion(
		fixture.urlB,
		pendingB.Checksum,
		pendingB.Revision,
	))

	fixture.bodyA.Store("second")
	version, err := fixture.httpComponent.GetStore().RefreshURLVersion(t.Context(), fixture.urlA)
	require.NoError(t, err)
	require.NotNil(t, version)
	require.True(t, fixture.httpComponent.GetStore().PromotePendingVersion(
		fixture.urlA,
		version.Checksum,
		version.Revision,
	))

	fixture.assertState(t, "a=second\nb=stable\n", 2, 1, 2, 2)
	fixture.assertState(t, "a=second\nb=stable\n", 2, 1, 2, 2)
}

func TestRenderServiceRetiresIncrementalHTTPLeaseSet(t *testing.T) {
	fixture := newIncrementalHTTPTestFixture(t)
	fixture.assertState(t, "a=first\nb=stable\n", 0, 0, 1, 1)
	fixture.assertState(t, "a=first\nb=stable\n", 1, 1, 1, 1)
	require.Empty(t, fixture.httpComponent.GetStore().EvictUnused())

	require.NoError(t, fixture.service.RetireIncrementalCache())
	require.NoError(t, fixture.service.RetireIncrementalCache())
	assert.ElementsMatch(t, []string{fixture.urlA, fixture.urlB}, fixture.httpComponent.GetStore().EvictUnused())

	_, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	assert.ErrorContains(t, err, "incremental render cache was retired")
}

// A watched input moving after the render no longer refuses its new content:
// acceptance is checked against the render's own snapshot (ADR-0030).
func TestRenderServiceResourceChangeAfterTheRenderStillAcceptsInitialHTTPCandidate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("candidate"))
	}))
	t.Cleanup(server.Close)

	cfg := &config.Config{
		Dataplane: testDataplaneConfig(),
		WatchedResources: map[string]config.WatchedResource{
			"routes": {
				APIVersion: "example.test/v1",
				Resources:  "routes",
				IndexBy:    []string{"metadata.namespace", "metadata.name"},
			},
		},
		TemplateSnippets: map[string]config.TemplateSnippet{
			"routes": {
				Name:        "routes",
				Requires:    []string{"routes"},
				Incremental: &config.IncrementalTemplate{Source: "routes"},
				Template: `{{ item | dig_string("", "metadata", "name") }}={{ http.Fetch(` + strconv.Quote(server.URL) + `, map[string]any{"critical": true}) }}
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
	httpComponent := controllerhttpstore.New(bus, logger, 0)
	service := NewRenderService(&RenderServiceConfig{
		Engine: engine, Config: cfg, Logger: logger, HTTPStoreComponent: httpComponent,
	})
	routes := k8sstore.NewMemoryStore(2)
	require.NoError(t, routes.Add(
		incrementalTestResource("default", "a", map[string]any{"value": "before"}),
		[]string{"default", "a"},
	))
	provider := stores.NewRealStoreProvider(map[string]stores.Store{"routes": routes})

	result, err := service.Render(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NotNil(t, result.InputTransaction)
	require.NoError(t, routes.Update(
		incrementalTestResource("default", "a", map[string]any{"value": "after"}),
		[]string{"default", "a"},
	))
	require.NoError(t, result.InputTransaction.Commit(t.Context()))
	descriptor, err := purehttpstore.DescribeSource(purehttpstore.FetchOptions{Critical: true}, nil)
	require.NoError(t, err)
	assert.True(t, httpComponent.GetStore().AcceptedSnapshot(server.URL, descriptor).Found)
	require.NoError(t, service.RetireIncrementalCache())
}

func TestRenderServiceHTTPCommitFailureDoesNotPublishIncrementalState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("candidate"))
	}))
	t.Cleanup(server.Close)

	cfg := &config.Config{
		Dataplane: testDataplaneConfig(),
		WatchedResources: map[string]config.WatchedResource{
			"routes": {
				APIVersion: "example.test/v1",
				Resources:  "routes",
				IndexBy:    []string{"metadata.namespace", "metadata.name"},
			},
		},
		TemplateSnippets: map[string]config.TemplateSnippet{
			"routes": {
				Name:        "routes",
				Requires:    []string{"routes"},
				Incremental: &config.IncrementalTemplate{Source: "routes"},
				Template: `{{ item | dig_string("", "metadata", "name") }}={{ http.Fetch(` + strconv.Quote(server.URL) + `, map[string]any{"critical": true}) }}
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
	httpComponent := controllerhttpstore.New(bus, logger, 0)
	service := NewRenderService(&RenderServiceConfig{
		Engine: engine, Config: cfg, Logger: logger, HTTPStoreComponent: httpComponent,
	})
	routes := k8sstore.NewMemoryStore(2)
	require.NoError(t, routes.Add(
		incrementalTestResource("default", "a", map[string]any{"value": "before"}),
		[]string{"default", "a"},
	))
	provider := stores.NewRealStoreProvider(map[string]stores.Store{"routes": routes})
	base := service.incremental.snapshot
	tempComponent75 := service.incremental.components["routes"]
	query := componentQueryKey(&tempComponent75, "routes", "default", "a")

	result, err := service.Render(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NotNil(t, result.InputTransaction)
	_, err = httpComponent.ReconcileSource(
		server.URL,
		purehttpstore.FetchOptions{Critical: true, Retries: 1},
		nil,
	)
	require.NoError(t, err)
	require.Error(t, result.InputTransaction.Commit(t.Context()))
	assert.Same(t, base, service.incremental.snapshot)
	assert.Zero(t, service.incremental.graph.Generation())
	assert.Zero(t, service.incremental.graph.Counters(query).Executions)

	assert.Equal(t, "a=candidate\n", renderAndCommitIncrementalCacheReady(t, service, provider))
	assert.Equal(t, "a=candidate\n", renderAndCommitIncrementalCacheReady(t, service, provider))
	assert.Equal(t, uint64(1), service.incremental.graph.Generation())
	assert.Equal(t, uint64(1), service.incremental.graph.Counters(query).Executions)
}

func renderAndCommitIncrementalCacheReady(
	t *testing.T,
	service *RenderService,
	provider stores.StoreProvider,
) string {
	t.Helper()
	result, err := service.Render(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NotNil(t, result.InputTransaction)
	require.NoError(t, result.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, service)
	return result.HAProxyConfig
}

func waitForIncrementalCache(tb testing.TB, service *RenderService) {
	tb.Helper()
	if service == nil || service.incremental == nil {
		return
	}
	service.incremental.mu.Lock()
	readinessErr := service.incremental.validateIncrementalCacheReadinessLocked()
	pending := service.incremental.cachePending
	ready := service.incremental.cacheReadySignal
	service.incremental.mu.Unlock()
	require.NoError(tb, readinessErr)
	if !pending {
		return
	}
	require.NotNil(tb, ready)
	select {
	case <-ready.done:
	case <-time.After(5 * time.Second):
		tb.Fatal("timed out waiting for incremental cache readiness")
	}
	require.NoError(tb, ready.result())
	service.incremental.mu.Lock()
	readinessErr = service.incremental.validateIncrementalCacheReadinessLocked()
	assert.False(tb, service.incremental.cachePending)
	assert.Zero(tb, service.incremental.cachePendingGeneration)
	assert.Nil(tb, service.incremental.cacheReadySignal)
	service.incremental.mu.Unlock()
	require.NoError(tb, readinessErr)
}

func incrementalTestResource(namespace, name string, spec map[string]any) map[string]any {
	return map[string]any{
		"apiVersion": "example.test/v1",
		"kind":       "Example",
		"metadata": map[string]any{
			"namespace": namespace,
			"name":      name,
		},
		"spec": spec,
	}
}
