package odmschema

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMarshalRoundTrip — JSON snapshot is canonical input for diff. The
// shape MUST round-trip without loss.
func TestMarshalRoundTrip(t *testing.T) {
	s := &Schema{
		FmtVer: 1,
		SchVer: 0xCAFE,
		Roots:  []TypeRef{{PkgPath: "p", Name: "Offer"}},
		Structs: []*StructDecl{
			{
				Type: TypeRef{PkgPath: "p", Name: "Offer"},
				Fields: []*FieldDecl{
					{Name: "ID", Tag: 1, Type: "uint64", Wire: WireVarint},
					{Name: "Items", Tag: 2, Type: "[]p.Item", Wire: WireLengthDelim, Elem: "p.Item"},
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
	if got.SchVer != s.SchVer || len(got.Structs) != 2 {
		t.Fatalf("round-trip failed: %+v", got)
	}
	if got.Structs[0].Fields[1].Elem != "p.Item" {
		t.Fatalf("elem lost in round-trip: %+v", got.Structs[0].Fields[1])
	}
}

// TestMarshalYAMLContains makes sure the human-readable surface
// contains every piece of information a reviewer would look for.
func TestMarshalYAMLContains(t *testing.T) {
	s := &Schema{
		FmtVer: 1, SchVer: 0xBEEF,
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
		"schVer: 48879",
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

// TestEndToEnd — exercise the full Analyze pipeline against a
// well-formed package and confirm we get a populated Schema with a
// non-zero schVer and zero issues.
func TestEndToEnd(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

//odm:root
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
	if res.Schema.SchVer == 0 {
		// (a 0 hash is technically possible but vanishingly unlikely
		// for a non-empty schema; flag it as a smoke-test failure.)
		t.Fatalf("schVer was zero")
	}
	if len(res.Schema.Structs) != 2 {
		t.Fatalf("expected 2 structs, got %d", len(res.Schema.Structs))
	}
}
