import importlib.util
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("promote_changelog", ROOT / "scripts/promote-changelog.py")
CHANGELOG = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CHANGELOG)


class ReleaseTests(unittest.TestCase):
    def test_latest_moves_only_to_the_highest_stable_release(self):
        tags = "v0.1.0-alpha.9\nv0.2.0\nv0.2.1\nv0.3.0-alpha.1\nv0.10.0-rc.1\n"
        ls_remote = "".join(f"{i:040x}\trefs/tags/{t}\n" for i, t in enumerate(tags.split()))
        cases = [
            ("highest stable", "v0.2.1", tags, 0),
            ("older stable", "v0.2.0", tags, 1),
            ("prerelease", "v0.3.0-alpha.1", tags, 1),
            ("prerelease above every stable", "v0.10.0-rc.1", tags, 1),
            ("maintenance release below a newer line", "v0.2.2", tags + "v0.2.2\nv0.3.0\n", 1),
            ("maintenance release of the newest line", "v0.2.2", tags + "v0.2.2\n", 0),
            ("numeric not lexical order", "v0.10.0", tags + "v0.9.0\nv0.10.0\n", 0),
            ("ls-remote input", "v0.2.1", ls_remote, 0),
            ("tag missing from input", "v0.2.2", tags, 2),
            ("empty input", "v0.2.1", "", 2),
        ]
        for name, tag, stdin, code in cases:
            with self.subTest(name=name):
                result = subprocess.run(["bash", str(ROOT / "scripts/release-is-latest.sh"), tag],
                                        input=stdin, capture_output=True, text=True)
                self.assertEqual(result.returncode, code, result.stderr)

    def test_release_commits_updated_pins_and_preserves_historical_upgrades(self):
        upgrade_notes = (ROOT / "docs/site/docs/upgrade-notes.md").read_text()
        upgrade_04, historical = upgrade_notes.split("## Upgrading to 0.3\n", 1)
        historical = (
            "## Upgrading to 0.3\n\n"
            "~~~markdown\n## Current installation\nhelm upgrade haptic --version 0.3.0\n~~~\n"
            + historical
        )
        with tempfile.TemporaryDirectory(prefix="haptic-release-test-") as temp:
            repo = Path(temp)
            files = {
                "go.mod": "module example.test/release\n",
                "VERSION": "0.1.0\n",
                "versions.env": 'DEFAULT_HAPROXY="3.4"\n',
                "CHANGELOG.md": "# Changelog\n\n## [Unreleased]\n\n### Fixed\n\n- A fix.\n",
                "charts/haptic/Chart.yaml": (
                    'version: 0.1.0\nappVersion: "0.1.0"\n'
                    'annotations:\n  artifacthub.io/images: |\n'
                    '    - name: varnish\n'
                    '      image: ghcr.io/haproxy-haptic/haptic/varnish:0.1.0\n'
                ),
                "charts/haptic/README.md": (
                    "https://haproxy-haptic.org/docs/dev/\n"
                    "https://github.com/haproxy-haptic/HAPTIC/blob/main/README.md\n"
                ),
                "charts/haptic/values.yaml": (
                    "# https://github.com/haproxy-haptic/HAPTIC/blob/main/README.md\n"
                    "# https://github.com/haproxy-haptic/HAPTIC/blob/v0.1.0/README.md\n"
                ),
                "charts/haptic/templates/NOTES.txt": "https://haproxy-haptic.org/docs/dev/\n",
                "README.md": (
                    "helm install haptic --version 0.1.0\n"
                    "https://haproxy-haptic.org/docs/0.1.0/getting-started/\n"
                    "https://haproxy-haptic.org/docs/0.1.0/template-libraries/\n"
                    "https://haproxy-haptic.org/docs/dev/development/agent-skill/\n"
                ),
                "docs/site/docs/install.md": "helm upgrade haptic --version 0.1.0\n",
                "docs/site/docs/upgrade-notes.md": (
                    "helm pull chart --version 0.1.0\n"
                    "haptic preflight --chart chart --expect-chart-version 0.1.0\n"
                    "Upgrade from 0.1.0.\n"
                    + upgrade_04 + historical +
                    "\n## Current installation\n"
                    "````markdown\n```\n## Upgrading to 9.9\n```\n````\n"
                    "helm install haptic --version 0.1.0\n"
                    "docker run registry.test/haptic:0.1.0-haproxy3.0\n"
                ),
                "docs/landing/overrides/home.html": '<span id="helm-version" class="t-num">0.1.0</span>\n',
            }
            for name, content in files.items():
                target = repo / name
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_text(content)
            (repo / "scripts").mkdir()
            shutil.copyfile(ROOT / "scripts/release.sh", repo / "scripts/release.sh")
            shutil.copyfile(ROOT / "scripts/update-release-docs.py", repo / "scripts/update-release-docs.py")
            shutil.copyfile(ROOT / "scripts/promote-changelog.py", repo / "scripts/promote-changelog.py")
            env = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
            env.update({
                "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull,
                "GIT_AUTHOR_NAME": "Release test", "GIT_AUTHOR_EMAIL": "release@example.test",
                "GIT_COMMITTER_NAME": "Release test", "GIT_COMMITTER_EMAIL": "release@example.test",
            })

            def run(*args):
                return subprocess.run(args, cwd=repo, env=env, check=True, capture_output=True, text=True).stdout

            run("git", "init", "--initial-branch=main")
            run("git", "add", ".")
            run("git", "commit", "-m", "Initial fixture")
            run("bash", "scripts/release.sh", "0.2.0-alpha.2")
            self.assertEqual(run("git", "status", "--porcelain"), "")
            self.assertEqual(run("git", "show", "HEAD:VERSION").strip(), "0.2.0-alpha.2")
            for path in ("charts/haptic/README.md", "charts/haptic/values.yaml", "charts/haptic/templates/NOTES.txt"):
                with self.subTest(path=path):
                    committed = run("git", "show", "HEAD:" + path)
                    self.assertIn("0.2.0-alpha.2", committed)
                    self.assertNotIn("/docs/dev/", committed)
                    self.assertNotIn("/blob/main/", committed)
            for version in ("0.2.0-alpha.2", "0.2.0", "0.3.0", "0.4.0", "0.4.1", "0.5.0"):
                with self.subTest(version=version):
                    run("bash", "scripts/release.sh", version)
                    self.assertEqual(run("git", "status", "--porcelain"), "")
                    readme = run("git", "show", "HEAD:README.md")
                    self.assertIn(f"https://haproxy-haptic.org/docs/{version}/getting-started/", readme)
                    self.assertIn(f"https://haproxy-haptic.org/docs/{version}/template-libraries/", readme)
                    self.assertIn("https://haproxy-haptic.org/docs/dev/development/agent-skill/", readme)
                    for path in ("charts/haptic/README.md", "charts/haptic/values.yaml"):
                        committed = run("git", "show", "HEAD:" + path)
                        self.assertIn(
                            "https://github.com/haproxy-haptic/HAPTIC/"
                            f"blob/v{version}/README.md", committed,
                        )
                        self.assertNotIn("/blob/main/", committed)
                    self.assertIn(f"haptic/varnish:{version}\n", run("git", "show", "HEAD:charts/haptic/Chart.yaml"))
                    changelog = run("git", "show", "HEAD:CHANGELOG.md")
                    self.assertEqual(changelog.count("- A fix.\n"), 1)
                    if version != "0.2.0-alpha.2":
                        self.assertNotIn("## [0.2.0-alpha.2]", changelog)
                    committed = run("git", "show", "HEAD:docs/site/docs/upgrade-notes.md")
                    self.assertIn(f"--version {version}\n", committed)
                    self.assertIn(f"--expect-chart-version {version}\n", committed)
                    self.assertIn("Upgrade from 0.1.0.\n", committed)
                    self.assertIn(historical, committed)
                    expected_04 = upgrade_04
                    if version in ("0.4.1", "0.5.0"):
                        expected_04 = expected_04.replace("--version 0.4.0", "--version 0.4.1")
                    self.assertIn(expected_04, committed)
                    self.assertIn(f"helm install haptic --version {version}\n", committed)
                    self.assertIn(f"registry.test/haptic:{version}-haproxy3.4\n", committed)


