package builtins

import (
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
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := stringerDecimal{s: tc.in}
			w := gsbm.NewWriter(nil)
			if err := EncodeDecimalString(w, in); err != nil {
				t.Fatalf("encode: %v", err)
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

func TestDecimalStringParseError(t *testing.T) {
	// A failing parse function must surface its error from
	// DecodeDecimalString — the codec must not silently coerce a parse
	// failure into a zero value.
	w := gsbm.NewWriter(nil)
	if err := EncodeDecimalString(w, stringerDecimal{s: "not a number"}); err != nil {
		t.Fatalf("encode: %v", err)
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
	if err := EncodeDecimalString(w, intStringer(in)); err != nil {
		t.Fatalf("encode: %v", err)
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
		"EncodeMyDecimal",
		"DecodeMyDecimal",
		"example.com/v1",
	)
	want := codecs.CodecDecl{
		Name:      "MyDecimal",
		GoType:    "example.com/v1.Decimal",
		WireType:  codecs.WireLengthDelim,
		EncodeFn:  "EncodeMyDecimal",
		DecodeFn:  "DecodeMyDecimal",
		PkgImport: "example.com/v1",
	}
	if d != want {
		t.Fatalf("NewDecimalStringDecl mismatch:\n got %+v\nwant %+v", d, want)
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
