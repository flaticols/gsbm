// Package gsbm implements the gsbm tagged binary wire format (fmtVer = 2).
//
// The format and its rules are normative; see docs/spec.md. This package
// supplies the heap-mode runtime: a Writer that appends to a caller-owned
// buffer and a Reader that decodes from a caller-owned slice. Higher-level
// MarshalGSBM / UnmarshalGSBM methods are emitted by codegen on top of these
// primitives.
package gsbm

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
)

// streamFlushThreshold caps how many body bytes the streaming write pass
// accumulates in its scratch buffer before flushing to the downstream
// io.Writer (which is the zstd encoder in the compressed case). Keeping
// this small is the load-bearing property behind the "raw never
// materializes" guarantee — peak heap during MarshalToWriter is bounded
// by streamFlushThreshold + the compressed output size, not by the raw
// body size. 8 KiB is large enough to amortize per-flush overhead and
// small enough to stay well under any realistic raw body.
const streamFlushThreshold = 8 * 1024

const (
	Magic = "GSBM"
	// FmtVer2 is the current wire-format version. fmtVer = 1 was a draft
	// that never carried production data; current decoders reject it
	// outright (callers regenerate codecs to advance).
	FmtVer2 = 2
	// HeaderSize is the byte count of the base blob header (magic, fmtVer,
	// flags, schemaHint, bodyLen). The body follows immediately after for
	// uncompressed blobs and for legacy compressed blobs (flags bit 3
	// clear). A new compressed blob sets the extended-header flag (bit 3)
	// and carries a 4-byte inflatedLen after the base header, for a
	// 16-byte header total — see [ExtendedHeaderSize] and
	// [extendedHeaderBit].
	HeaderSize = 12
	// ExtendedHeaderSize is the byte count of a compressed blob's header
	// when it carries the inflatedLen field (flags bit 3 set): the 12-byte
	// base header plus a 4-byte inflatedLen (uint32 LE). The decoder reads
	// inflatedLen to pre-size and bound the decompression buffer.
	ExtendedHeaderSize = 16
	// FlagCompressed is the legacy name for the zstd compression method.
	//
	// Deprecated: the flags byte now carries a 3-bit compression-method
	// enum (see [CompressionMethod] and [compressionMethodMask]) rather
	// than a single "compressed" bit. FlagCompressed remains exported as
	// uint8(CompressionZstd) for source compatibility; new code should
	// switch on the decoded CompressionMethod instead of masking this bit.
	FlagCompressed uint8 = uint8(CompressionZstd)
)

// CompressionMethod identifies the codec applied to the blob body. It is
// stored in the low 3 bits of the header flags byte (bits 0–2, see
// [compressionMethodMask]); bits 3–7 stay reserved. The values 0 and 1
// are byte-identical to the pre-gzip flags encoding — an uncompressed
// blob writes flags 0x00 and a zstd blob writes 0x01 exactly as before,
// so every blob written by an earlier encoder round-trips unchanged.
//
// Encoding the codec as an explicit small integer (rather than a pile of
// independent bits) keeps the wire trivially portable to the planned Rust
// decoder: a reader reads one masked value and switches on it. Methods
// 3–7 are reserved for future codecs; a decoder MUST reject a method it
// does not implement, exactly as it rejects a reserved high bit.
type CompressionMethod uint8

