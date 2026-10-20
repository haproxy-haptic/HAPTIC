#!/usr/bin/env python3
"""Exercise the shipped disabled-listener assertions with HAPTIC's regex engine."""

import json
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
PREFIX = "defaults\n  mode tcp\n  timeout connect 1s\n  timeout client 1s\n  timeout server 1s\n"
BACKEND = "backend echo\n  server echo 127.0.0.1:9\n"
GROUPS = [
    ("base", "http", ["http", "http-tcp", "http_frontend", "http_frontend_h2c"]),
    ("ssl", "https", ["https", "ssl-tcp"]),
]


def frontend(name, bind):
    return f"frontend {name}\n  bind {bind}\n  default_backend echo\n"


def check_case(directory, name, content, assertion, rejected):
    checks = unittest.TestCase()
    candidate = directory / f"{name}.json"
    candidate.write_text(json.dumps({
        "apiVersion": "haproxy-haptic.org/v1alpha1",
        "kind": "HAProxyTemplateConfig",
        "metadata": {"name": "proxy-bind-assertion"},
        "spec": {
            "podSelector": {"matchLabels": {"app": "example"}},
            "watchedResources": {"configmaps": {"apiVersion": "v1", "resources": "configmaps"}},
            "haproxyConfig": {"template": PREFIX + content + BACKEND},
            "validationTests": {name: {"assertions": [
                {"type": "haproxy_valid"}, assertion,
            ]}},
        },
    }))
    result = subprocess.run([
        str(ROOT / "bin/haptic"), "validate", "--file", str(candidate), "--output", "json",
    ], check=False, capture_output=True, text=True)
    checks.assertTrue(result.stdout, result.stderr)
    output = json.loads(result.stdout)
    checks.assertEqual(result.returncode, 1 if rejected else 0, result.stderr)
    checks.assertEqual(output["totalTests"], 1, output)
    checks.assertEqual(output["failedTests"], int(rejected), output)
    assertions = output["tests"][0]["assertions"]
    checks.assertTrue(assertions[0]["Passed"], assertions)
    checks.assertIs(assertions[1]["Passed"], not rejected, assertions)
    if rejected:
        checks.assertIn("unexpectedly found", assertions[1]["Error"], assertions)
    print(f"PASS {name}: {'rejected' if rejected else 'accepted'}")


def main():
    with tempfile.TemporaryDirectory(prefix="haptic-proxy-assertions-") as temporary:
        directory = Path(temporary)
        for library, protocol, names in GROUPS:
            source = ROOT / f"charts/haptic/charts/{library}/library.yaml"
            query = f'.validationTests["test-proxy-protocol-{protocol}-bind-disabled"].assertions[] | select(.type == "not_contains")'
            assertion = json.loads(subprocess.check_output(["yq", "-o=json", query, str(source)], text=True))
            for name in names:
                check_case(directory, f"reject-{name}", frontend(name, "*:8081 accept-proxy"), assertion, True)
                commented = frontend(name, "*:8081 accept-proxy").replace("\n  bind", "\n# operator comment\n  bind")
                check_case(directory, f"reject-comment-{name}", commented, assertion, True)
                regular = frontend(name, "*:8080")
                custom = frontend("platform-tcp", "*:2222 accept-proxy")
                check_case(directory, f"custom-after-{name}", regular + custom, assertion, False)
                check_case(directory, f"custom-before-{name}", custom + regular, assertion, False)
                check_case(directory, f"loopback-{name}", frontend(name, "unix@/tmp/haptic-example.sock accept-proxy"), assertion, False)
            boundary = frontend(names[0], "*:8080") + "backend boundary\n  mode tcp\n" + frontend("platform-tcp", "*:2222 accept-proxy")
            check_case(directory, f"backend-boundary-{protocol}", boundary, assertion, False)


if __name__ == "__main__":
    main()
