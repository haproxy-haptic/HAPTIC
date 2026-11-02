#!/usr/bin/env python3
"""Reject a rendered CR carrying a spec field its own CRD does not declare.

`spec` is seeded from `controller.config` wholesale, so any values key added
under it lands in the object. The apiserver applies these with server-side
apply against a *typed* schema, so an undeclared field is not ignored — it
fails the apply with "field not declared in schema" and takes `helm upgrade`
down with it. That has shipped once.

kubeconform cannot catch it: the chart's own CRDs are not in any public schema
catalog, so `make lint-chart` skips exactly these kinds. This closes that gap
by checking the rendered objects against the CRDs the chart itself ships.
"""

import subprocess
import sys
from pathlib import Path

try:
    import yaml
except ModuleNotFoundError:  # pragma: no cover - environment guard
    sys.exit(
        "check-cr-spec-conformance.py needs PyYAML: pip install pyyaml "
        "(the CI image gets it transitively today, which is not something to rely on)"
    )


def crd_spec_properties(crd_dir: Path) -> dict[str, set[str]]:
    """kind -> the spec properties its CRD declares."""
    declared: dict[str, set[str]] = {}
    for path in sorted(crd_dir.glob("*.yaml")):
        for doc in yaml.safe_load_all(path.read_text()):
            if not doc or doc.get("kind") != "CustomResourceDefinition":
                continue
            kind = doc["spec"]["names"]["kind"]
            for version in doc["spec"]["versions"]:
                schema = version.get("schema", {}).get("openAPIV3Schema", {})
                spec = schema.get("properties", {}).get("spec", {})
                # A spec that accepts anything cannot leak; skip it rather than
                # report every field as undeclared.
                if spec.get("x-kubernetes-preserve-unknown-fields"):
                    continue
                declared.setdefault(kind, set()).update(spec.get("properties", {}))
    return declared


def crd_spec_schemas(crd_dir: Path) -> dict[str, dict]:
    """kind -> its full spec schema, for constraint checking."""
    schemas: dict[str, dict] = {}
    for path in sorted(crd_dir.glob("*.yaml")):
        for doc in yaml.safe_load_all(path.read_text()):
            if not doc or doc.get("kind") != "CustomResourceDefinition":
                continue
            kind = doc["spec"]["names"]["kind"]
            for version in doc["spec"]["versions"]:
                schema = version.get("schema", {}).get("openAPIV3Schema", {})
                spec = schema.get("properties", {}).get("spec", {})
                if spec:
                    schemas.setdefault(kind, spec)
    return schemas


def constraint_violations(value, schema: dict, path: str):
    """Yield the constraint breaches that make an object unstorable.

    Only the ones the apiserver rejects outright and a chart can plausibly
    render: an empty string where minLength is 1, a missing required key, an
    empty list where minItems is 1, a value outside an enum. A rendered CR that
    trips one of these fails `helm install` for every operator, and the failure
    surfaces as an apiserver message with no pointer back to the template.
    """
    if schema.get("x-kubernetes-preserve-unknown-fields") and "properties" not in schema:
        return
    if isinstance(value, str):
        if len(value) < schema.get("minLength", 0):
            yield f"{path}: is empty but minLength is {schema['minLength']}"
        enum = schema.get("enum")
        if enum and value not in enum:
            yield f"{path}: {value!r} is not one of {enum}"
        return
    if isinstance(value, list):
        if len(value) < schema.get("minItems", 0):
            yield f"{path}: has {len(value)} items but minItems is {schema['minItems']}"
        items = schema.get("items")
        if isinstance(items, dict):
            for i, item in enumerate(value):
                yield from constraint_violations(item, items, f"{path}[{i}]")
        return
    if isinstance(value, dict):
        for key in schema.get("required", []):
            if key not in value:
                yield f"{path}.{key}: required by the CRD but absent"
        props = schema.get("properties") or {}
        extra = schema.get("additionalProperties")
        for key, sub in value.items():
            if key in props:
                yield from constraint_violations(sub, props[key], f"{path}.{key}")
            elif isinstance(extra, dict):
                yield from constraint_violations(sub, extra, f"{path}[{key!r}]")


def render(chart: Path, extra: list[str]) -> list[dict]:
    proc = subprocess.run(
        ["helm", "template", str(chart), "--namespace", "haptic", *extra],
        capture_output=True,
        text=True,
        check=False,
    )
    if proc.returncode != 0:
        # Surface helm's own message: a template error here is the operator's
        # bug to fix, and a Python traceback hides it.
        sys.exit(f"helm template failed ({proc.returncode}):\n{proc.stderr.strip()}")
    return [d for d in yaml.safe_load_all(proc.stdout) if d]


def main() -> int:
    if len(sys.argv) < 2:
        sys.exit(f"usage: {Path(sys.argv[0]).name} <chart-dir> [helm template args...]")
    chart = Path(sys.argv[1])
    extra = sys.argv[2:]
    crd_dir = chart / "charts" / "haptic-crds" / "crds"
    declared = crd_spec_properties(crd_dir)
    if not declared:
        print(f"no CRDs with a closed spec schema under {crd_dir}", file=sys.stderr)
        return 1

    schemas = crd_spec_schemas(crd_dir)
    failures = []
    breaches = []
    checked = 0
    for obj in render(chart, extra):
        kind = obj.get("kind")
        if kind not in declared or not isinstance(obj.get("spec"), dict):
            continue
        checked += 1
        name = obj.get("metadata", {}).get("name", "<unnamed>")
        undeclared = sorted(set(obj["spec"]) - declared[kind])
        if undeclared:
            failures.append((kind, name, undeclared))
        if kind in schemas:
            found = list(constraint_violations(obj["spec"], schemas[kind], "spec"))
            if found:
                breaches.append((kind, name, found))

    for kind, name, fields in failures:
        print(f"  ✗ {kind}/{name} carries spec fields its CRD does not declare:")
        for f in fields:
            print(f"      .spec.{f}")

    for kind, name, found in breaches:
        print(f"  ✗ {kind}/{name} breaks constraints its own CRD declares:")
        for v in found:
            print(f"      {v}")

    if failures:
        print()
        print("Server-side apply rejects these outright, so `helm upgrade` fails for")
        print("every operator. If the field is a chart-time-only switch, unset it from")
        print("$config in templates/haproxytemplateconfig.yaml; if it is real API,")
        print("add it to the Go type and regenerate the CRD.")
    if breaches:
        print()
        print("The apiserver refuses to store these, so `helm install` fails before the")
        print("controller ever starts. Fix the template that renders the value — the")
        print("CRD constraint is the contract, not the thing to relax.")
    if failures or breaches:
        return 1

    print(f"  ✓ OK ({checked} object(s) conform to the CRDs the chart ships)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
