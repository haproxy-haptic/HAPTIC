#!/usr/bin/env python3
"""Fail when an Ingress annotation renders raw HAProxy configuration past the switch.

Every annotation an enabled library declares in _migrationCoverage gets one
Ingress carrying a raw directive. With extraContext.ingressPermissions
.allowRawHAProxyConfig=false the directive must not reach haproxy.cfg, and the
gate must reject each annotation it lists; with the switch on, each listed
annotation must render the directive, so the probe can't pass vacuously.

  raw-annotation-probe.py generate <rendered chart> <probe config>  (prints the test count)
  raw-annotation-probe.py report <validate --output json> <test count>
"""

import json
import re
import sys

import yaml

GATE = "features-951-raw-haproxy-config-permission"
DIRECTIVE = "http-request set-var(txn.haptic_raw_probe) str(1)"
# The second copy catches a single-line annotation whose value can start a new line.
PROBE_VALUE = DIRECTIVE + "\n" + DIRECTIVE
DIRECTIVE_LINE = r"(?m)^\s*" + re.escape(DIRECTIVE)
NEVER_IN_AN_ERROR = "raw-annotation-probe-never-matches"
UNGATED = "raw-probe/ungated/"
GATED_OFF = "raw-probe/gated-off/"
GATED_ON = "raw-probe/gated-on/"
LOADER = getattr(yaml, "CSafeLoader", yaml.SafeLoader)
DUMPER = getattr(yaml, "CSafeDumper", yaml.SafeDumper)


def fail(message):
    print("raw-annotation probe: " + message, file=sys.stderr)
    sys.exit(1)


def spec(doc):
    return (doc or {}).get("spec") or {}


def known_annotations(docs):
    keys = set()
    for doc in docs:
        context = (spec(doc).get("templatingSettings") or {}).get("extraContext") or {}
        for source in (context.get("knownAnnotations") or {}).values():
            for prefix in source.get("prefixes") or []:
                keys.update(prefix + name for name in source.get("names") or [])
    return keys


def gated_annotations(docs):
    for doc in docs:
        snippet = (spec(doc).get("templateSnippets") or {}).get(GATE)
        if snippet:
            paths = (snippet.get("incremental") or {}).get("whenAnyPathExists") or []
            keys = [re.fullmatch(r"metadata\.annotations\['([^']+)'\]", path) for path in paths]
            if not paths or not all(keys):
                fail(f"{GATE} must list metadata.annotations['<key>'] paths in whenAnyPathExists")
            return {key.group(1) for key in keys}
    fail(f"the rendered chart has no {GATE} snippet")


def fixtures(annotation):
    return {
        "services": [{
            "apiVersion": "v1", "kind": "Service",
            "metadata": {"namespace": "default", "name": "raw-probe"},
            "spec": {"ports": [{"name": "http", "port": 80, "targetPort": 8080}]},
        }],
        "endpoints": [{
            "apiVersion": "discovery.k8s.io/v1", "kind": "EndpointSlice",
            "metadata": {"namespace": "default", "name": "raw-probe-1",
                         "labels": {"kubernetes.io/service-name": "raw-probe"}},
            "addressType": "IPv4",
            "endpoints": [{"addresses": ["10.0.0.10"], "conditions": {"ready": True}}],
            "ports": [{"name": "http", "port": 8080}],
        }],
        "ingresses": [{
            "apiVersion": "networking.k8s.io/v1", "kind": "Ingress",
            "metadata": {"namespace": "default", "name": "raw-probe", "annotations": {annotation: PROBE_VALUE}},
            "spec": {"ingressClassName": "haptic", "rules": [{"host": "raw-probe.example.com", "http": {"paths": [{
                "path": "/", "pathType": "Prefix",
                "backend": {"service": {"name": "raw-probe", "port": {"number": 80}}},
            }]}}]},
        }],
    }


