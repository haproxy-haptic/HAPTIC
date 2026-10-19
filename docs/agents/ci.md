# CI and merge verification

A pull request to `main` or a maintenance branch merges once the required
`verified` check passes on its merge with the target branch. The branch need not
be up to date, so the canonical `CI` workflow runs again on every push to the
target; a failed post-merge run blocks further merges until it is fixed. A local
test result, a review comment, and a skipped run cannot replace required CI. These
instructions apply to every agent and future session.

## Before pushing

1. Fetch the current target branch and use a dedicated worktree.
2. Finish the implementation, changelog, local validation, and known review fixes.
3. Run `make lint audit` before `make test`; both can modify or consume `vendor`,
   so never run them in parallel (`make -j` included).
4. For chart changes, also run `./scripts/test-templates.sh`.

## While iterating

- Batch fixes before pushing. Every pull request runs the complete matrix,
  including compatibility, churn, scale, and the upstream canary; prose-only
  changes run the common checks.
- Never add `[skip ci]` or otherwise skip verification.
- Diagnose failures before retrying. Rerun a specific job once for a demonstrated
  infrastructure failure. Fix test failures instead of rerunning until they pass.
- Rebase only for a conflict or another concrete requirement. Post-merge CI
  tests the combined code; routine rebases invalidate CI and review evidence.

## While a pull request is in CI

Waiting is only useful while the pull request can still merge.

- On every check, read `mergeable` and `mergeStateStatus` as well as the jobs.
  When the pull request conflicts, rebase and push right away; a run on a
  conflicting head can't merge. The new head supersedes the running attempt
  (cancel it only if your task owns it); collect review and `verified`
  evidence again for the new head SHA.
- Start investigating the first failed job at once: its log, its artifacts,
  and a local reproduction. Don't wait for the rest of the run or for
  `verified`.
- Prepare and gate the fix locally while the run continues, then push it once.
- If `verified` failed only because it read an incomplete job inventory or a
  GitHub API error while every job passed, rerun it once after the incident
  clears. If the receipt then reports jobs of an earlier attempt as missing,
  rerun the whole run once instead.
- While nothing has failed and the pull request is mergeable, use the wait for
  open verification instead of idling.

## Before merging

1. Read every Gitar review, thread, and summary for the final source SHA. Fix
   valid findings and resolve threads with evidence. A missing review is a
   blocker, not permission to waive review.
2. Confirm the required `verified` check passed for that SHA.
3. Merge the exact reviewed source:

```bash
gh pr merge "$pr_number" \
  --repo haproxy-haptic/HAPTIC \
  --squash --auto --match-head-commit "$reviewed_sha"
```

Never use `--admin` or disable a required check. Success from a manually
dispatched workflow cannot authorize a merge or publication. The receipt checks the exact source,
the complete required job inventory (`.github/ci-verification.json`), and the
run attempt.

## Completion and budget

Report the PR, source SHA, authoritative run, review state, and whether the
change is merged, published, or deployed. Keep those states separate. Report a
running workflow as running, not verified.

Keep CI within the user's EUR 20 monthly limit. Use standard GitHub-hosted
runners, which are free for the public repository, with short artifact retention
and bounded caches. Do not enable paid larger runners, change spending limits,
or change the plan without authorization. Gross metered usage on the billing page
is not the payable amount; check the net amount before a campaign. Cancel only
superseded work owned by your task.

## Feature and matrix coverage

Feature E2E jobs use `make test-e2e-profile` and the exact-name lists in
`tests/e2e/profiles/`. Each feature runs once, including its authentication, WAF,
and ordinary-routing interaction checks. Core HAProxy and Kubernetes matrices
enumerate and shard the complete test inventory. `make test-ci-rules` checks
profile coverage and Go test sharding; `make test-github-verification` checks the
workflow inventory, scheduling, and evidence gates.
