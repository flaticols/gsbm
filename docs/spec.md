# gsbm Wire Format Specification

**Version:** fmtVer = 2
**Status:** Draft
**Audience:** Anyone implementing an encoder or decoder for this format, in any language.

This document describes the byte layout only. It does not describe Go API, codegen structure, or implementation choices. Two different runtime implementations (heap-mode and arena-mode) share this exact wire format.

---

## 1. Conventions

- All multi-byte integers are little-endian unless explicitly noted otherwise.
- "varint" refers to LEB128-style unsigned variable-length integer encoding (compatible with protobuf varints): seven bits of payload per byte, MSB set on continuation, MSB clear on the last byte.
- "byte" means an unsigned 8-bit value.
- Field offsets in diagrams are byte offsets from the start of the enclosing structure.
- Encoders MUST write reserved fields and bits as zero. Decoders MUST reject reserved bits whose interpretation could change payload semantics — header `flags`, presence-byte reserved bits, and reserved wire types fall in this category for `fmtVer = 2`. Decoders MAY ignore reserved bits only where a future extension is known not to affect interpretation of any currently-defined field.

## 2. Blob structure

A serialized record (a "blob") consists of a 12-byte header followed by a body.

```
+--------+--------+--------+--------+
|         magic  (4 bytes)          |   offsets 0..3
+--------+--------+--------+--------+
| fmtVer | flags  |   schemaHint    |   offsets 4..7
+--------+--------+--------+--------+
|          bodyLen  (uint32 LE)     |   offsets 8..11
+--------+--------+--------+--------+
|             body                  |   offsets 12..end
+-----------------------------------+
```

### 2.1 Header fields

| Offset | Size | Name    | Type    | Description |
|--------|------|---------|---------|-------------|
| 0      | 4    | magic   | bytes   | ASCII `'G','S','B','M'` (0x47, 0x53, 0x42, 0x4D). |
| 4      | 1    | fmtVer  | uint8   | Wire format version. Currently `2`. |
| 5      | 1    | flags   | uint8   | Bitfield. Bit 0 reserved for future built-in compression marker. Bits 1-7 reserved. |
| 6      | 2    | schemaHint | uint16  | Weak schema-grouping hint computed by the writer's schema closure. Not unique. Not used to dispatch a decoder. Suitable for telemetry grouping; not suitable for drift detection. |
| 8      | 4    | bodyLen | uint32 LE | Byte count of the body that follows the header. MUST equal `len(blob) - 12`. Caps body at 4 GiB - 1. |

A decoder MUST verify magic and reject blobs whose magic does not match. A decoder MUST verify fmtVer matches a version it implements; if not, it MUST reject the blob. A decoder MUST NOT branch decode logic on schemaHint for the same fmtVer — schemaHint is informational.

For `fmtVer = 2`, decoders MUST reject blobs with any non-zero `flags` bit. No flag semantics are defined yet; a future encoder that sets bit 0 to indicate body compression would silently corrupt an old reader that ignored the flag. Encoders MUST write `flags = 0`.

A decoder MUST verify `bodyLen == len(blob) - 12` (the storage-layer byte count is authoritative; the in-header value must agree) and reject the blob as malformed on mismatch. This cross-check defends against truncation and against a writer that emitted the wrong size.

### 2.2 Body

The body is the encoding of a single root struct. It begins immediately after the 12-byte header and continues to the end of the blob. Its byte count is carried both in the header's `bodyLen` field and by the storage-layer length; the two MUST agree.

## 3. Field encoding

Inside any struct (root or nested), fields are encoded as a sequence of `(key, value)` pairs in arbitrary order. There is no terminator at the struct level when length is bounded externally (root: by blob length; nested: by length-prefix, see §5.4).

### 3.1 Field key

Each field begins with a varint-encoded key:

```
key = (tag << 3) | wire_type
```

- `tag` is a non-zero unsigned integer up to 2^29 - 1, identifying the field within its struct.
- `wire_type` occupies the low 3 bits.

Tag `0` is reserved and MUST NOT be used by encoders for real fields. A decoder encountering tag `0` MUST treat the blob as malformed. A decoder MUST also reject keys whose decoded `tag` exceeds `2^29 - 1` (the field-key encoding cannot represent larger tags) and keys whose varint overflows `uint64`.

### 3.2 Wire types

