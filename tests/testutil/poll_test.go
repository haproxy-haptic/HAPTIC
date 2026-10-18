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

package testutil

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakePollClock struct {
	instant time.Time
	waits   []time.Duration
	waitErr error
}

func (c *fakePollClock) now() time.Time { return c.instant }
func (c *fakePollClock) wait(_ context.Context, delay time.Duration) error {
	c.waits = append(c.waits, delay)
	c.instant = c.instant.Add(delay)
	return c.waitErr
}

func pollTestConfig() WaitConfig {
	return WaitConfig{InitialInterval: 2 * time.Second, MaxInterval: 4 * time.Second, Timeout: 7 * time.Second, Multiplier: 2}
}

func TestPollExplicitOutcomes(t *testing.T) {
	denial := errors.New("admission denied")
	for _, tc := range []struct {
		name      string
		result    PollResult
		cause     error
		wantError string
	}{
		{name: "success", result: PollSucceeded},
		{name: "terminal", result: PollFailed, cause: denial, wantError: "admission denied"},
		{name: "terminal without reason", result: PollFailed, wantError: "without a reason"},
		{name: "success with error", result: PollSucceeded, cause: denial, wantError: "reported success with an error"},
		{name: "unknown decision", result: PollResult(99), wantError: "unknown polling result"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &fakePollClock{}
			calls := 0
			err := poll(t.Context(), pollTestConfig(), "admission", func(context.Context) (PollResult, error) {
				calls++
				return tc.result, tc.cause
			}, clock)
			if tc.wantError == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.wantError)
			}
			if tc.cause != nil {
				require.ErrorIs(t, err, tc.cause)
			}
			assert.Equal(t, 1, calls)
			assert.Empty(t, clock.waits)
		})
	}
}

func TestPollRetriesTransientFailuresUntilSuccess(t *testing.T) {
	clock := &fakePollClock{}
	observations := []PollResult{PollPending, PollPending, PollSucceeded}
	err := poll(t.Context(), pollTestConfig(), "service routing", func(context.Context) (PollResult, error) {
		result := observations[0]
		observations = observations[1:]
		if result == PollPending {
			return result, errors.New("connection refused")
		}
		return result, nil
	}, clock)
	require.NoError(t, err)
	assert.Empty(t, observations)
	assert.Equal(t, []time.Duration{2 * time.Second, 4 * time.Second}, clock.waits)
}

func TestPollClampsWaitToOriginalDeadline(t *testing.T) {
	clock := &fakePollClock{}
	cause := errors.New("connection refused")
	err := poll(t.Context(), pollTestConfig(), "service routing", func(context.Context) (PollResult, error) {
		return PollPending, cause
	}, clock)
	require.ErrorContains(t, err, "timeout waiting for service routing")
	require.ErrorContains(t, err, "connection refused")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorIs(t, err, cause)
	assert.Equal(t, []time.Duration{2 * time.Second, 4 * time.Second, time.Second}, clock.waits)
	assert.Equal(t, time.Time{}.Add(7*time.Second), clock.instant)
}

func TestPollCancellationStopsObservationAndWaiting(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	clock := &fakePollClock{}
	err := poll(ctx, pollTestConfig(), "cancelled", func(context.Context) (PollResult, error) {
		t.Fatal("cancelled polling must not observe")
		return PollSucceeded, nil
	}, clock)
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, clock.waits)

	clock.waitErr = context.Canceled
	err = poll(t.Context(), pollTestConfig(), "cancelled wait", func(context.Context) (PollResult, error) {
		return PollPending, nil
	}, clock)
	require.ErrorIs(t, err, context.Canceled)
	assert.Len(t, clock.waits, 1)
}

func TestPollRejectsSuccessAfterDeadline(t *testing.T) {
	clock := &fakePollClock{}
	err := poll(t.Context(), pollTestConfig(), "late success", func(context.Context) (PollResult, error) {
		clock.instant = clock.instant.Add(8 * time.Second)
		return PollSucceeded, nil
	}, clock)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Empty(t, clock.waits)
}

func TestPollRejectsIntervalsThatWouldSpin(t *testing.T) {
	for _, multiplier := range []float64{0, -1, 0.5, math.NaN(), math.Inf(1)} {
		cfg := pollTestConfig()
		cfg.Multiplier = multiplier
		err := poll(t.Context(), cfg, "invalid settings", func(context.Context) (PollResult, error) {
			t.Fatal("invalid polling must not observe")
			return PollSucceeded, nil
		}, &fakePollClock{})
		require.ErrorContains(t, err, "positive intervals and a finite multiplier")
	}
}

func TestPollKeepsCancellationAndLastObservationError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cause := errors.New("last observation failed")
	err := poll(ctx, pollTestConfig(), "cancelled observation", func(context.Context) (PollResult, error) {
		cancel()
		return PollPending, cause
	}, &fakePollClock{})
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, cause)
}

func TestConsistentlyChecksUntilTheEndOfTheWindow(t *testing.T) {
	clock := &fakePollClock{}
	calls := 0
	err := consistently(t.Context(), 10*time.Second, 4*time.Second, "absence", func(context.Context) error {
		calls++
		return nil
	}, clock)
	require.NoError(t, err)
	assert.Equal(t, 4, calls)
	assert.Equal(t, []time.Duration{4 * time.Second, 4 * time.Second, 2 * time.Second}, clock.waits)
}

func TestConsistentlyFailsOnTheFirstBrokenCheck(t *testing.T) {
	clock := &fakePollClock{}
	found := errors.New("lease exists")
	calls := 0
	err := consistently(t.Context(), 10*time.Second, 4*time.Second, "absence", func(context.Context) error {
		calls++
		if calls == 2 {
			return found
		}
		return nil
	}, clock)
	require.ErrorIs(t, err, found)
	require.ErrorContains(t, err, "absence: lease exists (after 4s of 10s)")
	assert.Equal(t, 2, calls)
	assert.Equal(t, []time.Duration{4 * time.Second}, clock.waits)
}

func TestConsistentlyFailsWhenObservationEndsEarly(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := consistently(ctx, 10*time.Second, 4*time.Second, "absence", func(context.Context) error {
		t.Fatal("cancelled observation must not check")
		return nil
	}, &fakePollClock{})
	require.ErrorIs(t, err, context.Canceled)

	clock := &fakePollClock{waitErr: context.DeadlineExceeded}
	err = consistently(t.Context(), 10*time.Second, 4*time.Second, "absence", func(context.Context) error { return nil }, clock)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Len(t, clock.waits, 1)
}

func TestConsistentlyRejectsAnEmptyWindowOrInterval(t *testing.T) {
	for _, tc := range []struct{ window, interval time.Duration }{{0, time.Second}, {time.Second, 0}} {
		err := consistently(t.Context(), tc.window, tc.interval, "absence", func(context.Context) error {
			t.Fatal("invalid settings must not check")
			return nil
		}, &fakePollClock{})
		require.ErrorContains(t, err, "positive window and interval")
	}
}
