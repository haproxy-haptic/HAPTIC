# Install and operate HAPTIC with GitOps

Manage HAPTIC through Argo CD or Flux with the examples below. Stable Secrets
preserve credentials across reconciliations, and the chart's validation hooks
check configuration before rollout. Keep these hooks enabled.
Argo CD maps the CRD, validation, and agent-certificate bootstrap hooks to
`PreSync`. Flux runs them as Helm hooks. Both stop before applying the release's
configuration if preflight validation fails.

These examples install release `haptic` in namespace `haptic`. Use one GitOps
controller for the release. Adopting a release already managed by another tool
requires transferring ownership; that migration isn't covered here.

!!! note "Version requirement"
    These procedures require HAPTIC 0.2.0 or a matching development build.
    The `credentials.existingSecret` setting and automatic agent-certificate
    renewal aren't included in 0.2.0-alpha.3 or earlier.

## Prepare stable Secrets

You need an installed Argo CD or Flux controller, Bash, `kubectl`, `jq`, and OpenSSL.
The examples keep the default automatic agent-certificate renewal, which doesn't
require cert-manager. Webhook and frontend certificates use one of the two
sources below.

1. Create the namespace.

    ```bash
    kubectl create namespace haptic --dry-run=client -o yaml | kubectl apply -f -
    ```

2. Create the credential Secret if it doesn't exist.

    ```bash
    set -e
    existing=$(kubectl get secret haptic-gitops-credentials -n haptic --ignore-not-found -o name)
    if [ -z "$existing" ]; then
      password=$(openssl rand -base64 32)
      kubectl create secret generic haptic-gitops-credentials -n haptic \
        --from-literal=dataplane_username=admin \
        --from-literal=dataplane_password="$password"
      unset password
    fi
    ```

    Your Secret-management system can create this Secret instead. It must provide
    `dataplane_username` and `dataplane_password`. Keep its plaintext contents out
    of Git. When migrating from chart-generated credentials, use a new Secret
    name so removal of the old chart resource can't delete your replacement.

3. Select a certificate source.

    === "External certificates"

        Obtain a webhook server certificate for `haptic-webhook.haptic.svc` and
        a default frontend certificate for your application domains. This path
        uses `webhook.crt`, `webhook.key`, `webhook-ca.crt`, `frontend.crt`, and
        `frontend.key` from your issuer. The `.crt` files must contain the leaf
        and any intermediate certificates.

        ```bash
        kubectl create secret generic haptic-gitops-webhook -n haptic \
          --type=kubernetes.io/tls \
          --from-file=tls.crt=webhook.crt \
          --from-file=tls.key=webhook.key \
          --from-file=ca.crt=webhook-ca.crt \
          --dry-run=client -o yaml | kubectl apply -f -
        kubectl create secret tls haptic-gitops-frontend -n haptic \
          --cert=frontend.crt --key=frontend.key \
          --dry-run=client -o yaml | kubectl apply -f -
        jq -n --rawfile ca webhook-ca.crt '{
          credentials: {existingSecret: "haptic-gitops-credentials"},
          controller: {webhook: {
            secretName: "haptic-gitops-webhook",
            caBundle: ($ca | @base64),
            certManager: {enabled: false}
          }},
          defaultSSLCertificate: {
            secretName: "haptic-gitops-frontend",
            certManager: {enabled: false}
          }
        }' > haptic-values.json
        ```

        Your issuer must renew these certificates and update their Secrets before
        expiry. The controller reloads the webhook certificate; the frontend
        certificate follows normal configuration deployment. Changing the
        webhook's CA also requires updating `controller.webhook.caBundle` in
        Git. Automatic agent renewal doesn't renew these external certificates.

    === "cert-manager"

        Install cert-manager before applying this configuration. The chart uses
        it for webhook and default frontend certificates; agent certificates
        retain their independent default renewal mechanism.

        ```bash
        kubectl get crd certificates.cert-manager.io
        jq -n '{
          credentials: {existingSecret: "haptic-gitops-credentials"},
          controller: {webhook: {certManager: {enabled: true}}}
        }' > haptic-values.json
        ```

        This creates a self-signed default frontend certificate for `localdev.me`.
        Configure a production certificate before exposing application HTTPS;
        see [SSL certificates](../ssl-certificates.md).

The values file contains Secret names and public trust material. Store it with
your GitOps configuration. The chart hashes an external credential Secret's name,
so unchanged rendering doesn't generate a password or roll the pods. With
explicit legacy Basic authentication, rotating that Secret also requires
replacing the HAProxy pods because their password comes from the environment.
Default mutual TLS doesn't use that password for agent authentication.

## Argo CD installation

Use an Argo CD project that permits HAPTIC's namespaced resources, CRDs, cluster
RBAC, IngressClass, and admission webhook. The example uses the standard `default`
project. Argo CD Core installations need an AppProject created separately.

