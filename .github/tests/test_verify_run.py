import copy
import importlib.util
import json
from pathlib import Path
import unittest
from unittest.mock import Mock


SPEC = importlib.util.spec_from_file_location("verify_run", Path(__file__).parents[1] / "scripts/verify-run.py")
VERIFY = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(VERIFY)


class VerifyRunTests(unittest.TestCase):
    def setUp(self):
        self.run = {"id": 42, "head_sha": "abc", "run_attempt": 2, "repository": {"full_name": "owner/repo"}}
        self.job = {
            "id": 123, "run_id": 42, "run_attempt": 2, "head_sha": "abc", "name": "unit", "status": "completed",
            "conclusion": "success", "started_at": "2026-10-12T20:00:00Z", "completed_at": "2026-10-12T20:01:00Z",
        }

    def check(self, jobs, run=None, required=None):
        return VERIFY.verify(self.run if run is None else run, jobs, ["unit"] if required is None else required,
                             repository="owner/repo", sha="abc", attempt=2)

    def test_all_required_jobs_must_have_run_and_passed(self):
        self.assertEqual(self.check([self.job]), [{"name": "unit", "id": 123}])
        for conclusion in ("failure", "skipped", "cancelled", "neutral", "timed_out", None):
            with self.subTest(conclusion=conclusion), self.assertRaises(ValueError):
                self.check([dict(self.job, conclusion=conclusion)])
        for jobs in ([], [dict(self.job, name="other")], [dict(self.job, status="in_progress")]):
            with self.subTest(jobs=jobs), self.assertRaises(ValueError):
                self.check(jobs)

    def test_rereads_jobs_the_api_still_reports_unfinished(self):
        pending = dict(self.job, status="in_progress", conclusion=None, completed_at=None)
        reads = [[pending, dict(self.job, name="other", status="in_progress")], [self.job]]
        sleeps = []
        jobs = VERIFY.settled_jobs(lambda: reads.pop(0), {"unit"}, sleep=sleeps.append)
        self.assertEqual(self.check(jobs), [{"name": "unit", "id": 123}])
        self.assertEqual(len(sleeps), 1)
        sleeps.clear()
        jobs = VERIFY.settled_jobs(lambda: [pending], {"unit"}, delays=(1, 2), sleep=sleeps.append)
        self.assertEqual(sleeps, [1, 2])
        with self.assertRaises(ValueError):
            self.check(jobs)
        unrelated = VERIFY.settled_jobs(lambda: [self.job, dict(pending, name="other")], {"unit"},
                                        sleep=sleeps.append)
        self.assertEqual(len(unrelated), 2)

    def test_rereads_while_a_rerun_attempt_omits_carried_over_jobs(self):
        reads = [[dict(self.job, name="other")], [dict(self.job, name="other"), self.job]]
        sleeps = []
        jobs = VERIFY.settled_jobs(lambda: reads.pop(0), {"unit", "other"}, sleep=sleeps.append)
        self.assertEqual(self.check(jobs), [{"name": "unit", "id": 123}])
        self.assertEqual(len(sleeps), 1)

    def test_provenance_cannot_cross_revision_repository_run_or_attempt(self):
        for key, value in (("head_sha", "stale"), ("run_attempt", 1), ("repository", {"full_name": "another/repo"})):
            run = copy.deepcopy(self.run)
            run[key] = value
            with self.subTest(key=key), self.assertRaises(ValueError):
                self.check([self.job], run=run)
        for key, value in (("head_sha", "stale"), ("run_id", 41), ("run_attempt", 1)):
            with self.subTest(key=key), self.assertRaises(ValueError):
                self.check([dict(self.job, **{key: value})])

    def test_rejects_empty_duplicate_or_incomplete_evidence(self):
        for required in ([], ["unit", "unit"]):
            with self.subTest(required=required), self.assertRaises(ValueError):
                self.check([self.job], required=required)
        with self.assertRaises(ValueError):
            self.check([self.job, self.job])
        for key in ("started_at", "completed_at"):
            with self.subTest(key=key), self.assertRaises(ValueError):
                self.check([dict(self.job, **{key: None})])

    def test_reusable_workflow_requires_its_own_jobs(self):
        nested = dict(self.job, name="full / unit", id=124)
        arguments = {"repository": "owner/repo", "sha": "abc", "attempt": 2, "prefix": "full / "}
        self.assertEqual(VERIFY.verify(self.run, [self.job, nested], ["unit"], **arguments),
                         [{"name": "full / unit", "id": 124}])
        with self.assertRaises(ValueError):
            VERIFY.verify(self.run, [self.job], ["unit"], **arguments)
        with self.assertRaises(ValueError):
            VERIFY.verify(self.run, [self.job, dict(nested, conclusion="failure")], ["unit"], **arguments)
        arguments["prefix"] = "full"
        with self.assertRaises(ValueError):
            VERIFY.verify(self.run, [nested], ["unit"], **arguments)

    def test_canary_may_fail_but_cannot_be_absent_skipped_or_from_another_attempt(self):
        canary = dict(self.job, name="full / canary", id=124)
        unit = dict(self.job, name="full / unit")
        arguments = {"repository": "owner/repo", "sha": "abc", "attempt": 2,
                     "prefix": "full / ", "allowed_failures": ["canary"]}
        for conclusion in ("success", "failure", "timed_out"):
            canary["conclusion"] = conclusion
            receipt = VERIFY.verify(self.run, [unit, canary], ["unit", "canary"], **arguments)
            self.assertIn({"name": "full / canary", "id": 124, "conclusion": conclusion}, receipt)
        for key, value in (("conclusion", "skipped"), ("conclusion", "cancelled"),
                           ("status", "queued"), ("run_attempt", 1), ("head_sha", "other")):
            with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                VERIFY.verify(self.run, [unit, dict(canary, **{key: value})], ["unit", "canary"], **arguments)
        for jobs in ([unit], [dict(unit, conclusion="failure"), canary]):
            with self.assertRaises(ValueError):
                VERIFY.verify(self.run, jobs, ["unit", "canary"], **arguments)
        for allowed in (["missing"], ["canary", "canary"]):
            with self.assertRaises(ValueError):
                VERIFY.verify(self.run, [unit, canary], ["unit", "canary"],
                              **{**arguments, "allowed_failures": allowed})


