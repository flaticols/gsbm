# gsbm Implementation Notes

**Scope:** This document describes the Go runtime implementations of the gsbm wire format. The wire format itself is defined in `gsbm-wire-format.md` and is shared across implementations. This document covers two implementations:

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

There is no version distinction between heap-mode and arena-mode in the wire format. Same `fmtVer`, same `schemaHint`, same bytes.

## 3. Heap-mode implementation (current)

### 3.1 API surface

```go
package gsbm

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
    MarshalGSBM(w *Writer) error
}
type Unmarshaler interface {
    UnmarshalGSBM(r *Reader) error
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

For each type in the schema closure, codegen emits a companion file (`offer_gsbm.go` next to handwritten `offer.go`) containing:

- `MarshalGSBM(w *Writer) error`
- `UnmarshalGSBM(r *Reader) error`
- `Reset()`

All tags are inlined as integer literals. `UnmarshalGSBM` uses a switch on tag with a `SkipField(wireType)` default for unknown tags. No reflection. No runtime tag lookup.

### 3.3 Allocation behavior

**Encode:** target is 1 alloc/op for typical payloads, achieved by:
- Caller passes a pre-sized `[]byte` buffer (typically from `sync.Pool`).
- `Writer` uses `append` semantics; allocations occur only on buffer growth.
- All codegen `Marshal*` functions are append-only — no intermediate objects.

**Decode (cold):** allocations proportional to graph size. Each slice, map, and sub-struct in the decoded graph is its own heap allocation. This is the BDD-40-offers 5,241 allocs number from the prototype.

**Decode (warm, with `DecodeInto` + pool):** target is near-zero allocations after warmup. The decoder reuses the destination object's slice and map capacity. Fresh growth still allocates; steady-state with stable payload shapes converges to zero.

**Allocator policy.** Allocation behavior for `string` and `[]byte` is set by the `Allocator` installed on the `Reader` (see `storage/gsbm/allocator.go`). Two modes ship today:

- **heap-copy (default, `SetAllocator(nil)` or no call):** safe. Every `ReadString` / `ReadBytes` copies the payload into a fresh allocation. Decoded objects do not alias the input buffer; the caller may free or mutate the input as soon as decode returns. This is the right mode for any path that decodes from a persisted blob, since a Spanner read or `sync.Pool` reuse can re-use the input buffer at any time.
- **arena (`SetAllocator(arena)` with `*gsbmarena.Arena`):** zero-copy strings via `unsafe.String` over arena-owned bytes. Decoded objects reference arena memory and become invalid the moment `arena.Release()` runs. Suited to short-lived read-heavy paths (e.g., a request that decodes, projects, and discards within a single handler) where the caller can guarantee the lifetime invariant.

A borrow-from-input mode (zero-copy strings backed by the input slice itself) is **not** shipped; nothing in the runtime aliases the input bytes when no allocator is installed. The arena mode is the only zero-copy option.

These modes share the same wire decoder; they differ only at the string/`[]byte` allocation seam. Generated `MarshalGSBM` / `UnmarshalGSBM` code never branches on the policy.

**Benchmark suite and budget-as-test pattern.** Allocation behavior is measured by a suite of `Benchmark*` functions exercising 1–2 MiB payloads from the deterministic generator at `internal/bench/payload.go` (`MakeLargeOrder`, `MakeLargeCatalog`). The benchmarks cover encode (heap pooled, heap fresh), decode (heap cold, heap warm with `DecodeInto` + pool, arena single-shot, arena pool reuse), and round-trip; they live in `storage/gsbm/bench_encode_test.go`, `storage/gsbm/bench_decode_test.go`, `storage/gsbmarena/bench_test.go`, and `tools/gsbmcodegen/fixtures/graph/bench_test.go`. Every `Benchmark*` is partnered with a `Test*Budget` in the same file that wraps the same workload in `testing.AllocsPerRun(N, fn)` and asserts a numeric ceiling — so allocation regressions surface on every `go test ./...` invocation, not only on `go test -bench=.`. The ceilings are empirical (recorded in the plan that introduced the suite) and act as regression guards: loosening one without a documented spec or perf-doc citation is forbidden. Cold-decode benchmarks call `gsbm.ForgetPresence` after each iteration so the presence-tracking sidecar (§3.7) does not skew the numbers. Run the full suite with `make bench` (`BENCHTIME=3x` by default) and the fuzz harnesses (§7) with `make fuzz` (`FUZZTIME=30s` by default).

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

### 3.7 Field presence (`FieldPresent`)

Spec §3 / §7.2 defines a missing tag as decoding to the type's zero value. That is correct on the wire but loses information at the Go API layer: a field that was not on the wire is indistinguishable from a field whose value happened to be the zero value of its type. Migration logic that needs to fall back from a new tag to an old tag only when the new tag was absent has no way to tell the two cases apart from the decoded struct alone.

Each generated struct therefore exposes:

```go
func (v *T) FieldPresent(tag uint32) bool
```

`FieldPresent` returns true when `UnmarshalGSBM` consumed `tag` into `v` since the last `ClearPresence`/`Reset`/decode start, and false otherwise — including for unknown tags and for tags above `MaxTrackedTag`. For a receiver that has never been decoded into, the answer is normally false; the one exception is a fresh `*T` whose backing memory was previously occupied by another `T` that had been decoded into and then garbage-collected without a `ForgetPresence` call. The recycled (type, address) pair inherits the prior occupant's mask until the next `UnmarshalGSBM`/`Reset`/`ClearPresence` clears it. Callers who need the strong contract on never-decoded receivers should call `gsbm.ForgetPresence(v)` before dropping the previous occupant, or call `Reset()`/`UnmarshalGSBM`/`ClearPresence` on the new receiver before observing presence.

The wire format is unchanged. This is a Go-API-only feature; encoders and decoders from other implementations interoperate identically.

#### Sidecar layout

Presence bits are not stored on the user struct. They live in a package-level sidecar in `storage/gsbm/presence_track.go`:

```go
var presenceStore sync.Map // map[receiverKey]*presenceMask, where receiverKey = (typeID uintptr, addr uintptr)
```

`presenceMask` is a fixed `[16]atomic.Uint64` covering tags `1..MaxTrackedTag`. The key is composed of the receiver's concrete-type identity (the type word from the `any` interface header) and its data address, both stored as `uintptr` rather than `unsafe.Pointer`. Two consequences:

- The `uintptr` storage means the sidecar entry does **not** keep the receiver alive: when the user drops their last reference, the receiver is GC'd and its sidecar entry becomes a stale (unreachable-by-any-live-receiver) entry whose mask costs ~144 bytes until ForgetPresence evicts it or the address is reused. Storing `unsafe.Pointer` keys would have pinned every ever-decoded receiver because Go's GC traces through `unsafe.Pointer` values stored inside interface boxes.
- Including the type identity disambiguates a parent struct from a generated nested struct that lives at offset 0 (where `&parent == &parent.Field` as raw pointers). A pure pointer key would let the nested decoder's `ClearPresence` wipe the parent's bits.

The choice keeps the user's Go struct unchanged — handwritten field offsets, struct sizes, and embeddings are unaffected — at the cost of one `sync.Map` lookup per `FieldPresent` call and at most one mask allocation per (type, address) pair ever decoded into. Repeated decodes into the same receiver reuse the existing mask; `ClearPresence` zeroes the mask in place rather than evicting it.

#### `MaxTrackedTag`

`gsbm.MaxTrackedTag` is currently `1024`. Tags greater than the cap silently no-op on `MarkPresent` and return `false` from `IsPresent`. The codegen emits a generation-time warning when a struct declares a tag above the cap so the schema author sees that those fields will not participate in `FieldPresent`. Raising the cap is a runtime-only change (widen `presenceMask`); it requires no wire-format work.

#### Reset and pool reuse

`Reset()` calls `gsbm.ClearPresence(v)`, so a `Reset` followed by `FieldPresent(tag)` reports `false` for every tag. Generated `UnmarshalGSBM` also calls `ClearPresence` at the top of every decode, so a receiver pulled from a `sync.Pool` and decoded with a new blob does not leak presence bits from the previous decode.

Sidecar entries persist until explicitly evicted: there is no automatic reclamation. For pooled receivers the sidecar size is bounded by the pool size and the entry is reused across decodes. For receivers that are GC'd, the entry becomes stale and (a) wastes ~144 bytes per distinct (type, address) pair, (b) is overwritten safely if a new allocation lands at the same address with the same type. Programs that allocate many ad-hoc receivers and discard them should call `gsbm.ForgetPresence(v)` before dropping the last reference to bound the sidecar's footprint.

#### When to use it

Use `FieldPresent` for migration fallback logic where "wrote zero" and "never wrote" carry different meanings — the canonical case is a phased tag replacement during a `compat_write` window (see §10), where new code reads the successor tag and falls back to the predecessor only when the successor was absent. For ordinary read paths there is no reason to call `FieldPresent`; the zero value of an absent field is the correct semantic in §3 / §7.2.

#### Reserved annotation

The marker `//gsbm:presence` is reserved for a future opt-in toggle. The schema parser accepts it as a no-op today so handwritten code may begin tagging fields ahead of any default flip; the codegen ignores it. Until that follow-up lands, presence bits are maintained unconditionally for every field whose tag is within `MaxTrackedTag`.

