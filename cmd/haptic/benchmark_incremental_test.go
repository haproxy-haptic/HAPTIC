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

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"
	"runtime"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	scriggo "github.com/haproxy-haptic/scriggo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/helpers"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/renderer"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testrunner"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/typebootstrap"
	"github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderartifact"
	"github.com/haproxy-haptic/HAPTIC/pkg/dataplane/renderplan"
	k8sstore "github.com/haproxy-haptic/HAPTIC/pkg/k8s/store"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
	"github.com/haproxy-haptic/HAPTIC/pkg/templating"
)

var incrementalBenchmarkResourceCounts = []int{300, 1000, 3000}

var incrementalBenchmarkProfileLabels = os.Getenv("HAPTIC_BENCHMARK_PROFILE_LABELS") != ""
var incrementalBenchmarkSetupOnly = os.Getenv("HAPTIC_BENCHMARK_SETUP_ONLY") != ""

// incrementalBenchmarkSkipOracle drops the per-iteration cold oracle. The
// oracle is a full cold render, so it dwarfs the measured warm render in any
// whole-process CPU or allocation profile. Correctness runs must leave it on;
// it is only ever set while profiling.
var incrementalBenchmarkSkipOracle = os.Getenv("HAPTIC_BENCHMARK_SKIP_ORACLE") != ""

// incrementalBenchmarkBareEngine renders on the chart's engine itself instead
// of the counting wrapper. The document and assembly caches refuse a wrapped
// engine, because a wrapper could change post-processing, so the wrapper
// measures a controller without them; the bare engine measures production.
// The execution counters read zero under it.
var incrementalBenchmarkBareEngine = os.Getenv("HAPTIC_BENCHMARK_BARE_ENGINE") != ""
var incrementalBenchmarkDisableCarrier = os.Getenv("HAPTIC_BENCHMARK_DISABLE_CARRIER") != ""
var incrementalBenchmarkDisableWaves = os.Getenv("HAPTIC_BENCHMARK_DISABLE_WAVES") != ""
var incrementalBenchmarkDisableSourceTransactions = os.Getenv("HAPTIC_BENCHMARK_DISABLE_SOURCE_TRANSACTIONS") != ""
var incrementalBenchmarkAllocationProfileRate, _ = strconv.Atoi(
	os.Getenv("HAPTIC_BENCHMARK_ALLOC_PROFILE_RATE"),
)
var incrementalBenchmarkCPUProfile = os.Getenv("HAPTIC_BENCHMARK_CPU_PROFILE")
var incrementalBenchmarkRetainedHeapProfile = os.Getenv("HAPTIC_BENCHMARK_RETAINED_HEAP_PROFILE")
var incrementalBenchmarkNativeFrameStats = os.Getenv("HAPTIC_BENCHMARK_NATIVE_FRAME_STATS") != ""
var incrementalBenchmarkComponentCounts = os.Getenv("HAPTIC_BENCHMARK_COMPONENT_COUNTS") != ""

type incrementalBenchmarkCacheCompletion struct {
	identity renderer.IncrementalCacheBuildIdentity
	err      error
}

type incrementalBenchmarkCacheLifecycle struct {
	started   chan renderer.IncrementalCacheBuildIdentity
	completed chan incrementalBenchmarkCacheCompletion
	gate      <-chan struct{}
}

func newIncrementalBenchmarkCacheLifecycle(gate <-chan struct{}) *incrementalBenchmarkCacheLifecycle {
	return &incrementalBenchmarkCacheLifecycle{
		started:   make(chan renderer.IncrementalCacheBuildIdentity, 4),
		completed: make(chan incrementalBenchmarkCacheCompletion, 4),
		gate:      gate,
	}
}

func (l *incrementalBenchmarkCacheLifecycle) IncrementalCacheBuildStarted(
	ctx context.Context,
	identity renderer.IncrementalCacheBuildIdentity,
) {
	select {
	case l.started <- identity:
	case <-ctx.Done():
		return
	}
	if l.gate == nil {
		return
	}
	select {
	case <-l.gate:
	case <-ctx.Done():
	}
}

func (l *incrementalBenchmarkCacheLifecycle) IncrementalCacheBuildCompleted(
	identity renderer.IncrementalCacheBuildIdentity,
	err error,
) {
	l.completed <- incrementalBenchmarkCacheCompletion{identity: identity, err: err}
}

func (l *incrementalBenchmarkCacheLifecycle) waitStarted(
	ctx context.Context,
) (renderer.IncrementalCacheBuildIdentity, error) {
	select {
	case identity := <-l.started:
		if err := identity.ValidateAuthentication(); err != nil {
			return renderer.IncrementalCacheBuildIdentity{}, err
		}
		return identity, nil
	case <-ctx.Done():
		return renderer.IncrementalCacheBuildIdentity{}, context.Cause(ctx)
	}
}

func (l *incrementalBenchmarkCacheLifecycle) waitCompleted(
	ctx context.Context,
	identity renderer.IncrementalCacheBuildIdentity,
) error {
	if err := identity.ValidateAuthentication(); err != nil {
		return err
	}
	select {
	case completion := <-l.completed:
		if err := completion.identity.ValidateAuthentication(); err != nil {
			return err
		}
		if !completion.identity.Same(identity) {
			return errors.New("incremental benchmark cache completion has another identity")
		}
		return completion.err
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func BenchmarkIncrementalRenderService(b *testing.B) {
	cfg, engine := newIncrementalBenchmarkEngine(b)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	for _, n := range incrementalBenchmarkResourceCounts {
		store, provider := newIncrementalBenchmarkStore(b, n)

		b.Run(fmt.Sprintf("n=%d/cold", n), func(b *testing.B) {
			benchmarkIncrementalCold(b, cfg, engine, logger, provider)
		})
		b.Run(fmt.Sprintf("n=%d/no-change", n), func(b *testing.B) {
			benchmarkIncrementalNoChange(b, cfg, engine, logger, provider)
		})
		b.Run(fmt.Sprintf("n=%d/one-change", n), func(b *testing.B) {
			benchmarkIncrementalOneChange(b, cfg, engine, logger, store, provider)
		})
	}
}

func benchmarkIncrementalCold(
	b *testing.B,
	cfg *config.Config,
	engine *incrementalBenchmarkCountingEngine,
	logger *slog.Logger,
	provider stores.StoreProvider,
) {
	b.Helper()
	var executions uint64
	var rawRenders uint64
	var outputBytes int
	var expected bundledRenderSnapshot
	haveExpected := false
	sourceBefore := engine.sourceTransactionTopology()
	engine.resetSourceTransactionMaxChildren()
	b.ReportAllocs()
	b.ResetTimer()
	b.StopTimer()
	for range b.N {
		gate := make(chan struct{})
		lifecycle := newIncrementalBenchmarkCacheLifecycle(gate)
		service := newIncrementalBenchmarkService(cfg, engine, logger, lifecycle)
		before := engine.executionCount()
		beforeRaw := engine.rawRenderCount()
		b.StartTimer()
		result, measuredBytes, err := runIncrementalBenchmarkRenderResultPhase(
			"cold-authoritative-render", service, provider, nil,
		)
		b.StopTimer()
		if err != nil {
			close(gate)
			b.Fatal(err)
		}
		outputBytes = measuredBytes
		executions += engine.executionCount() - before
		rawRenders += engine.rawRenderCount() - beforeRaw
		identity, err := lifecycle.waitStarted(b.Context())
		if err != nil {
			close(gate)
			b.Fatal(err)
		}
		close(gate)
		if err := lifecycle.waitCompleted(b.Context(), identity); err != nil {
			b.Fatal(err)
		}
		snapshot := bundledRenderAcrossServices(b, result)
		if haveExpected {
			require.Equal(b, expected, snapshot)
		} else {
			expected = snapshot
			haveExpected = true
		}
		if err := service.RetireIncrementalCache(); err != nil {
			b.Fatal(err)
		}
	}
	reportIncrementalBenchmarkMetrics(b, executions, rawRenders, outputBytes)
	reportIncrementalBenchmarkSourceTransactionMetrics(
		b,
		incrementalBenchmarkSourceTransactionTopologySince(sourceBefore, engine.sourceTransactionTopology()),
	)
}

func benchmarkIncrementalNoChange(
	b *testing.B,
	cfg *config.Config,
	engine *incrementalBenchmarkCountingEngine,
	logger *slog.Logger,
	provider stores.StoreProvider,
) {
	b.Helper()
	lifecycle := newIncrementalBenchmarkCacheLifecycle(nil)
	service := newIncrementalBenchmarkService(cfg, engine, logger, lifecycle)
	cold, err := runIncrementalBenchmarkRenderCacheReady(b.Context(), service, provider, lifecycle)
	require.NoError(b, err)
	expected := bundledRenderBytes(b, cold)
	outputBytes, err := incrementalBenchmarkOutputBytes(cold)
	require.NoError(b, err)
	before := engine.executionCount()
	beforeRaw := engine.rawRenderCount()
	sourceBefore := engine.sourceTransactionTopology()
	engine.resetSourceTransactionMaxChildren()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		result, measuredBytes, renderErr := runIncrementalBenchmarkRenderResultPhase(
			"steady-warm-render", service, provider, nil,
		)
		b.StopTimer()
		err = renderErr
		if err != nil {
			b.Fatal(err)
		}
		outputBytes = measuredBytes
		require.Equal(b, expected, bundledRenderBytes(b, result))
		b.StartTimer()
	}
	b.StopTimer()
	reportIncrementalBenchmarkMetrics(
		b, engine.executionCount()-before, engine.rawRenderCount()-beforeRaw, outputBytes,
	)
	reportIncrementalBenchmarkSourceTransactionMetrics(
		b,
		incrementalBenchmarkSourceTransactionTopologySince(sourceBefore, engine.sourceTransactionTopology()),
	)
}

func benchmarkIncrementalOneChange(
	b *testing.B,
	cfg *config.Config,
	engine *incrementalBenchmarkCountingEngine,
	logger *slog.Logger,
	store *k8sstore.MemoryStore,
	provider stores.StoreProvider,
) {
	b.Helper()
	require.NoError(b, store.Update(
		incrementalBenchmarkResource(0, "baseline"),
		[]string{"default", "route-000000"},
	))
	lifecycle := newIncrementalBenchmarkCacheLifecycle(nil)
	service := newIncrementalBenchmarkService(cfg, engine, logger, lifecycle)
	cold, err := runIncrementalBenchmarkRenderCacheReady(b.Context(), service, provider, lifecycle)
	require.NoError(b, err)
	outputBytes, err := incrementalBenchmarkOutputBytes(cold)
	require.NoError(b, err)
	before := engine.executionCount()
	beforeRaw := engine.rawRenderCount()
	sourceBefore := engine.sourceTransactionTopology()
	engine.resetSourceTransactionMaxChildren()
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		b.StopTimer()
		value := "a"
		if i%2 == 0 {
			value = "b"
		}
		err = store.Update(incrementalBenchmarkResource(0, value), []string{"default", "route-000000"})
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		result, measuredBytes, renderErr := runIncrementalBenchmarkRenderResultPhase(
			"one-change-render", service, provider, nil,
		)
		b.StopTimer()
		err = renderErr
		if err != nil {
			b.Fatal(err)
		}
		outputBytes = measuredBytes
		oracleCfg, oracleEngine := newIncrementalBenchmarkEngine(b)
		oracleLifecycle := newIncrementalBenchmarkCacheLifecycle(nil)
		oracleService := newIncrementalBenchmarkService(
			oracleCfg, oracleEngine, logger, oracleLifecycle,
		)
		oracle, oracleErr := runIncrementalBenchmarkRenderCacheReady(
			b.Context(), oracleService, provider, oracleLifecycle,
		)
		require.NoError(b, oracleErr)
		require.Equal(b, bundledRenderAcrossServices(b, oracle), bundledRenderAcrossServices(b, result))
		b.StartTimer()
	}
	b.StopTimer()
	reportIncrementalBenchmarkMetrics(
		b, engine.executionCount()-before, engine.rawRenderCount()-beforeRaw, outputBytes,
	)
	reportIncrementalBenchmarkSourceTransactionMetrics(
		b,
		incrementalBenchmarkSourceTransactionTopologySince(sourceBefore, engine.sourceTransactionTopology()),
	)
}

func BenchmarkBundledChartIncrementalRenderService(b *testing.B) {
	cfg, setup, logger, cleanup := bundledChartSetup(b)
	b.Cleanup(cleanup)

	for _, n := range incrementalBenchmarkResourceCounts {
		apps := n / objectsPerApp
		b.Run(fmt.Sprintf("n=%d/no-change", n), func(b *testing.B) {
			benchmarkBundledIncrementalNoChange(b, cfg, setup, logger, apps)
		})
		b.Run(fmt.Sprintf("n=%d/one-change", n), func(b *testing.B) {
			benchmarkBundledIncrementalOneChange(b, cfg, setup, logger, apps)
		})
	}
}

func BenchmarkBundledChartHTTPRouteIncrementalRenderService(b *testing.B) {
	suspendIncrementalBenchmarkAllocationProfilingForSetup(b)
	cfg, setup, logger, cleanup := bundledChartSetup(b)
	b.Cleanup(cleanup)

	for _, routes := range incrementalBenchmarkResourceCounts {
		b.Run(fmt.Sprintf("routes=%d/cold", routes), func(b *testing.B) {
			benchmarkBundledHTTPRouteCold(b, cfg, setup, logger, routes)
		})
		b.Run(fmt.Sprintf("routes=%d/cache-ready", routes), func(b *testing.B) {
			benchmarkBundledHTTPRouteCacheReady(b, cfg, setup, logger, routes)
		})
		b.Run(fmt.Sprintf("routes=%d/immediate-second", routes), func(b *testing.B) {
			benchmarkBundledHTTPRouteImmediateSecond(b, cfg, setup, logger, routes)
		})
		b.Run(fmt.Sprintf("routes=%d/no-change", routes), func(b *testing.B) {
			benchmarkBundledHTTPRouteNoChange(b, cfg, setup, logger, routes)
		})
		b.Run(fmt.Sprintf("routes=%d/one-change", routes), func(b *testing.B) {
			benchmarkBundledHTTPRouteOneChange(b, cfg, setup, logger, routes, benchRouteAdvanced)
		})
		// The plain shape is what the gateway-api-bench scale workload creates:
		// a host and a path prefix, routed through the frontend maps, with no
		// per-route rule in the root document.
		b.Run(fmt.Sprintf("routes=%d/plain/one-change", routes), func(b *testing.B) {
			benchmarkBundledHTTPRouteOneChange(b, cfg, setup, logger, routes, benchRoutePlain)
		})
		// The probe workload: plain routes created one after another, each
		// adding a backend to the root document.
		b.Run(fmt.Sprintf("routes=%d/plain/add-one", routes), func(b *testing.B) {
			benchmarkBundledHTTPRouteAddOne(b, cfg, setup, logger, routes, benchRoutePlain)
		})
		// The scale workload's churn: a pod moves, so one backend's servers
		// change while the root document stays as it was.
		b.Run(fmt.Sprintf("routes=%d/plain/endpoint-change", routes), func(b *testing.B) {
			benchmarkBundledHTTPRouteEndpointChange(b, cfg, setup, logger, routes, benchRoutePlain)
		})
	}
}

// The route count stays fixed while the Gateway fleet grows, so a growing cost
// for the same unrelated one-route change is the per-Gateway/listener work of
// the root snippets (gateway/18-bind-per-gateway and friends), not route work.
func BenchmarkBundledChartHTTPRouteGatewayTopology(b *testing.B) {
	suspendIncrementalBenchmarkAllocationProfilingForSetup(b)
	cfg, setup, logger, cleanup := bundledChartSetup(b)
	b.Cleanup(cleanup)
	const routes = 1000
	for _, gateways := range []int{1, 10, 50} {
		topology := benchTopology{gateways: gateways, listeners: 2}
		b.Run(fmt.Sprintf("routes=%d/gateways=%d/one-change", routes, gateways), func(b *testing.B) {
			fixtures := benchGatewayTopologyFixtures(cfg, routes, topology)
			benchmarkBundledHTTPRouteChangesWithFixtures(b, cfg, setup, logger, fixtures, func(iteration int) benchChange {
				resource := benchTopologyHTTPRouteContent("route-0", "svc-0", 0)
				resource["spec"].(map[string]any)["hostnames"] = []any{fmt.Sprintf("route-0-v%d.gw0-l0.example.com", iteration)}
				return benchChange{store: "httproutes", resource: resource, keys: []string{"default", "route-0"}}
			})
		})
	}
}

func BenchmarkBundledChartHTTPRouteColdIncrementalRenderer(b *testing.B) {
	cfg, setup, logger, cleanup := bundledChartSetup(b)
	b.Cleanup(cleanup)
	httpStore := createHTTPStoreForBenchmark(nil, logger)

	for _, routes := range incrementalBenchmarkResourceCounts {
		storeMap, err := createStoresForBenchmark(
			cfg,
			setup.Engine,
			benchHTTPRouteScaleFixtures(cfg, routes),
		)
		require.NoError(b, err)
		b.Run(fmt.Sprintf("routes=%d", routes), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				benchFullRender(b, cfg, setup, logger, httpStore, storeMap)
			}
		})
	}
}

