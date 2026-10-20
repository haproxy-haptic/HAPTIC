# pkg/controller/deployer

`NewDeployStack` builds the three components that get validated configurations onto HAProxy pods:

- **`DeploymentScheduler`** — decides *when* to deploy. Keeps the last validated config + last discovered endpoints and queues at most one pending deployment ("latest wins"). Also times out deployments that take longer than `deploymentTimeout` so a dropped `DeploymentCompletedEvent` can't wedge the pipeline forever.
- **`Component`** (the deployer itself) — consumes `DeploymentScheduledEvent` and applies the render to every discovered pod through its HAPTIC agent (`pkg/dataplane/agent/client`).
- **`DriftPreventionMonitor`** — fires a synthetic `DriftPreventionTriggeredEvent` every `driftPreventionInterval`, which re-renders and re-applies. That pass reads each pod's state with `?verify=1`, so a file changed behind the controller's back is a digest difference the apply rewrites.

All three are leader-only — only the replica holding the `Lease` deploys, observers on other replicas stay idle.

## How one pod is applied to

1. `GET /v1/state` — the plan the pod applied, the plan its worker runs, the digest of every file it holds, its runtime inventory and its HAProxy version.
2. The baseline is that applied plan, resolved through the deployer's plan cache; on a miss the opaque blob the pod stored is decoded, which is what makes a leader change cost no reload. A blob whose schema version or plan id does not check out is no baseline, and the pod gets the complete state plus a reload.
3. `deployplan.Diff` answers `runtime`, `file_only` or `reload` plus the reasons. Pods reporting the same baseline and capabilities share one answer.
4. `POST /v1/apply` carries the complete desired file set at digest granularity, a part only for a file the agent lacks (`haproxy.cfg` always travels whole), and all operation batches in one transaction. Agents without `runtime_batches` support receive a full reload for multi-batch changes. At most 16 pods concurrently, each bounded by `syncTimeout`.
5. The ACK's applied and running plan ids, the mode and the reasons land in `HAProxyCfg.status.deployedToPods[]`.

Every apply is fenced by a token — the leader epoch (a counter on the leader Lease, claimed per leadership term) and a per-term apply sequence — plus the baseline it was composed against. A `409` answers with the pod's actual baseline: `prev_mismatch` re-diffs once, `unknown_baseline` falls back to the complete state, and `stale_epoch` stands this controller down. A `409` naming missing file parts is answered by resending exactly those.

A refused apply (NACK) counts `haptic_apply_rejected_total{pod}`, carries HAProxy's own words into the pod's status, and drops that pod's baseline. An agent speaking a different API major gets the complete state and a reload; one missing a composed op kind reloads only for the plans that need it. Both count `haptic_agent_version_skew_total` and are never a refusal, which would fence the repair path.

The full contract, with the sequence diagram, is in `docs/site/docs/development/design/deployment.md`.

## Construction

```go
stack := deployer.NewDeployStack(bus, cfg, logger, domainMetrics, renderInputs, fence)
```

`renderInputs` is what the deploy side feeds back into the next render: the plan the fleet ACKed (server slots) and the capabilities the fleet's lowest HAProxy version supports. `fence` is the leadership term every apply is stamped with; nil means a single writer at epoch zero.

The controller registers the returned components with the leader lifecycle in `pkg/controller/reconciliation.go`.

Durations come from `spec.dataplane` and `spec.controller` on the CRD: `deploymentTimeout`, `driftPreventionInterval` and `syncTimeout` bound the controller; `minDeploymentInterval` and `reloadVerificationTimeout` configure the agent's reload pacing and reload deadline through the chart.

## Event Flow

```
TemplateRenderedEvent ───────┐
RenderGateCompletedEvent ────┤
HAProxyPodsDiscoveredEvent ──┤
DriftPreventionTriggeredEvent┤
DeploymentCompletedEvent ────┤       (feedback edge)
                             ▼
                     DeploymentScheduler
                             │
                             ▼
                     DeploymentScheduledEvent
                             │
                             ▼
                         Component
                             │
             ▼
           DeploymentStartedEvent
           InstanceDeploymentFailedEvent (per pod)
           ConfigAppliedToPodEvent (per pod)
           DeploymentCompletedEvent
                             │
                             ▼
                   DriftPreventionMonitor
                             │
                             ▼
                   DriftPreventionTriggeredEvent (if idle for > interval)
```

