"""Keep access logs and request metrics flowing while the OTLP buffer is full."""

import copy
from contextlib import chdir
import json
import os
from pathlib import Path
import re
import socket
import subprocess
import tempfile
import time

import yaml


ROOT = Path(__file__).resolve().parents[2]
RECORDS = 10000
LOADER = getattr(yaml, "CSafeLoader", yaml.SafeLoader)


def run(*arguments, **kwargs):
    return subprocess.run(arguments, check=True, capture_output=True, text=True, **kwargs).stdout


def prepare(work):
    rendered = run("helm", "template", "haptic", str(ROOT / "charts/haptic"), "--namespace", "haptic",
                   "--set", "spoaHub.enabled=false",
                   "--set", "controller.config.templatingSettings.extraContext.tracing.enabled=true",
                   "--set", "controller.config.templatingSettings.extraContext.tracing.otlp.endpoint=http://127.0.0.1:4318/v1/traces")
    documents = list(yaml.load_all(rendered, Loader=LOADER))
    config, = [d for d in documents if d and d.get("kind") == "HAProxyTemplateConfig"]
    vector = config["spec"]["templatingSettings"]["extraContext"]["vector"]
    tests = [d["spec"]["validationTests"]["test-vector-omit-empty-log-fields"] for d in documents
             if d and "test-vector-omit-empty-log-fields" in d.get("spec", {}).get("validationTests", {})]
    test, = tests
    test["extraContext"]["vector"] = copy.deepcopy(vector)
    source = work / "chart.yaml"
    source.write_text(yaml.safe_dump_all(documents, sort_keys=False))
    dump = run(os.environ.get("CONTROLLER_BIN", str(ROOT / "bin/haptic")), "validate", "--file", str(source),
               "--schema-dir", str(ROOT / "tests/schemas"), "--test", "test-vector-omit-empty-log-fields", "--dump-rendered")
    body = []
    for line in dump.split("#### vector.yaml", 1)[1].splitlines():
        if line.startswith(("#### ", "=== ")):
            break
        if line and set(line) == {"-"}:
            continue
        body.append(line)
    pipeline = yaml.safe_load("\n".join(body))
    pipeline["data_dir"] = "/tmp/vector-backpressure"
    pipeline["sources"]["haproxy_log"]["path"] = "/probe/haproxy.sock"
    for sink in pipeline["sinks"].values():
        if sink["type"] == "prometheus_exporter":
            sink["address"] = sink["address"].replace("0.0.0.0", "127.0.0.1")
    (work / "vector.yaml").write_text(yaml.safe_dump(pipeline, sort_keys=False))
    images = {container["image"] for d in documents if d and d.get("kind") == "Deployment"
              for container in d["spec"]["template"]["spec"].get("initContainers", [])
              + d["spec"]["template"]["spec"].get("containers", []) if container["name"] == "vector"}
    image, = images
    return image, vector["metricsPort"]


def metrics(container, port):
    script = f'exec 3<>/dev/tcp/127.0.0.1/{port}; printf "GET /metrics HTTP/1.0\\r\\n\\r\\n" >&3; cat <&3'
    return run("docker", "exec", container, "/usr/bin/bash", "-c", script, timeout=5)


def total(text, name, component=None):
    values = []
    for line in text.splitlines():
        if not re.match(re.escape(name) + r"(?:\{|\s)", line):
            continue
        if component and f'component_id="{component}"' not in line:
            continue
        values.append(float(line.split()[-1]))
    return sum(values)


def send_records(work, start=0, count=RECORDS):
    base = {
        "ts": "2026-07-30T09:00:00.123456Z", "trace_id": "a" * 32, "span_id": "b" * 16,
        "upstream_span_id": "c" * 16, "parent_span_id": "", "trace_flags": "01",
        "te_us": 1785402000176900, "handshake_time_ms": 5, "idle_time_ms": 0,
        "request_time_ms": 2, "queue_time_ms": 0, "connect_time_ms": 1,
        "response_time_ms": 3, "transfer_time_ms": 40, "total_time_ms": 46,
        "retries": 0, "method": "GET", "path": "/pressure", "host": "example.com",
        "route": "example.com/pressure", "frontend": "https", "backend": "echo", "server": "echo-pod",
        "server_pod": "echo-pod", "resource": "default/echo", "service": "echo", "namespace": "default",
        "instance_pod": "haproxy-1", "status": 200, "bytes": 4096, "bytes_in": 128, "term": "----",
    }
    with chdir(work), socket.socket(socket.AF_UNIX, socket.SOCK_DGRAM) as sender:
        sender.settimeout(2)
        for index in range(start, start + count):
            record = dict(base, req_id=f"pressure-{index}")
            try:
                sender.sendto(json.dumps(record).encode(), "haproxy.sock")
            except TimeoutError as error:
                raise AssertionError(f"Vector stopped draining the log socket at record {index}") from error
            if index % 25 == 0:
                time.sleep(0.01)


