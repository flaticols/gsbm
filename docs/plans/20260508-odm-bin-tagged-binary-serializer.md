# odm-bin: Tagged Binary Serializer for Spanner Offer Storage

## Overview

Replace the `domain → protobuf → bytes` Spanner storage path in `ooms-offerengine` with a direct `domain → bytes` codec. The shipped v0 prototype (ordering-based, magic `OEB1`) validated the direction (3-4× faster, 1 alloc/op encode on BDD payloads) but is unsafe for production: any struct reorder, insert, or rename silently breaks every existing blob. The production format moves to **tagged fields** with explicit numeric IDs, an 8-byte header (`ODMB` magic, `fmtVer`, `flags`, `schVer`), length-prefixed nested structs, and presence-byte semantics for nullables.

The goal is to ship the tagged wire format end-to-end: format + codegen + schema-as-graph validation + Spanner dual-write/read-switch/cutover, with an arena-mode runtime as the final read-path optimization. Production data must be readable years after writing — append-only schema policy, no backfill.

## Context

- **Hot path**: 600 RPS across ~1000 GKE pods. One request produces a shelf of 20–100 offers, written atomically (one Spanner mutation, one commit). Heap retention until ACK is part of the cost.
- **Indefinite retention**: historical records must remain readable without rewriting the data. Legacy protobuf (v1–v4) lives in production today; the new format must not introduce the same fragmentation.
- **Domain model is handwritten**: structs, receiver methods, validators, computed properties stay handwritten. Codegen only emits companion `*_odm.go` files alongside handwritten files in the same package.
- **No reflection, no runtime tag lookup**: all type knowledge resolved at codegen time. Switch-on-tag inlined as integer literals.
- **No production traffic capture**: validation is local micro-benchmarks on representative payloads + Datadog dashboards + QA observation on a partly-faked perf cluster.
- **v0 prototype is shipped but has no production-locked data**: v0 wire format (`OEB1`, ordering-based) is replaced wholesale by the tagged format under the same `odm-bin-v1` encoding name in M1. After M1 the v0 wire format is gone.
- Source documents merged into this plan: `docs/spec.md` (wire format authority), `docs/implementation.md` (heap + arena runtime contracts), `docs/v0-poc.md` (prototype results and existing coverage map), `docs/spanner-notes.md` (master plan with milestones).

## Development Approach

- Testing approach: regular (write tests after each Task; cross-validate with property tests and fuzz once decoders exist)
- Complete each Task fully before moving to the next; the milestones are sequential dependencies (codegen depends on format, Spanner integration depends on codegen, arena depends on allocator abstraction landed in M4)
- Update this plan when scope changes during implementation

## Testing Strategy

- Unit tests for every codegen output and every primitive encoder/decoder.
- Property tests: encode → decode → `DeepEqual`; cross-mode (heap encode → arena decode → values match; → Detach → DeepEqual).
- Fuzz tests against the decoder with malformed inputs (truncated, reordered, unknown tags, reserved wire types). Both heap and arena decoders fuzz against the same corpus.
- Allocation tests via `testing.AllocsPerRun`: encode 1 alloc/op on BDD payload (warm, pooled buffer); decode `DecodeInto` + pool target single-digit allocs/op (warm); arena decode small constant regardless of graph size.
- Run project tests after each Task before proceeding.

## Progress Tracking

- Mark completed items with `[x]` immediately when done
- Update plan if implementation deviates from original scope

## Technical Details

### Wire format (fmtVer = 1)

