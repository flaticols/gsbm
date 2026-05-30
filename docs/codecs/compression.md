## Body compression (zstd + gzip)

`fmtVer = 2` carries a **compression-method enum** in the low three bits
of the header `flags` byte ([`docs/spec.md`](../spec.md) §2.1): `0` = no
compression, `1` = zstd (`SpeedFastest`), `2` = gzip (`DefaultCompression`);
method values 3-7 are reserved. **Bit 3** is the **extended-header** flag:
when set (only on a compressed blob), a 4-byte `inflatedLen` follows the
12-byte base header, making it 16 bytes. Bits 4-7 are reserved. When the
method is non-zero the body is a frame of that codec; when it is zero the
body is the raw bytes a pre-compression encoder would have written.
Compression is strictly opt-in on the writer side; decoders read the
method (and the extended-header flag) from the flags byte and decompress
transparently.

**gzip is the default codec.** A caller that opts into compression without
naming a codec (the deprecated `Options.Compress` bool) gets gzip. gzip
holds a far smaller resident codec working set than zstd (~1.3 MiB vs
~9.9 MiB per instance on the bench fixture), which matters under
concurrency where the codec pool holds several live instances, and it
produces a slightly smaller body on repetitive payloads. zstd decodes
faster and remains available via an explicit `Compression: CompressionZstd`.

This document covers when to enable compression, how to choose a codec,
the writer entry points, the reader contract, the pooling and streaming
guarantees, the decompression-bomb cap, and the ratio numbers measured on
the repeated-nested benchmark fixture.

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
  shape: 2.4–2.9× wire-size reduction at `N ≥ 100`.
- Repeated short strings dominate (airport codes, currency codes,
  feature flags, status enums). Both codecs capture these on the first
  occurrence and reference them for the rest.
- You are paying for storage or network bandwidth per byte (Spanner
  BYTES columns, Kafka topics, blob storage with egress pricing).
- Batch writes — one encode amortized over many bytes. The encoder
  pool keeps per-call construction cost out of the hot path, but the
  per-byte CPU floor still applies.

**Skip when:**

- Payloads are small (single records under ~1 KiB). The frame envelope
  alone (zstd 9-18 bytes, gzip ~18 bytes) plus dictionary/window cold
  start can produce a *larger* blob than the raw form. Measure before
  committing.
- The payload is already-binary data with no structure
  (`[]byte` columns carrying JPEG/PNG/MP4/already-compressed bytes).
  Recompressing high-entropy bytes adds CPU for ~0% size win.
- Latency on the single-record write path is the load-bearing metric.
  Encode CPU is several× the uncompressed path on the bench fixture; on
  hot per-record write loops the wall-time tax can outweigh the bytes
  saved.
- Callers downstream pin a hash of the blob bytes for idempotency or
  audit. Compression is byte-deterministic within one codec-library
  version but the bytes differ from the uncompressed form (and between
  codecs); turning the flag on or off, or switching codecs, is a
  wire-affecting change for any such consumer.

The decision is per-write, not per-schema. The same root struct can
be written compressed for batch archives and uncompressed for hot
single-record traffic; the reader handles every method transparently.

## Choosing a codec

| | zstd (`SpeedFastest`) | gzip (`DefaultCompression`, default) |
|---|---|---|
| Resident memory / codec instance | ~9.9 MiB | **~1.3 MiB** |
| Compressed ratio (repeatednested N=1000) | 2.57× | **2.88×** |
| Encode CPU | lower | higher |
| Decode CPU | **lower** | higher |

Pick **gzip** (the default) when many codec instances are live at once
(high-concurrency servers) or when density matters; the resident
footprint is the dominant difference. Pick **zstd** (explicit
`Compression: CompressionZstd`) when decode latency dominates and the
extra resident memory is affordable. Both are byte-stable within a
library version; neither is "better" — the tradeoff is memory/CPU, which
is why both ship.

## Writer entry points

Three entry points cover the encode side. `Marshal` is unchanged from
pre-compression — its bytes are byte-identical to today for every
caller. The opt-in lives entirely in `MarshalWithOptions` and
`MarshalToWriter`.

