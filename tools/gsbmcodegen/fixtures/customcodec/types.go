// Package customcodec is a fixture exercising the `bin:"N,custom=Name"`
// custom-codec feature. Record carries two third-party-ish types whose
// wire shape is dictated by a registered codec rather than by struct
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
//
// The committed *_gsbm.go and *_gsbm_arena.go siblings are produced by
// gsbmcodegen.GenerateWithCodecs against the registry built in
// regen_golden_test.go (built-ins + a DecimalAmount-bound DecimalString).
package customcodec

import "time"

//gsbm:root
type Record struct {
	CreatedAt  time.Time     `bin:"1,custom=Time"`
	Amount     DecimalAmount `bin:"2,custom=DecimalString"`
	OptionalAt *time.Time    `bin:"3,custom=Time"`
}
