# pkg/controller/proposalvalidator

Validates hypothetical configuration changes by rendering live stores with temporary overlays. `Service` owns validation; `Component` connects it to proposal events.

## Validation policies

| Caller | Entry point | Failure policy |
|--------|-------------|----------------|
| Admission webhook | `Service.ValidateSyncWithAdmissionSubject` | Checks observed inputs, then retries with only exact rejected revisions withheld |
| Background HTTP refresh | `Component` handling `ProposalValidationRequestedEvent` | Rejects invalid proposed output, including output identical to an invalid baseline |

The observed-input path retains its exact unchanged-output comparison. The
isolated-input fallback must produce valid output; it never uses that exception.
`ObservedStoreProvider` keeps admission snapshots in the reconciliation revision
family. `AdmissionStoreProvider` preserves all observed changes except exact
rejected revisions; `AcceptedStoreProvider` supplies validated inputs for HTTP
content proposals. HTTP-only proposals expose their pending URLs in `admissionSubject.httpURLs`; Kubernetes object identities remain absent. Every proposal still runs the complete render and output validation pipeline.

Cancellation and unavailable published files reject admission. Neither can use the unchanged-invalid exception. `CurrentFilesProvider` supplies one snapshot per decision, shared by the proposed and baseline renders.

## Admission service

```go
service := proposalvalidator.NewService(&proposalvalidator.ServiceConfig{
    Pipeline:             admissionPipeline,
    BaseStoreProvider:    storeProvider,
    CurrentFilesProvider: currentFilesProvider,
    Logger:               logger,
})
pipelineResult, result := service.ValidateSync(ctx, overlays)
```

The service has no event subscription or lifecycle. `ValidateSync` returns `(*pipeline.PipelineResult, *validation.ValidationResult)`. An admitted result includes the proposed rendered output, including when admission uses the unchanged-invalid exception. Rejections include the failing phase, error, and any warnings.

## HTTP-content event adapter

```go
adapter := proposalvalidator.New(eventBus, &proposalvalidator.ServiceConfig{
    Pipeline:             proposalPipeline,
    BaseStoreProvider:    storeProvider,
    CurrentFilesProvider: currentFilesProvider,
    Logger:               logger,
})
eventBus.Start()
go adapter.Start(ctx)
```

Construct every subscriber before starting the event bus. The adapter publishes a verdict carrying the request ID, including when validation panics. Cancelling its lifecycle context cancels active validation.

## Related packages

- [`pipeline`](../pipeline/) — render and validation execution
- [`dryrunvalidator`](../dryrunvalidator/) — admission overlays and error reporting
- [`httpstore`](../httpstore/) — pending HTTP-content promotion
- [`stores`](../../stores/) — temporary resource and HTTP overlays
