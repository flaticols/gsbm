// Package embed exercises anonymous embedded struct flattening (issue #11).
// The validator promotes each tagged field of the embedded type into the
// outer struct's tag space; the codegen accesses the promoted fields via
// the explicit dotted path `v.<Embed>.<Field>` rather than relying on Go's
// field promotion (so multi-level embeds with shadowed names compile
// unambiguously). The wire bytes are byte-identical to a hand-flattened
// struct carrying the same tags directly — pinned by Flat below.
//
// The committed *_gsbm.go siblings are byte-for-byte reproducible from
// these declarations via gsbmcodegen.Generate.
package embed

// Base is embedded into Extended and (transitively) into Deep — it carries
// tag 1.
type Base struct {
	Total int64 `bin:"1"`
}

// Mid is embedded into Deep, and itself embeds Base. The promoted Total
// field appears at the Deep top level with FlattenedFrom = "Mid.Base".
type Mid struct {
	Base
	Note string `bin:"3"`
}

// Extended is the simple value-embed case: `Base` is anonymous, so Total
// (tag 1) is promoted into Extended alongside Reason (tag 2).
//
//gsbm:root
type Extended struct {
	Base
	Reason string `bin:"2"`
}

// Flat is the hand-written equivalent of Extended — the exact tag/type
// layout that a user would write if they had inlined Base by hand. The
// byte-equality test in extended_test.go pins that Extended encodes
// byte-identically to Flat.
//
//gsbm:root
type Flat struct {
	Total  int64  `bin:"1"`
	Reason string `bin:"2"`
}

// PtrExtended embeds Base by pointer. When the pointer is nil the encoder
// skips every flattened tag (no orphan keys on the wire); when present
// (even with all-zero fields) every tag is emitted. The decoder lazily
// allocates v.Base on the first incoming Base-tag.
//
//gsbm:root
type PtrExtended struct {
	*Base
	Reason string `bin:"2"`
}

// Deep is the multi-level case: Deep embeds Mid which embeds Base. After
// flattening, Deep has tags 1 (Total, from Mid.Base), 3 (Note, from Mid),
// and 4 (Caller, direct).
//
//gsbm:root
type Deep struct {
	Mid
	Caller string `bin:"4"`
}
