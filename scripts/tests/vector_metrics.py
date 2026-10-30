"""Run behavioral assertions against the chart's rendered Vector transforms."""

import copy
import json
import pathlib
import sys

import yaml

work = pathlib.Path(sys.argv[1])
server_label_enabled = sys.argv[2] == "true"
documents = list(yaml.safe_load_all((work / "chart.yaml").read_text()))
if len(sys.argv) > 3 and sys.argv[3] == "--prepare":
    configs = [document for document in documents
               if document and document.get("kind") == "HAProxyTemplateConfig"]
    if len(configs) != 1:
        sys.exit("the chart must render exactly one HAProxyTemplateConfig")
    vector = configs[0]["spec"]["templatingSettings"]["extraContext"]["vector"]
    if vector["requestMetrics"].get("serverLabel") is not server_label_enabled:
        sys.exit("the chart did not project vector.requestMetrics.serverLabel")
    selected_tests = []
    for document in documents:
        tests = (document or {}).get("spec", {}).get("validationTests", {})
        if "test-vector-omit-empty-log-fields" in tests:
            selected_tests.append(tests["test-vector-omit-empty-log-fields"])
    if len(selected_tests) != 1:
        sys.exit("the chart must render exactly one Vector log-preservation test")
    selected_tests[0]["extraContext"]["vector"] = copy.deepcopy(vector)
    (work / "native-chart.yaml").write_text(yaml.safe_dump_all(documents, sort_keys=False))
    sys.exit(0)

images = set()
for document in documents:
    if not document or document.get("kind") != "Deployment":
        continue
    spec = document["spec"]["template"]["spec"]
    for container in spec.get("containers", []) + spec.get("initContainers", []):
        if container["name"] == "vector":
            images.add(container["image"])
if len(images) != 1:
    sys.exit("the chart must select exactly one Vector image")
(work / "vector-image").write_text(images.pop())

rendered = (work / "rendered.txt").read_text()
body = []
for line in rendered.split("#### vector.yaml", 1)[1].splitlines():
    if line.startswith(("#### ", "=== ")):
        break
    if line and set(line) == {"-"}:
        continue
    body.append(line)
config = yaml.safe_load("\n".join(body))
for compact in (False, True):
    forwarding = {
        "data_dir": "/tmp/vector-forwarding",
        "sources": {"haproxy_log": copy.deepcopy(config["sources"]["haproxy_log"])},
        "sinks": {"stdout": copy.deepcopy(config["sinks"]["stdout"])},
    }
    forwarding["sources"]["haproxy_log"]["path"] = "/probe/haproxy.sock"
    if compact:
        forwarding["transforms"] = {"log_drop_empty": copy.deepcopy(config["transforms"]["log_drop_empty"])}
    else:
        forwarding["sinks"]["stdout"]["inputs"] = ["haproxy_log"]
    (work / f"forwarding-{str(compact).lower()}.yaml").write_text(yaml.safe_dump(forwarding, sort_keys=False))

transforms = config["transforms"]
for transform in transforms.values():
    transform["inputs"] = ["test_access_log" if name == "haproxy_log" else name
                           for name in transform["inputs"]]
transforms["test_access_log"] = {"type": "remap", "inputs": ["haproxy_log"], "source": ". = ."}
metric_transforms = [name for name, value in transforms.items() if value["type"] == "log_to_metric"]
for name in metric_transforms:
    transforms["observe_" + name] = {"type": "metric_to_log", "inputs": [name]}

def output(name, source):
    return {"extract_from": name, "conditions": [{"type": "vrl", "source": source}]}


def case(name, message, outputs=(), absent=()):
    return {
        "name": name,
        "inputs": [{"insert_at": "test_access_log", "type": "log", "log_fields": {"message": message}}],
        "outputs": list(outputs),
        "no_outputs_from": list(absent),
    }


base = {
    "status": 200, "method": "GET", "resource": "team/route", "service": "echo",
    "instance_pod": "haproxy-1", "host": "example.com", "route": "example.com/api/*",
    "server": "echo-abc123",
    "term": "----", "bytes": 4096, "bytes_in": 128,
    "total_time_ms": 25, "request_time_ms": 2, "queue_time_ms": 3,
    "connect_time_ms": 4, "response_time_ms": 5,
    "rate_limit_degraded": "1", "waf_degraded": "2", "schema_degraded": "0",
    "denied_by": "rate_limit_shared",
}

checks = [output("request_metrics_base", '\n'.join([
    'assert_eq!(.rm_status, "200")', 'assert_eq!(.rm_ns, "team")',
    'assert_eq!(.rm_ing, "route")', 'assert_eq!(.rm_svc, "echo")',
    'assert_eq!(.rm_method, "GET")', 'assert_eq!(.rm_pod, "haproxy-1")',
    'assert_eq!(.rm_host, "example.com")', 'assert_eq!(.rm_path, "/api/*")',
    'assert_eq!(.rm_term, "----")', 'assert_eq!(.rm_bytes, 4096)',
    'assert_eq!(.rm_bytes_in, 128)', 'assert_eq!(.rm_req_dur, 0.025)',
    'assert_eq!(.rm_upstream, 0.020)', 'assert_eq!(.rm_connect, 0.004)',
    'assert_eq!(.rm_header, 0.005)',
    'assert_eq!(.rm_server, "echo-abc123")' if server_label_enabled else 'assert!(!exists(.rm_server))',
]))]
for key, metric, value in [
    ("rateLimitDegraded", "degraded_rate_limit_total", 1),
    ("wafDegraded", "degraded_waf_total", 2),
    ("schemaDegraded", "degraded_schema_total", 0),
]:
    checks.append(output("observe_" + key + "_metric", '\n'.join([
        f'assert_eq!(.name, "{metric}")', 'assert_eq!(.namespace, "haptic")',
        f'assert_eq!(.counter.value, {value}.0)', 'assert_eq!(.kind, "incremental")',
    ])))
