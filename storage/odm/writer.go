package odm

import (
	"encoding/binary"
	"math"
)

// reservedLenBytes is the slot size reserved for a length-delimited region's
// length varint. 5 bytes covers payloads up to (1<<35)-1, which exceeds any
// realistic blob.
const reservedLenBytes = 5

// Writer appends a tagged-binary blob to a caller-owned byte slice. The
// zero value is not usable; construct with NewWriter or Reset on a pooled
// instance. A Writer is not safe for concurrent use.
type Writer struct {
	buf []byte
	err error
}

// NewWriter wraps buf for appending. The caller retains ownership; the
// Writer never reallocates beyond what append on the slice does.
func NewWriter(buf []byte) *Writer {
	return &Writer{buf: buf}
}

// Bytes returns the accumulated output. The caller MUST treat the result
// as read-only until the Writer is reset or discarded.
func (w *Writer) Bytes() []byte { return w.buf }

// Reset re-points the Writer at buf (truncated to length 0) and clears the
// sticky error. Callers pool a Writer and call Reset(b[:0]) per request.
func (w *Writer) Reset(buf []byte) {
	w.buf = buf[:0]
	w.err = nil
}

// Err returns the first error captured during writing, or nil. Once an
// error is set, subsequent writes are no-ops.
func (w *Writer) Err() error { return w.err }

func (w *Writer) setErr(err error) {
	if w.err == nil {
		w.err = err
	}
}

// WriteHeader emits the 8-byte blob header (magic, fmtVer=1, flags, schVer).
// Per spec, a writer SHOULD call WriteHeader exactly once, before the body.
func (w *Writer) WriteHeader(flags uint8, schVer uint16) {
	if w.err != nil {
		return
	}
	w.buf = append(w.buf, Magic[0], Magic[1], Magic[2], Magic[3], FmtVer1, flags, 0, 0)
	binary.LittleEndian.PutUint16(w.buf[len(w.buf)-2:], schVer)
}

// WriteTag emits the field key (tag<<3 | wireType) as a varint. tag must
// be in [1, MaxTag]; wt must be a defined wire type for fmtVer 1.
func (w *Writer) WriteTag(tag uint32, wt WireType) {
	if w.err != nil {
		return
	}
	if tag == 0 {
		w.setErr(ErrZeroTag)
		return
	}
	if tag > MaxTag {
		w.setErr(ErrTagOverflow)
		return
	}
	if !validWireType(wt) {
		w.setErr(ErrReservedWire)
		return
	}
	w.buf = appendUvarint(w.buf, (uint64(tag)<<3)|uint64(wt))
}

// WriteUvarint writes an unsigned varint value (no key).
func (w *Writer) WriteUvarint(v uint64) {
	if w.err != nil {
		return
	}
	w.buf = appendUvarint(w.buf, v)
}

// WriteVarint writes a signed integer using zigzag encoding.
func (w *Writer) WriteVarint(v int64) {
	if w.err != nil {
		return
	}
	w.buf = appendUvarint(w.buf, zigzagEncode64(v))
}

// WriteBool writes a boolean as varint 0 or 1.
func (w *Writer) WriteBool(b bool) {
	if w.err != nil {
		return
	}
	if b {
		w.buf = append(w.buf, 1)
	} else {
		w.buf = append(w.buf, 0)
	}
}

// WriteFixed32 writes a 4-byte little-endian value (used for float32 bits
// or fixed-width 32-bit integers when chosen by the schema).
func (w *Writer) WriteFixed32(v uint32) {
	if w.err != nil {
		return
	}
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	w.buf = append(w.buf, b[:]...)
}

// WriteFixed64 writes an 8-byte little-endian value.
func (w *Writer) WriteFixed64(v uint64) {
	if w.err != nil {
		return
	}
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	w.buf = append(w.buf, b[:]...)
}

// WriteFloat32 writes the IEEE 754 LE bits of f.
func (w *Writer) WriteFloat32(f float32) { w.WriteFixed32(math.Float32bits(f)) }

// WriteFloat64 writes the IEEE 754 LE bits of f.
func (w *Writer) WriteFloat64(f float64) { w.WriteFixed64(math.Float64bits(f)) }

// WriteString writes a varint length followed by the string bytes.
func (w *Writer) WriteString(s string) {
	if w.err != nil {
		return
	}
	w.buf = appendUvarint(w.buf, uint64(len(s)))
	w.buf = append(w.buf, s...)
}

// WriteBytes writes a varint length followed by the byte payload. The
// caller's slice is copied into the Writer's buffer.
func (w *Writer) WriteBytes(p []byte) {
	if w.err != nil {
		return
	}
	w.buf = appendUvarint(w.buf, uint64(len(p)))
	w.buf = append(w.buf, p...)
}

// BeginLengthDelim opens a length-prefixed region (nested struct, slice
// body, or map body). It reserves space for the length varint and returns
// a marker that must be passed to EndLengthDelim once the body is written.
func (w *Writer) BeginLengthDelim() int {
	if w.err != nil {
		return 0
	}
	pos := len(w.buf)
	// Reserve the maximum varint footprint we expect for a length value.
	// The tail will be shifted left if the actual encoding is shorter.
	w.buf = append(w.buf, 0, 0, 0, 0, 0)
	return pos
}

// EndLengthDelim closes the region started at marker, patching its length
// prefix and shifting the body left if the encoded length is shorter than
// the reserved slot.
func (w *Writer) EndLengthDelim(marker int) {
	if w.err != nil {
		return
	}
	bodyLen := len(w.buf) - marker - reservedLenBytes
	if bodyLen < 0 {
		w.setErr(ErrTruncated)
		return
	}
	n := varintLen(uint64(bodyLen))
	if n > reservedLenBytes {
		// A length varint wider than reservedLenBytes would overrun the
		// reserved slot and corrupt the first body byte. Practically only
		// reachable for ≥2^35-byte bodies, but we refuse to silently
		// produce an unparseable blob.
		w.setErr(ErrBodyTooLarge)
		return
	}
	if n < reservedLenBytes {
		// Shift the body bytes left so the length varint sits flush.
		copy(w.buf[marker+n:], w.buf[marker+reservedLenBytes:])
		w.buf = w.buf[:len(w.buf)-(reservedLenBytes-n)]
	}
	putUvarint(w.buf[marker:marker+n], uint64(bodyLen))
}
