"""Run complete Gateway conformance and retain its verified source identity."""

import argparse
import contextlib
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile
import threading
import time


def plan(env, sha, version):
    repository = env["GITHUB_REPOSITORY"]
    if (not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9_.-]+", repository) or
            not re.fullmatch(r"[a-f0-9]{40}", sha) or
            sha != env["GITHUB_SHA"] or sha != env["CONFORMANCE_SOURCE_SHA"]):
        raise ValueError("Conformance checkout differs from its workflow source")
    if env.get("TEST_RUN_PATTERN"):
        raise ValueError("Conformance evidence requires the complete unfiltered suite")
    mode = env["CONFORMANCE_MODE"]
    if mode not in {"candidate", "release"}:
        raise ValueError("Unknown conformance evidence mode")
    release = mode == "release"
    if release:
        if (not re.fullmatch(r"\d+\.\d+\.\d+(?:-[0-9A-Za-z]+(?:[.-][0-9A-Za-z]+)*)?", version) or
                env["GITHUB_REF_TYPE"] != "tag" or env["GITHUB_REF_NAME"] != "v" + version):
            raise ValueError("Release conformance requires the exact version tag")
        image = "ghcr.io/" + repository.split("/")[0].lower() + "/haptic:" + version + "-haproxy3.4"
    else:
        version = "ci-" + sha[:12]
        image = "haptic:test"
    return {"sha": sha, "repository": repository, "version": version, "image": image,
            "release": release}


def execute(selected, env, *, startup_only=False, run=subprocess.run, output=subprocess.check_output):
    if startup_only and selected["release"]:
        raise ValueError("Published release conformance requires the complete report")
    if output(["kind", "get", "clusters"], text=True).strip():
        raise ValueError("Conformance evidence requires a fresh runner without existing Kind clusters")
    image = selected["image"]
    if selected["release"]:
        digest = json.loads(output(["docker", "buildx", "imagetools", "inspect", image,
                                    "--format", "{{json .Manifest.Digest}}"], text=True))
        if not re.fullmatch(r"sha256:[a-f0-9]{64}", digest):
            raise ValueError("Published controller image has an invalid manifest digest")
        image = image.rsplit(":", 1)[0] + "@" + digest
        run(["bash", "scripts/retry.sh", "docker", "pull", image], check=True)
        run(["docker", "tag", image, "haptic:test"], check=True)
    else:
        run(["make", "docker-build-test-native", "GIT_TAG=" + selected["version"]], check=True)
    report_env = {**env, "HAPROXY_VERSION": "3.4", "HAPTIC_E2E_PROFILE": "conformance",
                  "HAPTIC_E2E_GWAPI_CHANNEL": "experimental", "KEEP_CLUSTER": "true",
                  "SKIP_DOCKER_BUILD": "1", "HAPTIC_E2E_CONTROLLER_STARTUP": "parallel",
                  "HAPTIC_E2E_EXTRA_VALUES": str(Path(".github/conformance-values.yaml").resolve()),
                  "CONFORMANCE_PARALLEL": "4", "CONFORMANCE_CONTROLLER_IMAGE": image,
                  "CONFORMANCE_IMPL_VERSION": selected["version"],
                  "CONFORMANCE_RELEASE_EVIDENCE": "1" if selected["release"] else "",
                  "CONFORMANCE_ARTIFACT_DIR": "build/gateway-conformance",
                  "CI_JOB_URL": f"https://github.com/{selected['repository']}/actions/runs/{env['GITHUB_RUN_ID']}"}
    with observe_startup(Path("debug-logs/conformance-report")):
        run(["make", "test-e2e"], env={**report_env, "TEST_RUN_PATTERN": "^$"}, check=True)
    if not startup_only:
        run(["make", "test-gateway-conformance-report"], env=report_env, check=True)


