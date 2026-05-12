// Package trackpresence is a fixture exercising the //gsbm:track-presence
// opt-in. The marker tells the codegen to write decode-time presence bits
// to a user-declared `gsbmPresent` field on the receiver instead of routing
// through the package-level sidecar, so post-decode `FieldPresent(tag)` can
// observe which tags appeared on the wire even after the decode call
// returns. The wire format is unchanged — this is purely a Go-side
// opt-in to stored presence.
//
// Users must declare `gsbmPresent [K]uint64 ` + "`bin:\"-\"`" + ` ` on the marked
// struct (K large enough to cover the max in-range tag); the codegen
// validates the field shape and emits a clear error if it is missing.
package trackpresence

//gsbm:root
//gsbm:track-presence
type Offer struct {
	gsbmPresent [1]uint64 `bin:"-"`

	ID       string  `bin:"1"`
	Quantity int64   `bin:"2"`
	Price    float64 `bin:"3"`
	Active   bool    `bin:"4"`
	Note     *string `bin:"5"`
}
