#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
chart="${1:-charts/haptic}"
helm template haptic "$chart" --namespace haptic --set cache.varnish.enabled=true \
    --show-only templates/haproxytemplateconfig.yaml |
    yq -er 'select(.kind == "HAProxyTemplateConfig") | .spec.templatingSettings.extraContext.cache.varnish.image | select(length > 0)'
