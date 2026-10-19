#!/usr/bin/env bash
set -euo pipefail
umask 077

readonly DEFAULT_BENCH_REF="e81292ed876472804e0a2245876a7c445ab80881"
readonly DEFAULT_GATEWAY_API_VERSION="v1.4.0"
readonly DEFAULT_GATEWAY_API_CHANNEL="experimental"
readonly DEFAULT_BENCH_SCENARIOS="probe,scale,routechange"
readonly DEFAULT_BENCH_GATEWAYS="haptic-bench/haptic"
readonly DEFAULT_PROBE_ROUTES=3000
readonly DEFAULT_ROUTECHANGE_ITERATIONS=20
readonly DEFAULT_ROUTECHANGE_GRACE_PERIOD="200ms"
readonly DEFAULT_SCALE_NAMESPACES=50
readonly DEFAULT_SCALE_ROUTES_PER_NAMESPACE=100
readonly DEFAULT_SCALE_DURATION="10m"
readonly BENCH_GATEWAY_API_CHANNEL="${BENCH_GATEWAY_API_CHANNEL:-${DEFAULT_GATEWAY_API_CHANNEL}}"
readonly DEFAULT_GATEWAY_API_MANIFEST_SHA256="0414b160767377e85fd362855501200c6b83b84758bcd532652e3fe1cc677e49"
readonly BENCH_REPOSITORY="https://github.com/howardjohn/gateway-api-bench.git"
readonly RELEASE_NAME="haptic"
readonly RELEASE_NAMESPACE="haptic"
readonly HAPROXYCFG_NAME="haptic-config-haproxycfg"
readonly PROMETHEUS_SCRAPE_INTERVAL_SECONDS=5
readonly CONTROLLER_DEFAULT_WATCH_DEBOUNCE="100ms"
readonly DEFAULT_HAPROXYCFG_POLL_INTERVAL_SECONDS="5"
readonly SCALE_READINESS_POLL_INTERVAL_SECONDS="0.1"
readonly READINESS_RESULT_DEADLINE=1
readonly READINESS_RESULT_EVIDENCE_INVALID=2
readonly SCALE_READINESS_STAGE_METADATA='[
  {"stage":"route-status","reason_code":"route-status-timeout","invalid_message":"scale route-status readiness evidence is invalid"},
  {"stage":"initial-exact-current","reason_code":"exact-current-timeout","invalid_message":"at-scale HAProxyCfg convergence evidence is invalid"},
  {"stage":"initial-referenced-map","reason_code":"referenced-map-timeout","invalid_message":"at-scale referenced-map readiness evidence is invalid"},
  {"stage":"runtime-host-map","reason_code":"runtime-map-timeout","invalid_message":"runtime host-map readiness evidence is invalid"},
  {"stage":"post-live-exact-current","reason_code":"exact-current-timeout","invalid_message":"post-proof HAProxyCfg convergence evidence is invalid"},
  {"stage":"post-live-referenced-map","reason_code":"referenced-map-timeout","invalid_message":"post-proof referenced-map readiness evidence is invalid"},
  {"stage":"semantic-token","reason_code":"semantic-token-timeout","invalid_message":"host.map semantic-token evidence is invalid"}
]'

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
RUN_STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
RUN_TOKEN="${RUN_STAMP,,}-${BASHPID}"
DEFAULT_HAPROXY_VERSION="$(yq -r '.haproxyVersion' "${PROJECT_ROOT}/charts/haptic/values.yaml")"

BENCH_REF="${BENCH_REF:-${DEFAULT_BENCH_REF}}"
BENCH_SCENARIOS="${BENCH_SCENARIOS:-${DEFAULT_BENCH_SCENARIOS}}"
BENCH_OUTPUT_DIR="${BENCH_OUTPUT_DIR:-${PROJECT_ROOT}/artifacts/gateway-api-bench/${RUN_TOKEN}}"
BENCH_GATEWAYS="${BENCH_GATEWAYS:-${DEFAULT_BENCH_GATEWAYS}}"
BENCH_PROBE_ROUTES="${BENCH_PROBE_ROUTES:-${DEFAULT_PROBE_ROUTES}}"
BENCH_PROBE_TIMEOUT="${BENCH_PROBE_TIMEOUT:-6h}"
BENCH_ROUTECHANGE_ITERATIONS="${BENCH_ROUTECHANGE_ITERATIONS:-${DEFAULT_ROUTECHANGE_ITERATIONS}}"
BENCH_ROUTECHANGE_GRACE_PERIOD="${BENCH_ROUTECHANGE_GRACE_PERIOD:-${DEFAULT_ROUTECHANGE_GRACE_PERIOD}}"
BENCH_ROUTECHANGE_TIMEOUT="${BENCH_ROUTECHANGE_TIMEOUT:-10m}"
BENCH_SCALE_NAMESPACES="${BENCH_SCALE_NAMESPACES:-${DEFAULT_SCALE_NAMESPACES}}"
BENCH_SCALE_ROUTES_PER_NAMESPACE="${BENCH_SCALE_ROUTES_PER_NAMESPACE:-${DEFAULT_SCALE_ROUTES_PER_NAMESPACE}}"
BENCH_SCALE_DURATION="${BENCH_SCALE_DURATION:-${DEFAULT_SCALE_DURATION}}"
BENCH_SCALE_STARTUP_TIMEOUT="${BENCH_SCALE_STARTUP_TIMEOUT:-20m}"
BENCH_DEPLOY_INTERVAL="${BENCH_DEPLOY_INTERVAL:-}"
BENCH_WATCH_DEBOUNCE="${BENCH_WATCH_DEBOUNCE:-}"
BENCH_KIND_NODE_IMAGE="${BENCH_KIND_NODE_IMAGE:-}"
BENCH_KEEP_CLUSTER="${BENCH_KEEP_CLUSTER:-false}"
BENCH_ALLOW_DIRTY="${BENCH_ALLOW_DIRTY:-false}"
BENCH_ALLOW_COSCHEDULED_CLUSTERS="${BENCH_ALLOW_COSCHEDULED_CLUSTERS:-false}"
REUSE_CLUSTER="${REUSE_CLUSTER:-false}"
BUILD_ONLY="${BUILD_ONLY:-false}"
HAPROXY_VERSION="${HAPROXY_VERSION:-${DEFAULT_HAPROXY_VERSION}}"
BENCH_GATEWAY_API_VERSION="${BENCH_GATEWAY_API_VERSION:-${DEFAULT_GATEWAY_API_VERSION}}"
BENCH_CLUSTER_TOKEN="${BENCH_CLUSTER_TOKEN:-}"
if [[ "$REUSE_CLUSTER" == "true" ]]; then
    CLUSTER_NAME="${BENCH_CLUSTER_NAME:-}"
    DOCKER_NETWORK_NAME="${BENCH_DOCKER_NETWORK:-}"
    CLUSTER_OWNERSHIP_TOKEN="$BENCH_CLUSTER_TOKEN"
else
    CLUSTER_NAME="haptic-gwbench-${RUN_TOKEN}"
    DOCKER_NETWORK_NAME="${CLUSTER_NAME}"
    CLUSTER_OWNERSHIP_TOKEN="$RUN_TOKEN"
fi
CLUSTER_CONTEXT="kind-${CLUSTER_NAME}"
KUBECONFIG_PATH="/tmp/${CLUSTER_NAME}.kubeconfig"
WORKLOAD_IMAGE="${CLUSTER_NAME}-workload:local"

WORK_DIR=""
UPSTREAM_DIR=""
BIN_DIR=""
cluster_owned=false
network_owned=false
owned_cluster_absent=false
scale_pid=""
scale_timer_pid=""
routechange_observer_pid=""
active_workload_container=""
workload_image_built=false
declare -a routechange_tunnel_pids=()
declare -a SCALE_ACTIVITY_METRICS=(
    haptic_reconciliation_total
    haptic_reconciliation_errors_total
    haptic_deployment_total
    haptic_deployment_errors_total
    haptic_haproxy_reloads_total
    haptic_deploy_apply_total
    haptic_apply_rejected_total
    haptic_runtime_server_ops_total
    haptic_runtime_backend_ops_total
    haptic_runtime_map_divergence_total
    haptic_validation_total
    haptic_validation_errors_total
    haptic_events_dropped_critical_total
)
output_initialized=false
cluster_marker_created=false
kubeconfig_owned=false
kind_target_state=""
live_secret_scan_ready=false
live_secret_capture_failed=false
artifact_security_untrusted=false
live_secret_patterns=""
live_secret_capture_index=0
live_secret_scan_index=0
runtime_config_history_pid=""

info() {
    printf 'benchmark: %s\n' "$*" >&2
}

die() {
    printf 'benchmark: error: %s\n' "$*" >&2
    exit 1
}

command_required() {
    command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

validate_bool() {
    local name="$1"
    local value="$2"
    [[ "$value" == "true" || "$value" == "false" ]] || die "${name} must be true or false, got ${value}"
}

validate_positive_integer() {
    local name="$1"
    local value="$2"
    [[ "$value" =~ ^[1-9][0-9]*$ ]] || die "${name} must be a positive integer, got ${value}"
}

duration_seconds() {
    local value="$1"
    local number="${value%[smh]}"
    case "$value" in
        *s) printf '%d\n' "$number" ;;
        *m) printf '%d\n' "$((number * 60))" ;;
        *h) printf '%d\n' "$((number * 3600))" ;;
        *) return 1 ;;
    esac
}

kind_cluster_state() {
    local cluster_name="$1"
    local clusters
    if ! clusters="$(kind get clusters 2>&1)"; then
        printf 'benchmark: error: kind get clusters failed: %s\n' "$clusters" >&2
        return 2
    fi
    if awk -v name="$cluster_name" '$0 == name { found = 1 } END { exit !found }' <<< "$clusters"; then
        printf 'present\n'
    else
        printf 'absent\n'
    fi
}

capture_kind_cluster_inventory() {
    local label="$1"
    local expected_target_state="$2"
    local raw="${BENCH_OUTPUT_DIR}/cluster/kind-clusters-${label}.txt"
    local errors="${BENCH_OUTPUT_DIR}/cluster/kind-clusters-${label}.stderr.txt"
    local inventory="${BENCH_OUTPUT_DIR}/cluster/kind-clusters-${label}.json"
    local coscheduled="${BENCH_OUTPUT_DIR}/cluster/kind-clusters-${label}-coscheduled.json"
    local nodes="${BENCH_OUTPUT_DIR}/cluster/kind-nodes-${label}.json"
    if ! kind get clusters > "$raw" 2> "$errors"; then
        die "kind get clusters failed; see ${errors}"
    fi
    awk 'NF' "$raw" | jq -R . | jq -s . > "$inventory"
    capture_kind_node_states "$nodes"
    jq -e --slurpfile nodes "$nodes" 'all(.[]; . as $cluster |
        any($nodes[0][]; .cluster == $cluster))' "$inventory" >/dev/null || \
        die "cannot determine every Kind cluster's Docker state"
    jq --arg target "$CLUSTER_NAME" --slurpfile nodes "$nodes" '
        [.[] | select(. != $target) | . as $cluster |
            select(any($nodes[0][]; .cluster == $cluster and
                (.state.Running != false or .state.Paused != false or .state.Restarting != false or
                    (.state.Status != "exited" and .state.Status != "created"))))]
    ' "$inventory" > "$coscheduled"
    if jq -e --arg target "$CLUSTER_NAME" 'index($target) != null' "$inventory" >/dev/null; then
        kind_target_state=present
    else
        kind_target_state=absent
    fi
    [[ "$kind_target_state" == "$expected_target_state" ]] || \
        die "kind cluster ${CLUSTER_NAME} is ${kind_target_state}, expected ${expected_target_state}"
    if [[ "$label" == "preexisting" ]]; then
        cp "$raw" "${BENCH_OUTPUT_DIR}/cluster/preexisting-kind-clusters.txt"
        cp "$inventory" "${BENCH_OUTPUT_DIR}/cluster/preexisting-kind-clusters.json"
        cp "$coscheduled" "${BENCH_OUTPUT_DIR}/cluster/preexisting-coscheduled-kind-clusters.json"
        local metadata_tmp="${BENCH_OUTPUT_DIR}/metadata.json.tmp"
        jq --slurpfile inventory "$inventory" --slurpfile coscheduled "$coscheduled" '
            .cluster.preexisting_kind_clusters = $inventory[0] |
            .cluster.preexisting_coscheduled_kind_clusters = $coscheduled[0]
        ' "${BENCH_OUTPUT_DIR}/metadata.json" > "$metadata_tmp"
        mv "$metadata_tmp" "${BENCH_OUTPUT_DIR}/metadata.json"
    fi
    if [[ "$BENCH_ALLOW_COSCHEDULED_CLUSTERS" != "true" ]] && \
        ! jq -e 'length == 0' "$coscheduled" >/dev/null; then
        jq -r '.[]' "$coscheduled" >&2
        die "other Kind clusters are active; stop them or use BENCH_ALLOW_COSCHEDULED_CLUSTERS=true for a non-comparable smoke run"
    fi
}

capture_kind_node_states() {
    local output="$1" ids
    ids="$(docker ps -aq --filter label=io.x-k8s.kind.cluster)" || \
        die "could not list Kind Docker containers"
    if [[ -z "$ids" ]]; then
        printf '[]\n' > "$output"
        return
    fi
    local -a containers
    mapfile -t containers <<< "$ids"
    docker inspect --format '{"id":{{json .Id}},"cluster":{{json (index .Config.Labels "io.x-k8s.kind.cluster")}},"state":{{json .State}}}' \
        "${containers[@]}" | jq -s . > "$output" || \
        die "could not inspect Kind Docker container states"
}

verify_network_ownership() {
    local labels
    labels="$(docker network inspect "$DOCKER_NETWORK_NAME" --format '{{json .Labels}}')" || return 1
    jq -e \
        --arg cluster "$CLUSTER_NAME" \
        --arg token "$CLUSTER_OWNERSHIP_TOKEN" '
        .["haproxy-haptic.org/gateway-api-benchmark"] == "true" and
        .["haproxy-haptic.org/benchmark-cluster"] == $cluster and
        .["haproxy-haptic.org/benchmark-owner"] == $token
    ' <<< "$labels" >/dev/null
}

verify_cluster_ownership() {
    verify_network_ownership || return 1
    local marker
    marker="$(kubectl get configmap haptic-gateway-api-benchmark-owner -n kube-system -o json)" || return 1
    jq -e \
        --arg cluster "$CLUSTER_NAME" \
        --arg network "$DOCKER_NETWORK_NAME" \
        --arg token "$CLUSTER_OWNERSHIP_TOKEN" '
        .data.cluster == $cluster and .data.network == $network and .data.token == $token
    ' <<< "$marker" >/dev/null
}

