# support pointer elements and named collection aliases

## Overview

Extend the schema validator and codegen to accept two common storage-graph shapes that are currently rejected:

- `[]*T` — slice of pointer-to-tagged-struct (nil elements preserved on the wire via the existing presence-byte mechanism)
- `type ItemList []Item` — named slice alias; same wire shape as the underlying slice, but the schema snapshot records the named type so type changes are still visible

And by extension, named slice aliases over pointer elements: `type ItemList []*Item`.

Resolves [issue #8](https://github.com/flaticols/gsbm/issues/8).

## Context

- Today's validator rejects `[]*T` at `tools/gsbmschema/discover.go` with `slice element *... is not supported`, and rejects `type ItemList []Item` with `named type ... has unsupported underlying []...`.
- Spec §5 already covers slice encoding (`<length> <count> <elements>` value-only); pointer elements per spec §5.1 use LENGTH_DELIM with a presence-byte payload regardless of underlying type. The wire format already accommodates the proposed shapes — only the validator and codegen need updating.
- Named slice aliases must round-trip with byte-identical wire to their underlying slice. The named identity is for schema diffing only (change `ItemList` underlying from `[]Item` to `[]Other` → breaking).
- Existing fixtures `tools/gsbmcodegen/fixtures/sample/order.go` use slice-of-non-pointer (`[]Item`) and `tools/gsbmcodegen/fixtures/graph/section.go` use slice-of-pointer (`[]*Item`); the graph fixture proves the wire mechanics exist but doesn't exercise the named-alias case.

## Development Approach

- **Testing approach**: Regular — extend validator + codegen, then add fixtures and tests.
- Land in three commits matching the three implementation Tasks.
- Update this plan when scope changes during implementation.

### CRITICAL: do not commit the plan file to the branch

`docs/` contains only `spec.md` going forward. Stage files individually; never `git add -A`. If a plan lands in a commit, `git rm --cached docs/plans/**` and amend.

### CRITICAL: do not fix code or tests to make tests green

Investigate root cause. Production wrong → fix production; test wrong → cite spec/runtime contract, then fix. Forbidden: loosening assertions, swapping `errors.Is` for `err != nil`, swallowing errors, `t.Skip`, regenerating goldens to dodge hand-written tests. Surface `⚠️` blockers rather than papering over.

## Testing Strategy

- Two new fixture struct families:
  1. `Batch` with `Items []*Item` (slice of pointer) and `Optional []*OptionalNote` (slice of pointer to a builtin-primitive-bearing struct).
  2. `Catalog2` with `Groups ItemList` where `type ItemList []Item`, plus `Optionals ItemPtrList` where `type ItemPtrList []*Item`.
- Round-trip tests: encode → decode → DeepEqual round-trip. Plus a byte-equality test comparing the named-alias wire output to the underlying-slice baseline.
- Nil-element preservation test: `Items := []*Item{{...}, nil, {...}}` round-trips with the nil in the middle position preserved.
- Classifier test: changing `type ItemList []Item` to `type ItemList []OtherItem` is breaking.

## Progress Tracking

- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document blockers with ⚠️ prefix

## Technical Details

### Validator change

`tools/gsbmschema/discover.go` slice-element check: currently rejects `*types.Pointer` element types. Accept when the pointee is a named struct in the schema closure. Mirror the spec §5.1 nullable handling: each element is encoded as a length-delim envelope with a presence-byte.

Named slice aliases: walk the alias to its underlying `*types.Slice` and process as if the field were declared with the underlying type — but record the named type in the schema model so the snapshot/classifier sees it.

### Codegen change

`tools/gsbmcodegen/emit.go`:

- Slice-of-pointer encode: emit a loop where each iteration writes a length-delim envelope containing the presence byte and the element body (if non-nil). Same pattern as the existing nullable-field encode at `emitOptionalEncode`.
- Slice-of-pointer decode: emit a loop where each iteration reads the length-delim, reads the presence byte, allocates `*T` and decodes into it on `PresenceNonZero`; sets nil on `PresenceNil`.
- Named slice alias: the field's wire path uses the underlying slice's emit code; the Go-side variable name and the `make([]T,...)` call use the alias type so the assignment lands in the right typed slice.

### Schema snapshot

`TypeRef` for a named slice alias already records `PkgPath`+`Name`. Add a sibling `Underlying *TypeRef` (or `UnderlyingSlice *TypeRef`) so the named-alias's underlying element type is captured. Classifier flags a change to the underlying as breaking.

## Implementation Steps

### Task 1: Validator accepts `[]*T` and named slice aliases

- [ ] in `tools/gsbmschema/discover.go`, locate the slice-element rejection (`slice element ... is not supported`); accept `*types.Pointer` element types when the pointee is a named struct in the schema closure
- [ ] locate the named-slice rejection (`named type ... has unsupported underlying []...`); accept when the underlying is a slice whose element type is supported (including pointer-to-struct after the above change)
- [ ] preserve existing rejections for slice-of-interface, slice-of-map (handled by issue #9), slice-of-slice (handled by issue #9)
- [ ] write validator tests: `[]*Item` accepted; `type ItemList []Item` accepted; `type ItemPtrList []*Item` accepted; `[]*int64` (pointer to primitive) — define behavior (reject for v1; not in the spec's optional list); `[]interface{}` still rejected
- [ ] run project tests - must pass before next task

### Task 2: Codegen for `[]*T` and named slice aliases

- [ ] in `tools/gsbmcodegen/emit.go`, add slice-of-pointer encode/decode paths (length-delim per element, presence-byte semantics matching `emitOptionalEncode`/`emitOptionalDecode`)
- [ ] add named-slice-alias support: walk to the underlying element type for wire-emit, but use the alias type for Go-side variable declarations and `make`
- [ ] add a new fixture package `tools/gsbmcodegen/fixtures/aliasptr/` with `Batch{Items []*Item; Groups ItemList; OptionalGroups ItemPtrList}` plus the type defs; commit goldens via `REGEN_GOLDEN=1`
- [ ] write round-trip tests covering: populated slice of all-present pointers; slice with nil interspersed; empty slice; nil slice; named alias round-trip byte-equals the underlying slice form
- [ ] run project tests - must pass before next task

### Task 3: Schema snapshot captures named-alias underlying

- [ ] extend `TypeRef` in `tools/gsbmschema/types.go` with `Underlying *TypeRef` populated for named slice aliases
- [ ] update `snapshot.go` to marshal/unmarshal the new field; default `nil` for non-named types so older snapshots stay readable
- [ ] update `classifier.go` to flag a change in `Underlying` as breaking
- [ ] regenerate `schema_snapshot.json` files across `tools/gsbmcodegen/fixtures/`
- [ ] write classifier tests: rename a named slice alias (same underlying) → safe (rename-preserves-tag); change the underlying element type → breaking
- [ ] run project tests - must pass before next task

### Task 4: Verify acceptance criteria

- [ ] verify all requirements from Overview are implemented: `[]*T` accepted and round-trips with nil-preservation; named slice aliases accepted and byte-equal to underlying baseline; named alias-over-pointer accepted; snapshot records underlying; classifier handles underlying changes
- [ ] run `go test ./... -count=1`; all green
- [ ] run `go vet ./...` and `go build ./...`; clean
- [ ] close out: comment on issue #8 with the merge commit

## Post-Completion

*Items requiring manual intervention - no checkboxes, informational only*

- Schema snapshot gained a new optional field on `TypeRef`. Hash will bump once. Downstream consumers re-run `gsbmschema snapshot` after upgrading.
- The decision to reject `[]*int64` (slice of pointer to primitive) is intentional and conservative; revisit if a real use case shows up.
