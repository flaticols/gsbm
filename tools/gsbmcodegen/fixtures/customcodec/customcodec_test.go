package customcodec

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	"go.flaticols.dev/gsbm/storage/gsbm"
)

// encode marshals in into a fresh Writer. Tests that need to inspect the
// wire bytes call this directly; tests that need a decode side declare
// `out` locally so the presence sidecar (which keys by receiver address)
// stays valid for FieldPresent checks at the caller's scope.
func encode(t *testing.T, in Record) []byte {
	t.Helper()
	var w gsbm.Writer
	if err := in.MarshalGSBM(&w); err != nil {
		t.Fatalf("MarshalGSBM: %v", err)
	}
	return append([]byte(nil), w.Bytes()...)
}

// TestRecordRoundTrip exercises the canonical happy path: a non-nil
// OptionalAt, a non-zero Amount, and a known CreatedAt. The decoded
// record must compare equal on instant + value.
func TestRecordRoundTrip(t *testing.T) {
	then := time.Unix(1_700_000_000, 123_456_789).UTC()
	opt := time.Unix(1_700_000_001, 0).UTC()
	in := Record{
		CreatedAt:  then,
		Amount:     DecimalAmount{Integer: "12", Fraction: "500"},
		OptionalAt: &opt,
	}
	buf := encode(t, in)
	var out Record
	if err := out.UnmarshalGSBM(gsbm.NewReader(buf)); err != nil {
		t.Fatalf("UnmarshalGSBM: %v", err)
	}
	if out.CreatedAt.UnixNano() != then.UnixNano() {
		t.Errorf("CreatedAt instant: got %d, want %d", out.CreatedAt.UnixNano(), then.UnixNano())
	}
	if !reflect.DeepEqual(out.Amount, in.Amount) {
		t.Errorf("Amount: got %+v, want %+v", out.Amount, in.Amount)
	}
	if out.OptionalAt == nil {
		t.Fatalf("OptionalAt = nil, want non-nil")
	}
	if out.OptionalAt.UnixNano() != opt.UnixNano() {
		t.Errorf("OptionalAt instant: got %d, want %d", out.OptionalAt.UnixNano(), opt.UnixNano())
	}
}

// TestRecordRoundTripNilOptional confirms the presence-byte wrapper:
// OptionalAt = nil round-trips as nil (PresenceNil on the wire) but the
// field key is still on the wire, so FieldPresent(3) is true.
func TestRecordRoundTripNilOptional(t *testing.T) {
	in := Record{
		CreatedAt: time.Unix(0, 0).UTC(),
		Amount:    DecimalAmount{Integer: "0"},
	}
	buf := encode(t, in)
	var out Record
	if err := out.UnmarshalGSBM(gsbm.NewReader(buf)); err != nil {
		t.Fatalf("UnmarshalGSBM: %v", err)
	}
	if out.OptionalAt != nil {
		t.Fatalf("OptionalAt = %v, want nil", *out.OptionalAt)
	}
	if !out.FieldPresent(3) {
		t.Fatalf("FieldPresent(3) = false, want true (PresenceNil is still present on wire)")
	}
}

// TestRecordRoundTripPresentZero exercises the non-nil-pointer-with-zero-
// value case. Spec §5.1 forbids PresenceZero on the wire for non-builtin
// payloads, so the encoder must emit PresenceNonZero and the codec runs
// against the zero time.Time. Decoded value is a non-nil pointer whose
// UnixNano matches the input's UnixNano.
func TestRecordRoundTripPresentZero(t *testing.T) {
	z := time.Unix(0, 0).UTC()
	in := Record{
		CreatedAt:  time.Unix(1_700_000_000, 0).UTC(),
		Amount:     DecimalAmount{Integer: "0"},
		OptionalAt: &z,
	}
	buf := encode(t, in)
	var out Record
	if err := out.UnmarshalGSBM(gsbm.NewReader(buf)); err != nil {
		t.Fatalf("UnmarshalGSBM: %v", err)
	}
	if out.OptionalAt == nil {
		t.Fatalf("OptionalAt = nil, want non-nil pointer to zero time")
	}
	if out.OptionalAt.UnixNano() != z.UnixNano() {
		t.Fatalf("OptionalAt instant = %d, want %d", out.OptionalAt.UnixNano(), z.UnixNano())
	}
}

// TestRecordRoundTripNegativeNanos covers a pre-1970 timestamp whose
// UnixNano is negative. The TimeUnixNano codec uses zigzag VARINT via
// WriteVarint, so negatives must round-trip without truncation.
func TestRecordRoundTripNegativeNanos(t *testing.T) {
	pre1970 := time.Unix(-12345, -67890).UTC()
	in := Record{
		CreatedAt: pre1970,
		Amount:    DecimalAmount{Negative: true, Integer: "1", Fraction: "0"},
	}
	buf := encode(t, in)
	var out Record
	if err := out.UnmarshalGSBM(gsbm.NewReader(buf)); err != nil {
		t.Fatalf("UnmarshalGSBM: %v", err)
	}
	if out.CreatedAt.UnixNano() != pre1970.UnixNano() {
		t.Errorf("CreatedAt instant: got %d, want %d", out.CreatedAt.UnixNano(), pre1970.UnixNano())
	}
	if !reflect.DeepEqual(out.Amount, in.Amount) {
		t.Errorf("Amount: got %+v, want %+v", out.Amount, in.Amount)
	}
}

