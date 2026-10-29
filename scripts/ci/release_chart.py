"""Qualify distributed release metadata and the embedded chart file overlay."""

import argparse
import json
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile

import yaml


ROLES = {"controller", "agent", "apply-crds", "preflight"}


def render(chart, values):
    raw = subprocess.check_output(["helm", "template", "haptic", str(chart), "--namespace", "haptic",
                                   "--api-versions", "gateway.networking.k8s.io/v1/GatewayClass",
                                   "--values", str(values)], text=True)
    return [doc for doc in yaml.safe_load_all(raw) if doc]


def verify_roles(documents, expected):
    roles = set()
    for doc in documents:
        if doc["kind"] not in {"Deployment", "Job"}:
            continue
        pod = doc["spec"]["template"]["spec"]
        for container in pod.get("containers", []) + pod.get("initContainers", []):
            name = container["name"]
            if name in ROLES:
                if container["image"] != expected:
                    raise ValueError(f"Release {name} image differs from {expected}")
                roles.add(name)
    if roles != ROLES:
        raise ValueError("Release render is missing controller, agent, or lifecycle hook images")


def verify_chart(chart, embedded):
    with tempfile.TemporaryDirectory() as temporary:
        directory = Path(temporary)
        if chart.is_file():
            with tarfile.open(chart) as archive:
                archive.extractall(directory / "package", filter="data")
            chart = directory / "package" / "haptic"
        verify_chart_directory(chart, embedded, directory)


def verify_chart_directory(chart, embedded, directory):
    metadata_bytes = (chart / "release-images.json").read_text()
    metadata = json.loads(metadata_bytes)
    repository = yaml.safe_load((chart / "values.yaml").read_text())["controller"]["image"]["repository"]
    replay = directory / "embedded"
    shutil.copytree(embedded, replay)
    if (replay / "release-images.json").exists():
        raise ValueError("The image-embedded source chart must not contain release image metadata")
    (replay / "release-images.json").write_text(metadata_bytes)
    cases = [(series, {}, repository + "@" + digest) for series, digest in metadata["digests"].items()]
    series = next(iter(metadata["digests"]))
    digest = metadata["digests"][series]
    cases.extend([
        (series, {"repository": "mirror.example/haptic"}, "mirror.example/haptic@" + digest),
        (series, {"tag": "custom"}, repository + ":custom-haproxy" + series),
        (series, {"tag": "ignored", "digest": "sha256:" + "e" * 64}, repository + "@sha256:" + "e" * 64),
    ])
    for series, image, expected in cases:
        values = {"haproxyVersion": series,
                  "controller": {"image": image, "webhook": {"caBundle": "dGVzdA=="}},
                  "credentials": {"dataplane": {"password": "release-parity-fixture"}},
                  "defaultSSLCertificate": {"create": True, "cert": "fixture-cert", "key": "fixture-key",
                                            "certManager": {"enabled": False}}}
        values_file = directory / "values.yaml"
        values_file.write_text(yaml.safe_dump(values))
        documents = render(chart, values_file)
        verify_roles(documents, expected)
        secret = next(doc for doc in documents if doc["kind"] == "Secret" and
                      "release-images.json" in doc.get("stringData", {}))
        if secret["stringData"]["release-images.json"] != metadata_bytes:
            raise ValueError("Preflight Secret changed the release image metadata")
        values_file.write_text(secret["stringData"]["values.yaml"])
        if documents != render(replay, values_file):
            raise ValueError("Preflight file overlay does not reproduce the complete release render")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--chart", type=Path, required=True)
    parser.add_argument("--embedded", type=Path, required=True)
    args = parser.parse_args()
    verify_chart(args.chart, args.embedded)


if __name__ == "__main__":
    main()