func suspendIncrementalBenchmarkAllocationProfilingForSetup(tb testing.TB) {
	tb.Helper()
	if incrementalBenchmarkAllocationProfileRate <= 0 {
		return
	}
	previous := runtime.MemProfileRate
	runtime.MemProfileRate = 0
	tb.Cleanup(func() { runtime.MemProfileRate = previous })
}

func benchmarkBundledHTTPRouteCold(
	b *testing.B,
	cfg *config.Config,
	setup *ValidationSetup,
	logger *slog.Logger,
	routes int,
) {
	b.Helper()
	storeMap, err := createStoresForBenchmark(cfg, setup.Engine, benchHTTPRouteScaleFixtures(cfg, routes))
	require.NoError(b, err)
	provider := stores.NewRealStoreProvider(storeMap)
	if incrementalBenchmarkSetupOnly {
		return
	}
	control := requireBundledHTTPRouteSourceTransactionWaveControl(b, cfg, setup, logger, provider)
	runtime.GC()

	engine := newIncrementalBenchmarkCountingEngine(b, setup.Engine)
	before := engine.executionCount()
	beforeRaw := engine.rawRenderCount()
	beforeCarrierRuns := engine.carrierRunCount()
	engine.resetCarrierMaxConcurrency()
	sourceBefore := engine.sourceTransactionTopology()
	engine.resetSourceTransactionMaxChildren()
	var outputBytes int
	var baselineMemory runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&baselineMemory)
	b.ReportAllocs()
	b.ResetTimer()
	b.StopTimer()
	if incrementalBenchmarkNativeFrameStats {
		scriggo.ResetNativeFrameCallStatistics()
	}
	var retainedService *renderer.RenderService
	for range b.N {
		retainedService, outputBytes = runBundledColdBenchmarkIteration(
			b, cfg, setup, engine, logger, provider, &control, retainedService,
		)
	}
	var retainedMemory runtime.MemStats
	runtime.GC()
	if err := writeIncrementalBenchmarkRetainedHeapProfile(); err != nil {
		b.Fatal(err)
	}
	runtime.ReadMemStats(&retainedMemory)
	runtime.KeepAlive(retainedService)
	runtime.KeepAlive(provider)
	runtime.KeepAlive(storeMap)
	runtime.KeepAlive(engine)
	b.ReportMetric(float64(retainedMemory.HeapAlloc), "postgc-heap-B")
	if retainedMemory.HeapAlloc >= baselineMemory.HeapAlloc {
		b.ReportMetric(float64(retainedMemory.HeapAlloc-baselineMemory.HeapAlloc), "retained-heap-B")
	}
	if retainedService != nil {
		if err := retainedService.RetireIncrementalCache(); err != nil {
			b.Fatal(err)
		}
	}
	reportBundledColdBenchmarkMetrics(b, engine, &control, bundledColdBenchmarkCounters{
		before: before, beforeRaw: beforeRaw,
		beforeCarrierRuns: beforeCarrierRuns, sourceBefore: sourceBefore,
	}, outputBytes)
}

func runBundledColdBenchmarkIteration(
	b *testing.B,
	cfg *config.Config,
	setup *ValidationSetup,
	engine *incrementalBenchmarkCountingEngine,
	logger *slog.Logger,
	provider stores.StoreProvider,
	control *bundledHTTPRouteSourceTransactionControl,
	retainedService *renderer.RenderService,
) (service *renderer.RenderService, renderedBytes int) {
	b.Helper()
	if retainedService != nil {
		if err := retainedService.RetireIncrementalCache(); err != nil {
			b.Fatal(err)
		}
	}
	gate := make(chan struct{})
	lifecycle := newIncrementalBenchmarkCacheLifecycle(gate)
	retainedService = newBundledIncrementalBenchmarkService(cfg, setup, engine, logger, lifecycle)
	iterationExecutions := engine.executionCount()
	iterationRawRenders := engine.rawRenderCount()
	b.StartTimer()
	result, measuredBytes, renderErr := runIncrementalBenchmarkRenderResultPhase(
		"cold-authoritative-render", retainedService, provider, nil,
	)
	b.StopTimer()
	if renderErr != nil {
		close(gate)
		b.Fatal(renderErr)
	}
	identity, err := lifecycle.waitStarted(b.Context())
	if err != nil {
		close(gate)
		b.Fatal(err)
	}
	close(gate)
	if err := lifecycle.waitCompleted(b.Context(), identity); err != nil {
		b.Fatal(err)
	}
	snapshot := bundledRenderAcrossServices(b, result)
	require.Equal(b, control.snapshot, snapshot)
	require.Equal(b, control.executions, engine.executionCount()-iterationExecutions)
	require.Equal(b, control.rawRenders, engine.rawRenderCount()-iterationRawRenders)
	require.Equal(b, control.outputBytes, measuredBytes)
	return retainedService, measuredBytes
}

type bundledColdBenchmarkCounters struct {
	before            uint64
	beforeRaw         uint64
	beforeCarrierRuns uint64
	sourceBefore      incrementalBenchmarkSourceTransactionTopology
}

func reportBundledColdBenchmarkMetrics(
	b *testing.B,
	engine *incrementalBenchmarkCountingEngine,
	control *bundledHTTPRouteSourceTransactionControl,
	counters bundledColdBenchmarkCounters,
	outputBytes int,
) {
	b.Helper()
	if incrementalBenchmarkNativeFrameStats {
		stats := scriggo.ReadNativeFrameCallStatistics()
		operations := float64(max(b.N, 1))
		b.ReportMetric(float64(stats.FunctionFieldAuthenticated)/operations, "native-field-auth-calls/op")
		b.ReportMetric(float64(stats.FunctionFieldFallback)/operations, "native-field-fallback-calls/op")
		b.ReportMetric(float64(stats.InterfaceMethodAuthenticated)/operations, "native-method-auth-calls/op")
		b.ReportMetric(float64(stats.InterfaceMethodFallback)/operations, "native-method-fallback-calls/op")
	}
	reportIncrementalBenchmarkMetrics(
		b, engine.executionCount()-counters.before, engine.rawRenderCount()-counters.beforeRaw, outputBytes,
	)
	b.ReportMetric(float64(engine.carrierRunCount()-counters.beforeCarrierRuns)/float64(b.N), "carrier-run/op")
	b.ReportMetric(float64(engine.carrierMaxConcurrency()), "carrier-max")
	measuredSourceTopology := incrementalBenchmarkSourceTransactionTopologySince(
		counters.sourceBefore, engine.sourceTransactionTopology(),
	)
	if incrementalBenchmarkDisableSourceTransactions || incrementalBenchmarkDisableWaves ||
		incrementalBenchmarkDisableCarrier {
		require.Equal(b, incrementalBenchmarkSourceTransactionTopology{}, measuredSourceTopology)
	} else {
		require.Equal(b, incrementalBenchmarkSourceTransactionTopology{
			rows:        control.sourceTopology.rows * uint64(b.N),
			children:    control.sourceTopology.children * uint64(b.N),
			maxChildren: control.sourceTopology.maxChildren,
		}, measuredSourceTopology)
	}
	reportIncrementalBenchmarkSourceTransactionMetrics(
		b, measuredSourceTopology,
	)
	if incrementalBenchmarkComponentCounts {
		for _, component := range engine.sortedComponentCounts() {
			b.Logf("component executions: %s=%d", component.name, component.count)
		}
	}
}

type incrementalBenchmarkComponentCount struct {
	name  string
	count uint64
}

type bundledHTTPRouteSourceTransactionControl struct {
	snapshot       bundledRenderSnapshot
	executions     uint64
	rawRenders     uint64
	outputBytes    int
	sourceTopology incrementalBenchmarkSourceTransactionTopology
}

func requireBundledHTTPRouteSourceTransactionWaveControl(
	tb testing.TB,
	cfg *config.Config,
	setup *ValidationSetup,
	logger *slog.Logger,
	provider stores.StoreProvider,
) bundledHTTPRouteSourceTransactionControl {
	tb.Helper()
	candidateEngine := newIncrementalBenchmarkSourceEnabledWaveCandidateEngine(tb, setup.Engine)
	controlEngine := newIncrementalBenchmarkSourceDisabledWaveControlEngine(tb, setup.Engine)
	candidateLifecycle := newIncrementalBenchmarkCacheLifecycle(nil)
	controlLifecycle := newIncrementalBenchmarkCacheLifecycle(nil)
	candidateService := newBundledIncrementalBenchmarkService(
		cfg, setup, candidateEngine, logger, candidateLifecycle,
	)
	controlService := newBundledIncrementalBenchmarkService(
		cfg, setup, controlEngine, logger, controlLifecycle,
	)
	retired := false
	defer func() {
		if retired {
			return
		}
		_ = candidateService.RetireIncrementalCache()
		_ = controlService.RetireIncrementalCache()
	}()

	candidate, err := runIncrementalBenchmarkRenderCacheReady(
		tb.Context(), candidateService, provider, candidateLifecycle,
	)
	require.NoError(tb, err)
	control, err := runIncrementalBenchmarkRenderCacheReady(
		tb.Context(), controlService, provider, controlLifecycle,
	)
	require.NoError(tb, err)
	candidateSnapshot := bundledRenderAcrossServices(tb, candidate)
	controlSnapshot := bundledRenderAcrossServices(tb, control)
	require.Equal(tb, controlSnapshot, candidateSnapshot)
	require.Equal(tb, controlEngine.executionCount(), candidateEngine.executionCount())
	require.Equal(tb, controlEngine.rawRenderCount(), candidateEngine.rawRenderCount())
	candidateTopology := candidateEngine.sourceTransactionTopology()
	require.Positive(tb, candidateTopology.rows)
	require.Positive(tb, candidateTopology.children)
	require.Positive(tb, candidateTopology.maxChildren)
	require.Equal(tb, incrementalBenchmarkSourceTransactionTopology{}, controlEngine.sourceTransactionTopology())
	candidateOutputBytes, err := incrementalBenchmarkOutputBytes(candidate)
	require.NoError(tb, err)
	controlOutputBytes, err := incrementalBenchmarkOutputBytes(control)
	require.NoError(tb, err)
	require.Equal(tb, controlOutputBytes, candidateOutputBytes)
	result := bundledHTTPRouteSourceTransactionControl{
		snapshot:       controlSnapshot,
		executions:     controlEngine.executionCount(),
		rawRenders:     controlEngine.rawRenderCount(),
		outputBytes:    controlOutputBytes,
		sourceTopology: candidateTopology,
	}
	require.NoError(tb, candidateService.RetireIncrementalCache())
	require.NoError(tb, controlService.RetireIncrementalCache())
	retired = true

	candidateService = nil
	controlService = nil
	candidateEngine = nil
	controlEngine = nil
	runtime.GC()
	return result
}

func benchmarkBundledHTTPRouteCacheReady(
	b *testing.B,
	cfg *config.Config,
	setup *ValidationSetup,
	logger *slog.Logger,
	routes int,
) {
	b.Helper()
	storeMap, err := createStoresForBenchmark(cfg, setup.Engine, benchHTTPRouteScaleFixtures(cfg, routes))
	require.NoError(b, err)
	engine := newIncrementalBenchmarkCountingEngine(b, setup.Engine)
	provider := stores.NewRealStoreProvider(storeMap)
	before := engine.executionCount()
	beforeRaw := engine.rawRenderCount()
	beforeCarrierRuns := engine.carrierRunCount()
	engine.resetCarrierMaxConcurrency()
	sourceBefore := engine.sourceTransactionTopology()
	engine.resetSourceTransactionMaxChildren()
	var outputBytes int
	var expected bundledRenderSnapshot
	haveExpected := false
	if incrementalBenchmarkSetupOnly {
		return
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.StopTimer()
	for range b.N {
		lifecycle := newIncrementalBenchmarkCacheLifecycle(nil)
		service := newBundledIncrementalBenchmarkService(cfg, setup, engine, logger, lifecycle)
		b.StartTimer()
		result, measuredBytes, renderErr := runIncrementalBenchmarkRenderResultPhase(
			"cache-ready-render",
			service,
			provider,
			func(*renderer.RenderResult) error {
				identity, waitErr := lifecycle.waitStarted(b.Context())
				if waitErr != nil {
					return waitErr
				}
				return lifecycle.waitCompleted(b.Context(), identity)
			},
		)
		b.StopTimer()
		if renderErr != nil {
			b.Fatal(renderErr)
		}
		outputBytes = measuredBytes
		snapshot := bundledRenderAcrossServices(b, result)
		if haveExpected {
			require.Equal(b, expected, snapshot)
		} else {
			expected = snapshot
			haveExpected = true
		}
	}
	reportIncrementalBenchmarkMetrics(
		b, engine.executionCount()-before, engine.rawRenderCount()-beforeRaw, outputBytes,
	)
	b.ReportMetric(float64(engine.carrierRunCount()-beforeCarrierRuns)/float64(b.N), "carrier-run/op")
	b.ReportMetric(float64(engine.carrierMaxConcurrency()), "carrier-max")
	reportIncrementalBenchmarkSourceTransactionMetrics(
		b,
		incrementalBenchmarkSourceTransactionTopologySince(sourceBefore, engine.sourceTransactionTopology()),
	)
}

func benchmarkBundledHTTPRouteImmediateSecond(
	b *testing.B,
	cfg *config.Config,
	setup *ValidationSetup,
	logger *slog.Logger,
	routes int,
) {
	b.Helper()
	storeMap, err := createStoresForBenchmark(cfg, setup.Engine, benchHTTPRouteScaleFixtures(cfg, routes))
	require.NoError(b, err)
	engine := newIncrementalBenchmarkCountingEngine(b, setup.Engine)
	provider := stores.NewRealStoreProvider(storeMap)
	var executions uint64
	var rawRenders uint64
	var carrierRuns uint64
	var sourceTopology incrementalBenchmarkSourceTransactionTopology
	var outputBytes int
	engine.resetCarrierMaxConcurrency()
	if incrementalBenchmarkSetupOnly {
		return
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.StopTimer()
	for iteration := range b.N {
		measured := runBundledImmediateSecondIteration(
			b, cfg, setup, engine, logger, provider, storeMap, iteration,
		)
		outputBytes = measured.outputBytes
		executions += measured.executions
		rawRenders += measured.rawRenders
		carrierRuns += measured.carrierRuns
		sourceTopology.rows += measured.topology.rows
		sourceTopology.children += measured.topology.children
		sourceTopology.maxChildren = max(sourceTopology.maxChildren, measured.topology.maxChildren)
	}
	reportIncrementalBenchmarkMetrics(b, executions, rawRenders, outputBytes)
	b.ReportMetric(float64(carrierRuns)/float64(b.N), "carrier-run/op")
	b.ReportMetric(float64(engine.carrierMaxConcurrency()), "carrier-max")
	reportIncrementalBenchmarkSourceTransactionMetrics(b, sourceTopology)
}

type bundledImmediateSecondIteration struct {
	executions  uint64
	rawRenders  uint64
	carrierRuns uint64
	topology    incrementalBenchmarkSourceTransactionTopology
	outputBytes int
}

func runBundledImmediateSecondIteration(
	b *testing.B,
	cfg *config.Config,
	setup *ValidationSetup,
	engine *incrementalBenchmarkCountingEngine,
	logger *slog.Logger,
	provider stores.StoreProvider,
	storeMap map[string]stores.Store,
	iteration int,
) bundledImmediateSecondIteration {
	b.Helper()
	baselineVariant := "immediate-baseline-a"
	successorVariant := "immediate-successor-b"
	if iteration%2 != 0 {
		baselineVariant, successorVariant = successorVariant, baselineVariant
	}
	require.NoError(b, storeMap["httproutes"].Update(
		benchIncrementalHTTPRouteContent(baselineVariant),
		[]string{"default", "route-0"},
	))
	gate := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate) }) }
	b.Cleanup(release)
	lifecycle := newIncrementalBenchmarkCacheLifecycle(gate)
	service := newBundledIncrementalBenchmarkService(cfg, setup, engine, logger, lifecycle)
	cold, renderErr := runIncrementalBenchmarkRenderResult(service, provider)
	if renderErr != nil {
		release()
		b.Fatal(renderErr)
	}
	firstIdentity, waitErr := lifecycle.waitStarted(b.Context())
	if waitErr != nil {
		release()
		b.Fatal(waitErr)
	}
	require.NoError(b, storeMap["httproutes"].Update(
		benchIncrementalHTTPRouteContent(successorVariant),
		[]string{"default", "route-0"},
	))
	before := engine.executionCount()
	beforeRaw := engine.rawRenderCount()
	beforeCarrierRuns := engine.carrierRunCount()
	sourceBefore := engine.sourceTransactionTopology()
	engine.resetSourceTransactionMaxChildren()
	b.StartTimer()
	successor, measuredBytes, renderErr := runIncrementalBenchmarkRenderResultPhase(
		"immediate-second-render", service, provider, nil,
	)
	b.StopTimer()
	if renderErr != nil {
		release()
		b.Fatal(renderErr)
	}
	measured := bundledImmediateSecondIteration{
		executions:  engine.executionCount() - before,
		rawRenders:  engine.rawRenderCount() - beforeRaw,
		carrierRuns: engine.carrierRunCount() - beforeCarrierRuns,
		topology: incrementalBenchmarkSourceTransactionTopologySince(
			sourceBefore,
			engine.sourceTransactionTopology(),
		),
		outputBytes: measuredBytes,
	}
	requireBundledImmediateSuccession(b, lifecycle, firstIdentity, release)
	requireBundledImmediateOracleEquivalence(b, cfg, setup, logger, provider, cold, successor)
	return measured
}

