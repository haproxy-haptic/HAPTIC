# Sequence diagrams

## Startup and Initialization

The controller uses a **reinitialization loop** pattern where it responds to configuration changes by restarting with the new configuration. Each iteration follows these initialization steps:

```mermaid
sequenceDiagram
    participant Main
    participant Iteration as startIteration()
    participant EventBus
    participant Components
    participant ConfigChangeHandler
    participant ReloadAuthority
    participant ResourceWatcher as Resource<br/>Watcher
    participant CRDSingleWatcher as CRD/Secret<br/>SingleWatcher
    participant Reconciler

    Main->>Main: Reinitialization Loop

    loop Until Context Cancelled
        Main->>Iteration: Run iteration

        Note over Iteration,EventBus: 1. Setup Components (Stage 1)
        Iteration->>EventBus: Create EventBus(100)
        Iteration->>Components: Start validators, loaders, commentator
        Iteration->>ReloadAuthority: Observe reload requests

        Note over Iteration: 2. Load Accepted Config (Stage 2)
        Iteration->>Iteration: Fetch and validate on first start, or consume reload snapshot

        Note over Iteration,ResourceWatcher: 3. Setup Resource Watchers (Stage 3)
        Iteration->>ResourceWatcher: Create & Start
        Iteration->>ResourceWatcher: WaitForAllSync()

        Note over Iteration,CRDSingleWatcher: 4. Setup CRD + Secret SingleWatchers (Stage 4)
        Iteration->>CRDSingleWatcher: Create & Start
        Iteration->>CRDSingleWatcher: WaitForSync()

        Note over Iteration,Reconciler: 5. Reconciliation & Observability Components (Stage 5)
        Iteration->>Reconciler: Create Reconciler, Coordinator, DeploymentScheduler, Deployer, Discovery, ConfigPublisher, StatusApplier, Metrics
        Iteration->>EventBus: Publish initial ReconciliationTriggeredEvent (buffered)

        Note over Iteration,EventBus: 6. Start EventBus
        Iteration->>EventBus: Start() (replay buffered events)

        Note over Iteration: 7. Serving
        Iteration->>Iteration: Wait for config change or cancellation

        alt Config Change Detected
            CRDSingleWatcher->>EventBus: ConfigParsedEvent (new CRD spec)
            EventBus->>ConfigChangeHandler: Validate latest candidate
            ConfigChangeHandler->>ReloadAuthority: Accepted config snapshot
            ReloadAuthority-->>Main: Reload recorded (iteration keeps serving)
            Main->>Iteration: Start the successor (Stages 1–5, warm-up)
            Iteration->>Iteration: Retire the predecessor's leadership, keep the Lease
            Iteration->>Iteration: Successor enters election (Stages 6–8)
            Main->>Iteration: Tear the predecessor down
        else Context Cancelled
            Iteration-->>Main: Return nil (shutdown)
        end
    end
```

**Reinitialization Loop Pattern:**

The controller runs iterations that respond to configuration changes:

