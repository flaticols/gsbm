// Package builtins ships the default custom codecs that gsbm provides
// out of the box: Time (a full-range time.Time codec storing seconds +
// nanos inside a LENGTH_DELIM envelope), DecimalString (a templated
// string-form codec for decimal-like types built on `v.String()`),
// DecimalAppend (the append-style sibling of DecimalString, built on
// `v.AppendText(dst)` for source types that expose an append API), and
// StreamingJSON (a streaming-shape codec built on `json.Marshal` for
// large payloads that would otherwise double peak memory if cached).
// Users register additional project codecs by adding entries to the
// same Registry — see NewBuiltinRegistry below for the standard
// starting point.
//
// The encode/decode functions in this package use only the storage/gsbm
// Writer/Reader surface — no reflection, no new runtime types — so they
// fit straight into the emit-time call site that codegen generates for a
// `bin:"N,custom=Name"` field.
//
// # Analytic vs materializing-cached vs streaming codecs
//
// Every codec in this package — and every project codec a user adds to
// the same Registry — picks one of three shapes, distinguished by which
// fields it sets on its CodecDecl. The choice is dictated by whether the
// codec's body byte count is a pure function of v or only knowable by
// producing the body, and (for the latter) by how large the materialized
// body is expected to be.
//
//   - Analytic codecs (`SizeFn` + `EncodeFn`) — for codecs whose body
//     size is a pure function of v, computable without writing any
//     bytes. Codegen emits `SizeFn(v)` in the size pass and
//     `EncodeFn(w, v)` in the write pass; the hot path stays
//     branch-free. Time is the canonical analytic codec: `SizeTime(t)`
//     returns the byte count of the (seconds, nanos) body without
//     writing anything, matching what EncodeTime will write. Use this
//     shape for fixed-width primitives and anything whose width
//     follows directly from v.
//
//   - Materializing-cached codecs (`EmitFn` alone) — for codecs whose
//     body size depends on producing a small or medium body, e.g.
//     DecimalString (`v.String()` decides the byte count) or short
//     JSON. Codegen emits a single `EmitFn(w, v, callsite)` call
//     inside MarshalGSBM; the Writer is mode-aware (size-only vs
//     write) so the same function runs in both passes. The Writer's
//     per-call scratch cache, keyed by a codegen-emitted callsite id,
//     makes the underlying materialization run exactly once per
//     gsbm.Marshal call — the size pass populates the scratch entry
//     and the write pass reuses it. The trade-off: the materialized
//     bytes are retained alongside the output buffer, doubling peak
//     heap for the duration of the encode.
//
//   - Streaming codecs (`StreamFn` alone) — for codecs whose body size
//     depends on producing a body that can be large (~1 MiB and up).
//     Codegen emits `StreamFn(w, v)` (no callsite) in both passes
//     against a mode-aware Writer, and the result is discarded
//     between passes. The body is materialized twice (2× CPU) but
//     never retained alongside the output (1× peak heap). Suitable
//     for big JSON, compressed blobs, and anything whose
//     materialized form would meaningfully grow peak memory if
//     cached.
//
// The three shapes are mutually exclusive at registration time:
// declaring more than one of `(SizeFn, EncodeFn)`, `EmitFn`, and
// `StreamFn` on a single CodecDecl is rejected with
// `codec/conflicting-kinds`. Pick analytic when you can derive the
// size cheaply from v; pick materializing-cached when the body must
// be produced and is small/medium; pick streaming when the body must
// be produced and may be large enough that retaining it would matter.
//
// See ../README.md for the full codec-author decision guide,
// including the at-a-glance shape matrix, Writer-helper table for
// the materializing-cached path, the string-vs-append guidance for
// text-form codecs, and the determinism obligation for streaming
// codecs.
package builtins

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.flaticols.dev/gsbm/storage/gsbm"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/codecs"
)

