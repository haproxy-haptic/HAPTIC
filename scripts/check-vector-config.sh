#!/usr/bin/env bash
#
# Assert the chart renders a vector.yaml that vector can actually load.
#
# Nothing else covers this. The chart's validationTests match regexes against the
# rendered text, and the span harness slices out only the VRL — neither notices
# that the document as a whole stopped being YAML. The failure mode is quiet and
# expensive: one template line indented past the block scalar's base ends the
# scalar early, vector rejects the config and keeps its bootstrap one (stdout
# sink, no metrics exporter), so the readiness probe never passes and the
# DaemonSet rollout wedges — with every offline gate reporting green.
#
# Runs on any chart change. `make lint-chart` invokes it.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHART="$REPO/charts/haptic"
CONTROLLER_BIN="${CONTROLLER_BIN:-$REPO/bin/haptic}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

fail() { echo -e "\033[0;31m$*\033[0m" >&2; exit 1; }

[ -x "$CONTROLLER_BIN" ] || fail "controller binary not found at $CONTROLLER_BIN — run 'make build'"

# Tracing on: the span transform is dropped from the config without an OTLP
# endpoint, and it is the part most likely to break the document.
helm template "$CHART" --namespace default \
  --set controller.config.templatingSettings.extraContext.tracing.enabled=true \
  --set controller.config.templatingSettings.extraContext.tracing.otlp.endpoint=http://tempo:4318/v1/traces \
  2>/dev/null \
  | yq 'select(.kind == "HAProxyTemplateConfig" or .kind == "HAProxyTemplateLibrary")' \
  > "$WORK/config.yaml" || fail "helm template failed"

"$CONTROLLER_BIN" validate --file "$WORK/config.yaml" --schema-dir "$REPO/tests/schemas" \
  --test test-vector-omit-empty-log-fields --dump-rendered > "$WORK/dump.txt" 2>/dev/null \
  || fail "rendering the config failed"

python3 - "$WORK/dump.txt" <<'PY' || exit 1
import pathlib, sys, yaml

dump = pathlib.Path(sys.argv[1]).read_text()
marker = "#### vector.yaml"
if marker not in dump:
    sys.exit("no rendered vector.yaml in the dump — did --dump-rendered change?")

body = []
for line in dump[dump.index(marker) + len(marker):].split("\n"):
    if line.startswith(("### ", "#### ", "=== ")):
        break
    if line and set(line) == {"-"}:      # the dump's own section rules
        continue
    body.append(line)

try:
    doc = yaml.safe_load("\n".join(body))
except yaml.YAMLError as e:
    sys.exit(f"the rendered vector.yaml is not valid YAML — vector would refuse it:\n{e}")

if not isinstance(doc, dict):
    sys.exit("the rendered vector.yaml did not parse to a mapping")

# A truncated block scalar still parses; it just loses everything after the cut.
for key in ("sources", "transforms", "sinks"):
    if key not in doc:
        sys.exit(f"the rendered vector.yaml has no '{key}' section — the block scalar likely ended early")

# Parsing is not enough: a scalar that ends early still yields a valid document,
# just one missing everything after the cut. This render sets an OTLP endpoint,
# so the span pipeline must be in it.
if "spans" not in doc["transforms"]:
    sys.exit("no 'spans' transform despite an OTLP endpoint — the span pipeline was lost")
if "otlp_traces" not in doc["sinks"]:
    sys.exit("no 'otlp_traces' sink despite an OTLP endpoint")

print(f"  rendered vector.yaml is loadable: "
      f"{len(doc['sources'])} sources, {len(doc['transforms'])} transforms, {len(doc['sinks'])} sinks")

# Same class of failure, TOML flavor: the spoa-hub validator parses the
# rendered TOML at admission time, but nothing offline did — a fused line
# survived every chart gate and surfaced as a webhook denial in e2e. Parse
# every rendered .toml section here. tomllib is stdlib since Python 3.11.
import re, tomllib
toml_sections = re.findall(r"^#### (\S+\.toml)$", dump, re.M)
if "spoa-hub-config.toml" not in toml_sections:
    sys.exit("no rendered spoa-hub-config.toml in the dump — the TOML parse check would run on nothing")
for name in toml_sections:
    start = dump.index(f"#### {name}") + len(f"#### {name}")
    body = []
    for line in dump[start:].split("\n"):
        if line.startswith(("### ", "#### ", "=== ")):
            break
        if line and set(line) == {"-"}:
            continue
        body.append(line)
    try:
        tomllib.loads("\n".join(body))
    except tomllib.TOMLDecodeError as e:
        sys.exit(f"the rendered {name} is not valid TOML — the spoa-hub would refuse it:\n{e}")
    print(f"  rendered {name} is valid TOML")
PY

echo "✓ vector config OK"
