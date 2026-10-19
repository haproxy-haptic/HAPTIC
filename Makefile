.PHONY: help version lint lint-fix lint-chart lint-chart-ci audit check-all \
        test test-unit test-integration test-agent-docker test-acceptance test-acceptance-parallel test-e2e test-gateway-conformance test-ingress-conformance test-helm-defaults test-release-namespace-secrets build-integration-test \
        test-coverage test-integration-coverage test-coverage-combined bench bench-gateway-api \
        build check-source-hash docker-build docker-build-multiarch docker-build-multiarch-push docker-load-kind docker-push docker-clean \
        spoa-prep spoa-hub-image spoa-bundle-render spoa-bundle-check test-spoa-reload \
        tidy vendor verify verify-generate generate generate-chart-defaults clean fmt vet install-tools dev \
        release test-release check-controller-output goreleaser-snapshot \
        pgo-profile pgo-merge \
        extract-schemas chart-schema chart-schema-check

.DEFAULT_GOAL := help

# Variables
# env -u GOROOT: strips stale GOROOT set by IDEs (e.g. IntelliJ) so the asdf-managed
# Go toolchain is used consistently across all make targets including lint and audit.
GO := env -u GOROOT go
# renovate: datasource=github-releases depName=golangci/golangci-lint
GOLANGCI_LINT_VERSION := v2.14.0
GOLANGCI_LINT := $(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
GOVULNCHECK := $(GO) run golang.org/x/vuln/cmd/govulncheck
# renovate: datasource=github-releases depName=rhysd/actionlint
ACTIONLINT_VERSION := v1.7.12
ACTIONLINT := $(GO) run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)
# renovate: datasource=github-releases depName=losisin/helm-values-schema-json
HELM_VALUES_SCHEMA_JSON_VERSION := v2.6.0
HELM_VALUES_SCHEMA_JSON := $(GO) run github.com/losisin/helm-values-schema-json/v2@$(HELM_VALUES_SCHEMA_JSON_VERSION)
CHART_SCHEMA_FLAGS := --values charts/haptic/values.yaml --values charts/haptic/values.schema.extras.yaml \
	--draft 7 --indent 2 --no-additional-properties
ARCH_GO := $(shell which arch-go 2>/dev/null || echo "$(GO) run github.com/arch-go/arch-go/v2")
CONTROLLER_GEN := $(GO) run sigs.k8s.io/controller-tools/cmd/controller-gen

# Docker variables
IMAGE_NAME ?= haptic# Container image name (override: IMAGE_NAME=my-image)
IMAGE_TAG ?= dev# Image tag (override: IMAGE_TAG=v1.0.0)
REGISTRY ?=# Container registry (e.g., ghcr.io/myorg)
FULL_IMAGE := $(if $(REGISTRY),$(REGISTRY)/$(IMAGE_NAME):$(IMAGE_TAG),$(IMAGE_NAME):$(IMAGE_TAG))
DOCKER_BINARY_ARCH ?= amd64
DOCKER_BINARY_CONTEXT ?=
DOCKER_BUILD_FLAGS ?=
# HAProxy version baked into the controller image at build time.
# Override per-build with HAPROXY_VERSION=3.x.
HAPROXY_VERSION ?= $(shell yq -r '.haproxyVersion' charts/haptic/values.yaml)
KIND_CLUSTER ?= haptic-dev  # Kind cluster name for local testing
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
GIT_TAG := $(shell git describe --tags --exact-match 2>/dev/null || echo "dev")
VERSION := $(shell cat VERSION 2>/dev/null || echo "dev")
SOURCE_HASH := $(shell ./scripts/source-hash.sh 2>/dev/null || echo "unknown")
CHART_VERSION := $(shell grep '^version:' charts/haptic/Chart.yaml 2>/dev/null | awk '{print $$2}' || echo "dev")

# Coverage packages (excludes generated code)
COVERAGE_PACKAGES := ./cmd/...,./pkg/compression/...,./pkg/controller/...,./pkg/core/...,./pkg/dataplane/...,./pkg/events/...,./pkg/httpstore/...,./pkg/incremental/...,./pkg/introspection/...,./pkg/k8s/...,./pkg/lifecycle/...,./pkg/metrics/...,./pkg/persistenttree/...,./pkg/rendercontent/...,./pkg/stores/...,./pkg/templating/...,./pkg/webhook/...

# Default target
help: ## Show this help message
	@echo "Available targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-30s\033[0m %s\n", $$1, $$2}'

version: ## Display version information
	@echo "Version Information:"
	@echo "  Controller: $(VERSION)"
	@echo "  Chart:      $(CHART_VERSION)"
	@echo "  Git Commit: $(GIT_COMMIT)"
	@echo "  Git Tag:    $(GIT_TAG)"
	@echo "  Image:      $(FULL_IMAGE)"

## Linting targets

lint: vendor ## Run all linters (YAML, JSON, Markdown, Go)
	@$(MAKE) test-ci-rules test-admission-readiness
	@echo "Checking go.mod/go.sum are tidy..."
	@$(GO) mod tidy -diff || { echo "go.mod/go.sum are not tidy. Run 'go mod tidy' and commit the result."; exit 1; }
	@echo "Checking test inventory (every test must run somewhere)..."
	./scripts/check-test-inventory.sh
	@echo "Checking the no-Lua feature policy..."
	$(MAKE) check-no-lua
	@echo "Checking template libraries parse as YAML..."
	./scripts/check-library-yaml.sh
	@echo "Checking no comment fuses the lines around it..."
	./scripts/check-comment-fusion.sh
	@echo "Checking migration-coverage drift (declared vs. annotations read)..."
	./scripts/check-migration-coverage.sh
	@echo "Checking vendor annotation docs coverage..."
	./scripts/check-annotation-docs.sh
	@echo "Checking vendor annotation docs Status matches _migrationCoverage..."
	./scripts/check-annotation-status.sh
	@echo "Checking annotation compatibility tables are up-to-date..."
	./scripts/gen-migration-docs.sh --check
	@echo "Checking chart values docs coverage..."
	./scripts/check-values-docs.sh
	@echo "Checking denied_by metric values match the chart..."
	./scripts/check-shipped-log-metrics.sh
	@echo "Checking image pins and supported HAProxy series agree..."
	./scripts/check-image-pins.sh
	@echo "Checking every webhook-routed kind has a Go handler..."
	./scripts/check-webhook-kinds.sh
	@echo "Checking the Gateway API release is read from go.mod, not pinned..."
	./scripts/check-gwapi-version-pins.sh
	@echo "Checking storage paths are resolved, not literal..."
	./scripts/check-storage-dir-literals.sh
	@echo "Checking playground/facade highlight bundle is in sync..."
	node scripts/check-highlight-bundle.mjs
	@$(MAKE) test-playground-web
	@echo "Linting YAML files..."
	yamllint -c .yamllint.yml .
	@echo "Linting JSON files..."
	@for f in renovate.json; do \
		echo "  Checking $$f..."; \
		jq empty "$$f" || exit 1; \
	done
	@echo "Linting Markdown files..."
	markdownlint-cli2 "**/*.md" "#node_modules" "#**/node_modules" "#.claude" "#.codex" "#vendor" "#.cache" "#.remember" "#docs/*/site"
	@echo "Linting docs prose (vale)..."
	@command -v vale >/dev/null 2>&1 \
		|| { echo "vale not found — install it (Arch: pacman -S vale; other: https://vale.sh)"; exit 1; }
	vale docs/site/docs
	@echo "Running golangci-lint..."
ifdef CI
	$(GOLANGCI_LINT) run --output.code-climate.path=gl-code-quality-report.json \
		./cmd/... ./examples/... ./pkg/... ./tests/... ./tools/...
else
	$(GOLANGCI_LINT) run \
		./cmd/... ./examples/... ./pkg/... ./tests/... ./tools/...
endif
	@echo "Running golangci-lint over the playground-tagged files..."
	$(GOLANGCI_LINT) run --build-tags=playground ./pkg/dataplane/... ./pkg/generated/validators/...
	@$(MAKE) lint-e2e lint-integration lint-testinfra lint-acceptance lint-conformance lint-agent
	@echo "Checking no production binary links a HAProxy config parser..."
	./scripts/check-client-native-free.sh
	@echo "Running arch-go..."
	$(ARCH_GO)
	@echo "Running event immutability checker..."
	@mkdir -p bin
	@cd tools/linters/eventimmutability && $(GO) build -o ../../../bin/eventimmutability ./cmd/eventimmutability
	@env -u GOROOT ./bin/eventimmutability ./...
	@echo "Verifying generated code..."
	@$(MAKE) verify-generate

lint-fix: ## Run golangci-lint with auto-fix
	@echo "Running golangci-lint with auto-fix..."
	$(GOLANGCI_LINT) run --fix ./cmd/... ./examples/... ./pkg/... ./tests/... ./tools/...

.PHONY: lint-e2e lint-e2e-fix
.PHONY: lint-testinfra
lint-testinfra: ## Lint shared test infrastructure and its process integration tests
	$(GOLANGCI_LINT) run --build-tags=testinfra ./tests/process/... ./tests/e2e/availability/... ./tests/kubeexec/... ./tests/admission/... ./tests/testutil/... ./tests/tunnel/... ./tests/kindutil/... ./tests/scenarios/... ./tests/runner/... ./tests/httpclient/... ./tests/traffic/... ./tests/publication/... ./tests/fixtures/...

lint-e2e: ## Lint the full-stack e2e suite without creating a cluster
	$(GOLANGCI_LINT) run --build-tags=e2e ./tests/e2e/...

lint-e2e-fix: ## Apply supported automatic fixes to the full-stack e2e suite
	$(GOLANGCI_LINT) run --fix --build-tags=e2e ./tests/e2e/...

.PHONY: lint-integration
lint-integration: ## Lint the integration suite without creating a cluster
	$(GOLANGCI_LINT) run --build-tags=integration ./tests/integration/...

.PHONY: lint-acceptance
lint-acceptance: ## Lint the acceptance suite without creating a cluster
	$(GOLANGCI_LINT) run --build-tags=acceptance ./tests/acceptance/...

.PHONY: lint-conformance
lint-conformance: ## Lint the Gateway API and Ingress conformance suites without creating a cluster
	$(GOLANGCI_LINT) run --build-tags=gateway_conformance ./tests/conformance/...
	$(GOLANGCI_LINT) run --build-tags=ingress_conformance ./tests/conformance/...

.PHONY: lint-agent
lint-agent: ## Lint the Docker-backed agent suite without starting containers
	$(GOLANGCI_LINT) run --build-tags=agentdocker ./tests/agent/...

## Chart linting

# renovate: datasource=docker depName=quay.io/helmpack/chart-testing versioning=docker
CT_VERSION := v3.14.0
# renovate: datasource=docker depName=helmunittest/helm-unittest versioning=docker
HELM_UNITTEST_VERSION := 4.2.3-1.1.1
# renovate: datasource=docker depName=ghcr.io/yannh/kubeconform versioning=docker
KUBECONFORM_VERSION := v0.8.0-alpine
# renovate: datasource=docker depName=kindest/node
KUBE_VERSION := 1.37.0

