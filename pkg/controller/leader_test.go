// Copyright 2025 Philipp Hossner
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

package controller

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	coreconfig "github.com/haproxy-haptic/HAPTIC/pkg/core/config"
	busevents "github.com/haproxy-haptic/HAPTIC/pkg/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/lifecycle"
)

type blockedLeaderComponent struct {
	ready   chan struct{}
	started chan struct{}
	release chan struct{}
}

func newBlockedLeaderComponent() *blockedLeaderComponent {
	return &blockedLeaderComponent{
		ready:   make(chan struct{}),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (*blockedLeaderComponent) Name() string {
	return "blocked-leader"
}

func (c *blockedLeaderComponent) SubscriptionReady() <-chan struct{} {
	return c.ready
}

func (c *blockedLeaderComponent) Start(ctx context.Context) error {
	close(c.started)
	<-ctx.Done()
	<-c.release
	return nil
}

// TestSuperviseElection covers the exit and re-entry shapes of the
// leader-election loop. The critical case is "lease lost without shutdown":
// client-go's LeaderElector.Run returns permanently after a missed lease
// renewal and never re-enters the acquire loop, so the supervisor re-enters
// it — in place, without failing the iteration (a reinitialization would
// retire the admission validators for the whole resync). A replica must
// never end up a follower with a dead elector (issue #57).
func TestSuperviseElection(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("lease lost re-enters election", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var calls atomic.Int32
		done := make(chan error, 1)
		go func() {
			done <- superviseElection(ctx, func(ctx context.Context) error {
				if calls.Add(1) >= 2 {
					<-ctx.Done() // re-acquired the follower position
				}
				return nil // the lost-lease shape: nil with a live context
			}, nil, logger)
		}()
		require.Eventually(t, func() bool { return calls.Load() >= 2 },
			5*time.Second, 10*time.Millisecond, "the supervisor must re-enter election")
		cancel()
		require.NoError(t, <-done)
	})

	t.Run("normal teardown is not an error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		// Elector returns because the iteration context was cancelled
		// (shutdown or config-change reinitialization).
		err := superviseElection(ctx, func(ctx context.Context) error {
			cancel()
			<-ctx.Done()
			return nil
		}, nil, logger)
		assert.NoError(t, err)
	})

	t.Run("elector error propagates", func(t *testing.T) {
		ctx := context.Background()
		electErr := errors.New("creating leader elector: boom")
		err := superviseElection(ctx, func(context.Context) error { return electErr }, nil, logger)
		require.Error(t, err)
		assert.ErrorIs(t, err, electErr)
	})

	t.Run("a stand-down releases the attempt and re-enters", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		restart := make(chan struct{}, 1)
		var calls atomic.Int32
		done := make(chan error, 1)
		go func() {
			done <- superviseElection(ctx, func(ctx context.Context) error {
				calls.Add(1)
				<-ctx.Done() // holding the lease until the attempt is cancelled
				return ctx.Err()
			}, restart, logger)
		}()
		require.Eventually(t, func() bool { return calls.Load() == 1 },
			time.Second, 10*time.Millisecond)
		restart <- struct{}{}
		require.Eventually(t, func() bool { return calls.Load() >= 2 },
			5*time.Second, 10*time.Millisecond, "the stand-down must re-enter election")
		cancel()
		require.NoError(t, <-done)
	})
}

// A lost lease ends the term — components stop and are joined — but the
// iteration survives, and a re-acquisition starts the same instances again.
func TestLeaderCallbacksSurviveLeaseLossAndRestartTheNextTerm(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := lifecycle.NewRegistry().WithLogger(logger)
	component := newRestartableLeaderComponent()
	registry.Register(component, true)
	iterCtx, cancelCause := context.WithCancelCause(t.Context())
	iterCancel := func() { cancelCause(nil) }
	defer iterCancel()
	group, _ := errgroup.WithContext(iterCtx)
	callbacks, _ := makeLeaderCallbacks(iterCtx, leaderCallbackDeps{
		registry:    registry,
		logger:      logger,
		cancelCause: cancelCause,
		errGroup:    group,
	})

	leaderCtx, loseLeadership := context.WithCancel(iterCtx)
	callbacks.OnStartedLeading(leaderCtx)
	require.Eventually(t, func() bool { return component.starts.Load() == 1 },
		time.Second, 5*time.Millisecond)
	loseLeadership()
	callbacks.OnStoppedLeading()
	require.Equal(t, int32(1), component.stops.Load(), "the term end joined the component")
	require.NoError(t, iterCtx.Err(), "losing the lease must not fail the iteration")

	secondTerm, endSecondTerm := context.WithCancel(iterCtx)
	callbacks.OnStartedLeading(secondTerm)
	require.Eventually(t, func() bool { return component.starts.Load() == 2 },
		time.Second, 5*time.Millisecond, "re-acquisition restarts the same instance")
	endSecondTerm()
	callbacks.OnStoppedLeading()
	require.NoError(t, group.Wait())
}

