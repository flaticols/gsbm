## Testing patterns for custom codecs

A custom codec ships three observable behaviours: a wire shape, a
materialization schedule, and an error surface. Each has a test pattern
that pins it. This page catalogues the tests a serious codec author
writes — what each one asserts, why it matters, the scaffolding it
takes, and the canonical example in the gsbm fixture
[`tools/gsbmcodegen/fixtures/customcodec/customcodec_test.go`](../../tools/gsbmcodegen/fixtures/customcodec/customcodec_test.go).

Cross-references:

- Codec shape contract: [`tools/gsbmcodegen/codecs/README.md`](../../tools/gsbmcodegen/codecs/README.md).
- Wire format: [`docs/spec.md`](../spec.md) §5.8.
- Streaming determinism in depth: [`docs/codecs/streaming-determinism.md`](streaming-determinism.md).
- Benchmark patterns (this page is correctness only): [`docs/codecs/performance.md`](performance.md).

## 1. Round-trip

**Asserts.** `encode(v)` followed by `decode` produces a value that
compares equal to `v` (field-by-field for structs, `reflect.DeepEqual`
for composites, codec-defined equality for floating types).

**Why it matters.** Round-trip is the bedrock test: any asymmetry between
encoder and decoder — a forgotten field, a swapped uvarint/varint, a
sign-extension bug — surfaces here first. Run it across the value space
the codec was designed for: zero, signed zero, the wire-edge values,
and any input that triggers a different code path inside the codec body.

**Sketch.** Table-driven over the inputs that matter:

```go
func TestRoundTrip(t *testing.T) {
    cases := []struct {
        name string
        in   Record
    }{
        {"happy", Record{ /* ... */ }},
        {"zero-time", Record{CreatedAt: time.Time{}}},
        {"pre-epoch", Record{CreatedAt: time.Unix(-12345, 67890)}},
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            var w gsbm.Writer
            if err := tc.in.MarshalGSBM(&w); err != nil {
                t.Fatalf("MarshalGSBM: %v", err)
            }
            var out Record
            if err := out.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
                t.Fatalf("UnmarshalGSBM: %v", err)
            }
            if !reflect.DeepEqual(out, tc.in) {
                t.Errorf("round-trip diverged: got %+v want %+v", out, tc.in)
            }
        })
    }
}
```

**Reference.** `TestRecordRoundTrip`, `TestRecordRoundTripNilOptional`,
`TestRecordRoundTripPresentZero`, `TestRecordRoundTripPreEpoch`,
`TestRecordRoundTripZeroTime`, `TestRecordRoundTripDecimalEdgeCases`,
`TestRecordRoundTripStreamingPayload` cover this pattern across the
three codec kinds and the pointer-presence wrapper.

## 2. Hand-written wire byte-equality

**Asserts.** Encoding a known value produces an *exact* byte sequence,
constructed by hand from the spec.

**Why it matters.** Round-trip is symmetric: any encode/decode pair
that agrees passes. A hand-rolled golden refuses to agree silently —
a wire-format refactor that swaps two byte ranges, or a length prefix
that switches from outer-then-inner to inner-then-outer, is caught
immediately. Hand-built `want` buffers also serve as living
documentation of the wire layout for the field.

**Sketch.** Build the expected buffer by calling the same Writer
primitives the generated code calls, mirroring spec §5.8:

```go
func TestWireBytesGolden(t *testing.T) {
    in := Record{ /* ... */ }
    var got gsbm.Writer
    if err := in.MarshalGSBM(&got); err != nil {
        t.Fatalf("MarshalGSBM: %v", err)
    }

    var want gsbm.Writer
    want.WriteTag(1, gsbm.WireLengthDelim)
    want.WriteUvarint(uint64(gsbm.SizeVarint(int64(1))))
    want.WriteVarint(int64(1))
    // ... continue for each field, in tag order ...

    if !bytes.Equal(got.Bytes(), want.Bytes()) {
        t.Fatalf("wire bytes mismatch:\n got: % x\nwant: % x",
            got.Bytes(), want.Bytes())
    }
}
```

**Reference.** `TestRecordWireBytes` and
`TestRecordWireBytesPresentOptional` hand-build both the absent-pointer
and present-pointer envelopes for a `Record` with all three codec kinds
attached, and pin each tag's layout against §5.8.

## 3. Materialize-once property (cached codecs)

**Asserts.** A materializing-cached codec
(`WriteCachedString` / `WriteCachedAppendBytes` / `WriteCachedBytes`)
runs its materialization exactly **once per occurrence** across a
`gsbm.Marshal` call — the size pass populates the scratch cache, the
write pass replays it.

