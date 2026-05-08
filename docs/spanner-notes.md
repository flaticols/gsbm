# odm-bin: Binary Serializer for ODM → Spanner

## Goal

Replace `domain → protobuf → bytes` storage path in `ooms-offerengine` with `domain → bytes` direct serialization. Eliminates the protobuf mapping layer (~20% of total request allocations) and the intermediate PB object held in heap during Spanner Apply.

Not a goal: replacing protobuf in gRPC, Kafka, or any path other than Spanner storage.

## Constraints

- Hot path is 600 RPS across ~1000 GKE pods. One request can produce a shelf of ~20-100 offers, written atomically (one mutation, one commit).
- Atomicity requirement: the entire shelf must commit or fail; streaming write is not an option. Heap retention from holding the encoded payload until Spanner ACK is part of the cost picture.
- No production traffic capture available, no shadow deploys. Only Datadog dashboards and a partly-faked perf cluster. Validation happens through local micro-benchmarks on representative payloads plus QA observation.
- Domain model is large, contains generics, has near-zero `interface{}`/`any` usage. Cannot be rewritten to fit a foreign schema (already failed twice with protobuf-as-domain-model).
- **Domain model is authored by hand and stays that way.** Structs and their receiver methods (business logic, validators, computed properties) are not generated. Codegen only produces companion files (`*_odm.go`) alongside handwritten files within the same package. Existing repository layout already follows this split.
- **Indefinite retention.** Production data must be readable years after writing. Backfill of historical records is not feasible at scale. The format must support reading old records with current code without rewriting the data. The legacy protobuf format already lives across v1–v4 in production; the new format must not introduce the same fragmentation.

## Non-goals

- MessagePack wire compatibility. Only Go ↔ Go through Spanner; cross-language reads not required.
- Compactness above all. zstd at the Spanner layer covers blob size if it ever matters.
- Runtime reflection of any kind. All type knowledge is resolved at codegen time.
- Streaming encode/decode. Whole-blob in a buffer is fine and matches Spanner's interface.

## Prototype results (already shipped, ordering-based format)

Apple M2 Max, Go benchmark, Spanner offer storage workload.

| Fixture | Path | Method | ns/op | B/op | allocs/op |
|---|---|---|---|---|---|
| Synthetic 100 offers | Encode | odm-bin-v1 | 1,452,730 | 2,389,662 | 4,803 |
| Synthetic 100 offers | Encode | protobuf | 5,855,134 | 6,896,241 | 56,904 |
| Synthetic 100 offers | Decode | odm-bin-v1 | 3,758,028 | 5,512,519 | 95,301 |
| Synthetic 100 offers | Decode | protobuf | 8,033,257 | 11,893,152 | 154,210 |
| BDD 40 offers | Encode | odm-bin-v1 | 85,184 | 245,760 | 1 |
| BDD 40 offers | Encode | protobuf | 318,485 | 411,200 | 3,123 |
| BDD 40 offers | Decode | odm-bin-v1 | 204,616 | 325,121 | 5,241 |
| BDD 40 offers | Decode | protobuf | 423,245 | 688,762 | 8,769 |

Synthetic encode is 4x faster with 11.8x fewer allocations. BDD encode is 3.7x faster at exactly 1 alloc/op (single buffer growth). Decode is 2.1x faster but still allocates because it rebuilds the ODM graph from scratch.

The prototype validates the direction. It is not the final wire format — see "Format v2" below.

## What changes from the prototype

The prototype encodes struct fields by declaration order. That is unsafe for production: any reorder, insert, delete, or rename in a Go struct silently breaks all existing blobs. Reviewer was right to flag backward compatibility.

The production format moves to **tagged fields** with explicit numeric IDs declared via struct tags. Layout reorders are tolerated, schema evolution is bounded by review, `fieldalignment` becomes free to apply.

## Wire format v2

### Blob header (8 bytes)

```
+--------+--------+--------+--------+
|         magic "ODMB" (4B)         |
+--------+--------+--------+--------+
| fmtVer | flags  |    schVer (2B)  |
+--------+--------+--------+--------+
|        body (root struct)         |
+-----------------------------------+
```

