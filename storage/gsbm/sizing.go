package gsbm

// SizeTag returns the byte count of the field-key varint encoding
// `(tag<<3) | wt`. Codegen-emitted SizeGSBM bodies call this for every
// field they would have written a tag for, in lockstep with MarshalGSBM's
// WriteTag.
// Dual of [Writer.WriteTag].
func SizeTag(tag uint32, wt WireType) int {
	return varintLen((uint64(tag) << 3) | uint64(wt))
}

// SizeUvarint returns the byte count of v encoded as an unsigned varint.
// Dual of [Writer.WriteUvarint].
func SizeUvarint(v uint64) int { return varintLen(v) }

// SizeVarint returns the byte count of v encoded as a zigzag varint.
// Dual of [Writer.WriteVarint].
func SizeVarint(v int64) int { return varintLen(zigzagEncode64(v)) }

// SizeBool is the byte count of a bool field's body: always 1.
// Dual of [Writer.WriteBool].
func SizeBool() int { return 1 }

// SizeFixed32 is the byte count of a fixed-width 32-bit field's body.
// Dual of [Writer.WriteFixed32].
func SizeFixed32() int { return 4 }

// SizeFixed64 is the byte count of a fixed-width 64-bit field's body.
// Dual of [Writer.WriteFixed64].
func SizeFixed64() int { return 8 }

// SizeString returns the byte count of a length-prefixed string body:
// the uvarint length plus the string's byte count.
// Dual of [Writer.WriteString].
func SizeString(s string) int { return varintLen(uint64(len(s))) + len(s) }

// SizeBytes returns the byte count of a length-prefixed []byte body.
// Dual of [Writer.WriteBytes].
func SizeBytes(p []byte) int { return varintLen(uint64(len(p))) + len(p) }

// SizeLengthDelim returns the byte count of a length-delim envelope whose
// body is bodyLen bytes long: the uvarint length prefix plus bodyLen.
// Dual of [Writer.WriteLength] (when followed by bodyLen body bytes) and of
// [Writer.BeginLengthDelim]/[Writer.EndLengthDelim] (when the body is
// produced dynamically).
func SizeLengthDelim(bodyLen int) int { return varintLen(uint64(bodyLen)) + bodyLen }

// SizeNullable* helpers return the byte count of a §5.1 nullable-scalar
// field body (length-delim envelope: presence byte + optional value).
// They handle the three on-the-wire shapes uniformly:
//
//   - nil pointer → SizeLengthDelim(1)   (PresenceNil)
//   - present zero → SizeLengthDelim(1)  (PresenceZero, eligible per spec §5.1)
//   - present non-zero → SizeLengthDelim(1 + Size<value>)
//
// Codegen inlines this logic so its output reads symmetrically with the
// corresponding emitOptionalEncode branch. These helpers exist for
// hand-written MarshalGSBM callers and as a documentation anchor for
// what the inlined codegen output is computing.

// SizeNullableString returns the body byte count of a *string field.
func SizeNullableString(p *string) int {
	if p == nil || *p == "" {
		return SizeLengthDelim(1)
	}
	return SizeLengthDelim(1 + SizeString(*p))
}

// SizeNullableBytes returns the body byte count of a *[]byte field.
// Zero-elide applies on len == 0 (both nil and empty slice).
func SizeNullableBytes(p *[]byte) int {
	if p == nil || len(*p) == 0 {
		return SizeLengthDelim(1)
	}
	return SizeLengthDelim(1 + SizeBytes(*p))
}

// SizeNullableBool returns the body byte count of a *bool field.
func SizeNullableBool(p *bool) int {
	if p == nil || !*p {
		return SizeLengthDelim(1)
	}
	return SizeLengthDelim(1 + SizeBool())
}

// SizeNullableInt64 returns the body byte count of a *int64 field.
func SizeNullableInt64(p *int64) int {
	if p == nil || *p == 0 {
		return SizeLengthDelim(1)
	}
	return SizeLengthDelim(1 + SizeVarint(*p))
}

// SizeNullableUint64 returns the body byte count of a *uint64 field.
func SizeNullableUint64(p *uint64) int {
	if p == nil || *p == 0 {
		return SizeLengthDelim(1)
	}
	return SizeLengthDelim(1 + SizeUvarint(*p))
}

// SizeNullableFloat32 returns the body byte count of a *float32 field.
func SizeNullableFloat32(p *float32) int {
	if p == nil || *p == 0 {
		return SizeLengthDelim(1)
	}
	return SizeLengthDelim(1 + SizeFixed32())
}

// SizeNullableFloat64 returns the body byte count of a *float64 field.
func SizeNullableFloat64(p *float64) int {
	if p == nil || *p == 0 {
		return SizeLengthDelim(1)
	}
	return SizeLengthDelim(1 + SizeFixed64())
}
