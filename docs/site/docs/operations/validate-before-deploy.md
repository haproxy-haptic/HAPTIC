# Validate a configuration before you deploy it

Run `haptic preflight` to check your chart values before deployment. Helm runs it
by default during installation and upgrades. Adding it to your delivery pipeline
finds the same errors before you apply the release.

## Prerequisites

- Your release's Helm values in `haptic-values.yaml`.
- The same HAPTIC binary, chart, and HAProxy series you plan to deploy. See
  [Getting the binary and chart](#getting-the-binary-and-chart).
- Kubernetes credentials for the target cluster, or an offline [schema directory](#schemas).
- Docker or `podman` for Vector and Varnish validation. Without a container runtime,
  those checks are skipped with a warning; the HAPTIC and HAProxy checks still run.

## Run the check

With `haptic` on your path and the matching chart in `./chart`, run:

```bash
haptic preflight \
  --values ./haptic-values.yaml \
  --chart ./chart \
  --namespace haptic \
  --release haptic
```

A non-zero exit means validation failed or couldn't run. Resolve the reported
error before deploying. To make this a delivery-pipeline gate, run the check
before Helm and stop the pipeline if it fails:

```bash
set -e
haptic preflight --values ./haptic-values.yaml --chart ./chart --namespace haptic
helm upgrade --install haptic ./chart --namespace haptic --create-namespace \
  --values ./haptic-values.yaml
```

Keep `preRolloutValidation.enabled: true` so deployment also checks the final values.

### What it checks

| Check | Catches |
|---|---|
| Structural validation and the bundled `validationTests`, including `haproxy -c` | A configuration the controller would refuse to load |
| `vector validate` on the rendered sidecar config | A malformed document **or** a broken transform that would keep the supervised Vector child unavailable |
| `varnishd -C` on the rendered Varnish Configuration Language (VCL) | A VCL that doesn't compile, which leaves the cache pod in `CrashLoopBackOff` |

### Command-line flags

| Flag | Default | Meaning |
|---|---|---|
| `--values`, `-f` | *required* | Your values file. Repeatable — later files win, as with `helm -f` |
| `--namespace`, `-n` | `haptic` | Release namespace. Use the one you deploy to — chart output depends on it |
| `--release` | `haptic` | Release name, which resource names are derived from |
| `--chart` | image-embedded chart | Chart directory, then `$HAPTIC_CHART_DIR`, then the copy inside the controller image |
| `--kubeconfig` | `$KUBECONFIG`, then in-cluster | Which cluster to read API schemas from — see [Schemas](#schemas) |
| `--schema-dir` | `$HAPTIC_SCHEMA_DIR` | Read schemas from a directory instead of the cluster, for running fully offline |
| `--api-versions` | — | Extra API versions your cluster serves. The Gateway API `GatewayClass` version is always declared, so the Gateway library renders the same way it does in the cluster |
| `--expect-chart-version` | `$HAPTIC_EXPECT_CHART_VERSION` | Fail unless the chart being rendered carries exactly this version. Set it to the version you plan to install |

Set `HAPTIC_CONTAINER_RUNTIME` to choose a runtime (default: `docker`, then
`podman`). Preflight selects Vector's image from the rendered Helm workload
and Varnish's image from the workload mounting each VCL ConfigMap. It checks
every distinct config and image pair, including variants with the same file name.

Set image overrides in your Helm values so validation and deployment use the
same images. If you set `HAPTIC_VECTOR_IMAGE` or `HAPTIC_VARNISH_IMAGE`, it must
equal the corresponding rendered image. A mismatch or an unresolved image
fails the check.

### Schemas

Preflight requires schemas to resolve typed fields and determine which optional
resources and features are available. It fails if it can't load a schema source.

By default, it reads schemas from the target cluster using your Kubernetes
credentials. This includes the custom resource definitions (CRDs) and schema
versions installed there.

For offline validation, pass `--schema-dir` with CRD manifests or OpenAPI v3
schemas. Keep this directory aligned with the target cluster; preflight can't
verify that alignment without cluster access.

```bash
# Offline: schemas from a directory, no cluster contacted.
haptic preflight --values ./haptic-values.yaml --chart ./chart --schema-dir ./schemas
```

## Getting the binary and chart

[Install the CLI](../cli.md) for the release you plan to deploy and install the
matching HAProxy series on the same host. The check runs `haproxy -c` locally.

In a working directory without an existing `chart` directory, download the
matching chart:

```bash
helm pull oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
  --version 0.7.0 --untar --untardir ./chart-download
mv ./chart-download/haptic ./chart
```

Use the same release version for the binary and chart; validating one version and
deploying another leaves the deployed combination unchecked.

The controller image also contains the matching binary at `/usr/local/bin/haptic`
and chart at `/usr/share/haptic/chart`. CI systems can extract both from the image
used by their release. Run the binary on the host with HAProxy and a container
runtime available to include the sidecar checks.

## What it doesn't cover

The check renders against your values, not against your cluster's live
Ingresses, Gateways, and Services. It proves the configuration is loadable and
that the generated sidecar and cache configurations are valid; it doesn't
predict what a future routing resource renders to.

For that, keep the admission webhook enabled — it validates each watched
resource as it's applied, and rejects one that would break the rendered
configuration.

To check whether a change needs a reload, compare it with a running pod:

```bash
haptic diff -f candidate.yaml --schema-dir ./schemas
```

Use [offline schemas](../validation-tests.md#prepare-schemas) when the candidate
uses typed resources. See [configuration comparison](./debugging.md#check-whether-a-change-needs-a-reload)
for output meanings and file-to-file comparison.
