# gsbm — Go Simple Binary Marshaller

A tagged binary serializer for Go, designed for long-lived storage formats (e.g., Spanner BYTES columns) where schema evolves indefinitely without backfill. Tagged fields, append-only schema policy, codegen-only (no runtime reflection), with both a heap-mode and arena-mode runtime sharing the same wire bytes.

**Status:** draft, `fmtVer = 2`.

> **`fmtVer = 2` cut (May 2026):** the previous `fmtVer = 1` draft is rejected outright. The blob header grew from 8 bytes to 12 bytes (adds a `bodyLen uint32 LE` field at offsets 8..11). There is no migration path on the wire — any downstream project that vendored gsbm or pinned codegen output must regenerate codecs and re-emit data. See [`docs/spec.md`](docs/spec.md) §2 and §7.3.

## Why

`encoding/json` and protobuf both work, but neither is a great fit when the production hot path is "encode a domain struct, write to Spanner, read it back years later". Protobuf forces a parallel schema definition and a domain↔proto mapping layer; JSON is too slow and too lax about unknown fields. gsbm sits in the middle: write your Go structs once, annotate fields with `bin:"N"` tags, generate codecs, and the wire bytes stay readable across schema changes that follow the append-only rules.

- **Tagged fields, varint keys** — same shape as protobuf wire format (varint-compatible) but with a different wire-type table and explicit nullable framing. See [`docs/spec.md`](docs/spec.md).
- **Append-only schema** — fields can be added or deprecated; never removed, renamed (the tag stays), or retyped without explicit `--allow-breaking` ack.
- **Codegen, not reflection** — `MarshalGSBM` / `UnmarshalGSBM` / `Reset` / `FieldPresent` are emitted as static Go methods. Switch-on-tag with inline literals.
- **Two runtime modes, one wire format** — heap mode for the storage path; arena mode for read-heavy paths where releasing the entire decoded graph in one operation matters.
- **Schema diff classifier** — the `gsbmschema` CLI categorizes diffs as safe / warning / breaking and gates breaking changes behind explicit acknowledgement flags.

## Quick example

```go
//gsbm:root
type Order struct {
    ID         string  `bin:"1"`
    Quantity   int64   `bin:"2"`
    Price      float64 `bin:"3"`
    Note       *string `bin:"4"` // optional
    ExternalID int     `bin:"5,type=int64"`  // widen Go int  to int64  wire range
    Points     uint    `bin:"6,type=uint64"` // widen Go uint to uint64 wire range
}

// codegen produces order_gsbm.go with:
//   func (v *Order) SizeGSBM() int
//   func (v *Order) MarshalGSBM(w *gsbm.Writer) error
//   func (v *Order) UnmarshalGSBM(r *gsbm.Reader) error
//   func (v *Order) Reset()
//   func (v *Order) FieldPresent(tag uint32) bool

// Encode (canonical path: exact-size allocation, single call)
blob, err := gsbm.Marshal(&order, schemaHint)

// Encode with opt-in zstd body compression (flag bit 0; reader auto-detects)
blob, err = gsbm.MarshalWithOptions(&order, schemaHint, gsbm.Options{Compress: true})

// Decode (heap mode, cold) — same call for compressed and uncompressed
var dst Order
r := gsbm.NewReader(blob)
r.ReadHeader()
dst.UnmarshalGSBM(r)

// Decode (heap mode, warm — reuse capacity via DecodeInto)
gsbm.DecodeInto(blob, &dst)
```

Compression is opt-in and a strictly larger API surface — `Marshal`
output is byte-identical to its pre-compression form. For when to
enable it, the streaming `MarshalToWriter` entry point that avoids
materializing the raw body, and the ratio numbers on the
repeated-nested bench fixture, see
[`docs/codecs/compression.md`](docs/codecs/compression.md).

### Unsafe borrowed-string heap decode

Heap decode copies strings by default so decoded values are safe after the
input blob is reused or freed. A string-heavy type can opt into zero-copy
heap string decode with a struct marker:

```go
//gsbm:root
//gsbm:borrow-strings
type Order struct {
    ID string `bin:"1"`
}
```

Generated `UnmarshalGSBM` for that struct aliases decoded strings directly
into the `[]byte` passed to the reader when no custom allocator is installed.
**The caller MUST keep that byte slice alive and immutable for at least as
long as any decoded value, map key, slice element, or pooled receiver may be
observed.** Reusing or mutating the decode buffer while borrowed values are
live can corrupt strings and can break Go map invariants when borrowed strings
are used as map keys.

