# odm-bin Wire Format Specification

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
- Reserved fields and bits MUST be written as zero by encoders and MUST be ignored by decoders.

## 2. Blob structure

A serialized record (a "blob") consists of an 8-byte header followed by a body.

```
+--------+--------+--------+--------+
|         magic  (4 bytes)          |   offsets 0..3
+--------+--------+--------+--------+
| fmtVer | flags  |    schVer       |   offsets 4..7
+--------+--------+--------+--------+
|             body                  |   offsets 8..end
+-----------------------------------+
```

### 2.1 Header fields

| Offset | Size | Name    | Type    | Description |
|--------|------|---------|---------|-------------|
| 0      | 4    | magic   | bytes   | ASCII `'O','D','M','B'` (0x4F, 0x44, 0x4D, 0x42). |
| 4      | 1    | fmtVer  | uint8   | Wire format version. Currently `1`. |
| 5      | 1    | flags   | uint8   | Bitfield. Bit 0 reserved for future built-in compression marker. Bits 1-7 reserved. |
| 6      | 2    | schVer  | uint16  | Schema fingerprint hash (computed by the writer's schema closure). For observability and sanity checks; not used to select a decoder. |

A decoder MUST verify magic and reject blobs whose magic does not match. A decoder MUST verify fmtVer matches a version it implements; if not, it MUST reject the blob. A decoder MUST NOT branch decode logic on schVer for the same fmtVer — schVer is informational.

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

Tag `0` is reserved and MUST NOT be used by encoders for real fields. A decoder encountering tag `0` MUST treat the blob as malformed.

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

## 4. Primitive value encoding

### 4.1 Integers (wire type VARINT)

Unsigned integers (`uint8`, `uint16`, `uint32`, `uint64`) are encoded as varint of their value.

Signed integers (`int8`, `int16`, `int32`, `int64`) are encoded as varint of their **zigzag-encoded** value:

```
zigzag_encode(n) = (n << 1) ^ (n >> 63)   // for int64; analogous for smaller widths
```

Zigzag avoids long varints for small negative numbers.

A decoder reading an integer field MUST treat the value as signed iff the schema declares the field as signed.

### 4.2 Booleans (wire type VARINT)

Encoded as varint `0` (false) or `1` (true). Decoders MUST treat any non-zero value as true; encoders SHOULD always emit exactly `0` or `1`.

### 4.3 Floats

`float32`: wire type FIXED32. Bits of IEEE 754 single-precision representation, little-endian.

`float64`: wire type FIXED64. Bits of IEEE 754 double-precision representation, little-endian.

NaN and infinities are preserved by exact bit-pattern round-trip.

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

Fields declared as nullable in the schema (e.g., a Go `*T` pointer) are encoded with a single presence byte preceding the value payload. The presence byte carries:

| Bit | Name          | Meaning |
|-----|---------------|---------|
| 0   | present       | 0 = nil (no value follows). 1 = value is present. |
| 1   | zero-elided   | 1 = value is the zero value of its type; payload is omitted. 0 = payload follows. |
| 2   | dirty (reserved) | Reserved for future explicit-set tracking (partial-update use cases). MUST be 0. |
| 3-7 | reserved      | MUST be 0. |

Three states are used in fmtVer 1:

| Bits 0-1 | Meaning |
|----------|---------|
| `00`     | nil. No payload follows. |
| `01`     | Present and non-zero. Value payload follows. |
| `11`     | Present and zero. No payload follows; decoder restores type's zero value. |
| `10`     | Reserved. Decoders MUST treat as malformed. |

Zero-elision is permitted only for fields whose schema type is a Go builtin primitive (`int*`, `uint*`, `float*`, `bool`, `string`, `[]byte`). Encoders MUST NOT zero-elide named or user-defined types whose zero-value semantics are not stable across schema changes. Decoders that read a zero-elided value of an ineligible type MUST treat the blob as malformed.

The presence byte is part of the field value, not the key — the field's wire type still describes the underlying value type, not the presence byte.

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

K MUST be a primitive type or `string`. K MUST NOT be a struct, slice, map, or nullable. Decoders MAY reject blobs that violate this.

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

## 6. Root struct encoding

The body of a blob (offsets 8 onward) is the body of the root struct, NOT length-prefixed (the blob's external length bounds it). All other rules from §3-§5 apply.

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
- Emit a valid 8-byte header with correct magic and fmtVer.
- Use the field-key encoding from §3.1.
- Use varint for VARINT-typed values, IEEE 754 LE bits for floats, length-prefix for LENGTH_DELIM values.
- Apply zigzag encoding to signed integers.
- Apply presence-byte rules to nullable fields, including zero-elision restrictions.
- Length-prefix all nested structs, slices, and maps.
- Tag uniquely within a struct.

Encoders MUST NOT:
- Emit tag 0.
- Reuse a tag for a field of a different type (within or across schema versions).
- Zero-elide non-builtin types.
- Write deprecated fields (per the schema policy; this is a schema-level rule, not enforceable from the wire alone).

Decoders MUST:
- Verify magic and fmtVer; reject malformed.
- Skip unknown tags using wire-type rules.
- Treat missing tags as zero values for known fields.
- Reject reserved wire types (4-7) as malformed.
- Reject reserved presence-byte states (`10`) as malformed.

Decoders MAY:
- Validate UTF-8 in strings (not required).
- Reject map keys of disallowed types (recommended).
- Surface schVer for observability.

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
  4F 44 4D 42       magic "ODMB"
  01                fmtVer = 1
  00                flags
  XX XX             schVer (some uint16)

Body:
  08                key: (1<<3)|0 = 8     (tag=1, VARINT)
  2A                value: 42
  1A                key: (3<<3)|2 = 26    (tag=3, LENGTH_DELIM)
  02                length: 2
  41 46             "AF"
  38                key: (7<<3)|0 = 56    (tag=7, VARINT)
  01                value: zigzag(-1) = 1
```

Total 16 bytes for the body, 24 with header.

## 10. Versioning of this document

This is fmtVer = 1. Future revisions of the wire format will produce a new version of this document with explicit diffs. The fmtVer byte in the header is the authoritative version identifier; this document name should match.