- `magic` — `'O','D','M','B'`. Catches accidental reads of foreign blobs.
- `fmtVer` (uint8) — wire format version. Bumped only when the encoding rules themselves change (rare, years apart). Initially `1`.
- `flags` (uint8) — reserved bitfield. Bit 0 reserved for built-in compression flag, bits 1-7 reserved.
- `schVer` (uint16) — schema fingerprint hash, computed by codegen from the type closure. Visible in Datadog for rollout tracking.

8 bytes overhead amortizes to nothing on 40-offer shelves.

### Field encoding

Every field inside a struct is encoded as `<key:varint> <value>`, where:

```
key = (tag << 3) | wire_type
```

- `tag` is the stable numeric ID declared on the field (`bin:"5"`).
- `wire_type` (3 bits) tells the decoder how to skip unknown fields without knowing the schema.

Wire types (reusing the protobuf scheme because it is correct, not for compatibility):

| Code | Name | Used for |
|---|---|---|
| 0 | VARINT | int*, uint*, bool, enums |
| 1 | FIXED64 | float64, fixed-size int64 |
| 2 | LENGTH_DELIM | string, []byte, nested struct, slice, map |
| 3 | FIXED32 | float32 |
| 4-7 | RESERVED | future (extension types, etc.) |

Tags `1-15` fit in one varint byte (4 bits available after wire_type), so frequent fields should get low tags.

`tag = 0` is reserved globally (never used for real fields), kept available for future schema migration markers inside structs.

### Struct termination

Root struct: read until end of blob. No length prefix needed.

Nested structs: **length-prefixed**. The field key signals LENGTH_DELIM, then a varint of the byte length, then the payload. This gives O(1) skip for unknown nested structs and bounds reads against corruption. Cost is a few bytes per nested struct, paid for by guaranteed forward-compat.

### Slices and maps

`[]T`:

```
<count:varint> <element_0> ... <element_n>
```

No per-element tag — element type is fixed by schema. `count` enables preallocation in DecodeInto.

`map[K]V`:

```
<count:varint> <key_0><value_0> ... <key_n><value_n>
```

Map iteration order in Go is random. Two encodes of the same map produce different bytes. **Decision: do not sort keys in v1.** Round-trip equality is verified through `decode → DeepEqual`, not byte equality. Hashing of blobs is not currently a use case. If it becomes one, sorting can be added per-field via pooled key slices without a wire format change (the wire format does not specify ordering).

There are ~5 map fields in the offer model. Acceptable cost if sorting becomes required later.

When a slice or map is a struct field, it is wrapped in LENGTH_DELIM at the field level — the length prefix is the total bytes of the encoded slice/map (including its own count). This duplicates length+count slightly but enables skipping unknown slice/map fields without knowing the element type.

**Map key constraint:** keys must be primitive types or string. `map[ComplexStruct]V` is rejected by the codegen validator. Such maps are a code smell in storage models anyway and should be refactored to `[]Pair{K,V}` or ID-keyed.

### Nullable fields and presence-byte

Pointer fields (`*int64`, `*Segment`) require explicit nil/zero-value distinction. The original 1-bit presence byte is upgraded to a metadata vector:

| Bit | Name | Meaning |
|---|---|---|
| 0 | present | 0 = nil, 1 = value follows |
| 1 | zero-elide | 1 = present but value is zero of its type, payload omitted |
| 2 | dirty | reserved for explicit-set tracking (partial updates) |
| 3-7 | reserved | future use |

Bit 0 + bit 1 give three useful states: `00` nil, `01` present-and-nonzero (read payload), `11` present-and-zero (skip payload, restore zero). `10` is reserved.

Zero-elision applies only to **Go builtin types** (int, float, bool, string). User-defined types (`Currency`, `OfferID`) do not elide, because changing their internal structure later would change zero-value semantics and silently corrupt elided historical records. Codegen distinguishes by AST.

For a struct with many nullable fields, a header bitmap could replace per-field presence bytes (4 bytes covers 32 nullable fields). **Not in v1.** Add only if blob size measurements demand it.

### Strings

UTF-8 not validated on decode. Writers are trusted. Validation is a separate quality gate, not a wire concern.

### Floats

