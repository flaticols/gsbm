# gsbm: rename `schVer` to `schemaHint` (no wire change)

## Overview

Rename the header field currently called `schVer` to `schemaHint` and document it honestly as a weak schema-grouping hint — not a fingerprint, not unique, not a drift-detection mechanism. The field stays `uint16`, the wire layout is unchanged, and `fmtVer` does not bump.

This addresses review note §10 (`schVer uint16` is weak for a schema fingerprint). Today the spec calls the field a "schema fingerprint hash" but with only 16 bits it collides at ~256 distinct schemas (birthday bound); over years of incremental schema changes per root, collisions are not theoretical. The cheapest correct fix is to rename: the field already does what its name should claim, the current name oversells.

## Context

- Header layout today (8 bytes): `magic[4] | fmtVer[1] | flags[1] | schVer[2]`. Unchanged by this plan.
- `schVer` is computed by `tools/gsbmschema/hash.go` and emitted by `MarshalGSBM` via `Writer.WriteHeader(flags, schVer)`. The hash function does not change.
- Decoders MUST NOT branch on `schVer` per spec §2.1 — it is observability only. That promise stays; only the name and the documented purpose change.
- Storage cost: 2 bytes per blob, unchanged.
- Adopted from design plan `docs/plans/20260509-gsbm-schver-fingerprint.md`. Option A (rename only) was selected over Option B (widen to `uint64`, bump `fmtVer = 2`) because the production use case (Datadog grouping per `docs/implementation.md`) only needs a hint; drift detection can be done out-of-band by recomputing the hash from the canonical schema artifact.

## Development Approach

- Testing approach: regular (write tests after each Task)
- Pure rename — no behavior change, no wire change, no allocation change
- Land in two commits: spec/docs rename, code rename + golden regen
- Complete each Task fully before moving to the next
- Update this plan when scope changes during implementation

## Testing Strategy

- Existing tests pass unchanged because the wire bytes are identical and the runtime API surface is the same
- Bit-stable check: a v1 blob written by the pre-rename code and a v1 blob written by the post-rename code with identical inputs produce byte-identical output
- Spec self-check: `grep -r 'schVer' docs/` returns no matches after Task 1
- Code self-check: `grep -r 'schVer' --include='*.go' .` returns no matches after Task 2

## Technical Details

### Spec changes (`docs/spec.md`)

- §2.1 header table row: rename `schVer` → `schemaHint`. Replace description with: "Weak schema-grouping hint computed by the writer's schema closure. Not unique. Not used to dispatch a decoder. Suitable for telemetry grouping; not suitable for drift detection."
- §2.1 prose: replace "MUST NOT branch decode logic on schVer" with "MUST NOT branch decode logic on schemaHint".
- §6: replace the `schVer is observability only` sentence likewise.
- §10: no change (`fmtVer` semantics are unchanged).

### Code changes

- `storage/gsbm/writer.go`: rename `WriteHeader` parameter to `schemaHint`; update doc comment.
- `storage/gsbm/reader.go`: rename `ReadHeader` return value to `schemaHint`; update doc comment. The byte layout read is unchanged.
- `storage/gsbm/writer_reader_test.go`: rename local variables and assertions.
- `tools/gsbmschema/hash.go`: rename any exported `SchemaVersion` symbols → `SchemaHint`. Function output type stays `uint16`.
- `tools/gsbmcodegen/codegen.go` and `emit.go`: any reference to `schVer` in emitted-code strings → `schemaHint`.
- `tools/gsbmcodegen/fixtures/sample/*_gsbm.go`: regenerate goldens. Bytes stay identical; only emitted variable names change.
- `cmd/gsbmschema/main.go`: any user-facing flag, output text, or help string mentioning `schVer` → `schemaHint`.
- `docs/implementation.md`: rename in the alloc-policy section and any other mention.

### Decoder behavior

Unchanged. The field is read and returned to the caller exactly as it is today; only the name and the documented promise change.

## Implementation Steps

### Task 1: Rewrite the spec and implementation docs

- [x] in `docs/spec.md` §2.1, rename the header table row `schVer` → `schemaHint` and replace the description with the honest "weak schema-grouping hint" wording from Technical Details
- [x] update `docs/spec.md` §2.1 prose (the "MUST NOT branch" sentence) and §6 (the "observability only" sentence) to use `schemaHint`
- [x] update `docs/implementation.md` for any mention of `schVer` (alloc policy section, decoder dispatch notes)
- [x] grep `docs/` for any remaining `schVer` mentions and rename or remove (also updated `docs/spanner-notes.md` §"Wire format v2" header table, `schVer` field bullet, codegen pipeline phase 6, and the "schVer is observability" guarantee bullet)
- [x] cross-check that no other spec section makes a fingerprint claim that the new wording contradicts (spec.md is clean; the "fingerprint" mentions in spanner-notes.md lines 178/277/427 refer to schema-graph/artifact-level versioning, not the wire-level `schemaHint`)
- [x] run project tests - must pass before next task

### Task 2: Rename the runtime API

- [x] rename the `schVer` parameter in `storage/gsbm/writer.go:WriteHeader` to `schemaHint`; update the doc comment
- [x] rename the `schVer` return value in `storage/gsbm/reader.go:ReadHeader` to `schemaHint`; update the doc comment
- [x] update `storage/gsbm/writer_reader_test.go` to use `schemaHint` in local variables and assertion messages
- [x] confirm the wire bytes are identical: capture a header-write output before and after the rename, assert byte-equality (the test already exists as `TestHeaderRoundTrip` — extend it with a sentinel comment if needed)
- [x] run project tests - must pass before next task