func requireBundledImmediateSuccession(
	b *testing.B,
	lifecycle *incrementalBenchmarkCacheLifecycle,
	firstIdentity renderer.IncrementalCacheBuildIdentity,
	release func(),
) {
	b.Helper()
	if waitErr := lifecycle.waitCompleted(b.Context(), firstIdentity); waitErr == nil {
		release()
		b.Fatal("first cache build completed successfully before its immediate successor")
	}
	secondIdentity, waitErr := lifecycle.waitStarted(b.Context())
	if waitErr != nil {
		release()
		b.Fatal(waitErr)
	}
	firstGeneration, generationErr := firstIdentity.Generation()
	require.NoError(b, generationErr)
	secondGeneration, generationErr := secondIdentity.Generation()
	require.NoError(b, generationErr)
	require.Greater(b, secondGeneration, firstGeneration)
	release()
	if waitErr := lifecycle.waitCompleted(b.Context(), secondIdentity); waitErr != nil {
		b.Fatal(waitErr)
	}
}

func requireBundledImmediateOracleEquivalence(
	b *testing.B,
	cfg *config.Config,
	setup *ValidationSetup,
	logger *slog.Logger,
	provider stores.StoreProvider,
	cold *renderer.RenderResult,
	successor *renderer.RenderResult,
) {
	b.Helper()
	coldSnapshot := bundledRenderBytes(b, cold)
	successorSnapshot := bundledRenderBytes(b, successor)
	require.NotEqual(b, coldSnapshot, successorSnapshot)
	oracleEngine := newIncrementalBenchmarkCountingEngine(b, setup.Engine)
	oracleLifecycle := newIncrementalBenchmarkCacheLifecycle(nil)
	oracleService := newBundledIncrementalBenchmarkService(
		cfg, setup, oracleEngine, logger, oracleLifecycle,
	)
	oracle, oracleErr := runIncrementalBenchmarkRenderCacheReady(
		b.Context(), oracleService, provider, oracleLifecycle,
	)
	require.NoError(b, oracleErr)
	require.Equal(b, bundledRenderAcrossServices(b, oracle), bundledRenderAcrossServices(b, successor))
}

func writeIncrementalBenchmarkRetainedHeapProfile() error {
	if incrementalBenchmarkRetainedHeapProfile == "" {
		return nil
	}
	runtime.GC()
	var profile bytes.Buffer
	if err := pprof.WriteHeapProfile(&profile); err != nil {
		return fmt.Errorf("writing retained heap profile: %w", err)
	}
	if err := os.WriteFile(incrementalBenchmarkRetainedHeapProfile, profile.Bytes(), 0o600); err != nil {
		return fmt.Errorf("saving retained heap profile: %w", err)
	}
	return nil
}

func benchmarkBundledHTTPRouteNoChange(
	b *testing.B,
	cfg *config.Config,
	setup *ValidationSetup,
	logger *slog.Logger,
	routes int,
) {
	b.Helper()
	storeMap, err := createStoresForBenchmark(cfg, setup.Engine, benchHTTPRouteScaleFixtures(cfg, routes))
	require.NoError(b, err)
	engine := newIncrementalBenchmarkCountingEngine(b, setup.Engine)
	lifecycle := newIncrementalBenchmarkCacheLifecycle(nil)
	service := newBundledIncrementalBenchmarkService(cfg, setup, engine, logger, lifecycle)
	provider := stores.NewRealStoreProvider(storeMap)
	cold, err := runIncrementalBenchmarkRenderCacheReady(b.Context(), service, provider, lifecycle)
	require.NoError(b, err)
	expected := bundledRenderBytes(b, cold)
	outputBytes, err := incrementalBenchmarkOutputBytes(cold)
	require.NoError(b, err)
	before := engine.executionCount()
	beforeRaw := engine.rawRenderCount()
	sourceBefore := engine.sourceTransactionTopology()
	engine.resetSourceTransactionMaxChildren()

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		result, measuredBytes, renderErr := runIncrementalBenchmarkRenderResultPhase(
			"steady-warm-render", service, provider, nil,
		)
		b.StopTimer()
		err = renderErr
		if err != nil {
			b.Fatal(err)
		}
		outputBytes = measuredBytes
		require.Equal(b, expected, bundledRenderBytes(b, result))
		b.StartTimer()
	}
	b.StopTimer()
	reportIncrementalBenchmarkMetrics(
		b, engine.executionCount()-before, engine.rawRenderCount()-beforeRaw, outputBytes,
	)
	reportIncrementalBenchmarkSourceTransactionMetrics(
		b,
		incrementalBenchmarkSourceTransactionTopologySince(sourceBefore, engine.sourceTransactionTopology()),
	)
}

func benchmarkBundledHTTPRouteOneChange(
	b *testing.B,
	cfg *config.Config,
	setup *ValidationSetup,
	logger *slog.Logger,
	routes int,
	shape benchRouteShape,
) {
	b.Helper()
	benchmarkBundledHTTPRouteChanges(b, cfg, setup, logger, routes, shape, func(iteration int) benchChange {
		// Every iteration is a change the controller has not seen: alternating
		// two variants lets the document cache adopt a whole earlier assembly,
		// which a fleet's stream of distinct changes never allows.
		return benchChange{
			store:    "httproutes",
			resource: benchIncrementalHTTPRouteContentShaped(fmt.Sprintf("v%d", iteration), shape),
			keys:     []string{"default", "route-0"},
		}
	})
}

func benchmarkBundledHTTPRouteAddOne(
	b *testing.B,
	cfg *config.Config,
	setup *ValidationSetup,
	logger *slog.Logger,
	routes int,
	shape benchRouteShape,
) {
	b.Helper()
	benchmarkBundledHTTPRouteChanges(b, cfg, setup, logger, routes, shape, func(iteration int) benchChange {
		name := fmt.Sprintf("route-%d", routes+iteration)
		return benchChange{
			store:    "httproutes",
			resource: benchHTTPRouteContentShaped(name, "svc-0", shape),
			keys:     []string{"default", name},
		}
	})
}

func benchmarkBundledHTTPRouteEndpointChange(
	b *testing.B,
	cfg *config.Config,
	setup *ValidationSetup,
	logger *slog.Logger,
	routes int,
	shape benchRouteShape,
) {
	b.Helper()
	benchmarkBundledHTTPRouteChanges(b, cfg, setup, logger, routes, shape, func(iteration int) benchChange {
		slice := benchEndpointSliceContent("svc-0", 0, 0)
		endpoint := slice["endpoints"].([]any)[0].(map[string]any)
		endpoint["addresses"] = []any{fmt.Sprintf("10.200.%d.%d", (iteration>>8)&0xff, iteration&0xff)}
		return benchChange{store: "endpoints", resource: slice, keys: []string{"default", "svc-0-0"}}
	})
}

// benchChange is one store update a warm-render benchmark applies.
type benchChange struct {
	store    string
	resource map[string]any
	keys     []string
}

// benchmarkBundledHTTPRouteChanges measures one warm render per change that
// nextChange produces, on a cache built over the fixtures.
func benchmarkBundledHTTPRouteChanges(
	b *testing.B,
	cfg *config.Config,
	setup *ValidationSetup,
	logger *slog.Logger,
	routes int,
	shape benchRouteShape,
	nextChange func(iteration int) benchChange,
) {
	b.Helper()
	benchmarkBundledHTTPRouteChangesWithFixtures(
		b, cfg, setup, logger, benchHTTPRouteScaleFixturesShaped(cfg, routes, shape), nextChange,
	)
}

func benchmarkBundledHTTPRouteChangesWithFixtures(
	b *testing.B,
	cfg *config.Config,
	setup *ValidationSetup,
	logger *slog.Logger,
	fixtures map[string][]any,
	nextChange func(iteration int) benchChange,
) {
	b.Helper()
	storeMap, err := createStoresForBenchmark(cfg, setup.Engine, fixtures)
	require.NoError(b, err)
	engine := newIncrementalBenchmarkCountingEngine(b, setup.Engine)
	lifecycle := newIncrementalBenchmarkCacheLifecycle(nil)
	var serviceEngine templating.Engine = engine
	if incrementalBenchmarkBareEngine {
		serviceEngine = setup.Engine
	}
	service := newBundledIncrementalBenchmarkService(cfg, setup, serviceEngine, logger, lifecycle)
	provider := stores.NewRealStoreProvider(storeMap)
	cold, err := runIncrementalBenchmarkRenderCacheReady(b.Context(), service, provider, lifecycle)
	require.NoError(b, err)
	outputBytes, err := incrementalBenchmarkOutputBytes(cold)
	require.NoError(b, err)
	before := engine.executionCount()
	beforeRaw := engine.rawRenderCount()
	beforeRawNanos := engine.rawRenderDuration()
	sourceBefore := engine.sourceTransactionTopology()
	engine.resetSourceTransactionMaxChildren()

	b.ReportAllocs()
	b.ResetTimer()
	for iteration := range b.N {
		b.StopTimer()
		change := nextChange(iteration)
		if err = storeMap[change.store].Update(change.resource, change.keys); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		result, measuredBytes, renderErr := runIncrementalBenchmarkRenderResultPhase(
			"one-change-render", service, provider, nil,
		)
		b.StopTimer()
		err = renderErr
		if err != nil {
			b.Fatal(err)
		}
		outputBytes = measuredBytes
		if !incrementalBenchmarkSkipOracle {
			oracleEngine := newIncrementalBenchmarkCountingEngine(b, setup.Engine)
			oracleLifecycle := newIncrementalBenchmarkCacheLifecycle(nil)
			oracleService := newBundledIncrementalBenchmarkService(
				cfg, setup, oracleEngine, logger, oracleLifecycle,
			)
			oracle, oracleErr := runIncrementalBenchmarkRenderCacheReady(
				b.Context(), oracleService, provider, oracleLifecycle,
			)
			require.NoError(b, oracleErr)
			require.Equal(b, bundledRenderAcrossServices(b, oracle), bundledRenderAcrossServices(b, result))
		}
		b.StartTimer()
	}
	b.StopTimer()
	reportIncrementalBenchmarkMetrics(
		b, engine.executionCount()-before, engine.rawRenderCount()-beforeRaw, outputBytes,
	)
	reportIncrementalBenchmarkRootRenderCost(b, engine.rawRenderDuration()-beforeRawNanos)
	reportIncrementalBenchmarkSourceTransactionMetrics(
		b,
		incrementalBenchmarkSourceTransactionTopologySince(sourceBefore, engine.sourceTransactionTopology()),
	)
}

func benchmarkBundledIncrementalNoChange(
	b *testing.B,
	cfg *config.Config,
	setup *ValidationSetup,
	logger *slog.Logger,
	apps int,
) {
	b.Helper()
	storeMap, err := createStoresForBenchmark(cfg, setup.Engine, benchScaleFixtures(cfg, apps))
	require.NoError(b, err)
	engine := newIncrementalBenchmarkCountingEngine(b, setup.Engine)
	lifecycle := newIncrementalBenchmarkCacheLifecycle(nil)
	service := newBundledIncrementalBenchmarkService(cfg, setup, engine, logger, lifecycle)
	provider := stores.NewRealStoreProvider(storeMap)
	cold, err := runIncrementalBenchmarkRenderCacheReady(b.Context(), service, provider, lifecycle)
	require.NoError(b, err)
	expected := bundledRenderBytes(b, cold)
	outputBytes, err := incrementalBenchmarkOutputBytes(cold)
	require.NoError(b, err)
	before := engine.executionCount()
	beforeRaw := engine.rawRenderCount()
	sourceBefore := engine.sourceTransactionTopology()
	engine.resetSourceTransactionMaxChildren()

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		result, measuredBytes, renderErr := runIncrementalBenchmarkRenderResultPhase(
			"steady-warm-render", service, provider, nil,
		)
		b.StopTimer()
		err = renderErr
		if err != nil {
			b.Fatal(err)
		}
		outputBytes = measuredBytes
		require.Equal(b, expected, bundledRenderBytes(b, result))
		b.StartTimer()
	}
	b.StopTimer()
	reportIncrementalBenchmarkMetrics(
		b, engine.executionCount()-before, engine.rawRenderCount()-beforeRaw, outputBytes,
	)
	reportIncrementalBenchmarkSourceTransactionMetrics(
		b,
		incrementalBenchmarkSourceTransactionTopologySince(sourceBefore, engine.sourceTransactionTopology()),
	)
}

func benchmarkBundledIncrementalOneChange(
	b *testing.B,
	cfg *config.Config,
	setup *ValidationSetup,
	logger *slog.Logger,
	apps int,
) {
	b.Helper()
	storeMap, err := createStoresForBenchmark(cfg, setup.Engine, benchScaleFixtures(cfg, apps))
	require.NoError(b, err)
	engine := newIncrementalBenchmarkCountingEngine(b, setup.Engine)
	lifecycle := newIncrementalBenchmarkCacheLifecycle(nil)
	service := newBundledIncrementalBenchmarkService(cfg, setup, engine, logger, lifecycle)
	provider := stores.NewRealStoreProvider(storeMap)
	cold, err := runIncrementalBenchmarkRenderCacheReady(b.Context(), service, provider, lifecycle)
	require.NoError(b, err)
	outputBytes, err := incrementalBenchmarkOutputBytes(cold)
	require.NoError(b, err)
	before := engine.executionCount()
	beforeRaw := engine.rawRenderCount()
	sourceBefore := engine.sourceTransactionTopology()
	engine.resetSourceTransactionMaxChildren()

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		b.StopTimer()
		variant := "a"
		if i%2 == 0 {
			variant = "b"
		}
		err = storeMap[ingressStoreName].Update(
			benchIncrementalIngressContent("app-0", "svc-0", variant),
			[]string{"default", "app-0"},
		)
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		result, measuredBytes, renderErr := runIncrementalBenchmarkRenderResultPhase(
			"one-change-render", service, provider, nil,
		)
		b.StopTimer()
		if renderErr != nil {
			b.Fatal(renderErr)
		}
		outputBytes = measuredBytes
		if !incrementalBenchmarkSkipOracle {
			oracleEngine := newIncrementalBenchmarkCountingEngine(b, setup.Engine)
			oracleLifecycle := newIncrementalBenchmarkCacheLifecycle(nil)
			oracleService := newBundledIncrementalBenchmarkService(
				cfg, setup, oracleEngine, logger, oracleLifecycle,
			)
			oracle, oracleErr := runIncrementalBenchmarkRenderCacheReady(
				b.Context(), oracleService, provider, oracleLifecycle,
			)
			require.NoError(b, oracleErr)
			require.Equal(b, bundledRenderAcrossServices(b, oracle), bundledRenderAcrossServices(b, result))
		}
		b.StartTimer()
	}
	b.StopTimer()
	reportIncrementalBenchmarkMetrics(
		b, engine.executionCount()-before, engine.rawRenderCount()-beforeRaw, outputBytes,
	)
	reportIncrementalBenchmarkSourceTransactionMetrics(
		b,
		incrementalBenchmarkSourceTransactionTopologySince(sourceBefore, engine.sourceTransactionTopology()),
	)
}

func TestIncrementalRenderServiceExecutionScaling(t *testing.T) {
	cfg, engine := newIncrementalBenchmarkEngine(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	for _, n := range incrementalBenchmarkResourceCounts {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			store, provider := newIncrementalBenchmarkStore(t, n)
			lifecycle := newIncrementalBenchmarkCacheLifecycle(nil)
			service := newIncrementalBenchmarkService(cfg, engine, logger, lifecycle)

			before := engine.executionCount()
			cold, err := runIncrementalBenchmarkRenderCacheReady(t.Context(), service, provider, lifecycle)
			require.NoError(t, err)
			coldBytes, err := incrementalBenchmarkOutputBytes(cold)
			require.NoError(t, err)
			assert.Equal(t, uint64(n), engine.executionCount()-before)

			before = engine.executionCount()
			warmBytes, err := runIncrementalBenchmarkRender(service, provider)
			require.NoError(t, err)
			assert.Equal(t, coldBytes, warmBytes)
			assert.Zero(t, engine.executionCount()-before)

			require.NoError(t, store.Update(
				incrementalBenchmarkResource(0, "changed"),
				[]string{"default", "route-000000"},
			))
			before = engine.executionCount()
			changedBytes, err := runIncrementalBenchmarkRender(service, provider)
			require.NoError(t, err)
			assert.Greater(t, changedBytes, warmBytes)
			assert.Equal(t, uint64(1), engine.executionCount()-before)
		})
	}
}

