#!/usr/bin/env bash
set -euo pipefail

runner="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/bench-gateway-api.sh"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/bench-gateway-api-test.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT

assert_eq() {
    local expected="$1"
    local actual="$2"
    [[ "$actual" == "$expected" ]] || {
        printf 'expected %q, got %q\n' "$expected" "$actual" >&2
        exit 1
    }
}

assert_eq 1 "$(bash -c 'source "$1"; duration_seconds 1s' bash "$runner")"
assert_eq 120 "$(bash -c 'source "$1"; duration_seconds 2m' bash "$runner")"
assert_eq 3600 "$(bash -c 'source "$1"; duration_seconds 1h' bash "$runner")"

cat > "$tmp/network-pod.json" <<'EOF'
{"items":[{"metadata":{"uid":"network-pod"},"status":{"phase":"Running","containerStatuses":[{"name":"calico-node","containerID":"containerd://network","ready":true,"restartCount":0,"state":{"running":{}}}]}}]}
EOF
bash -c 'source "$1"; network_pod_identity' bash "$runner" < "$tmp/network-pod.json" > "$tmp/network-identity.json"
assert_eq network-pod "$(jq -r '.[0].uid' "$tmp/network-identity.json")"
for mutation in \
    '.items = []' \
    '.items += .items' \
    '.items[0].metadata.uid = ""' \
    '.items[0].metadata.deletionTimestamp = "2026-10-13T15:00:00Z"' \
    '.items[0].status.phase = "Pending"' \
    '.items[0].status.containerStatuses = []' \
    '.items[0].status.containerStatuses += .items[0].status.containerStatuses' \
    '.items[0].status.containerStatuses[0].containerID = ""' \
    '.items[0].status.containerStatuses[0].ready = false' \
    '.items[0].status.containerStatuses[0].restartCount = 1' \
    '.items[0].status.containerStatuses[0].state = {terminated: {reason: "OOMKilled"}}'; do
    jq "$mutation" "$tmp/network-pod.json" > "$tmp/network-invalid.json"
    if bash -c 'source "$1"; network_pod_identity' bash "$runner" < "$tmp/network-invalid.json" >/dev/null 2>&1; then
        echo "network identity accepted invalid state: $mutation" >&2
        exit 1
    fi
done

mkdir -p "$tmp/network-check/cluster"
network_health_check() {
    bash -c '
        source "$1"
        BENCH_OUTPUT_DIR="$2"
        kubectl() { cat "$network_fixture"; }
        network_fixture="$3"
        capture_network_health baseline
        network_fixture="$4"
        capture_network_health after
    ' bash "$runner" "$tmp/network-check" "$tmp/network-pod.json" "$1"
}
network_health_check "$tmp/network-pod.json"
jq '.items[0].metadata.uid = "replacement"' "$tmp/network-pod.json" > "$tmp/network-replacement.json"
if network_health_check "$tmp/network-replacement.json" > "$tmp/network-replacement.log" 2>&1; then
    echo "network health accepted a replacement Pod" >&2
    exit 1
fi

cat > "$tmp/pods.json" <<'EOF'
{"items":[{"metadata":{"name":"preflight","annotations":{"private":"secret-marker"}},"spec":{"containers":[{"name":"preflight","env":[{"name":"PASSWORD","value":"secret-marker"}],"resources":{"limits":{"memory":"1Gi"}}}]},"status":{"phase":"Failed","containerStatuses":[{"name":"preflight","restartCount":0,"state":{"terminated":{"reason":"OOMKilled","exitCode":137,"message":"secret-marker"}},"lastState":{"terminated":{"reason":"Error","exitCode":1,"message":"secret-marker"}}}]}}]}
EOF
bash -c 'source "$1"; pod_failure_summary' bash "$runner" < "$tmp/pods.json" > "$tmp/pod-summary.json"
assert_eq OOMKilled "$(jq -r '.[0].containers[0].terminated' "$tmp/pod-summary.json")"
assert_eq 137 "$(jq -r '.[0].containers[0].exit_code' "$tmp/pod-summary.json")"
assert_eq Error "$(jq -r '.[0].containers[0].previous_termination' "$tmp/pod-summary.json")"
assert_eq 1Gi "$(jq -r '.[0].limits[0].memory' "$tmp/pod-summary.json")"
if rg -q secret-marker "$tmp/pod-summary.json"; then
    echo "pod_failure_summary exposed pod content" >&2
    exit 1
