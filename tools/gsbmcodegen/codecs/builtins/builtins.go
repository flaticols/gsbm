// Package builtins ships the default custom codecs that gsbm provides
// out of the box: TimeUnixNano (a time.Time ↔ int64-nanoseconds VARINT
// codec) and DecimalString (a templated string-form codec for decimal-
// like types). Users register additional project codecs by adding entries
// to the same Registry — see NewBuiltinRegistry below for the standard
// starting point.
//
// The encode/decode functions in this package use only the storage/gsbm
// Writer/Reader surface — no reflection, no new runtime types — so they
// fit straight into the emit-time call site that codegen generates for a
// `bin:"N,custom=Name"` field.
package builtins

import (
	"fmt"
	"time"

	"go.flaticols.dev/gsbm/storage/gsbm"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/codecs"
)

// codecsPkgImport is the import path that codegen records in CodecDecl
// for codecs whose Go functions live in this package. Tests in the
// builtins package itself never use it — codegen does.
const codecsPkgImport = "go.flaticols.dev/gsbm/tools/gsbmcodegen/codecs/builtins"

// TimeUnixNanoDecl is the CodecDecl for the TimeUnixNano codec.
// It encodes a time.Time as a zigzag VARINT of UnixNano(). The
// zero-time value (time.Time{}) maps to a small negative integer
// (the offset of time.Time's zero instant from the Unix epoch); decode
// round-trips that exact int64 back through time.Unix(0, n).
//
// Wire form: a single VARINT carrying the int64 nanosecond count.
// No envelope — the codec is the entire field body. Pointer-wrap
// (`*time.Time`) is handled by the caller (codegen renders the spec
// §5.1 LENGTH_DELIM + presence-byte wrapper around the codec call).
var TimeUnixNanoDecl = codecs.CodecDecl{
	Name:      "TimeUnixNano",
	GoType:    "time.Time",
	WireType:  codecs.WireVarint,
	EncodeFn:  "EncodeTimeUnixNano",
	DecodeFn:  "DecodeTimeUnixNano",
	SizeFn:    "SizeTimeUnixNano",
	PkgImport: codecsPkgImport,
}

// EncodeTimeUnixNano writes t as a zigzag VARINT of t.UnixNano(). Returns
// the Writer's accumulated error, if any — gsbm.Writer's write methods
// don't surface per-call errors, so this never fails synchronously today,
// but the signature carries an error to match the codec contract and to
// leave room for future range checks.
func EncodeTimeUnixNano(w *gsbm.Writer, t time.Time) error {
	w.WriteVarint(t.UnixNano())
	return nil
}

// SizeTimeUnixNano returns the byte count EncodeTimeUnixNano writes for t:
// the zigzag VARINT of t.UnixNano(). The shape mirrors EncodeTimeUnixNano
// exactly so the codegen swap (`EncodeFn(w, v)` → `SizeFn(v)`) preserves
// the body byte count by construction.
func SizeTimeUnixNano(t time.Time) int {
	return gsbm.SizeVarint(t.UnixNano())
}

// DecodeTimeUnixNano reads a zigzag VARINT and stores the result in *t as
// time.Unix(0, n). The resulting time has the local time-zone (per
// time.Unix's contract); callers that need UTC should call t.UTC()
// post-decode. Location is not part of the wire shape — the codec
// encodes the instant only.
func DecodeTimeUnixNano(r *gsbm.Reader, t *time.Time) error {
	n, err := r.ReadVarint()
	if err != nil {
		return err
	}
	*t = time.Unix(0, n)
	return nil
}

// EmitDecimalString writes v.String() as a LENGTH_DELIM string against a
// mode-aware Writer, using a callsite-keyed scratch cache so the
// underlying v.String() call runs exactly once per gsbm.Marshal call even
// though MarshalGSBM walks the field in both the size pass and the write
// pass. Trailing zeros, leading minus signs, and the empty string all
// round-trip exactly because the underlying transport is the gsbm string
// codec (length-prefixed bytes, no normalization).
//
// This is the materializing-codec replacement for the previous
// (SizeDecimalString, EncodeDecimalString) pair: the size pass and the
// write pass call the same function against the same callsite id, so the
// Writer's scratch entry produced in the size pass is reused as-is for
// the write pass — no double materialization on the gsbm.Marshal path.
func EmitDecimalString[T fmt.Stringer](w *gsbm.Writer, v T, callsite uintptr) error {
	return w.WriteCachedString(callsite, func() string { return v.String() })
}

// DecodeDecimalString reads a LENGTH_DELIM string and parses it into *v
// via parse. The parse function is supplied by the user at codec
// registration time so this template stays agnostic of the user's
// concrete decimal type.
func DecodeDecimalString[T any](r *gsbm.Reader, v *T, parse func(string) (T, error)) error {
	s, err := r.ReadString()
	if err != nil {
		return err
	}
	parsed, err := parse(s)
	if err != nil {
		return err
	}
	*v = parsed
	return nil
}

// NewDecimalStringDecl builds a CodecDecl for a DecimalString-style codec
// bound to the user's concrete decimal type. The user supplies the codec
// name, the fully-qualified Go type the codec handles, and the function
// identifiers for the wrapper emit/decode functions they will write in
// their own package (which call EmitDecimalString / DecodeDecimalString
// underneath). pkgImport is the user's package; codegen records it so the
// generated file picks up the right import.
//
// Example registration (typical user code):
//
//	reg.Register(builtins.NewDecimalStringDecl(
//	    "DecimalString",
//	    "myapp/v1.Decimal",
//	    "EmitDecimal",        // user-written: calls EmitDecimalString
//	    "DecodeDecimal",      // user-written: calls DecodeDecimalString
//	    "myapp/v1",
//	))
func NewDecimalStringDecl(name, goType, emitFn, decFn, pkgImport string) codecs.CodecDecl {
	return codecs.CodecDecl{
		Name:      name,
		GoType:    goType,
		WireType:  codecs.WireLengthDelim,
		EmitFn:    emitFn,
		DecodeFn:  decFn,
		PkgImport: pkgImport,
	}
}

// NewBuiltinRegistry returns a fresh Registry pre-loaded with the codecs
// gsbm ships by default. Today that is TimeUnixNano only; DecimalString
// is shipped as a template (NewDecimalStringDecl) because the codec is
// generic over the user's decimal type — the user binds the type at
// registration time and adds their own entry to the returned Registry.
func NewBuiltinRegistry() *codecs.Registry {
	r := codecs.NewRegistry()
	if err := r.Register(TimeUnixNanoDecl); err != nil {
		panic(fmt.Errorf("codecs/builtins: failed to register TimeUnixNano: %w", err))
	}
	return r
}
