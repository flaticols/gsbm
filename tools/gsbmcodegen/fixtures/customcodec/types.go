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