The marker is struct-local: nested named structs must carry their own
`//gsbm:borrow-strings` marker to borrow inside their own decoder body. It is
not a wire-format change; it only changes generated Go allocation/lifetime
behavior. Allocator-backed readers (arena/custom allocators) keep allocator
semantics even on borrow-marked structs.

`DecodeInto` and receiver pools are safe only when the blob lifetime is managed
with the same care as the receiver lifetime. Do not return a receiver to a pool
or reuse its input buffer while any consumer can still observe borrowed strings.
See [`docs/borrow-strings.md`](docs/borrow-strings.md) for good/bad fit examples, the full contract, and the analyzer/linter follow-up.

`gsbm.Marshal` is the recommended encode entry point: it calls `SizeGSBM`
to pre-size the output buffer, writes the 12-byte header (including
`bodyLen`), invokes `MarshalGSBM`, and returns the finished blob. The
manual `Writer` form below remains available as the low-level escape
hatch for callers who need to interleave encoding with other writes:

```go
// Manual (low-level): caller owns the buffer and the header.
size := order.SizeGSBM()
buf  := make([]byte, 0, gsbm.HeaderSize+size)
w    := gsbm.NewWriter(buf)
w.WriteHeader(0, schemaHint, uint32(size))   // 12-byte header with bodyLen
if err := order.MarshalGSBM(w); err != nil { /* ... */ }
blob := w.Bytes()
```

For hand-written `MarshalGSBM` implementations (callers who did not go
through codegen and therefore have no generated `SizeGSBM`),
`gsbm.NewCountingWriter()` is the size-introspection escape hatch:

```go
cw := gsbm.NewCountingWriter()
_ = order.MarshalGSBM(cw)                    // counts bytes, does not buffer
size := cw.Size()                             // body byte count, header-exclusive
```

Codegen users should prefer the generated `value.SizeGSBM()` directly —
it avoids the per-write branch in size-mode and inlines better.

### Per-field wire-range contract (`type=`)

Each integer field has a Go type (what the field can hold) and a wire range (what it promises to hold on the wire). The default wire range is determined by the Go type: fixed-width integers (`int8/16/32/64`, `uint8/16/32/64`) use their own width; the platform-sized trio `int`/`uint`/`uintptr` defaults to 32 bits for cross-architecture portability. A schema author can override the wire range per field with the `bin:"N,type=<width>"` option:

```go
ExternalID int    `bin:"5,type=int64"`  // widen Go int  to int64  wire range
Points     uint   `bin:"6,type=uint64"` // widen Go uint to uint64 wire range
SmallCount int    `bin:"7,type=int32"`  // pin the int32 default explicitly
Quantity   int64  `bin:"8,type=int16"`  // narrow: domain fits int16
NamedID    UserID `bin:"9,type=int32"`  // named alias (type UserID int64)
```

Legal width lexemes are the eight basic-integer widths: `int8/16/32/64` and `uint8/16/32/64`. The override sign must match the Go type's sign and the override width must not exceed the Go type's width — with one platform-sized exception that lets `int`/`uint`/`uintptr` opt up to the 64-bit wire width. Identity overrides (`int32 type=int32`) emit byte-identical output to the un-annotated form and exist as documentation markers. Narrowing overrides on a fixed-width Go type (`int64 type=int16`) add a wire bounds check. Widening on a fixed-width Go type, cross-sign pairs, and overrides on float or non-numeric Go types are rejected at validate with diagnostic `tag/type-width-mismatch` — see [`docs/codecs/diagnostics.md`](docs/codecs/diagnostics.md).

The full reference — contract table, encode/decode behavior, cross-version compatibility, schema-fingerprint behavior — lives in [`docs/spec.md`](docs/spec.md) §5.9.

**Recommended practice.** When the natural domain of a numeric field is known, prefer encoding that domain in the Go type itself over reaching for `type=`. An explicit Go field type (`int64`, `uint64`, `int16`) carries the intent in the type system, not in a wire-tag annotation, and reads correctly to any Go tool that does not understand the gsbm tag grammar. Reach for `type=<width>` only when the Go type cannot be changed (a third-party model you do not own, an in-progress migration that needs to stay source-compatible with existing call sites, a public API contract that pins the field at a particular Go type for stability) or when narrowing is part of the domain contract you want enforced at the wire boundary even though the Go type carries a wider value. Widening overrides on the platform-sized trio (`int`/`uint`/`uintptr` → 64-bit) opt the field out of 32-bit portability — a 32-bit reader gracefully rejects out-of-range values with `ErrIntegerOverflow` rather than silently truncating.

### Presence tracking