**Why it matters.** The whole point of the cached shape is that
materializing the body once is acceptable; materializing it twice is
not. A regression where the callsite id is mis-threaded, the cache is
keyed against the wrong scope, or the write pass falls back to a fresh
materialization invalidates the kind's defining contract. The test
catches that drift the moment the counter exceeds 1.

**Sketch.** Wrap the source value in a probe type whose `String()` /
`AppendText()` increments a counter; run `gsbm.Marshal` through a
hand-rolled receiver that mirrors what codegen would emit; assert the
counter equals the expected occurrence count (1 for a singleton field,
N for an N-element slice).

```go
type probe struct {
    inner  Source
    calls  *int
}
func (p probe) String() string { *p.calls++; return p.inner.String() }

func TestMaterializeOnce(t *testing.T) {
    n := 0
    rec := &probeRecord{p: probe{inner: src, calls: &n}}
    if _, err := gsbm.Marshal(rec, 0); err != nil {
        t.Fatalf("Marshal: %v", err)
    }
    if n != 1 {
        t.Errorf("materializations = %d, want 1", n)
    }
}
```

**Reference.** `TestMaterializeOnceBothFlavors` runs the property
against both `EmitDecimalString` and `EmitDecimalAppend` simultaneously
and asserts `String()` is called exactly once for the string-form field
and `AppendText()` exactly once for the append-form field. The probe
plumbing — `probeDecimal`, `probeRecord`, the reused
`csRecord_2` / `csRecord_4` callsite constants — is the template for
project-specific materialize-once tests.

## 4. Materialize-twice property (streaming codecs)

**Asserts.** A streaming codec (`StreamFn`) runs its body exactly
**twice per occurrence** across a `gsbm.Marshal` call — once in the
size pass, once in the write pass — and retains nothing between them.

**Why it matters.** Streaming is the inverse contract of cached: 2× CPU
for 1× peak heap. A regression that smuggles the body into the scratch
cache silently converts the codec to the cached shape — invisible on
the wire, but the peak-heap win disappears. Conversely, a regression
that runs the body three times (a stray size-then-encode-then-fix-up
path) inflates CPU. The counter check pins both directions.

**Sketch.** Same shape as §3, but with the streaming entry point and
the expected count is 2:

```go
func streamProbe(w *gsbm.Writer, v probePayload) error {
    *v.calls++
    return builtins.StreamJSONBytes(w, v.inner)
}

func TestStreamingMaterializeTwice(t *testing.T) {
    n := 0
    rec := &probeStreamRecord{field: probePayload{inner: p, calls: &n}}
    if _, err := gsbm.Marshal(rec, 0); err != nil {
        t.Fatalf("Marshal: %v", err)
    }
    if n != 2 {
        t.Errorf("streaming body invocations = %d, want 2", n)
    }
}
```

**Reference.** `TestStreamingMaterializeTwice` exercises this exactly,
wrapping `builtins.StreamJSONBytes` in a counting probe and asserting
the body runs twice per `gsbm.Marshal` call.

## 5. Wire byte-equality across kinds

**Asserts.** For a value whose materializations agree across kinds,
the analytic, cached, and streaming codecs produce **byte-identical**
wire output.

**Why it matters.** The three codec kinds share one wire envelope
(LENGTH_DELIM) and one length-prefixing rule. Anything that breaks
the equality — a kind that emits the prefix twice, a kind that
forgets the outer key, a kind that picks a different size encoding —
is a wire-compatibility bug that round-trip will not catch (each kind
round-trips itself just fine; the divergence is across them).

**Sketch.** Construct three hand-rolled receivers that route the same
logical value through each kind, marshal each, and compare bytes
pairwise:

```go
analytic, _ := gsbm.Marshal(&probeAnalyticRecord{v: x}, 0)
cached,   _ := gsbm.Marshal(&probeCachedRecord{v: x}, 0)
streamed, _ := gsbm.Marshal(&probeStreamedRecord{v: x}, 0)
if !bytes.Equal(analytic, cached) || !bytes.Equal(cached, streamed) {
    t.Errorf("cross-kind divergence")
}
```

**Reference.** `TestWireBytesEqualAcrossThreeKinds` builds three
receivers — `probeAnalyticJSONRecord`, `probeCachedJSONRecord`,
`probeStreamedJSONRecord` — around a `jsonStringPayload` whose
`String()` returns its own `json.Marshal` bytes, then asserts all three
produce identical wire output. The construction of a payload type whose
materializations agree across kinds is the trick that makes cross-kind
equality testable; copy that pattern for project codecs.

A related test is `TestRecordWireBytesAppendMatchesString`, which pins
the same equality between the two materializing-cached entry points
(`WriteCachedString` vs `WriteCachedAppendBytes`).

## 6. Unknown-field skip

**Asserts.** A decoder built against an older schema must skip
unknown fields and decode the fields it does know — see
[`docs/spec.md`](../spec.md) §7.1.

