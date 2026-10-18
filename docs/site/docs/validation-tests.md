# Test your templates

<a id="validation-tests"></a>

<a id="overview"></a>

Check that a template produces the configuration you intend before using it for
live traffic. Each validation test supplies sample resources, called fixtures,
and assertions about the output: for example, that an Ingress creates a backend
or that an invalid annotation is rejected.

Define tests in `HAProxyTemplateConfig` or its libraries and run them with
`haptic validate`. The controller repeats them when loading configuration.
Tests cover the cases you supply, so include missing and invalid inputs as well
as a working route.

A failed configuration change leaves the previous configuration in place. A
failed startup load prevents the controller from becoming ready. To validate
Helm values and libraries together before rollout, use [preflight](operations/validate-before-deploy.md).

## Run the installed tests

[Install the CLI](cli.md) and a matching HAProxy binary, or use the
[container command](cli.md#use-a-container-on-macos-windows-or-linux).
Use the same HAPTIC and HAProxy versions as your installation, with `kubectl`
access to that cluster. Prepare a
[schema directory](#prepare-schemas) for typed resource access.

Export the installed configuration:

```bash
umask 077
kubectl exec --namespace haptic deployment/haptic-controller --container controller \
  -- haptic config view --input --namespace haptic > config.yaml
```

Run its bundled tests with the local CLI, or use the
[container command](cli.md#use-a-container-on-macos-windows-or-linux):

```bash
haptic validate -f config.yaml --schema-dir ./schemas
```

`config view --input` merges the configuration and its referenced libraries.
Exporting only the `HAProxyTemplateConfig` would omit the libraries. Add your own
tests under `spec.validationTests` in the exported file and run the same command
again. To keep those tests across upgrades, put them under
`controller.config.validationTests` in your Helm values.

<a id="quick-start"></a>

## Write a test in the browser

This example generates one backend for each Service. The test supplies a sample
Service and checks that its backend appears exactly once. Press **Run live**
to inspect the assertions in **tests**; no cluster or local binaries are needed:

<div class="pg-embed" markdown data-tab="tests" data-controls="tabs" data-title="Validation tests, live" data-height="560">

```yaml
watchedResources:
  services:
    apiVersion: v1
    resources: services
    indexBy:
      - metadata.namespace
      - metadata.name

haproxyConfig:
  template: |
    global
      maxconn 1000

    defaults
      mode http
      timeout connect 5s
      timeout client 30s
      timeout server 30s

    frontend http
      bind :8080
      default_backend not-found
    {%- for _, svc := range resources.services.List() %}
    backend {{ svc.metadata.namespace }}_{{ svc.metadata.name }}
      server app 127.0.0.1:8080
    {%- end %}

    backend not-found
      http-request deny deny_status 404

# Tests render this config against fixture resources and check the output.
validationTests:
  one-backend-per-service:
    description: Each Service becomes its own backend
    fixtures:
      services:
        - apiVersion: v1
          kind: Service
          metadata:
            name: shop
            namespace: storefront
          spec:
            ports:
              - port: 80
    assertions:
      - type: haproxy_valid
        description: Rendered config is valid
      - type: contains
        target: haproxy.cfg
        pattern: "backend storefront_shop"
        description: A backend exists for the shop Service
      - type: match_count
        target: haproxy.cfg
        pattern: "(?m)^backend "
        expected: "2"
        description: Exactly two backends (shop + not-found)
```

<p class="pg-task" markdown>Add a second Service with a different name to `fixtures.services`. Run the tests and inspect the failed count assertion. Change `expected` from `"2"` to `"3"` and run again to match the two Services plus the fallback backend.</p>

The browser checks syntax and schema. It can't run `haproxy -c`; run the tests
locally before deploying. Use **Re-run tests** after editing.

</div>

<a id="test-structure"></a>
<a id="fixtures"></a>
<a id="http-fixtures"></a>
<a id="current-servers"></a>
<a id="fixture-keys"></a>
<a id="the-reserved-_global-entry"></a>
<a id="conditional-tests-requires-and-requiresfields"></a>
<a id="assertion-types"></a>
<a id="assertion-targets"></a>
<a id="haproxy_valid"></a>
<a id="contains"></a>
<a id="not_contains"></a>
<a id="equals"></a>
<a id="jsonpath"></a>
<a id="match_count"></a>
<a id="match_order"></a>
<a id="deterministic"></a>

## Fixture and assertion reference

Use the [test reference](validation-reference.md) to look up fixture fields,
shared fixtures, conditional tests, output targets, and assertion types.

## Prepare schemas

`haptic validate` reads schemas from a directory; it doesn't fetch them from
your cluster. Supply them when templates use typed resource access, including
the bundled chart templates.

The [source repository](https://github.com/haproxy-haptic/HAPTIC) includes
`tests/schemas` for Kubernetes resources, Gateway API, and HAPTIC's custom resources.
Copy the bundle for your release into a new working directory. This method
requires Git and Bash; enter the release version without a leading `v`:

```bash
read -r -p "HAPTIC release version: " haptic_version
git clone --depth 1 --branch "v${haptic_version}" \
  https://github.com/haproxy-haptic/HAPTIC.git haptic-source
cp -R haptic-source/tests/schemas ./schemas
```

If you already have the matching source checkout, copy its `tests/schemas`
directory instead. Use a bundle that matches the release and API schemas you
intend to support; newer schema fields can make a local test pass for a feature
that an older cluster doesn't provide.

For your own resource types, the directory also accepts full CRD YAML files and
OpenAPI v3 schemas. A full CRD includes the resource name, group, and versions
HAPTIC needs to resolve a watch. For checking Helm values against the target
cluster's live schemas, use [preflight](operations/validate-before-deploy.md).

## Running tests

The commands below assume `config.yaml` contains your configuration and
`./schemas` contains its schemas:

```bash
# Run all tests
haptic validate -f config.yaml --schema-dir ./schemas

# Run specific test
haptic validate -f config.yaml --schema-dir ./schemas --test test-basic-routing

# Run several bundled Ingress tests with one template compilation
haptic validate -f config.yaml --schema-dir ./schemas \
  --test test-ingress-host-map-incremental-order-and-duplicates \
  --test test-ingress-host-map-incremental-empty

# Output formats
haptic validate -f config.yaml --schema-dir ./schemas --output json
haptic validate -f config.yaml --schema-dir ./schemas --output yaml

# Explicit parallelism (0=automatic CPU and memory budget, 1=sequential)
haptic validate -f config.yaml --schema-dir ./schemas --workers 4

```

HAPTIC chooses validation parallelism from the available CPU and memory.
`--workers` overrides this choice; every selected test still runs.

Install `haproxy` on your `PATH` before running `haptic validate`. The command
uses it to detect the version and run `haproxy_valid` assertions; validation fails
if the binary is missing. To test a specific HAProxy version, use the matching
controller image, which includes both binaries.

You can set `HAPTIC_SCHEMA_DIR` instead of passing `--schema-dir` each time.
Without schemas, templates must use untyped resource access such as `dig()`.

Exit code 0 means all tests passed.

### Run in CI

Add `haptic validate` to your pipeline and let a nonzero exit status fail the job.
Use the [controller image](operations/haproxy-versions.md) whose HAProxy version
matches your deployment. Commit `config.yaml` and the matching `schemas/` directory
to the repository so the job has both inputs.

GitLab CI (`.gitlab-ci.yml`) — override the image entrypoint so the job's `script` shell runs:

```yaml
validate-haptic-config:
  image:
    name: ghcr.io/haproxy-haptic/haptic:0.6.0-haproxy3.4
    entrypoint: [""]
  script:
    - haptic validate -f config.yaml --schema-dir ./schemas
```

GitHub Actions (`.github/workflows/validate.yml`):

```yaml
jobs:
  validate:
    runs-on: ubuntu-latest
    container:
      image: ghcr.io/haproxy-haptic/haptic:0.6.0-haproxy3.4
    steps:
      - uses: actions/checkout@v4
      - run: haptic validate -f config.yaml --schema-dir ./schemas
```

### Output example

```
✓ test-basic-routing (0.125s)
  ✓ HAProxy configuration must be syntactically valid
  ✓ Must have frontend

✗ test-tls-config (0.089s)
  ✗ Must have SSL certificate
    Error: pattern "ssl crt" not found in haproxy.cfg

Tests: 1 passed, 1 failed, 2 total (0.214s)
```

## Debugging failed tests

### `--verbose`

Shows content preview for failed assertions:

```bash
haptic validate -f config.yaml --schema-dir ./schemas --verbose
```

```
✗ test-gateway-routing
  ✗ Path map must have correct weight
    Error: pattern "MULTIBACKEND:100:" not found in map:path-prefix.map
    Content preview:
      split.example.com/app MULTIBACKEND:0:default_split-route_0/
```

### `--dump-rendered`

Shows all rendered content after test results:

```bash
haptic validate -f config.yaml --schema-dir ./schemas --dump-rendered
```

### `--trace-templates`

Shows top-level template execution order and timing:

```bash
haptic validate -f config.yaml --schema-dir ./schemas --trace-templates
```

```
Rendering: haproxy.cfg
Completed: haproxy.cfg (0.007ms)
Rendering: path-prefix.map
Completed: path-prefix.map (3.347ms)
```

### `--profile-includes`

Lists the 20 slowest includes and macro calls with cumulative timings:

```bash
haptic validate -f config.yaml --schema-dir ./schemas --profile-includes
```

### `--debug-filters`

Logs every comparison made by sort filters (`sort_by`) and similar operations, with the input types and the comparison result. Useful when route precedence or map ordering doesn't match what you expected:

```bash
haptic validate -f config.yaml --schema-dir ./schemas --debug-filters
```

<a id="combining-flags"></a>

## Test failure cases and files

<a id="testing-strategies"></a>

<a id="test-organization"></a>

### Testing template errors

A negative test passes when the render fails *as expected*. Assert on the `rendering_error` target (see [Assertion Targets](validation-reference.md#assertion-targets)) so the deliberate `fail()` is treated as the pass condition — without it, the failed render marks the whole test red:

```yaml
test-no-services-error:
  description: Should fail when no services exist
  fixtures:
    services: []
  assertions:
    - type: contains
      target: rendering_error
      pattern: "no services configured"
      description: Render is rejected with the expected fail() message
```

`rendering_error` can't accept a resource read, ambiguous `GetSingle`, or typed conversion failure. Those failures always fail the test.

### Testing auxiliary files

```yaml
test-hostname-map:
  description: Hostname map should contain all ingress hosts
  fixtures:
    ingresses:
      - metadata:
          name: main
        spec:
          rules:
            - host: api.example.com
  assertions:
    - type: contains
      target: map:hostnames.map
      pattern: "api.example.com"
```

<a id="best-practices"></a>

## Choose useful test cases

For a custom annotation, test a resource with the annotation, one without it,
and one with an invalid value. Check both the output you expect and directives
that must be absent. For a resource lookup, include a missing reference and an
empty result.

Keep fixtures focused on the behavior under test. Name each test after that
behavior, such as `ingress-with-tls-creates-https-frontend`, so a failure tells
you which requirement stopped working.

## Troubleshooting

| Problem | Solution |
|---------|----------|
| "haproxy: command not found" | Install HAProxy locally (the validator invokes `haproxy -c` on your `PATH`) |
| "template rendering failed" | Check for undefined variables, missing filters |
| Pattern not matching | Escape regex chars, check whitespace, use simpler patterns |
| JSONPath returns no results | Check the path; `jsonpath` reads scalar context values (for example `extraContext` keys), not the resource stores — assert on resources with `contains` / `match_count` |

<a id="complete-example"></a>

Use the [resource lookup examples](template-resources.md#cross-resource-lookups)
when writing tests that involve more than one resource type.

## See also

- [Template syntax](template-language.md) — expressions, loops, and conditions
- [Supported Configuration](./supported-configuration.md) - HAProxy directives
- [Troubleshooting](./troubleshooting.md) - Common issues