`FieldPresent(tag)` reports whether a tag appeared on the wire during the
most recent decode. By default the generated `UnmarshalGSBM` records
presence in a stack-local `[N]uint64` bitmap that dies with the call, so
`FieldPresent` returns `false` post-decode — the bitmap exists only for
the brief window between reading the tag and finishing decode. This
keeps the decode path allocation-free with respect to presence
bookkeeping.

To observe presence after decode, annotate the struct with
`//gsbm:track-presence` (in addition to `//gsbm:root`, or on a struct
reachable from a root — codegen only emits files for schema-discovered
types):

```go
//gsbm:root
//gsbm:track-presence
type Order struct {
    ID          string    `bin:"1"`
    Quantity    int64     `bin:"2"`
    gsbmPresent [1]uint64 `bin:"-"` // sized to (maxTag+63)/64
}
```

The marker is opt-in because it adds a hidden `gsbmPresent` field that
the user must declare on the struct (it is not on the wire — `bin:"-"`).
With the marker, `FieldPresent` reads directly from that field; without
it, the field is absent and `FieldPresent` always returns `false`.

The package-level `gsbm.MarkPresent` / `gsbm.IsPresent` sidecar API is
deprecated and retained for one release as an escape hatch for
hand-written `UnmarshalGSBM` implementations. New code should use
`FieldPresent` (with `//gsbm:track-presence` for post-decode
observability).

### Generator directives

- `//gsbm:root` — declares a struct as a serialization root. Generates `MarshalGSBM`/`UnmarshalGSBM`/`Reset`/`SizeGSBM`/`FieldPresent`.
- `//gsbm:track-presence` — opts the struct into stored presence tracking; requires a ``gsbmPresent [N]uint64 `bin:"-"` `` field on the struct (see above).
- `//gsbm:borrow-strings` — unsafe heap-decode opt-in: generated string decodes may alias the input blob instead of copying. The caller must keep the blob alive and immutable while decoded values are in use.
- `//gsbm:opaque` — marks a type as opaque to schema discovery; codegen does not descend into its layout.
- `//gsbm:cycle_break_via_id` — legacy form of the `bin:"N,id_ref"` tag option (see [`docs/spec.md`](docs/spec.md) §5.7).
- `//gsbm:reserved <tag-list>` — reserves tags so future fields cannot accidentally reuse them.
- `//gsbm:allow-breaking <justification>` — admits a breaking schema change at the classifier, recording the reason.
- `//gsbm:presence` — reserved no-op placeholder; codegen ignores it.

The `gsbmschema` CLI accepts directory arguments and Go-style package
patterns interchangeably. Both forms produce the same schema, so pick
whichever matches the way you invoke `go build` in your project:

```bash
# directory form (works from anywhere; anchors on the dirs' go.mod)
gsbmschema lint ./pkg/model

# pattern form (anchored on cwd's module; recursive ./... wildcards)
gsbmschema lint ./...
gsbmschema lint example.com/proj/pkg/...
```

Both forms require a `go.mod` in the target tree — the same
requirement `go build` has; loose `.go` files outside any module are
not accepted. The two forms can be mixed in one invocation when
convenient.

## Custom codecs

A field tagged `bin:"N,custom=Name"` opts out of the schema-driven
emit path and calls a user-supplied codec instead. Codecs come in
three shapes: **analytic** (cheap size from `v`, no materialization),
**materializing-cached** (materialize once per `gsbm.Marshal` call,
retained in the Writer's scratch cache — best for small/medium
bodies), and **streaming** (materialize per pass, never retained —
best when the body can be large enough that retention would
meaningfully grow peak heap; see the streaming-codec peak-heap
benchmark below). The three shapes are mutually exclusive at
registration time. Full authoring guide — lifecycle, worked example,
testing, and diagnostics: [`docs/codecs/index.md`](docs/codecs/index.md).
Go-side contract reference: [`tools/gsbmcodegen/codecs/README.md`](tools/gsbmcodegen/codecs/README.md).

For decimal-like types the analytic `DecimalBinary` builtin encodes
`(coefficient, scale, sign)` as varints with zero per-decimal
allocation — the allocation-free alternative to the text-form
`DecimalString` / `DecimalAppend` codecs:
[`docs/codecs/binary-decimal.md`](docs/codecs/binary-decimal.md).

## Benchmarks

Numbers below were taken on `darwin/arm64`, Apple M1, `go test -bench=. -benchmem -benchtime=3s`. Order/Catalog payloads are produced by the deterministic generator in [`internal/bench`](internal/bench/payload.go); the borrowed-string fixture lives in [`tools/gsbmcodegen/fixtures/borrowstrings`](tools/gsbmcodegen/fixtures/borrowstrings). All sit inside the 1-2 MiB target the design targets (Spanner offer batches).