fi
if bash -c 'source "$1"; duration_seconds 1d' bash "$runner" >/dev/null 2>&1; then
    echo "duration_seconds accepted an invalid suffix" >&2
    exit 1
fi

assert_eq standard "$(BENCH_GATEWAY_API_CHANNEL=standard bash -c 'source "$1"; printf "%s" "$BENCH_GATEWAY_API_CHANNEL"' bash "$runner")"

# pilot-load is stopped by signal at the end of the steady-churn interval; a
# signal exit after that stop is the stop working, before it a crash.
touch "$tmp/stop-issued"
workload_exit() {
    bash -c 'source "$1"; workload_exit_acceptable "$2" "$3" && echo ok || echo fail' bash "$runner" "$1" "$2"
}
assert_eq ok "$(workload_exit 0 "$tmp/missing")"
assert_eq ok "$(workload_exit 0 "$tmp/stop-issued")"
assert_eq ok "$(workload_exit 130 "$tmp/stop-issued")"
assert_eq ok "$(workload_exit 143 "$tmp/stop-issued")"
assert_eq fail "$(workload_exit 143 "$tmp/missing")"
assert_eq fail "$(workload_exit 137 "$tmp/stop-issued")"
assert_eq fail "$(workload_exit 1 "$tmp/stop-issued")"

mkdir "$tmp/bin"
cat > "$tmp/bin/kind" <<'EOF'
#!/usr/bin/env bash
if [[ "$1" == get && "$2" == clusters ]]; then
    printf '%s\n' haptic-gwbench-test haptic-dev
    exit 0
fi
exit 1
EOF
chmod 0755 "$tmp/bin/kind"
assert_eq present "$(PATH="$tmp/bin:$PATH" bash -c 'source "$1"; kind_cluster_state haptic-gwbench-test' bash "$runner")"
assert_eq absent "$(PATH="$tmp/bin:$PATH" bash -c 'source "$1"; kind_cluster_state missing' bash "$runner")"

mkdir -p "$tmp/inventory/cluster"
inventory_check() {
    PATH="$tmp/bin:$PATH" bash -c '
        source "$1"
        trap - EXIT INT TERM
        BENCH_OUTPUT_DIR="$2"
        CLUSTER_NAME=haptic-gwbench-test
        BENCH_ALLOW_COSCHEDULED_CLUSTERS=false
        node_state="$3"
        capture_kind_node_states() {
            jq -n --arg state "$node_state" '\''[
                {cluster: "haptic-gwbench-test", state: {Status: "running", Running: true}},
                {cluster: "haptic-dev", state: {
                    Status: $state, Running: ($state == "running"),
                    Paused: ($state == "paused"), Restarting: ($state == "restarting")
                }}
            ]'\'' > "$1"
        }
        capture_kind_cluster_inventory check present
    ' bash "$runner" "$tmp/inventory" "$1"
}
inventory_check exited
assert_eq '[]' "$(jq -c . "$tmp/inventory/cluster/kind-clusters-check-coscheduled.json")"
for state in running paused restarting unknown; do
    if inventory_check "$state" > "$tmp/inventory/$state.log" 2>&1; then
        echo "cluster inventory accepted a $state Kind node" >&2
        exit 1
    fi
done

cat > "$tmp/values.json" <<'EOF'
{"credentials":{"dataplane":{"password":"secret"}},"controller":{"webhook":{"caBundle":"certificate"}},"kept":"value"}
EOF
BENCH_GATEWAY_API_CHANNEL=standard bash -c 'source "$1"; redact_helm_values "$2" "$3"' bash "$runner" "$tmp/values.json" "$tmp/redacted.json"
jq -e '
    .credentials.dataplane.password == "<redacted>" and
    .controller.webhook.caBundle == "<redacted>" and
    .kept == "value"
' "$tmp/redacted.json" >/dev/null

