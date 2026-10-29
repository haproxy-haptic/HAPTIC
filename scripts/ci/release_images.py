"""Resolve signed, multi-platform controller indexes for a staged release chart."""

import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess


PLATFORMS = {("linux", "amd64", ""), ("linux", "arm64", ""), ("linux", "arm", "v7")}
INDEX_TYPES = {"application/vnd.oci.image.index.v1+json",
               "application/vnd.docker.distribution.manifest.list.v2+json"}


def index_digest(raw):
    index = json.loads(raw)
    if index.get("schemaVersion") != 2 or index.get("mediaType") not in INDEX_TYPES:
        raise ValueError("Controller reference must be a multi-platform image index")
    platforms = set()
    children = set()
    attestations = []
    for child in index["manifests"]:
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", child["digest"]):
            raise ValueError("Controller index contains an invalid manifest digest")
        platform = child["platform"]
        key = (platform["os"], platform["architecture"], platform.get("variant", ""))
        if key == ("unknown", "unknown", ""):
            annotations = child.get("annotations", {})
            if annotations.get("vnd.docker.reference.type") != "attestation-manifest":
                raise ValueError("Controller index contains an unknown platform")
            attestations.append(annotations.get("vnd.docker.reference.digest"))
            continue
        if key in platforms:
            raise ValueError("Controller index contains a duplicate platform")
        platforms.add(key)
        children.add(child["digest"])
    if platforms != PLATFORMS or any(digest not in children for digest in attestations):
        raise ValueError("Controller index must contain Linux amd64, arm64, and arm/v7 images")
    return "sha256:" + hashlib.sha256(raw).hexdigest()


def resolve_images(repository, version, series, identity, run=subprocess.check_output):
    if not series or len(set(series)) != len(series):
        raise ValueError("Supported HAProxy series must be nonempty and unique")
    digests = {}
    for variant in series:
        raw = run(["docker", "buildx", "imagetools", "inspect",
                   f"{repository}:{version}-haproxy{variant}", "--raw"])
        digest = index_digest(raw)
        run(["cosign", "verify", "--certificate-identity", identity,
             "--certificate-oidc-issuer", "https://token.actions.githubusercontent.com",
             f"{repository}@{digest}"])
        digests[variant] = digest
    return {"version": version, "digests": digests}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repository", required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--series", nargs="+", required=True)
    parser.add_argument("--identity", required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    metadata = resolve_images(args.repository, args.version, args.series, args.identity)
    args.output.write_text(json.dumps(metadata, indent=2, sort_keys=True) + "\n")


if __name__ == "__main__":
    main()
