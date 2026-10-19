#!/usr/bin/env bash
# A chart upgrade that changes only the chart version must not change any
# config spec: every running controller would treat it as a config change (#461).
set -euo pipefail

cd "$(dirname "$0")/../.."
test_dir="$(mktemp -d)"
trap 'rm -rf "$test_dir"' EXIT

cat >"$test_dir/all-libraries.yaml" <<'VALUES'
controller:
  templateLibraries:
    gateway: {enabled: true}
    governance: {enabled: true}
    hapticAnnotations: {enabled: true}
    haproxytech: {enabled: true}
    haproxyIngress: {enabled: true}
    nginxIngress: {enabled: true}
    ingressAnnotationsCompat: {enabled: true}
rateLimit:
  shared:
    enabled: true
VALUES

config_specs() {
    helm template haptic "$1" --namespace haptic "${@:2}" |
        yq ea '[select(.kind == "HAProxyTemplateConfig" or .kind == "HAProxyTemplateLibrary")] | .[] | {"kind": .kind, "name": .metadata.name, "spec": .spec}'
}

for version in 1.0.0 2.0.0; do
    cp -a charts/haptic "$test_dir/$version"
    yq -i ".version = \"$version\" | .appVersion = \"$version\"" "$test_dir/$version/Chart.yaml"
done

for values in "" "$test_dir/all-libraries.yaml"; do
    args=()
    [[ -n "$values" ]] && args=(-f "$values")
    config_specs "$test_dir/1.0.0" "${args[@]}" >"$test_dir/old.yaml"
    config_specs "$test_dir/2.0.0" "${args[@]}" >"$test_dir/new.yaml"
    grep -q 'kind: HAProxyTemplateConfig' "$test_dir/old.yaml"
    if ! diff -u "$test_dir/old.yaml" "$test_dir/new.yaml"; then
        echo "Config specs differ between chart versions${values:+ with $(basename "$values")}" >&2
        exit 1
    fi
done
echo "Chart config version-stability tests passed"