| Code | Name          | Used for |
|------|---------------|----------|
| 0    | VARINT        | Variable-length unsigned/signed integers, booleans, enums. |
| 1    | FIXED64       | 8 bytes, fixed length. Used for `float64`, optionally for fixed-width 64-bit integers when chosen by the schema. |
| 2    | LENGTH_DELIM  | Length-prefixed payload: strings, byte arrays, nested structs, slices, maps. |
| 3    | FIXED32       | 4 bytes, fixed length. Used for `float32`. |
| 4    | RESERVED      | Future use (extension types). Decoders MUST treat as malformed. |
| 5-7  | RESERVED      | Future use. Decoders MUST treat as malformed. |

A decoder encountering an unknown tag MUST skip the field by reading the value according to its wire type:
- VARINT: read and discard one varint.
- FIXED64: skip 8 bytes.
- LENGTH_DELIM: read varint length L, skip L bytes.
- FIXED32: skip 4 bytes.

This allows old decoders to skip fields added by newer encoders without knowing the schema.

For known tags, decoders MUST verify that the incoming wire type matches the schema-declared wire type for that field. A mismatch on a known tag is malformed; decoders MUST NOT skip-and-continue past it. Skipping past a known-tag mismatch can desync the parser — for example, a tag declared `LENGTH_DELIM` but written with `VARINT` would cause `SkipField(VARINT)` to consume only the next varint and then read the remaining bytes of the would-be payload as the next field key.

Every length-delimited read is bounded by its enclosing region: the root body is bounded by the blob length supplied by the storage layer, and any nested LENGTH_DELIM value is bounded by its own length prefix. Decoders MUST reject any length-delimited value whose declared length exceeds the remaining bytes of the current bounded region. (See `tools/gsbmcodegen/fixtures/graph` for round-trip evidence: `TestCatalogLengthBoundedRegionOverflow` exercises this rejection path against generated decoder code.)

### 3.3 Duplicate fields and duplicate map keys

If the same field tag appears more than once within a struct body, the **last** value wins; for slices and maps, the entire field value is replaced by the most recent occurrence. If a map payload contains the same key more than once, the **last** entry wins. Decoders MAY offer a strict mode that rejects duplicates, but the default behaviour is last-wins so generated decoders do not need to track per-tag or per-key seen-bitmaps. (See `tools/gsbmcodegen/fixtures/graph` for round-trip evidence: `TestCatalogDuplicateTagLastWins` and `TestCatalogDuplicateMapKeyLastWins` exercise both sub-rules against generated decoder code. The fuzz harness `FuzzWriterReaderRoundTripCanonical` in `storage/gsbm/fuzz_test.go` exercises the same invariant on arbitrary inputs by checking that re-decoding a re-encoded blob converges, which holds under last-wins but would diverge if duplicates were merged or order-dependent.)

### 3.4 Anonymous embedded struct flattening

When a struct embeds another named struct anonymously (Go syntax: `type Outer struct { Inner; ... }`), the embedded type's `bin:`-tagged fields are **flattened** into the outer struct's tag space. The wire format is byte-identical to a hand-written `Outer` that declared each of `Inner`'s fields directly with the same tags.

- Tag uniqueness (§3.1) is enforced across the embed boundary. If `Outer` and `Inner` both declare a field with the same tag, the validator emits `field/tag-collision` naming both fields and the embed; the schema is rejected.
- Flattening is recursive: if `Inner` itself embeds `Innermost`, all three layers' tagged fields appear in `Outer`'s flattened tag space, and uniqueness applies across all of them.
- Pointer embedding (`type Outer struct { *Inner; ... }`) is permitted and flattens the same way. On encode, if the embedded pointer is nil, all flattened fields are skipped entirely — the wire image carries none of `Inner`'s tags. (A non-nil pointer embed encodes the same way as a value embed: every tag is emitted, even when the underlying field is zero.) On decode, the embedded struct is lazily allocated on the first incoming tag belonging to it; if no such tag arrives, the pointer stays nil. This matches the "tag not present" rule from §7.2.
- Non-struct anonymous embedding (e.g., embedding a named primitive) is rejected with diagnostic `field/anonymous-non-struct`. Only struct embeds flatten.
- The snapshot records `flattened_from = "<EmbeddedTypeName>"` on each flattened field entry; the classifier treats a tag that moves between a direct declaration and an embed-flattened declaration as `safe` so long as the tag, type, and wire shape are preserved.

## 4. Primitive value encoding

### 4.1 Integers (wire type VARINT)

Unsigned integers (`uint8`, `uint16`, `uint32`, `uint64`) are encoded as varint of their value.

Signed integers (`int8`, `int16`, `int32`, `int64`) are encoded as varint of their **zigzag-encoded** value:

```
zigzag_encode(n) = (n << 1) ^ (n >> 63)   // for int64; analogous for smaller widths
```

