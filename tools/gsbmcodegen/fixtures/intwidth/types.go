// Package intwidth is a fixture exercising the `bin:"N,type=int32|int64"`
// wire-width override for Go `int` fields (issue #48).
//
//   - Small int `bin:"1,type=int32"` — explicit form of the default. Wire
//     bytes MUST be byte-identical to an un-annotated `int` field with the
//     same tag number, and out-of-range values MUST be rejected at encode
//     and decode with ErrIntegerOverflow.
//   - Large int `bin:"2,type=int64"` — opts the field out of the int32
//     bounds check. The wire shape matches a Go `int64` field for the
//     same value; values up to MaxInt64 round-trip cleanly.
//
// The committed *_gsbm.go and *_gsbm_arena.go siblings are byte-for-byte
// reproducible from these declarations via gsbmcodegen.Generate /
// GenerateArena.
package intwidth

//gsbm:root
type Record struct {
	Small int `bin:"1,type=int32"`
	Large int `bin:"2,type=int64"`
}
