# pkg/dataplane

Pure libraries for the path between a render and a running HAProxy: the wire
contract with the HAPTIC agent, the plan a render declares about itself, the
rules that decide what one pod has to do, and the `haproxy -c` runner.

Module path: `github.com/haproxy-haptic/HAPTIC`. Source is authoritative
(`go doc ./pkg/dataplane/...`); this README is a short map.

## What the packages do

1. `renderplan` holds the structure a render declares — the sections of
   `haproxy.cfg`, the backend and server records behind them, the entries of
   every map and crt-list, the file set. The generator produces it; nothing
   parses HAProxy configuration.
2. `agent/api` is the wire contract, compiled by both ends: `GET /v1/state`, `PUT /v1/plan`,
   `POST /v1/apply`, the typed ops, the limits.
3. `deployplan` compares two plans against one pod's baseline and returns a
   verdict — `runtime`, `file_only` or `reload` — plus the ops to run. Pure
   data in, data out, table tested per rule.
4. `agent/{server,files,cli}` is the agent: the transactional file tree, the
   op-to-command table, the state machine. `agent/client` is the controller's
   end.
5. `validate_haproxy.go` and `haproxy_exec.go` run `haproxy -c`, which the
   webhook, the config-load gate and `rendergate` all use.

## Sub-package map

| Purpose | Package |
|---------|---------|
| The structure a render declares about itself | `renderplan/` |
| The controller ↔ agent wire contract | `agent/api/` |
| What one pod has to do to reach a render | `deployplan/` |
| The agent: file tree, state machine, HTTP surface | `agent/{files,server}/` |
| Typed ops to HAProxy runtime commands | `agent/cli/` |
| The controller's end of the contract | `agent/client/` |
| An in-process fake agent for controller tests | `agent/agenttest/` |
| A modelled HAProxy worker and master socket | `agent/haproxytest/` |
| `haproxy -c` | `validate_haproxy.go`, `haproxy_exec.go`, `validator.go` |
| Auxiliary-file types the renderer produces | `auxiliaryfiles/types.go` |

Endpoint discovery (probing HAProxy pods, picking up credentials) is the
controller's job in `pkg/controller/discovery`, not this package's.

`parser/`, `validators/` and `validate_syntax.go` / `validate_schema.go` build
only under the `playground` build tag: they are the syntax + schema check the
browser playground answers `haproxy_valid` with, because a browser has no
HAProxy binary. No production binary may link them.

## Deciding what a change costs

```go
decision := deployplan.Diff(next, &deployplan.Baseline{
    Applied:   applied,                 // the plan this pod ACKed; nil means unknown
    Inventory: state.Inventory,         // what its worker actually loaded
    Caps:      deployplan.CapsFor(state.HAProxy.Version, state.AgentOps),
})
```

`Decision.Verdict` is what the pod does, `Decision.Ops` are the commands it
runs, `Decision.Files` is always the complete desired set, and
`Decision.Reasons` names every change that could not run at runtime — which is
what an operator reads when a render costs them a reload.

A nil baseline is not an error: the pod gets the complete file set and a reload.
That is also what a version skew, an unknown op kind and a rejected command
degrade to. The decision layer never refuses.

## Zero-reload rules of thumb

What stays off the reload path:

- Map entries; certificate, CA and crt-list content
- Secret files, which are plain writes for sidecars
- A server's address, port, weight or admin state
- A server added to or removed from a backend (3.0+; 3.1+ joins without a reload)
- A backend added or removed, on HAProxy 3.4, when the render declares it dynamic

What reloads: a section's text, a named defaults profile appearing or
disappearing, a file declared reload-on-change, and configuration text no
section accounts for.

A server keyword HAProxy has no runtime setter for takes that server's change
off the lane — the list is `deployplan/keywords.go`. That is why the bundled
libraries put `check` on `default-server` rather than on every server line.

## Testing

```bash
make test                            # unit tests, including the deployplan tables
make test-agent-docker               # a real HAProxy and the agent, in containers
make test-integration                # the same, as a pod in a Kind cluster
```

## See also

- `pkg/dataplane/CLAUDE.md` — the contract's invariants, the op table, testing strategy
- `docs/site/docs/development/agent.md` — the agent and the controller's side of it
- `docs/adr/0022-haptic-agent.md` — why the Data Plane API went
- `pkg/controller/deployer` — the event adapter that drives all of this
- `docs/site/docs/supported-configuration.md` — the user-facing view of what a change costs

## License

Apache-2.0 — see root `LICENSE`.