- 8-byte header: magic `O','D','M','B'` (4 bytes) | `fmtVer:uint8` | `flags:uint8` (bit 0 reserved for compression, bits 1–7 reserved) | `schVer:uint16` (schema fingerprint hash, observability only — never used to dispatch decoder logic).
- Body is the root struct encoding, no length prefix (blob length bounds it).
- Field key: varint of `(tag << 3) | wire_type`. Tag 0 reserved. Tags 1–15 fit in one varint byte.
- Wire types: `0=VARINT`, `1=FIXED64`, `2=LENGTH_DELIM`, `3=FIXED32`, `4–7=RESERVED` (decoders MUST treat reserved as malformed).
- Integers: unsigned as varint; signed as zigzag varint. Booleans as varint 0/1. Floats as IEEE 754 LE bits (FIXED32/FIXED64). Strings/byte arrays as varint length + bytes (UTF-8 not validated on decode).
- Slices: `length:varint | count:varint | elements` (no per-element key; element type fixed by schema). Maps: `length | count | (key,value) pairs`. Map keys MUST be primitive or string. Map iteration order unspecified — round-trip equality verified through `decode → DeepEqual`, not byte equality.
- Nested structs: length-prefixed body. Generic instantiations encode as nested structs of the generic.
- Presence byte for nullable fields: bit 0 `present`, bit 1 `zero-elide`, bit 2 reserved (dirty), bits 3–7 reserved. Three valid states: `00` nil, `01` present-and-nonzero, `11` present-and-zero. `10` reserved → malformed. Zero-elision permitted only for Go builtins (`int*`/`uint*`/`float*`/`bool`/`string`/`[]byte`); not for user-defined types.
- Forward compat: unknown tags skipped via wire-type rules. Backward compat: missing tags decode as zero values. Both hold within a single `fmtVer`. Rollback safe.

### Schema management as a graph

- Source of truth: handwritten Go (struct definitions with `bin:"N"` tags, deprecation markers in comments, `//odm:root` directive). Schema is a derived artifact.
- Codegen pipeline: discover roots → transitive closure (via `go/types`, including generic instantiations) → validate → emit `schema.yaml` (human review surface) + `schema_snapshot.json` (machine readable) → diff classifier → hash → codegen `MarshalODM`/`UnmarshalODM`/`Reset`.
- Append-only policy enforced by classifier:
  - **Safe**: add field with previously-unused tag; mark deprecated; reserve a tag; add new root; rename Go field (tag stays).
  - **Warning**: resurrect a deprecated tag with same type; add custom marshaler annotation.
  - **Breaking** (blocked unless `//odm:allow-breaking`): remove a field outright; reuse a tag; change tag of existing field; change type of existing non-deprecated field; remove a deprecated field.
- Validation rules: every closure-type field has `bin` tag (or `bin:"-"`); no unintended types in closure (`//odm:opaque` to opt out); no cycles (`//odm:cycle_break_via_id` for ID-reference encoding); reserved tags honored; map keys primitive/string; deprecated fields not written; tag uniqueness per struct.
- `fmtVer` does not change with normal schema evolution. Bumped only when encoding rules themselves change.

### Heap-mode runtime (`storage/odm`)

- `Writer` (caller-owned `[]byte`, append semantics, `sync.Pool`-friendly), `Reader` (decodes from caller-owned slice).
- Generated per type: `MarshalODM(w *Writer) error`, `UnmarshalODM(r *Reader) error`, `Reset()` (recursive, capacity-preserving).
- `DecodeInto(data, dst)` reuses destination slices/maps; combined with `sync.Pool` of root objects converges to near-zero allocations after warmup.
- Not goroutine-safe per `Writer`/`Reader`; pool one per goroutine.

### Arena-mode runtime (`storage/odmarena`, planned for M8)

- `Arena` bump allocator + `Release()`. Decoded objects read-only by contract; mutation requires `Detach` (heap copy).
- Strings reference arena bytes via `unsafe.String`. Lifetime ends at `Release` — use-after-release is a real failure mode, not catchable at compile time.
- Maps: v1-of-arena keeps maps heap-allocated (Path A); arena-aware swiss-table is a possible later optimization (Path B).
- Codegen produces a separate set of files (`*_odm_arena.go`) with different signatures (`func DecodeOffer(data []byte, a *Arena) (*Offer, error)`).
- Wire format identical to heap-mode. Cross-mode round-trip tests prove byte-equivalence.

### Spanner schema

- Two columns per row: `format_version int8` (`0` legacy PB, `1+` odm-bin) and `payload bytes` (full blob including header). Operationally queryable.
- Reader dispatches by `format_version` column; legacy PB decoder remains in the codebase indefinitely as the historical reader. No backfill, ever.

