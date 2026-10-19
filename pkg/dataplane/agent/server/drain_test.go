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

func TestValidateDrainFillsDefaultsAndRejectsAnInvertedWindow(t *testing.T) {
	cfg := &Config{}
	require.NoError(t, validateDrain(cfg))
	assert.Equal(t, DefaultDrainQuietPeriod, cfg.DrainQuietPeriod)
	assert.Equal(t, DefaultDrainMaxWait, cfg.DrainMaxWait)
	require.Error(t, validateDrain(&Config{DrainQuietPeriod: 3 * time.Second, DrainMaxWait: 2 * time.Second}))
	require.Error(t, validateDrain(&Config{DrainQuietPeriod: time.Second, DrainMaxWait: MaxDrainWait + time.Second}))
}
