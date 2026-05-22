## Custom codec lifecycle

End-to-end walkthrough from "I have a custom type" to a generated,
tested codec wired through codegen and exercised by `gsbm.Marshal`.
The example here is deliberately small — a `UUID [16]byte` wrapper
that maps to a fixed-width body — so each step stands on its own.
For a richer Money/Invoice case, see
[`example-money.md`](example-money.md). For the per-shape semantics
(analytic vs materializing-cached vs streaming) and the wire-level
rules, see [`tools/gsbmcodegen/codecs/README.md`](../../tools/gsbmcodegen/codecs/README.md)
and [`docs/spec.md`](../spec.md) §5.8.

The seven steps below are sequential; every step pins a deliverable
that the next step depends on.

## 1. Define your type

Start from the Go type the codec will encode. A custom codec opts
the field out of struct-traversal, so the type can be anything — a
named slice, a struct, an opaque ID — and the wire shape is whatever
your codec writes. Keep the type's invariants (zero value, equality,
text form) explicit; the codec will rely on them.

```go
package uuidcodec

// UUID is a 128-bit identifier. The codec below treats the zero
// value as a valid, distinguishable UUID — there is no in-band
// "nil UUID" marker on the wire.
type UUID [16]byte
```

For the worked example we want a fixed-width body (16 raw bytes).
Size is a pure function of `v` — `SizeUUID` will always return 16 —
which makes this an **analytic** codec. If the size depended on
producing the body (text form, JSON, compression) we would pick
materializing-cached or streaming instead; the README's "Choosing
between the three shapes" table is the decision rule.

## 2. Write encode, decode, size

The analytic shape is two free functions plus an inverse. The
encode function writes the **body** only — codegen wraps it in a
key and (for `WireLengthDelim`) a length prefix.

```go
import (
    "encoding/binary"

    "go.flaticols.dev/gsbm/storage/gsbm"
)

// EncodeUUID writes the 16-byte body of a UUID codec field as two
// fixed64s (high half then low half). For an analytic
// WireLengthDelim codec, EncodeFn writes the body only — codegen
// emits the field key and length prefix around this call. The
// Writer is mode-aware so the same code runs in the size pass
// (counts bytes) and the write pass (appends them).
func EncodeUUID(w *gsbm.Writer, u UUID) error {
    w.WriteFixed64(binary.BigEndian.Uint64(u[:8]))
    w.WriteFixed64(binary.BigEndian.Uint64(u[8:]))
    return nil
}

// SizeUUID reports the byte count EncodeUUID writes. Always 16
// for this codec; the signature matches EncodeUUID's value shape
// so codegen can swap `EncodeFn(w, v)` for `SizeFn(v)` at the
// call site.
func SizeUUID(u UUID) int { return 16 }

// DecodeUUID reads the body EncodeUUID wrote. Codegen has already
// consumed the field key and the length prefix; this function
// reads only the 16 body bytes.
func DecodeUUID(r *gsbm.Reader, u *UUID) error {
    hi, err := r.ReadFixed64()
    if err != nil {
        return err
    }
    lo, err := r.ReadFixed64()
    if err != nil {
        return err
    }
    binary.BigEndian.PutUint64(u[:8], hi)
    binary.BigEndian.PutUint64(u[8:], lo)
    return nil
}
```

`TimeDecl` in
[`builtins.go`](../../tools/gsbmcodegen/codecs/builtins/builtins.go)
is the canonical analytic decl; the framing rules for each
`WireType` are spelled out in the codec README's analytic section.

## 3. Register the codec

Codecs are registered against a `*codecs.Registry` at codegen time;
there is no global registry and no `init` side effects. The standard
entry point is `builtins.NewBuiltinRegistry` — it returns a fresh
Registry pre-loaded with the `Time` codec. Add your decl to it
before handing it to the emitter.

```go
import (
    "go.flaticols.dev/gsbm/tools/gsbmcodegen/codecs"
    "go.flaticols.dev/gsbm/tools/gsbmcodegen/codecs/builtins"
)

var UUIDDecl = codecs.CodecDecl{
    Name:      "UUID",
    GoType:    "example.com/uuidcodec.UUID",
    WireType:  codecs.WireLengthDelim,
    EncodeFn:  "EncodeUUID",
    DecodeFn:  "DecodeUUID",
    SizeFn:    "SizeUUID",
    PkgImport: "example.com/uuidcodec",
}

func registry() (*codecs.Registry, error) {
    r := builtins.NewBuiltinRegistry()
    if err := r.Register(UUIDDecl); err != nil {
        return nil, err
    }
    return r, nil
}
```

`Register` rejects conflicting shapes (`codec/conflicting-kinds`)
and missing functions (`codec/missing-size-fn`). Two registrations
of the same `Name` with non-identical decls fail with
`codecs.ErrDuplicateCodec`; an identical re-registration is a
no-op so calling `registry()` from multiple test files is safe.

