package odmschema

import (
	"testing"
)

// TestValidateTagUniqueness confirms that two fields sharing a tag in
// the same struct are flagged as `tag/duplicate`.
func TestValidateTagUniqueness(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

//odm:root
type Offer struct {
	A uint64 ` + "`bin:\"1\"`" + `
	B uint64 ` + "`bin:\"1\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := Discover(ps)
	s, _ := BuildSchema(ps, roots)
	issues := Validate(s, ps)
	if !hasIssueCode(issues, "tag/duplicate") {
		t.Fatalf("expected tag/duplicate, got %v", issues)
	}
}

// TestValidateReservedTagInUse — a reserved tag MUST NOT be assigned
// to a field; the validator surfaces this as `tag/reserved`.
func TestValidateReservedTagInUse(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

//odm:root
//odm:reserved 7
type Offer struct {
	A uint64 ` + "`bin:\"7\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := Discover(ps)
	s, _ := BuildSchema(ps, roots)
	issues := Validate(s, ps)
	if !hasIssueCode(issues, "tag/reserved") {
		t.Fatalf("expected tag/reserved, got %v", issues)
	}
}

// TestValidateMapKey — only primitive / string keys are allowed by
// spec §5.3; a struct-keyed map MUST be flagged.
func TestValidateMapKey(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

type K struct {
	A int ` + "`bin:\"1\"`" + `
}

//odm:root
type Offer struct {
	M map[K]int ` + "`bin:\"1\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := Discover(ps)
	s, _ := BuildSchema(ps, roots)
	issues := Validate(s, ps)
	if !hasIssueCode(issues, "map/bad-key") {
		t.Fatalf("expected map/bad-key, got %v", issues)
	}
}

// TestValidateNoCycles — A → B → A cycle MUST be rejected unless one
// of the participating fields carries //odm:cycle_break_via_id.
func TestValidateNoCycles(t *testing.T) {
	src := `
package p

//odm:root
type A struct {
	B *B ` + "`bin:\"1\"`" + `
}

type B struct {
	A *A ` + "`bin:\"1\"`" + `
}
`
	ps, err := ParseSource("p", []string{src})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := Discover(ps)
	s, _ := BuildSchema(ps, roots)
	issues := Validate(s, ps)
	if !hasIssueCode(issues, "type/cycle") {
		t.Fatalf("expected type/cycle, got %v", issues)
	}

	// Same graph, but A.B carries //odm:cycle_break_via_id — accepted.
	srcOK := `
package p

//odm:root
type A struct {
	//odm:cycle_break_via_id
	B *B ` + "`bin:\"1\"`" + `
}

type B struct {
	A *A ` + "`bin:\"1\"`" + `
}
`
	ps2, err := ParseSource("p", []string{srcOK})
	if err != nil {
		t.Fatal(err)
	}
	roots2, _ := Discover(ps2)
	s2, _ := BuildSchema(ps2, roots2)
	issues2 := Validate(s2, ps2)
	if hasIssueCode(issues2, "type/cycle") {
		t.Fatalf("did not expect type/cycle with cycle_break, got %v", issues2)
	}
}

// TestValidateOpaqueOptOut — an //odm:opaque struct is included as a
// placeholder and is NOT walked into. Field-level issues inside such a
// struct MUST NOT be raised.
func TestValidateOpaqueOptOut(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

//odm:opaque
type Inner struct {
	NoTag string
}

//odm:root
type Offer struct {
	I Inner ` + "`bin:\"1\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := Discover(ps)
	s, _ := BuildSchema(ps, roots)
	if hasIssueCode(Validate(s, ps), "tag/missing") {
		t.Fatalf("opaque struct should not surface field-level issues")
	}
}

// TestIsBuiltinPrimitive is the source-of-truth check codegen uses to
// decide whether a field can be zero-elided per spec §5.1.
func TestIsBuiltinPrimitive(t *testing.T) {
	for _, ok := range []string{"int64", "uint32", "float64", "bool", "string", "[]byte"} {
		if !IsBuiltinPrimitive(ok) {
			t.Errorf("%s: expected eligible", ok)
		}
	}
	for _, no := range []string{"time.Time", "test/p.Offer", "any"} {
		if IsBuiltinPrimitive(no) {
			t.Errorf("%s: expected ineligible", no)
		}
	}
}