func TestIncrementalBenchmarkCacheLifecyclePhases(t *testing.T) {
	t.Run("cache ready then steady warm", func(t *testing.T) {
		cfg, engine := newIncrementalBenchmarkEngine(t)
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		_, provider := newIncrementalBenchmarkStore(t, 2)
		lifecycle := newIncrementalBenchmarkCacheLifecycle(nil)
		service := newIncrementalBenchmarkService(cfg, engine, logger, lifecycle)

		cold, err := runIncrementalBenchmarkRenderCacheReady(t.Context(), service, provider, lifecycle)
		require.NoError(t, err)
		before := engine.executionCount()
		beforeRaw := engine.rawRenderCount()
		warm, err := runIncrementalBenchmarkRenderResult(service, provider)
		require.NoError(t, err)
		require.Equal(t, bundledRenderBytes(t, cold), bundledRenderBytes(t, warm))
		require.Zero(t, engine.executionCount()-before)
		require.Zero(t, engine.rawRenderCount()-beforeRaw)
	})

	t.Run("immediate successor supersedes pending build", func(t *testing.T) {
		cfg, engine := newIncrementalBenchmarkEngine(t)
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		store, provider := newIncrementalBenchmarkStore(t, 2)
		gate := make(chan struct{})
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(gate) }) }
		t.Cleanup(release)
		lifecycle := newIncrementalBenchmarkCacheLifecycle(gate)
		service := newIncrementalBenchmarkService(cfg, engine, logger, lifecycle)

		cold, err := runIncrementalBenchmarkRenderResult(service, provider)
		require.NoError(t, err)
		firstIdentity, err := lifecycle.waitStarted(t.Context())
		require.NoError(t, err)
		require.NoError(t, store.Update(
			incrementalBenchmarkResource(0, "changed"),
			[]string{"default", "route-000000"},
		))
		before := engine.executionCount()
		successor, err := runIncrementalBenchmarkRenderResult(service, provider)
		require.NoError(t, err)
		require.Positive(t, engine.executionCount()-before)
		require.Error(t, lifecycle.waitCompleted(t.Context(), firstIdentity))
		secondIdentity, err := lifecycle.waitStarted(t.Context())
		require.NoError(t, err)
		firstGeneration, err := firstIdentity.Generation()
		require.NoError(t, err)
		secondGeneration, err := secondIdentity.Generation()
		require.NoError(t, err)
		require.Greater(t, secondGeneration, firstGeneration)
		release()
		require.NoError(t, lifecycle.waitCompleted(t.Context(), secondIdentity))
		require.NotEqual(t, bundledRenderBytes(t, cold), bundledRenderBytes(t, successor))

		oracleCfg, oracleEngine := newIncrementalBenchmarkEngine(t)
		oracleLifecycle := newIncrementalBenchmarkCacheLifecycle(nil)
		oracleService := newIncrementalBenchmarkService(
			oracleCfg, oracleEngine, logger, oracleLifecycle,
		)
		oracle, err := runIncrementalBenchmarkRenderCacheReady(
			t.Context(), oracleService, provider, oracleLifecycle,
		)
		require.NoError(t, err)
		require.Equal(t, bundledRenderAcrossServices(t, oracle), bundledRenderAcrossServices(t, successor))
	})
}

func TestCanonicalBenchmarkStatusMapOnlyNormalizesTransitionTime(t *testing.T) {
	input := map[string]any{
		"lastTransitionTime": "2026-08-27T12:00:00Z",
		"reason":             "Accepted",
		"conditions": []any{
			map[string]any{
				"lastTransitionTime": "2026-08-27T12:00:01Z",
				"status":             "True",
			},
		},
	}
	want := map[string]any{
		"lastTransitionTime": "<runtime-transition-time>",
		"reason":             "Accepted",
		"conditions": []any{
			map[string]any{
				"lastTransitionTime": "<runtime-transition-time>",
				"status":             "True",
			},
		},
	}

	assert.Equal(t, want, canonicalBenchmarkStatusMap(input))
	assert.Equal(t, "2026-08-27T12:00:00Z", input["lastTransitionTime"])
}

func TestBundledChartIncrementalRenderServiceWarmExecution(t *testing.T) {
	cfg, setup, logger, cleanup := bundledChartSetup(t)
	t.Cleanup(cleanup)
	storeMap, err := createStoresForBenchmark(
		cfg,
		setup.Engine,
		benchScaleFixtures(cfg, 8),
	)
	require.NoError(t, err)
	engine := newIncrementalBenchmarkCountingEngine(t, setup.Engine)
	lifecycle := newIncrementalBenchmarkCacheLifecycle(nil)
	service := newBundledIncrementalBenchmarkService(cfg, setup, engine, logger, lifecycle)
	provider := stores.NewRealStoreProvider(storeMap)

	coldResult, err := runIncrementalBenchmarkRenderCacheReady(t.Context(), service, provider, lifecycle)
	require.NoError(t, err)
	coldSnapshot := bundledRenderBytes(t, coldResult)
	coldExecutions := engine.executionCount()
	require.Positive(t, coldExecutions)
	require.Positive(t, engine.rawRenderCount())

	before := engine.executionCount()
	beforeRaw := engine.rawRenderCount()
	warmResult, err := runIncrementalBenchmarkRenderResult(service, provider)
	require.NoError(t, err)
	warmSnapshot := bundledRenderBytes(t, warmResult)
	require.Equal(t, coldSnapshot, warmSnapshot)
	require.Zero(t, engine.executionCount()-before)
	require.Zero(t, engine.rawRenderCount()-beforeRaw)

	changedSnapshot, _ := assertWarmEndpointChangeRender(
		t, engine, service, provider, storeMap,
		"10.99.99.99", "", &warmSnapshot, coldExecutions,
	)

	_, secondResult := assertWarmEndpointChangeRender(
		t, engine, service, provider, storeMap,
		"10.88.88.88", "10.99.99.99", &changedSnapshot, coldExecutions,
	)

	freshEngine := newIncrementalBenchmarkCountingEngine(t, setup.Engine)
	freshLifecycle := newIncrementalBenchmarkCacheLifecycle(nil)
	freshService := newBundledIncrementalBenchmarkService(cfg, setup, freshEngine, logger, freshLifecycle)
	freshResult, err := runIncrementalBenchmarkRenderCacheReady(
		t.Context(), freshService, provider, freshLifecycle,
	)
	require.NoError(t, err)
	require.Equal(t, bundledRenderAcrossServices(t, secondResult), bundledRenderAcrossServices(t, freshResult))
}

func assertWarmEndpointChangeRender(
	t *testing.T,
	engine *incrementalBenchmarkCountingEngine,
	service *renderer.RenderService,
	provider stores.StoreProvider,
	storeMap map[string]stores.Store,
	address string,
	absentAddress string,
	previous *bundledRenderSnapshot,
	coldExecutions uint64,
) (bundledRenderSnapshot, *renderer.RenderResult) {
	t.Helper()
	changedEndpoint := benchEndpointSliceContent("svc-0", 0, 0)
	changedEndpoint["endpoints"].([]any)[0].(map[string]any)["addresses"] = []any{address}
	require.NoError(t, storeMap["endpoints"].Update(
		changedEndpoint,
		[]string{"default", "svc-0"},
	))
	selectedEndpoints, err := storeMap["endpoints"].Get("default", "svc-0")
	require.NoError(t, err)
	require.Contains(t, selectedEndpoints, changedEndpoint)
	before := engine.executionCount()
	beforeRaw := engine.rawRenderCount()
	result, err := runIncrementalBenchmarkRenderResult(service, provider)
	require.NoError(t, err)
	executions := engine.executionCount() - before
	snapshot := bundledRenderBytes(t, result)
	require.Falsef(t, reflect.DeepEqual(*previous, snapshot),
		"render output did not change; changed executions=%d", executions)
	if absentAddress != "" {
		require.NotContains(t, snapshot.HAProxyConfig, absentAddress+":8080")
	}
	require.Contains(t, snapshot.HAProxyConfig, address+":8080")
	require.Positive(t, executions)
	require.Positive(t, engine.rawRenderCount()-beforeRaw)
	require.Less(t, executions, coldExecutions)
	return snapshot, result
}

func TestBundledHTTPRouteSourceTransactionWaveControl(t *testing.T) {
	cfg, setup, logger, cleanup := bundledChartSetup(t)
	t.Cleanup(cleanup)
	if diagnostic, ok := setup.Engine.(interface{ IncrementalComponentVectorCarrierDiagnostic() error }); ok {
		require.NoError(t, diagnostic.IncrementalComponentVectorCarrierDiagnostic())
	}
	sourceRenderer, ok := setup.Engine.(templating.IncrementalComponentSourceTransactionsRenderer)
	require.True(t, ok)
	if !sourceRenderer.IncrementalComponentSourceTransactionsEligibility() {
		t.Fatalf("source transaction compilation failed: %v", sourceRenderer.RenderIncrementalComponentSourceTransactions(
			t.Context(), templating.IncrementalComponentSourceTransactionsInput{},
		))
	}
	storeMap, err := createStoresForBenchmark(
		cfg,
		setup.Engine,
		benchHTTPRouteScaleFixtures(cfg, 2),
	)
	require.NoError(t, err)
	requireBundledHTTPRouteSourceTransactionWaveControl(
		t,
		cfg,
		setup,
		logger,
		stores.NewRealStoreProvider(storeMap),
	)
}

func TestBundledChartHTTPRouteIncrementalRenderServiceWarmExecution(t *testing.T) {
	cfg, setup, logger, cleanup := bundledChartSetup(t)
	t.Cleanup(cleanup)
	storeMap, err := createStoresForBenchmark(
		cfg,
		setup.Engine,
		benchHTTPRouteScaleFixtures(cfg, 8),
	)
	require.NoError(t, err)
	engine := newIncrementalBenchmarkCountingEngine(t, setup.Engine)
	lifecycle := newIncrementalBenchmarkCacheLifecycle(nil)
	service := newBundledIncrementalBenchmarkService(cfg, setup, engine, logger, lifecycle)
	provider := stores.NewRealStoreProvider(storeMap)

	coldResult, err := runIncrementalBenchmarkRenderCacheReady(t.Context(), service, provider, lifecycle)
	require.NoError(t, err)
	coldSnapshot := bundledRenderBytes(t, coldResult)
	coldExecutions := engine.executionCount()
	require.Positive(t, coldExecutions)
	require.Positive(t, engine.rawRenderCount())

	beforeExecutions := engine.executionCount()
	beforeRaw := engine.rawRenderCount()
	warmResult, err := runIncrementalBenchmarkRenderResult(service, provider)
	require.NoError(t, err)
	require.Equal(t, coldSnapshot, bundledRenderBytes(t, warmResult))
	require.Zero(t, engine.executionCount()-beforeExecutions)
	require.Zero(t, engine.rawRenderCount()-beforeRaw)

	require.NoError(t, storeMap["httproutes"].Update(
		benchIncrementalHTTPRouteContent("changed"),
		[]string{"default", "route-0"},
	))
	beforeExecutions = engine.executionCount()
	beforeRaw = engine.rawRenderCount()
	changedResult, err := runIncrementalBenchmarkRenderResult(service, provider)
	require.NoError(t, err)
	changedSnapshot := bundledRenderBytes(t, changedResult)
	changedExecutions := engine.executionCount() - beforeExecutions
	require.False(t, reflect.DeepEqual(coldSnapshot, changedSnapshot), "render output did not change")
	require.Positive(t, changedExecutions)
	require.Positive(t, engine.rawRenderCount()-beforeRaw)
	require.Less(t, changedExecutions, coldExecutions)

	freshEngine := newIncrementalBenchmarkCountingEngine(t, setup.Engine)
	freshLifecycle := newIncrementalBenchmarkCacheLifecycle(nil)
	freshService := newBundledIncrementalBenchmarkService(cfg, setup, freshEngine, logger, freshLifecycle)
	freshResult, err := runIncrementalBenchmarkRenderCacheReady(
		t.Context(), freshService, provider, freshLifecycle,
	)
	require.NoError(t, err)
	require.Equal(t, bundledRenderAcrossServices(t, changedResult), bundledRenderAcrossServices(t, freshResult))
}

func TestBundledChartHapticActivationStaysExactAcrossScaleAndGovernance(t *testing.T) {
	cfg, setup, logger, cleanup := bundledChartSetup(t)
	t.Cleanup(cleanup)
	gatedComponents := bundledHapticActivationComponents(t, cfg)
	require.Len(t, gatedComponents, 101)

	var expectedOneChangeExecutions uint64
	for _, resourceCount := range incrementalBenchmarkResourceCounts {
		t.Run(fmt.Sprintf("resources=%d", resourceCount), func(t *testing.T) {
			storeMap, err := createStoresForBenchmark(
				cfg,
				setup.Engine,
				benchScaleFixtures(cfg, resourceCount/objectsPerApp),
			)
			require.NoError(t, err)
			provider := stores.NewRealStoreProvider(storeMap)
			engine := newBundledActivationCountingEngine(t, setup.Engine)
			lifecycle := newIncrementalBenchmarkCacheLifecycle(nil)
			service := newBundledIncrementalBenchmarkService(cfg, setup, engine, logger, lifecycle)

			cold, err := runIncrementalBenchmarkRenderCacheReady(t.Context(), service, provider, lifecycle)
			require.NoError(t, err)
			assertNoBundledHapticGatedExecutions(t, engine.componentCounts(), gatedComponents)

			before := engine.totalExecutions()
			warm, err := runIncrementalBenchmarkRenderResult(service, provider)
			require.NoError(t, err)
			require.Equal(t, bundledRenderBytes(t, cold), bundledRenderBytes(t, warm))
			require.Zero(t, engine.totalExecutions()-before)

			require.NoError(t, storeMap[ingressStoreName].Update(
				benchIncrementalIngressContent("app-0", "svc-0", "changed"),
				[]string{"default", "app-0"},
			))
			before = engine.totalExecutions()
			changed, err := runIncrementalBenchmarkRenderResult(service, provider)
			require.NoError(t, err)
			changedExecutions := engine.totalExecutions() - before
			require.Positive(t, changedExecutions)
			if expectedOneChangeExecutions == 0 {
				expectedOneChangeExecutions = changedExecutions
			} else {
				require.Equal(t, expectedOneChangeExecutions, changedExecutions)
			}
			assertNoBundledHapticGatedExecutions(t, engine.componentCounts(), gatedComponents)

			freshEngine := newBundledActivationCountingEngine(t, setup.Engine)
			freshLifecycle := newIncrementalBenchmarkCacheLifecycle(nil)
			freshService := newBundledIncrementalBenchmarkService(
				cfg, setup, freshEngine, logger, freshLifecycle,
			)
			freshChanged, err := runIncrementalBenchmarkRenderCacheReady(
				t.Context(), freshService, provider, freshLifecycle,
			)
			require.NoError(t, err)
			require.Equal(t, bundledRenderAcrossServices(t, freshChanged), bundledRenderAcrossServices(t, changed))
			assertNoBundledHapticGatedExecutions(t, freshEngine.componentCounts(), gatedComponents)
		})
	}
	t.Logf("one minimal Ingress change executes %d bundled components at every scale", expectedOneChangeExecutions)

	t.Run("governance injection and removal", func(t *testing.T) {
		storeMap, err := createStoresForBenchmark(cfg, setup.Engine, benchScaleFixtures(cfg, 1))
		require.NoError(t, err)
		provider := stores.NewRealStoreProvider(storeMap)
		engine := newBundledActivationCountingEngine(t, setup.Engine)
		lifecycle := newIncrementalBenchmarkCacheLifecycle(nil)
		service := newBundledIncrementalBenchmarkService(cfg, setup, engine, logger, lifecycle)

		baseline, err := runIncrementalBenchmarkRenderCacheReady(t.Context(), service, provider, lifecycle)
		require.NoError(t, err)
		assertNoBundledHapticGatedExecutions(t, engine.componentCounts(), gatedComponents)

		require.NoError(t, storeMap[ingressStoreName].Update(
			benchIncrementalIngressContent("app-0", "svc-0", "inactive"),
			[]string{"default", "app-0"},
		))
		beforeInactive := engine.componentCounts()
		_, err = runIncrementalBenchmarkRenderResult(service, provider)
		require.NoError(t, err)
		require.Empty(t, bundledHapticExecutionDelta(beforeInactive, engine.componentCounts(), gatedComponents))

		require.NoError(t, storeMap[ingressStoreName].Update(
			benchIngressContent("app-0", "svc-0"),
			[]string{"default", "app-0"},
		))
		reverted, err := runIncrementalBenchmarkRenderResult(service, provider)
		require.NoError(t, err)
		require.Equal(t, bundledRenderBytes(t, baseline), bundledRenderBytes(t, reverted))

		previousGovernance := cfg.TemplatingSettings.ExtraContext["governance"]
		t.Cleanup(func() { cfg.TemplatingSettings.ExtraContext["governance"] = previousGovernance })
		cfg.TemplatingSettings.ExtraContext["governance"] = map[string]any{
			"enabled": true,
			"rules": map[string]any{
				"inject-hsts": map[string]any{
					"enabled":  true,
					"resource": "ingresses",
					"path":     "metadata.annotations['haproxy-haptic.org/hsts']",
					"default":  "true",
				},
			},
		}
		beforeInjection := engine.componentCounts()
		injected, err := runIncrementalBenchmarkRenderResult(service, provider)
		require.NoError(t, err)
		require.Contains(t, injected.HAProxyConfig, "Strict-Transport-Security")
		require.Equal(t, map[string]uint64{"features-155-haptic-hsts": 1},
			bundledHapticExecutionDelta(beforeInjection, engine.componentCounts(), gatedComponents))

		beforeWarm := engine.totalExecutions()
		warmInjected, err := runIncrementalBenchmarkRenderResult(service, provider)
		require.NoError(t, err)
		require.Equal(t, bundledRenderBytes(t, injected), bundledRenderBytes(t, warmInjected))
		require.Zero(t, engine.totalExecutions()-beforeWarm)

		freshEngine := newBundledActivationCountingEngine(t, setup.Engine)
		freshLifecycle := newIncrementalBenchmarkCacheLifecycle(nil)
		freshService := newBundledIncrementalBenchmarkService(
			cfg, setup, freshEngine, logger, freshLifecycle,
		)
		freshInjected, err := runIncrementalBenchmarkRenderCacheReady(
			t.Context(), freshService, provider, freshLifecycle,
		)
		require.NoError(t, err)
		require.Equal(t, bundledRenderAcrossServices(t, injected), bundledRenderAcrossServices(t, freshInjected))

		beforeRemoval := engine.componentCounts()
		cfg.TemplatingSettings.ExtraContext["governance"] = previousGovernance
		removed, err := runIncrementalBenchmarkRenderResult(service, provider)
		require.NoError(t, err)
		require.Equal(t, bundledRenderBytes(t, baseline), bundledRenderBytes(t, removed))
		require.Empty(t, bundledHapticExecutionDelta(beforeRemoval, engine.componentCounts(), gatedComponents))
	})
}

