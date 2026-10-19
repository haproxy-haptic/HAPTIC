# The HAPTIC agent

The HAPTIC agent runs as the `agent` container of every HAProxy pod. It owns
that pod's file tree and its runtime socket: the controller sends it the
complete desired file set plus the typed runtime commands it composed for that
pod, and the agent writes the files in one transaction, runs the commands or
reloads, and reports what happened.

The agent makes no HAProxy decisions. An op kind it doesn't know, a baseline it
doesn't recognize, and a command HAProxy rejects all fall back to the same
thing: reload the file set that's on disk. That's why a classification bug
ships by rolling the controller, not the data plane.

The agent is the same binary as the controller: `haptic agent`.

Source: `pkg/dataplane/agent/{api,server,files,cli}`; the controller's end is
`pkg/dataplane/agent/client`.

## The controller's side

The controller decides, the agent executes. Between a render and an apply there
are three pure steps and one round trip.

1. **The render declares its structure.** Chart macros register what they emit —
   the sections of `haproxy.cfg`, the backend and server records behind them, the
   entries of every map, the file set — and the result is a
   `renderplan.Plan` (`pkg/dataplane/renderplan`). Nothing parses HAProxy
   configuration: the generator knows what it generated. A plan's ID is the
   digest of its canonical encoding, so two identical renders produce the same
   ID and the second apply changes nothing.
2. **The controller reads the pod's baseline.** `GET /v1/state` answers with the
   plan the pod acknowledged, the plan its worker runs, the digest of every file it
   holds, and the runtime inventory — the maps, certificates, CA files and
   crt-lists the worker actually loaded. A path that isn't in that inventory is
   a file write, never a runtime command.
3. **`deployplan.Diff` decides.** It compares the render with that baseline and
   returns a `Decision`: a verdict (`runtime`, `file_only` or `reload`), the
   typed ops for this pod, the complete file set, and a reason for every change
   that couldn't run at runtime. It's pure data in, data out — table tested per
   rule, and the playground runs the same function in a browser to answer "does
   this change reload?".
4. **The deployer applies.** One `POST /v1/apply` per pod, in parallel across the
   fleet, fenced with the leader epoch and the render sequence. Content travels
   only for the files the agent answers that it lacks. A `409` carrying the pod's
   actual baseline means the ops were composed against a state the pod no longer
   has: re-diff from what it returned. Nothing was written.
5. **The plan follows.** The pod keeps the plan's blob so a controller with a
   cold cache can read its baseline back. A runtime apply doesn't carry it:
   the controller encodes it afterwards and hands it over with
   `PUT /v1/plan?plan_id=…&proof=…`, bound to the proof that apply returned. A
   put for a plan the pod has since moved past answers `409` and is dropped.
   The blob rides the apply itself on a reload, a full re-send and the drift
   pass, and for an agent that predates the call.

A pod whose agent speaks a different API major, lacks an op kind the decision
needs, or doesn't advertise `worker_fence` gets the complete file set plus a
reload. An automatic apply from an older controller without a worker proof
also reloads instead of executing runtime ops. A manifest without
`identity_version: 1` is rejected with `400` and nothing is written.

Which macros declare what, and what takes a backend off the reload-free lane, is
the template author's side of the same contract; the chart's `Backend()`,
`BackendServers()` and `RegisterMap()` macros are where a render states it.

## Running it

```console
haptic agent \
  --base-dir /etc/haproxy \
  --config haproxy.cfg \
  --master-socket haproxy-master.sock \
  --worker-socket haproxy-worker.sock \
  --listen :5555 \
  --metrics-listen :5557 \
  --tls-dir /etc/haptic/agent-tls/..data \
  --tls-client-name haptic-controller-tls.haptic.svc \
  --state-file .haptic-agent.json \
  --reload-interval-min 5s
```

Socket flags are relative to `--base-dir` unless you give an absolute path. The
TLS directory contains `tls.crt`, `tls.key`, and `ca.crt`; the allowed client
name must match the controller identity. See [Agent certificates](../operations/agent-certificates.md)
for issuance and rotation. Legacy HTTP Basic authentication uses
`DATAPLANE_USERNAME` and `DATAPLANE_PASSWORD` only when TLS is explicitly disabled. `LOG_LEVEL` sets the
log level; logs are JSON.