lint-chart: ## Run chart linting (ct lint, helm-unittest, kubeconform) via Docker
	@$(MAKE) --no-print-directory chart-schema-check
	@echo "Running chart-testing lint..."
	docker run --rm -v $(PWD):/data -w /data quay.io/helmpack/chart-testing:$(CT_VERSION) \
		ct lint --config charts/haptic/.ct/ct.yaml --all
	@echo ""
	@echo "Running helm-unittest..."
	docker run --rm -v $(PWD)/charts/haptic:/apps \
		helmunittest/helm-unittest:$(HELM_UNITTEST_VERSION) .
	@echo ""
	@echo "Running kubeconform..."
	helm template charts/haptic \
		--kube-version $(KUBE_VERSION) \
		--api-versions=gateway.networking.k8s.io/v1/GatewayClass \
		--api-versions=gateway.networking.k8s.io/v1/TCPRoute \
		| docker run --rm -i ghcr.io/yannh/kubeconform:$(KUBECONFORM_VERSION) \
			-kubernetes-version $(KUBE_VERSION) \
			-schema-location default \
			-schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json' \
			-skip haproxy-haptic.org/v1alpha1/HAProxyTemplateConfig,haproxy-haptic.org/v1alpha1/HAProxyTemplateLibrary,haproxy-haptic.org/v1alpha1/HAProxyValidationTests,haproxy-haptic.org/v1alpha1/HAProxyConfig,haproxy-haptic.org/v1alpha1/HAProxyMapFile \
			-summary
	@echo ""
	@echo "Checking rendered CRs against the chart's own CRDs..."
	@$(MAKE) --no-print-directory cr-spec-conformance-check
	@echo ""
	@echo "Checking release-Secret size..."
	@$(MAKE) --no-print-directory chart-size-check
	@echo ""
	@echo "Checking per-object config size..."
	@$(MAKE) --no-print-directory cr-size-check
	@$(MAKE) --no-print-directory vector-config-check
	@echo ""
	@echo "All chart linting passed!"

# CI target (runs all chart linting - tools must be installed)
lint-chart-ci: ## Run all chart linting for CI (requires ct, helm-unittest, kubeconform)
	@$(MAKE) --no-print-directory chart-schema-check
	@echo "Running chart-testing lint..."
	ct lint --config charts/haptic/.ct/ct.yaml --all
	@echo ""
	@echo "Running helm-unittest..."
	helm unittest charts/haptic --output-type JUnit --output-file chart-test-results.xml
	@echo ""
	@echo "Running kubeconform..."
	helm template charts/haptic \
		--kube-version $(KUBE_VERSION) \
		--api-versions=gateway.networking.k8s.io/v1/GatewayClass \
		--api-versions=gateway.networking.k8s.io/v1/TCPRoute \
		| kubeconform \
			-kubernetes-version $(KUBE_VERSION) \
			-schema-location default \
			-schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json' \
			-skip haproxy-haptic.org/v1alpha1/HAProxyTemplateConfig,haproxy-haptic.org/v1alpha1/HAProxyTemplateLibrary,haproxy-haptic.org/v1alpha1/HAProxyValidationTests,haproxy-haptic.org/v1alpha1/HAProxyConfig,haproxy-haptic.org/v1alpha1/HAProxyMapFile \
			-summary
	@echo ""
	@echo "Checking rendered CRs against the chart's own CRDs..."
	@$(MAKE) --no-print-directory cr-spec-conformance-check
	@echo ""
	@echo "Checking release-Secret size..."
	@$(MAKE) --no-print-directory chart-size-check
	@echo ""
	@echo "Checking per-object config size..."
	@$(MAKE) --no-print-directory cr-size-check
	@$(MAKE) --no-print-directory vector-config-check
	@echo ""
	@echo "All chart linting passed!"

# The heaviest REALISTIC release-Secret profile: the default install plus the
# single heaviest vendor annotation-compat layer. An operator enables at most
# one of these three (the one matching their existing controller), never all
# three at once — see scripts/check-chart-release-size.py's header for the
# full default+vendor measurement table that picked nginxIngress as heaviest.
REALISTIC_MAX_LIBS := --set controller.templateLibraries.nginxIngress.enabled=true

# The worst-case library profile: every bundled library enabled at once.
# BLOCKING for cr-size-check (the etcd per-object limit genuinely cares about
# the largest object any operator could ever produce). INFORMATIONAL ONLY for
# chart-size-check — no operator runs this combination; that gate uses
# REALISTIC_MAX_LIBS above instead (see check-chart-release-size.py's header).
WORST_CASE_LIBS := \
	--set controller.templateLibraries.nginxIngress.enabled=true \
	--set controller.templateLibraries.haproxytech.enabled=true \
	--set controller.templateLibraries.haproxyIngress.enabled=true \
	--set controller.templateLibraries.customCrdExample.enabled=true \
	--set controller.templateLibraries.spoaHub.enabled=true

chart-schema: ## Regenerate charts/haptic/values.schema.json from values.yaml and values.schema.extras.yaml
	@$(HELM_VALUES_SCHEMA_JSON) $(CHART_SCHEMA_FLAGS) --output charts/haptic/values.schema.json

chart-schema-check: ## Fail when charts/haptic/values.schema.json is out of date with its inputs
	@tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	$(HELM_VALUES_SCHEMA_JSON) $(CHART_SCHEMA_FLAGS) --output "$$tmp/values.schema.json" >/dev/null || exit 1; \
	if ! diff -u charts/haptic/values.schema.json "$$tmp/values.schema.json"; then \
		echo "charts/haptic/values.schema.json is stale. Run: make chart-schema"; exit 1; \
	fi

chart-size-check: ## Estimate the Helm release-Secret size against realistic install profiles; fail if one nears the 1 MiB Secret limit
	@# Estimates the base64(gzip(json(release))) payload Helm stores in its
	@# release Secret (reconstruction method + full profile table + threshold
	@# rationale: scripts/check-chart-release-size.py's header). The hard
	@# 1,048,576-byte Secret limit caused a silent e2e/install failure once the
	@# chart approached it (chart MR !1105); this gate catches a regression in
	@# `make`/CI BEFORE an install fails.
	@#
	@# BLOCKING: the default install and the realistic max (default +
	@# heaviest vendor lib) — the configs an operator can actually end up
	@# running. INFORMATIONAL: every bundled library at once — nobody runs
	@# that config, so it never fails the build, but stays visible so a big
	@# regression there doesn't go unnoticed.
	@python3 scripts/check-chart-release-size.py charts/haptic --label "default install"
	@python3 scripts/check-chart-release-size.py charts/haptic $(REALISTIC_MAX_LIBS) --label "realistic max: default + nginxIngress"
	@python3 scripts/check-chart-release-size.py charts/haptic $(WORST_CASE_LIBS) --informational --label "all libraries (unrealistic, high-water mark)"

vector-config-check: build ## Assert the chart renders a vector.yaml that vector can load
	@# Regex assertions and the span harness both look at fragments; neither
	@# notices the document as a whole stopped being YAML. A block scalar that
	@# ends early makes vector keep its bootstrap config, so its readiness probe
	@# never passes and the rollout wedges with every offline gate green.
	@# Needs the controller: the check renders chart templates through the engine
	@# and parses the result, which nothing outside the engine can produce.
	@bash scripts/check-vector-config.sh

cr-spec-conformance-check: ## Reject a rendered CR the apiserver would refuse: an undeclared spec field, or a declared one breaking its CRD's constraints
	@# kubeconform -skips the chart's own kinds (they are in no public schema
	@# catalog), so nothing checked the rendered spec against the CRD that
	@# governs it. `spec` is seeded from controller.config wholesale, so a new
	@# values key silently becomes an undeclared spec field — and server-side
	@# apply rejects the object, failing `helm upgrade` for every operator.
	@python3 scripts/check-cr-spec-conformance.py charts/haptic \
		--api-versions=gateway.networking.k8s.io/v1/GatewayClass
	@# Again with every library on: the disabled ones render four more objects,
	@# and a library the defaults never emit cannot be checked by the run above.
	@python3 scripts/check-cr-spec-conformance.py charts/haptic \
		--api-versions=gateway.networking.k8s.io/v1/GatewayClass $(WORST_CASE_LIBS)

cr-size-check: ## Check each rendered HAProxyTemplateConfig against etcd's ~1.5 MiB per-object limit
	@# The OTHER size ceiling, independent of the release Secret: etcd refuses a
	@# write above ~1.5 MiB, and the apiserver reports it as a bare
	@# "request is too large" on install. Before this gate existed the single
	@# merged config had reached 99.4% of it and the only signal was an
	@# unexplained e2e failure. Renders every bundled library at once — the
	@# profile that used to be impossible to install — so the check covers the
	@# largest object any operator can produce.
	@python3 scripts/check-cr-size.py charts/haptic $(WORST_CASE_LIBS)

## Security & vulnerability scanning

audit: vendor ## Run security vulnerability scanning
	@echo "Running govulncheck..."
	$(GOVULNCHECK) ./...
	npm --prefix ci/images/ci audit
	npm --prefix scripts/pages audit

## Combined checks

check-all: lint audit test ## Run all checks (linting, security, tests)
	@echo "✓ All checks passed!"

## Testing

.PHONY: test-ci-rules
test-ci-rules: ## Check CI selection, schedules, feature coverage, and merge verification
	python3 -m unittest scripts/tests/test_e2e_profiles.py scripts/tests/test_shard_go_tests.py

.PHONY: test-admission-readiness
.PHONY: check-no-lua
check-no-lua: ## Reject Lua scripts and runtime hooks in HAPTIC
	python3 -m unittest scripts/tests/test_check_no_lua.py
	python3 scripts/check-no-lua.py

test-admission-readiness: ## Verify admission readiness polling fails closed
	$(GO) tool gotestsum --format testname -- -race ./tests/admission/...

test: ## Run all unit and supporting test suites
	@$(MAKE) test-support
	@$(MAKE) test-unit

.PHONY: test-support
test-support: build-map-identity-check ## Run script, browser, process, playground and cluster-helper tests
	@echo "Running tests..."
	$(MAKE) test-admission-readiness
	$(MAKE) test-playground-web
	bash scripts/tests/test_check_test_inventory.sh
	python3 -m unittest \
		scripts/tests/test_check_no_lua.py \
		scripts/tests/test_shard_go_tests.py \
		scripts/tests/test_go_test_weights.py \
		scripts/tests/test_scale_trend.py \
		scripts/tests/test_prepare_gateway_api_canary.py \
		scripts/tests/test_validate_conformance_report.py \
		scripts/tests/test_conformance_provenance.py \
		scripts/tests/test_check_controller_output.py \
		scripts/tests/test_check_image_pins.py \
		scripts/tests/test_analyze_gateway_api_bench.py \
		scripts/tests/test_analyze_gateway_api_children.py \
		scripts/tests/test_analyze_gateway_api_outcomes.py \
		scripts/tests/test_analyze_gateway_api_resources.py
	$(MAKE) test-registry-build-retry
	$(MAKE) test-gateway-benchmark-harness
	bash scripts/tests/test_chart_haproxy_image.sh
	$(MAKE) test-release
	bash scripts/tests/test_chart_spoa_image.sh
	bash scripts/tests/test_start_dev_env_images.sh
	bash scripts/tests/test_chart_varnish_image.sh
	bash scripts/tests/test_chart_config_version_stable.sh
	bash scripts/tests/test_shard_conformance_tests.sh
	@$(MAKE) test-process
	@$(MAKE) test-playground
	@$(MAKE) test-e2e-helpers

