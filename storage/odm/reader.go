package odm

import (
	"encoding/binary"
	"math"
)

// Reader decodes a tagged-binary blob from a caller-owned byte slice. The
// Reader does not copy input bytes — string and byte-slice reads return
// sub-slices of the original input. The caller therefore MUST NOT free or
// mutate the source slice while decoded values are still in use.
//
// A Reader is not safe for concurrent use.
type Reader struct {
	buf   []byte
	pos   int
	end   int // exclusive end of the current bounded region
	err   error
	alloc Allocator // nil ⇒ default heap path (no virtual dispatch)
}

// NewReader wraps src and points at offset 0. The whole slice forms the
// initial bounded region. The reader uses the default (heap) allocator;
// use SetAllocator to install a custom one (e.g., arena).
func NewReader(src []byte) *Reader {
	return &Reader{buf: src, end: len(src)}
}

// SetAllocator installs a custom Allocator. Passing nil restores the
// default heap path. The Reader holds the allocator until Reset.
func (r *Reader) SetAllocator(a Allocator) { r.alloc = a }

// Allocator returns the currently-installed Allocator (nil for the
// default heap path).
func (r *Reader) Allocator() Allocator { return r.alloc }

// Reset re-points the Reader at src and clears state. The installed
// Allocator is preserved so a pooled (Reader, Allocator) pair stays paired
// across decode calls.
func (r *Reader) Reset(src []byte) {
	r.buf = src
	r.pos = 0
	r.end = len(src)
	r.err = nil
}

// Err returns the first error captured by the Reader, or nil.
func (r *Reader) Err() error { return r.err }

// HasMore reports whether the current bounded region has unread bytes
// remaining. For the root struct the bound is the blob end; for a nested
// struct, slice, or map, it is the end of the length-prefixed region
// opened by BeginLengthDelim.
func (r *Reader) HasMore() bool {
	return r.err == nil && r.pos < r.end
}

// Pos returns the current read offset, exposed for codegen and tests.
func (r *Reader) Pos() int { return r.pos }

func (r *Reader) setErr(err error) {
	if r.err == nil {
		r.err = err
	}
}

// ReadHeader consumes the 8-byte blob header. It enforces the magic and
// the supported fmtVer; on success it returns flags and schVer for the
// caller to surface (e.g., to telemetry).
func (r *Reader) ReadHeader() (flags uint8, schVer uint16, err error) {
	if r.err != nil {
		return 0, 0, r.err
	}
	if r.end-r.pos < 8 {
		r.setErr(ErrTruncated)
		return 0, 0, r.err
	}
	if string(r.buf[r.pos:r.pos+4]) != Magic {
		r.setErr(ErrBadMagic)
		return 0, 0, r.err
	}
	if r.buf[r.pos+4] != FmtVer1 {
		r.setErr(ErrUnsupportedVer)
		return 0, 0, r.err
	}
	flags = r.buf[r.pos+5]
	schVer = binary.LittleEndian.Uint16(r.buf[r.pos+6 : r.pos+8])
	r.pos += 8
	return flags, schVer, nil
}

// ReadTag consumes a varint key and unpacks it into (tag, wireType).
// The reserved tag 0 and reserved wire types are surfaced as errors.
func (r *Reader) ReadTag() (tag uint32, wt WireType, err error) {
	if r.err != nil {
		return 0, 0, r.err
	}
	v, err := r.readUvarint()
	if err != nil {
		return 0, 0, err
	}
	wt = WireType(v & 0x7)
	tagU := v >> 3
	if tagU == 0 {
		r.setErr(ErrZeroTag)
		return 0, 0, r.err
	}
	if tagU > uint64(MaxTag) {
		r.setErr(ErrTagOverflow)
		return 0, 0, r.err
	}
	if !validWireType(wt) {
		r.setErr(ErrReservedWire)
		return 0, 0, r.err
	}
	return uint32(tagU), wt, nil
}

func (r *Reader) readUvarint() (uint64, error) {
	v, n, err := readUvarint(r.buf[:r.end], r.pos)
	if err != nil {
		r.setErr(err)
		return 0, err
	}
	r.pos += n
	return v, nil
}

// ReadUvarint reads a value of wire type VARINT as an unsigned integer.
func (r *Reader) ReadUvarint() (uint64, error) { return r.readUvarint() }

// ReadVarint reads a signed integer (zigzag-decoded varint).
func (r *Reader) ReadVarint() (int64, error) {
	v, err := r.readUvarint()
	if err != nil {
		return 0, err
	}
	return zigzagDecode64(v), nil
}

// ReadBool reads a boolean (varint 0/1; non-zero is true per spec §4.2).
func (r *Reader) ReadBool() (bool, error) {
	v, err := r.readUvarint()
	if err != nil {
		return false, err
	}
	return v != 0, nil
}

// ReadFixed32 reads a 4-byte little-endian integer.
func (r *Reader) ReadFixed32() (uint32, error) {
	if r.err != nil {
		return 0, r.err
	}
	if r.end-r.pos < 4 {
		r.setErr(ErrTruncated)
		return 0, r.err
	}
	v := binary.LittleEndian.Uint32(r.buf[r.pos : r.pos+4])
	r.pos += 4
	return v, nil
}

