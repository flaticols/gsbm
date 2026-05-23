// Package customcodec is a fixture exercising the `bin:"N,custom=Name"`
// custom-codec feature. Record carries third-party-ish types whose wire
// shape is dictated by a registered codec rather than by struct
// traversal:
//
//   - CreatedAt time.Time → encoded as a (seconds, nanos) pair inside a
//     LENGTH_DELIM envelope via the built-in Time codec. Full Go range,
//     including time.Time{}, round-trips exactly.
//   - Amount DecimalAmount → encoded as a string (LENGTH_DELIM) via a
//     user-bound DecimalString codec wired to ParseDecimalAmount.
//   - OptionalAt *time.Time → the same Time codec, wrapped in the spec
//     §5.1 nullable envelope. PresenceNil round-trips nil;
//     PresenceNonZero runs the codec on the dereferenced time.
//   - AmountAppend DecimalAmount → encoded as a string (LENGTH_DELIM)
//     via a user-bound DecimalAppend codec wired to
//     DecimalAmount.AppendText. Same wire shape as Amount for the same
//     value; exercises the materializing append-codec path.
//   - Payload LargePayload → encoded as JSON inside a LENGTH_DELIM
//     envelope via a user-bound StreamingJSON codec wired to the
//     built-in StreamJSONBytes / DecodeJSONBytes pair. Exercises the
//     streaming-codec path: body materialized per pass, never retained.
//   - AmountBinary DecimalAmount → encoded as the binary
//     `uvarint(coef) ++ uvarint(scale<<1|sign)` body (LENGTH_DELIM) via a
//     user-bound DecimalBinary codec wired to the built-in
//     EncodeDecimalBinary / SizeDecimalBinary / DecodeDecimalBinary trio.
//     Exercises the analytic-codec path: size is a pure function of the
//     value and the encode path is allocation-free (no String()).
//
// The committed *_gsbm.go and *_gsbm_arena.go siblings are produced by
// gsbmcodegen.GenerateWithCodecs against the registry built in
// regen_golden_test.go (built-ins + a DecimalAmount-bound DecimalString,
// DecimalAppend and DecimalBinary, plus a LargePayload-bound StreamingJSON).
package customcodec

import "time"

//gsbm:root
type Record struct {
	CreatedAt    time.Time     `bin:"1,custom=Time"`
	Amount       DecimalAmount `bin:"2,custom=DecimalString"`
	OptionalAt   *time.Time    `bin:"3,custom=Time"`
	AmountAppend DecimalAmount `bin:"4,custom=DecimalAppend"`
	Payload      LargePayload  `bin:"5,custom=StreamingJSON"`
	AmountBinary DecimalAmount `bin:"6,custom=DecimalBinary"`
}

// Container nests Record three ways to exercise the materializing-codec
// fallback path: as a value field, as a slice element, and as a map
// value. Without the fallback, parent MarshalGSBM would write
// `w.WriteLength(child.SizeGSBM())` and then `child.MarshalGSBM(w)`;
// child.SizeGSBM() runs the materializing EmitFn against a fresh
// CountingWriter while child.MarshalGSBM hits the outer writer's
// scratch cache populated once per gsbm.Marshal call. If the codec
// were non-deterministic the two sizes would diverge and the declared
// length prefix would not match the body written. The fallback
// (BeginLengthDelim/EndLengthDelim around child.MarshalGSBM) routes
// the length through the recording-region machinery so it observes
// the cached body byte count.
//
//gsbm:root
type Container struct {
	Inner    Record            `bin:"1"`
	Items    []Record          `bin:"2"`
	ByKey    map[string]Record `bin:"3"`
	PtrItems []*Record         `bin:"4"`
	// Pages exercises the transitive composite-fallback path: the outer
	// slice element is itself a slice carrying a materializing-codec field.
	// Without the slice-encode composite guard, the outer envelope would
	// declare an analytic length computed from Record.SizeGSBM() while the
	// inner []Record falls back to BeginLengthDelim and writes observed
	// bytes, leaving the two passes byte-misaligned for any non-deterministic
	// codec.
	Pages [][]Record `bin:"5"`
	// Buckets exercises the same hazard with a map-of-Record element. The
	// outer slice's elem is *types.Map; the inner emitMapEncode already
	// falls back per its own typeContainsMaterializingCodec check, so the
	// outer must follow.
	Buckets []map[string]Record `bin:"6"`
	// Aliased exercises the named-non-struct-alias path: PageList is a
	// `type PageList []Record`. Without the named-alias recursion in
	// namedContainsMaterializingCodec, Container would not be flagged as a
	// materializing-codec carrier through this field (Container's encode
	// would still route the Aliased field's MarshalGSBM through the
	// BeginLengthDelim fallback because AliasContainer is itself a struct
	// whose direct walk surfaces Record). The risk is the OUTER level: if
	// any future fixture wraps Container in a parent, the parent's
	// WriteLength(Container.SizeGSBM()) would diverge from the body bytes
	// produced by Container.MarshalGSBM. The fixture exists primarily so
	// the predicate's named-alias path is exercised at codegen time and
	// the generated AliasContainer code uses the fallback shape.
	Aliased AliasContainer `bin:"7"`
}

// PageList is a named-non-struct alias over a materializing-codec carrier
// (`Record` has DecimalString / DecimalAppend fields). The codegen must
// treat this as a materializing-codec carrier transitively: a parent that
// holds it is itself a carrier, so any outer struct nesting that parent
// must route through the BeginLengthDelim fallback instead of the
// analytic WriteLength path. (Named map aliases like `type BucketMap
// map[string]Record` are not currently supported by the schema; only the
// slice alias form is exercised here.)
type PageList []Record

// AliasContainer holds the named alias as a field. When wrapped by an
// outer struct (Container.Aliased), the outer's emit must observe that
// AliasContainer transitively carries a materializing codec through
// PageList — otherwise the outer would mis-declare the length prefix
// relative to the alias's BeginLengthDelim-bound body bytes.
type AliasContainer struct {
	Pages PageList `bin:"1"`
}

// LargePayload is a stand-in for the kind of value a streaming codec
// shines on: a body whose size is not knowable without producing it
// (json.Marshal is required to know the byte count) and that can be
// arbitrarily large in production. The two-field struct keeps the JSON
// output deterministic (encoding/json sorts struct fields by source
// order) so the streaming codec body matches between the size pass and
// the write pass — a precondition for any streaming codec whose
// materialization is non-deterministic to use this kind safely (see the
// StreamJSONBytes godoc).
type LargePayload struct {
	Tag  string `json:"tag"`
	Data []byte `json:"data,omitempty"`
}
