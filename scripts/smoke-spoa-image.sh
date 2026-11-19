#!/usr/bin/env bash
# Boot the published spoa-hub image with a minimal config that loads the
# bundled plugins that do not require fixture files, then assert the hub log
# reports each plugin loaded and the SPOP listener bound. Run inside a
# docker-in-docker job.
#
# Usage: scripts/smoke-spoa-image.sh <image-ref>

set -euo pipefail

IMAGE="${1:?usage: $0 <image-ref>}"

# Plugin name -> library filename (matches what prep-spoa-plugins.sh stages
# under /etc/haproxy-spoa-hub/plugins/). sso-auth differs from the rest.
declare -A PLUGIN_LIBS=(
    [api-gateway]="libapi_gateway_plugin.so"
    [coraza]="libcoraza_plugin.so"
    [coraza-rust]="libcoraza_rust_plugin.so"
    [external-auth]="libexternal_auth_plugin.so"
    [fingerprinting]="libfingerprinting_plugin.so"
    [maxmind]="libmaxmind_plugin.so"
    [mirror]="libmirror_plugin.so"
    [rate-limit]="librate_limit_plugin.so"
    [sso-auth]="libhaproxy_spoa_hub_plugin_sso_auth.so"
)
# Per-plugin minimal `[plugins.params]` body. The hub's plugin loader
# runs each plugin's JSON-Schema validation before init(), so plugins
# that declare required fields must be given enough config to pass.
# The values below are placeholders chosen to satisfy schemas with
# zero external-resource dependency:
#  - sso-auth needs cookie_secret_1 (>=16 chars) + cookie_domain.
#  - api-gateway / coraza / external-auth / fingerprinting / mirror / rate-limit tolerate empty params.
# maxmind is excluded entirely because its schema requires at least
# one `databases.<name>.path` pointing at a real MMDB file we can't
# realistically synthesize in CI; smoke for that plugin happens via
# the chart's test-integration / test-acceptance jobs which mount a
# fixture MMDB into a real container.
declare -A PLUGIN_PARAMS=(
    [api-gateway]=""
    [coraza]=""
    [external-auth]=""
    [fingerprinting]=""
    [mirror]=""
    [rate-limit]=""
    [sso-auth]='[plugins.params]
cookie_secret_1 = "0123456789abcdef0"
cookie_domain = "smoke.example.com"'
)
PLUGINS=(api-gateway coraza coraza-rust external-auth fingerprinting mirror rate-limit sso-auth)

echo "==> Pulling ${IMAGE}"
docker pull "${IMAGE}"

# Build a derived image with a minimal config bound on TCP that lists all
# bundled plugins. Each plugin block matches the libname produced by
# prep-spoa-plugins.sh (suffix stripped, so the file is plain libfoo_plugin.so).
echo "==> Building smoke image with all-plugin config"

plugin_blocks=""
for p in "${PLUGINS[@]}"; do
    name="${p//-/_}"
    lib="${PLUGIN_LIBS[${p}]}"
    params="${PLUGIN_PARAMS[${p}]:-}"
    plugin_blocks="${plugin_blocks}
[[plugins]]
name = \"${name}\"
library = \"${lib}\"
messages = []
"
    # Only emit a params block when the plugin needs one; an empty
    # `[plugins.params]` table is fine for schema-less plugins but the
    # explicit body is what satisfies plugins with required fields.
    if [ -n "${params}" ]; then
        plugin_blocks="${plugin_blocks}
${params}
"
    fi
done

cat > Dockerfile.smoke <<DOCKERFILE
FROM ${IMAGE}
RUN cat > /etc/haproxy-spoa-hub/config.toml <<EOF
plugin_dir = "/etc/haproxy-spoa-hub/plugins"
default_timeout_ms = 500
log_level = "debug"

[[listeners]]
type = "tcp"
address = "0.0.0.0:12345"
${plugin_blocks}
EOF
DOCKERFILE
docker build -t spoa-smoke -f Dockerfile.smoke .

echo "==> Running smoke container"
docker run -d --name spoa-smoke spoa-smoke

cleanup() {
    docker logs spoa-smoke 2>&1 | tail -100 || true
    docker stop spoa-smoke >/dev/null 2>&1 || true
    docker rm spoa-smoke >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Wait for the hub to finish loading all plugins (max 15s). The hub emits
# `"all plugins loaded","count":N` once per startup; that's the canonical
# barrier to check before per-plugin assertions, and it side-steps the
# log-flush race seen when grepping for individual `plugin loaded` lines
# right after `listening`.
echo "==> Waiting for hub startup to complete"
for i in $(seq 1 15); do
    if [ "$(docker inspect -f '{{.State.Running}}' spoa-smoke 2>/dev/null)" != "true" ]; then
        echo "FAIL: container exited unexpectedly"
        exit 1
    fi
    if docker logs spoa-smoke 2>&1 | grep -q '"all plugins loaded"'; then
        echo "OK: hub reports startup complete after ${i}s"
        break
    fi
    if [ "$i" -eq 15 ]; then
        echo "FAIL: hub did not finish loading plugins within 15s"
        exit 1
    fi
    sleep 1
done

logs=$(docker logs spoa-smoke 2>&1)

# count: the hub's "all plugins loaded" event reports how many plugins
# successfully loaded. Fewer than expected means at least one plugin
# failed at load time (typically a missing runtime shared library).
count=$(printf '%s' "$logs" | grep -o '"all plugins loaded","count":[0-9]\+' | grep -o '[0-9]\+$' | tail -1)
expected=${#PLUGINS[@]}
if [ "${count}" != "${expected}" ]; then
    echo "FAIL: expected ${expected} plugins loaded, got ${count}"
    echo "${logs}" | grep -E '"failed to load plugin"|"plugin loaded"' || true
    exit 1
fi

echo "==> Verifying every bundled plugin reported loaded"
for plugin in "${PLUGINS[@]}"; do
    name="${plugin//-/_}"
    if ! printf '%s' "$logs" | grep -q "\"plugin loaded\",\"plugin\":\"${name}\""; then
        echo "FAIL: plugin '${name}' did not report loaded"
        exit 1
    fi
done
echo "OK: every bundled plugin (${PLUGINS[*]}) reported loaded"

bash "$(dirname "${BASH_SOURCE[0]}")/test-spoa-reload.sh" "$IMAGE"

echo
echo "PASS - smoke test for ${IMAGE}"
