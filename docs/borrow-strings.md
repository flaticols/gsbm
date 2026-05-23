# Borrowed-string heap decode

`//gsbm:borrow-strings` is an unsafe generated-code opt-in for heap-mode decoders that are dominated by string allocation.

By default, heap decode copies every string out of the caller-owned blob. This is safe: callers can reuse or drop the input buffer immediately after decode. With `//gsbm:borrow-strings`, generated `UnmarshalGSBM` reads string payload bytes and builds Go strings that point directly into the input blob.

```go
//gsbm:root
//gsbm:borrow-strings
type Order struct {
    ID string `bin:"1"`
}
```

## Contract

**The caller MUST keep the Reader's current decode buffer — the slice returned by `Reader.BorrowSource()` — immutable and unrecycled for at least as long as any decoded value may be observed.** Reachability is automatic: each borrowed string carries a Go pointer into the buffer, so the GC will not reclaim the allocation while any borrowed value is live. The contract is therefore *not* about pinning against GC; it is about preventing in-place mutation and premature pool/scratch reuse.

For uncompressed payloads decoded from a caller-owned slice (`NewReader(src)` with or without a header; `DecodeInto`; `DecodeBodyInto`) this is the same slice the caller passed in. For compressed payloads, or for the streaming `NewReaderFrom` / `NewReaderFromN` constructors, the reader-owned buffer differs from any input the caller still holds — see "Compressed payloads + borrow-strings" below. `BorrowSource()` is the canonical accessor in either case.

This includes strings stored in:

- direct string fields;
- named string aliases;
- `*string` values;
- string slices;
- map keys and map values.

Mutating or reusing the input buffer while borrowed values are live can corrupt decoded strings. If a borrowed string is used as a Go map key, mutating the underlying bytes can also break the map's invariants.

## Good fit / bad fit

Good fits:

- The blob is immutable backing storage for the decoded value.
- The caller stores the blob alongside the decoded value, for example:

  ```go
  type CachedOrder struct {
      Blob  []byte // immutable backing bytes retained for Order
      Order *Order // strings borrow from Blob
  }
  ```

- Decode is request-scoped and the decoded value cannot escape past the input buffer's lifetime.
- The allocation profile is dominated by string copies and the caller can enforce the lifetime contract in one small ownership boundary.

Bad or suspicious fits:

- The input buffer comes from a scratch `[]byte`, `bytes.Buffer`, `gsbm.Writer`, or pool that will be reused after decode.
- The input buffer may be mutated after decode.
- A decoded value is returned, cached, or sent to another goroutine without retaining the source blob.
- A pooled receiver is returned to a pool while any consumer may still observe its borrowed strings.
- A small borrowed string may be retained for a long time; it can keep the entire source blob live.

## Scope

The marker is struct-local. It affects string decode sites emitted inside that struct's own generated `UnmarshalGSBM` body. Nested named structs decode through their own generated methods and must carry their own `//gsbm:borrow-strings` marker if they should borrow too.

The marker does not change wire bytes, `schemaHint`, or cross-language wire compatibility. Schema snapshots record it for review because it changes Go-side lifetime behavior.

## DecodeInto and pooling

`DecodeInto` remains usable, but receiver pooling becomes tied to blob lifetime. Do not return a receiver to a pool while any consumer may still observe borrowed strings from it, and do not reuse the decode buffer until all borrowed values from that receiver are dead.

Generated `Reset` clears borrowed string slices before truncating them so a pooled receiver does not keep old blob references in slice backing arrays.

## Linter follow-up

Suspicious lifetime patterns are good candidates for static analysis, but they are not all locally decidable. A follow-up issue tracks a best-effort Go analyzer / linter that can warn on obvious misuse, such as decoding from a buffer that is later reused, returning a borrow-marked value without retaining the blob, or receiver-pool patterns that put values back too early: <https://github.com/flaticols/gsbm/issues/54>.

Until that analyzer exists, code review must treat every `//gsbm:borrow-strings` usage like any other unsafe lifetime boundary: identify who owns the blob, prove it remains immutable, and prove it outlives every decoded value.

## Compressed payloads + borrow-strings

When the writer enables zstd body compression (see
[`docs/codecs/compression.md`](codecs/compression.md)), the reader allocates
a fresh decompressed buffer during `ReadHeader` and the borrowed strings
alias that buffer — *not* the compressed bytes the caller passed to
`NewReader`. Pinning only the compressed input is therefore not enough; the
decompressed buffer is a distinct allocation and is the one that must
outlive every borrowed value.

