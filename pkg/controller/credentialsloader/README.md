# pkg/controller/credentialsloader

Stage-1 event adapter that turns `Secret` updates into internal `Credentials`. Subscribes to `SecretResourceChangedEvent`, calls `config.ParseSecretData` + `config.LoadCredentials` (which rejects missing or empty `dataplane_username` / `dataplane_password`), and publishes `CredentialsUpdatedEvent` on success. Invalid updates are logged and leave the previously accepted credentials active. `config.ValidateCredentials` repeats the required-field check and rejects nil credentials at controller startup in `pkg/controller/config.go`.

Like its sibling [`configloader`](../configloader/), it's built on the `pkg/controller/component.Base` scaffold — the event-loop plumbing is shared, only the parse step differs.

## Minimal Usage

```go
import (
    "github.com/haproxy-haptic/HAPTIC/pkg/controller/credentialsloader"
    "github.com/haproxy-haptic/HAPTIC/pkg/events"
)

loader := credentialsloader.NewCredentialsLoaderComponent(bus, logger)
go loader.Start(ctx)
```

Subscription happens in the constructor, so buffered `SecretResourceChangedEvent`s from the single-resource watcher's initial sync are delivered when `bus.Start()` is called.

## Required Secret Keys

Two non-empty keys authenticate requests to each HAPTIC agent. Local HAProxy validation needs no credentials:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: haproxy-credentials
type: Opaque
stringData:
  dataplane_username: admin
  dataplane_password: <random>
```

`config.LoadCredentials` rejects either key being missing or decoded-to-zero-bytes. On failure the loader logs the Secret version and validation error; the previously accepted credentials stay active.

## Event Contract

**In** — `SecretResourceChangedEvent{Resource}` from a `SingleWatcher` that points at the Secret selected by `--secret-name` / `SECRET_NAME`. The `resourceVersion` is read from the unstructured resource by the loader itself; there's no separate version field on the event.

**Out** — `CredentialsUpdatedEvent{Credentials, SecretVersion}` on success, for every Secret update including label-only ones; `configchange` restarts the iteration only when the credentials differ from the active ones. Failures are logged directly because no component coordinates on them.

## See Also

- [`pkg/core/config`](../../core/config/) — `ParseSecretData` / `LoadCredentials` (the functions this adapter wraps), and `ValidateCredentials` for the startup-time structural check
- [`pkg/controller/component`](../component/) — shared event-loop base
- [`pkg/controller/configloader`](../configloader/) — sibling loader built on the same pattern
- `docs/site/docs/operations/security.md` — operator-facing credential rotation + secret-management guidance

## License

Apache-2.0 — see root `LICENSE`.
