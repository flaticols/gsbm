## Body compression (zstd)

`fmtVer = 2` reserves bit 0 of the header `flags` byte
([`docs/spec.md`](../spec.md) §2.1) for body compression. When the bit
is set the body is a zstd frame; when it is clear the body is the raw
bytes a pre-compression encoder would have written. Compression is
strictly opt-in on the writer side; decoders auto-detect via the flag
and decompress transparently.

This document covers when to enable it, the three writer entry points,
the reader contract, the pooling and streaming guarantees the
implementation makes, and the ratio numbers measured on the
repeated-nested benchmark fixture.

For the wire-format rules see [`docs/spec.md`](../spec.md) §2.1.
For the field-level compatibility note see
[`compatibility.md`](compatibility.md#body-compression-bit-0-is-opt-in).

## When to enable compression

The win is real on structured, repetitive payloads — exactly the
shape long-lived storage formats accumulate over time. The cost is
encoder CPU and the loss of byte-equivalent reproducibility for any
caller that pins a hash of the on-disk bytes.

**Enable when:**

- The payload contains many similar nested records (orders with
  line items, batches of telemetry events, catalog snapshots). The
  bench fixture in `internal/bench/repeatednested/` is the canonical
  shape: 2.4–2.6× wire-size reduction at `N ≥ 100`.
- Repeated short strings dominate (airport codes, currency codes,
  feature flags, status enums). zstd's literal dictionary captures
  these on the first occurrence and references them for the rest.
- You are paying for storage or network bandwidth per byte (Spanner
  BYTES columns, Kafka topics, blob storage with egress pricing).
- Batch writes — one encode amortized over many bytes. The encoder
  pool keeps per-call construction cost out of the hot path, but the
  per-byte CPU floor still applies.

**Skip when:**

- Payloads are small (single records under ~1 KiB). zstd's frame
  envelope alone is 9-18 bytes and the dictionary cold start can
  produce a *larger* blob than the raw form. Measure before
  committing.
- The payload is already-binary data with no structure
  (`[]byte` columns carrying JPEG/PNG/MP4/zstd-compressed-elsewhere).
  Recompressing high-entropy bytes adds CPU for ~0% size win.
- Latency on the single-record write path is the load-bearing metric.
  Encode CPU is ~2-3× the uncompressed path on the bench fixture; on
  hot per-record write loops the wall-time tax can outweigh the bytes
  saved.
- Callers downstream pin a hash of the blob bytes for idempotency or
  audit. Compression is byte-deterministic within one zstd library
  version but the bytes differ from the uncompressed form; turning the
  flag on or off is a wire-affecting change for any such consumer.

The decision is per-write, not per-schema. The same root struct can
be written compressed for batch archives and uncompressed for hot
single-record traffic; the reader handles both transparently.

## Writer entry points

Three entry points cover the encode side. `Marshal` is unchanged from
pre-compression — its bytes are byte-identical to today for every
caller. The opt-in lives entirely in `MarshalWithOptions` and
`MarshalToWriter`.

```go
type Options struct {
    Compress bool // if true, body is zstd-compressed and flag bit 0 is set
}

// Unchanged: uncompressed, flags = 0, byte-identical to pre-compression output.
func Marshal(v Marshaler, schemaHint uint16) ([]byte, error)

// Buffered, opt-in compression. Options{} (zero value) is a strict no-op:
// bytes are byte-identical to Marshal. Options{Compress: true} produces a
// zstd-framed body with flags = 0x01.
func MarshalWithOptions(v Marshaler, schemaHint uint16, opts Options) ([]byte, error)

// Streaming. For Options{} delegates to the buffered uncompressed path
// (then a single w.Write). For Options{Compress: true} streams the body
// directly through the pooled zstd encoder so the raw body never
// materializes as a single []byte — see the "raw never materializes"
// section below.
func MarshalToWriter(w io.Writer, v Marshaler, schemaHint uint16, opts Options) error
```

Worked shapes:

```go
// Hot path, no migration: byte-identical to pre-compression output.
blob, err := gsbm.Marshal(&batch, schemaHint)

// Batch archive: opt-in zstd, full blob in memory.
blob, err := gsbm.MarshalWithOptions(&batch, schemaHint,
    gsbm.Options{Compress: true})

// Streaming archive write: blob never fully resident.
err := gsbm.MarshalToWriter(archiveFile, &batch, schemaHint,
    gsbm.Options{Compress: true})
```

`MarshalWithOptions(v, hint, Options{Compress: true})` and
`MarshalToWriter(buf, v, hint, Options{Compress: true})` produce
byte-identical output for the same input. This is structural, not
incidental: both route through the same streaming code path so a
caller switching between them does not perturb the wire bytes.

## Reader side — auto-detect, no API change

`NewReader` and the generated `UnmarshalGSBM` decode compressed and
uncompressed blobs through the same call shape:

```go
var out Batch
r := gsbm.NewReader(blob)
if err := r.ReadHeader(); err != nil { /* ... */ }
if err := out.UnmarshalGSBM(r); err != nil { /* ... */ }
```

`ReadHeader` inspects the flags byte; when bit 0 is set it borrows a
pooled zstd decoder, decompresses the body in place, and replaces the
reader's body buffer with the inflated bytes. Subsequent primitive
reads see no difference from the uncompressed path.

A pre-compression decoder (one written before bit 0 acquired
semantics) reads a compressed blob and rejects with
`ErrReservedFlags` — graceful, by the same rule that rejects any
reserved-bit value. Never silent corruption. This is the load-bearing
forward-compat guarantee from §2.1.

A malformed compressed body (truncated frame, bad zstd magic) surfaces
`ErrCorruptCompressedBody` instead of bubbling raw zstd-library errors;
this is distinct from `ErrTruncated` and `ErrBadVarint`, which apply to
the inflated body once decompression succeeds. The reader never
panics on malformed compressed input — pinned by
`FuzzHeaderCorruption`.

### Streaming read

`NewReaderFrom(r io.Reader)` is the streaming counterpart to
`MarshalToWriter`. It reads the 12-byte header from `r`, pre-validates
magic / fmtVer / reserved-flag-bits before any large allocation, then
reads exactly `bodyLen` body bytes and returns a `*Reader` whose
behavior is identical to `NewReader(headerPlusBody)`. The caller
pattern stays the same:

```go
r, err := gsbm.NewReaderFrom(src)
if err != nil { /* ... */ }
if err := r.ReadHeader(); err != nil { /* ... */ }
if err := out.UnmarshalGSBM(r); err != nil { /* ... */ }
```

`NewReaderFrom` consumes exactly `HeaderSize + bodyLen` bytes from
`src`; trailing bytes are left in the reader for callers framing
multiple blobs back-to-back. The body allocation is sized from the
declared `bodyLen` *before* any body bytes are read, so wrapping `src`
in `io.LimitReader` is NOT a defense against a hostile peer that
declares a 4 GiB `bodyLen` and then closes the stream — the
`make([]byte, bodyLen)` runs first, only then is `io.ReadFull` invoked
against the (limited) source. When `src` is unbounded or untrusted (a
network socket, a stdin pipe), use `NewReaderFromN(src, maxBodyLen)`
instead and pass a `maxBodyLen` matched to your protocol's worst-case
blob size; a declared `bodyLen` exceeding `maxBodyLen` is rejected with
`ErrAllocTooLarge` before any allocation.

`maxBodyLen` bounds the on-disk body only. For an uncompressed blob
that is the only body-shaped allocation, so the bound is total. For a
**compressed** blob it bounds only the on-disk zstd frame; the
subsequent in-place decompression in `ReadHeader` can allocate up to
the pooled decoder's `WithDecoderMaxMemory` cap (~2-4 GiB; see
[`storage/gsbm/compress.go`](../../storage/gsbm/compress.go)). A small,
high-ratio frame ("zstd bomb") that fits under `maxBodyLen` can still
inflate into the GiB range. There is no per-call inflated-size knob in
this iteration: callers needing tighter protection against hostile
compressed input must keep the global decoder cap in mind, pre-filter
inputs, or reject compressed blobs at the framing layer. A
caller-controlled inflated-size limit is a possible follow-up.

