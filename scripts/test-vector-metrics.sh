#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d "$root/.vector-test.XXXXXX")"
container=""
cleanup() {
    if [[ -n "$container" ]]; then
        docker rm -f "$container" >/dev/null
    fi
    rm -rf "$work"
}
trap cleanup EXIT

for server_label in false true counters; do
    profile="$work/server-$server_label"
    mkdir -p "$profile"
    helm template haptic "$root/charts/haptic" --namespace haptic \
        --set vector.requestMetrics.serverLabel="$server_label" > "$profile/chart.yaml"
    python3 "$root/scripts/tests/vector_metrics.py" "$profile" "$server_label" --prepare
    "${CONTROLLER_BIN:-$root/bin/haptic}" validate --file "$profile/native-chart.yaml" \
        --schema-dir "$root/tests/schemas" --test test-vector-omit-empty-log-fields \
        --dump-rendered > "$profile/rendered.txt"
    python3 "$root/scripts/tests/vector_metrics.py" "$profile" "$server_label"
    container=$(docker create --network none \
        "$(cat "$profile/vector-image")" test /tmp/vector.yaml)
    docker cp "$profile/vector.yaml" "$container:/tmp/vector.yaml"
    docker start --attach "$container"
    [[ $(docker inspect -f '{{.State.ExitCode}}' "$container") == 0 ]]
    docker rm "$container" > /dev/null
    container=""
    for compact in false true; do
        rm -f "$profile/haproxy.sock"
        container=$(docker create --network none \
            --mount "type=bind,src=$profile,dst=/probe" \
            "$(cat "$profile/vector-image")" --config "/probe/forwarding-$compact.yaml")
        docker start "$container" > /dev/null
        python3 "$root/scripts/tests/vector_forwarding.py" send "$profile"
        for attempt in {1..100}; do
            docker logs "$container" > "$profile/stdout" 2> "$profile/stderr"
            if rg -q '/framing-complete' "$profile/stdout"; then
                break
            fi
            sleep 0.1
        done
        docker stop "$container" > /dev/null
        docker logs "$container" > "$profile/stdout" 2> "$profile/stderr"
        python3 "$root/scripts/tests/vector_forwarding.py" check "$profile"
        docker rm "$container" > /dev/null
        container=""
    done
done
