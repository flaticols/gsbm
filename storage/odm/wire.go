package odm

// WireType identifies the on-the-wire encoding of a field value.
type WireType uint8

const (
	WireVarint      WireType = 0
	WireFixed64     WireType = 1
	WireLengthDelim WireType = 2
	WireFixed32     WireType = 3
)

// MaxTag is the largest tag value that fits in the field-key encoding
// (29-bit tag shifted left by 3 to merge with the wire type).
const MaxTag uint32 = (1 << 29) - 1

// validWireType reports whether wt is a wire type defined in fmtVer 1.
// Codes 4..7 are reserved per spec §3.2 and MUST be rejected by decoders.
func validWireType(wt WireType) bool {
	return wt <= WireFixed32
}

// zigzagEncode64 maps a signed int64 to its zigzag uint64 form so that
// small-magnitude values (positive or negative) produce short varints.
func zigzagEncode64(v int64) uint64 {
	return uint64((v << 1) ^ (v >> 63))
}

// zigzagDecode64 is the inverse of zigzagEncode64.
func zigzagDecode64(u uint64) int64 {
	return int64((u >> 1) ^ -(u & 1))
}
