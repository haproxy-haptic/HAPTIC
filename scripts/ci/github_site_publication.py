"""Prepare and archive a verified website before GitHub Pages deployment."""

import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile

from .github import GitHub
from .github_pages import prepare
from .github_site_archive import archive, download
from .github_site_inputs import download_inputs
from . import github_site_state as storage
from .github_site_versions import assemble, payload, version_key


def source_file(path, sha=None):
    if sha is None:
        return Path(path).read_bytes()
    return subprocess.check_output(["git", "show", f"{sha}:{path}"])


def publication_source(receipt, repository, version, source_sha=None):
    if source_sha is not None and not re.fullmatch(r"[0-9a-f]{40}", source_sha):
        raise ValueError("Website content source must be a full commit SHA")
    sha = source_sha or subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
    tree = subprocess.check_output(["git", "rev-parse", f"{sha}^{{tree}}"], text=True).strip()
    if receipt["sha"] != sha or receipt["tree"] != tree or receipt["repository"] != repository:
        raise ValueError("Website content differs from its CI receipt. Verify the selected content commit.")
    if version == "dev":
        if receipt["branch"] != "main":
            raise ValueError("Development website publications require verified main")
    else:
        version_key(version)
        tag_sha = subprocess.check_output(["git", "rev-parse", "refs/tags/v" + version + "^{commit}"], text=True).strip()
        if receipt["profile"] != "full" or source_file("VERSION", source_sha).decode().strip() != version or tag_sha != sha:
            raise ValueError("Released website requires the matching verified release tag and version")


def require_no_rewind(api, repository, published_commit, sha):
    """The baseline's own dev section is an unpublished preview; the published dev site is the anchor."""
    if not re.fullmatch(r"[0-9a-f]{40}", published_commit):
        raise ValueError("Website baseline lacks the published development commit")
    compared = api.get(f"repos/{repository}/compare/{published_commit}...{sha}")
    if compared["status"] not in {"identical", "ahead"}:
        raise ValueError("First development publication would rewind the published development site")


def build(api, repository, receipt, version, output, evidence, source_sha=None):
    publication_source(receipt, repository, version, source_sha)
    if version != "dev":
        release = api.get(f"repos/{repository}/releases/tags/v{version}")
        if release["draft"] or release["tag_name"] != "v" + version:
            raise ValueError("Release website requires its published GitHub release")
    baseline = json.loads(source_file(".github/pages-baseline.json", source_sha))
    state_head, state = storage.read(api, repository, baseline)
    evidence.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="website-publication-") as temporary:
        scratch = Path(temporary)
        source = scratch / "baseline"
        download(baseline, source)
        if version == "dev" and state["dev"] is None:
            require_no_rewind(api, repository, baseline.get("published_dev_commit", ""), receipt["sha"])
        restored = {}
        entries = {**state["releases"], **({"dev": state["dev"]} if state["dev"] else {})}
        for old_version, entry in entries.items():
            target = scratch / ("existing-" + old_version)
            manifest = download(entry, target)
            if manifest["provenance"] != entry["provenance"]:
                raise ValueError("Stored website version differs from its catalog provenance")
            restored[old_version] = (target, manifest["provenance"])
        inputs, provenance = download_inputs(api, repository, receipt, scratch)
        provenance["version"] = version
        if "wasm" not in inputs:
            previous = restored.get("dev", (source, {"baseline": baseline}))
            inputs["wasm"] = previous[0]
            provenance["playground"] = json.loads((previous[0] / "playground/dev/release-identity.json").read_text())
            provenance["retained_playground"] = (state["dev"] or baseline)["sha256"]
        content = scratch / "content"
        payload(inputs["docs"], inputs["wasm"], content, provenance)
        bundle = evidence / "content.tar.gz"
        result = archive(content, bundle, provenance)
        entry = {key: result[key] for key in ("size", "sha256")}
        entry.update(provenance=provenance,
                     url=f"https://github.com/{repository}/releases/download/{storage.TAG}/website-{version}-{result['sha256']}.tar.gz")
        next_state = storage.advance(api, repository, state, entry)
        restored[version] = (content, provenance)
        publications = [restored[name] for name in (*next_state["releases"], "dev") if name in restored]
        assembled = scratch / "assembled"
        result = assemble(source, publications, assembled, next_state["superseded"])
        packaged = prepare(assembled, output)
        request = {"expected_state_head": state_head, "state": next_state, "entry": entry,
                   "assembly": result, "packaging": packaged}
        (evidence / "request.json").write_text(json.dumps(request, indent=2) + "\n")
        print(json.dumps({"assembly": result, "packaging": packaged}, indent=2))


def retain(api, repository, receipt, evidence, source_sha=None):
    request = json.loads((evidence / "request.json").read_text())
    entry = request["entry"]
    if entry["provenance"]["source"] != receipt:
        raise ValueError("Website source verification changed before retention")
    publication_source(receipt, repository, entry["provenance"]["version"], source_sha)
    current_head, state = storage.read(api, repository, request["state"]["baseline"])
    if current_head != request["expected_state_head"]:
        raise ValueError("Website catalog changed after browser verification; rebuild the publication")
    next_state = storage.advance(api, repository, state, entry)
    if next_state != request["state"]:
        raise ValueError("Website publication request differs from its planned state")
    with tempfile.TemporaryDirectory(prefix="website-retention-") as temporary:
        uploaded = storage.upload(api, repository, evidence / "content.tar.gz", entry["provenance"], Path(temporary))
    if uploaded != entry:
        raise ValueError("Retained website archive differs from its verified publication")
    head = storage.write(api, repository, current_head, next_state)
    (evidence / "retained.json").write_text(json.dumps({"state_head": head, "entry": entry}, indent=2) + "\n")
    print(f"Retained verified website archive and catalog at {head}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("build", "retain"))
    parser.add_argument("--source-sha")
    parser.add_argument("--version", default="dev")
    parser.add_argument("--output", type=Path)
    parser.add_argument("--evidence", type=Path, default=Path(".publication/pages"))
    args = parser.parse_args()
    receipt = json.loads(Path(".publication/verified-source.json").read_text())
    repository = os.environ["GITHUB_REPOSITORY"]
    client = GitHub(os.environ["GH_TOKEN"])
    if args.command == "build":
        if args.output is None:
            parser.error("build requires --output")
        build(client, repository, receipt, args.version, args.output, args.evidence, args.source_sha)
    else:
        retain(client, repository, receipt, args.evidence, args.source_sha)


if __name__ == "__main__":
    main()
