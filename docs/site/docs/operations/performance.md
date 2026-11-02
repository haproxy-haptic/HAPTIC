# Resource sizing and performance

For a default installation, reserve **about 1 CPU core and 5.4 GiB of memory**
across four pods: two controllers and two HAProxy pods, including their sidecars.
These are Kubernetes resource requests, not constant CPU or memory consumption.
The containers can use spare CPU when busy; their combined memory limits are
**7.5 GiB**.

## Controller resource sizing

### Starting budgets {#recommended-resources}

Size each controller for the routes and backend resources it watches. These
are rough planning estimates for the bundled templates, with one or a few
backends per route. Installation totals include two controllers, two HAProxy
pods, and their default sidecars:

| Routing workload | CPU request per controller | Memory request and limit per controller | Total installation requests: CPU / memory |
| --- | ---: | ---: | ---: |
| Small installation, up to roughly 100 routes | `500m` (chart default) | `1Gi` (chart default) | 1.85 cores / 5.4 GiB |
| Hundreds of routes, approaching 1,000 | `500m` | `2Gi` | 1.85 cores / 7.4 GiB |
| Several thousand routes, around 3,000 | `1` | `4Gi` | 2.85 cores / 11.4 GiB |

These are starting estimates, not capacity limits. Beyond 3,000 routes, add
roughly `1Gi` per controller for every further 1,000 routes, then confirm the
limit with the restart check below.
Many endpoints per Service, large certificates, custom templates, and frequent
configuration changes can need more resources at the same route count. The
totals exclude your applications, Kubernetes system components, and monitoring
services. Extra HAProxy replicas and optional services add to these budgets.