## The "raw body never materializes" guarantee

Both compressed entry points (`MarshalWithOptions{Compress: true}`
and `MarshalToWriter{Compress: true}`) route through the same
streaming encoder path, so peak heap is bounded by the compressed
body plus the 8 KiB streaming scratch — the raw body never lands in
any single `[]byte`. The difference is where the compressed bytes
end up: `MarshalWithOptions` returns them as a single slice (so its
peak briefly holds two copies of the compressed body during the
final `bytes.Buffer.Bytes()` hand-off), while `MarshalToWriter`
streams them straight to `w` (compressed-body bound only). Prefer
`MarshalToWriter` when even the compressed body is large enough that
the extra copy matters:

> The streaming compressed path's peak allocation is bounded by
> `O(compressed body)`, not `O(raw body)`. The raw body never lands
> in any single `[]byte`.

The implementation achieves this by:

1. Running a size pass that records each length-delimited region's
   body byte count in `BeginLengthDelim` order. The list of region
   sizes is the only allocation that scales with input shape; it does
   not hold any payload bytes.
2. Running a write pass that uses the recorded sizes to write
   canonical varint lengths up front, so the writer never needs to
   buffer a region body just to patch its length prefix in place.
3. Driving the raw bytes through the pooled zstd encoder directly:
   the encoder's `Write` accumulates compressed bytes into a small
   `bytes.Buffer`; the raw bytes are discarded immediately after
   compression.
4. Chunking large `WriteString` / `WriteBytes` payloads so a single
   multi-MB string cannot grow the internal flush buffer past the
   8 KiB streaming threshold (the geometric `append` realloc would
   otherwise push peak alloc to `O(raw)`).
