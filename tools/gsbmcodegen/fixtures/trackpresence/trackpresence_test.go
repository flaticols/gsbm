package trackpresence_test

import (
	"testing"

	"go.flaticols.dev/gsbm/storage/gsbm"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/fixtures/trackpresence"
)

func encodeBody(t *testing.T, v *trackpresence.Offer) []byte {
	t.Helper()
	w := gsbm.NewWriter(nil)
	if err := v.MarshalGSBM(w); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := w.Err(); err != nil {
		t.Fatalf("writer err: %v", err)
	}
	return w.Bytes()
}

// TestTrackPresenceRoundTrip checks the wire format is unchanged: encode
// then decode produces an identical (excluding the hidden gsbmPresent field)
// receiver. The bitmap is a Go-side concern only.
func TestTrackPresenceRoundTrip(t *testing.T) {
	note := "first try"
	orig := &trackpresence.Offer{
		ID:       "offer-1",
		Quantity: 42,
		Price:    19.99,
		Active:   true,
		Note:     &note,
	}
	body := encodeBody(t, orig)

	got := &trackpresence.Offer{}
	if err := gsbm.DecodeBodyInto(body, got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Wire-format-relevant fields round-trip; the hidden bitmap field is
	// populated only on the decode side and is not part of the on-wire
	// value semantics, so compare the fields explicitly.
	if got.ID != orig.ID || got.Quantity != orig.Quantity || got.Price != orig.Price || got.Active != orig.Active {
		t.Fatalf("scalar mismatch: got %+v want %+v", got, orig)
	}
	if got.Note == nil || *got.Note != *orig.Note {
		t.Fatalf("note mismatch: got %v want %q", got.Note, *orig.Note)
	}
}

// TestFieldPresentReflectsWireTags verifies that after decode, FieldPresent
// returns true for exactly the tags that appeared on the wire. Tags absent
// from the encoded payload — including missing optionals and zero-elided
// scalars whose source omitted them entirely — return false.
func TestFieldPresentReflectsWireTags(t *testing.T) {
	// Encode an Offer with every field populated; every tag appears on
	// the wire, so FieldPresent must return true for every declared tag.
	note := "x"
	full := &trackpresence.Offer{
		ID:       "i",
		Quantity: 1,
		Price:    1,
		Active:   true,
		Note:     &note,
	}
	body := encodeBody(t, full)
	got := &trackpresence.Offer{}
	if err := gsbm.DecodeBodyInto(body, got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for tag := uint32(1); tag <= 5; tag++ {
		if !got.FieldPresent(tag) {
			t.Errorf("tag %d: FieldPresent=false, want true (all fields populated)", tag)
		}
	}

	// Sanity: unknown tags above the declared set return false.
	if got.FieldPresent(0) {
		t.Errorf("tag 0: FieldPresent=true, want false (tag 0 is reserved)")
	}
	if got.FieldPresent(99) {
		t.Errorf("tag 99: FieldPresent=true, want false (above declared range)")
	}
	if got.FieldPresent(1024) {
		t.Errorf("tag 1024: FieldPresent=true, want false (well above [1]uint64 cap)")
	}
}

// TestFieldPresentZeroValuesWithTagOnWire confirms a zero-valued required
// scalar is still recorded as present, because MarshalGSBM emits the tag
// unconditionally for required fields. The wire-derived presence bit is
// what distinguishes "field omitted by sender" from "field set to zero by
// sender" for required fields after a future schema migration adds an
// optional fallback.
func TestFieldPresentZeroValuesWithTagOnWire(t *testing.T) {
	zero := &trackpresence.Offer{} // every required scalar is zero
	body := encodeBody(t, zero)
	got := &trackpresence.Offer{}
	if err := gsbm.DecodeBodyInto(body, got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Tags 1..4 are required scalars; the marshaler emits them even when
	// zero, so the decoder marks them present.
	for _, tag := range []uint32{1, 2, 3, 4} {
		if !got.FieldPresent(tag) {
			t.Errorf("tag %d: FieldPresent=false on zero-value required field; want true", tag)
		}
	}
	// Tag 5 (Note *string) is wrapped in an optional envelope; the
	// marshaler still emits the tag even when Note==nil (with PresenceNil
	// in the body), so the decoder records it as present even though the
	// pointee is nil.
	if !got.FieldPresent(5) {
		t.Errorf("tag 5: FieldPresent=false; the optional envelope was on the wire")
	}
}

// TestResetClearsPresence confirms Reset zeroes the gsbmPresent bitmap so
// re-querying FieldPresent on the reset receiver returns false for every
// tag, exactly like the post-Reset contract on the default sidecar path.
func TestResetClearsPresence(t *testing.T) {
	note := "x"
	v := &trackpresence.Offer{ID: "id", Note: &note}
	body := encodeBody(t, v)
	got := &trackpresence.Offer{}
	if err := gsbm.DecodeBodyInto(body, got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.FieldPresent(1) {
		t.Fatalf("precondition: tag 1 should be present before Reset")
	}
	got.Reset()
	for tag := uint32(1); tag <= 5; tag++ {
		if got.FieldPresent(tag) {
			t.Errorf("tag %d: FieldPresent=true after Reset; bitmap should be zeroed", tag)
		}
	}
}

// TestSidecarNotUsedForTrackedTypes confirms that decoding into a
// //gsbm:track-presence receiver never touches the package-level presence
// sidecar — the entire point of the opt-in is to avoid the sync.Map cost
// for marked types. We drain the sidecar, decode, and verify no entry
// landed in it.
func TestSidecarNotUsedForTrackedTypes(t *testing.T) {
	//nolint:staticcheck // intentional: deliberately exercising the deprecated sidecar to assert the opt-in path bypasses it.
	gsbm.ResetPresenceStore()

	note := "x"
	v := &trackpresence.Offer{ID: "id", Note: &note}
	body := encodeBody(t, v)
	got := &trackpresence.Offer{}
	if err := gsbm.DecodeBodyInto(body, got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// gsbm.IsPresent (the sidecar lookup) MUST return false because the
	// tracked path bypasses the sidecar entirely. FieldPresent (the
	// generated method) reads from v.gsbmPresent and returns true. The
	// two diverging is the entire point of the opt-in.
	//nolint:staticcheck // intentional: see test docstring; the assertion is precisely about sidecar non-use.
	if gsbm.IsPresent(got, 1) {
		t.Errorf("sidecar IsPresent(tag=1) returned true; tracked types must bypass the sidecar")
	}
	if !got.FieldPresent(1) {
		t.Errorf("FieldPresent(tag=1) returned false; the local bitmap should have it")
	}
}

// TestUnsetFieldPresenceIsFalse is the read-side guarantee that the bitmap
// records exactly what was on the wire — a tag that never appeared yields
// FieldPresent=false even after a partial decode. We feed the decoder a
// body that contains only tag 1 (by hand-crafting a minimal Offer that
// encodes a subset of fields via a partial round-trip).
func TestUnsetFieldPresenceIsFalse(t *testing.T) {
	// Encode only tag 1 by serializing a populated Offer and slicing the
	// body to drop fields would be brittle; easier to use a second Offer
	// that doesn't set Note explicitly — but every other required field
	// still gets emitted because they're required. Instead, route through
	// the writer manually to produce a body with only tag 1 set.
	w := gsbm.NewWriter(nil)
	w.WriteTag(1, gsbm.WireLengthDelim)
	w.WriteString("only-id")
	if err := w.Err(); err != nil {
		t.Fatalf("writer err: %v", err)
	}
	got := &trackpresence.Offer{}
	if err := gsbm.DecodeBodyInto(w.Bytes(), got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.FieldPresent(1) {
		t.Fatalf("tag 1 should be present")
	}
	for _, tag := range []uint32{2, 3, 4, 5} {
		if got.FieldPresent(tag) {
			t.Errorf("tag %d: FieldPresent=true but never appeared on the wire", tag)
		}
	}
}

// TestReDecodeClearsStalePresence ensures a re-decode into the same
// receiver clears the previous bitmap before recording the new tags. A
// stale bit from the first decode would otherwise survive a partial second
// blob and falsely report a missing field as present.
func TestReDecodeClearsStalePresence(t *testing.T) {
	// First decode: every tag present.
	note := "x"
	full := &trackpresence.Offer{ID: "i", Quantity: 1, Price: 1, Active: true, Note: &note}
	got := &trackpresence.Offer{}
	if err := gsbm.DecodeBodyInto(encodeBody(t, full), got); err != nil {
		t.Fatalf("decode 1: %v", err)
	}

	// Second decode: body has only tag 1. Stale bits from the first
	// decode must be cleared so tags 2..5 read false.
	w := gsbm.NewWriter(nil)
	w.WriteTag(1, gsbm.WireLengthDelim)
	w.WriteString("only-id")
	if err := gsbm.DecodeBodyInto(w.Bytes(), got); err != nil {
		t.Fatalf("decode 2: %v", err)
	}
	if !got.FieldPresent(1) {
		t.Errorf("tag 1: FieldPresent=false after re-decode; want true")
	}
	for _, tag := range []uint32{2, 3, 4, 5} {
		if got.FieldPresent(tag) {
			t.Errorf("tag %d: FieldPresent=true after re-decode with only tag 1; stale bitmap leaked", tag)
		}
	}
}

