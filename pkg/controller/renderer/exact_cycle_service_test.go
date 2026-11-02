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
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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

type exactCycleServiceFixture struct {
	service  *RenderService
	provider stores.StoreProvider
	routes   *k8sstore.MemoryStore
	unused   *k8sstore.MemoryStore
}

type exactCycleSharedFixture struct {
	service  *RenderService
	provider stores.StoreProvider
	winner   *k8sstore.MemoryStore
	loser    *k8sstore.MemoryStore
	unused   *k8sstore.MemoryStore
}

func newExactCycleSharedFixture(t *testing.T) *exactCycleSharedFixture {
	t.Helper()
	cfg := &config.Config{
		Dataplane: testDataplaneConfig(),
		WatchedResources: map[string]config.WatchedResource{
			"bootstrap": {
				APIVersion: "example.test/v1", Resources: "bootstrap",
				IndexBy: []string{"metadata.namespace", "metadata.name"},
			},
			"winner": {
				APIVersion: "example.test/v1", Resources: "winners",
				IndexBy: []string{"metadata.namespace", "metadata.name"},
			},
			"loser": {
				APIVersion: "example.test/v1", Resources: "losers",
				IndexBy: []string{"metadata.namespace", "metadata.name"},
			},
			"unused": {
				APIVersion: "example.test/v1", Resources: "unused",
				IndexBy: []string{"metadata.namespace", "metadata.name"},
			},
		},
		TemplateSnippets: map[string]config.TemplateSnippet{
			"bootstrap": {
				Name: "bootstrap", Requires: []string{"bootstrap"},
				Incremental: &config.IncrementalTemplate{Source: "bootstrap"},
				Template:    ``,
			},
		},
		HAProxyConfig: config.HAProxyConfig{Template: `{%
			var selected, _ = shared.ComputeIfAbsent("selection", func() any {
				return resources.winner.GetSingle("default", "winner") | dig_string("", "spec", "value")
			})
		%}{{ tostring(selected) }}{{ render "bootstrap" }}`},
		Maps: map[string]config.MapFile{"losing.map": {Template: `{%
			var _, _ = shared.ComputeIfAbsent("selection", func() any {
				return resources.loser.GetSingle("default", "loser") | dig_string("", "spec", "value")
			})
		%}`}},
	}
	declarations := helpers.BuildAdditionalDeclarations(cfg, &typebootstrap.Result{
		Types: map[string]reflect.Type{}, Kinds: map[string]string{}, Errors: map[string]error{},
	})
	engine, err := helpers.NewEngineFromConfigWithOptions(
		cfg, nil, nil, declarations, helpers.EngineOptions{},
	)
	require.NoError(t, err)
	service := NewRenderService(&RenderServiceConfig{Engine: engine, Config: cfg, Logger: slog.Default()})
	require.NotNil(t, service.exactCycleProgram)
	requiresRoots, err := service.exactCycleProgram.RequiresUnchangedInputRoots()
	require.NoError(t, err)
	require.False(t, requiresRoots)
	winner := k8sstore.NewMemoryStore(2)
	loser := k8sstore.NewMemoryStore(2)
	unused := k8sstore.NewMemoryStore(2)
	bootstrap := k8sstore.NewMemoryStore(2)
	require.NoError(t, winner.Add(
		incrementalTestResource("default", "winner", map[string]any{"value": "winner-v1"}),
		[]string{"default", "winner"},
	))
	require.NoError(t, winner.Add(
		incrementalTestResource("default", "noise", map[string]any{"value": 0}),
		[]string{"default", "noise"},
	))
	require.NoError(t, loser.Add(
		incrementalTestResource("default", "loser", map[string]any{"value": "loser-v1"}),
		[]string{"default", "loser"},
	))
	require.NoError(t, unused.Add(
		incrementalTestResource("default", "unused", map[string]any{"value": "unused-v1"}),
		[]string{"default", "unused"},
	))
	provider := stores.NewRealStoreProvider(map[string]stores.Store{
		"winner": winner, "loser": loser, "unused": unused, "bootstrap": bootstrap,
	})
	return &exactCycleSharedFixture{
		service: service, provider: provider, winner: winner, loser: loser, unused: unused,
	}
}

type exactCycleDirectHTTPFixture struct {
	service       *RenderService
	provider      stores.StoreProvider
	httpComponent *controllerhttpstore.Component
	routes        *k8sstore.MemoryStore
	unused        *k8sstore.MemoryStore
	url           string
	body          atomic.Value
	requests      atomic.Uint64
}

