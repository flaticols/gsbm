package builtins

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.flaticols.dev/gsbm/storage/gsbm"
	"go.flaticols.dev/gsbm/tools/gsbmcodegen/codecs"
)

func TestTimeUnixNanoRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   time.Time
	}{
		// Zero time. time.Time{}.UnixNano() is a large negative offset
		// from the Unix epoch (year-1 instant), which still has to
		// round-trip exactly through the codec.
		{"zero", time.Time{}},
		// Negative nanos = pre-1970. The zigzag varint encoding handles
		// negatives without loss; this is the explicit edge case the
		// plan calls out.
		{"pre-epoch", time.Date(1969, 12, 31, 23, 59, 0, 0, time.UTC)},
		// Far-past nanos.
		{"deep-pre-epoch", time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)},
		// A representative post-epoch value with non-zero nanos.
		{"post-epoch", time.Date(2026, 5, 11, 12, 34, 56, 789, time.UTC)},
		// Far future, within int64 nanos range.
		{"far-future", time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := gsbm.NewWriter(nil)
			if err := EncodeTimeUnixNano(w, tc.in); err != nil {
				t.Fatalf("encode: %v", err)
			}
			r := gsbm.NewReader(w.Bytes())
			var got time.Time
			if err := DecodeTimeUnixNano(r, &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			// Compare by UnixNano: the codec only carries the instant,
			// not the Location, so a direct Equal() would compare
			// time.Local vs UTC and fail spuriously.
			if got.UnixNano() != tc.in.UnixNano() {
				t.Fatalf("round-trip: got %d want %d (%s vs %s)",
					got.UnixNano(), tc.in.UnixNano(), got, tc.in)
			}
		})
	}
}

func TestTimeUnixNanoZeroWireForm(t *testing.T) {
	// The zero time's UnixNano is a large negative integer. Encoding via
	// WriteVarint (zigzag) must produce a non-empty payload; an empty
	// payload would mean the codec silently swapped to "skip on zero",
	// which the spec forbids (the wrapper controls presence).
	w := gsbm.NewWriter(nil)
	if err := EncodeTimeUnixNano(w, time.Time{}); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(w.Bytes()) == 0 {
		t.Fatal("zero time encoded to empty payload — codec must always emit a varint")
	}
}

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
			if err := EmitDecimalString(w, in, uintptr(0x100+i)); err != nil {
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

// TestSizeTimeUnixNanoMatchesEncode is the SizeFn/EncodeFn lockstep
// check for the TimeUnixNano codec: SizeTimeUnixNano(t) must equal
// len(bytes emitted by EncodeTimeUnixNano(w, t)) for every input. A
// mismatch corrupts bodyLen for any caller wiring this codec into a
// generated SizeGSBM.
func TestSizeTimeUnixNanoMatchesEncode(t *testing.T) {
	cases := []time.Time{
		time.Time{},
		time.Unix(0, 0),
		time.Unix(1, 0),
		time.Date(1969, 12, 31, 23, 59, 0, 0, time.UTC),
		time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 5, 11, 12, 34, 56, 789, time.UTC),
		time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	for _, tc := range cases {
		w := gsbm.NewWriter(nil)
		if err := EncodeTimeUnixNano(w, tc); err != nil {
			t.Fatalf("encode %s: %v", tc, err)
		}
		got := SizeTimeUnixNano(tc)
		want := len(w.Bytes())
		if got != want {
			t.Errorf("SizeTimeUnixNano(%s) = %d, encode wrote %d", tc, got, want)
		}
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
		cs := uintptr(0x200 + i)
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
	cs := uintptr(0xcafe)
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
	cs := uintptr(0xfeed)
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
	if err := EmitDecimalString(w, stringerDecimal{s: "not a number"}, uintptr(0x301)); err != nil {
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
	if err := EmitDecimalString(w, intStringer(in), uintptr(0x302)); err != nil {
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

func TestNewBuiltinRegistryHasTimeUnixNano(t *testing.T) {
	r := NewBuiltinRegistry()
	c, ok := r.Lookup("TimeUnixNano")
	if !ok {
		t.Fatal("TimeUnixNano not registered")
	}
	if c != TimeUnixNanoDecl {
		t.Fatalf("registered decl differs:\n got %+v\nwant %+v", c, TimeUnixNanoDecl)
	}
	// Built-in registry starts clean — no surprise extra codecs.
	if names := r.Names(); len(names) != 1 {
		t.Fatalf("NewBuiltinRegistry: got %v, want [TimeUnixNano]", names)
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
