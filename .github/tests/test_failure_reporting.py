import copy
from pathlib import Path
import unittest
from unittest import mock
import urllib.error

import yaml

from scripts.ci.github import GitHub
from scripts.ci.github_failures import family, publish, report_run, reports


ROOT = Path(__file__).parents[2]


class FailureReportingTests(unittest.TestCase):
    def setUp(self):
        self.run = {"id": 42, "run_attempt": 2, "head_sha": "a" * 40, "workflow_id": 7,
                    "path": ".github/workflows/ci.yml", "head_branch": "main", "event": "push",
                    "status": "completed", "conclusion": "failure",
                    "repository": {"full_name": "owner/repo"}, "head_repository": {"full_name": "owner/repo"}}
        self.job = {"id": 9, "run_id": 42, "run_attempt": 2, "head_sha": "a" * 40,
                    "name": "full / e2e-hp3.4-1", "conclusion": "failure", "steps": []}
        self.env = {"GITHUB_REPOSITORY": "owner/repo", "GITHUB_EVENT_NAME": "workflow_run"}
        self.event = {"repository": {"full_name": "owner/repo", "default_branch": "main"},
                      "workflow_run": copy.deepcopy(self.run)}
        self.api = mock.Mock(spec=GitHub)
        self.api.get.return_value = self.run
        self.api.all.side_effect = lambda path, field, **params: iter([self.job] if field == "jobs" else [])
        self.api.post.return_value = {"number": 123, "body": "created"}

    def selected(self, jobs=None):
        return reports("owner/repo", self.run, [self.job] if jobs is None else jobs)

    def test_failures_create_an_issue_with_exact_attempt_and_job_links(self):
        selected = report_run(self.api, self.env, self.event)
        self.assertEqual(len(selected), 1)
        self.api.get.assert_any_call("repos/owner/repo/actions/runs/42/attempts/2")
        self.api.all.assert_any_call("repos/owner/repo/actions/runs/42/attempts/2/jobs", "jobs")
        path, body = self.api.post.call_args.args
        self.assertEqual(path, "repos/owner/repo/issues")
        self.assertEqual(body["labels"], ["needs-triage", "ci-failure"])
        self.assertIn("/actions/runs/42/attempts/2", body["body"])
        self.assertIn("/actions/runs/42/job/9", body["body"])
        self.assertIn("github-ci-occurrence:42:2:failure", body["body"])

    def test_other_repositories_branches_events_workflows_and_cancellations_are_ignored(self):
        original = copy.deepcopy(self.event["workflow_run"])
        changes = [("head_repository", {"full_name": "fork/repo"}), ("repository", {"full_name": "other/repo"}),
                   ("head_repository", None),
                   ("head_branch", "feature"), ("event", "pull_request"), ("event", "merge_group"),
                   ("path", ".github/workflows/untrusted.yml"), ("status", "in_progress"),
                   ("conclusion", "cancelled"), ("conclusion", "skipped")]
        for key, value in changes:
            with self.subTest(key=key, value=value):
                self.event["workflow_run"] = {**original, key: value}
                self.assertEqual(report_run(self.api, self.env, self.event), [])
        self.api.get.assert_not_called()
        self.api.post.assert_not_called()

    def test_wrong_trigger_and_changed_attempt_or_revision_fail_before_writes(self):
        for key, value in (("id", 43), ("run_attempt", 3), ("head_sha", "b" * 40), ("workflow_id", 8),
                           ("path", ".github/workflows/other.yml"), ("event", "workflow_dispatch"),
                           ("head_branch", "feature"), ("status", "in_progress"), ("conclusion", "success")):
            with self.subTest(key=key):
                self.api.get.return_value = {**self.run, key: value}
                with self.assertRaises(ValueError):
                    report_run(self.api, self.env, self.event)
        self.env["GITHUB_EVENT_NAME"] = "push"
        with self.assertRaises(ValueError):
            report_run(self.api, self.env, self.event)
        self.api.post.assert_not_called()

    def test_job_evidence_from_another_attempt_cannot_be_reported(self):
        self.job["run_attempt"] = 1
        with self.assertRaises(ValueError):
            report_run(self.api, self.env, self.event)
        self.api.post.assert_not_called()

    def test_canary_failure_is_reported_even_when_the_workflow_and_job_succeed(self):
        self.run.update(conclusion="success")
        canary = {**self.job, "name": "full / gateway-canary", "conclusion": "success",
                  "steps": [{"conclusion": "failure"}]}
        selected = self.selected([canary])
        self.assertEqual(len(selected), 1)
        self.assertTrue(selected[0]["title"].startswith("Canary failure:"))
        self.assertIn("allowed to fail", selected[0]["body"])
        mixed = self.selected([self.job, canary])
        self.assertEqual(len(mixed), 2)
        self.assertNotEqual(mixed[0]["marker"], mixed[1]["marker"])

    def test_successful_runs_need_no_issue_access_and_unattributed_failures_remain_visible(self):
        self.run["conclusion"] = self.event["workflow_run"]["conclusion"] = "success"
        self.job["conclusion"] = "success"
        self.assertEqual(report_run(self.api, self.env, self.event), [])
        self.api.post.assert_not_called()
        self.assertEqual(self.api.all.call_count, 1)
        self.run["conclusion"] = "failure"
        self.assertIn("workflow infrastructure", self.selected([])[0]["title"])

    def test_canary_failure_keeps_its_non_blocking_classification(self):
        self.run["conclusion"] = "success"
        self.job["name"] = "full / gateway-canary"
        selected = self.selected()
        self.assertEqual(len(selected), 1)
        self.assertTrue(selected[0]["title"].startswith("Canary failure:"))

    def test_fingerprint_is_stable_across_shards_and_differs_across_failure_families(self):
        original = self.selected()[0]["marker"]
        self.job["name"] = "full / e2e-hp3.4-4"
        self.assertEqual(self.selected()[0]["marker"], original)
        self.job["name"] = "full / e2e-hp3.3-4"
        self.assertNotEqual(self.selected()[0]["marker"], original)
        self.assertEqual(family("full / gateway-2"), "gateway")
        self.assertEqual(family("full / e2e-minimum-kubernetes-4"), "e2e-minimum-kubernetes")
        self.assertEqual(family("full / gateway-compatibility-v1.1.0"), "gateway-compatibility-v1.1.0")

    def test_recurrences_comment_and_duplicate_events_are_idempotent(self):
        selected = self.selected()
        report = selected[0]
        issue = {"number": 123, "body": report["marker"]}
        comments = []
        self.api.all.side_effect = lambda path, field, **params: iter(comments if path.endswith("/comments") else [issue])
        publish(self.api, "owner/repo", selected)
        self.assertEqual(self.api.post.call_args.args[0], "repos/owner/repo/issues/123/comments")
        comments.append({"body": self.api.post.call_args.args[1]["body"]})
        self.api.post.reset_mock()
        publish(self.api, "owner/repo", selected)
        self.api.post.assert_not_called()
        comments.clear()
        issue["body"] += report["occurrence"]
        publish(self.api, "owner/repo", selected)
        self.api.post.assert_not_called()

    def test_issues_api_pagination_reads_beyond_the_first_page(self):
        api = GitHub("unused")
        api.get = mock.Mock(side_effect=[[{"number": index} for index in range(50)], [{"number": 123}]])
        issues = list(api.all("repos/owner/repo/issues", None, state="open", labels="ci-failure"))
        self.assertEqual(issues[-1]["number"], 123)
        api.get.assert_called_with("repos/owner/repo/issues", per_page=50, page=2, state="open", labels="ci-failure")

    def test_missing_labels_are_created_but_permission_errors_are_not_hidden(self):
        self.api.get.side_effect = self.enterContext(
            urllib.error.HTTPError("https://api.github.com", 404, "missing", {}, None))
        publish(self.api, "owner/repo", self.selected())
        self.api.post.assert_any_call("repos/owner/repo/labels", {"name": "ci-failure", "color": "d73a4a"})
        self.api.post.reset_mock()
        self.api.get.side_effect = self.enterContext(
            urllib.error.HTTPError("https://api.github.com", 403, "denied", {}, None))
        with self.assertRaises(urllib.error.HTTPError):
            publish(self.api, "owner/repo", self.selected())
        self.api.post.assert_not_called()

    def test_workflow_uses_trusted_default_checkout_without_artifacts_or_secrets(self):
        workflow = yaml.safe_load((ROOT / ".github/workflows/report-failures.yml").read_text())
        self.assertEqual(workflow["on"], {"workflow_run": {
            "workflows": ["CI"], "types": ["completed"]}})
        self.assertEqual(workflow["permissions"], {"contents": "read", "actions": "read", "issues": "write"})
        self.assertEqual(workflow["concurrency"], {
            "group": "report-failures-${{ github.event.workflow_run.id }}", "cancel-in-progress": False})
        steps = workflow["jobs"]["report"]["steps"]
        self.assertEqual(len(steps), 2)
        self.assertEqual(steps[0]["with"], {"persist-credentials": False})
        self.assertEqual(steps[1]["run"], "python3 -m scripts.ci.github_failures")
        self.assertNotIn("secrets.", str(workflow))


if __name__ == "__main__":
    unittest.main()
