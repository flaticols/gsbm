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
	// HeaderSize is the byte count of the blob header (magic, fmtVer,
	// flags, schemaHint, bodyLen). The body follows immediately after.
	HeaderSize = 12
	// FlagCompressed marks the body as a zstd frame (SpeedFastest). Bits
	// 1–7 of the flags byte remain reserved; decoders reject any set bit
	// outside FlagCompressed.
	FlagCompressed uint8 = 0x01
)

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
	// ErrCorruptCompressedBody fires when flags bit 0 is set (zstd-framed
	// body) and the zstd decoder rejects the body — truncated frame, bad
	// magic, malformed block, etc. The framing layer collapses all such
	// decoder errors into a single sentinel so callers can distinguish
	// "compression layer broke" from wire/varint errors above it.
	ErrCorruptCompressedBody = errors.New("gsbm: corrupt compressed body")
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
	// Compress, when true, runs the encoded body through zstd
	// (SpeedFastest) and sets bit 0 of the header flags. Old readers
	// reject such blobs cleanly with ErrReservedFlags; new readers
	// decompress transparently. Both MarshalWithOptions and
	// MarshalToWriter route through the same streaming encoder path,
	// so the raw body never materializes as a single []byte regardless
	// of entry point; the buffered MarshalWithOptions just collects the
	// compressed bytes before returning. Prefer MarshalToWriter when
	// even the compressed body would be uncomfortably large to hold.
	Compress bool
}

// MarshalWithOptions is the opt-in encode entry point. With the zero
// Options value it returns bytes identical to Marshal(v, schemaHint).
// With Options{Compress: true} the body is zstd-framed (SpeedFastest) and
// flag bit 0 is set; the on-disk bodyLen field carries the compressed
// length. Headers and the compatibility rules are unchanged otherwise —
// fmtVer stays at 2, reserved bits 1–7 stay reserved.
func MarshalWithOptions(v Marshaler, schemaHint uint16, opts Options) ([]byte, error) {
	if !opts.Compress {
		return marshalUncompressed(v, schemaHint)
	}
	return marshalCompressed(v, schemaHint)
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
func marshalCompressed(v Marshaler, schemaHint uint16) ([]byte, error) {
	var buf bytes.Buffer
	if err := marshalCompressedStreaming(&buf, v, schemaHint); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// marshalCompressedStreaming runs the size pass with region-size
// recording, then streams the write pass directly through a pool-borrowed
// zstd encoder so the raw body never materializes as a single []byte.
// Compressed bytes accumulate in a bytes.Buffer (size ≈ compressed body),
// which is then prefixed with the header and written to w in two
// io.Writer calls.
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
func marshalCompressedStreaming(w io.Writer, v Marshaler, schemaHint uint16) error {
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
	enc := getEncoder()
	// Default to dropping the encoder; the success path flips encUsable to
	// true only after enc.Close() returns cleanly. Any error before that —
	// MarshalGSBM, bw.Err, flushAll, or Close itself — leaves the encoder
	// in an unknown state, so the pool's New() factory replaces it on the
	// next Get rather than letting the next borrower inherit broken state.
	// Reset(nil) on the success path keeps the pooled encoder from pinning
	// &compressed across pool entries.
	encUsable := false
	defer func() {
		if encUsable {
			enc.Reset(nil)
			putEncoder(enc)
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

	var hdr [HeaderSize]byte
	copy(hdr[0:4], Magic)
	hdr[4] = FmtVer2
	hdr[5] = FlagCompressed
	binary.LittleEndian.PutUint16(hdr[6:8], schemaHint)
	binary.LittleEndian.PutUint32(hdr[8:12], uint32(compressed.Len()))

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
// With Options{Compress: true} the body is streamed directly through a
// zstd encoder, with the raw body never landing in any single buffer:
// peak heap during the call is bounded by streamFlushThreshold plus the
// compressed body size, not by the raw body size. This is the streaming
// counterpart to MarshalWithOptions and produces byte-identical output
// for the same input and options.
func MarshalToWriter(w io.Writer, v Marshaler, schemaHint uint16, opts Options) error {
	if !opts.Compress {
		blob, err := marshalUncompressed(v, schemaHint)
		if err != nil {
			return err
		}
		_, err = w.Write(blob)
		return err
	}
	return marshalCompressedStreaming(w, v, schemaHint)
}