checks.append(output("observe_deniedBy_metric", '\n'.join([
    'assert_eq!(.name, "denied_total")', 'assert_eq!(.counter.value, 1.0)',
    'assert_eq!(.tags.reason, "rate_limit_shared")',
])))

for transform, values in [
    ("request_metrics_core", {"requests_total": 1.0, "requests": 1.0, "request_duration_seconds": 0.025}),
    ("request_metrics_sizes", {"request_size": 128.0, "response_size": 4096.0}),
    ("request_metrics_connect", {"connect_duration_seconds": 0.004}),
    ("request_metrics_response", {"header_duration_seconds": 0.005, "response_duration_seconds": 0.020}),
]:
    checks.append(output("observe_" + transform, '\n'.join([
        'assert_eq!(.namespace, "haptic_ingress_controller")',
        'assert_eq!(.tags.status, "200")', 'assert_eq!(.tags.namespace, "team")',
        'assert_eq!(.tags.ingress, "route")', 'assert_eq!(.tags.service, "echo")',
        'assert_eq!(.tags.method, "GET")', 'assert_eq!(.tags.controller_pod, "haproxy-1")',
        'assert_eq!(.tags.server, "echo-abc123")' if server_label_enabled else 'assert!(!exists(.tags.server))',
        f'expected = {json.dumps(values)}',
        'value = get!(expected, [string!(.name)])',
        'if .name == "requests" || .name == "requests_total" { assert_eq!(.counter.value, value) } else {',
        '  assert_eq!(.distribution.samples[0].value, value)',
        '  assert_eq!(.distribution.samples[0].rate, 1)',
        '  assert_eq!(.distribution.statistic, "histogram")',
        '}',
    ])))

cases = [case("request fields and feature counters", json.dumps(base), checks)]
for name, server in [("no selected server", "<NOSRV>"), ("empty server", ""),
                     ("non-string server", None), ("missing server", None)]:
    record = dict(base, server=server)
    if name == "missing server":
        del record["server"]
    server_checks = [output("request_metrics_base",
                            'assert_eq!(.rm_server, "")' if server_label_enabled
                            else 'assert!(!exists(.rm_server))')]
    for transform in ["request_metrics_core", "request_metrics_sizes",
                      "request_metrics_connect", "request_metrics_response"]:
        server_checks.append(output("observe_" + transform,
                                    'assert_eq!(.tags.server, "")' if server_label_enabled
                                    else 'assert!(!exists(.tags.server))'))
    cases.append(case(name, json.dumps(record), server_checks))

for name, text in [
    ("process message", "[NOTICE] worker started"),
    ("truncated JSON", '{"status": 200'),
    ("non-object JSON", '[{"status": 200}]'),
]:
    cases.append(case(name, text, absent=["request_metrics_base"] + metric_transforms))
    cases.append({
        "name": name + " remains logged",
        "inputs": [{"insert_at": "log_drop_empty", "type": "log", "log_fields": {"message": text}}],
        "outputs": [output("log_drop_empty", f'assert_eq!(.message, {json.dumps(text)})')],
    })

cases.append(case("TCP record has no request metrics", json.dumps({"frontend": "tcp", "bytes": 100}),
                  absent=["request_metrics_base"] + metric_transforms))
cases.append(case("denial has no backend timer samples", json.dumps(dict(base, status=403,
                  connect_time_ms=-1, response_time_ms=-1)),
                  outputs=[output("request_metrics_base", 'assert_eq!(.rm_status, "403")')],
                  absent=["request_metrics_connect", "request_metrics_response"]))
for invalid in ["", "unknown", "rate_limit_suffix", 'rate_limit"', 1, None]:
    cases.append(case("bounded denial label " + repr(invalid), json.dumps(dict(base, denied_by=invalid)),
                      absent=["deniedBy_metric"]))
for invalid in ["", "-1", "1.5", "1e3", "1suffix", 1, None]:
    cases.append(case("numeric degradation " + repr(invalid), json.dumps(dict(base, waf_degraded=invalid)),
                      absent=["wafDegraded_metric"]))
for compact in [True, False]:
    message = json.dumps(base, separators=(",", ":") if compact else None)
    cases.append(case("JSON whitespace " + str(compact), message, checks))

config["tests"] = cases
(work / "vector.yaml").write_text(yaml.safe_dump(config, sort_keys=False))
print(f"Testing {len(cases)} cases against the rendered Vector pipeline")