`Reader.BorrowSource()` returns the slice borrow-strings decoders may
alias. It is the original `src` for uncompressed blobs (or the buffer
`NewReaderFrom` / `NewReaderFromN` read into) and the decompressed body
for compressed blobs. The method is always safe to call; before
`ReadHeader` it returns the user-provided source, after `ReadHeader` on a
compressed blob it returns the decompressed body. It does not allocate.

The standard pattern is a thin wrapper that decodes and returns both the
value and the source slice as a pair, so the caller has a single
ownership handle to audit against mutation and pool reuse:

```go
func DecodeWithBody(serialized []byte) (RecordBatch, []byte, error) {
    r := gsbm.NewReader(serialized)

    // Heap mode: no arena, so borrow-strings can alias the reader body.
    if _, _, _, err := r.ReadHeader(); err != nil {
        return RecordBatch{}, nil, err
    }

    var out RecordBatch
    if err := out.UnmarshalGSBM(r); err != nil {
        return RecordBatch{}, nil, err
    }
    if r.HasMore() {
        return RecordBatch{}, nil, fmt.Errorf("unexpected trailing bytes")
    }
    if err := r.Err(); err != nil {
        return RecordBatch{}, nil, err
    }

    return out, r.BorrowSource(), nil
}
```

Callers then route both values together so that whoever holds the
decoded `records` also clearly owns `body` and is responsible for not
mutating it or returning it to a pool:

```go
records, body, err := DecodeWithBody(blob)
if err != nil {
    return err
}
_ = body // hold for the audit; do not mutate, do not return to a pool

// Use records here. Borrowed strings remain valid for as long as
// `records` is reachable — the GC follows the string data pointers.
```

`runtime.KeepAlive(body)` is *not* required for GC reasons: each borrowed
string carries a pointer into the buffer, so the underlying allocation
stays reachable for as long as any borrowed value does. The reason to
return `body` is auditability of the mutation/reuse contract, not
lifetime pinning.

If looser pairing is a hazard in your codebase (decoded value and body
travel through different code paths, get stored in different structs, or
cross goroutine boundaries), a small wrapper type ties them together at
the type level:

```go
type Borrowed[T any] struct {
    Value T
    Body  []byte // immutable, not reused, for the lifetime of Value
}
```

`Borrowed[RecordBatch]` flows through code as a single value; misuse
becomes a type error rather than a code-review audit. The trade-off is a
new public type and one more allocation per decode — adopt it when the
loose-pairing form proves error-prone, not preemptively.

When an `Allocator` is installed (`r.Allocator() != nil`) generated
borrow decoders route through `AcquireString`, and `BorrowSource()`
still returns the buffer for API consistency. Whether pinning is
required then depends on the allocator: the bundled
`storage/gsbmarena.Arena` copies decoded bytes into arena-owned chunks,
so its strings live independently of `r.buf` and pinning is
unnecessary. A custom `Allocator` whose `AcquireString` aliases `b`
directly (e.g. returning `unsafe.String(&b[0], len(b))`) keeps the same
lifetime requirement as the heap-borrow path — the contract above still
applies.

See also: [`docs/codecs/compression.md`](codecs/compression.md) for the
compression flag, the decompressed-buffer allocation, and the writer-side
trade-offs.

## Arena/custom allocators

Borrow-marked generated code only aliases the source blob directly when the reader has no allocator installed. If `r.Allocator() != nil`, string materialization routes through `r.AcquireString` and the allocator chooses the lifetime: the bundled `gsbmarena.Arena` copies into arena-owned chunks (strings outlive `r.buf`), whereas a custom allocator that aliases `b` directly still requires pinning `BorrowSource()` for the lifetime of the decoded values.

## Performance target

The optimization removes per-string `string([]byte)` allocations. It does not promise that every decode is literally zero-allocation: receiver creation, first-use slices/maps, pointer fields, and heap-mode `[]byte` copies can still allocate.

The generated `borrowstrings` fixture demonstrates the target shape on a ~1.97 MB string-heavy payload:

| Path | allocs/op |
|---|---:|
| Default heap string copies | ~56,000 |
| `//gsbm:borrow-strings` heap decode | ~2 |

Run:

```bash
go test -bench='BenchmarkLargeStringRecordDecodeHeap' -benchmem ./tools/gsbmcodegen/fixtures/borrowstrings/
```