| Endpoint | Auth | Purpose |
|---|---|---|
| `GET /healthz` | none | The process is alive. |
| `GET /readyz` | none | Startup initialisation finished. |
| `GET /v1/state[?verify=1][&plan=0]` | mTLS | What this pod holds and runs; `plan=0` leaves the stored plan blob out. |
| `POST /v1/apply` | mTLS | Apply a desired state. |
| `PUT /v1/plan?plan_id=…&proof=…` | mTLS | Store the blob of the applied plan after the apply. |

The chart uses mutual TLS (mTLS) by default. Explicitly disabling agent TLS
selects legacy HTTP Basic authentication. The local Unix socket exposes state
and process health without network credentials. Kubernetes runs
`haptic agent health` through that socket, so an expired certificate rejects
control requests without causing a liveness restart.

`/readyz` turns true once the worker socket answers `show info`, the master
socket answers `show proc`, the tree is hashed against the state file, crash
recovery has run, and the runtime inventory is built. It stays true afterwards:
it means `this agent accepts applies`, never `the last apply succeeded`. A
readiness that tracked apply outcomes would drain the Service and fence the
repair path exactly when an operator needs it.

## Reading its state

`haptic agent state` reads `/v1/state` and prints it: the plans the pod applied,
runs and can fall back to, the runtime inventory, the deletes still outstanding,
and the last apply's outcome. It uses the read-only local Unix socket, so it needs no network credentials
inside the pod:

```console
kubectl exec -n haptic deployment/haptic-haproxy -c agent -- haptic agent state
```

`--verify` makes the agent re-hash its tree first, so the digests are
observations rather than its last-known set. `--files` lists every file it holds
with its digest and size, and `--output json` prints the raw response.
`--url` selects a remote endpoint instead of the local socket. For HTTPS,
provide the TLS identity and the expected server name.

## Paths

One string names a file everywhere. HAProxy identifies maps and certificates by
the literal string in the configuration, and the chart writes those
base-relative, so `maps/host.map` is the manifest path, the op path, the runtime
identifier in `set map` and `show map`, and the entry in the inventory.
`--base-dir` is prefixed for disk access only and never appears in a runtime
command.

Paths must be relative and canonical, must stay under 255 bytes, and must have
no dot-prefixed component. The last rule keeps the agent's own state file, its
per-mount temp directories, and its backup directories outside every manifest.

## The apply

An apply is one multipart request: the `manifest` JSON part first, then one raw
part per file whose content the agent doesn't already hold, then an optional
opaque `plan` blob. The order is load-bearing, and so is the order in which the
agent processes it.

A file the pod holds at a known content can arrive as a patch instead of whole:
the manifest's file entry carries `patch` with the base's digest and size and
the byte range it replaces, and the part carries only the replacement. The
agent splices the part into the file on disk, then verifies the result against
the file's digest and size exactly as it verifies a whole part; a base it
doesn't hold answers the path as missing, and the controller sends the whole
file. The controller patches every file whose content it last had the pod
accept, the byte range being the run between the common prefix and the common
suffix of the two contents, and only for an agent whose state lists
`file_patch` under `features`.

Agents advertising `runtime_batches` accept up to eight operation batches in
one apply. Each batch contains at most 1,000 operations; in-place operations
share the first batch's budget. The agent validates all operations before
executing the first batch, then commits the plan and reads back the runtime
state after the final batch. It holds the apply lock throughout, so another
apply or reload can't interrupt the transaction. Older agents receive a
complete reload when a change needs multiple batches.

```mermaid
sequenceDiagram
    participant C as Controller
    participant A as Agent
    participant D as Disk
    participant H as HAProxy

    C->>A: POST /v1/apply (manifest first)
    A->>A: limits, paths, mode
    A->>A: promote LKG if validated_plan_id == applied
    A->>A: fence: applied and worker plan proofs, token, leader epoch
    alt baseline mismatch
        A-->>C: 409 Conflict {reason}
    end
    A->>D: stage parts in the target mount, splice patches, verify each digest
    alt content or patch base missing
        A-->>C: 409 {missing}
    end
    A->>D: back up every changed path into the journal
    A->>D: rename staged files, haproxy.cfg last
    alt runtime ops
        A->>H: worker socket batch, worker pid checked before and after
    else reload
        A->>H: master socket reload, wait for the new worker
    end
    A-->>C: ACK or NACK
```