// The probes exempt a serving predecessor only while its term runs (ADR-0028).
func TestLeaderCallbacksReportTheRunningTerm(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := lifecycle.NewRegistry().WithLogger(logger)
	registry.Register(newRestartableLeaderComponent(), true)
	iterCtx, cancelCause := context.WithCancelCause(t.Context())
	defer cancelCause(nil)
	group, _ := errgroup.WithContext(iterCtx)
	callbacks, state := makeLeaderCallbacks(iterCtx, leaderCallbackDeps{
		registry:    registry,
		logger:      logger,
		cancelCause: cancelCause,
		errGroup:    group,
	})
	require.False(t, state.isLeading())

	term, endTerm := context.WithCancel(iterCtx)
	callbacks.OnStartedLeading(term)
	assert.True(t, state.isLeading())
	endTerm()
	callbacks.OnStoppedLeading()
	assert.False(t, state.isLeading(), "a lost lease ends the term")

	callbacks.OnStartedLeading(iterCtx)
	require.True(t, state.isLeading())
	state.retire()
	assert.False(t, state.isLeading(), "a term handed to the successor no longer counts")
	callbacks.OnStoppedLeading()
	require.NoError(t, group.Wait())
}

func TestLeaderCallbacksRejectDelayedStartAfterStop(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := lifecycle.NewRegistry().WithLogger(logger)
	component := newBlockedLeaderComponent()
	registry.Register(component, true)
	ctx, cancelCause := context.WithCancelCause(t.Context())
	cancel := func() { cancelCause(nil) }
	defer cancel()
	group, _ := errgroup.WithContext(ctx)
	callbacks, state := makeLeaderCallbacks(ctx, leaderCallbackDeps{
		registry:    registry,
		logger:      logger,
		cancelCause: cancelCause,
		errGroup:    group,
	})

	// A start callback firing late for an already-retired term carries that
	// term's cancelled context; it must not start anything.
	retired, endTerm := context.WithCancel(ctx)
	endTerm()
	callbacks.OnStoppedLeading()
	callbacks.OnStartedLeading(retired)
	select {
	case <-component.started:
		t.Fatal("delayed OnStartedLeading started a retired term")
	default:
	}

	// After the iteration teardown latch, no term starts at all.
	state.cancel()
	callbacks.OnStartedLeading(ctx)
	select {
	case <-component.started:
		t.Fatal("OnStartedLeading started a term after iteration teardown")
	default:
	}
	require.NoError(t, group.Wait())
}

func TestLeaderCallbacksPreserveEarlierIterationFailure(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	iterCtx, cancelCause := context.WithCancelCause(t.Context())
	group, _ := errgroup.WithContext(iterCtx)
	callbacks, _ := makeLeaderCallbacks(iterCtx, leaderCallbackDeps{
		registry:    lifecycle.NewRegistry().WithLogger(logger),
		logger:      logger,
		cancelCause: cancelCause,
		errGroup:    group,
	})
	failure := errors.New("required component failed")
	cancelCause(failure)

	callbacks.OnStoppedLeading()

	require.ErrorIs(t, context.Cause(iterCtx), failure)
	require.NoError(t, group.Wait())
}