class RevisionIdentityTests(unittest.TestCase):
    def setUp(self):
        self.event = {"repository": {"full_name": "owner/repo"}, "pull_request": {
            "number": 43, "merge_commit_sha": None,
            "head": {"sha": "head", "repo": {"full_name": "fork/repo"}},
            "base": {"sha": "base", "ref": "main", "repo": {"full_name": "owner/repo"}},
        }}

    def identity(self, event_name="pull_request", event=None, parents=None, tested_sha="merge"):
        return VERIFY.revision_identity(event_name, self.event if event is None else event,
                                        repository="owner/repo", tested_sha=tested_sha,
                                        parents=["base", "head"] if parents is None else parents)

    def test_pr_binds_api_head_and_tested_merge_without_relying_on_nullable_payload_sha(self):
        self.assertEqual(self.identity(), {"api_sha": "head", "source_sha": "head", "base_sha": "base",
                                           "source_repository": "fork/repo", "base_branch": "main", "pull_request": 43})

    def test_pr_accepts_a_base_branch_that_moved_forward_after_the_event(self):
        moved = VERIFY.revision_identity("pull_request", self.event, repository="owner/repo", tested_sha="merge",
                                         parents=["newer", "head"], is_ancestor=lambda old, new: (old, new) == ("base", "newer"))
        self.assertEqual(moved["base_sha"], "newer")
        with self.assertRaises(ValueError):
            VERIFY.revision_identity("pull_request", self.event, repository="owner/repo", tested_sha="merge",
                                     parents=["older", "head"], is_ancestor=lambda old, new: False)

    def test_pr_rejects_head_checkout_stale_base_and_other_repository(self):
        for parents in (["base"], ["head", "base"], ["stale", "head"], ["base", "other"], []):
            with self.subTest(parents=parents), self.assertRaises(ValueError):
                self.identity(parents=parents)
        with self.assertRaises(ValueError):
            self.identity(tested_sha="head")
        for target in (self.event["repository"], self.event["pull_request"]["base"]["repo"]):
            target["full_name"] = "other/repo"
            with self.assertRaises(ValueError):
                self.identity()
            target["full_name"] = "owner/repo"

    def test_push_and_scheduled_runs_still_bind_the_exact_checkout(self):
        self.event["after"] = "merge"
        for event_name in ("push", "schedule", "workflow_dispatch"):
            identity = self.identity(event_name=event_name)
            self.assertEqual(identity["api_sha"], "merge")
            self.assertEqual(identity["source_sha"], "merge")
        self.event["after"] = "other"
        with self.assertRaises(ValueError):
            self.identity(event_name="push")
        with self.assertRaises(ValueError):
            self.identity(event_name="pull_request_target")


