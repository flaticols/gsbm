// Package intwidth is a fixture exercising the `bin:"N,type=…"` wire-width
// override across every integer Go kind it supports (issue #50).
//
// Record covers the original int-only contract from PR #51:
//
//   - Small int `bin:"1,type=int32"` — explicit form of the default. Wire
//     bytes MUST be byte-identical to an un-annotated `int` field with the
//     same tag number, and out-of-range values MUST be rejected at encode
//     and decode with ErrIntegerOverflow.
//   - Large int `bin:"2,type=int64"` — opts the field out of the int32
//     bounds check. The wire shape matches a Go `int64` field for the
//     same value; values up to MaxInt64 round-trip cleanly.
//
// WideRecord covers the generalized contract added by issue #50: the
// symmetric platform-width fix for `uint`/`uintptr`, narrowing on the
// fixed-width signed and unsigned kinds, the identity-marker case, and
// the named-alias path.
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

// UserID is a named alias of int64. The named-alias path is covered by
// WideRecord.NamedAlias: WireOverrideCompat must walk *types.Named to the
// underlying basic kind before checking sign-compat and width-fit.
type UserID int64

//gsbm:root
type WideRecord struct {
	// Uint covers the symmetric platform-width fix: pre-#50 `uint`
	// always emitted a uint32 bounds check on encode and decode, so
	// values > MaxUint32 returned ErrIntegerOverflow even on a 64-bit
	// host. type=uint64 opts the field out of that check, mirroring
	// `int type=int64`.
	Uint uint `bin:"1,type=uint64"`
	// Uintptr is the same fix on `uintptr`, which shares the bug.
	Uintptr uintptr `bin:"2,type=uint64"`
	// NarrowSigned is a kind that today emits no bounds check at all
	// (the int64 wire is its natural shape); type=int16 narrows it and
	// enables encode + decode bounds checks against the int16 range.
	NarrowSigned int64 `bin:"3,type=int16"`
	// NarrowUnsigned is the unsigned analogue: uint64 narrowed to uint8.
	NarrowUnsigned uint64 `bin:"4,type=uint8"`
	// Identity is the documentation-marker case: type=int32 on an int32
	// field is legal and must produce byte-identical wire to an
	// un-annotated int32 — the bounds check is redundant (effective
	// wire width equals Go width) and must not be emitted.
	Identity int32 `bin:"5,type=int32"`
	// NamedAlias exercises the named-alias path: UserID's underlying
	// type is int64, so type=int32 is a legal narrowing.
	NamedAlias UserID `bin:"6,type=int32"`
}
