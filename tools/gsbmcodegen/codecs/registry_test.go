package codecs

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func sampleDecl() CodecDecl {
	return CodecDecl{
		Name:      "Time",
		GoType:    "time.Time",
		WireType:  WireLengthDelim,
		EncodeFn:  "EncodeTime",
		DecodeFn:  "DecodeTime",
		SizeFn:    "SizeTime",
		PkgImport: "example.com/codecs",
	}
}

func TestRegistryRegisterLookup(t *testing.T) {
	r := NewRegistry()
	c := sampleDecl()
	if err := r.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, ok := r.Lookup("Time")
	if !ok {
		t.Fatal("Lookup: not found")
	}
	if got != c {
		t.Fatalf("Lookup roundtrip mismatch:\n got %+v\nwant %+v", got, c)
	}
}

func TestRegistryDuplicateRegistration(t *testing.T) {
	r := NewRegistry()
	c := sampleDecl()
	if err := r.Register(c); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	// Identical re-registration is a no-op.
	if err := r.Register(c); err != nil {
		t.Fatalf("identical re-Register should be no-op, got %v", err)
	}
	// Different CodecDecl under the same name is an error.
	c2 := c
	c2.EncodeFn = "OtherEncode"
	err := r.Register(c2)
	if err == nil {
		t.Fatal("expected duplicate error, got nil")
	}
	if !errors.Is(err, ErrDuplicateCodec) {
		t.Fatalf("expected ErrDuplicateCodec, got %v", err)
	}
}

func TestRegistryLookupMiss(t *testing.T) {
	r := NewRegistry()
	if _, ok := r.Lookup("Missing"); ok {
		t.Fatal("Lookup of unregistered name should return false")
	}
}

func TestRegistryRejectsInvalidDecl(t *testing.T) {
	r := NewRegistry()
	cases := []struct {
		name string
		c    CodecDecl
	}{
		{"empty-name", CodecDecl{WireType: WireVarint, EncodeFn: "E", DecodeFn: "D", SizeFn: "S"}},
		{"empty-encode", CodecDecl{Name: "X", WireType: WireVarint, DecodeFn: "D", SizeFn: "S"}},
		{"empty-decode", CodecDecl{Name: "X", WireType: WireVarint, EncodeFn: "E", SizeFn: "S"}},
		{"empty-size", CodecDecl{Name: "X", WireType: WireVarint, EncodeFn: "E", DecodeFn: "D"}},
		{"emit-empty-decode", CodecDecl{Name: "X", WireType: WireLengthDelim, EmitFn: "Em"}},
		{"unknown-wire", CodecDecl{Name: "X", WireType: "bogus", EncodeFn: "E", DecodeFn: "D", SizeFn: "S"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := r.Register(tc.c); err == nil {
				t.Fatalf("expected error for %+v", tc.c)
			}
		})
	}
}

func TestRegistryNames(t *testing.T) {
	r := NewRegistry()
	for _, n := range []string{"Zeta", "Alpha", "Mu"} {
		c := sampleDecl()
		c.Name = n
		if err := r.Register(c); err != nil {
			t.Fatalf("Register %s: %v", n, err)
		}
	}
	got := r.Names()
	want := []string{"Alpha", "Mu", "Zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Names: got %v want %v", got, want)
	}
}

// TestRegistryMissingSizeFnDiagnostic — a CodecDecl missing SizeFn must
// be rejected with the `codec/missing-size-fn` diagnostic code in the
// error string. The codegen lint pipeline keys on that prefix, so the
// exact substring is load-bearing.
func TestRegistryMissingSizeFnDiagnostic(t *testing.T) {
	r := NewRegistry()
	c := sampleDecl()
	c.SizeFn = ""
	err := r.Register(c)
	if err == nil {
		t.Fatal("expected error registering decl without SizeFn, got nil")
	}
	if !strings.Contains(err.Error(), "codec/missing-size-fn") {
		t.Fatalf("expected diagnostic code 'codec/missing-size-fn', got %q", err.Error())
	}
	if !strings.Contains(err.Error(), `"Time"`) {
		t.Fatalf("expected codec name in diagnostic, got %q", err.Error())
	}
}

