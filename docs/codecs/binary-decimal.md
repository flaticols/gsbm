# Binary decimal codec

The binary decimal codec (`DecimalBinary`) encodes a decimal value as a
pair of varints — coefficient and packed scale+sign — rather than as its
decimal string. It is the allocation-free alternative to the text-form
`DecimalString` / `DecimalAppend` codecs and resolves
[issue #44](https://github.com/flaticols/gsbm/issues/44).

Cross-references:

- Wire format: [`docs/spec.md`](../spec.md) §5.8 ("Binary decimal codec
  wire shape").
- Codec shape contract: [`tools/gsbmcodegen/codecs/README.md`](../../tools/gsbmcodegen/codecs/README.md).
- Reference implementation: `EncodeDecimalBinary` / `SizeDecimalBinary` /
  `DecodeDecimalBinary` and `NewDecimalBinaryDecl` in
  [`tools/gsbmcodegen/codecs/builtins/builtins.go`](../../tools/gsbmcodegen/codecs/builtins/builtins.go).
- Testing patterns: [`docs/codecs/testing.md`](testing.md).

## Why a binary codec

The #32 investigation measured decimal-heavy encode as 99% codec text
materialization: `String()` / `AppendText` feeding a `strings.Builder`,
one string allocation per decimal field. A real-world payload driven by
`govalues/decimal` through a text codec sat at ~37,000 allocs/op — and
every one of those allocations was a stringified decimal.

A `(coefficient, scale, sign)` binary encoding never materializes a
string. It touches only the value's allocation-free accessors and the
Writer's varint primitives, so the decimal codec path drops to **0
allocs/op**. In the `internal/bench/money` benchmark (1000 lines × 3
decimal fields) the codec allocations fall from String=3445 / Append=3273
to Binary=4 allocs/op, and Binary stays at 4 when the line count is cut
to 250 — confirming the per-decimal allocation is genuinely zero and the
residual 4 is fixed `Marshal` overhead.

## Codec kind: analytic

`DecimalBinary` is an **analytic** codec — it declares `SizeFn` +
`EncodeFn` and joins `Time` in that pair shape. The body size is

```text
SizeUvarint(coef) + SizeUvarint(scale<<1 | signbit)
```

a pure function of the value, computable without producing the body.
That is the structural reason the binary codec can be allocation-free:

- **Analytic** (`DecimalBinary`, `Time`): size is a function of `v`.
  Codegen emits `SizeFn(v)` in the size pass and `EncodeFn(w, v)` in the
  write pass — no callsite, no scratch cache, nothing materialized.
- **Materializing-cached** (`DecimalString`, `DecimalAppend`): size
  depends on *producing* the body (the decimal string), so the body is
  materialized once and retained in the Writer's per-call scratch cache
  across the size and write passes. The materialization is the
  allocation.
- **Streaming** (`StreamingJSON`): size depends on producing the body
  and the body is too large to retain — materialized twice, never
  cached.

The text decimal codecs are materializing-cached *precisely because*
their size depends on the string. The binary codec sidesteps that: with
no string in the picture, size is analytic and the hot path is
branch-free and allocation-free. For any decimal-like type, the binary
codec is the codec to reach for unless a human-readable on-wire form is
a hard requirement.

## Wire shape

The codec writes a LENGTH_DELIM body of exactly two canonical uvarints
(the field tag and length prefix are added by codegen):

```text
body = uvarint(coef)                 // uint64 coefficient, 1-10 bytes
     ++ uvarint(scale<<1 | signbit)  // scale + sign packed, ~1 byte
```

- `coef` is the **unsigned** coefficient — the significant digits as an
  integer. `uint64` covers the full 19-digit range `govalues` caps at.
- The sign occupies **bit 0** of the second uvarint (`1` = negative).
- `scale` (the count of fractional digits) occupies the high bits;
  decode recovers `scale = packed >> 1`, `neg = packed & 1`.

The sign packs into `scale`, not into `coef`, because `coef << 1` would
overflow `uint64` for a 19-digit coefficient, whereas `scale` is small
enough that `scale<<1 | signbit` is a single varint byte for any
realistic value. `scale` must lie in `[0, 2^62)`; a value outside that
range does not round-trip the `<<1` packing and is rejected with
`codec/DecimalBinary: scale out of range`.

The body is 2–11 bytes — comparable in size to the text form
`"123.45"`, but with zero allocation because nothing is materialized.

## The decode wrinkle

`DecodeDecimalBinary` hands the caller `(coef uint64, scale int, neg
bool)` and a user-supplied `reconstruct` callback builds the concrete Go
decimal type from those parts. gsbm core stays agnostic of the decimal
library — the concrete type is not part of the wire contract.

The wrinkle is in the binding code, not in gsbm: a 19-digit decimal has
a coefficient `≥ 2^63`, which does **not** fit a signed `int64`. A
library whose constructor takes a signed value — e.g.
`govalues.New(value int64, scale int)` — cannot accept such a
coefficient directly; the binding must route large coefficients through
a `uint64`-accepting constructor or a string fallback. A library with a
`uint64` or big-integer constructor passes `coef` straight through. This
lives entirely in the user's `reconstruct` function.

## Binding a decimal type

The codec is generic over the structural interface

```go
type BinaryDecimal interface {
    Coef() uint64
    Scale() int
    IsNeg() bool
}
```

which is the minimal accessor set every mainstream Go decimal library
exposes — `govalues/decimal.Decimal`, `shopspring/decimal.Decimal`, and
`cockroachdb/apd.Decimal` all provide equivalent methods. gsbm itself
takes **no third-party decimal dependency**; the user adds the import in
their own module and registers the codec with `NewDecimalBinaryDecl`:

```go
reg.Register(builtins.NewDecimalBinaryDecl(
    "DecimalBinary",
    "myapp/v1.Decimal",
    "EncodeDecimal",  // user-written wrapper: calls EncodeDecimalBinary
    "DecodeDecimal",  // user-written wrapper: calls DecodeDecimalBinary
    "SizeDecimal",    // user-written wrapper: calls SizeDecimalBinary
    "myapp/v1",
))
```

The wrappers are thin: each calls the corresponding `*DecimalBinary`
builtin and supplies the type-specific `reconstruct` callback on decode.
The `customcodec` fixture
([`tools/gsbmcodegen/fixtures/customcodec/`](../../tools/gsbmcodegen/fixtures/customcodec/))
is the worked example — it binds `DecimalBinary` to a local
`DecimalAmount` type and exercises it through round-trip and golden
tests. Because the accessor set is identical across libraries, the same
`NewDecimalBinaryDecl` binds `shopspring/decimal` or `cockroachdb/apd`
unchanged.
