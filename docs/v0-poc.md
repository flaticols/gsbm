# Offer Binary Marshaller V1

## Summary

`odm-bin-v1` is an opt-in binary storage encoding for Spanner offer batches. It encodes the current Spanner root shape, `[]offer.Offer`, directly from ODM structs without protobuf mapping, msgpack, reflection, or generic object fallback.

The codec is intentionally narrow: it is a storage codec for offer batches, not a general-purpose serialization format. Schema evolution is handled by introducing a new encoding method/version, not by adding field tags to the wire format.

## Goals

- Reduce Spanner offer encode/decode CPU versus protobuf.
- Reduce allocations by avoiding ODM -> protobuf object graph construction.
- Keep existing protobuf storage paths intact for compatibility.
- Keep old stored records readable through their stored `Encoding` value.
- Use generated/fixed-order marshal and unmarshal code for the ODM graph.
- Avoid msgpack and reflection in the `odm-bin-v1` path.

## Non-Goals

- No migration of existing Spanner records.
- No general codec registration for arbitrary ODM types.
- No stable cross-language wire contract.
- No field-tag based schema evolution.
- No support for decoding `odm-bin-v1` data with protobuf readers, or vice versa.

## Storage Integration

The encoding method is:

```go
encoding.EncodingODMBinV1 = "odm-bin-v1"
```

Spanner offer storage registers it through `withOfferStorageEncodings`, which adds `offerBinV1Encoding` to the existing `Offers` encoder map. The adapter accepts `odm-bin-v1` in `Options.Validate`.

Protobuf and protobuf-gzip remain available. Msgpack is not registered in Spanner offer storage and is not used by the binary codec.

## Root Wire Format

The root payload is:

```text
magic        4 bytes     "OEB1"
offers?      marker      0 = nil, 1 = present
count        uvarint     only when offers is present
offers       repeated    fixed-order offer records
```

Decode rejects:

- Missing or invalid magic.
- Invalid marker bytes.
- Truncated primitive values.
- Unexpected trailing bytes after the root value.

## Primitive Encoding

Primitive operations live in `internal/pkg/binmarshal`.

| Type | Encoding |
|---|---|
| `nil` / present marker | one byte: `0` nil, `1` present |
| `bool` | marker byte: `0` false, `1` true |
| unsigned integers | `binary.AppendUvarint` |
| signed integers | `binary.AppendVarint` |
| `float64` | 8 bytes, little-endian IEEE 754 |
| `string` | byte length as uvarint, then raw bytes |
| `[]byte` | byte length as uvarint, then raw bytes |
| `time.Time` | nil/present marker; present value is Unix nanoseconds as varint, decoded as UTC |

Slices and maps are encoded as:

```text
present?     marker
count        uvarint     only when present
items        repeated
```

Nil and empty collections are distinct:

- nil slice/map: marker `0`
- empty non-nil slice/map: marker `1`, count `0`

Maps are encoded in sorted key order where direct map codecs are used. This keeps output deterministic.

## Amount Encoding

`common.Amount` uses a compact custom numeric representation instead of JSON/protobuf/msgpack:

```text
present?      marker
units         varint
nanos         varint
scale         varint
currencyCode  string
```

Empty amounts encode as absent. Non-empty amounts are converted through `Amount.value.Int64(scale)` and reconstructed with `NewAmountFromDecimal`.

## Struct Encoding

Structs are encoded in fixed field order by explicit encode/decode functions. There are no wire field names or tags.

Example pattern:

```go
func EncodeOfferBinV1(w *binmarshal.Writer, z *offer.Offer) error
func DecodeOfferBinV1(r *binmarshal.Reader, z *offer.Offer) error
```

Decode functions must read fields in exactly the same order as encode functions write them. Adding, removing, or reordering fields is a breaking change for the encoding method and requires a new version, for example `odm-bin-v2`.

## Generated Coverage

The current implementation uses direct marshal/unmarshal coverage for the Spanner offer graph, including:

- `offer.Offer`
- `offer.OfferItem`
- `offer.OfferService`
- offer root fields such as POS, trip type, offer criteria, metadata, products, pax trip maps, penalties, accommodations, deleted order items, and forfeited info
- item fields such as rich media, reprice metadata, existing order items, length of stay, trip purpose segment, change fee, and item metadata
- service fields such as seat assignment, rich media, service attributes, extra attributes, and price
- `common.Price`, `Amount`, `Tax`, `TaxMetadata`, `Fee`, `Discount`, `Surcharges`, `ExchangeRate`, `Terms`, cancellation/rebooking/name-change terms
- flight criteria, journeys, segments, legs, cabins, carrier info, transport points, travelers
- seat maps, seat profiles, distribution chain links, contact info
- product graph fields used by offer storage
- deterministic map codecs for string maps, pax journey maps, and reward definition maps

