# gsbm: schVer width / fingerprint strength

## Overview

Decide what `schVer` actually is and rename or widen it accordingly. Today the spec calls it a "Schema fingerprint hash" but it is only `uint16` — too narrow for meaningful identity, drift detection, or sanity beyond rough Datadog grouping. A 16-bit hash collides at ~256 distinct schemas (birthday bound); over years of incremental schema changes per root, collisions are not theoretical.

This addresses review note §10 (`schVer uint16` is weak for a schema fingerprint). Two options; one needs to be picked before this lands.

## Context

- Header layout today (8 bytes): `magic[4] | fmtVer[1] | flags[1] | schVer[2]`.
- `schVer` is computed by `tools/gsbmschema/hash.go` (FNV-style or similar) over the schema closure and emitted by `MarshalGSBM` via `Writer.WriteHeader(flags, schVer)`.
- Decoders MUST NOT branch on `schVer` (spec §2.1) — it is observability only.
- Storage cost: schVer is 2 bytes per blob. For Spanner offer batches in the 1-100KB range, 4-6 extra header bytes (if widened) is well below noise.

## Development Approach

Two paths; pick one before implementation:

### Option A — Honest rename, no wire change (recommended for v1)

Rename `schVer` → `schemaHint` and document it as "a weak schema-grouping hint, not unique, not for drift detection". Keep `uint16` and the existing computation. Wire format unchanged, header layout unchanged, no `fmtVer` bump.

This is the cheapest correct fix: the field already does what its name should claim. The current name oversells.

### Option B — Widen to a real fingerprint (`fmtVer = 2`)

New header layout (12 bytes): `magic[4] | fmtVer[1] | flags[1] | schemaHash[8 — uint64 LE] | reserved[2 — must-be-zero]`. Or: drop the trailing reserved bytes for 10 bytes total. Bumps `fmtVer` to 2; v1 readers reject v2 blobs, v2 readers dispatch to v1 decoder for legacy blobs (the existing fmtVer-registry pattern).

Cost: every v2 blob is 4-6 bytes larger. Real schema-drift detection becomes possible at the storage-audit level (e.g., "all rows for `Offer` should have one of these 12 known schema hashes; flag any others"). Decoder behavior is unchanged — schemaHash is still observability-only, never a dispatch key.

## Recommendation

Option A. The current production use case (Datadog grouping per `docs/implementation.md`) only needs a hint. Drift detection at the storage-audit level can be done out-of-band by recomputing the hash from the canonical schema artifact and comparing — it does not need to be in the blob. Bumping `fmtVer` for an observability-only widening is not justified.

## Testing Strategy

For Option A:
- Replace every textual occurrence of `schVer` with `schemaHint` in `docs/spec.md`, `docs/implementation.md`, `storage/gsbm/`, `tools/gsbmschema/hash.go`. Existing tests pass unchanged because the wire layout is identical.

For Option B (only if A is rejected):
- New test in `storage/gsbm/writer_reader_test.go`: a v2 blob round-trips schemaHash; a v1 blob is rejected by a v2-only decoder unless the legacy-dispatch path is wired up; a v2 blob is rejected with `ErrUnsupportedVer` by a v1-only decoder.
- Cross-version property test: encode under v1, decode under v2 via legacy dispatch; encode under v2, decode under v2; both DeepEqual.

## Technical Details (Option A)

### Spec changes (`docs/spec.md`)

- §2.1 header table row: rename `schVer` → `schemaHint`; update description to "Weak schema-grouping hint computed by the writer's schema closure. Not unique. Not used to dispatch a decoder. Suitable for telemetry grouping; not suitable for drift detection."
- §2.1 prose: replace "MUST NOT branch decode logic on schVer" with "MUST NOT branch decode logic on schemaHint".
- §6: replace the `schVer is observability only` sentence likewise.
- §10: no change (`fmtVer` semantics are unchanged).

### Code changes

- `storage/gsbm/writer.go`: `WriteHeader(flags uint8, schemaHint uint16)`.
- `storage/gsbm/reader.go`: `ReadHeader() (flags uint8, schemaHint uint16, err error)` — already returns the second value, just rename the local + the doc comment.
- `tools/gsbmschema/hash.go`: rename `SchemaVersion` → `SchemaHint` (or whatever the export is today). Output type stays `uint16`.
- `tools/gsbmcodegen/codegen.go`: any reference to `schVer` in emitted code → `schemaHint`.
- `tools/gsbmcodegen/fixtures/sample/*_gsbm.go`: regenerate goldens.
- `cmd/gsbmschema/main.go`: any user-facing flag/output mentioning `schVer` → `schemaHint`.
- `docs/implementation.md`: rename in the alloc-policy section and any §6 reference.

### Decoder behavior

Unchanged. The field is read and returned to the caller exactly as it is today; only the name and the documented promise change.

## Critical files

- `docs/spec.md` (§2.1, §6)
- `docs/implementation.md` (any `schVer` mention)
- `storage/gsbm/writer.go`, `reader.go`, `writer_reader_test.go`
- `tools/gsbmschema/hash.go` (and any export rename ripples)
- `tools/gsbmcodegen/codegen.go`, regenerated goldens under `fixtures/sample/`
- `cmd/gsbmschema/main.go`

## Verification

1. `go build ./... && go vet ./...` clean after the rename.
2. `go test ./... -count=1` — all existing tests pass; the wire bytes are identical so golden tests are unaffected once the goldens are regenerated for the new symbol name.
3. Spec self-check: grep `docs/` for any remaining `schVer` mention; rename or remove.
4. Sanity: a v1 blob written before this change and a v1 blob written after this change are byte-identical at the header. The collision risk that motivated this plan is acknowledged in the new wording, not engineered around.

## Open question to confirm before starting

Pick A or B. If B, the plan body needs to expand to cover the fmtVer-registry dispatch path and legacy-blob handling — this plan currently spec's only Option A in detail.