.PHONY: lint-github-actions test-github-verification
lint-github-actions: ## Validate GitHub workflow syntax and expressions
	$(ACTIONLINT)

test-github-verification: lint-github-actions ## Check GitHub verification evidence fails closed
	python3 -m unittest discover -s .github/tests -v

.PHONY: check-chart-upgrade-baselines
check-chart-upgrade-baselines: ## Require every published baseline in the active CI upgrade configurations
	./scripts/check-chart-upgrade-baselines.sh

.PHONY: schedule-github-verification
schedule-github-verification: ## Balance GitHub jobs using the recorded runner durations (RECORD_RUNS="<run-id>..." re-measures them first)
	$(if $(RECORD_RUNS),GH_TOKEN="$${GH_TOKEN:-$$(gh auth token)}") python3 -m scripts.ci.github_schedule $(if $(RECORD_RUNS),--record $(RECORD_RUNS))

.PHONY: test-github-toolchain-update
test-github-toolchain-update: ## Verify Dockerfile changes reach the selected GitHub toolchain
	python3 -m scripts.ci.toolchain_update_check "$(TOOLCHAIN_KIND)"

.PHONY: test-github-native-tools
test-github-native-tools: ## Check installed ARM tools match the shared build pins
	python3 -m scripts.ci.github_native_tools verify "$(RUNNER_TEMP)/haptic-arm-tools/plan.json"

.PHONY: test-registry-build-retry
test-registry-build-retry: ## Verify registry retries preserve permanent build failures
	python3 -m unittest scripts/tests/test_retry_registry_build.py -v

.PHONY: test-gateway-benchmark-harness
test-gateway-benchmark-harness: build-map-identity-check ## Verify benchmark setup, workload gates and bundle provenance
	python3 -m unittest scripts/tests/test_record_benchmark_config_identities.py
	bash scripts/tests/test_bench_gateway_api.sh
	bash scripts/tests/test_spoa_bundle_provenance.sh

.PHONY: test-process
test-process: ## Verify process supervision with real child processes, without a cluster
	$(GO) tool gotestsum --junitfile report-process.xml --format testname -- -race -count=1 -tags=testinfra ./tests/process/... ./tests/e2e/availability/...

test-unit: go-mod-download ## Run Go unit tests (PKG and TEST_RUN_PATTERN select a subset)
	@# No coverage flags here: instrumenting the module for coverage costs a
	@# further 3.2x on top of -race (measured on the renderer chart-scale
	@# suite, 25.5s -> 82.1s). `make test-coverage` and the CI coverage job
	@# produce the profile; a correctness run does not need it.
	@# -timeout: pkg/controller/renderer's chart-scale suites run well past go
	@# test's 10m default under -race, which fails the package on time alone.
	$(GO) tool gotestsum --junitfile report.xml --format testname -- -race -timeout 45m $(if $(COUNT),-count=$(COUNT)) $(if $(TEST_RUN_PATTERN),-run "$(TEST_RUN_PATTERN)") $${PKG:-./...}

.PHONY: test-unit-inventory
test-unit-inventory: ## Enumerate the tests compiled for race-enabled unit verification
	$(GO) test -race -list . $${PKG:-./...}

.PHONY: test-e2e-helpers
test-e2e-helpers: ## Test e2e client and cluster helpers without creating a cluster
	$(GO) tool gotestsum --junitfile report-e2e-helpers.xml --format testname -- \
		-tags=e2e -race ./tests/e2e/e2ecluster/... ./tests/e2e/grpcclient/... ./tests/httpclient/... ./tests/tunnel/...

