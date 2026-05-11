# support anonymous embedded fields via flattening policy

## Overview

Replace the current hard rejection of anonymous embedded fields with a **flattening policy**: when struct `Outer` embeds `Inner`, all of `Inner`'s `bin:`-tagged fields are flattened into `Outer`'s tag space. Tag collisions across the embed boundary are detected and reported with a clear diagnostic. The wire format is exactly as if the user had written `Outer` with the embedded fields inlined manually.

Resolves [issue #11](https://github.com/flaticols/gsbm/issues/11). Policy locked-in: **flatten** (the option the issue lists first and the closest match to how Go itself treats embedding).

## Context

- Current rejection: `tools/gsbmschema/discover.go` raises `field/anonymous` for any anonymous (embedded) struct field; documented at line ~278 in the file.
- Spec §3 already requires unique tags within a struct; the flattening policy reuses that uniqueness check across the embed boundary.
- The wire format is unchanged: an `Outer` with an embedded `Inner` encodes byte-identically to a hand-flattened `Outer` carrying `Inner`'s tags directly. Schema snapshot records the embedded type as a `flattened_from` reference so users see where each tag originated.
- The two rejected alternatives (require explicit `bin:"N"` on the embed; keep rejection + better diagnostic) are documented in the issue and not implemented here.

## Development Approach

- **Testing approach**: Regular — implement validator flattening, then codegen, then snapshot/classifier.
- Land in three commits matching the three Tasks.
- Update this plan when scope changes during implementation.

### CRITICAL: do not commit the plan file to the branch

`docs/` contains only `spec.md` going forward. Stage files individually; never `git add -A`. If a plan lands in a commit, `git rm --cached docs/plans/**` and amend.

### CRITICAL: do not fix code or tests to make tests green

Investigate root cause. Production wrong → fix production; test wrong → cite spec, then fix. Forbidden: loosening assertions, swapping `errors.Is` for `err != nil`, swallowing errors, `t.Skip`, regenerating goldens to dodge hand-written tests. Surface `⚠️` blockers.

## Testing Strategy

- New fixture package `tools/gsbmcodegen/fixtures/embed/` with `Base{Total int64 \`bin:"1"\`}` and `Extended{Base; Reason string \`bin:"2"\`}`. Round-trip test: encoding `Extended{Base: Base{Total: 42}, Reason: "x"}` produces the same bytes as a hand-flattened `Flat{Total int64 \`bin:"1"\`; Reason string \`bin:"2"\`}`.
- Collision-detection test: embedding `Base{X int64 \`bin:"1"\`}` into `Outer{Base; Y int64 \`bin:"1"\`}` produces a `field/tag-collision` issue naming both fields and the embed boundary.
- Multi-level embedding test: `A` embeds `B` embeds `C` — all of `C`'s tags appear at `A`'s top level; collisions across any pair of levels are detected.
- Pointer-embed test: `Outer{*Base}` — define behavior (recommendation: accept; the nil-pointer case means all flattened fields are absent on the wire, decoded as zero — same as the "tag not present" rule from spec §7.2).
- Classifier test: moving a tag from a struct into an embedded base (or vice versa) is `safe` as long as the tag, type, and wire shape are preserved; the snapshot's `flattened_from` reference changes but the wire bytes don't.

## Progress Tracking

- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document blockers with ⚠️ prefix

## Technical Details

### Validator change

`tools/gsbmschema/discover.go` field-walk:

- When iterating struct fields, detect anonymous fields (`field.Anonymous() == true`).
- For each anonymous field whose type is a named struct, recursively expand its fields and add them to the parent struct's field list with `flattened_from = "<EmbeddedTypeName>"` recorded on each.
- After flattening, run the existing tag-uniqueness check across the combined field set; emit `field/tag-collision` with both field names and the embed type if duplicates appear.
- Pointer-to-struct embedding: same flattening behavior; the codegen emits a nil-check on encode (skip all flattened fields if pointer is nil) and a zero-value materialization on decode (allocate the embedded struct if any of its tags are seen).
- Non-struct embedding (e.g., embedding a named string) — reject with a clearer diagnostic; only struct embeds flatten.

### Codegen change

`tools/gsbmcodegen/emit.go`: the `MarshalGSBM`/`UnmarshalGSBM` for the outer struct accesses flattened fields via `v.<EmbeddedName>.<FieldName>` (Go's promotion already gives `v.<FieldName>` directly, but we use the unambiguous explicit path to avoid name-resolution surprises across multi-level embeds with shadowed names).

For pointer embeds:

- Encode: skip flattened fields if `v.<EmbeddedName> == nil`.
- Decode: lazily allocate `v.<EmbeddedName> = &EmbeddedType{}` on the first incoming tag belonging to the embedded type.

### Snapshot + classifier

Add `FlattenedFrom string` to the snapshot's field entry. Classifier:

- Same tag, same type, `FlattenedFrom` field changes (e.g., previously direct, now from embed) → `safe` as long as wire shape stays identical.
- `FlattenedFrom` reference points to a type that itself was modified — caught by the underlying field comparison; not a new code.

## Implementation Steps

### Task 1: Validator flattens anonymous embedded struct fields

- [ ] in `tools/gsbmschema/discover.go`, locate the `field/anonymous` rejection (around line 278); replace with the flattening expansion described in Technical Details
- [ ] flattening is recursive: an embedded struct that itself embeds another struct contributes both layers' tagged fields
- [ ] preserve rejection for non-struct anonymous fields (embedding a named primitive); use a clearer diagnostic `field/anonymous-non-struct` pointing at the rewrite (use a named field)
- [ ] add tag-uniqueness check across the flattened field set; emit `field/tag-collision` naming both the parent field and the embedded field
- [ ] write validator tests: simple embed accepted with flattened field list; collision detected; multi-level embed flattened; non-struct embed rejected with the new diagnostic
- [ ] run project tests - must pass before next task

### Task 2: Codegen accesses flattened fields and handles pointer embeds

- [ ] in `tools/gsbmcodegen/emit.go`, route flattened fields through `v.<EmbeddedName>.<FieldName>` rather than relying on Go field promotion (avoids name-shadow surprises)
- [ ] add encode path for pointer embeds: nil-check the embedded pointer and skip flattened fields if nil
- [ ] add decode path for pointer embeds: lazily allocate the embedded struct on first matching tag; on encode, a present embed with all-zero flattened fields still emits each tag (same as the normal field rule)
- [ ] add fixture package `tools/gsbmcodegen/fixtures/embed/` with value-embed, pointer-embed, and multi-level-embed cases
- [ ] commit goldens via `REGEN_GOLDEN=1`
- [ ] write round-trip tests: byte-equality with a hand-flattened baseline; pointer embed with nil round-trips; pointer embed with zero-value embedded struct round-trips
- [ ] run project tests - must pass before next task

### Task 3: Schema snapshot records `flattened_from`; classifier handles the case

- [ ] extend the snapshot's field entry with `FlattenedFrom string` in `tools/gsbmschema/snapshot.go`
- [ ] update `classifier.go` to compare flattened-from references; same tag + same type + only `FlattenedFrom` differs → `safe` (this allows refactoring a struct to push fields into an embedded base without breaking the wire)
- [ ] regenerate `schema_snapshot.json` files across `tools/gsbmcodegen/fixtures/`
- [ ] write classifier tests: rename a direct field to an embed-flattened one with the same tag → safe; collision after refactor → breaking
- [ ] run project tests - must pass before next task

### Task 4: Verify acceptance criteria

- [ ] verify all requirements from Overview are implemented: anonymous embedded struct fields flatten into the outer struct; collisions are detected; pointer embeds work; multi-level embeds work; snapshot records the flattened-from origin; classifier handles flatten-vs-direct refactors
- [ ] run `go test ./... -count=1`; all green
- [ ] run `go vet ./...` and `go build ./...`; clean
- [ ] update `docs/spec.md` §3 or §5 with a short subsection documenting the flattening policy (encoding is byte-identical to manual flattening; tags must be unique across the embed boundary)
- [ ] close out: comment on issue #11 with the merge commit

## Post-Completion

*Items requiring manual intervention - no checkboxes, informational only*

- The flattening policy is a design choice. If a future use case wants the embed to encode as a nested length-delim field (the rejected alternative #2), that becomes a separate annotation, not a global policy change — to avoid breaking existing flatten-based schemas.
- Snapshot gained a new optional field; hash bump is one-time. Downstream consumers re-run `gsbmschema snapshot` after upgrading.
