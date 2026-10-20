import copy
import hashlib
import http.client
import io
import json
import tempfile
import unittest
import urllib.error
import zipfile
from unittest import mock

from scripts.ci.github import GitHub, jobs_for_run
from scripts.ci.github_history import successful_job_metrics
from scripts.ci.github_scale import TREND, run_head_sha


class HistoryTests(unittest.TestCase):
    def setUp(self):
        self.run = {"id": 42, "workflow_id": 7, "head_sha": "abc", "run_attempt": 2,
                    "head_branch": "main", "event": "schedule", "status": "completed", "conclusion": "success"}
        self.job = {"id": 9, "run_id": 42, "run_attempt": 2, "head_sha": "abc", "name": "nightly-scale",
                    "status": "completed", "conclusion": "success",
                    "started_at": "2026-10-13T01:00:00Z", "completed_at": "2026-10-13T01:05:00Z"}
        self.churn = dict(self.job, id=10, name="nightly-gateway-churn")
        self.artifact = {"id": 19, "name": "nightly-scale-metrics", "expired": False,
                         "created_at": "2026-10-13T01:04:00Z",
                         "workflow_run": {"id": 42, "head_sha": "abc", "head_branch": "main"}}
        self.api = mock.Mock(spec=GitHub)
        self.api.all.side_effect = lambda path, field, **params: iter({
            "workflow_runs": [self.run], "jobs": [self.job, self.churn], "artifacts": [self.artifact]}[field])
        self.api.artifact.return_value = self.archive()
        self.artifact["digest"] = "sha256:" + hashlib.sha256(self.api.artifact.return_value).hexdigest()

    @staticmethod
    def archive(name="scale-metrics.json", contents=None):
        output = io.BytesIO()
        with zipfile.ZipFile(output, "w") as archive:
            archive.writestr(name, json.dumps(contents or {"runner_cpu_model": "cpu", "controller_rss_bytes": 100}))
        return output.getvalue()

    def history(self, **kwargs):
        return successful_job_metrics(self.api, "owner/repo", 43, workflow="ci.yml", branch="main",
                                      job_name="nightly-scale", artifact_name="nightly-scale-metrics",
                                      metrics_name="scale-metrics.json", max_runs=14, max_workflows=60, **kwargs)

    def test_integrated_scale_uses_canonical_pushes_and_never_pr_or_fork_history(self):
        self.run.update(event="push", repository={"full_name": "owner/repo"},
                        head_repository={"full_name": "owner/repo"})
        self.assertEqual(self.history(), [])
        allowed = ("push", "schedule", "workflow_dispatch")
        self.assertEqual(len(self.history(events=allowed)), 1)
        self.run["head_repository"]["full_name"] = "fork/repo"
        self.assertEqual(self.history(events=allowed), [])
        self.run["head_repository"]["full_name"] = "owner/repo"
        for event in ("pull_request", "pull_request_target", "merge_group"):
            self.run["event"] = event
            self.assertEqual(self.history(events=allowed), [])

    def test_successful_job_is_a_baseline_even_if_another_job_failed(self):
        self.run["conclusion"] = "failure"
        self.assertEqual(self.history(), [{"runner_cpu_model": "cpu", "controller_rss_bytes": 100, "_job_id": 9}])

    def test_ineligible_runs_and_failed_jobs_are_not_baselines(self):
        original = copy.deepcopy(self.run)
        for key, value in (("id", 43), ("id", 44), ("head_branch", "feature"), ("event", "pull_request"), ("status", "in_progress")):
            with self.subTest(key=key):
                self.run.update(original)
                self.run[key] = value
                self.assertEqual(self.history(), [])
        self.run.update(original)
        for conclusion in ("failure", "skipped", "cancelled", "timed_out", None):
            self.job["conclusion"] = conclusion
            self.assertEqual(self.history(), [])
        self.api.artifact.assert_not_called()

    def test_expired_artifacts_are_skipped_but_api_failures_are_errors(self):
        self.artifact["expired"] = True
        self.assertEqual(self.history(), [])
        self.artifact["expired"] = False
        for code in (404, 410):
            self.api.artifact.side_effect = self.enterContext(
                urllib.error.HTTPError("https://api.github.com", code, "expired", {}, None))
            self.assertEqual(self.history(), [])
        self.api.artifact.side_effect = self.enterContext(
            urllib.error.HTTPError("https://api.github.com", 403, "denied", {}, None))
        with self.assertRaises(urllib.error.HTTPError):
            self.history()

    def test_cross_revision_attempt_and_stale_artifacts_fail(self):
        for key, value in (("head_sha", "other"), ("run_id", 41), ("run_attempt", 1)):
            original = self.job[key]
            self.job[key] = value
            with self.subTest(job=key), self.assertRaises(ValueError):
                self.history()
            self.job[key] = original
        for key, value in (("head_sha", "other"), ("id", 41), ("head_branch", "feature")):
            original = self.artifact["workflow_run"][key]
            self.artifact["workflow_run"][key] = value
            with self.subTest(artifact=key), self.assertRaises(ValueError):
                self.history()
            self.artifact["workflow_run"][key] = original
        self.artifact["created_at"] = "2026-10-12T01:04:00Z"
        with self.assertRaises(ValueError):
            self.history()

    def test_archive_paths_are_never_extracted_or_treated_as_metrics(self):
        for name in ("../scale-metrics.json", "other.json", "/scale-metrics.json"):
            self.api.artifact.return_value = self.archive(name)
            self.artifact["digest"] = "sha256:" + hashlib.sha256(self.api.artifact.return_value).hexdigest()
            with self.subTest(name=name), self.assertRaisesRegex(ValueError, "unexpected contents"):
                self.history()

    def test_artifact_bytes_must_match_the_digest_from_github(self):
        self.api.artifact.return_value = self.archive(contents={"controller_rss_bytes": 1})
        with self.assertRaisesRegex(ValueError, "digest does not match"):
            self.history()

    def test_duplicate_jobs_are_rejected(self):
        self.api.all.return_value = iter([self.job, self.job])
        self.api.all.side_effect = None
        with self.assertRaises(ValueError):
            jobs_for_run(self.api, "owner/repo", self.run)

    def test_scale_gate_binds_pull_requests_to_their_head_commit(self):
        self.assertEqual(run_head_sha({"GITHUB_EVENT_NAME": "push", "GITHUB_SHA": "abc"}), "abc")
        with tempfile.NamedTemporaryFile("w", suffix=".json") as event:
            json.dump({"pull_request": {"head": {"sha": "head"}}}, event)
            event.flush()
            env = {"GITHUB_EVENT_NAME": "pull_request", "GITHUB_SHA": "merge", "GITHUB_EVENT_PATH": event.name}
            self.assertEqual(run_head_sha(env), "head")

    def test_scale_gate_reuses_existing_comparison_contract(self):
        self.assertEqual(TREND.HARD, {"change_convergence_seconds_p95": 0.5,
                                     "haproxycfg_publication_seconds_p95": 0.5, "controller_rss_bytes": 0.5})
        self.assertEqual((TREND.WINDOW, TREND.MIN_BASELINES, TREND.MAX_RUNS), (7, 3, 14))
        history = [{"runner_cpu_model": "cpu", "_job_id": n, "controller_rss_bytes": 100} for n in range(3)]
        lines, failed = TREND.evaluate({"runner_cpu_model": "cpu", "controller_rss_bytes": 151}, history)
        self.assertTrue(failed, lines)
        lines, failed = TREND.evaluate({"runner_cpu_model": "other", "controller_rss_bytes": 151}, history)
        self.assertFalse(failed, lines)
        self.assertIn("TREND UNGATED", "\n".join(lines))


