# HAProxy Template Ingress Controller - Development Context

This file contains cross-cutting development context for working on this codebase. For package-specific context, see CLAUDE.md files in relevant subdirectories.

## Project Overview

Event-driven Kubernetes operator that manages HAProxy configurations through template-driven approaches. Uses pure components wrapped in event adapters for clean separation of concerns.

Architecture documentation: `docs/site/docs/development/design.md`

## Resource-Agnostic Design (RULE #1)

**The Go code must be agnostic to every Kubernetes resource an operator may want to watch.** This is HAPTIC's reason for existing as a separate project; it must hold across every design decision.

**The litmus test.** If an operator switched their setup from Gateway/Ingress to a custom CRD, they should only need to touch HAPTIC templates and config — **no Go code**. Writing templates for the operator's CRD must be just as comfortable as writing them for Ingress or Gateway API resources. **There must be no preferential treatment for well-known resources.**

**What this rules out in Go:**

- Pre-generated wrappers, helpers, or filters bound to specific kinds (`Service`, `Ingress`, `Gateway`, `HTTPRoute`, `BackendTLSPolicy`, etc.).
- Function signatures that *encode* a resource path even when the body is technically generic (e.g. `listenerTransitionTime(gateway, listenerName, status)` is wrong — `gateway.status.listeners[byName].conditions` is baked into the parameters).
- "Chart-render-time runtime context" types whose field names embed specific resource knowledge — e.g. `GlobalFeatures.TLSCertificates`, `SSLPassthroughBackend`, `GatewayListenerMTLSConfig`. These look generic at a glance but only make sense for charts that watch Secrets and Gateways. A different operator's chart wouldn't use them.
- Anything that requires controller-side build-time knowledge of the resource set. Schemas arrive from the kube-apiserver (live) or `--schema-dir` (offline) at runtime.

**What stays acceptable:**

- Generic engine utilities operating on the dig/typed-struct surface: `dig`, `dig_string`, `fallback`, `toSlice`, `to_str_map`, `tostring`, etc.
- Typed access to *any* watched resource via `pkg/k8s/typegen`, because typegen consumes the schema at runtime; it knows nothing about which resources will be there until the controller starts.
- Resource shape stored as `map[string]any` plus the cost of `.(map[string]any)` casts in chart code — this is the price of generality, accept it rather than carving Go-side exceptions for the bundled chart's specific resources.

**Scope — what RULE #1 does NOT govern (the operational-identity exception).** The rule is about the resources an operator *watches as template inputs* — the routing/application objects whose shape the chart consumes (Ingress, Gateway, HTTPRoute, a custom CRD, …). It does **not** govern the controller's own operational identity: the fixed inputs HAPTIC needs to bootstrap and run *itself*. Concrete, kind-specific Go for these is correct — **not** a violation, and must not be "fixed" into config-derived indirection:

- **HAPTIC's credentials source.** The Dataplane API credentials Secret — its hardcoded GVR, a `SecretResourceChangedEvent`, parsing its `.data` — is the controller's own config input, not something the chart templates over. Hardcoding "Secret" here is fine.
- **The HAProxy fleet HAPTIC manages.** The auto-injected `haproxy-pods` self-watch — its store key, the store separation, discovery filtering — is the controller observing the pods it deploys to. Operational plumbing, not a user-watched routing resource.
- **HAPTIC's own CRDs.** The `HAProxyTemplateConfig` input CRD and the output CRDs (`HAProxyCfg`, `HAProxyMapFile`, …) are HAPTIC's own API surface; typed clients and kind-specific code for them are expected.

The test for the exception: *could an operator plausibly swap this resource out as part of describing their routing?* If yes (a routing/application object the templates consume), it must stay generic. If it's a fixed input HAPTIC needs to function at all (its credentials, the fleet it manages, its own CRDs), concrete Go is allowed — generalising it only adds config knobs and indirection for zero real portability gain. Do not file these as RULE #1 violations.

**Sweep rule.** If you touch one resource-coupled helper anywhere in `pkg/`, sweep ALL helpers in that package and remove every other one too. The rule is per-package, not per-helper. See `pkg/templating/CLAUDE.md` for the in-engine version.

**Corollary on the chart side.** Resource-specific behaviour lives in resource-specific libraries (`ingress.yaml`, `gateway/*.yaml`, `haproxytech.yaml`, etc.), never in `base.yaml`. Vendor annotation libraries (`haproxy-ingress/`, `nginx-ingress/`, etc.) follow the same pattern. See `charts/CLAUDE.md` for the chart-side version.

## Validation Is Never Traded Away (RULE #2)

**Sacrificing validation to make an operation succeed is ALWAYS out of the question.**
Not to unblock an upgrade, not to turn a red pipeline green, not to ship on time. The
validation surface — the bundled `validationTests`, the admission webhook, the startup
load gate, the CRD schemas — *is* the product's guarantee that a config change cannot
break an operator's fleet. Remove a piece of it and HAPTIC still runs; it just stops
being trustworthy, which is the only reason to run it.

**None of these are fixes. They are the bug, relocated:**

- Narrowing a webhook's `objectSelector`, `rules`, or `namespaceSelector` so the
  objects that fail it stop being checked.
- Deleting, skipping, or `_helm_skip_test`-ing a validationTest that went red.
- Relaxing a CRD's `required:` list so a malformed object is storable.
- Widening an assertion, a timeout, or a tolerance until the failure stops reproducing.
- Turning a check off "only for a window" / "only during migration" with nothing
  stronger standing in its place.

**A gate that blocks a legitimate operation is reporting a design defect.** It is doing
its job. The defect is upstream of it — in the object model, the apply order, the
version contract — and that is what you fix. "The gate is in the way" is never the
finding; it is the symptom you were given to chase.

**Validation may be *moved* only when the move is strictly stronger.** Earlier, atomic,
and over the complete unit that is actually meaningful beats late, partial, and
per-fragment. Moving a set-level check to *before* the first object is mutated is an
improvement; deleting a per-object check because the set-level one "probably covers it"
is not. State the delta explicitly and show it is positive — if you cannot, the answer
is no.

**Cost is not an argument.** A slower, uglier, larger design that keeps the guarantee
beats an elegant one that loses it. Extra CRDs, extra hooks, extra CI minutes and a
bigger diff are all cheaper than one operator discovering at 03:00 that the gate they
relied on was quietly narrowed.

