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

package warmer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/inputisolation"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/metrics"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/pipeline"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
)

type recordingPipeline struct {
	mu      sync.Mutex
	calls   []rendercontext.RenderMode
	options []int
	result  *pipeline.PipelineResult
	err     error
	// gate, when set, holds every Execute until released so a burst of
	// triggers can pile up behind one render; entered reports each arrival.
	gate    chan struct{}
	entered chan struct{}
	ran     chan struct{}
}

func (p *recordingPipeline) Execute(
	_ context.Context, _ stores.StoreProvider, mode rendercontext.RenderMode, opts ...rendercontext.Option,
) (*pipeline.PipelineResult, error) {
	if p.entered != nil {
		p.entered <- struct{}{}
	}
	if p.gate != nil {
		<-p.gate
	}
	p.mu.Lock()
	p.calls = append(p.calls, mode)
	p.options = append(p.options, len(opts))
	p.mu.Unlock()
	if p.ran != nil {
		p.ran <- struct{}{}
	}
	return p.result, p.err
}

func (p *recordingPipeline) executions() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

type harness struct {
	bus       *busevents.EventBus
	pipeline  *recordingPipeline
	metrics   *metrics.Metrics
	published <-chan busevents.Event
	files     func() (rendercontext.CurrentAuxFilesSource, error)
	graphWarm atomic.Bool
	component *Component
}

func newHarness(t *testing.T, p *recordingPipeline) *harness {
	t.Helper()
	bus, logger := testutil.NewTestBusAndLogger()
	h := &harness{
		bus:      bus,
		pipeline: p,
		metrics:  metrics.NewMetrics(prometheus.NewRegistry()),
		files:    func() (rendercontext.CurrentAuxFilesSource, error) { return newTestCurrentFilesSource(), nil },
	}
	h.published = bus.SubscribeTypes("test", 100,
		events.EventTypeReconciliationStarted,
		events.EventTypeTemplateRendered,
		events.EventTypeReconciliationCompleted,
		events.EventTypeReconciliationFailed,
	)
	component := New(&Config{
		EventBus:           bus,
		Pipeline:           p,
		StoreProvider:      stores.NewRealStoreProvider(nil),
		CurrentFilesSource: func() (rendercontext.CurrentAuxFilesSource, error) { return h.files() },
		GraphWarm:          h.graphWarm.Load,
		Metrics:            h.metrics,
		Logger:             logger,
	})
	h.component = component
	h.graphWarm.Store(true)
	bus.Start()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = component.Start(ctx) }()
	time.Sleep(testutil.StartupDelay)
	return h
}

func (h *harness) waitForRender(t *testing.T) {
	t.Helper()
	select {
	case <-h.pipeline.ran:
	case <-time.After(testutil.EventTimeout):
		t.Fatal("timed out waiting for the warmer to render")
	}
}

func (h *harness) assertNoRender(t *testing.T) {
	t.Helper()
	select {
	case <-h.pipeline.ran:
		t.Fatal("the warmer rendered while it must not")
	case <-time.After(testutil.NoEventTimeout):
	}
}

func warmResult() *pipeline.PipelineResult {
	return &pipeline.PipelineResult{RenderDurationMs: 3, CacheState: "warm", CacheBuildMs: 2}
}

func TestWarmerRendersOnFollowerAndPublishesNothing(t *testing.T) {
	p := &recordingPipeline{result: warmResult(), ran: make(chan struct{}, 8)}
	h := newHarness(t, p)

	h.bus.Publish(events.NewReconciliationTriggeredEvent("resource_change", true))
	h.waitForRender(t)

	p.mu.Lock()
	require.Equal(t, []rendercontext.RenderMode{rendercontext.RenderModeReconcile}, p.calls)
	require.Equal(t, []int{1}, p.options, "the render carries the published currentFiles")
	p.mu.Unlock()
	testutil.AssertNoEvent[busevents.Event](t, h.published, testutil.NoEventTimeout)
	assert.Equal(t, 1.0, promtestutil.ToFloat64(h.metrics.RenderTotal.WithLabelValues("warm")))
}

func TestWarmerCoalescesABurstOfTriggers(t *testing.T) {
	p := &recordingPipeline{
		result: warmResult(), ran: make(chan struct{}, 8), gate: make(chan struct{}), entered: make(chan struct{}, 8),
	}
	h := newHarness(t, p)

	h.bus.Publish(events.NewReconciliationTriggeredEvent("resource_change", true))
	select {
	case <-p.entered:
	case <-time.After(testutil.EventTimeout):
		t.Fatal("the first render never started")
	}
	for range 5 {
		h.bus.Publish(events.NewReconciliationTriggeredEvent("resource_change", true))
	}
	time.Sleep(testutil.StartupDelay)
	close(p.gate)
	h.waitForRender(t)
	h.waitForRender(t)
	h.assertNoRender(t)
	assert.Equal(t, 2, p.executions(), "one render for the first trigger, one for the coalesced rest")
}

