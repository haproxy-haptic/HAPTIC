"""Assemble versioned documentation and playground artifacts without losing releases."""

import argparse
import json
from pathlib import Path, PurePosixPath
import re
import shutil
import tarfile
import tempfile

from .github_pages import inventory, redirect_page
from .github_site_archive import restore


VERSION = re.compile(r"(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-(alpha|beta|rc)\.(0|[1-9]\d*))?")
RESERVED = {"docs", "playground", "controller", "helm-chart", "_redirects", ".nojekyll"}
STAGES = {"alpha": 0, "beta": 1, "rc": 2, None: 3}


def version_key(value):
    match = VERSION.fullmatch(value)
    if not match:
        raise ValueError(f"Invalid website version: {value}")
    major, minor, patch, stage, number = match.groups()
    return (int(major), int(minor), int(patch), STAGES[stage], int(number or 0))


def superseded_prereleases(versions):
    """Pre-releases whose final release is also in versions."""
    keys = {version: version_key(version) for version in versions}
    finals = {key[:3] for key in keys.values() if key[3] == STAGES[None]}
    return {version for version, key in keys.items() if key[3] != STAGES[None] and key[:3] in finals}


def extract(source, destination, *, limit=3_000_000_000):
    if destination.exists():
        raise ValueError("Artifact extraction requires a new directory")
    destination.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(dir=destination.parent, prefix=".website-input-") as temporary:
        output = Path(temporary) / "files"
        output.mkdir()
        names, size = set(), 0
        with tarfile.open(source, "r:gz") as bundle:
            for member in bundle:
                name = member.name.removeprefix("./")
                path = PurePosixPath(name)
                if member.isdir() and name in {"", "."}:
                    continue
                if (path.is_absolute() or ".." in path.parts or not path.parts or
                        name.rstrip("/") != path.as_posix() or path.as_posix() in names or
                        not (member.isfile() or member.isdir())):
                    raise ValueError("Website artifact contains an unsafe or duplicate path")
                names.add(path.as_posix())
                size += member.size
                if size > limit:
                    raise ValueError("Website artifact exceeds the extraction limit")
                target = output.joinpath(*path.parts)
                if member.isdir():
                    target.mkdir(parents=True, exist_ok=True)
                else:
                    target.parent.mkdir(parents=True, exist_ok=True)
                    with bundle.extractfile(member) as content, target.open("xb") as stream:
                        shutil.copyfileobj(content, stream)
        output.rename(destination)


def check_identity(directory, expected):
    actual = json.loads((directory / "release-identity.json").read_text())
    if actual != expected or not (directory / "index.html").is_file():
        raise ValueError(f"Website artifact identity differs from its verified source: {directory.name}")


def payload(docs, playground, destination, provenance):
    """Keep only this run's content; historical preview files are not publication inputs."""
    if (destination.exists() or any(destination.resolve().is_relative_to(root.resolve())
                                   for root in (docs, playground) if root is not None)):
        raise ValueError("Website payload requires a new directory outside its inputs")
    check_identity(docs / "docs/dev", provenance["docs"])
    if playground is not None:
        check_identity(playground / "playground/dev", provenance["playground"])
    version = provenance["version"]
    if version != "dev":
        version_key(version)
        if playground is None:
            raise ValueError("A released website requires its verified playground")
    if not (docs / "index.html").is_file():
        raise ValueError("Website artifact is missing the landing page")
    destination.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(dir=destination.parent, prefix=".website-payload-") as temporary:
        output = Path(temporary) / "public"
        output.mkdir()
        # These inputs have already been safely extracted; reject links added afterwards.
        for root in (docs, playground):
            if root is not None and any(p.is_symlink() for p in root.rglob("*")):
                raise ValueError("Website publication inputs must not contain links")
        for source in docs.iterdir():
            if source.name not in RESERVED:
                if source.is_dir():
                    shutil.copytree(source, output / source.name)
                else:
                    shutil.copyfile(source, output / source.name)
        for area, root, identity in (("docs", docs, provenance["docs"]),
                                    ("playground", playground, provenance.get("playground"))):
            if root is None:
                continue
            target = output / area / version
            shutil.copytree(root / area / "dev", target)
            if version != "dev":
                (target / "build-identity.json").write_text(json.dumps(identity, indent=2) + "\n")
                (target / "release-identity.json").write_text(json.dumps({**identity, "version": version}, indent=2) + "\n")
                if area == "playground":
                    page = (target / "index.html").read_text()
                    if page.count('data-version="dev"') != 1:
                        raise ValueError("Unknown playground version declaration")
                    (target / "index.html").write_text(page.replace('data-version="dev"', f'data-version="{version}"'))
        output.rename(destination)


def replace_directory(source, destination):
    if destination.exists():
        if destination.is_symlink() or not destination.is_dir():
            raise ValueError(f"Website directory has an unexpected type: {destination}")
        shutil.rmtree(destination)
    shutil.copytree(source, destination)