Zigzag avoids long varints for small negative numbers.

A decoder reading an integer field MUST treat the value as signed iff the schema declares the field as signed.

Decoders MUST reject decoded values that fall outside the schema-declared integer width (`uint8` ⇒ `0..255`, `int8` ⇒ `-128..127`, and so on for `uint16`, `int16`, `uint32`, `int32`). Range checks happen after zigzag decoding for signed types. Out-of-range values are malformed.

For `fmtVer = 2`, all integer fields use VARINT. The schema does not yet support a fixed-width integer encoding; FIXED64 is reserved for `float64` and FIXED32 for `float32`. A future fmtVer may opt fields into FIXED encoding via a schema annotation; switching an existing field between VARINT and FIXED is a breaking schema change.

Encoders MUST emit canonical shortest-form varints for both keys and values. A varint MUST NOT exceed 10 bytes (the maximum needed for a `uint64`). Decoders MUST reject varints that overflow `uint64`. Decoders SHOULD accept non-canonical (overlong) varints under the default mode but MAY reject them in strict mode. (The fuzz harness `FuzzWriterReaderRoundTripCanonical` in `storage/gsbm/fuzz_test.go` exercises canonical-form convergence on arbitrary inputs, including overlong-varint and non-canonical body forms.)

### 4.2 Booleans (wire type VARINT)

Encoded as varint `0` (false) or `1` (true). Decoders MUST treat any non-zero value as true; encoders SHOULD always emit exactly `0` or `1`.

### 4.3 Floats

`float32`: wire type FIXED32. Bits of IEEE 754 single-precision representation, little-endian.

`float64`: wire type FIXED64. Bits of IEEE 754 double-precision representation, little-endian.

NaN and infinities are preserved by exact bit-pattern round-trip.

Encoders MUST NOT zero-elide float fields. IEEE 754 distinguishes `+0.0` and `-0.0` by their sign bit, so decoding a zero-elided float would silently lose the sign and break exact bit-pattern round-trip. A decoder that reads a zero-elided float (presence-byte state `11`, see §5.1) MUST treat the blob as malformed.

### 4.4 Strings (wire type LENGTH_DELIM)

```
+--------+----- ... -----+
| length | UTF-8 bytes   |
+--------+----- ... -----+
```

`length` is a varint giving the number of bytes in the payload. Payload is the string bytes. Encoders SHOULD produce valid UTF-8; decoders are NOT required to validate UTF-8 and MAY return the bytes as-is.

### 4.5 Byte arrays (wire type LENGTH_DELIM)

Identical to strings, but the payload is opaque bytes. No UTF-8 implication.

## 5. Composite value encoding

### 5.1 Pointer / nullable values (presence-byte)

Fields declared as nullable in the schema (e.g., a Go `*T` pointer) are encoded with **wire type `LENGTH_DELIM` regardless of the underlying type**. The length-delimited payload contains a single presence byte followed by the optional value-only encoding of the underlying type:

```
+--------+----------+----- ... -----+
| length | presence | value-payload |
+--------+----------+----- ... -----+
```

- `length` (varint): byte count of the rest of the payload (presence byte plus the value payload, if any).
- `presence` (1 byte): see the bit table below.
- `value-payload`: the value-only encoding of the underlying type (omitted in the `nil` and `present-and-zero` states).

Wrapping every nullable field in LENGTH_DELIM guarantees that an unknown-tag decoder can `SkipField(LENGTH_DELIM)` over the entire nullable as a single unit, without knowing the underlying type or how many bytes the value would occupy. Older designs that placed the presence byte after the field key with the underlying wire type cannot be skipped safely — a `VARINT`-keyed nullable would consume the presence byte (one varint) and leave the real payload misaligned in the stream.

The presence byte carries:

| Bit | Name          | Meaning |
|-----|---------------|---------|
| 0   | present       | 0 = nil (no value follows). 1 = value is present. |
| 1   | zero-elided   | 1 = value is the zero value of its type; payload is omitted. 0 = payload follows. |
| 2   | dirty (reserved) | Reserved for future explicit-set tracking (partial-update use cases). MUST be 0. |
| 3-7 | reserved      | MUST be 0. |

Three states are used in fmtVer 2:

| Bits 0-1 | Meaning |
|----------|---------|
| `00`     | nil. No payload follows; the outer length covers only the presence byte. |
| `01`     | Present and non-zero. Value payload follows. |
| `11`     | Present and zero. No payload follows; decoder restores type's zero value. |
| `10`     | Reserved. Decoders MUST treat as malformed. |

Decoders MUST reject presence bytes whose reserved bits 2-7 are non-zero.

