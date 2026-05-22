## Money/Invoice worked example

This page walks one realistic domain — an `Invoice` of `Money` lines —
through all three custom-codec kinds: analytic, materializing-cached,
and streaming. The theory (when each kind exists and what its
`CodecDecl` looks like) lives in the codec author's reference at
[`tools/gsbmcodegen/codecs/README.md`](../../tools/gsbmcodegen/codecs/README.md);
the wire-format rules are [`docs/spec.md`](../spec.md) §5.8. This
example only applies them.

The procedural side — write a codec, register it, run codegen, commit
the golden — is in [`docs/codecs/lifecycle.md`](./lifecycle.md). What
to look for in the benchmark output is in
[`docs/codecs/performance.md`](./performance.md).

## Domain types

`Money` carries an amount, a currency code, and a capture timestamp.
`Invoice` carries three slices of money lines plus an opaque
attachment blob. Each field deliberately picks a different codec
kind so the example shows all three side-by-side.

```go
package billing

import "time"

// Decimal is a stand-in for a third-party fixed-point decimal type.
// It exposes the two text-form methods relevant to text-encoded
// codecs: String returns the canonical decimal form, AppendText
// writes the same bytes into a caller-supplied buffer. Mirrors the
// shape used in the codec fixture (DecimalAmount in
// tools/gsbmcodegen/fixtures/customcodec/codec.go) so a reader who
// clicks through sees the same idea.
type Decimal struct {
    Negative bool
    Integer  string // digits left of the point
    Fraction string // digits right of the point, no dot
}

// AppendText writes the canonical decimal form into dst.
// String returns the same bytes; the fixture's DecimalAmount
// shows the implementation.
func (d Decimal) AppendText(dst []byte) ([]byte, error) { /* see fixture */ }
func (d Decimal) String() string                        { /* see fixture */ }
func ParseDecimal(s string) (Decimal, error)            { /* see fixture */ }

//gsbm:root
type Money struct {
    Amount   Decimal   `bin:"1,custom=DecimalAppend"` // materializing-cached
    Currency string    `bin:"2"`                      // built-in string
    Captured time.Time `bin:"3,custom=Time"`          // analytic (built-in Time)
}

//gsbm:root
type Invoice struct {
    Lines           []Money `bin:"1"`
    Taxes           []Money `bin:"2"`
    Adjustments     []Money `bin:"3"`
    LargeAttachment []byte  `bin:"4,custom=StreamingJSON"` // streaming
}
```

`Currency` has no `custom=` tag because the built-in string codec
already produces the right wire shape (`LENGTH_DELIM` + raw bytes,
per §4.4) and its size is `len(s)` — analytic by construction
without anyone writing a `CodecDecl` for it.

## Codec registration

Codecs are bound to user types at codegen time, against a
`codecs.Registry`. `builtins.NewBuiltinRegistry` returns a fresh
registry with `Time` already registered; the two `New…Decl`
constructors below add the `Decimal`-bound `DecimalAppend` and the
`[]byte`-bound `StreamingJSON`.

```go
package billing_codecs

import (
    "go.flaticols.dev/gsbm/storage/gsbm"
    "go.flaticols.dev/gsbm/tools/gsbmcodegen/codecs"
    "go.flaticols.dev/gsbm/tools/gsbmcodegen/codecs/builtins"
    "myapp/billing"
)

func Registry() (*codecs.Registry, error) {
    reg := builtins.NewBuiltinRegistry()
    if err := reg.Register(builtins.NewDecimalAppendDecl(
        "DecimalAppend",
        "myapp/billing.Decimal",
        "EmitDecimalAppend",   // wrapper in this package
        "DecodeDecimal",       // wrapper in this package
        "myapp/billing_codecs",
    )); err != nil {
        return nil, err
    }
    if err := reg.Register(builtins.NewStreamingJSONDecl(
        "StreamingJSON",
        "[]byte",
        "StreamAttachment",    // wrapper in this package
        "DecodeAttachment",    // wrapper in this package
        "myapp/billing_codecs",
    )); err != nil {
        return nil, err
    }
    return reg, nil
}

// Wrappers bind the generic builtin templates to the user's concrete
// types; codegen renders calls to these names, not to the builtins
// directly.

func EmitDecimalAppend(w *gsbm.Writer, v billing.Decimal, cs uint64) error {
    return builtins.EmitDecimalAppend(w, v, cs)
}

func DecodeDecimal(r *gsbm.Reader, v *billing.Decimal) error {
    return builtins.DecodeDecimalString(r, v, billing.ParseDecimal)
}

func StreamAttachment(w *gsbm.Writer, v []byte) error {
    return builtins.StreamJSONBytes(w, v)
}

func DecodeAttachment(r *gsbm.Reader, v *[]byte) error {
    return builtins.DecodeJSONBytes(r, v)
}
```