func newExactCycleDirectHTTPFixture(t *testing.T) *exactCycleDirectHTTPFixture {
	t.Helper()
	fixture := &exactCycleDirectHTTPFixture{}
	fixture.body.Store("accepted")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fixture.requests.Add(1)
		_, _ = w.Write([]byte(fixture.body.Load().(string)))
	}))
	t.Cleanup(server.Close)
	fixture.url = server.URL
	cfg := &config.Config{
		Dataplane: testDataplaneConfig(),
		WatchedResources: map[string]config.WatchedResource{
			"routes": {
				APIVersion: "example.test/v1", Resources: "routes",
				IndexBy: []string{"metadata.namespace", "metadata.name"},
			},
			"unused": {
				APIVersion: "example.test/v1", Resources: "unused",
				IndexBy: []string{"metadata.namespace", "metadata.name"},
			},
		},
		TemplateSnippets: map[string]config.TemplateSnippet{
			"routes": {
				Name: "routes", Requires: []string{"routes"},
				Incremental: &config.IncrementalTemplate{Source: "routes"},
				Template:    `{{ item | dig_string("", "metadata", "name") }}\n`,
			},
		},
		HAProxyConfig: config.HAProxyConfig{Template: fmt.Sprintf(
			`{{ http.Fetch(%q, map[string]any{"critical": true}) }}\n{{ render "routes" }}`,
			server.URL,
		)},
	}
	declarations := helpers.BuildAdditionalDeclarations(cfg, &typebootstrap.Result{
		Types: map[string]reflect.Type{}, Kinds: map[string]string{}, Errors: map[string]error{},
	})
	engine, err := helpers.NewEngineFromConfigWithOptions(cfg, nil, nil, declarations, helpers.EngineOptions{})
	require.NoError(t, err)
	bus, logger := testutil.NewTestBusAndLogger()
	fixture.httpComponent = controllerhttpstore.New(bus, logger, -time.Hour)
	fixture.service = NewRenderService(&RenderServiceConfig{
		Engine: engine, Config: cfg, Logger: logger, HTTPStoreComponent: fixture.httpComponent,
	})
	require.NotNil(t, fixture.service.exactCycleProgram)
	fixture.routes = k8sstore.NewMemoryStore(2)
	require.NoError(t, fixture.routes.Add(
		incrementalTestResource("default", "route", nil), []string{"default", "route"},
	))
	fixture.unused = k8sstore.NewMemoryStore(2)
	require.NoError(t, fixture.unused.Add(
		incrementalTestResource("default", "unused", nil), []string{"default", "unused"},
	))
	fixture.provider = stores.NewRealStoreProvider(map[string]stores.Store{
		"routes": fixture.routes, "unused": fixture.unused,
	})
	return fixture
}

func newExactCycleServiceFixture(t *testing.T) *exactCycleServiceFixture {
	t.Helper()
	cfg := &config.Config{
		Dataplane: testDataplaneConfig(),
		WatchedResources: map[string]config.WatchedResource{
			"routes": {
				APIVersion: "example.test/v1",
				Resources:  "routes",
				IndexBy:    []string{"metadata.namespace", "metadata.name"},
			},
			"unused": {
				APIVersion: "example.test/v1",
				Resources:  "unused",
				IndexBy:    []string{"metadata.namespace", "metadata.name"},
			},
		},
		TemplateSnippets: map[string]config.TemplateSnippet{
			"routes": {
				Name:        "routes",
				Requires:    []string{"routes"},
				Incremental: &config.IncrementalTemplate{Source: "routes"},
				Template:    `{{ item | dig_string("", "metadata", "name") }}\n`,
			},
		},
		HAProxyConfig: config.HAProxyConfig{Template: `{{ render "routes" }}`},
	}
	declarations := helpers.BuildAdditionalDeclarations(cfg, &typebootstrap.Result{
		Types: map[string]reflect.Type{}, Kinds: map[string]string{}, Errors: map[string]error{},
	})
	engine, err := helpers.NewEngineFromConfigWithOptions(
		cfg, nil, nil, declarations, helpers.EngineOptions{},
	)
	require.NoError(t, err)
	service := NewRenderService(&RenderServiceConfig{Engine: engine, Config: cfg, Logger: slog.Default()})
	require.NotNil(t, service.exactCycleProgram)
	routes := k8sstore.NewMemoryStore(2)
	unused := k8sstore.NewMemoryStore(2)
	require.NoError(t, routes.Add(incrementalTestResource("default", "route", nil), []string{"default", "route"}))
	require.NoError(t, unused.Add(incrementalTestResource("default", "unused", nil), []string{"default", "unused"}))
	provider := stores.NewRealStoreProvider(map[string]stores.Store{"routes": routes, "unused": unused})
	return &exactCycleServiceFixture{service: service, provider: provider, routes: routes, unused: unused}
}

func TestRenderServiceExactCycleReusesOnlyCommittedCandidate(t *testing.T) {
	fixture := newExactCycleServiceFixture(t)

	first, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NotNil(t, first.Plan)
	require.Nil(t, fixture.service.exactCycleCandidate)
	first.InputTransaction.Abort()
	require.Nil(t, fixture.service.exactCycleCandidate)

	committed, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, committed.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, fixture.service)
	require.NotNil(t, fixture.service.exactCycleCandidate)

	reused, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.Nil(t, reused.Plan)
	require.Same(t, committed.CycleSnapshot, reused.CycleSnapshot)
	require.NoError(t, reused.InputTransaction.Commit(t.Context()))
}

