## Custom codec diagnostics catalog

Every registration- and codegen-time error a codec author can hit,
keyed by the code that appears in the error string. For the registration
flow these errors guard, see [`lifecycle.md`](lifecycle.md); for the
per-shape contracts, see
[`tools/gsbmcodegen/codecs/README.md`](../../tools/gsbmcodegen/codecs/README.md).

Codes prefixed `codec/...` are stable identifiers — the lint pipeline
matches on them. Plain `codecs: ...` errors come from
`Registry.Register` and predate the prefixed scheme; they are equally
fatal. All sources live in `tools/gsbmcodegen/codecs/registry.go` and
`tools/gsbmcodegen/emit.go`.

The catalog also covers `tag/...` codes the schema validator emits
when a field-tag option is misapplied. Sources live in
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

**Trigger.** A field carries the `bin:"N,type=int32|int64"` wire-width
override on a Go type other than the basic `int`
([`docs/spec.md`](../spec.md) §5.9). The override is only meaningful
on `int`; applying it to a fixed-width integer, a named integer alias,
a string, or any composite is rejected at schema-validation time. Raised
from `tools/gsbmschema/discover.go` during `BuildSchema`.

```go
type Invoice struct {
    Amount int32 `bin:"1,type=int64"` // type= only legal on Go `int`
}
// tag/type-width-mismatch: Invoice.Amount: `bin:"1,type=int64"` —
// the type= width override is only valid on Go `int` fields (got int32)
```

**Fix.** Either drop the `type=` option (the field already has an
explicit width on the Go side), or change the field's Go type to `int`
if the intent is to carry the override. The recommended-practice
paragraph in the README explains when to reach for `type=int64` vs.
just declaring the field as `int64` directly.

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
