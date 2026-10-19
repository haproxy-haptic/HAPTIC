# pkg/events - Event Bus Infrastructure

Development context for working with event bus infrastructure.

**API Documentation**: See `pkg/events/README.md`
**Architecture**: See `/docs/site/docs/development/design/architecture-overview.md` (event-driven architecture)

## When to Work Here

Modify this package when:

- Fixing EventBus infrastructure bugs
- Adding new coordination patterns (like scatter-gather)
- Improving performance or memory usage of event system
- Enhancing startup coordination logic

**DO NOT** modify this package for:

- Adding new event types → Use `pkg/controller/events`
- Changing business logic → Use appropriate domain package
- Adding domain-specific validation → Use `pkg/controller/validator`

## Key Design Principle

This package is **domain-agnostic infrastructure**. It contains zero business logic and no knowledge of controllers, HAProxy, Kubernetes, or templates.

Think of this as a library that could be extracted and used in any Go project needing pub/sub or request-response patterns.

## Core Components

### EventBus

Thread-safe pub/sub coordinator with startup synchronization.

**Key Design Decisions:**

1. **Non-blocking publish**: Drops events to slow subscribers rather than blocking
2. **Startup buffering**: Prevents race conditions during initialization
3. **Pause / Resume**: `Pause()` puts the bus back into buffering mode; `Start()`
   replays the buffer. Used during leadership transitions to make late-subscribing
   leader-only components safe (see `pkg/controller/leaderelection/CLAUDE.md`).
   Both methods are idempotent.
4. **No event replay** (after delivery): Once delivered to a subscriber and that
   subscriber's buffer is full, the event is dropped — there is no per-subscriber
   replay log.
5. **Causal delivery**: `fanOut` sends under `deliverMu`, so an event a subscriber
   publishes in reaction to another reaches every subscriber after its cause.
6. **Minimal API**: Publish, Subscribe (+ typed/lossy variants), Pause, Start, Request
7. **Opt-in fan-out isolation**: `FanoutIsolatedEvent` snapshots an event before
   buffering and clones its container for each subscriber.

**Implementation Notes:**

```go
// bus.go internals (real shape — not just []chan Event)
type EventBus struct {
    subscribers      []subscriber           // universal subs; carries lossy flag, name, channel
    typedSubscribers []*typedSubscription   // type-filtered subs (SubscribeTypes)
    mu               sync.RWMutex

    // Startup coordination
    started        bool
    startMu        sync.Mutex
    preStartBuffer []Event

    // Drop accounting — separated by criticality so observability noise doesn't
    // mask real backpressure problems.
    droppedEventsCritical      uint64       // atomic counter; SubscribeLossy never increments this
    droppedEventsObservability uint64       // atomic counter for lossy subscribers
    onDrop                     DropCallback // fires only on critical drops
}
```

Why this design?

- RWMutex allows concurrent reads (publish walks the subscriber list).
- Separate `startMu` prevents deadlock between startup and publish.
- Pre-start buffering avoids lost events during component initialization,
  capped by `MaxPreStartBufferSize`.
- The lossy/critical split keeps observability subscribers (commentator, debug,
  metrics) from triggering the same drop alerts as business-critical paths.

`Publish` is non-blocking and returns the number of subscribers that received
the event (`int`, not `error`). Pre-start buffered events return `0`.

### Typed Subscriptions

Filter events at the bus level for improved performance.

**Pattern:** **SubscribeTypes()** filters by event type strings at the bus level
(variants: `SubscribeTypesLeaderOnly` for components subscribing after leader
election). Typed subscriptions are always critical; lossy delivery is
universal-only, via `SubscribeLossy`, for observability consumers where silent
drops are acceptable.

**When to Use:**

- Component only cares about specific event types
- High-volume event streams where filtering matters

**When NOT to Use:**

- Commentator/logging (needs all events)
- Debugging (universal subscription is clearer)

**Example:**

```go
// SubscribeTypes - efficient, filters at bus level
eventChan := bus.SubscribeTypes("reconciler", 100, "reconciliation.triggered", "reconciliation.completed")
for event := range eventChan {
    // Only receives the specified event types
    switch e := event.(type) {
    case *events.ReconciliationTriggeredEvent:
        // handle
    case *events.ReconciliationCompletedEvent:
        // handle
    }
}
```

### Request-Response (Scatter-Gather)

Synchronous coordination using timeout and response correlation.

**When to Use:**

- Configuration validation (need all validators to respond)
- Distributed queries (gather info from multiple sources)
- Coordinated operations (need acknowledgment from multiple parties)

**When NOT to Use:**

- Fire-and-forget notifications (use Publish instead)
- Single responder (use direct function call)
- High-frequency operations (too much overhead)

**Implementation Pattern:**

