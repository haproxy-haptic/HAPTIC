---
search:
  boost: 4
description: "Get started with HAPTIC, the template-driven HAProxy ingress controller for Kubernetes. Install with Helm, deploy HAProxy, and verify your setup."
---

# Getting started

<a id="overview"></a>

Install HAPTIC with Helm, then route your applications. The optional
walkthrough creates a sample route you can test locally.

To try HAPTIC without a cluster, use the [browser example](#try-in-your-browser).

## Prerequisites

- A Kubernetes 1.35 or newer cluster
- `kubectl` configured to access the cluster
- Helm 3.8 or newer
- Capacity for the [default installation](operations/performance.md): about
  1 CPU core and 5.4 GiB of memory requests, plus room for installation Jobs

## Install with Helm

If HAPTIC is already installed, continue to the sample app below or follow
[Upgrading with Helm](deploying-with-helm.md#upgrading) to change its version.

For a new installation, install the released chart below. Choose that version
in the documentation menu when following other guides; `dev` includes
unreleased features.

```bash
helm install haptic oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
  --version 0.6.0 \
  --namespace haptic --create-namespace
```

The chart installs the controller, HAProxy, and routing templates. Admission
validation checks proposed routing changes before Kubernetes accepts them.

The chart creates a default HTTPS certificate. It uses cert-manager for issuance
and renewal when available; otherwise, it creates a self-signed certificate.
For your own domains, configure [SSL certificates](./ssl-certificates.md).

Wait for the two controller replicas and two HAProxy replicas to become ready:

```bash
kubectl -n haptic rollout status deployment/haptic-controller --timeout=180s
kubectl -n haptic rollout status deployment/haptic-haproxy --timeout=180s
```

The chart creates IngressClass `haptic` and, when the Gateway API CRDs are
available, GatewayClass `haptic`.

For your own applications, use `ingressClassName: haptic` on an Ingress or
`gatewayClassName: haptic` on a Gateway. See the [Ingress examples](libraries/ingress.md)
or [Gateway routing guide](gateway-api.md#expose-a-service-through-a-gateway).

## Optional walkthrough: route a sample app

Follow this walkthrough to try a route, or continue with your own applications
using the [routing guides](routing.md).

### Deploy a sample app

Create a simple echo service:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: echo
  namespace: default
spec:
  replicas: 2
  selector:
    matchLabels:
      app: echo
  template:
    metadata:
      labels:
        app: echo
    spec:
      containers:
      - name: echo
        image: ealen/echo-server:latest
        ports:
        - containerPort: 80
        env:
        - name: PORT
          value: "80"
---
apiVersion: v1
kind: Service
metadata:
  name: echo
  namespace: default
spec:
  selector:
    app: echo
  ports:
  - port: 80
    targetPort: 80
```

Save as `echo-app.yaml` and apply:

```bash
kubectl apply -f echo-app.yaml
kubectl -n default rollout status deployment/echo --timeout=180s
```

### Create an Ingress

Create an Ingress that routes your test hostname to the echo service:

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: echo-ingress
  namespace: default
spec:
  ingressClassName: haptic
  rules:
  - host: echo.example.local
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: echo
            port:
              number: 80
```

Save as `echo-ingress.yaml` and apply:

```bash
kubectl apply -f echo-ingress.yaml
```

The route also serves HTTPS with the chart's default certificate. For your own
hostname, follow [TLS certificate setup](ssl-certificates.md).

### Test the routing

<a id="port-forward-to-haproxy"></a>

Forward a local port to HAProxy:

```bash
kubectl port-forward -n haptic svc/haptic-haproxy 8080:80
```

<a id="test-the-endpoint"></a>

In another terminal:

```bash
curl -H "Host: echo.example.local" http://localhost:8080/
```

The response includes the request headers and the serving pod's `HOSTNAME`.
Repeat the request to check that HAProxy distributes traffic across the echo pods.
If the request fails, follow [routing troubleshooting](troubleshooting.md#routing-issues).

<a id="inspect-the-configuration-optional"></a>
<a id="check-the-controller-logs"></a>
<a id="inspect-the-rendered-haproxy-configuration"></a>

To inspect what HAPTIC generated, see [configuration debugging](operations/debugging.md#inspect-the-generated-configuration).

## Next steps

| What you want to do | Read next |
| --- | --- |
| Route your applications | [Ingress](libraries/ingress.md) or [Gateway API](gateway-api.md#expose-a-service-through-a-gateway) |
| Replace another ingress controller | [Migration guide](migrating.md) |
| Change chart settings | [Helm deployment](deploying-with-helm.md) and [values reference](reference.md) |
| Add a custom annotation or routing rule | [Templating](templating.md) |
| Use your own resource types | [Watching resources](watching-resources.md) |
| Prepare for production traffic | [High availability](operations/high-availability.md), [security](operations/security.md), and [monitoring](operations/monitoring.md) |

<a id="troubleshooting"></a>

If installation or routing fails, follow [troubleshooting](troubleshooting.md).

## Clean up

Stop port forwarding with **Ctrl+C**, then remove the sample app if you deployed it:

```bash
kubectl delete ingress echo-ingress -n default
kubectl delete deployment echo -n default
kubectl delete service echo -n default
```

HAPTIC remains installed for your own applications. To remove the controller and
HAProxy too, follow [Uninstalling](deploying-with-helm.md#uninstalling).

## Try in your browser

This example turns sample Ingress resources into HAProxy configuration without
connecting to a cluster.

<div class="pg-embed" markdown data-scenario="ingress" data-tab="maps" data-focus="host.map" data-controls="tabs,resources" data-input="resources" data-input-focus="shop.example.com" data-title="Turn an Ingress into HAProxy configuration" data-height="480">

<p class="pg-task" markdown>In **Resources**, change `shop.example.com` to `store.example.com`. The **maps** output shows the new hostname in `host.map`.</p>

</div>