// errInvalidNanos is returned by DecodeTime when the wire payload's nanos
// component is outside the [0, 999_999_999] range that time.Time mandates.
// A malformed encoder is the only path that can produce such a value; the
// decoder refuses rather than constructing an out-of-range time.Time.
var errInvalidNanos = errors.New("codec/Time: nanoseconds out of range [0, 999999999]")

// codecsPkgImport is the import path that codegen records in CodecDecl
// for codecs whose Go functions live in this package. Tests in the
// builtins package itself never use it — codegen does.
const codecsPkgImport = "go.flaticols.dev/gsbm/tools/gsbmcodegen/codecs/builtins"

// TimeDecl is the CodecDecl for the Time codec. Body is
// `varint(seconds) ++ uvarint(nanos)` wrapped in the standard analytic
// LENGTH_DELIM envelope; codegen emits the envelope (`WriteTag` + length
// prefix) around the body so this codec only writes the two varints.
//
// Properties (issue #21):
//   - Full Go time range: every time.Time value Go can produce round-trips
//     exactly. A naïve int64-nanosecond codec silently wraps for any time
//     outside ~1677–2262; encoding seconds and nanos separately avoids
//     that overflow.
//   - Zero-preserving: time.Time{}.Unix() = -62_135_596_800 and
//     Nanosecond() = 0; decode via time.Unix(s, n) reproduces the
//     year-1-AD instant such that got.Equal(time.Time{}) == true.
//   - Analytic shape: keeps the `(SizeFn, EncodeFn)` pair, so codegen
//     avoids per-write materialization branches.
//
// Location is not preserved — the codec encodes the instant only.
// time.Unix returns a Local-location time.Time; callers that need a
// specific location should normalize pre-encode (`t.UTC()`) or post-decode
// (`got.In(loc)`). Round-trip equality is `Equal()` / `Unix()` /
// `Nanosecond()`, not `reflect.DeepEqual`.
var TimeDecl = codecs.CodecDecl{
	Name:      "Time",
	GoType:    "time.Time",
	WireType:  codecs.WireLengthDelim,
	EncodeFn:  "EncodeTime",
	DecodeFn:  "DecodeTime",
	SizeFn:    "SizeTime",
	PkgImport: codecsPkgImport,
}

// EncodeTime writes the body of a Time codec field: `varint(t.Unix()) ++
// uvarint(t.Nanosecond())`. The surrounding LENGTH_DELIM envelope (field
// tag + length prefix) is emitted by codegen, not by this function. The
// returned error is always nil today — gsbm.Writer's Write methods don't
// surface per-call errors — but the signature carries one to match the
// codec contract.
//
// Range: every time.Time Go can produce. Location: not preserved (instant
// only). See TimeDecl.
func EncodeTime(w *gsbm.Writer, t time.Time) error {
	w.WriteVarint(t.Unix())
	w.WriteUvarint(uint64(t.Nanosecond()))
	return nil
}

// SizeTime returns the byte count EncodeTime writes for t: the
// concatenated varint+uvarint body, excluding the LENGTH_DELIM envelope
// (codegen adds the length prefix per the analytic LENGTH_DELIM contract).
// The shape mirrors EncodeTime exactly so the codegen swap
// (`EncodeFn(w, v)` → `SizeFn(v)`) preserves the body byte count by
// construction.
func SizeTime(t time.Time) int {
	return gsbm.SizeVarint(t.Unix()) + gsbm.SizeUvarint(uint64(t.Nanosecond()))
}

