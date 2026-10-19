# Configure HAProxy pods and access

<a id="overview"></a>

The chart deploys two HAProxy replicas by default. Configure [Service access](#haproxy-service),
[replicas and autoscaling](#replicas-and-autoscaling), and [resource budgets](#resource-limits)
through Helm values. Use `haproxy.podSpec` for scheduling, volumes, and other pod
settings. Add settings to your [complete Helm values file](deploying-with-helm.md#change-settings).
You can also [manage the pods separately](#haproxy-pod-requirements).

## Resource limits

Use the [resource sizing guide](operations/performance.md#controller-resource-sizing) to budget the installation. Set HAProxy resources with `haproxy.resources` and agent resources with `haproxy.agent.resources`.

<a id="service-architecture"></a>

## Expose traffic and management ports

Separate Services expose controller health and metrics, the admission webhook, HAProxy traffic and stats, and the HAPTIC agent.

### Controller Service

The controller's `ClusterIP` Service exposes health and metrics ports. For a
release named `haptic`, the Service is also named `haptic`. Other release names
use the chart's `fullname`, usually `<release>-haptic`:

| Name | Container port | Values key | Purpose |
|------|----------------|------------|---------|
| `healthz` | 8080 | `controller.ports.healthz` | Single source for the process listener, liveness/readiness probes, Service, and `/debug/*` introspection endpoints |
| `metrics` | 9090 | `controller.ports.metrics` | Single source for the process listener, Service, and Prometheus monitors; `0` disables metrics |

The separate `<fullname>-webhook` Service exposes port `443` (`controller.webhook.service.port`) and forwards to port `9443` (`controller.ports.webhook`).

Configure the health and metrics Service under `controller.service`:

```yaml
controller:
  service:
    type: ClusterIP
    annotations: {}
```

### HAProxy Service

The `<fullname>-haproxy` Service sends traffic to HAProxy pods. For a release
named `haptic`, it's `haptic-haproxy`. It uses `NodePort` by default; configure
its ports through `haproxy.service.*` and pod ports through `haproxy.ports.*`:

| Name | Service port | Container port | nodePort default |
|------|--------------|----------------|------------------|
| `http` | 80 | 80 | 30080 |
| `https` | 443 | 443 | 30443 |
| `https-quic` (UDP) | 443 | 443 | 30443 |
| `stats` | 8404 | 8404 | 30404 |

`https-quic` carries [HTTP/3](#http3-quic). It follows the `https` entry's port
and nodePort, and setting `haproxy.service.https.port: 0` drops both.

The agent gets its own internal-only `ClusterIP` Service (`<fullname>-haproxy-dataplane`, for example `<release>-haptic-haproxy-dataplane`) on port 5555. Its type comes from `haproxy.agent.service.type`.

**Local access**, including kind clusters, works with `kubectl port-forward`
regardless of Service type. For a release named `haptic` in namespace `haptic`:

```bash
kubectl port-forward -n haptic service/haptic-haproxy 8080:80
```

In another terminal, send requests to `http://localhost:8080` with the hostname
configured on your Ingress. HTTP and HTTPS Gateways use
[their own Services](gateway-api.md#step-4-test-the-routing); port-forward to
the Gateway's Service to reach its listeners. NodePort access from the host also
requires a reachable node address or matching kind port mappings.

**LoadBalancer access** requires a load-balancer implementation in your cluster.
Set the Service type and any annotations required by that implementation:

```yaml
haproxy:
  service:
    type: LoadBalancer
```

**External / self-managed HAProxy** — turn off the chart's HAProxy deployment and manage pods yourself (see [HAProxy Pod Requirements](#haproxy-pod-requirements)):

```yaml
haproxy:
  enabled: false
```

### HTTP/3 (QUIC)

HTTP/3 is on by default. Every HTTPS listener that terminates TLS, including
HTTPS listeners on Gateways, also accepts QUIC on the same port number over UDP.
It uses the same certificates, client-certificate validation, and session
tickets as the TCP listener. HTTPS responses carry an `alt-svc` header, so
browsers and other HTTP/3 clients switch to QUIC on their next request:

```http
alt-svc: h3=":443"; ma=86400
```

HAProxy advertises the port the client used for HTTPS, read from the request's
`Host` header, or 443 when `Host` has no port. That's correct whenever UDP is
published on the same port as TCP, which the chart does: the HAProxy Service,
each Gateway's Service, the container, and the NetworkPolicy all add a UDP port
next to every HTTPS port.

**Allow UDP to HAProxy.** Firewalls, security groups, and load balancers in
front of the HAProxy Service must pass UDP on the HTTPS port (443 by default),
or on the nodePort when clients reach a `NodePort` Service directly. Clients
that can't reach UDP keep using HTTP/2 or HTTP/1.1 over TCP. A cloud load
balancer has to support Services that mix TCP and UDP ports, which Kubernetes
supports since 1.26.

If your load balancer publishes UDP on a different port than TCP, advertise
that port:

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        http3:
          altSvc:
            port: 8443      # UDP port clients reach
            maxAge: 86400   # seconds a client remembers the advertisement
```

To turn HTTP/3 off, which removes the UDP listeners, the UDP Service ports, and
the `alt-svc` header:

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        http3:
          enabled: false
```

Clients cache the advertisement for up to `maxAge` seconds. After you turn
HTTP/3 off, a client with a cached entry tries QUIC, fails, and falls back to
TCP.

HAProxy doesn't add HTTP/3 in these cases:

- **The [PROXY protocol](#proxy-protocol) listener.** QUIC can't carry a PROXY
  header, so an HTTP/3 client would reach HAProxy without its real address.
  Responses on `proxyProtocol.httpsPort` carry no `alt-svc` header.
- **Gateway listeners with `tls-max-version` below 1.3.** QUIC requires TLS 1.3,
  so such a listener stays TCP-only and isn't advertised.
- **TLS-Passthrough and TLS-Terminate (`TLSRoute`) listeners.** They forward TCP
  streams, not HTTP.

The bundled HAProxy images of every
[supported series](operations/haproxy-versions.md#supported-versions) are built
with QUIC support. A [self-managed HAProxy](#haproxy-pod-requirements) needs a
QUIC-capable build too; if `haproxy -vv` doesn't list `QUIC`, set
`http3.enabled: false`.

### PROXY protocol

If an upstream load balancer replaces the client address, HAProxy sees that
balancer as the source. Access logs, source-IP rate limits, and IP-based policies
then use the address of the balancer.

The load balancer fixes this by adding a PROXY protocol header that carries the
original address. Enable the matching listeners:

```yaml
controller:
  config:
    templatingSettings:
      extraContext:
        proxyProtocol:
          enabled: true
          httpPort: 8081
          httpsPort: 8444
```

That adds two binds, adds them to the HAProxy Service and the NetworkPolicy, and
leaves `haproxy.ports.http` / `haproxy.ports.https` exactly as they were. Point
the balancer at the new ports:

```haproxy
# On the upstream load balancer
server k8s-https 10.0.0.50:8444 send-proxy-v2
```

Requests arriving on the PROXY ports carry the real client through the access
log's `client_ip`, `src`-keyed rate limiting, the WAF, and IP access control
lists. Terminated HTTPS on `httpsPort` uses the same certificates, ciphers, and
protocol negotiation as the plain HTTPS bind; with TLS-Passthrough configured,
`httpsPort` attaches to the SNI-routing frontend instead so passthrough hosts
keep working.

!!! warning "Send the header, or the connection is dropped"
    HAProxy has no "PROXY header optional" mode. A connection reaching
    `httpPort` or `httpsPort` without the header is rejected, so only the
    upstream balancer may target these ports. Everything else — direct access,
    in-cluster clients, NodePort traffic, probes — keeps using the regular
    `haproxy.ports.http` / `haproxy.ports.https`, which is why these are
    additional ports rather than a flag on the existing ones.

This is separate from the
[`haproxy-haptic.org/proxy-protocol`](libraries/haptic-annotations.md)
annotation, which makes HAProxy *send* a PROXY header to a backend. For an L7
load balancer that sets `X-Forwarded-For` instead, see
[forwarded headers](operations/security.md#trust-an-l7-load-balancer).

### Full HAProxy Service reference

```yaml
haproxy:
  enabled: true
  ports:
    http: 80         # HAProxy container HTTP bind
    https: 443       # HAProxy container HTTPS bind
    stats: 8404      # Stats/health page
    dataplane: 5555  # HAPTIC agent
  service:
    type: NodePort   # ClusterIP, NodePort, or LoadBalancer
    annotations: {}
    loadBalancerIP: ""
    loadBalancerSourceRanges: []
    externalTrafficPolicy: ""   # Cluster | Local
    http:
      port: 80
      nodePort: 30080           # Only honored for NodePort/LoadBalancer
    https:
      port: 443
      nodePort: 30443
    stats:
      port: 8404
      nodePort: 30404
```

## Replicas and autoscaling

The chart runs 2 HAProxy replicas by default. Set `haproxy.replicaCount` to change the fixed count:

```yaml
haproxy:
  replicaCount: 3
```

For traffic-driven autoscaling, enable [KEDA](https://keda.sh/) under `haproxy.keda`. When `haproxy.keda.enabled` is true, the chart creates a `ScaledObject` and stops writing a fixed `replicas` onto the Deployment (KEDA owns it), scaling between `minReplicaCount` and `maxReplicaCount` from the triggers you define:

```yaml
haproxy:
  keda:
    enabled: true
    minReplicaCount: 2
    maxReplicaCount: 10
    triggers:
      - type: cpu
        metricType: Utilization
        metadata:
          value: "70"
```

KEDA must be installed in the cluster, and `haproxy.keda.triggers` must list at least one trigger — it's empty by default. Any [KEDA scaler](https://keda.sh/docs/latest/scalers/) works; the block above uses CPU utilization.

## Initial bootstrap config

New HAProxy pods start with a minimal configuration from `haproxy.initialConfig`,
stored in the `<fullname>-haproxy-config` ConfigMap. For the default release name,
that's `haptic-haproxy-config`. The controller replaces it with your rendered
configuration.

The default keeps `/healthz` returning 200 on the stats port and `/ready` returning 503 ("waiting for controller config"), so the pod stays NotReady until the controller applies its first real config.

Once the agent receives and successfully loads the rendered configuration,
`/ready` returns `200` and Kubernetes adds the pod to the Service. If that first
apply fails, the agent restores the bootstrap files and the pod stays unready.

On a later container restart, HAProxy reuses the configuration on disk if it
passes `haproxy -c`. It falls back to the bootstrap configuration only if the
file is missing or invalid.

If you replace `haproxy.initialConfig`, preserve the worker socket, `/healthz`,
and a `/ready` response of `503`. Returning `200` would send application traffic
to a pod before its routes exist. The value supports Helm template expressions;
changing it rolls the HAProxy pods on the next upgrade.

## Access logging

<a id="core-fields"></a>
<a id="add-your-own-fields"></a>
<a id="where-the-logs-go"></a>
<a id="the-access-log-is-lossy-under-back-pressure"></a>
<a id="why-a-ring-for-a-sidecar"></a>
<a id="dropping-records-you-dont-need"></a>
<a id="vector-sidecar"></a>
<a id="how-the-config-reaches-it"></a>
<a id="request-ids"></a>
<a id="contribute-a-field-from-your-own-library"></a>
<a id="change-the-destination-or-the-whole-format"></a>

Read, customize, and forward request logs with the [access logging guide](operations/access-logging.md).
It also covers request IDs, the Vector sidecar, and monitoring dropped records.

## Pod readiness and restarts

Each pod runs HAProxy in master-worker mode plus the HAPTIC agent, which owns
the pod's file tree and its runtime sockets. The chart supervises the SPOA hub
and Vector processes inside their sidecar containers: a child exit or repeated
failed health check leaves HAProxy running while the supervisor restarts only
that child, with a backoff capped at 30 seconds.

HAProxy's `/ready` endpoint controls pod readiness. It returns `503` while the
bootstrap configuration is active and `200` once a rendered configuration runs.
The agent, SPOA hub, and Vector run as native sidecars: Kubernetes starts them
before HAProxy and stops them after HAProxy exits, preserving dependencies during
connection draining.

The agent's `/readyz` endpoint reports whether it can accept configuration updates;
a rejected update doesn't make it unready. Its liveness probe uses the local Unix
socket. A stopped container or a failing probe on a custom sidecar can still make
the pod unready.

The watchdog uses `/usr/bin/bash` and `timeout`, which the default images provide.
With a custom sidecar image missing either command, the supervisor logs a warning
and still restarts child processes that exit.

## Pod termination

When a HAProxy pod is deleted, the agent holds the HAProxy container's `preStop`
hook until no Service routes new connections to the pod any more. Then kubelet
sends `SIGUSR1`, and HAProxy's soft stop closes the listeners and finishes the
connections in flight within `extraContext.hardStopAfter`.

The drain ends when all of these hold, and after `haproxy.drain.maxWaitSeconds`
(10 s) at the latest:

- Every Service that selects the pod lists it as not ready, and has another ready
  endpoint. With a `Local` traffic policy, that endpoint must run on the same node.
- kube-proxy on every node reports a rule sync that finished after that change
  reached the agent.
- No new connection reached the pod for `haproxy.drain.quietPeriodSeconds` (2 s).

The agent reads kube-proxy's sync time from its health endpoint on each node
(`haproxy.drain.kubeProxyHealthzPort`, default `10256`, kube-proxy v1.33 or
later). For that, the HAProxy pods get their own ServiceAccount, read access to
EndpointSlices and Services in the release namespace and to Nodes, and egress to
the API server and to that port; see [networking](operations/networking.md#default-configuration).
Only the agent container mounts the token.

The agent logs `drain finished` with a `reason`:

| Reason | Meaning |
| --- | --- |
| `routed-away` | The routing was withdrawn everywhere the agent could check, and the pod was quiet. |
| `unverified` | Logged at `WARN`. Part of the routing couldn't be observed; `routing` names it. The quiet period alone decided for that part. |
| `bound` | The drain reached `maxWaitSeconds`; `routing` says what still routed to the pod. Logged at `WARN` if part of the routing couldn't be observed. |
| `quiet` | The routing check is off; only the quiet period counted. |

The agent can't confirm the routing in these cases, and falls back to the quiet
period with an `unverified` drain:

- kube-proxy is replaced, for example by Cilium or Calico eBPF, or is older than
  v1.33.
- A node's port `10256` is unreachable, for example because of a firewall.
- An external load balancer sends traffic to pod IPs directly. The agent doesn't
  see when it stops; this case isn't logged as `unverified`.

Some pods still receive traffic at `maxWaitSeconds`, and HAProxy stops anyway:
the last ready pod of a Service, and a pod whose Service has
`externalTrafficPolicy: Local` without another ready HAProxy pod on its node.
kube-proxy keeps routing to such a pod until it's gone.

To turn the routing check off, set `haproxy.drain.kubeProxyHealthzPort: 0`. Clients
that pause for longer than the quiet period can then be refused while kube-proxy
still routes to the pod.

The design and its limits are in
[ADR-0037](https://github.com/haproxy-haptic/HAPTIC/blob/main/docs/adr/0037-drain-observes-routing.md).

## HAProxy Pod requirements

When `haproxy.enabled: false`, you're responsible for deploying HAProxy pods yourself. The controller discovers them via the pod selector at `controller.config.podSelector`, which defaults to:

```yaml
controller:
  config:
    podSelector:
      matchLabels:
        app.kubernetes.io/component: loadbalancer
        app.kubernetes.io/name: haptic        # set dynamically by the chart
        app.kubernetes.io/instance: haptic  # your Helm release name
```

If your existing HAProxy pods don't have those exact labels, either relabel them or override `controller.config.podSelector.matchLabels` to match.

Each discovered pod must:

1. **Carry labels matching `podSelector.matchLabels`**
2. **Run HAProxy in master-worker mode** with a master socket the agent can reload through, and a worker `stats socket` it can run runtime commands on
3. **Run the agent** in the same pod, from the HAPTIC image, sharing the config volume with HAProxy
4. **Expose the agent** on `haproxy.ports.dataplane` (default 5555), mounting its TLS identity and trusting the controller identity; see [agent certificates](operations/agent-certificates.md)
5. **Run the same HAProxy major.minor series as `haproxyVersion`** so the controller validates configuration with the matching binary
6. **Keep the agent available during termination** with native-sidecar ordering and the drain hook used by the chart
7. **Provide every dependency selected by your templates**, including SPOA plugins and log sockets when enabled

<a id="example-haproxy-pod-deployment-byo-haproxy"></a>

### Start from the deployment for your installed version

A separate workload controller must preserve the agent's TLS mounts, shared
volumes, runtime sockets, bootstrap configuration, probes, and termination
ordering. Use the chart-generated deployment for your version as the reference;
a minimal HAProxy-plus-agent manifest omits dependencies enabled by default.

For an existing `haptic` release in namespace `haptic`, export its deployment
for inspection:

```bash
kubectl get deployment haptic-haproxy -n haptic -o yaml > haptic-haproxy-reference.yaml
helm get manifest haptic -n haptic > haptic-release-reference.yaml
```

The second file includes the associated bootstrap ConfigMaps and Services.
Identity Secrets are managed separately, as described in
[agent certificate management](operations/agent-certificates.md).
Set `haproxy.enabled: false` only once your external workload definition supplies
these dependencies and matches the configured pod selector. Helm removes its
Deployment when you disable it; plan that ownership change as a rollout.