`math.Float32bits` / `Float64bits` round-trip. NaN, ±Inf preserved as IEEE 754. Round-trip property tests handle NaN equality specially.

### Endianness

LittleEndian, matching the prototype. This is a long-term commitment; changing it requires a `fmtVer` bump.

## Schema management as a graph

The schema is not "a version number" — it is the closure of types reachable from declared roots. Versioning, validation, and evolution all derive from this graph.

Source of truth is the **handwritten Go code** (struct definitions with `bin` tags, receiver methods, deprecation markers in comments). Schema is a **derived artifact** generated from the AST and committed for review. This direction is forced by the constraint that the model is handwritten — generating Go from a schema would lose receiver methods and require awkward extension patterns.

### Declaration

A root type is marked by a directive:

```go
//odm:root
type Offer struct {
    ID         OfferID         `bin:"1"`
    // bin:"2" reserved (was: legacyCode, deprecated in PR #1234, removed in PR #5678)
    Carrier    string          `bin:"3"`
    Segments   []Segment       `bin:"4"`
    Taxes      map[string]Tax  `bin:"5"`
    LegacyCode string          `bin:"6,deprecated"` // not written by new code; readable from historical records
    RetailCode int64           `bin:"7"`            // replacement for LegacyCode
}
```

There can be multiple roots in the codebase (Offer, Shelf, Booking, etc.). Each root has its own schema fingerprint and snapshot.

### Codegen pipeline

Codegen runs in distinct phases, each independently useful:

**1. Discovery.** Find all `//odm:root` types in the package set.

**2. Closure.** Transitive AST walk: from each root, gather every type reachable through fields, slice elements, map K and V, generic instantiations. Use `go/types` to resolve generic instantiations to concrete types. Closures from different roots may overlap; shared types get a single set of tags (consistent across roots that contain them).

**3. Validation.** Run schema invariants (see below). Fail with explicit errors on violations.

**4. Schema artifact emission.** Serialize the closure into two committed files:
- `schema.yaml` — human-readable, structured description of types, fields, tags, deprecation markers, reserved tags. The diff of this file in a PR is the canonical schema review surface.
- `schema_snapshot.json` — machine-readable equivalent for CI consumption.

**5. Schema diff classifier.** Compare new artifact against committed version. Classify changes as `safe` / `warning` / `breaking`. Block CI on `breaking` unless explicit override.

**6. Hash.** Compute stable hash from sorted closure description. This becomes `schVer` in the wire header.

**7. Codegen.** Emit `MarshalODM` / `UnmarshalODM` / `Reset` for every type in the closure into companion `*_odm.go` files alongside the handwritten files. Roots get the version-header wrapper. All tags and switch cases are inlined as literals; no runtime tag lookups, no reflection. Companion files carry the standard `// Code generated. DO NOT EDIT.` header.

Phases 1-5 can run as a standalone linter independent of codegen, useful for fast IDE/precommit feedback.

### File layout

Within a package containing domain types:

```
storage/odm/
  offer.go              // handwritten: type Offer + bin tags + business methods
  offer_odm.go          // generated:   MarshalODM, UnmarshalODM, Reset on Offer
  segment.go            // handwritten
  segment_odm.go        // generated
  ...
  schema.yaml           // generated artifact, committed, reviewed in PRs
  schema_snapshot.json  // generated machine-readable, committed for CI diff check
```

Codegen never touches handwritten files. `go generate` overwrites only `*_odm.go` and the two schema artifacts.

### Schema diff classifier

The classifier reads old and new schema artifacts and labels each change. Labels are deterministic, defined by formal rules over the diff:

**Safe (always allowed):**
- Add new field with previously-unused tag.
- Mark existing field as `deprecated`.
- Add reserved tag.
- Add new root type.
- Rename field in Go (struct field name changes; tag stays the same — wire format unaffected, but reviewer should still confirm semantic intent).

**Warning (allowed with explicit acknowledgment in PR):**
- Add a new tag that resurrects a previously deprecated tag with the same field type. (Suspicious: usually wrong, sometimes legitimate after a long deprecation.)
- Add a custom marshaler annotation to a previously-default-marshaled type.

