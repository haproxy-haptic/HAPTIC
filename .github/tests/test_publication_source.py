import copy
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import unittest
from unittest.mock import Mock, patch
import zipfile


SPEC = importlib.util.spec_from_file_location(
    "publication_source", Path(__file__).parents[1] / "scripts/verify-publication-source.py")
SOURCE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(SOURCE)


class PublicationSourceTests(unittest.TestCase):
    def test_frozen_source_identity_reads_git_objects_without_touching_working_files(self):
        sha = "a" * 40
        manifest = b'{"required_jobs":["frozen"]}'
        with patch.object(SOURCE.subprocess, "check_output", side_effect=[manifest, "tree\n"]), \
                patch.object(Path, "read_bytes", side_effect=AssertionError("Must not read working manifest")):
            self.assertEqual(SOURCE.source_identity(sha), (sha, "tree", manifest))

    def test_frozen_source_rejects_refs_paths_and_options_before_running_git(self):
        with patch.object(SOURCE.subprocess, "check_output") as git:
            for sha in ("main", "--help", "a" * 39, "a" * 40 + ":path"):
                with self.subTest(sha=sha), self.assertRaisesRegex(ValueError, "full commit SHA"):
                    SOURCE.source_identity(sha)
            git.assert_not_called()

    def setUp(self):
        self.run = {"id": 42, "head_sha": "sha", "run_attempt": 2, "repository": {"full_name": "owner/repo"},
                    "head_repository": {"full_name": "owner/repo"}, "path": ".github/workflows/ci.yml",
                    "event": "push", "head_branch": "main", "status": "completed", "conclusion": "success"}
        self.jobs = [{"id": number, "run_id": 42, "run_attempt": 2, "head_sha": "sha", "name": name,
                      "status": "completed", "conclusion": "success", "started_at": "2026-10-13T01:00:00Z",
                      "completed_at": "2026-10-13T01:01:00Z"}
                     for number, name in enumerate(("full / unit", "full / verified", "verified"), 10)]
        self.manifest = json.dumps({"required_jobs": ["unit"]}).encode()
        self.receipt = {"version": 2, "repository": "owner/repo", "run_id": 42, "run_attempt": 2,
                        "sha": "sha", "tree": "tree", "jobs": [{"name": "full / unit", "id": 10}],
                        "job_prefix": "full / ", "event": "push", "api_sha": "sha", "source_sha": "sha",
                        "source_repository": "owner/repo", "manifest_sha256": hashlib.sha256(self.manifest).hexdigest()}
        self.artifact = {"id": 20, "name": "ci-verification", "expired": False,
                         "created_at": "2026-10-13T01:00:30Z", "workflow_run": {
                             "id": 42, "head_sha": "sha", "head_branch": "main"}}
        self.pack()

    def pack(self, name="ci-verification.json"):
        data = io.BytesIO()
        with zipfile.ZipFile(data, "w") as archive:
            archive.writestr(name, json.dumps(self.receipt))
        self.content = data.getvalue()
        self.artifact["digest"] = "sha256:" + hashlib.sha256(self.content).hexdigest()

    def check(self):
        return SOURCE.check_evidence(self.run, self.jobs, self.artifact, self.content, repository="owner/repo",
                                     sha="sha", tree="tree", manifest=self.manifest)

    def test_accepts_complete_source_with_bound_artifact(self):
        self.assertEqual(self.check()["artifact_id"], 20)

    def test_rejects_noncanonical_and_unfinished_runs(self):
        original = copy.deepcopy(self.run)
        for key, value in (("repository", {"full_name": "other/repo"}),
                           ("head_repository", {"full_name": "fork/repo"}),
                           ("head_sha", "old"), ("path", ".github/workflows/ci-speed-proof.yml"),
                           ("event", "pull_request"), ("event", "schedule"), ("head_branch", "feature"),
                           ("status", "in_progress"), ("conclusion", "failure"), ("run_attempt", 3)):
            self.run = dict(original, **{key: value})
            with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                self.check()

    def test_requires_each_suite_and_both_aggregate_gates(self):
        for job in self.jobs:
            job["conclusion"] = "skipped"
            with self.subTest(job=job["name"]), self.assertRaises(ValueError):
                self.check()
            job["conclusion"] = "success"

    def test_rejects_stale_or_mismatched_receipt_fields(self):
        original = copy.deepcopy(self.receipt)
        for key, value in (("sha", "old"), ("tree", "other"), ("run_attempt", 1), ("run_id", 40),
                           ("manifest_sha256", "other"), ("jobs", [{"name": "full / unit", "id": 99}]),
                           ("source_repository", "fork/repo"), ("event", "pull_request")):
            self.receipt = dict(original, **{key: value})
            self.pack()
            with self.subTest(key=key), self.assertRaises(ValueError):
                self.check()

    def test_rejects_artifacts_outside_the_successful_attempt(self):
        for created in ("2026-10-13T00:59:59Z", "2026-10-13T01:01:01Z"):
            self.artifact["created_at"] = created
            with self.subTest(created=created), self.assertRaises(ValueError):
                self.check()

    def test_rejects_changed_or_unexpected_archive_contents(self):
        self.content += b"changed"
        with self.assertRaisesRegex(ValueError, "digest"):
            self.check()
        self.pack("unexpected.json")
        with self.assertRaisesRegex(ValueError, "unexpected contents"):
            self.check()

    def test_rejects_other_artifact_origin(self):
        for key, value in (("id", 41), ("head_sha", "old"), ("head_branch", "feature")):
            original = self.artifact["workflow_run"][key]
            self.artifact["workflow_run"][key] = value
            with self.subTest(key=key), self.assertRaises(ValueError):
                self.check()
            self.artifact["workflow_run"][key] = original

    def test_accepts_maintenance_branch_and_explicit_canonical_dispatch(self):
        self.run.update(head_branch="maint/0.3", event="workflow_dispatch")
        self.artifact["workflow_run"]["head_branch"] = "maint/0.3"
        self.receipt["event"] = "workflow_dispatch"
        self.pack()
        self.check()

    def test_documentation_receipt_requires_explicit_scope_and_recomputed_changes(self):
        self.manifest = json.dumps({"required_jobs": ["unit", "runtime"], "prose": {
            "paths": ["README.md"], "patterns": [], "required_jobs": ["unit"]}}).encode()
        paths = ["README.md"]
        self.receipt.update(profile="prose", comparison_base="a" * 40,
                            changed_paths_sha256=SOURCE.VERIFY.paths_digest(paths),
                            manifest_sha256=hashlib.sha256(self.manifest).hexdigest())
        self.pack()
        with self.assertRaisesRegex(ValueError, "requires full"):
            self.check()
        comparison = Mock(return_value=paths)
        arguments = dict(repository="owner/repo", sha="sha", tree="tree", manifest=self.manifest,
                         allow_prose=True, comparison=comparison)
        result = SOURCE.check_evidence(self.run, self.jobs, self.artifact, self.content, **arguments)
        self.assertEqual(result["profile"], "prose")
        comparison.assert_called_with("a" * 40, "sha")
        for changed in ([], ["README.md", "main.go"], ["CHANGELOG.md"]):
            comparison.return_value = changed
            with self.subTest(changed=changed), self.assertRaises(ValueError):
                SOURCE.check_evidence(self.run, self.jobs, self.artifact, self.content, **arguments)
        comparison.return_value = paths
        self.jobs[0]["conclusion"] = "skipped"
        with self.assertRaises(ValueError):
            SOURCE.check_evidence(self.run, self.jobs, self.artifact, self.content, **arguments)

    def test_publication_comparison_rejects_unrelated_or_malformed_base(self):
        with patch.object(SOURCE.VERIFY, "git_is_ancestor", return_value=False), \
                patch.object(SOURCE.VERIFY, "git_changed_paths") as paths:
            for base in ("--all", "b" * 40, "a" * 40):
                with self.subTest(base=base), self.assertRaises(ValueError):
                    SOURCE.publication_changes(base, "b" * 40)
            paths.assert_not_called()


if __name__ == "__main__":
    unittest.main()
