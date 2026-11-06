#!/usr/bin/env bash
# Generate the browser-playground's runtime assets (schema bundle + preset
# configs/resources) into an output directory. These are build artifacts, not
# committed — the shell (cmd/playground/web/) references them by relative path,
# and CI regenerates them per release.
#
# Each preset is one `helm template` render with a specific set of template
# libraries enabled, so the config shown demonstrates exactly that library's
# templates/snippets. Resources are committed static kubectl `kind: List`s
# (cmd/playground/web/presets/*.resources.yaml); the per-vendor ones are derived
# from the ingress example by annotating its "shop" Ingress so the vendor's
# directives actually fire.
#
# Usage: scripts/gen-playground-assets.sh <output-dir>
set -euo pipefail

OUT="${1:?usage: gen-playground-assets.sh <output-dir>}"
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHART="$REPO/charts/haptic"
WEB="$REPO/cmd/playground/web"
SCHEMA_DIR="$REPO/tests/schemas"

command -v helm >/dev/null || { echo "helm not found" >&2; exit 1; }
command -v yq   >/dev/null || { echo "yq not found" >&2; exit 1; }

mkdir -p "$OUT/presets" "$OUT/migration"
echo '{}' > "$OUT/migration/presets.json"

echo "==> schema bundle -> $OUT/schemas.json"
(cd "$REPO" && go run scripts/gen_playground_schema_bundle.go "$SCHEMA_DIR") > "$OUT/schemas.json"

set_preset_sources() {
  local id="$1" sources_csv="$2" sources_json tmp
  sources_json="$(SOURCES_CSV="$sources_csv" yq -n -o=json -I=0 'strenv(SOURCES_CSV) | split(",") | map(select(. != ""))')"
  tmp="$OUT/migration/.presets.json"
  PRESET_ID="$id" SOURCES_JSON="$sources_json" \
    yq -o=json -I=0 '. + {strenv(PRESET_ID): env(SOURCES_JSON)}' "$OUT/migration/presets.json" > "$tmp"
  mv "$tmp" "$OUT/migration/presets.json"
}

set_preset_sources starter ""
set_preset_sources crd ""

declare -A coverage_source_by_library=()
declare -A coverage_library_by_source=()

register_coverage() {
  local file="$1" relative library source count
  relative="${file#"$CHART/charts/"}"
  library="${relative%%/*}"
  count="$(yq '._migrationCoverage | length' "$file")"
  source="$(yq -r '._migrationCoverage[0].source' "$file")"
  if [ "$count" -ne 1 ]; then
    echo "FAIL [$library]: $file must declare exactly one migration source" >&2
    exit 1
  fi
  if [[ ! "$library" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$ ]] ||
     [[ ! "$source" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$ ]]; then
    echo "FAIL [$library]: unsafe migration source id $source" >&2
    exit 1
  fi
  if [ -n "${coverage_source_by_library[$library]+x}" ]; then
    echo "FAIL [$library]: more than one file declares migration coverage" >&2
    exit 1
  fi
  if [ -n "${coverage_library_by_source[$source]+x}" ]; then
    echo "FAIL [$source]: declared by both ${coverage_library_by_source[$source]} and $library" >&2
    exit 1
  fi
  coverage_source_by_library[$library]="$source"
  coverage_library_by_source[$source]="$library"
  yq -o=json -I=0 '._migrationCoverage[0]' "$file" > "$OUT/migration/$source.json"
}

while IFS= read -r coverage_file; do
  register_coverage "$coverage_file"
done < <(grep -rl --include='*.yaml' '^_migrationCoverage:' "$CHART/charts" | LC_ALL=C sort)