const (
	// CompressionNone leaves the body uncompressed (flags 0x00).
	CompressionNone CompressionMethod = 0
	// CompressionZstd frames the body as a zstd frame at SpeedFastest
	// (flags 0x01). This is the codec that shipped in #56.
	CompressionZstd CompressionMethod = 1
	// CompressionGzip frames the body as a gzip stream at
	// DefaultCompression (flags 0x02). Lower resident memory per codec
	// instance than zstd at a comparable ratio; see docs/codecs/compression.md.
	CompressionGzip CompressionMethod = 2

	// compressionMethodMask isolates the compression-method field (bits
	// 0–2) from the flags byte. Bit 3 is the extended-header flag (see
	// [extendedHeaderBit]); bits 4–7 are reserved and MUST be zero.
	compressionMethodMask uint8 = 0x07

	// extendedHeaderBit (flags bit 3) signals that a 4-byte inflatedLen
	// field follows the 12-byte base header, making the on-disk header
	// [ExtendedHeaderSize] bytes. It is set only on compressed blobs (a
	// compression method in bits 0–2) written by an encoder that records
	// the inflated body size; uncompressed blobs and legacy compressed
	// blobs leave it clear. A decoder predating this bit sees it as a
	// reserved high bit and rejects the blob with ErrReservedFlags —
	// graceful rejection by the same rule that admitted each codec.
	extendedHeaderBit uint8 = 0x08

	// defaultCompression is the codec chosen when a caller opts into
	// compression without naming one (the deprecated Options.Compress
	// bool, or any future "just compress it" entry point). gzip is the
	// default: its resident footprint is ~7× smaller than zstd's, which
	// matters more than zstd's faster decode for the structured-repetitive
	// payloads this format targets. Old blobs written with zstd still
	// decode transparently — only newly compressed writes change codec.
	defaultCompression = CompressionGzip
)

// compresses reports whether m names a codec that frames the body (i.e.
// anything other than CompressionNone).
func (m CompressionMethod) compresses() bool { return m != CompressionNone }

// validCompressionMethod reports whether m is a method this build knows
// how to decode. Reserved values (3–7) are rejected like reserved flag
// bits — graceful rejection, never silent misinterpretation.
func validCompressionMethod(m CompressionMethod) bool {
	return m == CompressionNone || m == CompressionZstd || m == CompressionGzip
}

// headerCompressionMethod extracts and validates the compression method
// and the extended-header flag from a header flags byte. It returns
// ErrReservedFlags when any reserved high bit (4–7) is set, when the
// method field (bits 0–2) names a codec this build does not implement,
// or when the extended-header bit (bit 3) is set on a blob that is not
// compressed — inflatedLen is meaningless without a compressed body. All
// three are graceful rejections by the same rule, so an unknown or
// inconsistent flag never silently changes how the body is interpreted.
// The two decoder entry points (NewReaderFrom's pre-validate and
// ReadHeader) share this so the flags rule lives in one place. extended
// reports whether a 4-byte inflatedLen field follows the 12-byte base
// header (a 16-byte header total).
func headerCompressionMethod(flags uint8) (method CompressionMethod, extended bool, err error) {
	if flags&^(compressionMethodMask|extendedHeaderBit) != 0 {
		return 0, false, ErrReservedFlags
	}
	method = CompressionMethod(flags & compressionMethodMask)
	if !validCompressionMethod(method) {
		return 0, false, ErrReservedFlags
	}
	extended = flags&extendedHeaderBit != 0
	if extended && !method.compresses() {
		return 0, false, ErrReservedFlags
	}
	return method, extended, nil
}

var (
	ErrBadMagic        = errors.New("gsbm: bad magic")
	ErrUnsupportedVer  = errors.New("gsbm: unsupported fmtVer")
	ErrReservedFlags   = errors.New("gsbm: non-zero reserved flag bits in header")
	ErrTruncated       = errors.New("gsbm: truncated input")
	ErrTrailingBytes   = errors.New("gsbm: trailing bytes in bounded region")
	ErrVarintOverflow  = errors.New("gsbm: varint overflow")
	ErrZeroTag         = errors.New("gsbm: tag 0 is reserved")
	ErrReservedWire    = errors.New("gsbm: reserved wire type")
	ErrWrongWireType   = errors.New("gsbm: wire type does not match schema for known tag")
	ErrTagOverflow     = errors.New("gsbm: tag exceeds 2^29-1")
	ErrInvalidMapKey   = errors.New("gsbm: map key must be primitive or string")
	ErrInvalidPresence = errors.New("gsbm: invalid presence byte")
	ErrBodyTooLarge    = errors.New("gsbm: length-delim body exceeds reserved length-prefix slot")
	ErrIntegerOverflow = errors.New("gsbm: integer value out of range for destination type")
	ErrAllocTooLarge   = errors.New("gsbm: slice allocation exceeds memory budget")
	ErrBodyLenMismatch = errors.New("gsbm: header bodyLen does not match blob size")
	// ErrCorruptCompressedBody fires when the flags byte names a
	// compressing codec (zstd or gzip) and that codec's decoder rejects
	// the body — truncated frame, bad magic, malformed block, an inflated
	// body that exceeds the decode cap, etc. The framing layer collapses
	// all such decoder errors into a single sentinel so callers can
	// distinguish "compression layer broke" from wire/varint errors above
	// it.
	ErrCorruptCompressedBody = errors.New("gsbm: corrupt compressed body")
	// ErrUnsupportedCompression fires when an encoder is asked for a
	// CompressionMethod this build cannot produce (a reserved value 3–7).
	// It is an encode-time programming error, distinct from the
	// decode-time ErrReservedFlags a reader raises for the same value on
	// the wire.
	ErrUnsupportedCompression = errors.New("gsbm: unsupported compression method")
)

