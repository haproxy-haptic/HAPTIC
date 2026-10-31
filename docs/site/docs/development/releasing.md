# Releasing

## Overview

The controller and the Helm chart are released together: one version number, one `v<version>` git tag, one changelog, one release pipeline. The `VERSION` file is the single source of truth; `charts/haptic/Chart.yaml` carries the same value in both `version` and `appVersion`, and CI refuses to tag a release when they disagree.

Versions follow [Semantic Versioning](https://semver.org/) with support for pre-release suffixes.

## Version numbering

**Format:** `MAJOR.MINOR.PATCH[-PRERELEASE]`

| Type | Example | Description |
|------|---------|-------------|
| Stable release | `0.1.0`, `1.0.0` | Production-ready version |
| Alpha | `0.1.0-alpha.1` | Early testing, APIs may change |
| Beta | `0.1.0-beta.1` | Feature complete, needs testing |
| Release candidate | `0.1.0-rc.1` | Final testing before release |

## `CHANGELOG` conventions

There is one changelog file, `CHANGELOG.md` at the repository root, covering the controller and the Helm chart. It follows the [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format with an `## [Unreleased]` header at the top.

- Controller changes go in a release's top-level `### Added`/`### Changed`/`### Fixed`/… sections.
- Helm chart changes (values, templates, chart defaults, values migrations) go under the release's `### Helm chart` subsection, with `#### Added`/`#### Changed`/… sub-headings.

`charts/haptic/CHANGELOG.md` is a pointer to the root changelog and doesn't take entries.

## Prerequisites

Before releasing:

1. **Clean working directory** - All changes committed
2. **All tests passing** - CI pipeline green on main branch
3. **Documentation updated** - Any new features documented

Run the commands below from a clone with `origin` pointing to
`github.com:haproxy-haptic/HAPTIC`. Authenticate
the [GitHub CLI](https://cli.github.com/) with `gh auth login`, then set:

```bash
haptic_repo=haproxy-haptic/HAPTIC
```

## Publish a main snapshot

Every `main` commit whose canonical `CI` push run succeeds publishes a multi-architecture snapshot: controller images `main-<sha>-haproxy<series>`, the SPOA hub `main-<sha>`, Varnish `main-<sha>`, and chart `0.0.0-main.g<sha>`. The workflow checks every required job and the retained verification receipt, then publishes the exact verified commit. The controller and SPOA images build in parallel; the chart waits for both images and the native amd64 and arm64 SPOA smoke tests. A docs-only commit passes prose CI, which builds no images, so it publishes no snapshot, and a manual dispatch for such a run fails.

After each publication, snapshots older than 180 days are deleted. A package version that also carries a release tag is never deleted.

To publish a snapshot for an earlier successful run again:

```bash
gh run list --repo "$haptic_repo" --workflow ci.yml --branch main \
  --status success --json databaseId,headSha,event,url
read -r -p 'Successful main CI run ID: ' verification_run
gh workflow run snapshot.yml --repo "$haptic_repo" --ref main \
  -f verification_run="$verification_run"
```

## Release process

After publishing and signing controller images, the chart job resolves each
supported HAProxy series to its multi-platform index digest and verifies the
signature against that release's workflow identity. It adds `release-images.json`
to a staging copy of the distributed chart before packaging. Source charts and
image-embedded charts retain their tag fallback; the pre-rollout hook mounts the
distributed metadata into its embedded chart for matching renders. See
[ADR-0038](https://github.com/haproxy-haptic/HAPTIC/blob/main/docs/adr/0038-release-controller-image-digests.md).

Releases go through a pull request (PR). After the merged commit passes canonical
CI, the tagging workflow creates its missing release
tag and dispatches publication.

<a id="step-2-cut-a-release-branch"></a>

### Step 1: Cut a release branch

Branch from current `main`; the release script commits to the checked-out branch.

```bash
read -r -p 'Release version without v: ' release_version
git fetch origin main
git switch -c "release/v$release_version" origin/main
```

<a id="step-1-curate-the-changelog"></a>

### Step 2: Curate the changelog

Rewrite the accumulated `## [Unreleased]` entries in `CHANGELOG.md` into user-facing release notes: merge related entries, and drop items that never shipped in a release. Update the hand-curated `artifacthub.io/changes` annotation in `charts/haptic/Chart.yaml` with summary bullets for the new release.

Don't rename `[Unreleased]` yourself — the release script does the mechanical promotion.
Commit your edits so the script starts with a clean working directory:

```bash
git add CHANGELOG.md charts/haptic/Chart.yaml
git diff --cached --quiet || git commit -m "docs: curate release $release_version"
```

### Step 3: Run the release script

```bash
make release RELEASE_VERSION="$release_version"
```

The script:

- Validates the version format (`X.Y.Z` or `X.Y.Z-suffix.N`)
- Promotes `CHANGELOG.md` `[Unreleased]` to `[<version>] - <date>` and leaves a fresh empty `[Unreleased]` above it (skipped when a `[<version>]` section already exists)
- For a final `X.Y.Z`, folds `[Unreleased]` and every `[X.Y.Z-…]` pre-release section into `[X.Y.Z]`: one list per heading in release order, oldest first, with identical entries once and the newest intro paragraph. It stops when it can't place a section, for example another version's unreleased pre-release, or both `[X.Y.Z]` and `[X.Y.Z-…]` sections
- Writes `<version>` to the `VERSION` file
- Updates `Chart.yaml` `version`, `appVersion`, and the `artifacthub.io/images` annotation (controller and spoa-hub image tags)
- Updates the `helm install ... --version <version>` examples in the READMEs and docs, and the landing page's fallback version
- Keeps commands under `Upgrading to <version>` headings within that version line; older migration steps retain their target version
- Re-pins the controller image tag in the docs' `migrate-check` docker one-liner (`haptic:<version>-haproxy<series>`, with the series taken from `versions.env`'s `DEFAULT_HAPROXY`)
- Pins the chart's hosted documentation links and GitHub or historical GitLab source links to the release version
- Stages and commits everything as `release: haptic v<version>`

The docs-site changelog page needs no release-time sync: it's generated from `CHANGELOG.md` on every mkdocs build (`docs/site/hooks/changelog.py`).

The script **doesn't** create a tag — that happens in CI after the PR merges.

<a id="step-4-push-and-open-the-mr"></a>

### Step 4: Push the release branch

```bash
git push -u origin "release/v$release_version"
```

### Step 5: Prepare the SPOA release image

Run the `Prepare immutable release bundle` workflow on the release branch:

```bash
gh workflow run prepare-spoa-release.yml --repo "$haptic_repo" \
  --ref "release/v$release_version"
gh run list --repo "$haptic_repo" --workflow prepare-spoa-release.yml \
  --branch "release/v$release_version" --json databaseId,headSha,status,conclusion,url
```

Wait for that run to succeed before opening the PR. This separate workflow builds
all three architectures under the chart's release tag, so install tests can pull
the exact image the chart references. The chart remains unpublished.

The job records the upstream image digest, Docker build inputs, and verified
plugin bytes in the image's input identity. An existing release image must
match those inputs and contain all three architectures; it's never rebuilt
or overwritten. If the bundle changes after preparation, choose a new release
version.

Temporary preparation tags use `ci-spoa-prepare-*`. Keep the versioned release
tag: publication verifies and reuses it, signs it, attaches its software bill of
materials, and smoke-tests amd64 and arm64 before publishing the chart.

### Step 6: Open and merge the PR

```bash
gh pr create --repo "$haptic_repo" --base main \
  --head "release/v$release_version" --title "release: haptic v$release_version" \
  --body "Release haptic v$release_version"
```

Finish code review, then merge the reviewed commit once the required PR checks
pass:

```bash
gh pr merge "release/v$release_version" --repo "$haptic_repo" \
  --squash --auto --match-head-commit "$(git rev-parse HEAD)"
```

Don't use `--admin` to bypass the required checks.

### Automatic tag creation

After the PR is merged, CI automatically:

1. Runs full canonical CI on the merged commit
2. Verifies `VERSION`, `Chart.yaml` `version`, and `appVersion` agree
3. Creates the annotated `v<version>` tag on the verified commit
4. Dispatches `release.yml` with that tag and the canonical CI run ID

??? note "Recover a failed release dispatch"
    Inspect the `Tag verified releases` logs first. If tag creation failed, fix the
    cause and rerun that workflow. If the tag exists on the verified commit but
    dispatch failed, start publication explicitly:

    ```bash
    read -r -p 'Successful canonical CI run ID for this release: ' verification_run
    gh workflow run release.yml --repo "$haptic_repo" --ref "v$release_version" \
      -f verification_run="$verification_run"
    ```

    Pushing a tag alone doesn't start publication. The release workflow rejects
    missing verification or a tag that differs from the verified source.

### What CI does automatically

The dispatched release workflow:

1. **Verify and sign the prepared SPOA image** (`spoa-hub:<version>`) without rebuilding it, then attach its software bill of materials (SBOM)
2. **Build controller binaries and images** for linux/amd64, linux/arm64, and linux/arm/v7, including every supported HAProxy series and the Varnish image
3. **Create the GitHub release** with binary archives, signed SHA256 checksums, and release notes from `CHANGELOG.md`; mark alpha, beta, and release candidates as pre-releases
4. **Sign the controller and Varnish images** and attach their SBOM attestations
5. **Require native amd64 and arm64 SPOA smoke tests**, which run alongside the controller build
6. **Publish the signed Helm chart** to `ghcr.io/haproxy-haptic/haptic/charts/haptic` after the images and smoke tests pass
7. **Run release conformance** against the published image and retain the
   [report and provenance](../operations/gateway-conformance.md) without scheduled expiration
8. **Publish versioned documentation** after the chart and release conformance succeed

Signing uses Cosign with keyless OpenID Connect (OIDC).

The conformance job runs after publication. A failed job marks the release
pipeline failed but doesn't retract published artifacts; inspect its report and
fix the failure before claiming conformance for that release.

If the controller job fails after it created the GitHub release, rerun the failed
jobs. The rerun reuses that release and its images, then finishes signing. If the
release lacks an asset its signed checksums list, the job stops; delete the release
with `gh release delete "v$release_version" --repo "$haptic_repo"` (the tag stays)
and rerun the failed jobs.

### Add the release to the upgrade test

Once the chart is published, add upgrade jobs for the new stable version on both
current and minimum Kubernetes to `.github/workflows/ci-speed-proof.yml`. Add both
jobs to `.github/ci-verification.json` and `.github/job-duration-weights.json`, then
run `make schedule-github-verification`. Until you add it,
`check-chart-upgrade-baselines` fails on every pipeline that runs the upgrade
tests, so the new release can't go untested. Pre-releases aren't required.
The check covers published stable versions at or below `VERSION`; a matrix
entry above `VERSION` also fails it.

## Patch releases for an older release line

A patch release for an older minor version (for example `0.2.2` while `main` already carries `0.3.0` work) is cut from a maintenance branch, `maint/<major>.<minor>`, instead of `main`.

### When to use a maintenance branch

Port only security fixes and severe regressions, and only to the latest released minor version. Never port a feature: a patch release must not change behaviour beyond the fix. If `main` holds nothing unreleased beyond fixes, release from `main` instead.

The fix lands on `main` first, or in parallel; the port is a separate PR.

### Step 1: Create the maintenance branch

Branch from the last tag of the release line. Apply the same review, required CI,
and force-push protections as `main` before accepting PRs.

```bash
read -r -p 'Maintenance line (major.minor): ' maintenance_line
read -r -p 'Last released version on that line without v: ' maintenance_base
git fetch origin --tags
git push origin "v$maintenance_base^{commit}:refs/heads/maint/$maintenance_line"
```

Skip this step if the branch already exists.

### Step 2: Port the fix

Branch off the maintenance branch:

```bash
read -r -p 'Fix branch name: ' fix_branch
git fetch origin
git switch -c "$fix_branch" "origin/maint/$maintenance_line"
```

Adapt the fix to the older code instead of cherry-picking it blindly: helpers, file layout, and tests may differ. Port the tests that prove the fix, and add the `CHANGELOG.md` entry under `## [Unreleased]` on the maintenance branch.

```bash
git push -u origin "$fix_branch"
gh pr create --repo "$haptic_repo" --base "maint/$maintenance_line" --head "$fix_branch"
```

### Step 3: Prepare the release

Once the fix is merged, prepare the release on a branch named exactly `release/v<version>`. The `prepare-spoa-release` job only runs for that name.

```bash
read -r -p 'Patch release version without v: ' release_version
git fetch origin
git switch -c "release/v$release_version" "origin/maint/$maintenance_line"
make release RELEASE_VERSION="$release_version"
```

Update the `artifacthub.io/changes` annotation in `charts/haptic/Chart.yaml` for
the new version, then commit and push the release branch:

```bash
git add charts/haptic/Chart.yaml
git diff --cached --quiet || git commit -m "docs: describe release $release_version"
git push -u origin "release/v$release_version"
```

Complete [SPOA image preparation](#step-5-prepare-the-spoa-release-image), then
open the PR into the maintenance branch:

```bash
gh pr create --repo "$haptic_repo" --base "maint/$maintenance_line" \
  --head "release/v$release_version" --title "release: haptic v$release_version" \
  --body "Release haptic v$release_version"
```

### Step 4: Merge and let CI tag

Complete review and use the same merge command as
[Step 6](#step-6-open-and-merge-the-pr). Maintenance PRs run the same full matrix
as `main`, including every supported HAProxy version and the conformance suites.
Canonical post-merge CI authorizes `Tag verified releases`, with the same
`VERSION`/`Chart.yaml` check as on `main`.

`test-chart-upgrade` upgrades only from published stable versions at or below the branch's `VERSION`, so a release from a newer line, such as `0.3.0` on `maint/0.2`, is never used as a baseline.

A maintenance branch never publishes `main`-only artifacts: no `main-<sha>` snapshots, no `dev` docs, no landing page, and no `/playground/dev/`.

### What moves to the new version

| Artifact | Behaviour for a maintenance release |
|----------|-------------------------------------|
| `latest-haproxy<series>` and `spoa-hub:latest` image tags | Move only when the version is the highest stable `v*` tag (`scripts/release-is-latest.sh`) |
| `/docs/<version>/` and `/playground/<version>/` | Added; no other version directory changes |
| `latest` docs alias | Points at the highest stable version by semver, so it stays on the newer line |
| `dev` docs and landing page | Unchanged; only `main` rebuilds them |

### Step 5: Follow up on main

After the chart is published, open one PR into `main` that:

1. Adds the `## [<version>]` section to `CHANGELOG.md`, in version order below `## [Unreleased]`.
2. Adds the version to both Kubernetes variants of the [upgrade tests](#add-the-release-to-the-upgrade-test).
3. Runs `./scripts/release.sh <version>` if the release line is the newest one, so `VERSION`, `Chart.yaml`, and the install examples on `main` point at the patched version. The script skips the changelog promotion because the section from item 1 already exists.

Don't open it before the chart is published: `check-chart-upgrade-baselines` fails on a matrix entry that isn't published yet.

### Security fixes

Use a [draft repository security advisory](https://docs.github.com/en/code-security/how-tos/report-and-fix-vulnerabilities/fix-reported-vulnerabilities/create-repository-advisory)
to discuss an undisclosed vulnerability privately. Collaborate in its temporary
private fork; a public issue or PR exposes its contents. Keep the advisory private
until the fix is released.

List the fix under `### Security` (or `#### Security` in the Helm chart subsection)
in `CHANGELOG.md`, without an exploit recipe. Before publishing the advisory,
record affected and fixed versions. You can
[request a CVE ID from GitHub](https://docs.github.com/en/code-security/how-tos/report-and-fix-vulnerabilities/fix-reported-vulnerabilities/publish-repository-advisory)
through the advisory if one hasn't already been assigned.

## Documentation versioning

Each release creates a versioned snapshot under `/docs/<version>/`. The version menu
keeps older stable releases available and marks the highest stable version as `latest`.

| Release type | Documentation behavior |
|--------------|------------------------|
| Stable (`0.3.0`) | Creates a snapshot; updates `latest` if this is the newest stable release |
| Prerelease (`0.3.0-alpha.1`) | Creates a snapshot without the `latest` alias |
| Final after pre-releases | Removes alpha, beta, and rc snapshots for that exact version |

For example, publishing `0.3.0` creates `/docs/0.3.0/` and removes
`/docs/0.3.0-alpha.1/`. It keeps `0.2.0`, `0.1.0`, `dev`, and any pre-releases of
later versions. Cleanup covers the documentation, playground, and their version
menus on every publication. Retrying an old prerelease can't bring it back after
its final release exists. Links into `/docs/0.3.0-alpha.1/` and
`/playground/0.3.0-alpha.1/` redirect to the matching `0.3.0` page.

The 0.1.0 entry links to its original, separate controller and Helm chart docs.
The playground menu only includes releases with a published playground bundle.

## Pre-release vs final release

### Pre-releases

Pre-releases (alpha, beta, rc) have these differences:

- Docker images built but **don't get `latest` tag**
- Documentation created but **not marked as `latest`**
- GitHub release marked as **pre-release**
- Not recommended for production use

### Final releases

Final releases (no suffix):

- Docker images get `latest` tags if the version is the highest stable release
- Documentation gets `latest` alias if the version is the highest stable release
- Pre-release documentation versions are removed
- Recommended for production use

## Troubleshooting

### Release script fails

| Error | Solution |
|-------|----------|
| `Warning: Working directory is not clean` — an interactive `Continue anyway? [y/N]` prompt, not a hard failure | Answer `N`, commit or stash your changes, and re-run |
| `CHANGELOG.md has no [Unreleased] section` | Restore the `## [Unreleased]` header |
| `Invalid version format` | Use `X.Y.Z` or `X.Y.Z-suffix.N` |

### CI pipeline fails

1. Open the failed run in GitHub Actions and read the first failed step.
2. Fix the cause and run `make lint audit` before `make test` locally.
3. Rerun the affected workflow only after the fix or a diagnosed infrastructure failure.

### Docker image missing

If images don't appear after release:

1. Check the release workflow's `controller` job completed
2. Verify registry authentication succeeded
3. Check for build errors in job logs

## Supply chain security

All release artifacts are signed and include security metadata:

### Artifact signing

All artifacts are signed with [Cosign](https://github.com/sigstore/cosign) using keyless OIDC:

- **Binaries**: Checksums file signed with a `.sigstore.json` verification bundle
- **Docker images**: Each image tag signed
- **Helm chart**: OCI artifact signed

**Verify image signature:**

Release tags produce one image per supported HAProxy series
(`<version>-haproxy<series>`). Images published by GitHub Actions use its OIDC
issuer and the exact release workflow and tag as their certificate identity:

```bash
read -r -p 'GitHub-published release version without v: ' release_version
release_identity="https://github.com/$haptic_repo/.github/workflows/release.yml@refs/tags/v$release_version"
cosign verify \
  --certificate-identity="$release_identity" \
  --certificate-oidc-issuer='https://token.actions.githubusercontent.com' \
  "ghcr.io/haproxy-haptic/haptic:$release_version-haproxy3.4"
```

Historical releases copied from GitLab retain their original signatures and
GitLab issuer. Copying an image into GitHub Container Registry doesn't change who
signed it. For example, verify the copied 0.1.0 image with its original issuer:

```bash
cosign verify \
  --certificate-identity-regexp='https://gitlab.com/haproxy-haptic/.*' \
  --certificate-oidc-issuer='https://gitlab.com' \
  ghcr.io/haproxy-haptic/haptic:0.1.0-haproxy3.4
```

### SBOM (software bill of materials)

Each controller image includes an SBOM attestation in Software Package Data Exchange (SPDX) format (the `spoa-hub` image uses CycloneDX — see [Stream Processing Offload Agent (SPOA) Hub](../operations/spoa-hub.md)):

**View SBOM:**

For the GitHub-published release selected above:

```bash
cosign verify-attestation \
  --type spdxjson \
  --certificate-identity="$release_identity" \
  --certificate-oidc-issuer='https://token.actions.githubusercontent.com' \
  "ghcr.io/haproxy-haptic/haptic:$release_version-haproxy3.4" \
  | jq -r '.payload' | base64 -d | jq '.predicate'
```

The SBOM lists all packages, libraries, and dependencies in the container image.

## Version files reference

| File | Content | Updated By |
|------|---------|------------|
| `VERSION` | Release version (single source of truth) | Release script |
| `Chart.yaml:version` | Same value as `VERSION` | Release script |
| `Chart.yaml:appVersion` | Same value as `VERSION` | Release script |
| `Chart.yaml` `artifacthub.io/images` | Controller and spoa-hub image tags | Release script |
| `Chart.yaml` `artifacthub.io/changes` | Release summary bullets | By hand (Step 2) |
