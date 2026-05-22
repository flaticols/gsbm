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
    ExternalID int     `bin:"5,type=int64"` // widen Go int to int64 wire range
}

// codegen produces order_gsbm.go with:
//   func (v *Order) SizeGSBM() int
//   func (v *Order) MarshalGSBM(w *gsbm.Writer) error
//   func (v *Order) UnmarshalGSBM(r *gsbm.Reader) error
//   func (v *Order) Reset()
//   func (v *Order) FieldPresent(tag uint32) bool

// Encode (canonical path: exact-size allocation, single call)
blob, err := gsbm.Marshal(&order, schemaHint)

// Decode (heap mode, cold)
var dst Order
r := gsbm.NewReader(blob)
r.ReadHeader()
dst.UnmarshalGSBM(r)

// Decode (heap mode, warm — reuse capacity via DecodeInto)
gsbm.DecodeInto(blob, &dst)
```

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

### `int` field wire width

By default a Go `int` field encodes as a varint bounded to the int32 range — encoder and decoder both reject values outside `[MinInt32, MaxInt32]`. This is the safe default for cross-architecture portability. A field whose natural domain exceeds int32 (external numeric IDs, loyalty points, large counters) can opt into the int64 wire range with `bin:"N,type=int64"`:

```go
ExternalID int `bin:"5,type=int64"` // accept any int64 value
SmallCount int `bin:"6,type=int32"` // pin the int32 default explicitly
```

`type=int32` emits byte-identical output to the un-annotated form and exists as an intent marker. `type=int64` is the actual widening. The full reference — wire-shape table, cross-version compatibility, schema-fingerprint behavior — lives in [`docs/spec.md`](docs/spec.md) §5.9.

**Recommended practice.** When the domain of an integer field exceeds int32, prefer changing the Go field type to `int64` over reaching for `type=int64` — an explicit Go type carries the intent in the type system, not in a wire-tag annotation, and reads correctly to any Go tool that does not understand the gsbm tag grammar. Reach for `type=int64` only when the Go type cannot be changed: a third-party model you do not own, an in-progress migration that needs to stay source-compatible with existing call sites, or a public API contract that pins the field as `int` for stability. The override is for those constrained cases, not for new code where you control the type. Note: `type=int64` opts the field out of 32-bit portability — a 32-bit reader gracefully rejects values outside the platform `int` range with `ErrIntegerOverflow` rather than silently truncating.

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

Numbers below were taken on `darwin/arm64`, Apple M1, `go test -bench=. -benchmem -benchtime=3s`. Payloads are produced by the deterministic generator in [`internal/bench`](internal/bench/payload.go) and sit inside the 1-2 MiB target the design targets (Spanner offer batches).

- **Order** payload: **1,277,171 bytes (1.22 MiB)** — ~19 fields, 100+ items, populated maps and optionals.
- **Catalog** payload: **2,055,741 bytes (1.96 MiB)** — graph fixture exercising slices-of-nullable, maps-of-nullable, and 2-level struct nesting.

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
go test -bench=. -benchmem ./tools/gsbmcodegen/fixtures/graph/  # Catalog graph fixture

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