Nothing is written before the fence passes. The fence accepts an apply only when
`expected_prev_plan_id`, `expected_prev_plan_proof`, and `expected_prev_token`
match what the agent has applied and `token.leader_epoch` isn't older than the
epoch it last accepted. Automatic applies also require the exact
`expected_worker_ops_plan_id` and `expected_worker_ops_plan_proof`, even when
they carry no runtime ops: a reload can load a map that the controller's state
read classified as file-only. A reload-mode apply requires the worker baseline
when its in-place batch executes.
The refusals are distinct because the controller's answers differ:

| `reason` | What happened | What the controller does |
|---|---|---|
| `prev_mismatch` | The pod is on a different plan than the ops assumed. | Re-diff from the returned state. |
| `stale_epoch` | A newer leader has already spoken to this pod. | Stand down until it re-acquires leadership. |
| `unknown_baseline` | The agent doesn't know what this pod runs. | Send full state with `mode: reload`. |
| `worker_ops_mismatch` | An automatic apply or executable in-place batch targets an outdated worker baseline. | Re-read state and re-diff against the current worker and its loaded maps; the applied baseline is intact. |

## The state machine

One apply is in flight at a time, and its phase is persisted before each step,
so a restart knows whether the tree can be a mix of two plans.

```mermaid
stateDiagram-v2
    [*] --> staged
    staged --> verified: every part matches its manifest digest
    verified --> backed_up: journal holds the last known good version
    backed_up --> written: renamed, haproxy.cfg last
    written --> applied: runtime ops succeeded
    written --> reloaded: reload succeeded
    written --> scheduled: reload deferred by the pacing window
    applied --> committed
    reloaded --> committed
    scheduled --> committed
    written --> aborted: op rejected, reload failed or disk error
    aborted --> [*]: journal restored, NACK
    committed --> [*]
```

The reported mode is what actually happened: `runtime` (ops only, no reload),
`file_only` (files changed, no command needed), `reload`, `scheduled`, `noop`,
or `rejected`.

## The backup journal

Every apply records, per path it's about to change, how that path looked when
the last known good plan was current: `modified` keeps a hard link to the
file in the mount's `.haptic-lkg` directory, `created` writes a tombstone so a
rollback deletes the file, and `deleted` takes that link before it removes the
file. The first entry per
path wins, so a path changed three times since the last known good plan still
restores to the version that was good.

A completed reload clears the journal, because the pod's own HAProxy binary just
accepted that file set. A rollback clears it too, for the same reason in
reverse: the tree is the last known good set again.

A hard link can't cross a filesystem, and the HAProxy pod has more than one —
`general/` is its own `emptyDir`. At startup the agent probes the `st_dev` of
every directory under `--base-dir` and gives each mount its own temp and backup
directory. A rename that still crosses a device falls back to a copy and is
counted; the count must stay at zero.

## Disk is the authority

The state file carries no per-file digests. It carries one fold of the observed
tree, and at startup the agent re-hashes the ownership set and compares. A
mismatch means something else wrote the tree — most often the HAProxy container
restarting and copying the bootstrap configuration back — so the applied plan
becomes unknown and the next apply is full state plus a reload.

`GET /v1/state?verify=1` re-hashes on demand, which is what the controller's
drift prevention compares against. A file keeps its proof only while its SHA-256,
mode, and owner still match what the agent wrote; any other file loses it, and the
controller sends it again. The plain `GET` returns the digests the agent last
observed.

## Runtime commands

Runtime commands use the worker stats socket. Worker identity combines the process
ID with the microsecond start time from `show info float`, so a container restart
can't preserve the old baseline by reusing its process ID. Each deferred delete
verifies that identity and keeps its wait, session shutdown, and delete on the same
connection without reconnecting. Reloads retire queued work for the outgoing
worker. Deletes enter the queue only after their inline traffic-stopping ops
succeed. The master socket carries `reload` and `show proc`.