func TestRenderServiceExactCycleRebasesRepeatedUnrelatedStoreChanges(t *testing.T) {
	fixture := newExactCycleServiceFixture(t)
	first, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, first.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, fixture.service)
	routesComponent := fixture.service.incremental.components["routes"]
	query := componentQueryKey(&routesComponent, "routes", "default", "route")
	require.Equal(t, uint64(1), fixture.service.incremental.graph.Counters(query).Executions)

	for revision := 1; revision <= 12; revision++ {
		require.NoError(t, fixture.unused.Update(
			incrementalTestResource("default", "unused", map[string]any{"revision": revision}),
			[]string{"default", "unused"},
		))
		result, renderErr := fixture.service.Render(
			t.Context(), fixture.provider, rendercontext.RenderModeReconcile,
		)
		require.NoError(t, renderErr)
		require.Nil(t, result.Plan)
		require.Same(t, first.CycleSnapshot, result.CycleSnapshot)
		require.NoError(t, result.InputTransaction.Commit(t.Context()))
		require.Equal(t, uint64(1), fixture.service.incremental.graph.Counters(query).Executions)
	}

	require.NoError(t, fixture.routes.Update(
		incrementalTestResource("default", "changed", nil),
		[]string{"default", "changed"},
	))
	changed, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NotSame(t, first.CycleSnapshot, changed.CycleSnapshot)
	require.NoError(t, changed.InputTransaction.Commit(t.Context()))
}

func TestRenderServiceExactCycleSharedLoserAndUnrelatedChangesReuse(t *testing.T) {
	fixture := newExactCycleSharedFixture(t)
	first, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.Contains(t, first.HAProxyConfig, "winner-v1")
	require.NoError(t, first.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, fixture.service)
	require.NotNil(t, fixture.service.exactCycleCandidate)

	require.NoError(t, fixture.unused.Update(
		incrementalTestResource("default", "unused", map[string]any{"value": "unused-v2"}),
		[]string{"default", "unused"},
	))
	unrelated, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.Nil(t, unrelated.Plan)
	require.Same(t, first.CycleSnapshot, unrelated.CycleSnapshot)
	require.NoError(t, unrelated.InputTransaction.Commit(t.Context()))

	require.NoError(t, fixture.loser.Update(
		incrementalTestResource("default", "loser", map[string]any{"value": "loser-v2"}),
		[]string{"default", "loser"},
	))
	loser, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.Nil(t, loser.Plan)
	require.Same(t, first.CycleSnapshot, loser.CycleSnapshot)
	require.NoError(t, loser.InputTransaction.Commit(t.Context()))

	require.NoError(t, fixture.winner.Update(
		incrementalTestResource("default", "winner", map[string]any{"value": "winner-v2"}),
		[]string{"default", "winner"},
	))
	changed, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NotSame(t, first.CycleSnapshot, changed.CycleSnapshot)
	require.Contains(t, changed.HAProxyConfig, "winner-v2")
	require.NoError(t, changed.InputTransaction.Commit(t.Context()))

	reused, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.Nil(t, reused.Plan)
	require.Same(t, changed.CycleSnapshot, reused.CycleSnapshot)
	require.NoError(t, reused.InputTransaction.Commit(t.Context()))
}

func TestRenderServiceExactCycleNonemptyOverlayCannotPublishOrReuse(t *testing.T) {
	fixture := newExactCycleServiceFixture(t)
	committed, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, committed.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, fixture.service)
	require.NotNil(t, fixture.service.exactCycleCandidate)

	overlay := stores.NewStoreOverlayForUpdate(&unstructured.Unstructured{Object: incrementalTestResource(
		"default", "route", map[string]any{"value": "proposed"},
	)})
	provider := stores.NewOverlayStoreProvider(
		fixture.provider,
		stores.NewValidationContext(map[string]*stores.StoreOverlay{"routes": overlay}),
	)

	firstOverlay, err := fixture.service.Render(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, firstOverlay.InputTransaction.Commit(t.Context()))
	require.Nil(t, fixture.service.exactCycleCandidate)

	secondOverlay, err := fixture.service.Render(t.Context(), provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, secondOverlay.InputTransaction.Commit(t.Context()))
	require.Nil(t, fixture.service.exactCycleCandidate)
}

func TestRenderServiceExactCycleRebaseStaysWithinJournalHorizon(t *testing.T) {
	fixture := newExactCycleSharedFixture(t)
	first, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, first.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, fixture.service)
	original, found := fixture.service.exactCycleCandidate.resources.scopes.Root().Get([]byte("winner"))
	require.True(t, found)

	const mutationsPerReuse = 1025
	for cycle := 1; cycle <= 5; cycle++ {
		for mutation := 1; mutation <= mutationsPerReuse; mutation++ {
			revision := cycle*mutationsPerReuse + mutation
			require.NoError(t, fixture.winner.Update(
				incrementalTestResource("default", "noise", map[string]any{"value": revision}),
				[]string{"default", "noise"},
			))
		}
		result, renderErr := fixture.service.Render(
			t.Context(), fixture.provider, rendercontext.RenderModeReconcile,
		)
		require.NoError(t, renderErr)
		require.Nil(t, result.Plan)
		require.Same(t, first.CycleSnapshot, result.CycleSnapshot)
		require.NoError(t, result.InputTransaction.Commit(t.Context()))

		current, pinErr := fixture.winner.Pin()
		require.NoError(t, pinErr)
		rebased, exists := fixture.service.exactCycleCandidate.resources.scopes.Root().Get([]byte("winner"))
		require.True(t, exists)
		require.Equal(t, current.RevisionSource(), rebased.source)
		require.Equal(t, current.Sequence(), rebased.sequence)
	}
	latest, found := fixture.service.exactCycleCandidate.resources.scopes.Root().Get([]byte("winner"))
	require.True(t, found)
	require.NotEqual(t, original.sequence, latest.sequence)
	require.False(t, original.root == latest.root)

	require.NoError(t, fixture.winner.Update(
		incrementalTestResource("default", "winner", map[string]any{"value": "winner-v2"}),
		[]string{"default", "winner"},
	))
	changed, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NotSame(t, first.CycleSnapshot, changed.CycleSnapshot)
	require.Contains(t, changed.HAProxyConfig, "winner-v2")
	require.NoError(t, changed.InputTransaction.Commit(t.Context()))
}

