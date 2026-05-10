package gsbmschema

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMarshalRoundTrip — JSON snapshot is canonical input for diff. The
// shape MUST round-trip without loss.
func TestMarshalRoundTrip(t *testing.T) {
	s := &Schema{
		FmtVer:     1,
		SchemaHint: 0xCAFE,
		Roots:  []TypeRef{{PkgPath: "p", Name: "Offer"}},
		Structs: []*StructDecl{
			{
				Type: TypeRef{PkgPath: "p", Name: "Offer"},
				Fields: []*FieldDecl{
					{Name: "ID", Tag: 1, Type: "uint64", Wire: WireVarint},
					{Name: "Items", Tag: 2, Type: "[]p.Item", Wire: WireLengthDelim, Elem: "p.Item"},
					// Survives JSON round-trip — the classifier reads the previous
					// snapshot from disk, so dropping CompatWrite here would silently
					// reclassify a compat_write→deprecated transition as a no-op.
					{Name: "Legacy", Tag: 3, Type: "string", Wire: WireLengthDelim, Deprecated: true, CompatWrite: true},
				},
				Reserved: []uint32{99},
			},
			{
				Type: TypeRef{PkgPath: "p", Name: "Item"},
				Fields: []*FieldDecl{
					{Name: "Code", Tag: 1, Type: "string", Wire: WireLengthDelim},
				},
			},
		},
	}
	b, err := MarshalJSON(s)
	if err != nil {
		t.Fatal(err)
	}
	var got Schema
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.SchemaHint != s.SchemaHint || len(got.Structs) != 2 {
		t.Fatalf("round-trip failed: %+v", got)
	}
	if got.Structs[0].Fields[1].Elem != "p.Item" {
		t.Fatalf("elem lost in round-trip: %+v", got.Structs[0].Fields[1])
	}
	legacy := got.Structs[0].Fields[2]
	if !legacy.Deprecated || !legacy.CompatWrite {
		t.Fatalf("Deprecated/CompatWrite lost in round-trip: %+v", legacy)
	}
}

// TestMarshalYAMLContains makes sure the human-readable surface
// contains every piece of information a reviewer would look for.
func TestMarshalYAMLContains(t *testing.T) {
	s := &Schema{
		FmtVer: 1, SchemaHint: 0xBEEF,
		Roots: []TypeRef{{PkgPath: "p", Name: "Offer"}},
		Structs: []*StructDecl{
			{
				Type: TypeRef{PkgPath: "p", Name: "Offer"},
				Fields: []*FieldDecl{
					{Name: "ID", Tag: 1, Type: "uint64", Wire: WireVarint},
					{Name: "M", Tag: 2, Type: "map[string]int", Wire: WireLengthDelim, MapKey: "string", MapValue: "int"},
				},
				Reserved: []uint32{99},
			},
		},
	}
	got := string(MarshalYAML(s))
	for _, want := range []string{
		"fmtVer: 1",
		"schemaHint: 48879",
		"p.Offer",
		"tag: 1",
		"mapKey: string",
		"reserved: [99]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("yaml missing %q\n%s", want, got)
		}
	}
}

// TestMarshalYAMLGenericTypeArgs — generic instantiations with
// non-named arguments (e.g. Box[int]) and nested generics
// (e.g. Box[List[int]]) must render with their inner args inline,
// without a stray leading dot or stacked YAML quoting.
func TestMarshalYAMLGenericTypeArgs(t *testing.T) {
	s := &Schema{
		FmtVer: 1, SchemaHint: 1,
		Roots: []TypeRef{{PkgPath: "p", Name: "Root"}},
		Structs: []*StructDecl{
			{
				Type: TypeRef{
					PkgPath: "p", Name: "Box",
					TypeArgs: []TypeRef{{Name: "int"}},
				},
			},
			{
				Type: TypeRef{
					PkgPath: "p", Name: "Box",
					TypeArgs: []TypeRef{{
						PkgPath: "p", Name: "List",
						TypeArgs: []TypeRef{{Name: "int"}},
					}},
				},
			},
		},
	}
	got := string(MarshalYAML(s))
	for _, want := range []string{
		`"p.Box[int]"`,
		`"p.Box[p.List[int]]"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("yaml missing %q\n%s", want, got)
		}
	}
	for _, bad := range []string{
		".int",
		`\"`,
	} {
		if strings.Contains(got, bad) {
			t.Errorf("yaml unexpectedly contains %q\n%s", bad, got)
		}
	}
}

// TestEndToEnd — exercise the full Analyze pipeline against a
// well-formed package and confirm we get a populated Schema with a
// non-zero schemaHint and zero issues.
func TestEndToEnd(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

//gsbm:root
type Offer struct {
	ID    uint64           ` + "`bin:\"1\"`" + `
	Items []Item           ` + "`bin:\"2\"`" + `
	Tags  map[string]string` + "`bin:\"3\"`" + `
}

type Item struct {
	Code string ` + "`bin:\"1\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	res := Analyze(ps)
	if len(res.Issues) != 0 {
		t.Fatalf("issues: %s", FormatIssues(res.Issues))
	}
	if res.Schema.SchemaHint == 0 {
		// (a 0 hash is technically possible but vanishingly unlikely
		// for a non-empty schema; flag it as a smoke-test failure.)
		t.Fatalf("schemaHint was zero")
	}
	if len(res.Schema.Structs) != 2 {
		t.Fatalf("expected 2 structs, got %d", len(res.Schema.Structs))
	}
}
