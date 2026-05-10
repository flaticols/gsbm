# gsbm: compat-write lifecycle for replacement field migrations

## Overview

Add a `compat_write` annotation to the `bin` struct tag and the schema classifier so that replacement field migrations (old field deprecated, new field added with different tag) are rollback-safe end-to-end, not only structurally. During the rollback window, the new code keeps writing the old field alongside the new one; only after the window closes does the encoder stop emitting it. The classifier and codegen enforce the lifecycle so this can't drift into a README rule.

This addresses review note §6 (rollback safety is overstated). The current append-only policy is structurally rollback-safe — old readers skip unknown new tags, new readers see zero for unread old tags — but if new code stops writing an old field outright, a rollback to old code will see the field disappear from a business-data perspective.

## Context

- Current schema state model is binary: a field is **active** (written + read) or **deprecated** (still readable from historical records, not written by new code). There is no in-between.
- `tools/gsbmschema/classifier.go` produces `safe | warning | breaking` labels. Replacement-style migrations land as a pair: `field/added` (new tag, safe) and `field/deprecated` (old tag, safe). The classifier doesn't know the two are linked.
- `tools/gsbmcodegen/emit.go:activeFields` filters out deprecated fields from `MarshalGSBM` — so the moment a field flips to deprecated, the encoder stops writing it.
- Production rollback windows in this org are 24-72h (per `docs/spanner-notes.md`). A field that disappears from new writes during that window is effectively gone from any rollback-decoded record.
- Adopted from design plan `docs/plans/20260509-gsbm-compat-write-lifecycle.md`.

## Development Approach

- Testing approach: regular (write tests after each Task; no TDD)
- Land in three small commits matching the three Tasks: tag-parser change, classifier states, codegen emit
- Complete each Task fully before moving to the next; later Tasks consume earlier ones (codegen needs the new `FieldDecl.CompatWrite` from the parser; classifier needs the parser too)
- Update this plan when scope changes during implementation
- Forward-only lifecycle: do not retro-link historical deprecated fields

## Testing Strategy

- Unit tests for the tag parser (accepts `compat_write` only with `deprecated`, rejects standalone)
- Table-driven classifier tests over before/after schema pairs covering each lifecycle transition (active → compat_write → deprecated → reserved) and the illegal jumps
- Codegen golden test on a fixture struct (renamed-field pair) confirming both tags are present on the wire while the old field is in the `compat_write` state
- Property test: a record written by the new (compat_write) encoder MUST decode under the old schema (still-deprecated field path) without losing the field's value
- Run project tests after each Task before proceeding

## Technical Details

### Lifecycle states

```
active                    bin:"7"
deprecated, compat_write  bin:"7,deprecated,compat_write"   ← new state
deprecated                bin:"7,deprecated"
reserved                  //gsbm:reserved 7
```

State transitions allowed by the classifier:

- `active → deprecated,compat_write` — safe (field is being phased out; encoder still writes it).
- `deprecated,compat_write → deprecated` — safe **only** when the diff command is invoked with `--allow-stop-compat-write` (mirrors `--allow-breaking`; the classifier cannot enforce calendar time, so it requires the explicit ack instead).
- `deprecated → reserved` — safe (existing rule, unchanged).
- `active → deprecated` (skipping compat_write) — **warning**, with the recommendation to land compat_write first. Allowed for additive-only fields where no rollback risk exists, but the classifier surfaces the skip so reviewers see it.

### Tag parser changes (`tools/gsbmschema/parse.go`)

The `bin:"N,deprecated,compat_write"` token list extends `parseFieldTag` with a new boolean. `compat_write` is only valid in combination with `deprecated`; the parser rejects standalone `compat_write` as an unknown qualifier.

### Classifier changes (`tools/gsbmschema/classifier.go`)

- New `FieldDecl.CompatWrite bool` populated by the parser.
- New change codes: `field/compat-write-added`, `field/compat-write-removed`.
- Update `compareField` so the active → compat_write → deprecated trajectory is recognized as a coherent lifecycle, not three independent edits.
- Diff command grows `--allow-stop-compat-write` flag mirroring `--allow-breaking` for the compat_write → deprecated transition.

### Codegen changes (`tools/gsbmcodegen/emit.go`)

- `activeFields` becomes `writableFields`: returns active fields plus deprecated-with-compat_write.
- Decoder unchanged — deprecated and deprecated-with-compat_write fields decode identically. The codegen's existing `FieldDecl.Deprecated` already gates whether the decode path stores the value.

## Implementation Steps

### Task 1: Extend the tag parser to recognize `compat_write`

