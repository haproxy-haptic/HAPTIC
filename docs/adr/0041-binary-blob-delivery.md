# ADR-0041: Deliver binary blobs to the data plane

## Status

Proposed.

## Context

The `maxmind` SPOA plugin reads MMDB databases from local paths in the hub and the
validator sidecar. HAPTIC has no way to deliver them (issue #743). Operators mount a
PersistentVolumeClaim or write init containers that download the files, once for the
HAProxy pods and again for the controller pod, with the download credential and the
internet egress on every data-plane pod. A new database release arrives only when the
pods restart, and nothing validates it before the hub loads it.

The coraza ruleset has the delivery the databases lack: `waf.crs.url` fetches the
Core Rule Set through the HTTP store, refreshes it with conditional requests, accepts
it reversibly (ADR-0030), delivers it to every hub and validator, and keeps the last
deployed ruleset when a fetch fails. None of its transport carries an MMDB file. A
City database is 60 to 130 MB and barely compresses:

| Path | Limit |
| --- | --- |
| HTTP store body (`pkg/httpstore/types.go`) | 10 MiB, held as a Go string |
| `untar_gz` (`pkg/templating/filters_archive.go`) | 8 MiB per entry, 64 MiB stream |
| `HAProxyGeneralFile` content and ADR-0033 checkpoints | ~1.5 MiB per object (etcd), text |
| Agent `POST /v1/apply` (`pkg/dataplane/agent/api/limits.go`) | 64 MiB for one apply's whole file set |
| `general-storage` volume of the HAProxy pod | 16 MiB, in memory |
| Validator protocol frame (`pkg/controller/pluggablevalidator/protocol.go`) | 8 MiB of UTF-8 JSON, data resent per request |
| Every `haproxy -c` sandbox | writes every general file again |

The hub reloads a plugin when that plugin's TOML parameters change, not when a data
file it reads changes (ADR-0007; coraza watches its own includes, maxmind does not).

## Decision

Add a separate, generic path for **binary blobs**: opaque files identified by their
SHA-256 digest, fetched by the controller, validated before the fleet uses them, and
streamed to the agents outside the apply request. Nothing in Go knows MMDB or MaxMind
(RULE #1); the chart wires the `maxmind` plugin to it.

### 1. Template API

`blob.Fetch(url, options)` declares a blob source and returns a handle with the
accepted digest, the size and the local path the hub and the validator see
(`/var/lib/haptic/blobs/<sha256>`), or an empty handle while no version is accepted.
Options: `interval` (refresh), `timeout`, `retries`, `sha256` (pins the content; any
other digest is refused), `unpack` (`none`, `gzip`, or `tar.gz` with `member`, the
archive path of the one file to keep), `maxSize` (default 256 MiB, capped by the
controller flag `--blob-max-size`). Like `http.Fetch` with `critical: false`, a blob
that cannot be fetched never fails a render. The handle's path changes with every new
version, so a template that renders it into the hub's TOML gives the hub a parameter
change, and the hub's per-plugin reload loads the new file.

### 2. Controller blob store

A new store, separate from the HTTP store and its in-memory bodies, streams each
download to the controller's disk (a size-limited emptyDir), verifies the size cap
while reading, unpacks into a temporary file, computes the digest and renames the
result into place. It reuses the HTTP store's refresh scheduling and conditional
requests (ETag, Last-Modified). A version moves through the same states as HTTP
content under ADR-0030: a new digest is **pending** until a render that uses it passes
its gate, then **accepted**; a refusal revokes it to the previous accepted digest and
records the refused output so the same bytes are not accepted against the same inputs
again. The store keeps the accepted digest, its predecessor and any digest a pod still
reports; everything else is removed.

### 3. Gate: validate the blob, not just the config

A render that accepts a new blob digest takes the validator check before acceptance,
exactly as a render that accepts new HTTP content takes the synchronous `haproxy -c`
(ADR-0022 §3). `haproxy -c` never sees blobs: they are not general files, so its
sandboxes are unchanged. The validator sidecar gets them by reference (§5) and runs
the plugin's own `validate()` against the real file. A plugin that refuses the file
refuses the render, and the store revokes the digest.

### 4. Agent: blob endpoints and volume

- `PUT /v1/blobs/{sha256}` streams one blob into the pod's new `blobs` volume, a
  disk-backed emptyDir mounted read-write in the agent and read-only in the spoa-hub
  container. The agent writes to a temporary file, verifies the digest, then renames
  it into place; a mismatch is refused and nothing is kept. It is authenticated like
  `/v1/apply` (controller certificate, or Basic auth in plaintext mode) and bounded by
  `--blob-max-size`, not by `MaxApplyBodyBytes`.
- `GET /v1/blobs/{sha256}` serves a blob the agent holds, for controller recovery (§6).
- `GET /v1/state` lists the digests the agent holds.
- The apply manifest names the blob digests its configuration requires. The agent
  refuses an apply whose blobs it does not hold (`missing_blobs`), and the controller
  pushes them and retries, so a pod never loads a configuration that references a
  missing file. Blobs are not part of the journal's file transaction: they are
  immutable and content-addressed, so the last-known-good set stays valid as long as
  its digests are kept.
- The agent removes blobs that neither its applied configuration nor its
  last-known-good set references, after the apply that stopped using them.

The controller pushes a new blob to each pod once, before the apply that needs it,
with bounded concurrency, and only to pods that do not report it. A new pod receives
the accepted blobs before its first apply.

### 5. Validator: blobs by reference

The controller pod gets a `blobs` volume shared by the controller (read-write) and the
validator sidecar (read-only). The controller writes accepted and pending digests
there. A validator request names blobs as `{path, sha256}` references instead of
inline `dataFiles` content (protocol version bump; a validator that does not announce
the capability gets no blob references and the controller refuses to accept a blob it
cannot have validated). The 8 MiB frame limit and per-request resending no longer
apply to them.

### 6. Recovery

Blob bytes never go to Kubernetes objects. ADR-0033 checkpoints record the digests
their configuration references. A controller that starts without a digest it needs,
or loses its disk, first takes it from an agent that reports it (`GET /v1/state`, then `GET /v1/blobs/{sha256}`),
verifying the digest, then falls back to a fresh fetch. While neither works, renders
keep referencing the deployed digest: the pods keep serving with the file they hold,
and only new pods wait for it, like any configuration they cannot receive yet.

### 7. Chart

- `extraContext.maxmind.databases.<name>` (a keyed map): `url`, optional `sha256`,
  `unpack` and `member`, `refreshInterval`. The `maxmind` plugin's TOML gets each
  database's handle path; a database without an accepted version leaves its lookups
  out, so requests pass unenriched (enrichment fails open, `charts/CLAUDE.md`).
- The HAProxy and controller Deployments get the `blobs` volumes only when a blob
  source is configured, with `sizeLimit` from `blobs.sizeLimit`.
- Memory: the plugin keeps each database in memory. The chart documents the hub and
  validator memory a database needs and refuses a render whose databases exceed the
  configured limits.

### Complexity gate (RULE #5)

| # | Answer |
| --- | --- |
| 1 | Seen in practice: GeoIP enrichment and sanctions blocking on a public ingress need region-level data; without delivery, operators maintain init containers in two pod types (#743). |
| 2 | Not an accepted risk; #743 asks for it. |
| 3 | Fixes the common paths: any installation with the maxmind plugin, behind a proxy or without pod egress. |
| 4 | HAProxy Enterprise's maxmind module has `maxmind-update` to download and refresh databases from the data plane; HAPTIC does it from the controller. |
| 5 | No new egress, credentials or permissions on data-plane pods: they talk only to the controller, over the existing authenticated agent channel. The controller gains egress to the configured URLs, as for `http.Fetch`. |
| 6 | New degraded modes: blob not yet accepted (lookups left out), agent missing a blob (apply waits for the push). Both reuse existing states. |
| 7 | Knobs: per-source URL, digest, unpack and refresh, plus volume size and the size cap. No knob that only disables the feature. |
| 8 | No new external coupling beyond the configured URLs. |
| 9 | Per pod: one copy of each blob per version (130 MB City, monthly or weekly), memory for the plugin's copy. One download per version for the fleet. |
| 10 | Disabled by default; no volume or endpoint use without a blob source. Rollback: remove the source; agents remove unreferenced blobs. Older agents without the endpoint get no blob references and the controller does not accept blobs for them (version skew degrades, never refuses). |
| 11 | Init containers suffice for a fixed file but give no validation, refresh or fallback, and put egress on every pod. |

## Consequences

- MMDB databases arrive like the CRS: fetched once, validated by the plugin before any
  pod uses them, refreshed, and kept when a source fails.
- The agent API, the validator protocol and both Deployments grow; old agents and
  validators keep working without blobs.
- Pods hold each database on disk once and in the hub's memory once; the chart's
  memory guidance and limits must account for it.
- Any other plugin that needs large binary data can use the same path.

## Alternatives considered

- **Raise the limits of the configuration path** (HTTP store, general files, apply,
  validator frames). Some limits cannot be raised: etcd refuses objects above
  ~1.5 MiB, so a general file cannot be a 130 MB `HAProxyGeneralFile` or checkpoint
  child, and that content is text. The others guard against copies that are cheap
  for small text files and expensive for a database: the controller holds a fetched
  body as a string in its accepted, pending and revocation versions, the render
  output, the file registry and the published-file baseline (about 6 × 130 MB against
  a 1 GiB limit); every `haproxy -c` (webhook, load gate, rendergate) writes all
  general files into its sandbox; the validator protocol resends data files inline
  with every request; one apply carries the whole file set, and the agent's journal
  keeps temporary and last-known-good copies in the 16 MiB in-memory `general-storage`
  volume. A database changes weekly or monthly and is immutable per version, so it
  moves once by digest instead of with every render, admission, validation and
  checkpoint.
- **Init containers or a fetcher sidecar per pod**: egress and credentials on every
  data-plane pod, no validation before use, every pod fetches and can drift.
- **Fetching in the hub or the plugin**: the same egress, in the request path, and
  outside HAPTIC's validation.
- **OCI image volumes**: needs a pipeline that republishes every database as an
  artifact and Kubernetes image-volume support; updates still roll pods and are not
  validated by the plugin.
