#!/usr/bin/env bash
# Download and verify upstream SPOA plugin .so files for all architectures
# named in versions-spoa.env, stage them into ./plugins/<arch>/<libname>.so
# for the build-spoa-image job to consume via --build-context.
#
# Release files are kept in .cache/spoa-plugins and reused;
# every run verifies each one, whether cached or downloaded.
#
# Verification (per file):
#   1. SHA256 against the upstream-published `SHA256SUMS` manifest
#   2. cosign verify-blob against the upstream project's tag identity
#
# Outputs (overwrites):
#   plugins/<arch>/<libname>.so
#
# Requires: bash, curl, sha256sum, cosign, awk.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck disable=SC1091
source "${REPO_ROOT}/versions-spoa.env"

GITHUB_ISSUER="https://token.actions.githubusercontent.com"
# Releases cut before the move to GitHub keep their original GitLab CI signatures.
GITLAB_ISSUER="https://gitlab.com"

# plugin shortname -> upstream version variable
declare -A PLUGINS=(
    [api-gateway]="${SPOA_PLUGIN_API_GATEWAY_VERSION}"
    [coraza]="${SPOA_PLUGIN_CORAZA_VERSION}"
    [external-auth]="${SPOA_PLUGIN_EXTERNAL_AUTH_VERSION}"
    [fingerprinting]="${SPOA_PLUGIN_FINGERPRINTING_VERSION}"
    [maxmind]="${SPOA_PLUGIN_MAXMIND_VERSION}"
    [mirror]="${SPOA_PLUGIN_MIRROR_VERSION}"
    [rate-limit]="${SPOA_PLUGIN_RATE_LIMIT_VERSION}"
    [sso-auth]="${SPOA_PLUGIN_SSO_AUTH_VERSION}"
)

# plugin shortname -> SO_NAME prefix (matches the upstream filename before
# the -<arch>-glibc<glibc> suffix). Most plugins follow the
# `lib<name>_plugin` convention; sso-auth's Cargo `name = ...` differs and
# produces `libhaproxy_spoa_hub_plugin_sso_auth.so` instead.
declare -A LIB_NAMES=(
    [api-gateway]="libapi_gateway_plugin"
    [coraza]="libcoraza_plugin libcoraza_rust_plugin"
    [external-auth]="libexternal_auth_plugin"
    [fingerprinting]="libfingerprinting_plugin"
    [maxmind]="libmaxmind_plugin"
    [mirror]="libmirror_plugin"
    [rate-limit]="librate_limit_plugin"
    [sso-auth]="libhaproxy_spoa_hub_plugin_sso_auth"
)

ARCHES=(amd64 arm64 armv7)
CACHE_DIR="${REPO_ROOT}/.cache/spoa-plugins"

fetch() {
    local target="$1" url="$2" headers
    headers="$(mktemp "${WORKDIR}/headers.XXXXXX")"
    # Exponential backoff over five minutes: only a cache miss downloads, and
    # release assets have answered 500 for longer than a fixed 45-second window.
    if ! curl --fail --silent --show-error --location \
        --retry 9 --retry-max-time 300 --retry-all-errors \
        --dump-header "${headers}" \
        --output "${target}" "${url}"; then
        echo "  ERROR: download failed: ${url}" >&2
        # Redirect headers can contain credentials; report only safe support identifiers.
        LC_ALL=C awk '
            { sub(/\r$/, "") }
            $1 ~ /^HTTP\/[0-9.]+$/ && $2 ~ /^[0-9][0-9][0-9]$/ {
                print "    HTTP status: " $2
            }
            tolower($1) == "x-request-id:" && NF == 2 && length($2) <= 256 &&
                $2 ~ /^[A-Za-z0-9][A-Za-z0-9._:-]*$/ {
                print "    X-Request-ID: " $2
            }
        ' "${headers}" >&2
        exit 1
    fi
}

# Sets REPLY to the cached copy, or downloads the file into staging.
release_file() {
    local name="$1"
    if [ -f "${release}/${name}" ]; then
        REPLY="${release}/${name}"
        reused=$((reused + 1))
    else
        REPLY="${staging}/${name}"
        fetch "${REPLY}" "${pkg_base}/${name}"
        downloaded=$((downloaded + 1))
    fi
}

reject() {
    echo "  ERROR: $1" >&2
    if [ -d "${release}" ]; then
        echo "  Cached files in ${release} failed verification; delete that directory to download them again." >&2
    fi
    exit 1
}

mkdir -p plugins "${CACHE_DIR}"
WORKDIR="$(mktemp -d)"
trap 'rm -rf "${WORKDIR}"' EXIT

