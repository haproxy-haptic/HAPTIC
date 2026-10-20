# Design documentation

## Overview

HAPTIC is a Kubernetes operator that watches Kubernetes resources and renders them through templates into validated HAProxy configurations.

Components communicate through a central EventBus using pub/sub and request-response patterns, enabling observability, testability, and loose coupling.

**Why this architecture:**

- Event-driven design allows components to evolve independently
- Template-based approach provides maximum flexibility without annotation constraints
- Admission checks, config-load tests, and the render gate detect invalid configurations
- Runtime API optimization minimizes service disruption during updates

## Navigation

This section is for people changing HAPTIC's Go code and charts; if you run HAPTIC rather than develop it, the Operations guides (starting with [High Availability](../operations/high-availability.md)) are for you.

The design documentation is organized the way the sidebar groups it:

**Architecture**

- **[Architecture Overview](design/architecture-overview.md)** - High-level system architecture with component diagrams showing the controller's internal event-driven structure, validation flow, and the operating assumptions and constraints the design rests on

- **[Package Structure](design/package-structure.md)** - Go package organization including directory structure, package dependencies, and key interfaces

- **[Sequence Diagrams](design/sequence-diagrams.md)** - Dynamic behavior including startup initialization, resource change handling, configuration validation, and zero-reload deployment

**Components**

- **[Configuration Model](design/configuration.md)** - User interface design showing how you configure the controller through HAProxyTemplateConfig CRD

- **[Deployment](design/deployment.md)** - Kubernetes deployment architecture showing controller pods, HAProxy pods, container configuration, and network topology

- **[Leader Election](design/leader-election.md)** - Lease-based leader election, the leader-only vs all-replica component split, and the bootstrap pattern that prevents missed events on leadership transitions

- **[Runtime Introspection](design/introspection.md)** - Debug HTTP endpoints for runtime state inspection, event history tracking, and integration with acceptance testing

**Validation and decisions**

- **[CRD Validation](crd-validation-design.md)** - Durable design decisions behind the `HAProxyTemplateConfig` CRD and the three-layer validation stack (OpenAPI schema, admission webhooks, runtime validation)

- **[Design Decisions](design/design-decisions.md)** - Key architectural choices with rationale covering validation strategy, template engine selection, concurrency model, and observability integration

Process pages — [Linting](linting.md), [Releasing](releasing.md), and the [Gateway API Upgrade Playbook](gateway-api-upgrade-playbook.md) — cover the contributor workflow rather than the design.

## Core capabilities

The controller provides these capabilities:

**Template-Driven Configuration**
Generate HAProxy configurations using Scriggo templates with access to any Kubernetes resources you choose to watch. Templates give you complete control over the HAProxy configuration without annotation limitations.

**Dynamic Resource Watching**
Monitor any Kubernetes resource types (Ingress, Service, ConfigMap, custom CRDs) you specify. Resources are indexed using JSONPath expressions for fast template lookups.

**Configuration validation**
Watched-resource admission and HTTP-store promotion run `haproxy -c` and every configured rendered-output validator through the proposal pipeline ([ADR-0020](adr/0020-authoritative-render-validation-pipeline.md)). Reconciliation also requires both checks before accepting watched inputs. It retains rejected revisions while independently valid changes advance ([ADR-0032](adr/0032-watched-input-isolation.md)); the leader-side render gate remains active. The `HAProxyTemplateConfig` itself has no webhook (ADR-0016); the leader validates its complete merged set, while the startup load gate and chart pre-rollout `preflight` hook also run its embedded validation tests. Protocol-v1 external validators run on every applicable invocation. The render gate tracks HAProxy verdicts by plan identity; see [ADR-0022](adr/0022-haptic-agent.md).

**Zero-Reload Optimization**
Configuration changes that only modify server weight, `maxconn`, address, port, or maintenance state are applied through HAProxy's runtime API without process reloads. This maintains existing connections and minimizes service disruption. Changes requiring structural modifications trigger a reload.