Zero-elision (state `11`) is permitted only for fields whose schema type is one of the Go builtin primitives `int*`, `uint*`, `bool`, `string`, or `[]byte`. **Floats are excluded** (see §4.3, `-0.0` would be lost). Encoders MUST NOT zero-elide named or user-defined types whose zero-value semantics are not stable across schema changes. Decoders that read a zero-elided value of an ineligible type MUST treat the blob as malformed.

### 5.2 Slices `[]T` (wire type LENGTH_DELIM)

```
+--------+--------+----- ... -----+
| length |  count |  elements     |
+--------+--------+----- ... -----+
```

- `length` (varint): total bytes following, covering count and elements.
- `count` (varint): number of elements.
- `elements`: `count` consecutive encodings of values of type T, with no per-element key. The encoding of T is determined by the schema and is the value-only encoding (no wire-type prefix; the slice declaration fixes T).

The redundant `length` enables skipping an unknown slice field without knowing T.

### 5.3 Maps `map[K]V` (wire type LENGTH_DELIM)

```
+--------+--------+----- ... -----+
| length |  count |  entries      |
+--------+--------+----- ... -----+
```

- `length` (varint): total bytes following.
- `count` (varint): number of entries.
- `entries`: `count` consecutive `(key, value)` pairs, where each key and each value is encoded as the value-only form for its respective type, with key first.

K MUST be one of: `string`, `bool`, signed integer (`int8`, `int16`, `int32`, `int64`, `int`), unsigned integer (`uint8`, `uint16`, `uint32`, `uint64`, `uint`, `uintptr`), or a named type whose underlying type is one of the above. K MUST NOT be a `float32`, `float64`, struct, slice, map, nullable, or `[]byte`. Float keys are excluded because IEEE 754 NaN compares unequal to itself, which makes float-keyed maps unreliable for long-lived storage. Decoders MAY reject blobs that violate this.

The order of entries is unspecified. Two encodings of the same map MAY differ byte-for-byte (Go map iteration order is non-deterministic). Decoders MUST NOT rely on any particular order. If byte-stable encoding is required by an implementation, it MAY sort keys before writing — this does not affect format compliance because readers do not require a particular order.

### 5.4 Nested structs (wire type LENGTH_DELIM)

A nested struct field is encoded as:

```
+--------+----- ... -----+
| length |  body         |
+--------+----- ... -----+
```

- `length` (varint): byte count of body.
- `body`: a sequence of `(key, value)` pairs as in §3.

The length prefix bounds the read and enables skipping unknown nested structs.

### 5.5 Generic instantiations

A schema generic type instantiated at concrete types (e.g., `List[Segment]`) is encoded as a regular nested struct of the generic, with its own field tags. The generic's fields use tags fixed by the generic declaration; the type parameters affect only the encoding of those fields' values, not the keys.

Example: `List[T]` declared with one field `items` at tag 1. An instantiation `List[Segment]` encodes as:

```
length-prefixed body: <key=1|LENGTH_DELIM> <slice-of-Segment encoding>
```

### 5.6 External package types

A schema closure MAY reference types from packages other than the package being analyzed only when one of the following holds:

1. The type is a primitive or a named type whose underlying type is a Go builtin primitive (e.g., `type Currency string` exported from another package).
2. The type already provides `MarshalGSBM(w *gsbm.Writer) error` and `UnmarshalGSBM(r *gsbm.Reader) error` methods that conform to this spec.
3. The field is tagged `bin:"N,custom=CodecName"` and `CodecName` is registered with the codegen (see §5.8).

Any other reachable external type (e.g., `time.Time`, `decimal.Decimal`, `uuid.UUID` without a registered codec) MUST cause schema validation to fail. Codegen does not silently expand external closures because the resulting wire shape would be coupled to a third-party type definition the schema owner cannot freeze.

### 5.7 Cycle-break ID references

A schema may form a closure cycle through a self-referential or mutually-recursive struct (e.g., a linked-list `Item.Previous *Item`). Such a cycle has no finite serialization as nested struct bodies. To break the cycle, the schema author annotates one field along the cycle as an **ID reference**: instead of encoding the referenced struct's full body, the encoder writes only the referenced struct's identifier field as a leaf scalar. Decoders surface the ID and leave hydration of the referenced struct to the caller.

The annotation lives at the field declaration in either of two equivalent forms:

- The `id_ref` tag option: `bin:"N,id_ref"`. Preferred — colocated with the wire tag where reviewers look for wire-format choices.
- The `//gsbm:cycle_break_via_id` comment marker on the field. Legacy form; still supported.

