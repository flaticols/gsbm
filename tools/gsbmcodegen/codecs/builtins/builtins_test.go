package builtins

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.flaticols.dev/gsbm/storage/gsbm"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/codecs"
)

// stringerDecimal is a minimal Stringer-typed decimal used for testing the
// DecimalString template. The exact string form is the canonical wire
// payload: trailing zeros, leading minus, and the empty string must all
// round-trip unchanged.
type stringerDecimal struct{ s string }

func (d stringerDecimal) String() string { return d.s }

func parseStringerDecimal(s string) (stringerDecimal, error) {
	return stringerDecimal{s: s}, nil
}

func TestDecimalStringRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"plain", "12345"},
		// Trailing zeros — the explicit edge case from the plan. A
		// numeric round-trip via float would silently drop these; the
		// string codec must preserve them.
		{"trailing-zeros", "100.000"},
		{"trailing-zeros-fractional", "1.2300"},
		{"negative", "-42.5"},
		{"zero", "0"},
		{"empty", ""},
		// Wide value — exercise the length-prefix path on something
		// larger than a single varint byte.
		{"wide", strings.Repeat("9", 250) + "." + strings.Repeat("0", 50)},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := stringerDecimal{s: tc.in}
			w := gsbm.NewWriter(nil)
			if err := EmitDecimalString(w, in, uint64(0x100+i)); err != nil {
				t.Fatalf("emit: %v", err)
			}
			r := gsbm.NewReader(w.Bytes())
			var got stringerDecimal
			if err := DecodeDecimalString(r, &got, parseStringerDecimal); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.s != in.s {
				t.Fatalf("round-trip: got %q want %q", got.s, in.s)
			}
		})
	}
}

// TestEmitDecimalStringSizeMatchesWrite is the materializing-codec
// lockstep check for the DecimalString codec: a size-mode Writer fed
// EmitDecimalString must accumulate the same byte count a real Writer
// would produce, since gsbm.Marshal's two-pass flow uses both modes
// against the same codec call. Exercises the same string edge cases the
// round-trip test covers (trailing zeros, empty, wide) since those are
// where the length-prefix arithmetic is most likely to diverge.
func TestEmitDecimalStringSizeMatchesWrite(t *testing.T) {
	cases := []string{
		"12345",
		"100.000",
		"1.2300",
		"-42.5",
		"0",
		"",
		strings.Repeat("9", 250) + "." + strings.Repeat("0", 50),
	}
	for i, tc := range cases {
		v := stringerDecimal{s: tc}
		cs := uint64(0x200 + i)
		bw := gsbm.NewWriter(nil)
		if err := EmitDecimalString(bw, v, cs); err != nil {
			t.Fatalf("emit (write-mode) %q: %v", tc, err)
		}
		cw := gsbm.NewCountingWriter()
		if err := EmitDecimalString(cw, v, cs); err != nil {
			t.Fatalf("emit (size-mode) %q: %v", tc, err)
		}
		got := cw.Size()
		want := len(bw.Bytes())
		if got != want {
			t.Errorf("EmitDecimalString(%q) size-mode = %d, write-mode wrote %d", tc, got, want)
		}
	}
}

// TestEmitDecimalStringPerOccurrenceMaterialization pins the cache
// contract within a single pass: each call to EmitDecimalString at a
// shared callsite is a distinct occurrence (a slice element, a
// separate field reachable along the same MarshalGSBM walk), so the
// underlying v.String() method runs once per occurrence. The cache
// pays its keep on the size→write hand-off — see
// TestAdoptScratchPreservesOccurrenceOrder for that property — not on
// repeated single-pass calls.
func TestEmitDecimalStringPerOccurrenceMaterialization(t *testing.T) {
	var calls int
	cs := uint64(0xcafe)
	probe := countingStringer{s: "42.500", calls: &calls}
	w := gsbm.NewWriter(nil)
	if err := EmitDecimalString(w, probe, cs); err != nil {
		t.Fatalf("first emit: %v", err)
	}
	firstLen := len(w.Bytes())
	if err := EmitDecimalString(w, probe, cs); err != nil {
		t.Fatalf("second emit: %v", err)
	}
	if calls != 2 {
		t.Fatalf("String() invoked %d times across two distinct emits, want 2 (one per occurrence)", calls)
	}
	got := w.Bytes()
	// Both occurrences pass the same probe value, so the bytes are
	// byte-identical even though each ran its own materialization.
	if firstLen*2 != len(got) {
		t.Fatalf("second emit produced different byte count: first = %d, total = %d", firstLen, len(got))
	}
	if !bytes.Equal(got[:firstLen], got[firstLen:]) {
		t.Fatalf("second emit bytes diverge from first:\n first: % x\nsecond: % x", got[:firstLen], got[firstLen:])
	}
}