cat > "$tmp/defaults.json" <<'EOF'
{"controller":{"resources":{"limits":{"memory":"1Gi"},"requests":{"cpu":"100m","memory":"1Gi"}},"livenessProbe":{"httpGet":{"path":"/livez"},"periodSeconds":10},"readinessProbe":{"httpGet":{"path":"/readyz"},"periodSeconds":5}}}
EOF
write_manifest() {
    local controller_resources="$1"
    cat > "$tmp/manifest.yaml" <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: haptic-controller
spec:
  template:
    spec:
      containers:
      - name: controller
        resources: $controller_resources
        livenessProbe: {httpGet: {path: /livez}, periodSeconds: 10}
        readinessProbe: {httpGet: {path: /readyz}, periodSeconds: 5}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: haptic-haproxy
spec:
  template:
    spec:
      containers:
      - name: haproxy
        resources: {requests: {cpu: 250m, memory: 1Gi}}
EOF
}
no_limit_workloads() {
    bash -c 'source "$1"; assert_no_limit_workloads "$2" "$3"' bash "$runner" "$tmp/manifest.yaml" "$tmp/defaults.json"
}
write_manifest '{requests: {cpu: 100m, memory: 1Gi}}'
no_limit_workloads
for variable in GOMEMLIMIT GOMAXPROCS GOGC TMPDIR; do
    write_manifest '{requests: {cpu: 100m, memory: 1Gi}}'
    VARIABLE="$variable" yq -i '(select(.metadata.name == "haptic-controller") | .spec.template.spec.containers[0].env) = [{"name": strenv(VARIABLE), "value": "override"}]' "$tmp/manifest.yaml"
    if no_limit_workloads; then
        echo "assert_no_limit_workloads accepted a retained $variable override" >&2
        exit 1
    fi
done
write_manifest '{requests: {cpu: 100m, memory: 1Gi}, limits: {memory: 1Gi}}'
if no_limit_workloads; then
    echo "assert_no_limit_workloads accepted a controller memory limit" >&2
    exit 1
fi
write_manifest '{requests: {cpu: 100m, memory: 512Mi}}'
if no_limit_workloads; then
    echo "assert_no_limit_workloads accepted controller requests that differ from the chart defaults" >&2
    exit 1
fi
write_manifest '{requests: {cpu: 100m, memory: 1Gi}}'
sed -i 's|path: /readyz|path: /healthz|' "$tmp/manifest.yaml"
if no_limit_workloads; then
    echo "assert_no_limit_workloads accepted a different readiness probe" >&2
    exit 1
fi
write_manifest '{requests: {cpu: 100m, memory: 1Gi}}'
sed -i 's|periodSeconds: 10|periodSeconds: 60|' "$tmp/manifest.yaml"
if no_limit_workloads; then
    echo "assert_no_limit_workloads accepted different liveness timing" >&2
    exit 1
fi

chart="$(dirname "$runner")/../charts/haptic"
yq -o=json '.' "$chart/values.yaml" > "$tmp/defaults.json"
helm template haptic "$chart" \
    --set 'controller.resources.limits=null' \
    --set 'controller.validators.resources.limits=null' \
    --set 'haproxy.resources.limits=null' \
    --set 'haproxy.agent.resources.limits=null' \
    --set 'spoaHub.resources.limits=null' \
    --set 'vector.resources.limits=null' > "$tmp/manifest.yaml"
no_limit_workloads

no_limit_pods() {
    yq -o=json -I=0 '.' "$tmp/manifest.yaml" | jq -s '
        {items: [.[] | select(.kind == "Deployment") |
          {metadata: .spec.template.metadata, spec: .spec.template.spec}]}
    ' > "$tmp/measured-pods.json"
    bash -c 'source "$1"; assert_no_limit_pods "$2" "$3"' bash "$runner" "$tmp/measured-pods.json" "$tmp/defaults.json"
}
no_limit_pods
cp "$tmp/manifest.yaml" "$tmp/product-default-manifest.yaml"
for container in controller haproxy validators spoa-hub agent vector; do
    for variable in GOMEMLIMIT GOMAXPROCS GOGC TMPDIR; do
        cp "$tmp/product-default-manifest.yaml" "$tmp/manifest.yaml"
        CONTAINER="$container" VARIABLE="$variable" yq -i '
            (select(.kind == "Deployment") | .spec.template.spec |
             (.containers[], .initContainers[]?) | select(.name == strenv(CONTAINER)) | .env) +=
            [{"name": strenv(VARIABLE), "value": "override"}]
        ' "$tmp/manifest.yaml"
        if no_limit_workloads || no_limit_pods; then
            echo "benchmark profile accepted $variable tuning in $container" >&2
            exit 1
        fi
    done
    for resource in cpu memory; do
        cp "$tmp/product-default-manifest.yaml" "$tmp/manifest.yaml"
        CONTAINER="$container" RESOURCE="$resource" yq -i '
            (select(.kind == "Deployment") | .spec.template.spec |
             (.containers[], .initContainers[]?) | select(.name == strenv(CONTAINER)) |
             .resources.limits[strenv(RESOURCE)]) = "1"
        ' "$tmp/manifest.yaml"
        if no_limit_workloads || no_limit_pods; then
            echo "benchmark profile accepted a $resource limit in $container" >&2
            exit 1
        fi
    done
