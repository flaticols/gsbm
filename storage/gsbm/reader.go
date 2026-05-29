package gsbm

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
)

// Reader decodes a tagged-binary blob from a caller-owned byte slice.
// Byte-slice reads return sub-slices of the original input. String reads
// copy in the default heap path, route through the installed Allocator
// when one is present, and may alias the input only when generated code
// explicitly opts into the unsafe //gsbm:borrow-strings path.
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

// NewReaderFrom is the streaming counterpart to NewReader: it consumes a
// complete blob from src (header + body) and returns a *Reader behaviorally
// identical to NewReader(blob) — callers proceed with the usual
// ReadHeader() then UnmarshalGSBM(r) pattern. It is the read-side mirror of
// MarshalToWriter.
//
// NewReaderFrom performs only a light pre-validation of the header so that
// a hostile stream cannot drive an unbounded body allocation: it rejects
// bad magic, unsupported fmtVer, and reserved flag bits before reading the
// body. The downstream ReadHeader() call re-validates and, when the flags
// byte names a compressing codec, transparently decompresses the body via
// the pooled decoder for that codec. This keeps decoder-pool exercise (and
// the rest of the framing rules) in exactly one place — the existing
// ReadHeader path.
//
// On a successful return, src has been read up to (header + bodyLen) bytes
// and no further. Any trailing bytes remain in src for the caller to
// handle (e.g., a length-framed stream of blobs).
//
// Errors:
//   - ErrTruncated — src ended before the full header + body had been read.
//   - ErrBadMagic / ErrUnsupportedVer / ErrReservedFlags — early header
//     rejects that happen before any body allocation.
//   - ErrAllocTooLarge — declared bodyLen would force an over-budget
//     allocation (see NewReaderFromN for a tighter, caller-controlled cap).
//   - any non-EOF error from src — returned verbatim.
//
// Hostile-stream note: bodyLen is a uint32, so the implicit upper bound on
// the body allocation is ~4 GiB on 64-bit and ~2 GiB on 32-bit. The body
// allocation happens BEFORE any body bytes are read, so wrapping src in
// io.LimitReader does NOT bound it — the make() runs against the declared
// bodyLen regardless of how many bytes src will actually supply. Callers
// that need a tighter, source-side-independent cap MUST use
// NewReaderFromN with an explicit maxBodyLen.
func NewReaderFrom(src io.Reader) (*Reader, error) {
	return newReaderFrom(src, -1)
}

// NewReaderFromN behaves like NewReaderFrom but rejects the blob with
// ErrAllocTooLarge before allocating if the on-disk bodyLen exceeds
// maxBodyLen.
//
// Scope of the bound: maxBodyLen applies to the on-disk body (the bytes
// between the 12-byte header and the end of the blob). For an
// uncompressed blob (compression method none) that is also the only
// body-shaped allocation, so maxBodyLen does cap total body memory. For a
// compressed blob (zstd or gzip) it bounds only the on-disk compressed
// frame; the subsequent decompression in ReadHeader can allocate up to
// decoderMaxDecompressedSize (~2-4 GiB) — enforced for zstd via the
// pooled decoder's WithDecoderMaxMemory and for gzip via decompressGzip's
// explicit inflate cap (see compress.go). A small high-ratio frame (a
// "compression bomb") that fits under maxBodyLen can therefore still
// inflate into the GiB range. There is no per-call inflated-size knob in
// this iteration; callers that need a tighter bound against hostile
// compressed input must keep the decoder cap in mind or pre-filter inputs.
//
// A negative maxBodyLen disables the explicit cap (the implicit uint32
// bodyLen ceiling still applies, as does the platform-int guard).
func NewReaderFromN(src io.Reader, maxBodyLen int) (*Reader, error) {
	return newReaderFrom(src, maxBodyLen)
}