# Drop unpinned releases so a cache restored from an older pin set stays bounded.
for cached in "${CACHE_DIR}"/*/*; do
    [ -d "${cached}" ] || continue
    cached_plugin="$(basename "$(dirname "${cached}")")"
    [ "${PLUGINS[${cached_plugin}]:-}" = "$(basename "${cached}")" ] || rm -rf "${cached}"
done

# Retries while cosign fails to fetch Sigstore's TUF metadata; any verdict returns at once.
verify_signature() {
    local identity="$1" issuer="$2" trust=() delay
    # Only protobuf bundles accept --trusted-root; legacy ones refresh TUF online per call.
    if grep -q '"mediaType" *: *"application/vnd.dev.sigstore.bundle' "${bundle}"; then
        trust=(--trusted-root "${trusted_root}")
    fi
    for delay in 1 2 4 8 16 32 64 0; do
        if REPLY="$(cosign verify-blob "${so}" --bundle "${bundle}" "${trust[@]}" \
                --certificate-identity "${identity}" --certificate-oidc-issuer "${issuer}" 2>&1 > /dev/null)"; then
            return 0
        fi
        [[ "${delay}" -gt 0 && "${REPLY,,}" == *tuf* ]] || return 1
        sleep "${delay}"
    done
}

# One TUF refresh per run, into a fresh root, since a TUF client trusts the root it finds on disk.
export TUF_ROOT="${WORKDIR}/tuf"
trusted_root="${TUF_ROOT}/tuf-repo-cdn.sigstore.dev/targets/trusted_root.json"
for delay in 1 2 4 8 16 32 64 0; do
    if cosign initialize > /dev/null 2>&1 && [ -s "${trusted_root}" ]; then
        break
    fi
    if [ "${delay}" -eq 0 ]; then
        echo "ERROR: cosign initialize did not produce ${trusted_root}, so no plugin can be verified. Retry once tuf-repo-cdn.sigstore.dev is reachable." >&2
        exit 1
    fi
    sleep "${delay}"
done

verified=0 reused=0 downloaded=0
for plugin in "${!PLUGINS[@]}"; do
    version="${PLUGINS[${plugin}]}"
    read -r -a libraries <<< "${LIB_NAMES[${plugin}]}"
    project_path="haproxy-haptic/haproxy-spoa-hub-plugin-${plugin}"
    pkg_base="https://github.com/${project_path}/releases/download/${version}"
    github_identity="https://github.com/${project_path}/.github/workflows/release.yml@refs/tags/${version}"
    gitlab_identity="https://gitlab.com/${project_path}//.gitlab-ci.yml@refs/tags/${version}"
    release="${CACHE_DIR}/${plugin}/${version}"
    staging="${WORKDIR}/${plugin}"
    mkdir -p "${staging}"

    echo "==> ${plugin} ${version}"

    # Pull the SHA256SUMS manifest as the authoritative file list for this
    # plugin/version. Each line is "<sha256>  <filename>".
    release_file SHA256SUMS
    sums="${REPLY}"

    for lib_name in "${libraries[@]}"; do
        for arch in "${ARCHES[@]}"; do
            suffixed="${lib_name}-${arch}-glibc${SPOA_PLUGIN_GLIBC_VERSION}.so"

            if ! grep -q "  ${suffixed}\$" "${sums}"; then
                reject "${suffixed} not present in upstream SHA256SUMS"
            fi

            release_file "${suffixed}"
            so="${REPLY}"
            release_file "${suffixed}.cosign.bundle"
            bundle="${REPLY}"

            expected=$(awk -v f="${suffixed}" '$2 == f {print $1}' "${sums}")
            actual=$(sha256sum "${so}" | awk '{print $1}')
            if [ "${expected}" != "${actual}" ]; then
                reject "SHA256 mismatch for ${suffixed} (expected ${expected}, actual ${actual})"
            fi

            # Cosign keyless verification anchored to this plugin's project tag.
            if ! verify_signature "${github_identity}" "${GITHUB_ISSUER}" &&
                ! verify_signature "${gitlab_identity}" "${GITLAB_ISSUER}"; then
                echo "${REPLY}" >&2
                reject "cosign rejected the signature of ${suffixed}"
            fi

            mkdir -p "plugins/${arch}"
            cp "${so}" "plugins/${arch}/${lib_name}.so"
            verified=$((verified + 1))
            echo "  ${arch}: verified ${suffixed}"
        done
    done

    # Only files that passed verification enter the cache.
    mkdir -p "${release}"
    find "${staging}" -type f -exec mv -t "${release}" {} +
done

echo
echo "OK: verified ${verified} plugin .so files from ${#PLUGINS[@]} releases for ${#ARCHES[@]} architectures (${reused} cached files reused, ${downloaded} downloaded)."