## 4. Tag the field

Tag every struct field that should use the codec with
`bin:"N,custom=Name"`, where `N` is the wire tag and `Name` is the
codec's registered name.

```go
package myapp

//gsbm:root
type Event struct {
    ID        UUID   `bin:"1,custom=UUID"`
    Payload   []byte `bin:"2"`
}
```

The schema records only the codec name. A field that names an
unregistered codec fails codegen with `codec/unregistered` — the
diagnostic lists every registered name, so typos are obvious.

## 5. Run codegen

Hand the registry to `gsbmcodegen.GenerateWithCodecs`. The
fixture's regen harness in
[`regen_golden_test.go`](../../tools/gsbmcodegen/fixtures/customcodec/regen_golden_test.go)
is the reference; the shape is:

```go
ps, _ := gsbmschema.LoadFromDirs([]string{"./myapp"})
res := gsbmschema.Analyze(ps)
files, _ := gsbmcodegen.GenerateWithCodecs(ps, res.Schema, reg)
for _, gf := range files {
    _ = os.WriteFile(gf.Path, gf.Contents, 0o644)
}
```

In a project, this typically lives in a `//go:generate` directive
or a `Makefile` target. Generated `*_gsbm.go` files are committed
alongside the schema source — the golden tests in
`regen_golden_test.go` (`TestGoldenCustomCodec`) pin the generator's
output so unexpected drift is caught in CI.

Generated `MarshalGSBM` for `Event.ID` calls `EncodeUUID(w, v.ID)`
directly; `UnmarshalGSBM` calls `DecodeUUID(r, &v.ID)`. No
reflection, no dispatch — the codec's wire shape replaces the
default struct-traversal emit for that field only.

## 6. Round-trip test

The minimum bar is a round-trip: encode a representative value,
decode it, assert equality. The fixture's
[`customcodec_test.go`](../../tools/gsbmcodegen/fixtures/customcodec/customcodec_test.go)
is the reference (`TestRecordRoundTrip` and friends). The shape
for our example:

```go
func TestEventRoundTrip(t *testing.T) {
    in := Event{
        ID:      UUID{0x01, 0x02, 0x03, 0x04, /* ... */ 0x10},
        Payload: []byte("hello"),
    }
    buf, err := gsbm.Marshal(&in, 0)
    if err != nil {
        t.Fatalf("Marshal: %v", err)
    }
    var out Event
    if err := out.UnmarshalGSBM(gsbm.NewReader(buf)); err != nil {
        t.Fatalf("UnmarshalGSBM: %v", err)
    }
    if out.ID != in.ID {
        t.Errorf("ID: got %x, want %x", out.ID, in.ID)
    }
}
```

Round-trip alone is necessary but not sufficient. Cover at least:

- **Zero value.** Decoding into a freshly-zeroed receiver must
  yield the same value the encoder wrote — `TestRecordRoundTripZeroTime`
  is the regression that locked this in for `Time`.
- **Boundary values.** Anything where the codec's bytes change
  shape (sign bit, maximum length, empty body).
- **Wire-bytes pin.** Construct the expected byte sequence
  by hand against `gsbm.Writer` and assert byte equality —
  `TestRecordWireBytes` shows the pattern. This catches changes
  that round-trip cleanly but break wire compatibility for
  existing readers.

See [`testing.md`](testing.md) for the full pattern catalog —
materialize-once probes for cached codecs, materialize-twice probes
for streaming codecs, and cross-kind wire-equivalence checks.

## 7. Benchmark

Once correctness is locked in, measure. For an analytic codec the
hot path is two direct calls per field with no Writer mode branch
and no cache lookup, so the benchmark is straightforward:

```go
func BenchmarkEventMarshal(b *testing.B) {
    in := Event{
        ID:      UUID{ /* ... */ },
        Payload: bytes.Repeat([]byte("x"), 64),
    }
    b.ReportAllocs()
    for i := 0; i < b.N; i++ {
        buf, err := gsbm.Marshal(&in, 0)
        if err != nil {
            b.Fatal(err)
        }
        _ = buf
    }
}
```

Materializing-cached and streaming codecs need different setups —
see [`performance.md`](performance.md) for the per-shape recipe and
for how to interpret `ReportAllocs` output across the three kinds.

## What you have at the end

- A typed Go value (`UUID`) with explicit encode/decode/size free
  functions.
- A `CodecDecl` registered under a stable name (`"UUID"`).
- Generated `MarshalGSBM` / `UnmarshalGSBM` for every struct field
  tagged `bin:"N,custom=UUID"`, with the codec call inlined at the
  emit site.
- Round-trip and wire-byte tests that pin behaviour against the
  generated code.
- A benchmark that the next change can be measured against.

The three artefacts that travel together are the codec source
(encode/decode/size), the `CodecDecl` registration, and the
committed `*_gsbm.go` golden. Any change to one demands a regen
and a fresh test pass against the other two.
