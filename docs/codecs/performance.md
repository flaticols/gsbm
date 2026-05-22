## Performance traps catalog

Common allocation pitfalls in hand-rolled custom codecs. The canonical
contract is in
[`tools/gsbmcodegen/codecs/README.md`](../../tools/gsbmcodegen/codecs/README.md);
this page is what to watch for on the encode hot path. Detection
commands assume a benchmark in the same package as the codec.

## Allocation budget

A well-behaved codec, measured with `go test -bench=. -benchmem`,
should hit:

| Shape                | Target            | Precondition                                                                                    |
|----------------------|-------------------|-------------------------------------------------------------------------------------------------|
| Analytic             | 0 allocs/op       | `SizeFn` and `EncodeFn` touch only stack-resident values (no `String()`, no slice growth).      |
| Materializing-cached | 1 alloc/op        | Exactly one materialization buffer survives the size pass; the cache holds the same slice.      |
| Streaming            | 2 allocs/op       | Body materialized once per pass; nothing retained between passes.                               |

Numbers are per codec call, not per `gsbm.Marshal`. A `Marshal` over a
slice of N elements multiplies through. The standalone `SizeGSBM`
budget is separate — see the README's "Standalone `SizeGSBM` cost"
section.

## Traps

### 1. `[]byte(v.String())` in a materializing body

**Symptom.** `-benchmem` shows two extra allocs/op on a materializing
codec — one for the `string`, one for the `[]byte` copy.

**Root cause.** Code that pre-dates the cached-as-string path (PR #34)
funneled everything through `w.WriteBytes`. Calling
`w.WriteBytes([]byte(v.String()))` produces the string, copies it into
a fresh byte slice, then copies again into the cache.

**Fix.** Use the string-form Writer helper directly, or — for hand-
rolled codecs that bypass the cache — `w.WriteString(v.String())`:

```go
// before
w.WriteBytes([]byte(v.String()))

// after, materializing-cached
return w.WriteCachedString(callsite, func() string { return v.String() })

// after, hand-rolled length-prefixed body
w.WriteString(v.String())
```

**Detection.** `go test -bench=BenchmarkEncode -benchmem`. Look for
`allocs/op` dropping by 2 and `B/op` dropping by roughly
`len(v.String())` bytes.

### 2. `v.String()` when `v.AppendText` exists

**Symptom.** One extra alloc/op on the cached path versus the
identical-output `AppendText` variant. The fixture's
`TestRecordWireBytesAppendMatchesString` pins that the two emit
byte-identical wire output.

**Root cause.** `WriteCachedString` keeps the `string` the closure
returned; `WriteCachedAppendBytes` writes straight into the cache's
scratch `[]byte`, so the intermediate `string` value never exists.
Source types that build their text form via `strings.Builder` always
produce one `string`; types that ship `AppendText` skip it.

**Fix.** Prefer `EmitDecimalAppend` (or
`w.WriteCachedAppendBytes`) for any type that implements
`encoding.TextAppender`. See the README's "String vs append for
text-form codecs" section.

**Detection.** `go test -bench=. -benchmem -run='^$'`. Compare
`BenchmarkEncodeString` against `BenchmarkEncodeAppend`. The append
variant should be one alloc/op lower.

### 3. Materializing-cached on a large payload

**Symptom.** Peak resident heap (e.g. `pprof -inuse_space` mid-encode,
or `MaxAlloc` from `runtime.MemStats`) grows ~2× the payload size —
once for the cache entry, once for the output buffer.

**Root cause.** The materializing-cached kind retains the materialized
body in the Writer's scratch cache across the size→write hand-off, so
both the body and the appended output exist in memory simultaneously.
For multi-MiB payloads this dominates peak heap.

**Fix.** Switch to the streaming kind (`StreamFn`). It materializes
twice (size pass + write pass) but retains nothing — 2× CPU on the
codec body for 1× peak heap.
[`./streaming-determinism.md`](./streaming-determinism.md) covers the
byte-stable invariant streaming codecs must hold;
`TestStreamingMaterializeTwice` in the customcodec fixture pins it.

**Detection.** `go test -bench=BenchmarkEncodeLarge -benchmem` across
a range of payload sizes (1 KiB, 1 MiB, 16 MiB). `B/op` for a
materializing-cached codec scales as ~2× payload; streaming scales as
~1× payload.

### 4. `json.Marshal` in an analytic `SizeFn`

**Symptom.** Two `json.Marshal` calls per field per `gsbm.Marshal`,
visible as duplicate marshal cost in CPU profiles.

**Root cause.** Analytic codecs assume `SizeFn(v)` is cheap and pure.
JSON marshaling is neither: the size pass materializes a buffer just
to read `len`, discards it, and the write pass repeats the work.

**Fix.** Use the materializing-cached kind with `EmitFn` (e.g.
`builtins.EmitJSON`-shaped wrapper around `WriteCachedBytes`) so the
materialization runs once and both passes read the same cached buffer.
If the payload may be large, use streaming instead (see trap 3).

**Detection.** `go test -bench=BenchmarkMarshal -benchmem -cpuprofile`.
Look for `encoding/json.Marshal` appearing twice in the profile
samples for one logical encode.

### 5. Varint vs fixed-width for predictable-range integers

**Symptom.** A field that benchmarks fine on small values blows up
wire size on large ones (varint), or stays large even when values are
tiny (fixed-width). No allocation impact; the trap is wire bytes and
decode cost.

**Root cause.** Varint is one byte per 7 value bits — small numbers
are cheap, large numbers cost up to 10 bytes plus per-byte branching
on decode. Fixed-width is constant 4 or 8 bytes with one
little-endian load.

**Fix.** Default to varint. Pick fixed-width (`WireFixed32` /
`WireFixed64`) when the distribution is predictably large (hashes,
opaque ids, full-range counters), when consumers want a fixed offset
for random access, or when alignment matters (mmap'd consumers). The
choice is per-codec via `WireType`; see `docs/spec.md` §5.8.

**Detection.** `go test -bench=BenchmarkWireSize`. Compare `B/op`
across representative value distributions; pick whichever the typical
payload favors.

### 6. Map-key sort allocating a fresh scratch slice

**Symptom.** N extra allocs/op for a map-encoding codec, where N is
the encode count — one fresh `[]K` per call to hold sorted keys.
Usually masked by larger materialization costs; only visible when the
map is small and the keys are cheap.

**Root cause.** The canonical sort-then-encode pattern
(`keys := make([]K, 0, len(m)); for k := range m { ... }; sort.Slice(keys, ...)`)
allocates a fresh slice every call.

**Fix.** Usually: do nothing. For maps with fewer than ~32 keys the
slice alloc is dwarfed by per-key materialization. If a benchmark
shows the slice dominating, pool it via a package-scope `sync.Pool` —
codec functions are stateless `(w, v, [callsite])`, so there is no
codec-instance hook to hang the scratch off.

**Detection.** `go test -bench=BenchmarkEncodeMap -benchmem` across
map sizes. If `allocs/op` is `2×N + c` rather than `N + c`, the
scratch slice is the second factor.

## Worked benchmark

See [`./example-money.md`](./example-money.md) for an end-to-end
Money/Invoice walkthrough that exercises the analytic and cached
shapes against the trap budget above.
