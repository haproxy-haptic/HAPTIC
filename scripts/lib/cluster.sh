# Source this from shell callers that need the shared backend isolation helper.
kind_blackhole_synthetic_backends() {
  local repo
  repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
  "$repo/scripts/test-infra.sh" blackhole-backends --cluster "$1"
}

kind_install_cni() {
  local repo
  repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
  "$repo/scripts/test-infra.sh" install-cni --cluster "$1"
}