Both forms set the same schema-level flag and produce the same wire encoding.

**Field-type constraint.** The annotated field MUST be a pointer to a named struct type. Annotating a non-pointer-to-struct field is rejected at schema validation (`tag/bad-id-ref`).

**Target ID convention.** The referenced struct MUST declare its identifier field at `bin:"1"`. The codegen errors with `idref/missing-id-tag` if the target has no field at tag 1. The wire type of the ID-reference field is taken from the target's tag-1 field:

- If the target's tag-1 field is an integer, the cycle-break field has wire type `VARINT`.
- If the target's tag-1 field is a string or `[]byte`, the cycle-break field has wire type `LENGTH_DELIM`.

**Wire shape.** A cycle-break field is encoded as a single leaf scalar — the value of the referenced struct's tag-1 field — using the wire type above. No length-delimited nested struct body is written. Concretely, the field key carries the cycle-break field's tag and the target's tag-1 wire type, followed immediately by the ID value:

```
<key=N | wire_type_of(target.bin:"1")> <ID value>
```

A `nil` pointer at this field is encoded by omitting the field's key from the body entirely; there is no presence-byte form for a cycle-break field. The decoder restores `nil` by the absence of the tag in the wire stream, matching how a missing tag decodes to the zero value (a `nil` pointer plus a zero-valued ID).

**Skip-safety.** Because the on-wire shape is a single primitive (VARINT or LENGTH_DELIM), an unknown-tag decoder can `SkipField` over a cycle-break field using the standard wire-type rules from §3.2. No special handling is required.

**Round-trip.** A decoded cycle-break field is materialized as a pointer to a partially-populated instance of the target struct — only the tag-1 (ID) field is set; all other fields are zero. The caller is responsible for hydrating the reference (e.g., by looking the ID up in an index or store) before observing the other fields. Re-encoding the decoded value reproduces the original wire bytes byte-for-byte, because the encoder reads only the target's tag-1 field.

**Classifier.** Toggling the cycle-break flag on an active field is a wire-affecting `breaking` change in both directions: switching from inline encoding to ID reference (or back) replaces the field's body shape (nested struct body ↔ leaf scalar) under the same tag, which old and new readers cannot interop across.

### 5.8 Custom codecs

A field MAY opt out of schema-driven encoding by tagging it `bin:"N,custom=CodecName"`. The codec is a small set of plain Go functions — `DecodeFn` plus one of `(SizeFn, EncodeFn)`, `EmitFn`, or `StreamFn`; see Registration below — registered with the codegen at generation time. The schema does not descend into the field's Go type, so external types (`time.Time`, `decimal.Decimal`, third-party UUIDs, etc.) can be encoded without their internal layout becoming part of the wire contract.

**Wire shape.** A custom-codec field is encoded exactly like any other field with the wire type the codec declares (VARINT, LENGTH_DELIM, FIXED32, FIXED64). The field key is the standard §3.1 key carrying that wire type, followed immediately by the codec's payload. No envelope, prefix, or marker distinguishes a custom-codec field from a primitive field on the wire — only the schema knows the difference. An unknown-tag decoder skips a custom-codec field using the standard §3.2 wire-type rules.

**Nullable custom codecs.** A field of type `*T` tagged `custom=CodecName` is wrapped in the §5.1 nullable envelope: outer wire type LENGTH_DELIM, one-byte presence header, then (for `PresenceNonZero`) the codec's payload bytes. The codec body itself runs only on `PresenceNonZero`; `PresenceNil` is encoded with an empty payload, and a non-nil pointer to a zero codec value still serializes as `PresenceNonZero` with whatever bytes the codec produces. `PresenceZero` is reserved for the schema-traversal path and is rejected by `ReadPresenceByte(false)` on a custom-codec field.

**Schema record.** The snapshot stores the codec name in the field entry's `custom` attribute. The classifier treats the name as part of the wire contract: adding `custom=` to a previously-untagged active field is wire-affecting; removing `custom=` from an active field is breaking; changing `custom=X` to `custom=Y` is breaking. The codes are `field/custom-added`, `field/custom-removed`, and `field/custom-changed` respectively. Transitions on deprecated or `compat_write` fields are shape-frozen and surface no diagnostic.

**Registration.** Codecs are registered at codegen time, not runtime. A registered codec MUST declare its body emission in exactly one of three mutually-exclusive shapes; in all shapes, codegen guarantees that the byte count reflected in the header `bodyLen` (§2.1) matches the body payload that follows.

