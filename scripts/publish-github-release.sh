#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
phase="${1:?usage: publish-github-release.sh spoa|controller|chart}"
[[ "$phase" == spoa || "$phase" == controller || "$phase" == chart ]] || exit 2
: "${GITHUB_REPOSITORY:?}" "${GITHUB_WORKFLOW_REF:?}"
version="$(<VERSION)"
repository="ghcr.io/${GITHUB_REPOSITORY%%/*}/haptic"
repository="${repository,,}"
identity="https://github.com/${GITHUB_WORKFLOW_REF}"
verification=(--certificate-identity "$identity" --certificate-oidc-issuer https://token.actions.githubusercontent.com)
mkdir -p .publication

[[ "$GITHUB_WORKFLOW_REF" == "${GITHUB_REPOSITORY}/.github/workflows/release.yml@refs/tags/v${version}" ]] || {
    printf 'Run release publication from the matching version tag\n' >&2
    exit 1
}

[[ "$(yq -r '.version' charts/haptic/Chart.yaml)" == "$version" &&
   "$(yq -r '.appVersion' charts/haptic/Chart.yaml)" == "$version" &&
   "$(git rev-parse "refs/tags/v${version}^{commit}")" == "$(git rev-parse HEAD)" ]] || {
    printf 'Release tag, VERSION and chart versions differ; publish the verified release commit\n' >&2
    exit 1
}
[[ "$(yq -r '.controller.image.repository' charts/haptic/values.yaml)" == "$repository" &&
   "$(yq -r '.spoaHub.image.repository' charts/haptic/values.yaml)" == "${repository}/spoa-hub" ]] || {
    printf 'Chart image repositories differ from GitHub publication; complete the registry migration first\n' >&2
    exit 1
}

digest_of() {
    bash scripts/retry.sh docker buildx imagetools inspect "$1" --format '{{json .Manifest.Digest}}' |
        jq -er 'select(test("^sha256:[0-9a-f]{64}$"))'
}

sign_and_attest() {
    local reference="$1" name="$2" format="$3" predicate_type="$4"
    cosign sign --yes "$reference"
    cosign verify "${verification[@]}" "$reference" > ".publication/${name}-signature.json"
    syft scan --platform linux/amd64 "$reference" -o "${format}=.publication/${name}-sbom.json"
    cosign attest --yes --type "$predicate_type" --predicate ".publication/${name}-sbom.json" "$reference"
    cosign verify-attestation "${verification[@]}" --type "$predicate_type" "$reference" \
        > ".publication/${name}-attestation.json"
}

case "$phase" in
    spoa)
        make spoa-prep
        CI_REGISTRY_IMAGE="$repository" bash scripts/spoa-release-image.sh verify
        digest="$(jq -er '."containerimage.digest" | select(test("^sha256:[0-9a-f]{64}$"))' metadata.json)"
        reference="${repository}/spoa-hub@${digest}"
        sign_and_attest "$reference" spoa cyclonedx-json cyclonedx
        printf '%s\n' "$reference" > .publication/spoa-reference.txt
        ;;
    controller)
        make prepare-github-release-config
        for series in $(yq -r '.haproxyPatchVersions | keys | .[]' charts/haptic/values.yaml); do
            pin="HAPROXY_IMAGE_${series//./}"
            image="$(bash scripts/chart-haproxy-image.sh "$series")"
            export "$pin=$image"
        done
        SOURCE_HASH="$(bash scripts/source-hash.sh)"
        [[ "$SOURCE_HASH" =~ ^[0-9a-f]{12}$ ]]
        export SOURCE_HASH
        awk -v ver="$version" '/^## \[/ { if (found) exit; if (index($0, "[" ver "]")) found=1 } found { print }' \
            CHANGELOG.md > .publication/release-notes.md
        [[ -s .publication/release-notes.md ]]
        goreleaser release --config .pilot/goreleaser.github.yml --clean --release-notes .publication/release-notes.md
        # shellcheck source=../versions.env
        source versions.env
        for variant in $HAPROXY_VERSIONS varnish; do
            if [[ "$variant" == varnish ]]; then
                target="${repository}/varnish:${version}"
            else
                target="${repository}:${version}-haproxy${variant}"
            fi
            digest="$(digest_of "$target")"
            sign_and_attest "${target%:*}@${digest}" "$variant" spdx-json spdxjson
        done
        cosign verify-blob "${verification[@]}" --bundle dist/checksums.txt.sigstore.json dist/checksums.txt
        cp dist/checksums.txt dist/checksums.txt.sigstore.json .publication/
        ;;
    chart)
        # shellcheck source=../versions.env
        source versions.env
        [[ ! -e charts/haptic/release-images.json ]]
        rm -rf .publication/chart
        mkdir -p .publication/chart
        cp -a charts/haptic/. .publication/chart/
        python3 -m scripts.ci.release_images --repository "$repository" --version "$version" \
            --series $HAPROXY_VERSIONS --identity "$identity" \
            --output .publication/chart/release-images.json
        helm package .publication/chart --destination .publication
        python3 -m scripts.ci.release_chart --chart ".publication/haptic-${version}.tgz" --embedded charts/haptic
        helm push ".publication/haptic-${version}.tgz" "oci://${repository}/charts"
        digest="$(digest_of "${repository}/charts/haptic:${version}")"
        reference="${repository}/charts/haptic@${digest}"
        cosign sign --yes "$reference"
        cosign verify "${verification[@]}" "$reference" > .publication/chart-signature.json
        mkdir .publication/chart-retrieved
        helm pull "oci://${repository}/charts/haptic" --version "$version" --destination .publication/chart-retrieved
        cmp ".publication/haptic-${version}.tgz" ".publication/chart-retrieved/haptic-${version}.tgz"
        git ls-remote --tags --refs origin > .publication/release-tags.txt
        latest=0
        bash scripts/release-is-latest.sh "v${version}" < .publication/release-tags.txt || latest=$?
        [[ "$latest" -le 1 ]] || exit "$latest"
        if [[ "$latest" == 0 ]]; then
            # shellcheck source=../versions.env
            source versions.env
            for series in $HAPROXY_VERSIONS; do
                docker buildx imagetools create --tag "${repository}:latest-haproxy${series}" \
                    "${repository}:${version}-haproxy${series}"
            done
            docker buildx imagetools create --tag "${repository}/spoa-hub:latest" \
                "${repository}/spoa-hub:${version}"
        fi
        ;;
esac
