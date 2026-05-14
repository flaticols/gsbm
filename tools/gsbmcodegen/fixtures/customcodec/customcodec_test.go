package customcodec

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	"go.flaticols.dev/gsbm/storage/gsbm"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/codecs/builtins"
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
		CreatedAt:    then,
		Amount:       DecimalAmount{Integer: "12", Fraction: "500"},
		OptionalAt:   &opt,
		AmountAppend: DecimalAmount{Negative: true, Integer: "7", Fraction: "25"},
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
	if !reflect.DeepEqual(out.AmountAppend, in.AmountAppend) {
		t.Errorf("AmountAppend: got %+v, want %+v", out.AmountAppend, in.AmountAppend)
	}
	if out.OptionalAt == nil {
		t.Fatalf("OptionalAt = nil, want non-nil")
	}
	if out.OptionalAt.UnixNano() != opt.UnixNano() {
		t.Errorf("OptionalAt instant: got %d, want %d", out.OptionalAt.UnixNano(), opt.UnixNano())
	}
}

// TestRecordRoundTripNilOptional confirms the presence-byte wrapper:
// OptionalAt = nil round-trips as nil (PresenceNil on the wire). Record
// is a default-mode type (no //gsbm:track-presence marker), so the
// local bitmap dies with the decode call and FieldPresent(3) reports
// false even though the field key appeared on the wire. The wire-level
// presence is checked by the nil pointer assertion above.
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
	if out.FieldPresent(3) {
		t.Fatalf("FieldPresent(3) = true; default-mode receivers must report tags absent post-decode")
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

// TestRecordRoundTripPreEpoch covers a pre-1970 timestamp whose seconds
// component is negative. The Time codec encodes Unix seconds via zigzag
// WriteVarint, so negatives must round-trip without truncation.
func TestRecordRoundTripPreEpoch(t *testing.T) {
	pre1970 := time.Unix(-12345, 67890).UTC()
	in := Record{
		CreatedAt: pre1970,
		Amount:    DecimalAmount{Negative: true, Integer: "1", Fraction: "0"},
	}
	buf := encode(t, in)
	var out Record
	if err := out.UnmarshalGSBM(gsbm.NewReader(buf)); err != nil {
		t.Fatalf("UnmarshalGSBM: %v", err)
	}
	if !out.CreatedAt.Equal(pre1970) {
		t.Errorf("CreatedAt instant: got %v, want %v", out.CreatedAt, pre1970)
	}
	if !reflect.DeepEqual(out.Amount, in.Amount) {
		t.Errorf("Amount: got %+v, want %+v", out.Amount, in.Amount)
	}
}

// TestRecordRoundTripZeroTime is the issue-#21 regression. time.Time{} —
// the year-1-AD UTC zero — is far outside the int64-nanosecond range and
// would be silently corrupted by a UnixNano-based codec. The Time codec
// stores (Unix seconds, Nanosecond) and must round-trip the zero value
// such that decoded.Equal(time.Time{}) is true.
func TestRecordRoundTripZeroTime(t *testing.T) {
	in := Record{
		CreatedAt: time.Time{},
		Amount:    DecimalAmount{Integer: "0"},
	}
	buf := encode(t, in)
	var out Record
	if err := out.UnmarshalGSBM(gsbm.NewReader(buf)); err != nil {
		t.Fatalf("UnmarshalGSBM: %v", err)
	}
	if !out.CreatedAt.Equal(time.Time{}) {
		t.Errorf("CreatedAt: got %v, want time.Time{} (Equal)", out.CreatedAt)
	}
	if !out.CreatedAt.IsZero() {
		t.Errorf("CreatedAt.IsZero() = false, want true (got %v)", out.CreatedAt)
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
// Record under the new Time codec. The codec body is
// `varint(Unix) ++ uvarint(Nanosecond)`; codegen wraps that body in the
// LENGTH_DELIM envelope (key + length prefix) so older readers can
// SkipField past the field. The hand-crafted `want` builder mirrors what
// the generated MarshalGSBM produces, byte for byte.
//
// Layout:
//
//	tag 1 (CreatedAt, codec wire = LENDLM):    key (1<<3)|2, length prefix, body
//	                                            body: varint(seconds) ++ uvarint(nanos)
//	tag 2 (Amount,    codec wire = LENDLM):    key (2<<3)|2, length-prefixed string
//	tag 3 (OptionalAt envelope, LENDLM):       key (3<<3)|2, length-prefixed body
//	                                            body: presence byte = Nil
//	tag 4 (AmountAppend, codec wire = LENDLM): key (4<<3)|2, length-prefixed bytes
//	                                            wire-identical to the DecimalString
//	                                            form for the same value (AppendText
//	                                            and String produce equal text).
func TestRecordWireBytes(t *testing.T) {
	in := Record{
		CreatedAt:    time.Unix(1, 0),
		Amount:       DecimalAmount{Integer: "3"},
		AmountAppend: DecimalAmount{Integer: "7"},
	}
	buf := encode(t, in)

	var w gsbm.Writer
	w.WriteTag(1, gsbm.WireLengthDelim)
	w.WriteUvarint(uint64(gsbm.SizeVarint(int64(1)) + gsbm.SizeUvarint(uint64(0))))
	w.WriteVarint(int64(1))
	w.WriteUvarint(uint64(0))
	w.WriteTag(2, gsbm.WireLengthDelim)
	w.WriteString("3")
	w.WriteTag(3, gsbm.WireLengthDelim)
	m := w.BeginLengthDelim()
	w.WritePresenceNil()
	w.EndLengthDelim(m)
	w.WriteTag(4, gsbm.WireLengthDelim)
	w.WriteString("7")
	want := w.Bytes()
	if !bytes.Equal(buf, want) {
		t.Fatalf("wire bytes mismatch:\n got: % x\nwant: % x", buf, want)
	}
}

// TestRecordWireBytesPresentOptional pins the byte shape of the optional
// envelope when OptionalAt is non-nil. The envelope is: outer key
// (LENGTH_DELIM), outer uvarint length, presence byte (NonZero), then the
// codec's value-payload — for the analytic LENGTH_DELIM Time codec that
// is an inner uvarint body-length followed by the body (varint(seconds)
// ++ uvarint(nanos)). Mirrors the value-case shape (key ++ length ++
// body) so analytic and materializing custom codecs are wire-identical
// for *T per spec §5.8.
func TestRecordWireBytesPresentOptional(t *testing.T) {
	opt := time.Unix(2, 0)
	in := Record{
		CreatedAt:    time.Unix(1, 0),
		Amount:       DecimalAmount{Integer: "0"},
		OptionalAt:   &opt,
		AmountAppend: DecimalAmount{Negative: true, Integer: "1", Fraction: "5"},
	}
	buf := encode(t, in)

	var w gsbm.Writer
	w.WriteTag(1, gsbm.WireLengthDelim)
	w.WriteUvarint(uint64(gsbm.SizeVarint(int64(1)) + gsbm.SizeUvarint(uint64(0))))
	w.WriteVarint(int64(1))
	w.WriteUvarint(uint64(0))
	w.WriteTag(2, gsbm.WireLengthDelim)
	w.WriteString("0")
	w.WriteTag(3, gsbm.WireLengthDelim)
	m := w.BeginLengthDelim()
	w.WritePresenceNonZero()
	w.WriteUvarint(uint64(gsbm.SizeVarint(int64(2)) + gsbm.SizeUvarint(uint64(0))))
	w.WriteVarint(int64(2))
	w.WriteUvarint(uint64(0))
	w.EndLengthDelim(m)
	w.WriteTag(4, gsbm.WireLengthDelim)
	w.WriteString("-1.5")
	want := w.Bytes()
	if !bytes.Equal(buf, want) {
		t.Fatalf("wire bytes mismatch:\n got: % x\nwant: % x", buf, want)
	}
}

// TestRecordResetClearsAllFields exercises the generated Reset for a
// custom-codec field. Reset must zero CreatedAt to the time.Time zero
// value, zero the DecimalAmount struct, and drop OptionalAt to nil.
// Record is a default-mode type (no //gsbm:track-presence marker), so
// FieldPresent already returns false post-decode; the Reset assertion
// here just doubles as a no-leak guard for the legacy sidecar surface.
// probeDecimal is a DecimalAmount sibling that counts how many times
// String() and AppendText() are invoked. It is what the materializing-
// codec scratch cache should run exactly once per occurrence across the
// gsbm.Marshal size/write hand-off.
type probeDecimal struct {
	d           DecimalAmount
	stringCalls *int
	appendCalls *int
}

func (p probeDecimal) String() string {
	*p.stringCalls++
	return p.d.String()
}

func (p probeDecimal) AppendText(dst []byte) ([]byte, error) {
	*p.appendCalls++
	return p.d.AppendText(dst)
}

// probeRecord mirrors Record's two materializing-codec fields against
// probeDecimal so the test can observe the per-pass materialization
// counts. It uses the same callsite constants the generator emits for
// Record (csRecord_2 / csRecord_4) so the cache shape matches.
type probeRecord struct {
	a probeDecimal
	b probeDecimal
}

func (p *probeRecord) SizeGSBM() int {
	cw := gsbm.NewCountingWriter()
	_ = p.MarshalGSBM(cw)
	return cw.Size()
}

func (p *probeRecord) MarshalGSBM(w *gsbm.Writer) error {
	w.WriteTag(2, gsbm.WireLengthDelim)
	if err := builtins.EmitDecimalString(w, p.a, csRecord_2); err != nil {
		return err
	}
	w.WriteTag(4, gsbm.WireLengthDelim)
	if err := builtins.EmitDecimalAppend(w, p.b, csRecord_4); err != nil {
		return err
	}
	return w.Err()
}

// TestMaterializeOnceBothFlavors asserts the materialize-once invariant
// holds across both materializing-codec paths at fixture scope: a single
// gsbm.Marshal call invokes the string-form codec's String() exactly
// once and the append-form codec's AppendText() exactly once per
// occurrence, even though the two-pass flow visits each field in both
// the size pass and the write pass.
func TestMaterializeOnceBothFlavors(t *testing.T) {
	aStr, aApp := 0, 0
	bStr, bApp := 0, 0
	p := &probeRecord{
		a: probeDecimal{d: DecimalAmount{Integer: "12", Fraction: "500"}, stringCalls: &aStr, appendCalls: &aApp},
		b: probeDecimal{d: DecimalAmount{Negative: true, Integer: "7"}, stringCalls: &bStr, appendCalls: &bApp},
	}
	if _, err := gsbm.Marshal(p, 0); err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if aStr != 1 {
		t.Errorf("string-codec field: String() invocations = %d, want 1", aStr)
	}
	if aApp != 0 {
		t.Errorf("string-codec field: AppendText() invocations = %d, want 0", aApp)
	}
	if bApp != 1 {
		t.Errorf("append-codec field: AppendText() invocations = %d, want 1", bApp)
	}
	if bStr != 0 {
		t.Errorf("append-codec field: String() invocations = %d, want 0", bStr)
	}
}

// TestRecordWireBytesAppendMatchesString proves the new DecimalAppend
// codec produces byte-identical wire output to DecimalString for the
// same logical value. The fixture's String and AppendText methods on
// DecimalAmount return the same text by construction; this test pins
// that wire equivalence end-to-end via gsbm.Marshal, so any future
// drift between the two codec paths surfaces as a hard byte mismatch.
func TestRecordWireBytesAppendMatchesString(t *testing.T) {
	cases := []DecimalAmount{
		{Integer: "0"},
		{Integer: "42"},
		{Negative: true, Integer: "1", Fraction: "5"},
		{Integer: "100", Fraction: "000"},
	}
	for _, d := range cases {
		stringSide := Record{
			CreatedAt: time.Unix(1, 0),
			Amount:    d,
		}
		appendSide := Record{
			CreatedAt:    time.Unix(1, 0),
			AmountAppend: d,
		}
		stringBuf := encode(t, stringSide)
		appendBuf := encode(t, appendSide)
		// Strip the differing tag prefix to compare bodies: locate tag 2
		// in stringBuf and tag 4 in appendBuf, then compare the
		// length-prefixed string payloads byte-for-byte.
		stringPayload, ok := payloadForTag(stringBuf, 2)
		if !ok {
			t.Fatalf("tag 2 not found in stringBuf: % x", stringBuf)
		}
		appendPayload, ok := payloadForTag(appendBuf, 4)
		if !ok {
			t.Fatalf("tag 4 not found in appendBuf: % x", appendBuf)
		}
		if !bytes.Equal(stringPayload, appendPayload) {
			t.Errorf("DecimalString vs DecimalAppend wire payloads diverge for %q:\n string=% x\n append=% x", d.String(), stringPayload, appendPayload)
		}
	}
}

// payloadForTag locates the LENGTH_DELIM field with the given tag in buf
// and returns the bytes of its length-prefixed body (the content, not the
// length prefix). Used by the wire-equivalence test to compare codec
// outputs irrespective of the tag they occupy in the surrounding record.
func payloadForTag(buf []byte, want uint32) ([]byte, bool) {
	r := gsbm.NewReader(buf)
	for r.HasMore() {
		tag, wt, err := r.ReadTag()
		if err != nil {
			return nil, false
		}
		if wt != gsbm.WireLengthDelim {
			return nil, false
		}
		b, err := r.ReadBytes()
		if err != nil {
			return nil, false
		}
		if tag == want {
			return b, true
		}
	}
	return nil, false
}

func TestRecordResetClearsAllFields(t *testing.T) {
	opt := time.Unix(2, 0)
	in := Record{
		CreatedAt:    time.Unix(1, 0),
		Amount:       DecimalAmount{Integer: "42"},
		OptionalAt:   &opt,
		AmountAppend: DecimalAmount{Integer: "99"},
	}
	buf := encode(t, in)
	var v Record
	if err := v.UnmarshalGSBM(gsbm.NewReader(buf)); err != nil {
		t.Fatalf("UnmarshalGSBM: %v", err)
	}
	v.Reset()
	if !v.CreatedAt.IsZero() {
		t.Errorf("CreatedAt = %v, want zero", v.CreatedAt)
	}
	if v.Amount != (DecimalAmount{}) {
		t.Errorf("Amount = %+v, want zero", v.Amount)
	}
	if v.AmountAppend != (DecimalAmount{}) {
		t.Errorf("AmountAppend = %+v, want zero", v.AmountAppend)
	}
	if v.OptionalAt != nil {
		t.Errorf("OptionalAt = %v, want nil", *v.OptionalAt)
	}
	for _, tag := range []uint32{1, 2, 3, 4} {
		if v.FieldPresent(tag) {
			t.Errorf("FieldPresent(%d) = true after Reset", tag)
		}
	}
}