**Breaking (blocked unless `//odm:allow-breaking` directive present in the change with justification):**
- Remove a field outright (without a deprecation period).
- Reuse a tag for a field of a different type.
- Change the tag of an existing field.
- Change the type of an existing non-deprecated field.
- Remove a deprecated field that is still potentially present in historical records.

Rules live as code (Go), tested with property-tests over pairs of `before/after` schemas. The classifier is callable both from CI and locally as `odm-schema diff`.

### Append-only schema policy

The classifier enforces an **append-only** policy by default. This is the deliberate choice for indefinite-retention storage:

- Fields are added, not removed. Removal goes through a deprecation period: field stays in the struct, marked `bin:"N,deprecated"`, no longer written by new code, but still readable from historical records.
- Tags are never reused. Once a tag is associated with a field (or marked reserved), it is permanently bound.
- Type changes go through a new field plus deprecation of the old. `LegacyCode string` becomes irrelevant → mark deprecated; introduce `RetailCode int64` with a new tag. Reader prefers the new field, falls back to the old one if the new is zero.
- Semantic changes (same type, different meaning) follow the same rule — they are new fields, not modifications.

The policy holds because the wire format gives forward and backward compatibility within a single `fmtVer`: old readers skip unknown tags, new readers fill missing tags with zero values. With append-only as the discipline, **`fmtVer` does not change with normal schema evolution**. Production records written years apart all have `fmtVer=1` and are read by the same decoder.

Cost of append-only: the struct accumulates deprecated fields over time. After several years, an Offer struct may carry tens of deprecated fields. RAM cost is bounded (a deprecated string field is one pointer + one length + zero data when unset) and practically negligible. Code clarity cost is the larger concern, mitigated by clustering deprecated fields at the bottom of structs and code review attention.

### When `fmtVer` actually changes

Only when the **encoding rules themselves** change — varint scheme, endianness, header layout. This is genuinely rare (years apart). When it happens, it is a deliberate, planned migration with parallel decoder versions during transition. It is not driven by domain model evolution.

If at some point the deprecated field accumulation becomes intolerable, an `fmtVer` bump is the moment to clean up — all deprecated fields drop out of the new model. By that time, historical reads of old `fmtVer` records use the legacy decoder, which still knows about deprecated fields. The two decoders coexist as long as records of both versions exist.

### Validation rules (over Go AST and schema)

1. **Every field in a closure type has a `bin` tag, or `bin:"-"` to opt out explicitly.** No silent omissions.
2. **No type appears in a closure unintentionally.** Non-domain types reachable from a root require explicit `bin:"-"` on the offending field or `//odm:opaque` on the type.
3. **No cycles.** Cycles cause infinite recursion. Codegen rejects them. Intentional cycles require `//odm:cycle_break_via_id` and ID-reference encoding.
4. **Reserved tags are honored.** Never reuse a tag listed in struct comments as reserved.
5. **Map keys are primitive or string.** Complex map keys are rejected.
6. **Deprecated fields are not written.** Codegen omits `WriteTag` calls for fields tagged `deprecated`. Decode still reads them.
7. **Tag uniqueness.** No two fields in the same struct share a tag. No tag collides with reserved.

### Why a graph framing is the right one

- Reachability detects forgotten types (added to a struct but missed annotation) and dead types (in codegen but unreferenced).
- Schema diff = graph diff. Add/remove node, retag edge. Code review sees structural change, not blob byte changes.
- Validation is graph properties: acyclicity, no orphan types, single-source reachability.
- Versioning is graph fingerprint. Same graph → same version, by construction.

The validation infrastructure overlaps with `kgraph` and `gorefact`; AST-walking is already familiar territory.

## Generated code shape

For each struct in the closure:

