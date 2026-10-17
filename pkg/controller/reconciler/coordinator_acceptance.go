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

package reconciler

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"weak"

	"github.com/haproxy-haptic/HAPTIC/pkg/controller/events"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/pipeline"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercontext"
	"github.com/haproxy-haptic/HAPTIC/pkg/controller/rendercycle"
	"github.com/haproxy-haptic/HAPTIC/pkg/httpstore"
)

// HTTPContentAcceptance is the HTTP store's reversible acceptance (ADR-0030).
type HTTPContentAcceptance interface {
	AcceptanceSequence() uint64
	ConfirmAcceptances(observations []httpstore.ObservationToken)
	RevokeAcceptances(sequence uint64) int
}

const maxRefusedOutputs = 32

// acceptanceWindow keeps exact confirmation tokens and a conservative revocation ceiling.
type acceptanceWindow struct {
	reached      uint64
	observations []httpstore.ObservationToken
}

type acceptanceLedger struct {
	mu              sync.Mutex
	windows         map[weak.Pointer[rendercycle.Occurrence]]acceptanceWindow
	refused         map[string]struct{}
	refusedOrder    []string
	attempting      bool
	requested       chan struct{}
	attempts        sync.WaitGroup
	attempted       atomic.Uint64
	lastRefusal     string
	criticalFailure error
	// deployedFrom is the acceptance sequence the latest deploying render read from.
	deployedFrom atomic.Uint64
}

func newAcceptanceLedger() *acceptanceLedger {
	return &acceptanceLedger{
		windows:   make(map[weak.Pointer[rendercycle.Occurrence]]acceptanceWindow),
		refused:   make(map[string]struct{}),
		requested: make(chan struct{}, 1),
	}
}

func (l *acceptanceLedger) record(occurrence *rendercycle.Occurrence, window acceptanceWindow) {
	l.mu.Lock()
	defer l.mu.Unlock()
	reference := weak.Make(occurrence)
	if _, exists := l.windows[reference]; !exists {
		// Delayed verdicts keep their window without the ledger retaining rendered snapshots.
		runtime.AddCleanup(occurrence, cleanAcceptanceWindow, acceptanceWindowCleanup{ledger: l, occurrence: reference})
	}
	l.windows[reference] = window
	runtime.KeepAlive(occurrence)
}

func (l *acceptanceLedger) window(occurrence *rendercycle.Occurrence) (acceptanceWindow, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	window, exists := l.windows[weak.Make(occurrence)]
	runtime.KeepAlive(occurrence)
	return window, exists
}

type acceptanceWindowCleanup struct {
	ledger     *acceptanceLedger
	occurrence weak.Pointer[rendercycle.Occurrence]
}

func cleanAcceptanceWindow(cleanup acceptanceWindowCleanup) {
	cleanup.ledger.mu.Lock()
	defer cleanup.ledger.mu.Unlock()
	delete(cleanup.ledger.windows, cleanup.occurrence)
}

func (l *acceptanceLedger) markRefused(checksum string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, exists := l.refused[checksum]; exists {
		return
	}
	l.refused[checksum] = struct{}{}
	l.refusedOrder = append(l.refusedOrder, checksum)
	if len(l.refusedOrder) > maxRefusedOutputs {
		delete(l.refused, l.refusedOrder[0])
		l.refusedOrder = l.refusedOrder[1:]
	}
}

func (l *acceptanceLedger) isRefused(checksum string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, refused := l.refused[checksum]
	return refused
}

// acceptanceSequence is 0 when the coordinator runs without an HTTP store.
func (c *Coordinator) acceptanceSequence() uint64 {
	if c.acceptance == nil {
		return 0
	}
	return c.acceptance.AcceptanceSequence()
}

func (c *Coordinator) settleAcceptances(event *events.RenderGateCompletedEvent) {
	if c.acceptance == nil || (!event.OK && !event.Refused) {
		return
	}
	occurrence, err := event.RenderOccurrence()
	if err != nil {
		return
	}
	cycle, err := occurrence.Snapshot()
	if err != nil {
		return
	}
	checksum, err := cycle.ContentChecksum()
	if err != nil {
		return
	}
	window, dispatched := c.ledger.window(occurrence)
	if event.Refused {
		c.ledger.markRefused(checksum)
	}
	if !dispatched {
		return
	}
	if event.OK {
		c.acceptance.ConfirmAcceptances(window.observations)
		return
	}
	if revoked := c.acceptance.RevokeAcceptances(window.reached); revoked > 0 {
		c.logger.Warn("HAProxy refused a render containing newly accepted HTTP content; "+
			"took the content back and rendering without it", "revoked", revoked)
	}
}

