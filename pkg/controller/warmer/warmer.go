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

// Package warmer keeps a follower's incremental render graph warm.
//
// Only the leader's Coordinator renders to deploy, so without this a follower's
// graph sits at generation zero and every leadership change starts with a cold
// render. The warmer runs the same reconcile render on every trigger a follower
// receives and commits it, which is what publishes the graph, then discards the
// output: nothing it renders reaches the fleet, the API server, or any other
// component.
package warmer

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/component"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/metrics"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/pipeline"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/stores"
)

const (
	// ComponentName is the unique identifier for this component.
	ComponentName = "render-warmer"

	// EventBufferSize matches the Coordinator's: the trigger stream is one
	// event per resource change, and the mailbox collapses it to one render.
	EventBufferSize = busevents.ResourceChurnSubscriberBuffer
)

// PipelineExecutor renders and commits; the warmer never reads the output.
type PipelineExecutor interface {
	Execute(ctx context.Context, provider stores.StoreProvider, mode rendercontext.RenderMode, extraOpts ...rendercontext.Option) (*pipeline.PipelineResult, error)
}

// Config wires a Component.
type Config struct {
	EventBus      *busevents.EventBus
	Pipeline      PipelineExecutor
	StoreProvider stores.StoreProvider
	// CurrentFilesSource returns a root-tracked view of the auxiliary files
	// the fleet runs, the same set a new leader's first render reads. A
	// root-tracked source — rather than a map snapshot — lets consecutive
	// renders prove the currentFiles input unchanged, which the exact-cycle
	// replay requires; a follower rendering from plain maps misses the replay
	// on every render and pays a full render each time.
	CurrentFilesSource func() (rendercontext.CurrentAuxFilesSource, error)
	// GraphWarm reports whether the render service holds a committed graph;
	// Warmed closes after the first render that leaves one. Nil means every
	// completed render counts.
	GraphWarm func() bool
	Metrics   *metrics.Metrics
	Logger    *slog.Logger
}

// Component renders on a follower and cancels its work when leadership changes.
type Component struct {
	*component.Base

	pipeline       PipelineExecutor
	storeProvider  stores.StoreProvider
	currentFiles   func() (rendercontext.CurrentAuxFilesSource, error)
	graphWarm      func() bool
	metrics        *metrics.Metrics
	leader         bool
	warmed         chan struct{}
	warmedOnce     sync.Once
	followerCtx    context.Context
	cancelFollower context.CancelFunc
	requests       chan context.Context
	inFlightMu     sync.Mutex
	inFlight       chan struct{}
	// failing is whether the last follower render failed; only transitions log above Debug.
	failing bool
}

// Warmed is closed once a render left a committed graph behind, which is when
// a hand-over to this iteration finds a warm graph. A render whose cache the
// next input change discarded does not count; that change's own trigger
// renders again.
func (c *Component) Warmed() <-chan struct{} {
	return c.warmed
}

// RenderInFlight returns a channel closed when the follower render running now
// ends, or nil when none is running.
func (c *Component) RenderInFlight() <-chan struct{} {
	c.inFlightMu.Lock()
	defer c.inFlightMu.Unlock()
	return c.inFlight
}

// New subscribes the component; call before the bus starts.
func New(cfg *Config) *Component {
	c := &Component{
		pipeline:      cfg.Pipeline,
		storeProvider: cfg.StoreProvider,
		currentFiles:  cfg.CurrentFilesSource,
		graphWarm:     cfg.GraphWarm,
		metrics:       cfg.Metrics,
		warmed:        make(chan struct{}),
		requests:      make(chan context.Context, 1),
	}
	c.Base = component.New(&component.Config{
		EventBus:   cfg.EventBus,
		Logger:     cfg.Logger,
		Name:       ComponentName,
		BufferSize: EventBufferSize,
		Handler:    c,
		EventTypes: []string{
			events.EventTypeReconciliationTriggered,
			events.EventTypeBecameLeader,
			events.EventTypeLostLeadership,
		},
	})
	return c
}

