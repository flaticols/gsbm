// Package gsbm implements the gsbm tagged binary wire format (fmtVer = 2).
//
// The format and its rules are normative; see docs/spec.md. This package
// supplies the heap-mode runtime: a Writer that appends to a caller-owned
// buffer and a Reader that decodes from a caller-owned slice. Higher-level
// MarshalGSBM / UnmarshalGSBM methods are emitted by codegen on top of these
// primitives.
package gsbm

import "errors"

const (
	Magic = "GSBM"
	// FmtVer2 is the current wire-format version. fmtVer = 1 was a draft
	// that never carried production data; current decoders reject it
	// outright (callers regenerate codecs to advance).
	FmtVer2 = 2
	// HeaderSize is the byte count of the blob header (magic, fmtVer,
	// flags, schemaHint, bodyLen). The body follows immediately after.
	HeaderSize = 12
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
)
