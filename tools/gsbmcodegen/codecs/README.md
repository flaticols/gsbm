# Custom codecs

> The curated authoring guide lives at
> [`docs/codecs/index.md`](../../../docs/codecs/index.md) — start there
> for lifecycle, worked examples, testing patterns, and diagnostics.
> This README is the package-level Go-side contract reference and
> remains the authority on the `CodecDecl` surface.

A field tagged `bin:"N,custom=CodecName"` opts out of the schema-driven
emit path. Codegen looks `CodecName` up in a `codecs.Registry`, resolves
it to a `CodecDecl`, and emits a direct call to user-supplied free
functions instead of recursing into the field's Go type. The wire shape
is whatever the codec declares (`WireVarint`, `WireLengthDelim`, …);
the schema records only the codec name.

This document covers the Go-side contract a codec author writes against.
The on-wire rules are in [`docs/spec.md`](../../../docs/spec.md) §5.8.

## Three codec shapes

A `CodecDecl` declares its body emission in exactly one of three ways.
The choice is dictated by whether the body's byte count is a pure
function of `v`, and — when the body must be produced to know its
size — by how large that materialized body is expected to be.

| Shape                   | Fields on `CodecDecl`     | Pick when                                                                                                                       | Trade-off                                                                                                                  |
|-------------------------|---------------------------|---------------------------------------------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------|
| Analytic                | `SizeFn` + `EncodeFn`     | Size is a pure function of `v` (`len(v.Bytes)`, fixed width, sum of cheap sub-sizes). `Time` is canonical.                      | None on the encode hot path: two direct calls per field, no Writer mode branch, no cache lookup.                           |
| Materializing-cached    | `EmitFn`                  | Computing the size requires producing the body, and the body is small or medium (~< 1 MiB rough heuristic). `DecimalString` is canonical. | Materializes the body once per occurrence per `gsbm.Marshal` call; retains it in the Writer's scratch cache alongside the output buffer → ~2× peak heap. |
| Streaming               | `StreamFn`                | Computing the size requires producing the body, and the body may be large enough that retaining it alongside the output buffer would meaningfully grow peak heap (~1 MiB and up, win grows linearly). `StreamJSONBytes` is canonical. | Materializes the body twice (size pass + write pass), but never retains it → 2× CPU on the codec body, 1× peak heap.       |

### Analytic — `(SizeFn, EncodeFn)`

For codecs whose body size is computable from `v` without producing
the body. The size pass calls `SizeFn(v)`; the write pass calls
`EncodeFn(w, v)`. The hot path stays branch-free.

`SizeFn` and `EncodeFn` describe the **body** only. For
`WireLengthDelim` codecs, codegen emits the field key, then a varint
length prefix derived from `SizeFn(v)`, then calls `EncodeFn(w, v)`
to write the body — `EncodeFn` does not write the length prefix
itself. `WireVarint`, `WireFixed32`, and `WireFixed64` codecs are
self-framing; `EncodeFn` writes the entire post-key payload directly.

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

`DecimalBinary` is the second analytic builtin: it encodes a decimal as
`(coefficient, scale, sign)` varints, so the body width is a pure
function of `v` and the encode path materializes no string — 0 allocs
per decimal field. Prefer it over the materializing-cached
`DecimalString` / `DecimalAppend` for decimal-like types unless a
human-readable on-wire form is a hard requirement. See
[`docs/codecs/binary-decimal.md`](../../../docs/codecs/binary-decimal.md).

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

Three Writer helpers cover the common shapes:

| Helper                                                                       | Use when materialization returns                                            |
|------------------------------------------------------------------------------|-----------------------------------------------------------------------------|
| `w.WriteCachedString(callsite, func() string)`                               | a string (`v.String()`, `fmt.Sprint`)                                       |
| `w.WriteCachedAppendBytes(callsite, func(dst []byte) ([]byte, error))`       | bytes via append (`v.AppendText(dst)`, `(*big.Int).Append`)                 |
| `w.WriteCachedBytes(callsite, func() []byte)`                                | bytes (`json.Marshal`, `gzip.Compress`)                                     |

All three write the cached payload as a `LENGTH_DELIM` value (varint
length prefix followed by the bytes) — the same wire shape
`WriteString` / `WriteBytes` produce. The string and append paths each
allocate exactly once per first occurrence (the string value, or the
appended buffer); cached occurrences allocate nothing.

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

`DecimalAppend` is the append-style sibling: same wire shape, same
materialize-once guarantee, but reaches the text form via
`v.AppendText(dst)` so source types with an append API skip the
intermediate string:

```go
func EmitDecimalAppend[T interface {
    AppendText(dst []byte) ([]byte, error)
}](w *gsbm.Writer, v T, callsite uint64) error {
    return w.WriteCachedAppendBytes(callsite, func(dst []byte) ([]byte, error) {
        return v.AppendText(dst)
    })
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

### Streaming — `StreamFn` alone

For codecs whose body must be produced to know its size and whose
materialized body can be large enough that retaining it would
meaningfully grow peak heap. Codegen emits a single `StreamFn(w, v)`
call (no `callsite`, no scratch interaction) and the Writer's
mode-aware `WriteString` / `WriteBytes` produce the right output in
each pass: size-mode counts bytes into `sizeAcc` and discards the
materialized slice; write-mode appends it to the output buffer and
discards it. Between the two passes nothing is retained — the body is
materialized exactly twice per `gsbm.Marshal` call.

The wire shape is `LENGTH_DELIM`, the same envelope
`WriteCachedBytes` writes; `StreamFn` differs only in *when* the
materialization happens (per pass) and *what is kept* (nothing).

`StreamJSONBytes` is the canonical example:

```go
func StreamJSONBytes[T any](w *gsbm.Writer, v T) error {
    b, err := json.Marshal(v)
    if err != nil {
        return err
    }
    w.WriteBytes(b)
    return nil
}

func NewStreamingJSONDecl(name, goType, streamFn, decFn, pkgImport string) codecs.CodecDecl {
    return codecs.CodecDecl{
        Name:      name,
        GoType:    goType,
        WireType:  codecs.WireLengthDelim,
        StreamFn:  streamFn,
        DecodeFn:  decFn,
        PkgImport: pkgImport,
    }
}
```

The materialized form must be byte-identical across the size pass and
the write pass — anything else produces a length prefix that
disagrees with the body bytes (`json.Marshal` over an unsorted map is
the classic gotcha). The streaming-codec fixture's property test
asserts the probe streaming codec is deterministic; project codecs
inherit the same obligation.

### String vs append for text-form codecs

When the source value can produce its text form in either shape — both
`String() string` and `AppendText(dst []byte) ([]byte, error)` exist
and return identical bytes — prefer the append variant. The string
shape allocates a string plus whatever the materializer's internals
need; the append shape writes those bytes straight into the cached
buffer the Writer holds, so the intermediate string never exists. Wire
output is byte-identical for matching inputs, so the two are
interchangeable on the receiving end — the choice is purely about
encode-time allocations on the source side.

Pick `WriteCachedString` when only `String()` is available (the common
case for `fmt.Stringer` types from third-party packages). Pick
`WriteCachedAppendBytes` when the source type exposes `AppendText` or
an equivalent `Append(dst []byte) []byte` — including `*big.Int`,
`*big.Float`, `time.Time` (via `AppendFormat`), and decimal libraries
that ship an append API.

## Choosing between the three shapes

- Pick **analytic** whenever the size is a pure function of `v`. The
  emitted code is two direct calls per field with no Writer mode
  branch and no cache lookup.
- Pick **materializing-cached** when computing the size requires
  producing the body and the body is small or medium (~< 1 MiB rough
  heuristic). The mode-aware Writer adds a small per-method branch the
  analytic path avoids, and the scratch map costs one map lookup per
  cached call. Both are dwarfed by the materialization itself, which
  is the cost the cache exists to amortize.
- Pick **streaming** when computing the size requires producing the
  body and the body can be large enough that retaining it alongside
  the output buffer would meaningfully grow peak heap (~ 1 MiB and
  above, with the win growing linearly). Streaming runs the codec
  body twice — once in the size pass, once in the write pass — and
  caches nothing between them: 2× CPU for 1× peak memory.

The three shapes are mutually exclusive at registration time.
Declaring more than one of `(SizeFn, EncodeFn)`, `EmitFn`, and
`StreamFn` on a single `CodecDecl` is rejected with
`codec/conflicting-kinds`; declaring none keeps the existing
`codec/missing-size-fn` diagnostic.

## Callsite ids

Materializing codecs take an extra `callsite uint64` parameter.
Codegen emits a unique per-field `const` whose value is the FNV-1a
hash of `<pkg-path>.<struct>.<tag>` as a `uint64` and passes it
inline at every call site. Codec authors never construct callsite ids
themselves — they forward the parameter into `WriteCachedString`,
`WriteCachedAppendBytes`, or `WriteCachedBytes`.

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
