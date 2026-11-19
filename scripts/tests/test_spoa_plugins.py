import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
LIBRARIES = {
    "api-gateway": ("libapi_gateway_plugin",),
    "coraza": ("libcoraza_plugin", "libcoraza_rust_plugin"),
    "external-auth": ("libexternal_auth_plugin",),
    "fingerprinting": ("libfingerprinting_plugin",),
    "maxmind": ("libmaxmind_plugin",),
    "mirror": ("libmirror_plugin",),
    "rate-limit": ("librate_limit_plugin",),
    "sso-auth": ("libhaproxy_spoa_hub_plugin_sso_auth",),
}
LIBRARY_PROJECTS = {library: project for project, libraries in LIBRARIES.items() for library in libraries}
LEGACY_BUNDLES = {"fingerprinting", "maxmind", "sso-auth"}
ARCHES = ("amd64", "arm64", "armv7")
TRUSTED_ROOT = "tuf-repo-cdn.sigstore.dev/targets/trusted_root.json"
COMMAND_STUB = r'''
import hashlib, json, os, pathlib, re, sys, urllib.parse
args = sys.argv[1:]
command = pathlib.Path(sys.argv[0]).name
state = pathlib.Path(os.environ["TEST_STATE"])
mode = os.environ["TEST_MODE"]
with (state / "calls.jsonl").open("a") as stream:
    stream.write(json.dumps({"command": command, "args": args, "tuf_root": os.environ.get("TUF_ROOT")}) + "\n")

def option(name, default=None):
    return args[args.index(name) + 1] if name in args else default

def flake_once(name):
    marker = state / f"{name}-flaked"
    if mode == "tuf-flaky" and not marker.exists():
        marker.touch()
        print("tuf refresh failed: Get https://tuf-repo-cdn.sigstore.dev/15.root.json: 503", file=sys.stderr)
        sys.exit(1)

if command == "curl":
    _, owner, repository, releases, download, version, name = urllib.parse.urlsplit(args[-1]).path.split("/")
    assert (owner, releases, download) == ("haproxy-haptic", "releases", "download")
    plugin = repository.removeprefix("haproxy-spoa-hub-plugin-")
    if mode == "transport-failure" or (mode == "transport-after-manifest" and name != "SHA256SUMS"):
        sys.exit(7)
    headers = option("--dump-header")
    if headers:
        pathlib.Path(headers).write_text(
            "HTTP/2 302\r\nLocation: https://example.test/?token=redirect-secret\r\n"
            "Set-Cookie: session=cookie-secret\r\n\r\n"
            "HTTP/2 409\r\nx-request-id: request-safe-123\r\n"
            "X-Request-ID: \x1b[31munsafe-id\r\n"
            "X-Request-ID: whitespace invalid\r\n"
            "X-Request-ID: nonascii-ä\r\n"
            "X-Request-ID: " + "oversized" * 40 + "\r\n\r\n"
            if mode == "http-failure" else "HTTP/2 200\r\nx-request-id: previous-download-id\r\n\r\n")
    if mode == "http-failure":
        sys.exit(22)
    libraries = json.loads(os.environ["TEST_LIBRARIES"])
    glibc = os.environ["TEST_GLIBC"]
    filenames = [f"{library}-{arch}-glibc{glibc}.so" for library in libraries[plugin]
                 for arch in ("amd64", "arm64", "armv7")]
    def payload(filename):
        return f"{plugin}:{version}:{filename}\n".encode()
    if name == "SHA256SUMS":
        content = "" if mode == "missing-architecture" else "".join(
            hashlib.sha256(payload(filename)).hexdigest() + "  " + filename + "\n"
            for filename in filenames)
        content = content.encode()
    elif name.endswith(".cosign.bundle"):
        if mode == "github-signed":
            identity = (f"https://github.com/haproxy-haptic/haproxy-spoa-hub-plugin-{plugin}"
                        f"/.github/workflows/release.yml@refs/tags/{version}")
            issuer = "https://token.actions.githubusercontent.com"
        else:
            identity = f"https://gitlab.com/haproxy-haptic/haproxy-spoa-hub-plugin-{plugin}//.gitlab-ci.yml@refs/tags/{version}"
            issuer = "https://gitlab.com"
        if mode == "wrong-tag":
            identity = identity.rsplit("/", 1)[0] + "/v999.0.0"
        elif mode == "wrong-project":
            identity = identity.replace("/haproxy-haptic/", "/untrusted-project/")
        bundle = {"identity": identity, "issuer": "https://untrusted.test" if mode == "wrong-issuer" else issuer,
                  "digest": hashlib.sha256(payload(name.removesuffix(".cosign.bundle"))).hexdigest()}
        if plugin not in json.loads(os.environ["TEST_LEGACY"]):
            bundle["mediaType"] = "application/vnd.dev.sigstore.bundle.v0.3+json"
        content = json.dumps(bundle).encode()
    else:
        content = b"corrupted" if mode == "checksum-mismatch" else payload(name)
    pathlib.Path(option("--output")).write_bytes(content)
elif command == "cosign" and args == ["initialize"]:
    flake_once("initialize")
    root = pathlib.Path(os.environ["TUF_ROOT"]) / os.environ["TEST_TRUSTED_ROOT"]
    root.parent.mkdir(parents=True)
    root.write_text("{}")
elif command == "cosign":
    assert args[0] == "verify-blob"
    assert not any(flag.startswith("--insecure") or flag == "--ignore-sct" for flag in args)
    bundle = json.loads(pathlib.Path(option("--bundle")).read_text())
    expected_root = str(pathlib.Path(os.environ["TUF_ROOT"]) / os.environ["TEST_TRUSTED_ROOT"])
    assert option("--trusted-root") == (expected_root if "mediaType" in bundle else None), args
    if "mediaType" not in bundle:
        flake_once("verify")
    identity = option("--certificate-identity")
    identity_matches = identity == bundle["identity"] if identity is not None else bool(
        re.fullmatch(option("--certificate-identity-regexp", ""), bundle["identity"]))
    digest = hashlib.sha256(pathlib.Path(args[1]).read_bytes()).hexdigest()
    if (not identity_matches or option("--certificate-oidc-issuer") != bundle["issuer"]
            or digest != bundle["digest"] or mode == "invalid-signature"):
        print("signature verification rejected", file=sys.stderr)
        sys.exit(1)
else:
    sys.exit("unexpected command")
'''