type incrementalBenchmarkCountingEngine struct {
	templating.Engine
	executor             templating.IncrementalComponentExecutor
	batchExecutor        templating.IncrementalComponentBatchExecutor
	vectorRenderer       templating.IncrementalComponentVectorRenderer
	carrierRenderer      templating.IncrementalComponentVectorCarrierWavesRenderer
	waveRenderer         templating.IncrementalComponentVectorCarrierWavesRenderer
	sourceRenderer       templating.IncrementalComponentSourceTransactionsRenderer
	planner              templating.IncrementalBindingPlannerExecutor
	snapshotPlanner      templating.IncrementalBindingSnapshotPlanner
	resourceBinder       templating.IncrementalResourceBinder
	sourceResourceBinder templating.IncrementalSourceTransactionResourceBinder
	executions           atomic.Uint64
	rawRenders           atomic.Uint64
	rawRenderNanos       atomic.Int64
	carrierRuns          atomic.Uint64
	carrierActive        atomic.Int64
	carrierMax           atomic.Int64
	sourceRows           atomic.Uint64
	sourceChildren       atomic.Uint64
	sourceMaxChildren    atomic.Uint64
	componentMu          sync.Mutex
	componentCounts      map[string]uint64
}

type bundledActivationCountingEngine struct {
	templating.Engine
	executor             templating.IncrementalComponentExecutor
	batchExecutor        templating.IncrementalComponentBatchExecutor
	vectorRenderer       templating.IncrementalComponentVectorRenderer
	carrierRenderer      templating.IncrementalComponentVectorCarrierWavesRenderer
	waveRenderer         templating.IncrementalComponentVectorCarrierWavesRenderer
	sourceRenderer       templating.IncrementalComponentSourceTransactionsRenderer
	planner              templating.IncrementalBindingPlannerExecutor
	snapshotPlanner      templating.IncrementalBindingSnapshotPlanner
	resourceBinder       templating.IncrementalResourceBinder
	sourceResourceBinder templating.IncrementalSourceTransactionResourceBinder

	mu                sync.Mutex
	counts            map[string]uint64
	raw               atomic.Uint64
	carrierRuns       atomic.Uint64
	sourceRows        atomic.Uint64
	sourceChildren    atomic.Uint64
	sourceMaxChildren atomic.Uint64
}

type incrementalBenchmarkSourceTransactionTopology struct {
	rows        uint64
	children    uint64
	maxChildren uint64
}

func incrementalBenchmarkSourceTransactionTopologyFor(
	input templating.IncrementalComponentSourceTransactionsInput,
) incrementalBenchmarkSourceTransactionTopology {
	var topology incrementalBenchmarkSourceTransactionTopology
	for _, wave := range input.Waves {
		for _, transaction := range wave.Transactions {
			children := uint64(len(transaction.Children))
			topology.rows++
			topology.children += children
			topology.maxChildren = max(topology.maxChildren, children)
		}
	}
	return topology
}

func recordIncrementalBenchmarkSourceTransactionTopology(
	rows *atomic.Uint64,
	children *atomic.Uint64,
	maxChildren *atomic.Uint64,
	topology incrementalBenchmarkSourceTransactionTopology,
) {
	rows.Add(topology.rows)
	children.Add(topology.children)
	for previous := maxChildren.Load(); topology.maxChildren > previous; previous = maxChildren.Load() {
		if maxChildren.CompareAndSwap(previous, topology.maxChildren) {
			break
		}
	}
}

type benchmarkExactCyclePreparer interface {
	PrepareExactCycleReplay([]string) (*templating.ExactCycleReplayProgram, error)
}

func prepareBenchmarkExactCycleReplay(
	engine templating.Engine,
	entryPoints []string,
) (*templating.ExactCycleReplayProgram, error) {
	preparer, ok := engine.(benchmarkExactCyclePreparer)
	if !ok {
		return nil, errors.New("incremental benchmark engine has no exact cycle replay preparer")
	}
	return preparer.PrepareExactCycleReplay(entryPoints)
}

func (e *incrementalBenchmarkCountingEngine) PrepareExactCycleReplay(
	entryPoints []string,
) (*templating.ExactCycleReplayProgram, error) {
	return prepareBenchmarkExactCycleReplay(e.Engine, entryPoints)
}

func (e *bundledActivationCountingEngine) PrepareExactCycleReplay(
	entryPoints []string,
) (*templating.ExactCycleReplayProgram, error) {
	return prepareBenchmarkExactCycleReplay(e.Engine, entryPoints)
}

func (e *bundledActivationCountingEngine) RenderRawTo(
	ctx context.Context,
	templateName string,
	templateContext map[string]any,
	output io.Writer,
) ([]templating.IncludeStats, error) {
	rawRenderer, ok := e.Engine.(templating.RawTextRenderer)
	if !ok {
		return nil, errors.New("incremental benchmark engine has no raw text renderer")
	}
	e.raw.Add(1)
	return rawRenderer.RenderRawTo(ctx, templateName, templateContext, output)
}

func (e *bundledActivationCountingEngine) RawTextRenderInstrumented() bool {
	rawRenderer, ok := e.Engine.(templating.RawTextRenderer)
	return !ok || rawRenderer.RawTextRenderInstrumented()
}

func newBundledActivationCountingEngine(
	tb testing.TB,
	base templating.Engine,
) *bundledActivationCountingEngine {
	tb.Helper()
	executor, ok := base.(templating.IncrementalComponentExecutor)
	require.True(tb, ok)
	batchExecutor, _ := base.(templating.IncrementalComponentBatchExecutor)
	vectorRenderer, _ := base.(templating.IncrementalComponentVectorRenderer)
	carrierRenderer, _ := base.(templating.IncrementalComponentVectorCarrierWavesRenderer)
	waveRenderer, _ := base.(templating.IncrementalComponentVectorCarrierWavesRenderer)
	sourceRenderer, _ := base.(templating.IncrementalComponentSourceTransactionsRenderer)
	sourceResourceBinder, _ := base.(templating.IncrementalSourceTransactionResourceBinder)
	if incrementalBenchmarkDisableSourceTransactions {
		sourceRenderer = nil
		sourceResourceBinder = nil
	}
	if incrementalBenchmarkDisableWaves {
		carrierRenderer = nil
		waveRenderer = nil
		sourceRenderer = nil
		sourceResourceBinder = nil
	}
	if incrementalBenchmarkDisableCarrier {
		carrierRenderer = nil
		waveRenderer = nil
		sourceRenderer = nil
		sourceResourceBinder = nil
	}
	planner, ok := base.(templating.IncrementalBindingPlannerExecutor)
	require.True(tb, ok)
	snapshotPlanner, ok := base.(templating.IncrementalBindingSnapshotPlanner)
	require.True(tb, ok)
	resourceBinder, ok := base.(templating.IncrementalResourceBinder)
	require.True(tb, ok)
	return &bundledActivationCountingEngine{
		Engine: base, executor: executor, batchExecutor: batchExecutor, vectorRenderer: vectorRenderer,
		carrierRenderer: carrierRenderer,
		waveRenderer:    waveRenderer, sourceRenderer: sourceRenderer,
		planner: planner, snapshotPlanner: snapshotPlanner, resourceBinder: resourceBinder,
		sourceResourceBinder: sourceResourceBinder,
		counts:               map[string]uint64{},
	}
}

func (e *bundledActivationCountingEngine) RenderIncrementalComponent(
	ctx context.Context,
	templateName string,
	templateContext map[string]any,
) (string, error) {
	e.mu.Lock()
	e.counts[templateName]++
	e.mu.Unlock()
	if !incrementalBenchmarkProfileLabels {
		return e.executor.RenderIncrementalComponent(ctx, templateName, templateContext)
	}
	var output string
	var renderErr error
	pprof.Do(ctx, pprof.Labels("incremental_entrypoint", templateName), func(labelCtx context.Context) {
		output, renderErr = e.executor.RenderIncrementalComponent(labelCtx, templateName, templateContext)
	})
	return output, renderErr
}

func (e *bundledActivationCountingEngine) RenderIncrementalComponents(
	ctx context.Context,
	templateName string,
	items []templating.IncrementalComponentBatchItem,
) ([]string, error) {
	if e.batchExecutor == nil {
		return nil, errors.New("incremental benchmark engine has no component batch executor")
	}
	e.mu.Lock()
	e.counts[templateName] += uint64(len(items))
	e.mu.Unlock()
	if !incrementalBenchmarkProfileLabels {
		return e.batchExecutor.RenderIncrementalComponents(ctx, templateName, items)
	}
	var outputs []string
	var renderErr error
	pprof.Do(ctx, pprof.Labels("incremental_entrypoint", templateName), func(labelCtx context.Context) {
		outputs, renderErr = e.batchExecutor.RenderIncrementalComponents(labelCtx, templateName, items)
	})
	return outputs, renderErr
}

func (e *bundledActivationCountingEngine) IncrementalComponentVectorEligibility(
	templateName string,
) (templating.IncrementalComponentVectorEligibility, bool) {
	if e.vectorRenderer == nil {
		return templating.IncrementalComponentVectorEligibility{}, false
	}
	return e.vectorRenderer.IncrementalComponentVectorEligibility(templateName)
}

func (e *bundledActivationCountingEngine) RenderIncrementalComponentVector(
	ctx context.Context,
	templateName string,
	input templating.IncrementalComponentVectorInput,
) error {
	if e.vectorRenderer == nil {
		return errors.New("incremental benchmark engine has no component vector renderer")
	}
	e.mu.Lock()
	e.counts[templateName] += uint64(input.Count)
	e.mu.Unlock()
	if !incrementalBenchmarkProfileLabels {
		return e.vectorRenderer.RenderIncrementalComponentVector(ctx, templateName, input)
	}
	var renderErr error
	pprof.Do(ctx, pprof.Labels("incremental_entrypoint", templateName), func(labelCtx context.Context) {
		renderErr = e.vectorRenderer.RenderIncrementalComponentVector(labelCtx, templateName, input)
	})
	return renderErr
}

func (e *bundledActivationCountingEngine) IncrementalComponentVectorCarrierEligibility() (
	templating.IncrementalComponentVectorCarrierEligibility,
	bool,
) {
	if e.carrierRenderer == nil {
		return templating.IncrementalComponentVectorCarrierEligibility{}, false
	}
	return e.carrierRenderer.IncrementalComponentVectorCarrierEligibility()
}

func (e *bundledActivationCountingEngine) RenderIncrementalComponentVectorCarrierWaves(
	ctx context.Context,
	input templating.IncrementalComponentVectorCarrierWavesInput,
) error {
	if e.waveRenderer == nil {
		return errors.New("incremental benchmark engine has no component vector carrier waves renderer")
	}
	e.carrierRuns.Add(1)
	e.mu.Lock()
	for _, wave := range input.Waves {
		for _, lane := range wave.Lanes {
			e.counts[lane.TemplateName] += uint64(lane.Count)
		}
	}
	e.mu.Unlock()
	return e.waveRenderer.RenderIncrementalComponentVectorCarrierWaves(ctx, input)
}

func (e *bundledActivationCountingEngine) IncrementalComponentSourceTransactionsEligibility() bool {
	return e.sourceRenderer != nil && e.sourceResourceBinder != nil &&
		e.sourceRenderer.IncrementalComponentSourceTransactionsEligibility()
}

func (e *bundledActivationCountingEngine) RenderIncrementalComponentSourceTransactions(
	ctx context.Context,
	input templating.IncrementalComponentSourceTransactionsInput,
) error {
	if e.sourceRenderer == nil {
		return errors.New("incremental benchmark engine has no component source transactions renderer")
	}
	topology := incrementalBenchmarkSourceTransactionTopologyFor(input)
	e.carrierRuns.Add(1)
	e.mu.Lock()
	for _, wave := range input.Waves {
		for _, transaction := range wave.Transactions {
			for _, child := range transaction.Children {
				e.counts[child.TemplateName]++
			}
		}
	}
	e.mu.Unlock()
	if err := e.sourceRenderer.RenderIncrementalComponentSourceTransactions(ctx, input); err != nil {
		return err
	}
	recordIncrementalBenchmarkSourceTransactionTopology(
		&e.sourceRows, &e.sourceChildren, &e.sourceMaxChildren, topology,
	)
	return nil
}

func (e *bundledActivationCountingEngine) RenderIncrementalBindings(
	ctx context.Context,
	templateName string,
	templateContext map[string]any,
) ([]byte, error) {
	return e.planner.RenderIncrementalBindings(ctx, templateName, templateContext)
}

func (e *bundledActivationCountingEngine) SnapshotIncrementalBindingInputs(
	entryPoints []string,
	templateContext map[string]any,
) (*templating.IncrementalBindingInputSnapshot, error) {
	return e.snapshotPlanner.SnapshotIncrementalBindingInputs(entryPoints, templateContext)
}

func (e *bundledActivationCountingEngine) MatchIncrementalBindingInputs(
	entryPoints []string,
	templateContext map[string]any,
	snapshot *templating.IncrementalBindingInputSnapshot,
) bool {
	return e.snapshotPlanner.MatchIncrementalBindingInputs(entryPoints, templateContext, snapshot)
}

func (e *bundledActivationCountingEngine) RenderIncrementalBindingsSnapshot(
	ctx context.Context,
	templateName string,
	snapshot *templating.IncrementalBindingInputSnapshot,
) ([]byte, error) {
	return e.snapshotPlanner.RenderIncrementalBindingsSnapshot(ctx, templateName, snapshot)
}

func (e *bundledActivationCountingEngine) BindIncrementalResources(
	templateName string,
	resources any,
	lease templating.IncrementalResourceInvocationLease,
) (any, error) {
	return e.resourceBinder.BindIncrementalResources(templateName, resources, lease)
}

func (e *bundledActivationCountingEngine) BindIncrementalSourceTransactionResources(
	templateNames []string,
	resources any,
	lease templating.IncrementalResourceInvocationLease,
	selector templating.IncrementalSourceTransactionChildSelector,
) (any, error) {
	if e.sourceResourceBinder == nil {
		return nil, errors.New("incremental benchmark engine has no source transaction resource binder")
	}
	return e.sourceResourceBinder.BindIncrementalSourceTransactionResources(
		templateNames, resources, lease, selector,
	)
}

func (e *bundledActivationCountingEngine) GlobalUsage(name string) (used, known bool) {
	introspector, ok := e.Engine.(templating.GlobalUsageIntrospector)
	if !ok {
		return false, false
	}
	return introspector.GlobalUsage(name)
}

func (e *bundledActivationCountingEngine) PostProcessBatch(
	ctx context.Context,
	templateName string,
	inputs []string,
) ([]string, error) {
	batcher, ok := e.Engine.(templating.PostProcessBatcher)
	if !ok {
		return nil, errors.New("incremental benchmark engine has no post-processor batcher")
	}
	return batcher.PostProcessBatch(ctx, templateName, inputs)
}

func (e *bundledActivationCountingEngine) PostProcessReuseProof(
	templateName string,
) (*templating.PostProcessReuseProof, error) {
	prover, ok := e.Engine.(templating.PostProcessReuseProver)
	if !ok {
		var absent *templating.PostProcessReuseProof
		return absent, nil
	}
	return prover.PostProcessReuseProof(templateName)
}

func (e *bundledActivationCountingEngine) componentCounts() map[string]uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	result := make(map[string]uint64, len(e.counts))
	for name, count := range e.counts {
		result[name] = count
	}
	return result
}

func (e *bundledActivationCountingEngine) totalExecutions() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	var total uint64
	for _, count := range e.counts {
		total += count
	}
	return total
}

func (e *bundledActivationCountingEngine) carrierRunCount() uint64 {
	return e.carrierRuns.Load()
}

