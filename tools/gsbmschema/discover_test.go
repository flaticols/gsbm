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
// is a map or array (e.g. `type LabelMap map[string]string`) cannot be
// decoded; named aliases over a supported slice element type are now
// supported (issue #8), so the rejected shapes here are the still-
// unhandled non-slice composites.
func TestRejectNamedNonPrimitiveUnderlying(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			"map-underlying",
			`
package p

type LabelMap map[string]string

//gsbm:root
type Offer struct {
	ID  uint64    ` + "`bin:\"1\"`" + `
	Lbl LabelMap  ` + "`bin:\"2\"`" + `
}
`,
		},
		{
			"array-underlying",
			`
package p

type FixedTriple [3]int64

//gsbm:root
type Offer struct {
	ID  uint64       ` + "`bin:\"1\"`" + `
	Tri FixedTriple  ` + "`bin:\"2\"`" + `
}
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ps, err := ParseSource("p", []string{tc.src})
			if err != nil {
				t.Fatal(err)
			}
			roots, _ := Discover(ps)
			_, issues := BuildSchema(ps, roots)
			if !hasIssueCode(issues, "type/unsupported") {
				t.Fatalf("expected type/unsupported for %s, got %v", tc.name, issues)
			}
		})
	}
}

// TestRejectOptionalNamedSliceAlias — pointer-to-named-slice (`*Labels`
// where `Labels = []string`) is rejected because codegen has no decode
// path for optional composites, mirroring the existing `*[]T` rejection.
// The pointer recursion in checkSupportedType catches this at depth>0.
func TestRejectOptionalNamedSliceAlias(t *testing.T) {
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

// TestAcceptSlicePointerToStruct — `[]*T` where T is a named struct is
// accepted (issue #8). Per spec §5.1 each element is a length-delim
// envelope with a presence byte so nil elements mid-slice survive
// round-trip.
func TestAcceptSlicePointerToStruct(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

type Item struct {
	Code string ` + "`bin:\"1\"`" + `
}

//gsbm:root
type Batch struct {
	ID    uint64  ` + "`bin:\"1\"`" + `
	Items []*Item ` + "`bin:\"2\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := Discover(ps)
	_, issues := BuildSchema(ps, roots)
	if len(issues) != 0 {
		t.Fatalf("expected no issues for []*Item, got %v", issues)
	}
}

