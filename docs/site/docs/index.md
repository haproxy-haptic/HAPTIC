---
description: "Customize Kubernetes traffic routing with templates. Start with Ingress and Gateway API, then add your own annotations, routing rules, and resource types."
hide:
  - navigation
---

# HAPTIC

HAPTIC lets you shape Kubernetes traffic routing by editing the [templates](templating.md)
that generate your HAProxy configuration. Start with ready-to-use Ingress and
Gateway API support, then add your own annotations, routing rules, or resource
types as your needs change.

<a id="what-is-haptic"></a>
<a id="whats-haptic"></a>

## Choose your next step

| Your task | Start here |
| --- | --- |
| Install HAPTIC | [Getting started](getting-started.md) |
| Replace an existing ingress controller | [Migration guide](migrating.md) |
| Upgrade an existing HAPTIC installation | [Upgrade procedure](deploying-with-helm.md#upgrading) and [release notes](upgrade-notes.md) |
| Add routing behavior or custom annotations | [Templating guide](templating.md) |
| Try a template before installing | [Browser playground](https://haproxy-haptic.org/playground/) |
| Monitor or troubleshoot an installation | [Operations guides](operations/index.md) |

## Quick start

Use Kubernetes 1.35 or newer and Helm 3.8 or newer. HAPTIC is pre-1.0: pin a
chart version and check the [upgrade notes](upgrade-notes.md) before updating.
Choose the installed version in the documentation menu; `dev` also describes
unreleased features.

```bash
helm install haptic oci://ghcr.io/haproxy-haptic/haptic/charts/haptic --version 0.7.0 --namespace haptic --create-namespace
```

This installs the controller, two HAProxy replicas, and the default routing
libraries. [Getting started](getting-started.md) explains how to check the installation
and includes an optional sample app.

<a id="what-makes-haptic-different"></a>

## Add behavior with a template

Add a custom annotation with a template snippet. This example lets each Ingress choose a request-ID header for its own backends. With Helm, put the snippet under `controller.config.templateSnippets`.

<div class="pg-embed" markdown data-scenario="extend" data-tab="haproxy.cfg" data-controls="tabs,resources" data-input="resources" data-input-focus="example.com/request-id-header" data-output-focus="http-request set-header X-Request-ID" data-title="A custom annotation, implemented as one snippet" data-height="440">

<p class="pg-task" markdown>In the **Resources** panel, change the `shop` Ingress's `example.com/request-id-header` value to `X-Trace-ID`. Check the `http-request set-header` line in `haproxy.cfg`, then remove the annotation to remove the line.</p>

```yaml
controller:
  config:
    templateSnippets:
      backend-directives-300-request-id:
        template: |
          {%- if ingress != nil %}
            {%- var header = ingress.metadata.annotations["example.com/request-id-header"] %}
            {%- if header != "" %}
              {%- if !regex_search(header, "^[A-Za-z0-9-]+$") %}
                {{ fail("example.com/request-id-header must contain only letters, digits, or hyphens.") }}
              {%- end %}
              http-request set-header {{ header }} %[uuid()]
            {%- end %}
          {%- end %}
```

</div>

Set `example.com/request-id-header: "X-Request-ID"` on an Ingress to enable the header. This example accepts letters, digits, and hyphens in the header name. The backend hook scopes the rule to that Ingress; changing it requires a reload. See the [Templating Guide](templating.md) for more examples.

<a id="key-features"></a>
<a id="routing-and-application-policies"></a>
<a id="configure-routing-with-the-bundled-libraries"></a>

## Route your applications

Use [Ingress](libraries/ingress.md) for host and path routing, or
[Gateway API](gateway-api.md) for listeners and routes managed separately.
The [routing guides](routing.md) cover TLS, authentication, rate limits, caching,
and compatibility with annotations from other controllers.

<a id="templates-and-resource-apis"></a>
<a id="deployment-and-operations"></a>
<a id="where-to-go-next"></a>

## Customize and operate HAPTIC

- [Write a template](templating.md) to add behavior or use your own resource types.
- [Test your templates](validation-tests.md) before applying a change.
- [Operate HAPTIC](operations/index.md) with monitoring, diagnostics, and deployment guides.

<a id="reading-the-docs-as-an-ai-agent"></a>
<a id="contributing-to-the-docs"></a>

HAPTIC is an independent community project and isn't affiliated with
HAProxy Technologies.
