# ADR-0020: One authoritative validation pipeline for rendered output

## Status

Accepted 2026-08-10; amended 2026-08-26 to remove built-in and protocol-v1
external verdict reuse. Supersedes ADR-0016's strict-first/fast-later
validation implementation; its other decisions stand.

[ADR-0022](0022-haptic-agent.md) supersedes decision steps 1 and 2 — the syntax
parse and the schema check — for every caller: no production binary parses
HAProxy configuration any more, and `haproxy -c` is a strict superset of both
for whether HAProxy can load the output. It supersedes step 3, the synchronous
`haproxy -c`, on the reconcile path only: that render is now render-only, and
the same check runs asynchronously in `rendergate`. The webhook and the
config-load gate keep step 3, and step 4 (the pluggable output validators) is
unchanged everywhere.

[ADR-0032](0032-watched-input-isolation.md) restores synchronous complete output
validation before watched input revisions are accepted. The asynchronous render
gate remains as an additional check.

## Context

Validation depended on the trigger path. Watched-resource admission and HTTP
promotion used full semantic validation, while reconciliation ran `haproxy -c`
only on the first render of an iteration. Pluggable validators ran only from
the admission adapter. Deletes, HTTP refreshes, startup races, and ordinary
resource changes could therefore produce output that had never passed every
configured gate.

The first-render shortcut also treated validation as a property of an
iteration. It's a property of the rendered config and all auxiliary files.
Those files can change without restarting the iteration.

## Decision

One pipeline owns render validation. Leader reconciliation, watched-resource
admission, and HTTP-store promotion all call it with their respective store
views. On every invocation it runs, in order:

1. client-native syntax parsing;
2. version-specific OpenAPI schema validation;
3. `haproxy -c` semantic validation; and
4. every configured rendered-output validator against the complete file set.

No success event is published before all stages pass. External-validator
errors use pipeline phase `external`; warnings remain on the successful result
and reach admission responses or reconciliation observability.

Cancellation isn't a verdict. The pipeline checks its authority between stages
and before returning success, and an interrupted external-validator fan-out
fails closed. A leader whose term context has ended discards the pipeline result
without publishing either success or failure events from the retired term.
The template boundary converts cancellation panics to `RenderTimeoutError`;
fatal template errors still panic while the render context remains active.

Built-in HAProxy validation and every configured protocol-v1 external
validator execute on every applicable invocation, including byte-identical
repeats. Exact output proves the input, not the environment that judges it.
The HAProxy binary or executor can change, and protocol v1 exposes no
authenticated validator-runtime identity.

Future verdict reuse requires a protocol that:

- obtains an authenticated hermetic-environment root through a `linearizable` lookup;
- covers the executable, configuration, dependencies, and runtime generation;
- binds the verdict to that root and an exact canonical input witness;
- stores and returns defensive response copies; and
- fails closed on cancelled, partial, missing, stale, or otherwise incomplete observations.

Until all of those properties are proven together, verdicts remain local to
one validation occurrence.

Proposal admission has one narrow recovery exception: if the live baseline is
already invalid, the proposal may proceed only when both renders complete and
their output checksums are identical. A render error or any changed invalid
output is denied. A validator transport or protocol failure supplies no verdict
for either render. Admission returns a retryable failure, and input isolation
retains its last accepted state until validation can finish.

## Consequences

- Every route into publication and deployment enforces the same validation
  surface.
- Pluggable validators cover deletes, HTTP refreshes, config changes, startup,
  and ordinary reconciliation, not only admission requests.
- Built-in HAProxy and protocol-v1 external validators run for drift checks and
  no-op renders, so a changed runtime verdict is observed even for exact output.
- HAProxy's own parse at reload remains a second gate, not justification for
  skipping the controller gate.