.PHONY: test-playground-web
test-playground-web: ## Test playground asset loading and generated scripts
	node --test cmd/playground/web/*.test.mjs

PLAYGROUND_TEST_BUNDLE ?= $(CURDIR)/build/playground-test

.PHONY: test-playground-tryout
test-playground-tryout: ## Download, validate, and run browser exports (requires npm, Docker or Podman; ports 80, 443, 8080, 8404)
	./scripts/build-playground.sh "$(PLAYGROUND_TEST_BUNDLE)" local
	$(MAKE) test-playground-tryout-bundle

.PHONY: test-playground-tryout-bundle
test-playground-tryout-bundle: ## Verify browser exports from an existing PLAYGROUND_TEST_BUNDLE
	npm ci --prefix scripts/visual-qa --no-audit --no-fund
	scripts/visual-qa/node_modules/.bin/playwright install --only-shell chromium
	node scripts/tests/test_playground_tryout.mjs "$(PLAYGROUND_TEST_BUNDLE)"

# The `playground` tag builds the client-native syntax + schema check that the
# browser playground answers `haproxy_valid` with. Nothing else may import it
# (depguard + check-client-native-free.sh), so its tests need their own run.
test-playground: ## Run the playground-tagged tests (client-native syntax + schema check)
	@echo "Running playground-tagged tests..."
	$(GO) tool gotestsum --junitfile report-playground.xml --format testname -- \
		-tags=playground -race ./pkg/dataplane/... ./pkg/generated/validators/...

test-integration: ## Run integration tests (requires kind cluster)
	@echo "Running integration tests..."
	@echo "Environment variables:"
	@echo "  KIND_NODE_IMAGE    - Kind node image (default: kindest/node:v1.32.0)"
	@echo "  KEEP_CLUSTER       - Keep cluster after tests (default: true)"
	@echo "  TEST_RUN_PATTERN   - Run specific tests matching pattern"
ifdef TEST_RUN_PATTERN
	@echo "Running tests matching pattern: $(TEST_RUN_PATTERN)"
	$(GO) tool gotestsum --junitfile report-integration.xml --format testname -- -tags=integration -v -race -timeout 15m -run "$(TEST_RUN_PATTERN)" ./tests/integration
else
	$(GO) tool gotestsum --junitfile report-integration.xml --format testname -- -tags=integration -v -race -timeout 15m ./tests/integration/...
endif

test-release: ## Verify release ordering and version-file staging
	python3 -m unittest scripts/tests/test_release.py scripts/tests/test_spoa_release_image.py scripts/tests/test_spoa_plugins.py

test-agent-docker: ## Run the agent docker suite (real HAProxy + agent containers, no cluster)
	@echo "Running agent docker tests against HAProxy $(HAPROXY_VERSION)..."
	@# The suite builds the haptic binary itself and lays it into the HAProxy
	@# image; HAPTIC_BINARY points it at a prebuilt one instead.
	HAPROXY_VERSION=$(HAPROXY_VERSION) $(GO) tool gotestsum --junitfile report-agent-docker.xml --format testname -- \
		-tags=agentdocker -v -timeout 20m -count=1 ./tests/agent/...

test-acceptance: $(if $(SKIP_DOCKER_BUILD),,docker-build-test) ## Run acceptance tests (builds image, creates kind cluster)
	@echo "Running acceptance tests..."
	@echo "Note: This will create a kind cluster and may take several minutes"
	@echo "Environment variables:"
	@echo "  KIND_NODE_IMAGE - Kind node image (default: kindest/node:v1.32.0)"
	@echo "  HAPTIC_ACCEPTANCE_CLUSTER_NAME - Kind cluster name (default: haproxy-test)"
	@echo "  HAPTIC_ACCEPTANCE_IMAGE - Controller image; set with SKIP_DOCKER_BUILD=1 (default: haptic:test)"
	$(GO) test -tags=acceptance -count=1 -v -timeout 15m $(if $(TEST_RUN_PATTERN),-run '$(TEST_RUN_PATTERN)',) ./tests/acceptance/...

test-acceptance-parallel: docker-build-test ## Run acceptance tests in parallel (faster, shared cluster)
	@echo "Running acceptance tests in parallel..."
	@echo "Note: Tests share a single Kind cluster with namespace isolation"
	@echo "Environment variables:"
	@echo "  KIND_NODE_IMAGE - Kind node image (default: kindest/node:v1.32.0)"
	@echo "  PARALLEL        - Max concurrent tests (default: 4)"
	$(GO) test -tags=acceptance -count=1 -v -timeout 30m -parallel $${PARALLEL:-4} -run TestAllAcceptanceParallel ./tests/acceptance/...

CONFORMANCE_IMAGE ?= haptic-conformance-test:latest
CONFORMANCE_KIND_NETWORK ?= kind
CONFORMANCE_KIND_CLUSTER ?= haptic-e2e
CONFORMANCE_TIMEOUT ?= 30m
# Cap on concurrently running conformance tests (forwarded as
# -test.parallel). Without it Go defaults to GOMAXPROCS — 16 on the CI
# runners — and the suite's parallel tests all start within milliseconds,
# creating ~6 Gateways (= per-Gateway LoadBalancer Services) in one burst.
# Every fresh VIP is unreachable ("connect: no route to host") for 1-3 s;
# under the burst the first test-created VIP ate >10s, exhausting the
# suite's MaxTimeToConsistency budget. Which layer (MetalLB announcement or
# kube-proxy rules) lags is unconfirmed; the conformance snapshotter's
# conformance-network-* ConfigMaps capture both. Capping
# concurrency spreads Gateway creation out; 4 matches the acceptance suite's
# default and costs ~1-2 min of shard wall-clock.
CONFORMANCE_PARALLEL ?= 4
# Container name used when CONFORMANCE_KEEP_CONTAINER is set (the report job
# keeps the stopped container so it can `docker cp` the written report out).
CONFORMANCE_CONTAINER ?= haptic-conformance-run

# Ingress conformance variables. The upstream
# kubernetes-sigs/ingress-controller-conformance project is dormant
# (last commit 2023-08-28, no releases, single maintainer); we pin to
# a specific SHA and never auto-follow master. Bumping the SHA is a
# deliberate, code-reviewed change.
INGRESS_CONFORMANCE_IMAGE ?= haptic-ingress-conformance-test:latest
INGRESS_CONFORMANCE_REPO ?= https://github.com/kubernetes-sigs/ingress-controller-conformance.git
INGRESS_CONFORMANCE_SHA ?= d920ed36a0076e169a9a329a850844ab3a695ae8

.PHONY: test-gateway-conformance-report github-conformance-report github-conformance-startup
test-gateway-conformance-report: ## Run the complete suite and verify its report and controller identity
	bash scripts/gateway-conformance-report.sh $(if $(CONFORMANCE_RELEASE_EVIDENCE),--release-evidence)

github-conformance-report: ## Prepare fresh GitHub conformance evidence for a candidate or published release
	python3 -m scripts.ci.github_conformance

github-conformance-startup: ## Diagnose fresh candidate startup without producing release evidence
	python3 -m scripts.ci.github_conformance --startup-only

test-gateway-conformance: ## Run upstream Gateway API conformance suite as a sibling container on the kind network
	@echo "Running Gateway API conformance suite against the $(CONFORMANCE_KIND_CLUSTER) cluster..."
	@echo "Note: this expects 'make test-e2e' to have provisioned the kind cluster"
	@echo "      and left it running (KEEP_CLUSTER=true is the default)."
	@echo "Environment variables:"
	@echo "  TEST_RUN_PATTERN - Run a subset of conformance tests matching the pattern"
	@echo "                     (forwarded as -test.run); empty = full suite."
	@echo "  CONFORMANCE_IMAGE - Image tag for the test binary (default: $(CONFORMANCE_IMAGE))"
	@echo "  CONFORMANCE_KIND_NETWORK - Docker network to attach the test container to"
	@echo "                             (default: $(CONFORMANCE_KIND_NETWORK) — kind's default)"
	@echo "  CONFORMANCE_KIND_CLUSTER - kind cluster name (default: $(CONFORMANCE_KIND_CLUSTER))"
	@echo "  CONFORMANCE_DEBUG - non-empty for upstream RoundTripper debug logging"
	@echo "  CONFORMANCE_PARALLEL - Max concurrently running tests (default: $(CONFORMANCE_PARALLEL));"
	@echo "                         see the variable's comment for why this is capped"
	@# Architecture: the conformance test binary is built statically here on
	@# the host (CGO_ENABLED=0), packaged into a tiny distroless image, and
	@# run as a sibling container on the kind docker network. From that
	@# vantage point Gateway.Status MetalLB IPs are directly routable, so
	@# the stock upstream RoundTripper handles every dial — no NodePort
	@# tunnel, no DinD remap, no CustomDialContext. Same code path locally
	@# and under GitLab's docker:dind, the only thing that changes is which
	@# docker daemon owns the network (host daemon vs DinD's nested daemon).
	@echo "Building conformance test binary (gateway_conformance tag)..."
	@# Static binary: distroless-static has no libc / dynamic loader.
	CGO_ENABLED=0 $(GO) test -mod=mod -tags=gateway_conformance -c -o /tmp/haptic-conformance.test ./tests/conformance/
	@echo "Resolving kind apiserver kubeconfig (--internal, for container-network DNS)..."
	@# The kubeconfig is BAKED INTO the image rather than bind-mounted because
	@# bind mounts don't cross the DinD boundary — `docker run -v src:dst`
	@# resolves `src` on the DinD daemon's filesystem, not the GitLab job
	@# container's, and the daemon doesn't have it. Baking works in both
	@# environments and per-cluster image churn is acceptable (we rebuild
	@# per `make test-e2e` anyway).
	@echo "Packaging into $(CONFORMANCE_IMAGE)..."
	@# Build with a minimal context so the daemon isn't asked to upload the
	@# whole repo (which is a few hundred MB and pointless — Dockerfile.
	@# conformance-test only COPYs the test binary + kubeconfig).
	@rm -rf /tmp/haptic-conformance-build
	@mkdir -p /tmp/haptic-conformance-build
	cp /tmp/haptic-conformance.test /tmp/haptic-conformance-build/
	cp Dockerfile.conformance-test /tmp/haptic-conformance-build/Dockerfile
	kind get kubeconfig --internal --name=$(CONFORMANCE_KIND_CLUSTER) > /tmp/haptic-conformance-build/kubeconfig
	docker build -t $(CONFORMANCE_IMAGE) /tmp/haptic-conformance-build
	@rm -rf /tmp/haptic-conformance-build
	@echo "Running conformance suite..."
	@# CONFORMANCE_KEEP_CONTAINER (set by the report job) keeps the stopped
	@# container under a fixed name so the report file the test wrote inside it
	@# can be `docker cp`'d out afterwards; otherwise --rm cleans up as before.
	@# CONFORMANCE_REPORT_OUTPUT / CONFORMANCE_IMPL_VERSION are forwarded into
	@# the test binary to enable + label the ConformanceReport.
	@# Pre-remove any stale kept-container from a prior run that was interrupted
	@# before its cleanup (job cancel / 60m timeout / runner death), so the
	@# `docker run --name` below is idempotent instead of failing with
	@# "name already in use". No-op when not keeping the container.
	$(if $(CONFORMANCE_KEEP_CONTAINER),docker rm -f $(CONFORMANCE_CONTAINER) >/dev/null 2>&1 || true,)
	docker run \
		$(if $(CONFORMANCE_KEEP_CONTAINER),--name $(CONFORMANCE_CONTAINER),--rm) \
		--network $(CONFORMANCE_KIND_NETWORK) \
		$(if $(CONFORMANCE_DEBUG),-e CONFORMANCE_DEBUG=$(CONFORMANCE_DEBUG)) \
		$(if $(CONFORMANCE_REPORT_OUTPUT),-e CONFORMANCE_REPORT_OUTPUT=$(CONFORMANCE_REPORT_OUTPUT)) \
		$(if $(CONFORMANCE_IMPL_VERSION),-e CONFORMANCE_IMPL_VERSION=$(CONFORMANCE_IMPL_VERSION)) \
		$(CONFORMANCE_IMAGE) \
		-test.v -test.timeout=$(CONFORMANCE_TIMEOUT) -test.parallel=$(CONFORMANCE_PARALLEL) \
		$(if $(TEST_RUN_PATTERN),-test.run "$(TEST_RUN_PATTERN)")

build-ingress-conformance-image: ## Build the ingress-conformance test image (clone upstream, apply patches, compile, docker build)
	@echo "Building ingress-conformance test image $(INGRESS_CONFORMANCE_IMAGE)..."
	@echo "Note: upstream kubernetes-sigs/ingress-controller-conformance is dormant"
	@echo "      (last commit 2023-08-28, no releases). Pinned to SHA:"
	@echo "      $(INGRESS_CONFORMANCE_SHA). We do NOT auto-follow master."
	@echo "Environment variables:"
	@echo "  INGRESS_CONFORMANCE_IMAGE - Image tag for the test image"
	@echo "                              (default: $(INGRESS_CONFORMANCE_IMAGE))"
	@echo "  INGRESS_CONFORMANCE_REPO  - Upstream git repo URL"
	@echo "                              (default: $(INGRESS_CONFORMANCE_REPO))"
	@echo "  INGRESS_CONFORMANCE_SHA   - Upstream commit SHA to build"
	@echo "                              (default: $(INGRESS_CONFORMANCE_SHA))"
	@echo "  CONFORMANCE_KIND_CLUSTER  - kind cluster name (default: $(CONFORMANCE_KIND_CLUSTER))"
	@echo "                              Only used when CONFORMANCE_BAKE_KUBECONFIG=1."
	@echo "  CONFORMANCE_BAKE_KUBECONFIG - When =1, bake the kind cluster's kubeconfig"
	@echo "                              into the image (legacy local-dev shape). Default"
	@echo "                              =0: image carries no kubeconfig and the runner"
	@echo "                              must bind-mount one at /etc/kubeconfig. CI sets"
	@echo "                              =0 because the cluster doesn't exist at"
	@echo "                              image-build time."
	@# Architecture: identical to test-gateway-conformance. Two binaries
	@# get baked into a distroless image: our Go test wrapper (built with
	@# the ingress_conformance tag, exec's the upstream and parses its
	@# Cucumber JSON into go-test subtests) and the upstream binary
	@# itself (built from a git clone at the pinned SHA, the same way
	@# upstream's own Makefile builds it). The container runs as a
	@# sibling on the kind docker network so it can reach the apiserver
	@# by its docker-DNS hostname.
	@#
	@# Split out from test-ingress-conformance so CI can build once per
	@# pipeline and share the image across the parallel:N shards instead
	@# of every shard re-cloning, re-patching, and re-compiling upstream
	@# (~150s wasted per extra shard). Local `make test-ingress-conformance`
	@# still triggers a build via the dependency edge below.
	@echo "Building wrapper binary (ingress_conformance tag)..."
	CGO_ENABLED=0 $(GO) test -mod=mod -tags=ingress_conformance -c -o /tmp/haptic-ingress-conformance.test ./tests/conformance/
	@echo "Cloning upstream at $(INGRESS_CONFORMANCE_SHA)..."
	@rm -rf /tmp/haptic-ingress-conformance-upstream
	git clone --quiet $(INGRESS_CONFORMANCE_REPO) /tmp/haptic-ingress-conformance-upstream
	git -C /tmp/haptic-ingress-conformance-upstream -c advice.detachedHead=false checkout --quiet $(INGRESS_CONFORMANCE_SHA)
	@echo "Applying vendored patches (see tests/conformance/patches/README)..."
	@# Upstream PR #101 (convergence retry) — vendored because upstream is
	@# dormant and never merged it. Cilium carries the same patch in its
	@# fork. Without this, single-shot HTTP requests lose to the inherent
	@# K8s-Endpoints→runtime-config-push race that every Ingress controller
	@# has a non-zero gap on.
	@for patch in tests/conformance/patches/*.patch; do \
	  echo "  applying $$patch"; \
	  git -C /tmp/haptic-ingress-conformance-upstream apply --verbose "$(CURDIR)/$$patch" || \
	    { echo "patch $$patch failed to apply — re-resolve against $(INGRESS_CONFORMANCE_SHA)"; exit 1; }; \
	done
	@echo "Building upstream ingress-controller-conformance binary..."
	@# The upstream's modules are never in haptic's cache, so every build
	@# downloads them and a proxy stream reset fails the job (pipeline
	@# 2830518002); the download is separated out so it can be retried.
	cd /tmp/haptic-ingress-conformance-upstream && for attempt in 1 2 3; do \
	  $(GO) mod download && break; \
	  [ "$$attempt" -lt 3 ] || { echo "module download failed after 3 attempts"; exit 1; }; \
	  echo "module download failed (attempt $$attempt), retrying"; sleep 5; \
	done
	@# Mirror upstream's own Makefile flags (-trimpath, ldflags). Running
	@# `go test -c` from inside the cloned tree uses the upstream's
	@# go.mod, not haptic's — that's exactly what we want for the pin
	@# to be honored end-to-end.
	cd /tmp/haptic-ingress-conformance-upstream && CGO_ENABLED=0 $(GO) test -c -trimpath -ldflags="-buildid= -w" -o /tmp/ingress-controller-conformance .
	@echo "Packaging into $(INGRESS_CONFORMANCE_IMAGE)..."
	@rm -rf /tmp/haptic-ingress-conformance-build
	@mkdir -p /tmp/haptic-ingress-conformance-build
	cp /tmp/haptic-ingress-conformance.test /tmp/haptic-ingress-conformance-build/
	cp /tmp/ingress-controller-conformance /tmp/haptic-ingress-conformance-build/
	cp -r /tmp/haptic-ingress-conformance-upstream/features /tmp/haptic-ingress-conformance-build/features
	cp Dockerfile.ingress-conformance-test /tmp/haptic-ingress-conformance-build/Dockerfile
	@# Kubeconfig handling. Local `make test-ingress-conformance` runs
	@# right after `make test-e2e`, so the kind cluster exists and we
	@# can bake its kubeconfig — same shape as
	@# Dockerfile.conformance-test. CI builds this image in a separate,
	@# earlier job (before the cluster exists), so it sets
	@# CONFORMANCE_BAKE_KUBECONFIG=0 and the test job mounts a
	@# kubeconfig into the container at run time. The Dockerfile's
	@# `COPY kubeconfig /etc/kubeconfig` would otherwise fail the build
	@# when the file isn't present; we materialise an empty stub so the
	@# COPY succeeds and let runtime overrides supersede it.
	@if [ "$(CONFORMANCE_BAKE_KUBECONFIG)" = "1" ]; then \
	  echo "Baking kubeconfig for kind cluster $(CONFORMANCE_KIND_CLUSTER)..."; \
	  kind get kubeconfig --internal --name=$(CONFORMANCE_KIND_CLUSTER) > /tmp/haptic-ingress-conformance-build/kubeconfig; \
	else \
	  echo "Skipping kubeconfig bake (CONFORMANCE_BAKE_KUBECONFIG!=1); runner must mount one."; \
	  : > /tmp/haptic-ingress-conformance-build/kubeconfig; \
	fi
	docker build -t $(INGRESS_CONFORMANCE_IMAGE) /tmp/haptic-ingress-conformance-build
	@rm -rf /tmp/haptic-ingress-conformance-build /tmp/haptic-ingress-conformance-upstream

test-ingress-conformance: ## Run upstream Kubernetes Ingress conformance suite as a sibling container on the kind network
	@echo "Running Ingress conformance suite against the $(CONFORMANCE_KIND_CLUSTER) cluster..."
	@echo "Note: this expects 'make test-e2e' to have provisioned the kind cluster"
	@echo "      and left it running (KEEP_CLUSTER=true is the default)."
	@echo "Environment variables:"
	@echo "  TEST_RUN_PATTERN - Run a subset of conformance scenarios matching the pattern"
	@echo "                     (forwarded as -test.run); empty = full suite."
	@echo "  SHARD_ID / SHARD_COUNT - Run only the assigned slice of the suite. Set by"
	@echo "                     GitLab CI's parallel:N keyword (CI_NODE_INDEX/CI_NODE_TOTAL)."
	@echo "                     Local dev: unset = full suite (the default)."
	@echo "  CONFORMANCE_IMAGE_PREBUILT - When =1, skip the build chain. The image must"
	@echo "                     already be tagged $(INGRESS_CONFORMANCE_IMAGE) locally"
	@echo "                     (CI pulls + retags from the registry before this target)."
	@echo "  CONFORMANCE_KIND_NETWORK  - Docker network for the test container"
	@echo "                              (default: $(CONFORMANCE_KIND_NETWORK))"
	@echo "  CONFORMANCE_KIND_CLUSTER  - kind cluster name"
	@echo "                              (default: $(CONFORMANCE_KIND_CLUSTER))"
	@if [ "$(CONFORMANCE_IMAGE_PREBUILT)" != "1" ]; then \
	  echo "Building $(INGRESS_CONFORMANCE_IMAGE) locally (set CONFORMANCE_IMAGE_PREBUILT=1 to skip)..."; \
	  $(MAKE) build-ingress-conformance-image CONFORMANCE_BAKE_KUBECONFIG=1; \
	else \
	  echo "Layering kind kubeconfig onto prebuilt $(INGRESS_CONFORMANCE_IMAGE)..."; \
	  : "# Bind-mounts don't cross the DinD boundary (the host path"; \
	  : "# resolves on the GitLab job's filesystem, not the DinD daemon's"; \
	  : "# — see Dockerfile.ingress-conformance-test header). The CI"; \
	  : "# build job builds with an empty kubeconfig stub because the"; \
	  : "# kind cluster doesn't exist yet at that point; here we layer"; \
	  : "# the real kubeconfig on top with a single COPY (cached base"; \
	  : "# image = sub-second rebuild)."; \
	  rm -rf /tmp/haptic-ingress-conformance-rebake; \
	  mkdir -p /tmp/haptic-ingress-conformance-rebake; \
	  kind get kubeconfig --internal --name=$(CONFORMANCE_KIND_CLUSTER) > /tmp/haptic-ingress-conformance-rebake/kubeconfig; \
	  printf 'FROM %s\nCOPY kubeconfig /etc/kubeconfig\n' "$(INGRESS_CONFORMANCE_IMAGE)" > /tmp/haptic-ingress-conformance-rebake/Dockerfile; \
	  docker build -t $(INGRESS_CONFORMANCE_IMAGE) /tmp/haptic-ingress-conformance-rebake; \
	  rm -rf /tmp/haptic-ingress-conformance-rebake; \
	fi
	@echo "Running conformance suite..."
	docker run \
		--rm \
		--network $(CONFORMANCE_KIND_NETWORK) \
		$(if $(SHARD_ID),-e SHARD_ID=$(SHARD_ID)) \
		$(if $(SHARD_COUNT),-e SHARD_COUNT=$(SHARD_COUNT)) \
		$(INGRESS_CONFORMANCE_IMAGE) \
		-test.v -test.timeout=$(CONFORMANCE_TIMEOUT) \
		$(if $(TEST_RUN_PATTERN),-test.run "$(TEST_RUN_PATTERN)")

VARNISH_OUTPUT ?= --load

.PHONY: docker-build-varnish
docker-build-varnish: ## Build the bundled non-root Varnish image
	@set -eu; image="$(VARNISH_IMAGE)"; \
	if [ -z "$$image" ]; then \
		if [ -n "$$(yq -r '.cache.varnish.image' charts/haptic/values.yaml)" ]; then \
			echo "Using the configured Varnish image; no bundled image build required"; exit 0; \
		fi; \
		image="$$(bash scripts/chart-varnish-image.sh)"; \
	fi; \
	docker buildx build $(if $(VARNISH_PLATFORMS),--platform "$(VARNISH_PLATFORMS)") \
		--label "org.opencontainers.image.source=https://github.com/haproxy-haptic/HAPTIC" \
		--label "org.opencontainers.image.revision=$$(git rev-parse HEAD)" \
		--provenance=false --tag "$$image" $(VARNISH_OUTPUT) -f Dockerfile.varnish .

.PHONY: test-e2e-profile
test-e2e-profile: ## Run the feature and interaction tests for HAPTIC_E2E_PROFILE
	@set -eu; \
	case "$${HAPTIC_E2E_PROFILE:-}" in \
		cache|rate-limit) ;; \
		api-gateway) test "$${HAPTIC_E2E_GWAPI_CHANNEL:-}" = experimental || { echo "api-gateway requires HAPTIC_E2E_GWAPI_CHANNEL=experimental" >&2; exit 1; } ;; \
		*) echo "Set HAPTIC_E2E_PROFILE to cache, rate-limit, or api-gateway" >&2; exit 1 ;; \
	esac; \
	mkdir -p debug-logs; \
	$(GO) test -mod=readonly -tags=e2e -list '^Test' ./tests/e2e/... > debug-logs/e2e-test-inventory.txt; \
	TEST_RUN_PATTERN="$$(python3 scripts/shard-go-tests.py --include "tests/e2e/profiles/$${HAPTIC_E2E_PROFILE}.txt" 1 1 < debug-logs/e2e-test-inventory.txt)"; \
	printf '%s\n' "$$TEST_RUN_PATTERN" > debug-logs/e2e-test-selection.txt; \
	TEST_RUN_PATTERN="$$TEST_RUN_PATTERN" $(MAKE) test-e2e

E2E_IMAGE_TAG ?= test

test-e2e: check-source-hash $(if $(SKIP_DOCKER_BUILD),,docker-build-test docker-build-varnish) ## Run full-stack e2e tests (self-contained — kind + helm install + fixtures)
	@echo "Running e2e tests..."
	@# The chart composes its image tag as "<image.tag>-haproxy<haproxyVersion>".
	@# docker-build-test produces "haptic:test" (with HAProxy $(HAPROXY_VERSION)
	@# bundled, sourced from versions.env). Tag with the suffix the chart looks
	@# for so the chart's auto-matching contract holds end-to-end.
	@# CI sets SKIP_DOCKER_BUILD=1 and pre-tags from the registry-pulled image,
	@# so this re-tag is a no-op there.
	docker tag haptic:$(E2E_IMAGE_TAG) haptic:$(E2E_IMAGE_TAG)-haproxy$(HAPROXY_VERSION)
	@cluster_name="$${HAPTIC_E2E_CLUSTER_NAME:-haptic-e2e}"; \
		echo "Note: This creates kind cluster '$$cluster_name', helm-installs the chart, deploys fixtures."
	@echo "Environment variables:"
	@echo "  HAPTIC_E2E_CLUSTER_NAME - Kind cluster name (default: haptic-e2e)"
	@echo "  KIND_NODE_IMAGE     - Kind node image (default: bundled Kind image)"
	@echo "  KEEP_CLUSTER        - Keep cluster after tests (default: true; set false to destroy)"
	@echo "  KEEP_NAMESPACE      - Keep test namespaces after failure for debugging (default: false)"
	@echo "  SKIP_CLUSTER_CREATE - CI mode: assume cluster already exists; skip kind create"
	@echo "  SKIP_DOCKER_BUILD   - CI mode: assume haptic:test-haproxyX.Y already loaded"
	@echo "  E2E_IMAGE_TAG       - Install haptic:<tag>-haproxyX.Y, tagged from haptic:<tag> (default: test)"
	@echo "  TEST_RUN_PATTERN    - Run specific tests matching pattern"
	@echo "  HAPTIC_E2E_EXTRA_VALUES - Additional Helm values file, after the selected test profile"
	@echo "  HAPTIC_E2E_EXTRA_SET - Extra helm --set pairs, ';'-separated. Match CI's shape"
	@echo "                        locally with controller.resources.limits.cpu=4: GOMAXPROCS"
	@echo "                        sizes the validationTests gate, so a big box hides gaps"
	@echo "  HAPTIC_E2E_AGENT_DEBUG - Set 1 to install the chart with haproxy.agent.logLevel=debug"
	@echo "  PARALLEL            - Max concurrent tests. Defaults to Go's default (GOMAXPROCS,"
	@echo "                        i.e. nproc), which auto-scales to the host. Verified stable"
	@echo "                        from 4 up through 16 on a 16-core box. Override with"
	@echo "                        PARALLEL=N for constrained environments"
	@controller_rollout_id="$$(docker image inspect --format '{{.Id}}' haptic:$(E2E_IMAGE_TAG)-haproxy$(HAPROXY_VERSION))"; \
	controller_binary_sha256="$$(docker run --rm --entrypoint sha256sum haptic:$(E2E_IMAGE_TAG)-haproxy$(HAPROXY_VERSION) /usr/local/bin/haptic | awk '{print $$1}')"; \
	controller_source_hash="$$(docker run --rm --entrypoint /usr/local/bin/haptic haptic:$(E2E_IMAGE_TAG)-haproxy$(HAPROXY_VERSION) version | awk '$$1 == "Source" && $$2 == "Hash:" {print $$3}')"; \
	if ! printf '%s\n' "$$controller_rollout_id" | grep -Eq '^sha256:[0-9a-f]{64}$$'; then echo "Controller image haptic:$(E2E_IMAGE_TAG)-haproxy$(HAPROXY_VERSION) has invalid ID '$$controller_rollout_id'; rebuild it with make docker-build-test."; exit 1; fi; \
	if ! printf '%s\n' "$$controller_binary_sha256" | grep -Eq '^[0-9a-f]{64}$$'; then echo "Controller binary digest '$$controller_binary_sha256' is invalid; rebuild haptic:$(E2E_IMAGE_TAG)-haproxy$(HAPROXY_VERSION)."; exit 1; fi; \
	if [ "$$controller_source_hash" != "$(SOURCE_HASH)" ]; then echo "Controller image source hash '$$controller_source_hash' does not match local source hash '$(SOURCE_HASH)'; rebuild it with make docker-build-test."; exit 1; fi; \
	HAPTIC_EXPECTED_CONTROLLER_ROLLOUT_ID="$$controller_rollout_id" HAPTIC_EXPECTED_CONTROLLER_BINARY_SHA256="$$controller_binary_sha256" HAPTIC_EXPECTED_SOURCE_HASH=$(SOURCE_HASH) HAPTIC_HAPROXY_VERSION=$(HAPROXY_VERSION) HAPTIC_E2E_IMAGE_TAG=$(E2E_IMAGE_TAG) $(GO) test -count=$(or $(COUNT),1) -mod=readonly -tags=e2e -v -timeout 30m $(if $(PARALLEL),-parallel $(PARALLEL)) $(if $(TEST_RUN_PATTERN),-run "$(TEST_RUN_PATTERN)") ./tests/e2e/...

test-helm-defaults: $(if $(HELM_DEFAULTS_IMAGE),,docker-build-test) ## Install the default chart in kind and run its deployment smoke tests
	@# Local runs must test this worktree's controller against this worktree's
	@# chart. CI supplies HELM_DEFAULTS_IMAGE explicitly and skips the build.
	@if [ -z "$(HELM_DEFAULTS_IMAGE)" ]; then \
		docker tag haptic:test haptic:test-haproxy$(HAPROXY_VERSION); \
	fi
	bash scripts/test-helm-defaults.sh --image "$(or $(HELM_DEFAULTS_IMAGE),haptic:test-haproxy$(HAPROXY_VERSION))"

check-controller-output: ## Capture controller logs and reject inconsistent rendered output
	bash scripts/check-controller-output.sh

test-vector-spans: ## Execute the span-building VRL under vector and assert the span geometry
	@# No cluster and no build: it renders the chart, extracts the transform and
	@# runs it against fixtures. Catches derived-arithmetic errors that emit
	@# plausible-looking spans, which nothing offline can see.
	bash scripts/test-vector-spans.sh

test-vector-metrics: build ## Verify rendered Vector metrics and log preservation with real records
	bash scripts/test-vector-metrics.sh

test-install-without-gateway-api: $(if $(SKIP_DOCKER_BUILD),,docker-build-test) ## Assert a default install converges on a cluster with no Gateway API CRDs
	@# Every other suite installs Gateway API first. This one must not, which is
	@# why it owns its own cluster rather than joining the e2e one.
	bash scripts/test-install-without-gateway-api.sh $(if $(KEEP_CLUSTER),--keep,)

test-release-namespace-secrets: $(if $(SKIP_DOCKER_BUILD),,docker-build-test) ## Install with controller.rbac.releaseNamespaceSecretsOnly and verify RBAC, TLS, and rejections
	@# Owns its own cluster: the chart's RBAC is the subject under test.
	bash scripts/test-release-namespace-secrets.sh $(if $(KEEP_CLUSTER),--keep,)

test-chart-upgrade: $(if $(SKIP_DOCKER_BUILD),,docker-build-test) ## Verify released-chart upgrades, traffic, rejection, and recovery
	@# Owns its own kind cluster: it installs a released chart whose pre-upgrade
	@# hook applies that release's cluster-scoped CRDs, which would downgrade the
	@# schemas under any suite sharing the cluster.
	bash scripts/test-chart-upgrade.sh $(if $(KEEP_CLUSTER),--keep,)

GITOPS_PROVIDER ?= argo
GITOPS_CERTIFICATES ?= external
test-gitops-lifecycle: $(if $(GITOPS_IMAGE),,docker-build-test) ## Verify Argo CD or Flux installs, syncs, upgrades, rejection, and recovery
	gitops_run_id="$${CI_JOB_ID:-$$(date +%s%N)}"; \
	bash scripts/test-gitops-lifecycle.sh --provider "$(GITOPS_PROVIDER)" \
		--certificates "$(GITOPS_CERTIFICATES)" \
		--image "$(or $(GITOPS_IMAGE),haptic:test)" \
		--cluster "$(or $(GITOPS_CLUSTER_NAME),haptic-gitops-$(GITOPS_PROVIDER)-$$gitops_run_id)" \
		--artifacts "$(or $(GITOPS_ARTIFACT_DIR),$(CURDIR)/debug-logs/gitops/$(GITOPS_PROVIDER)-$(GITOPS_CERTIFICATES)/$$gitops_run_id)" \
		$(if $(KEEP_CLUSTER),--keep,)

build-integration-test: ## Build integration test binary (without running)
	@echo "Building integration test binary..."
	@mkdir -p bin
	$(GO) test -c -o bin/integration.test ./tests/integration/...

.PHONY: test-coverage-unit
test-coverage-unit: ## Collect unit coverage independently of the race suite (PKG selects packages)
	$(GO) tool gotestsum --format testname -- \
		-count=1 -timeout 45m -coverprofile=coverage.out -covermode=atomic \
		-coverpkg=$(COVERAGE_PACKAGES) $${PKG:-./...}
	$(GO) tool cover -func=coverage.out > coverage.txt
	tail -1 coverage.txt

test-coverage: ## Run unit tests with coverage report
	@echo "Running unit tests with coverage..."
	$(GO) test -race -coverprofile=coverage.out -covermode=atomic -coverpkg=$(COVERAGE_PACKAGES) ./pkg/...
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report generated at coverage.html"

test-integration-coverage: ## Run integration tests with coverage (requires kind cluster)
	@echo "Running integration tests with coverage..."
	@mkdir -p coverage
	$(GO) test -tags=integration -race -timeout 15m -coverprofile=coverage/integration.out -covermode=atomic -coverpkg=$(COVERAGE_PACKAGES) ./tests/integration/...
	@echo "Integration coverage report generated at coverage/integration.out"

test-coverage-combined: ## Run unit and integration tests with combined coverage
	@echo "Running combined coverage (unit + integration tests)..."
	@mkdir -p coverage
	$(GO) test -race -coverprofile=coverage/unit.out -covermode=atomic -coverpkg=$(COVERAGE_PACKAGES) ./pkg/...
	$(GO) test -tags=integration -race -timeout 15m -coverprofile=coverage/integration.out -covermode=atomic -coverpkg=$(COVERAGE_PACKAGES) ./tests/integration/...
	@echo "Merging coverage profiles..."
	$(GO) run github.com/wadey/gocovmerge@latest coverage/unit.out coverage/integration.out > coverage/combined.out
	$(GO) tool cover -func=coverage/combined.out
	$(GO) tool cover -html=coverage/combined.out -o coverage/combined.html
	@echo "Combined coverage report generated at coverage/combined.html"

bench: ## Run benchmarks (usage: make bench PKG=./pkg/templating/ BENCH=BenchmarkVMPool COUNT=6; BENCHFLAGS='-cpuprofile=/tmp/cpu.prof' to profile)
	@echo "Running benchmarks..."
	$(GO) test \
		-run='^$$' \
		-bench=$${BENCH:-'.'} \
		-benchmem \
		-count=$${COUNT:-1} \
		-timeout=$${TIMEOUT:-5m} \
		$${BENCHFLAGS} \
		$${PKG:-./...}

.PHONY: bench-render-comparison
bench-render-comparison: ## Compare clean HEAD with BENCH_BASE_REF on the same host
	bash scripts/bench-render-comparison.sh

bench-gateway-api: build-map-identity-check ## Run pinned Gateway API benchmark programs against HAPTIC
	bash scripts/bench-gateway-api.sh

.PHONY: bench-gateway-api-ci
bench-gateway-api-ci: build-map-identity-check ## Run the hosted benchmark profile and validate its artifacts
	bash scripts/bench-gateway-api-ci.sh

.PHONY: build-map-identity-check
build-map-identity-check: ## Build the benchmark's published-map identity verifier
	$(GO) build -o bin/check-map-identities ./cmd/check-map-identities

## Schema extraction (for offline validate)

# SCHEMA_DIR is where extract-schemas writes its output. The default
# matches the path the chart's tests reference; operators can override
# to populate any local directory they later pass to
# `haptic validate --schema-dir=<path>`.
SCHEMA_DIR ?= tests/schemas

extract-schemas: ## Extract CustomResourceDefinitions into $(SCHEMA_DIR) for offline `validate --schema-dir`
	@echo "Extracting schemas to $(SCHEMA_DIR)/..."
	@mkdir -p $(SCHEMA_DIR)
	@# Chart-bundled CRDs first — the haptic project's own CRDs
	@# (haproxytemplateconfigs, haproxycfgs, the auxiliary file
	@# CRDs) are always relevant for chart authors who validate
	@# templates that reference them.
	@if [ -d charts/haptic/crds ]; then \
		find charts/haptic/crds -name '*.yaml' -exec cp -v {} $(SCHEMA_DIR)/ \; ; \
	fi
	@# Upstream Gateway API CRDs from the module cache. Picks up
	@# whichever release go.mod is currently on, so the extracted
	@# schemas track the build's actual dependency version. The
	@# Dir field on `go list -m -json` is unpopulated for modules
	@# that aren't vendored, so we reconstruct the path from
	@# GOMODCACHE + module@version explicitly.
	@# Upstream Gateway API resources include a ValidatingAdmissionPolicy
	@# alongside the CRDs (gateway.networking.k8s.io_vap_safeupgrades.yaml).
	@# Skip non-CRDs at extraction time so the output directory is
	@# CRD-only and clean for downstream tools. DirFetcher would also
	@# silently skip it at load time, but operators get a tidier
	@# directory listing this way.
	@# `-mod=mod` reads the version from go.mod directly, bypassing any
	@# vendor/modules.txt drift. extract-schemas pulls schemas from
	@# GOMODCACHE (not vendor/), so vendor-side inconsistencies are
	@# irrelevant to the extraction itself — but the default `go list -m`
	@# mode validates the active modules graph and exits non-zero on a
	@# checkout where the last `go mod vendor` ran before the most recent
	@# `go.mod` edit (transient between a Renovate dep bump and the next
	@# vendor sync). Without -mod=mod, version detection silently fails
	@# and the target emits the haptic CRDs only, surfacing as a
	@# confusing "where are the Gateway API schemas?" moment downstream.
	@gw_api_version="$$($(GO) list -mod=mod -m -f '{{.Version}}' sigs.k8s.io/gateway-api 2>/dev/null || true)"; \
	gw_api_dir="$$($(GO) env GOMODCACHE)/sigs.k8s.io/gateway-api@$${gw_api_version}"; \
	if [ -n "$$gw_api_version" ] && [ -d "$$gw_api_dir/config/crd/standard" ]; then \
		for f in $$gw_api_dir/config/crd/standard/gateway.networking.k8s.io_*.yaml; do \
			kind="$$(head -3 "$$f" | grep '^kind:' | awk '{print $$2}')"; \
			if [ "$$kind" = "CustomResourceDefinition" ]; then \
				cp -v "$$f" $(SCHEMA_DIR)/ ; \
			fi \
		done \
	fi
	@echo
	@echo "Schemas written to $(SCHEMA_DIR)/. Usage:"
	@echo "  haptic validate -f config.yaml --schema-dir=$(SCHEMA_DIR)"
	@echo "  HAPTIC_SCHEMA_DIR=$(SCHEMA_DIR) haptic validate -f config.yaml"

validate-helm-libraries: build ## Render the chart and run `haptic validate` against the merged HAProxyTemplateConfig (thin wrapper around scripts/test-templates.sh)
	@# Smoke-tests that the chart's bundled libraries merge cleanly and
	@# that the resulting config passes the controller's offline
	@# validate path — engine compile + chart validationTests. Used by
	@# CI (the helm-3.x jobs) and by chart authors who want to reproduce the
	@# CI check locally.
	@#
	@# Delegates to scripts/test-templates.sh so the render-and-validate
	@# flow has one source of truth — every flag/path/library decision
	@# lives in the script. HAPROXY_VERSION is forwarded via env so CI's
	@# per-version matrix renders with the matching haproxyVersion
	@# value.
	@HAPROXY_VERSION=$(HAPROXY_VERSION) bash scripts/test-templates.sh
	@$(MAKE) test-agent-skill

.PHONY: test-agent-skill test-agent-skill-package
test-agent-skill: build ## Validate the portable agent skill's examples with native HAProxy
	bin/haptic validate --file skills/haptic/assets/maintenance-config.yaml --schema-dir skills/haptic/assets/schemas
	@set -eu; candidate=$$(mktemp); trap 'rm -f "$$candidate"' EXIT; \
		helm template haptic charts/haptic --namespace haptic \
			--api-versions gateway.networking.k8s.io/v1/GatewayClass \
			--set haproxyVersion=$(HAPROXY_VERSION) \
			--values skills/haptic/assets/header-values.yaml > "$$candidate"; \
		bin/haptic validate --file "$$candidate" --schema-dir tests/schemas

test-agent-skill-package: ## Check skill downloads (requires mkdocs on PATH)
	python3 -m unittest discover -s docs/site/tests -v

## Build targets

.PHONY: build-ci-image
build-ci-image: ## Build the pinned CI image locally for HAPROXY_VERSION
	@HAPROXY_VERSION=$(HAPROXY_VERSION) bash scripts/build-ci-image.sh

.PHONY: build-docs-image
build-docs-image: ## Build the documentation toolchain from current pins
	@bash scripts/build-docs-image.sh

build: check-source-hash ## Build the controller binary for local development (with PGO if profile exists)
	@echo "Building controller..."
	@echo "  Version: $(VERSION)"
	@echo "  Git commit: $(GIT_COMMIT)"
	@if [ -f cmd/haptic/default.pgo ]; then echo "  PGO: enabled (using cmd/haptic/default.pgo)"; else echo "  PGO: disabled (no profile found)"; fi
	@mkdir -p bin
	$(GO) build \
		-mod=readonly \
		-pgo=auto \
		-ldflags="-X main.version=$(VERSION) -X main.commit=$(GIT_COMMIT) -X main.date=$(shell date -u +%Y-%m-%dT%H:%M:%SZ) -X main.sourceHash=$(SOURCE_HASH)" \
		-o bin/haptic \
		./cmd/haptic

build-for-docker: check-source-hash go-mod-download ## Build binary in platform-structured path for Docker builds with --build-context
	@echo "Building controller for Docker..."
	@echo "  Version: $(VERSION)"
	@echo "  Platform: linux/$(DOCKER_BINARY_ARCH)"
	@mkdir -p dist/linux/$(DOCKER_BINARY_ARCH)
	CGO_ENABLED=0 GOOS=linux GOARCH=$(DOCKER_BINARY_ARCH) $(GO) build \
		-mod=readonly \
		-pgo=auto \
		-trimpath \
		-buildvcs=false \
		-ldflags="-s -w -X main.version=$(VERSION) -X main.commit=$(GIT_COMMIT) -X main.sourceHash=$(SOURCE_HASH)" \
		-o dist/linux/$(DOCKER_BINARY_ARCH)/haptic \
		./cmd/haptic
	@echo "✓ Binary built: dist/linux/$(DOCKER_BINARY_ARCH)/haptic"
	@echo "  Use with: docker buildx build --platform linux/$(DOCKER_BINARY_ARCH) --build-context binary=dist --target runtime ..."

## Docker targets

check-source-hash:
	@SOURCE_HASH="$(SOURCE_HASH)" ./scripts/verify-source-hash.sh

docker-build: check-source-hash ## Build Docker image
	@echo "Building Docker image: $(FULL_IMAGE)"
	@echo "  Git commit:      $(GIT_COMMIT)"
	@echo "  Git tag:         $(GIT_TAG)"
	@echo "  Source hash:     $(SOURCE_HASH)"
	@echo "  HAProxy series:  $(HAPROXY_VERSION) (chart-selected patch)"
	DOCKER_BUILDKIT=1 bash scripts/retry-registry-build.sh docker build $(DOCKER_BUILD_FLAGS) \
		$(if $(DOCKER_BINARY_CONTEXT),--build-context "binary=$(DOCKER_BINARY_CONTEXT)" --target runtime) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg GIT_TAG=$(GIT_TAG) \
		--build-arg SOURCE_HASH=$(SOURCE_HASH) \
		--build-arg HAPROXY_IMAGE="$$(bash scripts/chart-haproxy-image.sh "$(HAPROXY_VERSION)")" \
		-t $(FULL_IMAGE) \
		.
	@echo "✓ Image built: $(FULL_IMAGE)"

docker-build-test: ## Build Docker image with test tag for acceptance tests
	IMAGE_TAG=test $(MAKE) docker-build

.PHONY: docker-build-test-native
docker-build-test-native: ## Build the test image with the host Go compiler and cache
	$(MAKE) build-for-docker VERSION="$(GIT_TAG)" DOCKER_BINARY_ARCH="$(shell $(GO) env GOARCH)"
	IMAGE_TAG=test DOCKER_BINARY_CONTEXT="$(CURDIR)/dist" $(MAKE) docker-build

docker-build-multiarch: check-source-hash ## Build multi-platform Docker image for local testing (linux/amd64 only)
	@echo "Building multi-platform Docker image: $(FULL_IMAGE)"
	@echo "  Platform: linux/amd64 (single platform for local load)"
	@echo "  Git commit: $(GIT_COMMIT)"
	@echo "  Git tag: $(GIT_TAG)"
	@echo "  Source hash: $(SOURCE_HASH)"
	DOCKER_BUILDKIT=1 docker buildx build \
		--platform linux/amd64 \
		--build-arg HAPROXY_IMAGE="$$(bash scripts/chart-haproxy-image.sh "$(HAPROXY_VERSION)")" \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg GIT_TAG=$(GIT_TAG) \
		--build-arg SOURCE_HASH=$(SOURCE_HASH) \
		--load \
		-t $(FULL_IMAGE) \
		.
	@echo "✓ Multi-platform image built and loaded: $(FULL_IMAGE)"

docker-build-multiarch-push: check-source-hash ## Build and push multi-platform Docker image (linux/amd64,linux/arm64)
	@if [ -z "$(REGISTRY)" ]; then \
		echo "Error: REGISTRY variable must be set for multi-arch push"; \
		echo "Example: make docker-build-multiarch-push REGISTRY=ghcr.io/myorg"; \
		exit 1; \
	fi
	@echo "Building and pushing multi-platform Docker image: $(FULL_IMAGE)"
	@echo "  Platforms: linux/amd64,linux/arm64"
	@echo "  Git commit: $(GIT_COMMIT)"
	@echo "  Git tag: $(GIT_TAG)"
	@echo "  Source hash: $(SOURCE_HASH)"
	DOCKER_BUILDKIT=1 docker buildx build \
		--platform linux/amd64,linux/arm64 \
		--build-arg HAPROXY_IMAGE="$$(bash scripts/chart-haproxy-image.sh "$(HAPROXY_VERSION)")" \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg GIT_TAG=$(GIT_TAG) \
		--build-arg SOURCE_HASH=$(SOURCE_HASH) \
		--push \
		-t $(FULL_IMAGE) \
		.
	@echo "✓ Multi-platform image pushed: $(FULL_IMAGE)"

docker-load-kind: docker-build ## Build Docker image and load into kind cluster
	@echo "Loading image into kind cluster: $(KIND_CLUSTER)"
	@if ! kind get clusters 2>/dev/null | grep -q "^$(KIND_CLUSTER)$$"; then \
		echo "Error: Kind cluster '$(KIND_CLUSTER)' not found"; \
		echo "Available clusters:"; \
		kind get clusters 2>/dev/null || echo "  (none)"; \
		exit 1; \
	fi
	kind load docker-image $(FULL_IMAGE) --name $(KIND_CLUSTER)
	@echo "✓ Image loaded into kind cluster: $(KIND_CLUSTER)"

docker-push: docker-build ## Build and push Docker image to registry
	@if [ -z "$(REGISTRY)" ]; then \
		echo "Error: REGISTRY variable must be set"; \
		echo "Example: make docker-push REGISTRY=ghcr.io/myorg"; \
		exit 1; \
	fi
	@echo "Pushing Docker image: $(FULL_IMAGE)"
	docker push $(FULL_IMAGE)
	@echo "✓ Image pushed: $(FULL_IMAGE)"

docker-clean: ## Remove Docker images and build cache
	@echo "Removing Docker images..."
	-docker rmi $(IMAGE_NAME):$(IMAGE_TAG) 2>/dev/null || true
	@if [ -n "$(REGISTRY)" ]; then \
		docker rmi $(REGISTRY)/$(IMAGE_NAME):$(IMAGE_TAG) 2>/dev/null || true; \
	fi
	@echo "Pruning build cache..."
	-docker builder prune -f
	@echo "✓ Docker cleanup complete"

## SPOA hub image targets

spoa-prep: ## Download and verify plugin .so files into plugins/<arch>/ (uses cosign)
	@bash scripts/prep-spoa-plugins.sh

spoa-bundle-render: ## Render docs/.../spoa-hub.md bundled-versions table from versions-spoa.env
	@bash scripts/render-spoa-bundle.sh

spoa-bundle-check: ## Verify docs/.../spoa-hub.md is in sync with versions-spoa.env (CI guard)
	@bash scripts/render-spoa-bundle.sh --check

SPOA_DEV_IMAGE ?= spoa-hub:dev
SPOA_DEV_PLATFORM ?= linux/amd64

spoa-hub-image: spoa-prep ## Build the SPOA hub locally (SPOA_DEV_IMAGE selects the tag, SPOA_DEV_PLATFORM the platform)
	@set -a; . ./versions-spoa.env; set +a; \
	HUB_TAG="$${SPOA_HUB_VERSION#v}"; \
	echo "Building $(SPOA_DEV_IMAGE) ($(SPOA_DEV_PLATFORM), FROM hub $$HUB_TAG)"; \
	docker buildx build $(DOCKER_BUILD_FLAGS) \
		--platform "$(SPOA_DEV_PLATFORM)" \
		--build-arg "SPOA_HUB_VERSION=$$HUB_TAG" \
		--build-context plugins=plugins \
		--load \
		-f Dockerfile.spoa-hub \
		-t "$(SPOA_DEV_IMAGE)" \
		.
	@echo "Built $(SPOA_DEV_IMAGE)"

test-spoa-reload: ## Verify removing Coraza in the chart-selected SPOA image
	@chart_image="$$(bash scripts/chart-spoa-image.sh)" || exit; \
		if [ -n "$(SPOA_RELOAD_IMAGE)" ] && [ "$(SPOA_RELOAD_IMAGE)" != "$$chart_image" ]; then \
			echo "SPOA_RELOAD_IMAGE differs from chart image $$chart_image; unset SPOA_RELOAD_IMAGE" >&2; \
			exit 1; \
		fi; \
		bash scripts/test-spoa-reload.sh "$$chart_image"

## Dependency management

tidy: ## Run go mod tidy
	@echo "Running go mod tidy..."
	$(GO) mod tidy

vendor: go-mod-download ## Sync vendor directory with go.mod (auto-runs before lint/audit)
	@$(GO) mod vendor

.PHONY: go-mod-download
go-mod-download: ## Download every module in go.mod, retrying a reset proxy connection
	@scripts/retry.sh $(GO) mod download

verify: ## Verify dependencies
	@echo "Verifying dependencies..."
	$(GO) mod verify

verify-generate: ## Verify generated code (CRDs, DeepCopy) is up-to-date
	@echo "Verifying generated code is up-to-date..."
	@$(MAKE) generate-crds generate-deepcopy generate-chart-defaults
	@# tests/schemas is the offline --schema-dir. Its HAPTIC CRD copies must
	@# track charts/haptic/crds, or an author can validate a config against a
	@# field the real CRD dropped (e.g. a deleted validationTest key) and never
	@# see it pruned live. Refresh those copies deterministically (a plain cp,
	@# no module cache) and fold them into the diff below. The Gateway API copies
	@# come from GOMODCACHE via `make extract-schemas`; the go.mod pin guards
	@# their version, so they are not regenerated here.
	@cp charts/haptic/crds/*.yaml $(SCHEMA_DIR)/
	@# --intent-to-add stages a brand-new CRD copy as empty so git diff sees it
	@# too, not just modifications to existing ones.
	@git add --intent-to-add '$(SCHEMA_DIR)/haproxy-haptic.org_*.yaml' >/dev/null 2>&1 || true
	@# Deliberately does NOT revert the regenerated files. It used to, which meant
	@# running this with a freshly regenerated (but not yet committed) type threw
	@# that work away and left the tree unbuildable. CI runs on a fresh checkout,
	@# so there is nothing there worth reverting either.
	@if ! git diff --quiet --exit-code -- charts/haptic/crds/ charts/haptic/charts/helm-helpers/files/default-values.json 'pkg/apis/**/zz_generated.*.go' '$(SCHEMA_DIR)/haproxy-haptic.org_*.yaml'; then \
		echo ""; \
		echo "ERROR: Generated files are out of date:"; \
		git diff --stat -- charts/haptic/crds/ charts/haptic/charts/helm-helpers/files/default-values.json 'pkg/apis/**/zz_generated.*.go' '$(SCHEMA_DIR)/haproxy-haptic.org_*.yaml'; \
		echo ""; \
		echo "The regenerated files have been left in place — commit them."; \
		exit 1; \
	fi
	@echo "✓ Generated code is up-to-date"

