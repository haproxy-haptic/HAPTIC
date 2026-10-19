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

// Package events provides an event bus for component coordination in the HAPTIC controller.
//
// The event bus supports two communication patterns:
// 1. Async pub/sub: Fire-and-forget event publishing for observability and loose coupling
// 2. Sync request-response: Scatter-gather pattern for coordinated validation and queries
package events

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
)

// EventBus provides centralized pub/sub coordination for all controller components.
//
// The EventBus supports two patterns:
// - Publish() for async fire-and-forget events (observability, notifications)
// - Request() for sync scatter-gather pattern (validation, queries)
//
// EventBus is thread-safe and can be used concurrently from multiple goroutines.
//
// Startup Coordination:
// Events published before Start() is called are buffered and replayed after Start().
// This prevents race conditions during component initialization.
//
// Typed Subscriptions:
// In addition to universal subscriptions (Subscribe), the EventBus supports typed
// subscriptions (SubscribeTypes) that filter events at the bus level for efficiency.
//
// Lossy Subscriptions:
// For observability components where occasional drops are acceptable, use SubscribeLossy().
// These subscriptions silently drop events without triggering
// the onDrop callback, and drops are counted separately in DroppedEventsObservability().
//
// Event Drop Monitoring:
// When subscriber buffers are full, events are dropped to prevent blocking.
// Use SetDropCallback() to receive notifications when critical drops occur.
// DroppedEventsCritical() returns drops from business-critical subscribers.
// DroppedEventsObservability() returns expected drops from lossy subscribers.
type EventBus struct {
	subscribers      []subscriber // Changed from []chan Event to support lossy flag
	typedSubscribers []*typedSubscription
	mu               sync.RWMutex
	deliverMu        sync.Mutex // serializes fanOut's sends; see fanOut

	// Startup coordination
	started        bool
	startMu        sync.Mutex
	preStartBuffer []Event
	bufferCapacity int

	// Event drop monitoring - separate counters for different subscriber types
	droppedEventsCritical      uint64       // atomic: drops from critical subscribers (triggers WARN)
	droppedEventsObservability uint64       // atomic: drops from lossy subscribers (silent)
	onDrop                     DropCallback // optional callback for drop notifications (critical only)
}

// NewEventBus creates a new EventBus.
//
// The bus starts in buffering mode - events published before Start() is called
// will be buffered and replayed when Start() is invoked. This ensures no events
// are lost during component initialization.
//
// The capacity parameter sets the initial buffer size for pre-start events.
// Recommended: 100 for most applications.
func NewEventBus(capacity int) *EventBus {
	return &EventBus{
		preStartBuffer: make([]Event, 0, capacity),
		bufferCapacity: capacity,
	}
}

// reportDrops accounts critical drops and hands each to the drop callback.
//
// Callers must hold no bus lock. The callback is arbitrary caller-supplied code:
// invoking it under b.mu would let a callback that subscribes deadlock, and
// invoking it under startMu (the replay path) would deadlock a callback that
// publishes. Drops from lossy subscribers never reach here — they are counted
// at the drop site in fanOut and deliberately never reported.
func (b *EventBus) reportDrops(drops []DropInfo) {
	if len(drops) == 0 {
		return
	}

	b.mu.RLock()
	cb := b.onDrop
	b.mu.RUnlock()

	for _, info := range drops {
		atomic.AddUint64(&b.droppedEventsCritical, 1)
		if cb != nil {
			cb(info)
		}
	}
}

// delivery is one subscriber's copy of an event, cloned before any send.
type delivery struct {
	ch    chan<- Event
	event Event
	lossy bool
	drop  DropInfo
}

// fanOut delivers event to every subscriber that accepts it, returning how many
// received it and the critical drops the caller must pass to reportDrops.
//
// b.mu is held for the whole walk: the loops index the subscriber slices, and
// UnsubscribeTyped removes an entry by overwriting it in place. Reporting the
// drops is left to the caller so the callback runs with no lock held.
//
// The sends run under deliverMu so delivery is causal: an event a subscriber
// publishes in reaction to this one reaches every other subscriber after it.
// Cloning happens first, outside deliverMu, because a clone can be large.
func (b *EventBus) fanOut(event Event) (sent int, criticalDrops []DropInfo) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	eventType := event.EventType()
	deliveries := make([]delivery, 0, len(b.subscribers)+len(b.typedSubscribers))

	for _, sub := range b.subscribers {
		deliveries = append(deliveries, delivery{
			ch:    sub.ch,
			event: cloneForSubscriber(event),
			lossy: sub.lossy,
			drop: DropInfo{
				EventType:      eventType,
				SubscriberName: sub.name,
				BufferSize:     sub.bufferSize,
			},
		})
	}

	for _, sub := range b.typedSubscribers {
		if !sub.filterFunc(event) {
			continue
		}
		// Typed subscriptions are always critical; SubscribeLossy is universal-only.
		deliveries = append(deliveries, delivery{
			ch:    sub.outputChan,
			event: cloneForSubscriber(event),
			drop: DropInfo{
				EventType:      eventType,
				SubscriberName: sub.name,
				BufferSize:     sub.bufferSize,
				EventTypes:     sub.eventTypesStr,
			},
		})
	}

	b.deliverMu.Lock()
	defer b.deliverMu.Unlock()
	for _, d := range deliveries {
		select {
		case d.ch <- d.event:
			sent++
		default:
			// Channel full, subscriber is lagging - drop event
			if d.lossy {
				atomic.AddUint64(&b.droppedEventsObservability, 1)
				continue
			}
			criticalDrops = append(criticalDrops, d.drop)
		}
	}

	return sent, criticalDrops
}

