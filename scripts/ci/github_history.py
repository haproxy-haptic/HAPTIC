"""Fetch metrics from successful jobs in one GitHub workflow."""

import datetime
import hashlib
import io
import itertools
import json
import urllib.error
import urllib.parse
import zipfile

from .github import jobs_for_run


def successful_job_metrics(api, repository, current_run, *, workflow, branch, job_name,
                           artifact_name, metrics_name, max_runs, max_workflows,
                           events=("schedule", "workflow_dispatch")):
    path = f"repos/{repository}/actions/workflows/{urllib.parse.quote(workflow, safe='')}/runs"
    history = []
    for run in itertools.islice(api.all(path, "workflow_runs", branch=branch), max_workflows):
        if int(run["id"]) >= int(current_run) or run["head_branch"] != branch:
            continue
        if run["event"] not in events:
            continue
        if run["event"] == "push" and (
                (run.get("head_repository") or {}).get("full_name") != repository or
                run.get("repository", {}).get("full_name") != repository):
            continue
        if run["status"] != "completed":
            continue
        jobs = [job for job in jobs_for_run(api, repository, run) if job["name"] == job_name]
        if not jobs or jobs[0]["status"] != "completed" or jobs[0]["conclusion"] != "success":
            continue
        artifacts = [artifact for artifact in api.all(
            f"repos/{repository}/actions/runs/{run['id']}/artifacts", "artifacts")
            if artifact["name"] == artifact_name and not artifact["expired"]]
        if not artifacts:
            continue
        started = datetime.datetime.fromisoformat(jobs[0]["started_at"])
        completed = datetime.datetime.fromisoformat(jobs[0]["completed_at"])
        # A rerun job keeps every attempt's artifact under one name; only the successful attempt's upload counts.
        artifacts = [artifact for artifact in artifacts
                     if started <= datetime.datetime.fromisoformat(artifact["created_at"]) <= completed]
        if not artifacts:
            raise ValueError("Metrics artifact was not uploaded during the successful job")
        if len(artifacts) != 1:
            raise ValueError("Workflow metrics artifact is ambiguous")
        artifact = artifacts[0]
        source = artifact["workflow_run"]
        if source["id"] != run["id"] or source["head_sha"] != run["head_sha"] or source["head_branch"] != branch:
            raise ValueError("Metrics artifact does not match the workflow revision")
        try:
            content = api.artifact(repository, artifact["id"], 2 * 1024 * 1024)
        except urllib.error.HTTPError as error:
            error.close()
            if error.code in {404, 410}:
                continue
            raise
        if artifact["digest"] != "sha256:" + hashlib.sha256(content).hexdigest():
            raise ValueError("Workflow metrics artifact digest does not match GitHub's record")
        with zipfile.ZipFile(io.BytesIO(content)) as archive:
            if archive.namelist() != [metrics_name] or archive.getinfo(metrics_name).file_size > 1024 * 1024:
                raise ValueError("Workflow metrics artifact has unexpected contents")
            metrics = json.loads(archive.read(metrics_name))
        metrics["_job_id"] = jobs[0]["id"]
        history.append(metrics)
        if len(history) == max_runs:
            break
    return history