func (e *bundledActivationCountingEngine) sourceTransactionExecutions() uint64 {
	return e.totalExecutions()
}

func (e *bundledActivationCountingEngine) sourceTransactionCarrierRuns() uint64 {
	return e.carrierRunCount()
}

func (e *bundledActivationCountingEngine) sourceTransactionTopology() incrementalBenchmarkSourceTransactionTopology {
	return incrementalBenchmarkSourceTransactionTopology{
		rows:        e.sourceRows.Load(),
		children:    e.sourceChildren.Load(),
		maxChildren: e.sourceMaxChildren.Load(),
	}
}

var (
	_ benchmarkExactCyclePreparer                               = (*incrementalBenchmarkCountingEngine)(nil)
	_ benchmarkExactCyclePreparer                               = (*bundledActivationCountingEngine)(nil)
	_ templating.GlobalUsageIntrospector                        = (*incrementalBenchmarkCountingEngine)(nil)
	_ templating.IncrementalBindingSnapshotPlanner              = (*incrementalBenchmarkCountingEngine)(nil)
	_ templating.IncrementalBindingSnapshotPlanner              = (*bundledActivationCountingEngine)(nil)
	_ templating.IncrementalComponentVectorRenderer             = (*incrementalBenchmarkCountingEngine)(nil)
	_ templating.IncrementalComponentVectorRenderer             = (*bundledActivationCountingEngine)(nil)
	_ templating.IncrementalComponentVectorCarrierWavesRenderer = (*incrementalBenchmarkCountingEngine)(nil)
	_ templating.IncrementalComponentVectorCarrierWavesRenderer = (*bundledActivationCountingEngine)(nil)
	_ templating.IncrementalComponentSourceTransactionsRenderer = (*incrementalBenchmarkCountingEngine)(nil)
	_ templating.IncrementalComponentSourceTransactionsRenderer = (*bundledActivationCountingEngine)(nil)
	_ templating.IncrementalResourceBinder                      = (*incrementalBenchmarkCountingEngine)(nil)
	_ templating.IncrementalResourceBinder                      = (*bundledActivationCountingEngine)(nil)
	_ templating.IncrementalSourceTransactionResourceBinder     = (*incrementalBenchmarkCountingEngine)(nil)
	_ templating.IncrementalSourceTransactionResourceBinder     = (*bundledActivationCountingEngine)(nil)
	_ templating.PostProcessBatcher                             = (*incrementalBenchmarkCountingEngine)(nil)
	_ templating.PostProcessBatcher                             = (*bundledActivationCountingEngine)(nil)
	_ templating.PostProcessReuseProver                         = (*incrementalBenchmarkCountingEngine)(nil)
	_ templating.PostProcessReuseProver                         = (*bundledActivationCountingEngine)(nil)
)

func newIncrementalBenchmarkCountingEngine(
	tb testing.TB,
	base templating.Engine,
) *incrementalBenchmarkCountingEngine {
	tb.Helper()
	return newIncrementalBenchmarkCountingEngineWithDisabledPaths(
		tb,
		base,
		incrementalBenchmarkDisableCarrier,
		incrementalBenchmarkDisableWaves,
		incrementalBenchmarkDisableSourceTransactions,
	)
}

func newIncrementalBenchmarkSourceDisabledWaveControlEngine(
	tb testing.TB,
	base templating.Engine,
) *incrementalBenchmarkCountingEngine {
	tb.Helper()
	return newIncrementalBenchmarkCountingEngineWithDisabledPaths(tb, base, false, false, true)
}

func newIncrementalBenchmarkSourceEnabledWaveCandidateEngine(
	tb testing.TB,
	base templating.Engine,
) *incrementalBenchmarkCountingEngine {
	tb.Helper()
	return newIncrementalBenchmarkCountingEngineWithDisabledPaths(tb, base, false, false, false)
}

func newIncrementalBenchmarkCountingEngineWithDisabledPaths(
	tb testing.TB,
	base templating.Engine,
	disableCarrier bool,
	disableWaves bool,
	disableSourceTransactions bool,
) *incrementalBenchmarkCountingEngine {
	tb.Helper()
	executor, ok := base.(templating.IncrementalComponentExecutor)
	require.True(tb, ok)
	batchExecutor, _ := base.(templating.IncrementalComponentBatchExecutor)
	vectorRenderer, _ := base.(templating.IncrementalComponentVectorRenderer)
	carrierRenderer, _ := base.(templating.IncrementalComponentVectorCarrierWavesRenderer)
	waveRenderer, _ := base.(templating.IncrementalComponentVectorCarrierWavesRenderer)
	sourceRenderer, _ := base.(templating.IncrementalComponentSourceTransactionsRenderer)
	sourceResourceBinder, _ := base.(templating.IncrementalSourceTransactionResourceBinder)
	if disableSourceTransactions {
		sourceRenderer = nil
		sourceResourceBinder = nil
	}
	if disableWaves {
		carrierRenderer = nil
		waveRenderer = nil
		sourceRenderer = nil
		sourceResourceBinder = nil
	}
	if disableCarrier {
		carrierRenderer = nil
		waveRenderer = nil
		sourceRenderer = nil
		sourceResourceBinder = nil
	}
	planner, _ := base.(templating.IncrementalBindingPlannerExecutor)
	snapshotPlanner, _ := base.(templating.IncrementalBindingSnapshotPlanner)
	resourceBinder, ok := base.(templating.IncrementalResourceBinder)
	require.True(tb, ok)
	engine := &incrementalBenchmarkCountingEngine{
		Engine:               base,
		executor:             executor,
		batchExecutor:        batchExecutor,
		vectorRenderer:       vectorRenderer,
		carrierRenderer:      carrierRenderer,
		waveRenderer:         waveRenderer,
		sourceRenderer:       sourceRenderer,
		planner:              planner,
		snapshotPlanner:      snapshotPlanner,
		resourceBinder:       resourceBinder,
		sourceResourceBinder: sourceResourceBinder,
	}
	if incrementalBenchmarkComponentCounts {
		engine.componentCounts = make(map[string]uint64)
	}
	return engine
}

func (e *incrementalBenchmarkCountingEngine) RenderIncrementalComponent(
	ctx context.Context,
	templateName string,
	templateContext map[string]any,
) (string, error) {
	e.executions.Add(1)
	if !incrementalBenchmarkProfileLabels {
		return e.executor.RenderIncrementalComponent(ctx, templateName, templateContext)
	}
	var output string
	var renderErr error
	pprof.Do(ctx, pprof.Labels("incremental_entrypoint", templateName), func(labelCtx context.Context) {
		output, renderErr = e.executor.RenderIncrementalComponent(labelCtx, templateName, templateContext)
	})
	return output, renderErr
}

func (e *incrementalBenchmarkCountingEngine) RenderRawTo(
	ctx context.Context,
	templateName string,
	templateContext map[string]any,
	output io.Writer,
) ([]templating.IncludeStats, error) {
	rawRenderer, ok := e.Engine.(templating.RawTextRenderer)
	if !ok {
		return nil, errors.New("incremental benchmark engine has no raw text renderer")
	}
	e.rawRenders.Add(1)
	started := time.Now()
	stats, err := rawRenderer.RenderRawTo(ctx, templateName, templateContext, output)
	e.rawRenderNanos.Add(int64(time.Since(started)))
	return stats, err
}

func (e *incrementalBenchmarkCountingEngine) rawRenderDuration() time.Duration {
	return time.Duration(e.rawRenderNanos.Load())
}

func (e *incrementalBenchmarkCountingEngine) RawTextRenderInstrumented() bool {
	rawRenderer, ok := e.Engine.(templating.RawTextRenderer)
	return !ok || rawRenderer.RawTextRenderInstrumented()
}

func (e *incrementalBenchmarkCountingEngine) RenderIncrementalComponents(
	ctx context.Context,
	templateName string,
	items []templating.IncrementalComponentBatchItem,
) ([]string, error) {
	if e.batchExecutor == nil {
		return nil, errors.New("incremental benchmark engine has no component batch executor")
	}
	e.executions.Add(uint64(len(items)))
	if !incrementalBenchmarkProfileLabels {
		return e.batchExecutor.RenderIncrementalComponents(ctx, templateName, items)
	}
	var outputs []string
	var renderErr error
	pprof.Do(ctx, pprof.Labels("incremental_entrypoint", templateName), func(labelCtx context.Context) {
		outputs, renderErr = e.batchExecutor.RenderIncrementalComponents(labelCtx, templateName, items)
	})
	return outputs, renderErr
}

func (e *incrementalBenchmarkCountingEngine) IncrementalComponentVectorEligibility(
	templateName string,
) (templating.IncrementalComponentVectorEligibility, bool) {
	if e.vectorRenderer == nil {
		return templating.IncrementalComponentVectorEligibility{}, false
	}
	return e.vectorRenderer.IncrementalComponentVectorEligibility(templateName)
}

func (e *incrementalBenchmarkCountingEngine) RenderIncrementalComponentVector(
	ctx context.Context,
	templateName string,
	input templating.IncrementalComponentVectorInput,
) error {
	if e.vectorRenderer == nil {
		return errors.New("incremental benchmark engine has no component vector renderer")
	}
	e.executions.Add(uint64(input.Count))
	if !incrementalBenchmarkProfileLabels {
		return e.vectorRenderer.RenderIncrementalComponentVector(ctx, templateName, input)
	}
	var renderErr error
	pprof.Do(ctx, pprof.Labels("incremental_entrypoint", templateName), func(labelCtx context.Context) {
		renderErr = e.vectorRenderer.RenderIncrementalComponentVector(labelCtx, templateName, input)
	})
	return renderErr
}

func (e *incrementalBenchmarkCountingEngine) IncrementalComponentVectorCarrierEligibility() (
	templating.IncrementalComponentVectorCarrierEligibility,
	bool,
) {
	if e.carrierRenderer == nil {
		return templating.IncrementalComponentVectorCarrierEligibility{}, false
	}
	return e.carrierRenderer.IncrementalComponentVectorCarrierEligibility()
}

func (e *incrementalBenchmarkCountingEngine) RenderIncrementalComponentVectorCarrierWaves(
	ctx context.Context,
	input templating.IncrementalComponentVectorCarrierWavesInput,
) error {
	if e.waveRenderer == nil {
		return errors.New("incremental benchmark engine has no component vector carrier waves renderer")
	}
	e.carrierRuns.Add(1)
	active := e.carrierActive.Add(1)
	for maximum := e.carrierMax.Load(); active > maximum; maximum = e.carrierMax.Load() {
		if e.carrierMax.CompareAndSwap(maximum, active) {
			break
		}
	}
	defer e.carrierActive.Add(-1)
	for _, wave := range input.Waves {
		for _, lane := range wave.Lanes {
			e.executions.Add(uint64(lane.Count))
		}
	}
	return e.waveRenderer.RenderIncrementalComponentVectorCarrierWaves(ctx, input)
}

func (e *incrementalBenchmarkCountingEngine) IncrementalComponentSourceTransactionsEligibility() bool {
	return e.sourceRenderer != nil && e.sourceResourceBinder != nil &&
		e.sourceRenderer.IncrementalComponentSourceTransactionsEligibility()
}

func (e *incrementalBenchmarkCountingEngine) RenderIncrementalComponentSourceTransactions(
	ctx context.Context,
	input templating.IncrementalComponentSourceTransactionsInput,
) error {
	if e.sourceRenderer == nil {
		return errors.New("incremental benchmark engine has no component source transactions renderer")
	}
	topology := incrementalBenchmarkSourceTransactionTopologyFor(input)
	if e.componentCounts != nil {
		e.componentMu.Lock()
		for _, wave := range input.Waves {
			for _, transaction := range wave.Transactions {
				for _, child := range transaction.Children {
					e.componentCounts[child.TemplateName]++
				}
			}
		}
		e.componentMu.Unlock()
	}
	e.carrierRuns.Add(1)
	active := e.carrierActive.Add(1)
	for maximum := e.carrierMax.Load(); active > maximum; maximum = e.carrierMax.Load() {
		if e.carrierMax.CompareAndSwap(maximum, active) {
			break
		}
	}
	defer e.carrierActive.Add(-1)
	e.executions.Add(topology.children)
	if err := e.sourceRenderer.RenderIncrementalComponentSourceTransactions(ctx, input); err != nil {
		return err
	}
	recordIncrementalBenchmarkSourceTransactionTopology(
		&e.sourceRows, &e.sourceChildren, &e.sourceMaxChildren, topology,
	)
	return nil
}

func (e *incrementalBenchmarkCountingEngine) sortedComponentCounts() []incrementalBenchmarkComponentCount {
	e.componentMu.Lock()
	counts := make([]incrementalBenchmarkComponentCount, 0, len(e.componentCounts))
	for name, count := range e.componentCounts {
		counts = append(counts, incrementalBenchmarkComponentCount{name: name, count: count})
	}
	e.componentMu.Unlock()
	slices.SortFunc(counts, func(left, right incrementalBenchmarkComponentCount) int {
		return strings.Compare(left.name, right.name)
	})
	return counts
}

func (e *incrementalBenchmarkCountingEngine) carrierRunCount() uint64 {
	return e.carrierRuns.Load()
}

func (e *incrementalBenchmarkCountingEngine) carrierMaxConcurrency() int64 {
	return e.carrierMax.Load()
}

func (e *incrementalBenchmarkCountingEngine) resetCarrierMaxConcurrency() {
	e.carrierMax.Store(0)
}

func (e *incrementalBenchmarkCountingEngine) RenderIncrementalBindings(
	ctx context.Context,
	templateName string,
	templateContext map[string]any,
) ([]byte, error) {
	if e.planner == nil {
		return nil, errors.New("incremental benchmark engine has no binding planner")
	}
	return e.planner.RenderIncrementalBindings(ctx, templateName, templateContext)
}

func (e *incrementalBenchmarkCountingEngine) SnapshotIncrementalBindingInputs(
	entryPoints []string,
	templateContext map[string]any,
) (*templating.IncrementalBindingInputSnapshot, error) {
	if e.snapshotPlanner == nil {
		return nil, errors.New("incremental benchmark engine has no snapshot binding planner")
	}
	return e.snapshotPlanner.SnapshotIncrementalBindingInputs(entryPoints, templateContext)
}

func (e *incrementalBenchmarkCountingEngine) MatchIncrementalBindingInputs(
	entryPoints []string,
	templateContext map[string]any,
	snapshot *templating.IncrementalBindingInputSnapshot,
) bool {
	return e.snapshotPlanner != nil &&
		e.snapshotPlanner.MatchIncrementalBindingInputs(entryPoints, templateContext, snapshot)
}

func (e *incrementalBenchmarkCountingEngine) RenderIncrementalBindingsSnapshot(
	ctx context.Context,
	templateName string,
	snapshot *templating.IncrementalBindingInputSnapshot,
) ([]byte, error) {
	if e.snapshotPlanner == nil {
		return nil, errors.New("incremental benchmark engine has no snapshot binding planner")
	}
	return e.snapshotPlanner.RenderIncrementalBindingsSnapshot(ctx, templateName, snapshot)
}

func (e *incrementalBenchmarkCountingEngine) BindIncrementalResources(
	templateName string,
	resources any,
	lease templating.IncrementalResourceInvocationLease,
) (any, error) {
	return e.resourceBinder.BindIncrementalResources(templateName, resources, lease)
}

func (e *incrementalBenchmarkCountingEngine) BindIncrementalSourceTransactionResources(
	templateNames []string,
	resources any,
	lease templating.IncrementalResourceInvocationLease,
	selector templating.IncrementalSourceTransactionChildSelector,
) (any, error) {
	if e.sourceResourceBinder == nil {
		return nil, errors.New("incremental benchmark engine has no source transaction resource binder")
	}
	return e.sourceResourceBinder.BindIncrementalSourceTransactionResources(
		templateNames, resources, lease, selector,
	)
}

func (e *incrementalBenchmarkCountingEngine) GlobalUsage(name string) (used, known bool) {
	introspector, ok := e.Engine.(templating.GlobalUsageIntrospector)
	if !ok {
		return false, false
	}
	return introspector.GlobalUsage(name)
}

func (e *incrementalBenchmarkCountingEngine) PostProcessBatch(
	ctx context.Context,
	templateName string,
	inputs []string,
) ([]string, error) {
	batcher, ok := e.Engine.(templating.PostProcessBatcher)
	if !ok {
		return nil, errors.New("incremental benchmark engine has no post-processor batcher")
	}
	return batcher.PostProcessBatch(ctx, templateName, inputs)
}

func (e *incrementalBenchmarkCountingEngine) PostProcessReuseProof(
	templateName string,
) (*templating.PostProcessReuseProof, error) {
	prover, ok := e.Engine.(templating.PostProcessReuseProver)
	if !ok {
		var absent *templating.PostProcessReuseProof
		return absent, nil
	}
	return prover.PostProcessReuseProof(templateName)
}

func (e *incrementalBenchmarkCountingEngine) executionCount() uint64 {
	return e.executions.Load()
}

func (e *incrementalBenchmarkCountingEngine) rawRenderCount() uint64 {
	return e.rawRenders.Load()
}

func (e *incrementalBenchmarkCountingEngine) sourceTransactionExecutions() uint64 {
	return e.executionCount()
}