// Publish sends an event to all subscribers.
//
// If Start() has not been called yet, the event is buffered and will be
// replayed when Start() is invoked. This prevents events from being lost
// during component initialization.
//
// After Start() is called, this is a non-blocking operation. If a subscriber's
// channel is full, the event is dropped for that subscriber to prevent slow
// consumers from blocking the entire system.
//
// Returns the number of subscribers that successfully received the event.
// Returns 0 if event was buffered (before Start()).
func (b *EventBus) Publish(event Event) int {
	// Check if bus has started
	b.startMu.Lock()
	if !b.started {
		if b.coalesceIntoBuffer(event) {
			b.startMu.Unlock()
			return 0
		}
		// Buffer event for replay after Start(), with capacity limit
		overflowed := len(b.preStartBuffer) >= MaxPreStartBufferSize
		if !overflowed {
			b.preStartBuffer = append(b.preStartBuffer, cloneForSubscriber(event))
		}
		b.startMu.Unlock()

		if overflowed {
			slog.Warn("Pre-start buffer capacity exceeded, dropping event",
				"capacity", MaxPreStartBufferSize,
				"event_type", event.EventType())
			b.reportDrops([]DropInfo{{
				EventType:      event.EventType(),
				SubscriberName: PreStartBufferSubscriber,
				BufferSize:     MaxPreStartBufferSize,
			}})
		}
		return 0
	}
	b.startMu.Unlock()

	// Bus has started - publish to subscribers
	sent, drops := b.fanOut(event)
	b.reportDrops(drops)
	return sent
}

// coalesceIntoBuffer merges a PreStartCoalescibleEvent into the buffered event
// with the same key, keeping one entry per key however many events arrive
// while the bus buffers. Returns false for events without the interface (or
// with no buffered predecessor), which then take the append-with-cap path.
// Must be called with startMu held.
//
// The scan is linear because merging keeps at most one entry per key and the
// buffer is bounded by MaxPreStartBufferSize; buffering is a startup/pause
// state, not the steady-state publish path.
func (b *EventBus) coalesceIntoBuffer(event Event) bool {
	ce, ok := event.(PreStartCoalescibleEvent)
	if !ok {
		return false
	}
	key := ce.PreStartCoalesceKey()
	for i, buffered := range b.preStartBuffer {
		prev, ok := buffered.(PreStartCoalescibleEvent)
		if !ok || prev.PreStartCoalesceKey() != key {
			continue
		}
		b.preStartBuffer[i] = cloneForSubscriber(ce.CoalesceWith(buffered))
		return true
	}
	return false
}

func cloneForSubscriber(event Event) Event {
	isolated, ok := event.(FanoutIsolatedEvent)
	if !ok {
		return event
	}
	clone := isolated.CloneForSubscriber()
	if clone == nil {
		panic("fanout-isolated event returned a nil clone")
	}
	if clone.EventType() != event.EventType() {
		panic("fanout-isolated event clone changed event type")
	}
	return clone
}

// Start releases all buffered events and switches the bus to normal operation mode.
//
// This method should be called after all components have subscribed to the bus
// during application startup. It ensures that no events are lost during the
// initialization phase.
//
// Behavior:
//  1. Marks the bus as started
//  2. Replays all buffered events to subscribers in order
//  3. Clears the buffer
//  4. All subsequent Publish() calls go directly to subscribers
//
// This method is idempotent - calling it multiple times has no additional effect.
// Thread-safe and can be called concurrently with Publish() and Subscribe().
//
// Example:
//
//	bus := NewEventBus(100)
//
//	// Components subscribe during setup
//	commentator := NewEventCommentator(bus, logger, 1000)
//	validator := NewValidator(bus)
//	// ... more subscribers ...
//
//	// Release buffered events
//	bus.Start()
func (b *EventBus) Start() {
	b.StartContext(context.Background())
}

// StartContext starts the bus only while ctx remains active.
func (b *EventBus) StartContext(ctx context.Context) bool {
	b.startMu.Lock()

	if ctx.Err() != nil {
		b.startMu.Unlock()
		return false
	}
	// Idempotent - return if already started
	if b.started {
		b.startMu.Unlock()
		return true
	}

	// Mark as started (must be done before replaying to avoid recursion)
	b.started = true

	// Replay buffered events to subscribers
	drops, completed := b.replayBufferedEvents(ctx)
	if !completed {
		b.started = false
	}
	b.startMu.Unlock()

	b.reportDrops(drops)
	return completed
}

