from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[2]
CHART = ROOT / "charts/haptic"


class ChartTestContextTests(unittest.TestCase):
    def test_release_identity_matches_deployed_resources(self):
        cases = [
            ("default", "r", {}, False),
            ("name", "r", {"nameOverride": "edge"}, False),
            ("fullname", "r", {"fullnameOverride": "edge"}, False),
            ("both", "r", {"nameOverride": "proxy", "fullnameOverride": "edge"}, False),
            ("contained", "team-edge", {"nameOverride": "edge"}, False),
            ("truncated", "r", {"fullnameOverride": "edge-" + "x" * 59}, False),
            ("alias", "r", {}, True),
            ("alias-name", "r", {"nameOverride": "proxy"}, True),
            ("alias-fullname", "r", {"fullnameOverride": "edge"}, True),
        ]
        with tempfile.TemporaryDirectory(prefix="haptic-test-context-") as directory:
            parent = Path(directory)
            dependency = parent / "charts/haptic"
            shutil.copytree(CHART, dependency)
            version = yaml.safe_load((CHART / "Chart.yaml").read_text())["version"]
            (parent / "Chart.yaml").write_text(yaml.safe_dump({
                "apiVersion": "v2", "name": "parent", "version": "0.1.0",
                "dependencies": [{"name": "haptic", "version": version, "alias": "edge"}],
            }))
            values_file = parent / "test-values.yaml"
            for label, release, overrides, aliased in cases:
                with self.subTest(case=label):
                    values = {
                        **overrides,
                        "rateLimit": {"shared": {"enabled": True}},
                        "cache": {"varnish": {"enabled": True}},
                        "haproxy": {"ports": {"http": 8088}},
                        "controller": {"config": {"templatingSettings": {"extraContext": {
                            "tracing": {"enabled": True, "sampleRate": 19,
                                        "otlp": {"endpoint": "http://collector:4318"}},
                        }}}},
                    }
                    values_file.write_text(yaml.safe_dump({"edge": values} if aliased else values))
                    rendered = subprocess.run(
                        ["helm", "template", release, str(parent if aliased else CHART),
                         "--namespace", "tenant", "--values", str(values_file)],
                        check=True, capture_output=True, text=True,
                    )
                    documents = list(yaml.load_all(
                        rendered.stdout, Loader=getattr(yaml, "CSafeLoader", yaml.SafeLoader),
                    ))
                    self.assert_context(documents)

    def assert_context(self, documents):
        config, = [doc for doc in documents if doc and doc.get("kind") == "HAProxyTemplateConfig"]
        settings = config["spec"]["templatingSettings"]
        deployed = settings["extraContext"]
        tested = settings["testExtraContext"]
        workloads = {
            doc["spec"]["selector"]["matchLabels"]["app.kubernetes.io/component"]: doc
            for doc in documents if doc and doc.get("kind") == "Deployment"
        }
        haproxy = workloads["loadbalancer"]
        controller = workloads["controller"]
        for context in (deployed, tested):
            self.assertEqual(context["haproxyServiceName"], haproxy["metadata"]["name"])
            self.assertEqual(context["tracing"]["otlp"]["deploymentName"], haproxy["metadata"]["name"])
            self.assertEqual(context["haproxyPodSelector"], haproxy["spec"]["selector"]["matchLabels"])
            self.assertEqual(context["haproxyPodSelector"]["app.kubernetes.io/name"],
                             controller["spec"]["selector"]["matchLabels"]["app.kubernetes.io/name"])
            self.assertEqual(context["controllerNamespace"], "tenant")
            for component, name in (
                ("rate-limit-store", context["rateLimit"]["shared"]["managedStore"]["serviceName"]),
                ("cache", context["cache"]["varnish"]["serviceName"]),
            ):
                policy, = [doc for doc in documents if doc and doc.get("kind") == "NetworkPolicy"
                           and doc["metadata"]["labels"].get("app.kubernetes.io/component") == component]
                self.assertEqual(name, policy["metadata"]["name"])
        self.assertEqual(deployed["httpPort"], 8088)
        self.assertEqual(tested["httpPort"], 80)
        self.assertEqual(deployed["tracing"]["sampleRate"], 19)
        self.assertEqual(tested["tracing"]["sampleRate"], 100)


if __name__ == "__main__":
    unittest.main()