### 3.8 Test fixtures

Codegen and schema-tooling tests live under `tools/gsbmcodegen/fixtures/`. Each fixture package is scoped to a distinct slice of behavior and stays self-contained (own `types.go`, own `*_test.go`, own committed goldens where applicable). Adding fixtures here rather than extending an existing one keeps each shape pinned to one interaction.

| Fixture | Scope |
|---|---|
| `sample/` | Original end-to-end shape: round-trip, presence bitmap, arena cross-mode, `sync.Pool` reuse, tag-1/15/16 boundaries, varint overflow, missing-tag zero-fill, unknown LENGTH_DELIM tag skip, header round-trip. |
| `graph/` | Composite-encoding interactions: `[]Section` (slice of named struct), `map[string]Tag` (map with named-struct value), deep named composition (`Section → Item`), the `2^29 − 1` upper-tag-boundary marker, optional-primitive presence states. Negative tests cover spec §3.3 duplicate-tag and duplicate-map-key last-wins, plus the spec §3.2 length-bounded-region rejection rule. |
| `evolution/` | Paired `before/`/`after/` sub-packages feeding the schema classifier — covers `field/added`, `field/compat-write-added`, `field/wire-changed`, `field/type-changed`, `field/tag-changed`, and `field/removed`. Also exercises the `--allow-breaking` and `--allow-stop-compat-write` ack gates and a forward/backward round-trip across the safe-add scenario. |
| `rejection/` | Single fixture proving the schema validator surfaces the `field/anonymous` issue from `tools/gsbmschema/discover.go`; the positive sibling struct (named composition) confirms the rule is anonymous-only, not composition-only. No codegen runs against this package. |