// TestEmitDecimalStringSizeOnlyMaterializes verifies a size-only call
// still produces the right byte count and invokes the materializer
// exactly once. SizeGSBM standalone has no hand-off so the cache lives
// and dies with the size-mode Writer, but the materialize-once property
// still holds within that scope — the documented double-materialization
// cost only applies when SizeGSBM and a separate MarshalGSBM run on
// disjoint Writers, not within one pass.
func TestEmitDecimalStringSizeOnlyMaterializes(t *testing.T) {
	var calls int
	cs := uint64(0xfeed)
	probe := countingStringer{s: "-12.500", calls: &calls}

	sw := gsbm.NewCountingWriter()
	if err := EmitDecimalString(sw, probe, cs); err != nil {
		t.Fatalf("size-mode emit: %v", err)
	}
	if calls != 1 {
		t.Fatalf("size-mode String() invoked %d times, want 1", calls)
	}

	// A real Writer fed the same input produces a byte count matching
	// the size pass — pinning the SizeFn/EncodeFn lockstep through the
	// materializing-codec path.
	bw := gsbm.NewWriter(nil)
	if err := EmitDecimalString(bw, countingStringer{s: probe.s, calls: new(int)}, cs); err != nil {
		t.Fatalf("write-mode emit: %v", err)
	}
	if got, want := sw.Size(), len(bw.Bytes()); got != want {
		t.Fatalf("size-mode accumulated %d, write-mode wrote %d", got, want)
	}
}

func TestDecimalStringParseError(t *testing.T) {
	// A failing parse function must surface its error from
	// DecodeDecimalString — the codec must not silently coerce a parse
	// failure into a zero value.
	w := gsbm.NewWriter(nil)
	if err := EmitDecimalString(w, stringerDecimal{s: "not a number"}, uint64(0x301)); err != nil {
		t.Fatalf("emit: %v", err)
	}
	r := gsbm.NewReader(w.Bytes())
	want := fmt.Errorf("parse failed")
	parse := func(s string) (int, error) { return 0, want }
	var got int
	err := DecodeDecimalString(r, &got, parse)
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if err != want {
		t.Fatalf("expected wrapped parse error, got %v", err)
	}
	if got != 0 {
		t.Fatalf("parse failure must leave *v untouched, got %d", got)
	}
}

func TestDecimalStringWithIntegerType(t *testing.T) {
	// Bind the template to a real (non-stringerDecimal) type so the
	// generic instantiation is exercised on a different shape. strconv
	// gives us a concrete parse function. This is the integration shape
	// codegen will emit at the call site.
	in := 100
	w := gsbm.NewWriter(nil)
	if err := EmitDecimalString(w, intStringer(in), uint64(0x302)); err != nil {
		t.Fatalf("emit: %v", err)
	}
	r := gsbm.NewReader(w.Bytes())
	var got int
	parse := func(s string) (int, error) { return strconv.Atoi(s) }
	if err := DecodeDecimalString(r, &got, parse); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != in {
		t.Fatalf("round-trip: got %d want %d", got, in)
	}
}

// countingStringer counts String() invocations via *calls so the
// materialize-once cache assertion can observe how many times the
// underlying materializer ran for a single Writer encode call.
type countingStringer struct {
	s     string
	calls *int
}

func (c countingStringer) String() string {
	*c.calls++
	return c.s
}

type intStringer int

func (i intStringer) String() string { return strconv.Itoa(int(i)) }

func TestNewBuiltinRegistryHasTime(t *testing.T) {
	r := NewBuiltinRegistry()
	tc, ok := r.Lookup("Time")
	if !ok {
		t.Fatal("Time not registered")
	}
	if tc != TimeDecl {
		t.Fatalf("registered decl differs:\n got %+v\nwant %+v", tc, TimeDecl)
	}
	// Built-in registry starts clean — only Time is shipped.
	got := r.Names()
	want := []string{"Time"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("NewBuiltinRegistry: got %v, want %v", got, want)
	}
}