1. **Component Setup (Stage 1)**: Create the EventBus and the config-management components (validators, loaders, commentator), plus the early infrastructure servers, so health and debug endpoints respond before the config is loaded.
2. **Config load (Stage 2)**: On first start, fetch and validate the `HAProxyTemplateConfig` and credentials `Secret`. The immediate iteration after a live change consumes the exact accepted raw/effective config, discovery resolution, sources, and credentials from the previous iteration; it doesn't fetch a newer candidate. The handoff is single-use, so a failed replacement attempt fetches live state on retry. Fresh loads and schema re-resolutions run Basic, Template, JSONPath, and `validationTests` before activation.
3. **Resource Watchers (Stage 3)**: Create bulk watchers for every `spec.watchedResources` entry and wait for initial sync.
4. **Config/Secret SingleWatchers (Stage 4)**: Create `pkg/k8s/watcher.SingleWatcher`s for the CRD and credentials Secret. These use immediate callbacks (no debouncing) so configuration updates reinitialize with no artificial delay.
5. **Reconciliation & Observability (Stage 5)**: Create reconciliation components (Reconciler, Coordinator, DeploymentScheduler, Deployer, Discovery, ConfigPublisher, StatusApplier, DriftPreventionMonitor) and observability components (Metrics, Debug HTTP server). Each subscribes in its constructor, and the initial trigger events are published — buffered — before the bus starts. The input selector runs rendering, HAProxy checks, and auxiliary-file validation synchronously before accepting watched changes ([ADR-0032](../adr/0032-watched-input-isolation.md)). The RenderGate also checks each published render asynchronously ([ADR-0022](../adr/0022-haptic-agent.md)). The config validators (Basic, Template, JSONPath, and `validationTests`) are Stage 1 scatter-gather participants over `ConfigValidationRequest`, not Stage 5 components.
6. **EventBus Start**: Call `EventBus.Start()` to replay the buffered events and begin normal operation.
7. **Leader Election, Webhook, Debug (Stages 6–8)**: Start leader election (Stage 6), the admission webhook when a TLS cert directory is mounted (Stage 7), and register debug variables and the full health checker (Stage 8).
8. **Reload authority**: Observe the config-change channel from the beginning of the iteration. An accepted request during startup cancels the startup's sync waits; once the iteration serves, it's recorded and the iteration keeps serving.
9. **Hand-over**: The successor iteration runs Stages 1–5 while the current one still leads, waits for its follower warmer to leave a warm graph (after 30 s it waits only for the warm-up render in flight, which a cold leader would otherwise repeat), then retires the predecessor's term: the leader-only components stop and the election loop stops with the Lease kept. The successor's election resumes the Lease under the same identity on its first acquire, so no other replica sees a vacancy and its first render as leader is warm. The predecessor is torn down once the successor serves. A successor that fails before the hand-over leaves the predecessor serving and retries from live state; one that fails after it tears both down and releases the kept Lease, so a standby replica takes over without waiting for it to expire.
10. **Reinitialization**: The active snapshot and latest accepted candidate remain distinct. A newer parsed config retires the older candidate's reload reason and restores active state consumers; credential and schema reasons remain pending. A served-CRD change re-resolves the authoritative raw config before rebuilding, and the replacement CRD watch compares discovery once after sync to cover changes made during handoff.

The stage numbers are the code's startup log labels (`Stage 1: Creating config management components` through `Stage 8: Registering debug variables and updating health checker` — see `pkg/controller/iteration.go` and its callees). `EventBus.Start()` carries no stage label of its own; it runs between Stages 5 and 6, after every component has subscribed.

## Resource change handling

```mermaid
sequenceDiagram
    participant K8S as Kubernetes API
    participant ResourceWatcher as Resource<br/>Watcher
    participant EventBus
    participant Reconciler as Reconciler<br/>(immediate trigger)
    participant Coordinator as Coordinator<br/>(leader-only)
    participant Selector as Input selector
    participant Pipeline as Pipeline<br/>(synchronous)
    participant Scheduler as Deployment<br/>Scheduler<br/>(leader-only)
    participant RenderGate as RenderGate<br/>(leader-only)
    participant Deployer as Deployer<br/>(leader-only)
    participant HAProxy1 as HAProxy<br/>Instance 1
    participant HAProxy2 as HAProxy<br/>Instance 2

    K8S->>ResourceWatcher: Resource update event
    ResourceWatcher->>ResourceWatcher: Update local index
    ResourceWatcher->>EventBus: Publish(ResourceIndexUpdatedEvent)

    EventBus->>Reconciler: ResourceIndexUpdatedEvent
    Note over Reconciler: Fires immediately on every event —<br/>no reconciler-level debounce or refractory window

    Reconciler->>EventBus: Publish(ReconciliationTriggeredEvent)

    EventBus->>Coordinator: ReconciliationTriggeredEvent
    Coordinator->>EventBus: Publish(ReconciliationStartedEvent)

    Coordinator->>Selector: Execute(ctx, observed stores)
    Selector->>Pipeline: Validate candidate inputs
    Note over Pipeline: Render, HAProxy check,<br/>and all auxiliary-file validators
    Pipeline-->>Selector: Validated output or error
    Note over Selector: On failure, validate smaller change groups<br/>against the last accepted inputs
    Selector-->>Coordinator: Validated output or error

    alt Pipeline succeeded
        Coordinator->>EventBus: Publish(TemplateRenderedEvent)
    else Pipeline failed
        Coordinator->>EventBus: Publish(ReconciliationFailedEvent)
    end

    EventBus->>Scheduler: TemplateRenderedEvent (+ HAProxyPodsDiscoveredEvent)
    Note over Scheduler: Wait for both inputs<br/>Apply min interval / latest-wins
    Scheduler->>EventBus: Publish(DeploymentScheduledEvent)

    EventBus->>RenderGate: TemplateRenderedEvent
    Note over RenderGate: haproxy -c -dr on the newest render,<br/>concurrent with the fan-out
    RenderGate->>EventBus: Publish(RenderGateCompletedEvent)

    EventBus->>Deployer: DeploymentScheduledEvent
    Deployer->>EventBus: Publish(DeploymentStartedEvent)

    par Parallel Deployment
        Deployer->>HAProxy1: POST /v1/apply to the agent
        HAProxy1-->>Deployer: Success
    and
        Deployer->>HAProxy2: POST /v1/apply to the agent
        HAProxy2-->>Deployer: Success
    end

    Deployer->>EventBus: Publish(DeploymentCompletedEvent)
    EventBus->>Coordinator: DeploymentCompletedEvent
    Coordinator->>EventBus: Publish(ReconciliationCompletedEvent)
```

