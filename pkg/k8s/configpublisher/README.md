# pkg/k8s/configpublisher

Publishes the rendered HAProxy runtime configuration as observable Kubernetes CRDs (`HAProxyCfg`, `HAProxyMapFile`, `HAProxyGeneralFile`, `HAProxyCRTListFile`) plus the SSL Secrets that auxiliary files reference.

## Overview

The reconciliation pipeline produces an authenticated `*renderoutput.Snapshot`; this package materializes it at the Kubernetes boundary and writes its config and artifacts into the cluster. Operators can `kubectl describe` them, GitOps tooling can diff them, and audit logs can trace which controller produced which config. The CRDs are owned by their parent `HAProxyTemplateConfig` so they cascade-delete on uninstall.

This package does **not** push config to HAProxy pods — that's `pkg/dataplane.Client.Sync` driven from `pkg/controller/deployer`. The publisher lives between rendering and deployment as a parallel observability path.

## Quick Start

```go
import (
    "context"

    "k8s.io/client-go/kubernetes"
    "github.com/haproxy-haptic/HAPTIC/pkg/generated/clientset/versioned"
    "github.com/haproxy-haptic/HAPTIC/pkg/k8s/configpublisher"
)

p := configpublisher.NewWithListers(k8sClient, crdClient, listers, logger)
// pass nil listers to fall back to direct API reads

result, err := p.PublishConfig(ctx, &configpublisher.PublishRequest{
    TemplateConfigName:      "haptic-config",
    TemplateConfigNamespace: "haptic",
    TemplateConfigUID:       crd.UID,
    OutputSnapshot:          outputSnapshot,
    // optional: ConfigPath, NameSuffix, ValidationError, CompressionThreshold
    // (see types.go for the full field list)
})
// result.RuntimeConfigName, MapFileNames, SecretNames, SSLCaFileNames,
// GeneralFileNames, CRTListFileNames
```

`NewWithListers` is the production path — passing the informer-backed listers means status updates check the cache before issuing a GET, cutting API-server load on busy clusters. A changed `HAProxyCfg` is updated from the cached copy; a stale copy fails the update's resourceVersion check and the retry reads the live object. Call `SetMetadataClient` as well: the cleanup fence then re-reads only the `HAProxyCfg` metadata before each stale-child deletion and reads the whole object only when its resourceVersion has moved.

`UpdateDeploymentStatus` applies each pod's `deployedToPods` entry with the pod's own field manager. The `HAProxyCfg` entry carries the pod's checksum and is applied first. Auxiliary-file entries go only to the files of the publication at that checksum: until the `HAProxyCfg` publishes it with its auxiliary references, the call returns `ErrRuntimeConfigNotPublished` and the caller retries after the next publication. A drift check (`IsDriftCheck`) reads the `HAProxyCfg` and one list per auxiliary-file kind live, and skips an apply only when the object still shows that pod's last identical apply.

## What Gets Published

| Kind | Field source | Purpose |
|------|--------------|---------|
| `HAProxyCfg` | `req.Config` | The rendered `haproxy.cfg` text |
| `HAProxyMapFile` | one per `AuxiliaryFiles.MapFiles[i]` | Map file contents (path-prefix.map, etc.) |
| `HAProxyGeneralFile` | one per general or GeneralCA artifact | Custom error pages, raw files, and generated CA bundles |
| `HAProxyCRTListFile` | one per `AuxiliaryFiles.CRTListFiles[i]` | crt-list manifests for SSL frontends |
| `Secret` (Opaque) | one per `AuxiliaryFiles.SSLCertificates[i]` | Certificate bundle and target path |
| `Secret` (Opaque) | one per `AuxiliaryFiles.SSLCaFiles[i]` | CA or trust bundle referenced separately from certificates |

Every child of an ordinary publication carries `metadata.ownerReferences` pointing at the parent `HAProxyCfg`, so `kubectl delete haproxycfg <name>` cascades to maps / general files / crt-lists / Secrets.

Retained `HAProxyCfg` objects include both the configuration text and a compressed recovery plan. Their auxiliary files are content-addressed objects owned by the `HAProxyTemplateConfig` and shared by every checkpoint with the same bytes; `PublishRetained` deletes the ones no checkpoint references after it prunes checkpoints. When compression is enabled, their combined size determines whether the configuration text is compressed. A nonpositive `CompressionThreshold` still disables configuration compression.

## Failure Mode

`PublishConfig` succeeds only after the `HAProxyCfg`, every desired child, the complete child-reference status, and removal of obsolete owned children have succeeded. A required write or cleanup failure returns an `IncompletePublicationError` identifying the failed stage and resource. Repeating the same request is safe: resources already in the desired state are skipped, and incomplete work resumes. The publisher canonicalizes identical file definitions and rejects conflicting Dataplane storage identities before its first API write. Children use immutable auxiliary-set names, and one status update commits the set ID with all child references. Readers verify the set annotation on every referenced child, including certificate and CA Secret metadata, before advancing. They never need Secret data to resolve the set and can retain the preceding complete set while a newer publication is partial. Auxiliary-set annotations, committed parent references, and object-version preconditions fence cleanup, preventing a retired publication from deleting newer output. Invalid publications suffix their child names with `-invalid`, keeping the last valid artifact set intact. Long, colliding, or foreign-owned names receive stable identity hashes, so every resource name stays valid and one runtime config never takes over another's child.

Publication status patches change only auxiliary references or the validation error. Each patch atomically checks the runtime config UID, owner references, complete spec, and auxiliary-set annotation; concurrent pod-status updates do not invalidate those checks. An empty merge patch initializes a missing status object without replacing fields. Unchanged-reference elision also checks publication identity before reporting success.

## See Also

- [`pkg/dataplane`](../../dataplane/) — the live HAProxy push, separate from this observability path
- [`pkg/controller/configpublisher`](../../controller/configpublisher/) — event adapter that drives this publisher
- [`pkg/apis/haproxytemplate/v1alpha1`](../../apis/haproxytemplate/v1alpha1/) — the CRD types this package reads/writes

## License

Apache-2.0 — see root `LICENSE`.