class RetryTests(unittest.TestCase):
    @mock.patch("time.sleep")
    @mock.patch("urllib.request.build_opener")
    def test_truncated_response_is_retried(self, opener, sleep):
        truncated = mock.MagicMock()
        truncated.__enter__.return_value.read.side_effect = http.client.IncompleteRead(b"", 225283)
        complete = mock.MagicMock()
        complete.__enter__.return_value.read.return_value = b'{"jobs": []}'
        opener.return_value.open.side_effect = [truncated, complete]
        self.assertEqual(GitHub("test-token").get("repos/owner/repo/actions/runs/1/jobs"), {"jobs": []})
        sleep.assert_called_once_with(2)

    @mock.patch("time.sleep")
    @mock.patch("urllib.request.build_opener")
    def test_persistent_transport_failure_raises(self, opener, sleep):
        opener.return_value.open.side_effect = urllib.error.URLError("connection reset")
        with self.assertRaises(urllib.error.URLError):
            GitHub("test-token").get("repos/owner/repo")
        self.assertEqual(sleep.call_count, 5)


class DownloadTests(unittest.TestCase):
    @mock.patch("urllib.request.urlopen")
    @mock.patch("urllib.request.build_opener")
    def test_signed_storage_download_receives_no_github_token(self, opener, urlopen):
        opener.return_value.open.side_effect = urllib.error.HTTPError(
            "https://api.github.com", 302, "found", {"Location": "https://storage.example/artifact?signature=secret"}, None)
        urlopen.return_value.read.return_value = b"zip"
        self.assertEqual(GitHub("test-token").artifact("owner/repo", 19, 100), b"zip")
        request = opener.return_value.open.call_args.args[0]
        self.assertEqual(request.get_header("Authorization"), "Bearer test-token")
        urlopen.assert_called_once_with("https://storage.example/artifact?signature=secret", timeout=30)

    @mock.patch("urllib.request.build_opener")
    def test_oversized_download_fails(self, opener):
        opener.return_value.open.return_value.read.return_value = b"oversized"
        with self.assertRaises(ValueError):
            GitHub("test-token").artifact("owner/repo", 19, 3)


if __name__ == "__main__":
    unittest.main()
