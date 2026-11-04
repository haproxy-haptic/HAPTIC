# pkg/controller/validator - Configuration Validation

Development context for validation components.

## When to Work Here

Work in this package when:

- Adding new validation rules
- Implementing new validator types
- Modifying scatter-gather coordination
- Fixing validation bugs

**DO NOT** work here for:

- Parsing configuration → Use `pkg/core/config`
- Event bus infrastructure → Use `pkg/events`

## Package Purpose

Houses the **responder** side of the configuration scatter-gather validation pattern. The orchestrator that fans `ConfigValidationRequest` out to these responders and aggregates the answers lives in `pkg/controller/configchange.ConfigChangeHandler`. Rendered-config validation (syntax + OpenAPI schema + `haproxy -c`) is performed synchronously inside `pkg/controller/pipeline.Pipeline` via `pkg/dataplane`'s `ValidateConfiguration` — there is no event-adapter for it.

## Architecture

```
ConfigParsedEvent
    ↓
configchange.ConfigChangeHandler  (issues request, gathers responses)
    ↓ ConfigValidationRequest (scatter via bus.Request)
    ├→ BasicValidator       (structural validation)
    ├→ TemplateValidator    (template syntax)
    ├→ JSONPathValidator    (JSONPath expressions)
    └→ ValidationTestsValidator (embedded validation tests)
        ↓ ConfigValidationResponse (gather)
configchange.ConfigChangeHandler  (publishes outcome)
    ↓
ConfigValidatedEvent  or  ConfigInvalidEvent  or  ConfigValidationInconclusiveEvent (retried)
```

## Validators

`BaseValidator` decodes each request and publishes its response. The handler receives the lifecycle context, typed config, and version, and returns a `Verdict`. A verdict is `Inconclusive` for a transient `SchemaAcquisitionError` (deadline, transport, apiserver 5xx/429) or a test run that times out without assertion failures; config defects remain conclusive. Classify by error type or structured test result, not message text. A panic produces a rejection through the same response path. Startup and live validation share `validationTestsVerdict`; startup still rejects every non-valid result, while live validation retries inconclusive results. Each caller keeps its own execution budget.

- **BasicValidator**: Structural validation (required fields, types)
- **TemplateValidator**: Template syntax validation. Calls `helpers.ExtractTemplatesFromConfig`, which walks `spec.haproxyConfig`, `spec.templateSnippets`, `spec.maps`, `spec.files`, and `spec.sslCertificates` (there is no flat `spec.templates` field), then compiles them with `templating.NewScriggoWithDeclarations`.
- **JSONPathValidator**: JSONPath expression validation (evaluates each `indexBy` expression)
- **ValidationTestsValidator**: Runs the embedded validation tests; incomplete runs without assertion failures are inconclusive.

## Resources

- Scatter-gather pattern: `pkg/events/CLAUDE.md`
- Configuration schema: `pkg/core/CLAUDE.md` (or `pkg/core/config/README.md` for the public API)