- **Order** payload: **1,277,171 bytes (1.22 MiB)** — ~19 fields, 100+ items, populated maps and optionals.
- **Catalog** payload: **2,055,741 bytes (1.96 MiB)** — graph fixture exercising slices-of-nullable, maps-of-nullable, and 2-level struct nesting.
- **LargeStringRecord** payload: ~**1.97 MB (1.88 MiB)** — generated fixture dominated by direct strings, named-string slices, and string-key/string-value maps to isolate `//gsbm:borrow-strings`.

### Encode (Order, 1.22 MiB)

| Path | ns/op | MB/s | B/op | allocs/op |
|---|---:|---:|---:|---:|
| `gsbm.Marshal` (exact-size, fresh buffer) | 3,685,287 | 346 | 3,211,336 | **5** |
| Heap, pooled buffer (`sync.Pool`) | 3,164,660 | **403** | 336,008 | **3** |
| Heap, fresh buffer per op (geometric `append` growth) | 3,955,961 | 322 | 6,978,359 | 36 |

`gsbm.Marshal` uses `SizeGSBM` to size the output buffer to `HeaderSize+SizeGSBM()` up front, so `len(blob)` lands at the exact byte count with no geometric-growth tax. `cap(blob)` may exceed `len(blob)` because `Writer.BeginLengthDelim` transiently over-reserves the inner length varint and triggers one append grow on the initial buffer — half the bytes and one-seventh the allocs of the legacy fresh path. The 5 allocs/op floor is the output buffer plus that transient grow from a nested `BeginLengthDelim` and two map-key scratch slices needed for §5.3 deterministic-order writes. The pooled-buffer path stays faster wall-clock when an external buffer pool is available (the 3 allocs are amortised setup, not per-field).

### Decode (Order, 1.22 MiB)

| Path | ns/op | MB/s | B/op | allocs/op |
|---|---:|---:|---:|---:|
| Heap, cold (fresh `*Order`, no pool) | 3,128,421 | 408 | 3,994,270 | 45,240 |
| Heap, warm (`sync.Pool` + `DecodeInto`) | 2,472,655 | **516** | 932,676 | 45,072 |
| Arena, single-shot (fresh arena per op) | 2,569,904 | 497 | 3,942,503 | 383 |
| Arena, pooled arenas | 2,572,568 | 496 | 3,943,501 | 383 |

Numbers reflect the local-presence-bitmap migration: generated `UnmarshalGSBM` records presence in a stack-local (or receiver-embedded) bitmap instead of routing through the package-level sidecar, cutting cold-decode allocs by ~55% and B/op by ~50%. Warm heap decode reuses slice and map capacity through `DecodeInto`, dropping per-op bytes from ~4 MiB (cold) to ~900 KiB. Arena decode aliases strings into arena memory (zero-copy strings) so per-string heap allocations disappear — only ~380 backing allocations remain.

### Borrowed-string heap decode fixture (~1.97 MB)

| Path | ns/op | MB/s | B/op | allocs/op |
|---|---:|---:|---:|---:|
| Heap, warm, default string copies | 1,313,885 | 1,502 | 2,688,651 | 56,004 |
| Heap, warm, `//gsbm:borrow-strings` | 458,573 | **4,305** | **222** | **2** |

The borrowed-string row removes the per-string `string([]byte)` copies in heap mode. The remaining allocations are the structural floor of the benchmark harness; the unsafe blob-lifetime contract applies.

### Round-trip (Order, encode + decode in one op)

| Path | ns/op | MB/s | B/op | allocs/op |
|---|---:|---:|---:|---:|
| Heap, pooled buffer + warm receiver | 5,786,323 | **220** | 1,691,614 | 45,098 |

### Catalog (1.96 MiB) — graph fixture with slices-of-nullable + maps-of-nullable

| Path | ns/op | MB/s | B/op | allocs/op |
|---|---:|---:|---:|---:|
| Encode, heap pooled | 3,518,009 | **584** | 139,350 | **2** |
| Decode, heap | 7,272,408 | 282 | 5,986,481 | 157,085 |
| Decode, arena | 5,936,390 | 346 | 5,582,053 | 49,200 |