// Pause temporarily suspends event delivery, buffering events for later replay.
//
// This reuses the existing preStartBuffer infrastructure used during startup.
// Events published while paused are buffered and will be replayed when Start()
// is called again.
//
// Use cases:
//   - Leadership transition (pause while starting leader-only components)
//   - Hot reload scenarios
//   - Testing
//
// This method is idempotent - calling it when already paused has no effect.
// Thread-safe and can be called concurrently with Publish() and Subscribe().
//
// Example:
//
//	// During leadership transition
//	bus.Pause()                                    // Buffer events
//	bus.Publish(BecameLeaderEvent{})               // Buffered
//	startLeaderOnlyComponents()                    // Components subscribe
//	bus.Start()                                    // Replay buffered events
func (b *EventBus) Pause() {
	b.startMu.Lock()
	defer b.startMu.Unlock()

	// Idempotent - return if already paused
	if !b.started {
		return
	}

	// Return to buffering mode
	b.started = false
	b.preStartBuffer = make([]Event, 0, b.bufferCapacity)

	slog.Debug("EventBus paused, entering buffering mode")
}

// replayBufferedEvents sends all buffered events to subscribers and returns the
// critical drops for the caller to report once startMu is released.
// Must be called while holding startMu.
func (b *EventBus) replayBufferedEvents(ctx context.Context) ([]DropInfo, bool) {
	if len(b.preStartBuffer) == 0 {
		return nil, ctx.Err() == nil
	}

	// fanOut takes b.mu per event, exactly as Publish does. Iterating a snapshot
	// of the subscriber slices instead would race UnsubscribeTyped, which
	// removes an entry by overwriting it in place.
	var drops []DropInfo
	var replayed int
	for replayed < len(b.preStartBuffer) {
		if ctx.Err() != nil {
			b.preStartBuffer = b.preStartBuffer[replayed:]
			return drops, false
		}
		event := b.preStartBuffer[replayed]
		_, eventDrops := b.fanOut(event)
		drops = append(drops, eventDrops...)
		replayed++
	}

	// Clear buffer
	b.preStartBuffer = nil

	return drops, ctx.Err() == nil
}

// Request sends a request event and waits for responses using the scatter-gather pattern.
//
// This is a synchronous operation that:
// 1. Publishes the request event to all subscribers (scatter phase)
// 2. Collects response events matching the request ID (gather phase)
// 3. Returns when all expected responders have replied or timeout occurs
//
// The request must implement the Request interface to provide a unique RequestID
// for correlating responses.
//
// Use this method when you need coordinated responses from multiple components,
// such as multi-phase validation or distributed queries.
//
// Example:
//
//	req := NewConfigValidationRequest(config, version)
//	result, err := bus.Request(ctx, req, RequestOptions{
//	    Timeout: 10 * time.Second,
//	    ExpectedResponders: []string{"basic", "template", "jsonpath"},
//	})
func (b *EventBus) Request(ctx context.Context, request Request, opts RequestOptions) (*RequestResult, error) {
	return executeRequest(ctx, b, request, opts)
}

// SetDropCallback sets a callback to be invoked when events are dropped
// from CRITICAL (non-lossy) subscriber buffers. The callback receives the
// event type string.
//
// This callback is NOT called for lossy subscribers (created via SubscribeLossy()).
// Lossy drops are expected and silently counted
// in DroppedEventsObservability().
//
// The callback runs on the publishing goroutine with no bus lock held, so it may
// safely call back into the bus. It runs inline, so a slow callback slows the
// publisher that hit the drop.
//
// This callback pattern keeps the EventBus domain-agnostic while allowing
// the controller layer to handle drops with appropriate logging and metrics.
//
// Set to nil to disable drop notifications.
//
// Example:
//
//	bus.SetDropCallback(func(info DropInfo) {
//	    slog.Warn("Event dropped from critical subscriber",
//	        "event_type", info.EventType,
//	        "subscriber", info.SubscriberName,
//	        "buffer_size", info.BufferSize)
//	    metrics.EventsDroppedCritical.Inc()
//	})
func (b *EventBus) SetDropCallback(cb DropCallback) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.onDrop = cb
}

// DroppedEventsCritical returns the number of events dropped from
// business-critical (non-lossy) subscribers, plus events discarded because the
// pre-start buffer was full.
//
// Non-zero values indicate a problem that needs attention - critical
// subscribers are not keeping up with event volume.
func (b *EventBus) DroppedEventsCritical() uint64 {
	return atomic.LoadUint64(&b.droppedEventsCritical)
}

// DroppedEventsObservability returns the number of events dropped from
// lossy subscribers (observability components like commentator, debug/events).
//
// Non-zero values are expected and acceptable during high load. These drops
// don't affect controller operation - they just mean some log entries or
// debug info was skipped.
func (b *EventBus) DroppedEventsObservability() uint64 {
	return atomic.LoadUint64(&b.droppedEventsObservability)
}

// SubscriberCount returns the number of active subscriptions (universal + typed).
//
// Leader-only components subscribe and unsubscribe per leadership term and
// scatter-gather subscribes per request, so the count changes after Start() and
// this read takes the lock like every other reader of the subscriber slices.
func (b *EventBus) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers) + len(b.typedSubscribers)
}