Framing rules the agent enforces, all measured on HAProxy 3.0 and 3.4:

- A `;`-joined line stays under 12 KiB. The line buffer is all-or-nothing: over
  it, nothing at all is applied.
- A payload command is the last one on its line, and its payload is chunked
  under 12 KiB. Over the payload cap, zero entries land.
- Map values with spaces travel in the payload form. The line form of `add map`
  and `set map` stops at the first space, so a value with one is a truncation
  the operator never sees.
- Names, keys, paths, and keyword arguments pass a negative-space check first: no
  `;`, no whitespace, no `<<`, no backslash, no control characters. A value that
  fails is refused, never rewritten.
- `del map` removes one duplicate per call on 3.4 and all of them on 3.0, so the
  agent repeats it until HAProxy refuses.
- A versioned map replace is `prepare map`, chunked `add map @<version>`, then
  `commit map`. `clear map @<version>` isn't an abort — it clears on 3.0 and is
  a no-op on 3.4 — so a failed chunk leaks the version and is only counted.
- A failed certificate transaction is aborted. An open one wedges that
  certificate until the next reload.

Success is matched per command. `set severity-output number` makes HAProxy tag
its own messages, so a response tagged `[0]` to `[3]` is a failure; warning-level
`[4]` replies require checking the message. Where the
success message is known (`New backend registered`, `New server registered.`,
`Backend published.`, `Server deleted.`, `Backend deleted.`, `Done.`) it must be
present. `name is already used by other proxy` and `Wait delay expired` become
typed outcomes: the first stops the apply and reloads, because nothing at
runtime reveals what shape the existing backend has.

