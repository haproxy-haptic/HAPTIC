# Networking

<a id="overview"></a>

Use NetworkPolicy to control which services the controller and HAProxy pods can
reach, and which clients can reach them. The chart creates policies by default;
review the allowed destinations below before restricting access further.

To expose application traffic through a Service or external load balancer, see
[HAProxy access](../haproxy-deployment.md#haproxy-service).

Add the values below to your [complete Helm values file](../deploying-with-helm.md#change-settings)
and apply it with Helm. Your cluster needs a network plugin that enforces
NetworkPolicy.

## Default configuration

The controller policy allows:

- DNS in `kube-system`.
- Kubernetes API ports `443` and `6443` at any IPv4 or IPv6 address.
- HAProxy agent and stats ports on pods selected by
  `controller.networkPolicy.egress.haproxyPods.podSelector`. An empty
  `namespaceSelector` limits this rule to the release namespace.
- All ports on all cluster pods, through `egress.additionalRules`, so templates
  can fetch in-cluster HTTP resources.

When you enable a shared cache or rate-limit store, the chart also creates
policies for those pods:

- `cache.varnish.networkPolicy.enabled` admits port 6081 only from
  the same release's HAProxy pods. Varnish egress is limited to cluster DNS and
  the same HAProxy pods' dedicated backend-fetch port (`cache.varnish.loopbackPort`, default `8090`) for cache-miss origin requests — never the client-facing HTTP/HTTPS ports.
  The HAProxy policy contains the reciprocal ingress rule, including when
  `haproxy.networkPolicy.allowExternal` is false.
- The HAProxy policy admits the metrics ports Prometheus needs without extra
  configuration, including when `haproxy.networkPolicy.allowExternal` is false:
  HAProxy's own stats port, and — while the Vector sidecar is enabled — its
  exporter ports (`vector.metricsPort`, plus `vector.sizeMetricsPort` when a
  [request-metrics](./monitoring.md#request-metrics) size family is on). Prometheus
  scrapes HAProxy and the agent directly; Vector re-exports SPOA hub metrics.
- `rateLimit.shared.managedStore.networkPolicy.enabled` admits Valkey and Sentinel
  only from the same release's HAProxy/SPOA pods and from the managed store pods
  themselves. Store egress is limited to DNS and store-internal replication,
  quorum, and failover traffic.

These policies select release-scoped labels, so two HAPTIC releases in one
namespace don't gain access to each other's cache or limiter tiers.

## Production hardening

Restrict API-server egress to your cluster's actual API endpoints. Ask your
cluster administrator which addresses and ports the network plugin sees: policy
can apply before or after Service address translation. Allowing only the Service
CIDR can block the controller from watching resources.

Inspect the Service and its backing endpoints:

```bash
kubectl get service kubernetes --namespace default -o wide
kubectl get endpointslice --namespace default \
  -l kubernetes.io/service-name=kubernetes -o wide
```

For example, this values file allows an API endpoint at `192.0.2.10:6443` and
removes the default rule allowing connections to all cluster pods. Replace the
example address with the endpoints your cluster administrator identifies before
applying it:

```yaml
controller:
  networkPolicy:
    egress:
      additionalRules: []
      kubernetesApi:
        - cidr: 192.0.2.10/32
          ports:
            - port: 6443
              protocol: TCP
```

Include every required endpoint, including IPv6 addresses on a dual-stack
cluster. Helm replaces the entire `kubernetesApi` list when you set it. If your
templates use `http.Fetch()`, add rules for those destinations under
`additionalRules` before removing the default allow-all rule.

<a id="kind-cluster-specifics"></a>

## Replacing the shipped policies

To supply your own policies, disable the corresponding chart policy:

| Component | Helm value |
|-----------|------------|
| Controller | `controller.networkPolicy.enabled: false` |
| HAProxy | `haproxy.networkPolicy.enabled: false` |
| Varnish | `cache.varnish.networkPolicy.enabled: false` |
| Managed Valkey/Sentinel | `rateLimit.shared.managedStore.networkPolicy.enabled: false` |

The controller policy selects only controller pods using release and component
labels. Preserve DNS, API-server, and agent egress, plus health-probe, webhook,
and monitoring ingress in a replacement. API-server egress may need the API
server's endpoint IP and port rather than its Service IP, depending on where
your network plugin enforces policy.

Start from the rendered policy for your release so configured ports and
selectors stay consistent:

```bash
helm get manifest haptic --namespace haptic > haptic-manifest.yaml
```

Edit the `NetworkPolicy` documents you intend to manage separately, then disable
only those chart policies in your Helm values. See
[Kubernetes NetworkPolicy](https://kubernetes.io/docs/concepts/services-networking/network-policies/)
for policy semantics.

## Allowing Prometheus scraping

Controller metrics ingress is closed by default. For [Prometheus monitoring](./monitoring.md),
allow the namespace and pod labels used by your Prometheus installation. This
example selects pods labelled `app: prometheus` in namespace `monitoring`:

```yaml
controller:
  networkPolicy:
    enabled: true
    ingress:
      monitoring:
        enabled: true
        podSelector:
          matchLabels:
            app: prometheus
        namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: monitoring
```