## Code generation

generate: generate-crds generate-deepcopy generate-clientset generate-chart-defaults ## Run all code generation

generate-chart-defaults: ## Copy the chart's default values to where its templates can read them
	yq -o json -I0 '... comments=""' charts/haptic/values.yaml > charts/haptic/charts/helm-helpers/files/default-values.json

generate-crds: ## Generate CRD manifests from Go types
	@echo "Generating CRD manifests..."
	@mkdir -p charts/haptic/crds
	$(CONTROLLER_GEN) crd:crdVersions=v1 \
		paths=./pkg/apis/haproxytemplate/v1alpha1/... \
		output:crd:dir=./charts/haptic/crds/
	@echo "✓ CRD manifests generated in charts/haptic/crds/"

generate-deepcopy: ## Generate DeepCopy methods for API types
	@echo "Generating DeepCopy methods..."
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt \
		paths=./pkg/apis/haproxytemplate/v1alpha1/...
	@echo "✓ DeepCopy methods generated"

generate-clientset: ## Generate Kubernetes clientset, informers, and listers
	@echo "Generating Kubernetes clientset, informers, and listers..."
	./hack/update-codegen.sh
	@echo "✓ Clientset, informers, and listers generated"

# Deliberately outside `make generate`: the output is the browser playground's
# `haproxy_valid` fallback, generated from pinned Data Plane API specs that no
# release moves any more. Run it by hand if a spec is ever refreshed.
generate-playground-validators: ## Regenerate the playground's OpenAPI validators
	@echo "Generating the playground's schema validators..."
	$(GO) run ./cmd/gen-validators
	@echo "✓ Validators generated in pkg/generated/validators/"