// TestTimeRoundTrip exercises the full Go time.Time range: every entry
// must round-trip via the codec with Equal == true. A naïve
// int64-nanosecond codec would silently wrap on the year-1, year-1500,
// year-4000, and far-future entries; the Time codec encodes (seconds,
// nanos) separately so each entry round-trips cleanly. See issue #21.
func TestTimeRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   time.Time
	}{
		{"zero", time.Time{}},
		{"year-1-ad", time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"year-1500", time.Date(1500, 6, 15, 12, 0, 0, 0, time.UTC)},
		{"unix-epoch", time.Unix(0, 0).UTC()},
		{"year-2026", time.Date(2026, 5, 13, 14, 30, 45, 123456789, time.UTC)},
		{"year-2300", time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"year-4000", time.Date(4000, 7, 4, 0, 0, 0, 0, time.UTC)},
		{"far-future", time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)},
		{"nano-1", time.Unix(1_000_000_000, 1).UTC()},
		{"nano-max", time.Unix(1_700_000_000, 999_999_999).UTC()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := gsbm.NewWriter(nil)
			if err := EncodeTime(w, tc.in); err != nil {
				t.Fatalf("encode: %v", err)
			}
			r := gsbm.NewReader(w.Bytes())
			var got time.Time
			if err := DecodeTime(r, &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !got.Equal(tc.in) {
				t.Fatalf("round-trip: got %s (unix=%d, nano=%d), want %s (unix=%d, nano=%d)",
					got, got.Unix(), got.Nanosecond(),
					tc.in, tc.in.Unix(), tc.in.Nanosecond())
			}
			if got.Unix() != tc.in.Unix() || got.Nanosecond() != tc.in.Nanosecond() {
				t.Fatalf("instant parts diverged: got unix=%d nano=%d, want unix=%d nano=%d",
					got.Unix(), got.Nanosecond(), tc.in.Unix(), tc.in.Nanosecond())
			}
		})
	}
}

// TestTimeZeroPreserved is the headline property from issue #21: the zero
// time.Time must round-trip exactly. A naïve UnixNano-based codec
// corrupts this (year 1 AD → year 1754 due to int64 overflow).
func TestTimeZeroPreserved(t *testing.T) {
	in := time.Time{}
	w := gsbm.NewWriter(nil)
	if err := EncodeTime(w, in); err != nil {
		t.Fatalf("encode: %v", err)
	}
	r := gsbm.NewReader(w.Bytes())
	var got time.Time
	if err := DecodeTime(r, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Equal(in) {
		t.Fatalf("zero-time not preserved: got %s, want %s", got, in)
	}
}

// TestSizeTimeMatchesEncode is the analytic-codec lockstep: SizeTime(t)
// must equal len(bytes written by EncodeTime(w, t)) for every input. A
// mismatch corrupts bodyLen at the call site and shifts every subsequent
// field on the wire.
func TestSizeTimeMatchesEncode(t *testing.T) {
	cases := []time.Time{
		{},
		time.Unix(0, 0),
		time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(1500, 6, 15, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 5, 13, 14, 30, 45, 123456789, time.UTC),
		time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(4000, 7, 4, 0, 0, 0, 0, time.UTC),
		time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC),
	}
	for _, tc := range cases {
		w := gsbm.NewWriter(nil)
		if err := EncodeTime(w, tc); err != nil {
			t.Fatalf("encode %s: %v", tc, err)
		}
		got := SizeTime(tc)
		want := len(w.Bytes())
		if got != want {
			t.Errorf("SizeTime(%s) = %d, encode wrote %d", tc, got, want)
		}
	}
}

// TestDecodeTimeRejectsInvalidNanos exercises the malformed-body branch:
// a body whose nanos varint exceeds 999_999_999 must return
// errInvalidNanos. time.Nanosecond() can never produce such a value, so a
// well-formed encoder doesn't trigger this path — but a malformed wire
// payload must be refused rather than constructing an out-of-range time.
func TestDecodeTimeRejectsInvalidNanos(t *testing.T) {
	w := gsbm.NewWriter(nil)
	w.WriteVarint(0)
	w.WriteUvarint(1_000_000_000) // one past the valid max
	r := gsbm.NewReader(w.Bytes())
	var got time.Time
	err := DecodeTime(r, &got)
	if !errors.Is(err, errInvalidNanos) {
		t.Fatalf("expected errInvalidNanos, got %v", err)
	}
}

// TestDecodeTimeRejectsTruncatedBody covers the two reader-error paths in
// DecodeTime: an empty body fails the seconds varint read, and a
// seconds-only body fails the nanos varint read. The decoder must
// surface the reader error rather than fall through to a zero-defaulted
// time.Time.
func TestDecodeTimeRejectsTruncatedBody(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"empty", nil},
		{"seconds-only", func() []byte {
			w := gsbm.NewWriter(nil)
			w.WriteVarint(42)
			return w.Bytes()
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := gsbm.NewReader(tc.body)
			got := time.Unix(1, 1).UTC()
			err := DecodeTime(r, &got)
			if err == nil {
				t.Fatalf("expected error decoding truncated body, got nil (got=%v)", got)
			}
		})
	}
}

