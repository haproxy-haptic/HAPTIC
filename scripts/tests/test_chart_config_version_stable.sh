#!/usr/bin/env bash
# A chart upgrade that changes only the chart version must not change any
# config spec (#461) or pod template (#467): every running controller would
# treat the first as a config change, and the second rolls the pods.
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

cat >"$test_dir/pinned.yaml" <<'VALUES'
controller:
  image: {tag: pinned}
spoaHub:
  image: {tag: pinned}
credentials:
  dataplane: {password: pinned}
VALUES

# Images follow appVersion by design, and an unset password is random per
# render, so only the pod-template render pins them. Hook Jobs are recreated on
# every upgrade anyway.
stable_specs() {
    helm template haptic "$1" --namespace haptic "${@:2}" |
        yq ea '[select(.kind == "HAProxyTemplateConfig" or .kind == "HAProxyTemplateLibrary")] | .[] | {"kind": .kind, "name": .metadata.name, "spec": .spec}'
    helm template haptic "$1" --namespace haptic "${@:2}" -f "$test_dir/pinned.yaml" |
        yq ea '[select((.spec.template or .spec.jobTemplate) and .metadata.annotations."helm.sh/hook" == null)] | .[] | {"kind": .kind, "name": .metadata.name, "template": (.spec.template // .spec.jobTemplate)}'
}

for version in 1.0.0 2.0.0; do
    cp -a charts/haptic "$test_dir/$version"
    yq -i ".version = \"$version\" | .appVersion = \"$version\"" "$test_dir/$version/Chart.yaml"
done

for values in "" "$test_dir/all-libraries.yaml"; do
    args=()
    [[ -n "$values" ]] && args=(-f "$values")
    stable_specs "$test_dir/1.0.0" "${args[@]}" >"$test_dir/old.yaml"
    stable_specs "$test_dir/2.0.0" "${args[@]}" >"$test_dir/new.yaml"
    grep -q 'kind: HAProxyTemplateConfig' "$test_dir/old.yaml"
    grep -q 'name: haptic-controller' "$test_dir/old.yaml"
    if ! diff -u "$test_dir/old.yaml" "$test_dir/new.yaml"; then
        echo "Config specs or pod templates differ between chart versions${values:+ with $(basename "$values")}" >&2
        exit 1
    fi
done
echo "Chart config and pod template version-stability tests passed"