**Event-Driven Flow:**

1. **Resource Change**: ResourceWatcher receives Kubernetes events, updates the local index, and coalesces bursts within a per-resource debounce window before publishing one `ResourceIndexUpdatedEvent` per quiet window. The window defaults to 100 ms (`pkg/k8s/types.DefaultDebounceInterval`); each watched resource can override it via `spec.watchedResources.<name>.debounceInterval` (the bundled chart sets `"0"` on EndpointSlice). This is the only debounce layer.
2. **Reconciliation Trigger**: Reconciler publishes `ReconciliationTriggeredEvent` immediately on every event it receives — there is no second reconciler-level refractory window. Whole-store events (`IndexSynchronizedEvent`, `BecameLeaderEvent`, `DriftPreventionTriggeredEvent`) trigger the same way; only the initial-sync variant of `ResourceIndexUpdatedEvent` is filtered out. Reload throttling happens downstream in the deployer's `minDeploymentInterval`.
3. **Coordinator (leader-only)**: subscribes to `ReconciliationTriggeredEvent`, publishes `ReconciliationStartedEvent`, then calls the input selector **synchronously**. The selector validates observed changes through `Pipeline.Execute` and retains rejected revisions at their last accepted state.
4. **Pipeline**: runs `RenderService.Render` (template engine + auxiliary files), computes the content checksum once, then runs the configured auxiliary-file validators (the chart wires the SPOA hub validator automatically). HAProxy validation must also succeed before the selector accepts the candidate input set (ADR-0032).
5. **Coordinator post-pipeline**: on success, publishes `TemplateRenderedEvent` for downstream consumers; on failure, publishes `ReconciliationFailedEvent` carrying a `*PipelineError` (use `errors.AsType[*PipelineError]` to extract the failed phase).
6. **DeploymentScheduler (leader-only)**: subscribes to `TemplateRenderedEvent` and `HAProxyPodsDiscoveredEvent`; deploys when both are present. Enforces `minDeploymentInterval` and "latest wins" coalescing. `RenderGateCompletedEvent` moves the gate's latch: a refusal holds every later render, and the pass that names the held one releases it.
6a. **RenderGate (leader-only)**: runs `haproxy -c -dr` on the newest render, on a semaphore slot of its own, and publishes `RenderGateCompletedEvent`. A refusal reverts the pods that took the plan without loading it, and the deployer names each passing plan on its next apply so the agents may promote their rollback baseline.
7. **Deployer (leader-only)**: diffs the render against each pod's baseline, applies the result to every endpoint in parallel, logs successful endpoints directly, and publishes per-endpoint `InstanceDeploymentFailedEvent` plus aggregate `DeploymentCompletedEvent`.
8. **Completion**: Coordinator subscribes to `DeploymentCompletedEvent` and publishes `ReconciliationCompletedEvent` with duration metrics.

The Coordinator calls rendering and full validation through the input selector.
The separate RenderGate and agent checks remain in place. Admission and
configuration loading also use a synchronous validation pipeline.

## Configuration validation process

Proposal validation, configuration loading, and watched-input acceptance all
require synchronous validation. Reconciliation also checks published renders
asynchronously in `RenderGate`.