# render_config <preset-id> <api-versions:yes|no> <coverage-sources> <overrides…>
# Each override is `key=value`, prefixed with `controller.templateLibraries.`
# unless it carries a `raw:` prefix (then it is passed to --set verbatim, for
# non-templateLibraries values like spoaHub.*).
render_config() {
  local id="$1" apiver="$2" expected_sources="$3"; shift 3
  local sets=() av=()
  local kv
  for kv in "$@"; do
    if [[ "$kv" == raw:* ]]; then sets+=(--set "${kv#raw:}")
    else sets+=(--set "controller.templateLibraries.$kv"); fi
  done
  [ "$apiver" = yes ] && av=(--api-versions=gateway.networking.k8s.io/v1/GatewayClass)
  echo "==> $id config -> $OUT/presets/$id.config.yaml"
  # The chart renders one HAProxyTemplateConfig plus one HAProxyTemplateLibrary
  # per enabled library (ADR-0017), but the playground shows and renders a
  # single config. Merge through the controller's own merge rather than a
  # yq-based one, so the preset is exactly what the controller would assemble.
  local rendered="$OUT/presets/.$id.multi.yaml"
  helm template "$CHART" --namespace default "${av[@]}" "${sets[@]}" \
    | yq 'select(.kind == "HAProxyTemplateConfig" or .kind == "HAProxyTemplateLibrary")' > "$rendered"

  local actual_sources=() slug
  while IFS= read -r slug; do
    if [ -n "${coverage_source_by_library[$slug]+x}" ]; then
      actual_sources+=("${coverage_source_by_library[$slug]}")
    fi
  done < <(yq -r 'select(.kind == "HAProxyTemplateLibrary") | .metadata.labels."haproxy-haptic.org/template-library"' "$rendered")
  local actual_csv
  actual_csv="$(IFS=,; echo "${actual_sources[*]}")"
  if [ "$actual_csv" != "$expected_sources" ]; then
    echo "FAIL [$id]: migration sources $actual_csv, expected $expected_sources" >&2
    exit 1
  fi
  set_preset_sources "$id" "$actual_csv"

  (cd "$REPO" && go run ./cmd/haptic validate -f "$rendered" --dump-merged) \
    | yq '{"apiVersion": "haproxy-haptic.org/v1alpha1", "kind": "HAProxyTemplateConfig", "metadata": {"name": "playground"}, "spec": del(.libraryRefs)}' \
    > "$OUT/presets/$id.config.yaml"
  if yq -e '.spec.migrationCoverage != null or .spec._migrationCoverage != null' "$OUT/presets/$id.config.yaml" >/dev/null 2>&1; then
    echo "FAIL [$id]: emitted config contains migration tooling metadata" >&2
    exit 1
  fi
  rm -f "$rendered"
}

render_config ingress          no  ""                 gateway.enabled=false hapticAnnotations.enabled=false haproxyIngress.enabled=false nginxIngress.enabled=false haproxytech.enabled=false
render_config haproxytech      no  haproxytech        gateway.enabled=false hapticAnnotations.enabled=false haproxytech.enabled=true haproxyIngress.enabled=false nginxIngress.enabled=false
render_config haproxy-ingress  no  haproxy-ingress    gateway.enabled=false hapticAnnotations.enabled=false haproxytech.enabled=false haproxyIngress.enabled=true nginxIngress.enabled=false
render_config nginx-ingress    no  ingress-nginx      gateway.enabled=false hapticAnnotations.enabled=false haproxytech.enabled=false haproxyIngress.enabled=false nginxIngress.enabled=true
# HAPTIC-native's headline features are SPOE-powered (shared rate limiting, SPOE
# auth, traffic mirroring), so the library imports macros that only exist when
# the spoa-hub library is loaded. Enable spoa-hub (and the mirror plugin, which
# otherwise only turns on with the Gateway library) so the preset renders the
# native library as it is meant to run and its validationTests all pass.
render_config haptic-annotations no haptic-annotations gateway.enabled=false hapticAnnotations.enabled=true haproxytech.enabled=false haproxyIngress.enabled=false nginxIngress.enabled=false raw:spoaHub.enabled=true raw:spoaHub.plugins.mirror.enabled=true
render_config gateway          yes ""                 hapticAnnotations.enabled=false haproxyIngress.enabled=false nginxIngress.enabled=false haproxytech.enabled=false
render_config all              yes haptic-annotations,haproxytech,haproxy-ingress,ingress-nginx nginxIngress.enabled=true haproxytech.enabled=true haproxyIngress.enabled=true

# Static example resources (committed) for ingress / gateway / all.
for id in ingress gateway all; do
  echo "==> $id resources -> $OUT/presets/$id.resources.yaml"
  cp "$WEB/presets/$id.resources.yaml" "$OUT/presets/$id.resources.yaml"
done

