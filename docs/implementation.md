# odm-bin Implementation Notes

**Scope:** This document describes the Go runtime implementations of the odm-bin wire format. The wire format itself is defined in `odm-bin-wire-format.md` and is shared across implementations. This document covers two implementations:

- **Heap-mode** (current, v1) — standard Go allocation, full mutability, default decoded objects.
- **Arena-mode** (planned) — single-arena allocation, restricted mutability, opt-in for read-heavy paths.

The two implementations share the wire format. They share **nothing** at the runtime API level: different `Reader`/`Writer` types, different generated method signatures, different lifetime contracts. A user picks one implementation per call site and uses its API consistently.

---

## 1. Why two implementations

The heap-mode implementation is sufficient for the storage path (write to Spanner, occasional reads). It hits the encode-side allocation target (1 alloc/op encode for the BDD payload) and brings decode allocations down significantly via `Reset` + `sync.Pool`.

Arena-mode targets the cases where heap-mode still allocates measurably:
- Large read-only scans (audit, replay, batch processing of historical records).
- Hot read paths that decode an entire offer graph and discard it shortly after.
- Memory-pressure-sensitive workloads where releasing the entire decoded graph in one operation matters more than per-field GC tracking.

These are not the dominant Spanner read path today, but they exist and grow over time. Arena-mode is intended as an additive option, not a replacement.

The two implementations cannot share a single API because their lifetime and mutability semantics are incompatible. Heap-mode returns long-lived, freely mutable objects. Arena-mode returns objects whose validity ends with `arena.Release()` and whose mutation requires an explicit `Detach()` step. Encoding the difference into one set of methods would force every caller to deal with both contracts; keeping them separate keeps each contract clear.

## 2. Wire format equivalence

Both implementations produce and consume identical bytes. A heap-mode encoder writes a blob; an arena-mode decoder reads it back into an arena-allocated graph; the result is structurally equal to what a heap-mode decoder would produce. The reverse is also true. Tests cross-validate this by encoding with one mode and decoding with the other, then comparing.

There is no version distinction between heap-mode and arena-mode in the wire format. Same `fmtVer`, same `schVer`, same bytes.

## 3. Heap-mode implementation (current)

### 3.1 API surface

```go
package odm

// Writer accumulates encoded bytes into a caller-owned buffer.
type Writer struct { ... }

func NewWriter(buf []byte) *Writer
func (w *Writer) Bytes() []byte
func (w *Writer) Reset(buf []byte)
func (w *Writer) Err() error

// Reader decodes from a caller-owned byte slice.
type Reader struct { ... }

func NewReader(data []byte) *Reader

// Marshaler / Unmarshaler interfaces, generated on every type in the schema closure.
type Marshaler interface {
    MarshalODM(w *Writer) error
}
type Unmarshaler interface {
    UnmarshalODM(r *Reader) error
}

// Convenience top-level functions for root types.
func Marshal(v Marshaler, buf []byte) ([]byte, error)
func Unmarshal(data []byte, v Unmarshaler) error

// DecodeInto reuses an existing graph to avoid allocation.
func DecodeInto(data []byte, dst Resettable) error

type Resettable interface {
    Unmarshaler
    Reset() // recursively resets fields, preserves slice/map capacity
}
```

### 3.2 Generated code shape

For each type in the schema closure, codegen emits a companion file (`offer_odm.go` next to handwritten `offer.go`) containing:

- `MarshalODM(w *Writer) error`
- `UnmarshalODM(r *Reader) error`
- `Reset()`

All tags are inlined as integer literals. `UnmarshalODM` uses a switch on tag with a `SkipField(wireType)` default for unknown tags. No reflection. No runtime tag lookup.

### 3.3 Allocation behavior

**Encode:** target is 1 alloc/op for typical payloads, achieved by:
- Caller passes a pre-sized `[]byte` buffer (typically from `sync.Pool`).
- `Writer` uses `append` semantics; allocations occur only on buffer growth.
- All codegen `Marshal*` functions are append-only — no intermediate objects.

**Decode (cold):** allocations proportional to graph size. Each slice, map, and sub-struct in the decoded graph is its own heap allocation. This is the BDD-40-offers 5,241 allocs number from the prototype.

**Decode (warm, with `DecodeInto` + pool):** target is near-zero allocations after warmup. The decoder reuses the destination object's slice and map capacity. Fresh growth still allocates; steady-state with stable payload shapes converges to zero.

### 3.4 Reset semantics

`Reset` is recursive and capacity-preserving:

```go
func (o *Offer) Reset() {
    o.ID.Reset()
    o.Carrier = ""
    for i := range o.Segments {
        o.Segments[i].Reset()
    }
    o.Segments = o.Segments[:0]
    clear(o.Taxes) // Go 1.21+; preserves capacity
    o.RetailCode = 0
}
```