```go
type CompressionMethod uint8

const (
    CompressionNone CompressionMethod = 0 // flags 0x00 (uncompressed)
    CompressionZstd CompressionMethod = 1 // flags 0x01 (zstd SpeedFastest)
    CompressionGzip CompressionMethod = 2 // flags 0x02 (gzip DefaultCompression)
)

type Options struct {
    // Deprecated: prefer Compression. true means the default codec (gzip).
    Compress bool
    // Compression selects the body codec. Zero value = CompressionNone.
    Compression CompressionMethod
}

// Unchanged: uncompressed, flags = 0, byte-identical to pre-compression output.
func Marshal(v Marshaler, schemaHint uint16) ([]byte, error)

// Buffered, opt-in compression. Options{} (zero value) is a strict no-op:
// bytes are byte-identical to Marshal. A compressing codec produces a
// framed body with the matching flags byte.
func MarshalWithOptions(v Marshaler, schemaHint uint16, opts Options) ([]byte, error)

// Streaming. For the no-compression case delegates to the buffered
// uncompressed path (then a single w.Write). For a compressing codec it
// streams the body directly through the pooled encoder so the raw body
// never materializes as a single []byte — see the "raw never
// materializes" section below.
func MarshalToWriter(w io.Writer, v Marshaler, schemaHint uint16, opts Options) error
```

Codec resolution: an explicit `Compression` field always wins; otherwise
the deprecated `Compress: true` maps to the default codec (gzip);
otherwise no compression. A reserved `Compression` value (3-7) is rejected
at encode time with `ErrUnsupportedCompression`.

Worked shapes:

```go
// Hot path, no migration: byte-identical to pre-compression output.
blob, err := gsbm.Marshal(&batch, schemaHint)

// Batch archive, default codec (gzip), full blob in memory.
blob, err := gsbm.MarshalWithOptions(&batch, schemaHint,
    gsbm.Options{Compression: gsbm.CompressionGzip})

// Pin zstd explicitly when decode latency dominates.
blob, err := gsbm.MarshalWithOptions(&batch, schemaHint,
    gsbm.Options{Compression: gsbm.CompressionZstd})

// Streaming archive write: blob never fully resident.
err := gsbm.MarshalToWriter(archiveFile, &batch, schemaHint,
    gsbm.Options{Compression: gsbm.CompressionGzip})
```

For any single codec, `MarshalWithOptions(v, hint, opts)` and
`MarshalToWriter(buf, v, hint, opts)` produce byte-identical output for
the same input. This is structural, not incidental: both route through
the same streaming code path so a caller switching between them does not
perturb the wire bytes.

## Reader side — auto-detect, no API change

`NewReader` and the generated `UnmarshalGSBM` decode every method
through the same call shape:

```go
var out Batch
r := gsbm.NewReader(blob)
if _, _, _, err := r.ReadHeader(); err != nil { /* ... */ }
if err := out.UnmarshalGSBM(r); err != nil { /* ... */ }
```

`ReadHeader` reads the compression method (and the extended-header flag)
from the flags byte; when the method is non-zero it borrows the pooled
decoder for that codec, decompresses the body in place — pre-sized and
length-checked against `inflatedLen` when the extended header is present —
and replaces the reader's body buffer with the inflated bytes. Subsequent
primitive reads see no difference from the uncompressed
path.

A reader that predates a method (e.g. a v0.0.5 reader that knows only
zstd) reads a blob written with that method and rejects with
`ErrReservedFlags` — graceful, by the same rule that rejects any
reserved-bit value. Never silent corruption. This is the load-bearing
forward-compat guarantee from §2.1, and the reason behind the
**reader-first upgrade ordering** described in *Compatibility* below.

A malformed compressed body (truncated frame, bad codec magic, or an
inflated stream that exceeds the decode cap) surfaces
`ErrCorruptCompressedBody` instead of bubbling raw codec-library errors;
this is distinct from the wire/varint errors that apply to the inflated
body once decompression succeeds. The reader never panics on malformed
compressed input — pinned by `FuzzHeaderCorruption` and
`FuzzReaderRobustness`.

### Streaming read

`NewReaderFrom(r io.Reader)` is the streaming counterpart to
`MarshalToWriter`. It reads the 12-byte base header from `r`, pre-validates
magic / fmtVer / flags (reserved bits, unknown codecs, and the
extended-header flag) before any large allocation, reads the 4-byte
`inflatedLen` when the extended-header flag is set, then reads exactly
`bodyLen` body bytes and returns a `*Reader` whose behavior is identical to
`NewReader(headerPlusBody)`. The caller pattern stays the same:

```go
r, err := gsbm.NewReaderFrom(src)
if err != nil { /* ... */ }
if _, _, _, err := r.ReadHeader(); err != nil { /* ... */ }
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
**compressed** blob it bounds only the on-disk frame; the subsequent
in-place decompression in `ReadHeader` can allocate up to the decode cap
(`decoderMaxDecompressedSize`, ~2-4 GiB; see
[`storage/gsbm/compress.go`](../../storage/gsbm/compress.go)). A small,
high-ratio frame (a "compression bomb") that fits under `maxBodyLen` can
still inflate up to that cap. There is no per-call inflated-size knob in
this iteration: callers needing tighter protection against hostile
compressed input must keep the global decode cap in mind, pre-filter
inputs, or reject compressed blobs at the framing layer. A
caller-controlled inflated-size limit is a possible follow-up.

## Decompression-bomb cap

Both decoders bound the inflated output to a **global** ceiling
(`decoderMaxDecompressedSize`) so a tiny high-ratio frame cannot drive an
unbounded allocation — the panic-free / bounded hostile-input rule from
[`docs/spec.md`](../spec.md) §8. The mechanism differs by codec:

- **zstd** sets the bound declaratively via
  `zstd.WithDecoderMaxMemory(decoderMaxDecompressedSize)`; the decoder
  rejects an over-cap `Frame_Content_Size` before allocating.
- **gzip** has no equivalent library knob, so the framing layer enforces
  the cap explicitly: it inflates through
  `io.LimitReader(gr, decoderMaxDecompressedSize+1)` and rejects any
  output that reaches the +1 overflow byte. The same ceiling, applied at
  the framing layer.

**Per-blob cap via the extended header.** When a compressed blob carries
`inflatedLen` (flags bit 3, the default for new writes), the decoder bounds
the inflate to that exact length and rejects any frame that inflates to a
different size — a *tighter* cap than the global ceiling, plus an integrity
check that catches a corrupt or mis-sized frame. Crucially, the decoder does
**not** pre-allocate the full declared `inflatedLen`: the speculative
reservation is clamped to `maxInflatePresize` (16 MiB), so a tiny frame that
lies about a multi-GiB `inflatedLen` is rejected by the length check without
ever allocating that much. Bodies at or under the ceiling pre-size exactly,
which is what removes the decompressor's geometric output-buffer growth (the
"2× with zstd" transient).

Either overflow, a `inflatedLen` mismatch, and any frame-corruption error,
collapses into `ErrCorruptCompressedBody`. Pinned by
`TestReadHeaderRejectsDecompressionBomb` (zstd),
`TestReadHeaderRejectsGzipDecompressionBomb` (gzip), and
`TestExtendedHeaderInflatedLenMismatch` /
`TestExtendedHeaderInflatedLenLieIsBounded` (the extended-header exact cap
and the bounded speculative allocation).

## The "raw body never materializes" guarantee

Both compressed entry points route through the same streaming encoder
path regardless of codec, so peak heap is bounded by the compressed
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
   not hold any payload bytes. Generated `MarshalGSBM` bypasses this
   list entirely — see the analytic-size note below — so the recorded
   region count scales with the number of hand-written marshalers and
   opaque types in the graph, not the total number of nested regions.
2. Running a write pass that uses the recorded sizes to write
   canonical varint lengths up front, so the writer never needs to
   buffer a region body just to patch its length prefix in place.
3. Driving the raw bytes through the pooled encoder directly: the
   encoder's `Write` accumulates compressed bytes into a small
   `bytes.Buffer`; the raw bytes are discarded immediately after
   compression. The encoder is reached through a small `bodyCompressor`
   interface (`io.Writer` + `Reset(io.Writer)` + `Close`) that both the
   zstd encoder and the gzip writer satisfy, so the streaming path is
   codec-agnostic.
4. Chunking large `WriteString` / `WriteBytes` payloads so a single
   multi-MB string cannot grow the internal flush buffer past the
   8 KiB streaming threshold (the geometric `append` realloc would
   otherwise push peak alloc to `O(raw)`).
5. Pinning encoder concurrency to 1 (`zstd.WithEncoderConcurrency(1)`;
   gzip is single-stream by construction). zstd's default `GOMAXPROCS`
   workers each maintain a per-goroutine block buffer; for a
   single-payload encode that pushes per-call `TotalAlloc` past the
   raw body and breaks the guarantee.

The peak-allocation budget is pinned by
`TestMarshalToWriterPeakAllocBelowRaw` in
`storage/gsbm/writer_stream_test.go`: on a ~10 MiB raw payload, peak
heap during `MarshalToWriter` is a small fraction of the raw size.

Buffering the *compressed* body in memory is acceptable (smaller than
raw, single buffer); buffering the *raw* body is not — this is the
explicit contract the streaming path defends.

### Generated code skips the recording-region list

On the streaming compressed path, generated `MarshalGSBM` contributes
**zero allocations** from length-prefix bookkeeping. Each generated
nested struct, slice, and map computes its body size analytically via
`SizeGSBM()` and emits the length prefix with `Writer.WriteLength(n)`,
which writes the varint inline without touching the `recordedRegions`
slice that `BeginLengthDelim` appends to. The recording-region machinery
is still used — and still allocates — for hand-written marshalers,
opaque-struct payloads, and any custom codec whose body size isn't
analytically known ahead of time.

## Pooling

Codec construction is expensive (per-instance tables and buffers), so
repeated `MarshalWithOptions` and `NewReader` calls amortize the cost via
`sync.Pool` instances in [`storage/gsbm/compress.go`](../../storage/gsbm/compress.go),
one pool per codec, reached through the `getCompressor`/`putCompressor`
(encode) and `decompressBody` (decode) dispatchers:

- **zstd** encoders are constructed with
  `zstd.WithEncoderLevel(zstd.SpeedFastest)` and
  `zstd.WithEncoderConcurrency(1)` (the concurrency knob is load-bearing
  for the streaming peak-alloc guarantee); decoders with
  `WithDecoderConcurrency(1)` and `WithDecoderMaxMemory(...)`.
- **gzip** writers are constructed at `gzip.DefaultCompression`; readers
  are bare `*gzip.Reader` values bound to their source via `Reset` on
  borrow.

`putCompressor` unbinds the downstream writer (`Reset(nil)`) before
returning a codec to its pool, so a pooled instance never pins the
caller's output buffer. Pool reuse is pinned by
`TestCompressEncoderPoolReuse`, `TestCompressDecoderPoolReuse`, and
`TestGzipEncoderPoolReuse`: paired `Get`/`Put` cycles cap distinct
instances well below the iteration count.

The decoder pools are exercised by both the buffered reader path
(`NewReader` + `ReadHeader`) and the streaming reader
(`NewReaderFrom` + `ReadHeader`), so a single instance amortizes across
both call shapes.

## Compression ratio — recorded numbers

Measured on the `internal/bench/repeatednested/` fixture
(`go test ./internal/bench/repeatednested/ -bench='RepeatedNested_(Zstd|Gzip)'
-benchmem -run=^$`, `nLinesPerItem=5`, `nTaxesPerLine=2`):

| N    | Uncompressed bytes | zstd bytes | zstd ratio | gzip bytes | gzip ratio |
|-----:|-------------------:|-----------:|-----------:|-----------:|-----------:|
|   10 |              3,412 |      1,665 |      2.05× |      1,577 |      2.16× |
|  100 |             34,051 |     14,188 |      2.40× |     12,524 |      2.72× |
| 1000 |            340,414 |    132,298 |      2.57× |    118,391 |      2.88× |

gzip produces a ~10% smaller body than zstd across the sweep, at higher
encode CPU; decode CPU is comparable. Both ratios improve monotonically
with N — more repetition gives the codec more leverage. The headline
reason to prefer gzip is the **resident codec footprint**: a fresh gzip
instance allocates ~1.3 MiB versus ~9.9 MiB for zstd, so under
concurrency (where the pool holds several live instances) gzip's pool
resident set is several× smaller. Per-op allocations are ~0 for both once
the pool is warm — the memory win is resident footprint, not per-op churn.

## Compatibility — what changes on the wire

All forms share `fmtVer = 2`. Uncompressed (0x00) and the **legacy**
12-byte compressed forms (0x01 zstd, 0x02 gzip) keep their original bytes,
so every such blob written by an earlier encoder round-trips byte-for-byte.
New compressed writes set the **extended-header flag** (bit 3), so a new
zstd blob is `0x09` and a new gzip blob `0x0A`, each with a 16-byte header
carrying `inflatedLen`. Bit 3 is additive within `fmtVer = 2`: a reader
that predates it sees it as a reserved high bit and rejects cleanly.

| Writer | Reader | flags | Result |
|--------|--------|-------|--------|
| pre-compression `Marshal` | any | 0x00 | works |
| new `Marshal` (no opts) | any | 0x00 | works, byte-identical to pre-compression |
| legacy zstd (12-byte) | v0.0.5+ (zstd-aware) | 0x01 | decompress + decode |
| legacy zstd (12-byte) | pre-compression reader | 0x01 | clean reject: `ErrReservedFlags` |
| new zstd (extended) | pre-extended reader | 0x09 | clean reject: `ErrReservedFlags` |
| new zstd (extended) | extended-aware reader | 0x09 | pre-size + decompress + decode |
| new gzip (extended, default) | pre-extended reader | 0x0A | clean reject: `ErrReservedFlags` |
| new gzip (extended, default) | extended-aware reader | 0x0A | pre-size + decompress + decode |
| reserved method/high bit (e.g. 0x04, 0x10), or bit 3 on 0x00 (0x08) | extended-aware reader | — | clean reject: `ErrReservedFlags` |

No `fmtVer` bump: the spec carries an extensible flags byte within
`fmtVer = 2`, so the extended-header bit widens the accepted form set
without a version cliff — the same mechanism that admitted each codec.
Reserved methods (3-7) and high bits (4-7) remain reserved; bit 3 is valid
only on a compressed blob.

### Operational gotcha: upgrade readers before writers

New compressed writes set bit 3 for **both** codecs (0x09 zstd, 0x0A gzip),
so any new compressed blob is rejected by a reader that predates the
extended header (`ErrReservedFlags`). In a mixed-version fleet, **upgrade
all readers before upgrading writers** (or before flipping compression on).
Uncompressed blobs and the legacy 12-byte compressed forms keep decoding
everywhere; only the new extended-header forms introduce the ordering
constraint — and it now binds zstd too, not just the gzip default. A writer
that must keep producing blobs readable by a pre-extended reader during the
rollout would need to emit a legacy 12-byte frame (no `inflatedLen`); the
shipped writer always emits the extended header. This is regression-guarded
by `TestV005ReaderRejectsGzipBlob` (which now also pins that legacy 12-byte
zstd stays readable while new 0x09/0x0A are rejected).

## Borrow-strings interaction

When the reader decompresses a body it allocates a fresh buffer to hold the
inflated bytes; that buffer — not the compressed input — is what
borrow-strings decoders alias, for any codec. Callers using
`//gsbm:borrow-strings` on a compressed blob therefore must hold the
immutability/no-reuse contract on the decompressed buffer, not on the
compressed bytes they passed in. (GC reachability is automatic — the
borrowed strings carry pointers into the decompressed buffer, so the
allocation stays live as long as any borrowed value does; the contract is
purely about not mutating or recycling it.)