func (e *incrementalBenchmarkCountingEngine) sourceTransactionCarrierRuns() uint64 {
	return e.carrierRunCount()
}

func (e *incrementalBenchmarkCountingEngine) sourceTransactionTopology() incrementalBenchmarkSourceTransactionTopology {
	return incrementalBenchmarkSourceTransactionTopology{
		rows:        e.sourceRows.Load(),
		children:    e.sourceChildren.Load(),
		maxChildren: e.sourceMaxChildren.Load(),
	}
}

func (e *incrementalBenchmarkCountingEngine) resetSourceTransactionMaxChildren() {
	e.sourceMaxChildren.Store(0)
}

func TestIncrementalBenchmarkCountingEngineForwardsGlobalUsage(t *testing.T) {
	for name, source := range map[string]string{
		"used":   `{{ currentConfig["value"] }}`,
		"unused": "static",
	} {
		t.Run(name, func(t *testing.T) {
			base, err := templating.New(map[string]string{"main": source}, &templating.Options{
				EntryPoints: []string{"main"},
				Declarations: map[string]any{
					"currentConfig": (*map[string]any)(nil),
				},
			})
			require.NoError(t, err)
			wrapper := &incrementalBenchmarkCountingEngine{Engine: base}

			used, known := wrapper.GlobalUsage("currentConfig")
			assert.True(t, known)
			assert.Equal(t, name == "used", used)
		})
	}

	base, err := templating.New(map[string]string{"main": "static"}, nil)
	require.NoError(t, err)
	wrapper := &incrementalBenchmarkCountingEngine{Engine: &benchmarkEngineWithoutGlobalUsage{Engine: base}}
	used, known := wrapper.GlobalUsage("currentConfig")
	assert.False(t, used)
	assert.False(t, known)

	_, bundled := newIncrementalBenchmarkEngine(t)
	used, known = bundled.GlobalUsage("currentConfig")
	assert.False(t, used)
	assert.True(t, known)
}

func TestIncrementalBenchmarkEnginesForwardPostProcessReuseProof(t *testing.T) {
	base, err := templating.New(map[string]string{"main": "static"}, nil)
	require.NoError(t, err)
	want, err := base.PostProcessReuseProof("main")
	require.NoError(t, err)
	require.NotNil(t, want)

	wrappers := []templating.PostProcessReuseProver{
		&incrementalBenchmarkCountingEngine{Engine: base},
		&bundledActivationCountingEngine{Engine: base},
	}
	for _, wrapper := range wrappers {
		got, proofErr := wrapper.PostProcessReuseProof("main")
		require.NoError(t, proofErr)
		assert.Same(t, want, got)
	}

	withoutProof := &incrementalBenchmarkCountingEngine{
		Engine: &benchmarkEngineWithoutGlobalUsage{Engine: base},
	}
	got, err := withoutProof.PostProcessReuseProof("main")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestIncrementalBenchmarkEnginesForwardExactCycleReplay(t *testing.T) {
	base, err := templating.New(map[string]string{"main": "static"}, &templating.Options{
		EntryPoints: []string{"main"},
	})
	require.NoError(t, err)

	wrappers := []benchmarkExactCyclePreparer{
		&incrementalBenchmarkCountingEngine{Engine: base},
		&bundledActivationCountingEngine{Engine: base},
	}
	for _, wrapper := range wrappers {
		program, prepareErr := wrapper.PrepareExactCycleReplay([]string{"main"})
		require.NoError(t, prepareErr)
		require.NotNil(t, program)
	}
}

func TestIncrementalBenchmarkEnginesForwardSourceTransactions(t *testing.T) {
	base, err := templating.New(map[string]string{"main": "static"}, &templating.Options{
		EntryPoints: []string{"main"},
	})
	require.NoError(t, err)
	input := templating.IncrementalComponentSourceTransactionsInput{
		SharedContext: map[string]any{"shared": "value"},
		Waves: []templating.IncrementalComponentSourceTransactionWave{
			{Transactions: []templating.IncrementalComponentSourceTransaction{
				{Children: []templating.IncrementalComponentSourceTransactionChild{
					{TemplateName: "main", Index: 0},
					{TemplateName: "other", Index: 1},
				}},
				{Children: []templating.IncrementalComponentSourceTransactionChild{
					{TemplateName: "main", Index: 2},
				}},
			}},
		},
	}

	t.Run("execution and resource forwarding", func(t *testing.T) {
		for _, testCase := range []struct {
			name string
			new  func(testing.TB, templating.Engine) sourceTransactionBenchmarkEngine
		}{
			{
				name: "incremental",
				new: func(tb testing.TB, engine templating.Engine) sourceTransactionBenchmarkEngine {
					tb.Helper()
					return newIncrementalBenchmarkCountingEngine(tb, engine)
				},
			},
			{
				name: "activation",
				new: func(tb testing.TB, engine templating.Engine) sourceTransactionBenchmarkEngine {
					tb.Helper()
					return newBundledActivationCountingEngine(tb, engine)
				},
			},
		} {
			t.Run(testCase.name, func(t *testing.T) {
				forwarder := &benchmarkSourceTransactionForwarder{ScriggoEngine: base, eligible: true}
				wrapper := testCase.new(t, forwarder)
				require.True(t, wrapper.IncrementalComponentSourceTransactionsEligibility())
				ctx := context.WithValue(t.Context(), benchmarkSourceTransactionContextKey{}, "forwarded")
				require.NoError(t, wrapper.RenderIncrementalComponentSourceTransactions(ctx, input))
				require.Same(t, ctx, forwarder.renderContext)
				require.Equal(t, input, forwarder.renderInput)
				assert.Equal(t, uint64(3), wrapper.sourceTransactionExecutions())
				assert.Equal(t, uint64(1), wrapper.sourceTransactionCarrierRuns())
				assert.Equal(t, incrementalBenchmarkSourceTransactionTopology{
					rows: 2, children: 3, maxChildren: 2,
				}, wrapper.sourceTransactionTopology())

				resources := &struct{ value string }{value: "resource"}
				bound := &struct{ value string }{value: "bound"}
				lease := &benchmarkSourceTransactionLease{}
				selector := &benchmarkSourceTransactionSelector{}
				forwarder.boundResources = bound
				got, bindErr := wrapper.BindIncrementalSourceTransactionResources(
					[]string{"main", "other"}, resources, lease, selector,
				)
				require.NoError(t, bindErr)
				assert.Same(t, bound, got)
				assert.Equal(t, []string{"main", "other"}, forwarder.templateNames)
				assert.Same(t, resources, forwarder.resources)
				assert.Same(t, lease, forwarder.lease)
				assert.Same(t, selector, forwarder.selector)
			})
		}
	})

	t.Run("failed topology is not authenticated", func(t *testing.T) {
		wantErr := errors.New("source transaction failed")
		for _, wrapper := range []sourceTransactionBenchmarkEngine{
			newIncrementalBenchmarkCountingEngine(t, &benchmarkSourceTransactionForwarder{
				ScriggoEngine: base, eligible: true, renderErr: wantErr,
			}),
			newBundledActivationCountingEngine(t, &benchmarkSourceTransactionForwarder{
				ScriggoEngine: base, eligible: true, renderErr: wantErr,
			}),
		} {
			require.ErrorIs(t, wrapper.RenderIncrementalComponentSourceTransactions(t.Context(), input), wantErr)
			assert.Equal(t, incrementalBenchmarkSourceTransactionTopology{}, wrapper.sourceTransactionTopology())
		}
	})

	t.Run("disable flags", func(t *testing.T) {
		previousDisableCarrier := incrementalBenchmarkDisableCarrier
		previousDisableWaves := incrementalBenchmarkDisableWaves
		previousDisableSourceTransactions := incrementalBenchmarkDisableSourceTransactions
		defer func() {
			incrementalBenchmarkDisableCarrier = previousDisableCarrier
			incrementalBenchmarkDisableWaves = previousDisableWaves
			incrementalBenchmarkDisableSourceTransactions = previousDisableSourceTransactions
		}()
		for _, testCase := range []struct {
			name                      string
			disableCarrier            bool
			disableWaves              bool
			disableSourceTransactions bool
		}{
			{name: "carrier", disableCarrier: true},
			{name: "waves", disableWaves: true},
			{name: "source transactions", disableSourceTransactions: true},
		} {
			t.Run(testCase.name, func(t *testing.T) {
				incrementalBenchmarkDisableCarrier = testCase.disableCarrier
				incrementalBenchmarkDisableWaves = testCase.disableWaves
				incrementalBenchmarkDisableSourceTransactions = testCase.disableSourceTransactions
				forwarder := &benchmarkSourceTransactionForwarder{ScriggoEngine: base, eligible: true}
				incremental := newIncrementalBenchmarkCountingEngine(t, forwarder)
				activation := newBundledActivationCountingEngine(t, forwarder)
				assert.False(t, incremental.IncrementalComponentSourceTransactionsEligibility())
				assert.False(t, activation.IncrementalComponentSourceTransactionsEligibility())
				require.Error(t, incremental.RenderIncrementalComponentSourceTransactions(t.Context(), input))
				require.Error(t, activation.RenderIncrementalComponentSourceTransactions(t.Context(), input))
				if testCase.disableSourceTransactions {
					assert.NotNil(t, incremental.waveRenderer)
					assert.NotNil(t, activation.waveRenderer)
					assert.NotNil(t, incremental.carrierRenderer)
					assert.NotNil(t, activation.carrierRenderer)
				} else {
					assert.Nil(t, incremental.carrierRenderer)
					assert.Nil(t, activation.carrierRenderer)
				}
			})
		}
	})
}

func TestIncrementalBenchmarkSourceComparisonEnginesIgnoreProcessFlags(t *testing.T) {
	base, err := templating.New(map[string]string{"main": "static"}, &templating.Options{
		EntryPoints: []string{"main"},
	})
	require.NoError(t, err)
	previousDisableCarrier := incrementalBenchmarkDisableCarrier
	previousDisableWaves := incrementalBenchmarkDisableWaves
	previousDisableSourceTransactions := incrementalBenchmarkDisableSourceTransactions
	t.Cleanup(func() {
		incrementalBenchmarkDisableCarrier = previousDisableCarrier
		incrementalBenchmarkDisableWaves = previousDisableWaves
		incrementalBenchmarkDisableSourceTransactions = previousDisableSourceTransactions
	})
	incrementalBenchmarkDisableCarrier = true
	incrementalBenchmarkDisableWaves = true
	incrementalBenchmarkDisableSourceTransactions = true
	forwarder := &benchmarkSourceTransactionForwarder{ScriggoEngine: base, eligible: true}

	control := newIncrementalBenchmarkSourceDisabledWaveControlEngine(t, forwarder)
	assert.NotNil(t, control.carrierRenderer)
	assert.NotNil(t, control.waveRenderer)
	assert.Nil(t, control.sourceRenderer)
	assert.Nil(t, control.sourceResourceBinder)
	assert.False(t, control.IncrementalComponentSourceTransactionsEligibility())
	_, err = control.BindIncrementalSourceTransactionResources(
		[]string{"main"},
		struct{}{},
		&benchmarkSourceTransactionLease{},
		&benchmarkSourceTransactionSelector{},
	)
	require.Error(t, err)

	candidate := newIncrementalBenchmarkSourceEnabledWaveCandidateEngine(t, forwarder)
	assert.NotNil(t, candidate.carrierRenderer)
	assert.NotNil(t, candidate.waveRenderer)
	assert.NotNil(t, candidate.sourceRenderer)
	assert.NotNil(t, candidate.sourceResourceBinder)
	assert.True(t, candidate.IncrementalComponentSourceTransactionsEligibility())
}

type sourceTransactionBenchmarkEngine interface {
	templating.IncrementalComponentSourceTransactionsRenderer
	templating.IncrementalSourceTransactionResourceBinder
	sourceTransactionExecutions() uint64
	sourceTransactionCarrierRuns() uint64
	sourceTransactionTopology() incrementalBenchmarkSourceTransactionTopology
}

type benchmarkSourceTransactionContextKey struct{}

type benchmarkSourceTransactionLease struct{}

func (*benchmarkSourceTransactionLease) ValidateIncrementalResourceInvocation(context.Context) error {
	return nil
}

type benchmarkSourceTransactionSelector struct{}

func (*benchmarkSourceTransactionSelector) ActiveIncrementalSourceTransactionChild() (int, error) {
	return 0, nil
}

type benchmarkSourceTransactionForwarder struct {
	*templating.ScriggoEngine
	eligible       bool
	renderContext  context.Context
	renderInput    templating.IncrementalComponentSourceTransactionsInput
	templateNames  []string
	resources      any
	lease          templating.IncrementalResourceInvocationLease
	selector       templating.IncrementalSourceTransactionChildSelector
	boundResources any
	renderErr      error
}

func (e *benchmarkSourceTransactionForwarder) IncrementalComponentSourceTransactionsEligibility() bool {
	return e.eligible
}

func (e *benchmarkSourceTransactionForwarder) RenderIncrementalComponentSourceTransactions(
	ctx context.Context,
	input templating.IncrementalComponentSourceTransactionsInput,
) error {
	e.renderContext = ctx
	e.renderInput = input
	return e.renderErr
}

func (e *benchmarkSourceTransactionForwarder) BindIncrementalSourceTransactionResources(
	templateNames []string,
	resources any,
	lease templating.IncrementalResourceInvocationLease,
	selector templating.IncrementalSourceTransactionChildSelector,
) (any, error) {
	e.templateNames = slices.Clone(templateNames)
	e.resources = resources
	e.lease = lease
	e.selector = selector
	return e.boundResources, nil
}

type benchmarkEngineWithoutGlobalUsage struct {
	templating.Engine
}

func newIncrementalBenchmarkEngine(tb testing.TB) (*config.Config, *incrementalBenchmarkCountingEngine) {
	tb.Helper()
	cfg := &config.Config{
		Dataplane: config.DataplaneConfig{
			MapsDir:           "/etc/haproxy/maps",
			SSLCertsDir:       "/etc/haproxy/ssl",
			GeneralStorageDir: "/etc/haproxy/files",
		},
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
				Template: `# {{ item | dig_string("", "metadata", "name") }}={{ item | dig_string("", "spec", "value") }}
`,
			},
		},
		HAProxyConfig: config.HAProxyConfig{Template: "global\n{{ render \"routes\" }}"},
	}
	declarations := helpers.BuildAdditionalDeclarations(cfg, &typebootstrap.Result{
		Types:  map[string]reflect.Type{},
		Kinds:  map[string]string{},
		Errors: map[string]error{},
	})
	base, err := helpers.NewEngineFromConfigWithOptions(cfg, nil, nil, declarations, helpers.EngineOptions{})
	require.NoError(tb, err)
	return cfg, newIncrementalBenchmarkCountingEngine(tb, base)
}

func newBundledIncrementalBenchmarkService(
	cfg *config.Config,
	setup *ValidationSetup,
	engine templating.Engine,
	logger *slog.Logger,
	observers ...renderer.IncrementalCacheBuildObserver,
) *renderer.RenderService {
	var observer renderer.IncrementalCacheBuildObserver
	if len(observers) > 0 {
		observer = observers[0]
	}
	return renderer.NewRenderService(&renderer.RenderServiceConfig{
		Engine:                        engine,
		Config:                        cfg,
		Logger:                        logger,
		Capabilities:                  setup.Capabilities,
		TypedResourceTypes:            setup.TypedResourceTypes,
		IncrementalCacheBuildObserver: observer,
	})
}

func benchIncrementalIngressContent(name, service, variant string) map[string]any {
	resource := benchIngressContent(name, service)
	rules := resource["spec"].(map[string]any)["rules"].([]any)
	rules[0].(map[string]any)["host"] = name + "-" + variant + ".example.com"
	return resource
}

// benchRouteShape selects what an HTTPRoute fixture matches on.
type benchRouteShape int

const (
	// benchRouteAdvanced matches a header and a query parameter besides the
	// path, which the gateway library serves with per-route frontend rules.
	benchRouteAdvanced benchRouteShape = iota
	// benchRoutePlain matches host and path prefix only, which the gateway
	// library serves through its frontend maps.
	benchRoutePlain
)

func benchHTTPRouteScaleFixtures(cfg *config.Config, routes int) map[string][]any {
	return benchHTTPRouteScaleFixturesShaped(cfg, routes, benchRouteAdvanced)
}

func benchHTTPRouteScaleFixturesShaped(cfg *config.Config, routes int, shape benchRouteShape) map[string][]any {
	services := make([]any, 0, routes)
	httpRoutes := make([]any, 0, routes)
	endpoints := make([]any, 0, routes*2)
	for index := range routes {
		service := fmt.Sprintf("svc-%d", index)
		services = append(services, benchServiceContent(service))
		httpRoutes = append(httpRoutes, benchHTTPRouteContentShaped(fmt.Sprintf("route-%d", index), service, shape))
		endpoints = append(endpoints,
			benchEndpointSliceContent(service, index, 0),
			benchEndpointSliceContent(service, index, 1),
		)
	}
	fixtures := map[string][]any{
		"services":   services,
		"gateways":   {benchGatewayContent()},
		"httproutes": httpRoutes,
		"endpoints":  endpoints,
	}
	return testrunner.MergeFixtures(cfg.ValidationTests["_global"].Fixtures, fixtures)
}