```mermaid
sequenceDiagram
    participant Caller
    participant Pipeline
    participant Render as RenderService
    participant Validate as ValidationService
    participant Binary as haproxy Binary
    participant External as Protocol-v1 Validators

    Caller->>Pipeline: Execute(ctx, storeProvider)
    Pipeline->>Render: Render(ctx, storeProvider)
    Render-->>Pipeline: authenticated output snapshot

    Pipeline->>Pipeline: retain authenticated content checksum
    Pipeline->>Validate: ValidateOutputSnapshotWithChecksum(ctx, output, checksum)
    Validate->>Validate: authenticate and materialize exact output

    Validate->>Validate: os.MkdirTemp("", "haproxy-validation-*")
    Note over Validate: Every occurrence gets a new sandbox;<br/>the checksum never bypasses the binary
    Validate->>Binary: haproxy -c -f /tmp/<unique>/haproxy.cfg
    alt HAProxy refuses the output
        Binary-->>Validate: exit 1 + stderr
        Validate-->>Pipeline: ValidationResult{Valid:false, Phase:"semantic"}
    else
        Binary-->>Validate: exit 0
        Validate-->>Pipeline: ValidationResult{Valid:true}
        Pipeline->>External: ValidateRenderedOutput(ctx, exact files)
        Note over External: Every matching v1 validator receives<br/>every request, including exact repeats
        External-->>Pipeline: warnings or error
    end

    Pipeline-->>Caller: *PipelineResult or *PipelineError
```

**Validation Steps:**

1. **Render**: the pipeline produces one authenticated immutable output snapshot.
2. **Identity**: the content checksum travels with that output for publishing and deployment comparisons. It doesn't authorize validation reuse.
3. **Built-in check**: each applicable call authenticates and materializes the snapshot, creates a fresh temp tree, and invokes `haproxy -c`. The tree mirrors the production `maps/`, `ssl/`, and `general/` layout.
4. **Rendered-output validators**: every matching protocol-v1 validator receives the complete request on every pipeline invocation. Persistent connections reuse transport only.
5. **Cancellation**: a context-aware gate removes cancelled waiters and terminates a running check. Cancellation immediately after a successful process exit still prevents a successful result.
6. **Result**: the pipeline returns `*PipelineResult` or a phase-tagged `*PipelineError`. Reconciliation publishes only after input acceptance passes full validation, then receives an additional per-occurrence verdict from `RenderGate`.

## Zero-reload deployment strategy

```mermaid
sequenceDiagram
    participant Deployer as Deployer<br/>(pkg/controller/deployer)
    participant Client as agent client
    participant Agent as HAPTIC agent
    participant HAProxy

    Deployer->>Client: State(ctx)
    Client->>Agent: GET /v1/state
    Agent-->>Client: applied plan, file digests, runtime inventory

    Deployer->>Deployer: deployplan.Diff(render, baseline)

    alt Verdict runtime
        Note over Deployer: Map entries, certificate content,<br/>server address/weight/state, backends on 3.4
        Deployer->>Agent: POST /v1/apply (mode auto, typed ops)
        Agent->>HAProxy: the ops verbatim, on the worker stats socket
        HAProxy-->>Agent: Applied
        Agent-->>Deployer: ACK (mode runtime, no reload)
    else Verdict reload
        Note over Deployer: A changed section, a new profile,<br/>or text no section accounts for
        Deployer->>Agent: POST /v1/apply (mode reload)
        Agent->>HAProxy: Write the file set, then reload through the master socket
        HAProxy-->>Agent: Reload complete
        Agent-->>Deployer: ACK (mode reload, new worker pid)
    end
```

**Deployment Optimization:**

`deployplan.Diff` compares the render with the pod's baseline to decide what that pod has to do:

1. **`runtime`**: every change is a typed command the pod's HAProxy can run — map entries, certificate and CA content, server address, weight or state, and on HAProxy 3.4 a backend the render declared dynamic. The agent writes the files, runs the commands on the worker socket, and the process keeps serving.
2. **`file_only`**: the files changed but nothing has to run — a general file no section reads, or a map the running worker never loaded. The next reload picks it up.
3. **`reload`**: a section's text changed, a profile appeared or disappeared, a file declares `reloadOnChange`, or the configuration changed in a way no section accounts for. The agent writes the whole set and reloads.

Every reason a change didn't stay reload-free is on the pod's status, so an
operator can see which part of a render cost them a reload.

The rules live in `pkg/dataplane/deployplan`, table tested per rule. A server keyword HAProxy has no runtime setter for makes that server's change structural; the keyword allow-list is `keywords.go`.