// Sizer reports the exact body byte count produced by MarshalGSBM —
// excluding the blob header. Codegen emits SizeGSBM on every generated
// root and nested struct so encoders can allocate the output buffer with
// `make([]byte, 0, HeaderSize + v.SizeGSBM())` and avoid geometric append
// growth.
//
// Generated SizeGSBM implementations are analytic and allocation-free
// (zero allocs/op on every generated type without opaque or streaming-
// codec fields): they sum [SizeTag], [SizeUvarint], [SizeString],
// [SizeLengthDelim], etc. instead of walking the body. The interface
// itself is unchanged — callers see the same SizeGSBM() int signature.
//
// The invariant `v.SizeGSBM() == len(body produced by v.MarshalGSBM())`
// is load-bearing: generated MarshalGSBM emits length prefixes via
// [Writer.WriteLength] with the SizeGSBM result, so any drift between
// size and marshal silently corrupts the wire. Hand-written MarshalGSBM
// implementations that need a SizeGSBM partner can compute one with
// [NewCountingWriter] (allocates, but matches the marshal output by
// construction).
type Sizer interface {
	SizeGSBM() int
}

// Size returns v.SizeGSBM(). It exists as a free function for symmetry
// with the future gsbm.Marshal helper; call sites that already hold a
// value of a concrete generated type can call v.SizeGSBM() directly.
func Size(v Sizer) int { return v.SizeGSBM() }

// Marshaler is the contract every codegen-emitted root type satisfies on
// its pointer receiver: it knows its exact body size and can append that
// body to a Writer. Hand-written implementations qualify too — see
// NewCountingWriter for a way to derive SizeGSBM without double-walking
// the field list.
type Marshaler interface {
	Sizer
	MarshalGSBM(w *Writer) error
}

// Marshal encodes v into a freshly allocated blob with the 12-byte header
// pre-populated from schemaHint and the body length determined by a
// size-mode pass over v. The returned slice's len equals
// HeaderSize+bodyLen exactly; cap may exceed len when the body contains
// nested length-delimited regions, because Writer.BeginLengthDelim
// transiently over-reserves the length varint slot and triggers one
// append grow on the initial buffer. The load-bearing invariant — final
// len matches the pre-computed size — is what bodyLen in the header
// depends on, and that always holds.
//
// Marshal threads ONE Writer through both passes via adoptScratch, so
// any materializing codec (DecimalString, JSON, …) materializes each
// occurrence exactly once per call: the size pass populates the
// scratch cache, the write pass hits cached entries instead of
// re-running the codec's gen function. Standalone SizeGSBM has no
// such hand-off and pays double materialization for materializing-
// codec fields — see tools/gsbmcodegen/codecs/README.md
// ("Standalone SizeGSBM cost") for the rationale.
//
// Marshal is the canonical encode entry point for codegen-generated
// types. Callers with a pooled buffer should construct a Writer directly
// (see BenchmarkLargeOrderEncodeHeapPooled) instead.
//
// Marshal is byte-for-byte identical to MarshalWithOptions(v, schemaHint,
// Options{}) — the no-opts case is a strict no-op that produces flags = 0
// and an uncompressed body. Compression is strictly opt-in via Options.
func Marshal(v Marshaler, schemaHint uint16) ([]byte, error) {
	return marshalUncompressed(v, schemaHint)
}

