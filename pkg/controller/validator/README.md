# pkg/controller/validator

Configuration validators (scatter-gather participants).

## Overview

Four validators run as the responder side of the scatter-gather validation pattern. Each one implements `ValidationHandler.Validate`, which receives a typed config and returns a verdict and errors. The shared `BaseValidator` owns the subscription, input checks, and response publishing, including rejection after a panic. The orchestrator that fans the request out and aggregates responses is **not** in this package — it lives in `pkg/controller/configchange.ConfigChangeHandler`. Rendered-HAProxy-config validation (syntax + OpenAPI schema + `haproxy -c`) runs synchronously inside `pkg/controller/pipeline.Pipeline` via `pkg/dataplane.ValidateConfiguration`, not through this package.

## Validators

- **BasicValidator** — Structural validation (required fields, type checks, basic schema sanity).
- **TemplateValidator** — Calls `helpers.ExtractTemplatesFromConfig` to collect every template defined under `haproxyConfig`, `templateSnippets`, `maps`, `files`, and `sslCertificates`, then compiles them with `templating.NewScriggoWithDeclarations` to surface syntax errors before they reach the render pipeline.
- **JSONPathValidator** — Evaluates the `indexBy` JSONPath expressions on every entry under `spec.watchedResources` against a synthetic resource.
- **ValidationTestsValidator** — Runs the config's entire embedded `validationTests` suite (render + assertions) through `pkg/controller/configtest`, which drives `pkg/controller/testrunner`. By far the slowest responder, which is why its run budget scales with suite size and the orchestrator's scatter-gather envelope is derived from the same formula.

## Quick Start

```go
import "github.com/haproxy-haptic/HAPTIC/pkg/controller/validator"

basic := validator.NewBasicValidator(bus, logger)
tmpl := validator.NewTemplateValidator(bus, logger, bootstrap)
jp := validator.NewJSONPathValidator(bus, logger)

go basic.Start(ctx)
go tmpl.Start(ctx)
go jp.Start(ctx)
```

`BasicValidator` and `JSONPathValidator` take `(eventBus, logger)`; `TemplateValidator` and `ValidationTestsValidator` additionally take a `TypeBootstrapper`, so they can build the typed watched-resource declarations before compiling. None takes an engine or a validator-name list. Each validator's internal name (`"basic"`, `"template"`, `"jsonpath"`, `"validationtests"`) is what the orchestrator uses in its `ExpectedResponders` list.

## Events

### Subscribed

- `ConfigValidationRequest` — scatter-gather validation request (handled by all four validators)

### Published

- `ConfigValidationResponse` — one per validator, per request

A response with `Inconclusive` set reports that the validator couldn't judge the config: schemas are transiently unavailable, or the test run timed out without an assertion failure. It is never `Valid`. Live validation retries the candidate with capped backoff and keeps the last good config active until the full suite passes. Assertion failures remain conclusive rejections.

`ConfigValidatedEvent`, `ConfigInvalidEvent`, and `ConfigValidationInconclusiveEvent` are published by the orchestrator (`configchange.ConfigChangeHandler`) after collecting all four responses, not by these validators directly.

## License

Apache-2.0 — see root `LICENSE`.