The coverage map and rationale are tracked in `docs/plans/20260510-wire-format-evolution-test-suite.md`. Goldens live alongside each fixture and regenerate via `REGEN_GOLDEN=1 go test ./tools/gsbmcodegen/fixtures/<name>/ -run TestRegenGolden`.

## 4. Arena-mode implementation (planned)

### 4.1 Goal

Reduce decode-side allocations to a small constant (the arena itself plus growth) regardless of graph size, at the cost of restricted lifetime and mutability semantics.

### 4.2 API surface

```go
package gsbmarena

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
func DetachOffer(o *Offer, a *Arena) *gsbm.Offer
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
arena := gsbmarena.NewArena()
defer arena.Release()

offer, err := gsbmarena.DecodeOffer(blob, arena)
if err != nil {
    return err
}

// Read offer freely.
processOffer(offer)

// At end of scope, Release frees all memory at once.
```

The arena owns the bulk of the decoded graph: slice backing arrays (per-element-type `*gsbm.TypedPool[T]`), the root struct itself, and string bytes. Pointer-typed fields, value `[]byte` fields, and maps remain heap-allocated in v1 (see §4.5 and §4.6). After `Release`, the arena drops references to its chunks, but heap-allocated child objects whose strings alias arena bytes via `unsafe.String` would observe garbage if read past Release; treat the entire decoded graph as invalidated by Release.

The Go runtime cannot enforce this. The discipline is on the caller. Misuse produces silent corruption or crashes.

### 4.5 Generated code shape

Codegen for arena-mode produces a separate set of files (e.g., `offer_gsbm_arena.go`) with different signatures:

```go
// Heap-mode (existing)
func (o *Offer) UnmarshalGSBM(r *gsbm.Reader) error

// Arena-mode (new)
func unmarshalOfferArena(r *gsbmarena.Reader) (*Offer, error)
```

Arena-mode decoders share the heap-mode `UnmarshalGSBM` body and route allocations through the installed `Allocator` only at the seams the heap-mode body already calls into:

- **Slice backing arrays** — `gsbm.MakeSlice[T](r, n)` checks for `SlicePoolStore` and pulls from the arena's per-T `*TypedPool[T]`. Arena-routed.
- **Strings** — `r.AcquireString(b)` calls into the arena's chunked byte buffer and hands back an `unsafe.String` view. Arena-routed.
- **Root struct** — the per-root `Decode<Root>` wrapper uses `gsbmarena.AllocStruct[Root]` (which itself routes through the slice pool with n=1) before invoking `UnmarshalGSBM`. Arena-routed.

Three categories stay on the heap in v1, by design:

- **Optional struct pointers** (`*Customer`, `*OptInfo`, …): the heap-mode body emits `&Customer{}` and lets escape analysis send it to the heap.
- **Optional scalar/named pointers** (`*int64`, `*Quantity`, `*Label`, …): emitted as `var tmp T; v.X = &tmp`, also escapes to the heap.
- **`[]byte` fields** (required and optional): emitted via `append(dst[:0], b...)` so the decoded value owns its bytes; the append allocates on the heap when the destination has insufficient capacity (as it always does for a freshly arena-allocated zero struct).

Call this **Path A1** by analogy with §4.6 Path A for maps: the arena buys the slice, string, and root-struct wins; pointer fields and `[]byte` fields stay heap-allocated because routing them through the arena would require either generic interface methods (which Go does not support) or a generated `*_gsbm_arena.go` body separate from the heap-mode body (which the M8 design explicitly rejected — single decoder body per root). Lifting any of these onto the arena is a follow-up, gated on whether they show up as a measurable hotspot after Path A1 ships.

### 4.6 Maps in arena

Go's built-in `map` is runtime-managed and cannot be allocated in a custom arena. Two paths:

**Path A: heap-allocate maps even in arena mode.** Maps remain on the heap, the rest of the graph in the arena. Partial benefit, simpler implementation, no custom map type.

**Path B: vendor an arena-aware hash map.** A swiss-table-style implementation that allocates buckets in the arena. Larger code surface, full benefit.

The decision is deferred. Path A is the v1 of arena-mode; Path B is a possible later optimization. Until then, map fields in arena-decoded objects cost their normal heap allocations — arena-mode improves everything except maps.

### 4.7 Detach

`Detach` walks the arena-allocated graph and produces a heap-allocated copy compatible with the heap-mode `Offer` type. After Detach, the heap copy has full mutation rights and outlives the arena.

```go
arena := gsbmarena.NewArena()
offerArena, err := gsbmarena.DecodeOffer(blob, arena)
// ... read-only operations ...

if needToMutate {
    offerHeap := gsbmarena.DetachOffer(offerArena, arena)
    arena.Release()
    return mutateAndUse(offerHeap) // heap-mode Offer, fully owned
}

arena.Release()
```

Detach costs roughly the same as a heap-mode decode would have. The arena path wins when most objects are not detached — i.e., when the graph is read and discarded.

### 4.8 Limitations

- Mutation requires Detach; no in-place modification of arena objects.
- Maps remain heap-allocated (Path A) or require vendored map type (Path B).
- Optional pointer fields (struct pointers, scalar pointers) and `[]byte` fields stay heap-allocated in v1 (Path A1, see §4.5). Lifting them onto the arena is a follow-up.
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
storage/gsbm/                  # heap-mode runtime + generated heap-mode code
  writer.go
  reader.go
  marshaler.go                 # Marshaler/Unmarshaler interfaces
  offer.go                     # handwritten domain type + business methods
  offer_gsbm.go                 # generated heap-mode (Marshal/Unmarshal/Reset)
  segment.go
  segment_gsbm.go
  ...
  schema.yaml                  # generated schema artifact
  schema_snapshot.json         # generated machine-readable snapshot

storage/gsbmarena/              # arena-mode runtime + generated arena-mode code
  arena.go
  reader.go
  detach.go
  offer_gsbm_arena.go           # generated arena-mode decode
  segment_gsbm_arena.go
  ...
