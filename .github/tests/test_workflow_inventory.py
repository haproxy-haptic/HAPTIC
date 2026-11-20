import collections
import json
from pathlib import Path
import unittest

import yaml


ROOT = Path(__file__).parents[2]


class WorkflowInventoryTests(unittest.TestCase):
    def setUp(self):
        self.workflow = yaml.safe_load((ROOT / ".github/workflows/ci-speed-proof.yml").read_text())
        self.manifest = json.loads((ROOT / ".github/ci-verification.json").read_text())

    def test_gate_requires_every_declared_job(self):
        names = [job["name"] for job in self.workflow["jobs"].values()]
        self.assertEqual(len(names), len(set(names)))
        self.assertEqual(set(names), set(self.manifest["required_jobs"]) | {"verified"})
        self.assertEqual(self.manifest["allowed_failure_jobs"], ["gateway-canary"])
        self.assertEqual({job["name"] for job in self.workflow["jobs"].values()
                          if job.get("continue-on-error")}, {"gateway-canary"})

    def test_skipped_jobs_keep_unique_names_without_matrix_expansion(self):
        names = [job["name"] for job in self.workflow["jobs"].values()]
        self.assertEqual(len(names), len(set(names)))
        self.assertEqual(set(names), set(self.manifest["required_jobs"]) | {"verified"})
        for job in self.workflow["jobs"].values():
            self.assertNotIn("strategy", job)
            if "CI_SUITE" in job.get("env", {}):
                suite = json.loads(job["env"]["CI_SUITE"])
                self.assertEqual(job["name"], suite["name"])
                self.assertEqual(job["runs-on"], suite["runner"])

    def test_coverage_has_every_profile_without_waiting_for_unrelated_suites(self):
        jobs = self.workflow["jobs"]
        self.assertEqual({jobs[group]["name"] for group in jobs["coverage"]["needs"]}, {
            "coverage-rest", "coverage-renderer",
            "integration-3.0", "integration-3.2", "integration-3.3", "integration-3.4",
        })
        self.assertEqual(set(jobs["verified"]["needs"]), set(jobs) - {"verified"})

    def test_execution_chains_include_coverage_within_the_runner_limit(self):
        jobs = self.workflow["jobs"]
        suites = {key: job for key, job in jobs.items() if "CI_SUITE" in job.get("env", {})}
        roots = {key for key, job in suites.items() if not job.get("needs")}
        self.assertLessEqual(len(roots), 60)
        parents = {key: job["needs"] for key, job in suites.items() if job.get("needs")}
        children = collections.Counter(parents.values())
        coverage_lanes = [key for key in jobs["coverage"]["needs"] if not children[key]]
        self.assertTrue(coverage_lanes, "Coverage must take over an input producer's lane")
        parents["coverage"] = coverage_lanes[0]
        children[coverage_lanes[0]] += 1
        suites["coverage"] = jobs["coverage"]
        for key, parent in parents.items():
            self.assertIn(parent, suites)
            visited = set()
            current = key
            while current not in roots:
                self.assertNotIn(current, visited, "Execution chains must not contain cycles")
                visited.add(current)
                current = parents[current]
        self.assertTrue(all(count == 1 for count in children.values()), "A lane must not fork")

    def test_prose_profile_keeps_lint_docs_and_toolchain_images(self):
        expected = {"lint", "docs", "docs-image", "ci-image-3.0", "ci-image-3.2",
                    "ci-image-3.3", "ci-image-3.4"}
        self.assertEqual(set(self.manifest["prose"]["required_jobs"]), expected)
        self.assertTrue(all(path.endswith(".md") or path == "LICENSE" for path in self.manifest["prose"]["paths"]))
        for key, job in self.workflow["jobs"].items():
            if key == "verified":
                self.assertEqual(job["if"], "always()")
                continue
            condition = ("${{ !cancelled() }}" if job["name"] in expected else
                         "${{ !cancelled() && inputs.verification_profile != 'prose' }}")
            self.assertEqual(job["if"], condition)
        self.assertEqual(list(self.workflow["on"]), ["workflow_call"])
        inputs = self.workflow["on"]["workflow_call"]["inputs"]
        self.assertEqual(inputs["verification_profile"]["default"], "full")

    def test_isolated_suites_keep_their_selection_deadlines_and_failure_policy(self):
        expected = {
            "gateway-compatibility-v1.1.0": (25, {"TEST_RUN_PATTERN": "TestGatewayAPIReleaseMatrix",
                                                   "HAPTIC_E2E_GWAPI_VERSION": "v1.1.0"}),
            "gateway-compatibility-v1.4.0": (25, {"TEST_RUN_PATTERN": "TestGatewayAPIReleaseMatrix",
                                                   "HAPTIC_E2E_GWAPI_VERSION": "v1.4.0"}),
            "gateway-compatibility-v1.5.1": (25, {"TEST_RUN_PATTERN": "TestGatewayAPIReleaseMatrix",
                                                   "HAPTIC_E2E_GWAPI_VERSION": "v1.5.1"}),
            "gateway-churn": (30, {"TEST_RUN_PATTERN": "TestGatewayChurn", "HAPTIC_E2E_CHURN": "1"}),
            "scale": (45, {"TEST_RUN_PATTERN": "TestScale", "HAPTIC_E2E_SCALE": "1", "HAPTIC_E2E_AGENT_DEBUG": "1"}),
            "gateway-canary": (60, {"HAPTIC_E2E_PROFILE": "conformance", "HAPTIC_E2E_GWAPI_CHANNEL": "experimental"}),
        }
        jobs = {job["name"]: job for job in self.workflow["jobs"].values()}
        for name, (timeout, env) in expected.items():
            with self.subTest(job=name):
                job = jobs[name]
                self.assertEqual((job["timeout-minutes"], job["runs-on"]), (timeout, "ubuntu-24.04-arm"))
                self.assertEqual({key: job["env"][key] for key in env}, env)
                self.assertEqual(job.get("continue-on-error", False), name == "gateway-canary")
        scale = next(step for step in jobs["scale"]["steps"] if step.get("name") == "Compare scale history")
        self.assertEqual(scale["env"]["SCALE_JOB_NAME"], "${{ inputs.verification_job_prefix }}scale")
        self.assertEqual(jobs["scale"]["env"]["PILOT_PROFILE_SCALE"], "1")

    def test_e2e_has_every_shard_of_every_supported_version(self):
        groups = collections.defaultdict(set)
        for job in self.workflow["jobs"].values():
            name = job["name"]
            if name.startswith(("e2e-hp", "e2e-minimum-kubernetes-")):
                self.assertEqual(int(job["env"]["E2E_SHARD_TOTAL"]), 4)
                group, index = name.rsplit("-", 1)
                self.assertEqual(int(job["env"]["E2E_SHARD_INDEX"]), int(index))
                groups[group].add(int(index))
        self.assertEqual(set(groups), {"e2e-hp3.0", "e2e-hp3.2", "e2e-hp3.3", "e2e-hp3.4",
                                       "e2e-minimum-kubernetes"})
        for group, indexes in groups.items():
            with self.subTest(group=group):
                self.assertEqual(indexes, {1, 2, 3, 4})

    def test_unit_shards_cover_command_and_renderer_with_one_cache_writer_each(self):
        groups = collections.defaultdict(list)
        for job in self.workflow["jobs"].values():
            suite = json.loads(job.get("env", {}).get("CI_SUITE", "{}"))
            if suite.get("suite") != "unit-shard" or suite["shard"] == "rest":
                continue
            groups[suite["shard"]].append(int(suite["index"]))
            self.assertEqual(job["env"]["SHARD_INDEX"], suite["index"])
            cache_steps = {step.get("name"): step for step in job["steps"]}
            writer = cache_steps["Reuse Go downloads and compilation"]
            reader = cache_steps["Restore shared unit compilation"]
            self.assertEqual(writer["with"]["key"], reader["with"]["key"])
            self.assertIn("fromJSON(env.CI_SUITE).index == 1", writer["if"])
            self.assertIn("fromJSON(env.CI_SUITE).index != 1", reader["if"])
        self.assertEqual(set(groups), {"command", "renderer"})
        for indexes in groups.values():
            self.assertEqual(sorted(indexes), [1, 2, 3])


if __name__ == "__main__":
    unittest.main()