// Options controls optional encode-time behaviors layered on top of the
// base wire format. The zero value (Options{}) is byte-for-byte identical
// to plain Marshal — every field is an opt-in knob, never a default.
type Options struct {
	// Compress requests body compression with the default codec.
	//
	// Deprecated: the default codec is now gzip, not zstd — set
	// Compression explicitly to pin a codec. Compress is retained for
	// source compatibility and is equivalent to
	// Compression: defaultCompression when Compression is left at its
	// zero value (CompressionNone). If Compression is set, it wins and
	// Compress is ignored.
	Compress bool

	// Compression selects the body codec. The zero value
	// (CompressionNone) means no compression unless the deprecated
	// Compress field is set. Whichever codec is chosen, the raw body
	// never materializes as a single []byte: both MarshalWithOptions and
	// MarshalToWriter route through the same streaming encoder path, and
	// the buffered MarshalWithOptions just collects the compressed bytes
	// before returning. Prefer MarshalToWriter when even the compressed
	// body would be uncomfortably large to hold. Old readers reject a
	// codec they predate cleanly with ErrReservedFlags; new readers
	// decompress transparently.
	Compression CompressionMethod
}

// method resolves the effective codec for an Options value: an explicit
// Compression field always wins; otherwise the deprecated Compress bool
// maps to the default codec; otherwise no compression.
func (o Options) method() CompressionMethod {
	if o.Compression.compresses() {
		return o.Compression
	}
	if o.Compress {
		return defaultCompression
	}
	return CompressionNone
}

// MarshalWithOptions is the opt-in encode entry point. With the zero
// Options value it returns bytes identical to Marshal(v, schemaHint).
// With a compressing codec selected (Options.Compression, or the
// deprecated Options.Compress bool which maps to the default codec) the
// body is framed by that codec and the flags byte carries its
// CompressionMethod; the on-disk bodyLen field carries the compressed
// length. Headers and the compatibility rules are unchanged otherwise —
// fmtVer stays at 2, reserved bits 3–7 stay reserved.
func MarshalWithOptions(v Marshaler, schemaHint uint16, opts Options) ([]byte, error) {
	method := opts.method()
	if !validCompressionMethod(method) {
		return nil, ErrUnsupportedCompression
	}
	if !method.compresses() {
		return marshalUncompressed(v, schemaHint)
	}
	return marshalCompressed(v, schemaHint, method)
}

// marshalUncompressed is the shared implementation behind Marshal and
// MarshalWithOptions{Compress:false}. Output is byte-for-byte identical
// across both callers — the load-bearing backward-compat invariant.
func marshalUncompressed(v Marshaler, schemaHint uint16) ([]byte, error) {
	sw := NewCountingWriter()
	if err := v.MarshalGSBM(sw); err != nil {
		return nil, err
	}
	if err := sw.Err(); err != nil {
		return nil, err
	}
	bodyLen := sw.Size()
	if bodyLen < 0 || uint64(bodyLen) > math.MaxUint32 {
		return nil, ErrBodyTooLarge
	}
	bw := NewWriter(make([]byte, 0, HeaderSize+bodyLen))
	bw.adoptScratch(sw)
	bw.WriteHeader(0, schemaHint, uint32(bodyLen))
	if err := v.MarshalGSBM(bw); err != nil {
		return nil, err
	}
	if err := bw.Err(); err != nil {
		return nil, err
	}
	return bw.Bytes(), nil
}

