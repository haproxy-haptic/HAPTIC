import copy
import contextlib
import hashlib
import json
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from unittest.mock import Mock, patch

import yaml

from scripts.ci import github_conformance
from scripts.ci.github_conformance import bundle, diagnostics, execute, plan, sample_startup


ROOT = Path(__file__).parents[2]


class ConformanceWorkflowTests(unittest.TestCase):
    def setUp(self):
        observer = patch("scripts.ci.github_conformance.observe_startup", return_value=contextlib.nullcontext())
        observer.start()
        self.addCleanup(observer.stop)
        self.sha = "a" * 40
        self.env = {"GITHUB_REPOSITORY": "owner/repo", "GITHUB_SHA": self.sha,
                    "CONFORMANCE_SOURCE_SHA": self.sha, "CONFORMANCE_MODE": "candidate",
                    "GITHUB_REF_TYPE": "branch", "GITHUB_REF_NAME": "feature",
                    "GITHUB_RUN_ID": "42", "GITHUB_RUN_ATTEMPT": "2"}

    def release(self):
        self.env.update(CONFORMANCE_MODE="release", GITHUB_REF_TYPE="tag", GITHUB_REF_NAME="v0.5.0")
        return plan(self.env, self.sha, "0.5.0")

    def test_candidate_builds_its_exact_source_then_runs_the_unfiltered_report(self):
        selected = plan(self.env, self.sha, "0.5.0")
        run = Mock()
        execute(selected, self.env, run=run, output=Mock(return_value=""))
        calls = run.call_args_list
        self.assertEqual(calls[0].args[0], ["make", "docker-build-test-native", "GIT_TAG=ci-" + self.sha[:12]])
        self.assertEqual(calls[1].args[0], ["make", "test-e2e"])
        self.assertEqual(calls[1].kwargs["env"]["TEST_RUN_PATTERN"], "^$")
        self.assertEqual(calls[2].args[0], ["make", "test-gateway-conformance-report"])
        report = calls[2].kwargs["env"]
        self.assertNotIn("TEST_RUN_PATTERN", report)
        self.assertEqual(report["CONFORMANCE_RELEASE_EVIDENCE"], "")
        self.assertEqual(report["CONFORMANCE_CONTROLLER_IMAGE"], "haptic:test")
        self.assertTrue(all(call.kwargs["check"] for call in calls))

    def test_release_uses_the_published_digest_and_never_rebuilds(self):
        selected = self.release()
        run = Mock()
        digest = "sha256:" + "b" * 64
        output = Mock(side_effect=["", json.dumps(digest)])
        execute(selected, self.env, run=run, output=output)
        image = "ghcr.io/owner/haptic@" + digest
        calls = run.call_args_list
        self.assertEqual(calls[0].args[0], ["bash", "scripts/retry.sh", "docker", "pull", image])
        self.assertEqual(calls[1].args[0], ["docker", "tag", image, "haptic:test"])
        self.assertEqual(calls[2].args[0], ["make", "test-e2e"])
        self.assertEqual(calls[3].args[0], ["make", "test-gateway-conformance-report"])
        self.assertEqual(calls[3].kwargs["env"]["CONFORMANCE_CONTROLLER_IMAGE"], image)
        self.assertEqual(calls[3].kwargs["env"]["CONFORMANCE_RELEASE_EVIDENCE"], "1")
        self.assertTrue(all("docker-build-test-native" not in call.args[0] for call in calls))

    def test_rejects_stale_sources_filters_and_mismatched_release_tags(self):
        for key, value in (("GITHUB_SHA", "b" * 40), ("CONFORMANCE_SOURCE_SHA", "b" * 40),
                           ("GITHUB_REPOSITORY", "owner/../repo"), ("TEST_RUN_PATTERN", "HTTP"),
                           ("CONFORMANCE_MODE", "unknown")):
            with self.subTest(key=key), self.assertRaises(ValueError):
                plan({**self.env, key: value}, self.sha, "0.5.0")
        self.release()
        for key, value in (("GITHUB_REF_TYPE", "branch"), ("GITHUB_REF_NAME", "v0.4.0")):
            with self.subTest(key=key), self.assertRaises(ValueError):
                plan({**self.env, key: value}, self.sha, "0.5.0")

    def test_rejects_reused_clusters_and_invalid_registry_digests(self):
        selected = self.release()
        run = Mock()
        for responses in (["existing-kind-cluster"], ["", json.dumps("latest")]):
            with self.subTest(responses=responses), self.assertRaises(ValueError):
                execute(selected, self.env, run=run, output=Mock(side_effect=responses))
        run.assert_not_called()

    def test_startup_and_report_failures_propagate(self):
        selected = plan(self.env, self.sha, "0.5.0")
        for step in (1, 2):
            responses = [None] * step + [subprocess.CalledProcessError(1, "make")]
            run = Mock(side_effect=responses)
            with self.subTest(step=step), self.assertRaises(subprocess.CalledProcessError):
                execute(selected, self.env, run=run, output=Mock(return_value=""))
            self.assertEqual(run.call_count, step + 1)

    def test_startup_diagnosis_keeps_real_setup_and_cannot_replace_release_conformance(self):
        selected = plan(self.env, self.sha, "0.5.0")
        run = Mock()
        execute(selected, self.env, startup_only=True, run=run, output=Mock(return_value=""))
        self.assertEqual([call.args[0] for call in run.call_args_list], [
            ["make", "docker-build-test-native", "GIT_TAG=ci-" + self.sha[:12]], ["make", "test-e2e"]])
        self.assertEqual(run.call_args_list[-1].kwargs["env"]["HAPTIC_E2E_CONTROLLER_STARTUP"], "parallel")
        run.reset_mock()
        with self.assertRaisesRegex(ValueError, "complete report"):
            execute(self.release(), self.env, startup_only=True, run=run, output=Mock())
        run.assert_not_called()

    def test_startup_sampler_records_missing_cluster_and_continues_collecting(self):
        run = Mock(side_effect=[subprocess.TimeoutExpired("docker", 5),
                               subprocess.CompletedProcess("kubectl", 1, "", "cluster not ready"),
                               subprocess.CompletedProcess("kubectl", 0, '{"items": []}', ""),
                               subprocess.CompletedProcess("docker", 0, "oom_kill 0\n", "")])
        result = sample_startup(run=run)
        self.assertIn("error", result["pods"])
        self.assertEqual(result["events"]["exit_code"], 1)
        self.assertEqual(result["usage"], {"items": []})
        self.assertEqual(result["node_cgroup"], "oom_kill 0\n")
        for call in run.call_args_list:
            self.assertNotIn("secrets", call.args[0])
            self.assertEqual(call.kwargs["timeout"], 5)

    def test_startup_sampler_keeps_restart_and_scheduling_state_without_pod_environment(self):
        pod = {"metadata": {"namespace": "haptic", "name": "controller", "creationTimestamp": "created"},
               "status": {"phase": "Pending", "conditions": [{"reason": "Unschedulable"}],
                          "containerStatuses": [{"restartCount": 1, "lastState": {"reason": "OOMKilled"}}]},
               "spec": {"containers": [{"env": [{"name": "private", "value": "excluded"}]}]}}
        run = Mock(side_effect=[subprocess.CompletedProcess("kubectl", 0, json.dumps({"items": [pod]}), "")]
                   + [subprocess.CompletedProcess("docker", 1, "", "unavailable")] * 3)
        result = sample_startup(run=run)
        self.assertEqual(result["pods"][0]["status"], pod["status"])
        self.assertNotIn("excluded", json.dumps(result))

    def test_release_workflow_keeps_the_report_after_image_and_chart_publication(self):
        release = yaml.safe_load((ROOT / ".github/workflows/release.yml").read_text())
        job = release["jobs"]["conformance"]
        self.assertEqual(set(job["needs"]), {"source", "chart"})
        self.assertEqual(job["uses"], "./.github/workflows/conformance-report.yml")
        self.assertEqual(job["with"], {"source_sha": "${{ needs.source.outputs.sha }}", "release": True})
        workflow = yaml.safe_load((ROOT / ".github/workflows/conformance-report.yml").read_text())
        self.assertEqual(workflow["jobs"]["report"]["timeout-minutes"], 60)
        publish = workflow["jobs"]["publish"]
        self.assertEqual(publish["if"], "inputs.release")
        self.assertEqual(publish["needs"], "report")
        self.assertEqual(publish["steps"][-1]["run"],
                         'gh release upload "$RELEASE_TAG" "$RUNNER_TEMP/gateway-conformance/$ARCHIVE"')

    def test_conformance_uses_the_native_arm_toolchain(self):
        wrapper = (ROOT / ".github/scripts/run-conformance-report.sh").read_text()
        self.assertIn('test "$(uname -m)" = aarch64', wrapper)
        self.assertLess(wrapper.index("make test-github-native-tools"), wrapper.index('make "$CONFORMANCE_TARGET"'))
        self.assertNotIn("docker run", wrapper)
        for filename, job_name in (("conformance-report.yml", "report"),):
            with self.subTest(workflow=filename):
                workflow = yaml.safe_load((ROOT / ".github/workflows" / filename).read_text())
                job = workflow["jobs"][job_name]
                self.assertEqual(job["runs-on"], "ubuntu-24.04-arm")
                steps = job["steps"]
                install = next(i for i, step in enumerate(steps)
                               if step.get("run") == "bash .github/scripts/install-arm-pilot-tools.sh")
                compiler = next(i for i, step in enumerate(steps)
                                if step.get("uses", "").startswith("actions/setup-go@"))
                run = next(i for i, step in enumerate(steps)
                           if step.get("run", "").startswith("bash .github/scripts/run-conformance-report.sh"))
                self.assertLess(install, compiler)
                self.assertLess(compiler, run)
                self.assertEqual(steps[compiler]["with"]["go-version"], "${{ env.HAPTIC_NATIVE_GO_VERSION }}")

    def test_diagnostics_survive_missing_pods_and_timed_out_commands(self):
        with tempfile.TemporaryDirectory() as directory:
            run = Mock(side_effect=[subprocess.TimeoutExpired("docker", 30)]
                       + [subprocess.CompletedProcess("kubectl", 1)] * 29)
            diagnostics(Path(directory), run=run)
            self.assertEqual(run.call_count, 30)
            results = json.loads((Path(directory) / "collection.json").read_text())
            self.assertIn("error", results["containers.txt"])
            self.assertEqual(results["pods.json"], {"exit_code": 1})
            for call in run.call_args_list:
                self.assertNotIn("secrets", call.args[0])
                self.assertEqual(call.kwargs["timeout"], 30)

    def test_diagnostics_retain_backend_logs_and_each_replica_mirror_outcomes(self):
        def run(command, **kwargs):
            stream = kwargs["stdout"]
            if "get" in command and "pods" in command:
                pods = [{"metadata": {"name": name, "namespace": namespace,
                         "labels": {"app.kubernetes.io/component": component}}}
                        for name, namespace, component in (
                            ("haproxy-a", "haptic", "loadbalancer"),
                            ("haproxy-b", "haptic", "loadbalancer"),
                            ("controller-a", "haptic", "controller"),
                            ("other", "other", "loadbalancer"))]
                stream.write(json.dumps({"items": pods}))
            if "curl" in command:
                if "haproxy-a" in command:
                    raise subprocess.TimeoutExpired(command, 30)
                stream.write('plugin_mirror_dispatch_outcomes_total{outcome="error"} 6\n')
            return subprocess.CompletedProcess(command, 0)

        with tempfile.TemporaryDirectory() as directory:
            recorded = Mock(side_effect=run)
            diagnostics(Path(directory), run=recorded)
            results = json.loads((Path(directory) / "collection.json").read_text())
            self.assertIn("error", results["haproxy-a-spoa.metrics"])
            self.assertEqual(results["haproxy-b-spoa.metrics"], {"exit_code": 0})
            self.assertIn('outcome="error"', (Path(directory) / "haproxy-b-spoa.metrics").read_text())
            self.assertFalse((Path(directory) / "controller-a-spoa.metrics").exists())
            self.assertFalse((Path(directory) / "other-spoa.metrics").exists())
            for backend in ("infra-backend-v1", "infra-backend-v2", "infra-backend-v3"):
                self.assertEqual(results[f"gateway-conformance-infra-{backend}-{backend}-current.log"],
                                 {"exit_code": 0})
            metrics = [call for call in recorded.call_args_list if "curl" in call.args[0]]
            self.assertEqual(len(metrics), 2)
            for call in metrics:
                self.assertEqual(call.args[0][-1], "http://127.0.0.1:9095/metrics")
                self.assertNotIn("shell", call.kwargs)

    def test_diagnostics_attribute_unreachable_vips_to_a_network_layer(self):
        with tempfile.TemporaryDirectory() as directory:
            run = Mock(return_value=subprocess.CompletedProcess("kubectl", 0))
            diagnostics(Path(directory), run=run)
            commands = [" ".join(call.args[0]) for call in run.call_args_list]
        for expected in ("get services -A", "get endpointslices -A", "get servicel2statuses.metallb.io -A",
                         "get configmap kube-proxy -n kube-system", "get events -A", "ip neigh show",
                         "logs -n metallb-system -l app=metallb,component=controller -c controller",
                         "logs -n metallb-system -l app=metallb,component=speaker -c speaker",
                         "logs -n kube-system -l k8s-app=kube-proxy -c kube-proxy",
                         "logs -n haptic -l app.kubernetes.io/component=controller -c controller"):
            matching = [command for command in commands if expected in command]
            with self.subTest(expected=expected):
                self.assertTrue(matching)
                if " logs " in matching[0]:
                    self.assertEqual(sum(command.endswith("--previous") for command in matching), 1)

    def test_conformance_shards_collect_diagnostics_into_the_retained_evidence(self):
        workflow = yaml.safe_load((ROOT / ".github/workflows/ci-speed-proof.yml").read_text())
        jobs = [job for job in workflow["jobs"].values()
                if any("make test-gateway-conformance" in step.get("run", "") for step in job.get("steps", []))]
        self.assertTrue(jobs)
        for job in jobs:
            with self.subTest(job=job["name"]):
                steps = job["steps"]
                names = [step.get("name") for step in steps]
                conformance = next(i for i, step in enumerate(steps)
                                   if "make test-gateway-conformance" in step.get("run", ""))
                collect = names.index("Collect conformance cluster diagnostics")
                retain = names.index("Retain verification evidence")
                self.assertLess(conformance, collect)
                self.assertLess(collect, retain)
                self.assertTrue(steps[collect]["if"].startswith("always()"))
                self.assertEqual(steps[collect]["run"],
                                 "python3 -m scripts.ci.github_conformance --diagnostics .pilot/conformance-diagnostics")
                self.assertIn(".pilot/", steps[retain]["with"]["path"].split())
        self.assertIn("suite_gateway_canary", [key for key, job in workflow["jobs"].items() if job in jobs])

    def test_diagnostics_mode_collects_without_planning_a_run(self):
        with patch("scripts.ci.github_conformance.diagnostics") as collect, \
                patch("scripts.ci.github_conformance.plan") as planned, \
                patch("sys.argv", ["github_conformance", "--diagnostics", "out"]):
            github_conformance.main()
        collect.assert_called_once_with(Path("out"))
        planned.assert_not_called()


class ConformanceArchiveTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.selected = {"sha": "a" * 40, "version": "0.5.0", "release": True}
        self.env = {"GITHUB_RUN_ID": "42", "GITHUB_RUN_ATTEMPT": "2",
                    "GITHUB_OUTPUT": str(self.root / "outputs")}
        (self.root / "report.yaml").write_text("verified report\n")
        (self.root / "kubeconfig").write_text("must not be uploaded")
        self.record = {"git_commit": "a" * 40, "release_evidence": True, "verified": True,
                       "test_exit_code": 0, "errors": [], "selection": "full-unsharded-suite",
                       "dirty_checkout": False, "controller": {"version": "0.5.0", "commit": "a" * 40,
                                                                 "registry_digests": ["image@sha256:abc"]},
                       "report_sha256": hashlib.sha256((self.root / "report.yaml").read_bytes()).hexdigest()}
        self.write()

    def write(self):
        (self.root / "provenance.json").write_text(json.dumps(self.record))
        sums = "".join(hashlib.sha256((self.root / name).read_bytes()).hexdigest() + "  " + name + "\n"
                       for name in ("report.yaml", "provenance.json"))
        (self.root / "SHA256SUMS").write_text(sums)

    def test_archive_contains_only_verified_public_evidence(self):
        name = bundle(self.selected, self.env, self.root)
        self.assertEqual(name, "gateway-conformance-0.5.0-42-2.tar.gz")
        with tarfile.open(self.root / name) as archive:
            self.assertEqual(archive.getnames(), ["report.yaml", "provenance.json", "SHA256SUMS"])
        self.assertIn("version=0.5.0\n", (self.root / "outputs").read_text())
        with self.assertRaises(FileExistsError):
            bundle(self.selected, self.env, self.root)

    def test_incomplete_stale_and_unpublished_evidence_is_rejected(self):
        original = copy.deepcopy(self.record)
        for key, value in (("git_commit", "b" * 40), ("release_evidence", False), ("verified", False),
                           ("test_exit_code", 1), ("errors", ["incomplete"]), ("selection", "shard-1"),
                           ("dirty_checkout", True), ("report_sha256", "other"),
                           ("controller", {"version": "0.5.0", "commit": "b" * 40, "registry_digests": ["x"]}),
                           ("controller", {"version": "0.5.0", "commit": "a" * 40, "registry_digests": []})):
            self.record = {**original, key: value}
            self.write()
            with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                bundle(self.selected, self.env, self.root)

    def test_tampered_or_ambiguous_checksum_inventory_is_rejected(self):
        sums = (self.root / "SHA256SUMS").read_text()
        for contents in (sums + sums, sums.replace("report.yaml", "kubeconfig"), sums.replace("  ", "  missing-")):
            (self.root / "SHA256SUMS").write_text(contents)
            with self.subTest(contents=contents), self.assertRaises(ValueError):
                bundle(self.selected, self.env, self.root)


if __name__ == "__main__":
    unittest.main()
