# 40. Verify website tooling separately from release content

Date: 2026-11-04

Status: Accepted

## Context

A website publication currently checks out the content's canonical CI commit for
both assembly and browser verification. A released version therefore also freezes
its publisher. Repairing its download transport or redirects requires running the
old publisher again or changing the release tag.

A hosted diagnostic for v0.11.0 recorded an engine download still pending after
30 seconds. Other page assets completed, and memory remained bounded. The
playground entry page also retained a redirect to v0.5.0 despite the catalog
identifying v0.11.0 as current.

## Decision

Pin tooling to the workflow commit. Require that exact commit's complete canonical
main push CI receipt before building, retaining, or verifying a publication. Recheck
the same receipt before retention and live verification; changed attempts fail.

Verify content's canonical source, tree, manifest, and artifacts directly from Git
objects. Read its version and publication baseline from those same objects; never
check out its code. Every Actions checkout uses the workflow commit directly, so
content cannot supply executable tooling or dependency installation paths. Retain
the existing release-tag checks. Store both receipts in publication
evidence. Version archives and their provenance remain immutable. Assembly and
shared transport use the verified tooling without rebuilding released engines.

The browser still checks every version, both engines, rendered configuration and
maps, compressed and decoded checksums, corruption rejection, decoder memory,
and redirects within the existing deadlines. Add the playground entry redirect.

Firefox can omit the load event for an empty HTTP 503 response. Observe main-page
5xx responses alongside the original full-load navigation so the existing bounded
retry can handle them. Successful pages still require full load within 30 seconds.

Cancel an engine transfer after five seconds without network progress and restart
it at most twice. Progress resets the idle deadline; a slow active transfer can
continue. Each attempt starts with an empty buffer. Size or checksum violations
fail immediately. This changes the downloader, not the browser's 30-second gate.

## Consequences

Publication now requires two independent source receipts instead of one. Tooling
changes cannot publish before their canonical CI passes. An old release receives
transport and redirect repairs without changing its binaries, chart, engine,
version archive, or tag. Stalled transfers can consume up to three downloads.
