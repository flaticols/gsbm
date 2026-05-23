## Codegen and schema-validation diagnostics catalog

Every registration-, codegen-, and schema-validation-time error a
schema or codec author can hit, keyed by the code that appears in the
error string. For the codec registration flow these errors guard, see
[`lifecycle.md`](lifecycle.md); for the per-shape contracts, see
[`tools/gsbmcodegen/codecs/README.md`](../../tools/gsbmcodegen/codecs/README.md).

Codes prefixed `codec/...` are stable identifiers raised by the codec
registry and emitter — the lint pipeline matches on them. Plain
`codecs: ...` errors come from `Registry.Register` and predate the
prefixed scheme; they are equally fatal. Sources live in
`tools/gsbmcodegen/codecs/registry.go` and `tools/gsbmcodegen/emit.go`.

Codes prefixed `tag/...` are raised by the schema validator when a
field-tag option is misapplied — these are not codec-author-specific
and any schema author can hit them. Sources live in
`tools/gsbmschema/discover.go`.

## codec/conflicting-kinds

**Trigger.** A `CodecDecl` sets functions for more than one of the three
shapes — analytic (`SizeFn`+`EncodeFn`), materializing-cached
(`EmitFn`), streaming (`StreamFn`).

```go
_ = reg.Register(codecs.CodecDecl{
    Name: "Money", GoType: "myapp/v1.Money", WireType: codecs.WireLengthDelim,
    EncodeFn: "EncodeMoney", SizeFn: "SizeMoney",
    EmitFn:   "EmitMoney", // conflicts with SizeFn/EncodeFn
    DecodeFn: "DecodeMoney",
})
// codec/conflicting-kinds: codec "Money": EmitFn is mutually
// exclusive with SizeFn/EncodeFn (...)
```

**Fix.** Pick exactly one shape. If size is a pure function of `v`, keep
`(SizeFn, EncodeFn)`; otherwise keep `EmitFn`, or `StreamFn` for bodies
large enough that retaining them alongside the output is unacceptable.
The decision matrix is in the codec README's "Three codec shapes" table.

## codec/missing-size-fn

**Trigger.** No shape was declared, or the analytic pair is half-set
(`EncodeFn` without `SizeFn`, or vice versa).

```go
_ = reg.Register(codecs.CodecDecl{
    Name: "Money", GoType: "myapp/v1.Money", WireType: codecs.WireLengthDelim,
    EncodeFn: "EncodeMoney", // SizeFn missing
    DecodeFn: "DecodeMoney",
})
// codec/missing-size-fn: codec "Money": SizeFn must be set
// (a `func(v T) int` matching EncodeFn's value shape) — ...
```

**Fix.** Set both `SizeFn` and `EncodeFn`, or drop them and set `EmitFn`
or `StreamFn`. `SizeFn` must return `int` and accept the same value
parameter as `EncodeFn` — the emitter substitutes one for the other at
the call site.

## codec/unregistered

**Trigger.** A field tag `bin:"N,custom=Name"` references a codec name
the codegen `Registry` does not know. Built by `codecs.UnregisteredError`
and raised from the emitter's `resolveCodec`.

```go
type Invoice struct {
    Total Money `bin:"1,custom=Mony"` // typo: should be "Money"
}
// codec/unregistered: codec "Mony" is not registered
// (registered: [DecimalAppend DecimalString Money Time])
```

**Fix.** Register the codec on the `Registry` you pass to the emitter
before generation runs — see lifecycle step 5. The diagnostic prints
the sorted list of registered names so typos surface immediately.

## codec/type-mismatch

**Trigger.** The codec resolves but its `GoType` differs from the
underlying type of the field carrying the `custom=` tag. Pointer
wrappers are stripped and type aliases (`type T = X`) unwrapped before
comparison; distinct named types (`type T X`) are not.

```go
// codec declared as GoType "myapp/v1.Money"
type Invoice struct {
    Total time.Time `bin:"1,custom=Money"` // wrong field type
}
// codec/type-mismatch: codec "Money" expects Go type
// "myapp/v1.Money", field has type "time.Time"
```

**Fix.** Change the field type or pick a matching codec. Leaving
`CodecDecl.GoType` empty bypasses the check (`go build` then catches a
real mismatch); do that only for intentionally type-polymorphic codecs.

## codecs: DecodeFn must be set

**Trigger.** `DecodeFn` is empty. Required unconditionally — every codec
must round-trip.

```go
_ = reg.Register(codecs.CodecDecl{
    Name: "Money", GoType: "myapp/v1.Money", WireType: codecs.WireLengthDelim,
    EmitFn: "EmitMoney", // no DecodeFn
})
// codecs: Money: DecodeFn must be set
```

**Fix.** Implement `DecodeFn(r *gsbm.Reader, dst *T) error` and set its
unqualified name on `CodecDecl.DecodeFn`. The fixture in
`tools/gsbmcodegen/fixtures/customcodec/codec.go` shows the signature
for each shape.

## codecs: unknown WireType

**Trigger.** `CodecDecl.WireType` is empty or not one of `WireVarint`,
`WireFixed64`, `WireFixed32`, `WireLengthDelim`.