redact_helm_values() {
    local input="$1"
    local output="$2"
    jq -S '
        if (.credentials?.dataplane? | type) == "object" and
           (.credentials.dataplane | has("password")) then
          .credentials.dataplane.password = "<redacted>"
        else . end |
        if (.controller?.webhook? | type) == "object" and
           (.controller.webhook | has("caBundle")) and
           ((.controller.webhook.caBundle // "") != "") then
          .controller.webhook.caBundle = "<redacted>"
        else . end
    ' "$input" > "$output"
}

redact_helm_manifest() {
    local input="$1"
    local output="$2"
    yq '
        (select(.kind == "Secret" and has("data")).data[]) = "<redacted>" |
        (select(.kind == "Secret" and has("stringData")).stringData[]) = "<redacted>" |
        (.. | select(tag == "!!map" and has("caBundle")).caBundle) = "<redacted>"
    ' "$input" > "$output"
}

build_live_secret_patterns() {
    local secrets="$1"
    local runtime_configs="$2"
    local output="$3"
    if ! python3 - "$secrets" "$runtime_configs" "$output" <<'PY'
import base64
import binascii
import hashlib
import json
import os
import sys

secrets_path, runtime_configs_path, output_path = sys.argv[1:]
with open(secrets_path, encoding="utf-8") as handle:
    document = json.load(handle)
with open(runtime_configs_path, encoding="utf-8") as handle:
    runtime_config_document = json.load(handle)

runtime_config_identities = {
    (
        item.get("metadata", {}).get("namespace", "default"),
        item.get("metadata", {}).get("name", ""),
        item.get("metadata", {}).get("uid", ""),
    )
    for item in runtime_config_document.get("items", [])
    if item.get("apiVersion") == "haproxy-haptic.org/v1alpha1"
    and item.get("kind") == "HAProxyCfg"
    and item.get("metadata", {}).get("uid")
}


def runtime_config_label(name):
    # Match configpublisher.runtimeConfigLabelValue for names beyond Kubernetes' label limit.
    return name if len(name) <= 63 else "haptic-" + hashlib.sha256(name.encode()).hexdigest()[:16]


patterns = []
for item in document.get("items", []):
    metadata = item.get("metadata", {})
    namespace = metadata.get("namespace", "default")
    name = metadata.get("name", "")
    labels = metadata.get("labels") or {}
    owner_references = metadata.get("ownerReferences") or []
    runtime_config = labels.get("haproxy-haptic.org/runtime-config")
    haptic_ssl_auxiliary = (
        item.get("type") == "Opaque"
        and labels.get("haproxy-haptic.org/type") == "ssl-certificate"
        and bool(runtime_config)
        and any(
            reference.get("apiVersion") == "haproxy-haptic.org/v1alpha1"
            and reference.get("kind") == "HAProxyCfg"
            and runtime_config_label(reference.get("name", "")) == runtime_config
            and (namespace, reference.get("name"), reference.get("uid"))
            in runtime_config_identities
            and reference.get("controller") is True
            for reference in owner_references
            if isinstance(reference, dict)
        )
    )
    for key, encoded in sorted((item.get("data") or {}).items()):
        try:
            decoded = base64.b64decode(encoded, validate=True)
        except (binascii.Error, ValueError) as error:
            raise SystemExit(f"invalid base64 in Secret {namespace}/{name}/{key}: {error}")
        if haptic_ssl_auxiliary and key == "path":
            continue
        if len(decoded) < 8:
            continue
        source = f"{namespace}/{name}/{key}"
        for representation, value in (("decoded", decoded), ("base64", encoded.encode("ascii"))):
            patterns.append({
                "source": source,
                "representation": representation,
                "bytes_base64": base64.b64encode(value).decode("ascii"),
            })

result = {"schema_version": 1, "minimum_decoded_bytes": 8, "patterns": patterns}
temporary = output_path + ".tmp"
with open(temporary, "w", encoding="utf-8") as handle:
    json.dump(result, handle, separators=(",", ":"), sort_keys=True)
    handle.write("\n")
os.chmod(temporary, 0o600)
os.replace(temporary, output_path)
PY
    then
        return 1
    fi
    jq -e '.patterns | length > 0' "$output" >/dev/null
}

redact_secret_matches() {
    local patterns="$1"
    local target="$2"
    local report="$3"
    [[ ! -e "$report" ]] || return 2
    local scan_rc=0
    python3 - "$patterns" "$target" "$report" <<'PY' || scan_rc=$?
import base64
import json
import os
from pathlib import Path
import stat
import sys

patterns_path, target_path, report_path = sys.argv[1:]
with open(patterns_path, encoding="utf-8") as handle:
    entries = json.load(handle)["patterns"]
patterns = [
    (base64.b64decode(entry["bytes_base64"], validate=True), entry)
    for entry in entries
]
patterns.sort(key=lambda item: len(item[0]), reverse=True)

root = Path(target_path).resolve()
report = Path(report_path).resolve()
redacted = []
for path in sorted(root.rglob("*")):
    if path.resolve() == report:
        continue
    if path.is_symlink():
        raise SystemExit(f"artifact scan refuses symlink: {path.relative_to(root)}")
    if not path.is_file():
        continue
    content = path.read_bytes()
    hits = []
    seen = set()
    replacement = content
    for pattern, entry in patterns:
        if pattern not in content:
            continue
        identity = (entry["source"], entry["representation"])
        if identity not in seen:
            hits.append({"secret": entry["source"], "representation": entry["representation"]})
            seen.add(identity)
        replacement = replacement.replace(pattern, b"<redacted>")
    if not hits:
        continue
    mode = stat.S_IMODE(path.stat().st_mode)
    temporary = path.with_name(f".{path.name}.redacted-{os.getpid()}")
    with open(temporary, "xb") as handle:
        handle.write(replacement)
    os.chmod(temporary, mode)
    os.replace(temporary, path)
    redacted.append({"artifact": str(path.relative_to(root)), "secrets": hits})

if redacted:
    for path in sorted(root.rglob("*")):
        if path.resolve() == report:
            continue
        if path.is_symlink():
            raise SystemExit(f"artifact rescan refuses symlink: {path.relative_to(root)}")
        if not path.is_file():
            continue
        content = path.read_bytes()
        if any(pattern in content for pattern, _ in patterns):
            raise SystemExit(f"artifact redaction left a selected Secret value: {path.relative_to(root)}")

result = {
    "schema_version": 1,
    "pass": not redacted,
    "method": "bytewise raw-base64 and decoded captured sensitive Secret value scan; HAPTIC SSL path metadata excluded",
    "redacted": redacted,
}
temporary_report = report.with_name(f".{report.name}.tmp-{os.getpid()}")
with open(temporary_report, "w", encoding="utf-8") as handle:
    json.dump(result, handle, indent=2, sort_keys=True)
    handle.write("\n")
os.chmod(temporary_report, 0o600)
os.replace(temporary_report, report)
raise SystemExit(1 if redacted else 0)
PY
    local method="bytewise raw-base64 and decoded captured sensitive Secret value scan; HAPTIC SSL path metadata excluded"
    if [[ $scan_rc -eq 0 ]] && jq -e --arg method "$method" '
        .schema_version == 1 and .pass == true and .method == $method and
        (.redacted | type) == "array" and (.redacted | length) == 0
    ' "$report" >/dev/null 2>&1; then
        return 0
    fi
    if [[ $scan_rc -eq 1 ]] && jq -e --arg method "$method" '
        .schema_version == 1 and .pass == false and .method == $method and
        (.redacted | type) == "array" and (.redacted | length) > 0
    ' "$report" >/dev/null 2>&1; then
        return 1
    fi
    return 2
}

verify_secret_redaction() {
    local fixture_dir="${WORK_DIR}/secret-redaction-fixture"
    mkdir -p "$fixture_dir/artifacts"
    local secret="fixture-${RUN_TOKEN}-credential"
    local encoded
    encoded="$(printf '%s' "$secret" | base64 | tr -d '\n')"
    jq -n --arg secret "$secret" --arg ca_bundle "$encoded" '
        {credentials: {dataplane: {username: "fixture", password: $secret}},
         controller: {webhook: {caBundle: $ca_bundle}}}
    ' > "$fixture_dir/values.json"
    redact_helm_values "$fixture_dir/values.json" "$fixture_dir/values-redacted.json"
    jq -e '
        .credentials.dataplane.password == "<redacted>" and
        .controller.webhook.caBundle == "<redacted>"
    ' "$fixture_dir/values-redacted.json" >/dev/null || die "Helm values redaction fixture failed"
    printf '%s\n' \
        'apiVersion: v1' 'kind: Secret' 'data:' "  password: ${encoded}" \
        '---' 'apiVersion: admissionregistration.k8s.io/v1' \
        'kind: ValidatingWebhookConfiguration' 'webhooks:' '- clientConfig:' "    caBundle: ${encoded}" \
        > "$fixture_dir/manifest.yaml"
    redact_helm_manifest "$fixture_dir/manifest.yaml" "$fixture_dir/manifest-redacted.yaml"
    yq -o=json -I=0 '.' "$fixture_dir/manifest-redacted.yaml" | jq -se '
        (map(select(.kind == "Secret"))[0].data.password == "<redacted>") and
        (map(select(.kind == "ValidatingWebhookConfiguration"))[0].webhooks[0].clientConfig.caBundle == "<redacted>")
    ' >/dev/null || die "Helm manifest redaction fixture failed"
    if rg -a -F -q -e "$secret" -e "$encoded" \
        "$fixture_dir/values-redacted.json" "$fixture_dir/manifest-redacted.yaml"; then
        die "structured Helm redaction fixture retained a credential"
    fi
    jq -n --arg source 'fixture/secret/password' --arg decoded "$encoded" \
        --arg raw "$(printf '%s' "$encoded" | base64 | tr -d '\n')" '
        {schema_version: 1, minimum_decoded_bytes: 8,
         patterns: [
           {source: $source, representation: "decoded", bytes_base64: $decoded},
           {source: $source, representation: "base64", bytes_base64: $raw}
         ]}
    ' > "$fixture_dir/patterns.json"
    printf '%s\n' "$secret" > "$fixture_dir/artifacts/plaintext.txt"
    printf '%s\n' "$encoded" > "$fixture_dir/artifacts/base64.txt"
    local fixture_rc=0
    redact_secret_matches "$fixture_dir/patterns.json" "$fixture_dir/artifacts" \
        "$fixture_dir/artifact-scan.json" || fixture_rc=$?
    [[ $fixture_rc -eq 1 ]] || die "artifact Secret scanner did not reject its credential fixture"
    jq -e '.pass == false and (.redacted | length) == 2' "$fixture_dir/artifact-scan.json" >/dev/null || \
        die "artifact Secret scanner fixture report is invalid"
    if rg -a -F -q -e "$secret" -e "$encoded" "$fixture_dir/artifacts"; then
        die "artifact Secret scanner did not redact its credential fixture"
    fi
}

capture_runtime_config_identities() {
    kubectl get haproxycfgs.haproxy-haptic.org --all-namespaces -o json | jq -S '
        {items: [.items[] |
          {apiVersion, kind,
           metadata: (.metadata | {namespace, name, uid})}]}
    ' > "$1" || return 1
    chmod 0600 "$1"
}

start_runtime_config_history() {
    python3 "${SCRIPT_DIR}/record-benchmark-config-identities.py" "$WORK_DIR" \
        2> "${WORK_DIR}/runtime-config-history.stderr" &
    runtime_config_history_pid=$!
    local deadline=$((SECONDS + 60))
    until [[ -f "${WORK_DIR}/runtime-config-history.ready" ]]; do
        kill -0 "$runtime_config_history_pid" 2>/dev/null || die "HAProxyCfg identity recorder exited before becoming ready"
        (( SECONDS < deadline )) || die "HAProxyCfg identity recorder did not become ready"
        sleep 0.1
    done
}

capture_runtime_config_history() {
    python3 - "${WORK_DIR}/runtime-config-history.jsonl" "$1" <<'PY'
import json
from pathlib import Path
import sys

history, destination = map(Path, sys.argv[1:])
lines = history.read_bytes().split(b"\n")[:-1] if history.exists() else []
items = [json.loads(line) for line in lines if line]
destination.write_text(json.dumps({"items": items}) + "\n")
destination.chmod(0o600)
PY
}

capture_live_secret_patterns_once() {
    live_secret_capture_index=$((live_secret_capture_index + 1))
    local capture_id
    capture_id="$(printf '%03d' "$live_secret_capture_index")"
    local secrets="${WORK_DIR}/live-secrets-${capture_id}.json"
    local runtime_configs="${WORK_DIR}/live-haproxycfgs-${capture_id}.json"
    local captured_patterns="${WORK_DIR}/live-secret-patterns-${capture_id}.json"
    live_secret_patterns="${WORK_DIR}/live-secret-patterns.json"
    capture_runtime_config_identities "${runtime_configs}.before" || return 1
    kubectl get secrets --all-namespaces -o json > "$secrets" || return 1
    chmod 0600 "$secrets" || return 1
    capture_runtime_config_identities "${runtime_configs}.after" || return 1
    capture_runtime_config_history "${runtime_configs}.history" || return 1
    # Garbage collection can leave a certificate after its verified owner disappears.
    jq -S -s '{items: ([.[].items[]] | unique_by([.metadata.namespace, .metadata.name, .metadata.uid]))}' \
        "${runtime_configs}.before" "${runtime_configs}.after" "${runtime_configs}.history" > "$runtime_configs" || return 1
    chmod 0600 "$runtime_configs" || return 1
    build_live_secret_patterns "$secrets" "$runtime_configs" "$captured_patterns" || return 1
    local merged="${live_secret_patterns}.tmp"
    if [[ -f "$live_secret_patterns" ]]; then
        jq -S -s '
            {schema_version: 1, minimum_decoded_bytes: 8,
             patterns: ([.[].patterns[]] |
               unique_by([.bytes_base64, .source, .representation]))}
        ' "$live_secret_patterns" "$captured_patterns" > "$merged" || return 1
    else
        cp "$captured_patterns" "$merged" || return 1
    fi
    chmod 0600 "$merged" || return 1
    mv "$merged" "$live_secret_patterns" || return 1
}

capture_live_secret_patterns() {
    if ! capture_live_secret_patterns_once; then
        live_secret_capture_failed=true
        return 1
    fi
    live_secret_scan_ready=true
}

scan_artifacts_for_live_secrets() {
    [[ "$live_secret_scan_ready" == "true" ]] || return 0
    local report="${BENCH_OUTPUT_DIR}/cluster/artifact-secret-scan.json"
    live_secret_scan_index=$((live_secret_scan_index + 1))
    local scan_report="${WORK_DIR}/artifact-secret-scan-$(printf '%03d' "$live_secret_scan_index").json"
    local scan_rc=0
    redact_secret_matches "$live_secret_patterns" "$BENCH_OUTPUT_DIR" "$scan_report" || scan_rc=$?
    if [[ $scan_rc -ne 0 && $scan_rc -ne 1 ]]; then
        artifact_security_untrusted=true
        return 2
    fi
    local merged="${report}.tmp"
    if [[ -f "$report" ]]; then
        jq -S -s '
            {schema_version: 1,
             pass: all(.[]; .pass == true),
             method: .[-1].method,
             scan_count: (map(.scan_count // 1) | add),
             redacted: ([.[].redacted[]] |
               unique_by([.artifact, (.secrets | tojson)]))}
        ' "$report" "$scan_report" > "$merged" || {
            artifact_security_untrusted=true
            return 2
        }
    else
        jq -S '. + {scan_count: 1}' "$scan_report" > "$merged" || {
            artifact_security_untrusted=true
            return 2
        }
    fi
    mv "$merged" "$report" || {
        artifact_security_untrusted=true
        return 2
    }
    if ! jq -e '
        .schema_version == 1 and (.pass | type) == "boolean" and
        (.method | type) == "string" and (.method | length) > 0 and
        (.scan_count | type) == "number" and .scan_count > 0 and
        (.redacted | type) == "array"
    ' "$report" >/dev/null; then
        artifact_security_untrusted=true
        return 2
    fi
    jq -e '.pass == true' "$report" >/dev/null && return 0
    return 1
}

# workload_exit_acceptable judges pilot-load's exit code. After the harness
# issued the steady-churn stop, exiting on INT (130) or TERM (143) is the
# stop working; needing KILL (137) still fails, as does any exit before it.
workload_exit_acceptable() {
    local rc="$1"
    local stop_issued="$2"
    [[ "$rc" -eq 0 ]] && return 0
    [[ -f "$stop_issued" ]] || return 1
    [[ "$rc" -eq 130 || "$rc" -eq 143 ]]
}

# workload_stop_grace_seconds is how long pilot-load gets to answer INT by
# deleting what it created before TERM cuts it short. Its cleanup deletes
# routes at roughly 400/s; 5,000 routes took 12 s and the fixed 10 s left
# 840 behind, which the harness then waited five minutes for.
workload_stop_grace_seconds() {
    local routes="${1:-0}"
    echo $((10 + routes / 100))
}

signal_workload_container() {
    local container="$1"
    local routes="${2:-0}"
    docker inspect "$container" >/dev/null 2>&1 || return 0
    local signal deadline running wait_seconds
    for signal in INT TERM KILL; do
        running="$(docker inspect --format '{{.State.Running}}' "$container" 2>/dev/null)" || return 1
        [[ "$running" == "true" ]] || return 0
        if ! docker kill --signal="$signal" "$container" >/dev/null 2>&1; then
            running="$(docker inspect --format '{{.State.Running}}' "$container" 2>/dev/null)" || return 1
            [[ "$running" == "false" ]] && return 0
            return 1
        fi
        wait_seconds="$(workload_stop_grace_seconds "$routes")"
        if [[ "$signal" == "KILL" ]]; then
            wait_seconds=3
        fi
        deadline=$((SECONDS + wait_seconds))
        while (( SECONDS < deadline )); do
            running="$(docker inspect --format '{{.State.Running}}' "$container" 2>/dev/null)" || return 1
            [[ "$running" == "true" ]] || return 0
            sleep 0.2
        done
    done
    running="$(docker inspect --format '{{.State.Running}}' "$container" 2>/dev/null)" || return 1
    [[ "$running" == "false" ]]
}

signal_running_workload_container() {
    workload_container_running "$1" || return 1
    signal_workload_container "$1"
}

workload_container_running() {
    [[ "$(docker inspect --format '{{.State.Running}}' "$1" 2>/dev/null)" == "true" ]]
}

record_event() {
    local event="$1"
    local scenario="${2:-}"
    jq -cn \
        --arg event "$event" \
        --arg scenario "$scenario" \
        --arg timestamp "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" \
        --argjson epoch "$(date +%s)" \
        '{event: $event, scenario: $scenario, timestamp: $timestamp, epoch: $epoch}' \
        >> "${BENCH_OUTPUT_DIR}/timestamps.ndjson"
}

best_effort_failure_capture() {
    local output="$1"
    shift
    local capture_rc=0
    if timeout --foreground --kill-after=2s 15s "$@" > "$output" 2> "${output}.stderr.txt"; then
        capture_rc=0
    else
        capture_rc=$?
    fi
    printf '%d\n' "$capture_rc" > "${output}.exit-code.txt"
}

pod_failure_summary() {
    jq -c '
        def states: [.[]? | {
            name, ready, restarts: .restartCount,
            waiting: .state.waiting.reason,
            terminated: .state.terminated.reason, exit_code: .state.terminated.exitCode,
            previous_termination: .lastState.terminated.reason,
            previous_exit_code: .lastState.terminated.exitCode
        }];
        [.items[] | {
            name: .metadata.name, phase: .status.phase,
            containers: (.status.containerStatuses | states),
            init_containers: (.status.initContainerStatuses | states),
            limits: [(.spec.initContainers[]?, .spec.containers[]?) | {
                name, cpu: .resources.limits.cpu, memory: .resources.limits.memory
            }]
        }]
    '
}

capture_failure_state() {
    local original_rc="$1"
    local output="${BENCH_OUTPUT_DIR}/failure"
    mkdir -p "$output"
    jq -n \
        --arg timestamp "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" \
        --argjson original_exit_code "$original_rc" \
        '{timestamp: $timestamp, original_exit_code: $original_exit_code}' \
        > "$output/capture.json"
    best_effort_failure_capture "$output/pods.json" \
        kubectl get pods --all-namespaces -o json
    best_effort_failure_capture "$output/workloads.json" \
        kubectl get deployments,statefulsets,daemonsets --all-namespaces -o json
    best_effort_failure_capture "$output/gateway-api.json" \
        kubectl get gatewayclasses,gateways,httproutes --all-namespaces -o json
    best_effort_failure_capture "$output/haptic-resources.json" \
        kubectl get haproxytemplateconfigs,haproxytemplatelibraries,haproxycfgs,haproxymapfiles,haproxygeneralfiles,haproxycrtlistfiles \
            --all-namespaces -o json
    best_effort_failure_capture "$output/events.json" \
        kubectl get events --all-namespaces -o json
    best_effort_failure_capture "$output/network.log" \
        kubectl logs -n kube-system -l k8s-app=calico-node --all-containers --prefix --tail=-1 --timestamps=true
    best_effort_failure_capture "$output/previous-network.log" \
        kubectl logs -n kube-system -l k8s-app=calico-node --all-containers --previous --prefix --tail=-1 --timestamps=true
    best_effort_failure_capture "$output/controller.log" \
        kubectl logs -n "$RELEASE_NAMESPACE" \
            -l app.kubernetes.io/instance=haptic,app.kubernetes.io/component=controller \
            --all-containers --prefix --tail=-1 --timestamps=true
    best_effort_failure_capture "$output/previous-controller.log" \
        kubectl logs -n "$RELEASE_NAMESPACE" \
            -l app.kubernetes.io/instance=haptic,app.kubernetes.io/component=controller \
            --container controller --previous --prefix --tail=-1 --timestamps=true
    best_effort_failure_capture "$output/loadbalancer.log" \
        kubectl logs -n "$RELEASE_NAMESPACE" \
            -l app.kubernetes.io/instance=haptic,app.kubernetes.io/component=loadbalancer \
            --all-containers --prefix --tail=-1 --timestamps=true
}

quarantine_untrusted_artifacts() {
    local quarantine="${WORK_DIR}/untrusted-artifacts"
    [[ -d "$BENCH_OUTPUT_DIR" && ! -e "$quarantine" ]] || return 1
    if python3 - "$BENCH_OUTPUT_DIR" "$quarantine" <<'PY'
import os
import sys

os.rename(sys.argv[1], sys.argv[2])
PY
    then
        mkdir -- "$BENCH_OUTPUT_DIR" || return 1
    else
        find "$BENCH_OUTPUT_DIR" -xdev -depth -mindepth 1 -delete || return 1
        [[ -z "$(find "$BENCH_OUTPUT_DIR" -xdev -mindepth 1 -print -quit)" ]] || return 1
    fi
    jq -n \
        --arg timestamp "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" \
        --argjson secret_capture_failed "$live_secret_capture_failed" \
        --argjson artifact_scan_failed "$artifact_security_untrusted" '
        {schema_version: 1,
         timestamp: $timestamp,
         pass: false,
         status: "invalid",
         reason: "live Secret inventory or artifact scan was untrusted",
         reason_code: "artifact-security-untrusted",
         secret_capture_failed: $secret_capture_failed,
         artifact_scan_failed: $artifact_scan_failed,
         artifacts_retained: false}
    ' > "${BENCH_OUTPUT_DIR}/artifact-security-invalid.json"
}

finalize_runner_summary() {
    local final_rc="$1"
    local summary="${BENCH_OUTPUT_DIR}/runner-summary.json"
    local temporary="${summary}.tmp"
    local finished_at
    local secret_inventory_trusted=true
    finished_at="$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)"
    if [[ "$live_secret_capture_failed" != "false" || "$artifact_security_untrusted" != "false" ]]; then
        secret_inventory_trusted=false
    fi
    if [[ -f "$summary" ]]; then
        jq \
            --arg finished_at "$finished_at" \
            --argjson final_exit_code "$final_rc" \
            --argjson secret_inventory_trusted "$secret_inventory_trusted" '
            .finished_at = $finished_at |
            .harness.pass = ($final_exit_code == 0) |
            .harness.status = (if $final_exit_code == 0 then "passed" else "invalid" end) |
            .harness.final_exit_code = $final_exit_code |
            .harness.secret_inventory_trusted = $secret_inventory_trusted
        ' "$summary" > "$temporary" || return 1
    else
        jq -n \
            --arg finished_at "$finished_at" \
            --argjson final_exit_code "$final_rc" \
            --argjson build_only "$BUILD_ONLY" \
            --argjson secret_inventory_trusted "$secret_inventory_trusted" '
            {schema_version: 1,
             finished_at: $finished_at,
             harness: {
               pass: ($final_exit_code == 0),
               status: (if $final_exit_code != 0 then "invalid"
                        elif $build_only then "build-only-complete"
                        else "passed" end),
               final_exit_code: $final_exit_code,
               secret_inventory_trusted: $secret_inventory_trusted,
               process_exit_semantics: "nonzero means harness, provenance, or evidence invalid; measured gaps remain structured results"
             },
             public_comparison: "ballpark-only",
             scenarios: [],
             measured_result: null}
        ' > "$temporary" || return 1
    fi
    mv "$temporary" "$summary"
}

verify_artifact_secrets() {
    local scan scan_rc
    for scan in 1 2; do
        scan_rc=0
        scan_artifacts_for_live_secrets || scan_rc=$?
        if [[ $scan_rc -eq 1 ]]; then
            printf 'benchmark: error: benchmark artifacts contained a live Secret value; affected files were redacted\n' >&2
            jq -c '{redacted: .redacted}' "${BENCH_OUTPUT_DIR}/cluster/artifact-secret-scan.json" >&2
            return 1
        elif [[ $scan_rc -ne 0 ]]; then
            printf 'benchmark: error: artifact Secret scan failed; artifacts are untrusted\n' >&2
            return 1
        fi
    done
}

cleanup() {
    local rc=$?
    local original_rc=$rc
    trap - EXIT INT TERM

    if [[ -n "$routechange_observer_pid" ]] && kill -0 "$routechange_observer_pid" 2>/dev/null; then
        kill "$routechange_observer_pid" 2>/dev/null || true
        wait "$routechange_observer_pid" 2>/dev/null || true
    fi
    stop_routechange_tunnels || [[ $rc -ne 0 ]] || rc=1
    if [[ -n "$active_workload_container" ]]; then
        if docker inspect "$active_workload_container" >/dev/null 2>&1; then
            signal_workload_container "$active_workload_container" || [[ $rc -ne 0 ]] || rc=1
            docker rm -f "$active_workload_container" >/dev/null 2>&1 || [[ $rc -ne 0 ]] || rc=1
        fi
        active_workload_container=""
    fi
    if [[ -n "$scale_timer_pid" ]]; then
        if pid_running "$scale_timer_pid"; then
            kill -TERM "$scale_timer_pid" 2>/dev/null || true
            local timer_deadline=$((SECONDS + 5))
            while (( SECONDS < timer_deadline )) && pid_running "$scale_timer_pid"; do
                sleep 0.1
            done
            pid_running "$scale_timer_pid" && kill -KILL "$scale_timer_pid" 2>/dev/null || true
        fi
        wait "$scale_timer_pid" 2>/dev/null || true
        scale_timer_pid=""
    fi
    if [[ -n "$scale_pid" ]] && kill -0 "$scale_pid" 2>/dev/null; then
        kill "$scale_pid" 2>/dev/null || true
        wait "$scale_pid" 2>/dev/null || true
    fi

    if [[ $original_rc -ne 0 && "$output_initialized" == "true" &&
        "$kubeconfig_owned" == "true" && -s "$KUBECONFIG_PATH" && ! -L "$KUBECONFIG_PATH" ]] &&
        verify_network_ownership &&
        [[ "$(kubectl config current-context 2>/dev/null)" == "$CLUSTER_CONTEXT" ]]; then
        capture_failure_state "$original_rc" || \
            printf 'benchmark: warning: failure-state capture was incomplete\n' >&2
    fi

    if [[ "$kubeconfig_owned" == "true" ]]; then
        if [[ ! -s "$KUBECONFIG_PATH" || -L "$KUBECONFIG_PATH" ]]; then
            live_secret_capture_failed=true
            printf 'benchmark: error: cannot refresh Secret patterns without the owned kubeconfig\n' >&2
            [[ $rc -ne 0 ]] || rc=1
        elif ! capture_live_secret_patterns; then
            printf 'benchmark: error: failed to refresh live Secret patterns before cleanup\n' >&2
            [[ $rc -ne 0 ]] || rc=1
        fi
    fi
    if [[ "$output_initialized" == "true" && "$live_secret_scan_ready" == "true" &&
        "$live_secret_capture_failed" == "false" && "$artifact_security_untrusted" == "false" ]]; then
        verify_artifact_secrets || [[ $rc -ne 0 ]] || rc=1
    fi

    if [[ -n "$runtime_config_history_pid" ]]; then
        kill "$runtime_config_history_pid" 2>/dev/null || true
        wait "$runtime_config_history_pid" 2>/dev/null || true
        runtime_config_history_pid=""
    fi

    if [[ "$cluster_owned" == "true" && "$BENCH_KEEP_CLUSTER" != "true" ]]; then
        local cluster_state state_rc
        if cluster_state="$(kind_cluster_state "$CLUSTER_NAME")"; then
            state_rc=0
        else
            state_rc=$?
        fi
        if [[ $state_rc -ne 0 ]]; then
            [[ $rc -ne 0 ]] || rc=1
        elif [[ "$cluster_state" == "absent" ]]; then
            owned_cluster_absent=true
        elif [[ "$cluster_state" == "present" ]]; then
            local ownership_valid=false
            if [[ "$cluster_marker_created" == "true" ]]; then
                verify_cluster_ownership && ownership_valid=true
            else
                verify_network_ownership && ownership_valid=true
            fi
            if [[ "$ownership_valid" != "true" ]]; then
                printf 'benchmark: error: refusing to delete cluster %s without its ownership token\n' \
                    "$CLUSTER_NAME" >&2
                [[ $rc -ne 0 ]] || rc=1
            elif kind delete cluster --name "$CLUSTER_NAME"; then
                if cluster_state="$(kind_cluster_state "$CLUSTER_NAME")"; then
                    state_rc=0
                else
                    state_rc=$?
                fi
                if [[ $state_rc -ne 0 || "$cluster_state" != "absent" ]]; then
                    printf 'benchmark: error: owned cluster %s still exists or could not be verified absent\n' \
                        "$CLUSTER_NAME" >&2
                    [[ $rc -ne 0 ]] || rc=1
                else
                    owned_cluster_absent=true
                fi
            elif [[ "$ownership_valid" == "true" ]]; then
                printf 'benchmark: error: failed to delete owned cluster %s\n' "$CLUSTER_NAME" >&2
                [[ $rc -ne 0 ]] || rc=1
            fi
        fi
    fi

    if [[ "$network_owned" == "true" && "$BENCH_KEEP_CLUSTER" != "true" ]]; then
        if docker network inspect "$DOCKER_NETWORK_NAME" >/dev/null 2>&1; then
            if ! verify_network_ownership; then
                printf 'benchmark: error: refusing to remove Docker network %s without its ownership token\n' \
                    "$DOCKER_NETWORK_NAME" >&2
                [[ $rc -ne 0 ]] || rc=1
            elif ! docker network rm "$DOCKER_NETWORK_NAME" >/dev/null; then
                printf 'benchmark: error: failed to remove owned Docker network %s\n' \
                    "$DOCKER_NETWORK_NAME" >&2
                [[ $rc -ne 0 ]] || rc=1
            elif docker network inspect "$DOCKER_NETWORK_NAME" >/dev/null 2>&1; then
                printf 'benchmark: error: owned Docker network %s still exists\n' \
                    "$DOCKER_NETWORK_NAME" >&2
                [[ $rc -ne 0 ]] || rc=1
            fi
        fi
    fi
    if [[ "$workload_image_built" == "true" ]]; then
        docker image rm "$WORKLOAD_IMAGE" >/dev/null 2>&1 || [[ $rc -ne 0 ]] || rc=1
    fi
    local remove_kubeconfig=false
    if [[ "$kubeconfig_owned" == "true" && "$cluster_owned" != "true" ]]; then
        remove_kubeconfig=true
    elif [[ "$kubeconfig_owned" == "true" && "$BENCH_KEEP_CLUSTER" != "true" &&
        "$owned_cluster_absent" == "true" ]]; then
        remove_kubeconfig=true
    fi
    if [[ "$remove_kubeconfig" == "true" && -e "$KUBECONFIG_PATH" ]]; then
        if [[ -f "$KUBECONFIG_PATH" && ! -L "$KUBECONFIG_PATH" &&
            "$KUBECONFIG_PATH" == "/tmp/${CLUSTER_NAME}.kubeconfig" ]]; then
            unlink "$KUBECONFIG_PATH" || [[ $rc -ne 0 ]] || rc=1
        else
            printf 'benchmark: error: refusing to remove unexpected kubeconfig path %s\n' "$KUBECONFIG_PATH" >&2
            [[ $rc -ne 0 ]] || rc=1
        fi
    fi

    if [[ "$output_initialized" == "true" &&
        ("$live_secret_capture_failed" == "true" || "$artifact_security_untrusted" == "true") ]]; then
        [[ $rc -ne 0 ]] || rc=1
        if ! quarantine_untrusted_artifacts; then
            printf 'benchmark: error: failed to quarantine artifacts after Secret inventory failure\n' >&2
            chmod 000 -- "$BENCH_OUTPUT_DIR" 2>/dev/null || true
            output_initialized=false
        fi
    fi

    if [[ "$output_initialized" == "true" && -d "$BENCH_OUTPUT_DIR" ]]; then
        if ! date -u +%Y-%m-%dT%H:%M:%S.%NZ > "${BENCH_OUTPUT_DIR}/runner-finished-at.txt"; then
            [[ $rc -ne 0 ]] || rc=1
        fi
        if ! printf '%d\n' "$original_rc" > "${BENCH_OUTPUT_DIR}/runner-original-exit-code.txt"; then
            [[ $rc -ne 0 ]] || rc=1
        fi
        if command -v jq >/dev/null 2>&1 && [[ -f "${BENCH_OUTPUT_DIR}/timestamps.ndjson" ]]; then
            if ! jq -cn \
                --arg event runner-exit \
                --arg timestamp "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" \
                --argjson epoch "$(date +%s)" \
                --argjson original_exit_code "$original_rc" \
                --argjson final_exit_code "$rc" \
                '{event: $event, timestamp: $timestamp, epoch: $epoch,
                  original_exit_code: $original_exit_code, final_exit_code: $final_exit_code}' \
                >> "${BENCH_OUTPUT_DIR}/timestamps.ndjson"; then
                [[ $rc -ne 0 ]] || rc=1
            fi
        fi
        if ! printf '%d\n' "$rc" > "${BENCH_OUTPUT_DIR}/runner-exit-code.txt"; then
            [[ $rc -ne 0 ]] || rc=1
        fi
        if ! finalize_runner_summary "$rc"; then
            [[ $rc -ne 0 ]] || rc=1
            printf '%d\n' "$rc" > "${BENCH_OUTPUT_DIR}/runner-exit-code.txt" 2>/dev/null || true
        fi
    fi

    local temp_root="${TMPDIR:-/tmp}"
    if [[ -n "$WORK_DIR" && -d "$WORK_DIR" && "$WORK_DIR" == "${temp_root%/}"/gateway-api-bench.* ]]; then
        find "$WORK_DIR" -depth -mindepth 1 -delete 2>/dev/null || true
        rmdir "$WORK_DIR" 2>/dev/null || true
    fi

    exit "$rc"
}

trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

validate_inputs() {
    validate_bool BENCH_KEEP_CLUSTER "$BENCH_KEEP_CLUSTER"
    validate_bool BENCH_ALLOW_DIRTY "$BENCH_ALLOW_DIRTY"
    validate_bool BENCH_ALLOW_COSCHEDULED_CLUSTERS "$BENCH_ALLOW_COSCHEDULED_CLUSTERS"
    validate_bool REUSE_CLUSTER "$REUSE_CLUSTER"
    validate_bool BUILD_ONLY "$BUILD_ONLY"
    validate_positive_integer BENCH_PROBE_ROUTES "$BENCH_PROBE_ROUTES"
    validate_positive_integer BENCH_ROUTECHANGE_ITERATIONS "$BENCH_ROUTECHANGE_ITERATIONS"
    validate_positive_integer BENCH_SCALE_NAMESPACES "$BENCH_SCALE_NAMESPACES"
    validate_positive_integer BENCH_SCALE_ROUTES_PER_NAMESPACE "$BENCH_SCALE_ROUTES_PER_NAMESPACE"

    if [[ "$REUSE_CLUSTER" == "true" ]]; then
        [[ -n "$CLUSTER_NAME" ]] || die "REUSE_CLUSTER=true requires BENCH_CLUSTER_NAME"
        [[ -n "$DOCKER_NETWORK_NAME" ]] || die "REUSE_CLUSTER=true requires BENCH_DOCKER_NETWORK"
        [[ -n "$CLUSTER_OWNERSHIP_TOKEN" ]] || die "REUSE_CLUSTER=true requires BENCH_CLUSTER_TOKEN"
    elif [[ -n "${BENCH_CLUSTER_NAME:-}" || -n "${BENCH_DOCKER_NETWORK:-}" || -n "$BENCH_CLUSTER_TOKEN" ]]; then
        die "BENCH_CLUSTER_NAME, BENCH_DOCKER_NETWORK, and BENCH_CLUSTER_TOKEN are only valid with REUSE_CLUSTER=true"
    fi
    [[ "$CLUSTER_NAME" == haptic-gwbench-* ]] || die "benchmark clusters must use the haptic-gwbench- prefix"
    [[ "$DOCKER_NETWORK_NAME" == "$CLUSTER_NAME" ]] || die "benchmark Docker network must equal the cluster name"
    [[ "$CLUSTER_OWNERSHIP_TOKEN" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || \
        die "benchmark cluster token must be a lowercase DNS token"
    [[ "$CLUSTER_NAME" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || die "benchmark cluster name is invalid"
    [[ "$DOCKER_NETWORK_NAME" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*$ ]] || die "benchmark Docker network name is invalid"
    [[ "$BENCH_REF" =~ ^[0-9A-Za-z._/-]+$ ]] || die "BENCH_REF contains unsupported characters"
    [[ "$BENCH_GATEWAY_API_VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || \
        die "BENCH_GATEWAY_API_VERSION must be a release tag such as v1.4.0"
    [[ "$BENCH_GATEWAY_API_CHANNEL" == "standard" || "$BENCH_GATEWAY_API_CHANNEL" == "experimental" ]] || \
        die "BENCH_GATEWAY_API_CHANNEL must be standard or experimental"
    [[ "$HAPROXY_VERSION" =~ ^[0-9]+\.[0-9]+$ ]] || die "HAPROXY_VERSION must be a major.minor version"
    if [[ -n "$BENCH_DEPLOY_INTERVAL" ]]; then
        [[ "$BENCH_DEPLOY_INTERVAL" =~ ^[0-9]+(ns|us|ms|s|m|h)$ ]] || \
            die "BENCH_DEPLOY_INTERVAL must be a duration"
    fi
    if [[ -n "$BENCH_WATCH_DEBOUNCE" ]]; then
        [[ "$BENCH_WATCH_DEBOUNCE" =~ ^[0-9]+(ns|us|ms|s|m|h)$ ]] || die "BENCH_WATCH_DEBOUNCE must be a duration"
    fi
    [[ "$BENCH_ROUTECHANGE_GRACE_PERIOD" =~ ^[0-9]+(ns|us|ms|s|m|h)$ ]] || die "BENCH_ROUTECHANGE_GRACE_PERIOD must be a duration"
    [[ "$BENCH_PROBE_TIMEOUT" =~ ^[1-9][0-9]*(s|m|h)$ ]] || die "BENCH_PROBE_TIMEOUT must be a positive duration"
    [[ "$BENCH_ROUTECHANGE_TIMEOUT" =~ ^[1-9][0-9]*(s|m|h)$ ]] || die "BENCH_ROUTECHANGE_TIMEOUT must be a positive duration"
    [[ "$BENCH_SCALE_STARTUP_TIMEOUT" =~ ^[1-9][0-9]*(s|m|h)$ ]] || die "BENCH_SCALE_STARTUP_TIMEOUT must be a positive duration"

    local seen="," scenario
    IFS=',' read -r -a SCENARIOS <<< "$BENCH_SCENARIOS"
    [[ ${#SCENARIOS[@]} -gt 0 ]] || die "BENCH_SCENARIOS is empty"
    for scenario in "${SCENARIOS[@]}"; do
        case "$scenario" in
            probe|routechange|scale) ;;
            *) die "unsupported BENCH_SCENARIOS entry: ${scenario}" ;;
        esac
        [[ "$seen" != *",${scenario},"* ]] || die "duplicate BENCH_SCENARIOS entry: ${scenario}"
        seen+="${scenario},"
    done

    IFS=',' read -r -a GATEWAYS <<< "$BENCH_GATEWAYS"
    [[ ${#GATEWAYS[@]} -gt 0 ]] || die "BENCH_GATEWAYS is empty"
    local gateway seen_gateways=","
    for gateway in "${GATEWAYS[@]}"; do
        [[ "$gateway" =~ ^[a-z0-9]([-a-z0-9.]*[a-z0-9])?/[a-z0-9]([-a-z0-9.]*[a-z0-9])?$ ]] || \
            die "BENCH_GATEWAYS entry must be namespace/name: ${gateway}"
        [[ "$seen_gateways" != *",${gateway},"* ]] || die "duplicate BENCH_GATEWAYS entry: ${gateway}"
        seen_gateways+="${gateway},"
    done

    if [[ ",${BENCH_SCENARIOS}," == *,scale,* ]]; then
        command_required timeout
        [[ "$BENCH_SCALE_DURATION" =~ ^[1-9][0-9]*(s|m|h)$ ]] || \
            die "BENCH_SCALE_DURATION must be a positive integer followed by s, m, or h"
        timeout "$BENCH_SCALE_DURATION" true >/dev/null 2>&1 || {
            local rc=$?
            [[ $rc -eq 124 ]] || die "BENCH_SCALE_DURATION is invalid: ${BENCH_SCALE_DURATION}"
        }
    fi
}

check_haptic_worktree() {
    local status
    status="$(git -C "$PROJECT_ROOT" status --porcelain=v1 --untracked-files=all)"
    if [[ -n "$status" && "$BENCH_ALLOW_DIRTY" != "true" ]]; then
        printf '%s\n' "$status" >&2
        die "HAPTIC worktree is dirty; commit the benchmarked state or set BENCH_ALLOW_DIRTY=true"
    fi
}

prepare_work_dir() {
    local temp_root="${TMPDIR:-/tmp}"
    WORK_DIR="$(mktemp -d "${temp_root%/}/gateway-api-bench.XXXXXX")"
    UPSTREAM_DIR="${WORK_DIR}/upstream"
    BIN_DIR="${WORK_DIR}/bin"
    mkdir -p "$UPSTREAM_DIR" "$BIN_DIR"
}

prepare_output() {
    local output_parent
    output_parent="$(dirname -- "$BENCH_OUTPUT_DIR")"
    mkdir -p -- "$output_parent"
    mkdir -- "$BENCH_OUTPUT_DIR" || die "BENCH_OUTPUT_DIR already exists: ${BENCH_OUTPUT_DIR}"
    output_initialized=true
    mkdir "${BENCH_OUTPUT_DIR}/upstream" "${BENCH_OUTPUT_DIR}/host" "${BENCH_OUTPUT_DIR}/cluster"
    : > "${BENCH_OUTPUT_DIR}/timestamps.ndjson"
    record_event runner-start
}

check_build_prerequisites() {
    command_required git
    command_required go
    command_required jq
    command_required rg
    command_required sha256sum
    command_required xargs
    command_required cp
    command_required unlink
    command_required curl
}

fetch_and_build_upstream() {
    info "fetching gateway-api-bench ${BENCH_REF}"
    git -C "$UPSTREAM_DIR" init -q
    git -C "$UPSTREAM_DIR" remote add origin "$BENCH_REPOSITORY"
    git -C "$UPSTREAM_DIR" fetch -q --depth=1 origin "$BENCH_REF"
    git -C "$UPSTREAM_DIR" checkout -q --detach FETCH_HEAD

    local resolved_ref
    resolved_ref="$(git -C "$UPSTREAM_DIR" rev-parse HEAD)"
    [[ "$resolved_ref" =~ ^[0-9a-f]{40}$ ]] || die "upstream ref did not resolve to a commit SHA"
    if [[ "$BENCH_REF" =~ ^[0-9a-f]{40}$ && "$resolved_ref" != "$BENCH_REF" ]]; then
        die "upstream ref resolved to ${resolved_ref}, expected exact commit ${BENCH_REF}"
    fi
    [[ -z "$(git -C "$UPSTREAM_DIR" status --porcelain --untracked-files=all)" ]] || die "upstream checkout is not clean"

    git -C "$UPSTREAM_DIR" show -s --format=fuller HEAD > "${BENCH_OUTPUT_DIR}/upstream/commit.txt"
    git -C "$UPSTREAM_DIR" status --porcelain=v1 --untracked-files=all > "${BENCH_OUTPUT_DIR}/upstream/status.txt"
    printf '%s\n' "$BENCH_REPOSITORY" > "${BENCH_OUTPUT_DIR}/upstream/repository.txt"
    printf '%s\n' "$BENCH_REF" > "${BENCH_OUTPUT_DIR}/upstream/requested-ref.txt"
    printf '%s\n' "$resolved_ref" > "${BENCH_OUTPUT_DIR}/upstream/resolved-commit.txt"
    (
        cd "$UPSTREAM_DIR"
        sha256sum go.mod go.sum tests/probe/probe.go tests/routechange/routechange.go \
            tests/route-load.sh install/basic.sh install/prometheus.yaml
    ) > "${BENCH_OUTPUT_DIR}/upstream/source-sha256.txt"
    if [[ "$resolved_ref" == "$DEFAULT_BENCH_REF" ]]; then
        local published_install_command
        published_install_command="kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/${DEFAULT_GATEWAY_API_VERSION}/${DEFAULT_GATEWAY_API_CHANNEL}-install.yaml --server-side"
        rg -Fx "$published_install_command" \
            "$UPSTREAM_DIR/install/basic.sh" > "${BENCH_OUTPUT_DIR}/upstream/gateway-api-install-command.txt" || \
            die "published benchmark setup no longer pins the v1.4.0 experimental bundle"
        rg -q '^[[:space:]]*sigs\.k8s\.io/gateway-api v1\.4\.0$' "$UPSTREAM_DIR/go.mod" || \
            die "published benchmark module no longer pins Gateway API v1.4.0"
    fi

    local gateway_api_manifest="${WORK_DIR}/gateway-api-${BENCH_GATEWAY_API_CHANNEL}-install.yaml"
    local gateway_api_url
    gateway_api_url="https://github.com/kubernetes-sigs/gateway-api/releases/download/${BENCH_GATEWAY_API_VERSION}/${BENCH_GATEWAY_API_CHANNEL}-install.yaml"
    curl --fail --location --silent --show-error --retry 7 --retry-all-errors \
        "$gateway_api_url" > "$gateway_api_manifest" || \
        die "failed to download the Gateway API ${BENCH_GATEWAY_API_VERSION} ${BENCH_GATEWAY_API_CHANNEL} bundle"
    local gateway_api_manifest_sha
    gateway_api_manifest_sha="$(sha256sum "$gateway_api_manifest" | awk '{print $1}')"
    if [[ "$BENCH_GATEWAY_API_VERSION" == "$DEFAULT_GATEWAY_API_VERSION" &&
        "$gateway_api_manifest_sha" != "$DEFAULT_GATEWAY_API_MANIFEST_SHA256" ]]; then
        die "Gateway API ${DEFAULT_GATEWAY_API_VERSION} experimental manifest digest changed"
    fi
    cp "$gateway_api_manifest" "${BENCH_OUTPUT_DIR}/upstream/gateway-api-experimental-install.yaml"
    printf '%s  %s\n' "$gateway_api_manifest_sha" gateway-api-experimental-install.yaml \
        > "${BENCH_OUTPUT_DIR}/upstream/gateway-api-experimental-install-sha256.txt"
    printf '%s\n' "$gateway_api_url" > "${BENCH_OUTPUT_DIR}/upstream/gateway-api-experimental-install-url.txt"

    info "building exact upstream tools"
    (
        cd "$UPSTREAM_DIR"
        CGO_ENABLED=0 go build -mod=readonly -trimpath -o "$BIN_DIR/gatewayapi-probe" ./tests/probe
        CGO_ENABLED=0 go build -mod=readonly -trimpath -o "$BIN_DIR/gatewayapi-routechange" ./tests/routechange
        CGO_ENABLED=0 go build -mod=readonly -trimpath -o "$BIN_DIR/pilot-load" github.com/howardjohn/pilot-load
        go list -mod=readonly -m -f '{{.Path}} {{.Version}} {{.Sum}}' github.com/howardjohn/pilot-load \
            > "${BENCH_OUTPUT_DIR}/upstream/pilot-load-module.txt"
    )
    (
        cd "$BIN_DIR"
        sha256sum gatewayapi-probe gatewayapi-routechange pilot-load
    ) > "${BENCH_OUTPUT_DIR}/upstream/binary-sha256.txt"
    [[ -z "$(git -C "$UPSTREAM_DIR" status --porcelain --untracked-files=all)" ]] || die "upstream build modified its checkout"

    go version > "${BENCH_OUTPUT_DIR}/host/go-version.txt"
    record_event upstream-built
}

write_initial_metadata() {
    local resolved_ref pilot_module haptic_commit source_hash
    resolved_ref="$(<"${BENCH_OUTPUT_DIR}/upstream/resolved-commit.txt")"
    pilot_module="$(<"${BENCH_OUTPUT_DIR}/upstream/pilot-load-module.txt")"
    haptic_commit="$(git -C "$PROJECT_ROOT" rev-parse HEAD)"
    source_hash="$("${PROJECT_ROOT}/scripts/source-hash.sh")"
    local deploy_interval_json=null watch_debounce_json=null
    if [[ -n "$BENCH_DEPLOY_INTERVAL" ]]; then
        deploy_interval_json="$(jq -cn --arg value "$BENCH_DEPLOY_INTERVAL" '$value')"
    fi
    if [[ -n "$BENCH_WATCH_DEBOUNCE" ]]; then
        watch_debounce_json="$(jq -cn --arg value "$BENCH_WATCH_DEBOUNCE" '$value')"
    fi

    jq -n \
        --arg benchmark_repository "$BENCH_REPOSITORY" \
        --arg benchmark_requested_ref "$BENCH_REF" \
        --arg benchmark_commit "$resolved_ref" \
        --arg benchmark_published_commit "$DEFAULT_BENCH_REF" \
        --arg pilot_load_module "$pilot_module" \
        --arg haptic_commit "$haptic_commit" \
        --arg haptic_source_hash "$source_hash" \
        --arg scenarios "$BENCH_SCENARIOS" \
        --arg gateways "$BENCH_GATEWAYS" \
        --arg default_gateways "$DEFAULT_BENCH_GATEWAYS" \
        --argjson deploy_interval "$deploy_interval_json" \
        --argjson watch_debounce "$watch_debounce_json" \
        --arg resource_limit_methodology "no container CPU or memory limits on measured HAPTIC pods" \
        --argjson prometheus_scrape_interval "$PROMETHEUS_SCRAPE_INTERVAL_SECONDS" \
        --arg allow_dirty "$BENCH_ALLOW_DIRTY" \
        --arg allow_coscheduled_clusters "$BENCH_ALLOW_COSCHEDULED_CLUSTERS" \
        --arg cluster_name "$CLUSTER_NAME" \
        --arg cluster_context "$CLUSTER_CONTEXT" \
        --arg kind_node_image "$BENCH_KIND_NODE_IMAGE" \
        --arg kubeconfig "$KUBECONFIG_PATH" \
        --arg docker_network "$DOCKER_NETWORK_NAME" \
        --arg cluster_token "$CLUSTER_OWNERSHIP_TOKEN" \
        --arg reuse_cluster "$REUSE_CLUSTER" \
        --arg keep_cluster "$BENCH_KEEP_CLUSTER" \
        --arg build_only "$BUILD_ONLY" \
        --arg haproxy_version "$HAPROXY_VERSION" \
        --arg default_haproxy_version "$DEFAULT_HAPROXY_VERSION" \
        --arg gateway_api_version "$BENCH_GATEWAY_API_VERSION" \
        --arg gateway_api_channel "$BENCH_GATEWAY_API_CHANNEL" \
        --arg default_gateway_api_version "$DEFAULT_GATEWAY_API_VERSION" \
        --arg default_gateway_api_channel "$DEFAULT_GATEWAY_API_CHANNEL" \
        --arg started_at "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" \
        --argjson probe_routes "$BENCH_PROBE_ROUTES" \
        --argjson default_probe_routes "$DEFAULT_PROBE_ROUTES" \
        --arg probe_timeout "$BENCH_PROBE_TIMEOUT" \
        --argjson routechange_iterations "$BENCH_ROUTECHANGE_ITERATIONS" \
        --argjson default_routechange_iterations "$DEFAULT_ROUTECHANGE_ITERATIONS" \
        --arg routechange_grace_period "$BENCH_ROUTECHANGE_GRACE_PERIOD" \
        --arg default_routechange_grace_period "$DEFAULT_ROUTECHANGE_GRACE_PERIOD" \
        --arg routechange_timeout "$BENCH_ROUTECHANGE_TIMEOUT" \
        --argjson scale_namespaces "$BENCH_SCALE_NAMESPACES" \
        --argjson default_scale_namespaces "$DEFAULT_SCALE_NAMESPACES" \
        --argjson scale_routes_per_namespace "$BENCH_SCALE_ROUTES_PER_NAMESPACE" \
        --argjson default_scale_routes_per_namespace "$DEFAULT_SCALE_ROUTES_PER_NAMESPACE" \
        --arg scale_duration "$BENCH_SCALE_DURATION" \
        --arg default_scale_duration "$DEFAULT_SCALE_DURATION" \
        --arg scale_startup_timeout "$BENCH_SCALE_STARTUP_TIMEOUT" \
        '($scenarios | split(",")) as $selected |
         (($benchmark_commit == $benchmark_published_commit) and
          ($gateway_api_version == $default_gateway_api_version) and
          ($gateway_api_channel == $default_gateway_api_channel) and
          ($gateways == $default_gateways) and
          (($selected | index("probe")) == null or $probe_routes == $default_probe_routes) and
          (($selected | index("routechange")) == null or
            ($routechange_iterations == $default_routechange_iterations and
             $routechange_grace_period == $default_routechange_grace_period)) and
          (($selected | index("scale")) == null or
            ($scale_namespaces == $default_scale_namespaces and
             $scale_routes_per_namespace == $default_scale_routes_per_namespace and
             $scale_duration == $default_scale_duration))) as $published_inputs_match |
         {benchmark_repository: $benchmark_repository,
          benchmark_requested_ref: $benchmark_requested_ref,
          benchmark_commit: $benchmark_commit,
          benchmark_published_commit: $benchmark_published_commit,
          benchmark_published_snapshot: ($benchmark_commit == $benchmark_published_commit),
          pilot_load_module: $pilot_load_module,
          haptic_commit: $haptic_commit,
          haptic_source_hash: $haptic_source_hash,
          scenarios: $selected,
          gateway_targets: ($gateways | split(",")),
          timings: {requested: {min_deployment_interval: $deploy_interval,
                                gateway_and_httproute_debounce: $watch_debounce}},
          resource_limit_methodology: $resource_limit_methodology,
          benchmark_required_profile: {
            gateway_experimental_validation: true,
            loadbalancer_share_process_namespace: false,
            supervised_child_evidence: "exact executable, boot ID, PID, proc starttime, and listener health before and after every scenario"
          },
          resource_metrics: {
            prometheus_scrape_interval_seconds: $prometheus_scrape_interval,
            capture: "paired Prometheus query_range values and source timestamps on the pinned 5-second evaluation grid",
            selectors: "scenario-specific anchored pod and real-container identities are recorded in each queries.json",
            haptic_container_diagnostics: {
              cpu: "raw counter, aggregate cpu label only",
              working_set: "raw gauge",
              rss: "raw gauge"
            },
            upstream_compatible_pod_cgroups: {
              cpu: "raw pod-root cgroup counter, aggregate cpu label only",
              working_set: "raw pod-root cgroup gauge"
            }
          },
          artifact_security: {
            helm_values: "dataplane passwords and webhook CA bundles redacted",
            helm_manifest: "all Secret data/stringData and CA bundles redacted",
            backstop: "bytewise raw-base64 and decoded live Secret values with at least 8 decoded bytes"
          },
          haptic_dirty_allowed: ($allow_dirty == "true"),
          haptic_source_provenance: {
            classification: (if ($allow_dirty == "true") then "dirty-non-comparable" else "clean" end),
            dirty_evidence: "tracked binary patch plus untracked path and content hashes; untracked contents are never copied"
          },
          cluster: {name: $cluster_name, context: $cluster_context, kubeconfig: $kubeconfig,
                    requested_node_image: (if $kind_node_image == "" then null else $kind_node_image end),
                    docker_network: $docker_network, ownership_token: $cluster_token,
                    reuse_requested: ($reuse_cluster == "true"), keep_created: ($keep_cluster == "true"),
                    coscheduled_clusters_allowed: ($allow_coscheduled_clusters == "true"),
                    provenance_class: (if ($build_only == "true") then "none"
                                       elif ($reuse_cluster == "true") then "reused-non-comparable"
                                       elif ($allow_coscheduled_clusters == "true") then "fresh-coscheduled-non-comparable"
                                       else "fresh-controlled" end)},
          comparison: {
            public_comparison: "ballpark-only",
            published_workload_inputs_match: $published_inputs_match,
            controlled_default_profile: (($build_only != "true") and
                                         ($reuse_cluster != "true") and
                                         ($allow_coscheduled_clusters != "true") and
                                         ($allow_dirty != "true") and
                                         ($haproxy_version == $default_haproxy_version) and
                                         ($deploy_interval == null) and
                                         ($watch_debounce == null) and
                                         $published_inputs_match),
            scope: "selected scenarios using pinned upstream programs in an isolated HAPTIC run",
            profile_deviation_reasons: ([
              if ($build_only == "true") then "build-only run has no measurements" else empty end,
              if ($reuse_cluster == "true") then "reused cluster" else empty end,
              if ($allow_coscheduled_clusters == "true") then
                "co-scheduled Kind clusters explicitly allowed; CPU, memory, and latency are non-comparable"
              else empty end,
              if ($allow_dirty == "true") then "dirty HAPTIC source" else empty end,
              if ($benchmark_commit != $benchmark_published_commit) then
                "upstream benchmark commit differs from the published snapshot"
              else empty end,
              if ($gateway_api_version != $default_gateway_api_version) then
                "Gateway API version differs from the published benchmark snapshot"
              else empty end,
              if ($gateway_api_channel != $default_gateway_api_channel) then
                "Gateway API channel differs from the published benchmark snapshot"
              else empty end,
              if ($haproxy_version != $default_haproxy_version) then
                "HAProxy version differs from the HAPTIC product default"
              else empty end,
              if ($gateways != $default_gateways) then "Gateway targets differ from the default workload" else empty end,
              if ($deploy_interval != null) then "minDeploymentInterval override requested" else empty end,
              if ($watch_debounce != null) then "watch debounce override requested" else empty end,
              if (($selected | index("probe")) != null and $probe_routes != $default_probe_routes) then
                "probe route count differs from the published workload"
              else empty end,
              if (($selected | index("routechange")) != null and
                  ($routechange_iterations != $default_routechange_iterations or
                   $routechange_grace_period != $default_routechange_grace_period)) then
                "routechange workload differs from the published workload"
              else empty end,
              if (($selected | index("scale")) != null and
                  ($scale_namespaces != $default_scale_namespaces or
                   $scale_routes_per_namespace != $default_scale_routes_per_namespace or
                   $scale_duration != $default_scale_duration)) then
                "scale workload or measurement duration differs from the default profile"
              else empty end
            ]),
            methodology_limits: [
              "public report results are joined multi-controller runs with shared status contention; this runner targets isolated HAPTIC",
              "probe and routechange logs are parsed directly instead of imported through the public VictoriaMetrics report pipeline",
              "probe and routechange pre-create the exact upstream backend and wait for a ready endpoint, so route 0 measures propagation rather than backend pod start-up",
              "routechange adds a strict per-HAProxy backendRef ResponseHeaderModifier observer as a separate HAPTIC gate",
              "scale substitutes the HAPTIC target and adds HAPTIC readiness proof before a runner-defined 10-minute steady analysis window"
            ],
            defaults: {
              gateways: $default_gateways,
              haproxy_version: $default_haproxy_version,
              probe_routes: $default_probe_routes,
              routechange: {iterations: $default_routechange_iterations,
                            grace_period: $default_routechange_grace_period},
              scale: {namespaces: $default_scale_namespaces,
                      routes_per_namespace: $default_scale_routes_per_namespace,
                      duration: $default_scale_duration}
            }
          },
          workload_execution: "static scratch container on the dedicated kind Docker network",
          gateway_api: {version: $gateway_api_version,
                        channel: $gateway_api_channel,
                        manifest: ("https://github.com/kubernetes-sigs/gateway-api/releases/download/" +
                                   $gateway_api_version + "/" + $gateway_api_channel + "-install.yaml"),
                        published_comparison_version: $default_gateway_api_version,
                        chart_experimental_validation_required: true,
                        published_bundle_inputs_match: (($gateway_api_version == $default_gateway_api_version) and
                                                        ($gateway_api_channel == $default_gateway_api_channel))},
          build_only: ($build_only == "true"),
          haproxy_version: $haproxy_version,
          probe_routes: $probe_routes,
          probe_timeout: $probe_timeout,
          routechange: {iterations: $routechange_iterations, grace_period: $routechange_grace_period, timeout: $routechange_timeout},
          scale: {namespaces: $scale_namespaces, routes_per_namespace: $scale_routes_per_namespace,
                  duration: $scale_duration, startup_timeout: $scale_startup_timeout},
          started_at: $started_at}' \
        > "${BENCH_OUTPUT_DIR}/metadata.json"
}

capture_host_provenance() {
    local output_dir="$1"
    mkdir -p "$output_dir"
    uname -a > "${output_dir}/uname.txt"
    nproc > "${output_dir}/nproc.txt"
    git -C "$PROJECT_ROOT" status --short --branch > "${output_dir}/haptic-git-status.txt"
    git -C "$PROJECT_ROOT" diff --stat HEAD > "${output_dir}/haptic-diff-stat.txt"
    git -C "$PROJECT_ROOT" diff --binary HEAD > "${output_dir}/haptic-dirty.patch"
    git -C "$PROJECT_ROOT" ls-files --others --exclude-standard -z \
        > "${output_dir}/haptic-untracked-files.zlist"
    if [[ -s "${output_dir}/haptic-untracked-files.zlist" ]]; then
        (
            cd "$PROJECT_ROOT"
            xargs -0 -r sha256sum --zero -- < "${output_dir}/haptic-untracked-files.zlist" \
                > "${output_dir}/haptic-untracked-sha256.zlist"
        )
    else
        : > "${output_dir}/haptic-untracked-sha256.zlist"
    fi
    local -a dirty_evidence=(
        haptic-dirty.patch
        haptic-untracked-files.zlist
        haptic-untracked-sha256.zlist
    )
    (cd "$output_dir" && sha256sum "${dirty_evidence[@]}") > "${output_dir}/haptic-dirty-sha256.txt"
    if command -v lscpu >/dev/null 2>&1; then
        lscpu > "${output_dir}/lscpu.txt"
    fi
    if command -v free >/dev/null 2>&1; then
        free -b > "${output_dir}/memory.txt"
    fi
    if command -v docker >/dev/null 2>&1; then
        docker version > "${output_dir}/docker-version.txt" 2>&1 || true
        docker info > "${output_dir}/docker-info.txt" 2>&1 || true
    fi
}

check_cluster_prerequisites() {
    local cmd
    for cmd in kind kubectl helm make docker python3 timeout yq curl ps base64 tr; do
        command_required "$cmd"
    done
    local analyzer="${PROJECT_ROOT}/scripts/analyze-gateway-api-bench.py"
    local resource_analyzer="${PROJECT_ROOT}/scripts/analyze-gateway-api-resources.py"
    local child_analyzer="${PROJECT_ROOT}/scripts/analyze-gateway-api-children.py"
    [[ -f "$analyzer" ]] || die "required analyzer not found: ${analyzer}"
    [[ -f "$resource_analyzer" ]] || die "required resource analyzer not found: ${resource_analyzer}"
    [[ -f "$child_analyzer" ]] || die "required child analyzer not found: ${child_analyzer}"
    python3 -m py_compile "$analyzer" "$resource_analyzer" "$child_analyzer"
    verify_secret_redaction
}

apply_verified_gateway_api_bundle() {
    local manifest="${WORK_DIR}/gateway-api-${BENCH_GATEWAY_API_CHANNEL}-install.yaml"
    local artifact="${BENCH_OUTPUT_DIR}/upstream/gateway-api-experimental-install.yaml"
    local digest_file="${BENCH_OUTPUT_DIR}/upstream/gateway-api-experimental-install-sha256.txt"
    [[ -f "$manifest" && ! -L "$manifest" ]] || die "verified Gateway API manifest is missing"
    [[ -f "$artifact" && ! -L "$artifact" ]] || die "Gateway API manifest artifact is missing"
    cmp -s "$manifest" "$artifact" || die "Gateway API manifest differs from its provenance artifact"

    local actual_digest recorded_digest
    actual_digest="$(sha256sum "$manifest" | awk '{print $1}')"
    recorded_digest="$(awk 'NR == 1 {print $1}' "$digest_file")"
    [[ "$recorded_digest" =~ ^[0-9a-f]{64}$ && "$actual_digest" == "$recorded_digest" ]] || \
        die "Gateway API manifest no longer matches its verified digest"

    printf '%s\n' 'kubectl apply --server-side -f <verified-gateway-api-manifest>' \
        > "${BENCH_OUTPUT_DIR}/cluster/gateway-api-verified-apply-command.txt"
    local apply_rc=0
    run_logged "${BENCH_OUTPUT_DIR}/cluster/gateway-api-verified-apply.log" \
        "${BENCH_OUTPUT_DIR}/cluster/gateway-api-verified-apply-exit-code.txt" \
        kubectl apply --server-side -f "$manifest" || apply_rc=$?
    [[ $apply_rc -eq 0 ]] || die "verified Gateway API bundle apply failed"
    record_event gateway-api-verified-bundle-applied
}

network_pod_identity() {
    jq -eS '
        [.items[] | select(.metadata.deletionTimestamp == null)] |
        if length != 1 then error("expected one benchmark network Pod") else . end |
        map(select(.status.phase == "Running" and (.status.containerStatuses | length) == 1) |
            {uid: .metadata.uid, containers: [.status.containerStatuses[]? |
                select(.ready == true and .restartCount == 0 and .state.running != null) |
                {name, containerID}]}) |
        if length != 1 or (.[0].uid // "") == "" or
           (.[0].containers | length) != 1 or (.[0].containers[0].containerID // "") == ""
        then error("benchmark network Pod is unready or restarted") else . end
    '
}

capture_network_health() {
    local phase="$1" output="${BENCH_OUTPUT_DIR}/cluster/network-${1}"
    kubectl get pods -n kube-system -l k8s-app=calico-node -o json > "${output}-pods.json" || \
        die "could not inspect the benchmark network daemon"
    network_pod_identity < "${output}-pods.json" > "${output}-identity.json" || \
        die "benchmark network daemon is unready or restarted; inspect cluster/network-${phase}-pods.json"
    if [[ "$phase" != baseline ]]; then
        cmp -s "${BENCH_OUTPUT_DIR}/cluster/network-baseline-identity.json" "${output}-identity.json" || \
            die "benchmark network daemon changed identity during the workload"
    fi
}

record_network_daemon() {
    kubectl get daemonset calico-node -n kube-system -o json > "${BENCH_OUTPUT_DIR}/cluster/network-daemonset.json"
    capture_network_health baseline
}

bootstrap_cluster() {
    export KUBECONFIG="$KUBECONFIG_PATH"
    [[ ! -L "$KUBECONFIG_PATH" ]] || die "refusing symlink kubeconfig path: ${KUBECONFIG_PATH}"

    local expected_target_state=absent
    if [[ "$REUSE_CLUSTER" == "true" ]]; then
        expected_target_state=present
    fi
    capture_kind_cluster_inventory preexisting "$expected_target_state"
    local cluster_state="$kind_target_state"
    if [[ "$cluster_state" == "present" ]]; then
        [[ "$REUSE_CLUSTER" == "true" ]] || die "kind cluster ${CLUSTER_NAME} already exists; wait for its owner or set REUSE_CLUSTER=true"
        info "reusing explicitly authorized cluster ${CLUSTER_NAME}"
        verify_network_ownership || \
            die "reused cluster network does not carry the requested benchmark ownership token"
        local reuse_kubeconfig="${WORK_DIR}/reuse-kubeconfig"
        kind get kubeconfig --name "$CLUSTER_NAME" > "$reuse_kubeconfig" || \
            die "failed to generate a kubeconfig for reused cluster ${CLUSTER_NAME}"
        chmod 0600 "$reuse_kubeconfig"
        KUBECONFIG="$reuse_kubeconfig" verify_cluster_ownership || \
            die "reused cluster does not carry the requested benchmark ownership token"
        KUBECONFIG="$reuse_kubeconfig" helm status "$RELEASE_NAME" -n "$RELEASE_NAMESPACE" >/dev/null || \
            die "reused cluster does not contain Helm release ${RELEASE_NAMESPACE}/${RELEASE_NAME}"
        if [[ -e "$KUBECONFIG_PATH" ]]; then
            [[ -f "$KUBECONFIG_PATH" && ! -L "$KUBECONFIG_PATH" && -O "$KUBECONFIG_PATH" ]] || \
                die "existing benchmark kubeconfig is not an owned regular file: ${KUBECONFIG_PATH}"
        fi
        local staged_kubeconfig
        staged_kubeconfig="$(mktemp "/tmp/${CLUSTER_NAME}.kubeconfig.XXXXXX")"
        if ! cp "$reuse_kubeconfig" "$staged_kubeconfig" || ! chmod 0600 "$staged_kubeconfig" || \
            ! mv -fT "$staged_kubeconfig" "$KUBECONFIG_PATH"; then
            [[ ! -e "$staged_kubeconfig" ]] || unlink "$staged_kubeconfig" || true
            die "failed to replace the retained benchmark kubeconfig"
        fi
        kubeconfig_owned=true
        export KUBECONFIG="$KUBECONFIG_PATH"
    elif [[ "$cluster_state" == "absent" ]]; then
        [[ "$REUSE_CLUSTER" != "true" ]] || die "REUSE_CLUSTER=true but ${CLUSTER_NAME} does not exist"
        if docker network inspect "$DOCKER_NETWORK_NAME" >/dev/null 2>&1; then
            die "dedicated Docker network already exists: ${DOCKER_NETWORK_NAME}"
        fi
        [[ ! -e "$KUBECONFIG_PATH" ]] || die "benchmark kubeconfig path already exists: ${KUBECONFIG_PATH}"
        cluster_owned=true
        network_owned=true
        kubeconfig_owned=true
        local network_labels
        network_labels="$(jq -cn --arg cluster "$CLUSTER_NAME" --arg owner "$CLUSTER_OWNERSHIP_TOKEN" \
            '{"haproxy-haptic.org/gateway-api-benchmark":"true", "haproxy-haptic.org/benchmark-cluster":$cluster, "haproxy-haptic.org/benchmark-owner":$owner}')"
        info "creating isolated HAPTIC e2e environment"
        local bootstrap_rc=0
        run_logged "${BENCH_OUTPUT_DIR}/cluster/bootstrap.log" \
            "${BENCH_OUTPUT_DIR}/cluster/bootstrap-exit-code.txt" env \
                -u SKIP_DOCKER_BUILD -u SKIP_CLUSTER_CREATE -u IMAGE_NAME -u IMAGE_TAG -u REGISTRY \
                -u HAPTIC_E2E_PROFILE -u TEST_RUN_PATTERN -u KEEP_CLUSTER -u KEEP_NAMESPACE \
                -u HAPTIC_E2E_CLUSTER_NAME -u HAPTIC_E2E_KUBECONFIG_PATH \
                -u HAPTIC_E2E_EXPOSE_HOST_PORTS -u KIND_EXPERIMENTAL_DOCKER_NETWORK -u KIND_NODE_IMAGE \
                -u HAPTIC_EXPECTED_CONTROLLER_ROLLOUT_ID -u HAPTIC_EXPECTED_CONTROLLER_BINARY_SHA256 \
                -u HAPTIC_EXPECTED_SOURCE_HASH -u HAPTIC_HAPROXY_VERSION -u HAPTIC_E2E_GWAPI_VERSION \
                -u HAPTIC_E2E_GWAPI_CHANNEL \
                -u MAKEFLAGS -u MAKEOVERRIDES \
                -u GO -u GOFLAGS -u CGO_ENABLED -u PARALLEL -u SPOA_TAG -u SOURCE_HASH \
                -u GIT_COMMIT -u GIT_TAG \
                HAPROXY_VERSION="$HAPROXY_VERSION" HAPTIC_E2E_PROFILE=conformance \
                HAPTIC_E2E_CLUSTER_NAME="$CLUSTER_NAME" \
                HAPTIC_E2E_KUBECONFIG_PATH="$KUBECONFIG_PATH" \
                HAPTIC_E2E_EXPOSE_HOST_PORTS=false \
                HAPTIC_E2E_NETWORK_LABELS="$network_labels" \
                KIND_EXPERIMENTAL_DOCKER_NETWORK="$DOCKER_NETWORK_NAME" \
                KIND_NODE_IMAGE="$BENCH_KIND_NODE_IMAGE" \
                HAPTIC_E2E_GWAPI_VERSION="$BENCH_GATEWAY_API_VERSION" \
                HAPTIC_E2E_GWAPI_CHANNEL="$BENCH_GATEWAY_API_CHANNEL" \
                TEST_RUN_PATTERN='^$' KEEP_CLUSTER=true make -C "$PROJECT_ROOT" test-e2e || bootstrap_rc=$?
        if [[ $bootstrap_rc -ne 0 ]]; then
            kubectl get pods -n "$RELEASE_NAMESPACE" -o json 2>/dev/null | pod_failure_summary >&2 || \
                printf 'benchmark: pod failure summary is unavailable\n' >&2
            die "isolated HAPTIC e2e bootstrap failed"
        fi
        verify_network_ownership || die "created Docker network does not carry the benchmark ownership token"
        docker network inspect "$DOCKER_NETWORK_NAME" --format '{{.Id}}' > "${BENCH_OUTPUT_DIR}/cluster/docker-network-id.txt" || \
            die "failed to record the owned benchmark Docker network"
        cluster_state="$(kind_cluster_state "$CLUSTER_NAME")" || \
            die "could not verify the created kind cluster"
        [[ "$cluster_state" == "present" ]] || die "e2e bootstrap did not create ${CLUSTER_NAME}"
        kubectl create configmap haptic-gateway-api-benchmark-owner -n kube-system \
            --from-literal="cluster=${CLUSTER_NAME}" \
            --from-literal="network=${DOCKER_NETWORK_NAME}" \
            --from-literal="token=${CLUSTER_OWNERSHIP_TOKEN}" \
            --dry-run=client -o yaml | kubectl create -f -
        cluster_marker_created=true
        verify_cluster_ownership || die "created cluster ownership marker does not match this run"
    else
        die "unexpected kind cluster state: ${cluster_state}"
    fi

    apply_verified_gateway_api_bundle
    capture_kind_cluster_inventory after-bootstrap present
    docker network inspect "$DOCKER_NETWORK_NAME" > "${BENCH_OUTPUT_DIR}/cluster/docker-network.json" || \
        die "benchmark Docker network disappeared after cluster bootstrap"
    kubectl get configmap haptic-gateway-api-benchmark-owner -n kube-system -o json \
        > "${BENCH_OUTPUT_DIR}/cluster/ownership.json" || \
        die "benchmark cluster ownership marker disappeared after bootstrap"
    [[ -s "$KUBECONFIG_PATH" ]] || die "benchmark kubeconfig is missing or empty: ${KUBECONFIG_PATH}"
    [[ "$(kubectl config current-context)" == "$CLUSTER_CONTEXT" ]] || die "unexpected kubectl context: $(kubectl config current-context)"
    [[ "$(kubectl config view --minify -o jsonpath='{.contexts[0].context.cluster}')" == "$CLUSTER_CONTEXT" ]] || \
        die "kubeconfig is not isolated to ${CLUSTER_CONTEXT}"
    docker inspect "${CLUSTER_NAME}-control-plane" > "${BENCH_OUTPUT_DIR}/cluster/kind-control-plane.json"
    docker inspect "${CLUSTER_NAME}-control-plane" | jq -e --arg network "$DOCKER_NETWORK_NAME" \
        '.[0].NetworkSettings.Networks | has($network)' >/dev/null || \
        die "kind control plane is not attached to ${DOCKER_NETWORK_NAME}"
    capture_gateway_api_provenance
    prepare_workload_image
    record_event cluster-ready
}

capture_gateway_api_provenance() {
    local output="${BENCH_OUTPUT_DIR}/cluster/gateway-api-crds.json"
    kubectl get customresourcedefinitions.apiextensions.k8s.io -o json | jq -S '
        [.items[] |
          select(.spec.group == "gateway.networking.k8s.io" or
                 .spec.group == "gateway.networking.x-k8s.io") |
          {name: .metadata.name,
           group: .spec.group,
           uid: .metadata.uid,
           resource_version: .metadata.resourceVersion,
           bundle_version: .metadata.annotations["gateway.networking.k8s.io/bundle-version"]}] |
        sort_by(.name)
    ' > "$output"
    jq -e \
        --arg expected_version "$BENCH_GATEWAY_API_VERSION" \
        --arg default_version "$DEFAULT_GATEWAY_API_VERSION" '
        all(.[]; .bundle_version == $expected_version) and
        if $expected_version == $default_version then
          ([.[].name] | sort) == ([
            "backendtlspolicies.gateway.networking.k8s.io",
            "gatewayclasses.gateway.networking.k8s.io",
            "gateways.gateway.networking.k8s.io",
            "grpcroutes.gateway.networking.k8s.io",
            "httproutes.gateway.networking.k8s.io",
            "referencegrants.gateway.networking.k8s.io",
            "tcproutes.gateway.networking.k8s.io",
            "tlsroutes.gateway.networking.k8s.io",
            "udproutes.gateway.networking.k8s.io",
            "xbackendtrafficpolicies.gateway.networking.x-k8s.io",
            "xlistenersets.gateway.networking.x-k8s.io",
            "xmeshes.gateway.networking.x-k8s.io"
          ] | sort)
        else
          length > 0 and any(.[]; .group == "gateway.networking.x-k8s.io")
        end
    ' "$output" >/dev/null || \
        die "installed Gateway API CRDs are not the ${BENCH_GATEWAY_API_VERSION} experimental bundle"
}

prepare_workload_image() {
    local context_dir="${WORK_DIR}/workload-image"
    mkdir -p "$context_dir"
    cp "$BIN_DIR/gatewayapi-probe" "$BIN_DIR/gatewayapi-routechange" "$BIN_DIR/pilot-load" "$context_dir/"
    chmod 0555 "$context_dir/gatewayapi-probe" "$context_dir/gatewayapi-routechange" "$context_dir/pilot-load"
    KIND_EXPERIMENTAL_DOCKER_NETWORK="$DOCKER_NETWORK_NAME" \
        kind get kubeconfig --internal --name "$CLUSTER_NAME" > "${WORK_DIR}/workload-kubeconfig"
    chmod 0400 "${WORK_DIR}/workload-kubeconfig"
    printf '%s\n' \
        'FROM scratch' \
        'COPY gatewayapi-probe /gatewayapi-probe' \
        'COPY gatewayapi-routechange /gatewayapi-routechange' \
        'COPY pilot-load /pilot-load' \
        'ENV KUBECONFIG=/kubeconfig' \
        'LABEL org.opencontainers.image.title="HAPTIC gateway-api-bench workload"' \
        > "$context_dir/Dockerfile"
    find "$context_dir" -mindepth 1 -maxdepth 1 -printf '%f\n' | sort \
        > "${BENCH_OUTPUT_DIR}/cluster/workload-image-context-files.txt"
    printf '%s\n' Dockerfile gatewayapi-probe gatewayapi-routechange pilot-load | sort | \
        cmp -s - "${BENCH_OUTPUT_DIR}/cluster/workload-image-context-files.txt" || \
        die "workload image context contains an unexpected file"
    (
        cd "$context_dir"
        sha256sum Dockerfile gatewayapi-probe gatewayapi-routechange pilot-load
    ) > "${BENCH_OUTPUT_DIR}/cluster/workload-image-context-sha256.txt"
    local build_rc=0
    run_logged "${BENCH_OUTPUT_DIR}/cluster/workload-image-build.log" \
        "${BENCH_OUTPUT_DIR}/cluster/workload-image-build-exit-code.txt" \
        env DOCKER_BUILDKIT=1 docker build --pull=false --tag "$WORKLOAD_IMAGE" "$context_dir" || build_rc=$?
    [[ $build_rc -eq 0 ]] || die "failed to build the static benchmark workload image"
    workload_image_built=true
    docker image inspect "$WORKLOAD_IMAGE" > "${BENCH_OUTPUT_DIR}/cluster/workload-image.json"
    docker history --no-trunc "$WORKLOAD_IMAGE" > "${BENCH_OUTPUT_DIR}/cluster/workload-image-history.txt"
    jq -e '.[0].Config.Env | index("KUBECONFIG=/kubeconfig") != null' \
        "${BENCH_OUTPUT_DIR}/cluster/workload-image.json" >/dev/null || \
        die "workload image does not declare the injected kubeconfig path"
}

create_workload_container() {
    local scenario="$1"
    local binary="$2"
    shift 2
    local scenario_dir="${BENCH_OUTPUT_DIR}/${scenario}"
    [[ -z "$active_workload_container" ]] || die "another benchmark workload container is still active"
    active_workload_container="${CLUSTER_NAME}-${scenario}"
    if ! docker create \
        --name "$active_workload_container" \
        --network "$DOCKER_NETWORK_NAME" \
        --label "haproxy-haptic.org/benchmark-run=${RUN_TOKEN}" \
        "$WORKLOAD_IMAGE" "/${binary}" "$@" \
        > "$scenario_dir/workload-container-create-id.txt" \
        2> "$scenario_dir/workload-container-create.stderr"; then
        die "failed to create ${scenario} workload container; see ${scenario_dir}/workload-container-create.stderr"
    fi
    [[ -s "$scenario_dir/workload-container-create-id.txt" ]] || \
        die "Docker created ${scenario} workload container without returning an ID"
    docker cp "${WORK_DIR}/workload-kubeconfig" "${active_workload_container}:/kubeconfig" || \
        die "failed to inject the internal kubeconfig into the stopped workload container"
    docker inspect "$active_workload_container" > "${BENCH_OUTPUT_DIR}/${scenario}/workload-container-before.json"
}

finish_workload_container() {
    local scenario_dir="$1"
    local expected_exit="$2"
    local state_exit
    docker inspect "$active_workload_container" > "$scenario_dir/workload-container-after.json"
    state_exit="$(jq -er '.[0].State | select(.Running == false) | .ExitCode' \
        "$scenario_dir/workload-container-after.json")" || die "workload container is still running"
    [[ "$state_exit" -eq "$expected_exit" ]] || \
        die "workload container exit ${state_exit} differs from observed exit ${expected_exit}"
    docker rm "$active_workload_container" >/dev/null || die "failed to remove workload container"
    active_workload_container=""
}

wait_for_config_valid() {
    local deadline=$((SECONDS + 360))
    local config
    while (( SECONDS < deadline )); do
        config="$(kubectl get haproxytemplateconfig "$RELEASE_NAME-config" -n "$RELEASE_NAMESPACE" -o json)"
        if jq -e '
            .metadata.generation as $generation |
            .status.observedGeneration == $generation and
            any(.status.conditions[]?; .type == "Validated" and .status == "True" and .observedGeneration == $generation)
        ' <<< "$config" >/dev/null; then
            return 0
        fi
        if jq -e '
            .metadata.generation as $generation |
            any(.status.conditions[]?; .type == "Validated" and .status == "False" and .observedGeneration == $generation)
        ' <<< "$config" >/dev/null; then
            jq '.status' <<< "$config" >&2
            die "HAProxyTemplateConfig validation failed"
        fi
        sleep 2
    done
    die "HAProxyTemplateConfig did not become Validated=True at its current generation"
}

assert_no_limit_workloads() {
    local manifest="$1"
    local defaults="$2"
    yq -o=json -I=0 '.' "$manifest" | jq -L "${PROJECT_ROOT}/scripts/lib" -se --slurpfile defaults "$defaults" '
        include "benchmark-runtime";
        $defaults[0].controller.resources.requests as $controller_requests |
        [.[] | select(.kind == "Deployment" and
          (.metadata.name == "haptic-controller" or .metadata.name == "haptic-haproxy"))] as $deployments |
        ($deployments | map(.metadata.name) | sort) == ["haptic-controller", "haptic-haproxy"] and
        all($deployments[]; all(.spec.template.spec | measured_containers;
          benchmark_runtime_matches($defaults[0]))) and
        any($deployments[]; .metadata.name == "haptic-controller" and
          any(.spec.template.spec.containers[]; .name == "controller" and
            .resources.requests == $controller_requests and
            .livenessProbe == $defaults[0].controller.livenessProbe and
            .readinessProbe == $defaults[0].controller.readinessProbe)) and
        any($deployments[]; .metadata.name == "haptic-haproxy" and
          ((.spec.template.spec.shareProcessNamespace // false) == false))
    ' >/dev/null
}

assert_no_limit_pods() {
    local pods="$1"
    local defaults="$2"
    jq -L "${PROJECT_ROOT}/scripts/lib" -e --slurpfile defaults "$defaults" '
        include "benchmark-runtime";
        [.items[] |
          select(.metadata.labels["app.kubernetes.io/component"] == "controller" or
                 .metadata.labels["app.kubernetes.io/component"] == "loadbalancer")] as $pods |
        any($pods[]; .metadata.labels["app.kubernetes.io/component"] == "controller") and
        any($pods[]; .metadata.labels["app.kubernetes.io/component"] == "loadbalancer") and
        all($pods[]; all(.spec | measured_containers; benchmark_runtime_matches($defaults[0])))
    ' "$pods" >/dev/null
}

assert_effective_profile() {
    local defaults="$1"
    local effective="$2"
    local output="$3"
    python3 - "$defaults" "$effective" "$output" <<'PY'
import json
import sys

defaults_path, effective_path, output_path = sys.argv[1:]
with open(defaults_path, encoding="utf-8") as handle:
    defaults = json.load(handle)
with open(effective_path, encoding="utf-8") as handle:
    effective = json.load(handle)

allowed = (
    "credentials.dataplane.password",
    "controller.image.",
    "controller.podSpec.podAnnotations",
    "controller.webhook.caBundle",
    "controller.resources.limits",
    "controller.validators.resources.limits",
    "haproxy.resources.limits",
    "haproxy.agent.resources.limits",
    "spoaHub.resources.limits",
    "vector.resources.limits",
    "haproxy.service.type",
    "haproxyVersion",
    "controller.config.dataplane.minDeploymentInterval",
    "controller.config.watchedResources.gateways.debounceInterval",
    "controller.config.watchedResources.httproutes.debounceInterval",
    "controller.templateLibraries.gateway.experimentalChannel",
)
missing = object()
differences = []


def artifact_value(name, value):
    if value is missing:
        return None
    if name in ("credentials.dataplane.password", "controller.webhook.caBundle"):
        return "<redacted>"
    return value


def walk(path, before, after):
    if isinstance(before, dict) and isinstance(after, dict):
        for key in sorted(set(before) | set(after)):
            walk(path + [key], before.get(key, missing), after.get(key, missing))
        return
    if before is missing and isinstance(after, dict):
        for key in sorted(after):
            walk(path + [key], missing, after[key])
        return
    if after is missing and isinstance(before, dict):
        for key in sorted(before):
            walk(path + [key], before[key], missing)
        return
    if before is None and after is missing:
        return
    if before == after:
        return
    name = ".".join(path)
    differences.append({
        "path": name,
        "chart_default_present": before is not missing,
        "chart_default": artifact_value(name, before),
        "effective_present": after is not missing,
        "effective": artifact_value(name, after),
        "allowed": any(name == prefix or prefix.endswith(".") and name.startswith(prefix) or
                       name.startswith(prefix + ".") for prefix in allowed),
    })


walk([], defaults, effective)
result = {
    "schema_version": 2,
    "method": "chart defaults plus explicit benchmark runtime overrides",
    "differences": differences,
    "unexpected": [item for item in differences if not item["allowed"]],
}
with open(output_path, "w", encoding="utf-8") as handle:
    json.dump(result, handle, indent=2, sort_keys=True)
    handle.write("\n")
if result["unexpected"]:
    json.dump(result["unexpected"], sys.stderr, indent=2, sort_keys=True)
    sys.stderr.write("\n")
    raise SystemExit(1)
PY
}

verify_spoa_bundle() {
    local phase="${1:-before}" evidence="${BENCH_OUTPUT_DIR}/cluster"
    local chart_image image_id
    chart_image="$(bash "${PROJECT_ROOT}/scripts/chart-spoa-image.sh")" || \
        die "cannot resolve the chart's SPOA image"
    local checksum_command='export LC_ALL=C; sha256sum /usr/local/bin/haproxy-spoa-hub /etc/haproxy-spoa-hub/plugins/*.so'
    if [[ "$phase" == "before" ]]; then
        docker image inspect "$chart_image" > "${evidence}/spoa-bundle-image.json"
        image_id="$(jq -er 'select(length == 1) | .[0].Id |
            select(test("^sha256:[0-9a-f]{64}$"))' "${evidence}/spoa-bundle-image.json")" || \
            die "chart SPOA image has no immutable image ID"
        printf '%s\n' "$chart_image" > "${evidence}/spoa-bundle-image-reference.txt"
        docker run --rm --network none --entrypoint sh "$image_id" \
            -ec "$checksum_command" > "${evidence}/spoa-bundle-expected.sha256"
        [[ -s "${evidence}/spoa-bundle-expected.sha256" ]] || die "SPOA bundle checksum evidence is empty"
    fi
    [[ "$(<"${evidence}/spoa-bundle-image-reference.txt")" == "$chart_image" ]] || \
        die "chart SPOA image changed during the benchmark"
    local pods="${evidence}/spoa-bundle-${phase}-pods.json"
    kubectl get pods -n "$RELEASE_NAMESPACE" -o json > "$pods"
    jq -e --arg image "$chart_image" '
        [.items[] | (.spec.initContainers[]?, .spec.containers[]) | select(.name == "spoa-hub" or .name == "validators")] as $containers |
        any($containers[]; .name == "spoa-hub") and any($containers[]; .name == "validators") and
        all($containers[]; .image == $image) and
        all(.items[] | select(any((.spec.initContainers[]?, .spec.containers[]); .name == "spoa-hub" or .name == "validators"));
          .metadata.deletionTimestamp == null) and
        all(.items[]; . as $pod |
          all((.spec.initContainers[]?, .spec.containers[]) | select(.name == "spoa-hub" or .name == "validators");
            .name as $name | any(($pod.status.initContainerStatuses[]?, $pod.status.containerStatuses[]?);
              .name == $name and .ready == true and (.imageID | length) > 0)))
    ' "$pods" >/dev/null || die "SPOA pods do not identify the ready chart bundle; inspect ${pods}"
    local pod container actual
    while IFS=$'\t' read -r pod container; do
        actual="${evidence}/spoa-bundle-${phase}-${pod}-${container}.sha256"
        kubectl exec -n "$RELEASE_NAMESPACE" "$pod" -c "$container" -- \
            sh -ec "$checksum_command" > "$actual"
        cmp "${evidence}/spoa-bundle-expected.sha256" "$actual" || \
            die "SPOA bundle bytes differ in ${pod}/${container}; redeploy the chart-selected image"
    done < <(jq -r '.items[] | .metadata.name as $pod | (.spec.initContainers[]?, .spec.containers[]) |
        select(.name == "spoa-hub" or .name == "validators") | [$pod, .name] | @tsv' "$pods")
}

configure_haptic() {
    local product_deploy_interval desired_deploy_interval
    product_deploy_interval="$(yq -er '.controller.config.dataplane.minDeploymentInterval' \
        "${PROJECT_ROOT}/charts/haptic/values.yaml")"
    [[ "$product_deploy_interval" =~ ^[0-9]+(ns|us|ms|s|m|h)$ ]] || \
        die "chart product minDeploymentInterval is invalid: ${product_deploy_interval}"
    desired_deploy_interval="${BENCH_DEPLOY_INTERVAL:-$product_deploy_interval}"
    rg -q '^[[:space:]]*DefaultDebounceInterval = 100 \* time\.Millisecond$' \
        "${PROJECT_ROOT}/pkg/k8s/types/types.go" || \
        die "controller watcher default changed; update benchmark timing provenance"
    local before_values="${WORK_DIR}/helm-values-before-normalization.json"
    local before_values_redacted="${BENCH_OUTPUT_DIR}/cluster/helm-values-before-normalization.json"
    local runtime_values="${WORK_DIR}/benchmark-runtime-values.json"
    local chart_defaults="${BENCH_OUTPUT_DIR}/cluster/chart-default-values.json"
    helm get values "$RELEASE_NAME" -n "$RELEASE_NAMESPACE" --all -o json | jq -S . > "$before_values"
    redact_helm_values "$before_values" "$before_values_redacted"
    yq -o=json -I=0 '.' "${PROJECT_ROOT}/charts/haptic/values.yaml" | jq -S . > "$chart_defaults"
    jq -S --arg haproxy_version "$HAPROXY_VERSION" '
        {haproxyVersion: $haproxy_version,
         credentials: {dataplane: .credentials.dataplane},
         controller: {
           image: .controller.image,
           podSpec: {podAnnotations: {
             "haproxy-haptic.org/source-hash": .controller.podSpec.podAnnotations["haproxy-haptic.org/source-hash"],
             "haproxy-haptic.org/e2e-rollout-id": .controller.podSpec.podAnnotations["haproxy-haptic.org/e2e-rollout-id"],
             "haproxy-haptic.org/controller-binary-sha256": .controller.podSpec.podAnnotations["haproxy-haptic.org/controller-binary-sha256"]}},
           webhook: {caBundle: .controller.webhook.caBundle}},
         haproxy: {service: {type: "LoadBalancer"}}}
    ' "$before_values" > "$runtime_values"
    jq -e '
        (.controller.image.repository | type) == "string" and
        (.controller.image.repository | length) > 0 and
        (.controller.image.tag | type) == "string" and
        (.controller.image.tag | length) > 0 and
        .controller.image.pullPolicy == "Never" and
        (.credentials.dataplane.username | type) == "string" and
        (.credentials.dataplane.username | length) > 0 and
        (.credentials.dataplane.password | type) == "string" and
        (.credentials.dataplane.password | length) >= 8 and
        (.controller.webhook.caBundle | type) == "string" and
        (.controller.webhook.caBundle | length) > 0 and
        (.controller.podSpec.podAnnotations["haproxy-haptic.org/source-hash"] | length) > 0 and
        (.controller.podSpec.podAnnotations["haproxy-haptic.org/e2e-rollout-id"] | length) > 0 and
        (.controller.podSpec.podAnnotations["haproxy-haptic.org/controller-binary-sha256"] | length) > 0
    ' "$runtime_values" >/dev/null || die "bootstrap values do not identify the locally built HAPTIC runtime"
    local -a helm_args=(
        upgrade "$RELEASE_NAME" "${PROJECT_ROOT}/charts/haptic"
        --namespace "$RELEASE_NAMESPACE"
        --reset-values
        --values "$runtime_values"
        --wait
        --timeout 10m
        --set-string "controller.config.dataplane.minDeploymentInterval=${desired_deploy_interval}"
        --set 'controller.resources.limits=null'
        --set 'controller.validators.resources.limits=null'
        --set 'haproxy.resources.limits=null'
        --set 'haproxy.agent.resources.limits=null'
        --set 'spoaHub.resources.limits=null'
        --set 'vector.resources.limits=null'
        --set 'controller.templateLibraries.gateway.experimentalChannel=true'
    )
    if [[ -n "$BENCH_WATCH_DEBOUNCE" ]]; then
        helm_args+=(
            --set-string "controller.config.watchedResources.gateways.debounceInterval=${BENCH_WATCH_DEBOUNCE}"
            --set-string "controller.config.watchedResources.httproutes.debounceInterval=${BENCH_WATCH_DEBOUNCE}"
        )
    fi

    info "setting benchmark timings and removing measured-pod resource limits"
    helm "${helm_args[@]}" 2>&1 | tee "${BENCH_OUTPUT_DIR}/cluster/configure.log"
    kubectl rollout status deployment/haptic-controller -n haptic --timeout=10m
    kubectl rollout status deployment/haptic-haproxy -n haptic --timeout=10m
    wait_for_config_valid
    wait_for_measured_pods_stable
    capture_effective_timings "$desired_deploy_interval" "$product_deploy_interval"

    local effective_values_raw="${WORK_DIR}/helm-values-all.json"
    local manifest_raw="${WORK_DIR}/helm-manifest.yaml"
    helm get values "$RELEASE_NAME" -n "$RELEASE_NAMESPACE" --all -o json | jq -S . \
        > "$effective_values_raw"
    redact_helm_values "$effective_values_raw" "${BENCH_OUTPUT_DIR}/cluster/helm-values-all.json"
    yq -P '.' "${BENCH_OUTPUT_DIR}/cluster/helm-values-all.json" \
        > "${BENCH_OUTPUT_DIR}/cluster/helm-values-all.yaml"
    assert_effective_profile "$chart_defaults" "$effective_values_raw" \
        "${BENCH_OUTPUT_DIR}/cluster/effective-values-diff.json" || \
        die "effective Helm values retain settings outside the product-default benchmark profile"
    jq -e '
        .controller.templateLibraries.gateway.experimentalChannel == true and
        .haproxy.podSpec.shareProcessNamespace == false
    ' \
        "$effective_values_raw" >/dev/null || \
        die "effective Helm values do not enable experimental validation with private process namespaces"
    helm get manifest "$RELEASE_NAME" -n "$RELEASE_NAMESPACE" > "$manifest_raw"
    redact_helm_manifest "$manifest_raw" "${BENCH_OUTPUT_DIR}/cluster/helm-manifest.yaml"
    kubectl get haproxytemplateconfig,haproxytemplatelibrary -n "$RELEASE_NAMESPACE" -o yaml \
        > "${BENCH_OUTPUT_DIR}/cluster/template-config.yaml"
    kubectl get haproxytemplatelibrary "$RELEASE_NAME-config-gateway" \
        -n "$RELEASE_NAMESPACE" -o json > "${BENCH_OUTPUT_DIR}/cluster/gateway-library.json"
    jq -e '
        (.spec.validationTests["test-httproute-retry-attempts-and-codes"] | type) == "object" and
        (.spec.validationTests["test-httproute-session-persistence-cookie-default"] | type) == "object"
    ' "${BENCH_OUTPUT_DIR}/cluster/gateway-library.json" >/dev/null || \
        die "effective HAPTIC Gateway library omitted experimental-channel validation tests"
    jq -n \
        --slurpfile values "$effective_values_raw" \
        --slurpfile library "${BENCH_OUTPUT_DIR}/cluster/gateway-library.json" '
        {schema_version: 1, requested_channel: "experimental",
         helm_value: $values[0].controller.templateLibraries.gateway.experimentalChannel,
         gateway_library: $library[0].metadata.name,
         experimental_validation_tests: [
           "test-httproute-retry-attempts-and-codes",
           "test-httproute-session-persistence-cookie-default"
         ],
         pass: ($values[0].controller.templateLibraries.gateway.experimentalChannel == true and
                ($library[0].spec.validationTests["test-httproute-retry-attempts-and-codes"] | type) == "object" and
                ($library[0].spec.validationTests["test-httproute-session-persistence-cookie-default"] | type) == "object")}
    ' > "${BENCH_OUTPUT_DIR}/cluster/experimental-channel-profile.json"

    assert_no_limit_workloads "$manifest_raw" "$chart_defaults" ||
        die "rendered HAPTIC workloads do not match the no-limit benchmark methodology"

    kubectl get pods -n "$RELEASE_NAMESPACE" -l app.kubernetes.io/instance=haptic -o json \
        > "${BENCH_OUTPUT_DIR}/cluster/measured-pods.json"
    assert_no_limit_pods "${BENCH_OUTPUT_DIR}/cluster/measured-pods.json" "$chart_defaults" || \
        die "measured HAPTIC pods differ from the no-limit product-default runtime profile"

    jq '
        [.items[] |
          select(.metadata.labels["app.kubernetes.io/component"] == "controller" or
                 .metadata.labels["app.kubernetes.io/component"] == "loadbalancer") |
          {namespace: .metadata.namespace, pod: .metadata.name,
           component: .metadata.labels["app.kubernetes.io/component"],
           containers: [((.spec.initContainers[]? | select(.restartPolicy == "Always")), .spec.containers[]) |
             {name, resources, env: [.env[]? | select(.name == "GOMEMLIMIT" or .name == "GOMAXPROCS" or
                                                    .name == "GOGC" or .name == "TMPDIR")]}]}]
    ' "${BENCH_OUTPUT_DIR}/cluster/measured-pods.json" > "${BENCH_OUTPUT_DIR}/cluster/resource-methodology.json"
    verify_controller_runtime_identity
    record_event haptic-configured
}

capture_effective_timings() {
    local desired_deploy_interval="$1"
    local product_deploy_interval="$2"
    local config="${BENCH_OUTPUT_DIR}/cluster/effective-template-config.json"
    local output="${BENCH_OUTPUT_DIR}/cluster/effective-timings.json"
    local deploy_request_json=null watch_request_json=null
    if [[ -n "$BENCH_DEPLOY_INTERVAL" ]]; then
        deploy_request_json="$(jq -cn --arg value "$BENCH_DEPLOY_INTERVAL" '$value')"
    fi
    if [[ -n "$BENCH_WATCH_DEBOUNCE" ]]; then
        watch_request_json="$(jq -cn --arg value "$BENCH_WATCH_DEBOUNCE" '$value')"
    fi
    kubectl get haproxytemplateconfig "$RELEASE_NAME-config" -n "$RELEASE_NAMESPACE" -o json > "$config"
    jq -e --arg deploy "$desired_deploy_interval" '
        .spec.dataplane.minDeploymentInterval == $deploy
    ' "$config" >/dev/null || die "effective minDeploymentInterval differs from the selected product/benchmark value"
    if [[ -n "$BENCH_WATCH_DEBOUNCE" ]]; then
        jq -e --arg debounce "$BENCH_WATCH_DEBOUNCE" '
            .spec.watchedResources.gateways.debounceInterval == $debounce and
            .spec.watchedResources.httproutes.debounceInterval == $debounce
        ' "$config" >/dev/null || die "effective Gateway/HTTPRoute debounce differs from BENCH_WATCH_DEBOUNCE"
    fi
    jq -n \
        --argjson deploy_request "$deploy_request_json" \
        --argjson watch_request "$watch_request_json" \
        --arg product_deploy "$product_deploy_interval" \
        --arg controller_watch_default "$CONTROLLER_DEFAULT_WATCH_DEBOUNCE" \
        --arg desired_deploy "$desired_deploy_interval" \
        --slurpfile config "$config" '
        ($config[0].spec.watchedResources.gateways.debounceInterval // null) as $gateway_configured |
        ($config[0].spec.watchedResources.httproutes.debounceInterval // null) as $route_configured |
        {requested: {min_deployment_interval: $deploy_request,
                     gateway_and_httproute_debounce: $watch_request},
         product_defaults: {min_deployment_interval: $product_deploy,
                            watcher_debounce: $controller_watch_default},
         configured: {min_deployment_interval: $config[0].spec.dataplane.minDeploymentInterval,
                      gateway_debounce: $gateway_configured,
                      httproute_debounce: $route_configured},
         effective: {min_deployment_interval: $desired_deploy,
                     gateway_debounce: ($gateway_configured // $controller_watch_default),
                     httproute_debounce: ($route_configured // $controller_watch_default)}}
    ' > "$output"
    local metadata_tmp="${BENCH_OUTPUT_DIR}/metadata.json.tmp"
    jq --slurpfile timings "$output" '.timings = $timings[0]' \
        "${BENCH_OUTPUT_DIR}/metadata.json" > "$metadata_tmp"
    mv "$metadata_tmp" "${BENCH_OUTPUT_DIR}/metadata.json"
}

wait_for_measured_pods_stable() {
    local deadline=$((SECONDS + 300))
    local pods="${WORK_DIR}/measured-pods.json"
    while (( SECONDS < deadline )); do
        kubectl get pods -n haptic -l app.kubernetes.io/instance=haptic -o json > "$pods"
        if jq -e '
            [.items[] |
              select(.metadata.labels["app.kubernetes.io/component"] == "controller" or
                     .metadata.labels["app.kubernetes.io/component"] == "loadbalancer")] as $pods |
            any($pods[]; .metadata.labels["app.kubernetes.io/component"] == "controller") and
            any($pods[]; .metadata.labels["app.kubernetes.io/component"] == "loadbalancer") and
            all($pods[];
              .metadata.deletionTimestamp == null and
              any(.status.conditions[]?; .type == "Ready" and .status == "True") and
              (.status.containerStatuses | length) > 0 and
              all(.status.containerStatuses[]?; .ready == true and .restartCount == 0))
        ' "$pods" >/dev/null; then
            return 0
        fi
        sleep 2
    done
    die "measured HAPTIC pods did not reach a stable Ready state"
}

verify_controller_runtime_identity() {
    local output_dir="${BENCH_OUTPUT_DIR}/cluster/controller-runtime"
    local before_pods="$output_dir/pods-before.json"
    local after_pods="$output_dir/pods-after.json"
    local before_identities="$output_dir/identities-before.json"
    local after_identities="$output_dir/identities-after.json"
    local deployment="$output_dir/deployment.json"
    local exec_results="$output_dir/binary-verification.ndjson"
    local expected_source rollout_id binary_sha256
    mkdir -p "$output_dir"
    expected_source="$("${PROJECT_ROOT}/scripts/source-hash.sh")"

    kubectl get pods -n "$RELEASE_NAMESPACE" -l app.kubernetes.io/instance=haptic -o json > "$before_pods"
    kubectl get deployment/haptic-controller -n "$RELEASE_NAMESPACE" -o json > "$deployment"
    extract_haptic_identities "$before_pods" "$before_identities"
    jq -e --arg source "$expected_source" --slurpfile deployment "$deployment" '
        [.items[] | select(.metadata.labels["app.kubernetes.io/component"] == "controller")] as $pods |
        ($deployment[0].spec.replicas // 1) as $desired |
        ($pods | length) == $desired and $desired > 0 and
        ($deployment[0].spec.template.metadata.annotations["haproxy-haptic.org/source-hash"] == $source) and
        all($pods[];
          .metadata.annotations["haproxy-haptic.org/source-hash"] == $source and
          (.metadata.annotations["haproxy-haptic.org/e2e-rollout-id"] |
            test("^sha256:[0-9a-f]{64}$")) and
          (.metadata.annotations["haproxy-haptic.org/controller-binary-sha256"] |
            test("^[0-9a-f]{64}$")) and
          any(.status.containerStatuses[]?; .name == "controller" and
            (.imageID // "") != "" and (.containerID // "") != "")) and
        ([ $pods[].metadata.annotations["haproxy-haptic.org/e2e-rollout-id"] ] | unique | length) == 1 and
        ([ $pods[].metadata.annotations["haproxy-haptic.org/controller-binary-sha256"] ] | unique | length) == 1
    ' "$before_pods" >/dev/null || die "controller runtime annotations do not identify this HAPTIC source"

    rollout_id="$(jq -er '[.items[] | select(.metadata.labels["app.kubernetes.io/component"] == "controller") |
        .metadata.annotations["haproxy-haptic.org/e2e-rollout-id"]] | unique | .[0]' "$before_pods")"
    binary_sha256="$(jq -er '[.items[] | select(.metadata.labels["app.kubernetes.io/component"] == "controller") |
        .metadata.annotations["haproxy-haptic.org/controller-binary-sha256"]] | unique | .[0]' "$before_pods")"
    : > "$exec_results"
    local pod checksum_output checksum
    local -a controller_pods
    mapfile -t controller_pods < <(jq -r '.items[] |
        select(.metadata.labels["app.kubernetes.io/component"] == "controller") | .metadata.name' "$before_pods" | sort)
    for pod in "${controller_pods[@]}"; do
        checksum_output="$(kubectl exec -n "$RELEASE_NAMESPACE" "$pod" -c controller -- \
            sha256sum /usr/local/bin/haptic)"
        checksum="$(awk '$2 == "/usr/local/bin/haptic" {print $1; found++}
            END {if (found != 1) exit 1}' <<< "$checksum_output")" || \
            die "controller binary checksum output is invalid on ${pod}"
        [[ "$checksum" == "$binary_sha256" ]] || die "controller binary checksum differs on ${pod}"
        jq -cn --arg pod "$pod" --arg checksum "$checksum" '{pod: $pod, checksum: $checksum}' \
            >> "$exec_results"
    done

    kubectl get pods -n "$RELEASE_NAMESPACE" -l app.kubernetes.io/instance=haptic -o json > "$after_pods"
    extract_haptic_identities "$after_pods" "$after_identities"
    cmp -s "$before_identities" "$after_identities" || \
        die "measured pod identity changed during controller binary verification"
    jq -n \
        --arg source_hash "$expected_source" \
        --arg rollout_id "$rollout_id" \
        --arg binary_sha256 "$binary_sha256" \
        --slurpfile identities "$before_identities" \
        --slurpfile verification "$exec_results" \
        '{source_hash: $source_hash, rollout_id: $rollout_id, binary_sha256: $binary_sha256,
          identities: $identities[0], binary_verification: $verification}' \
        > "$output_dir/verified.json"
}

wait_for_gateway() {
    local gateway="$1"
    local namespace="${gateway%/*}"
    local name="${gateway#*/}"
    local deadline=$((SECONDS + 300))
    local object
    while (( SECONDS < deadline )); do
        object="$(kubectl get gateway "$name" -n "$namespace" -o json 2>/dev/null || true)"
        if [[ -n "$object" ]] && jq -e '
            .metadata.generation as $generation |
            (.status.addresses | length) > 0 and
            any(.status.conditions[]?; .type == "Programmed" and .status == "True" and .observedGeneration == $generation)
        ' <<< "$object" >/dev/null; then
            return 0
        fi
        sleep 1
    done
    die "Gateway ${gateway} did not become Programmed=True with an address"
}

prepare_gateway() {
    local gateway namespace name
    for gateway in "${GATEWAYS[@]}"; do
        namespace="${gateway%/*}"
        name="${gateway#*/}"
        kubectl create namespace "$namespace" --dry-run=client -o yaml | kubectl apply -f -
        kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: ${name}
  namespace: ${namespace}
  labels:
    app.kubernetes.io/managed-by: haptic-gateway-api-benchmark
spec:
  gatewayClassName: haptic
  listeners:
    - name: http
      protocol: HTTP
      port: 80
      allowedRoutes:
        namespaces:
          from: All
EOF
    done
    for gateway in "${GATEWAYS[@]}"; do
        wait_for_gateway "$gateway"
    done
    kubectl get gatewayclass,gateway -A -o yaml > "${BENCH_OUTPUT_DIR}/cluster/gateways.yaml"
    record_event gateway-ready
}

assert_isolated_workload() {
    [[ "$(total_route_count)" -eq 0 ]] || die "cluster contains HTTPRoutes before the benchmark"
    local actual expected
    actual="$(kubectl get gateways.gateway.networking.k8s.io -A -o json | jq -c '[.items[] | "\(.metadata.namespace)/\(.metadata.name)"] | sort')"
    expected="$(printf '%s\n' "${GATEWAYS[@]}" | jq -R . | jq -sc 'sort')"
    [[ "$actual" == "$expected" ]] || die "cluster Gateway inventory is not isolated: expected ${expected}, got ${actual}"
    printf '%s\n' "$actual" > "${BENCH_OUTPUT_DIR}/cluster/benchmark-gateways.json"
    printf '0\n' > "${BENCH_OUTPUT_DIR}/cluster/pre-benchmark-httproute-count.txt"
}

install_prometheus() {
    local manifest="${UPSTREAM_DIR}/install/prometheus.yaml"
    local config="${BENCH_OUTPUT_DIR}/cluster/prometheus-config.yaml"
    yq -er 'select(.kind == "ConfigMap" and .metadata.name == "prometheus") | .data."prometheus.yml"' \
        "$manifest" > "$config"
    yq -e '
        .global.scrape_interval == "5s" and
        ([.scrape_configs[] | select(.job_name == "kubernetes-nodes-cadvisor")] | length) == 1 and
        ([.scrape_configs[] | select(.job_name == "kubernetes-nodes-cadvisor")][0].scrape_interval // "5s") == "5s" and
        ([.scrape_configs[] | select(.job_name == "kubernetes-nodes-cadvisor")][0] as $job |
          (($job | has("honor_timestamps") | not) or $job.honor_timestamps == true))
    ' "$config" >/dev/null || die "pinned Prometheus cAdvisor scrape methodology is not 5s with exporter timestamps"
    kubectl create namespace monitoring --dry-run=client -o yaml | kubectl apply -f -
    kubectl apply -f "$manifest"
    kubectl rollout status deployment/prometheus -n monitoring --timeout=5m
    kubectl wait --for=condition=Ready pod -n monitoring \
        -l app.kubernetes.io/name=prometheus,app.kubernetes.io/component=server --timeout=5m
    kubectl get all -n monitoring -o yaml > "${BENCH_OUTPUT_DIR}/cluster/prometheus-resources.yaml"
    (cd "$UPSTREAM_DIR" && sha256sum install/prometheus.yaml) \
        > "${BENCH_OUTPUT_DIR}/cluster/prometheus-manifest-sha256.txt"
    wait_for_prometheus_samples
    record_event prometheus-ready
}

urlencode() {
    python3 - "$1" <<'PY'
import sys
import urllib.parse

print(urllib.parse.quote(sys.argv[1], safe=""))
PY
}

prometheus_query() {
    local query="$1"
    local output="$2"
    local require_samples="${3:-true}"
    local encoded
    encoded="$(urlencode "$query")"
    kubectl get --raw "/api/v1/namespaces/monitoring/services/http:prometheus:9090/proxy/api/v1/query?query=${encoded}" \
        > "$output"
    jq -e '.status == "success"' "$output" >/dev/null || die "Prometheus query failed: ${query}"
    if [[ "$require_samples" == "true" ]]; then
        jq -e '.data.result | length > 0' "$output" >/dev/null || die "Prometheus query returned no samples: ${query}"
    fi
}

prometheus_query_range() {
    local query="$1"
    local start="$2"
    local end="$3"
    local output="$4"
    local require_samples="${5:-true}"
    awk -v start="$start" -v end="$end" 'BEGIN { exit !(end > start) }' || \
        die "Prometheus capture window is not positive"
    local encoded
    encoded="$(urlencode "$query")"
    kubectl get --raw "/api/v1/namespaces/monitoring/services/http:prometheus:9090/proxy/api/v1/query_range?query=${encoded}&start=${start}&end=${end}&step=${PROMETHEUS_SCRAPE_INTERVAL_SECONDS}" \
        > "$output"
    jq -e '.status == "success" and .data.resultType == "matrix"' "$output" >/dev/null || \
        die "Prometheus range query failed: ${query}"
    if [[ "$require_samples" == "true" ]]; then
        jq -e '.data.result | length > 0 and any(.[]; (.values | length) > 0)' "$output" >/dev/null || \
            die "Prometheus range query returned no samples: ${query}"
    fi
}

wait_for_prometheus_samples() {
    local deadline=$((SECONDS + 180))
    local container_output="${WORK_DIR}/prometheus-container-ready.json"
    local pod_output="${WORK_DIR}/prometheus-pod-ready.json"
    local container_query='container_memory_working_set_bytes{namespace="haptic",container!="",container!="POD"}'
    local pod_query='container_memory_working_set_bytes{namespace="haptic",container="",pod!=""}'
    local container_encoded pod_encoded
    container_encoded="$(urlencode "$container_query")"
    pod_encoded="$(urlencode "$pod_query")"
    while (( SECONDS < deadline )); do
        if kubectl get --raw "/api/v1/namespaces/monitoring/services/http:prometheus:9090/proxy/api/v1/query?query=${container_encoded}" \
            > "$container_output" 2>/dev/null &&
            kubectl get --raw "/api/v1/namespaces/monitoring/services/http:prometheus:9090/proxy/api/v1/query?query=${pod_encoded}" \
                > "$pod_output" 2>/dev/null &&
            jq -e '.status == "success" and (.data.result | length > 0)' "$container_output" >/dev/null &&
            jq -e '.status == "success" and (.data.result | length > 0)' "$pod_output" >/dev/null; then
            return 0
        fi
        sleep 2
    done
    die "Prometheus did not scrape HAPTIC container resource samples"
}

extract_haptic_identities() {
    local pods_json="$1"
    local output="$2"
    jq -S '
        [.items[] |
          select(.metadata.namespace == "haptic") |
          select(.metadata.labels["app.kubernetes.io/instance"] == "haptic") |
          select(.metadata.labels["app.kubernetes.io/component"] == "controller" or
                 .metadata.labels["app.kubernetes.io/component"] == "loadbalancer") |
          [.spec.initContainers[]? | select(.restartPolicy == "Always") | .name] as $sidecars |
          {namespace: .metadata.namespace,
           name: .metadata.name,
           uid: .metadata.uid,
           component: .metadata.labels["app.kubernetes.io/component"],
           containers: [((.status.initContainerStatuses[]? | select(.name | IN($sidecars[]))),
                         .status.containerStatuses[]?) |
             {name, image, imageID, containerID, restartCount, ready}] | sort_by(.name)}] |
        sort_by(.namespace, .name)
    ' "$pods_json" > "$output"
    jq -e '
        any(.[]; .component == "controller") and
        any(.[]; .component == "loadbalancer") and
        all(.[]; (.containers | length) > 0 and
                 all(.containers[]; (.imageID // "") != "" and (.containerID // "") != "" and
                                    .ready == true and .restartCount == 0))
    ' \
        "$output" >/dev/null || die "HAPTIC pod identity snapshot is incomplete"
}

agent_port() {
    local pods_json="$1" pod="$2" port_name="$3"
    jq -er --arg pod "$pod" --arg port_name "$port_name" '
        [.items[] | select(.metadata.name == $pod) | (.spec.initContainers[]?, .spec.containers[]) |
         select(.name == "agent") | .ports[]? |
         select(.name == $port_name and .protocol == "TCP") | .containerPort] |
        if length == 1 and (.[0] | type == "number" and . == floor and . > 0 and . <= 65535)
        then .[0] else error("missing or ambiguous agent port") end
    ' "$pods_json"
}

capture_quiescent_agent() {
    local pods_json="$1" pod="$2" output="$3" port deadline=$((SECONDS + 10))
    port="$(agent_port "$pods_json" "$pod" dataplane)" || die "invalid agent API port for ${pod}"
    while true; do
        kubectl exec -n haptic "$pod" -c agent -- /usr/local/bin/haptic agent state \
            --listen=":${port}" --output=json | jq -e '
                .pending_deletes | select(type == "object" and has("servers") and has("backends")) |
                select(all(.servers, .backends; . == null or (type == "array" and all(.[]; type == "string")))) |
                {servers: (.servers // []), backends: (.backends // [])}
            ' > "$output" || die "could not capture pending deletes for ${pod}"
        if jq -e '.servers == [] and .backends == []' "$output" >/dev/null; then
            return 0
        fi
        (( SECONDS < deadline )) || die "agent ${pod} still has pending deletes after baseline convergence"
        sleep 0.1
    done
}

capture_state() {
    local output_dir="$1"
    mkdir -p "$output_dir/cadvisor" "$output_dir/controller-metrics" "$output_dir/agent-metrics" \
        "$output_dir/agent-pending" "$output_dir/prometheus"
    date -u +%Y-%m-%dT%H:%M:%S.%NZ > "$output_dir/timestamp.txt"
    date +%s > "$output_dir/epoch.txt"
    kubectl get pods -n "$RELEASE_NAMESPACE" -l app.kubernetes.io/instance=haptic -o json \
        > "$output_dir/pods.json"
    extract_haptic_identities "$output_dir/pods.json" "$output_dir/haptic-identities.json"
    kubectl get deployments,statefulsets -n "$RELEASE_NAMESPACE" -o json > "$output_dir/workloads.json"
    kubectl get haproxycfg -n "$RELEASE_NAMESPACE" -o yaml > "$output_dir/haproxycfg.yaml"

    local pod node safe_node metrics_port
    local -a controller_pods agent_pods nodes
    mapfile -t controller_pods < <(kubectl get pods -n haptic \
        -l app.kubernetes.io/instance=haptic,app.kubernetes.io/component=controller \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
    [[ ${#controller_pods[@]} -gt 0 ]] || die "no controller pods found for metrics capture"
    for pod in "${controller_pods[@]}"; do
        kubectl get --raw "/api/v1/namespaces/haptic/pods/${pod}:9090/proxy/metrics" \
            > "$output_dir/controller-metrics/${pod}.prom"
        [[ -s "$output_dir/controller-metrics/${pod}.prom" ]] || die "empty controller metrics for ${pod}"
    done

    mapfile -t agent_pods < <(jq -r '.[] | select(.component == "loadbalancer") | .name' \
        "$output_dir/haptic-identities.json")
    [[ ${#agent_pods[@]} -gt 0 ]] || die "no agent pods found for metrics capture"
    for pod in "${agent_pods[@]}"; do
        capture_quiescent_agent "$output_dir/pods.json" "$pod" "$output_dir/agent-pending/${pod}.json"
        metrics_port="$(agent_port "$output_dir/pods.json" "$pod" agent-metrics)" || die "invalid agent metrics port for ${pod}"
        kubectl get --raw "/api/v1/namespaces/haptic/pods/${pod}:${metrics_port}/proxy/metrics" \
            > "$output_dir/agent-metrics/${pod}.prom"
        [[ -s "$output_dir/agent-metrics/${pod}.prom" ]] || die "empty agent metrics for ${pod}"
    done

    mapfile -t nodes < <(kubectl get nodes -o json | jq -r '.items[] |
        select(.metadata.labels["pilot-load.istio.io/node"] != "fake") | .metadata.name')
    [[ ${#nodes[@]} -gt 0 ]] || die "no nodes found for cAdvisor capture"
    for node in "${nodes[@]}"; do
        safe_node="${node//\//_}"
        kubectl get --raw "/api/v1/nodes/${node}/proxy/metrics/cadvisor" \
            > "$output_dir/cadvisor/${safe_node}.prom"
        [[ -s "$output_dir/cadvisor/${safe_node}.prom" ]] || die "empty cAdvisor metrics for ${node}"
    done

    prometheus_query 'up' "$output_dir/prometheus/up.json"
    prometheus_query 'container_cpu_usage_seconds_total{namespace="haptic",container!="",container!="POD",cpu=~"^(total)?$"}' \
        "$output_dir/prometheus/container-cpu.json"
    prometheus_query 'container_memory_working_set_bytes{namespace="haptic",container!="",container!="POD"}' \
        "$output_dir/prometheus/container-working-set.json"
    prometheus_query 'container_memory_rss{namespace="haptic",container!="",container!="POD"}' \
        "$output_dir/prometheus/container-rss.json"
    prometheus_query 'container_cpu_usage_seconds_total{namespace="haptic",container="",pod!="",cpu=~"^(total)?$"}' \
        "$output_dir/prometheus/pod-cgroup-cpu-counter.json"
    prometheus_query 'container_memory_working_set_bytes{namespace="haptic",container="",pod!=""}' \
        "$output_dir/prometheus/pod-cgroup-working-set.json"
}

snapshot_controller_counter() {
    local metric="$1"
    local output_dir="$2"
    mkdir -p "$output_dir"
    local pod value sum=0
    local -a pods
    mapfile -t pods < <(kubectl get pods -n haptic \
        -l app.kubernetes.io/instance=haptic,app.kubernetes.io/component=controller \
        -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
    [[ ${#pods[@]} -gt 0 ]] || die "no controller pods found for ${metric} snapshot"
    for pod in "${pods[@]}"; do
        kubectl get --raw "/api/v1/namespaces/haptic/pods/${pod}:9090/proxy/metrics" \
            > "$output_dir/${pod}.prom"
        value="$(awk -v metric="$metric" '$1 == metric { print $2; found++ } END { if (found != 1) exit 1 }' \
            "$output_dir/${pod}.prom")" || die "metric ${metric} missing or duplicated on ${pod}"
        sum="$(awk -v sum="$sum" -v value="$value" 'BEGIN { printf "%.17g", sum + value }')"
    done
    printf '%s\n' "$sum"
}

attach_lifecycle_outcome_quality() {
    local scenario_dir="$1"
    python3 "${PROJECT_ROOT}/scripts/analyze-gateway-api-outcomes.py" \
        --scenario-dir "$scenario_dir" --output "$scenario_dir/lifecycle-outcomes.json" || \
        die "scenario lifecycle outcome evidence is invalid"
    jq --slurpfile outcomes "$scenario_dir/lifecycle-outcomes.json" '
        .lifecycle_outcome_quality = $outcomes[0] |
        .haptic_scenario_quality.lifecycle_outcome_quality = $outcomes[0] |
        .haptic_scenario_quality.pass = (.haptic_scenario_quality.pass and $outcomes[0].pass) |
        .pass = (.pass and .haptic_scenario_quality.pass)
    ' "$scenario_dir/analysis.json" > "$scenario_dir/analysis.json.tmp"
    mv "$scenario_dir/analysis.json.tmp" "$scenario_dir/analysis.json"
}

capture_scale_activity_snapshot() {
    local scenario_dir="$1"
    local phase="$2"
    local expected_routes="$3"
    local output_dir="${scenario_dir}/steady-activity-${phase}"
    [[ "$phase" == "start" || "$phase" == "end" ]] || die "invalid scale activity snapshot phase: ${phase}"
    mkdir -p "$output_dir/raw"
    date -u +%Y-%m-%dT%H:%M:%S.%NZ > "$output_dir/capture-started-at.txt"
    date +%s.%N > "$output_dir/capture-started-epoch.txt"
    if [[ "$phase" == "end" ]]; then
        wc -l < "$scenario_dir/upstream.log" > "$output_dir/upstream-log-line-boundary.txt"
    fi
    kubectl get httproutes.gateway.networking.k8s.io -A -o json > "$output_dir/httproutes.json"
    jq -S --argjson expected "$expected_routes" '
        {schema_version: 1, expected_routes: $expected,
         routes: [.items[] |
           {namespace: .metadata.namespace, name: .metadata.name,
            uid: .metadata.uid, generation: .metadata.generation,
            path_prefix: .spec.rules[0].matches[0].path.value}] | sort_by(.namespace, .name)}
    ' "$output_dir/httproutes.json" > "$output_dir/route-inventory.json"
    jq -e '
        (.routes | length) == .expected_routes and
        ([.routes[].uid] | unique | length) == .expected_routes and
        ([.routes[] | .namespace + "/" + .name] | unique | length) == .expected_routes and
        all(.routes[];
          (.namespace | type) == "string" and (.namespace | length) > 0 and
          (.name | type) == "string" and (.name | length) > 0 and
          (.uid | type) == "string" and (.uid | length) > 0 and
          (.generation | type) == "number" and .generation > 0 and
          (.path_prefix | type) == "string" and
          (.path_prefix | test("^/[1-9][0-9]{0,4}$")) and
          ((.path_prefix[1:] | tonumber) <= 10000))
    ' "$output_dir/route-inventory.json" >/dev/null || \
        die "scale activity route inventory is not the exact pinned PathPrefix workload"
    jq -e '
        all(.items[];
          (.spec.rules | length) == 1 and
          (.spec.rules[0].matches | length) == 1 and
          .spec.rules[0].matches[0].path.type == "PathPrefix")
    ' "$output_dir/httproutes.json" >/dev/null || \
        die "scale activity routes no longer match the pinned HTTPRoute template"
    kubectl get pods -n "$RELEASE_NAMESPACE" \
        -l app.kubernetes.io/instance=haptic,app.kubernetes.io/component=controller \
        -o json > "$output_dir/pods.json"
    jq -S '
        [.items[] |
          {name: .metadata.name, uid: .metadata.uid, node: .spec.nodeName,
           deletion_timestamp: .metadata.deletionTimestamp,
           ready: any(.status.conditions[]?; .type == "Ready" and .status == "True"),
           containers: [.status.containerStatuses[]? |
             {name, image, imageID, containerID, restartCount, ready}] | sort_by(.name)}] |
        sort_by(.name)
    ' "$output_dir/pods.json" > "$output_dir/controller-identities.json"
    jq -e '
        length > 0 and
        all(.[]; .deletion_timestamp == null and .ready == true and
                 (.uid | type) == "string" and (.uid | length) > 0 and
                 (.containers | length) > 0 and
                 all(.containers[]; .ready == true and .restartCount == 0 and
                                      (.imageID | type) == "string" and (.imageID | length) > 0 and
                                      (.containerID | type) == "string" and (.containerID | length) > 0))
    ' "$output_dir/controller-identities.json" >/dev/null || \
        die "controller fleet is not Ready, restart-free, and identity-complete for scale activity capture"

    local pod
    while read -r pod; do
        kubectl get --raw "/api/v1/namespaces/${RELEASE_NAMESPACE}/pods/${pod}:9090/proxy/metrics" \
            > "$output_dir/raw/${pod}.prom"
        [[ -s "$output_dir/raw/${pod}.prom" ]] || die "empty controller metrics for ${pod}"
    done < <(jq -r '.[].name' "$output_dir/controller-identities.json")
    date -u +%Y-%m-%dT%H:%M:%S.%NZ > "$output_dir/capture-finished-at.txt"
    date +%s.%N > "$output_dir/capture-finished-epoch.txt"

    python3 - "$output_dir" "${SCALE_ACTIVITY_METRICS[@]}" <<'PY'
import json
import math
import re
import sys
from pathlib import Path

output = Path(sys.argv[1])
expected = sys.argv[2:]
with (output / "controller-identities.json").open(encoding="utf-8") as handle:
    identities = json.load(handle)

sample = re.compile(r"^([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{[^}]*\})?\s+(\S+)(?:\s+\S+)?$")
# Label vectors: summed over their series, and absent until the first event.
optional_vector = {
    "haptic_runtime_map_divergence_total",
    "haptic_deploy_apply_total",
    "haptic_apply_rejected_total",
    "haptic_runtime_server_ops_total",
    "haptic_runtime_backend_ops_total",
}
per_metric = {metric: [] for metric in expected}
for identity in identities:
    pod = identity["name"]
    values = {metric: [] for metric in expected}
    with (output / "raw" / f"{pod}.prom").open(encoding="utf-8") as handle:
        for raw_line in handle:
            if raw_line.startswith("#"):
                continue
            match = sample.match(raw_line.strip())
            if match is None or match.group(1) not in values:
                continue
            value = float(match.group(2))
            if not math.isfinite(value) or value < 0:
                raise SystemExit(f"invalid counter {match.group(1)} on {pod}")
            values[match.group(1)].append(value)
    for metric in expected:
        observed = values[metric]
        if metric in optional_vector:
            value = sum(observed)
        elif len(observed) == 1:
            value = observed[0]
        else:
            raise SystemExit(f"counter {metric} has {len(observed)} samples on {pod}, expected one")
        per_metric[metric].append({"pod": pod, "value": value})

result = {
    "schema_version": 1,
    "capture_started_at": (output / "capture-started-at.txt").read_text().strip(),
    "capture_started_epoch": (output / "capture-started-epoch.txt").read_text().strip(),
    "capture_finished_at": (output / "capture-finished-at.txt").read_text().strip(),
    "capture_finished_epoch": (output / "capture-finished-epoch.txt").read_text().strip(),
    "controller_identities": identities,
    "counters": {
        metric: {
            "fleet_sum": sum(item["value"] for item in rows),
            "per_pod": rows,
        }
        for metric, rows in per_metric.items()
    },
}
with (output / "counters.json").open("w", encoding="utf-8") as handle:
    json.dump(result, handle, indent=2, sort_keys=True)
    handle.write("\n")
PY
    if [[ "$phase" == "start" ]]; then
        wc -l < "$scenario_dir/upstream.log" > "$output_dir/upstream-log-line-boundary.txt"
    fi
}

analyze_scale_activity() {
    local scenario_dir="$1"
    local start_epoch="$2"
    local end_epoch="$3"
    local expected_routes="$4"
    python3 - "$scenario_dir/steady-activity-start/counters.json" \
        "$scenario_dir/steady-activity-end/counters.json" \
        "$scenario_dir/steady-activity-start/route-inventory.json" \
        "$scenario_dir/steady-activity-end/route-inventory.json" \
        "$scenario_dir/steady-activity-start/upstream-log-line-boundary.txt" \
        "$scenario_dir/steady-activity-end/upstream-log-line-boundary.txt" \
        "$scenario_dir/upstream.log" "$expected_routes" \
        "$start_epoch" "$end_epoch" "$scenario_dir/steady-activity.json" \
        "${SCALE_ACTIVITY_METRICS[@]}" <<'PY'
from decimal import Decimal
import json
from pathlib import Path
import re
import sys

(
    before_path,
    after_path,
    before_routes_path,
    after_routes_path,
    start_boundary_path,
    end_boundary_path,
    upstream_log_path,
    expected_routes_raw,
    window_start_raw,
    window_end_raw,
    output_path,
    *expected,
) = sys.argv[1:]
with open(before_path, encoding="utf-8") as handle:
    before = json.load(handle)
with open(after_path, encoding="utf-8") as handle:
    after = json.load(handle)
with open(before_routes_path, encoding="utf-8") as handle:
    before_routes = json.load(handle)
with open(after_routes_path, encoding="utf-8") as handle:
    after_routes = json.load(handle)

window_start = Decimal(window_start_raw)
window_end = Decimal(window_end_raw)
expected_routes = int(expected_routes_raw)
if window_end <= window_start:
    raise SystemExit("scale steady activity window is not positive")
if Decimal(before["capture_finished_epoch"]) > window_start:
    raise SystemExit("scale activity start snapshot crossed the steady resource boundary")
if Decimal(after["capture_started_epoch"]) < window_end:
    raise SystemExit("scale activity end snapshot preceded the steady resource boundary")
if before["controller_identities"] != after["controller_identities"]:
    raise SystemExit("controller identity changed across the scale steady interval")
if sorted(before["counters"]) != sorted(expected) or sorted(after["counters"]) != sorted(expected):
    raise SystemExit("scale activity counter inventory is incomplete")

if before_routes.get("expected_routes") != expected_routes or after_routes.get("expected_routes") != expected_routes:
    raise SystemExit("scale activity route inventory expectation changed")
before_by_uid = {item["uid"]: item for item in before_routes.get("routes", [])}
after_by_uid = {item["uid"]: item for item in after_routes.get("routes", [])}
if len(before_by_uid) != expected_routes or len(after_by_uid) != expected_routes:
    raise SystemExit("scale activity route inventory does not contain the expected unique UIDs")
if before_by_uid.keys() != after_by_uid.keys():
    raise SystemExit("HTTPRoute UID set changed across the scale steady interval")

changed_routes = []
generation_only_advances = 0
for uid in sorted(before_by_uid):
    old = before_by_uid[uid]
    new = after_by_uid[uid]
    if (old["namespace"], old["name"]) != (new["namespace"], new["name"]):
        raise SystemExit(f"HTTPRoute identity changed for UID {uid}")
    if new["generation"] < old["generation"]:
        raise SystemExit(f"HTTPRoute generation regressed for {old['namespace']}/{old['name']}")
    path_changed = new["path_prefix"] != old["path_prefix"]
    generation_advanced = new["generation"] > old["generation"]
    if path_changed and not generation_advanced:
        raise SystemExit(f"HTTPRoute path changed without a generation advance for {old['namespace']}/{old['name']}")
    if generation_advanced and path_changed:
        changed_routes.append({
            "namespace": old["namespace"],
            "name": old["name"],
            "uid": uid,
            "generation_before": old["generation"],
            "generation_after": new["generation"],
            "path_prefix_before": old["path_prefix"],
            "path_prefix_after": new["path_prefix"],
        })
    elif generation_advanced:
        generation_only_advances += 1

start_boundary = int(Path(start_boundary_path).read_text(encoding="utf-8").strip())
end_boundary = int(Path(end_boundary_path).read_text(encoding="utf-8").strip())
if start_boundary < 0 or end_boundary < start_boundary:
    raise SystemExit("pilot-load log line boundaries are invalid")
upstream_lines = Path(upstream_log_path).read_text(encoding="utf-8").splitlines()
if len(upstream_lines) < end_boundary:
    raise SystemExit("pilot-load log is shorter than the captured end boundary")
log_window = upstream_lines[start_boundary:end_boundary]
refresh_pattern = re.compile(
    r"refreshed config HTTPRoute/([a-z0-9](?:[-a-z0-9.]*[a-z0-9])?)/"
    r"([a-z0-9](?:[-a-z0-9.]*[a-z0-9])?) \(\*config\.Templated\)$"
)
known_routes = {(item["namespace"], item["name"]) for item in before_by_uid.values()}
refresh_lines = []
refresh_route_keys = set()
for line_number, line in enumerate(log_window, start=start_boundary + 1):
    match = refresh_pattern.search(line)
    if match is None:
        continue
    route_key = (match.group(1), match.group(2))
    if route_key not in known_routes:
        raise SystemExit(f"pilot-load logged a refresh for an unknown HTTPRoute at line {line_number}")
    refresh_route_keys.add(route_key)
    refresh_lines.append({"line_number": line_number, "line": line})

output_parent = Path(output_path).parent
with (output_parent / "steady-activity-upstream-log-window.txt").open("w", encoding="utf-8") as handle:
    for line in log_window:
        handle.write(line + "\n")
with (output_parent / "steady-activity-refresh-lines.txt").open("w", encoding="utf-8") as handle:
    for item in refresh_lines:
        handle.write(f"{item['line_number']}:{item['line']}\n")

metrics = []
monotonic = True
for metric in expected:
    before_rows = {item["pod"]: Decimal(str(item["value"])) for item in before["counters"][metric]["per_pod"]}
    after_rows = {item["pod"]: Decimal(str(item["value"])) for item in after["counters"][metric]["per_pod"]}
    if before_rows.keys() != after_rows.keys() or len(before_rows) != len(before["controller_identities"]):
        raise SystemExit(f"counter {metric} does not cover the exact controller fleet")
    per_pod = []
    for pod in sorted(before_rows):
        delta = after_rows[pod] - before_rows[pod]
        monotonic = monotonic and delta >= 0
        per_pod.append({
            "pod": pod,
            "before": float(before_rows[pod]),
            "after": float(after_rows[pod]),
            "delta": float(delta),
        })
    before_sum = sum(before_rows.values(), Decimal(0))
    after_sum = sum(after_rows.values(), Decimal(0))
    metrics.append({
        "metric": metric,
        "before": float(before_sum),
        "after": float(after_sum),
        "delta": float(after_sum - before_sum),
        "per_pod": per_pod,
    })

by_name = {item["metric"]: item for item in metrics}
reconciliation_advanced = by_name["haptic_reconciliation_total"]["delta"] > 0
dataplane_metrics = (
    "haptic_deployment_total",
    "haptic_deploy_apply_total",
    "haptic_haproxy_reloads_total",
)
dataplane_advanced = any(by_name[name]["delta"] > 0 for name in dataplane_metrics)
outcome_metrics = (
    "haptic_reconciliation_errors_total",
    "haptic_deployment_errors_total",
    "haptic_validation_errors_total",
    "haptic_apply_rejected_total",
    "haptic_runtime_map_divergence_total",
    "haptic_events_dropped_critical_total",
)
outcome_deltas = {name: by_name[name]["delta"] for name in outcome_metrics}
route_refresh_proven = bool(changed_routes) and bool(refresh_lines)
result = {
    "schema_version": 1,
    "window": {
        "start_epoch": window_start_raw,
        "end_epoch": window_end_raw,
        "sample_boundary": "start snapshot completed before the resource window; end snapshot began after it while pilot-load remained active",
    },
    "captures": {
        "start": {
            "started_at": before["capture_started_at"],
            "started_epoch": before["capture_started_epoch"],
            "finished_at": before["capture_finished_at"],
            "finished_epoch": before["capture_finished_epoch"],
        },
        "end": {
            "started_at": after["capture_started_at"],
            "started_epoch": after["capture_started_epoch"],
            "finished_at": after["capture_finished_at"],
            "finished_epoch": after["capture_finished_epoch"],
        },
    },
    "controller_identities_unchanged": True,
    "route_refresh_activity": {
        "pass": route_refresh_proven,
        "expected_routes": expected_routes,
        "uid_set_unchanged": True,
        "generations_monotonic": True,
        "generation_and_path_advanced_routes": len(changed_routes),
        "generation_only_advanced_routes": generation_only_advances,
        "changed_routes": changed_routes,
        "pilot_load_log": {
            "start_line_exclusive": start_boundary,
            "end_line_inclusive": end_boundary,
            "window_line_count": end_boundary - start_boundary,
            "pinned_refresh_line_count": len(refresh_lines),
            "refreshed_route_count": len(refresh_route_keys),
            "refresh_lines": refresh_lines,
        },
        "evidence_scope": {
            "temporal_overlap": True,
            "causal_mapping": False,
            "statement": "route generation/path advances and pinned pilot-load refresh lines overlap the captured activity interval; individual mutations are not causally mapped",
        },
    },
    "all_counters_monotonic_per_pod": monotonic,
    "reconciliation_advanced": reconciliation_advanced,
    "dataplane_path_advanced": dataplane_advanced,
    "dataplane_activity_metrics": list(dataplane_metrics),
    "metrics": metrics,
    "outcome_deltas": outcome_deltas,
    "outcome_quality": {
        "pass": all(delta == 0 for delta in outcome_deltas.values()),
        "requirement": "all adverse counter deltas are zero",
    },
    "pass": monotonic and reconciliation_advanced and dataplane_advanced and route_refresh_proven,
}
with open(output_path, "w", encoding="utf-8") as handle:
    json.dump(result, handle, indent=2, sort_keys=True)
    handle.write("\n")
if not result["pass"]:
    raise SystemExit("scale steady interval did not prove overlapping HTTPRoute refresh and HAPTIC dataplane work")
PY
}

capture_prometheus_range() {
    local output_dir="$1"
    local start="$2"
    local end="$3"
    local range_name="${4:-prometheus-range}"
    local identities="${5:-$output_dir/before/haptic-identities.json}"
    local require_samples="${6:-true}"
    local range_dir="$output_dir/$range_name"
    mkdir -p "$range_dir"
    [[ -s "$identities" ]] || die "identity snapshot is missing for Prometheus range capture"
    local cpu_query working_set_query rss_query pod_cpu_query pod_working_set_query pod_regex
    cpu_query="$(real_container_query 'container_cpu_usage_seconds_total' "$identities")"
    working_set_query="$(real_container_query 'container_memory_working_set_bytes' "$identities")"
    rss_query="$(real_container_query 'container_memory_rss' "$identities")"
    pod_regex="$(jq -r '[.[].name | gsub("\\."; "\\.")] | join("|")' "$identities")"
    [[ -n "$pod_regex" ]] || die "identity snapshot contains no pods for Prometheus capture"
    pod_cpu_query="container_cpu_usage_seconds_total{namespace=\"haptic\",container=\"\",image=\"\",name=\"\",pod=~\"^(${pod_regex})$\",cpu=~\"^(total)?$\"}"
    pod_working_set_query="container_memory_working_set_bytes{namespace=\"haptic\",container=\"\",image=\"\",name=\"\",pod=~\"^(${pod_regex})$\"}"
    jq -n \
        --arg cpu "$cpu_query" \
        --arg working_set "$working_set_query" \
        --arg rss "$rss_query" \
        --arg pod_cpu "$pod_cpu_query" \
        --arg pod_working_set "$pod_working_set_query" \
        --arg start "$start" \
        --arg end "$end" \
        --argjson evaluation_step "$PROMETHEUS_SCRAPE_INTERVAL_SECONDS" \
        --argjson maximum_source_age "$((PROMETHEUS_SCRAPE_INTERVAL_SECONDS * 4))" \
        '{haptic_container_cpu: $cpu, haptic_container_working_set: $working_set,
          haptic_container_rss: $rss, upstream_compatible_pod_cgroup_cpu: $pod_cpu,
          upstream_compatible_pod_cgroup_working_set: $pod_working_set,
          source_timestamp_queries: {
            haptic_container_cpu: ("timestamp(" + $cpu + ")"),
            haptic_container_working_set: ("timestamp(" + $working_set + ")"),
            haptic_container_rss: ("timestamp(" + $rss + ")"),
            upstream_compatible_pod_cgroup_cpu: ("timestamp(" + $pod_cpu + ")"),
            upstream_compatible_pod_cgroup_working_set: ("timestamp(" + $pod_working_set + ")")
          },
          capture: {api: "paired query_range values and timestamp(selector)",
                    evaluation_step_seconds: $evaluation_step,
                    source_sample_bounds: "retained source timestamps in (start,end]; leading lookback evaluations discarded",
                    maximum_source_age_seconds: $maximum_source_age,
                    start_epoch: $start, end_epoch: $end}}' \
        > "$range_dir/queries.json"
    printf '%s\n' "$start" > "$range_dir/start-epoch.txt"
    printf '%s\n' "$end" > "$range_dir/end-epoch.txt"
    prometheus_query_range "$cpu_query" "$start" "$end" "$range_dir/cpu.json" "$require_samples"
    prometheus_query_range "timestamp(${cpu_query})" "$start" "$end" \
        "$range_dir/cpu-source-timestamps.json" "$require_samples"
    prometheus_query_range "$working_set_query" "$start" "$end" \
        "$range_dir/working-set.json" "$require_samples"
    prometheus_query_range "timestamp(${working_set_query})" "$start" "$end" \
        "$range_dir/working-set-source-timestamps.json" "$require_samples"
    prometheus_query_range "$rss_query" "$start" "$end" "$range_dir/rss.json" "$require_samples"
    prometheus_query_range "timestamp(${rss_query})" "$start" "$end" \
        "$range_dir/rss-source-timestamps.json" "$require_samples"
    prometheus_query_range "$pod_cpu_query" "$start" "$end" \
        "$range_dir/pod-cgroup-cpu.json" "$require_samples"
    prometheus_query_range "timestamp(${pod_cpu_query})" "$start" "$end" \
        "$range_dir/pod-cgroup-cpu-source-timestamps.json" "$require_samples"
    prometheus_query_range "$pod_working_set_query" "$start" "$end" \
        "$range_dir/pod-cgroup-working-set.json" "$require_samples"
    prometheus_query_range "timestamp(${pod_working_set_query})" "$start" "$end" \
        "$range_dir/pod-cgroup-working-set-source-timestamps.json" "$require_samples"
}

real_container_query() {
    local metric="$1"
    local identities="$2"
    local pod_regex container_regex
    pod_regex="$(jq -r '[.[].name | gsub("\\."; "\\.")] | unique | join("|")' "$identities")"
    container_regex="$(jq -r '[.[].containers[].name | gsub("\\."; "\\.")] | unique | join("|")' "$identities")"
    [[ -n "$pod_regex" && -n "$container_regex" ]] || return 1
    local cpu_filter=""
    if [[ "$metric" == "container_cpu_usage_seconds_total" ]]; then
        cpu_filter=',cpu=~"^(total)?$"'
    fi
    printf '%s{namespace="haptic",pod=~"^(%s)$",container=~"^(%s)$"%s}' \
        "$metric" "$pod_regex" "$container_regex" "$cpu_filter"
}

verify_identity_unchanged() {
    local before="$1"
    local after="$2"
    local evidence="$3"
    if ! cmp -s "$before" "$after"; then
        diff -u "$before" "$after" > "$evidence" || true
        die "HAPTIC pod/container identity or restart count changed; see ${evidence}"
    fi
    : > "$evidence"
}

capture_supervised_children() {
    local state_dir="$1"
    local topology="$state_dir/supervised-child-topology.json"
    local capture="$state_dir/supervised-children.json"
    local raw_dir="$state_dir/supervised-children-raw"
    local rows="$state_dir/supervised-children.jsonl"
    mkdir -p "$raw_dir"
    : > "$rows"
    python3 "${PROJECT_ROOT}/scripts/analyze-gateway-api-children.py" topology \
        --workloads "$state_dir/workloads.json" \
        --pods "$state_dir/pods.json" \
        --output "$topology" || die "supervised-child topology is invalid"

    local child_script
    read -r -d '' child_script <<'CHILD_CAPTURE' || true
set -u
parse_proc_stat_line() {
    local stat_line="$1"
    local tail
    local -a fields
    [[ "$stat_line" == *") "* ]] || return 1
    tail="${stat_line##*) }"
    read -r -a fields <<< "$tail"
    (( ${#fields[@]} >= 20 )) || return 1
    parsed_state="${fields[0]}"
    parsed_ppid="${fields[1]}"
    parsed_starttime="${fields[19]}"
    [[ ${#parsed_state} -eq 1 && "$parsed_ppid" =~ ^[0-9]+$ &&
        "$parsed_starttime" =~ ^[1-9][0-9]*$ ]]
}
if [[ "${1:-}" == --parse-stat ]]; then
    parsed_state= parsed_ppid= parsed_starttime=
    parse_proc_stat_line "$2" || exit 86
    printf '%s\n%s\n%s\n' "$parsed_state" "$parsed_ppid" "$parsed_starttime"
    exit 0
fi
expected="$1"
method="$2"
port="$3"
path="$4"
accepted="$5"
required_config_file="$6"
required_config_pattern="$7"
[[ "$expected" == /* && -e "$expected" && -x "$expected" ]] || exit 70
[[ -x /usr/bin/bash && -x /usr/bin/timeout ]] || exit 71
[[ "$port" =~ ^[1-9][0-9]{0,4}$ ]] && (( port <= 65535 )) || exit 72
[[ "$method" == GET || "$method" == HEAD ]] || exit 73
[[ "$path" == /* && "$path" != *$'\n'* && "$path" != *$'\r'* ]] || exit 74

snapshot() {
    local proc pid argv0 stat_line state ppid starttime inode
    local -a matches=()
    local first_pid= first_argv0= first_inode=false first_state= first_ppid= first_starttime=
    for proc in /proc/[0-9]*; do
        [[ -d "$proc" ]] || continue
        pid="${proc#/proc/}"
        argv0=
        if ! IFS= read -r -d '' argv0 < "$proc/cmdline" 2>/dev/null; then
            [[ ! -e "$proc" || ! -s "$proc/cmdline" ]] && continue
            exit 75
        fi
        [[ "$argv0" == "$expected" ]] || continue
        [[ -e "$proc/exe" ]] || exit 76
        inode=false
        [[ "$proc/exe" -ef "$expected" ]] && inode=true
        IFS= read -r stat_line < "$proc/stat" 2>/dev/null || exit 77
        parsed_state= parsed_ppid= parsed_starttime=
        parse_proc_stat_line "$stat_line" || exit 78
        state="$parsed_state"
        ppid="$parsed_ppid"
        starttime="$parsed_starttime"
        matches+=("$pid")
        if (( ${#matches[@]} == 1 )); then
            first_pid="$pid"
            first_argv0="$argv0"
            first_inode="$inode"
            first_state="$state"
            first_ppid="$ppid"
            first_starttime="$starttime"
        fi
    done
    local joined=
    if (( ${#matches[@]} > 0 )); then
        local IFS=,
        joined="${matches[*]}"
    fi
    printf '%s|%s|%s|%s|%s|%s|%s|%s\n' \
        "${#matches[@]}" "$joined" "$first_pid" "$first_argv0" "$first_inode" \
        "$first_state" "$first_ppid" "$first_starttime"
}

boot_id=
IFS= read -r boot_id < /proc/sys/kernel/random/boot_id || exit 81
before="$(snapshot)" || exit $?
config_verified=true
if [[ -n "$required_config_file" ]]; then
    config_verified=false
    if [[ -r "$required_config_file" ]]; then
        while IFS= read -r line || [[ -n "$line" ]]; do
            if [[ "$line" =~ $required_config_pattern ]]; then
                config_verified=true
                break
            fi
        done < "$required_config_file"
    fi
fi

probe_rc=90
status_code=
status_line=
health_pass=false
if [[ "$config_verified" == true ]]; then
    health_output="$(/usr/bin/timeout 5s /usr/bin/bash -c '
        port="$1"; method="$2"; path="$3"
        exec 3<>"/dev/tcp/127.0.0.1/${port}" || exit 10
        printf "%s %s HTTP/1.0\r\nHost: localhost\r\nConnection: close\r\n\r\n" "$method" "$path" >&3
        status=
        IFS= read -r status <&3 || exit 11
        status="${status%$'"'"'\r'"'"'}"
        [[ "$status" =~ ^HTTP/[0-9]+\.[0-9]+[[:space:]]([0-9]{3})([[:space:]][[:print:]]*)?$ ]] || exit 12
        printf "%s\n%s\n" "${BASH_REMATCH[1]}" "$status"
    ' child-health "$port" "$method" "$path" 2>/dev/null)"
    probe_rc=$?
    if (( probe_rc == 0 )); then
        mapfile -t health_lines <<< "$health_output"
        (( ${#health_lines[@]} == 2 )) || exit 82
        status_code="${health_lines[0]}"
        status_line="${health_lines[1]}"
        [[ "$status_code" =~ ^[1-5][0-9]{2}$ ]] || exit 83
        if [[ "$accepted" == any-http || ",${accepted}," == *",${status_code},"* ]]; then
            health_pass=true
        fi
    fi
fi
after="$(snapshot)" || exit $?
boot_id_after=
IFS= read -r boot_id_after < /proc/sys/kernel/random/boot_id || exit 84
[[ "$boot_id" == "$boot_id_after" ]] || exit 85
IFS='|' read -r before_count before_pids before_pid before_argv0 before_inode before_state before_ppid before_starttime <<< "$before"
IFS='|' read -r after_count after_pids after_pid after_argv0 after_inode after_state after_ppid after_starttime <<< "$after"
printf '%s\n' \
    "$boot_id" \
    "$before_count" "$before_pids" "$before_pid" "$before_argv0" "$before_inode" "$before_state" "$before_ppid" "$before_starttime" \
    "$config_verified" "$probe_rc" "$status_code" "$status_line" "$health_pass" \
    "$after_count" "$after_pids" "$after_pid" "$after_argv0" "$after_inode" "$after_state" "$after_ppid" "$after_starttime" \
    "$boot_id_after"
CHILD_CAPTURE

    local task namespace pod container expected method port path accepted required_file required_pattern
    local slug raw_stdout raw_stderr exec_rc before_pids after_pids child_json
    local -a fields
    while IFS= read -r task; do
        namespace="$(jq -er '.namespace' <<< "$task")"
        pod="$(jq -er '.pod' <<< "$task")"
        container="$(jq -er '.container' <<< "$task")"
        expected="$(jq -er '.expected_executable' <<< "$task")"
        method="$(jq -er '.health.method' <<< "$task")"
        port="$(jq -er '.health.port' <<< "$task")"
        path="$(jq -er '.health.path' <<< "$task")"
        accepted="$(jq -er '.health.accepted_status_codes | join(",")' <<< "$task")"
        required_file="$(jq -r '.health.required_config_file // ""' <<< "$task")"
        required_pattern="$(jq -r '.health.required_config_pattern // ""' <<< "$task")"
        slug="${pod}-${container}"
        raw_stdout="$raw_dir/${slug}.stdout"
        raw_stderr="$raw_dir/${slug}.stderr"
        exec_rc=0
        kubectl exec -n "$namespace" "$pod" -c "$container" -- \
            /usr/bin/bash -c "$child_script" child-capture \
            "$expected" "$method" "$port" "$path" "$accepted" \
            "$required_file" "$required_pattern" > "$raw_stdout" 2> "$raw_stderr" || exec_rc=$?
        printf '%d\n' "$exec_rc" > "$raw_dir/${slug}.exit-code.txt"
        [[ $exec_rc -eq 0 ]] || die "could not capture supervised child ${namespace}/${pod}/${container}"
        mapfile -t fields < "$raw_stdout"
        [[ ${#fields[@]} -eq 23 ]] || die "supervised child ${namespace}/${pod}/${container} returned malformed evidence"
        [[ "${fields[0]}" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]] || \
            die "supervised child ${namespace}/${pod}/${container} returned an invalid boot ID"
        [[ "${fields[1]}" =~ ^[0-9]+$ && "${fields[14]}" =~ ^[0-9]+$ ]] || \
            die "supervised child ${namespace}/${pod}/${container} returned an invalid process count"
        [[ "${fields[2]}" =~ ^([1-9][0-9]*)(,[1-9][0-9]*)*$|^$ && \
            "${fields[15]}" =~ ^([1-9][0-9]*)(,[1-9][0-9]*)*$|^$ ]] || \
            die "supervised child ${namespace}/${pod}/${container} returned an invalid PID inventory"
        [[ "${fields[5]}" == true || "${fields[5]}" == false ]] || die "invalid child executable verdict"
        [[ "${fields[9]}" == true || "${fields[9]}" == false ]] || die "invalid child config verdict"
        [[ "${fields[13]}" == true || "${fields[13]}" == false ]] || die "invalid child health verdict"
        [[ "${fields[18]}" == true || "${fields[18]}" == false ]] || die "invalid child executable verdict"
        [[ "${fields[22]}" == "${fields[0]}" ]] || die "supervised child boot ID changed during capture"
        before_pids="$(jq -cn --arg raw "${fields[2]}" '$raw | if . == "" then [] else split(",") end')"
        after_pids="$(jq -cn --arg raw "${fields[15]}" '$raw | if . == "" then [] else split(",") end')"
        child_json="$(jq -cn \
            --argjson task "$task" \
            --arg boot_id "${fields[0]}" \
            --argjson before_count "${fields[1]}" --argjson before_pids "$before_pids" \
            --arg before_pid "${fields[3]}" --arg before_argv0 "${fields[4]}" \
            --argjson before_inode "${fields[5]}" --arg before_state "${fields[6]}" \
            --arg before_ppid "${fields[7]}" --arg before_starttime "${fields[8]}" \
            --argjson config_verified "${fields[9]}" --argjson probe_rc "${fields[10]}" \
            --arg status_code "${fields[11]}" --arg status_line "${fields[12]}" \
            --argjson health_pass "${fields[13]}" \
            --argjson after_count "${fields[14]}" --argjson after_pids "$after_pids" \
            --arg after_pid "${fields[16]}" --arg after_argv0 "${fields[17]}" \
            --argjson after_inode "${fields[18]}" --arg after_state "${fields[19]}" \
            --arg after_ppid "${fields[20]}" --arg after_starttime "${fields[21]}" '
            def optional($value): if $value == "" then null else $value end;
            def identity($count; $pids; $pid; $argv0; $inode; $state; $ppid; $starttime):
              {process_count: $count, matching_pids: $pids, pid: optional($pid),
               argv0: optional($argv0), executable_inode_matches: $inode,
               state: optional($state), ppid: optional($ppid), starttime: optional($starttime)};
            $task + {
              boot_id: $boot_id,
              identity_before_health: identity($before_count; $before_pids; $before_pid;
                                               $before_argv0; $before_inode; $before_state;
                                               $before_ppid; $before_starttime),
              identity_after_health: identity($after_count; $after_pids; $after_pid;
                                              $after_argv0; $after_inode; $after_state;
                                              $after_ppid; $after_starttime),
              capture_stable: ($before_count == $after_count and $before_pids == $after_pids and
                               $before_pid == $after_pid and $before_argv0 == $after_argv0 and
                               $before_inode == $after_inode and $before_state == $after_state and
                               $before_ppid == $after_ppid and $before_starttime == $after_starttime),
              health: ($task.health + {
                required_config_verified: $config_verified,
                probe_exit_code: $probe_rc,
                http_status_code: optional($status_code),
                http_status_line: optional($status_line),
                pass: $health_pass
              })
            }')" || die "could not encode supervised child evidence"
        printf '%s\n' "$child_json" >> "$rows"
    done < <(jq -c '.tasks[]' "$topology")

    jq -s --slurpfile topology "$topology" \
        '{schema_version: 1, evidence_valid: true, topology: $topology[0], children: .}' \
        "$rows" > "$capture" || die "could not assemble supervised-child evidence"
}

validate_supervised_child_baseline() {
    local scenario_dir="$1"
    python3 "${PROJECT_ROOT}/scripts/analyze-gateway-api-children.py" baseline \
        --input "$scenario_dir/before/supervised-children.json" \
        --output "$scenario_dir/supervised-child-baseline.json" || \
        die "supervised-child baseline is invalid"
}

attach_supervised_child_continuity() {
    local scenario_dir="$1"
    python3 "${PROJECT_ROOT}/scripts/analyze-gateway-api-children.py" continuity \
        --before "$scenario_dir/before/supervised-children.json" \
        --after "$scenario_dir/after/supervised-children.json" \
        --output "$scenario_dir/supervised-child-continuity.json" || \
        die "supervised-child continuity evidence is invalid"
    jq --slurpfile continuity "$scenario_dir/supervised-child-continuity.json" '
        .supervised_child_continuity = $continuity[0] |
        .haptic_scenario_quality.supervised_child_continuity = $continuity[0] |
        .haptic_scenario_quality.pass = (.haptic_scenario_quality.pass and $continuity[0].pass) |
        .pass = .haptic_scenario_quality.pass
    ' "$scenario_dir/analysis.json" > "$scenario_dir/analysis.json.tmp"
    mv "$scenario_dir/analysis.json.tmp" "$scenario_dir/analysis.json"
}

capture_scenario_logs() {
    local scenario_dir="$1"
    local since_time
    since_time="$(<"$scenario_dir/before/timestamp.txt")"
    kubectl logs -n haptic \
        -l app.kubernetes.io/instance=haptic,app.kubernetes.io/component=controller \
        --all-containers=true --prefix=true --timestamps=true --tail=-1 --since-time="$since_time" \
        > "$scenario_dir/controller.log" || die "could not capture controller logs"
    kubectl logs -n haptic \
        -l app.kubernetes.io/instance=haptic,app.kubernetes.io/component=loadbalancer \
        --all-containers=true --prefix=true --timestamps=true --tail=-1 --since-time="$since_time" \
        > "$scenario_dir/loadbalancer-all-containers.log" || die "could not capture load-balancer logs"
}

analyze_resources() {
    local scenario_dir="$1"
    local range_dir="${2:-$scenario_dir/prometheus-range}"
    local allow_insufficient="${3:-false}"
    local -a analyzer_args=(
        python3 "${PROJECT_ROOT}/scripts/analyze-gateway-api-resources.py"
        --cpu "$range_dir/cpu.json"
        --working-set "$range_dir/working-set.json"
        --rss "$range_dir/rss.json"
        --pod-cgroup-cpu "$range_dir/pod-cgroup-cpu.json"
        --pod-cgroup-working-set "$range_dir/pod-cgroup-working-set.json"
        --cpu-source-timestamps "$range_dir/cpu-source-timestamps.json"
        --working-set-source-timestamps "$range_dir/working-set-source-timestamps.json"
        --rss-source-timestamps "$range_dir/rss-source-timestamps.json"
        --pod-cgroup-cpu-source-timestamps "$range_dir/pod-cgroup-cpu-source-timestamps.json"
        --pod-cgroup-working-set-source-timestamps "$range_dir/pod-cgroup-working-set-source-timestamps.json"
        --identities-before "$scenario_dir/before/haptic-identities.json"
        --identities-after "$scenario_dir/after/haptic-identities.json"
        --window-start "$(<"$range_dir/start-epoch.txt")"
        --window-end "$(<"$range_dir/end-epoch.txt")"
        --output "$scenario_dir/resources.json"
    )
    if [[ "$allow_insufficient" == "true" ]]; then
        analyzer_args+=(--allow-insufficient-samples)
    fi
    "${analyzer_args[@]}" || die "resource analysis failed for $(basename "$scenario_dir")"
}

attach_resource_analysis() {
    local scenario_dir="$1"
    jq --slurpfile resources "$scenario_dir/resources.json" '
        .resource_analysis = {
          artifact: "resources.json",
          status: ($resources[0].analysis_status //
                   (if $resources[0].pass == true then "passed" else "failed" end)),
          gating: ($resources[0] | if has("gating") then .gating else true end),
          pass: $resources[0].pass
        }
    ' "$scenario_dir/analysis.json" > "$scenario_dir/analysis.json.tmp"
    mv "$scenario_dir/analysis.json.tmp" "$scenario_dir/analysis.json"
}

run_logged() {
    local log_file="$1"
    local exit_file="$2"
    shift 2
    local -a statuses
    local command_rc tee_rc
    set +e
    "$@" 2>&1 | tee "$log_file"
    statuses=("${PIPESTATUS[@]}")
    set -e
    command_rc=${statuses[0]}
    tee_rc=${statuses[1]}
    printf '%d\n' "$command_rc" > "$exit_file"
    printf '%d\n' "$tee_rc" > "${exit_file%.txt}-tee.txt"
    [[ $command_rc -eq 0 ]] || return "$command_rc"
    [[ $tee_rc -eq 0 ]] || return "$tee_rc"
    return 0
}

wait_for_no_benchmark_routes() {
    local deadline=$((SECONDS + 300))
    while (( SECONDS < deadline )); do
        if [[ "$(total_route_count)" -eq 0 ]]; then
            return 0
        fi
        sleep 2
    done
    die "benchmark HTTPRoutes were not cleaned up"
}

assert_upstream_backend_absent() {
    local scenario_dir="$1"
    local resources
    local workloads="$scenario_dir/upstream-backend-workloads-before.json"
    resources="$(kubectl get deployment/backend service/backend -n default -o name --ignore-not-found)" || \
        die "could not inspect the upstream backend inventory"
    [[ -z "$resources" ]] || die "upstream default/backend resources exist before $(basename "$scenario_dir")"
    kubectl get replicasets,pods -n default -l app=backend -o json > "$workloads" || \
        die "could not inspect upstream backend ReplicaSets and pods"
    jq -e '.items | length == 0' "$workloads" >/dev/null || \
        die "upstream backend ReplicaSets or pods exist before $(basename "$scenario_dir")"
    : > "$scenario_dir/upstream-backend-before.txt"
}

# extract_upstream_backend_manifest writes the backend Deployment+Service the
# pinned upstream program applies (its Go `backendTemplate` constant) to a
# file, byte-for-byte, so the runner never carries a transcribed copy.
extract_upstream_backend_manifest() {
    local program="$1"
    local output="$2"
    local source="${UPSTREAM_DIR}/tests/${program}/${program}.go"
    [[ -f "$source" ]] || die "upstream program source not found: ${source}"
    awk '/^const backendTemplate = `/ {capture = 1; next} capture && /^`/ {exit} capture' "$source" > "$output" || \
        die "could not extract the upstream backend manifest from ${source}"
    yq -o=json -I=0 '.' "$output" | jq -se '
        length == 2 and
        any(.[]; .kind == "Deployment" and .metadata.name == "backend") and
        any(.[]; .kind == "Service" and .metadata.name == "backend")
    ' >/dev/null || die "upstream backend manifest in ${source} is not the expected Deployment + Service pair"
}

# prewarm_upstream_backend applies the program's exact backend manifest ahead
# of the program and waits until its pod is Ready and serving in the Service's
# EndpointSlice. The upstream program then re-applies the identical manifest
# (a no-op server-side apply), so the workload is unchanged; the difference is
# that route 0's first request measures HAPTIC's route propagation instead of
# the backend image pull and pod start-up (which cost ~6 s of 503s per cold
# scenario). The published joined runs shared one long-lived backend across
# tests, so this is the closer reproduction.
prewarm_upstream_backend() {
    local scenario_dir="$1"
    local program="$2"
    local manifest="$scenario_dir/upstream-backend-prewarm.yaml"
    extract_upstream_backend_manifest "$program" "$manifest"
    sha256sum "$manifest" | awk '{print $1}' > "$scenario_dir/upstream-backend-prewarm-sha256.txt"
    local generation_before
    generation_before="$(kubectl get haproxycfg "$HAPROXYCFG_NAME" -n "$RELEASE_NAMESPACE" \
        -o jsonpath='{.metadata.generation}')" || die "could not read the HAProxyCfg generation before the prewarm"
    [[ "$generation_before" =~ ^[0-9]+$ ]] || die "HAProxyCfg generation before the prewarm is not a number"
    local apply_rc=0
    run_logged "$scenario_dir/upstream-backend-prewarm-apply.log" \
        "$scenario_dir/upstream-backend-prewarm-apply-exit-code.txt" \
        kubectl apply --server-side --field-manager=haptic-gateway-api-bench -n default -f "$manifest" || apply_rc=$?
    [[ $apply_rc -eq 0 ]] || die "could not pre-create the upstream backend fixture"
    kubectl rollout status deployment/backend -n default --timeout=3m || \
        die "upstream backend Deployment did not become available before ${program}"
    local deadline=$((SECONDS + 120)) ready=false
    while (( SECONDS < deadline )); do
        if kubectl get endpointslices.discovery.k8s.io -n default -l kubernetes.io/service-name=backend -o json | jq -e '
            [.items[].endpoints[]? | select(.conditions.ready == true and (.conditions.terminating // false) == false)] | length > 0
        ' >/dev/null 2>&1; then
            ready=true
            break
        fi
        sleep 1
    done
    [[ "$ready" == "true" ]] || die "upstream backend Service has no ready endpoint before ${program}"
    kubectl get pods -n default -l app=backend -o json > "$scenario_dir/upstream-backend-prewarm-pods.json" || \
        die "could not inspect the pre-created upstream backend pod"
    jq -e '(.items | length) == 1 and
        any(.items[0].status.conditions[]?; .type == "Ready" and .status == "True")' \
        "$scenario_dir/upstream-backend-prewarm-pods.json" >/dev/null || \
        die "pre-created upstream backend is not exactly one Ready pod"
    # A new pod normally changes the rendered configuration (the bundled chart
    # maps pod IPs to names), and that update may still be in flight when the
    # scenario captures its baseline. Wait for HAPTIC to publish and deploy a
    # newer generation; a chart that renders nothing per pod simply lets the
    # bounded wait expire, which is recorded but not an error.
    local settle_rc=0 settled=true
    poll_for_haproxycfg_converged "" "$scenario_dir/upstream-backend-prewarm-haproxycfg.json" \
        "" $((SECONDS + 90)) "$DEFAULT_HAPROXYCFG_POLL_INTERVAL_SECONDS" \
        "$scenario_dir/upstream-backend-prewarm-haproxycfg-report.json" "" "$generation_before" \
        "" prewarm-settle-timeout || settle_rc=$?
    if [[ $settle_rc -eq "$READINESS_RESULT_DEADLINE" ]]; then
        settled=false
    elif [[ $settle_rc -ne 0 ]]; then
        die "could not observe the HAProxyCfg after the upstream backend prewarm"
    fi
    jq -n --arg program "$program" --arg sha "$(<"$scenario_dir/upstream-backend-prewarm-sha256.txt")" \
        --argjson generation_before "$generation_before" --argjson settled "$settled" \
        --slurpfile pods "$scenario_dir/upstream-backend-prewarm-pods.json" '
        {program: $program, manifest_source: ("tests/" + $program + "/" + $program + ".go backendTemplate"),
         manifest_sha256: $sha, applied_by: "runner, server-side apply; the upstream program re-applies the identical manifest",
         pod: {name: $pods[0].items[0].metadata.name, uid: $pods[0].items[0].metadata.uid,
               resource_version: $pods[0].items[0].metadata.resourceVersion},
         haproxycfg_generation_before: $generation_before,
         haproxycfg_advanced_and_converged: $settled,
         reason: "route 0 measures propagation, not backend pod start-up", pass: true}
    ' > "$scenario_dir/upstream-backend-prewarm.json"
    record_event upstream-backend-prewarmed "$program"
}

capture_upstream_backend_identity() {
    local scenario_dir="$1"
    local resources="$scenario_dir/upstream-backend-before-cleanup.json"
    local pods="$scenario_dir/upstream-backend-pods-before-cleanup.json"
    local replicasets="$scenario_dir/upstream-backend-replicasets-before-cleanup.json"
    local identity="$scenario_dir/upstream-backend-pod-identity.json"
    local selector
    selector="$(jq -er '
        .items[] | select(.kind == "Deployment" and .metadata.name == "backend") |
        .spec.selector.matchLabels | to_entries | sort_by(.key) |
        map(.key + "=" + (.value | tostring)) | join(",") | select(length > 0)
    ' "$resources")" || die "upstream backend Deployment has no exact pod selector"
    kubectl get replicasets -n default -l "$selector" -o json > "$replicasets" || \
        die "could not inspect the upstream backend ReplicaSet"
    jq -e --slurpfile resources "$resources" '
        ($resources[0].items[] | select(.kind == "Deployment" and .metadata.name == "backend")) as $deployment |
        (.items | length) == 1 and
        (.items[0] as $replicaset |
          ($replicaset.metadata.uid | type) == "string" and ($replicaset.metadata.uid | length) > 0 and
          any($replicaset.metadata.ownerReferences[]?;
            .controller == true and .kind == "Deployment" and .uid == $deployment.metadata.uid))
    ' "$replicasets" >/dev/null || die "upstream backend ReplicaSet is not uniquely Deployment-owned"
    local deadline=$((SECONDS + 120))
    while (( SECONDS < deadline )); do
        kubectl get pods -n default -l "$selector" -o json > "$pods" || \
            die "could not inspect the upstream backend pod"
        if jq -e --slurpfile resources "$resources" --slurpfile replicasets "$replicasets" '
            ($resources[0].items[] | select(.kind == "Deployment" and .metadata.name == "backend")) as $deployment |
            $replicasets[0].items[0] as $replicaset |
            (.items | length) == 1 and
            (.items[0] as $pod |
              $deployment.status.observedGeneration == $deployment.metadata.generation and
              ($deployment.status.availableReplicas // 0) == ($deployment.spec.replicas // 1) and
              $pod.metadata.deletionTimestamp == null and
              $pod.status.phase == "Running" and
              any($pod.metadata.ownerReferences[]?;
                .controller == true and .kind == "ReplicaSet" and .uid == $replicaset.metadata.uid) and
              any($pod.status.conditions[]?; .type == "Ready" and .status == "True") and
              ($pod.spec.containers | length) > 0 and
              ($pod.status.containerStatuses | length) == ($pod.spec.containers | length) and
              all($pod.spec.containers[];
                .name as $container_name |
                any($pod.status.containerStatuses[]; .name == $container_name)) and
              all($pod.status.containerStatuses[];
                .ready == true and .started == true and .restartCount == 0 and
                (.containerID | type) == "string" and (.containerID | length) > 0 and
                (.image | type) == "string" and (.image | length) > 0 and
                (.imageID | type) == "string" and
                (.imageID | test("@sha256:[0-9a-f]{64}$"))))
        ' "$pods" >/dev/null; then
            break
        fi
        sleep 1
    done
    jq -e --slurpfile resources "$resources" --slurpfile replicasets "$replicasets" '
        ($resources[0].items[] | select(.kind == "Deployment" and .metadata.name == "backend")) as $deployment |
        $replicasets[0].items[0] as $replicaset |
        (.items | length) == 1 and
        (.items[0] as $pod |
          $deployment.status.observedGeneration == $deployment.metadata.generation and
          ($deployment.status.availableReplicas // 0) == ($deployment.spec.replicas // 1) and
          $pod.metadata.deletionTimestamp == null and
          $pod.status.phase == "Running" and
          any($pod.metadata.ownerReferences[]?;
            .controller == true and .kind == "ReplicaSet" and .uid == $replicaset.metadata.uid) and
          any($pod.status.conditions[]?; .type == "Ready" and .status == "True") and
          ($pod.spec.containers | length) > 0 and
          ($pod.status.containerStatuses | length) == ($pod.spec.containers | length) and
          all($pod.spec.containers[];
            .name as $container_name |
            any($pod.status.containerStatuses[]; .name == $container_name)) and
          all($pod.status.containerStatuses[];
            .ready == true and .started == true and .restartCount == 0 and
            (.containerID | type) == "string" and (.containerID | length) > 0 and
            (.image | type) == "string" and (.image | length) > 0 and
            (.imageID | test("@sha256:[0-9a-f]{64}$"))))
    ' "$pods" >/dev/null || die "upstream backend pod is not uniquely Ready, restart-free, and digest-identified"
    jq -S --slurpfile resources "$resources" --slurpfile replicasets "$replicasets" '
        ($resources[0].items[] | select(.kind == "Deployment" and .metadata.name == "backend")) as $deployment |
        $replicasets[0].items[0] as $replicaset |
        .items[0] as $pod |
        ($pod.status.containerStatuses | map({key: .name, value: .}) | from_entries) as $statuses |
         {deployment: {name: $deployment.metadata.name, uid: $deployment.metadata.uid,
                      generation: $deployment.metadata.generation,
                      observed_generation: $deployment.status.observedGeneration},
         replica_set: {name: $replicaset.metadata.name, uid: $replicaset.metadata.uid,
                       owner_references: $replicaset.metadata.ownerReferences},
         pod: {name: $pod.metadata.name, uid: $pod.metadata.uid, node: $pod.spec.nodeName,
               phase: $pod.status.phase, owner_references: $pod.metadata.ownerReferences,
               containers: [$pod.spec.containers[] |
                 . as $container | $statuses[$container.name] as $status |
                 {name: $container.name, declared_image: $container.image,
                  runtime_image: $status.image, image_id: $status.imageID,
                  container_id: $status.containerID, ready: $status.ready,
                  started: $status.started, restart_count: $status.restartCount}]},
         pass: true}
    ' "$pods" > "$identity"
}

cleanup_upstream_backend() {
    local scenario_dir="$1"
    kubectl get deployment/backend service/backend -n default -o json \
        > "$scenario_dir/upstream-backend-before-cleanup.json" || \
        die "upstream backend resources are incomplete before cleanup"
    jq -e '
        ([.items[] | .kind + "/" + .metadata.name] | sort) ==
        ["Deployment/backend", "Service/backend"]
    ' "$scenario_dir/upstream-backend-before-cleanup.json" >/dev/null || \
        die "upstream backend inventory differs from the exact probe/routechange fixture"
    capture_upstream_backend_identity "$scenario_dir"
    local cleanup_rc=0
    run_logged "$scenario_dir/upstream-backend-cleanup.log" \
        "$scenario_dir/upstream-backend-cleanup-exit-code.txt" \
        kubectl delete deployment/backend service/backend -n default --wait=true --timeout=2m || cleanup_rc=$?
    [[ $cleanup_rc -eq 0 ]] || die "failed to delete the upstream backend fixture"
    local deadline=$((SECONDS + 120)) resources
    local workloads="$scenario_dir/upstream-backend-workloads-after-cleanup.json"
    while (( SECONDS < deadline )); do
        resources="$(kubectl get deployment/backend service/backend -n default -o name --ignore-not-found)" || \
            die "could not verify upstream backend cleanup"
        kubectl get replicasets,pods -n default -l app=backend -o json > "$workloads" || \
            die "could not verify upstream backend workload cleanup"
        if [[ -z "$resources" ]] && jq -e '.items | length == 0' "$workloads" >/dev/null; then
            : > "$scenario_dir/upstream-backend-after-cleanup.txt"
            jq -n --slurpfile workloads "$workloads" '
                {named_resources: [], replica_sets_and_pods: $workloads[0].items, pass: true}
            ' > "$scenario_dir/upstream-backend-after-cleanup.json"
            return 0
        fi
        sleep 1
    done
    die "upstream default/backend Deployment, Service, ReplicaSet, or pod remained after cleanup"
}

validate_haproxycfg_convergence_inputs() {
    local candidate="$1"
    local pods="$2"
    jq -e '
        type == "object" and
        ([.metadata.uid, .metadata.resourceVersion, .spec.checksum] |
         all(.[]; type == "string" and length > 0)) and
        (.metadata.generation | type) == "number" and .metadata.generation > 0 and
        ((.status.validationError // "") | type) == "string" and
        ((.status.deployedToPods == null) or (.status.deployedToPods | type) == "array") and
        all((.status.deployedToPods // [])[];
            (.podName | type) == "string" and (.podName | length) > 0 and
            ((.podUID == null) or (.podUID | type) == "string") and
            ((.checksum == null) or (.checksum | type) == "string") and
            ((.lastError == null) or (.lastError | type) == "string") and
            ((.consecutiveErrors == null) or
             ((.consecutiveErrors | type) == "number" and .consecutiveErrors >= 0))) and
        ([.status.deployedToPods[]?.podName] | unique | length) ==
            ((.status.deployedToPods // []) | length)
    ' "$candidate" >/dev/null || return 1
    jq -e '
        type == "object" and (.items | type) == "array" and
        all(.items[];
            ([.metadata.name, .metadata.uid] | all(.[]; type == "string" and length > 0)) and
            ((.metadata.deletionTimestamp == null) or
             (.metadata.deletionTimestamp | type) == "string") and
            ((.status.conditions == null) or (.status.conditions | type) == "array") and
            all((.status.conditions // [])[];
                (.type | type) == "string" and (.status | type) == "string") and
            ((.status.containerStatuses == null) or (.status.containerStatuses | type) == "array") and
            all((.status.containerStatuses // [])[];
                (.name | type) == "string" and (.name | length) > 0 and
                (.ready | type) == "boolean" and
                (.restartCount | type) == "number" and .restartCount >= 0)) and
        ([.items[].metadata.name] | unique | length) == (.items | length) and
        ([.items[].metadata.uid] | unique | length) == (.items | length)
    ' "$pods" >/dev/null
}

capture_haproxycfg_convergence_observation() {
    local candidate="$1"
    local pods="$2"
    local expected_checksum="$3"
    local attempt="$4"
    local output="$5"
    local required_uid="${6:-}"
    local minimum_generation="${7:-0}"
    local excluded_checksum="${8:-}"
    local observed_at
    observed_at="$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" || return "$READINESS_RESULT_EVIDENCE_INVALID"
    jq -S \
        --arg expected "$expected_checksum" \
        --arg required_uid "$required_uid" \
        --arg excluded_checksum "$excluded_checksum" \
        --arg observed_at "$observed_at" \
        --argjson attempt "$attempt" \
        --argjson minimum_generation "$minimum_generation" \
        --slurpfile pods "$pods" '
        . as $cfg |
        ($pods[0].items | map(select(.metadata.deletionTimestamp == null))) as $current |
        (.spec.checksum // "") as $checksum |
        ($cfg.status.deployedToPods // []) as $deployed |
        ($excluded_checksum == "__ALLOW_DEPLOYED_LAG__") as $allow_deployed_lag |
        ([$deployed[]?.checksum // ""] | unique) as $deployed_checksums |
        (($deployed_checksums | length) == 1 and ($deployed_checksums[0] | length) > 0) as
            $deployed_checksum_consistent |
        (($current | length) > 0) as $fleet_nonempty |
        ($expected == "" or $checksum == $expected) as $expected_checksum_matches |
        ($required_uid == "" or $cfg.metadata.uid == $required_uid) as $resource_uid_matches |
        ($minimum_generation == 0 or $cfg.metadata.generation > $minimum_generation) as
            $generation_advanced |
        ($excluded_checksum == "" or $checksum != $excluded_checksum) as $checksum_changed |
        (($cfg.status.validationError // "") == "") as $validation_clean |
        (($deployed | length) == ($current | length)) as $deployment_record_count_matches |
        (all($current[];
            any(.status.conditions[]?; .type == "Ready" and .status == "True") and
            (.status.containerStatuses | length) > 0 and
            all(.status.containerStatuses[]?; .ready == true and .restartCount == 0))) as
            $fleet_ready_and_restart_free |
        (all($current[];
            . as $pod |
            any($deployed[]?;
                .podName == $pod.metadata.name and
                (.podUID // "") == $pod.metadata.uid and
                .checksum == (if $allow_deployed_lag then $deployed_checksums[0] else $checksum end) and
                ((.lastError // "") == "") and
                ((.consecutiveErrors // 0) == 0)))) as $exact_current_deployment |
        {attempt: $attempt,
         observed_at: $observed_at,
         haproxycfg: {
           uid: ($cfg.metadata.uid // null),
           generation: ($cfg.metadata.generation // null),
           resource_version: ($cfg.metadata.resourceVersion // null),
           spec_checksum: $checksum,
           deployed_checksums: [$deployed[]?.checksum],
           deployed_checksum_target: (if $allow_deployed_lag then ($deployed_checksums[0] // null) else $checksum end)
         },
         current_fleet: [$current[] |
           {name: .metadata.name, uid: .metadata.uid,
            ready: any(.status.conditions[]?; .type == "Ready" and .status == "True"),
            container_restarts: [.status.containerStatuses[]?.restartCount]}],
         checks: {
           fleet_nonempty: $fleet_nonempty,
           expected_checksum_matches: $expected_checksum_matches,
           resource_uid_matches: $resource_uid_matches,
           generation_advanced: $generation_advanced,
           checksum_changed: $checksum_changed,
           validation_clean: $validation_clean,
           deployment_record_count_matches: $deployment_record_count_matches,
           deployed_checksum_consistent: $deployed_checksum_consistent,
           allow_deployed_lag: $allow_deployed_lag,
           fleet_ready_and_restart_free: $fleet_ready_and_restart_free,
           exact_current_deployment: $exact_current_deployment
         },
         pass: ($fleet_nonempty and $expected_checksum_matches and $resource_uid_matches and
                $generation_advanced and $checksum_changed and
                $validation_clean and $deployment_record_count_matches and
                $fleet_ready_and_restart_free and $deployed_checksum_consistent and
                $exact_current_deployment)}
    ' "$candidate" > "$output"
}

write_readiness_poll_report() {
    local report="$1"
    local outcome="$2"
    local reason_code="$3"
    local attempts="$4"
    local started_at="$5"
    local started_seconds="$6"
    local deadline="$7"
    local poll_interval="$8"
    local observation="$9"
    [[ -n "$report" ]] || return 0
    local timeout_seconds=$((deadline - started_seconds))
    (( timeout_seconds >= 0 )) || timeout_seconds=0
    local finished_at="" timestamp_valid=true
    if ! finished_at="$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)"; then
        timestamp_valid=false
        if [[ "$outcome" != "evidence-invalid" ]]; then
            outcome=evidence-invalid
            reason_code=readiness-finished-timestamp-failed
        fi
    fi
    local available=null observation_valid=true
    if [[ -s "$observation" ]]; then
        if (( attempts > 0 )); then
            if ! available="$(jq -ce --argjson attempt "$attempts" '
                select(type == "object" and .attempt == $attempt and (.pass | type) == "boolean")
            ' "$observation")"; then
                available=null
                observation_valid=false
            fi
        else
            if ! available="$(jq -c . "$observation")"; then
                available=null
                observation_valid=false
            fi
        fi
    elif (( attempts > 0 )); then
        observation_valid=false
    fi
    if [[ "$outcome" != "evidence-invalid" && "$observation_valid" != "true" ]]; then
        outcome=evidence-invalid
        reason_code=readiness-observation-invalid
        timestamp_valid=false
    fi
    jq -S -n \
        --arg outcome "$outcome" \
        --arg reason_code "$reason_code" \
        --arg started_at "$started_at" \
        --arg finished_at "$finished_at" \
        --argjson attempts "$attempts" \
        --argjson timeout_seconds "$timeout_seconds" \
        --argjson elapsed_seconds "$((SECONDS - started_seconds))" \
        --argjson poll_interval_seconds "$poll_interval" \
        --argjson observation "$available" '
        def invalid_observation:
          if (($observation | type) == "object" and
              $observation.evidence_valid == false and
              $observation.attempt == $attempts and
              $observation.reason_code == $reason_code)
          then $observation
          else {attempt: $attempts, evidence_valid: false, reason_code: $reason_code,
                last_available_observation: $observation, pass: false}
          end;
        {outcome: $outcome,
         reason_code: $reason_code,
         evidence_valid: ($outcome != "evidence-invalid"),
         pass: ($outcome == "converged"),
         deadline_reached: ($outcome == "deadline"),
         attempts: $attempts,
         started_at: (if $started_at == "" then null else $started_at end),
         finished_at: (if $finished_at == "" then null else $finished_at end),
         timeout_seconds: $timeout_seconds,
         elapsed_seconds: $elapsed_seconds,
         poll_interval_seconds: $poll_interval_seconds,
         last_observation: (if $outcome == "evidence-invalid"
                            then invalid_observation else $observation end)}
    ' > "$report" || return "$READINESS_RESULT_EVIDENCE_INVALID"
    [[ "$timestamp_valid" == "true" ]] || return "$READINESS_RESULT_EVIDENCE_INVALID"
}

record_readiness_callback_failure() {
    local observation="$1"
    local attempt="$2"
    local reason_code="$3"
    local invalid="${observation}.invalid"
    local available=null
    if [[ -s "$observation" ]]; then
        available="$(jq -c . "$observation")" || available=null
    fi
    jq -S -n --arg reason_code "$reason_code" --argjson attempt "$attempt" \
        --argjson available "$available" '
        if (($available | type) == "object" and $available.evidence_valid == false and
            $available.attempt == $attempt and $available.reason_code == $reason_code)
        then $available
        else {attempt: $attempt, evidence_valid: false, reason_code: $reason_code,
              last_available_observation: $available, pass: false}
        end
    ' > "$invalid" && mv "$invalid" "$observation"
}

finish_readiness_evidence_failure() {
    local observation="$1"
    local attempt="$2"
    local reason_code="$3"
    local report="$4"
    local started_at="$5"
    local started_seconds="$6"
    local deadline="$7"
    local poll_interval="$8"
    write_readiness_poll_report "$report" evidence-invalid "$reason_code" \
        "$attempt" "$started_at" "$started_seconds" "$deadline" "$poll_interval" "$observation" || \
        return "$READINESS_RESULT_EVIDENCE_INVALID"
    return 0
}

observe_haproxycfg_convergence() {
    local attempt="$1"
    local observation="$2"
    local expected_checksum="$3"
    local output="$4"
    local required_uid="$5"
    local minimum_generation="$6"
    local excluded_checksum="$7"
    local candidate="${WORK_DIR}/haproxycfg-candidate.json"
    local candidate_read="${candidate}.read"
    local pods="${WORK_DIR}/haproxycfg-pods.json"
    local pods_read="${pods}.read"
    if ! kubectl get haproxycfg "$HAPROXYCFG_NAME" -n "$RELEASE_NAMESPACE" -o json > "$candidate_read"; then
        record_readiness_callback_failure "$observation" "$attempt" haproxycfg-read-failed
        return "$READINESS_RESULT_EVIDENCE_INVALID"
    fi
    mv "$candidate_read" "$candidate" || return "$READINESS_RESULT_EVIDENCE_INVALID"
    if ! kubectl get pods -n "$RELEASE_NAMESPACE" \
        -l app.kubernetes.io/instance=haptic,app.kubernetes.io/component=loadbalancer \
        -o json > "$pods_read"; then
        record_readiness_callback_failure "$observation" "$attempt" haproxy-pod-read-failed
        return "$READINESS_RESULT_EVIDENCE_INVALID"
    fi
    mv "$pods_read" "$pods" || return "$READINESS_RESULT_EVIDENCE_INVALID"
    if ! validate_haproxycfg_convergence_inputs "$candidate" "$pods"; then
        record_readiness_callback_failure "$observation" "$attempt" malformed-convergence-snapshot
        return "$READINESS_RESULT_EVIDENCE_INVALID"
    fi
    if ! capture_haproxycfg_convergence_observation "$candidate" "$pods" \
        "$expected_checksum" "$attempt" "$observation" "$required_uid" \
        "$minimum_generation" "$excluded_checksum"; then
        record_readiness_callback_failure "$observation" "$attempt" convergence-evaluation-failed
        return "$READINESS_RESULT_EVIDENCE_INVALID"
    fi
    cp "$candidate" "$output" && cp "$pods" "${output%.json}-pods.json" || \
        return "$READINESS_RESULT_EVIDENCE_INVALID"
    jq -e '.pass == true' "$observation" >/dev/null || return "$READINESS_RESULT_DEADLINE"
}

poll_for_readiness() {
    local callback="$1"
    local success_reason="$2"
    local timeout_reason="$3"
    local monitor_container="$4"
    local requested_deadline="$5"
    local maximum_wait_seconds="$6"
    local poll_interval="$7"
    local report="$8"
    local observation="$9"
    shift 9
    local started_seconds=$SECONDS
    local started_at=""
    local deadline=$((SECONDS + maximum_wait_seconds))
    if [[ -n "$requested_deadline" && "$requested_deadline" -lt "$deadline" ]]; then
        deadline="$requested_deadline"
    fi
    local attempts=0 observation_rc=0
    if ! printf 'null\n' > "$observation"; then
        started_at="$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" || started_at=""
        finish_readiness_evidence_failure "$observation" 0 \
            readiness-observation-initialization-failed "$report" "$started_at" \
            "$started_seconds" "$deadline" "$poll_interval" || true
        return "$READINESS_RESULT_EVIDENCE_INVALID"
    fi
    if ! started_at="$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)"; then
        finish_readiness_evidence_failure "$observation" 0 readiness-start-timestamp-failed \
            "$report" "$started_at" "$started_seconds" "$deadline" "$poll_interval" || true
        return "$READINESS_RESULT_EVIDENCE_INVALID"
    fi
    while (( SECONDS < deadline )); do
        if [[ -n "$monitor_container" ]] && ! workload_container_running "$monitor_container"; then
            finish_readiness_evidence_failure "$observation" "$attempts" workload-exited \
                "$report" "$started_at" "$started_seconds" "$deadline" "$poll_interval" || true
            return "$READINESS_RESULT_EVIDENCE_INVALID"
        fi
        observation_rc=0
        attempts=$((attempts + 1))
        "$callback" "$attempts" "$observation" "$@" || observation_rc=$?
        if [[ -n "$monitor_container" ]] && ! workload_container_running "$monitor_container"; then
            finish_readiness_evidence_failure "$observation" "$attempts" workload-exited \
                "$report" "$started_at" "$started_seconds" "$deadline" "$poll_interval" || true
            return "$READINESS_RESULT_EVIDENCE_INVALID"
        fi
        if [[ $observation_rc -eq "$READINESS_RESULT_EVIDENCE_INVALID" ]]; then
            local callback_reason
            callback_reason="$(jq -er --argjson attempt "$attempts" '
                select(type == "object" and .evidence_valid == false and
                       .attempt == $attempt and (.reason_code | type) == "string" and
                       (.reason_code | length) > 0) | .reason_code
            ' "$observation")" || callback_reason=readiness-callback-evidence-invalid
            finish_readiness_evidence_failure "$observation" "$attempts" \
                "$callback_reason" "$report" "$started_at" "$started_seconds" \
                "$deadline" "$poll_interval" || true
            return "$READINESS_RESULT_EVIDENCE_INVALID"
        fi
        if [[ $observation_rc -ne 0 && $observation_rc -ne "$READINESS_RESULT_DEADLINE" ]]; then
            finish_readiness_evidence_failure "$observation" "$attempts" \
                "unexpected-readiness-callback-exit-${observation_rc}" "$report" "$started_at" \
                "$started_seconds" "$deadline" "$poll_interval" || true
            return "$READINESS_RESULT_EVIDENCE_INVALID"
        fi
        if [[ $observation_rc -eq 0 ]] && (( SECONDS < deadline )); then
            write_readiness_poll_report "$report" converged "$success_reason" \
                "$attempts" "$started_at" "$started_seconds" "$deadline" "$poll_interval" "$observation" || \
                return "$READINESS_RESULT_EVIDENCE_INVALID"
            return 0
        fi
        (( SECONDS < deadline )) || break
        if ! sleep "$poll_interval"; then
            finish_readiness_evidence_failure "$observation" "$attempts" readiness-sleep-failed \
                "$report" "$started_at" "$started_seconds" "$deadline" "$poll_interval" || true
            return "$READINESS_RESULT_EVIDENCE_INVALID"
        fi
    done
    write_readiness_poll_report "$report" deadline "$timeout_reason" \
        "$attempts" "$started_at" "$started_seconds" "$deadline" "$poll_interval" "$observation" || \
        return "$READINESS_RESULT_EVIDENCE_INVALID"
    return "$READINESS_RESULT_DEADLINE"
}

poll_for_haproxycfg_converged() {
    local expected_checksum="$1"
    local output="$2"
    local monitor_container="${3:-}"
    local requested_deadline="${4:-}"
    local poll_interval="${5:-$DEFAULT_HAPROXYCFG_POLL_INTERVAL_SECONDS}"
    local report="${6:-}"
    local required_uid="${7:-}"
    local minimum_generation="${8:-0}"
    local excluded_checksum="${9:-}"
    local timeout_reason="${10:-exact-current-timeout}"
    rm -f -- "$output" "${output%.json}-pods.json" || return "$READINESS_RESULT_EVIDENCE_INVALID"
    poll_for_readiness observe_haproxycfg_convergence converged "$timeout_reason" \
        "$monitor_container" "$requested_deadline" 1200 "$poll_interval" "$report" \
        "${WORK_DIR}/haproxycfg-observation.json" "$expected_checksum" "$output" \
        "$required_uid" "$minimum_generation" "$excluded_checksum"
}

wait_for_haproxycfg_converged() {
    local expected_checksum="$1"
    local output="$2"
    local monitor_container="${3:-}"
    local requested_deadline="${4:-}"
    local wait_rc=0
    poll_for_haproxycfg_converged "$expected_checksum" "$output" "$monitor_container" \
        "$requested_deadline" "$DEFAULT_HAPROXYCFG_POLL_INTERVAL_SECONDS" || wait_rc=$?
    if [[ $wait_rc -eq 0 ]]; then
        return 0
    fi
    if [[ $wait_rc -eq "$READINESS_RESULT_DEADLINE" ]]; then
        die "HAProxyCfg did not converge to checksum ${expected_checksum:-<current>}"
    fi
    die "HAProxyCfg convergence evidence could not be collected"
}

capture_haproxycfg_baseline() {
    local scenario_dir="$1"
    wait_for_haproxycfg_converged "" "$scenario_dir/haproxycfg-baseline.json"
    jq -er '.spec.checksum | select(length > 0)' "$scenario_dir/haproxycfg-baseline.json" \
        > "$scenario_dir/haproxycfg-baseline-checksum.txt"
    wait_for_referenced_map_inventory "$scenario_dir/haproxycfg-baseline.json" \
        "$scenario_dir/haproxycfg-baseline-pods.json" "$scenario_dir/map-inventory-baseline.json"
}

wait_for_haproxycfg_baseline() {
    local scenario_dir="$1"
    local checksum
    checksum="$(<"$scenario_dir/haproxycfg-baseline-checksum.txt")"
    wait_for_haproxycfg_converged "$checksum" "$scenario_dir/haproxycfg-final.json"
    wait_for_referenced_map_inventory "$scenario_dir/haproxycfg-final.json" \
        "$scenario_dir/haproxycfg-final-pods.json" "$scenario_dir/map-inventory-final.json"
    cmp -s "$scenario_dir/map-inventory-baseline.json" "$scenario_dir/map-inventory-final.json" || {
        diff -u "$scenario_dir/map-inventory-baseline.json" "$scenario_dir/map-inventory-final.json" \
            > "$scenario_dir/map-inventory.diff" || true
        die "HAProxy referenced map checksums did not return to the pre-scenario baseline"
    }
    : > "$scenario_dir/map-inventory.diff"
}

# capture_referenced_map_inventory reads the parent HAProxyCfg fresh on every
# attempt and keeps the snapshot only for the object's identity: under churn
# the parent's set-id annotation and its status are two writes, and a snapshot
# taken between them can never satisfy the equality this polls for (run
# 20260909T075328Z timed out on exactly that after every map had converged).
capture_referenced_map_inventory() {
    local snapshot="$1"
    local pods="$2"
    local output="$3"
    local allow_deployed_lag="${4:-false}"
    local objects="${output%.json}-objects.json"
    local candidate="${output}.candidate"
    local cfg="${output%.json}-cfg.json"
    local identities="${output%.json}-identities.json"
    kubectl get haproxymapfiles -n "$RELEASE_NAMESPACE" -o json > "$objects" || \
        return "$READINESS_RESULT_EVIDENCE_INVALID"
    kubectl get haproxycfg "$HAPROXYCFG_NAME" -n "$RELEASE_NAMESPACE" -o json > "${cfg}.read" || \
        return "$READINESS_RESULT_EVIDENCE_INVALID"
    mv "${cfg}.read" "$cfg" || return "$READINESS_RESULT_EVIDENCE_INVALID"
    jq -e --slurpfile snapshot "$snapshot" '
        (.metadata.uid | type) == "string" and .metadata.uid == $snapshot[0].metadata.uid
    ' "$cfg" >/dev/null || return "$READINESS_RESULT_EVIDENCE_INVALID"
    validate_haproxycfg_convergence_inputs "$cfg" "$pods" || \
        return "$READINESS_RESULT_EVIDENCE_INVALID"
    jq -e 'type == "object" and (.items | type) == "array"' "$objects" >/dev/null || \
        return "$READINESS_RESULT_EVIDENCE_INVALID"
    jq -e '
        ((.status.auxiliaryFiles == null) or
         ((.status.auxiliaryFiles | type) == "object" and
          ((.status.auxiliaryFiles.mapFiles == null) or
           (.status.auxiliaryFiles.mapFiles | type) == "array")))
    ' "$cfg" >/dev/null || return "$READINESS_RESULT_EVIDENCE_INVALID"
    jq -e --arg default_namespace "$RELEASE_NAMESPACE" --slurpfile cfg "$cfg" '
        ($cfg[0].status.auxiliaryFiles.mapFiles // []) as $refs |
        [.items[] as $object |
          select(any($refs[]?;
            $object.metadata.name == .name and
            $object.metadata.namespace == (.namespace // $default_namespace))) |
          $object] as $selected |
        all($selected[]; . as $object |
          ([$object.metadata.namespace, $object.metadata.name, $object.metadata.uid,
            $object.metadata.resourceVersion] | all(.[]; type == "string" and length > 0)) and
          ($object.spec | type) == "object" and
          ([$object.spec.mapName, $object.spec.path, $object.spec.checksum] |
           all(.[]; type == "string" and length > 0)) and
          (($object.status.deployedToPods == null) or
           (($object.status.deployedToPods | type) == "array" and
            all($object.status.deployedToPods[];
              (.podName | type) == "string" and (.podName | length) > 0 and
              ([.podUID, .podRuntimeID, .checksum, .lastError] |
               all(.[]; . == null or type == "string")) and
              ((.consecutiveErrors == null) or
               ((.consecutiveErrors | type) == "number" and .consecutiveErrors >= 0))))))
    ' "$objects" >/dev/null || return "$READINESS_RESULT_EVIDENCE_INVALID"
    "$PROJECT_ROOT/bin/check-map-identities" "$cfg" "$objects" > "$identities" || \
        return "$READINESS_RESULT_EVIDENCE_INVALID"
    : > "$candidate" || return "$READINESS_RESULT_EVIDENCE_INVALID"
    jq -S --arg default_namespace "$RELEASE_NAMESPACE" --argjson allow_deployed_lag "$allow_deployed_lag" \
        --slurpfile cfg "$cfg" --slurpfile pods "$pods" --slurpfile identities "$identities" '
        $cfg[0] as $parent |
        ($parent.status.auxiliaryFiles.setID // "") as $set_id |
        ($parent.status.auxiliaryFiles.mapFiles // []) as $refs |
        ($pods[0].items | map(select(.metadata.deletionTimestamp == null))) as $current |
        [.items[] as $object |
          $refs[] as $ref |
          select($object.metadata.name == $ref.name and
                 $object.metadata.namespace == ($ref.namespace // $default_namespace)) |
          $object] as $selected |
        select($set_id != "" and
               $parent.metadata.annotations["haproxy-haptic.org/auxiliary-set-id"] == $set_id and
               ($refs | length) > 0) |
        select(all($refs[]; .kind == "HAProxyMapFile" and (.name // "") != "")) |
        select(($refs | map([(.namespace // $default_namespace), .name] | join("/")) | unique | length) == ($refs | length)) |
        select(($selected | length) == ($refs | length)) |
        select(all($selected[];
          ($identities[0].valid[.metadata.namespace + "/" + .metadata.name] == .metadata.uid and
           (.spec.mapName // "") != "" and (.spec.path // "") != "" and
           (.spec.checksum // "") != "" and
           ((.status.deployedToPods // []) | length) == ($current | length)) and
          (. as $map |
           all($current[];
             . as $pod |
             any($map.status.deployedToPods[]?;
               .podName == $pod.metadata.name and
               (.podUID // "") == $pod.metadata.uid and
               ((.podRuntimeID // "") != "") and
               .checksum == (if $allow_deployed_lag then
                 ([ $map.status.deployedToPods[]?.checksum ] | unique | select(length == 1) | .[0])
                 else $map.spec.checksum end) and
               ((.lastError // "") == "") and
               ((.consecutiveErrors // 0) == 0)))))) |
        {pods: [$current[] | {name: .metadata.name, uid: .metadata.uid}] | sort_by(.name),
         maps: [$selected[] |
           {namespace: .metadata.namespace, name: .metadata.name,
            map_name: .spec.mapName, path: .spec.path, checksum: .spec.checksum,
            deployed_to_pods: [.status.deployedToPods[] |
              {pod_name: .podName, pod_uid: .podUID, pod_runtime_id: .podRuntimeID,
               checksum}] | sort_by(.pod_name)}] |
           sort_by(.namespace, .name)}
    ' "$objects" > "$candidate" || return "$READINESS_RESULT_EVIDENCE_INVALID"
    [[ -s "$candidate" ]] || return "$READINESS_RESULT_DEADLINE"
    mv "$candidate" "$output" || return "$READINESS_RESULT_EVIDENCE_INVALID"
}

observe_referenced_map_inventory() {
    local attempt="$1"
    local observation="$2"
    local cfg="$3"
    local pods="$4"
    local output="$5"
    local allow_deployed_lag="${6:-false}"
    local inventory_rc=0 pass=false
    capture_referenced_map_inventory "$cfg" "$pods" "$output" "$allow_deployed_lag" || inventory_rc=$?
    if [[ $inventory_rc -eq "$READINESS_RESULT_EVIDENCE_INVALID" ]]; then
        record_readiness_callback_failure "$observation" "$attempt" referenced-map-evidence-invalid
        return "$READINESS_RESULT_EVIDENCE_INVALID"
    fi
    [[ $inventory_rc -eq 0 || $inventory_rc -eq "$READINESS_RESULT_DEADLINE" ]] || \
        return "$READINESS_RESULT_EVIDENCE_INVALID"
    [[ $inventory_rc -eq 0 ]] && pass=true
    jq -S -n --argjson attempt "$attempt" --argjson pass "$pass" \
        --slurpfile cfg "${output%.json}-cfg.json" --slurpfile pods "$pods" \
        --slurpfile objects "${output%.json}-objects.json" '
        {attempt: $attempt,
         referenced_maps: (($cfg[0].status.auxiliaryFiles.mapFiles // []) | length),
         available_map_objects: ($objects[0].items | length),
         current_pods: ([$pods[0].items[] |
           select(.metadata.deletionTimestamp == null)] | length),
         pass: $pass}
    ' > "$observation" || return "$READINESS_RESULT_EVIDENCE_INVALID"
    return "$inventory_rc"
}

poll_for_referenced_map_inventory() {
    local cfg="$1"
    local pods="$2"
    local output="$3"
    local monitor_container="${4:-}"
    local requested_deadline="${5:-}"
    local poll_interval="${6:-$DEFAULT_HAPROXYCFG_POLL_INTERVAL_SECONDS}"
    local report="${7:-}"
    local allow_deployed_lag="${8:-false}"
    poll_for_readiness observe_referenced_map_inventory referenced-map-ready referenced-map-timeout \
        "$monitor_container" "$requested_deadline" 300 "$poll_interval" "$report" \
        "${WORK_DIR}/referenced-map-observation.json" "$cfg" "$pods" "$output" "$allow_deployed_lag"
}

wait_for_referenced_map_inventory() {
    local wait_rc=0
    poll_for_referenced_map_inventory "$@" || wait_rc=$?
    if [[ $wait_rc -eq 0 ]]; then
        return 0
    fi
    if [[ $wait_rc -eq "$READINESS_RESULT_DEADLINE" ]]; then
        die "HAProxy referenced maps did not converge to the current pod fleet"
    fi
    die "HAProxy referenced map convergence evidence could not be collected"
}

extract_loadbalancer_identities() {
    local pods_json="$1"
    local output="$2"
    jq -S '
        [.items[] |
          select(.metadata.deletionTimestamp == null) |
          {namespace: .metadata.namespace, name: .metadata.name, uid: .metadata.uid,
           containers: [.status.containerStatuses[]? |
             {name, containerID, restartCount, ready}] | sort_by(.name)}] |
        sort_by(.namespace, .name)
    ' "$pods_json" > "$output"
    jq -e 'length > 0 and all(.[];
        (.containers | length) > 0 and
        all(.containers[]; .ready == true and .restartCount == 0 and (.containerID // "") != ""))' \
        "$output" >/dev/null || die "HAProxy fleet identity is incomplete"
}

pid_running() {
    local pid="$1"
    local state
    state="$(ps -o stat= -p "$pid" 2>/dev/null)" || return 1
    [[ "$state" != Z* ]]
}

stop_routechange_tunnels() {
    [[ ${#routechange_tunnel_pids[@]} -gt 0 ]] || return 0
    local pid deadline running
    for pid in "${routechange_tunnel_pids[@]}"; do
        pid_running "$pid" && kill -INT "$pid" 2>/dev/null || true
    done
    deadline=$((SECONDS + 3))
    while (( SECONDS < deadline )); do
        running=false
        for pid in "${routechange_tunnel_pids[@]}"; do
            pid_running "$pid" && running=true
        done
        [[ "$running" == "false" ]] && break
        sleep 0.1
    done
    for pid in "${routechange_tunnel_pids[@]}"; do
        pid_running "$pid" && kill -TERM "$pid" 2>/dev/null || true
    done
    deadline=$((SECONDS + 2))
    while (( SECONDS < deadline )); do
        running=false
        for pid in "${routechange_tunnel_pids[@]}"; do
            pid_running "$pid" && running=true
        done
        [[ "$running" == "false" ]] && break
        sleep 0.1
    done
    for pid in "${routechange_tunnel_pids[@]}"; do
        pid_running "$pid" && kill -KILL "$pid" 2>/dev/null || true
        wait "$pid" 2>/dev/null || true
    done
    for pid in "${routechange_tunnel_pids[@]}"; do
        pid_running "$pid" && return 1
    done
    routechange_tunnel_pids=()
}

start_routechange_tunnels() {
    local scenario_dir="$1"
    local pods="$scenario_dir/routechange-fleet-before-pods.json"
    local identities="$scenario_dir/routechange-fleet-before.json"
    local tunnel_dir="$scenario_dir/tunnels"
    mkdir -p "$tunnel_dir"
    kubectl get pods -n "$RELEASE_NAMESPACE" \
        -l app.kubernetes.io/instance=haptic,app.kubernetes.io/component=loadbalancer \
        -o json > "$pods"
    extract_loadbalancer_identities "$pods" "$identities"
    capture_gateway_service_targets "$scenario_dir/gateway-services-before.json" || \
        die "could not resolve benchmark Gateway Service targets"
    : > "$scenario_dir/tunnels.tsv"
    local gateway service service_uid service_rv target_port pod uid pid port deadline slug
    local -a service_rows=() pod_rows=()
    mapfile -t service_rows < <(jq -r '.[] | [.gateway, .service, .uid, .resource_version, .target_port] | @tsv' \
        "$scenario_dir/gateway-services-before.json")
    mapfile -t pod_rows < <(jq -r '.[] | [.name, .uid] | @tsv' "$identities")
    ((${#service_rows[@]} == ${#GATEWAYS[@]})) || die "routechange Gateway Service inventory is incomplete"
    ((${#pod_rows[@]} > 0)) || die "routechange HAProxy fleet inventory is empty"
    for service_row in "${service_rows[@]}"; do
        IFS=$'\t' read -r gateway service service_uid service_rv target_port <<< "$service_row"
        slug="${gateway//\//_}"
        for pod_row in "${pod_rows[@]}"; do
            IFS=$'\t' read -r pod uid <<< "$pod_row"
            kubectl port-forward --address 127.0.0.1 -n "$RELEASE_NAMESPACE" \
                "pod/${pod}" ":${target_port}" > "$tunnel_dir/${slug}-${pod}.log" 2>&1 < /dev/null &
            pid=$!
            routechange_tunnel_pids+=("$pid")
            deadline=$((SECONDS + 20))
            port=""
            while (( SECONDS < deadline )); do
                pid_running "$pid" || {
                    printf '%s\n' "port-forward pid ${pid} exited for ${gateway} via ${pod}" > "$scenario_dir/tunnel-error.txt"
                    die "port-forward for ${gateway} via ${pod} exited before becoming ready"
                }
                port="$(rg -o 'Forwarding from 127\.0\.0\.1:[0-9]+' "$tunnel_dir/${slug}-${pod}.log" 2>/dev/null | \
                    tail -n 1 | awk -F: '{print $NF}')" || port=""
                [[ "$port" =~ ^[1-9][0-9]*$ ]] && break
                sleep 0.1
            done
            [[ "$port" =~ ^[1-9][0-9]*$ ]] || {
                printf '%s\n' "port-forward log did not report a local port for ${gateway} via ${pod}" > "$scenario_dir/tunnel-error.txt"
                die "port-forward for ${gateway} via ${pod} did not report a local port"
            }
            printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
                "$gateway" "$service" "$service_uid" "$service_rv" "$target_port" \
                "$pod" "$uid" "$pid" "$port" >> "$scenario_dir/tunnels.tsv"
        done
    done
    assert_routechange_marker_absent "$scenario_dir" before
}

capture_gateway_service_targets() {
    local output="$1"
    local rows="${output}.ndjson"
    : > "$rows"
    local gateway gateway_namespace gateway_name services
    for gateway in "${GATEWAYS[@]}"; do
        gateway_namespace="${gateway%/*}"
        gateway_name="${gateway#*/}"
        services="${output}.${gateway_namespace}_${gateway_name}.json"
        kubectl get services -n "$RELEASE_NAMESPACE" \
            -l "gateway.networking.k8s.io/gateway-name=${gateway_name},gateway.networking.k8s.io/gateway-namespace=${gateway_namespace}" \
            -o json > "$services" || return 1
        jq -ce --arg gateway "$gateway" '
            select((.items | length) == 1) |
            .items[0] as $service |
            [$service.spec.ports[] |
              select(.port == 80 and (.protocol // "TCP") == "TCP" and (.targetPort | type) == "number")] as $ports |
            select(($service.metadata.name // "") != "" and
                   ($service.metadata.uid // "") != "" and
                   ($service.metadata.resourceVersion // "") != "" and
                   ($ports | length) == 1 and
                   $ports[0].targetPort > 0 and $ports[0].targetPort <= 65535) |
            {gateway: $gateway, namespace: $service.metadata.namespace, service: $service.metadata.name,
             uid: $service.metadata.uid, resource_version: $service.metadata.resourceVersion,
             port: 80, target_port: $ports[0].targetPort}
        ' "$services" >> "$rows" || return 1
    done
    jq -sS 'sort_by(.gateway)' "$rows" > "$output"
    jq -e --argjson expected "${#GATEWAYS[@]}" '
        length == $expected and ([.[].gateway] | unique | length) == $expected and
        ([.[].target_port] | unique | length) == $expected
    ' "$output" >/dev/null
}

revalidate_routechange_targets() {
    local scenario_dir="$1"
    local phase="$2"
    local current_pods="$scenario_dir/routechange-fleet-${phase}-pods.json"
    local current_identities="$scenario_dir/routechange-fleet-${phase}.json"
    local current_services="$scenario_dir/gateway-services-${phase}.json"
    kubectl get pods -n "$RELEASE_NAMESPACE" \
        -l app.kubernetes.io/instance=haptic,app.kubernetes.io/component=loadbalancer \
        -o json > "$current_pods"
    extract_loadbalancer_identities "$current_pods" "$current_identities"
    cmp -s "$scenario_dir/routechange-fleet-before.json" "$current_identities" || \
        die "HAProxy pod identities changed during routechange ${phase} proof"
    capture_gateway_service_targets "$current_services" || \
        die "Gateway Service targets disappeared during routechange ${phase} proof"
    if ! jq -S 'map(del(.resource_version))' "$scenario_dir/gateway-services-before.json" | \
        cmp - <(jq -S 'map(del(.resource_version))' "$current_services"); then
        die "Gateway Service identity or targetPort changed during routechange ${phase} proof"
    fi
}

assert_routechange_marker_absent() {
    local scenario_dir="$1"
    local phase="$2"
    local output_dir="$scenario_dir/live-http-${phase}"
    mkdir -p "$output_dir"
    local gateway service service_uid service_rv target_port pod uid pid port http_code slug
    while IFS=$'\t' read -r gateway service service_uid service_rv target_port pod uid pid port; do
        slug="${gateway//\//_}-${pod}"
        pid_running "$pid" || die "port-forward for ${gateway} via ${pod} exited during ${phase} proof"
        http_code="$(curl --silent --show-error --max-time 2 \
            --dump-header "$output_dir/${slug}.headers" \
            --output "$output_dir/${slug}.body" \
            --write-out '%{http_code}' \
            --header 'Host: route.example.com' \
            --header 'Connection: close' \
            "http://127.0.0.1:${port}/haptic-benchmark-${phase}")" || \
            die "direct request to ${gateway} via HAProxy pod ${pod} failed during ${phase} proof"
        printf '%s\n' "$http_code" > "$output_dir/${slug}.status-code.txt"
        [[ "$http_code" == "404" ]] || \
            die "direct request to ${gateway} via HAProxy pod ${pod} returned ${http_code} during ${phase} proof, expected 404"
        if rg -i -q '^my-added-header:[[:space:]]*added-value\r?$' "$output_dir/${slug}.headers"; then
            die "${gateway} via HAProxy pod ${pod} still served the routechange marker during ${phase} proof"
        fi
    done < "$scenario_dir/tunnels.tsv"
}

observe_routechange_dataplane() {
    local scenario_dir="$1"
    local route="$scenario_dir/httproute-intermediate-candidate.json"
    local output_dir="$scenario_dir/live-dataplane-intermediate"
    mkdir -p "$output_dir"
    local iteration=0 expected gateway service service_uid service_rv target_port pod uid pid port slug request_pid
    local -a request_pids=()
    expected="$(wc -l < "$scenario_dir/tunnels.tsv")"
    while true; do
        while IFS=$'\t' read -r gateway service service_uid service_rv target_port pod uid pid port; do
            pid_running "$pid" || return 2
        done < "$scenario_dir/tunnels.tsv"
        if kubectl get httproute route -n default -o json > "$route" 2>/dev/null &&
            jq -e 'any(.spec.rules[]?.backendRefs[]?.filters[]?;
                .type == "ResponseHeaderModifier" and
                any(.responseHeaderModifier.add[]?;
                    (.name | ascii_downcase) == "my-added-header" and .value == "added-value"))' \
                "$route" >/dev/null; then
            request_pids=()
            while IFS=$'\t' read -r gateway service service_uid service_rv target_port pod uid pid port; do
                slug="${gateway//\//_}-${pod}"
                [[ ! -f "$output_dir/${slug}.headers" ]] || continue
                (
                    local headers="${WORK_DIR}/routechange-${slug}-${iteration}.headers"
                    local body="${WORK_DIR}/routechange-${slug}-${iteration}.body"
                    local status="${WORK_DIR}/routechange-${slug}-${iteration}.status"
                    if curl --silent --show-error --max-time 0.5 \
                        --dump-header "$headers" --output "$body" --write-out '%{http_code}' \
                        --header 'Host: route.example.com' --header 'Connection: close' \
                        "http://127.0.0.1:${port}/${iteration}" > "$status" 2>/dev/null &&
                        [[ "$(<"$status")" == "200" ]] &&
                        rg -i -q '^my-added-header:[[:space:]]*added-value\r?$' "$headers"; then
                        cp "$headers" "$output_dir/${slug}.headers"
                        cp "$body" "$output_dir/${slug}.body"
                        cp "$status" "$output_dir/${slug}.status-code.txt"
                        date -u +%Y-%m-%dT%H:%M:%S.%NZ > "$output_dir/${slug}.observed-at.txt"
                    fi
                ) &
                request_pid=$!
                request_pids+=("$request_pid")
            done < "$scenario_dir/tunnels.tsv"
            for request_pid in "${request_pids[@]}"; do
                wait "$request_pid" 2>/dev/null || true
            done
        fi
        if [[ "$(find "$output_dir" -maxdepth 1 -name '*.headers' -type f | wc -l)" -eq "$expected" ]]; then
            revalidate_routechange_targets "$scenario_dir" intermediate
            cp "$route" "$scenario_dir/httproute-intermediate.json"
            jq -n --argjson expected "$expected" --arg observed_at "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" \
                '{proof: "per-Gateway per-pod HTTP 200 response header", expected_observations: $expected,
                  observed_observations: $expected, status_code: 200,
                  header: "my-added-header", value: "added-value",
                  observed_at: $observed_at, pass: true}' \
                > "$scenario_dir/live-dataplane-intermediate.json"
            return 0
        fi
        iteration=$((iteration + 1))
        sleep 0.1
    done
}

total_route_count() {
    kubectl get --raw '/apis/gateway.networking.k8s.io/v1/httproutes?limit=1' | \
        jq -er '(.items | length) + (.metadata.remainingItemCount // 0)'
}

validate_haptic_scale_routes() {
    local expected="$1"
    local output="$2"
    local snapshot="$3"
    local candidate="${output}.read"
    local gatewayclass="${output%.json}-gatewayclass.json"
    local gateway_json controller_name predicate_rc=0
    kubectl get httproutes.gateway.networking.k8s.io -A -o json > "$candidate" || \
        return "$READINESS_RESULT_EVIDENCE_INVALID"
    kubectl get gatewayclass haptic -o json > "$gatewayclass" || \
        return "$READINESS_RESULT_EVIDENCE_INVALID"
    jq -e '
        type == "object" and (.items | type) == "array" and
        all(.items[];
          ([.metadata.namespace, .metadata.name, .metadata.uid] |
           all(.[]; type == "string" and length > 0)) and
          (.metadata.generation | type) == "number" and .metadata.generation > 0 and
          (.spec.parentRefs | type) == "array" and
          all(.spec.parentRefs[]; (.name | type) == "string" and (.name | length) > 0) and
          ((.status == null) or
           ((.status | type) == "object" and
            ((.status.parents == null) or
             ((.status.parents | type) == "array" and
              all(.status.parents[];
                (.controllerName | type) == "string" and
                (.parentRef | type) == "object" and
                ((.conditions == null) or (.conditions | type) == "array")))))))
    ' "$candidate" >/dev/null || return "$READINESS_RESULT_EVIDENCE_INVALID"
    jq -e '
        type == "object" and (.items | type) == "array" and
        all(.items[];
          ([.metadata.namespace, .metadata.name, .metadata.uid] |
           all(.[]; type == "string" and length > 0)) and
          (.metadata.generation | type) == "number" and .metadata.generation > 0)
    ' "$snapshot" >/dev/null || return "$READINESS_RESULT_EVIDENCE_INVALID"
    gateway_json="$(printf '%s\n' "${GATEWAYS[@]}" |
        jq -R 'split("/") | select(length == 2 and all(.[]; length > 0)) |
            {namespace: .[0], name: .[1]}' | jq -e -s 'select(length > 0)')" || \
        return "$READINESS_RESULT_EVIDENCE_INVALID"
    controller_name="$(jq -er '
        select(type == "object") | .spec.controllerName |
        select(type == "string" and length > 0)
    ' "$gatewayclass")" || return "$READINESS_RESULT_EVIDENCE_INVALID"
    mv "$candidate" "$output" || return "$READINESS_RESULT_EVIDENCE_INVALID"
    jq -e --argjson gateways "$gateway_json" --argjson expected "$expected" --arg controller "$controller_name" \
        --slurpfile snapshot "$snapshot" '
        ($snapshot[0].items | map({key: (.metadata.namespace + "/" + .metadata.name), value: .}) | from_entries) as $snap |
        (.items | length) == $expected and
        all(.items[];
          . as $route |
          ($snap[$route.metadata.namespace + "/" + $route.metadata.name]) as $before |
          $before != null and $before.metadata.uid == $route.metadata.uid and
          ([$route.spec.parentRefs[]? |
             {namespace: (.namespace // $route.metadata.namespace), name,
              group: (.group // "gateway.networking.k8s.io"), kind: (.kind // "Gateway")}] |
             sort_by(.namespace, .name)) ==
            ([$gateways[] | . + {group: "gateway.networking.k8s.io", kind: "Gateway"}] |
             sort_by(.namespace, .name)) and
          ([$route.status.parents[]?] | length) == ($gateways | length) and
          all($gateways[];
            . as $gateway |
            ([$route.status.parents[]? |
              select(.controllerName == $controller and
                     .parentRef.name == $gateway.name and
                     (.parentRef.namespace // $route.metadata.namespace) == $gateway.namespace and
                     (.parentRef.group // "gateway.networking.k8s.io") == "gateway.networking.k8s.io" and
                     (.parentRef.kind // "Gateway") == "Gateway")] | length) == 1 and
            any($route.status.parents[]?;
              .controllerName == $controller and
                .parentRef.name == $gateway.name and
                (.parentRef.namespace // $route.metadata.namespace) == $gateway.namespace and
              any(.conditions[]?;
                .type == "Accepted" and .status == "True" and .reason == "Accepted" and
                .observedGeneration >= $before.metadata.generation) and
              any(.conditions[]?;
                .type == "ResolvedRefs" and .status == "True" and .reason == "ResolvedRefs" and
                .observedGeneration >= $before.metadata.generation))))
    ' "$output" >/dev/null || predicate_rc=$?
    if [[ $predicate_rc -eq 1 ]]; then
        return "$READINESS_RESULT_DEADLINE"
    fi
    [[ $predicate_rc -eq 0 ]] || return "$READINESS_RESULT_EVIDENCE_INVALID"
}

observe_haptic_scale_routes() {
    local attempt="$1"
    local observation="$2"
    local expected="$3"
    local output="$4"
    local snapshot="$5"
    local route_rc=0 pass=false
    validate_haptic_scale_routes "$expected" "$output" "$snapshot" || route_rc=$?
    if [[ $route_rc -eq "$READINESS_RESULT_EVIDENCE_INVALID" ]]; then
        record_readiness_callback_failure "$observation" "$attempt" route-status-evidence-invalid
        return "$READINESS_RESULT_EVIDENCE_INVALID"
    fi
    [[ $route_rc -eq 0 || $route_rc -eq "$READINESS_RESULT_DEADLINE" ]] || \
        return "$READINESS_RESULT_EVIDENCE_INVALID"
    [[ $route_rc -eq 0 ]] && pass=true
    jq -S -n --argjson attempt "$attempt" --argjson expected "$expected" \
        --argjson pass "$pass" --slurpfile routes "$output" '
        {attempt: $attempt, expected_routes: $expected,
         observed_routes: ($routes[0].items | length), pass: $pass}
    ' > "$observation" || return "$READINESS_RESULT_EVIDENCE_INVALID"
    return "$route_rc"
}

poll_for_haptic_scale_routes() {
    local expected="$1"
    local output="$2"
    local snapshot="$3"
    local monitor_container="$4"
    local deadline="$5"
    local poll_interval="$6"
    local report="$7"
    poll_for_readiness observe_haptic_scale_routes route-status-ready route-status-timeout \
        "$monitor_container" "$deadline" 1200 "$poll_interval" "$report" \
        "${report%.json}-observation.json" "$expected" "$output" "$snapshot"
}

capture_scale_route_snapshot() {
    local expected="$1"
    local output="$2"
    local gateway_json
    gateway_json="$(printf '%s\n' "${GATEWAYS[@]}" | jq -R 'split("/") | {namespace: .[0], name: .[1]}' | jq -s .)"
    kubectl get httproutes.gateway.networking.k8s.io -A -o json > "$output"
    jq -e --argjson gateways "$gateway_json" --argjson expected "$expected" '
        (.items | length) == $expected and
        ([.items[].metadata.uid] | unique | length) == $expected and
        all(.items[];
          . as $route |
          ($route.metadata.generation > 0) and
          ([$route.spec.parentRefs[]? |
             {namespace: (.namespace // $route.metadata.namespace), name,
              group: (.group // "gateway.networking.k8s.io"), kind: (.kind // "Gateway")}] |
             sort_by(.namespace, .name)) ==
            ([$gateways[] | . + {group: "gateway.networking.k8s.io", kind: "Gateway"}] |
             sort_by(.namespace, .name)) and
          (.spec.hostnames | length) == 1 and
          (.spec.hostnames[0] | ascii_downcase | endswith(".example.com")))
    ' "$output" >/dev/null || die "scale route snapshot is not the exact HAPTIC workload"
}

prove_scale_live_host_map() {
    local scenario_dir="$1"
    local expected_routes="$2"
    local routes="$scenario_dir/routes-at-scale-snapshot.json"
    local inventory="$scenario_dir/map-inventory-at-scale.json"
    local services="$scenario_dir/gateway-services-at-scale.json"
    local expected_hosts="$scenario_dir/expected-host-map-keys.txt"
    local expected_entries="$scenario_dir/expected-host-map-entries.txt"
    local host_map_identifier host_map_reference expected_entry_count reference_rc=0 resolve_rc=0
    jq -e --argjson expected "$expected_routes" '
        (.items | length) == $expected and
        all(.items[]; (.spec.hostnames | length) == 1 and
            (.spec.hostnames[0] | ascii_downcase | endswith(".example.com")))
    ' "$routes" >/dev/null || return "$READINESS_RESULT_EVIDENCE_INVALID"
    capture_gateway_service_targets "$services" || return "$READINESS_RESULT_EVIDENCE_INVALID"
    jq -r --slurpfile services "$services" '
        .items[].spec.hostnames[0] as $hostname |
        $services[0][] | (($hostname | ascii_downcase) + ":" + (.target_port | tostring))
    ' "$routes" | sort > "$expected_hosts" || return "$READINESS_RESULT_EVIDENCE_INVALID"
    awk '{print $1, $1}' "$expected_hosts" > "$expected_entries" || \
        return "$READINESS_RESULT_EVIDENCE_INVALID"
    expected_entry_count=$((expected_routes * ${#GATEWAYS[@]}))
    [[ "$(wc -l < "$expected_entries")" -eq "$expected_entry_count" ]] || \
        return "$READINESS_RESULT_EVIDENCE_INVALID"
    [[ "$(sort -u "$expected_entries" | wc -l)" -eq "$expected_entry_count" ]] || \
        return "$READINESS_RESULT_EVIDENCE_INVALID"
    host_map_reference="$(jq -er '[.maps[] | select(.map_name == "host.map")] |
        select(length == 1) | .[0].path | select(length > 0)' "$inventory")" || reference_rc=$?
    if [[ $reference_rc -ne 0 ]]; then
        [[ $reference_rc -eq 1 || $reference_rc -eq 4 ]] && return "$READINESS_RESULT_DEADLINE"
        return "$READINESS_RESULT_EVIDENCE_INVALID"
    fi
    host_map_identifier="$(resolve_map_runtime_identifier "$host_map_reference")" || resolve_rc=$?
    if [[ $resolve_rc -ne 0 ]]; then
        [[ $resolve_rc -eq 1 ]] && return "$READINESS_RESULT_DEADLINE"
        return "$READINESS_RESULT_EVIDENCE_INVALID"
    fi

    local live_dir="$scenario_dir/live-host-map"
    mkdir -p "$live_dir" || return "$READINESS_RESULT_EVIDENCE_INVALID"
    local pod raw_count suffix pod_names
    pod_names="$(jq -er '.pods | select(type == "array" and length > 0) | .[] |
        .name | select(type == "string" and length > 0)' "$inventory")" || \
        return "$READINESS_RESULT_EVIDENCE_INVALID"
    for suffix in "" "-after"; do
        while read -r pod; do
            capture_haproxy_runtime_map_entries "$pod" "$host_map_identifier" \
                "$live_dir/${pod}${suffix}.runtime" "$live_dir/${pod}${suffix}.entries" || \
                return "$READINESS_RESULT_EVIDENCE_INVALID"
            raw_count="$(wc -l < "$live_dir/${pod}${suffix}.entries")" || \
                return "$READINESS_RESULT_EVIDENCE_INVALID"
            [[ "$raw_count" -eq "$expected_entry_count" ]] || return "$READINESS_RESULT_DEADLINE"
            cmp -s "$expected_entries" "$live_dir/${pod}${suffix}.entries" || {
                diff -u "$expected_entries" "$live_dir/${pod}${suffix}.entries" \
                    > "$live_dir/${pod}${suffix}.diff" || true
                return "$READINESS_RESULT_DEADLINE"
            }
            : > "$live_dir/${pod}${suffix}.diff" || return "$READINESS_RESULT_EVIDENCE_INVALID"
        done <<< "$pod_names"
    done
    jq -n --arg identifier "$host_map_identifier" --argjson expected_routes "$expected_routes" \
        --argjson expected_entries "$expected_entry_count" \
        --slurpfile inventory "$inventory" \
        --slurpfile services "$services" \
        '{runtime_map_identifier: $identifier, expected_unique_hostnames: $expected_routes,
          expected_port_scoped_entries: $expected_entries, gateway_services: $services[0],
          referenced_map_inventory: $inventory[0],
          runtime_reads_per_pod: 2,
          exact_runtime_entries_on_every_pod: true, pass: true}' \
        > "$scenario_dir/live-host-map.json" || return "$READINESS_RESULT_EVIDENCE_INVALID"
}

observe_scale_live_host_map() {
    local attempt="$1"
    local observation="$2"
    local scenario_dir="$3"
    local expected_routes="$4"
    local proof_rc=0 pass=false
    prove_scale_live_host_map "$scenario_dir" "$expected_routes" || proof_rc=$?
    if [[ $proof_rc -eq "$READINESS_RESULT_EVIDENCE_INVALID" ]]; then
        record_readiness_callback_failure "$observation" "$attempt" runtime-map-evidence-invalid
        return "$READINESS_RESULT_EVIDENCE_INVALID"
    fi
    [[ $proof_rc -eq 0 || $proof_rc -eq "$READINESS_RESULT_DEADLINE" ]] || \
        return "$READINESS_RESULT_EVIDENCE_INVALID"
    [[ $proof_rc -eq 0 ]] && pass=true
    jq -S -n --argjson attempt "$attempt" --argjson pass "$pass" \
        '{attempt: $attempt, runtime_reads_completed: $pass, pass: $pass}' \
        > "$observation" || return "$READINESS_RESULT_EVIDENCE_INVALID"
    return "$proof_rc"
}

poll_for_scale_live_host_map() {
    local scenario_dir="$1"
    local expected_routes="$2"
    local monitor_container="$3"
    local deadline="$4"
    local poll_interval="$5"
    local report="$6"
    poll_for_readiness observe_scale_live_host_map runtime-map-ready runtime-map-timeout \
        "$monitor_container" "$deadline" 1200 "$poll_interval" "$report" \
        "${report%.json}-observation.json" "$scenario_dir" "$expected_routes"
}

capture_haproxy_runtime_map_entries() {
    local pod="$1"
    local map_path="$2"
    local raw_output="$3"
    local entries_output="$4"
    kubectl exec -n "$RELEASE_NAMESPACE" "$pod" -c haproxy -- sh -c \
        'printf "@1 show map %s\n" "$1" | socat - UNIX-CONNECT:/etc/haproxy/haproxy-master.sock' \
        runtime-map "$map_path" > "$raw_output" || return 1
    awk 'NF >= 3 && $1 ~ /^0x[[:xdigit:]]+$/ {print $2, $3}' "$raw_output" |
        sort > "$entries_output"
}

resolve_map_runtime_identifier() {
    local map_path="$1"
    local config="${2:-${BENCH_OUTPUT_DIR}/cluster/effective-template-config.json}"
    local maps_dir
    maps_dir="$(jq -er '.spec.dataplane.mapsDir |
        select(type == "string" and startswith("/"))' "$config")" || return 2
    python3 - "$map_path" "$maps_dir" <<'PY'
import os
import sys

map_path, maps_dir = sys.argv[1:]
maps_dir = os.path.normpath(maps_dir)
base_dir = os.path.dirname(maps_dir)
if os.path.isabs(map_path):
    identifier = os.path.normpath(map_path)
    storage_path = identifier
elif "/" in map_path:
    identifier = os.path.normpath(map_path)
    storage_path = os.path.normpath(os.path.join(base_dir, identifier))
else:
    identifier = os.path.join(os.path.basename(maps_dir), map_path)
    storage_path = os.path.normpath(os.path.join(base_dir, identifier))
if storage_path == maps_dir or os.path.commonpath((maps_dir, storage_path)) != maps_dir:
    raise SystemExit(1)
print(identifier)
PY
}

capture_host_map_semantic_token() {
    local inventory="$1"
    local output="$2"
    local map_path runtime_identifier token_rc=0
    map_path="$(jq -er '[.maps[] | select(.map_name == "host.map")] |
        select(length == 1) | .[0].path | select(length > 0)' "$inventory")" || token_rc=$?
    if [[ $token_rc -ne 0 ]]; then
        [[ $token_rc -eq 1 || $token_rc -eq 4 ]] && return "$READINESS_RESULT_DEADLINE"
        return "$READINESS_RESULT_EVIDENCE_INVALID"
    fi
    runtime_identifier="$(resolve_map_runtime_identifier "$map_path")" || token_rc=$?
    [[ $token_rc -eq 0 ]] || return "$token_rc"
    jq -Se --arg runtime_identifier "$runtime_identifier" '
        . as $inventory |
        ([.maps[] | select(.map_name == "host.map")] |
         select(length == 1) | .[0]) as $map |
        {pods: $inventory.pods, runtime_map_identifier: $runtime_identifier, checksum: $map.checksum,
         deployed_to_pods: $map.deployed_to_pods}
    ' "$inventory" > "$output" || return "$READINESS_RESULT_EVIDENCE_INVALID"
}

write_scale_readiness_timeout() {
    local scenario_dir="$1"
    local expected_routes="$2"
    local failure_stage="$3"
    local haproxycfg="$4"
    local readiness_report="$5"
    local baseline_generation baseline_checksum metadata_line reason_code stages_json
    local cfg_json=null readiness_attempts
    metadata_line="$(jq -er --arg stage "$failure_stage" '
        . as $metadata | [$metadata[] | select(.stage == $stage)] | select(length == 1) |
        [.[0].reason_code, ($metadata | map(.stage) | tojson)] | @tsv
    ' <<< "$SCALE_READINESS_STAGE_METADATA")" || die "unknown scale readiness stage: ${failure_stage}"
    IFS=$'\t' read -r reason_code stages_json <<< "$metadata_line"
    baseline_generation="$(jq -er '.metadata.generation' "$scenario_dir/haproxycfg-baseline.json")" || \
        die "scale readiness baseline generation is invalid"
    baseline_checksum="$(<"$scenario_dir/haproxycfg-baseline-checksum.txt")" || \
        die "scale readiness baseline checksum is invalid"
    readiness_attempts="$(jq -er '.attempts | select(type == "number" and . >= 0)' \
        "$readiness_report")" || die "scale readiness attempt evidence is invalid"
    if [[ "$readiness_attempts" -eq 0 &&
        ( "$failure_stage" == "initial-exact-current" ||
          "$failure_stage" == "post-live-exact-current" ) ]]; then
        cfg_json=null
    elif [[ -s "$haproxycfg" ]]; then
        cfg_json="$(jq -c 'if type == "object" or . == null then . else error("invalid") end' \
            "$haproxycfg")" || \
            die "scale readiness HAProxyCfg snapshot is invalid"
    elif [[ "$readiness_attempts" -gt 0 ]]; then
        die "scale readiness HAProxyCfg snapshot is missing after an observation"
    fi
    # The snapshot is megabytes at scale: a file, not an argument, or jq dies
    # with "argument list too long" and the evidence is lost.
    local cfg_file="$scenario_dir/readiness-haproxycfg-snapshot.json"
    printf '%s\n' "$cfg_json" > "$cfg_file"
    jq -S -n \
        --arg baseline_checksum "$baseline_checksum" \
        --arg reason_code "$reason_code" \
        --arg failure_stage "$failure_stage" \
        --argjson baseline_generation "$baseline_generation" \
        --argjson routes "$expected_routes" \
        --argjson stages "$stages_json" \
        --slurpfile cfg_file "$cfg_file" \
        --slurpfile readiness "$readiness_report" '
        $cfg_file[0] as $cfg |
        ($stages | index($failure_stage)) as $failed |
        select($failed != null) |
        {routes_with_snapshot_generation_haptic_status: $routes,
         baseline: {checksum: $baseline_checksum, generation: $baseline_generation},
         stage_observation: {
           attempted: ($readiness[0].attempts > 0),
           haproxycfg_snapshot: (if ($cfg | type) == "object" then {
             checksum: ($cfg.spec.checksum // null),
             generation: ($cfg.metadata.generation // null),
             deployed_checksums: [$cfg.status.deployedToPods[]?.checksum]
           } else null end)
         },
         at_scale: (if ($cfg | type) == "object" then {
           checksum: ($cfg.spec.checksum // null),
           generation: ($cfg.metadata.generation // null),
           deployed_checksums: [$cfg.status.deployedToPods[]?.checksum]
         } else null end),
         reason_code: $reason_code,
         failure_stage: $failure_stage,
         deadline_evidence: $readiness[0],
         completed_gates: ($stages | to_entries | map({
           key: (.value | gsub("-"; "_")), value: (.key < $failed)
         }) | from_entries),
         steady_window_started: false,
         pass: false}
    ' > "$scenario_dir/scale-readiness.json"
    jq -e --arg reason_code "$reason_code" --arg failure_stage "$failure_stage" '
        .reason_code == $reason_code and .failure_stage == $failure_stage and
        .pass == false and
        .steady_window_started == false and
        .deadline_evidence.reason_code == $reason_code and
        .deadline_evidence.outcome == "deadline" and
        .deadline_evidence.evidence_valid == true and
        .deadline_evidence.pass == false and
        .deadline_evidence.deadline_reached == true and
        .deadline_evidence.attempts >= 0 and
        .stage_observation.attempted == (.deadline_evidence.attempts > 0) and
        (.completed_gates | type) == "object"
    ' "$scenario_dir/scale-readiness.json" >/dev/null || \
        die "scale readiness timeout evidence is incomplete"
}

classify_scale_readiness_result() {
    local scenario_dir="$1"
    local expected_routes="$2"
    local result="$3"
    local failure_stage="$4"
    local evidence="$5"
    local report="$6"
    if [[ $result -eq 0 ]]; then
        return 0
    fi
    if [[ $result -eq "$READINESS_RESULT_DEADLINE" ]]; then
        write_scale_readiness_timeout "$scenario_dir" "$expected_routes" "$failure_stage" \
            "$evidence" "$report"
        return "$READINESS_RESULT_DEADLINE"
    fi
    local invalid_message
    invalid_message="$(jq -er --arg stage "$failure_stage" '
        [.[] | select(.stage == $stage)] | select(length == 1) | .[0].invalid_message
    ' <<< "$SCALE_READINESS_STAGE_METADATA")" || die "unknown scale readiness stage: ${failure_stage}"
    die "$invalid_message"
}

wait_for_scale_dataplane() {
    local scenario_dir="$1"
    local expected_routes="$2"
    local startup_deadline="$3"
    local baseline_generation baseline_checksum baseline_uid
    baseline_generation="$(jq -er '.metadata.generation' "$scenario_dir/haproxycfg-baseline.json")" || \
        die "scale baseline generation evidence is invalid"
    baseline_checksum="$(<"$scenario_dir/haproxycfg-baseline-checksum.txt")" || \
        die "scale baseline checksum evidence is invalid"
    baseline_uid="$(jq -er '.metadata.uid | select(length > 0)' "$scenario_dir/haproxycfg-baseline.json")" || \
        die "scale baseline identity evidence is invalid"
    mkdir -p "$scenario_dir/at-scale" || die "could not create scale readiness artifact directory"
    local unavailable_cfg="$scenario_dir/at-scale/haproxycfg-unavailable.json"
    printf 'null\n' > "$unavailable_cfg" || die "could not initialize scale readiness evidence"
    local route_report="$scenario_dir/at-scale/route-status-readiness.json"
    local readiness_rc=0
    poll_for_haptic_scale_routes "$expected_routes" "$scenario_dir/routes-at-scale.json" \
        "$scenario_dir/routes-at-scale-snapshot.json" "$active_workload_container" \
        "$startup_deadline" "$SCALE_READINESS_POLL_INTERVAL_SECONDS" "$route_report" || readiness_rc=$?
    classify_scale_readiness_result "$scenario_dir" "$expected_routes" "$readiness_rc" \
        route-status "$unavailable_cfg" "$route_report" || return "$READINESS_RESULT_DEADLINE"
    local initial_convergence="$scenario_dir/at-scale/haproxycfg-convergence.json"
    local convergence_rc=0
    poll_for_haproxycfg_converged "" "$scenario_dir/at-scale/haproxycfg.json" \
        "$active_workload_container" "$startup_deadline" \
        "$SCALE_READINESS_POLL_INTERVAL_SECONDS" "$initial_convergence" "$baseline_uid" \
        "$baseline_generation" "__ALLOW_DEPLOYED_LAG__" snapshot-deployed-timeout || convergence_rc=$?
    classify_scale_readiness_result "$scenario_dir" "$expected_routes" "$convergence_rc" \
        initial-exact-current "$scenario_dir/at-scale/haproxycfg.json" "$initial_convergence" || \
        return "$READINESS_RESULT_DEADLINE"
    jq -e --arg baseline_checksum "$baseline_checksum" --arg baseline_uid "$baseline_uid" \
        --argjson baseline_generation "$baseline_generation" '
        .metadata.uid == $baseline_uid and
        .spec.checksum != $baseline_checksum and .metadata.generation > $baseline_generation
    ' "$scenario_dir/at-scale/haproxycfg.json" >/dev/null || \
        die "at-scale HAProxyCfg is not a new generation tied to the scale workload"
    local initial_map_report="$scenario_dir/at-scale/referenced-map-readiness.json"
    local map_rc=0
    poll_for_referenced_map_inventory "$scenario_dir/at-scale/haproxycfg.json" \
        "$scenario_dir/at-scale/haproxycfg-pods.json" "$scenario_dir/map-inventory-at-scale.json" \
        "$active_workload_container" "$startup_deadline" "$SCALE_READINESS_POLL_INTERVAL_SECONDS" \
        "$initial_map_report" true || map_rc=$?
    classify_scale_readiness_result "$scenario_dir" "$expected_routes" "$map_rc" \
        initial-referenced-map "$scenario_dir/at-scale/haproxycfg.json" "$initial_map_report" || \
        return "$READINESS_RESULT_DEADLINE"
    local proof_attempts=0
    local semantic_started_seconds=$SECONDS
    local semantic_started_at
    semantic_started_at="$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" || \
        die "could not timestamp semantic-token readiness"
    local semantic_report="$scenario_dir/at-scale/semantic-token-readiness.json"
    local semantic_observation="$scenario_dir/at-scale/semantic-token-observation.json"
    printf 'null\n' > "$semantic_observation" || die "could not initialize semantic-token evidence"
    while true; do
        proof_attempts=$((proof_attempts + 1))
        local runtime_report="$scenario_dir/at-scale/runtime-map-readiness-${proof_attempts}.json"
        local runtime_rc=0
        poll_for_scale_live_host_map "$scenario_dir" "$expected_routes" \
            "$active_workload_container" "$startup_deadline" \
            "$SCALE_READINESS_POLL_INTERVAL_SECONDS" "$runtime_report" || runtime_rc=$?
        classify_scale_readiness_result "$scenario_dir" "$expected_routes" "$runtime_rc" \
            runtime-host-map "$scenario_dir/at-scale/haproxycfg.json" "$runtime_report" || \
            return "$READINESS_RESULT_DEADLINE"
        local after_live_convergence="$scenario_dir/at-scale/haproxycfg-after-live-proof-convergence.json"
        convergence_rc=0
        poll_for_haproxycfg_converged "" \
            "$scenario_dir/at-scale/haproxycfg-after-live-proof.json" \
            "$active_workload_container" "$startup_deadline" \
            "$SCALE_READINESS_POLL_INTERVAL_SECONDS" "$after_live_convergence" "$baseline_uid" \
            "$baseline_generation" "__ALLOW_DEPLOYED_LAG__" snapshot-deployed-timeout || convergence_rc=$?
        classify_scale_readiness_result "$scenario_dir" "$expected_routes" "$convergence_rc" \
            post-live-exact-current "$scenario_dir/at-scale/haproxycfg-after-live-proof.json" \
            "$after_live_convergence" || \
            return "$READINESS_RESULT_DEADLINE"
        jq -e --arg baseline_uid "$baseline_uid" '.metadata.uid == $baseline_uid' \
            "$scenario_dir/at-scale/haproxycfg-after-live-proof.json" >/dev/null || \
            die "HAProxyCfg identity changed during the live scale dataplane proof"
        local after_live_map_report="$scenario_dir/at-scale/referenced-map-after-live-proof-readiness.json"
        map_rc=0
        poll_for_referenced_map_inventory "$scenario_dir/at-scale/haproxycfg-after-live-proof.json" \
            "$scenario_dir/at-scale/haproxycfg-after-live-proof-pods.json" \
            "$scenario_dir/at-scale/map-inventory-after-live-proof.json" \
            "$active_workload_container" "$startup_deadline" "$SCALE_READINESS_POLL_INTERVAL_SECONDS" \
            "$after_live_map_report" true || map_rc=$?
        classify_scale_readiness_result "$scenario_dir" "$expected_routes" "$map_rc" \
            post-live-referenced-map "$scenario_dir/at-scale/haproxycfg-after-live-proof.json" \
            "$after_live_map_report" || \
            return "$READINESS_RESULT_DEADLINE"
        capture_host_map_semantic_token "$scenario_dir/map-inventory-at-scale.json" \
            "$scenario_dir/at-scale/host-map-token.json" || \
            die "at-scale referenced maps do not contain one valid host.map token"
        local after_token_rc=0 semantic_pass=false
        capture_host_map_semantic_token "$scenario_dir/at-scale/map-inventory-after-live-proof.json" \
            "$scenario_dir/at-scale/host-map-token-after-live-proof.json" || after_token_rc=$?
        [[ $after_token_rc -ne "$READINESS_RESULT_EVIDENCE_INVALID" ]] || \
            die "current host.map semantic-token evidence is invalid"
        if [[ $after_token_rc -eq 0 ]] && cmp -s "$scenario_dir/at-scale/host-map-token.json" \
            "$scenario_dir/at-scale/host-map-token-after-live-proof.json"; then
            semantic_pass=true
        fi
        jq -S -n --argjson attempt "$proof_attempts" --argjson pass "$semantic_pass" \
            --slurpfile before "$scenario_dir/at-scale/host-map-token.json" \
            --slurpfile after "$scenario_dir/at-scale/host-map-token-after-live-proof.json" '
            {attempt: $attempt, before: ($before[0] // null), after: ($after[0] // null), pass: $pass}
        ' > "$semantic_observation" || die "host.map semantic-token evidence is invalid"
        if [[ "$semantic_pass" == "true" ]] && (( SECONDS < startup_deadline )); then
            write_readiness_poll_report "$semantic_report" converged semantic-token-stable \
                "$proof_attempts" "$semantic_started_at" "$semantic_started_seconds" \
                "$startup_deadline" "$SCALE_READINESS_POLL_INTERVAL_SECONDS" "$semantic_observation" || \
                die "host.map semantic-token evidence is invalid"
            break
        fi
        if (( SECONDS >= startup_deadline )); then
            write_readiness_poll_report "$semantic_report" deadline semantic-token-timeout \
                "$proof_attempts" "$semantic_started_at" "$semantic_started_seconds" \
                "$startup_deadline" "$SCALE_READINESS_POLL_INTERVAL_SECONDS" "$semantic_observation" || \
                die "host.map semantic-token evidence is invalid"
            write_scale_readiness_timeout "$scenario_dir" "$expected_routes" semantic-token \
                "$scenario_dir/at-scale/haproxycfg-after-live-proof.json" "$semantic_report"
            return "$READINESS_RESULT_DEADLINE"
        fi
        cp "$scenario_dir/at-scale/haproxycfg-after-live-proof.json" \
            "$scenario_dir/at-scale/haproxycfg.json" || die "could not retain current HAProxyCfg evidence"
        cp "$scenario_dir/at-scale/haproxycfg-after-live-proof-pods.json" \
            "$scenario_dir/at-scale/haproxycfg-pods.json" || die "could not retain current HAProxy pod evidence"
        cp "$scenario_dir/at-scale/map-inventory-after-live-proof-objects.json" \
            "$scenario_dir/map-inventory-at-scale-objects.json" || \
            die "could not retain current referenced-map objects"
        cp "$scenario_dir/at-scale/map-inventory-after-live-proof.json" \
            "$scenario_dir/map-inventory-at-scale.json" || die "could not retain current map inventory"
        sleep "$SCALE_READINESS_POLL_INTERVAL_SECONDS" || die "scale readiness polling was interrupted"
    done
    jq -e --arg baseline_checksum "$baseline_checksum" --arg baseline_uid "$baseline_uid" \
        --argjson baseline_generation "$baseline_generation" '
        .metadata.uid == $baseline_uid and
        .spec.checksum != $baseline_checksum and .metadata.generation > $baseline_generation
    ' "$scenario_dir/at-scale/haproxycfg.json" >/dev/null || \
        die "final at-scale HAProxyCfg is not a new generation of the baseline resource"
    extract_loadbalancer_identities "$scenario_dir/at-scale/haproxycfg-pods.json" \
        "$scenario_dir/at-scale/haproxy-identities.json"
    extract_loadbalancer_identities "$scenario_dir/at-scale/haproxycfg-after-live-proof-pods.json" \
        "$scenario_dir/at-scale/haproxy-identities-after-live-proof.json"
    cmp -s "$scenario_dir/at-scale/haproxy-identities.json" \
        "$scenario_dir/at-scale/haproxy-identities-after-live-proof.json" || \
        die "HAProxy fleet identity changed during the live scale dataplane proof"
    capture_gateway_service_targets "$scenario_dir/at-scale/gateway-services-after-live-proof.json" || \
        die "Gateway Service targets disappeared during the live scale dataplane proof"
    if ! jq -S 'map(del(.resource_version))' "$scenario_dir/gateway-services-at-scale.json" | \
        cmp - <(jq -S 'map(del(.resource_version))' \
            "$scenario_dir/at-scale/gateway-services-after-live-proof.json"); then
        die "Gateway Service identity or targetPort changed during the live scale dataplane proof"
    fi
    jq -n \
        --arg baseline_checksum "$baseline_checksum" \
        --argjson baseline_generation "$baseline_generation" \
        --argjson routes "$expected_routes" \
        --argjson proof_attempts "$proof_attempts" \
        --slurpfile cfg "$scenario_dir/at-scale/haproxycfg.json" \
        --slurpfile initial_convergence "$initial_convergence" \
        --slurpfile after_live_convergence "$scenario_dir/at-scale/haproxycfg-after-live-proof-convergence.json" \
        '($cfg[0].spec.checksum) as $at_scale_checksum |
         ($cfg[0].metadata.generation) as $at_scale_generation |
         {routes_with_snapshot_generation_haptic_status: $routes,
          baseline: {checksum: $baseline_checksum, generation: $baseline_generation},
          at_scale: {checksum: $at_scale_checksum, generation: $at_scale_generation},
          checksum_changed: ($at_scale_checksum != $baseline_checksum),
          deployed_to_exact_current_fleet: true, auxiliary_set_resolved: true,
          exact_runtime_host_map_on_every_pod: true,
          host_map_semantic_token_stable: true,
          runtime_map_proof_attempts: $proof_attempts,
          haproxycfg_convergence: {
            initial: $initial_convergence[0],
            after_live_proof: $after_live_convergence[0]
          },
          steady_window_started: false,
          pass: true}' \
        > "$scenario_dir/scale-readiness.json" || die "could not write scale readiness evidence"
}

validate_probe_evidence() {
    local scenario_dir="$1"
    jq '
        . as $analysis |
        ([.expectations.gateway_names[] as $gateway |
          range(0; .expectations.routes) as $iteration |
          "\($gateway)\u0000\($iteration)"] | sort) as $expected |
        ([.samples[] | "\(.gateway)\u0000\(.iter)"]) as $samples |
        ([.failures[].code] - ["unexpected_statuses"]) as $structural_failures |
        {expected_samples: ($expected | length), observed_samples: ($samples | length),
         unique_samples: ($samples | unique | length),
         unexpected_statuses: (.errors | length), structural_failures: $structural_failures,
         valid: ((.expectations.samples == ($expected | length)) and
                 (.observed.duplicate_samples == 0) and
                 (($samples | sort) == $expected) and
                 (($samples | unique | length) == ($samples | length)) and
                 all(.errors[]; . as $error |
                   ($error.gateway | type) == "string" and
                   ($error.iter | type) == "number" and ($error.iter | floor) == $error.iter and
                   $error.iter >= 0 and $error.iter < $analysis.expectations.routes and
                   ($analysis.expectations.gateway_names | index($error.gateway)) != null and
                   ($error.status | type) == "number" and
                   $error.status >= 100 and $error.status <= 599) and
                 ($structural_failures | length) == 0)}
    ' "$scenario_dir/analysis.json" > "$scenario_dir/evidence.json"
    jq -e '.valid == true' "$scenario_dir/evidence.json" >/dev/null
}

run_probe() {
    local scenario_dir="${BENCH_OUTPUT_DIR}/probe"
    mkdir -p "$scenario_dir"
    record_event scenario-start probe
    assert_upstream_backend_absent "$scenario_dir"
    capture_state "$scenario_dir/before"
    capture_supervised_children "$scenario_dir/before"
    validate_supervised_child_baseline "$scenario_dir"
    capture_haproxycfg_baseline "$scenario_dir"
    # After the baseline: cleanup deletes the backend again, and the
    # post-scenario check expects the configuration back at this baseline.
    prewarm_upstream_backend "$scenario_dir" probe
    local workload_start workload_end workload_duration expected_samples command_rc=0
    expected_samples=$((BENCH_PROBE_ROUTES * ${#GATEWAYS[@]}))

    local -a upstream_command=(
        /gatewayapi-probe
        "--gateways=${BENCH_GATEWAYS}"
        "--routes=${BENCH_PROBE_ROUTES}"
    )
    create_workload_container probe gatewayapi-probe \
        "--gateways=${BENCH_GATEWAYS}" "--routes=${BENCH_PROBE_ROUTES}"
    local -a command=(
        timeout --foreground --kill-after=30s "$BENCH_PROBE_TIMEOUT"
        docker start --attach "$active_workload_container"
    )
    printf '%q ' "${command[@]}" > "$scenario_dir/command.txt"
    printf '\n' >> "$scenario_dir/command.txt"
    printf '%q ' "${upstream_command[@]}" > "$scenario_dir/upstream-command.txt"
    printf '\n' >> "$scenario_dir/upstream-command.txt"
    date -u +%Y-%m-%dT%H:%M:%S.%NZ > "$scenario_dir/workload-start.txt"
    date +%s.%N > "$scenario_dir/workload-start-epoch.txt"
    record_event workload-start probe
    run_logged "$scenario_dir/upstream.log" "$scenario_dir/exit-code.txt" "${command[@]}" || command_rc=$?
    date -u +%Y-%m-%dT%H:%M:%S.%NZ > "$scenario_dir/workload-end.txt"
    date +%s.%N > "$scenario_dir/workload-end-epoch.txt"
    record_event workload-end probe
    [[ $command_rc -eq 0 ]] || die "upstream probe failed"
    finish_workload_container "$scenario_dir" 0
    workload_start="$(<"$scenario_dir/workload-start-epoch.txt")"
    workload_end="$(<"$scenario_dir/workload-end-epoch.txt")"
    workload_duration="$(awk -v start="$workload_start" -v end="$workload_end" \
        'BEGIN { printf "%.9f", end - start }')"
    printf '%s\n' "$workload_duration" > "$scenario_dir/workload-duration-seconds.txt"

    local success_count unexpected_count
    success_count="$(rg -c 'probe completed: 200' "$scenario_dir/upstream.log" || true)"
    unexpected_count="$(rg -c 'unexpected status code' "$scenario_dir/upstream.log" || true)"
    success_count="${success_count:-0}"
    unexpected_count="${unexpected_count:-0}"
    printf '%s\n' "$success_count" > "$scenario_dir/success-samples.txt"
    printf '%s\n' "$unexpected_count" > "$scenario_dir/unexpected-statuses.txt"
    local analyzer="${PROJECT_ROOT}/scripts/analyze-gateway-api-bench.py"
    local -a analyzer_args=(
        python3 "$analyzer"
        --log "$scenario_dir/upstream.log"
        --output "$scenario_dir/analysis.json"
        --expected-samples "$expected_samples"
        --expected-routes "$BENCH_PROBE_ROUTES"
        --expected-gateways "${#GATEWAYS[@]}"
        --metadata "${BENCH_OUTPUT_DIR}/metadata.json"
    )
    local gateway
    for gateway in "${GATEWAYS[@]}"; do
        analyzer_args+=(--expected-gateway "$gateway")
    done
    local analyzer_rc=0
    "${analyzer_args[@]}" || analyzer_rc=$?
    printf '%d\n' "$analyzer_rc" > "$scenario_dir/analyzer-exit-code.txt"
    [[ $analyzer_rc -eq 0 || $analyzer_rc -eq 1 ]] || die "probe analyzer could not process the run"
    jq -e '.pass == true or .pass == false' "$scenario_dir/analysis.json" >/dev/null || \
        die "probe analyzer did not emit a verdict"
    validate_probe_evidence "$scenario_dir" || \
        die "probe output is incomplete or structurally invalid"
    local zero_status_quality
    zero_status_quality="$(jq -r '.pass' "$scenario_dir/analysis.json")"
    if [[ ( "$zero_status_quality" == "true" && $analyzer_rc -ne 0 ) ||
        ( "$zero_status_quality" == "false" && $analyzer_rc -ne 1 ) ]]; then
        die "probe analyzer exit code and verdict disagree"
    fi
    jq --argjson unexpected_statuses "$unexpected_count" '
        .probe_log_analysis_pass = .pass |
        .upstream_program = {pass: true, exit_code: 0, evidence_complete: true} |
        .unexpected_status_quality = {
          pass: ($unexpected_statuses == 0), count: $unexpected_statuses
        } |
        .haptic_non_vacuity = {applicable: false, pass: null} |
        .haptic_scenario_quality = {pass: false, measurement_complete: false} |
        .measurement_valid = false |
        .pass = false
    ' "$scenario_dir/analysis.json" > "$scenario_dir/analysis.json.tmp"
    mv "$scenario_dir/analysis.json.tmp" "$scenario_dir/analysis.json"

    wait_for_no_benchmark_routes
    cleanup_upstream_backend "$scenario_dir"
    wait_for_haproxycfg_baseline "$scenario_dir"
    capture_state "$scenario_dir/after"
    capture_supervised_children "$scenario_dir/after"
    capture_prometheus_range "$scenario_dir" "$workload_start" "$workload_end" prometheus-range \
        "$scenario_dir/before/haptic-identities.json" false
    verify_identity_unchanged "$scenario_dir/before/haptic-identities.json" \
        "$scenario_dir/after/haptic-identities.json" "$scenario_dir/identity.diff"
    analyze_resources "$scenario_dir" "$scenario_dir/prometheus-range" true
    attach_resource_analysis "$scenario_dir"
    jq '
        .measurement_valid = true |
        .haptic_scenario_quality = {
          pass: (.unexpected_status_quality.pass and (.resource_analysis.pass != false)),
          measurement_complete: true,
          unexpected_status_quality: .unexpected_status_quality,
          resource_analysis: .resource_analysis
        } |
        .pass = .haptic_scenario_quality.pass
    ' "$scenario_dir/analysis.json" > "$scenario_dir/analysis.json.tmp"
    mv "$scenario_dir/analysis.json.tmp" "$scenario_dir/analysis.json"
    attach_supervised_child_continuity "$scenario_dir"
    attach_lifecycle_outcome_quality "$scenario_dir"
    capture_scenario_logs "$scenario_dir"
    record_event scenario-complete probe
}

analyze_routechange_requests() {
    local scenario_dir="$1"
    local log_file="$scenario_dir/upstream.log"
    local rows_file="$scenario_dir/request-counts.tsv"
    local output_file="$scenario_dir/request-counts.json"
    local expected_json terminal_lines
    expected_json="$(printf '%s\n' "${GATEWAYS[@]}" | jq -R . | jq -sc 'sort')"
    terminal_lines="$(rg -c 'test complete' "$log_file" || true)"
    terminal_lines="${terminal_lines:-0}"
    printf '%s\n' "$terminal_lines" > "$scenario_dir/terminal-report-lines.txt"
    awk '
        /test complete/ {
            gateway = ""
            requests = ""
            for (i = 1; i <= NF; i++) {
                if ($i ~ /^gateway=/) {
                    gateway = substr($i, length("gateway=") + 1)
                } else if ($i ~ /^requests=/) {
                    requests = substr($i, length("requests=") + 1)
                }
            }
            if (gateway != "" && requests ~ /^[0-9]+$/) {
                print gateway "\t" requests
            }
        }
    ' "$log_file" | sort > "$rows_file"
    jq -n \
        --argjson expected "$expected_json" \
        --argjson terminal_lines "$terminal_lines" \
        --rawfile rows "$rows_file" '
        ($rows | split("\n") | map(select(length > 0) | split("\t") |
          {gateway: .[0], requests: (.[1] | tonumber)}) | sort_by(.gateway)) as $parsed |
        {per_gateway: $parsed,
         total_requests: ([$parsed[].requests] | add // 0),
         expected_gateways: $expected,
         terminal_report_lines: $terminal_lines,
         pass: ($terminal_lines == ($expected | length) and
                ($parsed | length) == ($expected | length) and
                ([$parsed[].gateway] == $expected) and
                ([$parsed[].gateway] | unique | length) == ($parsed | length) and
                all($parsed[]; .requests > 0))}
    ' > "$output_file" || die "failed to parse routechange terminal request counts"
    jq -e '.pass == true' "$output_file" >/dev/null || \
        die "routechange terminal request counts are missing, duplicated, unknown, or zero"
}

run_routechange() {
    local scenario_dir="${BENCH_OUTPUT_DIR}/routechange"
    mkdir -p "$scenario_dir"
    record_event scenario-start routechange
    assert_upstream_backend_absent "$scenario_dir"
    capture_state "$scenario_dir/before"
    capture_supervised_children "$scenario_dir/before"
    validate_supervised_child_baseline "$scenario_dir"
    capture_haproxycfg_baseline "$scenario_dir"
    prewarm_upstream_backend "$scenario_dir" routechange
    local workload_start workload_end workload_duration reloads_before reloads_after reload_delta fleet_size
    reloads_before="$(snapshot_controller_counter haptic_haproxy_reloads_total "$scenario_dir/reloads-before")"
    start_routechange_tunnels "$scenario_dir"
    fleet_size="$(jq 'length' "$scenario_dir/routechange-fleet-before.json")"
    [[ "$fleet_size" -gt 0 ]] || die "routechange found no ready HAProxy fleet pods"

    local -a upstream_command=(
        /gatewayapi-routechange
        "--gateways=${BENCH_GATEWAYS}"
        "--iterations=${BENCH_ROUTECHANGE_ITERATIONS}"
        "--gracePeriod=${BENCH_ROUTECHANGE_GRACE_PERIOD}"
    )
    create_workload_container routechange gatewayapi-routechange \
        "--gateways=${BENCH_GATEWAYS}" \
        "--iterations=${BENCH_ROUTECHANGE_ITERATIONS}" \
        "--gracePeriod=${BENCH_ROUTECHANGE_GRACE_PERIOD}"
    local -a command=(
        timeout --foreground --kill-after=30s "$BENCH_ROUTECHANGE_TIMEOUT"
        docker start --attach "$active_workload_container"
    )
    printf '%q ' "${command[@]}" > "$scenario_dir/command.txt"
    printf '\n' >> "$scenario_dir/command.txt"
    printf '%q ' "${upstream_command[@]}" > "$scenario_dir/upstream-command.txt"
    printf '\n' >> "$scenario_dir/upstream-command.txt"
    observe_routechange_dataplane "$scenario_dir" &
    routechange_observer_pid=$!
    local command_rc=0 observer_rc=0
    date -u +%Y-%m-%dT%H:%M:%S.%NZ > "$scenario_dir/workload-start.txt"
    date +%s.%N > "$scenario_dir/workload-start-epoch.txt"
    record_event workload-start routechange
    run_logged "$scenario_dir/upstream.log" "$scenario_dir/exit-code.txt" "${command[@]}" || command_rc=$?
    date -u +%Y-%m-%dT%H:%M:%S.%NZ > "$scenario_dir/workload-end.txt"
    date +%s.%N > "$scenario_dir/workload-end-epoch.txt"
    record_event workload-end routechange
    workload_start="$(<"$scenario_dir/workload-start-epoch.txt")"
    workload_end="$(<"$scenario_dir/workload-end-epoch.txt")"
    workload_duration="$(awk -v start="$workload_start" -v end="$workload_end" \
        'BEGIN { printf "%.9f", end - start }')"
    printf '%s\n' "$workload_duration" > "$scenario_dir/workload-duration-seconds.txt"
    if kill -0 "$routechange_observer_pid" 2>/dev/null; then
        kill "$routechange_observer_pid" 2>/dev/null || true
    fi
    set +e
    wait "$routechange_observer_pid"
    observer_rc=$?
    set -e
    routechange_observer_pid=""
    [[ $command_rc -eq 0 || $command_rc -eq 1 ]] || \
        die "upstream routechange did not complete normally"
    finish_workload_container "$scenario_dir" "$command_rc"
    local haptic_non_vacuity=false
    if [[ $observer_rc -eq 0 && -s "$scenario_dir/live-dataplane-intermediate.json" ]]; then
        haptic_non_vacuity=true
    fi
    local live_proof_artifact=""
    if [[ -s "$scenario_dir/live-dataplane-intermediate.json" ]]; then
        live_proof_artifact=live-dataplane-intermediate.json
    fi
    jq -n \
        --argjson pass "$haptic_non_vacuity" \
        --argjson observer_exit_code "$observer_rc" \
        --arg artifact "$live_proof_artifact" \
        '{pass: $pass, observer_exit_code: $observer_exit_code,
          proof_artifact: (if $artifact == "" then null else $artifact end),
          exact_workload_semantics: "backendRef ResponseHeaderModifier",
          evidence_scope: "strict live-header observation proves the exact route update reached every HAProxy pod",
          product_gap_classification: (if $pass then null else
            "valid product-negative: exact backendRef response-modifier semantics were not observed" end)}' \
        > "$scenario_dir/haptic-non-vacuity.json"
    kubectl get haproxycfg "$HAPROXYCFG_NAME" -n "$RELEASE_NAMESPACE" -o json \
        > "$scenario_dir/haproxycfg-after-workload-diagnostic.json" || true
    reloads_after="$(snapshot_controller_counter haptic_haproxy_reloads_total "$scenario_dir/reloads-after-immediate")"
    reload_delta="$(awk -v before="$reloads_before" -v after="$reloads_after" 'BEGIN { printf "%.17g", after - before }')"
    jq -n \
        --argjson fleet_size "$fleet_size" \
        --argjson reloads_before "$reloads_before" \
        --argjson reloads_after "$reloads_after" \
        --argjson reload_delta "$reload_delta" \
        '{fleet_size: $fleet_size, reloads_before: $reloads_before, reloads_after_immediate: $reloads_after,
          reload_delta: $reload_delta, reload_counter_is_diagnostic: true}' \
        > "$scenario_dir/non-vacuity.json"
    local unexpected_count request_failure_count churn_change_count
    rg -n 'unexpected status code' "$scenario_dir/upstream.log" \
        > "$scenario_dir/unexpected-statuses.txt" || true
    unexpected_count="$(wc -l < "$scenario_dir/unexpected-statuses.txt")"
    rg -n 'failed: (unexpected status code on iteration|Get "http://)' \
        "$scenario_dir/upstream.log" > "$scenario_dir/request-failures.txt" || true
    request_failure_count="$(wc -l < "$scenario_dir/request-failures.txt")"
    churn_change_count="$(rg -c 'changing route [0-9]+\.\.\.' "$scenario_dir/upstream.log" || true)"
    churn_change_count="${churn_change_count:-0}"
    analyze_routechange_requests "$scenario_dir"
    if [[ ( $command_rc -eq 0 && $request_failure_count -ne 0 ) ||
        ( $command_rc -eq 1 && ( $request_failure_count -eq 0 || $churn_change_count -eq 0 ) ) ]]; then
        die "routechange exit code lacks matching active-churn request failure evidence"
    fi
    local upstream_outcome=true
    if [[ $command_rc -eq 1 ]]; then
        upstream_outcome=false
    fi
    jq \
        --argjson iterations "$BENCH_ROUTECHANGE_ITERATIONS" \
        --arg grace_period "$BENCH_ROUTECHANGE_GRACE_PERIOD" \
        --argjson workload_duration "$workload_duration" \
        --argjson upstream_exit_code "$command_rc" \
        --argjson unexpected_statuses "$unexpected_count" \
        --argjson request_failures "$request_failure_count" \
        --argjson churn_changes "$churn_change_count" \
        --argjson upstream_outcome "$upstream_outcome" \
        --argjson haptic_non_vacuity "$haptic_non_vacuity" \
        --slurpfile live_proof "$scenario_dir/haptic-non-vacuity.json" \
        --slurpfile request_counts "$scenario_dir/request-counts.json" \
        '. + {scenario: "routechange", iterations: $iterations, grace_period: $grace_period,
              upstream_program: {pass: $upstream_outcome, exit_code: $upstream_exit_code,
                                 unexpected_statuses: $unexpected_statuses,
                                 request_failures: $request_failures,
                                 observed_churn_changes: $churn_changes,
                                 terminal_report_complete: true},
              haptic_non_vacuity: $live_proof[0], request_counts: $request_counts[0],
              workload_duration_seconds: $workload_duration,
              haptic_scenario_quality: {pass: false, measurement_complete: false},
              measurement_valid: false,
              pass: ($upstream_outcome and $haptic_non_vacuity)}' \
        "$scenario_dir/non-vacuity.json" > "$scenario_dir/analysis.json"

    wait_for_no_benchmark_routes
    cleanup_upstream_backend "$scenario_dir"
    wait_for_haproxycfg_baseline "$scenario_dir"
    revalidate_routechange_targets "$scenario_dir" cleanup
    assert_routechange_marker_absent "$scenario_dir" after
    stop_routechange_tunnels || die "routechange port-forward processes did not stop"
    capture_state "$scenario_dir/after"
    capture_supervised_children "$scenario_dir/after"
    capture_prometheus_range "$scenario_dir" "$workload_start" "$workload_end" prometheus-range \
        "$scenario_dir/before/haptic-identities.json" false
    verify_identity_unchanged "$scenario_dir/before/haptic-identities.json" \
        "$scenario_dir/after/haptic-identities.json" "$scenario_dir/identity.diff"
    analyze_resources "$scenario_dir" "$scenario_dir/prometheus-range" true
    attach_resource_analysis "$scenario_dir"
    jq '
        .measurement_valid = true |
        .haptic_scenario_quality = {
          pass: (.upstream_program.pass and .haptic_non_vacuity.pass and
                 (.resource_analysis.pass != false)),
          measurement_complete: true,
          upstream_program: .upstream_program,
          haptic_non_vacuity: .haptic_non_vacuity,
          resource_analysis: .resource_analysis
        } |
        .pass = .haptic_scenario_quality.pass
    ' "$scenario_dir/analysis.json" > "$scenario_dir/analysis.json.tmp"
    mv "$scenario_dir/analysis.json.tmp" "$scenario_dir/analysis.json"
    attach_supervised_child_continuity "$scenario_dir"
    attach_lifecycle_outcome_quality "$scenario_dir"
    capture_scenario_logs "$scenario_dir"
    record_event scenario-complete routechange
}

render_scale_config() {
    local output="$1"
    local original="${output%.yaml}-upstream.yaml"
    local gateway_diff="${output%.yaml}-gateway.diff"
    local capture_dir="${WORK_DIR}/capture"
    mkdir -p "$capture_dir"
    local capture_bin="${capture_dir}/pilot-load"
    printf '%s\n' '#!/usr/bin/env bash' 'set -euo pipefail' 'output="${PILOT_LOAD_CONFIG_OUTPUT:?}"' 'cat > "$output"' \
        > "$capture_bin"
    chmod +x "$capture_bin"

    PILOT_LOAD_CONFIG_OUTPUT="${capture_dir}/upstream.yaml" PATH="${capture_dir}:${PATH}" \
        bash "${UPSTREAM_DIR}/tests/route-load.sh" "$BENCH_SCALE_NAMESPACES" "$BENCH_SCALE_ROUTES_PER_NAMESPACE"
    [[ -s "${capture_dir}/upstream.yaml" ]] || die "upstream route-load script produced no configuration"
    cp "${capture_dir}/upstream.yaml" "$original"

    local gateways_yaml gateway
    gateways_yaml=""
    for gateway in "${GATEWAYS[@]}"; do
        gateways_yaml+="          - ${gateway}"$'\n'
    done
    awk -v replacement="$gateways_yaml" '
        /^          gateways:$/ {
            print
            printf "%s", replacement
            replacing = 1
            next
        }
        replacing && /^          - / { next }
        { replacing = 0; print }
    ' "${capture_dir}/upstream.yaml" > "$output"
    [[ "$(rg -c '^          gateways:$' "$output")" -eq 1 ]] || die "scale config gateway block was not uniquely replaced"
    local expected_gateway_lines=${#GATEWAYS[@]}
    [[ "$(rg -c '^          - .+/.+$' "$output")" -eq "$expected_gateway_lines" ]] || \
        die "scale config contains an unexpected gateway list"
    diff -u "$original" "$output" > "$gateway_diff" || {
        local diff_rc=$?
        [[ $diff_rc -eq 1 ]] || die "failed to compare upstream and parameterized scale configurations"
    }
}

wait_for_namespace_set() {
    local expected="$1"
    local deadline=$((SECONDS + 300))
    local current="${WORK_DIR}/current-namespaces.json"
    while (( SECONDS < deadline )); do
        kubectl get namespaces -o json | jq -S '[.items[] | {name: .metadata.name, uid: .metadata.uid}] | sort_by(.name)' > "$current"
        if cmp -s "$expected" "$current"; then
            return 0
        fi
        sleep 2
    done
    diff -u "$expected" "$current" >&2 || true
    die "pilot-load namespaces were not cleaned up"
}

wait_for_node_set() {
    local expected="$1"
    local deadline=$((SECONDS + 300))
    local current="${WORK_DIR}/current-nodes.json"
    while (( SECONDS < deadline )); do
        kubectl get nodes -o json | jq -S '[.items[] | {name: .metadata.name, uid: .metadata.uid}] | sort_by(.name)' > "$current"
        if cmp -s "$expected" "$current"; then
            return 0
        fi
        sleep 2
    done
    diff -u "$expected" "$current" >&2 || true
    die "pilot-load nodes were not cleaned up"
}

write_scale_readiness_timeout_analysis() {
    local scenario_dir="$1"
    local expected_routes="$2"
    local peak_routes="$3"
    jq -n \
        --argjson expected_routes "$expected_routes" \
        --argjson peak_routes "$peak_routes" \
        --arg startup_timeout "$BENCH_SCALE_STARTUP_TIMEOUT" \
        --slurpfile readiness "$scenario_dir/scale-readiness.json" '
        {scenario: "scale", expected_routes: $expected_routes, peak_routes: $peak_routes,
         upstream_synced: true, upstream_exit_code: 0, errors: 0,
         upstream_program: {
           pass: true, exit_code: 0, sync_marker_observed: true,
           expected_route_count_observed: true,
           stopped_by_runner_after_readiness_timeout: true
         },
         haptic_readiness: $readiness[0],
         haptic_non_vacuity: {pass: false, readiness: $readiness[0]},
         steady_window: {
           applicable: false, started: false, duration: null,
           reason_code: "scale-readiness-timeout"
         },
         outcome_quality: {
           applicable: false, pass: null, reason_code: "scale-readiness-timeout"
         },
         resource_analysis: {
           artifact: null, status: "not_applicable", gating: false, pass: null,
           reason_code: "scale-readiness-timeout"
         },
         haptic_scenario_quality: {
           pass: false, measurement_complete: true, steady_measurement_complete: false,
           reason_code: "scale-readiness-timeout",
           startup_timeout: $startup_timeout,
           upstream_program: {
             pass: true, exit_code: 0, sync_marker_observed: true,
             expected_route_count_observed: true,
             stopped_by_runner_after_readiness_timeout: true
           },
           haptic_non_vacuity: {pass: false, readiness: $readiness[0]},
           resource_analysis: {
             artifact: null, status: "not_applicable", gating: false, pass: null,
             reason_code: "scale-readiness-timeout"
           }
         },
         measurement_valid: true, pass: false}
    ' > "$scenario_dir/analysis.json"
    jq -e '
        .scenario == "scale" and .measurement_valid == true and .pass == false and
        .upstream_program.pass == true and .haptic_readiness.pass == false and
        .haptic_scenario_quality.measurement_complete == true and
        .haptic_scenario_quality.steady_measurement_complete == false and
        .steady_window.applicable == false and .steady_window.started == false and
        .resource_analysis.gating == false and .resource_analysis.pass == null and
        .resource_analysis.artifact == null
    ' "$scenario_dir/analysis.json" >/dev/null || \
        die "scale readiness timeout analysis is incomplete"
}

run_scale() {
    local scenario_dir="${BENCH_OUTPUT_DIR}/scale"
    mkdir -p "$scenario_dir"
    record_event scenario-start scale
    assert_upstream_backend_absent "$scenario_dir"
    wait_for_no_benchmark_routes
    capture_state "$scenario_dir/before"
    capture_supervised_children "$scenario_dir/before"
    validate_supervised_child_baseline "$scenario_dir"
    capture_haproxycfg_baseline "$scenario_dir"
    local start end expected_routes peak=0 synced=false steady_started=false scale_ready=false
    local readiness_timed_out=false
    local startup_deadline startup_seconds
    start="$(<"$scenario_dir/before/epoch.txt")"
    expected_routes=$((BENCH_SCALE_NAMESPACES * BENCH_SCALE_ROUTES_PER_NAMESPACE))
    startup_seconds="$(duration_seconds "$BENCH_SCALE_STARTUP_TIMEOUT")" || \
        die "could not convert BENCH_SCALE_STARTUP_TIMEOUT"
    startup_deadline=$((SECONDS + startup_seconds))
    kubectl get namespaces -o json | jq -S '[.items[] | {name: .metadata.name, uid: .metadata.uid}] | sort_by(.name)' \
        > "$scenario_dir/namespaces-before.json"
    kubectl get nodes -o json | jq -S '[.items[] | {name: .metadata.name, uid: .metadata.uid}] | sort_by(.name)' \
        > "$scenario_dir/nodes-before.json"

    render_scale_config "$scenario_dir/route-load.yaml"
    (
        cd "$scenario_dir"
        sha256sum route-load-upstream.yaml route-load.yaml
    ) > "$scenario_dir/route-load.sha256"
    local -a upstream_command=(/pilot-load cluster --config /route-load.yaml)
    create_workload_container scale pilot-load cluster --config /route-load.yaml
    docker cp "$scenario_dir/route-load.yaml" "${active_workload_container}:/route-load.yaml" || \
        die "failed to inject the exact scale config into the workload container"
    local -a command=(docker start --attach "$active_workload_container")
    printf '%q ' "${command[@]}" > "$scenario_dir/command.txt"
    printf '\n' >> "$scenario_dir/command.txt"
    printf '%q ' "${upstream_command[@]}" > "$scenario_dir/upstream-command.txt"
    printf '\n' >> "$scenario_dir/upstream-command.txt"

    date -u +%Y-%m-%dT%H:%M:%S.%NZ > "$scenario_dir/workload-start.txt"
    date +%s.%N > "$scenario_dir/workload-start-epoch.txt"
    record_event workload-start scale
    run_logged "$scenario_dir/upstream.log" "$scenario_dir/exit-code.txt" "${command[@]}" &
    scale_pid=$!
    local container_start_deadline=$((SECONDS + 20))
    while (( SECONDS < container_start_deadline )) && ! workload_container_running "$active_workload_container"; do
        pid_running "$scale_pid" || break
        sleep 0.1
    done
    workload_container_running "$active_workload_container" || \
        die "pilot-load workload container did not enter Running state"

    local next_report=$SECONDS
    while workload_container_running "$active_workload_container"; do
        capture_network_health scale-current
        local count
        count="$(total_route_count)"
        printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" "$count" >> "$scenario_dir/route-counts.txt"
        if (( count > peak )); then
            peak=$count
        fi
        if [[ "$synced" == "false" ]] && rg -q 'cluster "primary" synced, starting cluster scaler' "$scenario_dir/upstream.log"; then
            synced=true
            record_event scale-upstream-synced scale
        fi
        if (( SECONDS >= startup_deadline )) && [[ "$steady_started" == "false" ]]; then
            date -u +%Y-%m-%dT%H:%M:%S.%NZ > "$scenario_dir/startup-timeout-fired.txt"
            signal_workload_container "$active_workload_container" || true
            die "pilot-load did not reach proven HAPTIC scale within ${BENCH_SCALE_STARTUP_TIMEOUT}"
        fi
        if [[ "$count" -eq "$expected_routes" && "$synced" == "true" && "$scale_ready" == "false" ]]; then
            capture_scale_route_snapshot "$expected_routes" "$scenario_dir/routes-at-scale-snapshot.json"
            record_event scale-route-snapshot scale
            local readiness_rc=0
            wait_for_scale_dataplane "$scenario_dir" "$expected_routes" "$startup_deadline" || readiness_rc=$?
            if [[ $readiness_rc -eq "$READINESS_RESULT_DEADLINE" ]]; then
                readiness_timed_out=true
                date -u +%Y-%m-%dT%H:%M:%S.%NZ > "$scenario_dir/readiness-timeout-fired.txt"
                record_event scale-readiness-timeout scale
                [[ "$(total_route_count)" -eq "$expected_routes" ]] || \
                    die "scale route count changed at the HAPTIC readiness deadline"
                capture_scale_route_snapshot "$expected_routes" \
                    "$scenario_dir/routes-at-readiness-timeout.json"
                jq -e --slurpfile initial "$scenario_dir/routes-at-scale-snapshot.json" '
                    ([.items[].metadata.uid] | sort) ==
                    ([$initial[0].items[].metadata.uid] | sort)
                ' "$scenario_dir/routes-at-readiness-timeout.json" >/dev/null || \
                    die "scale HTTPRoute identity changed before the HAPTIC readiness deadline"
                if ! signal_running_workload_container "$active_workload_container"; then
                    date -u +%Y-%m-%dT%H:%M:%S.%NZ > "$scenario_dir/readiness-timeout-signal-failed.txt"
                    die "pilot-load could not be stopped after the HAPTIC readiness deadline"
                fi
                date -u +%Y-%m-%dT%H:%M:%S.%NZ > "$scenario_dir/readiness-timeout-stop-issued.txt"
                break
            fi
            [[ $readiness_rc -eq 0 ]] || die "scale readiness returned an invalid result"
            workload_container_running "$active_workload_container" || \
                die "pilot-load exited before the steady scale interval"
            [[ "$(total_route_count)" -eq "$expected_routes" ]] || \
                die "scale route count changed during the HAPTIC readiness proof"
            capture_scale_activity_snapshot "$scenario_dir" start "$expected_routes"
            date -u +%Y-%m-%dT%H:%M:%S.%NZ > "$scenario_dir/steady-start.txt"
            date +%s.%N > "$scenario_dir/steady-start-epoch.txt"
            record_event scale-steady-start scale
            scale_ready=true
            steady_started=true
            local scale_container="$active_workload_container"
            (
                local timer_rc=0
                timeout --foreground --kill-after=5s "$BENCH_SCALE_DURATION" \
                    docker wait "$scale_container" >/dev/null 2>&1 || timer_rc=$?
                if [[ $timer_rc -eq 124 ]]; then
                    date -u +%Y-%m-%dT%H:%M:%S.%NZ > "$scenario_dir/steady-duration-elapsed.txt"
                    date +%s.%N > "$scenario_dir/steady-end-epoch.txt"
                elif [[ $timer_rc -ne 0 ]]; then
                    printf '%d\n' "$timer_rc" > "$scenario_dir/steady-timer-error.txt"
                fi
            ) &
            scale_timer_pid=$!
        fi
        if (( count > expected_routes )); then
            signal_workload_container "$active_workload_container" || true
            die "scale workload exceeded ${expected_routes} HAPTIC HTTPRoutes"
        fi
        if [[ "$steady_started" == "true" && ! -f "$scenario_dir/steady-end-epoch.txt" &&
            "$count" -ne "$expected_routes" ]]; then
            signal_workload_container "$active_workload_container" || true
            die "scale route count changed during the steady-churn interval: ${count}/${expected_routes}"
        fi
        if (( SECONDS >= next_report )); then
            info "scale routes: ${count}/${expected_routes}"
            next_report=$((SECONDS + 30))
        fi
        if [[ -f "$scenario_dir/steady-end-epoch.txt" &&
            ! -f "$scenario_dir/steady-stop-issued.txt" ]]; then
            [[ "$count" -eq "$expected_routes" ]] || \
                die "scale route count changed at the steady-churn boundary: ${count}/${expected_routes}"
            capture_scale_activity_snapshot "$scenario_dir" end "$expected_routes"
            analyze_scale_activity "$scenario_dir" "$(<"$scenario_dir/steady-start-epoch.txt")" \
                "$(<"$scenario_dir/steady-end-epoch.txt")" "$expected_routes"
            record_event scale-steady-activity-proven scale
            capture_prometheus_range "$scenario_dir" \
                "$(<"$scenario_dir/steady-start-epoch.txt")" \
                "$(<"$scenario_dir/steady-end-epoch.txt")" steady-prometheus-range
            if ! signal_workload_container "$active_workload_container" "$expected_routes"; then
                date -u +%Y-%m-%dT%H:%M:%S.%NZ > "$scenario_dir/steady-signal-failed.txt"
                die "pilot-load could not be stopped after the steady-churn interval"
            fi
            date -u +%Y-%m-%dT%H:%M:%S.%NZ > "$scenario_dir/steady-stop-issued.txt"
        fi
        sleep 5
    done

    local scale_rc
    set +e
    wait "$scale_pid"
    scale_rc=$?
    set -e
    scale_pid=""
    if [[ -n "$scale_timer_pid" ]]; then
        wait "$scale_timer_pid" 2>/dev/null || true
    fi
    scale_timer_pid=""
    date -u +%Y-%m-%dT%H:%M:%S.%NZ > "$scenario_dir/workload-end.txt"
    date +%s.%N > "$scenario_dir/workload-end-epoch.txt"
    record_event workload-end scale
    printf '%d\n' "$peak" > "$scenario_dir/peak-route-count.txt"
    [[ ! -f "$scenario_dir/startup-timeout-fired.txt" ]] || die "pilot-load did not reach steady scale within ${BENCH_SCALE_STARTUP_TIMEOUT}"
    [[ ! -f "$scenario_dir/readiness-timeout-signal-failed.txt" ]] || \
        die "pilot-load did not stop after the HAPTIC readiness deadline"
    [[ ! -f "$scenario_dir/steady-signal-failed.txt" ]] || die "pilot-load did not stop after the steady-churn interval"
    [[ ! -f "$scenario_dir/steady-timer-error.txt" ]] || die "the steady-churn timer failed"
    workload_exit_acceptable "$scale_rc" "$scenario_dir/steady-stop-issued.txt" || \
        die "pilot-load exited with ${scale_rc}"
    finish_workload_container "$scenario_dir" "$scale_rc"
    if rg -ni '(^|[[:space:]])error([:=[:space:]]|$)|failed to' "$scenario_dir/upstream.log" \
        > "$scenario_dir/upstream-errors.txt"; then
        die "pilot-load logged an error"
    fi
    if [[ "$readiness_timed_out" == "true" ]]; then
        [[ "$steady_started" == "false" && "$scale_ready" == "false" && "$synced" == "true" &&
            "$peak" -eq "$expected_routes" && -s "$scenario_dir/scale-readiness.json" &&
            -f "$scenario_dir/readiness-timeout-stop-issued.txt" ]] || \
            die "scale readiness timeout evidence is incomplete"
        write_scale_readiness_timeout_analysis "$scenario_dir" "$expected_routes" "$peak"
    else
        [[ "$steady_started" == "true" && -f "$scenario_dir/steady-duration-elapsed.txt" ]] || \
            die "pilot-load exited before completing the ${BENCH_SCALE_DURATION} steady-churn interval"
        [[ -s "$scenario_dir/steady-activity.json" && -f "$scenario_dir/steady-stop-issued.txt" ]] || \
            die "scale steady interval lacks HAPTIC activity proof"
        [[ "$scale_ready" == "true" && "$synced" == "true" && "$peak" -eq "$expected_routes" ]] || \
            die "scale workload reached ${peak} HAPTIC HTTPRoutes, expected exactly ${expected_routes}"
        jq -n \
            --argjson expected_routes "$expected_routes" \
            --argjson peak_routes "$peak" \
            --arg duration "$BENCH_SCALE_DURATION" \
            --arg steady_started_at "$(<"$scenario_dir/steady-start.txt")" \
            --arg steady_ended_at "$(<"$scenario_dir/steady-duration-elapsed.txt")" \
            --slurpfile readiness "$scenario_dir/scale-readiness.json" \
            --slurpfile activity "$scenario_dir/steady-activity.json" \
            '{scenario: "scale", expected_routes: $expected_routes, peak_routes: $peak_routes,
              upstream_synced: true, steady_duration: $duration, steady_started_at: $steady_started_at,
              steady_ended_at: $steady_ended_at, upstream_exit_code: 0, errors: 0,
              upstream_program: {pass: true, exit_code: 0, sync_marker_observed: true},
              haptic_readiness: $readiness[0],
              haptic_non_vacuity: {pass: true, readiness: $readiness[0], steady_activity: $activity[0]},
              outcome_quality: $activity[0].outcome_quality,
              haptic_scenario_quality: {pass: false, measurement_complete: false},
              measurement_valid: false, pass: false}' \
            > "$scenario_dir/analysis.json"
    fi

    wait_for_no_benchmark_routes
    wait_for_namespace_set "$scenario_dir/namespaces-before.json"
    wait_for_node_set "$scenario_dir/nodes-before.json"
    wait_for_haproxycfg_baseline "$scenario_dir"
    capture_state "$scenario_dir/after"
    capture_supervised_children "$scenario_dir/after"
    end="$(<"$scenario_dir/after/epoch.txt")"
    verify_identity_unchanged "$scenario_dir/before/haptic-identities.json" \
        "$scenario_dir/after/haptic-identities.json" "$scenario_dir/identity.diff"
    if [[ "$readiness_timed_out" != "true" ]]; then
        capture_prometheus_range "$scenario_dir" "$start" "$end"
        [[ -s "$scenario_dir/steady-prometheus-range/cpu.json" ]] || \
            die "steady Prometheus range was not captured during the measured window"
        analyze_resources "$scenario_dir" "$scenario_dir/steady-prometheus-range"
        attach_resource_analysis "$scenario_dir"
        jq '
            .measurement_valid = true |
            .haptic_scenario_quality = {
              pass: (.upstream_program.pass and .haptic_non_vacuity.pass and .outcome_quality.pass and
                     (.resource_analysis.pass != false)),
              measurement_complete: true,
              upstream_program: .upstream_program,
              haptic_non_vacuity: .haptic_non_vacuity,
              outcome_quality: .outcome_quality,
              resource_analysis: .resource_analysis
            } |
            .pass = .haptic_scenario_quality.pass
        ' "$scenario_dir/analysis.json" > "$scenario_dir/analysis.json.tmp"
        mv "$scenario_dir/analysis.json.tmp" "$scenario_dir/analysis.json"
    fi
    attach_supervised_child_continuity "$scenario_dir"
    attach_lifecycle_outcome_quality "$scenario_dir"
    capture_scenario_logs "$scenario_dir"
    record_event scenario-complete scale
}

write_runner_summary() {
    local -a analyses=()
    local scenario
    for scenario in "${SCENARIOS[@]}"; do
        [[ -s "${BENCH_OUTPUT_DIR}/${scenario}/analysis.json" ]] || \
            die "scenario ${scenario} did not produce analysis.json"
        jq -e --arg scenario "$scenario" '
            .scenario == $scenario and .measurement_valid == true and
            (.pass | type) == "boolean" and
            (.upstream_program.pass | type) == "boolean" and
            (.haptic_scenario_quality.pass | type) == "boolean" and
            .lifecycle_outcome_quality.evidence_valid == true and
            (.lifecycle_outcome_quality.pass | type) == "boolean" and
            .supervised_child_continuity.evidence_valid == true and
            (.supervised_child_continuity.pass | type) == "boolean"
        ' "${BENCH_OUTPUT_DIR}/${scenario}/analysis.json" >/dev/null || \
            die "scenario ${scenario} analysis lacks a complete measured verdict"
        analyses+=("${BENCH_OUTPUT_DIR}/${scenario}/analysis.json")
    done
    jq -s \
        --arg finished_at "$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)" '
        . as $analyses |
        {schema_version: 1,
         finished_at: $finished_at,
         harness: {pass: null,
                   status: "pending-terminal-gates",
                   process_exit_semantics: "nonzero means harness, provenance, or evidence invalid; measured gaps remain structured results"},
         public_comparison: "ballpark-only",
         scenarios: [$analyses[] |
           {scenario, measurement_valid, pass,
            upstream_program, haptic_non_vacuity, supervised_child_continuity,
            haptic_scenario_quality, resource_analysis}],
         measured_result: {
           pass: all($analyses[]; .pass == true),
           negative_scenarios: [$analyses[] | select(.pass != true) | .scenario]
         }}
    ' "${analyses[@]}" > "${BENCH_OUTPUT_DIR}/runner-summary.json"
}

main() {
    validate_inputs
    check_build_prerequisites
    check_haptic_worktree
    prepare_work_dir
    capture_host_provenance "${WORK_DIR}/host-provenance"
    prepare_output
    cp -a "${WORK_DIR}/host-provenance/." "${BENCH_OUTPUT_DIR}/host/"
    fetch_and_build_upstream
    write_initial_metadata

    if [[ "$BUILD_ONLY" == "true" ]]; then
        record_event build-only-complete
        info "upstream tools built and verified; artifacts: ${BENCH_OUTPUT_DIR}"
        return 0
    fi

    check_cluster_prerequisites
    bootstrap_cluster
    record_network_daemon
    start_runtime_config_history
    capture_live_secret_patterns || \
        die "failed to capture live Secret patterns after cluster bootstrap"
    configure_haptic
    verify_spoa_bundle
    capture_live_secret_patterns || \
        die "failed to refresh live Secret patterns after HAPTIC configuration"
    verify_artifact_secrets || die "bootstrap artifacts failed the Secret scan"
    prepare_gateway
    assert_isolated_workload
    install_prometheus
    kubectl version -o yaml > "${BENCH_OUTPUT_DIR}/host/kubectl-version.yaml"
    helm version > "${BENCH_OUTPUT_DIR}/host/helm-version.txt"
    kind version > "${BENCH_OUTPUT_DIR}/host/kind-version.txt"

    local scenario
    for scenario in "${SCENARIOS[@]}"; do
        capture_kind_cluster_inventory "before-${scenario}" present
        capture_network_health "before-${scenario}"
        case "$scenario" in
            probe) run_probe ;;
            routechange) run_routechange ;;
            scale) run_scale ;;
        esac
        capture_network_health "after-${scenario}"
    done

    capture_kind_cluster_inventory final present
    verify_spoa_bundle after
    write_runner_summary
    verify_artifact_secrets || die "benchmark artifacts failed the Secret scan"
    record_event runner-complete
    if jq -e '.measured_result.pass == true' "${BENCH_OUTPUT_DIR}/runner-summary.json" >/dev/null; then
        info "benchmark completed with all measured scenario gates passing; artifacts: ${BENCH_OUTPUT_DIR}"
    else
        info "benchmark completed with measured gaps; artifacts: ${BENCH_OUTPUT_DIR}"
    fi
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
    main "$@"
fi