Order matters: inner `Reset` calls must happen before the outer slice is truncated (so inner capacity is preserved through the reset path, not orphaned).

### 3.5 Concurrency

`Writer` and `Reader` are not goroutine-safe. Each goroutine should own its own. `sync.Pool` of `Writer` and `Reader` per goroutine is the recommended pattern.

Decoded objects (e.g., `*Offer`) are plain Go values and follow normal Go concurrency rules — safe to read concurrently if no goroutine mutates, otherwise standard synchronization applies.

### 3.6 Limitations

- Decode-side allocations cannot reach zero on cold paths; the graph must be allocated.
- Pool warmup requires representative payload shapes; uneven shapes cause continued allocation as buffers grow to fit larger inputs.
- No protection against use-after-Reset bugs at compile time. If a caller holds a reference to an inner field after the parent is `Reset`, behavior is undefined per Go semantics (the slice/map is reused). This is acceptable for normal callers but is a class of bug to be aware of.

## 4. Arena-mode implementation (planned)

### 4.1 Goal

Reduce decode-side allocations to a small constant (the arena itself plus growth) regardless of graph size, at the cost of restricted lifetime and mutability semantics.

### 4.2 API surface

```go
package odmarena

// Arena is a bump allocator for a decoded graph.
type Arena struct { ... }

func NewArena() *Arena
func (a *Arena) Release() // invalidates all references derived from this arena

// Reader decodes into a specified arena.
type Reader struct { ... }

func NewReader(data []byte, a *Arena) *Reader

// Generated decode functions take an arena and return a pointer into it.
// Note the signature difference from heap-mode: the caller does not provide
// a destination object; the arena owns the storage.
func DecodeOffer(data []byte, a *Arena) (*Offer, error)

// Detach copies an arena-allocated graph to heap, returning a heap-mode
// equivalent. After Detach, the heap copy is independent of the arena.
func DetachOffer(o *Offer, a *Arena) *odm.Offer
```

The Writer in arena mode is identical to heap-mode (encoding does not benefit from arena). Only the Reader and decoded objects differ.

### 4.3 Decoded object semantics

Objects returned by arena-mode decoders are **read-only by contract**. Mutating them produces undefined behavior. Specifically:

- Field reads are safe.
- Receiver methods that only read are safe.
- Receiver methods that mutate (e.g., `Recalculate()` that updates internal state) are NOT safe to call directly. The caller must `Detach` first.

This is enforced by convention and documentation, not by the type system. Go does not provide a way to express read-only at the type level for a `*T`. Linting can catch some cases.

Strings inside arena-allocated objects reference bytes inside the arena's buffer via `unsafe.String`. They are valid only for the arena's lifetime.

### 4.4 Lifetime model

```go
arena := odmarena.NewArena()
defer arena.Release()

offer, err := odmarena.DecodeOffer(blob, arena)
if err != nil {
    return err
}

// Read offer freely.
processOffer(offer)

// At end of scope, Release frees all memory at once.
```

The arena owns all storage for the decoded graph: slices, maps (see §4.6), strings, sub-structs. `Release` invalidates every pointer derived from the arena. After `Release`, accessing any field is a use-after-free.

The Go runtime cannot enforce this. The discipline is on the caller. Misuse produces silent corruption or crashes.

### 4.5 Generated code shape

Codegen for arena-mode produces a separate set of files (e.g., `offer_odm_arena.go`) with different signatures:

```go
// Heap-mode (existing)
func (o *Offer) UnmarshalODM(r *odm.Reader) error

// Arena-mode (new)
func unmarshalOfferArena(r *odmarena.Reader) (*Offer, error)
```

Arena-mode decoders allocate via the arena, never via `make`. Slices and sub-structs come from `arena.AllocSlice[T](n)` and `arena.AllocStruct[T]()`. Strings use `arena.AcquireString(bytes)` which returns a `string` referencing the arena bytes via `unsafe.String`.

### 4.6 Maps in arena

Go's built-in `map` is runtime-managed and cannot be allocated in a custom arena. Two paths:

**Path A: heap-allocate maps even in arena mode.** Maps remain on the heap, the rest of the graph in the arena. Partial benefit, simpler implementation, no custom map type.

**Path B: vendor an arena-aware hash map.** A swiss-table-style implementation that allocates buckets in the arena. Larger code surface, full benefit.

The decision is deferred. Path A is the v1 of arena-mode; Path B is a possible later optimization. Until then, map fields in arena-decoded objects cost their normal heap allocations — arena-mode improves everything except maps.

### 4.7 Detach