```

Arena-mode generated files reference the same domain types from `storage/gsbm/` (the `Offer` struct itself is shared — only the decode functions differ). The codegen tool runs twice: once for heap-mode targeting the `gsbm` package, once for arena-mode targeting `gsbmarena`. The same schema artifact drives both runs.

## 6. Codegen: two passes, same schema

The codegen tool exposes two subcommands of `cmd/gsbmschema` that share the schema-closure analysis: `gen` emits heap-mode `<type>_gsbm.go` files; `gen-arena` emits arena-mode `<root>_gsbm_arena.go` wrappers. Each subcommand runs its own pass; both see the same types, tags, and validation rules.

Adding arena-mode code does not change heap-mode output. A team using only heap-mode never invokes `gen-arena`.

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

- **Arena map strategy** (Path A vs Path B in §4.6). Decide before arena implementation begins; defaults to Path A. **Resolved (M8): Path A shipped.** Maps remain heap-allocated; `gsbm.MakeMap` in arena mode delegates to `make` exactly as in heap mode. Decoded map values may carry `unsafe.String` keys/values aliasing arena bytes, so a heap-allocated map's lifetime is bounded by the arena's. Path B (vendored swiss-table) stays a follow-up; the decision criterion is whether maps remain a measurable hotspot after Path A ships.
- **Allocator interface in heap-mode v1.** Even though arena-mode has its own runtime, exposing a minimal allocator interface in heap-mode `Reader` (e.g., for tests and benchmarks) may be useful. Decide during Milestone 4 of the master plan. **Resolved (M4):** the heap-mode `Reader` carries an `Allocator` field with a single `AcquireString` method, plus a `SlicePoolStore` extension interface queried by `MakeSlice[T]` via type assertion. The default heap path keeps the field nil and short-circuits to `make` / `string([]byte)`.
- **Linting for arena misuse.** Static analysis catching use-after-Release patterns is desirable. Likely a custom analyzer leveraging `go/analysis` infrastructure. Out of scope for the initial arena implementation; track as a follow-up.
- **Detach implementation strategy.** Two viable approaches: (a) walk the graph manually with generated copy code; (b) reuse the heap-mode unmarshal path by feeding it the arena's pre-decoded structure. Decide during arena implementation. **Resolved (M8): option (b).** `DetachRoot` is generated as a thin wrapper that calls the heap-mode `MarshalGSBM` to a fresh buffer and then `gsbm.DecodeBodyInto` into a heap-allocated copy. There is exactly one decoder body per root, not two; the cost is one extra encode+decode per Detach call, paid only when the caller needs to outlive the arena.

## 9. M8 arena runtime — concrete shape

The heap-mode `UnmarshalGSBM` body is reused unchanged in arena mode. The arena hooks into the existing allocation seams:

- `Reader.AcquireString` routes through the installed `Allocator`. The arena's `AcquireString` copies bytes into a chunked byte buffer and returns an `unsafe.String` view.
- `gsbm.MakeSlice[T](r, n)` does a type-assertion check — if the allocator implements `SlicePoolStore`, it pulls a `*gsbm.TypedPool[T]` from the arena's `map[reflect.Type]any` and bumps a sub-slice off the pool's current chunk. Otherwise it falls through to `make([]T, n)`. This routing makes one generated `UnmarshalGSBM` body work for both modes.
- `gsbm.MakeMap` always delegates to `make` (Path A).
- Pointer fields and `[]byte` fields are **not** seamed: the heap-mode body emits `&T{}`, `var tmp T; &tmp`, and `append(dst[:0], b...)` directly, and those allocations land on the heap in both modes (Path A1; see §4.5).

Per-root arena codegen (`<root>_gsbm_arena.go`) emits three thin wrappers:

```go
// allocate root from arena, install allocator, parse header, run heap UnmarshalGSBM
func DecodeOrder(data []byte, a *gsbmarena.Arena) (*Order, error)
// same, headerless
func DecodeOrderBody(data []byte, a *gsbmarena.Arena) (*Order, error)
// re-encode + DecodeBodyInto into a heap-allocated *Order
func DetachOrder(o *Order) (*Order, error)
```

Nested structs (Customer, Item, Total, …) need no arena-specific code: their `UnmarshalGSBM` calls the arena-routed allocators (`MakeSlice`, `AcquireString`) when invoked through an arena `Reader`. Optional `*Nested` and `[]byte` fields inside those structs still allocate on the heap (Path A1).

### Mutation semantics

Arena-decoded values are read-only by contract. The underlying memory is shared between callers in the worst case — a `[]Item` chunk pool may hand the same backing array to two distinct decode calls within one arena lifetime. Mutating an arena-decoded field:

1. Risks corrupting another caller's view of the same chunk.
2. Risks silent UAF after `Release()` — Go does not invalidate pointers on `make([]T, 0)`, and an `unsafe.String` view does not error when its bytes are reclaimed; it returns garbage.

Code that needs mutable state calls `Detach<Root>` first to lift onto the heap. The audit of which existing receiver methods on the domain model mutate state is captured at the consuming repo's call site (offer methods live in ooms-offerengine, not in gsbm); the contract is recorded here so the arena's invariants are explicit.

## 10. Schema lifecycle: compat_write

A field that participates in a *replacement* migration — its tag is being phased out and a successor introduced at a fresh tag — passes through an extra lifecycle state, `deprecated, compat_write`, between `active` and plain `deprecated`. The state is encoded in the `bin:` struct tag and consumed by both the schema-diff tool and the encoder, so the rollback-safety story (`spec.md` §7.4) is enforced by tooling rather than left to a README rule.

```
active                    bin:"7"
deprecated, compat_write  bin:"7,deprecated,compat_write"
deprecated                bin:"7,deprecated"
reserved                  //gsbm:reserved 7
```

State transitions recognized by `tools/gsbmschema/classifier.go`:

- `active → deprecated, compat_write` — `safe`. The field is being phased out; the encoder still writes it so any rollback to the prior schema continues to see the value.
- `deprecated, compat_write → deprecated` — emitted as `breaking` (`field/compat-write-removed`). The CI gate blocks unless `gsbmschema diff` is invoked with `--allow-stop-compat-write`, which marks the entry acknowledged (mirroring how `//gsbm:allow-breaking` acknowledges other breaking changes). The severity stays `breaking` in the diff output; only the gate behavior changes. The classifier cannot enforce calendar time, so it requires explicit operator acknowledgement that the rollback window has elapsed.
- `deprecated → reserved` — `safe` (existing rule, unchanged).
- `active → deprecated` (skipping `compat_write`) — `warning` with code `field/deprecated`. Permitted for additive-only migrations where no rollback risk exists, but the classifier surfaces the skip so reviewers see it.

