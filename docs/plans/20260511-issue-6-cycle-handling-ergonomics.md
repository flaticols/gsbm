# improve cycle handling ergonomics for recursive domain graphs

## Overview

Make cycle-break handling easy to apply, verify, and review. Today the validator detects a closure cycle and suggests `//gsbm:cycle_break_via_id` on a field along the cycle, but: the marker is a comment that's easy to miss in review; the diagnostic doesn't always identify the shortest actionable field path; there's no committed fixture showing what the resulting wire shape looks like; the schema snapshot doesn't make "this field is encoded by ID, not inline" obviously diff-visible.

This plan addresses all four gaps:

- Document the exact `//gsbm:cycle_break_via_id` placement rule and the generated wire shape in `docs/spec.md`.
- Add a `bin:"N,id_ref"` field-tag alternative to the comment marker so the decision lives next to the tag, where reviewers expect to see wire-format choices.
- Add a small committed fixture demonstrating a recursive graph break (previous/current pattern) with round-trip tests.
- Improve diagnostics so the shortest cycle-break-candidate field path is named in the error message.
- Make schema snapshot record the cycle-break attribute so switching between inline and reference encoding is classified as wire-affecting.

Resolves [issue #6](https://github.com/flaticols/gsbm/issues/6).

## Context

- Current marker: `//gsbm:cycle_break_via_id` comment on a field, parsed at `tools/gsbmschema/parse.go` and used at `tools/gsbmschema/validate.go` (or `discover.go`) to break the cycle in the type closure graph.
- The marker today causes the validator to accept the graph but I'm not sure (worth checking during Task 1) whether codegen actually emits ID-reference encoding or just permits the type. Plan calls out: confirm codegen wires up the ID encoding; if it doesn't yet, that's part of Task 2.
- Spec §5 doesn't currently describe ID-reference encoding. Need to add a subsection.
- Diagnostic improvement: when a cycle of N fields exists, the validator should suggest the field with the **smallest tag** among cycle members (deterministic + likely the "anchor" field by convention), or the field whose name contains "previous"/"parent"/"ref" (heuristic).

## Development Approach

- **Testing approach**: Regular — improve diagnostic + add tag-option syntax + fixture + spec doc.
- Land in four commits: tag-option parser, codegen wire shape, diagnostic improvement, doc + snapshot.
- The `//gsbm:cycle_break_via_id` comment marker stays supported (back-compat); the new `bin:"N,id_ref"` tag option is an alias that does the same thing.
- Update this plan when scope changes during implementation.

### CRITICAL: do not commit the plan file to the branch

`docs/` contains only `spec.md` going forward. Stage files individually; never `git add -A`. If a plan lands in a commit, `git rm --cached docs/plans/**` and amend.

### CRITICAL: do not fix code or tests to make tests green

Investigate root cause. Production wrong → fix production; test wrong → cite spec, then fix. Forbidden: loosening assertions, swapping `errors.Is` for `err != nil`, swallowing errors, `t.Skip`, regenerating goldens to dodge hand-written tests. Surface `⚠️` blockers.

## Testing Strategy

- Fixture package `tools/gsbmcodegen/fixtures/cyclebreak/`: `Item{ID string \`bin:"1"\`; Label string \`bin:"2"\`; Previous *Item \`bin:"3,id_ref"\`}`. Demonstrates the previous-link pattern from the issue. Round-trip test confirms `Previous` round-trips as the ID string, not as an inline `*Item` body.
- Diagnostic test: a 3-node cycle (`A → B → C → A`) without any break marker produces a diagnostic naming the shortest break-candidate path (the lowest-tag field along the cycle).
- Snapshot test: removing `id_ref` from a field (switching back to inline encoding) is classified as wire-affecting `breaking`.
- Spec-doc round-trip test: encode → decode → DeepEqual on the cyclebreak fixture; assert the wire bytes match the documented shape from the new spec subsection.

## Progress Tracking

- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document blockers with ⚠️ prefix

## Technical Details

### Tag-option parser

In `tools/gsbmschema/parse.go`, extend `parseFieldTag` to accept `id_ref` alongside `deprecated` and `compat_write`. Stored as `FieldDecl.CycleBreakViaID bool` — same boolean the comment marker already sets, so downstream code is unchanged. Both forms set the same flag; the comment is the legacy syntax, the tag option is the new preferred form.

### Codegen for id-ref fields

Confirm in Task 2 that the codegen for an `id_ref` field actually emits ID-reference encoding (encode the referenced struct's ID field as a string/int, not the whole body). If not yet wired:

- The `id_ref` field must point at a struct (or pointer to struct) that has a designated "ID" field. Convention: the field tagged `bin:"1"` is the ID. Document this convention.
- Encode: write the referenced struct's `bin:"1"` field value using its wire type (VARINT for int IDs, LENGTH_DELIM for string IDs).
- Decode: read the ID, look it up in a caller-provided resolver table, or store the ID-only and let the caller hydrate. v1 ships ID-only (no resolver) — keeps the runtime simple, matches the issue's "reference encoding" framing.

### Diagnostic improvement

In `tools/gsbmschema/validate.go` (or wherever the cycle is detected), when emitting the `type/cycle` diagnostic, include:

- the full cycle (today's behavior),
- the **shortest break candidate**: the field along the cycle with the lowest tag,
- alternative candidates by name heuristic (`Previous`, `Parent`, `Ref`).

Example new diagnostic shape:

```
type/cycle: cycle in closure: [Item.Previous → Item.Previous → ...]
suggested break: Item.Previous (tag 3) — add `id_ref` to the bin tag or //gsbm:cycle_break_via_id comment
```

### Spec doc

`docs/spec.md` gets a new subsection (under §5, before §6 Root struct encoding) documenting the cycle-break wire shape: ID-reference fields are encoded using the referenced struct's `bin:"1"` field's wire type, treated as a leaf scalar. Skip-safety and round-trip rules apply unchanged.

### Snapshot + classifier

Add `CycleBreakViaID bool` to the snapshot's field entry. Classifier: toggling this flag is wire-affecting `breaking` because the same tag's payload changes from a nested struct body to a leaf scalar.

## Implementation Steps

### Task 1: Add `id_ref` tag option (alias for the comment marker)

- [ ] in `tools/gsbmschema/parse.go`, extend `parseFieldTag` to accept `id_ref`; set `FieldDecl.CycleBreakViaID = true`
- [ ] the comment marker `//gsbm:cycle_break_via_id` continues to work; both forms set the same flag
- [ ] reject combinations that don't make sense: `id_ref` on a non-pointer-to-struct field is malformed (`tag/bad-id-ref`)
- [ ] write parser tests: `bin:"3,id_ref"` accepted; `bin:"3,id_ref,deprecated"` accepted (combinable); `bin:"3,id_ref"` on `string` field rejected
- [ ] write validator tests: a cycle with `id_ref` on the cycle field is accepted (same as today's comment-marker case)
- [ ] run project tests - must pass before next task

### Task 2: Confirm/implement codegen ID-reference encoding

- [ ] read current codegen for `CycleBreakViaID` fields; confirm whether it emits ID-only encoding or just permits the type
- [ ] if not yet wired: add codegen support to encode/decode an `id_ref` field as the referenced struct's `bin:"1"` field value
- [ ] document the convention: the `id_ref` target must have its ID at tag 1; codegen errors with `idref/missing-id-tag` if not
- [ ] add fixture package `tools/gsbmcodegen/fixtures/cyclebreak/` with `Item{ID string \`bin:"1"\`; Label string \`bin:"2"\`; Previous *Item \`bin:"3,id_ref"\`}` and a small driver test
- [ ] commit goldens via `REGEN_GOLDEN=1`
- [ ] write round-trip tests: `Item` with `Previous` round-trips as an ID string; encoded bytes match the documented wire shape; decoded `Previous` is a `*Item` populated only with `ID`, leaving `Label` and `Previous.Previous` as zero values (caller hydrates)
- [ ] run project tests - must pass before next task

### Task 3: Improve cycle diagnostic with shortest-break suggestion

- [ ] in `tools/gsbmschema/validate.go`, locate the `type/cycle` diagnostic; extend the message with: shortest break candidate (lowest-tag field along the cycle), heuristic name candidates (`Previous`/`Parent`/`Ref`)
- [ ] suggest both `bin:"<tag>,id_ref"` and `//gsbm:cycle_break_via_id` in the diagnostic so users see both forms
- [ ] write tests: a 3-node cycle without a marker produces the new diagnostic shape; a 5-node cycle picks the lowest-tag candidate; a cycle whose lowest-tag field is named `Previous` shows it as the recommended candidate
- [ ] run project tests - must pass before next task

### Task 4: Doc + snapshot/classifier integration

- [ ] update `docs/spec.md` with a new subsection under §5 documenting ID-reference encoding (placement of the `id_ref` annotation, the convention that the target's `bin:"1"` is the ID, the wire shape — leaf scalar with the target's ID-field wire type, skip-safety)
- [ ] add `CycleBreakViaID bool` to the snapshot's field entry in `tools/gsbmschema/snapshot.go`
- [ ] update `classifier.go`: toggling `CycleBreakViaID` is wire-affecting `breaking`
- [ ] regenerate `schema_snapshot.json` files across `tools/gsbmcodegen/fixtures/`
- [ ] write classifier tests: adding `id_ref` → breaking; removing `id_ref` → breaking
- [ ] run project tests - must pass before next task

### Task 5: Verify acceptance criteria

- [ ] verify all requirements from Overview are implemented: `id_ref` tag option works; comment marker still works; codegen emits ID-only encoding; diagnostic names the shortest candidate; spec documents the wire shape; snapshot records the attribute; classifier flags toggles as breaking; cyclebreak fixture round-trips
- [ ] run `go test ./... -count=1`; all green
- [ ] run `go vet ./...` and `go build ./...`; clean
- [ ] close out: comment on issue #6 with the merge commit and a link to the new spec subsection

## Post-Completion

*Items requiring manual intervention - no checkboxes, informational only*

- v1 ships ID-only encoding; the caller is responsible for hydrating references after decode. A future enhancement could add a `gsbm.Resolver` interface so the decoder hydrates references inline, but that has lifetime and lookup-cost implications and stays out of scope here.
- The convention that the ID field is at `bin:"1"` is a project rule, not a wire rule. If a real schema needs a different ID tag, an alternative is `id_ref=<tag>` (`bin:"3,id_ref=2"` means the referenced struct's tag-2 field is the ID). Not in v1 of this plan; track as a follow-up if it comes up.
- Snapshot gained a new field. Hash bump is one-time.
