"""Prepare a complete, checksum-verified site for GitHub Pages."""

import argparse
import hashlib
import html
import json
from pathlib import Path
import posixpath
import re
import shutil
import subprocess
import tempfile


MAX_BYTES = 1_000_000_000
FETCH_HASH = "50fef1ca5327b86f1bd59edb794f2b54653e8391df443dc5f3c898fd3d88264e"
ENGINE = re.compile(r"playground/(dev|\d+\.\d+\.\d+(?:-(?:alpha|beta|rc)\.\d+)?)/playground\.wasm")
ASSETS = Path(__file__).parents[1] / "pages"
RECEIPT = "shared/pages-transport.json"
PRERELEASE_REDIRECT = re.compile(r"/(docs|playground)/((\d+\.\d+\.\d+)-(?:alpha|beta|rc)\.\d+)/\* /\1/\3/:splat 301")


def record(path):
    with path.open("rb") as source:
        checksum = hashlib.file_digest(source, "sha256").hexdigest()
    return {"size": path.stat().st_size, "sha256": checksum}


def inventory(root):
    root = root.resolve(strict=True)
    files = {}

    def visit(directory, ancestors):
        target = directory.resolve(strict=True)
        if not target.is_relative_to(root) or target in ancestors:
            raise ValueError(f"Site directory escapes its root or forms a cycle: {directory}")
        for path in sorted(directory.iterdir()):
            if not path.resolve(strict=True).is_relative_to(root):
                raise ValueError(f"Site file escapes its root: {path}")
            if path.is_dir():
                visit(path, ancestors | {target})
            elif path.is_file():
                files[path.relative_to(root).as_posix()] = record(path)
            else:
                raise ValueError(f"Unsupported site entry: {path}")

    visit(root, set())
    return files


def worker_transport(source, expected):
    matches = list(re.finditer(r"async function fetchWasmWithProgress\(\) \{.*?\n\}", source, re.S))
    if len(matches) != 1 or hashlib.sha256(matches[0][0].encode()).hexdigest() != FETCH_HASH:
        raise ValueError("Unknown playground worker download function; review its transport before publishing")
    revision = record(ASSETS / "playground-wasm-loader.mjs")["sha256"][:16]
    replacement = (
        "async function fetchWasmWithProgress() {\n"
        f"  const {{ fetchBrotliWasm }} = await import('../../shared/playground-wasm-loader.mjs?revision={revision}');\n"
        "  return fetchBrotliWasm(new URL('./playground.wasm.br', self.location.href),\n"
        f"    {json.dumps(expected, separators=(',', ':'))}, event => postMessage(event));\n"
        "}"
    )
    return source[:matches[0].start()] + replacement + source[matches[0].end():]


def redirect_page(destination):
    target = html.escape(destination, quote=True)
    script_target = json.dumps(destination).replace("<", "\\u003c")
    return (f'<!doctype html><html lang="en"><head><meta charset="utf-8">'
            f'<noscript><meta http-equiv="refresh" content="0;url={target}"></noscript>'
            f'<link rel="canonical" href="{target}"><title>Page moved</title>'
            f'<script>location.replace({script_target}+location.search+location.hash)</script>'
            f'</head><body><a href="{target}">Open page</a></body></html>\n')


def materialize_redirects(root):
    def write(target, destination):
        relative = posixpath.relpath(destination.lstrip("/"), target.parent.relative_to(root).as_posix())
        if destination.endswith("/"):
            relative += "/"
        target.write_text(redirect_page(relative))

    path = root / "_redirects"
    rules = path.read_text().splitlines() if path.exists() else []
    prereleases = {}
    for line in rules:
        if not line.strip() or line.startswith("#"):
            continue
        if redirect := PRERELEASE_REDIRECT.fullmatch(line):
            area, old, new = redirect.groups()
            final = root / area / new
            if (root / area / old).exists() or not final.is_dir():
                raise ValueError(f"Pre-release redirect must replace a removed version with its final release: {line}")
            for page in sorted(page for page in final.rglob("*.html") if page.is_file()):
                destination = f"/{area}/{new}/{page.relative_to(final).as_posix()}"
                target = root / area / old / page.relative_to(final)
                target.parent.mkdir(parents=True, exist_ok=True)
                write(target, destination.removesuffix("index.html") if page.name == "index.html" else destination)
            prereleases[f"{area}/{old}/"] = f"{area}/{new}/"
            continue
        match = re.fullmatch(r"/(controller|helm-chart)(/latest/\*|/latest|/) /docs(/latest/:splat|/latest/|/) 301", line)
        if not match:
            raise ValueError(f"Unsupported Pages redirect: {line}")
        prefix, old, new = match.groups()
        if old == "/latest/*" and new == "/latest/:splat":
            for source in sorted((root / "docs/latest").rglob("*")):
                if not source.is_file():
                    continue
                relative = source.relative_to(root / "docs/latest")
                target = root / prefix / "latest" / relative
                target.parent.mkdir(parents=True, exist_ok=True)
                if source.suffix == ".html":
                    write(target, "/docs/latest/" + relative.as_posix())
                else:
                    shutil.copyfile(source, target)
        elif (old, new) in {("/latest", "/latest/"), ("/", "/")}:
            target = root / prefix / old.strip("/") / "index.html"
            target.parent.mkdir(parents=True, exist_ok=True)
            write(target, "/docs/latest/")
        else:
            raise ValueError(f"Unsupported Pages redirect: {line}")
    if (root / "docs/latest/index.html").is_file() and not (root / "docs/index.html").exists():
        write(root / "docs/index.html", "/docs/latest/")
    if path.exists():
        path.unlink()
    return prereleases


