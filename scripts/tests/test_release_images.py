import copy
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

from scripts.ci.release_images import INDEX_TYPES, PLATFORMS, index_digest, resolve_images
from scripts.ci.release_chart import verify_chart


class ReleaseImageTests(unittest.TestCase):
    def setUp(self):
        self.index = {"schemaVersion": 2, "mediaType": sorted(INDEX_TYPES)[0], "manifests": [
            {"digest": "sha256:" + str(i) * 64,
             "platform": dict(os=os_name, architecture=arch, **({"variant": variant} if variant else {}))}
            for i, (os_name, arch, variant) in enumerate(sorted(PLATFORMS))]}

    def raw(self, index=None):
        return json.dumps(self.index if index is None else index).encode()

    def test_hashes_the_entire_multiplatform_index(self):
        for media_type in INDEX_TYPES:
            with self.subTest(media_type=media_type):
                self.index["mediaType"] = media_type
                self.assertEqual(index_digest(self.raw()), "sha256:" + hashlib.sha256(self.raw()).hexdigest())

    def test_rejects_missing_duplicate_and_unknown_platforms(self):
        cases = []
        for platform in sorted(PLATFORMS):
            index = copy.deepcopy(self.index)
            index["manifests"] = [child for child in index["manifests"] if
                                  (child["platform"]["os"], child["platform"]["architecture"],
                                   child["platform"].get("variant", "")) != platform]
            cases.append(index)
        for field, value in (("mediaType", "application/vnd.oci.image.manifest.v1+json"),
                             ("schemaVersion", 1)):
            index = copy.deepcopy(self.index)
            index[field] = value
            cases.append(index)
        index = copy.deepcopy(self.index)
        index["manifests"].append(copy.deepcopy(index["manifests"][0]))
        cases.append(index)
        index = copy.deepcopy(self.index)
        index["manifests"][0]["digest"] = "sha256:broken"
        cases.append(index)
        index = copy.deepcopy(self.index)
        index["manifests"][0]["platform"]["architecture"] = "s390x"
        cases.append(index)
        for index in cases:
            with self.subTest(index=index), self.assertRaises(ValueError):
                index_digest(self.raw(index))

    def test_accepts_only_attestations_linked_to_platform_manifests(self):
        attestation = {"platform": {"os": "unknown", "architecture": "unknown"},
                       "digest": "sha256:" + "a" * 64,
                       "annotations": {"vnd.docker.reference.type": "attestation-manifest",
                                       "vnd.docker.reference.digest": self.index["manifests"][0]["digest"]}}
        self.index["manifests"].append(attestation)
        index_digest(self.raw())
        attestation["annotations"]["vnd.docker.reference.digest"] = "sha256:" + "f" * 64
        with self.assertRaises(ValueError):
            index_digest(self.raw())
        attestation.pop("annotations")
        with self.assertRaises(ValueError):
            index_digest(self.raw())

    def test_binds_every_series_to_the_exact_release_signature_identity(self):
        calls = []

        def run(args):
            calls.append(args)
            return self.raw() if args[0] == "docker" else b"verified"

        identity = "https://github.com/owner/repo/.github/workflows/release.yml@refs/tags/v1.2.3"
        series = ["3.0", "3.2", "3.3", "3.4"]
        result = resolve_images("ghcr.io/owner/haptic", "1.2.3", series, identity, run)
        digest = index_digest(self.raw())
        self.assertEqual(result, {"version": "1.2.3", "digests": dict.fromkeys(series, digest)})
        for i, variant in enumerate(series):
            self.assertEqual(calls[2 * i], ["docker", "buildx", "imagetools", "inspect",
                                          f"ghcr.io/owner/haptic:1.2.3-haproxy{variant}", "--raw"])
            self.assertEqual(calls[2 * i + 1], ["cosign", "verify", "--certificate-identity", identity,
                                              "--certificate-oidc-issuer",
                                              "https://token.actions.githubusercontent.com",
                                              "ghcr.io/owner/haptic@" + digest])

    def test_signature_failure_prevents_metadata_return(self):
        def run(args):
            if args[0] == "cosign":
                raise subprocess.CalledProcessError(1, args)
            return self.raw()

        with self.assertRaises(subprocess.CalledProcessError):
            resolve_images("ghcr.io/owner/haptic", "1.2.3", ["3.4"], "release-identity", run)

    def test_rejects_empty_or_duplicate_series(self):
        for series in ([], ["3.4", "3.4"]):
            with self.subTest(series=series), self.assertRaises(ValueError):
                resolve_images("repo", "1.2.3", series, "identity")

    def test_packaged_chart_and_embedded_overlay_render_identically(self):
        root = Path(__file__).resolve().parents[2]
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            chart = directory / "haptic"
            shutil.copytree(root / "charts/haptic", chart)
            metadata = {"version": (root / "VERSION").read_text().strip(),
                        "digests": {series: "sha256:" + str(i) * 64
                                    for i, series in enumerate(("3.0", "3.2", "3.3", "3.4"))}}
            (chart / "release-images.json").write_text(json.dumps(metadata) + "\n")
            subprocess.run(["helm", "package", str(chart), "--destination", str(directory)],
                           check=True, capture_output=True)
            package = directory / ("haptic-" + metadata["version"] + ".tgz")
            verify_chart(package, root / "charts/haptic")
            with (chart / ".helmignore").open("a") as ignore:
                ignore.write("\nrelease-images.json\n")
            subprocess.run(["helm", "package", str(chart), "--destination", str(directory)],
                           check=True, capture_output=True)
            with self.assertRaises(FileNotFoundError):
                verify_chart(package, root / "charts/haptic")
