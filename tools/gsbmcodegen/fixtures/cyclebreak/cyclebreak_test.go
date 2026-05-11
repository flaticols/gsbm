package cyclebreak

import (
	"bytes"
	"reflect"
	"testing"

	"go.flaticols.dev/gsbm/storage/gsbm"
)

// TestItemRoundTripNilPrevious confirms a leaf Item (Previous=nil) emits
// no key for tag 3 and decodes back to the same value-equal struct.
func TestItemRoundTripNilPrevious(t *testing.T) {
	in := Item{ID: "x1", Label: "head"}
	var w gsbm.Writer
	if err := in.MarshalGSBM(&w); err != nil {
		t.Fatalf("MarshalGSBM: %v", err)
	}
	r := gsbm.NewReader(w.Bytes())
	var out Item
	if err := out.UnmarshalGSBM(r); err != nil {
		t.Fatalf("UnmarshalGSBM: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round-trip mismatch:\n got: %+v\nwant: %+v", out, in)
	}
	if out.FieldPresent(3) {
		t.Fatalf("FieldPresent(3) = true, want false (Previous omitted on nil)")
	}
}

// TestItemRoundTripWithPrevious is the canonical id_ref case from the
// plan: Previous is *Item, and the round-trip must produce a *Item that
// carries only the ID. Label and any further Previous chain stay at the
// zero value — hydration is the caller's job.
func TestItemRoundTripWithPrevious(t *testing.T) {
	in := Item{
		ID:    "child",
		Label: "current",
		Previous: &Item{
			ID:       "parent",
			Label:    "should-be-dropped",
			Previous: &Item{ID: "grandparent", Label: "also-dropped"},
		},
	}
	var w gsbm.Writer
	if err := in.MarshalGSBM(&w); err != nil {
		t.Fatalf("MarshalGSBM: %v", err)
	}
	r := gsbm.NewReader(w.Bytes())
	var out Item
	if err := out.UnmarshalGSBM(r); err != nil {
		t.Fatalf("UnmarshalGSBM: %v", err)
	}
	want := Item{
		ID:       "child",
		Label:    "current",
		Previous: &Item{ID: "parent"}, // Label and Previous zeroed
	}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("round-trip mismatch:\n got: %+v\nwant: %+v", out, want)
	}
	if got := out.Previous; got == nil {
		t.Fatalf("decoded Previous is nil, want non-nil with ID=%q", want.Previous.ID)
	}
	if got := out.Previous.Previous; got != nil {
		t.Fatalf("Previous.Previous = %+v, want nil (id_ref decode is ID-only)", got)
	}
	if got := out.Previous.Label; got != "" {
		t.Fatalf("Previous.Label = %q, want empty (id_ref decode is ID-only)", got)
	}
	if !out.FieldPresent(3) {
		t.Fatalf("FieldPresent(3) = false, want true (Previous was on the wire)")
	}
}

// TestItemRoundTripEmptyID covers the present-with-empty-ID edge case:
// Previous points to an Item whose ID is "". Empty string is a valid ID
// distinct from "Previous is nil"; the round-trip must preserve the
// distinction (key-3 present, value is the empty-string varint-length).
func TestItemRoundTripEmptyID(t *testing.T) {
	in := Item{ID: "x", Previous: &Item{ID: ""}}
	var w gsbm.Writer
	if err := in.MarshalGSBM(&w); err != nil {
		t.Fatalf("MarshalGSBM: %v", err)
	}
	r := gsbm.NewReader(w.Bytes())
	var out Item
	if err := out.UnmarshalGSBM(r); err != nil {
		t.Fatalf("UnmarshalGSBM: %v", err)
	}
	if out.Previous == nil {
		t.Fatalf("Previous = nil, want non-nil &Item{ID:\"\"}")
	}
	if out.Previous.ID != "" {
		t.Fatalf("Previous.ID = %q, want \"\"", out.Previous.ID)
	}
}

// TestItemWireShapePrevious pins the bytes emitted for the Previous
// field. The wire payload for an id_ref is a leaf scalar: the field key
// (tag 3, LENGTH_DELIM wire type because the target's bin:"1" is a
// string) followed by the value-only encoding of that string. No
// presence byte; no inner Item body. This is the diff-visible shape
// change the spec subsection documents.
func TestItemWireShapePrevious(t *testing.T) {
	in := Item{ID: "x", Previous: &Item{ID: "abc"}}
	var w gsbm.Writer
	if err := in.MarshalGSBM(&w); err != nil {
		t.Fatalf("MarshalGSBM: %v", err)
	}
	// Expected bytes:
	//   key tag=1 (LENGTH_DELIM): (1<<3)|2 = 0x0A
	//   "x":   len=1, byte 'x'
	//   key tag=2 (LENGTH_DELIM): (2<<3)|2 = 0x12
	//   "":    len=0
	//   key tag=3 (LENGTH_DELIM): (3<<3)|2 = 0x1A
	//   "abc": len=3, "abc"
	want := []byte{
		0x0A, 0x01, 'x',
		0x12, 0x00,
		0x1A, 0x03, 'a', 'b', 'c',
	}
	if !bytes.Equal(w.Bytes(), want) {
		t.Fatalf("wire bytes mismatch:\n got: % x\nwant: % x", w.Bytes(), want)
	}
}

// TestItemWireShapeNilPreviousOmitted asserts a nil Previous emits no
// key on the wire — the decoder restores nil purely by the case branch
// for tag 3 never running.
func TestItemWireShapeNilPreviousOmitted(t *testing.T) {
	in := Item{ID: "x"}
	var w gsbm.Writer
	if err := in.MarshalGSBM(&w); err != nil {
		t.Fatalf("MarshalGSBM: %v", err)
	}
	for _, b := range w.Bytes() {
		// Tag-3 key with wire-type 2 is 0x1A; any byte with the top of the
		// varint key shifted (3<<3 = 24) would have 0x18 in the low bits.
		if b == 0x1A {
			t.Fatalf("nil Previous: found tag-3 key byte 0x1A in wire output % x", w.Bytes())
		}
	}
}

// TestItemResetClearsPrevious confirms Reset zeroes the Previous pointer
// so a re-decoded Item never carries stale id_ref state from a prior
// decode pass.
func TestItemResetClearsPrevious(t *testing.T) {
	v := Item{ID: "x", Previous: &Item{ID: "p"}}
	v.Reset()
	if v.ID != "" || v.Label != "" {
		t.Fatalf("Reset left primitive fields populated: %+v", v)
	}
	if v.Previous != nil {
		t.Fatalf("Reset left Previous non-nil: %+v", v.Previous)
	}
	if v.FieldPresent(3) {
		t.Fatalf("Reset left FieldPresent(3) true")
	}
}