- *Analytic* (`SizeFn` + `EncodeFn`, both set): the codec's body size is a pure function of `v` and is computed by `SizeFn(v)` (return type `int`) without writing the body. The emitter calls `SizeFn(v)` during sizing and `EncodeFn(w, v)` during writing; `SizeFn(v)` MUST return the exact byte count `EncodeFn(w, v)` writes for the same `v`. Suitable for fixed-width primitives and any codec whose width follows directly from `v`. `Time` (`SizeTime` / `EncodeTime`, LENGTH_DELIM) is the canonical example.
- *Materializing-cached* (`EmitFn` alone): the codec's body size depends on producing the body (e.g. a stringified decimal, short JSON, compression, canonicalization) and the body is small or medium enough that retaining it through the encode is acceptable. The emitter calls `EmitFn(w, v, callsite)` against a mode-aware Writer in both the sizing and writing passes; the Writer's per-call scratch cache, keyed by a codegen-emitted callsite id, makes the materialization run exactly once per `gsbm.Marshal` call. `DecimalString` (`EmitDecimalString`, LENGTH_DELIM) is the canonical example.
- *Streaming* (`StreamFn` alone): the codec's body size depends on producing the body, but the materialized form is too large to retain alongside the output buffer (e.g. ~100 MiB JSON, large compressed blobs). The emitter calls `StreamFn(w, v)` — no callsite, no cache — against a mode-aware Writer in both passes; the body is materialized twice (2× CPU) but never retained between passes (1× peak heap). The wire bytes the Writer emits are identical to what the analytic and materializing-cached shapes would produce for the same payload — the choice is a Go-side performance/contract concern, not a wire-format one. **The codec body MUST be deterministic across both passes**: a streaming codec whose materialization can differ run-to-run (e.g. `json.Marshal` of an unordered Go map) will write a `bodyLen` that disagrees with its body and break the wire format. This is enforced by author discipline rather than by the framework.

Declaring more than one of `(SizeFn, EncodeFn)`, `EmitFn`, and `StreamFn` on a single codec is rejected with `codec/conflicting-kinds`. Declaring none is rejected with `codec/missing-size-fn` (the historical name for the missing-size-pass diagnostic). The reference implementation ships `Time` (analytic, LENGTH_DELIM), a templated `DecimalString` (materializing-cached, LENGTH_DELIM), a templated `DecimalBinary` (analytic, LENGTH_DELIM), and `StreamingJSON` (streaming, LENGTH_DELIM); users register their own against the same registry. A field referencing an unregistered codec name causes codegen to fail with `codec/unregistered`, and the diagnostic lists every registered name to make typos obvious.

**Binary decimal codec wire shape.** The reference `DecimalBinary` codec encodes a decimal value as a LENGTH_DELIM body of exactly two canonical uvarints:

```text
body = uvarint(coef)                 // uint64 coefficient — the significant digits, 1-10 bytes
     ++ uvarint(scale<<1 | signbit)  // fractional-digit count with the sign in bit 0, ~1 byte
```

`coef` is the unsigned coefficient (a 19-digit decimal near the govalues maximum has `coef ≥ 2^63`, so it MUST be carried as `uint64`, not `int64`). The sign occupies bit 0 of the second uvarint — `1` for a negative value — and `scale` (the number of fractional digits) occupies the remaining high bits; a decoder recovers `scale = packed >> 1` and `neg = packed & 1`. The sign packs into `scale` rather than into `coef` because `coef << 1` overflows `uint64` for 19-digit coefficients, whereas `scale` is small. `scale` MUST be in `[0, 2^30)` — a cap that fits a 32-bit platform `int` (`scale` is recovered as a Go `int`) while staying far above any real decimal's fractional-digit count; a value outside that range does not round-trip the `<<1` packing and is rejected as malformed. The body carries no decimal-library identity — reconstruction of the concrete Go decimal type from `(coef, scale, neg)` is the binding code's responsibility and is not part of the wire contract. Because the body width is `SizeUvarint(coef) + SizeUvarint(packed)` — a pure function of the value — this codec is *analytic*, and encoding it allocates nothing.

### 5.9 Wire-width override for `int` fields

By default a Go `int` field encodes as a varint bounded to the int32 range — both encoder and decoder reject values outside `[MinInt32, MaxInt32]`. This portability default lets a 32-bit reader accept any blob a 64-bit writer produced. A schema author can override the default per field with the `type=` tag option:

```text
bin-tag      = field-num *( "," option )
option       = "deprecated"
             / "compat_write"
             / "id_ref"
             / "custom=" name
             / "type=" width
width        = "int32" / "int64"
```

