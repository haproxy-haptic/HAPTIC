import contextlib
import copy
import io
import json
from pathlib import Path
import shutil
import tarfile
import tempfile
import unittest
from unittest.mock import Mock, patch

from scripts.ci import github_site_publication as publication
from scripts.ci.github_pages import inventory
from scripts.ci.github_site_archive import archive, restore
from scripts.ci.github_site_versions import (assemble, catalogs, extract, payload, superseded_prereleases,
                                             version_key)


ROOT = Path(__file__).parents[2]
REPOSITORY = "owner/project"


class SiteVersionsTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.baseline = self.root / "baseline"
        self.docs = self.root / "docs-input"
        self.wasm = self.root / "wasm-input"
        self.identity = {"version": "dev", "commit": "a" * 40, "pipeline": "123", "job": "suite_docs"}
        self.provenance = {"version": "dev", "docs": self.identity,
                           "playground": {**self.identity, "job": "suite_wasm"}}
        self.write(self.baseline / "index.html", "baseline landing")
        for area in ("docs", "playground"):
            self.write(self.baseline / area / "1.0.0/index.html", "released content")
            self.write(self.baseline / area / "dev/index.html", "old dev")
            self.write(self.baseline / area / "latest/index.html", "old latest")
        self.write(self.docs / "index.html", "new landing")
        self.write(self.docs / "shared/script.js", "new shared script")
        self.write(self.docs / "docs/dev/index.html", "new docs")
        self.write(self.docs / "docs/dev/guide/index.html", "new guide")
        self.write(self.docs / "docs/dev/release-identity.json", json.dumps(self.identity))
        self.write(self.docs / "docs/1.0.0/index.html", "outdated preview history must never be used")
        self.write(self.wasm / "playground/dev/index.html",
                   '<html data-version="dev"><script>new Worker(\'./playground.worker.js\')</script></html>')
        self.write(self.wasm / "playground/dev/playground.wasm", "verified engine bytes")
        self.write(self.wasm / "playground/dev/playground.wasm.br", "compressed engine bytes")
        shutil.copyfile(ROOT / "cmd/playground/web/playground.worker.js", self.wasm / "playground/dev/playground.worker.js")
        self.write(self.wasm / "playground/dev/release-identity.json", json.dumps(self.provenance["playground"]))

    def write(self, path, value):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(value)

    def build_payload(self, version="dev", name="payload", playground=True):
        destination = self.root / name
        provenance = {**self.provenance, "version": version}
        payload(self.docs, self.wasm if playground else None, destination, provenance)
        return destination, provenance

    def test_development_payload_updates_only_current_content(self):
        publication = self.build_payload()
        before = inventory(self.baseline)
        output = self.root / "output"
        assemble(self.baseline, [publication], output)
        self.assertEqual((output / "docs/dev/index.html").read_text(), "new docs")
        self.assertEqual((output / "index.html").read_text(), "new landing")
        self.assertEqual((output / "shared/script.js").read_text(), "new shared script")
        self.assertEqual((output / "docs/1.0.0/index.html").read_text(), "released content")
        self.assertEqual(inventory(self.baseline), before)

    def test_multiple_archived_releases_survive_independent_rebuilds(self):
        releases = [self.build_payload(version, name=version) for version in ("1.1.0", "1.2.0-rc.1")]
        restored = []
        for content, provenance in releases:
            bundle = self.root / (content.name + ".tar.gz")
            expected = archive(content, bundle, provenance)
            target = self.root / (content.name + "-restored")
            manifest = restore(bundle, target, expected)
            restored.append((target, manifest["provenance"]))
        for iteration in range(2):
            output = self.root / f"rebuild-{iteration}"
            assemble(self.baseline, restored, output)
            for version in ("1.0.0", "1.1.0", "1.2.0-rc.1"):
                self.assertTrue((output / f"docs/{version}/index.html").is_file())
                self.assertTrue((output / f"playground/{version}/index.html").is_file())
            catalog = json.loads((output / "playground/versions.json").read_text())
            self.assertEqual(catalog["current"], "1.1.0")
            self.assertIn('location.replace("1.1.0/"+location.search+location.hash)',
                          (output / "playground/index.html").read_text())
            docs = json.loads((output / "docs/versions.json").read_text())
            self.assertEqual([v["version"] for v in docs if v["aliases"]], ["1.1.0"])
            self.assertEqual((output / "index.html").read_text(), "baseline landing")
        self.assertEqual(inventory(self.root / "rebuild-0"), inventory(self.root / "rebuild-1"))

    def test_release_stamping_preserves_engine_and_build_provenance(self):
        content, _ = self.build_payload("1.1.0")
        self.assertEqual((content / "playground/1.1.0/playground.wasm").read_bytes(),
                         (self.wasm / "playground/dev/playground.wasm").read_bytes())
        self.assertIn('data-version="1.1.0"', (content / "playground/1.1.0/index.html").read_text())
        self.assertEqual(json.loads((content / "playground/1.1.0/build-identity.json").read_text()),
                         self.provenance["playground"])
        self.assertEqual(json.loads((content / "docs/1.1.0/release-identity.json").read_text()),
                         {**self.identity, "version": "1.1.0"})

    def test_release_overwrite_and_duplicate_versions_fail_atomically(self):
        release = self.build_payload("1.0.0")
        with self.assertRaisesRegex(ValueError, "cannot be overwritten"):
            assemble(self.baseline, [release], self.root / "output")
        self.assertFalse((self.root / "output").exists())
        development = self.build_payload(name="development")
        with self.assertRaisesRegex(ValueError, "unique"):
            assemble(self.baseline, [development, development], self.root / "output")
        self.assertFalse((self.root / "output").exists())

    def test_mismatched_identity_and_missing_engine_fail(self):
        for key in ("version", "commit", "pipeline", "job"):
            with self.subTest(key=key):
                self.write(self.docs / "docs/dev/release-identity.json", json.dumps({**self.identity, key: "wrong"}))
                with self.assertRaisesRegex(ValueError, "identity differs"):
                    self.build_payload()
                self.assertFalse((self.root / "payload").exists())
        self.write(self.docs / "docs/dev/release-identity.json", json.dumps(self.identity))
        with self.assertRaisesRegex(ValueError, "verified playground"):
            self.build_payload("1.1.0", playground=False)

    def test_prose_only_payload_keeps_the_existing_engine(self):
        publication = self.build_payload(playground=False)
        output = self.root / "output"
        assemble(self.baseline, [publication], output)
        self.assertEqual((output / "playground/dev/index.html").read_text(), "old dev")
        self.assertEqual((output / "docs/dev/index.html").read_text(), "new docs")

    def test_older_release_does_not_rewind_latest_and_all_history_is_retained(self):
        self.write(self.baseline / "docs/2.0.0/index.html", "newer docs")
        self.write(self.baseline / "playground/2.0.0/index.html", "newer playground")
        output = self.root / "output"
        assemble(self.baseline, [self.build_payload("1.1.0")], output)
        self.assertEqual((output / "docs/latest/index.html").read_text(), "newer docs")
        self.assertEqual(json.loads((output / "playground/versions.json").read_text())["current"], "2.0.0")
        self.assertIn('location.replace("2.0.0/"', (output / "playground/index.html").read_text())

    def test_stable_is_preferred_to_newer_prerelease_without_deleting_it(self):
        self.write(self.baseline / "docs/2.0.0-rc.1/index.html", "prerelease docs")
        self.write(self.baseline / "playground/2.0.0-rc.1/index.html", "prerelease playground")
        catalogs(self.baseline)
        self.assertEqual(json.loads((self.baseline / "playground/versions.json").read_text())["current"], "1.0.0")
        self.assertTrue((self.baseline / "playground/2.0.0-rc.1/index.html").exists())
        self.assertLess(version_key("1.0.0-rc.10"), version_key("1.0.0"))
        self.assertLess(version_key("1.0.0-rc.2"), version_key("1.0.0-rc.10"))

    def test_superseded_prereleases_compare_semver_cores(self):
        versions = ["0.6.0", "0.6.0-alpha.1", "0.6.0-beta.2", "0.6.0-rc.10", "0.6.1-rc.1", "0.7.0-rc.1",
                    "10.6.0-rc.1", "1.6.0-rc.1", "0.60.0-rc.1", "0.6.10-rc.1"]
        self.assertEqual(superseded_prereleases(versions), {"0.6.0-alpha.1", "0.6.0-beta.2", "0.6.0-rc.10"})
        self.assertEqual(superseded_prereleases(["10.6.0", "0.6.0-rc.1", "1.6.0-rc.1", "0.6.0-rc.2"]), set())
        self.assertEqual(superseded_prereleases(["0.6.0-rc.2", "0.6.0-rc.3"]), set())
        for invalid in ("0.6.0x", "0.06.0", "v0.6.0", "0.6.0-rc1", "dev"):
            with self.subTest(invalid=invalid), self.assertRaisesRegex(ValueError, "Invalid website version"):
                superseded_prereleases(["0.6.0-rc.1", invalid])

    def test_final_release_retires_archived_prereleases_and_redirects_them(self):
        for area in ("docs", "playground"):
            self.write(self.baseline / area / "1.1.0-rc.1/index.html", "archived prerelease")
            self.write(self.baseline / area / "1.2.0-rc.1/index.html", "next prerelease")
        self.write(self.baseline / "_redirects", "/controller/ /docs/ 301")
        output = self.root / "output"
        result = assemble(self.baseline, [self.build_payload("1.1.0")], output, ["1.1.0-beta.1"])
        self.assertEqual(result["superseded"], ["1.1.0-beta.1", "1.1.0-rc.1"])
        for area in ("docs", "playground"):
            self.assertFalse((output / area / "1.1.0-rc.1").exists())
            self.assertEqual((output / area / "1.2.0-rc.1/index.html").read_text(), "next prerelease")
        self.assertEqual((output / "_redirects").read_text().splitlines(), [
            "/controller/ /docs/ 301",
            "/docs/1.1.0-beta.1/* /docs/1.1.0/:splat 301", "/playground/1.1.0-beta.1/* /playground/1.1.0/:splat 301",
            "/docs/1.1.0-rc.1/* /docs/1.1.0/:splat 301", "/playground/1.1.0-rc.1/* /playground/1.1.0/:splat 301"])
        self.assertEqual([entry["version"] for entry in json.loads((output / "docs/versions.json").read_text())],
                         ["1.2.0-rc.1", "1.1.0", "1.0.0", "dev"])
        with self.assertRaisesRegex(ValueError, "superseded by its final release"):
            assemble(self.baseline, [self.build_payload("1.0.0-rc.1", name="old")], self.root / "rejected")
        for superseded in (["1.3.0-rc.1"], ["1.1.0"]):
            with self.subTest(superseded=superseded), self.assertRaisesRegex(ValueError, "published final release"):
                assemble(self.baseline, [], self.root / "rejected", superseded)
        self.assertFalse((self.root / "rejected").exists())

    def decompress(self, command, *, stdout, check):
        stdout.write(Path(command[3]).with_suffix("").read_bytes())

    def publish(self, version, state, archives, name=None):
        evidence, output = self.root / ("evidence-" + (name or version)), self.root / ("site-" + (name or version))
        receipt = {"repository": REPOSITORY, "sha": "e" * 40}
        provenance = {"source": receipt, "artifacts": {}, "docs": self.identity,
                      "playground": self.provenance["playground"]}

        def download(config, destination):
            if "provenance" not in config:
                shutil.copytree(self.baseline, destination)
                return None
            return restore(archives[config["url"]], destination, config)

        api = Mock()
        api.get.return_value = {"draft": False, "tag_name": "v" + version, "status": "ahead"}
        with (patch.object(publication, "publication_source"),
              patch.object(publication.storage, "read", return_value=("head", state)),
              patch.object(publication, "download", side_effect=download),
              patch.object(publication, "download_inputs",
                           return_value=({"docs": self.docs, "wasm": self.wasm}, copy.deepcopy(provenance))),
              patch("scripts.ci.github_pages.subprocess.run", side_effect=self.decompress),
              contextlib.redirect_stdout(io.StringIO())):
            publication.build(api, REPOSITORY, receipt, version, output, evidence)
        request = json.loads((evidence / "request.json").read_text())
        archives[request["entry"]["url"]] = evidence / "content.tar.gz"
        return output, request["state"]

    def test_final_publication_removes_its_prereleases_and_later_publications_keep_them_removed(self):
        baseline = json.loads((ROOT / ".github/pages-baseline.json").read_text())
        state = {"version": 1, "baseline": baseline, "releases": {}, "superseded": [], "dev": None}
        archives = {}
        for version in ("2.0.0-alpha.1", "2.0.0-beta.1", "2.0.0-rc.2", "2.1.0-rc.1"):
            content, provenance = self.build_payload(version, name=version)
            provenance["source"] = {"repository": REPOSITORY, "sha": "a" * 40}
            bundle = self.root / (version + ".tar.gz")
            entry = {key: value for key, value in archive(content, bundle, provenance).items()
                     if key in ("size", "sha256")}
            entry.update(provenance=provenance, url=f"https://github.com/{REPOSITORY}/releases/download/"
                                                     f"website-content/website-{version}-{entry['sha256']}.tar.gz")
            archives[entry["url"]] = bundle
            state["releases"][version] = entry
        prereleases = dict(state["releases"])
        retired = ["2.0.0-alpha.1", "2.0.0-beta.1", "2.0.0-rc.2"]
        redirects = {f"{area}/{version}/": f"{area}/2.0.0/" for version in retired for area in ("docs", "playground")}
        final, state = self.publish("2.0.0", state, archives)
        self.assertEqual((sorted(state["releases"]), state["superseded"]), (["2.0.0", "2.1.0-rc.1"], retired))
        development, later = self.publish("dev", state, archives)
        self.assertEqual(later["superseded"], retired)
        unretired = {**later, "releases": {**prereleases, **later["releases"]}, "superseded": []}
        healed, healed_state = self.publish("dev", unretired, archives, name="healed")
        self.assertEqual((healed_state["releases"].keys(), healed_state["superseded"]), (later["releases"].keys(), retired))
        for site in (final, development, healed):
            for area in ("docs", "playground"):
                versions = json.loads((site / area / "versions.json").read_text())
                listed = [entry["version"] for entry in (versions if area == "docs" else versions["versions"])]
                self.assertEqual(listed, ["2.1.0-rc.1", "2.0.0", "1.0.0", "dev"])
            receipt = json.loads((site / "shared/pages-transport.json").read_text())
            self.assertEqual(receipt["prerelease_redirects"], redirects)
            self.assertFalse(any(name.startswith(tuple(redirects)) and not name.endswith(".html")
                                 for name in receipt["published_files"]))
            self.assertFalse((site / "docs/2.0.0-rc.1").exists())
            self.assertIn('location.replace("../../2.0.0/guide/"', (site / "docs/2.0.0-rc.2/guide/index.html").read_text())
            self.assertIn('location.replace("../2.0.0/"', (site / "playground/2.0.0-beta.1/index.html").read_text())
            self.assertTrue((site / "playground/2.1.0-rc.1/playground.wasm.br").is_file())
        with self.assertRaisesRegex(ValueError, "superseded by its final release"):
            self.publish("2.0.0-rc.2", later, archives)

    def test_new_catalog_removes_only_legacy_aliases_for_redirect_regeneration(self):
        self.write(self.baseline / "controller/latest/assets.js", "stale alias")
        self.write(self.baseline / "controller/1.0.0/index.html", "historical URL")
        catalogs(self.baseline)
        self.assertFalse((self.baseline / "controller/latest").exists())
        self.assertEqual((self.baseline / "controller/1.0.0/index.html").read_text(), "historical URL")

    def test_unsafe_archives_and_expansion_limit_leave_no_output(self):
        for name, kind in (("../escape", tarfile.REGTYPE), ("/absolute", tarfile.REGTYPE),
                           ("link", tarfile.SYMTYPE), ("hardlink", tarfile.LNKTYPE),
                           ("file", tarfile.REGTYPE)):
            with self.subTest(name=name):
                bundle = self.root / "input.tar.gz"
                with tarfile.open(bundle, "w:gz") as stream:
                    member = tarfile.TarInfo(name)
                    member.type = kind
                    member.linkname = "../outside"
                    member.size = 2 if member.isfile() else 0
                    stream.addfile(member, io.BytesIO(b"xx"))
                with self.assertRaises(ValueError):
                    extract(bundle, self.root / "extracted", limit=1)
                self.assertFalse((self.root / "extracted").exists())

    def test_tar_roundtrip_and_duplicate_paths(self):
        bundle = self.root / "input.tar.gz"
        with tarfile.open(bundle, "w:gz") as stream:
            stream.add(self.docs, arcname=".")
        extract(bundle, self.root / "extracted")
        self.assertEqual(inventory(self.docs), inventory(self.root / "extracted"))
        with tarfile.open(bundle, "w:gz") as stream:
            stream.add(self.docs / "index.html", arcname="index.html")
            stream.add(self.docs / "index.html", arcname="./index.html")
        with self.assertRaisesRegex(ValueError, "duplicate"):
            extract(bundle, self.root / "duplicate")
        self.assertFalse((self.root / "duplicate").exists())


if __name__ == "__main__":
    unittest.main()
