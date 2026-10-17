#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
fixture="$(mktemp -d)"
trap 'rm -rf "$fixture"' EXIT
mkdir -p "$fixture/scripts" "$fixture/tests/e2e"
cp "$root/scripts/check-test-inventory.sh" "$fixture/scripts/"
git -C "$fixture" init --quiet

check_inventory() {
    bash "$fixture/scripts/check-test-inventory.sh" > "$fixture/result.log" 2>&1
}

printf 'package e2e\n' > "$fixture/tests/e2e/new_test.go"
if check_inventory; then
    echo 'Inventory accepted an untracked e2e test without its build tag' >&2
    exit 1
fi
grep -q 'UNWIRED: tests/e2e/new_test.go' "$fixture/result.log"
printf '//go:build e2e\n\npackage e2e\n' > "$fixture/tests/e2e/new_test.go"
check_inventory

git -C "$fixture" add tests/e2e/new_test.go
printf 'package e2e\n' > "$fixture/tests/e2e/new_test.go"
if check_inventory; then
    echo 'Inventory accepted a tracked e2e test without its build tag' >&2
    exit 1
fi
printf '//go:build e2e\n\npackage e2e\n' > "$fixture/tests/e2e/new_test.go"
printf 'tests/e2e/ignored_test.go\n' > "$fixture/.gitignore"
printf 'package e2e\n' > "$fixture/tests/e2e/ignored_test.go"
check_inventory
mkdir -p "$fixture/tests/process"
printf '//go:build testinfra && unix\n\npackage process\n' > "$fixture/tests/process/process_integration_test.go"
check_inventory
printf '//go:build never_runs\n\npackage process\n' > "$fixture/tests/process/process_integration_test.go"
if check_inventory; then
    echo 'Inventory accepted an unwired process integration test' >&2
    exit 1
fi
printf '//go:build testinfra && unix\n\npackage process\n' > "$fixture/tests/process/process_integration_test.go"

expect_unreached() {
    if check_inventory; then
        echo "Inventory accepted $1" >&2
        exit 1
    fi
    grep -qF "UNREACHED: $2" "$fixture/result.log"
}

acceptance_test() {
    printf '//go:build acceptance\n\npackage acceptance\n\nfunc %s(t *testing.T) {\n%s\n}\n' "$2" "$3" > "$fixture/tests/acceptance/$1"
}

mkdir -p "$fixture/tests/acceptance" "$fixture/.github/scripts"
printf '    acceptance) make test-acceptance-parallel ;;\n' > "$fixture/.github/scripts/run-remaining-suite.sh"
printf 'test-acceptance-parallel:\n\tgo test -run TestAllAcceptanceParallel ./tests/acceptance/...\n' > "$fixture/Makefile"
cat > "$fixture/tests/acceptance/parallel_test.go" <<'EOF'
//go:build acceptance

package acceptance

func TestAllAcceptanceParallel(t *testing.T) {
	features := []types.Feature{
		// Listed features
		buildListedFeature("a/b", 1),
	}
	runFeaturesWithLimit(t, features, 2)
}
EOF
acceptance_test listed_test.go TestListed $'\ttestEnv.Test(t, buildListedFeature("a/b", 1))'
check_inventory

acceptance_test unlisted_test.go TestUnlisted $'\ttestEnv.Test(t, buildUnlistedFeature())'
expect_unreached 'an acceptance test the parallel runner does not list' 'tests/acceptance/unlisted_test.go: TestUnlisted'
acceptance_test unlisted_test.go TestUnlisted $'\ttestEnv.Test(t, buildListedFeature("a/b", 2))'
expect_unreached 'an acceptance test whose feature arguments differ from the listed ones' 'tests/acceptance/unlisted_test.go: TestUnlisted'
acceptance_test unlisted_test.go TestUnlisted $'\tfeature := buildListedFeature("a/b", 1)\n\ttestEnv.Test(t, feature)'
expect_unreached 'an acceptance test that builds its feature inline' 'tests/acceptance/unlisted_test.go: TestUnlisted'
rm "$fixture/tests/acceptance/unlisted_test.go"
check_inventory

printf 'test-acceptance-parallel:\n\tgo test -run TestListed ./tests/acceptance/...\n' > "$fixture/Makefile"
expect_unreached 'a changed acceptance entry point' 'tests/acceptance — '
printf 'Test inventory regression checks passed\n'