func TestSuperviseElectionReentersAfterCallbackLeadershipLoss(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	iterCtx, cancelCause := context.WithCancelCause(t.Context())
	cancel := func() { cancelCause(nil) }
	defer cancel()
	group, _ := errgroup.WithContext(iterCtx)
	callbacks, _ := makeLeaderCallbacks(iterCtx, leaderCallbackDeps{
		registry:    lifecycle.NewRegistry().WithLogger(logger),
		logger:      logger,
		cancelCause: cancelCause,
		errGroup:    group,
	})

	var calls atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- superviseElection(iterCtx, func(ctx context.Context) error {
			if calls.Add(1) >= 2 {
				<-ctx.Done()
			} else {
				callbacks.OnStoppedLeading()
			}
			return nil
		}, nil, logger)
	}()

	require.Eventually(t, func() bool { return calls.Load() >= 2 },
		5*time.Second, 10*time.Millisecond, "a callback-reported loss must re-enter election")
	require.NoError(t, iterCtx.Err(), "losing the lease must not fail the iteration")
	cancel()
	require.NoError(t, <-done)
	require.NoError(t, group.Wait())
}

func TestStandaloneCanceledHandoffDoesNotResumeEventBus(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bus := busevents.NewEventBus(2)
	received := bus.Subscribe("receiver", 2)
	bus.Start()
	iterCtx, cancelCause := context.WithCancelCause(t.Context())
	cancel := func() { cancelCause(nil) }
	group, groupCtx := errgroup.WithContext(iterCtx)
	setup := &componentSetup{
		Bus:         bus,
		Registry:    lifecycle.NewRegistry().WithLogger(logger),
		IterCtx:     groupCtx,
		Cancel:      cancel,
		CancelCause: cancelCause,
		ErrGroup:    group,
	}
	failure := errors.New("required component failed")
	cancelCause(failure)

	_, err := setupLeaderElection(setup, &coreconfig.Config{}, nil, logger)

	require.ErrorIs(t, err, failure)
	bus.Publish(deliveryTestEvent{timestamp: time.Now()})
	select {
	case event := <-received:
		t.Fatalf("delivered %T after canceled standalone handoff", event)
	default:
	}
	require.NoError(t, group.Wait())
}

func TestTeardownCancelsLeaderStartupBeforeWaiting(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := lifecycle.NewRegistry().WithLogger(logger)
	component := newBlockedLeaderComponent()
	registry.Register(component, true)
	iterCtx, cancelCause := context.WithCancelCause(t.Context())
	cancel := func() { cancelCause(nil) }
	group, groupCtx := errgroup.WithContext(iterCtx)
	callbacks, state := makeLeaderCallbacks(iterCtx, leaderCallbackDeps{
		registry:    registry,
		logger:      logger,
		cancelCause: cancelCause,
		errGroup:    group,
	})
	setup := &componentSetup{
		IterCtx:     groupCtx,
		Cancel:      cancel,
		CancelCause: cancelCause,
		ErrGroup:    group,
		LeaderState: state,
	}

	startedCallbackDone := make(chan struct{})
	go func() {
		callbacks.OnStartedLeading(groupCtx)
		close(startedCallbackDone)
	}()
	<-component.started
	teardownDone := make(chan error, 1)
	go func() {
		teardownDone <- teardownIteration(setup, logger)
	}()
	select {
	case <-startedCallbackDone:
	case <-time.After(time.Second):
		t.Fatal("iteration teardown did not cancel leader startup")
	}
	select {
	case <-teardownDone:
		t.Fatal("iteration teardown passed an active leader component")
	default:
	}

	close(component.release)
	require.NoError(t, <-teardownDone)
}

var _ lifecycle.SubscriptionReadySignaler = (*blockedLeaderComponent)(nil)

// restartableLeaderComponent counts terms: instantly ready, runs until the
// term context ends, and can be started again — the shape every leader-only
// component has under in-place re-election.
type restartableLeaderComponent struct {
	ready  chan struct{}
	starts atomic.Int32
	stops  atomic.Int32
}

func newRestartableLeaderComponent() *restartableLeaderComponent {
	c := &restartableLeaderComponent{ready: make(chan struct{})}
	close(c.ready)
	return c
}

func (*restartableLeaderComponent) Name() string { return "restartable-leader" }

func (c *restartableLeaderComponent) SubscriptionReady() <-chan struct{} { return c.ready }

func (c *restartableLeaderComponent) Start(ctx context.Context) error {
	c.starts.Add(1)
	<-ctx.Done()
	c.stops.Add(1)
	return nil
}