// TestRegistryNeitherPairDiagnostic — a CodecDecl that declares neither
// the analytic pair (SizeFn, EncodeFn) nor the materializing EmitFn must
// surface the `codec/missing-size-fn` diagnostic code per docs/spec.md
// §5.8. Without the prefix the codegen lint pipeline cannot classify the
// failure, and users would see a different surface for two cases the
// spec presents as one.
func TestRegistryNeitherPairDiagnostic(t *testing.T) {
	r := NewRegistry()
	c := sampleDecl()
	c.EncodeFn = ""
	c.SizeFn = ""
	err := r.Register(c)
	if err == nil {
		t.Fatal("expected error registering decl without any encode/emit, got nil")
	}
	if !strings.Contains(err.Error(), "codec/missing-size-fn") {
		t.Fatalf("expected diagnostic code 'codec/missing-size-fn', got %q", err.Error())
	}
	if !strings.Contains(err.Error(), `"Time"`) {
		t.Fatalf("expected codec name in diagnostic, got %q", err.Error())
	}
}

func TestUnregisteredError(t *testing.T) {
	err := UnregisteredError("DecimalString", []string{"Time"})
	msg := err.Error()
	if !strings.Contains(msg, "codec/unregistered") {
		t.Errorf("missing diagnostic code in: %s", msg)
	}
	if !strings.Contains(msg, `"DecimalString"`) {
		t.Errorf("missing offending name in: %s", msg)
	}
	if !strings.Contains(msg, "Time") {
		t.Errorf("missing registered-names hint in: %s", msg)
	}
}

// TestRegistryAcceptsMaterializingDecl — a CodecDecl with EmitFn alone
// (no SizeFn or EncodeFn) is the materializing-codec shape and must
// register cleanly. Round-trip via Lookup preserves all fields including
// EmitFn.
func TestRegistryAcceptsMaterializingDecl(t *testing.T) {
	r := NewRegistry()
	c := CodecDecl{
		Name:      "DecimalString",
		GoType:    "myapp.Decimal",
		WireType:  WireLengthDelim,
		DecodeFn:  "DecodeDecimalString",
		EmitFn:    "EmitDecimalString",
		PkgImport: "example.com/codecs",
	}
	if err := r.Register(c); err != nil {
		t.Fatalf("Register materializing codec: %v", err)
	}
	got, ok := r.Lookup("DecimalString")
	if !ok {
		t.Fatal("Lookup: not found")
	}
	if got != c {
		t.Fatalf("Lookup roundtrip mismatch:\n got %+v\nwant %+v", got, c)
	}
	if k := got.Kind(); k != CodecKindMaterializing {
		t.Fatalf("Kind() = %v, want CodecKindMaterializing", k)
	}
}

// TestRegistryConflictingKindsDiagnostic — declaring any two-or-more of
// (SizeFn/EncodeFn, EmitFn, StreamFn) must fail with the load-bearing
// `codec/conflicting-kinds` diagnostic prefix. The codegen lint pipeline
// keys on that prefix; widen the matrix here so every cross-kind overlap
// is covered.
func TestRegistryConflictingKindsDiagnostic(t *testing.T) {
	r := NewRegistry()
	cases := []struct {
		name string
		c    CodecDecl
	}{
		{
			"emit+encode",
			CodecDecl{Name: "X", WireType: WireLengthDelim, DecodeFn: "D", EncodeFn: "E", EmitFn: "Em"},
		},
		{
			"emit+size",
			CodecDecl{Name: "X", WireType: WireLengthDelim, DecodeFn: "D", SizeFn: "S", EmitFn: "Em"},
		},
		{
			"emit+size+encode",
			CodecDecl{Name: "X", WireType: WireLengthDelim, DecodeFn: "D", SizeFn: "S", EncodeFn: "E", EmitFn: "Em"},
		},
		{
			"stream+encode",
			CodecDecl{Name: "X", WireType: WireLengthDelim, DecodeFn: "D", EncodeFn: "E", StreamFn: "St"},
		},
		{
			"stream+size",
			CodecDecl{Name: "X", WireType: WireLengthDelim, DecodeFn: "D", SizeFn: "S", StreamFn: "St"},
		},
		{
			"stream+size+encode",
			CodecDecl{Name: "X", WireType: WireLengthDelim, DecodeFn: "D", SizeFn: "S", EncodeFn: "E", StreamFn: "St"},
		},
		{
			"stream+emit",
			CodecDecl{Name: "X", WireType: WireLengthDelim, DecodeFn: "D", EmitFn: "Em", StreamFn: "St"},
		},
		{
			"stream+emit+encode",
			CodecDecl{Name: "X", WireType: WireLengthDelim, DecodeFn: "D", EncodeFn: "E", EmitFn: "Em", StreamFn: "St"},
		},
		{
			"all-four",
			CodecDecl{Name: "X", WireType: WireLengthDelim, DecodeFn: "D", SizeFn: "S", EncodeFn: "E", EmitFn: "Em", StreamFn: "St"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := r.Register(tc.c)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), "codec/conflicting-kinds") {
				t.Fatalf("expected diagnostic 'codec/conflicting-kinds', got %q", err.Error())
			}
		})
	}
}