```go
func (o *Offer) MarshalODM(w *odm.Writer) error {
    odm.WriteTag(w, 1, odm.WireLengthDelim)
    o.ID.MarshalODM(w)

    odm.WriteTag(w, 3, odm.WireLengthDelim)
    odm.WriteString(w, o.Carrier)

    odm.WriteTag(w, 4, odm.WireLengthDelim)
    writeOfferSegments(w, o.Segments)

    odm.WriteTag(w, 5, odm.WireLengthDelim)
    writeOfferTaxes(w, o.Taxes)

    return w.Err()
}

func (o *Offer) UnmarshalODM(r *odm.Reader) error {
    for r.HasMore() {
        key, _ := r.ReadVarint()
        tag := key >> 3
        wt := key & 7
        switch tag {
        case 1:
            if err := o.ID.UnmarshalODM(r); err != nil { return err }
        case 3:
            o.Carrier, _ = r.ReadString()
        case 4:
            if err := readOfferSegments(r, &o.Segments); err != nil { return err }
        case 5:
            if err := readOfferTaxes(r, &o.Taxes); err != nil { return err }
        default:
            r.SkipField(wt)
        }
    }
    return nil
}
```

All tags are literals. The switch compiles to a jump table when dense. Unknown tags are skipped by wire-type, preserving forward compatibility.

### Specialized accessors per field type

Codegen emits a per-field-type specialized function (or inlines, when small enough) — never a reflection-based generic loop:

- Primitive → inline `WriteVarint`/`WriteString`/etc.
- Named type with method → call `t.MarshalODM(w)`.
- Pointer → presence-byte then conditional payload.
- Slice → length prefix, specialized loop body.
- Map → length prefix, specialized loop, both K and V resolved.
- Generic instantiation → call generic method, parameterized at instantiation.

Generic containers are encoded once at the generic level, constraint-bounded:

```go
func (l *List[T]) MarshalODM(w *odm.Writer) error {
    odm.WriteVarint(w, uint64(len(l.items)))
    for i := range l.items {
        l.items[i].MarshalODM(w)
    }
    return w.Err()
}
```

The constraint `T: Marshaler` carries the method requirement. Primitive instantiations (`List[int64]`) get specialization — codegen emits a concrete `writeListInt64` rather than wrapping primitives in named types.

### DecodeInto with reset

To eliminate decode-side allocations on hot read paths, codegen also emits a `Reset` method:

```go
func (o *Offer) Reset() {
    o.ID.Reset()
    o.Carrier = ""
    o.Segments = o.Segments[:0]
    for i := range o.Segments {
        o.Segments[i].Reset()
    }
    clear(o.Taxes) // preserves capacity
}
```

`DecodeInto(data []byte, dst *Offer) error` calls `dst.Reset()` then decodes, reusing slice and map capacity. Combined with `sync.Pool` of root objects at the call site, sustained read workloads converge to zero allocations after warmup.

Reset must be deeply recursive — preserving inner map/slice capacity matters as much as outer.

## Spanner integration

Two columns per row:

- `format_version int8` — `0` for legacy protobuf, `1+` for odm-bin. Operationally queryable, unambiguous about which decoder to use.
- `payload bytes` — the blob, header included.

Storing format version both as a column and as the `fmtVer` byte in the blob is intentional redundancy: the column gives operational visibility (counts, queries, dashboards), the blob bytes keep the format self-describing if the data ever moves outside Spanner.

### Migration plan

The append-only schema policy combined with indefinite retention means there is **no backfill phase**. Historical protobuf records remain readable through the existing PB decoder for as long as they exist in Spanner. The new format is introduced as a parallel write path, and over time new records accumulate in the new format while old PB records are read through the legacy path on demand.

**Phase 1: dual-write, single-read PB.** New code writes both PB and odm-bin (or, equivalently, only odm-bin into a new column while PB still goes to its existing column). All reads remain from PB. Roll out, observe Datadog for a week. odm-bin path is exercised by production traffic but not depended on. Reversible by feature flag.

**Phase 2: switch reads to odm-bin for new records.** The reader checks `format_version`: if `1+`, decode via odm-bin path; if `0` (legacy PB), decode via the existing PB path. Both paths return `current.Offer` to the business layer, which is unchanged. New records (written after Phase 1 rolled out) are read via odm-bin; pre-existing records are read via PB.

**Phase 3: stop dual-write.** New code writes only odm-bin. PB column receives no new data but is preserved indefinitely for historical reads. The mapping code from domain to PB is deleted; this is when the allocation reduction lands in production. The PB decoder remains in the codebase as the legacy reader.