`Reader.BorrowSource()` returns whichever buffer the reader is currently
exposing for borrow-string aliasing: the original `src` for uncompressed
blobs, the decompressed body for compressed blobs. Always safe to call,
zero allocations, identical call shape across both paths. Capture it after
`ReadHeader` (or after decode) and return it alongside the decoded value
so the consumer has a single handle to audit against in-place mutation
and pool/scratch reuse. The
[`docs/borrow-strings.md`](../borrow-strings.md#compressed-payloads--borrow-strings)
worked `DecodeWithBody` example shows the full pattern.

## Cross-references

- Wire-format rules: [`docs/spec.md`](../spec.md) §2.1.
- Field-level compatibility note: [`compatibility.md`](compatibility.md#body-compression-bit-0-is-opt-in).
- Implementation: [`storage/gsbm/compress.go`](../../storage/gsbm/compress.go),
  [`storage/gsbm/gsbm.go`](../../storage/gsbm/gsbm.go) (`CompressionMethod`,
  `Options`, `MarshalWithOptions`, `MarshalToWriter`),
  [`storage/gsbm/reader.go`](../../storage/gsbm/reader.go)
  (`NewReaderFrom`, `ReadHeader` decompress path).
- Bench fixture: [`internal/bench/repeatednested/`](../../internal/bench/repeatednested/).
- Borrow-strings lifetime contract: [`docs/borrow-strings.md`](../borrow-strings.md).