HEAD = "# Changelog\n\nIntro text.\n\n"
OLD = "## [0.5.0] - 2026-10-08\n\n### Fixed\n\n- Old fix.\n"
RC1 = """## [0.6.0-rc.1] - 2026-10-17

First candidate intro.

**Before upgrading:** read the notes.

### Added

- Metrics:
    - `a_total`.
- Feature one.

### Changed

- **BREAKING:** A change.

### Helm chart

#### Changed

- Chart change.

#### Fixed

- Chart fix one.

"""
RC2 = """## [0.6.0-rc.2] - 2026-10-20

Second candidate intro.

### Added

- Metrics:
    - `b_total`.

### Fixed

- Shared fix.
- RC2 fix.

### Helm chart

#### Fixed

- Chart fix two.

"""


def section(name, body=""):
    return f"## [{name}] - 2026-10-01\n\n{body}"


class ChangelogPromotionTests(unittest.TestCase):
    def promote(self, text, version):
        return CHANGELOG.promote(text, version, "2026-10-30")[0]

    def test_final_release_without_unreleased_entries_keeps_its_prerelease_verbatim(self):
        promoted = self.promote(HEAD + "## [Unreleased]\n\n" + RC1 + OLD, "0.6.0")
        self.assertEqual(promoted, HEAD + "## [Unreleased]\n\n"
                         + RC1.replace("[0.6.0-rc.1] - 2026-10-17", "[0.6.0] - 2026-10-30") + OLD)

    def test_final_release_folds_one_prerelease_and_unreleased_entries(self):
        unreleased = """## [Unreleased]

Final intro.

### Security

- A security fix.

### Added

- Feature two.
- Feature one.

### Helm chart

#### Added

- Chart feature.

"""
        self.assertEqual(self.promote(HEAD + unreleased + RC1 + OLD, "0.6.0"), HEAD + """## [Unreleased]

## [0.6.0] - 2026-10-30

Final intro.

### Added

- Metrics:
    - `a_total`.
- Feature one.
- Feature two.

### Changed

- **BREAKING:** A change.

### Security

- A security fix.

### Helm chart

#### Added

- Chart feature.

#### Changed

- Chart change.

#### Fixed

- Chart fix one.

""" + OLD)

    def test_final_release_folds_two_prereleases_and_unreleased_entries(self):
        unreleased = """## [Unreleased]

### Fixed

- Unreleased fix.
- Shared fix.

### Helm chart

#### Security

- Chart security fix.

"""
        self.assertEqual(self.promote(HEAD + unreleased + RC2 + RC1 + OLD, "0.6.0"), HEAD + """## [Unreleased]

## [0.6.0] - 2026-10-30

Second candidate intro.

### Added

- Metrics:
    - `a_total`.
    - `b_total`.
- Feature one.

### Changed

- **BREAKING:** A change.

### Fixed

- Shared fix.
- RC2 fix.
- Unreleased fix.

### Helm chart

#### Changed

- Chart change.

#### Fixed

- Chart fix one.
- Chart fix two.

#### Security

- Chart security fix.

""" + OLD)

    def test_promotion_without_prereleases_to_fold_only_inserts_the_heading(self):
        unreleased = "## [Unreleased]\n\n### Fixed\n\n- New fix.\n\n"
        cases = [
            ("next pre-release", "0.6.0-rc.2", HEAD + unreleased + RC1 + OLD),
            ("final release", "0.5.1", HEAD + unreleased + OLD),
        ]
        for name, version, text in cases:
            with self.subTest(name=name):
                self.assertEqual(self.promote(text, version), text.replace(
                    "## [Unreleased]\n", f"## [Unreleased]\n\n## [{version}] - 2026-10-30\n", 1))

    def test_existing_release_section_is_left_alone(self):
        for version, text in (("0.6.0-rc.1", HEAD + "## [Unreleased]\n\n" + RC1 + OLD),
                              ("0.5.0", HEAD + "## [Unreleased]\n\n" + OLD)):
            with self.subTest(version=version):
                self.assertEqual(self.promote(text, version), text)

    def test_refuses_prerelease_sections_it_cannot_place(self):
        cases = [
            ("another version's pre-release in between", "[0.5.1-rc.1] is unreleased",
             HEAD + "## [Unreleased]\n\n" + RC1 + section("0.5.1-rc.1") + OLD),
            ("only another version's pre-release", "[0.5.1-rc.1] is unreleased",
             HEAD + "## [Unreleased]\n\n" + section("0.5.1-rc.1") + OLD),
            ("pre-release below a release", "[0.6.0-rc.1] is not adjacent",
             HEAD + "## [Unreleased]\n\n" + OLD + "\n" + RC1),
            ("final beside its pre-release", "both [0.6.0] and [0.6.0-rc.1]",
             HEAD + "## [Unreleased]\n\n" + section("0.6.0") + RC1 + OLD),
            ("no unreleased section", "no [Unreleased] section", HEAD + RC1 + OLD),
        ]
        for name, message, text in cases:
            with self.subTest(name=name), self.assertRaisesRegex(CHANGELOG.ChangelogError, re.escape(message)):
                self.promote(text, "0.6.0")

    def test_refusal_exits_nonzero_and_leaves_the_file(self):
        with tempfile.TemporaryDirectory(prefix="haptic-changelog-test-") as temp:
            changelog = Path(temp) / "CHANGELOG.md"
            text = HEAD + "## [Unreleased]\n\n" + RC1 + section("0.5.1-rc.1") + OLD
            changelog.write_text(text)
            result = subprocess.run(["python3", str(ROOT / "scripts/promote-changelog.py"), "0.6.0", "2026-10-30",
                                     str(changelog)], capture_output=True, text=True)
            self.assertEqual(result.returncode, 1)
            self.assertIn("Error: CHANGELOG.md: [0.5.1-rc.1]", result.stderr)
            self.assertEqual(changelog.read_text(), text)


if __name__ == "__main__":
    unittest.main()
