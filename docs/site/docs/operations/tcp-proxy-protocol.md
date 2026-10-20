# Forward TCP with PROXY protocol

Use platform-owned template snippets to accept TCP connections from a trusted
PROXY-speaking load balancer and forward the client address to a PROXY-v2-aware
backend. This works with the bundled Kubernetes backend library, including when
you use Ingress for HTTP routing.

This example owns its frontend and Service port directly. It doesn't attach to a
Gateway or TCPRoute, produce their status, or apply their `allowedRoutes` and
`ReferenceGrant` checks. The bundled TCPRoute listener has no `accept-proxy` option.
Ingress describes HTTP routing; it doesn't expose arbitrary TCP services.

## Before you start

- Your backend accepts PROXY protocol v2 before its application bytes. A TCP or
  SSH server doesn't necessarily support PROXY protocol; verify the server's
  configuration first.
- Your upstream sends a PROXY v1 or v2 header on **every** connection, including
  checks directed at this listener.
- Your cluster enforces Kubernetes NetworkPolicy, and you control which pods can
  use the trusted proxy's namespace and labels.
- The `base` and `kubernetesBackends` template libraries are enabled, as they're
  by default.

The example uses an in-cluster upstream in namespace `ingress-system`, with label
`app: trusted-l4-proxy`. It exposes port `2222` on HAPTIC's ClusterIP Service and
forwards to Service `apps/tcp-app`, port `2222`. The backend Service may use a
different `targetPort`; endpoint addresses and ports come from EndpointSlices.

## Configure the frontend and backend

1. Download [tcp-proxy-values.yaml](../examples/tcp-proxy-values.yaml).

2. Configure your backend Service and pods so that `apps/tcp-app:2222` resolves to
   a PROXY-v2-aware listener.

3. Review the values against your existing deployment.

   The file adds `frontends-800-platform-tcp` and `backends-800-platform-tcp`
   through the existing template hooks. Change the fixed namespace, Service,
   Service port, and frontend port to match your deployment. Keep the values and
   their embedded validation fixtures together.

   The file changes the shared HAProxy Service to `ClusterIP`. Preserve your
   existing exposure settings if you also need external HTTP or HTTPS access.
   Helm replaces lists: include existing `extraPorts` and `extraIngress` entries
   in the combined values instead of dropping them.

   The NetworkPolicy allows the custom port only from the selected upstream pods.
   It also restricts the chart's HTTP, HTTPS, and monitoring ports through
   `allowedSources`. Reconcile those sources with your existing clients and
   monitoring before applying the file.

   `haproxy.service.extraPorts` exposes the frontend with numeric `targetPort:
   2222`. That number is the HAProxy bind port; no additional `containerPort` entry
   is needed. A named `targetPort` would need a matching named container port.
   Neither a Service port nor a `containerPort` creates an HAProxy listener.
   See [Kubernetes Service port definitions](https://kubernetes.io/docs/concepts/services-networking/service/#defining-a-service).

   Reserve the bind port across all custom snippets, Gateway listeners, and
   chart listeners, including agent, stats, and cache ports. The example doesn't
   add a Gateway port-ownership check; a collision can reject the configuration.

4. Validate the combined values before deploying.

   With the matching chart checked out at `./chart`, for a new installation:

   ```bash
   haptic preflight --chart ./chart --namespace haptic --release haptic \
     --values ./tcp-proxy-values.yaml
   ```

   For an existing installation with values in `haptic-values.yaml`:

   ```bash
   haptic preflight --chart ./chart --namespace haptic --release haptic \
     --values ./haptic-values.yaml --values ./tcp-proxy-values.yaml
   ```

   Use your release name, namespace, and existing values files. This command reads
   schemas from the configured cluster; for offline validation, also pass
   `--schema-dir ./schemas`. See [Validate before deploying](validate-before-deploy.md)
   for the CLI, native HAProxy, and schema requirements.

5. Install or update the release.

   For a new installation:

   ```bash
   helm upgrade --install haptic ./chart --namespace haptic --create-namespace \
     --values ./tcp-proxy-values.yaml
   ```

   For an existing installation:

   ```bash
   helm upgrade --install haptic ./chart --namespace haptic --create-namespace \
     --values ./haptic-values.yaml --values ./tcp-proxy-values.yaml
   ```

6. Configure the trusted upstream to connect to HAPTIC's Service port `2222`
   and send PROXY protocol.

   For a release named `haptic` in namespace `haptic`, an upstream HAProxy server
   line is:

   ```haproxy
   server haptic-tcp haptic-haproxy.haptic.svc.cluster.local:2222 check send-proxy-v2 check-send-proxy
   ```

   Custom release names, namespaces, or `fullnameOverride` change the Service
   address. Verify it with `kubectl --namespace haptic get service`.

## Keep the client address trustworthy

`accept-proxy` requires a header, but doesn't authenticate the address inside it.
Only the trusted upstream may reach port `2222`; an arbitrary client that can
reach it can claim another client's address.

NetworkPolicy rules are additive. Leaving `allowExternal: true`, or retaining
another policy that allows all TCP traffic to these pods, defeats the custom
port's allowlist. The backend's own ingress policy must also admit HAProxy pods
on the endpoint port, including health checks. The chart's default egress policy
already allows connections to backend pods.

For an external load balancer, use a Service exposure method and source rules
appropriate to that provider. Verify the source address the Container Network
Interface (CNI) plugin actually sees:
Service translation can change it to a node or load-balancer address. Restrict
the load balancer and any NodePorts at the network boundary too; copying a
claimed client CIDR into the pod policy doesn't establish trust in the sender.
See [Kubernetes NetworkPolicy behavior](https://kubernetes.io/docs/concepts/services-networking/network-policies/#behavior-of-to-and-from-selectors).

## Backend checks and failure behavior

The backend uses `Backend()` and `BackendServers()` so HAPTIC retains structured
backend and endpoint records. Its profile contains `mode tcp` and
`default-server check send-proxy-v2 check-send-proxy`. These are native HAProxy
options; no annotation supplies raw configuration.

`check` enables TCP connection checks. `send-proxy-v2` sends the client address
to the application, and `check-send-proxy` explicitly enables PROXY on health
checks. HAProxy normally inherits PROXY for checks to the same address and port;
an explicit check address or port needs `check-send-proxy` if that endpoint
expects PROXY. A separate plain-text health endpoint must not receive the
header. See the [HAProxy server options](https://docs.haproxy.org/3.4/configuration.html#5.2-check-send-proxy).

A TCP connection check proves that the port accepts a connection, not that the
application is ready or correctly processes PROXY. Use an application-specific
native check when you need that stronger guarantee.

Health checks have no client address. HAProxy sends a PROXY v2 `LOCAL` header for
these checks; verify that your backend accepts it.

An absent Service or empty EndpointSlice set leaves the backend without servers;
connections can't reach the application until endpoints appear. Not-ready
endpoints remain disabled. Missing or malformed incoming PROXY headers are
rejected. Changing the frontend can require a reload.

Before directing production traffic to the listener, test a real connection
through the trusted upstream and confirm the original client address at the
backend. Also verify that a direct connection without a header fails and that
another source can't reach the listener. The included validation tests
check rendered HAProxy syntax, endpoint selection, and PROXY directives; they
don't exercise your cluster's network path.