def probe(annotation, allowed, assertion):
    return {
        "description": annotation,
        "extraContext": {
            "ingressPermissions": {"allowRawHAProxyConfig": allowed},
            "annotationLibraries": {"haptic": True, "nginx": True, "haproxyIngress": True, "haproxytech": True},
        },
        "fixtures": fixtures(annotation),
        # A rendering_error assertion lets a failed render pass, and skips the deployment-values pass.
        "assertions": [
            assertion,
            {"type": "not_contains", "target": "rendering_error", "pattern": NEVER_IN_AN_ERROR},
        ],
    }


def generate(rendered, output):
    with open(rendered) as stream:
        docs = [doc for doc in yaml.load_all(stream, Loader=LOADER) if doc]
    known = known_annotations(docs)
    gated = gated_annotations(docs)
    if not known:
        fail("the rendered chart declares no knownAnnotations; enable the annotation libraries")
    if gated - known:
        fail(f"{GATE} lists annotations no enabled library declares: {', '.join(sorted(gated - known))}")

    tests = {}
    for annotation in sorted(known):
        if annotation in gated:
            rejected = r"sets [^\n]*'" + re.escape(annotation) + r"'[^\n]*, but extraContext\.ingressPermissions\.allowRawHAProxyConfig=false"
            tests[GATED_OFF + annotation] = probe(annotation, False, {
                "type": "contains", "target": "rendering_error", "pattern": rejected})
            tests[GATED_ON + annotation] = probe(annotation, True, {
                "type": "contains", "target": "haproxy.cfg", "pattern": DIRECTIVE_LINE})
        else:
            tests[UNGATED + annotation] = probe(annotation, False, {
                "type": "not_contains", "target": "haproxy.cfg", "pattern": DIRECTIVE_LINE})

    config = None
    for doc in docs:
        existing = spec(doc).get("validationTests")
        if existing:
            spec(doc)["validationTests"] = {name: test for name, test in existing.items() if name == "_global"}
        if doc.get("kind") == "HAProxyTemplateConfig":
            config = doc
    if config is None:
        fail("the rendered chart has no HAProxyTemplateConfig")
    config["spec"].setdefault("validationTests", {}).update(tests)
    with open(output, "w") as stream:
        yaml.dump_all(docs, stream, Dumper=DUMPER, sort_keys=False)
    print(len(tests))


def report(results_path, expected):
    try:
        with open(results_path) as stream:
            results = json.load(stream)
    except (OSError, ValueError) as error:
        fail(f"haptic validate produced no JSON results ({error})")
    probes = [test for test in results.get("tests") or [] if test["testName"].startswith("raw-probe/")]
    if len(probes) != expected:
        fail(f"expected {expected} probe tests, haptic validate ran {len(probes)}")
    failed = {UNGATED: [], GATED_OFF: [], GATED_ON: []}
    for test in probes:
        if not test["passed"]:
            prefix = next(prefix for prefix in failed if test["testName"].startswith(prefix))
            detail = ": " + test["renderError"] if test.get("renderError") else ""
            failed[prefix].append(test["testName"][len(prefix):] + detail)
    explanations = {
        UNGATED: f"render a raw directive into haproxy.cfg with allowRawHAProxyConfig=false. Add them to {GATE} in ingress-annotations-compat.",
        GATED_OFF: f"are listed in {GATE}, but it didn't reject them with allowRawHAProxyConfig=false.",
        GATED_ON: f"are listed in {GATE}, but don't render the directive with allowRawHAProxyConfig=true. Remove stale entries.",
    }
    for prefix, annotations in failed.items():
        if annotations:
            print("These annotations " + explanations[prefix], file=sys.stderr)
            for annotation in annotations:
                print("  " + annotation, file=sys.stderr)
    if any(failed.values()):
        sys.exit(1)
    print(f"raw-annotation probe: {len(probes)} probes passed", file=sys.stderr)


def main(argv):
    if len(argv) == 4 and argv[1] == "generate":
        generate(argv[2], argv[3])
    elif len(argv) == 4 and argv[1] == "report":
        report(argv[2], int(argv[3]))
    else:
        fail(__doc__)


main(sys.argv)
