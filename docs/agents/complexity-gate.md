# Complexity gate

Background for [RULE #5 in CLAUDE.md](../../CLAUDE.md#complexity-must-pay-for-itself-rule-5).
The rule holds the gate itself; this page explains where each question comes from, scores
the case that prompted it, and gives the shape of the note you send the user when the gate
says "questionable".

The user has given [standing authorization for Gateway API features implemented
through HAProxy configuration alone](../../CLAUDE.md#gateway-api-feature-coverage).
Assess and address their costs without asking again whether to implement them.
Features requiring a SPOA hub plugin still need a case-by-case user decision.

## The failure mode

An agent asked for "the proper fix" closes the first gap, sees a smaller remaining gap,
and adds mechanism to close that too. Every step is locally correct and well tested.
Nobody adds up the total: what the change now depends on, what it can fail in, what it
grants, and how often the problem it solves actually happens. Each review looks at one
step, so the sum is never reviewed.

The gate breaks that loop in two ways. It is answered for the cumulative change, not the
latest step. And a questionable benefit paired with a questionable cost moves the decision to the
user before code exists, when declining is free.

## Where each question comes from

| Question | Source and what it contributes |
|---|---|
| 1. Observed frequency and impact | Linux kernel [submitting-patches](https://www.kernel.org/doc/html/latest/process/submitting-patches.html): "Describe your problem", "Describe user-visible impact", "Quantify optimizations and trade-offs". Google [eng-practices](https://google.github.io/eng-practices/review/reviewer/looking-for.html): solve "the problem they know needs to be solved now, not the problem that the developer speculates *might* need to be solved". |
| 2. Risk already accepted | Google SRE, [Embracing Risk](https://sre.google/sre-book/embracing-risk/): "100% is probably never the right reliability target"; each increment "may cost 100x more than the previous increment". A risk the user accepted is inside the budget; spending engineering on it needs the user to reopen it. |
| 3. Coverage of common paths | Fowler, [Yagni](https://martinfowler.com/bliki/Yagni.html): cost of build and cost of repair. A fix for a rare path that misses the common ones carries full cost for partial benefit. |
| 4. Prior art | Kubernetes [KEP template](https://github.com/kubernetes/enhancements/blob/master/keps/NNNN-kep-template/README.md): "What other approaches did you consider, and why did you rule them out?" Dan McKinley, [Choose Boring Technology](https://mcfunley.com/choose-boring-technology): novelty spends a scarce "innovation token". When no comparable project does something, the likely reason is cost, not oversight. Find the reason before assuming you are first. |
| 5. Permissions and attack surface | KEP template PRR: "Will enabling / using this feature result in any new API calls?" Every grant on an internet-facing pod widens what a compromise reaches. |
| 6. Failure and fallback modes | KEP template PRR: "What are other known failure modes?" Ousterhout, [A Philosophy of Software Design](https://web.stanford.edu/~ouster/cgi-bin/aposd.php) ch. 2: complexity is dependencies plus obscurity; each fallback mode is an obscure state an operator must learn to read. |
| 7. Config knobs | Ousterhout ch. 4 (deep vs. shallow modules) and ch. 6: a knob pushes a decision the module should make onto every operator. TigerStyle, [TIGER_STYLE.md](https://github.com/tigerbeetle/tigerbeetle/blob/main/docs/TIGER_STYLE.md): "Simplicity is not a free pass", it is "the hardest revision". |
| 8. Coupling | Rich Hickey, [Simple Made Easy](https://www.infoq.com/presentations/Simple-Made-Easy/): simple means not interleaved with other concerns; easy means near at hand. Reading another component's internal state is easy and complects HAPTIC with it. KEP PRR: "Does this feature depend on any specific services running in the cluster?" |
| 9. Cost at scale | TigerStyle: "Perform back-of-the-envelope sketches with respect to the four resources (network, disk, memory, CPU)". McKinley: "the long-term costs of keeping a system working reliably vastly exceed any inconveniences you encounter while building it." Fowler: cost of carry. |
| 10. Reversibility | KEP PRR: "Can the feature be disabled once it has been enabled (i.e. can we roll back the enablement)?" Kernel [regression handling](https://www.kernel.org/doc/html/latest/process/handling-regressions.html): a change that breaks a working setup is a regression, whatever it fixes. |
| 11. Smaller option | KEP template: "Why should this KEP *not* be implemented?" Rule of three (Fowler, *Refactoring*, after Don Roberts): generalise only once the third real case shows its shape. Reverse Chesterton's fence ([Chesterton, *The Thing*, 1929](https://en.wikipedia.org/wiki/G._K._Chesterton#Chesterton's_Fence)): before putting up a fence, know what it keeps out and how often. |

Chesterton's fence also cuts the other way. Removing existing mechanism needs its reason
first. The gate governs adding; it does not license deleting code whose purpose you have
not found.

## The trigger case, scored

PR #449 (routing-aware termination drain), cumulative diff:

| # | Answer | Verdict |
|---|---|---|
| 1 | Seen only with a forced kube-proxy freeze and a client pause over 2 s | Questionable |
| 2 | #224 accepted the soft-stop refusal window | Questionable |
| 3 | Missed `Local` policy behind cloud load balancers, pod-IP load balancers, and kube-proxy replacements | Questionable |
| 4 | None of about 20 surveyed projects observes kube-proxy; all use a timer or a connection count | Questionable |
| 5 | Pod API token, RBAC for EndpointSlices, Services, and Nodes, egress to every node | Questionable |
| 6 | About a dozen fallback modes | Questionable |
| 7 | New port knob to disable the check | Questionable |
| 8 | kube-proxy healthz format, version-dependent | Questionable |
| 9 | Polls every node from every terminating pod | Questionable |
| 10 | Rollback left the ServiceAccount behind (#451) | Questionable |
| 11 | Documenting the accepted window sufficed | Questionable |

Question 1 alone (benefit) together with question 5 alone (cost) should have stopped the build. All eleven were questionable.

## When the gate says "questionable"

Send the user one short note before writing code:

1. **Problem as observed.** Frequency, impact, and how it was seen (production, user
   report, or forced in a test).
2. **Cost answers.** The gate table for the cumulative change.
3. **Options, cheapest first.** No change; document the limitation; the smallest partial
   fix; the full mechanism. One line each with what it covers and what it costs.
4. **Recommendation.** One option and why.

Then wait. A draft branch built "to show what it would look like" is building first.

## Relation to RULE #2

RULE #2 protects the guarantees HAPTIC already makes. RULE #5 governs new mechanism
beyond those guarantees. They meet in two places:

- Declining to build mechanism for a rare, accepted risk leaves every guarantee intact.
  It is not weakening validation.
- A test may assert more than the product guarantees, for example zero refused
  connections across pod termination when the documented contract is HAProxy's soft
  stop. Aligning that test to the documented guarantee is allowed only with the user's
  explicit consent, recorded in the PR. Without consent, treat the red test as RULE #2
  does.