// benchTopology sizes the Gateway fleet a route set attaches to.
type benchTopology struct {
	gateways  int
	listeners int
}

func benchGatewayTopologyFixtures(cfg *config.Config, routes int, topology benchTopology) map[string][]any {
	gateways := make([]any, 0, topology.gateways)
	for index := range topology.gateways {
		gateways = append(gateways, benchGatewayTopologyContent(index, topology.listeners))
	}
	services := make([]any, 0, routes)
	httpRoutes := make([]any, 0, routes)
	endpoints := make([]any, 0, routes*2)
	for index := range routes {
		service := fmt.Sprintf("svc-%d", index)
		services = append(services, benchServiceContent(service))
		httpRoutes = append(httpRoutes, benchTopologyHTTPRouteContent(fmt.Sprintf("route-%d", index), service, index%topology.gateways))
		endpoints = append(endpoints,
			benchEndpointSliceContent(service, index, 0),
			benchEndpointSliceContent(service, index, 1),
		)
	}
	fixtures := map[string][]any{
		"services":   services,
		"gateways":   gateways,
		"httproutes": httpRoutes,
		"endpoints":  endpoints,
	}
	return testrunner.MergeFixtures(cfg.ValidationTests["_global"].Fixtures, fixtures)
}

// Every listener carries its own hostname pattern, so each one is a distinct
// listener-state entry; routes attach to listener 0 of their Gateway only.
func benchGatewayTopologyContent(index, listeners int) map[string]any {
	specListeners := make([]any, 0, listeners)
	for listener := range listeners {
		specListeners = append(specListeners, map[string]any{
			"name": fmt.Sprintf("http-%d", listener), "port": int64(80 + listener), "protocol": "HTTP",
			"hostname": fmt.Sprintf("*.gw%d-l%d.example.com", index, listener),
		})
	}
	return map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "Gateway",
		"metadata":   map[string]any{"name": fmt.Sprintf("gw-%d", index), "namespace": "default"},
		"spec": map[string]any{
			"gatewayClassName": "haproxy",
			"listeners":        specListeners,
		},
	}
}

func benchTopologyHTTPRouteContent(name, service string, gateway int) map[string]any {
	resource := benchHTTPRouteContentShaped(name, service, benchRoutePlain)
	spec := resource["spec"].(map[string]any)
	spec["parentRefs"] = []any{map[string]any{"name": fmt.Sprintf("gw-%d", gateway), "namespace": "default"}}
	spec["hostnames"] = []any{fmt.Sprintf("%s.gw%d-l0.example.com", name, gateway)}
	return resource
}

func benchGatewayContent() map[string]any {
	return map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "Gateway",
		"metadata":   map[string]any{"name": "main-gateway", "namespace": "default"},
		"spec": map[string]any{
			"gatewayClassName": "haproxy",
			"listeners": []any{map[string]any{
				"name": "http", "port": int64(80), "protocol": "HTTP",
			}},
		},
	}
}

func benchHTTPRouteContentShaped(name, service string, shape benchRouteShape) map[string]any {
	match := map[string]any{
		"path": map[string]any{"type": "PathPrefix", "value": "/api"},
	}
	if shape == benchRouteAdvanced {
		match["headers"] = []any{map[string]any{"name": "X-Version", "value": "v1"}}
		match["queryParams"] = []any{map[string]any{"name": "debug", "value": "true"}}
	}
	return map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "HTTPRoute",
		"metadata":   map[string]any{"name": name, "namespace": "default"},
		"spec": map[string]any{
			"parentRefs": []any{map[string]any{"name": "main-gateway", "namespace": "default"}},
			"hostnames":  []any{name + ".example.com"},
			"rules": []any{map[string]any{
				"matches": []any{match},
				"backendRefs": []any{map[string]any{
					"name": service, "port": int64(80), "weight": int64(1),
				}},
			}},
		},
	}
}

func benchIncrementalHTTPRouteContent(variant string) map[string]any {
	return benchIncrementalHTTPRouteContentShaped(variant, benchRouteAdvanced)
}

func benchIncrementalHTTPRouteContentShaped(variant string, shape benchRouteShape) map[string]any {
	const name = "route-0"
	const service = "svc-0"
	resource := benchHTTPRouteContentShaped(name, service, shape)
	resource["spec"].(map[string]any)["hostnames"] = []any{name + "-" + variant + ".example.com"}
	return resource
}

func newIncrementalBenchmarkStore(
	tb testing.TB,
	count int,
) (*k8sstore.MemoryStore, stores.StoreProvider) {
	tb.Helper()
	store := k8sstore.NewMemoryStore(2)
	for i := range count {
		name := fmt.Sprintf("route-%06d", i)
		require.NoError(tb, store.Add(
			incrementalBenchmarkResource(i, "a"),
			[]string{"default", name},
		))
	}
	return store, stores.NewRealStoreProvider(map[string]stores.Store{"routes": store})
}

func incrementalBenchmarkResource(index int, value string) map[string]any {
	return map[string]any{
		"apiVersion": "example.test/v1",
		"kind":       "Route",
		"metadata": map[string]any{
			"namespace": "default",
			"name":      fmt.Sprintf("route-%06d", index),
		},
		"spec": map[string]any{"value": value},
	}
}

func newIncrementalBenchmarkService(
	cfg *config.Config,
	engine templating.Engine,
	logger *slog.Logger,
	observers ...renderer.IncrementalCacheBuildObserver,
) *renderer.RenderService {
	var observer renderer.IncrementalCacheBuildObserver
	if len(observers) > 0 {
		observer = observers[0]
	}
	return renderer.NewRenderService(&renderer.RenderServiceConfig{
		Engine:                        engine,
		Config:                        cfg,
		Logger:                        logger,
		IncrementalCacheBuildObserver: observer,
	})
}

func runIncrementalBenchmarkRender(
	service *renderer.RenderService,
	provider stores.StoreProvider,
) (int, error) {
	result, err := runIncrementalBenchmarkRenderResult(service, provider)
	if err != nil {
		return 0, err
	}
	return incrementalBenchmarkOutputBytes(result)
}

func runIncrementalBenchmarkRenderResultPhase(
	phase string,
	service *renderer.RenderService,
	provider stores.StoreProvider,
	after func(*renderer.RenderResult) error,
) (*renderer.RenderResult, int, error) {
	if incrementalBenchmarkAllocationProfileRate > 0 {
		runtime.MemProfileRate = incrementalBenchmarkAllocationProfileRate
	}
	if incrementalBenchmarkCPUProfile != "" {
		profile, err := os.Create(incrementalBenchmarkCPUProfile)
		if err != nil {
			return nil, 0, err
		}
		if err := pprof.StartCPUProfile(profile); err != nil {
			_ = profile.Close()
			return nil, 0, err
		}
		defer func() {
			pprof.StopCPUProfile()
			_ = profile.Close()
		}()
	}
	run := func() (*renderer.RenderResult, int, error) {
		result, err := runIncrementalBenchmarkRenderResult(service, provider)
		if err != nil {
			return nil, 0, err
		}
		outputBytes, err := incrementalBenchmarkOutputBytes(result)
		if err != nil {
			return nil, 0, err
		}
		if after != nil {
			if err := after(result); err != nil {
				return nil, 0, err
			}
		}
		return result, outputBytes, nil
	}
	if !incrementalBenchmarkProfileLabels {
		return run()
	}
	var result *renderer.RenderResult
	var outputBytes int
	var err error
	pprof.Do(context.Background(), pprof.Labels("benchmark_phase", phase), func(context.Context) {
		result, outputBytes, err = run()
	})
	return result, outputBytes, err
}

func runIncrementalBenchmarkRenderResult(
	service *renderer.RenderService,
	provider stores.StoreProvider,
) (*renderer.RenderResult, error) {
	ctx := context.Background()
	result, err := service.Render(ctx, provider, rendercontext.RenderModeReconcile)
	if err != nil {
		return nil, err
	}
	if result.InputTransaction == nil {
		return nil, errors.New("incremental render returned no input transaction")
	}
	if err := result.InputTransaction.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

func runIncrementalBenchmarkRenderCacheReady(
	ctx context.Context,
	service *renderer.RenderService,
	provider stores.StoreProvider,
	lifecycle *incrementalBenchmarkCacheLifecycle,
) (*renderer.RenderResult, error) {
	if lifecycle == nil {
		return nil, errors.New("incremental benchmark cache lifecycle is nil")
	}
	result, err := runIncrementalBenchmarkRenderResult(service, provider)
	if err != nil {
		return nil, err
	}
	if err := waitIncrementalBenchmarkCacheReady(ctx, lifecycle); err != nil {
		return nil, err
	}
	return result, nil
}

func waitIncrementalBenchmarkCacheReady(
	ctx context.Context,
	lifecycle *incrementalBenchmarkCacheLifecycle,
) error {
	if lifecycle == nil {
		return errors.New("incremental benchmark cache lifecycle is nil")
	}
	identity, err := lifecycle.waitStarted(ctx)
	if err != nil {
		return err
	}
	if err := lifecycle.waitCompleted(ctx, identity); err != nil {
		return err
	}
	return nil
}

type bundledRenderSnapshot struct {
	HAProxyConfig   string
	ContentChecksum string
	PlanID          string
	Plan            *renderplan.Plan
	Files           map[string]string
	StatusPatches   []templating.StatusPatch
	Events          []templating.RenderedEvent
	Resources       []templating.RenderedResource
}

func bundledRenderBytes(tb testing.TB, result *renderer.RenderResult) bundledRenderSnapshot {
	tb.Helper()
	plan, err := result.MaterializePlan()
	require.NoError(tb, err)
	statusPatches, err := result.MaterializeStatusPatches()
	require.NoError(tb, err)
	events, err := result.MaterializeEvents()
	require.NoError(tb, err)
	resources, err := result.MaterializeRenderedResources()
	require.NoError(tb, err)
	snapshot := bundledRenderSnapshot{
		HAProxyConfig:   result.HAProxyConfig,
		ContentChecksum: result.ContentChecksum,
		PlanID:          result.PlanID,
		Plan:            plan,
		Files:           map[string]string{},
		StatusPatches:   statusPatches,
		Events:          events,
		Resources:       resources,
	}
	files, err := result.MaterializeAuxiliaryFiles()
	require.NoError(tb, err)
	for _, file := range files.MapFiles {
		snapshot.Files["map:"+file.GetIdentifier()] = file.GetContent()
	}
	for _, file := range files.GeneralFiles {
		snapshot.Files["general:"+file.GetIdentifier()] = file.GetContent()
	}
	for _, file := range files.SSLCertificates {
		snapshot.Files["certificate:"+file.GetIdentifier()] = file.GetContent()
	}
	for _, file := range files.SSLCaFiles {
		snapshot.Files["ca:"+file.GetIdentifier()] = file.GetContent()
	}
	for _, file := range files.CRTListFiles {
		snapshot.Files["crt-list:"+file.GetIdentifier()] = file.GetContent()
	}
	return snapshot
}

func bundledRenderAcrossServices(tb testing.TB, result *renderer.RenderResult) bundledRenderSnapshot {
	tb.Helper()
	snapshot := bundledRenderBytes(tb, result)
	patches := slices.Clone(snapshot.StatusPatches)
	for patchIndex := range patches {
		if patches[patchIndex].Variants == nil {
			continue
		}
		variants := make(map[string]map[string]any, len(patches[patchIndex].Variants))
		for outcome, status := range patches[patchIndex].Variants {
			variants[outcome] = canonicalBenchmarkStatusMap(status)
		}
		patches[patchIndex].Variants = variants
	}
	snapshot.StatusPatches = patches
	return snapshot
}

func canonicalBenchmarkStatusMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	result := make(map[string]any, len(input))
	for name, value := range input {
		if name == "lastTransitionTime" {
			result[name] = "<runtime-transition-time>"
			continue
		}
		result[name] = canonicalBenchmarkStatusValue(value)
	}
	return result
}

func canonicalBenchmarkStatusValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		return canonicalBenchmarkStatusMap(value)
	case []any:
		result := make([]any, len(value))
		for index := range value {
			result[index] = canonicalBenchmarkStatusValue(value[index])
		}
		return result
	default:
		return value
	}
}

func bundledHapticActivationComponents(t *testing.T, cfg *config.Config) []string {
	t.Helper()
	const annotationPrefix = "metadata.annotations['haproxy-haptic.org/"
	components := make([]string, 0)
	for name, snippet := range cfg.TemplateSnippets {
		if snippet.Incremental == nil || len(snippet.Incremental.WhenAnyPathExists) == 0 {
			continue
		}
		for _, path := range snippet.Incremental.WhenAnyPathExists {
			if strings.HasPrefix(path, annotationPrefix) {
				components = append(components, name)
				break
			}
		}
	}
	slices.Sort(components)
	return components
}

func assertNoBundledHapticGatedExecutions(
	t *testing.T,
	counts map[string]uint64,
	components []string,
) {
	t.Helper()
	for _, component := range components {
		assert.Zero(t, counts[helpers.IncrementalEntryPointName(component)], component)
	}
}

func bundledHapticExecutionDelta(
	before map[string]uint64,
	after map[string]uint64,
	components []string,
) map[string]uint64 {
	delta := map[string]uint64{}
	for _, component := range components {
		entryPoint := helpers.IncrementalEntryPointName(component)
		if after[entryPoint] > before[entryPoint] {
			delta[component] = after[entryPoint] - before[entryPoint]
		}
	}
	return delta
}

func incrementalBenchmarkOutputBytes(result *renderer.RenderResult) (int, error) {
	if result == nil {
		return 0, errors.New("incremental benchmark render result is nil")
	}
	outputBytes := len(result.HAProxyConfig)
	if result.CycleSnapshot != nil {
		artifactBytes, err := incrementalBenchmarkArtifactBytes(result)
		if err != nil {
			return 0, err
		}
		return outputBytes + artifactBytes, nil
	}
	if result.AuxiliaryFiles == nil {
		return outputBytes, nil
	}
	for _, file := range result.AuxiliaryFiles.MapFiles {
		outputBytes += len(file.Content)
	}
	for _, file := range result.AuxiliaryFiles.GeneralFiles {
		outputBytes += len(file.Content)
	}
	for _, file := range result.AuxiliaryFiles.SSLCertificates {
		outputBytes += len(file.Content)
	}
	for _, file := range result.AuxiliaryFiles.SSLCaFiles {
		outputBytes += len(file.Content)
	}
	for _, file := range result.AuxiliaryFiles.CRTListFiles {
		outputBytes += len(file.Content)
	}
	return outputBytes, nil
}

func incrementalBenchmarkArtifactBytes(result *renderer.RenderResult) (int, error) {
	output, err := result.CycleSnapshot.OutputSnapshot()
	if err != nil {
		return 0, fmt.Errorf("reading benchmark output snapshot: %w", err)
	}
	artifacts, err := output.ArtifactSnapshot()
	if err != nil {
		return 0, fmt.Errorf("reading benchmark artifact snapshot: %w", err)
	}
	artifactBytes := 0
	err = artifacts.Walk(func(artifact *renderartifact.Artifact) error {
		content, contentErr := artifact.Content()
		if contentErr != nil {
			return contentErr
		}
		bytes, contentErr := content.Bytes()
		if contentErr != nil {
			return contentErr
		}
		artifactBytes += bytes
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("counting benchmark artifact bytes: %w", err)
	}
	return artifactBytes, nil
}

func reportIncrementalBenchmarkMetrics(
	b *testing.B,
	executions uint64,
	rawRenders uint64,
	outputBytes int,
) {
	b.Helper()
	b.ReportMetric(float64(executions)/float64(b.N), "component-exec/op")
	b.ReportMetric(float64(rawRenders)/float64(b.N), "raw-root-render/op")
	b.ReportMetric(float64(outputBytes), "output-B/op")
}

// reportIncrementalBenchmarkRootRenderCost separates the root document
// template's own time from the rest of a render. Component execution is flat
// across scale; this is the term that is not, so it is the one an optimisation
// has to move.
func reportIncrementalBenchmarkRootRenderCost(b *testing.B, elapsed time.Duration) {
	b.Helper()
	b.ReportMetric(float64(elapsed.Nanoseconds())/float64(b.N), "root-render-ns/op")
}

func incrementalBenchmarkSourceTransactionTopologySince(
	before incrementalBenchmarkSourceTransactionTopology,
	after incrementalBenchmarkSourceTransactionTopology,
) incrementalBenchmarkSourceTransactionTopology {
	return incrementalBenchmarkSourceTransactionTopology{
		rows:        after.rows - before.rows,
		children:    after.children - before.children,
		maxChildren: after.maxChildren,
	}
}

func reportIncrementalBenchmarkSourceTransactionMetrics(
	b *testing.B,
	topology incrementalBenchmarkSourceTransactionTopology,
) {
	b.Helper()
	b.ReportMetric(float64(topology.rows)/float64(b.N), "source-txn-row/op")
	b.ReportMetric(float64(topology.children)/float64(b.N), "source-txn-child/op")
	b.ReportMetric(float64(topology.maxChildren), "source-txn-max-child/row")
}