done
for gc_value in 100 null; do
    cp "$tmp/product-default-manifest.yaml" "$tmp/manifest.yaml"
    GC_VALUE="$gc_value" yq -i '
        (select(.kind == "Deployment") | .spec.template.spec.initContainers[]? |
         select(.name == "validators" or .name == "spoa-hub") | .env) =
        [{"name": "GOGC", "value": strenv(GC_VALUE)}]
    ' "$tmp/manifest.yaml"
    if no_limit_workloads || no_limit_pods; then
        echo "benchmark profile accepted sidecar GC settings that differ from chart defaults" >&2
        exit 1
    fi
done

# extract_upstream_backend_manifest must lift the program's backendTemplate
# constant byte-for-byte and accept only the Deployment + Service pair.
mkdir -p "$tmp/upstream/tests/probe"
cat > "$tmp/upstream/tests/probe/probe.go" <<'EOF'
package main

const backendTemplate = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: backend
spec:
  selector:
    matchLabels:
      app: backend
---
apiVersion: v1
kind: Service
metadata:
  name: backend
spec:
  ports:
  - name: http
    port: 80
`

const other = "unrelated"
EOF
bash -c 'source "$1"; UPSTREAM_DIR="$3"; extract_upstream_backend_manifest probe "$2"' bash "$runner" "$tmp/backend.yaml" "$tmp/upstream"
assert_eq "$(sed -n '/^const backendTemplate = `/,/^`/p' "$tmp/upstream/tests/probe/probe.go" | sed '1d;$d')" "$(cat "$tmp/backend.yaml")"
cat > "$tmp/upstream/tests/probe/probe.go" <<'EOF'
package main

const backendTemplate = `
apiVersion: v1
kind: Service
metadata:
  name: backend
