package odmschema

import (
	"strings"
	"testing"
)

// TestDiscoverFindsRoots checks the basic `//odm:root` discovery —
// only types with the marker are surfaced, and they are sorted by
// (PkgPath, Name) for determinism.
func TestDiscoverFindsRoots(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

//odm:root
type Offer struct {
	ID uint64 ` + "`bin:\"1\"`" + `
}

type Helper struct {
	X int ` + "`bin:\"1\"`" + `
}

//odm:root
type Audit struct {
	OfferID uint64 ` + "`bin:\"1\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	roots, issues := Discover(ps)
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %v", issues)
	}
	if len(roots) != 2 {
		t.Fatalf("expected 2 roots, got %d", len(roots))
	}
	names := []string{roots[0].Obj().Name(), roots[1].Obj().Name()}
	if names[0] != "Audit" || names[1] != "Offer" {
		t.Fatalf("expected sorted [Audit, Offer], got %v", names)
	}
}

// TestBuildSchemaWalkClosure makes sure that every type transitively
// reachable from a root via struct fields is included, and that field
// shape (wire, optional, elem, mapKey/mapValue) is set correctly.
func TestBuildSchemaWalkClosure(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

//odm:root
type Offer struct {
	ID       uint64           ` + "`bin:\"1\"`" + `
	Items    []Item           ` + "`bin:\"2\"`" + `
	Tags     map[string]Tag   ` + "`bin:\"3\"`" + `
	Carrier  *Carrier         ` + "`bin:\"4\"`" + `
	Currency string           ` + "`bin:\"5\"`" + `
	RetailUS int64            ` + "`bin:\"6\"`" + `
	Price    float64          ` + "`bin:\"7\"`" + `
}

type Item struct {
	Code string ` + "`bin:\"1\"`" + `
}

type Tag struct {
	V int ` + "`bin:\"1\"`" + `
}

type Carrier struct {
	Code string ` + "`bin:\"1\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := Discover(ps)
	schema, issues := BuildSchema(ps, roots)
	if len(issues) != 0 {
		t.Fatalf("unexpected issues: %v", issues)
	}
	want := map[string]bool{"Offer": false, "Item": false, "Tag": false, "Carrier": false}
	for _, sd := range schema.Structs {
		want[sd.Type.Name] = true
	}
	for n, ok := range want {
		if !ok {
			t.Errorf("expected %s in closure", n)
		}
	}
	// Drill into Offer to verify field shape decisions.
	offer := findStruct(schema, "Offer")
	if offer == nil {
		t.Fatal("Offer missing")
	}
	for _, fd := range offer.Fields {
		switch fd.Name {
		case "Items":
			if fd.Wire != WireLengthDelim || fd.Elem == "" {
				t.Errorf("Items: %+v", fd)
			}
		case "Tags":
			if fd.MapKey != "string" || fd.MapValue == "" {
				t.Errorf("Tags: %+v", fd)
			}
		case "Carrier":
			if !fd.Optional || fd.Wire != WireLengthDelim {
				t.Errorf("Carrier: %+v", fd)
			}
		case "Currency":
			if fd.Wire != WireLengthDelim {
				t.Errorf("Currency wire: %s", fd.Wire)
			}
		case "RetailUS":
			if fd.Wire != WireVarint {
				t.Errorf("RetailUS wire: %s", fd.Wire)
			}
		case "Price":
			if fd.Wire != WireFixed64 {
				t.Errorf("Price wire: %s", fd.Wire)
			}
		}
	}
}

// TestMissingBinTagSurfacesIssue verifies that a closure-type field
// without `bin:"…"` produces an Issue rather than being silently
// included.
func TestMissingBinTagSurfacesIssue(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

//odm:root
type Offer struct {
	ID       uint64 ` + "`bin:\"1\"`" + `
	Mystery  string
}
`})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := Discover(ps)
	_, issues := BuildSchema(ps, roots)
	if !hasIssueCode(issues, "tag/missing") {
		t.Fatalf("expected tag/missing issue, got %v", issues)
	}
}

// TestRootMustBeStruct rejects `//odm:root` on aliases and non-struct
// types — those would have no fields and would silently produce empty
// schemas otherwise.
func TestRootMustBeStruct(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

//odm:root
type Offer = uint64
`})
	if err != nil {
		t.Fatal(err)
	}
	_, issues := Discover(ps)
	if !hasIssueCode(issues, "root/not-named") && !hasIssueCode(issues, "root/not-struct") {
		t.Fatalf("expected root validity error, got %v", issues)
	}
}

// TestUnknownODMDirective rejects `//odm:rooot` and similar typos.
func TestUnknownODMDirective(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

//odm:rooot
type Offer struct {
	ID uint64 ` + "`bin:\"1\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	_, issues := Discover(ps)
	if !hasIssueCode(issues, "marker/parse") {
		t.Fatalf("expected marker/parse issue, got %v", issues)
	}
}

func findStruct(s *Schema, name string) *StructDecl {
	for _, sd := range s.Structs {
		if sd.Type.Name == name {
			return sd
		}
	}
	return nil
}

func hasIssueCode(issues []Issue, code string) bool {
	for _, i := range issues {
		if i.Code == code {
			return true
		}
	}
	return false
}

// debugIssues is a helper for failed test diagnostics.
func debugIssues(issues []Issue) string {
	var b strings.Builder
	for _, i := range issues {
		b.WriteString(i.Error())
		b.WriteByte('\n')
	}
	return b.String()
}

var _ = debugIssues