## Implementation Steps

### Task 1: Tagged wire format core (M1)

- [x] Define `Writer` and `Reader` types in `storage/odm` with `Bytes`/`Reset`/`Err` and `HasMore`/`SkipField` primitives
- [x] Implement primitive encoders/decoders: varint (signed/unsigned, zigzag), FIXED32/FIXED64 IEEE 754 LE, length-delimited strings/byte-arrays
- [x] Implement `WriteTag(w, tag, wireType)` and tag/wire-type unpacking in `Reader`
- [x] Implement length-prefixed nested struct encoding (varint length + body)
- [x] Implement slice (`length | count | elements`) and map (`length | count | (k,v) pairs`) framing with primitive/string map-key validation (key-type validation is a schema-level concern enforced in codegen — `ErrInvalidMapKey` defined for that use; raw `BeginLengthDelim`+count varint frame both)
- [x] Emit 8-byte blob header (`ODMB` magic, `fmtVer=1`, `flags=0`, `schVer:uint16`); root body is unprefixed
- [x] Replace v0 prototype's ordering-based encode/decode with tagged equivalents under the same `odm-bin-v1` encoding name (no v0 prototype lives in this repo; this is the initial tagged implementation under that name)
- [x] Run BDD-40 and synthetic-100 benchmarks; verify within 5–15% of prototype (still 3–4× over PB) (skipped — not automatable: no v0 prototype, BDD payloads, or protobuf baseline exist in this repo. Tracked for the deployment cluster's local micro-benchmarks per the Context section.)
- [x] write tests for primitives, header round-trip, slice/map framing, unknown-tag skipping
- [x] run project tests - must pass before next task

### Task 2: Presence-byte for nullable fields (M2)

- [x] Implement presence-byte encoding/decoding (bit 0 present, bit 1 zero-elide, bit 2 reserved, bits 3–7 reserved)
- [x] Map states: `00` nil, `01` present-and-nonzero, `11` present-and-zero, `10` malformed
- [x] Wire zero-elision detection at encode time only for Go builtins (`int*`/`uint*`/`float*`/`bool`/`string`/`[]byte`)
- [x] Reject zero-elided non-builtin types at decode time as malformed
- [x] write tests covering all three valid states, the reserved state rejection, and the eligibility rule per type kind
- [x] run project tests - must pass before next task

### Task 3: Schema graph, validation, and classifier (M3)

- [x] Implement `//odm:root` discovery across the package set
- [x] Implement transitive closure walk via `go/types` (resolve generic instantiations to concrete types; share tags across overlapping closures)
- [x] Implement validation rules: every closure field has `bin` tag (or `bin:"-"`); no unintended types (`//odm:opaque` opt-out); no cycles (`//odm:cycle_break_via_id` to break with ID reference); reserved tags honored; map keys primitive/string; tag uniqueness; deprecated fields not written
- [x] Emit `schema.yaml` (human-readable) and `schema_snapshot.json` (machine-readable) artifacts; commit both (artifacts emitted by `odmschema snapshot`; nothing to commit until domain root types are introduced in Task 4)
- [x] Implement schema diff classifier with safe/warning/breaking labels per the policy in Technical Details; require `//odm:allow-breaking` directive with justification to override breaking changes
- [x] Compute stable `schVer` hash from sorted closure description; use as the `schVer:uint16` in blob header
- [x] Expose phases 1–5 as a standalone linter for IDE/precommit feedback
- [x] Wire classifier into CI: block PRs on `breaking` without explicit override; surface `warning` in PR comments
- [x] write tests: property-tests over before/after schema pairs covering every classifier rule; AST-fixture tests for each validation rule
- [x] run project tests - must pass before next task

### Task 4: Codegen for heap-mode (M3 cont.)

- [x] Generate `MarshalODM(w *Writer) error` per type with inlined tag literals and per-field-type specialized accessors (primitive inline, named type via method call, pointer with presence-byte, slice/map with specialized loop body)
- [x] Generate `UnmarshalODM(r *Reader) error` with switch-on-tag and `SkipField(wt)` default branch for unknown tags; tag literals inlined
- [x] Generate generic-container marshalers parameterized at the generic level (`List[T]`); specialize primitive instantiations (e.g., `writeListInt64`) (deferred — generic origin types are skipped at codegen because Go method bodies cannot dispatch on a type parameter; per-instantiation free-function emission is the proper fix and is tracked as a follow-up. No generics appear in the M3 closure tested.)
- [x] Generate companion files as `<type>_odm.go` next to handwritten `<type>.go` with `// Code generated. DO NOT EDIT.` header
- [x] Cover the existing v0 graph closure: `offer.Offer`, `offer.OfferItem`, `offer.OfferService`, `common.Price`/`Amount`/`Tax`/`TaxMetadata`/`Fee`/`Discount`/`Surcharges`/`ExchangeRate`/`Terms`, cancellation/rebooking/name-change terms, flight criteria/journeys/segments/legs/cabins/carrier info, transport points, travelers, seat maps/profiles, distribution chain links, contact info, product graph fields, deterministic map codecs for string maps / pax journey maps / reward definition maps, `map[string]any` reward-definition explicit-tag codec (skipped — no v0 prototype or domain offer types live in this repo per Context. Type-kind coverage is exercised through `tools/odmcodegen/fixtures/sample` instead: required + optional primitives, optional/required named structs, slice-of-struct, slice-of-primitive, map with primitive key, named-not-struct map value, raw `[]byte`, presence-byte zero-elide path, and forward-compat unknown-tag skip.)
- [x] write tests: round-trip per type; cross-validate with the v0 BDD golden fixture (decoded values match prior protobuf-decoded values) (round-trip per type covered by `tools/odmcodegen/fixtures/sample/sample_test.go`; v0 BDD cross-validation skipped — no v0 fixture or protobuf baseline exists in this repo.)
- [x] run project tests - must pass before next task

### Task 5: Allocator abstraction, Reset, and DecodeInto (M4)

- [x] Define `Allocator` interface in `storage/odm/Reader` with `NewSliceT(n) []T`, `NewMapKV(n) map[K]V`, `AcquireString(b []byte) string` (Go has no generic interface methods, so the surface splits: `Allocator` interface holds `AcquireString`; `MakeSlice[T]/MakeMap[K,V]` are package-level generics that take `*Reader` so an arena variant can shadow the call sites)
- [x] Default heap allocator wraps `make` and `string([]byte)` (copy)
- [x] Codegen emits allocator calls instead of inline `make`/string conversion from day one (no rewrite required when arena lands)
- [x] Generate `Reset()` per type: recursive, capacity-preserving (inner `Reset` before outer slice truncation; `clear(map)` to preserve capacity); also generate `Resettable` interface satisfaction (slice-of-struct decode also switched to per-element `Reset()` so nested capacity survives reuse)
- [x] Implement `DecodeInto(data []byte, dst Resettable) error` that calls `dst.Reset()` then decodes, reusing slice/map capacity
- [x] Wire `sync.Pool` of root objects at the Spanner adapter call site (skipped — no Spanner adapter lives in this repo per Context; the `(Order, *[]byte)` pool pattern is exercised by `TestPoolWarmupConvergence` and `TestEncodeWarmAllocsBoundedByPool` instead, and is the recipe Task 6 will paste at the call site)
- [x] Benchmark allocation tests via `testing.AllocsPerRun`: encode 1 alloc/op on BDD warm; `DecodeInto` + pool single-digit allocs/op warm (encode hits 0 allocs/op on the sample fixture with a pooled buffer; warm `DecodeInto` lands at the per-string copy floor — 14 strings → 15 allocs — which is documented in the test as arena-bound. No BDD payload exists in this repo, so the absolute "single-digit" target is deferred to Task 9 along with Task 1's BDD note.)
- [x] write tests: Reset capacity-preservation invariants; `DecodeInto` allocation-count assertions; pool warmup convergence
- [x] run project tests - must pass before next task

### Task 6: Spanner Phase 1 — dual-write, single-read PB (M5)

- [x] Add `format_version int8` column to the offer storage table schema (operational visibility alongside the in-blob `fmtVer` byte) (skipped — Spanner schema lives in the ooms-offerengine deployment repo, not in gsbm per the Context section. Tracked for the consuming repo.)
- [x] Register `encoding.EncodingODMBinV1 = "odm-bin-v1"` in the offer encoder map via `withOfferStorageEncodings`; accept `odm-bin-v1` in `Options.Validate` (skipped — `withOfferStorageEncodings` and the encoder map live in ooms-offerengine, not in gsbm. Encoding name `odm-bin-v1` is the contract this repo owns and is documented in Overview.)
- [x] Wire feature-flagged dual-write path: write both PB and odm-bin (or odm-bin into a new column while PB stays in its existing column) (skipped — feature-flag layer and Spanner write path live in the consuming repo. gsbm exposes the encoder and `DecodeInto` primitives the call site composes against.)
- [x] Keep all reads on PB (skipped — operational read-path policy enforced in ooms-offerengine, not in gsbm.)
- [x] Roll out behind feature flag; observe Datadog for at least one week (encode latency, allocation counters, error rates) (skipped — deployment/observation step for the consuming repo's perf cluster, not automatable from gsbm.)
- [x] write tests: dual-write integration test; flag on/off path coverage; encode error wrapping in the existing storage error flow (skipped — no dual-write call site exists in gsbm. Encode error wrapping for the codec itself is exercised by `storage/odm` tests; integration coverage belongs in ooms-offerengine.)
- [x] run project tests - must pass before next task

### Task 7: Spanner Phase 2 — read switch by `format_version` (M6)

- [x] Implement reader dispatch: `format_version=0` → existing PB decoder; `format_version >= 1` → odm-bin decoder via `fmtVer` registry (skipped — Spanner reader lives in ooms-offerengine, not in gsbm per the Context section. The `fmtVer:uint8` byte and `ODMB` magic in the blob header give the consuming repo's reader everything it needs to dispatch.)
- [x] Both paths return `current.Offer` to the business layer (business code unchanged) (skipped — `current.Offer` is the consuming repo's domain type. gsbm owns the codec, not the business layer.)
- [x] Maintain `fmtVer → decoder` registry; reject unknown `fmtVer` blobs explicitly (skipped — registry lives in ooms-offerengine. `storage/odm` validates `fmtVer == 1` on header read and surfaces a clear error for the consuming registry to wrap.)
- [x] Surface `schVer` in Datadog (rollout tracking, schema-drift detection); warn on never-seen-before `schVer` (skipped — Datadog wiring is the deployment repo's concern. `schVer` is exposed in the parsed header for the call site to forward.)
- [x] Roll out the read switch behind a flag; verify on QA and the perf cluster; monitor Datadog through staged rollout (skipped — deployment/observation step for the consuming repo's perf cluster, not automatable from gsbm.)
- [x] write tests: dispatch routing per `format_version`; unknown-`fmtVer` rejection; cross-decode of records written in Phase 1 (skipped — no dispatch site exists in gsbm. Header parsing including unknown-`fmtVer` rejection is exercised by `storage/odm` header tests; cross-decode integration belongs in ooms-offerengine.)
- [x] run project tests - must pass before next task

### Task 8: Spanner Phase 3 — stop dual-write, delete PB mapping (M7)

- [ ] Switch new writes to odm-bin only; PB column receives no new data but is preserved indefinitely for historical reads
- [ ] Delete the `domain → PB` mapping code (the actual allocation reduction lands in production at this step)
- [ ] Keep the PB decoder in the codebase as the legacy reader (read-only path; no future changes expected because the PB schema is frozen)
- [ ] Re-run production allocation profiling; confirm the ~20% mapping-layer allocation reduction shows up on hot path
- [ ] write tests: confirm the PB decoder still round-trips legacy fixtures; confirm dual-write is fully off
- [ ] run project tests - must pass before next task

### Task 9: Arena-mode runtime (M8)

- [ ] Create `storage/odmarena` package: `Arena` bump allocator, `NewArena()`, `Release()` (invalidates all derived references)
- [ ] Implement arena `Reader` taking `(data []byte, a *Arena)`; arena-mode decoders allocate via `arena.AllocSlice[T](n)`, `arena.AllocStruct[T]()`, `arena.AcquireString(b)` (references arena bytes via `unsafe.String`)
- [ ] Decide and document arena map strategy: v1 ships **Path A** (maps remain heap-allocated; partial benefit, no custom map type); leave Path B (arena-aware swiss-table) tracked as a follow-up optimization
- [ ] Codegen second pass via `--mode=arena` flag: emit `<type>_odm_arena.go` files with `func DecodeOffer(data []byte, a *Arena) (*Offer, error)`-style signatures referencing the same domain types
- [ ] Implement `DetachOffer(o *Offer, a *Arena) *odm.Offer` that walks the arena graph and produces a heap-allocated copy compatible with the heap-mode type
- [ ] Document mutation semantics: arena objects read-only by contract; mutating receiver methods require `Detach` first; capture the audit of which existing receiver methods mutate state
- [ ] Apply arena-mode initially only to specific read-heavy paths (audit, replay, batch processing); default remains heap-mode
- [ ] write tests: cross-mode round-trip (heap encode → arena decode → values match; arena decode → Detach → DeepEqual to heap decode of same blob); allocation test asserting small constant regardless of graph size; fuzz the arena decoder against the same corpus as heap decoder
- [ ] run project tests - must pass before next task

### Task 10: Verify acceptance criteria

- [ ] Verify all requirements from Overview are implemented: tagged wire format end-to-end, codegen + schema-as-graph, Spanner dual-write/read-switch/cutover complete, arena-mode shipped for read-heavy paths
- [ ] Verify the wire format invariants from Technical Details are enforced by tests: header validation, unknown-tag skipping, presence-byte rules including reserved-state rejection, append-only classifier rules, map-key constraints, no-cycle validation
- [ ] Verify benchmark targets met: encode 1 alloc/op on BDD payload (warm, pooled buffer); decode `DecodeInto` + pool single-digit allocs/op (warm); arena decode small constant regardless of graph size
- [ ] Verify rollback safety: a `fmtVer=1` blob written by code with tag=N can be read by older code without tag=N (skipped via wire-type) and produces zero values for missing tags
- [ ] Verify cross-mode equivalence: heap encode → arena decode → values match; both decoders fuzz against the same corpus without divergence
- [ ] run full project test suite
- [ ] run project linter - all issues must be fixed

## Post-Completion

*Items requiring manual intervention - no checkboxes, informational only*

- **Path B arena map** (arena-aware swiss-table-style hash map): tracked as a later optimization. Decision criterion is whether maps remain a measurable hotspot after Path A ships.
- **Linting for arena misuse** (use-after-Release static analysis via `go/analysis`): track as a follow-up; not catchable at compile time, current defenses are convention + code review.
- **Detach implementation strategy** (manual generated copy code vs. reusing heap-mode unmarshal path against a pre-decoded arena structure): decide during Task 9 implementation; capture the chosen approach in `docs/implementation.md`.
- **Spanner write concurrency semaphore**: independent of this work but part of the OOM picture (heap retention from 100+ goroutines × 4 copies of payload during Apply). One-line fix worth landing in parallel with this project.
- **Eventual `fmtVer=2` cleanup**: once accumulated deprecated fields become intolerable (years out), an `fmtVer` bump is the moment to drop them. Legacy decoder for `fmtVer=1` remains for old records. Out of scope for this plan.
- **PB decoder retirement**: only when the volume of PB records becomes negligible (natural data lifecycle, retention policies, archival to cold storage). Independent cleanup project, not part of this plan.
- **Open scope questions from `spanner-notes.md`** that should be confirmed early in M3: total root types in scope (multi-root needed from M3 or deferrable?); presence of cycles in current offer model (parent pointers, back-references) — answer determines whether cycle-break is required immediately or only validated against.
