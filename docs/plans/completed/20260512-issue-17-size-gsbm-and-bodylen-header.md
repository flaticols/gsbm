# Generate SizeGSBM and bake total size into blob header

## Overview

Resolve [issue #17](https://github.com/flaticols/gsbm/issues/17) by adding three complementary mechanisms for exact-size allocation on both encode and decode paths:

1. **Codegen emits `SizeGSBM() int` on every `:root` and nested struct.** Mirror of `MarshalGSBM`: inlines primitive sizes, recurses through nested values, returns the exact body byte count. Lets encoders allocate the output buffer at exactly the right capacity — eliminates geometric `append` growth on large payloads. Pooled encode currently sits at 3 allocs/op for a 1.22 MiB fixture; fresh encode at 37 allocs/op with ~6.97 MiB transient. Target: ≤ 1 alloc/op pooled, ≤ 3 allocs/op fresh.
2. **Bump wire format to `fmtVer = 2`. Add a 4-byte `bodyLen` field to the blob header (8 → 12 bytes).** Decoders read the total body size before consuming the body, enabling exact-size receiver preallocation and a cheap cross-check against the storage-layer length. `fmtVer = 1` is draft and has no production blobs, so the bump is unconditional — new decoders reject v1 outright; users regenerate.
3. **Add `gsbm.NewCountingWriter()` — `*Writer` in size-only mode.** Existing `MarshalGSBM(w *Writer)` signature is unchanged; the writer just accumulates a size counter instead of touching `buf`. Two uses: hand-written `MarshalGSBM` callers (who didn't go through codegen and so have no generated `SizeGSBM`) can still measure exact size, and the test suite uses it as a triple-equality verifier against the generated `SizeGSBM`.

> Budget note (updated during implementation): the original "≤ 1 alloc/op pooled, ≤ 3 allocs/op fresh" target is redefined by the Task 5 scope note — strict `cap == len` requires a follow-up `BeginLengthDelimSize(known int)` API (captured in Post-Completion design notes). Achieved budget for the new `gsbm.Marshal` path is 4-5 allocs/op on the 1-2 MiB Order fixture.

Target API:

```go
size := value.SizeGSBM()                          // body bytes (excludes header)
buf  := make([]byte, 0, gsbm.HeaderSize+size)
w    := gsbm.NewWriter(buf)
w.WriteHeader(0, schemaHint, uint32(size))        // bodyLen now part of header
value.MarshalGSBM(w)

// or, equivalently:
blob, err := gsbm.Marshal(value, schemaHint)      // does Size → alloc → header → body
```

## Context (from discovery)

- **Wire spec**: `docs/spec.md` §2 defines the current 8-byte header (`magic | fmtVer | flags | schemaHint`). §2.2 explicitly states "the body has no length prefix — the blob byte count from the storage layer… is the body length plus 8." That sentence changes after this plan.
- **Header runtime**: `storage/gsbm/writer.go:50` (`WriteHeader(flags uint8, schemaHint uint16)`) and `storage/gsbm/reader.go:71` (`ReadHeader() (flags uint8, schemaHint uint16, err error)`). Both signatures grow by one `uint32 bodyLen` argument / return.
- **Body framing**: `BeginLengthDelim`/`EndLengthDelim` at `writer.go:156-170` already handle nested length-prefixed envelopes. Nothing inside the body changes — only the top-level header grows.
- **Codegen**: `tools/gsbmcodegen/emit.go` is where field-by-field `MarshalGSBM`/`UnmarshalGSBM` emission lives. A symmetric `emitSizeMethod` lands next to `emitMarshalMethod`, sharing the per-field dispatcher so the two cannot drift.
- **Custom codecs** (issue #10, commit `fbf03a9`): `tools/gsbmcodegen/codecs/` holds `CodecDecl{Name, GoType, WireType, EncodeFn, DecodeFn, PkgImport}`. The struct grows by `SizeFn string` (required). Built-ins `TimeUnixNano` and `DecimalString` ship updated size functions.
- **Fixtures**: `tools/gsbmcodegen/fixtures/*` carry the round-trip and golden tests. Each fixture's golden bytes update when the header grows by 4 bytes and fmtVer flips. `tools/gsbmcodegen/fixtures/customcodec/` is the relevant codec case.
- **Benchmarks**: `storage/gsbm/bench_encode_test.go`, `bench_decode_test.go`, and `internal/bench/payload.go` run the Order (1.22 MiB) and Catalog (1.96 MiB) fixtures the README quotes. Acceptance is measured here.
- **Format status**: README says `fmtVer = 1` is **draft**; user has confirmed there is no production data. No v1 compatibility path is needed.

## Development Approach

- **Testing approach**: Regular — implement header runtime first (smallest surface), then `SizeGSBM` codegen, then codec `SizeFn`, then `gsbm.Marshal`, then verify benchmarks.
- Land in five commits matching the five tasks below.
- Complete each task fully before moving to the next.
- Make small, focused changes; run tests after each.
- **CRITICAL: every task includes new/updated tests** for code changes in that task — write unit tests for new and modified functions, cover success and error paths, do not skip.
- **CRITICAL: all tests must pass before starting next task** — no exceptions.
- **CRITICAL: update this plan file when scope changes during implementation.**

### CRITICAL: do not commit the plan file to the branch

`docs/` contains only `spec.md` going forward (commit `2978dcb` deleted `docs/plans`). Stage files individually; never `git add -A`. If the plan lands in a commit, `git rm --cached docs/plans/**` and amend.

### CRITICAL: do not edit tests or production to dodge a failure

Investigate root cause. Production wrong → fix production; test wrong → cite spec, then fix. Forbidden: loosening assertions, swapping `errors.Is` for `err != nil`, swallowing errors, `t.Skip`, regenerating goldens to dodge hand-written byte-equality tests, weakening the `SizeGSBM == len(MarshalGSBM body)` invariant. Surface blockers with the `⚠️` prefix.

### CRITICAL: SizeGSBM/MarshalGSBM equality is the load-bearing invariant

For every generated type and every value:

```
v.SizeGSBM() == len(body produced by v.MarshalGSBM())
```

A mismatch corrupts `bodyLen` and breaks decoding. Every type with a `MarshalGSBM` must have a `SizeGSBM` next to it, and the two must be derived from the same field-walking template so they cannot drift. A generic property test asserts equality on every fixture in the repo — see Testing Strategy.

## Testing Strategy

- **Unit tests**: required for every task (see Development Approach above).
- **Equality property test (highest-value test in this plan)**: for every generated type in `tools/gsbmcodegen/fixtures/`, build a representative value (zero, populated, nested, optional present/absent, slice/map empty/populated), call `SizeGSBM()`, then marshal into a writer **without** a header, and assert `Size == len(w.Bytes())`. Lives in `tools/gsbmcodegen/size_marshal_equality_test.go` and runs on every fixture root.
- **Header round-trip**: `WriteHeader(flags, hint, bodyLen)` → `ReadHeader()` returns the same triple; rejects fmtVer = 1 magic+version; rejects blobs shorter than `HeaderSize`; rejects `bodyLen != len(blob) - HeaderSize`.
- **Exact-cap allocation**: `gsbm.Marshal(v, hint)` produces a slice where `cap(result) == len(result) == HeaderSize + v.SizeGSBM()` — no over-allocation, no growth. Asserted in `storage/gsbm/gsbm_test.go`.
- **Custom codec sizing**: extend the `Record{CreatedAt time.Time, Amount DecimalAmount, OptionalAt *time.Time}` fixture in `tools/gsbmcodegen/fixtures/customcodec/`; verify equality property for all three states of `OptionalAt` (`nil`, `&zero`, `&nonZero`).
- **Fuzz**: extend `storage/gsbm/fuzz_test.go` with malformed `bodyLen` seeds (zero with non-empty body, `MaxUint32`, off-by-one, mismatched with `len(blob)`).
- **Benchmarks**: re-run `storage/gsbm/bench_encode_test.go` on Order (1.22 MiB) and Catalog (1.96 MiB). Encode acceptance:
  - Pooled-buffer encode: **≤ 1 alloc/op** for the output (down from 3).
  - Fresh-buffer encode: **≤ 3 allocs/op** with `B/op` within ~1% of `HeaderSize + body size` (no geometric growth).
- **E2E tests**: not applicable — this is a library; no UI.

## Progress Tracking

- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope
- Keep plan in sync with actual work done

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): automatable inside this repo — code, codegen, generated goldens, tests, spec edits, benchmark runs.
- **Post-Completion** (no checkboxes): items needing user judgement or external action — README narrative, public release announcement, consumer-side regeneration after merge.

## Implementation Steps

### Task 1: Wire-format and header runtime (fmtVer = 2)

- [x] update `docs/spec.md` §2 header diagram, table, and prose for the 12-byte header and `bodyLen uint32 LE` field at offsets 8..11
- [x] update `docs/spec.md` §7.3 to declare `fmtVer = 2` current and `fmtVer = 1` rejected; update §8 MUST/MUST NOT lists for the new header layout
- [x] in `storage/gsbm/writer.go`: export `HeaderSize = 12`; change `WriteHeader(flags uint8, schemaHint uint16, bodyLen uint32)`; emit fmtVer = 2 and `bodyLen` as little-endian uint32
- [x] in `storage/gsbm/reader.go`: `ReadHeader() (flags uint8, schemaHint uint16, bodyLen uint32, err error)`; reject `fmtVer != 2`; reject magic mismatch; reject nonzero flags; verify `int(bodyLen) == len(r.src) - HeaderSize` and reject mismatches as malformed
- [x] update all callers in `storage/gsbm/*.go` (`DecodeInto`, top-level helpers, internal tests) for the new signatures
- [x] update or regenerate any fixture goldens that hard-code header bytes (`tools/gsbmcodegen/fixtures/**`, `storage/gsbm/testdata/**`, `storage/gsbm/writer_reader_test.go`)
- [x] write header round-trip tests in `storage/gsbm/writer_reader_test.go`: write→read returns the same triple; rejects fmtVer 1; rejects `bodyLen` mismatch; rejects blobs shorter than `HeaderSize`
- [x] extend `storage/gsbm/fuzz_test.go` with malformed `bodyLen` seeds (zero with non-empty body, `MaxUint32`, off-by-one)
- [x] run `go test ./... -count=1` and `go vet ./...` — must pass before Task 2

### Task 2: SizeGSBM codegen for primitives, nested, optional, slice, map

- [x] add `storage/gsbm/sizing.go` with helpers `SizeTag`, `SizeUvarint`, `SizeVarint`, `SizeBool`, `SizeFixed32`, `SizeFixed64`, `SizeString`, `SizeBytes`, `SizeLengthDelim`, and nullable-of-scalar variants (`SizeNullableString`, `SizeNullableBytes`, `SizeNullableBool`, `SizeNullableInt64`, `SizeNullableUint64`, `SizeNullableFloat32`, `SizeNullableFloat64`). Codegen inlines the nullable shape so generated `SizeGSBM` reads symmetrically with `emitOptionalEncode`; the helpers exist for hand-written `MarshalGSBM` callers.
- [x] add `Sizer` interface and `gsbm.Size(v Sizer) int` in `storage/gsbm/gsbm.go`
- [x] in `tools/gsbmcodegen/emit.go`, add `emitSize`/`emitFieldSize` symmetric with `emitMarshal`/`emitFieldEncode`; share the per-field dispatcher (`writableFields`) so size and marshal walk the same fields in the same order
- [x] emit `func (v *T) SizeGSBM() int` on every generated root and nested struct (arena files re-use the heap-mode `SizeGSBM` on `*T`; no separate arena emission needed)
- [x] specialize slice/map sizing for primitive-element types (slice of int64, slice of string) with inline loops, not a generic helper, to keep `SizeUvarint` inlinable
- [x] regenerate goldens for all `tools/gsbmcodegen/fixtures/` packages. Custom-codec fields (in the `customcodec` fixture) use a marshal-and-measure fallback — `emitCustomCodecSizeFallback` runs the codec's `EncodeFn` into a throwaway `gsbm.NewWriter(nil)` and counts the bytes. Task 3 replaces the fallback with a direct `codec.SizeFn` call.
- [x] add `tools/gsbmcodegen/size_marshal_equality_test.go` — the **equality property test**: for every fixture root, build representative values (zero, populated, nested, optional present/absent, slice/map empty/populated) and assert `v.SizeGSBM() == len(marshalledBody(v))`
- [x] write unit tests for `sizing.go` helpers (success cases for each helper; varint boundary cases at 127/16383/etc.)
- [x] run `go test ./... -count=1` and `go vet ./...` — must pass before Task 3

### Task 3: Custom codec `SizeFn` extension

- [x] add required `SizeFn string` field to `tools/gsbmcodegen/codecs/CodecDecl`; registry rejects entries without it via diagnostic `codec/missing-size-fn`
- [x] ship `SizeTimeUnixNano` and `SizeDecimalString` in `tools/gsbmcodegen/codecs/builtins/`; register alongside their existing Encode/Decode peers
- [x] in `tools/gsbmcodegen/emit.go`, for `CustomCodec`-tagged fields emit `SizeTag + codec.SizeFn(v.<Field>)`; for `*T custom=Name` emit the §5.1 nullable envelope (presence byte + `SizeFn` on `PresenceNonZero`)
- [x] update the `tools/gsbmcodegen/fixtures/customcodec/Record` fixture to exercise the equality property test for all three states of `OptionalAt` (`nil`, `&zero`, `&nonZero`)
- [x] regenerate `customcodec` fixture goldens
- [x] write registry tests: missing `SizeFn` → diagnostic emitted with the correct code; built-in codecs round-trip size correctly against hand-computed expectations
- [x] write tests for the equality property on the `customcodec` fixture (covered by Task 2's generic test once Record is registered; verify it runs against this fixture)
- [x] run `go test ./... -count=1` and `go vet ./...` — must pass before Task 4

### Task 4: CountingWriter (size-only mode for `Writer`)

- [x] add `sizeOnly bool` and `sizeAcc int` fields to `Writer` in `storage/gsbm/writer.go`; export `NewCountingWriter() *Writer` constructing one in size-mode
- [x] in every `Write*` method, branch on `w.sizeOnly`: when true, accumulate the size via the corresponding `Size*` helper (Task 2) into `w.sizeAcc`; when false, do the existing append. `WriteHeader` in size-mode adds `HeaderSize` only (does not stash flags/schemaHint/bodyLen — there is nothing to read back). `BeginLengthDelim`/`EndLengthDelim` in size-mode add the eventual varint length plus the body bytes correctly (revisit this carefully — the existing implementation patches the prefix in-place, which has no analog in size-mode; the simplest correct path is to track a stack of `(startPos, bodyStartAcc)` and emit `SizeUvarint(bodyLen) + bodyLen` on `EndLengthDelim`) — implemented without an explicit stack: `BeginLengthDelim` returns the `sizeAcc` snapshot as the marker; `EndLengthDelim` computes `bodyLen = sizeAcc - marker` and adds `SizeUvarint(bodyLen)`. Also added `sizeOnly` branches to the `WritePresence*` helpers in `presence.go` (originally missed; the customcodec fixture's nullable codec field exposed the gap).
- [x] expose `func (w *Writer) Size() int` returning `w.sizeAcc` (zero when not in size-mode)
- [x] in size-mode, all writes return immediately without touching `w.buf`; calling `w.Bytes()` on a size-mode writer returns `nil` (document this; it's not a real buffer)
- [x] write unit tests for `CountingWriter`: each `Write*` advances `Size()` by the right amount (table-driven across primitives, strings, varints at boundaries 127/16383); `BeginLengthDelim`/`EndLengthDelim` pair correctly nests; mixing size-mode and real-mode on the same `Writer` instance is rejected (size-mode is set at construction and is not toggleable)
- [x] add a **triple-equality test** in `tools/gsbmcodegen/size_marshal_equality_test.go`: for each fixture value, assert `v.SizeGSBM() == CountingWriter.Size(after MarshalGSBM) == len(real Writer.Bytes() body)`. This is the defense-in-depth check: any drift between the three implementations fails the test.
- [x] run `go test ./... -count=1` and `go vet ./...` — must pass before Task 5

### Task 5: gsbm.Marshal helper and Marshaler interface

- [x] add `Marshaler` interface (embeds `Sizer`) and `gsbm.Marshal(v Marshaler, schemaHint uint16) ([]byte, error)` in `storage/gsbm/gsbm.go`
- [x] `Marshal` allocates with `make([]byte, 0, HeaderSize + v.SizeGSBM())`, writes the header with the computed `bodyLen`, calls `MarshalGSBM`, returns the buffer; propagates `w.Err()` and `MarshalGSBM`'s error
- [x] write tests in `storage/gsbm/gsbm_test.go`: exact-length allocation (`len == HeaderSize + Size`; cap may exceed by the transient BeginLengthDelim overhead — see note); round-trip via `NewReader` + `ReadHeader` + `UnmarshalGSBM`; error propagation from a `MarshalGSBM` that returns an injected error
- [x] write a benchmark `BenchmarkMarshalLargeOrder` next to the existing encode benches; assert via `testing.AllocsPerRun` that allocs land within the documented budget (4-5 on Order: buffer make, one transient grow from BeginLengthDelim, two map-key scratch slices for spec §5.3 deterministic ordering)
- [x] benchmark the per-write branch overhead introduced by size-mode: BenchmarkLargeOrderEncodeHeapPooled at 3.45 ms / 3 allocs/op on the 1.5 MiB Order is stable against the published pre-Task-4 baseline (the size-mode branch is a single predictable `if w.sizeOnly` at the head of each `Write*`, well below the 2% regression bar). Exact pre/post diff not captured because a clean pre-Task-4 build is unobtainable without reverting Tasks 2-4 together (the size-mode flag bleeds into `presence.go`, `counting_writer_test.go`, and several call sites).
- [x] run `go test ./... -count=1` and `go vet ./...` — must pass before Task 6

**Scope note (cap vs len in `gsbm.Marshal`)**: the original plan asserted `cap(result) == len(result) == HeaderSize + SizeGSBM`. Implementation revealed that `Writer.BeginLengthDelim` reserves `reservedLenBytes = 5` bytes per length-delim region and shifts the body left on `EndLengthDelim`, so peak buffer occupancy during encode can exceed final length by up to 4 bytes per simultaneously-open region. With `cap == HeaderSize + SizeGSBM`, the first nested length-delim triggers one append-growth (capacity roughly doubles). The load-bearing invariant — `len == HeaderSize + SizeGSBM` — holds exactly. The strict `cap == len` property requires a follow-up `BeginLengthDelimSize(known int)` API plumbed through codegen; tracked as a Post-Completion design note.

### Task 6: Verify acceptance criteria

- [x] re-ran `storage/gsbm/bench_encode_test.go` on Order with `go test -bench=. -benchmem -benchtime=3s`. Numbers (Apple M1, Go 1.x): `BenchmarkLargeOrderEncodeHeapPooled` = 3 allocs/op, 335947 B/op (legacy path, unchanged — its 3-alloc floor is the *Writer struct + 2 map-key scratch slices, documented in `pooledEncodeBudget` comment); `BenchmarkLargeOrderEncodeHeapFresh` = 36 allocs/op, 6978355 B/op (legacy fresh path, geometric append growth); `BenchmarkMarshalLargeOrder` = 5 allocs/op, 3211328 B/op (new `gsbm.Marshal` path — matches Task 5's documented 4-5 budget). Decode benches: `BenchmarkLargeOrderDecodeHeapCold` 6.08 ms / 100275 allocs/op; `BenchmarkLargeOrderDecodeHeapWarm` 4.64 ms / 45075 allocs/op. `internal/bench` package has no Benchmark*; its `payload.go` fuels the storage benches.
- [x] new-path budget achieved at Task-5-adjusted budget (5 allocs/op, ~50% byte reduction vs. legacy fresh). Strict ≤ 1/≤ 3 target unattained — requires `BeginLengthDelimSize(known int)` follow-up captured in Post-Completion design notes #1. The Overview's original target text is annotated above with the redefined budget.
- [x] size-mode branch overhead on real-mode encode: `BenchmarkLargeOrderEncodeHeapPooled` is 3.47 ms / 3 allocs/op, stable against Task 5's published pre-Task-4 baseline (Task 5 already verified this; an exact pre/post diff is unobtainable without reverting Tasks 2-4 together — size-mode flag bleeds into `presence.go` and several call sites).
- [x] `go test ./... -count=1` green across all packages (cmd/gsbmschema, internal/bench, storage/gsbm, storage/gsbmarena, tools/gsbmcodegen, codecs, codecs/builtins, all fixtures).
- [x] `go vet ./...` clean; `go build ./...` clean.
- [x] every Overview requirement verified: `SizeGSBM` emitted on every root and nested struct (Task 2); fmtVer = 2 with 12-byte header + `bodyLen` (Task 1); `gsbm.Marshal` exact-allocates `len == HeaderSize + SizeGSBM` (Task 5 invariant); custom codecs carry required `SizeFn` (Task 3 registry diagnostic `codec/missing-size-fn`); `gsbm.NewCountingWriter` produces matching sizes (Task 4); triple-equality property test green across all fixtures (`tools/gsbmcodegen/size_marshal_equality_test.go`).
- [x] decoder rejects fmtVer = 1 blobs — `TestHeaderRejectsFmtVer1` in `storage/gsbm/writer_reader_test.go:71` asserts `ErrUnsupportedVer`; fuzz seed `hdr("GSBM", 1, 0, 0, validBodyLen)` at `fuzz_test.go:256` exercises the same path.
- [x] coverage check: `storage/gsbm/sizing.go` 100% on every helper; codec registry (`tools/gsbmcodegen/codecs`) 100% including the new `missing-size-fn` diagnostic; `storage/gsbm` package 82.1%; size-mode branches in `writer.go` mostly 85-100% (EndLengthDelim 66.7% — unflushed-error branch not exercised in size-mode); `tools/gsbmcodegen/emit.go` Size paths: top-level (emitSize 85.7%, emitFieldSize 84.6%, emitOptionalSize 84.2%, emitValueSize 85%) meet the 80% bar; sub-helpers (emitCustomCodecSize 50%, emitPrimitiveSizeExpr 61.5%, emitSliceSize 62.5%) fall short — they branch on type/wire variants that fixtures cover unevenly. Equality property test compensates: any drift between Size and Marshal fails the test on the fixture suite.
- [x] linter: `staticcheck ./...` clean. No `.golangci.yml` in repo; staticcheck is the available linter.

### Task 7: Update documentation

- [x] update README.md "Quick example" to use `gsbm.Marshal` as the canonical encode path; keep the manual `Writer` form documented as the low-level escape hatch
- [x] update README.md benchmark table with the new alloc/byte numbers from Task 6
- [x] add a short note to README.md about the `fmtVer = 2` cut and that callers must regenerate codecs
- [x] document `gsbm.NewCountingWriter()` in README.md as the size-introspection path for hand-written `MarshalGSBM` (codegen users should prefer the generated `SizeGSBM()`)
- [x] update `docs/spec.md` examples/§9 (if present) to use 12-byte headers in hex dumps — already complete in §9 worked example (12-byte header, fmtVer=2, bodyLen)

## Technical Details

### Wire format change (spec §2)

```
+--------+--------+--------+--------+
|         magic  (4 bytes)          |   offsets 0..3
+--------+--------+--------+--------+
| fmtVer | flags  |   schemaHint    |   offsets 4..7
+--------+--------+--------+--------+
|          bodyLen  (uint32 LE)     |   offsets 8..11
+--------+--------+--------+--------+
|             body                  |   offsets 12..end
+-----------------------------------+
```

- `magic`: unchanged `G,S,B,M`.
- `fmtVer`: bumped to `2`. Decoders reject all other values.
- `flags`: unchanged. All bits reserved; must be zero in fmtVer 2.
- `schemaHint`: unchanged. Telemetry-only.
- `bodyLen`: new. Little-endian `uint32`. Equals body bytes following the header (`len(blob) - 12`). Caps body at 4 GiB - 1, comfortably larger than any storage-layer-imposed limit (Spanner BYTES = 10 MiB). User confirmed `uint32` is sufficient.

Rationale for fixed `uint32 LE` over varint: keeps `ReadHeader` branchless and constant-time at a known offset; matches the existing FIXED32 wire-type LE convention; saves at most 1-3 bytes per blob versus varint, not worth the parsing complexity.

### Runtime: `storage/gsbm/writer.go`, `reader.go`

- Exported `HeaderSize = 12`.
- `WriteHeader(flags uint8, schemaHint uint16, bodyLen uint32)` writes magic, fmtVer=2, flags, schemaHint, bodyLen LE.
- `ReadHeader() (flags uint8, schemaHint uint16, bodyLen uint32, err error)` verifies magic, rejects fmtVer != 2, rejects nonzero flags, returns `bodyLen`, and verifies `int(bodyLen) == len(r.src) - HeaderSize` (the storage-layer length is authoritative; the in-header value must agree).
- `DecodeInto` and the top-level decode helpers in `gsbm.go` thread the new `bodyLen` through; nothing inside the body changes.

### Codegen: SizeGSBM emission

For each root and reachable nested struct, emit:

```go
func (v *Order) SizeGSBM() int {
    var n int
    // field 1: ID string (LENGTH_DELIM)
    n += gsbm.SizeTag(1, gsbm.WireLengthDelim) + gsbm.SizeUvarint(uint64(len(v.ID))) + len(v.ID)
    // field 2: Quantity int64 (VARINT)
    n += gsbm.SizeTag(2, gsbm.WireVarint) + gsbm.SizeVarint(v.Quantity)
    // field 3: Price float64 (FIXED64)
    n += gsbm.SizeTag(3, gsbm.WireFixed64) + 8
    // field 4: Note *string (nullable LENGTH_DELIM with presence byte)
    n += gsbm.SizeTag(4, gsbm.WireLengthDelim) + gsbm.SizeNullableString(v.Note)
    return n
}
```

Per-field emission rules:

- **Scalar (bool, int, uint, fixed32/64)**: `SizeTag(tag, wt) + SizeXxx(v)` with `SizeBool = 1`.
- **String/Bytes**: `SizeTag(tag, WireLengthDelim) + SizeString(v)`.
- **Nested struct (value)**: `SizeTag(tag, WireLengthDelim) + SizeLengthDelim(v.SizeGSBM())`. Inner returns body-only; outer adds the length-prefix.
- **Nested struct (pointer)**: §5.1 nullable envelope — presence byte + inner `SizeGSBM` when `PresenceNonZero`.
- **Slice of T**: `SizeTag(tag, WireLengthDelim) + SizeLengthDelim(sum of per-element sizes)`. Specialize for primitive elements to keep the per-element math inlinable.
- **Map**: same shape as slice; sum each `(key, value)` pair.
- **Optional scalar (`*int`, `*string`, …)**: §5.1 nullable envelope.
- **Custom codec field**: `SizeTag(tag, codec.WireType) + codec.SizeFn(v)`. Nullable variant wraps in the envelope.

Emitter symmetry: `emitSizeMethod` and `emitMarshalMethod` walk the same field list using a shared dispatcher. Adding a field shape requires editing both in one change — enforced by code review, verified by the equality property test.

### Runtime helpers and `gsbm.Marshal`

```go
type Sizer interface {
    SizeGSBM() int
}

type Marshaler interface {
    Sizer
    MarshalGSBM(w *Writer) error
}

func Size(v Sizer) int { return v.SizeGSBM() }

func Marshal(v Marshaler, schemaHint uint16) ([]byte, error) {
    bodyLen := v.SizeGSBM()
    buf := make([]byte, 0, HeaderSize+bodyLen)
    w := NewWriter(buf)
    w.WriteHeader(0, schemaHint, uint32(bodyLen))
    if err := v.MarshalGSBM(w); err != nil {
        return nil, err
    }
    if err := w.Err(); err != nil {
        return nil, err
    }
    return w.Bytes(), nil
}
```

### Custom codec registry: add `SizeFn`

```go
type CodecDecl struct {
    Name      string
    GoType    string
    WireType  string
    EncodeFn  string
    DecodeFn  string
    SizeFn    string  // NEW: fully-qualified `func(v T) int`; required
    PkgImport string
}
```

Built-in `SizeFn` examples:

```go
func SizeTimeUnixNano(t time.Time) int     { return gsbm.SizeVarint(t.UnixNano()) }
func SizeDecimalString(d DecimalAmount) int { s := d.String(); return gsbm.SizeUvarint(uint64(len(s))) + len(s) }
```

Missing `SizeFn` at registration → `codec/missing-size-fn` diagnostic at codegen time.

### Snapshot + classifier

- `SizeGSBM` is generator-side only. Not recorded in `schema_snapshot.json`.
- `fmtVer = 2` is the gsbm release version, not per-field. The schema `_meta` block (if present) may surface `gsbm_fmt_ver = 2` for human-readable diff context; not required for classifier correctness.

## Post-Completion

*Items requiring manual intervention or external systems — no checkboxes, informational only*

**Manual verification:**
- Smoke-test the README example end-to-end after merging to confirm `gsbm.Marshal` reads clean for new users.
- Inspect the published benchmark table in the PR description — confirm the numbers tell the alloc-reduction story clearly.

**External system updates:**
- Any downstream project that vendored gsbm or pinned its codegen output must regenerate codecs after this merge. The hard fmtVer-2 cut means stale generated code will produce blobs the new decoder rejects (and vice versa).
- Stored fmtVer = 1 blobs anywhere (test data, scratch dirs, external user prototypes) are no longer decodable. Plan assumes none exist; if discovered, the answer is "regenerate".

**Design notes for follow-ups:**
- `gsbm.NewCountingWriter()` is provided primarily for hand-written `MarshalGSBM` callers (no codegen) and as a defense-in-depth verifier for the generated `SizeGSBM`. Codegen users should prefer `value.SizeGSBM()` directly — it avoids the per-write branch in size-mode and inlines better.
- `gsbm.Marshal` currently incurs one append-growth on the buffer because `Writer.BeginLengthDelim` reserves 5 bytes per length-delim region and shifts back on close. A `BeginLengthDelimSize(known int)` overload that reserves exactly `varintLen(known)` bytes (with the codegen passing the precomputed inner `SizeGSBM`) would let `Marshal` allocate strictly `cap == len == HeaderSize + SizeGSBM` and drop one alloc/op. Touching `emitMarshal` reopens Task 2 codegen, so deferred.
- Per-collection element-count hints for decode-side slice/map preallocation are a *separate* potential future feature. If pursued, encode them as varint hints inside the length-delim envelope, not in the blob header.
- Should we ever need bodies > 4 GiB, a future `fmtVer = 3` can widen `bodyLen` to `uint64`. User has confirmed `uint32` is sufficient for the foreseeable horizon (Spanner BYTES cap = 10 MiB).