def sample_startup(*, run=subprocess.run):
    kubectl = ["docker", "exec", "haptic-e2e-control-plane", "kubectl", "--kubeconfig=/etc/kubernetes/admin.conf"]
    commands = {
        "pods": kubectl + ["get", "pods", "-A", "-o", "json"],
        "events": kubectl + ["get", "events", "-A", "-o", "json"],
        "usage": kubectl + ["get", "--raw", "/apis/metrics.k8s.io/v1beta1/pods"],
        "node_cgroup": ["docker", "exec", "haptic-e2e-control-plane", "sh", "-c",
                        'for item in cpu.stat cpu.pressure memory.current memory.max memory.events memory.pressure io.pressure; '
                        'do printf "%s\\n" "$item"; cat "/sys/fs/cgroup/$item"; done'],
    }
    sample = {"time": time.time()}
    for name, command in commands.items():
        try:
            result = run(command, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                         timeout=5, check=False)
            if result.returncode:
                sample[name] = {"exit_code": result.returncode, "error": result.stderr}
                continue
            if name == "node_cgroup":
                sample[name] = result.stdout
                continue
            data = json.loads(result.stdout)
            if name == "pods":
                sample[name] = [{"namespace": pod["metadata"]["namespace"], "name": pod["metadata"]["name"],
                                 "created": pod["metadata"]["creationTimestamp"], "status": pod["status"]}
                                for pod in data["items"]]
            else:
                sample[name] = data
        except (OSError, ValueError, KeyError, TypeError, subprocess.TimeoutExpired) as error:
            sample[name] = {"error": str(error)}
    return sample


@contextlib.contextmanager
def observe_startup(directory):
    directory.mkdir(parents=True, exist_ok=True)
    finished = threading.Event()

    def collect():
        with (directory / "startup-timeline.jsonl").open("w", buffering=1) as stream:
            while not finished.is_set():
                stream.write(json.dumps(sample_startup()) + "\n")
                finished.wait(5)

    observer = threading.Thread(target=collect)
    observer.start()
    try:
        yield
    finally:
        finished.set()
        observer.join()


def verify_startup(selected, directory):
    spec = importlib.util.spec_from_file_location("startup", Path(".github/scripts/verify-startup.py"))
    verifier = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(verifier)
    pods = json.loads((directory / "pods.json").read_text())
    first = verifier.verify((directory / "controller-controller-current.log").read_text(), pods)
    controllers = {"items": [pod for pod in pods["items"]
                             if pod["metadata"].get("labels", {}).get("app.kubernetes.io/component") == "controller"]}
    verifier.verify_environment(controllers, {"GOMAXPROCS": "2", "GOGC": "300", "TMPDIR": "/v"})
    result = {"diagnostic_only": True, "source": selected, "controllers": first}
    (directory / "startup-verdict.json").write_text(json.dumps(result, indent=2) + "\n")


def bundle(selected, env, directory):
    record = json.loads((directory / "provenance.json").read_text())
    expected = {"git_commit": selected["sha"], "release_evidence": selected["release"],
                "verified": True, "test_exit_code": 0, "errors": [], "selection": "full-unsharded-suite"}
    if any(record.get(key) != value for key, value in expected.items()):
        raise ValueError("Conformance provenance does not verify this complete source and suite")
    if selected["release"] and (record.get("dirty_checkout") is not False or
                                record["controller"].get("commit") != selected["sha"] or
                                not record["controller"].get("registry_digests")):
        raise ValueError("Release conformance requires the published binary and a clean checkout")
    if record["controller"]["version"] != selected["version"]:
        raise ValueError("Conformance report has a different implementation version")
    if record["report_sha256"] != hashlib.sha256((directory / "report.yaml").read_bytes()).hexdigest():
        raise ValueError("Conformance report changed after provenance verification")
    expected_sums = {name: hashlib.sha256((directory / name).read_bytes()).hexdigest()
                     for name in ("report.yaml", "provenance.json")}
    actual_sums = {}
    for line in (directory / "SHA256SUMS").read_text().splitlines():
        digest, name = line.split("  ", 1)
        if name in actual_sums:
            raise ValueError("Conformance checksum inventory contains duplicate files")
        actual_sums[name] = digest
    if actual_sums != expected_sums:
        raise ValueError("Conformance checksums differ from the verified evidence")
    run_id, attempt = env["GITHUB_RUN_ID"], env["GITHUB_RUN_ATTEMPT"]
    if not run_id.isdecimal() or not attempt.isdecimal():
        raise ValueError("Conformance archive requires a workflow run and attempt")
    name = f"gateway-conformance-{selected['version']}-{run_id}-{attempt}.tar.gz"
    with tarfile.open(directory / name, "x:gz") as archive:
        for filename in ("report.yaml", "provenance.json", "SHA256SUMS"):
            archive.add(directory / filename, arcname=filename, recursive=False)
    with open(env["GITHUB_OUTPUT"], "a", encoding="utf-8") as output:
        output.write(f"archive={name}\nversion={selected['version']}\n")
    return name