1. Generate the Application.

    ```bash
    read -r -p "Chart version to install: " HAPTIC_CHART_VERSION
    test -n "$HAPTIC_CHART_VERSION"
    jq -n --arg version "$HAPTIC_CHART_VERSION" --slurpfile values haptic-values.json '{
      apiVersion: "argoproj.io/v1alpha1",
      kind: "Application",
      metadata: {name: "haptic", namespace: "argocd"},
      spec: {
        project: "default",
        destination: {server: "https://kubernetes.default.svc", namespace: "haptic"},
        source: {
          repoURL: "ghcr.io/haproxy-haptic/haptic/charts",
          chart: "haptic",
          targetRevision: $version,
          helm: {releaseName: "haptic", valuesObject: $values[0]}
        },
        syncPolicy: {
          automated: {enabled: true, prune: true, selfHeal: true},
          retry: {limit: 1},
          syncOptions: ["ServerSideApply=true", "DisableClientSideApplyMigration=true"]
        }
      }
    }' > haptic-application.json
    ```

    Keep the Application name equal to the Helm release name when Argo CD uses
    its default label-based resource tracking. This preserves the instance labels
    used by the chart's selectors. The OCI Helm repository URL has no `oci://`
    prefix in Argo CD's Helm source format.

2. Apply the Application.

    ```bash
    kubectl apply -f haptic-application.json
    ```

    Commit the generated Application to the configuration repository watched by
    your Argo CD bootstrap application for subsequent changes.

3. Inspect the result.

    ```bash
    kubectl get application haptic -n argocd
    kubectl get jobs,pods -n haptic
    ```

Use full syncs. Argo CD selective resource syncs don't run hooks. Don't add
Argo-specific hook annotations to individual chart resources: Argo CD ignores
Helm hooks when it finds Argo-specific hooks in the application. The chart already
sets hook ordering. See [Argo CD's Helm hook mapping](https://argo-cd.readthedocs.io/en/stable/user-guide/helm/#helm-hooks).

## Flux installation

The example uses an `OCIRepository` and a `HelmRelease`. `RetryOnFailure` retries a
failed operation without uninstalling the existing release or attempting a
rollback over the chart's CRDs.

1. Generate the source and release.

    ```bash
    read -r -p "Chart version to install: " HAPTIC_CHART_VERSION
    test -n "$HAPTIC_CHART_VERSION"
    jq -n --arg version "$HAPTIC_CHART_VERSION" '{
      apiVersion: "source.toolkit.fluxcd.io/v1",
      kind: "OCIRepository",
      metadata: {name: "haptic", namespace: "flux-system"},
      spec: {
        interval: "10m",
        url: "oci://ghcr.io/haproxy-haptic/haptic/charts/haptic",
        ref: {tag: $version},
        layerSelector: {mediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip", operation: "copy"}
      }
    }' > haptic-source.json
    jq -n --slurpfile values haptic-values.json '{
      apiVersion: "helm.toolkit.fluxcd.io/v2",
      kind: "HelmRelease",
      metadata: {name: "haptic", namespace: "flux-system"},
      spec: {
        interval: "10m",
        timeout: "15m",
        releaseName: "haptic",
        targetNamespace: "haptic",
        chartRef: {kind: "OCIRepository", name: "haptic"},
        install: {strategy: {name: "RetryOnFailure", retryInterval: "5m"}},
        upgrade: {strategy: {name: "RetryOnFailure", retryInterval: "5m"}},
        values: $values[0]
      }
    }' > haptic-release.json
    ```

2. Apply both resources.

    ```bash
    kubectl apply -f haptic-source.json -f haptic-release.json
    ```

    Commit them to the repository watched by your Flux `Kustomization`. If the same
    repository provisions the Secrets, order their `Kustomization` before this
    release with `dependsOn` and a Secret readiness check.

3. Inspect the result.

    ```bash
    kubectl get ocirepository,helmrelease -n flux-system
    kubectl get jobs,pods -n haptic
    ```

Flux runs Helm install and upgrade actions, including live `lookup` calls. Argo
CD renders Helm templates offline. External Secrets make the examples stable in
both paths. See [Flux HelmRelease behavior](https://fluxcd.io/flux/components/helm/helmreleases/).

## Upgrade and recover a rejected change

Pin a chart version and keep the chart and controller image from the same build.
The validation Job renders the chart embedded in that image and checks its
version before validating the complete future configuration. The CRD hook runs
first; CRD changes may therefore have applied even when later validation fails.

Argo CD with server-side diff (`controller.diff.server.side: "true"`) compares
the new release against the CRDs already in the cluster before any hook runs.
Upgrading to the next release works directly. If you skip a release, apply the
target version's CRDs first, or the Application stays `Unknown` with
`field not declared in schema`:

```bash
read -r -p "Chart version to upgrade to: " HAPTIC_CHART_VERSION
helm show crds oci://ghcr.io/haproxy-haptic/haptic/charts/haptic --version "$HAPTIC_CHART_VERSION" \
  | kubectl apply --server-side --force-conflicts -f -
```

When validation rejects a sync, inspect its retained Job:

```bash
kubectl get job haptic-haptic-pre-rollout -n haptic
kubectl logs job/haptic-haptic-pre-rollout -n haptic
```

Fix the rejected values or select a corrected release in Git. A full Argo CD sync
or Flux reconciliation reruns the hooks. Existing HAProxy workers keep their last
configuration while preflight blocks the rejected release.

Once the corrected version and values appear in the GitOps resources, you can
request immediate reconciliation with the Argo CD or Flux CLI:

=== "Argo CD"

    ```bash
    argocd app sync haptic
    argocd app wait haptic --sync --health --timeout 900
    ```

=== "Flux"

    ```bash
    flux reconcile source oci haptic -n flux-system
    flux reconcile helmrelease haptic -n flux-system
    ```

Don't use `--no-hooks`, selective sync, or disabled validation to force the bad
release through. Check the [rollback limitations](../deploying-with-helm.md#recover-a-failed-upgrade)
before changing versions; recover across configuration API changes with a
corrected forward release.
