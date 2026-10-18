#!/usr/bin/env bash
set -euo pipefail

# Render config and library objects with Helm before validating their combined spec.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
CHART_DIR="${PROJECT_ROOT}/charts/haptic"
CONTROLLER_BIN="${CONTROLLER_BIN:-${PROJECT_ROOT}/bin/haptic}"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Print warning message
warn() {
    echo -e "${YELLOW}Warning: $1${NC}" >&2
}

# Assert that one Helm values combination is rejected with the expected text.
# Some ownership-migration checks deliberately add a key absent from
# values.yaml; helm-unittest cannot preserve those unknown descendants through
# its per-leaf `set` merger, while Helm itself does. Keep these checks on the
# real `helm template --set` path operators use.
run_helm_failure_guard() {
    local label=$1
    local expected=$2
    shift 2
    local guard_err
    guard_err=$(mktemp /tmp/haptic-helm-guard-XXXXXX.log)
    echo -e "${YELLOW}${label}...${NC}" >&2
    if helm template "$CHART_DIR" --namespace default $HAPROXY_VERSION_ARG "$@" > /dev/null 2> "$guard_err"; then
        echo -e "${RED}Error: ${label} did not fail${NC}" >&2
        rm -f "$guard_err"
        exit 1
    fi
    if ! grep -Fq "$expected" "$guard_err"; then
        echo -e "${RED}Error: ${label} returned unexpected error:${NC}" >&2
        cat "$guard_err" >&2
        rm -f "$guard_err"
        exit 1
    fi
    rm -f "$guard_err"
}

# True when the caller selected a single test with `--test NAME`. Matching the
# exact argument rather than a substring of "$*": a value belonging to another
# flag (a path, a label) that merely contains the text `--test` must not suppress
# the whole-suite extras below.
single_test_requested() {
    local arg
    for arg in "$@"; do
        [[ "$arg" == "--test" ]] && return 0
    done
    return 1
}

usage() {
    cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Test HAProxy template libraries by rendering the merged Helm chart and running
validation tests.

This script automates the correct workflow:
  1. helm template to render the selected libraries
  2. yq to extract HAProxyTemplateConfig and HAProxyTemplateLibrary objects
  3. controller validate with offline schemas to run tests

OPTIONS:
  --test NAME           Run specific test by name; repeat to select several tests
  --workers N           Number of parallel test workers (0=automatic CPU and memory budget, 1=sequential, default: 0)
  --dump-rendered       Dump all rendered content
  --verbose             Show rendered content preview for failed assertions
  --trace-templates     Show template execution trace (top-level; use with --profile-includes for full call tree)
  --profile-includes    Show include timing statistics (top 20 slowest)
  --output FORMAT       Output format: summary, json, yaml (default: summary)
  --help                Show this help message

ENVIRONMENT:
  HAPTIC_TEMPLATE_VALUES  Optional Helm values file for the initial render

EXAMPLES:
  # Run all validation tests
  $(basename "$0")

  # Run specific test
  $(basename "$0") --test test-httproute-method-matching

  # Run test with debugging output
  $(basename "$0") --test test-httproute-method-matching --dump-rendered

  # Show all available tests
  $(basename "$0") --output yaml | yq '.tests[].name'

  # Verbose output for failed assertions
  $(basename "$0") --test test-httproute-method-matching --verbose

  # Run with 8 parallel workers
  $(basename "$0") --workers 8

  # Run sequentially (for debugging)
  $(basename "$0") --workers 1

  # Profile include execution times
  $(basename "$0") --profile-includes

NOTES:
  - Gateway API tests require --api-versions flag (automatically included)
  - This is the recommended way to test template changes
  - Do NOT test library files directly - always test the merged output

EOF
    exit 0
}

# Check for help flag first
for arg in "$@"; do
    if [[ "$arg" == "--help" || "$arg" == "-h" ]]; then
        usage
    fi
done

# Check if controller binary exists
if [[ ! -x "$CONTROLLER_BIN" ]]; then
    echo -e "${RED}Error: Controller binary not found at $CONTROLLER_BIN${NC}" >&2
    echo "Run 'make build' first to build the controller" >&2
    exit 1
fi

# Refuse to run against a binary older than the source.
#
# A hard error rather than a warning because a stale binary does not fail
# recognisably: an older `validate` cannot parse the current chart's output and
# reports "no validation tests found in config", which reads like a chart bug
# and sends you looking in the wrong place entirely. The warning this replaced
# scrolled past in a few hundred lines of render output.
#
# CI is unaffected — `make validate-helm-libraries` depends on `build`.
# No `| grep -q`: grep exits at the first match, find takes SIGPIPE finishing its
# walk of pkg/, and pipefail turns that into a non-zero pipeline — so the check
# reads "not stale" exactly when files ARE stale, and the run proceeds on the
# binary this guard exists to reject.
stale_sources="$(find cmd/haptic pkg go.mod go.sum VERSION -newer "$CONTROLLER_BIN" 2>/dev/null || true)"
if [ -n "$stale_sources" ]; then
    echo -e "${RED}Error: $CONTROLLER_BIN is older than the source tree${NC}" >&2
    echo "Run 'make build', then re-run this script." >&2
    echo "(A stale binary reports 'no validation tests found in config' instead of failing recognisably.)" >&2
    exit 1
fi

# Check if helm is installed
if ! command -v helm &> /dev/null; then
    echo -e "${RED}Error: helm command not found${NC}" >&2
    echo "Install helm: https://helm.sh/docs/intro/install/" >&2
    exit 1
fi

# Check if yq is installed
if ! command -v yq &> /dev/null; then
    echo -e "${RED}Error: yq command not found${NC}" >&2
    echo "Install yq: https://github.com/mikefarah/yq" >&2
    exit 1
fi

# Check if chart directory exists
if [[ ! -d "$CHART_DIR" ]]; then
    echo -e "${RED}Error: Chart directory not found at $CHART_DIR${NC}" >&2
    exit 1
fi

# Create temporary file for merged config
TEMP_CONFIG=$(mktemp)
trap 'rm -f "$TEMP_CONFIG"' EXIT

# Render Helm chart with Gateway API support and extract HAProxyTemplateConfig.
# Use --namespace default for consistent behavior regardless of HELM_NAMESPACE
# env var (matches the _global fixtures which provide SSL certs in 'default').
#
# HAPROXY_VERSION env var is honored so CI's per-version matrix
# (the helm-3.x jobs) can render with the
# matching haproxyVersion value. When unset, the chart's values.yaml
# default applies.
HAPROXY_VERSION_ARG=""
if [[ -n "${HAPROXY_VERSION:-}" ]]; then
    HAPROXY_VERSION_ARG="--set haproxyVersion=${HAPROXY_VERSION}"
fi
# Render with operator values the bundled validationTests have broken on real
# deployments: a custom default-cert name, governance on with a global rule,
# session tickets on, non-default ports, overrides of keys that tests assert the
# chart default of, and a password-hash policy every hash fails. Assertions must
# not see any of them (they render with testExtraContext); the deployment-values
# pass must still render them.
TEMPLATE_VALUES_ARGS=()
if [[ -n "${HAPTIC_TEMPLATE_VALUES:-}" ]]; then
    if [[ ! -f "$HAPTIC_TEMPLATE_VALUES" ]]; then
        echo "HAPTIC_TEMPLATE_VALUES file does not exist: $HAPTIC_TEMPLATE_VALUES" >&2
        exit 1
    fi
    TEMPLATE_VALUES_ARGS=(--values "$HAPTIC_TEMPLATE_VALUES")
fi
echo -e "${YELLOW}Rendering Helm chart (operator-value isolation regression)...${NC}" >&2
if ! helm template "$CHART_DIR" \
    --namespace default \
    $HAPROXY_VERSION_ARG \
    "${TEMPLATE_VALUES_ARGS[@]}" \
    --set controller.templateLibraries.gateway.enabled=true \
    --set controller.templateLibraries.gateway.experimentalChannel=true \
    --set controller.templateLibraries.hapticAnnotations.enabled=true \
    --set controller.templateLibraries.haproxytech.enabled=true \
    --set controller.templateLibraries.haproxyIngress.enabled=true \
    --set controller.templateLibraries.nginxIngress.enabled=true \
    --set controller.templateLibraries.customCrdExample.enabled=true \
    --set controller.templateLibraries.kubernetesBackends.podReadinessGate.enabled=true \
    --set defaultSSLCertificate.secretName=regression-custom-rsa-cert \
    --set defaultSSLCertificate.ecdsaSecretName=regression-custom-ecdsa-cert \
    --set 'controller.config.templatingSettings.extraContext.tls.sessionTickets.enabled=true' \
    --set 'controller.config.templatingSettings.extraContext.ingressPermissions.allowRawHAProxyConfig=true' \
    --set 'controller.config.templatingSettings.extraContext.governance.enabled=true' \
    --set 'controller.config.templatingSettings.extraContext.governance.rules[0].resource=ingresses' \
    --set-string "controller.config.templatingSettings.extraContext.governance.rules[0].path=metadata.annotations['haproxy-haptic.org/waf-policy']" \
    --set 'controller.config.templatingSettings.extraContext.governance.rules[0].required=true' \
    --set 'controller.config.templatingSettings.extraContext.governance.rules[0].enforcement=audit' \
    --set haproxy.ports.http=9080 \
    --set haproxy.ports.https=9443 \
    --set controller.config.templatingSettings.extraContext.hardStopAfter=61m \
    --set controller.config.templatingSettings.extraContext.tune.bufsize=262144 \
    --set controller.config.templatingSettings.extraContext.sslRedirectDefault=true \
    --set controller.config.templatingSettings.extraContext.ingressDefaultSSLRedirect=true \
    --set 'controller.config.templatingSettings.extraContext.annotationCompatibility.basicAuth.passwordHashValidation.regex=^NOMATCH$' \
    | yq 'select(.kind == "HAProxyTemplateConfig" or .kind == "HAProxyTemplateLibrary")' \
    > "$TEMP_CONFIG"; then
    echo -e "${RED}Error: Failed to render Helm chart${NC}" >&2
    exit 1
