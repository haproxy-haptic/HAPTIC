---
description: "Helm chart for deploying HAPTIC, a template-driven HAProxy ingress controller for Kubernetes, with preconfigured template libraries for Ingress and Gateway API."
---

# Deploying with Helm

Install HAPTIC with the Helm chart. It deploys the controller, two HAProxy
replicas, custom resource definitions (CRDs), and the [template libraries](template-libraries.md)
for Ingress and Gateway API. Configure the installation through Helm values.
For a first installation and an optional sample route, follow [Getting started](getting-started.md).

## Prerequisites

- Kubernetes 1.35 or newer; see [Kubernetes compatibility checks](./operations/kubernetes-versions.md)
- Helm 3.8 or newer

!!! note
    Set `haproxyVersion` to select matching controller and HAProxy images. The default is 3.4; see [HAProxy versions](./operations/haproxy-versions.md) for supported alternatives.

## Installation

```bash
helm install haptic oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
  --version 0.9.0 --namespace haptic --create-namespace
```

With custom values:

```bash
helm install haptic oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
  --version 0.9.0 --namespace haptic --create-namespace \
  -f haptic-values.yaml
```

## Check the installation

Wait for the controller and HAProxy Deployments to become ready:

```bash
kubectl rollout status deployment/haptic-controller --namespace haptic --timeout=180s
kubectl rollout status deployment/haptic-haproxy --namespace haptic --timeout=180s
```

