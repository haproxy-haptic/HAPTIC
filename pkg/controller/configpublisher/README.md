# pkg/controller/configpublisher

Event adapter that turns the controller's `TemplateRenderedEvent` / `RenderGateCompletedEvent` / deployment lifecycle into CRD writes via `pkg/k8s/configpublisher.Publisher`.

## Overview

This is the **leader-only** component that publishes the rendered HAProxy configuration as observable Kubernetes CRDs (`HAProxyCfg`, `HAProxyMapFile`, `HAProxyGeneralFile`, `HAProxyCRTListFile`) plus the SSL Secrets that auxiliary files reference. It also writes per-pod deployment status back onto the published `HAProxyCfg` so operators can `kubectl get haproxycfg <name>` and see exactly which pods accepted which version.

The component subscribes only on the leader, publishes on `TemplateRenderedEvent` (the render is what the fleet is given), records the render gate's later verdict on the same object as conditions, and runs four async worker goroutines so K8s API latency doesn't block the event loop.

## Quick Start

```go
import (
    "time"

    k8spublisher "github.com/haproxy-haptic/HAPTIC/pkg/k8s/configpublisher"
    "github.com/haproxy-haptic/HAPTIC/pkg/controller/configpublisher"
)

basePublisher := k8spublisher.NewWithListers(k8sClient, crdClient, listers, logger)

component := configpublisher.New(
    basePublisher,
    eventBus,
    logger,
    configpublisher.WithPublishInterval(5 * time.Second), // optional throttle
)
go component.Start(ctx) // blocks; subscribes only when leadership is held
```

`WithPublishInterval` enables a leading-edge refractory throttle on CRD writes — every reconciliation still pushes config to HAProxy pods through the deployer, but this throttles the etcd-side observability writes (which carry the full ~500 KB rendered config) to keep API-server pressure manageable during endpoint churn. Pass `0` (the default) for no throttling.

## Event Flow

The component is leader-only: the lifecycle registry only invokes `Start()` once
leadership is held, and `Start()` then subscribes (via `SubscribeTypesLeaderOnly`,
which suppresses the "late subscriber" warning) and spins up the three worker
goroutines. There is no subscription to `BecameLeaderEvent` itself — the leader
contract drives the start, not an event handler.

| Subscribed event | What the component does |
|------------------|-------------------------|
| `ConfigValidatedEvent` | Refresh the cached template config; the iteration's config is seeded at construction (`WithTemplateConfig`), so a render never waits for this event |
| `TemplateRenderedEvent` | Cache the rendered config + aux files, keyed by correlation ID |
| `RenderGateCompletedEvent` | Mirror the gate's latch; queue the `ConfigValidated` / `ConfigPinned` conditions for the verdict worker, and publish a render the pass releases |
| `ValidationFailedEvent` | Queue a `validationFailedWorkItem` so the failure shows up as an `-invalid` `HAProxyCfg` |
| `DeployedConfigPublishRequest` | Publish, as the HAProxyCfg spec, the exact bytes the deployer just applied (deploy-driven publish path; uses a separate pending slot so a validation publish cannot coalesce it away) |
| `ConfigAppliedToPodEvent` | Coalesce per-pod status updates (last-wins) and signal the status worker |
| `HAProxyPodTerminatedEvent` | Same channel — clears stale per-pod status when a pod goes away |
| `HAProxyPodsDiscoveredEvent` | Same channel — refreshes the known-pod set used to scope status writes |
| `LostLeadershipEvent` | Clear cached configuration state (`templateConfig`, the per-correlation `renderedConfigs` map, `lastPublishedChecksum`). Pending work items in the publish / validation-failed / status channels are *not* drained here — the workers stop when the lifecycle cancels Start's context, and unprocessed items go with it. |

## Three Workers, Three Throttles

| Worker | Channel | Purpose |
|--------|---------|---------|
| `publishWorker` | `publishWork` | `Publisher.PublishConfig` (creates/updates the CRDs and Secrets) |
| `validationFailedWorker` | `validationFailedWork` | Publishes the failure as an *invalid* `HAProxyCfg` (suffixed `-invalid`) so operators can inspect what was rejected |
| `statusWorker` | triggered via `statusWorkTrigger`; pending writes coalesce in `statusWorkPending` keyed by `namespace/runtimeConfig/podName` | Per-pod deployment status updates |

Each worker is a single goroutine. The publish worker enforces the leading-edge refractory throttle (when `WithPublishInterval` is set); the status worker reuses the same interval to throttle the (also expensive) status subresource writes.

A status update the `HAProxyCfg` can't take yet (`ErrRuntimeConfigNotPublished`: the object is missing, its auxiliary references aren't written, or it publishes another checksum) waits in `statusWorkParked`. The next publication attempt of the `HAProxyCfg` moves it back to `statusWorkPending`; a newer update for the same pod replaces it.

An incomplete publication stays on its worker and retries transient failures with bounded exponential backoff. The retry uses the immutable request snapshot captured at enqueue time. A newer validation generation interrupts and supersedes an older validation retry, while deploy-driven work retains arrival order without superseding validation work. Permanent API rejections release the worker for later items. Only a complete, still-authoritative publication advances content deduplication or emits `ConfigPublishedEvent`. Validation-driven repeats reconcile same-checksum resources idempotently, so deletion or metadata drift heals without duplicate completion events. Each leadership term owns fresh queues, throttles, retry scheduling, and subscription-readiness state.

`Drain(ctx)` (the `lifecycle.Drainer` hook a graceful leader stop calls after the deployer's drain) handles every event already delivered, then publishes the newest queued deploy-driven render that carries a worker receipt on the publish worker, past the throttle and subject to the failure latch. Once that publication commits, the older queued renders are dropped: the deployment intent names the newest deployment, so only its checkpoint keeps retained recovery available (ADR-0033). A failed publication leaves the queue as it was. A drain whose term already ended changes nothing.

## See Also

- [`pkg/k8s/configpublisher`](../../k8s/configpublisher/) — the underlying `Publisher` that does the actual CRD writes
- [`pkg/controller/deployer`](../deployer/) — the parallel path that pushes config to HAProxy pods (this package writes the observability artefacts; the deployer writes the live HAProxy configuration)
- [`pkg/controller/events`](../events/) — `TemplateRenderedEvent`, `RenderGateCompletedEvent`, `ConfigAppliedToPodEvent`, etc.

## License

Apache-2.0 — see root `LICENSE`.
