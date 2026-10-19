# ADR-0037: The termination drain waits for the routing, not for a quiet client

## Status

Withdrawn (2026-10-19). Implemented in #449 and #451, removed before any release.
The 0.6.0 quiet-period drain is back, with the limit below documented.

The check only helped a client that pauses longer than `quietPeriodSeconds` while
kube-proxy lags. It can't see the common production paths: `Local` policy behind a
cloud load balancer, load balancers that target pod IPs, and kube-proxy
replacements. Withdrawal on the load-balancer side isn't observable from the
cluster, and no surveyed project observes routing withdrawal (see *Prior art*).
For that, it gave internet-facing pods an API token, a cluster-wide Node `list`
grant, and node egress, polled every node per drain, and had already caused a
rollback regression (#451). The rest of this record is kept as history.

## Context

A terminating HAProxy pod runs the agent's drain from its `preStop` hook. Once the
drain ends, kubelet sends `SIGUSR1` and HAProxy's soft stop unbinds the listeners.
Until #446 the drain ended after `quietPeriodSeconds` (2 s) without a new
connection, on the assumption that silence meant kube-proxy had stopped routing to
the pod. When the client pauses, silence means nothing about the routing: with
kube-proxy frozen, a 3 s client pause ended the drain after 2.003 s while iptables
still forwarded to the pod, and 23 of 72 later requests were refused. With a
continuous client, none failed.

The pod can't see kube-proxy's rules, and Kubernetes publishes no per-node "this
EndpointSlice change is programmed" signal. The facts this decision rests on
(sources under [References](#references)):

- **EndpointSlice.** Deleting a pod sets its endpoint to `ready=false`,
  `terminating=true`, `serving` from its readiness. kube-proxy (KEP-1669, GA 1.28)
  uses only ready endpoints, but falls back to serving-terminating ones when a
  Service has no ready endpoint, separately for `Cluster` and, per node, for
  `Local` traffic policies. `publishNotReadyAddresses` keeps `ready=true`.
- **kube-proxy healthz (`:10256`).** The top-level `lastUpdated` is overwritten
  with the current time whenever kube-proxy is healthy (since v1.17). Since v1.33,
  `status.<IPv4|IPv6>.lastUpdated` is the completion time of the last successful
  rule sync. A sync starts on a Service or EndpointSlice change, never earlier than
  `minSyncPeriod` (1 s) after the previous one; iptables and nftables otherwise
  resync only hourly.
- **The sync time is not a version.** A sync snapshots pending changes when it
  starts and stamps the time when it ends. A change that reaches kube-proxy while
  an unrelated sync runs isn't in that sync, but its time is later than the
  change; kube-proxy then runs another sync for it at most `minSyncPeriod` later.
- **Prior art.** Kubernetes' own answer is a timer (KEP-3960's `preStop` sleep).
  ingress-nginx, Istio, Traefik, Skipper, Linkerd, and the AWS and GKE load
  balancer integrations also wait a fixed time; Contour and Istio's
  `EXIT_ON_ZERO_ACTIVE_CONNECTIONS` count connections; Knative's drainer is
  HAPTIC's old quiet period with 45 s. None observes kube-proxy.

## Decision

The drain ends when **both** hold:

1. **Nothing observable routes new connections to the pod.** The agent watches
   the EndpointSlices and Services of its namespace that kube-proxy programs (not
   headless ones, nor those another proxy owns) for the pod's whole life and
   records when each Service's slices last changed. The routing is withdrawn
   when, for every Service whose slices list the pod:
    - the pod's endpoint is not ready;
    - another endpoint of the same address family is ready, so kube-proxy has no
      reason to fall back to this pod (only while the pod still serves); and
    - with a `Local` external (`NodePort`, `LoadBalancer`) or internal traffic
      policy, another ready endpoint runs on the pod's node.

   Then the kube-proxy on every node must report, for each address family the pod
   is listed in, a rule sync that completed after the last change to those
   Services' slices reached the agent. The agent compares
   ages rather than instants: the age of the node's last sync in the node's clock
   against the age of the change in its own clock, at the midpoint of the request.
   Node clock skew cancels out to within half a round trip.
2. **No new connection arrived for `quietPeriodSeconds`.** The old signal stays as
   an additional condition. It covers connections that kube-proxy forwarded just
   before its sync and paths the agent can't observe.

`maxWaitSeconds` bounds the drain as before; it ends with reason `bound` if the
routing never goes. The drain reports `routed-away` when everything was observed,
and `unverified` when it fell back. Any drain that couldn't observe part of the
routing, including one that ends at the bound, logs at `WARN` with what it
couldn't see.

### What the agent can't observe

It never assumes a path is withdrawn. A path it can't observe is named in the
`unverified` result, and the quiet period alone decides for it:

| Situation | Signal | Drain behaviour |
| --- | --- | --- |
| No slice in the namespace lists the pod, or the watch hasn't synced | none | `unverified`: quiet period |
| A listing Service's traffic policy can't be read | that Service's `Local` fallback | `unverified`: nodes still awaited, then quiet period |
| A node's `:10256` refuses, the connection fails, or something other than kube-proxy healthz answers | that node unobservable | `unverified`: the other nodes are still awaited, then quiet period |
| kube-proxy older than v1.33 (no per-family `status`) | that node unobservable | as above |
| kube-proxy accepts the connection but doesn't answer (stopped, overloaded) | node pending | waits; `bound` at worst |
| kube-proxy replaced (Cilium, Calico eBPF) | `:10256` absent or not kube-proxy | `unverified`: quiet period |
| `externalTrafficPolicy: Local` with no other ready pod on the node | kube-proxy still routes to the pod | waits until `bound` |
| External load balancers that target pod IPs directly | not observed, and not reported | quiet period only |

Turning the routing check off (`haproxy.drain.kubeProxyHealthzPort: 0`, or a
custom `haproxy.lifecycle`) reverts to the quiet period alone, reason `quiet`.

### Access

The HAProxy pods get their own ServiceAccount with `automountServiceAccountToken:
false`; only the agent container mounts a projected token. The grants are `list`
and `watch` on `endpointslices` and `services` in the release namespace and `list`
on `nodes`. The HAProxy NetworkPolicy allows the controller's API-server egress and
TCP to `kubeProxyHealthzPort` at `haproxy.networkPolicy.kubeProxyCIDRs`.

## Consequences

- A paused client no longer ends the drain while kube-proxy still forwards to the
  pod; `TestHAProxyPodDeletionWaitsForKubeProxy` freezes kube-proxy, pauses the
  client past the quiet period, and requires every request to succeed and the
  drain to end `routed-away`.
- **Residual race.** kube-proxy's sync time isn't tied to a resource version. If
  an unrelated sync is running when the change reaches kube-proxy, and finishes
  after the agent received the change, the agent confirms that node one sync early.
  The following sync comes at most `minSyncPeriod` plus one sync duration later;
  the quiet period still has to pass. Closing this needs a per-node programmed
  generation, which Kubernetes doesn't publish.
- **Slow confirmations hold, they don't fail.** If kube-proxy synced before the
  agent's own watch delivered the change, no later sync exists to confirm it until
  the next change or the hourly resync, and the drain runs to `bound`. Clients are
  served for longer; nothing is refused.
- The last ready pod of a Service, or a `Local`-policy pod without a local peer,
  drains for the full `maxWaitSeconds`: kube-proxy routes to it for exactly that
  long. Scale-to-zero and `Recreate` rollouts take `maxWaitSeconds` per pod.
- The data plane gains an API token and read access to EndpointSlices, Services,
  and Nodes, plus egress to the API server and to every node's `:10256`. These are
  read-only; the token is in the agent container alone, and the HAProxy container
  no longer receives the namespace's default token.
- Every HAProxy pod holds a namespace-wide EndpointSlice and Service watch.

## Alternatives considered

- **A longer or adaptive quiet period, a `preStop` sleep, HAProxy `grace`.** Each
  guesses kube-proxy's timing and replaces HAProxy's soft stop with a timer.
  Rejected as a standing project rule.
- **Probe the pod through its Service ClusterIP.** It exercises only the local
  node's kube-proxy, is probabilistic (each probe picks a random endpoint), and
  can't distinguish "not selected this time" from "withdrawn".
- **Poll kube-proxy's metrics** (`last_queued` ≤ `last_timestamp`). They bind to
  `127.0.0.1:10249` and aren't reachable from a pod; reading them would need node
  access. They also have the same race as the healthz times.
- **The controller observes and signals the agent.** One watcher for the fleet, no
  token in the data plane, but a controller outage or rollout disables every drain
  in that window. That's the moment a `helm upgrade` rolls both, so the agent
  observes for itself.
- **Use the EndpointSlice change time as the anchor.** `managedFields` carries
  only seconds, and `last-change-trigger-time` is the pod's transition time, not
  the slice write. Both compare against node clocks; the agent's age comparison
  needs neither.

## References

- [EndpointSlice conditions](https://kubernetes.io/docs/concepts/services-networking/endpoint-slices/#conditions)
- [KEP-1669: proxy terminating endpoints](https://github.com/kubernetes/enhancements/tree/master/keps/sig-network/1669-proxy-terminating-endpoints),
  [`CategorizeEndpoints`](https://github.com/kubernetes/kubernetes/blob/master/pkg/proxy/topology.go)
- [KEP-3960: `preStop` sleep action](https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/3960-pod-lifecycle-sleep-action)
- [kube-proxy health server](https://github.com/kubernetes/kubernetes/blob/master/pkg/proxy/healthcheck/proxy_health.go),
  [iptables proxier](https://github.com/kubernetes/kubernetes/blob/master/pkg/proxy/iptables/proxier.go),
  [per-family status (kubernetes#129271)](https://github.com/kubernetes/kubernetes/pull/129271)
- [Network programming latency SLI](https://github.com/kubernetes/community/blob/master/sig-scalability/slos/network_programming_latency.md),
  which rules out per-change tracking
- [Envoy draining](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/operations/draining),
  [Contour shutdown manager](https://projectcontour.io/docs/main/redeploy-envoy/),
  [Knative drainer](https://github.com/knative/pkg/blob/main/network/handlers/drain.go)
