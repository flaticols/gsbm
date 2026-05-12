package gsbm

import (
	"errors"
	"testing"
)

// TestPresenceNilRoundTrip exercises spec §5.1 state 0b00.
func TestPresenceNilRoundTrip(t *testing.T) {
	w := NewWriter(nil)
	w.WritePresenceNil()
	if w.Err() != nil {
		t.Fatal(w.Err())
	}
	if got := w.Bytes(); len(got) != 1 || got[0] != 0x00 {
		t.Fatalf("nil presence bytes: % x", got)
	}
	r := NewReader(w.Bytes())
	state, err := r.ReadPresenceByte(true)
	if err != nil {
		t.Fatal(err)
	}
	if state != PresenceNil {
		t.Fatalf("got state %v want PresenceNil", state)
	}
	if r.HasMore() {
		t.Fatal("HasMore after lone nil presence byte")
	}
}

// TestPresenceNonZeroRoundTrip covers 0b01 — payload-follows form. We
// piggyback an int64 value to confirm the presence byte is independent of
// the payload (spec §5.1: "presence byte is part of the field value").
func TestPresenceNonZeroRoundTrip(t *testing.T) {
	w := NewWriter(nil)
	w.WritePresenceNonZero()
	w.WriteVarint(-7)
	if w.Err() != nil {
		t.Fatal(w.Err())
	}
	if got := w.Bytes()[0]; got != 0x01 {
		t.Fatalf("nonzero presence byte: %02x", got)
	}
	r := NewReader(w.Bytes())
	state, err := r.ReadPresenceByte(false) // works for both builtins and named
	if err != nil {
		t.Fatal(err)
	}
	if state != PresenceNonZero {
		t.Fatalf("got state %v want PresenceNonZero", state)
	}
	v, err := r.ReadVarint()
	if err != nil || v != -7 {
		t.Fatalf("payload v=%d err=%v", v, err)
	}
}

// TestPresenceZeroRoundTripBuiltin covers 0b11 against a builtin-eligible
// reader (allowZeroElide=true).
func TestPresenceZeroRoundTripBuiltin(t *testing.T) {
	w := NewWriter(nil)
	w.WritePresenceZero()
	if w.Err() != nil {
		t.Fatal(w.Err())
	}
	if got := w.Bytes(); len(got) != 1 || got[0] != 0x03 {
		t.Fatalf("zero-elided presence bytes: % x", got)
	}
	r := NewReader(w.Bytes())
	state, err := r.ReadPresenceByte(true)
	if err != nil {
		t.Fatal(err)
	}
	if state != PresenceZero {
		t.Fatalf("got state %v want PresenceZero", state)
	}
	if r.HasMore() {
		t.Fatal("HasMore after zero-elided presence byte (no payload should follow)")
	}
}

// TestPresenceZeroRejectedForNamed enforces the eligibility rule from
// §5.1: a decoder for a named/user-defined type MUST treat 0b11 as
// malformed.
func TestPresenceZeroRejectedForNamed(t *testing.T) {
	r := NewReader([]byte{0x03})
	if _, err := r.ReadPresenceByte(false); !errors.Is(err, ErrInvalidPresence) {
		t.Fatalf("named type w/ zero-elide: want ErrInvalidPresence, got %v", err)
	}
}

// TestPresenceReservedStateRejected rejects 0b10 in both eligibility modes
// — it is reserved in fmtVer 2.
func TestPresenceReservedStateRejected(t *testing.T) {
	for _, allow := range []bool{true, false} {
		r := NewReader([]byte{0x02})
		if _, err := r.ReadPresenceByte(allow); !errors.Is(err, ErrInvalidPresence) {
			t.Fatalf("allowZeroElide=%v: want ErrInvalidPresence for 0b10, got %v", allow, err)
		}
	}
}

// TestPresenceReservedUpperBitsRejected covers spec §5.1's "bits 2..7
// reserved, MUST be 0" — including the dirty bit 2.
func TestPresenceReservedUpperBitsRejected(t *testing.T) {
	cases := []byte{
		0x04, // bit 2 (dirty) set
		0x08, // bit 3 set
		0x80, // bit 7 set
		0xFD, // many reserved bits set
	}
	for _, b := range cases {
		r := NewReader([]byte{b})
		if _, err := r.ReadPresenceByte(true); !errors.Is(err, ErrInvalidPresence) {
			t.Fatalf("byte %02x: want ErrInvalidPresence, got %v", b, err)
		}
	}
}

