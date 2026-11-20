# GitHub verification scheduling

Pull requests and pushes to `main` or `maint/*` run seven common checks when
all changed paths are website sources or listed prose files. Other changes run
the full matrix in `.github/workflows/ci-speed-proof.yml`. No workflow runs on a schedule. Only the upstream Gateway
API canary may fail; a missing, skipped, or cancelled job fails the verification
receipt, which binds all jobs to the same source and run attempt.

`.github/job-duration-weights.json` records each job's upper-quartile runner
duration across the successful CI runs in `source_runs`. Re-measure it when suite
durations drift with `make schedule-github-verification RECORD_RUNS="<run-id> ..."`,
passing at least five recent successful runs; stale weights put a long suite behind
another in one lane, which sets the wall clock. `make schedule-github-verification`
places long suites first and fills all 60 execution lanes with shorter suites, and
prints the longest planned lane. Coverage aggregation takes over the lane
of its longest measured producer and still waits for all six inputs. Coverage
producers start immediately, alongside the command and general unit-test cache
producers. Command shards run sequentially on fresh runners, and support tests
follow the general unit tests, so consumers reuse their predecessors' compiled
packages. Every suite still executes when its predecessor fails and no cache is
available. After the initial placement, the scheduler swaps groups between lanes
to shorten the longest chains while keeping cache consumers together and
coverage producers at the start. Weights predict scheduling; only a completed
first-attempt workflow measures lead time.

Renderer verification uses three disjoint shards, selected from the compiled
inventory with tests, examples, and fuzz seeds included. All retain `-race` and
`-count=1`, and restore the same renderer compiler cache with one writer.

Native suites share the architecture- and Go-version-specific compiler and
module cache. The dependency key covers `go.mod`, `go.sum`, and the PGO profile;
Go's build cache checks source, compiler options, and compiler identity itself.
The native image is built from the current checkout with source identity checks.

## Isolated suites

The Gateway API compatibility versions, churn, scale, and the upstream canary
each keep a fresh cluster: compatibility jobs change cluster-wide CRDs, churn
changes diagnostic settings, and scale measures latency and memory in isolation.
They lengthen the matrix by about a minute (22m12s and 21m42s in runs
`38534640184` and `38555150518`, against 20m53s without them in run
`38432655131`), which buys one gate instead of a separate schedule. A canary
failure on `main` opens a non-blocking `Canary failure` issue.

`make check-chart-upgrade-baselines` compares the registry's released versions
with the CI upgrade jobs. CI must cover each baseline on current and minimum
Kubernetes, use distinct node images for those variants, and require every
upgrade job in its final verification gate. Missing, duplicate, or unexpected
baselines fail the check.
