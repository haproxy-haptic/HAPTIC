# Supported HAProxy configuration

Your templates can generate any directive supported by the HAProxy version you
run. How a change reaches the running proxy depends on the directive: some
changes use the Runtime API, while others require a reload. Use the tables below
to check what to expect. To preview a specific change, use
[`haptic diff`](operations/debugging.md#common-recipes).

The bundled libraries describe backends, servers, maps, and certificates to
HAPTIC so it can update them at runtime. Custom templates can use the same
[helpers](libraries/reload-free.md). Configuration written outside those helpers
still deploys, but changes to it require a reload.

## Reload behavior

The controller compares the desired configuration with each pod's acknowledged
state. It chooses an action for that pod:

| Action | What happens |
|--------|--------------|
| `runtime` | The agent writes the files and applies supported changes to the running HAProxy worker. |
| `file_only` | The agent updates files without runtime commands or a reload. This includes a map the worker hasn't loaded. |
| `reload` | The agent writes the configuration and requests a reload through the master socket; HAProxy must accept the new configuration. |

A reload starts a new worker for new connections while the old worker drains
existing connections. Connections still open at `hard-stop-after` are closed;
see [Reload pacing and drain limits](./operations/performance.md#graceful-reload-drain-bound).

A missing baseline, an unsupported agent operation, or a change the controller
can't express at runtime requires a reload. If a runtime operation fails, the
agent attempts a reload with the desired configuration. Validation or reload
failures remain deployment errors.

### Server changes

For backends described by the templates, HAPTIC can apply these changes at
runtime on HAProxy 3.0 and later:

- Change a server's IP address, port, weight, or maintenance state.
- Add a server when its address is a literal IP, its keywords support runtime
  creation, and its load-balancing algorithm accepts dynamic servers.
- Remove a server after disabling it and draining its connections.

`roundrobin`, `leastconn`, `random`, and `first` accept dynamic servers. Hash-based
algorithms require `hash-type consistent`; `static-rr` doesn't accept server
additions at runtime. The bundled libraries set consistent hashing for hash-based
algorithms unless you override it.

Adding a server and changing an existing server have different limits. For
example, a new server can carry `ssl`, `check`, and `maxconn`, but changing those
keywords on an existing server requires a reload. Server additions also require
referenced certificate and CA files to be available in HAProxy's runtime store.

### Backend changes

HAProxy 3.4 and later can add and remove backends at runtime when templates use
`Backend()` and declare a dynamic backend. A new backend must inherit a named
`defaults` profile already present in the running configuration. Introducing a
new profile requires a reload first.

Changing an existing backend's profile, mode, balance algorithm, `hash-type`,
`guid`, `default-server` settings, or body requires a reload. A structural backend
can still receive supported server updates at runtime.

See [When a backend is static](./libraries/reload-free.md#when-a-backend-is-static)
for template authoring constraints.

### Maps, certificates, and auxiliary files

| Change | Behavior |
|--------|----------|
| Content of a map loaded by the worker | Runtime entry updates; whole-map replacement when needed to preserve entry order or represent the values. |
| Content of a map the worker hasn't loaded | File update only. Adding a configuration reference requires a reload. |
| Certificate or CA content | Create or update the runtime-store object. |
| Add, remove, or change a CRT-list, including its entries | Reload. Templates register CRT-lists as files, so their entries can't be updated at runtime. |
| Change or remove a general file with `reloadOnPush: true` (the default) | Reload. |
| Other file changes | File update only; changes to the configuration that references the file may also require a reload. |

The controller checks each pod's reported runtime capabilities before sending
operations. All runtime changes also update the files on disk so a later reload
reads the desired content.

For maps, declare `ordered: false` only when lookup order doesn't affect matching.
See [`maps`](./crd-reference.md#maps) for the lookup types that allow this.

### Routing changes

The bundled libraries keep host and path routing, header values, redirects, and
settable timeouts in maps. Updating those values can avoid a reload when the
configuration already contains the required processing rules and backends.

A new header name can require a new processing rule. A new backend profile,
listener, access control list, or inline request rule also changes configuration
text and requires a reload. See [Reload-free routing](./libraries/reload-free.md)
for the library helpers that separate changing values from shared rules.

## Listen sections

Templates can emit `listen` sections. The bundled libraries use separate
`frontend` and `backend` sections so eligible backend changes can run at runtime.
Changes to a `listen` section require a reload.

## HAProxy versions and validation

Admission and configuration loading run `haproxy -c` before accepting a change.
During reconciliation, auxiliary-file validators run before deployment, while
the HAProxy render check runs alongside it. The pod's own HAProxy binary must
accept a reload. See [validation failures and recovery](operations/debugging.md#haproxy-refused-the-config-the-fleet-was-given-configvalidatedfalse).

Use matching HAProxy versions for the validator and the deployed pods. The chart's
`haproxyVersion` value selects both. Directives unavailable in that version fail
validation; emitting a directive doesn't add support for it to HAProxy.

The bundled images use HAProxy Community Edition. Enterprise-only directives
require a compatible Enterprise binary and validation setup; the controller
doesn't enable Enterprise features automatically. See
[HAProxy versions](./operations/haproxy-versions.md) and
[Pluggable validators](./operations/pluggable-validators.md).