fi

# Guard the CHART-DEFAULT coraza directive ordering. This must live HERE
# (not in the chart's validationTests): validationTests ship inside the CR
# and run at config load under WHATEVER values the deployment uses — an
# assertion about values.yaml defaults would reject perfectly valid configs
# whose values override the directives (dev-values does). This script always
# renders chart defaults for the coraza directives, so the check is exact:
# SecRuleEngine On must come AFTER the includes, because
# @coraza.conf-recommended itself sets DetectionOnly and a reorder would
# silently ship a detection-only WAF.
if ! python3 -c '
import re, sys
s = open(sys.argv[1]).read()
if "@coraza.conf-recommended" in s:
    if not re.search(r"Include @owasp_crs/\*\.conf\n\s*SecRuleEngine On", s):
        sys.exit(1)
sys.exit(0)
' "$TEMP_CONFIG"; then
    echo -e "${RED}Error: chart-default coraza directives are mis-ordered:${NC}" >&2
    echo "  'SecRuleEngine On' must come AFTER 'Include @coraza.conf-recommended'" >&2
    echo "  (the include sets SecRuleEngine DetectionOnly; a later On is required" >&2
    echo "  or the default WAF silently becomes detection-only)." >&2
    exit 1
fi


# Verify the config file is not empty
if [[ ! -s "$TEMP_CONFIG" ]]; then
    echo -e "${RED}Error: Rendered HAProxyTemplateConfig is empty${NC}" >&2
    echo "This usually means the Helm template didn't output a HAProxyTemplateConfig resource" >&2
    exit 1
fi

# Run controller validate with all provided arguments.
#
# `--schema-dir=tests/schemas` is auto-wired so typed-access in chart
# templates (the `gateways`, `httproutes`, ... top-level globals) compiles
# offline. Operators passing their own `--schema-dir` explicitly via
# `$@` override this default — the loop below skips the auto-wiring if
# either `--schema-dir` or `--schema-dir=...` appears in the forwarded
# args. The HAPTIC_SCHEMA_DIR env var (read by the validate CLI's flag
# default) is also a valid override and similarly skips the auto-wiring.
#
# An array (not a plain string) is used so an empty value contributes
# zero arguments without relying on word-splitting behaviour — and so a
# non-empty value passes through unchanged even if PROJECT_ROOT
# contains spaces. shellcheck SC2086 / SC2128 stay clean.
SCHEMA_DIR_ARGS=()
SCHEMA_DIR="${PROJECT_ROOT}/tests/schemas"
if [[ -d "$SCHEMA_DIR" && -z "${HAPTIC_SCHEMA_DIR:-}" ]] \
    && ! printf '%s\n' "$@" | grep -qE -- '^--schema-dir(=|$)'; then
    SCHEMA_DIR_ARGS=("--schema-dir=$SCHEMA_DIR")
fi

echo -e "${YELLOW}Running validation tests...${NC}" >&2
FULL_RC=0
"$CONTROLLER_BIN" validate --file "$TEMP_CONFIG" "${SCHEMA_DIR_ARGS[@]}" "$@" || FULL_RC=$?

if [[ $FULL_RC -eq 0 ]] && ! single_test_requested "$@"; then
    INGRESS_CONFIG=$(mktemp /tmp/haptic-ingress-without-hub-XXXXXX.yaml)
    echo -e "${YELLOW}Validating Ingress with Gateway API and SPOA hub disabled...${NC}" >&2
    if ! helm template "$CHART_DIR" --namespace default $HAPROXY_VERSION_ARG \
        --set controller.templateLibraries.gateway.enabled=false \
        --set controller.templateLibraries.hapticAnnotations.enabled=true \
        --set controller.templateLibraries.spoaHub.enabled=false \
        --set spoaHub.enabled=false \
        | yq 'select(.kind == "HAProxyTemplateConfig" or .kind == "HAProxyTemplateLibrary")' \
        > "$INGRESS_CONFIG"; then
        rm -f "$INGRESS_CONFIG"
        exit 1
    fi
    "$CONTROLLER_BIN" validate --file "$INGRESS_CONFIG" "${SCHEMA_DIR_ARGS[@]}" \
        --test test-ingress-duplicate-backend-different-ports || FULL_RC=$?
    rm -f "$INGRESS_CONFIG"
fi

# Every declared annotation must keep raw HAProxy directives out of haproxy.cfg
# unless extraContext.ingressPermissions.allowRawHAProxyConfig allows them.
if [[ $FULL_RC -eq 0 ]] && ! single_test_requested "$@"; then
    echo -e "${YELLOW}Probing every annotation for raw HAProxy configuration...${NC}" >&2
    PROBE_CONFIG=$(mktemp /tmp/haptic-raw-probe-XXXXXX.yaml)
    PROBE_RESULTS=$(mktemp /tmp/haptic-raw-probe-XXXXXX.json)
    PROBE_LOG=$(mktemp /tmp/haptic-raw-probe-XXXXXX.log)
    if ! PROBE_COUNT=$(python3 "$PROJECT_ROOT/scripts/raw-annotation-probe.py" generate "$TEMP_CONFIG" "$PROBE_CONFIG"); then
        FULL_RC=1
    else
        "$CONTROLLER_BIN" validate --file "$PROBE_CONFIG" "${SCHEMA_DIR_ARGS[@]}" --output json \
            > "$PROBE_RESULTS" 2> "$PROBE_LOG" || true
        if ! python3 "$PROJECT_ROOT/scripts/raw-annotation-probe.py" report "$PROBE_RESULTS" "$PROBE_COUNT"; then
            tail -20 "$PROBE_LOG" >&2
            FULL_RC=1
        fi
    fi
    rm -f "$PROBE_CONFIG" "$PROBE_RESULTS" "$PROBE_LOG"
fi

if [[ $FULL_RC -eq 0 ]] && ! single_test_requested "$@"; then
    # The two bundled tests render chart snippets that end mid-line just
    # before the operator's frontend-extra snippet.
    SNIPPET_CONFIG=$(mktemp /tmp/haptic-extension-point-snippets-XXXXXX.yaml)
    echo -e "${YELLOW}Validating operator snippets without surrounding newlines...${NC}" >&2
    if ! helm template "$CHART_DIR" --namespace default $HAPROXY_VERSION_ARG \
        --set controller.templateLibraries.hapticAnnotations.enabled=true \
        --set controller.templateLibraries.haproxyIngress.enabled=true \
        --values "$PROJECT_ROOT/tests/fixtures/extension-point-snippets/values.yaml" \
        | yq 'select(.kind == "HAProxyTemplateConfig" or .kind == "HAProxyTemplateLibrary")' \
        > "$SNIPPET_CONFIG"; then
        rm -f "$SNIPPET_CONFIG"
        exit 1
    fi
    "$CONTROLLER_BIN" validate --file "$SNIPPET_CONFIG" "${SCHEMA_DIR_ARGS[@]}" \
        --test test-operator-snippets-start-on-their-own-line \
        --test test-haptic-config-injection \
        --test test-haproxy-ingress-config-frontend || FULL_RC=$?
    rm -f "$SNIPPET_CONFIG"
fi

if [[ $FULL_RC -eq 0 ]] && ! single_test_requested "$@"; then
    echo -e "${YELLOW}Validating operator forwarded headers after client sanitization...${NC}" >&2
    HAPTIC_TEMPLATE_VALUES="$PROJECT_ROOT/tests/fixtures/forwarded-headers/values.yaml" \
        "$0" --test test-operator-forwarded-headers-after-sanitization \
        --test test-operator-forwarded-headers-trusted-proxies || FULL_RC=$?
fi

if ! single_test_requested "$@"; then
    # Access-log coverage invariant: every frontend the chart renders must carry a
    # log-format, or that frontend silently falls back to `option httplog` from
    # `defaults` and emits text lines into an otherwise-JSON stream.
    #
    # This lives here rather than in a validationTest on purpose. A count of
    # frontends depends on which frontend-adding features a deployment enables (the
    # Varnish cache tier adds haptic_cache_origin, Gateway listeners add per-port
    # frontends), and validationTests also run as the controller's FATAL load gate
    # against the operator's own values — so a count baked into the shipped CR would
    # block every new controller pod for enabling an unrelated feature. Here it is a
    # repo-side check against a render we control, which is exactly what catches a
    # new frontend added without wiring the log-format into it.
    echo -e "${YELLOW}Checking access-log coverage (every frontend has a log-format)...${NC}" >&2
    COVERAGE_CONFIG=$(mktemp /tmp/haptic-access-log-coverage-XXXXXX.yaml)
    helm template "$CHART_DIR" --namespace default $HAPROXY_VERSION_ARG \
        --api-versions=gateway.networking.k8s.io/v1/GatewayClass \
        --set controller.templateLibraries.gateway.enabled=true \
        --set controller.templateLibraries.gateway.experimentalChannel=true \
        --set controller.templateLibraries.hapticAnnotations.enabled=true \
        --set cache.varnish.enabled=true \
        2>/dev/null | yq 'select(.kind == "HAProxyTemplateConfig" or .kind == "HAProxyTemplateLibrary")' > "$COVERAGE_CONFIG"
    # Two fixtures, because no single one renders every frontend: the TCPRoute test
    # produces the library-owned gateway-tcp-port-* (and, with the cache tier on,
    # haptic_cache_origin), while the access-log fixture's TLS Ingress produces
    # base's status / http-tcp / http_frontend / http_frontend_h2c / https.
    for COVERAGE_TEST in test-tcproute-basic-l4-forward test-access-log-every-frontend-has-a-format; do
        COVERAGE_DUMP=$(mktemp /tmp/haptic-access-log-dump-XXXXXX.txt)
        if ! "$CONTROLLER_BIN" validate --file "$COVERAGE_CONFIG" "${SCHEMA_DIR_ARGS[@]}" \
                --test "$COVERAGE_TEST" --dump-rendered > "$COVERAGE_DUMP" 2>&1; then
            echo -e "${RED}Error: access-log coverage render failed for ${COVERAGE_TEST}:${NC}" >&2
            tail -30 "$COVERAGE_DUMP" >&2
            rm -f "$COVERAGE_CONFIG" "$COVERAGE_DUMP"
            exit 1
        fi
        if ! python3 -c '
