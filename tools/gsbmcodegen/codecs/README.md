# Custom codecs

A field tagged `bin:"N,custom=CodecName"` opts out of the schema-driven
emit path. Codegen looks `CodecName` up in a `codecs.Registry`, resolves
it to a `CodecDecl`, and emits a direct call to user-supplied free
functions instead of recursing into the field's Go type. The wire shape
is whatever the codec declares (`WireVarint`, `WireLengthDelim`, …);
the schema records only the codec name.

This document covers the Go-side contract a codec author writes against.
The on-wire rules are in [`docs/spec.md`](../../../docs/spec.md) §5.8.

## Two codec shapes

A `CodecDecl` declares its body emission in exactly one of two ways.
The choice is dictated by whether the body's byte count is a pure
function of `v` or whether the body must be produced to know its size.

### Analytic — `(SizeFn, EncodeFn)`

For codecs whose body size is computable from `v` without producing
the body. The size pass calls `SizeFn(v)`; the write pass calls
`EncodeFn(w, v)`. The hot path stays branch-free.

Use this shape for fixed-width primitives and anything whose width
follows directly from `v`. `Time` is the canonical example:

```go
func EncodeTime(w *gsbm.Writer, t time.Time) error {
    w.WriteVarint(t.Unix())
    w.WriteUvarint(uint64(t.Nanosecond()))
    return nil
}

func SizeTime(t time.Time) int {
    return gsbm.SizeVarint(t.Unix()) + gsbm.SizeUvarint(uint64(t.Nanosecond()))
}

var TimeDecl = codecs.CodecDecl{
    Name:     "Time",
    GoType:   "time.Time",
    WireType: codecs.WireLengthDelim,
    EncodeFn: "EncodeTime",
    DecodeFn: "DecodeTime",
    SizeFn:   "SizeTime",
}
```

### Materializing — `EmitFn` alone

For codecs whose body size depends on producing the body:
`DecimalString` (`v.String()` decides the byte count), JSON
(`json.Marshal`), or any compression / canonicalization. Codegen
emits a single `EmitFn(w, v, callsite)` call inside `MarshalGSBM`.
The Writer is mode-aware — size-only vs write — so the same function
runs in both passes.

The Writer's per-call scratch cache, keyed by the codegen-emitted
`callsite` id and ordered by occurrence within the pass, makes the
underlying materialization run **exactly once per occurrence per
`gsbm.Marshal` call**. The size pass appends one entry per visit to
that callsite; the write pass walks the same MarshalGSBM body and
reads each entry back in order — slice and map elements at a shared
callsite each get their own materialization rather than aliasing to
the first element. `gsbm.Marshal` threads one Writer through both
passes, so the cache spans the hand-off.

Two Writer helpers cover the common shapes:

| Helper                                                   | Use when materialization returns        |
|----------------------------------------------------------|-----------------------------------------|
| `w.WriteCachedString(callsite, func() string)`           | a string (`v.String()`, `fmt.Sprint`)   |
| `w.WriteCachedBytes(callsite, func() []byte)`            | bytes (`json.Marshal`, `gzip.Compress`) |

Both write the cached payload as a `LENGTH_DELIM` value (varint length
prefix followed by the bytes) — the same wire shape `WriteString` /
`WriteBytes` produce.

`DecimalString` is the canonical example:

```go
func EmitDecimalString[T fmt.Stringer](w *gsbm.Writer, v T, callsite uint64) error {
    return w.WriteCachedString(callsite, func() string { return v.String() })
}

func NewDecimalStringDecl(name, goType, emitFn, decFn, pkgImport string) codecs.CodecDecl {
    return codecs.CodecDecl{
        Name:      name,
        GoType:    goType,
        WireType:  codecs.WireLengthDelim,
        EmitFn:    emitFn,
        DecodeFn:  decFn,
        PkgImport: pkgImport,
    }
}
```

A hypothetical JSON codec follows the same shape with
`WriteCachedBytes`:

```go
func EmitJSON[T any](w *gsbm.Writer, v T, callsite uint64) error {
    return w.WriteCachedBytes(callsite, func() []byte {
        b, _ := json.Marshal(v) // error handling elided for the sketch
        return b
    })
}
```

## Choosing between the two shapes

- Pick **analytic** whenever the size is a pure function of `v`. The
  emitted code is two direct calls per field with no Writer mode
  branch and no cache lookup.
- Pick **materializing** only when computing the size requires
  producing the body. The mode-aware Writer adds a small per-method
  branch the analytic path avoids, and the scratch map costs one map
  lookup per cached call. Both are dwarfed by the materialization
  itself, which is the cost the cache exists to amortize.

The two shapes are mutually exclusive at registration time. Declaring
both `(SizeFn, EncodeFn)` and `EmitFn` on a single `CodecDecl` is
rejected with `codec/conflicting-emit-and-encode`; declaring neither
keeps the existing `codec/missing-size-fn` diagnostic.

## Callsite ids

Materializing codecs take an extra `callsite uint64` parameter.
Codegen emits a unique per-field `const` whose value is the FNV-1a
hash of `<pkg-path>.<struct>.<tag>` as a `uint64` and passes it
inline at every call site. Codec authors never construct callsite ids
themselves — they forward the parameter into `WriteCachedString` /
`WriteCachedBytes`.

Two unrelated materializing-codec fields must use different constants
so their cache entries do not collide; codegen guarantees this by
deriving the id from the field's fully-qualified location. Repeated
visits at one callsite — slice elements, map values reusing the same
nested `MarshalGSBM` — share the same constant safely because the
cache stores per-occurrence entries in walk order rather than a
single shared value.

## Standalone `SizeGSBM` cost

The generated `SizeGSBM()` runs the codec against a fresh
`CountingWriter`, so a standalone `v.SizeGSBM()` call (not via
`gsbm.Marshal`) materializes each materializing-codec field once but
discards the scratch when `CountingWriter` is dropped. A subsequent
`v.MarshalGSBM(otherWriter)` against a separate Writer therefore
materializes again — `gsbm.Marshal` is the path that gives you the
cache benefit because it threads one Writer through both passes.

Analytic codecs have no such concern; `SizeFn` writes nothing.

## Registration

Codecs are registered against a `codecs.Registry` at codegen time,
not runtime. There is no global registry and no `init`-time side
effects. `builtins.NewBuiltinRegistry` returns a fresh Registry
pre-loaded with `Time`; project codecs are added to the same
Registry before it is handed to the emitter. A field referencing an
unregistered codec name fails codegen with `codec/unregistered`; the
diagnostic lists every registered name so typos are obvious.
