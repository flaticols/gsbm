# gsbm Wire Format Specification

**Version:** fmtVer = 1
**Status:** Draft
**Audience:** Anyone implementing an encoder or decoder for this format, in any language.

This document describes the byte layout only. It does not describe Go API, codegen structure, or implementation choices. Two different runtime implementations (heap-mode and arena-mode) share this exact wire format.

---

## 1. Conventions

- All multi-byte integers are little-endian unless explicitly noted otherwise.
- "varint" refers to LEB128-style unsigned variable-length integer encoding (compatible with protobuf varints): seven bits of payload per byte, MSB set on continuation, MSB clear on the last byte.
- "byte" means an unsigned 8-bit value.
- Field offsets in diagrams are byte offsets from the start of the enclosing structure.
- Encoders MUST write reserved fields and bits as zero. Decoders MUST reject reserved bits whose interpretation could change payload semantics — header `flags`, presence-byte reserved bits, and reserved wire types fall in this category for `fmtVer = 1`. Decoders MAY ignore reserved bits only where a future extension is known not to affect interpretation of any currently-defined field.

## 2. Blob structure

A serialized record (a "blob") consists of an 8-byte header followed by a body.

```
+--------+--------+--------+--------+
|         magic  (4 bytes)          |   offsets 0..3
+--------+--------+--------+--------+
| fmtVer | flags  |   schemaHint    |   offsets 4..7
+--------+--------+--------+--------+
|             body                  |   offsets 8..end
+-----------------------------------+
```

### 2.1 Header fields

| Offset | Size | Name    | Type    | Description |
|--------|------|---------|---------|-------------|
| 0      | 4    | magic   | bytes   | ASCII `'G','S','B','M'` (0x47, 0x53, 0x42, 0x4D). |
| 4      | 1    | fmtVer  | uint8   | Wire format version. Currently `1`. |
| 5      | 1    | flags   | uint8   | Bitfield. Bit 0 reserved for future built-in compression marker. Bits 1-7 reserved. |
| 6      | 2    | schemaHint | uint16  | Weak schema-grouping hint computed by the writer's schema closure. Not unique. Not used to dispatch a decoder. Suitable for telemetry grouping; not suitable for drift detection. |

A decoder MUST verify magic and reject blobs whose magic does not match. A decoder MUST verify fmtVer matches a version it implements; if not, it MUST reject the blob. A decoder MUST NOT branch decode logic on schemaHint for the same fmtVer — schemaHint is informational.

For `fmtVer = 1`, decoders MUST reject blobs with any non-zero `flags` bit. No flag semantics are defined yet; a future encoder that sets bit 0 to indicate body compression would silently corrupt an old reader that ignored the flag. Encoders MUST write `flags = 0`.

### 2.2 Body

The body is the encoding of a single root struct. It begins immediately after the header and continues to the end of the blob. The body has no length prefix — the blob byte count from the storage layer (e.g., a Spanner BYTES cell) is the body length plus 8.

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

Every length-delimited read is bounded by its enclosing region: the root body is bounded by the blob length supplied by the storage layer, and any nested LENGTH_DELIM value is bounded by its own length prefix. Decoders MUST reject any length-delimited value whose declared length exceeds the remaining bytes of the current bounded region.

### 3.3 Duplicate fields and duplicate map keys

If the same field tag appears more than once within a struct body, the **last** value wins; for slices and maps, the entire field value is replaced by the most recent occurrence. If a map payload contains the same key more than once, the **last** entry wins. Decoders MAY offer a strict mode that rejects duplicates, but the default behaviour is last-wins so generated decoders do not need to track per-tag or per-key seen-bitmaps.

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

For `fmtVer = 1`, all integer fields use VARINT. The schema does not yet support a fixed-width integer encoding; FIXED64 is reserved for `float64` and FIXED32 for `float32`. A future fmtVer may opt fields into FIXED encoding via a schema annotation; switching an existing field between VARINT and FIXED is a breaking schema change.

Encoders MUST emit canonical shortest-form varints for both keys and values. A varint MUST NOT exceed 10 bytes (the maximum needed for a `uint64`). Decoders MUST reject varints that overflow `uint64`. Decoders SHOULD accept non-canonical (overlong) varints under the default mode but MAY reject them in strict mode.

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

Three states are used in fmtVer 1:

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
3. The field is annotated `//gsbm:opaque` and a custom codec for the type is registered with the codegen.

Any other reachable external type (e.g., `time.Time`, `decimal.Decimal`, `uuid.UUID` without a registered codec) MUST cause schema validation to fail. Codegen does not silently expand external closures because the resulting wire shape would be coupled to a third-party type definition the schema owner cannot freeze.

## 6. Root struct encoding

The body of a blob (offsets 8 onward) is the body of the root struct, NOT length-prefixed (the blob's external length bounds it). All other rules from §3-§5 apply.

The wire format does not encode the identity of the root struct type. The expected root type is supplied by the storage location, the calling code, or a decoder entrypoint (e.g., `gsbm.DecodeInto`). `schemaHint` is observability only and MUST NOT be used to dispatch a root-type decoder; two roots with disjoint shapes can collide on the same `schemaHint`.

## 7. Compatibility rules

### 7.1 Forward compatibility (old reader, new blob)

A reader at fmtVer 1 reading a blob written by a newer schema (more fields than the reader knows) MUST skip unknown tags using the wire-type rules in §3.2. All fields the reader knows are decoded as usual.

### 7.2 Backward compatibility (new reader, old blob)

A reader reading a blob written by an older schema (missing some fields the reader knows) leaves those fields at the type's zero value. No special action is required.

### 7.3 Cross-fmtVer

A reader at fmtVer N MUST refuse to decode a blob with fmtVer != N unless it explicitly implements multi-version dispatch. An implementation MAY register multiple decoders (one per supported fmtVer) and dispatch via the header.

### 7.4 Rollback

Both forward and backward compatibility hold within a single fmtVer. A deployment may be rolled back to a prior schema without re-encoding stored data — old code reading newly-written blobs skips unknown tags; new code reading old blobs sees zero values for missing tags.

## 8. Constraints summary for encoders and decoders

Encoders MUST:
- Emit a valid 8-byte header with correct magic, `fmtVer = 1`, and `flags = 0`.
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
- Write deprecated fields (per the schema policy; this is a schema-level rule, not enforceable from the wire alone).

Decoders MUST:
- Verify magic and fmtVer; reject malformed.
- Reject blobs with non-zero `flags` bits (no flag semantics defined for fmtVer 1).
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
Header (8 bytes):
  47 53 42 4D       magic "GSBM"
  01                fmtVer = 1
  00                flags
  XX XX             schemaHint (some uint16)

Body:
  08                key: (1<<3)|0 = 8     (tag=1, VARINT)
  2A                value: 42
  1A                key: (3<<3)|2 = 26    (tag=3, LENGTH_DELIM)
  02                length: 2
  41 46             "AF"
  38                key: (7<<3)|0 = 56    (tag=7, VARINT)
  01                value: zigzag(-1) = 1
```

Total 8 bytes for the body, 16 with the 8-byte header.

## 10. Versioning of this document

This is fmtVer = 1. Future revisions of the wire format will produce a new version of this document with explicit diffs. The fmtVer byte in the header is the authoritative version identifier; this document name should match.