def release_of(url):
    *_, repository, _, _, version, _ = url.split("/")
    return repository.removeprefix("haproxy-spoa-hub-plugin-"), version


def github_identity(plugin, version):
    return (f"https://github.com/haproxy-haptic/haproxy-spoa-hub-plugin-{plugin}"
            f"/.github/workflows/release.yml@refs/tags/{version}",
            "https://token.actions.githubusercontent.com")


def gitlab_identity(plugin, version):
    return (f"https://gitlab.com/haproxy-haptic/haproxy-spoa-hub-plugin-{plugin}"
            f"//.gitlab-ci.yml@refs/tags/{version}", "https://gitlab.com")


def commands(calls, name, *first):
    return [call for call in calls if call["command"] == name and tuple(call["args"][:len(first)]) == first]


class SPOAPluginPreparationTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory(prefix="haptic-spoa-plugins-test-")
        self.addCleanup(temp.cleanup)
        self.repo = Path(temp.name)
        (self.repo / "scripts").mkdir()
        (self.repo / "bin").mkdir()
        shutil.copyfile(ROOT / "scripts/prep-spoa-plugins.sh", self.repo / "scripts/prep-spoa-plugins.sh")
        shutil.copyfile(ROOT / "versions-spoa.env", self.repo / "versions-spoa.env")
        version_result = subprocess.run(
            ["bash", "-c", 'source versions-spoa.env; for pin_name in ${!SPOA_PLUGIN_@}; do '
             'printf "%s=%s\\n" "$pin_name" "${!pin_name}"; done'],
            cwd=self.repo, text=True, capture_output=True, check=True)
        self.versions = dict(line.split("=", 1) for line in version_result.stdout.splitlines())
        for command in ("curl", "cosign"):
            executable = self.repo / "bin" / command
            executable.write_text(f"#!{sys.executable}\n" + COMMAND_STUB)
            executable.chmod(0o755)
        self.cache = self.repo / ".cache/spoa-plugins"

    def run_preparation(self, mode, **env):
        (self.repo / "calls.jsonl").write_text("")
        env = os.environ | {
            "PATH": str(self.repo / "bin") + os.pathsep + os.environ["PATH"],
            "TEST_STATE": str(self.repo), "TEST_MODE": mode, "TEST_TRUSTED_ROOT": TRUSTED_ROOT,
            "TEST_LIBRARIES": json.dumps(LIBRARIES), "TEST_LEGACY": json.dumps(sorted(LEGACY_BUNDLES)),
            "TEST_GLIBC": self.versions["SPOA_PLUGIN_GLIBC_VERSION"],
        } | env
        result = subprocess.run(["bash", "scripts/prep-spoa-plugins.sh"], cwd=self.repo,
                                env=env, text=True, capture_output=True, timeout=60, check=False)
        calls = [json.loads(line) for line in (self.repo / "calls.jsonl").read_text().splitlines()]
        staged = {str(path.relative_to(self.repo)): hashlib.sha256(path.read_bytes()).hexdigest()
                  for path in (self.repo / "plugins").glob("*/*.so")}
        return result, calls, staged

    def pin(self, plugin):
        return self.versions["SPOA_PLUGIN_" + plugin.upper().replace("-", "_") + "_VERSION"]

    def cached(self, plugin, name):
        return self.cache / plugin / self.pin(plugin) / name

    def assert_all_verified(self, result, calls, staged, expected_identities):
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("verified 27 plugin .so files", result.stdout)
        self.assertEqual(set(staged), {f"plugins/{arch}/{name}.so"
                                       for arch in ARCHES for name in LIBRARY_PROJECTS})
        verifications = commands(calls, "cosign", "verify-blob")
        self.assertEqual(len(verifications), len(ARCHES) * len(LIBRARY_PROJECTS) * len(expected_identities))
        for index, call in enumerate(verifications):
            arguments = call["args"]
            with self.subTest(target=arguments[1]):
                self.assertNotIn("--certificate-identity-regexp", arguments)
                bundle = Path(arguments[arguments.index("--bundle") + 1]).name
                plugin = LIBRARY_PROJECTS[bundle.split("-", 1)[0]]
                identity, issuer = expected_identities[index % len(expected_identities)](plugin, self.pin(plugin))
                self.assertEqual(arguments[arguments.index("--certificate-identity") + 1], identity)
                self.assertEqual(arguments[arguments.index("--certificate-oidc-issuer") + 1], issuer)
                self.assertEqual("--trusted-root" in arguments, plugin not in LEGACY_BUNDLES)

    def test_verifies_pre_move_releases_with_their_exact_gitlab_identity(self):
        result, calls, staged = self.run_preparation("valid")
        self.assert_all_verified(result, calls, staged, (github_identity, gitlab_identity))
        downloads = [call["args"] for call in commands(calls, "curl")]
        self.assertEqual(len(downloads), len(LIBRARIES) + 2 * len(ARCHES) * len(LIBRARY_PROJECTS))
        for arguments in downloads:
            plugin, version = release_of(arguments[-1])
            self.assertEqual(version, self.pin(plugin))
            self.assertEqual(arguments[arguments.index("--retry") + 1], "9")
            self.assertEqual(arguments[arguments.index("--retry-max-time") + 1], "300")
            self.assertNotIn("--retry-delay", arguments)
            self.assertIn("--retry-all-errors", arguments)

    def test_verifies_github_releases_with_their_exact_github_identity(self):
        self.assert_all_verified(*self.run_preparation("github-signed"), (github_identity,))

    def test_reuses_cached_release_files_and_verifies_them_again(self):
        self.assert_all_verified(*self.run_preparation("valid"), (github_identity, gitlab_identity))
        planted = self.repo / "planted-tuf"
        result, calls, staged = self.run_preparation("transport-failure", TUF_ROOT=str(planted))
        self.assert_all_verified(result, calls, staged, (github_identity, gitlab_identity))
        self.assertIn("(62 cached files reused, 0 downloaded)", result.stdout)
        self.assertEqual(commands(calls, "curl"), [])
        initializations = commands(calls, "cosign", "initialize")
        self.assertEqual(len(initializations), 1)
        self.assertNotEqual(initializations[0]["tuf_root"], str(planted))
        self.assertFalse(Path(initializations[0]["tuf_root"]).exists())

    def test_rejects_tampered_cached_files(self):
        self.assert_all_verified(*self.run_preparation("valid"), (github_identity, gitlab_identity))
        library = self.cached("coraza", f"libcoraza_rust_plugin-arm64-glibc{self.versions['SPOA_PLUGIN_GLIBC_VERSION']}.so")
        sums = self.cached("coraza", "SHA256SUMS")
        original = library.read_bytes(), sums.read_text()
        library.write_bytes(b"tampered")
        result, calls, _ = self.run_preparation("transport-failure")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(f"SHA256 mismatch for {library.name}", result.stderr)
        self.assertIn(f"Cached files in {library.parent} failed verification", result.stderr)
        self.assertNotIn("OK: verified", result.stdout)
        self.assertEqual(commands(calls, "curl"), [])

        tampered_digest = hashlib.sha256(b"tampered").hexdigest()
        sums.write_text(original[1].replace(hashlib.sha256(original[0]).hexdigest(), tampered_digest))
        result, _, _ = self.run_preparation("transport-failure")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("signature verification rejected", result.stderr)
        self.assertIn(f"cosign rejected the signature of {library.name}", result.stderr)
        self.assertNotIn("OK: verified", result.stdout)

    def test_unverified_downloads_are_not_cached(self):
        for mode in ("checksum-mismatch", "invalid-signature"):
            with self.subTest(mode=mode):
                result, _, _ = self.run_preparation(mode)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual([path for path in self.cache.rglob("*") if path.is_file()], [])
                self.assertNotIn("Cached files in", result.stderr)

    def test_prunes_release_files_no_longer_pinned(self):
        stale = [self.cache / "coraza/v0.0.1/SHA256SUMS", self.cache / "retired-plugin/v1.0.0/SHA256SUMS"]
        for path in stale:
            path.parent.mkdir(parents=True)
            path.write_text("stale")
        self.assert_all_verified(*self.run_preparation("valid"), (github_identity, gitlab_identity))
        for path in stale:
            self.assertFalse(path.parent.exists(), path)

    def test_retries_failed_sigstore_fetches_but_not_verdicts(self):
        result, calls, staged = self.run_preparation("tuf-flaky")
        self.assertEqual(len(commands(calls, "cosign", "initialize")), 2)
        verifications = commands(calls, "cosign", "verify-blob")
        self.assertEqual(len(verifications), len(ARCHES) * len(LIBRARY_PROJECTS) * 2 + 1)
        retried = [index for index in range(len(verifications) - 1)
                   if verifications[index]["args"] == verifications[index + 1]["args"]]
        self.assertEqual(len(retried), 1)
        self.assertNotIn("--trusted-root", verifications[retried[0]]["args"])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(staged), len(ARCHES) * len(LIBRARY_PROJECTS))

    def test_rejects_untrusted_or_wrong_version_signature(self):
        for mode in ("wrong-tag", "wrong-project", "wrong-issuer", "invalid-signature"):
            with self.subTest(mode=mode):
                result, calls, _ = self.run_preparation(mode)
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertIn("signature verification rejected", result.stderr)
                self.assertEqual(len(commands(calls, "cosign", "verify-blob")), 2)
                self.assertNotIn("OK: verified", result.stdout)

    def test_rejects_invalid_checksum_and_missing_architecture(self):
        for mode, message in (("checksum-mismatch", "SHA256 mismatch"),
                              ("missing-architecture", "not present in upstream SHA256SUMS")):
            with self.subTest(mode=mode):
                result, calls, _ = self.run_preparation(mode)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(message, result.stderr)
                self.assertEqual(commands(calls, "cosign", "verify-blob"), [])

    def test_http_failure_reports_only_safe_request_diagnostics(self):
        result, calls, _ = self.run_preparation("http-failure")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("download failed:", result.stderr)
        self.assertIn("HTTP status: 409", result.stderr)
        self.assertIn("request-safe-123", result.stderr)
        for excluded in ("redirect-secret", "cookie-secret", "unsafe-id", "\x1b",
                         "whitespace", "nonascii", "oversized"):
            self.assertNotIn(excluded, result.stdout + result.stderr)
        self.assertEqual(len(commands(calls, "curl")), 1)

    def test_transport_failure_is_not_accepted_or_verified(self):
        for mode, expected_calls in (("transport-failure", 1), ("transport-after-manifest", 2)):
            with self.subTest(mode=mode):
                result, calls, _ = self.run_preparation(mode)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("download failed:", result.stderr)
                self.assertEqual(len(commands(calls, "curl")), expected_calls)
                self.assertEqual(commands(calls, "cosign", "verify-blob"), [])
                self.assertNotIn("OK: verified", result.stdout)
                self.assertNotIn("HTTP status:", result.stderr)
                self.assertNotIn("previous-download-id", result.stderr)


if __name__ == "__main__":
    unittest.main()