There is no Phase 4. Old PB records are never rewritten. The PB decoder lives in the codebase indefinitely as a read-only path for historical data. Its maintenance cost is near zero — it doesn't change because the PB schema doesn't change.

If, years from now, the volume of PB records becomes negligible (natural data lifecycle, retention policies, archival to cold storage), the PB decoder and its column can be retired in a separate cleanup project. That decision is independent of this work.

## Arena allocation (deferred, abstraction reserved)

A long-term improvement for the read path: instead of allocating each decoded slice, map, and sub-struct individually, allocate the entire decoded ODM graph in a single contiguous arena buffer. Releasing the graph becomes one operation rather than thousands of GC-tracked allocations. This is the model used by Cap'n Proto.

The realistic gain on read: the prototype decode currently allocates ~5,241 times for 40 BDD offers, mostly slice/map/sub-struct creation while rebuilding the graph. With arena allocation this could reduce to single-digit allocations (the arena itself plus growth). It is qualitatively different from `Reset` + `sync.Pool`, which still incurs allocations on first warmup and on growth events.

This is **not in v1**. The complications make it a separate milestone:

- **Strings and `[]byte`** in the decoded model would need to live in the arena, requiring `unsafe.String` references with lifetime managed against arena release. Mistakes here are use-after-free.
- **Go's `map`** is runtime-managed and cannot be allocated in a custom arena without replacing it with an arena-aware hash table. Either accept maps remain heap-allocated (partial win) or vendor a swiss-table-style map.
- **Mutation semantics.** Business code on the domain model includes receiver methods that mutate state (e.g., recalculation). Arena-decoded objects are typically read-only by convention; mutation would require an explicit `Detach()` step that copies to heap before mutating. This compromises the benefit for any read-modify-write path.
- **Lifetime ownership.** An explicit arena release API is needed (`offer.Release()` or similar). Conventions for caller responsibility have to be documented and enforced through linting.

What v1 must do to keep the door open: the `Reader` interface should not call `make([]T, n)` or `make(map[K]V, n)` directly. It should call into an allocator abstraction:

```go
type Allocator interface {
    NewSliceSegment(n int) []Segment
    NewMapStringTax(n int) map[string]Tax
    AcquireString(b []byte) string  // either copies (heap-mode) or references (arena-mode)
}
```

Default allocator wraps `make` and `string([]byte)`. Arena-mode allocator can be added later without rewriting decoders. Codegen emits allocator calls instead of inline allocations from day one. This costs almost nothing in v1 and prevents a large rewrite when arena work happens.

Rules below are enforced by the schema diff classifier (see "Schema management as a graph"). The default policy is append-only.

What is safe (classifier label `safe`, allowed automatically):

- **Add field.** New tag, old readers skip via wire-type. Forward compat free.
- **Mark field deprecated.** Field stays in struct, no longer written by new code, still readable from historical records. Old readers still write it, which is fine — new readers read it as a normal field.
- **Rename field in Go.** Tag stays the same, wire format unchanged. Free.
- **Reorder struct fields.** Wire format does not depend on declaration order. `fieldalignment -fix` can run anytime.
- **Add a new root type.** Independent fingerprint, no impact on existing roots.
- **Add reserved tag.** No-op for the wire format, prevents future reuse.

What is breaking (classifier label `breaking`, blocked unless `//odm:allow-breaking` directive present with justification):

- **Remove a field.** Use deprecation instead. Outright removal breaks readers of historical records.
- **Reuse a tag.** A tag once associated with a field (active or deprecated) is permanently bound to that field.
- **Change tag of an existing field.** Same as removing the field and adding a new one — historical records become unreadable for that field.
- **Change type of an existing field.** Old payload no longer fits. Use a new field with a new tag instead.

What requires `fmtVer` bump:

- **Changes to the encoding rules themselves** — varint scheme, endianness, header layout. Years apart. Done as a deliberate, planned migration with parallel decoder versions during transition.
- **Bulk cleanup of accumulated deprecated fields.** When the deprecated field count becomes intolerable (years out), an `fmtVer` bump can be the moment to drop them. The legacy decoder remains for old records.