// appenderDecimal is the append-API counterpart to stringerDecimal. Its
// AppendText returns the textual form by appending bytes to dst, so the
// EmitDecimalAppend path can be exercised without an intermediate
// string materialization. The empty string round-trip case is preserved
// (AppendText on an empty value appends nothing).
type appenderDecimal struct{ s string }

func (d appenderDecimal) AppendText(dst []byte) ([]byte, error) {
	return append(dst, d.s...), nil
}

// stringAndAppendDecimal exposes both String() and AppendText so the
// wire-equivalence test can prove EmitDecimalAppend and
// EmitDecimalString produce byte-identical output when the textual form
// agrees.
type stringAndAppendDecimal struct{ s string }

func (d stringAndAppendDecimal) String() string { return d.s }
func (d stringAndAppendDecimal) AppendText(dst []byte) ([]byte, error) {
	return append(dst, d.s...), nil
}

func TestDecimalAppendRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"plain", "12345"},
		{"trailing-zeros", "100.000"},
		{"trailing-zeros-fractional", "1.2300"},
		{"negative", "-42.5"},
		{"zero", "0"},
		{"empty", ""},
		{"wide", strings.Repeat("9", 250) + "." + strings.Repeat("0", 50)},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := appenderDecimal{s: tc.in}
			w := gsbm.NewWriter(nil)
			if err := EmitDecimalAppend(w, in, uint64(0x400+i)); err != nil {
				t.Fatalf("emit: %v", err)
			}
			r := gsbm.NewReader(w.Bytes())
			parse := func(s string) (appenderDecimal, error) {
				return appenderDecimal{s: s}, nil
			}
			var got appenderDecimal
			if err := DecodeDecimalString(r, &got, parse); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.s != in.s {
				t.Fatalf("round-trip: got %q want %q", got.s, in.s)
			}
		})
	}
}

// TestDecimalAppendWireEqualsDecimalString proves the append path
// produces byte-identical wire output to the string path for any value
// whose AppendText and String agree. This pins the no-drift property
// the issue calls out: choosing the append codec is purely an
// allocation optimization, not a wire change.
func TestDecimalAppendWireEqualsDecimalString(t *testing.T) {
	cases := []string{
		"12345",
		"100.000",
		"1.2300",
		"-42.5",
		"0",
		"",
		strings.Repeat("9", 250) + "." + strings.Repeat("0", 50),
	}
	for i, tc := range cases {
		v := stringAndAppendDecimal{s: tc}
		cs := uint64(0x500 + i)

		ws := gsbm.NewWriter(nil)
		if err := EmitDecimalString(ws, v, cs); err != nil {
			t.Fatalf("EmitDecimalString %q: %v", tc, err)
		}
		wa := gsbm.NewWriter(nil)
		if err := EmitDecimalAppend(wa, v, cs); err != nil {
			t.Fatalf("EmitDecimalAppend %q: %v", tc, err)
		}
		if !bytes.Equal(ws.Bytes(), wa.Bytes()) {
			t.Fatalf("wire drift for %q:\n string: % x\n append: % x", tc, ws.Bytes(), wa.Bytes())
		}
	}
}

// TestEmitDecimalAppendSizeMatchesWrite is the size/write lockstep
// check for the append codec, mirroring
// TestEmitDecimalStringSizeMatchesWrite.
func TestEmitDecimalAppendSizeMatchesWrite(t *testing.T) {
	cases := []string{
		"12345",
		"100.000",
		"1.2300",
		"-42.5",
		"0",
		"",
		strings.Repeat("9", 250) + "." + strings.Repeat("0", 50),
	}
	for i, tc := range cases {
		v := appenderDecimal{s: tc}
		cs := uint64(0x600 + i)
		bw := gsbm.NewWriter(nil)
		if err := EmitDecimalAppend(bw, v, cs); err != nil {
			t.Fatalf("emit (write-mode) %q: %v", tc, err)
		}
		cw := gsbm.NewCountingWriter()
		if err := EmitDecimalAppend(cw, v, cs); err != nil {
			t.Fatalf("emit (size-mode) %q: %v", tc, err)
		}
		got := cw.Size()
		want := len(bw.Bytes())
		if got != want {
			t.Errorf("EmitDecimalAppend(%q) size-mode = %d, write-mode wrote %d", tc, got, want)
		}
	}
}