Backend deletion also verifies absence with `wait 1 be-removable` on the same
worker connection. HAProxy 3.4.4's `del backend` uses a 1024-byte thread-local
message buffer without clearing earlier notices. When those notices fill the
buffer, deletion succeeds but its acknowledgement is omitted. The agent accepts
that ambiguous reply only after a successful pre-deletion wait and an exact
post-deletion `Failed. No such backend.` reply. Explicit command rejections,
incomplete replies, and unconfirmed absence remain failures. A retry succeeds
if its initial wait confirms the backend is already absent. See `proxy.c` and
`errors.c` in the [HAProxy 3.4.4 source](https://www.haproxy.org/download/3.4/src/haproxy-3.4.4.tar.gz).

Within one batched line, a failure is attributed to the earliest command that
could have produced it. That's a reporting limit, not a safety one: the answer
to any rejected op is to reload the desired set.

Deletes run off the apply path. The controller composes the whole A4 sequence;
the agent executes the traffic-stopping half immediately (`unpublish backend`,
`disable server`) and queues the removal half, which blocks on `wait`. The queue
caps at 1000 servers and 100 backends per pod; past that the apply reloads
instead of growing the queue.

## Pacing, rollback, and the known-bad cache

`--reload-interval-min` is the shortest interval between two reloads. A reload
inside the window is scheduled, never dropped and never cancelled by a later
apply. While one is pending the files of a newer apply still land, its `ops` are
skipped, and its `in_place_ops` run against the running worker — guarded by
`expected_worker_ops_plan_id`, because those ops were composed against the
worker's state, not the file set's. An apply that asks for a reload inside the
window is scheduled the same way and its `in_place_ops` run at once, so the
worker that keeps serving until the reload fires gets the endpoint changes
immediately. Once they ran the pod records the manifest's
`worker_ops_plan_id`: the worker's plan with exactly those ops applied, which
the controller derives and keeps. It's never the render's own id, because an
in-place batch can carry only part of the change, and the next batch has to be
composed against what the worker holds. Map updates wait together if any map
operation or its required configuration needs the reload. This keeps a denial
entry active until its replacement enforcement is available. Replacing a map
value uses an atomic set or map transaction; it never deletes the key temporarily.
The answer says when the reload fires (`reload.scheduled_at`), so the
controller can follow up. A rejected in-place op invalidates the pod's baseline
and is reported; it never triggers a second reload.

A reload that answers `Success=0` restores the journal, reloads the restored set
when an op had already changed the running worker, and reports the failure with
HAProxy's own words. The recovery reload can itself fail — readiness stays true and the
failure becomes a condition and a metric, because a data plane that fences its
own repair path is worse than one running an unknown configuration.

Only HAProxy's own verdict on a set of bytes is remembered as known-bad, for 60
seconds. The key is the work the manifest asks for — its plan, mode, files by
the digests the agent verified, and ops — not the request bytes, because the
same rejected render comes back with a different baseline attached and a file
may arrive whole or as a patch. Input and output errors and timeouts are never
cached: they say nothing about the configuration.

## Invariants

The agent states its invariants as code. A violation increments
`haptic_agent_invariant_violations_total{name}` and logs at error level; it
never panics, because a data plane must not take itself down over an assertion.
The safety layer refuses or aborts the apply, and the decision layer degrades to
a reload.

| Name | What it asserts |
|---|---|
| `generation_monotonic` | A successful apply moves the generation by exactly one. |
| `disk_is_the_desired_set` | After a successful apply the tree is what the manifest declared. |
| `runtime_mode_does_not_reload` | A `runtime` outcome performed no reload. |
| `reload_mode_reloads` | A `reload` outcome performed or scheduled one. |
| `noop_runs_no_ops` | A `noop` outcome ran no commands. |
| `journal_only_while_diverged` | Backups exist only while the applied plan differs from the last known good one. |
| `mount_probe_found_every_mount` | No rename ever fell back to a cross-device copy. |
| `ops_executable` | Every op in a batch compiled to a command this agent knows. |

## Limits

Every loop the agent runs is bounded by one of these or by a counted collection.
They live in `pkg/dataplane/agent/api/limits.go` and are asserted at both ends.

| Limit | Value | Why |
|---|---|---|
| `MaxApplyBodyBytes` | 64 MiB | One apply request. |
| `MaxFiles` | 4096 | Files per manifest. |
| `MaxPlanBlobBytes` | 8 MiB | The opaque plan. |
| `MaxPathBytes` | 255 | One manifest path. |
| `MaxOpsPerApply` | 1000 | Operations per execution batch, including in-place operations in the first batch. |
| `MaxOpBatches` | 8 | Execution batches within one agent transaction. |
| `MaxCommandLineBytes` | 12 KiB | One `;`-joined line, under the 16 KiB buffer. |
| `MaxPayloadBytes` | 12 KiB | One payload command, under HAProxy 3.0's cap. |
| `MaxWaitBudgetMs` | 30000 | Total `wait …-removable` per apply. |
| `MaxReloadMs` | 60000 | One reload. |
| `MaxPendingServerDeletes` | 1000 | Queued server deletes per pod. |
| `MaxPendingBackendDeletes` | 100 | Queued backend deletes per pod. |
| `MaxMapDelRepeat` | 64 | `del map` calls for one key. |

## Metrics

The agent exports its own metrics on `--metrics-listen`, scraped by the chart's
PodMonitor. The controller's own view of the same applies is in
[Monitoring](../operations/metrics-reference.md#deployment-metrics); these are the
per-pod facts it can't see.

| Metric | Labels | Meaning |
|---|---|---|
| `haptic_agent_apply_total` | `mode` | Applies completed, by outcome. |
| `haptic_agent_apply_rejected_total` | `stage` | Applies refused or rolled back. |
| `haptic_agent_invariant_violations_total` | `name` | Invariants that failed. |
| `haptic_agent_reloads_total` | `result` | Reloads asked of the master process. |
| `haptic_agent_rollbacks_total` | — | File sets restored to the last known good one. |
| `haptic_agent_deferred_deletes_total` | `kind`, `outcome` | Deferred deletes: `done`, `deferred` (retrying), `superseded` (worker replaced), or `abandoned` (given up until reload). Both kinds' abandonment counters start at zero. |
| `haptic_agent_op_errors_total` | `kind` | Ops HAProxy rejected. |
| `haptic_agent_generation` | — | The apply generation. |
| `haptic_agent_map_divergence_total` | — | Read-backs that found the worker out of step. |

## Testing

Five layers cover the agent, and each answers a different question.

| Layer | Question it answers | Where |
| --- | --- | --- |
| Unit tests | Does the client hold the contract's limits and classify every answer? | `pkg/dataplane/agent/client` |
| Fake HAProxy | Does the agent's transaction, fencing, and op execution behave against a modelled worker and master socket, including under injected faults? | `pkg/dataplane/agent/haproxytest`, used by `server` and `cli` tests |
| Fake agent | Does the controller's deployer react correctly to fencing, conflicts and rejections? | `pkg/dataplane/agent/agenttest` |
| Docker suite | Does a real HAProxy do what the contract says it does? | `tests/agent` |
| Integration suite | Does a real HAProxy pod in a cluster converge on what a render declares? | `tests/integration` |

### The in-process fake agent

`pkg/dataplane/agent/agenttest` is an `httptest` server that speaks the contract
without a container. It models what the deployer reasons about — the file set at
digest granularity, the four plan ids, the fencing token, the runtime inventory —
and records every apply for assertions. It runs no HAProxy commands and writes no
files.

```go
agent := agenttest.New(t)
c, err := client.New(&client.Config{
    BaseURL:  agent.URL(),
    Username: agent.Username(),
    Password: agent.Password(),
})
require.NoError(t, err)

result, err := c.Apply(context.Background(), manifest, parts, nil)
require.NoError(t, err)
require.True(t, result.OK)
require.Equal(t, api.ResultReload, result.Mode)
require.Len(t, agent.Applies(), 1)
```

Drive the paths a deployer has to survive with `agent.SetReloadPending(true)`
(the apply comes back `scheduled`, with only the in-place ops executed),
`agent.RejectOp(api.OpServerAdd)` (the apply is rejected and the pod's baseline
is invalidated), and the `WithAgentOps` option (the skew check reports missing
op kinds). A manifest's file digests must be `renderplan.Digest` of the content;
the fake verifies them, as the real agent does.

### The Docker suite

`tests/agent` brings up the chart's topology in containers — HAProxy in
master-worker mode with both sockets, the agent in its own container against the
same mounts, `general/` on a mount of its own — and drives it through the
controller's client. It imports no agent package, so it tests the wire contract
rather than the implementation.

Run it against one HAProxy version:

```bash
make test-agent-docker HAPROXY_VERSION=3.4
```

The suite builds the `haptic` binary itself and lays it into the HAProxy image,
so no image build is needed first. To test a binary you already have, point it
at one:

```bash
HAPTIC_BINARY=$PWD/bin/haptic make test-agent-docker HAPROXY_VERSION=3.0
```

The suite skips with a message when docker is unreachable or when the binary has
no `agent` subcommand. CI runs it on HAProxy 3.0 and 3.4 — the two versions
whose runtime CLI differs — with no Kubernetes cluster involved.

Under `default-path origin`, HAProxy names a map, a certificate and a crt-list at
runtime by the literal base-relative string the configuration references. That
string is also the manifest's `File.Path` and an op's `Path`, so no component
translates paths. `TestRuntimeNamesAreTheManifestPaths` pins that.

To debug one test and see the logs emitted on failure:

```bash
go test -tags=agentdocker -run TestMapOpsRunAtRuntimeAndKeepEveryByte -v ./tests/agent/
```

Each test dumps the agent's and HAProxy's logs when it fails, then removes its
containers and volumes.

### The integration suite

`tests/integration` deploys the same topology into a Kind cluster — the HAProxy
container plus an `agent` container against the same mounts — and drives it
through `deployplan.Diff` and the client, one pod per test. It asserts what an
operator can observe: the pod's file tree through `kubectl exec … cat`, HAProxy's
runtime state through `show map` and `show info` on the worker stats socket, and
the runtime inventory through `GET /v1/state`.

Where the docker suite hand-writes ops to pin one behaviour, the integration
suite declares two file sets and lets the diff compose the ops — so it covers
the decision and the execution together, against 120 configuration fixtures.

```bash
make test-integration HAPROXY_VERSION=3.4
```

CI runs it on every supported HAProxy series for every pull request.
