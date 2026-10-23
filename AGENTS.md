# HAPTIC agent instructions

Read [CLAUDE.md](CLAUDE.md) before changing this repository, and read the applicable
package's `CLAUDE.md` before editing that package. These instructions apply to all
agents and subagents.

Read [CI spending and merge verification](docs/agents/ci.md) before pushing,
starting or retrying CI, or merging. CI savings never authorize an unverified merge.

**Never add Lua-based HAPTIC features or workarounds.** This includes prototypes,
optional features, and per-thread scripts. Follow [No Lua features](CLAUDE.md#no-lua-features-rule-4);
passing tests or benchmarks does not authorize an exception. Carry this constraint
into every delegated task.

**Implement Gateway API features achievable through HAProxy configuration alone.**
This is [standing authorization](CLAUDE.md#gateway-api-feature-coverage); assess
and qualify the implementation without asking again whether to build it. Features
requiring a SPOA hub plugin need a case-by-case user decision. Carry this policy
into every delegated task.

**Weigh total cost before adding mechanism.** Apply the [complexity gate](CLAUDE.md#complexity-must-pay-for-itself-rule-5)
before building. Outside the standing native Gateway API authorization, present
questionable cost and benefit to the user before building.
