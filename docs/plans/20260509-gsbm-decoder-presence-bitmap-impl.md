# gsbm: per-struct decoder presence bitmap (sidecar layout)

## Overview

Emit a per-struct presence bitmap so caller code can distinguish **field missing on the wire** from **field present with the zero value of its type**. Today the decoder writes a missing field as the Go zero value and there is no way for downstream logic to tell the two cases apart. That blocks any migration logic that needs to fall back from a new tag to an old tag only when the new tag was absent — a common shape during phased replacements.

The bitmap is implemented as a **sidecar map** keyed on the receiver pointer (`map[*T]presenceMask`), populated by `UnmarshalGSBM` and looked up by a generated `FieldPresent(tag uint32) bool` method. The receiver Go struct is not modified — handwritten field offsets stay stable.

This addresses review note §22 (field presence during decode).

## Context

- Spec §3 / §7.2 says missing tags decode as the type's zero value. That is correct on the wire. The gap is at the Go API layer.
- The current generated decoder (`tools/gsbmcodegen/emit.go:emitUnmarshal`) does not record which tags it actually consumed. Ground truth is lost the moment the switch case writes the value.
- Sidecar layout (chosen): `map[*T]presenceMask` indexed by receiver pointer. Adds a `sync.Map` lookup per `FieldPresent` call but keeps the user struct untouched.
- Wire format does **not** change. This is a Go-API-only feature.
- Adopted from design plan `docs/plans/20260509-gsbm-decoder-presence-bitmap.md`. The "in-struct field" alternative was considered and rejected (too invasive on user types).

## Development Approach

- Testing approach: regular (write tests after each Task)
- Build the bitmap unconditionally first (every field gets a bit) so the codegen change is one diff; the optional `//gsbm:presence` annotation is reserved as a future opt-in but not implemented in this plan
- Land in three commits: runtime sidecar primitive, codegen emit, end-to-end fixture/test
- Complete each Task fully before moving to the next
- Update this plan when scope changes during implementation

## Testing Strategy

- Round-trip test: encode a struct with a known-zero field set, decode, assert `FieldPresent(tag) == true`. Then re-encode without writing that tag, decode, assert `FieldPresent(tag) == false` even though the Go field is the zero value.
- Skip-aware test: encode with an unknown tag, decode, assert known-tag presence bits are unaffected and the unknown-tag bit position is `false`.
- Reset test: after `Reset()`, `FieldPresent` returns `false` for every tag.
- Allocation test: `testing.AllocsPerRun` confirms the sidecar adds at most a bounded one-time map insert per receiver, no per-call allocations.
- Pool test: a `*T` reused via `sync.Pool` does not leak presence state from a previous decode (the sidecar entry must be overwritten or cleared on the next `UnmarshalGSBM`).

## Technical Details

### Sidecar runtime primitive (new file `storage/gsbm/presence_track.go`)

```go
// presenceMask is a fixed-width bit set for tags 1..maxTrackedTag. Tags
// above maxTrackedTag are not tracked (FieldPresent returns false).
type presenceMask [16]uint64 // covers tags 1..1024

// presenceStore is the package-level sidecar. Indexed by *T (any).
var presenceStore sync.Map // map[unsafe.Pointer]*presenceMask
```

API:

- `func MarkPresent(receiver any, tag uint32)` — set the bit for `tag` on the receiver's mask, allocating a fresh mask if none exists.
- `func ClearPresence(receiver any)` — drop the receiver's mask (called by generated `Reset` and at the start of every `UnmarshalGSBM`).
- `func IsPresent(receiver any, tag uint32) bool` — read the bit; returns false on missing receiver, missing mask, or out-of-range tag.

Tags above `maxTrackedTag` (1024) silently no-op on `MarkPresent` and return `false` on `IsPresent`. The codegen warns at generation time if a struct declares a tag above the cap.

### Codegen changes (`tools/gsbmcodegen/emit.go`)

- At the top of `emitUnmarshal`, emit `gsbm.ClearPresence(v)` so a re-decode into the same receiver starts fresh.
- After each successful field decode in the switch body (per known case), emit `gsbm.MarkPresent(v, <tag>)`.
- Emit a `FieldPresent` method per struct: `func (v *T) FieldPresent(tag uint32) bool { return gsbm.IsPresent(v, tag) }`.
- The unknown-tag default branch does **not** call `MarkPresent`.
- `emitReset` adds `gsbm.ClearPresence(v)` so the existing `Reset()` contract (capacity-preserving + presence-clearing) is satisfied.

### Schema annotation (`//gsbm:presence`)

