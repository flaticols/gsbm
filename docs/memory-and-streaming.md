# Resident memory & streaming — analysis and roadmap

**Status:** Phase 1 (string interning) and Phase 2 (extended-header
`inflatedLen` to cut the compressed-decode transient) implemented on
`feat/streaming-decode-dedup`. Phase 3 (true streaming decode) remains a
follow-up. The chosen Phase 2 direction was "gzip pre-size + zstd wire
field" — realised as a single extended-header field that serves both codecs.

## Result summary

| Lever | Ask | Outcome |
|---|---|---|
| String interning (Phase 1) | "don't duplicate the same values" | 31.5× dedup fold; decode allocs 40,001→7,106 (5.6×); N=100 decode 3× faster; retained graph ~10% smaller for 3-char codes (more for longer values) |
| Extended-header `inflatedLen` (Phase 2) | "2× memory with zstd" | zstd decode `B/op` 2.07×→**1.33×** of uncompressed; gzip 1.67×→1.33×. The transient over the graph dropped from ~1.16 MB to exactly one raw body (~0.36 MB) — no geometric over-growth |

1.33× is the floor for a non-streaming decode: the body must materialise once
to be parsed. Eliminating that last ~0.33× requires Phase 3 (streaming
decode), deferred.

This note records the analysis behind reducing gsbm's resident-memory
footprint, the three distinct levers it splits into, and why a wire-level
value dictionary was **not** the chosen mechanism.

## The problem, split into the levers that actually move it

The original ask bundled three things — "reduce resident memory",
"streaming support", "2× memory with zstd", and "don't duplicate the same
values like protobuf". Measured against the `internal/bench/repeatednested`
fixture (N=1000: 340 KB raw body, ~1.07 MB decoded graph), these resolve
into **two different memory profiles** that need **two different fixes**:

| Symptom | Profile | Lever |
|---|---|---|
| "2× memory with zstd" | decode-time **peak** (transient) | pre-size / stream the inflate |
| "don't duplicate the same values" | **retained** graph | string interning |

Key finding: **the retained decoded graph is identical whether the blob was
uncompressed, zstd, or gzip** — `ReadHeader` inflates the body into a
transient buffer that is GC'd once decode returns, leaving the same Go
objects. So:

- The **"2× with zstd"** is *not* in the retained graph. It is the
  decode-time transient: `decompressBody` calls `zstd.DecodeAll` /
  `io.ReadAll`, which inflate the **entire** body into one buffer **and
  over-grow it geometrically**, held alongside the materialized graph.
  Measured total allocation per decode (N=1000):
  uncompressed **1.07 MB**, zstd **2.23 MB**, gzip **1.80 MB** — the zstd
  transient adds ~1.16 MB (~3.4× the 0.34 MB raw body, from growth waste),
  gzip ~0.73 MB.
- The **duplicate-values** waste *is* in the retained graph: 40,001 decode
  allocations at N=1000, almost all per-string copies of values drawn from
  tiny pools (50 airport + 10 currency + 20 tax codes).

## Phase 1 — string interning (implemented)

`gsbm.Interner` implements the existing `Allocator` interface
(`AcquireString(b []byte) string`). All generated code already routes every
string — plain fields, slice elements, map keys/values, and
`//gsbm:borrow-strings` structs (which call `AcquireString` whenever an
allocator is installed) — through that one seam, so interning needs **no
wire-format change and no codegen change**. Install it via
`r.SetAllocator(in)` or the `DecodeInterned` convenience entry point.

It deduplicates decoded strings: the first occurrence of a distinct byte
sequence is copied once into a shared table; later occurrences return that
same Go string. The decoded graph is `==`-equal to a heap decode (proved by
`FuzzInternedDecodeMatchesHeap`, 1.9M execs); only string backing storage
is shared. Interned strings own their bytes and are immutable, so — unlike
borrow-strings — they are **safe as map keys** and safe after the blob is
reused or freed.

**Measured on repeatednested N=1000:**

| Metric | Heap | Interned |
|---|---|---|
| distinct strings | — | 1,080 of 34,000 occurrences (**31.5× fold**) |
| decode allocations | 40,001 | **7,106** (5.6× fewer) |
| decode time | 2.35 ms | 1.98 ms (N=100: **3× faster**) |
| retained graph | 1,049 KiB | 940 KiB (**~10%** smaller) |

**Honest calibration of magnitude.** The big win is allocation count and
decode throughput (GC pressure). The *resident* win is modest for short
strings: a `string` header (16 bytes) stays in every struct field
regardless — interning only folds the duplicate **backing bytes**. For
3-char codes that is ~10%; for long repeated values (URLs, UUIDs, JSON
fragments, descriptions) it approaches the full duplicate-bytes saving.
Interning does **not** help low-repetition payloads (it adds table overhead
with nothing to fold) — it is an opt-in tool for repeated-value data.