**Why it matters.** Codec authors who emit an unusual wire type, or
who add a new field guarded by a custom codec, can accidentally break
forward compatibility: a buggy `SkipField` on the reader side reads
into the next field's tag and corrupts the decode. The test pins the
forward-compat invariant by encoding with one schema and decoding with
another.

**Sketch.** Encode against a "new" version of the record that has an
extra custom-codec field at a high tag, then decode against the "old"
version (the existing fixture) and assert the known fields decode
correctly:

```go
type RecordV2 struct {
    Record
    ExtraTag99 CustomThing `bin:"99,custom=Custom"`
}

func TestUnknownFieldSkip(t *testing.T) {
    var w gsbm.Writer
    v2 := RecordV2{Record: known, ExtraTag99: extra}
    _ = v2.MarshalGSBM(&w)

    var out Record // old schema, no tag 99
    if err := out.UnmarshalGSBM(gsbm.NewReader(w.Bytes())); err != nil {
        t.Fatalf("decode: %v", err)
    }
    if !reflect.DeepEqual(out, known) {
        t.Errorf("known fields corrupted by unknown-tag skip")
    }
}
```

**Reference.** The generated `UnmarshalGSBM` in
[`tools/gsbmcodegen/fixtures/customcodec/record_gsbm.go`](../../tools/gsbmcodegen/fixtures/customcodec/record_gsbm.go)
delegates unknown tags to `reader.SkipField`, which handles the LENGTH_DELIM
envelope every custom codec uses; project tests just need to exercise
the path with a synthesized "future" blob as above.

## 7. Determinism property (streaming)

**Asserts.** A streaming codec produces **byte-identical** output on
both the size pass and the write pass — equivalently, two back-to-back
encodes of the same value produce identical bytes.

**Why it matters.** The streaming kind writes the length prefix
computed in pass 1 ahead of the body produced in pass 2. If the two
passes disagree on the body bytes — `json.Marshal` over an unsorted
map, a codec that consults `time.Now()`, a `sync.Pool` that yields a
buffer with non-zero residue — the length prefix lies about the body
and decoders read past the field boundary. See
[`docs/codecs/streaming-determinism.md`](streaming-determinism.md).

**Sketch.** Encode twice, compare bytes:

```go
func TestStreamingDeterminism(t *testing.T) {
    rec := &probeStreamRecord{field: payload}
    a, err := gsbm.Marshal(rec, 0)
    if err != nil { t.Fatalf("Marshal: %v", err) }
    b, err := gsbm.Marshal(rec, 0)
    if err != nil { t.Fatalf("Marshal: %v", err) }
    if !bytes.Equal(a, b) {
        t.Fatalf("non-deterministic streaming output:\n a: % x\n b: % x", a, b)
    }
}
```

For codecs whose materialization is suspected of being order-dependent
(maps, sets), repeat under `-count=100` or wrap the assertion in a
loop — a single pass against one Go map iteration order is not enough.

**Reference.** `TestRecordRoundTripStreamingPayload` indirectly exercises
this — the round-trip would fail on length-prefix mismatch if the
streaming body diverged across passes. A direct two-encode comparison
is what the standalone determinism test adds; the
[`streaming-determinism.md`](streaming-determinism.md) page lists the
known non-deterministic Go primitives to audit a codec against.

## 8. Allocation budgets

**Asserts.** A specific code path allocates no more than N times per
encode (often 0 or 1).

**Why it matters.** The materializing-cached and streaming kinds make
explicit promises about allocations on cached vs first-occurrence
paths (see [the codec README §materializing](../../tools/gsbmcodegen/codecs/README.md)).
A regression that adds a stray `fmt.Sprintf` or replaces a buffer
reuse with a fresh `make` is silent on the wire and silent on
round-trip, but moves the encode-time allocation count off-budget.
`testing.AllocsPerRun` pins the budget.

**Sketch.**

```go
func TestEncodeAllocations(t *testing.T) {
    rec := &Record{ /* warm value */ }
    avg := testing.AllocsPerRun(100, func() {
        var w gsbm.Writer
        _ = rec.MarshalGSBM(&w)
    })
    if avg > 2 {
        t.Errorf("MarshalGSBM allocations = %.1f, want <= 2", avg)
    }
}
```

Run with `-count=1` and a discard-then-measure warmup if the receiver
or codec lazily initialises a `sync.Pool` — the first iteration
otherwise dominates the average. The fixtures under
[`storage/gsbm/writer_scratch_test.go`](../../storage/gsbm/writer_scratch_test.go)
and [`storage/gsbm/bench_encode_test.go`](../../storage/gsbm/bench_encode_test.go)
show the production shape.

This page is about correctness budgets — bounds you assert. Throughput
and per-op cost belong in benchmarks; see
[`docs/codecs/performance.md`](performance.md).