For an installation with hundreds of routes, start with these Helm values.
Merge them into your [complete values file](../deploying-with-helm.md#change-settings)
and apply it with Helm:

```yaml
controller:
  resources:
    requests:
      cpu: 500m
      memory: 2Gi
    limits:
      memory: 2Gi
```

Set memory through the container's Kubernetes requests and limits. Leave CPU
limits unset unless your cluster requires them, so startup and bursts of
configuration changes can use spare node CPU.

Don't lower the CPU request below the `500m` default. Each controller start
validates the configuration before the pod becomes ready, which takes about 50
CPU-seconds and must finish within 4 minutes. On a busy node, the request is
the CPU the controller can count on.

Keep the default `1Gi` controller memory budget even for a small route set:
startup also compiles templates and validates the configuration. Setting memory
requests equal to limits reserves that memory when Kubernetes schedules the pod.

### Size memory for a restart {#size-memory-for-a-restart}

A controller needs the most memory while it starts, when it renders every route
at once. Running controllers use far less, so a limit that looks generous during
normal operation can still be too small for a restart. With 3,000 Ingresses, a
restarting controller peaked at about 2.9 GiB, while running controllers used
about 0.8 GiB of Go heap.

If the limit is too small, a restarting controller is `OOMKilled` on every
attempt and never renders the configuration. After you change controller
resources or add many routes, restart the controllers one at a time and check
that the rollout completes:

```bash
kubectl rollout restart deployment/haptic-controller -n haptic
kubectl rollout status deployment/haptic-controller -n haptic --timeout=10m
kubectl get pods -n haptic -l app.kubernetes.io/component=controller
```

The rollout must finish with no controller restarts. If a controller shows
`OOMKilled` in `kubectl describe pod`, raise its memory request and limit.

## What the default installation reserves

These values include the default Gateway API, request logging, and metrics
features. CPU uses Kubernetes units (`1000m` = one core).

| Container | Copies | CPU request each | Memory request each | Memory limit each |
| --- | ---: | ---: | ---: | ---: |
| Controller | 2 | `500m` | `1Gi` | `1Gi` |
| Configuration validator | 2 | `25m` | `64Mi` | `256Mi` |
| HAProxy | 2 | `250m` | `1Gi` | `1Gi` |
| HAPTIC agent | 2 | `50m` | `256Mi` | `256Mi` |
| Vector log and metrics collector | 2 | `50m` | `256Mi` | `1Gi` |
| SPOA plugin hub | 2 | `50m` | `128Mi` | `256Mi` |
| **Total** | **4 pods** | **`1850m`** | **`5.375Gi`** | **`7.5Gi`** |

Allow room for rolling upgrades. With these defaults, one extra controller pod
and one extra HAProxy pod bring memory requests to about **8.1 GiB** during
rollout. Before rollout, the temporary validation Job requests `200m` CPU and
`512Mi` memory, with a `1Gi` memory limit. If you increase controller or HAProxy
resources, budget for an extra pod of each at the increased size. A cluster
filled to its steady-state reservation may have no room to complete an upgrade.

Optional services add to the total:

| Feature | Additional default reservation |
| --- | --- |
| Shared response cache | `100m` CPU and `384Mi` memory per Varnish pod |
| Shared rate-limit store | Three Valkey/Sentinel pods, together `225m` CPU and `576Mi` memory |
| Custom sidecars | The requests you configure for those containers |

See [response caching](response-cache.md) and
[shared rate limiting](spoa-hub.md#managed-shared-rate-limit-store) before enabling
those services. Their memory use depends on cache size and active keys.

## Size HAProxy for traffic {#haproxy-optimization}

Route count chiefly affects the controller. Requests per second, concurrent
connections, TLS handshakes, compression, and web application firewall (WAF)
inspection determine traffic capacity. Start with the chart's **two HAProxy replicas**, each requesting
`250m` CPU and `1Gi` memory, plus the sidecars listed above.

For a busy edge service, reserve more CPU for HAProxy and add replicas as traffic
grows. For example:

```yaml
haproxy:
  replicaCount: 3
  resources:
    requests:
      cpu: 1
      memory: 1Gi
    limits:
      memory: 1Gi
```

A short plaintext request and a WAF-inspected upload have different costs, so these budgets don't specify a requests-per-second
capacity. Use the [symptoms below](#when-to-adjust-the-budget) to decide when to
increase resources during normal operation.

### Scaling strategies

Increase `haproxy.replicaCount` for more traffic capacity, or configure
[HAProxy autoscaling](../haproxy-deployment.md#replicas-and-autoscaling).
Keep at least two replicas for availability. Include the agent, Vector, and SPOA
hub in the budget for every additional HAProxy pod.

Increase `controller.resources` when configuration changes become slow or a
controller runs out of memory. Adding controller replicas provides failover and
more admission capacity; it doesn't divide the rendering workload between them.
See [high availability](high-availability.md).

### Response compression

Response compression is off by default. It saves bandwidth and costs HAProxy
CPU. Set `haproxy-haptic.org/compress-enable: "true"` on an Ingress to enable it
for eligible responses.
See [compression settings](../libraries/haptic-annotations.md#compression)
for content types, CPU limits, and response-safety considerations.

### Password hash performance

Large basic-auth user lists and expensive password hashes increase
authentication, configuration validation, and reload costs. Keep the password
protection your security policy requires. For large user sets, consider
[external authentication](spoa-hub.md#what-each-plugin-does) instead of weakening
hashes to fit a CPU budget.

## When to adjust the budget

Enable the [bundled monitoring](monitoring.md) and watch these symptoms during
ordinary operation:

| Symptom | First action |
| --- | --- |
| Controller is `OOMKilled`, or memory repeatedly approaches its limit | Increase `controller.resources.requests.memory` and `limits.memory` together. |
| Configuration changes lag while controller CPU stays busy | Increase its CPU request; check CPU throttling if you set a limit. |
| Traffic latency rises while HAProxy CPU stays busy | Increase HAProxy's CPU request or replica count. |
| Vector is `OOMKilled` or drops access-log records | Increase `vector.resources`; reduce unused metric labels as described in [monitoring](monitoring.md#controlling-cardinality). |
| WAF processing is saturated | Increase `spoaHub.resources`; inspect hub timeout and queue metrics before raising timeouts. |

CPU requests reserve scheduling capacity; they don't cap usage. Memory limits
cap each container, so available memory elsewhere in the pod doesn't prevent an
individual container from being killed. Include startup and rolling upgrades
when checking memory headroom.

### Resource watching optimization

The bundled chart already fetches Secrets on demand and ignores common noisy
metadata. For custom watches, use on-demand storage for large objects you read
occasionally and indexes for lookups. Preserve every field your templates need.
See [watching resources](../watching-resources.md) for settings and examples.

### Template debugging

If a custom template makes configuration changes slow, use
`haptic validate --file config.yaml --schema-dir ./schemas --trace-templates` to identify expensive
snippets. This needs your complete configuration and the
[schemas for offline validation](../validation-tests.md#prepare-schemas). For a running installation,
start with [fleet diagnostics](diagnostics.md).

## Reconciliation tuning

Keep the chart's timing defaults unless configuration delivery is missing your
latency target. Most watches batch updates within `100ms`; the bundled
EndpointSlice watch has no debounce delay so backend address changes arrive
promptly. A render already in progress can still delay a subsequent change.

### Deployment pacing

The chart spaces reloads of an individual HAProxy pod at least **5 seconds**
apart. Runtime updates don't wait for this reload interval. Raising it reduces
reload frequency but delays changes that require a reload.

```yaml
controller:
  config:
    dataplane:
      minDeploymentInterval: 5s
```

See [supported runtime updates](../supported-configuration.md) for which changes
need a reload and the [dataplane reference](../crd-reference.md#dataplane)
for other timing controls.

### Graceful reload drain bound

After a reload, HAProxy lets the old worker finish existing connections for up
to **60 seconds** by default, then closes those still open. Long-lived streams
need a longer drain window if they must survive reloads:

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        hardStopAfter: 5m
haproxy:
  podSpec:
    terminationGracePeriodSeconds: 330
```

The pod termination budget must also cover the drain window, up to 10 seconds
for traffic to stop arriving, and sidecar shutdown. The chart defaults to
90 seconds; the example above provides a five-minute drain window during a
reload or a graceful pod replacement.

Longer windows retain old workers and their memory for longer. Connections still
open at the deadline close, so applications with indefinite streams must support
reconnection. An empty string disables the reload bound; Kubernetes still enforces
the pod termination budget. If you replace `haproxy.initialConfig`, also set
`hard-stop-after` in that custom bootstrap configuration.
