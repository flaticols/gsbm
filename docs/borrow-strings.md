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

**The caller MUST keep the input `[]byte` alive and immutable for at least as long as any decoded value may be observed.**

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

## Arena/custom allocators

Borrow-marked generated code only aliases the source blob when the reader has no allocator installed. If `r.Allocator() != nil`, string materialization still routes through `r.AcquireString`, preserving arena/custom allocator lifetime semantics.

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
