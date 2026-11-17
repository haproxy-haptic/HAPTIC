"""Deliver every access-log record of a burst Vector is not reading (#677).

Real HAProxy and the chart's rendered Vector config share one network namespace,
as they do in the pod. Vector is frozen while HAProxy serves requests and
reloads, so every record waits in the kernel; each must arrive once Vector
resumes. A burst larger than the receive buffer must show up in
haptic_access_log_dropped_records_total.
"""

import os
from pathlib import Path
import re
import subprocess
import tempfile
import time

import yaml


ROOT = Path(__file__).resolve().parents[2]
BURST = 60
OVERFLOW = 600
LOADER = getattr(yaml, "CSafeLoader", yaml.SafeLoader)


def run(*arguments, **kwargs):
    return subprocess.run(arguments, check=True, capture_output=True, text=True, **kwargs).stdout


def rendered_section(dump, name):
    body = []
    for line in dump.split(f"{name}\n", 1)[1].splitlines():
        if line.startswith(("### ", "#### ", "=== ")):
            break
        if line and set(line) == {"-"}:
            continue
        body.append(line)
    return "\n".join(body)


def prepare(work):
    arguments = ["helm", "template", "haptic", str(ROOT / "charts/haptic"), "--namespace", "haptic"]
    if os.environ.get("HAPROXY_VERSION"):
        arguments += ["--set", f"haproxyVersion={os.environ['HAPROXY_VERSION']}"]
    documents = [d for d in yaml.load_all(run(*arguments), Loader=LOADER) if d]
    config, = [d for d in documents if d.get("kind") == "HAProxyTemplateConfig"]
    vector = config["spec"]["templatingSettings"]["extraContext"]["vector"]
    test, = [d["spec"]["validationTests"]["test-vector-omit-empty-log-fields"] for d in documents
             if "test-vector-omit-empty-log-fields" in d.get("spec", {}).get("validationTests", {})]
    test["extraContext"]["vector"] = vector
    test["extraContext"]["accessLog"] = config["spec"]["templatingSettings"]["extraContext"]["accessLog"]
    source = work / "chart.yaml"
    source.write_text(yaml.safe_dump_all(documents, sort_keys=False))
    dump = run(os.environ.get("CONTROLLER_BIN", str(ROOT / "bin/haptic")), "validate", "--file", str(source),
               "--schema-dir", str(ROOT / "tests/schemas"), "--test", "test-vector-omit-empty-log-fields",
               "--dump-rendered")
    pipeline = yaml.safe_load(rendered_section(dump, "#### vector.yaml"))
    pipeline["data_dir"] = "/tmp"
    for sink in pipeline["sinks"].values():
        if sink["type"] == "prometheus_exporter":
            sink["address"] = sink["address"].replace("0.0.0.0", "127.0.0.1")
    (work / "vector.yaml").write_text(yaml.safe_dump(pipeline, sort_keys=False))
    defaults = re.search(r"(?ms)^defaults haptic-base$(.*?)^\S", rendered_section(dump, "### haproxy.cfg")).group(1)
    log_line, = re.findall(r"(?m)^  (log .*)$", defaults)
    # Pads records to the 815-byte average measured on a real fleet: the buffer
    # holds fewer records the larger they are.
    (work / "haproxy.cfg").write_text(f"""global
    log stdout format raw local0
defaults
    mode http
    {log_line}
    timeout client 5s
    timeout server 5s
    timeout connect 1s
frontend fe
    bind 127.0.0.1:8080
    http-request set-var(txn.path) path
    log-format "%{{+json}}o %(path)[var(txn.path)] %(status)ST %(pad)[str({"x" * 760})]"
    http-request return status 200
""")
    images = {c["name"]: c["image"] for d in documents if d.get("kind") == "Deployment"
              for c in d["spec"]["template"]["spec"].get("initContainers", []) + d["spec"]["template"]["spec"]["containers"]
              if c["name"] in ("vector", "haproxy")}
    return images["vector"], images["haproxy"], vector["metricsPort"]