def apply_payload(site, content, version, *, landing):
    allowed = {"docs", "playground"}
    for area in allowed:
        source = content / area
        if not source.exists():
            if area == "docs" or version != "dev":
                raise ValueError(f"Website payload is missing {area}/{version}")
            continue
        if {path.name for path in source.iterdir()} != {version}:
            raise ValueError("Website payload contains another version")
        target = site / area / version
        if target.exists() and version != "dev":
            if inventory(source / version) != inventory(target):
                raise ValueError(f"Released website content cannot be overwritten: {area}/{version}")
        else:
            replace_directory(source / version, target)
    if landing:
        for source in content.iterdir():
            if source.name in RESERVED:
                continue
            target = site / source.name
            if source.is_dir():
                replace_directory(source, target)
            else:
                shutil.copyfile(source, target)


def catalogs(site):
    for area in ("docs", "playground"):
        root = site / area
        versions = sorted((p.name for p in root.iterdir() if p.is_dir() and VERSION.fullmatch(p.name)),
                          key=version_key, reverse=True)
        if not versions:
            raise ValueError(f"Website is missing released {area}")
        stable = [version for version in versions if "-" not in version]
        current = (stable or versions)[0]
        available = versions + (["dev"] if (root / "dev/index.html").is_file() else [])
        if area == "docs":
            data = [{"version": version, "title": version,
                     "aliases": ["latest"] if version == current else []} for version in available]
            replace_directory(root / current, root / "latest")
            (root / "index.html").write_text(redirect_page("latest/"))
            for legacy in ("controller", "helm-chart"):
                previous = site / legacy / "latest"
                if previous.exists():
                    if previous.is_symlink() or not previous.is_dir():
                        raise ValueError("Legacy documentation alias has an unexpected type")
                    shutil.rmtree(previous)
        else:
            data = {"versions": [{"version": version, "stable": version != "dev" and "-" not in version}
                                  for version in available], "current": current}
            (root / "index.html").write_text(redirect_page(current + "/"))
        (root / "versions.json").write_text(json.dumps(data, indent=2) + "\n")


def retire_prereleases(site, superseded, publications):
    present = {path.name for area in ("docs", "playground") for path in (site / area).iterdir()
               if path.is_dir() and VERSION.fullmatch(path.name)}
    retired = superseded_prereleases(present | set(superseded))
    if not retired.issuperset(superseded):
        raise ValueError("Superseded website versions must be pre-releases of a published final release")
    if retired & publications:
        raise ValueError(f"Website publication is superseded by its final release: {sorted(retired & publications)}")
    rules = []
    for version in sorted(retired, key=version_key):
        final = version.partition("-")[0]
        for area in ("docs", "playground"):
            if (site / area / version).exists():
                shutil.rmtree(site / area / version)
            if (site / area / final).is_dir():
                rules.append(f"/{area}/{version}/* /{area}/{final}/:splat 301")
    if rules:
        path = site / "_redirects"
        lines = path.read_text().splitlines() if path.exists() else []
        path.write_text("\n".join(lines + rules) + "\n")
    return retired


def assemble(baseline, publications, destination, superseded=()):
    """superseded: pre-releases an earlier final publication removed; their URLs keep redirecting."""
    if destination.exists() or destination.resolve().is_relative_to(baseline.resolve()):
        raise ValueError("Website assembly requires a new directory outside the baseline")
    before = inventory(baseline)
    destination.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(dir=destination.parent, prefix=".website-assembly-") as temporary:
        output = Path(temporary) / "public"
        shutil.copytree(baseline, output)
        versions = set()
        for content, provenance in publications:
            version = provenance["version"]
            if version in versions or (version != "dev" and not VERSION.fullmatch(version)):
                raise ValueError("Website publication versions must be valid and unique")
            versions.add(version)
            apply_payload(output, content, version, landing=version == "dev")
        retired = retire_prereleases(output, superseded, versions)
        catalogs(output)
        after = inventory(output)
        for name, expected in before.items():
            parts = PurePosixPath(name).parts
            if len(parts) >= 3 and parts[0] in {"docs", "playground"} and VERSION.fullmatch(parts[1]):
                if after.get(name) != (None if parts[1] in retired else expected):
                    raise ValueError(f"Website assembly changed an archived release: {name}")
        output.rename(destination)
    return {"files": len(after), "bytes": sum(item["size"] for item in after.values()),
            "publications": sorted(versions), "superseded": sorted(retired, key=version_key)}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--baseline", type=Path, required=True)
    parser.add_argument("--catalog", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    catalog = json.loads(args.catalog.read_text())
    if catalog.get("version") != 1:
        raise ValueError("Unsupported website publication catalog")
    with tempfile.TemporaryDirectory(prefix="website-versions-") as temporary:
        publications = []
        for index, entry in enumerate(catalog["publications"]):
            output = Path(temporary) / str(index)
            manifest = restore(Path(entry["archive"]), output, entry)
            if manifest["provenance"] != entry["provenance"]:
                raise ValueError("Website archive provenance differs from its catalog")
            publications.append((output, manifest["provenance"]))
        print(json.dumps(assemble(args.baseline, publications, args.output, catalog.get("superseded", ())), indent=2))


if __name__ == "__main__":
    main()