func TestNewDecimalAppendDecl(t *testing.T) {
	d := NewDecimalAppendDecl(
		"MyDecimalAppend",
		"example.com/v1.Decimal",
		"EmitMyDecimalAppend",
		"DecodeMyDecimalAppend",
		"example.com/v1",
	)
	want := codecs.CodecDecl{
		Name:      "MyDecimalAppend",
		GoType:    "example.com/v1.Decimal",
		WireType:  codecs.WireLengthDelim,
		EmitFn:    "EmitMyDecimalAppend",
		DecodeFn:  "DecodeMyDecimalAppend",
		PkgImport: "example.com/v1",
	}
	if d != want {
		t.Fatalf("NewDecimalAppendDecl mismatch:\n got %+v\nwant %+v", d, want)
	}
	if k := d.Kind(); k != codecs.CodecKindMaterializing {
		t.Fatalf("Kind() = %v, want CodecKindMaterializing", k)
	}

	r := NewBuiltinRegistry()
	if err := r.Register(d); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := r.Lookup("MyDecimalAppend"); !ok {
		t.Fatal("Lookup after Register failed")
	}
}

// jsonPayload is a representative struct used to exercise StreamJSONBytes
// across the common JSON shapes: scalars, nested struct, map (with
// json's deterministic key sort), and a slice.
type jsonPayload struct {
	ID     int            `json:"id"`
	Name   string         `json:"name"`
	Tags   []string       `json:"tags"`
	Attrs  map[string]int `json:"attrs"`
	Nested *jsonPayload   `json:"nested,omitempty"`
}

// TestStreamJSONBytesRoundTrip exercises StreamJSONBytes / DecodeJSONBytes
// on a representative set of JSON-encodable values: empty struct, scalars,
// nested struct, maps with multiple keys (sorted by encoding/json), and a
// large payload to exercise the length-prefix path on a multi-byte
// varint.
func TestStreamJSONBytesRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   jsonPayload
	}{
		{"empty", jsonPayload{}},
		{"scalars", jsonPayload{ID: 42, Name: "alice"}},
		{"slice", jsonPayload{ID: 1, Tags: []string{"a", "b", "c"}}},
		{"map", jsonPayload{ID: 2, Attrs: map[string]int{"x": 1, "y": 2, "z": 3}}},
		{"nested", jsonPayload{ID: 3, Nested: &jsonPayload{ID: 4, Name: "child"}}},
		{"wide", jsonPayload{ID: 5, Name: strings.Repeat("x", 4096)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := gsbm.NewWriter(nil)
			if err := StreamJSONBytes(w, tc.in); err != nil {
				t.Fatalf("stream: %v", err)
			}
			r := gsbm.NewReader(w.Bytes())
			var got jsonPayload
			if err := DecodeJSONBytes(r, &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !reflect.DeepEqual(got, tc.in) {
				t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, tc.in)
			}
		})
	}
}

// TestStreamJSONBytesMatchesHandRolled pins the byte output of
// StreamJSONBytes against a hand-rolled `json.Marshal` + `w.WriteBytes`
// composition — the codec's defining property is that it adds no
// framing of its own beyond what the LENGTH_DELIM envelope provides.
// A divergence here would mean the codec silently transformed the
// payload (e.g. compression, escaping) and broken the documented
// contract.
func TestStreamJSONBytesMatchesHandRolled(t *testing.T) {
	cases := []jsonPayload{
		{},
		{ID: 42, Name: "alice"},
		{ID: 1, Tags: []string{"a", "b", "c"}},
		{ID: 2, Attrs: map[string]int{"x": 1, "y": 2, "z": 3}},
		{ID: 5, Name: strings.Repeat("x", 4096)},
	}
	for i, tc := range cases {
		wA := gsbm.NewWriter(nil)
		if err := StreamJSONBytes(wA, tc); err != nil {
			t.Fatalf("case %d StreamJSONBytes: %v", i, err)
		}
		wB := gsbm.NewWriter(nil)
		b, err := json.Marshal(tc)
		if err != nil {
			t.Fatalf("case %d json.Marshal: %v", i, err)
		}
		wB.WriteBytes(b)
		if !bytes.Equal(wA.Bytes(), wB.Bytes()) {
			t.Fatalf("case %d wire drift:\n stream: % x\n   hand: % x", i, wA.Bytes(), wB.Bytes())
		}
	}
}

