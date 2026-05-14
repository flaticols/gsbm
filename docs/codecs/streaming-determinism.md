## Streaming-codec determinism

Streaming codecs (`StreamFn` alone — see [`tools/gsbmcodegen/codecs/README.md`](../../tools/gsbmcodegen/codecs/README.md))
trade peak heap for CPU by running the codec body twice per
`gsbm.Marshal` call: once in the size pass to count bytes for the
length prefix, once in the write pass to emit the body. Nothing is
retained between passes. That arrangement carries one hard invariant.

## The invariant

For a given `v`, the body bytes produced by `StreamFn(w, v)` in the
size-mode pass MUST equal the body bytes produced by `StreamFn(w, v)`
in the write-mode pass, byte for byte.

The size pass commits `len(body)` into the `LENGTH_DELIM` envelope's
varint length prefix; the write pass then writes the body. If the two
passes disagree the writer still happily produces output — there is
no runtime check — but the on-wire length prefix names `N` bytes
while the body that follows is `M ≠ N` bytes. Readers see a malformed
blob: the next field key is parsed from the middle of the body, or
the body absorbs bytes from the following field. Corruption is
silent at encode time and surfaces as decode errors (or worse, as
seemingly-valid garbage) at the other end.

[Spec §5.8](../spec.md) states the same rule on the wire side; this
document is the Go-side author guide for satisfying it.

## Common non-determinism sources

1. **`json.Marshal` of a Go `map[K]V`.** Go's runtime randomizes map
   iteration order, so naive serializers emit entries in a different
   order each pass. `encoding/json` sorts **string-keyed** maps by key,
   so `map[string]V` is safe; `map[int]V`, `map[MyType]V`, and any
   non-string key type are not sorted by stdlib and will differ
   pass-to-pass. Fix: convert to a sorted slice before marshalling, or
   use a marshaller that canonicalizes key order.

2. **Mutable `*time.Location`.** `time.Time` formatters that close
   over a `*time.Location` whose zone data can change mid-encode (rare
   in production, but possible with hot-reloaded tzdata or test
   harnesses that swap `time.Local`) produce different bytes between
   passes. Pin a fixed location (`time.UTC`) inside the codec.

3. **Randomized salts or per-call nonces.** A codec that mixes in
   `crypto/rand`, `math/rand` without a seed, or a UUID generated
   inside `StreamFn` will produce different bytes on each invocation.
   If the payload genuinely needs a nonce, generate it on the source
   value before `gsbm.Marshal` and let the codec read it back out.

4. **External state read during encode.** Reading env vars,
   files, or `time.Now()` inside `StreamFn` makes the output a
   function of the world, not just `v`. The two passes happen in quick
   succession so the world usually agrees, but "usually" is not an
   invariant. Capture any needed state into `v` before encoding.

5. **Goroutine-local state.** Anything that consults a `sync.Pool`,
   a `context.Context` value, or a goroutine-id-keyed map can differ
   between the two passes if the runtime schedules them differently.
   Stick to pure functions of `v`.

## Testing determinism as a property

Encode the same value twice and byte-compare the outputs. The
fixture's `TestStreamingMaterializeTwice` in
[`tools/gsbmcodegen/fixtures/customcodec/customcodec_test.go`](../../tools/gsbmcodegen/fixtures/customcodec/customcodec_test.go)
pins the size pass + write pass count; the matching determinism
property looks like:

```go
func TestStreamCodecDeterministic(t *testing.T) {
    v := /* representative value */
    a, err := gsbm.Marshal(&Record{Field: v}, 0)
    if err != nil {
        t.Fatal(err)
    }
    b, err := gsbm.Marshal(&Record{Field: v}, 0)
    if err != nil {
        t.Fatal(err)
    }
    if !bytes.Equal(a, b) {
        t.Fatalf("non-deterministic encode:\n a: % x\n b: % x", a, b)
    }
}
```

A tighter variant catches intra-marshal drift that inter-marshal
repetition can mask: run `StreamFn` against `gsbm.NewCountingWriter()`
and against a real Writer, and assert `cw.Size() == len(w.Bytes())`.
See `TestStreamJSONBytesSizeMatchesWrite` in
[`tools/gsbmcodegen/codecs/builtins/builtins_test.go`](../../tools/gsbmcodegen/codecs/builtins/builtins_test.go)
and [`docs/codecs/testing.md`](testing.md) for the full property-test
catalogue.

## When to fall back to materializing-cached

If you cannot guarantee determinism — third-party marshaller you do
not control, payload includes a map you cannot sort, codec depends on
state outside `v` — switch the codec from `StreamFn` to `EmitFn`
(materializing-cached). The body is then produced once and held in
the Writer's scratch cache; the size pass and write pass read from
the same bytes, so determinism is no longer a correctness condition.
The cost is ~2× peak heap for the duration of the marshal, paid in
exchange for not having to prove a property about your codec.

See [`docs/codecs/performance.md`](performance.md) for the
heap-vs-CPU trade-off in detail.
