# Roll out without dropped requests

A Deployment deletes an old pod as soon as its replacement is Ready. Kubernetes
decides Ready from the pod's own probes, and HAProxy learns about the new
endpoint a moment later. With a single replica, requests sent in that gap fail
with `503` because HAProxy has no server it can use.

The readiness gate `haproxy-haptic.org/load-balancer-ready` closes the gap.
HAPTIC sets this condition on a pod only after every HAProxy pod runs a
configuration that routes to it, so the Deployment keeps the old pod until
HAProxy can use the new one.

## Enable the gate in HAPTIC

Export your release's values as described in
[Change settings](../deploying-with-helm.md#change-settings), then upgrade with the
gate enabled:

```bash
helm upgrade haptic oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
  --version 0.8.0 --namespace haptic \
  -f haptic-values.yaml \
  --set controller.templateLibraries.kubernetesBackends.podReadinessGate.enabled=true
```

Enabling the gate adds a cluster-wide watch on pods. The controller's ClusterRole
gains `get`, `list`, and `watch` on `pods` and `patch` on `pods/status`.

## Add the gate to a workload

List the condition under `readinessGates` in the pod template:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
spec:
  template:
    spec:
      readinessGates:
        - conditionType: haproxy-haptic.org/load-balancer-ready
```

To add the gate to an existing Deployment, patch it. The change starts a rollout:

```bash
kubectl patch deployment my-app --type=json \
  -p '[{"op":"add","path":"/spec/template/spec/readinessGates","value":[{"conditionType":"haproxy-haptic.org/load-balancer-ready"}]}]'
```

Check that HAPTIC set the condition on the new pods:

```bash
kubectl get pods -l app=my-app \
  -o custom-columns='NAME:.metadata.name,GATE:.status.conditions[?(@.type=="haproxy-haptic.org/load-balancer-ready")].status'
```

## How a gated pod becomes Ready

1. The pod's containers pass their readiness probes. Kubernetes reports
   `ContainersReady`, but the pod isn't Ready yet.
2. HAPTIC routes the pod. It treats the gated pod's endpoint as ready because the
   containers are ready.
3. HAPTIC sets the condition after every HAProxy pod runs that configuration. If
   the deployment to any HAProxy pod fails, HAPTIC doesn't set the condition and
   the rollout waits.
4. The pod becomes Ready, and the Deployment removes an old pod.

HAPTIC doesn't remove the condition after setting it.

## Limits of the gate

- **Gate only pods behind a Service.** HAPTIC sets the condition for pods listed
  in an EndpointSlice. A pod no Service selects never becomes Ready.
- **A gated pod becomes Ready only while HAPTIC runs.** Without a controller
  leader, new gated pods stay NotReady and the Deployment reports the stalled
  rollout after its `progressDeadlineSeconds`. Pods that are already Ready keep
  serving.
- **Every consumer of the pod's readiness waits for HAProxy.** In-cluster clients
  of the Service don't receive the pod's endpoint until HAPTIC sets the
  condition.
- **Use one HAPTIC installation per gated pod.** Two installations write the same
  condition and overwrite each other.

## Shut down old pods gracefully

The gate keeps an old pod until HAProxy routes to its replacement. It doesn't
delay the old pod's shutdown. A request that fails to connect to the exiting
pod is retried on another server, and so is an idempotent request whose
connection closes without a response; see
[Connection reliability and timeouts](../libraries/base.md#connection-reliability-and-timeouts).
A non-idempotent request in progress when the old pod exits fails. Have the
application finish in-progress requests after `SIGTERM` before it exits.