**Structured Configuration Comparison**
Templates declare sections, backends, servers, and files in a render plan. The controller compares that plan with each pod's acknowledged state to choose runtime updates, file writes, or a reload; it doesn't parse HAProxy configuration text.

**Declarative Kubernetes Resource Emission**
Templates declared under `spec.k8sResources` produce one or more Kubernetes resources per render (multi-doc YAML, `---`-separated). The renderer parses the rendered YAML, validates required fields, and feeds the result into the controller's `resourceapplier`, which reconciles the set to the cluster via Server-Side Apply with field manager `haptic`, prunes orphans across renders, and injects an `OwnerReference` to the `HAProxyTemplateConfig` CR so cascade-delete (for example, `helm uninstall`) GCs the rendered resources. Same engine context as the main `haproxyConfig` template (resources, filters, snippets, file registry, shared cache) — chart libraries can compose extension points and shared state across the two passes.

## Design principles

**Resource-Agnostic Engine**
The Go code must be agnostic to every Kubernetes resource you choose to watch. If you decide to use some CRD instead of Gateway or Ingress resources, you should only need to touch HAPTIC templates and config — **no Go code**. Writing templates for an arbitrary CRD must be just as comfortable as for Ingress or Gateway API resources, with **no preferential treatment for well-known resources**. Resource shape comes from the kube-apiserver (live) or `--schema-dir` (offline) at runtime; the controller never bakes in a fixed list of supported kinds. This applies to all Go-side machinery — engine filters, runtime-context types, generated wrappers, helpers — and to chart-side scaffolding that crosses the Go/template boundary. The corollary on the chart side: resource-specific behavior lives in resource-specific template libraries (`ingress.yaml`, `gateway/*.yaml`, vendor annotation libs), never in `base.yaml`.

**Fail-Safe Operation**
Admission rejects invalid watched-resource changes. A failed config-load test keeps the last accepted configuration active. During reconciliation, a render-gate refusal holds later renders and reverts eligible pods to their last known good files; see [Render validation](../operations/debugging.md#haproxy-refused-the-config-the-fleet-was-given-configvalidatedfalse).

**Performance Through Indexing**
Composite indexes let templates fetch matching resources without scanning the
whole store. Watchers debounce change notifications, the coordinator coalesces
queued triggers, and the deployer's `minDeploymentInterval` paces reloads.

**Observable Event Flow**
Components publish lifecycle and result events through the EventBus; the render pipeline uses direct calls. The Event Commentator subscribes to all events and produces structured logs with contextual insights. Metrics track reconciliation cycles, validation results, and deployment success rates.

**Clean Component Separation**
Pure business logic components (`templating`, `k8s`, `dataplane`) have no event dependencies and can be tested in isolation. Event adapters in the controller package coordinate these pure components through EventBus messages.

## See also

### User guides

- [Templating Guide](../templating.md) - User guide for writing templates
- [CRD Reference](../crd-reference.md) - HAProxyTemplateConfig CRD documentation
- [Supported Configuration Reference](../supported-configuration.md) - What HAProxy features you can configure

### Operations

- [High Availability](../operations/high-availability.md) - Leader election and HA deployments
- [Monitoring](../operations/monitoring.md) - Prometheus metrics and alerting
- [Debugging](../operations/debugging.md) - Runtime introspection and troubleshooting
- [Security](../operations/security.md) - RBAC and security best practices
- [Performance](../operations/performance.md) - Resource sizing and optimization

### Package documentation

- [Controller Package](https://github.com/haproxy-haptic/HAPTIC/blob/main/pkg/controller/README.md) - Event-driven controller implementation
- [Template Engine](https://github.com/haproxy-haptic/HAPTIC/blob/main/pkg/templating/README.md) - Template engine API reference
- [Kubernetes Integration](https://github.com/haproxy-haptic/HAPTIC/blob/main/pkg/k8s/README.md) - Resource watching and indexing API
- [Dataplane Integration](https://github.com/haproxy-haptic/HAPTIC/blob/main/pkg/dataplane/README.md) - HAProxy configuration synchronization