5. Pinning encoder concurrency to 1
   (`zstd.WithEncoderConcurrency(1)`). zstd's default `GOMAXPROCS`
   workers each maintain a per-goroutine block buffer; for a
   single-payload encode that pushes per-call `TotalAlloc` past the
   raw body and breaks the guarantee.

The peak-allocation budget is pinned by
`TestMarshalToWriterPeakAllocBelowRaw` in
`storage/gsbm/writer_stream_test.go`: on a ~10 MiB raw payload, peak
heap during `MarshalToWriter` is ~0.006× the raw size. The test is
sized large enough that one-time encoder construction costs (1-10 MiB
of hash/match tables on a cold encoder) don't dominate the
measurement.

Buffering the *compressed* body in memory is acceptable (smaller than
raw, single buffer); buffering the *raw* body is not — this is the
explicit contract the streaming path defends.

## Pooling

Encoder and decoder construction is expensive: per-instance hash and
match tables, ~1-10 MiB heap on cold construction. Repeated
`MarshalWithOptions` and `NewReader` calls amortize the cost via
`sync.Pool` instances in [`storage/gsbm/compress.go`](../../storage/gsbm/compress.go):

- Encoders are constructed with `zstd.WithEncoderLevel(zstd.SpeedFastest)`
  and `zstd.WithEncoderConcurrency(1)`. The concurrency knob is
  load-bearing for the streaming peak-alloc guarantee (see above).
- Decoders are constructed with no special options.

Pool reuse is pinned by `TestCompressEncoderPoolReuse` and
`TestCompressDecoderPoolReuse`: 100 paired `Get`/`Put` cycles cap
distinct instances at 8, generous enough to tolerate occasional GC
eviction of pool entries, tight enough to catch the "no reuse at
all" regression.

The same decoder pool is exercised by both the buffered reader path
(`NewReader` + `ReadHeader`) and the streaming reader
(`NewReaderFrom` + `ReadHeader`), so a single instance can amortize
across both call shapes.

## Compression ratio — recorded numbers

Measured on the `internal/bench/repeatednested/` fixture (Apple M1,
`go test ./internal/bench/repeatednested/ -bench=. -benchmem -run=^$
-count=3`, median of 3, `nLinesPerItem=5`, `nTaxesPerLine=2`):

| N    | Uncompressed bytes | Zstd bytes | Ratio (uncomp ÷ zstd) |
|-----:|-------------------:|-----------:|----------------------:|
|   10 |              3,424 |      1,665 |                 2.06× |
|  100 |             34,063 |     14,188 |                 2.40× |
| 1000 |            340,426 |    132,298 |                 2.57× |

The ratio improves monotonically with N — the core hypothesis from
the source ticket. More repetition gives zstd's literal dictionary
more leverage. CPU cost is roughly 2.4× the uncompressed encode
across the sweep; decode CPU is ~1.4×.

Streaming encode (`MarshalToWriter` → `io.Discard`) matches the
buffered encode wall-time within noise and allocates slightly less
per call. The streaming path does not pay an extra buffering layer
for the peak-heap win.

## Compatibility — what changes on the wire

The wire-format consequences of turning compression on are summarized
below. Old/new refer to readers before and after compression shipped;
both share `fmtVer = 2`.

| Writer | Reader | flags | Result |
|--------|--------|-------|--------|
| pre-compression `Marshal` | any | 0 | works |
| new `Marshal` (no opts) | any | 0 | works, byte-identical to pre-compression |
| `MarshalWithOptions{Compress: true}` | pre-compression | 0x01 | clean reject: `ErrReservedFlags` |
| `MarshalWithOptions{Compress: true}` | compression-aware | 0x01 | decompress + decode |
| corrupted bit 1-7 set (e.g. 0x02) | compression-aware | 0x02 | clean reject: `ErrReservedFlags` |

No `fmtVer` bump. The spec preplanned bit 0 for this purpose within
`fmtVer = 2`, so the reader rule widens from "flags must be 0" to
"`(flags & 0xFE)` must be 0" without forcing a version cliff. Reserved
bits 1-7 remain reserved; future flag semantics get their own bit
allocation in a separate spec change.

## Cross-references

- Wire-format rules: [`docs/spec.md`](../spec.md) §2.1.
- Field-level compatibility note: [`compatibility.md`](compatibility.md#body-compression-bit-0-is-opt-in).
- Implementation: [`storage/gsbm/compress.go`](../../storage/gsbm/compress.go),
  [`storage/gsbm/gsbm.go`](../../storage/gsbm/gsbm.go) (`Options`,
  `MarshalWithOptions`, `MarshalToWriter`),
  [`storage/gsbm/reader.go`](../../storage/gsbm/reader.go)
  (`NewReaderFrom`, `ReadHeader` decompress path).
- Bench fixture: [`internal/bench/repeatednested/`](../../internal/bench/repeatednested/).