Notable details:

- The scheduler only deploys when it has *all three* inputs: a rendered config, a successful validation, and at least one discovered HAProxy endpoint. Partial state waits.
- "Latest wins" is a single slot — concurrent changes don't queue up as a FIFO, they coalesce to the most recent one. One deployment is in flight at a time, which is the whole rate limit: reload pacing belongs to the agent, so an apply that needs no reload is not held back.
- A gate verdict can become stale before delivery even when it was newest at the gate. The scheduler checks it against its held and dispatched render content before changing the latch, accepted render, or pending deployment. A stale verdict is debug-level tracing; an unauthenticated verdict remains an error.
- `DeploymentCompletedEvent.Succeeded` counts the pods *running* the render, not the pods whose apply was accepted. A pod whose paced reload is still scheduled is neither converged nor failed.
- The `DeploymentCompletedEvent` matching the active deployment ID closes the scheduler's in-progress flag. Every completion resets the drift monitor's idle timer, which is why the event is on the feedback edge in the diagram.
- `deploymentTimeout` is a safety net, not an operational target — hitting it means a lost completion event or a stuck apply, both of which are bugs to investigate.
- Cancellation uses a separate control subscription, so it reaches a blocked apply without waiting behind that call in the deployment mailbox. A timed-out deployment keeps the scheduler slot until its exact deployment ID reports termination.
- An endpoint-authority change (a pod replaced under the same URL) retires the in-flight deployment: its pods are not the fleet any more.

## Leadership Transitions

On `LostLeadershipEvent` the scheduler drops any pending deployment and clears its in-progress flag (otherwise a new leader would wait on a deployment the dead leader was handling); the drift monitor stops its timer. The deployer closes its pooled agent connections and restarts its apply sequence on the next term, under a new epoch.

On `BecameLeaderEvent` the scheduler is bootstrapped from two sides:

- All-replica components that maintain state replay their last event so the new leader's scheduler doesn't have to wait. Currently that's `HAProxyPodsDiscoveredEvent` (from `pkg/controller/discovery`) and `ConfigValidatedEvent` (from `pkg/controller/configchange`). Grep for `leadership.NewStateReplayer[` to see the canonical list.
- Neither `TemplateRenderedEvent` nor `RenderGateCompletedEvent` is replayed — the render comes from the leader-only `reconciler.Coordinator` (ADR-0001) and the verdict from the leader-only `rendergate`, so both only exist on the leader to begin with. Instead, the reconciler triggers a fresh reconciliation on `BecameLeaderEvent`, which produces a fresh render (and a fresh verdict) rather than stale replays.

The new leader's first deployment reloads nothing: each pod reports the plan it applied, and the blob it stored is decodable by any leader.

See `pkg/controller/LEADER_ONLY_COMPONENTS.md` for the full replay/clear contract every leader-only component implements.

## See Also

- [`pkg/dataplane/agent/client`](../../dataplane/agent/client/) — the `State`/`Apply` calls the executor drives
- [`pkg/dataplane/deployplan`](../../dataplane/deployplan/) — decides what one pod has to do to reach a render
- [`pkg/dataplane/renderplan`](../../dataplane/renderplan/) — the structure a render declares about its own output
- [`pkg/controller/discovery`](../discovery/) — publishes `HAProxyPodsDiscoveredEvent`
- [`pkg/controller/reconciler`](../reconciler/) — leader-only `Coordinator` that publishes `TemplateRenderedEvent` from inside `Pipeline.Execute`
- [`pkg/controller/rendergate`](../rendergate/) — leader-only gate that publishes `RenderGateCompletedEvent`, HAProxy's verdict on that render
- [`pkg/controller/leadership`](../leadership/) — the gating helper these components use
- `pkg/controller/LEADER_ONLY_COMPONENTS.md` — leadership-transition patterns
- `docs/site/docs/operations/high-availability.md` — user-facing view of the leader-only deployment split

## License

Apache-2.0 — see root `LICENSE`.