Provenance: on 2026-07-28 a pre-split → post-split `helm upgrade` failed because the
running (old) webhook validated the new per-library config objects standalone. The
first proposal was an `objectSelector` excluding those objects from admission forever —
trading a permanent guarantee for one command's exit code. Rejected: the object model
was wrong, and that is what got fixed.

## Comments (RULE #3)

**The default is no comment.** A comment is a liability: it rots and it is re-read on
every visit.

**Size is not the argument, in either direction.** Keep comments short because a long
one is a reading tax and rots faster — never to make a byte gate pass, and never
because a gate has room. A comment that earns its place is worth its bytes; one that
does not is dead weight at any size, and deleting it is the fix whether or not
anything is measuring. If size is genuinely the concern, read the gate's output
(`cr-size-check` per object, `chart-size-check` for the Helm release Secret, both in
`make lint-chart`) and act on what it actually names. Prose is never the lever that
rescues an overrun: an object close enough to a ceiling that comments decide it is
one structural change behind schedule, and shortening comments to buy room only
hides that a release earlier.

Do not reason about comment cost from whatever the chart currently strips before
rendering. That has changed more than once, it differs by section, and a rule that
depends on it is wrong the day it changes. `haptic.filterTests` drops validationTest
`description` fields; nothing else about the pipeline should enter a decision about
what to write.

**Delete the comment, fix the code.** Each of these has a refactor, not a comment:

| Smell | Instead |
|---|---|
| Restates the code | Delete it |
| Carries a unit or meaning | Rename the identifier (`widthInPixels`, not `width // pixels`) |
| Labels a block | Extract a function / snippet |
| Explains a sub-expression | Introduce a named variable or constant |
| Asserts a precondition | Write the check |
| Explains bad code | Rewrite the code (Kernighan & Pike, rule 22) |
| Commented-out code, history, attribution | Delete it — git has it |
| Banner boxes, ASCII decoration | One plain line |

**Keep only what the code cannot say** — Ousterhout's test: it must be *not obvious
from the code*, and at a different level of detail than the code.

- why **not** the obvious alternative; a guard against a well-meaning "fix"
- a non-local invariant or ordering dependency
- units, ownership, what nil means, sentinel values, boundary conditions
- surprising external behaviour, or a workaround — **with a reference**

**Length is capped by intent, not by taste:**

- One line is the default. Two is a lot. **A paragraph means it belongs elsewhere.**
- Long rationale goes to an ADR, a docs page, or the PR description; the code keeps
  **one line and a pointer**. Document once, reference it — never restate it.
- Applies equally to `validationTests` `description:` fields and any prose inside a
  chart template — those DO ship in the CR.

If you catch yourself explaining a hard-won debugging insight inline, that is the
signal to write it in the PR or an ADR and leave one line behind.

