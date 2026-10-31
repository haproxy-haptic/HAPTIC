import hashlib
import json
from pathlib import Path
import re
import subprocess
import unittest

import yaml


ROOT = Path(__file__).parents[2]


def workflow(name):
    return yaml.safe_load((ROOT / ".github/workflows" / name).read_text())


class ReleaseWorkflowTests(unittest.TestCase):
    def setUp(self):
        self.release = workflow("release.yml")
        self.script = (ROOT / "scripts/publish-github-release.sh").read_text()

    def test_release_work_directories_are_ignored(self):
        scripts = self.script + (ROOT / ".github/scripts/run-release-publication.sh").read_text()
        directories = set(re.findall(r"(?<![\w./-])(\.[a-z][a-z-]*)/", scripts)) - {".github"}
        self.assertTrue({".pilot", ".publication"} <= directories, directories)
        for directory in sorted(directories):
            with self.subTest(directory=directory):
                ignored = subprocess.run(["git", "check-ignore", "-q", f"{directory}/probe"], cwd=ROOT, check=False)
                self.assertEqual(ignored.returncode, 0, f"{directory}/ makes goreleaser see a dirty tree")

    def test_chart_publishes_only_after_the_signed_runtime_and_both_smokes(self):
        jobs = self.release["jobs"]
        self.assertEqual(set(jobs["chart"]["needs"]), {"source", "spoa", "controller", "smoke"})
        self.assertNotIn("if", jobs["chart"])
        self.assertEqual(set(jobs["smoke"]["strategy"]["matrix"]["runner"]), {"ubuntu-24.04", "ubuntu-24.04-arm"})
        spoa = self.script[self.script.index("    spoa)"):self.script.index("    controller)")]
        self.assertIn("scripts/spoa-release-image.sh verify", spoa)
        self.assertIn('sign_and_attest "$reference" spoa cyclonedx-json cyclonedx', spoa)
        preparation = workflow("prepare-spoa-release.yml")
        self.assertEqual(preparation["concurrency"]["group"], "spoa-release-publish")
        self.assertIs(preparation["concurrency"]["cancel-in-progress"], False)

    def test_image_builds_keep_attestations_without_dangling_oci_subjects(self):
        for name in ("run-release-publication.sh", "run-snapshot-publication.sh"):
            text = (ROOT / ".github/scripts" / name).read_text()
            with self.subTest(script=name):
                self.assertIn("--env BUILDX_NO_DEFAULT_OCI_ARTIFACT=true", text)
                self.assertNotIn("BUILDX_NO_DEFAULT_ATTESTATIONS", text)

    def test_chart_packages_signed_image_metadata_and_checks_parity_before_pushing(self):
        chart = self.script[self.script.index("    chart)"):]
        stages = ["cp -a charts/haptic/.", "scripts.ci.release_images", "helm package .publication/chart",
                  "scripts.ci.release_chart", "helm push"]
        positions = [chart.index(stage) for stage in stages]
        self.assertEqual(positions, sorted(positions))
        self.assertIn('--series $HAPROXY_VERSIONS --identity "$identity"', chart)
        self.assertIn('--chart ".publication/haptic-${version}.tgz" --embedded charts/haptic', chart)

    def test_release_signature_identity_requires_the_matching_tag(self):
        start = self.script.index('[[ "$GITHUB_WORKFLOW_REF" ==')
        guard = self.script[start:self.script.index("\n}\n", start) + 3]
        for ref, expected in (("refs/tags/v1.2.3", 0), ("refs/tags/v1.2.2", 1), ("refs/heads/main", 1)):
            with self.subTest(ref=ref):
                prefix = ('GITHUB_REPOSITORY=owner/repo version=1.2.3\n'
                          f'GITHUB_WORKFLOW_REF=owner/repo/.github/workflows/release.yml@{ref}\n')
                result = subprocess.run(["bash", "-c", prefix + guard], capture_output=True, check=False)
                self.assertEqual(result.returncode, expected)

    def test_every_controller_and_varnish_image_attests_its_own_repository(self):
        start = self.script.index("        for variant in $HAPROXY_VERSIONS varnish; do")
        loop = self.script[start:self.script.index("        done\n", start) + len("        done\n")]
        program = '''
            set -euo pipefail
            repository=ghcr.io/owner/haptic version=1.2.3 HAPROXY_VERSIONS="3.0 3.4"
            digest_of() { printf 'sha256:%064d\\n' "${#1}"; }
            sign_and_attest() { printf '%s %s %s %s\\n' "$@"; }
        ''' + loop.replace("# shellcheck source=../versions.env\n", "")
        result = subprocess.run(["bash", "-c", program], capture_output=True, text=True, check=True)
        digest = lambda target: "sha256:%064d" % len(target)
        self.assertEqual(result.stdout.splitlines(), [
            f"ghcr.io/owner/haptic@{digest('ghcr.io/owner/haptic:1.2.3-haproxy3.0')} 3.0 spdx-json spdxjson",
            f"ghcr.io/owner/haptic@{digest('ghcr.io/owner/haptic:1.2.3-haproxy3.4')} 3.4 spdx-json spdxjson",
            f"ghcr.io/owner/haptic/varnish@{digest('ghcr.io/owner/haptic/varnish:1.2.3')} varnish spdx-json spdxjson",
        ])

    def test_controller_rerun_reuses_only_a_complete_existing_release(self):
        functions = self.script[self.script.index("release_lookup() {"):self.script.index("sign_and_attest() {")]
        start = self.script.index('        case "$(release_lookup)" in')
        dispatch = self.script[start:self.script.index("        esac\n", start) + len("        esac\n")]
        stub = '''
            set -euo pipefail
            cd "$(mktemp -d)" && mkdir .publication
            GITHUB_TOKEN=t GITHUB_REPOSITORY=owner/repo version=1.2.3
            goreleaser() { echo built; }
            curl() {
                local out
                while (($#)); do [[ $1 == -o ]] && out=$2; shift; done
                case $out in
                    .publication/existing-release.json) printf '{"assets":[%s]}' "$ASSETS" > "$out"; printf %s "$STATUS" ;;
                    dist/checksums.txt) printf %s "$CHECKSUMS" > "$out" ;;
                    "dist/$TAMPERED") printf tampered > "$out" ;;
                    *) printf %s "${out#dist/}" > "$out" ;;
                esac
            }
        '''
        archives = ("haptic.tar.gz", "haptic.zip")
        checksums = "".join(f"{hashlib.sha256(name.encode()).hexdigest()}  {name}\n" for name in archives)
        asset = lambda name: '{"name":"%s","url":"u/%s"}' % (name, name)
        signed = [asset("checksums.txt"), asset("checksums.txt.sigstore.json"), asset("haptic.tar.gz")]
        for name, status, assets, tampered, code, stdout, stderr in (
            ("absent", "404", [], "", 0, "built\nsigning\n", ""),
            ("complete", "200", signed + [asset("haptic.zip")], "", 0, "signing\n", ""),
            ("incomplete", "200", signed, "", 1, "", "lacks haptic.zip"),
            ("tampered", "200", signed + [asset("haptic.zip")], "haptic.zip", 1, "haptic.zip: FAILED\n", "checksum did NOT match"),
            ("lookup error", "502", [], "", 1, "", "lookup for v1.2.3 failed"),
        ):
            with self.subTest(name):
                result = subprocess.run(
                    ["bash", "-c", stub + functions + dispatch + "echo signing\n"],
                    env={"PATH": "/usr/bin:/bin", "STATUS": status, "ASSETS": ",".join(assets),
                         "CHECKSUMS": checksums, "TAMPERED": tampered},
                    capture_output=True, text=True, check=False)
                self.assertEqual((result.returncode, result.stdout), (code, stdout), result.stderr)
                self.assertIn(stderr, result.stderr)

    def test_maintenance_branches_tag_releases_and_latest_waits_for_the_ordering_check(self):
        condition = workflow("release-tag.yml")["jobs"]["tag"]["if"]
        for clause in ("conclusion == 'success'", "event == 'push'", "head_branch == 'main'",
                       "startsWith(github.event.workflow_run.head_branch, 'maint/')"):
            self.assertIn(clause, condition)
        chart = self.script[self.script.index("    chart)"):]
        self.assertLess(chart.index("scripts/release-is-latest.sh"), chart.index("imagetools create --tag"))
        self.assertIsNone(re.search(r"alpha|beta|rc\b", chart))

    def test_upgrade_jobs_cover_each_baseline_once_on_both_kubernetes_versions(self):
        jobs = workflow("ci-speed-proof.yml")["jobs"].values()
        upgrades = {"current": [], "minimum": []}
        for job in jobs:
            suite = json.loads(job.get("env", {}).get("CI_SUITE", "{}"))
            if suite.get("suite") == "upgrade":
                variant = job["name"].rsplit("-", 1)[1]
                upgrades[variant].append(job["env"]["BASELINE_CHART_VERSION"])
        self.assertEqual(sorted(upgrades["current"]), sorted(upgrades["minimum"]))
        self.assertIn("0.2.0-alpha.3", upgrades["current"])
        self.assertEqual(len(upgrades["current"]), len(set(upgrades["current"])))
        for version in upgrades["current"]:
            self.assertRegex(version, r"^\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$")

    def test_baseline_guard_runs_whenever_the_upgrade_jobs_do(self):
        jobs = workflow("ci-speed-proof.yml")["jobs"]
        guard_key, guard = next((key, job) for key, job in jobs.items() if job["name"] == "chart-minimum")
        for job in jobs.values():
            if json.loads(job.get("env", {}).get("CI_SUITE", "{}")).get("suite") == "upgrade":
                self.assertEqual(job["if"], guard["if"])
        self.assertIn(guard_key, jobs["verified"]["needs"])
        suite = (ROOT / ".github/scripts/run-build-suite.sh").read_text()
        case = suite[suite.index("chart-minimum)"):]
        self.assertIn("check-chart-upgrade-baselines", case[:case.index(";;")])


if __name__ == "__main__":
    unittest.main()