class VerificationProfileTests(unittest.TestCase):
    def setUp(self):
        self.manifest = json.loads((Path(__file__).parents[1] / "ci-verification.json").read_text())
        self.base, self.sha = "a" * 40, "b" * 40
        self.paths = Mock(return_value=["docs/site/docs/getting-started.md"])
        self.ancestor = Mock(return_value=True)

    def select(self, event_name="push", before=None, identity=None):
        return VERIFY.selection(self.manifest, event_name,
                                {"before": self.base if before is None else before},
                                {} if identity is None else identity, sha=self.sha,
                                is_ancestor=self.ancestor, changed_paths=self.paths)

    def test_only_the_existing_prose_allowlist_gets_focused_verification(self):
        for paths in (["README.md"], ["charts/haptic/README.md", "CHANGELOG.md"],
                      ["docs/site/docs/development/linting.md"]):
            with self.subTest(paths=paths):
                self.assertEqual(VERIFY.paths_profile(self.manifest, paths), "prose")
        for paths in ([], ["README.md", "pkg/controller/controller.go"], ["docs/site/mkdocs.yml"],
                      [".github/workflows/ci.yml"], ["docs/site/docs/example.yaml"],
                      ["scripts/generate.md"], ["charts/haptic/values.yaml"], ["VERSION"],
                      ["docs/site/docs/line\nbreak.md"], ["pkg/old.go", "docs/site/docs/new.md"]):
            with self.subTest(paths=paths):
                self.assertEqual(VERIFY.paths_profile(self.manifest, paths), "full")

    def test_push_and_pr_use_their_real_comparison_base(self):
        for event_name in ("push", "pull_request"):
            result = self.select(event_name, identity={"base_sha": self.base})
            self.assertEqual(result, {"profile": "prose", "comparison_base": self.base,
                                      "changed_paths_sha256": VERIFY.paths_digest(self.paths.return_value)})
            self.paths.assert_called_with(self.base, self.sha)
        self.paths.return_value.append(".github/ci-verification.json")
        self.assertEqual(self.select()["profile"], "full")

    def test_new_branches_force_pushes_and_dispatches_require_full_verification(self):
        for before in ("0" * 40, "", "--all", "short"):
            with self.subTest(before=before):
                self.assertEqual(self.select(before=before), {"profile": "full"})
        self.ancestor.return_value = False
        self.assertEqual(self.select(), {"profile": "full"})
        for event_name in ("schedule", "workflow_dispatch"):
            self.assertEqual(self.select(event_name), {"profile": "full"})
        self.paths.assert_not_called()
        with self.assertRaises(ValueError):
            self.select("pull_request", identity={"base_sha": self.base})

    def test_profile_inventory_rejects_unknown_or_incomplete_policy(self):
        self.assertEqual(VERIFY.required_jobs(self.manifest, "full"), self.manifest["required_jobs"])
        for selected in ([], ["lint", "lint"], ["missing-job"]):
            self.manifest["prose"]["required_jobs"] = selected
            with self.subTest(selected=selected), self.assertRaises(ValueError):
                VERIFY.required_jobs(self.manifest, "prose")
        for profile in ("", "skip", "docs"):
            with self.subTest(profile=profile), self.assertRaises(ValueError):
                VERIFY.required_jobs(self.manifest, profile)


if __name__ == "__main__":
    unittest.main()
