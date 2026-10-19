# Deployment rollout preparation

`rollout` is an operational Kubernetes utility. It changes one named container
image with a Deployment UID/resourceVersion precondition, preserves the chosen
strategy, and waits for actual binary identity and complete old-Pod termination.
It does not inspect watched routing resources or accept configuration verdicts.

Capture ownership before mutation. Pods are traced through ReplicaSet ownership,
with observed Pod UIDs retained even if labels change or the ReplicaSet is pruned.
Neither Deployment availability nor image-tag equality proves the old watchers
have exited. Build identity reads use the public, metadata-only health endpoint;
never expose or proxy loopback-only `/debug/*` as part of this utility.

Release the image patch's managedFields entry right after patching; a retained owner
makes the release tool's next server-side apply (upgrade or rollback) conflict.
A retried hook can find its own patch applied with old Pods still running: release
ownership again and wait; Pods on the previous image are a rollout, not a retag.
Guard metadata patches with a `resourceVersion` value (409, retried), never a JSON
Patch `test` op (422, not retried).
