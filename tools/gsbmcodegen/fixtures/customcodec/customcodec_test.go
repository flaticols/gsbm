package customcodec

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.flaticols.dev/gsbm/storage/gsbm"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/codecs/builtins"
)

// jsonBytes returns json.Marshal(v) or fails the test. Used by wire-bytes
// tests that hand-craft the streaming-codec body — the streaming codec
// writes json.Marshal(v) verbatim, so a stable golden body requires the
// same call shape.
func jsonBytes(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

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

// TestContainerRoundTrip pins the materializing-codec fallback's
// correctness end-to-end. Container nests Record by value, slice, map,
// and pointer-slice; every nested site exercises the
// BeginLengthDelim/EndLengthDelim fallback because Record carries
// DecimalString and DecimalAppend (CodecKindMaterializing). A regression
// that re-routes those sites through the analytic WriteLength path
// would mis-declare the length prefix (size run = fresh codec
// materialization, body bytes = cached materialization from the shared
// scratch) — gsbm.Marshal would still complete, but the decode side
// would either fail or read mis-aligned bytes.
func TestContainerRoundTrip(t *testing.T) {
	in := Container{
		Inner: Record{
			CreatedAt:    time.Unix(1700001000, 0).UTC(),
			Amount:       DecimalAmount{Integer: "1", Fraction: "5"},
			AmountAppend: DecimalAmount{Integer: "2", Fraction: "25"},
		},
		Items: []Record{
			{
				CreatedAt:    time.Unix(1700002000, 0).UTC(),
				Amount:       DecimalAmount{Integer: "10"},
				AmountAppend: DecimalAmount{Integer: "10"},
			},
			{
				CreatedAt:    time.Unix(1700003000, 0).UTC(),
				Amount:       DecimalAmount{Negative: true, Integer: "20", Fraction: "5"},
				AmountAppend: DecimalAmount{Negative: true, Integer: "20", Fraction: "5"},
			},
		},
		ByKey: map[string]Record{
			"alpha": {
				CreatedAt:    time.Unix(1700004000, 0).UTC(),
				Amount:       DecimalAmount{Integer: "0", Fraction: "001"},
				AmountAppend: DecimalAmount{Integer: "0", Fraction: "001"},
			},
			"beta": {
				CreatedAt:    time.Unix(1700005000, 0).UTC(),
				Amount:       DecimalAmount{Integer: "999999"},
				AmountAppend: DecimalAmount{Integer: "999999"},
			},
		},
		PtrItems: []*Record{
			nil,
			{
				CreatedAt:    time.Unix(1700006000, 0).UTC(),
				Amount:       DecimalAmount{Integer: "7", Fraction: "7"},
				AmountAppend: DecimalAmount{Integer: "7", Fraction: "7"},
			},
		},
		// Pages and Buckets exercise the transitive composite-fallback path
		// — outer slice whose element is itself a slice/map carrying a
		// materializing-codec field. Without the slice-encode composite
		// guard, the outer envelope would mis-declare its length.
		Pages: [][]Record{
			{
				{CreatedAt: time.Unix(1700007000, 0).UTC(), Amount: DecimalAmount{Integer: "11"}, AmountAppend: DecimalAmount{Integer: "11"}},
				{CreatedAt: time.Unix(1700007001, 0).UTC(), Amount: DecimalAmount{Integer: "22", Fraction: "5"}, AmountAppend: DecimalAmount{Integer: "22", Fraction: "5"}},
			},
			{
				{CreatedAt: time.Unix(1700007002, 0).UTC(), Amount: DecimalAmount{Negative: true, Integer: "3"}, AmountAppend: DecimalAmount{Negative: true, Integer: "3"}},
			},
		},
		Buckets: []map[string]Record{
			{
				"a": {CreatedAt: time.Unix(1700008000, 0).UTC(), Amount: DecimalAmount{Integer: "100"}, AmountAppend: DecimalAmount{Integer: "100"}},
				"b": {CreatedAt: time.Unix(1700008001, 0).UTC(), Amount: DecimalAmount{Integer: "200"}, AmountAppend: DecimalAmount{Integer: "200"}},
			},
			{
				"only": {CreatedAt: time.Unix(1700008002, 0).UTC(), Amount: DecimalAmount{Integer: "300"}, AmountAppend: DecimalAmount{Integer: "300"}},
			},
		},
		// Aliased exercises the named-non-struct-alias path through
		// PageList = []Record. Without the namedContainsMaterializingCodec
		// recursion into non-struct underlyings, Container would route
		// the Aliased field via WriteLength(Aliased.SizeGSBM()) while
		// AliasContainer's own slice encode falls back to BeginLengthDelim
		// — declared length would not match body bytes.
		Aliased: AliasContainer{
			Pages: PageList{
				{CreatedAt: time.Unix(1700009000, 0).UTC(), Amount: DecimalAmount{Integer: "5", Fraction: "5"}, AmountAppend: DecimalAmount{Integer: "5", Fraction: "5"}},
				{CreatedAt: time.Unix(1700009001, 0).UTC(), Amount: DecimalAmount{Negative: true, Integer: "9"}, AmountAppend: DecimalAmount{Negative: true, Integer: "9"}},
			},
		},
	}
	blob, err := gsbm.Marshal(&in, 0)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out Container
	if err := gsbm.DecodeInto(blob, &out); err != nil {
		t.Fatalf("DecodeInto: %v", err)
	}
	if out.Inner.CreatedAt.UnixNano() != in.Inner.CreatedAt.UnixNano() {
		t.Errorf("Inner.CreatedAt mismatch")
	}
	if !reflect.DeepEqual(out.Inner.Amount, in.Inner.Amount) {
		t.Errorf("Inner.Amount: got %+v want %+v", out.Inner.Amount, in.Inner.Amount)
	}
	if !reflect.DeepEqual(out.Inner.AmountAppend, in.Inner.AmountAppend) {
		t.Errorf("Inner.AmountAppend: got %+v want %+v", out.Inner.AmountAppend, in.Inner.AmountAppend)
	}
	if len(out.Items) != len(in.Items) {
		t.Fatalf("Items len: got %d want %d", len(out.Items), len(in.Items))
	}
	for i := range in.Items {
		if !reflect.DeepEqual(out.Items[i].Amount, in.Items[i].Amount) {
			t.Errorf("Items[%d].Amount: got %+v want %+v", i, out.Items[i].Amount, in.Items[i].Amount)
		}
		if !reflect.DeepEqual(out.Items[i].AmountAppend, in.Items[i].AmountAppend) {
			t.Errorf("Items[%d].AmountAppend: got %+v want %+v", i, out.Items[i].AmountAppend, in.Items[i].AmountAppend)
		}
	}
	if len(out.ByKey) != len(in.ByKey) {
		t.Fatalf("ByKey len: got %d want %d", len(out.ByKey), len(in.ByKey))
	}
	for k, want := range in.ByKey {
		got, ok := out.ByKey[k]
		if !ok {
			t.Errorf("ByKey[%q] missing", k)
			continue
		}
		if !reflect.DeepEqual(got.Amount, want.Amount) {
			t.Errorf("ByKey[%q].Amount: got %+v want %+v", k, got.Amount, want.Amount)
		}
	}
	if len(out.PtrItems) != len(in.PtrItems) {
		t.Fatalf("PtrItems len: got %d want %d", len(out.PtrItems), len(in.PtrItems))
	}
	for i, want := range in.PtrItems {
		got := out.PtrItems[i]
		if (got == nil) != (want == nil) {
			t.Errorf("PtrItems[%d] nilness: got nil=%v want nil=%v", i, got == nil, want == nil)
			continue
		}
		if want == nil {
			continue
		}
		if !reflect.DeepEqual(got.Amount, want.Amount) {
			t.Errorf("PtrItems[%d].Amount: got %+v want %+v", i, got.Amount, want.Amount)
		}
	}
	if len(out.Pages) != len(in.Pages) {
		t.Fatalf("Pages len: got %d want %d", len(out.Pages), len(in.Pages))
	}
	for i := range in.Pages {
		if len(out.Pages[i]) != len(in.Pages[i]) {
			t.Fatalf("Pages[%d] len: got %d want %d", i, len(out.Pages[i]), len(in.Pages[i]))
		}
		for j := range in.Pages[i] {
			if !reflect.DeepEqual(out.Pages[i][j].Amount, in.Pages[i][j].Amount) {
				t.Errorf("Pages[%d][%d].Amount: got %+v want %+v", i, j, out.Pages[i][j].Amount, in.Pages[i][j].Amount)
			}
			if !reflect.DeepEqual(out.Pages[i][j].AmountAppend, in.Pages[i][j].AmountAppend) {
				t.Errorf("Pages[%d][%d].AmountAppend: got %+v want %+v", i, j, out.Pages[i][j].AmountAppend, in.Pages[i][j].AmountAppend)
			}
		}
	}
	if len(out.Buckets) != len(in.Buckets) {
		t.Fatalf("Buckets len: got %d want %d", len(out.Buckets), len(in.Buckets))
	}
	for i, want := range in.Buckets {
		got := out.Buckets[i]
		if len(got) != len(want) {
			t.Fatalf("Buckets[%d] len: got %d want %d", i, len(got), len(want))
		}
		for k, wantR := range want {
			gotR, ok := got[k]
			if !ok {
				t.Errorf("Buckets[%d][%q] missing", i, k)
				continue
			}
			if !reflect.DeepEqual(gotR.Amount, wantR.Amount) {
				t.Errorf("Buckets[%d][%q].Amount: got %+v want %+v", i, k, gotR.Amount, wantR.Amount)
			}
		}
	}
	if len(out.Aliased.Pages) != len(in.Aliased.Pages) {
		t.Fatalf("Aliased.Pages len: got %d want %d", len(out.Aliased.Pages), len(in.Aliased.Pages))
	}
	for i := range in.Aliased.Pages {
		if !reflect.DeepEqual(out.Aliased.Pages[i].Amount, in.Aliased.Pages[i].Amount) {
			t.Errorf("Aliased.Pages[%d].Amount: got %+v want %+v", i, out.Aliased.Pages[i].Amount, in.Aliased.Pages[i].Amount)
		}
		if !reflect.DeepEqual(out.Aliased.Pages[i].AmountAppend, in.Aliased.Pages[i].AmountAppend) {
			t.Errorf("Aliased.Pages[%d].AmountAppend: got %+v want %+v", i, out.Aliased.Pages[i].AmountAppend, in.Aliased.Pages[i].AmountAppend)
		}
	}
}

// TestContainerRoundTripStreamingCompressed pins the same correctness
// across the streaming-compressed path. The streaming-mode Writer reads
// pre-recorded region sizes from the size pass, so any drift between
// size-pass and write-pass body bytes manifests as a streamSizes / actual
// mismatch — which would surface here as an Unmarshal error rather than
// silent corruption.
func TestContainerRoundTripStreamingCompressed(t *testing.T) {
	in := Container{
		Inner: Record{
			CreatedAt:    time.Unix(1700007000, 0).UTC(),
			Amount:       DecimalAmount{Integer: "42", Fraction: "0"},
			AmountAppend: DecimalAmount{Integer: "42", Fraction: "0"},
		},
		Items: []Record{
			{
				CreatedAt:    time.Unix(1700008000, 0).UTC(),
				Amount:       DecimalAmount{Integer: "1"},
				AmountAppend: DecimalAmount{Integer: "1"},
				Payload:      LargePayload{Tag: "x", Data: []byte("hello")},
			},
		},
		ByKey: map[string]Record{
			"key": {
				CreatedAt:    time.Unix(1700009000, 0).UTC(),
				Amount:       DecimalAmount{Integer: "3", Fraction: "14"},
				AmountAppend: DecimalAmount{Integer: "3", Fraction: "14"},
			},
		},
		Pages: [][]Record{
			{
				{CreatedAt: time.Unix(1700010000, 0).UTC(), Amount: DecimalAmount{Integer: "1", Fraction: "5"}, AmountAppend: DecimalAmount{Integer: "1", Fraction: "5"}},
			},
		},
		Buckets: []map[string]Record{
			{
				"k": {CreatedAt: time.Unix(1700010100, 0).UTC(), Amount: DecimalAmount{Integer: "2"}, AmountAppend: DecimalAmount{Integer: "2"}},
			},
		},
	}
	var buf bytes.Buffer
	if err := gsbm.MarshalToWriter(&buf, &in, 0, gsbm.Options{Compress: true}); err != nil {
		t.Fatalf("MarshalToWriter: %v", err)
	}
	var out Container
	if err := gsbm.DecodeInto(buf.Bytes(), &out); err != nil {
		t.Fatalf("DecodeInto: %v", err)
	}
	if !reflect.DeepEqual(out.Inner.Amount, in.Inner.Amount) {
		t.Errorf("Inner.Amount: got %+v want %+v", out.Inner.Amount, in.Inner.Amount)
	}
	if len(out.Items) != 1 || !reflect.DeepEqual(out.Items[0].Amount, in.Items[0].Amount) {
		t.Errorf("Items[0].Amount: got %+v want %+v", out.Items[0].Amount, in.Items[0].Amount)
	}
	if got, ok := out.ByKey["key"]; !ok || !reflect.DeepEqual(got.Amount, in.ByKey["key"].Amount) {
		t.Errorf("ByKey[key].Amount: got %+v want %+v", got.Amount, in.ByKey["key"].Amount)
	}
	if len(out.Pages) != 1 || len(out.Pages[0]) != 1 || !reflect.DeepEqual(out.Pages[0][0].Amount, in.Pages[0][0].Amount) {
		t.Errorf("Pages: got %+v want %+v", out.Pages, in.Pages)
	}
	if len(out.Buckets) != 1 {
		t.Fatalf("Buckets len: got %d want 1", len(out.Buckets))
	}
	if got, ok := out.Buckets[0]["k"]; !ok || !reflect.DeepEqual(got.Amount, in.Buckets[0]["k"].Amount) {
		t.Errorf("Buckets[0][k].Amount: got %+v want %+v", got.Amount, in.Buckets[0]["k"].Amount)
	}
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
	// tag 5 (Payload, streaming codec wire = LENDLM): json.Marshal of the
	// zero LargePayload produces `{"tag":""}` (Data is omitempty); the
	// streaming codec writes it as a length-prefixed byte string.
	w.WriteTag(5, gsbm.WireLengthDelim)
	w.WriteBytes(jsonBytes(t, in.Payload))
	// tag 6 (AmountBinary, analytic codec wire = LENDLM): key (6<<3)|2, inner
	// uvarint length prefix, body = uvarint(coef) ++ uvarint(scale<<1|sign).
	// The zero DecimalAmount is coef 0, scale 0, sign 0 → body [0x00 0x00].
	w.WriteTag(6, gsbm.WireLengthDelim)
	w.WriteUvarint(2)
	w.WriteUvarint(0)
	w.WriteUvarint(0)
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
	// tag 5 Payload — zero LargePayload JSON-marshals to `{"tag":""}`.
	w.WriteTag(5, gsbm.WireLengthDelim)
	w.WriteBytes(jsonBytes(t, in.Payload))
	// tag 6 AmountBinary — zero DecimalAmount: coef 0, scale 0, sign 0.
	w.WriteTag(6, gsbm.WireLengthDelim)
	w.WriteUvarint(2)
	w.WriteUvarint(0)
	w.WriteUvarint(0)
	want := w.Bytes()
	if !bytes.Equal(buf, want) {
		t.Fatalf("wire bytes mismatch:\n got: % x\nwant: % x", buf, want)
	}
}

// TestRecordRoundTripDecimalBinary exercises the analytic binary decimal
// codec field end-to-end: each DecimalAmount encodes to the
// `uvarint(coef) ++ uvarint(scale<<1|sign)` body and decodes back equal.
// The table covers the cases the binary form is most likely to break:
// trailing-zero scale, an integer-only value, a signed value, and a
// coefficient ≥ 2^63 (the decode wrinkle — a 19-digit coefficient does not
// fit a signed int64, so the reconstruct callback must accept uint64).
func TestRecordRoundTripDecimalBinary(t *testing.T) {
	cases := []DecimalAmount{
		{Integer: "12", Fraction: "500"},
		{Negative: true, Integer: "7", Fraction: "25"},
		{Integer: "42"},
		{Integer: "0"},
		{Integer: "1", Fraction: "00"},
		{Integer: "9223372036854775808"}, // 2^63 — coefficient ≥ 2^63
	}
	for _, amt := range cases {
		in := Record{
			CreatedAt:    time.Unix(1, 0).UTC(),
			Amount:       DecimalAmount{Integer: "0"},
			AmountBinary: amt,
		}
		buf := encode(t, in)
		var out Record
		if err := out.UnmarshalGSBM(gsbm.NewReader(buf)); err != nil {
			t.Fatalf("UnmarshalGSBM: %v", err)
		}
		if !reflect.DeepEqual(out.AmountBinary, amt) {
			t.Errorf("AmountBinary %q: round-trip got %+v, want %+v", amt.String(), out.AmountBinary, amt)
		}
	}
}

// TestRecordWireBytesDecimalBinary pins the exact bytes the analytic binary
// decimal codec emits for a known value: coef=12345, scale=2, neg=true. The
// body is uvarint(12345) ++ uvarint(2<<1|1) = uvarint(12345) ++ uvarint(5),
// wrapped by codegen in the LENGTH_DELIM envelope (key + inner length).
func TestRecordWireBytesDecimalBinary(t *testing.T) {
	in := Record{
		CreatedAt:    time.Unix(1, 0),
		Amount:       DecimalAmount{Integer: "0"},
		AmountBinary: DecimalAmount{Negative: true, Integer: "123", Fraction: "45"},
	}
	buf := encode(t, in)
	bin, ok := payloadForTag(buf, 6)
	if !ok {
		t.Fatalf("tag 6 not found in buf: % x", buf)
	}

	var body gsbm.Writer
	body.WriteUvarint(12345)            // coef
	body.WriteUvarint(uint64(2)<<1 | 1) // scale 2, negative
	if want := body.Bytes(); !bytes.Equal(bin, want) {
		t.Fatalf("DecimalBinary body mismatch:\n got: % x\nwant: % x", bin, want)
	}
}

// TestRecordDecimalBinaryScaleLimit pins the encode/decode symmetry of the
// binary decimal codec at its concrete scale boundary. ReconstructDecimalAmount
// caps the fractional-digit count it will rebuild at maxReconstructScale, so
// EncodeDecimalAmountBinary must reject anything past that bound — otherwise a
// value would marshal successfully and then fail to unmarshal through the same
// binding, leaving the fixture codec not self-inverse.
func TestRecordDecimalBinaryScaleLimit(t *testing.T) {
	// A value exactly at the limit must round-trip. The fraction is
	// maxReconstructScale digits but its coefficient is 1, so Coef() stays
	// well within uint64 — the round-trip exercises the scale bound, not
	// the separate coefficient-overflow caveat documented on Coef().
	atLimit := DecimalAmount{Fraction: strings.Repeat("0", maxReconstructScale-1) + "1"}
	in := Record{
		CreatedAt:    time.Unix(1, 0).UTC(),
		Amount:       DecimalAmount{Integer: "0"},
		AmountBinary: atLimit,
	}
	buf := encode(t, in)
	var out Record
	if err := out.UnmarshalGSBM(gsbm.NewReader(buf)); err != nil {
		t.Fatalf("UnmarshalGSBM at scale %d: %v", maxReconstructScale, err)
	}
	if !reflect.DeepEqual(out.AmountBinary, atLimit) {
		t.Errorf("AmountBinary at scale %d: round-trip got %+v, want %+v", maxReconstructScale, out.AmountBinary, atLimit)
	}

	// A value one past the limit must be rejected at encode time, so it
	// never reaches the wire as a payload decode cannot accept.
	overLimit := Record{
		CreatedAt:    time.Unix(1, 0).UTC(),
		Amount:       DecimalAmount{Integer: "0"},
		AmountBinary: DecimalAmount{Fraction: strings.Repeat("0", maxReconstructScale) + "1"},
	}
	var w gsbm.Writer
	if err := overLimit.MarshalGSBM(&w); err == nil {
		t.Fatalf("MarshalGSBM at scale %d: want error, got nil", maxReconstructScale+1)
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
		Payload:      LargePayload{Tag: "reset", Data: []byte("xyz")},
		AmountBinary: DecimalAmount{Integer: "5", Fraction: "5"},
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
	if v.Payload.Tag != "" || v.Payload.Data != nil {
		t.Errorf("Payload = %+v, want zero", v.Payload)
	}
	if v.AmountBinary != (DecimalAmount{}) {
		t.Errorf("AmountBinary = %+v, want zero", v.AmountBinary)
	}
	for _, tag := range []uint32{1, 2, 3, 4, 5, 6} {
		if v.FieldPresent(tag) {
			t.Errorf("FieldPresent(%d) = true after Reset", tag)
		}
	}
}

// TestRecordRoundTripStreamingPayload exercises the streaming-codec field:
// a LargePayload with a non-empty Tag and Data must round-trip exactly
// through json.Marshal / json.Unmarshal in the LENGTH_DELIM envelope the
// streaming codec emits. The streaming kind materializes the body twice
// per gsbm.Marshal call (once per pass), but the wire bytes are identical
// across passes because json.Marshal on this struct is deterministic.
func TestRecordRoundTripStreamingPayload(t *testing.T) {
	in := Record{
		CreatedAt: time.Unix(1, 0).UTC(),
		Amount:    DecimalAmount{Integer: "0"},
		Payload:   LargePayload{Tag: "alpha", Data: []byte("hello-streaming")},
	}
	buf := encode(t, in)
	var out Record
	if err := out.UnmarshalGSBM(gsbm.NewReader(buf)); err != nil {
		t.Fatalf("UnmarshalGSBM: %v", err)
	}
	if out.Payload.Tag != in.Payload.Tag {
		t.Errorf("Payload.Tag: got %q, want %q", out.Payload.Tag, in.Payload.Tag)
	}
	if !bytes.Equal(out.Payload.Data, in.Payload.Data) {
		t.Errorf("Payload.Data: got % x, want % x", out.Payload.Data, in.Payload.Data)
	}
}

// streamProbe is a streaming-codec body that increments a counter every
// time it runs. Wrapping builtins.StreamJSONBytes preserves the wire shape
// (LENGTH_DELIM JSON) while letting the test observe the materialize-per-
// pass invariant: the streaming kind runs the body exactly twice per
// gsbm.Marshal call per occurrence (once in the size pass, once in the
// write pass), with no scratch cache between passes.
type streamProbePayload struct {
	p     LargePayload
	calls *int
}

func streamProbe(w *gsbm.Writer, v streamProbePayload) error {
	*v.calls++
	return builtins.StreamJSONBytes(w, v.p)
}

// probeStreamRecord wires a streaming-codec field through gsbm.Marshal so
// the test can observe the per-pass body invocation count. The hand-rolled
// MarshalGSBM mirrors what codegen would emit for a streaming-codec field
// at tag 5: no callsite, no cache, just the StreamFn call inside the
// LENGTH_DELIM envelope the codec body produces.
type probeStreamRecord struct {
	field streamProbePayload
}

func (p *probeStreamRecord) SizeGSBM() int {
	cw := gsbm.NewCountingWriter()
	_ = p.MarshalGSBM(cw)
	return cw.Size()
}

func (p *probeStreamRecord) MarshalGSBM(w *gsbm.Writer) error {
	w.WriteTag(5, gsbm.WireLengthDelim)
	if err := streamProbe(w, p.field); err != nil {
		return err
	}
	return w.Err()
}

// TestStreamingMaterializeTwice pins the streaming-codec invariant: the
// body runs exactly twice per gsbm.Marshal call per occurrence — once
// during the size pass, once during the write pass — because the
// streaming kind retains nothing between passes. This is the inverse of
// the materializing-cached invariant (1× per occurrence) and is the
// defining property of CodecKindStreaming.
func TestStreamingMaterializeTwice(t *testing.T) {
	calls := 0
	p := &probeStreamRecord{
		field: streamProbePayload{
			p:     LargePayload{Tag: "twice", Data: []byte("body")},
			calls: &calls,
		},
	}
	if _, err := gsbm.Marshal(p, 0); err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if calls != 2 {
		t.Errorf("streaming codec body invocations = %d, want 2 (size pass + write pass)", calls)
	}
}

// jsonStringPayload is a payload whose String() form equals
// json.Marshal(v) bytes-for-bytes, by construction. That equality is what
// lets the wire-equivalence test below assert that a materializing-cached
// codec (which writes v.String() as a LENGTH_DELIM string) and a streaming
// codec (which writes json.Marshal(v) as LENGTH_DELIM bytes) produce
// byte-identical wire bodies for the same value.
type jsonStringPayload struct {
	Tag string `json:"tag"`
}

func (j jsonStringPayload) String() string {
	b, err := json.Marshal(j)
	if err != nil {
		// json.Marshal on a struct of exported strings cannot fail.
		panic(err)
	}
	return string(b)
}

// probeCachedJSONRecord routes jsonStringPayload through the
// materializing-cached path via EmitDecimalString (which writes
// v.String() as a LENGTH_DELIM string via the callsite scratch cache).
type probeCachedJSONRecord struct {
	payload jsonStringPayload
}

func (p *probeCachedJSONRecord) SizeGSBM() int {
	cw := gsbm.NewCountingWriter()
	_ = p.MarshalGSBM(cw)
	return cw.Size()
}

func (p *probeCachedJSONRecord) MarshalGSBM(w *gsbm.Writer) error {
	w.WriteTag(5, gsbm.WireLengthDelim)
	if err := builtins.EmitDecimalString(w, p.payload, 0xdeadbeefcafebabe); err != nil {
		return err
	}
	return w.Err()
}

// probeStreamedJSONRecord routes the same jsonStringPayload through the
// streaming path via StreamJSONBytes (which writes json.Marshal(v) as
// LENGTH_DELIM bytes with no scratch cache).
type probeStreamedJSONRecord struct {
	payload jsonStringPayload
}

func (p *probeStreamedJSONRecord) SizeGSBM() int {
	cw := gsbm.NewCountingWriter()
	_ = p.MarshalGSBM(cw)
	return cw.Size()
}

func (p *probeStreamedJSONRecord) MarshalGSBM(w *gsbm.Writer) error {
	w.WriteTag(5, gsbm.WireLengthDelim)
	if err := builtins.StreamJSONBytes(w, p.payload); err != nil {
		return err
	}
	return w.Err()
}

// probeAnalyticJSONRecord routes the same jsonStringPayload through an
// analytic-shaped codec body: the size pass reports len(json.Marshal(v))
// directly (no body retention), the write pass writes those same bytes.
// This is the analytic kind in spirit — SizeFn and EncodeFn live in the
// same MarshalGSBM only because we're not exercising the codegen
// dispatcher here; the property under test is wire-bytes parity across
// kinds, not registry plumbing.
type probeAnalyticJSONRecord struct {
	payload jsonStringPayload
}

func (p *probeAnalyticJSONRecord) SizeGSBM() int {
	cw := gsbm.NewCountingWriter()
	_ = p.MarshalGSBM(cw)
	return cw.Size()
}

func (p *probeAnalyticJSONRecord) MarshalGSBM(w *gsbm.Writer) error {
	w.WriteTag(5, gsbm.WireLengthDelim)
	b, err := json.Marshal(p.payload)
	if err != nil {
		return err
	}
	w.WriteBytes(b)
	return w.Err()
}

// TestWireBytesEqualAcrossThreeKinds proves that all three codec kinds —
// analytic, materializing-cached, streaming — produce byte-identical wire
// output for values where the underlying materializations match (here, a
// payload whose String() returns its own json.Marshal bytes). Same logical
// value, same body bytes, same length prefix, same envelope: no behavioral
// drift across kinds when each is configured to encode equivalently. This
// pins the cross-kind invariant from the issue #30 Testing Strategy.
func TestWireBytesEqualAcrossThreeKinds(t *testing.T) {
	cases := []jsonStringPayload{
		{Tag: ""},
		{Tag: "alpha"},
		{Tag: "long-string-with-symbols !@#"},
	}
	for _, p := range cases {
		analyticBuf, err := gsbm.Marshal(&probeAnalyticJSONRecord{payload: p}, 0)
		if err != nil {
			t.Fatalf("analytic Marshal: %v", err)
		}
		cachedBuf, err := gsbm.Marshal(&probeCachedJSONRecord{payload: p}, 0)
		if err != nil {
			t.Fatalf("cached Marshal: %v", err)
		}
		streamedBuf, err := gsbm.Marshal(&probeStreamedJSONRecord{payload: p}, 0)
		if err != nil {
			t.Fatalf("streamed Marshal: %v", err)
		}
		if !bytes.Equal(analyticBuf, cachedBuf) {
			t.Errorf("analytic vs cached wire bytes diverge for %q:\n  analytic: % x\n  cached:   % x", p.Tag, analyticBuf, cachedBuf)
		}
		if !bytes.Equal(cachedBuf, streamedBuf) {
			t.Errorf("cached vs streaming wire bytes diverge for %q:\n  cached: % x\n  stream: % x", p.Tag, cachedBuf, streamedBuf)
		}
		if !bytes.Equal(analyticBuf, streamedBuf) {
			t.Errorf("analytic vs streaming wire bytes diverge for %q:\n  analytic: % x\n  stream:   % x", p.Tag, analyticBuf, streamedBuf)
		}
	}
}
