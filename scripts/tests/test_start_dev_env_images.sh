#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/../.."
test_dir="$(mktemp -d)"
trap 'rm -rf "$test_dir"' EXIT

real_make="$(command -v make)"
mkdir "$test_dir/bin"
cat > "$test_dir/bin/docker" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$STUB_LOG"
case "$1" in
    build) printf '%s\n' "$@" > "$STUB_DIR/build-args" ;;
    exec) tr ':' ' ' < "$STUB_DIR/loaded" ;;
    image) exit 1 ;;
    version) echo amd64 ;;
esac
EOF
# Records the spoa-hub build instead of running it: spoa-prep needs network and cosign.
cat > "$test_dir/bin/make" <<EOF
#!/usr/bin/env bash
if [[ " \$* " == *" spoa-hub-image "* ]]; then
    printf '%s\n' "\$@" > "\$STUB_DIR/spoa-make-args"
    exit 0
fi
exec "$real_make" "\$@"
EOF
cat > "$test_dir/bin/kind" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$3" >> "$STUB_DIR/loaded"
EOF
chmod 0755 "$test_dir/bin/docker" "$test_dir/bin/kind" "$test_dir/bin/make"
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
grep -qx "SPOA_DEV_IMAGE=$SPOA_IMAGE" "$test_dir/spoa-make-args" && grep -qx "SPOA_DEV_PLATFORM=linux/amd64" "$test_dir/spoa-make-args" || {
    echo "Dev spoa-hub image was not built with make spoa-hub-image" >&2; exit 1; }
grep -qx "$SPOA_IMAGE" "$test_dir/loaded" || { echo "Dev spoa-hub image was not loaded into kind" >&2; exit 1; }
build_helm_args false false false
helm_args=" ${HELM_ARGS[*]} "
[[ "$helm_args" == *" spoaHub.image.repository=haptic-spoa "* && "$helm_args" == *" spoaHub.image.tag=${SPOA_IMAGE#*:} "* ]] || {
    echo "Helm arguments do not select the local spoa-hub image" >&2; exit 1; }

grep -qx -- "--no-cache" "$test_dir/build-args" && { echo "Dev controller build skipped the cache without --force-rebuild" >&2; exit 1; }
FORCE_REBUILD=true
build_and_load_local_image > "$test_dir/build.log" 2>&1 || { cat "$test_dir/build.log" >&2; exit 1; }
grep -qx -- "--no-cache" "$test_dir/build-args" && grep -qx "DOCKER_BUILD_FLAGS=--no-cache" "$test_dir/spoa-make-args" || {
    echo "--force-rebuild does not disable the build cache for both images" >&2; exit 1; }
for target in docker-build spoa-hub-image; do
    "$real_make" -n --no-print-directory "$target" DOCKER_BUILD_FLAGS=--no-cache SPOA_DEV_IMAGE=x > "$test_dir/dry-run"
    grep -q -- "build --no-cache" "$test_dir/dry-run" || {
        echo "make $target ignores DOCKER_BUILD_FLAGS" >&2; exit 1; }
done

SPOA_TAG=0.0.0-test
build_helm_args false false false
helm_args=" ${HELM_ARGS[*]} "
[[ "$helm_args" == *" spoaHub.image.tag=0.0.0-test "* && "$helm_args" != *"spoaHub.image.repository"* ]] || {
    echo "SPOA_TAG does not select the published spoa-hub image" >&2; exit 1; }

echo "Dev environment image tests passed"
