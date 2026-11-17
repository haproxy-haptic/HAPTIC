#!/usr/bin/env bash
set -euo pipefail

# render-snapshot.sh - Snapshot every bundled validationTest's rendered output.
#
# Renders the chart with every template library enabled, runs the whole
# validationTest corpus, and writes each test's output as
#
#     <dir>/<test-name>/haproxy.cfg
#     <dir>/<test-name>/maps/<name>
#     <dir>/<test-name>/files/<name>
#     <dir>/<test-name>/secret-files.txt   (names only, never content)
#
# so a change that is meant to leave the rendered configuration alone can be
# proven to: snapshot two checkouts and compare the trees.
#
#     git worktree add /tmp/base origin/main
#     (cd /tmp/base && make build && hack/render-snapshot.sh /tmp/snap-base)
#     make build && hack/render-snapshot.sh /tmp/snap-head
#     diff -rw /tmp/snap-base /tmp/snap-head
#
# `-w` because a refactor is allowed to move whitespace inside a directive;
# drop it to hold the output byte-identical. A content-only change to a
# certificate, secret file or sensitive map doesn't show in the diff.
#
# Usage: hack/render-snapshot.sh <output-dir>
#
# Env:
#   CONTROLLER_BIN    haptic binary to use (default: bin/haptic)
#   HAPROXY_VERSION   haproxyVersion value to render with (default: chart default)
#
# Exits non-zero when the render itself fails. Failing ASSERTIONS do not stop
# the snapshot — a snapshot of a red corpus is still the thing to diff.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
CHART_DIR="${PROJECT_ROOT}/charts/haptic"
CONTROLLER_BIN="${CONTROLLER_BIN:-${PROJECT_ROOT}/bin/haptic}"

if [[ $# -ne 1 || "$1" == "-h" || "$1" == "--help" ]]; then
    sed -n '3,32p' "${BASH_SOURCE[0]}" >&2
    exit 1
fi
OUT_DIR="$1"

if [[ ! -x "$CONTROLLER_BIN" ]]; then
    echo "Error: no haptic binary at $CONTROLLER_BIN - run 'make build' first" >&2
    exit 1
fi
for tool in helm yq; do
    command -v "$tool" >/dev/null || { echo "Error: $tool not found" >&2; exit 1; }
done

HAPROXY_VERSION_ARG=()
if [[ -n "${HAPROXY_VERSION:-}" ]]; then
    HAPROXY_VERSION_ARG=("--set" "haproxyVersion=${HAPROXY_VERSION}")
fi

CONFIG=$(mktemp /tmp/haptic-snapshot-config-XXXXXX.yaml)
trap 'rm -f "$CONFIG"' EXIT

echo "Rendering the chart with every template library enabled..." >&2
helm template "$CHART_DIR" \
    --namespace default \
    "${HAPROXY_VERSION_ARG[@]}" \
    --set controller.templateLibraries.gateway.enabled=true \
    --set controller.templateLibraries.gateway.experimentalChannel=true \
    --set controller.templateLibraries.hapticAnnotations.enabled=true \
    --set controller.templateLibraries.haproxytech.enabled=true \
    --set controller.templateLibraries.haproxyIngress.enabled=true \
    --set controller.templateLibraries.nginxIngress.enabled=true \
    | yq 'select(.kind == "HAProxyTemplateConfig" or .kind == "HAProxyTemplateLibrary")' \
    > "$CONFIG"

SCHEMA_DIR_ARGS=()
if [[ -d "${PROJECT_ROOT}/tests/schemas" && -z "${HAPTIC_SCHEMA_DIR:-}" ]]; then
    SCHEMA_DIR_ARGS=("--schema-dir=${PROJECT_ROOT}/tests/schemas")
fi

rm -rf "$OUT_DIR"
mkdir -p "$OUT_DIR"

echo "Rendering the validationTest corpus into $OUT_DIR..." >&2
# A failing assertion exits 1 but has still written the snapshot, which is what
# this script is for; only a hard error (exit >= 2) is fatal here.
RC=0
"$CONTROLLER_BIN" validate --file "$CONFIG" "${SCHEMA_DIR_ARGS[@]}" \
    --snapshot-dir "$OUT_DIR" --output summary >/dev/null || RC=$?
if [[ $RC -gt 1 ]]; then
    echo "Error: the render failed (exit $RC)" >&2
    exit "$RC"
fi
if [[ $RC -eq 1 ]]; then
    echo "Note: some assertions failed; the snapshot was still written." >&2
fi

echo "Snapshotted $(find "$OUT_DIR" -mindepth 1 -maxdepth 1 -type d | wc -l) tests into $OUT_DIR" >&2
