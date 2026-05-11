# support nested composite values in slices and maps

## Overview

Extend the validator and codegen to accept nested composite collection shapes that are currently rejected:

- `map[K][]V` — map of key to slice
- `map[K]map[K2]V` — map of map (two levels deep)
- `[]map[K]V` — slice of map
- Bounded deeper nesting where each level's key/value/element type is itself supported

Resolves [issue #9](https://github.com/flaticols/gsbm/issues/9).

## Context

- Today's rejection sites in `tools/gsbmschema/discover.go`: `map value ... is not supported` and `slice element ... is not supported` when the value/element is itself a collection.
- Spec §5.2 (slices) and §5.3 (maps) already use LENGTH_DELIM with internal length+count framing. Nested composites are length-bounded by their parent's LENGTH_DELIM, so unknown-field skipping stays safe — the wire format already accommodates this. Only the validator and codegen need updates.
- The recursive nesting is intentionally bounded (rule of thumb: 3 levels deep is enough for real storage payloads — `map[K]map[K2][]V` is the practical ceiling). A configurable cap protects against pathological schema graphs and keeps generated code's switch-on-type predictable.

## Development Approach

- **Testing approach**: Regular — extend validator + codegen, then fixtures and tests.
- Land in three commits: validator accepts; codegen emits; nesting-depth cap + diagnostic.
- Update this plan when scope changes during implementation.

### CRITICAL: do not commit the plan file to the branch

`docs/` contains only `spec.md` going forward. Stage files individually; never `git add -A`. If a plan lands in a commit, `git rm --cached docs/plans/**` and amend.

### CRITICAL: do not fix code or tests to make tests green

Investigate root cause. Production wrong → fix production; test wrong → cite spec, then fix. Forbidden: loosening assertions, swapping `errors.Is` for `err != nil`, swallowing errors, `t.Skip`, regenerating goldens to dodge hand-written tests. Surface `⚠️` blockers.

## Testing Strategy

- New fixture package `tools/gsbmcodegen/fixtures/nestedcomp/` with an `Index` struct exposing:
  - `IDsByGroup map[string][]string` — map of slice
  - `LabelsByGroup map[string]map[string]string` — map of map
  - `MetadataVariants []map[string]string` — slice of map
  - `Deep map[string][]map[string]int64` — 3-level nesting at the recursion cap
- Round-trip tests for each: encode → decode → DeepEqual.
- Skip-safety test: encode an `Index` with a known set of fields, hand-craft a blob with an **unknown** tag whose value is a `map[K]map[K2]V` payload, confirm the decoder skips it cleanly using only the outer LENGTH_DELIM length.
- Nesting-depth cap test: a schema with `map[K1]map[K2]map[K3]map[K4]V` (4-deep map) triggers a `type/nesting-too-deep` issue with a clear diagnostic.
- Classifier test: changing `map[string][]string` to `map[string]string` is breaking (wire shape changed).

## Progress Tracking

- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document blockers with ⚠️ prefix

## Technical Details

### Validator change

`tools/gsbmschema/discover.go`: the type classification has a recursion entry already (it descends into struct fields). Apply the same recursion to map values and slice elements when those are themselves collections. Track nesting depth as an integer parameter; cap at 3 with a `type/nesting-too-deep` issue.

### Codegen change

`tools/gsbmcodegen/emit.go`: the existing map encode/decode emits already wrap each entry's value in the appropriate encoding (primitive → varint; string → length-delim with bytes; named-struct → length-delim with body). Extend the dispatch so a composite value type recursively re-enters the emit path. Same for slice elements.

Concretely, three helper paths:

- `emitMapValueEncode(out, kind, valueType)` and `emitMapValueDecode(out, ...)` — already exist; extend the `valueType` switch to handle `*types.Slice` and `*types.Map` by recursive call into `emitSliceEncode` / `emitMapEncode`.
- `emitSliceElementEncode(out, ...)` / `emitSliceElementDecode(out, ...)` — extend similarly.

Each level of nesting emits its own `BeginLengthDelim`/`EndLengthDelim` pair so the wire bytes are correctly bounded.

### Depth cap

Use a constant `MaxNestingDepth = 3` in `tools/gsbmschema/discover.go`. The cap is on COMPOSITE nesting (slice/map), not on struct nesting (which is governed by the cycle-break path).

## Implementation Steps

### Task 1: Validator accepts nested composite values up to the depth cap

- [x] in `tools/gsbmschema/discover.go`, lift the "map value not supported" / "slice element not supported" hard rejections; replace with a recursive type-classification helper that takes a depth parameter
- [x] add `MaxNestingDepth = 3`; emit `type/nesting-too-deep` issue when exceeded, with the field path in the message
- [x] preserve existing rejections for unsupported leaf types (interface, chan, func, …) — those still fail at the leaf, just at any depth now
- [x] write tests: `map[string][]string` accepted; `map[string]map[string]string` accepted; `[]map[string]string` accepted; 3-deep accepted; 4-deep rejected with `type/nesting-too-deep`
- [x] run project tests - must pass before next task

### Task 2: Codegen emits nested composite encode/decode

- [x] in `tools/gsbmcodegen/emit.go`, extend `emitMapValueEncode`/`emitMapValueDecode` to recursively call into `emitSliceEncode`/`emitMapEncode` when the value type is `*types.Slice`/`*types.Map`
- [x] extend `emitSliceElementEncode`/`emitSliceElementDecode` symmetrically
- [x] each nested level uses its own `BeginLengthDelim`/`EndLengthDelim` pair
- [x] add the fixture package `tools/gsbmcodegen/fixtures/nestedcomp/` from Technical Details; commit goldens via `REGEN_GOLDEN=1`
- [x] write round-trip tests for each shape (`map[K][]V`, `map[K]map[K2]V`, `[]map[K]V`, the 3-deep `map[K][]map[K2]V`)
- [x] write a skip-safety test: hand-craft a blob with an unknown tag whose payload is one of the new shapes; decoder must skip via the outer LENGTH_DELIM length without inspecting nested structure
- [x] run project tests - must pass before next task

### Task 3: Schema snapshot captures the full nested shape; classifier flags shape changes

- [x] confirm `TypeRef` in `tools/gsbmschema/types.go` already captures nested composite shape — in this codebase the full nested shape lives in `FieldDecl.Type` (a string built recursively by `shapeOf`), not in `TypeRef`. `TypeRef` is reserved for named-type identity (PkgPath/Name/TypeArgs/Underlying for slice aliases) and intentionally does not mirror composite structure; pinning the recursive `fd.Type` rendering via TestNestedCompositeTypeStringCapturesFullShape covers the "any structural change at any depth surfaces in the snapshot" requirement without a parallel representation
- [x] update `classifier.go` to compare the full nested shape — already in place: the existing `field/type-changed` branch compares `pf.Type` vs `cf.Type` verbatim, and because `shapeOf` renders nested composites recursively into that string, drift at any depth flips it and fires breaking. No code change needed
- [x] regenerate `schema_snapshot.json` files across `tools/gsbmcodegen/fixtures/` — no snapshot artifacts exist in the repo (`MarshalJSON`/`MarshalYAML` are exercised only via `tools/gsbmschema/snapshot_test.go` against in-memory schemas); nothing on disk to regenerate
- [x] write classifier tests: `map[string][]string` → `map[string]string` is breaking; `map[string][]string` → `map[string][]int64` is breaking (leaf type change at depth 2)
- [x] run project tests - must pass before next task

### Task 4: Verify acceptance criteria

- [x] verify all requirements from Overview are implemented: `map[K][]V`, `map[K]map[K2]V`, `[]map[K]V`, 3-deep nesting all accepted and round-trip cleanly; 4-deep rejected with a clear diagnostic; skip-safety holds for nested unknown fields; classifier flags shape changes
- [x] run `go test ./... -count=1`; all green
- [x] run `go vet ./...` and `go build ./...`; clean
- [x] close out: comment on issue #9 with the merge commit (skipped - not automatable; requires merge to exist)

## Post-Completion

*Items requiring manual intervention - no checkboxes, informational only*

- `MaxNestingDepth = 3` is a guard, not a spec rule. Revisit if real schemas hit it — the wire format itself has no depth limit, only the codegen does.
- If a schema needs 4+ levels of composite nesting, the recommendation in the diagnostic should be: introduce a named struct at one of the intermediate levels (`type LabelMap map[string]string`), which trades one level of codegen for a named type that schema-snapshot tracks more precisely anyway.