```go
// Internal correlation
type pendingRequest struct {
    request   Request
    responses chan Response
    done      chan struct{}
}

// Request() creates correlation context and waits
func (b *EventBus) Request(ctx context.Context, req Request, opts RequestOptions) (*RequestResult, error) {
    // 1. Create correlation context
    // 2. Publish request to all subscribers
    // 3. Collect responses matching request ID
    // 4. Return when all expected responses received or timeout
}
```

## Testing Approach

### Test Infrastructure, Not Domain Logic

```go
// Test event defined at package scope (Go doesn't allow methods inside func bodies).
type testEvent struct{ value string }

func (e testEvent) EventType() string    { return "test" }
func (e testEvent) Timestamp() time.Time { return time.Now() }

func TestEventBus_Publish(t *testing.T) {
    bus := NewEventBus(100)

    sub := bus.Subscribe("test-sub", 10)
    bus.Start()

    bus.Publish(testEvent{value: "hello"})

    event := <-sub
    assert.Equal(t, "hello", event.(testEvent).value)
}
```

### Test Scenarios

1. **Basic pub/sub**: Publish event, verify subscriber receives it
2. **Startup buffering**: Publish before Start(), verify replay after Start()
3. **Slow subscribers**: Fill subscriber buffer, verify drop behavior
4. **Concurrent publish**: Multiple goroutines publishing simultaneously
5. **Request-response**: Send request, verify response correlation
6. **Timeout handling**: Request with no responders, verify timeout
7. **Context cancellation**: Cancel context during Request(), verify cleanup

## Common Pitfalls

### Blocking in Event Handlers

**Problem**: Subscriber blocks, channel fills, events dropped.

```go
// Bad - blocks for 5 seconds
for event := range eventChan {
    time.Sleep(5 * time.Second)  // Simulating slow work
    process(event)
}
```

**Solution**: Process quickly or spawn goroutine.

```go
// Good - non-blocking handler
for event := range eventChan {
    event := event  // Capture
    go func() {
        process(event)  // Long-running work in goroutine
    }()
}
```

### Buffer Sizing

**Problem**: Buffer too small → frequent drops; too large → high memory.

**Guidelines:**

```go
// Control events (low frequency)
controlChan := bus.Subscribe("control", 10)  // Small buffer OK

// High-volume events (resource changes)
resourceChan := bus.Subscribe("resources", 200)  // Larger buffer

// Pre-start buffer
bus := NewEventBus(100)  // Based on expected init events
```

**Rule of Thumb** (see `pkg/controller/buffers` for the two controller-side tiers):

- Control events: 10-50
- Resource events: 100-200
- Pre-start buffer: 100-200

### Forgetting EventBus.Start()

**Problem**: Events published before Start() never reach early subscribers.

```go
// Bad
bus := NewEventBus(100)
component1 := NewComponent1(bus)  // Subscribes
component2 := NewComponent2(bus)  // Subscribes
// Events published during setup are buffered
// Forgot to call bus.Start() - events never replayed!
```

**Solution**: Always call Start() after all components subscribe.

```go
// Good
bus := NewEventBus(100)

// Components subscribe during initialization
component1 := NewComponent1(bus)
component2 := NewComponent2(bus)

// Start after all subscribers ready
bus.Start()  // Replays buffered events

// Now normal operation
bus.Publish(SystemReadyEvent{})
```

### Request() Deadlock

**Problem**: Responder also calls Request(), causing deadlock.

```go
// Bad - deadlock risk
func (c *Component) Start(ctx context.Context) error {
    for event := range c.eventChan {
        if req, ok := event.(MyRequest); ok {
            // This can deadlock if request depends on this component responding
            result, _ := c.eventBus.Request(ctx, OtherRequest{}, ...)
            c.eventBus.Publish(MyResponse{result: result})
        }
    }
    return nil
}
```

**Solution**: Use separate goroutine or don't nest Request() calls.

```go
// Good - handle in goroutine
func (c *Component) Start(ctx context.Context) error {
    for event := range c.eventChan {
        if req, ok := event.(MyRequest); ok {
            req := req  // Capture
            go func() {
                // Won't block event loop
                result, _ := c.eventBus.Request(ctx, OtherRequest{}, ...)
                c.eventBus.Publish(MyResponse{result: result})
            }()
        }
    }
    return nil
}
```

## Extension Points

### Adding New Event Interfaces

If you need new event metadata beyond EventType():

```go
// events/types.go
type Event interface {
    EventType() string
    Timestamp() time.Time
}

// New interface for events with priority
type PrioritizedEvent interface {
    Event
    Priority() int
}

// Update EventBus to handle priority (if needed)
func (b *EventBus) PublishPriority(event PrioritizedEvent) {
    // Implementation
}
```

### Adding New Coordination Patterns

Follow scatter-gather as example:

1. Define new interfaces (if needed)
2. Add method to EventBus
3. Implement correlation logic
4. Add comprehensive tests
5. Update README.md with usage examples