func TestWarmerStandsDownWhileLeader(t *testing.T) {
	p := &recordingPipeline{result: warmResult(), ran: make(chan struct{}, 8)}
	h := newHarness(t, p)

	h.bus.Publish(events.NewBecameLeaderEvent("this-replica"))
	h.bus.Publish(events.NewReconciliationTriggeredEvent("became_leader", false))
	h.bus.Publish(events.NewReconciliationTriggeredEvent("resource_change", true))
	h.assertNoRender(t)

	h.bus.Publish(events.NewLostLeadershipEvent("this-replica", "lease_lost"))
	h.bus.Publish(events.NewReconciliationTriggeredEvent("resource_change", true))
	h.waitForRender(t)
	assert.Equal(t, 1, p.executions())
}

func TestWarmerSkipsWhenCurrentFilesAreUnavailable(t *testing.T) {
	p := &recordingPipeline{result: warmResult(), ran: make(chan struct{}, 8)}
	h := newHarness(t, p)
	h.files = func() (rendercontext.CurrentAuxFilesSource, error) {
		return nil, errors.New("published set is ambiguous")
	}

	h.bus.Publish(events.NewReconciliationTriggeredEvent("resource_change", true))
	h.assertNoRender(t)
	testutil.AssertNoEvent[busevents.Event](t, h.published, testutil.NoEventTimeout)
}

func TestWarmerSurvivesAFailedRender(t *testing.T) {
	p := &recordingPipeline{err: errors.New("render exploded"), ran: make(chan struct{}, 8)}
	h := newHarness(t, p)

	h.bus.Publish(events.NewReconciliationTriggeredEvent("resource_change", true))
	h.waitForRender(t)
	h.bus.Publish(events.NewReconciliationTriggeredEvent("resource_change", true))
	h.waitForRender(t)
	testutil.AssertNoEvent[busevents.Event](t, h.published, testutil.NoEventTimeout)
	assert.Equal(t, 2.0, promtestutil.ToFloat64(h.metrics.RenderTotal.WithLabelValues("warm")))
	assert.Equal(t, 2.0, promtestutil.ToFloat64(h.metrics.RenderErrors.WithLabelValues("warm")))
}

func TestWarmerCountsFailedRendersByTheGraphTheyRanFrom(t *testing.T) {
	p := &recordingPipeline{err: errors.New("render exploded"), ran: make(chan struct{}, 8)}
	h := newHarness(t, p)
	h.graphWarm.Store(false)

	h.bus.Publish(events.NewReconciliationTriggeredEvent("resource_change", true))
	h.waitForRender(t)
	require.Eventually(t, func() bool {
		return promtestutil.ToFloat64(h.metrics.RenderErrors.WithLabelValues("cold")) == 1
	}, testutil.EventTimeout, 5*time.Millisecond)
	assert.Equal(t, 1.0, promtestutil.ToFloat64(h.metrics.RenderTotal.WithLabelValues("cold")))
}

func TestWarmerDoesNotCountAHeldRender(t *testing.T) {
	p := &recordingPipeline{err: fmt.Errorf("%w: source down", pipeline.ErrRenderHeld), ran: make(chan struct{}, 8)}
	h := newHarness(t, p)

	h.bus.Publish(events.NewReconciliationTriggeredEvent("resource_change", true))
	h.waitForRender(t)
	h.bus.Publish(events.NewReconciliationTriggeredEvent("resource_change", true))
	h.waitForRender(t)
	testutil.AssertNoEvent[busevents.Event](t, h.published, testutil.NoEventTimeout)
	for _, cacheState := range []string{"cold", "warm", "replay"} {
		assert.Zero(t, promtestutil.ToFloat64(h.metrics.RenderTotal.WithLabelValues(cacheState)), cacheState)
		assert.Zero(t, promtestutil.ToFloat64(h.metrics.RenderErrors.WithLabelValues(cacheState)), cacheState)
	}
}

// The production sequence on the replica that wins the first election: the
// warmer is inside its cold first render when BecameLeaderEvent and the
// leader's triggers arrive behind it. Whatever it dequeues afterwards must
// not render, or it races the Coordinator's first cold render for the graph.
func TestWarmerStandsDownWhenLeadershipArrivesMidRender(t *testing.T) {
	p := &recordingPipeline{
		result: warmResult(), ran: make(chan struct{}, 8), gate: make(chan struct{}), entered: make(chan struct{}, 8),
	}
	h := newHarness(t, p)

	h.bus.Publish(events.NewReconciliationTriggeredEvent("initial_sync_complete", true))
	select {
	case <-p.entered:
	case <-time.After(testutil.EventTimeout):
		t.Fatal("the first render never started")
	}
	h.bus.Pause()
	h.bus.Publish(events.NewBecameLeaderEvent("this-replica"))
	h.bus.Start()
	h.bus.Publish(events.NewReconciliationTriggeredEvent("became_leader", false))
	for range 4 {
		h.bus.Publish(events.NewReconciliationTriggeredEvent("fleet_capabilities_changed", true))
	}
	time.Sleep(testutil.StartupDelay)
	close(p.gate)
	h.waitForRender(t)
	h.assertNoRender(t)
	assert.Equal(t, 1, p.executions(), "only the render that was already in flight")
}

