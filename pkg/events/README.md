# pkg/events

Generic, domain-agnostic event bus with pub/sub and scatter-gather coordination. The controller pulls this in; everything else in the tree stays clean of it (only `pkg/controller/events` knows about domain event types).

Module path: `github.com/haproxy-haptic/HAPTIC`. Source is authoritative (`go doc ./pkg/events`); this README is a short orientation.

## Minimal Usage

```go
import "github.com/haproxy-haptic/HAPTIC/pkg/events"

bus := events.NewEventBus(100)          // pre-start buffer capacity
ch := bus.Subscribe("my-component", 50) // per-subscriber buffer

bus.Start()   // call once all subscribers are registered; releases buffered events

bus.Publish(MyEvent{...})

for event := range ch {
    switch e := event.(type) {
    case MyEvent:
        // ...
    }
}
```

Every event type implements `Event`:

```go
type Event interface {
    EventType() string
    Timestamp() time.Time
}
```

The `CoalescibleEvent` interface marks events that `pkg/controller/coalesce` can collapse when bursts arrive faster than a component can process them.

An event with authoritative private state can implement `FanoutIsolatedEvent`.
The bus then snapshots it before pre-start buffering and gives every subscriber
an independent event container. Mutating one delivered container can't change
what another subscriber observes. Events without this interface retain the
zero-copy fan-out behavior.

## Startup Buffering

Subscribers must register **before** `Start()` or they miss the replay of events published during initialisation. The controller's reinitialisation loop does this in a strict order inside `iteration.go`; bespoke users of this library should follow the same pattern:

1. `NewEventBus(n)` — `n` is the buffer that holds pre-start publishes.
2. Construct every component. Each calls `Subscribe(...)` inside its constructor.
3. `bus.Start()` — flushes the pre-start buffer to each subscriber's channel.
4. Start the component goroutines.

Forgetting step 3 is a common footgun: publishes succeed silently (buffered) but nothing ever fires.

## Typed Subscriptions

Filter pattern on top of the base `Subscribe`:

- **`SubscribeTypes(name, bufferSize, types...)`** — filters at the bus, only delivers events whose `EventType()` is in the list. Cheapest when you only care about a handful of types. Variant: `SubscribeTypesLeaderOnly`, for components that subscribe after leader election. Typed subscriptions are always critical — lossy is universal-only.

The commentator and anything logging "everything" should use plain `Subscribe` — filtering there would just hide events.

## Scatter-Gather (Request/Response)

Used when multiple independent responders all need to approve something — the canonical caller is `pkg/controller/configchange.ConfigChangeHandler`, which fans `ConfigValidationRequest` out to `BasicValidator`, `TemplateValidator`, and `JSONPathValidator` and aggregates the responses. Note that the admission webhook does **not** use scatter-gather; production calls `dryrunvalidator.ValidateDirect` synchronously to keep the admission path tight.

```go
req := NewMyRequest(payload)
result, err := bus.Request(ctx, req, events.RequestOptions{
    Timeout:            10 * time.Second,
    ExpectedResponders: []string{"basic", "template", "jsonpath"},
})
if err != nil {
    // timeout or context cancelled
}
for _, resp := range result.Responses {
    // resp.Responder() identifies who sent it, resp.RequestID() ties it back
}
```

Responders listen on their own subscription, match on request ID, and `Publish` a `Response` — there's no direct wiring, they just need to call `bus.Publish(resp)` with the request ID matching.

Don't nest `Request()` calls on the same path without spawning a goroutine for the outer call — a responder that blocks on another `Request` while its own pending request waits can deadlock.

## Delivery Order

Delivery is causal: if a subscriber publishes an event in reaction to one it received, every subscriber gets the original first. Concurrent, unrelated publishes have no defined order.

## Back-Pressure

Publish is non-blocking. If a subscriber's buffer is full, the event is **dropped for that subscriber** (others still receive it). The drop accounting splits into two paths (see `recordDrop` in `bus.go`):

- **Non-lossy ("critical") subscribers** — the default `Subscribe` / `SubscribeTypes`. Drops bump `DroppedEventsCritical` *and* fire the registered `DropCallback`. The controller records the drop, cancels the current iteration, and rebuilds every component from authoritative state.
- **Lossy subscribers** — opt-in via `SubscribeLossy` (universal subscriptions only). Drops bump `DroppedEventsObservability` only; the `DropCallback` is **not** invoked. Use this for observability subscribers (commentator, debug ring buffer) where occasional drops on a burst are acceptable and shouldn't trip the per-subscriber alert metric.

Keep critical handlers responsive. A full critical buffer restarts the controller iteration because continuing after lost coordination work is unsafe.

## Buffer Sizing Rule of Thumb

`pkg/events/defaults.go` exposes five named constants — prefer them over raw integers so the intent is readable at the call site:

| Tier | Constant | Value | For |
|------|----------|-------|-----|
| Low | `LowVolumeSubscriberBuffer` | 10 | Components that see at most a handful of events per second (leadership transitions, config reloads). |
| Standard | `StandardSubscriberBuffer` | 50 | Default for most controller components. |
| High | `HighVolumeSubscriberBuffer` | 100 | Reconciliation-path consumers that fan in from many resource types. |
| Publishing | `PublishingSubscriberBuffer` | 200 | Components that publish downstream work in response to every event. |
| Debug | `DebugSubscriberBuffer` | 1000 | Observability consumers (commentator, debug ring buffer) that must catch everything. |

Pre-start buffer on `NewEventBus`: roughly the number of events you expect during initialisation; 100 is fine for the main controller.

## Testing

```bash
go test ./pkg/events/...           # unit tests
go test ./pkg/events/... -race     # race detector
go test ./pkg/events/... -bench=.  # pub/sub benchmarks
```

Tests define ad-hoc `Event` types inline — the infrastructure never needs domain types.

## See Also

- `pkg/events/CLAUDE.md` — design rationale, pitfall catalogue (blocking handlers, buffer sizing, Request deadlocks), extension points
- `pkg/events/ringbuffer` — generic thread-safe ring buffer used by commentator and debug event history
- `pkg/controller/events` — domain event catalogue (~50 types across lifecycle, config, resources, reconciliation, deployment, leader election)
- `pkg/controller/commentator` — subscribes to every event for domain-aware logging
- `pkg/controller/coalesce` — collapses `CoalescibleEvent` bursts

## License

Apache-2.0 — see root `LICENSE`.
