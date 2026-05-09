# gsbm: per-struct decoder presence bitmap

## Overview

Emit an internal presence bitmap in generated `UnmarshalGSBM` so caller code can distinguish **field missing on the wire** from **field present with the zero value of its type**. Today the decoder writes a missing field as the Go zero value and there is no way for downstream logic to tell the two cases apart. That blocks any migration logic that needs to fall back from a new tag to an old tag only when the new tag was absent — a common shape during phased replacements.

This addresses review note §22 (field presence during decode).

## Context

- Spec §3 / §7.2 says missing tags decode as the type's zero value. That is correct on the wire. The gap is at the Go API layer.
- The current generated decoder (`tools/gsbmcodegen/emit.go:emitUnmarshal`) does not record which tags it actually consumed. Ground truth is lost the moment the switch case writes the value.
- A subset of fields (those involved in fallback or migration logic, not all fields) need presence tracking. Generating a presence bit for every field is feasible (a per-struct `[N/64]uint64` is cheap) but the API surface is the harder question — exposing it requires either a parallel method, an extension interface, or an annotation that opts a field in.
- Schema annotation `//gsbm:presence` (per-field) would scope cost and surface area to the fields that actually need it. The wire format does not change.

## Development Approach

- Build the bitmap unconditionally first (every field gets a bit) so the codegen change is one diff. Then iterate: if benchmarks show overhead on hot paths, add the `//gsbm:presence` annotation to opt fields out (or in, if the default flips).
- API surface: a generated `func (v *T) FieldPresent(tag uint32) bool` method on every root and nested struct that has fields. Keeps it explicit and avoids leaking the bitmap layout.

## Testing Strategy

- Round-trip test: encode a struct with a known-zero field set, decode, assert `FieldPresent(tag) == true`. Then re-encode without writing that tag, decode, assert `FieldPresent(tag) == false` even though the Go field is the zero value.
- Skip-aware test: encode with an unknown tag, decode, assert known-tag presence bits are unaffected and the unknown-tag bit position is `false`.
- Allocation test: `testing.AllocsPerRun` confirms the bitmap is on the receiver (no per-decode allocation).

## Technical Details

### Bitmap layout

A `[ceil(N/64)]uint64` field on the receiver, named `_gsbmPresent`, where `N` is the highest declared tag in the struct. Stored at the end of the struct so handwritten field offsets are unaffected. The codegen warns when `N > 1024` (for very wide structs) and suggests `//gsbm:presence` opt-in instead.

Wait — adding a field to the user's handwritten struct is invasive. Alternative: emit presence as a sidecar `map[*T]presenceMask` indexed by receiver pointer, populated by `UnmarshalGSBM` and looked up by `FieldPresent`. Adds a sync.Map lookup per call but keeps the user struct untouched. **Decision deferred to implementation; the sidecar is the safer default.**

### API

```go
// Generated alongside MarshalGSBM/UnmarshalGSBM/Reset:
func (v *T) FieldPresent(tag uint32) bool
```

Returns `true` iff the most recent `UnmarshalGSBM` call consumed `tag`. Reset by `Reset()` and by the next `UnmarshalGSBM`. Returns `false` for tags above the schema's highest tag, for unknown tags, and on a fresh / not-yet-decoded receiver.

### Codegen changes (`tools/gsbmcodegen/emit.go`)

- In `emitUnmarshal`, after a successful field decode, set the corresponding bit.
- Emit the `FieldPresent` method per struct.
- The unknown-tag default branch does not touch the bitmap.

### Schema validation

- New annotation `//gsbm:presence` is a no-op for now (the bitmap is unconditional in v1 of this feature). Reserved as the opt-in escape hatch if the unconditional default proves expensive on a hot type.

### Wire format

Unchanged. This is a Go-API-only feature; the bitmap exists only in memory.

## Critical files

- `tools/gsbmcodegen/emit.go` (presence bitmap generation, `FieldPresent` method)
- `tools/gsbmcodegen/codegen.go` (top-level emit driver)
- `tools/gsbmcodegen/fixtures/sample/sample_test.go` (presence tests)
- `tools/gsbmschema/parse.go` (`//gsbm:presence` parser, even if a no-op)
- `docs/implementation.md` (new subsection on the FieldPresent API and when to use it)

## Verification

1. `go test ./tools/gsbmcodegen/...` and `./tools/gsbmcodegen/fixtures/sample/` cover the round-trip presence cases.
2. Allocation test confirms the sidecar approach does not introduce per-decode allocations beyond a bounded one-time map insert.
3. Manual: read a generated `FieldPresent` body to confirm the bitmap is computed from consumed-tag bits, not from value-non-zero checks (those are not the same thing).