// A request during acceptance schedules one more attempt for sources that arrived meanwhile.
func (c *Coordinator) requestAcceptanceAttempt(ctx context.Context, generation uint64) {
	c.ledger.mu.Lock()
	defer c.ledger.mu.Unlock()
	if c.ledger.attempting {
		select {
		case c.ledger.requested <- struct{}{}:
		default:
		}
		return
	}
	c.ledger.attempting = true
	c.ledger.attempts.Add(1)
	go c.runAcceptanceAttempts(ctx, generation)
}

func (c *Coordinator) runAcceptanceAttempts(ctx context.Context, generation uint64) {
	defer c.ledger.attempts.Done()
	for {
		c.attemptAcceptance(ctx, generation)
		c.ledger.attempted.Add(1)
		if !c.ledger.anotherAttempt(ctx) {
			return
		}
	}
}

// An unavailable critical source is refetched by the HTTP store, whose
// recovery triggers the render that requests the next attempt.
func (l *acceptanceLedger) anotherAttempt(ctx context.Context) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ctx.Err() == nil {
		select {
		case <-l.requested:
			return true
		default:
		}
	}
	l.attempting = false
	return false
}

// Acceptance output is discarded; a deploying render reads what was accepted
// since the latest one read the store, by this attempt or another render (#363).
func (c *Coordinator) attemptAcceptance(ctx context.Context, generation uint64) {
	opts, err := c.renderOptions(generation)
	if err != nil {
		c.logger.Debug("HTTP content acceptance attempt skipped", "error", err)
		return
	}
	deployed := c.ledger.deployedFrom.Load()
	defer func() {
		if ctx.Err() == nil && c.acceptance.AcceptanceSequence() != deployed {
			c.eventBus.Publish(events.NewReconciliationTriggeredEvent("http_content_accepted", true, events.WithNewCorrelation()))
		}
	}()
	attemptCtx := pipeline.WithRefusedOutputs(ctx, c.ledger.isRefused)
	result, err := c.pipeline.Execute(attemptCtx, c.storeProvider, rendercontext.RenderModeReconcile, opts...)
	if ctx.Err() == nil {
		c.recordRender(result, err)
	}
	if err != nil {
		if ctx.Err() == nil {
			c.reportPendingContent(err)
			if c.ledger.updateCriticalContentFailure(err) {
				c.eventBus.Publish(events.NewReconciliationTriggeredEvent("critical_http_content_unavailable", true, events.WithNewCorrelation()))
			}
		}
		return
	}
	c.ledger.mu.Lock()
	c.ledger.lastRefusal = ""
	c.ledger.criticalFailure = nil
	c.ledger.mu.Unlock()
}

func (l *acceptanceLedger) criticalContentFailure() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.criticalFailure
}

func (l *acceptanceLedger) updateCriticalContentFailure(err error) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !pipeline.CriticalContentUnavailable(err) {
		l.criticalFailure = nil
		return false
	}
	firstFailure := l.criticalFailure == nil
	l.criticalFailure = err
	return firstFailure
}

// Repeated refusals under watch churn should not bury the original diagnostic.
func (c *Coordinator) reportPendingContent(err error) {
	if pipeline.InputsMoved(err) || errors.Is(err, pipeline.ErrOutputRefusedByGate) {
		c.logger.Debug("HTTP content stays pending", "error", err)
		return
	}
	reason := err.Error()
	c.ledger.mu.Lock()
	repeated := c.ledger.lastRefusal == reason
	c.ledger.lastRefusal = reason
	c.ledger.mu.Unlock()
	if repeated {
		return
	}
	c.logger.Warn("New http.Fetch content stays pending. Non-critical sources deploy without it; "+
		"a critical source holds every deploy. Fix the source or the template the error names.",
		"error", err)
}