## Performance Characteristics

### Memory

- EventBus: O(N) where N = number of subscribers (channel slice)
- Pre-start buffer: O(M) where M = events before Start()
- Request tracking: O(R) where R = concurrent requests

### CPU

- Publish: O(N) with non-blocking select (very fast)
- Subscribe: O(1) append to slice
- Request correlation: O(R×T) where T = responses per request

### Benchmarking

```go
// Reuses the package-scope testEvent declared above.
func BenchmarkEventBus_Publish(b *testing.B) {
    bus := NewEventBus(100)
    sub := bus.Subscribe("bench", 1000)
    bus.Start()

    event := testEvent{value: "test"}

    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        bus.Publish(event)
    }

    b.StopTimer()
    // Drain subscriber
    for len(sub) > 0 {
        <-sub
    }
}
```

Expected: ~100-500ns per publish with 1 subscriber.

## Related Packages

**Domain Event Types:**

- `pkg/controller/events` - All domain-specific event definitions

**Event Consumers:**

- `pkg/controller/commentator` - Logs all events with domain context
- `pkg/controller/reconciler` - Debounces changes and orchestrates pipeline execution

**Event Producers:**

- `pkg/k8s/watcher` - Publishes resource change events
- `pkg/controller/configloader` - Publishes config events
- All controller components - Publish completion/failure events

## Troubleshooting

### Events Not Reaching Subscriber

**Diagnosis:**

1. Verify EventBus.Start() was called
2. Check if subscriber buffer is full (events being dropped)
3. Verify event type matches subscriber's type assertion
4. Check subscriber is subscribed before event published

```go
// Debug subscriber state
log.Info("subscriber buffer usage", "buffered", len(eventChan), "capacity", cap(eventChan))
```

### Request() Always Timing Out

**Diagnosis:**

1. Verify responders are subscribed and running
2. Check request ID correlation matches
3. Verify responder publishes Response with correct request ID
4. Check context timeout is reasonable

```go
// Debug request-response flow
log.Info("request sent", "req_id", req.RequestID(), "expected", opts.ExpectedResponders)

// In responder
log.Info("response sent", "req_id", resp.RequestID(), "responder", resp.Responder())
```

### High Memory Usage

**Diagnosis:**

1. Check subscriber buffer sizes (reduce if too large)
2. Verify subscribers are draining channels
3. Check for subscriber goroutine leaks
4. Profile with pprof

```bash
go tool pprof http://localhost:8080/debug/pprof/heap
```

## Best Practices

### 1. Keep Event Interfaces Minimal

```go
// Good - current interface
type Event interface {
    EventType() string
    Timestamp() time.Time
}

// Avoid - too much infrastructure
type Event interface {
    EventType() string
    Timestamp() time.Time
    CorrelationID() string
    Priority() int
    // ...
}
```

### 2. Keep Domain Type Switches Out of `pkg/events`

Inside `pkg/events` itself, code should treat events as opaque `Event`
values. If something in this package starts switching on concrete event
types, the logic almost certainly belongs in a consumer (e.g. a controller
component) — `pkg/events` should stay domain-agnostic.

```go
// Good (inside pkg/events) - dispatch on the interface, not on concrete types
func deliver(sub subscriber, event Event) {
    select {
    case sub.ch <- event:
    default:
        // drop
    }
}

// Bad (inside pkg/events) - switching on domain types couples the bus
// to controller events
switch event.(type) {
case ReconciliationTriggeredEvent:
    // …
case DeploymentCompletedEvent:
    // …
}
```

**Consumers (controller components) are different.** A `switch event.(type)`
inside a component's event loop is the canonical shape — it's how
`reconciler.handleEvent`, `deployer.Component.Start`, and the commentator's
insight pipelines all dispatch their subscribed event types. Don't rewrite
those into chains of `if _, ok := event.(X); ok` — that loses the
exhaustiveness signal and reads worse.

### 3. Document Event Contracts

Event types should document their contract:

```go
// MyRequest is published when X happens.
// Responders must publish MyResponse with matching request ID within 5 seconds.
type MyRequest struct {
    id string
}

func (r MyRequest) RequestID() string { return r.id }
```

### 4. Test Thoroughly

Event infrastructure bugs affect the entire system. Write extensive tests:

- Unit tests for basic behavior
- Concurrent stress tests
- Timeout tests
- Memory leak tests

## Migration Guide

If modifying EventBus interface:

1. Update EventBus implementation
2. Update tests
3. Update README.md
4. Search codebase for all EventBus usage
5. Update all consumers incrementally
6. Run full test suite including integration tests

Example breaking change:

```go
// Old (current)
bus.Publish(event Event) int

// Hypothetical breaking change
bus.Publish(event Event) error
```

This would require updating every call site across the controller; tests need
to accept the int return value as a delivered-subscriber count today.