// TestStreamJSONBytesSizeMatchesWrite is the streaming-codec
// lockstep: the size-mode Writer fed StreamJSONBytes must accumulate
// the same byte count the write-mode Writer produces, since
// gsbm.Marshal's two-pass flow runs StreamFn against both Writer
// modes against the same codec call. Streaming intentionally runs
// the body twice; both passes must agree on the byte count or the
// length prefix shifts every following field.
func TestStreamJSONBytesSizeMatchesWrite(t *testing.T) {
	cases := []jsonPayload{
		{},
		{ID: 42, Name: "alice"},
		{ID: 1, Tags: []string{"a", "b", "c"}},
		{ID: 2, Attrs: map[string]int{"x": 1, "y": 2, "z": 3}},
		{ID: 5, Name: strings.Repeat("x", 4096)},
	}
	for i, tc := range cases {
		bw := gsbm.NewWriter(nil)
		if err := StreamJSONBytes(bw, tc); err != nil {
			t.Fatalf("case %d write-mode: %v", i, err)
		}
		cw := gsbm.NewCountingWriter()
		if err := StreamJSONBytes(cw, tc); err != nil {
			t.Fatalf("case %d size-mode: %v", i, err)
		}
		got := cw.Size()
		want := len(bw.Bytes())
		if got != want {
			t.Errorf("case %d: size-mode = %d, write-mode wrote %d", i, got, want)
		}
	}
}

// TestStreamJSONBytesMarshalError verifies that an unmarshalable input
// surfaces the underlying json.Marshal error rather than silently
// writing a partial body. A function value is the standard
// trigger for an unsupported-type error from encoding/json.
func TestStreamJSONBytesMarshalError(t *testing.T) {
	w := gsbm.NewWriter(nil)
	err := StreamJSONBytes(w, func() {})
	if err == nil {
		t.Fatal("expected error for unmarshalable value, got nil")
	}
	// json.Marshal returns *json.UnsupportedTypeError; we just require the
	// error is surfaced and the Writer is left unchanged (no partial body).
	if len(w.Bytes()) != 0 {
		t.Fatalf("Writer must not record partial body on marshal error, got % x", w.Bytes())
	}
}

// TestDecodeJSONBytesUnmarshalError verifies that a payload whose
// LENGTH_DELIM body is not valid JSON returns json.Unmarshal's error
// rather than silently leaving *v zero-valued and succeeding.
func TestDecodeJSONBytesUnmarshalError(t *testing.T) {
	w := gsbm.NewWriter(nil)
	w.WriteBytes([]byte("{not valid json"))
	r := gsbm.NewReader(w.Bytes())
	var got jsonPayload
	err := DecodeJSONBytes(r, &got)
	if err == nil {
		t.Fatal("expected json.Unmarshal error, got nil")
	}
}

// TestNewStreamingJSONDecl checks the decl-construction shape and that
// the result registers cleanly alongside the built-ins. The Kind() must
// be CodecKindStreaming — the streaming-shape signature lives or dies
// on that classification (codegen branches on it).
func TestNewStreamingJSONDecl(t *testing.T) {
	d := NewStreamingJSONDecl(
		"StreamingJSON",
		"example.com/v1.LargePayload",
		"StreamMyJSON",
		"DecodeMyJSON",
		"example.com/v1",
	)
	want := codecs.CodecDecl{
		Name:      "StreamingJSON",
		GoType:    "example.com/v1.LargePayload",
		WireType:  codecs.WireLengthDelim,
		StreamFn:  "StreamMyJSON",
		DecodeFn:  "DecodeMyJSON",
		PkgImport: "example.com/v1",
	}
	if d != want {
		t.Fatalf("NewStreamingJSONDecl mismatch:\n got %+v\nwant %+v", d, want)
	}
	if k := d.Kind(); k != codecs.CodecKindStreaming {
		t.Fatalf("Kind() = %v, want CodecKindStreaming", k)
	}

	r := NewBuiltinRegistry()
	if err := r.Register(d); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := r.Lookup("StreamingJSON"); !ok {
		t.Fatal("Lookup after Register failed")
	}
}

// binProbe is a minimal BinaryDecimal-satisfying decimal used to
// exercise the binary decimal codec. The three parts — unsigned
// coefficient, scale, sign — are exactly what the codec encodes, so a
// round-trip is correct iff all three survive.
type binProbe struct {
	coef  uint64
	scale int
	neg   bool
}

func (d binProbe) Coef() uint64 { return d.coef }
func (d binProbe) Scale() int   { return d.scale }
func (d binProbe) IsNeg() bool  { return d.neg }