## Cleanup

clean: ## Clean build artifacts
	@echo "Cleaning..."
	rm -rf bin/
	rm -rf coverage/
	rm -f coverage.out coverage.html
	rm -f controller integration.test *.test

## Development helpers

fmt: ## Format code with gofmt
	@echo "Formatting code..."
	$(GO) fmt ./...

fix: ## Run automated code modernizers (go fix + gofmt)
	@echo "Running go fix..."
	$(GO) fix ./cmd/... ./examples/... ./pkg/... ./tests/... ./tools/...
	@echo "Formatting code..."
	$(GO) fmt ./...

vet: ## Run go vet
	@echo "Running go vet..."
	$(GO) vet ./...

## Installation helpers

install-tools: ## Install/sync all tool dependencies (from go.mod tools section)
	@echo "Installing tool dependencies..."
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	$(GO) install golang.org/x/vuln/cmd/govulncheck
	$(GO) install github.com/arch-go/arch-go/v2
	$(GO) install sigs.k8s.io/controller-tools/cmd/controller-gen@latest
	@echo "✓ All tools installed!"

## Convenience targets

dev: clean build test lint ## Clean, build, test, and lint (common dev workflow)
	@echo "✓ Development build complete!"

## Release targets

# RELEASE_VERSION is deliberately its own variable: VERSION defaults to the
# current VERSION file content, which would silently re-release the current
# version when the argument is forgotten.
release: ## Prepare a release of controller + chart (usage: make release RELEASE_VERSION=0.2.0)
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "Error: RELEASE_VERSION must be specified (e.g., make release RELEASE_VERSION=0.2.0)"; \
		exit 1; \
	fi
	@./scripts/release.sh $(RELEASE_VERSION)