Codegen behavior follows the lifecycle state:

- `active` and `deprecated, compat_write` fields are written by `MarshalGSBM`. The `writableFields` helper in `tools/gsbmcodegen/emit.go` is the union of the two.
- `deprecated` (without `compat_write`) and `reserved` fields are not written.
- All four states decode identically. A deprecated field that still exists in the Go struct decodes into it; a deprecated field that has been removed from the Go struct hits the unknown-tag path and is skipped via wire-type rules. The decoder never branches on `compat_write`.

Operationally, a replacement migration lands in three steps:

1. Add the successor field at a fresh tag and mark the old field `deprecated, compat_write`. Classifier reports `safe`. Encoders keep emitting the old tag, so application code MUST continue populating the deprecated field for the duration of the window — the codegen does not synthesize the value from the successor field. Typically this means writing both fields at the call site (or having the setter for the new field also assign the old one). Without this app-level discipline the encoder will write the field's zero value to the old tag, defeating the rollback guarantee.
2. Deploy and bake for at least the deployment's rollback window (recommended two full windows). The classifier cannot enforce calendar time, so the operator's explicit `--allow-stop-compat-write` flag stands in for the bake-time check.
3. Flip the old field from `deprecated, compat_write` to plain `deprecated`. The classifier still emits `breaking` (`field/compat-write-removed`); the diff command requires `--allow-stop-compat-write` to acknowledge the entry and unblock the CI gate (the severity in the diff output remains `breaking`). Encoders stop emitting the old tag. Application code can stop populating the old field at the same time.

The lifecycle is forward-only. Fields tagged `deprecated` before this change shipped do not retroactively pass through `compat_write`; only fields that adopt the annotation after this lands participate.