// TestPresenceTruncated: empty input must surface as ErrTruncated.
func TestPresenceTruncated(t *testing.T) {
	r := NewReader(nil)
	if _, err := r.ReadPresenceByte(true); !errors.Is(err, ErrTruncated) {
		t.Fatalf("want ErrTruncated, got %v", err)
	}
}

// TestPresenceWriterStickyError: a writer in error state must not emit any
// presence byte, mirroring the rest of the writer API.
func TestPresenceWriterStickyError(t *testing.T) {
	w := NewWriter(nil)
	w.WriteTag(0, WireVarint) // pre-existing sticky error
	if w.Err() == nil {
		t.Fatal("expected sticky error")
	}
	w.WritePresenceNil()
	w.WritePresenceNonZero()
	w.WritePresenceZero()
	if len(w.Bytes()) != 0 {
		t.Fatalf("writer emitted bytes after sticky error: % x", w.Bytes())
	}
}

// TestPresenceEligibilityRule documents — at the runtime layer — how
// codegen wires zero-elision detection at encode time only for builtin
// types. For each builtin, zero-valued *T encodes to PresenceZero (1
// byte, no payload). For a non-builtin pointer the codegen-equivalent
// output is PresenceNonZero followed by the payload, never PresenceZero.
func TestPresenceEligibilityRule(t *testing.T) {
	// Builtin (int64) zero-elide path — mirrors what codegen will emit
	// for *int64 fields.
	encodeBuiltinNullableInt64 := func(p *int64) []byte {
		w := NewWriter(nil)
		switch {
		case p == nil:
			w.WritePresenceNil()
		case *p == 0:
			w.WritePresenceZero()
		default:
			w.WritePresenceNonZero()
			w.WriteVarint(*p)
		}
		if w.Err() != nil {
			t.Fatal(w.Err())
		}
		return w.Bytes()
	}
	// Non-builtin (named) path — the encoder MUST NOT zero-elide.
	type named struct{ N int64 }
	encodeNamedNullable := func(p *named) []byte {
		w := NewWriter(nil)
		if p == nil {
			w.WritePresenceNil()
		} else {
			w.WritePresenceNonZero()
			// Tagged body is wire concern, but for this test a single varint
			// payload stands in for "the named type's encoding."
			w.WriteVarint(p.N)
		}
		if w.Err() != nil {
			t.Fatal(w.Err())
		}
		return w.Bytes()
	}

	zero := int64(0)
	if got := encodeBuiltinNullableInt64(&zero); len(got) != 1 || got[0] != 0x03 {
		t.Fatalf("builtin zero ptr: got % x want 03", got)
	}
	nonzero := int64(42)
	if got := encodeBuiltinNullableInt64(&nonzero); len(got) == 0 || got[0] != 0x01 {
		t.Fatalf("builtin nonzero ptr first byte: got %02x want 01", got[0])
	}
	if got := encodeBuiltinNullableInt64(nil); len(got) != 1 || got[0] != 0x00 {
		t.Fatalf("builtin nil ptr: got % x want 00", got)
	}

	if got := encodeNamedNullable(&named{N: 0}); got[0] != 0x01 {
		t.Fatalf("named zero ptr first byte: got %02x — must NOT zero-elide", got[0])
	}
	if got := encodeNamedNullable(nil); len(got) != 1 || got[0] != 0x00 {
		t.Fatalf("named nil ptr: got % x want 00", got)
	}

	// Round-trip: builtin decoder accepts zero-elide; named decoder rejects.
	zeroElide := []byte{0x03}
	if _, err := NewReader(zeroElide).ReadPresenceByte(true); err != nil {
		t.Fatalf("builtin must accept zero-elide: %v", err)
	}
	if _, err := NewReader(zeroElide).ReadPresenceByte(false); !errors.Is(err, ErrInvalidPresence) {
		t.Fatalf("named must reject zero-elide: %v", err)
	}
}
