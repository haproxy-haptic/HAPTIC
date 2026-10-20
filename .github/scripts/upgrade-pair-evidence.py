"""Retain input identity and fail closed on cleanup between diagnostic arms."""

import hashlib
import json
from datetime import datetime, timezone
from pathlib import Path
import re
import subprocess
import sys


def output(args, cwd=None):
    return subprocess.check_output(args, cwd=cwd, text=True).strip()


def inputs(baseline, candidate):
    reports = []
    for repository in (Path(baseline), Path(candidate)):
        paths = output(["git", "ls-files", "charts"], repository).splitlines()
        reports.append({
            "worktree": str(repository),
            "head": output(["git", "rev-parse", "HEAD"], repository),
            "product_source_hash": output(["scripts/source-hash.sh"], repository),
            "charts": {
                path: hashlib.sha256((repository / path).read_bytes()).hexdigest()
                for path in paths
            },
        })
    Path(".pilot/pair-inputs.json").write_text(json.dumps(reports, indent=2) + "\n")
    if (reports[0]["product_source_hash"] != reports[1]["product_source_hash"]
            or reports[0]["charts"] != reports[1]["charts"]):
        raise SystemExit("Paired arms have different product or chart inputs")


def arm_result(arm, started, result, log_result):
    directory = Path(".pilot") / arm
    log = (directory / "test.log").read_text()
    phases = [
        "released baseline", "upgrade", "rejected upgrade",
        "released rollback", "repair broken live configuration",
    ]
    traffic_records = re.findall(
        r"^(.+?): (\d+) consecutive HTTP/HTTPS probe pairs passed$", log, re.M,
    )
    passed = dict(traffic_records)
    markers = re.findall(r"^Upgrade ([^:]+): (.+)$", log, re.M)
    expected_markers = [(markers[0][0], phase) for phase in phases] if markers else []
    complete_log = (
        bool(markers) and markers == expected_markers
        and len(traffic_records) == len(phases) - 1
        and set(passed) == set(phases[1:])
        and all(int(count) > 0 for count in passed.values())
        and re.search(
            rf"^PASS: upgrade from {re.escape(markers[0][0])} preserves traffic, "
            r"rejects bad releases, and repairs bad live configuration$",
            log, re.M,
        ) is not None
    )
    initial = set(Path(".pilot/initial-containers.txt").read_text().splitlines())
    remaining = output(["docker", "ps", "--all", "--quiet", "--no-trunc"]).splitlines()
    clusters = output(["kind", "get", "clusters"]).splitlines()
    images = json.loads(output([
        "docker", "image", "inspect", "haptic:test",
        output(["bash", "scripts/chart-spoa-image.sh"]),
    ]))
    expected_images = json.loads(Path(".pilot/runtime-images.json").read_text())
    image_ids = [image["Id"] for image in images]
    expected_ids = [image["Id"] for image in expected_images]
    captures = {
        str(path): path.read_text()
        for path in (Path("debug-logs") / arm).rglob("upgrade-diagnostics/result.txt")
    }
    reached = {
        phase: re.search(
            rf"^Upgrade [^:]+: {re.escape(phase)}$", log, re.M,
        ) is not None
        for phase in phases
    }
    finished = "PASS: upgrade from " in log
    verdicts = {}
    for index, phase in enumerate(phases):
        completed = (
            reached[phases[index + 1]] if index + 1 < len(phases) else finished
        )
        verdicts[phase] = {
            "verdict": "passed" if completed else "failed" if reached[phase] else "unreached",
            "traffic_probe_pairs_passed": int(passed[phase]) if phase in passed else None,
        }
    report = {
        "state": "finished",
        "started": started,
        "finished": datetime.now(timezone.utc).isoformat(),
        "suite_exit": int(result),
        "log_writer_exit": int(log_result),
        "lifecycle_log_complete": complete_log,
        "phases": verdicts,
        "capture_results": captures,
        "capture_complete": (
            len(captures) == 1
            and all(value.strip() == "capture_error=<nil>" for value in captures.values())
        ),
        "remaining_kind_clusters": clusters,
        "added_container_ids": sorted(set(remaining) - initial),
        "missing_initial_container_ids": sorted(initial - set(remaining)),
        "runtime_image_ids": image_ids,
        "runtime_images_unchanged": image_ids == expected_ids,
        "cleanup_verified": set(remaining) == initial and not clusters,
        "canonical": False,
    }
    identity = json.loads(Path(".pilot/pair-inputs.json").read_text())
    selected = identity[0 if arm == "baseline" else 1]
    repository = Path(selected["worktree"])
    report["chart_inputs_unchanged"] = all(
        hashlib.sha256((repository / path).read_bytes()).hexdigest() == digest
        for path, digest in selected["charts"].items()
    )
    (directory / "result.json").write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report, indent=2))
    if (not report["cleanup_verified"] or not report["runtime_images_unchanged"]
            or not report["chart_inputs_unchanged"]):
        raise SystemExit("Paired run stopped: cleanup or input identity changed")
    if int(result) == 0 and not report["capture_complete"]:
        raise SystemExit("Successful suite has incomplete diagnostic evidence")
    if int(log_result) != 0 or int(result) == 0 and not complete_log:
        raise SystemExit("Paired run stopped: lifecycle log is incomplete")


if __name__ == "__main__":
    if sys.argv[1] == "inputs":
        inputs(*sys.argv[2:])
    elif sys.argv[1] == "arm":
        arm_result(*sys.argv[2:])
    else:
        raise SystemExit("Expected inputs or arm")