func newReaderFrom(src io.Reader, maxBodyLen int) (*Reader, error) {
	var hdr [HeaderSize]byte
	if _, err := io.ReadFull(src, hdr[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, ErrTruncated
		}
		return nil, err
	}
	if string(hdr[0:4]) != Magic {
		return nil, ErrBadMagic
	}
	if hdr[4] != FmtVer2 {
		return nil, ErrUnsupportedVer
	}
	// Pre-validate the flags byte so a hostile stream that combines a
	// 4 GiB bodyLen with a reserved-bit (or unknown-codec) flag is
	// rejected before any large allocation. ReadHeader re-runs the same
	// check on the same bytes; the second pass is cheap and keeps the
	// validation rule in one place semantically.
	if _, err := headerCompressionMethod(hdr[5]); err != nil {
		return nil, err
	}
	bodyLen := binary.LittleEndian.Uint32(hdr[8:12])
	// Guard the make() against int overflow on 32-bit platforms (where
	// int is 32 bits): a bodyLen near 4 GiB plus the 12-byte header
	// would wrap to a negative length and panic. uint64 arithmetic
	// against math.MaxInt keeps the check correct on both 32-bit and
	// 64-bit builds. Surfaces as ErrAllocTooLarge so the panic-free
	// acceptance criterion in spec.md §8 holds for hostile inputs.
	if uint64(bodyLen) > uint64(math.MaxInt-HeaderSize) {
		return nil, ErrAllocTooLarge
	}
	// Apply the caller-supplied tighter bound before allocating. This
	// is the only effective DoS guard for hostile sources: io.LimitReader
	// would only kick in after make(), so callers MUST set maxBodyLen
	// explicitly when src is untrusted.
	if maxBodyLen >= 0 && uint64(bodyLen) > uint64(maxBodyLen) {
		return nil, ErrAllocTooLarge
	}

	blob := make([]byte, HeaderSize+int(bodyLen))
	copy(blob, hdr[:])
	if bodyLen > 0 {
		if _, err := io.ReadFull(src, blob[HeaderSize:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, ErrTruncated
			}
			return nil, err
		}
	}
	return NewReader(blob), nil
}

// SetAllocator installs a custom Allocator. Passing nil restores the
// default heap path. The Reader holds the allocator until Reset.
func (r *Reader) SetAllocator(a Allocator) { r.alloc = a }

// Allocator returns the currently-installed Allocator (nil for the
// default heap path).
func (r *Reader) Allocator() Allocator { return r.alloc }

// BorrowSource returns the Reader's current decode buffer — the byte
// slice that borrowed string decoders may alias. Callers that use
// //gsbm:borrow-strings MUST keep this slice immutable and not reuse it
// (e.g. via a buffer pool or scratch arena) for at least as long as any
// borrowed decoded value (or any borrowed slice element, map key/value,
// or pooled receiver field) can be observed. Mutating or reusing the
// slice while borrowed values are reachable corrupts them silently.
//
// Lifetime/reachability is automatic: each borrowed string holds a Go
// pointer into this buffer, so the GC keeps the underlying allocation
// live as long as any borrowed value is reachable. Callers do not need
// runtime.KeepAlive on the returned slice for GC purposes.
//
// The exact slice returned depends on how the Reader was constructed and
// whether the body was compressed:
//
//   - NewReader(src), uncompressed (compression method none, or before
//     ReadHeader / when no header is read at all): the original src,
//     verbatim. For a full blob this includes the 12-byte header; for
//     the headerless DecodeBodyInto path it is the body-only slice the
//     caller passed in.
//   - NewReaderFrom / NewReaderFromN, uncompressed: the header+body
//     buffer the streaming constructor allocated and read into.
//   - Either constructor, compressed (zstd or gzip, after ReadHeader):
//     the decompressed body buffer the reader allocated during
//     ReadHeader. This is a distinct allocation from any user-provided
//     input — pinning the original compressed bytes is NOT sufficient.
//
// Always safe to call. The returned slice may be empty (e.g. after
// Reset(nil)); callers should treat the lifetime contract as a no-op in
// that case.
//
// When an Allocator is installed (Allocator() != nil), generated borrow
// decoders route through AcquireString. Whether pinning this buffer is
// still required then depends on the allocator: the bundled
// storage/gsbmarena.Arena copies decoded bytes into arena-owned chunks,
// so its strings live independently of r.buf and pinning is unnecessary.
// A custom Allocator whose AcquireString aliases b directly (e.g. via
// unsafe.String) keeps the same lifetime requirement as the heap-borrow
// path — the pinning contract above still applies. The method returns
// the buffer for API consistency either way.
//
// Typical use is to return the buffer alongside the decoded value so the
// caller has a single ownership handle to prove the immutability/no-reuse
// contract is met. See docs/borrow-strings.md for the worked
// DecodeWithBody pattern and docs/codecs/compression.md for the
// compression-specific implications.
func (r *Reader) BorrowSource() []byte { return r.buf }

// Reset re-points the Reader at src and clears state. The installed
// Allocator is preserved so a pooled (Reader, Allocator) pair stays paired
// across decode calls. After Reset, BorrowSource() returns the new buffer;
// callers that still need to pin the previous source for borrowed values
// decoded against it must capture BorrowSource() before calling Reset.
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

// Pos returns the current read offset, exposed for tests that need to
// assert on byte boundaries.
func (r *Reader) Pos() int { return r.pos }

func (r *Reader) setErr(err error) {
	if r.err == nil {
		r.err = err
	}
}

