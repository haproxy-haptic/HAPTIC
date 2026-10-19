# HAProxy versions

<a id="overview"></a>

Set `haproxyVersion` to select the HAProxy series for your installation. The chart
uses it for both images:

- The **controller image** tag suffix (for example `-haproxy3.2`) — selects the matching configuration validator
- The **HAProxy pod image** tag — defaults to the latest tested patch for that series

Keep the controller and HAProxy on the same series. Use `haproxyVersion` to
change both; use `haproxy.image.tag` only to pin a patch within that series.

## Supported versions

| Series | HAPTIC build | Community image | Enterprise image |
|--------|--------|-----------------|------------------|
| 3.0 | Supported (LTS) | `haproxytech/haproxy-debian:3.0.x` | `...:3.0r1` |
| 3.2 | Supported (LTS) | `haproxytech/haproxy-debian:3.2.x` | `...:3.2r1` |
| 3.3 | Supported (non-LTS) | `haproxytech/haproxy-debian:3.3.x` | — |
| 3.4 | Supported (LTS, default) | `haproxytech/haproxy-debian:3.4.x` | — |

The chart rejects any other `haproxyVersion`. HAProxy's even-numbered series (3.0, 3.2, 3.4) are LTS with about five years of support; odd-numbered series such as 3.3 get a shorter maintenance window. This chart defaults to 3.4. HAPTIC 0.6 removed HAProxy 3.1, which no longer receives upstream fixes; see [Upgrading to 0.6](../upgrade-notes.md#upgrading-to-06) to move off it. Check [HAProxy's maintenance table](https://www.haproxy.org/) when choosing a series.

During a rolling upgrade, HAPTIC renders for the lowest HAProxy series in the
fleet. A pod reloads for changes its version can't apply at runtime.

## Feature version requirements

Most chart features work on every supported series. A few require a minimum HAProxy series:

| Feature | Minimum series | Behavior below the minimum |
|---------|----------------|----------------------------|
| SSL/TLS termination, [CRT-list management](../libraries/ssl.md#crt-list-certificate-management), [OCSP stapling](../libraries/ssl.md#ocsp-stapling) | 3.0 | Not applicable — 3.0 is the minimum supported series |
| [SPOA hub](spoa-hub.md) native transport (`mode spop`) | 3.1 | Auto-falls back to `mode tcp`; the hub still works |
| Runtime server creation (`add server`) | 3.0 | Not applicable — all supported series provide it |
| Initial health state on runtime server creation (`init-state`) | 3.1 | HAPTIC omits `init-state`; health checks establish readiness |
| Reload-free route adds and removals (`add backend` / `del backend`) | 3.4 | Routes still work, but adding or removing one reloads the pod |
| [Shared-memory stats persistence](../libraries/base.md#shared-memory-stats-haproxy-33) (`shm-stats-file`) | 3.3 | Silently omitted; stats counters reset on every reload |

Runtime server creation works on every supported series. Runtime backend creation requires 3.4 and an eligible backend shape; see [Reload-free routing](../libraries/reload-free.md).

## Selecting a version

Set `haproxyVersion` in your [complete Helm values file](../deploying-with-helm.md#change-settings).
For example, to select the 3.2 series:

```yaml
haproxyVersion: "3.2"
```

Use these values when installing HAPTIC. For an existing release, follow
[upgrading to a new series](#upgrading-to-a-new-series) below.

## Patch version pinning

By default, the HAProxy pod image is pinned to the latest patch version tested with the chart, looked up from the `haproxyPatchVersions` map in [`charts/haptic/values.yaml`](https://github.com/haproxy-haptic/HAPTIC/blob/main/charts/haptic/values.yaml). An installed release keeps that image until you upgrade it.

To pin a specific patch version yourself, set `haproxy.image.tag`:

```yaml
haproxyVersion: "3.2"
haproxy:
  image:
    tag: "3.2.16"  # Pin to a specific patch
```

## Keeping patches up to date

A chart upgrade adopts its tested HAProxy patch unless you override `haproxy.image.tag`.

If you override `haproxy.image.tag`, update that pin yourself or through your
image-update automation. A chart upgrade won't replace an explicit tag.

## HAProxy Enterprise

Enterprise deployments require:

1. Setting `haproxy.enterprise.enabled: true`
2. Configuring `haproxy.podSpec.imagePullSecrets` with your registry credentials
3. Building your own controller image and pointing `controller.image.repository` (and optionally `controller.image.tag`) at it — the HAPTIC project doesn't distribute enterprise controller images

```yaml
haproxyVersion: "3.2"
haproxy:
  enterprise:
    enabled: true
  podSpec:
    imagePullSecrets:
      - name: hapee-registry-secret
```

With `enterprise.enabled: true`, an empty `haproxy.image.repository` selects
`hapee-registry.haproxy.com/haproxy-enterprise`, and the tag defaults to the
tested revision from `haproxyEnterprisePatchVersions` (for example `3.2r1`).
The same `haproxyVersion` also derives the Enterprise binary path, so image and
binary series can't drift. The chart fails if the selected series has no tested
Enterprise revision. To use a custom registry or pin a specific revision:

```yaml
haproxy:
  image:
    repository: registry.example.com/haproxy-enterprise
    tag: "3.2r2"
```

Check [HAProxy Enterprise release notes](https://www.haproxy.com/documentation/haproxy-enterprise/release-notes/) for available revisions.

## Upgrading to a new series

For a chart-managed Community Edition deployment, this example selects HAProxy
3.3 and clears explicit image tags so the chart selects matching controller and
HAProxy images. It preserves the release's other values:

```bash
helm upgrade haptic oci://ghcr.io/haproxy-haptic/haptic/charts/haptic \
  --version 0.7.0 --namespace haptic --reuse-values \
  --set-string haproxyVersion=3.3 \
  --set-string haproxy.image.tag= --set-string controller.image.tag=
```

If you manage values in Git, make the same changes there before your next
reconciliation. Custom or Enterprise images require tags built for the selected
series; use the [Enterprise setup](#haproxy-enterprise) for those deployments.

The chart defaults to two HAProxy replicas and a `RollingUpdate` strategy with
`maxUnavailable: 0` and `maxSurge: 1`. Kubernetes waits for a replacement pod to
be ready before removing an old one. This also works with one replica if the
cluster has capacity for the replacement. Two or more replicas additionally
allow for an unexpected pod failure. Existing connections drain subject to the
[graceful reload and shutdown limits](./performance.md#graceful-reload-drain-bound).