`
EOF
if bash -c 'source "$1"; UPSTREAM_DIR="$3"; extract_upstream_backend_manifest probe "$2"' bash "$runner" "$tmp/backend.yaml" "$tmp/upstream" >/dev/null 2>&1; then
    echo "extract_upstream_backend_manifest accepted a manifest without the Deployment" >&2
    exit 1
fi

# The readiness-timeout evidence carries the HAProxyCfg snapshot, which is
# megabytes at scale; as a jq argument it exceeded the argument list and the
# run lost its evidence.
scenario="$tmp/scale"
mkdir -p "$scenario"
printf '{"metadata":{"generation":3}}\n' > "$scenario/haproxycfg-baseline.json"
printf 'abc123\n' > "$scenario/haproxycfg-baseline-checksum.txt"
printf '{"attempts":2,"reason_code":"exact-current-timeout","outcome":"deadline","evidence_valid":true,"pass":false,"deadline_reached":true}\n' > "$tmp/readiness.json"
python3 - "$tmp/haproxycfg.json" <<'EOF'
import json, sys
cfg = {"metadata": {"generation": 9}, "spec": {"checksum": "big", "config": "x" * 3_000_000},
       "status": {"deployedToPods": [{"checksum": "big"}]}}
json.dump(cfg, open(sys.argv[1], "w"))
EOF
bash -c 'source "$1"; write_scale_readiness_timeout "$2" 5000 initial-exact-current "$3" "$4"' bash "$runner" "$scenario" "$tmp/haproxycfg.json" "$tmp/readiness.json"
assert_eq big "$(jq -r '.at_scale.checksum' "$scenario/scale-readiness.json")"
assert_eq exact-current-timeout "$(jq -r '.reason_code' "$scenario/scale-readiness.json")"

# The stop grace grows with the routes pilot-load has to delete on INT.
assert_eq 10 "$(bash -c 'source "$1"; workload_stop_grace_seconds 0' bash "$runner")"
assert_eq 60 "$(bash -c 'source "$1"; workload_stop_grace_seconds 5000' bash "$runner")"

# The referenced-map poll reads the parent fresh: a snapshot taken between the
# set-id annotation write and the status write never satisfies the equality it
# checks, while the live object does once both writes landed.
maps="$tmp/maps"
mkdir -p "$maps/bin"
python3 - "$maps" <<'EOF'
import json, sys
d = sys.argv[1]
pod = {"metadata": {"name": "lb-0", "uid": "uid-0"}, "status": {"containerStatuses": []}}
pods = {"items": [pod]}
deployed = [{"podName": "lb-0", "podUID": "uid-0", "podRuntimeID": "rt-0", "checksum": "m1"}]
map_object = {"metadata": {"name": "map-a", "namespace": "haptic", "uid": "map-uid", "resourceVersion": "5",
                           "ownerReferences": [{"apiVersion": "haproxy-haptic.org/v1alpha1", "kind": "HAProxyCfg",
                                                "name": "config", "uid": "cfg-uid", "controller": True}],
                           "annotations": {"haproxy-haptic.org/auxiliary-set-id": "set-2"}},
              "spec": {"mapName": "a", "path": "maps/a.map", "checksum": "m1"},
              "status": {"deployedToPods": deployed}}
refs = [{"kind": "HAProxyMapFile", "name": "map-a", "namespace": "haptic"}]
def cfg(annotation, status_set):
    return {"metadata": {"name": "config", "namespace": "haptic", "uid": "cfg-uid", "resourceVersion": "9", "generation": 4,
                         "annotations": {"haproxy-haptic.org/auxiliary-set-id": annotation}},
            "spec": {"checksum": "c2"},
            "status": {"deployedToPods": [{"podName": "lb-0", "podUID": "uid-0", "checksum": "c2"}],
                       "auxiliaryFiles": {"setID": status_set, "mapFiles": refs}}}
json.dump(cfg("set-2", "set-1"), open(f"{d}/snapshot.json", "w"))
json.dump(cfg("set-2", "set-2"), open(f"{d}/live.json", "w"))
json.dump(pods, open(f"{d}/pods.json", "w"))
json.dump({"items": [map_object]}, open(f"{d}/objects.json", "w"))
EOF
cat > "$maps/bin/kubectl" <<EOF
#!/bin/bash
case "\$2" in
    haproxymapfiles) cat "$maps/objects.json" ;;
    haproxycfg) cat "$maps/live.json" ;;
    *) exit 1 ;;
esac
EOF
chmod +x "$maps/bin/kubectl"
assert_eq 0 "$(PATH="$maps/bin:$PATH" bash -c 'source "$1"; WORK_DIR="$2"; if capture_referenced_map_inventory "$2/snapshot.json" "$2/pods.json" "$2/inventory.json" true; then echo 0; else echo $?; fi' bash "$runner" "$maps")"
assert_eq set-2 "$(jq -r '.status.auxiliaryFiles.setID' "$maps/inventory-cfg.json")"
cat > "$maps/bin/kubectl" <<EOF
#!/bin/bash
case "\$2" in
    haproxymapfiles) cat "$maps/objects.json" ;;
    haproxycfg) cat "$maps/snapshot.json" ;;
    *) exit 1 ;;
esac
EOF
assert_eq "$(bash -c 'source "$1"; echo $READINESS_RESULT_DEADLINE' bash "$runner")" "$(PATH="$maps/bin:$PATH" bash -c 'source "$1"; WORK_DIR="$2"; if capture_referenced_map_inventory "$2/snapshot.json" "$2/pods.json" "$2/inventory.json" true; then echo 0; else echo $?; fi' bash "$runner" "$maps")"

printf 'bench-gateway-api shell tests: OK\n'