The `bin:"-"` directive lets a field opt out of serialization entirely (analogous to `json:"-"`). The `bin:"N,deprecated"` directive marks a field as no-longer-written but still-readable.

## Backward / forward compatibility guarantees

- **Forward compat:** old binary reading new blob — unknown tags skip via wire-type. Always works within the same `fmtVer`.
- **Backward compat:** new binary reading old blob — missing tags decode as zero values. Always works within the same `fmtVer`.
- **schVer is observability, not decoder selection.** A single decoder per `fmtVer` reads any record of that `fmtVer`, regardless of `schVer`. The schVer fingerprint is for Datadog visibility (rollout tracking, schema-drift detection across services), sanity checks (warn on never-seen-before schVer), and backfill targeting (if ever needed). It does not switch decoder logic.
- **Cross fmtVer:** explicit. The reader registry maps `fmtVer → decoder`. Each `fmtVer` keeps its decoder for as long as records of that version exist. Removed only by deliberate cleanup project, never automatically.
- **Rollback safety:** because both directions of compat hold within a `fmtVer`, a deployment can be rolled back without re-encoding data. New code added a field with tag=7, was deployed, wrote some records, rolled back — old code reads those records, unknown tag=7 is skipped, business logic gets zero value where the new field would have been. No data loss.

## What is deferred

- Arena allocator for the decoded graph. Abstraction reserved in v1 (allocator interface in `Reader`), implementation later. See "Arena allocation" section.
- Sorted map keys for byte-stable encoding. Add only when a use case demands it.
- Header bitmap for batch nullable presence. Add only if blob size matters.
- Built-in compression (`flags` bit 0). zstd at the Spanner layer is simpler.
- Wire-type 4 (extension types) for ULID, decimal with custom representation. Currently encoded as length-delimited bytes.
- Multi-record blobs. Single-root only for now.
- Schema-as-data export consumable by other languages. The wire format is Go-only by design at this stage; `schema.yaml` is for human review, not as a portable IDL.

## Implementation milestones

**Milestone 1 — Tagged format core.** Replace prototype's ordering-based encoding with tagged fields, header, length-prefixed nested structs, wire-types for skip. Re-run benchmarks. Expect 5-15% slowdown vs prototype, still in the 3-4x improvement range over PB.

**Milestone 2 — Presence-byte for nullables.** Bit 0 nil, bit 1 zero-elide, bit 2 reserved-dirty, bits 3-7 reserved.

**Milestone 3 — Schema graph + validation + classifier.** Discovery, closure, validation rules, schema.yaml + snapshot generation, diff classifier with append-only enforcement. Standalone linter mode for IDE/precommit. This is the schema management surface; without it, append-only policy is just a README and breaks under team pressure.

**Milestone 4 — Allocator abstraction + Reset + DecodeInto.** Reader uses an allocator interface (default: heap). Recursive Reset codegen, DecodeInto API, sync.Pool integration in call sites. Eliminates the bulk of decode-side allocations for hot read paths.

**Milestone 5 — Spanner Phase 1 dual-write.** Roll out write path to QA, observe Datadog. No production read dependency.

**Milestone 6 — Spanner Phase 2 read switch.** Reader dispatches by `format_version` column. New records read via odm-bin, historical records read via PB. No backfill.

**Milestone 7 — Spanner Phase 3 stop dual-write.** New code writes only odm-bin. PB mapping code deleted. PB decoder remains as legacy reader.

**Milestone 8 (later) — Arena allocator.** Implement arena-mode allocator. Update `string` handling to reference arena memory via `unsafe.String`. Document mutation semantics (read-only by default, `Detach()` for mutation). Initially apply to specific read-heavy paths (e.g., audit, replay), not as default.

## Open questions

- How many root types in scope total? Confirms whether multi-root support is needed from milestone 3 or can be deferred.
- Are there cycles in the current offer model (parent pointers, back-references)? Needs answer before milestone 3 to decide whether cycle-breaking is required immediately or only validation against cycles.
- Concurrency limit on Spanner writes — independent of this work, but the OOM picture also includes heap retention from holding 100+ goroutines × 4 copies of payload during Apply. A semaphore is a one-line fix worth landing in parallel with this project, not after.