// A reused candidate is published at the roots the render pinned, so a change
// that lands mid-commit sits in the journal after them and the next render
// replays only once the journal proves nothing it read moved.
func TestRenderServiceExactCycleLateResourceCommitPublishesAtPinnedRoots(t *testing.T) {
	fixture := newExactCycleServiceFixture(t)
	first, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, first.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, fixture.service)

	committedCandidate := fixture.service.exactCycleCandidate
	committedCache := fixture.service.lastRenderCache
	aborted, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	aborted.InputTransaction.Abort()
	require.Same(t, committedCandidate, fixture.service.exactCycleCandidate)
	require.Same(t, committedCache, fixture.service.lastRenderCache)

	unrelated, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	pinnedUnused, err := fixture.unused.Pin()
	require.NoError(t, err)
	require.NoError(t, fixture.unused.Update(
		incrementalTestResource("default", "unused", map[string]any{"revision": 1}),
		[]string{"default", "unused"},
	))
	require.NoError(t, unrelated.InputTransaction.Commit(t.Context()))
	require.Same(t, first.CycleSnapshot, unrelated.CycleSnapshot)
	require.NotSame(t, committedCandidate, fixture.service.exactCycleCandidate)
	unusedRoot, found := fixture.service.exactCycleCandidate.storeRoots.roots.Root().Get([]byte("unused"))
	require.True(t, found)
	require.Equal(t, pinnedUnused.Sequence(), unusedRoot.sequence)
	afterUnrelated, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.Same(t, first.CycleSnapshot, afterUnrelated.CycleSnapshot)
	require.NoError(t, afterUnrelated.InputTransaction.Commit(t.Context()))

	aba, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, fixture.routes.Update(
		incrementalTestResource("default", "route", map[string]any{"revision": "away"}),
		[]string{"default", "route"},
	))
	require.NoError(t, fixture.routes.Update(
		incrementalTestResource("default", "route", nil),
		[]string{"default", "route"},
	))
	require.NoError(t, aba.InputTransaction.Commit(t.Context()))
	require.Same(t, first.CycleSnapshot, aba.CycleSnapshot)
	afterABA, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.Same(t, first.CycleSnapshot, afterABA.CycleSnapshot)
	require.NoError(t, afterABA.InputTransaction.Commit(t.Context()))

	beforeRelevant := fixture.service.exactCycleCandidate
	relevant, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, fixture.routes.Add(
		incrementalTestResource("default", "added", nil),
		[]string{"default", "added"},
	))
	require.NoError(t, relevant.InputTransaction.Commit(t.Context()))
	require.Same(t, first.CycleSnapshot, relevant.CycleSnapshot)
	require.NotSame(t, beforeRelevant, fixture.service.exactCycleCandidate)
	rendered, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NotSame(t, first.CycleSnapshot, rendered.CycleSnapshot)
	require.Contains(t, rendered.HAProxyConfig, "added")
	require.NoError(t, rendered.InputTransaction.Commit(t.Context()))
}

func seedExactCycleForceColdGraphCandidate(
	t *testing.T,
	fixture *incrementalHTTPTestFixture,
	routes *k8sstore.MemoryStore,
) {
	t.Helper()
	require.NoError(t, routes.Delete("default", "b", []string{"default", "b"}))

	first, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, first.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, fixture.service)
	seeded, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, seeded.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, fixture.service)
	require.NotNil(t, fixture.service.exactCycleCandidate)
	require.Equal(t, exactCycleCandidateGraph, fixture.service.exactCycleCandidate.mode)
}

// invalidateExactCycleCandidate makes the next render discard the published
// candidate, which retries it as a full cold render.
func invalidateExactCycleCandidate(t *testing.T, service *RenderService) {
	t.Helper()
	require.NotNil(t, service.exactCycleCandidate)
	service.exactCycleCandidate.mode = 0
}

