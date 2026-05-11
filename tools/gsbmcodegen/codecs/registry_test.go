package codecs

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func sampleDecl() CodecDecl {
	return CodecDecl{
		Name:      "TimeUnixNano",
		GoType:    "time.Time",
		WireType:  WireVarint,
		EncodeFn:  "EncodeTimeUnixNano",
		DecodeFn:  "DecodeTimeUnixNano",
		PkgImport: "example.com/codecs",
	}
}

func TestRegistryRegisterLookup(t *testing.T) {
	r := NewRegistry()
	c := sampleDecl()
	if err := r.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, ok := r.Lookup("TimeUnixNano")
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
		{"empty-name", CodecDecl{WireType: WireVarint, EncodeFn: "E", DecodeFn: "D"}},
		{"empty-encode", CodecDecl{Name: "X", WireType: WireVarint, DecodeFn: "D"}},
		{"empty-decode", CodecDecl{Name: "X", WireType: WireVarint, EncodeFn: "E"}},
		{"unknown-wire", CodecDecl{Name: "X", WireType: "bogus", EncodeFn: "E", DecodeFn: "D"}},
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

func TestUnregisteredError(t *testing.T) {
	err := UnregisteredError("DecimalString", []string{"TimeUnixNano"})
	msg := err.Error()
	if !strings.Contains(msg, "codec/unregistered") {
		t.Errorf("missing diagnostic code in: %s", msg)
	}
	if !strings.Contains(msg, `"DecimalString"`) {
		t.Errorf("missing offending name in: %s", msg)
	}
	if !strings.Contains(msg, "TimeUnixNano") {
		t.Errorf("missing registered-names hint in: %s", msg)
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
