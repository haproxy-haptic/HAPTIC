import importlib.util
import io
import json
from pathlib import Path
import unittest
from unittest import mock


ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("check_image_pins", ROOT / "scripts/check-image-pins.py")
CHECKER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CHECKER)


class CheckImagePinsTests(unittest.TestCase):
    def setUp(self):
        managers = json.loads((ROOT / "renovate.json").read_text())["customManagers"]
        self.managers = [m for m in managers if m.get("packageNameTemplate") == "haproxytech/haproxy-debian"]
        self.files = {
            "charts/haptic/values.yaml": (
                '# renovate: datasource=docker depName=haproxytech/haproxy-debian\n'
                '  "3.2": "3.2.23"\n'
                '# renovate: datasource=docker depName=haproxytech/haproxy-debian\n'
                '  "3.4": "3.4.4"\n'
            ),
            "scripts/dev-env-assets/haproxy-production.yaml": "image: haproxytech/haproxy-debian:3.2.23\n",
            "charts/haptic/charts/helm-helpers/files/default-values.json": '{"haproxyPatchVersions":{"3.2":"3.2.23","3.4":"3.4.4"}}\n',
            "ci/build-pins.yml": (
                '  HAPROXY_IMAGE_32: "haproxytech/haproxy-debian:3.2.23@sha256:' + 'a' * 64 + '"\n'
                '  HAPROXY_IMAGE_34: "haproxytech/haproxy-debian:3.4.4@sha256:' + 'b' * 64 + '"\n'
            ),
        }

    def pins(self):
        return CHECKER.collect_pins(self.managers, self.files, self.files.__getitem__)

    def test_compares_each_series_separately(self):
        pins = self.pins()
        self.assertEqual(set(pins), {"haproxy-debian 3.2.x", "haproxy-debian 3.4.x"})
        self.assertEqual(len(pins["haproxy-debian 3.2.x"]["3.2.23"]), 4)
        self.assertFalse(CHECKER.report_pins(pins, io.StringIO()))

    def test_stale_ci_patch_fails(self):
        self.files["ci/build-pins.yml"] = self.files["ci/build-pins.yml"].replace("3.4.4", "3.4.3")
        output = io.StringIO()
        self.assertTrue(CHECKER.report_pins(self.pins(), output))
        self.assertIn("FAIL: haproxy-debian 3.4.x", output.getvalue())
        self.assertIn("ci/build-pins.yml", output.getvalue())
        self.assertIn("charts/haptic/values.yaml", output.getvalue())

    def test_renovate_captures_digest_with_the_tag(self):
        manager = self.managers[0]
        pattern = CHECKER.re.compile(CHECKER.to_python(manager["matchStrings"][0]))
        matches = list(pattern.finditer(self.files["ci/build-pins.yml"]))
        self.assertEqual(len(matches), 2)
        self.assertEqual(matches[0].group("currentDigest"), "sha256:" + "a" * 64)
        self.assertEqual(matches[1].group("currentValue"), "3.4.4")

    def test_missing_capture_fails_instead_of_skipping_the_dependency(self):
        with self.assertRaisesRegex(ValueError, "no capture for missing"):
            CHECKER.dependency_name("image {{{missing}}}", {})

    def test_static_dependency_name(self):
        self.assertEqual(CHECKER.dependency_name("varnish", {}), "varnish")

    def test_file_patterns_preserve_anchored_regex_matching(self):
        matcher = CHECKER.file_matchers({"managerFilePatterns": [r"/^scripts/dev-env-assets/haproxy-.*\.yaml$/"]})[0]
        self.assertIsNotNone(matcher.search("scripts/dev-env-assets/haproxy-production.yaml"))
        self.assertIsNone(matcher.search("old/scripts/dev-env-assets/haproxy-production.yaml"))
        self.assertIsNone(matcher.search("scripts/dev-env-assets/haproxy-production.yaml.bak"))

    def test_file_patterns_support_explicit_case_insensitivity(self):
        matcher = CHECKER.file_matchers({"managerFilePatterns": [r"/^Dockerfile$/i"]})[0]
        self.assertIsNotNone(matcher.search("DOCKERFILE"))
        sensitive = CHECKER.file_matchers({"managerFilePatterns": [r"/^Dockerfile$/"]})[0]
        self.assertIsNone(sensitive.search("DOCKERFILE"))

    def test_unsupported_or_missing_file_patterns_fail_closed(self):
        cases = [{}, {"managerFilePatterns": []}, {"fileMatch": ["^Dockerfile$"]}]
        cases.extend({"managerFilePatterns": [pattern]} for pattern in (
            "*.yaml", "!/^Dockerfile$/", "^Dockerfile$", "/^Dockerfile$/g", "/[/", "//"))
        for manager in cases:
            with self.subTest(manager=manager), self.assertRaises(ValueError):
                CHECKER.file_matchers(manager)

    def test_all_repo_managers_use_migrated_regex_patterns(self):
        managers = json.loads((ROOT / "renovate.json").read_text())["customManagers"]
        for manager in managers:
            with self.subTest(manager=manager["description"]):
                self.assertNotIn("fileMatch", manager)
                self.assertTrue(CHECKER.file_matchers(manager))

    def test_manager_matching_nothing_fails(self):
        self.files.pop("charts/haptic/values.yaml")
        with self.assertRaisesRegex(ValueError, "matched no version pins"):
            self.pins()

    def test_empty_pin_set_fails(self):
        self.assertTrue(CHECKER.report_pins({}, io.StringIO()))

    def test_git_failure_is_reported_without_a_traceback(self):
        output = io.StringIO()
        error = CHECKER.subprocess.CalledProcessError(128, ["git", "ls-files"])
        with mock.patch.object(CHECKER.subprocess, "run", side_effect=error), mock.patch.object(CHECKER.sys, "stderr", output):
            self.assertEqual(CHECKER.main(), 1)
        self.assertIn("Image pin check failed:", output.getvalue())
        self.assertNotIn("Traceback", output.getvalue())

    def test_ci_requires_chart_patch_and_digest_under_the_right_variable(self):
        digest = "@sha256:" + "c" * 64
        patches = {"3.2": "3.2.23", "3.4": "3.4.4"}
        valid = {"HAPROXY_IMAGE_32": "image:3.2.23" + digest, "HAPROXY_IMAGE_34": "image:3.4.4" + digest}
        CHECKER.validate_haproxy_ci_pins(patches, valid)
        cases = {
            "missing": {"HAPROXY_IMAGE_32": valid["HAPROXY_IMAGE_32"]},
            "floating": {**valid, "HAPROXY_IMAGE_34": "image:3.4"},
            "no digest": {**valid, "HAPROXY_IMAGE_34": "image:3.4.4"},
            "stale": {**valid, "HAPROXY_IMAGE_34": "image:3.4.3" + digest},
            "wrong key": {"HAPROXY_IMAGE_34": valid["HAPROXY_IMAGE_32"], "HAPROXY_IMAGE_32": valid["HAPROXY_IMAGE_34"]},
        }
        for name, variables in cases.items():
            with self.subTest(name=name), self.assertRaises(ValueError):
                CHECKER.validate_haproxy_ci_pins(patches, variables)

    def test_supported_series_lists_must_agree(self):
        chart = {"haproxyPatchVersions": {"3.2": "3.2.23", "3.4": "3.4.4"},
                 "haproxyEnterprisePatchVersions": {"3.2": "3.2r1", "3.4": ""}}
        schema = {"properties": {"haproxyVersion": {"enum": ["3.2", "3.4"]}}}
        versions_env = 'HAPROXY_VERSIONS="3.2 3.4"\n'
        CHECKER.validate_haproxy_series(chart, schema, versions_env)
        cases = {
            "versions.env HAPROXY_VERSIONS": (chart, schema, 'HAPROXY_VERSIONS="3.1 3.2 3.4"\n'),
            "haproxyVersion enum": (chart, {"properties": {"haproxyVersion": {"type": "string"}}}, versions_env),
            "haproxyEnterprisePatchVersions": (
                {**chart, "haproxyEnterprisePatchVersions": {"3.1": "3.1r1", "3.2": "3.2r1", "3.4": ""}},
                schema, versions_env),
        }
        for name, args in cases.items():
            with self.subTest(name=name), self.assertRaisesRegex(ValueError, name):
                CHECKER.validate_haproxy_series(*args)


if __name__ == "__main__":
    unittest.main()