func TestRenderServiceExactCycleForceColdPublishesOutputOnlySuccessor(t *testing.T) {
	fixture := newIncrementalHTTPTestFixture(t)
	routes, ok := fixture.provider.GetStore("routes").(*k8sstore.MemoryStore)
	require.True(t, ok)
	seedExactCycleForceColdGraphCandidate(t, fixture, routes)

	require.NoError(t, routes.Add(
		incrementalTestResource("default", "b", map[string]any{"url": fixture.urlB}),
		[]string{"default", "b"},
	))
	invalidateExactCycleCandidate(t, fixture.service)
	forced, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.Contains(t, forced.HAProxyConfig, "b=stable")
	require.NoError(t, forced.InputTransaction.Commit(t.Context()))
	require.NotNil(t, fixture.service.exactCycleCandidate)
	require.Equal(t, exactCycleCandidateOutputOnly, fixture.service.exactCycleCandidate.mode)
	require.Equal(t, uint64(0), fixture.service.incremental.graph.Counters(fixture.queryB).Executions)
	require.True(t, fixture.httpComponent.GetStore().HasActiveLease(fixture.urlB))

	reused, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.Nil(t, reused.Plan)
	require.Same(t, forced.CycleSnapshot, reused.CycleSnapshot)
	require.NoError(t, reused.InputTransaction.Commit(t.Context()))
	require.Equal(t, exactCycleCandidateOutputOnly, fixture.service.exactCycleCandidate.mode)
	require.Equal(t, uint64(0), fixture.service.incremental.graph.Counters(fixture.queryB).Executions)
	require.True(t, fixture.httpComponent.GetStore().HasActiveLease(fixture.urlB))

	requireOutputOnlyCandidateSurvivesLateMutation(t, fixture, routes, forced, "b=stable")

	fixture.bodyB.Store("changed")
	promoteIncrementalHTTPBody(t, fixture.httpComponent, fixture.urlB)
	changed, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.Contains(t, changed.HAProxyConfig, "b=changed")
	require.NotSame(t, forced.CycleSnapshot, changed.CycleSnapshot)
	require.NoError(t, changed.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, fixture.service)
	require.Equal(t, exactCycleCandidateGraph, fixture.service.exactCycleCandidate.mode)

	final, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.Nil(t, final.Plan)
	require.Same(t, changed.CycleSnapshot, final.CycleSnapshot)
	require.NoError(t, final.InputTransaction.Commit(t.Context()))

	require.NoError(t, fixture.service.RetireIncrementalCache())
	require.False(t, fixture.httpComponent.GetStore().HasActiveLease(fixture.urlA))
	require.False(t, fixture.httpComponent.GetStore().HasActiveLease(fixture.urlB))
}

// An output-only candidate replays only under unchanged roots: a route moving
// under its reuse commit still publishes a successor, and the next render goes
// through the graph.
func requireOutputOnlyCandidateSurvivesLateMutation(
	t *testing.T,
	fixture *incrementalHTTPTestFixture,
	routes *k8sstore.MemoryStore,
	changed *RenderResult,
	wantB string,
) {
	t.Helper()
	outputOnly := fixture.service.exactCycleCandidate
	lateMutation, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, routes.Update(
		incrementalTestResource("default", "a", map[string]any{"url": fixture.urlA, "noise": "changed"}),
		[]string{"default", "a"},
	))
	require.NoError(t, lateMutation.InputTransaction.Commit(t.Context()))
	require.Same(t, changed.CycleSnapshot, lateMutation.CycleSnapshot)
	require.NotSame(t, outputOnly, fixture.service.exactCycleCandidate)
	require.Equal(t, exactCycleCandidateOutputOnly, fixture.service.exactCycleCandidate.mode)
	rerendered, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NotSame(t, changed.CycleSnapshot, rerendered.CycleSnapshot)
	require.Contains(t, rerendered.HAProxyConfig, wantB)
	require.NoError(t, rerendered.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, fixture.service)
	require.Equal(t, exactCycleCandidateGraph, fixture.service.exactCycleCandidate.mode)
}

// A cold render must not make the next changed render cold too: under constant
// churn that chain never ends, and every reconcile, endpoint changes included,
// pays a full cold render.
func TestRenderServiceExactCycleOutputOnlyMismatchRendersThroughGraph(t *testing.T) {
	fixture := newIncrementalHTTPTestFixture(t)
	routes, ok := fixture.provider.GetStore("routes").(*k8sstore.MemoryStore)
	require.True(t, ok)
	seedExactCycleForceColdGraphCandidate(t, fixture, routes)

	require.NoError(t, routes.Add(
		incrementalTestResource("default", "b", map[string]any{"url": fixture.urlB}),
		[]string{"default", "b"},
	))
	invalidateExactCycleCandidate(t, fixture.service)
	forced, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, forced.InputTransaction.Commit(t.Context()))
	require.Equal(t, exactCycleCandidateOutputOnly, fixture.service.exactCycleCandidate.mode)

	for round, noise := range []string{"first", "second"} {
		executionsBefore := fixture.service.incremental.graph.Counters(fixture.queryA).Executions
		require.NoError(t, routes.Update(
			incrementalTestResource("default", "a", map[string]any{"url": fixture.urlA, "noise": noise}),
			[]string{"default", "a"},
		))
		changed, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
		require.NoError(t, err)
		require.Contains(t, changed.HAProxyConfig, "a=first")
		require.Contains(t, changed.HAProxyConfig, "b=stable")
		require.NoError(t, changed.InputTransaction.Commit(t.Context()))
		waitForIncrementalCache(t, fixture.service)
		require.Greater(t, fixture.service.incremental.graph.Counters(fixture.queryA).Executions, executionsBefore,
			"changed render %d after a cold render skipped the incremental graph", round)
	}
	require.NoError(t, fixture.service.RetireIncrementalCache())
}

