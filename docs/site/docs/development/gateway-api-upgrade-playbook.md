# Gateway API upgrade playbook

Mechanical checklist for bumping the `sigs.k8s.io/gateway-api` dependency.
Distilled from the v1.6.0 upgrade retrospective
([issue #63](https://github.com/haproxy-haptic/HAPTIC/issues/63)): the bump
itself takes under an hour — the cost lives in suite adaptation and
verification. Work through the sections in order.

## Before the bump

1. Check the canary results. The `gateway-canary` CI job runs the
    conformance suite from gateway-api's `main` branch in every CI run, so the
    next release's breakage should already be filed as canary-flagged
    `needs-triage` issues. Review them before you start:

    ```bash
    gh issue list --repo haproxy-haptic/HAPTIC --label needs-triage
    ```

2. Review the [upstream release notes](https://github.com/kubernetes-sigs/gateway-api/releases)
    for:

    - new conformance tests — they run automatically once the suite module is
      bumped (see [Suite adaptation](#suite-adaptation)),
    - features moving between the standard and experimental channels,
    - CRD schema changes — new fields matter for the chart's `requires` /
      `requiresFields` stripping.

## The bump

1. Take the Renovate MR, or bump manually. `go.mod` pins **two** modules —
    the API types and the conformance suite — and they must move together:

    ```bash
    go get sigs.k8s.io/gateway-api@v1.7.0 sigs.k8s.io/gateway-api/conformance@v1.7.0
    go mod tidy
    go mod vendor
    ```

2. Refresh the offline schemas. The target downloads the release pinned in
    `go.mod` and copies its experimental-channel gateway CRDs into
    `tests/schemas/`. The main template profile tests the experimental
    channel; the `tests/schemas-ga-*` bundles cover the standard channel.
    Commit the result: a test whose `requiresFields` the schemas lack is
    stripped, not failed.

    ```bash
    make extract-schemas
    ```

3. Add an old-release schema bundle if the previous default release should
    join the degraded-profile coverage (the `tests/schemas-ga-*` bundles):

    ```bash
    scripts/fetch-gateway-api-schemas.sh v1.6.0 standard tests/schemas-ga-v1.6
    ```

    Then create `tests/schemas-ga-v1.6/expected-stripped.txt`: run
    `./scripts/test-templates.sh` and seed the allowlist from the degraded
    profile's `⊘ <name> stripped:` output lines (one test name per line,
    `#` comments allowed).

4. Regenerate the gateway library's `requires` annotations:

    ```bash
    helm template charts/haptic \
      --set controller.templateLibraries.gateway.experimentalChannel=true \
      | yq 'select(.kind == "HAProxyTemplateConfig" or .kind == "HAProxyTemplateLibrary")' \
      | yq ea '. as $i ireduce ({}; . * $i)' > /tmp/merged.yaml
    python3 scripts/gen-gateway-requires.py /tmp/merged.yaml
    ```

5. Nothing to re-pin. The conformance suite refuses to run when the installed
    CRDs and the suite disagree on `bundle-version`, so both installers derive
    the release from `go.mod` rather than copying it:

    - `defaultGatewayAPIVersion` in `tests/e2e/main_test.go` is
      `consts.BundleVersion`, taken from the module itself
    - `scripts/start-dev-env.sh` resolves the `standard-install.yaml` URL from
      `go list -m sigs.k8s.io/gateway-api`

    `make lint` runs `scripts/check-gwapi-version-pins.sh`, which fails if a
    version literal reappears in either place.

6. Update the old-release matrix. If you added a schema bundle in step 3,
    add a `gateway-compatibility-<release>` job to
    `.github/workflows/ci-speed-proof.yml` and its name to `required_jobs` in
    `.github/ci-verification.json`; then run `make schedule-github-verification`.
    The list mirrors the `tests/schemas-ga-*` bundles.

## Suite adaptation

1. Diff `SupportedFeatures`. `tests/conformance/gateway_conformance_test.go`
    declares every standard-channel entry of `features.AllFeatures` minus an
    explicit `excluded` set, so features the new release adds enroll
    automatically. For each new feature, either implement it in the chart or
    add it to `excluded` with a concrete upstream-capability reason — never
    under-declare to dodge a failing test.

2. Review the new tests before CI meets them. The shard script enumerates
    the suite's test names from the module cache, and with a single shard it
    prints them all — run it before and after the bump and diff:

    ```bash
    scripts/shard-conformance-tests.sh 1 1
    ```

3. Expect the shards to rebalance. `scripts/shard-conformance-tests.sh` maps
    each test name to a shard via a CRC32 hash modulo the shard count, so new
    upstream tests shift existing assignments and shard durations change. If
    a shard outgrows its job timeout, add a `gateway-<n>` shard job to
    `.github/workflows/ci-speed-proof.yml` and raise the shard count those
    jobs pass to `scripts/shard-conformance-tests.sh`.

4. Skip individual broken upstream tests via `opts.SkipTests`, each entry
    with an issue link in a comment. Never `t.Skip()` the whole suite.

5. Leave the suite's consistency budgets alone. `MaxTimeToConsistency` and
    the object-status budgets stay at upstream defaults because they absorb
    cluster-infra latency (kube-proxy Destination Network Address Translation (DNAT) programming, apiserver churn) that
    HAPTIC can't control; HAPTIC's own latency contracts live in dedicated
    e2e assertions instead. The full rationale is encoded in the
    budget-doctrine comments in
    `tests/conformance/gateway_conformance_test.go` — keep those comments
    intact in review.

## Verification

1. Run the offline template harness. The degraded profiles must pass with
    zero failing tests, and each bundle's stripped set must exactly match its
    `expected-stripped.txt`:

    ```bash
    ./scripts/test-templates.sh
    ```

2. Check the pull request's CI run: it includes the compatibility matrix,
    churn, scale, and `gateway-canary`, proving them live on the new release.
    Retry only failed jobs whose inputs match.

3. Debug any failing conformance test locally on a **fresh** kind cluster —
    never take a verdict from a reused cluster:

    ```bash
    kind delete cluster --name haptic-e2e
    TEST_RUN_PATTERN="^$" make test-e2e
    TEST_RUN_PATTERN='^TestGatewayAPIConformance/HTTPRouteWeight$' make test-gateway-conformance
    ```

    `TEST_RUN_PATTERN="^$"` boots the cluster and chart without running the
    e2e tests; the second pattern selects one conformance test by its
    upstream `ShortName`.

## Process guardrails

- **Deliverable-ref discipline.** Before the first fix, write down the MR
  whose head pipeline must go green. Land every fix on that branch, and cite
  that pipeline's ID in every "green" claim. In the v1.6.0 campaign, fixes
  accumulated on a side branch for hours while the MR pipeline stayed red.
- **Commit-content verification.** Before declaring a fix pushed, diff the
  pushed commit against the intended change — the commit message isn't the
  content. In the v1.6.0 campaign, a fix commit shipped only its CHANGELOG
  line (the `values.yaml` edit was never staged) and burned two CI rounds:

    ```bash
    git show --stat origin/$(git branch --show-current)
    ```
