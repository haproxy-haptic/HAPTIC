#!/usr/bin/env bash
set -euo pipefail

baseline=b8bebd85a1c9102e7a300d9e5feebf3df7848841
root="$PWD"
pair_dir="$(mktemp -d "$RUNNER_TEMP/haptic-arm-pair-XXXXXX")"
baseline_root="$pair_dir/baseline"
mkdir -p .pilot debug-logs
git diff --exit-code "$PILOT_SOURCE_SHA"
git merge-base --is-ancestor "$baseline" "$PILOT_SOURCE_SHA"
test -z "$(kind get clusters)"
git worktree add --detach "$baseline_root" "$baseline"
{
    git rev-parse HEAD
    scripts/source-hash.sh
    lscpu
    uname -a
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
}
trap stop_background EXIT

sed -i "s|^version:.*|version: 0.0.0-github.g${PILOT_SOURCE_SHA:0:8}|" charts/haptic/Chart.yaml
yq -i '.cache.varnish.image = strenv(PILOT_VARNISH_IMAGE)' charts/haptic/values.yaml
bash .github/scripts/spoa-test-image.sh select
git diff > .pilot/test-values.patch
cp charts/haptic/Chart.yaml "$baseline_root/charts/haptic/Chart.yaml"
cp charts/haptic/values.yaml "$baseline_root/charts/haptic/values.yaml"
python3 .github/scripts/upgrade-pair-evidence.py inputs "$baseline_root" "$root"

setsid bash .github/scripts/spoa-test-image.sh build > .pilot/spoa-build.log 2>&1 &
spoa_build_pid=$!
make docker-build-test-native 2>&1 | tee .pilot/build.log
wait "$spoa_build_pid"
spoa_build_pid=
cat .pilot/spoa-build.log
docker image inspect haptic:test "$(bash scripts/chart-spoa-image.sh)" > .pilot/runtime-images.json
docker ps --all --quiet --no-trunc | sort > .pilot/initial-containers.txt
for repository in "$baseline_root" "$root"; do
    (cd "$repository" && bash scripts/test-infra.sh chart-upgrade --help)
done > .pilot/harness-builds.log 2>&1

overall=0
for arm in baseline candidate; do
    mkdir -p ".pilot/$arm"
    printf '{"state":"not_started"}\n' > ".pilot/$arm/result.json"
done
for arm in baseline candidate; do
    repository="$root"
    if [[ "$arm" == baseline ]]; then repository="$baseline_root"; fi
    started="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    set +e
    (
        cd "$repository"
        UPGRADE_CLUSTER_NAME="haptic-upgrade-$arm" \
            UPGRADE_ARTIFACT_DIR="$root/debug-logs/$arm" \
            SKIP_DOCKER_BUILD=1 make test-chart-upgrade
    ) 2>&1 | tee ".pilot/$arm/test.log"
    statuses=("${PIPESTATUS[@]}")
    result=${statuses[0]}
    log_result=${statuses[1]}
    set -e
    printf '%s\n' "$result" > ".pilot/$arm/exit"
    printf '%s\n' "$log_result" > ".pilot/$arm/log-exit"
    if ! python3 .github/scripts/upgrade-pair-evidence.py arm "$arm" "$started" "$result" "$log_result"; then
        printf '{"exit_code":1,"state":"aborted","after_arm":"%s","canonical":false}\n' "$arm" > .pilot/result.json
        exit 1
    fi
    if [[ "$result" -ne 0 ]]; then overall=1; fi
done
git worktree remove --force "$baseline_root"
printf '{"exit_code":%s,"canonical":false,"publication_authority":false}\n' "$overall" > .pilot/result.json
printf 'Predefined ARM pair completed; preserved aggregate exit: **%s**.\n' "$overall" >> "$GITHUB_STEP_SUMMARY"
exit "$overall"