The fixture's
[`tools/gsbmcodegen/fixtures/customcodec/regen_golden_test.go`](../../tools/gsbmcodegen/fixtures/customcodec/regen_golden_test.go)
shows the same three-codec registry built in a test harness.

## Why each kind for each field

| Field             | Kind                  | Reason                                                                                                                                            |
|-------------------|-----------------------|---------------------------------------------------------------------------------------------------------------------------------------------------|
| `Money.Amount`    | materializing-cached  | Decimal size requires materializing the text form (`AppendText` decides the byte count). The text is small — a few dozen bytes — so caching wins. |
| `Money.Currency`  | built-in string       | Size is `len(s)` — already analytic via the built-in string codec, no `custom=` needed.                                                            |
| `Money.Captured`  | analytic (`Time`)     | `SizeTime(t)` returns the byte count of `varint(Unix) ++ uvarint(Nanosecond)` without touching the output buffer. Pick analytic whenever possible. |
| `Invoice.Lines/Taxes/Adjustments` | (not a custom codec; built-in slice traversal) | The `[]Money` slice itself uses the built-in slice encoding (§5.2); each `Money` element runs the codecs above. |
| `Invoice.LargeAttachment` | streaming     | The attachment is opaque bytes routed through a transformation (here: `json.Marshal`, e.g. wrapping in a JSON envelope; in production this is more often a compressor whose output size you can't know without producing it). Bodies can exceed a few MiB; retaining them alongside the output buffer would double peak heap. |

A note on the `[]Money` slice fields: each `Money` element holds a
`custom=DecimalAppend` field. All elements within one slice share
one codegen callsite constant (because they appear at the same
source location), but the Writer's scratch cache stores entries
in walk order, so a 10 000-line invoice runs `AppendText` exactly
10 000 times in the size pass and zero times in the write pass — one
materialization per occurrence, not one per callsite. The fixture's
`TestMaterializeOnceBothFlavors`
([`customcodec_test.go:348`](../../tools/gsbmcodegen/fixtures/customcodec/customcodec_test.go))
pins this property.

On the `LargeAttachment` choice: `[]byte` with no `custom=` tag would
encode as a plain `LENGTH_DELIM` byte string, which is the right
answer when the bytes are already final. Pick a streaming codec
here only when a transformation (JSON envelope, gzip, age, …) sits
between the in-memory value and the wire bytes *and* that
transformation's output can be megabytes. For a kilobyte attachment
that needs JSON wrapping, the materializing-cached path is the
better trade — streaming is for the case where retention itself is
the cost.

## Generated codec shape

Codegen renders one call site per field, dispatched on the codec's
kind. The shape below is abbreviated from what
`gsbmcodegen.GenerateWithCodecs` would emit for `Money` against
the registry above (the analogous fixture output is
[`tools/gsbmcodegen/fixtures/customcodec/record_gsbm.go`](../../tools/gsbmcodegen/fixtures/customcodec/record_gsbm.go)).

```go
const csMoney_1 uint64 = 0x... // per-callsite id, FNV-1a of pkg.Type.tag

func (v *Money) MarshalGSBM(w *gsbm.Writer) error {
    // tag 1 Amount — materializing-cached: one call, callsite threaded.
    w.WriteTag(1, gsbm.WireLengthDelim)
    if err := billing_codecs.EmitDecimalAppend(w, v.Amount, csMoney_1); err != nil {
        return err
    }
    // tag 2 Currency — built-in string.
    w.WriteTag(2, gsbm.WireLengthDelim)
    w.WriteString(v.Currency)
    // tag 3 Captured — analytic: SizeFn then EncodeFn, no Writer-mode branch.
    w.WriteTag(3, gsbm.WireLengthDelim)
    w.WriteUvarint(uint64(builtins.SizeTime(v.Captured)))
    if err := builtins.EncodeTime(w, v.Captured); err != nil {
        return err
    }
    return w.Err()
}
```

`Invoice.LargeAttachment` emits the shortest call site of the three —
one `StreamFn(w, v)` call, no callsite constant, no size helper:

```go
w.WriteTag(4, gsbm.WireLengthDelim)
if err := billing_codecs.StreamAttachment(w, v.LargeAttachment); err != nil {
    return err
}
```

The cost picture follows directly from the call shapes: analytic is
two direct calls (`SizeFn` then `EncodeFn`), materializing-cached is
one call carrying a `callsite uint64` keyed against the Writer's
per-pass scratch, streaming is one call with no caching that runs
twice per `gsbm.Marshal`.

## Round-trip test

```go
package billing_test

import (
    "bytes"
    "testing"
    "time"

    "go.flaticols.dev/gsbm/storage/gsbm"
    "myapp/billing"
)

func TestInvoiceRoundTrip(t *testing.T) {
    in := billing.Invoice{
        Lines: []billing.Money{
            {
                Amount:   billing.Decimal{Integer: "12", Fraction: "500"},
                Currency: "EUR",
                Captured: time.Unix(1_700_000_000, 0).UTC(),
            },
            {
                Amount:   billing.Decimal{Negative: true, Integer: "0", Fraction: "25"},
                Currency: "EUR",
                Captured: time.Unix(1_700_000_001, 0).UTC(),
            },
        },
        Taxes:           nil,
        Adjustments:     nil,
        LargeAttachment: []byte(`{"contract":"R-99"}`),
    }
    buf, err := gsbm.Marshal(&in, 0)
    if err != nil {
        t.Fatalf("Marshal: %v", err)
    }
    var out billing.Invoice
    if err := out.UnmarshalGSBM(gsbm.NewReader(buf)); err != nil {
        t.Fatalf("UnmarshalGSBM: %v", err)
    }
    if len(out.Lines) != 2 {
        t.Fatalf("Lines: got %d, want 2", len(out.Lines))
    }
    if out.Lines[0].Amount != in.Lines[0].Amount {
        t.Errorf("Lines[0].Amount: got %+v, want %+v", out.Lines[0].Amount, in.Lines[0].Amount)
    }
    if !out.Lines[0].Captured.Equal(in.Lines[0].Captured) {
        t.Errorf("Lines[0].Captured: got %v, want %v", out.Lines[0].Captured, in.Lines[0].Captured)
    }
    if !bytes.Equal(out.LargeAttachment, in.LargeAttachment) {
        t.Errorf("LargeAttachment: got % x, want % x", out.LargeAttachment, in.LargeAttachment)
    }
}
```

The cross-kind properties this example relies on — materialize-once
for cached, materialize-twice for streaming, wire-byte parity across
kinds — are pinned at fixture scope by `TestMaterializeOnceBothFlavors`,
`TestStreamingMaterializeTwice`, and `TestWireBytesEqualAcrossThreeKinds`
in [`tools/gsbmcodegen/fixtures/customcodec/customcodec_test.go`](../../tools/gsbmcodegen/fixtures/customcodec/customcodec_test.go).
Project codecs that reuse the builtins inherit those guarantees and
do not need to re-prove them.

## Benchmark

A batch-encode benchmark over many invoices is the right shape to
expose the three kinds' per-field costs against each other. The
materializing-cached field amortizes its `AppendText` over the
two-pass walk; the analytic field is branch-free; the streaming
field runs twice but allocates only the per-pass body.

```go
func BenchmarkEncodeInvoiceBatch(b *testing.B) {
    inv := makeInvoice(1000) // 1k Money lines, 1 MiB attachment
    b.ReportAllocs()
    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        if _, err := gsbm.Marshal(&inv, 0); err != nil {
            b.Fatal(err)
        }
    }
}
```

Run it with:

```bash
go test -bench=BenchmarkEncodeInvoiceBatch -benchmem -benchtime=3s
```

The numbers to read from the output — ns/op contribution per kind,
allocs/op from the cached scratch vs the streaming per-pass body,
peak-heap signal — are walked through in
[`docs/codecs/performance.md`](./performance.md). The benchmark
itself just produces the data.

## See also

- [`tools/gsbmcodegen/codecs/README.md`](../../tools/gsbmcodegen/codecs/README.md) — codec author reference (three kinds, registration, callsite ids).
- [`docs/codecs/lifecycle.md`](./lifecycle.md) — end-to-end procedural walkthrough (write codec, register, regenerate, commit golden).
- [`docs/codecs/performance.md`](./performance.md) — interpreting benchmark output across the three kinds.
- [`docs/spec.md`](../spec.md) §5.8 — on-wire rules for `custom=` fields.
- [`tools/gsbmcodegen/fixtures/customcodec/`](../../tools/gsbmcodegen/fixtures/customcodec/) — the canonical three-kind fixture this example mirrors.