.PHONY: build-docs-preview test-snapshot-publication
build-docs-preview: ## Build the versioned documentation preview artifact
	bash .github/scripts/build-docs-preview.sh

.PHONY: prepare-github-pages test-github-pages-browser archive-github-pages restore-github-pages
archive-github-pages: ## Archive every original website file and its source provenance
	python3 -m scripts.ci.github_site_archive create --source "$(PAGES_SOURCE)" --archive "$(PAGES_ARCHIVE)" --provenance "$(PAGES_PROVENANCE)"

restore-github-pages: ## Restore the complete pinned GitHub website baseline
	python3 -m scripts.ci.github_site_archive restore --config .github/pages-baseline.json --output "$(PAGES_OUTPUT)" $(if $(PAGES_ARCHIVE),--archive "$(PAGES_ARCHIVE)")

prepare-github-pages: ## Package a complete website below the GitHub Pages size limit
	python3 -m scripts.ci.github_pages --source "$(PAGES_SOURCE)" --output "$(PAGES_OUTPUT)"

test-github-pages-browser: ## Verify every packaged playground engine and the playground embed in a real browser
	node scripts/tests/test_github_pages_browser.mjs "$(PAGES_OUTPUT)"
	node --test docs/site/tests/playground-embed.mjs

.PHONY: build-github-website retain-github-website assemble-github-website restore-current-github-website
restore-current-github-website: ## Restore every version recorded in the GitHub website catalog
	python3 -m scripts.ci.github_site_state --output "$(PAGES_OUTPUT)"