import re, sys
dump = open(sys.argv[1]).read()
# Split into per-section blocks: a frontend body is indented, so its block runs
# to the next column-0 section header.
blocks = re.split(r"(?m)^(?=(?:frontend|backend|listen|defaults|global|peers|userlist|resolvers) )", dump)
fes = [b for b in blocks if b.startswith("frontend ")]
if not fes:
    print("no frontends in the rendered config - this check would pass vacuously", file=sys.stderr)
    sys.exit(1)
missing = [b.split("\n", 1)[0].strip() for b in fes if "log-format " not in b]
if missing:
    print("frontends without a log-format: " + ", ".join(missing), file=sys.stderr)
    sys.exit(1)
# Trace-id capture must precede every rule that can end the request. HAProxy
# stops evaluating http-request rules at a deny/return/tarpit/reject, so a
# set-var placed after one never runs for exactly the short-circuited requests
# traces are for: WAF denies, rate-limit 429s, redirects, fixed responses. The
# span builder aborts on an empty trace_id, so the symptom is a silently missing
# span, not an error — and it would falsify the coverage claim in values.yaml.
# Ordering cannot be expressed as a validationTest: Go RE2 has no lookahead.
#
# http-request only, deliberately. `tcp-request ... reject` / `silent-drop` also
# short-circuit, but they fire before the request is parsed, so there is no HTTP
# transaction to trace and no access-log record to build a span from — a missing
# span there is correct. Including them would also make the check useless: tcp-
# request rules always precede http-request rules within a frontend, so every
# frontend carrying any L4 reject would be flagged.
STOP = re.compile(r"^\s+http-request\s+(deny|return|tarpit|reject|silent-drop)\b", re.M)
TRACE = re.compile(r"^\s+http-request\s+set-var(-fmt)?\(txn\.trace_id\)", re.M)
late = []
for b in fes:
    t, x = TRACE.search(b), STOP.search(b)
    if x and (t is None or t.start() > x.start()):
        late.append(b.split("\n", 1)[0].strip())
if late:
    print("trace_id captured after a request-ending rule in: " + ", ".join(late), file=sys.stderr)
    sys.exit(1)
print("checked %d frontends" % len(fes), file=sys.stderr)
' "$COVERAGE_DUMP"; then
            echo -e "${RED}Error: a rendered frontend has no log-format (render: ${COVERAGE_TEST}).${NC}" >&2
            echo "  Wire {{ render \"util-log-format-http\" }} (HTTP mode) or" >&2
            echo "  {{ render \"util-log-format-tcp\" }} (TCP mode) into it; otherwise it" >&2
            echo "  inherits 'option httplog' from defaults and emits text lines into the" >&2
            echo "  JSON access-log stream. See base.yaml's \"Access log\" block." >&2
            rm -f "$COVERAGE_CONFIG" "$COVERAGE_DUMP"
            exit 1
        fi
        rm -f "$COVERAGE_DUMP"
    done
    rm -f "$COVERAGE_CONFIG"
    echo -e "${GREEN}Access-log coverage OK${NC}" >&2
fi