// ReadHeader consumes the 12-byte blob header. It enforces the magic,
// the supported fmtVer, the flags rule (bits 0–2 = compression method,
// 0 none / 1 zstd / 2 gzip; bits 3–7 reserved), and the bodyLen
// cross-check; on success it returns flags, schemaHint, and bodyLen for
// the caller to surface (e.g., to telemetry).
//
// When flags names a compressing codec, the on-disk body is a frame of
// that codec. ReadHeader transparently decompresses it via a pool-borrowed
// decoder, replaces the Reader's view with the decompressed bytes, and the
// rest of the Reader API proceeds as if the blob had been written
// uncompressed. The returned bodyLen carries the on-disk (compressed)
// length, matching the header field; callers downstream of ReadHeader use
// the Reader's HasMore / Pos against the decompressed length implicitly.
func (r *Reader) ReadHeader() (flags uint8, schemaHint uint16, bodyLen uint32, err error) {
	if r.err != nil {
		return 0, 0, 0, r.err
	}
	if r.end-r.pos < HeaderSize {
		r.setErr(ErrTruncated)
		return 0, 0, 0, r.err
	}
	if string(r.buf[r.pos:r.pos+4]) != Magic {
		r.setErr(ErrBadMagic)
		return 0, 0, 0, r.err
	}
	if r.buf[r.pos+4] != FmtVer2 {
		r.setErr(ErrUnsupportedVer)
		return 0, 0, 0, r.err
	}
	flags = r.buf[r.pos+5]
	// Bits 0–2 carry the compression method (0 none / 1 zstd / 2 gzip);
	// bits 3–7 are reserved. A reserved bit set, or a method field naming
	// a codec this build predates, would change payload interpretation in
	// a way an old reader couldn't see, so reject rather than decode
	// blindly. method drives the decompress dispatch below.
	method, ferr := headerCompressionMethod(flags)
	if ferr != nil {
		r.setErr(ferr)
		return 0, 0, 0, r.err
	}
	schemaHint = binary.LittleEndian.Uint16(r.buf[r.pos+6 : r.pos+8])
	bodyLen = binary.LittleEndian.Uint32(r.buf[r.pos+8 : r.pos+12])
	// Cross-check the in-header bodyLen against the storage-layer length.
	// The storage layer's slice length is authoritative; a mismatch
	// indicates truncation or a writer bug, either of which makes the
	// blob malformed. For compressed blobs bodyLen carries the compressed
	// (on-disk) length — the cross-check is identical because the body
	// bytes between header and slice end are the compressed frame.
	if uint64(bodyLen) != uint64(r.end-r.pos-HeaderSize) {
		r.setErr(ErrBodyLenMismatch)
		return 0, 0, 0, r.err
	}
	r.pos += HeaderSize
	if method.compresses() {
		decompressed, derr := decompressBody(method, r.buf[r.pos:r.end])
		if derr != nil {
			r.setErr(ErrCorruptCompressedBody)
			return 0, 0, 0, r.err
		}
		// Replace the Reader's view with the decompressed body. Subsequent
		// reads (primitive, length-delim) operate on the inflated bytes;
		// callers above the framing layer (codegen UnmarshalGSBM) see no
		// difference from an uncompressed blob.
		r.buf = decompressed
		r.pos = 0
		r.end = len(decompressed)
	}
	return flags, schemaHint, bodyLen, nil
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
func (r *Reader) ReadUvarint() (uint64, error) {
	if r.err != nil {
		return 0, r.err
	}
	return r.readUvarint()
}

// ReadVarint reads a signed integer (zigzag-decoded varint).
func (r *Reader) ReadVarint() (int64, error) {
	if r.err != nil {
		return 0, r.err
	}
	v, err := r.readUvarint()
	if err != nil {
		return 0, err
	}
	return zigzagDecode64(v), nil
}

// ReadBool reads a boolean (varint 0/1; non-zero is true per spec §4.2).
func (r *Reader) ReadBool() (bool, error) {
	if r.err != nil {
		return false, r.err
	}
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
// (e.g., arena) ownership follows the allocator's documented lifetime.
// Generated //gsbm:borrow-strings decoders deliberately bypass ReadString
// in heap mode and use ReadBytes + unsafe.String to alias the input blob.
func (r *Reader) ReadString() (string, error) {
	b, err := r.readLenBytes()
	if err != nil {
		return "", err
	}
	return r.AcquireString(b), nil
}

// AcquireString routes a freshly-read byte sub-slice through the installed
// Allocator. The default (no Allocator) path uses string(b), which copies.
// Codegen calls this directly when it has the bytes already in hand, and
// borrow-string code still uses it for allocator-backed Readers so arena
// and custom allocator lifetimes are preserved.
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
	if uint64(r.end-r.pos) < n {
		r.setErr(ErrTruncated)
		return 0, r.err
	}
	savedEnd = r.end
	r.end = r.pos + int(n)
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
// the default branch in generated UnmarshalGSBM switch statements,
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