Legal widths are exactly `int32` and `int64`. No other widths are accepted; an unknown width is rejected at parse time. The override is only valid on the Go `int` basic type — applying it to `int8`, `int16`, `int32`, `int64`, a named integer alias, a string, or any composite is rejected at schema-validation time with diagnostic `tag/type-width-mismatch`. The `type=` option MUST NOT be combined with `custom=` on the same field; the two are conceptually incompatible (custom routes the whole field) and the parser rejects the combination.

Wire shape for the three forms a Go `int` field can take:

| Go type | Tag option        | Encode                              | Decode                              |
|---------|-------------------|-------------------------------------|-------------------------------------|
| `int`   | (none)            | varint, bounded to int32 range      | varint, bounded to int32 range      |
| `int`   | `type=int32`      | varint, bounded to int32 range      | varint, bounded to int32 range      |
| `int`   | `type=int64`      | varint, full int64 range            | varint, full int64 range            |
| `int32` | (any `type=`)     | — rejected at schema build —        | —                                   |

The `type=int32` form is a no-op intent marker: it MUST emit and accept byte-identical output to today's un-annotated `int` field. It exists so a schema author can pin the current behavior explicitly and future-proof against any later change to the default.

The `type=int64` form widens the wire range. Encoder emits the bounds-check-free varint at int64 width; decoder accepts the full int64 range on a 64-bit host. On a 32-bit host the destination Go `int` cannot hold values outside `[MinInt32, MaxInt32]`, so the decoder guards the assignment with a platform-sized check (`math.MinInt`/`math.MaxInt`) and surfaces `ErrIntegerOverflow` rather than silently truncating — schema authors who reach for `type=int64` are opting out of 32-bit portability for that field, and a 32-bit reader rejects out-of-range values the same way it would reject them at the un-annotated default. The on-wire shape is identical to a Go `int64` field (§4.1) — only the schema records which form the author wrote.

**Cross-version compatibility.** A reader without `type=` support (an older deploy, or any third-party implementation that only knows the default `int` mapping) decoding a blob that carries a `type=int64` value greater than `MaxInt32` sees `ErrIntegerOverflow` and rejects the field. This is graceful rejection, not silent corruption — the old reader cannot decode the wider value, but it cannot misinterpret it either. Schema authors widening from un-annotated `int` (or `type=int32`) to `type=int64` MUST treat the change as a forward-compatible widening for new readers and a hard reject for old readers on out-of-range values; see [`docs/codecs/compatibility.md`](codecs/compatibility.md) for the operational sequencing.

**Schema fingerprint.** The schema snapshot records the override on each field that adopts it (`wireOverride: "int64"` or `"int32"`); flipping the override on, off, or between widths is visible in the snapshot diff and changes the fingerprint hash. The classifier surfaces the change like any other wire-affecting field annotation.

## 6. Root struct encoding

The body of a blob (offsets 12 onward) is the body of the root struct, NOT length-prefixed by an additional varint (the header's `bodyLen` field and the blob's external length both bound it; see §2.1). All other rules from §3-§5 apply.

The wire format does not encode the identity of the root struct type. The expected root type is supplied by the storage location, the calling code, or a decoder entrypoint (e.g., `gsbm.DecodeInto`). `schemaHint` is observability only and MUST NOT be used to dispatch a root-type decoder; two roots with disjoint shapes can collide on the same `schemaHint`.

## 7. Compatibility rules

### 7.1 Forward compatibility (old reader, new blob)

A reader at fmtVer 2 reading a blob written by a newer schema (more fields than the reader knows) MUST skip unknown tags using the wire-type rules in §3.2. All fields the reader knows are decoded as usual.

### 7.2 Backward compatibility (new reader, old blob)

A reader reading a blob written by an older schema (missing some fields the reader knows) leaves those fields at the type's zero value. No special action is required.

### 7.3 Cross-fmtVer

A reader at fmtVer N MUST refuse to decode a blob with fmtVer != N unless it explicitly implements multi-version dispatch. An implementation MAY register multiple decoders (one per supported fmtVer) and dispatch via the header.

The current wire version is `fmtVer = 2`. `fmtVer = 1` (the draft introduced in earlier prototypes; no production data exists at that version) is rejected outright by current decoders. Implementations updating from a fmtVer = 1 prototype regenerate codecs and re-emit data; there is no migration path on the wire.

### 7.4 Rollback

Both forward and backward compatibility hold within a single fmtVer. A deployment may be rolled back to a prior schema without re-encoding stored data — old code reading newly-written blobs skips unknown tags; new code reading old blobs sees zero values for missing tags.