func TestLeaderCallbacksLogExpectedShutdownAtInfo(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cause    error
		retiring bool
		want     string
	}{
		{name: "shutdown", cause: context.Canceled, want: "level=INFO"},
		{name: "handover", retiring: true, want: "level=INFO"},
		{name: "lease loss", want: "level=WARN"},
		{name: "failed iteration", cause: errors.New("iteration failed"), want: "level=WARN"},
		{name: "deadline", cause: context.DeadlineExceeded, want: "level=WARN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			var output bytes.Buffer
			callbacks, state := makeLeaderCallbacks(ctx, leaderCallbackDeps{
				logger: slog.New(slog.NewTextHandler(&output, nil)),
			})
			if tc.retiring {
				state.retire()
			}
			if tc.cause != nil {
				cancel(tc.cause)
			}
			callbacks.OnStoppedLeading()
			assert.Contains(t, output.String(), tc.want)
		})
	}
}

func TestComponentFailurePreservesLeadershipStopCause(t *testing.T) {
	for _, leaderOnly := range []bool{false, true} {
		name := "all replicas"
		if leaderOnly {
			name = "leader only"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancelCause := context.WithCancelCause(t.Context())
			defer cancelCause(nil)
			group, groupCtx := errgroup.WithContext(ctx)
			var output bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&output, nil))
			failure := errors.New("component failed")
			registry := lifecycle.NewRegistry().WithLogger(logger)
			registry.Register(&failedLifecycleComponent{failure: failure}, leaderOnly)
			callbacks, _ := makeLeaderCallbacks(groupCtx, leaderCallbackDeps{
				registry: registry, logger: logger, cancelCause: cancelCause, errGroup: group,
			})
			if leaderOnly {
				callbacks.OnStartedLeading(groupCtx)
			} else {
				startReconciliationComponents(groupCtx, registry, logger, cancelCause, group)
			}
			require.ErrorIs(t, group.Wait(), failure)
			require.ErrorIs(t, context.Cause(ctx), failure)
			callbacks.OnStoppedLeading()
			assert.Contains(t, output.String(), "level=WARN msg=\"Lost leadership, stopping deployment components\"")
		})
	}
}

type failedLifecycleComponent struct {
	failure error
}

func (*failedLifecycleComponent) Name() string { return "failed-component" }

func (c *failedLifecycleComponent) Start(context.Context) error { return c.failure }

// A superseded iteration never starts a term: it would deploy the previous
// config. A term already running keeps serving until the successor takes over.
func TestLeaderCallbacksSupersededIterationStartsNoTerm(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := lifecycle.NewRegistry().WithLogger(logger)
	component := newRestartableLeaderComponent()
	registry.Register(component, true)
	iterCtx, cancelCause := context.WithCancelCause(t.Context())
	defer cancelCause(nil)
	group, _ := errgroup.WithContext(iterCtx)
	var left atomic.Int32
	callbacks, state := makeLeaderCallbacks(iterCtx, leaderCallbackDeps{
		registry:      registry,
		logger:        logger,
		cancelCause:   cancelCause,
		errGroup:      group,
		leaveElection: func() { left.Add(1) },
	})

	require.False(t, state.supersede(), "a follower runs no term")
	callbacks.OnStartedLeading(iterCtx)
	assert.False(t, state.isLeading())
	assert.Zero(t, component.starts.Load())
	assert.Equal(t, int32(1), left.Load(), "a won Lease is released")
	require.NoError(t, group.Wait())
}

func TestLeaderCallbacksSupersedeKeepsTheRunningTerm(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := lifecycle.NewRegistry().WithLogger(logger)
	registry.Register(newRestartableLeaderComponent(), true)
	iterCtx, cancelCause := context.WithCancelCause(t.Context())
	defer cancelCause(nil)
	group, _ := errgroup.WithContext(iterCtx)
	callbacks, state := makeLeaderCallbacks(iterCtx, leaderCallbackDeps{
		registry:      registry,
		logger:        logger,
		cancelCause:   cancelCause,
		errGroup:      group,
		leaveElection: func() { t.Error("a running term must not leave the election") },
	})

	callbacks.OnStartedLeading(iterCtx)
	require.True(t, state.supersede())
	assert.True(t, state.isLeading())
	state.retire()
	callbacks.OnStoppedLeading()
	require.NoError(t, group.Wait())
}
