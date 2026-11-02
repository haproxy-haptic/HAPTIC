#!/usr/bin/env bash
set -euo pipefail

mkdir -p .pilot debug-logs
rm -f .pilot/test-finished
git diff --exit-code "$PILOT_SOURCE_SHA"
{
    git rev-parse HEAD
    scripts/source-hash.sh
    lscpu
    free -b
    df -B1 . /var/lib/docker
    go version
    docker version
    helm version
    kind version
} > .pilot/host.txt 2>&1
python3 .github/scripts/runner-metrics.py .pilot/metrics.jsonl &
metrics_pid=$!
stop_background() {
    kill "$metrics_pid" ${spoa_build_pid:+-- "-$spoa_build_pid"} 2>/dev/null || true
    wait "$metrics_pid" ${spoa_build_pid:+"$spoa_build_pid"} 2>/dev/null || true
    [[ -z "${spoa_build_pid:-}" ]] || cat .pilot/spoa-build.log
}
trap stop_background EXIT

if [[ "${PILOT_CAPTURE_NETWORK:-0}" == 1 ]]; then
    sudo apt-get update -qq
    sudo apt-get install -y tcpdump
    python3 .github/scripts/capture-cluster-network.py > .pilot/network-capture.log 2>&1 &
    network_pid=$!
fi

test "$(kind get clusters | wc -l)" -eq 0
chart_version="0.0.0-github.g${PILOT_SOURCE_SHA:0:8}"
sed -i "s|^version:.*|version: ${chart_version}|" charts/haptic/Chart.yaml
yq -i '.cache.varnish.image = strenv(PILOT_VARNISH_IMAGE)' charts/haptic/values.yaml
bash .github/scripts/spoa-test-image.sh select
export PILOT_GOMAXPROCS="${PILOT_GOMAXPROCS:-2}"
values_file=scripts/dev-env-assets/e2e-values.yaml
if [[ "${HAPTIC_E2E_PROFILE:-}" == conformance ]]; then
    values_file=scripts/dev-env-assets/conformance-values.yaml
fi
yq -i '.controller.extraEnv += [{"name": "GOMAXPROCS", "value": strenv(PILOT_GOMAXPROCS)}, {"name": "GOGC", "value": strenv(PILOT_GOGC)}] | (.controller.extraEnv[] | select(.name == "GOMAXPROCS" or .name == "GOGC") | .value) style="double"' "$values_file"
if [[ "${PILOT_VALIDATION_TMPFS:-0}" == 1 ]]; then
    yq -i '.controller.extraEnv += [{"name": "TMPDIR", "value": "/v"}] | .controller.extraVolumes += [{"name": "validation-scratch", "emptyDir": {"medium": "Memory", "sizeLimit": "512Mi"}}] | .controller.extraVolumeMounts += [{"name": "validation-scratch", "mountPath": "/v"}]' "$values_file"
fi
if [[ "${HAPTIC_E2E_PROFILE:-}" == cache ]]; then
    export PILOT_VARNISH_IMAGE=haptic-varnish:pilot
    make docker-build-varnish VARNISH_IMAGE="$PILOT_VARNISH_IMAGE" VARNISH_PLATFORMS=linux/arm64
    yq -i '.cache.varnish.image = strenv(PILOT_VARNISH_IMAGE)' charts/haptic/values.yaml
fi
build_target=docker-build-test
if [[ "${PILOT_NATIVE_BUILD:-0}" == 1 ]]; then
    build_target=docker-build-test-native
fi
setsid bash .github/scripts/spoa-test-image.sh build > .pilot/spoa-build.log 2>&1 &
spoa_build_pid=$!
make "$build_target" 2>&1 | tee .pilot/build.log
wait "$spoa_build_pid" || exit 1
spoa_build_pid=
docker image inspect haptic:test > .pilot/controller-image.json
if [[ "${PILOT_STARTUP_PROFILE:-1}" == 1 ]]; then
    python3 .github/scripts/profile-startup.py > .pilot/startup-profile.log 2>&1 &
    profile_pid=$!
fi
test_target=test-e2e
if [[ "${PILOT_STARTUP_ONLY:-0}" == 1 ]]; then
    export TEST_RUN_PATTERN='^$'
elif [[ -n "${HAPTIC_E2E_PROFILE:-}" ]]; then
    test_target=test-e2e-profile
