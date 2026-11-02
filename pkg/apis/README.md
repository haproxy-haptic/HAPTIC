# pkg/apis

Kubernetes API type definitions for the controller's CRDs.

## Overview

`pkg/apis/haproxytemplate/v1alpha1/` holds the Go type definitions plus kubebuilder markers for every CRD the controller owns:

- `HAProxyTemplateConfig` — primary input CRD that operators write
- `HAProxyCfg` — published rendered HAProxy config (output)
- `HAProxyMapFile`, `HAProxyGeneralFile`, `HAProxyCRTListFile` — published auxiliary files (output)

All `zz_generated_deepcopy.go` files are produced by `controller-gen`. The OpenAPI CRD manifests in `charts/haptic/charts/haptic-crds/crds/` are also generated from these types — `make generate` regenerates both.

## Workflow

```bash
# After editing pkg/apis/haproxytemplate/v1alpha1/types_*.go:
make generate                    # runs controller-gen + clientset-gen
make verify-generate             # CI runs this; fails if any output drifted
```

Never hand-edit anything in this tree's `zz_generated_*.go` files or the corresponding output under `pkg/generated/` and `charts/haptic/charts/haptic-crds/crds/` — they are recreated from scratch on every `make generate`.

## See Also

- `pkg/generated/` — typed clientset, informers, and listers generated from these types
- `charts/haptic/charts/haptic-crds/crds/` — OpenAPI CRD manifests generated from these types
- `docs/site/docs/crd-reference.md` — operator-facing field reference
- `Makefile` — `generate` / `generate-crds` / `generate-deepcopy` / `generate-clientset` targets

## License

Apache-2.0 — see root `LICENSE`.