// TestAcceptNamedSliceAlias — `type ItemList []Item` (and the pointer-
// element variant `type ItemPtrList []*Item`) are accepted, with the
// named identity preserved for schema diffing while the wire shape is
// byte-identical to the underlying slice.
func TestAcceptNamedSliceAlias(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

type Item struct {
	Code string ` + "`bin:\"1\"`" + `
}

type ItemList []Item
type ItemPtrList []*Item

//gsbm:root
type Catalog struct {
	ID       uint64      ` + "`bin:\"1\"`" + `
	Groups   ItemList    ` + "`bin:\"2\"`" + `
	Optional ItemPtrList ` + "`bin:\"3\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := Discover(ps)
	_, issues := BuildSchema(ps, roots)
	if len(issues) != 0 {
		t.Fatalf("expected no issues for named slice aliases, got %v", issues)
	}
}

// TestRejectSliceOfPointerToPrimitive — `[]*int64` is intentionally
// rejected for v1; the spec's optional-shape list covers direct fields
// only and slice-of-pointer-to-primitive has no codegen support.
func TestRejectSliceOfPointerToPrimitive(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

//gsbm:root
type Offer struct {
	ID    uint64   ` + "`bin:\"1\"`" + `
	Marks []*int64 ` + "`bin:\"2\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := Discover(ps)
	_, issues := BuildSchema(ps, roots)
	if !hasIssueCode(issues, "type/unsupported") {
		t.Fatalf("expected type/unsupported for []*int64, got %v", issues)
	}
}

// TestRejectSliceOfInterface — interfaces remain rejected as slice
// elements after the issue #8 changes.
func TestRejectSliceOfInterface(t *testing.T) {
	ps, err := ParseSource("p", []string{`
package p

//gsbm:root
type Offer struct {
	ID   uint64        ` + "`bin:\"1\"`" + `
	Vals []interface{} ` + "`bin:\"2\"`" + `
}
`})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := Discover(ps)
	_, issues := BuildSchema(ps, roots)
	if !hasIssueCode(issues, "type/unsupported") {
		t.Fatalf("expected type/unsupported for []interface{}, got %v", issues)
	}
}

// TestAcceptNestedCompositeShapes — issue #9: the validator accepts
// `map[K][]V`, `map[K]map[K2]V`, `[]map[K]V`, and `[][]V` up to
// MaxNestingDepth=3. Each level's key/element/value is independently
// validated and the wire format already accommodates them under the
// existing LENGTH_DELIM framing.
func TestAcceptNestedCompositeShapes(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			"map-of-slice",
			`
package p

//gsbm:root
type Index struct {
	ID         uint64              ` + "`bin:\"1\"`" + `
	IDsByGroup map[string][]string ` + "`bin:\"2\"`" + `
}
`,
		},
		{
			"map-of-map",
			`
package p

//gsbm:root
type Index struct {
	ID            uint64                        ` + "`bin:\"1\"`" + `
	LabelsByGroup map[string]map[string]string  ` + "`bin:\"2\"`" + `
}
`,
		},
		{
			"slice-of-map",
			`
package p

//gsbm:root
type Index struct {
	ID               uint64              ` + "`bin:\"1\"`" + `
	MetadataVariants []map[string]string ` + "`bin:\"2\"`" + `
}
`,
		},
		{
			"slice-of-slice-of-bytes",
			`
package p

//gsbm:root
type Index struct {
	ID    uint64   ` + "`bin:\"1\"`" + `
	Blobs [][]byte ` + "`bin:\"2\"`" + `
}
`,
		},
		{
			"three-deep-at-cap",
			`
package p

//gsbm:root
type Index struct {
	ID   uint64                          ` + "`bin:\"1\"`" + `
	Deep map[string][]map[string]int64   ` + "`bin:\"2\"`" + `
}
`,
		},
		{
			// Named slice alias whose element is itself a nested composite:
			// the emitter routes alias slices through emitSliceEncode →
			// emitValueEncode which already handles nested map/slice
			// elements, so the validator must accept the alias form too.
			"named-slice-alias-of-map",
			`
package p

type Variants []map[string]string

//gsbm:root
type Index struct {
	ID       uint64    ` + "`bin:\"1\"`" + `
	Variants Variants  ` + "`bin:\"2\"`" + `
}
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ps, err := ParseSource("p", []string{tc.src})
			if err != nil {
				t.Fatal(err)
			}
			roots, _ := Discover(ps)
			_, issues := BuildSchema(ps, roots)
			if len(issues) != 0 {
				t.Fatalf("expected no issues for %s, got %v", tc.name, issues)
			}
		})
	}
}

// TestRejectNestingPastCap — anything strictly deeper than MaxNestingDepth
// surfaces `type/nesting-too-deep` with the offending field path in the
// diagnostic. The wire format itself has no depth limit; this is a codegen
// guard.
func TestRejectNestingPastCap(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			"four-deep-map",
			`
package p

//gsbm:root
type Index struct {
	ID      uint64                                            ` + "`bin:\"1\"`" + `
	TooDeep map[string]map[string]map[string]map[string]int64 ` + "`bin:\"2\"`" + `
}
`,
		},
		{
			"four-deep-slice",
			`
package p

//gsbm:root
type Index struct {
	ID      uint64       ` + "`bin:\"1\"`" + `
	TooDeep [][][][]int64 ` + "`bin:\"2\"`" + `
}
`,
		},
		{
			// []byte is length-delim on the wire just like any other
			// slice; the cap must count it as a composite layer so
			// `[][][][]byte` is rejected at 4 levels even though codegen
			// treats the innermost []byte as a leaf (WriteBytes).
			"four-deep-byte",
			`
package p

//gsbm:root
type Index struct {
	ID      uint64        ` + "`bin:\"1\"`" + `
	TooDeep [][][][]byte ` + "`bin:\"2\"`" + `
}
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ps, err := ParseSource("p", []string{tc.src})
			if err != nil {
				t.Fatal(err)
			}
			roots, _ := Discover(ps)
			_, issues := BuildSchema(ps, roots)
			if !hasIssueCode(issues, "type/nesting-too-deep") {
				t.Fatalf("expected type/nesting-too-deep for %s, got %v", tc.name, issues)
			}
			// Diagnostic must name the offending field.
			var hit *Issue
			for i := range issues {
				if issues[i].Code == "type/nesting-too-deep" {
					hit = &issues[i]
					break
				}
			}
			if hit == nil || !strings.Contains(hit.Message, "TooDeep") {
				t.Fatalf("expected diagnostic to name TooDeep field, got: %v", hit)
			}
		})
	}
}

// TestNestedCompositeStillRejectsLeafErrors — widening the validator to
// accept nested composites must not let unsupported leaf types slip
// through at depth. An interface or channel buried inside `[]map[K]V`
// still fails at the leaf, just at any depth now.
func TestNestedCompositeStillRejectsLeafErrors(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			"map-of-slice-of-interface",
			`
package p

//gsbm:root
type Offer struct {
	ID   uint64                  ` + "`bin:\"1\"`" + `
	Vals map[string][]interface{} ` + "`bin:\"2\"`" + `
}
`,
		},
		{
			"slice-of-map-of-chan",
			`
package p

//gsbm:root
type Offer struct {
	ID   uint64                ` + "`bin:\"1\"`" + `
	Vals []map[string]chan int ` + "`bin:\"2\"`" + `
}
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ps, err := ParseSource("p", []string{tc.src})
			if err != nil {
				t.Fatal(err)
			}
			roots, _ := Discover(ps)
			_, issues := BuildSchema(ps, roots)
			if !hasIssueCode(issues, "type/unsupported") {
				t.Fatalf("expected type/unsupported leaf rejection in %s, got %v", tc.name, issues)
			}
		})
	}
}

// TestNestedCompositeTypeStringCapturesFullShape — the snapshot's
// fd.Type string must recursively encode every level of composite
// nesting. This is what the classifier's field/type-changed branch
// compares, so any drift here would let a wire-shape change at depth 2+
// slip through as "safe". Pin the exact rendered string per shape so a
// future shapeOf refactor cannot silently break the diff.
func TestNestedCompositeTypeStringCapturesFullShape(t *testing.T) {
	cases := []struct {
		name      string
		field     string
		wantType  string
		wantElem  string
		wantMapV  string
	}{
		{
			name:     "map-of-slice",
			field:    "map[string][]string",
			wantType: "map[string][]string",
			wantMapV: "[]string",
		},
		{
			name:     "map-of-map",
			field:    "map[string]map[string]string",
			wantType: "map[string]map[string]string",
			wantMapV: "map[string]string",
		},
		{
			name:     "slice-of-map",
			field:    "[]map[string]string",
			wantType: "[]map[string]string",
			wantElem: "map[string]string",
		},
		{
			name:     "three-deep-at-cap",
			field:    "map[string][]map[string]int64",
			wantType: "map[string][]map[string]int64",
			wantMapV: "[]map[string]int64",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := `
package p

//gsbm:root
type Index struct {
	ID uint64 ` + "`bin:\"1\"`" + `
	M  ` + tc.field + ` ` + "`bin:\"2\"`" + `
}
`
			ps, err := ParseSource("p", []string{src})
			if err != nil {
				t.Fatal(err)
			}
			roots, _ := Discover(ps)
			schema, issues := BuildSchema(ps, roots)
			if len(issues) != 0 {
				t.Fatalf("unexpected issues: %v", issues)
			}
			idx := findStruct(schema, "Index")
			var m *FieldDecl
			for _, fd := range idx.Fields {
				if fd.Name == "M" {
					m = fd
				}
			}
			if m == nil {
				t.Fatal("M field missing")
			}
			if m.Type != tc.wantType {
				t.Errorf("fd.Type = %q, want %q", m.Type, tc.wantType)
			}
			if tc.wantElem != "" && m.Elem != tc.wantElem {
				t.Errorf("fd.Elem = %q, want %q", m.Elem, tc.wantElem)
			}
			if tc.wantMapV != "" && m.MapValue != tc.wantMapV {
				t.Errorf("fd.MapValue = %q, want %q", m.MapValue, tc.wantMapV)
			}
		})
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
