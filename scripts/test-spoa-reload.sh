#!/usr/bin/env bash
set -euo pipefail

image="${1:?usage: test-spoa-reload.sh IMAGE}"
test_dir="$(mktemp -d)"
container=""
remove_container() {
    if [[ -n "$container" ]]; then
        docker inspect -f 'hub state: running={{.State.Running}} exit={{.State.ExitCode}} oom={{.State.OOMKilled}} error={{.State.Error}}' "$container" || true
        docker logs "$container" || true
        docker rm -f "$container" >/dev/null
        container=""
    fi
}
trap 'remove_container; rm -rf "$test_dir"' EXIT

wait_for_log() {
    local message="$1" deadline=$((SECONDS + 5))
    until docker logs "$container" 2>&1 | grep -qF "$message"; do
        if [[ "$(docker inspect -f '{{.State.Running}}' "$container")" != true ]]; then
            docker inspect -f 'hub exited with status {{.State.ExitCode}}' "$container" >&2
            return 1
        fi
        if (( SECONDS >= deadline )); then
            printf 'Hub did not report %s\n' "$message" >&2
            return 1
        fi
        sleep 0.1
    done
}

cat > "$test_dir/without-coraza.toml" <<'EOF'
plugin_dir = "/etc/haproxy-spoa-hub/plugins"
log_level = "debug"
[[listeners]]
type = "tcp"
address = "127.0.0.1:12345"
[[plugins]]
name = "mirror"
library = "libmirror_plugin.so"
messages = ["mirror"]
EOF
for library in libcoraza_plugin.so libcoraza_rust_plugin.so; do
    cp "$test_dir/without-coraza.toml" "$test_dir/config.toml"
    cat >> "$test_dir/config.toml" <<EOF
[[plugins]]
name = "coraza"
library = "$library"
messages = ["coraza"]
EOF

    chmod 0644 "$test_dir/config.toml" "$test_dir/without-coraza.toml"
    container="$(docker create --network none --user 65532:65532 "$image" --config /etc/haproxy-spoa-hub/reload-test.toml)"
    docker cp "$test_dir/config.toml" "$container:/etc/haproxy-spoa-hub/reload-test.toml"
    docker start "$container" >/dev/null
    wait_for_log '"all plugins loaded"'
    docker exec "$container" grep -qF "$library" /proc/1/maps
    docker cp "$test_dir/without-coraza.toml" "$container:/etc/haproxy-spoa-hub/reload-next.toml"
    docker exec --user 0 "$container" mv /etc/haproxy-spoa-hub/reload-next.toml /etc/haproxy-spoa-hub/reload-test.toml
    docker kill --signal HUP "$container" >/dev/null
    wait_for_log '"configuration reloaded successfully"'
    # The hub retains native library mappings after plugin instances are retired.
    docker exec "$container" grep -qF "$library" /proc/1/maps
    printf 'SPOA plugin removal preserves the %s mapping\n' "$library"
    remove_container
done