// DecodeTime reads the body of a Time codec field — `varint(seconds) ++
// uvarint(nanos)` — and stores `time.Unix(seconds, int64(nanos))` in *t.
// Returns errInvalidNanos if the wire payload's nanos component exceeds
// 999_999_999 (the only value time.Nanosecond() can return); the surrounding
// LENGTH_DELIM envelope is consumed by codegen before this function runs.
//
// The resulting time has the local time-zone (per time.Unix's contract);
// the codec encodes the instant only. Callers that need a specific
// location should call .UTC() or .In(loc) post-decode.
func DecodeTime(r *gsbm.Reader, t *time.Time) error {
	s, err := r.ReadVarint()
	if err != nil {
		return err
	}
	n, err := r.ReadUvarint()
	if err != nil {
		return err
	}
	if n > 999_999_999 {
		return errInvalidNanos
	}
	*t = time.Unix(s, int64(n))
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
// This is the materializing-codec replacement for the analytic
// (SizeDecimalString, EncodeDecimalString) pair (kept as deprecated
// aliases below): the size pass and the write pass call the same function
// against the same callsite id, so the Writer's scratch entry produced in
// the size pass is reused as-is for the write pass — no double
// materialization on the gsbm.Marshal path.
func EmitDecimalString[T fmt.Stringer](w *gsbm.Writer, v T, callsite uint64) error {
	return w.WriteCachedString(callsite, func() string { return v.String() })
}

// EncodeDecimalString writes v.String() as a LENGTH_DELIM string without
// the materializing-codec scratch cache. Retained as a thin alias so
// downstream codec registrations that still declare `(SizeFn, EncodeFn)`
// continue to compile during the migration window.
//
// Deprecated: register the codec with EmitFn=EmitDecimalString to skip the
// double materialization on the size→write hand-off.
func EncodeDecimalString[T fmt.Stringer](w *gsbm.Writer, v T) error {
	w.WriteString(v.String())
	return nil
}

// SizeDecimalString returns the byte count EncodeDecimalString writes for
// v. Retained alongside EncodeDecimalString for analytic-shape codec
// registrations.
//
// Deprecated: register the codec with EmitFn=EmitDecimalString to skip the
// double materialization on the size→write hand-off.
func SizeDecimalString[T fmt.Stringer](v T) int {
	return gsbm.SizeString(v.String())
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

// EmitDecimalAppend is the append-style sibling of EmitDecimalString:
// it writes the text form of v as a LENGTH_DELIM string, but reaches
// that form via v.AppendText(dst) rather than v.String(). For source
// types that expose an append API (encoding.TextAppender,
// (*big.Int).Append, custom decimal types with AppendText), this skips
// the intermediate string allocation entirely — the appended bytes go
// straight from the source value into the Writer's scratch cache.
//
// Wire shape is identical to EmitDecimalString for inputs that produce
// identical text bytes (LENGTH_DELIM: varint length followed by the
// text bytes), so the two codecs are wire-interchangeable for any value
// whose String() and AppendText output agree. Use EmitDecimalAppend
// when the source type offers an append method; use EmitDecimalString
// when only String() is available.
//
// Like EmitDecimalString, the materialize-once-per-occurrence guarantee
// holds across the gsbm.Marshal size/write hand-off via the callsite
// scratch cache.
func EmitDecimalAppend[T interface {
	AppendText(dst []byte) ([]byte, error)
}](w *gsbm.Writer, v T, callsite uint64) error {
	return w.WriteCachedAppendBytes(callsite, func(dst []byte) ([]byte, error) {
		return v.AppendText(dst)
	})
}

// NewDecimalAppendDecl builds a CodecDecl for a DecimalAppend-style
// codec bound to the user's concrete decimal type. Shape matches
// NewDecimalStringDecl exactly — both produce a materializing
// LENGTH_DELIM CodecDecl naming the user's wrapper emit/decode
// functions. The difference is intent: the user's emit wrapper calls
// EmitDecimalAppend (and therefore needs the source type's
// AppendText(dst []byte) ([]byte, error) method) instead of
// EmitDecimalString.
//
// Decoding is symmetric with DecimalString: the wire is a LENGTH_DELIM
// string, so DecodeDecimalString is the natural decode template for
// the user wrapper.
//
// Example registration (typical user code):
//
//	reg.Register(builtins.NewDecimalAppendDecl(
//	    "DecimalAppend",
//	    "myapp/v1.Decimal",
//	    "EmitDecimalAppend",   // user-written: calls EmitDecimalAppend
//	    "DecodeDecimalAppend", // user-written: calls DecodeDecimalString
//	    "myapp/v1",
//	))
func NewDecimalAppendDecl(name, goType, emitFn, decFn, pkgImport string) codecs.CodecDecl {
	return codecs.CodecDecl{
		Name:      name,
		GoType:    goType,
		WireType:  codecs.WireLengthDelim,
		EmitFn:    emitFn,
		DecodeFn:  decFn,
		PkgImport: pkgImport,
	}
}

// StreamJSONBytes is the built-in streaming codec body: marshals v to
// JSON and writes the resulting bytes as a LENGTH_DELIM string against
// the mode-aware Writer. Suitable for payloads whose materialized form
// can be large (~1 MiB and up) — the codec is invoked once per pass
// (size + write), but the materialized JSON is never retained between
// passes, so peak heap stays at one body's worth of bytes rather than
// two (the materializing-cached path's overhead).
//
// Wire shape: LENGTH_DELIM string containing exactly what
// json.Marshal(v) produces. Round-trip is via json.Unmarshal on the
// decoded bytes.
//
// Determinism: the streaming kind runs the codec body twice (once per
// pass) — for the wire bytes to match across passes, json.Marshal(v)
// must be deterministic. Go's encoding/json sorts map keys; struct and
// slice encodings are deterministic by construction; opaque types whose
// MarshalJSON is non-deterministic are not safe for streaming and
// should use the materializing-cached path (which only materializes
// once).
func StreamJSONBytes[T any](w *gsbm.Writer, v T) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	w.WriteBytes(b)
	return nil
}

// DecodeJSONBytes is the symmetric decode for StreamJSONBytes: reads
// the LENGTH_DELIM byte payload and unmarshals it into *v. Streaming
// codecs do not encode any framing beyond the standard LENGTH_DELIM
// envelope (which codegen emits around the body), so the decode is a
// straight pair to the encode.
func DecodeJSONBytes[T any](r *gsbm.Reader, v *T) error {
	b, err := r.ReadBytes()
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// NewStreamingJSONDecl builds a CodecDecl for a StreamingJSON-style
// codec bound to the user's concrete Go type. Shape mirrors
// NewDecimalAppendDecl / NewDecimalStringDecl, but the resulting
// CodecDecl has StreamFn set (streaming shape) rather than EmitFn — so
// codegen emits a no-callsite, no-cache call site that materializes
// the body twice but never retains it.
//
// The user supplies the codec name, the fully-qualified Go type, and
// the function identifiers for the wrapper stream/decode functions in
// their own package (which call StreamJSONBytes / DecodeJSONBytes
// underneath). pkgImport is the user's package; codegen records it so
// the generated file picks up the right import.
//
// Example registration (typical user code):
//
//	reg.Register(builtins.NewStreamingJSONDecl(
//	    "StreamingJSON",
//	    "myapp/v1.LargePayload",
//	    "StreamLargePayload",  // user-written: calls StreamJSONBytes
//	    "DecodeLargePayload",  // user-written: calls DecodeJSONBytes
//	    "myapp/v1",
//	))
func NewStreamingJSONDecl(name, goType, streamFn, decFn, pkgImport string) codecs.CodecDecl {
	return codecs.CodecDecl{
		Name:      name,
		GoType:    goType,
		WireType:  codecs.WireLengthDelim,
		StreamFn:  streamFn,
		DecodeFn:  decFn,
		PkgImport: pkgImport,
	}
}

// NewBuiltinRegistry returns a fresh Registry pre-loaded with the codecs
// gsbm ships by default. Today that is Time only; DecimalString is
// shipped as a template (NewDecimalStringDecl) because the codec is
// generic over the user's decimal type — the user binds the type at
// registration time and adds their own entry to the returned Registry.
func NewBuiltinRegistry() *codecs.Registry {
	r := codecs.NewRegistry()
	if err := r.Register(TimeDecl); err != nil {
		panic(fmt.Errorf("codecs/builtins: failed to register Time: %w", err))
	}
	return r
}
