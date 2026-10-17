# ADR-0030: HTTP content acceptance is reversible and off the deploy path

## Status

Accepted 2026-10-06 (issue #289). Reverses the rejection of option 2 in #276.

[ADR-0032](0032-watched-input-isolation.md) adds synchronous validation before
watched inputs advance and isolates rejected revisions. HTTP proposals use that
accepted input view; the content revocation rules below remain in force.

## Context

`http.Fetch` content becomes the store's *accepted* version only after a render
containing it passes a synchronous `haproxy -c` (`checkBeforeCommit`). Until
this ADR, the commit also required every watched input the render read to be
unchanged in the live store (`commitOutlivesNextRender`). Acceptance could not
be undone: the render gate's verdict reverts the fleet's files, not the store
(ADR-0022 §3).

Under churn that live check never holds. On a contended node the render plus
its check take 0.6–1.2 s; the cache shard of !1955 changed about 18 resources
per second. The custom-http-errors pages lost 17 of 17 acceptance attempts, and
each attempt (candidate render, check, re-render without it, re-trigger) sat in
front of the deploy, so a rolling restart's endpoint change took about 1.5 s to
reach HAProxy (#289; earlier forms #276, #278).

Issue #276 rejected checking acceptance against the render's own snapshot (option 2)
as a narrowing. Two facts were not weighed then:

- Deploy renders are not checked before they reach the fleet. The render gate
  checks them after optimistic dispatch (ADR-0022); the live check protected
  only the acceptance window, roughly one render plus one check.
- Because acceptance was irreversible, content valid with inputs S but invalid
  with S′ was only kept out when S′ arrived inside that window. If S′ arrived a
  millisecond later, every render failed the gate until the content or the
  inputs changed.

## Decision

1. **Accept against the render's own snapshot.** A render that fetched new
   content commits it when its own inputs and its `haproxy -c` agree.
   `commitOutlivesNextRender` keeps the live-store check for admission only.
   `verifyHTTPInputs` and the lease's `ErrInputsMoved` are unchanged: a render
   still cannot accept content another render replaced.
   If a sibling publishes the graph first, an uncached acceptance still
   verifies its captured HTTP lease atomically with input publication. It
   neither acknowledges pending lease changes nor changes refresher ownership.
2. **Revoke on refusal.** The HTTP store records each acceptance with the
   version it replaced until a render that read it passes the gate.
   - The coordinator binds each dispatched occurrence to the exact HTTP read
     tokens it produced and the acceptance sequence reached when it finished.
     Weak keys keep that window until the occurrence becomes unreachable,
     including while a check or verdict is delayed, without retaining render
     snapshots in the coordinator.
   - A pass confirms only those exact source declarations and revisions. A
     cached execution without a fresh HTTP observation leaves acceptance
     reversible; another source's pass or an old same-content read cannot
     confirm it.
   - A refusal revokes every unconfirmed acceptance up to `reached`. The URL
     returns to its previous accepted version, or to no accepted content, and
     the next render goes without it.
   - Each revocation emits an `HTTPContentRevoked` Warning Event and
     increments `haptic_http_content_revoked_total`.
3. **Bound revocation.** The coordinator remembers the outputs the gate
   refused. An acceptance attempt whose render produces one of them stops
   before its check, so the same bytes are not accepted again against the same
   failing inputs. Re-acceptance needs a different output, which means new
   content or new inputs. Without a refused output, the attempt's own
   `haproxy -c` refuses content that is still invalid.
4. **Accept off the deploy path.** The leader's reconcile renders with pending
   content withheld and deploys immediately. A withheld source asks for an
   acceptance attempt. The coordinator runs at most one at a time, beside the
   reconcile loop, and discards its output. When content was accepted after
   the latest deploying render read the store, by the attempt or by another
   render on the replica, it triggers the reconcile that deploys the content
   (#363). Followers' warm-up renders
   still accept before completing warm-up. Both accepting paths defer initial
   HTTP I/O: a render discovers missing sources and releases its graph and
   the sole cold-render slot (ADR-0029). The pipeline then fetches with at
   most `GOMAXPROCS` workers outside the renderer, retaining only source
   declarations and fetched candidates, and retries with their exact source
   and version proofs. A changed source is fetched again, and a failed
   publication fence discards the deferred reads before retrying. Nested URLs can require several attempts.
   Cancellation drains these fetches before the pipeline returns.
   Render publication copies HTTP cache entries to update their access time.
   These bookkeeping copies preserve fetch and candidate authority only when
   every other source, content, validation, and revision field is identical.
   A new allocation alone cannot invalidate an in-flight read; an actual
   source or version change still does, including a change and return to the
   same bytes. Prepared publication authentication continues to compare the
   exact current entry, including access time.
   This preserves cold-render serialization and lets deployment progress while
   a follower or acceptance worker waits for HTTP. Before the first graph is
   published, rendering and its validation check still share the cold slot;
   after publication, acceptance runs beside warm deployment renders.
5. **Critical sources.** A critical source without accepted content fails the
   deploying render (`ErrCandidateWithheld`) instead of deploying without it,
   whether it was never accepted or was revoked. The reconcile succeeds again
   once an attempt accepts the content.
   If fetching required content fails, the acceptance worker records that failure
   and requests reconciliation once. The coordinator reports the blocked render
   through its normal failure events, allowing retained-configuration recovery.
   Successful acceptance clears the failure.
6. **An unavailable critical source holds renders, not just deploys** (issue
   #332). Without a graph, every reconcile render is cold and fails on the same
   missing source, so retrying it per trigger burned a full render and a fetch
   for each resource change. When a critical source without accepted content
   fails to fetch, the HTTP store records the failure and refetches the source
   outside any render, every `interval` or every 30 s without one. Renders that
   reach the source read the recorded failure instead of fetching it. After a
   reconcile render fails on it, the replica's reconcile renders (follower
   warm-up, deploy, and acceptance) return that failure without rendering until
   the refetch succeeds, which triggers a render that adopts the fetched
   candidate. A refetch that fails after renders were skipped triggers one render,
   because the skipped inputs may no longer need the source. A completed render
   ends every outage, since an unavailable critical source fails its render.
   Configuration changes start a new iteration without that state. The leader
   reports the held failure once per term. Input isolation (ADR-0032) asks for
   the hold before it captures and stages the watched set, so a held trigger
   skips that work too (issue #354).

## RULE #2 delta

| Case | Before | After |
|---|---|---|
| S′ (invalid with X) lands while X is being accepted | X stays pending | X is accepted. The first render with S′ + X is refused by the gate, X is revoked, and the next render goes without it |
| S′ lands after X was accepted, before any render with X passed | Every render fails the gate until X or the inputs change | Same refusal, then X is revoked and the fleet keeps progressing |
| S′ lands after a render with X passed | Every render fails the gate | Unchanged: X is part of the last-known-good configuration, so an input that breaks it is an input problem |
| Every deploy render | Checked by the gate after dispatch | Unchanged |

The first row trades one keep-out for a refused render: the ADR-0022 exposure
window of one check plus one apply, then recovery. The second row turns a
stuck fleet into a recovered one. Every render is still checked, and content
reaches the store's accepted version only through a passing `haproxy -c`.
Content that a later input makes invalid is now caught and recovered from,
where before it was caught only inside a window of about one second. That is
strictly stronger.

## Consequences

- A non-critical source's fetch does not hold the render gate. Before the
  first graph, its render and check remain serialized with other cold work;
  afterward they run beside deployment. Watched resource churn no longer
  invalidates acceptance; HTTP publication conflicts still require another
  attempt.
- After a controller start, the first deploy omits non-critical content until
  the attempt accepts it, after its background fetch and check. Content that must never be
  missing has to be `critical: true` (the existing guidance).
- A refused render that read unconfirmed content also revokes content that did
  not cause the refusal. That content returns on the next attempt, whose own
  check passes.
