#!/usr/bin/env bash
set -euo pipefail

mkdir -p .pilot debug-logs
git diff --exit-code "$PILOT_SOURCE_SHA"
{
    git rev-parse HEAD
    scripts/source-hash.sh
    lscpu
    free -b
    df -B1 . /var/lib/docker
    docker version
} > .pilot/host.txt 2>&1
python3 .github/scripts/runner-metrics.py .pilot/metrics.jsonl &
metrics_pid=$!
trap 'kill "$metrics_pid" 2>/dev/null || true; wait "$metrics_pid" 2>/dev/null || true' EXIT
PILOT_CI_IMAGE=$(python3 -m scripts.ci.github_toolchain "$PILOT_CI_IMAGE" --evidence .pilot/toolchain-resolution.json)
export PILOT_CI_IMAGE
docker image inspect "$PILOT_CI_IMAGE" > .pilot/toolchain-image.json
cache_args=()
website_args=()
if [[ "$PILOT_SUITE" == docs ]]; then
    website_args+=(--env GH_TOKEN)
fi
if [[ -n "${PILOT_GO_CACHE_DIR:-}" ]]; then
    mkdir -p "$PILOT_GO_CACHE_DIR"/{build,modules}
    cache_args+=(--volume "$PILOT_GO_CACHE_DIR:/ci-go-cache" --env GOCACHE=/ci-go-cache/build --env GOMODCACHE=/ci-go-cache/modules)
fi
started=$(date +%s)
set +e
docker run --name haptic-pilot --network host \
    "${cache_args[@]}" \
    "${website_args[@]}" \
    --volume /var/run/docker.sock:/var/run/docker.sock \
    --volume "$PWD:$PWD" --workdir "$PWD" \
    --env PILOT_SUITE --env PILOT_SOURCE_SHA --env PILOT_VARNISH_IMAGE \
    --env GITHUB_RUN_ID --env GITHUB_JOB \
    --env HAPROXY_VERSION --env KIND_NODE_IMAGE --env BASELINE_CHART_VERSION \
    --env GITOPS_PROVIDER --env GITOPS_CERTIFICATES --env SHARD_ID --env SHARD_COUNT \
    --entrypoint bash "$PILOT_CI_IMAGE" .github/scripts/run-build-suite.sh \
    2>&1 | tee .pilot/test.log
result=${PIPESTATUS[0]}
if [[ "$PILOT_SUITE" == docs && "$result" == 0 ]]; then
    # Use the runner's Node and include browser failures in the docs evidence.
    {
        npm ci --prefix scripts/visual-qa --no-audit --no-fund &&
        scripts/visual-qa/node_modules/.bin/playwright install --with-deps --only-shell chromium &&
        node --test docs/site/tests/playground-embed.mjs
    } 2>&1 | tee -a .pilot/test.log
    result=$?
fi
set -e
elapsed=$(( $(date +%s) - started ))
printf '{"exit_code":%s,"test_seconds":%s}\n' "$result" "$elapsed" > .pilot/result.json
docker stats --no-stream > .pilot/docker-stats.txt
docker ps -a > .pilot/docker-ps.txt
sudo chown -R "$(id -u):$(id -g)" .pilot debug-logs
if [[ -n "${PILOT_GO_CACHE_DIR:-}" ]]; then
    sudo chown -R "$(id -u):$(id -g)" "$PILOT_GO_CACHE_DIR"
    du --bytes --summarize "$PILOT_GO_CACHE_DIR" > .pilot/cache-size.txt
fi
printf 'Suite: **%s**; exit: **%s**; duration: **%s seconds**\n' "$PILOT_SUITE" "$result" "$elapsed" >> "$GITHUB_STEP_SUMMARY"
exit "$result"
