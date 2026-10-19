#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/../.."
test_dir="$(mktemp -d)"
trap 'rm -rf "$test_dir"' EXIT

mkdir "$test_dir/bin"
cat > "$test_dir/bin/docker" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$STUB_LOG"
case "$1" in
    build) printf '%s\n' "$@" > "$STUB_DIR/build-args" ;;
    exec) tr ':' ' ' < "$STUB_DIR/loaded" ;;
    version) echo amd64 ;;
esac
EOF
cat > "$test_dir/bin/kind" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$3" >> "$STUB_DIR/loaded"
EOF
chmod 0755 "$test_dir/bin/docker" "$test_dir/bin/kind"
touch "$test_dir/loaded"
export PATH="$test_dir/bin:$PATH" STUB_DIR="$test_dir" STUB_LOG="$test_dir/docker.log"

# shellcheck source=../start-dev-env.sh
source scripts/start-dev-env.sh
VERBOSE=true
compute_haproxy_image
source_hash="$(scripts/source-hash.sh)"

unset SPOA_TAG
build_and_load_local_image > "$test_dir/build.log" 2>&1 || { cat "$test_dir/build.log" >&2; exit 1; }

build_arg() { grep -A1 -x -- --build-arg "$test_dir/build-args" | grep "^$1=" | cut -d= -f2- || true; }
version="$(build_arg GIT_TAG)"
[[ -n "$version" && "$version" != unknown ]] || { echo "Dev controller image has no version (GIT_TAG='$version')" >&2; exit 1; }
[[ "$(build_arg SOURCE_HASH)" == "$source_hash" ]] || { echo "Dev controller image source hash differs from $source_hash" >&2; exit 1; }
[[ "$(build_arg HAPROXY_IMAGE)" == "$(bash scripts/chart-haproxy-image.sh "$HAPROXY_VERSION")" ]] || {
    echo "Dev controller image does not use the chart's HAProxy $HAPROXY_VERSION image" >&2; exit 1; }
grep -qx "haptic:dev-${source_hash}-haproxy${HAPROXY_VERSION}" "$test_dir/loaded" || { echo "Dev controller image was not loaded into kind" >&2; exit 1; }

[[ "$SPOA_IMAGE" == haptic-spoa:dev-* ]] || { echo "Dev spoa-hub image '$SPOA_IMAGE' is not the local build" >&2; exit 1; }
grep -qx "$SPOA_IMAGE" "$test_dir/loaded" || { echo "Dev spoa-hub image was not loaded into kind" >&2; exit 1; }
build_helm_args false false false
helm_args=" ${HELM_ARGS[*]} "
[[ "$helm_args" == *" spoaHub.image.repository=haptic-spoa "* && "$helm_args" == *" spoaHub.image.tag=${SPOA_IMAGE#*:} "* ]] || {
    echo "Helm arguments do not select the local spoa-hub image" >&2; exit 1; }

SPOA_TAG=0.0.0-test
build_helm_args false false false
helm_args=" ${HELM_ARGS[*]} "
[[ "$helm_args" == *" spoaHub.image.tag=0.0.0-test "* && "$helm_args" != *"spoaHub.image.repository"* ]] || {
    echo "SPOA_TAG does not select the published spoa-hub image" >&2; exit 1; }

echo "Dev environment image tests passed"