The `odm-bin-v1` path does not call msgpack and does not use reflection-based marshal/unmarshal.

## Any Map Support

The only intentionally generic value codec is for `map[string]any` in product reward definitions. It is explicit and tag-based, not reflection-based.

Supported value tags:

| Tag | Type |
|---:|---|
| `0` | nil |
| `1` | string |
| `2` | bool |
| `3` | signed integer |
| `4` | float64 |
| `5` | `map[string]any` |
| `6` | `[]any` |

Unsupported runtime value types return an encode error.

## Error Handling

Encoders return contextual errors such as:

```text
encode offer 3: offer items: item 7: price: taxes: tax 2: metadata: ...
```

The Spanner adapter wraps encode/decode failures in the existing storage error flow.

Readers validate primitive boundaries and return `io.ErrUnexpectedEOF` for truncated payloads. Marker values other than `0` or `1` are invalid.

## Compatibility

Compatibility is selected by the stored encoding method:

- Existing protobuf records continue decoding through protobuf readers.
- New `odm-bin-v1` records decode only through the `odm-bin-v1` reader.
- There is no automatic migration.
- Wire compatibility for `odm-bin-v1` is fixed to the implemented field order.

Breaking wire changes require a new encoding method/version.

## Benchmarks

Latest local benchmark command:

```sh
go test ./internal/adapt/storage/spanner -run '^$' -bench 'BenchmarkSpannerOfferEncoding' -benchmem -count=1
```

Environment:

- Apple M2 Max
- Go benchmark
- Spanner offer storage benchmark

| Fixture | Path | Method | ns/op | B/op | allocs/op |
|---|---|---|---:|---:|---:|
| Synthetic 100 offers | Full encode | `odm-bin-v1` | 1,452,730 | 2,389,662 | 4,803 |
| Synthetic 100 offers | Full encode | protobuf | 5,855,134 | 6,896,241 | 56,904 |
| Synthetic 100 offers | Full decode | `odm-bin-v1` | 3,758,028 | 5,512,519 | 95,301 |
| Synthetic 100 offers | Full decode | protobuf | 8,033,257 | 11,893,152 | 154,210 |
| BDD 40 offers | Full encode | `odm-bin-v1` | 85,184 | 245,760 | 1 |
| BDD 40 offers | Full encode | protobuf | 318,485 | 411,200 | 3,123 |
| BDD 40 offers | Full decode | `odm-bin-v1` | 204,616 | 325,121 | 5,241 |
| BDD 40 offers | Full decode | protobuf | 423,245 | 688,762 | 8,769 |

Interpretation:

- Encode avoids ODM -> protobuf mapping and is allocation-minimal for the BDD fixture.
- Decode is already faster and lower-allocation than protobuf, but it still rebuilds the ODM graph.
- Decode allocations mostly come from creating slices, maps, pointers, and strings.

## Testing

Focused tests:

```sh
go test ./internal/pkg/binmarshal ./internal/pkg/encoding ./internal/models/odm/common ./internal/models/odm/offer ./internal/adapt/storage/spanner
```

Coverage includes:

- primitive package compile/test coverage
- model package compile/test coverage
- Spanner option validation for `odm-bin-v1`
- synthetic offer round-trip
- BDD golden-loaded offer round-trip
- existing protobuf paths remain available

## Future Work

Decode allocation reduction can be addressed with reusable decode targets:

```go
DecodeInto(serialized []byte, dst *[]offer.Offer) error
```

The implementation should reset existing objects and reuse slice/map capacity where possible:

- `dst = dst[:0]` for the root slice.
- Clear maps before reuse.
- Reuse nested slices when capacity is sufficient.
- Reuse pointer targets where the field remains present.
- Reset absent pointer fields to nil.

This would preserve the current wire format while reducing decode allocations for repeated reads.

## Operational Notes

- `odm-bin-v1` is experimental and opt-in.
- Do not use it for records that must be readable by older binaries lacking this codec.
- Keep protobuf as the compatibility baseline until migration and rollback plans exist.
- Treat every field-order change as a wire-format change.