// TestRegistryAcceptsStreamingDecl — a CodecDecl with StreamFn alone (no
// SizeFn, EncodeFn, or EmitFn) is the streaming-codec shape and must
// register cleanly. Round-trip via Lookup preserves all fields including
// StreamFn, and Kind() reports CodecKindStreaming.
func TestRegistryAcceptsStreamingDecl(t *testing.T) {
	r := NewRegistry()
	c := CodecDecl{
		Name:      "StreamingJSON",
		GoType:    "myapp.LargePayload",
		WireType:  WireLengthDelim,
		DecodeFn:  "DecodeStreamingJSON",
		StreamFn:  "StreamJSONBytes",
		PkgImport: "example.com/codecs",
	}
	if err := r.Register(c); err != nil {
		t.Fatalf("Register streaming codec: %v", err)
	}
	got, ok := r.Lookup("StreamingJSON")
	if !ok {
		t.Fatal("Lookup: not found")
	}
	if got != c {
		t.Fatalf("Lookup roundtrip mismatch:\n got %+v\nwant %+v", got, c)
	}
	if k := got.Kind(); k != CodecKindStreaming {
		t.Fatalf("Kind() = %v, want CodecKindStreaming", k)
	}
}

func TestCodecDeclKind(t *testing.T) {
	analytic := sampleDecl()
	if k := analytic.Kind(); k != CodecKindAnalytic {
		t.Errorf("analytic Kind() = %v, want CodecKindAnalytic", k)
	}
	mat := CodecDecl{
		Name: "M", GoType: "T", WireType: WireLengthDelim,
		DecodeFn: "D", EmitFn: "Em", PkgImport: "p",
	}
	if k := mat.Kind(); k != CodecKindMaterializing {
		t.Errorf("materializing Kind() = %v, want CodecKindMaterializing", k)
	}
	stream := CodecDecl{
		Name: "S", GoType: "T", WireType: WireLengthDelim,
		DecodeFn: "D", StreamFn: "St", PkgImport: "p",
	}
	if k := stream.Kind(); k != CodecKindStreaming {
		t.Errorf("streaming Kind() = %v, want CodecKindStreaming", k)
	}
	// Invalid combinations return zero. Register rejects these, but
	// Kind itself is total — exercise the fallthrough.
	invalid := CodecDecl{Name: "X"}
	if k := invalid.Kind(); k != 0 {
		t.Errorf("invalid Kind() = %v, want 0", k)
	}
	overlap := CodecDecl{
		Name: "X", WireType: WireLengthDelim, DecodeFn: "D",
		EmitFn: "Em", StreamFn: "St",
	}
	if k := overlap.Kind(); k != 0 {
		t.Errorf("overlap Kind() = %v, want 0 (invalid)", k)
	}
}

func TestWireIdent(t *testing.T) {
	cases := map[string]string{
		WireVarint:      "WireVarint",
		WireFixed64:     "WireFixed64",
		WireFixed32:     "WireFixed32",
		WireLengthDelim: "WireLengthDelim",
		"bogus":         "",
	}
	for in, want := range cases {
		if got := WireIdent(in); got != want {
			t.Errorf("WireIdent(%q) = %q, want %q", in, got, want)
		}
	}
}
