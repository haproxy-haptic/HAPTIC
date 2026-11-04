"""Verify the immutable workflow checkout independently of its website content."""

import argparse
import json
import os
from pathlib import Path
import subprocess
import sys


sys.path.insert(0, str(Path(__file__).resolve().parents[2]))
from scripts.ci.github import GitHub
from scripts.ci.github_evidence import (
    check_evidence, load_evidence, publication_changes, require_unchanged_run,
)


def verify(api, repository, sha, tree, manifest, run_id=None):
    root = f"repos/{repository}/actions"
    if run_id is None:
        candidates = list(api.all(root + "/workflows/ci.yml/runs", "workflow_runs",
                                  branch="main", event="push", head_sha=sha))
        matching = [run for run in candidates if run["head_sha"] == sha]
        if not matching:
            raise ValueError("Website tooling requires canonical CI for its exact workflow commit")
        run_id = max(run["id"] for run in matching)
    run = api.get(root + f"/runs/{run_id}")
    if (run["head_sha"], run["head_branch"], run["event"]) != (sha, "main", "push"):
        raise ValueError("Website tooling requires canonical main push CI for its exact workflow commit")
    jobs, artifact, content = load_evidence(api, repository, run)
    receipt = check_evidence(run, jobs, artifact, content, repository=repository,
                             sha=sha, tree=tree, manifest=manifest, allow_prose=True,
                             comparison=publication_changes)
    require_unchanged_run(api, repository, run)
    return receipt


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run", type=int)
    parser.add_argument("--expected", type=Path)
    args = parser.parse_args()
    sha = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
    tree = subprocess.check_output(["git", "rev-parse", "HEAD^{tree}"], text=True).strip()
    if sha != os.environ["GITHUB_WORKFLOW_SHA"]:
        raise ValueError("Website tooling checkout differs from the workflow commit")
    receipt = verify(GitHub(os.environ["GH_TOKEN"]), os.environ["GITHUB_REPOSITORY"], sha, tree,
                     Path(".github/ci-verification.json").read_bytes(), args.run)
    if args.expected and json.loads(args.expected.read_text()) != receipt:
        raise ValueError("Website tooling verification changed after the site was built")
    output = Path(".publication/verified-tooling.json")
    output.parent.mkdir(exist_ok=True)
    output.write_text(json.dumps(receipt, indent=2) + "\n")
    with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as stream:
        stream.write(f"run={receipt['run_id']}\n")
    print(f"Website tooling verified {len(receipt['jobs'])} jobs for {sha} ({tree})")


if __name__ == "__main__":
    main()