// reconstructBinProbe is the user-supplied binding callback for binProbe.
// It accepts the unsigned uint64 coefficient directly — binProbe has no
// signed-int64 constructor wrinkle — so coefficients ≥ 2^63 pass straight
// through.
func reconstructBinProbe(coef uint64, scale int, neg bool) (binProbe, error) {
	return binProbe{coef: coef, scale: scale, neg: neg}, nil
}

// binDecimalCases is the shared round-trip / lockstep table: zero,
// positive, negative, scale 0, max accepted scale, max-uint64
// coefficient, a trailing-zero scale, and a coefficient ≥ 2^63 (the
// decode-wrinkle case a signed-int64 constructor cannot handle).
var binDecimalCases = []struct {
	name string
	in   binProbe
}{
	{"zero", binProbe{coef: 0, scale: 0, neg: false}},
	{"positive", binProbe{coef: 12345, scale: 2, neg: false}},
	{"negative", binProbe{coef: 12345, scale: 2, neg: true}},
	{"scale-0", binProbe{coef: 999, scale: 0, neg: false}},
	{"max-scale", binProbe{coef: 1, scale: maxBinaryDecimalScale - 1, neg: false}},
	{"max-coef", binProbe{coef: math.MaxUint64, scale: 0, neg: false}},
	{"trailing-zero-scale", binProbe{coef: 100, scale: 3, neg: false}},
	{"coef-ge-2pow63", binProbe{coef: 1<<63 + 5, scale: 4, neg: true}},
}

// TestDecimalBinaryRoundTrip encodes each table value, decodes it back
// through reconstructBinProbe, and requires all three parts to survive —
// including the coefficient ≥ 2^63 case that a signed-int64 constructor
// would silently corrupt.
func TestDecimalBinaryRoundTrip(t *testing.T) {
	for _, tc := range binDecimalCases {
		t.Run(tc.name, func(t *testing.T) {
			w := gsbm.NewWriter(nil)
			if err := EncodeDecimalBinary(w, tc.in); err != nil {
				t.Fatalf("encode: %v", err)
			}
			r := gsbm.NewReader(w.Bytes())
			var got binProbe
			if err := DecodeDecimalBinary(r, &got, reconstructBinProbe); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got != tc.in {
				t.Fatalf("round-trip: got %+v want %+v", got, tc.in)
			}
		})
	}
}

// TestSizeDecimalBinaryMatchesEncode is the analytic-codec lockstep:
// SizeDecimalBinary(v) must equal len(bytes written by
// EncodeDecimalBinary(w, v)) for every input. A mismatch corrupts bodyLen
// at the call site and shifts every following field on the wire.
func TestSizeDecimalBinaryMatchesEncode(t *testing.T) {
	for _, tc := range binDecimalCases {
		t.Run(tc.name, func(t *testing.T) {
			w := gsbm.NewWriter(nil)
			if err := EncodeDecimalBinary(w, tc.in); err != nil {
				t.Fatalf("encode: %v", err)
			}
			got := SizeDecimalBinary(tc.in)
			want := len(w.Bytes())
			if got != want {
				t.Errorf("SizeDecimalBinary(%+v) = %d, encode wrote %d", tc.in, got, want)
			}
		})
	}
}

// TestDecimalBinaryWireBytes pins the exact wire bytes for a known value:
// coef=12345, scale=2, neg=true encodes as uvarint(12345) ++
// uvarint(2<<1|1). 12345 = 0x3039 → varint 0xB9 0x60; 2<<1|1 = 5 → 0x05.
func TestDecimalBinaryWireBytes(t *testing.T) {
	w := gsbm.NewWriter(nil)
	if err := EncodeDecimalBinary(w, binProbe{coef: 12345, scale: 2, neg: true}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := []byte{0xB9, 0x60, 0x05}
	if !bytes.Equal(w.Bytes(), want) {
		t.Fatalf("wire bytes: got % x, want % x", w.Bytes(), want)
	}
}

// TestDecimalBinaryZeroAlloc pins the headline property of issue #44: the
// encode and size functions touch only accessors and varint primitives,
// so both run at exactly 0 allocs/op.
func TestDecimalBinaryZeroAlloc(t *testing.T) {
	v := binProbe{coef: 123456789, scale: 4, neg: true}
	buf := make([]byte, 0, 64)
	w := gsbm.NewWriter(buf)

	encAllocs := testing.AllocsPerRun(100, func() {
		w.Reset(buf)
		if err := EncodeDecimalBinary(w, v); err != nil {
			t.Fatalf("encode: %v", err)
		}
	})
	if encAllocs != 0 {
		t.Errorf("EncodeDecimalBinary: %.1f allocs/op, want 0", encAllocs)
	}

	sizeAllocs := testing.AllocsPerRun(100, func() {
		_ = SizeDecimalBinary(v)
	})
	if sizeAllocs != 0 {
		t.Errorf("SizeDecimalBinary: %.1f allocs/op, want 0", sizeAllocs)
	}
}

// TestEncodeDecimalBinaryRejectsBadScale verifies the encode-side guard:
// a negative scale or one at/above 2^62 cannot round-trip through the
// scale<<1 packing and must be refused rather than silently corrupted.
func TestEncodeDecimalBinaryRejectsBadScale(t *testing.T) {
	cases := []struct {
		name  string
		scale int
	}{
		{"negative", -1},
		{"at-cap", maxBinaryDecimalScale},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := gsbm.NewWriter(nil)
			err := EncodeDecimalBinary(w, binProbe{coef: 1, scale: tc.scale})
			if !errors.Is(err, errScaleOutOfRange) {
				t.Fatalf("expected errScaleOutOfRange, got %v", err)
			}
		})
	}
}

