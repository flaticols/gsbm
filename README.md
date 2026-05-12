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
    ID       string  `bin:"1"`
    Quantity int64   `bin:"2"`
    Price    float64 `bin:"3"`
    Note     *string `bin:"4"` // optional
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

## Benchmarks

Numbers below were taken on `darwin/arm64`, Apple M1, `go test -bench=. -benchmem -benchtime=3s`. Payloads are produced by the deterministic generator in [`internal/bench`](internal/bench/payload.go) and sit inside the 1-2 MiB target the design targets (Spanner offer batches).

- **Order** payload: **1,277,171 bytes (1.22 MiB)** — ~19 fields, 100+ items, populated maps and optionals.
- **Catalog** payload: **2,055,741 bytes (1.96 MiB)** — graph fixture exercising slices-of-nullable, maps-of-nullable, and 2-level struct nesting.

### Encode (Order, 1.22 MiB)

| Path | ns/op | MB/s | B/op | allocs/op |
|---|---:|---:|---:|---:|
| `gsbm.Marshal` (exact-size, fresh buffer) | 4,117,663 | 310 | 3,211,335 | **5** |
| Heap, pooled buffer (`sync.Pool`) | 3,479,721 | **367** | 336,006 | **3** |
| Heap, fresh buffer per op (geometric `append` growth) | 4,522,329 | 282 | 6,978,363 | 36 |

`gsbm.Marshal` uses `SizeGSBM` to allocate the output buffer at the exact byte count up front, so it pays no geometric-growth tax even with a fresh buffer per op — half the bytes and one-seventh the allocs of the legacy fresh path. The 5 allocs/op floor is the output buffer plus a transient grow from a nested `BeginLengthDelim` and two map-key scratch slices needed for §5.3 deterministic-order writes. The pooled-buffer path stays faster wall-clock when an external buffer pool is available (the 3 allocs are amortised setup, not per-field).

### Decode (Order, 1.22 MiB)

| Path | ns/op | MB/s | B/op | allocs/op |
|---|---:|---:|---:|---:|
| Heap, cold (fresh `*Order`, no pool) | 5,827,864 | 219 | 8,077,494 | 100,256 |
| Heap, warm (`sync.Pool` + `DecodeInto`) | 4,619,201 | **277** | 932,758 | 45,075 |
| Arena, single-shot (fresh arena per op) | 5,353,283 | 239 | 8,029,477 | 55,423 |
| Arena, pooled arenas | 5,408,189 | 236 | 8,033,017 | 55,440 |

Warm heap decode reuses slice and map capacity through `DecodeInto`, dropping per-op bytes from 8 MiB (cold) to ~900 KiB. Arena decode aliases strings into arena memory (zero-copy strings) so per-string heap allocations disappear, but the slice/map allocations still dominate this fixture. Arena's edge widens dramatically on string-heavy graphs (not exercised here).

### Round-trip (Order, encode + decode in one op)

| Path | ns/op | MB/s | B/op | allocs/op |
|---|---:|---:|---:|---:|
| Heap, pooled buffer + warm receiver | 9,490,520 | **135** | 1,456,795 | 46,226 |

### Catalog (1.96 MiB) — graph fixture with slices-of-nullable + maps-of-nullable

| Path | ns/op | MB/s | B/op | allocs/op |
|---|---:|---:|---:|---:|
| Encode, heap pooled | 3,704,683 | **555** | 139,353 | **2** |
| Decode, heap | 23,676,343 | 87 | 22,912,216 | 384,527 |
| Decode, arena | 21,278,395 | 97 | 22,505,557 | 276,628 |

Catalog's encode-pooled hits 2 allocs/op (essentially the buffer + presence-tracking sidecar). Decode is heavier than Order because the graph fixture intentionally maximises composite-encoding paths (every Section has a slice of nullable Items; every Tag is read through a map with nullable values).

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
- `FuzzHeaderCorruption` — header byte mutations must surface the matching `Err*` sentinel (`ErrBadMagic` / `ErrReservedFlags` / `ErrUnsupportedVer`) and never panic.
- `FuzzArenaDecodeAgainstHeap` — heap and arena decoders must agree on accept/reject and on the decoded values.

```bash
# Run a single fuzz harness for 30 seconds
go test -fuzz=FuzzReaderRobustness -fuzztime=30s ./storage/gsbm/

# Or via the Makefile
FUZZTIME=30s make fuzz
```

## Project layout

```
storage/gsbm/         heap-mode runtime (Writer, Reader, allocator, presence sidecar)
storage/gsbmarena/    arena-mode runtime (Arena, AllocStruct, AllocSlice)
tools/gsbmschema/     schema discovery, validation, classifier
tools/gsbmcodegen/    code generator + golden fixtures
cmd/gsbmschema/       gsbmschema CLI (lint, snapshot, diff, hash, gen, gen-arena)
internal/bench/       deterministic 1-2 MiB payload generator (test-only)
docs/                 spec.md (wire-format specification)
```

## Documentation

- [`docs/spec.md`](docs/spec.md) — wire format specification (the byte layout authority).

## License

See [LICENSE](LICENSE).
