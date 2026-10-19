# ADR-0036: Prepare compatible controllers before changing templates

## Status

Accepted 2026-10-18. Extends [ADR-0016](0016-one-config-kind-many-instances-pre-rollout-validation.md)
without changing its complete-set preflight or startup/live validation gates.

## Problem

A rolling upgrade applies new library content while old controllers still watch
the same config. Revision assembly prevents torn reads, but a coherent new set
can require a function absent from the old binary. In issue #428, 0.6.0 replicas
rejected valid new templates using `on_own_lines`, emitted rejection metrics,
and overwrote validation status while new replicas accepted them.

Changing a new binary cannot teach the shipped old binary to ignore incompatible
content. Renaming configs would also change ownership of generated Services and
runtime artifacts, making Helm pruning unsafe.

## Decision

The existing preflight hook validates the complete incoming configuration first.
Its preparation phase reads the existing Deployment's config name and assembles
that live set. If the candidate binary can validate it, the hook patches only the
controller image and pull policy, with Deployment UID/resourceVersion preconditions.
The Deployment's rollout strategy remains the operator's choice.
Images retain `/usr/local/bin/haptic-controller` as an alias for the command
used by shipped 0.1 Deployments; preparation leaves their command and arguments intact.

The hook waits for the observed Deployment generation and desired availability,
including `minReadySeconds`, and for every old Pod to disappear. Pod ownership is
traced through ReplicaSet to Deployment UID; terminating Pods still count. Config
and library UIDs/generations are checked before mutation and after rollout. A
concurrent spec change aborts preparation.

A read-only `/build` endpoint on the health listener reports controller version,
source hash, and bundled HAProxy version. Actual running Pods must match the
preflight binary. These values are architecture-independent; platform image
hashes are not. The existing `/debug/*` loopback restriction is unchanged.
The hook reads the owned Pod IP directly, as the shipped health NetworkPolicy
allows same-namespace Pods. Requests use no ambient proxy, refuse redirects,
and limit responses and duration.
An unchanged image reference with another build is refused before mutation;
operators must use a new tag or digest.

A definitively invalid live config or confirmed missing library selects the
existing apply-and-startup recovery path. Requiring live validity unconditionally
would violate ADR-0016: a valid incoming release must still repair a broken fleet.
API failures, revision mismatches, changed sources, and unavailable identity
checks do not authorize recovery. The incoming set always passes full preflight.

## Failure and delivery semantics

A failed preparation leaves incoming templates unapplied. Already rolled
replicas keep the previous config. The operator retries or rolls back; availability
follows the configured strategy, including intentional downtime under `Recreate`.
No extra rollout policy, config identity, validation exception, or status override
is introduced.

Helm and Flux execute the pre-upgrade hook; Argo maps it to `PreSync`. Direct apply
and `--no-hooks` retain fail-closed startup/live validation and can run preparation
explicitly. Rollback uses the previous chart's hooks and can start old controllers
before restoring old templates, so rollback to 0.6 is not guaranteed free of
transient rejection logs. Existing validation and retained-state fencing still
protect the dataplane.
