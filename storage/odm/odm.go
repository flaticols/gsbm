// Package odm implements the odm-bin tagged binary wire format (fmtVer = 1).
//
// The format and its rules are normative; see docs/spec.md. This package
// supplies the heap-mode runtime: a Writer that appends to a caller-owned
// buffer and a Reader that decodes from a caller-owned slice. Higher-level
// MarshalODM / UnmarshalODM methods are emitted by codegen on top of these
// primitives.
package odm

import "errors"

const (
	Magic   = "ODMB"
	FmtVer1 = 1
)

var (
	ErrBadMagic        = errors.New("odm: bad magic")
	ErrUnsupportedVer  = errors.New("odm: unsupported fmtVer")
	ErrTruncated       = errors.New("odm: truncated input")
	ErrTrailingBytes   = errors.New("odm: trailing bytes in bounded region")
	ErrVarintOverflow  = errors.New("odm: varint overflow")
	ErrZeroTag         = errors.New("odm: tag 0 is reserved")
	ErrReservedWire    = errors.New("odm: reserved wire type")
	ErrTagOverflow     = errors.New("odm: tag exceeds 2^29-1")
	ErrInvalidMapKey   = errors.New("odm: map key must be primitive or string")
	ErrInvalidPresence = errors.New("odm: invalid presence byte")
	ErrBodyTooLarge    = errors.New("odm: length-delim body exceeds reserved length-prefix slot")
)