def diagnostics(directory, *, run=subprocess.run):
    directory.mkdir(parents=True, exist_ok=True)
    kubectl = ["docker", "exec", "haptic-e2e-control-plane", "kubectl", "--kubeconfig=/etc/kubernetes/admin.conf"]
    commands = {"containers.txt": ["docker", "ps", "-a"],
                "pods.json": kubectl + ["get", "pods", "-A", "-o", "json"],
                "events.json": kubectl + ["get", "events", "-A", "-o", "json"],
                "pod-details.txt": kubectl + ["describe", "pods", "-A"],
                "jobs.json": kubectl + ["get", "jobs", "-n", "haptic", "-o", "json"],
                "services.json": kubectl + ["get", "services", "-A", "-o", "json"],
                "endpointslices.json": kubectl + ["get", "endpointslices", "-A", "-o", "json"],
                "servicel2statuses.json": kubectl + ["get", "servicel2statuses.metallb.io", "-A", "-o", "json"],
                "kube-proxy-config.yaml": kubectl + ["get", "configmap", "kube-proxy", "-n", "kube-system", "-o", "yaml"],
                "node-neighbours.txt": ["docker", "exec", "haptic-e2e-control-plane", "ip", "neigh", "show"]}
    for namespace, selector, container in (
            ("haptic", "app.kubernetes.io/component=pre-rollout-validation", "preflight"),
            ("haptic", "app.kubernetes.io/component=controller", "controller"),
            ("haptic", "app.kubernetes.io/component=controller", "validators"),
            ("haptic", "app.kubernetes.io/component=loadbalancer", "spoa-hub"),
            ("gateway-conformance-infra", "app=infra-backend-v1", "infra-backend-v1"),
            ("gateway-conformance-infra", "app=infra-backend-v2", "infra-backend-v2"),
            ("gateway-conformance-infra", "app=infra-backend-v3", "infra-backend-v3"),
            ("metallb-system", "app=metallb,component=controller", "controller"),
            ("metallb-system", "app=metallb,component=speaker", "speaker"),
            ("kube-system", "k8s-app=kube-proxy", "kube-proxy")):
        component = selector.rpartition("=")[2]
        prefix = component if namespace == "haptic" else f"{namespace}-{component}"
        for previous in (False, True):
            name = f"{prefix}-{container}-{'previous' if previous else 'current'}.log"
            commands[name] = kubectl + ["logs", "-n", namespace, "-l", selector,
                                        "-c", container, "--prefix", "--tail=-1", "--max-log-requests=20"]
            if previous:
                commands[name].append("--previous")
    results = {}

    def collect(name, command):
        with (directory / name).open("w") as stream:
            try:
                result = run(command, stdout=stream, stderr=subprocess.STDOUT, timeout=30, check=False)
                results[name] = {"exit_code": result.returncode}
            except (OSError, subprocess.TimeoutExpired) as error:
                results[name] = {"error": str(error)}

    for name, command in commands.items():
        collect(name, command)
    try:
        pods = json.loads((directory / "pods.json").read_text())["items"]
    except (ValueError, KeyError, TypeError) as error:
        results["spoa-metrics"] = {"error": str(error)}
    else:
        for pod in pods:
            metadata = pod.get("metadata", {})
            if (metadata.get("namespace") == "haptic" and
                    metadata.get("labels", {}).get("app.kubernetes.io/component") == "loadbalancer"):
                name = metadata["name"]
                collect(f"{name}-spoa.metrics", kubectl + ["exec", "-n", "haptic", name,
                        "-c", "haproxy", "--", "curl", "--silent", "--show-error", "--fail",
                        "--max-time", "10", "http://127.0.0.1:9095/metrics"])
    (directory / "collection.json").write_text(json.dumps(results, indent=2) + "\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bundle", action="store_true")
    parser.add_argument("--startup-only", action="store_true")
    parser.add_argument("--diagnostics", type=Path, metavar="DIRECTORY",
                        help="only collect cluster diagnostics into DIRECTORY")
    args = parser.parse_args()
    if args.diagnostics:
        diagnostics(args.diagnostics)
        return
    if args.bundle and args.startup_only:
        raise ValueError("Startup diagnostics cannot produce a conformance report archive")
    sha = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
    selected = plan(os.environ, sha, Path("VERSION").read_text().strip())
    if args.bundle:
        print(bundle(selected, os.environ, Path("build/gateway-conformance")))
    else:
        try:
            execute(selected, dict(os.environ), startup_only=args.startup_only)
        finally:
            diagnostics(Path("debug-logs/conformance-report"))
        if args.startup_only:
            verify_startup(selected, Path("debug-logs/conformance-report"))


if __name__ == "__main__":
    main()
