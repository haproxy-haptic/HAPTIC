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

package server

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func drainTestServer(quiet, bound time.Duration, counter func(map[string]bool) (uint64, error)) *Server {
	return &Server{
		cfg:          Config{DrainQuietPeriod: quiet, DrainMaxWait: bound, DrainIgnoreFrontends: []string{"status"}},
		logger:       slog.New(slog.DiscardHandler),
		drainCounter: counter,
		drainPoll:    2 * time.Millisecond,
	}
}

func TestDrainEndsAfterTheQuietPeriod(t *testing.T) {
	s := drainTestServer(30*time.Millisecond, time.Second, func(map[string]bool) (uint64, error) { return 42, nil })
	start := time.Now()
	result := s.drain(nil)
	assert.Equal(t, DrainReasonQuiet, result.Reason)
	assert.Equal(t, uint64(42), result.Connections)
	assert.GreaterOrEqual(t, time.Since(start), 30*time.Millisecond)
	assert.Less(t, time.Since(start), 500*time.Millisecond)
}

func TestDrainRestartsTheQuietPeriodOnEveryNewConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var connections atomic.Uint64
		s := drainTestServer(40*time.Millisecond, time.Second, func(map[string]bool) (uint64, error) {
			return connections.Load(), nil
		})
		result := make(chan DrainResult, 1)
		go func() { result <- s.drain(nil) }()
		synctest.Wait()
		for range 5 {
			time.Sleep(30 * time.Millisecond)
			connections.Add(1)
			synctest.Wait()
			select {
			case <-result:
				t.Fatal("drain finished while new connections were arriving")
			default:
			}
		}
		lastConnection := time.Now()
		drained := <-result
		assert.Equal(t, DrainReasonQuiet, drained.Reason)
		assert.Equal(t, connections.Load(), drained.Connections)
		assert.GreaterOrEqual(t, time.Since(lastConnection), s.cfg.DrainQuietPeriod)
	})
}

func TestDrainMeasuresQuietTimeAfterReadingConnections(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		var observed time.Time
		s := drainTestServer(40*time.Millisecond, time.Second, func(map[string]bool) (uint64, error) {
			calls++
			if calls == 1 {
				return 0, nil
			}
			if calls == 2 {
				time.Sleep(100 * time.Millisecond)
				observed = time.Now()
			}
			return 1, nil
		})
		result := s.drain(nil)
		assert.Equal(t, DrainReasonQuiet, result.Reason)
		assert.GreaterOrEqual(t, time.Since(observed), s.cfg.DrainQuietPeriod)
	})
}

func TestDrainStopsAtTheBoundUnderConstantTraffic(t *testing.T) {
	var connections atomic.Uint64
	s := drainTestServer(50*time.Millisecond, 120*time.Millisecond, func(map[string]bool) (uint64, error) {
		return connections.Add(1), nil
	})
	start := time.Now()
	result := s.drain(nil)
	assert.Equal(t, DrainReasonBound, result.Reason)
	assert.GreaterOrEqual(t, time.Since(start), 120*time.Millisecond)
	assert.Less(t, time.Since(start), time.Second)
}

func TestDrainReturnsAtOnceWhenTheWorkerIsGone(t *testing.T) {
	s := drainTestServer(time.Second, 5*time.Second, func(map[string]bool) (uint64, error) {
		return 0, errors.New("socket closed")
	})
	start := time.Now()
	result := s.drain(nil)
	assert.Equal(t, DrainReasonUnavailable, result.Reason)
	assert.Less(t, time.Since(start), 200*time.Millisecond)
}

func TestDrainEndsWhenTheAgentStops(t *testing.T) {
	s := drainTestServer(time.Second, 5*time.Second, func(map[string]bool) (uint64, error) { return 1, nil })
	stop := make(chan struct{})
	go func() {
		time.Sleep(30 * time.Millisecond)
		close(stop)
	}()
	result := s.drain(stop)
	assert.Equal(t, DrainReasonCancelled, result.Reason)
}

// routingScript reports each step's state once its delay since the previous
// step elapsed, then holds until the drain stops watching.
type routingScript struct {
	steps   []routingStep
	stopped chan struct{}
}

type routingStep struct {
	after   time.Duration
	routing Routing
}

func (r *routingScript) Watch(ctx context.Context, report func(Routing)) {
	defer close(r.stopped)
	for _, step := range r.steps {
		select {
		case <-time.After(step.after):
			report(step.routing)
		case <-ctx.Done():
			return
		}
	}
	<-ctx.Done()
}

func drainWithRouting(quiet, bound time.Duration, connections func() uint64, steps ...routingStep) (*Server, *routingScript) {
	script := &routingScript{steps: steps, stopped: make(chan struct{})}
	s := drainTestServer(quiet, bound, func(map[string]bool) (uint64, error) { return connections(), nil })
	s.cfg.DrainRouting = script
	return s, script
}