// TestDecodeDecimalBinaryRejectsBadScale exercises the decode-side guard:
// a malformed body whose packed scale is at/above 2^62 must be refused.
func TestDecodeDecimalBinaryRejectsBadScale(t *testing.T) {
	w := gsbm.NewWriter(nil)
	w.WriteUvarint(1)                                  // coef
	w.WriteUvarint(uint64(maxBinaryDecimalScale) << 1) // packed scale = 2^62, sign 0
	r := gsbm.NewReader(w.Bytes())
	var got binProbe
	err := DecodeDecimalBinary(r, &got, reconstructBinProbe)
	if !errors.Is(err, errScaleOutOfRange) {
		t.Fatalf("expected errScaleOutOfRange, got %v", err)
	}
}

// TestDecodeDecimalBinaryReconstructError verifies a failing reconstruct
// callback surfaces its error rather than storing a partial value.
func TestDecodeDecimalBinaryReconstructError(t *testing.T) {
	w := gsbm.NewWriter(nil)
	if err := EncodeDecimalBinary(w, binProbe{coef: 7, scale: 1}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	r := gsbm.NewReader(w.Bytes())
	want := errors.New("reconstruct failed")
	reconstruct := func(coef uint64, scale int, neg bool) (int, error) { return 0, want }
	got := 99
	err := DecodeDecimalBinary(r, &got, reconstruct)
	if !errors.Is(err, want) {
		t.Fatalf("expected reconstruct error, got %v", err)
	}
	if got != 99 {
		t.Fatalf("reconstruct failure must leave *v untouched, got %d", got)
	}
}

// TestDecodeDecimalBinaryRejectsTruncatedBody covers the two reader-error
// paths: an empty body fails the coef read, a coef-only body fails the
// packed-scale read.
func TestDecodeDecimalBinaryRejectsTruncatedBody(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"empty", nil},
		{"coef-only", func() []byte {
			w := gsbm.NewWriter(nil)
			w.WriteUvarint(42)
			return w.Bytes()
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := gsbm.NewReader(tc.body)
			got := binProbe{coef: 1, scale: 1}
			if err := DecodeDecimalBinary(r, &got, reconstructBinProbe); err == nil {
				t.Fatalf("expected error decoding truncated body, got nil (got=%+v)", got)
			}
		})
	}
}

func TestNewDecimalStringDecl(t *testing.T) {
	d := NewDecimalStringDecl(
		"MyDecimal",
		"example.com/v1.Decimal",
		"EmitMyDecimal",
		"DecodeMyDecimal",
		"example.com/v1",
	)
	want := codecs.CodecDecl{
		Name:      "MyDecimal",
		GoType:    "example.com/v1.Decimal",
		WireType:  codecs.WireLengthDelim,
		EmitFn:    "EmitMyDecimal",
		DecodeFn:  "DecodeMyDecimal",
		PkgImport: "example.com/v1",
	}
	if d != want {
		t.Fatalf("NewDecimalStringDecl mismatch:\n got %+v\nwant %+v", d, want)
	}
	if k := d.Kind(); k != codecs.CodecKindMaterializing {
		t.Fatalf("Kind() = %v, want CodecKindMaterializing", k)
	}

	// Registers cleanly alongside built-ins.
	r := NewBuiltinRegistry()
	if err := r.Register(d); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := r.Lookup("MyDecimal"); !ok {
		t.Fatal("Lookup after Register failed")
	}
}