// marshalCompressed produces a compressed blob as a single []byte by
// routing the streaming compressed encode into a bytes.Buffer. Sharing
// the streaming code path with MarshalToWriter guarantees byte-identical
// output between the two entry points — required by the equivalence test
// and load-bearing for callers that compare blobs across paths.
func marshalCompressed(v Marshaler, schemaHint uint16, method CompressionMethod) ([]byte, error) {
	var buf bytes.Buffer
	if err := marshalCompressedStreaming(&buf, v, schemaHint, method); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// marshalCompressedStreaming runs the size pass with region-size
// recording, then streams the write pass directly through a pool-borrowed
// encoder for the chosen codec so the raw body never materializes as a
// single []byte. Compressed bytes accumulate in a bytes.Buffer
// (size ≈ compressed body), which is then prefixed with the header and
// written to w in two io.Writer calls.
//
// Steps:
//  1. Recording size pass — records each length-delim region's body byte
//     count in BeginLengthDelim order; sizeAcc gives the total raw body
//     length for the bodyTooLarge guard.
//  2. Streaming write pass — drives a streamingWriter that writes
//     canonical varint lengths up-front (using the recorded sizes) and
//     flushes its small scratch buffer to the encoder whenever it
//     crosses streamFlushThreshold.
//  3. enc.Close() flushes the final block; the encoder is returned to
//     the pool (Reset/Write/Close → next Reset cycles cleanly).
//  4. Header + compressed body are written to w as two writes.
//
// method MUST be a compressing codec (CompressionZstd or CompressionGzip);
// callers route the no-compression case to marshalUncompressed.
func marshalCompressedStreaming(w io.Writer, v Marshaler, schemaHint uint16, method CompressionMethod) error {
	sw := newRecordingSizeWriter()
	if err := v.MarshalGSBM(sw); err != nil {
		return err
	}
	if err := sw.Err(); err != nil {
		return err
	}
	rawLen := sw.Size()
	if rawLen < 0 || uint64(rawLen) > math.MaxUint32 {
		return ErrBodyTooLarge
	}

	var compressed bytes.Buffer
	enc := getCompressor(method)
	// Default to dropping the codec; the success path flips encUsable to
	// true only after enc.Close() returns cleanly. Any error before that —
	// MarshalGSBM, bw.Err, flushAll, or Close itself — leaves the codec
	// in an unknown state, so the pool's New() factory replaces it on the
	// next Get rather than letting the next borrower inherit broken state.
	// putCompressor's Reset(nil) on the success path keeps the pooled codec
	// from pinning &compressed across pool entries.
	encUsable := false
	defer func() {
		if encUsable {
			putCompressor(method, enc)
		}
	}()
	enc.Reset(&compressed)

	bw := newStreamingWriter(enc, sw.recordedRegionSizes(), streamFlushThreshold)
	bw.adoptScratch(sw)
	if err := v.MarshalGSBM(bw); err != nil {
		_ = enc.Close()
		return err
	}
	if err := bw.Err(); err != nil {
		_ = enc.Close()
		return err
	}
	if err := bw.flushAll(); err != nil {
		_ = enc.Close()
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	encUsable = true

	if uint64(compressed.Len()) > math.MaxUint32 {
		return ErrBodyTooLarge
	}

	// New compressed blobs carry the inflated body length in an extended
	// 16-byte header (flags bit 3 set), so the decoder can pre-size and
	// hard-bound the decompression buffer instead of letting the codec's
	// output grow geometrically. rawLen is the size-pass total — the exact
	// uncompressed body byte count the decoder will inflate to — so it is
	// the inflatedLen the reader cross-checks against the decoded length.
	var hdr [ExtendedHeaderSize]byte
	copy(hdr[0:4], Magic)
	hdr[4] = FmtVer2
	hdr[5] = uint8(method) | extendedHeaderBit
	binary.LittleEndian.PutUint16(hdr[6:8], schemaHint)
	binary.LittleEndian.PutUint32(hdr[8:12], uint32(compressed.Len()))
	binary.LittleEndian.PutUint32(hdr[12:16], uint32(rawLen))

	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := w.Write(compressed.Bytes()); err != nil {
		return err
	}
	return nil
}

// MarshalToWriter streams the encoded blob to w. With Options{} the call
// is equivalent to Marshal followed by w.Write; the raw body materializes
// once as a single []byte (the buffered fallback is intentional — the
// streaming requirement only binds the compressed path).
//
// With a compressing codec selected the body is streamed directly through
// that codec's encoder, with the raw body never landing in any single
// buffer: peak heap during the call is bounded by streamFlushThreshold
// plus the compressed body size, not by the raw body size. This is the
// streaming counterpart to MarshalWithOptions and produces byte-identical
// output for the same input and options.
func MarshalToWriter(w io.Writer, v Marshaler, schemaHint uint16, opts Options) error {
	method := opts.method()
	if !validCompressionMethod(method) {
		return ErrUnsupportedCompression
	}
	if !method.compresses() {
		blob, err := marshalUncompressed(v, schemaHint)
		if err != nil {
			return err
		}
		_, err = w.Write(blob)
		return err
	}
	return marshalCompressedStreaming(w, v, schemaHint, method)
}