**Lifetime:** `DecodeInterned` uses a fresh per-decode interner (bounded by
the graph, discarded after). A caller may reuse one `Interner` across
decodes for cross-blob dedup, but that retains every distinct string;
`NewInternerN(max)` caps the table as a safety valve.

## Why not a wire-level value dictionary

The "like protobuf, don't duplicate values" phrasing suggests a front-loaded
string table referenced by index (protobuf itself does **not** do this;
the real precedents are Arrow/Parquet dictionary encoding). It was rejected:

1. **A string-typed Go API rehydrates any dictionary back to `string` on
   decode, so the resident result is identical to interning** — the
   16-byte headers still land in every field. The dictionary buys nothing
   over interning on the retained side.
2. **Compression already folds repeated bytes off the wire** (gzip 2.88×,
   zstd 2.57× on this fixture via LZ back-references). A dictionary's
   wire-size win over *compressed* output is marginal.
3. It would cost a **breaking `fmtVer` bump**, codegen changes, a
   classifier/snapshot change, and Rust-port decode complexity.
4. **It conflicts with streaming encode**: a front-loaded dictionary needs
   a full pass over the value set before the first byte can be written —
   the opposite of the streaming-encode guarantee `MarshalToWriter`
   provides today.

Net: interning achieves the dedup outcome the ask wanted, at a fraction of
the cost and risk.

## Phase 2 — extended-header `inflatedLen` (implemented)

The decode transient was the real "2× with zstd" — `DecodeAll`/`io.ReadAll`
inflate the whole body into a transient buffer **and over-grow it
geometrically**. Frame content-size availability (probed on our own output)
decided the mechanism: gzip frames carry an exact trailing `ISIZE`, but our
streaming-`Write` zstd frames carry **no** `FrameContentSize` (`HasFCS=false`),
so zstd cannot be pre-sized from the frame without breaking the
streaming-encode guarantee. Rather than special-case each codec, a single
**extended-header `inflatedLen` field** (flags bit 3, +4 header bytes on
compressed blobs) serves both: the decoder reserves up to a 16 MiB ceiling,
inflates, and rejects any frame whose inflated length ≠ `inflatedLen`. This
also *tightens* the bomb defence (a per-blob exact cap on top of the global
ceiling) — a lying tiny frame is rejected without a huge allocation.

It is additive within `fmtVer = 2`: uncompressed blobs are byte-unchanged;
legacy compressed blobs (flags 0x01/0x02) still decode; new compressed writes
set bit 3 (flags 0x09/0x0A) and are rejected by pre-extended readers via the
existing reserved-bit rule. Cost: new compressed blobs (zstd included) need a
reader upgrade before a writer upgrade — the same "readers first" ordering the
gzip default already implied, now binding for both codecs. See
[`docs/spec.md`](spec.md) §2.1.

## Phase 3 — true streaming decode (deferred)

This is where the "2× with zstd" and "streaming support" asks live. Frame
content-size availability (probed on our own output) shapes the options:

- **gzip** frames carry an exact trailing `ISIZE` (uncompressed size mod
  2³², always present; our bodies are uint32-bounded). The decoder can read
  4 bytes and pre-size the inflate buffer **exactly**, eliminating the
  `io.ReadAll` over-growth — a cheap, safe transient cut for the **default
  codec**, no wire change.
- **zstd** frames from our streaming encoder carry **no** `FrameContentSize`
  (`HasFCS=false`), because the streaming `Write`/`Close` path cannot know
  the size up front. Pre-sizing zstd therefore requires either storing the
  inflated length in the gsbm header (a small additive wire change) or
  switching the buffered encode to `EncodeAll` (which re-materializes the
  raw body, breaking the streaming-encode guarantee).

Three candidate directions, increasing in scope:

1. **Pre-size the inflate buffer (gzip now; zstd via a wire `inflatedLen`
   field).** Cuts the transient from ~2× toward ~1× of raw. Small, contained.
   The zstd half needs the wire change the project pre-authorized.
2. **Caller-controlled inflate cap + reuse.** Decompress into a pooled,
   caller-sized buffer. Bounds and amortizes the transient without a full
   rewrite.
3. **Streaming decode (`NewStreamingReader(io.Reader)`).** Inflate
   incrementally and parse incrementally so the decompressed body **never
   fully materializes**; decode peak becomes `O(window + current field +
   graph)`. This is the genuine "streaming support" ask and the largest
   peak win, but it is a real reader rewrite: every direct `r.buf[r.pos:…]`
   index and the `r.end` bounded-region model become buffered/logical-offset
   reads, and it forces copy-or-intern strings (a streaming buffer cannot be
   aliased by borrow-strings). Highest value, highest risk against
   "everything else still works".

These are sequenced after Phase 1 so the low-risk dedup win lands and is
reviewed independently of the reader rewrite.
