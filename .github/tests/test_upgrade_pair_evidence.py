import contextlib
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch


SPEC = importlib.util.spec_from_file_location(
    "upgrade_pair_evidence",
    Path(__file__).parents[1] / "scripts" / "upgrade-pair-evidence.py",
)
EVIDENCE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(EVIDENCE)


class UpgradePairEvidenceTest(unittest.TestCase):
    def setUp(self):
        temporary = self.enterContext(tempfile.TemporaryDirectory())
        self.root = Path(temporary)
        self.enterContext(contextlib.chdir(self.root))
        self.enterContext(contextlib.redirect_stdout(io.StringIO()))
        self.directory = self.root / ".pilot" / "baseline"
        self.directory.mkdir(parents=True)
        (self.root / "chart.yaml").write_text("chart")
        identity = {
            "worktree": str(self.root),
            "charts": {"chart.yaml": hashlib.sha256(b"chart").hexdigest()},
        }
        (self.root / ".pilot/pair-inputs.json").write_text(json.dumps([identity, identity]))
        (self.root / ".pilot/initial-containers.txt").write_text("")
        (self.root / ".pilot/runtime-images.json").write_text('[{"Id":"runtime"}]')
        self.outputs = {
            ("docker", "ps", "--all", "--quiet", "--no-trunc"): "",
            ("kind", "get", "clusters"): "",
            ("bash", "scripts/chart-spoa-image.sh"): "spoa:test",
            ("docker", "image", "inspect", "haptic:test", "spoa:test"): '[{"Id":"runtime"}]',
        }
        self.enterContext(patch.object(EVIDENCE, "output", side_effect=lambda args: self.outputs[tuple(args)]))

    def report(self, log, result=1, log_result=0):
        (self.directory / "test.log").write_text(log)
        EVIDENCE.arm_result("baseline", "2026-10-20T00:00:00Z", str(result), str(log_result))
        return json.loads((self.directory / "result.json").read_text())

    def complete_capture(self):
        capture = self.root / "debug-logs/baseline/run/upgrade-diagnostics"
        capture.mkdir(parents=True)
        (capture / "result.txt").write_text("capture_error=<nil>\n")

    @staticmethod
    def complete_log():
        phases = (
            "released baseline", "upgrade", "rejected upgrade",
            "released rollback", "repair broken live configuration",
        )
        return (
            "".join(f"Upgrade 0.6.0: {phase}\n" for phase in phases)
            + "".join(f"{phase}: 100 consecutive HTTP/HTTPS probe pairs passed\n" for phase in phases[1:])
            + "PASS: upgrade from 0.6.0 preserves traffic, rejects bad releases, "
            "and repairs bad live configuration\n"
        )

    def test_traffic_success_does_not_hide_collector_failure(self):
        report = self.report(
            "Upgrade 0.6.0: released baseline\n"
            "Upgrade 0.6.0: upgrade\n"
            "upgrade: 1456 consecutive HTTP/HTTPS probe pairs passed\n"
            "upgrade: sample upgrade node: exit status 1\n"
        )
        self.assertEqual(report["suite_exit"], 1)
        self.assertEqual(report["phases"]["released baseline"]["verdict"], "passed")
        self.assertEqual(report["phases"]["upgrade"]["verdict"], "failed")
        self.assertEqual(report["phases"]["upgrade"]["traffic_probe_pairs_passed"], 1456)
        self.assertEqual(report["phases"]["rejected upgrade"]["verdict"], "unreached")

    def test_complete_lifecycle_keeps_all_phase_verdicts(self):
        self.complete_capture()
        report = self.report(self.complete_log(), 0)
        self.assertEqual({item["verdict"] for item in report["phases"].values()}, {"passed"})
        self.assertTrue(report["cleanup_verified"])

    def test_truncated_or_reordered_log_cannot_qualify_a_success(self):
        self.complete_capture()
        full = self.complete_log()
        for log in ("", full.replace("100 consecutive", "0 consecutive"), full.split("PASS:")[0],
                    full.replace("Upgrade 0.6.0: upgrade\n", "")):
            with self.subTest(log=log), self.assertRaisesRegex(SystemExit, "lifecycle log is incomplete"):
                self.report(log, 0)

    def test_log_writer_failure_preserves_native_exit(self):
        self.complete_capture()
        with self.assertRaisesRegex(SystemExit, "lifecycle log is incomplete"):
            self.report(self.complete_log(), 0, 1)
        report = json.loads((self.directory / "result.json").read_text())
        self.assertEqual(report["suite_exit"], 0)
        self.assertEqual(report["log_writer_exit"], 1)

    def test_success_without_capture_stops_next_arm(self):
        with self.assertRaisesRegex(SystemExit, "incomplete diagnostic evidence"):
            self.report("", 0)

    def test_remaining_observer_stops_next_arm(self):
        self.outputs[("docker", "ps", "--all", "--quiet", "--no-trunc")] = "observer"
        with self.assertRaisesRegex(SystemExit, "cleanup or input identity"):
            self.report("")
        report = json.loads((self.directory / "result.json").read_text())
        self.assertEqual(report["added_container_ids"], ["observer"])
        self.assertFalse(report["cleanup_verified"])

    def test_changed_runtime_or_chart_stops_next_arm(self):
        for change in ("runtime", "chart"):
            with self.subTest(change=change):
                self.outputs[("docker", "image", "inspect", "haptic:test", "spoa:test")] = (
                    '[{"Id":"changed"}]' if change == "runtime" else '[{"Id":"runtime"}]'
                )
                (self.root / "chart.yaml").write_text("changed" if change == "chart" else "chart")
                with self.assertRaisesRegex(SystemExit, "cleanup or input identity"):
                    self.report("")