def check_logs(container, expected_count):
    logs = run("docker", "logs", container).splitlines()
    records = [json.loads(line) for line in logs if line.startswith("{")]
    ids = [record["req_id"] for record in records]
    expected = {f"pressure-{index}" for index in range(expected_count)}
    assert len(ids) == expected_count and set(ids) == expected, f"Missing or duplicated stdout records: {len(ids)}/{expected_count}"


def check_outage(container, port):
    deadline = time.monotonic() + 15
    while True:
        text = metrics(container, port)
        requests = total(text, "haptic_ingress_controller_requests_total")
        dropped = total(text, "vector_buffer_discarded_events_total", "otlp_traces")
        if requests == RECORDS and dropped > 0:
            break
        if time.monotonic() >= deadline:
            raise AssertionError(f"Expected {RECORDS} requests and OTLP buffer overflow, got requests={requests}, dropped={dropped}")
        time.sleep(0.1)
    exported = total(text, "vector_component_sent_events_total", "otlp_traces")
    assert exported == 0, f"The isolated OTLP endpoint unexpectedly exported {exported} spans"
    check_logs(container, RECORDS)
    print(json.dumps({"access_records": RECORDS, "requests_total": requests,
                      "otlp_buffer_dropped": dropped, "otlp_exported": exported}))


def received_request_ids(work):
    path = work / "received.json"
    if not path.exists():
        return set()
    result = set()
    for line in path.read_text().split("\n")[:-1]:
        for resource in json.loads(line).get("resourceSpans", []):
            for scope in resource["scopeSpans"]:
                for span in scope["spans"]:
                    for attribute in span["attributes"]:
                        if attribute["key"] == "haptic.req_id":
                            result.add(attribute["value"]["stringValue"])
    return result


def check_recovery(work, container, port):
    collector = {
        "data_dir": "/tmp/vector-collector",
        "sources": {"otlp": {"type": "http_server", "address": "127.0.0.1:4318", "path": "/v1/traces",
                             "decoding": {"codec": "json"}, "framing": {"method": "bytes"}}},
        "sinks": {"received": {"type": "file", "inputs": ["otlp"], "path": "/probe/received.json",
                               "encoding": {"codec": "json"}}},
    }
    (work / "collector.yaml").write_text(yaml.safe_dump(collector, sort_keys=False))
    run("docker", "exec", "--detach", container, "/usr/bin/bash", "-c",
        "exec vector --config /probe/collector.yaml > /probe/collector.stdout 2> /probe/collector.stderr")
    deadline = time.monotonic() + 30
    while True:
        text = metrics(container, port)
        if (total(text, "vector_component_sent_events_total", "otlp_traces") > 0
                and total(text, "vector_buffer_size_events", "otlp_traces") == 0):
            break
        if time.monotonic() >= deadline:
            raise AssertionError("Trace export did not recover after the collector started")
        time.sleep(0.1)
    send_records(work, start=RECORDS, count=1)
    deadline = time.monotonic() + 15
    while True:
        text = metrics(container, port)
        if (f"pressure-{RECORDS}" in received_request_ids(work)
                and total(text, "haptic_ingress_controller_requests_total") == RECORDS + 1):
            break
        if time.monotonic() >= deadline:
            raise AssertionError("Fresh spans or request metrics did not reach the recovered collector")
        time.sleep(0.1)
    check_logs(container, RECORDS + 1)
    print(json.dumps({"recovered_access_records": RECORDS + 1, "fresh_span_delivered": True}))


def main():
    with tempfile.TemporaryDirectory(prefix=".vector-backpressure-", dir=ROOT) as directory:
        work = Path(directory)
        image, port = prepare(work)
        container = run("docker", "create", "--network", "none", "--mount", f"type=bind,src={work},dst=/probe",
                        image, "--config", "/probe/vector.yaml").strip()
        try:
            run("docker", "start", container)
            deadline = time.monotonic() + 15
            while not (work / "haproxy.sock").exists():
                if time.monotonic() >= deadline:
                    raise AssertionError("Vector did not create its access-log socket")
                time.sleep(0.05)
            send_records(work)
            check_outage(container, port)
            check_recovery(work, container, port)
        except Exception:
            print(run("docker", "inspect", "--format", "{{json .State}}", container))
            samples = metrics(container, port)
            logs = run("docker", "logs", container).splitlines()
            print(json.dumps({
                "stdout_records": sum(line.startswith("{") for line in logs),
                "requests_total": total(samples, "haptic_ingress_controller_requests_total"),
                "otlp_buffer_size": total(samples, "vector_buffer_size_events", "otlp_traces"),
                "otlp_buffer_dropped": total(samples, "vector_buffer_discarded_events_total", "otlp_traces"),
            }))
            output = subprocess.run(["docker", "logs", "--tail", "20", container], capture_output=True, text=True)
            print(output.stderr)
            if (work / "collector.stderr").exists():
                print((work / "collector.stderr").read_text())
            raise
        finally:
            run("docker", "rm", "--force", container)


if __name__ == "__main__":
    main()