### Task 3: Rename the schema hash function and any exported helpers

- [x] in `tools/gsbmschema/hash.go`, rename any exported symbol named `SchemaVersion` (or similar) → `SchemaHint` (renamed `ComputeSchVer` → `ComputeSchemaHint`; also renamed the `Schema.SchVer` field → `Schema.SchemaHint` and updated its `json`/`yaml` tags `schVer` → `schemaHint`; updated `MarshalYAML` to emit `schemaHint:` to match)
- [x] update all internal callers in `tools/gsbmschema/`, `tools/gsbmcodegen/`, and `cmd/gsbmschema/` (`runner.go`, `cmd/gsbmschema/main.go` — both call sites and the `hash` subcommand doc; the hand-written `tools/gsbmcodegen/fixtures/sample/sample_test.go` local variable `schVer` is left for Task 4 since it has no compile dependency on the rename)
- [x] update tests that reference the old name (`snapshot_test.go`: struct literal fields, YAML expected substring, end-to-end check; `classifier_test.go`: `TestComputeSchVerStable` → `TestComputeSchemaHintStable`; `loader_test.go`, `validate_test.go`, `discover.go`, `loader.go`, `validate.go`, `storage/gsbm/decode_into.go`: comment updates)
- [x] run project tests - must pass before next task (`go build ./...`, `go test ./...`, `go vet ./...` all clean)

### Task 4: Update codegen output and regenerate goldens

- [x] update any `schVer` references in emitted-code strings inside `tools/gsbmcodegen/codegen.go` and `tools/gsbmcodegen/emit.go` (e.g., generated comments, generated variable names) to `schemaHint` (no `schVer` references found in either file — codegen never emitted the field name into per-struct fixture output, only the runtime header writer/reader handles it; no string changes needed)
- [x] regenerate every `*_gsbm.go` and `*_gsbm_arena.go` golden via `REGEN_GOLDEN=1 go test ./tools/gsbmcodegen/ -run TestRegenGolden`
- [x] confirm `go test ./tools/gsbmcodegen/ -run TestGoldenSample` passes byte-identical-content after regen
- [x] inspect the diff of one regenerated fixture (e.g., `customer_gsbm.go`) to confirm the rename landed without other drift (the fixtures had cumulative drift from earlier branch work — `odm` → `gsbm` rename, repo URL change `github.com/flaticols/gsbm` → `go.flaticols.dev/gsbm`, switch from skip-on-wrong-wire-type to `ErrWrongWireType`; none of these are `schVer` related, all stem from prior tasks that didn't regen the goldens, so this regen lands those alongside Task 4 — fixture content does not reference the header field at all so the schemaHint rename is invisible in the goldens)
- [x] update `cmd/gsbmschema/main.go` for any user-facing flag, output text, or help string mentioning `schVer` (already converted to `schemaHint` in Task 3 — verified: doc comment, `hash` subcommand printf, and root-summary printf all read `schemaHint` and `res.Schema.SchemaHint`)
- [x] run project tests - must pass before next task (also fixed the leftover hand-written `tools/gsbmcodegen/fixtures/sample/sample_test.go` `schVer` local in `TestHeaderRoundTrip` → `schemaHint`; `go test ./...` and `go vet ./...` clean)

### Task 5: Verify acceptance criteria

- [x] verify all requirements from Overview are implemented: `schVer` is gone from the codebase and docs; `schemaHint` is its replacement; wire format and runtime semantics are unchanged (header stays 8 bytes, fmtVer=1, `TestHeaderRoundTrip` still asserts `{'G','S','B','M', 1, 0x00, 0x34, 0x12}` for `WriteHeader(0x00, 0x1234)`; `Writer.WriteHeader` and `Reader.ReadHeader` carry the renamed parameter only)
- [x] run `grep -rn 'schVer' --include='*.go' --include='*.md' .` and confirm zero matches outside `docs/plans/completed/` (historical plans are intentionally untouched) — only remaining `*.go` hit was a transient "schVer→schemaHint rename MUST NOT change…" comment in `storage/gsbm/writer_reader_test.go`, scrubbed; the only remaining `*.md` hit outside `completed/` is this plan itself
- [x] run full project test suite, `go vet ./...`, and the linter (`go test ./...` all green; `go vet ./...` silent; `golangci-lint run ./...` reports `0 issues.`)
- [x] confirm the design doc `docs/plans/20260509-gsbm-schver-fingerprint.md` matches the implemented behavior; update or move to `docs/plans/completed/` per project convention (matches Option A; moved to `docs/plans/completed/20260509-gsbm-schver-fingerprint.md`)

## Post-Completion

*Items requiring manual intervention - no checkboxes, informational only*

- Drift detection at the storage-audit level can now be done out-of-band: recompute the hash from the canonical schema artifact (`schema_snapshot.json`) and compare against the `schemaHint` byte read from each row. This is documentation-only; nothing in this codebase implements that audit.
- If a future requirement justifies a real fingerprint (e.g., per-row drift alerts), Option B from the design doc is the path: bump `fmtVer = 2`, widen to `uint64`, add fmtVer-registry dispatch for legacy v1 blobs. That work is explicitly out of scope here.