# Gateway policies must also work without any Ingress annotation adapter.
if [[ $FULL_RC -eq 0 ]] && ! single_test_requested "$@"; then
    POLICY_CONFIG=$(mktemp /tmp/haptic-policy-config-XXXXXX.yaml)
    helm template "$CHART_DIR" --namespace default $HAPROXY_VERSION_ARG \
        --set controller.templateLibraries.ingress.enabled=false \
        --set controller.templateLibraries.ingressAnnotationsCompat.enabled=false \
        --set controller.templateLibraries.hapticAnnotations.enabled=false \
        --set controller.templateLibraries.haproxytech.enabled=false \
        --set controller.templateLibraries.haproxyIngress.enabled=false \
        --set controller.templateLibraries.nginxIngress.enabled=false \
        --set cache.varnish.enabled=true \
        --set rateLimit.shared.enabled=true \
        --set rateLimit.shared.managedStore.enabled=true \
        --set spoaHub.plugins.coraza.enabled=true \
        | yq 'select(.kind == "HAProxyTemplateConfig" or .kind == "HAProxyTemplateLibrary")' > "$POLICY_CONFIG"
    "$CONTROLLER_BIN" validate --file "$POLICY_CONFIG" "${SCHEMA_DIR_ARGS[@]}" \
        --test test-gateway-policy-authentication --test test-gateway-policy-shared-rate-limit \
        --test test-gateway-policy-private-cache --test test-gateway-policy-waf-catalogs "$@" || FULL_RC=$?
    if [[ $FULL_RC -eq 0 && ${#SCHEMA_DIR_ARGS[@]} -gt 0 ]]; then
        POLICY_SCHEMAS=$(mktemp -d /tmp/haptic-policy-schemas-XXXXXX)
        cp "$SCHEMA_DIR"/*.yaml "$POLICY_SCHEMAS/"
        rm "$POLICY_SCHEMAS/haproxy-haptic.org_haproxyroutepolicies.yaml"
        "$CONTROLLER_BIN" validate --file "$POLICY_CONFIG" --schema-dir "$POLICY_SCHEMAS" \
            --test test-gateway-policy-unavailable-watch "$@" || FULL_RC=$?
        rm -rf "$POLICY_SCHEMAS"
    fi
    rm -f "$POLICY_CONFIG"
fi

# Optional shared-rate-limit profile. The normal render above must keep the
# feature off so validationTests can assert that using
# haproxy-haptic.org/rate-limit-requests without the opt-in fails loudly.
# These tests need the opt-in because they assert the active map + SPOE
# dispatch path.
if [[ $FULL_RC -eq 0 ]] && ! single_test_requested "$@"; then
    RATE_LIMIT_CONFIG=$(mktemp /tmp/haptic-rate-limit-config-XXXXXX.yaml)
    trap 'rm -f "$TEMP_CONFIG" "$RATE_LIMIT_CONFIG"' EXIT
    echo -e "${YELLOW}Rendering shared rate-limit profile...${NC}" >&2
    if ! helm template "$CHART_DIR" \
        --namespace default \
        $HAPROXY_VERSION_ARG \
        --set controller.templateLibraries.gateway.enabled=true \
        --set controller.templateLibraries.gateway.experimentalChannel=true \
        --set controller.templateLibraries.hapticAnnotations.enabled=true \
        --set controller.templateLibraries.haproxytech.enabled=true \
        --set controller.templateLibraries.haproxyIngress.enabled=true \
        --set controller.templateLibraries.nginxIngress.enabled=true \
        --set cache.varnish.enabled=true \
        --set rateLimit.shared.enabled=true \
        --set rateLimit.shared.managedStore.enabled=true \
        --set spoaHub.plugins.coraza.enabled=true \
        | yq 'select(.kind == "HAProxyTemplateConfig" or .kind == "HAProxyTemplateLibrary")' \
        > "$RATE_LIMIT_CONFIG"; then
        echo -e "${RED}Error: Failed to render shared rate-limit Helm profile${NC}" >&2
        exit 1
    fi
    "$CONTROLLER_BIN" validate --file "$RATE_LIMIT_CONFIG" "${SCHEMA_DIR_ARGS[@]}" \
        --test test-haptic-canary-by-header \
        --test test-nginx-ingress-canary-by-header \
        --test test-haptic-cache-core \
        --test test-gateway-policy-shared-rate-limit \
        --test test-gateway-policy-waf \
        --test test-gateway-policy-waf-invalid \
        --test test-gateway-policy-waf-catalogs \
        --test test-gateway-policy-waf-baseline \
        --test test-haptic-rate-limit-shared-ip \
        --test test-haptic-rate-limit-shared-ip-allowlist \
        --test test-haptic-rate-limit-shared-ip-before-waf \
        --test test-haptic-rate-limit-shared-exact-consumer \
        --test test-haptic-rate-limit-shared-strict-unavailable \
        --test test-haptic-rate-limit-shared-consumer-cache-route \
        --test test-haptic-rate-limit-shared-consumer-cache-without-api-key-accepted \
        --test test-haptic-cache-shared-rate-limit-loopback \
        --test test-haptic-cache-autoscaling \
        --test test-haptic-rate-limit-shared-invalid-requests \
        --test test-haptic-rate-limit-shared-invalid-period-zero \
        --test test-haptic-rate-limit-shared-refill-horizon-too-long \
        "$@" || FULL_RC=$?
fi

# Optional API-gateway request-validation profile. The normal render keeps the
# feature off so validationTests can assert that request-schema annotations fail
# loudly without the opt-in. These tests assert the enabled map + SPOE dispatch
# path and render-time guardrails.
if [[ $FULL_RC -eq 0 ]] && ! single_test_requested "$@"; then
    REQUEST_VALIDATION_CONFIG=$(mktemp /tmp/haptic-request-validation-config-XXXXXX.yaml)
    trap 'rm -f "$TEMP_CONFIG" "${RATE_LIMIT_CONFIG:-}" "$REQUEST_VALIDATION_CONFIG"' EXIT
    echo -e "${YELLOW}Rendering request-validation profile...${NC}" >&2
    if ! helm template "$CHART_DIR" \
        --namespace default \
        $HAPROXY_VERSION_ARG \
        --set controller.templateLibraries.gateway.enabled=true \
        --set controller.templateLibraries.gateway.experimentalChannel=true \
        --set controller.templateLibraries.hapticAnnotations.enabled=true \
        --set controller.templateLibraries.haproxytech.enabled=true \
        --set controller.templateLibraries.haproxyIngress.enabled=true \
        --set controller.templateLibraries.nginxIngress.enabled=true \
        --set controller.config.templatingSettings.extraContext.apiGateway.requestSchemaValidation.enabled=true \
        | yq 'select(.kind == "HAProxyTemplateConfig" or .kind == "HAProxyTemplateLibrary")' \
        > "$REQUEST_VALIDATION_CONFIG"; then
        echo -e "${RED}Error: Failed to render request-validation Helm profile${NC}" >&2
        exit 1
    fi
    "$CONTROLLER_BIN" validate --file "$REQUEST_VALIDATION_CONFIG" "${SCHEMA_DIR_ARGS[@]}" \
        --test test-haptic-request-validation-configmap \
        --test test-haptic-request-validation-secret \
        --test test-haptic-request-validation-rejects-two-sources \
        --test test-haptic-request-validation-missing-key \
        --test test-haptic-request-validation-max-body-bounded \
        --test test-haptic-request-validation-max-body-fits-buffer \
        "$@" || FULL_RC=$?
    if [[ $FULL_RC -eq 0 ]]; then
        echo -e "${YELLOW}Request-validation Helm guard: requestBody.defaultMaxBytes hard cap...${NC}" >&2
        GUARD_ERR=$(mktemp /tmp/haptic-request-validation-guard-XXXXXX.log)
        if helm template "$CHART_DIR" \
            --namespace default \
            $HAPROXY_VERSION_ARG \
            --set controller.templateLibraries.hapticAnnotations.enabled=true \
            --set controller.config.templatingSettings.extraContext.apiGateway.requestSchemaValidation.enabled=true \
            --set controller.config.templatingSettings.extraContext.apiGateway.requestSchemaValidation.requestBody.defaultMaxBytes=1048577 \
            > /dev/null 2> "$GUARD_ERR"; then
            echo -e "${RED}Error: request-validation requestBody.defaultMaxBytes hard-cap guard did not fail${NC}" >&2
            rm -f "$GUARD_ERR"
            exit 1
        fi
        if ! grep -q "apiGateway.requestSchemaValidation.requestBody.defaultMaxBytes must be between 1 and 1048576 bytes" "$GUARD_ERR"; then
            echo -e "${RED}Error: request-validation requestBody.defaultMaxBytes hard-cap guard returned unexpected error:${NC}" >&2
            cat "$GUARD_ERR" >&2
            rm -f "$GUARD_ERR"
            exit 1
        fi
        rm -f "$GUARD_ERR"

        echo -e "${YELLOW}Request-body inspection Helm guard: haproxyBuffer.sizeBytes lower bound...${NC}" >&2
        GUARD_ERR=$(mktemp /tmp/haptic-request-validation-guard-XXXXXX.log)
        if helm template "$CHART_DIR" \
            --namespace default \
            $HAPROXY_VERSION_ARG \
            --set controller.templateLibraries.hapticAnnotations.enabled=true \
            --set controller.config.templatingSettings.extraContext.apiGateway.requestSchemaValidation.enabled=true \
            --set controller.config.templatingSettings.extraContext.requestBodyInspection.haproxyBuffer.sizeBytes=8192 \
            > /dev/null 2> "$GUARD_ERR"; then
            echo -e "${RED}Error: request-body inspection haproxyBuffer.sizeBytes lower-bound guard did not fail${NC}" >&2
            rm -f "$GUARD_ERR"
            exit 1
        fi
        if ! grep -q "requestBodyInspection.haproxyBuffer.sizeBytes must be between 16384 and 2097152 bytes" "$GUARD_ERR"; then
            echo -e "${RED}Error: request-body inspection haproxyBuffer.sizeBytes lower-bound guard returned unexpected error:${NC}" >&2
            cat "$GUARD_ERR" >&2
            rm -f "$GUARD_ERR"
            exit 1
        fi
        rm -f "$GUARD_ERR"

        echo -e "${YELLOW}Request-body inspection Helm guard: reservedBytes fits sizeBytes...${NC}" >&2
        GUARD_ERR=$(mktemp /tmp/haptic-request-validation-guard-XXXXXX.log)
        if helm template "$CHART_DIR" \
            --namespace default \
            $HAPROXY_VERSION_ARG \
            --set controller.templateLibraries.hapticAnnotations.enabled=true \
            --set controller.config.templatingSettings.extraContext.apiGateway.requestSchemaValidation.enabled=true \
            --set controller.config.templatingSettings.extraContext.requestBodyInspection.haproxyBuffer.sizeBytes=16384 \
            --set controller.config.templatingSettings.extraContext.requestBodyInspection.haproxyBuffer.reservedBytes=16384 \
            > /dev/null 2> "$GUARD_ERR"; then
            echo -e "${RED}Error: request-validation reservedBytes guard did not fail${NC}" >&2
            rm -f "$GUARD_ERR"
            exit 1
        fi
        if ! grep -q "haproxyBuffer.reservedBytes must be a positive integer smaller than requestBodyInspection.haproxyBuffer.sizeBytes" "$GUARD_ERR"; then
            echo -e "${RED}Error: request-validation reservedBytes guard returned unexpected error:${NC}" >&2
            cat "$GUARD_ERR" >&2
            rm -f "$GUARD_ERR"
            exit 1
        fi
        rm -f "$GUARD_ERR"

        echo -e "${YELLOW}Request-validation Helm guard: requestBody.defaultMaxBytes leaves reserved buffer capacity...${NC}" >&2
        GUARD_ERR=$(mktemp /tmp/haptic-request-validation-guard-XXXXXX.log)
        if helm template "$CHART_DIR" \
            --namespace default \
            $HAPROXY_VERSION_ARG \
            --set controller.templateLibraries.hapticAnnotations.enabled=true \
            --set controller.config.templatingSettings.extraContext.apiGateway.requestSchemaValidation.enabled=true \
            --set controller.config.templatingSettings.extraContext.apiGateway.requestSchemaValidation.requestBody.defaultMaxBytes=9000 \
            --set controller.config.templatingSettings.extraContext.requestBodyInspection.haproxyBuffer.sizeBytes=16384 \
            > /dev/null 2> "$GUARD_ERR"; then
            echo -e "${RED}Error: request-validation buffer-headroom guard did not fail${NC}" >&2
            rm -f "$GUARD_ERR"
            exit 1
        fi
        if ! grep -q "requestBody.defaultMaxBytes must not exceed requestBodyInspection.haproxyBuffer.sizeBytes minus requestBodyInspection.haproxyBuffer.reservedBytes" "$GUARD_ERR"; then
            echo -e "${RED}Error: request-validation buffer-headroom guard returned unexpected error:${NC}" >&2
            cat "$GUARD_ERR" >&2
            rm -f "$GUARD_ERR"
            exit 1
        fi
        rm -f "$GUARD_ERR"

        echo -e "${YELLOW}Request-validation Helm guard: requestBody.waitTimeout duration...${NC}" >&2
        GUARD_ERR=$(mktemp /tmp/haptic-request-validation-guard-XXXXXX.log)
        if helm template "$CHART_DIR" \
            --namespace default \
            $HAPROXY_VERSION_ARG \
            --set controller.templateLibraries.hapticAnnotations.enabled=true \
            --set controller.config.templatingSettings.extraContext.apiGateway.requestSchemaValidation.enabled=true \
            --set-string 'controller.config.templatingSettings.extraContext.apiGateway.requestSchemaValidation.requestBody.waitTimeout=100ms http-request deny' \
            > /dev/null 2> "$GUARD_ERR"; then
            echo -e "${RED}Error: request-validation requestBody.waitTimeout guard did not fail${NC}" >&2
            rm -f "$GUARD_ERR"
            exit 1
        fi
        if ! grep -q "requestBody.waitTimeout must be a positive HAProxy duration" "$GUARD_ERR"; then
            echo -e "${RED}Error: request-validation requestBody.waitTimeout guard returned unexpected error:${NC}" >&2
            cat "$GUARD_ERR" >&2
            rm -f "$GUARD_ERR"
            exit 1
        fi
        rm -f "$GUARD_ERR"
    fi
fi

# Assert that one Helm values combination renders successfully. The
# values-surface counterpart of run_helm_failure_guard: exercises fields the
# Helm-level pre-validation must accept, so the haproxytemplateconfig.yaml
# allowlist can't silently drift behind the Scriggo-side registerPolicy
# allowlist (that drift shipped once: allowedMethods rejected at helm
# template time while every chart test passed).
run_helm_success_guard() {
    local label=$1
    shift 1
    local guard_err
    guard_err=$(mktemp /tmp/haptic-helm-guard-XXXXXX.log)
    echo -e "${YELLOW}${label}...${NC}" >&2
    if ! helm template "$CHART_DIR" --namespace default $HAPROXY_VERSION_ARG "$@" > /dev/null 2> "$guard_err"; then
        echo -e "${RED}Error: ${label} failed to render:${NC}" >&2
        cat "$guard_err" >&2
        rm -f "$guard_err"
        exit 1
    fi
    rm -f "$guard_err"
}

if [[ $FULL_RC -eq 0 ]] && ! single_test_requested "$@"; then
    run_helm_success_guard \
        "Access-log Helm guard: accept the full accessLog values surface" \
        --set-json 'controller.config.templatingSettings.extraContext.accessLog={"maxLineBytes":32768,"fields":{"tenant":"req.hdr(X-Tenant)","region":"str(prod-eu)"}}'
    run_helm_failure_guard \
        "Access-log Helm guard: reject unknown accessLog fields" \
        "/extraContext/accessLog': additional properties 'fieldz' not allowed" \
        --set-string 'controller.config.templatingSettings.extraContext.accessLog.fieldz=x'
    run_helm_failure_guard \
        "Access-log Helm guard: reject an invalid JSON field name" \
        "accessLog.fields contains invalid field name \"bad-name\"" \
        --set-string 'controller.config.templatingSettings.extraContext.accessLog.fields.bad-name=src'
    run_helm_failure_guard \
        "Access-log Helm guard: reject a field expression that could continue the directive" \
        "must not contain whitespace" \
        --set-string 'controller.config.templatingSettings.extraContext.accessLog.fields.evil=str(a) if TRUE'
    run_helm_success_guard \
        "Access-log Helm guard: accept a buffered ring target for a log-shipper sidecar" \
        --set-json 'controller.config.templatingSettings.extraContext.accessLog.targets={"main":{"ring":{"name":"accesslog","address":"127.0.0.1:6514","size":65536,"logProto":"legacy","connectTimeout":"5s","serverTimeout":"10s","serverOptions":"ssl verify none"}},"second":{"address":"stdout"}}'
    run_helm_failure_guard \
        "Access-log Helm guard: reject a target that sets both address and ring" \
        "sets both address and ring" \
        --set-json 'controller.config.templatingSettings.extraContext.accessLog.targets={"main":{"address":"stdout","ring":{"name":"r","address":"127.0.0.1:6514"}}}'
    run_helm_failure_guard \
        "Access-log Helm guard: reject an unroutable log target address" \
        "is not a valid HAProxy log target" \
        --set-json 'controller.config.templatingSettings.extraContext.accessLog.targets={"main":{"address":"stdout local0 info"}}'
    run_helm_failure_guard \
        "Access-log Helm guard: reject injection through ring serverOptions" \
        "must not contain control characters or '#'" \
        --set-json 'controller.config.templatingSettings.extraContext.accessLog.targets={"main":{"ring":{"name":"r","address":"127.0.0.1:6514","serverOptions":"ssl # x"}}}'
    run_helm_success_guard \
        "Access-log Helm guard: accept opt-in suppression of successful requests" \
        --set controller.config.templatingSettings.extraContext.accessLog.suppress.successful=true
    run_helm_failure_guard \
        "Access-log Helm guard: reject an unknown accessLog.suppress field" \
        "/accessLog/suppress': additional properties 'successfull' not allowed" \
        --set-json 'controller.config.templatingSettings.extraContext.accessLog.suppress={"successfull":true}'
    run_helm_failure_guard \
        "Access-log Helm guard: reject a non-boolean accessLog.suppress.successful" \
        "/accessLog/suppress/successful': got string, want boolean" \
        --set-string controller.config.templatingSettings.extraContext.accessLog.suppress.successful=maybe
    run_helm_failure_guard \
        "HAProxy Helm guard: reject an invalid agent log level" \
        "/haproxy/agent/logLevel': value must be one of" \
        --set haproxy.agent.logLevel=verbose
    run_helm_failure_guard \
        "HAProxy Helm guard: reject the removed dataplane log level" \
        "haproxy.dataplane.logLevel" \
        --set haproxy.dataplane.logLevel=debug
    run_helm_failure_guard \
        "Access-log Helm guard: reject a level that silently drops every record" \
        "silently drops every one of them" \
        --set-json 'controller.config.templatingSettings.extraContext.accessLog.targets={"main":{"address":"stdout","level":"notice"}}'
    run_helm_failure_guard \
        "Access-log Helm guard: reject a UNIX-socket ring server" \
        "HAProxy 3.4 rejects a UNIX ring server" \
        --set-json 'controller.config.templatingSettings.extraContext.accessLog.targets={"main":{"ring":{"name":"r","address":"unix@/var/run/log.sock"}}}'
    run_helm_failure_guard \
        "Access-log Helm guard: reject a ring reference no target declares" \
        "points at a ring no target declares" \
        --set-json 'controller.config.templatingSettings.extraContext.accessLog.targets={"main":{"address":"ring@nowhere"}}'
    # Matched WAF request data must not be in the DEFAULT rendered log-format — it
    # echoes request payload fragments. Deliberately a repo-side check, not a
    # validationTest: operators are documented to opt in by contributing a
    # log-fields-* snippet for txn.hub.coraza.data, and validationTests are also
    # the controller's fatal load gate against the operator's own config — so as
    # a test this would block the controller of anyone who followed the
    # documentation. Reuses the coverage render above, which is ours.
    echo -e "${YELLOW}Checking matched WAF request data is not in the default log-format...${NC}" >&2
    WAFDATA_CONFIG=$(mktemp /tmp/haptic-wafdata-XXXXXX.yaml)
    WAFDATA_DUMP=$(mktemp /tmp/haptic-wafdata-dump-XXXXXX.txt)
    helm template "$CHART_DIR" --namespace default $HAPROXY_VERSION_ARG \
        --set controller.templateLibraries.nginxIngress.enabled=true \
        --set controller.templateLibraries.hapticAnnotations.enabled=true \
        2>/dev/null | yq 'select(.kind == "HAProxyTemplateConfig" or .kind == "HAProxyTemplateLibrary")' > "$WAFDATA_CONFIG"
    if ! "$CONTROLLER_BIN" validate --file "$WAFDATA_CONFIG" "${SCHEMA_DIR_ARGS[@]}" \
            --test test-spoa-hub-access-log-fields --dump-rendered > "$WAFDATA_DUMP" 2>&1; then
        echo -e "${RED}Error: WAF-data render failed:${NC}" >&2
        tail -20 "$WAFDATA_DUMP" >&2
        rm -f "$WAFDATA_CONFIG" "$WAFDATA_DUMP"
        exit 1
    fi
    if ! grep -q 'log-format "%{+json}o' "$WAFDATA_DUMP"; then
        echo -e "${RED}WAF-data guard: no JSON log-format in the rendered dump — the check cannot run${NC}" >&2
        rm -f "$WAFDATA_CONFIG" "$WAFDATA_DUMP"
        exit 1
    fi
    if grep 'log-format "%{+json}o' "$WAFDATA_DUMP" | grep -q 'txn\.hub\.coraza\.data'; then
        echo -e "${RED}WAF-data guard: the default log-format logs txn.hub.coraza.data (matched request payload)${NC}" >&2
        rm -f "$WAFDATA_CONFIG" "$WAFDATA_DUMP"
        exit 1
    fi
    rm -f "$WAFDATA_CONFIG" "$WAFDATA_DUMP"
    echo -e "${GREEN}WAF-data guard: matched request data is not in the default log-format${NC}" >&2
    run_helm_success_guard \
        "Access-log Helm guard: accept a socket path whose name ends in digits" \
        --set-json 'controller.config.templatingSettings.extraContext.accessLog.targets={"main":{"address":"/var/run/log:99999"}}'
    run_helm_failure_guard \
        "Access-log Helm guard: reject an out-of-range port in a log target address" \
        "outside 1-65535" \
        --set-json 'controller.config.templatingSettings.extraContext.accessLog.targets={"main":{"address":"10.0.0.5:99999"}}'
    run_helm_failure_guard \
        "Access-log Helm guard: reject an out-of-range port in a ring server address" \
        "outside 1-65535" \
        --set-json 'controller.config.templatingSettings.extraContext.accessLog.targets={"main":{"ring":{"name":"r","address":"10.0.0.5:99999"}}}'
    run_helm_failure_guard \
        "Access-log Helm guard: reject a ring buffer too small for one record" \
        "too small for accessLog.maxLineBytes" \
        --set-json 'controller.config.templatingSettings.extraContext.accessLog.targets={"main":{"ring":{"name":"r","address":"127.0.0.1:6514","size":4096}}}'
    run_helm_failure_guard \
        "Access-log Helm guard: reject two targets that render the same log line" \
        "would be logged twice" \
        --set-json 'controller.config.templatingSettings.extraContext.accessLog.targets={"a":{"ring":{"name":"x","address":"127.0.0.1:6514"}},"b":{"address":"ring@x"}}'
    run_helm_failure_guard \
        "Access-log Helm guard: reject an empty target map" \
        "accessLog.targets is empty" \
        --set-json 'controller.config.templatingSettings.extraContext.accessLog.targets={}'
    run_helm_success_guard \
        "Access-log Helm guard: accept one address with distinct per-target settings" \
        --set-json 'controller.config.templatingSettings.extraContext.accessLog.targets={"plain":{"address":"stdout"},"local1":{"address":"stdout","facility":"local1"}}'
    run_helm_failure_guard \
        "Access-log Helm guard: reject two targets declaring the same ring name" \
        "declared by an earlier target" \
        --set-json 'controller.config.templatingSettings.extraContext.accessLog.targets={"a":{"ring":{"name":"dup","address":"127.0.0.1:6514"}},"b":{"ring":{"name":"dup","address":"127.0.0.1:6515"}}}'
    run_helm_failure_guard \
        "Vector migration guard: vector.excludeMetrics names its new home" \
        "vector.excludeMetrics moved to controller.config.templatingSettings.extraContext.prometheusExporter.excludeMetrics" \
        --set-json 'vector.excludeMetrics={"backendHttpCompression":{"enabled":false}}'
    run_helm_failure_guard \
        "Vector migration guard: vector.podMonitor names its new home" \
        "vector.podMonitor moved to haproxy.monitoring.podMonitor" \
        --set vector.podMonitor.enabled=true
    run_helm_failure_guard \
        "Prometheus exporter guard: reject an unknown field on an entry" \
        "/excludeMetrics/serverBackendMax': additional properties 'enable' not allowed" \
        --set-json 'controller.config.templatingSettings.extraContext.prometheusExporter.excludeMetrics={"serverBackendMax":{"enable":true}}'
    run_helm_failure_guard \
        "Prometheus exporter guard: reject an enabled entry with no families" \
        "is enabled but lists no" \
        --set-json 'controller.config.templatingSettings.extraContext.prometheusExporter.excludeMetrics={"mine":{"enabled":true}}'
    run_helm_failure_guard \
        "Prometheus exporter guard: reject a non-boolean enabled" \
        "/excludeMetrics/mine/enabled': got string, want boolean" \
        --set-json 'controller.config.templatingSettings.extraContext.prometheusExporter.excludeMetrics={"mine":{"enabled":"yes","families":["haproxy_x"]}}'
    run_helm_failure_guard \
        "Prometheus exporter guard: reject a family that is not a bare metric name" \
        "is not a bare metric name" \
        --set-json 'controller.config.templatingSettings.extraContext.prometheusExporter.excludeMetrics={"mine":{"enabled":true,"families":["haproxy_x%"]}}'
    run_helm_failure_guard \
        "Prometheus exporter guard: reject a regex pattern carried over from vector.excludeMetrics" \
        "filters by exact name only" \
        --set-json 'controller.config.templatingSettings.extraContext.prometheusExporter.excludeMetrics={"mine":{"enabled":true,"pattern":"^haproxy_"}}'
    run_helm_success_guard \
        "Prometheus exporter guard: a disabled entry needs no families" \
        --set-json 'controller.config.templatingSettings.extraContext.prometheusExporter.excludeMetrics={"backendHttpCompression":{"enabled":false}}'
    run_helm_failure_guard \
        "Access-log Helm guard: reject an out-of-range log line length" \
        "accessLog.maxLineBytes must be an integer between 1024 and 65535." \
        --set controller.config.templatingSettings.extraContext.accessLog.maxLineBytes=512
    run_helm_success_guard \
        "WAF policy Helm guard: accept the full inline-policy field surface" \
        --set-json 'controller.config.templatingSettings.extraContext.waf.policies.inline={"full-surface":{"description":"every valid field","enforcement":"deny","allowedMethods":["GET","HEAD","POST","OPTIONS","PUT","PATCH","DELETE"],"paranoiaLevel":2,"anomalyThreshold":{"inbound":10,"outbound":8},"ruleExclusions":[{"rules":[930130],"onPathContains":".git/"},{"rules":[941320],"excludeTarget":"ARGS:wp_post"},{"tags":["attack-sqli"],"excludeTarget":"ARGS:q"}],"secLang":"SecRuleRemoveById 999999","requestBody":{"mode":"json","maxBytes":2048}}}'
    run_helm_failure_guard \
        "WAF policy Helm guard: reject unknown inline policy fields" \
        'waf.policies.inline.bad contains unknown field "allowedMethodz"' \
        --set-json 'controller.config.templatingSettings.extraContext.waf.policies.inline={"bad":{"allowedMethodz":["GET"]}}'
    run_helm_failure_guard \
        "WAF policy Helm guard: reject unknown ConfigMap reference fields" \
        "/configMapRefs/security': additional properties 'namespce' not allowed" \
        --set-string 'controller.config.templatingSettings.extraContext.waf.policies.configMapRefs.security.name=policies' \
        --set-string 'controller.config.templatingSettings.extraContext.waf.policies.configMapRefs.security.namespce=security'
    run_helm_failure_guard \
        "Reusable-WAF Helm guard: immutable policy requires a default" \
        "waf.policies.defaultPolicy is required when waf.ingressPermissions.allowPolicySelection=false" \
        --set controller.config.templatingSettings.extraContext.waf.ingressPermissions.allowPolicySelection=false \
        --set controller.config.templatingSettings.extraContext.waf.policies.inline.baseline.enforcement=deny
    run_helm_failure_guard \
        "Shared rate-limit Helm guard: reject ambiguous legacy store ownership" \
        "/rateLimit/shared': additional properties 'store' not allowed" \
        --set rateLimit.shared.store.enabled=false
    run_helm_failure_guard \
        "Shared rate-limit Helm guard: reject Redis CLI spelling in public values" \
        "rateLimit.shared.managedStore.maxmemory and maxmemoryPolicy were renamed to maxMemory and maxMemoryPolicy" \
        --set-string rateLimit.shared.managedStore.maxmemory=64mb
    run_helm_failure_guard \
        "Managed Valkey Helm guard: reject invalid image pull policy" \
        "/rateLimit/shared/managedStore/imagePullPolicy': value must be one of 'Always', 'IfNotPresent', 'Never'" \
        --set-string rateLimit.shared.managedStore.imagePullPolicy=Sometimes
    # A values block left over from before the otel plugin was removed must fail
    # the render, not reach config.toml and fail when the hub tries to load a
    # plugin the image no longer ships.
    run_helm_failure_guard \
        "SPOA Hub Helm guard: reject a leftover otel plugin block" \
        "spoaHub.plugins.otel was removed" \
        --set spoaHub.plugins.otel.enabled=true \
        --set spoaHub.plugins.otel.timeoutMs=50
    run_helm_failure_guard \
        "SPOA Hub Helm guard: reject zero HAProxy processing margin" \
        "spoaHub.haproxy.timeoutProcessingMarginMs must be between 1 and 60000 milliseconds." \
        --set spoaHub.haproxy.timeoutProcessingMarginMs=0
    # spoaHub.enabled=true with every plugin disabled used to render a sidecar
    # plus a bootstrap config the controller then orphan-deleted, and an
    # auto-wired validator entry pointing at a file no render produced.
    # Disabling the gateway library turns off `mirror`, the only default-on
    # plugin, which is what empties the SPOE message union.
    run_helm_failure_guard \
        "SPOA Hub Helm guard: reject a forced-on sidecar with no enabled plugin" \
        "spoaHub is enabled but no enabled plugin contributes an SPOE message" \
        --set spoaHub.enabled=true \
        --set controller.templateLibraries.gateway.enabled=false
    run_helm_failure_guard \
        "Controller Helm guard: reject CRD terminology for config object name" \
        "controller.crdName was renamed to controller.configName" \
        --set-string controller.crdName=legacy-config
    run_helm_failure_guard \
        "Controller Helm guard: reject duplicate debug listener owner" \
        "controller.debugPort was removed; controller.ports.healthz is now the single source of truth" \
        --set-string controller.debugPort=8081
    run_helm_failure_guard \
        "Controller Helm guard: reject no-op metrics CR field" \
        "controller.config.controller.metricsPort was a no-op and has been removed" \
        --set-string controller.config.controller.metricsPort=9191
    run_helm_failure_guard \
        "Controller Helm guard: reject duplicate Dataplane API port owner" \
        "controller.config.dataplane.port was removed; haproxy.ports.dataplane is now the single source of truth" \
        --set-string controller.config.dataplane.port=6666
    run_helm_failure_guard \
        "Controller Helm guard: reject chart-only routing field beside CR fields" \
        "controller.config.routing moved to controller.config.templatingSettings.extraContext.routing" \
        --set-string controller.config.routing.regexMatchOrder=last
    run_helm_success_guard \
        "Controller Helm guard: accept the e2e scale tier's value set (tests/e2e/main_test.go)" \
        --set controller.resources.limits.memory=2Gi \
        --set controller.logLevel=INFO \
        --set controller.config.logging.level=INFO
    run_helm_failure_guard \
        "Controller Helm guard: reject ambiguous root workload values" \
        "image moved to controller.image so every workload setting has an explicit component owner" \
        --set-string image.repository=example.invalid/haptic
    run_helm_failure_guard \
        "Controller Helm guard: reject misplaced status-patch policy" \
        "controller.statusPatches moved to controller.config.templatingSettings.extraContext.statusPatches" \
        --set controller.statusPatches.enabled=false
    run_helm_failure_guard \
        "Controller Helm guard: reject broad legacy debug toggle" \
        "extraContext.debug uses a removed flat value" \
        --set controller.config.templatingSettings.extraContext.debug=true
    run_helm_failure_guard \
        "Controller Helm guard: reject direct metrics environment override" \
        "controller.extraEnv must not override METRICS_PORT; use controller.ports.metrics" \
        --set-string controller.extraEnv[0].name=METRICS_PORT \
        --set-string controller.extraEnv[0].value=9191
    run_helm_failure_guard \
        "HAProxy Helm guard: reject duplicate Enterprise series owner" \
        "haproxy.enterprise.version was removed; haproxyVersion now selects" \
        --set-string haproxy.enterprise.version=3.2
    # Enterprise enablement must fail only for series without a tested image
    # pin (haproxyEnterprisePatchVersions maps them to ""). The effective
    # series is the CI matrix's HAPROXY_VERSION or the chart default.
    EFFECTIVE_HAPROXY_SERIES="${HAPROXY_VERSION:-$(yq '.haproxyVersion' "$CHART_DIR/values.yaml")}"
    ENTERPRISE_PIN="$(yq ".haproxyEnterprisePatchVersions.\"${EFFECTIVE_HAPROXY_SERIES}\"" "$CHART_DIR/values.yaml")"
    if [[ -z "$ENTERPRISE_PIN" || "$ENTERPRISE_PIN" == "null" ]]; then
        run_helm_failure_guard \
            "HAProxy Helm guard: reject unpinned Enterprise series" \
            "haproxy.enterprise.enabled=true has no tested image pin for haproxyVersion \"${EFFECTIVE_HAPROXY_SERIES}\"" \
            --set haproxy.enterprise.enabled=true
    fi
fi

# PROXY-protocol opt-in profile. Unlike the profiles above this runs the WHOLE
# test set, not named tests: every test's fixtures must still render, and pass
# haproxy -c, with the opt-in on.
if [[ $FULL_RC -eq 0 ]] && ! single_test_requested "$@"; then
    PROXY_PROTOCOL_CONFIG=$(mktemp /tmp/haptic-proxy-protocol-config-XXXXXX.yaml)
    trap 'rm -f "$TEMP_CONFIG" "$RATE_LIMIT_CONFIG" "$PROXY_PROTOCOL_CONFIG"' EXIT
    echo -e "${YELLOW}Rendering PROXY-protocol profile...${NC}" >&2
    if ! helm template "$CHART_DIR" \
        --namespace default \
        $HAPROXY_VERSION_ARG \
        --set controller.templateLibraries.haproxyIngress.enabled=true \
        --set controller.templateLibraries.nginxIngress.enabled=true \
        --set controller.config.templatingSettings.extraContext.proxyProtocol.enabled=true \
        | yq 'select(.kind == "HAProxyTemplateConfig" or .kind == "HAProxyTemplateLibrary")' \
        > "$PROXY_PROTOCOL_CONFIG"; then
        echo -e "${RED}Error: Failed to render PROXY-protocol Helm profile${NC}" >&2
        exit 1
    fi
    echo -e "${YELLOW}PROXY-protocol profile: full validation pass...${NC}" >&2
    if ! "$CONTROLLER_BIN" validate --file "$PROXY_PROTOCOL_CONFIG" "${SCHEMA_DIR_ARGS[@]}" "$@"; then
        echo -e "${RED}PROXY-protocol profile: tests that pass with the opt-in OFF must also pass with it ON — the load gate keeps the controller from starting otherwise${NC}" >&2
        FULL_RC=1
    fi
fi

# Trusted-proxy opt-in profile: every test's fixtures must render and pass
# haproxy -c with extraContext.forwardedHeaders.trustedProxies set.
if [[ $FULL_RC -eq 0 ]] && ! single_test_requested "$@"; then
    TRUSTED_PROXIES_CONFIG=$(mktemp /tmp/haptic-trusted-proxies-config-XXXXXX.yaml)
    trap 'rm -f "$TEMP_CONFIG" "$RATE_LIMIT_CONFIG" "$PROXY_PROTOCOL_CONFIG" "$TRUSTED_PROXIES_CONFIG"' EXIT
    echo -e "${YELLOW}Rendering trusted-proxy profile...${NC}" >&2
    if ! helm template "$CHART_DIR" \
        --namespace default \
        $HAPROXY_VERSION_ARG \
        --set controller.templateLibraries.haproxyIngress.enabled=true \
        --set controller.templateLibraries.nginxIngress.enabled=true \
        --set controller.templateLibraries.haproxytech.enabled=true \
        --set-json 'controller.config.templatingSettings.extraContext.forwardedHeaders.trustedProxies=["10.0.0.0/8","2001:db8::/32"]' \
        | yq 'select(.kind == "HAProxyTemplateConfig" or .kind == "HAProxyTemplateLibrary")' \
        > "$TRUSTED_PROXIES_CONFIG"; then
        echo -e "${RED}Error: Failed to render trusted-proxy Helm profile${NC}" >&2
        exit 1
    fi
    echo -e "${YELLOW}Trusted-proxy profile: full validation pass...${NC}" >&2
    if ! "$CONTROLLER_BIN" validate --file "$TRUSTED_PROXIES_CONFIG" "${SCHEMA_DIR_ARGS[@]}" "$@"; then
        echo -e "${RED}Trusted-proxy profile: tests that pass with the opt-in OFF must also pass with it ON — the load gate crash-loops the controller otherwise${NC}" >&2
        FULL_RC=1
    fi
    rm -f "$TRUSTED_PROXIES_CONFIG"
fi

# Tracing opt-in profile. Runs the WHOLE test set with tracing on, for the same
# reason as the PROXY-protocol profile above, plus one specific to tracing: the
# fields and the route lookup are removed at HELM time when the opt-in is off
# (_helm_load.unset), so a per-test extraContext cannot switch them back on.
# Tests that assert on them are therefore _helm_skip_test-ed in the default
# render and only execute here. Without this profile they would never run.
if [[ $FULL_RC -eq 0 ]] && ! single_test_requested "$@"; then
    TRACING_CONFIG=$(mktemp /tmp/haptic-tracing-config-XXXXXX.yaml)
    trap 'rm -f "$TEMP_CONFIG" "$RATE_LIMIT_CONFIG" "$PROXY_PROTOCOL_CONFIG" "$TRACING_CONFIG"' EXIT
    echo -e "${YELLOW}Rendering tracing profile...${NC}" >&2
    if ! helm template "$CHART_DIR" \
        --namespace default \
        $HAPROXY_VERSION_ARG \
        --api-versions=gateway.networking.k8s.io/v1/GatewayClass \
        --set controller.templateLibraries.gateway.enabled=true \
        --set controller.templateLibraries.haproxyIngress.enabled=true \
        --set controller.templateLibraries.nginxIngress.enabled=true \
        --set controller.config.templatingSettings.extraContext.tracing.enabled=true \
        --set controller.config.templatingSettings.extraContext.tracing.otlp.endpoint=http://tempo:4318/v1/traces \
        | yq 'select(.kind == "HAProxyTemplateConfig" or .kind == "HAProxyTemplateLibrary")' \
        > "$TRACING_CONFIG"; then
        echo -e "${RED}Error: Failed to render tracing Helm profile${NC}" >&2
        exit 1
    fi
    # The skipped-by-default tests must actually be present here, or this
    # profile would pass by running nothing.
    if ! grep -q "test-tracing-enabled-mints-and-propagates" "$TRACING_CONFIG"; then
        echo -e "${RED}Tracing profile: the tracing tests are absent — this profile would test nothing${NC}" >&2
        FULL_RC=1
    fi
    echo -e "${YELLOW}Tracing profile: full validation pass...${NC}" >&2
    if ! "$CONTROLLER_BIN" validate --file "$TRACING_CONFIG" "${SCHEMA_DIR_ARGS[@]}" "$@"; then
        echo -e "${RED}Tracing profile: tests that pass with the opt-in OFF must also pass with it ON${NC}" >&2
        FULL_RC=1
    fi
fi

# Release-namespace Secrets opt-in. The fixtures hold Secrets in many namespaces;
# the deployment's watch scope must not reach them, or the load gate rejects the
# config of every install that turns the opt-in on.
if [[ $FULL_RC -eq 0 ]] && ! single_test_requested "$@"; then
    SECRET_SCOPE_CONFIG=$(mktemp /tmp/haptic-secret-scope-config-XXXXXX.yaml)
    trap 'rm -f "$TEMP_CONFIG" "$RATE_LIMIT_CONFIG" "$PROXY_PROTOCOL_CONFIG" "$TRACING_CONFIG" "$SECRET_SCOPE_CONFIG"' EXIT
    echo -e "${YELLOW}Rendering release-namespace Secrets profile...${NC}" >&2
    if ! helm template "$CHART_DIR" \
        --namespace default \
        $HAPROXY_VERSION_ARG \
        --set controller.templateLibraries.haproxyIngress.enabled=true \
        --set controller.templateLibraries.nginxIngress.enabled=true \
        --set controller.rbac.releaseNamespaceSecretsOnly=true \
        | yq 'select(.kind == "HAProxyTemplateConfig" or .kind == "HAProxyTemplateLibrary")' \
        > "$SECRET_SCOPE_CONFIG"; then
        echo -e "${RED}Error: Failed to render the release-namespace Secrets profile${NC}" >&2
        exit 1
    fi
    if [[ "$(yq 'select(.kind == "HAProxyTemplateConfig") | .spec.watchedResources.secrets.namespaces[0]' "$SECRET_SCOPE_CONFIG")" != "default" ]]; then
        echo -e "${RED}Release-namespace Secrets profile: the Secret watch is not scoped — this profile would test nothing${NC}" >&2
        FULL_RC=1
    fi
    echo -e "${YELLOW}Release-namespace Secrets profile: full validation pass...${NC}" >&2
    if ! "$CONTROLLER_BIN" validate --file "$SECRET_SCOPE_CONFIG" "${SCHEMA_DIR_ARGS[@]}" "$@"; then
        echo -e "${RED}Release-namespace Secrets profile: tests that pass with the opt-in OFF must also pass with it ON — the load gate crash-loops the controller otherwise${NC}" >&2
        FULL_RC=1
    fi
    rm -f "$SECRET_SCOPE_CONFIG"
fi

# Non-default namespace. Every render above uses `--namespace default`, so an
# assertion that pins a release-derived value — the namespace itself, the
# fullname, anything from .Release — passes here and fails on every install that
# is not in `default`. These tests run on the operator's own config through the
# fail-closed load gate, so that is not a red test: it is a controller that
# never starts. This profile is what catches it; it shipped once.
if [[ $FULL_RC -eq 0 ]] && ! single_test_requested "$@"; then
    NS_CONFIG=$(mktemp /tmp/haptic-ns-config-XXXXXX.yaml)
    trap 'rm -f "$TEMP_CONFIG" "$RATE_LIMIT_CONFIG" "$PROXY_PROTOCOL_CONFIG" "$TRACING_CONFIG" "$NS_CONFIG"' EXIT
    echo -e "${YELLOW}Rendering non-default-namespace profile...${NC}" >&2
    if ! helm template "$CHART_DIR" \
        --namespace haptic-ns-probe \
        $HAPROXY_VERSION_ARG \
        --set controller.templateLibraries.haproxyIngress.enabled=true \
        --set controller.templateLibraries.nginxIngress.enabled=true \
        --set controller.config.templatingSettings.extraContext.tracing.enabled=true \
        --set controller.config.templatingSettings.extraContext.tracing.otlp.endpoint=http://tempo:4318/v1/traces \
        | yq 'select(.kind == "HAProxyTemplateConfig" or .kind == "HAProxyTemplateLibrary")' \
        > "$NS_CONFIG"; then
        echo -e "${RED}Error: Failed to render the non-default-namespace profile${NC}" >&2
        exit 1
    fi
    echo -e "${YELLOW}Non-default namespace: full validation pass...${NC}" >&2
    if ! "$CONTROLLER_BIN" validate --file "$NS_CONFIG" "${SCHEMA_DIR_ARGS[@]}" "$@"; then
        echo -e "${RED}Tests that pass in namespace 'default' must pass in any namespace — an assertion on a release-derived value keeps the controller from starting everywhere else${NC}" >&2
        FULL_RC=1
    fi
    rm -f "$NS_CONFIG"
fi

# WAF configMapRefs opt-in profile. A configured catalog source makes EVERY
# render need that ConfigMap in its fixture store; the chart covers this by
# synthesising a _global catalog fixture per referenced identity into the
# operator object (templates/haproxytemplateconfig.yaml). This profile runs
# the whole set with a ref configured — it is what fails when the synthesis
# is lost (observed: 594/690 tests failing in the e2e preflight hook while
# the default render stayed green).
if [[ $FULL_RC -eq 0 ]] && ! single_test_requested "$@"; then
    WAF_REFS_CONFIG=$(mktemp /tmp/haptic-waf-refs-config-XXXXXX.yaml)
    trap 'rm -f "$TEMP_CONFIG" "$RATE_LIMIT_CONFIG" "$PROXY_PROTOCOL_CONFIG" "$TRACING_CONFIG" "$NS_CONFIG" "$WAF_REFS_CONFIG"' EXIT
    echo -e "${YELLOW}Rendering WAF configMapRefs profile...${NC}" >&2
    if ! helm template "$CHART_DIR" \
        --namespace default \
        $HAPROXY_VERSION_ARG \
        --set controller.templateLibraries.haproxyIngress.enabled=true \
        --set controller.templateLibraries.nginxIngress.enabled=true \
        --set-string controller.config.templatingSettings.extraContext.waf.policies.configMapRefs.security.namespace=security \
        --set-string controller.config.templatingSettings.extraContext.waf.policies.configMapRefs.security.name=haptic-waf-policies \
        --set-string controller.config.templatingSettings.extraContext.waf.policies.configMapRefs.security.key=policies.yaml \
        | yq 'select(.kind == "HAProxyTemplateConfig" or .kind == "HAProxyTemplateLibrary")' \
        > "$WAF_REFS_CONFIG"; then
        echo -e "${RED}Error: Failed to render the WAF configMapRefs profile${NC}" >&2
        exit 1
    fi
    # The synthesised fixture must be present, or the pass below succeeds for
    # the wrong reason the day the values path stops reaching the injection.
    if ! grep -q "haptic-validation-fixture" "$WAF_REFS_CONFIG"; then
        echo -e "${RED}WAF configMapRefs profile: the synthesised _global catalog fixture is absent — every test would render against a missing ConfigMap${NC}" >&2
        FULL_RC=1
    fi
    echo -e "${YELLOW}WAF configMapRefs profile: full validation pass...${NC}" >&2
    if ! "$CONTROLLER_BIN" validate --file "$WAF_REFS_CONFIG" "${SCHEMA_DIR_ARGS[@]}" "$@"; then
        echo -e "${RED}WAF configMapRefs profile: tests that pass without a catalog source must also pass with one — the load gate keeps the controller from starting otherwise${NC}" >&2
        FULL_RC=1
    fi
    rm -f "$WAF_REFS_CONFIG"
fi

# ---------------------------------------------------------------------------
# Degraded Gateway API profiles (skipped when a single --test is requested or
# a custom schema dir is in play). For each committed old-release CRD bundle
# (tests/schemas-ga-*), render the STANDARD-channel chart, resolve against the
# bundle (the controller strips features whose CRDs the release doesn't serve
# AND tests whose requiresFields name schema fields the release's generation
# lacks), and require:
#   (a) ZERO failing tests — with field-level stripping in place, every test
#       that would fail on that schema generation must have been stripped
#       instead (a failure here is exactly the load-gate failure of
#       issue #59); and
#   (b) the set of STRIPPED tests (the validate CLI's "⊘ <name> stripped:"
#       lines) to EXACTLY match the bundle's allowlist
#       (tests/schemas-ga-<rel>/expected-stripped.txt). A newly-stripped
#       test OR a stale allowlist entry both fail the run.
# ---------------------------------------------------------------------------
if [[ $FULL_RC -eq 0 && ${#SCHEMA_DIR_ARGS[@]} -gt 0 ]] && ! single_test_requested "$@"; then
    STD_CONFIG=$(mktemp /tmp/haptic-std-config-XXXXXX.yaml)
    trap 'rm -f "$TEMP_CONFIG" "${RATE_LIMIT_CONFIG:-}" "${REQUEST_VALIDATION_CONFIG:-}" "$STD_CONFIG"' EXIT
    helm template "$CHART_DIR" --namespace default $HAPROXY_VERSION_ARG \
        --set controller.templateLibraries.gateway.enabled=true \
        --set controller.templateLibraries.hapticAnnotations.enabled=true \
        --set controller.templateLibraries.haproxytech.enabled=true \
        --set controller.templateLibraries.haproxyIngress.enabled=true \
        --set controller.templateLibraries.nginxIngress.enabled=true \
        | yq 'select(.kind == "HAProxyTemplateConfig" or .kind == "HAProxyTemplateLibrary")' > "$STD_CONFIG"
    for BUNDLE in "$PROJECT_ROOT"/tests/schemas-ga-*; do
        [[ -d "$BUNDLE" ]] || continue
        REL=$(basename "$BUNDLE")
        MERGED=$(mktemp -d /tmp/haptic-schemas-XXXXXX)
        cp "$PROJECT_ROOT"/tests/schemas/core_v1_*.yaml \
           "$PROJECT_ROOT"/tests/schemas/discovery_*.yaml \
           "$PROJECT_ROOT"/tests/schemas/haproxy-haptic.org_*.yaml \
           "$PROJECT_ROOT"/tests/schemas/networking_*.yaml "$MERGED/"
        # The "none" profile (no Gateway API at all) has no CRDs to add.
        find "$BUNDLE" -name '*.gateway.networking.k8s.io.yaml' -exec cp {} "$MERGED/" \;
        echo -e "${YELLOW}Degraded profile ${REL}...${NC}" >&2
        DEGRADED_RC=0
        DEGRADED_OUT=$("$CONTROLLER_BIN" validate --file "$STD_CONFIG" --schema-dir "$MERGED" 2>&1) || DEGRADED_RC=$?
        rm -rf "$MERGED"
        FAILED=$(printf '%s\n' "$DEGRADED_OUT" | grep '^✗' | awk '{print $2}' | sort || true)
        if [[ $DEGRADED_RC -ne 0 || -n "$FAILED" ]]; then
            echo -e "${RED}Degraded profile ${REL}: expected ZERO failing tests (rc=${DEGRADED_RC})${NC}" >&2
            echo -e "${RED}Failing tests must be stripped via requires/requiresFields, not fail the run:${NC}" >&2
            printf '%s\n' "$FAILED" >&2
            printf '%s\n' "$DEGRADED_OUT" | tail -30 >&2
            exit 1
        fi
        # Match only schema-strip lines ("⊘ <name> stripped: <reason>").
        # The test runner separately prints "⊘ <name> [skipped]" for
        # minHAProxyVersion skips, which vary with the HAProxy version of
        # the validating binary (this script runs per version in CI) and
        # must not perturb the schema-generation allowlist.
        ACTUAL=$(printf '%s\n' "$DEGRADED_OUT" | grep '^⊘ .* stripped: ' | awk '{print $2}' | sort || true)
        EXPECTED=$(grep -v '^#' "$BUNDLE/expected-stripped.txt" 2>/dev/null | grep -v '^$' | sort || true)
        if [[ "$ACTUAL" != "$EXPECTED" ]]; then
            echo -e "${RED}Degraded profile ${REL}: stripped-test set does not match ${BUNDLE}/expected-stripped.txt${NC}" >&2
            diff <(echo "$EXPECTED") <(echo "$ACTUAL") >&2 || true
            exit 1
        fi
        echo -e "${GREEN}Degraded profile ${REL}: zero failures, stripped set matches allowlist${NC}" >&2
    done
fi
# Exercise the whole load gate with administrator-enabled fleet compression.
if [[ $FULL_RC -eq 0 ]] && ! single_test_requested "$@"; then
    COMPRESSION_CONFIG=$(mktemp /tmp/haptic-compression-config-XXXXXX.yaml)
    echo -e "${YELLOW}Rendering fleet compression profile...${NC}" >&2
    if ! helm template "$CHART_DIR" \
        --namespace default \
        $HAPROXY_VERSION_ARG \
        --set controller.templateLibraries.haproxyIngress.enabled=true \
        --set controller.templateLibraries.nginxIngress.enabled=true \
        --set-string controller.config.templatingSettings.extraContext.governance.rules.haptic-compress-enable.default=true \
        | yq 'select(.kind == "HAProxyTemplateConfig" or .kind == "HAProxyTemplateLibrary")' \
        > "$COMPRESSION_CONFIG"; then
        rm -f "$COMPRESSION_CONFIG"
        echo -e "${RED}Failed to render fleet compression profile${NC}" >&2
        exit 1
    fi
    echo -e "${YELLOW}Fleet compression profile: full validation pass...${NC}" >&2
    if ! "$CONTROLLER_BIN" validate --file "$COMPRESSION_CONFIG" "${SCHEMA_DIR_ARGS[@]}" "$@"; then
        FULL_RC=1
    fi
    rm -f "$COMPRESSION_CONFIG"
fi
# Opt-out profiles. `_helm_load` drops snippets and libraries by release
# values, so every test asserting on them must be absent with them (#331).
if [[ $FULL_RC -eq 0 ]] && ! single_test_requested "$@"; then
    OPT_OUT_CONFIG=$(mktemp /tmp/haptic-opt-out-config-XXXXXX.yaml)
    for OPT_OUT in \
        "vector.enabled=false,spoaHub.enabled=false" \
        "vector.requestMetrics.enabled=false,spoaHub.plugins.mirror.enabled=false,spoaHub.plugins.coraza.enabled=true"; do
        echo -e "${YELLOW}Opt-out profile ${OPT_OUT}: full validation pass...${NC}" >&2
        if ! helm template "$CHART_DIR" \
            --namespace default \
            $HAPROXY_VERSION_ARG \
            --set controller.templateLibraries.gateway.experimentalChannel=true \
            --set controller.templateLibraries.haproxytech.enabled=true \
            --set controller.templateLibraries.haproxyIngress.enabled=true \
            --set controller.templateLibraries.nginxIngress.enabled=true \
            --set "$OPT_OUT" \
            | yq 'select(.kind == "HAProxyTemplateConfig" or .kind == "HAProxyTemplateLibrary")' \
            > "$OPT_OUT_CONFIG"; then
            rm -f "$OPT_OUT_CONFIG"
            echo -e "${RED}Failed to render opt-out profile ${OPT_OUT}${NC}" >&2
            exit 1
        fi
        if ! "$CONTROLLER_BIN" validate --file "$OPT_OUT_CONFIG" "${SCHEMA_DIR_ARGS[@]}" "$@"; then
            FULL_RC=1
        fi
    done
    rm -f "$OPT_OUT_CONFIG"
fi
exit $FULL_RC