def prepare(source, destination, *, limit=MAX_BYTES):
    source = source.resolve(strict=True)
    destination = destination.resolve()
    if destination.exists() or destination.is_relative_to(source):
        raise ValueError("Pages output must be a new directory outside the source site")
    before = inventory(source)
    engines = {name: entry for name, entry in before.items() if ENGINE.fullmatch(name)}
    if not engines or RECEIPT in before:
        raise ValueError("Pages input must contain the original playground engines")
    vendor = json.loads((ASSETS / "brotli-dec-wasm-2.3.2/provenance.json").read_text())
    for name, expected in vendor["files"].items():
        if record(ASSETS / "brotli-dec-wasm-2.3.2" / name) != expected:
            raise ValueError(f"Brotli decoder checksum mismatch: {name}")
    removed = {name + suffix for name in engines for suffix in ("", ".gz")}
    removed &= before.keys()
    changed = {}
    destination.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(dir=destination.parent, prefix=".pages-") as temporary:
        output = Path(temporary) / "public"
        output.mkdir()
        for name in before.keys() - removed:
            target = output / name
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(source / name, target)
        for name, decoded in engines.items():
            compressed = name + ".br"
            if compressed not in before:
                raise ValueError(f"Missing Brotli playground engine: {compressed}")
            restored = Path(temporary) / "decoded.wasm"
            with restored.open("wb") as stream:
                subprocess.run(["brotli", "--decompress", "--stdout", str(source / compressed)], stdout=stream, check=True)
            if record(restored) != decoded:
                raise ValueError(f"Brotli engine differs from its original: {name}")
            worker = str(Path(name).with_name("playground.worker.js"))
            expected = {"compressed": before[compressed], "decoded": decoded}
            patched = worker_transport((source / worker).read_text(), expected)
            (output / worker).write_text(patched)
            changed[worker] = {"original": before[worker], "published": record(output / worker)}
            index = str(Path(name).with_name("index.html"))
            original = (source / index).read_text()
            declaration = "new Worker('./playground.worker.js')"
            if original.count(declaration) != 1:
                raise ValueError(f"Unknown playground worker reference: {index}")
            revision = changed[worker]["published"]["sha256"][:16]
            (output / index).write_text(original.replace(declaration, f"new Worker('./playground.worker.js?transport={revision}')"))
            changed[index] = {"original": before[index], "published": record(output / index)}
        shared = output / "shared"
        shared.mkdir(exist_ok=True)
        shutil.copyfile(ASSETS / "playground-wasm-loader.mjs", shared / "playground-wasm-loader.mjs")
        shutil.copytree(ASSETS / "brotli-dec-wasm-2.3.2", shared / "brotli-dec-wasm-2.3.2")
        prerelease_redirects = materialize_redirects(output)
        (output / ".nojekyll").touch()
        after = inventory(output)
        for name in before.keys() - removed - changed.keys() - {"_redirects"}:
            if after.get(name) != before[name]:
                raise ValueError(f"Pages packaging changed a retained file: {name}")
        receipt = {"version": 1, "codec": "brotli", "before_bytes": sum(e["size"] for e in before.values()),
                   "source_files": before, "removed_transport_copies": sorted(removed), "changed_transport_files": changed,
                   "published_files": after, "engines": engines, "limit_bytes": limit,
                   "prerelease_redirects": prerelease_redirects}
        (output / RECEIPT).write_text(json.dumps(receipt, indent=2, sort_keys=True) + "\n")
        size = sum(e["size"] for e in inventory(output).values())
        if size > limit:
            raise ValueError(f"Pages site is {size} bytes; limit is {limit}")
        output.rename(destination)
    return {"before_bytes": receipt["before_bytes"], "published_bytes": size,
            "original_files": len(before), "engine_versions": len(engines), "limit_bytes": limit}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    print(json.dumps(prepare(args.source, args.output), indent=2))


if __name__ == "__main__":
    main()
