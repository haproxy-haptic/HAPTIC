# CI build pins

`ci/build-pins.yml` supplies immutable image references and downloaded-tool
checksums to the CI image builds. Keep HAProxy's patch tags aligned with
`charts/haptic/values.yaml` for every supported series. Renovate tracks both
the HAProxy tags and their digests alongside the chart's patch map.

Run `make build-ci-image HAPROXY_VERSION=3.4` to build the same Dockerfile and
pins locally. Node/npm come from a separate pinned toolchain image: Debian's
`npm` dependency chain pulls OpenSSL development headers, which can conflict
with the runtime libraries already installed in a newer HAProxy base.

The Markdown linter's `smol-toml` override fixes
[CVE-2026-85730](https://github.com/advisories/GHSA-7w5x-hrqm-74c2).
The `markdown-it` override fixes
[link parsing](https://github.com/advisories/GHSA-r7fv-28h4-cvq7),
[HTML parsing](https://github.com/advisories/GHSA-vfgh-jcmw-g37q), and
[link-reference parsing](https://github.com/advisories/GHSA-gfr2-rv44-r23c) denial of service.
Remove each override once the linter pins a fixed parser. `make audit` checks
the locked npm dependencies as well as Go vulnerabilities.

Run `make lint` after changing pins. The image-pin check also runs before CI
computes its image tag, so conflicting versions stop downstream image builds
and tests. Its regression tests run through `make test`.

The cache key hashes every file listed in `ci/image-build-inputs.txt`,
including the pin file. A pin change rebuilds the CI images; verify those build
jobs and the affected test matrix before merging. Preserve snapshot dates and
checksums when retrying a transient download failure.

GitHub jobs reuse the images recorded in `.github/ci-toolchains.json` when their
build inputs match. Changed inputs rebuild the toolchain before tests run.
The native ARM jobs select Go's exact version from the pinned `GO_BUILD_IMAGE`
metadata and read other tool versions from the same build pins.

When updating Kind, Helm, kubectl, yq, or cosign, add the matching ARM64 download checksum
to `.github/native-tool-checksums.json`. Keep the checksum for its bootstrap yq
version too. Run `make test-github-verification` to check the complete selection.