```go
_ = reg.Register(codecs.CodecDecl{
    Name: "Money", WireType: "bytes", // not a known label
    EmitFn: "EmitMoney", DecodeFn: "DecodeMoney",
})
// codecs: Money: unknown WireType "bytes" (want "varint",
// "fixed64", "fixed32", or "length-delim")
```

**Fix.** Use one of the four `codecs.Wire*` constants. The wire-format
semantics for each are in [`docs/spec.md`](../spec.md) §5.8.

## codecs: empty codec name

**Trigger.** `CodecDecl.Name` is the empty string; checked before any
other validation.

```go
_ = reg.Register(codecs.CodecDecl{Name: "", /* ... */})
// codecs: empty codec name
```

**Fix.** Set `Name` to the identifier the schema tags reference
(matching `[A-Za-z_][A-Za-z0-9_]*`).

## tag/type-width-mismatch

**Trigger.** A field carries the `bin:"N,type=<width>"` wire-range
override ([`docs/spec.md`](../spec.md) §5.9) in a `(Go type, override)`
pair the schema validator rejects. The contract is sign-compatible and
≤-Go-width (with one platform-sized exception that lets `int`/`uint`/
`uintptr` opt up to the 64-bit wire width). Every other pairing is
illegal. Raised from `tools/gsbmschema/discover.go` during
`BuildSchema`; the central decision lives in
`tools/gsbmschema/wireoverride.go` (`WireOverrideCompat`). The `reason`
substring in the diagnostic identifies which rule the pair failed.

The five reject reasons:

**Redundant widening — override wider than a fixed-width Go type.**
The Go type already bounds the field tighter than the override, so
the override emits dead code. Accepting it would also create two
ways to spell the same wire shape, which muddies the
`field/wire-intent-changed` classifier event.

```go
type Invoice struct {
    Amount int32 `bin:"1,type=int64"` // int32 already bounds tighter
}
// tag/type-width-mismatch: Invoice.Amount: `bin:"1,type=int64"` —
// type= override "int64" is wider than Go type int32 — the override
// would be redundant
```

Fix: drop the `type=` option, or widen the Go type to `int64`.

**Cross-sign — override sign mismatches the Go type's sign.** The
wire shape would have to flip varint ↔ uvarint, which propagates
through hash/classifier/Wire-enum in messy ways and is out of scope
for this option.

```go
type Counter struct {
    Value int `bin:"1,type=uint32"` // signed Go type, unsigned override
}
// tag/type-width-mismatch: Counter.Value: `bin:"1,type=uint32"` —
// type= override "uint32" is unsigned but Go type int is signed
```

Fix: change the Go type to match the override's sign (`uint`), or
change the override to a signed width (`type=int32`/`int64`).

**Override on a float Go type.** IEEE-754 narrowing is precision-loss,
not range-overflow — the wrong shape for this option. The parser only
accepts the eight integer width lexemes, so a literal
`type=float32` is rejected at parse time; an integer override
applied to a float Go type is rejected by the validator.

```go
type Point struct {
    X float64 `bin:"1,type=int32"` // float64 is not an integer
}
// tag/type-width-mismatch: Point.X: `bin:"1,type=int32"` —
// type= override "int32" is only valid on integer Go types (got float64)
```

Fix: drop the `type=` option. If the storage really should be
narrower than `float64`, declare the field as `float32` on the Go side.

**Override on a non-numeric Go type.** Strings, structs, slices,
maps, pointers — none of these have a wire range to narrow.

```go
type Order struct {
    ID string `bin:"1,type=int32"` // string is not an integer
}
// tag/type-width-mismatch: Order.ID: `bin:"1,type=int32"` —
// type= override "int32" is only valid on integer Go types (got string)
```

Fix: drop the `type=` option.

**Unknown / malformed width.** The eight legal width lexemes are
`int8/16/32/64` and `uint8/16/32/64`. Anything else — including
case-mangled spellings (`Int32`) and made-up widths (`int24`,
`float32`) — is rejected at parse time, before validation.

```go
type Counter struct {
    Value int `bin:"1,type=int24"` // not a legal width lexeme
}
// bin tag option "type=int24": wire-type width "int24" not recognized
// (legal values: int8, int16, int32, int64, uint8, uint16, uint32, uint64)
```

Fix: pick one of the eight legal widths.

The recommended-practice paragraph in the README explains when to
reach for `type=<width>` vs. just declaring the field with the
matching Go type directly: prefer changing the Go type when you own
it; reach for the override only when the Go type is fixed (third-party
model, in-progress migration, public API contract) or when narrowing
is part of the domain contract.

## codecs: duplicate registration

**Trigger.** Two distinct `CodecDecl` values share a `Name`. Registering
the same value twice is idempotent — only non-identical second
registrations fail. Wrapped from `ErrDuplicateCodec`.

```go
_ = reg.Register(money1) // OK
_ = reg.Register(money2) // same Name, different EncodeFn
// codecs: duplicate registration: Money already registered as {...}
```

**Fix.** Pick one canonical `CodecDecl` per name — two builds of the
same schema must produce identical bytes. If you need a second wire
shape, register it under a new name.