# Per-vendor resources = the ingress example with the "shop" Ingress annotated so
# the vendor's snippets fire (annotation keys mirror each upstream controller).
gen_vendor_resources() {
  local id="$1" annotations="$2"
  echo "==> $id resources ($id-annotated) -> $OUT/presets/$id.resources.yaml"
  yq "(.items[] | select(.kind == \"Ingress\" and .metadata.name == \"shop\").metadata.annotations) += $annotations" \
    "$WEB/presets/ingress.resources.yaml" > "$OUT/presets/$id.resources.yaml"
}
gen_vendor_resources haproxytech     '{"haproxy.org/timeout-connect":"5s","haproxy.org/timeout-server":"45s","haproxy.org/load-balance":"leastconn","haproxy.org/check-http":"/healthz","haproxy.org/check-interval":"2s","haproxy.org/scale-server-slots":"10","haproxy.org/server-proto":"h2","haproxy.org/request-set-header":"X-Request-Source haproxy-edge","haproxy.org/response-set-header":"X-Frame-Options DENY","haproxy.org/rate-limit-requests":"100","haproxy.org/rate-limit-period":"10s","haproxy.org/cookie-persistence-no-dynamic":"SERVERID","haproxy.org/cors-enable":"true","haproxy.org/cors-allow-methods":"GET, POST, OPTIONS","haproxy.org/ssl-redirect":"true","haproxy.org/allow-list":"10.0.0.0/8, 192.168.0.0/16"}'
gen_vendor_resources haproxy-ingress '{"haproxy-ingress.github.io/balance-algorithm":"leastconn","haproxy-ingress.github.io/timeout-server":"60s","haproxy-ingress.github.io/timeout-connect":"5s","haproxy-ingress.github.io/maxconn-server":"256","haproxy-ingress.github.io/backend-check-interval":"2s","haproxy-ingress.github.io/health-check-uri":"/healthz","haproxy-ingress.github.io/affinity":"cookie","haproxy-ingress.github.io/session-cookie-name":"SHOPSESSION","haproxy-ingress.github.io/forwardfor":"add","haproxy-ingress.github.io/headers":"X-Env:production|X-Frontend:haptic","haproxy-ingress.github.io/cors-enable":"true","haproxy-ingress.github.io/cors-allow-origin":"https://shop.example.com","haproxy-ingress.github.io/backend-protocol":"h2","haproxy-ingress.github.io/limit-rps":"10","haproxy-ingress.github.io/allowlist-source-range":"10.0.0.0/8,192.168.0.0/16","haproxy-ingress.github.io/app-root":"/web"}'
# nginx: two supported directives (fire in haproxy.cfg) plus two annotations the
# migration tab flags — hsts (different: emitted only when explicitly true) and
# server-snippet (dropped: no HAProxy equivalent) — so the migration report
# demonstrates a real mixed verdict rather than an all-clear.
gen_vendor_resources haptic-annotations '{"haproxy-haptic.org/load-balance":"leastconn","haproxy-haptic.org/timeout-connect":"5s","haproxy-haptic.org/timeout-server":"45s","haproxy-haptic.org/health-check-uri":"/healthz","haproxy-haptic.org/health-check-interval":"2s","haproxy-haptic.org/maxconn-server":"256","haproxy-haptic.org/pod-maxconn":"2000","haproxy-haptic.org/affinity":"cookie","haproxy-haptic.org/session-cookie-name":"SHOPSESSION","haproxy-haptic.org/forwardfor":"add","haproxy-haptic.org/cors-enable":"true","haproxy-haptic.org/cors-allow-origin":"https://shop.example.com","haproxy-haptic.org/allowlist-source-range":"10.0.0.0/8,192.168.0.0/16","haproxy-haptic.org/redirect-scheme":"https","haproxy-haptic.org/root-redirect":"/web"}'
gen_vendor_resources nginx-ingress   '{"nginx.ingress.kubernetes.io/proxy-connect-timeout":"10","nginx.ingress.kubernetes.io/proxy-read-timeout":"60","nginx.ingress.kubernetes.io/load-balance":"ewma","nginx.ingress.kubernetes.io/backend-protocol":"HTTPS","nginx.ingress.kubernetes.io/limit-rps":"10","nginx.ingress.kubernetes.io/app-root":"/dashboard","nginx.ingress.kubernetes.io/whitelist-source-range":"10.0.0.0/8,192.168.0.0/16","nginx.ingress.kubernetes.io/denylist-source-range":"203.0.113.0/24","nginx.ingress.kubernetes.io/custom-response-headers":"X-Frame-Options:SAMEORIGIN|X-Content-Type-Options:nosniff","nginx.ingress.kubernetes.io/upstream-vhost":"internal.shop.svc","nginx.ingress.kubernetes.io/proxy-next-upstream-tries":"3","nginx.ingress.kubernetes.io/affinity":"cookie","nginx.ingress.kubernetes.io/session-cookie-name":"SHOPSESSIONID","nginx.ingress.kubernetes.io/enable-cors":"true","nginx.ingress.kubernetes.io/cors-allow-origin":"https://shop.example.com","nginx.ingress.kubernetes.io/hsts":"true"}'

# extend = the ingress preset + the landing's request-id snippet, shown against an
# Ingress that opts in via example.com/request-id-header. The backend hook
# emits the directive only in that Ingress's backends.
echo "==> extend config -> $OUT/presets/extend.config.yaml"
yq ".spec.templateSnippets += load(\"$WEB/presets/extend.snippet.yaml\")" \
  "$OUT/presets/ingress.config.yaml" > "$OUT/presets/extend.config.yaml"
gen_vendor_resources extend '{"example.com/request-id-header":"X-Request-ID"}'
set_preset_sources extend ""

(cd "$REPO" && go run ./cmd/playground/migrationassetcheck "$OUT/migration")

echo "==> done. $(wc -c < "$OUT/schemas.json") bytes of schema, $(ls "$OUT/presets"/*.config.yaml | wc -l) preset configs"