Sources: [Google TotT — To Comment or Not to Comment?](https://testing.googleblog.com/2017/07/code-health-to-comment-or-not-to-comment.html),
[Google C++ style](https://google.github.io/styleguide/cppguide.html#Comments),
Kernighan & Pike *The Practice of Programming* ch.1,
[Ousterhout on comments](https://web.stanford.edu/~ouster/cgi-bin/cs190-winter18/lecture.php?topic=comments),
[Linux kernel ch.8](https://www.kernel.org/doc/html/latest/process/coding-style.html),
[go.dev/doc/comment](https://go.dev/doc/comment).

## No Lua features (RULE #4)

**Never implement or propose Lua-based HAPTIC features or workarounds.** Lua's
high-load performance costs are incompatible with this project's architecture.
This rule applies to all agents and subagents, including prototypes, optional
features, per-thread scripts, generated or embedded Lua, runtime actions, services,
fetches, converters, and HAProxy patches intended to enable a Lua workaround.

Use native HAProxy capabilities. If they cannot satisfy a requirement, report the
capability gap and investigate a native solution. Passing tests or benchmarks,
release pressure, and upstream limitations do not authorize an exception. Carry
this constraint into delegated tasks; never weaken validation to hide the gap.

`make lint` checks for Lua scripts and runtime hooks. Its three existing upstream
mailers fixtures are pinned by content hash solely to preserve compatibility
coverage; they are not permission to build a feature on Lua. Generic upstream
schemas and operator-file transport remain generic. Do not expand exemptions or
disable the check to introduce Lua.

## Gateway API feature coverage

**Implement all Gateway API features that HAProxy configuration alone can
support.** This is standing user authorization, including Core and Extended
features. Do not ask again whether to implement a feature merely because it is
optional, lacks a production report, or adds native configuration complexity.
Assess and qualify its performance, security, and lifecycle costs as part of the
implementation. Resource-agnostic Go, chart library boundaries, validation, and
the no-Lua rule still apply.

If HAProxy configuration cannot implement a feature and a SPOA hub plugin is
needed, present the capability gap and proposed plugin before building it. The
user decides those cases individually; this rule does not authorize plugins.

## Complexity Must Pay for Itself (RULE #5)

**Judge the total cost of a change against the problem actually observed, not each
step's local correctness.** "It's correct" does not mean "it's worth it." "Do the proper
fix" means proportionate, not exhaustive.

Run this gate before writing code for any fix or feature that adds a watch, API call,
permission, background loop, fallback path, config knob, or dependency on another
component. Run it again each time the same fix grows; judge the cumulative diff.
Write the answers in the plan or PR as numbers or yes/no.

For [native Gateway API features](#gateway-api-feature-coverage), the user has
already decided to implement them. Record and address the costs without asking
for that feature decision again. Plugin proposals still require a case-by-case
decision.

| # | Question | Questionable when |
|---|---|---|
| 1 | How often, and with what impact, has the problem been seen outside a test that forces it? | Only test-forced or theoretical |
| 2 | Has the user already accepted this risk (issue, ADR, CLAUDE.md)? | Yes, and not reopened |
| 3 | Which common deployment paths does it fix, and which does it miss? | Misses the common ones |
| 4 | Do comparable projects do this? If none does, why not? | None does, and no reason is known |
| 5 | New permissions, credentials, egress, or attack surface? | Any on a data-plane pod |
| 6 | New failure, fallback, or degraded modes? | Two or more |
| 7 | New config knobs? | Any that exists only to tune or disable this |
| 8 | New coupling to other components, kinds, or external systems? | Any outside HAPTIC's ownership |
| 9 | Cost at fleet scale (API calls, watches, memory, CPU), back-of-envelope? | Grows per data-plane pod, or with nodes |
| 10 | Do upgrade, rollback, and disable stay clean? | No, or untested |
| 11 | Does a smaller fix, a documented limitation, or no change suffice? | Yes |

Questions 1–4 and 11 weigh the benefit; 5–10 weigh the cost. **If a benefit answer and
a cost answer are both questionable, stop before building.** Present the user with the
observed problem, the cost answers, the cheaper options (including documenting the
limitation), and your recommendation. Building first and asking afterwards is not
asking. Otherwise record the answers and proceed: the gate is not a reason to hand
ordinary code decisions back to the user.

**This does not relax RULE #2.** Declining to add mechanism for a rare, accepted risk
does not weaken validation. Loosening a check to make it pass still does. A test that
asserts more than the product guarantees may be aligned to the documented guarantee
only with the user's explicit consent, stated in the PR. Where a validation guarantee is
at stake, RULE #2 wins: cost is not an argument there.

Provenance: #224 accepted the ~45 ms connection-refusal window between HAProxy's soft
stop and kube-proxy deprogramming. #449 then added a routing-aware drain to close a
narrower case: EndpointSlice and Service watches, cluster-wide Node listing, polling
every node's kube-proxy healthz, a pod API token with RBAC and egress, and a dozen
fallback modes. It missed the common production paths, caused a rollback regression
(#451), and none of about 20 surveyed projects does anything similar. Each step was
judged locally correct; the total was never weighed. Removed on 2026-10-19.

Rationale and sources: [docs/agents/complexity-gate.md](docs/agents/complexity-gate.md).

## Coding Standards

### Naming

- **Name generic code for what it does, not who first uses it.** A file, function, type, or package that is generic — usable by more than one caller — must be named after its capability, never after its first consumer or use case. Naming `filters_governance.go` for the generic `resource()` / `jsonpathGet()` / `jsonpathSet()` helpers because governance happened to be the first caller is wrong; the capability name (`filters_jsonpath.go`) is correct. The same holds for functions (`buildGovernanceContext` for an otherwise-generic context builder), types, and packages. Test: if the name only makes sense once you know the first consumer, rename it. A consumer-specific name is right only for genuinely consumer-specific code.

### Comments and user-visible messages

**Short and to the point — this applies to code comments AND to every message a
human reads at runtime**: error strings, `fail()` messages, Kubernetes Events,
warnings, log lines, rendered config comments.

A user-visible message answers three things and stops:

1. **What is wrong**, naming the resource or setting.
2. **What it causes** — the observable symptom, not the internals.
3. **What to do** — one concrete fix.

Everything else belongs in the code comment or the docs, not in the message.
Background, mechanism, and rationale are for the reader who is debugging the
code; the operator reading an error is trying to fix their config.

```go
// Too long — mechanism, a parenthetical aside, and two alternative fixes.
"Ingress default/api declares a gRPC backend and selects WAF policy 'p', whose
requestBody.mode inspects the body. Coraza buffers a complete body to inspect it
and ships no protobuf body processor, so a gRPC stream can never satisfy it:
client-streaming and bidirectional calls will time out with 408 (unary calls keep
working, which hides it). Set requestBody.mode: none on the policy for this route
— metadata inspection still applies — or move the gRPC service to a route
without a body-inspecting policy."

// Right — what, effect, fix.
"Ingress default/api has a gRPC backend and WAF policy 'p', which buffers request
bodies. A gRPC stream never completes its body, so streaming calls fail with 408.
Set requestBody.mode: none on the policy."
```

Comments follow the same discipline: explain what is not obvious from the code —
the constraint, the measurement, the reason a simpler version is wrong — and cut
everything a reader can see for themselves. A comment longer than the code it
explains is usually restating it.

### Go Idioms

- Follow standard Go conventions (effective Go, Go proverbs)
- Use `gofmt` and `goimports` for formatting
- Run linters before commits: `make lint`
- Table-driven tests for multiple scenarios
- Early returns for error cases
- **NEVER add lint rules to the global ignore list** in `.golangci.yml` `excludes` - Use localized per-file exclusions in the `exclusions.rules` section instead
- **NEVER use //nolint directives** - Fix linting issues properly by refactoring code, not by suppressing warnings

### Error Handling

```go
// Wrap errors with context
if err != nil {
    return fmt.Errorf("failed to parse config: %w", err)
}

// Custom error types for different failure modes
type ValidationError struct {
    Field   string
    Message string
    Err     error
}

func (e *ValidationError) Error() string {
    return fmt.Sprintf("validation failed on %s: %s", e.Field, e.Message)
}

func (e *ValidationError) Unwrap() error {
    return e.Err
}
```

### Context Propagation

Always propagate context through the call chain:

```go
func ProcessResource(ctx context.Context, resource Resource) error {
    // Pass context to all calls
    result, err := fetchData(ctx, resource.ID)
    if err != nil {
        return err
    }

    // Use context for timeouts
    ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
    defer cancel()

    return deploy(ctx, result)
}
```

## Pre-commit and CI Requirements

**CRITICAL**: All code must pass linting and security checks before committing.

### Pre-commit Hooks

Pre-commit hooks run automatically via git hooks and enforce:

- **golangci-lint**: Code quality, style, and common mistakes
- **govulncheck**: Security vulnerability scanning

### Commit Requirements

- **NEVER use `git commit --no-verify`** to bypass pre-commit hooks
- **Fix all linting issues** reported by `make lint` before committing
- **Address security vulnerabilities** reported by `govulncheck`
- **Run `gofmt` and `goimports`** to fix formatting issues
- **Refactor code** to resolve complexity and maintainability issues

### Why This Matters

**CI pipeline will fail** if code contains:

- Linting violations (golangci-lint errors)
- Security vulnerabilities (govulncheck findings)
- Formatting issues (gofmt/goimports)
- Test failures

Bypassing pre-commit hooks with `--no-verify` only delays the problem until CI runs, wasting time and blocking merges.

### Fixing Common Issues

```bash
# Run linting and see all issues
make lint

# Fix formatting automatically
gofmt -w .
goimports -w .

# Check for security vulnerabilities
govulncheck ./...

# Run all checks locally before committing
make check-all
```

**If extensive refactoring is needed**: Create separate commits or PRs for linting fixes rather than bypassing checks.

### CI and merging

Follow [CI and merge verification](docs/agents/ci.md) before pushing, starting or
retrying CI, or merging. Batch local fixes before pushing; every pull request runs
the complete matrix. Merge only once the required `verified` check passes.

After implementing and verifying a change, open the pull request without asking,
subscribe to its activity, and drive it to a mergeable state: fix CI failures and
address review findings, Gitar's included. Once `verified` and the review pass,
merge it. See `.claude/skills/steward/SKILL.md`.

### GitHub CLI (gh) Commands

Issues and pull requests live in `haproxy-haptic/HAPTIC`; see
[issue tracking](docs/agents/issue-tracker.md). Historical GitLab issue and MR URLs
stay valid as references.

```bash
# Create a pull request
gh pr create --repo haproxy-haptic/HAPTIC --title "Fix template rendering" --body-file body.md

# View a pull request, its conversation, and its checks
gh pr view 123 --repo haproxy-haptic/HAPTIC --comments
gh pr checks 123 --repo haproxy-haptic/HAPTIC

# List workflow runs and view a failed job's log
gh run list --repo haproxy-haptic/HAPTIC --branch "$branch"
gh run view "$run_id" --repo haproxy-haptic/HAPTIC --log-failed

# Download a run's artifacts for debugging CI failures
gh run download "$run_id" --repo haproxy-haptic/HAPTIC --dir artifacts

# Create an issue
gh issue create --repo haproxy-haptic/HAPTIC --title "Bug: ..." --body-file body.md
```

## Event-Driven Architecture Principles

### Pure Components Pattern

Business logic should be pure (no event dependencies):

```go
// pkg/templating/engine_interface.go - Pure component
type Engine interface {
    Render(ctx context.Context, templateName string, templateContext map[string]any) (string, error)
}
```

### Event Adapter Pattern

Only controller package contains event adapters:

```go
// Illustrative event-adapter shape — not a real package.
// The production renderer is the synchronous `renderer.RenderService`
// driven by `pkg/controller/pipeline.Pipeline` (no event hop, see ADR-0001).
// For the production event-adapter scaffold, see `pkg/controller/component.Base`.
type RendererComponent struct {
    engine    templating.Engine     // Pure component
    eventBus  *events.EventBus
    eventChan <-chan events.Event   // Subscribed in constructor
}

func NewRendererComponent(bus *events.EventBus, engine templating.Engine) *RendererComponent {
    return &RendererComponent{
        engine:    engine,
        eventBus:  bus,
        eventChan: bus.Subscribe("renderer", 100),  // Subscribe in constructor, before Start()
    }
}

// Method name is Start (not Run) — matches the lifecycle.Component contract.
func (r *RendererComponent) Start(ctx context.Context) error {
    for {
        select {
        case event := <-r.eventChan:
            if req, ok := event.(ReconciliationTriggeredEvent); ok {
                // Call pure component
                output, err := r.engine.Render(ctx, "haproxy.cfg", req.Context)

                // Publish result event
                if err != nil {
                    r.eventBus.Publish(RenderFailedEvent{Error: err.Error()})
                } else {
                    r.eventBus.Publish(RenderCompletedEvent{Output: output})
                }
            }
        case <-ctx.Done():
            return ctx.Err()
        }
    }
}
```

### When to Use Events vs Direct Calls

**Use EventBus (async pub/sub):**

- Component coordination across packages
- Observability and logging
- Extensibility (new features can subscribe to existing events)
- Fire-and-forget notifications

**Use Request() (scatter-gather):**

- Configuration validation (multiple validators must respond)
- Distributed queries
- Coordinated operations requiring multiple confirmations

**Use direct function calls:**

- Within the same package
- Pure components calling other pure components
- Utility components (see below)
- No need for decoupling or observability

### Utility Components vs Pure Components

Not all dependencies require event-driven coordination. The codebase distinguishes between:

**Pure Components** (require event adapters):

- Contain domain business logic
- Examples: `pkg/templating`, `pkg/k8s`, `pkg/dataplane`
- Should be wrapped in event adapters when used in `pkg/controller`
- Changes to these affect reconciliation logic

**Utility Components** (can be called directly):

- Infrastructure/cross-cutting concerns
- Examples: EventBus, StoreProvider, Metrics, RestMapper
- Provide services used by multiple components
- No domain-specific business logic
- Can be injected and called directly without events

#### Examples of Direct Utility Calls

```go
// Good - direct utility component calls (illustrative pseudocode)
func (c *SomeComponent) handleAdmission(resourceType string, obj runtime.Object) {
    // StoreProvider is a utility component - building a dry-run overlay is a direct call
    overlay := stores.NewStoreOverlayForCreate(obj)
    provider := stores.NewOverlayStoreProvider(c.baseProvider,
        stores.NewValidationContext(map[string]*stores.StoreOverlay{resourceType: overlay}))

    // Metrics is a utility component - direct call
    if c.metrics != nil {
        c.metrics.RecordValidation(true)
    }

    // RestMapper is a utility component - direct call
    gvk, err := c.restMapper.KindFor(gvr)
}

// Bad - calling pure component without event adapter
func (c *SomeComponent) Run(ctx context.Context) error {
    // This should go through an event adapter, not called directly
    config, err := c.templateEngine.Render("haproxy.cfg", context)
    return err
}
```

#### Decision Tree: Events vs Direct Calls

```
Does the call involve domain business logic?
├─ YES → Use event-driven pattern
│   └─ Examples: template rendering, config validation, HAProxy sync
│
└─ NO → Is it infrastructure/utility?
    ├─ YES → Direct call is acceptable
    │   └─ Examples: EventBus.Publish(), StoreProvider.GetStore(), Metrics.Record()
    │
    └─ MAYBE → Review with team
        └─ Ask: "Could this become reusable business logic?"
```

#### Utility Components Registry

Current utility components that can be called directly:

- **EventBus** (`pkg/events`): Event infrastructure
- **StoreProvider** (`pkg/stores`): Resource store provider + dry-run overlay (`OverlayStoreProvider`, `StoreOverlay`); the watcher-side store lives in `pkg/k8s/store`
- **Metrics** (`pkg/controller/metrics`): Prometheus metrics recording
- **RestMapper** (`k8s.io/apimachinery/pkg/api/meta`): Kubernetes API mapping
- **Logger** (`log/slog`): Structured logging

When adding new components, explicitly document if they are "pure" or "utility" in their CLAUDE.md file.

## Import Path Conventions

### Internal Organization

```go
// Core packages (minimal dependencies)
import "github.com/haproxy-haptic/HAPTIC/pkg/core/config"
import "github.com/haproxy-haptic/HAPTIC/pkg/core/logging"

// Infrastructure (no domain knowledge)
import "github.com/haproxy-haptic/HAPTIC/pkg/events"

// Domain packages (depends on core + infrastructure)
import "github.com/haproxy-haptic/HAPTIC/pkg/templating"
import "github.com/haproxy-haptic/HAPTIC/pkg/k8s"
import "github.com/haproxy-haptic/HAPTIC/pkg/dataplane"

// Coordination (depends on everything)
import "github.com/haproxy-haptic/HAPTIC/pkg/controller"
import "github.com/haproxy-haptic/HAPTIC/pkg/controller/events"  // Event type catalog
```

### Dependency Rules

- `pkg/events` should have no dependencies on other pkg/ packages
- `pkg/templating`, `pkg/k8s`, `pkg/dataplane` should be pure libraries (no cross-dependencies)
- `pkg/controller` can import everything (coordination layer)
- `pkg/core` provides shared primitives (config types, logging setup)
- Domain-specific event types go in `pkg/controller/events`, not `pkg/events`

## Testing Strategy

### Unit Tests

Test pure components without event infrastructure:

```go
func TestEngine_Render(t *testing.T) {
    tests := []struct {
        name     string
        template string
        context  map[string]interface{}
        want     string
        wantErr  bool
    }{
        {
            name:     "simple variable",
            template: "Hello {{ name }}",
            context:  map[string]interface{}{"name": "World"},
            want:     "Hello World",
        },
        // More test cases...
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            engine, err := templating.New(
                map[string]string{"test": tt.template}, nil)
            require.NoError(t, err)

            got, err := engine.Render(context.Background(), "test", tt.context)
            if tt.wantErr {
                require.Error(t, err)
                return
            }

            require.NoError(t, err)
            assert.Equal(t, tt.want, got)
        })
    }
}
```

### Integration Tests

Located in `tests/` directory. Require kind cluster:

```bash
# Run integration tests
make test-integration

# Run specific integration test
KEEP_CLUSTER=true go test -tags=integration ./tests/integration -run TestSyncFrontends -v
```

Tests use real Kubernetes clusters (kind) and HAProxy pods. The `KEEP_CLUSTER=true` environment variable prevents cluster cleanup for debugging.

### Event-Driven Component Tests

Test event adapters with mock EventBus. The example below uses the
hypothetical `RendererComponent` from the Event Adapter Pattern section
above; the real renderer is a synchronous `RenderService` (ADR-0001) and
isn't tested via this shape. For an actual event-adapter test, see
`pkg/controller/configloader/loader_test.go`.

```go
func TestRendererComponent(t *testing.T) {
    bus := events.NewEventBus(100)
    engine, _ := templating.New(templates, nil)
    renderer := NewRendererComponent(bus, engine)

    // Subscribe to output events
    eventChan := bus.Subscribe("test", 10)
    bus.Start()

    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    // Start component (method is Start, matching the lifecycle.Component contract)
    go renderer.Start(ctx)

    // NOTE: RendererComponent / RenderCompletedEvent / RenderFailedEvent are
    // illustrative placeholders. In production, rendering is synchronous
    // (renderer.RenderService, ADR-0001) and its real result event is
    // *events.TemplateRenderedEvent — there is no RenderCompletedEvent type.
    // Trigger reconciliation
    bus.Publish(ReconciliationTriggeredEvent{Context: testContext})

    // Verify output event
    select {
    case event := <-eventChan:
        if completed, ok := event.(RenderCompletedEvent); ok {
            assert.Contains(t, completed.Output, "expected content")
        } else {
            t.Fatalf("expected RenderCompletedEvent, got %T", event)
        }
    case <-time.After(1 * time.Second):
        t.Fatal("timeout waiting for event")
    }
}
```

## Build Commands

```bash
# Build binary
make build

# Run all tests
make test

# Run linting (golangci-lint)
make lint

# Run all checks (tests + linting)
make check-all

# Integration tests (requires kind)
make test-integration

# Coverage report
make test-coverage

# Build Docker image
make docker-build

# Test template libraries (Helm charts)
# IMPORTANT: Use this script when testing template changes to ensure
# proper helm rendering. Do NOT test library files directly.
./scripts/test-templates.sh

# Test specific template validation test
./scripts/test-templates.sh --test test-httproute-method-matching

# Test with debug output
./scripts/test-templates.sh --test test-httproute-method-matching --dump-rendered --verbose
```

## Development Environment

### Local Kind Cluster

**IMPORTANT**: Always use the `kind-haptic-dev` context for development work.

```bash
# Verify you're using the correct cluster
kubectl config current-context
# Should output: kind-haptic-dev

# If not, switch to it
kubectl config use-context kind-haptic-dev

# Start the dev environment (creates cluster if needed)
./scripts/start-dev-env.sh

# Build and deploy changes to dev cluster
# IMPORTANT: Always use this script - do not run manual build commands
./scripts/start-dev-env.sh restart

# View controller logs
./scripts/start-dev-env.sh logs

# Check deployment status
./scripts/start-dev-env.sh status

# Test ingress functionality
./scripts/start-dev-env.sh test

# Check HAProxy configuration
kubectl -n echo get pods -l app=haproxy
kubectl -n echo exec <haproxy-pod> -- cat /etc/haproxy/haproxy.cfg

# Clean up dev environment
./scripts/start-dev-env.sh down
```

**Cluster Names:**

- **Dev cluster**: `kind-haptic-dev` - Use this for development
- **Test cluster**: `kind-haproxy-test` - Used by integration tests only

### Verifying Dev Environment Code

The dev environment uses source file hashing to verify the running code matches local files:

```bash
# Check if dev environment is running current code
./scripts/start-dev-env.sh status

# Output shows IN SYNC or OUT OF SYNC
# Source Code Sync:
#   Local source hash:   a1b2c3d4e5f6
#   Running source hash: a1b2c3d4e5f6
# ✔ IN SYNC - dev environment is running current code
```

The source hash covers all `.go` files in `pkg/` and `cmd/`, `go.mod`, `go.sum`, and the controller's PGO profile. It changes whenever one of those inputs changes, committed or not.

**Always run `status` before debugging** to confirm you're testing the right code. If OUT OF SYNC, run `./scripts/start-dev-env.sh restart`.

### HAProxy Version Management

**Build-time** (what CI builds):

- `versions.env` defines supported versions (`HAPROXY_VERSIONS="3.0 3.2 3.3 3.4"`)
- `Dockerfile` uses `DEFAULT_HAPROXY` for local builds
- CI builds one controller image per version: `<version>-haproxy3.0` … `<version>-haproxy3.4` (for example `0.2.0-alpha.1-haproxy3.4`)

**Deploy-time** (what the chart uses):

- `haproxyVersion` in `values.yaml` selects the version; its schema enum lists the series CI builds, and `make lint` checks it against `versions.env`
- This single value drives both:
    - Controller image tag: `ghcr.io/haproxy-haptic/haptic:0.2.0-alpha.1-haproxy3.4`
    - HAProxy image tag: `haproxytech/haproxy-debian:3.4`
- This guarantees the controller and HAProxy pod always use matching versions

When updating default versions, update both:

1. `versions.env` / `Dockerfile` - for build defaults
2. `charts/haptic/values.yaml` - for deploy defaults

## Common Patterns

### Graceful Shutdown

```go
func main() {
    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer stop()

    // Create components
    components := []Component{
        watcher,
        reconciler,
        executor,
    }

    // Start components with errgroup
    g, gCtx := errgroup.WithContext(ctx)
    for _, comp := range components {
        comp := comp  // Capture loop variable
        g.Go(func() error {
            return comp.Start(gCtx)
        })
    }

    // Wait for shutdown signal or error
    <-ctx.Done()
    slog.Info("Shutdown signal received, stopping components...")

    // Wait for components to finish (with timeout)
    done := make(chan error)
    go func() {
        done <- g.Wait()
    }()

    select {
    case err := <-done:
        if err != nil {
            slog.Error("Component error during shutdown", "error", err)
        }
    case <-time.After(30 * time.Second):
        slog.Error("Shutdown timeout exceeded")
    }
}
```

### Structured Logging

```go
import "log/slog"

// Create logger with structured fields
logger := slog.Default().With(
    "component", "reconciler",
    "namespace", resource.Namespace,
)

// Log with structured attributes
logger.Info("reconciliation started",
    "resource", resource.Name,
    "trigger", "config_change",
)

logger.Error("reconciliation failed",
    "error", err,
    "duration_ms", time.Since(start).Milliseconds(),
)
```

## Common Pitfalls

### Event Bus

- **Don't block in event handlers** - Process events quickly or spawn goroutines
- **Buffer sizing matters** - Small buffers (10-50) for control events, large buffers (200+) for high-volume events
- **Always call EventBus.Start()** after all components are created (prevents lost events during startup)
- **Subscribe in constructors, not in Start()** - ALL components must subscribe during construction (in `New()` functions) BEFORE `EventBus.Start()` is called. This ensures proper startup synchronization without race conditions or timing-based sleeps.
- **Never use timing-based solutions** - Do not use `time.Sleep()` or delays to fix event ordering issues. These are brittle, non-deterministic, and bad practice. Fix the root cause by ensuring proper subscription ordering.

### Proper Subscription Pattern

**CORRECT Pattern** (all components follow this):

```go
// Component constructor subscribes immediately
func New(eventBus *EventBus, ...) *Component {
    // Subscribe BEFORE returning the component.
    // Subscribe(name, bufferSize) — name is required for drop accounting.
    eventChan := eventBus.Subscribe(ComponentName, EventBufferSize)

    return &Component{
        eventBus:  eventBus,
        eventChan: eventChan,  // Store channel for use in Start()
        // ...
    }
}

// Start() uses pre-subscribed channel
func (c *Component) Start(ctx context.Context) error {
    for {
        select {
        case event := <-c.eventChan:  // Use stored channel
            c.handleEvent(event)
        case <-ctx.Done():
            return ctx.Err()
        }
    }
}

// Controller startup sequence
components := createAllComponents()  // All subscribe during construction
eventBus.Start()                      // Now safe to start bus
startAllComponents()                  // Start goroutines
```

**INCORRECT Pattern** (creates race conditions):

```go
// BAD - subscribes in Start()
func (c *Component) Start(ctx context.Context) error {
    eventChan := c.eventBus.Subscribe(ComponentName, EventBufferSize)  // TOO LATE!
    // May miss events published before this subscription
}
```

### Context

- **Always respect context cancellation** - Check `ctx.Done()` in loops
- **Don't ignore context timeout errors** - They indicate system overload
- **Pass context through the call chain** - Don't create new contexts except for timeouts

### Testing

- **Don't use real Kubernetes API in unit tests** - Use fake clients from `k8s.io/client-go/kubernetes/fake`
- **Never shell out to external binaries in unit tests** - Mock through a seam instead; for haproxy use `pkg/dataplane/dataplanetest.InstallFakeHAProxy` in the package's `TestMain` (see `pkg/dataplane/CLAUDE.md`). Real-binary verdicts belong in integration tests
- **Tests must only listen on loopback** - Bind `127.0.0.1`/`localhost:0`, never `:port`/`0.0.0.0` (the `httptest` package does this right by default). All-interface binds trigger a Windows Firewall whitelist prompt for every freshly built test binary
- **Integration tests are slow** - Keep them focused and minimal
- **Mock EventBus carefully** - Subscribe before publishing to avoid race conditions

### Kubernetes

- **Wait for initial sync** - Don't process resources before all informers sync
- **Handle resource versions** - They're not monotonic across resource types
- **Field selectors are limited** - Not all fields support field selectors (use label selectors instead)

### Development Practices

**CRITICAL - Task Completion Standards:**

- **NEVER mark a task as completed without verifying it actually works**
    - Run tests to confirm the implementation is correct
    - Test the actual behavior, not just that code compiles
    - Verify edge cases and error conditions

- **NEVER take shortcuts by skipping hard parts of implementation**
    - If a test fails, FIX the code to make it pass correctly
    - Do NOT change the test to match broken behavior
    - Do NOT add TODO comments for things that should be fixed immediately
    - If something is genuinely out of scope, discuss with the user first

- **ALWAYS inform the user about incomplete or buggy implementations**
    - If you discover a bug during implementation, fix it
    - If you can't complete a task, explain why and what remains
    - Do NOT hide problems behind TODO comments
    - Transparency builds trust

- **FIX ALL test failures, lint errors, and runtime warnings/errors immediately**
    - Do NOT speculate about whether your changes caused the issue
    - Do NOT dismiss issues as "pre-existing" or "unrelated"
    - If tests fail or logs show warnings/errors after your changes, fix them
    - The dev environment must work correctly with no errors before task completion
    - Run the full test suite and verify dev environment logs are clean

**Example of UNACCEPTABLE behavior:**

1. Test fails because routes are in wrong order
2. Change test to validate the wrong order
3. Add TODO comment saying "should fix this later"
4. Mark task as completed
5. Don't tell user about the bug

**Example of CORRECT behavior:**

1. Test fails because routes are in wrong order
2. Debug the sorting logic to find root cause
3. Fix the sorting implementation
4. Verify test passes with correct behavior
5. Mark task as completed

**Why this matters:** Taking shortcuts wastes time. The bug will eventually need to be fixed, and by then the context is lost, making it harder. Do it right the first time.

## Resources

- Architecture: `docs/site/docs/development/design.md`
- Package READMEs: `pkg/*/README.md`
- Linting guidelines: `docs/site/docs/development/linting.md`
- Configuration reference: `docs/site/docs/supported-configuration.md`

## User Documentation Authoring

Applies to **all** user-facing docs — the merged docs site (`docs/site/docs/`,
served at `/docs/`, covering both the controller and the Helm chart) and the
landing page (`docs/landing/`). The reader is often new to HAPTIC and takes every
sentence literally.

- **Answer the user's practical question first.** Give usable defaults, rough
  resource estimates, and complete examples before optional detail. Resource
  guidance must help an operator budget an installation without running our
  benchmarks. Label estimates and their assumptions; don't invent certainty.
- **Include detail only when it helps the reader act or decide.** Automatic
  implementation details such as `automemlimit` and `GOMEMLIMIT` don't belong in
  user documentation. Describe memory sizing through Kubernetes requests and
  limits. Keep benchmark methodology, raw measurements, and engineering evidence
  in maintainer records; moving them to another user-facing page doesn't make
  them useful. Preserve advanced reference material when it serves a concrete
  operator or template-author task.
- **Examine prerequisites vs. procedure for contradictions.** Cross-check every
  "Before you start" / "Prerequisites" item against the steps that follow. If a
  step performs it, it's not a prerequisite (e.g. don't list "HAPTIC is installed"
  when step 1 installs it).
- **Don't state a restriction unless it's actually true.** Prohibitions ("don't
  install it beforehand", "must be X", "only works with Y") must be technically
  necessary, not just the path you happened to describe. Ask *what actually breaks
  if the reader does the opposite?* If nothing breaks, describe the alternative
  path instead of forbidding it.
- **Everything must be copy-pastable.** Never tell the reader to hand-edit a
  command ("swap `install` for `upgrade`", "drop the `--create-namespace` flag").
  Give the complete runnable command for each case, even if two commands overlap.
- **Don't assume the reader's state.** They may have already created Ingresses
  with HAPTIC's class, installed HAPTIC, or set custom values. Don't write
  sentences that only hold under an assumed state; branch explicitly ("if X, do A;
  otherwise B") instead.
- **Never add sentences that carry no value.** Every sentence must instruct, warn,
  or inform. Cut reassurances and filler ("anyway", "as you'd expect", "simply") —
  they don't help and can confuse readers who read them as load-bearing.
- **No technically-true-but-vacuous phrasing.** A clause that sounds informative
  but adds nothing makes readers parse the sentence twice looking for the meaning
  that isn't there. Watch for: tautological qualifiers ("write any HAProxy
  directive *using the configuration language*" — what else would you write it
  in?), circular statements ("the validator validates the config"), self-evident
  claims presented as features, dangling qualifiers with no criteria ("where
  applicable", "as needed" — say *when*), and sentences whose real point hides
  behind a vacuous main clause ("deploy with confidence knowing X is validated" —
  the point is the validation, lead with it). Test: if negating the clause
  produces nonsense rather than a real alternative, the clause says nothing — cut
  it or replace it with the concrete fact it was gesturing at. The same test
  applies to *advantage* claims: if every alternative's docs could truthfully
  say the same sentence ("deploy config changes in minutes", "fast",
  "flexible"), it differentiates nothing — cut it or replace it with the
  HAPTIC-specific mechanism. Watch for bullets whose intended point is already
  the previous bullet.

### Distilled style rules (Google/Microsoft/Diátaxis)

- **Second person, active voice, present tense.** "You configure the class" /
  "The controller writes status" — not "the class is configured" or "will write".
- **Don't mix doc types on one page** (Diátaxis). A how-to gives steps; a
  reference lists facts; an explanation gives background. Keep them separate — a
  tutorial that also explains theory serves no one.
- **One action per step.** Numbered steps are imperative and single-purpose
  ("Run X.", "Set Y."). Put results/notes in a separate sentence, not the step.
- **Front-load.** Lead each section/paragraph with what it's about; put the most
  important information first. Descriptive headings, sentence case.
- **Descriptive link text** — link the thing ("[Getting Started](...)"), never
  "click [here]" or a bare URL.
- **Define an acronym on first use**, then reuse it. Don't assume jargon.
- **Contractions are fine and preferred** (`isn't`, `don't`) for a warm, clear
  tone — but stay consistent (don't mix `isn't` and "is not" for the same kind of
  statement). Prefer negative contractions so readers don't miss a "not".
- **Write for a global audience.** Short sentences, common words, no idioms/slang;
  the serial (Oxford) comma; spell out ambiguous dates (2026-07-01, not 07/01).
- **Show, then tell.** Prefer a runnable example over a paragraph describing it.

### Interactive playground examples

The docs embed a live playground (`docs/shared/playground-embed.js`, the `.pg-embed`
component). When you author or edit a runnable example:

- **Default to typed resource access, not `dig()`.** Every non-scriggo embed loads
  the schema bundle, so a real cluster's typed access works: write
  `svc.metadata.name`, `ingress.spec.rules`, `path.backend.service.port.number` —
  not `dig(svc, "metadata", "name")`. Typed access is the idiom the controller
  gives you against a live apiserver, so it's what examples should teach. Reserve
  `dig()` for the three cases where it's genuinely right: the `dig`/`fallback`
  lesson itself, inline `map[string]any` literals (no watched resource), and a
  schema-less custom resource with no bundled schema. (`dig()` is *required* in the
  chart libraries — that's RULE #1, resource-agnostic code — but that's the chart,
  not user templates. Don't let chart `dig()` usage set the default for examples.)
  Typed gotchas: range optional typed slices **directly** (`range ingress.spec.rules`)
  — wrapping them in `fallback(x, []any{})` boxes elements to `interface{}` and
  breaks the following typed field access; a nil typed slice ranges zero times.
  Check presence with `len(x.paths) > 0` (a value-struct field can't compare to nil).
  Show a `map` with `... | toJSON()`.
- **Use the minimal `data-scriggo` embed for template-language demos.** When the
  point is a helper function, a control-flow construct, or whitespace control —
  anything that doesn't need watched resources — use `<div class="pg-embed"
  data-scriggo>` with a bare template. It skips the whole `HAProxyTemplateConfig`
  wrapper, so the reader sees only the template and its output. Reserve full-config
  embeds (with `watchedResources`/`haproxyConfig`) for examples that genuinely
  render resources into config.
- **Teach before you test.** Put a challenge (`data-difficulty`, a `pg-solution`)
  *after* the concept it exercises, never before — a reader can't solve a task for
  something the page hasn't shown yet. Every task/challenge intro is one
  `<p class="pg-task">` callout (the component styles it); don't leave it as plain
  prose.

Full guides: [Google](https://developers.google.com/style),
[Microsoft](https://learn.microsoft.com/en-us/style-guide/welcome/),
[Diátaxis](https://diataxis.fr/).

## Changelog Guidelines

There is ONE changelog: the root `CHANGELOG.md`. It documents user-facing changes to the controller and the Helm chart. Chart changes (values, templates, chart defaults, values migrations) go under a `### Helm chart` subsection of each release (with `#### Added`/`#### Changed`/… sub-headings); controller changes go in the release's top-level `### Added`/`### Changed`/… sections. `charts/haptic/CHANGELOG.md` is only a pointer to the root changelog.

**Every notable change must have a changelog entry.** Keep entries concise - one line per change, focus on what changed, not implementation details. Avoid verbose justifications or explanations in parentheses.

**Include:**

- New features and capabilities (what the controller can do)
- User-facing commands and APIs
- Behavior changes and bug fixes
- Metrics and observability features
- Security-related changes
- Helm chart values, defaults, and template library changes (in the `### Helm chart` subsection)

**Exclude (not notable):**

- Test additions or fixes
- Lint fixes
- CI/CD pipeline fixes
- Development scripts and tooling
- Internal testing infrastructure

**Don't call changes "BREAKING" when the feature being broken was itself introduced after the last release.** The CHANGELOG is read by operators upgrading between released versions; if the affected behavior never shipped to a real release, the only people impacted are snapshot/main consumers — note the change but don't tag it as a `BREAKING` migration. Check `git tag -l | sort -V | tail` for the latest released version and `git log <last-tag>..HEAD -- <files>` to see what's actually post-release.

## Release Process

The controller and the Helm chart are released together: one version, one `v<version>` tag, one changelog. `VERSION` is the single source of truth; `charts/haptic/Chart.yaml` `version` and `appVersion` must equal it (CI refuses to tag otherwise).

To prepare a release:

1. **Curate `CHANGELOG.md` [Unreleased]** - Rewrite the accumulated entries into user-facing release notes (merge related entries, drop items that never shipped in a release); also update the hand-curated `artifacthub.io/changes` annotation in `charts/haptic/Chart.yaml`
2. **Run `./scripts/release.sh <version>`** - Promotes `[Unreleased]` to the version section and updates every version-bearing file (VERSION, Chart.yaml, install examples) in one commit; the docs-site changelog page is generated from `CHANGELOG.md` at mkdocs build time and needs no release-time sync
3. **Merge the release PR to main** - after the pull request and post-merge CI runs pass, `release-tag.yml` creates the `v<version>` tag, and the release workflow publishes binaries, images, the chart, and versioned docs

See `docs/site/docs/development/releasing.md` for the full process.

## Package-Specific Context

For detailed development context on specific packages, see:

- `pkg/CLAUDE.md` - Package organization principles
- `pkg/events/CLAUDE.md` - Event bus infrastructure
- `pkg/controller/CLAUDE.md` - Controller orchestration
- `pkg/controller/leaderelection/CLAUDE.md` - Leader election event adapter
- `pkg/controller/metrics/CLAUDE.md` - Metrics collection
- `pkg/k8s/CLAUDE.md` - Kubernetes integration
- `pkg/k8s/leaderelection/CLAUDE.md` - Pure leader election component
- `pkg/dataplane/CLAUDE.md` - HAProxy integration
- `pkg/templating/CLAUDE.md` - Template engine
- `pkg/core/CLAUDE.md` - Core functionality
- `pkg/httpstore/CLAUDE.md` - Pure HTTP resource store
- `pkg/introspection/CLAUDE.md` - Debug HTTP server infrastructure (`/debug/vars`)
- `pkg/lifecycle/CLAUDE.md` - Component lifecycle registry and leader gating
- `pkg/metrics/CLAUDE.md` - Prometheus registry and server primitives
- `pkg/webhook/CLAUDE.md` - Pure admission-webhook HTTPS server
- `cmd/haptic/CLAUDE.md` - Entry point and startup
- `charts/CLAUDE.md` - Helm chart and template libraries
- `tests/CLAUDE.md` - Test suites (integration, e2e, acceptance)

## Agent skills

### Issue tracker

Issues live as GitHub issues in `haproxy-haptic/HAPTIC`. Use the `gh` CLI. See
`docs/agents/issue-tracker.md`.

### Triage labels

The five canonical triage roles use their default label strings (`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`). See `docs/agents/triage-labels.md`.

### Domain docs

Single-context layout: one `CONTEXT.md` and `docs/adr/` at the repo root, created lazily by `/grill-with-docs`. See `docs/agents/domain.md`.
