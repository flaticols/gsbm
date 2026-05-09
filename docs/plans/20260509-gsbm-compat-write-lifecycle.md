# gsbm: compat-write lifecycle for replacement field migrations

## Overview

Add a `compat_write` annotation to the `bin` struct tag and the schema classifier so that **replacement** field migrations (old field deprecated, new field added with different tag) are rollback-safe end-to-end, not only structurally. During the rollback window, the new code MUST keep writing the old field alongside the new one; only after the window closes does the encoder stop emitting it. The classifier and codegen enforce the lifecycle so this can't drift into a README rule.

This addresses review note §6 (rollback safety is overstated). The current append-only policy is structurally rollback-safe — old readers skip unknown new tags, new readers see zero for unread old tags — but if new code stops writing an old field outright, a rollback to old code will see the field disappear from a business-data perspective. That is data loss in operational terms.

## Context

- Current schema state model is binary: a field is **active** (written + read) or **deprecated** (still readable from historical records, not written by new code). There is no in-between.
- `tools/gsbmschema/classifier.go` produces `safe | warning | breaking` labels. Replacement-style migrations land as a pair: `field/added` (the new tag, label safe) and `field/deprecated` (the old tag, label safe). The classifier doesn't know the two are linked.
- `tools/gsbmcodegen/emit.go:activeFields` filters out deprecated fields from `MarshalGSBM` — so the moment a field flips to deprecated, the encoder stops writing it.
- Production rollback windows in this org are 24-72h (per `docs/spanner-notes.md`). A field that disappears from new writes during that window is effectively gone from any rollback-decoded record.

## Development Approach

- Test approach: classifier tests (table-driven over before/after schema pairs) + codegen golden tests (a fixture struct with a `compat_write` field exercises dual-write).
- Land in three small commits: tag-parser change, classifier states, codegen emit. Each commit ships with its own tests.
- Do not retro-link historical deprecated fields — the lifecycle is forward-only and applies to fields tagged after this lands.

## Testing Strategy

- New unit tests in `tools/gsbmschema/parse_test.go` for the `compat_write` tag-parse path (accepts, rejects on missing `deprecated`, etc.).
- New table-driven tests in `tools/gsbmschema/classifier_test.go` covering the four lifecycle transitions (active → compat_write → deprecated → reserved) and the illegal jumps (active → deprecated without compat_write window).
- New fixture struct in `tools/gsbmcodegen/fixtures/sample/` (e.g., a renamed-field `Order.LegacyCode`/`Order.RetailCode` pair) plus a round-trip test that confirms both tags are present on the wire while the old field is in the `compat_write` state.
- Property test: a record written by the new (compat_write) encoder MUST decode under the old schema (the still-deprecated field path) without losing the field's value.

## Technical Details

### Lifecycle states

```
active                  bin:"7"
deprecated, compat_write bin:"7,deprecated,compat_write"   ← new state
deprecated              bin:"7,deprecated"
reserved                //gsbm:reserved 7
```

State transitions allowed by the classifier:
- `active → deprecated,compat_write` — safe (field is being phased out; encoder still writes it).
- `deprecated,compat_write → deprecated` — safe **only when the diff also marks at least one rollback window has elapsed** (operationally: after the deploy that flipped to compat_write has been baked for ≥ the rollback SLO; the classifier can't enforce time, so it requires an explicit `--allow-stop-compat-write` flag on the diff command, mirroring `//gsbm:allow-breaking`).
- `deprecated → reserved` — safe (existing rule, unchanged).
- `active → deprecated` (skipping compat_write) — **warning**, with the recommendation to land compat_write first. Allowed for additive-only fields where no rollback risk exists, but the classifier surfaces the skip so reviewers see it.

### Tag parser changes (`tools/gsbmschema/parse.go`)

The `bin:"N,deprecated,compat_write"` token list extends `parseFieldTag` with a new boolean. Compat_write is only valid in combination with `deprecated`; the parser rejects standalone `compat_write` as an unknown qualifier.

### Classifier changes (`tools/gsbmschema/classifier.go`)

- New `FieldDecl.CompatWrite bool` populated by the parser.
- New change codes: `field/compat-write-added`, `field/compat-write-removed`.
- Update `compareField` so the active→compat_write→deprecated trajectory is recognized as a coherent lifecycle, not three independent edits.
- The diff command grows a `--allow-stop-compat-write` flag mirroring `--allow-breaking` for the compat_write→deprecated transition.

### Codegen changes (`tools/gsbmcodegen/emit.go`)

- `activeFields` becomes `writableFields`: returns active fields plus deprecated-with-compat_write.
- Decoder unchanged — deprecated and deprecated-with-compat_write fields decode identically (read into the field if it exists in the Go struct, otherwise SkipField on the wire). The codegen's existing `FieldDecl.Deprecated` already gates whether the decode path stores the value.

## Critical files

- `tools/gsbmschema/parse.go` (tag parser)
- `tools/gsbmschema/types.go` (`FieldDecl.CompatWrite` field)
- `tools/gsbmschema/classifier.go` (new states, change codes, lifecycle table)
- `tools/gsbmschema/parse_test.go`, `classifier_test.go`
- `tools/gsbmcodegen/emit.go` (`activeFields` → `writableFields`)
- `tools/gsbmcodegen/fixtures/sample/order.go` (new fixture pair)
- `cmd/gsbmschema/main.go` (`--allow-stop-compat-write` flag on diff subcommand)
- `docs/spec.md` §7 (rollback) — append a paragraph describing the compat_write window
- `docs/implementation.md` — short subsection on the lifecycle from the codegen perspective

## Verification

1. `go test ./tools/gsbmschema/...` covers all lifecycle transitions.
2. `go test ./tools/gsbmcodegen/...` golden tests show both tags emitted for the compat_write fixture.
3. Manual: run `gsbmschema diff` on a synthetic before/after pair representing each transition; check labels and exit codes.
4. Spec self-check: re-read `docs/spec.md` §7 after the rollback paragraph lands, confirm no contradictions with the existing forward/backward compatibility rules.
