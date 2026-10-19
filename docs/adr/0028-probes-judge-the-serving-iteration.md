# ADR-0028: Controller probes judge the serving iteration

## Status

Accepted 2026-10-02. Resolves the readiness blast radius that
[ADR-0016](0016-one-config-kind-many-instances-pre-rollout-validation.md) §5a
deferred (issue #253).

## Context

### What the probes measure today

Readiness, liveness and the startup probe all request `/healthz`. `/healthz`
reports the *newest* iteration: while a successor starts it shows that
successor's early checker, softened for one reinitialization grace episode
(`ReinitGraceWindow`, 165 s). An unresolved failure then returns 503:

1. readiness fails after 15 s, the webhook Service loses every endpoint, and the
   watched-resource rule (`failurePolicy: Fail`, no `namespaceSelector`) denies
   every routing write in the cluster;
2. liveness fails 15 s later and restarts the container. The fresh process
   reads the same live state the retries already read, fails the same way, and
   now has nothing to validate with.

Both replicas read the same set, so `replicaCount: 2` does not help.

### Since 2a89e3141 the old iteration keeps serving

The iteration sequence hands leadership over only after the successor's graph
is warm. Until then the predecessor keeps its term, its watchers and its
validator generation; when the successor fails before the hand-over, the
predecessor keeps all three (`iterationSequence.step`). A failure after the
hand-over tears the predecessor down.

### When a pod answers admission correctly

The webhook answers from the validator generation an iteration installed. A
generation exists only for an iteration that loaded its configuration through
a gate — the startup load gate, or every live validator for a reload — and
whose stores synced. Without one the listener denies with 503
(`no validator registered`). The verdict is correct when it is the verdict the
fleet's renderer would reach, i.e. when the pod validates against the
configuration the leader renders with. Three facts follow:

- **The leader is always correct.** The fleet runs its configuration by
  definition.
- **A follower is correct only if it runs the leader's configuration**, which a
  follower cannot observe.
- **A stale leader stops being the right leader once another replica runs the
  accepted configuration** — that replica should lead, and while it does not,
  the replicas validate against two configurations.

Neither leadership nor a healthy reconcile is needed to compute a verdict; the
generation is. What reconcile health adds is *which* configuration the verdict
is about.

## Decision

1. **`/healthz` is unchanged.** It reports convergence of the newest iteration
   and stays the startup probe, the e2e gate and the operator signal.
2. **New `/readyz` and `/livez` judge the serving iteration.** A predecessor
   that completed startup is judged instead of its starting successor when all
   of these hold:
   - it is still serving (not torn down) and **leads**;
   - no other controller replica reports a converged `/healthz` (HTTP 200 with
     no grace entry). Pods are found by the controller Deployment's
     `spec.selector`, which the chart passes as `CONTROLLER_POD_SELECTOR`
     from the same helper. It is immutable, so it also matches the pods of a
     newer release; pods of older charts carry `helm.sh/chart` and
     `app.kubernetes.io/version` and would miss them after an upgrade.
     Without the variable the exemption never applies. Only Ready siblings
     are asked; a sibling that does not answer counts as possibly converged,
     so the exemption ends.
   Otherwise both endpoints return the `/healthz` verdict.
3. **`/readyz` also requires an installed validator generation** whenever the
   admission listener exists. **`/livez` also fails** when a startup attempt has
   not returned within `ReinitGraceWindow`: a retry loop re-reads live state
   and picks up a fix, a hung attempt never does.
4. **Chart:** `readinessProbe` → `/readyz`, `livenessProbe` → `/livez`,
   `startupProbe` stays `/healthz`. The controller NetworkPolicy lets controller
   pods reach each other's health port.

## RULE #2 delta

The admission decision function does not change: same rules, same
`failurePolicy: Fail`, no selector, same dry-run pipeline, and a pod without a
generation still denies. Only *which pods are endpoints* and *which pods are
restarted* change:

| state | before | after |
|---|---|---|
| converged | Ready, validates | same |
| reinit within grace, generation installed | Ready, validates | same |
| generation retired (failed hand-over), within grace | Ready, **denies everything** with 503 | NotReady; a sibling validates, else the apiserver denies |
| stale **leader**, past grace, no converged sibling | NotReady, restarted → no validator anywhere | Ready, validates against the configuration the fleet runs |
| stale leader, a sibling converged | NotReady, restarted, sibling leads | same (exemption ends) |
| stale follower | NotReady, restarted | same |
| startup failure (no predecessor) | NotReady, denied | same |
| successor attempt hung | restarted after ~195 s | same bound |
| draining | NotReady | same |

Every admitted write is still validated by the full pipeline under a gated
configuration, and the only newly Ready state validates against the
configuration the fleet renders with. Two-configuration skew is never longer
than today: a stale follower keeps today's bound, and a stale leader loses the
exemption the moment a sibling converges. Routing gets strictly stronger: a pod
whose generation is gone leaves the endpoints instead of denying requests a
sibling could validate.

What gets longer is the time a *single* configuration — the last one that ran —
stays active when no replica can start the accepted one. That state existed for
165 s before and ended in a cluster-wide deny; it now lasts until a successor
starts. The leader keeps deploying it, and its stale followers keep failing
probes and restarting, so the failure stays visible in pod restarts, `/healthz`
503 and the iteration error log.

## Consequences

- No admission capacity is lost to a reinit failure that the leader survives.
- A configuration change that no replica can activate is reported by `/healthz`,
  follower restarts and error logs, not by a cluster-wide admission outage.
- Operators who overrode `readinessProbe`/`livenessProbe` keep their override.
- The recovery path never crosses the webhook: `HAProxyTemplateConfig`,
  `HAProxyTemplateLibrary`, Secrets, CRDs and RBAC are not admission-checked.

## Alternatives considered

**Readiness from the validator generation alone, liveness unchanged.** Buys
15 s: liveness restarts the pod into a startup that fails the same way.

**Every stale replica Ready.** Two config changes inside one grace window can
leave a follower on an older configuration than the leader with no converged
replica to end it.

**Stale leader exempt without the sibling check.** A replica that starts the
accepted configuration — routine during a chart upgrade, when old pods cannot
load what new pods can — would validate against it while the old leader keeps
rendering the old one, indefinitely.

**`publishNotReadyAddresses` on the webhook Service.** Routes requests to pods
that are still binding, which then fail; strictly worse than routing on
readiness.

**`failurePolicy: Ignore` or a narrower rule.** RULE #2.