func TestRenderServiceExactCycleDiscardsTamperedCandidateAndPublishesSuccessor(t *testing.T) {
	fixture := newExactCycleServiceFixture(t)
	first, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, first.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, fixture.service)
	poisoned := fixture.service.exactCycleCandidate
	require.NotNil(t, poisoned)
	poisoned.mode = 0

	recovered, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.Nil(t, fixture.service.exactCycleCandidate)
	require.NoError(t, recovered.InputTransaction.Commit(t.Context()))
	require.NotNil(t, fixture.service.exactCycleCandidate)
	require.NotSame(t, poisoned, fixture.service.exactCycleCandidate)
	require.Equal(t, exactCycleCandidateOutputOnly, fixture.service.exactCycleCandidate.mode)
	require.NoError(t, fixture.service.exactCycleCandidate.validate())

	reused, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.Nil(t, reused.Plan)
	require.Same(t, recovered.CycleSnapshot, reused.CycleSnapshot)
	require.NoError(t, reused.InputTransaction.Commit(t.Context()))
}

func TestRenderServiceExactCycleCapturesFirstAcceptedHTTPAtCommit(t *testing.T) {
	fixture := newExactCycleDirectHTTPFixture(t)
	first, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.True(t, first.InputTransaction.HasCandidates())
	require.Nil(t, fixture.service.exactCycleCandidate)
	require.NoError(t, first.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, fixture.service)
	require.NotNil(t, fixture.service.exactCycleCandidate)

	reused, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.Nil(t, reused.Plan)
	require.Same(t, first.CycleSnapshot, reused.CycleSnapshot)
	require.NoError(t, reused.InputTransaction.Commit(t.Context()))
	require.True(t, fixture.httpComponent.GetStore().HasActiveLease(fixture.url))
	require.NoError(t, fixture.service.RetireIncrementalCache())
	require.False(t, fixture.httpComponent.GetStore().HasActiveLease(fixture.url))
}

// Fetched content is accepted against the render's own snapshot: a watched
// input moving before the commit no longer refuses it, because the render gate
// revokes content that a later render cannot load (ADR-0030). The next render
// still reads the moved input.
func TestRenderServiceExactCycleCandidateAcceptanceToleratesMovedInputs(t *testing.T) {
	fixture := newExactCycleDirectHTTPFixture(t)
	first, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.True(t, first.InputTransaction.HasCandidates())
	require.NoError(t, fixture.routes.Add(
		incrementalTestResource("default", "late", nil), []string{"default", "late"},
	))
	require.NoError(t, first.InputTransaction.Commit(t.Context()))
	_, accepted := fixture.httpComponent.GetStore().Get(fixture.url)
	require.True(t, accepted)

	next, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.False(t, next.InputTransaction.HasCandidates())
	require.Contains(t, next.HAProxyConfig, "late")
	require.NoError(t, next.InputTransaction.Commit(t.Context()))
	require.NoError(t, fixture.service.RetireIncrementalCache())
}

func TestRenderServiceExactCycleDirectHTTPSkipsRootAcrossUnrelatedWatchedChange(t *testing.T) {
	fixture := newExactCycleDirectHTTPFixture(t)
	first, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, first.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, fixture.service)
	require.Equal(t, uint64(1), fixture.requests.Load())
	require.Equal(t, exactCycleCandidateGraph, fixture.service.exactCycleCandidate.mode)

	require.NoError(t, fixture.unused.Update(
		incrementalTestResource("default", "unused", map[string]any{"revision": 2}),
		[]string{"default", "unused"},
	))
	unrelated, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.Nil(t, unrelated.Plan)
	require.Same(t, first.CycleSnapshot, unrelated.CycleSnapshot)
	require.Equal(t, uint64(1), fixture.requests.Load())
	require.NoError(t, unrelated.InputTransaction.Commit(t.Context()))
}

func TestRenderServiceExactCycleDirectHTTPRejectsAcceptedABA(t *testing.T) {
	fixture := newExactCycleDirectHTTPFixture(t)
	first, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, first.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, fixture.service)
	before := fixture.service.exactCycleCandidate
	require.NotNil(t, before)
	beforeSnapshots := before.http.state.Snapshots()
	require.Len(t, beforeSnapshots, 1)

	fixture.body.Store("changed")
	changed, err := fixture.httpComponent.GetStore().RefreshURLVersion(t.Context(), fixture.url)
	require.NoError(t, err)
	require.NotNil(t, changed)
	require.True(t, fixture.httpComponent.GetStore().PromotePendingVersion(
		fixture.url, changed.Checksum, changed.Revision,
	))
	fixture.body.Store("accepted")
	recreated, err := fixture.httpComponent.GetStore().RefreshURLVersion(t.Context(), fixture.url)
	require.NoError(t, err)
	require.NotNil(t, recreated)
	require.True(t, fixture.httpComponent.GetStore().PromotePendingVersion(
		fixture.url, recreated.Checksum, recreated.Revision,
	))
	requestsBeforeRender := fixture.requests.Load()

	result, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.Contains(t, result.HAProxyConfig, "accepted")
	require.Equal(t, requestsBeforeRender, fixture.requests.Load())
	require.NoError(t, result.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, fixture.service)
	require.NotSame(t, before, fixture.service.exactCycleCandidate)
	afterSnapshots := fixture.service.exactCycleCandidate.http.state.Snapshots()
	require.Len(t, afterSnapshots, 1)
	require.NotEqual(t, beforeSnapshots[0].Token, afterSnapshots[0].Token)
}

