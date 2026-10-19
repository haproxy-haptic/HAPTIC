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

package controller

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/testutil"
)

const coldTakeoverMessage = "Taking leadership with a cold graph"

type fakeWarmUp struct {
	warmed   chan struct{}
	mu       sync.Mutex
	inFlight chan struct{}
}

func newFakeWarmUp() *fakeWarmUp {
	return &fakeWarmUp{warmed: make(chan struct{})}
}

func (w *fakeWarmUp) Warmed() <-chan struct{} { return w.warmed }

func (w *fakeWarmUp) RenderInFlight() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.inFlight
}

func (w *fakeWarmUp) startRender() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.inFlight = make(chan struct{})
}

func (w *fakeWarmUp) finishRender(warm bool) {
	w.mu.Lock()
	done := w.inFlight
	w.inFlight = nil
	w.mu.Unlock()
	if warm {
		close(w.warmed)
	}
	close(done)
}

type handoverRun struct {
	done chan struct{}
	logs *testutil.LogRecorder
}

func startHandover(ctx context.Context, warmer warmUp, deadline <-chan time.Time, shuttingDown bool) *handoverRun {
	logger, logs := testutil.NewRecordingLogger()
	run := &handoverRun{done: make(chan struct{}), logs: logs}
	go func() {
		defer close(run.done)
		awaitHandover(ctx, warmer, deadline, func() bool { return shuttingDown }, logger)
	}()
	return run
}

func (r *handoverRun) requireDone(t *testing.T) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(testutil.EventTimeout):
		t.Fatal("the hand-over did not proceed")
	}
}

func (r *handoverRun) requireWaiting(t *testing.T) {
	t.Helper()
	select {
	case <-r.done:
		t.Fatal("the hand-over proceeded while the warm-up render was still running")
	case <-time.After(testutil.NoEventTimeout):
	}
}

func expiredDeadline() <-chan time.Time {
	deadline := make(chan time.Time, 1)
	deadline <- time.Now()
	return deadline
}

// Issue #450: a cold render still running at the deadline is the warm-up
// progressing. Taking over then cancelled it and the new leader repeated it.
func TestHandoverAwaitsTheWarmUpRenderInFlightAtTheDeadline(t *testing.T) {
	warmer := newFakeWarmUp()
	warmer.startRender()
	run := startHandover(t.Context(), warmer, expiredDeadline(), false)

	run.requireWaiting(t)
	warmer.finishRender(true)
	run.requireDone(t)
	assert.Empty(t, run.logs.Levels(coldTakeoverMessage))
}

func TestHandoverReportsAColdTakeover(t *testing.T) {
	tests := []struct {
		name         string
		render       bool
		shuttingDown bool
		want         slog.Level
	}{
		{name: "warm-up render left no graph", render: true, want: slog.LevelWarn},
		{name: "no warm-up render ran", want: slog.LevelWarn},
		{name: "controller shutting down", render: true, shuttingDown: true, want: slog.LevelInfo},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warmer := newFakeWarmUp()
			if tt.render {
				warmer.startRender()
			}
			run := startHandover(t.Context(), warmer, expiredDeadline(), tt.shuttingDown)
			if tt.render {
				run.requireWaiting(t)
				warmer.finishRender(false)
			}
			run.requireDone(t)
			assert.Equal(t, []slog.Level{tt.want}, run.logs.Levels(coldTakeoverMessage))
		})
	}
}

func TestHandoverEndsQuietlyWithTheIteration(t *testing.T) {
	warmer := newFakeWarmUp()
	warmer.startRender()
	ctx, cancel := context.WithCancel(t.Context())
	run := startHandover(ctx, warmer, expiredDeadline(), false)

	run.requireWaiting(t)
	cancel()
	run.requireDone(t)
	assert.Empty(t, run.logs.Levels(coldTakeoverMessage))
}

func TestHandoverProceedsOnceWarmBeforeTheDeadline(t *testing.T) {
	warmer := newFakeWarmUp()
	close(warmer.warmed)
	run := startHandover(t.Context(), warmer, make(chan time.Time), false)

	run.requireDone(t)
	require.Empty(t, run.logs.Levels(coldTakeoverMessage))
}