def served(container, first, count):
    script = (f'for i in $(seq {first} {first + count - 1}); do exec 3<>/dev/tcp/127.0.0.1/8080; '
              'printf "GET /burst-%d HTTP/1.0\\r\\n\\r\\n" "$i" >&3; read -r status <&3; exec 3<&-; '
              'case "$status" in *" 200 "*) ;; *) echo "request $i: $status"; exit 1;; esac; done')
    run("docker", "exec", container, "bash", "-c", script)


def burst_ids(vector):
    return set(re.findall(r'"path": ?"/burst-(\d+)"', run("docker", "logs", vector)))


def dropped(haproxy, port):
    script = (f'exec 3<>/dev/tcp/127.0.0.1/{port}; printf "GET /metrics HTTP/1.0\\r\\n\\r\\n" >&3; cat <&3')
    for line in run("docker", "exec", haproxy, "bash", "-c", script).splitlines():
        if line.startswith("haptic_access_log_dropped_records_total"):
            return float(line.split()[-1])
    return None


def wait(what, budget, probe):
    deadline = time.monotonic() + budget
    while True:
        value = probe()
        if value:
            return value
        if time.monotonic() >= deadline:
            raise AssertionError(f"timed out waiting for {what}")
        time.sleep(0.2)


def main():
    with tempfile.TemporaryDirectory(prefix=".vector-burst-", dir=ROOT) as directory:
        work = Path(directory)
        vector_image, haproxy_image, metrics_port = prepare(work)
        vector = run("docker", "run", "--detach", "--network", "none", "--mount",
                     f"type=bind,src={work},dst=/probe,readonly", vector_image,
                     "--config", "/probe/vector.yaml").strip()
        haproxy = None
        try:
            haproxy = run("docker", "run", "--detach", "--network", f"container:{vector}", "--mount",
                          f"type=bind,src={work},dst=/probe,readonly", "--entrypoint", "haproxy", haproxy_image,
                          "-W", "-db", "-f", "/probe/haproxy.cfg").strip()
            wait("HAProxy to serve", 15, lambda: subprocess.run(
                ["docker", "exec", haproxy, "bash", "-c", "exec 3<>/dev/tcp/127.0.0.1/8080"],
                capture_output=True).returncode == 0)
            served(haproxy, 0, 1)
            wait("Vector to forward the first record", 15, lambda: "0" in burst_ids(vector))
            wait("the first drop-counter sample", 30, lambda: dropped(haproxy, metrics_port) is not None)

            run("docker", "pause", vector)
            served(haproxy, 1, BURST // 2)
            run("docker", "kill", "--signal", "USR2", haproxy)
            served(haproxy, 1 + BURST // 2, BURST - BURST // 2)
            run("docker", "unpause", vector)
            expected = {str(i) for i in range(BURST + 1)}
            try:
                wait(f"all {BURST + 1} records", 15, lambda: burst_ids(vector) >= expected)
            except AssertionError:
                missing = len(expected - burst_ids(vector))
                raise AssertionError(f"{missing} of {BURST + 1} access-log records never reached Vector") from None

            run("docker", "pause", vector)
            served(haproxy, BURST + 1, OVERFLOW)
            run("docker", "unpause", vector)
            total = BURST + 1 + OVERFLOW

            def counted():
                lost = total - len(burst_ids(vector))
                return lost if lost > 0 and dropped(haproxy, metrics_port) == lost else None
            lost = wait("haptic_access_log_dropped_records_total to equal the records that never arrived",
                        30, counted)
            print(f"burst of {BURST} delivered across a reload; overflow of {OVERFLOW} lost {lost}, all counted")
        except Exception:
            print(run("docker", "logs", "--tail", "30", vector))
            if haproxy:
                print(subprocess.run(["docker", "logs", "--tail", "30", haproxy], capture_output=True, text=True).stderr)
            raise
        finally:
            for container in (haproxy, vector):
                if container:
                    subprocess.run(["docker", "rm", "--force", container], capture_output=True)


if __name__ == "__main__":
    main()