// ReadFixed64 reads an 8-byte little-endian integer.
func (r *Reader) ReadFixed64() (uint64, error) {
	if r.err != nil {
		return 0, r.err
	}
	if r.end-r.pos < 8 {
		r.setErr(ErrTruncated)
		return 0, r.err
	}
	v := binary.LittleEndian.Uint64(r.buf[r.pos : r.pos+8])
	r.pos += 8
	return v, nil
}

// ReadFloat32 reads an IEEE 754 LE float32.
func (r *Reader) ReadFloat32() (float32, error) {
	v, err := r.ReadFixed32()
	if err != nil {
		return 0, err
	}
	return math.Float32frombits(v), nil
}

// ReadFloat64 reads an IEEE 754 LE float64.
func (r *Reader) ReadFloat64() (float64, error) {
	v, err := r.ReadFixed64()
	if err != nil {
		return 0, err
	}
	return math.Float64frombits(v), nil
}

// readLenBytes reads a varint length followed by that many bytes and
// returns a sub-slice of the source buffer.
func (r *Reader) readLenBytes() ([]byte, error) {
	n, err := r.readUvarint()
	if err != nil {
		return nil, err
	}
	if uint64(r.end-r.pos) < n {
		r.setErr(ErrTruncated)
		return nil, r.err
	}
	out := r.buf[r.pos : r.pos+int(n)]
	r.pos += int(n)
	return out, nil
}

// ReadString reads a varint length and returns the payload as a string.
// In the default heap mode the result is a copy of the underlying bytes
// and is safe past the source slice's lifetime; with a custom Allocator
// (e.g., arena) it may alias the source via unsafe.String.
func (r *Reader) ReadString() (string, error) {
	b, err := r.readLenBytes()
	if err != nil {
		return "", err
	}
	return r.AcquireString(b), nil
}

// AcquireString routes a freshly-read byte sub-slice through the installed
// Allocator. The default (no Allocator) path uses string(b), which copies.
// Codegen calls this directly when it has the bytes already in hand.
func (r *Reader) AcquireString(b []byte) string {
	if r.alloc == nil {
		return string(b)
	}
	return r.alloc.AcquireString(b)
}

// ReadBytes reads a varint length and returns a sub-slice of the source
// buffer. The caller MUST NOT outlive the source slice.
func (r *Reader) ReadBytes() ([]byte, error) {
	return r.readLenBytes()
}

// ReadLength reads a varint and returns it as an int. Used by codegen
// for slice/map count fields. The value MUST fit into the remaining
// bounded region: every element of a slice or (k,v) pair of a map is at
// least one byte on the wire, so a count exceeding the remaining bytes
// is provably malformed. Without this guard a malicious blob can trick
// MakeSlice into a panic via len out of range.
func (r *Reader) ReadLength() (int, error) {
	v, err := r.readUvarint()
	if err != nil {
		return 0, err
	}
	if v > uint64(r.end-r.pos) {
		r.setErr(ErrTruncated)
		return 0, r.err
	}
	return int(v), nil
}

// BeginLengthDelim consumes a varint length prefix and narrows the
// bounded region accordingly. The returned saved value MUST be passed to
// EndLengthDelim to restore the parent bound.
func (r *Reader) BeginLengthDelim() (savedEnd int, err error) {
	n, err := r.readUvarint()
	if err != nil {
		return 0, err
	}
	newEnd := r.pos + int(n)
	if newEnd > r.end || newEnd < r.pos { // overflow guard
		r.setErr(ErrTruncated)
		return 0, r.err
	}
	savedEnd = r.end
	r.end = newEnd
	return savedEnd, nil
}

// EndLengthDelim restores the bound saved by BeginLengthDelim. It is an
// error to leave bytes unread inside the region; callers either consume
// the entire body, or use SkipField to skip an unknown nested value
// without entering it.
func (r *Reader) EndLengthDelim(savedEnd int) error {
	if r.err != nil {
		return r.err
	}
	if r.pos != r.end {
		r.setErr(ErrTrailingBytes)
		return r.err
	}
	r.end = savedEnd
	return nil
}

// SkipField advances past one field value of the given wire type. It is
// the default branch in generated UnmarshalODM switch statements,
// supporting forward compatibility (unknown tags written by newer code).
func (r *Reader) SkipField(wt WireType) error {
	if r.err != nil {
		return r.err
	}
	switch wt {
	case WireVarint:
		_, err := r.readUvarint()
		return err
	case WireFixed64:
		if r.end-r.pos < 8 {
			r.setErr(ErrTruncated)
			return r.err
		}
		r.pos += 8
		return nil
	case WireFixed32:
		if r.end-r.pos < 4 {
			r.setErr(ErrTruncated)
			return r.err
		}
		r.pos += 4
		return nil
	case WireLengthDelim:
		n, err := r.readUvarint()
		if err != nil {
			return err
		}
		if uint64(r.end-r.pos) < n {
			r.setErr(ErrTruncated)
			return r.err
		}
		r.pos += int(n)
		return nil
	default:
		r.setErr(ErrReservedWire)
		return r.err
	}
}