Reserved but **not implemented in this plan**. The parser in `tools/gsbmschema/parse.go` accepts the marker as a no-op so existing handwritten code can start tagging fields; the codegen ignores it. A follow-up plan can flip the default to opt-in if benchmarks justify it.

### Wire format

Unchanged.

## Implementation Steps

### Task 1: Add the sidecar runtime primitive

- [ ] create `storage/gsbm/presence_track.go` with `presenceMask`, package-level `sync.Map`-backed store, and the three exported helpers (`MarkPresent`, `ClearPresence`, `IsPresent`)
- [ ] use `unsafe.Pointer` for the map key so the receiver type does not need to be in the key (avoids interface boxing per call)
- [ ] cap tracked tags at `maxTrackedTag = 1024` (16 × `uint64`); higher tags no-op silently
- [ ] write tests in `storage/gsbm/presence_track_test.go`: bit set/get round-trip; clear empties the mask; unknown receiver returns false; out-of-range tag returns false; concurrent MarkPresent on different receivers is race-free (run with `-race`)
- [ ] run project tests - must pass before next task

### Task 2: Wire the bitmap into codegen

- [ ] in `tools/gsbmcodegen/emit.go:emitUnmarshal`, prepend `gsbm.ClearPresence(v)` before the decode loop
- [ ] in each known-tag switch case, emit `gsbm.MarkPresent(v, <tag>)` after the successful field write
- [ ] emit `func (v *T) FieldPresent(tag uint32) bool { return gsbm.IsPresent(v, tag) }` for every generated struct (root and nested)
- [ ] in `emitReset`, append `gsbm.ClearPresence(v)` so handwritten callers' expectations of `Reset()` clearing all decode state hold
- [ ] add a generation-time warning when a struct's max declared tag exceeds `gsbm.MaxTrackedTag`
- [ ] regenerate the existing fixture goldens via `REGEN_GOLDEN=1 go test ./tools/gsbmcodegen/ -run TestRegenGolden`
- [ ] write tests confirming the regenerated goldens contain the new `MarkPresent`/`FieldPresent` lines for at least one fixture
- [ ] run project tests - must pass before next task

### Task 3: End-to-end fixture coverage

- [ ] add a fixture test in `tools/gsbmcodegen/fixtures/sample/sample_test.go`: round-trip the existing `Order` fixture, assert `FieldPresent(tag)` is true for every tag the encoder wrote
- [ ] add a missing-tag test: write a partial blob omitting one tag, decode, assert `FieldPresent(omittedTag) == false` and `FieldPresent(presentTag) == true`
- [ ] add a present-but-zero test: write a tag whose value is the type's zero value, decode, assert `FieldPresent(tag) == true` even though the Go field equals its zero value
- [ ] add a reset test: decode → call `Reset()` → assert every `FieldPresent` returns `false`
- [ ] add a `sync.Pool` reuse test: decode a value, return to pool, decode a different blob into the same `*T`, assert old presence state is gone
- [ ] add a `testing.AllocsPerRun` test confirming the sidecar adds at most a bounded one-time map insert per receiver
- [ ] run project tests - must pass before next task

### Task 4: Document the API and reserve the annotation

- [ ] add a subsection to `docs/implementation.md` describing `FieldPresent`, when to use it (migration fallback logic), the sidecar layout, and the `MaxTrackedTag` cap
- [ ] add a no-op parser path for `//gsbm:presence` in `tools/gsbmschema/parse.go` so handwritten code can start using the marker; document it as reserved for a future opt-in toggle
- [ ] cross-check that the spec is unchanged (this is a Go-API feature, not a wire-format feature) — no edit to `docs/spec.md` should be needed
- [ ] run project tests - must pass before next task

### Task 5: Verify acceptance criteria

- [ ] verify all requirements from Overview are implemented: `FieldPresent` method on every generated struct; sidecar map populated by decode; cleared by Reset; no wire-format change; user struct unchanged
- [ ] run full project test suite, including `-race`
- [ ] run project linter - all issues must be fixed
- [ ] confirm the design doc `docs/plans/20260509-gsbm-decoder-presence-bitmap.md` matches the implemented behavior; update or move to `docs/plans/completed/` per project convention

## Post-Completion

*Items requiring manual intervention - no checkboxes, informational only*

- Sidecar `sync.Map` retains an entry per ever-decoded receiver pointer until the receiver is GC'd. For pooled receivers this is bounded by pool size; for ad-hoc allocations the entry is collected with the receiver. Monitor sidecar size in production via `runtime.MemStats` or a small expvar counter if needed.
- The reserved `//gsbm:presence` annotation can be promoted to opt-in in a follow-up plan if a hot type shows measurable overhead from unconditional bitmap maintenance.