Catalog's encode-pooled hits 2 allocs/op (the output buffer plus a transient map-keys scratch slice for deterministic-order writes). Decode is heavier than Order because the graph fixture intentionally maximises composite-encoding paths (every Section has a slice of nullable Items; every Tag is read through a map with nullable values). The presence-bitmap migration cuts Catalog heap-decode allocs ~59% and arena ~82% relative to the pre-PR sidecar baseline.

### Streaming-codec peak heap (issue #30, 256 × 32 KiB JSON payloads, ~11 MiB blob)

| Codec kind | Peak heap during `gsbm.Marshal` | Ratio to blob |
|---|---:|---:|
| Materializing-cached (`EmitFn` + Writer scratch) | 23.85 MiB | **2.13 ×** |
| Streaming (`StreamFn`, no cache) | 14.00 MiB | **1.25 ×** |

Streaming cuts peak heap by ~41 % on this fixture by materializing the JSON body once per pass (size + write) and discarding it between passes, instead of retaining every occurrence's bytes in the Writer's scratch cache alongside the output buffer. The trade-off is 2× CPU on the codec body (the materializing-cached path runs it once per occurrence per `gsbm.Marshal` call; streaming runs it twice). Pick streaming when the materialized body can be large enough that retention would matter, materializing-cached otherwise.

Numbers come from `BenchmarkEncodePeakMemoryStreamingVsCached` / `TestEncodePeakMemoryStreamingVsCachedBudget`, both in [`storage/gsbm/bench_encode_test.go`](storage/gsbm/bench_encode_test.go); methodology is a synchronous-GC two-sample probe (`gsbm.MarshalWithProbe` — test-only) anchored at the size→write transition and at the end of the write pass. The probe sizes its output buffer to exactly `HeaderSize+bodyLen` (the same capacity `gsbm.Marshal` uses), so any write-pass append-grow on the first nested length-delim region lands in the probe as well — the streaming row's `1.25 ×` includes that retained over-cap on the returned slice. The budget test gates the production peak directly.

### Reproducing

```bash
# All benchmarks
go test -bench='^Benchmark' -benchmem -benchtime=3s ./...

# Per-package
go test -bench=. -benchmem ./storage/gsbm/        # encode + decode + round-trip on Order
go test -bench=. -benchmem ./storage/gsbmarena/   # arena-mode decode
go test -bench=. -benchmem ./tools/gsbmcodegen/fixtures/graph/          # Catalog graph fixture
go test -bench=. -benchmem ./tools/gsbmcodegen/fixtures/borrowstrings/  # borrowed-string decode fixture

# Or via the Makefile
make bench
```

Each benchmark is partnered with a `Test*Budget` that asserts the allocation count via `testing.AllocsPerRun`; budgets ride along on every `go test` invocation, so allocation regressions block CI rather than only showing up in benchmark runs.

## Fuzz testing

Three harnesses cover the wire-format invariants on top of the existing arena↔heap cross-check:

- `FuzzReaderRobustness` — arbitrary input must surface a documented `gsbm.Err*` sentinel or succeed; never panic, always make forward progress.
- `FuzzWriterReaderRoundTripCanonical` — any blob the reader accepts must re-encode to byte-identical output (canonical varints + last-wins duplicate handling). Maps carve-out documented inline (Go iteration order vs. on-wire deterministic-key sorting).
- `FuzzHeaderCorruption` — header byte mutations must surface the matching `Err*` sentinel (`ErrBadMagic` / `ErrReservedFlags` / `ErrUnsupportedVer` / `ErrBodyLenMismatch`) and never panic.
- `FuzzArenaDecodeAgainstHeap` — heap and arena decoders must agree on accept/reject and on the decoded values.

```bash
# Run a single fuzz harness for 30 seconds
go test -fuzz=FuzzReaderRobustness -fuzztime=30s ./storage/gsbm/

# Or via the Makefile
FUZZTIME=30s make fuzz
```

## Project layout

```
storage/gsbm/         heap-mode runtime (Writer, Reader, allocator, deprecated presence sidecar)
storage/gsbmarena/    arena-mode runtime (Arena, AllocStruct, AllocSlice)
tools/gsbmschema/     schema discovery, validation, classifier
tools/gsbmcodegen/    code generator + golden fixtures
cmd/gsbmschema/       gsbmschema CLI (lint, snapshot, diff, hash, gen, gen-arena)
internal/bench/       deterministic 1-2 MiB payload generator (test-only)
internal/bench/largepayload/  large-JSON-payload fixture for the peak-heap streaming-vs-cached bench
docs/                 spec.md (wire-format specification)
```

## Documentation

- [`docs/spec.md`](docs/spec.md) — wire format specification (the byte layout authority).

## License

See [LICENSE](LICENSE).
