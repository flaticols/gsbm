package gsbmschema

import (
	"reflect"
	"testing"
)

// TestParseFieldTag covers every shape ParseFieldTag is expected to
// understand, plus the input cases the validator relies on to surface
// errors (tag 0, missing tag, unknown options).
func TestParseFieldTag(t *testing.T) {
	cases := []struct {
		name string
		tag  reflect.StructTag
		want FieldTag
		err  bool
	}{
		{"none", ``, FieldTag{}, false},
		{"plain", `bin:"5"`, FieldTag{Set: true, Tag: 5}, false},
		{"deprecated", `bin:"7,deprecated"`, FieldTag{Set: true, Tag: 7, Deprecated: true}, false},
		{"compat_write", `bin:"7,deprecated,compat_write"`, FieldTag{Set: true, Tag: 7, Deprecated: true, CompatWrite: true}, false},
		{"compat_write-reversed", `bin:"7,compat_write,deprecated"`, FieldTag{Set: true, Tag: 7, Deprecated: true, CompatWrite: true}, false},
		{"compat_write-standalone", `bin:"7,compat_write"`, FieldTag{Set: true, Tag: 7}, true},
		{"compat_write-repeated", `bin:"7,deprecated,compat_write,compat_write"`, FieldTag{Set: true, Tag: 7, Deprecated: true, CompatWrite: true}, true},
		{"deprecated-repeated", `bin:"7,deprecated,deprecated"`, FieldTag{Set: true, Tag: 7, Deprecated: true}, true},
		{"custom", `bin:"3,custom=PriceCodec"`, FieldTag{Set: true, Tag: 3, Custom: "PriceCodec"}, false},
		{"custom-time", `bin:"3,custom=Time"`, FieldTag{Set: true, Tag: 3, Custom: "Time"}, false},
		{"custom-with-deprecated", `bin:"3,custom=DecimalString,deprecated"`, FieldTag{Set: true, Tag: 3, Custom: "DecimalString", Deprecated: true}, false},
		{"custom-repeated", `bin:"3,custom=Foo,custom=Bar"`, FieldTag{Set: true, Tag: 3}, true},
		{"id_ref", `bin:"3,id_ref"`, FieldTag{Set: true, Tag: 3, CycleBreakViaID: true}, false},
		{"id_ref-with-deprecated", `bin:"3,id_ref,deprecated"`, FieldTag{Set: true, Tag: 3, CycleBreakViaID: true, Deprecated: true}, false},
		{"id_ref-with-deprecated-compat", `bin:"3,id_ref,deprecated,compat_write"`, FieldTag{Set: true, Tag: 3, CycleBreakViaID: true, Deprecated: true, CompatWrite: true}, false},
		{"id_ref-repeated", `bin:"3,id_ref,id_ref"`, FieldTag{Set: true, Tag: 3, CycleBreakViaID: true}, true},
		{"id_ref-with-custom", `bin:"3,id_ref,custom=Foo"`, FieldTag{Set: true, Tag: 3}, true},
		{"custom-with-id_ref", `bin:"3,custom=Foo,id_ref"`, FieldTag{Set: true, Tag: 3}, true},
		{"type-int8", `bin:"5,type=int8"`, FieldTag{Set: true, Tag: 5, WireOverride: "int8"}, false},
		{"type-int16", `bin:"5,type=int16"`, FieldTag{Set: true, Tag: 5, WireOverride: "int16"}, false},
		{"type-int32", `bin:"5,type=int32"`, FieldTag{Set: true, Tag: 5, WireOverride: "int32"}, false},
		{"type-int64", `bin:"5,type=int64"`, FieldTag{Set: true, Tag: 5, WireOverride: "int64"}, false},
		{"type-uint8", `bin:"5,type=uint8"`, FieldTag{Set: true, Tag: 5, WireOverride: "uint8"}, false},
		{"type-uint16", `bin:"5,type=uint16"`, FieldTag{Set: true, Tag: 5, WireOverride: "uint16"}, false},
		{"type-uint32", `bin:"5,type=uint32"`, FieldTag{Set: true, Tag: 5, WireOverride: "uint32"}, false},
		{"type-uint64", `bin:"5,type=uint64"`, FieldTag{Set: true, Tag: 5, WireOverride: "uint64"}, false},
		{"type-with-deprecated", `bin:"5,type=int64,deprecated"`, FieldTag{Set: true, Tag: 5, WireOverride: "int64", Deprecated: true}, false},
		{"type-empty", `bin:"5,type="`, FieldTag{Set: true, Tag: 5}, true},
		{"type-int24", `bin:"5,type=int24"`, FieldTag{Set: true, Tag: 5}, true},
		{"type-uint24", `bin:"5,type=uint24"`, FieldTag{Set: true, Tag: 5}, true},
		{"type-Int32-case-sensitive", `bin:"5,type=Int32"`, FieldTag{Set: true, Tag: 5}, true},
		{"type-UINT64-case-sensitive", `bin:"5,type=UINT64"`, FieldTag{Set: true, Tag: 5}, true},
		{"type-foo", `bin:"5,type=foo"`, FieldTag{Set: true, Tag: 5}, true},
		{"type-repeated", `bin:"5,type=int32,type=int64"`, FieldTag{Set: true, Tag: 5, WireOverride: "int32"}, true},
		{"type-repeated-int8-uint8", `bin:"5,type=int8,type=uint8"`, FieldTag{Set: true, Tag: 5, WireOverride: "int8"}, true},
		{"type-with-custom", `bin:"5,type=int64,custom=Foo"`, FieldTag{Set: true, Tag: 5}, true},
		{"type-with-custom-uint8", `bin:"5,type=uint8,custom=Foo"`, FieldTag{Set: true, Tag: 5}, true},
		{"custom-with-type", `bin:"5,custom=Foo,type=int64"`, FieldTag{Set: true, Tag: 5}, true},
		{"type-with-id_ref", `bin:"5,type=int64,id_ref"`, FieldTag{Set: true, Tag: 5}, true},
		{"type-with-id_ref-uint16", `bin:"5,type=uint16,id_ref"`, FieldTag{Set: true, Tag: 5}, true},
		{"id_ref-with-type", `bin:"5,id_ref,type=int64"`, FieldTag{Set: true, Tag: 5}, true},
		{"skip", `bin:"-"`, FieldTag{Set: true, Skip: true}, false},
		{"zero-tag", `bin:"0"`, FieldTag{Set: true}, true},
		{"too-large", `bin:"1073741824"`, FieldTag{Set: true}, true}, // 2^30
		{"empty", `bin:""`, FieldTag{Set: true}, true},
		{"unknown-option", `bin:"5,wat"`, FieldTag{Set: true}, true},
		{"empty-custom", `bin:"5,custom="`, FieldTag{Set: true, Tag: 5}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseFieldTag(tc.tag)
			if tc.err {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

// TestParseMarkers covers the //gsbm: directives recognized by the
// schema tool. Unknown directives MUST surface as an error so a typo'd
// `//gsbm:rooot` does not silently leave a type out of the schema.
func TestParseMarkers(t *testing.T) {
	t.Run("recognized", func(t *testing.T) {
		ps, err := ParseSource("p", []string{`
package p

//gsbm:root
//gsbm:reserved 3, 5
//gsbm:allow-breaking dropping deprecated tag 7 for cleanup
type Offer struct{}
`})
		if err != nil {
			t.Fatal(err)
		}
		_, _, ok := ps.Packages[0].findStructDoc("Offer")
		if !ok {
			t.Fatal("Offer not found")
		}
	})

	t.Run("presence is accepted as no-op", func(t *testing.T) {
		ps, err := ParseSource("p", []string{`
package p

//gsbm:root
//gsbm:presence
type Offer struct {
	//gsbm:presence
	ID int64 ` + "`bin:\"1\"`" + `
}
`})
		if err != nil {
			t.Fatal(err)
		}
		_, issues := Discover(ps)
		for _, is := range issues {
			if is.Code == "marker/parse" {
				t.Fatalf("//gsbm:presence should be a no-op, got marker/parse issue: %+v", is)
			}
		}
	})

	t.Run("track-presence recognized", func(t *testing.T) {
		ps, err := ParseSource("p", []string{`
package p

//gsbm:root
//gsbm:track-presence
type Offer struct {
	ID int64 ` + "`bin:\"1\"`" + `
}
`})
		if err != nil {
			t.Fatal(err)
		}
		roots, dIssues := Discover(ps)
		for _, is := range dIssues {
			t.Fatalf("unexpected discover issue: %+v", is)
		}
		s, bIssues := BuildSchema(ps, roots)
		for _, is := range bIssues {
			t.Fatalf("unexpected build issue: %+v", is)
		}
		var sd *StructDecl
		for _, decl := range s.Structs {
			if decl.Type.Name == "Offer" {
				sd = decl
				break
			}
		}
		if sd == nil {
			t.Fatalf("Offer not in schema; got %+v", s.Structs)
		}
		if !sd.TrackPresence {
			t.Fatalf("expected TrackPresence=true on Offer, got %+v", sd)
		}
		if vIssues := Validate(s, ps); len(vIssues) != 0 {
			t.Fatalf("unexpected validate issues: %+v", vIssues)
		}
	})

	t.Run("track-presence absent defaults to false", func(t *testing.T) {
		ps, err := ParseSource("p", []string{`
package p

//gsbm:root
type Offer struct {
	ID int64 ` + "`bin:\"1\"`" + `
}
`})
		if err != nil {
			t.Fatal(err)
		}
		roots, _ := Discover(ps)
		s, _ := BuildSchema(ps, roots)
		for _, sd := range s.Structs {
			if sd.TrackPresence {
				t.Fatalf("%s: expected TrackPresence=false, got true", sd.Type.Name)
			}
		}
	})

	t.Run("borrow-strings recognized", func(t *testing.T) {
		ps, err := ParseSource("p", []string{`
package p

//gsbm:root
//gsbm:borrow-strings
type Offer struct {
	ID string ` + "`bin:\"1\"`" + `
}
`})
		if err != nil {
			t.Fatal(err)
		}
		roots, dIssues := Discover(ps)
		for _, is := range dIssues {
			t.Fatalf("unexpected discover issue: %+v", is)
		}
		s, bIssues := BuildSchema(ps, roots)
		for _, is := range bIssues {
			t.Fatalf("unexpected build issue: %+v", is)
		}
		if len(s.Structs) != 1 || !s.Structs[0].BorrowStrings {
			t.Fatalf("expected BorrowStrings=true on Offer, got %+v", s.Structs)
		}
		if vIssues := Validate(s, ps); len(vIssues) != 0 {
			t.Fatalf("unexpected validate issues: %+v", vIssues)
		}
	})

	t.Run("borrow-strings does not change schema hint", func(t *testing.T) {
		base := &Schema{FmtVer: FmtVer, Roots: []TypeRef{{PkgPath: "p", Name: "Offer"}}, Structs: []*StructDecl{{Type: TypeRef{PkgPath: "p", Name: "Offer"}, Fields: []*FieldDecl{{Name: "ID", Tag: 1, Type: "string", Wire: WireLengthDelim}}}}}
		borrow := &Schema{FmtVer: FmtVer, Roots: []TypeRef{{PkgPath: "p", Name: "Offer"}}, Structs: []*StructDecl{{Type: TypeRef{PkgPath: "p", Name: "Offer"}, BorrowStrings: true, Fields: []*FieldDecl{{Name: "ID", Tag: 1, Type: "string", Wire: WireLengthDelim}}}}}
		if ComputeSchemaHint(base) != ComputeSchemaHint(borrow) {
			t.Fatal("BorrowStrings is a Go-side lifetime marker and must not affect schemaHint")
		}
	})

	t.Run("track-presence rejected on opaque", func(t *testing.T) {
		ps, err := ParseSource("p", []string{`
package p

//gsbm:root
type Holder struct {
	B Box ` + "`bin:\"1\"`" + `
}

//gsbm:opaque
//gsbm:track-presence
type Box struct {
	X int64
}
`})
		if err != nil {
			t.Fatal(err)
		}
		roots, _ := Discover(ps)
		s, _ := BuildSchema(ps, roots)
		issues := Validate(s, ps)
		var found bool
		for _, is := range issues {
			if is.Code == "marker/track-presence-opaque" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected marker/track-presence-opaque issue, got %+v", issues)
		}
	})

	t.Run("track-presence on field rejected", func(t *testing.T) {
		ps, err := ParseSource("p", []string{`
package p

//gsbm:root
type Offer struct {
	//gsbm:track-presence
	ID int64 ` + "`bin:\"1\"`" + `
}
`})
		if err != nil {
			t.Fatal(err)
		}
		roots, _ := Discover(ps)
		_, bIssues := BuildSchema(ps, roots)
		var found bool
		for _, is := range bIssues {
			if is.Code == "marker/track-presence-misplaced" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected marker/track-presence-misplaced issue, got %+v", bIssues)
		}
	})

	t.Run("track-presence on anonymous embed rejected", func(t *testing.T) {
		ps, err := ParseSource("p", []string{`
package p

type Base struct {
	ID int64 ` + "`bin:\"1\"`" + `
}

//gsbm:root
type Offer struct {
	//gsbm:track-presence
	Base
}
`})
		if err != nil {
			t.Fatal(err)
		}
		roots, _ := Discover(ps)
		_, bIssues := BuildSchema(ps, roots)
		var found bool
		for _, is := range bIssues {
			if is.Code == "marker/track-presence-misplaced" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected marker/track-presence-misplaced issue, got %+v", bIssues)
		}
	})

	t.Run("track-presence on pointer anonymous embed rejected", func(t *testing.T) {
		ps, err := ParseSource("p", []string{`
package p

type Base struct {
	ID int64 ` + "`bin:\"1\"`" + `
}

//gsbm:root
type Offer struct {
	//gsbm:track-presence
	*Base
}
`})
		if err != nil {
			t.Fatal(err)
		}
		roots, _ := Discover(ps)
		_, bIssues := BuildSchema(ps, roots)
		var found bool
		for _, is := range bIssues {
			if is.Code == "marker/track-presence-misplaced" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected marker/track-presence-misplaced issue, got %+v", bIssues)
		}
	})

	t.Run("borrow-strings rejected on opaque", func(t *testing.T) {
		ps, err := ParseSource("p", []string{`
package p

//gsbm:root
type Holder struct {
	B Box ` + "`bin:\"1\"`" + `
}

//gsbm:opaque
//gsbm:borrow-strings
type Box struct {
	X string
}
`})
		if err != nil {
			t.Fatal(err)
		}
		roots, _ := Discover(ps)
		s, _ := BuildSchema(ps, roots)
		issues := Validate(s, ps)
		var found bool
		for _, is := range issues {
			if is.Code == "marker/borrow-strings-opaque" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected marker/borrow-strings-opaque issue, got %+v", issues)
		}
	})

	t.Run("borrow-strings on field rejected", func(t *testing.T) {
		ps, err := ParseSource("p", []string{`
package p

//gsbm:root
type Offer struct {
	//gsbm:borrow-strings
	ID string ` + "`bin:\"1\"`" + `
}
`})
		if err != nil {
			t.Fatal(err)
		}
		roots, _ := Discover(ps)
		_, bIssues := BuildSchema(ps, roots)
		var found bool
		for _, is := range bIssues {
			if is.Code == "marker/borrow-strings-misplaced" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected marker/borrow-strings-misplaced issue, got %+v", bIssues)
		}
	})

	t.Run("borrow-strings on anonymous embed rejected", func(t *testing.T) {
		ps, err := ParseSource("p", []string{`
package p

type Base struct {
	ID string ` + "`bin:\"1\"`" + `
}

//gsbm:root
type Offer struct {
	//gsbm:borrow-strings
	Base
}
`})
		if err != nil {
			t.Fatal(err)
		}
		roots, _ := Discover(ps)
		_, bIssues := BuildSchema(ps, roots)
		var found bool
		for _, is := range bIssues {
			if is.Code == "marker/borrow-strings-misplaced" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected marker/borrow-strings-misplaced issue, got %+v", bIssues)
		}
	})

	t.Run("unknown directive errors", func(t *testing.T) {
		ps, err := ParseSource("p", []string{`
package p
//gsbm:rooot
type Offer struct{}
`})
		if err != nil {
			t.Fatal(err)
		}
		// Parse-time is silent; Discover surfaces unknown markers as an
		// Issue with code "marker/parse".
		_, issues := Discover(ps)
		var found bool
		for _, is := range issues {
			if is.Code == "marker/parse" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected marker/parse issue for unknown directive, got %+v", issues)
		}
	})
}