type cancelablePipeline struct {
	started chan context.Context
}

func (*cancelablePipeline) HeldRender(rendercontext.RenderMode) error { return nil }

func (p *cancelablePipeline) Execute(ctx context.Context, _ stores.StoreProvider, _ rendercontext.RenderMode, _ ...rendercontext.Option) (*pipeline.PipelineResult, error) {
	p.started <- ctx
	<-ctx.Done()
	return warmResult(), ctx.Err()
}

func TestWarmerCancelsInFlightRenderOnPromotionAndShutdown(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(map[bool]string{false: "context", true: "stop"}[stop], func(t *testing.T) {
			testWarmerCancellation(t, stop)
		})
	}
}

func testWarmerCancellation(t *testing.T, stop bool) {
	t.Helper()
	bus, logger := testutil.NewTestBusAndLogger()
	p := &cancelablePipeline{started: make(chan context.Context, 4)}
	selector := inputisolation.New(p, nil, logger)
	provider := stores.NewRealStoreProvider(nil)
	c := New(&Config{
		EventBus: bus, Pipeline: selector, StoreProvider: provider, Logger: logger,
		CurrentFilesSource: func() (rendercontext.CurrentAuxFilesSource, error) {
			return newTestCurrentFilesSource(), nil
		},
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = c.Start(ctx)
	}()
	t.Cleanup(func() { cancel(); <-stopped })
	bus.Start()
	bus.Publish(events.NewReconciliationTriggeredEvent("initial_sync_complete", true))
	follower := waitForPipelineContext(t, p.started)
	bus.Publish(events.NewBecameLeaderEvent("this-replica"))
	select {
	case <-follower.Done():
	case <-time.After(testutil.EventTimeout):
		t.Fatal("promotion did not cancel the in-flight follower render")
	}

	leaderCtx, cancelLeader := context.WithCancel(ctx)
	leaderDone := make(chan error, 1)
	go func() {
		_, err := selector.Execute(leaderCtx, provider, rendercontext.RenderModeReconcile)
		leaderDone <- err
	}()
	waitForPipelineContext(t, p.started)
	cancelLeader()
	require.ErrorIs(t, <-leaderDone, context.Canceled)
	select {
	case <-c.Warmed():
		t.Fatal("canceled render reported a warm graph")
	default:
	}

	bus.Publish(events.NewLostLeadershipEvent("this-replica", "lease_lost"))
	bus.Publish(events.NewReconciliationTriggeredEvent("resource_change", true))
	nextFollower := waitForPipelineContext(t, p.started)
	require.NoError(t, nextFollower.Err())
	if stop {
		c.Stop()
	} else {
		cancel()
	}
	select {
	case <-stopped:
	case <-time.After(testutil.EventTimeout):
		t.Fatal("shutdown did not drain the render worker")
	}
	require.ErrorIs(t, nextFollower.Err(), context.Canceled)
}

func waitForPipelineContext(t *testing.T, started <-chan context.Context) context.Context {
	t.Helper()
	select {
	case ctx := <-started:
		return ctx
	case <-time.After(testutil.EventTimeout):
		t.Fatal("pipeline did not start")
		return nil
	}
}

// A hand-over waits for a committed graph. A render whose cache the next
// input change discarded leaves none, so it must not report the graph warm;
// the render after it does.
func TestWarmerReportsWarmOnlyOnceARenderLeftAGraph(t *testing.T) {
	p := &recordingPipeline{result: warmResult(), ran: make(chan struct{}, 1)}
	h := newHarness(t, p)
	h.graphWarm.Store(false)

	h.bus.Publish(events.NewReconciliationTriggeredEvent("resource_change", true))
	h.waitForRender(t)
	select {
	case <-h.component.Warmed():
		t.Fatal("the graph was reported warm after a render that left no cache")
	case <-time.After(testutil.NoEventTimeout):
	}

	h.graphWarm.Store(true)
	h.bus.Publish(events.NewReconciliationTriggeredEvent("resource_change", true))
	h.waitForRender(t)
	select {
	case <-h.component.Warmed():
	case <-time.After(testutil.EventTimeout):
		t.Fatal("the graph was not reported warm after a render left a cache")
	}
}

// testCurrentFilesSource is a minimal root-tracked source for the harness.
type testCurrentFilesSource struct{}

func newTestCurrentFilesSource() rendercontext.CurrentAuxFilesSource { return testCurrentFilesSource{} }

func (testCurrentFilesSource) ValidateAuthentication() error { return nil }

func (testCurrentFilesSource) SameRoot(other rendercontext.CurrentAuxFilesSource) (bool, error) {
	_, ok := other.(testCurrentFilesSource)
	return ok, nil
}

func (testCurrentFilesSource) MaterializeCurrentAuxFiles() (map[string]string, error) {
	return map[string]string{"maps/a.map": "x"}, nil
}