build-github-website: ## Build the website from verified CI artifacts and archived versions
	python3 -m scripts.ci.github_site_publication build --version "$(WEBSITE_VERSION)" --output "$(PAGES_OUTPUT)"

retain-github-website: ## Retain the verified website before deployment
	python3 -m scripts.ci.github_site_publication retain

assemble-github-website: ## Restore a website from locally retained version archives
	python3 -m scripts.ci.github_site_versions --baseline "$(PAGES_SOURCE)" --catalog "$(PAGES_CATALOG)" --output "$(PAGES_OUTPUT)"

test-snapshot-publication: ## Verify image and chart publication in a local registry
	bash .github/scripts/test-snapshot-publication.sh

.PHONY: prepare-github-release-config publish-github-release smoke-spoa-image
.PHONY: prepare-spoa-release
prepare-spoa-release: spoa-prep ## Prepare the immutable bundle for a release branch
	bash scripts/spoa-release-image.sh prepare

.PHONY: tag-github-release
tag-github-release: ## Tag and dispatch a release after canonical source verification
	python3 -m scripts.ci.github_release_tag

publish-github-release: ## Publish one verified release phase to GitHub
	bash scripts/publish-github-release.sh "$(PUBLICATION_PHASE)"

.PHONY: publish-github-snapshot
publish-github-snapshot: ## Publish matching snapshot images and chart from verified main
	bash scripts/publish-github-snapshot.sh "$(PUBLICATION_PHASE)"

smoke-spoa-image: ## Run the native SPOA smoke suite against an immutable image
	bash scripts/smoke-spoa-image.sh "$(SPOA_SMOKE_IMAGE)"

prepare-github-release-config: ## Preserve release builds and signatures while targeting GitHub
	python3 -m scripts.ci.github_release --repository "$(GITHUB_REPOSITORY)" --output .pilot/goreleaser.github.yml $(if $(GITHUB_SNAPSHOT_SHA),--snapshot-sha "$(GITHUB_SNAPSHOT_SHA)")
	goreleaser check --config .pilot/goreleaser.github.yml

GORELEASER_CONFIG ?= .goreleaser.yml

goreleaser-snapshot: check-source-hash ## Test GoReleaser locally (no push)
	@set -eu; \
	for series in $$(yq -r '.haproxyPatchVersions | keys | .[]' charts/haptic/values.yaml); do \
		pin="HAPROXY_IMAGE_$$(printf '%s' "$$series" | tr -d '.')"; \
		image="$$(bash scripts/chart-haproxy-image.sh "$$series")"; \
		export "$$pin=$$image"; \
	done; \
	GITHUB_RUN_ID=$${GITHUB_RUN_ID:-local} SOURCE_HASH=$(SOURCE_HASH) goreleaser release --config "$(GORELEASER_CONFIG)" --snapshot --clean --skip=sign

## PGO (Profile-Guided Optimization) targets

pgo-profile: ## Collect CPU profile from dev environment for PGO
	@echo "Collecting 30-second CPU profile from dev environment..."
	@echo ""
	@echo "Prerequisites:"
	@echo "  1. Dev environment running: ./scripts/start-dev-env.sh"
	@echo "  2. Port-forward active: kubectl -n haptic port-forward deploy/haptic-controller 8080:8080"
	@echo ""
	@echo "Starting profile collection (30 seconds)..."
	curl -o cmd/haptic/default.pgo http://localhost:8080/debug/pprof/profile?seconds=30
	@echo ""
	@echo "Profile saved to cmd/haptic/default.pgo"
	@echo "Rebuild with: make build"

pgo-merge: ## Merge multiple PGO profiles into one
	@if [ -z "$(PROFILES)" ]; then echo "Usage: make pgo-merge PROFILES='profile1.pgo profile2.pgo'"; exit 1; fi
	@echo "Merging PGO profiles: $(PROFILES)"
	$(GO) tool pprof -proto $(PROFILES) > cmd/haptic/default.pgo
	@echo "Merged profile saved to cmd/haptic/default.pgo"