// Start drains leadership events independently of the single render worker.
func (c *Component) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.followerCtx, c.cancelFollower = context.WithCancel(ctx)
	defer func() { c.cancelFollower() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.runRenders(ctx)
	}()
	err := c.Base.Start(ctx)
	cancel()
	<-done
	return err
}

func (c *Component) runRenders(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case renderCtx := <-c.requests:
			if renderCtx.Err() == nil {
				c.renderTracked(renderCtx)
			}
		}
	}
}

func (c *Component) renderTracked(ctx context.Context) {
	done := make(chan struct{})
	c.inFlightMu.Lock()
	c.inFlight = done
	c.inFlightMu.Unlock()
	defer func() {
		c.inFlightMu.Lock()
		c.inFlight = nil
		c.inFlightMu.Unlock()
		close(done)
	}()
	component.SafeDispatch(c.Logger(), ComponentName, nil, func() { c.render(ctx) })
}

// CoalescesOn collapses a burst of triggers to the latest one: every render
// re-reads the stores, so the newest trigger is all a follower needs.
func (c *Component) CoalescesOn() []string {
	return []string{events.EventTypeReconciliationTriggered}
}

// CoalescesAcrossQueue keeps one trigger queued whatever sits between them: a
// leadership event does not make an older trigger worth a render of its own.
func (c *Component) CoalescesAcrossQueue() bool { return true }

// HandleEvent implements component.EventHandler.
func (c *Component) HandleEvent(event busevents.Event) {
	switch event.(type) {
	case *events.BecameLeaderEvent:
		c.leader = true
		c.cancelFollower()
		c.Logger().Debug("Follower renders stopped: this replica leads")
	case *events.LostLeadershipEvent:
		c.leader = false
		c.cancelFollower()
		c.followerCtx, c.cancelFollower = context.WithCancel(c.LifecycleContext())
		c.Logger().Debug("Follower renders resumed: this replica follows")
	case *events.ReconciliationTriggeredEvent:
		if !c.leader {
			select {
			case <-c.requests:
			default:
			}
			c.requests <- c.followerCtx
		}
	}
}

func (c *Component) render(ctx context.Context) {
	source, err := c.currentFiles()
	if err != nil {
		c.Logger().Debug("Follower render skipped", "reason", err)
		return
	}
	result, err := c.pipeline.Execute(
		ctx, c.storeProvider, rendercontext.RenderModeReconcile, rendercontext.WithCurrentAuxFilesSource(source),
	)
	if ctx.Err() != nil {
		return
	}
	if pipeline.RenderHeld(err) {
		c.Logger().Debug("Follower render skipped while rendering is paused", "error", err)
		return
	}
	if err != nil {
		c.renderFailed(err)
		return
	}
	if c.failing {
		c.failing = false
		c.Logger().Info("Follower renders succeed again")
	}
	if c.metrics != nil {
		c.metrics.RecordRender(result.CacheState)
	}
	if c.graphWarm == nil || c.graphWarm() {
		c.warmedOnce.Do(func() { close(c.warmed) })
	}
	c.Logger().Debug("Follower render completed",
		"render_ms", result.RenderDurationMs,
		"cache_state", result.CacheState,
		"cache_build_ms", result.CacheBuildMs)
}

func (c *Component) renderFailed(err error) {
	if c.metrics != nil {
		c.metrics.RecordRenderFailure(c.graphWarm == nil || c.graphWarm())
	}
	if c.failing || pipeline.InputsMoved(err) || errors.Is(err, stores.ErrSnapshotChanged) {
		c.Logger().Debug("Follower render failed", "error", err)
		return
	}
	c.failing = true
	c.Logger().Warn("Follower render failed, so a leadership change on this replica starts cold. Fix the error it reports.",
		"error", err)
}
