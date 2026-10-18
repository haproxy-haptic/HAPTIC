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
	"fmt"
	"math"
	"time"
)

type PollResult uint8

const (
	PollPending PollResult = iota
	PollSucceeded
	PollFailed
)

type pollClock interface {
	now() time.Time
	wait(context.Context, time.Duration) error
}

type realPollClock struct{}

func (realPollClock) now() time.Time { return time.Now() }

func (realPollClock) wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Consistently runs check every interval until window has elapsed and returns
// its first error at once. The last check runs at the end of the window.
func Consistently(ctx context.Context, window, interval time.Duration, description string, check func(context.Context) error) error {
	return consistently(ctx, window, interval, description, check, realPollClock{})
}

func consistently(ctx context.Context, window, interval time.Duration, description string, check func(context.Context) error, clock pollClock) error {
	if window <= 0 || interval <= 0 {
		return errors.New("consistently needs a positive window and interval")
	}
	start := clock.now()
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%s: observation ended early: %w", description, err)
		}
		if err := check(ctx); err != nil {
			return fmt.Errorf("%s: %w (after %v of %v)", description, err, clock.now().Sub(start), window)
		}
		remaining := window - clock.now().Sub(start)
		if remaining <= 0 {
			return nil
		}
		if err := clock.wait(ctx, min(interval, remaining)); err != nil {
			return fmt.Errorf("%s: observation ended early: %w", description, err)
		}
	}
}

// Poll retries only PollPending; PollFailed and cancellation stop immediately.
func Poll(ctx context.Context, cfg WaitConfig, description string, observe func(context.Context) (PollResult, error)) error {
	return poll(ctx, cfg, description, observe, realPollClock{})
}

func poll(ctx context.Context, cfg WaitConfig, description string, observe func(context.Context) (PollResult, error), clock pollClock) error {
	if cfg.InitialInterval <= 0 || cfg.MaxInterval <= 0 || cfg.Multiplier < 1 || math.IsNaN(cfg.Multiplier) || math.IsInf(cfg.Multiplier, 0) {
		return errors.New("polling needs positive intervals and a finite multiplier of at least one")
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	start := clock.now()
	interval := cfg.InitialInterval
	var lastErr error
	attempts := 0
	for {
		elapsed := clock.now().Sub(start)
		if ctx.Err() != nil {
			return waitTimeoutError(description, attempts, elapsed, lastErr, ctx.Err())
		}
		if elapsed >= cfg.Timeout {
			return waitTimeoutError(description, attempts, elapsed, lastErr, context.DeadlineExceeded)
		}
		attempts++
		result, err := observe(ctx)
		if err != nil {
			lastErr = err
		}
		if ctx.Err() != nil {
			return waitTimeoutError(description, attempts, clock.now().Sub(start), lastErr, ctx.Err())
		}
		if clock.now().Sub(start) >= cfg.Timeout {
			return waitTimeoutError(description, attempts, clock.now().Sub(start), lastErr, context.DeadlineExceeded)
		}
		if stop, verdictErr := pollOutcome(description, result, err); stop {
			return verdictErr
		}
		elapsed = clock.now().Sub(start)
		remaining := cfg.Timeout - elapsed
		if remaining <= 0 {
			return waitTimeoutError(description, attempts, elapsed, lastErr, context.DeadlineExceeded)
		}
		if err := clock.wait(ctx, min(interval, remaining)); err != nil {
			return waitTimeoutError(description, attempts, clock.now().Sub(start), lastErr, err)
		}
		interval = time.Duration(min(float64(interval)*cfg.Multiplier, float64(cfg.MaxInterval)))
	}
}

func pollOutcome(description string, result PollResult, err error) (bool, error) {
	switch result {
	case PollSucceeded:
		if err != nil {
			return true, fmt.Errorf("%s reported success with an error: %w", description, err)
		}
		return true, nil
	case PollFailed:
		if err == nil {
			err = errors.New("condition failed without a reason")
		}
		return true, fmt.Errorf("%s: %w", description, err)
	case PollPending:
		return false, nil
	default:
		return true, fmt.Errorf("%s returned unknown polling result %d", description, result)
	}
}