The rule above protects the structural shape of stored blobs but does not, on its own, protect business semantics across a *replacement* migration where an old field is being phased out and a successor introduced at a different tag. If new code stops emitting the old tag the moment it begins emitting the new one, a rollback to the prior schema sees the old tag absent and decodes it as the type's zero value — operationally indistinguishable from data loss for any record written during the rollback window. Encoders participating in such a migration SHOULD continue emitting the old tag alongside the new one (a `compat_write` window) for at least the duration of the deployment's rollback window, and only stop emitting the old tag once the new schema has been baked long enough that a rollback is no longer a deployment option. This is a wire-level recommendation; the two tags are distinct so §3.3's duplicate-tag rule does not apply. The Go reference implementation expresses the window as a `compat_write` annotation on the deprecated field and gates exit from it behind an explicit operator acknowledgement at schema-diff time; alternative implementations are free to express it differently as long as the dual-write behavior is preserved on the wire.

## 8. Constraints summary for encoders and decoders

Encoders MUST:
- Emit a valid 12-byte header with correct magic, `fmtVer = 2`, `flags = 0`, and a `bodyLen` (uint32 LE) equal to the byte count of the body that follows.
- Use the field-key encoding from §3.1.
- Emit canonical shortest-form varints (≤ 10 bytes).
- Use varint for VARINT-typed values, IEEE 754 LE bits for floats, length-prefix for LENGTH_DELIM values.
- Apply zigzag encoding to signed integers.
- Wrap nullable fields in LENGTH_DELIM with the presence-byte payload from §5.1.
- Length-prefix all nested structs, slices, and maps.
- Tag uniquely within a struct.
- Emit every active non-nullable field of the schema, even when the value equals the Go zero value of its type. (Zero-elision is a nullable-only optimization, see §5.1.)

Encoders MUST NOT:
- Emit tag 0.
- Reuse a tag for a field of a different type (within or across schema versions).
- Zero-elide float fields or non-builtin types.
- Set non-zero `flags` bits in the header.
- Write deprecated fields, except when the field is annotated `compat_write` for the duration of a rollback bake window (see §7.4). This is a schema-level rule, not enforceable from the wire alone.

Decoders MUST:
- Verify magic and fmtVer; reject malformed.
- Reject blobs with non-zero `flags` bits (no flag semantics defined for fmtVer 2).
- Reject blobs whose header `bodyLen` does not equal `len(blob) - 12`.
- Reject keys with tag 0 or tag > `2^29 - 1`.
- Reject varints longer than 10 bytes or that overflow `uint64`.
- For known tags, reject blobs whose wire type does not match the schema-declared wire type.
- Reject length-delimited values whose declared length exceeds the remaining bytes of the enclosing region.
- For narrow integer types (`uint8`, `int8`, `uint16`, `int16`, `uint32`, `int32`), reject decoded values outside the schema-declared range.
- Skip unknown tags using wire-type rules.
- Treat missing tags as zero values for known fields.
- Reject reserved wire types (4-7) as malformed.
- Reject reserved presence-byte bits or states as malformed (see §5.1).

Decoders MAY:
- Validate UTF-8 in strings (not required).
- Reject map keys of disallowed types (recommended).
- Surface schemaHint for observability.
- Offer a strict mode that rejects duplicate tags, duplicate map keys, or non-canonical varints.

## 9. Worked example

A struct `Offer` with three fields:

| Tag | Name      | Type     | Wire type     |
|-----|-----------|----------|---------------|
| 1   | ID        | uint64   | VARINT        |
| 3   | Carrier   | string   | LENGTH_DELIM  |
| 7   | RetailCode| int64    | VARINT (zigzag) |

Encoding `Offer{ID: 42, Carrier: "AF", RetailCode: -1}`:

```
Header (12 bytes):
  47 53 42 4D       magic "GSBM"
  02                fmtVer = 2
  00                flags
  XX XX             schemaHint (some uint16)
  08 00 00 00       bodyLen = 8 (uint32 LE)

Body:
  08                key: (1<<3)|0 = 8     (tag=1, VARINT)
  2A                value: 42
  1A                key: (3<<3)|2 = 26    (tag=3, LENGTH_DELIM)
  02                length: 2
  41 46             "AF"
  38                key: (7<<3)|0 = 56    (tag=7, VARINT)
  01                value: zigzag(-1) = 1
```

Total 8 bytes for the body, 20 with the 12-byte header.

## 10. Versioning of this document

This is fmtVer = 2. Future revisions of the wire format will produce a new version of this document with explicit diffs. The fmtVer byte in the header is the authoritative version identifier; this document name should match.