// TestRecordRoundTripDecimalEdgeCases pins the decimal-string behaviour
// across cases the user is most likely to break: trailing zeros, an
// integer-only value, a signed zero, and an explicit ".0". DecimalString
// does not normalize so the decoded DecimalAmount equals the input
// field-by-field.
func TestRecordRoundTripDecimalEdgeCases(t *testing.T) {
	cases := []DecimalAmount{
		{Integer: "1", Fraction: "000"},
		{Integer: "42"},
		{Negative: true, Integer: "0"},
		{Integer: "0", Fraction: "0"},
	}
	for _, amt := range cases {
		in := Record{
			CreatedAt: time.Unix(1, 0).UTC(),
			Amount:    amt,
		}
		buf := encode(t, in)
		var out Record
		if err := out.UnmarshalGSBM(gsbm.NewReader(buf)); err != nil {
			t.Fatalf("UnmarshalGSBM: %v", err)
		}
		if !reflect.DeepEqual(out.Amount, amt) {
			t.Errorf("Amount %q: round-trip got %+v, want %+v", amt.String(), out.Amount, amt)
		}
	}
}

// TestRecordWireBytes pins the exact byte sequence emitted for a known
// Record. The codec functions are direct calls into the gsbm primitive
// surface, so the wire shape MUST equal what we'd hand-craft via the
// public Writer API.
//
// Layout:
//
//	tag 1 (CreatedAt, codec wire = VARINT):  key (1<<3)|0, zigzag UnixNano
//	tag 2 (Amount,    codec wire = LENDLM):  key (2<<3)|2, length-prefixed string
//	tag 3 (OptionalAt envelope, LENDLM):     key (3<<3)|2, length-prefixed body
//	                                          body: presence byte = Nil
func TestRecordWireBytes(t *testing.T) {
	in := Record{
		CreatedAt: time.Unix(1, 0),
		Amount:    DecimalAmount{Integer: "3"},
	}
	buf := encode(t, in)

	var w gsbm.Writer
	w.WriteTag(1, gsbm.WireVarint)
	w.WriteVarint(time.Unix(1, 0).UnixNano())
	w.WriteTag(2, gsbm.WireLengthDelim)
	w.WriteString("3")
	w.WriteTag(3, gsbm.WireLengthDelim)
	m := w.BeginLengthDelim()
	w.WritePresenceNil()
	w.EndLengthDelim(m)
	want := w.Bytes()
	if !bytes.Equal(buf, want) {
		t.Fatalf("wire bytes mismatch:\n got: % x\nwant: % x", buf, want)
	}
}

// TestRecordWireBytesPresentOptional pins the byte shape of the optional
// envelope when OptionalAt is non-nil. The envelope is: outer key
// (LENGTH_DELIM), uvarint inner length, presence byte (NonZero), then the
// codec output (UnixNano as zigzag VARINT).
func TestRecordWireBytesPresentOptional(t *testing.T) {
	opt := time.Unix(2, 0)
	in := Record{
		CreatedAt:  time.Unix(1, 0),
		Amount:     DecimalAmount{Integer: "0"},
		OptionalAt: &opt,
	}
	buf := encode(t, in)

	var w gsbm.Writer
	w.WriteTag(1, gsbm.WireVarint)
	w.WriteVarint(time.Unix(1, 0).UnixNano())
	w.WriteTag(2, gsbm.WireLengthDelim)
	w.WriteString("0")
	w.WriteTag(3, gsbm.WireLengthDelim)
	m := w.BeginLengthDelim()
	w.WritePresenceNonZero()
	w.WriteVarint(time.Unix(2, 0).UnixNano())
	w.EndLengthDelim(m)
	want := w.Bytes()
	if !bytes.Equal(buf, want) {
		t.Fatalf("wire bytes mismatch:\n got: % x\nwant: % x", buf, want)
	}
}

// TestRecordResetClearsAllFields exercises the generated Reset for a
// custom-codec field. Reset must zero CreatedAt to the time.Time zero
// value, zero the DecimalAmount struct, drop OptionalAt to nil, and
// clear the presence sidecar. Presence bits are only set by
// UnmarshalGSBM, so we decode an encoded record first to populate them;
// otherwise the FieldPresent assertions would be a no-op (returning
// false because they were never true, not because Reset cleared them).
func TestRecordResetClearsAllFields(t *testing.T) {
	opt := time.Unix(2, 0)
	in := Record{
		CreatedAt:  time.Unix(1, 0),
		Amount:     DecimalAmount{Integer: "42"},
		OptionalAt: &opt,
	}
	buf := encode(t, in)
	var v Record
	if err := v.UnmarshalGSBM(gsbm.NewReader(buf)); err != nil {
		t.Fatalf("UnmarshalGSBM: %v", err)
	}
	for _, tag := range []uint32{1, 2, 3} {
		if !v.FieldPresent(tag) {
			t.Fatalf("FieldPresent(%d) = false before Reset (decode did not populate presence)", tag)
		}
	}
	v.Reset()
	if !v.CreatedAt.IsZero() {
		t.Errorf("CreatedAt = %v, want zero", v.CreatedAt)
	}
	if v.Amount != (DecimalAmount{}) {
		t.Errorf("Amount = %+v, want zero", v.Amount)
	}
	if v.OptionalAt != nil {
		t.Errorf("OptionalAt = %v, want nil", *v.OptionalAt)
	}
	for _, tag := range []uint32{1, 2, 3} {
		if v.FieldPresent(tag) {
			t.Errorf("FieldPresent(%d) = true after Reset", tag)
		}
	}
}
