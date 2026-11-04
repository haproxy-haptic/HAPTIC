import copy
import contextlib
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

import test_publication_source as fixtures


SPEC = importlib.util.spec_from_file_location(
    "website_tooling", Path(__file__).parents[1] / "scripts/verify-website-tooling.py")
TOOLING = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(TOOLING)


class WebsiteToolingTests(unittest.TestCase):
    def setUp(self):
        self.source = fixtures.PublicationSourceTests()
        self.source.setUp()
        self.api = Mock()
        self.api.get.return_value = self.source.run
        self.api.artifact.return_value = self.source.content
        self.api.all.side_effect = self.inventory

    def inventory(self, path, field, **params):
        if field == "workflow_runs":
            return [self.source.run]
        if field == "jobs":
            return self.source.jobs
        if field == "artifacts":
            return [self.source.artifact]
        raise AssertionError(path)

    def verify(self, run=None):
        return TOOLING.verify(self.api, "owner/repo", "sha", "tree", self.source.manifest, run)

    def test_selects_and_fully_checks_exact_main_push_ci(self):
        receipt = self.verify()
        self.assertEqual(receipt["run_id"], 42)
        self.assertEqual(receipt["run_attempt"], 2)
        self.api.all.assert_any_call("repos/owner/repo/actions/workflows/ci.yml/runs", "workflow_runs",
                                     branch="main", event="push", head_sha="sha")

    def test_rejects_manual_pull_request_foreign_and_unfinished_verification(self):
        original = copy.deepcopy(self.source.run)
        for key, value in (("event", "workflow_dispatch"), ("event", "pull_request"),
                           ("head_sha", "other"), ("head_branch", "maint/1.0"),
                           ("path", ".github/workflows/other.yml"), ("status", "in_progress"),
                           ("conclusion", "failure"), ("run_attempt", 3)):
            self.source.run = {**original, key: value}
            self.api.get.return_value = self.source.run
            with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                self.verify(42)

    def test_no_canonical_run_cannot_fall_back_to_an_older_commit(self):
        self.source.run = {**self.source.run, "head_sha": "older"}
        with self.assertRaisesRegex(ValueError, "exact workflow commit"):
            self.verify()
        self.api.artifact.assert_not_called()

    def test_missing_suite_and_changed_attempt_are_rejected(self):
        self.source.jobs.pop(0)
        with self.assertRaises(ValueError):
            self.verify(42)
        self.source.setUp()
        self.api.get.side_effect = [self.source.run, {**self.source.run, "run_attempt": 3}]
        with self.assertRaisesRegex(ValueError, "changed while"):
            self.verify(42)

    def test_retained_tooling_receipt_cannot_change_between_phases(self):
        receipt = self.verify(42)
        with tempfile.TemporaryDirectory() as temporary, contextlib.chdir(temporary):
            manifest = Path(".github/ci-verification.json")
            manifest.parent.mkdir()
            manifest.write_bytes(self.source.manifest)
            Path("expected.json").write_text(json.dumps({**receipt, "run_attempt": 1}))
            with patch.object(TOOLING, "verify", return_value=receipt), \
                    patch.object(TOOLING.subprocess, "check_output", side_effect=["sha", "tree"]), \
                    patch.dict(os.environ, {"GITHUB_WORKFLOW_SHA": "sha", "GITHUB_REPOSITORY": "owner/repo", "GH_TOKEN": "test"}), \
                    patch("sys.argv", ["tooling", "--run", "42", "--expected", "expected.json"]):
                with self.assertRaisesRegex(ValueError, "changed after"):
                    TOOLING.main()
            self.assertFalse(Path(".publication").exists())
