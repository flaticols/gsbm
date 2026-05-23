// Package gsbm implements the gsbm tagged binary wire format (fmtVer = 2).
//
// The format and its rules are normative; see docs/spec.md. This package
// supplies the heap-mode runtime: a Writer that appends to a caller-owned
// buffer and a Reader that decodes from a caller-owned slice. Higher-level
// MarshalGSBM / UnmarshalGSBM methods are emitted by codegen on top of these
// primitives.
package gsbm

import (
	"errors"
	"math"
)

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
// The invariant `v.SizeGSBM() == len(body produced by v.MarshalGSBM())`
// is load-bearing: it lets the bodyLen header field be filled in before
// the body is written. Hand-written MarshalGSBM implementations that need
// a SizeGSBM partner can compute one with gsbm.NewCountingWriter (Task 4).
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
func Marshal(v Marshaler, schemaHint uint16) ([]byte, error) {
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
