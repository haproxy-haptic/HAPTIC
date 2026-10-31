# pkg/dataplane - the path from a render to a running HAProxy

Development context for the plan a render declares, the decision the controller
makes from it, the agent that executes it, and the `haproxy -c` runner.

**API Documentation**: See `pkg/dataplane/README.md`
**The contract, end to end**: `docs/site/docs/development/agent.md`
**Why**: `docs/adr/0022-haptic-agent.md`

**No Lua-based features or workarounds, including HAProxy patches to enable them.**
Use native HAProxy capabilities; see [the project-wide rule](../../CLAUDE.md#no-lua-features-rule-4).

## When to Work Here

Modify this package when:

- Changing what a render declares about itself (`renderplan`)
- Changing what a change costs — a new op kind, a new rule (`deployplan`)
- Changing the wire contract (`agent/api`) or either end of it
- Changing how the agent writes files, runs commands or recovers
- Touching the `haproxy -c` runner

**DO NOT** modify this package for:

- Template rendering → Use `pkg/templating`
- Event coordination → Use `pkg/controller`
- Kubernetes integration → Use `pkg/k8s`
- Endpoint discovery → Use `pkg/controller/discovery`

## Package Structure

```
pkg/dataplane/
├── renderplan/                 # What a render declares: sections, backends, maps, files
├── deployplan/                 # What one pod has to do to reach a render
├── agent/
│   ├── api/                    # The wire contract, compiled by both ends
│   ├── client/                 # The controller's end: State + streaming Apply
│   ├── server/                 # The agent: HTTP surface, state machine, transaction
│   ├── files/                  # The file tree it owns: mounts, journal, temp+rename
│   ├── cli/                    # Typed ops → HAProxy runtime commands
│   ├── agenttest/              # An in-process fake agent, for deployer tests
│   └── haproxytest/            # A modelled worker + master socket, for agent tests
├── auxiliaryfiles/types.go     # Auxiliary-file types the renderer produces
├── validate_haproxy.go         # `haproxy -c`
├── haproxy_exec.go             #   its process runner
├── validator.go                # ValidateConfigurationContext entry point
├── errors.go                   # ValidationError / SimplifyValidationError / ...
├── parser/                     # playground-only: the client-native syntax parse
├── validators/                 # playground-only: the OpenAPI schema check
├── validate_syntax.go          #   both are `//go:build playground`
├── validate_schema.go          #
└── dataplanetest/              # Fake haproxy binary for unit tests (see below)
```

The `playground` build tag is the whole of the exception below: those files
build for the browser playground, which has no HAProxy binary, and for nothing
else. `scripts/check-client-native-free.sh` fails the build if a production
binary starts linking them; `make test-playground` is what runs their tests.

## The one rule this package exists to keep

**Nothing in production parses HAProxy configuration.** The generator declares
what it generated; HAProxy parses what it is given. A change that needs to know
the structure of a config file is a change to `renderplan` (declare more), never
a parser.

`depguard` enforces it: `haproxytech/client-native`'s parser is reachable only
from the differential CI test and from the playground build tag.

## Key Concepts

### The plan is the contract between render and deploy

`renderplan.Plan` is immutable data with a digest for an ID. Two identical
renders produce the same ID, which is what makes a re-render a noop rather than
a reload. Its `Canonical()` encoding is the digest input, so **any new field is
part of the identity** — adding one changes every plan ID once, which costs the
fleet one reload. That is acceptable; silently excluding a field from the digest
is not, because two different renders would then compare equal.

The one exception: a secret file's `File.Digest` is replaced by its keyed
`Seal` in the identity and in retained checkpoints, because both are readable
without Secret access and an unkeyed digest would let a reader test guesses of
the secret offline. The wire manifest still carries the digest the agent verifies.

### The decision layer never refuses

`deployplan.Diff` returns a verdict for every input. A nil baseline, a schema
mismatch, an op kind the pod's agent does not execute, more ops than a batch
allows — each of them degrades to `reload` with a reason, never to an error.
The reasons ship in the pod's status, so an operator can see what cost them a
reload.

The rules are table tested per rule (`deployplan/*_test.go`). A new rule needs
its own table, and a rule that can force a reload needs a case proving it does.

### The agent executes, it does not decide

The agent's fallbacks are all the same fallback: reload the file set on disk. An
unknown op kind, a baseline it does not recognise, a command HAProxy rejects,
a worker that changed underneath a batch — all of them reload. That is why a
classification bug ships by rolling the controller, not the data plane.

### Fencing before writing

Every apply carries the baseline it was composed against plus a token
(leader epoch, render sequence). A mismatch is a `409` carrying the agent's
actual state and **nothing is written**. The caller re-diffs from what came
back. Never "just retry" an apply that 409'd — the ops in it were composed for
a state the pod is not in.

### Disk is the authority

At startup the agent hashes its tree and compares with its persisted state file.
A mismatch means the baseline is unknown, and the next apply is full state plus
a reload. Absence in a manifest deletes a path **only if the agent put it
there** — its own dot directories and anything HAProxy writes itself (an `acme`
account key, for instance) are not its to remove.

### Runtime names are manifest paths

Under `default-path origin` and `crt-base`, HAProxy names a map, a certificate
and a crt-list by the literal base-relative string the configuration references.
That string is the manifest's `File.Path` and an op's `Path`, so no component
translates paths. The one asymmetry: a crt-list *line* token is resolved against
`crt-base`, so `crtlist_add.Cert` is the bare filename while `cert_set.Path` is
`ssl/<file>`. `tests/agent` pins both.

## Adding an op kind

1. Add the constant and its field comment to `agent/api/api.go`. The contract is
   additive within a major: a new kind is fine, changing what one means is not.
2. Add the compiler to `agent/cli/program*.go` — command text, expected reply,
   abort commands. Every token goes through `validateToken`; a value that can
   carry spaces goes through the payload form, not the line form.
3. Teach `deployplan` to compose it, with a table case.
4. Teach `agent/haproxytest` to model it, so the unit tests cover it.
5. Add a case to `tests/agent`, which runs it against real HAProxy.

An agent reports the kinds it executes in `/v1/state`; a controller that
composes a kind the pod does not report gets a reload instead. So step 3 is
safe to land before every pod has step 2.

## Testing Strategies

Four layers, each answering a different question:

| Layer | Question | Where |
| --- | --- | --- |
| Unit | Does the client hold the contract's limits, and does every rule fire? | `agent/client`, `deployplan` |
| Fake HAProxy | Does the transaction, fencing and op execution behave, including under injected faults? | `agent/haproxytest`, used by `server` and `cli` |
| Fake agent | Does the deployer react correctly to conflicts and rejections? | `agent/agenttest` |
| Real HAProxy | Does HAProxy do what the contract says? | `tests/agent` (docker), `tests/integration` (a pod in Kind) |

```bash
make test                      # unit
make test-agent-docker         # HAProxy + agent in containers
make test-integration          # the same, as a pod in a Kind cluster
```

### Faking the HAProxy binary (required in unit tests)

A unit test must never shell out to a real `haproxy`. Packages that reach the
validation path install a fake in `TestMain`:

```go
func TestMain(m *testing.M) {
    dataplanetest.InstallFakeHAProxy()
    os.Exit(m.Run())
}
```

Real-binary verdicts belong in `tests/agent` and `tests/integration`.

## Common Pitfalls

### Deciding inside the agent

An `if` in the agent that chooses between two HAProxy behaviours is a bug in
the making: it puts a classification decision in the data plane, where a fix
means rolling every HAProxy pod. Move it to `deployplan` and let the agent do
what it is told.

### Reaching for a parser

"Just parse the config to find out X" is how this package looked before
ADR-0022. If a decision needs X, the render declares X.

### Trusting a cached inventory

The runtime inventory is what the worker *had* loaded. It goes stale when the
agent's own ops add a store entry, when HAProxy reloads, and when the container
restarts underneath. Every one of those re-reads it. A decision made against a
stale inventory composes a create for something that exists, HAProxy refuses it,
and the pod reloads for a change that was reload-free.

### Assuming a listing format

HAProxy's `show` output differs per command: `show map` wraps the path in
parentheses, `show ssl ca-file` appends `" - <n> certificate(s)"` and lists
`@system-ca`, `show ssl cert` is a bare path. Model a new one in
`agent/haproxytest` **as HAProxy prints it**, and pin it in `tests/agent`
against a real one — a mock that answers in a shape HAProxy never uses hides
the bug instead of catching it.

## Resources

- `docs/site/docs/development/agent.md` — the contract, both ends, with the flags
- `docs/adr/0022-haptic-agent.md` — the decision and its measurements
- `docs/site/docs/supported-configuration.md` — what a change costs, for operators
- `pkg/controller/deployer` — the event adapter that drives all of this
- [HAProxy Runtime API](https://www.haproxy.com/documentation/haproxy-runtime-api/) — the commands the agent runs
