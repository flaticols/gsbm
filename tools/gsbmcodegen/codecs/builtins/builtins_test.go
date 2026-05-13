package builtins

import (
	"bytes"
	"errors"
	"fmt"
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
