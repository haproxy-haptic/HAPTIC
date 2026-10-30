# ADR-0038: Published charts carry controller image index digests

## Status

Accepted for implementation (2026-10-29), issue [#527](https://github.com/haproxy-haptic/HAPTIC/issues/527).

## Context

A chart upgrade with a consumer's old controller digest correctly fails the
embedded-chart version guard. Consumers should not need to maintain a second
release pin. The controller image embeds the chart, so embedding its own digest
would create a circular build dependency. The image's existing OCI labels come
from its HAProxy base image and cannot establish HAPTIC release identity.

## Decision

After controller publication, stage a copy of the chart and add
`release-images.json`: the chart version and a digest for every supported HAProxy
series. Resolve immutable multi-platform indexes, require Linux amd64, arm64, and
arm/v7 manifests, and verify their signatures against this release workflow's
exact tag identity before packaging. Never modify the source or embedded chart,
or replace an already published release.

The shared image helper selects an explicit digest first, an explicit base tag
second, then the packaged series digest. A repository override changes only the
repository, allowing byte-preserving mirrors. Without the file, source and
development charts retain the appVersion tag fallback. Present but invalid,
incomplete, or version-mismatched metadata fails rendering.

The preflight hook carries the exact metadata file in its existing values Secret
and mounts that key read-only at `/usr/share/haptic/chart/release-images.json`.
The embedded chart can then render the same input without changing operator
values. This adds no CLI flag, public value, registry lookup, or RBAC permission.
The chart version guard and complete-set validation remain mandatory.

Release checks exercise the staged chart and the hook's reconstructed embedded
chart with identical values, compare renders, and verify all controller roles.
Negative checks cover missing platforms, signature failure, malformed metadata,
missing series, and version mismatch. Native multi-architecture pulls and a fresh
Kind install qualify the published-image and Secret mount paths before closure.

## Alternatives

Keeping consumer-maintained digest overrides retains duplicated version selection.
A separate metadata artifact requires another fetch or packaging dependency.
Putting resolved digests into hook values changes the values visible to operator
templates. A mounted chart file preserves them and uses the existing Secret.

## Complexity gate

| Question | Answer |
|----------|--------|
| 1: Observed impact | 1 reported upgrade rejected with an old consumer digest; no unsafe rollout |
| 2: Accepted risk | No; #527 explicitly requests removing duplicated pins |
| 3: Common paths | Yes: Helm, Flux, Argo CD, offline renders, and byte-preserving mirrors |
| 4: Prior art | Yes: Cilium generates release image digest defaults |
| 5: Permissions, credentials, runtime egress | 0 new grants, credentials, or network calls |
| 6: New modes | 1 fail-closed metadata validation; existing source tag fallback retained |
| 7: Public knobs | 0 |
| 8: External coupling | 0 new systems; existing release registry and signature verification |
| 9: Fleet cost | 0 API calls or watches per pod; one small Secret key and one hook file mount |
| 10: Upgrade and rollback | Yes: immutable chart metadata follows the selected release; old charts retain existing behavior |
| 11: Smaller sufficient fix | No: documenting a second consumer pin does not meet the requested release contract |

## References

- [Helm chart files](https://helm.sh/docs/chart_template_guide/accessing_files/)
- [Kubernetes subPath mounts](https://kubernetes.io/docs/concepts/storage/volumes/#using-subpath)
- [OCI image indexes](https://specs.opencontainers.org/image-spec/image-index/)
- [Cilium release values template](https://github.com/cilium/cilium/blob/main/install/kubernetes/cilium/values.yaml.tmpl)
- [Controller-first upgrades](0036-controller-first-template-upgrades.md)