func TestRenderServiceExactCycleDirectHTTPRejectsCorruptedObservationAuthority(t *testing.T) {
	fixture := newExactCycleDirectHTTPFixture(t)
	first, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, first.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, fixture.service)
	poisoned := fixture.service.exactCycleCandidate
	require.NotNil(t, poisoned)
	poisoned.http.source = 0

	recovered, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NotSame(t, first.CycleSnapshot, recovered.CycleSnapshot)
	require.NoError(t, recovered.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, fixture.service)
	require.NotNil(t, fixture.service.exactCycleCandidate)
	require.NotSame(t, poisoned, fixture.service.exactCycleCandidate)
	require.NoError(t, fixture.service.exactCycleCandidate.validate())
}

func TestRenderServiceExactCycleInitialHTTPAbortPublishesNeitherCandidateNorLease(t *testing.T) {
	fixture := newExactCycleDirectHTTPFixture(t)
	descriptor, err := purehttpstore.DescribeSource(purehttpstore.FetchOptions{Critical: true}, nil)
	require.NoError(t, err)

	aborted, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.True(t, aborted.InputTransaction.HasCandidates())
	aborted.InputTransaction.Abort()
	require.Nil(t, fixture.service.exactCycleCandidate)
	require.False(t, fixture.httpComponent.GetStore().HasActiveLease(fixture.url))
	require.False(t, fixture.httpComponent.GetStore().AcceptedSnapshot(fixture.url, descriptor).Found)

	committed, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, committed.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, fixture.service)
	require.NotNil(t, fixture.service.exactCycleCandidate)
	require.True(t, fixture.httpComponent.GetStore().HasActiveLease(fixture.url))
}

func TestRenderServiceExactCycleRejectsHTTPMutationImmediatelyAfterAuthorityRelease(t *testing.T) {
	fixture := newExactCycleDirectHTTPFixture(t)
	first, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	fixture.body.Store("changed")
	started := make(chan struct{})
	mutated := make(chan error, 1)
	first.InputTransaction = stageRenderPublication(first.InputTransaction, func() {
		go func() {
			close(started)
			version, refreshErr := fixture.httpComponent.GetStore().RefreshURLVersion(t.Context(), fixture.url)
			if refreshErr == nil && !fixture.httpComponent.GetStore().PromotePendingVersion(
				fixture.url, version.Checksum, version.Revision,
			) {
				refreshErr = fmt.Errorf("promoting refreshed HTTP version")
			}
			mutated <- refreshErr
		}()
		<-started
		require.Never(t, func() bool { return len(mutated) > 0 }, 25*time.Millisecond, time.Millisecond)
	})
	require.NoError(t, first.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, fixture.service)
	require.NoError(t, <-mutated)
	require.NotNil(t, fixture.service.exactCycleCandidate)

	changed, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NotSame(t, first.CycleSnapshot, changed.CycleSnapshot)
	require.Contains(t, changed.HAProxyConfig, "changed")
	require.NoError(t, changed.InputTransaction.Commit(t.Context()))
	require.NotNil(t, fixture.service.exactCycleCandidate)

	reused, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.Nil(t, reused.Plan)
	require.Same(t, changed.CycleSnapshot, reused.CycleSnapshot)
	require.NoError(t, reused.InputTransaction.Commit(t.Context()))
}

func TestRenderServiceExactCycleHTTPCommitRebasesOnlyOnPublication(t *testing.T) {
	fixture := newExactCycleDirectHTTPFixture(t)
	first, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, first.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, fixture.service)
	committed := fixture.service.exactCycleCandidate
	require.NotNil(t, committed)

	unrelated, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	fixture.httpComponent.GetStore().LoadFixture("https://unrelated.example.test/value", "A")
	require.NoError(t, unrelated.InputTransaction.Commit(t.Context()))
	require.Same(t, first.CycleSnapshot, unrelated.CycleSnapshot)
	rebased := fixture.service.exactCycleCandidate
	require.NotNil(t, rebased)
	require.NotSame(t, committed, rebased)
	require.Equal(
		t, fixture.httpComponent.GetStore().ReplayWatermark(), rebased.http.state.ReplayWatermark(),
	)

	aborted, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	fixture.httpComponent.GetStore().LoadFixture("https://unrelated.example.test/value", "B")
	aborted.InputTransaction.Abort()
	require.Same(t, rebased, fixture.service.exactCycleCandidate)
	require.NotEqual(
		t,
		fixture.httpComponent.GetStore().ReplayWatermark(),
		fixture.service.exactCycleCandidate.http.state.ReplayWatermark(),
	)

	afterAbort, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.Same(t, first.CycleSnapshot, afterAbort.CycleSnapshot)
	require.NoError(t, afterAbort.InputTransaction.Commit(t.Context()))
	require.Equal(
		t,
		fixture.httpComponent.GetStore().ReplayWatermark(),
		fixture.service.exactCycleCandidate.http.state.ReplayWatermark(),
	)

	beforeConflict := fixture.service.exactCycleCandidate
	relevant, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	fixture.body.Store("pending")
	version, err := fixture.httpComponent.GetStore().RefreshURLVersion(t.Context(), fixture.url)
	require.NoError(t, err)
	require.NotNil(t, version)
	require.Error(t, relevant.InputTransaction.Commit(t.Context()))
	require.Same(t, beforeConflict, fixture.service.exactCycleCandidate)
}

