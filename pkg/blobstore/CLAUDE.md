# pkg/blobstore - Blob Store

Pure component: content-addressed binary blobs on the controller's disk (ADR-0041). No event bus.

- `Ingest` streams a body into a temporary file, unpacks it (`none`, `gzip`, one `tar.gz`
  member), applies the size cap while reading, computes the SHA-256, refuses a pinned-digest
  mismatch, and renames the file to `<dir>/<sha256>` (mode 0444). Refusals wrap `blob.ErrRefused`.
- `Open` and `Has` serve held blobs for streaming to the data plane.
- `Retain(inUse)` deletes files no holder (`AddHolder`) and no in-use digest references, once they
  are older than `OrphanGrace`. The grace covers render-local candidates, which no HTTP store
  entry references until their render publishes them.

The HTTP store (`pkg/httpstore`) calls `Ingest` through its `BlobSink` interface; the pure store
knows nothing about HTTP.

## Event adapter

`pkg/controller/blobstore` enables blob sources on one iteration's HTTP store component, registers
that store's digests as a holder (accepted, pending, replaced, and each source's previous accepted
digest), and runs `Retain` every `RetentionInterval`. The store itself outlives iterations: it is
created in `cmd/haptic` from `--blob-dir` and `--blob-max-size` and lives in `persistentInfra`.