`Detach` walks the arena-allocated graph and produces a heap-allocated copy compatible with the heap-mode `Offer` type. After Detach, the heap copy has full mutation rights and outlives the arena.

```go
arena := odmarena.NewArena()
offerArena, err := odmarena.DecodeOffer(blob, arena)
// ... read-only operations ...

if needToMutate {
    offerHeap := odmarena.DetachOffer(offerArena, arena)
    arena.Release()
    return mutateAndUse(offerHeap) // heap-mode Offer, fully owned
}

arena.Release()
```

Detach costs roughly the same as a heap-mode decode would have. The arena path wins when most objects are not detached — i.e., when the graph is read and discarded.

### 4.8 Limitations

- Mutation requires Detach; no in-place modification of arena objects.
- Maps remain heap-allocated (Path A) or require vendored map type (Path B).
- Use-after-release is a real failure mode, not catchable at compile time. Linting and code review are the defenses.
- `unsafe.String` use means changes to Go's string representation in future Go versions could in principle break the implementation. This has been stable for many releases but is a non-zero risk.
- Receiver methods on the domain model that mutate state cannot be called on arena objects. Some methods may need to be re-examined to confirm read-only-ness; some may need refactoring (e.g., split into `pure` and `mutating` variants).

### 4.9 When to use which

| Use case                                    | Mode        |
|---------------------------------------------|-------------|
| Storage write path                          | Heap (encode is the same; decoded graph not used here) |
| Standard read in business logic that mutates | Heap        |
| Audit / replay scans of historical records  | Arena       |
| Batch processing of decoded offers          | Arena, with Detach as needed |
| Hot read with measured allocation pressure  | Profile both, pick winner |

Arena-mode is opt-in. Heap-mode is the default and remains so indefinitely.

## 5. Code organization

```
storage/odm/                  # heap-mode runtime + generated heap-mode code
  writer.go
  reader.go
  marshaler.go                 # Marshaler/Unmarshaler interfaces
  offer.go                     # handwritten domain type + business methods
  offer_odm.go                 # generated heap-mode (Marshal/Unmarshal/Reset)
  segment.go
  segment_odm.go
  ...
  schema.yaml                  # generated schema artifact
  schema_snapshot.json         # generated machine-readable snapshot

storage/odmarena/              # arena-mode runtime + generated arena-mode code
  arena.go
  reader.go
  detach.go
  offer_odm_arena.go           # generated arena-mode decode
  segment_odm_arena.go
  ...
```

Arena-mode generated files reference the same domain types from `storage/odm/` (the `Offer` struct itself is shared — only the decode functions differ). The codegen tool runs twice: once for heap-mode targeting the `odm` package, once for arena-mode targeting `odmarena`. The same schema artifact drives both runs.

## 6. Codegen: two passes, same schema

The codegen tool accepts a `--mode={heap,arena}` flag (or runs both by default). Each mode produces its own set of generated files. The schema closure analysis is shared — both passes see the same types, tags, and validation rules.

Adding arena-mode code does not change heap-mode output. A team using only heap-mode never sees arena-mode files.

## 7. Testing strategy

Property tests verify round-trip equivalence:
- Heap encode → heap decode → DeepEqual to original.
- Heap encode → arena decode → values match (with arena-aware comparison).
- Arena encode (same as heap) → arena decode → values match.
- Heap encode → arena decode → Detach → DeepEqual to a heap decode of the same blob.

Fuzz tests target the decoder with malformed inputs (truncated, reordered, unknown tags, reserved wire types). Both heap and arena decoders fuzz identically against the same corpus — the wire format is shared.

Allocation tests use `testing.AllocsPerRun`:
- Heap encode of representative payload: 1 alloc/op (warm, with pooled buffer).
- Heap decode with `DecodeInto` + pool: target single-digit allocs/op (warm).
- Arena decode: target small constant (arena + growth) regardless of graph size.

## 8. Outstanding decisions

- **Arena map strategy** (Path A vs Path B in §4.6). Decide before arena implementation begins; defaults to Path A.
- **Allocator interface in heap-mode v1.** Even though arena-mode has its own runtime, exposing a minimal allocator interface in heap-mode `Reader` (e.g., for tests and benchmarks) may be useful. Decide during Milestone 4 of the master plan.
- **Linting for arena misuse.** Static analysis catching use-after-Release patterns is desirable. Likely a custom analyzer leveraging `go/analysis` infrastructure. Out of scope for the initial arena implementation; track as a follow-up.
- **Detach implementation strategy.** Two viable approaches: (a) walk the graph manually with generated copy code; (b) reuse the heap-mode unmarshal path by feeding it the arena's pre-decoded structure. Decide during arena implementation.