func TestExactCycleMatchingEvaluatesInheritedIndexes(t *testing.T) {
	fixture := newExactCycleServiceFixture(t)
	first, err := fixture.service.Render(t.Context(), fixture.provider, rendercontext.RenderModeReconcile)
	require.NoError(t, err)
	require.NoError(t, first.InputTransaction.Commit(t.Context()))
	waitForIncrementalCache(t, fixture.service)
	observations := fixture.service.exactCycleCandidate.incremental
	require.NoError(t, fixture.routes.Add(
		incrementalTestResource("default", "second", nil), []string{"default", "second"},
	))
	session, err := fixture.service.incremental.begin(
		t.Context(), fixture.provider, nil, rendercontext.RenderModeReconcile,
		map[string]any{}, rendercontext.NewResourceErrorCollector(),
		incrementalLoggerContext{logger: slog.Default()},
	)
	require.NoError(t, err)
	defer session.abort()
	require.False(t, session.cold)
	require.NotEmpty(t, session.newQueries)
	count := 0
	observations.entries.Root().Walk(func(_ []byte, observation exactCycleIncrementalObservation) bool {
		require.Same(t, observation.root.effects, session.groupIndexes[observation.group])
		count++
		return false
	})
	require.Positive(t, count)
	matched, err := observations.matches(t.Context(), session)
	require.NoError(t, err)
	require.False(t, matched, "inherited index identity does not prove pending inputs were evaluated")
}

func TestReturnToAnEarlierCycleKeepsAnHTTPReadChange(t *testing.T) {
	var body atomic.Value
	body.Store("alice")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body.Load().(string)))
	}))
	t.Cleanup(server.Close)
	cfg := &config.Config{
		Dataplane: testDataplaneConfig(),
		WatchedResources: map[string]config.WatchedResource{
			"hosts": {APIVersion: "example.test/v1", Resources: "hosts", IndexBy: []string{"metadata.namespace", "metadata.name"}},
		},
		TemplateSnippets: map[string]config.TemplateSnippet{
			"host-lines": {
				Name: "host-lines", Requires: []string{"hosts"},
				Incremental: &config.IncrementalTemplate{Source: "hosts", Group: "hosts"},
				Template:    `{{ item | dig_string("", "metadata", "name") }} {{ item | dig_string("", "spec", "value") }}` + "\n",
			},
		},
		HAProxyConfig: config.HAProxyConfig{Template: "hosts:\n{{ render \"host-lines\" }}"},
		Maps: map[string]config.MapFile{
			"users.map": {Template: fmt.Sprintf(`{{ http.Fetch(%q, map[string]any{"critical": true}) }}`, server.URL)},
		},
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
	hosts := k8sstore.NewMemoryStore(2)
	require.NoError(t, hosts.Add(incrementalTestResource("default", "h1", map[string]any{"value": "a"}), []string{"default", "h1"}))
	provider := stores.NewRealStoreProvider(map[string]stores.Store{"hosts": hosts})
	render := func(step string) *RenderResult {
		result, err := service.Render(t.Context(), provider, rendercontext.RenderModeReconcile)
		require.NoError(t, err)
		require.NoError(t, result.InputTransaction.Commit(t.Context()))
		waitForIncrementalCache(t, service)
		t.Logf("%s: cache state %q, roots reused %d, users.map %q", step, result.CacheState, result.RootsReused, auxContents(t, result)["users.map"])
		return result
	}
	setHost := func(v string) {
		require.NoError(t, hosts.Update(incrementalTestResource("default", "h1", map[string]any{"value": v}), []string{"default", "h1"}))
	}

	render("state A")
	render("state A again")
	setHost("b")
	render("state B")
	body.Store("alice bob")
	promoteIncrementalHTTPBody(t, component, server.URL)
	changed := render("state B, users changed")
	assert.Contains(t, auxContents(t, changed)["users.map"], "bob")
	assert.EqualValues(t, "replay", render("state B, unchanged").CacheState)
	setHost("a")
	back := render("back to state A")
	assert.Positive(t, back.RootsReused)
	assert.Contains(t, auxContents(t, back)["users.map"], "bob")
}
