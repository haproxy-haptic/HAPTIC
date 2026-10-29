# HAPTIC

<div align="center">

<img src="docs/site/docs/assets/logo.svg" alt="HAPTIC" width="400" height="150">

</div>

[![License](https://img.shields.io/badge/License-Apache%202.0-007ec6)](https://opensource.org/licenses/Apache-2.0)
[![CI](https://github.com/haproxy-haptic/HAPTIC/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/haproxy-haptic/HAPTIC/actions/workflows/ci.yml)

**HAPTIC** (**HAP**roxy **T**emplate **I**ngress **C**ontroller) is a template-driven [HAProxy](https://www.haproxy.org/) Ingress Controller for Kubernetes that generates HAProxy configurations using [Scriggo](https://scriggo.com/) templates and deploys them through the HAPTIC agent in each HAProxy pod.

> **Documentation**: <https://haproxy-haptic.org/>

## Key Features

### Template-Driven Flexibility

Traditional ingress controllers embed configuration logic in code. HAPTIC inverts this:

- **Full HAProxy access** - If HAProxy supports it, your templates can emit it — every section, every directive in the [configuration manual](https://www.haproxy.com/documentation/haproxy-configuration-manual/latest/)
- **Add features without code changes** - New directives are template updates, not controller releases
- **Rich template context** - Access any Kubernetes resource, fetch external data via HTTP, and use controller state in your templates
- **Everything is templatable** - Generate not just `haproxy.cfg` but also map files, SSL certificates, CRT-lists, and custom auxiliary files

### Validation and operations

- **High availability** - Leader election with automatic failover
- **Layered validation** - Admission webhook, template validation, and tests you can run in CI before anything reaches a cluster
- **Observability** - Prometheus metrics, structured logging, and debug endpoints

## Quick Start

Features described on `main` can include unreleased changes. Select your installed
release in the [versioned documentation](https://haproxy-haptic.org/docs/) before
following setup instructions.

```bash
helm install my-controller oci://ghcr.io/haproxy-haptic/haptic/charts/haptic --version 0.8.1
```

The chart ships with [template libraries](https://haproxy-haptic.org/docs/dev/template-libraries/) for Kubernetes Ingress and Gateway API enabled by default. Use them for standard routing, or extend them with your own templates.

For complete setup instructions including HAProxy pod deployment, see the [Getting Started](https://haproxy-haptic.org/docs/dev/getting-started/) guide.

## Development

For AI-assisted configuration, see the [agent skill guide](https://haproxy-haptic.org/docs/dev/development/agent-skill/).

### Build and Test

```bash
# Build the controller binary
make build

# Run unit tests
make test

# Run integration tests (requires kind cluster)
make test-integration

# Run Gateway API conformance (requires the test-e2e cluster)
make test-gateway-conformance

# Run Ingress conformance (requires the test-e2e cluster)
make test-ingress-conformance

# Run linting checks
make lint

# Run tests, linting, and security checks
make check-all

# Build Docker image
make docker-build

# Generate coverage report
make test-coverage
```

### Local Development Environment

The project includes scripts for local development with kind:

```bash
# Start development cluster with controller
./scripts/start-dev-env.sh

# Rebuild and restart controller after code changes
./scripts/start-dev-env.sh restart

# View controller logs
./scripts/start-dev-env.sh logs

# Check deployment status
./scripts/start-dev-env.sh status

# Test ingress functionality
./scripts/start-dev-env.sh test

# Clean up development environment
./scripts/start-dev-env.sh down
```

> [!WARNING]
> Always use the `kind-haptic-dev` cluster context for development work. The `kind-haproxy-test` context is reserved for integration tests and will be automatically created and destroyed by test runs.

### Pre-commit Hooks

Set up automatic code quality checks using pre-commit:

```bash
# Install pre-commit (one-time setup)
pip install pre-commit
# or: brew install pre-commit

# Install git hooks (one-time per repository clone)
pre-commit install

# Hooks now run automatically on git commit (run `make lint` and `make audit`)
git commit -m "my changes"

# Run hooks manually on all files
pre-commit run --all-files
```

Do not bypass the hooks with `--no-verify`; CI runs the same checks, so skipping them only defers the failure.

## Contributing

Contributions are welcome. Before submitting pull requests:

1. Run `make check-all` to verify code quality
2. Add tests for new functionality
3. Update documentation as needed
4. Follow existing code style and patterns

See the [development documentation](docs/site/docs/development/design.md) for architecture and patterns.

## License

Licensed under the Apache License 2.0 - see [LICENSE](LICENSE) file for details.

Copyright 2025 Philipp Hossner

## Acknowledgments

This project builds on open source software:

- [Kubernetes client-go](https://github.com/kubernetes/client-go) - Kubernetes API client library
- [HAProxy client-native](https://github.com/haproxytech/client-native) - HAProxy models and parsing used by the playground and test oracles
- [Scriggo](https://scriggo.com/) - Go-native templating engine