func idle() uint64 { return 7 }

func TestDrainHoldsAQuietPodWhileItIsStillRouted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, script := drainWithRouting(20*time.Millisecond, 500*time.Millisecond, idle,
			routingStep{routing: Routing{Routed: true, Detail: "kube-proxy on node-a has not synced"}})
		start := time.Now()
		result := s.drain(nil)
		assert.Equal(t, DrainReasonBound, result.Reason)
		assert.Equal(t, "kube-proxy on node-a has not synced", result.Routing)
		assert.GreaterOrEqual(t, time.Since(start), 500*time.Millisecond)
		<-script.stopped
	})
}

func TestDrainEndsOnceTheRoutingIsWithdrawnAndThePodIsQuiet(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := drainWithRouting(20*time.Millisecond, 5*time.Second, idle,
			routingStep{routing: Routing{Routed: true}},
			routingStep{after: 300 * time.Millisecond, routing: Routing{}})
		start := time.Now()
		result := s.drain(nil)
		assert.Equal(t, DrainReasonRoutedAway, result.Reason)
		assert.GreaterOrEqual(t, time.Since(start), 300*time.Millisecond)
		assert.Less(t, time.Since(start), 500*time.Millisecond)
	})
}

func TestDrainStillWaitsForTheQuietPeriodAfterTheRoutingIsWithdrawn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var connections atomic.Uint64
		s, _ := drainWithRouting(50*time.Millisecond, 5*time.Second, connections.Load,
			routingStep{routing: Routing{}})
		result := make(chan DrainResult, 1)
		go func() { result <- s.drain(nil) }()
		for range 5 {
			time.Sleep(30 * time.Millisecond)
			connections.Add(1)
			synctest.Wait()
			select {
			case <-result:
				t.Fatal("drain finished while new connections were arriving")
			default:
			}
		}
		lastConnection := time.Now()
		drained := <-result
		assert.Equal(t, DrainReasonRoutedAway, drained.Reason)
		assert.GreaterOrEqual(t, time.Since(lastConnection), s.cfg.DrainQuietPeriod)
	})
}

func TestDrainFallsBackToTheQuietPeriodForRoutingItCannotObserve(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := drainWithRouting(20*time.Millisecond, 5*time.Second, idle,
			routingStep{routing: Routing{Unverified: "kube-proxy healthz unreachable on node-b"}})
		result := s.drain(nil)
		assert.Equal(t, DrainReasonUnverified, result.Reason)
		assert.Equal(t, "kube-proxy healthz unreachable on node-b", result.Routing)
		assert.True(t, result.Unverified)
	})
}

func TestDrainReportsUnverifiedRoutingWhenItEndsAtTheBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var connections atomic.Uint64
		s, _ := drainWithRouting(50*time.Millisecond, 200*time.Millisecond, func() uint64 { return connections.Add(1) },
			routingStep{routing: Routing{Unverified: "no EndpointSlice lists this pod"}})
		result := s.drain(nil)
		assert.Equal(t, DrainReasonBound, result.Reason)
		assert.True(t, result.Unverified)
	})
}

func TestDrainCountsThePodAsRoutedUntilTheFirstReport(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, script := drainWithRouting(20*time.Millisecond, 200*time.Millisecond, idle)
		result := s.drain(nil)
		assert.Equal(t, DrainReasonBound, result.Reason)
		<-script.stopped
	})
}

func TestDrainFollowsTheLatestReportWhenTheRoutingReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := drainWithRouting(100*time.Millisecond, 400*time.Millisecond, idle,
			routingStep{routing: Routing{}},
			routingStep{after: 50 * time.Millisecond, routing: Routing{Routed: true, Detail: "a new slice routes here"}})
		result := s.drain(nil)
		assert.Equal(t, DrainReasonBound, result.Reason)
		assert.Equal(t, "a new slice routes here", result.Routing)
	})
}

func TestValidateDrainFillsDefaultsAndRejectsAnInvertedWindow(t *testing.T) {
	cfg := &Config{}
	require.NoError(t, validateDrain(cfg))
	assert.Equal(t, DefaultDrainQuietPeriod, cfg.DrainQuietPeriod)
	assert.Equal(t, DefaultDrainMaxWait, cfg.DrainMaxWait)
	require.Error(t, validateDrain(&Config{DrainQuietPeriod: 3 * time.Second, DrainMaxWait: 2 * time.Second}))
	require.Error(t, validateDrain(&Config{DrainQuietPeriod: time.Second, DrainMaxWait: MaxDrainWait + time.Second}))
}