These names use the release and namespace from the install command. For a
failed rollout, follow [installation troubleshooting](troubleshooting.md#install-issues).

## Change settings

Keep your custom settings in `haptic-values.yaml`. Add the values shown in these
guides to that file, then pass the complete file when you install or upgrade.

If you already have a values file for this release, use it. If you don't, export
the values from your installed release before making changes:

```bash
helm get values haptic --namespace haptic --output yaml > haptic-values.yaml
if [ "$(cat haptic-values.yaml)" = null ]; then
  printf '{}\n' > haptic-values.yaml
fi
```

This includes settings supplied with `--set`. Keep them when adding a new setting;
passing only the new setting to `helm upgrade` can restore other settings to their
chart defaults. Use the [upgrade command](#upgrading) to apply the edited file.

<a id="whats-in-this-chart"></a>

The chart manages the controller, HAProxy pods, configuration, permissions, and
validation hooks. See [HAProxy deployment settings](haproxy-deployment.md) to
change the pod and Service settings, or the [values reference](reference.md)
for all options.

## Running multiple HAPTIC instances in one cluster

The example below gives a second HAPTIC release its own namespace, classes, and
controller identifiers. This separates its configuration and HAProxy fleet from
the first installation:

| Setting | Values key | Purpose |
|---------|-----------|--------------------|
| Release name and namespace | `helm install <name> --namespace <ns>` | Separates workloads, configuration, Secrets, and the leader-election lease |
| Ingress class | `ingressClass.name` | The controller watches only Ingresses whose `spec.ingressClassName` equals this value. Two releases sharing it would both process the same Ingresses |
| Gateway class | `gatewayClass.name` | The controller watches only Gateways whose `spec.gatewayClassName` equals this value |
| Controller identifier | `ingressClass.controllerName` and `gatewayClass.controllerName` | The `GatewayClass` watch is filtered to `spec.controllerName`; two releases sharing it would fight over the same GatewayClasses' status. Default: `haproxy-haptic.org/controller` |

The chart derives watch filters from `ingressClass.name` and `gatewayClass.name`.
Set the class names in values; the chart updates the filters to match.

The leader-election lease name defaults to the release's full name. The
configuration name defaults to `haptic-config`, independently of the release name.
If you place releases in the same namespace, give each a distinct
`controller.configName` and ensure they don't share managed Secrets. Separate
namespaces avoid those ownership conflicts.

Example values for a second release with its own classes:

```yaml
# team-b-values.yaml
ingressClass:
  name: haptic-team-b
  controllerName: haproxy-haptic.org/team-b
gatewayClass:
  name: haptic-team-b
  controllerName: haproxy-haptic.org/team-b
```

```bash
helm install haptic-team-b oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
  --version 0.9.0 \
  --namespace haptic-team-b --create-namespace \
  -f team-b-values.yaml
```

Ingress and Gateway authors then select this release with `ingressClassName: haptic-team-b` or `gatewayClassName: haptic-team-b`.

## Upgrading

For an upgrade from 0.5, follow the [0.6 upgrade notes](upgrade-notes.md#upgrading-to-06). For an upgrade from 0.3, follow the [0.4 upgrade notes](upgrade-notes.md#upgrading-to-04). For an upgrade from 0.2, follow the [0.3 upgrade notes](upgrade-notes.md#upgrading-to-03). For an upgrade from 0.1.0 or a 0.2.0 alpha, read the [0.2 upgrade notes](upgrade-notes.md#upgrading-to-02) first.

Pass your complete values file when upgrading. If you configured the release with
`--set` and have no saved file, [export its values first](#change-settings):

```bash
helm upgrade haptic oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
  --version 0.9.0 --namespace haptic \
  -f haptic-values.yaml
```

For an installation with no custom settings, you can upgrade without a values file:

```bash
helm upgrade haptic oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
  --version 0.9.0 --namespace haptic
```

[Check the Deployments](#check-the-installation) after an upgrade.

!!! warning "The chart owns the `HAProxyTemplateConfig`"
    Put persistent configuration changes under `controller.config` in your values file. Edits made with `kubectl edit` or `kubectl patch` take effect immediately, but a later `helm upgrade` can overwrite them. Helm also owns the library objects it creates.

### Controller preparation

After validating the complete incoming configuration, the pre-upgrade hook checks
whether the new binary can load the current configuration. If it can, the hook
updates the existing controller image and waits for the old replicas to exit
before Helm applies new templates. Config and library identities stay unchanged.
The hook preserves your Deployment strategy: `Recreate` still permits controller
downtime; HAProxy keeps its loaded configuration.

If the live configuration is invalid or a referenced library is missing, the hook
reports that it's using the startup recovery path. The validated incoming set is
then applied, and each controller must pass its normal startup gate. An API
failure, a partially applied library revision, or a changed source stops preparation
instead of selecting recovery.

Use a new image tag or digest for each controller build. If an unchanged image
reference resolves to a different build, preparation stops before changing the
Deployment. Paused Deployments must be resumed before preparation. With zero
replicas, the hook waits for existing pods to disappear; the final Deployment
starts against the new configuration.

The hook reads immutable build metadata directly from each controller Pod's
`/build` health endpoint. The shipped NetworkPolicy allows this traffic from the
preflight Pod in the same namespace. If you restrict health-port ingress, allow
the preflight Pod to reach that port. The hook's namespace Role grants read access
to the config, libraries, and workload identities, plus patch access to this
release's controller Deployment. `/debug/*` remains loopback-only.

Flux HelmRelease runs this Helm hook; Argo CD maps it to `PreSync`. Applying
rendered manifests directly or using `--no-hooks` skips preparation. Those paths
retain the startup and live validation gates, but old replicas can reject newer
templates until their replacements start. For release `haptic` in namespace `haptic`, run this command in the target
controller image, with cluster credentials, access to controller Pod IPs, and your complete `values.yaml`,
before applying the rendered manifests:

```bash
haptic preflight --prepare-upgrade --values values.yaml --namespace haptic --release haptic
```

The Job's `preRolloutValidation.activeDeadlineSeconds` bounds validation and
preparation together. Increase that configured budget when your chosen
`minReadySeconds` or rollout size requires more time; the Helm timeout must also
cover the complete operation.

A failed preparation doesn't apply the incoming templates. Some replicas may
already use the new image with the previous configuration. Correct the reported
cause and retry the upgrade, or roll back the release. Rollback to a chart that
predates preparation uses that chart's hooks and can produce temporary template
rejections; the startup and live gates still prevent invalid configurations from
reaching HAProxy.

### Recover a failed upgrade

Check the release status and validation Job for the default installation:

```bash
helm status haptic --namespace haptic
kubectl get jobs --namespace haptic
kubectl logs --namespace haptic job/haptic-haptic-pre-rollout
```

If pre-rollout validation fails, fix the reported values or templates and repeat
the [upgrade command](#upgrading) with your complete values file. Existing
HAProxy workers keep their last configuration while this check blocks rollout.
Earlier hooks may already have updated CRDs or certificates; a failed upgrade
doesn't mean every resource is unchanged.

If validation passed but a Deployment doesn't become ready, follow
[installation troubleshooting](troubleshooting.md#install-issues). Keep hooks
and validation enabled when retrying.

Helm rollback doesn't reverse CRD changes or run the chart's pre-upgrade
validation. HAPTIC doesn't guarantee downgrades across configuration API
changes. Recover with corrected values or a corrected forward release, and
check the [upgrade notes](upgrade-notes.md) for version-specific migrations.

## Uninstalling

```bash
helm uninstall haptic --namespace haptic
```

Use the release name and namespace from installation. Uninstall removes the
release workloads and configuration. CRDs, retained default-certificate Secrets,
and runtime-created agent certificate Secrets remain. Keep the agent issuer and
identity Secrets together if you plan to reuse them on reinstall.

Delete the CRDs only when no HAPTIC installation still needs them. This deletes
**every instance of these resources across all namespaces**, including route
policies and custom configurations:

```bash
kubectl delete crd \
  haproxytemplateconfigs.haproxy-haptic.org \
  haproxytemplatelibraries.haproxy-haptic.org \
  haproxycfgs.haproxy-haptic.org \
  haproxygeneralfiles.haproxy-haptic.org \
  haproxycrtlistfiles.haproxy-haptic.org \
  haproxymapfiles.haproxy-haptic.org \
  haproxyroutepolicies.haproxy-haptic.org
```

## Where to go next

- [Route traffic](routing.md) to your applications.
- [Configure HAProxy pods and access](haproxy-deployment.md), including replicas and Service type.
- [Validate a change before rollout](operations/validate-before-deploy.md).
- [Set up monitoring](operations/monitoring.md) and [high availability](operations/high-availability.md).