- [x] add a `CompatWrite bool` field to `tools/gsbmschema/types.go:FieldDecl`
- [x] extend `parseFieldTag` in `tools/gsbmschema/parse.go` to recognize the `compat_write` qualifier in `bin:"N,deprecated,compat_write"`
- [x] reject `compat_write` when `deprecated` is absent — return a clear validation error
- [x] write tests in `tools/gsbmschema/parse_test.go` covering: accepts the combined form; rejects standalone `compat_write`; rejects `compat_write` repeated; preserves the existing `bin:"N,deprecated"` behavior unchanged
- [x] run project tests - must pass before next task

### Task 2: Wire `compat_write` into the schema classifier

- [x] add change codes `field/compat-write-added` and `field/compat-write-removed` to `tools/gsbmschema/classifier.go`
- [x] update `compareField` to treat `active → compat_write → deprecated` as a recognized lifecycle (not three independent diff entries) and emit the new change codes with label `safe`
- [x] gate the `compat_write → deprecated` transition behind a new `--allow-stop-compat-write` CLI flag in `cmd/gsbmschema/main.go`, mirroring the existing `--allow-breaking` plumbing; without the flag the transition is `breaking`
- [x] downgrade `active → deprecated` (skipping compat_write) to label `warning` with a recommendation to land compat_write first
- [x] write table-driven tests in `tools/gsbmschema/classifier_test.go` for each lifecycle transition and each illegal jump
- [x] write tests for the new CLI flag's effect on diff exit code
- [x] run project tests - must pass before next task

### Task 3: Update codegen to dual-write `compat_write` fields

- [x] rename `activeFields` to `writableFields` in `tools/gsbmcodegen/emit.go` and broaden it to include deprecated-with-compat_write fields
- [x] confirm the decoder path stays correct: deprecated and deprecated-with-compat_write fields decode identically (the existing `Deprecated` gate already handles store-or-skip)
- [x] add a renamed-field fixture pair to `tools/gsbmcodegen/fixtures/sample/` (e.g., `Order.LegacyCode` becoming `Order.RetailCode` with `LegacyCode` in the `compat_write` state)
- [x] regenerate the golden files for the touched fixture via the existing `REGEN_GOLDEN=1 go test ./tools/gsbmcodegen/ -run TestRegenGolden` path
- [x] write a round-trip test that confirms both tags appear on the wire while the old field is in the `compat_write` state
- [x] write a cross-version test: a record written by the compat_write encoder decodes under the old schema (still-deprecated path) without losing the value
- [x] run project tests - must pass before next task

### Task 4: Document the lifecycle in the spec and implementation docs

- [x] append a paragraph to `docs/spec.md` §7 (rollback) describing the compat_write window and citing this lifecycle
- [x] add a short subsection to `docs/implementation.md` covering the lifecycle from the codegen perspective (when to use, how the diff flag works, what the encoder does)
- [x] cross-check that the new spec wording does not contradict §3 (duplicate-tag last-wins) or the existing rollback §7.4 paragraph
- [x] run project tests - must pass before next task

### Task 5: Verify acceptance criteria

- [x] verify all requirements from Overview are implemented: `compat_write` annotation parses, classifier recognizes the lifecycle, codegen dual-writes during the window, diff requires explicit ack to leave the window
- [x] run full project test suite
- [x] run project linter - all issues must be fixed
- [x] confirm the design doc `docs/plans/20260509-gsbm-compat-write-lifecycle.md` matches the implemented behavior; update or move to `docs/plans/completed/` per project convention

**Follow-up coverage (added 2026-05-10 by `docs/plans/20260510-wire-format-evolution-test-suite.md`):** integration coverage for the lifecycle now lives in `tools/gsbmcodegen/fixtures/evolution/compatwrite/`. `TestEvolutionCompatWriteReplace` pins the `active → deprecated, compat_write` transition (`field/compat-write-added`, `safe`); `TestEvolutionCompatWriteStopBakeGate` pins the `deprecated, compat_write → deprecated` exit gate, asserting `field/compat-write-removed` blocks without `--allow-stop-compat-write` and unblocks (severity stays `breaking`, `GateBlocks` flips) with the flag set. The full classifier matrix (add / wire-change / type-change / tag-change / field-removed) sits in the same package as the surrounding contract.

## Post-Completion

*Items requiring manual intervention - no checkboxes, informational only*

- The `--allow-stop-compat-write` flag is operator-facing; team should agree on a deploy-bake SLO before invoking it (recommended: two full rollback windows have elapsed since the field flipped to compat_write).
- Existing deprecated fields are not retro-linked — only fields tagged `compat_write` after this lands participate in the lifecycle.
