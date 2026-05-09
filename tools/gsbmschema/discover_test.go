package gsbmschema

import (
	"strings"
	"testing"
)

// TestDiscoverFindsRoots checks the basic `//gsbm:root` discovery —
// only types with the marker are surfaced, and they are sorted by
// (PkgPath, Name) for determinism.
func TestDiscoverFindsRoots(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

//gsbm:root
type Offer struct {
	ID uint64 ` + "`bin:\"1\"`" + `
}

type Helper struct {
	X int ` + "`bin:\"1\"`" + `
}

//gsbm:root
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

//gsbm:root
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

//gsbm:root
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

// TestRootMustBeStruct rejects `//gsbm:root` on aliases and non-struct
// types — those would have no fields and would silently produce empty
// schemas otherwise.
func TestRootMustBeStruct(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

//gsbm:root
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

// TestUnknownGSBMDirective rejects `//gsbm:rooot` and similar typos.
func TestUnknownGSBMDirective(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

//gsbm:rooot
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

// TestRejectComplexBasic — codegen has no encoder/decoder for complex
// kinds; rejecting them at schema-validation time keeps the failure
// mode where it belongs (lint/snapshot) instead of leaking into gen.
func TestRejectComplexBasic(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

//gsbm:root
type Offer struct {
	ID uint64     ` + "`bin:\"1\"`" + `
	C  complex64  ` + "`bin:\"2\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := Discover(ps)
	_, issues := BuildSchema(ps, roots)
	if !hasIssueCode(issues, "type/unsupported") {
		t.Fatalf("expected type/unsupported for complex64, got %v", issues)
	}
}

// TestRejectNamedNonPrimitiveUnderlying — a named type whose underlying
// is a slice/map/array (e.g. `type Labels []string`) cannot be decoded
// because emitPrimitiveDecodeAssign requires *types.Basic underlying.
// Reject at schema-validation time.
func TestRejectNamedNonPrimitiveUnderlying(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

type Labels []string

//gsbm:root
type Offer struct {
	ID  uint64 ` + "`bin:\"1\"`" + `
	Lbl Labels ` + "`bin:\"2\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := Discover(ps)
	_, issues := BuildSchema(ps, roots)
	if !hasIssueCode(issues, "type/unsupported") {
		t.Fatalf("expected type/unsupported for named-slice, got %v", issues)
	}
}

// TestRejectOptionalNamedNonPrimitiveUnderlying — same gap as above
// but reached through a pointer field. The optional-composite check in
// validateStruct misses this (fd.Type is the named alias, not the
// slice form) so checkSupportedType must catch it via pointer recursion.
func TestRejectOptionalNamedNonPrimitiveUnderlying(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

type Labels []string

//gsbm:root
type Offer struct {
	ID  uint64   ` + "`bin:\"1\"`" + `
	Lbl *Labels  ` + "`bin:\"2\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := Discover(ps)
	_, issues := BuildSchema(ps, roots)
	if !hasIssueCode(issues, "type/unsupported") {
		t.Fatalf("expected type/unsupported for *named-slice, got %v", issues)
	}
}

// TestRejectNestedSliceOfBytes — `[][]byte` reaches a slice element that
// is itself a (non-byte) slice. Codegen has no decode path for nested
// composites, so validation must reject before the user gets to gen.
func TestRejectNestedSliceOfBytes(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

//gsbm:root
type Offer struct {
	ID    uint64    ` + "`bin:\"1\"`" + `
	Blobs [][]byte  ` + "`bin:\"2\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := Discover(ps)
	_, issues := BuildSchema(ps, roots)
	if !hasIssueCode(issues, "type/unsupported") {
		t.Fatalf("expected type/unsupported for [][]byte, got %v", issues)
	}
}

// TestRejectMapOfSlice — `map[string][]Item` reaches a map value that is
// a (non-byte) slice. emitMapDecode has no recursion for nested composite
// values, so validation must reject before gen.
func TestRejectMapOfSlice(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

type Item struct {
	ID uint64 ` + "`bin:\"1\"`" + `
}

//gsbm:root
type Offer struct {
	ID     uint64              ` + "`bin:\"1\"`" + `
	Groups map[string][]Item   ` + "`bin:\"2\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := Discover(ps)
	_, issues := BuildSchema(ps, roots)
	if !hasIssueCode(issues, "type/unsupported") {
		t.Fatalf("expected type/unsupported for map[string][]Item, got %v", issues)
	}
}

// TestNamedPrimitiveTypeCapturesUnderlying — a named primitive alias
// (e.g. `type Quantity int64`) must record its underlying primitive in
// the schema Type field, otherwise switching the underlying primitive
// (int64 → int32) would not change the Type/Wire pair and the classifier
// would silently report the change as safe even though old blobs may
// fail to decode (ErrIntegerOverflow on the narrower type).
func TestNamedPrimitiveTypeCapturesUnderlying(t *testing.T) {
	src := func(under string) string {
		return `
package p

type Quantity ` + under + `

//gsbm:root
type Offer struct {
	ID  uint64   ` + "`bin:\"1\"`" + `
	Qty Quantity ` + "`bin:\"2\"`" + `
}
`
	}
	build := func(under string) *FieldDecl {
		ps, err := ParseSource("p", []string{src(under)})
		if err != nil {
			t.Fatal(err)
		}
		roots, _ := Discover(ps)
		schema, issues := BuildSchema(ps, roots)
		if len(issues) != 0 {
			t.Fatalf("unexpected issues: %v", issues)
		}
		offer := findStruct(schema, "Offer")
		for _, fd := range offer.Fields {
			if fd.Name == "Qty" {
				return fd
			}
		}
		t.Fatal("Qty field missing")
		return nil
	}
	a := build("int64")
	b := build("int32")
	if a.Type == b.Type {
		t.Fatalf("expected named-primitive Type to differ when underlying changes; got %q for both", a.Type)
	}
	if !strings.Contains(a.Type, "int64") || !strings.Contains(b.Type, "int32") {
		t.Fatalf("expected underlying primitive in Type strings; got %q vs %q", a.Type, b.Type)
	}
}

// TestCustomMarshalerAnnotationPlumbed — `bin:"N,custom=Foo"` must be
// captured on FieldDecl so the classifier can warn on add and block on
// remove/change. Without plumbing, the parser-captured Custom is silently
// discarded.
func TestCustomMarshalerAnnotationPlumbed(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

//gsbm:root
type Offer struct {
	ID    uint64 ` + "`bin:\"1\"`" + `
	Price uint64 ` + "`bin:\"2,custom=PriceCodec\"`" + `
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
	offer := findStruct(schema, "Offer")
	for _, fd := range offer.Fields {
		if fd.Name == "Price" {
			if fd.Custom != "PriceCodec" {
				t.Fatalf("expected Custom=PriceCodec, got %q", fd.Custom)
			}
			return
		}
	}
	t.Fatal("Price field missing")
}

// TestRefKeyOpaqueGenericNoLeadingDot — generic instantiations whose
// type arguments are non-named (e.g. Box[int]) must not leak a stray
// leading dot into the dedup key, the field-type string, or the
// canonical hash input. Without the empty-PkgPath guard in refKey,
// the basic-type arg renders as ".int" and stacks under the parent
// as `p.Box[.int]`.
func TestRefKeyOpaqueGenericNoLeadingDot(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

//gsbm:opaque
type Box[T any] struct {
	Value T
}

//gsbm:root
type Root struct {
	B Box[int] ` + "`bin:\"1\"`" + `
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
	root := findStruct(schema, "Root")
	if root == nil {
		t.Fatal("Root struct missing from schema")
	}
	for _, fd := range root.Fields {
		if strings.Contains(fd.Type, ".int") {
			t.Errorf("field %s Type leaked malformed `.int`: %q", fd.Name, fd.Type)
		}
		if !strings.Contains(fd.Type, "Box[int]") {
			t.Errorf("field %s Type missing expected `Box[int]`: %q", fd.Name, fd.Type)
		}
	}
	canon := canonicalize(schema)
	if strings.Contains(canon, ".int") {
		t.Errorf("canonical hash input leaked malformed `.int`:\n%s", canon)
	}
	if !strings.Contains(canon, "p.Box[int]") {
		t.Errorf("canonical hash input missing expected `p.Box[int]`:\n%s", canon)
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