elif [[ -n "${TEST_RUN_PATTERN:-}" ]]; then
    printf '%s\n' "$TEST_RUN_PATTERN" > .pilot/e2e-selection.txt
else
    go test -mod=readonly -tags=e2e -list '^Test' ./tests/e2e/... > debug-logs/e2e-test-inventory.txt
    TEST_RUN_PATTERN=$(python3 scripts/shard-go-tests.py --weights "${E2E_SHARD_WEIGHTS:-tests/e2e/shard-weights.json}" "${E2E_SHARD_INDEX:-1}" "${E2E_SHARD_TOTAL:-3}" < debug-logs/e2e-test-inventory.txt)
    export TEST_RUN_PATTERN
    printf '%s\n' "$TEST_RUN_PATTERN" > .pilot/e2e-selection.txt
fi
if [[ "${PILOT_PROFILE_SCALE:-0}" == 1 ]]; then
    python3 .github/scripts/profile-scale.py > .pilot/scale-profile.log 2>&1 &
    scale_profile_pid=$!
fi
python3 .github/scripts/capture-startup.py > .pilot/startup-capture.log 2>&1 &
startup_pid=$!
started=$(date +%s)
# Debug-level agent logs name every runtime command a failed test's pod ran (#590).
export HAPTIC_E2E_AGENT_DEBUG="${HAPTIC_E2E_AGENT_DEBUG:-1}"
set +e
KEEP_CLUSTER=true SKIP_DOCKER_BUILD=1 make "$test_target" 2>&1 | tee .pilot/test.log
result=${PIPESTATUS[0]}
set -e
elapsed=$(( $(date +%s) - started ))
touch .pilot/test-finished
if [[ -n "${network_pid:-}" ]]; then wait "$network_pid"; fi
startup_result=0
wait "$startup_pid" || startup_result=$?
if [[ "$result" == 0 && "$startup_result" != 0 ]]; then result=$startup_result; fi
printf '{"exit_code":%s,"test_seconds":%s,"startup_exit_code":%s}\n' "$result" "$elapsed" "$startup_result" > .pilot/result.json
if [[ -n "${scale_profile_pid:-}" ]]; then wait "$scale_profile_pid" || true; fi
if [[ -n "${profile_pid:-}" ]]; then wait "$profile_pid" || true; fi
for resource in nodes pods events; do
    docker exec haptic-e2e-control-plane kubectl --kubeconfig=/etc/kubernetes/admin.conf get "$resource" --all-namespaces -o json > ".pilot/$resource.json" 2>&1 || true
done
docker exec haptic-e2e-control-plane kubectl --kubeconfig=/etc/kubernetes/admin.conf describe pods --all-namespaces > .pilot/pod-details.txt 2>&1 || true
docker exec haptic-e2e-control-plane kubectl --kubeconfig=/etc/kubernetes/admin.conf logs --namespace haptic --selector job-name=haptic-haptic-pre-rollout --all-containers --prefix --tail=-1 --max-log-requests=20 > .pilot/preflight-logs.txt 2>&1 || true
docker exec haptic-e2e-control-plane kubectl --kubeconfig=/etc/kubernetes/admin.conf logs --namespace haptic --selector app.kubernetes.io/component=loadbalancer --container spoa-hub --prefix --tail=-1 --max-log-requests=20 > .pilot/spoa-hub-logs.txt 2>&1 || true
docker exec haptic-e2e-control-plane kubectl --kubeconfig=/etc/kubernetes/admin.conf logs --namespace haptic --selector app.kubernetes.io/component=controller --container validators --prefix --tail=-1 --max-log-requests=20 > .pilot/validator-logs.txt 2>&1 || true
for previous in current previous; do
    flags=()
    if [[ "$previous" == previous ]]; then flags+=(--previous); fi
    docker exec haptic-e2e-control-plane kubectl --kubeconfig=/etc/kubernetes/admin.conf \
        logs --namespace haptic --selector app.kubernetes.io/component=controller \
        --container controller --prefix --tail=-1 --max-log-requests=20 "${flags[@]}" \
        > ".pilot/$previous-controller-logs.txt" 2>&1 || true
done
sudo chown -R "$(id -u):$(id -g)" .pilot debug-logs
printf 'Native ARM E2E exit: **%s**; duration: **%s seconds**\n' "$result" "$elapsed" >> "$GITHUB_STEP_SUMMARY"
exit "$result"
