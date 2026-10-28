# High availability

The chart starts two controller replicas and two HAProxy replicas by default.
Spread each pair across nodes so a node failure leaves a working controller and
proxy; see [anti-affinity](#anti-affinity). Each controller needs the full
[controller resource budget](performance.md#controller-resource-sizing).

<a id="overview"></a>

## What happens when a controller fails

Leader election uses a Kubernetes Lease to choose the replica that deploys
configuration. Every replica watches resources, renders changes, and serves
admission requests. Followers keep their caches and render state ready for takeover.

If the leader fails, a follower acquires the lease and renders the current state
before deploying. A voluntary handoff releases the lease without waiting for it
to expire. HAProxy continues serving its existing configuration during election.

If the new leader can't render, it can start new HAProxy pods with an acknowledged
configuration retained in Kubernetes. This also works after every controller
replica restarts. A warm follower keeps render caches, but those caches don't
provide the deployer's last validated configuration.

The controller checks the retained configuration and every auxiliary file against
their recorded content, ownership, and applied-pod evidence. It runs the current
HAProxy binary's configuration check before dispatch. Existing configured pods
receive no apply or reload. A new pod becomes Ready only when HAProxy loads the
configuration; a rejected apply leaves it NotReady.

Recovery requires the latest recorded deployment and every configured HAProxy pod
to agree. Before applying a configuration, the leader records the configuration's
identity on the `<controller.configName>-retained-state` Lease
(`haptic-config-retained-state` by default). If a newer deployment started but its checkpoint wasn't
saved, an older checkpoint can't start replacement pods. A partial rollout, an unreachable
pod, or disagreement with the checkpoint leaves new pods NotReady until rendering
recovers or the newest checkpoint can be verified. Planned and sudden leader changes
use the same checks.

A leader that stops gracefully, for example on `kubectl delete pod`, a rollout,
or an eviction, starts no further deployment, waits for the one in progress and
any HAProxy reload it scheduled, and saves its checkpoint before it releases the
Lease. It saves nothing if another replica has already taken the Lease. A leader
that stops without that step, for example after a crash, an out-of-memory kill,
or node loss, can leave its last deployment without a checkpoint for up to
`configPublishInterval` (10 seconds by default). The next leader then can't start
replacement pods until rendering recovers.

`RetainedConfigActive` Warning events identify the checksum and render failure.
The leader reports `haptic_retained_config_active = 1` while using this recovery
path. `/healthz` includes the last render error under
`components.reconciliation-coordinator.error`. Its HTTP status still reports
component availability, so it can return 200 while rendering is blocked. Rendering
must recover before HAPTIC can apply endpoint, route, certificate, or fetched-list
changes.
Retained server addresses can become stale even though HAProxy is Ready. The first
successful render resumes normal per-pod updates and clears the gauge after the
fleet converges.

A checkpoint is available after a controller supporting retention has observed
a successful deployment and published its acknowledgement. First installs and
upgrades blocked before that first deployment have no checkpoint. Recovery also
requires a running controller: if every replica fails the startup load gate,
existing HAProxy pods keep serving, but replacement pods can't obtain retained
configuration. Repair the startup error. Agents don't read Kubernetes or need
permission to read Secrets for recovery.

## Configuration

Add settings to your [complete Helm values file](../deploying-with-helm.md#change-settings)
and apply it with `helm upgrade`.

<a id="enable-leader-election"></a>

### Leader election defaults

Leader election is **enabled by default** in the Helm chart:

```yaml
# values.yaml (chart defaults)
controller:
  replicaCount: 2  # Run 2 replicas for HA
  config:
    controller:
      leaderElection:
        enabled: true
        leaseName: ""         # Defaults to the Helm release fullname
        leaseDuration: 30s    # Lease-expiry delay before takeover attempts
        renewDeadline: 20s    # Leader retries renewal for this long
        retryPeriod: 5s       # Interval between renewal attempts
```

!!! note
    The defaults allow brief API-server and CPU stalls without surrendering leadership. Shorter lease and renewal periods can reduce failover time but make the controller more sensitive to those stalls.

### Disable leader election

A single replica can keep leader election enabled. If you choose to disable it,
keep `controller.replicaCount: 1` and autoscaling off so two controllers can't
deploy independently:

```yaml
# values.yaml
controller:
  replicaCount: 1
  config:
    controller:
      leaderElection:
        enabled: false
```

### Timing parameters

The timing parameters control failover speed and tolerance:

| Parameter | Chart default | Purpose | Recommendations |
|-----------|---------------|---------|-----------------|
| `leaseDuration` | `30s` | Lease-expiry delay before takeover attempts | Increase for flaky networks (`60s`+) |
| `renewDeadline` | `20s` | How long leader retries before giving up | Must be < `leaseDuration` |
| `retryPeriod` | `5s` | Interval between leader renewal attempts | Should be < `renewDeadline` |

After a crash, a follower waits `leaseDuration` from its last observation of a
lease renewal before attempting election. Retry jitter and API latency add to
that delay; `leaseDuration + retryPeriod` isn't a hard upper bound. A voluntary
handoff releases the lease without waiting for expiry. HAProxy keeps serving its
current configuration while a new leader is elected.

## Deployment

<a id="standard-ha-deployment"></a>

### Standard high-availability Deployment

The [default Helm installation](../getting-started.md#install-with-helm) already
enables leader election and starts two controller replicas.

### Scaling

Set the replica count in your Helm values and apply them through your normal
deployment workflow:

```yaml
controller:
  replicaCount: 3
```

Keep at least two replicas for failover. Adding replicas increases the total
resource reservation; it doesn't reduce the work each replica performs.

### Autoscaling

The chart ships an optional HorizontalPodAutoscaler:

```yaml
controller:
  autoscaling:
    enabled: true
    minReplicas: 2      # keep at least 2 for failover
    maxReplicas: 10
    targetCPUUtilizationPercentage: 80
```

Adding controller replicas increases admission capacity and maintains more warm
standbys. Each replica renders, but only the leader deploys; replicas don't divide
one rendering workload between them. To increase traffic capacity, scale HAProxy
with `haproxy.keda` or `haproxy.replicaCount`.

<a id="rbac-requirements"></a>

The chart grants the Lease permissions needed for leader election and for the
leader's deployment record.

## Monitoring leadership

### Check current leader

The Lease resource is named after the chart `fullname` — `haptic` for `helm install haptic …`, but `<release>-haptic` when the release name doesn't contain `haptic`. Override by setting `controller.config.controller.leaderElection.leaseName`.

```bash
# List leases in the release namespace
kubectl get lease -n haptic

# View the lease for the haptic release
kubectl get lease -n haptic haptic -o yaml

# Output shows current leader:
# spec:
#   holderIdentity: haptic-controller-7d9f8b4c6d-abc12
```

The list also shows the `-retained-state` Lease, which holds the leader's
deployment record and has no holder.

### View leadership status in logs

```bash
# Leader logs show:
kubectl logs -n haptic deployment/haptic-controller | grep -E "leader|election"

# Example output:
# level=INFO msg="Leader election started" identity=pod-abc12 lease=<release>
# level=INFO msg="Became leader: pod-abc12" identity=pod-abc12
```

### Prometheus metrics

Monitor leader election via metrics endpoint:

```bash
kubectl port-forward -n haptic deployment/haptic-controller 9090:9090
```

In another terminal:

```bash
curl http://localhost:9090/metrics | grep leader_election
```

For a Prometheus setup that adds the Kubernetes `namespace` label, query the
default installation:

```promql
# Current leader (should be 1 across all replicas)
sum(haptic_leader_election_is_leader{namespace="haptic"})

# Identify which pod is leader
haptic_leader_election_is_leader{namespace="haptic"} == 1

# Leadership transition rate (should be low)
rate(haptic_leader_election_transitions_total{namespace="haptic"}[1h])
```

## Troubleshooting

Start with [fleet diagnostics](diagnostics.md) to identify the controller or
HAProxy pod that needs attention:

```bash
haptic doctor --namespace haptic
```

<a id="no-leader-elected"></a>
<a id="multiple-leaders-split-brain"></a>
<a id="frequent-leadership-changes"></a>
<a id="leader-not-deploying"></a>
<a id="leader-isnt-deploying"></a>

| Symptom | Check next |
| --- | --- |
| No leader and no new configurations deploying | Read controller logs for Lease permission errors, API connectivity failures, or missing pod identity. The chart supplies permissions and identity by default. |
| More than one reported leader | Restrict the metric query to one release. Its controller replicas must use the same Lease name and namespace. Separate releases each have a leader. |
| Frequent leadership changes | Check CPU throttling, memory pressure, node health, and API latency. Adjust lease timings only after identifying why renewal fails. |
| One leader, but HAProxy isn't updating | Check validation and deployment findings in `haptic doctor`; inspect agent connectivity and `minDeploymentInterval` if structural changes are waiting. |

Read recent logs from all controller replicas in the default release:

```bash
kubectl logs --namespace haptic \
  --selector app.kubernetes.io/instance=haptic,app.kubernetes.io/component=controller \
  --container controller --tail=100
```

For resource pressure, use the [sizing guide](performance.md). For rejected
configuration or failed deployment, follow [troubleshooting](../troubleshooting.md).

## Best practices

### Replica count

Keep at least two controller replicas when you need failover. A single replica
is sufficient when you can tolerate a controller outage; HAProxy continues
serving its last configuration during that outage.

The chart creates a PodDisruptionBudget with `minAvailable: 1` when
`controller.replicaCount > 1`. It limits voluntary disruption, such as a node
drain; it doesn't prevent an unexpected node failure. Configure it with:

```yaml
controller:
  podDisruptionBudget:
    enabled: true
    minAvailable: 1
```

### Resource allocation

Every replica renders changes and holds resource and render caches. The leader
also deploys configuration and writes status. Give all replicas enough CPU and
memory to handle peak load after election. Use the [resource sizing guide](./performance.md#controller-resource-sizing) for
starting requests and limits. Apply the same budget to every replica.

### Anti-affinity

Prefer separate nodes for the controller replicas and for the HAProxy replicas.
This example targets the `haptic` release:

```yaml
controller:
  podSpec:
    affinity:
      podAntiAffinity:
        preferredDuringSchedulingIgnoredDuringExecution:
          - weight: 100
            podAffinityTerm:
              labelSelector:
                matchLabels:
                  app.kubernetes.io/name: haptic
                  app.kubernetes.io/instance: haptic
                  app.kubernetes.io/component: controller
              topologyKey: kubernetes.io/hostname
haproxy:
  podSpec:
    affinity:
      podAntiAffinity:
        preferredDuringSchedulingIgnoredDuringExecution:
          - weight: 100
            podAffinityTerm:
              labelSelector:
                matchLabels:
                  app.kubernetes.io/name: haptic
                  app.kubernetes.io/instance: haptic
                  app.kubernetes.io/component: loadbalancer
              topologyKey: kubernetes.io/hostname
```

This is a scheduling preference, so it doesn't guarantee separate nodes when
capacity is limited. After rollout, check the `NODE` column and confirm that
each pair spans more than one node:

```bash
kubectl get pods --namespace haptic --selector app.kubernetes.io/instance=haptic -o wide
```

### Monitoring and alerts

The bundled `PrometheusRule` includes `HAProxyControllerNoLeader`. Enable it
through the [monitoring setup](./monitoring.md#enable-the-bundled-monitoring).

## Migration from single-replica

Keep your complete settings in [haptic-values.yaml](../deploying-with-helm.md#change-settings).
The chart already grants the Lease permissions needed for leader election.

1. If you previously disabled leader election, enable it while keeping one
   replica, then apply the values and wait for the rollout before scaling:

    ```yaml
    controller:
      replicaCount: 1
      config:
        controller:
          leaderElection:
            enabled: true
    ```

    ```bash
    helm upgrade haptic oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
      --version 0.8.0 --namespace haptic --values haptic-values.yaml
    kubectl rollout status deployment/haptic-controller -n haptic
    ```

2. Set `controller.replicaCount: 2` in the same values file and apply it:

    ```bash
    helm upgrade haptic oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
      --version 0.8.0 --namespace haptic --values haptic-values.yaml
    ```

3. Wait for both controller replicas to become ready:

    ```bash
    kubectl rollout status deployment/haptic-controller -n haptic
    ```

4. Confirm that the Lease names an active leader:

    ```bash
    kubectl get lease haptic -n haptic -o jsonpath='{.spec.holderIdentity}{"\n"}'
    ```

## See also

- [Monitoring Guide](./monitoring.md) - Prometheus metrics and alerting
- [Debugging Guide](./debugging.md) - Runtime introspection and troubleshooting
- [Security Guide](./security.md) - RBAC and security best practices
- [Performance Guide](./performance.md) - Resource sizing and optimization
- [Troubleshooting Guide](../troubleshooting.md) - General troubleshooting
